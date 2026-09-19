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
