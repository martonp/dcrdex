//go:build pgonline

package pg

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
	"github.com/davecgh/go-spew/spew"
)

const cancelThreshWindow = 100 // spec

// revokeOrderForTest revokes ord with a generated (pseudo) cancel order via
// the shared revokeOrder helper, the same path the orders_revoked and
// epoch_processed event appliers take.
func revokeOrderForTest(ord order.Order, exempt bool) (order.OrderID, time.Time, error) {
	timeStamp := time.Now().Truncate(time.Millisecond).UTC()
	cancelID, err := archie.revokeOrder(archie.db, ord, exempt, timeStamp)
	return cancelID, timeStamp, err
}

func seedMarketLifecycle(t *testing.T, lc *db.MarketLifecycle) {
	t.Helper()
	if lc.RunParams.LotSize == 0 {
		// Seed a valid set for rows that predate run params.
		lc.RunParams = meshevents.MarketRunParams{
			LotSize:                LotSize,
			RateStep:               RateStep,
			ParcelSize:             10,
			MaxUserCancelsPerEpoch: math.MaxUint32, // historical unlimited-cancels default
		}
	}
	tx, err := archie.db.Begin()
	if err != nil {
		t.Fatalf("Begin error: %v", err)
	}
	defer tx.Rollback()
	if err := archie.upsertMarketLifecycleTx(tx, lc); err != nil {
		t.Fatalf("upsertMarketLifecycleTx error: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit error: %v", err)
	}
}

// storeOrderForTest seeds an order directly into persistent storage for the
// specified epoch ID (idx:dur) with the provided status, bypassing the event
// appliers. It is test-only: in production all durable order writes flow
// exclusively through the event appliers. The order is validated via
// validateOrder so only sensible orders reach storage.
func storeOrderForTest(a *Archiver, ord order.Order, epochIdx, epochDur int64, status order.OrderStatus) error {
	return a.storeOrder(a.db, ord, epochIdx, epochDur, db.EpochGapNA, marketToPgStatus(status))
}

func TestStoreOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	orderBadLotSize := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0)
	orderBadLotSize.Quantity /= 2

	orderBadMarket := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0)
	orderBadMarket.BaseAsset = AssetDCR
	orderBadMarket.QuoteAsset = AssetDCR // same as base

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	limitA := newLimitOrder(false, 4800000, 1, order.StandingTiF, 0)
	marketSellA := newMarketSellOrder(2, 1)
	marketSellB := newMarketSellOrder(2, 0)
	cancelA := newCancelOrder(targetOrderID, AssetDCR, AssetBTC, 0)

	// Order with the same commitment as limitA, but different order id.
	limitAx := new(order.LimitOrder)
	*limitAx = *limitA
	limitAx.SetTime(time.Now())

	var epochIdx, epochDur int64 = 13245678, 6000

	type args struct {
		ord    order.Order
		status order.OrderStatus
	}
	tests := []struct {
		name        string
		args        args
		wantErr     bool
		wantErrType error
	}{
		{
			name: "ok limit booked (active)",
			args: args{
				ord:    newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
				status: order.OrderStatusBooked,
			},
			wantErr: false,
		},
		{
			name: "ok limit epoch (active)",
			args: args{
				ord:    newLimitOrder(false, 5000000, 1, order.StandingTiF, 0),
				status: order.OrderStatusEpoch,
			},
			wantErr: false,
		},
		{
			name: "ok limit canceled (archived)",
			args: args{
				ord:    newLimitOrder(false, 4700000, 1, order.StandingTiF, 0),
				status: order.OrderStatusCanceled,
			},
			wantErr: false,
		},
		{
			name: "ok limit executed (archived)",
			args: args{
				ord:    limitA,
				status: order.OrderStatusExecuted,
			},
			wantErr: false,
		},
		{
			name: "limit duplicate",
			args: args{
				ord:    limitA,
				status: order.OrderStatusExecuted,
			},
			wantErr: true, // same OID already in orders_archived: primary key
		},
		{
			name: "limit duplicate by commit only",
			args: args{
				ord:    limitAx,
				status: order.OrderStatusExecuted,
			},
			wantErr: false, // new OID, commit only in archive
		},
		{
			name: "limit bad quantity (lot size)",
			args: args{
				ord:    orderBadLotSize,
				status: order.OrderStatusEpoch,
			},
			wantErr:     true,
			wantErrType: db.ArchiveError{Code: db.ErrInvalidOrder},
		},
		{
			name: "limit bad trading pair",
			args: args{
				ord:    orderBadMarket,
				status: order.OrderStatusEpoch,
			},
			wantErr:     true,
			wantErrType: db.ArchiveError{Code: db.ErrUnsupportedMarket},
		},
		{
			name: "market sell - bad status (booked)",
			args: args{
				ord:    marketSellB,
				status: order.OrderStatusBooked,
			},
			wantErr:     true,
			wantErrType: db.ArchiveError{Code: db.ErrInvalidOrder},
		},
		{
			name: "market sell - bad status (canceled)",
			args: args{
				ord:    marketSellB,
				status: order.OrderStatusCanceled,
			},
			wantErr:     true,
			wantErrType: db.ArchiveError{Code: db.ErrInvalidOrder},
		},
		{
			name: "market sell - active",
			args: args{
				ord:    marketSellB,
				status: order.OrderStatusEpoch,
			},
			wantErr: false,
		},
		{
			name: "market sell - archived",
			args: args{
				ord:    marketSellA,
				status: order.OrderStatusExecuted,
			},
			wantErr: false,
		},
		{
			name: "market sell - already in other table",
			args: args{
				ord:    marketSellA,
				status: order.OrderStatusExecuted,
			},
			wantErr: true, // same OID already in orders_archived: primary key
		},
		{
			name: "market sell - duplicate archived order",
			args: args{
				ord:    marketSellB, // still live in orders_active from the epoch insert
				status: order.OrderStatusExecuted,
			},
			wantErr:     true,
			wantErrType: db.ArchiveError{Code: db.ErrReusedCommit},
		},
		{
			name: "cancel order",
			args: args{
				ord:    cancelA,
				status: order.OrderStatusExecuted,
			},
			wantErr: false,
		},
		{
			name: "cancel order - duplicate archived order",
			args: args{
				ord:    cancelA,
				status: order.OrderStatusExecuted,
			},
			wantErr: true, // same OID already in cancels_archived: primary key
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := storeOrderForTest(archie, tt.args.ord, epochIdx, epochDur, tt.args.status)
			if (err != nil) != tt.wantErr {
				t.Errorf("StoreOrder() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				t.Logf("%s: %v", tt.name, err)
				if tt.wantErrType != nil && !db.SameErrorTypes(err, tt.wantErrType) {
					t.Errorf("Wrong error. Got %v, expected %v", err, tt.wantErrType)
				}
			}
		})
	}
}

func TestApplyOrderAcceptedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	ord := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	update := &db.OrderAcceptedUpdate{
		Order:    ord,
		EpochIdx: 13245678,
		EpochDur: 6000,
		EpochGap: db.EpochGapNA,
	}
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:        "dcr_btc",
		State:         db.MarketStateRunning,
		StartEpochIdx: update.EpochIdx,
		StartEpochDur: update.EpochDur,
		PendingAction: db.MarketPendingNone,
	})

	applyOrderAccepted := func(meta *db.EventLogMeta, update *db.OrderAcceptedUpdate) *db.EventLogEntry {
		t.Helper()

		log, err := archie.ApplyOrderAcceptedEvent(ctx, meta, update)
		if err != nil {
			t.Fatalf("ApplyOrderAcceptedEvent error: %v", err)
		}
		if log == nil {
			t.Fatalf("ApplyOrderAcceptedEvent returned nil log")
		}
		return log
	}

	requireStoredEpochOrder := func(ord order.Order) {
		t.Helper()

		stored, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote())
		if err != nil {
			t.Fatalf("Order error: %v", err)
		}
		if stored.ID() != ord.ID() {
			t.Fatalf("stored order id = %v, want %v", stored.ID(), ord.ID())
		}
		if status != order.OrderStatusEpoch {
			t.Fatalf("stored order status = %v, want %v", status, order.OrderStatusEpoch)
		}
	}

	// A new accepted order stores the epoch order and appends the first
	// order_accepted event log entry.
	firstEvent := []byte("order-accepted-event")
	firstTip := testEventApplyTip(t, nil, 1, meshevents.EventKindOrderAccepted, firstEvent, update)
	firstLog := applyOrderAccepted(&db.EventLogMeta{Event: firstEvent}, update)
	requireEventApplyLog(t, firstLog, 1, meshevents.EventKindOrderAccepted, firstEvent, firstTip, update)
	requireStoredEpochOrder(ord)

	// Reapplying the same order is idempotent for the order table, but the
	// replicated event is still recorded in the event log.
	duplicateEvent := []byte("order-accepted-duplicate")
	duplicateTip := testEventApplyTip(t, firstLog.TipHash, 2, meshevents.EventKindOrderAccepted, duplicateEvent, update)
	duplicateLog := applyOrderAccepted(&db.EventLogMeta{
		Seq:             2,
		Event:           duplicateEvent,
		ExpectedTipHash: duplicateTip,
	}, update)
	requireEventApplyLog(t, duplicateLog, 2, meshevents.EventKindOrderAccepted, duplicateEvent, duplicateTip, update)
	requireStoredEpochOrder(ord)

	// A different order cannot reuse the same commitment, and the rejected
	// apply must not advance the log.
	conflicting := new(order.LimitOrder)
	*conflicting = *ord
	conflicting.SetTime(ord.ServerTime.Add(time.Second))
	conflictUpdate := &db.OrderAcceptedUpdate{
		Order:    conflicting,
		EpochIdx: update.EpochIdx,
		EpochDur: update.EpochDur,
		EpochGap: update.EpochGap,
	}
	_, err := archie.ApplyOrderAcceptedEvent(ctx, &db.EventLogMeta{
		Seq:   3,
		Event: []byte("order-accepted-conflict"),
	}, conflictUpdate)
	var archiveErr db.ArchiveError
	if !errors.As(err, &archiveErr) || archiveErr.Code != db.ErrReusedCommit {
		t.Fatalf("ApplyOrderAcceptedEvent error = %v, want reused commit", err)
	}
	if _, status, err := archie.Order(conflicting.ID(), conflicting.Base(), conflicting.Quote()); err == nil || status != order.OrderStatusUnknown {
		t.Fatalf("conflicting order status = %v, err = %v, want unknown order", status, err)
	}
	frontier, err := archie.EventLogFrontier(ctx)
	if err != nil {
		t.Fatalf("EventLogFrontier error: %v", err)
	}
	if frontier.Seq != 2 || !bytes.Equal(frontier.TipHash, duplicateLog.TipHash) {
		t.Fatalf("frontier = (%d, %x), want (2, %x)", frontier.Seq, frontier.TipHash, duplicateLog.TipHash)
	}
}

func TestApplyOrderAcceptedEventReusesArchivedCommit(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	if err := assertNoArchivedCommitUnique(archie.db); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	archived := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	if err := storeOrderForTest(archie, archived, 100, 6000, order.OrderStatusExecuted); err != nil {
		t.Fatalf("store archived order: %v", err)
	}

	reuse := new(order.LimitOrder)
	*reuse = *archived
	reuse.SetTime(archived.ServerTime.Add(time.Second))
	update := &db.OrderAcceptedUpdate{
		Order:    reuse,
		EpochIdx: 13245678,
		EpochDur: 6000,
		EpochGap: db.EpochGapNA,
	}
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:        "dcr_btc",
		State:         db.MarketStateRunning,
		StartEpochIdx: update.EpochIdx,
		StartEpochDur: update.EpochDur,
		PendingAction: db.MarketPendingNone,
	})

	event := []byte("order-accepted-reused-commit")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindOrderAccepted, event, update)
	logEntry, err := archie.ApplyOrderAcceptedEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyOrderAcceptedEvent reuse archived commit: %v", err)
	}
	requireEventApplyLog(t, logEntry, 1, meshevents.EventKindOrderAccepted, event, tip, update)
	stored, status, err := archie.Order(reuse.ID(), reuse.Base(), reuse.Quote())
	if err != nil || stored.ID() != reuse.ID() || status != order.OrderStatusEpoch {
		t.Fatalf("reused-commit order status = %v (err %v), want epoch", status, err)
	}

	// Completing the second life must copy commit+preimage into archive.
	preimage := make([]byte, order.PreimageSize)
	copy(preimage, []byte("0123456789abcdef0123456789abcdef"))
	schema, err := archie.marketSchema(reuse.Base(), reuse.Quote())
	if err != nil {
		t.Fatalf("marketSchema: %v", err)
	}
	activeTable := fullOrderTableName(archie.dbName, schema, true)
	archivedTable := fullOrderTableName(archie.dbName, schema, false)
	if _, err := archie.db.Exec(fmt.Sprintf("UPDATE %s SET preimage = $1 WHERE oid = $2", archivedTable),
		preimage, archived.ID()); err != nil {
		t.Fatalf("set archived preimage: %v", err)
	}
	if _, err := archie.db.Exec(fmt.Sprintf("UPDATE %s SET preimage = $1 WHERE oid = $2", activeTable),
		preimage, reuse.ID()); err != nil {
		t.Fatalf("set active preimage: %v", err)
	}
	if err := archie.updateOrderStatusWithExecutor(archie.db, reuse, orderStatusExecuted); err != nil {
		t.Fatalf("archive reused-commit order: %v", err)
	}
	if _, status, err := archie.Order(reuse.ID(), reuse.Base(), reuse.Quote()); err != nil || status != order.OrderStatusExecuted {
		t.Fatalf("archived reused-commit order status = %v (err %v)", status, err)
	}
}

func TestOrdersWithCommit(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochIdx, epochDur int64 = 13245678, 6000

	requireIDs := func(got []db.CommitOrder, want ...db.CommitOrder) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("OrdersWithCommit returned %d rows, want %d", len(got), len(want))
		}
		byID := make(map[order.OrderID]order.OrderStatus, len(got))
		for _, row := range got {
			byID[row.Order.ID()] = row.Status
		}
		for _, w := range want {
			st, ok := byID[w.Order.ID()]
			if !ok || st != w.Status {
				t.Fatalf("missing or wrong status for %v: got %v, want %v", w.Order.ID(), st, w.Status)
			}
		}
	}

	older := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	newer := newLimitOrder(false, 4_900_000, 2, order.StandingTiF, 10)
	newer.Commit = older.Commit
	base, quote := older.Base(), older.Quote()
	commit := older.Commit

	got, err := archie.OrdersWithCommit(ctx, base, quote, commit, older.ServerTime)
	if err != nil {
		t.Fatalf("empty lookup: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty lookup returned %d rows", len(got))
	}

	if err := storeOrderForTest(archie, older, epochIdx, epochDur, order.OrderStatusEpoch); err != nil {
		t.Fatalf("store active: %v", err)
	}
	got, err = archie.OrdersWithCommit(ctx, base, quote, commit, older.ServerTime)
	if err != nil {
		t.Fatalf("active lookup: %v", err)
	}
	requireIDs(got, db.CommitOrder{Order: older, Status: order.OrderStatusEpoch})

	if err := archie.updateOrderStatusWithExecutor(archie.db, older, orderStatusExecuted); err != nil {
		t.Fatalf("archive older: %v", err)
	}
	if err := storeOrderForTest(archie, newer, epochIdx+1, epochDur, order.OrderStatusExecuted); err != nil {
		t.Fatalf("store newer archived: %v", err)
	}

	got, err = archie.OrdersWithCommit(ctx, base, quote, commit, older.ServerTime)
	if err != nil {
		t.Fatalf("both archived: %v", err)
	}
	requireIDs(got,
		db.CommitOrder{Order: older, Status: order.OrderStatusExecuted},
		db.CommitOrder{Order: newer, Status: order.OrderStatusExecuted},
	)

	got, err = archie.OrdersWithCommit(ctx, base, quote, commit, newer.ServerTime)
	if err != nil {
		t.Fatalf("cutoff lookup: %v", err)
	}
	requireIDs(got, db.CommitOrder{Order: newer, Status: order.OrderStatusExecuted})

	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	older = newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	active := newLimitOrder(false, 4_900_000, 2, order.StandingTiF, 10)
	active.Commit = older.Commit
	base, quote, commit = older.Base(), older.Quote(), older.Commit
	if err := storeOrderForTest(archie, older, epochIdx, epochDur, order.OrderStatusExecuted); err != nil {
		t.Fatalf("store archived life: %v", err)
	}
	if err := storeOrderForTest(archie, active, epochIdx+1, epochDur, order.OrderStatusEpoch); err != nil {
		t.Fatalf("store active life: %v", err)
	}
	got, err = archie.OrdersWithCommit(ctx, base, quote, commit, older.ServerTime)
	if err != nil {
		t.Fatalf("active plus archived: %v", err)
	}
	requireIDs(got,
		db.CommitOrder{Order: active, Status: order.OrderStatusEpoch},
		db.CommitOrder{Order: older, Status: order.OrderStatusExecuted},
	)
}

func TestApplyOrderAcceptedEventRejectsPendingSuspendBoundary(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const (
		finalEpochIdx = 13245678
		epochDur      = 6000
	)
	persistBook := true
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:          "dcr_btc",
		State:           db.MarketStateRunning,
		StartEpochIdx:   finalEpochIdx - 10,
		StartEpochDur:   epochDur,
		FinalEpochIdx:   finalEpochIdx,
		FinalEpochDur:   epochDur,
		PendingAction:   db.MarketPendingSuspend,
		PendingEpochIdx: finalEpochIdx,
		PendingEpochDur: epochDur,
		PersistBook:     &persistBook,
	})

	ord := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	ord.SetTime(time.UnixMilli((finalEpochIdx + 1) * epochDur))
	update := &db.OrderAcceptedUpdate{
		Order:    ord,
		EpochIdx: finalEpochIdx,
		EpochDur: epochDur,
		EpochGap: db.EpochGapNA,
	}
	_, err := archie.ApplyOrderAcceptedEvent(ctx, &db.EventLogMeta{
		Event: []byte("order-accepted-boundary"),
	}, update)
	if err == nil {
		t.Fatalf("ApplyOrderAcceptedEvent succeeded for boundary-stamped order")
	}
	if _, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote()); err == nil || status != order.OrderStatusUnknown {
		t.Fatalf("boundary order status = %v, err = %v, want unknown order", status, err)
	}
	frontier, err := archie.EventLogFrontier(ctx)
	if err != nil {
		t.Fatalf("EventLogFrontier error: %v", err)
	}
	if frontier.Seq != 0 || len(frontier.TipHash) != 0 {
		t.Fatalf("frontier = (%d, %x), want empty", frontier.Seq, frontier.TipHash)
	}
}

func TestApplyAdvanceEpochEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	update := &db.AdvanceEpochUpdate{
		Market:         "dcr_btc",
		ClosedEpochIdx: 42,
		OpenedEpochIdx: 43,
		EpochDur:       6000,
		ClosedOrderIDs: []order.OrderID{randomOrderID()},
	}
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:            update.Market,
		State:             db.MarketStateRunning,
		StartEpochIdx:     update.ClosedEpochIdx,
		StartEpochDur:     update.EpochDur,
		PendingAction:     db.MarketPendingNone,
		ActiveEpochIdx:    update.ClosedEpochIdx,
		ProcessedEpochIdx: update.ClosedEpochIdx - 1,
	})

	// advance_epoch records the authoritative transition event and moves the
	// lifecycle row's active epoch cursor so a restarting node can rebuild its
	// epoch memory from storage alone.
	event := []byte("advance-epoch-event")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindAdvanceEpoch, event, update)
	log, err := archie.ApplyAdvanceEpochEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyAdvanceEpochEvent error: %v", err)
	}
	requireEventApplyLog(t, log, 1, meshevents.EventKindAdvanceEpoch, event, tip, update)
	assertEventLogFrontier(t, 1, tip)
	lc, err := archie.MarketLifecycle(update.Market)
	if err != nil {
		t.Fatalf("MarketLifecycle error: %v", err)
	}
	if lc.ActiveEpochIdx != update.OpenedEpochIdx {
		t.Fatalf("active epoch cursor = %d, want %d", lc.ActiveEpochIdx, update.OpenedEpochIdx)
	}

	// An advance closing an epoch other than the cursor is rejected: every
	// event that moves the current epoch must move the cursor with it, so a
	// mismatch means cursor maintenance is broken.
	stale := *update
	stale.ClosedEpochIdx, stale.OpenedEpochIdx = 42, 43
	if _, err := archie.ApplyAdvanceEpochEvent(ctx, &db.EventLogMeta{
		Seq:   2,
		Event: []byte("advance-epoch-stale"),
	}, &stale); err == nil {
		t.Fatalf("ApplyAdvanceEpochEvent accepted a closed epoch behind the cursor")
	}
	assertEventLogFrontier(t, 1, tip)

	// A wrong expected tip on a state-consistent event (the shape of a real
	// fork: identical committed prefix, divergent next event) rejects the
	// append and leaves the frontier pinned at the last committed event.
	forked := *update
	forked.ClosedEpochIdx, forked.OpenedEpochIdx = 43, 44
	_, err = archie.ApplyAdvanceEpochEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("advance-epoch-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, &forked)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplyAdvanceEpochEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	assertEventLogFrontier(t, 1, tip)
	// The rolled-back divergent apply must not have moved the cursor.
	lc, err = archie.MarketLifecycle(update.Market)
	if err != nil {
		t.Fatalf("MarketLifecycle error: %v", err)
	}
	if lc.ActiveEpochIdx != update.OpenedEpochIdx {
		t.Fatalf("active epoch cursor after rollback = %d, want %d", lc.ActiveEpochIdx, update.OpenedEpochIdx)
	}
}

// TestApplyEpochProcessedPreimageOutcomes applies an epoch_processed event's
// preimage outcomes under suspend-drain for a pre-final epoch — the
// pipelined-straggler case the drain gate must accept (an epoch's close can
// land after the final close parks the lifecycle row in drain).
func TestApplyEpochProcessedPreimageOutcomes(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochIdx, epochDur int64 = 13245678, 6000
	persist := true
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:          "dcr_btc",
		State:           db.MarketStateRunning,
		StartEpochIdx:   epochIdx,
		StartEpochDur:   epochDur,
		FinalEpochIdx:   epochIdx + 1,
		FinalEpochDur:   epochDur,
		PendingAction:   db.MarketPendingSuspendDrain,
		PendingEpochIdx: epochIdx + 1,
		PendingEpochDur: epochDur,
		PersistBook:     &persist,
		// Neither the straggler epoch nor the final epoch is processed yet.
		ProcessedEpochIdx: epochIdx - 1,
	})
	revealed, pi := newLimitOrderRevealed(false, 4_900_000, 1, order.StandingTiF, 0)
	missed, _ := newLimitOrderRevealed(true, 4_800_000, 1, order.StandingTiF, 10)
	for _, ord := range []order.Order{revealed, missed} {
		if err := storeOrderForTest(archie, ord, epochIdx, epochDur, order.OrderStatusEpoch); err != nil {
			t.Fatalf("StoreOrder %v error: %v", ord.ID(), err)
		}
	}

	epochEnd := time.UnixMilli(1670000000000).UTC()
	revokeTime := epochEnd.Add(500 * time.Millisecond)
	epochResults := func(idx int64) *db.EpochResults {
		return &db.EpochResults{
			MktBase:   AssetDCR,
			MktQuote:  AssetBTC,
			Idx:       idx,
			Dur:       epochDur,
			MatchTime: epochEnd.UnixMilli(),
			CSum:      []byte{0x0c},
			Seed:      []byte{0x5e},
		}
	}
	update := &db.EpochProcessedUpdate{
		Epoch: epochResults(epochIdx),
		Misses: []*db.PreimageMissUpdate{{
			Order:      missed,
			RevokeTime: revokeTime,
		}},
		Reveals: []*db.PreimageRevealUpdate{{
			Order:    revealed,
			Preimage: pi,
		}},
		// Every epoch order must leave epoch status in the close.
		TradesBooked: []*order.LimitOrder{revealed},
	}

	// Apply one event and verify its durable DB effects.
	event := []byte("epoch-processed-event")
	policy := &db.ReputationOutcomePolicy{PreimageLimit: 1, OrderLimit: 1}
	logEntry, err := archie.ApplyEpochProcessedEvent(ctx, &db.EventLogMeta{Event: event}, policy, update)
	if err != nil {
		t.Fatalf("ApplyEpochProcessedEvent error: %v", err)
	}
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindEpochProcessed, event, update)
	requireEventApplyLog(t, logEntry, 1, meshevents.EventKindEpochProcessed, event, tip, update)
	assertEventLogFrontier(t, 1, tip)

	// Reveals store preimages; misses revoke the order.
	gotPI, err := archie.OrderPreimage(revealed)
	if err != nil {
		t.Fatalf("OrderPreimage error: %v", err)
	}
	if gotPI != pi {
		t.Fatalf("stored preimage = %x, want %x", gotPI, pi)
	}
	if _, status, err := archie.Order(missed.ID(), missed.Base(), missed.Quote()); err != nil || status != order.OrderStatusRevoked {
		t.Fatalf("missed order status = %v, err = %v, want revoked", status, err)
	}

	// The event facts are translated into reputation outcomes by the DB layer.
	missedPimgs, _, missedOrds, err := archie.GetUserReputationData(ctx, missed.User(), 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData missed user error: %v", err)
	}
	if len(missedPimgs) != 1 || !missedPimgs[0].Miss || len(missedOrds) != 1 || missedOrds[0].Canceled {
		t.Fatalf("missed user reputation data pimgs=%+v ords=%+v, want miss and non-canceled revoke", missedPimgs, missedOrds)
	}
	revealPimgs, _, revealOrds, err := archie.GetUserReputationData(ctx, revealed.User(), 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData revealed user error: %v", err)
	}
	if len(revealPimgs) != 1 || revealPimgs[0].Miss || len(revealOrds) != 0 {
		t.Fatalf("revealed user reputation data pimgs=%+v ords=%+v, want one success preimage only", revealPimgs, revealOrds)
	}

	// A close that leaves one of the epoch's orders in epoch status is
	// rejected, and the rejection commits nothing.
	strandedOrd, strandedPI := newLimitOrderRevealed(false, 5_100_000, 1, order.StandingTiF, 30)
	omitted, _ := newLimitOrderRevealed(true, 5_200_000, 1, order.StandingTiF, 40)
	for _, ord := range []order.Order{strandedOrd, omitted} {
		if err := storeOrderForTest(archie, ord, epochIdx+1, epochDur, order.OrderStatusEpoch); err != nil {
			t.Fatalf("StoreOrder %v error: %v", ord.ID(), err)
		}
	}
	incompleteUpdate := &db.EpochProcessedUpdate{
		Epoch: epochResults(epochIdx + 1),
		Reveals: []*db.PreimageRevealUpdate{{
			Order:    strandedOrd,
			Preimage: strandedPI,
		}},
		TradesBooked: []*order.LimitOrder{strandedOrd},
		// The omitted order has no disposition.
	}
	_, err = archie.ApplyEpochProcessedEvent(ctx, &db.EventLogMeta{Seq: 2, Event: []byte("epoch-processed-incomplete")},
		policy, incompleteUpdate)
	if err == nil || !strings.Contains(err.Error(), "in epoch status") {
		t.Fatalf("incomplete close error = %v, want epoch-status rejection", err)
	}
	if pgStatus, _, _, err := archie.orderStatusByID(strandedOrd.ID(), strandedOrd.Base(), strandedOrd.Quote()); err != nil || pgStatus != orderStatusEpoch {
		t.Fatalf("revealed order pg status after rejected close = %v (err %v), want unchanged epoch status", pgStatus, err)
	}
	assertEventLogFrontier(t, 1, tip)

	// A divergent log append rolls back all DB work staged in the transaction.
	rollbackUpdate := &db.EpochProcessedUpdate{
		Epoch: epochResults(epochIdx + 1),
		Misses: []*db.PreimageMissUpdate{{
			Order:      omitted,
			RevokeTime: revokeTime,
		}},
		Reveals: []*db.PreimageRevealUpdate{{
			Order:    strandedOrd,
			Preimage: strandedPI,
		}},
		TradesBooked: []*order.LimitOrder{strandedOrd},
	}
	_, err = archie.ApplyEpochProcessedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("epoch-processed-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, policy, rollbackUpdate)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplyEpochProcessedEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	if gotPI, err := archie.OrderPreimage(strandedOrd); err == nil && gotPI == strandedPI {
		t.Fatalf("rollback reveal preimage = %x, want not committed", gotPI)
	}
	assertEventLogFrontier(t, 1, tip)
}

func TestBookOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Store order (epoch) for new order
	// BookOrder for existing order

	var epochIdx, epochDur int64 = 13245678, 6000

	// Store standing limit order in epoch status.
	lo := newLimitOrder(true, 4200000, 1, order.StandingTiF, 0)
	err := storeOrderForTest(archie, lo, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Book the same limit order.
	err = archie.updateOrderStatusWithExecutor(archie.db, lo, orderStatusBooked)
	if err != nil {
		t.Fatalf("book status update failed: %v", err)
	}
}

func TestExecuteOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Store order (executed) for new order
	// ExecuteOrder for existing order

	var epochIdx, epochDur int64 = 13245678, 6000

	// Store standing limit order in executed status.
	lo := newLimitOrder(true, 4200000, 1, order.StandingTiF, 0)
	err := storeOrderForTest(archie, lo, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Execute the same limit order.
	err = archie.updateOrderStatusWithExecutor(archie.db, lo, orderStatusExecuted)
	if err != nil {
		t.Fatalf("executed status update failed: %v", err)
	}
}

func TestCancelOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Standing limit == OK
	var epochIdx, epochDur int64 = 13245678, 6000
	lo := newLimitOrder(false, 4800000, 1, order.StandingTiF, 0)
	err := storeOrderForTest(archie, lo, epochIdx, epochDur, order.OrderStatusBooked)
	if err != nil {
		t.Fatalf("BookOrder failed: %v", err)
	}

	// Cancel the same limit order.
	err = archie.updateOrderStatusWithExecutor(archie.db, lo, orderStatusCanceled)
	if err != nil {
		t.Fatalf("canceled status update failed: %v", err)
	}

	// Cancel an order not in the tables yet
	lo2 := newLimitOrder(true, 4600000, 1, order.StandingTiF, 0)
	err = archie.updateOrderStatusWithExecutor(archie.db, lo2, orderStatusCanceled)
	if !db.IsErrOrderUnknown(err) {
		t.Fatalf("canceled status update should have failed for unknown order.")
	}
}

func TestRevokeOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Standing limit == OK
	var epochIdx, epochDur int64 = 13245678, 6000
	lo := newLimitOrder(false, 4800000, 1, order.StandingTiF, 0)
	err := storeOrderForTest(archie, lo, epochIdx, epochDur, order.OrderStatusBooked)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Revoke the same limit order.
	cancelID, timeStamp, err := revokeOrderForTest(lo, false)
	if err != nil {
		t.Fatalf("revokeOrder failed: %v", err)
	}

	// Check for the server-generated cancel order.
	co, coStatus, err := archie.Order(cancelID, lo.BaseAsset, lo.QuoteAsset)
	if err != nil {
		t.Fatalf("Failed to locate cancel order: %v", err)
	}
	if co.ID() != cancelID {
		t.Errorf("incorrect cancel ID retrieved")
	}
	coT, ok := co.(*order.CancelOrder)
	if !ok {
		t.Fatalf("not a cancel order")
	}
	if !coT.ClientTime.Equal(timeStamp) {
		t.Errorf("got ClientTime %v, expected %v", coT.ClientTime, timeStamp)
	}
	if !coT.ServerTime.Equal(timeStamp) {
		t.Errorf("got ServerTime %v, expected %v", coT.ServerTime, timeStamp)
	}
	if coStatus != order.OrderStatusRevoked {
		t.Errorf("got order status %v, expected %v", coStatus, order.OrderStatusRevoked)
	}
	if !coT.Commit.IsZero() {
		t.Errorf("generated cancel order did not have NULL/zero-value commitment")
	}

	// Market orders may be revoked too, while swap is in progress.
	// NOTE: executed -> revoked status change may be odd.
	mo := newMarketSellOrder(1, 0)
	err = storeOrderForTest(archie, mo, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	cancelID, timeStamp, err = revokeOrderForTest(mo, true)
	if err != nil {
		t.Fatalf("revokeOrder (uncounted) failed: %v", err)
	}

	co, coStatus, err = archie.Order(cancelID, mo.BaseAsset, mo.QuoteAsset)
	if err != nil {
		t.Fatalf("Failed to locate cancel order: %v", err)
	}
	if co.ID() != cancelID {
		t.Errorf("incorrect cancel ID retrieved")
	}
	coT, ok = co.(*order.CancelOrder)
	if !ok {
		t.Fatalf("not a cancel order")
	}
	if !coT.ClientTime.Equal(timeStamp) {
		t.Errorf("got ClientTime %v, expected %v", coT.ClientTime, timeStamp)
	}
	if !coT.ServerTime.Equal(timeStamp) {
		t.Errorf("got ServerTime %v, expected %v", coT.ServerTime, timeStamp)
	}
	if coStatus != order.OrderStatusRevoked {
		t.Errorf("got order status %v, expected %v", coStatus, order.OrderStatusRevoked)
	}
	if !coT.Commit.IsZero() {
		t.Errorf("generated cancel order did not have NULL/zero-value commitment")
	}

	// Revoke an order not in the tables yet
	lo2 := newLimitOrder(true, 4600000, 1, order.StandingTiF, 0)
	_, _, err = revokeOrderForTest(lo2, false)
	if !db.IsErrOrderUnknown(err) {
		t.Fatalf("revokeOrder should have failed for unknown order.")
	}
}

func TestLoadOrderUnknown(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var oid order.OrderID
	copy(oid[:], orderID0)

	ordOut, statusOut, err := archie.Order(oid, mktInfo.Base, mktInfo.Quote)
	if err == nil || ordOut != nil {
		t.Errorf("Order should have failed to load non-existent order")
	}
	if statusOut != order.OrderStatusUnknown {
		t.Errorf("status of non-existent order should be OrderStatusUnknown, got %s", statusOut)
	}
}

func TestStoreLoadLimitOrderActive(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// Limit: buy, standing, booked
	ordIn := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0)
	statusIn := order.OrderStatusBooked

	// Do not use Stringers when dumping, and stop after 4 levels deep
	spew.Config.MaxDepth = 4
	spew.Config.DisableMethods = true

	oid, base, quote := ordIn.ID(), ordIn.BaseAsset, ordIn.QuoteAsset

	err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	ordOut, statusOut, err := archie.Order(oid, base, quote)
	if err != nil {
		t.Fatalf("Order failed: %v", err)
	}

	if ordOut.ID() != oid {
		t.Errorf("Incorrect OrderId for retrieved order. Got %v, expected %v.",
			ordOut.ID(), oid)
		spew.Dump(ordIn)
		spew.Dump(ordOut)
	}

	if statusOut != statusIn {
		t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
			statusOut, statusIn)
	}
}

func TestStoreLoadLimitOrderArchived(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// Limit: buy, standing, executed
	ordIn := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0)
	statusIn := order.OrderStatusExecuted

	// Do not use Stringers when dumping, and stop after 4 levels deep
	spew.Config.MaxDepth = 4
	spew.Config.DisableMethods = true

	oid, base, quote := ordIn.ID(), ordIn.BaseAsset, ordIn.QuoteAsset

	err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	ordOut, statusOut, err := archie.Order(oid, base, quote)
	if err != nil {
		t.Fatalf("Order failed: %v", err)
	}

	if ordOut.ID() != oid {
		t.Errorf("Incorrect OrderId for retrieved order. Got %v, expected %v.",
			ordOut.ID(), oid)
		spew.Dump(ordIn)
		spew.Dump(ordOut)
	}

	if statusOut != statusIn {
		t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
			statusOut, statusIn)
	}
}

func TestStoreLoadMarketOrderActive(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// Market: sell, epoch (active)
	ordIn := newMarketSellOrder(1, 0)
	statusIn := order.OrderStatusEpoch

	// Do not use Stringers when dumping, and stop after 4 levels deep
	spew.Config.MaxDepth = 4
	spew.Config.DisableMethods = true

	oid, base, quote := ordIn.ID(), ordIn.BaseAsset, ordIn.QuoteAsset

	err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	ordOut, statusOut, err := archie.Order(oid, base, quote)
	if err != nil {
		t.Fatalf("Order failed: %v", err)
	}

	if ordOut.ID() != oid {
		t.Errorf("Incorrect OrderId for retrieved order. Got %v, expected %v.",
			ordOut.ID(), oid)
		spew.Dump(ordIn)
		spew.Dump(ordOut)
	}

	if statusOut != statusIn {
		t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
			statusOut, statusIn)
	}
}

func TestStoreLoadCancelOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	// Cancel: epoch (active)
	ordIn := newCancelOrder(targetOrderID, AssetDCR, AssetBTC, 0)
	statusIn := order.OrderStatusEpoch

	// Do not use Stringers when dumping, and stop after 4 levels deep
	spew.Config.MaxDepth = 4
	spew.Config.DisableMethods = true

	oid, base, quote := ordIn.ID(), ordIn.BaseAsset, ordIn.QuoteAsset

	err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	ordOut, statusOut, err := archie.Order(oid, base, quote)
	if err != nil {
		t.Fatalf("Order failed: %v", err)
	}

	if ordOut.ID() != oid {
		t.Errorf("Incorrect OrderId for retrieved order. Got %v, expected %v.",
			ordOut.ID(), oid)
		spew.Dump(ordIn)
		spew.Dump(ordOut)
	}

	if statusOut != statusIn {
		t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
			statusOut, statusIn)
	}
}

func TestOrderStatusUnknown(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ord := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0) // not stored
	_, _, _, err := archie.OrderStatus(ord)
	if err == nil {
		t.Fatalf("OrderStatus succeeded to find nonexistent order!")
	}
	if !db.SameErrorTypes(err, db.ArchiveError{Code: db.ErrUnknownOrder}) {
		if errA, ok := err.(db.ArchiveError); ok {
			t.Fatalf("Expected ArchiveError with code ErrUnknownOrder, got %d", errA.Code)
		}
		t.Fatalf("Expected ArchiveError with code ErrUnknownOrder, got %v", err)
	}
}

// Test ActiveOrderCoins, BookOrders, and EpochOrders.
func TestActiveOrderCoins(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	multiCoinLO := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0)
	multiCoinLO.Coins = append(multiCoinLO.Coins, order.CoinID{0x22, 0x23})

	epochLO := newLimitOrder(true, 1, 1, order.StandingTiF, 0)
	epochCO := newCancelOrder(multiCoinLO.ID(), AssetDCR, AssetBTC, 0)

	orderStatuses := []struct {
		ord         order.Order
		status      order.OrderStatus
		activeCoins int
	}{
		{
			multiCoinLO,
			order.OrderStatusBooked, // active, buy, booked
			-1,
		},
		{
			newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
			order.OrderStatusExecuted, // archived, buy
			0,
		},
		{
			newMarketSellOrder(2, 0),
			order.OrderStatusEpoch, // active, sell, epoch
			1,
		},
		{
			epochLO,
			order.OrderStatusEpoch, // active, buy, epoch
			1,
		},
		{
			epochCO,
			order.OrderStatusEpoch, // cancel, epoch
			0,
		},
		{
			newMarketSellOrder(1, 0),
			order.OrderStatusExecuted, // archived, sell
			0,
		},
		{
			newMarketBuyOrder(2000000000, 0),
			order.OrderStatusEpoch, // active, buy
			-1,
		},
		{
			newMarketBuyOrder(2100000000, 0),
			order.OrderStatusExecuted, // archived, buy
			0,
		},
	}

	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		statusIn := orderStatuses[i].status
		err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
		if err != nil {
			t.Fatalf("StoreOrder failed: %v", err)
		}
	}

	baseCoins, quoteCoins, err := archie.ActiveOrderCoins(mktInfo.Base, mktInfo.Quote)
	if err != nil {
		t.Fatalf("ActiveOrderCoins failed: %v", err)
	}

	for _, os := range orderStatuses {
		var coins, wantCoins []order.CoinID
		switch os.activeCoins {
		case 0: // no active
		case 1: // active base coins (sell order)
			coins = baseCoins[os.ord.ID()]
			wantCoins = os.ord.Trade().Coins
		case -1: // active quote coins (buy order)
			coins = quoteCoins[os.ord.ID()]
			wantCoins = os.ord.Trade().Coins
		}

		if len(coins) != len(wantCoins) {
			t.Errorf("Order %v has %d coins, expected %d", os.ord.ID(),
				len(coins), len(wantCoins))
			continue
		}
		for i := range coins {
			if !bytes.Equal(coins[i], wantCoins[i]) {
				t.Errorf("Order %v coin %d mismatch:\n\tgot %v\n\texpected %v",
					os.ord.ID(), i, coins[i], wantCoins[i])
			}
		}
	}

	bookOrders, err := archie.BookOrders(mktInfo.Base, mktInfo.Quote)
	if err != nil {
		t.Fatalf("BookOrders failed: %v", err)
	}

	if len(bookOrders) != 1 {
		t.Fatalf("got %d book orders, expected 1", len(bookOrders))
	}

	// Verify the order ID of the loaded order is correct. This ensures the
	// order is being loaded with all the fields to provide and identical
	// serialization.
	if multiCoinLO.ID() != bookOrders[0].ID() {
		t.Errorf("loaded book order has an incorrect order ID. Got %v, expected %v",
			bookOrders[0].ID(), multiCoinLO.ID())
	}

	los, mos, cos, err := archie.epochOrders(mktInfo.Base, mktInfo.Quote)
	if err != nil {
		t.Fatalf("epochOrders failed: %v", err)
	}

	if len(los) != 1 || len(mos) != 2 || len(cos) != 1 {
		t.Fatalf("got %d epoch limit orders, %d epoch market orders, and %d epoch cancel orders, expected 1, 2, and 1",
			len(los), len(mos), len(cos))
	}

	// Verify the order ID of the loaded order is correct. This ensures the
	// order is being loaded with all the fields to provide and identical
	// serialization.
	if epochLO.ID() != los[0].ID() {
		t.Errorf("epoch limit order has an incorrect order ID. Got %v, expected %v",
			los[0].ID(), epochLO.ID())
	}
	if epochCO.ID() != cos[0].ID() {
		t.Errorf("epoch cancel order has an incorrect order ID. Got %v, expected %v",
			cos[0].ID(), epochCO.ID())
	}

	// The exported version should return the same orders.
	orders, err := archie.EpochOrders(mktInfo.Base, mktInfo.Quote)
	if err != nil {
		t.Fatalf("EpochOrders failed: %v", err)
	}

	if len(orders) != 4 {
		t.Fatalf("got %d epoch orders, expected 4", len(orders))
	}
	for _, o := range orders {
		if o.ID() == los[0].ID() ||
			o.ID() == mos[0].ID() ||
			o.ID() == mos[1].ID() ||
			o.ID() == cos[0].ID() {
			continue
		}
		t.Fatalf("order %v in EpochOrders but not epochOrders", o.ID())
	}
}

func TestOrderStatus(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	orderStatuses := []struct {
		ord    order.Order
		status order.OrderStatus
	}{
		{
			newLimitOrder(false, 4900000, 1, order.StandingTiF, 0),
			order.OrderStatusBooked, // active
		},
		{
			newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
			order.OrderStatusExecuted, // archived
		},
		{
			newMarketSellOrder(2, 0),
			order.OrderStatusEpoch, // active
		},
		{
			newMarketSellOrder(1, 0),
			order.OrderStatusExecuted, // archived
		},
		{
			newMarketBuyOrder(2000000000, 0),
			order.OrderStatusEpoch, // active
		},
		{
			newMarketBuyOrder(2100000000, 0),
			order.OrderStatusExecuted, // archived
		},
	}

	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		trade := ordIn.Trade()
		statusIn := orderStatuses[i].status
		err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
		if err != nil {
			t.Fatalf("StoreOrder failed: %v", err)
		}

		statusOut, typeOut, filledOut, err := archie.OrderStatus(ordIn)
		if err != nil {
			t.Fatalf("OrderStatus(%d:%v) failed: %v", i, ordIn, err)
		}

		if statusOut != statusIn {
			t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
				statusOut, statusIn)
		}

		if typeOut != ordIn.Type() {
			t.Errorf("Incorrect OrderType for retrieved order. Got %v, expected %v.",
				typeOut, ordIn.Type())
		}

		if filledOut != int64(trade.Filled()) {
			t.Errorf("Incorrect FillAmt for retrieved order. Got %v, expected %v.",
				filledOut, trade.Filled())
		}
	}
}

func TestCancelOrderStatus(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	// Cancel: executed (archived)
	ordIn := newCancelOrder(targetOrderID, mktInfo.Base, mktInfo.Quote, 0)
	statusIn := order.OrderStatusExecuted

	//oid, base, quote := ordIn.ID(), ordIn.BaseAsset, ordIn.QuoteAsset

	err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	statusOut, typeOut, filledOut, err := archie.OrderStatus(ordIn)
	if err != nil {
		t.Fatalf("Order failed: %v", err)
	}

	if statusOut != statusIn {
		t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
			statusOut, statusIn)
	}

	if typeOut != ordIn.Type() {
		t.Errorf("Incorrect OrderType for retrieved order. Got %v, expected %v.",
			typeOut, ordIn.Type())
	}

	if filledOut != -1 {
		t.Errorf("Incorrect FilledAmt for retrieved order. Got %v, expected %v.",
			filledOut, -1)
	}
}

func TestUpdateOrderUnknown(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ord := newLimitOrder(false, 4900000, 1, order.StandingTiF, 0) // not stored

	err := archie.updateOrderStatusWithExecutor(archie.db, ord, orderStatusExecuted)
	if err == nil {
		t.Fatalf("UpdateOrder succeeded to update nonexistent order!")
	}
	if !db.SameErrorTypes(err, db.ArchiveError{Code: db.ErrUnknownOrder}) {
		if errA, ok := err.(db.ArchiveError); ok {
			t.Fatalf("Expected ArchiveError with code ErrUnknownOrder, got %d", errA.Code)
		}
		t.Fatalf("Expected ArchiveError with code ErrUnknownOrder, got %v", err)
	}
}

func TestUpdateOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	orderStatuses := []struct {
		ord       order.Order
		status    order.OrderStatus
		newStatus order.OrderStatus
		newFilled uint64
		wantErr   bool
	}{
		{
			newLimitOrder(false, 4900000, 1, order.StandingTiF, 0),
			order.OrderStatusEpoch,  // active
			order.OrderStatusBooked, // active
			0,
			false,
		},
		{
			newLimitOrder(false, 4100000, 1, order.StandingTiF, 0),
			order.OrderStatusBooked,   // active
			order.OrderStatusExecuted, // archived
			0,
			false,
		},
		{
			newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
			order.OrderStatusExecuted, // archived
			order.OrderStatusBooked,   // active, should err
			0,
			true,
		},
		{
			newMarketSellOrder(2, 0),
			order.OrderStatusEpoch,  // active
			order.OrderStatusBooked, // active, invalid for market
			0,
			false,
		},
		{
			newMarketSellOrder(1, 0),
			order.OrderStatusExecuted, // archived
			order.OrderStatusExecuted, // archived, no change
			0,
			false,
		},
		{
			newMarketBuyOrder(2000000000, 0),
			order.OrderStatusEpoch,    // active
			order.OrderStatusExecuted, // archived
			2000000000,
			false,
		},
		{
			newCancelOrder(targetOrderID, mktInfo.Base, mktInfo.Quote, 1),
			order.OrderStatusEpoch,    // active
			order.OrderStatusExecuted, // archived
			0,
			false,
		},
		{
			newCancelOrder(targetOrderID, mktInfo.Base, mktInfo.Quote, 2),
			order.OrderStatusExecuted, // archived
			order.OrderStatusCanceled, // archived
			0,
			false,
		},
	}

	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		statusIn := orderStatuses[i].status
		err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
		if err != nil {
			t.Fatalf("StoreOrder failed: %v", err)
		}

		switch ot := ordIn.(type) {
		case *order.LimitOrder:
			ot.FillAmt = orderStatuses[i].newFilled
		case *order.MarketOrder:
			ot.FillAmt = orderStatuses[i].newFilled
		}

		newStatus := orderStatuses[i].newStatus
		err = archie.updateOrderStatusWithExecutor(archie.db, ordIn, marketToPgStatus(newStatus))
		if (err != nil) != orderStatuses[i].wantErr {
			t.Fatalf("updateOrderStatusWithExecutor(%d:%v, %s) failed: %v", i, ordIn, newStatus, err)
		}
	}
}

func TestStorePreimage(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	lo, pi := newLimitOrderRevealed(false, 4900000, 1, order.StandingTiF, 0)
	err := storeOrderForTest(archie, lo, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	err = archie.storePreimage(archie.db, lo, pi)
	if err != nil {
		t.Fatalf("storePreimage failed: %v", err)
	}

	piOut, err := archie.OrderPreimage(lo)
	if err != nil {
		t.Fatalf("OrderPreimage failed: %v", err)
	}

	if pi != piOut {
		t.Errorf("got preimage %v, expected %v", piOut, pi)
	}

	// Now test OrderPreimage when preimage is NULL.
	lo2, _ := newLimitOrderRevealed(false, 4900000, 1, order.StandingTiF, 0)
	err = storeOrderForTest(archie, lo2, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	piOut2, err := archie.OrderPreimage(lo2)
	if err != nil {
		t.Fatalf("OrderPreimage failed: %v", err)
	}
	if !piOut2.IsZero() {
		t.Errorf("Preimage should have been the zero value, got %v", piOut2)
	}
}

func TestFailCancelOrder(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	co := newCancelOrder(targetOrderID, mktInfo.Base, mktInfo.Quote, 1)
	err := storeOrderForTest(archie, co, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	err = archie.updateOrderStatusWithExecutor(archie.db, co, orderStatusFailed)
	if err != nil {
		t.Fatalf("failed status update failed: %v", err)
	}
	_, status, err := loadCancelOrder(archie.db, archie.dbName, mktInfo.Name, co.ID())
	if err != nil {
		t.Errorf("loadCancelOrder failed: %v", err)
	}

	if status != orderStatusFailed {
		t.Errorf("cancel order should have been %s, got %s", orderStatusFailed, status)
	}
}

func TestUpdateOrderFilled(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	// order ID for a cancel order
	orderID0, _ := hex.DecodeString("dd64e2ae2845d281ba55a6d46eceb9297b2bdec5c5bada78f9ae9e373164df0d")
	var targetOrderID order.OrderID
	copy(targetOrderID[:], orderID0)

	orderStatuses := []struct {
		ord           *order.LimitOrder
		status        order.OrderStatus
		newFilled     uint64
		wantUpdateErr bool
	}{
		{
			newLimitOrder(false, 4900000, 1, order.StandingTiF, 0),
			order.OrderStatusBooked, // active
			0,
			false,
		},
		{
			newLimitOrder(false, 4100000, 1, order.StandingTiF, 0),
			order.OrderStatusBooked, // active
			0,
			false,
		},
		{
			newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
			order.OrderStatusExecuted, // archived
			0,
			false,
		},
	}

	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		statusIn := orderStatuses[i].status
		err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
		if err != nil {
			t.Fatalf("StoreOrder failed: %v", err)
		}

		ordIn.FillAmt = orderStatuses[i].newFilled

		err = archie.updateOrderFilledByIDWithExecutor(archie.db, ordIn.ID(),
			ordIn.Base(), ordIn.Quote(), int64(ordIn.Trade().Filled()))
		if (err != nil) != orderStatuses[i].wantUpdateErr {
			t.Fatalf("updateOrderFilledByIDWithExecutor(%d:%v) failed: %v", i, ordIn, err)
		}
	}
}

func TestUserOrderStatuses(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var epochIdx, epochDur int64 = 13245678, 6000

	orderStatuses := []struct {
		ord    order.Order
		status order.OrderStatus
	}{
		{
			newLimitOrder(false, 4900000, 1, order.StandingTiF, 0),
			order.OrderStatusBooked, // active
		},
		{
			newLimitOrder(false, 4500000, 1, order.StandingTiF, 0),
			order.OrderStatusExecuted, // archived
		},
		{
			newMarketSellOrder(2, 0),
			order.OrderStatusEpoch, // active
		},
		{
			newMarketSellOrder(1, 0),
			order.OrderStatusExecuted, // archived
		},
		{
			newMarketBuyOrder(2000000000, 0),
			order.OrderStatusEpoch, // active
		},
		{
			newMarketBuyOrder(2100000000, 0),
			order.OrderStatusExecuted, // archived
		},
	}

	unsavedOrder1 := newMarketBuyOrder(3000000000, 0)
	unsavedOrder2 := newMarketSellOrder(4, 0)

	orders := make([]order.Order, 0, len(orderStatuses)+2)
	orderIDs := make([]order.OrderID, 0, len(orderStatuses)+2)

	// Add unsaved orders
	orders = append(orders, unsavedOrder1, unsavedOrder2)
	orderIDs = append(orderIDs, unsavedOrder1.ID(), unsavedOrder2.ID())

	accountID := randomAccountID()
	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		statusIn := orderStatuses[i].status
		if lo, ok := ordIn.(*order.LimitOrder); ok {
			lo.BaseAsset, lo.QuoteAsset = AssetBTC, AssetLTC // swap the assets to test across different mkts
		}
		ordIn.Prefix().AccountID = accountID
		err := storeOrderForTest(archie, ordIn, epochIdx, epochDur, statusIn)
		if err != nil {
			t.Fatalf("StoreOrder failed: %v", err)
		}
		orders = append(orders, ordIn)
		orderIDs = append(orderIDs, ordIn.ID())
	}

	// All orders except the 2 limit orders are DCR-BTC.
	orderStatusesOut, err := archie.UserOrderStatuses(accountID, AssetDCR, AssetBTC, orderIDs)
	if err != nil {
		t.Fatalf("OrderStatuses failed: %v", err)
	}
	if len(orderStatusesOut) != len(orderStatuses)-2 /*the 2 limits*/ {
		t.Fatalf("OrderStatuses returned %d orders instead of %d", len(orderStatusesOut), len(orderStatuses)-2)
	}
	outMap := make(map[order.OrderID]*db.OrderStatus, len(orderStatusesOut))
	for _, orderStatus := range orderStatusesOut {
		outMap[orderStatus.ID] = orderStatus
	}
	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		orderOut, found := outMap[ordIn.ID()]
		if !found {
			continue
		}
		statusIn := orderStatuses[i].status
		if orderOut.Status != statusIn {
			t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
				orderOut.Status, statusIn)
		}
	}

	// Check statuses for the 2 limit orders that are BTC-LTC.
	orderStatusesOut, err = archie.UserOrderStatuses(accountID, AssetBTC, AssetLTC, orderIDs)
	if err != nil {
		t.Fatalf("OrderStatuses failed: %v", err)
	}
	if len(orderStatusesOut) != 2 /*the 2 limits*/ {
		t.Fatalf("OrderStatuses returned %d orders instead of %d", len(orderStatusesOut), 2)
	}
	outMap = make(map[order.OrderID]*db.OrderStatus, len(orderStatusesOut))
	for _, orderStatus := range orderStatusesOut {
		outMap[orderStatus.ID] = orderStatus
	}
	for i := range orderStatuses {
		ordIn := orderStatuses[i].ord
		orderOut, found := outMap[ordIn.ID()]
		if !found {
			continue
		}
		statusIn := orderStatuses[i].status
		if orderOut.Status != statusIn {
			t.Errorf("Incorrect OrderStatus for retrieved order. Got %v, expected %v.",
				orderOut.Status, statusIn)
		}
	}

	// Expect nothing for wrong user ID.
	orderStatusesOut, err = archie.UserOrderStatuses(randomAccountID(), AssetDCR, AssetBTC, orderIDs)
	if err != nil {
		t.Fatalf("OrderStatuses failed: %v", err)
	}
	if len(orderStatusesOut) != 0 {
		t.Fatalf("OrderStatuses returned %d orders for wrong account ID", len(orderStatusesOut))
	}
}
func TestActiveUserOrderStatuses(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Two orders, different accounts, DCR-BTC.
	maker := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	taker := newLimitOrder(true, 4490000, 1, order.StandingTiF, 10)

	var epochIdx, epochDur int64 = 13245678, 6000
	err := storeOrderForTest(archie, maker, epochIdx, epochDur, order.OrderStatusBooked)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}
	err = storeOrderForTest(archie, taker, epochIdx, epochDur, order.OrderStatusBooked)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Second order from the same maker account.
	maker2 := newLimitOrder(false, 4500000, 1, order.ImmediateTiF, 20)
	maker2.AccountID = maker.AccountID

	// Store it.
	err = storeOrderForTest(archie, maker2, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Same taker account, different market (BTC-LTC).
	taker2 := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 30)
	taker2.BaseAsset = AssetBTC
	taker2.QuoteAsset = AssetLTC
	taker2.AccountID = taker.AccountID

	// Store it.
	err = storeOrderForTest(archie, taker2, epochIdx, epochDur, order.OrderStatusEpoch)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Store cancel order for taker account.
	taker2Incomplete := newLimitOrder(true, 4390000, 1, order.StandingTiF, 20)
	taker2Incomplete.AccountID = taker.AccountID
	err = storeOrderForTest(archie, taker2Incomplete, epochIdx, epochDur, order.OrderStatusCanceled)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Maker should have 2 active orders in 1 market.
	// Taker should have 2 active orders in 2 markets and 1 inactive (canceled) order.

	tests := []struct {
		name              string
		acctID            account.AccountID
		numExpected       int
		wantOrderIDs      []order.OrderID
		wantOrderStatuses []order.OrderStatus
		wantedErr         error
	}{
		{
			"ok maker",
			maker.User(),
			2,
			[]order.OrderID{maker.ID(), maker2.ID()},
			[]order.OrderStatus{order.OrderStatusBooked, order.OrderStatusEpoch},
			nil,
		},
		{
			"ok taker",
			taker.User(),
			2,
			[]order.OrderID{taker.ID(), taker2.ID()},
			[]order.OrderStatus{order.OrderStatusBooked, order.OrderStatusEpoch},
			nil,
		},
		{
			"nope",
			randomAccountID(),
			0,
			nil,
			nil,
			nil,
		},
	}

	idInSlice := func(oid order.OrderID, oids []order.OrderID) int {
		for i := range oids {
			if oids[i] == oid {
				return i
			}
		}
		return -1
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orderStatuses, err := archie.ActiveUserOrderStatuses(tt.acctID)
			if err != tt.wantedErr {
				t.Fatal(err)
			}
			if len(orderStatuses) != tt.numExpected {
				t.Errorf("Retrieved %d active orders for user %v, expected %d.", len(orderStatuses), tt.acctID, tt.numExpected)
			}
			for _, ord := range orderStatuses {
				wantId := idInSlice(ord.ID, tt.wantOrderIDs)
				if wantId == -1 {
					t.Errorf("Unexpected order ID %v retrieved.", ord.ID)
					continue
				}
				if ord.Status != tt.wantOrderStatuses[wantId] {
					t.Errorf("Incorrect order status for order %v. Got %d, want %d.",
						ord.ID, ord.Status, tt.wantOrderStatuses[wantId])
				}
			}
		})
	}
}

func TestCompletedUserOrders(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	nowMs := func() int64 {
		return time.Now().UnixMilli()
	}

	// Two orders, different accounts, DCR-BTC.
	maker := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	taker := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	var epochIdx, epochDur int64 = 13245678, 6000
	err := storeOrderForTest(archie, maker, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}
	err = storeOrderForTest(archie, taker, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Set the orders' swap completion times.
	tSwapDoneMaker := nowMs()
	if err = archie.setOrderCompleteTime(archie.db, maker, tSwapDoneMaker); err != nil {
		t.Fatalf("setOrderCompleteTime failed: %v", err)
	}

	tSwapDoneTaker := tSwapDoneMaker + 10
	if err = archie.setOrderCompleteTime(archie.db, taker, tSwapDoneTaker); err != nil {
		t.Fatalf("setOrderCompleteTime failed: %v", err)
	}

	// Second order from the same maker account.
	maker2 := newLimitOrder(false, 4500000, 1, order.StandingTiF, 20)
	maker2.AccountID = maker.AccountID

	// Store it.
	err = storeOrderForTest(archie, maker2, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}
	// Set swap complete time.
	tSwapDoneMaker2 := nowMs()
	if err = archie.setOrderCompleteTime(archie.db, maker2, tSwapDoneMaker2); err != nil {
		t.Fatalf("setOrderCompleteTime failed: %v", err)
	}

	// Same taker account, different market (BTC-LTC).
	taker2 := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 30)
	taker2.BaseAsset = AssetBTC
	taker2.QuoteAsset = AssetLTC
	taker2.AccountID = taker.AccountID

	// Store it.
	err = storeOrderForTest(archie, taker2, epochIdx, epochDur, order.OrderStatusExecuted)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Set swap complete time.
	tSwapDoneTaker2 := nowMs()
	if err = archie.setOrderCompleteTime(archie.db, taker2, tSwapDoneTaker2); err != nil {
		t.Fatalf("setOrderCompleteTime failed: %v", err)
	}

	// Order without completion time set.
	taker2Incomplete := newLimitOrder(true, 4390000, 1, order.StandingTiF, 20)
	taker2Incomplete.AccountID = taker.AccountID
	err = storeOrderForTest(archie, taker2Incomplete, epochIdx, epochDur, order.OrderStatusCanceled) // archived, but not complete
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}
	// NO SetOrderCompleteTime, BUT in an orders_archived table.

	// Try and fail to set completion time for an order not in executed status.
	taker3 := newLimitOrder(true, 4390000, 1, order.StandingTiF, 20)
	err = storeOrderForTest(archie, taker3, epochIdx, epochDur, order.OrderStatusBooked)
	if err != nil {
		t.Fatalf("StoreOrder failed: %v", err)
	}

	// Set swap complete time.
	tSwapDoneTaker3 := nowMs()
	if err = archie.setOrderCompleteTime(archie.db, taker3, tSwapDoneTaker3); !db.IsErrOrderNotExecuted(err) {
		t.Fatalf("setOrderCompleteTime should have returned a ErrOrderNotExecuted error for booked (not executed) order")
	}

	// Maker should have 2 completed orders in 1 market.
	// Taker should have 2 completed orders in 2 markets.

	tests := []struct {
		name          string
		acctID        account.AccountID
		numExpected   int
		wantOrderIDs  []order.OrderID
		wantCompTimes []int64
		wantedErr     error
	}{
		{
			"ok maker",
			maker.User(),
			2,
			[]order.OrderID{maker.ID(), maker2.ID()},
			[]int64{tSwapDoneMaker, tSwapDoneMaker2},
			nil,
		},
		{
			"ok taker",
			taker.User(),
			2,
			[]order.OrderID{taker.ID(), taker2.ID()},
			[]int64{tSwapDoneTaker, tSwapDoneTaker2},
			nil,
		},
		{
			"nope",
			randomAccountID(),
			0,
			nil,
			nil,
			nil,
		},
	}

	idInSlice := func(mid order.OrderID, mids []order.OrderID) int {
		for i := range mids {
			if mids[i] == mid {
				return i
			}
		}
		return -1
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oids, compTimes, err := archie.CompletedUserOrders(tt.acctID, cancelThreshWindow)
			if err != tt.wantedErr {
				t.Fatal(err)
			}
			if len(oids) != tt.numExpected {
				t.Errorf("Retrieved %d completed orders for user %v, expected %d.", len(oids), tt.acctID, tt.numExpected)
			}
			for i := range oids {
				loc := idInSlice(oids[i], tt.wantOrderIDs)
				if loc == -1 {
					t.Errorf("Unexpected order ID %v retrieved.", oids[i])
					continue
				}
				if compTimes[i] != tt.wantCompTimes[loc] {
					t.Errorf("Incorrect order completion time. Got %d, want %d.",
						compTimes[loc], tt.wantCompTimes[i])
				}
			}
		})
	}
}
