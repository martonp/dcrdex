// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"testing"

	"decred.org/dcrdex/dex/order"
)

func TestMarketLifecycleEventEncodeDecode(t *testing.T) {
	persist := true
	revoked := testLimitOrder(5, order.Preimage{})

	event := NewMarketLifecycleEvent(LifecycleActionSuspend, "dcr_btc", 100, 6000)
	event.PersistBook = &persist
	event.Timestamp = 123456789001
	event.ResumeRevokes = []StartupOrderRevokeRecord{
		NewStartupOrderRevokeRecord(revoked, StartupOrderRevokeReasonFundingCoinSpent),
	}

	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	got, err := DecodeMarketLifecycleEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	// The wire round trip must be byte-identical.
	rePayload, err := got.Encode()
	if err != nil {
		t.Fatalf("re-Encode error: %v", err)
	}
	if string(payload) != string(rePayload) {
		t.Fatalf("round trip payload mismatch:\n%s\n%s", payload, rePayload)
	}
	if got.Action != LifecycleActionSuspend || got.Market != "dcr_btc" ||
		got.EpochIdx != 100 || got.EpochDur != 6000 || got.Timestamp != 123456789001 ||
		got.PersistBook == nil || !*got.PersistBook ||
		len(got.ResumeRevokes) != 1 || got.ResumeRevokes[0].Reason != StartupOrderRevokeReasonFundingCoinSpent {
		t.Fatalf("unexpected round trip: %+v", got)
	}
	if got.Kind() != EventKindMarketLifecycle {
		t.Fatalf("kind = %q, want %q", got.Kind(), EventKindMarketLifecycle)
	}

	// The minimal scheduling form omits the empty optional fields.
	minimal := NewMarketLifecycleEvent(LifecycleActionScheduleResume, "dcr_btc", 100, 6000)
	payload, err = minimal.Encode()
	if err != nil {
		t.Fatalf("Encode minimal error: %v", err)
	}
	const wantMinimal = `{"action":"schedule_resume","market":"dcr_btc","epochIdx":100,"epochDur":6000}`
	if string(payload) != wantMinimal {
		t.Fatalf("minimal payload = %s, want %s", payload, wantMinimal)
	}
}

func TestMarketLifecycleEventValidate(t *testing.T) {
	valid := func(action string) *MarketLifecycleEvent {
		event := NewMarketLifecycleEvent(action, "dcr_btc", 100, 6000)
		event.Timestamp = 123456789001
		if action == LifecycleActionResume {
			runParams := testRunParams()
			event.RunParams = &runParams
		}
		return event
	}
	for _, action := range []string{LifecycleActionScheduleSuspend, LifecycleActionSuspend,
		LifecycleActionScheduleResume, LifecycleActionResume} {
		if err := valid(action).Validate(); err != nil {
			t.Fatalf("Validate %s error: %v", action, err)
		}
	}
	// The scheduling actions carry no revocation timestamp.
	if err := NewMarketLifecycleEvent(LifecycleActionScheduleSuspend, "dcr_btc", 100, 6000).Validate(); err != nil {
		t.Fatalf("Validate schedule_suspend without timestamp error: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*MarketLifecycleEvent)
	}{
		{"missing market", func(e *MarketLifecycleEvent) { e.Market = "" }},
		{"unknown action", func(e *MarketLifecycleEvent) { e.Action = "explode" }},
		{"empty action", func(e *MarketLifecycleEvent) { e.Action = "" }},
		{"missing epoch", func(e *MarketLifecycleEvent) { e.EpochIdx = 0 }},
		{"negative epoch", func(e *MarketLifecycleEvent) { e.EpochIdx = -1 }},
		{"missing epoch duration", func(e *MarketLifecycleEvent) { e.EpochDur = 0 }},
		{"resume missing timestamp", func(e *MarketLifecycleEvent) { e.Timestamp = 0 }},
		{"suspend missing timestamp", func(e *MarketLifecycleEvent) {
			e.Action = LifecycleActionSuspend
			e.Timestamp = 0
		}},
		{"resume missing run params", func(e *MarketLifecycleEvent) { e.RunParams = nil }},
		{"resume invalid run params", func(e *MarketLifecycleEvent) { e.RunParams.LotSize = 0 }},
		{"suspend must not carry run params", func(e *MarketLifecycleEvent) {
			e.Action = LifecycleActionSuspend
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := valid(LifecycleActionResume)
			tt.mutate(event)
			if err := event.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
			payload, err := event.Encode()
			if err != nil {
				t.Fatalf("Encode error: %v", err)
			}
			if _, err := DecodeMarketLifecycleEvent(payload); err == nil {
				t.Fatalf("expected decode validation error")
			}
		})
	}
}

func TestValidLifecycleAction(t *testing.T) {
	for _, a := range []string{LifecycleActionScheduleSuspend, LifecycleActionSuspend,
		LifecycleActionScheduleResume, LifecycleActionResume} {
		if !ValidLifecycleAction(a) {
			t.Fatalf("action %q should be valid", a)
		}
	}
	if ValidLifecycleAction("nonsense") {
		t.Fatalf("unknown action should be invalid")
	}
}
