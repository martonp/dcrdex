// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
)

// LimitOrderToMsgjson converts a dex/order LimitOrder to a msgjson.LimitOrder.
func LimitOrderToMsgjson(lo *order.LimitOrder) *msgjson.LimitOrder {
	p := lo.Prefix()
	t := lo.Trade()
	coins := make([]*msgjson.Coin, 0, len(t.Coins))
	for _, c := range t.Coins {
		coins = append(coins, &msgjson.Coin{ID: msgjson.Bytes(c)})
	}
	tif := uint8(msgjson.ImmediateOrderNum)
	if lo.Force == order.StandingTiF {
		tif = uint8(msgjson.StandingOrderNum)
	}
	side := uint8(msgjson.BuyOrderNum)
	if t.Sell {
		side = uint8(msgjson.SellOrderNum)
	}
	return &msgjson.LimitOrder{
		Prefix: msgjson.Prefix{
			AccountID:  p.AccountID[:],
			Base:       p.BaseAsset,
			Quote:      p.QuoteAsset,
			OrderType:  msgjson.LimitOrderNum,
			ClientTime: uint64(p.ClientTime.UnixMilli()),
			ServerTime: uint64(p.ServerTime.UnixMilli()),
			Commit:     p.Commit[:],
		},
		Trade: msgjson.Trade{
			Side:     side,
			Quantity: t.Quantity,
			Coins:    coins,
			Address:  t.Address,
		},
		Rate: lo.Rate,
		TiF:  tif,
	}
}

// CancelOrderToMsgjson converts a dex/order CancelOrder to a msgjson.CancelOrder.
func CancelOrderToMsgjson(co *order.CancelOrder) *msgjson.CancelOrder {
	p := co.Prefix()
	return &msgjson.CancelOrder{
		Prefix: msgjson.Prefix{
			AccountID:  p.AccountID[:],
			Base:       p.BaseAsset,
			Quote:      p.QuoteAsset,
			OrderType:  msgjson.CancelOrderNum,
			ClientTime: uint64(p.ClientTime.UnixMilli()),
			ServerTime: uint64(p.ServerTime.UnixMilli()),
			Commit:     p.Commit[:],
		},
		TargetID: co.TargetOrderID[:],
	}
}
