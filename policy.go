package main

import (
	"context"
	"fmt"
	"time"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// The noscat event kinds (docs/nip-noscat.md in the noscat repository).
const (
	KindKeyPackage = 30443 // a device's MLS key package, authored by the device
	KindInbox      = 10050 // where a device receives gift wraps, authored by the device
	KindRelayList  = 10002 // a device's relay list, authored by the device
	KindGiftWrap   = 1059  // NIP-59 gift wrap (Welcome, join request), random signer
	KindGroup      = 445   // Marmot group message (commit, advert), random signer
)

// giftWrapBackdate is how far NIP-59 lets a gift wrap's created_at lie in
// the past (noscat randomizes it up to two days back).
const giftWrapBackdate = 48 * time.Hour

func isNoscatKind(k int) bool {
	switch k {
	case KindKeyPackage, KindInbox, KindRelayList, KindGiftWrap, KindGroup:
		return true
	}
	return false
}

// ownedKind: events of these kinds are signed by the device itself, so
// their author must be the authenticated device.
func ownedKind(k int) bool {
	return k == KindKeyPackage || k == KindInbox || k == KindRelayList
}

// Policy decides what the relay accepts. Writes and reads need a NIP-42
// login. Mesh members may use every noscat kind; a device that is not a
// member yet (joining) may publish its own key package and relay lists and
// gift wraps, and read gift wraps addressed to it plus relay lists, so it
// can join through this relay alone. Group messages are members only.
type Policy struct {
	Members         *Allowlist
	Groups          map[string]bool // allowed #h routing ids (hex); empty = any
	Window          time.Duration   // accepted clock skew for created_at
	MaxContentBytes int
	MemberLimit     *RateLimiter // per authenticated member npub
	JoinLimit       *RateLimiter // per non-member npub and per IP
	Now             func() time.Time
	// OnMiss reloads the member list (rate limited, see Reloader) when a
	// logged-in key that is not a member asks for something only members
	// get: it may have joined since the last refresh.
	OnMiss func()
}

// recheck reloads the members (when OnMiss is set) and asks again.
func (p *Policy) recheck(pk string) bool {
	if p.OnMiss == nil {
		return false
	}
	p.OnMiss()
	return p.Members.Has(pk)
}

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func firstTag(evt *nostr.Event, name string) (string, bool) {
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1], true
		}
	}
	return "", false
}

// CheckEvent decides one EVENT. `authed` is the NIP-42 pubkey (hex, "" when
// not logged in) and `ip` the client address. Reasons use the NIP-01
// machine-readable prefixes; "auth-required:" makes khatru send a challenge.
func (p *Policy) CheckEvent(evt *nostr.Event, authed, ip string) (reject bool, msg string) {
	if !isNoscatKind(evt.Kind) {
		return true, "blocked: this relay only carries noscat events"
	}
	if authed == "" {
		return true, "auth-required: log in with your device key"
	}
	if p.MaxContentBytes > 0 && len(evt.Content) > p.MaxContentBytes {
		return true, fmt.Sprintf("invalid: content larger than %d bytes", p.MaxContentBytes)
	}

	// Timestamps: a gift wrap may be backdated (NIP-59); nothing may be
	// from the future beyond the clock skew window.
	now := p.now()
	ts := evt.CreatedAt.Time()
	oldest := now.Add(-p.Window)
	if evt.Kind == KindGiftWrap {
		oldest = oldest.Add(-giftWrapBackdate)
	}
	if ts.Before(oldest) || ts.After(now.Add(p.Window)) {
		return true, "invalid: created_at is outside the accepted time window"
	}

	if ownedKind(evt.Kind) && evt.PubKey != authed {
		return true, "restricted: publish only your own key package and relay lists"
	}

	member := p.Members.Has(authed)
	if evt.Kind == KindGroup {
		if !member {
			member = p.recheck(authed)
		}
		if !member {
			return true, "restricted: group messages are for mesh members only"
		}
		h, ok := firstTag(evt, "h")
		if !ok {
			return true, "invalid: group message without an h tag"
		}
		if len(p.Groups) > 0 && !p.Groups[h] {
			return true, "restricted: not a group this relay serves"
		}
	}

	if member {
		if p.MemberLimit != nil && !p.MemberLimit.Allow(authed) {
			return true, "rate-limited: slow down"
		}
	} else if p.JoinLimit != nil && (!p.JoinLimit.Allow("pk:"+authed) || (ip != "" && !p.JoinLimit.Allow("ip:"+ip))) {
		return true, "rate-limited: slow down"
	}
	return false, ""
}

// CheckFilter decides one REQ filter. Every query must name its kinds and
// be scoped: group messages by #h, gift wraps by #p (only the reader's own
// key), device-authored kinds by authors. Non-members may read only gift
// wraps addressed to them and relay lists.
func (p *Policy) CheckFilter(f nostr.Filter, authed string) (reject bool, msg string) {
	if authed == "" {
		return true, "auth-required: log in with your device key"
	}
	if len(f.Kinds) == 0 {
		return true, "blocked: queries must name their kinds"
	}
	member := p.Members.Has(authed)
	for _, k := range f.Kinds {
		if !isNoscatKind(k) {
			continue // matches nothing stored: an empty EOSE
		}
		switch k {
		case KindGroup:
			if !member {
				member = p.recheck(authed)
			}
			if !member {
				return true, "restricted: group messages are for mesh members only"
			}
			if len(f.Tags["h"]) == 0 {
				return true, "blocked: group message queries must be scoped by #h"
			}
		case KindGiftWrap:
			ps := f.Tags["p"]
			if len(ps) == 0 {
				return true, "blocked: gift wrap queries must be scoped by #p"
			}
			for _, pk := range ps {
				if pk != authed {
					return true, "restricted: read only gift wraps addressed to you"
				}
			}
		case KindKeyPackage:
			if !member {
				member = p.recheck(authed)
			}
			if !member {
				return true, "restricted: key packages are for mesh members only"
			}
			fallthrough
		default: // relay lists
			if len(f.Authors) == 0 {
				return true, "blocked: queries must be scoped by authors"
			}
		}
	}
	return false, ""
}

// keepFromLiveDelivery: whether a stored event must not be pushed to an
// open subscription now. Membership can end while a subscription is open,
// so group messages and key packages are re-checked at delivery time.
func (p *Policy) keepFromLiveDelivery(ws *khatru.WebSocket, evt *nostr.Event) bool {
	switch evt.Kind {
	case KindGroup, KindKeyPackage:
		return !p.Members.Has(ws.AuthedPublicKey)
	}
	return false
}

// Install wires the policy into a khatru relay.
func (p *Policy) Install(relay *khatru.Relay) {
	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, evt *nostr.Event) (bool, string) {
		return p.CheckEvent(evt, khatru.GetAuthed(ctx), khatru.GetIP(ctx))
	})
	relay.RejectFilter = append(relay.RejectFilter, func(ctx context.Context, f nostr.Filter) (bool, string) {
		return p.CheckFilter(f, khatru.GetAuthed(ctx))
	})
	relay.RejectCountFilter = append(relay.RejectCountFilter, func(context.Context, nostr.Filter) (bool, string) {
		return true, "blocked: this relay does not count events"
	})
	relay.PreventBroadcast = append(relay.PreventBroadcast, p.keepFromLiveDelivery)
}
