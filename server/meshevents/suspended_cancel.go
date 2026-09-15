// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// SuspendedCancelEvent is a cancel executed against a persisted book while
// the market is suspended. There is no epoch to match in, so the master
// emits it directly from the cancel submission.
type SuspendedCancelEvent struct {
	Market string    `json:"market"`
	Base   uint32    `json:"base"`
	Quote  uint32    `json:"quote"`
	Cancel dex.Bytes `json:"cancel"`
	Target dex.Bytes `json:"target"`
	// EpochIdx and EpochDur (ms) are synthesized from the master's clock —
	// no epoch is open while suspended — and only stamp the recorded cancel
	// match's EpochID.
	EpochIdx        int64  `json:"epochIdx"`
	EpochDur        int64  `json:"epochDur"`
	FeeRateBase     uint64 `json:"feeRateBase"`
	FeeRateQuote    uint64 `json:"feeRateQuote"`
	MatchServerTime int64  `json:"matchServerTime"` // Unix ms
}

// NewSuspendedCancelEvent builds a suspended_cancel event.
func NewSuspendedCancelEvent(marketName string, base, quote uint32, cancel *order.CancelOrder,
	target *order.LimitOrder, epochIdx, epochDur int64, feeRateBase, feeRateQuote uint64,
	matchServerTime time.Time) *SuspendedCancelEvent {

	return &SuspendedCancelEvent{
		Market:          marketName,
		Base:            base,
		Quote:           quote,
		Cancel:          dex.Bytes(order.EncodeOrder(cancel)),
		Target:          dex.Bytes(order.EncodeOrder(target)),
		EpochIdx:        epochIdx,
		EpochDur:        epochDur,
		FeeRateBase:     feeRateBase,
		FeeRateQuote:    feeRateQuote,
		MatchServerTime: matchServerTime.UnixMilli(),
	}
}

// Kind identifies the mesh event kind.
func (e *SuspendedCancelEvent) Kind() string { return EventKindSuspendedCancel }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *SuspendedCancelEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeSuspendedCancelEvent decodes and validates a suspended_cancel wire
// payload.
func DecodeSuspendedCancelEvent(payload []byte) (*SuspendedCancelEvent, error) {
	return decodeEvent[SuspendedCancelEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state,
// including that the carried orders decode to a cancel and a standing limit
// order.
func (e *SuspendedCancelEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil suspended cancel event")
	}
	if e.Market == "" {
		return fmt.Errorf("suspended_cancel event missing market name")
	}
	if _, err := e.CancelOrder(); err != nil {
		return err
	}
	if _, err := e.TargetOrder(); err != nil {
		return err
	}
	return nil
}

// CancelOrder decodes the cancel order carried by the event.
func (e *SuspendedCancelEvent) CancelOrder() (*order.CancelOrder, error) {
	ord, err := order.DecodeOrder(e.Cancel)
	if err != nil {
		return nil, err
	}
	cancel, ok := ord.(*order.CancelOrder)
	if !ok {
		return nil, fmt.Errorf("suspended_cancel order is %T, want cancel", ord)
	}
	return cancel, nil
}

// TargetOrder decodes the target standing limit order carried by the event.
func (e *SuspendedCancelEvent) TargetOrder() (*order.LimitOrder, error) {
	ord, err := order.DecodeOrder(e.Target)
	if err != nil {
		return nil, err
	}
	target, ok := ord.(*order.LimitOrder)
	if !ok || target.Force != order.StandingTiF {
		return nil, fmt.Errorf("suspended_cancel target is not standing limit")
	}
	return target, nil
}

// MatchServerTimeTime is the match's server timestamp.
func (e *SuspendedCancelEvent) MatchServerTimeTime() time.Time {
	return time.UnixMilli(e.MatchServerTime).UTC()
}
