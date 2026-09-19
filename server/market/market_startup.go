// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

// encodeStartupOrderRevokes encodes the revoked orders and includes their
// revocation reasons in event records.
func encodeStartupOrderRevokes(revokes []*db.StartupOrderRevoke) []meshevents.StartupOrderRevokeRecord {
	records := make([]meshevents.StartupOrderRevokeRecord, 0, len(revokes))
	for _, revoke := range revokes {
		records = append(records, meshevents.NewStartupOrderRevokeRecord(revoke.Order, revoke.Reason))
	}
	return records
}

// applyMarketStartedMemory removes revoked orders, unlocks their funding
// coins, and resets the market's epoch queues. It returns the orders removed
// from the book.
func (m *Market) applyMarketStartedMemory(update *db.MarketStartedUpdate) (removed []*order.LimitOrder) {
	removed = m.applyMarketStartedBook(update)
	m.applyMarketStartedEpochs(update)
	// Unlock every revoked epoch order, including orders that were not
	// restored into the epoch queues.
	for _, revoke := range update.EpochRevokes {
		m.unlockOrderCoins(revoke.Order)
	}
	return removed
}

// applyMarketStartedBook removes revoked orders from the book, unlocks their
// funding coins, and sets the book epoch. It returns the orders actually
// removed from the book.
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

// applyMarketStartedEpochs clears the epoch orders and commitments, creates
// empty queues for the current and next epochs, and sets the market's start
// and active epoch indexes.
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
