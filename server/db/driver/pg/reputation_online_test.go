//go:build pgonline

package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
)

func TestReputation(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	acct := tNewAccount(t)
	user := acct.ID

	if err := archie.CreateAccountWithBond(acct, &db.Bond{CoinID: []byte{1}}); err != nil {
		t.Fatalf("Error creating account: %v", err)
	}

	// Set reputation version to zero
	query := fmt.Sprintf(internal.UpdateReputationVersion, archie.tables.accounts)
	if _, err := archie.db.ExecContext(ctx, query, 0, user); err != nil {
		t.Fatalf("Error zeroing reputation version: %v", err)
	}

	if ver, err := archie.GetUserReputationVersion(ctx, user); err != nil {
		t.Fatalf("Error retrieving zeroed reputation version: %v", err)
	} else if ver != 0 {
		t.Fatalf("Reputation version not updated. Expected 0, got %d", ver)
	}

	randomOrderID := func() (oid order.OrderID) {
		copy(oid[:], encode.RandomBytes(32))
		return
	}

	pimgs := []*db.PreimageOutcome{
		{OrderID: randomOrderID(), Miss: true},
		{OrderID: randomOrderID(), Miss: false},
		{OrderID: randomOrderID(), Miss: true},
		{OrderID: randomOrderID(), Miss: false},
	}

	randomMatchID := func() (mid order.MatchID) {
		copy(mid[:], encode.RandomBytes(32))
		return
	}

	matches := []*db.MatchResult{
		{MatchID: randomMatchID(), MatchOutcome: db.OutcomeNoRedeemAsMaker},
		{MatchID: randomMatchID(), MatchOutcome: db.OutcomeNoRedeemAsTaker},
		{MatchID: randomMatchID(), MatchOutcome: db.OutcomeNoSwapAsMaker},
		{MatchID: randomMatchID(), MatchOutcome: db.OutcomeSwapSuccess},
	}

	ords := []*db.OrderOutcome{
		{OrderID: randomOrderID(), Canceled: true},
		{OrderID: randomOrderID(), Canceled: false},
		{OrderID: randomOrderID(), Canceled: true},
		{OrderID: randomOrderID(), Canceled: false},
	}

	_, _, _, err := archie.UpgradeUserReputationV1(ctx, user, pimgs, matches, ords)
	if err != nil {
		t.Fatalf("Error upgrading user: %v", err)
	}

	if ver, err := archie.GetUserReputationVersion(ctx, user); err != nil {
		t.Fatalf("Error retrieving updated reputation version: %v", err)
	} else if ver != 1 {
		t.Fatalf("Reputation version not updated. Expected 1, got %d", ver)
	}

	loadedPimgs, loadedMatches, loadedOrds, err := archie.GetUserReputationData(ctx, user, 100, 100, 100)
	if err != nil {
		t.Fatalf("Error loading reputation data: %v", err)
	}

	if len(loadedPimgs) != len(pimgs) {
		t.Fatalf("Wrong number of preimage outcomes loaded. Expected %d, got %d", len(pimgs), len(loadedPimgs))
	}
	for i, pimg := range pimgs {
		loadedPimg := loadedPimgs[i]
		if pimg.DBID == 0 || loadedPimg.DBID != pimg.DBID {
			t.Fatalf("Incorrect DB ID upgraded preimage outcome %d %d", pimg.DBID, loadedPimg.DBID)
		}
		if pimg.OrderID != loadedPimg.OrderID {
			t.Fatalf("Wrong order ID for loaded preimage outcome %d", i)
		}
		if pimg.Miss != loadedPimg.Miss {
			t.Fatalf("Wrong miss value for loaded preimage outcome")
		}
	}

	if len(loadedMatches) != len(matches) {
		t.Fatalf("Wrong number of match outcomes loaded. Expected %d, got %d", len(matches), len(loadedMatches))
	}
	for i, match := range matches {
		loadedMatch := loadedMatches[i]
		if match.DBID == 0 || loadedMatch.DBID != match.DBID {
			t.Fatalf("Incorrect DB ID upgraded match outcome %d %d", match.DBID, loadedMatch.DBID)
		}
		if match.MatchID != loadedMatch.MatchID {
			t.Fatalf("Wrong match ID for loaded match outcome %d", i)
		}
		if match.MatchOutcome != loadedMatch.MatchOutcome {
			t.Fatalf("Wrong outcome value for loaded match outcome")
		}
	}

	if len(loadedOrds) != len(ords) {
		t.Fatalf("Wrong number of order outcomes loaded. Expected %d, got %d", len(ords), len(loadedOrds))
	}

	for i, ord := range ords {
		loadedOrd := loadedOrds[i]
		if ord.DBID == 0 || loadedOrd.DBID != ord.DBID {
			t.Fatalf("Incorrect DB ID upgraded order outcome %d %d", ord.DBID, loadedOrd.DBID)
		}
		if ord.OrderID != loadedOrd.OrderID {
			t.Fatalf("Wrong order ID for loaded order outcome %d", i)
		}
		if ord.Canceled != loadedOrd.Canceled {
			t.Fatalf("Wrong canceled value for loaded order outcome")
		}
	}

	if err := archie.PruneOutcomes(ctx, user, db.OutcomeClassPreimage, pimgs[2].DBID); err != nil {
		t.Fatalf("Error pruning preimage outcomes: %v", err)
	}
	if err := archie.PruneOutcomes(ctx, user, db.OutcomeClassMatch, matches[2].DBID); err != nil {
		t.Fatalf("Error pruning match outcomes: %v", err)
	}
	if err := archie.PruneOutcomes(ctx, user, db.OutcomeClassOrder, ords[2].DBID); err != nil {
		t.Fatalf("Error pruning order outcomes: %v", err)
	}

	loadedPimgs, loadedMatches, loadedOrds, _ = archie.GetUserReputationData(ctx, user, 100, 100, 100)
	if len(loadedPimgs) != 1 || loadedPimgs[0].DBID != pimgs[3].DBID {
		t.Fatal("Pruning preimages failed")
	}
	if len(loadedMatches) != 1 || loadedMatches[0].DBID != matches[3].DBID {
		t.Fatal("Pruning matches failed")
	}
	if len(loadedOrds) != 1 || loadedOrds[0].DBID != ords[3].DBID {
		t.Fatal("Pruning orders failed")
	}

	// Only one success left for each class.
	// Add a fail for each class.

	oid := randomOrderID()
	if pimg, err := archie.AddPreimageOutcome(ctx, user, oid, true); err != nil {
		t.Fatalf("Error adding preimage outcome: %v", err)
	} else if pimg.OrderID != oid || !pimg.Miss || pimg.DBID == 0 {
		t.Fatalf("Bad added preimage outcome return")
	}
	mid := randomMatchID()
	outcome := db.OutcomeNoRedeemAsMaker
	if match, err := archie.AddMatchOutcome(ctx, user, mid, outcome); err != nil {
		t.Fatalf("Error adding match outcome: %v", err)
	} else if match.MatchID != mid || match.MatchOutcome != outcome || match.DBID == 0 {
		t.Fatalf("Bad added match outcome return")
	}
	if ord, err := archie.AddOrderOutcome(ctx, user, oid, true); err != nil {
		t.Fatalf("Error adding order outcome: %v", err)
	} else if ord.OrderID != oid || !ord.Canceled || ord.DBID == 0 {
		t.Fatalf("Bad added order outcome return")
	}

	loadedPimgs, loadedMatches, loadedOrds, _ = archie.GetUserReputationData(ctx, user, 100, 100, 100)
	if len(loadedPimgs) != 2 || len(loadedMatches) != 2 || len(loadedOrds) != 2 {
		t.Fatal("Wrong number of loaded outcomes", len(loadedPimgs), len(loadedMatches), len(loadedOrds))
	}

	if err := archie.ForgiveUser(ctx, user); err != nil {
		t.Fatalf("Error forgiving user: %v", err)
	}

	loadedPimgs, loadedMatches, loadedOrds, _ = archie.GetUserReputationData(ctx, user, 100, 100, 100)
	if len(loadedPimgs) != 1 || len(loadedMatches) != 1 || len(loadedOrds) != 1 {
		t.Fatal("Wrong number of loaded outcomes after forgiveness", len(loadedPimgs), len(loadedMatches), len(loadedOrds))
	}
	if loadedPimgs[0].Miss || loadedMatches[0].MatchOutcome != db.OutcomeSwapSuccess || loadedOrds[0].Canceled {
		t.Fatal("Forgiving didn't forgive", loadedPimgs[0].Miss, loadedMatches[0].MatchOutcome, loadedOrds[0].Canceled)
	}
}

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
