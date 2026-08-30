//go:build !pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"decred.org/dcrdex/server/db"
)

func TestCommitEventTxCanceled(t *testing.T) {
	testDB, err := sql.Open("stub", "")
	if err != nil {
		t.Fatal(err)
	}
	defer testDB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	err = commitEventTx(ctx, tx)
	var unknown *db.EventCommitUnknownError
	if err == nil || errors.As(err, &unknown) {
		t.Fatalf("commit error = %T %[1]v, want known cancellation", err)
	}
	if eventTxInterrupted(ctx, errors.New("driver failure")) {
		t.Fatal("unrelated driver error classified as cancellation")
	}
}
