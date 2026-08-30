// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
	"github.com/lib/pq"
)

// Wrap the CoinID slice to implement custom Scanner and Valuer.
type dbCoins []order.CoinID

// Value implements the sql/driver.Valuer interface. The coin IDs are encoded as
// L0|ID0|L1|ID1|... where | is simple concatenation, Ln is the length of the
// nth coin ID, and IDn is the bytes of the nth coinID.
func (coins dbCoins) Value() (driver.Value, error) {
	if len(coins) == 0 {
		return []byte{}, nil
	}
	// As an initial guess that's likely accurate for most coins, allocate as if
	// each coin ID is the same length.
	lenGuess := len(coins[0])
	b := make([]byte, 0, len(coins)*(lenGuess+1))
	for _, coin := range coins {
		b = append(b, byte(len(coin)))
		b = append(b, coin...)
	}
	return b, nil
}

// Scan implements the sql.Scanner interface.
func (coins *dbCoins) Scan(src any) error {
	b := src.([]byte)
	if len(b) == 0 {
		*coins = dbCoins{}
		return nil
	}
	lenGuess := int(b[0])
	if lenGuess == 0 {
		return fmt.Errorf("zero-length coin ID indicated")
	}
	c := make(dbCoins, 0, len(b)/(lenGuess+1))
	for len(b) > 0 {
		cLen := int(b[0])
		if cLen == 0 {
			return fmt.Errorf("zero-length coin ID indicated")
		}
		if len(b) < cLen+1 {
			return fmt.Errorf("too many bytes indicated")
		}

		// Deep copy the coin ID (a slice) since the backing buffer may be
		// reused.
		bc := make([]byte, cLen)
		copy(bc, b[1:cLen+1])
		c = append(c, bc)

		b = b[cLen+1:]
	}

	*coins = c
	return nil
}

var _ db.OrderArchiver = (*Archiver)(nil)

// Order retrieves an order with the given OrderID, stored for the market
// specified by the given base and quote assets. A non-nil error will be
// returned if the market is not recognized. If the order is not found, the
// error value is ErrUnknownOrder, and the type is order.OrderStatusUnknown. The
// only recognized order types are market, limit, and cancel.
func (a *Archiver) Order(oid order.OrderID, base, quote uint32) (order.Order, order.OrderStatus, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, order.OrderStatusUnknown, err
	}

	// Since order type is unknown:
	// - try to load from orders table, which includes market and limit orders
	// - if found, coerce into the correct order type and return
	// - if not found, try loading a cancel order with this oid
	var errA db.ArchiveError
	ord, status, err := loadTrade(a.db, a.dbName, marketSchema, oid)
	if errors.As(err, &errA) {
		if errA.Code != db.ErrUnknownOrder {
			return nil, order.OrderStatusUnknown, err
		}
		// Try the cancel orders.
		var co *order.CancelOrder
		co, status, err = loadCancelOrder(a.db, a.dbName, marketSchema, oid)
		if err != nil {
			return nil, order.OrderStatusUnknown, err // includes ErrUnknownOrder
		}
		co.BaseAsset, co.QuoteAsset = base, quote
		return co, pgToMarketStatus(status), err
		// no other order types to try presently
	}
	if err != nil {
		return nil, order.OrderStatusUnknown, err
	}
	prefix := ord.Prefix()
	prefix.BaseAsset, prefix.QuoteAsset = base, quote
	return ord, pgToMarketStatus(status), nil
}

type pgOrderStatus int16

const (
	orderStatusUnknown pgOrderStatus = iota
	orderStatusEpoch
	orderStatusBooked
	orderStatusExecuted
	orderStatusFailed // failed helps distinguish matched from unmatched executed cancel orders
	orderStatusCanceled
	orderStatusRevoked // indicates a trade order was revoked, or in the cancels table that the cancel is server-generated
)

func marketToPgStatus(status order.OrderStatus) pgOrderStatus {
	switch status {
	case order.OrderStatusEpoch:
		return orderStatusEpoch
	case order.OrderStatusBooked:
		return orderStatusBooked
	case order.OrderStatusExecuted:
		return orderStatusExecuted
	case order.OrderStatusCanceled:
		return orderStatusCanceled
	case order.OrderStatusRevoked:
		return orderStatusRevoked
	}
	return orderStatusUnknown
}

func pgToMarketStatus(status pgOrderStatus) order.OrderStatus {
	switch status {
	case orderStatusEpoch:
		return order.OrderStatusEpoch
	case orderStatusBooked:
		return order.OrderStatusBooked
	case orderStatusExecuted, orderStatusFailed: // failed is executed as far as the market is concerned
		return order.OrderStatusExecuted
	case orderStatusCanceled:
		return order.OrderStatusCanceled
	case orderStatusRevoked, -orderStatusRevoked: // negative revoke status means forgiven preimage miss
		return order.OrderStatusRevoked
	}
	return order.OrderStatusUnknown
}

func (status pgOrderStatus) String() string {
	switch status {
	case orderStatusFailed:
		return "failed"
	default:
		return pgToMarketStatus(status).String()
	}
}

func (status pgOrderStatus) active() bool {
	switch status {
	case orderStatusEpoch, orderStatusBooked:
		return true
	case orderStatusCanceled, orderStatusRevoked, -orderStatusRevoked,
		orderStatusExecuted, orderStatusFailed, orderStatusUnknown:
		return false
	default:
		panic("unknown order status!") // programmer error
	}
}

// ApplyOrderAcceptedEvent stores the order_accepted event's persistent state in
// one transaction.
func (a *Archiver) ApplyOrderAcceptedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.OrderAcceptedUpdate) (result *db.EventLogEntry, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil order accepted update")
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}
	ord := update.Order
	marketSchema, err := a.marketSchema(ord.Base(), ord.Quote())
	if err != nil {
		return nil, err
	}

	return a.applyEventTx(ctx, meta, meshevents.EventKindOrderAccepted, txData, func(tx *sql.Tx) error {
		commit := ord.Commitment()
		var storedAlready bool
		for schema := range a.markets {
			found, prevOid, err := orderForCommit(ctx, tx, a.dbName, schema, commit)
			if err != nil {
				return err
			}
			if found {
				if prevOid == ord.ID() {
					storedAlready = true
					break
				}
				return db.ArchiveError{
					Code: db.ErrReusedCommit,
					Detail: fmt.Sprintf("order %v reuses commit %v from previous order %v",
						ord.UID(), commit, prevOid),
				}
			}
		}

		var n int64
		if !storedAlready {
			mkt := a.markets[marketSchema]
			if mkt == nil {
				return fmt.Errorf("unknown market schema %s", marketSchema)
			}
			if err := a.validateOrderAcceptedLifecycleTx(tx, mkt.Name, update.EpochIdx, update.EpochDur, ord.Time()); err != nil {
				return err
			}
			switch ot := ord.(type) {
			case *order.CancelOrder:
				tableName := fullCancelOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
				var storeErr error
				n, storeErr = storeCancelOrder(tx, tableName, ot, orderStatusEpoch, update.EpochIdx, update.EpochDur, update.EpochGap)
				if storeErr != nil {
					a.fatalBackendErr(storeErr)
					return fmt.Errorf("storeCancelOrder failed: %w", storeErr)
				}
			case *order.MarketOrder:
				tableName := fullOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
				var storeErr error
				n, storeErr = storeMarketOrder(tx, tableName, ot, orderStatusEpoch, update.EpochIdx, update.EpochDur)
				if storeErr != nil {
					a.fatalBackendErr(storeErr)
					return fmt.Errorf("storeMarketOrder failed: %w", storeErr)
				}
			case *order.LimitOrder:
				tableName := fullOrderTableName(a.dbName, marketSchema, orderStatusEpoch.active())
				var storeErr error
				n, storeErr = storeLimitOrder(tx, tableName, ot, orderStatusEpoch, update.EpochIdx, update.EpochDur)
				if storeErr != nil {
					a.fatalBackendErr(storeErr)
					return fmt.Errorf("storeLimitOrder failed: %w", storeErr)
				}
			default:
				panic("ValidateOrder should have caught this")
			}

			if n != 1 {
				return fmt.Errorf("failed to store order %v: %d rows affected, expected 1", ord.UID(), n)
			}
		}
		return nil
	})
}

// ApplyAdvanceEpochEvent closes an epoch in the lifecycle row (or parks a
// final close in suspend drain) and appends the log row.
func (a *Archiver) ApplyAdvanceEpochEvent(ctx context.Context, meta *db.EventLogMeta, update *db.AdvanceEpochUpdate) (result *db.EventLogEntry, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil advance epoch update")
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}
	return a.applyEventTx(ctx, meta, meshevents.EventKindAdvanceEpoch, txData, func(tx *sql.Tx) error {
		return a.applyAdvanceEpochLifecycleTx(tx, update)
	})
}

func makePseudoCancel(target order.OrderID, user account.AccountID, base, quote uint32, timeStamp time.Time) *order.CancelOrder {
	// Create a server-generated cancel order to record the server's revoke
	// order action.
	return &order.CancelOrder{
		P: order.Prefix{
			AccountID:  user,
			BaseAsset:  base,
			QuoteAsset: quote,
			OrderType:  order.CancelOrderType,
			ClientTime: timeStamp,
			ServerTime: timeStamp,
			// The zero-value for Commitment is stored as NULL. See
			// (Commitment).Value.
		},
		TargetOrderID: target,
	}
}

// BookOrders retrieves all booked orders (with order status booked) for the
// specified market. This will be used to repopulate a market's book on
// construction of the market.
func (a *Archiver) BookOrders(base, quote uint32) ([]*order.LimitOrder, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}

	// All booked orders are active.
	tableName := fullOrderTableName(a.dbName, marketSchema, true) // active (true)

	// no query timeout here, only explicit cancellation
	ords, err := ordersByStatusFromTable(a.ctx, a.db, tableName, base, quote, orderStatusBooked)
	if err != nil {
		return nil, err
	}

	// Verify loaded orders are limits, and cast to *LimitOrder.
	limits := make([]*order.LimitOrder, 0, len(ords))
	for _, ord := range ords {
		lo, ok := ord.(*order.LimitOrder)
		if !ok {
			log.Errorf("loaded book order %v that was not a limit order", ord.ID())
			continue
		}

		limits = append(limits, lo)
	}

	return limits, nil
}

// EpochOrders retrieves all epoch orders for the specified market returns them
// as a slice of order.Order.
func (a *Archiver) EpochOrders(base, quote uint32) ([]order.Order, error) {
	los, mos, cos, err := a.epochOrders(base, quote)
	if err != nil {
		return nil, err
	}
	orders := make([]order.Order, 0, len(los)+len(mos)+len(cos))
	for _, o := range los {
		orders = append(orders, o)
	}
	for _, o := range mos {
		orders = append(orders, o)
	}
	for _, o := range cos {
		orders = append(orders, o)
	}
	return orders, nil
}

// epochOrders retrieves all epoch orders for the specified market.
func (a *Archiver) epochOrders(base, quote uint32) ([]*order.LimitOrder, []*order.MarketOrder, []*order.CancelOrder, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, nil, nil, err
	}

	tableName := fullOrderTableName(a.dbName, marketSchema, true) // active (true)

	// no query timeout here, only explicit cancellation
	ords, err := ordersByStatusFromTable(a.ctx, a.db, tableName, base, quote, orderStatusEpoch)
	if err != nil {
		return nil, nil, nil, err
	}

	// Verify loaded order type and add to correct slice.
	var limits []*order.LimitOrder
	var markets []*order.MarketOrder
	for _, ord := range ords {
		switch o := ord.(type) {
		case *order.LimitOrder:
			limits = append(limits, o)
		case *order.MarketOrder:
			markets = append(markets, o)
		default:
			log.Errorf("loaded epoch order %v that was not a limit or market order: %T", ord.ID(), ord)
		}
	}

	tableName = fullCancelOrderTableName(a.dbName, marketSchema, true) // active(true)
	cancels, err := cancelOrdersByStatusFromTable(a.ctx, a.db, tableName, base, quote, orderStatusEpoch)
	if err != nil {
		return nil, nil, nil, err
	}

	return limits, markets, cancels, nil
}

// ActiveOrderCoins retrieves a CoinID slice for each active order.
func (a *Archiver) ActiveOrderCoins(base, quote uint32) (baseCoins, quoteCoins map[order.OrderID][]order.CoinID, err error) {
	var marketSchema string
	marketSchema, err = a.marketSchema(base, quote)
	if err != nil {
		return
	}

	tableName := fullOrderTableName(a.dbName, marketSchema, true) // active (true)
	stmt := fmt.Sprintf(internal.SelectOrderCoinIDs, tableName)

	var rows *sql.Rows
	rows, err = a.db.Query(stmt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
		fallthrough
	case err == nil:
		baseCoins = make(map[order.OrderID][]order.CoinID)
		quoteCoins = make(map[order.OrderID][]order.CoinID)
	default:
		return
	}
	defer rows.Close()

	for rows.Next() {
		var oid order.OrderID
		var coins dbCoins
		var sell bool
		err = rows.Scan(&oid, &sell, &coins)
		if err != nil {
			return nil, nil, err
		}

		// Sell orders lock base asset coins.
		if sell {
			baseCoins[oid] = coins
		} else {
			// Buy orders lock quote asset coins.
			quoteCoins[oid] = coins
		}
	}

	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	return
}

const (
	exemptEpochIdx  int64 = -1
	countedEpochIdx int64 = 0
	dummyEpochDur   int64 = 1 // for idx*duration math
)

func (a *Archiver) revokeOrder(dbe sqlQueryExecutor, ord order.Order, exempt bool, timeStamp time.Time) (cancelID order.OrderID, err error) {
	// Revoke the targeted order.
	err = a.updateOrderStatusWithExecutor(dbe, ord, orderStatusRevoked)
	if err != nil {
		return
	}

	return a.storeRevocationCancel(dbe, ord.ID(), ord.User(), ord.Base(), ord.Quote(), exempt, timeStamp)
}

func (a *Archiver) revokeOrderByID(dbe sqlQueryExecutor, oid order.OrderID, user account.AccountID, base, quote uint32, exempt bool, timeStamp time.Time) (cancelID order.OrderID, err error) {
	status, ordType, _, err := a.orderStatusByIDWithExecutor(dbe, oid, base, quote)
	if err != nil {
		return order.OrderID{}, err
	}
	if status != orderStatusBooked || ordType != order.LimitOrderType {
		return order.OrderID{}, fmt.Errorf("cannot revoke non-booked order %v in status %v with type %v", oid, status, ordType)
	}

	if err = a.updateOrderStatusByIDWithExecutor(dbe, oid, base, quote, orderStatusRevoked, -1); err != nil {
		return order.OrderID{}, err
	}

	return a.storeRevocationCancel(dbe, oid, user, base, quote, exempt, timeStamp)
}

func (a *Archiver) storeRevocationCancel(dbe sqlQueryExecutor, oid order.OrderID, user account.AccountID, base, quote uint32, exempt bool, timeStamp time.Time) (cancelID order.OrderID, err error) {
	timeStamp = timeStamp.Truncate(time.Millisecond).UTC()

	// Store the pseudo-cancel order with 0 epoch idx and duration and status
	// orderStatusRevoked as indicators that this is a revocation.
	co := makePseudoCancel(oid, user, base, quote, timeStamp)
	cancelID = co.ID()
	epochIdx := countedEpochIdx
	if exempt {
		epochIdx = exemptEpochIdx
	}
	err = a.storeOrder(dbe, co, epochIdx, dummyEpochDur, db.EpochGapNA, orderStatusRevoked)
	return cancelID, err
}

func validateOrder(ord order.Order, status pgOrderStatus, mkt *dex.MarketInfo) bool {
	if status == orderStatusFailed && ord.Type() != order.CancelOrderType {
		return false
	}
	return db.ValidateOrder(ord, pgToMarketStatus(status), mkt)
}

func (a *Archiver) storeOrder(dbe sqlQueryExecutor, ord order.Order, epochIdx, epochDur int64, epochGap int32, status pgOrderStatus) error {
	marketSchema, err := a.marketSchema(ord.Base(), ord.Quote())
	if err != nil {
		return err
	}

	if !validateOrder(ord, status, a.markets[marketSchema]) {
		return db.ArchiveError{
			Code: db.ErrInvalidOrder,
			Detail: fmt.Sprintf("invalid order %v for status %v and market %v",
				ord.UID(), status, a.markets[marketSchema]),
		}
	}

	// Reject a second live order with this commitment. Archived rows are
	// not a uniqueness domain: a later order may reuse a historical commit.
	commit := ord.Commitment()
	found, prevOid, err := a.orderWithCommit(a.ctx, dbe, commit) // no query timeouts in storeOrder, only explicit cancellation
	if err != nil {
		return err
	}
	if found {
		return db.ArchiveError{
			Code: db.ErrReusedCommit,
			Detail: fmt.Sprintf("order %v reuses commit %v from previous order %v",
				ord.UID(), commit, prevOid),
		}
	}

	var N int64
	switch ot := ord.(type) {
	case *order.CancelOrder:
		tableName := fullCancelOrderTableName(a.dbName, marketSchema, status.active())
		N, err = storeCancelOrder(dbe, tableName, ot, status, epochIdx, epochDur, epochGap)
		if err != nil {
			a.fatalBackendErr(err)
			return fmt.Errorf("storeCancelOrder failed: %w", err)
		}
	case *order.MarketOrder:
		tableName := fullOrderTableName(a.dbName, marketSchema, status.active())
		N, err = storeMarketOrder(dbe, tableName, ot, status, epochIdx, epochDur)
		if err != nil {
			a.fatalBackendErr(err)
			return fmt.Errorf("storeMarketOrder failed: %w", err)
		}
	case *order.LimitOrder:
		tableName := fullOrderTableName(a.dbName, marketSchema, status.active())
		N, err = storeLimitOrder(dbe, tableName, ot, status, epochIdx, epochDur)
		if err != nil {
			a.fatalBackendErr(err)
			return fmt.Errorf("storeLimitOrder failed: %w", err)
		}
	default:
		panic("ValidateOrder should have caught this")
	}

	if N != 1 {
		err = fmt.Errorf("failed to store order %v: %d rows affected, expected 1",
			ord.UID(), N)
		a.fatalBackendErr(err)
		return err
	}

	return nil
}

func (a *Archiver) orderTableName(ord order.Order) (string, pgOrderStatus, error) {
	return a.orderTableNameWithExecutor(a.db, ord)
}

func (a *Archiver) orderTableNameWithExecutor(dbe sqlQueryer, ord order.Order) (string, pgOrderStatus, error) {
	status, orderType, _, err := a.orderStatusWithExecutor(dbe, ord)
	if err != nil {
		return "", status, err
	}

	marketSchema, err := a.marketSchema(ord.Base(), ord.Quote())
	if err != nil {
		return "", status, err
	}

	var tableName string
	switch orderType {
	case order.MarketOrderType, order.LimitOrderType:
		tableName = fullOrderTableName(a.dbName, marketSchema, status.active())
	case order.CancelOrderType:
		tableName = fullCancelOrderTableName(a.dbName, marketSchema, status.active())
	default:
		return "", status, fmt.Errorf("unrecognized order type %v", orderType)
	}
	return tableName, status, nil
}

func (a *Archiver) OrderPreimage(ord order.Order) (order.Preimage, error) {
	var pi order.Preimage

	tableName, _, err := a.orderTableName(ord)
	if err != nil {
		return pi, err
	}

	stmt := fmt.Sprintf(internal.SelectOrderPreimage, tableName)
	err = a.db.QueryRow(stmt, ord.ID()).Scan(&pi)
	return pi, err
}

func (a *Archiver) storePreimage(dbe sqlQueryExecutor, ord order.Order, pi order.Preimage) error {
	tableName, status, err := a.orderTableNameWithExecutor(dbe, ord)
	if err != nil {
		return err
	}

	// Preimages are stored during epoch processing, specifically after users
	// have responded with their preimages but before swap negotiation begins.
	// Thus, this order should be "active" i.e. not in an archived orders table.
	if !status.active() {
		log.Warnf("Attempting to set preimage for archived order %v", ord.UID())
	}

	stmt := fmt.Sprintf(internal.SetOrderPreimage, tableName)
	N, err := sqlExec(dbe, stmt, pi, ord.ID())
	if err != nil {
		a.fatalBackendErr(err)
		return err
	}
	if N != 1 {
		return fmt.Errorf("failed to update 1 order's preimage, updated %d", N)
	}
	return nil
}

// setOrderCompleteTime sets the successful swap completion time for an
// existing order. It is an error if the order is not in executed status.
func (a *Archiver) setOrderCompleteTime(dbe sqlQueryExecutor, ord order.Order, compTimeMs int64) error {
	status, orderType, _, err := a.orderStatusWithExecutor(dbe, ord)
	if err != nil {
		return err
	}

	if status != orderStatusExecuted { // complete_time is only set for executed orders, not canceled or revoked
		log.Warnf("Attempting to set swap completion time for order %v in status %v, not executed",
			ord.UID(), status)
		return db.ArchiveError{
			Code: db.ErrOrderNotExecuted,
			Detail: fmt.Sprintf("unable to set completed time for order %v in status %v, not executed",
				ord.UID(), status),
		}
	}

	marketSchema, err := a.marketSchema(ord.Base(), ord.Quote())
	if err != nil {
		return db.ArchiveError{
			Code: db.ErrInvalidOrder,
			Detail: fmt.Sprintf("unknown market (%d, %d) for order %v",
				ord.Base(), ord.Quote(), ord.UID()),
		}
	}

	var tableName string
	switch orderType {
	case order.MarketOrderType, order.LimitOrderType:
		tableName = fullOrderTableName(a.dbName, marketSchema, status.active())
	case order.CancelOrderType:
		tableName = fullCancelOrderTableName(a.dbName, marketSchema, status.active())
	default:
		return db.ArchiveError{
			Code:   db.ErrInvalidOrder,
			Detail: fmt.Sprintf("unknown type for order %v: %v", ord.UID(), orderType),
		}
	}

	stmt := fmt.Sprintf(internal.SetOrderCompleteTime, tableName)
	N, err := sqlExec(dbe, stmt, compTimeMs, ord.ID())
	if err != nil {
		a.fatalBackendErr(err)
		return db.ArchiveError{
			Code:   db.ErrGeneralFailure,
			Detail: "SetOrderCompleteTime failed:" + err.Error(),
		}
	}
	if N != 1 {
		return db.ArchiveError{
			Code:   db.ErrUpdateCount,
			Detail: fmt.Sprintf("failed to update 1 order's completion time, updated %d", N),
		}
	}
	return nil
}

func (a *Archiver) setOrderCompleteTimeByID(dbe sqlQueryExecutor, oid order.OrderID, base, quote uint32, compTimeMs int64) error {
	status, orderType, _, err := a.orderStatusByIDWithExecutor(dbe, oid, base, quote)
	if err != nil {
		return err
	}

	if status != orderStatusExecuted {
		log.Warnf("Attempting to set swap completion time for order %v in status %v, not executed",
			oid, status)
		return db.ArchiveError{
			Code: db.ErrOrderNotExecuted,
			Detail: fmt.Sprintf("unable to set completed time for order %v in status %v, not executed",
				oid, status),
		}
	}

	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return db.ArchiveError{
			Code: db.ErrInvalidOrder,
			Detail: fmt.Sprintf("unknown market (%d, %d) for order %v",
				base, quote, oid),
		}
	}

	var tableName string
	switch orderType {
	case order.MarketOrderType, order.LimitOrderType:
		tableName = fullOrderTableName(a.dbName, marketSchema, status.active())
	case order.CancelOrderType:
		tableName = fullCancelOrderTableName(a.dbName, marketSchema, status.active())
	default:
		return db.ArchiveError{
			Code:   db.ErrInvalidOrder,
			Detail: fmt.Sprintf("unknown type for order %v: %v", oid, orderType),
		}
	}

	stmt := fmt.Sprintf(internal.SetOrderCompleteTime, tableName)
	N, err := sqlExec(dbe, stmt, compTimeMs, oid)
	if err != nil {
		a.fatalBackendErr(err)
		return db.ArchiveError{
			Code:   db.ErrGeneralFailure,
			Detail: fmt.Sprintf("error setting completion time for order %v", oid),
		}
	}
	if N != 1 {
		return db.ArchiveError{
			Code:   db.ErrUnknownOrder,
			Detail: fmt.Sprintf("update count = %d for order %v, expected 1", N, oid),
		}
	}
	return nil
}

type orderCompStamped struct {
	oid order.OrderID
	t   int64
}

// CompletedUserOrders retrieves the N most recently completed orders for a user
// across all markets.
func (a *Archiver) CompletedUserOrders(aid account.AccountID, N int) (oids []order.OrderID, compTimes []int64, err error) {
	var ords []orderCompStamped

	for schema := range a.markets {
		tableName := fullOrderTableName(a.dbName, schema, false) // NOT active table
		ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
		mktOids, err := completedUserOrders(ctx, a.db, tableName, aid, N)
		cancel()
		if err != nil {
			return nil, nil, err
		}
		ords = append(ords, mktOids...)
	}

	sort.Slice(ords, func(i, j int) bool {
		return ords[i].t > ords[j].t // descending, latest completed order first
	})

	if N > len(ords) {
		N = len(ords)
	}

	for i := range ords[:N] {
		oids = append(oids, ords[i].oid)
		compTimes = append(compTimes, ords[i].t)
	}

	return
}

func completedUserOrders(ctx context.Context, dbe *sql.DB, tableName string, aid account.AccountID, N int) (oids []orderCompStamped, err error) {
	stmt := fmt.Sprintf(internal.RetrieveCompletedOrdersForAccount, tableName)
	var rows *sql.Rows
	rows, err = dbe.QueryContext(ctx, stmt, aid, N)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var oid order.OrderID
		var acct account.AccountID
		var completeTime sql.NullInt64
		err = rows.Scan(&oid, &acct, &completeTime)
		if err != nil {
			return nil, err
		}

		oids = append(oids, orderCompStamped{oid, completeTime.Int64})
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return
}

// OrderStatusByID gets the status, type, and filled amount of the order with
// the given OrderID in the market specified by a base and quote asset. See also
// OrderStatus. If the order is not found, the error value is ErrUnknownOrder,
// and the type is order.OrderStatusUnknown.
func (a *Archiver) OrderStatusByID(oid order.OrderID, base, quote uint32) (order.OrderStatus, order.OrderType, int64, error) {
	pgStatus, orderType, filled, err := a.orderStatusByID(oid, base, quote)
	return pgToMarketStatus(pgStatus), orderType, filled, err
}

func (a *Archiver) orderStatusByID(oid order.OrderID, base, quote uint32) (pgOrderStatus, order.OrderType, int64, error) {
	return a.orderStatusByIDWithExecutor(a.db, oid, base, quote)
}

func (a *Archiver) orderStatusByIDWithExecutor(dbe sqlQueryer, oid order.OrderID, base, quote uint32) (pgOrderStatus, order.OrderType, int64, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return orderStatusUnknown, order.UnknownOrderType, -1, err
	}
	status, orderType, filled, err := orderStatus(dbe, oid, a.dbName, marketSchema)
	if db.IsErrOrderUnknown(err) {
		status, err = cancelOrderStatus(dbe, oid, a.dbName, marketSchema)
		if err != nil {
			// The severity of an unknown order is up to the caller.
			if !db.IsErrOrderUnknown(err) {
				a.fatalBackendErr(err)
			}
			return orderStatusUnknown, order.UnknownOrderType, -1, err // includes ErrUnknownOrder
		}
		filled = -1
		orderType = order.CancelOrderType
	}
	return status, orderType, filled, err
}

// OrderStatus gets the status, ID, and filled amount of the given order. See
// also OrderStatusByID.
func (a *Archiver) OrderStatus(ord order.Order) (order.OrderStatus, order.OrderType, int64, error) {
	return a.OrderStatusByID(ord.ID(), ord.Base(), ord.Quote())
}

func (a *Archiver) orderStatusWithExecutor(dbe sqlQueryer, ord order.Order) (pgOrderStatus, order.OrderType, int64, error) {
	return a.orderStatusByIDWithExecutor(dbe, ord.ID(), ord.Base(), ord.Quote())
}

func (a *Archiver) updateOrderStatusByIDWithExecutor(dbe sqlQueryExecutor, oid order.OrderID, base, quote uint32, status pgOrderStatus, filled int64) error {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return err
	}

	initStatus, orderType, initFilled, err := a.orderStatusByIDWithExecutor(dbe, oid, base, quote)
	if err != nil {
		return err
	}

	if initStatus == status && filled == initFilled {
		log.Tracef("Not updating order with no status or filled amount change: %v.", oid)
		return nil
	}
	if filled == -1 {
		filled = initFilled
	}

	tableChange := status.active() != initStatus.active()

	if !initStatus.active() {
		if tableChange {
			return fmt.Errorf("Moving an order from an archived to active status: "+
				"Order %s (%s -> %s)", oid, initStatus, status)
		}
		log.Infof("Archived order is changing status: Order %s (%s -> %s)",
			oid, initStatus, status)
	}

	switch orderType {
	case order.LimitOrderType, order.MarketOrderType:
		srcTableName := fullOrderTableName(a.dbName, marketSchema, initStatus.active())
		if tableChange {
			dstTableName := fullOrderTableName(a.dbName, marketSchema, status.active())
			return a.moveOrderWithExecutor(dbe, oid, srcTableName, dstTableName, status, filled)
		}

		// No table move, just update the order.
		return updateOrderStatusAndFilledAmt(dbe, srcTableName, oid, status, uint64(filled))

	case order.CancelOrderType:
		srcTableName := fullCancelOrderTableName(a.dbName, marketSchema, initStatus.active())
		if tableChange {
			dstTableName := fullCancelOrderTableName(a.dbName, marketSchema, status.active())
			return a.moveCancelOrderWithExecutor(dbe, oid, srcTableName, dstTableName, status)
		}

		// No table move, just update the order.
		return updateCancelOrderStatus(dbe, srcTableName, oid, status)
	default:
		return fmt.Errorf("unsupported order type: %v", orderType)
	}
}

// updateOrderStatusWithExecutor updates the status and filled amount of the
// given order. Both the market and new filled amount are determined from the
// Order, and orderStatusByIDWithExecutor is used to locate the existing order.
func (a *Archiver) updateOrderStatusWithExecutor(dbe sqlQueryExecutor, ord order.Order, status pgOrderStatus) error {
	var filled int64
	if ord.Type() != order.CancelOrderType {
		filled = int64(ord.Trade().Filled())
	}
	return a.updateOrderStatusByIDWithExecutor(dbe, ord.ID(), ord.Base(), ord.Quote(), status, filled)
}

func (a *Archiver) moveOrderWithExecutor(dbe sqlExecutor, oid order.OrderID, srcTableName, dstTableName string, status pgOrderStatus, filled int64) error {
	// Move the order, updating status and filled amount.
	moved, err := moveOrder(dbe, srcTableName, dstTableName, oid,
		status, uint64(filled))
	if err != nil {
		a.fatalBackendErr(err)
		return err
	}
	if !moved {
		return fmt.Errorf("order %s not moved from %s to %s", oid, srcTableName, dstTableName)
	}
	return nil
}

func (a *Archiver) moveCancelOrderWithExecutor(dbe sqlExecutor, oid order.OrderID, srcTableName, dstTableName string, status pgOrderStatus) error {
	// Move the order, updating status and filled amount.
	moved, err := moveCancelOrder(dbe, srcTableName, dstTableName, oid,
		status)
	if err != nil {
		a.fatalBackendErr(err)
		return err
	}
	if !moved {
		return fmt.Errorf("cancel order %s not moved from %s to %s", oid, srcTableName, dstTableName)
	}
	return nil
}

// updateOrderFilledByIDWithExecutor updates the filled amount of the order
// with the given OrderID in the market specified by a base and quote asset.
// This function applies only to market and limit orders, not cancel orders.
// The order's status is used to locate the existing order. If the order is not
// found, the error value is ErrUnknownOrder.
func (a *Archiver) updateOrderFilledByIDWithExecutor(dbe sqlQueryExecutor, oid order.OrderID, base, quote uint32, filled int64) error {
	// Locate the order.
	status, orderType, initFilled, err := a.orderStatusByIDWithExecutor(dbe, oid, base, quote)
	if err != nil {
		return err
	}

	switch orderType {
	case order.MarketOrderType, order.LimitOrderType:
	default:
		return fmt.Errorf("cannot set filled amount for order type %v", orderType)
	}

	if filled == initFilled {
		return nil // nothing to do
	}

	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return err // should be caught already by a.OrderStatusByID
	}
	tableName := fullOrderTableName(a.dbName, marketSchema, status.active())
	err = updateOrderFilledAmt(dbe, tableName, oid, uint64(filled))
	if err != nil {
		a.fatalBackendErr(err) // TODO: it could have changed tables since this function is not atomic
	}
	return err
}

// UserOrderStatuses retrieves the statuses and filled amounts of the orders
// with the provided order IDs for the given account in the market specified
// by a base and quote asset.
// The number and ordering of the returned statuses is not necessarily the same
// as the number and ordering of the provided order IDs. It is not an error if
// any or all of the provided order IDs cannot be found for the given account
// in the specified market.
func (a *Archiver) UserOrderStatuses(aid account.AccountID, base, quote uint32, oids []order.OrderID) ([]*db.OrderStatus, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}

	// Active orders.
	fullTable := fullOrderTableName(a.dbName, marketSchema, true)
	activeOrderStatuses, err := a.userOrderStatusesFromTable(fullTable, aid, oids)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.fatalBackendErr(err)
		log.Errorf("Failed to query for active order statuses by user for market %v and account %v",
			marketSchema, aid)
		return nil, err
	}

	if len(oids) == len(activeOrderStatuses) {
		return activeOrderStatuses, nil
	}

	foundOrders := make(map[order.OrderID]bool, len(activeOrderStatuses))
	for _, status := range activeOrderStatuses {
		foundOrders[status.ID] = true
	}
	var remainingOids []order.OrderID
	for _, oid := range oids {
		if !foundOrders[oid] {
			remainingOids = append(remainingOids, oid)
		}
	}

	// Archived Orders.
	fullTable = fullOrderTableName(a.dbName, marketSchema, false)
	archivedOrderStatuses, err := a.userOrderStatusesFromTable(fullTable, aid, remainingOids)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.fatalBackendErr(err)
		log.Errorf("Failed to query for archived order statuses by user for market %v and account %v",
			marketSchema, aid)
		return nil, err
	}

	return append(activeOrderStatuses, archivedOrderStatuses...), nil
}

// ActiveUserOrderStatuses retrieves the statuses and filled amounts of all
// active orders for a user across all markets.
func (a *Archiver) ActiveUserOrderStatuses(aid account.AccountID) ([]*db.OrderStatus, error) {
	var orders []*db.OrderStatus
	for schema := range a.markets {
		tableName := fullOrderTableName(a.dbName, schema, true) // active table
		mktOrders, err := a.userOrderStatusesFromTable(tableName, aid, nil)
		if err != nil {
			return nil, err
		}
		orders = append(orders, mktOrders...)
	}
	return orders, nil
}

// Pass nil or empty oids to return statuses for all user orders in the
// specified table.
func (a *Archiver) userOrderStatusesFromTable(fullTable string, aid account.AccountID, oids []order.OrderID) ([]*db.OrderStatus, error) {
	execQuery := func(ctx context.Context) (*sql.Rows, error) {
		if len(oids) == 0 {
			stmt := fmt.Sprintf(internal.SelectUserOrderStatuses, fullTable)
			return a.db.QueryContext(ctx, stmt, aid)
		}
		oidArr := make(pq.ByteaArray, 0, len(oids))
		for i := range oids {
			oidArr = append(oidArr, oids[i][:])
		}
		stmt := fmt.Sprintf(internal.SelectUserOrderStatusesByID, fullTable)
		return a.db.QueryContext(ctx, stmt, aid, oidArr)
	}

	ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
	rows, err := execQuery(ctx)
	defer cancel()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := make([]*db.OrderStatus, 0, len(oids))
	for rows.Next() {
		var oid order.OrderID
		var status pgOrderStatus
		err = rows.Scan(&oid, &status)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, &db.OrderStatus{
			ID:     oid,
			Status: pgToMarketStatus(status),
		})
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return statuses, nil
}

func (a *Archiver) OrdersWithCommit(ctx context.Context, base, quote uint32,
	commit order.Commitment, archivedCutoff time.Time) ([]db.CommitOrder, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}
	found, oid, err := orderForCommit(ctx, a.db, a.dbName, marketSchema, commit)
	if err != nil {
		a.fatalBackendErr(err)
		return nil, err
	}
	oids := []order.OrderID{}
	if found {
		oids = append(oids, oid)
	}
	archived, err := archivedOrdersForCommitSince(a.db, a.dbName, marketSchema, commit, archivedCutoff)
	if err != nil {
		a.fatalBackendErr(err)
		return nil, err
	}
	oids = append(oids, archived...)

	out := make([]db.CommitOrder, 0, len(oids))
	for _, oid := range oids {
		ord, status, err := a.Order(oid, base, quote)
		if err != nil {
			return nil, err
		}
		out = append(out, db.CommitOrder{Order: ord, Status: status})
	}
	return out, nil
}

func archivedOrdersForCommitSince(dbe sqlQueryer, dbName, marketSchema string,
	commit order.Commitment, cutoff time.Time) ([]order.OrderID, error) {
	var oids []order.OrderID
	scan := func(fullTable string) error {
		stmt := fmt.Sprintf(internal.SelectOrderByCommitSince, fullTable)
		rows, err := dbe.Query(stmt, commit, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var oid order.OrderID
			if err := rows.Scan(&oid); err != nil {
				return err
			}
			oids = append(oids, oid)
		}
		return rows.Err()
	}
	if err := scan(fullOrderTableName(dbName, marketSchema, false)); err != nil {
		return nil, err
	}
	if err := scan(fullCancelOrderTableName(dbName, marketSchema, false)); err != nil {
		return nil, err
	}
	return oids, nil
}

// orderWithCommit searches all markets' active trade and cancel orders for
// the given Commitment.
func (a *Archiver) orderWithCommit(ctx context.Context, dbe sqlQueryer, commit order.Commitment) (found bool, oid order.OrderID, err error) {
	// Check all markets.
	for marketSchema := range a.markets {
		found, oid, err = orderForCommit(ctx, dbe, a.dbName, marketSchema, commit)
		if err != nil {
			a.fatalBackendErr(err)
			log.Errorf("Failed to query for orders by commit for market %v and commit %v",
				marketSchema, commit)
			return
		}
		if found {
			return
		}
	}
	return // false, zero, nil
}

// BEGIN regular order functions

func orderStatus(dbe sqlQueryer, oid order.OrderID, dbName, marketSchema string) (pgOrderStatus, order.OrderType, int64, error) {
	// Search active orders first.
	fullTable := fullOrderTableName(dbName, marketSchema, true)
	found, status, orderType, filled, err := findOrder(dbe, oid, fullTable)
	if err != nil {
		return orderStatusUnknown, order.UnknownOrderType, -1, err
	}
	if found {
		return status, orderType, filled, nil
	}

	// Search archived orders.
	fullTable = fullOrderTableName(dbName, marketSchema, false)
	found, status, orderType, filled, err = findOrder(dbe, oid, fullTable)
	if err != nil {
		return orderStatusUnknown, order.UnknownOrderType, -1, err
	}
	if found {
		return status, orderType, filled, nil
	}

	// Order not found in either orders table.
	return orderStatusUnknown, order.UnknownOrderType, -1, db.ArchiveError{Code: db.ErrUnknownOrder}
}

func findOrder(dbe sqlQueryer, oid order.OrderID, fullTable string) (bool, pgOrderStatus, order.OrderType, int64, error) {
	stmt := fmt.Sprintf(internal.OrderStatus, fullTable)
	var status pgOrderStatus
	var filled int64
	var orderType order.OrderType
	err := dbe.QueryRow(stmt, oid).Scan(&orderType, &status, &filled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, orderStatusUnknown, order.UnknownOrderType, -1, nil
	case err == nil:
		return true, status, orderType, filled, nil
	default:
		return false, orderStatusUnknown, order.UnknownOrderType, -1, err
	}
}

// loadTrade does NOT set BaseAsset and QuoteAsset!
func loadTrade(dbe *sql.DB, dbName, marketSchema string, oid order.OrderID) (order.Order, pgOrderStatus, error) {
	// Search active orders first.
	fullTable := fullOrderTableName(dbName, marketSchema, true)
	ord, status, err := loadTradeFromTable(dbe, fullTable, oid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// try archived orders next
	case err == nil:
		// found
		return ord, status, nil
	default:
		// query error
		return ord, orderStatusUnknown, err
	}

	// Search archived orders.
	fullTable = fullOrderTableName(dbName, marketSchema, false)
	ord, status, err = loadTradeFromTable(dbe, fullTable, oid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, orderStatusUnknown, db.ArchiveError{Code: db.ErrUnknownOrder}
	case err == nil:
		// found
		return ord, status, nil
	default:
		// query error
		return nil, orderStatusUnknown, err
	}
}

// loadTradeFromTable does NOT set BaseAsset and QuoteAsset!
func loadTradeFromTable(dbe sqlQueryer, fullTable string, oid order.OrderID) (order.Order, pgOrderStatus, error) {
	stmt := fmt.Sprintf(internal.SelectOrder, fullTable)

	var prefix order.Prefix
	var trade order.Trade
	var id order.OrderID
	var tif order.TimeInForce
	var rate uint64
	var status pgOrderStatus
	err := dbe.QueryRow(stmt, oid).Scan(&id, &prefix.OrderType, &trade.Sell,
		&prefix.AccountID, &trade.Address, &prefix.ClientTime, &prefix.ServerTime,
		&prefix.Commit, (*dbCoins)(&trade.Coins),
		&trade.Quantity, &rate, &tif, &status, &trade.FillAmt)
	if err != nil {
		return nil, orderStatusUnknown, err
	}
	switch prefix.OrderType {
	case order.LimitOrderType:
		return &order.LimitOrder{
			T:     *trade.Copy(), // govet would complain because Trade has a Mutex
			P:     prefix,
			Rate:  rate,
			Force: tif,
		}, status, nil
	case order.MarketOrderType:
		return &order.MarketOrder{
			T: *trade.Copy(),
			P: prefix,
		}, status, nil

	}
	return nil, 0, fmt.Errorf("unknown order type %d retrieved", prefix.OrderType)
}

func cancelOrdersByStatusFromTable(ctx context.Context, dbe *sql.DB, fullTable string, base, quote uint32, status pgOrderStatus) ([]*order.CancelOrder, error) {
	stmt := fmt.Sprintf(internal.SelectCancelOrdersByStatus, fullTable)
	rows, err := dbe.QueryContext(ctx, stmt, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cos []*order.CancelOrder

	for rows.Next() {
		var co order.CancelOrder
		co.OrderType = order.CancelOrderType
		err := rows.Scan(&co.AccountID, &co.ClientTime,
			&co.ServerTime, &co.Commit, &co.TargetOrderID)
		if err != nil {
			return nil, err
		}
		co.BaseAsset, co.QuoteAsset = base, quote
		cos = append(cos, &co)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return cos, nil
}

// base and quote are used to set the prefix, not specify which table to search.
func ordersByStatusFromTable(ctx context.Context, dbe *sql.DB, fullTable string, base, quote uint32, status pgOrderStatus) ([]order.Order, error) {
	stmt := fmt.Sprintf(internal.SelectOrdersByStatus, fullTable)
	rows, err := dbe.QueryContext(ctx, stmt, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []order.Order

	for rows.Next() {
		var prefix order.Prefix
		var trade order.Trade
		var id order.OrderID
		var tif order.TimeInForce
		var rate uint64
		err = rows.Scan(&id, &prefix.OrderType, &trade.Sell,
			&prefix.AccountID, &trade.Address, &prefix.ClientTime, &prefix.ServerTime,
			&prefix.Commit, (*dbCoins)(&trade.Coins),
			&trade.Quantity, &rate, &tif, &trade.FillAmt)
		if err != nil {
			return nil, err
		}
		prefix.BaseAsset, prefix.QuoteAsset = base, quote

		var ord order.Order
		switch prefix.OrderType {
		case order.LimitOrderType:
			ord = &order.LimitOrder{
				P:     prefix,
				T:     *trade.Copy(),
				Rate:  rate,
				Force: tif,
			}
		case order.MarketOrderType:
			ord = &order.MarketOrder{
				P: prefix,
				T: *trade.Copy(),
			}
		default:
			log.Errorf("ordersByStatusFromTable: encountered unexpected order type %v",
				prefix.OrderType)
			continue
		}

		orders = append(orders, ord)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return orders, nil
}

func orderForCommit(ctx context.Context, dbe sqlQueryer, dbName, marketSchema string, commit order.Commitment) (bool, order.OrderID, error) {
	var zeroOrderID order.OrderID

	execCheckOrderStmt := func(stmt string) (bool, order.OrderID, error) {
		var oid order.OrderID
		err := dbe.QueryRowContext(ctx, stmt, commit).Scan(&oid)
		if err == nil {
			return true, oid, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, zeroOrderID, err
		}
		// sql.ErrNoRows
		return false, zeroOrderID, nil
	}

	checkTradeOrders := func() (bool, order.OrderID, error) {
		fullTable := fullOrderTableName(dbName, marketSchema, true)
		stmt := fmt.Sprintf(internal.SelectOrderByCommit, fullTable)
		return execCheckOrderStmt(stmt)
	}

	checkCancelOrders := func() (bool, order.OrderID, error) {
		fullTable := fullCancelOrderTableName(dbName, marketSchema, true)
		stmt := fmt.Sprintf(internal.SelectOrderByCommit, fullTable)
		return execCheckOrderStmt(stmt)
	}

	found, oid, err := checkTradeOrders()
	if found || err != nil {
		return found, oid, err
	}
	return checkCancelOrders()
}

func storeLimitOrder(dbe sqlExecutor, tableName string, lo *order.LimitOrder, status pgOrderStatus, epochIdx, epochDur int64) (int64, error) {
	stmt := fmt.Sprintf(internal.InsertOrder, tableName)
	return sqlExec(dbe, stmt, lo.ID(), lo.Type(), lo.Sell, lo.AccountID,
		lo.Address, lo.ClientTime, lo.ServerTime, lo.Commit, dbCoins(lo.Coins),
		lo.Quantity, lo.Rate, lo.Force, status, lo.Filled(), epochIdx, epochDur)
}

func storeMarketOrder(dbe sqlExecutor, tableName string, mo *order.MarketOrder, status pgOrderStatus, epochIdx, epochDur int64) (int64, error) {
	stmt := fmt.Sprintf(internal.InsertOrder, tableName)
	return sqlExec(dbe, stmt, mo.ID(), mo.Type(), mo.Sell, mo.AccountID,
		mo.Address, mo.ClientTime, mo.ServerTime, mo.Commit, dbCoins(mo.Coins),
		mo.Quantity, 0, order.ImmediateTiF, status, mo.Filled(), epochIdx, epochDur)
}

func updateOrderStatus(dbe sqlExecutor, tableName string, oid order.OrderID, status pgOrderStatus) error {
	stmt := fmt.Sprintf(internal.UpdateOrderStatus, tableName)
	_, err := dbe.Exec(stmt, status, oid)
	return err
}

func updateOrderFilledAmt(dbe sqlExecutor, tableName string, oid order.OrderID, filled uint64) error {
	stmt := fmt.Sprintf(internal.UpdateOrderFilledAmt, tableName)
	_, err := dbe.Exec(stmt, filled, oid)
	return err
}

func updateOrderStatusAndFilledAmt(dbe sqlExecutor, tableName string, oid order.OrderID, status pgOrderStatus, filled uint64) error {
	stmt := fmt.Sprintf(internal.UpdateOrderStatusAndFilledAmt, tableName)
	_, err := dbe.Exec(stmt, status, filled, oid)
	return err
}

func moveOrder(dbe sqlExecutor, oldTableName, newTableName string, oid order.OrderID, newStatus pgOrderStatus, newFilled uint64) (bool, error) {
	stmt := fmt.Sprintf(internal.MoveOrder, oldTableName, newStatus, newFilled, newTableName)
	moved, err := sqlExec(dbe, stmt, oid)
	if err != nil {
		return false, err
	}
	if moved != 1 {
		panic(fmt.Sprintf("moved %d orders instead of 1", moved))
	}
	return true, nil
}

// END regular order functions

// BEGIN cancel order functions

func storeCancelOrder(dbe sqlExecutor, tableName string, co *order.CancelOrder, status pgOrderStatus, epochIdx, epochDur int64, epochGap int32) (int64, error) {
	stmt := fmt.Sprintf(internal.InsertCancelOrder, tableName)
	return sqlExec(dbe, stmt, co.ID(), co.AccountID, co.ClientTime,
		co.ServerTime, co.Commit, co.TargetOrderID, status, epochIdx, epochDur, epochGap)
}

// loadCancelOrderFromTable does NOT set BaseAsset and QuoteAsset!
func loadCancelOrderFromTable(dbe *sql.DB, fullTable string, oid order.OrderID) (*order.CancelOrder, pgOrderStatus, error) {
	stmt := fmt.Sprintf(internal.SelectCancelOrder, fullTable)

	var co order.CancelOrder
	var id order.OrderID
	var status pgOrderStatus
	err := dbe.QueryRow(stmt, oid).Scan(&id, &co.AccountID, &co.ClientTime,
		&co.ServerTime, &co.Commit, &co.TargetOrderID, &status)
	if err != nil {
		return nil, orderStatusUnknown, err
	}

	co.OrderType = order.CancelOrderType

	return &co, status, nil
}

// loadCancelOrder does NOT set BaseAsset and QuoteAsset!
func loadCancelOrder(dbe *sql.DB, dbName, marketSchema string, oid order.OrderID) (*order.CancelOrder, pgOrderStatus, error) {
	// Search active orders first.
	fullTable := fullCancelOrderTableName(dbName, marketSchema, true)
	co, status, err := loadCancelOrderFromTable(dbe, fullTable, oid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	// try archived orders next
	case err == nil:
		// found
		return co, status, nil
	default:
		// query error
		return co, orderStatusUnknown, err
	}

	// Search archived orders.
	fullTable = fullCancelOrderTableName(dbName, marketSchema, false)
	co, status, err = loadCancelOrderFromTable(dbe, fullTable, oid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, orderStatusUnknown, db.ArchiveError{Code: db.ErrUnknownOrder}
	case err == nil:
		// found
		return co, status, nil
	default:
		// query error
		return nil, orderStatusUnknown, err
	}
}

func cancelOrderStatus(dbe sqlQueryer, oid order.OrderID, dbName, marketSchema string) (pgOrderStatus, error) {
	// Search active orders first.
	found, status, err := findCancelOrder(dbe, oid, dbName, marketSchema, true)
	if err != nil {
		return orderStatusUnknown, err
	}
	if found {
		return status, nil
	}

	// Search archived orders.
	found, status, err = findCancelOrder(dbe, oid, dbName, marketSchema, false)
	if err != nil {
		return orderStatusUnknown, err
	}
	if found {
		return status, nil
	}

	// Order not found in either orders table.
	return orderStatusUnknown, db.ArchiveError{Code: db.ErrUnknownOrder}
}

func (a *Archiver) cancelOrderEpochGapWithExecutor(dbe sqlQueryer, oid order.OrderID, base, quote uint32) (int32, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return db.EpochGapNA, err
	}
	epochGap, found, err := findCancelOrderEpochGap(dbe, oid, a.dbName, marketSchema, true)
	if err != nil {
		return db.EpochGapNA, err
	}
	if found {
		return epochGap, nil
	}
	epochGap, found, err = findCancelOrderEpochGap(dbe, oid, a.dbName, marketSchema, false)
	if err != nil {
		return db.EpochGapNA, err
	}
	if found {
		return epochGap, nil
	}
	return db.EpochGapNA, db.ArchiveError{Code: db.ErrUnknownOrder}
}

func findCancelOrderEpochGap(dbe sqlQueryer, oid order.OrderID, dbName, marketSchema string, active bool) (int32, bool, error) {
	fullTable := fullCancelOrderTableName(dbName, marketSchema, active)
	stmt := fmt.Sprintf("SELECT epoch_gap FROM %s WHERE oid = $1", fullTable)
	var epochGap int32
	err := dbe.QueryRow(stmt, oid).Scan(&epochGap)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return db.EpochGapNA, false, nil
	case err == nil:
		return epochGap, true, nil
	default:
		return db.EpochGapNA, false, err
	}
}

func findCancelOrder(dbe sqlQueryer, oid order.OrderID, dbName, marketSchema string, active bool) (bool, pgOrderStatus, error) {
	fullTable := fullCancelOrderTableName(dbName, marketSchema, active)
	stmt := fmt.Sprintf(internal.CancelOrderStatus, fullTable)
	var status pgOrderStatus
	err := dbe.QueryRow(stmt, oid).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, orderStatusUnknown, nil
	case err == nil:
		return true, status, nil
	default:
		return false, orderStatusUnknown, err
	}
}

func updateCancelOrderStatus(dbe sqlExecutor, tableName string, oid order.OrderID, status pgOrderStatus) error {
	return updateOrderStatus(dbe, tableName, oid, status)
}

func moveCancelOrder(dbe sqlExecutor, oldTableName, newTableName string, oid order.OrderID, newStatus pgOrderStatus) (bool, error) {
	stmt := fmt.Sprintf(internal.MoveCancelOrder, oldTableName, newStatus, newTableName)
	moved, err := sqlExec(dbe, stmt, oid)
	if err != nil {
		return false, err
	}
	if moved != 1 {
		panic(fmt.Sprintf("moved %d cancel orders instead of 1", moved))
	}
	return true, nil
}

// END cancel order functions
