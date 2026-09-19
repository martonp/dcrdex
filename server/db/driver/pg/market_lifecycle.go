// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"database/sql"
	"errors"
	"fmt"

	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
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
