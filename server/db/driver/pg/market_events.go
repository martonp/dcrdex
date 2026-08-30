// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

// ApplyMarketStartedEvent applies the market_started event in one
// transaction. The event's epoch revokes must exactly match the market's
// active epoch orders.
func (a *Archiver) ApplyMarketStartedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.MarketStartedUpdate) (logEntry *db.EventLogEntry, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil market started update")
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}
	revocationTime := update.RevocationTime

	return a.applyEventTx(ctx, meta, meshevents.EventKindMarketStarted, txData, func(tx *sql.Tx) error {
		if err := a.applyMarketStartedLifecycleTx(tx, update); err != nil {
			return err
		}
		for _, revoke := range update.BookedRevokes {
			if revoke == nil || revoke.Order == nil {
				return fmt.Errorf("nil startup booked revoke")
			}
			ord := revoke.Order
			oid := ord.ID()
			if _, err := a.revokeOrderByID(tx, oid, ord.User(), update.Base, update.Quote, true, revocationTime); err != nil {
				return fmt.Errorf("startup booked revoke %v: %w", oid, err)
			}
		}
		return a.applyMarketStartedEpochRevokesTx(tx, update, revocationTime)
	})
}

// applyMarketStartedEpochRevokesTx revokes the epoch-status orders listed in
// a market_started event. The revoke set must exactly match the market's
// active epoch orders.
func (a *Archiver) applyMarketStartedEpochRevokesTx(tx *sql.Tx, update *db.MarketStartedUpdate, revocationTime time.Time) error {
	active, err := a.activeEpochOrderIDsTx(tx, update.Base, update.Quote)
	if err != nil {
		return err
	}
	revokeIDs := make([]order.OrderID, 0, len(update.EpochRevokes))
	for _, revoke := range update.EpochRevokes {
		if revoke == nil || revoke.Order == nil {
			return fmt.Errorf("nil startup epoch revoke")
		}
		revokeIDs = append(revokeIDs, revoke.Order.ID())
	}
	if !sameOrderIDs(active, revokeIDs) {
		return fmt.Errorf("market_started epoch revoke set does not match the %d active epoch orders for market %s",
			len(active), update.Market)
	}
	for _, revoke := range update.EpochRevokes {
		ord := revoke.Order
		oid := ord.ID()
		if ord.Type() == order.CancelOrderType {
			if err := a.updateOrderStatusWithExecutor(tx, ord, orderStatusFailed); err != nil {
				return fmt.Errorf("startup epoch cancel failure %v: %w", oid, err)
			}
			continue
		}
		if _, err := a.revokeOrder(tx, ord, true, revocationTime); err != nil {
			return fmt.Errorf("startup epoch revoke %v: %w", oid, err)
		}
	}
	return nil
}

// ApplyOrdersRevokedEvent applies the orders_revoked event in one
// transaction: each booked order is revoked with a generated cancel and a
// neutral (non-canceled) order outcome. Server revocations never count
// toward the owner's cancellation rate, for any reason. Non-booked targets
// are an error; targets come from the replicated book at apply time.
func (a *Archiver) ApplyOrdersRevokedEvent(ctx context.Context, meta *db.EventLogMeta, policy *db.ReputationOutcomePolicy, update *db.OrdersRevokedUpdate) (*db.EventLogEntry, error) {
	if err := validateOrdersRevokedUpdate(update); err != nil {
		return nil, err
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	return a.applyRepEventTx(ctx, meta, meshevents.EventKindOrdersRevoked, txData, policy, func(tx *sql.Tx, repUpdates *reputationOutcomeBatch) error {
		for _, lo := range update.Orders {
			oid, user := lo.ID(), lo.User()
			status, ordType, _, err := a.orderStatusByIDWithExecutor(tx, oid, lo.Base(), lo.Quote())
			if err != nil {
				return fmt.Errorf("orders_revoked status lookup for order %v: %w", oid, err)
			}
			if status != orderStatusBooked || ordType != order.LimitOrderType {
				return fmt.Errorf("cannot revoke order %v in status %v with type %v", oid, status, ordType)
			}
			cancelID, err := a.revokeOrderByID(tx, oid, user, lo.Base(), lo.Quote(), false, update.RevokeTime)
			if err != nil {
				return fmt.Errorf("orders_revoked revoke %v: %w", oid, err)
			}
			repUpdates.orders = append(repUpdates.orders, &reputationOrderOutcome{
				user: user,
				oid:  cancelID,
			})
		}
		return nil
	})
}

func validateOrdersRevokedUpdate(update *db.OrdersRevokedUpdate) error {
	if update == nil {
		return fmt.Errorf("nil orders revoked update")
	}
	if !meshevents.ValidOrderRevokeReason(update.Reason) {
		return fmt.Errorf("invalid order revoke reason %d", update.Reason)
	}
	if update.RevokeTime.IsZero() {
		return fmt.Errorf("empty orders_revoked revoke time")
	}
	if len(update.Orders) == 0 {
		return fmt.Errorf("empty orders_revoked order list")
	}
	for _, lo := range update.Orders {
		if lo == nil {
			return fmt.Errorf("nil orders_revoked order")
		}
	}
	return nil
}

// ApplyEpochProcessedEvent applies the epoch_processed event's persistent state
// transition in one transaction: the epoch's preimage outcomes and its match
// results. Orders whose owners missed the preimage request are revoked with a
// reputation penalty; revealed preimages are stored with a success outcome.
func (a *Archiver) ApplyEpochProcessedEvent(ctx context.Context, meta *db.EventLogMeta, policy *db.ReputationOutcomePolicy, update *db.EpochProcessedUpdate) (logEntry *db.EventLogEntry, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil epoch processed update")
	}
	if update.Epoch == nil {
		return nil, fmt.Errorf("nil epoch results")
	}
	baseTxData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	marketName, err := dex.MarketName(update.Epoch.MktBase, update.Epoch.MktQuote)
	if err != nil {
		return nil, err
	}
	return a.applyRepEventTx(ctx, meta, meshevents.EventKindEpochProcessed, baseTxData, policy, func(tx *sql.Tx, repUpdates *reputationOutcomeBatch) error {
		if err := a.applyEpochProcessedLifecycleTx(tx, marketName, update.Epoch.Idx, update.Epoch.Dur); err != nil {
			return err
		}
		for _, miss := range update.Misses {
			if miss == nil {
				return fmt.Errorf("nil preimage miss update")
			}
			cancelID, err := a.revokeOrder(tx, miss.Order, false, miss.RevokeTime)
			if err != nil {
				return err
			}
			repUpdates.orders = append(repUpdates.orders, &reputationOrderOutcome{
				user: miss.Order.User(),
				oid:  cancelID,
			})
			repUpdates.preimages = append(repUpdates.preimages, &reputationPreimageOutcome{
				user: miss.Order.User(),
				oid:  miss.Order.ID(),
				miss: true,
			})
		}
		for _, reveal := range update.Reveals {
			if reveal == nil {
				return fmt.Errorf("nil preimage reveal update")
			}
			if err := a.storePreimage(tx, reveal.Order, reveal.Preimage); err != nil {
				return err
			}
			repUpdates.preimages = append(repUpdates.preimages, &reputationPreimageOutcome{
				user: reveal.Order.User(),
				oid:  reveal.Order.ID(),
			})
		}
		if err := a.insertEpoch(tx, update.Epoch); err != nil {
			return err
		}
		for _, lo := range update.TradesBooked {
			if err := a.updateOrderStatusWithExecutor(tx, lo, orderStatusBooked); err != nil {
				return err
			}
		}
		for _, lo := range update.TradesPartial {
			if err := a.updateOrderFilledByIDWithExecutor(tx, lo.ID(), lo.Base(), lo.Quote(), int64(lo.Trade().Filled())); err != nil {
				return err
			}
		}
		for _, ord := range update.TradesCompleted {
			if err := a.updateOrderStatusWithExecutor(tx, ord, orderStatusExecuted); err != nil {
				return err
			}
		}
		for _, lo := range update.TradesCanceled {
			if err := a.updateOrderStatusWithExecutor(tx, lo, orderStatusCanceled); err != nil {
				return err
			}
		}
		for _, ord := range update.TradesFailed {
			if err := a.updateOrderStatusWithExecutor(tx, ord, orderStatusExecuted); err != nil {
				return err
			}
		}
		for _, co := range update.CancelsFailed {
			if err := a.updateOrderStatusWithExecutor(tx, co, orderStatusFailed); err != nil {
				return err
			}
		}
		for _, co := range update.CancelsExecuted {
			epochGap, err := a.cancelOrderEpochGapWithExecutor(tx, co.ID(), co.Base(), co.Quote())
			if err != nil {
				return err
			}
			if err = a.updateOrderStatusWithExecutor(tx, co, orderStatusExecuted); err != nil {
				return err
			}
			penalizedCancel := policy != nil && epochGap >= 0 && epochGap < policy.FreeCancelThreshold
			repUpdates.orders = append(repUpdates.orders, &reputationOrderOutcome{
				user:            co.User(),
				oid:             co.ID(),
				penalizedCancel: penalizedCancel,
			})
		}

		for _, match := range update.Matches {
			matchesTableName, err := a.matchTableName(match)
			if err != nil {
				return err
			}
			n, err := upsertMatch(tx, matchesTableName, match)
			if err != nil {
				a.fatalBackendErr(err)
				return err
			}
			if n != 1 {
				return fmt.Errorf("upsertMatch: updated %d rows, expected 1", n)
			}
		}

		// Make sure no orders from the epoch were left in epoch status.
		stranded, err := a.epochOrderIDsForEpochTx(tx, update.Epoch.MktBase, update.Epoch.MktQuote,
			update.Epoch.Idx, update.Epoch.Dur)
		if err != nil {
			return err
		}
		if n := len(stranded); n > 0 {
			return fmt.Errorf("epoch_processed for epoch %d leaves %d orders in epoch status",
				update.Epoch.Idx, n)
		}

		return nil
	})
}
