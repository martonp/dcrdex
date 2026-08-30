// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// MatchAckRecord is a single verified client match acknowledgement carried by
// a match_acks_recorded event.
type MatchAckRecord struct {
	MatchID order.MatchID `json:"matchID"`
	Base    uint32        `json:"base"`
	Quote   uint32        `json:"quote"`
	// Maker is true when the acking user is the match's maker.
	Maker bool `json:"maker"`
	// Cancel marks a cancel match; it involves no swap and carries no Address.
	Cancel  bool      `json:"cancel,omitempty"`
	Sig     dex.Bytes `json:"sig"`
	Address string    `json:"address,omitempty"` // acker's swap address; empty for cancel matches
}

// MatchAcksRecordedEvent is a batch of verified client match acknowledgements.
type MatchAcksRecordedEvent struct {
	AckTime int64            `json:"ackTime"` // Unix ms; master's verification time
	Records []MatchAckRecord `json:"records"`
}

// Kind identifies the mesh event kind.
func (e *MatchAcksRecordedEvent) Kind() string { return EventKindMatchAcksRecorded }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *MatchAcksRecordedEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeMatchAcksRecordedEvent decodes and validates a match_acks_recorded
// wire payload.
func DecodeMatchAcksRecordedEvent(payload []byte) (*MatchAcksRecordedEvent, error) {
	return decodeEvent[MatchAcksRecordedEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *MatchAcksRecordedEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil match acks recorded event")
	}
	if e.AckTime == 0 {
		return fmt.Errorf("empty match acks recorded time")
	}
	if len(e.Records) == 0 {
		return fmt.Errorf("empty match acks recorded event")
	}
	for i, record := range e.Records {
		if len(record.Sig) == 0 {
			return fmt.Errorf("empty match ack signature at index %d", i)
		}
		if !record.Cancel && record.Address == "" {
			return fmt.Errorf("empty match ack address for non-cancel match %v", record.MatchID)
		}
	}
	return nil
}
