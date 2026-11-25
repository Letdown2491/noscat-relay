package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

func mustNpub(t *testing.T, pk string) string {
	t.Helper()
	n, err := nip19.EncodePublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type device struct {
	sk, pk string
	relay  *nostr.Relay
}

// login connects and authenticates with NIP-42. khatru sends its challenge
// with the first auth-required refusal, so a refused probe comes first.
func login(t *testing.T, ctx context.Context, url string) *device {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	r, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// No explicit Close: go-nostr's client races its own ping loop there
	// (relay.go, v0.52.3); the test context ends the connection.
	probe := nostr.Event{Kind: KindInbox, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"relay", "wss://x"}}}
	_ = probe.Sign(sk)
	if err := r.Publish(ctx, probe); err == nil || !strings.Contains(err.Error(), "auth-required") {
		t.Fatalf("unauthenticated publish: got %v, want auth-required", err)
	}
	if err := r.Auth(ctx, func(e *nostr.Event) error { return e.Sign(sk) }); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return &device{sk: sk, pk: pk, relay: r}
}

func (d *device) publish(ctx context.Context, kind int, tags nostr.Tags, content string) error {
	signer := d.sk
	if kind == KindGroup || kind == KindGiftWrap {
		signer = nostr.GeneratePrivateKey() // random per event, like noscat
	}
	e := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Tags: tags, Content: content}
	_ = e.Sign(signer)
	return d.relay.Publish(ctx, e)
}

func (d *device) query(ctx context.Context, f nostr.Filter) ([]*nostr.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sub, err := d.relay.Subscribe(ctx, nostr.Filters{f})
	if err != nil {
		return nil, err
	}
	defer sub.Unsub()
	var out []*nostr.Event
	for {
		select {
		case e := <-sub.Events:
			out = append(out, e)
		case <-sub.EndOfStoredEvents:
			return out, nil
		case reason := <-sub.ClosedReason:
			return nil, &closedError{reason}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type closedError struct{ reason string }

func (e *closedError) Error() string { return e.reason }

// A member, a joining device and an outsider against a live relay: the
// member uses the mesh, the joiner can publish its key package and a join
// request and read its own gift wraps, nobody else gets in.
func TestRelayEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	members := NewAllowlist()
	policy := &Policy{Members: members, Window: 15 * time.Minute, MaxContentBytes: 256 << 10}

	srv := httptest.NewUnstartedServer(nil)
	url := "ws://" + srv.Listener.Addr().String()
	rl := newRelay(Config{RelayURL: url, RetentionDays: 30, MaxContentKB: 256}, store, policy)
	srv.Config.Handler = rl
	srv.Start()
	defer srv.Close()

	member := login(t, ctx, url)
	joiner := login(t, ctx, url)
	members.Set([]string{member.pk})
	h := nostr.Tags{{"h", strings.Repeat("ab", 32)}}

	if err := member.publish(ctx, KindGroup, h, "advert"); err != nil {
		t.Fatalf("member group message: %v", err)
	}
	if err := joiner.publish(ctx, KindGroup, h, "intrusion"); err == nil {
		t.Fatal("non-member group message accepted")
	}
	if err := joiner.publish(ctx, KindKeyPackage, nostr.Tags{{"d", "0"}}, "kp"); err != nil {
		t.Fatalf("joiner key package: %v", err)
	}
	if err := joiner.publish(ctx, KindGiftWrap, nostr.Tags{{"p", member.pk}}, "join request"); err != nil {
		t.Fatalf("joiner join request: %v", err)
	}
	if err := member.publish(ctx, KindGiftWrap, nostr.Tags{{"p", joiner.pk}}, "welcome"); err != nil {
		t.Fatalf("member welcome: %v", err)
	}
	if err := member.publish(ctx, 1, nil, "a note"); err == nil {
		t.Fatal("kind 1 accepted")
	}

	got, err := member.query(ctx, nostr.Filter{Kinds: []int{KindGroup}, Tags: nostr.TagMap{"h": {h[0][1]}}})
	if err != nil || len(got) != 1 || got[0].Content != "advert" {
		t.Fatalf("member reads group: %v, %v", got, err)
	}
	if _, err := joiner.query(ctx, nostr.Filter{Kinds: []int{KindGroup}, Tags: nostr.TagMap{"h": {h[0][1]}}}); err == nil {
		t.Fatal("non-member read group messages")
	}
	got, err = member.query(ctx, nostr.Filter{Kinds: []int{KindKeyPackage}, Authors: []string{joiner.pk}})
	if err != nil || len(got) != 1 {
		t.Fatalf("member reads joiner key package: %v, %v", got, err)
	}
	got, err = member.query(ctx, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {member.pk}}})
	if err != nil || len(got) != 1 || got[0].Content != "join request" {
		t.Fatalf("member reads its join request: %v, %v", got, err)
	}
	got, err = joiner.query(ctx, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {joiner.pk}}})
	if err != nil || len(got) != 1 || got[0].Content != "welcome" {
		t.Fatalf("joiner reads its welcome: %v, %v", got, err)
	}
	if _, err := joiner.query(ctx, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {member.pk}}}); err == nil {
		t.Fatal("joiner read someone else's gift wraps")
	}

	// The joiner is invited: now a member, it reads the group.
	members.Set([]string{member.pk, joiner.pk})
	got, err = joiner.query(ctx, nostr.Filter{Kinds: []int{KindGroup}, Tags: nostr.TagMap{"h": {h[0][1]}}})
	if err != nil || len(got) != 1 {
		t.Fatalf("new member reads group: %v, %v", got, err)
	}

	// Removed while subscribed: no more group messages are delivered live.
	sub, err := joiner.relay.Subscribe(ctx, nostr.Filters{{Kinds: []int{KindGroup}, Tags: nostr.TagMap{"h": {h[0][1]}}, Since: func() *nostr.Timestamp { n := nostr.Now(); return &n }()}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsub()
	// Stored events from this second may arrive before EOSE; drain them.
	for eose := false; !eose; {
		select {
		case <-sub.Events:
		case <-sub.EndOfStoredEvents:
			eose = true
		case r := <-sub.ClosedReason:
			t.Fatalf("live subscription closed: %s", r)
		case <-time.After(3 * time.Second):
			t.Fatal("no EOSE on the live subscription")
		}
	}
	members.Set([]string{member.pk})
	if err := member.publish(ctx, KindGroup, h, "after removal"); err != nil {
		t.Fatal(err)
	}
	select {
	case e, ok := <-sub.Events:
		if ok {
			t.Fatalf("removed member received %q live", e.Content)
		}
	case <-time.After(500 * time.Millisecond):
	}
}

// Pruning removes old group messages and gift wraps, keeps fresh ones and
// relay lists.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sk := nostr.GeneratePrivateKey()
	save := func(kind int, age time.Duration) {
		e := nostr.Event{Kind: kind, CreatedAt: nostr.Timestamp(time.Now().Add(-age).Unix()), Tags: nostr.Tags{{"h", "00"}}}
		_ = e.Sign(sk)
		if err := store.SaveEvent(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	save(KindGroup, 40*24*time.Hour)
	save(KindGiftWrap, 40*24*time.Hour)
	save(KindRelayList, 40*24*time.Hour)
	save(KindGroup, time.Hour)
	if n := prune(ctx, store, 30*24*time.Hour); n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	ch, _ := store.QueryEvents(ctx, nostr.Filter{Kinds: []int{KindGroup, KindGiftWrap, KindRelayList}})
	left := 0
	for range ch {
		left++
	}
	if left != 2 {
		t.Fatalf("%d events left, want 2", left)
	}
}
