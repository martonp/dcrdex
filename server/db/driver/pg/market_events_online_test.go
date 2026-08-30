//go:build pgonline

package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

func TestApplyMarketStartedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	update := &db.MarketStartedUpdate{
		Market:          "dcr_btc",
		Base:            AssetDCR,
		Quote:           AssetBTC,
		CurrentEpochIdx: 12345,
		EpochDur:        int64(EpochDuration),
		RevocationTime:  time.UnixMilli(987654321).UTC(),
	}
	event := []byte("market-started-event")
	logEntry, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyMarketStartedEvent error: %v", err)
	}

	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindMarketStarted, event, update)
	requireEventApplyLog(t, logEntry, 1, meshevents.EventKindMarketStarted, event, tip, update)
	assertEventLogFrontier(t, 1, tip)
}

func TestApplyMarketStartedEventRevokesEpochOrdersWhenRunning(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochDur = int64(EpochDuration)
	currentEpochIdx := int64(20000)
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:        "dcr_btc",
		State:         db.MarketStateRunning,
		StartEpochIdx: currentEpochIdx - 10,
		StartEpochDur: epochDur,
		PendingAction: db.MarketPendingNone,
	})

	// A leftover epoch trade and a leftover epoch cancel from an epoch that
	// never processed. Every market_started disposes of both explicitly:
	// the trade is revoked penalty-free, the cancel fails like an unmatched
	// cancel while its booked target order stays booked.
	ord := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	ord.SetTime(time.UnixMilli((currentEpochIdx-5)*epochDur + 1))
	if err := storeOrderForTest(archie, ord, currentEpochIdx-5, epochDur, order.OrderStatusEpoch); err != nil {
		t.Fatalf("StoreOrder error: %v", err)
	}
	target := newLimitOrder(false, 4_950_000, 1, order.StandingTiF, 5)
	if err := storeOrderForTest(archie, target, currentEpochIdx-8, epochDur, order.OrderStatusBooked); err != nil {
		t.Fatalf("StoreOrder target error: %v", err)
	}
	cancel := newCancelOrder(target.ID(), AssetDCR, AssetBTC, 6)
	if err := archie.storeOrder(archie.db, cancel, currentEpochIdx-5, epochDur, 3, orderStatusEpoch); err != nil {
		t.Fatalf("storeOrder cancel error: %v", err)
	}

	// A revoke set that does not cover the active epoch orders is rejected and
	// rolls back, so no startup can silently finalize (or preserve) them.
	update := &db.MarketStartedUpdate{
		Market:          "dcr_btc",
		Base:            AssetDCR,
		Quote:           AssetBTC,
		CurrentEpochIdx: currentEpochIdx,
		EpochDur:        epochDur,
		RevocationTime:  time.UnixMilli(987654321).UTC(),
	}
	if _, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("ms-missing-revokes")}, update); err == nil {
		t.Fatalf("ApplyMarketStartedEvent without epoch revokes succeeded with active epoch orders")
	}
	if _, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote()); err != nil || status != order.OrderStatusEpoch {
		t.Fatalf("order status after rollback = %v (err %v), want %v", status, err, order.OrderStatusEpoch)
	}
	assertEventLogFrontier(t, 0, nil)

	update.EpochRevokes = []*db.StartupOrderRevoke{
		{Order: ord, Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned},
		{Order: cancel, Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned},
	}
	if _, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("ms-epoch-revokes-running")}, update); err != nil {
		t.Fatalf("ApplyMarketStartedEvent error: %v", err)
	}

	_, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote())
	if err != nil {
		t.Fatalf("Order error: %v", err)
	}
	if status != order.OrderStatusRevoked {
		t.Fatalf("epoch trade status = %v, want %v", status, order.OrderStatusRevoked)
	}
	_, status, err = archie.Order(cancel.ID(), cancel.Base(), cancel.Quote())
	if err != nil {
		t.Fatalf("Order cancel error: %v", err)
	}
	if status != order.OrderStatusExecuted { // failed reports as executed for cancels
		t.Fatalf("epoch cancel status = %v, want %v", status, order.OrderStatusExecuted)
	}
	if pgStatus, _, _, err := archie.orderStatusByID(cancel.ID(), cancel.Base(), cancel.Quote()); err != nil || pgStatus != orderStatusFailed {
		t.Fatalf("epoch cancel pg status = %v (err %v), want %v", pgStatus, err, orderStatusFailed)
	}
	// The cancel's target order stays booked.
	if _, status, err = archie.Order(target.ID(), target.Base(), target.Quote()); err != nil || status != order.OrderStatusBooked {
		t.Fatalf("cancel target status = %v (err %v), want %v", status, err, order.OrderStatusBooked)
	}
	// The dispositions are penalty-free: no reputation order outcomes.
	for _, user := range []struct {
		label string
		ord   order.Order
	}{{"trade", ord}, {"cancel", cancel}} {
		_, _, outcomes, err := archie.GetUserReputationData(ctx, user.ord.User(), 10, 10, 10)
		if err != nil {
			t.Fatalf("GetUserReputationData %s error: %v", user.label, err)
		}
		if len(outcomes) != 0 {
			t.Fatalf("%s owner order outcomes = %+v, want none", user.label, outcomes)
		}
	}
}

func TestApplyMarketStartedEventEpochRevokes(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochDur = int64(EpochDuration)
	finalEpochIdx := int64(30000)
	persistBook := true
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:          "dcr_btc",
		State:           db.MarketStateRunning,
		StartEpochIdx:   finalEpochIdx - 5,
		StartEpochDur:   epochDur,
		FinalEpochIdx:   finalEpochIdx,
		FinalEpochDur:   epochDur,
		PendingAction:   db.MarketPendingSuspend,
		PendingEpochIdx: finalEpochIdx,
		PendingEpochDur: epochDur,
		PersistBook:     &persistBook,
	})

	ord := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, 0)
	ord.SetTime(time.UnixMilli(finalEpochIdx*epochDur + 1))
	if err := storeOrderForTest(archie, ord, finalEpochIdx, epochDur, order.OrderStatusEpoch); err != nil {
		t.Fatalf("StoreOrder error: %v", err)
	}

	// The pending suspend's final epoch ended while the market was down, so
	// market_started enters the drain and revokes the leftover epoch order.
	revocationTime := time.UnixMilli(987654321).UTC()
	update := &db.MarketStartedUpdate{
		Market:          "dcr_btc",
		Base:            AssetDCR,
		Quote:           AssetBTC,
		CurrentEpochIdx: finalEpochIdx + 3,
		EpochDur:        epochDur,
		RevocationTime:  revocationTime,
		EpochRevokes: []*db.StartupOrderRevoke{{
			Order:  ord,
			Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned,
		}},
	}
	if _, err := archie.ApplyMarketStartedEvent(ctx, &db.EventLogMeta{Event: []byte("ms-epoch-revokes")}, update); err != nil {
		t.Fatalf("ApplyMarketStartedEvent error: %v", err)
	}

	_, status, err := archie.Order(ord.ID(), ord.Base(), ord.Quote())
	if err != nil {
		t.Fatalf("Order error: %v", err)
	}
	if status != order.OrderStatusRevoked {
		t.Fatalf("epoch revoke status = %v, want %v", status, order.OrderStatusRevoked)
	}
	lc, err := archie.MarketLifecycle(update.Market)
	if err != nil {
		t.Fatalf("MarketLifecycle error: %v", err)
	}
	if lc.PendingAction != db.MarketPendingSuspendDrain {
		t.Fatalf("lifecycle pending action = %v, want drain", lc.PendingAction)
	}
}

func TestApplyEpochProcessedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	const epochIdx, epochDur int64 = 13245678, 6000
	seedMarketLifecycle(t, &db.MarketLifecycle{
		Market:        "dcr_btc",
		State:         db.MarketStateRunning,
		StartEpochIdx: epochIdx,
		StartEpochDur: epochDur,
		PendingAction: db.MarketPendingNone,
		// Both test epochs are closed, neither processed yet.
		ActiveEpochIdx:    epochIdx + 2,
		ProcessedEpochIdx: epochIdx - 1,
	})

	storeEpochOrder := func(label string, ord order.Order, idx int64, status order.OrderStatus) {
		t.Helper()

		if err := storeOrderForTest(archie, ord, idx, epochDur, status); err != nil {
			t.Fatalf("StoreOrder %s %v error: %v", label, ord.ID(), err)
		}
	}

	storeEpochCancel := func(label string, cancel *order.CancelOrder, idx int64, gap int32) {
		t.Helper()

		if err := archie.storeOrder(archie.db, cancel, idx, epochDur, gap, orderStatusEpoch); err != nil {
			t.Fatalf("storeOrder (epoch) %s error: %v", label, err)
		}
	}

	newStoredMatch := func(epochIdx int64, timeOffset int64) *order.Match {
		t.Helper()

		maker := newLimitOrder(false, 4_900_000, 1, order.StandingTiF, timeOffset)
		taker := newLimitOrder(true, 4_800_000, 1, order.ImmediateTiF, timeOffset+1)
		for _, ord := range []order.Order{maker, taker} {
			storeEpochOrder("match", ord, epochIdx, order.OrderStatusExecuted)
		}
		return newMatch(maker, taker, taker.Quantity, order.EpochID{
			Idx: uint64(epochIdx),
			Dur: uint64(epochDur),
		})
	}

	requireOrderStatus := func(ord order.Order, want order.OrderStatus) int64 {
		t.Helper()

		status, _, filled, err := archie.OrderStatus(ord)
		if err != nil || status != want {
			t.Fatalf("order %v status = %v, err = %v, want %v", ord.ID(), status, err, want)
		}
		return filled
	}

	requireOrderFilled := func(ord order.Order, want int64) {
		t.Helper()

		if filled := requireOrderStatus(ord, order.OrderStatusBooked); filled != want {
			t.Fatalf("order %v filled = %d, want %d", ord.ID(), filled, want)
		}
	}

	requireActiveMatch := func(match *order.Match) {
		t.Helper()

		storedMatch, err := archie.MatchByID(match.ID(), match.Maker.Base(), match.Maker.Quote())
		if err != nil {
			t.Fatalf("MatchByID error: %v", err)
		}
		if storedMatch.Status != order.NewlyMatched || !storedMatch.Active {
			t.Fatalf("stored match status/active = %v/%v, want newly matched/active", storedMatch.Status, storedMatch.Active)
		}
		if storedMatch.ID != match.ID() || storedMatch.Taker != match.Taker.ID() || storedMatch.Maker != match.Maker.ID() {
			t.Fatalf("stored match ids = %v/%v/%v, want %v/%v/%v",
				storedMatch.ID, storedMatch.Taker, storedMatch.Maker, match.ID(), match.Taker.ID(), match.Maker.ID())
		}
		if storedMatch.Quantity != match.Quantity || storedMatch.Rate != match.Rate {
			t.Fatalf("stored match quantity/rate = %d/%d, want %d/%d",
				storedMatch.Quantity, storedMatch.Rate, match.Quantity, match.Rate)
		}
		if storedMatch.TakerSell != match.Taker.Trade().Sell {
			t.Fatalf("stored match taker sell = %t, want %t", storedMatch.TakerSell, match.Taker.Trade().Sell)
		}
		if storedMatch.BaseRate != match.FeeRateBase || storedMatch.QuoteRate != match.FeeRateQuote {
			t.Fatalf("stored match fee rates = %d/%d, want %d/%d",
				storedMatch.BaseRate, storedMatch.QuoteRate, match.FeeRateBase, match.FeeRateQuote)
		}
	}

	requireCancelMatch := func(match *order.Match) {
		t.Helper()

		storedMatch, err := archie.MatchByID(match.ID(), match.Maker.Base(), match.Maker.Quote())
		if err != nil {
			t.Fatalf("MatchByID cancel error: %v", err)
		}
		if storedMatch.Status != order.MatchComplete || storedMatch.Active {
			t.Fatalf("stored cancel match status/active = %v/%v, want complete/inactive", storedMatch.Status, storedMatch.Active)
		}
		if storedMatch.Maker != match.Maker.ID() || storedMatch.Taker != match.Taker.ID() {
			t.Fatalf("stored cancel match maker/taker = %v/%v, want %v/%v",
				storedMatch.Maker, storedMatch.Taker, match.Maker.ID(), match.Taker.ID())
		}
		if storedMatch.BaseRate != 0 || storedMatch.QuoteRate != 0 || storedMatch.TakerSell {
			t.Fatalf("stored cancel swap fields = baseRate %d quoteRate %d takerSell %t, want zero/zero/false",
				storedMatch.BaseRate, storedMatch.QuoteRate, storedMatch.TakerSell)
		}
	}

	requireMissingMatch := func(match *order.Match) {
		t.Helper()

		if _, err := archie.MatchByID(match.ID(), match.Maker.Base(), match.Maker.Quote()); err == nil {
			t.Fatalf("match %v was inserted despite event-log divergence", match.ID())
		}
	}

	requireOrderOutcome := func(label string, ord order.Order, wantCanceled bool) {
		t.Helper()

		_, _, ords, err := archie.GetUserReputationData(ctx, ord.User(), 10, 10, 10)
		if err != nil {
			t.Fatalf("GetUserReputationData %s error: %v", label, err)
		}
		if len(ords) != 1 || ords[0].Canceled != wantCanceled {
			t.Fatalf("%s order outcomes = %+v, want one outcome with canceled=%t", label, ords, wantCanceled)
		}
	}

	requireNoOrderOutcomes := func(label string, ord order.Order) {
		t.Helper()

		_, _, ords, err := archie.GetUserReputationData(ctx, ord.User(), 10, 10, 10)
		if err != nil {
			t.Fatalf("GetUserReputationData %s error: %v", label, err)
		}
		if len(ords) != 0 {
			t.Fatalf("%s order outcomes = %+v, want none", label, ords)
		}
	}

	requireLastRate := func(label string, base, quote uint32, want uint64) uint64 {
		t.Helper()

		rate, err := archie.LastEpochRate(base, quote)
		if err != nil {
			t.Fatalf("LastEpochRate %s error: %v", label, err)
		}
		if rate != want {
			t.Fatalf("last rate %s = %d, want %d", label, rate, want)
		}
		return rate
	}

	booked := newLimitOrder(false, 5_000_000, 1, order.StandingTiF, 10)
	storeEpochOrder("booked", booked, epochIdx, order.OrderStatusEpoch)
	partial := newLimitOrder(false, 5_050_000, 3, order.StandingTiF, 13)
	storeEpochOrder("partial", partial, epochIdx, order.OrderStatusBooked)
	partial.Trade().SetFill(LotSize)
	cancelTarget := newLimitOrder(false, 5_025_000, 1, order.StandingTiF, 14)
	storeEpochOrder("cancel target", cancelTarget, epochIdx, order.OrderStatusBooked)
	fastCancel := newCancelOrder(booked.ID(), AssetDCR, AssetBTC, 11)
	storeEpochCancel("fast cancel", fastCancel, epochIdx, 1)
	freeCancel := newCancelOrder(booked.ID(), AssetDCR, AssetBTC, 12)
	storeEpochCancel("free cancel", freeCancel, epochIdx, 2)
	matchedCancel := newCancelOrder(cancelTarget.ID(), AssetDCR, AssetBTC, 15)
	storeEpochCancel("matched cancel", matchedCancel, epochIdx, 1)
	match := newStoredMatch(epochIdx, 20)
	cancelMatch := newMatch(cancelTarget, matchedCancel, 0, order.EpochID{
		Idx: uint64(epochIdx),
		Dur: uint64(epochDur),
	})
	update := &db.EpochProcessedUpdate{
		Epoch: &db.EpochResults{
			MktBase:        AssetDCR,
			MktQuote:       AssetBTC,
			Idx:            epochIdx,
			Dur:            epochDur,
			MatchTime:      time.UnixMilli(epochIdx*epochDur + 1).UTC().UnixMilli(),
			CSum:           []byte{0x01, 0x02},
			Seed:           []byte{0x03, 0x04},
			OrdersRevealed: []order.OrderID{booked.ID()},
			MatchVolume:    match.Quantity,
			QuoteVolume:    match.Quantity * match.Rate,
			HighRate:       5_000_000,
			LowRate:        4_800_000,
			StartRate:      4_900_000,
			EndRate:        4_950_000,
		},
		TradesBooked:    []*order.LimitOrder{booked},
		TradesPartial:   []*order.LimitOrder{partial},
		TradesCanceled:  []*order.LimitOrder{cancelTarget},
		CancelsExecuted: []*order.CancelOrder{fastCancel, freeCancel, matchedCancel},
		Matches:         []*order.Match{match, cancelMatch},
	}

	// The event transaction applies all epoch storage effects and appends one
	// authoritative log row.
	event := []byte("epoch-processed-event")
	policy := &db.ReputationOutcomePolicy{OrderLimit: 10, FreeCancelThreshold: 2}
	logEntry, err := archie.ApplyEpochProcessedEvent(ctx, &db.EventLogMeta{Event: event}, policy, update)
	if err != nil {
		t.Fatalf("ApplyEpochProcessedEvent error: %v", err)
	}
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindEpochProcessed, event, update)
	requireEventApplyLog(t, logEntry, 1, meshevents.EventKindEpochProcessed, event, tip, update)
	assertEventLogFrontier(t, 1, tip)

	requireOrderStatus(booked, order.OrderStatusBooked)
	requireOrderFilled(partial, int64(LotSize))
	requireOrderStatus(cancelTarget, order.OrderStatusCanceled)
	requireOrderStatus(matchedCancel, order.OrderStatusExecuted)
	requireActiveMatch(match)
	requireCancelMatch(cancelMatch)
	requireOrderOutcome("fast cancel", fastCancel, true)
	requireOrderOutcome("free cancel", freeCancel, false)
	requireOrderOutcome("matched cancel", matchedCancel, true)
	lastRate := requireLastRate("after apply", booked.Base(), booked.Quote(), update.Epoch.EndRate)

	// A divergence after the DB work has been staged must roll back the epoch
	// row, order moves, partial fill, match insert, reputation outcome, and log
	// append.
	rollbackOrder := newLimitOrder(false, 5_100_000, 1, order.StandingTiF, 30)
	storeEpochOrder("rollback order", rollbackOrder, epochIdx+1, order.OrderStatusEpoch)
	rollbackPartial := newLimitOrder(false, 5_110_000, 3, order.StandingTiF, 31)
	storeEpochOrder("rollback partial", rollbackPartial, epochIdx+1, order.OrderStatusBooked)
	rollbackPartial.Trade().SetFill(LotSize)
	rollbackCancelTarget := newLimitOrder(false, 5_120_000, 1, order.StandingTiF, 32)
	storeEpochOrder("rollback cancel target", rollbackCancelTarget, epochIdx+1, order.OrderStatusBooked)
	rollbackMatch := newStoredMatch(epochIdx+1, 40)
	rollbackCancel := newCancelOrder(rollbackOrder.ID(), AssetDCR, AssetBTC, 31)
	storeEpochCancel("rollback cancel", rollbackCancel, epochIdx+1, 1)
	rollbackMatchedCancel := newCancelOrder(rollbackCancelTarget.ID(), AssetDCR, AssetBTC, 33)
	storeEpochCancel("rollback matched cancel", rollbackMatchedCancel, epochIdx+1, 1)
	rollbackCancelMatch := newMatch(rollbackCancelTarget, rollbackMatchedCancel, 0, order.EpochID{
		Idx: uint64(epochIdx + 1),
		Dur: uint64(epochDur),
	})
	rollbackUpdate := &db.EpochProcessedUpdate{
		Epoch: &db.EpochResults{
			MktBase:   AssetDCR,
			MktQuote:  AssetBTC,
			Idx:       epochIdx + 1,
			Dur:       epochDur,
			MatchTime: time.UnixMilli((epochIdx+1)*epochDur + 1).UTC().UnixMilli(),
			CSum:      []byte{0x05},
			Seed:      []byte{0x06},
			EndRate:   5_200_000,
		},
		TradesBooked:    []*order.LimitOrder{rollbackOrder},
		TradesPartial:   []*order.LimitOrder{rollbackPartial},
		TradesCanceled:  []*order.LimitOrder{rollbackCancelTarget},
		CancelsExecuted: []*order.CancelOrder{rollbackCancel, rollbackMatchedCancel},
		Matches:         []*order.Match{rollbackMatch, rollbackCancelMatch},
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
	assertEventLogFrontier(t, 1, tip)
	requireOrderStatus(rollbackOrder, order.OrderStatusEpoch)
	requireOrderFilled(rollbackPartial, 0)
	requireOrderStatus(rollbackCancelTarget, order.OrderStatusBooked)
	requireOrderStatus(rollbackMatchedCancel, order.OrderStatusEpoch)
	requireMissingMatch(rollbackMatch)
	requireMissingMatch(rollbackCancelMatch)
	requireNoOrderOutcomes("rollback cancel", rollbackCancel)
	requireNoOrderOutcomes("rollback matched cancel", rollbackMatchedCancel)
	requireLastRate("after rollback", rollbackOrder.Base(), rollbackOrder.Quote(), lastRate)
}
