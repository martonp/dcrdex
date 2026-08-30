// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"bytes"
	"testing"
	"time"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/meshevents"
)

func TestMarketStartedUpdateEventTxDataIncludesEpochState(t *testing.T) {
	var acct account.AccountID
	acct[0] = 7
	ord := &order.LimitOrder{
		P: order.Prefix{
			AccountID:  acct,
			BaseAsset:  42,
			QuoteAsset: 0,
			OrderType:  order.LimitOrderType,
			ClientTime: time.UnixMilli(1000),
			ServerTime: time.UnixMilli(1001),
		},
		T: order.Trade{
			Sell:     true,
			Quantity: 10,
		},
		Rate:  5,
		Force: order.StandingTiF,
	}
	update := &MarketStartedUpdate{
		Market:          "dcr_btc",
		Base:            42,
		Quote:           0,
		CurrentEpochIdx: 123,
		EpochDur:        500,
		RevocationTime:  time.UnixMilli(123456789).UTC(),
	}

	txData, err := update.EventTxData()
	if err != nil {
		t.Fatalf("EventTxData error: %v", err)
	}
	ver, pushes, err := encode.DecodeBlob(txData)
	if err != nil {
		t.Fatalf("DecodeBlob error: %v", err)
	}
	if ver != 0 {
		t.Fatalf("tx data version = %d, want 0", ver)
	}
	if len(pushes) != 8 {
		t.Fatalf("push count = %d, want 8", len(pushes))
	}
	if !bytes.Equal(pushes[3], int64Bytes(update.CurrentEpochIdx)) {
		t.Fatalf("current epoch push = %x, want %x", pushes[3], int64Bytes(update.CurrentEpochIdx))
	}
	if !bytes.Equal(pushes[4], int64Bytes(update.EpochDur)) {
		t.Fatalf("epoch duration push = %x, want %x", pushes[4], int64Bytes(update.EpochDur))
	}
	emptyEpochRevokes, err := startupOrderRevokesTxData(nil)
	if err != nil {
		t.Fatalf("startupOrderRevokesTxData(nil) error: %v", err)
	}
	if !bytes.Equal(pushes[7], emptyEpochRevokes) {
		t.Fatalf("empty epoch revokes push = %x, want %x", pushes[7], emptyEpochRevokes)
	}

	update.EpochRevokes = []*StartupOrderRevoke{{
		Order:  ord,
		Reason: meshevents.StartupOrderRevokeReasonEpochAbandoned,
	}}
	txData, err = update.EventTxData()
	if err != nil {
		t.Fatalf("EventTxData with epoch revokes error: %v", err)
	}
	ver, pushes, err = encode.DecodeBlob(txData)
	if err != nil {
		t.Fatalf("DecodeBlob error: %v", err)
	}
	if ver != 0 || len(pushes) != 8 {
		t.Fatalf("tx data version/pushes = %d/%d, want 0/8", ver, len(pushes))
	}
	wantEpochRevokes, err := startupOrderRevokesTxData(update.EpochRevokes)
	if err != nil {
		t.Fatalf("startupOrderRevokesTxData error: %v", err)
	}
	if !bytes.Equal(pushes[7], wantEpochRevokes) {
		t.Fatalf("epoch revokes push = %x, want %x", pushes[7], wantEpochRevokes)
	}
}

func TestSuspendedCancelUpdateEventTxDataRequiresCompletedMatch(t *testing.T) {
	var acct account.AccountID
	acct[0] = 1
	const base, quote uint32 = 42, 0

	target := &order.LimitOrder{
		P: order.Prefix{
			AccountID:  acct,
			BaseAsset:  base,
			QuoteAsset: quote,
			OrderType:  order.LimitOrderType,
			ClientTime: time.UnixMilli(1000),
			ServerTime: time.UnixMilli(1001),
		},
		T: order.Trade{
			Sell:     true,
			Quantity: 10,
		},
		Rate:  5,
		Force: order.StandingTiF,
	}
	cancel := &order.CancelOrder{
		P: order.Prefix{
			AccountID:  acct,
			BaseAsset:  base,
			QuoteAsset: quote,
			OrderType:  order.CancelOrderType,
			ClientTime: time.UnixMilli(2000),
			ServerTime: time.UnixMilli(2001),
		},
		TargetOrderID: target.ID(),
	}
	match := &order.Match{
		Taker:    cancel,
		Maker:    target,
		Quantity: target.Remaining(),
		Rate:     target.Rate,
		Epoch:    order.EpochID{Idx: 123, Dur: 500},
	}
	update := &SuspendedCancelUpdate{
		Market:          "dcr_btc",
		Base:            base,
		Quote:           quote,
		Cancel:          cancel,
		TargetOrderID:   target.ID(),
		TargetAccount:   acct,
		TargetSell:      target.Sell,
		EpochIdx:        123,
		EpochDur:        500,
		MatchServerTime: time.UnixMilli(3000),
		Match:           match,
	}

	if _, err := update.EventTxData(); err == nil {
		t.Fatalf("EventTxData succeeded for non-complete cancel match")
	}

	match.Status = order.MatchComplete
	txData, err := update.EventTxData()
	if err != nil {
		t.Fatalf("EventTxData error: %v", err)
	}
	_, pushes, err := encode.DecodeBlob(txData)
	if err != nil {
		t.Fatalf("DecodeBlob error: %v", err)
	}
	if len(pushes) == 0 {
		t.Fatalf("no tx data pushes")
	}
	if !bytes.Equal(pushes[len(pushes)-1], []byte{byte(order.MatchComplete)}) {
		t.Fatalf("status push = %x, want %x", pushes[len(pushes)-1], []byte{byte(order.MatchComplete)})
	}
}
