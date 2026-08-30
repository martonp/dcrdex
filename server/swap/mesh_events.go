// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/asset"
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

// SetMeshService sets the mesh service used by the swapper.
func (s *Swapper) SetMeshService(mesh MeshService) {
	s.mesh = mesh
}

// Events returns the mesh event appliers. Each applies an already-decided
// event to this node's state and event log.
func (s *Swapper) Events() map[string]mesh.EventApplier {
	return map[string]mesh.EventApplier{
		meshevents.EventKindMatchAcksRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			acks, err := meshevents.DecodeMatchAcksRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyMatchAcksRecordedEvent(applyCtx, event, applyCtx.Position, acks)
		},
		meshevents.EventKindMatchFailed: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			failed, err := meshevents.DecodeMatchFailedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyMatchFailedEvent(applyCtx, event, applyCtx.Position, failed)
		},
		meshevents.EventKindSwapContractRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeSwapContractRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applySwapContractRecordedEvent(applyCtx, event, applyCtx.Position, recorded)
		},
		meshevents.EventKindSwapRedemptionRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeSwapRedemptionRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applySwapRedemptionRecordedEvent(applyCtx, event, applyCtx.Position, recorded)
		},
		meshevents.EventKindAuditAckRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeAuditAckRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyAuditAckRecordedEvent(applyCtx, event, applyCtx.Position, recorded)
		},
		// TODO(mesh): consider if we actually need the redemption acknowledgement events.
		meshevents.EventKindRedemptionAckRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeRedemptionAckRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyRedemptionAckRecordedEvent(applyCtx, event, applyCtx.Position, recorded)
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

func newSwapContractRecordedEvent(step *stepInformation, params *msgjson.Init, contract *asset.Contract, swapTime time.Time) (*mesh.Event, error) {
	coin := coinProjection(contract)
	event := &meshevents.SwapContractRecordedEvent{
		MatchID:     step.match.ID(),
		Base:        step.match.Maker.BaseAsset,
		Quote:       step.match.Maker.QuoteAsset,
		Maker:       step.actor.isMaker,
		Status:      step.nextStep,
		CoinID:      append(dex.Bytes(nil), params.CoinID...),
		CoinTxID:    coin.txID,
		CoinString:  coin.coinString,
		Value:       coin.value,
		FeeRate:     coin.feeRate,
		Contract:    append(dex.Bytes(nil), params.Contract...),
		SwapAddress: contract.SwapAddress,
		SecretHash:  append(dex.Bytes(nil), contract.SecretHash...),
		LockTime:    contract.LockTime.UnixMilli(),
		TxData:      append(dex.Bytes(nil), contract.TxData...),
		SwapTime:    swapTime.UnixMilli(),
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

type eventCoin struct {
	id            dex.Bytes
	txID          string
	coinString    string
	value         uint64
	feeRate       uint64
	confirmations func(context.Context) (int64, error)
}

func coinProjection(coin asset.Coin) *eventCoin {
	return &eventCoin{
		id:         append(dex.Bytes(nil), coin.ID()...),
		txID:       coin.TxID(),
		coinString: coin.String(),
		value:      coin.Value(),
		feeRate:    coin.FeeRate(),
	}
}

func (c *eventCoin) Confirmations(ctx context.Context) (int64, error) {
	if c.confirmations != nil {
		return c.confirmations(ctx)
	}
	return 0, nil
}

func (c *eventCoin) ID() []byte {
	return c.id
}

func (c *eventCoin) TxID() string {
	if c.txID != "" {
		return c.txID
	}
	return fmt.Sprintf("%x", []byte(c.id))
}

func (c *eventCoin) String() string {
	if c.coinString != "" {
		return c.coinString
	}
	return c.TxID()
}

func (c *eventCoin) Value() uint64 {
	return c.value
}

func (c *eventCoin) FeeRate() uint64 {
	return c.feeRate
}

type validatedSwapContractRecordedEvent struct {
	event    *meshevents.SwapContractRecordedEvent
	match    *matchTracker
	status   *swapStatus
	contract *asset.Contract
	record   *db.SwapContract
}

func (s *Swapper) applySwapContractRecordedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.SwapContractRecordedEvent) (*db.EventLogEntry, error) {
	validated, err := s.validateSwapContractRecordedEvent(ctx, event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplySwapContractRecordedEvent(ctx, dbEventLogMeta(meta, meshEvent), validated.record)
	if err != nil {
		return nil, fmt.Errorf("saving swap contract for match %v: %w", event.MatchID, err)
	}
	s.applySwapContractRecordedMemory(validated)
	return logEntry, nil
}

func (s *Swapper) validateSwapContractRecordedEvent(ctx context.Context, event *meshevents.SwapContractRecordedEvent) (*validatedSwapContractRecordedEvent, error) {
	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		// Deliberately NO lazy reconstruction from the DB here (nor in the
		// other match-referencing appliers). Every active match gets its
		// tracker at startup — restoreActiveSwaps covers all active matches,
		// with or without swap data, and the mesh snapshot carries the orders
		// they reference — so a missing tracker means either a duplicate/late
		// event for a completed match (handled where benign, e.g. maker
		// redemption acks) or genuinely inconsistent state. Reconstructing
		// inside an applier would trade a loud, retryable failure for
		// nondeterminism: the rebuild depends on projection rows and asset
		// backends whose state can differ between nodes mid-replay, and event
		// application must be deterministic across the mesh. Failing here
		// keeps the event durable on the sender; the wedge is escalated by
		// the mesh layer and a restart after a fix resumes cleanly.
		return nil, fmt.Errorf("swap contract recorded event for unknown match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, fmt.Errorf("swap contract recorded market mismatch for match %v", event.MatchID)
	}

	status := match.takerStatus
	if event.Maker {
		status = match.makerStatus
	}
	swapAsset := s.coins[status.swapAsset]
	if swapAsset == nil {
		return nil, fmt.Errorf("no swap asset %d for match %v", status.swapAsset, event.MatchID)
	}

	coin := &eventCoin{
		id:         append(dex.Bytes(nil), event.CoinID...),
		txID:       event.CoinTxID,
		coinString: event.CoinString,
		value:      event.Value,
		feeRate:    event.FeeRate,
		confirmations: func(ctx context.Context) (int64, error) {
			contract, err := swapAsset.Backend.Contract(event.CoinID, event.Contract)
			if err != nil {
				return 0, err
			}
			return contract.Confirmations(ctx)
		},
	}
	contract := &asset.Contract{
		Coin:         coin,
		SwapAddress:  event.SwapAddress,
		ContractData: append([]byte(nil), event.Contract...),
		SecretHash:   append([]byte(nil), event.SecretHash...),
		LockTime:     time.UnixMilli(event.LockTime).UTC(),
		TxData:       append([]byte(nil), event.TxData...),
	}

	if err := s.checkSwapContractDedup(event.MatchID, event.CoinID, event.Contract, contract.SecretHash, event.Maker); err != nil {
		return nil, err
	}

	mid := db.MarketMatchID{
		MatchID: event.MatchID,
		Base:    event.Base,
		Quote:   event.Quote,
	}
	record := &db.SwapContract{
		MID:       mid,
		Maker:     event.Maker,
		Contract:  append([]byte(nil), event.Contract...),
		CoinID:    append([]byte(nil), event.CoinID...),
		Timestamp: event.SwapTime,
	}

	return &validatedSwapContractRecordedEvent{
		event:    event,
		match:    match,
		status:   status,
		contract: contract,
		record:   record,
	}, nil
}

func (s *Swapper) applySwapContractRecordedMemory(validated *validatedSwapContractRecordedEvent) {
	event, status := validated.event, validated.status
	s.registerSwapContractDedup(event.MatchID, event.CoinID, event.Contract, validated.contract.SecretHash, event.Maker)

	swapTime := time.UnixMilli(event.SwapTime).UTC()
	status.mtx.Lock()
	status.swap = validated.contract
	status.swapTime = swapTime
	status.mtx.Unlock()

	validated.match.mtx.Lock()
	validated.match.Status = event.Status
	validated.match.mtx.Unlock()
}

func newSwapRedemptionRecordedEvent(step *stepInformation, params *msgjson.Redeem, redemption asset.Coin, redeemTime time.Time) (*mesh.Event, error) {
	secret := dex.Bytes(nil)
	if step.actor.isMaker {
		secret = append(secret, params.Secret...)
	}
	coin := coinProjection(redemption)
	event := &meshevents.SwapRedemptionRecordedEvent{
		MatchID:    step.match.ID(),
		Base:       step.match.Maker.BaseAsset,
		Quote:      step.match.Maker.QuoteAsset,
		Maker:      step.actor.isMaker,
		Status:     step.nextStep,
		CoinID:     append(dex.Bytes(nil), params.CoinID...),
		CoinTxID:   coin.txID,
		CoinString: coin.coinString,
		Value:      coin.value,
		FeeRate:    coin.feeRate,
		Secret:     secret,
		RedeemTime: redeemTime.UnixMilli(),
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

func swapRedemptionRequiredStatus(maker bool) order.MatchStatus {
	if maker {
		return order.TakerSwapCast
	}
	return order.MakerRedeemed
}

type validatedSwapRedemptionRecordedEvent struct {
	event       *meshevents.SwapRedemptionRecordedEvent
	match       *matchTracker
	actorStatus *swapStatus
	redemption  asset.Coin
	redeemTime  time.Time
	record      *db.SwapRedemption
}

func (s *Swapper) applySwapRedemptionRecordedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.SwapRedemptionRecordedEvent) (*db.EventLogEntry, error) {
	validated, err := s.validateSwapRedemptionRecordedEvent(event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplySwapRedemptionRecordedEvent(
		ctx, dbEventLogMeta(meta, meshEvent), s.authMgr.ReputationOutcomePolicy(), validated.record)
	if err != nil {
		return nil, fmt.Errorf("saving redeem transaction (match id=%v, maker=%v): %w",
			event.MatchID, event.Maker, err)
	}
	s.applySwapRedemptionRecordedMemory(validated)
	return logEntry, nil
}

func (s *Swapper) validateSwapRedemptionRecordedEvent(event *meshevents.SwapRedemptionRecordedEvent) (*validatedSwapRedemptionRecordedEvent, error) {
	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		return nil, fmt.Errorf("swap redemption recorded event for unknown match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, fmt.Errorf("swap redemption recorded market mismatch for match %v", event.MatchID)
	}

	actorStatus, counterPartyStatus := match.takerStatus, match.makerStatus
	if event.Maker {
		actorStatus, counterPartyStatus = match.makerStatus, match.takerStatus
	}

	var cpContract []byte
	counterPartyStatus.mtx.RLock()
	if counterPartyStatus.swap != nil {
		cpContract = append([]byte(nil), counterPartyStatus.swap.ContractData...)
	}
	counterPartyStatus.mtx.RUnlock()
	if len(cpContract) == 0 {
		return nil, fmt.Errorf("counterparty swap contract missing for redemption event match %v", event.MatchID)
	}

	redeemTime := time.UnixMilli(event.RedeemTime).UTC()

	s.matchMtx.RLock()
	if s.matches[event.MatchID] != match {
		s.matchMtx.RUnlock()
		return nil, fmt.Errorf("redeem txn found after match was revoked (match id=%v, maker=%v)",
			event.MatchID, event.Maker)
	}
	requiredStatus := swapRedemptionRequiredStatus(event.Maker)
	match.mtx.RLock()
	localStatus := match.Status
	match.mtx.RUnlock()
	if localStatus != requiredStatus {
		s.matchMtx.RUnlock()
		return nil, fmt.Errorf("swap redemption recorded event requires status %v, local status is %v for match %v",
			requiredStatus, localStatus, event.MatchID)
	}
	s.matchMtx.RUnlock()

	mid := db.MarketMatchID{
		MatchID: event.MatchID,
		Base:    event.Base,
		Quote:   event.Quote,
	}
	record := &db.SwapRedemption{
		MID:       mid,
		Maker:     event.Maker,
		CoinID:    append([]byte(nil), event.CoinID...),
		Secret:    append([]byte(nil), event.Secret...),
		Timestamp: event.RedeemTime,
	}

	return &validatedSwapRedemptionRecordedEvent{
		event:       event,
		match:       match,
		actorStatus: actorStatus,
		redemption: &eventCoin{
			id:         append(dex.Bytes(nil), event.CoinID...),
			txID:       event.CoinTxID,
			coinString: event.CoinString,
			value:      event.Value,
			feeRate:    event.FeeRate,
		},
		redeemTime: redeemTime,
		record:     record,
	}, nil
}

func (s *Swapper) applySwapRedemptionRecordedMemory(validated *validatedSwapRedemptionRecordedEvent) {
	event := validated.event

	validated.actorStatus.mtx.Lock()
	validated.actorStatus.redemption = validated.redemption
	validated.actorStatus.redeemTime = validated.redeemTime
	if event.Maker {
		validated.actorStatus.secret = append([]byte(nil), event.Secret...)
	}
	validated.actorStatus.mtx.Unlock()

	validated.match.mtx.Lock()
	validated.match.Status = event.Status
	validated.match.mtx.Unlock()

	actorOrder := validated.match.Taker
	if event.Maker {
		actorOrder = validated.match.Maker
	}
	s.swapDone(actorOrder, validated.match.Match, false)

	if !event.Maker {
		log.Debugf("Deleting completed match %v", event.MatchID)
		s.matchMtx.Lock()
		if s.matches[event.MatchID] == validated.match {
			s.deleteMatch(validated.match)
		}
		s.matchMtx.Unlock()
	}
}

type validatedAuditAckRecordedEvent struct {
	event *meshevents.AuditAckRecordedEvent
	match *matchTracker
	ack   *db.AuditAck
}

func newAuditAckRecordedEvent(match *matchTracker, maker bool, sig []byte) (*mesh.Event, error) {
	event := &meshevents.AuditAckRecordedEvent{
		MatchID: match.ID(),
		Base:    match.Maker.BaseAsset,
		Quote:   match.Maker.QuoteAsset,
		Maker:   maker,
		Sig:     append(dex.Bytes(nil), sig...),
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

func (s *Swapper) applyAuditAckRecordedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.AuditAckRecordedEvent) (*db.EventLogEntry, error) {
	validated, err := s.validateAuditAckRecordedEvent(ctx, event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplyAuditAckRecordedEvent(ctx, dbEventLogMeta(meta, meshEvent), validated.ack)
	if err != nil {
		return nil, fmt.Errorf("saving audit ack signature for match %v: %w", event.MatchID, err)
	}
	applyAuditAckRecordedMemory(validated)
	return logEntry, nil
}

func (s *Swapper) validateAuditAckRecordedEvent(ctx context.Context, event *meshevents.AuditAckRecordedEvent) (*validatedAuditAckRecordedEvent, error) {
	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		return nil, fmt.Errorf("audit ack recorded event for unknown match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, fmt.Errorf("audit ack recorded market mismatch for match %v", event.MatchID)
	}

	mid := db.MarketMatchID{
		MatchID: event.MatchID,
		Base:    event.Base,
		Quote:   event.Quote,
	}
	ack := &db.AuditAck{
		MID:   mid,
		Maker: event.Maker,
		Sig:   append([]byte(nil), event.Sig...),
	}

	return &validatedAuditAckRecordedEvent{
		event: event,
		match: match,
		ack:   ack,
	}, nil
}

func applyAuditAckRecordedMemory(validated *validatedAuditAckRecordedEvent) {
	match, event := validated.match, validated.event
	match.mtx.Lock()
	defer match.mtx.Unlock()
	if event.Maker {
		match.Sigs.MakerAudit = append([]byte(nil), event.Sig...)
	} else {
		match.Sigs.TakerAudit = append([]byte(nil), event.Sig...)
	}
}

func newRedemptionAckRecordedEvent(match *matchTracker, maker bool, sig []byte) (*mesh.Event, error) {
	event := &meshevents.RedemptionAckRecordedEvent{
		MatchID: match.ID(),
		Base:    match.Maker.BaseAsset,
		Quote:   match.Maker.QuoteAsset,
		Maker:   maker,
		Sig:     append(dex.Bytes(nil), sig...),
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

type validatedRedemptionAckRecordedEvent struct {
	event       *meshevents.RedemptionAckRecordedEvent
	match       *matchTracker
	ack         *db.RedemptionAck
	alreadyGone bool
}

func (s *Swapper) applyRedemptionAckRecordedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.RedemptionAckRecordedEvent) (*db.EventLogEntry, error) {
	validated, err := s.validateRedemptionAckRecordedEvent(event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplyRedemptionAckRecordedEvent(ctx, dbEventLogMeta(meta, meshEvent), validated.ack)
	if err != nil {
		return nil, fmt.Errorf("saving redemption ack signature for match %v: %w", event.MatchID, err)
	}
	s.applyRedemptionAckRecordedMemory(validated)
	return logEntry, nil
}

func (s *Swapper) validateRedemptionAckRecordedEvent(event *meshevents.RedemptionAckRecordedEvent) (*validatedRedemptionAckRecordedEvent, error) {
	ack := &db.RedemptionAck{
		MID: db.MarketMatchID{
			MatchID: event.MatchID,
			Base:    event.Base,
			Quote:   event.Quote,
		},
		Maker: event.Maker,
		Sig:   append([]byte(nil), event.Sig...),
	}

	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()

	if match == nil {
		// Match already gone is fine (ack after taker redeem deleted it).
		// DB still stores the ack by match ID.
		log.Debugf("Recording %s redemption ack for already removed match %v",
			makerTaker(event.Maker), event.MatchID)
		return &validatedRedemptionAckRecordedEvent{
			event:       event,
			alreadyGone: true,
			ack:         ack,
		}, nil
	}

	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, fmt.Errorf("redemption ack recorded market mismatch for match %v", event.MatchID)
	}

	return &validatedRedemptionAckRecordedEvent{
		event: event,
		match: match,
		ack:   ack,
	}, nil
}

func (s *Swapper) applyRedemptionAckRecordedMemory(validated *validatedRedemptionAckRecordedEvent) {
	if validated.alreadyGone {
		return
	}

	match, event := validated.match, validated.event
	if event.Maker {
		// Defensive: unexpected live match after taker redeem; delete to free dedup.
		log.Errorf("Maker redemption ack found live match %v; deleting as defense-in-depth",
			event.MatchID)
		s.matchMtx.Lock()
		if s.matches[event.MatchID] == match {
			s.deleteMatch(match)
		}
		s.matchMtx.Unlock()
	}

	match.mtx.Lock()
	defer match.mtx.Unlock()

	if event.Maker {
		match.Sigs.MakerRedeem = append([]byte(nil), event.Sig...)
		return
	}
	match.Sigs.TakerRedeem = append([]byte(nil), event.Sig...)
}

// newMatchFailedEvent builds a match_failed from decision-time status.
func (s *Swapper) newMatchFailedEvent(match *matchTracker, status order.MatchStatus, userFault bool, takerAddrFault bool) (*mesh.Event, error) {
	reason, err := matchFailureReason(status, userFault, takerAddrFault)
	if err != nil {
		return nil, err
	}

	event := &meshevents.MatchFailedEvent{
		MatchID:  match.ID(),
		Base:     match.Maker.BaseAsset,
		Quote:    match.Maker.QuoteAsset,
		FailTime: time.Now().Truncate(time.Millisecond).UTC().UnixMilli(),
		Reason:   reason,
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return nil, err
	}

	return meshEvent, nil
}

func matchFailureReason(status order.MatchStatus, userFault bool, takerAddrFault bool) (meshevents.MatchFailureReason, error) {
	if takerAddrFault && (!userFault || status != order.NewlyMatched) {
		return meshevents.MatchFailureReasonInvalid, fmt.Errorf("invalid taker address fault at status %v", status)
	}
	if !userFault {
		switch status {
		case order.NewlyMatched:
			return meshevents.MatchFailureNoFaultNewlyMatched, nil
		case order.MakerSwapCast:
			return meshevents.MatchFailureNoFaultMakerSwapCast, nil
		case order.TakerSwapCast:
			return meshevents.MatchFailureNoFaultTakerSwapCast, nil
		case order.MakerRedeemed:
			return meshevents.MatchFailureNoFaultMakerRedeemed, nil
		default:
			return meshevents.MatchFailureReasonInvalid, fmt.Errorf("invalid failMatch status %v", status)
		}
	}
	switch status {
	case order.NewlyMatched:
		if takerAddrFault {
			return meshevents.MatchFailureTakerNoAddress, nil
		}
		return meshevents.MatchFailureMakerNoSwap, nil
	case order.MakerSwapCast:
		return meshevents.MatchFailureTakerNoSwap, nil
	case order.TakerSwapCast:
		return meshevents.MatchFailureMakerNoRedeem, nil
	case order.MakerRedeemed:
		return meshevents.MatchFailureTakerNoRedeem, nil
	default:
		return meshevents.MatchFailureReasonInvalid, fmt.Errorf("invalid failMatch status %v", status)
	}
}

func (s *Swapper) applyMatchFailedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.MatchFailedEvent) (*db.EventLogEntry, error) {
	match, update, err := s.prepareMatchFailedEvent(event)
	if err != nil {
		return nil, err
	}

	logEntry, err := s.storage.ApplyMatchFailedEvent(
		ctx, dbEventLogMeta(meta, meshEvent), s.authMgr.ReputationOutcomePolicy(), update)
	if err != nil {
		return nil, fmt.Errorf("applying match_failed event for match %v: %w", event.MatchID, err)
	}

	s.applyMatchFailedMemory(match, update.Reason)
	return logEntry, nil
}

// errMatchFailedRaceLost is a match_failed rejection: the match is gone or
// its status no longer matches the event's reason.
var errMatchFailedRaceLost = errors.New("match_failed decision superseded")

func (s *Swapper) prepareMatchFailedEvent(event *meshevents.MatchFailedEvent) (*matchTracker, *db.MatchFailedUpdate, error) {
	if err := event.Validate(); err != nil {
		return nil, nil, err
	}

	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		return nil, nil, fmt.Errorf("%w: match_failed event for unknown match %v", errMatchFailedRaceLost, event.MatchID)
	}
	if match.Taker.Type() == order.CancelOrderType {
		return nil, nil, fmt.Errorf("match_failed event for cancel match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, nil, fmt.Errorf("match_failed market mismatch for match %v", event.MatchID)
	}

	reason := db.MatchFailureReason(event.Reason)
	match.mtx.RLock()
	localStatus := match.Status
	match.mtx.RUnlock()
	details, _ := db.MatchFailureReasonDetails(reason)
	if localStatus != details.Status {
		return nil, nil, fmt.Errorf("%w: match_failed reason %d requires status %v, local status is %v for match %v",
			errMatchFailedRaceLost, event.Reason, details.Status, localStatus, event.MatchID)
	}

	mid := db.MarketMatchID{
		MatchID: event.MatchID,
		Base:    event.Base,
		Quote:   event.Quote,
	}
	update := &db.MatchFailedUpdate{
		MID:        mid,
		FailTimeMS: event.FailTime,
		Reason:     reason,
	}

	return match, update, nil
}

func (s *Swapper) applyMatchFailedMemory(match *matchTracker, reason db.MatchFailureReason) {
	matchID := match.ID()

	s.matchMtx.Lock()
	if s.matches[matchID] == match {
		s.deleteMatch(match)
	}
	s.matchMtx.Unlock()

	details, _ := db.MatchFailureReasonDetails(reason)
	if details.ProcessMaker() {
		s.swapDone(match.Maker, match.Match, details.MakerFault())
	}
	s.swapDone(match.Taker, match.Match, details.TakerFault())

	s.revoke(match)
}

func newMatchAcksRecordedEvent(ackTime time.Time, records []meshevents.MatchAckRecord) (*mesh.Event, error) {
	event := &meshevents.MatchAcksRecordedEvent{
		AckTime: ackTime.UnixMilli(),
		Records: records,
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

type matchAckApply struct {
	record meshevents.MatchAckRecord
	match  *matchTracker
	cancel bool
}

func (s *Swapper) validateMatchAcksRecordedEvent(event *meshevents.MatchAcksRecordedEvent) ([]matchAckApply, *db.MatchAcksRecordedUpdate, error) {
	applies := make([]matchAckApply, 0, len(event.Records))
	acks := make([]*db.MatchAck, 0, len(event.Records))
	for _, record := range event.Records {
		s.matchMtx.RLock()
		match := s.matches[record.MatchID]
		s.matchMtx.RUnlock()

		cancel := record.Cancel
		if match == nil {
			if !cancel {
				return nil, nil, fmt.Errorf("match %v was not registered", record.MatchID)
			}
		} else {
			cancel = match.Taker.Type() == order.CancelOrderType
			if cancel != record.Cancel {
				return nil, nil, fmt.Errorf("match %v cancel flag mismatch", record.MatchID)
			}
			if record.Base != match.Maker.BaseAsset || record.Quote != match.Maker.QuoteAsset {
				return nil, nil, fmt.Errorf("match %v market mismatch", record.MatchID)
			}
		}
		mid := db.MarketMatchID{
			MatchID: record.MatchID,
			Base:    record.Base,
			Quote:   record.Quote,
		}
		applies = append(applies, matchAckApply{
			record: record,
			match:  match,
			cancel: cancel,
		})
		acks = append(acks, &db.MatchAck{
			MID:     mid,
			Maker:   record.Maker,
			Cancel:  cancel,
			Sig:     append([]byte(nil), record.Sig...),
			Address: record.Address,
		})
	}
	return applies, &db.MatchAcksRecordedUpdate{Acks: acks}, nil
}

func (s *Swapper) applyMatchAckUpdates(ackTime time.Time, applies []matchAckApply) {
	var addrNotifications []*matchTracker
	for _, apply := range applies {
		match := apply.match
		if match == nil {
			continue
		}

		sig, addr := &match.Sigs.TakerMatch, &match.takerSwapAddr
		if apply.record.Maker {
			sig, addr = &match.Sigs.MakerMatch, &match.makerSwapAddr
		}

		match.mtx.Lock()
		*sig = apply.record.Sig
		if !apply.cancel {
			if *addr == "" || *addr == apply.record.Address {
				*addr = apply.record.Address
			} else { // first recorded address wins
				log.Warnf("applyMatchAckUpdates: match %v (maker=%v) re-ack address %q, "+
					"keeping recorded %q",
					apply.record.MatchID, apply.record.Maker, apply.record.Address, *addr)
			}
		}
		if !apply.cancel && match.makerSwapAddr != "" && match.takerSwapAddr != "" && !match.counterPartyAddrsSent {
			match.counterPartyAddrsSent = true
			match.time = ackTime
			addrNotifications = append(addrNotifications, match)
		}
		match.mtx.Unlock()
	}

	for _, match := range addrNotifications {
		s.sendCounterPartyAddresses(match)
	}
}

func (s *Swapper) applyMatchAcksRecordedEvent(ctx context.Context, meshEvent *mesh.Event, meta *db.EventLogPosition, event *meshevents.MatchAcksRecordedEvent) (*db.EventLogEntry, error) {
	applies, update, err := s.validateMatchAcksRecordedEvent(event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplyMatchAcksRecordedEvent(ctx, dbEventLogMeta(meta, meshEvent), update)
	if err != nil {
		return nil, fmt.Errorf("saving match acks: %w", err)
	}
	s.applyMatchAckUpdates(time.UnixMilli(event.AckTime).UTC(), applies)
	return logEntry, nil
}

var _ MeshService = (*mesh.Service)(nil)
