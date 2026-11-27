package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func TestConfigureMemoryRespectsExplicitLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	const want = 1500 << 20 // simulate operator GOMEMLIMIT=1500MiB
	debug.SetMemoryLimit(want)
	if got := configureMemory(); got != want {
		t.Fatalf("configureMemory() = %d, want %d", got, want)
	}
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("GOMEMLIMIT was overridden: got %d, want %d", got, want)
	}
}

const (
	memberPK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	joinerPK = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	randomPK = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var fixedNow = time.Unix(1_800_000_000, 0)

func testPolicy() *Policy {
	return &Policy{
		Members:         NewAllowlist(memberPK),
		Window:          15 * time.Minute,
		MaxContentBytes: 1024,
		Now:             func() time.Time { return fixedNow },
	}
}

func ev(kind int, pubkey string, age time.Duration, tags ...nostr.Tag) *nostr.Event {
	return &nostr.Event{Kind: kind, PubKey: pubkey, CreatedAt: nostr.Timestamp(fixedNow.Add(-age).Unix()), Tags: tags}
}

func TestCheckEvent(t *testing.T) {
	h := nostr.Tag{"h", "00"}
	cases := []struct {
		name   string
		evt    *nostr.Event
		authed string
		reject bool
	}{
		{"not a noscat kind", ev(1, memberPK, 0), memberPK, true},
		{"not logged in", ev(KindGroup, randomPK, 0, h), "", true},
		{"member group message", ev(KindGroup, randomPK, 0, h), memberPK, false},
		{"group message without h", ev(KindGroup, randomPK, 0), memberPK, true},
		{"non-member group message", ev(KindGroup, randomPK, 0, h), joinerPK, true},
		{"joiner key package", ev(KindKeyPackage, joinerPK, 0), joinerPK, false},
		{"joiner inbox relays", ev(KindInbox, joinerPK, 0), joinerPK, false},
		{"someone else's key package", ev(KindKeyPackage, memberPK, 0), joinerPK, true},
		{"joiner gift wrap (join request)", ev(KindGiftWrap, randomPK, 0), joinerPK, false},
		{"gift wrap backdated two days", ev(KindGiftWrap, randomPK, 47*time.Hour), memberPK, false},
		{"gift wrap backdated three days", ev(KindGiftWrap, randomPK, 72*time.Hour), memberPK, true},
		{"group message an hour old", ev(KindGroup, randomPK, time.Hour, h), memberPK, true},
		{"from the future", ev(KindGroup, randomPK, -time.Hour, h), memberPK, true},
		{"deleting own key package", ev(KindDeletion, joinerPK, 0, nostr.Tag{"a", "30443:" + joinerPK + ":ab"}, nostr.Tag{"k", "30443"}), joinerPK, false},
		{"deletion not logged in", ev(KindDeletion, joinerPK, 0, nostr.Tag{"a", "30443:" + joinerPK + ":ab"}), "", true},
		{"deleting someone else's key package", ev(KindDeletion, joinerPK, 0, nostr.Tag{"a", "30443:" + memberPK + ":ab"}), joinerPK, true},
		{"deletion signed by someone else", ev(KindDeletion, memberPK, 0, nostr.Tag{"a", "30443:" + memberPK + ":ab"}), joinerPK, true},
		{"deleting another kind", ev(KindDeletion, joinerPK, 0, nostr.Tag{"a", "10002:" + joinerPK + ":"}), joinerPK, true},
		{"deleting by event id", ev(KindDeletion, joinerPK, 0, nostr.Tag{"e", "00"}), joinerPK, true},
		{"deletion naming nothing", ev(KindDeletion, joinerPK, 0), joinerPK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := testPolicy().CheckEvent(tc.evt, tc.authed, "")
			if got != tc.reject {
				t.Fatalf("CheckEvent = %v (%q), want %v", got, msg, tc.reject)
			}
		})
	}
}

func TestCheckEventLimits(t *testing.T) {
	p := testPolicy()
	big := ev(KindGiftWrap, randomPK, 0)
	big.Content = string(make([]byte, 2048))
	if r, _ := p.CheckEvent(big, memberPK, ""); !r {
		t.Fatal("oversized content accepted")
	}
	p.Groups = map[string]bool{"11": true}
	if r, _ := p.CheckEvent(ev(KindGroup, randomPK, 0, nostr.Tag{"h", "22"}), memberPK, ""); !r {
		t.Fatal("group message for another group accepted")
	}
	if r, msg := p.CheckEvent(ev(KindGroup, randomPK, 0, nostr.Tag{"h", "11"}), memberPK, ""); r {
		t.Fatalf("group message for a served group rejected: %s", msg)
	}
	p.JoinLimit = NewRateLimiter(2, time.Minute)
	for i := 0; i < 2; i++ {
		if r, msg := p.CheckEvent(ev(KindGiftWrap, randomPK, 0), joinerPK, "1.2.3.4"); r {
			t.Fatalf("join %d rejected: %s", i, msg)
		}
	}
	if r, _ := p.CheckEvent(ev(KindGiftWrap, randomPK, 0), joinerPK, "1.2.3.4"); !r {
		t.Fatal("joiner past its rate limit accepted")
	}
}

func TestCheckFilter(t *testing.T) {
	h := nostr.TagMap{"h": {"00"}}
	cases := []struct {
		name   string
		filter nostr.Filter
		authed string
		reject bool
	}{
		{"not logged in", nostr.Filter{Kinds: []int{KindGroup}, Tags: h}, "", true},
		{"no kinds", nostr.Filter{Tags: h}, memberPK, true},
		{"member groups by h", nostr.Filter{Kinds: []int{KindGroup}, Tags: h}, memberPK, false},
		{"member groups unscoped", nostr.Filter{Kinds: []int{KindGroup}}, memberPK, true},
		{"non-member groups", nostr.Filter{Kinds: []int{KindGroup}, Tags: h}, joinerPK, true},
		{"own gift wraps", nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {joinerPK}}}, joinerPK, false},
		{"someone else's gift wraps", nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {memberPK}}}, joinerPK, true},
		{"gift wraps unscoped", nostr.Filter{Kinds: []int{KindGiftWrap}}, memberPK, true},
		{"member key packages by author", nostr.Filter{Kinds: []int{KindKeyPackage}, Authors: []string{joinerPK}}, memberPK, false},
		{"non-member key packages", nostr.Filter{Kinds: []int{KindKeyPackage}, Authors: []string{memberPK}}, joinerPK, true},
		{"joiner reads inbox relays", nostr.Filter{Kinds: []int{KindInbox}, Authors: []string{memberPK}}, joinerPK, false},
		{"relay lists unscoped", nostr.Filter{Kinds: []int{KindRelayList}}, memberPK, true},
		{"other kinds: empty answer", nostr.Filter{Kinds: []int{1}}, joinerPK, false},
		{"deletion requests", nostr.Filter{Kinds: []int{KindDeletion}, Authors: []string{joinerPK}}, memberPK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := testPolicy().CheckFilter(tc.filter, tc.authed)
			if got != tc.reject {
				t.Fatalf("CheckFilter = %v (%q), want %v", got, msg, tc.reject)
			}
		})
	}
}

func TestParseMembers(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	npub := mustNpub(t, pk)
	text := "mesh \"home\", 3 member(s)\n" +
		npub + "  motherbox            this device\n" +
		"# " + randomPK + " commented out\n" +
		memberPK + "\n" +
		npub + " again\n" +
		"npub1bad not one\n"
	got := ParseMembers(text)
	if len(got) != 2 || got[0] != pk || got[1] != memberPK {
		t.Fatalf("ParseMembers = %v, want [%s %s]", got, pk, memberPK)
	}
}

// A member the list does not have yet is let in after an on-demand reload,
// and reloads are rate limited.
func TestRecheckReloadsOnMiss(t *testing.T) {
	p := testPolicy()
	loads := 0
	r := &Reloader{Every: time.Hour, Load: func() { loads++; p.Members.Set([]string{memberPK, joinerPK}) }}
	p.OnMiss = r.Reload
	h := nostr.Tag{"h", "00"}
	if reject, msg := p.CheckEvent(ev(KindGroup, randomPK, 0, h), joinerPK, ""); reject {
		t.Fatalf("new member refused after reload: %s", msg)
	}
	if reject, _ := p.CheckFilter(nostr.Filter{Kinds: []int{KindGroup}, Tags: nostr.TagMap{"h": {"00"}}}, randomPK); !reject {
		t.Fatal("outsider let in")
	}
	if loads != 1 {
		t.Fatalf("%d reloads, want 1 (rate limited)", loads)
	}
}

// At boot the members command can fail (noscat still starting): the first
// load is retried soon, not after a whole refresh interval.
func TestRefreshRetriesAFailedFirstLoad(t *testing.T) {
	prev := firstLoadRetry
	firstLoadRetry = 50 * time.Millisecond
	t.Cleanup(func() { firstLoadRetry = prev })
	file := filepath.Join(t.TempDir(), "members")
	a := NewAllowlist()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go MemberSource{File: file}.Refresh(ctx, a, time.Hour)
	time.Sleep(100 * time.Millisecond)
	if a.Len() != 0 {
		t.Fatalf("loaded %d before the file existed", a.Len())
	}
	pk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(pk)
	if err := os.WriteFile(file, []byte(pub+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.Len() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the failed first load was not retried")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The list that loaded last time is served at the next start, before the
// source answers (at boot noscat may be waiting for this very relay).
func TestTheLastListIsServedUntilTheSourceAnswers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "members")
	pk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(pk)
	if err := os.WriteFile(file, []byte(pub+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := MemberSource{File: file, Cache: filepath.Join(dir, "members.cache")}
	if !src.Update(context.Background(), NewAllowlist()) {
		t.Fatal("first load failed")
	}
	// Next start: the source is gone for now.
	os.Remove(file)
	a := NewAllowlist()
	src.LoadCache(a)
	if !a.Has(pub) || a.Len() != 1 {
		t.Fatalf("cache not served: %d members", a.Len())
	}
}
