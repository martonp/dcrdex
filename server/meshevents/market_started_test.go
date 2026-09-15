// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"fmt"
	"testing"
	"time"

	"decred.org/dcrdex/dex/order"
)

func testRunParams() MarketRunParams {
	return MarketRunParams{
		LotSize:                100_000,
		RateStep:               1000,
		ParcelSize:             10,
		MaxUserCancelsPerEpoch: 2,
		MinimumRate:            500,
	}
}

func TestMarketStartedEventEncodeDecode(t *testing.T) {
	ord := testLimitOrder(5, order.Preimage{})
	revocationTime := time.UnixMilli(123456789).UTC()

	event := NewMarketStartedEvent("dcr_btc", 42, 6000, testRunParams(), revocationTime,
		[]StartupOrderRevokeRecord{NewStartupOrderRevokeRecord(ord, StartupOrderRevokeReasonLotSizeIncompatible)})
	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	// The wire encoding is consensus-critical and must not change.
	wantJSON := fmt.Sprintf(`{"market":"dcr_btc","currentEpochIdx":42,"epochDur":6000,`+
		`"runParams":{"lotSize":100000,"rateStep":1000,"parcelSize":10,"maxUserCancels":2,"minimumRate":500},`+
		`"revocationTime":123456789,"bookedRevokes":[{"order":"%x","reason":1}]}`, order.EncodeOrder(ord))
	if string(payload) != wantJSON {
		t.Fatalf("payload = %s, want %s", payload, wantJSON)
	}
	got, err := DecodeMarketStartedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if got.Market != "dcr_btc" || got.CurrentEpochIdx != 42 || got.EpochDur != 6000 ||
		got.RunParams != testRunParams() ||
		got.RevocationTime != revocationTime.UnixMilli() || len(got.BookedRevokes) != 1 ||
		got.BookedRevokes[0].Reason != StartupOrderRevokeReasonLotSizeIncompatible {
		t.Fatalf("unexpected round trip: %+v", got)
	}
	if got.Kind() != EventKindMarketStarted {
		t.Fatalf("kind = %q, want %q", got.Kind(), EventKindMarketStarted)
	}

	// Empty booked-revokes list is encoded, not null.
	empty := NewMarketStartedEvent("dcr_btc", 42, 6000, testRunParams(), revocationTime, nil)
	payload, err = empty.Encode()
	if err != nil {
		t.Fatalf("Encode empty error: %v", err)
	}
	const wantEmpty = `{"market":"dcr_btc","currentEpochIdx":42,"epochDur":6000,` +
		`"runParams":{"lotSize":100000,"rateStep":1000,"parcelSize":10,"maxUserCancels":2,"minimumRate":500},` +
		`"revocationTime":123456789,"bookedRevokes":[]}`
	if string(payload) != wantEmpty {
		t.Fatalf("empty payload = %s, want %s", payload, wantEmpty)
	}
}

func TestMarketStartedEventValidate(t *testing.T) {
	base := func() *MarketStartedEvent {
		return NewMarketStartedEvent("dcr_btc", 42, 6000, testRunParams(), time.UnixMilli(123456789), nil)
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	// A zero rate floor and a zero cancel cap are real configurations.
	zeroOK := base()
	zeroOK.RunParams.MinimumRate = 0
	zeroOK.RunParams.MaxUserCancelsPerEpoch = 0
	if err := zeroOK.Validate(); err != nil {
		t.Fatalf("Validate with zero rate floor and cancel cap: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*MarketStartedEvent)
	}{
		{"missing market", func(e *MarketStartedEvent) { e.Market = "" }},
		{"missing revocation time", func(e *MarketStartedEvent) { e.RevocationTime = 0 }},
		{"negative current epoch", func(e *MarketStartedEvent) { e.CurrentEpochIdx = -1 }},
		{"zero current epoch", func(e *MarketStartedEvent) { e.CurrentEpochIdx = 0 }},
		{"zero duration", func(e *MarketStartedEvent) { e.EpochDur = 0 }},
		{"zero lot size", func(e *MarketStartedEvent) { e.RunParams.LotSize = 0 }},
		{"zero rate step", func(e *MarketStartedEvent) { e.RunParams.RateStep = 0 }},
		{"zero parcel size", func(e *MarketStartedEvent) { e.RunParams.ParcelSize = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := base()
			tt.mutate(event)
			if err := event.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
			payload, err := event.Encode()
			if err != nil {
				t.Fatalf("Encode error: %v", err)
			}
			if _, err := DecodeMarketStartedEvent(payload); err == nil {
				t.Fatalf("expected decode validation error")
			}
		})
	}
}

func TestValidStartupOrderRevokeReason(t *testing.T) {
	for _, r := range []StartupOrderRevokeReason{
		StartupOrderRevokeReasonLotSizeIncompatible,
		StartupOrderRevokeReasonFundingCoinSpent,
		StartupOrderRevokeReasonAccountLowBalance,
		StartupOrderRevokeReasonEpochAbandoned,
	} {
		if !ValidStartupOrderRevokeReason(r) {
			t.Fatalf("reason %d should be valid", r)
		}
	}
	for _, r := range []StartupOrderRevokeReason{StartupOrderRevokeReasonInvalid, 5, 200} {
		if ValidStartupOrderRevokeReason(r) {
			t.Fatalf("reason %d should be invalid", r)
		}
	}
}
