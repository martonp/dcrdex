// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"context"
	"testing"
	"time"

	"decred.org/dcrdex/server/market"
)

// TestNewMasterWorkersMarketsReadySentinel verifies that the MarketsReady
// sentinel is registered after every market worker, so the Swapper's inaction
// clocks restart only once the markets have reported ready.
func TestNewMasterWorkersMarketsReadySentinel(t *testing.T) {
	var readied bool
	swapperRun := func(context.Context, func(error)) {}
	_, workers := newMasterWorkers(swapperRun, func() { readied = true },
		map[string]*market.Market{"abc_xyz": nil, "def_xyz": nil})

	if len(workers) != 4 {
		t.Fatalf("expected 4 master workers, got %d", len(workers))
	}
	if workers[0].Name != "Swapper" {
		t.Fatalf("first worker = %q, want Swapper", workers[0].Name)
	}
	last := workers[len(workers)-1]
	if last.Name != "MarketsReady" {
		t.Fatalf("last worker = %q, want MarketsReady", last.Name)
	}

	ready := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		last.Run(ctx, func(err error) { ready <- err })
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("sentinel readiness error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sentinel never reported ready")
	}
	if !readied {
		t.Fatal("sentinel reported ready without calling marketsReady")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sentinel did not stop on context cancel")
	}
}
