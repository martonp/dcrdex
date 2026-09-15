// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"fmt"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
)

func TestOrdersRevokedEventEncodeDecode(t *testing.T) {
	var oid order.OrderID
	oid[0] = 0xab
	revokeTime := time.UnixMilli(123456789000).UTC()

	event := NewOrdersRevokedForOrdersEvent("dcr_btc", []order.OrderID{oid},
		OrderRevokeReasonFundingSpent, revokeTime)
	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	// The wire encoding is consensus-critical and must not change.
	wantJSON := fmt.Sprintf(`{"market":"dcr_btc","orders":["%x"],"reason":1,"revokeTime":123456789000}`, oid[:])
	if string(payload) != wantJSON {
		t.Fatalf("payload = %s, want %s", payload, wantJSON)
	}
	got, err := DecodeOrdersRevokedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if got.Market != "dcr_btc" || len(got.OrderIDs) != 1 || got.Reason != OrderRevokeReasonFundingSpent ||
		got.RevokeTime != revokeTime.UnixMilli() {
		t.Fatalf("unexpected round trip: %+v", got)
	}
	if got.Kind() != EventKindOrdersRevoked {
		t.Fatalf("kind = %q, want %q", got.Kind(), EventKindOrdersRevoked)
	}

	var user account.AccountID
	user[0] = 0xcd
	userEvent := NewOrdersRevokedForUserEvent(user, OrderRevokeReasonDisconnected, revokeTime)
	payload, err = userEvent.Encode()
	if err != nil {
		t.Fatalf("Encode user form error: %v", err)
	}
	wantJSON = fmt.Sprintf(`{"user":"%x","reason":2,"revokeTime":123456789000}`, user[:])
	if string(payload) != wantJSON {
		t.Fatalf("user form payload = %s, want %s", payload, wantJSON)
	}
	if _, err := DecodeOrdersRevokedEvent(payload); err != nil {
		t.Fatalf("Decode user form error: %v", err)
	}
}

func TestOrdersRevokedEventValidate(t *testing.T) {
	var oid order.OrderID
	var user account.AccountID
	revokeTime := time.UnixMilli(1000)
	orderForm := func() *OrdersRevokedEvent {
		return NewOrdersRevokedForOrdersEvent("dcr_btc", []order.OrderID{oid}, OrderRevokeReasonAdmin, revokeTime)
	}
	userForm := func() *OrdersRevokedEvent {
		return NewOrdersRevokedForUserEvent(user, OrderRevokeReasonPenalty, revokeTime)
	}
	if err := orderForm().Validate(); err != nil {
		t.Fatalf("order form Validate error: %v", err)
	}
	if err := userForm().Validate(); err != nil {
		t.Fatalf("user form Validate error: %v", err)
	}

	tests := []struct {
		name  string
		event *OrdersRevokedEvent
	}{
		{"invalid reason", func() *OrdersRevokedEvent { e := orderForm(); e.Reason = OrderRevokeReasonInvalid; return e }()},
		{"unknown reason", func() *OrdersRevokedEvent { e := orderForm(); e.Reason = 200; return e }()},
		{"missing revoke time", func() *OrdersRevokedEvent { e := orderForm(); e.RevokeTime = 0; return e }()},
		{"both forms set", func() *OrdersRevokedEvent { e := orderForm(); e.User = user[:]; e.Market = ""; return e }()},
		{"neither form set", func() *OrdersRevokedEvent { e := orderForm(); e.OrderIDs = nil; return e }()},
		{"short user ID", func() *OrdersRevokedEvent { e := userForm(); e.User = e.User[:8]; return e }()},
		{"user form with market", func() *OrdersRevokedEvent { e := userForm(); e.Market = "dcr_btc"; return e }()},
		{"order form without market", func() *OrdersRevokedEvent { e := orderForm(); e.Market = ""; return e }()},
		{"short order ID", func() *OrdersRevokedEvent {
			e := orderForm()
			e.OrderIDs = []dex.Bytes{oid[:8]}
			return e
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
			payload, err := tt.event.Encode()
			if err != nil {
				t.Fatalf("Encode error: %v", err)
			}
			if _, err := DecodeOrdersRevokedEvent(payload); err == nil {
				t.Fatalf("expected decode validation error")
			}
		})
	}
}
