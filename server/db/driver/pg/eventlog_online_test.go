//go:build pgonline

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

func resetEventLog(t *testing.T) {
	t.Helper()
	if _, err := archie.db.Exec(fmt.Sprintf("TRUNCATE %s;", archie.tables.eventLog)); err != nil {
		t.Fatalf("truncate event log: %v", err)
	}
}

func assertEventLogFrontier(t *testing.T, wantSeq uint64, wantTip []byte) {
	t.Helper()
	frontier, err := archie.EventLogFrontier(context.Background())
	if err != nil {
		t.Fatalf("EventLogFrontier error: %v", err)
	}
	if frontier.Seq != wantSeq || !bytes.Equal(frontier.TipHash, wantTip) {
		t.Fatalf("frontier = (%d, %x), want (%d, %x)",
			frontier.Seq, frontier.TipHash, wantSeq, wantTip)
	}
}

func appendEventLogEntry(entry *db.EventLogEntry) (*db.EventLogEntry, error) {
	ctx := context.Background()
	tx, err := archie.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	stored, err := archie.appendEventLog(ctx, tx, &eventLogAppendRequest{entry: entry})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

func appendEventLogInTx(t *testing.T, appendReq *eventLogAppendRequest) (*db.EventLogEntry, error) {
	t.Helper()
	tx, err := archie.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx error: %v", err)
	}
	entry, err := archie.appendEventLog(context.Background(), tx, appendReq)
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			t.Fatalf("Rollback error after append failure %v: %v", err, rollbackErr)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return entry, nil
}

func TestEventLogAppendAndRead(t *testing.T) {
	resetEventLog(t)

	first, err := appendEventLogEntry(&db.EventLogEntry{
		Kind:   meshevents.EventKindBondPosted,
		Event:  []byte("one"),
		TxData: []byte(`{"n":1}`),
	})
	if err != nil {
		t.Fatalf("appendEventLog first error: %v", err)
	}
	if first.Seq != 1 || len(first.TipHash) != db.EventLogTipHashSize {
		t.Fatalf("first entry = %+v", first)
	}

	second, err := appendEventLogEntry(&db.EventLogEntry{
		Kind:   meshevents.EventKindOrderAccepted,
		Event:  []byte("two"),
		TxData: []byte(`{"n":2}`),
	})
	if err != nil {
		t.Fatalf("appendEventLog second error: %v", err)
	}
	if second.Seq != 2 || bytes.Equal(first.TipHash, second.TipHash) {
		t.Fatalf("second entry = %+v", second)
	}
	assertEventLogFrontier(t, 2, second.TipHash)

	entries, err := archie.EventLogEntriesAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("EventLogEntriesAfter error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("EventLogEntriesAfter returned %d entries, want 2", len(entries))
	}
	if entries[0].Seq != first.Seq || entries[0].Kind != first.Kind ||
		!bytes.Equal(entries[0].Event, first.Event) ||
		!bytes.Equal(entries[0].TxData, first.TxData) ||
		!bytes.Equal(entries[0].TipHash, first.TipHash) {
		t.Fatalf("first loaded entry = %+v, want %+v", entries[0], first)
	}
	if entries[1].Seq != second.Seq || entries[1].Kind != second.Kind ||
		!bytes.Equal(entries[1].Event, second.Event) ||
		!bytes.Equal(entries[1].TxData, second.TxData) ||
		!bytes.Equal(entries[1].TipHash, second.TipHash) {
		t.Fatalf("second loaded entry = %+v, want %+v", entries[1], second)
	}

	entries, err = archie.EventLogEntriesAfter(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("EventLogEntriesAfter limited error: %v", err)
	}
	if len(entries) != 1 || entries[0].Seq != 2 {
		t.Fatalf("limited entries = %+v, want only seq 2", entries)
	}
}

func TestEventLogConcurrentAppend(t *testing.T) {
	resetEventLog(t)

	const n = 8
	errs := make(chan error, n)
	seqs := make(chan uint64, n)
	for i := range n {
		go func(i int) {
			stored, err := appendEventLogEntry(&db.EventLogEntry{
				Kind:  meshevents.EventKindBondPosted,
				Event: []byte(fmt.Sprintf("event-%d", i)),
			})
			if err != nil {
				errs <- err
				return
			}
			seqs <- stored.Seq
			errs <- nil
		}(i)
	}

	var got []uint64
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("appendEventLog error: %v", err)
		}
		got = append(got, <-seqs)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i, seq := range got {
		want := uint64(i + 1)
		if seq != want {
			t.Fatalf("seq[%d] = %d, want %d (all seqs %v)", i, seq, want, got)
		}
	}
}

func TestEventLogRejectsSequenceAndExpectedTip(t *testing.T) {
	resetEventLog(t)

	first, err := appendEventLogEntry(&db.EventLogEntry{
		Kind:  meshevents.EventKindBondPosted,
		Event: []byte("one"),
	})
	if err != nil {
		t.Fatalf("appendEventLog first error: %v", err)
	}

	_, err = appendEventLogEntry(&db.EventLogEntry{
		Seq:   3,
		Kind:  meshevents.EventKindBondPosted,
		Event: []byte("gap"),
	})
	if err == nil {
		t.Fatalf("appendEventLog with skipped seq succeeded")
	}
	assertEventLogFrontier(t, 1, first.TipHash)

	wrongTip := bytes.Repeat([]byte{0x01}, db.EventLogTipHashSize)
	_, err = appendEventLogInTx(t, &eventLogAppendRequest{
		entry: &db.EventLogEntry{
			Seq:    2,
			Kind:   meshevents.EventKindBondPosted,
			Event:  []byte("two"),
			TxData: []byte("tx-two"),
		},
		expectedTipHash: wrongTip,
	})
	if err == nil {
		t.Fatalf("appendEventLog with wrong expected tip succeeded")
	}
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("appendEventLog error = %T %[1]v, want EventLogDivergenceError", err)
	}
	if divergence.Seq != 2 || !bytes.Equal(divergence.ExpectedTipHash, wrongTip) {
		t.Fatalf("event log divergence = %+v, want seq 2 and expected tip %x", divergence, wrongTip)
	}
	assertEventLogFrontier(t, 1, first.TipHash)

	expectedTip := eventLogHash(first.TipHash, 2, meshevents.EventKindBondPosted, []byte("two"), []byte("tx-two"))
	second, err := appendEventLogInTx(t, &eventLogAppendRequest{
		entry: &db.EventLogEntry{
			Seq:    2,
			Kind:   meshevents.EventKindBondPosted,
			Event:  []byte("two"),
			TxData: []byte("tx-two"),
		},
		expectedTipHash: expectedTip,
	})
	if err != nil {
		t.Fatalf("appendEventLog with correct expected tip error: %v", err)
	}
	assertEventLogFrontier(t, 2, second.TipHash)
}
