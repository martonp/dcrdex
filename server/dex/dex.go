// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/fiatrates"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/apidata"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/auth"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg"
	"decred.org/dcrdex/server/market"
	"decred.org/dcrdex/server/noderelay"
	"decred.org/dcrdex/server/swap"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	// PreAPIVersion covers all API iterations before versioning started.
	PreAPIVersion  = iota
	BondAPIVersion // when we drop the legacy reg fee proto
	V1APIVersion

	// APIVersion is the current API version.
	APIVersion = V1APIVersion
)

// Asset represents an asset in the Config file.
type Asset struct {
	Symbol      string `json:"bip44symbol"`
	Network     string `json:"network"`
	LotSizeOLD  uint64 `json:"lotSize,omitempty"`
	RateStepOLD uint64 `json:"rateStep,omitempty"`
	MaxFeeRate  uint64 `json:"maxFeeRate"`
	SwapConf    uint32 `json:"swapConf"`
	ConfigPath  string `json:"configPath"`
	RegFee      uint64 `json:"regFee,omitempty"`
	RegConfs    uint32 `json:"regConfs,omitempty"`
	RegXPub     string `json:"regXPub,omitempty"`
	BondAmt     uint64 `json:"bondAmt,omitempty"`
	BondConfs   uint32 `json:"bondConfs,omitempty"`
	Disabled    bool   `json:"disabled"`
	NodeRelayID string `json:"nodeRelayID,omitempty"`
}

// Market represents the markets specified in the Config file.
type Market struct {
	Base       string  `json:"base"`
	Quote      string  `json:"quote"`
	LotSize    uint64  `json:"lotSize"`
	ParcelSize uint32  `json:"parcelSize"`
	RateStep   uint64  `json:"rateStep"`
	Duration   uint64  `json:"epochDuration"`
	MBBuffer   float64 `json:"marketBuyBuffer"`
	Disabled   bool    `json:"disabled"`
}

// Config is a market and asset configuration file.
type Config struct {
	Markets []*Market         `json:"markets"`
	Assets  map[string]*Asset `json:"assets"`
}

// LoadConfig loads the Config from the specified file.
func LoadConfig(net dex.Network, filePath string) ([]*dex.MarketInfo, []*Asset, error) {
	src, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()
	return loadMarketConf(net, src)
}

func loadMarketConf(net dex.Network, src io.Reader) ([]*dex.MarketInfo, []*Asset, error) {
	settings, err := io.ReadAll(src)
	if err != nil {
		return nil, nil, err
	}

	var conf Config
	err = json.Unmarshal(settings, &conf)
	if err != nil {
		return nil, nil, err
	}

	log.Debug("|-------------------- BEGIN parsed markets.json --------------------")
	log.Debug("MARKETS")
	log.Debug("                  Base         Quote    LotSize     EpochDur")
	for i, mktConf := range conf.Markets {
		if mktConf.LotSize == 0 {
			return nil, nil, fmt.Errorf("market (%s, %s) has NO lot size specified (was an asset setting)",
				mktConf.Base, mktConf.Quote)
		}
		if mktConf.RateStep == 0 {
			return nil, nil, fmt.Errorf("market (%s, %s) has NO rate step specified (was an asset setting)",
				mktConf.Base, mktConf.Quote)
		}
		log.Debugf("Market %d: % 12s  % 12s   %6de8  % 8d ms",
			i, mktConf.Base, mktConf.Quote, mktConf.LotSize/1e8, mktConf.Duration)
	}
	log.Debug("")

	log.Debug("ASSETS")
	log.Debug("             MaxFeeRate   SwapConf   Network")
	for asset, assetConf := range conf.Assets {
		if assetConf.LotSizeOLD > 0 {
			return nil, nil, fmt.Errorf("asset %s has a lot size (%d) specified, "+
				"but this is now a market setting", asset, assetConf.LotSizeOLD)
		}
		if assetConf.RateStepOLD > 0 {
			return nil, nil, fmt.Errorf("asset %s has a rate step (%d) specified, "+
				"but this is now a market setting", asset, assetConf.RateStepOLD)
		}
		log.Debugf("%-12s % 10d  % 9d % 9s", asset, assetConf.MaxFeeRate, assetConf.SwapConf, assetConf.Network)
	}
	log.Debug("|--------------------- END parsed markets.json ---------------------|")

	// Normalize the asset names to lower case.
	var assets []*Asset
	assetMap := make(map[uint32]struct{})
	unused := make(map[uint32]string)
	for assetName, assetConf := range conf.Assets {
		if assetConf.Disabled {
			continue
		}
		network, err := dex.NetFromString(assetConf.Network)
		if err != nil {
			return nil, nil, fmt.Errorf("unrecognized network %s for asset %s",
				assetConf.Network, assetName)
		}
		if net != network {
			continue
		}

		symbol := strings.ToLower(assetConf.Symbol)
		assetID, found := dex.BipSymbolID(symbol)
		if !found {
			return nil, nil, fmt.Errorf("asset %q symbol %q unrecognized", assetName, assetConf.Symbol)
		}

		if assetConf.MaxFeeRate == 0 {
			return nil, nil, fmt.Errorf("max fee rate of 0 is invalid for asset %q", assetConf.Symbol)
		}

		unused[assetID] = assetConf.Symbol
		assetMap[assetID] = struct{}{}
		assets = append(assets, assetConf)
	}

	sort.Slice(assets, func(i, j int) bool {
		return assets[i].Symbol < assets[j].Symbol
	})

	var markets []*dex.MarketInfo
	for _, mktConf := range conf.Markets {
		if mktConf.Disabled {
			continue
		}
		baseConf, ok := conf.Assets[mktConf.Base]
		if !ok {
			return nil, nil, fmt.Errorf("missing configuration for asset %s", mktConf.Base)
		}
		if baseConf.Disabled {
			return nil, nil, fmt.Errorf("required base asset %s is disabled", mktConf.Base)
		}
		quoteConf, ok := conf.Assets[mktConf.Quote]
		if !ok {
			return nil, nil, fmt.Errorf("missing configuration for asset %s", mktConf.Quote)
		}
		if quoteConf.Disabled {
			return nil, nil, fmt.Errorf("required quote asset %s is disabled", mktConf.Base)
		}

		baseID, _ := dex.BipSymbolID(baseConf.Symbol)
		quoteID, _ := dex.BipSymbolID(quoteConf.Symbol)

		delete(unused, baseID)
		delete(unused, quoteID)

		if is, parentID := asset.IsToken(baseID); is {
			if _, found := assetMap[parentID]; !found {
				return nil, nil, fmt.Errorf("parent asset %s not enabled for token %s", dex.BipIDSymbol(parentID), baseConf.Symbol)
			}
			delete(unused, parentID)
		}

		if is, parentID := asset.IsToken(quoteID); is {
			if _, found := assetMap[parentID]; !found {
				return nil, nil, fmt.Errorf("parent asset %s not enabled for token %s", dex.BipIDSymbol(parentID), quoteConf.Symbol)
			}
			delete(unused, parentID)
		}

		baseNet, err := dex.NetFromString(baseConf.Network)
		if err != nil {
			return nil, nil, fmt.Errorf("unrecognized network %s", baseConf.Network)
		}
		quoteNet, err := dex.NetFromString(quoteConf.Network)
		if err != nil {
			return nil, nil, fmt.Errorf("unrecognized network %s", quoteConf.Network)
		}

		if baseNet != quoteNet {
			return nil, nil, fmt.Errorf("assets are for different networks (%s and %s)",
				baseConf.Network, quoteConf.Network)
		}

		if baseNet != net {
			continue
		}

		if mktConf.ParcelSize == 0 {
			return nil, nil, fmt.Errorf("parcel size cannot be zero")
		}

		mkt, err := dex.NewMarketInfoFromSymbols(baseConf.Symbol, quoteConf.Symbol,
			mktConf.LotSize, mktConf.RateStep, mktConf.Duration, mktConf.ParcelSize, mktConf.MBBuffer)
		if err != nil {
			return nil, nil, err
		}
		markets = append(markets, mkt)
	}

	if len(unused) > 0 {
		symbols := make([]string, 0, len(unused))
		for _, symbol := range unused {
			symbols = append(symbols, symbol)
		}
		return nil, nil, fmt.Errorf("unused assets %+v", symbols)
	}

	return markets, assets, nil
}

// DBConf groups the database configuration parameters.
type DBConf struct {
	DBName       string
	User         string
	Pass         string
	Host         string
	Port         uint16
	ShowPGConfig bool
}

// ValidateConfigFile validates the market+assets configuration file.
// ValidateConfigFile prints information to stdout. An error is returned for any
// configuration errors.
func ValidateConfigFile(cfgPath string, net dex.Network, log dex.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registeredAssets := asset.Assets()
	coinpapAssets := make([]*fiatrates.CoinpaprikaAsset, 0, len(registeredAssets))
	for _, a := range registeredAssets {
		coinpapAssets = append(coinpapAssets, &fiatrates.CoinpaprikaAsset{
			AssetID: a.AssetID,
			Name:    a.Name,
			Symbol:  a.Symbol,
		})
	}
	fiatRates := fiatrates.FetchCoinpaprikaRates(ctx, coinpapAssets, dex.StdOutLogger("CPAP", dex.LevelInfo))

	log.Debugf("Loaded %d fiat rates from coinpaprika", len(fiatRates))

	markets, assets, err := LoadConfig(net, cfgPath)
	if err != nil {
		return fmt.Errorf("error loading config file at %q: %w", cfgPath, err)
	}

	log.Debugf("Loaded %d markets and %d assets from configuration", len(markets), len(assets))

	var failures []string

	type parsedAsset struct {
		*Asset
		unit        string
		minLotSize  uint64
		minBondSize uint64
		fiatRate    float64
		cFactor     float64
		ui          *dex.UnitInfo
	}
	parsedAssets := make(map[uint32]*parsedAsset, len(assets))

	printSuccess := func(s string, a ...interface{}) {
		fmt.Printf(s+"\n", a...)
	}

	for _, a := range assets {
		if dex.TokenSymbol(a.Symbol) == "dextt" {
			continue
		}

		assetID, ok := dex.BipSymbolID(a.Symbol)
		if !ok {
			return fmt.Errorf("no asset ID found for symbol %q", a.Symbol)
		}

		minLotSize, minBondSize, found := asset.Minimums(assetID, a.MaxFeeRate)
		if !found {
			return fmt.Errorf("no asset registered for %s (%d)", a.Symbol, assetID)
		}

		ui, err := asset.UnitInfo(assetID)
		if err != nil {
			return fmt.Errorf("error getting unit info for %s: %w", a.Symbol, err)
		}

		fiatRate, found := fiatRates[assetID]
		if !found {
			return fmt.Errorf("no fiat exchange rate found for asset %s (%d)", dex.BipIDSymbol(assetID), assetID)
		}

		unit := ui.Conventional.Unit
		minLotSizeUSD := float64(minLotSize) / float64(ui.Conventional.ConversionFactor) * fiatRate
		minBondSizeUSD := float64(minLotSize) / float64(ui.Conventional.ConversionFactor) * fiatRate
		parsedAssets[assetID] = &parsedAsset{
			Asset:       a,
			unit:        unit,
			minLotSize:  minLotSize,
			minBondSize: minBondSize,
			fiatRate:    fiatRate,
			cFactor:     float64(ui.Conventional.ConversionFactor),
			ui:          &ui,
		}
		printSuccess(
			"Calculated for %s: min lot size = %s %s (%d %s) (~ %.4f USD), min bond size = %s %s (%d %s) (%.4f USD)",
			a.Symbol, ui.ConventionalString(minLotSize), unit, minLotSize, ui.AtomicUnit, minLotSizeUSD,
			ui.ConventionalString(minBondSize), unit, minBondSize, ui.AtomicUnit, minBondSizeUSD,
		)
	}

	for _, a := range parsedAssets {
		if a.BondAmt == 0 {
			continue
		}
		if a.BondAmt < a.minBondSize {
			failures = append(failures, fmt.Sprintf("Bond amount for %s is too small. %d < %d", a.Symbol, a.BondAmt, a.minBondSize))
		} else {
			printSuccess("Bond size for %s passes: %d >= %d", a.Symbol, a.BondAmt, a.minBondSize)
		}
	}

	for _, m := range markets {
		// Check lot size.
		b, q := parsedAssets[m.Base], parsedAssets[m.Quote]
		if b == nil || q == nil {
			continue // should be dextt pair
		}
		const quoteConversionBuffer = 1.5 // Buffer for accomodating rate changes.
		minFromQuote := uint64(math.Round(float64(q.minLotSize) / q.cFactor * q.fiatRate / b.fiatRate * b.cFactor * quoteConversionBuffer))
		// Slightly different messaging if we're limited by the conversion from
		// the quote asset minimums.
		if minFromQuote > b.minLotSize {
			if m.LotSize < minFromQuote {
				failures = append(failures, fmt.Sprintf("Lot size for %s (converted from quote asset %s) is too low. %d < %d", m.Name, q.unit, m.LotSize, minFromQuote))
			} else {
				printSuccess("Market %s lot size (converted from quote asset %s) passes: %d >= %d", m.Name, q.unit, m.LotSize, minFromQuote)
			}
		} else {
			if m.LotSize < b.minLotSize {
				failures = append(failures, fmt.Sprintf("Lot size for %s is too low. %d < %d", m.Name, m.LotSize, b.minLotSize))
			} else {
				printSuccess("Market %s lot size passes: %d >= %d", m.Name, m.LotSize, b.minLotSize)
			}
		}
	}

	for _, s := range failures {
		fmt.Println("FAIL:", s)
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d market or asset configuration problems need fixing", len(failures))
	}

	return nil
}

// RPCConfig is an alias for the comms Server's RPC config struct.
type RPCConfig = comms.RPCConfig

// DexConf is the configuration data required to create a new DEX.
type DexConf struct {
	DataDir          string
	LogBackend       *dex.LoggerMaker
	Markets          []*dex.MarketInfo
	Assets           []*Asset
	Network          dex.Network
	DBConf           *DBConf
	BroadcastTimeout time.Duration
	TxWaitExpiration time.Duration
	CancelThreshold  float64
	FreeCancels      bool
	PenaltyThreshold uint32
	DEXPrivKey       *secp256k1.PrivateKey
	CommsCfg         *RPCConfig
	NoResumeSwaps    bool
	NodeRelayAddr    string
}

type signer struct {
	*secp256k1.PrivateKey
}

func (s signer) Sign(hash []byte) *ecdsa.Signature {
	return ecdsa.Sign(s.PrivateKey, hash)
}

type subsystem struct {
	name string
	// either a ssw or cm
	ssw *dex.StartStopWaiter
	cm  *dex.ConnectionMaster
}

func (ss *subsystem) stop() {
	if ss.ssw != nil {
		ss.ssw.Stop()
		ss.ssw.WaitForShutdown()
	} else {
		ss.cm.Disconnect()
		ss.cm.Wait()
	}
}

// DEX is the DEX manager, which creates and controls the lifetime of all
// components of the DEX.
type DEX struct {
	network     dex.Network
	markets     map[string]*market.Market
	assets      map[uint32]*swap.SwapperAsset
	storage     db.DEXArchivist
	authMgr     *auth.AuthManager
	swapper     *swap.Swapper
	orderRouter *market.OrderRouter
	bookRouter  *market.BookRouter
	subsystems  []subsystem
	server      *comms.Server

	configRespMtx sync.RWMutex
	configResp    *configResponse
}

// configResponse is defined here to leave open the possibility for hot
// adjustable parameters while storing a pre-encoded config response message. An
// update method will need to be defined in the future for this purpose.
type configResponse struct {
	configMsg *msgjson.ConfigResult // constant for now
	configEnc json.RawMessage
}

func newConfigResponse(cfg *DexConf, bondAssets map[string]*msgjson.BondAsset,
	cfgAssets []*msgjson.Asset, cfgMarkets []*msgjson.Market) (*configResponse, error) {

	configMsg := &msgjson.ConfigResult{
		APIVersion:       uint16(APIVersion),
		DEXPubKey:        cfg.DEXPrivKey.PubKey().SerializeCompressed(),
		BroadcastTimeout: uint64(cfg.BroadcastTimeout.Milliseconds()),
		CancelMax:        cfg.CancelThreshold,
		Assets:           cfgAssets,
		Markets:          cfgMarkets,
		BondAssets:       bondAssets,
		BondExpiry:       uint64(dex.BondExpiry(cfg.Network)), // temporary while we figure it out
		BinSizes:         candles.BinSizes,
		PenaltyThreshold: cfg.PenaltyThreshold,
		MaxScore:         auth.ScoringMatchLimit,
	}

	// NOTE/TODO: To include active epoch in the market status objects, we need
	// a channel from Market to push status changes back to DEX manager.
	// Presently just include start epoch that we set when launching the
	// Markets, and suspend info that DEX obtained when calling the Market's
	// Suspend method.

	encResult, err := json.Marshal(configMsg)
	if err != nil {
		return nil, err
	}

	return &configResponse{
		configMsg: configMsg,
		configEnc: encResult,
	}, nil
}

func (cr *configResponse) setMktSuspend(name string, finalEpoch uint64, persist bool) {
	for _, mkt := range cr.configMsg.Markets {
		if mkt.Name == name {
			mkt.MarketStatus.FinalEpoch = finalEpoch
			mkt.MarketStatus.Persist = &persist
			cr.remarshal()
			return
		}
	}
	log.Errorf("Failed to update MarketStatus for market %q", name)
}

func (cr *configResponse) setMktResume(name string, startEpoch uint64) (epochLen uint64) {
	for _, mkt := range cr.configMsg.Markets {
		if mkt.Name == name {
			mkt.MarketStatus.StartEpoch = startEpoch
			mkt.MarketStatus.FinalEpoch = 0
			cr.remarshal()
			return mkt.EpochLen
		}
	}
	log.Errorf("Failed to update MarketStatus for market %q", name)
	return 0
}

func (cr *configResponse) remarshal() {
	encResult, err := json.Marshal(cr.configMsg)
	if err != nil {
		log.Errorf("failed to marshal config message: %v", err)
		return
	}
	cr.configEnc = encResult
}

// Stop shuts down the DEX. Stop returns only after all components have
// completed their shutdown.
func (dm *DEX) Stop() {
	log.Infof("Stopping all DEX subsystems.")
	for _, ss := range dm.subsystems {
		log.Infof("Stopping %s...", ss.name)
		ss.stop()
		log.Infof("%s is now shut down.", ss.name)
	}
	log.Infof("Stopping storage...")
	if err := dm.storage.Close(); err != nil {
		log.Errorf("DEXArchivist.Close: %v", err)
	}
}

func marketSubSysName(name string) string {
	return fmt.Sprintf("Market[%s]", name)
}

func (dm *DEX) handleDEXConfig(any) (any, error) {
	dm.configRespMtx.RLock()
	defer dm.configRespMtx.RUnlock()
	return dm.configResp.configEnc, nil
}

func (dm *DEX) handleHealthFlag(any) (any, error) {
	return dm.Healthy(), nil
}

// FeeCoiner describes a type that can check a transaction output, namely a fee
// payment, for a particular asset.
type FeeCoiner interface {
	FeeCoin(coinID []byte) (addr string, val uint64, confs int64, err error)
}

// Bonder describes a type that supports parsing raw bond transactions and
// locating them on-chain via coin ID.
type Bonder interface {
	BondVer() uint16
	BondCoin(ctx context.Context, ver uint16, coinID []byte) (amt, lockTime, confs int64,
		acct account.AccountID, err error)
	ParseBondTx(ver uint16, rawTx []byte) (bondCoinID []byte, amt int64, bondAddr string,
		bondPubKeyHash []byte, lockTime int64, acct account.AccountID, err error)
}

// NewDEX creates the dex manager and starts all subsystems. Use Stop to
// shutdown cleanly. The Context is used to abort setup.
//  1. Validate each specified asset.
//  2. Create CoinLockers for each asset.
//  3. Create and start asset backends.
//  4. Create the archivist and connect to the storage backend.
//  5. Create the authentication manager.
//  6. Create and start the Swapper.
//  7. Create and start the markets.
//  8. Create and start the book router, and create the order router.
//  9. Create and start the comms server.
func NewDEX(ctx context.Context, cfg *DexConf) (*DEX, error) {
	var subsystems []subsystem
	startSubSys := func(name string, rc any) (err error) {
		subsys := subsystem{name: name}
		switch st := rc.(type) {
		case dex.Runner:
			subsys.ssw = dex.NewStartStopWaiter(st)
			subsys.ssw.Start(context.Background()) // stopped with Stop
		case dex.Connector:
			subsys.cm = dex.NewConnectionMaster(st)
			err = subsys.cm.Connect(context.Background()) // stopped with Disconnect
			if err != nil {
				return
			}
		default:
			panic(fmt.Sprintf("Invalid subsystem type %T", rc))
		}

		subsystems = append([]subsystem{subsys}, subsystems...) // top of stack
		return
	}

	// Do not wrap the caller's context for the DB since we must coordinate it's
	// shutdown in sequence with the other subsystems.
	ctxDB, cancelDB := context.WithCancel(context.Background())
	var ready bool
	defer func() {
		if ready {
			return
		}
		for _, ss := range subsystems {
			ss.stop()
		}
		// If the DB is running, kill it too.
		cancelDB()
	}()

	// Check each configured asset.
	assetIDs := make([]uint32, len(cfg.Assets))
	var nodeRelayIDs []string
	for i, assetConf := range cfg.Assets {
		symbol := strings.ToLower(assetConf.Symbol)

		// Ensure the symbol is a recognized BIP44 symbol, and retrieve its ID.
		assetID, found := dex.BipSymbolID(symbol)
		if !found {
			return nil, fmt.Errorf("asset symbol %q unrecognized", assetConf.Symbol)
		}

		// Double check the asset's network.
		net, err := dex.NetFromString(assetConf.Network)
		if err != nil {
			return nil, fmt.Errorf("unrecognized network %s for asset %s",
				assetConf.Network, symbol)
		}
		if cfg.Network != net {
			return nil, fmt.Errorf("asset %q is configured for network %q, expected %q",
				symbol, assetConf.Network, cfg.Network.String())
		}

		if assetConf.MaxFeeRate == 0 {
			return nil, fmt.Errorf("max fee rate of 0 is invalid for asset %q", symbol)
		}

		if assetConf.NodeRelayID != "" {
			nodeRelayIDs = append(nodeRelayIDs, assetConf.NodeRelayID)
		}

		assetIDs[i] = assetID
	}

	// Create DEXArchivist with the pg DB driver. The fee Addressers require the
	// archivist for key index storage and retrieval.
	pgCfg := &pg.Config{
		Host:         cfg.DBConf.Host,
		Port:         strconv.Itoa(int(cfg.DBConf.Port)),
		User:         cfg.DBConf.User,
		Pass:         cfg.DBConf.Pass,
		DBName:       cfg.DBConf.DBName,
		ShowPGConfig: cfg.DBConf.ShowPGConfig,
		QueryTimeout: 20 * time.Minute,
		MarketCfg:    cfg.Markets,
	}
	// After DEX construction, the storage subsystem should be stopped
	// gracefully with its Close method, and in coordination with other
	// subsystems via Stop. To abort its setup, rig a temporary link to the
	// caller's Context.
	running := make(chan struct{})
	defer close(running) // break the link
	go func() {
		select {
		case <-ctx.Done(): // cancelled construction
			cancelDB()
		case <-running: // DB shutdown now only via dex.Stop=>db.Close
		}
	}()
	storage, err := db.Open(ctxDB, "pg", pgCfg)
	if err != nil {
		return nil, fmt.Errorf("db.Open: %w", err)
	}

	relayAddrs := make(map[string]string, len(nodeRelayIDs))
	if len(nodeRelayIDs) > 0 {
		nexusPort := "17537"
		switch cfg.Network {
		case dex.Testnet:
			nexusPort = "17538"
		case dex.Simnet:
			nexusPort = "17539"
		}
		relayDir := filepath.Join(cfg.DataDir, "noderelay")
		relay, err := noderelay.NewNexus(&noderelay.NexusConfig{
			ExternalAddr: cfg.NodeRelayAddr,
			Dir:          relayDir,
			Port:         nexusPort,
			Logger:       cfg.LogBackend.NewLogger("NR", log.Level()),
			RelayIDs:     nodeRelayIDs,
		})
		if err != nil {
			return nil, fmt.Errorf("error creating node relay: %w", err)
		}
		if err := startSubSys("Node relay", relay); err != nil {
			return nil, fmt.Errorf("error starting node relay: %w", err)
		}
		select {
		case <-relay.WaitForSourceNodes():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		for _, relayID := range nodeRelayIDs {
			if relayAddrs[relayID], err = relay.RelayAddr(relayID); err != nil {
				return nil, fmt.Errorf("error getting relay address for ID %s: %w", relayID, err)
			}
		}
	}

	// Create a MasterCoinLocker for each asset.
	dexCoinLocker := coinlock.NewDEXCoinLocker(assetIDs)

	// Prepare bonders.
	bondAssets := make(map[string]*msgjson.BondAsset)
	bonders := make(map[uint32]Bonder)

	// Start asset backends.
	lockableAssets := make(map[uint32]*swap.SwapperAsset, len(cfg.Assets))
	backedAssets := make(map[uint32]*asset.BackedAsset, len(cfg.Assets))
	cfgAssets := make([]*msgjson.Asset, 0, len(cfg.Assets))
	assetLogger := cfg.LogBackend.Logger("ASSET")
	txDataSources := make(map[uint32]auth.TxDataSource)
	feeMgr := NewFeeManager()
	addAsset := func(assetID uint32, assetConf *Asset) error {
		symbol := strings.ToLower(assetConf.Symbol)

		assetVer, err := asset.Version(assetID)
		if err != nil {
			return fmt.Errorf("failed to retrieve asset %q version: %w", symbol, err)
		}

		// Create a new asset backend. An asset driver with a name matching the
		// asset symbol must be available.
		log.Infof("Starting asset backend %q...", symbol)
		logger := assetLogger.SubLogger(symbol)

		isToken, parentID := asset.IsToken(assetID)
		var be asset.Backend
		if isToken {
			parent, found := backedAssets[parentID]
			if !found {
				return fmt.Errorf("attempting to load token asset %d before parent %d", assetID, parentID)
			}
			backer, is := parent.Backend.(asset.TokenBacker)
			if !is {
				return fmt.Errorf("token %d parent %d is not a TokenBacker", assetID, parentID)
			}
			be, err = backer.TokenBackend(assetID, assetConf.ConfigPath)
			if err != nil {
				return fmt.Errorf("failed to setup token %q: %w", symbol, err)
			}
		} else {
			cfg := &asset.BackendConfig{
				AssetID:    assetID,
				ConfigPath: assetConf.ConfigPath,
				Logger:     logger,
				Net:        cfg.Network,
				RelayAddr:  relayAddrs[assetConf.NodeRelayID],
			}
			be, err = asset.Setup(cfg)
			if err != nil {
				return fmt.Errorf("failed to setup asset %q: %w", symbol, err)
			}
		}

		err = startSubSys(fmt.Sprintf("Asset[%s]", symbol), be)
		if err != nil {
			return fmt.Errorf("failed to start asset %q: %w", symbol, err)
		}

		if assetConf.BondAmt > 0 && assetConf.BondConfs > 0 {
			// Make sure we can check on fee transactions.
			bc, ok := be.(Bonder)
			if !ok {
				return fmt.Errorf("asset %v is not a Bonder", symbol)
			}
			bondAssets[symbol] = &msgjson.BondAsset{
				Version: bc.BondVer(),
				ID:      assetID,
				Amt:     assetConf.BondAmt,
				Confs:   assetConf.BondConfs,
			}
			bonders[assetID] = bc
			log.Infof("Bonds accepted using %s: amount %d, confs %d",
				symbol, assetConf.BondAmt, assetConf.BondConfs)
		}

		unitInfo, err := asset.UnitInfo(assetID)
		if err != nil {
			return err
		}

		var coinLocker coinlock.CoinLocker
		if _, isAccountRedeemer := be.(asset.AccountBalancer); isAccountRedeemer {
			coinLocker = dexCoinLocker.AssetLocker(assetID).Swap()
		}

		ba := &asset.BackedAsset{
			Asset: dex.Asset{
				ID:         assetID,
				Symbol:     symbol,
				Version:    assetVer,
				MaxFeeRate: assetConf.MaxFeeRate,
				SwapConf:   assetConf.SwapConf,
				UnitInfo:   unitInfo,
			},
			Backend: be,
		}

		backedAssets[assetID] = ba
		lockableAssets[assetID] = &swap.SwapperAsset{
			BackedAsset: ba,
			Locker:      coinLocker,
		}
		feeMgr.AddFetcher(ba)

		// Prepare assets portion of config response.
		cfgAssets = append(cfgAssets, &msgjson.Asset{
			Symbol:     assetConf.Symbol,
			ID:         assetID,
			Version:    assetVer,
			MaxFeeRate: assetConf.MaxFeeRate,
			SwapConf:   uint16(assetConf.SwapConf),
			UnitInfo:   unitInfo,
		})

		txDataSources[assetID] = be.TxData
		return nil
	}

	// Add base chain assets before tokens.
	tokens := make(map[uint32]*Asset)

	for i, assetConf := range cfg.Assets {
		assetID := assetIDs[i]
		if isToken, _ := asset.IsToken(assetID); isToken {
			tokens[assetID] = assetConf
			continue
		}
		if err := addAsset(assetID, assetConf); err != nil {
			return nil, err
		}
	}

	for assetID, assetConf := range tokens {
		if err := addAsset(assetID, assetConf); err != nil {
			return nil, err
		}
	}

	for _, mkt := range cfg.Markets {
		mkt.Name = strings.ToLower(mkt.Name)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Create the user order unbook dispatcher for the AuthManager.
	markets := make(map[string]*market.Market, len(cfg.Markets))
	userUnbookFun := func(user account.AccountID) {
		for _, mkt := range markets {
			mkt.UnbookUserOrders(user)
		}
	}

	bondChecker := func(ctx context.Context, assetID uint32, version uint16, coinID []byte) (amt, lockTime, confs int64,
		acct account.AccountID, err error) {
		bc := bonders[assetID]
		if bc == nil {
			err = fmt.Errorf("unsupported bond asset")
			return
		}
		return bc.BondCoin(ctx, version, coinID)
	}

	bondTxParser := func(assetID uint32, version uint16, rawTx []byte) (bondCoinID []byte,
		amt, lockTime int64, acct account.AccountID, err error) {
		bc := bonders[assetID]
		if bc == nil {
			err = fmt.Errorf("unsupported bond asset")
			return
		}
		bondCoinID, amt, _, _, lockTime, acct, err = bc.ParseBondTx(version, rawTx)
		return
	}

	if cfg.PenaltyThreshold == 0 {
		cfg.PenaltyThreshold = auth.DefaultPenaltyThreshold
	}

	// Client comms RPC server.
	server, err := comms.NewServer(cfg.CommsCfg)
	if err != nil {
		return nil, fmt.Errorf("NewServer failed: %w", err)
	}

	dataAPI := apidata.NewDataAPI(storage, server.RegisterHTTP)

	authCfg := auth.Config{
		Storage:          storage,
		Signer:           signer{cfg.DEXPrivKey},
		BondAssets:       bondAssets,
		BondTxParser:     bondTxParser,
		BondChecker:      bondChecker,
		BondExpiry:       uint64(dex.BondExpiry(cfg.Network)),
		UserUnbooker:     userUnbookFun,
		MiaUserTimeout:   cfg.BroadcastTimeout,
		CancelThreshold:  cfg.CancelThreshold,
		FreeCancels:      cfg.FreeCancels,
		PenaltyThreshold: cfg.PenaltyThreshold,
		TxDataSources:    txDataSources,
		Route:            server.Route,
	}

	authMgr := auth.NewAuthManager(&authCfg)
	log.Infof("Cancellation rate threshold %f, new user grace period %d cancels",
		cfg.CancelThreshold, authMgr.GraceLimit())
	log.Infof("MIA user order unbook timeout %v", cfg.BroadcastTimeout)
	if authCfg.FreeCancels {
		log.Infof("Cancellations are NOT COUNTED (the cancellation rate threshold is ignored).")
	}
	log.Infof("Penalty threshold is %v", cfg.PenaltyThreshold)

	// Create a swapDone dispatcher for the Swapper.
	swapDone := func(ord order.Order, match *order.Match, fail bool) {
		name, err := dex.MarketName(ord.Base(), ord.Quote())
		if err != nil {
			log.Errorf("bad market for order %v: %v", ord.ID(), err)
			return
		}
		markets[name].SwapDone(ord, match, fail)
	}

	// Create the swapper.
	swapperCfg := &swap.Config{
		Assets:           lockableAssets,
		Storage:          storage,
		AuthManager:      authMgr,
		BroadcastTimeout: cfg.BroadcastTimeout,
		TxWaitExpiration: cfg.TxWaitExpiration,
		LockTimeTaker:    dex.LockTimeTaker(cfg.Network),
		LockTimeMaker:    dex.LockTimeMaker(cfg.Network),
		SwapDone:         swapDone,
		NoResume:         cfg.NoResumeSwaps,
		// TODO: set the AllowPartialRestore bool to allow startup with a
		// missing asset backend if necessary in an emergency.
	}

	swapper, err := swap.NewSwapper(swapperCfg)
	if err != nil {
		return nil, fmt.Errorf("NewSwapper: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Because the dexBalancer relies on the marketTunnels map, and NewMarket
	// checks necessary balances for account-based assets using the dexBalancer,
	// that means that each market can only query orders for the markets that
	// were initialized before it was, which is fine, but notable. The
	// resulting behavior is that a user could have orders involving an
	// account-based asset approved for re-booking on one market, but have
	// orders rejected on a market involving the same asset created afterwards,
	// since the later balance query is accounting for the earlier market.
	//
	// The current behavior is to reject all orders for the market if the
	// account balance is too low to support them all, though an algorithm could
	// be developed to do reject only some orders, based on available funding.
	//
	// This pattern is only safe because the markets are not Run until after
	// they are all instantiated, so we are synchronous in our use of the
	// marketTunnels map.
	marketTunnels := make(map[string]market.MarketTunnel, len(cfg.Markets))
	pendingAccounters := make(map[string]market.PendingAccounter, len(cfg.Markets))

	dexBalancer, err := market.NewDEXBalancer(pendingAccounters, backedAssets, swapper)
	if err != nil {
		return nil, fmt.Errorf("NewDEXBalancer error: %w", err)
	}

	// Markets
	var orderRouter *market.OrderRouter
	usersWithOrders := make(map[account.AccountID]struct{})
	for _, mktInf := range cfg.Markets {
		// nilness of the coin locker signals account-based asset.
		var baseCoinLocker, quoteCoinLocker coinlock.CoinLocker
		b, q := backedAssets[mktInf.Base], backedAssets[mktInf.Quote]
		if _, ok := b.Backend.(asset.OutputTracker); ok {
			baseCoinLocker = dexCoinLocker.AssetLocker(mktInf.Base).Book()
		}
		if _, ok := q.Backend.(asset.OutputTracker); ok {
			quoteCoinLocker = dexCoinLocker.AssetLocker(mktInf.Quote).Book()
		}

		// Calculate a minimum market rate that avoids dust.
		// quote_dust = base_lot * min_rate / rate_encoding_factor
		// => min_rate = quote_dust * rate_encoding_factor * base_lot
		quoteMinLotSize, _, _ := asset.Minimums(mktInf.Quote, q.Asset.MaxFeeRate)
		minRate := calc.MinimumMarketRate(mktInf.LotSize, quoteMinLotSize)

		mkt, err := market.NewMarket(&market.Config{
			MarketInfo:      mktInf,
			Storage:         storage,
			Swapper:         swapper,
			AuthManager:     authMgr,
			FeeFetcherBase:  feeMgr.FeeFetcher(mktInf.Base),
			CoinLockerBase:  baseCoinLocker,
			FeeFetcherQuote: feeMgr.FeeFetcher(mktInf.Quote),
			CoinLockerQuote: quoteCoinLocker,
			DataCollector:   dataAPI,
			Balancer:        dexBalancer,
			CheckParcelLimit: func(user account.AccountID, calcParcels market.MarketParcelCalculator) bool {
				return orderRouter.CheckParcelLimit(user, mktInf.Name, calcParcels)
			},
			MinimumRate: minRate,
		})
		if err != nil {
			return nil, fmt.Errorf("NewMarket failed: %w", err)
		}
		markets[mktInf.Name] = mkt
		marketTunnels[mktInf.Name] = mkt
		pendingAccounters[mktInf.Name] = mkt
		log.Infof("Preparing historical market data API for market %v...", mktInf.Name)
		err = dataAPI.AddMarketSource(mkt)
		if err != nil {
			return nil, fmt.Errorf("DataSource.AddMarketSource: %w", err)
		}

		// Having loaded the book, get the accounts owning the orders.
		_, buys, sells := mkt.Book()
		for _, lo := range buys {
			usersWithOrders[lo.AccountID] = struct{}{}
		}
		for _, lo := range sells {
			usersWithOrders[lo.AccountID] = struct{}{}
		}
	}

	// Having enumerated all users with booked orders, configure the AuthManager
	// to expect them to connect in a certain time period.
	authMgr.ExpectUsers(usersWithOrders, cfg.BroadcastTimeout)

	// Start the AuthManager and Swapper subsystems after populating the markets
	// map used by the unbook callbacks, and setting the AuthManager's unbook
	// timers for the users with currently booked orders.
	startSubSys("Auth manager", authMgr)
	startSubSys("Swapper", swapper)

	// Set start epoch index for each market. Also create BookSources for the
	// BookRouter, and MarketTunnels for the OrderRouter.
	now := time.Now().UnixMilli()
	bookSources := make(map[string]market.BookSource, len(cfg.Markets))
	cfgMarkets := make([]*msgjson.Market, 0, len(cfg.Markets))
	for name, mkt := range markets {
		startEpochIdx := 1 + now/int64(mkt.EpochDuration())
		mkt.SetStartEpochIdx(startEpochIdx)
		bookSources[name] = mkt
		cfgMarkets = append(cfgMarkets, &msgjson.Market{
			Name:            name,
			Base:            mkt.Base(),
			Quote:           mkt.Quote(),
			LotSize:         mkt.LotSize(),
			RateStep:        mkt.RateStep(),
			EpochLen:        mkt.EpochDuration(),
			MarketBuyBuffer: mkt.MarketBuyBuffer(),
			ParcelSize:      mkt.ParcelSize(),
			MarketStatus: msgjson.MarketStatus{
				StartEpoch: uint64(startEpochIdx),
			},
		})
	}

	// Book router
	bookRouter := market.NewBookRouter(bookSources, feeMgr, server.Route)
	startSubSys("BookRouter", bookRouter)

	// The data API gets the order book from the book router.
	dataAPI.SetBookSource(bookRouter)

	// Market, now that book router is running.
	for name, mkt := range markets {
		startSubSys(marketSubSysName(name), mkt)
	}

	// Order router
	orderRouter = market.NewOrderRouter(&market.OrderRouterConfig{
		Assets:       backedAssets,
		AuthManager:  authMgr,
		Markets:      marketTunnels,
		FeeSource:    feeMgr,
		DEXBalancer:  dexBalancer,
		MatchSwapper: swapper,
	})
	startSubSys("OrderRouter", orderRouter)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfgResp, err := newConfigResponse(cfg, bondAssets, cfgAssets, cfgMarkets)
	if err != nil {
		return nil, err
	}

	dexMgr := &DEX{
		network:     cfg.Network,
		markets:     markets,
		assets:      lockableAssets,
		swapper:     swapper,
		authMgr:     authMgr,
		storage:     storage,
		orderRouter: orderRouter,
		bookRouter:  bookRouter,
		subsystems:  subsystems,
		server:      server,
		configResp:  cfgResp,
	}

	server.RegisterHTTP(msgjson.ConfigRoute, dexMgr.handleDEXConfig)
	server.RegisterHTTP(msgjson.HealthRoute, dexMgr.handleHealthFlag)

	mux := server.Mux()

	// Data API endpoints.
	mux.Route("/api", func(rr chi.Router) {
		if log.Level() == dex.LevelTrace {
			rr.Use(middleware.Logger)
		}
		rr.Use(server.LimitRate)
		rr.Get("/config", server.NewRouteHandler(msgjson.ConfigRoute))
		rr.Get("/healthy", server.NewRouteHandler(msgjson.HealthRoute))
		rr.Get("/spots", server.NewRouteHandler(msgjson.SpotsRoute))
		rr.With(candleParamsParser).Get("/candles/{baseSymbol}/{quoteSymbol}/{binSize}", server.NewRouteHandler(msgjson.CandlesRoute))
		rr.With(candleParamsParser).Get("/candles/{baseSymbol}/{quoteSymbol}/{binSize}/{count}", server.NewRouteHandler(msgjson.CandlesRoute))
		rr.With(orderBookParamsParser).Get("/orderbook/{baseSymbol}/{quoteSymbol}", server.NewRouteHandler(msgjson.OrderBookRoute))
	})

	startSubSys("Comms Server", server)

	ready = true // don't shut down on return

	return dexMgr, nil
}

// Asset retrieves an asset backend by its ID.
func (dm *DEX) Asset(id uint32) (*asset.BackedAsset, error) {
	asset, found := dm.assets[id]
	if !found {
		return nil, fmt.Errorf("no backend for asset %d", id)
	}
	return asset.BackedAsset, nil
}

// SetFeeRateScale specifies a scale factor that the Swapper should use to scale
// the optimal fee rates for new swaps for for the specified asset. That is,
// values above 1 increase the fee rate, while values below 1 decrease it.
func (dm *DEX) SetFeeRateScale(assetID uint32, scale float64) {
	for _, mkt := range dm.markets {
		if mkt.Base() == assetID || mkt.Quote() == assetID {
			mkt.SetFeeRateScale(assetID, scale)
		}
	}
}

// ScaleFeeRate scales the provided fee rate with the given asset's swap fee
// rate scale factor, which is 1.0 by default.
func (dm *DEX) ScaleFeeRate(assetID uint32, rate uint64) uint64 {
	// Any market will have the rate. Just find the first one.
	for _, mkt := range dm.markets {
		if mkt.Base() == assetID || mkt.Quote() == assetID {
			return mkt.ScaleFeeRate(assetID, rate)
		}
	}
	return rate
}

// ConfigMsg returns the current dex configuration, marshalled to JSON.
func (dm *DEX) ConfigMsg() json.RawMessage {
	dm.configRespMtx.RLock()
	defer dm.configRespMtx.RUnlock()
	return dm.configResp.configEnc
}

// TODO: for just market running status, the DEX manager should use its
// knowledge of Market subsystem state.
func (dm *DEX) MarketRunning(mktName string) (found, running bool) {
	mkt := dm.markets[mktName]
	if mkt == nil {
		return
	}
	return true, mkt.Running()
}

// MarketStatus returns the market.Status for the named market. If the market is
// unknown to the DEX, nil is returned.
func (dm *DEX) MarketStatus(mktName string) *market.Status {
	mkt := dm.markets[mktName]
	if mkt == nil {
		return nil
	}
	return mkt.Status()
}

// MarketStatuses returns a map of market names to market.Status for all known
// markets.
func (dm *DEX) MarketStatuses() map[string]*market.Status {
	statuses := make(map[string]*market.Status, len(dm.markets))
	for name, mkt := range dm.markets {
		statuses[name] = mkt.Status()
	}
	return statuses
}

// SuspendMarket schedules a suspension of a given market, with the option to
// persist the orders on the book (or purge the book automatically on market
// shutdown). The scheduled final epoch and suspend time are returned. This is a
// passthrough to the OrderRouter. A TradeSuspension notification is broadcasted
// to all connected clients.
func (dm *DEX) SuspendMarket(name string, tSusp time.Time, persistBooks bool) (suspEpoch *market.SuspendEpoch, err error) {
	name = strings.ToLower(name)

	// Locate the (running) subsystem for this market.
	i := dm.findSubsys(marketSubSysName(name))
	if i == -1 {
		err = fmt.Errorf("market subsystem %s not found", name)
		return
	}
	if !dm.subsystems[i].ssw.On() {
		err = fmt.Errorf("market subsystem %s is not running", name)
		return
	}

	// Go through the order router since OrderRouter is likely to have market
	// status tracking built into it to facilitate resume.
	suspEpoch = dm.orderRouter.SuspendMarket(name, tSusp, persistBooks)
	if suspEpoch == nil {
		err = fmt.Errorf("unable to locate market %s", name)
		return
	}

	// Update config message with suspend schedule.
	dm.configRespMtx.Lock()
	dm.configResp.setMktSuspend(name, uint64(suspEpoch.Idx), persistBooks)
	dm.configRespMtx.Unlock()

	// Broadcast a TradeSuspension notification to all connected clients.
	note, errMsg := msgjson.NewNotification(msgjson.SuspensionRoute, msgjson.TradeSuspension{
		MarketID:    name,
		FinalEpoch:  uint64(suspEpoch.Idx),
		SuspendTime: uint64(suspEpoch.End.UnixMilli()),
		Persist:     persistBooks,
	})
	if errMsg != nil {
		log.Errorf("Failed to create suspend notification: %v", errMsg)
		// Notification or not, the market is resuming, so do not return error.
	} else {
		dm.server.Broadcast(note)
	}
	return
}

func (dm *DEX) findSubsys(name string) int {
	for i := range dm.subsystems {
		if dm.subsystems[i].name == name {
			return i
		}
	}
	return -1
}

// ResumeMarket launches a stopped market subsystem as early as the given time.
// The actual time the market will resume depends on the configure epoch
// duration, as the market only starts at the beginning of an epoch.
func (dm *DEX) ResumeMarket(name string, asSoonAs time.Time) (startEpoch int64, startTime time.Time, err error) {
	name = strings.ToLower(name)
	mkt := dm.markets[name]
	if mkt == nil {
		err = fmt.Errorf("unknown market %s", name)
		return
	}

	// Get the next available start epoch given the earliest allowed time.
	// Requires the market to be stopped already.
	startEpoch = mkt.ResumeEpoch(asSoonAs)
	if startEpoch == 0 {
		err = fmt.Errorf("unable to resume market %s at time %v", name, asSoonAs)
		return
	}

	// Locate the (stopped) subsystem for this market.
	i := dm.findSubsys(marketSubSysName(name))
	if i == -1 {
		err = fmt.Errorf("market subsystem %s not found", name)
		return
	}
	if dm.subsystems[i].ssw.On() {
		err = fmt.Errorf("market subsystem %s not stopped", name)
		return
	}

	// Update config message with resume schedule.
	dm.configRespMtx.Lock()
	epochLen := dm.configResp.setMktResume(name, uint64(startEpoch))
	dm.configRespMtx.Unlock()
	if epochLen == 0 {
		return // couldn't set the new start epoch
	}

	// Configure the start epoch with the Market.
	startTimeMS := int64(epochLen) * startEpoch
	startTime = time.UnixMilli(startTimeMS)
	mkt.SetStartEpochIdx(startEpoch)

	// Relaunch the market.
	ssw := dex.NewStartStopWaiter(mkt)
	dm.subsystems[i].ssw = ssw
	ssw.Start(context.Background())

	// Broadcast a TradeResumption notification to all connected clients.
	note, errMsg := msgjson.NewNotification(msgjson.ResumptionRoute, msgjson.TradeResumption{
		MarketID:   name,
		ResumeTime: uint64(startTimeMS),
		StartEpoch: uint64(startEpoch),
	})
	if errMsg != nil {
		log.Errorf("Failed to create resume notification: %v", errMsg)
		// Notification or not, the market is resuming, so do not return error.
	} else {
		dm.server.Broadcast(note)
	}

	return
}

// AccountInfo returns data for an account.
func (dm *DEX) AccountInfo(aid account.AccountID) (*db.Account, error) {
	// TODO: consider asking the auth manager for account info, including tier.
	// connected, tier := dm.authMgr.AcctStatus(aid)
	return dm.storage.AccountInfo(aid)
}

// ForgiveMatchFail forgives a user for a specific match failure, potentially
// allowing them to resume trading if their score becomes passing.
func (dm *DEX) ForgiveMatchFail(aid account.AccountID, mid order.MatchID) (forgiven, unbanned bool, err error) {
	return dm.authMgr.ForgiveMatchFail(aid, mid)
}

func (dm *DEX) CreatePrepaidBonds(n int, strength uint32, durSecs int64) ([][]byte, error) {
	return dm.authMgr.CreatePrepaidBonds(n, strength, durSecs)
}

func (dm *DEX) AccountMatchOutcomesN(aid account.AccountID, n int) ([]*auth.MatchOutcome, error) {
	return dm.authMgr.AccountMatchOutcomesN(aid, n)
}

func (dm *DEX) UserMatchFails(aid account.AccountID, n int) ([]*auth.MatchFail, error) {
	return dm.authMgr.UserMatchFails(aid, n)
}

// Notify sends a text notification to a connected client.
func (dm *DEX) Notify(acctID account.AccountID, msg *msgjson.Message) {
	dm.authMgr.Notify(acctID, msg)
}

// NotifyAll sends a text notification to all connected clients.
func (dm *DEX) NotifyAll(msg *msgjson.Message) {
	dm.server.Broadcast(msg)
}

// BookOrders returns booked orders for market with base and quote.
func (dm *DEX) BookOrders(base, quote uint32) ([]*order.LimitOrder, error) {
	return dm.storage.BookOrders(base, quote)
}

// EpochOrders returns epoch orders for market with base and quote.
func (dm *DEX) EpochOrders(base, quote uint32) ([]order.Order, error) {
	return dm.storage.EpochOrders(base, quote)
}

// Healthy returns the health status of the DEX.  This is true if
// the storage does not report an error and the BTC backend is synced.
func (dm *DEX) Healthy() bool {
	if dm.storage.LastErr() != nil {
		return false
	}
	if assetID, found := dex.BipSymbolID("btc"); found {
		if synced, _ := dm.assets[assetID].Backend.Synced(); !synced {
			return false
		}
	}
	return true
}

// MatchData embeds db.MatchData with decoded swap transaction coin IDs.
type MatchData struct {
	db.MatchData
	MakerSwap   string
	TakerSwap   string
	MakerRedeem string
	TakerRedeem string
}

func convertMatchData(baseAsset, quoteAsset asset.Backend, md *db.MatchDataWithCoins) *MatchData {
	matchData := MatchData{
		MatchData: md.MatchData,
	}
	// asset0 is the maker swap / taker redeem asset.
	// asset1 is the taker swap / maker redeem asset.
	// Maker selling means asset 0 is base; asset 1 is quote.
	asset0, asset1 := baseAsset, quoteAsset
	if md.TakerSell {
		asset0, asset1 = quoteAsset, baseAsset
	}
	if len(md.MakerSwapCoin) > 0 {
		coinStr, err := asset0.ValidateCoinID(md.MakerSwapCoin)
		if err != nil {
			log.Errorf("Unable to decode coin %x: %v", md.MakerSwapCoin, err)
		}
		matchData.MakerSwap = coinStr
	}
	if len(md.TakerSwapCoin) > 0 {
		coinStr, err := asset1.ValidateCoinID(md.TakerSwapCoin)
		if err != nil {
			log.Errorf("Unable to decode coin %x: %v", md.TakerSwapCoin, err)
		}
		matchData.TakerSwap = coinStr
	}
	if len(md.MakerRedeemCoin) > 0 {
		coinStr, err := asset0.ValidateCoinID(md.MakerRedeemCoin)
		if err != nil {
			log.Errorf("Unable to decode coin %x: %v", md.MakerRedeemCoin, err)
		}
		matchData.MakerRedeem = coinStr
	}
	if len(md.TakerRedeemCoin) > 0 {
		coinStr, err := asset1.ValidateCoinID(md.TakerRedeemCoin)
		if err != nil {
			log.Errorf("Unable to decode coin %x: %v", md.TakerRedeemCoin, err)
		}
		matchData.TakerRedeem = coinStr
	}

	return &matchData
}

// MarketMatchesStreaming streams all matches for market with base and quote.
func (dm *DEX) MarketMatchesStreaming(base, quote uint32, includeInactive bool, N int64, f func(*MatchData) error) (int, error) {
	baseAsset := dm.assets[base]
	if baseAsset == nil {
		return 0, fmt.Errorf("asset %d not found", base)
	}
	quoteAsset := dm.assets[quote]
	if quoteAsset == nil {
		return 0, fmt.Errorf("asset %d not found", quote)
	}
	fDB := func(md *db.MatchDataWithCoins) error {
		matchData := convertMatchData(baseAsset.Backend, quoteAsset.Backend, md)
		return f(matchData)
	}
	return dm.storage.MarketMatchesStreaming(base, quote, includeInactive, N, fDB)
}

// MarketMatches returns matches for market with base and quote.
func (dm *DEX) MarketMatches(base, quote uint32) ([]*MatchData, error) {
	baseAsset := dm.assets[base]
	if baseAsset == nil {
		return nil, fmt.Errorf("asset %d not found", base)
	}
	quoteAsset := dm.assets[quote]
	if quoteAsset == nil {
		return nil, fmt.Errorf("asset %d not found", quote)
	}
	mds, err := dm.storage.MarketMatches(base, quote)
	if err != nil {
		return nil, err
	}

	matchDatas := make([]*MatchData, 0, len(mds))
	for _, md := range mds {
		matchData := convertMatchData(baseAsset.Backend, quoteAsset.Backend, md)
		matchDatas = append(matchDatas, matchData)
	}

	return matchDatas, nil
}

// EnableDataAPI can be called via admin API to enable or disable the HTTP data
// API endpoints.
func (dm *DEX) EnableDataAPI(yes bool) {
	dm.server.EnableDataAPI(yes)
}

func (dm *DEX) ForgiveUser(user account.AccountID) error {
	return dm.authMgr.ForgiveUser(user)
}

// candleParamsParser is middleware for the /candles routes. Parses the
// *msgjson.CandlesRequest from the URL parameters.
func candleParamsParser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baseID, quoteID, errMsg := parseBaseQuoteIDs(r)
		if errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}

		// Ensure the bin size is a valid duration string.
		binSize := chi.URLParam(r, "binSize")
		_, err := time.ParseDuration(binSize)
		if err != nil {
			http.Error(w, "bin size unparseable", http.StatusBadRequest)
			return
		}

		countStr := chi.URLParam(r, "count")
		count := 0
		if countStr != "" {
			count, err = strconv.Atoi(countStr)
			if err != nil {
				http.Error(w, "count unparseable", http.StatusBadRequest)
				return
			}
		}
		ctx := context.WithValue(r.Context(), comms.CtxThing, &msgjson.CandlesRequest{
			BaseID:     baseID,
			QuoteID:    quoteID,
			BinSize:    binSize,
			NumCandles: count,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// orderBookParamsParser is middleware for the /orderbook route. Parses the
// *msgjson.OrderBookSubscription from the URL parameters.
func orderBookParamsParser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baseID, quoteID, errMsg := parseBaseQuoteIDs(r)
		if errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), comms.CtxThing, &msgjson.OrderBookSubscription{
			Base:  baseID,
			Quote: quoteID,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// parseBaseQuoteIDs parses the "baseSymbol" and "quoteSymbol" URL parameters
// from the request.
func parseBaseQuoteIDs(r *http.Request) (baseID, quoteID uint32, errMsg string) {
	baseID, found := dex.BipSymbolID(chi.URLParam(r, "baseSymbol"))
	if !found {
		return 0, 0, "unknown base"
	}
	quoteID, found = dex.BipSymbolID(chi.URLParam(r, "quoteSymbol"))
	if !found {
		return 0, 0, "unknown quote"
	}
	return
}
