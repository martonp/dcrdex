// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"decred.org/dcrdex/client/asset"
	_ "decred.org/dcrdex/client/asset/importall" // register all assets
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

type verifyOptions struct {
	proofFile    string
	startTimeMs  uint64
	endTimeMs    uint64
	epochDurMs   uint64
	requiredQty  float64
	maxSpreadPct float64
	outputFile   string
	dexPubKey    *secp256k1.PublicKey
}

func (o *verifyOptions) parseFlags() error {
	var dexPubKeyHex string

	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	fs.StringVar(&o.proofFile, "proof", "", "Path to the proof JSON file")
	fs.StringVar(&dexPubKeyHex, "pubkey", "", "DEX server public key (hex encoded, required)")
	fs.Uint64Var(&o.startTimeMs, "start", 0, "Start time in Unix milliseconds (required)")
	fs.Uint64Var(&o.endTimeMs, "end", 0, "End time in Unix milliseconds (required)")
	fs.Uint64Var(&o.epochDurMs, "epochdur", 0, "DEX epoch duration in milliseconds (required)")
	fs.Float64Var(&o.requiredQty, "qty", 0, "Required quantity on both sides (conventional units, e.g. DCR, required)")
	fs.Float64Var(&o.maxSpreadPct, "maxspreadpct", 0, "Max allowed spread percentage for liquidity counting (0 disables spread filter)")
	fs.StringVar(&o.outputFile, "out", "", "Output file for JSON report (default: stdout)")
	err := fs.Parse(os.Args[1:])
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	pubKeyBytes, err := hex.DecodeString(dexPubKeyHex)
	if err != nil {
		return fmt.Errorf("invalid pubkey hex: %w", err)
	}
	o.dexPubKey, err = secp256k1.ParsePubKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("failed to parse pubkey: %w", err)
	}

	if o.proofFile == "" {
		return errors.New("proof file is required (-proof)")
	}
	if o.requiredQty <= 0 {
		return errors.New("-qty is required and must be > 0")
	}
	if o.dexPubKey == nil {
		return errors.New("DEX public key is required (-pubkey)")
	}
	if o.startTimeMs == 0 || o.endTimeMs == 0 {
		return errors.New("both -start and -end are required")
	}
	if o.epochDurMs == 0 {
		return errors.New("-epochdur is required")
	}
	if o.maxSpreadPct < 0 {
		return errors.New("-maxspreadpct must be >= 0")
	}

	return nil
}

func readProofFile(proofFile string) (*core.MarketMakingProof, error) {
	proofData, err := os.ReadFile(proofFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read proof file: %w", err)
	}
	var proof core.MarketMakingProof
	if err := json.Unmarshal(proofData, &proof); err != nil {
		return nil, fmt.Errorf("failed to parse proof JSON: %w", err)
	}
	return &proof, nil
}

func runVerify() error {
	var opts verifyOptions
	if err := opts.parseFlags(); err != nil {
		return err
	}

	proof, err := readProofFile(opts.proofFile)
	if err != nil {
		return fmt.Errorf("failed to read proof file: %w", err)
	}
	if len(proof.Orders) == 0 {
		return errors.New("proof contains no orders")
	}
	fmt.Printf("Loaded proof with %d orders\n", len(proof.Orders))

	baseAssetID := proof.Orders[0].Order.Base
	quoteAssetID := proof.Orders[0].Order.Quote
	acctID := proof.Orders[0].Order.AccountID

	// Get the conversion factor for the base asset
	unitInfo, err := asset.UnitInfo(baseAssetID)
	if err != nil {
		return fmt.Errorf("failed to get unit info for base asset ID %d: %w", baseAssetID, err)
	}
	conversionFactor := unitInfo.Conventional.ConversionFactor

	// Initialize report - store quantities in conventional units
	report := &CoverageReport{
		StartTimeMs:      opts.startTimeMs,
		EndTimeMs:        opts.endTimeMs,
		EpochDurMs:       opts.epochDurMs,
		RequiredQty:      opts.requiredQty,
		MaxSpreadPct:     opts.maxSpreadPct,
		ConversionFactor: conversionFactor,
		BaseAssetID:      baseAssetID,
		BaseAssetSymbol:  dex.BipIDSymbol(baseAssetID),
		QuoteAssetID:     quoteAssetID,
		QuoteAssetSymbol: dex.BipIDSymbol(quoteAssetID),
		AccountID:        hex.EncodeToString(acctID),
	}

	// Verify the proof (signatures + cross-checks).
	fmt.Println("Verifying proof...")
	if err := verifyOrders(proof, opts.dexPubKey); err != nil {
		report.ProofValid = false
		report.ProofError = err.Error()
	} else {
		report.ProofValid = true
	}
	fmt.Printf("Proof valid: %t\n", report.ProofValid)
	if report.ProofError != "" {
		fmt.Printf("Proof error: %s\n", report.ProofError)
	}

	// Generate coverage report (only if proof is valid)
	if report.ProofValid {
		fmt.Println("Calculating coverage...")
		generateCoverageReport(proof, report)
		fmt.Printf("Coverage done. Total epochs=%d, buy covered=%d, sell covered=%d\n",
			report.TotalEpochs, report.BuyCoveredEpochs, report.SellCoveredEpochs)
	}

	// Output JSON report
	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode report: %w", err)
	}

	if opts.outputFile != "" {
		if err := os.WriteFile(opts.outputFile, reportJSON, 0644); err != nil {
			return fmt.Errorf("failed to write report file: %w", err)
		}
	} else {
		fmt.Println(string(reportJSON))
	}

	return nil
}

// verifyOrders validates the following:
//   - All signatures are valid
//   - The cancels, matches, and revokes target the correct order
//   - There are no duplicate order IDs
//   - Match/cancel/revoke quantities and timestamps are internally consistent
func verifyOrders(proof *core.MarketMakingProof, pubKey *secp256k1.PublicKey) error {
	seenOrders := make(map[string]bool)

	var baseID, quoteID uint32
	var acctID []byte

	for i, ord := range proof.Orders {
		if i%1000 == 0 {
			fmt.Printf("Verifying order %d / %d\n", i, len(proof.Orders))
		}
		// Check that the order targets the expected base and quote ID
		if i == 0 {
			baseID = ord.Order.Base
			quoteID = ord.Order.Quote
			acctID = ord.Order.AccountID
		} else {
			if baseID != ord.Order.Base || quoteID != ord.Order.Quote {
				return fmt.Errorf("order %d has different base or quote ID than the first order", i)
			}
			if !bytes.Equal(acctID, ord.Order.AccountID) {
				return fmt.Errorf("order %d has different account ID than the first order", i)
			}
		}

		orderID := convertMsgLimitOrder(ord.Order).ID()
		orderIDHex := hex.EncodeToString(orderID[:])

		// Check for duplicate orders
		if seenOrders[orderIDHex] {
			return fmt.Errorf("duplicate order ID %s in proof", orderIDHex)
		}
		seenOrders[orderIDHex] = true

		// Verify order signature (signature is in Order.Prefix.Sig).
		if len(ord.Order.Sig) == 0 {
			return fmt.Errorf("order %d (%s) has no DEX signature", i, orderIDHex)
		}
		if ord.Order.ServerTime == 0 {
			return fmt.Errorf("order %d (%s) has no server time", i, orderIDHex)
		}
		if err := verifySig(ord.Order.Serialize(), ord.Order.Sig, pubKey); err != nil {
			return fmt.Errorf("order %d (%s) signature verification failed: %w", i, orderIDHex, err)
		}

		// Verify cancel order signature if present
		if ord.Cancel != nil {
			if ord.Cancel.ServerTime == 0 {
				return fmt.Errorf("cancel order for order %d has no server time", i)
			}
			if len(ord.Cancel.Sig) == 0 {
				return fmt.Errorf("cancel order for order %d has no DEX signature", i)
			}
			if err := verifySig(ord.Cancel.Serialize(), ord.Cancel.Sig, pubKey); err != nil {
				return fmt.Errorf("cancel order for order %d signature verification failed: %w", i, err)
			}
			if !bytes.Equal(ord.Cancel.TargetID, orderID[:]) {
				return fmt.Errorf("cancel order for order %d targets %x but order ID is %x",
					i, ord.Cancel.TargetID, orderID[:])
			}
		}

		// Verify revoke_order signature if present.
		if ord.Revoke != nil {
			if len(ord.Revoke.Sig) == 0 || ord.Revoke.Time == 0 {
				return fmt.Errorf("order %d has partial revoke proof (need both revokeSig and revokeTime)", i)
			}
			if err := verifySig(ord.Revoke.Serialize(), ord.Revoke.Sig, pubKey); err != nil {
				return fmt.Errorf("order %d revoke_order signature verification failed: %w", i, err)
			}
			if !bytes.Equal(ord.Revoke.OrderID, orderID[:]) {
				return fmt.Errorf("revoke order for order %d targets %x but order ID is %x",
					i, ord.Revoke.OrderID, orderID[:])
			}
		}

		// Enforce event-replay invariants (prevents applyEvent underflow panics).
		if ord.Cancel != nil && ord.Revoke != nil {
			return fmt.Errorf("order %d (%s) has both cancel and revoke proofs", i, orderIDHex)
		}

		orderQty := ord.Order.Trade.Quantity
		orderTime := ord.Order.ServerTime

		var matchedQty uint64
		closeTime := uint64(0)
		if ord.Cancel != nil {
			closeTime = ord.Cancel.ServerTime
		}
		if ord.Revoke != nil {
			closeTime = ord.Revoke.Time
		}
		if closeTime > 0 && closeTime < orderTime {
			return fmt.Errorf("order %d (%s) close time %d is before order time %d", i, orderIDHex, closeTime, orderTime)
		}

		// Verify match signatures
		for j, match := range ord.Matches {
			if len(match.Sig) == 0 {
				return fmt.Errorf("match %d for order %d has no signature", j, i)
			}
			if err := verifySig(match.Serialize(), match.Sig, pubKey); err != nil {
				return fmt.Errorf("match %d for order %d signature verification failed: %w", j, i, err)
			}
			if !bytes.Equal(match.OrderID, orderID[:]) {
				return fmt.Errorf("match %d for order %d references order %x but order ID is %x",
					j, i, match.OrderID, orderID[:])
			}

			if match.ServerTime < orderTime {
				return fmt.Errorf("match %d for order %d (%s) has server time %d before order time %d", j, i, orderIDHex, match.ServerTime, orderTime)
			}
			if closeTime > 0 && match.ServerTime > closeTime {
				return fmt.Errorf("match %d for order %d (%s) has server time %d after close time %d", j, i, orderIDHex, match.ServerTime, closeTime)
			}

			// Matched quantity must not exceed the order quantity.
			if match.Quantity > orderQty-matchedQty {
				return fmt.Errorf("order %d (%s) match quantities exceed order quantity (%d + %d > %d)", i, orderIDHex, matchedQty, match.Quantity, orderQty)
			}
			matchedQty += match.Quantity
		}

		// If the order is fully closed by matches (no cancel/revoke), enforce exact fill.
		if ord.Cancel == nil && ord.Revoke == nil && ord.Order.TiF == msgjson.StandingOrderNum && matchedQty >= orderQty && matchedQty != orderQty {
			return fmt.Errorf("order %d (%s) has matched quantity %d exceeding order quantity %d", i, orderIDHex, matchedQty, orderQty)
		}
	}

	return nil
}

// verifySig verifies a secp256k1 ECDSA signature.
func verifySig(msg, sigBytes []byte, pubKey *secp256k1.PublicKey) error {
	sig, err := ecdsa.ParseDERSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("failed to parse signature: %w", err)
	}
	hash := sha256.Sum256(msg)
	if !sig.Verify(hash[:], pubKey) {
		return errors.New("signature verification failed")
	}
	return nil
}

// orderFullyClosed returns true if we have an end time for the full quantity of the order.
func orderFullyClosed(ord *core.OrderProof) bool {
	if ord.Cancel != nil || ord.Revoke != nil {
		return true
	}

	matchedQty := uint64(0)
	for _, match := range ord.Matches {
		matchedQty += match.Quantity
	}

	return matchedQty >= ord.Order.Trade.Quantity
}

// orderEvent represents an event that changes the order book state.
type orderEvent struct {
	stampMs  uint64
	sell     bool
	rate     uint64
	qtyDelta int64 // positive for add, negative for remove
}

func orderRunEvents(proof *core.MarketMakingProof) ([]*orderEvent, []dex.Bytes) {
	var events []*orderEvent
	events = make([]*orderEvent, 0, len(proof.Orders)*2)
	var unclosedOrders []dex.Bytes

	toSigned := func(q uint64, neg bool) int64 {
		const maxInt64 = int64(^uint64(0) >> 1)
		if q > uint64(maxInt64) {
			panic(fmt.Sprintf("quantity overflows int64: %d", q))
		}
		if neg {
			return -int64(q)
		}
		return int64(q)
	}

	for _, ord := range proof.Orders {
		orderID := convertMsgLimitOrder(ord.Order).ID()
		if ord.Order.TiF != msgjson.StandingOrderNum {
			// Only standing limit orders sit on the book
			continue
		}

		// If order is not closed, record it and skip
		if !orderFullyClosed(ord) {
			unclosedOrders = append(unclosedOrders, orderID[:])
			continue
		}

		sell := ord.Order.Trade.Side == msgjson.SellOrderNum

		events = append(events, &orderEvent{
			stampMs:  ord.Order.ServerTime,
			sell:     sell,
			rate:     ord.Order.Rate,
			qtyDelta: toSigned(ord.Order.Trade.Quantity, false),
		})

		unmatchedQty := ord.Order.Trade.Quantity

		for _, match := range ord.Matches {
			unmatchedQty -= match.Quantity
			events = append(events, &orderEvent{
				stampMs:  match.ServerTime,
				sell:     sell,
				rate:     ord.Order.Rate,
				qtyDelta: toSigned(match.Quantity, true),
			})
		}

		if ord.Cancel != nil {
			events = append(events, &orderEvent{
				stampMs:  ord.Cancel.ServerTime,
				sell:     sell,
				rate:     ord.Order.Rate,
				qtyDelta: toSigned(unmatchedQty, true),
			})
		}

		if ord.Revoke != nil {
			events = append(events, &orderEvent{
				stampMs:  ord.Revoke.Time,
				sell:     sell,
				rate:     ord.Order.Rate,
				qtyDelta: toSigned(unmatchedQty, true),
			})
		}
	}

	// Sort events by timestamp, additions before removals.
	sort.Slice(events, func(i, j int) bool {
		if events[i].stampMs == events[j].stampMs {
			return events[i].qtyDelta > events[j].qtyDelta
		}
		return events[i].stampMs < events[j].stampMs
	})

	return events, unclosedOrders
}

func calcSpreadPct(bestBid, bestAsk, mid uint64) float64 {
	if bestBid == 0 || bestAsk == 0 || mid == 0 || bestAsk <= bestBid {
		return 0
	}
	return float64(bestAsk-bestBid) / float64(mid) * 100
}

func bookSummary(epochNum uint64, buys, sells map[uint64]uint64, maxSpreadPct float64) *TimePoint {
	tp := &TimePoint{
		EpochNum: epochNum,
	}
	var (
		totalBuy, totalSell uint64
		bestBid             uint64
		bestAsk             uint64
	)

	for rate, qty := range buys {
		if qty == 0 {
			continue
		}
		totalBuy += qty
		if rate > bestBid {
			bestBid = rate
		}
	}
	for rate, qty := range sells {
		if qty == 0 {
			continue
		}
		totalSell += qty
		if bestAsk == 0 || rate < bestAsk {
			bestAsk = rate
		}
	}

	var mid uint64
	switch {
	case bestBid > 0 && bestAsk > 0:
		mid = (bestBid + bestAsk) / 2
	case bestBid > 0:
		mid = bestBid
	case bestAsk > 0:
		mid = bestAsk
	}

	tp.TotalBuyQty, tp.TotalSellQty = totalBuy, totalSell
	tp.BestBidRate, tp.BestAskRate = bestBid, bestAsk
	tp.MidRate = mid
	tp.SpreadPct = calcSpreadPct(bestBid, bestAsk, mid)

	// If maxSpreadPct == 0 (or no midpoint), use total quantities.
	if maxSpreadPct <= 0 || mid == 0 {
		tp.BuyQty, tp.SellQty = totalBuy, totalSell
		return tp
	}

	half := maxSpreadPct / 200 // e.g. 4% max spread => 2% on either side of mid.
	lower := float64(mid) * (1 - half)
	upper := float64(mid) * (1 + half)

	var buyIn, sellIn uint64
	for rate, qty := range buys {
		if qty == 0 {
			continue
		}
		if float64(rate) >= lower {
			buyIn += qty
		}
	}
	for rate, qty := range sells {
		if qty == 0 {
			continue
		}
		if float64(rate) <= upper {
			sellIn += qty
		}
	}

	tp.BuyQty, tp.SellQty = buyIn, sellIn
	return tp
}

func applyEvent(buys, sells map[uint64]uint64, event *orderEvent) {
	if event.qtyDelta == 0 {
		return
	}

	side := buys
	if event.sell {
		side = sells
	}

	if event.qtyDelta > 0 {
		side[event.rate] += uint64(event.qtyDelta)
		return
	}

	qty := side[event.rate]
	sub := uint64(-event.qtyDelta)
	if sub > qty {
		// Shouldn't happen if verifyOrders is working correctly.
		panic(fmt.Sprintf("applyEvent: quantity underflow: %d > %d", sub, qty))
	}

	if sub == qty {
		delete(side, event.rate)
		return
	}

	side[event.rate] = qty - sub
}

func createTimePoints(events []*orderEvent, startTimeMs, endTimeMs, epochDurMs uint64, maxSpreadPct float64) []*TimePoint {
	startEpoch := startTimeMs / epochDurMs
	endEpoch := endTimeMs / epochDurMs

	timePoints := make([]*TimePoint, 0, len(events)+2)
	// buys and sells are maps of rate to quantity
	buys := make(map[uint64]uint64)
	sells := make(map[uint64]uint64)

	// Always start with a TimePoint at startEpoch
	// First, accumulate state from events before startEpoch
	eventIdx := 0
	for eventIdx < len(events) {
		event := events[eventIdx]
		epochNum := event.stampMs / epochDurMs
		if epochNum > startEpoch {
			break
		}
		applyEvent(buys, sells, event)
		eventIdx++
	}

	// Add the start epoch point with accumulated state
	timePoints = append(timePoints, bookSummary(startEpoch, buys, sells, maxSpreadPct))

	// Process remaining events within the range
	for ; eventIdx < len(events); eventIdx++ {
		event := events[eventIdx]
		epochNum := event.stampMs / epochDurMs
		if epochNum > endEpoch {
			break
		}

		applyEvent(buys, sells, event)

		// If this is the last event, add a new time point
		if eventIdx == len(events)-1 {
			timePoints = append(timePoints, bookSummary(endEpoch, buys, sells, maxSpreadPct))
			break
		}

		// If this is the last event in this epoch, add a new time point
		nextEvent := events[eventIdx+1]
		nextEpochNum := nextEvent.stampMs / epochDurMs
		if nextEpochNum != epochNum {
			timePoints = append(timePoints, bookSummary(epochNum, buys, sells, maxSpreadPct))
		}
	}

	// Ensure there's a TimePoint at endEpoch
	lastTimePoint := timePoints[len(timePoints)-1]
	if lastTimePoint.EpochNum < endEpoch {
		timePoints = append(timePoints, bookSummary(endEpoch, buys, sells, maxSpreadPct))
	}

	return timePoints
}

// calculateCoverageSummary calculates coverage statistics from TimePoints.
// TimePoints must have entries at both startEpoch and endEpoch.
func calculateCoverageSummary(timePoints []*TimePoint, requiredAtomic uint64) (totalEpochs, buyCovered, sellCovered uint64) {
	if len(timePoints) < 2 {
		return 0, 0, 0
	}

	totalEpochs = timePoints[len(timePoints)-1].EpochNum - timePoints[0].EpochNum + 1

	// Iterate through consecutive pairs of TimePoints
	for i := 0; i < len(timePoints)-1; i++ {
		curr := timePoints[i]
		next := timePoints[i+1]
		epochsInState := next.EpochNum - curr.EpochNum

		if curr.BuyQty >= requiredAtomic {
			buyCovered += epochsInState
		}
		if curr.SellQty >= requiredAtomic {
			sellCovered += epochsInState
		}
	}

	// Count the final epoch (endEpoch)
	last := timePoints[len(timePoints)-1]
	if last.BuyQty >= requiredAtomic {
		buyCovered++
	}
	if last.SellQty >= requiredAtomic {
		sellCovered++
	}

	return totalEpochs, buyCovered, sellCovered
}

// generateCoverageReport analyzes the proof and populates the coverage report.
func generateCoverageReport(proof *core.MarketMakingProof, report *CoverageReport) {
	events, unclosedOrders := orderRunEvents(proof)

	fmt.Printf("Built %d order events\n", len(events))
	report.UnclosedOrders = unclosedOrders
	fmt.Printf("Unclosed orders: %d\n", len(report.UnclosedOrders))
	report.TimeSeries = createTimePoints(events, report.StartTimeMs, report.EndTimeMs, report.EpochDurMs, report.MaxSpreadPct)
	fmt.Printf("Time series points: %d\n", len(report.TimeSeries))

	requiredAtomic := uint64(report.RequiredQty * float64(report.ConversionFactor))
	report.TotalEpochs, report.BuyCoveredEpochs, report.SellCoveredEpochs =
		calculateCoverageSummary(report.TimeSeries, requiredAtomic)
}

func convertMsgLimitOrder(msgOrder *msgjson.LimitOrder) *order.LimitOrder {
	tif := order.ImmediateTiF
	if msgOrder.TiF == msgjson.StandingOrderNum {
		tif = order.StandingTiF
	}
	return &order.LimitOrder{
		P:     convertMsgPrefix(&msgOrder.Prefix, order.LimitOrderType),
		T:     convertMsgTrade(&msgOrder.Trade),
		Rate:  msgOrder.Rate,
		Force: tif,
	}
}

func convertMsgPrefix(msgPrefix *msgjson.Prefix, oType order.OrderType) order.Prefix {
	var commit order.Commitment
	copy(commit[:], msgPrefix.Commit)
	var acctID account.AccountID
	copy(acctID[:], msgPrefix.AccountID)
	return order.Prefix{
		AccountID:  acctID,
		BaseAsset:  msgPrefix.Base,
		QuoteAsset: msgPrefix.Quote,
		OrderType:  oType,
		ServerTime: time.UnixMilli(int64(msgPrefix.ServerTime)),
		ClientTime: time.UnixMilli(int64(msgPrefix.ClientTime)),
		Commit:     commit,
	}
}

func convertMsgTrade(msgTrade *msgjson.Trade) order.Trade {
	coins := make([]order.CoinID, 0, len(msgTrade.Coins))
	for _, coin := range msgTrade.Coins {
		var b []byte = coin.ID
		coins = append(coins, b)
	}
	sell := true
	if msgTrade.Side == msgjson.BuyOrderNum {
		sell = false
	}
	return order.Trade{
		Coins:    coins,
		Sell:     sell,
		Quantity: msgTrade.Quantity,
		Address:  msgTrade.Address,
	}
}
