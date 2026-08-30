// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

type marketEpochDriver struct {
	m             *Market
	epochPump     *epochPump
	epochDuration int64
}

func newMarketEpochDriver(m *Market) *marketEpochDriver {
	// We use the configured epoch duration here because the epoch driver
	// will be used by the master to create new events, never to replay
	// existing ones.
	return &marketEpochDriver{
		m:             m,
		epochDuration: m.configuredParams.epochDur,
	}
}

func (d *marketEpochDriver) run(parent context.Context, ready chan<- error) {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup

	startupEpoch, err := d.prepareStartup(ctx)
	if err != nil {
		cancel()
		log.Errorf("Market %q startup failed: %v", d.m.name, err)
		ready <- err
		return
	}

	if startupEpoch > 0 {
		if err := d.startAcceptingOrders(ctx, startupEpoch); err != nil {
			cancel()
			log.Errorf("Market %q startup failed: %v", d.m.name, err)
			ready <- err
			return
		}
	}

	d.epochPump = newEpochPump()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.epochPump.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.runEpochProcessing(ctx, cancel)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.runEpochAdvancer(ctx); err != nil {
			if !quietDriverStop(ctx, err) {
				log.Errorf("Market %s epoch advancer stopped: %v", d.m.name, err)
			}
			if ctx.Err() == nil {
				cancel()
			}
		}
	}()

	ready <- nil
	wg.Wait()

	cancel()
	d.markMarketStopped()
}

// quietDriverStop reports a failed submit that should end the run without an
// error log: the run context is already cancelled, or the mesh is unavailable
// because the node has halted and shutdown has not torn the market down yet.
func quietDriverStop(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, mesh.ErrUnavailable)
}

func (d *marketEpochDriver) runEpochProcessing(ctx context.Context, cancel context.CancelFunc) {
	for ep := range d.epochPump.ready {
		if err := d.m.processReadyEpoch(ctx, ep); err != nil {
			if !quietDriverStop(ctx, err) {
				log.Criticalf("Stopping market %s run: epoch %d close failed: %v",
					d.m.name, ep.Epoch, err)
			}
			cancel()
			break
		}
		if d.m.lifecyclePendingSuspendDrain(ep.Epoch, ep.Duration) {
			if err := d.m.submitMarketSuspend(ctx); err != nil {
				if !d.m.lifecycleFinalizingSuspend() {
					// The suspend landed despite the error report.
					log.Debugf("Market %s suspend applied despite a submit error: %v",
						d.m.name, err)
					continue
				}
				if !quietDriverStop(ctx, err) {
					log.Errorf("Failed to apply market suspend event for market %s after final epoch %d: %v",
						d.m.name, ep.Epoch, err)
				}
				cancel()
				break
			}
		}
	}
	// Even after cancellation the pump keeps delivering its queued epochs
	// until it drains; consume them so its Run can exit.
	for skipped := range d.epochPump.ready {
		log.Warnf("Skipping market %s epoch %d after an earlier pipeline failure.",
			d.m.name, skipped.Epoch)
	}
	log.Debugf("epoch pump drained for market %s", d.m.name)
}

func (d *marketEpochDriver) prepareStartup(ctx context.Context) (int64, error) {
	d.m.epochMtx.RLock()
	suspended := d.m.lifecycleState == db.MarketStateSuspended
	d.m.epochMtx.RUnlock()
	if suspended {
		log.Infof("Market %q is starting suspended; waiting for lifecycle resume event.", d.m.name)
		return 0, nil
	}
	if err := d.waitForChainSync(ctx); err != nil {
		return 0, err
	}

	startupEpoch, err := d.m.submitMarketStarted(ctx)
	if err != nil {
		return 0, err
	}
	if d.m.lifecycleFinalizingSuspend() {
		if err := d.m.submitMarketSuspend(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return startupEpoch, nil
}

func (d *marketEpochDriver) waitForChainSync(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.checkStorageHealth(); err != nil {
			return err
		}

		synced, err := d.m.swapper.ChainsSynced(d.m.base, d.m.quote)
		if err != nil {
			log.Errorf("Not starting %s market because of ChainsSynced error: %v", d.m.name, err)
			if err := d.waitUntilNextEpoch(ctx); err != nil {
				return err
			}
			continue
		}
		if !synced {
			log.Debugf("Delaying start of %s market because chains aren't synced", d.m.name)
			if err := d.waitUntilNextEpoch(ctx); err != nil {
				return err
			}
			continue
		}

		return nil
	}
}

func (d *marketEpochDriver) waitUntilNextEpoch(ctx context.Context) error {
	now := time.Now().UnixMilli()
	nextEpochStart := time.UnixMilli((now/d.epochDuration + 1) * d.epochDuration).UTC()
	return d.waitUntil(ctx, nextEpochStart)
}

func (d *marketEpochDriver) checkStorageHealth() error {
	if err := d.m.storage.LastErr(); err != nil {
		log.Criticalf("Archivist failing. Last unexpected error: %v", err)
		return err
	}
	return nil
}

func (d *marketEpochDriver) startAcceptingOrders(ctx context.Context, startupEpoch int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.m.epochMtx.Lock()
	d.m.activeEpochIdx = startupEpoch
	d.m.epochMtx.Unlock()

	d.m.running.Store(true)

	log.Infof("Market %s now accepting orders, epoch %d:%d", d.m.name,
		startupEpoch, d.epochDuration)
	return nil
}

func (d *marketEpochDriver) runEpochAdvancer(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.checkStorageHealth(); err != nil {
			return err
		}

		snap := d.epochAdvanceSnapshot()
		if snap.nextEpoch == nil {
			if err := d.handleIdleEpochAdvance(ctx, snap); err != nil {
				return err
			}
			continue
		}

		if err := d.waitUntil(ctx, snap.nextEpoch.Start); err != nil {
			return err
		}
		closingIdx := snap.nextEpoch.Epoch - 1
		if err := d.waitForEpochClosures(ctx, closingIdx); err != nil {
			return err
		}
		if err := d.advanceEpoch(ctx); err != nil {
			return err
		}
	}
}

// waitForEpochClosures blocks until closing the given epoch would leave at
// most db.MaxUnprocessedClosedEpochs closed epochs awaiting epoch_processed,
// matching the advance_epoch applier gate.
func (d *marketEpochDriver) waitForEpochClosures(ctx context.Context, closingIdx int64) error {
	minProcessed := closingIdx - db.MaxUnprocessedClosedEpochs
	if d.processedEpoch() >= minProcessed {
		return nil
	}
	log.Warnf("Market %s cannot close epoch %d until epoch %d's close applies (closure cursor %d); "+
		"pausing the boundary advance.", d.m.name, closingIdx, minProcessed, d.processedEpoch())
	for {
		if d.processedEpoch() >= minProcessed {
			log.Infof("Market %s epoch closes caught up (closure cursor %d); resuming the boundary advance.",
				d.m.name, d.processedEpoch())
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.m.closureWake:
		}
	}
}

func (d *marketEpochDriver) processedEpoch() int64 {
	d.m.epochMtx.RLock()
	defer d.m.epochMtx.RUnlock()
	return d.m.processedEpochIdx
}

type epochAdvanceSnapshot struct {
	nextEpoch       *EpochQueue
	lifecycleState  db.MarketState
	pendingAction   db.MarketPendingAction
	pendingEpochIdx int64
	pendingEpochDur int64
}

func (d *marketEpochDriver) epochAdvanceSnapshot() epochAdvanceSnapshot {
	d.m.epochMtx.RLock()
	defer d.m.epochMtx.RUnlock()
	return epochAdvanceSnapshot{
		nextEpoch:       d.m.nextEpoch,
		lifecycleState:  d.m.lifecycleState,
		pendingAction:   d.m.pendingLifecycleAction,
		pendingEpochIdx: d.m.pendingLifecycleEpochIdx,
		pendingEpochDur: d.m.pendingLifecycleEpochDur,
	}
}

// handleIdleEpochAdvance parks the advancer while there is no next epoch to
// open: pending resume, suspended/draining idle, or a fatal missing-epoch state.
// A nil error means the caller should re-snapshot and continue.
func (d *marketEpochDriver) handleIdleEpochAdvance(ctx context.Context, snap epochAdvanceSnapshot) error {
	switch {
	case snap.lifecycleState == db.MarketStateSuspended && snap.pendingAction == db.MarketPendingResume:
		return d.runPendingResume(ctx, snap.pendingEpochIdx, snap.pendingEpochDur)
	case snap.lifecycleState == db.MarketStateSuspended || snap.pendingAction == db.MarketPendingSuspendDrain:
		return d.waitLifecycleWake(ctx)
	default:
		err := fmt.Errorf("market %s cannot advance epoch before market_started: missing next epoch",
			d.m.name)
		log.Errorf("%v", err)
		return err
	}
}

func (d *marketEpochDriver) runPendingResume(ctx context.Context, pendingEpochIdx, pendingEpochDur int64) error {
	resumeTime := time.UnixMilli(pendingEpochIdx * pendingEpochDur)
	woke, err := d.waitUntilOrLifecycleWake(ctx, resumeTime)
	if err != nil {
		return err
	}
	if woke {
		return nil
	}
	if err := d.waitForChainSync(ctx); err != nil {
		return err
	}
	if err := d.m.submitMarketResume(ctx, pendingEpochIdx, pendingEpochDur); err != nil {
		if !d.m.lifecyclePendingResume(pendingEpochIdx, pendingEpochDur) {
			log.Debugf("Market %s pending resume %d:%d changed before resume event applied.",
				d.m.name, pendingEpochIdx, pendingEpochDur)
			return nil
		}
		if !quietDriverStop(ctx, err) {
			log.Errorf("Failed to apply market resume event for market %s: %v", d.m.name, err)
		}
		return err
	}
	return nil
}

func (d *marketEpochDriver) waitLifecycleWake(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.m.lifecycleWake:
		return nil
	}
}

func (d *marketEpochDriver) waitUntilOrLifecycleWake(ctx context.Context, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	wait := time.Until(at)
	if wait <= 0 {
		return false, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-d.m.lifecycleWake:
		return true, nil
	case <-timer.C:
		return false, ctx.Err()
	}
}

func (d *marketEpochDriver) waitUntil(ctx context.Context, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := time.Until(at)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (d *marketEpochDriver) advanceEpoch(ctx context.Context) error {
	closedEpoch, err := d.applyAdvanceEpoch(ctx)
	if err != nil {
		return err
	}

	if !d.m.enqueueEpoch(d.epochPump, closedEpoch) {
		return fmt.Errorf("market %s failed to enqueue closed epoch %d", d.m.name, closedEpoch.Epoch)
	}

	return nil
}

func (d *marketEpochDriver) applyAdvanceEpoch(ctx context.Context) (*EpochQueue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.m.epochMtx.RLock()
	currentEpoch, nextEpoch := d.m.currentEpoch, d.m.nextEpoch
	pendingAction := d.m.pendingLifecycleAction
	pendingEpochIdx := d.m.pendingLifecycleEpochIdx
	if currentEpoch == nil || nextEpoch == nil {
		d.m.epochMtx.RUnlock()
		return nil, fmt.Errorf("market %s cannot advance epoch before market_started", d.m.name)
	}
	closedEpochIdx, openedEpochIdx, epochDur := currentEpoch.Epoch, nextEpoch.Epoch, nextEpoch.Duration
	if pendingAction == db.MarketPendingSuspend && closedEpochIdx == pendingEpochIdx {
		openedEpochIdx = 0
	}
	d.m.epochMtx.RUnlock()

	advanceEpochEvent, err := mesh.NewEvent(meshevents.NewAdvanceEpochEvent(d.m.name, closedEpochIdx, openedEpochIdx, epochDur))
	if err != nil {
		return nil, fmt.Errorf("failed to build advance epoch event for epoch %d: %w", openedEpochIdx, err)
	}
	if _, err := d.m.mesh.ApplyEvent(ctx, advanceEpochEvent); err != nil {
		return nil, fmt.Errorf("failed to apply advance epoch event for epoch %d: %w", openedEpochIdx, err)
	}

	return currentEpoch, nil
}

func (d *marketEpochDriver) markMarketStopped() {
	d.stopAcceptingOrders()

	d.m.epochMtx.Lock()
	d.m.activeEpochIdx = 0
	d.m.epochMtx.Unlock()

	d.m.tasks.Wait()

	// Do not directly finalize open epoch orders here. Persistent order-status
	// changes must be event-backed in mesh mode; epoch orders left by process
	// shutdown are disposed of by the next authoritative startup, whose
	// market_started event revokes every leftover epoch order with owner
	// notification and unlocks their funding coins.
	log.Infof("Market %q stopped.", d.m.name)
}

func (d *marketEpochDriver) stopAcceptingOrders() {
	d.m.running.Store(false)
}
