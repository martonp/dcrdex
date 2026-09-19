// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"fmt"
)

// MarketRunParams contains the trading parameters for a market run.
// These parameters are recorded in the event log so all nodes use the same
// rules for order validation and matching when applying or replaying events.
type MarketRunParams struct {
	LotSize                uint64 `json:"lotSize"`
	RateStep               uint64 `json:"rateStep"`
	ParcelSize             uint32 `json:"parcelSize"`
	MaxUserCancelsPerEpoch uint32 `json:"maxUserCancels"`
	MinimumRate            uint64 `json:"minimumRate"`
}

// Validate checks that the run parameters are well-formed.
// MinimumRate and MaxUserCancelsPerEpoch may be zero.
func (p *MarketRunParams) Validate() error {
	if p == nil {
		return fmt.Errorf("missing market run parameters")
	}
	if p.LotSize == 0 {
		return fmt.Errorf("market run parameters missing lot size")
	}
	if p.RateStep == 0 {
		return fmt.Errorf("market run parameters missing rate step")
	}
	if p.ParcelSize == 0 {
		return fmt.Errorf("market run parameters missing parcel size")
	}
	return nil
}
