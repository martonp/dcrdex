//go:build proofgen

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	_ "decred.org/dcrdex/client/asset/importall" // register all assets
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Env vars:
//
// Required:
//   - PROOFGEN_START_MS (unix ms)
//   - PROOFGEN_END_MS   (unix ms)
//
// Optional:
//   - PROOFGEN_OUT              (default: ./proof.json in $PWD)
//   - PROOFGEN_UNCLOSED         (default: 0)
//   - PROOFGEN_SEED             (default: 1)
//   - PROOFGEN_PROFILE          (default: good) // one of: good, mixed, offline
//   - PROOFGEN_EPOCHDUR_MS      (default: 15_000)
//   - PROOFGEN_BASE_ID          (default: 42)
//   - PROOFGEN_QUOTE_ID         (default: 0)
//   - PROOFGEN_REQUIRED_QTY     (default: 1000)
//   - PROOFGEN_MAX_SPREAD_PCT   (default: 4)
//   - PROOFGEN_P0_RATE          (default: 100_000_000)
//   - PROOFGEN_PRICE_MOVE_PCT   (default: 1.0) // +/- percent change when price moves
//   - PROOFGEN_PRICE_MOVE_MS    (default: 3_600_000) // ~1 hour
//   - PROOFGEN_MATCH_EVERY_MS   (default: 1_800_000) // ~30 minutes
//   - PROOFGEN_OFFLINE_EVERY_MS (default: 21_600_000) // ~6 hours (offline profile)
//   - PROOFGEN_OFFLINE_DUR_MS   (default: 1_800_000) // ~30 minutes (offline profile)
func TestGenerateProof(t *testing.T) {
	getEnv := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	parseUint64 := func(k string, def uint64, required bool) uint64 {
		s := getEnv(k)
		if s == "" {
			if required {
				t.Fatalf("missing %s", k)
			}
			return def
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("bad %s=%q: %v", k, s, err)
		}
		return v
	}
	parseInt64 := func(k string, def int64) int64 {
		s := getEnv(k)
		if s == "" {
			return def
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("bad %s=%q: %v", k, s, err)
		}
		return v
	}
	parseInt := func(k string, def int) int {
		s := getEnv(k)
		if s == "" {
			return def
		}
		v, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("bad %s=%q: %v", k, s, err)
		}
		return v
	}
	parseFloat := func(k string, def float64) float64 {
		s := getEnv(k)
		if s == "" {
			return def
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("bad %s=%q: %v", k, s, err)
		}
		return v
	}

	startMs := parseUint64("PROOFGEN_START_MS", 0, true)
	endMs := parseUint64("PROOFGEN_END_MS", 0, true)
	if startMs == 0 || endMs == 0 || endMs <= startMs {
		t.Fatalf("invalid time range start=%d end=%d", startMs, endMs)
	}

	outPath := getEnv("PROOFGEN_OUT")
	// `go test` often runs with the package directory as CWD (and may also set $PWD
	// accordingly). For a "put files where I'm running this from" experience, we
	// resolve relative output paths against the repo root (directory containing
	// go.mod), which is typically where you invoke `go test` from.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	baseDir := findRepoRoot(wd)
	if outPath == "" {
		outPath = filepath.Join(baseDir, "proof.json")
	} else if !filepath.IsAbs(outPath) {
		// Treat relative paths as relative to the invocation directory.
		outPath = filepath.Join(baseDir, outPath)
	}
	unclosedN := parseInt("PROOFGEN_UNCLOSED", 0)
	seed := parseInt64("PROOFGEN_SEED", 1)
	profile := strings.ToLower(getEnv("PROOFGEN_PROFILE"))
	if profile == "" {
		profile = "mixed"
	}
	epochDurMs := parseUint64("PROOFGEN_EPOCHDUR_MS", 15_000, false)
	baseID := uint32(parseUint64("PROOFGEN_BASE_ID", 42, false))
	quoteID := uint32(parseUint64("PROOFGEN_QUOTE_ID", 0, false))
	requiredQty := parseFloat("PROOFGEN_REQUIRED_QTY", 1000)
	maxSpreadPct := parseFloat("PROOFGEN_MAX_SPREAD_PCT", 4)
	p0Rate := uint64(parseUint64("PROOFGEN_P0_RATE", 100_000_000, false))
	priceMovePct := parseFloat("PROOFGEN_PRICE_MOVE_PCT", 1.0)
	priceMoveEveryMs := parseUint64("PROOFGEN_PRICE_MOVE_MS", 3_600_000, false)
	matchEveryMs := parseUint64("PROOFGEN_MATCH_EVERY_MS", 1_800_000, false)
	offlineEveryMs := parseUint64("PROOFGEN_OFFLINE_EVERY_MS", 21_600_000, false)
	offlineDurMs := parseUint64("PROOFGEN_OFFLINE_DUR_MS", 1_800_000, false)

	if epochDurMs == 0 {
		t.Fatalf("PROOFGEN_EPOCHDUR_MS must be > 0")
	}
	if requiredQty <= 0 {
		t.Fatalf("PROOFGEN_REQUIRED_QTY must be > 0")
	}
	if maxSpreadPct < 0 {
		t.Fatalf("PROOFGEN_MAX_SPREAD_PCT must be >= 0")
	}
	if priceMoveEveryMs == 0 || matchEveryMs == 0 {
		t.Fatalf("PROOFGEN_PRICE_MOVE_MS and PROOFGEN_MATCH_EVERY_MS must be > 0")
	}
	if profile != "good" && profile != "mixed" && profile != "offline" {
		t.Fatalf("unknown PROOFGEN_PROFILE %q (expected good, mixed, offline)", profile)
	}

	unitInfo, err := asset.UnitInfo(baseID)
	if err != nil {
		t.Fatalf("asset.UnitInfo(%d): %v", baseID, err)
	}
	convFactor := float64(unitInfo.Conventional.ConversionFactor)
	requiredAtomic := uint64(requiredQty * convFactor)
	if requiredAtomic == 0 {
		t.Fatalf("requiredAtomic computed as 0; check PROOFGEN_REQUIRED_QTY and unit conversion")
	}

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	rng := rand.New(rand.NewSource(seed))

	sign := func(msg []byte) []byte {
		h := sha256.Sum256(msg)
		return ecdsa.Sign(priv, h[:]).Serialize()
	}

	// Fixed AccountID for all orders (proof subject).
	acctID := make([]byte, 32)
	rng.Read(acctID)

	mk32 := func() []byte {
		b := make([]byte, 32)
		rng.Read(b)
		return b
	}

	mkLimit := func(serverTime uint64, sell bool, rate, qty uint64) *msgjson.LimitOrder {
		side := uint8(msgjson.BuyOrderNum)
		if sell {
			side = uint8(msgjson.SellOrderNum)
		}
		lo := &msgjson.LimitOrder{
			Prefix: msgjson.Prefix{
				AccountID:  acctID,
				Base:       baseID,
				Quote:      quoteID,
				OrderType:  msgjson.LimitOrderNum,
				ClientTime: serverTime - 1,
				ServerTime: serverTime,
				Commit:     mk32(),
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
		lo.SetSig(sign(lo.Serialize()))
		return lo
	}

	mkCancel := func(serverTime uint64, targetID []byte) *msgjson.CancelOrder {
		co := &msgjson.CancelOrder{
			Prefix: msgjson.Prefix{
				AccountID:  mk32(), // not enforced by verifier; keep realistic random
				Base:       baseID,
				Quote:      quoteID,
				OrderType:  msgjson.CancelOrderNum,
				ClientTime: serverTime - 1,
				ServerTime: serverTime,
				Commit:     mk32(),
			},
			TargetID: targetID,
		}
		co.SetSig(sign(co.Serialize()))
		return co
	}

	// (no revoke generation for this slow-market model)

	// Simulate a slow-moving market maker.
	// - 15s epochs by default.
	// - Mid moves by +/- ~1% about once per hour.
	// - 4 orders per side, staggered around the mid.
	// - ~1 match every 30 minutes.
	//
	// Note: Orders must be "fully closed" (cancel/revoke or fully matched) to be
	// considered by the verifier's book replay. We model orders sitting on-book
	// by canceling them when the price moves / maker goes offline, and re-adding
	// new orders at the new mid.
	type simOrder struct {
		op        *core.OrderProof
		remaining uint64
	}
	var (
		orderProofs []*core.OrderProof
		active      []*simOrder
		online      = true
	)

	midRate := float64(p0Rate)
	if midRate < 1 {
		midRate = 1
	}

	jitter := func(base uint64, frac float64) uint64 {
		// +/- frac jitter around base.
		if base == 0 {
			return 0
		}
		j := float64(base) * frac
		d := (rng.Float64()*2 - 1) * j
		v := float64(base) + d
		if v < 1 {
			v = 1
		}
		return uint64(v)
	}

	nextPriceMove := startMs + jitter(priceMoveEveryMs, 0.25)
	nextMatch := startMs + jitter(matchEveryMs, 0.20)
	nextOffline := startMs + jitter(offlineEveryMs, 0.30)
	offlineUntil := uint64(0)

	cancelAll := func(cancelT uint64) {
		for _, so := range active {
			oid := convertMsgLimitOrder(so.op.Order).ID()
			ct := cancelT
			// Ensure close time is strictly after order time to satisfy verifier invariants.
			if ct <= so.op.Order.ServerTime {
				ct = so.op.Order.ServerTime + 1
			}
			so.op.Cancel = mkCancel(ct, oid[:])
			orderProofs = append(orderProofs, so.op)
		}
		active = nil
	}

	// Fixed stagger offsets from mid (within a 4% spread => +/-2% band).
	withinOffsets := []float64{0.0025, 0.0075, 0.0125, 0.0175}
	outsideOffsets := []float64{0.0300, 0.0500, 0.0700, 0.0900}

	splitQty := func(total uint64, n int) []uint64 {
		if n <= 1 {
			return []uint64{total}
		}
		weights := make([]float64, n)
		sum := 0.0
		for i := range weights {
			weights[i] = 0.8 + 0.4*rng.Float64()
			sum += weights[i]
		}
		out := make([]uint64, n)
		var used uint64
		for i := 0; i < n; i++ {
			q := uint64(float64(total) * (weights[i] / sum))
			out[i] = q
			used += q
		}
		// Fix rounding error by distributing remainder.
		rem := total - used
		for i := 0; i < n && rem > 0; i++ {
			out[i]++
			rem--
		}
		return out
	}

	placeOrders := func(placeT, cancelT uint64) {
		if cancelT <= placeT {
			cancelT = placeT + 1
		}
		P := uint64(midRate)
		if P == 0 {
			P = 1
		}
		offsets := withinOffsets
		if profile == "mixed" {
			// Half the ladder within band, half outside.
			offsets = []float64{withinOffsets[0], withinOffsets[1], outsideOffsets[0], outsideOffsets[1]}
		}
		buyQtys := splitQty(requiredAtomic, 4)
		sellQtys := splitQty(requiredAtomic, 4)

		for i := 0; i < 4; i++ {
			off := offsets[i]
			bid := uint64(float64(P) * (1 - off))
			ask := uint64(float64(P) * (1 + off))
			if bid == 0 {
				bid = 1
			}
			if ask == 0 {
				ask = 1
			}

			// 4 orders per side, staggered a bit in time.
			bt := placeT + uint64(i)
			st := placeT + uint64(10+i)

			buyOrd := mkLimit(bt, false, bid, buyQtys[i])
			sellOrd := mkLimit(st, true, ask, sellQtys[i])

			active = append(active,
				&simOrder{op: &core.OrderProof{Order: buyOrd}, remaining: buyQtys[i]},
				&simOrder{op: &core.OrderProof{Order: sellOrd}, remaining: sellQtys[i]},
			)
		}

		// Cancel is applied later by cancelAll(cancelT) when an event happens.
		_ = cancelT
	}

	doMatch := func(matchT uint64) {
		// One match approximately every 30 minutes.
		if len(active) == 0 {
			return
		}
		replenish := profile != "mixed"
		// Pick a random active order with remaining > 0.
		for tries := 0; tries < 10; tries++ {
			idx := rng.Intn(len(active))
			so := active[idx]
			if so.remaining == 0 {
				continue
			}
			// Don't match/cancel an order before it exists on the book.
			// (We sometimes stagger ServerTime slightly in the future.)
			if matchT <= so.op.Order.ServerTime {
				continue
			}
			oid := convertMsgLimitOrder(so.op.Order).ID()
			// Match a small amount (5–25 DCR) but never exceed remaining.
			min := uint64(5 * convFactor)
			max := uint64(25 * convFactor)
			if max > so.remaining {
				max = so.remaining
			}
			if min > max {
				min = max
			}
			mq := min
			if max > min {
				mq = min + uint64(rng.Int63n(int64(max-min+1)))
			}
			if mq == 0 {
				return
			}
			so.remaining -= mq
			mt := matchT
			if mt <= so.op.Order.ServerTime {
				mt = so.op.Order.ServerTime + 1
			}
			so.op.Matches = append(so.op.Matches, func() *msgjson.Match {
				m := &msgjson.Match{
					OrderID:    oid[:],
					MatchID:    mk32(),
					Quantity:   mq,
					Rate:       so.op.Order.Rate,
					ServerTime: mt,
					Address:    "",
				}
				m.SetSig(sign(m.Serialize()))
				return m
			}())

			// In the "good" and "offline" profiles (when online), replenish depth by
			// canceling the partially filled order and immediately replacing it with
			// a fresh order at the same level. This keeps the book roughly at 4 orders
			// per side and near the target depth.
			if replenish {
				// Close the current order shortly after the match, but never before it exists.
				cancelT := mt + 1
				if cancelT <= so.op.Order.ServerTime {
					cancelT = so.op.Order.ServerTime + 1
				}
				soID := convertMsgLimitOrder(so.op.Order).ID()
				so.op.Cancel = mkCancel(cancelT, soID[:])
				orderProofs = append(orderProofs, so.op)

				// Remove from active.
				active[idx] = active[len(active)-1]
				active = active[:len(active)-1]

				// Replace with a new order shortly after cancel with the original size.
				origQty := so.op.Order.Trade.Quantity
				newOrd := mkLimit(cancelT+1, so.op.Order.Trade.Side == uint8(msgjson.SellOrderNum), so.op.Order.Rate, origQty)
				active = append(active, &simOrder{op: &core.OrderProof{Order: newOrd}, remaining: origQty})
			}
			return
		}
	}

	// Start with an initial placement shortly after start.
	placeT := startMs + 1
	if nextPriceMove < placeT {
		nextPriceMove = placeT + priceMoveEveryMs
	}
	// Orders live until the next reprice/offline/end.
	nextCancel := nextPriceMove
	if profile == "offline" && nextOffline < nextCancel {
		nextCancel = nextOffline
	}
	if endMs+epochDurMs < nextCancel {
		nextCancel = endMs + epochDurMs
	}
	placeOrders(placeT, nextCancel)

	curT := placeT
	for curT < endMs {
		// Handle offline transitions (offline profile only).
		if profile == "offline" && online && curT >= nextOffline {
			cancelAll(curT)
			online = false
			offlineUntil = curT + offlineDurMs
		}
		if profile == "offline" && !online && offlineUntil > 0 && curT >= offlineUntil {
			online = true
			nextOffline = curT + jitter(offlineEveryMs, 0.30)
			offlineUntil = 0
			// Place fresh orders at current mid.
			nextCancel = nextPriceMove
			if nextOffline < nextCancel {
				nextCancel = nextOffline
			}
			if endMs+epochDurMs < nextCancel {
				nextCancel = endMs + epochDurMs
			}
			placeOrders(curT+1, nextCancel)
		}

		// Price move.
		if curT >= nextPriceMove {
			// Cancel current orders and reprice.
			if online {
				cancelAll(curT)
			}
			dir := 1.0
			if rng.Intn(2) == 0 {
				dir = -1.0
			}
			midRate = midRate * (1 + dir*priceMovePct/100)
			if midRate < 1 {
				midRate = 1
			}
			nextPriceMove = curT + jitter(priceMoveEveryMs, 0.25)
			if online {
				nextCancel = nextPriceMove
				if profile == "offline" && nextOffline < nextCancel {
					nextCancel = nextOffline
				}
				if endMs+epochDurMs < nextCancel {
					nextCancel = endMs + epochDurMs
				}
				placeOrders(curT+1, nextCancel)
			}
		}

		// Match event.
		if curT >= nextMatch {
			if online {
				doMatch(curT)
			}
			nextMatch = curT + jitter(matchEveryMs, 0.20)
		}

		// Advance in 15s epochs.
		curT += epochDurMs
	}

	// Close any remaining live orders after end time so they contribute through the window.
	if online && len(active) > 0 {
		cancelAll(endMs + epochDurMs)
	}

	// Add extra unclosed orders at random times (do not contribute to coverage, but show up in UnclosedOrders).
	for i := 0; i < unclosedN; i++ {
		t0 := startMs + 1 + uint64(rng.Int63n(int64(endMs-startMs)))
		if t0 <= startMs {
			t0 = startMs + 1
		}
		// Place near mid but randomize.
		rate := uint64(midRate * (0.98 + 0.04*rng.Float64()))
		if rate == 0 {
			rate = 1
		}
		qty := uint64(float64(requiredAtomic) * (0.1 + 0.3*rng.Float64()))
		if qty == 0 {
			qty = 1
		}
		sell := rng.Intn(2) == 0
		lo := mkLimit(t0+uint64(i), sell, rate, qty)
		orderProofs = append(orderProofs, &core.OrderProof{Order: lo})
	}

	proof := &core.MarketMakingProof{Orders: orderProofs}

	outDir := filepath.Dir(outPath)
	if outDir == "." {
		outDir, _ = os.Getwd()
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", outDir, err)
	}

	b, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if err := os.WriteFile(outPath, b, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", outPath, err)
	}

	commandsPath := strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".commands.txt"
	cmds := fmt.Sprintf(`# Generated by TestGenerateProof (build tag "proofgen")
#
# Market: %s/%s (%d/%d)
# AccountID: %s
# Profile: %s
# Start: %s
# End:   %s
#
# DEX pubkey (hex): %s
#
# Example verify:
verifymm verify -proof %q -pubkey %s -start %d -end %d -epochdur %d -qty %.8f -maxspreadpct %.4f -out report.json
#
# Example PDF:
verifymm genreport -report report.json -out coverage.pdf
`,
		dex.BipIDSymbol(baseID), dex.BipIDSymbol(quoteID), baseID, quoteID,
		hex.EncodeToString(acctID),
		profile,
		time.UnixMilli(int64(startMs)).UTC().Format(time.RFC3339),
		time.UnixMilli(int64(endMs)).UTC().Format(time.RFC3339),
		pubHex,
		outPath, pubHex, startMs, endMs, epochDurMs, requiredQty, maxSpreadPct,
	)
	if err := os.WriteFile(commandsPath, []byte(cmds), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", commandsPath, err)
	}

	// NOTE: These are visible with `go test -v ...`.
	t.Logf("Wrote proof file: %s", outPath)
	t.Logf("Wrote commands:   %s", commandsPath)
	t.Logf("DEX pubkey hex:   %s", pubHex)
	t.Logf("Verify/Report commands:\n%s", cmds)
}

// (no helpers below)

func findRepoRoot(startDir string) string {
	dir := startDir
	for i := 0; i < 20; i++ {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return startDir
}
