//go:build pgonline

package pg

import (
	"context"
	"testing"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

// TestApplyMarketLifecycleEventSuspendPurge exercises the derive-at-apply
// purge: a no-persist suspend revokes exactly the orders the booked table
// holds at the apply point and reports them, sorted, for the in-memory
// projections.
func TestApplyMarketLifecycleEventSuspendPurge(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochDur = int64(EpochDuration)
	finalEpochIdx := int64(30000)
	persist := false
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:          "dcr_btc",
		State:           db.MarketStateRunning,
		StartEpochIdx:   finalEpochIdx - 10,
		StartEpochDur:   epochDur,
		FinalEpochIdx:   finalEpochIdx,
		FinalEpochDur:   epochDur,
		PendingAction:   db.MarketPendingSuspendDrain,
		PendingEpochIdx: finalEpochIdx,
		PendingEpochDur: epochDur,
		PersistBook:     &persist,
		// The suspend event requires the final epoch's close applied.
		ProcessedEpochIdx: finalEpochIdx,
	})

	bookedA := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	bookedB := newLimitOrder(true, 5_100_000, 1, order.StandingTiF, 10)
	for _, lo := range []*order.LimitOrder{bookedA, bookedB} {
		if err := storeOrderForTest(archie, lo, finalEpochIdx-2, epochDur, order.OrderStatusBooked); err != nil {
			t.Fatalf("StoreOrder error: %v", err)
		}
	}

	update := &db.MarketLifecycleUpdate{
		Action:    db.MarketLifecycleActionSuspend,
		Market:    "dcr_btc",
		Base:      AssetDCR,
		Quote:     AssetBTC,
		EpochIdx:  finalEpochIdx,
		EpochDur:  epochDur,
		Timestamp: time.UnixMilli(987654321).UTC(),
	}
	result, err := archie.ApplyMarketLifecycleEvent(ctx, &db.EventLogMeta{Event: []byte("ml-suspend")}, update)
	if err != nil {
		t.Fatalf("ApplyMarketLifecycleEvent error: %v", err)
	}

	// The purge set is the booked table's content, sorted by order ID.
	wantPurge := []order.OrderID{bookedA.ID(), bookedB.ID()}
	sortOrderIDs(wantPurge)
	if len(result.PurgeOrders) != 2 || result.PurgeOrders[0] != wantPurge[0] || result.PurgeOrders[1] != wantPurge[1] {
		t.Fatalf("purge orders = %v, want %v", result.PurgeOrders, wantPurge)
	}
	for _, lo := range []*order.LimitOrder{bookedA, bookedB} {
		if _, status, err := archie.Order(lo.ID(), lo.Base(), lo.Quote()); err != nil || status != order.OrderStatusRevoked {
			t.Fatalf("purged order status = %v (err %v), want %v", status, err, order.OrderStatusRevoked)
		}
	}
	if result.Lifecycle.State != db.MarketStateSuspended || result.Lifecycle.PendingAction != db.MarketPendingNone ||
		result.Lifecycle.PersistBook == nil || *result.Lifecycle.PersistBook {
		t.Fatalf("lifecycle row after suspend = %+v", result.Lifecycle)
	}
}

// TestApplyMarketLifecycleEventResumeSkipsUnbooked exercises the resume revoke
// filter: revokes for orders no longer booked at the apply point are skipped,
// and only the applied subset is reported.
func TestApplyMarketLifecycleEventResumeSkipsUnbooked(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochDur = int64(EpochDuration)
	resumeEpochIdx := int64(40000)
	persist := true
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:          "dcr_btc",
		State:           db.MarketStateSuspended,
		StartEpochIdx:   resumeEpochIdx,
		StartEpochDur:   epochDur,
		PendingAction:   db.MarketPendingResume,
		PendingEpochIdx: resumeEpochIdx,
		PendingEpochDur: epochDur,
		PersistBook:     &persist,
	})

	// stillBooked is revocable; alreadyRevoked was revoked by an intervening
	// event after the master selected it; neverStored is unknown entirely.
	stillBooked := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	alreadyRevoked := newLimitOrder(true, 5_100_000, 1, order.StandingTiF, 10)
	neverStored := newLimitOrder(false, 4_800_000, 1, order.StandingTiF, 20)
	for _, lo := range []*order.LimitOrder{stillBooked, alreadyRevoked} {
		if err := storeOrderForTest(archie, lo, resumeEpochIdx-25, epochDur, order.OrderStatusBooked); err != nil {
			t.Fatalf("StoreOrder error: %v", err)
		}
	}
	if _, _, err := revokeOrderForTest(alreadyRevoked, true); err != nil {
		t.Fatalf("revokeOrderForTest error: %v", err)
	}

	// Timestamp well before the scheduled epoch, so the open epoch is the
	// scheduled one.
	timestamp := time.UnixMilli((resumeEpochIdx - 1) * epochDur).UTC()
	update := &db.MarketLifecycleUpdate{
		Action:    db.MarketLifecycleActionResume,
		Market:    "dcr_btc",
		Base:      AssetDCR,
		Quote:     AssetBTC,
		EpochIdx:  resumeEpochIdx,
		EpochDur:  epochDur,
		Timestamp: timestamp,
		ResumeRevokes: []*db.StartupOrderRevoke{
			{Order: stillBooked, Reason: meshevents.StartupOrderRevokeReasonFundingCoinSpent},
			{Order: alreadyRevoked, Reason: meshevents.StartupOrderRevokeReasonFundingCoinSpent},
			{Order: neverStored, Reason: meshevents.StartupOrderRevokeReasonFundingCoinSpent},
		},
	}
	result, err := archie.ApplyMarketLifecycleEvent(ctx, &db.EventLogMeta{Event: []byte("ml-resume")}, update)
	if err != nil {
		t.Fatalf("ApplyMarketLifecycleEvent error: %v", err)
	}

	// Only the still-booked order's revoke applies.
	if len(result.ResumeRevokes) != 1 || result.ResumeRevokes[0].Order.ID() != stillBooked.ID() {
		t.Fatalf("applied resume revokes = %+v, want only %v", result.ResumeRevokes, stillBooked.ID())
	}
	if _, status, err := archie.Order(stillBooked.ID(), stillBooked.Base(), stillBooked.Quote()); err != nil || status != order.OrderStatusRevoked {
		t.Fatalf("still-booked order status = %v (err %v), want %v", status, err, order.OrderStatusRevoked)
	}
	// The market reopened at the scheduled epoch.
	if result.Lifecycle.State != db.MarketStateRunning || result.Lifecycle.StartEpochIdx != resumeEpochIdx ||
		result.Lifecycle.PendingAction != db.MarketPendingNone || result.Lifecycle.PersistBook != nil {
		t.Fatalf("lifecycle row after resume = %+v", result.Lifecycle)
	}
}
