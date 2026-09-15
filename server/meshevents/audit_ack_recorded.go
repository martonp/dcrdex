// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// AuditAckRecordedEvent is a client ack of the counterparty's swap contract.
type AuditAckRecordedEvent struct {
	MatchID order.MatchID `json:"matchID"`
	Base    uint32        `json:"base"`
	Quote   uint32        `json:"quote"`
	// Maker is true when the acking client is the match's maker.
	Maker bool      `json:"maker"`
	Sig   dex.Bytes `json:"sig"`
}

// Kind identifies the mesh event kind.
func (e *AuditAckRecordedEvent) Kind() string { return EventKindAuditAckRecorded }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *AuditAckRecordedEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeAuditAckRecordedEvent decodes and validates an audit_ack_recorded
// wire payload.
func DecodeAuditAckRecordedEvent(payload []byte) (*AuditAckRecordedEvent, error) {
	return decodeEvent[AuditAckRecordedEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *AuditAckRecordedEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil audit ack recorded event")
	}
	return validateAckRecorded("audit ack", e.MatchID, e.Base, e.Quote, e.Sig)
}

// validateAckRecorded is the shared validation for the structurally identical
// audit_ack_recorded and redemption_ack_recorded events. what labels the
// errors: "audit ack" or "redemption ack".
func validateAckRecorded(what string, matchID order.MatchID, base, quote uint32, sig dex.Bytes) error {
	if matchID == (order.MatchID{}) {
		return fmt.Errorf("empty %s match id", what)
	}
	if base == 0 && quote == 0 {
		return fmt.Errorf("empty %s market for match %v", what, matchID)
	}
	if len(sig) == 0 {
		return fmt.Errorf("empty %s signature for match %v", what, matchID)
	}
	return nil
}
