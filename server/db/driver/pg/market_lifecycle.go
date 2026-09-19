// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
)

// MarketLifecycle returns the stored market lifecycle, or nil if none exists.
func (a *Archiver) MarketLifecycle(market string) (*db.MarketLifecycle, error) {
	lc, err := scanMarketLifecycle(a.db.QueryRow(fmt.Sprintf(internal.SelectMarketLifecycle, a.tables.marketLifecycle), market))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return lc, err
}

func (a *Archiver) marketLifecycleForUpdate(tx *sql.Tx, market string) (*db.MarketLifecycle, error) {
	lc, err := scanMarketLifecycle(tx.QueryRow(fmt.Sprintf(internal.SelectMarketLifecycleForUpdate, a.tables.marketLifecycle), market))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return lc, err
}

func scanMarketLifecycle(row *sql.Row) (*db.MarketLifecycle, error) {
	lc := new(db.MarketLifecycle)
	err := row.Scan(&lc.Market, &lc.State, &lc.StartEpochIdx, &lc.StartEpochDur,
		&lc.FinalEpochIdx, &lc.FinalEpochDur, &lc.PendingAction,
		&lc.PendingEpochIdx, &lc.PendingEpochDur, &lc.PersistBook, &lc.ActiveEpochIdx,
		&lc.ProcessedEpochIdx,
		&lc.RunParams.LotSize, &lc.RunParams.RateStep, &lc.RunParams.ParcelSize,
		&lc.RunParams.MaxUserCancelsPerEpoch, &lc.RunParams.MinimumRate)
	if err != nil {
		return nil, err
	}
	return lc, nil
}

func (a *Archiver) upsertMarketLifecycleTx(tx *sql.Tx, lc *db.MarketLifecycle) error {
	if lc == nil {
		return fmt.Errorf("nil market lifecycle")
	}
	_, err := tx.Exec(fmt.Sprintf(internal.UpsertMarketLifecycle, a.tables.marketLifecycle),
		lc.Market, lc.State, lc.StartEpochIdx, lc.StartEpochDur,
		lc.FinalEpochIdx, lc.FinalEpochDur, lc.PendingAction,
		lc.PendingEpochIdx, lc.PendingEpochDur, lc.PersistBook,
		lc.ActiveEpochIdx, lc.ProcessedEpochIdx, lc.RunParams.LotSize,
		lc.RunParams.RateStep, lc.RunParams.ParcelSize,
		lc.RunParams.MaxUserCancelsPerEpoch, lc.RunParams.MinimumRate)
	return err
}

func (a *Archiver) updateMarketLifecycleTx(tx *sql.Tx, lc *db.MarketLifecycle) error {
	if lc == nil {
		return fmt.Errorf("nil market lifecycle")
	}
	n, err := sqlExec(tx, fmt.Sprintf(internal.UpdateMarketLifecycle, a.tables.marketLifecycle),
		lc.Market, lc.State, lc.StartEpochIdx, lc.StartEpochDur,
		lc.FinalEpochIdx, lc.FinalEpochDur, lc.PendingAction,
		lc.PendingEpochIdx, lc.PendingEpochDur, lc.PersistBook,
		lc.ActiveEpochIdx, lc.ProcessedEpochIdx, lc.RunParams.LotSize,
		lc.RunParams.RateStep, lc.RunParams.ParcelSize,
		lc.RunParams.MaxUserCancelsPerEpoch, lc.RunParams.MinimumRate)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("market lifecycle update affected %d rows, expected 1", n)
	}
	return nil
}

// applyMarketStartedLifecycleTx projects the market_started event into the
// lifecycle row.
func (a *Archiver) applyMarketStartedLifecycleTx(tx *sql.Tx, update *db.MarketStartedUpdate) (*db.MarketLifecycle, error) {
	lc, err := a.marketLifecycleForUpdate(tx, update.Market)
	if err != nil {
		return nil, err
	}
	next, changed, err := db.ProjectMarketStartedLifecycle(lc, update)
	if err != nil {
		return nil, err
	}
	if !changed {
		return next, nil
	}
	if lc == nil {
		err = a.upsertMarketLifecycleTx(tx, next)
	} else {
		err = a.updateMarketLifecycleTx(tx, next)
	}
	if err != nil {
		return nil, err
	}
	return next, nil
}

func sortOrderIDs(ids []order.OrderID) {
	sort.Slice(ids, func(i, j int) bool {
		return string(ids[i][:]) < string(ids[j][:])
	})
}

func (a *Archiver) validateLifecycleMarket(market string, base, quote uint32) error {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return err
	}
	mkt := a.markets[marketSchema]
	if mkt == nil {
		return fmt.Errorf("unknown lifecycle market %s for assets %d:%d", market, base, quote)
	}
	if mkt.Name != market {
		return fmt.Errorf("lifecycle market/assets mismatch: event market %s assets %d:%d resolve to %s",
			market, base, quote, mkt.Name)
	}
	return nil
}

func (a *Archiver) applyAdvanceEpochLifecycleTx(tx *sql.Tx, event *meshevents.AdvanceEpochEvent) error {
	lifecycle, err := a.marketLifecycleForUpdate(tx, event.Market)
	if err != nil {
		return err
	}
	next, err := db.ProjectAdvanceEpochLifecycle(lifecycle, event)
	if err != nil {
		return err
	}
	return a.updateMarketLifecycleTx(tx, next)
}

// applyEpochProcessedLifecycleTx locks the lifecycle row, checks that the epoch
// is next to be processed, and advances ProcessedEpochIdx.
func (a *Archiver) applyEpochProcessedLifecycleTx(tx *sql.Tx, market string, epochIdx, epochDur int64) error {
	lc, err := a.marketLifecycleForUpdate(tx, market)
	if err != nil {
		return err
	}
	next, err := db.ProjectEpochProcessedLifecycle(lc, market, epochIdx, epochDur)
	if err != nil {
		return err
	}
	return a.updateMarketLifecycleTx(tx, next)
}

// checkOrderAcceptanceTx locks the market's lifecycle row and checks
// that the current lifecycle permits accepting the order into its epoch.
func (a *Archiver) checkOrderAcceptanceTx(tx *sql.Tx, market string, epochIdx, epochDur, orderTimeMs int64) error {
	lifecycle, err := a.marketLifecycleForUpdate(tx, market)
	if err != nil {
		return err
	}
	if lifecycle == nil {
		return fmt.Errorf("missing lifecycle row for market %s", market)
	}
	if lifecycle.State != db.MarketStateRunning {
		return fmt.Errorf("order_accepted for non-running market %s", market)
	}
	if lifecycle.PendingAction == db.MarketPendingNone {
		return nil
	}
	if lifecycle.PendingAction != db.MarketPendingSuspend {
		return fmt.Errorf("order_accepted rejected for market %s lifecycle pending action %d",
			market, lifecycle.PendingAction)
	}

	if epochDur != lifecycle.PendingEpochDur {
		return fmt.Errorf("order_accepted epoch duration %d mismatches pending suspend duration %d for market %s",
			epochDur, lifecycle.PendingEpochDur, market)
	}
	suspendBoundary := (lifecycle.PendingEpochIdx + 1) * lifecycle.PendingEpochDur
	if orderTimeMs >= suspendBoundary {
		return fmt.Errorf("order_accepted time %d at/after pending suspend boundary %d for market %s",
			orderTimeMs, suspendBoundary, market)
	}
	if epochIdx > lifecycle.PendingEpochIdx {
		return fmt.Errorf("order_accepted epoch %d after pending suspend final epoch %d for market %s",
			epochIdx, lifecycle.PendingEpochIdx, market)
	}
	return nil
}

func selectOrderIDsByStatus(dbe sqlQueryer, tableName string, status pgOrderStatus) ([]order.OrderID, error) {
	return scanOrderIDRows(dbe.Query(fmt.Sprintf(internal.SelectOrderIDsByStatus, tableName), status))
}

func selectOrderIDsByStatusAndEpoch(dbe sqlQueryer, tableName string, status pgOrderStatus, epochIdx, epochDur int64) ([]order.OrderID, error) {
	return scanOrderIDRows(dbe.Query(fmt.Sprintf(internal.SelectOrderIDsByStatusAndEpoch, tableName), status, epochIdx, epochDur))
}

func scanOrderIDRows(rows *sql.Rows, err error) ([]order.OrderID, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []order.OrderID
	for rows.Next() {
		var oid order.OrderID
		if err := rows.Scan(&oid); err != nil {
			return nil, err
		}
		ids = append(ids, oid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Archiver) activeEpochOrderIDsTx(tx *sql.Tx, base, quote uint32) ([]order.OrderID, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}
	tradesActive := fullOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
	tradeIDs, err := selectOrderIDsByStatus(tx, tradesActive, orderStatusEpoch)
	if err != nil {
		return nil, err
	}
	cancelsActive := fullCancelOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
	cancelIDs, err := selectOrderIDsByStatus(tx, cancelsActive, orderStatusEpoch)
	if err != nil {
		return nil, err
	}
	return append(tradeIDs, cancelIDs...), nil
}

// unprocessedEpochOrderIDsTx returns trade and cancel order IDs still in epoch
// status for the specified market and epoch.
func (a *Archiver) unprocessedEpochOrderIDsTx(tx *sql.Tx, base, quote uint32, epochIdx, epochDur int64) ([]order.OrderID, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}
	tradesActive := fullOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
	tradeIDs, err := selectOrderIDsByStatusAndEpoch(tx, tradesActive, orderStatusEpoch, epochIdx, epochDur)
	if err != nil {
		return nil, err
	}
	cancelsActive := fullCancelOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
	cancelIDs, err := selectOrderIDsByStatusAndEpoch(tx, cancelsActive, orderStatusEpoch, epochIdx, epochDur)
	if err != nil {
		return nil, err
	}
	return append(tradeIDs, cancelIDs...), nil
}
