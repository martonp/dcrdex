// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

type tBookSource struct {
	users map[account.AccountID]int
}

func (s *tBookSource) BookedUsers() map[account.AccountID]int {
	users := make(map[account.AccountID]int, len(s.users))
	for user, count := range s.users {
		users[user] = count
	}
	return users
}

// tPresenceMesh answers connectivity queries with the intersection of the
// queried users and the configured connected set, like a real peer would.
type tPresenceMesh struct {
	connected map[account.AccountID]struct{}
	queryErr  error
	failures  int // fail this many queries, then answer normally
	queries   [][]account.AccountID
	applied   []*mesh.Event
	applyErr  error
}

func (m *tPresenceMesh) setConnected(users ...account.AccountID) {
	m.connected = make(map[account.AccountID]struct{}, len(users))
	for _, user := range users {
		m.connected[user] = struct{}{}
	}
}

func (m *tPresenceMesh) QueryClientConnected(_ context.Context, users []account.AccountID) ([]account.AccountID, error) {
	m.queries = append(m.queries, users)
	if m.failures > 0 {
		m.failures--
		return nil, errors.New("transient query failure")
	}
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	var connected []account.AccountID
	for _, user := range users {
		if _, found := m.connected[user]; found {
			connected = append(connected, user)
		}
	}
	return connected, nil
}

func (m *tPresenceMesh) ApplyEvent(_ context.Context, event *mesh.Event) (any, error) {
	if m.applyErr != nil {
		return nil, m.applyErr
	}
	m.applied = append(m.applied, event)
	return nil, nil
}

func tAcctID(b byte) account.AccountID {
	var user account.AccountID
	user[0] = b
	return user
}

// tTierSource reports a clean tier-1 reputation and not connected locally,
// unless overridden. errs fakes a reputation load failure; unknown fakes an
// unknown account (nil rep, nil error).
type tTierSource struct {
	reps      map[account.AccountID]*account.Reputation
	connected map[account.AccountID]bool
	errs      map[account.AccountID]error
	unknown   map[account.AccountID]bool
}

func (s *tTierSource) AcctRepStatus(user account.AccountID) (bool, *account.Reputation, error) {
	connected := s.connected[user]
	if err := s.errs[user]; err != nil {
		return connected, nil, err
	}
	if s.unknown[user] {
		return connected, nil, nil
	}
	if rep, found := s.reps[user]; found {
		return connected, rep, nil
	}
	return connected, &account.Reputation{BondedTier: 1}, nil
}

func newTestUnbooker(books map[string]bookedUserSource, timeout time.Duration) (*presenceUnbooker, *tPresenceMesh, *tTierSource) {
	tiers := &tTierSource{
		reps:      make(map[account.AccountID]*account.Reputation),
		connected: make(map[account.AccountID]bool),
		errs:      make(map[account.AccountID]error),
		unknown:   make(map[account.AccountID]bool),
	}
	ub := newPresenceUnbookerForSources(dex.StdOutLogger("TEST", dex.LevelOff), books, tiers, timeout)
	meshSvc := new(tPresenceMesh)
	ub.setMesh(meshSvc)
	return ub, meshSvc, tiers
}

func decodeUserRevoke(t *testing.T, event *mesh.Event) (account.AccountID, meshevents.OrderRevokeReason) {
	t.Helper()
	var payload struct {
		User   dex.Bytes `json:"user"`
		Reason uint8     `json:"reason"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("failed to decode orders_revoked payload: %v", err)
	}
	var user account.AccountID
	copy(user[:], payload.User)
	return user, meshevents.OrderRevokeReason(payload.Reason)
}

func decodeRevokedUser(t *testing.T, event *mesh.Event) account.AccountID {
	t.Helper()
	user, reason := decodeUserRevoke(t, event)
	if reason != meshevents.OrderRevokeReasonDisconnected {
		t.Fatalf("reason = %d, want disconnected", reason)
	}
	return user
}

// TestSweepClassification runs one sweep per reputation/connectivity state
// and checks the outcome. noRevoke is outside the valid OrderRevokeReason range.
func TestSweepClassification(t *testing.T) {
	const noRevoke = meshevents.OrderRevokeReason(0)
	user := tAcctID(1)
	timeout := time.Minute
	tests := []struct {
		name      string
		rep       *account.Reputation // nil: clean tier 1 (unless unknown)
		loadErr   bool                // reputation load failure
		unknown   bool                // unknown account: nil rep, nil error
		localConn bool
		peerConn  bool
		armClock  bool // MIA clock already past the timeout

		wantRevoke  meshevents.OrderRevokeReason // noRevoke: no event
		wantClock   bool
		wantQueries int
	}{{
		// Clean tier-1 user, connected locally: nothing to do.
		name:      "clean, locally connected",
		localConn: true,
	}, {
		// Unknown account (nil rep, no error): unbook for reputation. That
		// also clears an armed MIA clock.
		name:       "unknown account",
		unknown:    true,
		localConn:  true,
		armClock:   true,
		wantRevoke: meshevents.OrderRevokeReasonPenalty,
	}, {
		// Load error and connected: leave alone.
		name:      "load error, locally connected",
		loadErr:   true,
		localConn: true,
	}, {
		// Load error and disconnected: MIA path.
		name:        "load error, absent",
		loadErr:     true,
		wantClock:   true,
		wantQueries: 1,
	}, {
		// Load error with armed clock: peer can rescue like any other user.
		name:        "load error, armed clock, peer rescue",
		loadErr:     true,
		peerConn:    true,
		armClock:    true,
		wantQueries: 1,
	}, {
		// Bond expiry only (tier < 1, no score penalties): keep orders if connected.
		name:      "expired bond, locally connected",
		rep:       &account.Reputation{BondedTier: 0},
		localConn: true,
	}, {
		// Bond expiry only, not local: ordinary MIA candidate; peer can rescue.
		name:        "expired bond, peer connected",
		rep:         &account.Reputation{BondedTier: 0},
		peerConn:    true,
		wantQueries: 1,
	}, {
		// Bond expiry only, absent: arm MIA clock, no revoke yet.
		name:        "expired bond, absent",
		rep:         &account.Reputation{BondedTier: 0},
		wantClock:   true,
		wantQueries: 1,
	}, {
		// Bond expiry only, past timeout: revoke as disconnected, not penalty.
		name:        "expired bond, MIA past timeout",
		rep:         &account.Reputation{BondedTier: 0},
		armClock:    true,
		wantRevoke:  meshevents.OrderRevokeReasonDisconnected,
		wantQueries: 1,
	}, {
		// Bond expiry plus score penalties: unbook for reputation.
		name:       "expired bond with penalties",
		rep:        &account.Reputation{BondedTier: 0, Penalties: 1, Score: -20},
		localConn:  true,
		wantRevoke: meshevents.OrderRevokeReasonPenalty,
	}, {
		// Score penalty with bond still intact: unbook for reputation.
		name:       "penalties with intact bond",
		rep:        &account.Reputation{BondedTier: 1, Penalties: 1, Score: -20},
		localConn:  true,
		wantRevoke: meshevents.OrderRevokeReasonPenalty,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			books := map[string]bookedUserSource{
				"dcr_btc": &tBookSource{users: map[account.AccountID]int{user: 1}},
			}
			ub, meshSvc, tiers := newTestUnbooker(books, timeout)
			if tt.rep != nil {
				tiers.reps[user] = tt.rep
			}
			if tt.loadErr {
				tiers.errs[user] = errors.New("db down")
			}
			tiers.unknown[user] = tt.unknown
			tiers.connected[user] = tt.localConn
			if tt.peerConn {
				meshSvc.setConnected(user)
			}
			now := time.Now()
			if tt.armClock {
				ub.absentSince[user] = now.Add(-2 * timeout)
			}

			ub.sweep(context.Background(), now)

			if tt.wantRevoke == noRevoke {
				if len(meshSvc.applied) != 0 {
					t.Fatalf("applied %d events, want 0", len(meshSvc.applied))
				}
			} else if len(meshSvc.applied) != 1 {
				t.Fatalf("applied %d events, want 1", len(meshSvc.applied))
			} else if gotUser, reason := decodeUserRevoke(t, meshSvc.applied[0]); gotUser != user || reason != tt.wantRevoke {
				t.Fatalf("revoked (%v, reason %d), want (%v, reason %d)", gotUser, reason, user, tt.wantRevoke)
			}
			if _, tracked := ub.absentSince[user]; tracked != tt.wantClock {
				t.Fatalf("MIA clock armed = %v, want %v", tracked, tt.wantClock)
			}
			if len(meshSvc.queries) != tt.wantQueries {
				t.Fatalf("issued %d peer queries, want %d", len(meshSvc.queries), tt.wantQueries)
			}
		})
	}
}

func TestMIASweep(t *testing.T) {
	miaUser, localUser, peerUser := tAcctID(1), tAcctID(2), tAcctID(3)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{
			miaUser:   2,
			localUser: 1,
			peerUser:  1,
		}},
	}
	timeout := time.Minute
	ub, meshSvc, tiers := newTestUnbooker(books, timeout)
	tiers.connected[localUser] = true
	meshSvc.setConnected(peerUser)

	ctx := context.Background()
	now := time.Now()

	// First sweep queries the peer for the two non-local users and arms the
	// absence deadline only for the user connected nowhere.
	ub.sweep(ctx, now)
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users on first sweep, want 0", len(meshSvc.applied))
	}
	if len(meshSvc.queries) != 1 || len(meshSvc.queries[0]) != 2 {
		t.Fatalf("peer queries = %v, want one for the 2 non-local users", meshSvc.queries)
	}
	if _, tracked := ub.absentSince[miaUser]; !tracked {
		t.Fatal("absent user deadline not armed")
	}
	if _, tracked := ub.absentSince[localUser]; tracked {
		t.Fatal("locally connected user deadline armed")
	}
	if _, tracked := ub.absentSince[peerUser]; tracked {
		t.Fatal("peer-connected user deadline armed")
	}

	// Before the timeout, still nothing.
	ub.sweep(ctx, now.Add(timeout/2))
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users before timeout, want 0", len(meshSvc.applied))
	}

	// A reconnect to the peer clears the deadline at the next refresh, which
	// happens before the fire check even when the deadline has expired.
	meshSvc.setConnected(peerUser, miaUser)
	ub.sweep(ctx, now.Add(peerConnectedQueryInterval))
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("deadline not cleared on peer reconnect")
	}
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users despite peer reconnect, want 0", len(meshSvc.applied))
	}

	// Disconnect again: the clock restarts.
	meshSvc.setConnected(peerUser)
	rearm := now.Add(2 * timeout)
	ub.sweep(ctx, rearm)
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users right after re-arm, want 0", len(meshSvc.applied))
	}

	// Past the timeout, the user's orders are revoked with one user-form
	// event, and the deadline is cleared.
	ub.sweep(ctx, rearm.Add(timeout))
	if len(meshSvc.applied) != 1 {
		t.Fatalf("applied %d events, want 1", len(meshSvc.applied))
	}
	if user := decodeRevokedUser(t, meshSvc.applied[0]); user != miaUser {
		t.Fatalf("revoked user %v, want %v", user, miaUser)
	}
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("deadline not cleared after revoke")
	}

	// The connected users were never touched.
	for _, event := range meshSvc.applied {
		if user := decodeRevokedUser(t, event); user == localUser || user == peerUser {
			t.Fatalf("connected user %v was revoked", user)
		}
	}
}

func TestMIASweepQueryError(t *testing.T) {
	miaUser := tAcctID(1)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{miaUser: 1}},
	}
	timeout := 90 * time.Second
	ub, meshSvc, _ := newTestUnbooker(books, timeout)

	ctx := context.Background()
	now := time.Now()

	// The user is connected to the peer, but the query fails: treated as
	// absent, the deadline arms and the failure still counts as a refresh.
	meshSvc.setConnected(miaUser)
	meshSvc.queryErr = errors.New("peer down")
	ub.sweep(ctx, now)
	if _, tracked := ub.absentSince[miaUser]; !tracked {
		t.Fatal("deadline not armed on query error")
	}
	ub.sweep(ctx, now.Add(miaSweepInterval))
	if len(meshSvc.queries) != 1 {
		t.Fatalf("failed query retried within the interval: %d queries", len(meshSvc.queries))
	}

	// Another failed refresh past the interval, still before the timeout.
	ub.sweep(ctx, now.Add(timeout-miaSweepInterval))
	if len(meshSvc.queries) != 2 {
		t.Fatalf("peer queries = %d, want 2", len(meshSvc.queries))
	}

	// With the peer still unreachable past the timeout, the revoke fires. The
	// pre-fire re-query is attempted, also fails, and must not block it.
	ub.sweep(ctx, now.Add(timeout))
	if len(meshSvc.queries) != 3 {
		t.Fatalf("peer queries = %d, want 3 (pre-fire re-query attempted)", len(meshSvc.queries))
	}
	if len(meshSvc.applied) != 1 {
		t.Fatalf("applied %d events with peer down, want 1", len(meshSvc.applied))
	}
	if user := decodeRevokedUser(t, meshSvc.applied[0]); user != miaUser {
		t.Fatalf("revoked user %v, want %v", user, miaUser)
	}
}

func TestMIASweepPreFireRequery(t *testing.T) {
	miaUser := tAcctID(1)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{miaUser: 1}},
	}
	timeout := 10 * time.Minute
	ub, meshSvc, _ := newTestUnbooker(books, timeout)

	ctx := context.Background()
	now := time.Now()

	// Arm the deadline with the user absent everywhere, and refresh the cache
	// shortly before the deadline so it is stale-but-valid at fire time.
	ub.sweep(ctx, now)
	ub.sweep(ctx, now.Add(timeout-miaSweepInterval))

	// The user connects to the peer inside the cache window. The fire-time
	// sweep must re-query the fire set and cancel the revoke.
	meshSvc.setConnected(miaUser)
	queries := len(meshSvc.queries)
	ub.sweep(ctx, now.Add(timeout))
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users despite pre-fire reconnect, want 0", len(meshSvc.applied))
	}
	if len(meshSvc.queries) != queries+1 {
		t.Fatalf("pre-fire sweep issued %d queries, want 1", len(meshSvc.queries)-queries)
	}
	if got := meshSvc.queries[len(meshSvc.queries)-1]; len(got) != 1 || got[0] != miaUser {
		t.Fatalf("pre-fire re-query = %v, want just the fire set", got)
	}
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("deadline not cleared by pre-fire re-query")
	}

	// The rescue is folded into the cached answer: sweeps inside the query
	// window neither re-arm the clock nor issue another query.
	ub.sweep(ctx, now.Add(timeout+miaSweepInterval))
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("clock re-armed off the stale cache after a pre-fire rescue")
	}
	if len(meshSvc.queries) != queries+1 {
		t.Fatalf("cached sweep after pre-fire rescue issued a query")
	}
}

func TestMIASweepPreFireRequeryAfterFailedRefresh(t *testing.T) {
	miaUser := tAcctID(1)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{miaUser: 1}},
	}
	// Harness-like configuration: the MIA timeout equals the query interval,
	// so the fire-time sweep is also the first-refresh sweep.
	timeout := peerConnectedQueryInterval
	ub, meshSvc, _ := newTestUnbooker(books, timeout)

	ctx := context.Background()
	now := time.Now()

	// Promotion-like state: the clock has been armed for a full timeout with
	// no successful peer query since.
	ub.absentSince[miaUser] = now.Add(-timeout)
	ub.lastQuery = now.Add(-peerConnectedQueryInterval)

	// The refresh at fire time fails transiently, but the user is connected
	// to the peer. A failed refresh must not stand in for the pre-fire
	// re-query: the re-query still runs and cancels the revoke.
	meshSvc.setConnected(miaUser)
	meshSvc.failures = 1
	ub.sweep(ctx, now)
	if len(meshSvc.queries) != 2 {
		t.Fatalf("peer queries = %d, want 2 (failed refresh + pre-fire re-query)", len(meshSvc.queries))
	}
	if len(meshSvc.applied) != 0 {
		t.Fatalf("revoked %d users on a single transient query failure, want 0", len(meshSvc.applied))
	}
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("deadline not cleared by the pre-fire re-query")
	}
}

func TestRunMasterSkipsPromotionQuery(t *testing.T) {
	miaUser := tAcctID(1)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{miaUser: 1}},
	}
	ub, meshSvc, _ := newTestUnbooker(books, time.Minute)

	// A canceled context stops runMaster right after the promotion sweep.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var readyErr error
	ready := false
	ub.runMaster(ctx, func(err error) { ready, readyErr = true, err })

	if !ready || readyErr != nil {
		t.Fatalf("readiness = %v, %v; want reported with nil error", ready, readyErr)
	}
	// The promotion sweep never blocks readiness on the peer: no query, but
	// the absence clock is armed for the next sweeps to act on.
	if len(meshSvc.queries) != 0 {
		t.Fatalf("promotion sweep issued %d peer queries, want 0", len(meshSvc.queries))
	}
	if _, tracked := ub.absentSince[miaUser]; !tracked {
		t.Fatal("promotion sweep did not arm the absence clock")
	}
}

func TestMIASweepRetryOnApplyError(t *testing.T) {
	miaUser := tAcctID(1)
	books := map[string]bookedUserSource{
		"dcr_btc": &tBookSource{users: map[account.AccountID]int{miaUser: 1}},
	}
	timeout := time.Minute
	ub, meshSvc, _ := newTestUnbooker(books, timeout)

	ctx := context.Background()
	now := time.Now()
	ub.sweep(ctx, now)

	// Apply fails: the deadline is retained for a retry.
	meshSvc.applyErr = context.DeadlineExceeded
	ub.sweep(ctx, now.Add(2*timeout))
	if _, tracked := ub.absentSince[miaUser]; !tracked {
		t.Fatal("deadline dropped after failed apply")
	}

	// Next sweep succeeds.
	meshSvc.applyErr = nil
	ub.sweep(ctx, now.Add(3*timeout))
	if len(meshSvc.applied) != 1 {
		t.Fatalf("applied %d events after retry, want 1", len(meshSvc.applied))
	}
	if _, tracked := ub.absentSince[miaUser]; tracked {
		t.Fatal("deadline not cleared after successful retry")
	}
}
