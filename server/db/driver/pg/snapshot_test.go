// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"strings"
	"testing"

	"decred.org/dcrdex/dex"
	"github.com/lib/pq"
)

// TestLifecycleForConfiguredMarkets: lifecycle uses market names, not schema keys.
func TestLifecycleForConfiguredMarkets(t *testing.T) {
	if got := lifecycleForConfiguredMarkets(nil); got != "FALSE" {
		t.Fatalf("empty markets: got %q, want FALSE", got)
	}

	markets := map[string]*dex.MarketInfo{
		"dcrTKNbtc": {Name: "dcr.btc"},
		"btc_ltc":   {Name: "btc_ltc"},
	}
	got := lifecycleForConfiguredMarkets(markets)
	want := "market IN (" + pq.QuoteLiteral("btc_ltc") + ", " + pq.QuoteLiteral("dcr.btc") + ")"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "dcrTKNbtc") {
		t.Fatalf("clause used schema key instead of market name: %s", got)
	}
}

// TestValidateSnapshotTableSet: exact table set, no extras/missing/duplicates.
func TestValidateSnapshotTableSet(t *testing.T) {
	expected := []snapshotTable{
		{publicSchema, accountsTableName, ""},
		{publicSchema, bondsTableName, ""},
		{"mkt", ordersActiveTableName, ""},
	}
	dumps := func(keys ...string) []snapshotTableDump {
		out := make([]snapshotTableDump, len(keys))
		for i, key := range keys {
			parts := strings.SplitN(key, ".", 2)
			out[i] = snapshotTableDump{Schema: parts[0], Table: parts[1]}
		}
		return out
	}

	if err := validateSnapshotTableSet(dumps(
		"public.accounts", "public.bonds", "mkt.orders_active",
	), expected); err != nil {
		t.Fatalf("exact set: %v", err)
	}
	// Order of got must not matter.
	if err := validateSnapshotTableSet(dumps(
		"mkt.orders_active", "public.bonds", "public.accounts",
	), expected); err != nil {
		t.Fatalf("permuted set: %v", err)
	}

	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"extra", []string{"public.accounts", "public.bonds", "mkt.orders_active", "public.markets"}},
		{"missing", []string{"public.accounts", "public.bonds"}},
		{"duplicate", []string{"public.accounts", "public.bonds", "mkt.orders_active", "public.accounts"}},
	} {
		err := validateSnapshotTableSet(dumps(tt.keys...), expected)
		if err == nil || !strings.Contains(err.Error(), "snapshot table set mismatch") {
			t.Fatalf("%s: err = %v", tt.name, err)
		}
	}
}
