// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// legacyRedemptionAckRecordedEvent mirrors the swap-local wire struct this
// event replaced. Its JSON must match the canonical event byte for byte.
type legacyRedemptionAckRecordedEvent struct {
	MatchID order.MatchID `json:"matchID"`
	Base    uint32        `json:"base"`
	Quote   uint32        `json:"quote"`
	Maker   bool          `json:"maker"`
	Sig     dex.Bytes     `json:"sig"`
}

func TestRedemptionAckRecordedEventWireCompat(t *testing.T) {
	var mid order.MatchID
	mid[0] = 0x01
	event := &RedemptionAckRecordedEvent{
		MatchID: mid,
		Base:    42,
		Quote:   0,
		Maker:   true,
		Sig:     dex.Bytes{0x0a, 0x0b},
	}
	legacy := &legacyRedemptionAckRecordedEvent{
		MatchID: mid,
		Base:    42,
		Quote:   0,
		Maker:   true,
		Sig:     dex.Bytes{0x0a, 0x0b},
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

	got, err := DecodeRedemptionAckRecordedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round trip mismatch.\nwant: %#v\n got: %#v", event, got)
	}
}

func TestRedemptionAckRecordedEventValidate(t *testing.T) {
	var mid order.MatchID
	mid[0] = 0x01
	tests := []struct {
		name    string
		event   *RedemptionAckRecordedEvent
		wantErr bool
	}{
		{"ok", &RedemptionAckRecordedEvent{MatchID: mid, Base: 42, Sig: dex.Bytes{0x0a}}, false},
		{"zero match id", &RedemptionAckRecordedEvent{Base: 42, Sig: dex.Bytes{0x0a}}, true},
		{"empty market", &RedemptionAckRecordedEvent{MatchID: mid, Sig: dex.Bytes{0x0a}}, true},
		{"empty sig", &RedemptionAckRecordedEvent{MatchID: mid, Base: 42}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
