//go:build pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"decred.org/dcrdex/server/db"
)

func loadDBSnap(t *testing.T, file string) {
	t.Helper()
	pgSnapGZ, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer pgSnapGZ.Close()

	r, err := gzip.NewReader(pgSnapGZ)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, strings.TrimSuffix(file, ".gz"))
	dbFile, err := os.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dbPath)

	_, err = io.Copy(dbFile, r)
	dbFile.Close()
	if err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	cmd := exec.Command("psql", "-U", PGTestsUser, "-h", PGTestsHost, "-d", PGTestsDBName, "-a", "-f", dbPath)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		t.Fatalf("psql failed: %v / output: %+v\n: %+v", err, out.String(), stderr.String())
	}
}

func Test_upgradeDB(t *testing.T) {
	ctx := context.Background()

	tryUpgrade := func(gzFile string) error {
		log.Info("start")
		// Get a clean slate.
		err := nukeAll(archie.db)
		if err != nil {
			return fmt.Errorf("nukeAll: %w", err)
		}

		// Import the data into the test db.
		loadDBSnap(t, gzFile)

		// Run the upgrades.
		err = upgradeDB(ctx, archie.db)
		if err != nil {
			return fmt.Errorf("upgradeDB: %w", err)
		}
		if err := assertNoArchivedCommitUnique(archie.db); err != nil {
			return fmt.Errorf("after upgrade of %s: %w", gzFile, err)
		}
		if err := assertArchivedCommitIndexes(archie.db); err != nil {
			return fmt.Errorf("after upgrade of %s: %w", gzFile, err)
		}
		found, err := columnExists(archie.db, publicSchema, accountsTableName, "fee_asset")
		if err != nil {
			return fmt.Errorf("check accounts.fee_asset after upgrade of %s: %w", gzFile, err)
		}
		if found {
			return fmt.Errorf("accounts.fee_asset still exists after upgrade of %s", gzFile)
		}
		return nil
	}

	// These are all positive path tests (no corruption in DB).
	snaps := []string{
		"dcrdex_test_db_v0-master.sql.gz",              // v0 DB with no meta table
		"dcrdex_test_db_v0-release-0.1.sql.gz",         // v0 DB with a meta table with state_hash from release-0.1
		"dcrdex_test_db_v0-release-0.1-matches.sql.gz", // v0 with meta table and a ton of matches for v2 upgrade
	}

	for _, snap := range snaps {
		if err := tryUpgrade(snap); err != nil {
			t.Errorf("upgrade of DB snapshot %q failed: %v", snap, err)
		}
	}

	// Try with canceled context.
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	cancel()
	// To try hitting the Exec/Query/etc. errors, attempt to cancel mid upgrade:
	// go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	err := tryUpgrade(snaps[2]) // the bigger upgrade
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("wrong error for canceled context, got %v", err)
	}
	t.Logf("Got an expected error: %v", err)
	// NOTE: That was a very limited test of cancellation as the transaction
	// wasn't even started and thus was not rolled back, but it does stop the
	// upgrade chain cleanly and there should be no schema_version.
	_, err = DBVersion(archie.db)
	if err == nil {
		t.Errorf("expected error from DBVersion with no meta table")
	}
}

// TestStampMeshGenesis: virgin stays at seq 0; trading state gets seq-1 genesis;
// re-stamp is a no-op; next event chains; two stamps get different tips.
func TestStampMeshGenesis(t *testing.T) {
	ctx := context.Background()
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	stamp := func() {
		t.Helper()
		tx, err := archie.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if err := stampMeshGenesis(tx); err != nil {
			tx.Rollback()
			t.Fatalf("stampMeshGenesis: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	frontier := func(stage string) *db.EventLogPosition {
		t.Helper()
		pos, err := archie.EventLogFrontier(ctx)
		if err != nil {
			t.Fatalf("%s: EventLogFrontier: %v", stage, err)
		}
		return pos
	}
	addPoint := func() {
		t.Helper()
		if _, err := archie.db.Exec(fmt.Sprintf(
			"INSERT INTO %s (account, link, class, outcome) VALUES ($1, $2, 1, 1)",
			archie.tables.points), []byte{0x0a}, []byte{0x0b}); err != nil {
			t.Fatalf("insert point: %v", err)
		}
	}

	// A virgin database is left virgin: it must stay seedable as a slave.
	stamp()
	if pos := frontier("virgin"); pos.Seq != 0 {
		t.Fatalf("genesis stamped on a virgin database: frontier %s", pos)
	}

	// Trading state present: exactly one genesis row at seq 1.
	addPoint()
	stamp()
	pos := frontier("stamped")
	if pos.Seq != 1 {
		t.Fatalf("frontier after stamp = %s, want seq 1", pos)
	}

	// The row is chain-valid: recomputing the hash from the stored fields
	// reproduces the tip, so appendEventLog's verification math holds.
	var kind string
	var payload, txData, tipHash []byte
	if err := archie.db.QueryRow(fmt.Sprintf(
		"SELECT kind, event, tx_data, tip_hash FROM %s WHERE seq = 1",
		archie.tables.eventLog)).Scan(&kind, &payload, &txData, &tipHash); err != nil {
		t.Fatalf("read genesis row: %v", err)
	}
	if kind != db.MeshGenesisKind {
		t.Fatalf("genesis kind = %q, want %q", kind, db.MeshGenesisKind)
	}
	if want := eventLogHash(nil, 1, kind, payload, txData); !bytes.Equal(tipHash, want) {
		t.Fatalf("genesis tip %x is not the chain hash of its own row (%x)", tipHash, want)
	}
	firstTip := append([]byte{}, tipHash...)

	// Re-stamping is a no-op.
	stamp()
	if pos := frontier("re-stamped"); pos.Seq != 1 || !bytes.Equal(pos.TipHash, firstTip) {
		t.Fatalf("re-stamp moved the frontier to %s", pos)
	}

	// A real event chains from the genesis tip.
	entry, err := archie.applyEventTx(ctx, &db.EventLogMeta{Event: []byte{0x01}}, "genesis_test",
		[]byte{0x01}, func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("applyEventTx after genesis: %v", err)
	}
	if entry.Seq != 2 {
		t.Fatalf("first post-genesis event seq = %d, want 2", entry.Seq)
	}
	if want := eventLogHash(firstTip, 2, entry.Kind, entry.Event, entry.TxData); !bytes.Equal(entry.TipHash, want) {
		t.Fatalf("post-genesis event does not chain from the genesis tip")
	}

	// An independently stamped database gets a distinct tip: two upgraded
	// legacy servers must meet as divergence, not equality.
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables (second database): %v", err)
	}
	addPoint()
	stamp()
	if pos := frontier("second database"); pos.Seq != 1 || bytes.Equal(pos.TipHash, firstTip) {
		t.Fatalf("second database's genesis tip %s is not distinct from the first (%x)", pos, firstTip)
	}

	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables (cleanup): %v", err)
	}
}
