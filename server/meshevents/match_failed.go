// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"

	"decred.org/dcrdex/dex/order"
)

// MatchFailureReason identifies why a match failed. It is the catalog-local
// equivalent of db.MatchFailureReason, which meshevents cannot import; the
// constant values and JSON (number) encoding must remain identical. The
// durable meaning of each reason (required status, fault side, reputation
// outcome) is db-side derivation and stays with db.MatchFailureReasonDetails.
type MatchFailureReason uint8

const (
	MatchFailureReasonInvalid MatchFailureReason = iota
	MatchFailureNoFaultNewlyMatched
	MatchFailureNoFaultMakerSwapCast
	MatchFailureNoFaultTakerSwapCast
	MatchFailureNoFaultMakerRedeemed
	MatchFailureMakerNoSwap
	MatchFailureTakerNoAddress
	MatchFailureTakerNoSwap
	MatchFailureMakerNoRedeem
	MatchFailureTakerNoRedeem
)

// ValidMatchFailureReason indicates whether reason is a known match failure
// reason.
func ValidMatchFailureReason(reason MatchFailureReason) bool {
	switch reason {
	case MatchFailureNoFaultNewlyMatched, MatchFailureNoFaultMakerSwapCast,
		MatchFailureNoFaultTakerSwapCast, MatchFailureNoFaultMakerRedeemed,
		MatchFailureMakerNoSwap, MatchFailureTakerNoAddress,
		MatchFailureTakerNoSwap, MatchFailureMakerNoRedeem,
		MatchFailureTakerNoRedeem:
		return true
	}
	return false
}

// MatchFailedEvent ends a match without a full swap.
type MatchFailedEvent struct {
	MatchID  order.MatchID      `json:"matchID"`
	Base     uint32             `json:"base"`
	Quote    uint32             `json:"quote"`
	FailTime int64              `json:"failTime"` // Unix ms; stamps durable revocations
	Reason   MatchFailureReason `json:"reason"`
}

// Kind identifies the mesh event kind.
func (e *MatchFailedEvent) Kind() string { return EventKindMatchFailed }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *MatchFailedEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeMatchFailedEvent decodes and validates a match_failed wire payload.
func DecodeMatchFailedEvent(payload []byte) (*MatchFailedEvent, error) {
	return decodeEvent[MatchFailedEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *MatchFailedEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil match failed event")
	}
	if e.MatchID == (order.MatchID{}) {
		return fmt.Errorf("empty match_failed match ID")
	}
	if e.Base == 0 && e.Quote == 0 {
		return fmt.Errorf("empty match_failed market for match %v", e.MatchID)
	}
	if e.FailTime <= 0 {
		return fmt.Errorf("empty match_failed time for match %v", e.MatchID)
	}
	if !ValidMatchFailureReason(e.Reason) {
		return fmt.Errorf("invalid match failure reason %d", e.Reason)
	}
	return nil
}
