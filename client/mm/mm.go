// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/mm/libxc"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

// clientCore is satisfied by core.Core.
type clientCore interface {
	NotificationFeed() *core.NoteFeed
	ExchangeMarket(host string, base, quote uint32) (*core.Market, error)
	SyncBook(host string, base, quote uint32) (*orderbook.OrderBook, core.BookFeed, error)
	SupportedAssets() map[uint32]*core.SupportedAsset
	SingleLotFees(form *core.SingleLotFeesForm) (uint64, uint64, uint64, error)
	Cancel(oidB dex.Bytes) error
	AssetBalance(assetID uint32) (*core.WalletBalance, error)
	WalletState(assetID uint32) *core.WalletState
	MultiTrade(pw []byte, form *core.MultiTradeForm) ([]*core.Order, error)
	MaxFundingFees(fromAsset uint32, host string, numTrades uint32, fromSettings map[string]string) (uint64, error)
	User() *core.User
	Login(pw []byte) error
	OpenWallet(assetID uint32, appPW []byte) error
	Broadcast(core.Notification)
	FiatConversionRates() map[uint32]float64
	Send(pw []byte, assetID uint32, value uint64, address string, subtract bool) (asset.Coin, error)
	NewDepositAddress(assetID uint32) (string, error)
	Order(oidB dex.Bytes) (*core.Order, error)
	WalletTransaction(uint32, string) (*asset.WalletTransaction, error)
}

var _ clientCore = (*core.Core)(nil)

// dexOrderBook is satisfied by orderbook.OrderBook.
// Avoids having to mock the entire orderbook in tests.
type dexOrderBook interface {
	MidGap() (uint64, error)
	VWAP(lots, lotSize uint64, sell bool) (avg, extrema uint64, filled bool, err error)
}

var _ dexOrderBook = (*orderbook.OrderBook)(nil)

// MarketWithHost represents a market on a specific dex server.
type MarketWithHost struct {
	Host    string `json:"host"`
	BaseID  uint32 `json:"base"`
	QuoteID uint32 `json:"quote"`
}

// MarketMaker handles the market making process. It supports running different
// strategies on different markets.
type MarketMaker struct {
	ctx     context.Context
	die     context.CancelFunc
	running atomic.Bool
	log     dex.Logger
	core    clientCore
	cfgPath string

	// syncedOracle is only available while the MarketMaker is running. It
	// periodically refreshes the prices for the markets that have bots
	// running on them.
	syncedOracleMtx sync.RWMutex
	syncedOracle    *priceOracle

	// unsyncedOracle is always available and can be used to query prices on
	// all markets. It does not periodically refresh the prices, and queries
	// them on demand.
	unsyncedOracle *priceOracle

	runningBotsMtx sync.RWMutex
	runningBots    map[MarketWithHost]interface{}

	ordersMtx  sync.RWMutex
	orderToBot map[order.OrderID]string
}

// NewMarketMaker creates a new MarketMaker.
func NewMarketMaker(c clientCore, cfgPath string, log dex.Logger) (*MarketMaker, error) {
	if _, err := os.Stat(cfgPath); err != nil {
		cfg := new(MarketMakingConfig)
		cfgB, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal empty config file: %w", err)
		}
		err = os.WriteFile(cfgPath, cfgB, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to write empty config file: %w", err)
		}
	}

	return &MarketMaker{
		core:           c,
		log:            log,
		cfgPath:        cfgPath,
		running:        atomic.Bool{},
		orderToBot:     make(map[order.OrderID]string),
		runningBots:    make(map[MarketWithHost]interface{}),
		unsyncedOracle: newUnsyncedPriceOracle(log),
	}, nil
}

// Running returns true if the MarketMaker is running.
func (m *MarketMaker) Running() bool {
	return m.running.Load()
}

// RunningBots returns the markets on which a bot is running.
func (m *MarketMaker) RunningBots() []MarketWithHost {
	m.runningBotsMtx.RLock()
	defer m.runningBotsMtx.RUnlock()

	mkts := make([]MarketWithHost, 0, len(m.runningBots))
	for mkt := range m.runningBots {
		mkts = append(mkts, mkt)
	}

	return mkts
}

func marketsRequiringPriceOracle(cfgs []*BotConfig) []*mkt {
	mkts := make([]*mkt, 0, len(cfgs))

	for _, cfg := range cfgs {
		if cfg.requiresPriceOracle() {
			mkts = append(mkts, &mkt{base: cfg.BaseAsset, quote: cfg.QuoteAsset})
		}
	}

	return mkts
}

func priceOracleFromConfigs(ctx context.Context, cfgs []*BotConfig, log dex.Logger) (*priceOracle, error) {
	var oracle *priceOracle
	var err error
	marketsRequiringOracle := marketsRequiringPriceOracle(cfgs)
	if len(marketsRequiringOracle) > 0 {
		oracle, err = newAutoSyncPriceOracle(ctx, marketsRequiringOracle, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create PriceOracle: %v", err)
		}
	}

	return oracle, nil
}

func (m *MarketMaker) markBotAsRunning(mkt MarketWithHost, running bool) {
	m.runningBotsMtx.Lock()
	defer m.runningBotsMtx.Unlock()
	if running {
		m.runningBots[mkt] = struct{}{}
	} else {
		delete(m.runningBots, mkt)
	}

	if len(m.runningBots) == 0 {
		m.die()
	}
}

// MarketReport returns information about the oracle rates on a market
// pair and the fiat rates of the base and quote assets.
func (m *MarketMaker) MarketReport(base, quote uint32) (*MarketReport, error) {
	fiatRates := m.core.FiatConversionRates()
	baseFiatRate := fiatRates[base]
	quoteFiatRate := fiatRates[quote]

	m.syncedOracleMtx.RLock()
	if m.syncedOracle != nil {
		price, oracles, err := m.syncedOracle.getOracleInfo(base, quote)
		if err != nil && !errors.Is(err, errUnsyncedMarket) {
			m.log.Errorf("failed to get oracle info for market %d-%d: %v", base, quote, err)
		}
		if err == nil {
			m.syncedOracleMtx.RUnlock()
			return &MarketReport{
				Price:         price,
				Oracles:       oracles,
				BaseFiatRate:  baseFiatRate,
				QuoteFiatRate: quoteFiatRate,
			}, nil
		}
	}
	m.syncedOracleMtx.RUnlock()

	price, oracles, err := m.unsyncedOracle.getOracleInfo(base, quote)
	if err != nil {
		return nil, err
	}

	return &MarketReport{
		Price:         price,
		Oracles:       oracles,
		BaseFiatRate:  baseFiatRate,
		QuoteFiatRate: quoteFiatRate,
	}, nil
}

func (m *MarketMaker) loginAndUnlockWallets(pw []byte, cfgs []*BotConfig) error {
	err := m.core.Login(pw)
	if err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}
	unlocked := make(map[uint32]any)
	for _, cfg := range cfgs {
		if _, done := unlocked[cfg.BaseAsset]; !done {
			err := m.core.OpenWallet(cfg.BaseAsset, pw)
			if err != nil {
				return fmt.Errorf("failed to unlock wallet for asset %d: %w", cfg.BaseAsset, err)
			}
			unlocked[cfg.BaseAsset] = true
		}

		if _, done := unlocked[cfg.QuoteAsset]; !done {
			err := m.core.OpenWallet(cfg.QuoteAsset, pw)
			if err != nil {
				return fmt.Errorf("failed to unlock wallet for asset %d: %w", cfg.QuoteAsset, err)
			}
			unlocked[cfg.QuoteAsset] = true
		}
	}

	return nil
}

// duplicateBotConfig returns an error if there is more than one bot config for
// the same market on the same dex host.
func duplicateBotConfig(cfgs []*BotConfig) error {
	mkts := make(map[string]struct{})

	for _, cfg := range cfgs {
		mkt := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
		if _, found := mkts[mkt]; found {
			return fmt.Errorf("duplicate bot config for market %s", mkt)
		}
		mkts[mkt] = struct{}{}
	}

	return nil
}

func validateAndFilterEnabledConfigs(cfgs []*BotConfig) ([]*BotConfig, error) {
	enabledCfgs := make([]*BotConfig, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.requiresCEX() && cfg.CEXCfg == nil {
			mktID := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
			return nil, fmt.Errorf("bot at %s requires cex config", mktID)
		}
		if !cfg.Disabled {
			enabledCfgs = append(enabledCfgs, cfg)
		}
	}
	if len(enabledCfgs) == 0 {
		return nil, errors.New("no enabled bots")
	}
	if err := duplicateBotConfig(enabledCfgs); err != nil {
		return nil, err
	}
	return enabledCfgs, nil
}

// botInitialBaseBalances returns the initial base balances for each bot.
func botInitialBaseBalances(cfgs []*BotConfig, core clientCore, cexes map[string]libxc.CEX) (dexBalances, cexBalances map[string]map[uint32]uint64, err error) {
	dexBalances = make(map[string]map[uint32]uint64, len(cfgs))
	cexBalances = make(map[string]map[uint32]uint64, len(cfgs))

	type trackedBalance struct {
		available uint64
		reserved  uint64
	}

	dexBalanceTracker := make(map[uint32]*trackedBalance)
	cexBalanceTracker := make(map[string]map[string]*trackedBalance)

	trackAssetOnDEX := func(assetID uint32) error {
		if _, found := dexBalanceTracker[assetID]; found {
			return nil
		}
		bal, err := core.AssetBalance(assetID)
		if err != nil {
			return fmt.Errorf("failed to get balance for asset %d: %v", assetID, err)
		}
		dexBalanceTracker[assetID] = &trackedBalance{
			available: bal.Available,
		}
		return nil
	}

	trackAssetOnCEX := func(assetSymbol string, assetID uint32, cexName string) error {
		cexBalances, found := cexBalanceTracker[cexName]
		if !found {
			cexBalanceTracker[cexName] = make(map[string]*trackedBalance)
			cexBalances = cexBalanceTracker[cexName]
		}

		if _, found := cexBalances[assetSymbol]; found {
			return nil
		}

		cex, found := cexes[cexName]
		if !found {
			return fmt.Errorf("no cex config for %s", cexName)
		}

		// TODO: what if conversion factors of an asset on different chains
		// are different? currently they are all the same.
		balance, err := cex.Balance(assetID)
		if err != nil {
			return err
		}

		cexBalances[assetSymbol] = &trackedBalance{
			available: balance.Available,
		}

		return nil
	}

	calcBalance := func(balType BalanceType, balAmount, availableBal uint64) uint64 {
		if balType == Percentage {
			return availableBal * balAmount / 100
		}
		return balAmount
	}

	for _, cfg := range cfgs {
		err := trackAssetOnDEX(cfg.BaseAsset)
		if err != nil {
			return nil, nil, err
		}
		err = trackAssetOnDEX(cfg.QuoteAsset)
		if err != nil {
			return nil, nil, err
		}

		mktID := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)

		// Calculate DEX balances
		baseBalance := dexBalanceTracker[cfg.BaseAsset]
		quoteBalance := dexBalanceTracker[cfg.QuoteAsset]
		baseRequired := calcBalance(cfg.BaseBalanceType, cfg.BaseBalance, baseBalance.available)
		quoteRequired := calcBalance(cfg.QuoteBalanceType, cfg.QuoteBalance, quoteBalance.available)
		if baseRequired == 0 && quoteRequired == 0 {
			return nil, nil, fmt.Errorf("both base and quote balance are zero for market %s", mktID)
		}
		if baseRequired > baseBalance.available-baseBalance.reserved {
			return nil, nil, fmt.Errorf("insufficient balance for asset %d", cfg.BaseAsset)
		}
		if quoteRequired > quoteBalance.available-quoteBalance.reserved {
			return nil, nil, fmt.Errorf("insufficient balance for asset %d", cfg.QuoteAsset)
		}
		baseBalance.reserved += baseRequired
		quoteBalance.reserved += quoteRequired

		dexBalances[mktID] = map[uint32]uint64{
			cfg.BaseAsset:  baseRequired,
			cfg.QuoteAsset: quoteRequired,
		}

		trackTokenFeeAsset := func(base bool) error {
			assetID := cfg.QuoteAsset
			balType := cfg.QuoteFeeAssetBalanceType
			balAmount := cfg.QuoteFeeAssetBalance
			baseOrQuote := "quote"
			if base {
				assetID = cfg.BaseAsset
				balType = cfg.BaseFeeAssetBalanceType
				balAmount = cfg.BaseFeeAssetBalance
				baseOrQuote = "base"
			}
			token := asset.TokenInfo(assetID)
			if token == nil {
				return nil
			}
			err := trackAssetOnDEX(token.ParentID)
			if err != nil {
				return err
			}
			tokenFeeAsset := dexBalanceTracker[token.ParentID]
			tokenFeeAssetRequired := calcBalance(balType, balAmount, tokenFeeAsset.available)
			if tokenFeeAssetRequired == 0 {
				return fmt.Errorf("%s fee asset balance is zero for market %s", baseOrQuote, mktID)
			}
			if tokenFeeAssetRequired > tokenFeeAsset.available-tokenFeeAsset.reserved {
				return fmt.Errorf("insufficient balance for asset %d", token.ParentID)
			}
			tokenFeeAsset.reserved += tokenFeeAssetRequired
			dexBalances[mktID][token.ParentID] += tokenFeeAssetRequired
			return nil
		}
		err = trackTokenFeeAsset(true)
		if err != nil {
			return nil, nil, err
		}
		err = trackTokenFeeAsset(false)
		if err != nil {
			return nil, nil, err
		}

		// Calculate CEX balances
		if cfg.CEXCfg != nil {
			baseSymbol := dex.BipIDSymbol(cfg.BaseAsset)
			if baseSymbol == "" {
				return nil, nil, fmt.Errorf("unknown asset ID %d", cfg.BaseAsset)
			}
			baseAssetSymbol := dex.TokenSymbol(baseSymbol)

			quoteSymbol := dex.BipIDSymbol(cfg.QuoteAsset)
			if quoteSymbol == "" {
				return nil, nil, fmt.Errorf("unknown asset ID %d", cfg.QuoteAsset)
			}
			quoteAssetSymbol := dex.TokenSymbol(quoteSymbol)

			err = trackAssetOnCEX(baseAssetSymbol, cfg.BaseAsset, cfg.CEXCfg.Name)
			if err != nil {
				return nil, nil, err
			}
			err = trackAssetOnCEX(quoteAssetSymbol, cfg.QuoteAsset, cfg.CEXCfg.Name)
			if err != nil {
				return nil, nil, err
			}
			baseCEXBalance := cexBalanceTracker[cfg.CEXCfg.Name][baseAssetSymbol]
			quoteCEXBalance := cexBalanceTracker[cfg.CEXCfg.Name][quoteAssetSymbol]
			cexBaseRequired := calcBalance(cfg.CEXCfg.BaseBalanceType, cfg.CEXCfg.BaseBalance, baseCEXBalance.available)
			cexQuoteRequired := calcBalance(cfg.QuoteBalanceType, cfg.QuoteBalance, quoteCEXBalance.available)
			if cexBaseRequired == 0 && cexQuoteRequired == 0 {
				return nil, nil, fmt.Errorf("both base and quote CEX balances are zero for market %s", mktID)
			}
			if cexBaseRequired > baseCEXBalance.available-baseCEXBalance.reserved {
				return nil, nil, fmt.Errorf("insufficient CEX base balance for asset %d", cfg.BaseAsset)
			}
			if cexQuoteRequired > quoteCEXBalance.available-quoteCEXBalance.reserved {
				return nil, nil, fmt.Errorf("insufficient CEX quote balance for asset %d", cfg.QuoteAsset)
			}
			baseCEXBalance.reserved += cexBaseRequired
			quoteCEXBalance.reserved += cexQuoteRequired

			cexBalances[mktID] = map[uint32]uint64{
				cfg.BaseAsset:  cexBaseRequired,
				cfg.QuoteAsset: cexQuoteRequired,
			}
		}
	}

	return dexBalances, cexBalances, nil
}

func (m *MarketMaker) initCEXConnections(cfgs []*CEXConfig) (map[string]libxc.CEX, map[string]*dex.ConnectionMaster) {
	cexes := make(map[string]libxc.CEX)
	cexCMs := make(map[string]*dex.ConnectionMaster)

	for _, cfg := range cfgs {
		if _, found := cexes[cfg.Name]; !found {
			logger := m.log.SubLogger(fmt.Sprintf("CEX-%s", cfg.Name))
			cex, err := libxc.NewCEX(cfg.Name, cfg.APIKey, cfg.APISecret, logger, dex.Simnet)
			if err != nil {
				m.log.Errorf("Failed to create %s: %v", cfg.Name, err)
				continue
			}

			cm := dex.NewConnectionMaster(cex)
			err = cm.Connect(m.ctx)
			if err != nil {
				m.log.Errorf("Failed to connect to %s: %v", cfg.Name, err)
				continue
			}

			cexes[cfg.Name] = cex
			cexCMs[cfg.Name] = cm
		}
	}

	return cexes, cexCMs
}

func (m *MarketMaker) logInitialBotBalances(dexBalances, cexBalances map[string]map[uint32]uint64) {
	var msg strings.Builder
	msg.WriteString("Initial market making balances:\n")
	for mkt, botDexBals := range dexBalances {
		msg.WriteString(fmt.Sprintf("-- %s:\n", mkt))

		i := 0
		msg.WriteString("  DEX: ")
		for assetID, amount := range botDexBals {
			msg.WriteString(fmt.Sprintf("%s: %d", dex.BipIDSymbol(assetID), amount))
			if i <= len(botDexBals)-1 {
				msg.WriteString(", ")
			}
			i++
		}

		i = 0
		if botCexBals, found := cexBalances[mkt]; found {
			msg.WriteString("\n  CEX: ")
			for assetID, amount := range botCexBals {
				msg.WriteString(fmt.Sprintf("%s: %d", dex.BipIDSymbol(assetID), amount))
				if i <= len(botCexBals)-1 {
					msg.WriteString(", ")
				}
				i++
			}
		}
	}

	m.log.Info(msg.String())
}

// Run starts the MarketMaker. There can only be one BotConfig per dex market.
func (m *MarketMaker) Run(ctx context.Context, pw []byte, alternateConfigPath *string) error {
	if !m.running.CompareAndSwap(false, true) {
		return errors.New("market making is already running")
	}

	var startedMarketMaking bool
	defer func() {
		if !startedMarketMaking {
			m.running.Store(false)
		}
	}()

	path := m.cfgPath
	if alternateConfigPath != nil {
		path = *alternateConfigPath
	}
	cfg, err := getMarketMakingConfig(path)
	if err != nil {
		return fmt.Errorf("error getting market making config: %v", err)
	}

	m.ctx, m.die = context.WithCancel(ctx)

	enabledBots, err := validateAndFilterEnabledConfigs(cfg.BotConfigs)
	if err != nil {
		return err
	}

	if err := m.loginAndUnlockWallets(pw, enabledBots); err != nil {
		return err
	}

	oracle, err := priceOracleFromConfigs(m.ctx, enabledBots, m.log.SubLogger("PriceOracle"))
	if err != nil {
		return err
	}
	m.syncedOracleMtx.Lock()
	m.syncedOracle = oracle
	m.syncedOracleMtx.Unlock()
	defer func() {
		m.syncedOracleMtx.Lock()
		m.syncedOracle = nil
		m.syncedOracleMtx.Unlock()
	}()

	cexes, cexCMs := m.initCEXConnections(cfg.CexConfigs)

	dexBaseBalances, cexBaseBalances, err := botInitialBaseBalances(enabledBots, m.core, cexes)
	if err != nil {
		return err
	}
	m.logInitialBotBalances(dexBaseBalances, cexBaseBalances)

	startedMarketMaking = true
	m.core.Broadcast(newMMStartStopNote(true))

	wg := new(sync.WaitGroup)

	var cexCfgMap map[string]*CEXConfig
	if len(cfg.CexConfigs) > 0 {
		cexCfgMap = make(map[string]*CEXConfig, len(cfg.CexConfigs))
		for _, cexCfg := range cfg.CexConfigs {
			cexCfgMap[cexCfg.Name] = cexCfg
		}
	}

	for _, cfg := range enabledBots {
		switch {
		case cfg.BasicMMConfig != nil:
			wg.Add(1)
			go func(cfg *BotConfig) {
				defer wg.Done()

				mkt := MarketWithHost{cfg.Host, cfg.BaseAsset, cfg.QuoteAsset}
				m.markBotAsRunning(mkt, true)
				defer func() {
					m.markBotAsRunning(mkt, false)
				}()

				m.core.Broadcast(newBotStartStopNote(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset, true))
				defer func() {
					m.core.Broadcast(newBotStartStopNote(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset, false))
				}()

				mktID := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
				logger := m.log.SubLogger(fmt.Sprintf("MarketMaker-%s", mktID))
				exchangeAdaptor := unifiedExchangeAdaptorForBot(&exchangeAdaptorCfg{
					botID:              mktID,
					market:             &mkt,
					baseDexBalances:    dexBaseBalances[mktID],
					baseCexBalances:    cexBaseBalances[mktID],
					core:               m.core,
					cex:                nil,
					maxBuyPlacements:   uint32(len(cfg.BasicMMConfig.BuyPlacements)),
					maxSellPlacements:  uint32(len(cfg.BasicMMConfig.SellPlacements)),
					baseWalletOptions:  cfg.BaseWalletOptions,
					quoteWalletOptions: cfg.QuoteWalletOptions,
					log:                logger,
				})
				exchangeAdaptor.run(ctx)

				RunBasicMarketMaker(m.ctx, cfg, exchangeAdaptor, oracle, logger)
			}(cfg)
		case cfg.SimpleArbConfig != nil:
			wg.Add(1)
			go func(cfg *BotConfig) {
				defer wg.Done()

				mktID := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
				logger := m.log.SubLogger(fmt.Sprintf("SimpleArbitrage-%s", mktID))

				cex, found := cexes[cfg.CEXCfg.Name]
				if !found {
					logger.Errorf("Cannot start %s bot due to CEX not starting", mktID)
					return
				}

				mkt := MarketWithHost{cfg.Host, cfg.BaseAsset, cfg.QuoteAsset}
				m.markBotAsRunning(mkt, true)
				defer func() {
					m.markBotAsRunning(mkt, false)
				}()

				exchangeAdaptor := unifiedExchangeAdaptorForBot(&exchangeAdaptorCfg{
					botID:              mktID,
					market:             &mkt,
					baseDexBalances:    dexBaseBalances[mktID],
					baseCexBalances:    cexBaseBalances[mktID],
					core:               m.core,
					cex:                cex,
					maxBuyPlacements:   1,
					maxSellPlacements:  1,
					baseWalletOptions:  cfg.BaseWalletOptions,
					quoteWalletOptions: cfg.QuoteWalletOptions,
					rebalanceCfg:       cfg.CEXCfg.AutoRebalance,
					log:                logger,
				})
				exchangeAdaptor.run(ctx)

				RunSimpleArbBot(m.ctx, cfg, exchangeAdaptor, exchangeAdaptor, logger)
			}(cfg)
		case cfg.ArbMarketMakerConfig != nil:
			wg.Add(1)
			go func(cfg *BotConfig) {
				defer wg.Done()

				mktID := dexMarketID(cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
				logger := m.log.SubLogger(fmt.Sprintf("ArbMarketMaker-%s", mktID))

				cex, found := cexes[cfg.CEXCfg.Name]
				if !found {
					logger.Errorf("Cannot start %s bot due to CEX not starting", mktID)
					return
				}

				mkt := MarketWithHost{cfg.Host, cfg.BaseAsset, cfg.QuoteAsset}
				m.markBotAsRunning(mkt, true)
				defer func() {
					m.markBotAsRunning(mkt, false)
				}()

				exchangeAdaptor := unifiedExchangeAdaptorForBot(&exchangeAdaptorCfg{
					botID:              mktID,
					market:             &mkt,
					baseDexBalances:    dexBaseBalances[mktID],
					baseCexBalances:    cexBaseBalances[mktID],
					core:               m.core,
					cex:                cex,
					maxBuyPlacements:   uint32(len(cfg.ArbMarketMakerConfig.BuyPlacements)),
					maxSellPlacements:  uint32(len(cfg.ArbMarketMakerConfig.SellPlacements)),
					baseWalletOptions:  cfg.BaseWalletOptions,
					quoteWalletOptions: cfg.QuoteWalletOptions,
					rebalanceCfg:       cfg.CEXCfg.AutoRebalance,
					log:                logger,
				})
				exchangeAdaptor.run(ctx)

				RunArbMarketMaker(m.ctx, cfg, exchangeAdaptor, exchangeAdaptor, logger)
			}(cfg)
		default:
			m.log.Errorf("No bot config provided. Skipping %s-%d-%d", cfg.Host, cfg.BaseAsset, cfg.QuoteAsset)
		}
	}

	go func() {
		wg.Wait()
		for cexName, cm := range cexCMs {
			m.log.Infof("Shutting down connection to %s", cexName)
			cm.Wait()
			m.log.Infof("Connection to %s shut down", cexName)
		}
		m.running.Store(false)
		m.core.Broadcast(newMMStartStopNote(false))
	}()

	return nil
}

func getMarketMakingConfig(path string) (*MarketMakingConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("no config file provided")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &MarketMakingConfig{}
	err = json.Unmarshal(data, cfg)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// GetMarketMakingConfig returns the market making config.
func (m *MarketMaker) GetMarketMakingConfig() (*MarketMakingConfig, error) {
	return getMarketMakingConfig(m.cfgPath)
}

// UpdateMarketMakingConfig updates the configuration for one of the bots.
func (m *MarketMaker) UpdateBotConfig(updatedCfg *BotConfig) (*MarketMakingConfig, error) {
	cfg, err := m.GetMarketMakingConfig()
	if err != nil {
		return nil, fmt.Errorf("error getting market making config: %v", err)
	}

	var updated bool
	for i, c := range cfg.BotConfigs {
		if c.Host == updatedCfg.Host && c.QuoteAsset == updatedCfg.QuoteAsset && c.BaseAsset == updatedCfg.BaseAsset {
			cfg.BotConfigs[i] = updatedCfg
			updated = true
			break
		}
	}
	if !updated {
		cfg.BotConfigs = append(cfg.BotConfigs, updatedCfg)
	}

	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("error marshalling market making config: %v", err)
	}

	err = os.WriteFile(m.cfgPath, data, 0644)
	if err != nil {
		return nil, fmt.Errorf("error writing market making config: %v", err)
	}
	return cfg, nil
}

// RemoveConfig removes a bot config from the market making config.
func (m *MarketMaker) RemoveBotConfig(host string, baseID, quoteID uint32) (*MarketMakingConfig, error) {
	cfg, err := m.GetMarketMakingConfig()
	if err != nil {
		return nil, fmt.Errorf("error getting market making config: %v", err)
	}

	var updated bool
	for i, c := range cfg.BotConfigs {
		if c.Host == host && c.QuoteAsset == quoteID && c.BaseAsset == baseID {
			cfg.BotConfigs = append(cfg.BotConfigs[:i], cfg.BotConfigs[i+1:]...)
			updated = true
			break
		}
	}
	if !updated {
		return nil, fmt.Errorf("config not found")
	}

	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("error marshalling market making config: %v", err)
	}

	err = os.WriteFile(m.cfgPath, data, 0644)
	if err != nil {
		return nil, fmt.Errorf("error writing market making config: %v", err)
	}
	return cfg, nil
}

// Stop stops the MarketMaker.
func (m *MarketMaker) Stop() {
	if m.die != nil {
		m.die()
	}
}
