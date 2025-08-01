// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"github.com/davecgh/go-spew/spew"
)

func tryWithTimeout(t *testing.T, f func() error) {
	t.Helper()
	var err error
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		err = f()
		if err == nil {
			return
		}
	}
	t.Fatal(err)
}

func TestEventLogDB(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := newBoltEventLogDB(ctx, filepath.Join(dir, "event_log.db"), tLogger)
	if err != nil {
		t.Fatalf("error creating event log db: %v", err)
	}

	startTime := time.Now().Unix()
	mkt := &MarketWithHost{
		Host:    "dex.com",
		BaseID:  42,
		QuoteID: 60,
	}

	fiatRates := map[uint32]float64{
		42: 20,
		60: 2500,
	}

	cfg := &BotConfig{
		Host:    "dex.com",
		BaseID:  42,
		QuoteID: 60,
		CEXName: "Binance",
		ArbMarketMakerConfig: &ArbMarketMakerConfig{
			BuyPlacements: []*ArbMarketMakingPlacement{
				{
					Lots:       1,
					Multiplier: 2,
				},
			},
			SellPlacements: []*ArbMarketMakingPlacement{
				{
					Lots:       1,
					Multiplier: 2,
				},
			},
		},
	}

	initialBals := map[uint32]uint64{
		42: 3e9,
		60: 3e9,
	}

	currBals := map[uint32]*BotBalance{
		42: {
			Available: initialBals[42],
		},
		60: {
			Available: initialBals[60],
		},
	}

	inventoryMods := map[uint32]int64{}

	currBalanceState := func() *BalanceState {
		balances := make(map[uint32]*BotBalance, len(currBals))
		for k, v := range currBals {
			balances[k] = &BotBalance{
				Available: v.Available,
				Pending:   v.Pending,
				Locked:    v.Locked,
			}
		}
		rates := make(map[uint32]float64, len(fiatRates))
		for k, v := range fiatRates {
			rates[k] = v
		}
		return &BalanceState{
			Balances:      balances,
			FiatRates:     rates,
			InventoryMods: inventoryMods,
		}
	}

	currFinalBals := func() map[uint32]uint64 {
		bs := currBalanceState()
		bals := make(map[uint32]uint64, len(bs.Balances))
		for k, v := range bs.Balances {
			bals[k] = v.Available + v.Pending + v.Locked + v.Reserved
		}
		return bals
	}

	err = db.storeNewRun(startTime, mkt, cfg, currBalanceState())
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}

	runs, err := db.runs(0, nil, nil)
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	expectedRun := &MarketMakingRun{
		StartTime: startTime,
		Market:    mkt,
	}
	if !reflect.DeepEqual(runs[0], expectedRun) {
		t.Fatalf("expected run:\n%v\n\ngot:\n%v", expectedRun, runs[0])
	}

	event1 := &MarketMakingEvent{
		ID:        1,
		TimeStamp: startTime + 1,
		BalanceEffects: &BalanceEffects{
			Settled: map[uint32]int64{
				42: 1e6,
			},
			Locked: map[uint32]uint64{
				60: 2e6,
			},
		},
		Pending: true,
		DEXOrderEvent: &DEXOrderEvent{
			ID:   "order1",
			Rate: 5e7,
			Qty:  5e6,
			Sell: false,
			Transactions: []*asset.WalletTransaction{
				{
					Type:   asset.Swap,
					ID:     "tx1",
					Amount: 2e6,
					Fees:   100,
				},
				{
					Type:   asset.Redeem,
					ID:     "tx2",
					Amount: 1e6,
					Fees:   200,
				},
			},
		},
	}

	currBals[42].Available += 2e6
	currBals[42].Pending += 1e6
	currBals[60].Available += 8e6
	db.storeEvent(startTime, mkt, event1, currBalanceState())

	event2 := &MarketMakingEvent{
		ID:        2,
		TimeStamp: startTime + 1,
		BalanceEffects: &BalanceEffects{
			Settled: map[uint32]int64{
				42: 3e6,
			},
			Locked: map[uint32]uint64{
				60: 4e6,
			},
		},
		Pending: true,
		CEXOrderEvent: &CEXOrderEvent{
			ID:          "order1",
			Rate:        5e7,
			Qty:         5e6,
			Sell:        false,
			BaseFilled:  1e6,
			QuoteFilled: 2e6,
		},
	}
	currBals[42].Available += 3e6
	currBals[60].Available += 4e6
	currBals[42].Pending += 1e6
	db.storeEvent(startTime, mkt, event2, currBalanceState())

	// Get all run events
	check := func() error {
		runEvents, err := db.runEvents(startTime, mkt, 0, nil, false, nil)
		if err != nil {
			return fmt.Errorf("error getting run events: %v", err)
		}
		if len(runEvents) != 2 {
			return fmt.Errorf("expected 2 run event, got %d", len(runEvents))
		}
		if !reflect.DeepEqual(runEvents[0], event2) {
			return fmt.Errorf("expected event:\n%v\n\ngot:\n%v", event2, runEvents[0])
		}
		if !reflect.DeepEqual(runEvents[1], event1) {
			return fmt.Errorf("expected event:\n%v\n\ngot:\n%v", event1, runEvents[1])
		}
		return nil
	}
	tryWithTimeout(t, check)

	// Get only 1 run event
	runEvents, err := db.runEvents(startTime, mkt, 1, nil, false, nil)
	if err != nil {
		t.Fatalf("error getting run events: %v", err)
	}
	if len(runEvents) != 1 {
		t.Fatalf("expected 1 run event, got %d", len(runEvents))
	}
	if !reflect.DeepEqual(runEvents[0], event2) {
		t.Fatalf("expected event:\n%v\n\ngot:\n%v", event2, runEvents[0])
	}

	// Get run events with ref ID
	runEvents, err = db.runEvents(startTime, mkt, 1, &event1.ID, false, nil)
	if err != nil {
		t.Fatalf("error getting run events: %v", err)
	}
	if len(runEvents) != 1 {
		t.Fatalf("expected 1 run event, got %d", len(runEvents))
	}
	if !reflect.DeepEqual(runEvents[0], event1) {
		t.Fatalf("expected event:\n%v\n\ngot:\n%v", event1, runEvents[0])
	}

	runs, err = db.runs(0, nil, nil)
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	pl := newProfitLoss(initialBals, currFinalBals(), nil, fiatRates)
	expectedRun.Profit = pl.Profit
	if !reflect.DeepEqual(runs[0], expectedRun) {
		t.Fatalf("expected run:\n%v\n\ngot:\n%v", expectedRun, runs[0])
	}

	// Update event1 and fiat rates
	event1.BalanceEffects.Settled[42] += 100
	event1.BalanceEffects.Locked[60] -= 100
	event1.Pending = false
	currBals[42].Available += 100 - 20
	currBals[60].Available -= 200 + 10
	fiatRates[42] = 25
	fiatRates[60] = 3000
	db.storeEvent(startTime, mkt, event1, currBalanceState())

	// Get all run events
	check = func() error {
		runEvents, err := db.runEvents(startTime, mkt, 0, nil, false, nil)
		if err != nil {
			return fmt.Errorf("error getting run events: %v", err)
		}
		if len(runEvents) != 2 {
			return fmt.Errorf("expected 2 run event, got %d", len(runEvents))
		}
		if !reflect.DeepEqual(runEvents[0], event2) {
			return fmt.Errorf("expected event:\n%v\n\ngot:\n%v", event2, runEvents[0])
		}
		if !reflect.DeepEqual(runEvents[1], event1) {
			return fmt.Errorf("expected event:\n%v\n\ngot:\n%v", event1, runEvents[1])
		}
		return nil
	}
	tryWithTimeout(t, check)

	runs, err = db.runs(0, nil, nil)
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	pl = newProfitLoss(initialBals, currFinalBals(), nil, fiatRates)
	expectedRun.Profit = pl.Profit
	if !reflect.DeepEqual(runs[0], expectedRun) {
		t.Fatalf("expected run:\n%v\n\ngot:\n%v", expectedRun, runs[0])
	}

	// Fetch pending runs only
	runEvents, err = db.runEvents(startTime, mkt, 0, nil, true, nil)
	if err != nil {
		t.Fatalf("error getting run events: %v", err)
	}
	if len(runEvents) != 1 {
		t.Fatalf("expected 1 run events, got %d", len(runEvents))
	}
	if !reflect.DeepEqual(runEvents[0], event2) {
		t.Fatalf("expected event:\n%v\n\ngot:\n%v", event2, runEvents[0])
	}

	err = db.endRun(startTime, mkt)
	if err != nil {
		t.Fatalf("error ending run: %v", err)
	}

	overview, err := db.runOverview(startTime, mkt)
	if err != nil {
		t.Fatalf("error getting run overview: %v", err)
	}
	if *overview.EndTime < startTime || *overview.EndTime > time.Now().Unix() {
		t.Fatalf("expected end time %d, got %d", startTime, overview.EndTime)
	}
	if !reflect.DeepEqual(overview.InitialBalances, initialBals) {
		t.Fatalf("expected initial balances %v, got %v", initialBals, overview.InitialBalances)
	}
	expPL := newProfitLoss(initialBals, currFinalBals(), nil, fiatRates)
	if overview.ProfitLoss.Profit != expPL.Profit {
		t.Fatalf("expected profit loss %v, got %v", expPL, overview.ProfitLoss)
	}
	if !reflect.DeepEqual(overview.Cfgs[0].Cfg, cfg) {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", spew.Sdump(cfg), spew.Sdump(overview.Cfgs[0]))
	}

	// Test sorting / pagination of runs
	err = db.storeNewRun(startTime+1, mkt, cfg, currBalanceState())
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}
	err = db.storeNewRun(startTime-1, mkt, cfg, currBalanceState())
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}
	runs, err = db.runs(2, nil, nil)
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].StartTime != startTime+1 {
		t.Fatalf("expected run start time %d, got %d", startTime+1, runs[0].StartTime)
	}
	if runs[1].StartTime != startTime {
		t.Fatalf("expected run start time %d, got %d", startTime, runs[1].StartTime)
	}

	refStartTime := uint64(startTime)
	runs, err = db.runs(2, &refStartTime, mkt)
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].StartTime != startTime {
		t.Fatalf("expected run start time %d, got %d", startTime, runs[0].StartTime)
	}
	if runs[1].StartTime != startTime-1 {
		t.Fatalf("expected run start time %d, got %d", startTime-1, runs[1].StartTime)
	}

	// Update config and modify inventory
	updatedCfgB, _ := json.Marshal(cfg)
	updatedCfg := new(BotConfig)
	json.Unmarshal(updatedCfgB, updatedCfg)
	updatedCfg.ArbMarketMakerConfig.BuyPlacements[0].Lots++
	inventoryMods[42] = 1e6
	inventoryMods[60] = -2e6
	updateCfgEvent := &MarketMakingEvent{
		ID:           3,
		TimeStamp:    startTime + 2,
		UpdateConfig: updatedCfg,
	}
	db.storeEvent(startTime, mkt, updateCfgEvent, currBalanceState())

	check = func() error {
		runEvents, err := db.runOverview(startTime, mkt)
		if err != nil {
			return fmt.Errorf("error getting run events: %v", err)
		}
		if len(runEvents.Cfgs) != 2 {
			return fmt.Errorf("expected 2 cfgs, got %d", len(runEvents.Cfgs))
		}
		if !reflect.DeepEqual(runEvents.Cfgs[1].Cfg, updatedCfg) {
			return fmt.Errorf("expected updated cfg:\n%v\n\ngot:\n%v", spew.Sdump(updatedCfg), spew.Sdump(runEvents.Cfgs[1].Cfg))
		}
		if !reflect.DeepEqual(runEvents.Cfgs[0].Cfg, cfg) {
			return fmt.Errorf("expected original cfg:\n%v\n\ngot:\n%v", spew.Sdump(cfg), spew.Sdump(runEvents.Cfgs[0].Cfg))
		}
		return nil
	}

	tryWithTimeout(t, check)
}

func TestUpdateFinalBalanceDueToEventDiff(t *testing.T) {
	originalEvent := &MarketMakingEvent{
		ID: 1,
		BalanceEffects: &BalanceEffects{
			Settled: map[uint32]int64{
				42: 1e6,
				0:  2e6,
			},
			Locked: map[uint32]uint64{
				42: 3e6,
				0:  4e6,
			},
			Pending: map[uint32]uint64{
				42: 5e6,
				0:  6e6,
			},
			Reserved: map[uint32]uint64{
				42: 7e6,
				0:  8e6,
			},
		},
	}

	finalState := &BalanceState{
		FiatRates: map[uint32]float64{
			42: 20,
			60: 2500,
		},
		Balances: map[uint32]*BotBalance{
			42: {
				Available: 3e6,
				Locked:    3e6,
				Pending:   5e6,
				Reserved:  7e6,
			},
			0: {
				Available: 4e6,
				Locked:    4e6,
				Pending:   6e6,
				Reserved:  8e6,
			},
		},
	}

	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := newBoltEventLogDB(ctx, filepath.Join(dir, "event_log.db"), tLogger)
	if err != nil {
		t.Fatalf("error creating event log db: %v", err)
	}

	startTime := time.Now().Unix()
	mkt := &MarketWithHost{
		Host:    "dex.com",
		BaseID:  42,
		QuoteID: 60,
	}

	cfg := &BotConfig{}

	err = db.storeNewRun(startTime, mkt, cfg, finalState)
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}

	db.storeEvent(startTime, mkt, originalEvent, finalState)

	updatedEvent := &MarketMakingEvent{
		ID: 1,
		BalanceEffects: &BalanceEffects{
			Settled: map[uint32]int64{
				42: 1e6 + 100,
				0:  2e6 - 300,
			},
			Locked: map[uint32]uint64{
				42: 3e6 + 500,
				0:  4e6 + 200,
			},
			Pending: map[uint32]uint64{
				42: 5e6 - 100,
				0:  6e6 - 800,
			},
			Reserved: map[uint32]uint64{
				42: 0,
				0:  0,
			},
		},
	}

	db.storeEvent(startTime, mkt, updatedEvent, nil)

	expectedUpdatedFinalState := &BalanceState{
		FiatRates: map[uint32]float64{
			42: 20,
			60: 2500,
		},
		Balances: map[uint32]*BotBalance{
			42: {
				Available: 3e6 + 100,
				Locked:    3e6 + 500,
				Pending:   5e6 - 100,
				Reserved:  0,
			},
			0: {
				Available: 4e6 - 300,
				Locked:    4e6 + 200,
				Pending:   6e6 - 800,
				Reserved:  0,
			},
		},
	}

	checkFinalState := func() error {
		overview, err := db.runOverview(startTime, mkt)
		if err != nil {
			return fmt.Errorf("error getting final state: %v", err)
		}
		if !reflect.DeepEqual(overview.FinalState, expectedUpdatedFinalState) {
			return fmt.Errorf("expected final state:\n%v\n\ngot:\n%v", expectedUpdatedFinalState, finalState)
		}
		return nil
	}

	tryWithTimeout(t, checkFinalState)
}
