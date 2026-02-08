package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func signMsg(priv *secp256k1.PrivateKey, msg []byte) []byte {
	h := sha256.Sum256(msg)
	return ecdsa.Sign(priv, h[:]).Serialize()
}

func mustPrivKey(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	return priv
}

func mk32(b byte) []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = b
	}
	return s
}

func mkLimitOrder(base, quote uint32, serverTime uint64, sell bool, rate, qty uint64) *msgjson.LimitOrder {
	side := uint8(msgjson.BuyOrderNum)
	if sell {
		side = uint8(msgjson.SellOrderNum)
	}
	return &msgjson.LimitOrder{
		Prefix: msgjson.Prefix{
			AccountID:  mk32(0x01),
			Base:       base,
			Quote:      quote,
			OrderType:  msgjson.LimitOrderNum,
			ClientTime: serverTime - 1,
			ServerTime: serverTime,
			Commit:     mk32(0x02),
		},
		Trade: msgjson.Trade{
			Side:     side,
			Quantity: qty,
			Coins:    []*msgjson.Coin{},
			Address:  "",
		},
		Rate: rate,
		TiF:  uint8(msgjson.StandingOrderNum),
	}
}

func mkCancel(base, quote uint32, serverTime uint64, targetID []byte) *msgjson.CancelOrder {
	return &msgjson.CancelOrder{
		Prefix: msgjson.Prefix{
			AccountID:  mk32(0x03),
			Base:       base,
			Quote:      quote,
			OrderType:  msgjson.CancelOrderNum,
			ClientTime: serverTime - 1,
			ServerTime: serverTime,
			Commit:     mk32(0x04),
		},
		TargetID: targetID,
	}
}

func mkRevoke(orderID []byte, time uint64) *msgjson.RevokeOrder {
	return &msgjson.RevokeOrder{
		OrderID: orderID,
		Time:    time,
	}
}

func mkMatch(orderID []byte, matchID []byte, serverTime uint64, qty, rate uint64) *msgjson.Match {
	return &msgjson.Match{
		OrderID:    orderID,
		MatchID:    matchID,
		Quantity:   qty,
		Rate:       rate,
		ServerTime: serverTime,
		Address:    "",
	}
}

type e2eExpect struct {
	runErrSubstr   string
	proofValid     bool
	proofErrSubstr string
	checkReport    func(t *testing.T, rep *CoverageReport)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func runVerifyE2E(t *testing.T, priv *secp256k1.PrivateKey, proof *core.MarketMakingProof, startMs, endMs, epochDurMs uint64, qty float64, maxSpreadPct float64, expect e2eExpect) {
	t.Helper()

	tmpDir := t.TempDir()
	proofPath := filepath.Join(tmpDir, "proof.json")
	outPath := filepath.Join(tmpDir, "report.json")
	writeJSON(t, proofPath, proof)

	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	prevArgs := os.Args
	defer func() { os.Args = prevArgs }()

	os.Args = []string{
		"verifymm",
		"-proof", proofPath,
		"-pubkey", pubHex,
		"-start", strconv.FormatUint(startMs, 10),
		"-end", strconv.FormatUint(endMs, 10),
		"-epochdur", strconv.FormatUint(epochDurMs, 10),
		"-qty", strconv.FormatFloat(qty, 'f', -1, 64),
		"-out", outPath,
	}
	if maxSpreadPct != 0 {
		os.Args = append(os.Args, "-maxspreadpct", strconv.FormatFloat(maxSpreadPct, 'f', -1, 64))
	}

	err := runVerify()
	if expect.runErrSubstr != "" {
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(expect.runErrSubstr)) {
			t.Fatalf("expected runVerify error containing %q, got %v", expect.runErrSubstr, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected runVerify error: %v", err)
	}

	// For non-error runs, verify output file.
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("failed to read output report: %v", err)
	}
	var rep CoverageReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("failed to parse output report: %v", err)
	}

	if rep.ProofValid != expect.proofValid {
		t.Fatalf("expected ProofValid=%t, got %t (ProofError=%q)", expect.proofValid, rep.ProofValid, rep.ProofError)
	}
	if expect.proofErrSubstr != "" && !bytes.Contains([]byte(rep.ProofError), []byte(expect.proofErrSubstr)) {
		t.Fatalf("expected ProofError containing %q, got %q", expect.proofErrSubstr, rep.ProofError)
	}
	if expect.proofErrSubstr == "" && rep.ProofValid && rep.ProofError != "" {
		t.Fatalf("expected empty ProofError for valid proof, got %q", rep.ProofError)
	}
	if expect.checkReport != nil {
		expect.checkReport(t, &rep)
	}
}

func TestRunVerify_ValidProof(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid: true,
	})
}

func TestRunVerify_BadOrderSig(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	sig := signMsg(priv, lo.Serialize())
	sig[0] ^= 0x01
	lo.SetSig(sig)

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "signature verification failed",
	})
}

func TestRunVerify_DuplicateOrders(t *testing.T) {
	priv := mustPrivKey(t)

	lo1 := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo1.SetSig(signMsg(priv, lo1.Serialize()))
	lo2 := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo2.SetSig(signMsg(priv, lo2.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo1}, {Order: lo2}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "duplicate order ID",
	})
}

func TestRunVerify_BaseQuoteMismatch(t *testing.T) {
	priv := mustPrivKey(t)

	lo1 := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo1.SetSig(signMsg(priv, lo1.Serialize()))
	lo2 := mkLimitOrder(43, 0, 1000, false, 100, 10)
	lo2.SetSig(signMsg(priv, lo2.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo1}, {Order: lo2}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "different base or quote ID",
	})
}

func TestRunVerify_AccountIDMismatch(t *testing.T) {
	priv := mustPrivKey(t)

	lo1 := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo1.SetSig(signMsg(priv, lo1.Serialize()))

	lo2 := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo2.AccountID = mk32(0x99)
	lo2.SetSig(signMsg(priv, lo2.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo1}, {Order: lo2}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "different account ID",
	})
}

func TestRunVerify_CancelTargetMismatch(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()
	badTarget := make([]byte, 32)
	copy(badTarget, oid[:])
	badTarget[0] ^= 0x01

	co := mkCancel(lo.Base, lo.Quote, 1100, badTarget)
	co.SetSig(signMsg(priv, co.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Cancel: co}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "targets",
	})
}

func TestRunVerify_MatchQtyExceedsOrderQty(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()

	m := mkMatch(oid[:], mk32(0x09), 1050, 11, 100)
	m.SetSig(signMsg(priv, m.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Matches: []*msgjson.Match{m}}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "match quantities exceed order quantity",
	})
}

func TestRunVerify_MatchBeforeOrderTime(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()

	m := mkMatch(oid[:], mk32(0x09), 999, 1, 100)
	m.SetSig(signMsg(priv, m.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Matches: []*msgjson.Match{m}}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "before order time",
	})
}

func TestRunVerify_MatchAfterCancelTime(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()

	co := mkCancel(lo.Base, lo.Quote, 1100, oid[:])
	co.SetSig(signMsg(priv, co.Serialize()))

	m := mkMatch(oid[:], mk32(0x09), 1200, 1, 100)
	m.SetSig(signMsg(priv, m.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Cancel: co, Matches: []*msgjson.Match{m}}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "after close time",
	})
}

func TestRunVerify_CancelBeforeOrderTime(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()

	co := mkCancel(lo.Base, lo.Quote, 999, oid[:])
	co.SetSig(signMsg(priv, co.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Cancel: co}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "close time",
	})
}

func TestRunVerify_CancelAndRevokeBothPresent(t *testing.T) {
	priv := mustPrivKey(t)

	lo := mkLimitOrder(42, 0, 1000, false, 100, 10)
	lo.SetSig(signMsg(priv, lo.Serialize()))
	oid := convertMsgLimitOrder(lo).ID()

	co := mkCancel(lo.Base, lo.Quote, 1100, oid[:])
	co.SetSig(signMsg(priv, co.Serialize()))

	ro := mkRevoke(oid[:], 1100)
	ro.SetSig(signMsg(priv, ro.Serialize()))

	proof := &core.MarketMakingProof{Orders: []*core.OrderProof{{Order: lo, Cancel: co, Revoke: ro}}}

	runVerifyE2E(t, priv, proof, 1, 3001, 1000, 10, 0, e2eExpect{
		proofValid:     false,
		proofErrSubstr: "both cancel and revoke",
	})
}

func TestRunVerify_OneSidedMidpointAndSpreadFilter(t *testing.T) {
	priv := mustPrivKey(t)

	// Create three standing orders that cancel AFTER endMs so they stay on the
	// book for the whole report window. This exercises midpoint + spread logic.
	endMs := uint64(3001)
	cancelAfter := uint64(5000)

	o1 := mkLimitOrder(42, 0, 500, false, 100, 10)
	o1.SetSig(signMsg(priv, o1.Serialize()))
	oid1 := convertMsgLimitOrder(o1).ID()
	c1 := mkCancel(o1.Base, o1.Quote, cancelAfter, oid1[:])
	c1.SetSig(signMsg(priv, c1.Serialize()))

	o2 := mkLimitOrder(42, 0, 1500, false, 90, 10)
	o2.SetSig(signMsg(priv, o2.Serialize()))
	oid2 := convertMsgLimitOrder(o2).ID()
	c2 := mkCancel(o2.Base, o2.Quote, cancelAfter, oid2[:])
	c2.SetSig(signMsg(priv, c2.Serialize()))

	o3 := mkLimitOrder(42, 0, 2500, true, 110, 10)
	o3.SetSig(signMsg(priv, o3.Serialize()))
	oid3 := convertMsgLimitOrder(o3).ID()
	c3 := mkCancel(o3.Base, o3.Quote, cancelAfter, oid3[:])
	c3.SetSig(signMsg(priv, c3.Serialize()))

	proof := &core.MarketMakingProof{
		Orders: []*core.OrderProof{
			{Order: o1, Cancel: c1},
			{Order: o2, Cancel: c2},
			{Order: o3, Cancel: c3},
		},
	}

	runVerifyE2E(t, priv, proof, 1, endMs, 1000, 10, 4, e2eExpect{
		proofValid: true,
		checkReport: func(t *testing.T, rep *CoverageReport) {
			if len(rep.TimeSeries) < 2 {
				t.Fatalf("expected at least 2 time points, got %d", len(rep.TimeSeries))
			}
			if rep.TimeSeries[0].EpochNum != 0 {
				t.Fatalf("expected first EpochNum=0, got %d", rep.TimeSeries[0].EpochNum)
			}
			if rep.TimeSeries[0].BuyQty != 10 || rep.TimeSeries[0].SellQty != 0 {
				t.Fatalf("expected epoch0 in-band 10/0, got %d/%d", rep.TimeSeries[0].BuyQty, rep.TimeSeries[0].SellQty)
			}
			last := rep.TimeSeries[len(rep.TimeSeries)-1]
			endEpoch := endMs / 1000
			if last.EpochNum != endEpoch {
				t.Fatalf("expected end epoch %d, got %d", endEpoch, last.EpochNum)
			}
			// After the sell arrives, in-band depth should be 0/0 with 4% max spread.
			if last.BuyQty != 0 || last.SellQty != 0 {
				t.Fatalf("expected end in-band qtys 0/0, got %d/%d", last.BuyQty, last.SellQty)
			}
			if last.TotalBuyQty != 20 || last.TotalSellQty != 10 {
				t.Fatalf("expected totals 20/10, got %d/%d", last.TotalBuyQty, last.TotalSellQty)
			}
			if last.BestBidRate != 100 || last.BestAskRate != 110 {
				t.Fatalf("expected best bid/ask 100/110, got %d/%d", last.BestBidRate, last.BestAskRate)
			}
		},
	})
}
