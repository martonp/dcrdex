// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

// ApplyMarketStartedEvent updates the market lifecycle, revokes the listed
// booked orders and all abandoned epoch trades, and marks abandoned cancel
// orders as failed. It returns the lifecycle and event log entry committed
// with these changes.
func (a *Archiver) ApplyMarketStartedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.MarketStartedUpdate) (*db.MarketStartedApplyResult, error) {
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	result := new(db.MarketStartedApplyResult)
	logEntry, err := a.applyEventTx(ctx, meta, meshevents.EventKindMarketStarted, txData, func(tx *sql.Tx) error {
		lifecycle, err := a.applyMarketStartedLifecycleTx(tx, update)
		if err != nil {
			return err
		}
		result.Lifecycle = lifecycle
		for _, revoke := range update.BookedRevokes {
			ord := revoke.Order
			oid := ord.ID()
			if _, err := a.revokeBookedOrderByID(tx, oid, ord.User(), update.Base, update.Quote, true, update.RevocationTime); err != nil {
				return fmt.Errorf("startup booked revoke %v: %w", oid, err)
			}
		}
		return a.applyMarketStartedEpochRevokesTx(tx, update)
	})
	if err != nil {
		return nil, err
	}
	result.Log = logEntry
	return result, nil
}

// applyMarketStartedEpochRevokesTx revokes the epoch-status orders listed in
// a market_started event. The revoke set must exactly match the market's
// active epoch orders.
func (a *Archiver) applyMarketStartedEpochRevokesTx(tx *sql.Tx, update *db.MarketStartedUpdate) error {
	activeIDs, err := a.activeEpochOrderIDsTx(tx, update.Base, update.Quote)
	if err != nil {
		return err
	}
	revokeIDs := make([]order.OrderID, 0, len(update.EpochRevokes))
	for _, revoke := range update.EpochRevokes {
		revokeIDs = append(revokeIDs, revoke.Order.ID())
	}
	sortOrderIDs(activeIDs)
	sortOrderIDs(revokeIDs)
	if !slices.Equal(activeIDs, revokeIDs) {
		return fmt.Errorf("market_started epoch revoke set does not match the %d active epoch orders for market %s",
			len(activeIDs), update.Market)
	}
	for _, revoke := range update.EpochRevokes {
		ord := revoke.Order
		oid := ord.ID()
		if ord.Type() == order.CancelOrderType {
			if err := a.updateOrderStatus(tx, ord, orderStatusFailed); err != nil {
				return fmt.Errorf("startup epoch cancel failure %v: %w", oid, err)
			}
			continue
		}
		if _, err := a.revokeOrder(tx, ord, true, update.RevocationTime); err != nil {
			return fmt.Errorf("startup epoch revoke %v: %w", oid, err)
		}
	}
	return nil
}
