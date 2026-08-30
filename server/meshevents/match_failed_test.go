// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"decred.org/dcrdex/dex/order"
)

// legacyMatchFailedEvent mirrors the swap-local wire struct this event
// replaced, whose Reason field was the db reason enum with the same numeric
// JSON encoding. Its JSON must match the canonical event byte for byte.
type legacyMatchFailedEvent struct {
	MatchID  order.MatchID `json:"matchID"`
	Base     uint32        `json:"base"`
	Quote    uint32        `json:"quote"`
	FailTime int64         `json:"failTime"`
	Reason   uint8         `json:"reason"`
}

func TestMatchFailedEventWireCompat(t *testing.T) {
	var mid order.MatchID
	mid[0] = 0x01
	event := &MatchFailedEvent{
		MatchID:  mid,
		Base:     42,
		Quote:    0,
		FailTime: 1670000000123,
		Reason:   MatchFailureMakerNoRedeem,
	}
	legacy := &legacyMatchFailedEvent{
		MatchID:  mid,
		Base:     42,
		Quote:    0,
		FailTime: 1670000000123,
		Reason:   uint8(MatchFailureMakerNoRedeem),
	}

	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	legacyPayload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("legacy marshal error: %v", err)
	}
	if !bytes.Equal(payload, legacyPayload) {
		t.Fatalf("wire payload mismatch.\nnew: %s\nold: %s", payload, legacyPayload)
	}

	got, err := DecodeMatchFailedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round trip mismatch.\nwant: %#v\n got: %#v", event, got)
	}
}

func TestMatchFailedEventValidate(t *testing.T) {
	var mid order.MatchID
	mid[0] = 0x01
	valid := func() *MatchFailedEvent {
		return &MatchFailedEvent{
			MatchID:  mid,
			Base:     42,
			FailTime: 1670000000123,
			Reason:   MatchFailureTakerNoSwap,
		}
	}
	mutate := func(f func(e *MatchFailedEvent)) *MatchFailedEvent {
		e := valid()
		f(e)
		return e
	}
	tests := []struct {
		name    string
		event   *MatchFailedEvent
		wantErr bool
	}{
		{"ok", valid(), false},
		{"zero match id", mutate(func(e *MatchFailedEvent) { e.MatchID = order.MatchID{} }), true},
		{"empty market", mutate(func(e *MatchFailedEvent) { e.Base = 0 }), true},
		{"empty fail time", mutate(func(e *MatchFailedEvent) { e.FailTime = 0 }), true},
		{"negative fail time", mutate(func(e *MatchFailedEvent) { e.FailTime = -1 }), true},
		{"invalid reason", mutate(func(e *MatchFailedEvent) { e.Reason = MatchFailureReasonInvalid }), true},
		{"unknown reason", mutate(func(e *MatchFailedEvent) { e.Reason = MatchFailureTakerNoRedeem + 1 }), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
