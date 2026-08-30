// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/book"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

// submitMarketStarted runs master-side startup cleanup checks and submits the
// market_started event that every node projects into memory.
func (m *Market) submitMarketStarted(ctx context.Context) (currentEpochIdx int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m.mesh == nil {
		return 0, fmt.Errorf("market %s startup requires SetMeshService before Run", m.name)
	}

	params := m.startParams()
	bookedRevokes, err := m.startupBookedRevokes(ctx, params.LotSize)
	if err != nil {
		return 0, err
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	currentEpochIdx = currentMarketEpochIdx(params.epochDur)
	epochRevokes, err := m.startupEpochRevokes(ctx)
	if err != nil {
		return 0, err
	}
	revocationTime := time.Now().Truncate(time.Millisecond).UTC()
	startedEvent := meshevents.NewMarketStartedEvent(m.name, currentEpochIdx, params.epochDur,
		params.MarketRunParams, revocationTime, encodeStartupOrderRevokes(bookedRevokes))
	startedEvent.EpochRevokes = encodeStartupOrderRevokes(epochRevokes)
	event, err := mesh.NewEvent(startedEvent)
	if err != nil {
		return 0, err
	}
	if _, err := m.mesh.ApplyEvent(ctx, event); err != nil {
		return 0, err
	}
	// The epoch driver (prepareStartup) owns the finalizing-suspend check that
	// decides whether this epoch index actually starts order intake.
	return currentEpochIdx, nil
}

func currentMarketEpochIdx(epochDur int64) int64 {
	return time.Now().Truncate(time.Millisecond).UTC().UnixMilli() / epochDur
}

// startupEpochRevokes returns the epoch-status orders that a market_started
// event will revoke. Any order that is in epoch status when market_started
// is called was not completed by a previous run. Instead of starting a market
// mid-epoch, we just revoke all of these orders and start processing with a new,
// clean epoch.
func (m *Market) startupEpochRevokes(ctx context.Context) ([]*db.StartupOrderRevoke, error) {
	ords, err := m.storage.EpochOrders(m.base, m.quote)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sortOrdersByID(ords)
	revokes := make([]*db.StartupOrderRevoke, 0, len(ords))
	for _, ord := range ords {
		if ord == nil {
			continue
		}
		revokes = append(revokes, &db.StartupOrderRevoke{
			Order:  ord,
			Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned,
		})
	}
	return revokes, nil
}

func (m *Market) startupBookedRevokes(ctx context.Context, lotSize uint64) (bookedRevokes []*db.StartupOrderRevoke, err error) {
	bookOrders, err := m.loadStartupBookOrders(ctx)
	if err != nil {
		return nil, err
	}

	selectedBookedRevokes := make(map[order.OrderID]struct{}, len(bookOrders))
	remaining := make(map[order.OrderID]*order.LimitOrder, len(bookOrders))

	bookedRevokes, baseAcctStats, quoteAcctStats, err := m.scanBookedOrdersForStartupRevokes(ctx, lotSize, bookOrders, selectedBookedRevokes, remaining)
	if err != nil {
		return nil, err
	}
	balanceRevokes, err := m.lowBalanceBookedRevokes(remaining, baseAcctStats, quoteAcctStats, selectedBookedRevokes)
	if err != nil {
		return nil, err
	}
	bookedRevokes = append(bookedRevokes, balanceRevokes...)

	sortStartupOrderRevokes(bookedRevokes)
	return bookedRevokes, nil
}

func (m *Market) loadStartupBookOrders(ctx context.Context) ([]*order.LimitOrder, error) {
	base, quote := m.base, m.quote
	bookOrders, err := m.storage.BookOrders(base, quote)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bookOrders, nil
}

func (m *Market) scanBookedOrdersForStartupRevokes(ctx context.Context, lotSize uint64, bookOrders []*order.LimitOrder,
	selectedBookedRevokes map[order.OrderID]struct{}, remaining map[order.OrderID]*order.LimitOrder) (
	[]*db.StartupOrderRevoke, accountCounter, accountCounter, error) {

	var bookedRevokes []*db.StartupOrderRevoke
	baseAcctStats := make(accountCounter)
	quoteAcctStats := make(accountCounter)

	for _, lo := range bookOrders {
		oid := lo.ID()
		if book.IncompatibleLotSize(lo, lotSize) {
			bookedRevokes = appendStartupBookedRevoke(bookedRevokes, selectedBookedRevokes, lo, meshevents.StartupOrderRevokeReasonLotSizeIncompatible)
			continue
		}
		remaining[oid] = lo
		if lo.FillAmt > 0 {
			continue
		}
		spent, err := m.bookedOrderFundingSpent(ctx, lo)
		if err != nil {
			return nil, nil, nil, err
		}
		if spent {
			bookedRevokes = appendStartupBookedRevoke(bookedRevokes, selectedBookedRevokes, lo, meshevents.StartupOrderRevokeReasonFundingCoinSpent)
			delete(remaining, oid)
			continue
		}

		m.addAccountBackedOrderStats(lo, lotSize, baseAcctStats, quoteAcctStats)
	}
	return bookedRevokes, baseAcctStats, quoteAcctStats, nil
}

func (m *Market) bookedOrderFundingSpent(ctx context.Context, lo *order.LimitOrder) (bool, error) {
	assetID := m.quote
	utxoFunded := m.coinLockerQuote != nil
	if lo.Sell {
		assetID = m.base
		utxoFunded = m.coinLockerBase != nil
	}
	if !utxoFunded {
		return false, nil
	}
	for _, coinID := range lo.Coins {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := func() error {
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return m.swapper.CheckUnspent(callCtx, assetID, coinID)
		}()
		if err == nil {
			continue
		}
		if errors.Is(err, asset.CoinNotFoundError) {
			return true, nil
		}
		// TODO(mesh): on other RPC failures, we skip the check to avoid the entire node
		// going down during failover due a potentially temporary issue. The tradeoff is
		// allowing some orders to remain on the books until they either match or the user
		// tries to submit another order. Consider some alternative options.
		log.Errorf("Unexpected error checking coinID %v for order %v: %v", coinID, lo.ID(), err)
		return false, nil
	}
	return false, nil
}

func (m *Market) addAccountBackedOrderStats(lo *order.LimitOrder, lotSize uint64, baseAcctStats, quoteAcctStats accountCounter) {
	if m.coinLockerBase == nil {
		addr, qty, lots, redeems, ok := startupAccountStats(lo, lotSize, true)
		if !ok {
			log.Errorf("Skipping startup base account balance stats for order %s with invalid account coins", lo.ID())
			return
		}
		baseAcctStats.add(addr, qty, lots, redeems)
	}
	if m.coinLockerQuote == nil {
		addr, qty, lots, redeems, ok := startupAccountStats(lo, lotSize, false)
		if !ok {
			log.Errorf("Skipping startup quote account balance stats for order %s with invalid account coins", lo.ID())
			return
		}
		quoteAcctStats.add(addr, qty, lots, redeems)
	}
}

func (m *Market) lowBalanceBookedRevokes(remaining map[order.OrderID]*order.LimitOrder,
	baseAcctStats, quoteAcctStats accountCounter, selectedBookedRevokes map[order.OrderID]struct{}) (
	[]*db.StartupOrderRevoke, error) {

	failedBaseAccts, err := m.failedStartupAccounts(baseAcctStats, m.base)
	if err != nil {
		return nil, err
	}
	failedQuoteAccts, err := m.failedStartupAccounts(quoteAcctStats, m.quote)
	if err != nil {
		return nil, err
	}

	var revokes []*db.StartupOrderRevoke
	for oid, lo := range remaining {
		if failedBaseAccts[lo.BaseAccount()] || failedQuoteAccts[lo.QuoteAccount()] {
			revokes = appendStartupBookedRevoke(revokes, selectedBookedRevokes, lo, meshevents.StartupOrderRevokeReasonAccountLowBalance)
			delete(remaining, oid)
		}
	}
	return revokes, nil
}

func (m *Market) failedStartupAccounts(stats accountCounter, assetID uint32) (map[string]bool, error) {
	if len(stats) == 0 {
		return nil, nil
	}
	if m.balancer == nil {
		return nil, fmt.Errorf("market %s startup cleanup requires account balancer", m.name)
	}
	failed := make(map[string]bool)
	for acctAddr := range stats {
		if !m.balancer.CheckReserved(acctAddr, assetID) {
			failed[acctAddr] = true
		}
	}
	return failed, nil
}

func appendStartupBookedRevoke(bookedRevokes []*db.StartupOrderRevoke, selected map[order.OrderID]struct{},
	lo *order.LimitOrder, reason meshevents.StartupOrderRevokeReason) []*db.StartupOrderRevoke {

	oid := lo.ID()
	if _, found := selected[oid]; found {
		return bookedRevokes
	}
	selected[oid] = struct{}{}
	return append(bookedRevokes, &db.StartupOrderRevoke{Order: lo, Reason: reason})
}

func startupAccountStats(lo *order.LimitOrder, lotSize uint64, baseAsset bool) (addr string, qty, lots uint64, redeems int, ok bool) {
	if baseAsset {
		if lo.Sell {
			if len(lo.Coins) != 1 {
				return "", 0, 0, 0, false
			}
			return string(lo.Coins[0]), lo.Quantity, lo.Quantity / lotSize, 0, true
		}
		return lo.Address, 0, 0, int((lo.Quantity - lo.FillAmt) / lotSize), true
	}
	if lo.Sell {
		return lo.Address, 0, 0, int((lo.Quantity - lo.FillAmt) / lotSize), true
	}
	if len(lo.Coins) != 1 {
		return "", 0, 0, 0, false
	}
	return string(lo.Coins[0]), calc.BaseToQuote(lo.Rate, lo.Quantity), lo.Quantity / lotSize, 0, true
}

func sortStartupOrderRevokes(revokes []*db.StartupOrderRevoke) {
	sort.Slice(revokes, func(i, j int) bool {
		idi := revokes[i].Order.ID()
		idj := revokes[j].Order.ID()
		if idi == idj {
			return revokes[i].Reason < revokes[j].Reason
		}
		return string(idi[:]) < string(idj[:])
	})
}

// encodeStartupOrderRevokes converts decoded revokes into mesh wire records.
func encodeStartupOrderRevokes(revokes []*db.StartupOrderRevoke) []meshevents.StartupOrderRevokeRecord {
	records := make([]meshevents.StartupOrderRevokeRecord, 0, len(revokes))
	for _, revoke := range revokes {
		records = append(records, meshevents.NewStartupOrderRevokeRecord(revoke.Order, revoke.Reason))
	}
	return records
}

// applyMarketStartedMemory projects a validated market_started event into this
// process's in-memory market state.
func (m *Market) applyMarketStartedMemory(validated *validatedMarketStartedEvent) (removed []*order.LimitOrder) {
	update := validated.update
	removed = m.applyMarketStartedBook(update)
	m.applyMarketStartedEpochs(update)
	// Release the funding coins of every epoch-status order from the event's
	// validated revoke set, which the DB apply required to exactly cover
	// them — queued or not. Queue memory plays no part in the disposal.
	for _, revoke := range update.EpochRevokes {
		m.unlockOrderCoins(revoke.Order)
	}
	return removed
}

// applyMarketStartedBook removes the event's booked revokes from the book,
// advances the book epoch, and releases the revoked orders' funding coins,
// returning the orders actually removed for the router's unbook notes.
func (m *Market) applyMarketStartedBook(update *db.MarketStartedUpdate) (removed []*order.LimitOrder) {
	m.bookMtx.Lock()
	for _, revoke := range update.BookedRevokes {
		if lo, ok := m.book.Remove(revoke.Order.ID()); ok {
			removed = append(removed, lo)
		}
	}
	m.bookEpochIdx = update.CurrentEpochIdx
	m.bookMtx.Unlock()
	for _, revoke := range update.BookedRevokes {
		m.unlockOrderCoins(revoke.Order)
	}
	return removed
}

// applyMarketStartedEpochs replaces all epoch memory with fresh queues at the
// started epoch. Coin unlocks are driven solely by the event's epoch revokes.
func (m *Market) applyMarketStartedEpochs(update *db.MarketStartedUpdate) {
	m.epochMtx.Lock()
	m.epochOrders = make(map[order.OrderID]order.Order)
	m.epochCommitments = make(map[order.Commitment]order.OrderID)
	m.currentEpoch = NewEpoch(update.CurrentEpochIdx, update.EpochDur)
	m.nextEpoch = NewEpoch(update.CurrentEpochIdx+1, update.EpochDur)
	m.startEpochIdx = update.CurrentEpochIdx
	m.activeEpochIdx = update.CurrentEpochIdx
	m.epochMtx.Unlock()
}
