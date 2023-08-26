package mm

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
)

var (
	tUTXOAssetA = &dex.Asset{
		ID:           42,
		Symbol:       "dcr",
		Version:      0, // match the stubbed (*TXCWallet).Info result
		SwapSize:     251,
		SwapSizeBase: 85,
		RedeemSize:   200,
		MaxFeeRate:   10,
		SwapConf:     1,
	}

	tUTXOAssetB = &dex.Asset{
		ID:           0,
		Symbol:       "btc",
		Version:      0, // match the stubbed (*TXCWallet).Info result
		SwapSize:     225,
		SwapSizeBase: 76,
		RedeemSize:   260,
		MaxFeeRate:   2,
		SwapConf:     1,
	}
	tACCTAsset = &dex.Asset{
		ID:           60,
		Symbol:       "eth",
		Version:      0, // match the stubbed (*TXCWallet).Info result
		SwapSize:     135000,
		SwapSizeBase: 135000,
		RedeemSize:   68000,
		MaxFeeRate:   20,
		SwapConf:     1,
	}
	tWalletInfo = &asset.WalletInfo{
		Version:           0,
		SupportedVersions: []uint32{0},
		UnitInfo: dex.UnitInfo{
			Conventional: dex.Denomination{
				ConversionFactor: 1e8,
			},
		},
		AvailableWallets: []*asset.WalletDefinition{{
			Type: "type",
		}},
	}
)

type tCreator struct {
	*tDriver
	doesntExist bool
	existsErr   error
	createErr   error
}

func (ctr *tCreator) Exists(walletType, dataDir string, settings map[string]string, net dex.Network) (bool, error) {
	return !ctr.doesntExist, ctr.existsErr
}

func (ctr *tCreator) Create(*asset.CreateWalletParams) error {
	return ctr.createErr
}

func init() {
	asset.Register(tUTXOAssetA.ID, &tDriver{
		decodedCoinID: tUTXOAssetA.Symbol + "-coin",
		winfo:         tWalletInfo,
	})
	asset.Register(tUTXOAssetB.ID, &tCreator{
		tDriver: &tDriver{
			decodedCoinID: tUTXOAssetB.Symbol + "-coin",
			winfo:         tWalletInfo,
		},
	})
	asset.Register(tACCTAsset.ID, &tCreator{
		tDriver: &tDriver{
			decodedCoinID: tACCTAsset.Symbol + "-coin",
			winfo:         tWalletInfo,
		},
	})
	rand.Seed(time.Now().UnixNano())
}

type tDriver struct {
	wallet        asset.Wallet
	decodedCoinID string
	winfo         *asset.WalletInfo
}

func (drv *tDriver) Open(cfg *asset.WalletConfig, logger dex.Logger, net dex.Network) (asset.Wallet, error) {
	return drv.wallet, nil
}

func (drv *tDriver) DecodeCoinID(coinID []byte) (string, error) {
	return drv.decodedCoinID, nil
}

func (drv *tDriver) Info() *asset.WalletInfo {
	return drv.winfo
}

type tBookFeed struct{}

func (t *tBookFeed) Next() <-chan *core.BookUpdate { return make(chan *core.BookUpdate, 1) }
func (t *tBookFeed) Close()                        {}
func (t *tBookFeed) Candles(dur string) error      { return nil }

type tCore struct {
	assetBalances     map[uint32]*core.WalletBalance
	assetBalanceErr   error
	market            *core.Market
	orderEstimate     *core.OrderEstimate
	sellSwapFees      uint64
	sellRedeemFees    uint64
	sellRefundFees    uint64
	buySwapFees       uint64
	buyRedeemFees     uint64
	buyRefundFees     uint64
	singleLotFeesErr  error
	preOrderParam     *core.TradeForm
	tradeResult       *core.Order
	multiTradeResult  []*core.Order
	noteFeed          chan core.Notification
	isAccountLocker   map[uint32]bool
	maxBuyEstimate    *core.MaxOrderEstimate
	maxBuyErr         error
	maxSellEstimate   *core.MaxOrderEstimate
	maxSellErr        error
	cancelsPlaced     []dex.Bytes
	buysPlaced        []*core.TradeForm
	sellsPlaced       []*core.TradeForm
	multiTradesPlaced []*core.MultiTradeForm
	maxFundingFees    uint64
	book              *orderbook.OrderBook
}

func (c *tCore) NotificationFeed() *core.NoteFeed {
	return &core.NoteFeed{C: c.noteFeed}
}
func (c *tCore) ExchangeMarket(host string, base, quote uint32) (*core.Market, error) {
	return c.market, nil
}

var _ core.BookFeed = (*tBookFeed)(nil)

func (t *tCore) SyncBook(host string, base, quote uint32) (*orderbook.OrderBook, core.BookFeed, error) {
	return t.book, &tBookFeed{}, nil
}
func (*tCore) SupportedAssets() map[uint32]*core.SupportedAsset {
	return nil
}
func (c *tCore) SingleLotFees(form *core.SingleLotFeesForm) (uint64, uint64, uint64, error) {
	if c.singleLotFeesErr != nil {
		return 0, 0, 0, c.singleLotFeesErr
	}
	if form.Sell {
		return c.sellSwapFees, c.sellRedeemFees, c.sellRefundFees, nil
	}
	return c.buySwapFees, c.buyRedeemFees, c.buyRefundFees, nil
}
func (t *tCore) Cancel(oidB dex.Bytes) error {
	t.cancelsPlaced = append(t.cancelsPlaced, oidB)
	return nil
}
func (c *tCore) Trade(pw []byte, form *core.TradeForm) (*core.Order, error) {
	if form.Sell {
		c.sellsPlaced = append(c.sellsPlaced, form)
	} else {
		c.buysPlaced = append(c.buysPlaced, form)
	}
	return c.tradeResult, nil
}
func (c *tCore) MaxBuy(host string, base, quote uint32, rate uint64) (*core.MaxOrderEstimate, error) {
	if c.maxBuyErr != nil {
		return nil, c.maxBuyErr
	}
	return c.maxBuyEstimate, nil
}
func (c *tCore) MaxSell(host string, base, quote uint32) (*core.MaxOrderEstimate, error) {
	if c.maxSellErr != nil {
		return nil, c.maxSellErr
	}
	return c.maxSellEstimate, nil
}
func (c *tCore) AssetBalance(assetID uint32) (*core.WalletBalance, error) {
	return c.assetBalances[assetID], c.assetBalanceErr
}
func (c *tCore) PreOrder(form *core.TradeForm) (*core.OrderEstimate, error) {
	c.preOrderParam = form
	return c.orderEstimate, nil
}
func (c *tCore) MultiTrade(pw []byte, forms *core.MultiTradeForm) ([]*core.Order, error) {
	c.multiTradesPlaced = append(c.multiTradesPlaced, forms)
	return c.multiTradeResult, nil
}

func (c *tCore) WalletState(assetID uint32) *core.WalletState {
	isAccountLocker := c.isAccountLocker[assetID]

	var traits asset.WalletTrait
	if isAccountLocker {
		traits |= asset.WalletTraitAccountLocker
	}

	return &core.WalletState{
		Traits: traits,
	}
}
func (c *tCore) MaxFundingFees(fromAsset uint32, host string, numTrades uint32, options map[string]string) (uint64, error) {
	return c.maxFundingFees, nil
}
func (c *tCore) Login(pw []byte) error {
	return nil
}
func (c *tCore) OpenWallet(assetID uint32, pw []byte) error {
	return nil
}

func (c *tCore) User() *core.User {
	return nil
}

var _ clientCore = (*tCore)(nil)

func tMaxOrderEstimate(lots uint64, swapFees, redeemFees uint64) *core.MaxOrderEstimate {
	return &core.MaxOrderEstimate{
		Swap: &asset.SwapEstimate{
			RealisticWorstCase: swapFees,
			Lots:               lots,
		},
		Redeem: &asset.RedeemEstimate{
			RealisticWorstCase: redeemFees,
		},
	}
}

func (c *tCore) setAssetBalances(balances map[uint32]uint64) {
	c.assetBalances = make(map[uint32]*core.WalletBalance)
	for assetID, bal := range balances {
		c.assetBalances[assetID] = &core.WalletBalance{
			Balance: &db.Balance{
				Balance: asset.Balance{
					Available: bal,
				},
			},
		}
	}
}

func (c *tCore) clearTradesAndCancels() {
	c.cancelsPlaced = make([]dex.Bytes, 0)
	c.buysPlaced = make([]*core.TradeForm, 0)
	c.sellsPlaced = make([]*core.TradeForm, 0)
	c.multiTradesPlaced = make([]*core.MultiTradeForm, 0)
}

func newTCore() *tCore {
	return &tCore{
		assetBalances:   make(map[uint32]*core.WalletBalance),
		noteFeed:        make(chan core.Notification),
		isAccountLocker: make(map[uint32]bool),
		cancelsPlaced:   make([]dex.Bytes, 0),
		buysPlaced:      make([]*core.TradeForm, 0),
		sellsPlaced:     make([]*core.TradeForm, 0),
	}
}

type tOrderBook struct {
	midGap    uint64
	midGapErr error

	bidsVWAP map[uint64]vwapResult
	asksVWAP map[uint64]vwapResult
	vwapErr  error
}

var _ dexOrderBook = (*tOrderBook)(nil)

func (t *tOrderBook) VWAP(numLots, _ uint64, sell bool) (avg, extrema uint64, filled bool, err error) {
	if t.vwapErr != nil {
		return 0, 0, false, t.vwapErr
	}

	if sell {
		res, found := t.asksVWAP[numLots]
		if !found {
			return 0, 0, false, nil
		}
		return res.avg, res.extrema, true, nil
	}

	res, found := t.bidsVWAP[numLots]
	if !found {
		return 0, 0, false, nil
	}
	return res.avg, res.extrema, true, nil
}

func (o *tOrderBook) MidGap() (uint64, error) {
	if o.midGapErr != nil {
		return 0, o.midGapErr
	}
	return o.midGap, nil
}

type tOracle struct {
	marketPrice float64
}

func (o *tOracle) getMarketPrice(base, quote uint32) float64 {
	return o.marketPrice
}

var tLogger = dex.StdOutLogger("mm_TEST", dex.LevelTrace)

func TestSetupBalances(t *testing.T) {
	tCore := newTCore()

	dcrBtcID := fmt.Sprintf("%s-%d-%d", "host1", 42, 0)
	dcrEthID := fmt.Sprintf("%s-%d-%d", "host1", 42, 60)

	tests := []struct {
		name          string
		cfgs          []*BotConfig
		assetBalances map[uint32]uint64

		wantReserves map[string]map[uint32]uint64
		wantErr      bool
	}{
		{
			name: "percentages only, ok",
			cfgs: []*BotConfig{
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       0,
					BaseBalanceType:  Percentage,
					BaseBalance:      50,
					QuoteBalanceType: Percentage,
					QuoteBalance:     50,
				},
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       60,
					BaseBalanceType:  Percentage,
					BaseBalance:      50,
					QuoteBalanceType: Percentage,
					QuoteBalance:     100,
				},
			},

			assetBalances: map[uint32]uint64{
				0:  1000,
				42: 1000,
				60: 2000,
			},

			wantReserves: map[string]map[uint32]uint64{
				dcrBtcID: {
					0:  500,
					42: 500,
				},
				dcrEthID: {
					42: 500,
					60: 2000,
				},
			},
		},

		{
			name: "50% + 51% error",
			cfgs: []*BotConfig{
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       0,
					BaseBalanceType:  Percentage,
					BaseBalance:      50,
					QuoteBalanceType: Percentage,
					QuoteBalance:     50,
				},
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       60,
					BaseBalanceType:  Percentage,
					BaseBalance:      51,
					QuoteBalanceType: Percentage,
					QuoteBalance:     100,
				},
			},

			assetBalances: map[uint32]uint64{
				0:  1000,
				42: 1000,
				60: 2000,
			},

			wantErr: true,
		},

		{
			name: "combine amount and percentages, ok",
			cfgs: []*BotConfig{
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       0,
					BaseBalanceType:  Amount,
					BaseBalance:      499,
					QuoteBalanceType: Percentage,
					QuoteBalance:     50,
				},
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       60,
					BaseBalanceType:  Percentage,
					BaseBalance:      50,
					QuoteBalanceType: Percentage,
					QuoteBalance:     100,
				},
			},

			assetBalances: map[uint32]uint64{
				0:  1000,
				42: 1000,
				60: 2000,
			},

			wantReserves: map[string]map[uint32]uint64{
				dcrBtcID: {
					0:  500,
					42: 499,
				},
				dcrEthID: {
					42: 500,
					60: 2000,
				},
			},
		},
		{
			name: "combine amount and percentages, too high error",
			cfgs: []*BotConfig{
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       0,
					BaseBalanceType:  Amount,
					BaseBalance:      501,
					QuoteBalanceType: Percentage,
					QuoteBalance:     50,
				},
				{
					Host:             "host1",
					BaseAsset:        42,
					QuoteAsset:       60,
					BaseBalanceType:  Percentage,
					BaseBalance:      50,
					QuoteBalanceType: Percentage,
					QuoteBalance:     100,
				},
			},

			assetBalances: map[uint32]uint64{
				0:  1000,
				42: 1000,
				60: 2000,
			},

			wantErr: true,
		},
	}

	for _, test := range tests {
		tCore.setAssetBalances(test.assetBalances)

		mm, err := NewMarketMaker(tCore, tLogger)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		err = mm.setupBalances(test.cfgs)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error, got nil", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		for botID, wantReserve := range test.wantReserves {
			botReserves := mm.botBalances[botID]
			for assetID, wantReserve := range wantReserve {
				if botReserves.balances[assetID].Available != wantReserve {
					t.Fatalf("%s: unexpected reserve for bot %s, asset %d. "+
						"want %d, got %d", test.name, botID, assetID, wantReserve,
						botReserves.balances[assetID])
				}
			}
		}
	}
}

func TestSegregatedCoreMaxSell(t *testing.T) {
	tCore := newTCore()
	tCore.isAccountLocker[60] = true
	dcrBtcID := fmt.Sprintf("%s-%d-%d", "host1", 42, 0)
	dcrEthID := fmt.Sprintf("%s-%d-%d", "host1", 42, 60)

	// Whatever is returned from PreOrder is returned from this function.
	// What we need to test is what is passed to PreOrder.
	orderEstimate := &core.OrderEstimate{
		Swap: &asset.PreSwap{
			Estimate: &asset.SwapEstimate{
				Lots:               5,
				Value:              5e8,
				MaxFees:            1600,
				RealisticWorstCase: 12010,
				RealisticBestCase:  6008,
			},
		},
		Redeem: &asset.PreRedeem{
			Estimate: &asset.RedeemEstimate{
				RealisticBestCase:  2800,
				RealisticWorstCase: 6500,
			},
		},
	}
	tCore.orderEstimate = orderEstimate

	expectedResult := &core.MaxOrderEstimate{
		Swap: &asset.SwapEstimate{
			Lots:               5,
			Value:              5e8,
			MaxFees:            1600,
			RealisticWorstCase: 12010,
			RealisticBestCase:  6008,
		},
		Redeem: &asset.RedeemEstimate{
			RealisticBestCase:  2800,
			RealisticWorstCase: 6500,
		},
	}

	tests := []struct {
		name          string
		cfg           *BotConfig
		assetBalances map[uint32]uint64
		market        *core.Market
		swapFees      uint64
		redeemFees    uint64
		refundFees    uint64

		expectPreOrderParam *core.TradeForm
		wantErr             bool
	}{
		{
			name: "ok",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     4 * 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
		},
		{
			name: "1 lot",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e6 + 1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
		},
		{
			name: "not enough for 1 swap",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e6 + 999,
				QuoteBalanceType: Amount,
				QuoteBalance:     1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			wantErr:    true,
		},
		{
			name: "not enough for 1 lot of redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       60,
				BaseBalanceType:  Amount,
				BaseBalance:      1e6 + 1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     999,
			},
			assetBalances: map[uint32]uint64{
				42: 1e7,
				60: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			wantErr:    true,
		},
		{
			name: "redeem fees don't matter if not account locker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e6 + 1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     999,
			},
			assetBalances: map[uint32]uint64{
				42: 1e7,
				0:  1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     1e6,
			},
		},
		{
			name: "2 lots with refund fees, not account locker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      2e6 + 2000,
				QuoteBalanceType: Amount,
				QuoteBalance:     1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     2e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 1000,
		},
		{
			name: "1 lot with refund fees, account locker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       60,
				BaseBalanceType:  Amount,
				BaseBalance:      2e6 + 2000,
				QuoteBalanceType: Amount,
				QuoteBalance:     1000,
			},
			assetBalances: map[uint32]uint64{
				60: 1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   60,
				Sell:    true,
				Qty:     1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 1000,
		},
	}

	for _, test := range tests {
		tCore.setAssetBalances(test.assetBalances)
		tCore.market = test.market
		tCore.sellSwapFees = test.swapFees
		tCore.sellRedeemFees = test.redeemFees
		tCore.sellRefundFees = test.refundFees

		mm, err := NewMarketMaker(tCore, tLogger)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		err = mm.setupBalances([]*BotConfig{test.cfg})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		mkt := dcrBtcID
		if test.cfg.QuoteAsset == 60 {
			mkt = dcrEthID
		}

		segregatedCore := mm.wrappedCoreForBot(mkt)
		res, err := segregatedCore.MaxSell("host1", test.cfg.BaseAsset, test.cfg.QuoteAsset)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if !reflect.DeepEqual(tCore.preOrderParam, test.expectPreOrderParam) {
			t.Fatalf("%s: expected pre order param %+v != actual %+v", test.name, test.expectPreOrderParam, tCore.preOrderParam)
		}

		if !reflect.DeepEqual(res, expectedResult) {
			t.Fatalf("%s: expected max sell result %+v != actual %+v", test.name, expectedResult, res)
		}
	}
}

func TestSegregatedCoreMaxBuy(t *testing.T) {
	tCore := newTCore()

	tCore.isAccountLocker[60] = true
	dcrBtcID := fmt.Sprintf("%s-%d-%d", "host1", 42, 0)
	ethBtcID := fmt.Sprintf("%s-%d-%d", "host1", 60, 0)

	// Whatever is returned from PreOrder is returned from this function.
	// What we need to test is what is passed to PreOrder.
	orderEstimate := &core.OrderEstimate{
		Swap: &asset.PreSwap{
			Estimate: &asset.SwapEstimate{
				Lots:               5,
				Value:              5e8,
				MaxFees:            1600,
				RealisticWorstCase: 12010,
				RealisticBestCase:  6008,
			},
		},
		Redeem: &asset.PreRedeem{
			Estimate: &asset.RedeemEstimate{
				RealisticBestCase:  2800,
				RealisticWorstCase: 6500,
			},
		},
	}
	tCore.orderEstimate = orderEstimate

	expectedResult := &core.MaxOrderEstimate{
		Swap: &asset.SwapEstimate{
			Lots:               5,
			Value:              5e8,
			MaxFees:            1600,
			RealisticWorstCase: 12010,
			RealisticBestCase:  6008,
		},
		Redeem: &asset.RedeemEstimate{
			RealisticBestCase:  2800,
			RealisticWorstCase: 6500,
		},
	}

	tests := []struct {
		name          string
		cfg           *BotConfig
		assetBalances map[uint32]uint64
		market        *core.Market
		rate          uint64
		swapFees      uint64
		redeemFees    uint64
		refundFees    uint64

		expectPreOrderParam *core.TradeForm
		wantErr             bool
	}{
		{
			name: "ok",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Rate:    5e7,
				Qty:     9 * 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
		},
		{
			name: "1 lot",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     (1e6 * 5e7 / 1e8) + 1000,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     1e6,
				Rate:    5e7,
			},
			swapFees:   1000,
			redeemFees: 1000,
		},
		{
			name: "not enough for 1 swap",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     (1e6 * 5e7 / 1e8) + 999,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			wantErr:    true,
		},
		{
			name: "not enough for 1 lot of redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        60,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      999,
				QuoteBalanceType: Amount,
				QuoteBalance:     (1e6 * 5e7 / 1e8) + 1000,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				60: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			wantErr:    true,
		},
		{
			name: "only account locker affected by redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      999,
				QuoteBalanceType: Amount,
				QuoteBalance:     (1e6 * 5e7 / 1e8) + 1000,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     1e6,
				Rate:    5e7,
			},
		},
		{
			name: "2 lots with refund fees, not account locker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     (2e6 * 5e7 / 1e8) + 2000,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     2e6,
				Rate:    5e7,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 1000,
		},
		{
			name: "1 lot with refund fees, account locker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        60,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     (2e6 * 5e7 / 1e8) + 2000,
			},
			rate: 5e7,
			assetBalances: map[uint32]uint64{
				60: 1e7,
				0:  1e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			expectPreOrderParam: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    60,
				Quote:   0,
				Sell:    false,
				Qty:     1e6,
				Rate:    5e7,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 1000,
		},
	}

	for _, test := range tests {
		if test.name != "1 lot with refund fees, account locker" {
			continue
		}
		tCore.setAssetBalances(test.assetBalances)
		tCore.market = test.market
		tCore.buySwapFees = test.swapFees
		tCore.buyRedeemFees = test.redeemFees

		mm, err := NewMarketMaker(tCore, tLogger)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		err = mm.setupBalances([]*BotConfig{test.cfg})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		mkt := dcrBtcID
		if test.cfg.BaseAsset != 42 {
			mkt = ethBtcID
		}
		segregatedCore := mm.wrappedCoreForBot(mkt)
		res, err := segregatedCore.MaxBuy("host1", test.cfg.BaseAsset, test.cfg.QuoteAsset, test.rate)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if !reflect.DeepEqual(tCore.preOrderParam, test.expectPreOrderParam) {
			t.Fatalf("%s: expected pre order param %+v != actual %+v", test.name, test.expectPreOrderParam, tCore.preOrderParam)
		}

		if !reflect.DeepEqual(res, expectedResult) {
			t.Fatalf("%s: expected max buy result %+v != actual %+v", test.name, expectedResult, res)
		}
	}
}

func assetBalancesMatch(expected map[uint32]*botBalance, botName string, mm *MarketMaker) error {
	for assetID, exp := range expected {
		actual := mm.botBalances[botName].balances[assetID]
		if !reflect.DeepEqual(exp, actual) {
			return fmt.Errorf("asset %d expected %+v != actual %+v\n", assetID, exp, actual)
		}
	}
	return nil
}

func TestSegregatedCoreTrade(t *testing.T) {
	t.Run("single trade", func(t *testing.T) {
		testSegregatedCoreTrade(t, false)
	})
	t.Run("multi trade", func(t *testing.T) {
		testSegregatedCoreTrade(t, true)
	})
}

func testSegregatedCoreTrade(t *testing.T, testMultiTrade bool) {
	id := encode.RandomBytes(order.OrderIDSize)
	id2 := encode.RandomBytes(order.OrderIDSize)

	matchIDs := make([]order.MatchID, 5)
	for i := range matchIDs {
		var matchID order.MatchID
		copy(matchID[:], encode.RandomBytes(order.MatchIDSize))
		matchIDs[i] = matchID
	}

	type noteAndBalances struct {
		note    core.Notification
		balance map[uint32]*botBalance
	}

	type test struct {
		name           string
		multiTradeOnly bool

		cfg               *BotConfig
		multiTrade        *core.MultiTradeForm
		trade             *core.TradeForm
		assetBalances     map[uint32]uint64
		postTradeBalances map[uint32]*botBalance
		market            *core.Market
		swapFees          uint64
		redeemFees        uint64
		refundFees        uint64
		tradeRes          *core.Order
		multiTradeRes     []*core.Order
		notifications     []*noteAndBalances
		isAccountLocker   map[uint32]bool
		maxFundingFees    uint64

		wantErr bool
	}

	tests := []test{
		// "cancelled order, 1/2 lots filled, sell"
		{
			name: "cancelled order, 1/2 lots filled, sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       2e6 + 2000,
				RedeemLockedAmt: 2000,
				Sell:            true,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000,
					FundingOrder: 2000,
				},
				42: {
					Available:    (1e7 / 2) - 2e6 - 2000,
					FundingOrder: 2e6 + 2000,
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      true,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchComplete,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusCanceled,
							BaseID:           42,
							QuoteID:          0,
							Qty:              2e6,
							Sell:             true,
							Filled:           2e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       800,
								Redemption: 800,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) + calc.BaseToQuote(5e7, 1e6) - 800,
						},
						42: {
							Available: (1e7 / 2) - 1e6 - 800,
						},
					},
				},
			},
		},
		// "cancelled order, 1/2 lots filled, buy"
		{
			name: "cancelled order, 1/2 lots filled, buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:        id,
				LockedAmt: calc.BaseToQuote(5e7, 2e6) + 2000,
				Sell:      false,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
					FundingOrder: calc.BaseToQuote(5e7, 2e6) + 2000,
				},
				42: {
					Available: (1e7 / 2),
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      false,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Swap:    &core.Coin{},
									Status:  order.MakerSwapCast,
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchComplete,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 1000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusCanceled,
							BaseID:           42,
							QuoteID:          0,
							Qty:              2e6,
							Sell:             false,
							Filled:           2e6,
							AllFeesConfirmed: true,
							Rate:             5e7,
							FeesPaid: &core.FeeBreakdown{
								Swap:       800,
								Redemption: 800,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) - 800 - calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 800,
						},
					},
				},
			},
		},
		// "fully filled order, sell"
		{
			name: "fully filled order, sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       2e6 + 2000,
				RedeemLockedAmt: 2000,
				Sell:            true,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000,
					FundingOrder: 2000,
				},
				42: {
					Available:    (1e7 / 2) - 2e6 - 2000,
					FundingOrder: 2e6 + 2000,
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      true,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchComplete,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 1e6 + 2000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:      id,
							Status:  order.OrderStatusExecuted,
							BaseID:  42,
							QuoteID: 0,
							Qty:     2e6,
							Sell:    true,
							Filled:  2e6,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    55e6,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    55e6,
							Status:  order.MatchComplete,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    55e6,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6) + calc.BaseToQuote(55e6, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000,
							FundingOrder: 2000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusExecuted,
							BaseID:           42,
							QuoteID:          0,
							Qty:              2e6,
							Sell:             true,
							Filled:           2e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
									Status:  order.MatchConfirmed,
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    55e6,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
									Status:  order.MatchConfirmed,
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) - 1600 + calc.BaseToQuote(5e7, 1e6) + calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available: (1e7 / 2) - 2e6 - 1600,
						},
					},
				},
			},
		},
		// "fully filled order, buy"
		{
			name: "fully filled order, buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:        id,
				LockedAmt: calc.BaseToQuote(5e7, 2e6) + 2000,
				Sell:      true,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
					FundingOrder: calc.BaseToQuote(5e7, 2e6) + 2000,
				},
				42: {
					Available: (1e7 / 2),
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      false,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Swap:    &core.Coin{},
									Status:  order.MakerSwapCast,
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchComplete,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6),
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000,
						},
						42: {
							Available: (1e7 / 2) - 1000 + 1e6,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:      id,
							Status:  order.OrderStatusExecuted,
							BaseID:  42,
							QuoteID: 0,
							Qty:     2e6,
							Rate:    5e7,
							Sell:    false,
							Filled:  2e6,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
									Status:  order.MatchConfirmed,
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    45e6,
									Swap:    &core.Coin{},
									Status:  order.MakerSwapCast,
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:     (1e7 / 2) + 1e6 - 1000,
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    45e6,
							Status:  order.MatchComplete,
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:     (1e7 / 2) + 1e6 - 1000,
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    45e6,
							Status:  order.MatchConfirmed,
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available: (1e7 / 2) + 2e6 - 2000,
						},
					},
				},
			},
		},
		// "fully filled order, sell, accountLocker"
		{
			name: "fully filled order, sell, accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       2e6 + 2000,
				RedeemLockedAmt: 2000,
				RefundLockedAmt: 1600,
				Sell:            true,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000,
					FundingOrder: 2000,
				},
				42: {
					Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
					FundingOrder: 2e6 + 2000 + 1600,
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      true,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Swap:    &core.Coin{},
									Status:  order.MakerSwapCast,
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 1e6 + 2000 + 1600,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchComplete,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000,
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 1e6 + 2000 + 1600,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 1e6 + 2000 + 1600,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:      id,
							Status:  order.OrderStatusExecuted,
							BaseID:  42,
							QuoteID: 0,
							Qty:     2e6,
							Sell:    true,
							Filled:  2e6,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    55e6,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 2000 + 1600,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    55e6,
							Status:  order.MatchComplete,
							Redeem:  &core.Coin{},
							Swap:    &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6),
							FundingOrder:  2000,
							PendingRedeem: calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 2000 + 1600,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    55e6,
							Status:  order.MatchConfirmed,
							Redeem:  &core.Coin{},
							Swap:    &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 + calc.BaseToQuote(5e7, 1e6) + calc.BaseToQuote(55e6, 1e6),
							FundingOrder: 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 2000 - 1600,
							FundingOrder: 2000 + 1600,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusExecuted,
							BaseID:           42,
							QuoteID:          0,
							Qty:              2e6,
							Sell:             true,
							Filled:           2e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    55e6,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) - 1600 + calc.BaseToQuote(5e7, 1e6) + calc.BaseToQuote(55e6, 1e6),
						},
						42: {
							Available: (1e7 / 2) - 2e6 - 1600,
						},
					},
				},
			},
		},
		// "fully filled order, buy, accountLocker"
		{
			name: "fully filled order, buy, accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     2e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       calc.BaseToQuote(5e7, 2e6) + 2000,
				RefundLockedAmt: 1600,
				Sell:            true,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6) - 1600,
					FundingOrder: calc.BaseToQuote(5e7, 2e6) + 2000 + 1600,
				},
				42: {
					Available: (1e7 / 2),
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      false,
							Filled:    1e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6) - 1600,
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000 + 1600,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchComplete,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6) - 1600,
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000 + 1600,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 2e6) - 1600,
							FundingOrder: calc.BaseToQuote(5e7, 1e6) + 2000 + 1600,
						},
						42: {
							Available: (1e7 / 2) - 1000 + 1e6,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:      id,
							Status:  order.OrderStatusExecuted,
							BaseID:  42,
							QuoteID: 0,
							Qty:     2e6,
							Rate:    5e7,
							Sell:    false,
							Filled:  2e6,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Redemption: 1600,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchConfirmed,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    45e6,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6) - 1600,
							FundingOrder: 2000 + 1600,
						},
						42: {
							Available:     (1e7 / 2) + 1e6 - 1000,
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    45e6,
							Status:  order.MatchComplete,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6) - 1600,
							FundingOrder: 2000 + 1600,
						},
						42: {
							Available:     (1e7 / 2) + 1e6 - 1000,
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    45e6,
							Status:  order.MatchConfirmed,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 2000 - calc.BaseToQuote(5e7, 1e6) - calc.BaseToQuote(45e6, 1e6) - 1600,
							FundingOrder: 2000 + 1600,
						},
						42: {
							Available: (1e7 / 2) + 2e6 - 2000,
						},
					},
				},
			},
		},
		// "buy, 1 match refunded, 1 revoked before swap, 1 redeemed match, not accountLocker"
		{
			name: "buy, 1 refunded, 1 revoked before swap, 1 redeemed match, not accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     3e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:        id,
				LockedAmt: calc.BaseToQuote(5e7, 3e6) + 3000,
				Sell:      false,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 3e6) - 3000,
					FundingOrder: calc.BaseToQuote(5e7, 3e6) + 3000,
				},
				42: {
					Available: (1e7 / 2),
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      false,
							Filled:    2e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 3000 - calc.BaseToQuote(5e7, 3e6),
							FundingOrder: 3000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 3e6 - 3000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Swap:    &core.Coin{},
							Refund:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 3000 - calc.BaseToQuote(5e7, 2e6) - 800,
							FundingOrder: 3000,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 2e6 - 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Status:  order.NewlyMatched,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 3000 - calc.BaseToQuote(5e7, 2e6) - 800,
							FundingOrder: 3000 + calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[2][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - 3000 - calc.BaseToQuote(5e7, 2e6) - 800,
							FundingOrder: 3000 + calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 1000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusRevoked,
							BaseID:           42,
							QuoteID:          0,
							Qty:              2e6,
							Sell:             false,
							Filled:           2e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Refund:     400,
								Redemption: 500,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Revoked: true,
									Refund:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Revoked: true,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) - calc.BaseToQuote(5e7, 1e6) - 1600 - 400,
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 500,
						},
					},
				},
			},
		},
		// "sell, 1 match refunded, 1 revoked before swap, 1 redeemed match, not accountLocker"
		{
			name: "sell, 1 refunded, 1 revoked before swap, 1 redeemed match, not accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     3e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:        id,
				LockedAmt: 3e6 + 3000,
				Sell:      false,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available: (1e7 / 2),
				},
				42: {
					Available:    (1e7 / 2) - 3e6 - 3000,
					FundingOrder: 3e6 + 3000,
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      true,
							Filled:    2e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 3e6) - 3000,
						},
						42: {
							Available:    (1e7 / 2) - 3e6 - 3000,
							FundingOrder: 3000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Swap:    &core.Coin{},
							Refund:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 2e6) - 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 800,
							FundingOrder: 3000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Status:  order.NewlyMatched,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 1e6) - 1000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 800,
							FundingOrder: 3000 + 1e6,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[2][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) + calc.BaseToQuote(5e7, 1e6) - 1000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 800,
							FundingOrder: 3000 + 1e6,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusRevoked,
							BaseID:           42,
							QuoteID:          0,
							Qty:              3e6,
							Sell:             true,
							Filled:           3e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Refund:     400,
								Redemption: 500,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Revoked: true,
									Refund:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Revoked: true,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) + calc.BaseToQuote(5e7, 1e6) - 500,
						},
						42: {
							Available: (1e7 / 2) - 1e6 - 1600 - 400,
						},
					},
				},
			},
		},
		// "buy, 1 match refunded, 1 revoked before swap, 1 redeemed match, accountLocker"
		{
			name: "buy, 1 refunded, 1 revoked before swap, 1 redeemed match, accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     3e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       calc.BaseToQuote(5e7, 3e6) + 3000,
				RefundLockedAmt: 2400,
				Sell:            false,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 3e6) - 3000 - 2400,
					FundingOrder: calc.BaseToQuote(5e7, 3e6) + 3000 + 2400,
				},
				42: {
					Available: (1e7 / 2),
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      false,
							Filled:    2e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 3e6) - 3000 - 2400,
							FundingOrder: 3000 + 2400,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 3e6 - 3000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Swap:    &core.Coin{},
							Refund:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 2e6) - 3000 - 2400,
							FundingOrder: 3000 + 2400,
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 2e6 - 2000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Status:  order.NewlyMatched,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 2e6) - 3000 - 2400,
							FundingOrder: 3000 + 2400 + calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available:     (1e7 / 2),
							PendingRedeem: 1e6 - 1000,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[2][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:    (1e7 / 2) - calc.BaseToQuote(5e7, 2e6) - 3000 - 2400,
							FundingOrder: 3000 + 2400 + calc.BaseToQuote(5e7, 1e6),
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 1000,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusRevoked,
							BaseID:           42,
							QuoteID:          0,
							Qty:              3e6,
							Sell:             false,
							Filled:           3e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Refund:     400,
								Redemption: 500,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Revoked: true,
									Refund:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Revoked: true,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) - calc.BaseToQuote(5e7, 1e6) - 1600 - 400,
						},
						42: {
							Available: (1e7 / 2) + 1e6 - 500,
						},
					},
				},
			},
		},
		// "sell, 1 match refunded, 1 revoked before swap, 1 redeemed match, accountLocker"
		{
			name: "sell, 1 refunded, 1 revoked before swap, 1 redeemed match, accountLocker",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Percentage,
				BaseBalance:      50,
				QuoteBalanceType: Percentage,
				QuoteBalance:     50,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     3e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 1e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			refundFees: 800,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       3e6 + 3000,
				RefundLockedAmt: 2400,
				Sell:            false,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available: (1e7 / 2),
				},
				42: {
					Available:    (1e7 / 2) - 3e6 - 3000 - 2400,
					FundingOrder: 3e6 + 3000 + 2400,
				},
			},
			notifications: []*noteAndBalances{
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:        id,
							Status:    order.OrderStatusBooked,
							BaseID:    42,
							QuoteID:   0,
							Qty:       2e6,
							Sell:      true,
							Filled:    2e6,
							LockedAmt: 1e6,
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MakerSwapCast,
									Swap:    &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 3e6) - 3000,
						},
						42: {
							Available:    (1e7 / 2) - 3e6 - 3000 - 2400,
							FundingOrder: 3000 + 2400,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[0][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Swap:    &core.Coin{},
							Refund:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 2e6) - 2000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 2400,
							FundingOrder: 3000 + 2400,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[1][:],
							Qty:     1e6,
							Rate:    5e7,
							Revoked: true,
							Status:  order.NewlyMatched,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available:     (1e7 / 2),
							PendingRedeem: calc.BaseToQuote(5e7, 1e6) - 1000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 2400,
							FundingOrder: 3000 + 2400 + 1e6,
						},
					},
				},
				{
					note: &core.MatchNote{
						OrderID: id,
						Match: &core.Match{
							MatchID: matchIDs[2][:],
							Qty:     1e6,
							Rate:    5e7,
							Swap:    &core.Coin{},
							Redeem:  &core.Coin{},
							Status:  order.MatchConfirmed,
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) + calc.BaseToQuote(5e7, 1e6) - 1000,
						},
						42: {
							Available:    (1e7 / 2) - 2e6 - 3000 - 2400,
							FundingOrder: 3000 + 2400 + 1e6,
						},
					},
				},
				{
					note: &core.OrderNote{
						Order: &core.Order{
							ID:               id,
							Status:           order.OrderStatusRevoked,
							BaseID:           42,
							QuoteID:          0,
							Qty:              3e6,
							Sell:             true,
							Filled:           3e6,
							AllFeesConfirmed: true,
							FeesPaid: &core.FeeBreakdown{
								Swap:       1600,
								Refund:     400,
								Redemption: 500,
							},
							Matches: []*core.Match{
								{
									MatchID: matchIDs[0][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Revoked: true,
									Refund:  &core.Coin{},
								},
								{
									MatchID: matchIDs[1][:],
									Qty:     1e6,
									Rate:    5e7,
									Revoked: true,
									Status:  order.NewlyMatched,
								},
								{
									MatchID: matchIDs[2][:],
									Qty:     1e6,
									Rate:    5e7,
									Status:  order.MatchComplete,
									Swap:    &core.Coin{},
									Redeem:  &core.Coin{},
								},
							},
						},
					},
					balance: map[uint32]*botBalance{
						0: {
							Available: (1e7 / 2) + calc.BaseToQuote(5e7, 1e6) - 500,
						},
						42: {
							Available: (1e7 / 2) - 1e6 - 1600 - 400,
						},
					},
				},
			},
		},
		// "edge enough balance for single buy"
		{
			name: "edge enough balance for single buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + 1500,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     5e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			tradeRes: &core.Order{
				ID:        id,
				LockedAmt: calc.BaseToQuote(5e7, 5e6) + 1000,
				Sell:      false,
				FeesPaid: &core.FeeBreakdown{
					Funding: 400,
				},
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    100,
					FundingOrder: calc.BaseToQuote(5e7, 5e6) + 1000,
				},
				42: {
					Available: 5e6,
				},
			},
		},
		// "edge not enough balance for single buy, with maxFundingFee > 0"
		{
			name: "edge not enough balance for single buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + 1499,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    false,
				Qty:     5e6,
				Rate:    5e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			wantErr:        true,
		},
		// "edge enough balance for single sell"
		{
			name: "edge enough balance for single sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6 + 1500,
				QuoteBalanceType: Amount,
				QuoteBalance:     5e6,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     5e6,
				Rate:    1e8,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 0,
				Sell:            true,
				FeesPaid: &core.FeeBreakdown{
					Funding: 400,
				},
			},

			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available: 5e6,
				},
				42: {
					Available:    100,
					FundingOrder: 5e6 + 1000,
				},
			},
		},
		// "edge not enough balance for single sell"
		{
			name: "edge not enough balance for single sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6 + 1499,
				QuoteBalanceType: Amount,
				QuoteBalance:     5e6,
			},
			assetBalances: map[uint32]uint64{
				0:  1e7,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Sell:    true,
				Qty:     5e6,
				Rate:    1e8,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			wantErr:        true,
		},
		// "edge enough balance for single buy with redeem fees"
		{
			name: "edge enough balance for single buy with redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(52e7, 5e6) + 1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Qty:     5e6,
				Rate:    52e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       calc.BaseToQuote(52e7, 5e6) + 1000,
				RedeemLockedAmt: 1000,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    0,
					FundingOrder: calc.BaseToQuote(52e7, 5e6) + 1000,
				},
				42: {
					Available:    0,
					FundingOrder: 1000,
				},
			},
			isAccountLocker: map[uint32]bool{42: true},
		},
		// "edge not enough balance for single buy due to redeem fees"
		{
			name: "edge not enough balance for single buy due to redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      999,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(52e7, 5e6) + 1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Base:    42,
				Quote:   0,
				Qty:     5e6,
				Rate:    52e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:        1000,
			redeemFees:      1000,
			isAccountLocker: map[uint32]bool{42: true},
			wantErr:         true,
		},
		// "edge enough balance for single sell with redeem fees"
		{
			name: "edge enough balance for single sell with redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6 + 1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     1000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Sell:    true,
				Base:    42,
				Quote:   0,
				Qty:     5e6,
				Rate:    52e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			tradeRes: &core.Order{
				ID:              id,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 1000,
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    0,
					FundingOrder: 1000,
				},
				42: {
					Available:    0,
					FundingOrder: 5e6 + 1000,
				},
			},
			isAccountLocker: map[uint32]bool{0: true},
		},
		// "edge not enough balance for single buy due to redeem fees"
		{
			name: "edge not enough balance for single sell due to redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6 + 1000,
				QuoteBalanceType: Amount,
				QuoteBalance:     999,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			trade: &core.TradeForm{
				Host:    "host1",
				IsLimit: true,
				Sell:    true,
				Base:    42,
				Quote:   0,
				Qty:     5e6,
				Rate:    52e7,
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:        1000,
			redeemFees:      1000,
			isAccountLocker: map[uint32]bool{0: true},
			wantErr:         true,
		},
		// "edge enough balance for multi buy"
		{
			name: "edge enough balance for multi buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2500,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			multiTradeRes: []*core.Order{{
				ID:              id,
				LockedAmt:       calc.BaseToQuote(5e7, 5e6) + 1000,
				RedeemLockedAmt: 0,
				Sell:            true,
				FeesPaid: &core.FeeBreakdown{
					Funding: 400,
				},
			}, {
				ID:              id2,
				LockedAmt:       calc.BaseToQuote(52e7, 5e6) + 1000,
				RedeemLockedAmt: 0,
				Sell:            true,
			},
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    100,
					FundingOrder: calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2000,
				},
				42: {
					Available: 5e6,
				},
			},
		},
		// "edge not enough balance for multi buy"
		{
			name: "edge not enough balance for multi buy",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      5e6,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2499,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			wantErr:        true,
		},
		// "edge enough balance for multi sell"
		{
			name: "edge enough balance for multi sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e7 + 2500,
				QuoteBalanceType: Amount,
				QuoteBalance:     5e6,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e8,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Sell:  true,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			multiTradeRes: []*core.Order{{
				ID:              id,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 0,
				Sell:            true,
				FeesPaid: &core.FeeBreakdown{
					Funding: 400,
				},
			}, {
				ID:              id2,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 0,
				Sell:            true,
			},
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available: 5e6,
				},
				42: {
					Available:    100,
					FundingOrder: 1e7 + 2000,
				},
			},
		},
		// "edge not enough balance for multi sell"
		{
			name: "edge not enough balance for multi sell",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e7 + 2499,
				QuoteBalanceType: Amount,
				QuoteBalance:     5e6,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e8,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Sell:  true,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:       1000,
			redeemFees:     1000,
			maxFundingFees: 500,
			wantErr:        true,
		},
		// "edge enough balance for multi buy with redeem fees"
		{
			name: "edge enough balance for multi buy with redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      2000,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			multiTradeRes: []*core.Order{{
				ID:              id,
				LockedAmt:       calc.BaseToQuote(5e7, 5e6) + 1000,
				RedeemLockedAmt: 1000,
				Sell:            true,
			}, {
				ID:              id2,
				LockedAmt:       calc.BaseToQuote(52e7, 5e6) + 1000,
				RedeemLockedAmt: 1000,
				Sell:            true,
			},
			},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    0,
					FundingOrder: calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2000,
				},
				42: {
					Available:    0,
					FundingOrder: 2000,
				},
			},
			isAccountLocker: map[uint32]bool{42: true},
		},
		// "edge not enough balance for multi buy due to redeem fees"
		{
			name: "edge not enough balance for multi buy due to redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1999,
				QuoteBalanceType: Amount,
				QuoteBalance:     calc.BaseToQuote(5e7, 5e6) + calc.BaseToQuote(52e7, 5e6) + 2000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e7,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:        1000,
			redeemFees:      1000,
			wantErr:         true,
			isAccountLocker: map[uint32]bool{42: true},
		},
		// "edge enough balance for multi sell with redeem fees"
		{
			name: "edge enough balance for multi sell with redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e7 + 2000,
				QuoteBalanceType: Amount,
				QuoteBalance:     2000,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e8,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Sell:  true,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:   1000,
			redeemFees: 1000,
			multiTradeRes: []*core.Order{{
				ID:              id,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 1000,
				Sell:            true,
			}, {
				ID:              id2,
				LockedAmt:       5e6 + 1000,
				RedeemLockedAmt: 1000,
				Sell:            true,
			},
			},
			isAccountLocker: map[uint32]bool{0: true},
			postTradeBalances: map[uint32]*botBalance{
				0: {
					Available:    0,
					FundingOrder: 2000,
				},
				42: {
					Available:    0,
					FundingOrder: 1e7 + 2000,
				},
			},
		},
		// "edge not enough balance for multi sell due to redeem fees"
		{
			name: "edge enough balance for multi sell with redeem fees",
			cfg: &BotConfig{
				Host:             "host1",
				BaseAsset:        42,
				QuoteAsset:       0,
				BaseBalanceType:  Amount,
				BaseBalance:      1e7 + 2000,
				QuoteBalanceType: Amount,
				QuoteBalance:     1999,
			},
			assetBalances: map[uint32]uint64{
				0:  1e8,
				42: 1e8,
			},
			multiTradeOnly: true,
			multiTrade: &core.MultiTradeForm{
				Host:  "host1",
				Base:  42,
				Quote: 0,
				Sell:  true,
				Placements: []*core.QtyRate{
					{
						Qty:  5e6,
						Rate: 52e7,
					},
					{
						Qty:  5e6,
						Rate: 5e7,
					},
				},
			},
			market: &core.Market{
				LotSize: 5e6,
			},
			swapFees:        1000,
			redeemFees:      1000,
			isAccountLocker: map[uint32]bool{0: true},
			wantErr:         true,
		},
	}

	runTest := func(test *test) {
		if test.multiTradeOnly && !testMultiTrade {
			return
		}

		mktID := dexMarketID(test.cfg.Host, test.cfg.BaseAsset, test.cfg.QuoteAsset)

		tCore := newTCore()
		tCore.setAssetBalances(test.assetBalances)
		tCore.market = test.market
		var sell bool
		if test.multiTradeOnly {
			sell = test.multiTrade.Sell
		} else {
			sell = test.trade.Sell
		}

		if sell {
			tCore.sellSwapFees = test.swapFees
			tCore.sellRedeemFees = test.redeemFees
			tCore.sellRefundFees = test.refundFees
		} else {
			tCore.buySwapFees = test.swapFees
			tCore.buyRedeemFees = test.redeemFees
			tCore.buyRefundFees = test.refundFees
		}

		if test.isAccountLocker == nil {
			tCore.isAccountLocker = make(map[uint32]bool)
		} else {
			tCore.isAccountLocker = test.isAccountLocker
		}
		tCore.maxFundingFees = test.maxFundingFees

		if testMultiTrade {
			if test.multiTradeOnly {
				tCore.multiTradeResult = test.multiTradeRes
			} else {
				tCore.multiTradeResult = []*core.Order{test.tradeRes}
			}
		} else {
			tCore.tradeResult = test.tradeRes
		}
		tCore.noteFeed = make(chan core.Notification)

		mm, err := NewMarketMaker(tCore, tLogger)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}
		mm.doNotKillWhenBotsStop = true
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err = mm.Run(ctx, []*BotConfig{test.cfg}, []*CEXConfig{}, []byte{})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		segregatedCore := mm.wrappedCoreForBot(mktID)

		if testMultiTrade {

			if test.multiTradeOnly {
				_, err = segregatedCore.MultiTrade([]byte{}, test.multiTrade)
			} else {
				_, err = segregatedCore.MultiTrade([]byte{}, &core.MultiTradeForm{
					Host:  test.trade.Host,
					Sell:  test.trade.Sell,
					Base:  test.trade.Base,
					Quote: test.trade.Quote,
					Placements: []*core.QtyRate{
						{
							Qty:  test.trade.Qty,
							Rate: test.trade.Rate,
						},
					},
					Options: test.trade.Options,
				})
			}
		} else {
			_, err = segregatedCore.Trade([]byte{}, test.trade)
		}
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if err := assetBalancesMatch(test.postTradeBalances, mktID, mm); err != nil {
			t.Fatalf("%s: unexpected post trade balance: %v", test.name, err)
		}

		dummyNote := &core.BondRefundNote{}
		for i, noteAndBalances := range test.notifications {
			tCore.noteFeed <- noteAndBalances.note
			tCore.noteFeed <- dummyNote

			if err := assetBalancesMatch(noteAndBalances.balance, mktID, mm); err != nil {
				t.Fatalf("%s: unexpected balances after note %d: %v", test.name, i, err)
			}
		}
	}

	for _, test := range tests {
		runTest(&test)
	}
}
