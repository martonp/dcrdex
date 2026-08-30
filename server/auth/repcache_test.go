// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
)

func testAcctID(b byte) account.AccountID {
	var user account.AccountID
	user[0] = b
	return user
}

func fixedRepFetcher(score int32, calls *int32) func(context.Context, account.AccountID) (*repData, error) {
	return func(context.Context, account.AccountID) (*repData, error) {
		atomic.AddInt32(calls, 1)
		return &repData{exists: true, score: score}, nil
	}
}

// blockedGet is a c.get that blocks inside fetch until release is called.
type blockedGet struct {
	wait    <-chan struct{} // closed when fetch starts
	release func()
	done    <-chan struct{} // closed when get returns
	data    *repData
	err     error
}

// startBlockedGet runs c.get in a goroutine with a fetch that signals on wait,
// then blocks until release. Optional calls is incremented when the fetch runs.
func startBlockedGet(c *repCache, user account.AccountID, score int32, calls *int32) *blockedGet {
	fetching := make(chan struct{})
	proceed := make(chan struct{})
	finished := make(chan struct{})
	bg := &blockedGet{
		wait:    fetching,
		release: func() { close(proceed) },
		done:    finished,
	}
	go func() {
		defer close(finished)
		bg.data, bg.err = c.get(context.Background(), user, func(context.Context, account.AccountID) (*repData, error) {
			if calls != nil {
				atomic.AddInt32(calls, 1)
			}
			close(fetching)
			<-proceed
			return &repData{exists: true, score: score}, nil
		})
	}()
	return bg
}

func TestRepCacheHitMissInvalidate(t *testing.T) {
	c := newRepCache(8, time.Hour)
	user := testAcctID(1)
	ctx := context.Background()

	var calls int32
	for i := 0; i < 3; i++ {
		data, err := c.get(ctx, user, fixedRepFetcher(-5, &calls))
		if err != nil {
			t.Fatalf("get error: %v", err)
		}
		if data.score != -5 {
			t.Fatalf("wrong score %d", data.score)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 fetch, got %d", calls)
	}
	hits, misses, _, _ := c.stats()
	if hits != 2 || misses != 1 {
		t.Fatalf("expected 2 hits and 1 miss, got %d and %d", hits, misses)
	}

	c.invalidate(user)
	if _, err := c.get(ctx, user, fixedRepFetcher(-6, &calls)); err != nil {
		t.Fatalf("get error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected refetch after invalidation, got %d fetches", calls)
	}
	if data, _ := c.get(ctx, user, fixedRepFetcher(0, &calls)); data.score != -6 {
		t.Fatalf("expected cached score -6, got %d", data.score)
	}
}

func TestRepCacheErrorNotCached(t *testing.T) {
	c := newRepCache(8, time.Hour)
	user := testAcctID(1)
	ctx := context.Background()

	fetchErr := errors.New("db down")
	if _, err := c.get(ctx, user, func(context.Context, account.AccountID) (*repData, error) {
		return nil, fetchErr
	}); !errors.Is(err, fetchErr) {
		t.Fatalf("expected fetch error, got %v", err)
	}

	var calls int32
	if data, err := c.get(ctx, user, fixedRepFetcher(3, &calls)); err != nil || data.score != 3 {
		t.Fatalf("get after error: %v, %v", data, err)
	}
	if calls != 1 {
		t.Fatalf("expected a fresh fetch after an error, got %d fetches", calls)
	}
}

func TestRepCacheLRUEviction(t *testing.T) {
	c := newRepCache(2, time.Hour)
	ctx := context.Background()
	var calls int32
	fetch := fixedRepFetcher(1, &calls)

	userA, userB, userC := testAcctID(1), testAcctID(2), testAcctID(3)
	c.get(ctx, userA, fetch)
	c.get(ctx, userB, fetch)
	c.get(ctx, userA, fetch) // A more recent than B
	c.get(ctx, userC, fetch) // evicts B

	if calls != 3 {
		t.Fatalf("expected 3 fetches, got %d", calls)
	}
	c.get(ctx, userA, fetch)
	if calls != 3 {
		t.Fatal("user A should not have been evicted")
	}
	c.get(ctx, userB, fetch)
	if calls != 4 {
		t.Fatal("user B should have been evicted")
	}
	if _, _, _, evictions := c.stats(); evictions != 2 {
		t.Fatalf("expected 2 evictions, got %d", evictions)
	}
}

func TestRepCacheMaxAge(t *testing.T) {
	c := newRepCache(8, time.Millisecond)
	user := testAcctID(1)
	ctx := context.Background()
	var calls int32
	fetch := fixedRepFetcher(1, &calls)

	c.get(ctx, user, fetch)
	time.Sleep(5 * time.Millisecond)
	c.get(ctx, user, fetch)
	if calls != 2 {
		t.Fatalf("expected the aged-out entry to be refetched, got %d fetches", calls)
	}
}

func TestRepCacheSingleflight(t *testing.T) {
	c := newRepCache(8, time.Hour)
	user := testAcctID(1)

	var calls int32
	leader := startBlockedGet(c, user, 9, &calls)
	<-leader.wait

	const followers = 5
	results := make(chan int32, followers)
	var wg sync.WaitGroup
	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.get(context.Background(), user, fixedRepFetcher(9, &calls))
			if err != nil {
				t.Errorf("follower get error: %v", err)
				results <- 0
				return
			}
			results <- data.score
		}()
	}

	time.Sleep(10 * time.Millisecond) // let followers attach
	leader.release()
	<-leader.done
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected a single shared fetch, got %d", got)
	}
	for i := 0; i < followers; i++ {
		if score := <-results; score != 9 {
			t.Fatalf("follower got score %d", score)
		}
	}
}

func TestRepDataBondTier(t *testing.T) {
	now := time.Now().Unix()
	data := &repData{
		exists: true,
		bonds: []cachedBond{
			{strength: 1, lockTime: now - 1},    // expired
			{strength: 2, lockTime: now},        // boundary inclusive
			{strength: 4, lockTime: now + 3600}, // active
		},
	}
	if tier := data.bondTier(now); tier != 6 {
		t.Fatalf("expected bond tier 6, got %d", tier)
	}
	if tier := data.bondTier(now + 7200); tier != 0 {
		t.Fatalf("expected all bonds expired, got tier %d", tier)
	}
	if tier := (&repData{}).bondTier(now); tier != 0 {
		t.Fatalf("expected zero tier for unknown account, got %d", tier)
	}
}

// Post-invalidate get must fetch fresh; stale leader must not clobber the cache.
func TestRepCacheInvalidateDetachesLoad(t *testing.T) {
	c := newRepCache(8, time.Hour)
	user := testAcctID(1)

	stale := startBlockedGet(c, user, -100, nil)
	<-stale.wait
	c.invalidate(user)

	var calls int32
	data, err := c.get(context.Background(), user, fixedRepFetcher(7, &calls))
	if err != nil {
		t.Fatalf("get error: %v", err)
	}
	if data.score != 7 || calls != 1 {
		t.Fatalf("post-invalidation get: score=%d calls=%d, want 7/1", data.score, calls)
	}

	stale.release()
	<-stale.done
	if stale.err != nil || stale.data.score != -100 {
		t.Errorf("stale leader got %v, %v", stale.data, stale.err)
	}
	if _, err := c.get(context.Background(), user, fixedRepFetcher(0, &calls)); err != nil {
		t.Fatalf("get error: %v", err)
	}
	if calls != 1 {
		t.Fatal("fresh entry was lost after the stale leader completed")
	}
}

// Detached stale leader must not remove a successor load from the loads map.
func TestRepCacheStaleLeaderLeavesSuccessor(t *testing.T) {
	c := newRepCache(8, time.Hour)
	user := testAcctID(1)

	stale := startBlockedGet(c, user, -1, nil)
	<-stale.wait
	c.invalidate(user)

	succ := startBlockedGet(c, user, 2, nil)
	<-succ.wait

	stale.release()
	<-stale.done

	var calls int32
	thirdDone := make(chan struct{})
	go func() {
		defer close(thirdDone)
		data, err := c.get(context.Background(), user, fixedRepFetcher(99, &calls))
		if err != nil || data.score != 2 {
			t.Errorf("third get got %v, %v", data, err)
		}
	}()

	time.Sleep(10 * time.Millisecond) // let third get attach
	succ.release()
	<-succ.done
	<-thirdDone
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("third get fetched on its own (%d fetches) instead of sharing the successor", got)
	}
}

func TestReputationFromDBAccountError(t *testing.T) {
	authMgr, storage := newEventTestAuthManager(t)
	user := testAcctID(5)

	storage.accountReadErr = errors.New("db down")
	if _, _, err := authMgr.reputationFromDB(context.Background(), user); err == nil {
		t.Fatal("expected an error from a failed account read")
	}

	// Failure must not have been cached as a nonexistent account.
	storage.accountReadErr = nil
	storage.acct = &account.Account{ID: user}
	storage.bonds = []*db.Bond{{Strength: 2, LockTime: time.Now().Add(48 * time.Hour).Unix()}}
	before := time.Now().Add(authMgr.bondExpiry).Unix()
	rep, _, err := authMgr.reputationFromDB(context.Background(), user)
	after := time.Now().Add(authMgr.bondExpiry).Unix()
	if err != nil {
		t.Fatalf("reputationFromDB error after recovery: %v", err)
	}
	if rep == nil {
		t.Fatal("user read as unknown after a transient account read error")
	}
	if rep.BondedTier != 2 {
		t.Fatalf("bonded tier = %d, want 2", rep.BondedTier)
	}
	if rep.BondExpiryThreshold < before || rep.BondExpiryThreshold > after {
		t.Fatalf("bond expiry threshold = %d, want between %d and %d", rep.BondExpiryThreshold, before, after)
	}
}
