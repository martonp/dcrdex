// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"context"
	"fmt"
	"sync"

	"decred.org/dcrdex/server/market"
	"decred.org/dcrdex/server/mesh"
)

type marketRunWorker struct {
	name   string
	market *market.Market

	mtx     sync.RWMutex
	started bool
}

func newMarketRunWorker(name string, mkt *market.Market) *marketRunWorker {
	return &marketRunWorker{
		name:   name,
		market: mkt,
	}
}

func newMasterWorkers(swapperRun func(context.Context, func(error)), marketsReady func(),
	markets map[string]*market.Market) (map[string]*marketRunWorker, []mesh.MasterWorker) {

	marketRuns := make(map[string]*marketRunWorker, len(markets))
	masterWorkers := make([]mesh.MasterWorker, 0, len(markets)+2)
	masterWorkers = append(masterWorkers, mesh.MasterWorker{Name: "Swapper", Run: swapperRun})
	for name, mkt := range markets {
		worker := newMarketRunWorker(name, mkt)
		marketRuns[name] = worker
		masterWorkers = append(masterWorkers, worker.masterWorker())
	}
	// Workers start sequentially, so this sentinel runs only after every
	// market has reported ready. It restarts the Swapper's inaction clocks:
	// until here a market held back by chain sync would burn the clients'
	// broadcast-timeout budget while they cannot act.
	masterWorkers = append(masterWorkers, mesh.MasterWorker{
		Name: "MarketsReady",
		Run: func(ctx context.Context, reportReady func(error)) {
			marketsReady()
			reportReady(nil)
			<-ctx.Done()
		},
	})
	return marketRuns, masterWorkers
}

func (w *marketRunWorker) masterWorker() mesh.MasterWorker {
	return mesh.MasterWorker{
		Name: marketSubSysName(w.name),
		Run:  w.Run,
	}
}

func (w *marketRunWorker) Run(ctx context.Context, reportReady func(error)) {
	w.markStarted()

	w.market.Run(ctx, func(err error) {
		if err != nil {
			err = fmt.Errorf("market %s startup cleanup failed: %w", w.name, err)
		}
		reportReady(err)
	})
}

func (w *marketRunWorker) hasStarted() bool {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.started
}

func (w *marketRunWorker) markStarted() {
	w.mtx.Lock()
	w.started = true
	w.mtx.Unlock()
}
