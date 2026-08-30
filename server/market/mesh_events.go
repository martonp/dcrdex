// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

// MeshService is the part of mesh.Service this package calls:
// ExecuteCommand for client requests, ApplyEvent for replicated state.
type MeshService interface {
	ExecuteCommand(context.Context, mesh.CommandRequest) *msgjson.Error
	ApplyEvent(context.Context, *mesh.Event) (any, error)
}

type validatedOrderAcceptedEvent struct {
	meshEvent      *mesh.Event
	ord            order.Order
	mkt            *Market
	book           *msgBook
	epochIdx       int64
	epochDur       int64
	epochGap       int32
	alreadyApplied bool
}

type validatedMarketStartedEvent struct {
	mkt    *Market
	book   *msgBook
	update *db.MarketStartedUpdate
}

type validatedMarketLifecycleEvent struct {
	mkt    *Market
	book   *msgBook
	update *db.MarketLifecycleUpdate
}

type validatedSuspendedCancelEvent struct {
	mkt             *Market
	book            *msgBook
	update          *db.SuspendedCancelUpdate
	matchServerTime time.Time
}

type validatedAdvanceEpochEvent struct {
	event        *meshevents.AdvanceEpochEvent
	mkt          *Market
	book         *msgBook
	raw          *mesh.Event
	closedOrders []order.Order
}

func lifecycleActionToDB(action string) db.MarketLifecycleAction {
	switch action {
	case meshevents.LifecycleActionScheduleSuspend:
		return db.MarketLifecycleActionScheduleSuspend
	case meshevents.LifecycleActionSuspend:
		return db.MarketLifecycleActionSuspend
	case meshevents.LifecycleActionScheduleResume:
		return db.MarketLifecycleActionScheduleResume
	case meshevents.LifecycleActionResume:
		return db.MarketLifecycleActionResume
	default:
		return db.MarketLifecycleActionInvalid
	}
}

func decodeOrderIDBytes(raw dex.Bytes) (order.OrderID, error) {
	var oid order.OrderID
	if len(raw) != order.OrderIDSize {
		return oid, fmt.Errorf("invalid order ID length %d", len(raw))
	}
	copy(oid[:], raw)
	return oid, nil
}

// decodeStartupOrderRevokeRecords decodes wire revoke records into the shared
// db.StartupOrderRevoke form used by startup submission and event application.
func decodeStartupOrderRevokeRecords(records []meshevents.StartupOrderRevokeRecord) ([]*db.StartupOrderRevoke, error) {
	revokes := make([]*db.StartupOrderRevoke, 0, len(records))
	for _, record := range records {
		if !meshevents.ValidStartupOrderRevokeReason(record.Reason) {
			return nil, fmt.Errorf("invalid order revoke reason %d", record.Reason)
		}
		if len(record.EncodedOrder) == 0 {
			return nil, fmt.Errorf("empty order revoke")
		}
		ord, err := order.DecodeOrder(record.EncodedOrder)
		if err != nil {
			return nil, err
		}
		revokes = append(revokes, &db.StartupOrderRevoke{
			Order:  ord,
			Reason: record.Reason,
		})
	}
	return revokes, nil
}

// validateStartupOrderRevokes checks a decoded set of startup order revokes
// for duplicates and market mismatch, optionally requiring the limit order
// type. The seen map is shared between the booked and epoch sets so an order
// cannot appear in both.
func (m *Market) validateStartupOrderRevokes(revokes []*db.StartupOrderRevoke, seen map[order.OrderID]bool,
	requireLimit bool, label string) ([]*db.StartupOrderRevoke, error) {

	valid := make([]*db.StartupOrderRevoke, 0, len(revokes))
	for _, revoke := range revokes {
		ord := revoke.Order
		oid := ord.ID()
		if seen[oid] {
			return nil, fmt.Errorf("duplicate startup order revoke %v", oid)
		}
		seen[oid] = true
		if ord.Base() != m.base || ord.Quote() != m.quote {
			return nil, fmt.Errorf("startup %s revoke %v market mismatch", label, oid)
		}
		if requireLimit && ord.Type() != order.LimitOrderType {
			return nil, fmt.Errorf("startup %s revoke %v type %v, want limit", label, oid, ord.Type())
		}
		valid = append(valid, revoke)
	}
	return valid, nil
}

type revokeTarget struct {
	mkt  *Market
	book *msgBook
	lo   *order.LimitOrder
}

type validatedOrdersRevokedEvent struct {
	raw        *mesh.Event
	reason     meshevents.OrderRevokeReason
	revokeTime time.Time
	targets    []revokeTarget
}

func applyOrdersRevokedEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter,
	event *mesh.Event) (*db.EventLogEntry, error) {

	validated, err := validateOrdersRevokedEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	update := &db.OrdersRevokedUpdate{
		Reason:     validated.reason,
		RevokeTime: validated.revokeTime,
		Orders:     make([]*order.LimitOrder, 0, len(validated.targets)),
	}
	for _, target := range validated.targets {
		update.Orders = append(update.Orders, target.lo)
	}
	mkt := validated.targets[0].mkt
	logEntry, err := mkt.storage.ApplyOrdersRevokedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event),
		mkt.auth.ReputationOutcomePolicy(), update)
	if err != nil {
		return nil, err
	}
	applyOrdersRevokedMemory(validated)
	return logEntry, nil
}

// validateOrdersRevokedEvent derives the revocation targets from the current
// book state. The derivation must be a pure function of replicated state so
// every node revokes an identical order set: unknown or unbooked orders are
// skipped, funding-spent revokes skip orders that have since partially filled,
// and the final target list is sorted by order ID.
func validateOrdersRevokedEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedOrdersRevokedEvent, error) {
	revoked, err := meshevents.DecodeOrdersRevokedEvent(event.Payload)
	if err != nil {
		return nil, err
	}

	var targets []revokeTarget
	if len(revoked.User) > 0 {
		var user account.AccountID
		if len(revoked.User) != len(user) {
			return nil, fmt.Errorf("invalid account ID length %d", len(revoked.User))
		}
		copy(user[:], revoked.User)
		mktNames := make([]string, 0, len(markets))
		for name := range markets {
			mktNames = append(mktNames, name)
		}
		sort.Strings(mktNames)
		for _, name := range mktNames {
			mkt, book, err := marketAndBook(markets, bookRouter, name)
			if err != nil {
				return nil, err
			}
			buys, sells := mkt.book.UserOrders(user)
			for _, lo := range append(buys, sells...) {
				targets = append(targets, revokeTarget{mkt: mkt, book: book, lo: lo})
			}
		}
	} else {
		mkt, book, err := marketAndBook(markets, bookRouter, revoked.Market)
		if err != nil {
			return nil, err
		}
		for _, oidB := range revoked.OrderIDs {
			oid, err := decodeOrderIDBytes(oidB)
			if err != nil {
				return nil, err
			}
			lo := mkt.book.Order(oid)
			if lo == nil {
				log.Debugf("Skipping orders_revoked target %v: no longer booked on market %s",
					oid, revoked.Market)
				continue
			}
			if revoked.Reason == meshevents.OrderRevokeReasonFundingSpent && lo.Filled() != 0 {
				log.Debugf("Skipping funding-spent orders_revoked target %v: order has filled amount", oid)
				continue
			}
			targets = append(targets, revokeTarget{mkt: mkt, book: book, lo: lo})
		}
	}

	if len(targets) == 0 {
		// This can only reject the event at master emission time, before it is
		// committed and streamed: a committed event derives an identical,
		// non-empty target set on every node.
		return nil, fmt.Errorf("orders_revoked event has no booked targets")
	}
	sort.Slice(targets, func(i, j int) bool {
		iID, jID := targets[i].lo.ID(), targets[j].lo.ID()
		return bytes.Compare(iID[:], jID[:]) < 0
	})

	return &validatedOrdersRevokedEvent{
		raw:        event,
		reason:     revoked.Reason,
		revokeTime: time.UnixMilli(revoked.RevokeTime).UTC(),
		targets:    targets,
	}, nil
}

func applyOrdersRevokedMemory(validated *validatedOrdersRevokedEvent) {
	for _, target := range validated.targets {
		target.mkt.applyOrderRevokedMemory(target.lo)
	}
	if validated.reason != meshevents.OrderRevokeReasonPenalty {
		return
	}
	notified := make(map[account.AccountID]bool, 1)
	for _, target := range validated.targets {
		user := target.lo.User()
		if notified[user] {
			continue
		}
		notified[user] = true
		target.mkt.sendPenaltyNote(user, validated.revokeTime)
	}
}

// LifecycleTransition is which lifecycle transition just applied.
type LifecycleTransition uint8

const (
	LifecycleTransitionStarted LifecycleTransition = iota + 1
	LifecycleTransitionScheduleSuspend
	LifecycleTransitionSuspend
	LifecycleTransitionScheduleResume
	LifecycleTransitionResume
)

// lifecycleTransition maps an applied market_lifecycle action to its observer
// transition.
func lifecycleTransition(action db.MarketLifecycleAction) LifecycleTransition {
	switch action {
	case db.MarketLifecycleActionScheduleSuspend:
		return LifecycleTransitionScheduleSuspend
	case db.MarketLifecycleActionSuspend:
		return LifecycleTransitionSuspend
	case db.MarketLifecycleActionScheduleResume:
		return LifecycleTransitionScheduleResume
	case db.MarketLifecycleActionResume:
		return LifecycleTransitionResume
	default:
		return 0
	}
}

// LifecycleUpdated is invoked from market_started and market_lifecycle apply.
type LifecycleUpdated func(transition LifecycleTransition, lc *db.MarketLifecycle)

// Events returns the mesh event appliers. Each applies an already-decided
// event to this node's state and event log.
func Events(markets map[string]*Market, bookRouter *BookRouter, sendIfLocal func(account.AccountID, *msgjson.Message) error, lifecycleUpdated LifecycleUpdated) map[string]mesh.EventApplier {
	return map[string]mesh.EventApplier{
		meshevents.EventKindOrderAccepted: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyOrderAcceptedEvent(applyCtx, markets, bookRouter, event)
		},
		meshevents.EventKindMarketStarted: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyMarketStartedEvent(applyCtx, markets, bookRouter, lifecycleUpdated, event)
		},
		meshevents.EventKindMarketLifecycle: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyMarketLifecycleEvent(applyCtx, markets, bookRouter, lifecycleUpdated, event)
		},
		meshevents.EventKindSuspendedCancel: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applySuspendedCancelEvent(applyCtx, markets, bookRouter, event)
		},
		meshevents.EventKindAdvanceEpoch: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyAdvanceEpochEvent(applyCtx, markets, bookRouter, event)
		},
		meshevents.EventKindEpochProcessed: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyEpochProcessedEvent(applyCtx, markets, bookRouter, sendIfLocal, event)
		},
		meshevents.EventKindOrdersRevoked: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			return applyOrdersRevokedEvent(applyCtx, markets, bookRouter, event)
		},
	}
}

func dbEventLogMeta(meta *db.EventLogPosition, event *mesh.Event) *db.EventLogMeta {
	if event == nil {
		return nil
	}
	logMeta := &db.EventLogMeta{
		Event: append([]byte(nil), event.Payload...),
	}
	if meta != nil {
		logMeta.Seq = meta.Seq
		logMeta.ExpectedTipHash = append([]byte(nil), meta.TipHash...)
	}
	return logMeta
}

func applyOrderAcceptedEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*db.EventLogEntry, error) {
	validated, err := validateOrderAcceptedEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	logEntry, err := applyOrderAcceptedDB(applyCtx, validated)
	if err != nil {
		return nil, err
	}
	applyOrderAcceptedMemory(bookRouter, validated)
	return logEntry, nil
}

func validateOrderAcceptedEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedOrderAcceptedEvent, error) {
	accepted, err := meshevents.DecodeOrderAcceptedEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	ord, err := accepted.Order()
	if err != nil {
		return nil, err
	}
	mktName, err := dex.MarketName(ord.Base(), ord.Quote())
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, mktName)
	if err != nil {
		return nil, err
	}
	validated, err := mkt.validateOrderAcceptedEvent(ord, book)
	if err != nil {
		return nil, err
	}
	validated.meshEvent = event
	return validated, nil
}

func applyOrderAcceptedDB(applyCtx *mesh.EventApplyContext, accepted *validatedOrderAcceptedEvent) (*db.EventLogEntry, error) {
	logEntry, err := accepted.mkt.storage.ApplyOrderAcceptedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, accepted.meshEvent), &db.OrderAcceptedUpdate{
		Order:    accepted.ord,
		EpochIdx: accepted.epochIdx,
		EpochDur: accepted.epochDur,
		EpochGap: accepted.epochGap,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: failed to store accepted order %v: %w", errEpochOrderStorage, accepted.ord.ID(), err)
	}
	return logEntry, nil
}

func applyOrderAcceptedMemory(bookRouter *BookRouter, accepted *validatedOrderAcceptedEvent) {
	if accepted.alreadyApplied {
		return
	}
	accepted.mkt.applyOrderAcceptedMemory(accepted)
	mktName := accepted.mkt.name
	epochNote := epochOrderNote(accepted.ord, mktName, accepted.epochIdx)
	bookRouter.applyOrderAcceptedEvent(accepted.book, epochNote, accepted.epochIdx)
}

func applyMarketStartedEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter, lifecycleUpdated LifecycleUpdated, event *mesh.Event) (*db.EventLogEntry, error) {
	validated, err := validateMarketStartedEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	logEntry, err := validated.mkt.storage.ApplyMarketStartedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), validated.update)
	if err != nil {
		return nil, err
	}
	lifecycleRow, err := validated.mkt.storage.MarketLifecycle(validated.update.Market)
	if err != nil {
		return nil, &mesh.CommittedEventApplyError{Applied: logEntry, Err: err}
	}
	removed := validated.mkt.applyMarketStartedMemory(validated)
	if lifecycleRow != nil {
		if err := validated.mkt.applyMarketLifecycleRow(lifecycleRow); err != nil {
			return nil, &mesh.CommittedEventApplyError{Applied: logEntry, Err: err}
		}
	}
	bookRouter.applyMarketStartedEvent(validated.book, validated.update.CurrentEpochIdx, removed)
	for _, revoke := range validated.update.BookedRevokes {
		ord := revoke.Order
		validated.mkt.sendRevokeOrderNote(ord.ID(), ord.User())
	}
	for _, revoke := range validated.update.EpochRevokes {
		ord := revoke.Order
		if ord.Type() == order.CancelOrderType {
			// An epoch cancel fails exactly like an unmatched cancel, so its
			// owner gets the same nomatch note the normal pipeline sends.
			validated.mkt.sendNoMatchNote(ord.ID(), ord.User())
			continue
		}
		validated.mkt.sendRevokeOrderNote(ord.ID(), ord.User())
	}
	if lifecycleUpdated != nil && lifecycleRow != nil {
		lifecycleUpdated(LifecycleTransitionStarted, lifecycleRow)
	}
	return logEntry, nil
}

func validateMarketStartedEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedMarketStartedEvent, error) {
	started, err := meshevents.DecodeMarketStartedEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, started.Market)
	if err != nil {
		return nil, err
	}
	booked, err := decodeStartupOrderRevokeRecords(started.BookedRevokes)
	if err != nil {
		return nil, err
	}
	seen := make(map[order.OrderID]bool, len(booked))
	bookedUpdates, err := mkt.validateStartupOrderRevokes(booked, seen, true, "booked")
	if err != nil {
		return nil, err
	}
	// Coverage must come from the booked revokes alone: an epoch revoke
	// leaves nothing on the book, so an incompatible booked order listed
	// only there would survive unmatched.
	bookedSeen := make(map[order.OrderID]bool, len(bookedUpdates))
	for _, revoke := range bookedUpdates {
		bookedSeen[revoke.Order.ID()] = true
	}
	// Reject an under-covered booked set before persist. SetLotSize re-checks
	// after the revokes have been removed.
	if err := mkt.book.CheckLotSize(started.RunParams.LotSize, bookedSeen); err != nil {
		return nil, fmt.Errorf("market_started for %s: %w", started.Market, err)
	}

	epochRevokes, err := decodeStartupOrderRevokeRecords(started.EpochRevokes)
	if err != nil {
		return nil, err
	}
	epochUpdates, err := mkt.validateStartupOrderRevokes(epochRevokes, seen, false, "epoch")
	if err != nil {
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
			BookedRevokes:   bookedUpdates,
			EpochRevokes:    epochUpdates,
		},
	}, nil
}

func applyMarketLifecycleEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter,
	lifecycleUpdated LifecycleUpdated, event *mesh.Event) (*db.EventLogEntry, error) {

	validated, err := validateMarketLifecycleEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	result, err := validated.mkt.storage.ApplyMarketLifecycleEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), validated.update)
	if err != nil {
		return nil, err
	}
	notify := func() {
		if lifecycleUpdated != nil {
			lifecycleUpdated(lifecycleTransition(validated.update.Action), result.Lifecycle)
		}
	}
	applyRow := func() error {
		if err := validated.mkt.applyMarketLifecycleRow(result.Lifecycle); err != nil {
			return &mesh.CommittedEventApplyError{Applied: result.Log, Err: err}
		}
		return nil
	}
	switch validated.update.Action {
	case db.MarketLifecycleActionScheduleSuspend, db.MarketLifecycleActionScheduleResume:
		if err := applyRow(); err != nil {
			return nil, err
		}
		notify()
	case db.MarketLifecycleActionSuspend:
		if err := applyRow(); err != nil {
			return nil, err
		}
		persist := result.Lifecycle.PersistBook != nil && *result.Lifecycle.PersistBook
		if !persist {
			validated.mkt.applyMarketSuspendPurge(result.PurgeOrders)
		}
		bookRouter.applyMarketSuspendedEvent(validated.book, result.Lifecycle.FinalEpochIdx, persist, result.PurgeOrders)
		notify()
	case db.MarketLifecycleActionResume:
		removed := validated.mkt.applyMarketResumeCleanup(result.ResumeRevokes)
		if err := applyRow(); err != nil {
			return nil, err
		}
		notify()
		bookRouter.applyMarketResumedEvent(validated.book, result.Lifecycle.StartEpochIdx, removed)
	}

	return result.Log, nil
}

func validateMarketLifecycleEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedMarketLifecycleEvent, error) {
	lifecycleEvent, err := meshevents.DecodeMarketLifecycleEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, lifecycleEvent.Market)
	if err != nil {
		return nil, err
	}
	if lifecycleEvent.Action == meshevents.LifecycleActionScheduleSuspend {
		if err := mkt.validateScheduleSuspendEvent(lifecycleEvent.EpochIdx, lifecycleEvent.EpochDur); err != nil {
			return nil, err
		}
	}
	resumeRevokes, err := decodeStartupOrderRevokeRecords(lifecycleEvent.ResumeRevokes)
	if err != nil {
		return nil, err
	}
	if lifecycleEvent.Action == meshevents.LifecycleActionResume {
		// The revoke set must cover every order the new lot size strands.
		revoked := make(map[order.OrderID]bool, len(resumeRevokes))
		for _, revoke := range resumeRevokes {
			revoked[revoke.Order.ID()] = true
		}
		if err := mkt.book.CheckLotSize(lifecycleEvent.RunParams.LotSize, revoked); err != nil {
			return nil, fmt.Errorf("resume for %s: %w", lifecycleEvent.Market, err)
		}
	}
	update := &db.MarketLifecycleUpdate{
		Action:        lifecycleActionToDB(lifecycleEvent.Action),
		Market:        lifecycleEvent.Market,
		Base:          mkt.base,
		Quote:         mkt.quote,
		EpochIdx:      lifecycleEvent.EpochIdx,
		EpochDur:      lifecycleEvent.EpochDur,
		PersistBook:   cloneBool(lifecycleEvent.PersistBook),
		Timestamp:     time.UnixMilli(lifecycleEvent.Timestamp).UTC(),
		ResumeRevokes: resumeRevokes,
		RunParams:     cloneRunParams(lifecycleEvent.RunParams),
	}
	return &validatedMarketLifecycleEvent{
		mkt:    mkt,
		book:   book,
		update: update,
	}, nil
}

func applySuspendedCancelEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter,
	event *mesh.Event) (*db.EventLogEntry, error) {

	validated, err := validateSuspendedCancelEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	result, err := validated.mkt.storage.ApplySuspendedCancelEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), validated.update)
	if err != nil {
		return nil, err
	}
	lo := result.TargetOrder
	validated.mkt.bookMtx.Lock()
	delete(validated.mkt.settling, lo.ID())
	validated.mkt.book.Remove(lo.ID())
	validated.mkt.bookMtx.Unlock()
	validated.mkt.unlockOrderCoins(lo)

	sendTail := func(context.Context) {
		bookRouter.applyUnbookOrderEvent(validated.book, lo)
		validated.mkt.sendSuspendedCancelMatchRequest(result.Cancel.User(), result.Match, validated.matchServerTime)
	}
	if !applyCtx.AfterCommandResult(sendTail) {
		bookRouter.applyUnbookOrderEvent(validated.book, lo)
	}
	return result.Log, nil
}

func validateSuspendedCancelEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedSuspendedCancelEvent, error) {
	cancelEvent, err := meshevents.DecodeSuspendedCancelEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	cancel, err := cancelEvent.CancelOrder()
	if err != nil {
		return nil, err
	}
	target, err := cancelEvent.TargetOrder()
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, cancelEvent.Market)
	if err != nil {
		return nil, err
	}
	if cancel.Base() != mkt.base || cancel.Quote() != mkt.quote {
		return nil, fmt.Errorf("suspended_cancel market mismatch")
	}
	if target.Base() != mkt.base || target.Quote() != mkt.quote {
		return nil, fmt.Errorf("suspended_cancel target market mismatch")
	}
	match := &order.Match{
		Taker:        cancel,
		Maker:        target,
		Quantity:     target.Remaining(),
		Rate:         target.Rate,
		Epoch:        order.EpochID{Idx: uint64(cancelEvent.EpochIdx), Dur: uint64(cancelEvent.EpochDur)},
		FeeRateBase:  cancelEvent.FeeRateBase,
		FeeRateQuote: cancelEvent.FeeRateQuote,
		Status:       order.MatchComplete,
	}
	matchServerTime := cancelEvent.MatchServerTimeTime()
	return &validatedSuspendedCancelEvent{
		mkt:  mkt,
		book: book,
		update: &db.SuspendedCancelUpdate{
			Market:          cancelEvent.Market,
			Base:            cancelEvent.Base,
			Quote:           cancelEvent.Quote,
			Cancel:          cancel,
			TargetOrderID:   target.ID(),
			TargetAccount:   target.AccountID,
			TargetSell:      target.Sell,
			EpochIdx:        cancelEvent.EpochIdx,
			EpochDur:        cancelEvent.EpochDur,
			FeeRateBase:     cancelEvent.FeeRateBase,
			FeeRateQuote:    cancelEvent.FeeRateQuote,
			MatchServerTime: matchServerTime,
			Match:           match,
		},
		matchServerTime: matchServerTime,
	}, nil
}

func applyAdvanceEpochEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*db.EventLogEntry, error) {
	validated, err := validateAdvanceEpochEvent(markets, bookRouter, event)
	if err != nil {
		return nil, err
	}
	logEntry, err := applyAdvanceEpochDBTx(applyCtx, validated)
	if err != nil {
		return nil, err
	}
	if err := validated.mkt.applyAdvanceEpochEvent(validated.event, validated.closedOrders); err != nil {
		return nil, &mesh.CommittedEventApplyError{Applied: logEntry, Err: err}
	}
	if validated.event.OpenedEpochIdx > 0 {
		bookRouter.applyAdvanceEpochEvent(validated.book, validated.event.OpenedEpochIdx)
	}
	return logEntry, nil
}

func validateAdvanceEpochEvent(markets map[string]*Market, bookRouter *BookRouter, event *mesh.Event) (*validatedAdvanceEpochEvent, error) {
	advanceEpoch, err := meshevents.DecodeAdvanceEpochEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, advanceEpoch.Market)
	if err != nil {
		return nil, err
	}
	closedOrders, err := mkt.validateAdvanceEpochState(advanceEpoch)
	if err != nil {
		return nil, err
	}
	return &validatedAdvanceEpochEvent{
		event:        advanceEpoch,
		mkt:          mkt,
		book:         book,
		raw:          event,
		closedOrders: closedOrders,
	}, nil
}

func applyAdvanceEpochDBTx(applyCtx *mesh.EventApplyContext, validated *validatedAdvanceEpochEvent) (*db.EventLogEntry, error) {
	return validated.mkt.storage.ApplyAdvanceEpochEvent(applyCtx, dbEventLogMeta(applyCtx.Position, validated.raw), &db.AdvanceEpochUpdate{
		Market:         validated.event.Market,
		ClosedEpochIdx: validated.event.ClosedEpochIdx,
		OpenedEpochIdx: validated.event.OpenedEpochIdx,
		EpochDur:       validated.event.EpochDur,
		ClosedOrderIDs: orderIDs(validated.closedOrders),
	})
}

func applyEpochProcessedEvent(applyCtx *mesh.EventApplyContext, markets map[string]*Market, bookRouter *BookRouter,
	sendIfLocal func(account.AccountID, *msgjson.Message) error, event *mesh.Event) (*db.EventLogEntry, error) {

	processed, err := meshevents.DecodeEpochProcessedEvent(event.Payload)
	if err != nil {
		return nil, err
	}
	mkt, book, err := marketAndBook(markets, bookRouter, processed.Market)
	if err != nil {
		return nil, err
	}
	result, err := mkt.applyEpochProcessedEvent(applyCtx, processed, event)
	if err != nil {
		return nil, err
	}
	bookRouter.applyEpochProcessedEvent(book, processed, result)
	for _, ord := range result.nomatched {
		oid := ord.Order.ID()
		msg, err := noMatchMessage(oid)
		if err != nil {
			log.Errorf("Failed to encode 'nomatch' notification.")
			continue
		}
		if err := sendIfLocal(ord.Order.User(), msg); err != nil {
			log.Infof("Failed to send nomatch to user %s: %v", ord.Order.User(), err)
		}
	}
	bookRouter.applyEpochReportEvent(book, &epochReport{
		epochIdx:     processed.EpochIdx,
		epochDur:     processed.EpochDur,
		spot:         result.spot,
		stats:        result.stats,
		baseFeeRate:  processed.FeeRateBase,
		quoteFeeRate: processed.FeeRateQuote,
		matches:      result.matchReport,
	})
	mkt.sendMMSnapshots(processed.EpochIdx, processed.EpochDur)
	applyCtx.SetResult(result)
	return result.dbLog, nil
}

func marketAndBook(markets map[string]*Market, bookRouter *BookRouter, mktName string) (*Market, *msgBook, error) {
	mkt := markets[mktName]
	if mkt == nil {
		return nil, nil, fmt.Errorf("unknown event market %q", mktName)
	}
	book := bookRouter.books[mktName]
	if book == nil {
		return nil, nil, fmt.Errorf("unknown event book market %q", mktName)
	}
	return mkt, book, nil
}
