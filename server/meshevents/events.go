// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package meshevents is the wire catalog of replicated DEX events.
// Peers must share EventSchemaVersion.
package meshevents

import "encoding/json"

// EventSchemaVersion identifies the event payload semantics that must match
// between mesh peers. Mesh is unreleased, so this stays at 0 until the
// event schema is ready to ship; only bump it once peers in the wild need
// to be told apart by schema semantics.
const EventSchemaVersion uint32 = 0

// decodeEvent unmarshals a wire payload into a freshly allocated event and
// validates it. Every exported per-kind Decode function is a thin wrapper
// around this helper.
func decodeEvent[T any, PT interface {
	*T
	Validate() error
}](payload []byte) (PT, error) {
	e := PT(new(T))
	if err := json.Unmarshal(payload, e); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

const (
	EventKindBondPosted             = "bond_posted"
	EventKindPrepaidBondsCreated    = "prepaid_bonds_created"
	EventKindOrderAccepted          = "order_accepted"
	EventKindMarketStarted          = "market_started"
	EventKindMarketLifecycle        = "market_lifecycle"
	EventKindAdvanceEpoch           = "advance_epoch"
	EventKindEpochProcessed         = "epoch_processed"
	EventKindSuspendedCancel        = "suspended_cancel"
	EventKindMatchAcksRecorded      = "match_acks_recorded"
	EventKindSwapContractRecorded   = "swap_contract_recorded"
	EventKindAuditAckRecorded       = "audit_ack_recorded"
	EventKindSwapRedemptionRecorded = "swap_redemption_recorded"
	EventKindRedemptionAckRecorded  = "redemption_ack_recorded"
	EventKindMatchFailed            = "match_failed"
	EventKindOrdersRevoked          = "orders_revoked"
	EventKindReputationForgiven     = "reputation_forgiven"
)
