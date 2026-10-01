// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"context"
	"fmt"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

// MeshService applies swap events.
type MeshService interface {
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
