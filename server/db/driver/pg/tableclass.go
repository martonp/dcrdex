// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Mesh table treatment for snapshot/join. Unclassified tables fail at boot,
// WriteSnapshot, and LoadSnapshot.

// tableClass is how mesh treats one base table.
type tableClass int

const (
	// classSnapshot: dumped by WriteSnapshot; must be empty on LoadSnapshot.
	classSnapshot tableClass = iota + 1
	// classChecked: empty on LoadSnapshot; not dumped (epochs; event log via frontier).
	classChecked
	// classConfig: not event-sourced; may retain rows (markets, meta, fee_keys).
	classConfig
)

// Pre-bonds fee key index: still on upgraded DBs, absent on fresh ones.
// classConfig so upgraded senders do not fail table-set validation on fresh receivers.
const legacyFeeKeysTableName = "fee_keys"

var publicTableClasses = map[string]tableClass{
	accountsTableName:        classSnapshot,
	bondsTableName:           classSnapshot,
	prepaidBondsTableName:    classSnapshot,
	pointsTableName:          classSnapshot,
	marketLifecycleTableName: classSnapshot,
	eventLogTableName:        classChecked,
	marketsTableName:         classConfig,
	metaTableName:            classConfig,
	legacyFeeKeysTableName:   classConfig,
}

var marketTableClasses = map[string]tableClass{
	ordersActiveTableName:    classSnapshot,
	ordersArchivedTableName:  classSnapshot,
	cancelsActiveTableName:   classSnapshot,
	cancelsArchivedTableName: classSnapshot,
	matchesTableName:         classSnapshot,
	epochReportsTableName:    classSnapshot,
	epochsTableName:          classChecked,
	// candles_<bin> tables match by prefix in classifyTable.
}

func classifyTable(schema, table string) (tableClass, error) {
	if schema == publicSchema {
		if class, ok := publicTableClasses[table]; ok {
			return class, nil
		}
	} else {
		if class, ok := marketTableClasses[table]; ok {
			return class, nil
		}
		if strings.HasPrefix(table, candlesTableName+"_") {
			return classSnapshot, nil
		}
	}
	return 0, fmt.Errorf("table %s.%s has no mesh snapshot classification; "+
		"add it to publicTableClasses or marketTableClasses in tableclass.go",
		schema, table)
}

type classifiedTable struct {
	table string
	class tableClass
}

// classifiedTables lists schema's base tables (by name) with their classes.
func classifiedTables(ctx context.Context, tx *sql.Tx, schema string) ([]classifiedTable, error) {
	names, err := listBaseTables(ctx, tx, schema)
	if err != nil {
		return nil, err
	}
	tables := make([]classifiedTable, 0, len(names))
	for _, name := range names {
		class, err := classifyTable(schema, name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, classifiedTable{table: name, class: class})
	}
	return tables, nil
}

func listBaseTables(ctx context.Context, tx *sql.Tx, schema string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
			WHERE table_schema = $1 AND table_type = 'BASE TABLE'
			ORDER BY table_name`, schema)
	if err != nil {
		return nil, fmt.Errorf("list %s tables: %w", schema, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// verifyTableClassification fails if any public or markets-table schema table
// is missing from the registry.
func (a *Archiver) verifyTableClassification(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = eventSourcedTables(ctx, tx)
	return err
}
