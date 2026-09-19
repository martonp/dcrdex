// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"fmt"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

// LifecycleTransition identifies a change to a market's trading lifecycle.
type LifecycleTransition uint8

const LifecycleTransitionStarted LifecycleTransition = 1

// LifecycleUpdated is a callback that receives the applied lifecycle
// transition and updated market lifecycle.
type LifecycleUpdated func(transition LifecycleTransition, lc *db.MarketLifecycle)

// Events returns the market event appliers keyed by event kind.
func Events(markets map[string]*Market, bookRouter *BookRouter, lifecycleUpdated LifecycleUpdated) map[string]mesh.EventApplier {
	return map[string]mesh.EventApplier{
		meshevents.EventKindMarketStarted: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyMarketStartedEvent(applyCtx, markets, bookRouter, lifecycleUpdated, event)
		},
	}
}

type validatedMarketStartedEvent struct {
	mkt    *Market
	book   *msgBook
	update *db.MarketStartedUpdate
}

// applyMarketStartedEvent records startup revocations and lifecycle changes,
// updates the market and book router, and notifies affected order owners.
func applyMarketStartedEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter, lifecycleUpdated LifecycleUpdated, event *mesh.Event) (*db.EventLogEntry, error) {
	validated, err := validateMarketStartedEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}

	mkt, update := validated.mkt, validated.update
	result, err := mkt.storage.ApplyMarketStartedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), update)
	if err != nil {
		return nil, err
	}

	removedOrders := mkt.applyMarketStartedMemory(update)
	mkt.applyMarketLifecycleRow(result.Lifecycle)
	bookRouter.applyMarketStartedEvent(validated.book, update.CurrentEpochIdx, removedOrders)

	for _, revoke := range update.BookedRevokes {
		ord := revoke.Order
		mkt.sendRevokeOrderNote(ord.ID(), ord.User())
	}
	for _, revoke := range update.EpochRevokes {
		ord := revoke.Order
		if ord.Type() == order.CancelOrderType {
			// Abandoned cancel orders receive nomatch notifications, just like
			// cancels that did not match during epoch processing.
			mkt.sendNoMatchNote(ord.ID(), ord.User())
			continue
		}
		mkt.sendRevokeOrderNote(ord.ID(), ord.User())
	}

	if lifecycleUpdated != nil && result.Lifecycle != nil {
		lifecycleUpdated(LifecycleTransitionStarted, result.Lifecycle)
	}
	return result.Log, nil
}

// validateMarketStartedEvent decodes the event, checks its revocations and
// book lot-size compatibility, and builds the database update.
func validateMarketStartedEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedMarketStartedEvent, error) {
	started, err := meshevents.DecodeMarketStartedEvent(event.Payload)
	if err != nil {
		return nil, err
	}

	mkt, book, err := marketAndBook(markets, bookRouter, started.Market)
	if err != nil {
		return nil, err
	}

	bookedRevokes, err := decodeStartupOrderRevokeRecords(started.BookedRevokes)
	if err != nil {
		return nil, err
	}
	seen := make(map[order.OrderID]bool, len(bookedRevokes))
	if err := mkt.validateStartupOrderRevokes(bookedRevokes, seen, true, "booked"); err != nil {
		return nil, err
	}
	// Every booked order incompatible with the new lot size must be revoked.
	// Check before adding epoch revocations to seen so only booked revocations
	// are excluded from the lot-size check.
	if err := mkt.book.CheckLotSize(started.RunParams.LotSize, seen); err != nil {
		return nil, fmt.Errorf("market_started for %s: %w", started.Market, err)
	}

	epochRevokes, err := decodeStartupOrderRevokeRecords(started.EpochRevokes)
	if err != nil {
		return nil, err
	}
	if err := mkt.validateStartupOrderRevokes(epochRevokes, seen, false, "epoch"); err != nil {
		return nil, err
	}

	return &validatedMarketStartedEvent{
		mkt:  mkt,
		book: book,
		update: &db.MarketStartedUpdate{
			Market:          started.Market,
			Base:            mkt.base,
			Quote:           mkt.quote,
			CurrentEpochIdx: started.CurrentEpochIdx,
			EpochDur:        started.EpochDur,
			RunParams:       started.RunParams,
			RevocationTime:  time.UnixMilli(started.RevocationTime).UTC(),
			BookedRevokes:   bookedRevokes,
			EpochRevokes:    epochRevokes,
		},
	}, nil
}

// decodeStartupOrderRevokeRecords decodes revoked orders and validates
// their revocation reasons.
func decodeStartupOrderRevokeRecords(records []meshevents.StartupOrderRevokeRecord) ([]*db.StartupOrderRevoke, error) {
	revokes := make([]*db.StartupOrderRevoke, 0, len(records))
	for _, record := range records {
		if !meshevents.ValidStartupOrderRevokeReason(record.Reason) {
			return nil, fmt.Errorf("invalid order revoke reason %d", record.Reason)
		}
		if len(record.EncodedOrder) == 0 {
			return nil, fmt.Errorf("startup revocation has no encoded order")
		}
		ord, err := order.DecodeOrder(record.EncodedOrder)
		if err != nil {
			return nil, fmt.Errorf("decode startup revoked order: %w", err)
		}
		revokes = append(revokes, &db.StartupOrderRevoke{
			Order:  ord,
			Reason: record.Reason,
		})
	}
	return revokes, nil
}

// validateStartupOrderRevokes checks for duplicate IDs, wrong markets, and,
// when requireLimit is true, non-limit orders. It adds IDs to seen so duplicates
// can be detected across revocation lists.
func (m *Market) validateStartupOrderRevokes(revokes []*db.StartupOrderRevoke, seen map[order.OrderID]bool,
	requireLimit bool, label string) error {

	for _, revoke := range revokes {
		ord := revoke.Order
		oid := ord.ID()
		if seen[oid] {
			return fmt.Errorf("duplicate startup order revoke %v", oid)
		}
		seen[oid] = true

		if ord.Base() != m.base || ord.Quote() != m.quote {
			return fmt.Errorf("startup %s revoke %v market mismatch", label, oid)
		}

		if requireLimit && ord.Type() != order.LimitOrderType {
			return fmt.Errorf("startup %s revoke %v type %v, want limit", label, oid, ord.Type())
		}
	}
	return nil
}

// dbEventLogMeta builds event-log metadata from the event payload and
// optional log position.
func dbEventLogMeta(position *db.EventLogPosition, event *mesh.Event) *db.EventLogMeta {
	if event == nil {
		return nil
	}
	logMeta := &db.EventLogMeta{
		Event: append([]byte(nil), event.Payload...),
	}
	if position != nil {
		logMeta.Seq = position.Seq
		logMeta.ExpectedTipHash = append([]byte(nil), position.TipHash...)
	}
	return logMeta
}

// marketAndBook finds the market and its corresponding book in the book router.
func marketAndBook(markets map[string]*Market, bookRouter *BookRouter, marketName string) (*Market, *msgBook, error) {
	mkt := markets[marketName]
	if mkt == nil {
		return nil, nil, fmt.Errorf("unknown event market %q", marketName)
	}
	book := bookRouter.books[marketName]
	if book == nil {
		return nil, nil, fmt.Errorf("unknown event book market %q", marketName)
	}
	return mkt, book, nil
}
