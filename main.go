package main

import (
	"context"
	_ "embed"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
)

// maxLimit caps one query. noscat catches up on group messages with an
// unlimited query from where it left off, so those get the full cap (see
// fillGroupLimit) rather than a truncated newest-first slice.
const maxLimit = 20000

//go:embed static/index.html
var landingTempl []byte

// openStore opens the badger store, sized for a small box: the data set is
// a few hundred MB at most.
func openStore(dir string) (*badger.BadgerBackend, error) {
	b := &badger.BadgerBackend{
		Path:     dir,
		MaxLimit: maxLimit,
		BadgerOptionsModifier: func(o badgerdb.Options) badgerdb.Options {
			// Commits must survive a crash: keep synchronous writes (the
			// write rate is a few events a minute per device).
			o.SyncWrites = true
			o.MemTableSize = 16 << 20
			o.NumMemtables = 2
			o.BlockCacheSize = 32 << 20
			o.IndexCacheSize = 16 << 20
			o.ValueLogFileSize = 64 << 20
			o.Logger = nil
			return o
		},
	}
	return b, b.Init()
}

// fillGroupLimit gives unlimited group message queries the full cap.
func fillGroupLimit(_ context.Context, f *nostr.Filter) {
	if f.Limit != 0 {
		return
	}
	for _, k := range f.Kinds {
		if k == KindGroup {
			f.Limit = maxLimit
			return
		}
	}
}

// newRelay builds the relay around a store and a policy.
func newRelay(cfg Config, store *badger.BadgerBackend, policy *Policy) *khatru.Relay {
	relay := khatru.NewRelay()
	// NIP-42 logins must name this URL.
	relay.ServiceURL = cfg.RelayURL

	relay.Info.Name = cfg.RelayName
	relay.Info.Software = "https://github.com/Letdown2491/noscat-relay"
	relay.Info.Version = StringVersion()
	relay.Info.PubKey = cfg.RelayPubkey
	relay.Info.Description = cfg.RelayDescription
	relay.Info.Icon = cfg.RelayIcon
	relay.Info.Contact = cfg.RelayContact
	relay.Info.URL = cfg.RelayURL
	relay.Info.Banner = cfg.RelayBanner
	relay.Info.SupportedNIPs = []any{1, 11, 42, 59}
	relay.Info.Limitation = &nip11.RelayLimitationDocument{
		MaxMessageLength: 2 * cfg.MaxContentKB << 10,
		MaxSubscriptions: 20,
		MaxEventTags:     100,
		MaxContentLength: cfg.MaxContentKB << 10,
		MaxLimit:         maxLimit,
		AuthRequired:     true,
		RestrictedWrites: true,
	}
	relay.Info.Retention = []*nip11.RelayRetentionDocument{
		{Kinds: [][]int{{KindGiftWrap, KindGiftWrap}, {KindGroup, KindGroup}}, Time: int64(cfg.RetentionDays) * 86400},
	}

	// khatru adds NIP-9 (deletions) when a delete hook exists; this relay
	// refuses kind 5, so state the real list.
	relay.OverwriteRelayInformation = append(relay.OverwriteRelayInformation,
		func(_ context.Context, _ *http.Request, info nip11.RelayInformationDocument) nip11.RelayInformationDocument {
			info.SupportedNIPs = []any{1, 11, 42, 59}
			return info
		})

	policy.Install(relay)
	relay.OverwriteFilter = append(relay.OverwriteFilter, fillGroupLimit)
	relay.StoreEvent = append(relay.StoreEvent, store.SaveEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, store.ReplaceEvent)
	relay.QueryEvents = append(relay.QueryEvents, store.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, store.DeleteEvent)
	return relay
}

func main() {
	log.SetPrefix("noscat-relay ")
	log.Printf("Running %s", StringVersion())
	cfg := LoadConfig()

	if !strings.HasPrefix(cfg.RelayURL, "wss://") && !strings.HasPrefix(cfg.RelayURL, "ws://") {
		log.Fatalf("RELAY_URL must be the relay's public wss:// URL (NIP-42 logins are checked against it)")
	}
	source := MemberSource{File: cfg.MembersFile, Command: cfg.MembersCommand}
	if !source.Configured() {
		log.Fatalf("set MEMBERS_FILE or MEMBERS_COMMAND: the relay serves mesh members only")
	}

	configureMemory()

	dbPath := path.Join(cfg.WorkingDirectory, "database")
	store, err := openStore(dbPath)
	if err != nil {
		log.Fatalf("failed to open storage at %s: %s", dbPath, err)
	}
	log.Printf("Storage: %s, retention %d days", dbPath, cfg.RetentionDays)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	members := NewAllowlist()
	go source.Refresh(ctx, members, time.Duration(cfg.MembersRefreshSeconds)*time.Second)

	groups := map[string]bool{}
	for _, g := range cfg.Groups {
		if g = strings.TrimSpace(strings.ToLower(g)); g != "" {
			groups[g] = true
		}
	}
	policy := &Policy{
		Members:         members,
		Groups:          groups,
		Window:          time.Duration(cfg.AcceptWindowMinutes) * time.Minute,
		MaxContentBytes: cfg.MaxContentKB << 10,
		MemberLimit:     NewRateLimiter(cfg.MemberRatePerMinute, time.Minute),
		JoinLimit:       NewRateLimiter(cfg.JoinRatePerMinute, time.Minute),
	}
	reloader := &Reloader{Every: 2 * time.Second, Load: func() { source.Update(ctx, members) }}
	policy.OnMiss = reloader.Reload
	relay := newRelay(cfg, store, policy)
	go pruneLoop(ctx, store, time.Duration(cfg.RetentionDays)*24*time.Hour)

	page := template.Must(template.New("webpage").Parse(string(landingTempl)))
	relay.Router().HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		if err := page.Execute(w, relay.Info); err != nil {
			http.Error(w, "Error executing template", http.StatusInternalServerError)
		}
	})

	server := &http.Server{
		Addr:    cfg.RelayPort,
		Handler: relay,
		// Bounds the HTTP handshake only, not the WebSocket afterwards.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("Relay listening on %s for %s", cfg.RelayPort, cfg.RelayURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen error: %s", err)
		}
	}()

	if cfg.STUNAddr != "" {
		limit := NewRateLimiter(cfg.STUNRatePerMinute, time.Minute)
		go func() {
			if err := serveSTUN(ctx, cfg.STUNAddr, limit); err != nil {
				log.Fatalf("STUN: %s", err)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("Received %s: shutting down", <-sig)
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %s", err)
	}
	store.Close()
	log.Println("Shutdown complete")
}
