//go:build pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
)

// TestWipeEventSourcedState: wipe clears event-sourced rows (incl. historical),
// keeps markets/meta, and a fresh event chain starts at seq 1.
func TestWipeEventSourcedState(t *testing.T) {
	ctx := context.Background()
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Public projection, historical market projection (archive is not in the
	// active-state snapshot), and an event-log row.
	if _, err := archie.db.Exec(fmt.Sprintf(
		"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1)",
		archie.tables.points), []byte{0xaa}, []byte{0xbb}); err != nil {
		t.Fatalf("insert point: %v", err)
	}
	lo := newLimitOrder(true, 4_900_000, 1, order.StandingTiF, 10)
	if err := storeOrderForTest(archie, lo, 12345, int64(EpochDuration), order.OrderStatusExecuted); err != nil {
		t.Fatalf("StoreOrder: %v", err)
	}
	if _, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: []byte("e1")}, "wipe_test",
		[]byte("e1"), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("applyEventTx: %v", err)
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := archie.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	marketsBefore, metaBefore := count("markets"), count("meta")
	if marketsBefore == 0 || metaBefore == 0 {
		t.Fatalf("expected config rows: markets=%d meta=%d", marketsBefore, metaBefore)
	}

	if err := archie.WipeEventSourcedState(ctx); err != nil {
		t.Fatalf("WipeEventSourcedState: %v", err)
	}

	empty, err := archie.HasNoEventSourcedState(ctx)
	if err != nil {
		t.Fatalf("HasNoEventSourcedState: %v", err)
	}
	if !empty {
		t.Fatal("event-sourced state remains after wipe")
	}
	if n := count("markets"); n != marketsBefore {
		t.Fatalf("markets rows = %d after wipe, want %d", n, marketsBefore)
	}
	if n := count("meta"); n != metaBefore {
		t.Fatalf("meta rows = %d after wipe, want %d", n, metaBefore)
	}

	entry, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: []byte("fresh")}, "wipe_test",
		[]byte("fresh"), func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("applyEventTx after wipe: %v", err)
	}
	if entry.Seq != 1 {
		t.Fatalf("first post-wipe event seq = %d, want 1", entry.Seq)
	}
}
