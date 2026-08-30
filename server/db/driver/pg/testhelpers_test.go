package pg

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"github.com/decred/slog"
)

func startLogger() {
	logger := slog.NewBackend(os.Stdout).Logger("PG_DB_TEST")
	logger.SetLevel(slog.LevelDebug)
	UseLogger(logger)
}

const (
	LotSize         = uint64(100_0000_0000) // 100
	RateStep        = uint64(10_0000)       // 0.001
	EpochDuration   = uint64(10_000)
	MarketBuyBuffer = 1.1
)

// The asset integer IDs should set in TestMain or other bring up function (e.g.
// openDB()) prior to using them.
var (
	AssetDCR uint32
	AssetBTC uint32
	AssetLTC uint32
)

func assertNoArchivedCommitUnique(db *sql.DB) error {
	const q = `
		SELECT n.nspname || '.' || con.conname
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = rel.relnamespace
		WHERE con.contype = 'u'
		  AND rel.relname IN ('orders_archived', 'cancels_archived')
		  AND (con.conname LIKE '%_commit_key' OR con.conname LIKE '%_preimage_key')`
	rows, err := db.Query(q)
	if err != nil {
		return fmt.Errorf("query archived unique constraints: %w", err)
	}
	defer rows.Close()
	var leftover []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		leftover = append(leftover, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(leftover) > 0 {
		return fmt.Errorf("archived unique constraints still present: %v", leftover)
	}
	return nil
}

func assertArchivedCommitIndexes(db *sql.DB) error {
	mkts, err := loadMarkets(db, marketsTableName)
	if err != nil {
		return fmt.Errorf("load markets: %w", err)
	}
	const q = `
		SELECT 1 FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2 AND indexname = $3`
	for _, mkt := range mkts {
		schema := marketSchema(mkt.Name)
		for _, idx := range []struct{ table, name string }{
			{ordersArchivedTableName, indexArchivedOrdersCommitName},
			{cancelsArchivedTableName, indexArchivedCancelsCommitName},
		} {
			var one int
			err := db.QueryRow(q, schema, idx.table, idx.name).Scan(&one)
			if err != nil {
				return fmt.Errorf("archived commit index %s.%s on %s: %w",
					schema, idx.name, idx.table, err)
			}
		}
	}
	return nil
}

func randomBytes(len int) []byte {
	bytes := make([]byte, len)
	rand.Read(bytes)
	return bytes
}

func randomAccountID() account.AccountID {
	pk := randomBytes(account.PubKeySize) // size is not important since it is going to be hashed
	return account.NewID(pk)
}

func randomPreimage() (pi order.Preimage) {
	rand.Read(pi[:])
	return
}

func randomCommitment() (com order.Commitment) {
	rand.Read(com[:])
	return
}

func mktConfig() (markets []*dex.MarketInfo) {
	mktConfig, err := dex.NewMarketInfoFromSymbols("DCR", "BTC", LotSize, RateStep, EpochDuration, 0, MarketBuyBuffer)
	if err != nil {
		panic(fmt.Sprintf("you broke it: %v", err))
	}
	markets = append(markets, mktConfig)

	mktConfig, err = dex.NewMarketInfoFromSymbols("BTC", "LTC", LotSize, RateStep, EpochDuration, 0, MarketBuyBuffer)
	if err != nil {
		panic(fmt.Sprintf("you broke it: %v", err))
	}
	markets = append(markets, mktConfig)

	// specify more here...
	return
}

// Used in online test.
func newMatch(maker *order.LimitOrder, taker order.Order, quantity uint64, epochID order.EpochID) *order.Match {
	return &order.Match{
		Maker:        maker,
		Taker:        taker,
		Quantity:     quantity,
		Rate:         maker.Rate,
		FeeRateBase:  12,
		FeeRateQuote: 14,
		Status:       order.NewlyMatched,
		Sigs:         order.Signatures{},
		Epoch:        epochID,
	}
}

// Used in online test.
func newLimitOrderRevealed(sell bool, rate, quantityLots uint64, force order.TimeInForce, timeOffset int64) (*order.LimitOrder, order.Preimage) {
	lo := newLimitOrder(sell, rate, quantityLots, force, timeOffset)
	pi := randomPreimage()
	lo.Commit = pi.Commit()
	return lo, pi
}

func newLimitOrder(sell bool, rate, quantityLots uint64, force order.TimeInForce, timeOffset int64) *order.LimitOrder {
	return newLimitOrderWithAssets(sell, rate, quantityLots, force, timeOffset, AssetDCR, AssetBTC)
}

func newLimitOrderWithAssets(sell bool, rate, quantityLots uint64, force order.TimeInForce, timeOffset int64, base, quote uint32) *order.LimitOrder {
	addr := "DcqXswjTPnUcd4FRCkX4vRJxmVtfgGVa5ui"
	if sell {
		addr = "149RQGLaHf2gGiL4NXZdH7aA8nYEuLLrgm"
	}
	return &order.LimitOrder{
		P: order.Prefix{
			AccountID:  randomAccountID(),
			BaseAsset:  base,
			QuoteAsset: quote,
			OrderType:  order.LimitOrderType,
			ClientTime: time.Unix(1566497653+timeOffset, 0).UTC(),
			ServerTime: time.Unix(1566497656+timeOffset, 0).UTC(),
			Commit:     randomCommitment(),
		},
		T: order.Trade{
			Coins: []order.CoinID{
				randomBytes(36),
				randomBytes(36),
			},
			Sell:     sell,
			Quantity: quantityLots * LotSize,
			Address:  addr,
		},
		Rate:  rate,
		Force: force,
	}
}

// Used in online test.
func newMarketSellOrder(quantityLots uint64, timeOffset int64) *order.MarketOrder {
	return &order.MarketOrder{
		P: order.Prefix{
			AccountID:  randomAccountID(),
			BaseAsset:  AssetDCR,
			QuoteAsset: AssetBTC,
			OrderType:  order.MarketOrderType,
			ClientTime: time.Unix(1566497653+timeOffset, 0).UTC(),
			ServerTime: time.Unix(1566497656+timeOffset, 0).UTC(),
			Commit:     randomCommitment(),
		},
		T: order.Trade{
			Coins:    []order.CoinID{randomBytes(36)},
			Sell:     true,
			Quantity: quantityLots * LotSize,
			Address:  "149RQGLaHf2gGiL4NXZdH7aA8nYEuLLrgm",
		},
	}
}

// Used in online test.
func newMarketBuyOrder(quantityQuoteAsset uint64, timeOffset int64) *order.MarketOrder {
	return &order.MarketOrder{
		P: order.Prefix{
			AccountID:  randomAccountID(),
			BaseAsset:  AssetDCR,
			QuoteAsset: AssetBTC,
			OrderType:  order.MarketOrderType,
			ClientTime: time.Unix(1566497653+timeOffset, 0).UTC(),
			ServerTime: time.Unix(1566497656+timeOffset, 0).UTC(),
			Commit:     randomCommitment(),
		},
		T: order.Trade{
			Coins:    []order.CoinID{randomBytes(36)},
			Sell:     false,
			Quantity: quantityQuoteAsset,
			Address:  "DcqXswjTPnUcd4FRCkX4vRJxmVtfgGVa5ui",
		},
	}
}

// Used in online test.
func newCancelOrder(targetOrderID order.OrderID, base, quote uint32, timeOffset int64) *order.CancelOrder {
	return &order.CancelOrder{
		P: order.Prefix{
			AccountID:  randomAccountID(),
			BaseAsset:  base,
			QuoteAsset: quote,
			OrderType:  order.CancelOrderType,
			ClientTime: time.Unix(1566497653+timeOffset, 0).UTC(),
			ServerTime: time.Unix(1566497656+timeOffset, 0).UTC(),
			Commit:     randomCommitment(),
		},
		TargetOrderID: targetOrderID,
	}
}
