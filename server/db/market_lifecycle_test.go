// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"reflect"
	"testing"

	"decred.org/dcrdex/server/meshevents"
)

func testRunParams() meshevents.MarketRunParams {
	return meshevents.MarketRunParams{
		LotSize:                100_000,
		RateStep:               1000,
		ParcelSize:             10,
		MaxUserCancelsPerEpoch: 2,
		MinimumRate:            500,
	}
}

func TestProjectMarketStartedLifecycle(t *testing.T) {
	persist := true
	const market = "dcr_btc"
	const dur int64 = 10_000
	params := testRunParams()
	update := &MarketStartedUpdate{Market: market, CurrentEpochIdx: 25, EpochDur: dur, RunParams: params}
	running := MarketLifecycle{
		Market: market, State: MarketStateRunning, StartEpochIdx: 10, StartEpochDur: dur,
		ActiveEpochIdx: 19, ProcessedEpochIdx: 18, RunParams: params,
	}
	running.RunParams.LotSize *= 2 // Startup may change the trading parameters.
	pending := running
	pending.FinalEpochIdx, pending.FinalEpochDur = 25, dur
	pending.PendingAction = MarketPendingSuspend
	pending.PendingEpochIdx, pending.PendingEpochDur = 25, dur
	pending.PersistBook = &persist
	pastFinal := pending
	pastFinal.FinalEpochIdx, pastFinal.PendingEpochIdx = 20, 20
	pastFinal.RunParams = params
	draining := MarketLifecycle{
		Market: market, State: MarketStateDraining, StartEpochIdx: 10, StartEpochDur: dur,
		FinalEpochIdx: 20, FinalEpochDur: dur, PersistBook: &persist,
		ProcessedEpochIdx: 19, RunParams: params,
	}
	wantRunning := MarketLifecycle{
		Market: market, State: MarketStateRunning, StartEpochIdx: 25, StartEpochDur: dur,
		ActiveEpochIdx: 25, ProcessedEpochIdx: 24, RunParams: params,
	}
	wantPending := pending
	wantPending.ActiveEpochIdx, wantPending.ProcessedEpochIdx = 25, 24
	wantPending.RunParams = params
	wantDraining := draining
	wantDraining.ProcessedEpochIdx = 20

	for _, tt := range []struct {
		name    string
		prev    *MarketLifecycle
		want    MarketLifecycle
		changed bool
	}{
		{"first start", nil, wantRunning, true},
		{"running restart", &running, wantRunning, true},
		{"restart in final epoch keeps suspension", &pending, wantPending, true},
		{"restart after final epoch starts draining", &pastFinal, wantDraining, true},
		{"draining restart resolves abandoned epochs", &draining, wantDraining, true},
		{"draining restart with no abandoned epochs", &wantDraining, wantDraining, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			next, changed, err := ProjectMarketStartedLifecycle(tt.prev, update)
			if err != nil {
				t.Fatal(err)
			}
			if next == nil || !reflect.DeepEqual(*next, tt.want) || changed != tt.changed {
				t.Fatalf("got lifecycle %+v, changed %v; want %+v, changed %v", next, changed, tt.want, tt.changed)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		prev   MarketLifecycle
		mutate func(*MarketStartedUpdate)
	}{
		{"pending suspension rejects duration change", pending, func(u *MarketStartedUpdate) { u.EpochDur *= 2 }},
		{"draining rejects duration change", draining, func(u *MarketStartedUpdate) { u.EpochDur *= 2 }},
		{"draining rejects parameter change", wantDraining, func(u *MarketStartedUpdate) { u.RunParams.LotSize *= 2 }},
		{"passed suspension rejects parameter change", pastFinal, func(u *MarketStartedUpdate) { u.RunParams.LotSize *= 2 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := *update
			tt.mutate(&u)
			if _, _, err := ProjectMarketStartedLifecycle(&tt.prev, &u); err == nil {
				t.Fatal("expected invalid startup parameters to be rejected")
			}
		})
	}
	t.Run("rejects suspended", func(t *testing.T) {
		suspended := wantDraining
		suspended.State = MarketStateSuspended
		if _, _, err := ProjectMarketStartedLifecycle(&suspended, update); err == nil {
			t.Fatal("expected suspended market startup to be rejected")
		}
	})
}

func TestProjectAdvanceEpochLifecycle(t *testing.T) {
	persist := true
	const market = "dcr_btc"
	const dur int64 = 10_000

	running := func(active int64) *MarketLifecycle {
		return &MarketLifecycle{
			Market:            market,
			State:             MarketStateRunning,
			StartEpochIdx:     10,
			StartEpochDur:     dur,
			PendingAction:     MarketPendingNone,
			ActiveEpochIdx:    active,
			ProcessedEpochIdx: active - 1,
		}
	}

	checkAdvance := func(t *testing.T, prev *MarketLifecycle, update *meshevents.AdvanceEpochEvent, want *MarketLifecycle) {
		t.Helper()
		before := *prev
		next, err := ProjectAdvanceEpochLifecycle(prev, update)
		if err != nil {
			t.Fatalf("ProjectAdvanceEpochLifecycle: %v", err)
		}
		if !reflect.DeepEqual(next, want) {
			t.Fatalf("lifecycle = %+v, want %+v", next, want)
		}
		if !reflect.DeepEqual(*prev, before) {
			t.Fatal("projection modified the previous lifecycle")
		}
	}

	for _, test := range []struct {
		name          string
		pendingFinal  int64
		active        int64
		opened        int64
		processingLag int64
	}{
		{"normal advance", 0, 15, 16, 1},
		{"pending suspension", 20, 15, 16, 1},
		{"final close", 20, 20, 0, 1},
		{"maximum processing lag", 0, 15, 16, MaxUnprocessedClosedEpochs},
	} {
		t.Run(test.name, func(t *testing.T) {
			prev := running(test.active)
			prev.ProcessedEpochIdx = test.active - test.processingLag
			if test.pendingFinal != 0 {
				prev.PendingAction = MarketPendingSuspend
				prev.PendingEpochIdx, prev.PendingEpochDur = test.pendingFinal, dur
				prev.FinalEpochIdx, prev.FinalEpochDur = test.pendingFinal, dur
				prev.PersistBook = &persist
			}
			want := *prev
			want.ActiveEpochIdx = test.opened
			if test.opened == 0 {
				want.State = MarketStateDraining
				want.PendingAction = MarketPendingNone
				want.PendingEpochIdx, want.PendingEpochDur = 0, 0
			}
			checkAdvance(t, prev, &meshevents.AdvanceEpochEvent{
				Market: market, ClosedEpochIdx: test.active, OpenedEpochIdx: test.opened, EpochDur: dur,
			}, &want)
		})
	}

	for _, test := range []struct {
		name   string
		change func(*MarketLifecycle, *meshevents.AdvanceEpochEvent)
	}{
		{"closed epoch mismatch", func(_ *MarketLifecycle, u *meshevents.AdvanceEpochEvent) {
			u.ClosedEpochIdx--
			u.OpenedEpochIdx--
		}},
		{"duration mismatch", func(_ *MarketLifecycle, u *meshevents.AdvanceEpochEvent) { u.EpochDur++ }},
		{"too many unprocessed epochs", func(lc *MarketLifecycle, _ *meshevents.AdvanceEpochEvent) {
			lc.ProcessedEpochIdx = lc.ActiveEpochIdx - MaxUnprocessedClosedEpochs - 1
		}},
		{"draining market", func(lc *MarketLifecycle, _ *meshevents.AdvanceEpochEvent) { lc.State = MarketStateDraining }},
		{"premature final close", func(_ *MarketLifecycle, u *meshevents.AdvanceEpochEvent) { u.OpenedEpochIdx = 0 }},
		{"advance past final epoch", func(lc *MarketLifecycle, _ *meshevents.AdvanceEpochEvent) {
			lc.PendingEpochIdx, lc.FinalEpochIdx = lc.ActiveEpochIdx, lc.ActiveEpochIdx
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			prev := running(15)
			prev.PendingAction = MarketPendingSuspend
			prev.PendingEpochIdx, prev.PendingEpochDur = 20, dur
			prev.FinalEpochIdx, prev.FinalEpochDur = 20, dur
			prev.PersistBook = &persist
			update := &meshevents.AdvanceEpochEvent{Market: market, ClosedEpochIdx: 15, OpenedEpochIdx: 16, EpochDur: dur}
			test.change(prev, update)
			if _, err := ProjectAdvanceEpochLifecycle(prev, update); err == nil {
				t.Fatal("expected advance to be rejected")
			}
		})
	}
}
