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

// legacySwapRedemptionRecordedEvent mirrors the swap-local wire struct this
// event replaced. Its JSON must match the canonical event byte for byte.
type legacySwapRedemptionRecordedEvent struct {
	MatchID    order.MatchID     `json:"matchID"`
	Base       uint32            `json:"base"`
	Quote      uint32            `json:"quote"`
	Maker      bool              `json:"maker"`
	Status     order.MatchStatus `json:"status"`
	CoinID     dex.Bytes         `json:"coinID"`
	CoinTxID   string            `json:"coinTxID,omitempty"`
	CoinString string            `json:"coinString,omitempty"`
	Value      uint64            `json:"value"`
	FeeRate    uint64            `json:"feeRate"`
	Secret     dex.Bytes         `json:"secret,omitempty"`
	RedeemTime int64             `json:"redeemTime"`
}

func tSwapRedemptionRecordedEvent() *SwapRedemptionRecordedEvent {
	var mid order.MatchID
	mid[0] = 0x01
	return &SwapRedemptionRecordedEvent{
		MatchID:    mid,
		Base:       42,
		Quote:      0,
		Maker:      true,
		Status:     order.MakerRedeemed,
		CoinID:     dex.Bytes{0x0a, 0x0b},
		CoinTxID:   "txid",
		CoinString: "txid:0",
		Value:      1e8,
		FeeRate:    10,
		Secret:     dex.Bytes{0x0c},
		RedeemTime: 1670000000123,
	}
}

func TestSwapRedemptionRecordedEventWireCompat(t *testing.T) {
	event := tSwapRedemptionRecordedEvent()
	legacy := &legacySwapRedemptionRecordedEvent{
		MatchID:    event.MatchID,
		Base:       event.Base,
		Quote:      event.Quote,
		Maker:      event.Maker,
		Status:     event.Status,
		CoinID:     event.CoinID,
		CoinTxID:   event.CoinTxID,
		CoinString: event.CoinString,
		Value:      event.Value,
		FeeRate:    event.FeeRate,
		Secret:     event.Secret,
		RedeemTime: event.RedeemTime,
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

	got, err := DecodeSwapRedemptionRecordedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round trip mismatch.\nwant: %#v\n got: %#v", event, got)
	}
}

func TestSwapRedemptionRecordedEventValidate(t *testing.T) {
	mutate := func(f func(e *SwapRedemptionRecordedEvent)) *SwapRedemptionRecordedEvent {
		e := tSwapRedemptionRecordedEvent()
		f(e)
		return e
	}
	tests := []struct {
		name    string
		event   *SwapRedemptionRecordedEvent
		wantErr bool
	}{
		{"maker ok", tSwapRedemptionRecordedEvent(), false},
		{"taker ok", mutate(func(e *SwapRedemptionRecordedEvent) {
			e.Maker = false
			e.Status = order.MatchComplete
			e.Secret = nil
		}), false},
		{"zero match id", mutate(func(e *SwapRedemptionRecordedEvent) { e.MatchID = order.MatchID{} }), true},
		{"empty market", mutate(func(e *SwapRedemptionRecordedEvent) { e.Base, e.Quote = 0, 0 }), true},
		{"empty coin id", mutate(func(e *SwapRedemptionRecordedEvent) { e.CoinID = nil }), true},
		{"empty redeem time", mutate(func(e *SwapRedemptionRecordedEvent) { e.RedeemTime = 0 }), true},
		{"maker wrong status", mutate(func(e *SwapRedemptionRecordedEvent) { e.Status = order.MatchComplete }), true},
		{"taker wrong status", mutate(func(e *SwapRedemptionRecordedEvent) {
			e.Maker = false
			e.Status = order.MakerRedeemed
		}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
