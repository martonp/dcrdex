// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"encoding/json"
	"fmt"
)

// Market lifecycle action strings replicated on the wire.
const (
	LifecycleActionScheduleSuspend = "schedule_suspend"
	LifecycleActionSuspend         = "suspend"
	LifecycleActionScheduleResume  = "schedule_resume"
	LifecycleActionResume          = "resume"
)

// ValidLifecycleAction indicates whether action is a known market lifecycle
// action.
func ValidLifecycleAction(action string) bool {
	switch action {
	case LifecycleActionScheduleSuspend, LifecycleActionSuspend,
		LifecycleActionScheduleResume, LifecycleActionResume:
		return true
	}
	return false
}

// MarketLifecycleEvent suspends or resumes a market, or schedules that.
type MarketLifecycleEvent struct {
	Action string `json:"action"`
	Market string `json:"market"`
	// EpochIdx is the last trading epoch for suspend actions and the resume
	// epoch for resume actions.
	EpochIdx int64 `json:"epochIdx"`
	EpochDur int64 `json:"epochDur"` // milliseconds
	// PersistBook is the operator's book disposition, carried only by
	// schedule_suspend; the later actions read the disposition persisted in
	// the durable lifecycle row.
	PersistBook *bool `json:"persistBook,omitempty"`
	// Timestamp is the master's decision clock in Unix ms. It stamps the
	// revocations of the executing actions (suspend, resume) and pins
	// resume's first open epoch when the scheduled epoch already passed.
	Timestamp int64 `json:"timestamp,omitempty"`
	// ResumeRevokes are booked orders the resuming master revokes after
	// re-running the startup book checks; carried by resume only.
	ResumeRevokes []StartupOrderRevokeRecord `json:"resumeRevokes,omitempty"`
	// RunParams re-pins the market run parameters; carried by resume only.
	RunParams *MarketRunParams `json:"runParams,omitempty"`
}

// NewMarketLifecycleEvent builds a market_lifecycle event with the given
// action pivoting on the given epoch. The caller populates the remaining
// action-specific fields.
func NewMarketLifecycleEvent(action, marketName string, epochIdx, epochDur int64) *MarketLifecycleEvent {
	return &MarketLifecycleEvent{
		Action:   action,
		Market:   marketName,
		EpochIdx: epochIdx,
		EpochDur: epochDur,
	}
}

// Kind identifies the mesh event kind.
func (e *MarketLifecycleEvent) Kind() string { return EventKindMarketLifecycle }

// Encode returns the canonical wire payload replicated across the mesh.
func (e *MarketLifecycleEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeMarketLifecycleEvent decodes and validates a market_lifecycle wire
// payload.
func DecodeMarketLifecycleEvent(payload []byte) (*MarketLifecycleEvent, error) {
	return decodeEvent[MarketLifecycleEvent](payload)
}

// Validate checks the event is well-formed, independent of any node state.
func (e *MarketLifecycleEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("nil market lifecycle event")
	}
	if e.Market == "" {
		return fmt.Errorf("market_lifecycle event missing market name")
	}
	if !ValidLifecycleAction(e.Action) {
		return fmt.Errorf("market_lifecycle event unknown action %q", e.Action)
	}
	if e.EpochIdx <= 0 {
		return fmt.Errorf("market_lifecycle event invalid epoch %d", e.EpochIdx)
	}
	if e.EpochDur <= 0 {
		return fmt.Errorf("market_lifecycle event invalid epoch duration %d", e.EpochDur)
	}
	// The executing actions stamp durable revocations with the master's
	// clock, so they must carry it.
	if (e.Action == LifecycleActionSuspend || e.Action == LifecycleActionResume) && e.Timestamp <= 0 {
		return fmt.Errorf("market_lifecycle %s event missing timestamp", e.Action)
	}
	if e.Action == LifecycleActionResume {
		if err := e.RunParams.Validate(); err != nil {
			return fmt.Errorf("market_lifecycle resume event: %w", err)
		}
	} else if e.RunParams != nil {
		return fmt.Errorf("market_lifecycle %s event must not carry run parameters", e.Action)
	}
	return nil
}
