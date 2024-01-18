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

// BasicMarketMakingConfig is the configuration for a simple market
// maker that places orders on both sides of the order book.
type BasicMarketMakingConfig struct {
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

	// BaseOptions are the multi-order options for the base asset wallet.
	BaseOptions map[string]string `json:"baseOptions"`

	// QuoteOptions are the multi-order options for the quote asset wallet.
	QuoteOptions map[string]string `json:"quoteOptions"`
}

func needBreakEvenHalfSpread(strat GapStrategy) bool {
	return strat == GapStrategyAbsolutePlus || strat == GapStrategyPercentPlus || strat == GapStrategyMultiplier
}

func (c *BasicMarketMakingConfig) Validate() error {
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

	sellPlacements := make(map[float64]bool, len(c.SellPlacements))
	for _, p := range c.SellPlacements {
		if _, duplicate := sellPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate sell placement %f", p.GapFactor)
		}
		sellPlacements[p.GapFactor] = true
		if err := validatePlacement(p); err != nil {
			return fmt.Errorf("invalid sell placement: %w", err)
		}
	}

	buyPlacements := make(map[float64]bool, len(c.BuyPlacements))
	for _, p := range c.BuyPlacements {
		if _, duplicate := buyPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate buy placement %f", p.GapFactor)
		}
		buyPlacements[p.GapFactor] = true
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

type basicMarketMaker struct {
	ctx              context.Context
	host             string
	base             uint32
	quote            uint32
	cfg              *BasicMarketMakingConfig
	book             dexOrderBook
	log              dex.Logger
	core             botCoreAdaptor
	oracle           oracle
	mkt              *core.Market
	rebalanceRunning atomic.Bool
}

// basisPrice calculates the basis price for the market maker.
// The mid-gap of the dex order book is used, and if oracles are
// available, and the oracle weighting is > 0, the oracle price
// is used to adjust the basis price.
// If the dex market is empty, but there are oracles available and
// oracle weighting is > 0, the oracle rate is used.
// If the dex market is empty and there are either no oracles available
// or oracle weighting is 0, the fiat rate is used.
// If there is no fiat rate available, the empty market rate in the
// configuration is used.
func basisPrice(book dexOrderBook, oracle oracle, cfg *BasicMarketMakingConfig, mkt *core.Market, fiatRate uint64, log dex.Logger) uint64 {
	midGap, err := book.MidGap()
	if err != nil && !errors.Is(err, orderbook.ErrEmptyOrderbook) {
		log.Errorf("MidGap error: %v", err)
		return 0
	}

	basisPrice := float64(midGap) // float64 message-rate units

	var oracleWeighting, oraclePrice float64
	if cfg.OracleWeighting != nil && *cfg.OracleWeighting > 0 {
		oracleWeighting = *cfg.OracleWeighting
		oraclePrice = oracle.getMarketPrice(mkt.BaseID, mkt.QuoteID)
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
		} else {
			basisPrice = msgOracleRate*oracleWeighting + basisPrice*(1-oracleWeighting)
			log.Tracef("basisPrice: oracle-weighted basis price = %f", basisPrice)
		}
	}

	if basisPrice > 0 {
		return steppedRate(uint64(basisPrice), mkt.RateStep)
	}

	// TODO: add a configuration to turn off use of fiat rate?
	if fiatRate > 0 {
		return steppedRate(fiatRate, mkt.RateStep)
	}

	if cfg.EmptyMarketRate > 0 {
		emptyMsgRate := mkt.ConventionalRateToMsg(cfg.EmptyMarketRate)
		return steppedRate(emptyMsgRate, mkt.RateStep)
	}

	return 0
}

func (m *basicMarketMaker) mktRateUsingFiatSources() uint64 {
	baseRate := m.core.FiatRate(m.base)
	quoteRate := m.core.FiatRate(m.quote)
	if baseRate == 0 || quoteRate == 0 {
		return 0
	}
	return m.mkt.ConventionalRateToMsg(baseRate / quoteRate)

}

func (m *basicMarketMaker) basisPrice() uint64 {
	return basisPrice(m.book, m.oracle, m.cfg, m.mkt, m.mktRateUsingFiatSources(), m.log)
}

func (m *basicMarketMaker) halfSpread(basisPrice uint64) (uint64, error) {
	form := &core.SingleLotFeesForm{
		Host:  m.host,
		Base:  m.base,
		Quote: m.quote,
		Sell:  true,
	}

	if basisPrice == 0 { // prevent divide by zero later
		return 0, fmt.Errorf("basis price cannot be zero")
	}

	baseFees, quoteFees, _, err := m.core.SingleLotFees(form)
	if err != nil {
		return 0, fmt.Errorf("SingleLotFees error: %v", err)
	}

	form.Sell = false
	newQuoteFees, newBaseFees, _, err := m.core.SingleLotFees(form)
	if err != nil {
		return 0, fmt.Errorf("SingleLotFees error: %v", err)
	}

	baseFees += newBaseFees
	quoteFees += newQuoteFees

	g := float64(calc.BaseToQuote(basisPrice, baseFees)+quoteFees) /
		float64(baseFees+2*m.mkt.LotSize) * m.mkt.AtomToConv

	halfGap := uint64(math.Round(g * calc.RateEncodingFactor))

	m.log.Tracef("halfSpread: base basis price = %d, lot size = %d, base fees = %d, quote fees = %d, half-gap = %d",
		basisPrice, m.mkt.LotSize, baseFees, quoteFees, halfGap)

	return halfGap, nil
}

func (m *basicMarketMaker) groupedOrders() (buys, sells map[uint64][]*core.Order) {
	return m.core.GroupedBookedOrders()
}

func (m *basicMarketMaker) placeMultiTrade(placements []*rateLots, sell bool) {
	multiTradePlacements := make([]*multiTradePlacement, 0, len(placements))
	for _, p := range placements {
		multiTradePlacements = append(multiTradePlacements, &multiTradePlacement{
			qty:      p.lots * m.mkt.LotSize,
			rate:     p.rate,
			grouping: uint64(p.placementIndex),
		})
	}

	var options map[string]string
	if sell {
		options = m.cfg.BaseOptions
	} else {
		options = m.cfg.QuoteOptions
	}

	_, err := m.core.MultiTrade(&multiTradeForm{
		host:       m.host,
		sell:       sell,
		baseID:     m.base,
		quoteID:    m.quote,
		placements: multiTradePlacements,
		options:    options,
	})
	if err != nil {
		m.log.Errorf("Error placing rebalancing order: %v", err)
		return
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

type rebalancer interface {
	basisPrice() uint64
	halfSpread(uint64) (uint64, error)
	groupedOrders() (buys, sells map[uint64][]*core.Order)
}

type rateLots struct {
	rate           uint64
	lots           uint64
	placementIndex int
}

func basicMMRebalance(newEpoch uint64, m rebalancer, c botCoreAdaptor, cfg *BasicMarketMakingConfig, mkt *core.Market, buyFees,
	sellFees *orderFees, log dex.Logger) (cancels []dex.Bytes, buyOrders, sellOrders []*rateLots) {
	basisPrice := m.basisPrice()
	if basisPrice == 0 {
		log.Errorf("No basis price available and no empty-market rate set")
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
	getExistingOrders := func(index uint64, sell bool) []*core.Order {
		if sell {
			return existingSells[index]
		}
		return existingBuys[index]
	}

	// Get highest existing buy and lowest existing sell to avoid
	// self-matches.
	var highestExistingBuy, lowestExistingSell uint64 = 0, math.MaxUint64
	for _, placementOrders := range existingBuys {
		for _, o := range placementOrders {
			if o.Rate > highestExistingBuy {
				highestExistingBuy = o.Rate
			}
		}
	}
	for _, placementOrders := range existingSells {
		for _, o := range placementOrders {
			if o.Rate < lowestExistingSell {
				lowestExistingSell = o.Rate
			}
		}
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

	baseBalance, err := c.DEXBalance(mkt.BaseID)
	if err != nil {
		log.Errorf("Error getting base balance: %v", err)
		return
	}
	quoteBalance, err := c.DEXBalance(mkt.QuoteID)
	if err != nil {
		log.Errorf("Error getting quote balance: %v", err)
		return
	}

	cancels = make([]dex.Bytes, 0, len(cfg.SellPlacements)+len(cfg.BuyPlacements))
	addCancel := func(o *core.Order) {
		if newEpoch-o.Epoch < 2 { // TODO: check epoch
			log.Debugf("rebalance: skipping cancel not past free cancel threshold")
			return
		}
		cancels = append(cancels, o.ID)
	}

	processSide := func(sell bool) []*rateLots {
		log.Debugf("rebalance: processing %s side", map[bool]string{true: "sell", false: "buy"}[sell])

		var cfgPlacements []*OrderPlacement
		if sell {
			cfgPlacements = cfg.SellPlacements
		} else {
			cfgPlacements = cfg.BuyPlacements
		}
		if len(cfgPlacements) == 0 {
			return nil
		}

		var remainingBalance uint64
		if sell {
			remainingBalance = baseBalance.Available
			if remainingBalance > sellFees.funding {
				remainingBalance -= sellFees.funding
			} else {
				return nil
			}
		} else {
			remainingBalance = quoteBalance.Available
			if remainingBalance > buyFees.funding {
				remainingBalance -= buyFees.funding
			} else {
				return nil
			}
		}

		rlPlacements := make([]*rateLots, 0, len(cfgPlacements))

		for i, p := range cfgPlacements {
			placementRate := orderPrice(basisPrice, breakEven, cfg.GapStrategy, p.GapFactor, sell, mkt)
			log.Debugf("placement %d rate: %d", i, placementRate)

			if placementRate == 0 {
				log.Warnf("skipping %s placement %d because it would result in a zero rate",
					map[bool]string{true: "sell", false: "buy"}[sell], i)
				continue
			}
			if rateCausesSelfMatch(placementRate, sell) {
				log.Warnf("skipping %s placement %d because it would cause a self-match",
					map[bool]string{true: "sell", false: "buy"}[sell], i)
				continue
			}

			existingOrders := getExistingOrders(uint64(i), sell)
			var numLotsOnBooks uint64
			for _, o := range existingOrders {
				lots := (o.Qty - o.Filled) / mkt.LotSize
				numLotsOnBooks += lots
				if !withinTolerance(o.Rate, placementRate) {
					addCancel(o)
				}
			}

			var lotsToPlace uint64
			if p.Lots > numLotsOnBooks {
				lotsToPlace = p.Lots - numLotsOnBooks
			}
			if lotsToPlace == 0 {
				continue
			}

			log.Debugf("placement %d: placing %d lots", i, lotsToPlace)

			// TODO: handle redeem/refund fees for account lockers
			var requiredPerLot uint64
			if sell {
				requiredPerLot = sellFees.swap + mkt.LotSize
			} else {
				requiredPerLot = calc.BaseToQuote(placementRate, mkt.LotSize) + buyFees.swap
			}
			if remainingBalance/requiredPerLot < lotsToPlace {
				log.Debugf("placement %d: not enough balance to place %d lots, placing %d", i, lotsToPlace, remainingBalance/requiredPerLot)
				lotsToPlace = remainingBalance / requiredPerLot
			}
			if lotsToPlace == 0 {
				continue
			}

			remainingBalance -= requiredPerLot * lotsToPlace
			rlPlacements = append(rlPlacements, &rateLots{
				rate:           placementRate,
				lots:           lotsToPlace,
				placementIndex: i,
			})
		}

		return rlPlacements
	}

	buyOrders = processSide(false)
	sellOrders = processSide(true)

	return cancels, buyOrders, sellOrders
}

func (m *basicMarketMaker) rebalance(newEpoch uint64) {
	if !m.rebalanceRunning.CompareAndSwap(false, true) {
		return
	}
	defer m.rebalanceRunning.Store(false)
	m.log.Tracef("rebalance: epoch %d", newEpoch)

	buyFees, sellFees, err := m.core.OrderFees()
	if err != nil {
		m.log.Errorf("Error getting order fees: %v", err)
		return
	}

	cancels, buyOrders, sellOrders := basicMMRebalance(newEpoch, m, m.core, m.cfg, m.mkt, buyFees, sellFees, m.log)

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

func (m *basicMarketMaker) run() {
	book, bookFeed, err := m.core.SyncBook(m.host, m.base, m.quote)
	if err != nil {
		m.log.Errorf("Failed to sync book: %v", err)
		return
	}
	m.book = book

	wg := sync.WaitGroup{}

	// Process book updates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case n := <-bookFeed.Next():
				if n.Action == core.EpochMatchSummary {
					payload := n.Payload.(*core.EpochMatchSummaryPayload)
					m.rebalance(payload.Epoch + 1)
				}
			case <-m.ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	m.core.CancelAllOrders()
}

// RunBasicMarketMaker starts a basic market maker bot.
func RunBasicMarketMaker(ctx context.Context, cfg *BotConfig, c botCoreAdaptor, oracle oracle, log dex.Logger) {
	if cfg.BasicMMConfig == nil {
		// implies bug in caller
		log.Errorf("No market making config provided. Exiting.")
		return
	}

	err := cfg.BasicMMConfig.Validate()
	if err != nil {
		c.Broadcast(newValidationErrorNote(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset, fmt.Sprintf("invalid market making config: %v", err)))
		return
	}

	mkt, err := c.ExchangeMarket(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
	if err != nil {
		log.Errorf("Failed to get market: %v. Not starting market maker.", err)
		return
	}

	mm := &basicMarketMaker{
		ctx:    ctx,
		core:   c,
		log:    log,
		cfg:    cfg.BasicMMConfig,
		host:   cfg.Host,
		base:   cfg.BaseAsset,
		quote:  cfg.QuoteAsset,
		oracle: oracle,
		mkt:    mkt,
	}

	mm.run()
}
