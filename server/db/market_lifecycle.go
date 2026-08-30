// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import "fmt"

// MaxUnprocessedClosedEpochs is how many closed epochs may await
// epoch_processed before the next advance is refused.
const MaxUnprocessedClosedEpochs = 2

// ProjectMarketLifecycle returns the lifecycle row after applying update.
// It validates the transition against prev but performs no storage I/O.
func ProjectMarketLifecycle(prev *MarketLifecycle, update *MarketLifecycleUpdate) (*MarketLifecycle, error) {
	if update == nil {
		return nil, fmt.Errorf("nil market lifecycle update")
	}
	if prev == nil {
		return nil, fmt.Errorf("missing lifecycle row for market %s", update.Market)
	}
	switch update.Action {
	case MarketLifecycleActionScheduleSuspend:
		return projectScheduleSuspend(prev, update)
	case MarketLifecycleActionSuspend:
		return projectSuspend(prev, update)
	case MarketLifecycleActionScheduleResume:
		return projectScheduleResume(prev, update)
	case MarketLifecycleActionResume:
		return projectResume(prev, update)
	default:
		return nil, fmt.Errorf("unknown market lifecycle action %d", update.Action)
	}
}

func projectScheduleSuspend(prev *MarketLifecycle, update *MarketLifecycleUpdate) (*MarketLifecycle, error) {
	if prev.State != MarketStateRunning ||
		(prev.PendingAction != MarketPendingNone && prev.PendingAction != MarketPendingSuspend) {
		return nil, fmt.Errorf("cannot schedule suspend for market %s in lifecycle %d/%d",
			update.Market, prev.State, prev.PendingAction)
	}
	persist, err := requirePersistBook(update.PersistBook, "schedule_suspend")
	if err != nil {
		return nil, err
	}
	next := *prev
	next.FinalEpochIdx = update.EpochIdx
	next.FinalEpochDur = update.EpochDur
	next.PendingAction = MarketPendingSuspend
	next.PendingEpochIdx = update.EpochIdx
	next.PendingEpochDur = update.EpochDur
	next.PersistBook = persist
	return &next, nil
}

func projectSuspend(prev *MarketLifecycle, update *MarketLifecycleUpdate) (*MarketLifecycle, error) {
	if prev.State != MarketStateRunning || prev.PendingAction != MarketPendingSuspendDrain {
		return nil, fmt.Errorf("cannot suspend market %s in lifecycle %d/%d",
			update.Market, prev.State, prev.PendingAction)
	}
	if !sameLifecycleEpoch(prev.PendingEpochIdx, prev.PendingEpochDur, update.EpochIdx, update.EpochDur) {
		return nil, fmt.Errorf("suspend final epoch mismatch for market %s", update.Market)
	}
	if prev.ProcessedEpochIdx != prev.PendingEpochIdx {
		return nil, fmt.Errorf("suspend before final epoch %d close applied (closure cursor %d) for market %s",
			prev.PendingEpochIdx, prev.ProcessedEpochIdx, update.Market)
	}
	if prev.PersistBook == nil {
		return nil, fmt.Errorf("suspend missing persist_book")
	}
	next := *prev
	next.State = MarketStateSuspended
	next.FinalEpochIdx = update.EpochIdx
	next.FinalEpochDur = update.EpochDur
	next.PendingAction = MarketPendingNone
	next.PendingEpochIdx = 0
	next.PendingEpochDur = 0
	next.PersistBook = cloneBoolPtr(prev.PersistBook)
	next.ActiveEpochIdx = 0
	return &next, nil
}

func projectScheduleResume(prev *MarketLifecycle, update *MarketLifecycleUpdate) (*MarketLifecycle, error) {
	if prev.State != MarketStateSuspended ||
		(prev.PendingAction != MarketPendingNone && prev.PendingAction != MarketPendingResume) {
		return nil, fmt.Errorf("cannot schedule resume for market %s in lifecycle %d/%d",
			update.Market, prev.State, prev.PendingAction)
	}
	persist := cloneBoolPtr(prev.PersistBook)
	if persist == nil {
		return nil, fmt.Errorf("schedule_resume missing persisted persist_book")
	}
	next := *prev
	next.StartEpochIdx = update.EpochIdx
	next.StartEpochDur = update.EpochDur
	next.FinalEpochIdx = 0
	next.FinalEpochDur = 0
	next.PendingAction = MarketPendingResume
	next.PendingEpochIdx = update.EpochIdx
	next.PendingEpochDur = update.EpochDur
	next.PersistBook = persist
	return &next, nil
}

func projectResume(prev *MarketLifecycle, update *MarketLifecycleUpdate) (*MarketLifecycle, error) {
	if prev.State != MarketStateSuspended || prev.PendingAction != MarketPendingResume {
		return nil, fmt.Errorf("cannot resume market %s in lifecycle %d/%d",
			update.Market, prev.State, prev.PendingAction)
	}
	if !sameLifecycleEpoch(prev.PendingEpochIdx, prev.PendingEpochDur, update.EpochIdx, update.EpochDur) {
		return nil, fmt.Errorf("resume pending epoch mismatch for market %s", update.Market)
	}
	if update.RunParams == nil {
		return nil, fmt.Errorf("resume missing run parameters for market %s", update.Market)
	}
	return &MarketLifecycle{
		Market:            update.Market,
		State:             MarketStateRunning,
		StartEpochIdx:     update.OpenEpochIdx(),
		StartEpochDur:     update.EpochDur,
		PendingAction:     MarketPendingNone,
		ActiveEpochIdx:    update.OpenEpochIdx(),
		ProcessedEpochIdx: update.OpenEpochIdx() - 1,
		RunParams:         *update.RunParams,
	}, nil
}

// ProjectMarketStartedLifecycle returns the lifecycle row after applying a
// market_started update. changed is false when the existing row needs no write.
func ProjectMarketStartedLifecycle(prev *MarketLifecycle, update *MarketStartedUpdate) (next *MarketLifecycle, changed bool, err error) {
	if update == nil {
		return nil, false, fmt.Errorf("nil market started update")
	}
	// A first start has no row; a restart with nothing pending re-baselines
	// it. Both take a fresh start row.
	if prev == nil {
		return marketStartedLifecycleRow(update), true, nil
	}
	if prev.State != MarketStateRunning {
		return nil, false, fmt.Errorf("market_started for market %s in state %d", update.Market, prev.State)
	}

	// market_started's repair revokes leftover epoch-status orders, so every
	// path re-baselines the closure cursor. Those epochs are never processed.
	switch prev.PendingAction {
	case MarketPendingNone:
		return marketStartedLifecycleRow(update), true, nil
	case MarketPendingSuspendDrain:
		// This start does not open trading. Reject a params change so we
		// do not revoke the book against a lot size that never trades.
		if update.EpochDur != prev.StartEpochDur || update.RunParams != prev.RunParams {
			return nil, false, fmt.Errorf("market_started must carry the run's pinned parameters while market %s drains", update.Market)
		}
		if prev.ProcessedEpochIdx == prev.PendingEpochIdx {
			return prev, false, nil
		}
		drained := *prev
		drained.ProcessedEpochIdx = prev.PendingEpochIdx
		return &drained, true, nil
	case MarketPendingSuspend:
		// Pending suspend epochs are in the old duration unit.
		if update.EpochDur != prev.PendingEpochDur {
			return nil, false, fmt.Errorf("market_started duration %d mismatches pending suspend duration %d for market %s; "+
				"the epoch duration cannot change while a suspend is pending",
				update.EpochDur, prev.PendingEpochDur, update.Market)
		}
		if update.CurrentEpochIdx > prev.PendingEpochIdx {
			// Final trading epoch passed while down: go straight to drain.
			// Same as drain: do not change params.
			if update.RunParams != prev.RunParams {
				return nil, false, fmt.Errorf("market_started must carry the run's pinned parameters while market %s completes its suspend", update.Market)
			}
			drained := *prev
			drained.PendingAction = MarketPendingSuspendDrain
			drained.ActiveEpochIdx = 0
			drained.ProcessedEpochIdx = prev.PendingEpochIdx
			return &drained, true, nil
		}
		// Trading resumes at the update's epoch with the pending suspend
		// still standing; the cursor jumps with the current epoch.
		resumed := *prev
		resumed.ActiveEpochIdx = update.CurrentEpochIdx
		resumed.ProcessedEpochIdx = update.CurrentEpochIdx - 1
		resumed.RunParams = update.RunParams
		return &resumed, true, nil
	default:
		return nil, false, fmt.Errorf("market %s lifecycle row has invalid state/action %d/%d",
			update.Market, prev.State, prev.PendingAction)
	}
}

// ProjectAdvanceEpochLifecycle validates an advance_epoch update against the
// lifecycle row and returns the row after applying it. The only transition an
// epoch advance drives is the final close of a pending suspend, which parks
// the row in MarketPendingSuspendDrain until the suspend event lands; changed
// is false when the existing row needs no write.
func ProjectAdvanceEpochLifecycle(prev *MarketLifecycle, update *AdvanceEpochUpdate) (next *MarketLifecycle, changed bool, err error) {
	if update == nil {
		return nil, false, fmt.Errorf("nil advance epoch update")
	}
	if prev == nil {
		return nil, false, fmt.Errorf("missing lifecycle row for market %s", update.Market)
	}
	if prev.State != MarketStateRunning {
		return nil, false, fmt.Errorf("advance_epoch for non-running market %s", update.Market)
	}
	if update.EpochDur != prev.StartEpochDur {
		// The cursor arithmetic below is meaningless across units.
		return nil, false, fmt.Errorf("advance_epoch duration %d mismatches run duration %d for market %s",
			update.EpochDur, prev.StartEpochDur, update.Market)
	}
	if update.ClosedEpochIdx != prev.ActiveEpochIdx {
		return nil, false, fmt.Errorf("advance_epoch closed epoch %d does not match active epoch cursor %d for market %s",
			update.ClosedEpochIdx, prev.ActiveEpochIdx, update.Market)
	}
	if prev.ProcessedEpochIdx < update.ClosedEpochIdx-MaxUnprocessedClosedEpochs {
		return nil, false, fmt.Errorf("advance_epoch closing epoch %d would exceed %d unprocessed epochs (closure cursor %d) for market %s",
			update.ClosedEpochIdx, MaxUnprocessedClosedEpochs, prev.ProcessedEpochIdx, update.Market)
	}
	switch prev.PendingAction {
	case MarketPendingNone:
		if update.OpenedEpochIdx <= 0 {
			return nil, false, fmt.Errorf("advance_epoch opened epoch must be positive for market %s", update.Market)
		}
		next := *prev
		next.ActiveEpochIdx = update.OpenedEpochIdx
		return &next, true, nil
	case MarketPendingSuspend:
		if update.OpenedEpochIdx == 0 {
			if !sameLifecycleEpoch(prev.PendingEpochIdx, prev.PendingEpochDur, update.ClosedEpochIdx, update.EpochDur) {
				return nil, false, fmt.Errorf("final-close epoch mismatch for market %s", update.Market)
			}
			drained := *prev
			drained.PendingAction = MarketPendingSuspendDrain
			drained.ActiveEpochIdx = 0
			return &drained, true, nil
		}
		if update.ClosedEpochIdx >= prev.PendingEpochIdx || update.OpenedEpochIdx > prev.PendingEpochIdx {
			return nil, false, fmt.Errorf("advance_epoch crosses pending suspend final epoch for market %s", update.Market)
		}
		next := *prev
		next.ActiveEpochIdx = update.OpenedEpochIdx
		return &next, true, nil
	default:
		return nil, false, fmt.Errorf("advance_epoch rejected for market %s lifecycle pending action %d",
			update.Market, prev.PendingAction)
	}
}

// ProjectEpochProcessedLifecycle returns the lifecycle row after an
// epoch_processed close. The epoch must be ProcessedEpochIdx+1 and already
// closed.
func ProjectEpochProcessedLifecycle(prev *MarketLifecycle, market string, epochIdx, epochDur int64) (*MarketLifecycle, error) {
	if prev == nil {
		return nil, fmt.Errorf("missing lifecycle row for market %s", market)
	}
	if prev.State != MarketStateRunning {
		return nil, fmt.Errorf("epoch_processed for non-running market %s", market)
	}
	if epochDur != prev.StartEpochDur {
		// The cursor arithmetic below is meaningless across units.
		return nil, fmt.Errorf("epoch_processed duration %d mismatches run duration %d for market %s",
			epochDur, prev.StartEpochDur, market)
	}
	lastClosed, err := lastClosedEpoch(prev)
	if err != nil {
		return nil, err
	}
	if epochIdx != prev.ProcessedEpochIdx+1 {
		return nil, fmt.Errorf("epoch_processed epoch %d does not follow closure cursor %d for market %s",
			epochIdx, prev.ProcessedEpochIdx, market)
	}
	if epochIdx > lastClosed {
		return nil, fmt.Errorf("epoch_processed epoch %d is not closed (last closed epoch %d) for market %s",
			epochIdx, lastClosed, market)
	}
	if prev.PendingAction == MarketPendingSuspendDrain &&
		epochIdx == prev.PendingEpochIdx &&
		!sameLifecycleEpoch(epochIdx, epochDur, prev.PendingEpochIdx, prev.PendingEpochDur) {
		return nil, fmt.Errorf("epoch_processed final-drain epoch mismatch for market %s", market)
	}
	next := *prev
	next.ProcessedEpochIdx = epochIdx
	return &next, nil
}

// lastClosedEpoch is the newest epoch whose intake has closed. While running
// that is ActiveEpochIdx-1; while draining, intake is parked so it is the
// final pending epoch.
func lastClosedEpoch(lc *MarketLifecycle) (int64, error) {
	switch lc.PendingAction {
	case MarketPendingNone, MarketPendingSuspend:
		return lc.ActiveEpochIdx - 1, nil
	case MarketPendingSuspendDrain:
		return lc.PendingEpochIdx, nil
	default:
		return 0, fmt.Errorf("epoch_processed rejected for market %s lifecycle pending action %d",
			lc.Market, lc.PendingAction)
	}
}

func marketStartedLifecycleRow(update *MarketStartedUpdate) *MarketLifecycle {
	return &MarketLifecycle{
		Market:            update.Market,
		State:             MarketStateRunning,
		StartEpochIdx:     update.CurrentEpochIdx,
		StartEpochDur:     update.EpochDur,
		PendingAction:     MarketPendingNone,
		ActiveEpochIdx:    update.CurrentEpochIdx,
		ProcessedEpochIdx: update.CurrentEpochIdx - 1,
		RunParams:         update.RunParams,
	}
}

func sameLifecycleEpoch(idxA, durA, idxB, durB int64) bool {
	return idxA == idxB && durA == durB
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	cpy := *v
	return &cpy
}

func requirePersistBook(v *bool, action string) (*bool, error) {
	if v == nil {
		return nil, fmt.Errorf("%s missing persist_book", action)
	}
	return cloneBoolPtr(v), nil
}
