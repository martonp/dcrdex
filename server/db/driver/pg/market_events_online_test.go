//go:build pgonline

package pg

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

func seedMarketLifecycle(t *testing.T, lc *db.MarketLifecycle) {
	t.Helper()
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

func testMarketRunParams() meshevents.MarketRunParams {
	return meshevents.MarketRunParams{
		LotSize: LotSize, RateStep: RateStep, ParcelSize: 10,
		MaxUserCancelsPerEpoch: math.MaxUint32,
	}
}

func TestApplyMarketStartedEvent(t *testing.T) {
	ctx := context.Background()
	const epochDur = int64(EpochDuration)

	checkLifecycle := func(t *testing.T, want *db.MarketLifecycle) {
		t.Helper()
		got, err := archie.MarketLifecycle(want.Market)
		if err != nil {
			t.Fatalf("MarketLifecycle: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("lifecycle = %+v, want %+v", got, want)
		}
	}

	checkResult := func(t *testing.T, result *db.MarketStartedApplyResult) {
		t.Helper()
		if result == nil || result.Log == nil || result.Lifecycle == nil {
			t.Fatalf("incomplete market started result: %+v", result)
		}
		checkLifecycle(t, result.Lifecycle)
	}

	t.Run("first start", func(t *testing.T) {
		if err := cleanTables(archie.db); err != nil {
			t.Fatalf("cleanTables: %v", err)
		}
		const epochIdx = int64(12345)
		update := &db.MarketStartedUpdate{
			Market: "dcr_btc", Base: AssetDCR, Quote: AssetBTC,
			CurrentEpochIdx: epochIdx, EpochDur: epochDur,
			RunParams: testMarketRunParams(), RevocationTime: time.UnixMilli(epochIdx * epochDur).UTC(),
		}
		event := []byte("market-started-event")
		result, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: event}, update)
		if err != nil {
			t.Fatalf("ApplyMarketStartedEvent: %v", err)
		}
		checkResult(t, result)
		tip := testEventApplyTip(t, nil, 1, meshevents.EventKindMarketStarted, event, update)
		requireEventApplyLog(t, result.Log, 1, meshevents.EventKindMarketStarted, event, tip, update)
		assertEventLogFrontier(t, ctx, 1, tip)
		checkLifecycle(t, &db.MarketLifecycle{
			Market: "dcr_btc", State: db.MarketStateRunning,
			StartEpochIdx: epochIdx, StartEpochDur: epochDur,
			ActiveEpochIdx: epochIdx, ProcessedEpochIdx: epochIdx - 1,
			RunParams: update.RunParams,
		})
	})

	t.Run("running restart", func(t *testing.T) {
		if err := cleanTables(archie.db); err != nil {
			t.Fatalf("cleanTables: %v", err)
		}
		const epochIdx = int64(20000)
		lifecycle := &db.MarketLifecycle{
			Market: "dcr_btc", State: db.MarketStateRunning,
			StartEpochIdx: epochIdx - 10, StartEpochDur: epochDur,
			ActiveEpochIdx: epochIdx - 5, ProcessedEpochIdx: epochIdx - 6,
			RunParams: testMarketRunParams(),
		}
		seedMarketLifecycle(t, lifecycle)

		booked := newLimitOrder(false, 4_900_000, 2, order.StandingTiF, 0)
		booked.SetTime(time.UnixMilli((epochIdx-8)*epochDur + 1))
		booked.AddFill(LotSize)
		target := newLimitOrder(false, 4_950_000, 1, order.StandingTiF, 0)
		target.SetTime(time.UnixMilli((epochIdx-8)*epochDur + 2))
		for _, ord := range []*order.LimitOrder{booked, target} {
			if err := storeOrderForTest(archie, ord, epochIdx-8, epochDur, order.OrderStatusBooked); err != nil {
				t.Fatalf("store booked order: %v", err)
			}
		}
		trade := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
		trade.SetTime(time.UnixMilli((epochIdx-5)*epochDur + 1))
		cancel := newCancelOrder(target.ID(), AssetDCR, AssetBTC, 0)
		cancel.AccountID = target.User()
		cancel.SetTime(time.UnixMilli((epochIdx-5)*epochDur + 2))
		for _, ord := range []order.Order{trade, cancel} {
			if err := storeOrderForTest(archie, ord, epochIdx-5, epochDur, order.OrderStatusEpoch); err != nil {
				t.Fatalf("store epoch order: %v", err)
			}
		}

		update := &db.MarketStartedUpdate{
			Market: "dcr_btc", Base: AssetDCR, Quote: AssetBTC,
			CurrentEpochIdx: epochIdx, EpochDur: epochDur,
			RunParams: lifecycle.RunParams, RevocationTime: time.UnixMilli(epochIdx * epochDur).UTC(),
			BookedRevokes: []*db.StartupOrderRevoke{{Order: booked, Reason: meshevents.StartupOrderRevokeReasonFundingCoinSpent}},
		}
		// Missing epoch revokes must roll back the lifecycle and booked revocation.
		if _, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("missing-revokes")}, update); err == nil {
			t.Fatal("startup succeeded without revoking the active epoch orders")
		}
		checkLifecycle(t, lifecycle)
		if _, status, err := archie.Order(booked.ID(), AssetDCR, AssetBTC); err != nil || status != order.OrderStatusBooked {
			t.Fatalf("booked order after rollback: status %v, error %v", status, err)
		}
		if _, status, err := archie.Order(trade.ID(), AssetDCR, AssetBTC); err != nil || status != order.OrderStatusEpoch {
			t.Fatalf("epoch order after rollback: status %v, error %v", status, err)
		}
		assertEventLogFrontier(t, ctx, 0, nil)

		update.EpochRevokes = []*db.StartupOrderRevoke{
			{Order: trade, Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned},
			{Order: cancel, Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned},
		}
		result, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("running-restart")}, update)
		if err != nil {
			t.Fatalf("ApplyMarketStartedEvent: %v", err)
		}
		checkResult(t, result)
		checkLifecycle(t, &db.MarketLifecycle{
			Market: "dcr_btc", State: db.MarketStateRunning,
			StartEpochIdx: epochIdx, StartEpochDur: epochDur,
			ActiveEpochIdx: epochIdx, ProcessedEpochIdx: epochIdx - 1,
			RunParams: update.RunParams,
		})
		for _, ord := range []*order.LimitOrder{booked, trade} {
			stored, status, err := archie.Order(ord.ID(), AssetDCR, AssetBTC)
			if err != nil || status != order.OrderStatusRevoked {
				t.Fatalf("revoked order %v: status %v, error %v", ord.ID(), status, err)
			}
			if stored.Trade().Filled() != ord.Filled() {
				t.Fatalf("revocation changed filled amount for %v", ord.ID())
			}
			// A server-generated cancel records each revocation, but is not counted.
			revocation := makePseudoCancel(ord.ID(), ord.User(), AssetDCR, AssetBTC, update.RevocationTime)
			if _, status, err := archie.Order(revocation.ID(), AssetDCR, AssetBTC); err != nil || status != order.OrderStatusRevoked {
				t.Fatalf("revocation cancel for %v: status %v, error %v", ord.ID(), status, err)
			}
			counted, err := archie.ExecutedCancelsForUser(ord.User(), 10)
			if err != nil || len(counted) != 0 {
				t.Fatalf("counted cancels for %v: %v, error %v", ord.ID(), counted, err)
			}
		}
		if status, _, _, err := archie.orderStatusByID(archie.db, cancel.ID(), AssetDCR, AssetBTC); err != nil || status != orderStatusFailed {
			t.Fatalf("abandoned cancel: status %v, error %v", status, err)
		}
		if _, status, err := archie.Order(target.ID(), AssetDCR, AssetBTC); err != nil || status != order.OrderStatusBooked {
			t.Fatalf("cancel target: status %v, error %v", status, err)
		}
		for _, ord := range []order.Order{booked, trade, cancel} {
			_, _, outcomes, err := archie.GetUserReputationData(ctx, ord.User(), 10, 10, 10)
			if err != nil || len(outcomes) != 0 {
				t.Fatalf("reputation outcomes for %v: %v, error %v", ord.ID(), outcomes, err)
			}
		}
	})

	t.Run("restart after final epoch", func(t *testing.T) {
		if err := cleanTables(archie.db); err != nil {
			t.Fatalf("cleanTables: %v", err)
		}
		const finalEpochIdx = int64(30000)
		persistBook := true
		lifecycle := &db.MarketLifecycle{
			Market: "dcr_btc", State: db.MarketStateRunning,
			StartEpochIdx: finalEpochIdx - 5, StartEpochDur: epochDur,
			FinalEpochIdx: finalEpochIdx, FinalEpochDur: epochDur,
			PendingAction:   db.MarketPendingSuspend,
			PendingEpochIdx: finalEpochIdx, PendingEpochDur: epochDur,
			PersistBook: &persistBook, RunParams: testMarketRunParams(),
		}
		seedMarketLifecycle(t, lifecycle)
		ord := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
		ord.SetTime(time.UnixMilli(finalEpochIdx*epochDur + 1))
		if err := storeOrderForTest(archie, ord, finalEpochIdx, epochDur, order.OrderStatusEpoch); err != nil {
			t.Fatalf("store epoch order: %v", err)
		}
		update := &db.MarketStartedUpdate{
			Market: "dcr_btc", Base: AssetDCR, Quote: AssetBTC,
			CurrentEpochIdx: finalEpochIdx + 3, EpochDur: epochDur,
			RunParams: lifecycle.RunParams, RevocationTime: time.UnixMilli((finalEpochIdx + 3) * epochDur).UTC(),
			EpochRevokes: []*db.StartupOrderRevoke{{Order: ord, Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned}},
		}
		result, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("draining-restart")}, update)
		if err != nil {
			t.Fatalf("ApplyMarketStartedEvent: %v", err)
		}
		checkResult(t, result)
		if _, status, err := archie.Order(ord.ID(), AssetDCR, AssetBTC); err != nil || status != order.OrderStatusRevoked {
			t.Fatalf("epoch order: status %v, error %v", status, err)
		}
		checkLifecycle(t, &db.MarketLifecycle{
			Market: "dcr_btc", State: db.MarketStateDraining,
			StartEpochIdx: lifecycle.StartEpochIdx, StartEpochDur: epochDur,
			FinalEpochIdx: finalEpochIdx, FinalEpochDur: epochDur,
			ProcessedEpochIdx: finalEpochIdx,
			PersistBook:       &persistBook, RunParams: lifecycle.RunParams,
		})
	})
}
