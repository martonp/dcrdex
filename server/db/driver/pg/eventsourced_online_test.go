//go:build pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
)

// TestHasNoEventSourcedState: prepared DB is empty; any event-sourced row is not.
func TestHasNoEventSourcedState(t *testing.T) {
	ctx := context.Background()
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	assertEmpty := func(want bool, stage string) {
		t.Helper()
		empty, err := archie.HasNoEventSourcedState(ctx)
		if err != nil {
			t.Fatalf("%s: HasNoEventSourcedState: %v", stage, err)
		}
		if empty != want {
			t.Fatalf("%s: HasNoEventSourcedState = %v, want %v", stage, empty, want)
		}
	}

	assertEmpty(true, "prepared but unused")

	// A public projection row.
	points := archie.tables.points
	if _, err := archie.db.Exec(fmt.Sprintf(
		"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1)",
		points), []byte{0x01}, []byte{0x02}); err != nil {
		t.Fatalf("insert point: %v", err)
	}
	assertEmpty(false, "with a points row")
	if _, err := archie.db.Exec("TRUNCATE TABLE " + points + " RESTART IDENTITY"); err != nil {
		t.Fatalf("clear points: %v", err)
	}
	assertEmpty(true, "points cleared")

	// A market-schema row.
	lo := newLimitOrder(false, 4_500_000, 1, order.StandingTiF, 0)
	if err := storeOrderForTest(archie, lo, 12345, int64(EpochDuration), order.OrderStatusBooked); err != nil {
		t.Fatalf("StoreOrder: %v", err)
	}
	assertEmpty(false, "with a booked order")
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	assertEmpty(true, "order cleared")

	// An event-log row, with every projection empty.
	if _, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: []byte{0xbe}}, "es_test",
		[]byte{0xbe}, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("applyEventTx: %v", err)
	}
	assertEmpty(false, "with an event-log row")

	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables (cleanup): %v", err)
	}
}

// TestLoadSnapshotRefusesNonEmpty: non-empty target fails without modification.
func TestLoadSnapshotRefusesNonEmpty(t *testing.T) {
	ctx := context.Background()
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Sender-side state with a non-zero frontier.
	points := archie.tables.points
	if _, err := archie.db.Exec(fmt.Sprintf(
		"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1)",
		points), []byte{0xaa}, []byte{0xbb}); err != nil {
		t.Fatalf("insert point: %v", err)
	}
	if _, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: []byte{0x01}}, "es_test",
		[]byte{0x01}, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("applyEventTx: %v", err)
	}

	var buf bytes.Buffer
	if _, err := archie.WriteSnapshot(ctx, &buf); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	// The same database still holds that state, so the load must refuse.
	if _, err := archie.LoadSnapshot(ctx, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatalf("LoadSnapshot into a non-empty database did not error")
	} else if !strings.Contains(err.Error(), "existing state") {
		t.Fatalf("LoadSnapshot error %q is not the emptiness refusal", err)
	}

	// The refused load must not have modified anything: the point row survives
	// and the frontier is unchanged.
	var n int
	if err := archie.db.QueryRow("SELECT count(*) FROM " + points).Scan(&n); err != nil {
		t.Fatalf("count points: %v", err)
	}
	if n != 1 {
		t.Fatalf("points rows after refused load = %d, want 1", n)
	}
	frontier, err := archie.EventLogFrontier(ctx)
	if err != nil {
		t.Fatalf("EventLogFrontier: %v", err)
	}
	if frontier.Seq != 1 {
		t.Fatalf("frontier seq after refused load = %d, want 1", frontier.Seq)
	}

	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables (cleanup): %v", err)
	}
}
