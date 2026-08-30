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

// legacySwapContractRecordedEvent mirrors the swap-local wire struct this
// event replaced. Its JSON must match the canonical event byte for byte.
type legacySwapContractRecordedEvent struct {
	MatchID     order.MatchID     `json:"matchID"`
	Base        uint32            `json:"base"`
	Quote       uint32            `json:"quote"`
	Maker       bool              `json:"maker"`
	Status      order.MatchStatus `json:"status"`
	CoinID      dex.Bytes         `json:"coinID"`
	CoinTxID    string            `json:"coinTxID,omitempty"`
	CoinString  string            `json:"coinString,omitempty"`
	Value       uint64            `json:"value"`
	FeeRate     uint64            `json:"feeRate"`
	Contract    dex.Bytes         `json:"contract"`
	SwapAddress string            `json:"swapAddress"`
	SecretHash  dex.Bytes         `json:"secretHash"`
	LockTime    int64             `json:"lockTime"`
	TxData      dex.Bytes         `json:"txData,omitempty"`
	SwapTime    int64             `json:"swapTime"`
}

func tSwapContractRecordedEvent() *SwapContractRecordedEvent {
	var mid order.MatchID
	mid[0] = 0x01
	return &SwapContractRecordedEvent{
		MatchID:     mid,
		Base:        42,
		Quote:       0,
		Maker:       true,
		Status:      order.MakerSwapCast,
		CoinID:      dex.Bytes{0x0a, 0x0b},
		CoinTxID:    "txid",
		CoinString:  "txid:0",
		Value:       1e8,
		FeeRate:     10,
		Contract:    dex.Bytes{0x0c},
		SwapAddress: "swap-addr",
		SecretHash:  dex.Bytes{0x0d},
		LockTime:    1670000000123,
		TxData:      dex.Bytes{0x0e},
		SwapTime:    1670000000456,
	}
}

func TestSwapContractRecordedEventWireCompat(t *testing.T) {
	event := tSwapContractRecordedEvent()
	legacy := &legacySwapContractRecordedEvent{
		MatchID:     event.MatchID,
		Base:        event.Base,
		Quote:       event.Quote,
		Maker:       event.Maker,
		Status:      event.Status,
		CoinID:      event.CoinID,
		CoinTxID:    event.CoinTxID,
		CoinString:  event.CoinString,
		Value:       event.Value,
		FeeRate:     event.FeeRate,
		Contract:    event.Contract,
		SwapAddress: event.SwapAddress,
		SecretHash:  event.SecretHash,
		LockTime:    event.LockTime,
		TxData:      event.TxData,
		SwapTime:    event.SwapTime,
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

	got, err := DecodeSwapContractRecordedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round trip mismatch.\nwant: %#v\n got: %#v", event, got)
	}
}

func TestSwapContractRecordedEventValidate(t *testing.T) {
	mutate := func(f func(e *SwapContractRecordedEvent)) *SwapContractRecordedEvent {
		e := tSwapContractRecordedEvent()
		f(e)
		return e
	}
	tests := []struct {
		name    string
		event   *SwapContractRecordedEvent
		wantErr bool
	}{
		{"maker ok", tSwapContractRecordedEvent(), false},
		{"taker ok", mutate(func(e *SwapContractRecordedEvent) {
			e.Maker = false
			e.Status = order.TakerSwapCast
		}), false},
		{"zero match id", mutate(func(e *SwapContractRecordedEvent) { e.MatchID = order.MatchID{} }), true},
		{"empty market", mutate(func(e *SwapContractRecordedEvent) { e.Base, e.Quote = 0, 0 }), true},
		{"empty coin id", mutate(func(e *SwapContractRecordedEvent) { e.CoinID = nil }), true},
		{"empty contract", mutate(func(e *SwapContractRecordedEvent) { e.Contract = nil }), true},
		{"empty swap address", mutate(func(e *SwapContractRecordedEvent) { e.SwapAddress = "" }), true},
		{"empty secret hash", mutate(func(e *SwapContractRecordedEvent) { e.SecretHash = nil }), true},
		{"empty lock time", mutate(func(e *SwapContractRecordedEvent) { e.LockTime = 0 }), true},
		{"empty swap time", mutate(func(e *SwapContractRecordedEvent) { e.SwapTime = 0 }), true},
		{"maker wrong status", mutate(func(e *SwapContractRecordedEvent) { e.Status = order.TakerSwapCast }), true},
		{"taker wrong status", mutate(func(e *SwapContractRecordedEvent) {
			e.Maker = false
			e.Status = order.MakerSwapCast
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
