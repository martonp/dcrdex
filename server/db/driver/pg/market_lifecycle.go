// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
)

func scanMarketLifecycle(row *sql.Row) (*db.MarketLifecycle, error) {
	var persist sql.NullBool
	lc := new(db.MarketLifecycle)
	err := row.Scan(&lc.Market, &lc.State, &lc.StartEpochIdx, &lc.StartEpochDur,
		&lc.FinalEpochIdx, &lc.FinalEpochDur, &lc.PendingAction,
		&lc.PendingEpochIdx, &lc.PendingEpochDur, &persist, &lc.ActiveEpochIdx,
		&lc.ProcessedEpochIdx,
		&lc.RunParams.LotSize, &lc.RunParams.RateStep, &lc.RunParams.ParcelSize,
		&lc.RunParams.MaxUserCancelsPerEpoch, &lc.RunParams.MinimumRate)
	if err != nil {
		return nil, err
	}
	if persist.Valid {
		lc.PersistBook = new(bool)
		*lc.PersistBook = persist.Bool
	}
	return lc, nil
}

func (a *Archiver) marketLifecycleTx(tx *sql.Tx, market string, forUpdate bool) (*db.MarketLifecycle, error) {
	query := internal.SelectMarketLifecycle
	if forUpdate {
		query = internal.SelectMarketLifecycleForUpdate
	}
	lc, err := scanMarketLifecycle(tx.QueryRow(fmt.Sprintf(query, a.tables.marketLifecycle), market))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return lc, err
}

// MarketLifecycle retrieves the durable lifecycle projection for a market.
func (a *Archiver) MarketLifecycle(market string) (*db.MarketLifecycle, error) {
	lc, err := scanMarketLifecycle(a.db.QueryRow(fmt.Sprintf(internal.SelectMarketLifecycle, a.tables.marketLifecycle), market))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return lc, err
}

func persistBookArg(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}

func (a *Archiver) upsertMarketLifecycleTx(tx *sql.Tx, lc *db.MarketLifecycle) error {
	if lc == nil {
		return fmt.Errorf("nil market lifecycle")
	}
	_, err := tx.Exec(fmt.Sprintf(internal.UpsertMarketLifecycle, a.tables.marketLifecycle),
		lc.Market, lc.State, lc.StartEpochIdx, lc.StartEpochDur,
		lc.FinalEpochIdx, lc.FinalEpochDur, lc.PendingAction,
		lc.PendingEpochIdx, lc.PendingEpochDur, persistBookArg(lc.PersistBook),
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
		lc.PendingEpochIdx, lc.PendingEpochDur, persistBookArg(lc.PersistBook),
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
func (a *Archiver) applyMarketStartedLifecycleTx(tx *sql.Tx, update *db.MarketStartedUpdate) error {
	lc, err := a.marketLifecycleTx(tx, update.Market, true)
	if err != nil {
		return err
	}
	next, changed, err := db.ProjectMarketStartedLifecycle(lc, update)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if lc == nil {
		return a.upsertMarketLifecycleTx(tx, next)
	}
	return a.updateMarketLifecycleTx(tx, next)
}

func sameEpoch(idxA, durA, idxB, durB int64) bool {
	return idxA == idxB && durA == durB
}

func sortOrderIDs(ids []order.OrderID) {
	sort.Slice(ids, func(i, j int) bool {
		return string(ids[i][:]) < string(ids[j][:])
	})
}

func sameOrderIDs(a, b []order.OrderID) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]order.OrderID(nil), a...)
	b = append([]order.OrderID(nil), b...)
	sortOrderIDs(a)
	sortOrderIDs(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

func (a *Archiver) applySuspendTx(tx *sql.Tx, lc *db.MarketLifecycle, update *db.MarketLifecycleUpdate) (*db.MarketLifecycle, []order.OrderID, error) {
	next, err := db.ProjectMarketLifecycle(lc, update)
	if err != nil {
		return nil, nil, err
	}
	var purged []order.OrderID
	if !*next.PersistBook {
		var purgeErr error
		if purged, purgeErr = a.purgeBookAtTx(tx, update.Base, update.Quote, update.Timestamp); purgeErr != nil {
			return nil, nil, purgeErr
		}
	}
	return next, purged, a.updateMarketLifecycleTx(tx, next)
}

// applyResumeTx revokes the event's chain-selected booked orders and reopens
// the market. The revoke set is filtered against replicated order state: an
// order no longer booked at the apply point (revoked by an intervening event)
// is skipped, so every node revokes an identical set regardless of when the
// master built the event.
func (a *Archiver) applyResumeTx(tx *sql.Tx, lc *db.MarketLifecycle, update *db.MarketLifecycleUpdate) (*db.MarketLifecycle, []*db.StartupOrderRevoke, error) {
	next, err := db.ProjectMarketLifecycle(lc, update)
	if err != nil {
		return nil, nil, err
	}
	booked, err := a.bookedOrderIDSetTx(tx, update.Base, update.Quote)
	if err != nil {
		return nil, nil, err
	}
	revoked := make([]*db.StartupOrderRevoke, 0, len(update.ResumeRevokes))
	for _, revoke := range update.ResumeRevokes {
		if revoke == nil || revoke.Order == nil {
			return nil, nil, fmt.Errorf("nil resume booked revoke")
		}
		ord := revoke.Order
		if !booked[ord.ID()] {
			continue
		}
		if _, err := a.revokeOrderByID(tx, ord.ID(), ord.User(), update.Base, update.Quote, true, update.Timestamp); err != nil {
			return nil, nil, err
		}
		revoked = append(revoked, revoke)
	}
	return next, revoked, a.updateMarketLifecycleTx(tx, next)
}

// bookedOrderIDSetTx returns the set of booked order IDs for a market. Only
// limit trades can hold booked status, so membership doubles as the type
// check.
func (a *Archiver) bookedOrderIDSetTx(tx *sql.Tx, base, quote uint32) (map[order.OrderID]bool, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}
	tradesActive := fullOrderTableName(a.dbName, marketSchema, orderStatusBooked.active())
	ids, err := selectOrderIDsByStatus(tx, tradesActive, orderStatusBooked)
	if err != nil {
		return nil, err
	}
	booked := make(map[order.OrderID]bool, len(ids))
	for _, oid := range ids {
		booked[oid] = true
	}
	return booked, nil
}

// ApplyMarketLifecycleEvent applies a market_lifecycle event's persistent state
// transition in one transaction.
func (a *Archiver) ApplyMarketLifecycleEvent(ctx context.Context, meta *db.EventLogMeta, update *db.MarketLifecycleUpdate) (result *db.MarketLifecycleApplyResult, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil market lifecycle update")
	}
	if err := a.validateLifecycleMarket(update.Market, update.Base, update.Quote); err != nil {
		return nil, err
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}
	result = &db.MarketLifecycleApplyResult{}
	logEntry, err := a.applyEventTx(ctx, meta, meshevents.EventKindMarketLifecycle, txData, func(tx *sql.Tx) error {
		lc, err := a.marketLifecycleTx(tx, update.Market, true)
		if err != nil {
			return err
		}
		if lc == nil {
			return fmt.Errorf("missing lifecycle row for market %s", update.Market)
		}
		var next *db.MarketLifecycle
		switch update.Action {
		case db.MarketLifecycleActionScheduleSuspend, db.MarketLifecycleActionScheduleResume:
			next, err = db.ProjectMarketLifecycle(lc, update)
			if err != nil {
				return err
			}
			if err = a.updateMarketLifecycleTx(tx, next); err != nil {
				return err
			}
		case db.MarketLifecycleActionSuspend:
			next, result.PurgeOrders, err = a.applySuspendTx(tx, lc, update)
		case db.MarketLifecycleActionResume:
			next, result.ResumeRevokes, err = a.applyResumeTx(tx, lc, update)
		default:
			err = fmt.Errorf("unknown market lifecycle action %d", update.Action)
		}
		if err != nil {
			return err
		}
		result.Lifecycle = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Log = logEntry
	return result, nil
}

// purgeBookAtTx moves every booked order to revoked status and records a
// pseudo-cancel for each at timeStamp. The purge set is whatever the order
// tables hold at the apply point — replicated state, so every node purges an
// identical set. The purged order IDs are returned, sorted, for the in-memory
// projections.
func (a *Archiver) purgeBookAtTx(tx *sql.Tx, base, quote uint32, timeStamp time.Time) ([]order.OrderID, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}
	timeStamp = timeStamp.Truncate(time.Millisecond).UTC()
	srcTableName := fullOrderTableName(a.dbName, marketSchema, orderStatusBooked.active())
	dstTableName := fullOrderTableName(a.dbName, marketSchema, orderStatusRevoked.active())
	stmt := fmt.Sprintf(internal.PurgeBook, srcTableName, orderStatusRevoked, dstTableName)
	rows, err := tx.Query(stmt, orderStatusBooked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type purgedOrder struct {
		oid  order.OrderID
		acct account.AccountID
	}
	var purged []purgedOrder
	for rows.Next() {
		var p purgedOrder
		var sell bool
		if err := rows.Scan(&p.oid, &sell, &p.acct); err != nil {
			return nil, err
		}
		purged = append(purged, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(purged, func(i, j int) bool {
		return string(purged[i].oid[:]) < string(purged[j].oid[:])
	})
	cancelTable := fullCancelOrderTableName(a.dbName, marketSchema, orderStatusRevoked.active())
	stmt = fmt.Sprintf(internal.InsertCancelOrder, cancelTable)
	ids := make([]order.OrderID, 0, len(purged))
	for _, p := range purged {
		co := makePseudoCancel(p.oid, p.acct, base, quote, timeStamp)
		_, err = tx.Exec(stmt, co.ID(), co.AccountID, co.ClientTime,
			co.ServerTime, nil, co.TargetOrderID, orderStatusRevoked, exemptEpochIdx, dummyEpochDur, db.EpochGapNA)
		if err != nil {
			return nil, fmt.Errorf("failed to store pseudo-cancel order: %w", err)
		}
		ids = append(ids, p.oid)
	}
	return ids, nil
}

func (a *Archiver) applyAdvanceEpochLifecycleTx(tx *sql.Tx, update *db.AdvanceEpochUpdate) error {
	lc, err := a.marketLifecycleTx(tx, update.Market, true)
	if err != nil {
		return err
	}
	next, changed, err := db.ProjectAdvanceEpochLifecycle(lc, update)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return a.updateMarketLifecycleTx(tx, next)
}

// applyEpochProcessedLifecycleTx validates the epoch_processed event's epoch
// against the lifecycle row and advances the closure cursor.
func (a *Archiver) applyEpochProcessedLifecycleTx(tx *sql.Tx, market string, epochIdx, epochDur int64) error {
	lc, err := a.marketLifecycleTx(tx, market, true)
	if err != nil {
		return err
	}
	next, err := db.ProjectEpochProcessedLifecycle(lc, market, epochIdx, epochDur)
	if err != nil {
		return err
	}
	return a.updateMarketLifecycleTx(tx, next)
}

func (a *Archiver) validateOrderAcceptedLifecycleTx(tx *sql.Tx, market string, epochIdx, epochDur, orderTime int64) error {
	lc, err := a.marketLifecycleTx(tx, market, true)
	if err != nil {
		return err
	}
	if lc == nil {
		return fmt.Errorf("missing lifecycle row for market %s", market)
	}
	if lc.State != db.MarketStateRunning {
		return fmt.Errorf("order_accepted for non-running market %s", market)
	}
	switch lc.PendingAction {
	case db.MarketPendingNone:
		return nil
	case db.MarketPendingSuspend:
		if epochDur != lc.PendingEpochDur {
			return fmt.Errorf("order_accepted epoch duration %d mismatches pending suspend duration %d for market %s",
				epochDur, lc.PendingEpochDur, market)
		}
		suspendBoundary := (lc.PendingEpochIdx + 1) * lc.PendingEpochDur
		if orderTime >= suspendBoundary {
			return fmt.Errorf("order_accepted time %d at/after pending suspend boundary %d for market %s",
				orderTime, suspendBoundary, market)
		}
		if epochIdx > lc.PendingEpochIdx {
			return fmt.Errorf("order_accepted epoch %d after pending suspend final epoch %d for market %s",
				epochIdx, lc.PendingEpochIdx, market)
		}
		return nil
	default:
		return fmt.Errorf("order_accepted rejected for market %s lifecycle pending action %d",
			market, lc.PendingAction)
	}
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
	ids := append(tradeIDs, cancelIDs...)
	sortOrderIDs(ids)
	return ids, nil
}

// epochOrderIDsForEpochTx returns the IDs of the market's orders still in
// epoch status for the given epoch, trades and cancels both.
func (a *Archiver) epochOrderIDsForEpochTx(tx *sql.Tx, base, quote uint32, epochIdx, epochDur int64) ([]order.OrderID, error) {
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
	ids := append(tradeIDs, cancelIDs...)
	sortOrderIDs(ids)
	return ids, nil
}

func suspendedCancelMatchFromTarget(cancel *order.CancelOrder, target *order.LimitOrder, epochIdx, epochDur int64, feeRateBase, feeRateQuote uint64) *order.Match {
	return &order.Match{
		Taker:        cancel,
		Maker:        target,
		Quantity:     target.Remaining(),
		Rate:         target.Rate,
		Epoch:        order.EpochID{Idx: uint64(epochIdx), Dur: uint64(epochDur)},
		FeeRateBase:  feeRateBase,
		FeeRateQuote: feeRateQuote,
		Status:       order.MatchComplete,
	}
}

func validateSuspendedCancelMatch(got, want *order.Match) error {
	if got == nil {
		return fmt.Errorf("nil suspended cancel match")
	}
	if got.Taker == nil || got.Maker == nil {
		return fmt.Errorf("nil suspended cancel match order")
	}
	if got.ID() != want.ID() || got.Taker.ID() != want.Taker.ID() || got.Maker.ID() != want.Maker.ID() {
		return fmt.Errorf("suspended_cancel match order mismatch")
	}
	if got.Quantity != want.Quantity || got.Rate != want.Rate {
		return fmt.Errorf("suspended_cancel match quantity/rate mismatch")
	}
	if got.Epoch != want.Epoch {
		return fmt.Errorf("suspended_cancel match epoch mismatch")
	}
	if got.FeeRateBase != want.FeeRateBase || got.FeeRateQuote != want.FeeRateQuote {
		return fmt.Errorf("suspended_cancel match fee-rate mismatch")
	}
	if got.Status != order.MatchComplete {
		return fmt.Errorf("suspended_cancel match status %d, want %d", got.Status, order.MatchComplete)
	}
	return nil
}

func loadBookedLimitOrderTx(tx *sql.Tx, dbName, marketSchema string, oid order.OrderID, base, quote uint32) (*order.LimitOrder, error) {
	tableName := fullOrderTableName(dbName, marketSchema, orderStatusBooked.active())
	ord, status, err := loadTradeFromTable(tx, tableName, oid)
	if err != nil {
		return nil, err
	}
	if status != orderStatusBooked {
		return nil, fmt.Errorf("order %v has status %v, want booked", oid, status)
	}
	lo, ok := ord.(*order.LimitOrder)
	if !ok || lo.Force != order.StandingTiF {
		return nil, fmt.Errorf("order %v is not a booked standing limit order", oid)
	}
	lo.BaseAsset = base
	lo.QuoteAsset = quote
	return lo, nil
}

// ApplySuspendedCancelEvent applies a suspended_cancel event's persistent state
// transition in one transaction.
func (a *Archiver) ApplySuspendedCancelEvent(ctx context.Context, meta *db.EventLogMeta, update *db.SuspendedCancelUpdate) (result *db.SuspendedCancelApplyResult, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil suspended cancel update")
	}
	if update.Cancel == nil {
		return nil, fmt.Errorf("nil suspended cancel order")
	}
	if update.Match == nil {
		return nil, fmt.Errorf("nil suspended cancel match")
	}
	if err := a.validateLifecycleMarket(update.Market, update.Base, update.Quote); err != nil {
		return nil, err
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}
	result = &db.SuspendedCancelApplyResult{
		Cancel: update.Cancel,
		Match:  update.Match,
	}
	logEntry, err := a.applyEventTx(ctx, meta, meshevents.EventKindSuspendedCancel, txData, func(tx *sql.Tx) error {
		target, match, err := a.applySuspendedCancelTx(ctx, tx, update)
		if err != nil {
			return err
		}
		result.TargetOrder = target
		result.Match = match
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Log = logEntry
	return result, nil
}

// applySuspendedCancelTx validates a suspended_cancel update against the
// lifecycle and target order rows, then stores the executed cancel, cancels
// the target order, and upserts the cancel match. It returns the loaded target
// order and the derived match.
func (a *Archiver) applySuspendedCancelTx(ctx context.Context, tx *sql.Tx, update *db.SuspendedCancelUpdate) (*order.LimitOrder, *order.Match, error) {
	lc, err := a.marketLifecycleTx(tx, update.Market, true)
	if err != nil {
		return nil, nil, err
	}
	if lc == nil || lc.State != db.MarketStateSuspended ||
		(lc.PendingAction != db.MarketPendingNone && lc.PendingAction != db.MarketPendingResume) {
		return nil, nil, fmt.Errorf("suspended_cancel requires suspended lifecycle for market %s", update.Market)
	}
	if update.Cancel.Base() != update.Base || update.Cancel.Quote() != update.Quote {
		return nil, nil, fmt.Errorf("suspended_cancel market mismatch")
	}
	if update.Cancel.TargetOrderID != update.TargetOrderID {
		return nil, nil, fmt.Errorf("suspended_cancel target mismatch")
	}
	if update.Cancel.AccountID != update.TargetAccount {
		return nil, nil, fmt.Errorf("suspended_cancel account mismatch")
	}
	marketSchema, err := a.marketSchema(update.Base, update.Quote)
	if err != nil {
		return nil, nil, err
	}
	target, err := loadBookedLimitOrderTx(tx, a.dbName, marketSchema, update.TargetOrderID, update.Base, update.Quote)
	if err != nil {
		return nil, nil, err
	}
	if target.AccountID != update.TargetAccount || target.Sell != update.TargetSell {
		return nil, nil, fmt.Errorf("suspended_cancel target order mismatch")
	}
	expectedMatch := suspendedCancelMatchFromTarget(update.Cancel, target, update.EpochIdx, update.EpochDur,
		update.FeeRateBase, update.FeeRateQuote)
	if err := validateSuspendedCancelMatch(update.Match, expectedMatch); err != nil {
		return nil, nil, err
	}
	if found, oid, err := a.orderWithCommit(ctx, tx, update.Cancel.Commitment()); err != nil {
		return nil, nil, err
	} else if found {
		return nil, nil, fmt.Errorf("suspended_cancel order %v reuses commitment from %v", update.Cancel.ID(), oid)
	}
	cancelTable := fullCancelOrderTableName(a.dbName, marketSchema, orderStatusExecuted.active())
	n, err := storeCancelOrder(tx, cancelTable, update.Cancel, orderStatusExecuted, update.EpochIdx, update.EpochDur, db.EpochGapNA)
	if err != nil {
		return nil, nil, err
	}
	if n != 1 {
		return nil, nil, fmt.Errorf("stored suspended cancel rows = %d, want 1", n)
	}
	if err := a.updateOrderStatusWithExecutor(tx, target, orderStatusCanceled); err != nil {
		return nil, nil, err
	}
	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
	n, err = upsertMatch(tx, matchesTableName, expectedMatch)
	if err != nil {
		return nil, nil, err
	}
	if n != 1 {
		return nil, nil, fmt.Errorf("upsertMatch: updated %d rows, expected 1", n)
	}
	return target, expectedMatch, nil
}
