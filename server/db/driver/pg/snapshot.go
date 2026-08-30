// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"github.com/lib/pq"
)

func init() {
	gob.Register([]byte(nil))
	gob.Register(time.Time{})
}

// snapshotTable is one relation in the dump, with an optional row filter.
type snapshotTable struct {
	schema string
	table  string
	where  string
}

func (st snapshotTable) qualified() string {
	return pq.QuoteIdentifier(st.schema) + "." + pq.QuoteIdentifier(st.table)
}

func (st snapshotTable) key() string {
	return st.schema + "." + st.table
}

// snapshotHistoryDays bounds archived history for client status reconcile.
const snapshotHistoryDays = 30

// snapshotTables: classSnapshot tables in public + configured market schemas,
// with retention filters. Live discovery; unclassified is an error.
func (a *Archiver) snapshotTables(ctx context.Context, tx *sql.Tx) ([]snapshotTable, error) {
	schemas := append([]string{publicSchema}, slices.Sorted(maps.Keys(a.markets))...)
	var tables []snapshotTable
	for _, schema := range schemas {
		cts, err := classifiedTables(ctx, tx, schema)
		if err != nil {
			return nil, err
		}
		for _, ct := range cts {
			if ct.class != classSnapshot {
				continue
			}
			tables = append(tables, snapshotTable{schema, ct.table, a.snapshotFilter(schema, ct.table)})
		}
	}
	return tables, nil
}

// snapshotFilter is the dump WHERE clause, or "" for a full dump.
func (a *Archiver) snapshotFilter(schema, table string) string {
	switch table {
	case marketLifecycleTableName:
		return lifecycleForConfiguredMarkets(a.markets)
	case ordersArchivedTableName:
		return archivedOrdersInSnapshot(schema)
	case cancelsArchivedTableName:
		return serverTimeInSnapshotHistory()
	case matchesTableName:
		return activeOrRecentMatches()
	case epochReportsTableName:
		return recentEpochReports(schema)
	}
	if strings.HasPrefix(table, candlesTableName+"_") {
		return lastCacheSizeCandles(schema, table)
	}
	return ""
}

// lifecycleForConfiguredMarkets filters market_lifecycle by market name.
func lifecycleForConfiguredMarkets(markets map[string]*dex.MarketInfo) string {
	if len(markets) == 0 {
		return "FALSE"
	}
	names := make([]string, 0, len(markets))
	for _, mkt := range markets {
		names = append(names, mkt.Name)
	}
	slices.Sort(names)
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = pq.QuoteLiteral(name)
	}
	return "market IN (" + strings.Join(quoted, ", ") + ")"
}

func qualifySchemaTable(schema, table string) string {
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
}

func serverTimeInSnapshotHistory() string {
	return fmt.Sprintf("server_time > now() - interval '%d days'", snapshotHistoryDays)
}

// matchEpochHorizonMs is now − snapshotHistoryDays in ms (epochIdx*epochDur).
func matchEpochHorizonMs() string {
	return fmt.Sprintf("(extract(epoch from now())*1000)::int8 - %d",
		int64(snapshotHistoryDays)*24*int64(time.Hour/time.Millisecond))
}

// archivedOrdersInSnapshot: snapshotted-match legs, cancel targets, or history window.
func archivedOrdersInSnapshot(schema string) string {
	return fmt.Sprintf("(%s) OR (%s) OR %s",
		oidsOfSnapshottedMatchOrders(schema),
		oidsOfArchivedCancelTargets(schema),
		serverTimeInSnapshotHistory())
}

// oidsOfSnapshottedMatchOrders pairs archived orders with the match dump set.
func oidsOfSnapshottedMatchOrders(schema string) string {
	matchesTable := qualifySchemaTable(schema, matchesTableName)
	matchSet := activeOrRecentMatches()
	return fmt.Sprintf(
		"oid IN (SELECT makerOrder FROM %s WHERE %s UNION SELECT takerOrder FROM %s WHERE %s)",
		matchesTable, matchSet, matchesTable, matchSet)
}

// oidsOfArchivedCancelTargets pairs targets with the cancels_archived window.
func oidsOfArchivedCancelTargets(schema string) string {
	return fmt.Sprintf(
		"oid IN (SELECT target_order FROM %s WHERE %s)",
		qualifySchemaTable(schema, cancelsArchivedTableName),
		serverTimeInSnapshotHistory())
}

func activeOrRecentMatches() string {
	return fmt.Sprintf("active OR epochIdx*epochDur > %s", matchEpochHorizonMs())
}

func recentEpochReports(schema string) string {
	return fmt.Sprintf("epoch_end > (SELECT coalesce(max(epoch_end),0) FROM %s) - %d",
		qualifySchemaTable(schema, epochReportsTableName), largestCandleBinMs())
}

// lastCacheSizeCandles keeps the newest candles.CacheSize rows.
func lastCacheSizeCandles(schema, table string) string {
	return fmt.Sprintf(
		"end_stamp >= (SELECT coalesce(min(end_stamp),0) FROM (SELECT end_stamp FROM %s ORDER BY end_stamp DESC LIMIT %d) latest)",
		qualifySchemaTable(schema, table), candles.CacheSize)
}

var largestCandleBinMs = sync.OnceValue(func() int64 {
	var largest time.Duration
	for _, bin := range candles.BinSizes {
		if dur, err := time.ParseDuration(bin); err == nil && dur > largest {
			largest = dur
		}
	}
	return largest.Milliseconds()
})

type snapshotTableDump struct {
	Schema  string
	Table   string
	Columns []string
	Rows    [][]interface{}
}

func (t snapshotTableDump) qualified() string {
	return pq.QuoteIdentifier(t.Schema) + "." + pq.QuoteIdentifier(t.Table)
}

func (t snapshotTableDump) key() string {
	return t.Schema + "." + t.Table
}

// pgSnapshot is the gob payload exchanged by WriteSnapshot and LoadSnapshot.
type pgSnapshot struct {
	FrontierSeq     uint64
	FrontierTipHash []byte
	Tables          []snapshotTableDump
}

func (s *pgSnapshot) frontier() *db.EventLogPosition {
	return &db.EventLogPosition{Seq: s.FrontierSeq, TipHash: s.FrontierTipHash}
}

// WriteSnapshot encodes a RR cut of active state and returns its event-log
// frontier. The read TX is closed before encode so w may do network I/O.
func (a *Archiver) WriteSnapshot(ctx context.Context, w io.Writer) (*db.EventLogPosition, error) {
	snap, err := a.buildSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err := gob.NewEncoder(w).Encode(snap); err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return snap.frontier(), nil
}

func (a *Archiver) buildSnapshot(ctx context.Context) (*pgSnapshot, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	frontier, err := a.eventLogFrontierTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read frontier: %w", err)
	}

	tables, err := a.snapshotTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	snap := &pgSnapshot{FrontierSeq: frontier.Seq, FrontierTipHash: frontier.TipHash}
	for _, st := range tables {
		dump, err := dumpSnapshotTable(ctx, tx, st)
		if err != nil {
			return nil, fmt.Errorf("dump %s.%s: %w", st.schema, st.table, err)
		}
		snap.Tables = append(snap.Tables, *dump)
	}

	return snap, nil
}

func dumpSnapshotTable(ctx context.Context, tx *sql.Tx, st snapshotTable) (*snapshotTableDump, error) {
	q := "SELECT * FROM " + st.qualified()
	if st.where != "" {
		q += " WHERE " + st.where
	}
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	dump := &snapshotTableDump{Schema: st.schema, Table: st.table, Columns: cols}
	for rows.Next() {
		row, err := scanInterfaceRow(rows, len(cols))
		if err != nil {
			return nil, err
		}
		dump.Rows = append(dump.Rows, row)
	}
	return dump, rows.Err()
}

func scanInterfaceRow(rows *sql.Rows, n int) ([]interface{}, error) {
	vals := make([]interface{}, n)
	ptrs := make([]interface{}, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	return vals, nil
}

// LoadSnapshot replaces active state from r and seeds the event-log frontier.
// Requires exact table-set match with snapshotTables and same-TX emptiness.
func (a *Archiver) LoadSnapshot(ctx context.Context, r io.Reader) (*db.EventLogPosition, error) {
	snap, err := decodePGSnapshot(r)
	if err != nil {
		return nil, err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	expected, err := a.snapshotTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := validateSnapshotTableSet(snap.Tables, expected); err != nil {
		return nil, err
	}

	// No event-sourced state in the same TX as the replace: never load over real data.
	empty, err := hasNoEventSourcedState(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("event-sourced state check: %w", err)
	}
	if !empty {
		return nil, fmt.Errorf("refusing to load a snapshot into a database with existing state")
	}

	if err := replaceTablesFromSnapshot(ctx, tx, snap.Tables); err != nil {
		return nil, err
	}
	frontier, err := a.seedEventLogAnchor(ctx, tx, snap.FrontierSeq, snap.FrontierTipHash)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit snapshot load: %w", err)
	}
	return frontier, nil
}

func decodePGSnapshot(r io.Reader) (*pgSnapshot, error) {
	var snap pgSnapshot
	if err := gob.NewDecoder(r).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

// validateSnapshotTableSet requires exactly the receiver's snapshotTables set.
func validateSnapshotTableSet(got []snapshotTableDump, expected []snapshotTable) error {
	want := make([]string, len(expected))
	for i, st := range expected {
		want[i] = st.key()
	}
	have := make([]string, len(got))
	for i, t := range got {
		have[i] = t.key()
	}
	slices.Sort(want)
	slices.Sort(have)
	if !slices.Equal(want, have) {
		return fmt.Errorf("snapshot table set mismatch: got %v, want %v", have, want)
	}
	return nil
}

func replaceTablesFromSnapshot(ctx context.Context, tx *sql.Tx, tables []snapshotTableDump) error {
	for _, t := range tables {
		if err := loadSnapshotTable(ctx, tx, t); err != nil {
			return fmt.Errorf("load %s.%s: %w", t.Schema, t.Table, err)
		}
	}
	return nil
}

// seedEventLogAnchor clears the log and inserts a frontier anchor.
func (a *Archiver) seedEventLogAnchor(ctx context.Context, tx *sql.Tx, seq uint64, tipHash []byte) (*db.EventLogPosition, error) {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s", a.tables.eventLog)); err != nil {
		return nil, fmt.Errorf("clear event log: %w", err)
	}
	frontier := &db.EventLogPosition{Seq: seq, TipHash: tipHash}
	if seq > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(internal.InsertEventLog, a.tables.eventLog),
			int64(seq), db.SnapshotAnchorKind, []byte{}, []byte{}, tipHash); err != nil {
			return nil, fmt.Errorf("seed frontier anchor: %w", err)
		}
	}
	return frontier, nil
}

func loadSnapshotTable(ctx context.Context, tx *sql.Tx, t snapshotTableDump) error {
	if _, err := tx.ExecContext(ctx, "TRUNCATE TABLE "+t.qualified()); err != nil {
		return err
	}
	if len(t.Rows) == 0 {
		return nil
	}
	if err := copyInRows(ctx, tx, t.Schema, t.Table, t.Columns, t.Rows); err != nil {
		return err
	}
	return syncSerialSequences(ctx, tx, t.Schema, t.Table)
}

func copyInRows(ctx context.Context, tx *sql.Tx, schema, table string, columns []string, rows [][]interface{}) error {
	stmt, err := tx.PrepareContext(ctx, pq.CopyInSchema(schema, table, columns...))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil { // flush COPY
		return err
	}
	return stmt.Close()
}

// syncSerialSequences fixes auto-increment counters after we COPY rows that
// already have ids. Postgres does not bump those counters during COPY, so the
// next plain INSERT could reuse an id and collide. Among snapshot tables only
// points.id uses a serial today; other tables have no work to do.
func syncSerialSequences(ctx context.Context, tx *sql.Tx, schema, table string) error {
	rel := qualifySchemaTable(schema, table)
	cols, err := listSerialColumns(ctx, tx, rel)
	if err != nil {
		return err
	}
	for _, cs := range cols {
		if err := advanceSerialSequence(ctx, tx, rel, cs); err != nil {
			return err
		}
	}
	return nil
}

// serialColumn is an auto-increment column and the sequence behind it.
type serialColumn struct {
	col string
	seq string
}

// listSerialColumns finds auto-increment columns on rel (e.g. "public.points")
// and the sequence each one uses. Returns nil if the table has none.
func listSerialColumns(ctx context.Context, tx *sql.Tx, rel string) ([]serialColumn, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.attname, pg_get_serial_sequence($1, a.attname)
		FROM pg_attribute a
		WHERE 
			a.attrelid = $1::regclass AND 
			a.attnum > 0 AND 
			NOT a.attisdropped AND
			pg_get_serial_sequence($1, a.attname) IS NOT NULL`, rel)
	if err != nil {
		return nil, fmt.Errorf("list serial sequences: %w", err)
	}
	defer rows.Close()

	var cols []serialColumn
	for rows.Next() {
		var cs serialColumn
		if err := rows.Scan(&cs.col, &cs.seq); err != nil {
			return nil, err
		}
		cols = append(cols, cs)
	}
	return cols, rows.Err()
}

// advanceSerialSequence points the sequence at one past the highest id already
// in the table (or 1 if the table is empty), so the next insert gets a free id.
func advanceSerialSequence(ctx context.Context, tx *sql.Tx, rel string, cs serialColumn) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		"SELECT setval($1, COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
		pq.QuoteIdentifier(cs.col), rel), cs.seq); err != nil {
		return fmt.Errorf("sync sequence %s: %w", cs.seq, err)
	}
	return nil
}
