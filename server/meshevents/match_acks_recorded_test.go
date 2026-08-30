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

// legacyMatchAckRecord and legacyMatchAcksRecordedEvent mirror the swap-local
// wire structs this event replaced. Their JSON must match the canonical event
// byte for byte.
type legacyMatchAckRecord struct {
	MatchID order.MatchID `json:"matchID"`
	Base    uint32        `json:"base"`
	Quote   uint32        `json:"quote"`
	Maker   bool          `json:"maker"`
	Cancel  bool          `json:"cancel,omitempty"`
	Sig     dex.Bytes     `json:"sig"`
	Address string        `json:"address,omitempty"`
}

type legacyMatchAcksRecordedEvent struct {
	AckTime int64                  `json:"ackTime"`
	Records []legacyMatchAckRecord `json:"records"`
}

func TestMatchAcksRecordedEventWireCompat(t *testing.T) {
	var mid, cancelMID order.MatchID
	mid[0] = 0x01
	cancelMID[0] = 0x02
	event := &MatchAcksRecordedEvent{
		AckTime: 1670000000123,
		Records: []MatchAckRecord{{
			MatchID: mid,
			Base:    42,
			Quote:   0,
			Maker:   true,
			Sig:     dex.Bytes{0x0a, 0x0b},
			Address: "swap-addr",
		}, {
			MatchID: cancelMID,
			Base:    42,
			Quote:   0,
			Cancel:  true,
			Sig:     dex.Bytes{0x0c},
		}},
	}
	legacy := &legacyMatchAcksRecordedEvent{
		AckTime: 1670000000123,
		Records: []legacyMatchAckRecord{{
			MatchID: mid,
			Base:    42,
			Quote:   0,
			Maker:   true,
			Sig:     dex.Bytes{0x0a, 0x0b},
			Address: "swap-addr",
		}, {
			MatchID: cancelMID,
			Base:    42,
			Quote:   0,
			Cancel:  true,
			Sig:     dex.Bytes{0x0c},
		}},
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

	got, err := DecodeMatchAcksRecordedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round trip mismatch.\nwant: %#v\n got: %#v", event, got)
	}
}

func TestMatchAcksRecordedEventValidate(t *testing.T) {
	var mid order.MatchID
	mid[0] = 0x01
	tradeRecord := MatchAckRecord{
		MatchID: mid,
		Base:    42,
		Maker:   true,
		Sig:     dex.Bytes{0x0a},
		Address: "swap-addr",
	}
	cancelRecord := MatchAckRecord{
		MatchID: mid,
		Base:    42,
		Cancel:  true,
		Sig:     dex.Bytes{0x0b},
	}
	noSig := tradeRecord
	noSig.Sig = nil
	noAddr := tradeRecord
	noAddr.Address = ""

	tests := []struct {
		name    string
		event   *MatchAcksRecordedEvent
		wantErr bool
	}{
		{"ok", &MatchAcksRecordedEvent{AckTime: 1, Records: []MatchAckRecord{tradeRecord, cancelRecord}}, false},
		{"no ack time", &MatchAcksRecordedEvent{Records: []MatchAckRecord{tradeRecord}}, true},
		{"no records", &MatchAcksRecordedEvent{AckTime: 1}, true},
		{"empty sig", &MatchAcksRecordedEvent{AckTime: 1, Records: []MatchAckRecord{noSig}}, true},
		{"trade without address", &MatchAcksRecordedEvent{AckTime: 1, Records: []MatchAckRecord{noAddr}}, true},
		{"cancel without address ok", &MatchAcksRecordedEvent{AckTime: 1, Records: []MatchAckRecord{cancelRecord}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
