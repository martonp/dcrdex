// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import "decred.org/dcrdex/dex/order"

// MatchFailureFault identifies which match side caused a match_failed event.
type MatchFailureFault uint8

const (
	MatchFailureNoUserFault MatchFailureFault = iota
	MatchFailureMakerFault
	MatchFailureTakerFault
)

// MatchFailureDetails describes the durable meaning of a match_failed reason.
type MatchFailureDetails struct {
	Status  order.MatchStatus
	Fault   MatchFailureFault
	Outcome Outcome
}

// UserFault reports whether the failure should count as user-caused.
func (d MatchFailureDetails) UserFault() bool {
	return d.Fault != MatchFailureNoUserFault
}

// MakerFault reports whether the maker is the at-fault side.
func (d MatchFailureDetails) MakerFault() bool {
	return d.Fault == MatchFailureMakerFault
}

// TakerFault reports whether the taker is the at-fault side.
func (d MatchFailureDetails) TakerFault() bool {
	return d.Fault == MatchFailureTakerFault
}

// ProcessMaker reports whether match_failed should process maker-side order
// completion/revocation effects. MakerRedeemed failures have already completed
// maker-side accounting.
func (d MatchFailureDetails) ProcessMaker() bool {
	return d.Status != order.MakerRedeemed
}

// MatchFailureReasonDetails returns the status, fault, and side-effect rules for
// a match_failed reason.
func MatchFailureReasonDetails(reason MatchFailureReason) (MatchFailureDetails, bool) {
	switch reason {
	case MatchFailureNoFaultNewlyMatched:
		return MatchFailureDetails{
			Status: order.NewlyMatched,
		}, true
	case MatchFailureNoFaultMakerSwapCast:
		return MatchFailureDetails{
			Status: order.MakerSwapCast,
		}, true
	case MatchFailureNoFaultTakerSwapCast:
		return MatchFailureDetails{
			Status: order.TakerSwapCast,
		}, true
	case MatchFailureNoFaultMakerRedeemed:
		return MatchFailureDetails{
			Status: order.MakerRedeemed,
		}, true
	case MatchFailureMakerNoSwap:
		return MatchFailureDetails{
			Status:  order.NewlyMatched,
			Fault:   MatchFailureMakerFault,
			Outcome: OutcomeNoSwapAsMaker,
		}, true
	case MatchFailureTakerNoAddress:
		return MatchFailureDetails{
			Status:  order.NewlyMatched,
			Fault:   MatchFailureTakerFault,
			Outcome: OutcomeNoAddrAsTaker,
		}, true
	case MatchFailureTakerNoSwap:
		return MatchFailureDetails{
			Status:  order.MakerSwapCast,
			Fault:   MatchFailureTakerFault,
			Outcome: OutcomeNoSwapAsTaker,
		}, true
	case MatchFailureMakerNoRedeem:
		return MatchFailureDetails{
			Status:  order.TakerSwapCast,
			Fault:   MatchFailureMakerFault,
			Outcome: OutcomeNoRedeemAsMaker,
		}, true
	case MatchFailureTakerNoRedeem:
		return MatchFailureDetails{
			Status:  order.MakerRedeemed,
			Fault:   MatchFailureTakerFault,
			Outcome: OutcomeNoRedeemAsTaker,
		}, true
	default:
		return MatchFailureDetails{}, false
	}
}
