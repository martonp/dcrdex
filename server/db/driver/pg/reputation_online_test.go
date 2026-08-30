//go:build pgonline

package pg

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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

func seedReputationOutcome(t *testing.T, ctx context.Context, user account.AccountID, link [32]byte, class db.OutcomeClass, outcome db.Outcome) {
	t.Helper()
	stmt := "INSERT INTO " + archie.tables.points + " (account, link, class, outcome) VALUES ($1, $2, $3, $4)"
	if _, err := archie.db.ExecContext(ctx, stmt, user, order.OrderID(link), class, outcome); err != nil {
		t.Fatalf("insert reputation outcome: %v", err)
	}
}

func TestApplyReputationForgivenEvent(t *testing.T) {
	for _, tt := range []struct {
		name         string
		scope        meshevents.ReputationForgivenessScope
		active       bool
		missing      bool
		wrongTip     bool
		wantForgiven []bool // One result per application, including repeats.
	}{
		{name: "user", scope: meshevents.ReputationForgivenessScopeUser, wantForgiven: []bool{true, false}},
		{name: "match", scope: meshevents.ReputationForgivenessScopeMatch, wantForgiven: []bool{true, true}},
		{name: "active match", scope: meshevents.ReputationForgivenessScopeMatch, active: true, wantForgiven: []bool{false}},
		{name: "unknown match", scope: meshevents.ReputationForgivenessScopeMatch, missing: true, wantForgiven: []bool{false}},
		{name: "user rollback", scope: meshevents.ReputationForgivenessScopeUser, wrongTip: true, wantForgiven: []bool{false}},
		{name: "match rollback", scope: meshevents.ReputationForgivenessScopeMatch, wrongTip: true, wantForgiven: []bool{false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := cleanTables(archie.db); err != nil {
				t.Fatalf("cleanTables: %v", err)
			}
			ctx := context.Background()
			user, otherUser := randomAccountID(), randomAccountID()
			mid := db.MarketMatchID{MatchID: randomReputationMatchID()}
			if !tt.missing {
				pair := generateMatch(t, order.NewlyMatched, tt.active, user, otherUser)
				mid = testMarketMatchID(pair.match)
			}

			// Seed a penalty and success in each class, plus another failed match
			// that match-scoped forgiveness must leave alone.
			seedReputationOutcome(t, ctx, user, randomReputationOrderID(), db.OutcomeClassPreimage, db.OutcomePreimageMiss)
			seedReputationOutcome(t, ctx, user, randomReputationOrderID(), db.OutcomeClassPreimage, db.OutcomePreimageSuccess)
			seedReputationOutcome(t, ctx, user, mid.MatchID, db.OutcomeClassMatch, db.OutcomeNoSwapAsMaker)
			seedReputationOutcome(t, ctx, user, randomReputationMatchID(), db.OutcomeClassMatch, db.OutcomeSwapSuccess)
			seedReputationOutcome(t, ctx, user, randomReputationMatchID(), db.OutcomeClassMatch, db.OutcomeNoRedeemAsMaker)
			seedReputationOutcome(t, ctx, user, randomReputationOrderID(), db.OutcomeClassOrder, db.OutcomeOrderCanceled)
			seedReputationOutcome(t, ctx, user, randomReputationOrderID(), db.OutcomeClassOrder, db.OutcomeOrderComplete)
			beforePreimages, beforeMatches, beforeOrders, err := archie.GetUserReputationData(ctx, user, 10, 10, 10)
			if err != nil {
				t.Fatalf("GetUserReputationData: %v", err)
			}
			if len(beforePreimages) != 2 || len(beforeMatches) != 3 || len(beforeOrders) != 2 {
				t.Fatal("unexpected seeded reputation outcomes")
			}

			// The other account shares these links, so deleting by link alone
			// would incorrectly remove its penalties too.
			seedReputationOutcome(t, ctx, otherUser, beforePreimages[0].OrderID, db.OutcomeClassPreimage, db.OutcomePreimageMiss)
			seedReputationOutcome(t, ctx, otherUser, mid.MatchID, db.OutcomeClassMatch, db.OutcomeNoSwapAsTaker)
			seedReputationOutcome(t, ctx, otherUser, beforeOrders[0].OrderID, db.OutcomeClassOrder, db.OutcomeOrderCanceled)
			otherPreimages, otherMatches, otherOrders, err := archie.GetUserReputationData(ctx, otherUser, 10, 10, 10)
			if err != nil {
				t.Fatalf("GetUserReputationData for other account: %v", err)
			}

			calls := captureRepListener(t)
			update := &meshevents.ReputationForgivenEvent{AccountID: user, Scope: tt.scope}
			if tt.scope == meshevents.ReputationForgivenessScopeMatch {
				update.MatchID = midPtr(mid.MatchID)
			}
			wantPreimages, wantMatches, wantOrders := beforePreimages, beforeMatches, beforeOrders
			var tip []byte
			for i, wantForgiven := range tt.wantForgiven {
				seq := uint64(i + 1)
				event := []byte(tt.name)
				wantTip := testEventApplyTip(t, tip, seq, meshevents.EventKindReputationForgiven, event, update)
				meta := &db.EventLogMeta{Seq: seq, Event: event, ExpectedTipHash: wantTip}
				if tt.wrongTip {
					meta.ExpectedTipHash = wrongEventTip()
				}
				result, err := archie.ApplyReputationForgivenEvent(ctx, meta, update)
				if tt.wrongTip {
					var divergence *db.EventLogDivergenceError
					if !errors.As(err, &divergence) || result != nil {
						t.Fatalf("result = %+v, error = %v, want nil result and divergence", result, err)
					}
					assertEventLogFrontier(t, ctx, 0, nil)
					requireRepListenerCall(t, *calls, 0)
				} else {
					if err != nil {
						t.Fatalf("application %d: %v", seq, err)
					}
					if result == nil || result.Forgiven != wantForgiven {
						t.Fatalf("application %d: result = %+v, want forgiven %v", seq, result, wantForgiven)
					}
					requireEventApplyLog(t, result.Log, seq, meshevents.EventKindReputationForgiven, event, wantTip, update)
					assertEventLogFrontier(t, ctx, seq, wantTip)
					requireRepListenerCall(t, *calls, i+1, user)
					tip = wantTip

					if wantForgiven {
						if tt.scope == meshevents.ReputationForgivenessScopeUser {
							// User forgiveness keeps only the success in each class.
							wantPreimages = beforePreimages[1:]
							wantMatches = beforeMatches[1:2]
							wantOrders = beforeOrders[1:]
						} else {
							// Match forgiveness removes only the targeted failure.
							wantMatches = beforeMatches[1:]
						}
					}
				}

				preimages, matches, orders, err := archie.GetUserReputationData(ctx, user, 10, 10, 10)
				if err != nil {
					t.Fatalf("GetUserReputationData after application %d: %v", seq, err)
				}
				if !reflect.DeepEqual(preimages, wantPreimages) || !reflect.DeepEqual(matches, wantMatches) || !reflect.DeepEqual(orders, wantOrders) {
					t.Fatalf("unexpected reputation outcomes after application %d", seq)
				}
				preimages, matches, orders, err = archie.GetUserReputationData(ctx, otherUser, 10, 10, 10)
				if err != nil {
					t.Fatalf("GetUserReputationData for other account: %v", err)
				}
				if !reflect.DeepEqual(preimages, otherPreimages) || !reflect.DeepEqual(matches, otherMatches) || !reflect.DeepEqual(orders, otherOrders) {
					t.Fatalf("other account's reputation changed after application %d", seq)
				}
				if !tt.missing {
					matchForgiven := tt.scope == meshevents.ReputationForgivenessScopeMatch && !tt.wrongTip && wantForgiven
					requireMatchForgiven(t, ctx, mid, matchForgiven)
					if matchForgiven {
						fails, err := archie.UserMatchFails(user, 10)
						if err != nil {
							t.Fatalf("UserMatchFails: %v", err)
						}
						if len(fails) != 0 {
							t.Fatalf("UserMatchFails = %+v, want none", fails)
						}
					}
				}
			}
		})
	}
}

func requireMatchForgiven(t *testing.T, ctx context.Context, mid db.MarketMatchID, want bool) {
	t.Helper()
	schema, err := archie.marketSchema(mid.Base, mid.Quote)
	if err != nil {
		t.Fatalf("marketSchema: %v", err)
	}
	var forgiven bool
	stmt := "SELECT COALESCE(forgiven, FALSE) FROM " + fullMatchesTableName(archie.dbName, schema) + " WHERE matchid = $1"
	if err := archie.db.QueryRowContext(ctx, stmt, mid.MatchID).Scan(&forgiven); err != nil {
		t.Fatalf("query forgiven status: %v", err)
	}
	if forgiven != want {
		t.Fatalf("match forgiven = %v, want %v", forgiven, want)
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
		"test_reputation", []byte("rep-listener-fail-tx"), policy,
		func(tx *sql.Tx, batch *reputationOutcomeBatch) error {
			batch.orders = append(batch.orders, &reputationOrderOutcome{user: userA, oid: randomReputationOrderID()})
			return applyErr
		})
	if !errors.Is(err, applyErr) {
		t.Fatalf("applyRepEventTx error = %v, want %v", err, applyErr)
	}
	requireRepListenerCall(t, *calls, 0)

	_, err = archie.applyRepEventTx(ctx, &db.EventLogMeta{Event: []byte("rep-listener-commit")},
		"test_reputation", []byte("rep-listener-commit-tx"), policy,
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
		"test_reputation", []byte("rep-listener-empty-tx"), policy,
		func(tx *sql.Tx, batch *reputationOutcomeBatch) error { return nil })
	if err != nil {
		t.Fatalf("applyRepEventTx empty error: %v", err)
	}
	requireRepListenerCall(t, *calls, 1, userA, userB)
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
