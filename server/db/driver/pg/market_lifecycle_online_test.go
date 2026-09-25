//go:build pgonline

package pg

import (
	"context"
	"reflect"
	"testing"

	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

func TestApplyMarketSuspendScheduledEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	const epochDur int64 = 10000
	running := &db.MarketLifecycle{
		Market:            "dcr_btc",
		State:             db.MarketStateRunning,
		StartEpochIdx:     10,
		StartEpochDur:     epochDur,
		ActiveEpochIdx:    15,
		ProcessedEpochIdx: 14,
		RunParams:         testMarketRunParams(),
	}
	seedMarketLifecycle(t, running)

	update := &db.MarketSuspendScheduledUpdate{
		Market:        running.Market,
		Base:          AssetDCR,
		Quote:         AssetBTC,
		FinalEpochIdx: 20,
		EpochDur:      epochDur,
		PersistBook:   false,
	}
	result, err := archie.ApplyMarketSuspendScheduledEvent(context.Background(), &db.EventLogMeta{Event: []byte("schedule")}, update)
	if err != nil {
		t.Fatalf("ApplyMarketSuspendScheduledEvent: %v", err)
	}

	if result.Log.Kind != meshevents.EventKindMarketSuspendScheduled {
		t.Fatalf("event kind = %q, want %q", result.Log.Kind, meshevents.EventKindMarketSuspendScheduled)
	}
	want := *running
	want.PendingAction = db.MarketPendingSuspend
	want.PendingEpochIdx = 20
	want.PendingEpochDur = epochDur
	want.FinalEpochIdx = 20
	want.FinalEpochDur = epochDur
	want.PersistBook = &update.PersistBook
	if !reflect.DeepEqual(result.Lifecycle, &want) {
		t.Fatalf("returned lifecycle = %+v, want %+v", result.Lifecycle, want)
	}

	stored, err := archie.MarketLifecycle(running.Market)
	if err != nil {
		t.Fatalf("MarketLifecycle: %v", err)
	}
	if !reflect.DeepEqual(stored, &want) {
		t.Fatalf("stored lifecycle = %+v, want %+v", stored, want)
	}
}
