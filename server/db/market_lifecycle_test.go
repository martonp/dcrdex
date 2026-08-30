// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"testing"
	"time"

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

func TestProjectMarketLifecycle(t *testing.T) {
	persist := true
	const market = "dcr_btc"
	const dur int64 = 10_000

	running := &MarketLifecycle{
		Market:        market,
		State:         MarketStateRunning,
		StartEpochIdx: 10,
		StartEpochDur: dur,
		PendingAction: MarketPendingNone,
	}
	pendingSuspend := &MarketLifecycle{
		Market:          market,
		State:           MarketStateRunning,
		StartEpochIdx:   10,
		StartEpochDur:   dur,
		FinalEpochIdx:   20,
		FinalEpochDur:   dur,
		PendingAction:   MarketPendingSuspend,
		PendingEpochIdx: 20,
		PendingEpochDur: dur,
		PersistBook:     &persist,
	}
	draining := &MarketLifecycle{
		Market:            market,
		State:             MarketStateRunning,
		StartEpochIdx:     10,
		StartEpochDur:     dur,
		FinalEpochIdx:     20,
		FinalEpochDur:     dur,
		PendingAction:     MarketPendingSuspendDrain,
		PendingEpochIdx:   20,
		PendingEpochDur:   dur,
		PersistBook:       &persist,
		ProcessedEpochIdx: 20, // final epoch's close applied; suspend requires it
	}
	suspended := &MarketLifecycle{
		Market:        market,
		State:         MarketStateSuspended,
		StartEpochIdx: 10,
		StartEpochDur: dur,
		FinalEpochIdx: 20,
		FinalEpochDur: dur,
		PendingAction: MarketPendingNone,
		PersistBook:   &persist,
	}
	pendingResume := &MarketLifecycle{
		Market:          market,
		State:           MarketStateSuspended,
		StartEpochIdx:   30,
		StartEpochDur:   dur,
		PendingAction:   MarketPendingResume,
		PendingEpochIdx: 30,
		PendingEpochDur: dur,
		PersistBook:     &persist,
	}

	t.Run("schedule_suspend", func(t *testing.T) {
		next, err := ProjectMarketLifecycle(running, &MarketLifecycleUpdate{
			Action:      MarketLifecycleActionScheduleSuspend,
			Market:      market,
			EpochIdx:    20,
			EpochDur:    dur,
			PersistBook: &persist,
		})
		if err != nil {
			t.Fatalf("ProjectMarketLifecycle: %v", err)
		}
		if next.PendingAction != MarketPendingSuspend || next.PendingEpochIdx != 20 || next.PersistBook == nil || !*next.PersistBook {
			t.Fatalf("unexpected schedule_suspend projection: %+v", next)
		}
	})

	t.Run("suspend", func(t *testing.T) {
		next, err := ProjectMarketLifecycle(draining, &MarketLifecycleUpdate{
			Action:   MarketLifecycleActionSuspend,
			Market:   market,
			EpochIdx: 20,
			EpochDur: dur,
		})
		if err != nil {
			t.Fatalf("ProjectMarketLifecycle: %v", err)
		}
		if next.State != MarketStateSuspended || next.PendingAction != MarketPendingNone || next.PersistBook == nil || !*next.PersistBook {
			t.Fatalf("unexpected suspend projection: %+v", next)
		}
	})

	t.Run("schedule_resume", func(t *testing.T) {
		next, err := ProjectMarketLifecycle(suspended, &MarketLifecycleUpdate{
			Action:   MarketLifecycleActionScheduleResume,
			Market:   market,
			EpochIdx: 30,
			EpochDur: dur,
		})
		if err != nil {
			t.Fatalf("ProjectMarketLifecycle: %v", err)
		}
		if next.PendingAction != MarketPendingResume || next.PendingEpochIdx != 30 || next.FinalEpochIdx != 0 {
			t.Fatalf("unexpected schedule_resume projection: %+v", next)
		}
	})

	t.Run("resume", func(t *testing.T) {
		runParams := testRunParams()
		next, err := ProjectMarketLifecycle(pendingResume, &MarketLifecycleUpdate{
			Action:    MarketLifecycleActionResume,
			Market:    market,
			EpochIdx:  30,
			EpochDur:  dur,
			Timestamp: time.UnixMilli(30 * dur).UTC(),
			RunParams: &runParams,
		})
		if err != nil {
			t.Fatalf("ProjectMarketLifecycle: %v", err)
		}
		if next.State != MarketStateRunning || next.StartEpochIdx != 30 || next.PersistBook != nil {
			t.Fatalf("unexpected resume projection: %+v", next)
		}
		// No market_started follows a resume, so the resume row itself must
		// re-initialize the epoch cursors.
		if next.ActiveEpochIdx != 30 {
			t.Fatalf("resume active epoch cursor = %d, want 30", next.ActiveEpochIdx)
		}
		if next.ProcessedEpochIdx != 29 {
			t.Fatalf("resume closure cursor = %d, want 29", next.ProcessedEpochIdx)
		}
		// The resume re-pins the run parameters.
		if next.RunParams != runParams {
			t.Fatalf("resume run params = %+v, want %+v", next.RunParams, runParams)
		}
	})

	t.Run("resume requires run params", func(t *testing.T) {
		if _, err := ProjectMarketLifecycle(pendingResume, &MarketLifecycleUpdate{
			Action:    MarketLifecycleActionResume,
			Market:    market,
			EpochIdx:  30,
			EpochDur:  dur,
			Timestamp: time.UnixMilli(30 * dur).UTC(),
		}); err == nil {
			t.Fatal("expected resume without run params to fail")
		}
	})

	t.Run("suspend requires the final epoch processed", func(t *testing.T) {
		prev := *draining
		prev.ProcessedEpochIdx = 19
		if _, err := ProjectMarketLifecycle(&prev, &MarketLifecycleUpdate{
			Action:   MarketLifecycleActionSuspend,
			Market:   market,
			EpochIdx: 20,
			EpochDur: dur,
		}); err == nil {
			t.Fatal("expected suspend with unprocessed final epoch to fail")
		}
	})

	t.Run("suspend zeroes cursor", func(t *testing.T) {
		prev := *draining
		prev.ActiveEpochIdx = 21 // stale by construction; suspend must clear it
		next, err := ProjectMarketLifecycle(&prev, &MarketLifecycleUpdate{
			Action:   MarketLifecycleActionSuspend,
			Market:   market,
			EpochIdx: 20,
			EpochDur: dur,
		})
		if err != nil {
			t.Fatalf("ProjectMarketLifecycle: %v", err)
		}
		if next.ActiveEpochIdx != 0 {
			t.Fatalf("suspended active epoch cursor = %d, want 0", next.ActiveEpochIdx)
		}
	})

	t.Run("rejects bad transitions", func(t *testing.T) {
		if _, err := ProjectMarketLifecycle(pendingSuspend, &MarketLifecycleUpdate{
			Action:   MarketLifecycleActionSuspend,
			Market:   market,
			EpochIdx: 20,
			EpochDur: dur,
		}); err == nil {
			t.Fatal("expected suspend from pending-suspend to fail")
		}
		if _, err := ProjectMarketLifecycle(nil, &MarketLifecycleUpdate{
			Action: MarketLifecycleActionResume,
			Market: market,
		}); err == nil {
			t.Fatal("expected nil prev to fail")
		}
		if _, err := ProjectMarketLifecycle(running, nil); err == nil {
			t.Fatal("expected nil update to fail")
		}
	})
}

func TestProjectMarketStartedLifecycle(t *testing.T) {
	persist := true
	const market = "dcr_btc"
	const dur int64 = 10_000
	update := &MarketStartedUpdate{Market: market, CurrentEpochIdx: 25, EpochDur: dur, RunParams: testRunParams()}

	t.Run("nil prev inserts running row", func(t *testing.T) {
		next, changed, err := ProjectMarketStartedLifecycle(nil, update)
		if err != nil || !changed || next.StartEpochIdx != 25 || next.PendingAction != MarketPendingNone {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
		if next.ActiveEpochIdx != update.CurrentEpochIdx {
			t.Fatalf("active epoch cursor = %d, want %d", next.ActiveEpochIdx, update.CurrentEpochIdx)
		}
		if next.ProcessedEpochIdx != update.CurrentEpochIdx-1 {
			t.Fatalf("closure cursor = %d, want %d", next.ProcessedEpochIdx, update.CurrentEpochIdx-1)
		}
		if next.RunParams != update.RunParams {
			t.Fatalf("run params = %+v, want %+v", next.RunParams, update.RunParams)
		}
	})

	t.Run("pending suspend refuses a duration change", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspend,
			PendingEpochIdx: 20, PendingEpochDur: dur * 2, PersistBook: &persist,
			ActiveEpochIdx: 19, ProcessedEpochIdx: 18,
		}
		if _, _, err := ProjectMarketStartedLifecycle(prev, update); err == nil {
			t.Fatal("expected cross-duration restart with pending suspend to fail")
		}
	})

	t.Run("pending suspend past final drains", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspend,
			PendingEpochIdx: 20, PendingEpochDur: dur, PersistBook: &persist,
			ActiveEpochIdx: 19, ProcessedEpochIdx: 18, RunParams: testRunParams(),
		}
		next, changed, err := ProjectMarketStartedLifecycle(prev, update)
		if err != nil || !changed || next.PendingAction != MarketPendingSuspendDrain {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
		if next.ActiveEpochIdx != 0 {
			t.Fatalf("draining active epoch cursor = %d, want 0", next.ActiveEpochIdx)
		}
		// The repair revoked whatever the crash stranded, so the drained
		// epochs count as resolved up to the final epoch.
		if next.ProcessedEpochIdx != 20 {
			t.Fatalf("draining closure cursor = %d, want 20", next.ProcessedEpochIdx)
		}
	})

	t.Run("pending suspend same epoch keeps suspend, moves cursor", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspend,
			PendingEpochIdx: 25, PendingEpochDur: dur, PersistBook: &persist,
			ActiveEpochIdx: 18, ProcessedEpochIdx: 17,
		}
		next, changed, err := ProjectMarketStartedLifecycle(prev, update)
		if err != nil || !changed || next.ActiveEpochIdx != update.CurrentEpochIdx {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
		if next.PendingAction != MarketPendingSuspend || next.PendingEpochIdx != 25 ||
			next.StartEpochIdx != prev.StartEpochIdx {
			t.Fatalf("pending suspend fields not preserved: %+v", next)
		}
		if next.ProcessedEpochIdx != update.CurrentEpochIdx-1 {
			t.Fatalf("closure cursor = %d, want %d", next.ProcessedEpochIdx, update.CurrentEpochIdx-1)
		}
		// The restart re-pins the run parameters even with a suspend pending.
		if next.RunParams != update.RunParams {
			t.Fatalf("run params = %+v, want %+v", next.RunParams, update.RunParams)
		}
	})

	t.Run("pending drain re-baselines the closure cursor", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspendDrain,
			StartEpochDur: dur, FinalEpochIdx: 20, FinalEpochDur: dur,
			PendingEpochIdx: 20, PendingEpochDur: dur,
			PersistBook: &persist, ProcessedEpochIdx: 19, RunParams: testRunParams(),
		}
		next, changed, err := ProjectMarketStartedLifecycle(prev, update)
		if err != nil || !changed || next.PendingAction != MarketPendingSuspendDrain {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
		// The unprocessed final epoch was resolved by the repair.
		if next.ProcessedEpochIdx != 20 {
			t.Fatalf("draining closure cursor = %d, want 20", next.ProcessedEpochIdx)
		}
	})

	t.Run("pending drain with current cursor is a no-op", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspendDrain,
			StartEpochDur: dur, FinalEpochIdx: 20, FinalEpochDur: dur,
			PendingEpochIdx: 20, PendingEpochDur: dur,
			PersistBook: &persist, ProcessedEpochIdx: 20, RunParams: testRunParams(),
		}
		next, changed, err := ProjectMarketStartedLifecycle(prev, update)
		if err != nil || changed || next != prev {
			t.Fatalf("got next=%p changed=%v err=%v", next, changed, err)
		}
	})

	t.Run("pending drain rejects a params change", func(t *testing.T) {
		prev := &MarketLifecycle{
			Market: market, State: MarketStateRunning, PendingAction: MarketPendingSuspendDrain,
			StartEpochDur: dur, FinalEpochIdx: 20, FinalEpochDur: dur,
			PendingEpochIdx: 20, PendingEpochDur: dur,
			PersistBook: &persist, ProcessedEpochIdx: 20, RunParams: testRunParams(),
		}
		prev.RunParams.LotSize *= 3
		if _, _, err := ProjectMarketStartedLifecycle(prev, update); err == nil {
			t.Fatal("expected params change during drain to fail")
		}
	})

	t.Run("rejects suspended", func(t *testing.T) {
		prev := &MarketLifecycle{Market: market, State: MarketStateSuspended, PersistBook: &persist}
		if _, _, err := ProjectMarketStartedLifecycle(prev, update); err == nil {
			t.Fatal("expected suspended market_started to fail")
		}
	})
}

func TestProjectAdvanceEpochLifecycleCursor(t *testing.T) {
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

	t.Run("routine advance moves cursor", func(t *testing.T) {
		next, changed, err := ProjectAdvanceEpochLifecycle(running(15), &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 15, OpenedEpochIdx: 16, EpochDur: dur,
		})
		if err != nil || !changed || next.ActiveEpochIdx != 16 {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
		if next.PendingAction != MarketPendingNone || next.StartEpochIdx != 10 {
			t.Fatalf("advance mutated unrelated fields: %+v", next)
		}
	})

	t.Run("rejects closed epoch not matching cursor", func(t *testing.T) {
		if _, _, err := ProjectAdvanceEpochLifecycle(running(15), &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 14, OpenedEpochIdx: 15, EpochDur: dur,
		}); err == nil {
			t.Fatal("expected cursor mismatch to fail")
		}
	})

	t.Run("routine advance under pending suspend moves cursor", func(t *testing.T) {
		prev := running(15)
		prev.PendingAction = MarketPendingSuspend
		prev.PendingEpochIdx, prev.PendingEpochDur = 20, dur
		prev.FinalEpochIdx, prev.FinalEpochDur = 20, dur
		prev.PersistBook = &persist
		next, changed, err := ProjectAdvanceEpochLifecycle(prev, &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 15, OpenedEpochIdx: 16, EpochDur: dur,
		})
		if err != nil || !changed || next.ActiveEpochIdx != 16 || next.PendingAction != MarketPendingSuspend {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
	})

	t.Run("final close drains and zeroes cursor", func(t *testing.T) {
		prev := running(20)
		prev.PendingAction = MarketPendingSuspend
		prev.PendingEpochIdx, prev.PendingEpochDur = 20, dur
		prev.FinalEpochIdx, prev.FinalEpochDur = 20, dur
		prev.PersistBook = &persist
		next, changed, err := ProjectAdvanceEpochLifecycle(prev, &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 20, OpenedEpochIdx: 0, EpochDur: dur,
		})
		if err != nil || !changed || next.PendingAction != MarketPendingSuspendDrain || next.ActiveEpochIdx != 0 {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
	})

	t.Run("advance allows the closure lag limit exactly", func(t *testing.T) {
		prev := running(15)
		prev.ProcessedEpochIdx = 15 - MaxUnprocessedClosedEpochs
		next, changed, err := ProjectAdvanceEpochLifecycle(prev, &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 15, OpenedEpochIdx: 16, EpochDur: dur,
		})
		if err != nil || !changed || next.ActiveEpochIdx != 16 {
			t.Fatalf("got next=%+v changed=%v err=%v", next, changed, err)
		}
	})

	t.Run("advance rejects one past the closure lag limit", func(t *testing.T) {
		prev := running(15)
		prev.ProcessedEpochIdx = 15 - MaxUnprocessedClosedEpochs - 1
		if _, _, err := ProjectAdvanceEpochLifecycle(prev, &AdvanceEpochUpdate{
			Market: market, ClosedEpochIdx: 15, OpenedEpochIdx: 16, EpochDur: dur,
		}); err == nil {
			t.Fatal("expected closure lag limit to fail the advance")
		}
	})
}

func TestProjectEpochProcessedLifecycle(t *testing.T) {
	persist := true
	const market = "dcr_btc"
	const dur int64 = 10_000

	row := func(active, processed int64, pending MarketPendingAction) *MarketLifecycle {
		lc := &MarketLifecycle{
			Market:            market,
			State:             MarketStateRunning,
			StartEpochIdx:     10,
			StartEpochDur:     dur,
			PendingAction:     pending,
			ActiveEpochIdx:    active,
			ProcessedEpochIdx: processed,
		}
		if pending == MarketPendingSuspend || pending == MarketPendingSuspendDrain {
			lc.FinalEpochIdx, lc.FinalEpochDur = 20, dur
			lc.PendingEpochIdx, lc.PendingEpochDur = 20, dur
			lc.PersistBook = &persist
		}
		if pending == MarketPendingSuspendDrain {
			lc.ActiveEpochIdx = 0
		}
		return lc
	}

	tests := []struct {
		name          string
		prev          *MarketLifecycle
		epochIdx      int64
		epochDur      int64
		wantProcessed int64
		wantErr       bool
	}{
		{
			name: "close catching up to the newest closed epoch",
			prev: row(16, 14, MarketPendingNone), epochIdx: 15, epochDur: dur,
			wantProcessed: 15,
		}, {
			name: "rejects a close skipping an epoch",
			prev: row(16, 13, MarketPendingNone), epochIdx: 15, epochDur: dur,
			wantErr: true,
		}, {
			name: "rejects the still-open epoch",
			prev: row(16, 15, MarketPendingNone), epochIdx: 16, epochDur: dur,
			wantErr: true,
		}, {
			name: "pending suspend accepts a pre-final close",
			prev: row(19, 17, MarketPendingSuspend), epochIdx: 18, epochDur: dur,
			wantProcessed: 18,
		}, {
			name: "drain accepts a pre-final straggler",
			prev: row(0, 18, MarketPendingSuspendDrain), epochIdx: 19, epochDur: dur,
			wantProcessed: 19,
		}, {
			name: "drain accepts the final close",
			prev: row(0, 19, MarketPendingSuspendDrain), epochIdx: 20, epochDur: dur,
			wantProcessed: 20,
		}, {
			name: "drain rejects the final close with a duration mismatch",
			prev: row(0, 19, MarketPendingSuspendDrain), epochIdx: 20, epochDur: dur + 1,
			wantErr: true,
		}, {
			name: "drain rejects past the final epoch",
			prev: row(0, 20, MarketPendingSuspendDrain), epochIdx: 21, epochDur: dur,
			wantErr: true,
		}, {
			name: "rejects a suspended market",
			prev: func() *MarketLifecycle {
				lc := row(16, 15, MarketPendingNone)
				lc.State = MarketStateSuspended
				return lc
			}(), epochIdx: 16, epochDur: dur,
			wantErr: true,
		}, {
			name:    "rejects a missing row",
			prev:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, err := ProjectEpochProcessedLifecycle(tt.prev, market, tt.epochIdx, tt.epochDur)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected projection error, got %+v", next)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProjectEpochProcessedLifecycle: %v", err)
			}
			if next.ProcessedEpochIdx != tt.wantProcessed {
				t.Fatalf("closure cursor = %d, want %d", next.ProcessedEpochIdx, tt.wantProcessed)
			}
		})
	}
}
