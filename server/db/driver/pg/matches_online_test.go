//go:build pgonline

package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
)

// insertMatchForTest stores or updates a match row via the shared upsertMatch
// helper, the same path the epoch_processed event applier takes.
func insertMatchForTest(match *order.Match) error {
	matchesTableName, err := archie.matchTableName(match)
	if err != nil {
		return err
	}
	N, err := upsertMatch(archie.db, matchesTableName, match)
	if err != nil {
		return err
	}
	if N != 1 {
		return fmt.Errorf("upsertMatch: updated %d rows, expected 1", N)
	}
	return nil
}

// saveContractForTest records a swap contract via the unexported
// swap_contract_recorded applier helper.
func saveContractForTest(mid db.MarketMatchID, maker bool, contract, coinID []byte, timestamp int64) error {
	return archie.applySwapContractRecordedEvent(archie.db, &db.SwapContract{
		MID:       mid,
		Maker:     maker,
		Contract:  contract,
		CoinID:    coinID,
		Timestamp: timestamp,
	})
}

// saveRedeemForTest records a redemption via the shared recordRedeemData
// helper used by the swap_redemption_recorded event applier.
func saveRedeemForTest(mid db.MarketMatchID, maker bool, coinID, secret []byte, timestamp int64) error {
	return archie.recordRedeemData(archie.db, &db.SwapRedemption{
		MID:       mid,
		Maker:     maker,
		CoinID:    coinID,
		Secret:    secret,
		Timestamp: timestamp,
	})
}

func Test_upsertMatch(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Make a perfect 1 lot match.
	limitBuyStanding := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	epochID := order.EpochID{132412341, 1000}
	// Taker is selling.
	matchA := newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)

	base, quote := limitBuyStanding.Base(), limitBuyStanding.Quote()

	matchAUpdated := matchA
	matchAUpdated.Status = order.MakerSwapCast
	// matchAUpdated.Sigs.MakerMatch = randomBytes(73)

	cancelLOBuy := newCancelOrder(limitBuyStanding.ID(), base, quote, 0)
	matchCancel := newMatch(limitBuyStanding, cancelLOBuy, 0, epochID)
	matchCancel.Status = order.MatchComplete // will be forced to complete on store too

	tests := []struct {
		name     string
		match    *order.Match
		wantErr  bool
		isCancel bool
	}{
		{
			"store ok",
			matchA,
			false,
			false,
		},
		{
			"update ok",
			matchAUpdated,
			false,
			false,
		},
		{
			"update again ok",
			matchAUpdated,
			false,
			false,
		},
		{
			"cancel",
			matchCancel,
			false,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := insertMatchForTest(tt.match)
			if (err != nil) != tt.wantErr {
				t.Errorf("insertMatchForTest() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			matchID := tt.match.ID()
			matchData, err := archie.MatchByID(matchID, base, quote)
			if err != nil {
				t.Fatal(err)
			}
			if matchData.ID != matchID {
				t.Errorf("Retrieved match with ID %v, expected %v", matchData.ID, matchID)
			}
			if matchData.Status != tt.match.Status {
				t.Errorf("Incorrect match status, got %d, expected %d",
					matchData.Status, tt.match.Status)
			}
			if tt.isCancel {
				if matchData.Active {
					t.Errorf("Incorrect match active flag, got %v, expected false",
						matchData.Active)
				}
				trade := tt.match.Taker.Trade()
				if trade != nil {
					if matchData.TakerSell != trade.Sell {
						t.Errorf("expected takerSell = %v, got %v", trade.Sell, matchData.TakerSell)
					}
					if matchData.BaseRate != tt.match.FeeRateBase {
						t.Errorf("expected base fee rate %d, got %d", tt.match.FeeRateBase, matchData.BaseRate)
					}
				} else {
					if matchData.BaseRate != 0 {
						t.Errorf("cancel order should have 0 base fee rate, got %d", matchData.BaseRate)
					}
					if matchData.QuoteRate != 0 {
						t.Errorf("cancel order should have 0 quote fee rate, got %d", matchData.QuoteRate)
					}
					if matchData.TakerSell {
						t.Errorf("cancel order should have false for takerSell")
					}
				}
				if matchData.TakerAddr != "" {
					t.Errorf("Expected empty taker address for cancel match, got %v", matchData.TakerAddr)
				}
				if matchData.MakerAddr != "" {
					t.Errorf("Expected empty maker address for cancel match, got %v", matchData.MakerAddr)
				}
			}
		})
	}
}

func TestSetSwapData(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Make a perfect 1 lot match.
	limitBuyStanding := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	epochID := order.EpochID{132412341, 1000}
	matchA := newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	matchID := matchA.ID()

	base, quote := limitBuyStanding.Base(), limitBuyStanding.Quote()

	checkMatch := func(wantStatus order.MatchStatus, wantActive bool) error {
		matchData, err := archie.MatchByID(matchID, base, quote)
		if err != nil {
			return err
		}
		if matchData.ID != matchID {
			return fmt.Errorf("Retrieved match with ID %v, expected %v", matchData.ID, matchID)
		}
		if matchData.Status != wantStatus {
			return fmt.Errorf("Incorrect match status, got %d, expected %d",
				matchData.Status, wantStatus)
		}
		if matchData.Active != wantActive {
			return fmt.Errorf("Incorrect match active flag, got %v, expected %v",
				matchData.Active, wantActive)
		}
		return nil
	}

	err := insertMatchForTest(matchA)
	if err != nil {
		t.Errorf("insertMatchForTest() failed: %v", err)
	}

	if err = checkMatch(order.NewlyMatched, true); err != nil {
		t.Fatal(err)
	}

	mid := db.MarketMatchID{
		MatchID: matchA.ID(),
		Base:    base,
		Quote:   quote,
	}

	// Match Ack Sig A (maker's match ack sig)
	sigMakerMatch := randomBytes(73)
	err = archie.saveMatchAck(archie.db, &db.MatchAck{
		MID:     mid,
		Maker:   true,
		Sig:     sigMakerMatch,
		Address: "maker-swap-addr",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, swapData, err := archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.NewlyMatched {
		t.Errorf("Got status %v, expected %v", status, order.NewlyMatched)
	}
	if !bytes.Equal(swapData.SigMatchAckMaker, sigMakerMatch) {
		t.Fatalf("SigMatchAckMaker incorrect. got %v, expected %v",
			swapData.SigMatchAckMaker, sigMakerMatch)
	}

	// Match Ack Sig B (taker's match ack sig)
	sigTakerMatch := randomBytes(73)
	err = archie.saveMatchAck(archie.db, &db.MatchAck{
		MID:     mid,
		Maker:   false,
		Sig:     sigTakerMatch,
		Address: "taker-swap-addr",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.NewlyMatched {
		t.Errorf("Got status %v, expected %v", status, order.NewlyMatched)
	}
	if !bytes.Equal(swapData.SigMatchAckTaker, sigTakerMatch) {
		t.Fatalf("SigMatchAckTaker incorrect. got %v, expected %v",
			swapData.SigMatchAckTaker, sigTakerMatch)
	}

	// Contract A
	contractA := randomBytes(128)
	coinIDA := randomBytes(36)
	contractATime := int64(1234)
	err = saveContractForTest(mid, true, contractA, coinIDA, contractATime)
	if err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.MakerSwapCast {
		t.Errorf("Got status %v, expected %v", status, order.MakerSwapCast)
	}
	if !bytes.Equal(swapData.ContractA, contractA) {
		t.Fatalf("ContractA incorrect. got %v, expected %v",
			swapData.ContractA, contractA)
	}
	if !bytes.Equal(swapData.ContractACoinID, coinIDA) {
		t.Fatalf("ContractACoinID incorrect. got %v, expected %v",
			swapData.ContractACoinID, coinIDA)
	}
	if swapData.ContractATime != contractATime {
		t.Fatalf("ContractATime incorrect. got %d, expected %d",
			swapData.ContractATime, contractATime)
	}

	// Party B's signature for acknowledgement of contract A
	auditSigB := randomBytes(73)
	if err = archie.applyAuditAckRecordedEvent(archie.db, &db.AuditAck{
		MID:   mid,
		Maker: false,
		Sig:   auditSigB,
	}); err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.MakerSwapCast {
		t.Errorf("Got status %v, expected %v", status, order.MakerSwapCast)
	}
	if !bytes.Equal(swapData.ContractAAckSig, auditSigB) {
		t.Fatalf("ContractAAckSig incorrect. got %v, expected %v",
			swapData.ContractAAckSig, auditSigB)
	}

	// Contract B
	contractB := randomBytes(128)
	coinIDB := randomBytes(36)
	contractBTime := int64(1235)
	err = saveContractForTest(mid, false, contractB, coinIDB, contractBTime)
	if err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.TakerSwapCast {
		t.Errorf("Got status %v, expected %v", status, order.TakerSwapCast)
	}
	if !bytes.Equal(swapData.ContractB, contractB) {
		t.Fatalf("ContractB incorrect. got %v, expected %v",
			swapData.ContractB, contractB)
	}
	if !bytes.Equal(swapData.ContractBCoinID, coinIDB) {
		t.Fatalf("ContractBCoinID incorrect. got %v, expected %v",
			swapData.ContractBCoinID, coinIDB)
	}
	if swapData.ContractBTime != contractBTime {
		t.Fatalf("ContractBTime incorrect. got %d, expected %d",
			swapData.ContractBTime, contractBTime)
	}

	// Party A's signature for acknowledgement of contract B
	auditSigA := randomBytes(73)
	if err = archie.applyAuditAckRecordedEvent(archie.db, &db.AuditAck{
		MID:   mid,
		Maker: true,
		Sig:   auditSigA,
	}); err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.TakerSwapCast {
		t.Errorf("Got status %v, expected %v", status, order.TakerSwapCast)
	}
	if !bytes.Equal(swapData.ContractBAckSig, auditSigA) {
		t.Fatalf("ContractBAckSig incorrect. got %v, expected %v",
			swapData.ContractBAckSig, auditSigB)
	}

	// Redeem A
	redeemCoinIDA := randomBytes(36)
	secret := randomBytes(72)
	redeemATime := int64(1234)
	err = saveRedeemForTest(mid, true, redeemCoinIDA, secret, redeemATime)
	if err != nil {
		t.Fatal(err)
	}
	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.MakerRedeemed {
		t.Errorf("Got status %v, expected %v", status, order.MakerRedeemed)
	}
	if !bytes.Equal(swapData.RedeemACoinID, redeemCoinIDA) {
		t.Fatalf("RedeemACoinID incorrect. got %v, expected %v",
			swapData.RedeemACoinID, redeemCoinIDA)
	}
	if !bytes.Equal(swapData.RedeemASecret, secret) {
		t.Fatalf("RedeemASecret incorrect. got %v, expected %v",
			swapData.RedeemASecret, secret)
	}
	if swapData.RedeemATime != redeemATime {
		t.Fatalf("RedeemATime incorrect. got %d, expected %d",
			swapData.RedeemATime, redeemATime)
	}

	// Party B's signature for acknowledgement of A's redemption
	redeemAckSigB := randomBytes(73)
	if err = archie.applyRedemptionAckRecordedEvent(archie.db, &db.RedemptionAck{
		MID:   mid,
		Maker: false,
		Sig:   redeemAckSigB,
	}); err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.MakerRedeemed {
		t.Errorf("Got status %v, expected %v", status, order.MakerRedeemed)
	}
	if !bytes.Equal(swapData.RedeemAAckSig, redeemAckSigB) {
		t.Fatalf("RedeemAAckSig incorrect. got %v, expected %v",
			swapData.RedeemAAckSig, redeemAckSigB)
	}

	// Redeem B
	redeemCoinIDB := randomBytes(36)
	redeemBTime := int64(1234)
	err = saveRedeemForTest(mid, false, redeemCoinIDB, nil, redeemBTime)
	if err != nil {
		t.Fatal(err)
	}

	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatal(err)
	}
	if status != order.MatchComplete {
		t.Errorf("Got status %v, expected %v", status, order.MatchComplete)
	}
	if !bytes.Equal(swapData.RedeemBCoinID, redeemCoinIDB) {
		t.Fatalf("RedeemBCoinID incorrect. got %v, expected %v",
			swapData.RedeemBCoinID, redeemCoinIDB)
	}
	if swapData.RedeemBTime != redeemBTime {
		t.Fatalf("RedeemBTime incorrect. got %d, expected %d",
			swapData.RedeemBTime, redeemBTime)
	}

	// Check active flag via MatchByID.
	if err = checkMatch(order.MatchComplete, false); err != nil {
		t.Fatal(err)
	}
}

func TestApplyMatchAcksRecordedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	sharedUser := randomAccountID()
	makerAckPair := generateMatch(t, order.NewlyMatched, true, sharedUser, randomAccountID())
	takerAckPair := generateMatch(t, order.NewlyMatched, true, randomAccountID(), sharedUser, 132412342)
	makerAckMID := testMarketMatchID(makerAckPair.match)
	takerAckMID := testMarketMatchID(takerAckPair.match)
	update := &db.MatchAcksRecordedUpdate{Acks: []*db.MatchAck{
		{MID: makerAckMID, Maker: true, Sig: []byte("maker-match-sig"), Address: "maker-swap-addr"},
		{MID: takerAckMID, Maker: false, Sig: []byte("taker-match-sig"), Address: "taker-swap-addr"},
	}}

	// A single user's match ack response may include multiple matches. This event
	// writes one maker-side ack and one taker-side ack for different matches in
	// one DB transaction before appending the event-log entry.
	event := []byte("match-acks-event")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindMatchAcksRecorded, event, update)
	log, err := archie.ApplyMatchAcksRecordedEvent(ctx, &db.EventLogMeta{Event: event}, update)
	if err != nil {
		t.Fatalf("ApplyMatchAcksRecordedEvent error: %v", err)
	}
	requireEventApplyLog(t, log, 1, meshevents.EventKindMatchAcksRecorded, event, tip, update)
	assertEventLogFrontier(t, 1, tip)

	_, makerSwapData, err := archie.SwapData(makerAckMID)
	if err != nil {
		t.Fatalf("SwapData maker ack error: %v", err)
	}
	if !bytes.Equal(makerSwapData.SigMatchAckMaker, []byte("maker-match-sig")) ||
		makerSwapData.MakerSwapAddr != "maker-swap-addr" ||
		len(makerSwapData.SigMatchAckTaker) != 0 || makerSwapData.TakerSwapAddr != "" {
		t.Fatalf("maker match swap data = %+v, want only maker ack and address", makerSwapData)
	}
	_, takerSwapData, err := archie.SwapData(takerAckMID)
	if err != nil {
		t.Fatalf("SwapData taker ack error: %v", err)
	}
	if !bytes.Equal(takerSwapData.SigMatchAckTaker, []byte("taker-match-sig")) ||
		takerSwapData.TakerSwapAddr != "taker-swap-addr" ||
		len(takerSwapData.SigMatchAckMaker) != 0 || takerSwapData.MakerSwapAddr != "" {
		t.Fatalf("taker match swap data = %+v, want only taker ack and address", takerSwapData)
	}

	rollbackUpdate := &db.MatchAcksRecordedUpdate{Acks: []*db.MatchAck{{
		MID:     makerAckMID,
		Maker:   true,
		Sig:     []byte("rolled-back-maker-sig"),
		Address: "rolled-back-addr",
	}}}
	_, err = archie.ApplyMatchAcksRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("match-acks-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, rollbackUpdate)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplyMatchAcksRecordedEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	_, makerSwapData, err = archie.SwapData(makerAckMID)
	if err != nil {
		t.Fatalf("SwapData after rollback error: %v", err)
	}
	if !bytes.Equal(makerSwapData.SigMatchAckMaker, []byte("maker-match-sig")) || makerSwapData.MakerSwapAddr != "maker-swap-addr" {
		t.Fatalf("rollback changed maker ack data: %+v", makerSwapData)
	}
	assertEventLogFrontier(t, 1, tip)

	// A re-ack event with a different address must refresh the ack sig but
	// keep the first recorded address, and the no-op address update must
	// still count as the one updated row.
	reackUpdate := &db.MatchAcksRecordedUpdate{Acks: []*db.MatchAck{{
		MID:     makerAckMID,
		Maker:   true,
		Sig:     []byte("maker-match-resig"),
		Address: "divergent-maker-addr",
	}}}
	reackEvent := []byte("match-acks-reack-event")
	tip2 := testEventApplyTip(t, tip, 2, meshevents.EventKindMatchAcksRecorded, reackEvent, reackUpdate)
	log2, err := archie.ApplyMatchAcksRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           reackEvent,
		ExpectedTipHash: tip2,
	}, reackUpdate)
	if err != nil {
		t.Fatalf("ApplyMatchAcksRecordedEvent re-ack error: %v", err)
	}
	requireEventApplyLog(t, log2, 2, meshevents.EventKindMatchAcksRecorded, reackEvent, tip2, reackUpdate)
	_, makerSwapData, err = archie.SwapData(makerAckMID)
	if err != nil {
		t.Fatalf("SwapData after re-ack error: %v", err)
	}
	if !bytes.Equal(makerSwapData.SigMatchAckMaker, []byte("maker-match-resig")) {
		t.Fatalf("re-ack did not refresh sig: %+v", makerSwapData)
	}
	if makerSwapData.MakerSwapAddr != "maker-swap-addr" {
		t.Fatalf("re-ack displaced first-wins address: got %q, want maker-swap-addr",
			makerSwapData.MakerSwapAddr)
	}
	assertEventLogFrontier(t, 2, tip2)
}

func TestApplySwapContractRecordedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	pair := generateMatch(t, order.NewlyMatched, true, randomAccountID(), randomAccountID())
	mid := testMarketMatchID(pair.match)
	makerContract := &db.SwapContract{
		MID:       mid,
		Maker:     true,
		Contract:  []byte("maker-contract"),
		CoinID:    []byte("maker-coin"),
		Timestamp: 1670000000000,
	}

	// The maker contract advances the persisted match status and stores the
	// maker's contract script, coin ID, and timestamp beside the event log.
	event := []byte("swap-contract-maker-event")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindSwapContractRecorded, event, makerContract)
	log, err := archie.ApplySwapContractRecordedEvent(ctx, &db.EventLogMeta{Event: event}, makerContract)
	if err != nil {
		t.Fatalf("ApplySwapContractRecordedEvent maker error: %v", err)
	}
	requireEventApplyLog(t, log, 1, meshevents.EventKindSwapContractRecorded, event, tip, makerContract)
	status, swapData, err := archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData maker error: %v", err)
	}
	if status != order.MakerSwapCast || !bytes.Equal(swapData.ContractA, makerContract.Contract) ||
		!bytes.Equal(swapData.ContractACoinID, makerContract.CoinID) || swapData.ContractATime != makerContract.Timestamp {
		t.Fatalf("maker swap data status=%v data=%+v", status, swapData)
	}

	takerContract := &db.SwapContract{
		MID:       mid,
		Contract:  []byte("taker-contract"),
		CoinID:    []byte("taker-coin"),
		Timestamp: 1670000001111,
	}
	_, err = archie.ApplySwapContractRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("swap-contract-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, takerContract)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplySwapContractRecordedEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData after rollback error: %v", err)
	}
	if status != order.MakerSwapCast || len(swapData.ContractB) != 0 {
		t.Fatalf("rollback changed taker contract status=%v data=%+v", status, swapData)
	}
	assertEventLogFrontier(t, 1, tip)

	takerEvent := []byte("swap-contract-taker-event")
	takerTip := testEventApplyTip(t, tip, 2, meshevents.EventKindSwapContractRecorded, takerEvent, takerContract)
	log, err = archie.ApplySwapContractRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           takerEvent,
		ExpectedTipHash: takerTip,
	}, takerContract)
	if err != nil {
		t.Fatalf("ApplySwapContractRecordedEvent taker error: %v", err)
	}
	requireEventApplyLog(t, log, 2, meshevents.EventKindSwapContractRecorded, takerEvent, takerTip, takerContract)
	status, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData taker error: %v", err)
	}
	if status != order.TakerSwapCast || !bytes.Equal(swapData.ContractB, takerContract.Contract) ||
		!bytes.Equal(swapData.ContractBCoinID, takerContract.CoinID) || swapData.ContractBTime != takerContract.Timestamp {
		t.Fatalf("taker swap data status=%v data=%+v", status, swapData)
	}
}

func TestApplyAuditAckRecordedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	pair := generateMatch(t, order.TakerSwapCast, true, randomAccountID(), randomAccountID())
	mid := testMarketMatchID(pair.match)
	takerAck := &db.AuditAck{MID: mid, Sig: []byte("taker-audit-ack")}

	// The taker audit ack signs the maker's contract and is stored with the
	// event-log entry.
	event := []byte("audit-ack-taker-event")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindAuditAckRecorded, event, takerAck)
	log, err := archie.ApplyAuditAckRecordedEvent(ctx, &db.EventLogMeta{Event: event}, takerAck)
	if err != nil {
		t.Fatalf("ApplyAuditAckRecordedEvent taker error: %v", err)
	}
	requireEventApplyLog(t, log, 1, meshevents.EventKindAuditAckRecorded, event, tip, takerAck)
	_, swapData, err := archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData taker ack error: %v", err)
	}
	if !bytes.Equal(swapData.ContractAAckSig, takerAck.Sig) {
		t.Fatalf("ContractAAckSig = %x, want %x", swapData.ContractAAckSig, takerAck.Sig)
	}

	makerAck := &db.AuditAck{MID: mid, Maker: true, Sig: []byte("maker-audit-ack")}
	_, err = archie.ApplyAuditAckRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("audit-ack-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, makerAck)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplyAuditAckRecordedEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	_, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData after rollback error: %v", err)
	}
	if len(swapData.ContractBAckSig) != 0 {
		t.Fatalf("rollback stored maker audit ack: %x", swapData.ContractBAckSig)
	}
	assertEventLogFrontier(t, 1, tip)

	makerEvent := []byte("audit-ack-maker-event")
	makerTip := testEventApplyTip(t, tip, 2, meshevents.EventKindAuditAckRecorded, makerEvent, makerAck)
	log, err = archie.ApplyAuditAckRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           makerEvent,
		ExpectedTipHash: makerTip,
	}, makerAck)
	if err != nil {
		t.Fatalf("ApplyAuditAckRecordedEvent maker error: %v", err)
	}
	requireEventApplyLog(t, log, 2, meshevents.EventKindAuditAckRecorded, makerEvent, makerTip, makerAck)
	_, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData maker ack error: %v", err)
	}
	if !bytes.Equal(swapData.ContractBAckSig, makerAck.Sig) {
		t.Fatalf("ContractBAckSig = %x, want %x", swapData.ContractBAckSig, makerAck.Sig)
	}
}

func TestApplySwapRedemptionRecordedEvent(t *testing.T) {
	ctx := context.Background()
	policy := &db.ReputationOutcomePolicy{MatchLimit: 10, OrderLimit: 10}

	newRedemption := func(pair *matchPair, maker bool, stamp int64) *db.SwapRedemption {
		coinID := []byte("taker-redeem-coin")
		secret := []byte(nil)
		if maker {
			coinID = []byte("maker-redeem-coin")
			secret = []byte("secret")
		}
		return &db.SwapRedemption{
			MID:       testMarketMatchID(pair.match),
			Maker:     maker,
			CoinID:    coinID,
			Secret:    secret,
			Timestamp: stamp,
		}
	}

	apply := func(t *testing.T, redemption *db.SwapRedemption, event []byte) *db.EventLogEntry {
		t.Helper()
		log, err := archie.ApplySwapRedemptionRecordedEvent(ctx, &db.EventLogMeta{Event: event}, policy, redemption)
		if err != nil {
			t.Fatalf("ApplySwapRedemptionRecordedEvent error: %v", err)
		}
		baseTxData, err := redemption.EventTxData()
		if err != nil {
			t.Fatalf("EventTxData error: %v", err)
		}
		tip := testEventApplyTipForTxData(t, nil, 1, meshevents.EventKindSwapRedemptionRecorded, event, baseTxData)
		requireEventApplyLogTxData(t, log, 1, meshevents.EventKindSwapRedemptionRecorded, event, tip, baseTxData)
		return log
	}

	requireCompleted := func(t *testing.T, user account.AccountID, oid order.OrderID, wantTime int64) {
		t.Helper()
		completed, compTimes, err := archie.CompletedUserOrders(user, 10)
		if err != nil {
			t.Fatalf("CompletedUserOrders error: %v", err)
		}
		if len(completed) != 1 || completed[0] != oid || compTimes[0] != wantTime {
			t.Fatalf("completed orders = %v/%v, want %v at %d", completed, compTimes, oid, wantTime)
		}
	}

	requireNoCompleted := func(t *testing.T, user account.AccountID) {
		t.Helper()
		completed, _, err := archie.CompletedUserOrders(user, 10)
		if err != nil {
			t.Fatalf("CompletedUserOrders error: %v", err)
		}
		if len(completed) != 0 {
			t.Fatalf("completed orders = %v, want none", completed)
		}
	}

	requireReputation := func(t *testing.T, user account.AccountID, wantMatches, wantOrders int) {
		t.Helper()
		_, matches, orders, err := archie.GetUserReputationData(ctx, user, 10, 10, 10)
		if err != nil {
			t.Fatalf("GetUserReputationData error: %v", err)
		}
		if len(matches) != wantMatches || len(orders) != wantOrders {
			t.Fatalf("reputation matches=%+v orders=%+v, want %d/%d", matches, orders, wantMatches, wantOrders)
		}
		if wantMatches == 1 && matches[0].MatchOutcome != db.OutcomeSwapSuccess {
			t.Fatalf("match outcome = %v, want swap success", matches[0].MatchOutcome)
		}
		if wantOrders == 1 && orders[0].Canceled {
			t.Fatalf("order outcome = %+v, want completed", orders[0])
		}
	}

	addMakerMatch := func(t *testing.T, makerOrder *order.LimitOrder, taker account.AccountID, status order.MatchStatus) {
		t.Helper()
		loSell := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)
		loSell.P.AccountID = taker
		epochID := order.EpochID{uint64(132412999), 1000}
		if err := storeOrderForTest(archie, loSell, int64(epochID.Idx), int64(epochID.Dur), order.OrderStatusExecuted); err != nil {
			t.Fatalf("failed to store related taker order: %v", err)
		}
		match := newMatch(makerOrder, loSell, loSell.Quantity, epochID)
		match.Status = status
		if err := insertMatchForTest(match); err != nil {
			t.Fatalf("insertMatchForTest related match failed: %v", err)
		}
		mid := testMarketMatchID(match)
		if status >= order.MakerSwapCast {
			if err := saveContractForTest(mid, true, encode.RandomBytes(50), encode.RandomBytes(36), 0); err != nil {
				t.Fatalf("saveContractForTest (maker) related match error: %v", err)
			}
		}
		if status >= order.TakerSwapCast {
			if err := saveContractForTest(mid, false, encode.RandomBytes(50), encode.RandomBytes(36), 0); err != nil {
				t.Fatalf("saveContractForTest (taker) related match error: %v", err)
			}
		}
		if status >= order.MakerRedeemed {
			if err := saveRedeemForTest(mid, true, encode.RandomBytes(36), encode.RandomBytes(32), 0); err != nil {
				t.Fatalf("saveRedeemForTest (maker) related match error: %v", err)
			}
		}
	}

	addTakerMatch := func(t *testing.T, maker account.AccountID, takerOrder *order.LimitOrder, status order.MatchStatus) {
		t.Helper()
		loBuy := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
		loBuy.P.AccountID = maker
		epochID := order.EpochID{uint64(132413999), 1000}
		if err := storeOrderForTest(archie, loBuy, int64(epochID.Idx), int64(epochID.Dur), order.OrderStatusExecuted); err != nil {
			t.Fatalf("failed to store related maker order: %v", err)
		}
		match := newMatch(loBuy, takerOrder, takerOrder.Quantity, epochID)
		match.Status = status
		if err := insertMatchForTest(match); err != nil {
			t.Fatalf("insertMatchForTest related taker match failed: %v", err)
		}
		mid := testMarketMatchID(match)
		if status >= order.MakerSwapCast {
			if err := saveContractForTest(mid, true, encode.RandomBytes(50), encode.RandomBytes(36), 0); err != nil {
				t.Fatalf("saveContractForTest (maker) related taker match error: %v", err)
			}
		}
		if status >= order.TakerSwapCast {
			if err := saveContractForTest(mid, false, encode.RandomBytes(50), encode.RandomBytes(36), 0); err != nil {
				t.Fatalf("saveContractForTest (taker) related taker match error: %v", err)
			}
		}
		if status >= order.MakerRedeemed {
			if err := saveRedeemForTest(mid, true, encode.RandomBytes(36), encode.RandomBytes(32), 0); err != nil {
				t.Fatalf("saveRedeemForTest (maker) related taker match error: %v", err)
			}
		}
	}

	t.Run("nil redemption validates before event log", func(t *testing.T) {
		if err := cleanTables(archie.db); err != nil {
			t.Fatalf("cleanTables: %v", err)
		}
		_, err := archie.ApplySwapRedemptionRecordedEvent(ctx, &db.EventLogMeta{Event: []byte("nil-redemption")}, policy, nil)
		if err == nil {
			t.Fatalf("nil redemption apply succeeded")
		}
		assertEventLogFrontier(t, 0, nil)
	})

	t.Run("rollback restores redemption completion and reputation", func(t *testing.T) {
		if err := cleanTables(archie.db); err != nil {
			t.Fatalf("cleanTables: %v", err)
		}
		maker := randomAccountID()
		pair := generateMatch(t, order.TakerSwapCast, true, maker, randomAccountID())
		redemption := newRedemption(pair, true, 1670000002222)
		_, err := archie.ApplySwapRedemptionRecordedEvent(ctx, &db.EventLogMeta{
			Seq:             1,
			Event:           []byte("swap-redemption-bad-tip"),
			ExpectedTipHash: wrongEventTip(),
		}, policy, redemption)
		var divergence *db.EventLogDivergenceError
		if !errors.As(err, &divergence) {
			t.Fatalf("ApplySwapRedemptionRecordedEvent error = %T %[1]v, want EventLogDivergenceError", err)
		}
		status, swapData, err := archie.SwapData(redemption.MID)
		if err != nil {
			t.Fatalf("SwapData after rollback error: %v", err)
		}
		if status != order.TakerSwapCast || len(swapData.RedeemACoinID) != 0 {
			t.Fatalf("rollback changed redemption status=%v data=%+v", status, swapData)
		}
		requireNoCompleted(t, maker)
		requireReputation(t, maker, 0, 0)
		assertEventLogFrontier(t, 0, nil)
	})

	t.Run("status mismatch validates before writes", func(t *testing.T) {
		tests := []struct {
			name       string
			maker      bool
			status     order.MatchStatus
			wantStatus order.MatchStatus
		}{
			{
				name:       "maker redemption requires taker swap",
				maker:      true,
				status:     order.MakerSwapCast,
				wantStatus: order.MakerSwapCast,
			},
			{
				name:       "taker redemption requires maker redemption",
				status:     order.TakerSwapCast,
				wantStatus: order.TakerSwapCast,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if err := cleanTables(archie.db); err != nil {
					t.Fatalf("cleanTables: %v", err)
				}
				pair := generateMatch(t, tt.status, true, randomAccountID(), randomAccountID())
				redemption := newRedemption(pair, tt.maker, 1670000003333)
				_, err := archie.ApplySwapRedemptionRecordedEvent(ctx, &db.EventLogMeta{
					Event: []byte(tt.name),
				}, policy, redemption)
				if err == nil {
					t.Fatalf("ApplySwapRedemptionRecordedEvent succeeded for status %v", tt.status)
				}

				status, swapData, err := archie.SwapData(redemption.MID)
				if err != nil {
					t.Fatalf("SwapData after status mismatch error: %v", err)
				}
				if status != tt.wantStatus {
					t.Fatalf("status = %v, want %v", status, tt.wantStatus)
				}
				if len(swapData.RedeemACoinID) != 0 || len(swapData.RedeemBCoinID) != 0 {
					t.Fatalf("status mismatch wrote redemption data: %+v", swapData)
				}
				assertEventLogFrontier(t, 0, nil)
			})
		}
	})

	tests := []struct {
		name        string
		maker       bool
		status      order.MatchStatus
		makerStatus order.OrderStatus
		takerStatus order.OrderStatus
		sameUser    bool
		addMaker    order.MatchStatus
		addTaker    order.MatchStatus
		wantBlock   bool
	}{
		{
			name:        "maker redemption completes maker order",
			maker:       true,
			status:      order.TakerSwapCast,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
		},
		{
			name:        "taker redemption completes taker order",
			status:      order.MakerRedeemed,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
		},
		{
			name:        "booked maker order does not complete",
			maker:       true,
			status:      order.TakerSwapCast,
			makerStatus: order.OrderStatusBooked,
			takerStatus: order.OrderStatusExecuted,
			wantBlock:   true,
		},
		{
			name:        "unsettled maker match blocks completion",
			maker:       true,
			status:      order.TakerSwapCast,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
			addMaker:    order.TakerSwapCast,
			wantBlock:   true,
		},
		{
			name:        "unsettled taker match blocks completion",
			status:      order.MakerRedeemed,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
			addTaker:    order.TakerSwapCast,
			wantBlock:   true,
		},
		{
			name:        "already maker redeemed match does not block maker completion",
			maker:       true,
			status:      order.TakerSwapCast,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
			addMaker:    order.MakerRedeemed,
		},
		{
			name:        "self match suppresses match reputation only",
			maker:       true,
			status:      order.TakerSwapCast,
			makerStatus: order.OrderStatusExecuted,
			takerStatus: order.OrderStatusExecuted,
			sameUser:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := cleanTables(archie.db); err != nil {
				t.Fatalf("cleanTables: %v", err)
			}
			maker, taker := randomAccountID(), randomAccountID()
			if tt.sameUser {
				taker = maker
			}
			pair := generateMatchWithOrderStatuses(t, tt.status, true, maker, taker, tt.makerStatus, tt.takerStatus)
			if tt.addMaker != 0 {
				addMakerMatch(t, pair.match.Maker, randomAccountID(), tt.addMaker)
			}
			if tt.addTaker != 0 {
				takerOrder, ok := pair.match.Taker.(*order.LimitOrder)
				if !ok {
					t.Fatalf("test match taker = %T, want *order.LimitOrder", pair.match.Taker)
				}
				addTakerMatch(t, randomAccountID(), takerOrder, tt.addTaker)
			}
			redemption := newRedemption(pair, tt.maker, 1670000003333)
			apply(t, redemption, []byte(tt.name))

			status, swapData, err := archie.SwapData(redemption.MID)
			if err != nil {
				t.Fatalf("SwapData error: %v", err)
			}
			wantStatus := order.MatchComplete
			if tt.maker {
				wantStatus = order.MakerRedeemed
				if !bytes.Equal(swapData.RedeemACoinID, redemption.CoinID) ||
					!bytes.Equal(swapData.RedeemASecret, redemption.Secret) ||
					swapData.RedeemATime != redemption.Timestamp {
					t.Fatalf("maker redeem data status=%v data=%+v", status, swapData)
				}
			} else if !bytes.Equal(swapData.RedeemBCoinID, redemption.CoinID) ||
				swapData.RedeemBTime != redemption.Timestamp {
				t.Fatalf("taker redeem data status=%v data=%+v", status, swapData)
			}
			if status != wantStatus {
				t.Fatalf("redemption status = %v, want %v", status, wantStatus)
			}

			user, oid := taker, pair.match.Taker.ID()
			if tt.maker {
				user, oid = maker, pair.match.Maker.ID()
			}
			if tt.wantBlock {
				requireNoCompleted(t, user)
				requireReputation(t, user, 1, 0)
				return
			}
			requireCompleted(t, user, oid, redemption.Timestamp)
			wantMatches := 1
			if tt.sameUser {
				wantMatches = 0
			}
			requireReputation(t, user, wantMatches, 1)
		})
	}
}

func TestApplyRedemptionAckRecordedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	pair := generateMatch(t, order.MakerRedeemed, true, randomAccountID(), randomAccountID())
	mid := testMarketMatchID(pair.match)
	takerAck := &db.RedemptionAck{MID: mid, Sig: []byte("taker-redemption-ack")}

	// The taker redemption ack is durable DB state because it acknowledges the
	// maker's redemption and may later complete the DB-side match lifecycle.
	event := []byte("redemption-ack-taker-event")
	tip := testEventApplyTip(t, nil, 1, meshevents.EventKindRedemptionAckRecorded, event, takerAck)
	log, err := archie.ApplyRedemptionAckRecordedEvent(ctx, &db.EventLogMeta{Event: event}, takerAck)
	if err != nil {
		t.Fatalf("ApplyRedemptionAckRecordedEvent taker error: %v", err)
	}
	requireEventApplyLog(t, log, 1, meshevents.EventKindRedemptionAckRecorded, event, tip, takerAck)
	_, swapData, err := archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData taker ack error: %v", err)
	}
	if !bytes.Equal(swapData.RedeemAAckSig, takerAck.Sig) {
		t.Fatalf("RedeemAAckSig = %x, want %x", swapData.RedeemAAckSig, takerAck.Sig)
	}

	rollbackAck := &db.RedemptionAck{MID: mid, Sig: []byte("rolled-back-redemption-ack")}
	_, err = archie.ApplyRedemptionAckRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           []byte("redemption-ack-bad-tip"),
		ExpectedTipHash: wrongEventTip(),
	}, rollbackAck)
	var divergence *db.EventLogDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("ApplyRedemptionAckRecordedEvent error = %T %[1]v, want EventLogDivergenceError", err)
	}
	_, swapData, err = archie.SwapData(mid)
	if err != nil {
		t.Fatalf("SwapData after rollback error: %v", err)
	}
	if !bytes.Equal(swapData.RedeemAAckSig, takerAck.Sig) {
		t.Fatalf("rollback changed taker redemption ack to %x", swapData.RedeemAAckSig)
	}
	assertEventLogFrontier(t, 1, tip)

	makerAck := &db.RedemptionAck{MID: mid, Maker: true, Sig: []byte("maker-redemption-ack")}
	makerEvent := []byte("redemption-ack-maker-event")
	makerTip := testEventApplyTip(t, tip, 2, meshevents.EventKindRedemptionAckRecorded, makerEvent, makerAck)
	log, err = archie.ApplyRedemptionAckRecordedEvent(ctx, &db.EventLogMeta{
		Seq:             2,
		Event:           makerEvent,
		ExpectedTipHash: makerTip,
	}, makerAck)
	if err != nil {
		t.Fatalf("ApplyRedemptionAckRecordedEvent maker error: %v", err)
	}
	requireEventApplyLog(t, log, 2, meshevents.EventKindRedemptionAckRecorded, makerEvent, makerTip, makerAck)
}

type matchFailedUsers struct {
	maker account.AccountID
	taker account.AccountID
}

type matchFailedFixture struct {
	ctx      context.Context
	policy   *db.ReputationOutcomePolicy
	failTime time.Time
}

func newMatchFailedFixture() *matchFailedFixture {
	return &matchFailedFixture{
		ctx:      context.Background(),
		policy:   &db.ReputationOutcomePolicy{MatchLimit: 10, OrderLimit: 10},
		failTime: time.UnixMilli(1670000000000).UTC(),
	}
}

func (f *matchFailedFixture) newMatchFailedUpdate(t *testing.T, status order.MatchStatus, reason db.MatchFailureReason, users matchFailedUsers, makerStatus, takerStatus order.OrderStatus) (*matchPair, *db.MatchFailedUpdate) {
	t.Helper()
	pair := generateMatchWithOrderStatuses(t, status, true, users.maker, users.taker, makerStatus, takerStatus)
	update := &db.MatchFailedUpdate{
		MID:        testMarketMatchID(pair.match),
		FailTimeMS: f.failTime.UnixMilli(),
		Reason:     reason,
	}
	return pair, update
}

func (f *matchFailedFixture) apply(t *testing.T, update *db.MatchFailedUpdate, event []byte, meta *db.EventLogMeta) (*db.EventLogEntry, error) {
	t.Helper()
	if meta == nil {
		meta = &db.EventLogMeta{Event: event}
	} else if meta.Event == nil {
		meta.Event = event
	}
	return archie.ApplyMatchFailedEvent(f.ctx, meta, f.policy, update)
}

func (f *matchFailedFixture) requireMatchActive(t *testing.T, match *order.Match, want bool) {
	t.Helper()
	matchData, err := archie.MatchByID(match.ID(), match.Maker.Base(), match.Maker.Quote())
	if err != nil {
		t.Fatalf("MatchByID error: %v", err)
	}
	if matchData.Active != want {
		t.Fatalf("match active = %v, want %v", matchData.Active, want)
	}
}

func (f *matchFailedFixture) requireMatchOutcome(t *testing.T, user account.AccountID, want db.Outcome) {
	t.Helper()
	_, matches, _, err := archie.GetUserReputationData(f.ctx, user, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData match outcomes error: %v", err)
	}
	if len(matches) != 1 || matches[0].MatchOutcome != want {
		t.Fatalf("match outcomes = %+v, want one %v", matches, want)
	}
}

func (f *matchFailedFixture) requireNoMatchOutcomes(t *testing.T, users ...account.AccountID) {
	t.Helper()
	seen := make(map[account.AccountID]struct{}, len(users))
	for _, user := range users {
		if _, found := seen[user]; found {
			continue
		}
		seen[user] = struct{}{}
		_, matches, _, err := archie.GetUserReputationData(f.ctx, user, 10, 10, 10)
		if err != nil {
			t.Fatalf("GetUserReputationData match outcomes error: %v", err)
		}
		if len(matches) != 0 {
			t.Fatalf("user %v match outcomes = %+v, want none", user, matches)
		}
	}
}

func (f *matchFailedFixture) requireOrderOutcome(t *testing.T, user account.AccountID, oid order.OrderID, canceled bool) {
	t.Helper()
	_, _, orders, err := archie.GetUserReputationData(f.ctx, user, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData order outcomes error: %v", err)
	}
	if len(orders) != 1 || orders[0].OrderID != oid || orders[0].Canceled != canceled {
		t.Fatalf("order outcomes = %+v, want one order %v canceled=%v", orders, oid, canceled)
	}
}

func (f *matchFailedFixture) requireNoOrderOutcomes(t *testing.T, user account.AccountID) {
	t.Helper()
	_, _, orders, err := archie.GetUserReputationData(f.ctx, user, 10, 10, 10)
	if err != nil {
		t.Fatalf("GetUserReputationData order outcomes error: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("order outcomes = %+v, want none", orders)
	}
}

func (f *matchFailedFixture) requireCompletedOrder(t *testing.T, user account.AccountID, oid order.OrderID, wantTime time.Time) {
	t.Helper()
	completed, compTimes, err := archie.CompletedUserOrders(user, 10)
	if err != nil {
		t.Fatalf("CompletedUserOrders error: %v", err)
	}
	if len(completed) != 1 || completed[0] != oid || compTimes[0] != wantTime.UnixMilli() {
		t.Fatalf("completed orders = %v/%v, want %v at %d", completed, compTimes, oid, wantTime.UnixMilli())
	}
}

func (f *matchFailedFixture) requireNoCompletedOrders(t *testing.T, user account.AccountID) {
	t.Helper()
	completed, _, err := archie.CompletedUserOrders(user, 10)
	if err != nil {
		t.Fatalf("CompletedUserOrders error: %v", err)
	}
	if len(completed) != 0 {
		t.Fatalf("completed orders = %+v, want none", completed)
	}
}

func (f *matchFailedFixture) requireOrderStatus(t *testing.T, oid order.OrderID, base, quote uint32, want order.OrderStatus) {
	t.Helper()
	_, status, err := archie.Order(oid, base, quote)
	if err != nil {
		t.Fatalf("Order error: %v", err)
	}
	if status != want {
		t.Fatalf("order %v status = %v, want %v", oid, status, want)
	}
}

func TestApplyMatchFailedEventDurableEffects(t *testing.T) {
	f := newMatchFailedFixture()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "maker fault revokes booked maker and completes taker",
			run: func(t *testing.T) {
				users := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
				pair, update := f.newMatchFailedUpdate(t, order.NewlyMatched, db.MatchFailureMakerNoSwap, users,
					order.OrderStatusBooked, order.OrderStatusExecuted)
				event := []byte("match-failed-event")

				logEntry, err := f.apply(t, update, event, nil)
				if err != nil {
					t.Fatalf("ApplyMatchFailedEvent error: %v", err)
				}
				baseTxData, err := update.EventTxData()
				if err != nil {
					t.Fatalf("EventTxData error: %v", err)
				}
				tip := testEventApplyTipForTxData(t, nil, 1, meshevents.EventKindMatchFailed, event, baseTxData)
				requireEventApplyLogTxData(t, logEntry, 1, meshevents.EventKindMatchFailed, event, tip, baseTxData)

				f.requireMatchActive(t, pair.match, false)

				expectedCancelID := makePseudoCancel(pair.match.Maker.ID(), users.maker,
					pair.match.Maker.Base(), pair.match.Maker.Quote(), f.failTime).ID()
				f.requireMatchOutcome(t, users.maker, db.OutcomeNoSwapAsMaker)
				f.requireOrderOutcome(t, users.maker, expectedCancelID, false)
				f.requireOrderStatus(t, pair.match.Maker.ID(), pair.match.Maker.Base(), pair.match.Maker.Quote(), order.OrderStatusRevoked)

				f.requireCompletedOrder(t, users.taker, pair.match.Taker.ID(), f.failTime)
				f.requireOrderOutcome(t, users.taker, pair.match.Taker.ID(), false)
			},
		},
		{
			name: "event log divergence rolls back match order and reputation writes",
			run: func(t *testing.T) {
				users := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
				pair, update := f.newMatchFailedUpdate(t, order.NewlyMatched, db.MatchFailureMakerNoSwap, users,
					order.OrderStatusBooked, order.OrderStatusExecuted)

				_, err := f.apply(t, update, []byte("match-failed-bad-tip"), &db.EventLogMeta{
					Seq:             1,
					ExpectedTipHash: wrongEventTip(),
				})
				var divergence *db.EventLogDivergenceError
				if !errors.As(err, &divergence) {
					t.Fatalf("ApplyMatchFailedEvent error = %T %[1]v, want EventLogDivergenceError", err)
				}
				assertEventLogFrontier(t, 0, nil)

				f.requireMatchActive(t, pair.match, true)
				f.requireOrderStatus(t, pair.match.Maker.ID(), pair.match.Maker.Base(), pair.match.Maker.Quote(), order.OrderStatusBooked)
				f.requireNoMatchOutcomes(t, users.maker, users.taker)
				f.requireNoOrderOutcomes(t, users.maker)
				f.requireNoOrderOutcomes(t, users.taker)
				f.requireNoCompletedOrders(t, users.taker)
			},
		},
		{
			name: "shared maker order completes only after last active match fails",
			run: func(t *testing.T) {
				makerAcct := randomAccountID()
				maker := newLimitOrder(false, 4500000, 2, order.StandingTiF, 0)
				maker.P.AccountID = makerAcct
				taker1 := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)
				taker2 := newLimitOrder(true, 4480000, 1, order.ImmediateTiF, 20)
				for _, ord := range []order.Order{maker, taker1, taker2} {
					if err := storeOrderForTest(archie, ord, 132412341, 1000, order.OrderStatusExecuted); err != nil {
						t.Fatalf("StoreOrder error: %v", err)
					}
				}

				match1 := newMatch(maker, taker1, taker1.Quantity, order.EpochID{Idx: 132412341, Dur: 1000})
				match1.Status = order.MakerSwapCast
				if err := insertMatchForTest(match1); err != nil {
					t.Fatalf("insertMatchForTest match1 error: %v", err)
				}
				match2 := newMatch(maker, taker2, taker2.Quantity, order.EpochID{Idx: 132412342, Dur: 1000})
				match2.Status = order.MakerSwapCast
				if err := insertMatchForTest(match2); err != nil {
					t.Fatalf("insertMatchForTest match2 error: %v", err)
				}

				applyFailure := func(t *testing.T, match *order.Match, at time.Time) {
					t.Helper()
					mid := testMarketMatchID(match)
					_, err := f.apply(t, &db.MatchFailedUpdate{
						MID:        mid,
						FailTimeMS: at.UnixMilli(),
						Reason:     db.MatchFailureTakerNoSwap,
					}, mid.MatchID[:], nil)
					if err != nil {
						t.Fatalf("ApplyMatchFailedEvent error: %v", err)
					}
				}

				applyFailure(t, match1, f.failTime)
				f.requireNoCompletedOrders(t, makerAcct)
				f.requireNoCompletedOrders(t, taker1.User())

				secondFailTime := f.failTime.Add(time.Second)
				applyFailure(t, match2, secondFailTime)
				f.requireCompletedOrder(t, makerAcct, maker.ID(), secondFailTime)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if err := cleanTables(archie.db); err != nil {
				t.Fatalf("cleanTables: %v", err)
			}
			tt.run(t)
		})
	}
}

func TestApplyMatchFailedEventReasonMapping(t *testing.T) {
	f := newMatchFailedFixture()

	tests := []struct {
		name        string
		status      order.MatchStatus
		reason      db.MatchFailureReason
		sameUser    bool
		wantUser    func(matchFailedUsers) account.AccountID
		wantOutcome db.Outcome
		wantErr     bool
	}{
		{
			name:        "newly matched maker fault",
			status:      order.NewlyMatched,
			reason:      db.MatchFailureMakerNoSwap,
			wantUser:    func(users matchFailedUsers) account.AccountID { return users.maker },
			wantOutcome: db.OutcomeNoSwapAsMaker,
		},
		{
			name:        "newly matched taker address fault",
			status:      order.NewlyMatched,
			reason:      db.MatchFailureTakerNoAddress,
			wantUser:    func(users matchFailedUsers) account.AccountID { return users.taker },
			wantOutcome: db.OutcomeNoAddrAsTaker,
		},
		{
			name:        "maker swap cast taker fault",
			status:      order.MakerSwapCast,
			reason:      db.MatchFailureTakerNoSwap,
			wantUser:    func(users matchFailedUsers) account.AccountID { return users.taker },
			wantOutcome: db.OutcomeNoSwapAsTaker,
		},
		{
			name:        "taker swap cast maker fault",
			status:      order.TakerSwapCast,
			reason:      db.MatchFailureMakerNoRedeem,
			wantUser:    func(users matchFailedUsers) account.AccountID { return users.maker },
			wantOutcome: db.OutcomeNoRedeemAsMaker,
		},
		{
			name:        "maker redeemed taker fault",
			status:      order.MakerRedeemed,
			reason:      db.MatchFailureTakerNoRedeem,
			wantUser:    func(users matchFailedUsers) account.AccountID { return users.taker },
			wantOutcome: db.OutcomeNoRedeemAsTaker,
		},
		{
			name:   "no user fault records no match reputation",
			status: order.MakerSwapCast,
			reason: db.MatchFailureNoFaultMakerSwapCast,
		},
		{
			name:     "same account suppresses match reputation",
			status:   order.MakerSwapCast,
			reason:   db.MatchFailureTakerNoSwap,
			sameUser: true,
		},
		{
			name:    "invalid reason enum rolls back",
			status:  order.NewlyMatched,
			reason:  db.MatchFailureReasonInvalid,
			wantErr: true,
		},
		{
			name:    "reason status mismatch rolls back",
			status:  order.MakerSwapCast,
			reason:  db.MatchFailureTakerNoAddress,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if err := cleanTables(archie.db); err != nil {
				t.Fatalf("cleanTables: %v", err)
			}
			users := matchFailedUsers{maker: randomAccountID(), taker: randomAccountID()}
			if tt.sameUser {
				users.taker = users.maker
			}
			pair, update := f.newMatchFailedUpdate(t, tt.status, tt.reason, users,
				order.OrderStatusExecuted, order.OrderStatusExecuted)

			_, err := f.apply(t, update, []byte(tt.name), nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ApplyMatchFailedEvent succeeded, want error")
				}
				f.requireMatchActive(t, pair.match, true)
				return
			}
			if err != nil {
				t.Fatalf("ApplyMatchFailedEvent error: %v", err)
			}

			if tt.wantUser == nil {
				f.requireNoMatchOutcomes(t, users.maker, users.taker)
				return
			}
			f.requireMatchOutcome(t, tt.wantUser(users), tt.wantOutcome)
		})
	}
}

func TestMatchByID(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Make a perfect 1 lot match.
	limitBuyStanding := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	base, quote := limitBuyStanding.Base(), limitBuyStanding.Quote()

	// Store it.
	epochID := order.EpochID{132412341, 1000}
	match := newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	err := insertMatchForTest(match)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}

	tests := []struct {
		name        string
		matchID     order.MatchID
		base, quote uint32
		wantedErr   error
	}{
		{
			"ok",
			match.ID(),
			base, quote,
			nil,
		},
		{
			"no order",
			order.MatchID{},
			base, quote,
			db.ArchiveError{Code: db.ErrUnknownMatch},
		},
		{
			"bad market",
			match.ID(),
			base, base,
			db.ArchiveError{Code: db.ErrUnsupportedMarket},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matchData, err := archie.MatchByID(tt.matchID, tt.base, tt.quote)
			if !db.SameErrorTypes(err, tt.wantedErr) {
				t.Fatal(err)
			}
			if err == nil && matchData.ID != tt.matchID {
				t.Errorf("Retrieved match with ID %v, expected %v", matchData.ID, tt.matchID)
			}
		})
	}
}

func TestSwapDataFullByID(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	got, err := archie.SwapDataFullByID(order.MatchID{})
	if err != nil {
		t.Fatalf("missing match: %v", err)
	}
	if got != nil {
		t.Fatalf("missing match returned %+v", got)
	}

	user1 := randomAccountID()
	user2 := randomAccountID()
	mp := generateMatch(t, order.MakerSwapCast, true, user1, user2)
	mid := mp.match.ID()

	got, err = archie.SwapDataFullByID(mid)
	if err != nil {
		t.Fatalf("SwapDataFullByID: %v", err)
	}
	if got == nil {
		t.Fatal("SwapDataFullByID returned nil")
	}
	base, quote := mp.match.Maker.Base(), mp.match.Maker.Quote()
	md, err := archie.MatchByID(mid, base, quote)
	if err != nil {
		t.Fatalf("MatchByID: %v", err)
	}
	_, sd, err := archie.SwapData(db.MarketMatchID{MatchID: mid, Base: base, Quote: quote})
	if err != nil {
		t.Fatalf("SwapData: %v", err)
	}
	if got.ID != md.ID || got.Base != base || got.Quote != quote {
		t.Fatalf("got ID %v market %d-%d, want %v %d-%d", got.ID, got.Base, got.Quote, md.ID, base, quote)
	}
	if !bytes.Equal(got.ContractACoinID, sd.ContractACoinID) || !bytes.Equal(got.ContractA, sd.ContractA) {
		t.Fatalf("SwapData contract mismatch: %+v vs %+v", got.SwapData, sd)
	}
}

func TestMarketMatches(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Make a perfect 1 lot match.
	limitBuyStanding := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	base, quote := limitBuyStanding.Base(), limitBuyStanding.Quote()

	// Store it.
	epochID := order.EpochID{132412341, 1000}
	match := newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	err := insertMatchForTest(match)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}
	// Make another perfect 1 lot match.
	limitBuyStanding = newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate = newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	// Store it.
	match = newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	err = insertMatchForTest(match)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}
	archie.setMatchInactive(archie.db, db.MarketMatchID{
		MatchID: match.ID(),
		Base:    base,
		Quote:   quote,
	}, false)

	// This one has txns.
	mktMatchID := db.MarketMatchID{
		MatchID: match.ID(),
		Base:    limitBuyStanding.Base(),
		Quote:   limitBuyStanding.Quote(),
	}
	midWithCoins := mktMatchID.MatchID
	MakerSwap, MakerContract := encode.RandomBytes(36), encode.RandomBytes(50)
	err = saveContractForTest(mktMatchID, true, MakerContract, MakerSwap, 0)
	if err != nil {
		t.Fatalf("saveContractForTest (maker) error: %v", err)
	}

	TakerSwap, TakerContract := encode.RandomBytes(36), encode.RandomBytes(50)
	err = saveContractForTest(mktMatchID, false, TakerContract, TakerSwap, 0)
	if err != nil {
		t.Fatalf("saveContractForTest/saveRedeemForTest error: %v", err)
	}

	MakerRedeem, Secret := encode.RandomBytes(36), encode.RandomBytes(32)
	err = saveRedeemForTest(mktMatchID, true, MakerRedeem, Secret, 0)
	if err != nil {
		t.Fatalf("saveContractForTest/saveRedeemForTest error: %v", err)
	}
	// TakerRedeem not stored.

	// Make another perfect 1 lot match on another market.
	limitBuyStanding = newLimitOrderWithAssets(false, 4500000, 1, order.StandingTiF, 0, AssetBTC, AssetLTC)
	limitSellImmediate = newLimitOrderWithAssets(true, 4490000, 1, order.ImmediateTiF, 10, AssetBTC, AssetLTC)

	// Store it.
	match = newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	err = insertMatchForTest(match)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}

	// Only active.
	matchData, err := archie.MarketMatches(base, quote)
	if err != nil {
		t.Fatal(err)
	}
	if len(matchData) != 1 {
		t.Errorf("Retrieved %d matches for market, expected 1.", len(matchData))
	}
	// Include inactive (true), and no limit (-1).
	matchData = []*db.MatchDataWithCoins{}
	N, err := archie.MarketMatchesStreaming(base, quote, true, -1, func(md *db.MatchDataWithCoins) error {
		matchData = append(matchData, md)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if N != len(matchData) {
		t.Errorf("Retrieved %d matches for market, but method claimed %d.", len(matchData), N)
	}
	if len(matchData) != 2 {
		t.Errorf("Retrieved %d matches for market, expected 2.", len(matchData))
	}

	// Find the match with the stored coins and verify them.
	var found bool
	for _, md := range matchData {
		if md.ID == midWithCoins {
			found = true
			if !bytes.Equal(md.MakerSwapCoin, MakerSwap) {
				t.Errorf("Wrong maker swap coin %x, wanted %x", md.MakerSwapCoin, MakerSwap)
			}
			if !bytes.Equal(md.TakerSwapCoin, TakerSwap) {
				t.Errorf("Wrong taker swap coin %x, wanted %x", md.TakerSwapCoin, TakerSwap)
			}
			if !bytes.Equal(md.MakerRedeemCoin, MakerRedeem) {
				t.Errorf("Wrong maker redeem coin %x, wanted %x", md.MakerRedeemCoin, MakerRedeem)
			}
			if len(md.TakerRedeemCoin) > 0 {
				t.Errorf("got taker redeem coin %x, but expected none", md.TakerRedeemCoin)
			}
			break
		}
	}
	if !found {
		t.Errorf("failed to find match with the coins")
	}

	// Bad Market.
	matchData, err = archie.MarketMatches(base, base)
	noMktErr := new(db.ArchiveError)
	if !errors.As(err, noMktErr) || noMktErr.Code != db.ErrUnsupportedMarket {
		t.Fatalf("incorrect error for unsupported market: %v", err)
	}
}

type matchPair struct {
	match  *order.Match
	status *db.MatchStatus
}

func generateMatch(t *testing.T, matchStatus order.MatchStatus, active bool, makerBuyer, takerSeller account.AccountID, epochIdx ...uint64) *matchPair {
	return generateMatchWithOrderStatuses(t, matchStatus, active, makerBuyer, takerSeller,
		order.OrderStatusExecuted, order.OrderStatusExecuted, epochIdx...)
}

func generateMatchWithOrderStatuses(t *testing.T, matchStatus order.MatchStatus, active bool, makerBuyer, takerSeller account.AccountID, makerStatus, takerStatus order.OrderStatus, epochIdx ...uint64) *matchPair {
	t.Helper()
	loBuy := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	loBuy.P.AccountID = makerBuyer
	loSell := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)
	loSell.P.AccountID = takerSeller

	epIdx := uint64(132412341)
	if len(epochIdx) > 0 {
		epIdx = epochIdx[0]
	}
	epochID := order.EpochID{epIdx, 1000}

	err := storeOrderForTest(archie, loBuy, int64(epochID.Idx), int64(epochID.Dur), makerStatus)
	if err != nil {
		t.Fatalf("failed to store order: %v", err)
	}
	err = storeOrderForTest(archie, loSell, int64(epochID.Idx), int64(epochID.Dur), takerStatus)
	if err != nil {
		t.Fatalf("failed to store order: %v", err)
	}

	match := newMatch(loBuy, loSell, loSell.Quantity, epochID)
	match.Status = matchStatus
	err = insertMatchForTest(match)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}
	matchID := match.ID()
	mktMatchID := db.MarketMatchID{
		MatchID: matchID,
		Base:    loBuy.Base(),
		Quote:   loBuy.Quote(),
	}
	// Just alternate the active state.
	status := &db.MatchStatus{
		Status: matchStatus,
		Active: active,
	}
	if !active {
		archie.setMatchInactive(archie.db, mktMatchID, false)
	}
	for iStatus := order.NewlyMatched; iStatus <= matchStatus; iStatus++ {
		switch iStatus {
		case order.MakerSwapCast:
			status.MakerContract = encode.RandomBytes(50)
			status.MakerSwap = encode.RandomBytes(36)
			err := saveContractForTest(mktMatchID, true, status.MakerContract, status.MakerSwap, 0)
			if err != nil {
				t.Fatalf("saveContractForTest (maker) error: %v", err)
			}
		case order.TakerSwapCast:
			status.TakerContract = encode.RandomBytes(50)
			status.TakerSwap = encode.RandomBytes(36)
			err := saveContractForTest(mktMatchID, false, status.TakerContract, status.TakerSwap, 0)
			if err != nil {
				t.Fatalf("saveContractForTest/saveRedeemForTest error: %v", err)
			}
		case order.MakerRedeemed:
			status.MakerRedeem = encode.RandomBytes(36)
			status.Secret = encode.RandomBytes(32)
			err := saveRedeemForTest(mktMatchID, true, status.MakerRedeem, status.Secret, 0)
			if err != nil {
				t.Fatalf("saveContractForTest/saveRedeemForTest error: %v", err)
			}
		case order.MatchComplete:
			status.TakerRedeem = encode.RandomBytes(36)
			err := saveRedeemForTest(mktMatchID, false, status.TakerRedeem, nil, 0)
			if err != nil {
				t.Fatalf("saveContractForTest/saveRedeemForTest error: %v", err)
			}
		}
	}
	return &matchPair{match: match, status: status}
}

func TestCompletedAndAtFaultMatchStats(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	epIdx := uint64(132412341)
	nextIdx := func() uint64 {
		epIdx++
		return epIdx
	}

	maker, taker := randomAccountID(), randomAccountID()
	matches := []*matchPair{
		generateMatch(t, order.TakerSwapCast, false, maker, taker, nextIdx()), // 0: failed, maker fault
		generateMatch(t, order.MatchComplete, false, maker, taker, nextIdx()), // 1: success
		generateMatch(t, order.MakerRedeemed, true, maker, taker, nextIdx()),  // 2: still active, but maker success
		generateMatch(t, order.MakerRedeemed, false, maker, taker, nextIdx()), // 3: failed, maker success, taker fault
		generateMatch(t, order.MakerRedeemed, false, maker, maker, nextIdx()), // 4: failed, maker fault (no same-user maker success until MatchComplete)
		generateMatch(t, order.MakerSwapCast, false, maker, taker, nextIdx()), // 5: failed, taker fault
		generateMatch(t, order.NewlyMatched, false, maker, taker, nextIdx()),  // 6: failed, maker fault
	}

	// Make a perfect 1 lot match in different market (BTC-LTC).
	limitBuy := newLimitOrder(false, 4500000, 1, order.StandingTiF, 20)
	limitBuy.BaseAsset, limitBuy.QuoteAsset = AssetBTC, AssetLTC
	limitBuy.AccountID = maker
	limitSell := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 30)
	limitSell.BaseAsset, limitSell.QuoteAsset = AssetBTC, AssetLTC
	taker2 := randomAccountID()
	limitSell.AccountID = taker2
	matchLTC := newMatch(limitBuy, limitSell, limitSell.Quantity, order.EpochID{nextIdx(), 1000})
	matchLTC.Status = order.MatchComplete
	err := insertMatchForTest(matchLTC)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}
	archie.setMatchInactive(archie.db, db.MarketMatchID{
		MatchID: matchLTC.ID(),
		Base:    limitBuy.Base(),
		Quote:   limitBuy.Quote(),
	}, false)
	// 7: success
	matches = append(matches, &matchPair{
		match: matchLTC,
		status: &db.MatchStatus{
			Active: false,
			Status: matchLTC.Status,
		},
	})
	// TODO: update with a forgiven one

	epochTime := func(mp *matchPair) int64 {
		return mp.match.Epoch.End().UnixMilli()
	}

	tests := []struct {
		name         string
		acctID       account.AccountID
		wantOutcomes []*db.MatchOutcome
		wantedErr    error
	}{
		{
			"maker",
			maker,
			[]*db.MatchOutcome{ // ascending by time (MatchID field TODO)
				{
					Status: matches[0].match.Status,
					Fail:   true,
					Time:   epochTime(matches[0]),
				}, {
					Status: matches[1].match.Status,
					Fail:   false,
					Time:   epochTime(matches[1]),
				}, {
					Status: matches[2].match.Status,
					Fail:   false,
					Time:   epochTime(matches[2]),
				}, {
					Status: matches[3].match.Status,
					Fail:   false,
					Time:   epochTime(matches[3]),
				}, {
					Status: matches[4].match.Status,
					Fail:   true,
					Time:   epochTime(matches[4]),
				}, {
					Status: matches[6].match.Status,
					Fail:   true,
					Time:   epochTime(matches[6]),
				}, {
					Status: matches[7].match.Status,
					Fail:   false,
					Time:   epochTime(matches[7]),
				},
			},
			nil,
		},
		{
			"taker",
			taker,
			[]*db.MatchOutcome{
				{
					Status: matches[1].match.Status,
					Fail:   false,
					Time:   epochTime(matches[1]),
				}, {
					Status: matches[3].match.Status,
					Fail:   true,
					Time:   epochTime(matches[3]),
				}, {
					Status: matches[5].match.Status,
					Fail:   true,
					Time:   epochTime(matches[5]),
				},
			},
			nil,
		},
		{
			"nope",
			randomAccountID(),
			nil,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcomes, err := archie.CompletedAndAtFaultMatchStats(tt.acctID, 60)
			if err != tt.wantedErr {
				t.Fatal(err)
			}
			if len(outcomes) != len(tt.wantOutcomes) {
				t.Errorf("Retrieved %d match outcomes for user %v, expected %d.", len(outcomes), tt.acctID, len(tt.wantOutcomes))
			}
			for i, mo := range tt.wantOutcomes {
				if outcomes[i].Time != mo.Time || outcomes[i].Status != mo.Status || outcomes[i].Fail != mo.Fail {
					t.Log(outcomes[i])
					t.Log(mo)
					t.Errorf("wrong %d", i)
				}
			}
		})
	}
}

func TestUserMatchFails(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	epIdx := uint64(132412341)
	nextIdx := func() uint64 {
		epIdx++
		return epIdx
	}

	user, otherUser := randomAccountID(), randomAccountID()
	matches := []*matchPair{
		generateMatch(t, order.TakerSwapCast, false, user, otherUser, nextIdx()), // 0: failed, user fault
		generateMatch(t, order.MatchComplete, false, user, otherUser, nextIdx()), // 1: success
		generateMatch(t, order.MakerRedeemed, true, user, otherUser, nextIdx()),  // 2: still active, but user success
		generateMatch(t, order.MakerRedeemed, false, otherUser, user, nextIdx()), // 3: failed, user success, otherUser fault
		generateMatch(t, order.MakerSwapCast, false, otherUser, user, nextIdx()), // 5: failed, user fault
		generateMatch(t, order.NewlyMatched, false, otherUser, user, nextIdx()),  // 6: failed, otherUser fault
	}
	// Put one of them on another market
	m4 := matches[4]
	m4.match.Maker.Prefix().BaseAsset = AssetBTC
	m4.match.Maker.Prefix().QuoteAsset = AssetLTC
	m4.match.Taker.Prefix().BaseAsset = AssetBTC
	m4.match.Taker.Prefix().QuoteAsset = AssetLTC
	for _, m := range matches {
		err := insertMatchForTest(m.match)
		if err != nil {
			t.Fatalf("insertMatchForTest() failed: %v", err)
		}
	}
	fails, err := archie.UserMatchFails(user, 100)
	if err != nil {
		t.Fatalf("UserMatchFails() failed: %v", err)
	}
check:
	for _, i := range []int{0, 3, 4} {
		matchID := matches[i].match.ID()
		for _, fail := range fails {
			if fail.ID == matchID {
				continue check
			}
		}
		t.Fatalf("expected to find fail for match at index %d, but did not", i)
	}
}

func TestAllActiveUserMatches(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Make a perfect 1 lot match.
	limitBuyStanding := newLimitOrder(false, 4500000, 1, order.StandingTiF, 0)
	limitSellImmediate := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 10)

	// Make it complete and store it.
	epochID := order.EpochID{132412341, 1000}
	// maker buy (quote swap asset), taker sell (base swap asset)
	match := newMatch(limitBuyStanding, limitSellImmediate, limitSellImmediate.Quantity, epochID)
	match.Status = order.TakerSwapCast // failed here
	err := insertMatchForTest(match)   // active by default
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}
	err = archie.setMatchInactive(archie.db, db.MatchID(match), false) // set inactive, not forgiven
	if err != nil {
		t.Fatalf("setMatchInactive() failed: %v", err)
	}

	// Make a perfect 1 lot match, same parties.
	limitBuyStanding2 := newLimitOrder(false, 4500000, 1, order.StandingTiF, 20)
	limitBuyStanding2.AccountID = limitBuyStanding.AccountID
	limitSellImmediate2 := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 30)
	limitSellImmediate2.AccountID = limitSellImmediate.AccountID

	// Store it.
	epochID2 := order.EpochID{132412342, 1000}
	// maker buy (quote swap asset), taker sell (base swap asset)
	match2 := newMatch(limitBuyStanding2, limitSellImmediate2, limitSellImmediate2.Quantity, epochID2)
	err = insertMatchForTest(match2)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}

	// Make a perfect 1 lot BTC-LTC match.
	limitBuyStanding3 := newLimitOrder(false, 4500000, 1, order.StandingTiF, 20)
	limitBuyStanding3.BaseAsset = AssetBTC
	limitBuyStanding3.QuoteAsset = AssetLTC
	limitBuyStanding3.AccountID = limitBuyStanding.AccountID
	limitSellImmediate3 := newLimitOrder(true, 4490000, 1, order.ImmediateTiF, 30)
	limitSellImmediate3.BaseAsset = AssetBTC
	limitSellImmediate3.QuoteAsset = AssetLTC
	limitSellImmediate3.AccountID = limitSellImmediate.AccountID

	// Store it.
	epochID3 := order.EpochID{132412342, 1000}
	match3 := newMatch(limitBuyStanding3, limitSellImmediate3, limitSellImmediate3.Quantity, epochID3)
	err = insertMatchForTest(match3)
	if err != nil {
		t.Fatalf("insertMatchForTest() failed: %v", err)
	}

	tests := []struct {
		name        string
		acctID      account.AccountID
		numExpected int
		wantMatch   []*order.Match
		wantedErr   error
	}{
		{
			"ok maker",
			limitBuyStanding.User(),
			2,
			[]*order.Match{match2, match3},
			nil,
		},
		{
			"ok taker",
			limitSellImmediate.User(),
			2,
			[]*order.Match{match2, match3},
			nil,
		},
		{
			"nope",
			randomAccountID(),
			0,
			nil,
			nil,
		},
	}

	idInMatchSlice := func(mid order.MatchID, ms []*order.Match) int {
		for i := range ms {
			if ms[i].ID() == mid {
				return i
			}
		}
		return -1
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userMatch, err := archie.AllActiveUserMatches(tt.acctID)
			if err != tt.wantedErr {
				t.Fatal(err)
			}
			if len(userMatch) != tt.numExpected {
				t.Errorf("Retrieved %d matches for user %v, expected %d.", len(userMatch), tt.acctID, tt.numExpected)
			}
			for _, match := range userMatch {
				loc := idInMatchSlice(match.ID, tt.wantMatch)
				if loc == -1 {
					t.Errorf("Unknown match ID retrieved: %v.", match.ID)
					continue
				}
				if tt.wantMatch[loc].FeeRateBase != match.BaseRate {
					t.Errorf("incorrect base fee rate. got %d, want %d",
						match.BaseRate, tt.wantMatch[loc].FeeRateBase)
				}
				if tt.wantMatch[loc].FeeRateQuote != match.QuoteRate {
					t.Errorf("incorrect quote fee rate. got %d, want %d",
						match.QuoteRate, tt.wantMatch[loc].FeeRateQuote)
				}
				if tt.wantMatch[loc].Epoch.End() != match.Epoch.End() {
					t.Errorf("incorrect match time. got %v, want %v",
						match.Epoch.End(), tt.wantMatch[loc].Epoch.End())
				}
				if tt.wantMatch[loc].Taker.Trade().Address != match.TakerAddr {
					t.Errorf("incorrect counterparty swap address. got %v, want %v",
						match.TakerAddr, tt.wantMatch[loc].Taker.Trade().Address)
				}
				if tt.wantMatch[loc].Maker.Address != match.MakerAddr {
					t.Errorf("incorrect counterparty swap address. got %v, want %v",
						match.MakerAddr, tt.wantMatch[loc].Maker.Address)
				}
			}
		})
	}
}

func TestActiveSwaps(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	swapsDetails, err := archie.ActiveSwaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(swapsDetails) > 0 {
		t.Fatalf("got details for %d swaps, expected 0", len(swapsDetails))
	}

	user1 := randomAccountID()
	user2 := randomAccountID()
	match := generateMatch(t, order.MakerRedeemed, true, user1, user2)

	swapsDetails, err = archie.ActiveSwaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(swapsDetails) != 1 {
		t.Fatalf("got details for %d swaps, expected 1", len(swapsDetails))
	}
	swapDetails := swapsDetails[0]

	taker, _, err := archie.Order(swapDetails.MatchData.Taker, swapDetails.Base, swapDetails.Quote)
	if err != nil {
		t.Fatalf("Failed to load taker order: %v", err)
	}
	if taker.ID() != swapDetails.MatchData.Taker {
		t.Fatalf("Failed to load order %v, computed ID %v instead", swapDetails.MatchData.Taker, taker.ID())
	}
	if match.match.Taker.ID() != swapDetails.MatchData.Taker {
		t.Fatalf("Failed to load order %v, computed ID %v instead", swapDetails.MatchData.Taker, taker.ID())
	}

	maker, _, err := archie.Order(swapDetails.MatchData.Maker, swapDetails.Base, swapDetails.Quote)
	if err != nil {
		t.Fatalf("Failed to load maker order: %v", err)
	}
	if maker.ID() != swapDetails.MatchData.Maker {
		t.Fatalf("Failed to load order %v, computed ID %v instead", swapDetails.MatchData.Maker, maker.ID())
	}
	if match.match.Maker.ID() != swapDetails.MatchData.Maker {
		t.Fatalf("Failed to load order %v, computed ID %v instead", swapDetails.MatchData.Maker, maker.ID())
	}

	if match.match.Rate != swapDetails.Rate {
		t.Fatalf("wrong rate loaded, got %d want %d", swapDetails.Rate, match.match.Rate)
	}
	if match.match.Quantity != swapDetails.Quantity {
		t.Fatalf("wrong quantity loaded, got %d want %d", swapDetails.Quantity, match.match.Quantity)
	}
	makerLO, ok := maker.(*order.LimitOrder)
	if !ok {
		t.Fatalf("Maker order was not a limit order: %T", maker)
	}

	matchBack := &order.Match{
		Taker:        taker,
		Maker:        makerLO,
		Quantity:     swapDetails.Quantity,
		Rate:         swapDetails.Rate,
		FeeRateBase:  swapDetails.BaseRate,
		FeeRateQuote: swapDetails.QuoteRate,
		Epoch:        swapDetails.Epoch,
		Status:       swapDetails.Status,
		Sigs: order.Signatures{ // not really needed
			MakerMatch:  swapDetails.SwapData.SigMatchAckMaker,
			TakerMatch:  swapDetails.SwapData.SigMatchAckTaker,
			MakerAudit:  swapDetails.SwapData.ContractAAckSig,
			TakerAudit:  swapDetails.SwapData.ContractBAckSig,
			TakerRedeem: swapDetails.SwapData.RedeemAAckSig,
		},
	}

	wantMid := match.match.ID()
	if wantMid != swapDetails.MatchData.ID {
		t.Fatalf("incorrect match ID %v, expected %v", swapDetails.MatchData.ID, wantMid)
	}
	// recompute the match ID from the loaded orders (their computed IDs), match rate, qty, etc.
	if wantMid != matchBack.ID() {
		t.Fatalf("Failed to reconstruct Match %v, computed ID %v instead", matchBack.ID(), wantMid)
	}
}

func TestMatchStatuses(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	// Unknown market
	aid := randomAccountID()
	var mid order.MatchID
	copy(mid[:], encode.RandomBytes(32))
	_, err := archie.MatchStatuses(aid, 100, 101, []order.MatchID{mid})
	noMktErr := new(db.ArchiveError)
	if !errors.As(err, noMktErr) || noMktErr.Code != db.ErrUnsupportedMarket {
		t.Fatalf("incorrect error for unsupported market: %v", err)
	}

	user1 := randomAccountID()
	user2 := randomAccountID()

	matches := []*matchPair{
		generateMatch(t, order.NewlyMatched, true, user1, user2),                           // 0
		generateMatch(t, order.MakerSwapCast, false, user1, user2),                         // 1
		generateMatch(t, order.TakerSwapCast, true, user1, user2),                          // 2
		generateMatch(t, order.MakerRedeemed, true, user1, user2),                          // 3
		generateMatch(t, order.MatchComplete, false, user1, user2),                         // 4 -- inactive via SaveRedeemB
		generateMatch(t, order.MakerRedeemed, false, randomAccountID(), randomAccountID()), // 5
	}

	idList := func(idxs ...int) []order.MatchID {
		ids := make([]order.MatchID, 0, len(idxs))
		for _, i := range idxs {
			ids = append(ids, matches[i].match.ID())
		}
		return ids
	}

	tests := []struct {
		name string
		user account.AccountID
		req  []order.MatchID
		exp  []int // matches index
	}{
		// user 1: 1 hit
		{
			name: "find1",
			user: user1,
			req:  idList(0),
			exp:  []int{0},
		},
		// user 1: 1 hit + 1 miss.
		{
			name: "find1-miss1",
			user: user1,
			req:  idList(1, 5),
			exp:  []int{1},
		},
		// user 2 hit 4
		{
			name: "find4",
			user: user2,
			req:  idList(0, 1, 2, 3),
			exp:  []int{0, 1, 2, 3},
		},
	}

	for _, tt := range tests {
		statuses, err := archie.MatchStatuses(tt.user, AssetDCR, AssetBTC, tt.req)
		if err != nil {
			t.Fatalf("%s: error getting order statuses: %v", tt.name, err)
		}
		if len(statuses) != len(tt.exp) {
			t.Fatalf("%s: wrongs number of statuses returned. expected %d, got %d", tt.name, len(tt.exp), len(statuses))
		}
	top:
		for _, expIdx := range tt.exp {
			matchPair := matches[expIdx]
			expStatus := matchPair.status
			matchID := matchPair.match.ID()
			// Find the status
			for _, status := range statuses {
				if status.ID != matchID {
					continue
				}
				if status.Status != expStatus.Status {
					t.Fatalf("%s: expIdx = %d, wrong status. expected %s, got %s", tt.name, expIdx, expStatus.Status, status.Status)
				}
				if !bytes.Equal(status.MakerContract, expStatus.MakerContract) {
					t.Fatalf("%s: wrong MakerContract. expected %x, got %x", tt.name, expStatus.MakerContract, status.MakerContract)
				}
				if !bytes.Equal(status.TakerContract, expStatus.TakerContract) {
					t.Fatalf("%s: wrong TakerContract. expected %x, got %x", tt.name, expStatus.TakerContract, status.TakerContract)
				}
				if !bytes.Equal(status.MakerSwap, expStatus.MakerSwap) {
					t.Fatalf("%s: wrong MakerSwap. expected %x, got %x", tt.name, expStatus.MakerSwap, status.MakerSwap)
				}
				if !bytes.Equal(status.TakerSwap, expStatus.TakerSwap) {
					t.Fatalf("%s: wrong TakerSwap. expected %x, got %x", tt.name, expStatus.TakerSwap, status.TakerSwap)
				}
				if !bytes.Equal(status.MakerRedeem, expStatus.MakerRedeem) {
					t.Fatalf("%s: wrong MakerRedeem. expected %x, got %x", tt.name, expStatus.MakerRedeem, status.MakerRedeem)
				}
				if !bytes.Equal(status.TakerRedeem, expStatus.TakerRedeem) {
					t.Fatalf("%s: wrong TakerRedeem. expected %x, got %x", tt.name, expStatus.TakerRedeem, status.TakerRedeem)
				}
				if !bytes.Equal(status.Secret, expStatus.Secret) {
					t.Fatalf("%s: wrong Secret. expected %x, got %x", tt.name, expStatus.Secret, status.Secret)
				}
				if status.Active != expStatus.Active {
					t.Fatalf("%s: wrong Active. expected %t, got %t", tt.name, expStatus.Active, status.Active)
				}
				continue top
			}
			t.Fatalf("%s: expected match at index %d not found in results", tt.name, expIdx)
		}
	}

}

func TestEpochReport(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	lastRate, err := archie.LastEpochRate(42, 0)
	if err != nil {
		t.Fatalf("error getting last epoch rate from empty table (should be err = nil, rate = 0): %v", err)
	}
	if lastRate != 0 {
		t.Fatalf("wrong initial last rate. expected 0, got %d", lastRate)
	}

	var epochIdx, epochDur int64 = 13245678, 6000
	err = archie.insertEpoch(archie.db, &db.EpochResults{
		MktBase:     42,
		MktQuote:    0,
		Idx:         epochIdx,
		Dur:         epochDur,
		MatchVolume: 1,
		HighRate:    2,
		LowRate:     3,
		StartRate:   4,
		EndRate:     5,
		QuoteVolume: 6,
	})

	if err != nil {
		t.Fatalf("error inserting first epoch: %v", err)
	}

	startStamp := uint64(epochIdx * epochDur)
	endStamp := startStamp + uint64(epochDur)
	candle := &candles.Candle{
		StartStamp:  startStamp,
		EndStamp:    endStamp,
		MatchVolume: 1,
		HighRate:    2,
		LowRate:     3,
		StartRate:   4,
		EndRate:     5,
		QuoteVolume: 6,
	}
	addCandles := make([]*candles.Candle, 3)
	addCandles[0] = candle

	lastRate, err = archie.LastEpochRate(42, 0)
	if err != nil {
		t.Fatalf("error getting last epoch rate from after first epoch: %v", err)
	}
	if lastRate != 5 {
		t.Fatalf("wrong first epoch last rate. expected 5, got %d", lastRate)
	}

	// Trying for the same epoch should violate a primary key constraint.
	err = archie.insertEpoch(archie.db, &db.EpochResults{
		MktBase:  42,
		MktQuote: 0,
		Idx:      epochIdx,
		Dur:      epochDur,
	})
	if err == nil {
		t.Fatalf("no error for duplicate epoch")
	}

	err = archie.insertEpoch(archie.db, &db.EpochResults{
		MktBase:     42,
		MktQuote:    0,
		Idx:         epochIdx + 1,
		Dur:         epochDur,
		MatchVolume: 11,
		HighRate:    12,
		LowRate:     13,
		StartRate:   14,
		EndRate:     15,
		QuoteVolume: 16,
	})
	if err != nil {
		t.Fatalf("error inserting second epoch: %v", err)
	}

	startStamp = uint64((epochIdx + 1) * epochDur)
	endStamp = startStamp + uint64(epochDur)
	candle = &candles.Candle{
		StartStamp:  startStamp,
		EndStamp:    endStamp,
		MatchVolume: 11,
		HighRate:    12,
		LowRate:     13,
		StartRate:   14,
		EndRate:     15,
		QuoteVolume: 16,
	}
	addCandles[1] = candle

	lastRate, err = archie.LastEpochRate(42, 0)
	if err != nil {
		t.Fatalf("error getting last epoch rate from after second-to-last epoch: %v", err)
	}
	if lastRate != 15 {
		t.Fatalf("wrong second-to-last epoch last rate. expected 15, got %d", lastRate)
	}

	archie.insertEpoch(archie.db, &db.EpochResults{
		MktBase:     42,
		MktQuote:    0,
		Idx:         epochIdx + 2,
		Dur:         epochDur,
		MatchVolume: 100,
		HighRate:    100,
		LowRate:     100,
		StartRate:   100,
		EndRate:     100,
		QuoteVolume: 100,
	})

	startStamp = uint64((epochIdx + 2) * epochDur)
	endStamp = startStamp + uint64(epochDur)
	candle = &candles.Candle{
		StartStamp:  startStamp,
		EndStamp:    endStamp,
		MatchVolume: 100,
		HighRate:    100,
		LowRate:     100,
		StartRate:   100,
		EndRate:     100,
		QuoteVolume: 100,
	}
	addCandles[2] = candle

	startStamp = uint64((epochIdx + 2) * epochDur)
	endStamp = startStamp + uint64(epochDur)
	dayCandle := &candles.Candle{
		StartStamp:  startStamp,
		EndStamp:    endStamp,
		MatchVolume: 112,
		HighRate:    100,
		LowRate:     3,
		StartRate:   4,
		EndRate:     100,
		QuoteVolume: 122,
	}

	if err = archie.InsertCandles(42, 0, uint64(epochDur), addCandles); err != nil {
		t.Fatalf("error inserting candles: %v", err)
	}

	if err = archie.InsertCandles(42, 0, uint64(time.Hour*24/time.Millisecond), []*candles.Candle{dayCandle}); err != nil {
		t.Fatalf("error inserting candle: %v", err)
	}

	epochCache := candles.NewCache(3, uint64(epochDur))
	dayCache := candles.NewCache(2, uint64(time.Hour*24/time.Millisecond))

	err = archie.LoadEpochStats(42, 0, []*candles.Cache{epochCache, dayCache})
	if err != nil {
		t.Fatalf("error loading epoch stats: %v", err)
	}

	epochCandles := epochCache.WireCandles(3).Candles()
	if len(epochCandles) != 3 {
		t.Fatalf("epoch cache has wrong number of entries. expected 3, got %d", len(epochCandles))
	}
	lastCandle := epochCandles[len(epochCandles)-1]
	if lastCandle.MatchVolume != 100 {
		t.Fatalf("wrong last epoch candle match volume. expected 100, got %d", lastCandle.MatchVolume)
	}

	dayCandles := dayCache.WireCandles(2).Candles()
	if len(dayCandles) != 1 {
		t.Fatalf("day cache has wrong number of entries. expected 1, got %d", len(dayCandles))
	}
	lastCandle = dayCandles[len(dayCandles)-1]
	if lastCandle.MatchVolume != 112 { // 1 + 11
		t.Fatalf("wrong last day candle MatchVolume. expected 112, got %d", lastCandle.MatchVolume)
	}
	if lastCandle.QuoteVolume != 122 { // 6 + 16
		t.Fatalf("wrong last day candle QuoteVolume. expected 122, got %d", lastCandle.MatchVolume)
	}
	if lastCandle.HighRate != 100 {
		t.Fatalf("wrong last day candle HighRate. expected 100, got %d", lastCandle.HighRate)
	}
	if lastCandle.LowRate != 3 {
		t.Fatalf("wrong last day candle LowRate. expected 3, got %d", lastCandle.LowRate)
	}
	if lastCandle.StartRate != 4 {
		t.Fatalf("wrong last day candle StartRate. expected 4, got %d", lastCandle.StartRate)
	}
	if lastCandle.EndRate != 100 {
		t.Fatalf("wrong last day candle EndRate. expected 100, got %d", lastCandle.EndRate)
	}

}
