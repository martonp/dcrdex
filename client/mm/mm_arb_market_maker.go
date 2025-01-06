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

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/mm/libxc"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/order"
)

// ArbMarketMakingPlacement is the configuration for an order placement
// on the DEX order book based on the existing orders on a CEX order book.
type ArbMarketMakingPlacement struct {
	Lots       uint64  `json:"lots"`
	Multiplier float64 `json:"multiplier"`
}

type MultiHopCfg struct {
	BaseAssetMarket  [2]uint32 `json:"baseAssetMarket"`
	QuoteAssetMarket [2]uint32 `json:"quoteAssetMarket"`
}

// ArbMarketMakerConfig is the configuration for a market maker that places
// orders on both sides of the DEX order book, at rates where there are
// profitable counter trades on a CEX order book. Whenever a DEX order is
// filled, the opposite trade will immediately be made on the CEX.
//
// Each placement in BuyPlacements and SellPlacements represents an order
// that will be made on the DEX order book. The first placement will be
// placed at a rate closest to the CEX mid-gap, and each subsequent one
// will get farther.
//
// The bot calculates the extrema rate on the CEX order book where it can
// buy or sell the quantity of lots specified in the placement multiplied
// by the multiplier amount. This will be the rate of the expected counter
// trade. The bot will then place an order on the DEX order book where if
// both trades are filled, the bot will earn the profit specified in the
// configuration.
//
// The multiplier is important because it ensures that even if some of the
// trades closest to the mid-gap on the CEX order book are filled before
// the bot's orders on the DEX are matched, the bot will still be able to
// earn the expected profit.
//
// Consider the following example:
//
//	Market:
//		DCR/BTC, lot size = 10 DCR.
//
//	Sell Placements:
//		1. { Lots: 1, Multiplier: 1.5 }
//		2. { Lots 1, Multiplier: 1.0 }
//
//	 Profit:
//	   0.01 (1%)
//
//	CEX Asks:
//		1. 10 DCR @ .005 BTC/DCR
//		2. 10 DCR @ .006 BTC/DCR
//		3. 10 DCR @ .007 BTC/DCR
//
// For the first placement, the bot will find the rate at which it can
// buy 15 DCR (1 lot * 1.5 multiplier). This rate is .006 BTC/DCR. Therefore,
// it will place place a sell order at .00606 BTC/DCR (.006 BTC/DCR * 1.01).
//
// For the second placement, the bot will go deeper into the CEX order book
// and find the rate at which it can buy 25 DCR. This is the previous 15 DCR
// used for the first placement plus the Quantity * Multiplier of the second
// placement. This rate is .007 BTC/DCR. Therefore it will place a sell order
// at .00707 BTC/DCR (.007 BTC/DCR * 1.01).
type ArbMarketMakerConfig struct {
	BuyPlacements      []*ArbMarketMakingPlacement `json:"buyPlacements"`
	SellPlacements     []*ArbMarketMakingPlacement `json:"sellPlacements"`
	Profit             float64                     `json:"profit"`
	DriftTolerance     float64                     `json:"driftTolerance"`
	NumEpochsLeaveOpen uint64                      `json:"orderPersistence"`
	MultiHop           *MultiHopCfg                `json:"multiHop"`
}

func (c *ArbMarketMakerConfig) isMultiHop() bool {
	return c.MultiHop != nil
}

type placementLots struct {
	baseLots  uint64
	quoteLots uint64
}

type arbMarketMaker struct {
	*unifiedExchangeAdaptor
	cex              botCexAdaptor
	core             botCoreAdaptor
	cfgV             atomic.Value // *ArbMarketMakerConfig
	placementLotsV   atomic.Value // *placementLots
	book             dexOrderBook
	rebalanceRunning atomic.Bool
	currEpoch        atomic.Uint64

	matchesMtx    sync.Mutex
	matchesSeen   map[order.MatchID]bool
	pendingOrders map[order.OrderID]uint64 // orderID -> rate for counter trade on cex

	cexTradesMtx sync.RWMutex
	cexTrades    map[string]uint64
}

var _ bot = (*arbMarketMaker)(nil)

func (a *arbMarketMaker) cfg() *ArbMarketMakerConfig {
	return a.cfgV.Load().(*ArbMarketMakerConfig)
}

func (a *arbMarketMaker) shouldCompleteMultiHopArb(update *libxc.Trade) (isIntermediateTrade bool, baseID, quoteID, assetToTrade uint32, qty uint64) {
	cfg := a.cfg()
	if a.baseID == update.BaseID || a.baseID == update.QuoteID {
		baseID = cfg.MultiHop.QuoteAssetMarket[0]
		quoteID = cfg.MultiHop.QuoteAssetMarket[1]
		if a.baseID == update.BaseID {
			qty = update.QuoteFilled
			assetToTrade = update.QuoteID
			isIntermediateTrade = update.Qty > 0
		} else {
			qty = update.BaseFilled
			assetToTrade = update.BaseID
			isIntermediateTrade = update.QuoteQty > 0
		}
	} else {
		baseID = cfg.MultiHop.BaseAssetMarket[0]
		quoteID = cfg.MultiHop.BaseAssetMarket[1]
		if a.quoteID == update.QuoteID {
			qty = update.BaseFilled
			assetToTrade = update.BaseID
			isIntermediateTrade = update.QuoteQty > 0
		} else {
			qty = update.QuoteFilled
			assetToTrade = update.QuoteID
			isIntermediateTrade = update.Qty > 0
		}
	}

	if !isIntermediateTrade {
		return false, 0, 0, 0, 0
	}

	return true, baseID, quoteID, assetToTrade, qty
}

func (a *arbMarketMaker) handleCEXTradeUpdate(update *libxc.Trade) {
	if !update.Complete {
		return
	}

	a.cexTradesMtx.Lock()
	if _, ok := a.cexTrades[update.ID]; !ok {
		a.cexTradesMtx.Unlock()
		return
	}
	delete(a.cexTrades, update.ID)
	a.cexTradesMtx.Unlock()

	cfg := a.cfg()
	if !cfg.isMultiHop() {
		return
	}

	if !update.Market {
		a.log.Errorf("multi hop bot cex trade is not a market trade: %+v", update)
		return
	}

	isIntermediateTrade, baseID, quoteID, assetToTrade, qty := a.shouldCompleteMultiHopArb(update)
	if isIntermediateTrade {
		a.marketTradeOnCEX(baseID, quoteID, assetToTrade, qty)
	}
}

// tradeOnCEX executes a trade on the CEX.
func (a *arbMarketMaker) tradeOnCEX(doTrade func() (*libxc.Trade, error)) {
	a.cexTradesMtx.Lock()

	cexTrade, err := doTrade()
	if err != nil {
		a.cexTradesMtx.Unlock()
		a.log.Errorf("Error sending trade to CEX: %v", err)
		return
	}

	// Keep track of the epoch in which the trade was sent to the CEX. This way
	// the bot can cancel the trade if it is not filled after a certain number
	// of epochs.
	a.cexTrades[cexTrade.ID] = a.currEpoch.Load()
	a.cexTradesMtx.Unlock()

	a.handleCEXTradeUpdate(cexTrade)
}

func (a *arbMarketMaker) limitTradeOnCEX(rate, qty uint64, sell bool) {
	a.tradeOnCEX(func() (*libxc.Trade, error) {
		return a.cex.CEXTrade(a.ctx, a.baseID, a.quoteID, sell, rate, qty)
	})
}

func (a *arbMarketMaker) marketTradeOnCEX(baseID, quoteID, assetToTrade uint32, qty uint64) {
	a.tradeOnCEX(func() (*libxc.Trade, error) {
		return a.cex.CEXMarketTrade(a.ctx, baseID, quoteID, assetToTrade, qty)
	})
}

func (a *arbMarketMaker) intermediateMarketTradeOnCEX(sell bool, match *core.Match) {
	cfg := a.cfg()
	var baseID, quoteID, assetToTrade uint32
	var qty uint64
	if sell {
		baseID = cfg.MultiHop.QuoteAssetMarket[0]
		quoteID = cfg.MultiHop.QuoteAssetMarket[1]
		assetToTrade = a.quoteID
		qty = calc.BaseToQuote(match.Rate, match.Qty)
	} else {
		baseID = cfg.MultiHop.BaseAssetMarket[0]
		quoteID = cfg.MultiHop.BaseAssetMarket[1]
		assetToTrade = a.baseID
		qty = match.Qty
	}

	a.marketTradeOnCEX(baseID, quoteID, assetToTrade, qty)
}

func (a *arbMarketMaker) processDEXOrderUpdate(o *core.Order) {
	var orderID order.OrderID
	copy(orderID[:], o.ID)

	a.matchesMtx.Lock()
	defer a.matchesMtx.Unlock()

	cexRate, found := a.pendingOrders[orderID]
	if !found {
		return
	}

	for _, match := range o.Matches {
		var matchID order.MatchID
		copy(matchID[:], match.MatchID)

		if !a.matchesSeen[matchID] {
			a.matchesSeen[matchID] = true

			cfg := a.cfg()
			if cfg.isMultiHop() {
				a.intermediateMarketTradeOnCEX(o.Sell, match)
			} else {
				a.limitTradeOnCEX(cexRate, match.Qty, !o.Sell)
			}
		}
	}

	if !o.Status.IsActive() {
		delete(a.pendingOrders, orderID)
		for _, match := range o.Matches {
			var matchID order.MatchID
			copy(matchID[:], match.MatchID)
			delete(a.matchesSeen, matchID)
		}
	}
}

// cancelExpiredCEXTrades cancels any trades on the CEX that have been open for
// more than the number of epochs specified in the config.
func (a *arbMarketMaker) cancelExpiredCEXTrades() {
	currEpoch := a.currEpoch.Load()

	a.cexTradesMtx.RLock()
	defer a.cexTradesMtx.RUnlock()

	for tradeID, epoch := range a.cexTrades {
		if currEpoch-epoch >= a.cfg().NumEpochsLeaveOpen {
			err := a.cex.CancelTrade(a.ctx, a.baseID, a.quoteID, tradeID)
			if err != nil {
				a.log.Errorf("Error canceling CEX trade %s: %v", tradeID, err)
			}

			a.log.Infof("Cex trade %s was cancelled before it was filled", tradeID)
		}
	}
}

// dexPlacementRate calculates the rate at which an order should be placed on
// the DEX order book based on the rate of the counter trade on the CEX. The
// rate is calculated so that the difference in rates between the DEX and the
// CEX will pay for the network fees and still leave the configured profit.
func dexPlacementRate(cexRate uint64, sell bool, profitRate float64, mkt *market, feesInQuoteUnits uint64, log dex.Logger) (uint64, error) {
	var unadjustedRate uint64
	if sell {
		unadjustedRate = uint64(math.Round(float64(cexRate) * (1 + profitRate)))
	} else {
		unadjustedRate = uint64(math.Round(float64(cexRate) / (1 + profitRate)))
	}

	rateAdj := rateAdjustment(feesInQuoteUnits, mkt.lotSize)

	if log.Level() <= dex.LevelTrace {
		log.Tracef("%s %s placement rate: cexRate = %s, profitRate = %.3f, unadjustedRate = %s, rateAdj = %s, fees = %s",
			mkt.name, sellStr(sell), mkt.fmtRate(cexRate), profitRate, mkt.fmtRate(unadjustedRate), mkt.fmtRate(rateAdj), mkt.fmtQuoteFees(feesInQuoteUnits),
		)
	}

	if sell {
		return steppedRate(unadjustedRate+rateAdj, mkt.rateStep), nil
	}

	if rateAdj > unadjustedRate {
		return 0, fmt.Errorf("rate adjustment required for fees %d > rate %d", rateAdj, unadjustedRate)
	}

	return steppedRate(unadjustedRate-rateAdj, mkt.rateStep), nil
}

func rateAdjustment(feesInQuoteUnits, lotSize uint64) uint64 {
	return uint64(math.Round(float64(feesInQuoteUnits) / float64(lotSize) * calc.RateEncodingFactor))
}

// dexPlacementRate calculates the rate at which an order should be placed on
// the DEX order book based on the rate of the counter trade on the CEX. The
// logic is in the dexPlacementRate function, so that it can be separately
// tested.
func (a *arbMarketMaker) dexPlacementRate(cexRate uint64, sell bool) (uint64, error) {
	feesInQuoteUnits, err := a.OrderFeesInUnits(sell, false, cexRate)
	if err != nil {
		return 0, fmt.Errorf("error getting fees in quote units: %w", err)
	}
	return dexPlacementRate(cexRate, sell, a.cfg().Profit, a.market, feesInQuoteUnits, a.log)
}

func inverseRate(rate uint64, baseID, quoteID uint32) uint64 {
	baseUI, _ := asset.UnitInfo(baseID)
	quoteUI, _ := asset.UnitInfo(quoteID)
	convRate := calc.ConventionalRate(rate, baseUI, quoteUI)
	return calc.MessageRate(1/convRate, baseUI, quoteUI)
}

func aggregateRates(baseMarketRate, quoteMarketRate uint64, mkt *market, baseMarket, quoteMarket [2]uint32) uint64 {
	msgRate := func(rate float64, baseID, quoteID uint32) uint64 {
		baseUI, _ := asset.UnitInfo(baseID)
		quoteUI, _ := asset.UnitInfo(quoteID)
		return calc.MessageRate(rate, baseUI, quoteUI)
	}
	convRate := func(rate uint64, baseID, quoteID uint32) float64 {
		baseUI, _ := asset.UnitInfo(baseID)
		quoteUI, _ := asset.UnitInfo(quoteID)
		return calc.ConventionalRate(rate, baseUI, quoteUI)
	}
	if mkt.baseID != baseMarket[0] {
		baseMarketRate = inverseRate(baseMarketRate, baseMarket[0], baseMarket[1])
	}
	if mkt.quoteID != quoteMarket[0] {
		quoteMarketRate = inverseRate(quoteMarketRate, quoteMarket[0], quoteMarket[1])
	}
	convBaseRate := convRate(baseMarketRate, baseMarket[0], baseMarket[1])
	convQuoteRate := convRate(quoteMarketRate, quoteMarket[0], quoteMarket[1])
	convAggRate := convBaseRate / convQuoteRate
	return msgRate(convAggRate, mkt.baseID, mkt.quoteID)
}

type vwapFunc func(uint32, uint32, bool, uint64) (uint64, uint64, bool, error)

// multiHopExtrema calculates the extrema price for buying or selling a certain
// asset on one of the multi hop markets. assetID is the asset of which qty and
// sell refer. For example, on a DCR/USDT market, if assetID is USDT, and sell
// is true, this means that will be buying DCR on the CEX using qty USDT.
func multiHopExtrema(baseMarket bool, assetID uint32, qty uint64, sell bool, multiHopCfg *MultiHopCfg, mkt *market, vwapF, invVwapF vwapFunc) (extrema, counterQty uint64, filled bool, err error) {
	var baseID, quoteID uint32
	if baseMarket {
		baseID = multiHopCfg.BaseAssetMarket[0]
		quoteID = multiHopCfg.BaseAssetMarket[1]
	} else {
		baseID = multiHopCfg.QuoteAssetMarket[0]
		quoteID = multiHopCfg.QuoteAssetMarket[1]
	}

	var vwapFunc func(uint32, uint32, bool, uint64) (uint64, uint64, bool, error)
	var sellOnCEX bool
	if assetID == baseID {
		vwapFunc = vwapF
		sellOnCEX = sell
	} else {
		vwapFunc = invVwapF
		sellOnCEX = !sell
	}

	_, extrema, filled, err = vwapFunc(baseID, quoteID, sellOnCEX, qty)
	if err != nil {
		return 0, 0, false, fmt.Errorf("error getting VWAP: %w", err)
	}
	if !filled {
		return
	}

	if assetID == baseID {
		counterQty = calc.BaseToQuote(extrema, qty)
	} else {
		counterQty = calc.QuoteToBase(extrema, qty)
	}

	return
}

func multiHopRate(sellOnDEX bool, depth uint64, multiHopCfg *MultiHopCfg, mkt *market, vwap, invVwap vwapFunc) (uint64, bool, error) {
	intermediateAsset := multiHopCfg.BaseAssetMarket[0]
	if mkt.baseID == intermediateAsset {
		intermediateAsset = multiHopCfg.BaseAssetMarket[1]
	}

	baseRate, intAssetQty, filled, err := multiHopExtrema(true, mkt.baseID, depth, !sellOnDEX, multiHopCfg, mkt, vwap, invVwap)
	if err != nil {
		return 0, false, fmt.Errorf("error getting intermediate market VWAP: %w", err)
	}
	if !filled {
		return 0, false, nil
	}

	quoteRate, _, filled, err := multiHopExtrema(false, intermediateAsset, intAssetQty, !sellOnDEX, multiHopCfg, mkt, vwap, invVwap)
	if err != nil {
		return 0, false, fmt.Errorf("error getting target market VWAP: %w", err)
	}
	if !filled {
		return 0, false, nil
	}

	return aggregateRates(baseRate, quoteRate, mkt, multiHopCfg.BaseAssetMarket, multiHopCfg.QuoteAssetMarket), true, nil
}

func arbMMVWAP(sell bool, depth uint64, multiHopCfg *MultiHopCfg, mkt *market, vwap, invVwap vwapFunc) (uint64, bool, error) {
	if multiHopCfg != nil {
		return multiHopRate(sell, depth, multiHopCfg, mkt, vwap, invVwap)
	} else {
		_, rate, filled, err := vwap(mkt.baseID, mkt.quoteID, sell, depth)
		return rate, filled, err
	}
}

func (a *arbMarketMaker) ordersToPlace() (buys, sells []*TradePlacement, err error) {
	orders := func(cfgPlacements []*ArbMarketMakingPlacement, sellOnDEX bool) ([]*TradePlacement, error) {
		newPlacements := make([]*TradePlacement, 0, len(cfgPlacements))
		var cumulativeCEXDepth uint64
		for i, cfgPlacement := range cfgPlacements {
			cumulativeCEXDepth += uint64(float64(cfgPlacement.Lots*a.lotSize) * cfgPlacement.Multiplier)
			cexRate, filled, err := arbMMVWAP(sellOnDEX, cumulativeCEXDepth, a.cfg().MultiHop, a.market, a.CEX.VWAP, a.CEX.InvVWAP)
			if err != nil {
				return nil, fmt.Errorf("error getting VWAP: %w", err)
			}
			if a.log.Level() == dex.LevelTrace {
				a.log.Tracef("%s placement orders: %s placement # %d, lots = %d, cex rate = %s, filled = %t",
					a.name, sellStr(sellOnDEX), i, cfgPlacement.Lots, a.fmtRate(cexRate), filled,
				)
			}

			if !filled {
				newPlacements = append(newPlacements, &TradePlacement{})
				continue
			}

			placementRate, err := a.dexPlacementRate(cexRate, sellOnDEX)
			if err != nil {
				return nil, fmt.Errorf("error calculating DEX placement rate: %w", err)
			}

			newPlacements = append(newPlacements, &TradePlacement{
				Rate:             placementRate,
				Lots:             cfgPlacement.Lots,
				CounterTradeRate: cexRate,
			})
		}

		return newPlacements, nil
	}

	buys, err = orders(a.cfg().BuyPlacements, false)
	if err != nil {
		return
	}

	sells, err = orders(a.cfg().SellPlacements, true)
	return
}

// distribution parses the current inventory distribution and checks if better
// distributions are possible via deposit or withdrawal.
func (a *arbMarketMaker) distribution(additionalDEX, additionalCEX map[uint32]uint64) (dist *distribution, err error) {
	cfgI := a.placementLotsV.Load()
	if cfgI == nil {
		return nil, errors.New("no placements?")
	}
	placements := cfgI.(*placementLots)
	if placements.baseLots == 0 && placements.quoteLots == 0 {
		return nil, errors.New("zero placement lots?")
	}
	dexSellLots, dexBuyLots := placements.baseLots, placements.quoteLots
	dexBuyRate, dexSellRate, err := a.cexCounterRates(dexSellLots, dexBuyLots, a.cfg().MultiHop)
	if err != nil {
		return nil, fmt.Errorf("error getting cex counter-rates: %w", err)
	}
	adjustedBuy, err := a.dexPlacementRate(dexBuyRate, false)
	if err != nil {
		return nil, fmt.Errorf("error getting adjusted buy rate: %v", err)
	}
	adjustedSell, err := a.dexPlacementRate(dexSellRate, true)
	if err != nil {
		return nil, fmt.Errorf("error getting adjusted sell rate: %v", err)
	}

	perLot, err := a.lotCosts(adjustedBuy, adjustedSell)
	if perLot == nil {
		return nil, fmt.Errorf("error getting lot costs: %w", err)
	}
	dist = a.newDistribution(perLot)
	a.optimizeTransfers(dist, dexSellLots, dexBuyLots, dexSellLots, dexBuyLots, additionalDEX, additionalCEX)
	return dist, nil
}

// rebalance is called on each new epoch. It will calculate the rates orders
// need to be placed on the DEX orderbook based on the CEX orderbook, and
// potentially update the orders on the DEX orderbook. It will also process
// and potentially needed withdrawals and deposits, and finally cancel any
// trades on the CEX that have been open for more than the number of epochs
// specified in the config.
func (a *arbMarketMaker) rebalance(epoch uint64, book *orderbook.OrderBook) {
	if !a.rebalanceRunning.CompareAndSwap(false, true) {
		return
	}
	defer a.rebalanceRunning.Store(false)
	a.log.Tracef("rebalance: epoch %d", epoch)

	currEpoch := a.currEpoch.Load()
	if epoch <= currEpoch {
		return
	}
	a.currEpoch.Store(epoch)

	if !a.checkBotHealth(epoch) {
		a.tryCancelOrders(a.ctx, &epoch, false)
		return
	}

	actionTaken, err := a.tryTransfers(currEpoch, a.distribution)
	if err != nil {
		a.log.Errorf("Error performing transfers: %v", err)
	} else if actionTaken {
		return
	}

	var buysReport, sellsReport *OrderReport
	buyOrders, sellOrders, determinePlacementsErr := a.ordersToPlace()
	if determinePlacementsErr != nil {
		a.tryCancelOrders(a.ctx, &epoch, false)
	} else {
		var buys, sells map[order.OrderID]*dexOrderInfo
		buys, buysReport = a.multiTrade(buyOrders, false, a.cfg().DriftTolerance, currEpoch)
		for id, ord := range buys {
			a.matchesMtx.Lock()
			a.pendingOrders[id] = ord.counterTradeRate
			a.matchesMtx.Unlock()
		}

		sells, sellsReport = a.multiTrade(sellOrders, true, a.cfg().DriftTolerance, currEpoch)
		for id, ord := range sells {
			a.matchesMtx.Lock()
			a.pendingOrders[id] = ord.counterTradeRate
			a.matchesMtx.Unlock()
		}
	}

	epochReport := &EpochReport{
		BuysReport:  buysReport,
		SellsReport: sellsReport,
		EpochNum:    epoch,
	}
	epochReport.setPreOrderProblems(determinePlacementsErr)
	a.updateEpochReport(epochReport)

	a.cancelExpiredCEXTrades()
	a.registerFeeGap()
}

// TODO: test fee gap with simple arb mm, nil cfg
func feeGap(core botCoreAdaptor, multiHopCfg *MultiHopCfg, cex libxc.CEX, mkt *market) (*FeeGapStats, error) {
	s := &FeeGapStats{
		BasisPrice: cex.MidGap(mkt.baseID, mkt.quoteID),
	}
	buy, filled, err := arbMMVWAP(false, mkt.lotSize, multiHopCfg, mkt, cex.VWAP, cex.InvVWAP)
	if err != nil {
		return nil, fmt.Errorf("VWAP buy error: %w", err)
	}
	if !filled {
		return s, nil
	}
	sell, filled, err := arbMMVWAP(true, mkt.lotSize, multiHopCfg, mkt, cex.VWAP, cex.InvVWAP)
	if err != nil {
		return nil, fmt.Errorf("VWAP sell error: %w", err)
	}
	if !filled {
		return s, nil
	}
	s.RemoteGap = sell - buy
	if multiHopCfg != nil {
		s.BasisPrice = (buy + sell) / 2
	} else {
		s.BasisPrice = cex.MidGap(mkt.baseID, mkt.quoteID)
	}
	sellFeesInBaseUnits, err := core.OrderFeesInUnits(true, true, sell)
	if err != nil {
		return nil, fmt.Errorf("error getting sell fees: %w", err)
	}
	buyFeesInBaseUnits, err := core.OrderFeesInUnits(false, true, buy)
	if err != nil {
		return nil, fmt.Errorf("error getting buy fees: %w", err)
	}
	s.RoundTripFees = sellFeesInBaseUnits + buyFeesInBaseUnits
	feesInQuoteUnits := calc.BaseToQuote((sell+buy)/2, s.RoundTripFees)
	s.FeeGap = rateAdjustment(feesInQuoteUnits, mkt.lotSize)
	return s, nil
}

func (a *arbMarketMaker) registerFeeGap() {
	feeGap, err := feeGap(a.core, a.cfg().MultiHop, a.CEX, a.market)
	if err != nil {
		a.log.Warnf("error getting fee-gap stats: %v", err)
		return
	}
	a.unifiedExchangeAdaptor.registerFeeGap(feeGap)
}

func (a *arbMarketMaker) botLoop(ctx context.Context) (*sync.WaitGroup, error) {
	book, bookFeed, err := a.core.SyncBook(a.host, a.baseID, a.quoteID)
	if err != nil {
		return nil, fmt.Errorf("failed to sync book: %v", err)
	}
	a.book = book

	cexMkts := make([][2]uint32, 0, 2)
	if a.cfg().isMultiHop() {
		cexMkts = append(cexMkts, a.cfg().MultiHop.BaseAssetMarket)
		cexMkts = append(cexMkts, a.cfg().MultiHop.QuoteAssetMarket)
	} else {
		cexMkts = append(cexMkts, [2]uint32{a.baseID, a.quoteID})
	}
	for _, mkt := range cexMkts {
		fmt.Println("~~~~~~~~~~~~ subscribeMarket ", mkt[0], mkt[1])
		err = a.cex.SubscribeMarket(ctx, mkt[0], mkt[1])
		if err != nil {
			bookFeed.Close()
			return nil, fmt.Errorf("failed to subscribe to cex market: %v", err)
		}
	}

	tradeUpdates := a.cex.SubscribeTradeUpdates()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bookFeed.Close()
		for {
			select {
			case ni := <-bookFeed.Next():
				switch epoch := ni.Payload.(type) {
				case *core.ResolvedEpoch:
					a.rebalance(epoch.Current, book)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case update := <-tradeUpdates:
				a.handleCEXTradeUpdate(update)
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		orderUpdates := a.core.SubscribeOrderUpdates()
		for {
			select {
			case n := <-orderUpdates:
				a.processDEXOrderUpdate(n)
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		for _, mkt := range cexMkts {
			a.cex.UnsubscribeMarket(mkt[0], mkt[1])
		}
	}()

	a.registerFeeGap()

	return &wg, nil
}

func (a *arbMarketMaker) setTransferConfig(cfg *ArbMarketMakerConfig) {
	var baseLots, quoteLots uint64
	for _, p := range cfg.BuyPlacements {
		quoteLots += p.Lots
	}
	for _, p := range cfg.SellPlacements {
		baseLots += p.Lots
	}
	a.placementLotsV.Store(&placementLots{
		baseLots:  baseLots,
		quoteLots: quoteLots,
	})
}

func (a *arbMarketMaker) updateConfig(cfg *BotConfig) error {
	if cfg.ArbMarketMakerConfig == nil {
		return errors.New("no arb market maker config provided")
	}

	a.cfgV.Store(cfg.ArbMarketMakerConfig)
	a.setTransferConfig(cfg.ArbMarketMakerConfig)
	a.unifiedExchangeAdaptor.updateConfig(cfg)
	return nil
}

func newArbMarketMaker(cfg *BotConfig, adaptorCfg *exchangeAdaptorCfg, log dex.Logger) (*arbMarketMaker, error) {
	if cfg.ArbMarketMakerConfig == nil {
		// implies bug in caller
		return nil, errors.New("no arb market maker config provided")
	}

	adaptor, err := newUnifiedExchangeAdaptor(adaptorCfg)
	if err != nil {
		return nil, fmt.Errorf("error constructing exchange adaptor: %w", err)
	}

	arbMM := &arbMarketMaker{
		unifiedExchangeAdaptor: adaptor,
		cex:                    adaptor,
		core:                   adaptor,
		matchesSeen:            make(map[order.MatchID]bool),
		pendingOrders:          make(map[order.OrderID]uint64),
		cexTrades:              make(map[string]uint64),
	}

	adaptor.setBotLoop(arbMM.botLoop)

	arbMM.cfgV.Store(cfg.ArbMarketMakerConfig)
	arbMM.setTransferConfig(cfg.ArbMarketMakerConfig)
	return arbMM, nil
}
