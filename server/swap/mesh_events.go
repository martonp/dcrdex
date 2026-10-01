// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"context"
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

// MeshService executes swap commands and applies swap events.
type MeshService interface {
	ExecuteCommand(context.Context, mesh.CommandRequest) *msgjson.Error
	ApplyEvent(context.Context, *mesh.Event) (any, error)
}

// SetMeshService sets the mesh service used by the swapper.
func (s *Swapper) SetMeshService(mesh MeshService) {
	s.mesh = mesh
}

// Events returns the mesh event appliers.
func (s *Swapper) Events() map[string]mesh.EventApplier {
	return map[string]mesh.EventApplier{
		meshevents.EventKindMatchAcksRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			acks, err := meshevents.DecodeMatchAcksRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyMatchAcksRecordedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), acks)
		},
		meshevents.EventKindSwapContractRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeSwapContractRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applySwapContractRecordedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), recorded)
		},
		meshevents.EventKindAuditAckRecorded: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			recorded, err := meshevents.DecodeAuditAckRecordedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return s.applyAuditAckRecordedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), recorded)
		},
	}
}

func dbEventLogMeta(position *db.EventLogPosition, event *mesh.Event) *db.EventLogMeta {
	if event == nil {
		return nil
	}
	logMeta := &db.EventLogMeta{
		Event: event.Payload,
	}
	if position != nil {
		logMeta.Seq = position.Seq
		logMeta.ExpectedTipHash = position.TipHash
	}
	return logMeta
}

func newSwapContractRecordedEvent(step *stepInformation, params *msgjson.Init, contract *asset.Contract, swapTime time.Time) (*mesh.Event, error) {
	event := &meshevents.SwapContractRecordedEvent{
		MatchID:     step.match.ID(),
		Base:        step.match.Maker.BaseAsset,
		Quote:       step.match.Maker.QuoteAsset,
		Maker:       step.actor.isMaker,
		Status:      step.nextStep,
		CoinID:      params.CoinID,
		CoinTxID:    contract.TxID(),
		CoinString:  contract.String(),
		Value:       contract.Value(),
		FeeRate:     contract.FeeRate(),
		Contract:    params.Contract,
		SwapAddress: contract.SwapAddress,
		SecretHash:  contract.SecretHash,
		LockTime:    contract.LockTime.UnixMilli(),
		TxData:      contract.TxData,
		SwapTime:    swapTime.UnixMilli(),
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

// eventCoin implements asset.Coin using the details recorded in an event,
// allowing event application to reconstruct a contract without querying the
// blockchain. Confirmation counts are fetched from the backend when requested.
type eventCoin struct {
	id            dex.Bytes
	txID          string
	coinString    string
	value         uint64
	feeRate       uint64
	confirmations func(context.Context) (int64, error)
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

func (s *Swapper) applySwapContractRecordedEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.SwapContractRecordedEvent) (*db.EventLogEntry, error) {
	match, status, contract, err := s.prepareSwapContractUpdate(event)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplySwapContractRecordedEvent(ctx, meta, event)
	if err != nil {
		return nil, fmt.Errorf("saving swap contract for match %v: %w", event.MatchID, err)
	}

	s.registerSwapContractDedup(event.MatchID, event.CoinID, event.Contract, event.SecretHash, event.Maker)

	status.mtx.Lock()
	status.swap = contract
	status.swapTime = time.UnixMilli(event.SwapTime).UTC()
	status.mtx.Unlock()

	match.mtx.Lock()
	match.Status = event.Status
	match.mtx.Unlock()
	return logEntry, nil
}

// prepareSwapContractUpdate checks the match and contract reuse, then reconstructs
// the contract from the event's recorded details.
func (s *Swapper) prepareSwapContractUpdate(event *meshevents.SwapContractRecordedEvent) (*matchTracker, *swapStatus, *asset.Contract, error) {
	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		return nil, nil, nil, fmt.Errorf("swap contract recorded event for unknown match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, nil, nil, fmt.Errorf("swap contract recorded market mismatch for match %v", event.MatchID)
	}

	status := match.takerStatus
	if event.Maker {
		status = match.makerStatus
	}
	swapAsset := s.coins[status.swapAsset]
	if swapAsset == nil {
		return nil, nil, nil, fmt.Errorf("no swap asset %d for match %v", status.swapAsset, event.MatchID)
	}

	if err := s.checkSwapContractDedup(event.MatchID, event.CoinID, event.Contract, event.SecretHash, event.Maker); err != nil {
		return nil, nil, nil, err
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

	return match, status, contract, nil
}

func newAuditAckRecordedEvent(match *matchTracker, maker bool, sig []byte) (*mesh.Event, error) {
	event := &meshevents.AuditAckRecordedEvent{
		MatchID: match.ID(),
		Base:    match.Maker.BaseAsset,
		Quote:   match.Maker.QuoteAsset,
		Maker:   maker,
		Sig:     sig,
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return mesh.NewEvent(event)
}

// applyAuditAckRecordedEvent stores an audit signature before updating the
// tracked match's acknowledgements.
func (s *Swapper) applyAuditAckRecordedEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.AuditAckRecordedEvent) (*db.EventLogEntry, error) {
	s.matchMtx.RLock()
	match := s.matches[event.MatchID]
	s.matchMtx.RUnlock()
	if match == nil {
		return nil, fmt.Errorf("audit ack recorded event for unknown match %v", event.MatchID)
	}
	if event.Base != match.Maker.BaseAsset || event.Quote != match.Maker.QuoteAsset {
		return nil, fmt.Errorf("audit ack recorded market mismatch for match %v", event.MatchID)
	}
	logEntry, err := s.storage.ApplyAuditAckRecordedEvent(ctx, meta, event)
	if err != nil {
		return nil, fmt.Errorf("saving audit ack signature for match %v: %w", event.MatchID, err)
	}
	match.mtx.Lock()
	if event.Maker {
		match.Sigs.MakerAudit = event.Sig
	} else {
		match.Sigs.TakerAudit = event.Sig
	}
	match.mtx.Unlock()
	return logEntry, nil
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

// applyMatchAcksRecordedEvent stores acknowledgements before updating tracked
// swaps and sending counterparty addresses.
func (s *Swapper) applyMatchAcksRecordedEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.MatchAcksRecordedEvent) (*db.EventLogEntry, error) {
	acks, err := s.resolveMatchAckRecords(event.Records)
	if err != nil {
		return nil, err
	}
	logEntry, err := s.storage.ApplyMatchAcksRecordedEvent(ctx, meta, event)
	if err != nil {
		return nil, fmt.Errorf("saving match acks: %w", err)
	}
	s.applyMatchAckUpdates(time.UnixMilli(event.AckTime).UTC(), acks)
	return logEntry, nil
}

type resolvedMatchAck struct {
	record meshevents.MatchAckRecord
	match  *matchTracker
}

// resolveMatchAckRecords finds the tracked swaps and checks that their market
// and match type agree with the records. Cancel acknowledgements are omitted
// because cancel matches are not tracked in memory.
func (s *Swapper) resolveMatchAckRecords(records []meshevents.MatchAckRecord) ([]resolvedMatchAck, error) {
	acks := make([]resolvedMatchAck, 0, len(records))
	for _, record := range records {
		s.matchMtx.RLock()
		match := s.matches[record.MatchID]
		s.matchMtx.RUnlock()
		if match == nil {
			if !record.Cancel {
				return nil, fmt.Errorf("match %v was not registered", record.MatchID)
			}
			continue
		}
		if (match.Taker.Type() == order.CancelOrderType) != record.Cancel {
			return nil, fmt.Errorf("match %v cancel flag mismatch", record.MatchID)
		}
		if record.Base != match.Maker.BaseAsset || record.Quote != match.Maker.QuoteAsset {
			return nil, fmt.Errorf("match %v market mismatch", record.MatchID)
		}
		if record.Cancel {
			continue
		}
		acks = append(acks, resolvedMatchAck{record: record, match: match})
	}
	return acks, nil
}

// applyMatchAckUpdates updates acknowledgement signatures and swap addresses.
// When both addresses become available, it starts the maker's deadline and
// notifies each counterparty.
func (s *Swapper) applyMatchAckUpdates(ackTime time.Time, acks []resolvedMatchAck) {
	var addrNotifications []*matchTracker
	for _, ack := range acks {
		match, record := ack.match, ack.record
		match.mtx.Lock()
		sig, addr := &match.Sigs.TakerMatch, &match.takerSwapAddr
		if record.Maker {
			sig, addr = &match.Sigs.MakerMatch, &match.makerSwapAddr
		}

		*sig = record.Sig
		if *addr == "" {
			*addr = record.Address
		} else if *addr != record.Address {
			log.Warnf("applyMatchAckUpdates: match %v (maker=%v) re-ack address %q, "+
				"keeping recorded %q",
				record.MatchID, record.Maker, record.Address, *addr)
		}
		if match.makerSwapAddr != "" && match.takerSwapAddr != "" && !match.counterPartyAddrsSent {
			match.counterPartyAddrsSent = true
			// Give the maker the full broadcast timeout once both addresses are available.
			match.time = ackTime
			addrNotifications = append(addrNotifications, match)
		}
		match.mtx.Unlock()
	}

	for _, match := range addrNotifications {
		s.sendCounterPartyAddresses(match)
	}
}

var _ MeshService = (*mesh.Service)(nil)
