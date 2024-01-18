// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"context"
	"encoding/hex"
	"testing"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/mm/libxc"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
)

func TestArbMarketMakerRebalance(t *testing.T) {
	const rateStep uint64 = 1e3
	const lotSize uint64 = 50e8
	const newEpoch = 123_456_789
	const driftTolerance = 0.001
	const profit = 0.01

	buyFees := &orderFees{
		swap:       1e4,
		redemption: 2e4,
		funding:    3e4,
	}
	sellFees := &orderFees{
		swap:       2e4,
		redemption: 1e4,
		funding:    4e4,
	}

	orderIDs := make([]order.OrderID, 5)
	for i := range orderIDs {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	mkt := &core.Market{
		RateStep:    rateStep,
		AtomToConv:  1,
		LotSize:     lotSize,
		BaseID:      42,
		QuoteID:     0,
		BaseSymbol:  "dcr",
		QuoteSymbol: "btc",
	}

	cfg1 := &ArbMarketMakerConfig{
		DriftTolerance: driftTolerance,
		Profit:         profit,
		BuyPlacements: []*ArbMarketMakingPlacement{{
			Lots:       1,
			Multiplier: 1.5,
		}},
		SellPlacements: []*ArbMarketMakingPlacement{{
			Lots:       1,
			Multiplier: 1.5,
		}},
	}

	cfg2 := &ArbMarketMakerConfig{
		DriftTolerance: driftTolerance,
		Profit:         profit,
		BuyPlacements: []*ArbMarketMakingPlacement{
			{
				Lots:       1,
				Multiplier: 2,
			},
			{
				Lots:       1,
				Multiplier: 1.5,
			},
		},
		SellPlacements: []*ArbMarketMakingPlacement{
			{
				Lots:       1,
				Multiplier: 2,
			},
			{
				Lots:       1,
				Multiplier: 1.5,
			},
		},
	}

	type test struct {
		name string

		buyVWAP      map[uint64]*vwapResult
		sellVWAP     map[uint64]*vwapResult
		groupedBuys  map[uint64][]*core.Order
		groupedSells map[uint64][]*core.Order
		cfg          *ArbMarketMakerConfig
		dexBalances  map[uint32]*botBalance
		cexBalances  map[uint32]*botBalance
		reserves     autoRebalanceReserves

		expectedCancels []dex.Bytes
		expectedBuys    []*rateLots
		expectedSells   []*rateLots
	}

	multiplyRate := func(u uint64, m float64) uint64 {
		return steppedRate(uint64(float64(u)*m), mkt.RateStep)
	}
	divideRate := func(u uint64, d float64) uint64 {
		return steppedRate(uint64(float64(u)/d), mkt.RateStep)
	}
	lotSizeMultiplier := func(m float64) uint64 {
		return uint64(float64(mkt.LotSize) * m)
	}

	tests := []*test{
		//  "no existing orders"
		{
			name: "no existing orders",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2e6,
					extrema: 1.9e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
			},
			cfg: cfg1,
			dexBalances: map[uint32]*botBalance{
				42: {Available: lotSize * 3},
				0:  {Available: calc.BaseToQuote(1e6, 3*lotSize)},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedBuys: []*rateLots{{
				rate: 1.881e6,
				lots: 1,
			}},
			expectedSells: []*rateLots{{
				rate: 2.222e6,
				lots: 1,
			}},
		},
		// "existing orders within drift tolerance"
		{
			name: "existing orders within drift tolerance",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2e6,
					extrema: 1.9e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
			},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					Rate: 1.882e6,
					Qty:  lotSize,
				}},
			},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					Rate: 2.223e6,
					Qty:  lotSize,
				}},
			},
			cfg: cfg1,
			dexBalances: map[uint32]*botBalance{
				42: {Available: lotSize * 3},
				0:  {Available: calc.BaseToQuote(1e6, 3*lotSize)},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
		},
		// "existing orders outside drift tolerance"
		{
			name: "existing orders outside drift tolerance",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2e6,
					extrema: 1.9e6,
				},
			},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					ID:   orderIDs[0][:],
					Rate: 1.883e6,
					Qty:  lotSize,
				}},
			},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:   orderIDs[1][:],
					Rate: 2.225e6,
					Qty:  lotSize,
				}},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
			},
			cfg: cfg1,
			dexBalances: map[uint32]*botBalance{
				42: {Available: lotSize * 3},
				0:  {Available: calc.BaseToQuote(1e6, 3*lotSize)},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedCancels: []dex.Bytes{
				orderIDs[0][:],
				orderIDs[1][:],
			},
		},
		// "don't cancel before free cancel"
		{
			name: "don't cancel before free cancel",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2e6,
					extrema: 1.9e6,
				},
			},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					ID:    orderIDs[0][:],
					Rate:  1.883e6,
					Qty:   lotSize,
					Epoch: newEpoch - 1,
				}},
			},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:    orderIDs[1][:],
					Rate:  2.225e6,
					Qty:   lotSize,
					Epoch: newEpoch - 2,
				}},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(1.5): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
			},
			cfg: cfg1,
			dexBalances: map[uint32]*botBalance{
				42: {Available: lotSize * 3},
				0:  {Available: calc.BaseToQuote(1e6, 3*lotSize)},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedCancels: []dex.Bytes{
				orderIDs[1][:],
			},
		},
		// "no existing orders, two orders each, dex balance edge, enough"
		{
			name: "no existing orders, two orders each, dex balance edge, enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 2*(lotSize+sellFees.swap) + sellFees.funding},
				0: {Available: calc.BaseToQuote(divideRate(1.9e6, 1+profit), lotSize) +
					calc.BaseToQuote(divideRate(1.7e6, 1+profit), lotSize) +
					2*buyFees.swap + buyFees.funding},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
		// "no existing orders, two orders each, dex balance edge, not enough"
		{
			name: "no existing orders, two orders each, dex balance edge, not enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 2*(lotSize+sellFees.swap) + sellFees.funding - 1},
				0: {Available: calc.BaseToQuote(divideRate(1.9e6, 1+profit), lotSize) +
					calc.BaseToQuote(divideRate(1.7e6, 1+profit), lotSize) +
					2*buyFees.swap + buyFees.funding - 1},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
			},
		},
		// "no existing orders, two orders each, cex balance edge, enough"
		{
			name: "no existing orders, two orders each, cex balance edge, enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize)},
				42: {Available: 2 * mkt.LotSize},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
		// "no existing orders, two orders each, cex balance edge, not enough"
		{
			name: "no existing orders, two orders each, cex balance edge, not enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize) - 1},
				42: {Available: 2*mkt.LotSize - 1},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
			},
		},
		// "one existing order, enough cex balance for second"
		{
			name: "one existing order, enough cex balance for second",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					Rate:   divideRate(1.9e6, 1+profit),
					Qty:    2 * lotSize,
					Filled: lotSize,
				}},
			},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					Rate:   multiplyRate(2.2e6, 1+profit),
					Qty:    2 * lotSize,
					Filled: lotSize,
				}},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize)},
				42: {Available: 2 * mkt.LotSize},
			},
			expectedBuys: []*rateLots{
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
		// "one existing order, not enough cex balance for second"
		{
			name: "one existing order, not enough cex balance for second",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					Rate: divideRate(1.9e6, 1+profit),
					Qty:  lotSize,
				}},
			},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					Rate: multiplyRate(2.2e6, 1+profit),
					Qty:  lotSize,
				}},
			},
			cfg: cfg2,
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize) - 1},
				42: {Available: 2*mkt.LotSize - 1},
			},
		},
		// "no existing orders, two orders each, dex balance edge with reserves, enough"
		{
			name: "no existing orders, two orders each, dex balance edge with reserves, enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			reserves: autoRebalanceReserves{
				baseDexReserves: 2 * lotSize,
			},
			dexBalances: map[uint32]*botBalance{
				42: {Available: 2*(lotSize+sellFees.swap) + sellFees.funding + 2*lotSize},
				0: {Available: calc.BaseToQuote(divideRate(1.9e6, 1+profit), lotSize) + calc.BaseToQuote(divideRate(1.7e6, 1+profit), lotSize) +
					2*buyFees.swap + buyFees.funding},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
		// "no existing orders, two orders each, dex balance edge with reserves, not enough"
		{
			name: "no existing orders, two orders each, dex balance edge with reserves, not enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			reserves: autoRebalanceReserves{
				baseDexReserves: 2 * lotSize,
			},
			dexBalances: map[uint32]*botBalance{
				42: {Available: 2*(lotSize+sellFees.swap) + sellFees.funding + 2*lotSize - 1},
				0: {Available: calc.BaseToQuote(divideRate(1.9e6, 1+profit), lotSize) +
					calc.BaseToQuote(divideRate(1.7e6, 1+profit), lotSize) +
					2*buyFees.swap + buyFees.funding},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: 1e19},
				42: {Available: 1e19},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
			},
		},
		// "no existing orders, two orders each, cex balance edge with reserves, enough"
		{
			name: "no existing orders, two orders each, cex balance edge with reserves, enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			reserves: autoRebalanceReserves{
				quoteCexReserves: lotSize,
			},
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize) + lotSize},
				42: {Available: 2 * mkt.LotSize},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
				{
					rate:           divideRate(1.7e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
		// "no existing orders, two orders each, cex balance edge with reserves, enough"
		{
			name: "no existing orders, two orders each, cex balance edge with reserves, not enough",
			buyVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2e6,
					extrema: 1.9e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     1.8e6,
					extrema: 1.7e6,
				},
			},
			sellVWAP: map[uint64]*vwapResult{
				lotSizeMultiplier(2): {
					avg:     2.1e6,
					extrema: 2.2e6,
				},
				lotSizeMultiplier(3.5): {
					avg:     2.3e6,
					extrema: 2.4e6,
				},
			},
			cfg: cfg2,
			reserves: autoRebalanceReserves{
				baseCexReserves: lotSize,
			},
			dexBalances: map[uint32]*botBalance{
				42: {Available: 1e19},
				0:  {Available: 1e19},
			},
			cexBalances: map[uint32]*botBalance{
				0:  {Available: calc.BaseToQuote(2.2e6, mkt.LotSize) + calc.BaseToQuote(2.4e6, mkt.LotSize) + lotSize - 1},
				42: {Available: 2 * mkt.LotSize},
			},
			expectedBuys: []*rateLots{
				{
					rate: divideRate(1.9e6, 1+profit),
					lots: 1,
				},
			},
			expectedSells: []*rateLots{
				{
					rate: multiplyRate(2.2e6, 1+profit),
					lots: 1,
				},
				{
					rate:           multiplyRate(2.4e6, 1+profit),
					lots:           1,
					placementIndex: 1,
				},
			},
		},
	}

	for _, test := range tests {
		tCore := newTCore()
		coreAdaptor := newTBotCoreAdaptor(tCore)
		coreAdaptor.balances = test.dexBalances
		coreAdaptor.buyFees = buyFees
		coreAdaptor.sellFees = sellFees
		coreAdaptor.groupedBuys = test.groupedBuys
		coreAdaptor.groupedSells = test.groupedSells

		cex := newTBotCEXAdaptor()
		cex.balances = test.cexBalances
		cex.bidsVWAP = test.buyVWAP
		cex.asksVWAP = test.sellVWAP

		arbMM := &arbMarketMaker{
			core:     coreAdaptor,
			cex:      cex,
			log:      tLogger,
			cfg:      test.cfg,
			reserves: test.reserves,
			mkt:      mkt,
		}
		arbMM.currEpoch.Store(newEpoch)

		cancels, buys, sells := arbMM.ordersToPlace()

		if len(cancels) != len(test.expectedCancels) {
			t.Fatalf("%s: expected %d cancels, got %d", test.name, len(test.expectedCancels), len(cancels))
		}
		for i := range cancels {
			if !cancels[i].Equal(test.expectedCancels[i]) {
				t.Fatalf("%s: cancel %d expected %x, got %x", test.name, i, test.expectedCancels[i], cancels[i])
			}
		}

		if len(buys) != len(test.expectedBuys) {
			t.Fatalf("%s: expected %d buys, got %d", test.name, len(test.expectedBuys), len(buys))
		}
		for i := range buys {
			if buys[i].rate != test.expectedBuys[i].rate {
				t.Fatalf("%s: buy %d expected rate %d, got %d", test.name, i, test.expectedBuys[i].rate, buys[i].rate)
			}
			if buys[i].lots != test.expectedBuys[i].lots {
				t.Fatalf("%s: buy %d expected lots %d, got %d", test.name, i, test.expectedBuys[i].lots, buys[i].lots)
			}
			if buys[i].placementIndex != test.expectedBuys[i].placementIndex {
				t.Fatalf("%s: buy %d expected placement index %d, got %d", test.name, i, test.expectedBuys[i].placementIndex, buys[i].placementIndex)
			}
		}

		if len(sells) != len(test.expectedSells) {
			t.Fatalf("%s: expected %d sells, got %d", test.name, len(test.expectedSells), len(sells))
		}
		for i := range sells {
			if sells[i].rate != test.expectedSells[i].rate {
				t.Fatalf("%s: sell %d expected rate %d, got %d", test.name, i, test.expectedSells[i].rate, sells[i].rate)
			}
			if sells[i].lots != test.expectedSells[i].lots {
				t.Fatalf("%s: sell %d expected lots %d, got %d", test.name, i, test.expectedSells[i].lots, sells[i].lots)
			}
			if sells[i].placementIndex != test.expectedSells[i].placementIndex {
				t.Fatalf("%s: sell %d expected placement index %d, got %d", test.name, i, test.expectedSells[i].placementIndex, sells[i].placementIndex)
			}
		}
	}
}

func TestArbMarketMakerDEXUpdates(t *testing.T) {
	const lotSize uint64 = 50e8
	const profit float64 = 0.01

	orderIDs := make([]order.OrderID, 5)
	for i := 0; i < 5; i++ {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	matchIDs := make([]order.MatchID, 5)
	for i := 0; i < 5; i++ {
		copy(matchIDs[i][:], encode.RandomBytes(32))
	}

	mkt := &core.Market{
		RateStep:    1e3,
		AtomToConv:  1,
		LotSize:     lotSize,
		BaseID:      42,
		QuoteID:     0,
		BaseSymbol:  "dcr",
		QuoteSymbol: "btc",
	}

	multiplyRate := func(u uint64, m float64) uint64 {
		return steppedRate(uint64(float64(u)*m), mkt.RateStep)
	}
	divideRate := func(u uint64, d float64) uint64 {
		return steppedRate(uint64(float64(u)/d), mkt.RateStep)
	}

	type test struct {
		name              string
		pendingOrderIDs   map[order.OrderID]bool
		orderUpdates      []*core.Order
		expectedCEXTrades []*libxc.Trade
	}

	tests := []*test{
		{
			name: "one buy and one sell match, repeated",
			pendingOrderIDs: map[order.OrderID]bool{
				orderIDs[0]: true,
				orderIDs[1]: true,
			},
			orderUpdates: []*core.Order{
				{
					ID:   orderIDs[0][:],
					Sell: true,
					Qty:  lotSize,
					Rate: 8e5,
					Matches: []*core.Match{
						{
							MatchID: matchIDs[0][:],
							Qty:     lotSize,
							Rate:    8e5,
						},
					},
				},
				{
					ID:   orderIDs[1][:],
					Sell: false,
					Qty:  lotSize,
					Rate: 6e5,
					Matches: []*core.Match{
						{
							MatchID: matchIDs[1][:],
							Qty:     lotSize,
							Rate:    6e5,
						},
					},
				},
				{
					ID:   orderIDs[0][:],
					Sell: true,
					Qty:  lotSize,
					Rate: 8e5,
					Matches: []*core.Match{
						{
							MatchID: matchIDs[0][:],
							Qty:     lotSize,
							Rate:    8e5,
						},
					},
				},
				{
					ID:   orderIDs[1][:],
					Sell: false,
					Qty:  lotSize,
					Rate: 6e5,
					Matches: []*core.Match{
						{
							MatchID: matchIDs[1][:],
							Qty:     lotSize,
							Rate:    6e5,
						},
					},
				},
			},
			expectedCEXTrades: []*libxc.Trade{
				{
					BaseID:  42,
					QuoteID: 0,
					Qty:     lotSize,
					Rate:    divideRate(8e5, 1+profit),
					Sell:    false,
				},
				{
					BaseID:  42,
					QuoteID: 0,
					Qty:     lotSize,
					Rate:    multiplyRate(6e5, 1+profit),
					Sell:    true,
				},
				nil,
				nil,
			},
		},
	}

	runTest := func(test *test) {
		cex := newTBotCEXAdaptor()
		tCore := newTCore()
		coreAdaptor := newTBotCoreAdaptor(tCore)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		arbMM := &arbMarketMaker{
			cex:         cex,
			core:        coreAdaptor,
			ctx:         ctx,
			baseID:      42,
			quoteID:     0,
			matchesSeen: make(map[order.MatchID]bool),
			cexTrades:   make(map[string]uint64),
			mkt:         mkt,
			cfg: &ArbMarketMakerConfig{
				Profit: profit,
			},
			pendingOrders: test.pendingOrderIDs,
		}
		arbMM.currEpoch.Store(123)
		go arbMM.run()

		for i, note := range test.orderUpdates {
			cex.lastTrade = nil

			coreAdaptor.orderUpdates <- note
			coreAdaptor.orderUpdates <- &core.Order{} // Dummy update should have no effect

			expectedCEXTrade := test.expectedCEXTrades[i]
			if (expectedCEXTrade == nil) != (cex.lastTrade == nil) {
				t.Fatalf("%s: expected cex order after update %d %v but got %v", test.name, i, (expectedCEXTrade != nil), (cex.lastTrade != nil))
			}

			if cex.lastTrade != nil &&
				*cex.lastTrade != *expectedCEXTrade {
				t.Fatalf("%s: cex order %+v != expected %+v", test.name, cex.lastTrade, expectedCEXTrade)
			}
		}
	}

	for _, test := range tests {
		runTest(test)
	}
}

func TestArbMarketMakerAutoRebalance(t *testing.T) {
	orderIDs := make([]order.OrderID, 5)
	for i := 0; i < 5; i++ {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	matchIDs := make([]order.MatchID, 5)
	for i := 0; i < 5; i++ {
		copy(matchIDs[i][:], encode.RandomBytes(32))
	}

	mkt := &core.Market{
		LotSize: 4e8,
	}

	baseID, quoteID := uint32(42), uint32(0)

	profitRate := float64(0.01)

	type test struct {
		name            string
		cfg             *AutoRebalanceConfig
		cexBaseBalance  *botBalance
		cexQuoteBalance *botBalance
		dexBaseBalance  *botBalance
		dexQuoteBalance *botBalance
		activeCEXOrders bool
		groupedBuys     map[uint64][]*core.Order
		groupedSells    map[uint64][]*core.Order

		expectedDeposit      *withdrawArgs
		expectedWithdraw     *withdrawArgs
		expectedCancels      []dex.Bytes
		expectedReserves     autoRebalanceReserves
		expectedBasePending  bool
		expectedQuotePending bool
	}

	currEpoch := uint64(123)

	tests := []*test{
		// "no orders, no need to rebalance"
		{
			name: "no orders, no need to rebalance",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  1e16,
				MinQuoteAmt: 1e12,
			},
			cexBaseBalance: &botBalance{
				Available: 1e16,
			},
			cexQuoteBalance: &botBalance{
				Available: 1e12,
			},
			dexBaseBalance: &botBalance{
				Available: 1e16,
			},
			dexQuoteBalance: &botBalance{
				Available: 1e12,
			},
		},
		//  "no action with active cex orders"
		{
			name: "no action with active cex orders",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       4 * mkt.LotSize,
					Rate:      6e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 3 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 5 * mkt.LotSize,
				Locked:    8 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			activeCEXOrders: true,
		},
		// "no orders, need to withdraw base"
		{
			name: "no orders, need to withdraw base",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  1e16,
				MinQuoteAmt: 1e12,
			},
			cexBaseBalance: &botBalance{
				Available: 8e16,
			},
			cexQuoteBalance: &botBalance{
				Available: 1e12,
			},
			dexBaseBalance: &botBalance{
				Available: 9e15,
			},
			dexQuoteBalance: &botBalance{
				Available: 1e12,
			},
			expectedWithdraw: &withdrawArgs{
				assetID: 42,
				amt:     (9e15+8e16)/2 - 9e15,
			},
			expectedBasePending: true,
		},
		//  "need to deposit base, no need to cancel order"
		{
			name: "need to deposit base, no need to cancel order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       4 * mkt.LotSize,
					Rate:      6e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 3 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 5 * mkt.LotSize,
				Locked:    8 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			expectedDeposit: &withdrawArgs{
				assetID: 42,
				amt:     5 * mkt.LotSize,
			},
			expectedBasePending: true,
		},
		//  "need to deposit base, need to cancel 1 order"
		{
			name: "need to deposit base, need to cancel 1 order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       4 * mkt.LotSize,
					Rate:      6e7,
					Sell:      true,
					LockedAmt: 4 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 3 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 5*mkt.LotSize - 2,
				Locked:    8 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			expectedCancels: []dex.Bytes{
				orderIDs[1][:],
			},
			expectedReserves: autoRebalanceReserves{
				baseDexReserves: (16*mkt.LotSize-2)/2 - 3*mkt.LotSize,
			},
		},
		//  "need to deposit base, need to cancel 2 orders"
		{
			name: "need to deposit base, need to cancel 2 orders",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  3 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       2 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 2 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       2 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 2 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				2: {{
					ID:        orderIDs[2][:],
					Qty:       6 * mkt.LotSize,
					Filled:    4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 2 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 0,
			},
			dexBaseBalance: &botBalance{
				Available: 1000,
				Locked:    6 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			expectedCancels: []dex.Bytes{
				orderIDs[2][:],
				orderIDs[1][:],
			},
			expectedReserves: autoRebalanceReserves{
				baseDexReserves: (6*mkt.LotSize + 1000) / 2,
			},
		},
		//  "need to withdraw base, no need to cancel order"
		{
			name: "need to withdraw base, no need to cancel order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  3 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			cexBaseBalance: &botBalance{
				Available: 6 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 0,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			expectedWithdraw: &withdrawArgs{
				assetID: baseID,
				amt:     3 * mkt.LotSize,
			},
			expectedBasePending: true,
		},
		//  "need to withdraw base, need to cancel 1 order"
		{
			name: "need to withdraw base, need to cancel 1 order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  3 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedSells: map[uint64][]*core.Order{},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       2 * mkt.LotSize,
					Rate:      5e7,
					Sell:      false,
					LockedAmt: 2*mkt.LotSize + 1500,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       2 * mkt.LotSize,
					Rate:      6e7,
					Sell:      false,
					LockedAmt: 2*mkt.LotSize + 1500,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 8*mkt.LotSize - 2,
			},
			dexBaseBalance: &botBalance{
				Available: 0,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 6*mkt.LotSize),
				Locked:    4*mkt.LotSize + 3000,
			},
			expectedCancels: []dex.Bytes{
				orderIDs[1][:],
			},
			expectedReserves: autoRebalanceReserves{
				baseCexReserves: 4*mkt.LotSize - 1,
			},
		},
		//  "need to deposit quote, no need to cancel order"
		{
			name: "need to deposit quote, no need to cancel order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedSells: map[uint64][]*core.Order{},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       2 * mkt.LotSize,
					Rate:      5e7,
					Sell:      false,
					LockedAmt: calc.BaseToQuote(5e7, 2*mkt.LotSize),
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       2 * mkt.LotSize,
					Rate:      5e7,
					Sell:      false,
					LockedAmt: calc.BaseToQuote(5e7, 2*mkt.LotSize),
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 4*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 8*mkt.LotSize),
				Locked:    2 * calc.BaseToQuote(5e7, 2*mkt.LotSize),
			},
			expectedDeposit: &withdrawArgs{
				assetID: 0,
				amt:     calc.BaseToQuote(5e7, 4*mkt.LotSize),
			},
			expectedQuotePending: true,
		},
		//  "need to deposit quote, need to cancel 1 order"
		{
			name: "need to deposit quote, no need to cancel order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedSells: map[uint64][]*core.Order{},
			groupedBuys: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      false,
					LockedAmt: calc.BaseToQuote(5e7, 4*mkt.LotSize),
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       4 * mkt.LotSize,
					Rate:      5e7,
					Sell:      false,
					LockedAmt: calc.BaseToQuote(5e7, 4*mkt.LotSize),
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 4*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 2*mkt.LotSize),
				Locked:    2 * calc.BaseToQuote(5e7, 4*mkt.LotSize),
			},
			expectedCancels: []dex.Bytes{
				orderIDs[1][:],
			},
			expectedReserves: autoRebalanceReserves{
				quoteDexReserves: calc.BaseToQuote(5e7, 3*mkt.LotSize),
			},
		},
		//  "need to withdraw quote, no need to cancel order"
		{
			name: "need to withdraw quote, no need to cancel order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       3 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 3 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       3 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 3 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
				Locked:    6 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 4*mkt.LotSize) + calc.BaseToQuote(uint64(float64(5e7)*(1+profitRate)), 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 2*mkt.LotSize) + 12e6,
			},
			expectedWithdraw: &withdrawArgs{
				assetID: quoteID,
				amt:     calc.BaseToQuote(5e7, 4*mkt.LotSize),
			},
			expectedQuotePending: true,
		},
		//  "need to withdraw quote, need to cancel 1 order"
		{
			name: "need to withdraw quote, need to cancel 1 order",
			cfg: &AutoRebalanceConfig{
				MinBaseAmt:  6 * mkt.LotSize,
				MinQuoteAmt: calc.BaseToQuote(5e7, 6*mkt.LotSize),
			},
			groupedBuys: map[uint64][]*core.Order{},
			groupedSells: map[uint64][]*core.Order{
				0: {{
					ID:        orderIDs[0][:],
					Qty:       3 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 3 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
				1: {{
					ID:        orderIDs[1][:],
					Qty:       3 * mkt.LotSize,
					Rate:      5e7,
					Sell:      true,
					LockedAmt: 3 * mkt.LotSize,
					Epoch:     currEpoch - 2,
				}},
			},
			cexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
			},
			dexBaseBalance: &botBalance{
				Available: 10 * mkt.LotSize,
				Locked:    6 * mkt.LotSize,
			},
			cexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 4*mkt.LotSize) + calc.BaseToQuote(uint64(float64(5e7)*(1+profitRate)), 6*mkt.LotSize),
			},
			dexQuoteBalance: &botBalance{
				Available: calc.BaseToQuote(5e7, 2*mkt.LotSize) + 12e6 - 2,
			},
			expectedCancels: []dex.Bytes{
				orderIDs[1][:],
			},
			expectedReserves: autoRebalanceReserves{
				quoteCexReserves: calc.BaseToQuote(5e7, 4*mkt.LotSize) + 1,
			},
		},
	}

	runTest := func(test *test) {
		cex := newTBotCEXAdaptor()
		cex.balances = map[uint32]*botBalance{
			baseID:  test.cexBaseBalance,
			quoteID: test.cexQuoteBalance,
		}

		tCore := newTCore()
		coreAdaptor := newTBotCoreAdaptor(tCore)
		coreAdaptor.balances = map[uint32]*botBalance{
			baseID:  test.dexBaseBalance,
			quoteID: test.dexQuoteBalance,
		}
		coreAdaptor.groupedBuys = test.groupedBuys
		coreAdaptor.groupedSells = test.groupedSells

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mm := &arbMarketMaker{
			ctx:     ctx,
			cex:     cex,
			core:    coreAdaptor,
			baseID:  baseID,
			quoteID: quoteID,
			log:     tLogger,
			cfg: &ArbMarketMakerConfig{
				AutoRebalance: test.cfg,
				Profit:        profitRate,
			},
			mkt: mkt,
		}

		if test.activeCEXOrders {
			mm.cexTrades = map[string]uint64{"abc": 1234}
		}

		mm.rebalanceAssets()

		if (test.expectedDeposit == nil) != (cex.lastDepositArgs == nil) {
			t.Fatalf("%s: expected deposit %v but got %v", test.name, (test.expectedDeposit != nil), (cex.lastDepositArgs != nil))
		}
		if test.expectedDeposit != nil {
			if *cex.lastDepositArgs != *test.expectedDeposit {
				t.Fatalf("%s: expected deposit %+v but got %+v", test.name, test.expectedDeposit, cex.lastDepositArgs)
			}
		}

		if (test.expectedWithdraw == nil) != (cex.lastWithdrawArgs == nil) {
			t.Fatalf("%s: expected withdraw %v but got %v", test.name, (test.expectedWithdraw != nil), (cex.lastWithdrawArgs != nil))
		}
		if test.expectedWithdraw != nil {
			if *cex.lastWithdrawArgs != *test.expectedWithdraw {
				t.Fatalf("%s: expected withdraw %+v but got %+v", test.name, test.expectedWithdraw, cex.lastWithdrawArgs)
			}
		}

		if len(tCore.cancelsPlaced) != len(test.expectedCancels) {
			t.Fatalf("%s: expected %d cancels but got %d", test.name, len(test.expectedCancels), len(tCore.cancelsPlaced))
		}
		for i := range test.expectedCancels {
			if !tCore.cancelsPlaced[i].Equal(test.expectedCancels[i]) {
				t.Fatalf("%s: expected cancel %d %s but got %s", test.name, i, hex.EncodeToString(test.expectedCancels[i]), hex.EncodeToString(tCore.cancelsPlaced[i]))
			}
		}

		if test.expectedReserves != mm.reserves {
			t.Fatalf("%s: expected reserves %+v but got %+v", test.name, test.expectedReserves, mm.reserves)
		}

		if test.expectedBasePending != mm.pendingBaseRebalance.Load() {
			t.Fatalf("%s: expected pending base rebalance %v but got %v", test.name, test.expectedBasePending, mm.pendingBaseRebalance.Load())
		}
		if test.expectedQuotePending != mm.pendingQuoteRebalance.Load() {
			t.Fatalf("%s: expected pending quote rebalance %v but got %v", test.name, test.expectedQuotePending, mm.pendingQuoteRebalance.Load())
		}
	}

	for _, test := range tests {
		runTest(test)
	}
}
