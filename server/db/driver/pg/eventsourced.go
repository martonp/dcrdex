// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// eventSourcedTables: public + markets-table schemas, minus classConfig.
// Live discovery; unclassified tables are an error.
func eventSourcedTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	mkts, err := loadMarkets(tx, marketsTableName)
	if err != nil {
		return nil, fmt.Errorf("load markets: %w", err)
	}
	schemas := make([]string, 0, len(mkts)+1)
	for _, mkt := range mkts {
		schemas = append(schemas, marketSchema(mkt.Name))
	}
	slices.Sort(schemas)
	schemas = append([]string{publicSchema}, schemas...)

	var tables []string
	for _, schema := range schemas {
		cts, err := classifiedTables(ctx, tx, schema)
		if err != nil {
			return nil, err
		}
		for _, ct := range cts {
			if ct.class == classConfig {
				continue
			}
			tables = append(tables, qualifySchemaTable(schema, ct.table))
		}
	}
	return tables, nil
}

// hasNoEventSourcedState is true if every event-sourced table has no rows.
func hasNoEventSourcedState(ctx context.Context, tx *sql.Tx) (bool, error) {
	tables, err := eventSourcedTables(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, tbl := range tables {
		var hasRows bool
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s)", tbl)).Scan(&hasRows); err != nil {
			return false, fmt.Errorf("event-sourced state probe of %s: %w", tbl, err)
		}
		if hasRows {
			return false, nil
		}
	}
	return true, nil
}

// HasNoEventSourcedState implements db.EventSourcedStateChecker.
func (a *Archiver) HasNoEventSourcedState(ctx context.Context) (bool, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return hasNoEventSourcedState(ctx, tx)
}

// WipeEventSourcedState implements db.EventSourcedStateChecker.
func (a *Archiver) WipeEventSourcedState(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Same table list as hasNoEventSourcedState (incl. decommissioned markets).
	tables, err := eventSourcedTables(ctx, tx)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, "TRUNCATE TABLE "+strings.Join(tables, ", ")+" RESTART IDENTITY"); err != nil {
		return fmt.Errorf("truncate event-sourced tables: %w", err)
	}
	return tx.Commit()
}
