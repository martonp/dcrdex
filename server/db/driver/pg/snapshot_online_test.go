//go:build pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
)

const snapTestEpochIdx int64 = 12345

// testOrderTimeBase is the Unix second used by newLimitOrder/newCancelOrder at
// offset 0 (2019). Add (now - base) so server_time lands near "now".
const testOrderTimeBase int64 = 1566497653

func snapNowOffset() int64 { return time.Now().Unix() - testOrderTimeBase }

func mustCleanTables(t *testing.T) {
	t.Helper()
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
}

func mustStoreOrder(t *testing.T, ord order.Order, status order.OrderStatus) {
	t.Helper()
	if err := storeOrderForTest(archie, ord, snapTestEpochIdx, int64(EpochDuration), status); err != nil {
		t.Fatalf("StoreOrder(%v): %v", ord.ID(), err)
	}
}

func mustOrderStatus(t *testing.T, ord order.Order, want order.OrderStatus) {
	t.Helper()
	stored, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote())
	if err != nil {
		t.Fatalf("Order(%v): %v", ord.ID(), err)
	}
	if stored.ID() != ord.ID() || status != want {
		t.Fatalf("Order(%v) = %v/%v, want %v/%v", ord.ID(), stored.ID(), status, ord.ID(), want)
	}
}

func mustOrderUnknown(t *testing.T, ord order.Order) {
	t.Helper()
	if _, _, err := archie.Order(ord.ID(), ord.Base(), ord.Quote()); !db.IsErrOrderUnknown(err) {
		t.Fatalf("Order(%v): err = %v, want unknown order", ord.ID(), err)
	}
}

// reloadFromSnapshot writes a snapshot, wipes to an empty prepared DB, and loads it.
func reloadFromSnapshot(t *testing.T, ctx context.Context) *db.EventLogPosition {
	t.Helper()
	var buf bytes.Buffer
	writeFrontier, err := archie.WriteSnapshot(ctx, &buf)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	mustCleanTables(t)
	loadFrontier, err := archie.LoadSnapshot(ctx, &buf)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if loadFrontier.Seq != writeFrontier.Seq || !bytes.Equal(loadFrontier.TipHash, writeFrontier.TipHash) {
		t.Fatalf("loaded frontier = %s, want %s", loadFrontier, writeFrontier)
	}
	return loadFrontier
}

// insertTestMatch stores a match; if inactive, marks it settled.
func insertTestMatch(t *testing.T, maker *order.LimitOrder, taker order.Order, epochIdx uint64, inactive bool) *order.Match {
	t.Helper()
	match := newMatch(maker, taker, taker.Trade().Quantity, order.EpochID{Idx: epochIdx, Dur: EpochDuration})
	if err := insertMatchForTest(match); err != nil {
		t.Fatalf("insertMatchForTest: %v", err)
	}
	if inactive {
		if err := archie.setMatchInactive(archie.db, db.MatchID(match), false); err != nil {
			t.Fatalf("setMatchInactive: %v", err)
		}
	}
	return match
}

func appendSnapEvent(t *testing.T, ctx context.Context, payload []byte) *db.EventLogEntry {
	t.Helper()
	entry, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: payload}, "snap_test",
		payload, func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("applyEventTx: %v", err)
	}
	return entry
}

// TestSnapshotRoundTrip: write → empty target → load restores state and frontier.
func TestSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	mustCleanTables(t)

	lo := newLimitOrder(false, 4_800_000, 1, order.StandingTiF, 0)
	mustStoreOrder(t, lo, order.OrderStatusBooked)

	recentArchived := newLimitOrder(true, 4_900_000, 1, order.StandingTiF, snapNowOffset())
	mustStoreOrder(t, recentArchived, order.OrderStatusExecuted)
	oldArchived := newLimitOrder(true, 5_000_000, 1, order.StandingTiF, 0)
	mustStoreOrder(t, oldArchived, order.OrderStatusExecuted)

	const hourMs = uint64(60 * 60 * 1000)
	wantCandles := []*candles.Candle{
		{EndStamp: 3 * hourMs, MatchVolume: 1, QuoteVolume: 1, HighRate: 5, LowRate: 4, StartRate: 4, EndRate: 5},
		{EndStamp: 4 * hourMs, MatchVolume: 2, QuoteVolume: 2, HighRate: 6, LowRate: 5, StartRate: 5, EndRate: 6},
	}
	if err := archie.InsertCandles(lo.Base(), lo.Quote(), hourMs, wantCandles); err != nil {
		t.Fatalf("InsertCandles: %v", err)
	}

	points := archie.tables.points
	var pointID int64
	if err := archie.db.QueryRow(fmt.Sprintf(
		"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1) RETURNING id",
		points), []byte{0xaa}, []byte{0xbb}).Scan(&pointID); err != nil {
		t.Fatalf("insert point: %v", err)
	}

	appendSnapEvent(t, ctx, []byte("e1"))
	wantFrontier := appendSnapEvent(t, ctx, []byte("e2"))

	loadFrontier := reloadFromSnapshot(t, ctx)
	if loadFrontier.Seq != wantFrontier.Seq || !bytes.Equal(loadFrontier.TipHash, wantFrontier.TipHash) {
		t.Fatalf("loaded frontier = %s, want seq=%d hash=%x",
			loadFrontier, wantFrontier.Seq, wantFrontier.TipHash)
	}

	mustOrderStatus(t, lo, order.OrderStatusBooked)
	mustOrderStatus(t, recentArchived, order.OrderStatusExecuted)
	mustOrderUnknown(t, oldArchived)

	candlesTable := fmt.Sprintf("%s.%s_1h", marketSchema(mktInfo.Name), candlesTableName)
	var candleCount int
	if err := archie.db.QueryRow("SELECT count(*) FROM " + candlesTable).Scan(&candleCount); err != nil {
		t.Fatalf("count candles: %v", err)
	}
	if candleCount != len(wantCandles) {
		t.Fatalf("restored %d candles, want %d", candleCount, len(wantCandles))
	}

	var rows int
	var anchorSeq int64
	var anchorKind string
	if err := archie.db.QueryRow(fmt.Sprintf(
		"SELECT count(*), coalesce(max(seq),0), coalesce(max(kind),'') FROM %s",
		archie.tables.eventLog)).Scan(&rows, &anchorSeq, &anchorKind); err != nil {
		t.Fatalf("count event log: %v", err)
	}
	if rows != 1 || uint64(anchorSeq) != wantFrontier.Seq {
		t.Fatalf("event log after load: %d rows, anchor seq %d; want 1 row at seq %d",
			rows, anchorSeq, wantFrontier.Seq)
	}
	if anchorKind != db.SnapshotAnchorKind {
		t.Fatalf("anchor kind = %q, want %q", anchorKind, db.SnapshotAnchorKind)
	}

	next := appendSnapEvent(t, ctx, []byte("e3"))
	if next.Seq != wantFrontier.Seq+1 {
		t.Fatalf("next event seq = %d, want %d", next.Seq, wantFrontier.Seq+1)
	}

	var nextPointID int64
	if err := archie.db.QueryRow(fmt.Sprintf(
		"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1) RETURNING id",
		points), []byte{0xcc}, []byte{0xdd}).Scan(&nextPointID); err != nil {
		t.Fatalf("post-snapshot point insert collided: %v", err)
	}
	if nextPointID <= pointID {
		t.Fatalf("post-snapshot point id = %d, want > %d", nextPointID, pointID)
	}
}

// TestSnapshotCarriesActiveMatchOrders: archived legs of active matches must load
// so restoreActiveSwaps can rebuild trackers (omit → catch-up livelock).
func TestSnapshotCarriesActiveMatchOrders(t *testing.T) {
	ctx := context.Background()
	mustCleanTables(t)

	maker := newLimitOrder(false, 4_500_000, 1, order.StandingTiF, 0)
	taker := newLimitOrder(true, 4_490_000, 1, order.ImmediateTiF, 10)
	mustStoreOrder(t, maker, order.OrderStatusBooked)
	mustStoreOrder(t, taker, order.OrderStatusExecuted)
	bystander := newLimitOrder(true, 4_700_000, 1, order.ImmediateTiF, 20)
	mustStoreOrder(t, bystander, order.OrderStatusExecuted)

	match := insertTestMatch(t, maker, taker, uint64(snapTestEpochIdx), false)
	reloadFromSnapshot(t, ctx)

	swaps, err := archie.ActiveSwaps()
	if err != nil {
		t.Fatalf("ActiveSwaps after load: %v", err)
	}
	if len(swaps) != 1 || swaps[0].MatchData.ID != match.ID() {
		t.Fatalf("ActiveSwaps after load = %d rows, want the one active match %v", len(swaps), match.ID())
	}

	mustOrderStatus(t, maker, order.OrderStatusBooked)
	mustOrderStatus(t, taker, order.OrderStatusExecuted)
	mustOrderUnknown(t, bystander)
}

// TestSnapshotCarriesRecentInactiveMatchOrders: recent match legs with old server_time.
func TestSnapshotCarriesRecentInactiveMatchOrders(t *testing.T) {
	ctx := context.Background()
	mustCleanTables(t)

	// Legs use default 2019 server_time (outside the 30-day window); only match
	// membership should pull them.
	maker := newLimitOrder(false, 4_500_000, 1, order.StandingTiF, 0)
	taker := newLimitOrder(true, 4_490_000, 1, order.ImmediateTiF, 10)
	mustStoreOrder(t, maker, order.OrderStatusExecuted)
	mustStoreOrder(t, taker, order.OrderStatusExecuted)

	epochDur := int64(EpochDuration)
	recentEpochIdx := uint64(time.Now().UnixMilli() / epochDur)
	insertTestMatch(t, maker, taker, recentEpochIdx, true)

	// Old inactive match outside the window must not force its legs in.
	oldMaker := newLimitOrder(false, 4_600_000, 1, order.StandingTiF, 20)
	oldTaker := newLimitOrder(true, 4_590_000, 1, order.ImmediateTiF, 30)
	if err := storeOrderForTest(archie, oldMaker, 1, epochDur, order.OrderStatusExecuted); err != nil {
		t.Fatalf("StoreOrder(oldMaker): %v", err)
	}
	if err := storeOrderForTest(archie, oldTaker, 1, epochDur, order.OrderStatusExecuted); err != nil {
		t.Fatalf("StoreOrder(oldTaker): %v", err)
	}
	oldHorizonMs := time.Now().UnixMilli() - int64(snapshotHistoryDays+5)*24*int64(time.Hour/time.Millisecond)
	oldEpochIdx := oldHorizonMs / epochDur
	if oldEpochIdx < 1 {
		oldEpochIdx = 1
	}
	insertTestMatch(t, oldMaker, oldTaker, uint64(oldEpochIdx), true)

	reloadFromSnapshot(t, ctx)

	mustOrderStatus(t, maker, order.OrderStatusExecuted)
	mustOrderStatus(t, taker, order.OrderStatusExecuted)
	mustOrderUnknown(t, oldMaker)
	mustOrderUnknown(t, oldTaker)
}

// TestSnapshotCarriesArchivedCancelTargets: recent cancel pulls old target order.
func TestSnapshotCarriesArchivedCancelTargets(t *testing.T) {
	ctx := context.Background()
	mustCleanTables(t)

	// Target booked in 2019 (outside server_time window), canceled recently.
	target := newLimitOrder(false, 4_500_000, 1, order.StandingTiF, 0)
	mustStoreOrder(t, target, order.OrderStatusCanceled)
	cancel := newCancelOrder(target.ID(), AssetDCR, AssetBTC, snapNowOffset())
	cancel.AccountID = target.AccountID
	mustStoreOrder(t, cancel, order.OrderStatusExecuted)

	bystander := newLimitOrder(true, 4_700_000, 1, order.ImmediateTiF, 20)
	mustStoreOrder(t, bystander, order.OrderStatusExecuted)

	reloadFromSnapshot(t, ctx)

	mustOrderStatus(t, target, order.OrderStatusCanceled)
	got, status, err := archie.Order(cancel.ID(), cancel.Base(), cancel.Quote())
	if err != nil {
		t.Fatalf("Order(cancel): %v", err)
	}
	if status != order.OrderStatusExecuted {
		t.Fatalf("Order(cancel) status = %v, want executed", status)
	}
	co, ok := got.(*order.CancelOrder)
	if !ok || co.TargetOrderID != target.ID() {
		t.Fatalf("cancel target after load = %v, want %v", got, target.ID())
	}
	mustOrderUnknown(t, bystander)
}
