// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
)

func writeHashBytes(h interface{ Write([]byte) (int, error) }, b []byte) {
	var lenB [8]byte
	binary.BigEndian.PutUint64(lenB[:], uint64(len(b)))
	h.Write(lenB[:])
	h.Write(b)
}

type eventLogAppendRequest struct {
	entry           *db.EventLogEntry
	expectedTipHash []byte
}

func newEventLogAppend(meta *db.EventLogMeta, kind string, txData []byte) (*eventLogAppendRequest, error) {
	if meta == nil {
		return nil, fmt.Errorf("nil event log metadata")
	}
	var expectedTipHash []byte
	if meta.ExpectedTipHash != nil {
		expectedTipHash = append([]byte{}, meta.ExpectedTipHash...)
	}
	return &eventLogAppendRequest{
		entry: &db.EventLogEntry{
			Seq:    meta.Seq,
			Kind:   kind,
			Event:  append([]byte(nil), meta.Event...),
			TxData: append([]byte(nil), txData...),
		},
		expectedTipHash: expectedTipHash,
	}, nil
}

func eventLogHash(prev []byte, seq uint64, kind string, event, txData []byte) []byte {
	h := sha256.New()
	writeHashBytes(h, prev)
	var seqB [8]byte
	binary.BigEndian.PutUint64(seqB[:], seq)
	h.Write(seqB[:])
	writeHashBytes(h, []byte(kind))
	writeHashBytes(h, event)
	writeHashBytes(h, txData)
	return h.Sum(nil)
}

func scanEventLogFrontier(row *sql.Row) (*db.EventLogPosition, error) {
	var seq int64
	var tipHash []byte
	err := row.Scan(&seq, &tipHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return &db.EventLogPosition{}, nil
		}
		return nil, err
	}
	return &db.EventLogPosition{
		Seq:     uint64(seq),
		TipHash: tipHash,
	}, nil
}

func (a *Archiver) eventLogFrontierTx(ctx context.Context, tx *sql.Tx) (*db.EventLogPosition, error) {
	stmt := fmt.Sprintf(internal.SelectEventLogFrontier, a.tables.eventLog)
	return scanEventLogFrontier(tx.QueryRowContext(ctx, stmt))
}

func (a *Archiver) appendEventLog(ctx context.Context, tx *sql.Tx, appendReq *eventLogAppendRequest) (*db.EventLogEntry, error) {
	if appendReq == nil || appendReq.entry == nil {
		return nil, fmt.Errorf("nil event log entry")
	}
	entry := appendReq.entry
	if entry.Seq > math.MaxInt64 {
		return nil, fmt.Errorf("event log seq %d overflows int64", entry.Seq)
	}
	if appendReq.expectedTipHash != nil && entry.Seq == 0 {
		return nil, fmt.Errorf("expected event log tip hash requires non-zero seq")
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(internal.LockEventLog, a.tables.eventLog)); err != nil {
		return nil, err
	}

	prev, err := a.eventLogFrontierTx(ctx, tx)
	if err != nil {
		return nil, err
	}

	if prev.Seq != 0 && len(prev.TipHash) != db.EventLogTipHashSize {
		return nil, fmt.Errorf("event log previous tip hash length %d, want %d", len(prev.TipHash), db.EventLogTipHashSize)
	}
	if prev.Seq == math.MaxInt64 {
		return nil, fmt.Errorf("event log seq %d overflows int64", prev.Seq+1)
	}

	nextSeq := prev.Seq + 1
	seq := entry.Seq
	if seq == 0 {
		seq = nextSeq
	} else if seq != nextSeq {
		return nil, fmt.Errorf("event log seq mismatch: got %d after %d", seq, prev.Seq)
	}

	cpy := &db.EventLogEntry{
		Seq:    seq,
		Kind:   entry.Kind,
		Event:  append([]byte{}, entry.Event...),
		TxData: append([]byte{}, entry.TxData...),
	}
	cpy.TipHash = eventLogHash(prev.TipHash, cpy.Seq, cpy.Kind, cpy.Event, cpy.TxData)

	if appendReq.expectedTipHash != nil {
		if !bytes.Equal(appendReq.expectedTipHash, cpy.TipHash) {
			return nil, &db.EventLogDivergenceError{
				Seq:             cpy.Seq,
				ExpectedTipHash: append([]byte(nil), appendReq.expectedTipHash...),
				ActualTipHash:   append([]byte(nil), cpy.TipHash...),
				Err:             fmt.Errorf("event log tip hash mismatch"),
			}
		}
	}

	stmt := fmt.Sprintf(internal.InsertEventLog, a.tables.eventLog)
	if _, err := tx.ExecContext(ctx, stmt, int64(cpy.Seq), cpy.Kind, cpy.Event, cpy.TxData, cpy.TipHash); err != nil {
		return nil, err
	}

	return cpy, nil
}

func eventTxInterrupted(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	return ctxErr != nil && (errors.Is(err, ctxErr) || errors.Is(err, sql.ErrTxDone))
}

// commitEventTx wraps commit errors whose outcome is unknown. Cancellation
// before Commit starts is known not to have landed.
func commitEventTx(ctx context.Context, tx *sql.Tx) error {
	err := tx.Commit()
	if err == nil {
		return nil
	}
	if eventTxInterrupted(ctx, err) {
		return fmt.Errorf("apply interrupted before commit: %w", err)
	}
	return &db.EventCommitUnknownError{Err: err}
}

// applyEventTx commits apply + event log. For reputation outcomes use
// applyRepEventTx; other rep-input writers must call notifyRepInputsOnCommit after.
func (a *Archiver) applyEventTx(ctx context.Context, meta *db.EventLogMeta, kind string, txData []byte, apply func(*sql.Tx) error) (logEntry *db.EventLogEntry, err error) {
	eventLog, err := newEventLogAppend(meta, kind, txData)
	if err != nil {
		return nil, err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		if !eventTxInterrupted(ctx, err) {
			a.fatalBackendErr(err)
		}
		return nil, err
	}
	defer func() {
		if err == nil || errors.Is(err, sql.ErrTxDone) {
			return
		}
		if errRollback := tx.Rollback(); errRollback != nil {
			log.Errorf("Rollback failed: %v", errRollback)
		}
	}()

	if err = apply(tx); err != nil {
		return nil, err
	}
	logEntry, err = a.appendEventLog(ctx, tx, eventLog)
	if err != nil {
		return nil, err
	}
	if err = commitEventTx(ctx, tx); err != nil {
		if !eventTxInterrupted(ctx, err) {
			a.fatalBackendErr(err)
		}
		return nil, err
	}
	return logEntry, nil
}

// EventLogFrontier queries the database for the current event log frontier.
func (a *Archiver) EventLogFrontier(ctx context.Context) (*db.EventLogPosition, error) {
	stmt := fmt.Sprintf(internal.SelectEventLogFrontier, a.tables.eventLog)
	return scanEventLogFrontier(a.db.QueryRowContext(ctx, stmt))
}

// EventLogEntriesAfter queries the database for event log entries after a
// given sequence number.
func (a *Archiver) EventLogEntriesAfter(ctx context.Context, after uint64, limit int) ([]*db.EventLogEntry, error) {
	if after > math.MaxInt64 {
		return nil, fmt.Errorf("event log seq %d overflows int64", after)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("event log limit must be positive")
	}
	stmt := fmt.Sprintf(internal.SelectEventLogSince, a.tables.eventLog)
	rows, err := a.db.QueryContext(ctx, stmt, int64(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*db.EventLogEntry
	for rows.Next() {
		var seq int64
		entry := new(db.EventLogEntry)
		if err := rows.Scan(&seq, &entry.Kind, &entry.Event, &entry.TxData, &entry.TipHash); err != nil {
			return nil, err
		}
		entry.Seq = uint64(seq)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
