// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"
)

// AdvanceEpochEvent closes one epoch and optionally opens the next.
// The closed epoch's orders come from local market state.
type AdvanceEpochEvent struct {
	Market         string `json:"market"`
	ClosedEpochIdx int64  `json:"closedEpochIdx"`
	// OpenedEpochIdx is the epoch that opens after the close. Zero means
	// close the final epoch without opening another.
	OpenedEpochIdx int64 `json:"openedEpochIdx"`
	EpochDur       int64 `json:"epochDur"` // milliseconds
}

// NewAdvanceEpochEvent builds an advance_epoch event. A zero openedEpochIdx
// means the market closes its final epoch without opening a new one.
func NewAdvanceEpochEvent(marketName string, closedEpochIdx, openedEpochIdx, epochDur int64) *AdvanceEpochEvent {
	return &AdvanceEpochEvent{
		Market:         marketName,
		ClosedEpochIdx: closedEpochIdx,
		OpenedEpochIdx: openedEpochIdx,
		EpochDur:       epochDur,
	}
}

// Kind identifies the mesh event kind.
func (e *AdvanceEpochEvent) Kind() string { return EventKindAdvanceEpoch }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *AdvanceEpochEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeAdvanceEpochEvent decodes and validates an advance_epoch wire payload.
func DecodeAdvanceEpochEvent(payload []byte) (*AdvanceEpochEvent, error) {
	return decodeEvent[AdvanceEpochEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *AdvanceEpochEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil advance epoch event")
	}
	if e.Market == "" {
		return fmt.Errorf("advance_epoch event missing market name")
	}
	if e.EpochDur <= 0 {
		return fmt.Errorf("advance_epoch event invalid epoch duration %d", e.EpochDur)
	}
	if e.ClosedEpochIdx < 0 {
		return fmt.Errorf("advance_epoch event invalid closed epoch %d", e.ClosedEpochIdx)
	}
	if e.OpenedEpochIdx == 0 {
		return nil
	}
	if e.OpenedEpochIdx != e.ClosedEpochIdx+1 {
		return fmt.Errorf("advance_epoch event opened epoch %d does not follow closed epoch %d",
			e.OpenedEpochIdx, e.ClosedEpochIdx)
	}
	return nil
}
