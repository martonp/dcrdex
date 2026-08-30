// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// SwapRedemptionRecordedEvent is an audited on-chain redemption.
type SwapRedemptionRecordedEvent struct {
	MatchID order.MatchID `json:"matchID"`
	Base    uint32        `json:"base"`
	Quote   uint32        `json:"quote"`
	// Maker is true when this is the maker's redemption (the redeeming side).
	Maker bool `json:"maker"`
	// Status is the match status after this redemption is recorded.
	Status     order.MatchStatus `json:"status"`
	CoinID     dex.Bytes         `json:"coinID"`
	CoinTxID   string            `json:"coinTxID,omitempty"`
	CoinString string            `json:"coinString,omitempty"`
	Value      uint64            `json:"value"`
	FeeRate    uint64            `json:"feeRate"`
	Secret     dex.Bytes         `json:"secret,omitempty"`
	RedeemTime int64             `json:"redeemTime"` // Unix ms
}

// Kind identifies the mesh event kind.
func (e *SwapRedemptionRecordedEvent) Kind() string { return EventKindSwapRedemptionRecorded }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *SwapRedemptionRecordedEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeSwapRedemptionRecordedEvent decodes and validates a
// swap_redemption_recorded wire payload.
func DecodeSwapRedemptionRecordedEvent(payload []byte) (*SwapRedemptionRecordedEvent, error) {
	return decodeEvent[SwapRedemptionRecordedEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *SwapRedemptionRecordedEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil swap redemption recorded event")
	}
	if e.MatchID == (order.MatchID{}) {
		return fmt.Errorf("empty swap redemption recorded match id")
	}
	if e.Base == 0 && e.Quote == 0 {
		return fmt.Errorf("empty swap redemption recorded market for match %v", e.MatchID)
	}
	if len(e.CoinID) == 0 {
		return fmt.Errorf("empty swap redemption coin id for match %v", e.MatchID)
	}
	if e.RedeemTime == 0 {
		return fmt.Errorf("empty swap redemption time for match %v", e.MatchID)
	}
	if e.Maker {
		if e.Status != order.MakerRedeemed {
			return fmt.Errorf("maker swap redemption event has status %v", e.Status)
		}
	} else if e.Status != order.MatchComplete {
		return fmt.Errorf("taker swap redemption event has status %v", e.Status)
	}
	return nil
}
