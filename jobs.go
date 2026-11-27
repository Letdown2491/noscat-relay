package main

import (
	"context"
	"log"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/nbd-wtf/go-nostr"
)

// pruneLoop deletes group messages and gift wraps older than the retention
// period, then reclaims badger's value log. Relay lists and key packages are
// replaceable (one per device), so they are left alone.
func pruneLoop(ctx context.Context, b *badger.BadgerBackend, retention time.Duration) {
	run := func() {
		n := prune(ctx, b, retention)
		if n > 0 {
			log.Printf("pruned %d events older than %s", n, retention)
		}
		for b.DB != nil && b.RunValueLogGC(0.5) == nil {
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// prune deletes in batches (a query returns at most maxLimit events) until
// nothing older than the cutoff is left.
func prune(ctx context.Context, b *badger.BadgerBackend, retention time.Duration) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cutoff := nostr.Timestamp(time.Now().Add(-retention).Unix())
	total := 0
	for ctx.Err() == nil {
		n := pruneBatch(ctx, b, cutoff)
		if n == 0 {
			break
		}
		total += n
	}
	return total
}

func pruneBatch(ctx context.Context, b *badger.BadgerBackend, cutoff nostr.Timestamp) int {
	ch, err := b.QueryEvents(ctx, nostr.Filter{Kinds: []int{KindGroup, KindGiftWrap, KindDeletion}, Until: &cutoff, Limit: maxLimit})
	if err != nil {
		log.Printf("prune: %s", err)
		return 0
	}
	// Always drain the channel (the producer does not watch ctx); stop
	// deleting once the deadline passes.
	n := 0
	for ev := range ch {
		if ctx.Err() != nil {
			continue
		}
		if err := b.DeleteEvent(ctx, ev); err != nil {
			log.Printf("prune: delete %s: %s", ev.ID, err)
			continue
		}
		n++
	}
	return n
}
