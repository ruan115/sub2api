package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A cache entry only becomes readable once the request that writes it starts
// streaming a response. Until then every concurrent request sharing the same
// prefix is a cache miss, so N parallel large requests on one session pay N
// full-price cache creations instead of one creation and N-1 cheap reads.
//
// Serialising them per (account, session) restores the intended shape: the
// first request writes the cache and the rest read it. This is not throttling —
// the later requests come out cheaper, not merely later.
const coldCacheFlightTimeout = 2 * time.Minute

type coldCacheFlight struct {
	slot chan struct{}
	refs int
}

// coldCacheFlights is guarded by a plain mutex rather than a sync.Map: entries
// have to be reference-counted so the last releaser can remove them, and a
// load-then-delete race would hand a waiter a brand-new channel and silently
// break the exclusion this exists to provide.
type coldCacheFlightTable struct {
	mu      sync.Mutex
	flights map[string]*coldCacheFlight
}

func (t *coldCacheFlightTable) checkout(key string) *coldCacheFlight {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.flights == nil {
		t.flights = map[string]*coldCacheFlight{}
	}
	flight := t.flights[key]
	if flight == nil {
		flight = &coldCacheFlight{slot: make(chan struct{}, 1)}
		t.flights[key] = flight
	}
	flight.refs++
	return flight
}

func (t *coldCacheFlightTable) checkin(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	flight := t.flights[key]
	if flight == nil {
		return
	}
	flight.refs--
	if flight.refs <= 0 {
		delete(t.flights, key)
	}
}

// acquireColdCacheFlight blocks until this (account, session) has no other large
// request in flight. The returned release must be called exactly once; it is a
// no-op when the request did not need serialising.
func (a *app) acquireColdCacheFlight(ctx context.Context, accountID int64, sessionHash string, large bool) (func(), error) {
	noop := func() {}
	if !large || accountID <= 0 || sessionHash == "" {
		return noop, nil
	}
	key := fmt.Sprintf("%d:%s", accountID, sessionHash)
	flight := a.coldCacheFlights.checkout(key)
	unwind := func() { a.coldCacheFlights.checkin(key) }

	timer := time.NewTimer(coldCacheFlightTimeout)
	defer timer.Stop()
	select {
	case flight.slot <- struct{}{}:
	case <-ctx.Done():
		unwind()
		return noop, ctx.Err()
	case <-timer.C:
		// Waiting longer than an upstream call should ever take means the holder
		// is wedged. Serving uncached is expensive, but blocking forever is
		// worse, so let this one through rather than failing it.
		unwind()
		return noop, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-flight.slot
			unwind()
		})
	}, nil
}

// The upstream reports remaining input tokens rounded to the nearest thousand,
// so comparing raw values would reorder the pool on noise. Bucketing keeps
// cold-start placement stable while still preferring an account with real
// headroom, and leaves the existing tie-breaks to decide within a bucket.
const coldStartBudgetBucketSize = 100_000

func coldStartBudgetBucket(remaining int64) int64 {
	if remaining < 0 {
		return -1
	}
	return remaining / coldStartBudgetBucketSize
}
