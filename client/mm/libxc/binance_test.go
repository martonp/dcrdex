// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package libxc

import (
	"net/url"
	"testing"

	_ "decred.org/dcrdex/client/asset/importall"
	"decred.org/dcrdex/client/mm/libxc/bntypes"
)

func TestSubscribeTradeUpdates(t *testing.T) {
	bn := &binance{
		tradeUpdaters: make(map[int]chan *Trade),
	}
	_, unsub0, _ := bn.SubscribeTradeUpdates()
	_, _, id1 := bn.SubscribeTradeUpdates()
	unsub0()
	_, _, id2 := bn.SubscribeTradeUpdates()
	if len(bn.tradeUpdaters) != 2 {
		t.Fatalf("wrong number of updaters. wanted 2, got %d", len(bn.tradeUpdaters))
	}
	if id1 == id2 {
		t.Fatalf("ids should be unique. got %d twice", id1)
	}
	if _, found := bn.tradeUpdaters[id1]; !found {
		t.Fatalf("id1 not found")
	}
	if _, found := bn.tradeUpdaters[id2]; !found {
		t.Fatalf("id2 not found")
	}
}

func TestBinanceToDexSymbol(t *testing.T) {
	tests := map[[2]string]string{
		{"ETH", "ETH"}:     "eth",
		{"ETH", "MATIC"}:   "weth.polygon",
		{"MATIC", "MATIC"}: "polygon",
		{"USDC", "ETH"}:    "usdc.eth",
		{"USDC", "MATIC"}:  "usdc.polygon",
		{"BTC", "BTC"}:     "btc",
		{"WBTC", "ETH"}:    "wbtc.eth",
	}

	for test, expected := range tests {
		dexSymbol := binanceCoinNetworkToDexSymbol(test[0], test[1])
		if expected != dexSymbol {
			t.Fatalf("expected %s but got %v", expected, dexSymbol)
		}
	}
}

func TestBncAssetCfg(t *testing.T) {
	tests := map[uint32]*bncAssetConfig{
		0: {
			assetID:          0,
			symbol:           "btc",
			coin:             "BTC",
			chain:            "BTC",
			conversionFactor: 1e8,
		},
		2: {
			assetID:          2,
			symbol:           "ltc",
			coin:             "LTC",
			chain:            "LTC",
			conversionFactor: 1e8,
		},
		3: {
			assetID:          3,
			symbol:           "doge",
			coin:             "DOGE",
			chain:            "DOGE",
			conversionFactor: 1e8,
		},
		5: {
			assetID:          5,
			symbol:           "dash",
			coin:             "DASH",
			chain:            "DASH",
			conversionFactor: 1e8,
		},
		60: {
			assetID:          60,
			symbol:           "eth",
			coin:             "ETH",
			chain:            "ETH",
			conversionFactor: 1e9,
		},
		42: {
			assetID:          42,
			symbol:           "dcr",
			coin:             "DCR",
			chain:            "DCR",
			conversionFactor: 1e8,
		},
		966001: {
			assetID:          966001,
			symbol:           "usdc.polygon",
			coin:             "USDC",
			chain:            "MATIC",
			conversionFactor: 1e6,
		},
		966002: {
			assetID:          966002,
			symbol:           "weth.polygon",
			coin:             "ETH",
			chain:            "MATIC",
			conversionFactor: 1e9,
		},
	}

	for test, expected := range tests {
		cfg, err := bncAssetCfg(test)
		if err != nil {
			t.Fatalf("error getting asset config: %v", err)
		}
		if *expected != *cfg {
			t.Fatalf("expected %v but got %v", expected, cfg)
		}
	}
}

func TestBuildTradeRequest(t *testing.T) {
	baseCfg := &bncAssetConfig{
		assetID:          60,
		symbol:           "eth",
		coin:             "ETH",
		chain:            "ETH",
		conversionFactor: 1e9,
	}
	quoteCfg := &bncAssetConfig{
		assetID:          0,
		symbol:           "btc",
		coin:             "BTC",
		chain:            "BTC",
		conversionFactor: 1e8,
	}
	market := &bntypes.Market{
		Symbol:   "ETHBTC",
		MinPrice: 1000,   // 0.00001 ETH/BTC
		MaxPrice: 100000, // 0.001 ETH/BTC
		RateStep: 100,    // 0.000001 ETH
		MinQty:   1e8,    // 0.1 ETH
		MaxQty:   1e12,   // 1000 ETH
		LotSize:  1e7,    // 0.01 ETH
	}
	tradeID := "test123"

	tests := []struct {
		name      string
		sell      bool
		orderType OrderType
		rate      uint64
		qty       uint64
		wantErr   bool
		wantVals  url.Values
	}{
		{
			name:      "limit buy",
			sell:      false,
			orderType: OrderTypeLimit,
			rate:      50000,
			qty:       5e9,
			wantVals: url.Values{
				"symbol":           []string{"ETHBTC"},
				"side":             []string{"BUY"},
				"type":             []string{"LIMIT"},
				"timeInForce":      []string{"GTC"},
				"newClientOrderId": []string{"test123"},
				"quantity":         []string{"5.00"},
				"price":            []string{"0.0050000"},
			},
		},
		{
			name:      "limit sell",
			sell:      true,
			orderType: OrderTypeLimit,
			rate:      50000, // 0.0005 BTC/ETH
			qty:       5e9,   // 5 ETH
			wantVals: url.Values{
				"symbol":           []string{"ETHBTC"},
				"side":             []string{"SELL"},
				"type":             []string{"LIMIT"},
				"timeInForce":      []string{"GTC"},
				"newClientOrderId": []string{"test123"},
				"quantity":         []string{"5.00"},
				"price":            []string{"0.0050000"},
			},
		},
		{
			name:      "market buy",
			sell:      false,
			orderType: OrderTypeMarket,
			qty:       5e7,
			wantVals: url.Values{
				"symbol":           []string{"ETHBTC"},
				"side":             []string{"BUY"},
				"type":             []string{"MARKET"},
				"timeInForce":      []string{"IOC"},
				"newClientOrderId": []string{"test123"},
				"quoteOrderQty":    []string{"0.50000000"},
			},
		},
		{
			name:      "market sell",
			sell:      true,
			orderType: OrderTypeMarket,
			qty:       5e9,
			wantVals: url.Values{
				"symbol":           []string{"ETHBTC"},
				"side":             []string{"SELL"},
				"type":             []string{"MARKET"},
				"timeInForce":      []string{"IOC"},
				"newClientOrderId": []string{"test123"},
				"quantity":         []string{"5.00"},
			},
		},
		{
			name:      "rate too low",
			sell:      false,
			orderType: OrderTypeLimit,
			rate:      500,
			qty:       5e9,
			wantErr:   true,
		},
		{
			name:      "rate too high",
			sell:      false,
			orderType: OrderTypeLimit,
			rate:      150000,
			qty:       5e9,
			wantErr:   true,
		},
		{
			name:      "quantity too low",
			sell:      false,
			orderType: OrderTypeLimit,
			rate:      50000,
			qty:       5e7,
			wantErr:   true,
		},
		{
			name:      "quantity too high",
			sell:      false,
			orderType: OrderTypeLimit,
			rate:      50000,
			qty:       2e12,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vals, err := buildTradeRequest(baseCfg, quoteCfg, market, tt.sell, tt.orderType, tt.rate, tt.qty, tradeID)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildTradeRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(vals) != len(tt.wantVals) {
				t.Errorf("buildTradeRequest() got %d values, want %d", len(vals), len(tt.wantVals))
				return
			}
			for k, want := range tt.wantVals {
				got := vals[k]
				if len(got) != 1 || got[0] != want[0] {
					t.Errorf("buildTradeRequest() key %q = %v, want %v", k, got, want)
				}
			}
		})
	}
}
