// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/order"
)

const (
	// Our mid-gap rate derived from the local DEX order book is converted to an
	// effective mid-gap that can only vary by up to 3% from the oracle rate.
	// This is to prevent someone from taking advantage of a sparse market to
	// force a bot into giving a favorable price. In reality a market maker on
	// an empty market should use a high oracle bias anyway, but this should
	// prevent catastrophe.
	maxOracleMismatch = 0.03
)

// GapStrategy is a specifier for an algorithm to choose the maker bot's target
// spread.
type GapStrategy string

const (
	// GapStrategyMultiplier calculates the spread by multiplying the
	// break-even gap by the specified multiplier, 1 <= r <= 100.
	GapStrategyMultiplier GapStrategy = "multiplier"
	// GapStrategyAbsolute sets the spread to the rate difference.
	GapStrategyAbsolute GapStrategy = "absolute"
	// GapStrategyAbsolutePlus sets the spread to the rate difference plus the
	// break-even gap.
	GapStrategyAbsolutePlus GapStrategy = "absolute-plus"
	// GapStrategyPercent sets the spread as a ratio of the mid-gap rate.
	// 0 <= r <= 0.1
	GapStrategyPercent GapStrategy = "percent"
	// GapStrategyPercentPlus sets the spread as a ratio of the mid-gap rate
	// plus the break-even gap.
	GapStrategyPercentPlus GapStrategy = "percent-plus"
)

// OrderPlacement represents the distance from the mid-gap and the
// amount of lots that should be placed at this distance.
type OrderPlacement struct {
	// Lots is the max number of lots to place at this distance from the
	// mid-gap rate. If there is not enough balance to place this amount
	// of lots, the max that can be afforded will be placed.
	Lots uint64 `json:"lots"`

	// GapFactor controls the gap width in a way determined by the GapStrategy.
	GapFactor float64 `json:"gapFactor"`
}

// MarketMakingConfig is the configuration for a simple market
// maker that places orders on both sides of the order book.
type MarketMakingConfig struct {
	// GapStrategy selects an algorithm for calculating the distance from
	// the basis price to place orders.
	GapStrategy GapStrategy `json:"gapStrategy"`

	// SellPlacements is a list of order placements for sell orders.
	// The orders are prioritized from the first in this list to the
	// last.
	SellPlacements []*OrderPlacement `json:"sellPlacements"`

	// BuyPlacements is a list of order placements for buy orders.
	// The orders are prioritized from the first in this list to the
	// last.
	BuyPlacements []*OrderPlacement `json:"buyPlacements"`

	// DriftTolerance is how far away from an ideal price orders can drift
	// before they are replaced (units: ratio of price). Default: 0.1%.
	// 0 <= x <= 0.01.
	DriftTolerance float64 `json:"driftTolerance"`

	// OracleWeighting affects how the target price is derived based on external
	// market data. OracleWeighting, r, determines the target price with the
	// formula:
	//   target_price = dex_mid_gap_price * (1 - r) + oracle_price * r
	// OracleWeighting is limited to 0 <= x <= 1.0.
	// Fetching of price data is disabled if OracleWeighting = 0.
	OracleWeighting *float64 `json:"oracleWeighting"`

	// OracleBias applies a bias in the positive (higher price) or negative
	// (lower price) direction. -0.05 <= x <= 0.05.
	OracleBias float64 `json:"oracleBias"`

	// EmptyMarketRate can be set if there is no market data available, and is
	// ignored if there is market data available.
	EmptyMarketRate float64 `json:"emptyMarketRate"`

	// SplitTxAllowed indicates whether the wallet (if it supports multi-splits)
	// is allowed to do multi split transactions when funding orders. For UTXO
	// based assets, it may not be possible to fund each of the orders without
	// a split transaction which creates outputs of the size required by each
	// order.
	SplitTxAllowed bool `json:"splitTxAllowed"`

	// SplitBuffer indicates whether the quote asset wallet (if it supports
	// multi-splits) should add a buffer to the split amount. This is useful
	// to prevent too many split transactions from being created. During the
	// operation of this market maker, orders will be frequently placed and
	// canceled as the price moves. If the price moves upward and there is
	// no split buffer, the wallet may need to create a new split transaction
	// to fund the new orders.
	//
	// The split buffer is a percentage of the total amount required for an
	// order. For example if the order requires 0.1 BTC and the split buffer
	// is 5, the wallet will create a split transaction with an output of
	// 0.105 BTC.
	SplitBuffer *uint64 `json:"splitBuffer"`
}

type basicMMDecisionInfo struct {
	MidGap     uint64 `json:"midGap"`
	OracleRate uint64 `json:"oracleRate"`
	FiatRate   uint64 `json:"fiatRate"`
	BasisPrice uint64 `json:"basisPrice"`

	BaseSingleLotFees  uint64 `json:"baseSingleLotFees"`
	QuoteSingleLotFees uint64 `json:"quoteSingleLotFees"`
	HalfGap            uint64 `json:"halfGap"`

	SellOrders []*rateLots `json:"sellOrders"`
	BuyOrders  []*rateLots `json:"buyOrders"`
}

func getBasicMMDecisionInfo(cfg *BotConfig, mkt *core.Market, ob dexOrderBook, oracle oracle,
	baseFiatRate, quoteFiatRate float64, core clientCore, log dex.Logger) (*basicMMDecisionInfo, error) {
	if cfg.MMCfg == nil {
		return nil, fmt.Errorf("no market making config")
	}

	midGap, err := ob.MidGap()
	if err != nil && !errors.Is(err, orderbook.ErrEmptyOrderbook) {
		return nil, fmt.Errorf("MidGap error: %v", err)
	}

	oracleRate := oracle.GetMarketPrice(cfg.BaseAsset, cfg.QuoteAsset)
	msgOracleRate := mkt.ConventionalRateToMsg(oracleRate)
	var fiatRate uint64
	if baseFiatRate > 0 && quoteFiatRate > 0 {
		fiatRate = mkt.ConventionalRateToMsg(baseFiatRate / quoteFiatRate)
	}
	basisPrice := basisPrice(ob, oracle, cfg.MMCfg, mkt, fiatRate, log)
	info := &basicMMDecisionInfo{
		MidGap:     midGap,
		OracleRate: msgOracleRate,
		FiatRate:   fiatRate,
		BasisPrice: basisPrice,
	}

	if basisPrice == 0 {
		return info, nil
	}

	var halfSpread uint64
	if needBreakEvenHalfSpread(cfg.MMCfg.GapStrategy) {
		hs, baseFees, quoteFees, err := calculateHalfSpread(basisPrice, cfg.Host, cfg.BaseAsset, cfg.QuoteAsset, mkt, core)
		if err != nil {
			return nil, fmt.Errorf("calculateHalfSpread error: %v", err)
		}
		halfSpread = hs
		info.HalfGap = halfSpread
		info.BaseSingleLotFees = baseFees
		info.QuoteSingleLotFees = quoteFees
	}

	info.BuyOrders = make([]*rateLots, 0, len(cfg.MMCfg.BuyPlacements))
	for _, p := range cfg.MMCfg.BuyPlacements {
		rate := orderPrice(basisPrice, halfSpread, cfg.MMCfg.GapStrategy, p.GapFactor, false, mkt)
		if rate == 0 {
			continue
		}
		info.BuyOrders = append(info.BuyOrders, &rateLots{
			Rate: rate,
			Lots: p.Lots,
		})
	}

	info.SellOrders = make([]*rateLots, 0, len(cfg.MMCfg.SellPlacements))
	for _, p := range cfg.MMCfg.SellPlacements {
		rate := orderPrice(basisPrice, halfSpread, cfg.MMCfg.GapStrategy, p.GapFactor, true, mkt)
		if rate == 0 {
			continue
		}
		info.SellOrders = append(info.SellOrders, &rateLots{
			Rate: rate,
			Lots: p.Lots,
		})
	}

	return info, nil
}

func needBreakEvenHalfSpread(strat GapStrategy) bool {
	return strat == GapStrategyAbsolutePlus || strat == GapStrategyPercentPlus || strat == GapStrategyMultiplier
}

func (c *MarketMakingConfig) Validate() error {
	if c.OracleBias < -0.05 || c.OracleBias > 0.05 {
		return fmt.Errorf("bias %f out of bounds", c.OracleBias)
	}
	if c.OracleWeighting != nil {
		w := *c.OracleWeighting
		if w < 0 || w > 1 {
			return fmt.Errorf("oracle weighting %f out of bounds", w)
		}
	}

	if c.DriftTolerance == 0 {
		c.DriftTolerance = 0.001
	}
	if c.DriftTolerance < 0 || c.DriftTolerance > 0.01 {
		return fmt.Errorf("drift tolerance %f out of bounds", c.DriftTolerance)
	}

	if c.GapStrategy != GapStrategyMultiplier &&
		c.GapStrategy != GapStrategyPercent &&
		c.GapStrategy != GapStrategyPercentPlus &&
		c.GapStrategy != GapStrategyAbsolute &&
		c.GapStrategy != GapStrategyAbsolutePlus {
		return fmt.Errorf("unknown gap strategy %q", c.GapStrategy)
	}

	validatePlacement := func(p *OrderPlacement) error {
		var limits [2]float64
		switch c.GapStrategy {
		case GapStrategyMultiplier:
			limits = [2]float64{1, 100}
		case GapStrategyPercent, GapStrategyPercentPlus:
			limits = [2]float64{0, 0.1}
		case GapStrategyAbsolute, GapStrategyAbsolutePlus:
			limits = [2]float64{0, math.MaxFloat64} // validate at < spot price at creation time
		default:
			return fmt.Errorf("unknown gap strategy %q", c.GapStrategy)
		}

		if p.GapFactor < limits[0] || p.GapFactor > limits[1] {
			return fmt.Errorf("%s gap factor %f is out of bounds %+v", c.GapStrategy, p.GapFactor, limits)
		}

		return nil
	}

	sellPlacements := make(map[float64]interface{}, len(c.SellPlacements))
	for _, p := range c.SellPlacements {
		if _, duplicate := sellPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate sell placement %f", p.GapFactor)
		}
		sellPlacements[p.GapFactor] = true
		if err := validatePlacement(p); err != nil {
			return fmt.Errorf("invalid sell placement: %w", err)
		}
	}

	buyPlacements := make(map[float64]interface{}, len(c.BuyPlacements))
	for _, p := range c.BuyPlacements {
		if _, duplicate := buyPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate buy placement %f", p.GapFactor)
		}
		if err := validatePlacement(p); err != nil {
			return fmt.Errorf("invalid buy placement: %w", err)
		}
	}

	return nil
}

// steppedRate rounds the rate to the nearest integer multiple of the step.
// The minimum returned value is step.
func steppedRate(r, step uint64) uint64 {
	steps := math.Round(float64(r) / float64(step))
	if steps == 0 {
		return step
	}
	return uint64(math.Round(steps * float64(step)))
}

// orderFees are the fees used for calculating the half-spread.
type orderFees struct {
	swap       uint64
	redemption uint64
	funding    uint64
}

type basicMarketMaker struct {
	ctx    context.Context
	host   string
	base   uint32
	quote  uint32
	cfg    *MarketMakingConfig
	book   dexOrderBook
	log    dex.Logger
	core   clientCore
	oracle oracle
	mkt    *core.Market
	// TODO: update fees occasionally
	buyFees  *orderFees
	sellFees *orderFees

	rebalanceRunning atomic.Bool

	ordMtx         sync.RWMutex
	ords           map[order.OrderID]*core.Order
	oidToPlacement map[order.OrderID]int

	// the fiat rate is the rate determined by comparing the fiat rates
	// of the two assets.
	fiatRateV atomic.Uint64
}

// groupedOrder is a subset of an *core.Order used internally for sorting.
type groupedOrder struct {
	*core.Order
	id   order.OrderID
	rate uint64
	lots uint64
}

// groupedOrders returns lists of buy and sell orders, with buys sorted
// high to low by rate, and sells low to high.
func (m *basicMarketMaker) groupedOrders() (buys, sells map[int][]*groupedOrder) {
	makeGroupedOrder := func(o *core.Order) *groupedOrder {
		var oid order.OrderID
		copy(oid[:], o.ID)
		return &groupedOrder{
			Order: o,
			id:    oid,
			rate:  o.Rate,
			lots:  (o.Qty - o.Filled) / m.mkt.LotSize,
		}
	}

	buys, sells = make(map[int][]*groupedOrder), make(map[int][]*groupedOrder)
	m.ordMtx.RLock()
	for _, ord := range m.ords {
		var oid order.OrderID
		copy(oid[:], ord.ID)
		placementIndex := m.oidToPlacement[oid]
		if ord.Sell {
			if _, found := sells[placementIndex]; !found {
				sells[placementIndex] = make([]*groupedOrder, 0, 1)
			}
			sells[placementIndex] = append(sells[placementIndex], makeGroupedOrder(ord))
		} else {
			if _, found := buys[placementIndex]; !found {
				buys[placementIndex] = make([]*groupedOrder, 0, 1)
			}
			buys[placementIndex] = append(buys[placementIndex], makeGroupedOrder(ord))
		}
	}
	m.ordMtx.RUnlock()

	return buys, sells
}

func basisPrice(book dexOrderBook, oracle oracle, cfg *MarketMakingConfig, mkt *core.Market, fiatRate uint64, log dex.Logger) uint64 {
	midGap, err := book.MidGap()
	if err != nil && !errors.Is(err, orderbook.ErrEmptyOrderbook) {
		log.Errorf("MidGap error: %v", err)
		return 0
	}

	basisPrice := float64(midGap) // float64 message-rate units

	var oracleWeighting, oraclePrice float64
	if cfg.OracleWeighting != nil && *cfg.OracleWeighting > 0 {
		oracleWeighting = *cfg.OracleWeighting
		oraclePrice = oracle.GetMarketPrice(mkt.BaseID, mkt.QuoteID)
		if oraclePrice == 0 {
			log.Warnf("no oracle price available for %s bot", mkt.Name)
		}
	}

	if oraclePrice > 0 {
		msgOracleRate := float64(mkt.ConventionalRateToMsg(oraclePrice))

		// Apply the oracle mismatch filter.
		if basisPrice > 0 {
			low, high := msgOracleRate*(1-maxOracleMismatch), msgOracleRate*(1+maxOracleMismatch)
			if basisPrice < low {
				log.Debug("local mid-gap is below safe range. Using effective mid-gap of %d%% below the oracle rate.", maxOracleMismatch*100)
				basisPrice = low
			} else if basisPrice > high {
				log.Debug("local mid-gap is above safe range. Using effective mid-gap of %d%% above the oracle rate.", maxOracleMismatch*100)
				basisPrice = high
			}
		}

		if cfg.OracleBias != 0 {
			msgOracleRate *= 1 + cfg.OracleBias
		}

		if basisPrice == 0 { // no mid-gap available. Use the oracle price.
			basisPrice = msgOracleRate
			log.Tracef("basisPrice: using basis price %.0f from oracle because no mid-gap was found in order book", basisPrice)
		} else if oracleWeighting > 0 {
			basisPrice = msgOracleRate*oracleWeighting + basisPrice*(1-oracleWeighting)
			log.Tracef("basisPrice: oracle-weighted basis price = %f", basisPrice)
		}
	}

	if basisPrice > 0 {
		return steppedRate(uint64(basisPrice), mkt.RateStep)
	}

	if fiatRate > 0 {
		return steppedRate(fiatRate, mkt.RateStep)
	}

	if cfg.EmptyMarketRate > 0 {
		msgRate := mkt.ConventionalRateToMsg(cfg.EmptyMarketRate)
		return steppedRate(msgRate, mkt.RateStep)
	}

	return 0
}

func (m *basicMarketMaker) basisPrice() uint64 {
	return basisPrice(m.book, m.oracle, m.cfg, m.mkt, m.fiatRateV.Load(), m.log)
}

func calculateHalfSpread(basisPrice uint64, host string, base, quote uint32, mkt *core.Market, c clientCore) (halfSpread, baseFees, quoteFees uint64, err error) {
	form := &core.SingleLotFeesForm{
		Host:  host,
		Base:  base,
		Quote: quote,
		Sell:  true,
	}

	if basisPrice == 0 { // prevent divide by zero later
		return 0, 0, 0, fmt.Errorf("basis price cannot be zero")
	}

	baseFees, quoteFees, err = c.SingleLotFees(form)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("SingleLotFees error: %v", err)
	}

	form.Sell = false
	newQuoteFees, newBaseFees, err := c.SingleLotFees(form)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("SingleLotFees error: %v", err)
	}

	baseFees += newBaseFees
	quoteFees += newQuoteFees

	g := float64(calc.BaseToQuote(basisPrice, baseFees)+quoteFees) /
		float64(baseFees+2*mkt.LotSize) * mkt.AtomToConv

	halfGap := uint64(math.Round(g * calc.RateEncodingFactor))

	return halfGap, baseFees, quoteFees, nil
}

func (m *basicMarketMaker) halfSpread(basisPrice uint64) (uint64, error) {
	halfGap, baseFees, quoteFees, err := calculateHalfSpread(basisPrice, m.host, m.base, m.quote, m.mkt, m.core)
	if err != nil {
		return 0, err
	}

	m.log.Tracef("halfSpread: base basis price = %d, lot size = %d, base fees = %d, quote fees = %d, half-gap = %d",
		basisPrice, m.mkt.LotSize, baseFees, quoteFees, halfGap)

	return halfGap, nil
}

func (m *basicMarketMaker) placeMultiTrade(placements []*rateLots, sell bool) {
	qtyRates := make([]*core.QtyRate, 0, len(placements))
	for _, p := range placements {
		qtyRates = append(qtyRates, &core.QtyRate{
			Qty:  p.Lots * m.mkt.LotSize,
			Rate: p.Rate,
		})
	}

	options := map[string]string{}
	if m.cfg.SplitTxAllowed {
		options["multisplit"] = "true"
	}
	if !sell {
		if m.cfg.SplitBuffer != nil {
			options["multisplitbuffer"] = fmt.Sprintf("%d", *m.cfg.SplitBuffer)
		}
	}

	orders, err := m.core.MultiTrade(nil, &core.MultiTradeForm{
		Host:       m.host,
		Sell:       sell,
		Base:       m.base,
		Quote:      m.quote,
		Placements: qtyRates,
		Options:    options,
	})
	if err != nil {
		m.log.Errorf("Error placing rebalancing order: %v", err)
		return
	}

	m.ordMtx.Lock()
	for i, ord := range orders {
		var oid order.OrderID
		copy(oid[:], ord.ID)
		m.ords[oid] = ord
		placementIndex := placements[i].placementIndex
		m.oidToPlacement[oid] = placementIndex
	}
	m.ordMtx.Unlock()
}

func (m *basicMarketMaker) processFiatRates(rates map[uint32]float64) {
	var fiatRate uint64

	baseRate := rates[m.base]
	quoteRate := rates[m.quote]
	if baseRate > 0 && quoteRate > 0 {
		fiatRate = m.mkt.ConventionalRateToMsg(baseRate / quoteRate)
	}

	m.fiatRateV.Store(fiatRate)
}

func (m *basicMarketMaker) processTrade(o *core.Order) {
	if len(o.ID) == 0 {
		return
	}

	var oid order.OrderID
	copy(oid[:], o.ID)

	m.log.Tracef("processTrade: oid = %s, status = %s", oid, o.Status)

	m.ordMtx.Lock()
	defer m.ordMtx.Unlock()
	_, found := m.ords[oid]
	if !found {
		return
	}

	convRate := m.mkt.MsgRateToConventional(o.Rate)
	m.log.Tracef("processTrade: oid = %s, status = %s, qty = %d, filled = %d, rate = %f", oid, o.Status, o.Qty, o.Filled, convRate)

	if o.Status > order.OrderStatusBooked {
		// We stop caring when the order is taken off the book.
		delete(m.ords, oid)
		delete(m.oidToPlacement, oid)

		switch {
		case o.Filled == o.Qty:
			m.log.Tracef("processTrade: order filled")
		case o.Status == order.OrderStatusCanceled:
			if len(o.Matches) == 0 {
				m.log.Tracef("processTrade: order canceled WITHOUT matches")
			} else {
				m.log.Tracef("processTrade: order canceled WITH matches")
			}
		}
		return
	} else {
		// Update our reference.
		m.ords[oid] = o
	}
}

type rebalancer interface {
	basisPrice() uint64
	halfSpread(uint64) (uint64, error)
	groupedOrders() (buys, sells map[int][]*groupedOrder)
}

type rateLots struct {
	Rate           uint64 `json:"rate"`
	Lots           uint64 `json:"lots"`
	placementIndex int
}

func (m *basicMarketMaker) rebalance(newEpoch uint64) {
	if !m.rebalanceRunning.CompareAndSwap(false, true) {
		return
	}
	defer m.rebalanceRunning.Store(false)

	cancels, buyOrders, sellOrders := basicMMRebalance(newEpoch, m, m.core, m.cfg, m.mkt, m.buyFees, m.sellFees, m.log)

	for _, cancel := range cancels {
		err := m.core.Cancel(cancel)
		if err != nil {
			m.log.Errorf("Error canceling order %s: %v", cancel, err)
			return
		}
	}

	if len(buyOrders) > 0 {
		m.placeMultiTrade(buyOrders, false)
	}
	if len(sellOrders) > 0 {
		m.placeMultiTrade(sellOrders, true)
	}
}

func orderPrice(basisPrice, breakEven uint64, strategy GapStrategy, factor float64, sell bool, mkt *core.Market) uint64 {
	var halfSpread uint64

	// Apply the base strategy.
	switch strategy {
	case GapStrategyMultiplier:
		halfSpread = uint64(math.Round(float64(breakEven) * factor))
	case GapStrategyPercent, GapStrategyPercentPlus:
		halfSpread = uint64(math.Round(factor * float64(basisPrice)))
	case GapStrategyAbsolute, GapStrategyAbsolutePlus:
		halfSpread = mkt.ConventionalRateToMsg(factor)
	}

	// Add the break-even to the "-plus" strategies
	switch strategy {
	case GapStrategyAbsolutePlus, GapStrategyPercentPlus:
		halfSpread += breakEven
	}

	halfSpread = steppedRate(halfSpread, mkt.RateStep)

	if sell {
		return basisPrice + halfSpread
	}

	if basisPrice < halfSpread {
		return 0
	}

	return basisPrice - halfSpread
}

func basicMMRebalance(newEpoch uint64, m rebalancer, c clientCore, cfg *MarketMakingConfig, mkt *core.Market, buyFees, sellFees *orderFees, log dex.Logger) (cancels []dex.Bytes, buyOrders, sellOrders []*rateLots) {
	basisPrice := m.basisPrice()
	if basisPrice == 0 {
		log.Errorf("No basis price available")
		return
	}

	log.Debugf("rebalance (%s): basis price = %d", mkt.Name, basisPrice)

	var breakEven uint64
	if needBreakEvenHalfSpread(cfg.GapStrategy) {
		var err error
		breakEven, err = m.halfSpread(basisPrice)
		if err != nil {
			log.Errorf("Could not calculate break-even spread: %v", err)
			return
		}
	}

	existingBuys, existingSells := m.groupedOrders()
	var highestExistingBuy, lowestExistingSell uint64 = 0, math.MaxUint64
	for _, placementOrders := range existingBuys {
		for _, o := range placementOrders {
			if o.rate > highestExistingBuy {
				highestExistingBuy = o.rate
			}
		}
	}
	for _, placementOrders := range existingSells {
		for _, o := range placementOrders {
			if o.rate < lowestExistingSell {
				lowestExistingSell = o.rate
			}
		}
	}
	getExistingOrders := func(index int, sell bool) []*groupedOrder {
		if sell {
			return existingSells[index]
		}
		return existingBuys[index]
	}

	rateCausesSelfMatch := func(rate uint64, sell bool) bool {
		if sell {
			return rate <= highestExistingBuy
		}
		return rate >= lowestExistingSell
	}

	withinTolerance := func(rate, target uint64) bool {
		driftTolerance := uint64(float64(target) * cfg.DriftTolerance)
		lowerBound := target - driftTolerance
		upperBound := target + driftTolerance
		return rate >= lowerBound && rate <= upperBound
	}

	baseBalance, err := c.AssetBalance(mkt.BaseID)
	if err != nil {
		log.Errorf("Error getting base balance: %v", err)
		return
	}

	quoteBalance, err := c.AssetBalance(mkt.QuoteID)
	if err != nil {
		log.Errorf("Error getting quote balance: %v", err)
		return
	}

	cancels = make([]dex.Bytes, 0, len(cfg.SellPlacements)+len(cfg.BuyPlacements))
	addCancel := func(o *groupedOrder) {
		if newEpoch-o.Epoch < 2 {
			log.Debugf("rebalance: skipping cancel not past free cancel threshold")
			return
		}
		cancels = append(cancels, o.ID[:])
	}

	processSide := func(sell bool) []*rateLots {
		log.Tracef("rebalance: processing %s side", map[bool]string{true: "sell", false: "buy"}[sell])
		var placements []*OrderPlacement
		if sell {
			placements = cfg.SellPlacements
		} else {
			placements = cfg.BuyPlacements
		}
		if len(placements) == 0 {
			return nil
		}

		rlPlacements := make([]*rateLots, 0, len(placements))

		var remainingBalance uint64
		if sell {
			remainingBalance = baseBalance.Available
			if cfg.SplitTxAllowed {
				remainingBalance -= sellFees.funding
			}
		} else {
			remainingBalance = quoteBalance.Available
			if cfg.SplitTxAllowed {
				remainingBalance -= buyFees.funding
			}
		}

		for i, p := range placements {
			placementRate := orderPrice(basisPrice, breakEven, cfg.GapStrategy, p.GapFactor, sell, mkt)
			if placementRate == 0 {
				log.Tracef("rebalance: skipping placement %d because rate is zero", i)
				continue
			}
			if rateCausesSelfMatch(placementRate, sell) {
				log.Debugf("rebalance: skipping placement %d because it would cause a self-match", i)
				continue
			}
			existingOrders := getExistingOrders(i, sell)
			var requiredCancel bool
			var numLotsWithinTolerance uint64
			for _, o := range existingOrders {
				if !withinTolerance(o.rate, placementRate) {
					addCancel(o)
					requiredCancel = true
				} else {
					numLotsWithinTolerance += o.lots
				}
			}
			if requiredCancel {
				continue
			}

			log.Tracef("p.Lots = %d --- numLotsWithinTolerance = %d", p.Lots, numLotsWithinTolerance)
			if p.Lots > numLotsWithinTolerance {
				var requiredPerLot uint64
				if sell {
					requiredPerLot = sellFees.swap + mkt.LotSize
				} else {
					requiredPerLot = calc.BaseToQuote(placementRate, mkt.LotSize) + buyFees.swap
				}

				lotsToPlace := p.Lots - numLotsWithinTolerance
				if remainingBalance/requiredPerLot < lotsToPlace {
					lotsToPlace = remainingBalance / requiredPerLot
				}

				log.Tracef("rebalance: remainingBalance: %d, requiredPerLot %d", remainingBalance, requiredPerLot)
				log.Tracef("rebalance: placement %d placing %d lots", i, lotsToPlace)
				if lotsToPlace == 0 {
					continue
				}
				remainingBalance -= requiredPerLot * lotsToPlace
				rlPlacements = append(rlPlacements, &rateLots{
					Rate:           placementRate,
					Lots:           lotsToPlace,
					placementIndex: i,
				})
			}
		}

		return rlPlacements
	}

	buyOrders = processSide(false)
	sellOrders = processSide(true)

	return cancels, buyOrders, sellOrders
}

func (m *basicMarketMaker) handleNotification(note core.Notification) {
	switch n := note.(type) {
	case *core.OrderNote:
		ord := n.Order
		if ord == nil {
			return
		}
		m.processTrade(ord)
	case *core.EpochNotification:
		go m.rebalance(n.Epoch)
	case *core.FiatRatesNote:
		go m.processFiatRates(n.FiatRates)
	}
}

func (m *basicMarketMaker) cancelAllOrders() {
	m.ordMtx.Lock()
	defer m.ordMtx.Unlock()
	for oid := range m.ords {
		if err := m.core.Cancel(oid[:]); err != nil {
			m.log.Errorf("error cancelling order: %v", err)
		}
	}
	m.ords = make(map[order.OrderID]*core.Order)
	m.oidToPlacement = make(map[order.OrderID]int)
}

func (m *basicMarketMaker) run() {
	book, bookFeed, err := m.core.SyncBook(m.host, m.base, m.quote)
	if err != nil {
		m.log.Errorf("Failed to sync book: %v", err)
		return
	}
	m.book = book

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-bookFeed.Next():
				// Really nothing to do with the updates. We just need to keep
				// the subscription live in order to get a mid-gap rate when
				// needed. We could use this to trigger rebalances mid-epoch
				// though, which I think would provide some advantage.
			case <-m.ctx.Done():
				return
			}
		}
	}()

	noteFeed := m.core.NotificationFeed()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer noteFeed.ReturnFeed()
		for {
			select {
			case n := <-noteFeed.C:
				m.handleNotification(n)
			case <-m.ctx.Done():
				return
			}
		}
	}()

	wg.Wait()

	m.cancelAllOrders()
}

// RunBasicMarketMaker starts a basic market maker bot.
func RunBasicMarketMaker(ctx context.Context, cfg *BotConfig, c clientCore, oracle oracle, baseFiatRate, quoteFiatRate float64, log dex.Logger,
	notify func(core.Notification)) {
	if cfg.MMCfg == nil {
		// implies bug in caller
		log.Errorf("No market making config provided. Exiting.")
		return
	}

	err := cfg.MMCfg.Validate()
	if err != nil {
		notify(newValidationErrorNote(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset, fmt.Sprintf("invalid market making config: %v", err)))
		return
	}

	mkt, err := c.ExchangeMarket(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
	if err != nil {
		log.Errorf("Failed to get market: %v. Not starting market maker.", err)
		return
	}

	buySwapFees, buyRedeemFees, err := c.SingleLotFees(&core.SingleLotFeesForm{
		Host:          cfg.Host,
		Base:          cfg.BaseAsset,
		Quote:         cfg.QuoteAsset,
		UseMaxFeeRate: true,
		UseSafeTxSize: true,
	})
	if err != nil {
		log.Errorf("Failed to get fees: %v. Not starting market maker.", err)
		return
	}

	sellSwapFees, sellRedeemFees, err := c.SingleLotFees(&core.SingleLotFeesForm{
		Host:          cfg.Host,
		Base:          cfg.BaseAsset,
		Quote:         cfg.QuoteAsset,
		UseMaxFeeRate: true,
		UseSafeTxSize: true,
		Sell:          true,
	})
	if err != nil {
		log.Errorf("Failed to get fees: %v. Not starting market maker", err)
		return
	}

	buyFundingFees, err := c.MaxFundingFees(cfg.BaseAsset, uint32(len(cfg.MMCfg.BuyPlacements)), nil)
	if err != nil {
		log.Errorf("Failed to get funding fees: %v. Not starting market maker", err)
		return
	}

	sellFundingFees, err := c.MaxFundingFees(cfg.QuoteAsset, uint32(len(cfg.MMCfg.SellPlacements)), nil)
	if err != nil {
		log.Errorf("Failed to get funding fees: %v. Not starting market maker", err)
		return
	}

	var fiatRateV atomic.Uint64
	if baseFiatRate > 0 && quoteFiatRate > 0 {
		fiatRateV.Store(mkt.ConventionalRateToMsg(baseFiatRate / quoteFiatRate))
	}

	mm := &basicMarketMaker{
		ctx:            ctx,
		core:           c,
		log:            log,
		cfg:            cfg.MMCfg,
		host:           cfg.Host,
		base:           cfg.BaseAsset,
		quote:          cfg.QuoteAsset,
		oracle:         oracle,
		mkt:            mkt,
		ords:           make(map[order.OrderID]*core.Order),
		oidToPlacement: make(map[order.OrderID]int),
		buyFees: &orderFees{
			swap:       buySwapFees,
			redemption: buyRedeemFees,
			funding:    buyFundingFees,
		},
		sellFees: &orderFees{
			swap:       sellSwapFees,
			redemption: sellRedeemFees,
			funding:    sellFundingFees,
		},
	}

	if baseFiatRate > 0 && quoteFiatRate > 0 {
		mm.fiatRateV.Store(mkt.ConventionalRateToMsg(baseFiatRate / quoteFiatRate))
	}

	mm.run()
}
