package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr/nip19"
)

// Allowlist is the set of mesh members (hex pubkeys) allowed to use the
// relay. It is replaced as a whole on every refresh.
type Allowlist struct {
	mu      sync.RWMutex
	members map[string]bool
}

func NewAllowlist(pubkeys ...string) *Allowlist {
	a := &Allowlist{}
	a.Set(pubkeys)
	return a
}

func (a *Allowlist) Has(pubkey string) bool {
	if pubkey == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.members[pubkey]
}

func (a *Allowlist) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.members)
}

func (a *Allowlist) Set(pubkeys []string) {
	m := make(map[string]bool, len(pubkeys))
	for _, pk := range pubkeys {
		m[pk] = true
	}
	a.mu.Lock()
	a.members = m
	a.mu.Unlock()
}

var memberToken = regexp.MustCompile(`\b(npub1[02-9ac-hj-np-z]{58}|[0-9a-f]{64})\b`)

// ParseMembers extracts every npub and 64-character hex pubkey from text,
// so it reads a plain list (one per line, # comments) as well as the
// output of `noscat mesh members` or its JSON form. Returns hex pubkeys.
func ParseMembers(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, tok := range memberToken.FindAllString(line, -1) {
			pk := tok
			if strings.HasPrefix(tok, "npub1") {
				_, v, err := nip19.Decode(tok)
				if err != nil {
					continue
				}
				s, ok := v.(string)
				if !ok {
					continue
				}
				pk = s
			} else if _, err := hex.DecodeString(tok); err != nil {
				continue
			}
			if !seen[pk] {
				seen[pk] = true
				out = append(out, pk)
			}
		}
	}
	return out
}

// MemberSource reads the member list from a file, a command, or both
// (merged).
type MemberSource struct {
	File    string
	Command string
}

func (s MemberSource) Configured() bool { return s.File != "" || s.Command != "" }

func (s MemberSource) Load(ctx context.Context) ([]string, error) {
	var text strings.Builder
	if s.File != "" {
		b, err := os.ReadFile(s.File)
		if err != nil {
			return nil, fmt.Errorf("members file: %w", err)
		}
		text.Write(b)
		text.WriteByte('\n')
	}
	if s.Command != "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c", s.Command).Output()
		if err != nil {
			return nil, fmt.Errorf("members command: %w", err)
		}
		text.Write(out)
	}
	return ParseMembers(text.String()), nil
}

// Update loads the members into a. A failed or empty load keeps the
// previous list: a hiccup in noscat must not lock the mesh out (or, worse,
// open the relay to nobody's list).
func (s MemberSource) Update(ctx context.Context, a *Allowlist) bool {
	pks, err := s.Load(ctx)
	switch {
	case err != nil:
		log.Printf("members: %s; keeping %d", err, a.Len())
	case len(pks) == 0:
		log.Printf("members: source listed nobody; keeping %d", a.Len())
	default:
		if before := a.Len(); before != len(pks) {
			log.Printf("members: %d (was %d)", len(pks), before)
		}
		a.Set(pks)
		return true
	}
	return false
}

// firstLoadRetry is how soon a failed first load is retried, doubling up
// to the refresh interval: at boot noscat may still be starting, and until
// a load succeeds the relay lets nobody in.
var firstLoadRetry = 2 * time.Second

// Refresh loads the members now, retrying soon until that succeeds, and
// then every interval.
func (s MemberSource) Refresh(ctx context.Context, a *Allowlist, every time.Duration) {
	for wait := firstLoadRetry; !s.Update(ctx, a); wait = min(wait*2, every) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Update(ctx, a)
		}
	}
}

// Reloader reloads the member list on demand, at most once per interval:
// a device that joined (or a mesh made private) moments ago is let in at
// once instead of at the next scheduled refresh. Callers wait for a reload
// in progress, so they see its result.
type Reloader struct {
	Every time.Duration
	Load  func()

	mu   sync.Mutex
	last time.Time
}

func (r *Reloader) Reload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.last) < r.Every {
		return
	}
	r.Load()
	r.last = time.Now()
}
