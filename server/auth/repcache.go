// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"container/list"
	"context"
	"sync"
	"time"

	"decred.org/dcrdex/server/account"
)

const (
	repCacheCapacity = 1 << 16
	repCacheMaxAge   = 10 * time.Minute
)

// cachedBond is the reputation-relevant subset of an active bond. lockTime is
// kept so bond expiry (no mesh event) is applied at tier computation time.
type cachedBond struct {
	strength uint32
	lockTime int64 // unix seconds
}

// repData is a user's cached score and active bonds. exists is false for an
// unknown account.
type repData struct {
	exists bool
	score  int32
	bonds  []cachedBond
}

// bondTier sums strengths with lockTime >= expiryThresh.
func (d *repData) bondTier(expiryThresh int64) int64 {
	var tier int64
	for _, bond := range d.bonds {
		if bond.lockTime >= expiryThresh {
			tier += int64(bond.strength)
		}
	}
	return tier
}

// repLoad is a shared in-flight fetch. invalidate detaches it (stale, removed
// from loads) so later readers start a fresh fetch; waiters already attached
// still receive this result, which is not cached.
type repLoad struct {
	stale bool
	done  chan struct{}
	data  *repData
	err   error
}

// repEntry is a cached repData with its load time.
type repEntry struct {
	user     account.AccountID
	data     *repData
	loadedAt time.Time
}

// repCache is a fixed-capacity LRU of user reputation data. Coherence is via
// storage's rep-inputs listener (NewAuthManager registers invalidate).
type repCache struct {
	mtx     sync.Mutex
	cap     int
	maxAge  time.Duration
	entries map[account.AccountID]*list.Element
	lru     *list.List // *repEntry values, front is most recently used
	loads   map[account.AccountID]*repLoad

	hits, misses, invalidations, evictions uint64
}

func newRepCache(capacity int, maxAge time.Duration) *repCache {
	return &repCache{
		cap:     capacity,
		maxAge:  maxAge,
		entries: make(map[account.AccountID]*list.Element),
		lru:     list.New(),
		loads:   make(map[account.AccountID]*repLoad),
	}
}

// get returns cached reputation data or loads via fetch. Concurrent misses
// share one fetch. The returned *repData is shared and must not be mutated.
func (c *repCache) get(ctx context.Context, user account.AccountID,
	fetch func(context.Context, account.AccountID) (*repData, error)) (*repData, error) {

	data, load, leader := c.probe(user)
	if load == nil { // cache hit
		return data, nil
	}
	if !leader {
		select {
		case <-load.done:
			return load.data, load.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	data, err := fetch(ctx, user)
	c.completeLoad(user, load, data, err)
	return data, err
}

// probe reports a hit (load == nil), an in-flight load to wait on
// (leader false), or a newly registered load the caller must fetch (leader true).
func (c *repCache) probe(user account.AccountID) (data *repData, load *repLoad, leader bool) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if elem, found := c.entries[user]; found {
		entry := elem.Value.(*repEntry)
		if time.Since(entry.loadedAt) < c.maxAge {
			c.lru.MoveToFront(elem)
			c.hits++
			return entry.data, nil, false
		}
		c.removeLocked(elem) // aged out
	}
	c.misses++
	if inFlight, running := c.loads[user]; running {
		return nil, inFlight, false
	}
	load = &repLoad{done: make(chan struct{})}
	c.loads[user] = load
	return nil, load, true
}

// completeLoad caches a non-stale result and unblocks waiters.
func (c *repCache) completeLoad(user account.AccountID, load *repLoad, data *repData, err error) {
	c.mtx.Lock()
	// Only drop our own entry; a successor may already be registered.
	if c.loads[user] == load {
		delete(c.loads, user)
	}
	if err == nil && !load.stale {
		c.storeLocked(user, data)
	}
	c.mtx.Unlock()

	load.data, load.err = data, err
	close(load.done)
}

// invalidate drops cached data and detaches in-flight loads. Call only after
// the DB transaction that changed reputation inputs has committed.
func (c *repCache) invalidate(users ...account.AccountID) {
	c.mtx.Lock()
	for _, user := range users {
		if elem, found := c.entries[user]; found {
			c.removeLocked(elem)
		}
		if load, running := c.loads[user]; running {
			load.stale = true
			delete(c.loads, user)
		}
		c.invalidations++
	}
	c.mtx.Unlock()
}

// stats returns cache activity counters.
func (c *repCache) stats() (hits, misses, invalidations, evictions uint64) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.hits, c.misses, c.invalidations, c.evictions
}

func (c *repCache) storeLocked(user account.AccountID, data *repData) {
	if elem, found := c.entries[user]; found {
		entry := elem.Value.(*repEntry)
		entry.data, entry.loadedAt = data, time.Now()
		c.lru.MoveToFront(elem)
		return
	}
	c.entries[user] = c.lru.PushFront(&repEntry{
		user:     user,
		data:     data,
		loadedAt: time.Now(),
	})
	for len(c.entries) > c.cap {
		c.removeLocked(c.lru.Back())
		c.evictions++
	}
}

func (c *repCache) removeLocked(elem *list.Element) {
	delete(c.entries, elem.Value.(*repEntry).user)
	c.lru.Remove(elem)
}
