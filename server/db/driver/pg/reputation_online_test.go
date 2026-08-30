//go:build pgonline

package pg

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

func randomReputationOrderID() (oid order.OrderID) {
	copy(oid[:], encode.RandomBytes(32))
	return
}

func midPtr(mid order.MatchID) *order.MatchID { return &mid }

func randomReputationMatchID() (mid order.MatchID) {
	copy(mid[:], encode.RandomBytes(32))
	return
}

func captureRepListener(t *testing.T) *[][]account.AccountID {
	t.Helper()
	var calls [][]account.AccountID
	archie.repListenerMtx.Lock()
	prev := archie.repListener
	archie.repListener = func(users ...account.AccountID) {
		calls = append(calls, append([]account.AccountID(nil), users...))
	}
	archie.repListenerMtx.Unlock()
	t.Cleanup(func() {
		archie.repListenerMtx.Lock()
		archie.repListener = prev
		archie.repListenerMtx.Unlock()
	})
	return &calls
}

func requireRepListenerCall(t *testing.T, calls [][]account.AccountID, wantCalls int, wantUsers ...account.AccountID) {
	t.Helper()
	if len(calls) != wantCalls {
		t.Fatalf("listener called %d times, want %d: %v", len(calls), wantCalls, calls)
	}
	if wantCalls == 0 {
		return
	}
	last := calls[len(calls)-1]
	if len(last) != len(wantUsers) {
		t.Fatalf("last notification = %v, want users %v", last, wantUsers)
	}
	notified := make(map[account.AccountID]bool, len(last))
	for _, user := range last {
		notified[user] = true
	}
	for _, user := range wantUsers {
		if !notified[user] {
			t.Fatalf("last notification = %v, missing user %v", last, user)
		}
	}
}

func TestApplyRepEventTxListener(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	ctx := context.Background()
	calls := captureRepListener(t)
	userA, userB := randomAccountID(), randomAccountID()
	policy := &db.ReputationOutcomePolicy{PreimageLimit: 5, MatchLimit: 5, OrderLimit: 5}

	// Rolled-back apply: no notify even if the batch was staged.
	applyErr := errors.New("apply failed")
	_, err := archie.applyRepEventTx(ctx, &db.EventLogMeta{Event: []byte("rep-listener-fail")},
		meshevents.EventKindEpochProcessed, []byte("rep-listener-fail-tx"), policy,
		func(tx *sql.Tx, batch *reputationOutcomeBatch) error {
			batch.orders = append(batch.orders, &reputationOrderOutcome{user: userA, oid: randomReputationOrderID()})
			return applyErr
		})
	if !errors.Is(err, applyErr) {
		t.Fatalf("applyRepEventTx error = %v, want %v", err, applyErr)
	}
	requireRepListenerCall(t, *calls, 0)

	_, err = archie.applyRepEventTx(ctx, &db.EventLogMeta{Event: []byte("rep-listener-commit")},
		meshevents.EventKindEpochProcessed, []byte("rep-listener-commit-tx"), policy,
		func(tx *sql.Tx, batch *reputationOutcomeBatch) error {
			batch.preimages = append(batch.preimages, &reputationPreimageOutcome{user: userA, oid: randomReputationOrderID(), miss: true})
			batch.orders = append(batch.orders, &reputationOrderOutcome{user: userA, oid: randomReputationOrderID()})
			batch.matches = append(batch.matches, &reputationMatchOutcome{
				user: userB, mid: db.MarketMatchID{MatchID: randomReputationMatchID()}, outcome: db.OutcomeNoSwapAsTaker,
			})
			return nil
		})
	if err != nil {
		t.Fatalf("applyRepEventTx error: %v", err)
	}
	requireRepListenerCall(t, *calls, 1, userA, userB)

	pimgs, matches, ords, err := archie.GetUserReputationData(ctx, userA, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData error: %v", err)
	}
	if len(pimgs) != 1 || len(ords) != 1 || len(matches) != 0 {
		t.Fatalf("userA outcomes pimgs=%d ords=%d matches=%d, want 1/1/0", len(pimgs), len(ords), len(matches))
	}

	// Empty batch: committed, no notify.
	_, err = archie.applyRepEventTx(ctx, &db.EventLogMeta{Event: []byte("rep-listener-empty")},
		meshevents.EventKindEpochProcessed, []byte("rep-listener-empty-tx"), policy,
		func(tx *sql.Tx, batch *reputationOutcomeBatch) error { return nil })
	if err != nil {
		t.Fatalf("applyRepEventTx empty error: %v", err)
	}
	requireRepListenerCall(t, *calls, 1, userA, userB)
}

func TestReputation(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var user account.AccountID
	copy(user[:], encode.RandomBytes(len(user)))
	keepPreimage := randomReputationOrderID()
	keepMatch := randomReputationMatchID()
	keepOrder := randomReputationOrderID()

	tx, err := archie.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx error: %v", err)
	}

	// The helper inserts all requested outcome rows, then prunes each affected
	// class down to the supplied retention limit before the transaction commits.
	err = archie.insertReputationOutcomeRows(tx,
		&db.ReputationOutcomePolicy{PreimageLimit: 1, MatchLimit: 1, OrderLimit: 1},
		&reputationOutcomeBatch{
			preimages: []*reputationPreimageOutcome{
				{user: user, oid: randomReputationOrderID(), miss: true},
				{user: user, oid: keepPreimage},
			},
			matches: []*reputationMatchOutcome{
				{user: user, mid: db.MarketMatchID{MatchID: randomReputationMatchID()}, outcome: db.OutcomeNoRedeemAsMaker},
				{user: user, mid: db.MarketMatchID{MatchID: keepMatch}, outcome: db.OutcomeSwapSuccess},
			},
			orders: []*reputationOrderOutcome{
				{user: user, oid: randomReputationOrderID(), penalizedCancel: true},
				{user: user, oid: keepOrder},
			},
		})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("insertReputationOutcomeRows error: %v", err)
	}
	if err := commitEventTx(ctx, tx); err != nil {
		t.Fatalf("commitEventTx error: %v", err)
	}

	pimgs, matches, ords, err := archie.GetUserReputationData(ctx, user, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData error: %v", err)
	}
	if len(pimgs) != 1 || pimgs[0].OrderID != keepPreimage || pimgs[0].Miss {
		t.Fatalf("preimage outcomes = %+v, want retained success %v", pimgs, keepPreimage)
	}
	if len(matches) != 1 || matches[0].MatchID != keepMatch || matches[0].MatchOutcome != db.OutcomeSwapSuccess {
		t.Fatalf("match outcomes = %+v, want retained success %v", matches, keepMatch)
	}
	if len(ords) != 1 || ords[0].OrderID != keepOrder || ords[0].Canceled {
		t.Fatalf("order outcomes = %+v, want retained completion %v", ords, keepOrder)
	}

	var rollbackUser account.AccountID
	copy(rollbackUser[:], encode.RandomBytes(len(rollbackUser)))
	tx, err = archie.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx rollback error: %v", err)
	}
	err = archie.insertReputationOutcomeRows(tx,
		&db.ReputationOutcomePolicy{PreimageLimit: 1},
		&reputationOutcomeBatch{
			preimages: []*reputationPreimageOutcome{{
				user: rollbackUser,
				oid:  randomReputationOrderID(),
				miss: true,
			}},
		})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("insertReputationOutcomeRows rollback seed error: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback error: %v", err)
	}
	pimgs, matches, ords, err = archie.GetUserReputationData(ctx, rollbackUser, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData rollback user error: %v", err)
	}
	if len(pimgs)+len(matches)+len(ords) != 0 {
		t.Fatalf("rollback user reputation data pimgs=%+v matches=%+v ords=%+v, want none", pimgs, matches, ords)
	}
}

func seedReputationOutcomesWithPolicy(t *testing.T, ctx context.Context, policy *db.ReputationOutcomePolicy, updates *reputationOutcomeBatch) {
	t.Helper()
	tx, err := archie.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx error: %v", err)
	}
	err = archie.insertReputationOutcomeRows(tx, policy, updates)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("insertReputationOutcomeRows error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit reputation seed error: %v", err)
	}
}

func seedReputationOutcomes(t *testing.T, ctx context.Context, updates *reputationOutcomeBatch) {
	t.Helper()
	seedReputationOutcomesWithPolicy(t, ctx,
		&db.ReputationOutcomePolicy{PreimageLimit: 20, MatchLimit: 20, OrderLimit: 20},
		updates)
}

func TestApplyReputationForgivenEventUserScope(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	ctx := context.Background()
	user := randomAccountID()
	keepPreimage := randomReputationOrderID()
	keepMatch := randomReputationMatchID()
	keepOrder := randomReputationOrderID()
	seedReputationOutcomes(t, ctx, &reputationOutcomeBatch{
		preimages: []*reputationPreimageOutcome{
			{user: user, oid: randomReputationOrderID(), miss: true},
			{user: user, oid: keepPreimage},
		},
		matches: []*reputationMatchOutcome{
			{user: user, mid: db.MarketMatchID{MatchID: randomReputationMatchID()}, outcome: db.OutcomeNoRedeemAsMaker},
			{user: user, mid: db.MarketMatchID{MatchID: keepMatch}, outcome: db.OutcomeSwapSuccess},
		},
		orders: []*reputationOrderOutcome{
			{user: user, oid: randomReputationOrderID(), penalizedCancel: true},
			{user: user, oid: keepOrder},
		},
	})

	calls := captureRepListener(t)
	update := &meshevents.ReputationForgivenEvent{
		AccountID: user,
		Scope:     meshevents.ReputationForgivenessScopeUser,
	}
	event := []byte("user-reputation-forgiven")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindReputationForgiven, event, update)
	result, err := archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent error: %v", err)
	}
	if result == nil || !result.Forgiven {
		t.Fatalf("result = %+v, want forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 1, meshevents.EventKindReputationForgiven, event, tip, update)
	requireRepListenerCall(t, *calls, 1, user)

	pimgs, matches, ords, err := archie.GetUserReputationData(ctx, user, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData error: %v", err)
	}
	if len(pimgs) != 1 || pimgs[0].OrderID != keepPreimage || pimgs[0].Miss {
		t.Fatalf("preimage outcomes = %+v, want retained success %v", pimgs, keepPreimage)
	}
	if len(matches) != 1 || matches[0].MatchID != keepMatch || matches[0].MatchOutcome != db.OutcomeSwapSuccess {
		t.Fatalf("match outcomes = %+v, want retained success %v", matches, keepMatch)
	}
	if len(ords) != 1 || ords[0].OrderID != keepOrder || ords[0].Canceled {
		t.Fatalf("order outcomes = %+v, want retained completion %v", ords, keepOrder)
	}

	noopEvent := []byte("user-reputation-forgiven-noop")
	noopTip := testEventApplyTip(t, tip, 2, meshevents.EventKindReputationForgiven, noopEvent, update)
	result, err = archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           noopEvent,
		ExpectedTipHash: noopTip,
	}, update)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent noop error: %v", err)
	}
	if result == nil || result.Forgiven {
		t.Fatalf("noop result = %+v, want not forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 2, meshevents.EventKindReputationForgiven, noopEvent, noopTip, update)
}

func TestApplyReputationForgivenEventMatchScope(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	ctx := context.Background()
	users := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
	f := newMatchFailedFixture()
	matchForgiven := func(t *testing.T, mid db.MarketMatchID) bool {
		t.Helper()
		marketSchema, err := archie.marketSchema(mid.Base, mid.Quote)
		if err != nil {
			t.Fatalf("marketSchema error: %v", err)
		}
		var forgiven bool
		err = archie.db.QueryRowContext(ctx,
			"SELECT COALESCE(forgiven, FALSE) FROM "+fullMatchesTableName(archie.dbName, marketSchema)+" WHERE matchid = $1",
			mid.MatchID).Scan(&forgiven)
		if err != nil {
			t.Fatalf("query forgiven status error: %v", err)
		}
		return forgiven
	}
	pair, _ := f.newMatchFailedUpdate(t, order.NewlyMatched, db.MatchFailureMakerNoSwap, users,
		order.OrderStatusExecuted, order.OrderStatusExecuted)
	mid := testMarketMatchID(pair.match)
	if err := archie.setMatchInactive(archie.db, mid, false); err != nil {
		t.Fatalf("setMatchInactive error: %v", err)
	}
	seedReputationOutcomes(t, ctx, &reputationOutcomeBatch{
		matches: []*reputationMatchOutcome{{
			user:    users.maker,
			mid:     mid,
			outcome: db.OutcomeNoSwapAsMaker,
		}},
	})

	// Forgiving the match marks the match row forgiven and deletes the
	// account's failure points, so the conduct score recovers.
	update := &meshevents.ReputationForgivenEvent{
		AccountID: users.maker,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   midPtr(mid.MatchID),
	}
	event := []byte("match-reputation-forgiven")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindReputationForgiven, event, update)
	result, err := archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent error: %v", err)
	}
	if result == nil || !result.Forgiven {
		t.Fatalf("result = %+v, want forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 1, meshevents.EventKindReputationForgiven, event, tip, update)
	if !matchForgiven(t, mid) {
		t.Fatalf("match not marked forgiven")
	}
	f.requireNoMatchOutcomes(t, users.maker)
	fails, err := archie.UserMatchFails(users.maker, 10)
	if err != nil {
		t.Fatalf("UserMatchFails error: %v", err)
	}
	if len(fails) != 0 {
		t.Fatalf("UserMatchFails = %+v, want none", fails)
	}

	// Re-forgiving the same inactive match updates the match row again, as
	// with the legacy ForgiveMatchFail.
	repeatEvent := []byte("match-reputation-forgiven-repeat")
	repeatTip := testEventApplyTip(t, tip, 2, meshevents.EventKindReputationForgiven, repeatEvent, update)
	result, err = archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           repeatEvent,
		ExpectedTipHash: repeatTip,
	}, update)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent repeat error: %v", err)
	}
	if result == nil || !result.Forgiven {
		t.Fatalf("repeat result = %+v, want forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 2, meshevents.EventKindReputationForgiven, repeatEvent, repeatTip, update)

	// A match id not present in any market forgives nothing, but the event
	// still applies and is logged.
	unknownUpdate := &meshevents.ReputationForgivenEvent{
		AccountID: users.maker,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   midPtr(randomReputationMatchID()),
	}
	unknownEvent := []byte("unknown-match-reputation-forgiven")
	unknownTip := testEventApplyTip(t, repeatTip, 3, meshevents.EventKindReputationForgiven, unknownEvent, unknownUpdate)
	result, err = archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{
		Seq:             3,
		Event:           unknownEvent,
		ExpectedTipHash: unknownTip,
	}, unknownUpdate)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent unknown match error: %v", err)
	}
	if result == nil || result.Forgiven {
		t.Fatalf("unknown match result = %+v, want not forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 3, meshevents.EventKindReputationForgiven, unknownEvent, unknownTip, unknownUpdate)

	// An active match cannot be forgiven, and its failure points are left
	// untouched.
	activeUsers := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
	activePair := generateMatchWithOrderStatuses(t, order.NewlyMatched, true, activeUsers.maker, activeUsers.taker,
		order.OrderStatusExecuted, order.OrderStatusExecuted)
	activeMID := testMarketMatchID(activePair.match)
	seedReputationOutcomes(t, ctx, &reputationOutcomeBatch{
		matches: []*reputationMatchOutcome{{
			user:    activeUsers.maker,
			mid:     activeMID,
			outcome: db.OutcomeNoSwapAsMaker,
		}},
	})
	activeUpdate := &meshevents.ReputationForgivenEvent{
		AccountID: activeUsers.maker,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   midPtr(activeMID.MatchID),
	}
	activeEvent := []byte("active-match-reputation-forgiven")
	activeTip := testEventApplyTip(t, unknownTip, 4, meshevents.EventKindReputationForgiven, activeEvent, activeUpdate)
	result, err = archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{
		Seq:             4,
		Event:           activeEvent,
		ExpectedTipHash: activeTip,
	}, activeUpdate)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent active match error: %v", err)
	}
	if result == nil || result.Forgiven {
		t.Fatalf("active match result = %+v, want not forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 4, meshevents.EventKindReputationForgiven, activeEvent, activeTip, activeUpdate)
	if matchForgiven(t, activeMID) {
		t.Fatalf("active match marked forgiven")
	}
	f.requireMatchOutcome(t, activeUsers.maker, db.OutcomeNoSwapAsMaker)

	// An inactive match with no failure points is still marked forgiven, as
	// with the legacy ForgiveMatchFail, without creating any points rows.
	noPointsUsers := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
	noPointsPair := generateMatchWithOrderStatuses(t, order.NewlyMatched, true, noPointsUsers.maker, noPointsUsers.taker,
		order.OrderStatusExecuted, order.OrderStatusExecuted)
	noPointsMID := testMarketMatchID(noPointsPair.match)
	if err := archie.setMatchInactive(archie.db, noPointsMID, false); err != nil {
		t.Fatalf("setMatchInactive no-points error: %v", err)
	}
	noPointsUpdate := &meshevents.ReputationForgivenEvent{
		AccountID: noPointsUsers.maker,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   midPtr(noPointsMID.MatchID),
	}
	noPointsEvent := []byte("no-points-match-reputation-forgiven")
	noPointsTip := testEventApplyTip(t, activeTip, 5, meshevents.EventKindReputationForgiven, noPointsEvent, noPointsUpdate)
	result, err = archie.ApplyReputationForgivenEvent(ctx, &db.EventLogMeta{
		Seq:             5,
		Event:           noPointsEvent,
		ExpectedTipHash: noPointsTip,
	}, noPointsUpdate)
	if err != nil {
		t.Fatalf("ApplyReputationForgivenEvent no-points error: %v", err)
	}
	if result == nil || !result.Forgiven {
		t.Fatalf("no-points result = %+v, want forgiven", result)
	}
	requireEventApplyLog(t, result.Log, 5, meshevents.EventKindReputationForgiven, noPointsEvent, noPointsTip, noPointsUpdate)
	if !matchForgiven(t, noPointsMID) {
		t.Fatalf("no-points match not marked forgiven")
	}
	f.requireNoMatchOutcomes(t, noPointsUsers.maker)

	frontier, err := archie.EventLogFrontier(ctx)
	if err != nil {
		t.Fatalf("EventLogFrontier error: %v", err)
	}
	if frontier.Seq != 5 || !bytes.Equal(frontier.TipHash, noPointsTip) {
		t.Fatalf("frontier = %+v, want seq 5 tip %x", frontier, noPointsTip)
	}
}

func TestValidateReputationOutcomeUpdates(t *testing.T) {
	var user account.AccountID
	copy(user[:], encode.RandomBytes(len(user)))
	oid := randomReputationOrderID()
	mid := randomReputationMatchID()
	policy := &db.ReputationOutcomePolicy{PreimageLimit: 1, MatchLimit: 1, OrderLimit: 1}

	tests := []struct {
		name    string
		policy  *db.ReputationOutcomePolicy
		updates *reputationOutcomeBatch
		wantErr bool
	}{
		{
			name:    "nil updates",
			updates: nil,
			wantErr: true,
		},
		{
			name:    "empty updates are no-op",
			updates: &reputationOutcomeBatch{},
		},
		{
			name:   "nil preimage update",
			policy: policy,
			updates: &reputationOutcomeBatch{
				preimages: []*reputationPreimageOutcome{nil},
			},
			wantErr: true,
		},
		{
			name:   "zero preimage user",
			policy: policy,
			updates: &reputationOutcomeBatch{
				preimages: []*reputationPreimageOutcome{{oid: oid}},
			},
			wantErr: true,
		},
		{
			name:   "zero preimage order id",
			policy: policy,
			updates: &reputationOutcomeBatch{
				preimages: []*reputationPreimageOutcome{{user: user}},
			},
			wantErr: true,
		},
		{
			name:   "nil match update",
			policy: policy,
			updates: &reputationOutcomeBatch{
				matches: []*reputationMatchOutcome{nil},
			},
			wantErr: true,
		},
		{
			name:   "zero match user",
			policy: policy,
			updates: &reputationOutcomeBatch{
				matches: []*reputationMatchOutcome{{mid: db.MarketMatchID{MatchID: mid}, outcome: db.OutcomeSwapSuccess}},
			},
			wantErr: true,
		},
		{
			name:   "zero match id",
			policy: policy,
			updates: &reputationOutcomeBatch{
				matches: []*reputationMatchOutcome{{user: user, outcome: db.OutcomeSwapSuccess}},
			},
			wantErr: true,
		},
		{
			name:   "invalid match outcome",
			policy: policy,
			updates: &reputationOutcomeBatch{
				matches: []*reputationMatchOutcome{{user: user, mid: db.MarketMatchID{MatchID: mid}, outcome: db.OutcomeOrderCanceled}},
			},
			wantErr: true,
		},
		{
			name:   "nil order update",
			policy: policy,
			updates: &reputationOutcomeBatch{
				orders: []*reputationOrderOutcome{nil},
			},
			wantErr: true,
		},
		{
			name:   "zero order user",
			policy: policy,
			updates: &reputationOutcomeBatch{
				orders: []*reputationOrderOutcome{{oid: oid}},
			},
			wantErr: true,
		},
		{
			name:   "zero order id",
			policy: policy,
			updates: &reputationOutcomeBatch{
				orders: []*reputationOrderOutcome{{user: user}},
			},
			wantErr: true,
		},
		{
			name:   "missing limit for affected class",
			policy: &db.ReputationOutcomePolicy{MatchLimit: 1, OrderLimit: 1},
			updates: &reputationOutcomeBatch{
				preimages: []*reputationPreimageOutcome{{user: user, oid: oid}},
			},
			wantErr: true,
		},
		{
			name:   "valid updates",
			policy: policy,
			updates: &reputationOutcomeBatch{
				preimages: []*reputationPreimageOutcome{{user: user, oid: oid}},
				matches:   []*reputationMatchOutcome{{user: user, mid: db.MarketMatchID{MatchID: mid}, outcome: db.OutcomeSwapSuccess}},
				orders:    []*reputationOrderOutcome{{user: user, oid: oid}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateReputationOutcomeUpdates(tt.policy, tt.updates)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateReputationOutcomeUpdates error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
