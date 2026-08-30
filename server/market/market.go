// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/ws"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/auth"
	"decred.org/dcrdex/server/book"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/matcher"
)

// Error is just a basic error.
type Error string

// Error satisfies the error interface.
func (e Error) Error() string {
	return string(e)
}

const (
	ErrMarketNotRunning       = Error("market not running")
	ErrInvalidOrder           = Error("order failed validation")
	ErrInvalidRate            = Error("limit order rate too low")
	ErrInvalidCommitment      = Error("order commitment invalid")
	ErrEpochMissed            = Error("order unexpectedly missed its intended epoch")
	ErrDuplicateOrder         = Error("order already in epoch") // maybe remove since this is ill defined
	ErrQuantityTooHigh        = Error("order quantity exceeds user limit")
	ErrDuplicateCancelOrder   = Error("equivalent cancel order already in epoch")
	ErrTooManyCancelOrders    = Error("too many cancel orders in current epoch")
	ErrCancelNotPermitted     = Error("cancel order account does not match targeted order account")
	ErrTargetNotActive        = Error("target order not active on this market")
	ErrTargetNotCancelable    = Error("targeted order is not a limit order with standing time-in-force")
	ErrSuspendedAccount       = Error("suspended account")
	ErrMalformedOrderResponse = Error("malformed order response")
	ErrInternalServer         = Error("internal server error")
)

// Swapper coordinates atomic swaps for one or more matchsets.
type Swapper interface {
	Negotiate(matchSets []*order.MatchSet)
	CheckUnspent(ctx context.Context, asset uint32, coinID []byte) error
	ChainsSynced(base, quote uint32) (bool, error)
}

type DataCollector interface {
	ReportEpoch(base, quote uint32, epochIdx uint64, stats *matcher.MatchCycleStats) (*msgjson.Spot, error)
}

// FeeFetcher is a fee fetcher for fetching fees. Fees are fickle, so fetch fees
// with FeeFetcher fairly frequently.
type FeeFetcher interface {
	FeeRate(context.Context) uint64
	SwapFeeRate(context.Context) uint64
	LastRate() uint64
	MaxFeeRate() uint64
}

// Balancer provides a method to check that an account on an account-based
// asset has sufficient balance.
type Balancer interface {
	// CheckBalance checks that the address's account has sufficient balance to
	// trade the outgoing number of lots (totaling qty) and incoming number of
	// redeems.
	CheckBalance(acctAddr string, assetID, redeemAssetID uint32, qty, lots uint64, redeems int) bool
}

// Config is the Market configuration.
type Config struct {
	MarketInfo       *dex.MarketInfo
	Storage          Storage
	Swapper          Swapper
	AuthManager      AuthManager
	FeeFetcherBase   FeeFetcher
	CoinLockerBase   coinlock.CoinLocker
	FeeFetcherQuote  FeeFetcher
	CoinLockerQuote  coinlock.CoinLocker
	DataCollector    DataCollector
	Balancer         Balancer
	CheckParcelLimit func(user account.AccountID, calcParcels MarketParcelCalculator) bool
	MinimumRate      uint64
}

// Market is the market manager. It should not be overly involved with details
// of accounts and authentication. Via the account package it should request
// account status with new orders, verification of order signatures. The Market
// should also perform various account package callbacks such as order status
// updates so that the account package code can keep various data up-to-date,
// including order status, history, cancellation statistics, etc.
//
// The Market performs the following:
//  1. Receive and validate new order data (amounts vs. lot size, check fees,
//     utxos, sufficient market buy buffer, etc.).
//  2. Put incoming orders into the current epoch queue.
//  3. Maintain an order book, which must also implement matcher.Booker.
//  4. Initiate order matching with matcher.Match(book, currentQueue)
//  5. During and/or after matching:
//     * update the book (remove orders, add new standing orders, etc.)
//     * retire/archive the epoch queue
//     * publish the matches (and order book changes?)
//     * initiate swaps for each match (possibly groups of related matches)
//  6. Cycle the epochs.
//  7. Record all events with the archivist.
type Market struct {
	marketInfo *dex.MarketInfo

	tasks sync.WaitGroup // for lazy asynchronous tasks e.g. revoke ntfns

	// Communications.
	orderRouter chan *orderUpdateSignal // incoming orders, via SubmitOrderAsync

	orderFeedMtx sync.RWMutex         // guards orderFeeds and running
	orderFeeds   []chan *updateSignal // all outgoing notification consumers

	runMtx  sync.RWMutex
	running chan struct{} // closed when running (accepting new orders)
	up      uint32        // Run is called, either waiting for first epoch or running

	bookMtx      sync.Mutex // guards book and bookEpochIdx
	book         *book.Book
	bookEpochIdx int64 // next epoch from the point of view of the book
	settling     map[order.OrderID]uint64

	epochMtx         sync.RWMutex
	startEpochIdx    int64
	activeEpochIdx   int64
	suspendEpochIdx  int64
	persistBook      bool
	epochCommitments map[order.Commitment]order.OrderID
	epochOrders      map[order.OrderID]order.Order

	matcher *matcher.Matcher
	swapper Swapper
	auth    AuthManager

	feeScalesMtx sync.RWMutex
	feeScales    struct {
		base  float64
		quote float64
	}

	coinLockerBase  coinlock.CoinLocker
	coinLockerQuote coinlock.CoinLocker

	baseFeeFetcher  FeeFetcher
	quoteFeeFetcher FeeFetcher

	// Persistent data storage
	storage Storage

	// Data API
	dataCollector DataCollector
	lastRate      uint64

	checkParcelLimit func(user account.AccountID, calcParcels MarketParcelCalculator) bool

	minimumRate uint64

	mmSnapshotMtx  sync.RWMutex
	mmSnapshotSubs map[account.AccountID]struct{}
}

// Storage is the DB interface required by Market.
type Storage interface {
	db.OrderArchiver
	LastErr() error
	Fatal() <-chan struct{}
	Close() error
	InsertEpoch(ed *db.EpochResults) error
	LastEpochRate(base, quote uint32) (uint64, error)
	MarketMatches(base, quote uint32) ([]*db.MatchDataWithCoins, error)
	InsertMatch(match *order.Match) error
}

// NewMarket creates a new Market for the provided base and quote assets, with
// an epoch cycling at given duration in milliseconds.
func NewMarket(cfg *Config) (*Market, error) {
	// Make sure the DEXArchivist is healthy before taking orders.
	storage, mktInfo, swapper := cfg.Storage, cfg.MarketInfo, cfg.Swapper
	if err := storage.LastErr(); err != nil {
		return nil, err
	}

	// Load existing book orders from the DB.
	base, quote := mktInfo.Base, mktInfo.Quote

	bookOrders, err := storage.BookOrders(base, quote)
	if err != nil {
		return nil, err
	}
	log.Infof("Loaded %d stored book orders.", len(bookOrders))

	baseIsAcctBased := cfg.CoinLockerBase == nil
	quoteIsAcctBased := cfg.CoinLockerQuote == nil

	// Put the book orders in a map so orders that no longer have funding coins
	// can be removed easily.
	bookOrdersByID := make(map[order.OrderID]*order.LimitOrder, len(bookOrders))
	for _, lo := range bookOrders {
		// Limit order amount requirements are simple unlike market buys.
		if lo.Quantity%mktInfo.LotSize != 0 || lo.FillAmt%mktInfo.LotSize != 0 {
			// To change market configuration, the operator should suspended the
			// market with persist=false, but that may not have happened, or
			// maybe a revoke failed.
			log.Errorf("Not rebooking order %v with amount (%v/%v) incompatible with current lot size (%v)",
				lo.ID(), lo.FillAmt, lo.Quantity, mktInfo.LotSize)
			// Revoke the order, but do not count this against the user.
			if _, _, err = storage.RevokeOrderUncounted(lo); err != nil {
				log.Errorf("Failed to revoke order %v: %v", lo, err)
				// But still not added back on the book.
			}
			continue
		}
		bookOrdersByID[lo.ID()] = lo
	}

	// "execute" any epoch orders in DB that may be left over from unclean
	// shutdown. Whatever epoch they were in will not be seen again.
	epochOrders, err := storage.EpochOrders(base, quote)
	if err != nil {
		return nil, err
	}
	for _, ord := range epochOrders {
		oid := ord.ID()
		log.Infof("Dropping old epoch order %v", oid)
		if co, ok := ord.(*order.CancelOrder); ok {
			if err := storage.FailCancelOrder(co); err != nil {
				log.Errorf("Failed to set orphaned epoch cancel order %v as executed: %v", oid, err)
			}
			continue
		}
		if err := storage.ExecuteOrder(ord); err != nil {
			log.Errorf("Failed to set orphaned epoch trade order %v as executed: %v", oid, err)
		}
	}

	// Set up tracking. Which of these are actually used depend on whether the
	// assets are account- or utxo-based.
	// utxo-based
	var baseCoins, quoteCoins map[order.OrderID][]order.CoinID
	var missingCoinFails map[order.OrderID]struct{}
	// account-based
	var quoteAcctStats, baseAcctStats accountCounter
	var failedBaseAccts, failedQuoteAccts map[string]bool
	var failedAcctOrders map[order.OrderID]struct{}
	var acctTracking book.AccountTracking

	if baseIsAcctBased {
		acctTracking |= book.AccountTrackingBase
		baseAcctStats = make(accountCounter)
		failedBaseAccts = make(map[string]bool)
		failedAcctOrders = make(map[order.OrderID]struct{})
	} else {
		baseCoins = make(map[order.OrderID][]order.CoinID)
		missingCoinFails = make(map[order.OrderID]struct{})
	}

	if quoteIsAcctBased {
		acctTracking |= book.AccountTrackingQuote
		quoteAcctStats = make(accountCounter)
		failedQuoteAccts = make(map[string]bool)
		if failedAcctOrders == nil {
			failedAcctOrders = make(map[order.OrderID]struct{})
		}
	} else {
		quoteCoins = make(map[order.OrderID][]order.CoinID)
		if missingCoinFails == nil {
			missingCoinFails = make(map[order.OrderID]struct{})
		}
	}

ordersLoop:
	for id, lo := range bookOrdersByID {
		if lo.FillAmt > 0 {
			// Order already matched with another trade, so it is expected that
			// the funding coins are spent in a swap.
			//
			// In general, our position is that the server is not ultimately
			// responsible for verifying that all orders have locked coins since
			// the client will be penalized if they cannot complete the swap.
			// The least the server can do is ensure funding coins for NEW
			// orders are unspent and owned by the user.

			// On to the next order. Do not lock coins that are spent or should
			// be spent in a swap contract.
			continue
		}

		// Verify all funding coins for this order.
		assetID := quote
		if lo.Sell {
			assetID = base
		}
		for i := range lo.Coins {
			err = swapper.CheckUnspent(context.Background(), assetID, lo.Coins[i]) // no timeout
			if err == nil {
				continue
			}

			if errors.Is(err, asset.CoinNotFoundError) {
				// spent, exclude this order
				log.Warnf("Coin %s not unspent for unfilled order %v. "+
					"Revoking the order.", fmtCoinID(assetID, lo.Coins[i]), lo)
			} else {
				// other failure (coinID decode, RPC, etc.)
				return nil, fmt.Errorf("unexpected error checking coinID %v for order %v: %w",
					lo.Coins[i], lo, err)
				// NOTE: This does not revoke orders from storage since this is
				// likely to be a configuration or node issue.
			}

			delete(bookOrdersByID, id)
			// Revoke the order, but do not count this against the user.
			if _, _, err = storage.RevokeOrderUncounted(lo); err != nil {
				log.Errorf("Failed to revoke order %v: %v", lo, err)
			}
			// No penalization here presently since the market was down, but if
			// a suspend message with persist=true was sent, the users should
			// have kept their coins locked. (TODO)
			continue ordersLoop
		}

		if baseIsAcctBased {
			var addr string
			var qty, lots uint64
			var redeems int
			if lo.Sell {
				// address is zeroth coin
				if len(lo.Coins) != 1 {
					log.Errorf("rejecting account-based-base-asset order %s that has no coins ¯\\_(ツ)_/¯", lo.ID())
					continue ordersLoop
				}
				addr = string(lo.Coins[0])
				qty = lo.Quantity
				lots = qty / mktInfo.LotSize
			} else {
				addr = lo.Address
				redeems = int((lo.Quantity - lo.FillAmt) / mktInfo.LotSize)
			}
			baseAcctStats.add(addr, qty, lots, redeems)
		} else if lo.Sell {
			baseCoins[id] = lo.Coins
		}

		if quoteIsAcctBased {
			var addr string
			var qty, lots uint64
			var redeems int
			if lo.Sell { // sell base => redeem acct-based quote
				addr = lo.Address
				redeems = int((lo.Quantity - lo.FillAmt) / mktInfo.LotSize)
			} else { // buy base => offer acct-based quote
				// address is zeroth coin
				if len(lo.Coins) != 1 {
					log.Errorf("rejecting account-based-base-asset order %s that has no coins ¯\\_(ツ)_/¯", lo.ID())
					continue ordersLoop
				}
				addr = string(lo.Coins[0])
				lots = lo.Quantity / mktInfo.LotSize
				qty = calc.BaseToQuote(lo.Rate, lo.Quantity)
			}
			quoteAcctStats.add(addr, qty, lots, redeems)
		} else if !lo.Sell {
			quoteCoins[id] = lo.Coins
		}
	}

	if baseIsAcctBased {
		log.Debugf("Checking %d base asset (%d) balances.", len(baseAcctStats), base)
		for acctAddr, stats := range baseAcctStats {
			if !cfg.Balancer.CheckBalance(acctAddr, mktInfo.Base, mktInfo.Quote, stats.qty, stats.lots, stats.redeems) {
				log.Info("%s base asset account failed the startup balance check on the %s market", acctAddr, mktInfo.Name)
				failedBaseAccts[acctAddr] = true
			}
		}
	} else {
		log.Debugf("Locking %d base asset (%d) coins.", len(baseCoins), base)
		if log.Level() <= dex.LevelTrace {
			for oid, coins := range baseCoins {
				log.Tracef(" - order %v: %v", oid, coins)
			}
		}
		for oid := range cfg.CoinLockerBase.LockCoins(baseCoins) {
			missingCoinFails[oid] = struct{}{}
		}
	}

	if quoteIsAcctBased {
		log.Debugf("Checking %d quote asset (%d) balances.", len(quoteAcctStats), quote)
		for acctAddr, stats := range quoteAcctStats { // quoteAcctStats is nil for utxo-based quote assets
			if !cfg.Balancer.CheckBalance(acctAddr, mktInfo.Quote, mktInfo.Base, stats.qty, stats.lots, stats.redeems) {
				log.Errorf("%s quote asset account failed the startup balance check on the %s market", acctAddr, mktInfo.Name)
				failedQuoteAccts[acctAddr] = true
			}
		}
	} else {
		log.Debugf("Locking %d quote asset (%d) coins.", len(quoteCoins), quote)
		if log.Level() <= dex.LevelTrace {
			for oid, coins := range quoteCoins {
				log.Tracef(" - order %v: %v", oid, coins)
			}
		}
		for oid := range cfg.CoinLockerQuote.LockCoins(quoteCoins) {
			missingCoinFails[oid] = struct{}{}
		}
	}

	for oid := range missingCoinFails {
		log.Warnf("Revoking book order %v with already locked coins.", oid)
		bad := bookOrdersByID[oid]
		delete(bookOrdersByID, oid)
		// Revoke the order, but do not count this against the user.
		if _, _, err = storage.RevokeOrderUncounted(bad); err != nil {
			log.Errorf("Failed to revoke order %v: %v", bad, err)
			// But still not added back on the book.
		}
	}

	Book := book.New(mktInfo.LotSize, acctTracking)
	for _, lo := range bookOrdersByID {
		// Catch account-based asset low-balance rejections here.
		if baseIsAcctBased && failedBaseAccts[lo.BaseAccount()] {
			failedAcctOrders[lo.ID()] = struct{}{}
			log.Warnf("Skipping insert of order %s into %s book because base asset "+
				"account failed the balance check", lo.ID(), mktInfo.Name)
			continue
		}
		if quoteIsAcctBased && failedQuoteAccts[lo.QuoteAccount()] {
			failedAcctOrders[lo.ID()] = struct{}{}
			log.Warnf("Skipping insert of order %s into %s book because quote asset "+
				"account failed the balance check", lo.ID(), mktInfo.Name)
			continue
		}
		if ok := Book.Insert(lo); !ok {
			// This can only happen if one of the loaded orders has an
			// incompatible lot size for the current market config, which was
			// already checked above.
			log.Errorf("Failed to insert order %v into %v book.", mktInfo.Name, lo)
		}
	}

	// Revoke the low-balance rejections in the database.
	for oid := range failedAcctOrders {
		// Already logged in the Book.Insert loop.
		if _, _, err = storage.RevokeOrderUncounted(bookOrdersByID[oid]); err != nil {
			log.Errorf("Failed to revoke order with insufficient account balance %v: %v", bookOrdersByID[oid], err)
		}
	}

	// Populate the order settling amount map from the active matches in DB.
	activeMatches, err := storage.MarketMatches(base, quote)
	if err != nil {
		return nil, fmt.Errorf("failed to load active matches for market %v: %w", mktInfo.Name, err)
	}
	settling := make(map[order.OrderID]uint64)
	for _, match := range activeMatches {
		settling[match.Taker] += match.Quantity
		settling[match.Maker] += match.Quantity
		// Note: we actually don't want to bother with matches for orders that
		// were canceled or had at-fault match failures, since including them
		// give that user another shot to get a successfully "completed" order
		// if they complete these remaining matches, but it's OK. We'd have to
		// query these order statuses, and look for at-fault match failures
		// involving them, so just give the user the benefit of the doubt.
	}
	log.Infof("Tracking %d orders with %d active matches.", len(m.settling), len(activeMatches))

	lastEpochEndRate, err := storage.LastEpochRate(base, quote)
	if err != nil {
		return fmt.Errorf("failed to load last epoch end rate: %w", err)
	}
	m.lastRate = lastEpochEndRate

	// Not just reads: this acquires coin locks in the shared lockers.
	if err := restoreStartupBookCoinLocks(m.name, insertedBookOrders, m.coinLockerBase, m.coinLockerQuote); err != nil {
		return err
	}

	return m.seedEpochMemory(lifecycleRow)
}

// seedEpochMemory rebuilds a running market's in-memory epoch state from
// durable storage.
func (m *Market) seedEpochMemory(lc *db.MarketLifecycle) error {
	if lc == nil {
		return nil
	}
	switch lc.State {
	case db.MarketStateRunning:
	case db.MarketStateSuspended:
		return m.verifyNoStoredEpochOrders()
	default: // never started
		return nil
	}
	epochOrders, err := m.storage.EpochOrders(m.base, m.quote)
	if err != nil {
		return fmt.Errorf("load epoch orders for %s: %w", m.name, err)
	}
	sortOrdersByID(epochOrders)

	if lc.PendingAction == db.MarketPendingSuspendDrain {
		// No epoch to seed while draining; lock the leftover orders' coins.
		return m.lockEpochOrderCoins(epochOrders)
	}

	if err := m.validateEpochSeed(lc, epochOrders); err != nil {
		return err
	}
	if err := m.lockEpochOrderCoins(epochOrders); err != nil {
		return err
	}
	m.seedEpochQueues(lc.ActiveEpochIdx, lc.StartEpochDur, epochOrders)
	return nil
}

// verifyNoStoredEpochOrders checks that a suspended market holds no
// epoch-status orders: the suspend transition requires the final epoch
// processed, so none can exist.
func (m *Market) verifyNoStoredEpochOrders() error {
	ords, err := m.storage.EpochOrders(m.base, m.quote)
	if err != nil {
		return fmt.Errorf("load epoch orders for %s: %w", m.name, err)
	}
	if len(ords) > 0 {
		return fmt.Errorf("suspended market %s has %d epoch-status orders; storage is inconsistent",
			m.name, len(ords))
	}
	return nil
}

// validateEpochSeed rejects storage states the seeding cannot faithfully
// project: a missing cursor, or an order stamped beyond the next epoch
// window. A configured duration that disagrees with the row is a warning;
// it takes effect at the next market_started this node masters.
func (m *Market) validateEpochSeed(lc *db.MarketLifecycle, epochOrders []order.Order) error {
	name := m.name
	active, epochDur := lc.ActiveEpochIdx, lc.StartEpochDur
	if epochDur != m.configuredParams.epochDur {
		log.Warnf("Market %s runs with log-pinned epoch duration %d; configured duration %d "+
			"takes effect at the next market start.", name, epochDur, m.configuredParams.epochDur)
	}
	if active <= 0 {
		return fmt.Errorf("running market %s has no active epoch cursor; "+
			"this node's DB predates the cursor column — re-initialize it or re-seed from a mesh peer", name)
	}
	nextEnd := (active + 2) * epochDur
	for _, ord := range epochOrders {
		if ord.Time() >= nextEnd {
			return fmt.Errorf("market %s epoch order %v stamped %d beyond the next epoch window of cursor %d",
				name, ord.ID(), ord.Time(), active)
		}
	}
	return nil
}

// seedEpochQueues projects the current and next epoch queues from the stored
// epoch orders.
func (m *Market) seedEpochQueues(active, epochDur int64, epochOrders []order.Order) {
	currentOrders := ordersInEpoch(epochOrders, active, epochDur)
	nextOrders := ordersInEpoch(epochOrders, active+1, epochDur)
	m.epochMtx.Lock()
	m.currentEpoch = NewEpoch(active, epochDur)
	m.nextEpoch = NewEpoch(active+1, epochDur)
	m.activeEpochIdx = active
	for _, ord := range currentOrders {
		m.insertEpochOrderLocked(m.currentEpoch, ord)
	}
	for _, ord := range nextOrders {
		m.insertEpochOrderLocked(m.nextEpoch, ord)
	}
	m.epochMtx.Unlock()

	m.bookMtx.Lock()
	if active > m.bookEpochIdx {
		m.bookEpochIdx = active
	}
	m.bookMtx.Unlock()
	log.Infof("Seeded market %s epoch memory at epoch %d: %d current, %d next, %d awaiting processing.",
		m.name, active, len(currentOrders), len(nextOrders),
		len(epochOrders)-len(currentOrders)-len(nextOrders))
}

func restoreStartupBookCoinLocks(marketName string, bookOrders []*order.LimitOrder, baseLocker, quoteLocker coinlock.CoinLocker) error {
	candidates := make([]*order.LimitOrder, 0, len(bookOrders))
	for _, lo := range bookOrders {
		if lo.Sell {
			if baseLocker != nil {
				candidates = append(candidates, lo)
			}
			continue
		}
		if quoteLocker != nil {
			candidates = append(candidates, lo)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		idi := candidates[i].ID()
		idj := candidates[j].ID()
		return bytes.Compare(idi[:], idj[:]) < 0
	})

	var lockedBase, lockedQuote []order.OrderID
	rollback := func() {
		if baseLocker != nil {
			baseLocker.UnlockOrdersCoins(lockedBase)
		}
		if quoteLocker != nil {
			quoteLocker.UnlockOrdersCoins(lockedQuote)
		}
	}

	for _, lo := range candidates {
		oid := lo.ID()
		locker := quoteLocker
		locked := &lockedQuote
		if lo.Sell {
			locker = baseLocker
			locked = &lockedBase
		}
		if failed := locker.LockCoins(map[order.OrderID][]order.CoinID{
			oid: lo.Coins,
		}); len(failed) > 0 {
			rollback()
			return fmt.Errorf("failed to restore startup book coin locks for market %s order %v", marketName, oid)
		}
		*locked = append(*locked, oid)
	}
	return nil
}

// SetMeshService configures the mesh service. It must be set before the comms
// routes serve traffic.
func (m *Market) SetMeshService(mesh MeshService) {
	m.mesh = mesh
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	cpy := *v
	return &cpy
}

func cloneRunParams(p *meshevents.MarketRunParams) *meshevents.MarketRunParams {
	if p == nil {
		return nil
	}
	cpy := *p
	return &cpy
}

func (m *Market) wakeLifecycleDriver() {
	select {
	case m.lifecycleWake <- struct{}{}:
	default:
	}
}

// wakeClosureWaiter signals the epoch advancer that the closure watermark
// moved (an epoch close applied, or a lifecycle event re-baselined it).
func (m *Market) wakeClosureWaiter() {
	select {
	case m.closureWake <- struct{}{}:
	default:
	}
}

// projectMarketLifecycleLocked copies durable lifecycle fields into the market.
// Caller must hold epochMtx.
func (m *Market) projectMarketLifecycleLocked(lc *db.MarketLifecycle) {
	m.liveParams.Store(&marketRun{MarketRunParams: lc.RunParams, epochDur: lc.StartEpochDur})
	m.lifecycleState = lc.State
	m.startEpochIdx = lc.StartEpochIdx
	m.suspendEpochIdx = lc.FinalEpochIdx
	m.pendingLifecycleAction = lc.PendingAction
	m.pendingLifecycleEpochIdx = lc.PendingEpochIdx
	m.pendingLifecycleEpochDur = lc.PendingEpochDur
	m.processedEpochIdx = lc.ProcessedEpochIdx
	m.persistBookSet = lc.PersistBook != nil
	if lc.PersistBook != nil {
		m.persistBook = *lc.PersistBook
	}
	if lc.State == db.MarketStateSuspended || lc.PendingAction == db.MarketPendingSuspendDrain {
		m.activeEpochIdx = 0
		m.currentEpoch = nil
		m.nextEpoch = nil
	}
	m.wakeClosureWaiter()
}

// restoreMarketLifecycle applies a durable lifecycle snapshot at construction
// or other startup paths. It does not open order admission, seed live epochs,
// or rebind the book; LoadState follows it by building the book and calling
// seedEpochMemory for running markets.
func (m *Market) restoreMarketLifecycle(lc *db.MarketLifecycle) {
	if lc == nil {
		return
	}
	m.epochMtx.Lock()
	m.projectMarketLifecycleLocked(lc)
	m.epochMtx.Unlock()
}

// applyMarketLifecycleRow applies a durable lifecycle row on a live market,
// seeding epochs and order admission when the market is running.
func (m *Market) applyMarketLifecycleRow(lc *db.MarketLifecycle) error {
	if lc == nil {
		return nil
	}
	if err := m.book.SetLotSize(lc.RunParams.LotSize); err != nil {
		return err
	}
	m.epochMtx.Lock()
	m.projectMarketLifecycleLocked(lc)
	if lc.State == db.MarketStateRunning && lc.PendingAction == db.MarketPendingNone && m.currentEpoch == nil {
		m.currentEpoch = NewEpoch(lc.ActiveEpochIdx, lc.StartEpochDur)
		m.nextEpoch = NewEpoch(lc.ActiveEpochIdx+1, lc.StartEpochDur)
		m.activeEpochIdx = lc.ActiveEpochIdx
	}
	acceptOrders := lc.State == db.MarketStateRunning &&
		lc.PendingAction != db.MarketPendingSuspendDrain && m.currentEpoch != nil
	m.epochMtx.Unlock()

	m.running.Store(acceptOrders)
	m.wakeLifecycleDriver()
	return nil
}

func (m *Market) lifecyclePendingSuspendDrain(epochIdx, epochDur int64) bool {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	return m.lifecycleState == db.MarketStateRunning &&
		m.pendingLifecycleAction == db.MarketPendingSuspendDrain &&
		m.pendingLifecycleEpochIdx == epochIdx &&
		m.pendingLifecycleEpochDur == epochDur
}

func (m *Market) lifecycleFinalizingSuspend() bool {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	return m.lifecycleState == db.MarketStateRunning &&
		m.pendingLifecycleAction == db.MarketPendingSuspendDrain
}

func (m *Market) submitMarketSuspend(ctx context.Context) error {
	m.epochMtx.RLock()
	finalEpochIdx := m.pendingLifecycleEpochIdx
	finalEpochDur := m.pendingLifecycleEpochDur
	m.epochMtx.RUnlock()
	if finalEpochIdx == 0 || finalEpochDur == 0 {
		return fmt.Errorf("market %s has no pending final epoch to suspend", m.name)
	}
	event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionSuspend, m.name, finalEpochIdx, finalEpochDur)
	event.Timestamp = time.Now().Truncate(time.Millisecond).UTC().UnixMilli()
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return err
	}
	_, err = m.mesh.ApplyEvent(ctx, meshEvent)
	return err
}

func (m *Market) lifecyclePendingResume(epochIdx, epochDur int64) bool {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	return m.lifecycleState == db.MarketStateSuspended &&
		m.pendingLifecycleAction == db.MarketPendingResume &&
		m.pendingLifecycleEpochIdx == epochIdx &&
		m.pendingLifecycleEpochDur == epochDur
}

func (m *Market) submitMarketResume(ctx context.Context, pendingEpochIdx, pendingEpochDur int64) error {
	m.resumeSubmitMtx.Lock()
	defer m.resumeSubmitMtx.Unlock()

	if pendingEpochIdx == 0 || pendingEpochDur == 0 || !m.lifecyclePendingResume(pendingEpochIdx, pendingEpochDur) {
		return fmt.Errorf("market %s has no matching pending resume epoch %d:%d",
			m.name, pendingEpochIdx, pendingEpochDur)
	}
	// schedule_resume named this epoch in the old duration unit.
	if pendingEpochDur != m.configuredParams.epochDur {
		return fmt.Errorf("market %s epoch duration changed from %d to %d; revert the configured duration, "+
			"resume, and change it at the next market start",
			m.name, pendingEpochDur, m.configuredParams.epochDur)
	}
	runParams := m.configuredParams.MarketRunParams
	bookedRevokes, err := m.startupBookedRevokes(ctx, runParams.LotSize)
	if err != nil {
		return err
	}
	sortStartupOrderRevokes(bookedRevokes)
	event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionResume, m.name, pendingEpochIdx, pendingEpochDur)
	event.Timestamp = time.Now().Truncate(time.Millisecond).UTC().UnixMilli()
	event.ResumeRevokes = encodeStartupOrderRevokes(bookedRevokes)
	event.RunParams = &runParams
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return err
	}
	_, err = m.mesh.ApplyEvent(ctx, meshEvent)
	return err
}

func (m *Market) applyMarketSuspendPurge(purged []order.OrderID) {
	if len(purged) == 0 {
		return
	}
	m.bookMtx.Lock()
	var removed []*order.LimitOrder
	for _, oid := range purged {
		lo, ok := m.book.Remove(oid)
		if !ok {
			continue
		}
		delete(m.settling, oid)
		removed = append(removed, lo)
	}
	m.bookMtx.Unlock()
	for _, lo := range removed {
		m.unlockOrderCoins(lo)
		m.sendRevokeOrderNote(lo.ID(), lo.User())
	}
}

func (m *Market) applyMarketResumeCleanup(revokes []*db.StartupOrderRevoke) []*order.LimitOrder {
	if len(revokes) == 0 {
		return nil
	}
	m.bookMtx.Lock()
	var removed []order.Order
	var removedLimits []*order.LimitOrder
	for _, revoke := range revokes {
		if revoke == nil || revoke.Order == nil {
			continue
		}
		lo, ok := m.book.Remove(revoke.Order.ID())
		if ok {
			delete(m.settling, lo.ID())
			removedLimits = append(removedLimits, lo)
		}
		removed = append(removed, revoke.Order)
	}
	m.bookMtx.Unlock()
	for _, ord := range removed {
		m.unlockOrderCoins(ord)
		m.sendRevokeOrderNote(ord.ID(), ord.User())
	}
	return removedLimits
}

func (m *Market) suspendEpoch(asSoonAs time.Time) (finalEpochIdx int64, finalEpochEnd time.Time) {
	dur := int64(m.EpochDuration())

	epochEnd := func(idx int64) time.Time {
		start := time.UnixMilli(idx * dur)
		return start.Add(time.Duration(dur) * time.Millisecond)
	}

	// Soonest final epoch is the one after the live epoch (matches the
	// schedule_suspend validator).
	soonestFinalIdx := m.activeEpochIdx + 1
	if m.activeEpochIdx == 0 {
		if m.startEpochIdx == 0 {
			return -1, time.Time{}
		}
		soonestFinalIdx = m.startEpochIdx - 1
	}

	if soonestEnd := epochEnd(soonestFinalIdx); asSoonAs.Before(soonestEnd) {
		finalEpochIdx = soonestFinalIdx
		finalEpochEnd = soonestEnd
	} else {
		ms := asSoonAs.UnixMilli()
		finalEpochIdx = ms / dur
		if ms%dur == 0 {
			finalEpochIdx--
		}
		finalEpochEnd = epochEnd(finalEpochIdx)
	}
	return
}

// ScheduleSuspendEvent builds a schedule_suspend event (last trading epoch
// at or after asSoonAs) for the caller to apply through mesh. Fails before
// a live epoch exists.
func (m *Market) ScheduleSuspendEvent(asSoonAs time.Time, persistBook bool) (*mesh.Event, *SuspendEpoch, error) {
	m.epochMtx.RLock()
	liveEpoch := m.currentEpoch != nil || m.activeEpochIdx > 0
	if !liveEpoch {
		m.epochMtx.RUnlock()
		return nil, nil, fmt.Errorf("unable to schedule suspend for market %s before a live epoch is established", m.name)
	}
	finalEpochIdx, finalEpochEnd := m.suspendEpoch(asSoonAs)
	epochDur := int64(m.EpochDuration())
	m.epochMtx.RUnlock()
	if finalEpochIdx < 0 {
		return nil, nil, fmt.Errorf("unable to schedule suspend for market %s", m.name)
	}
	event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionScheduleSuspend, m.name, finalEpochIdx, epochDur)
	event.PersistBook = &persistBook
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return nil, nil, err
	}
	return meshEvent, &SuspendEpoch{Idx: finalEpochIdx, End: finalEpochEnd}, nil
}

func (m *Market) validateScheduleSuspendEvent(finalEpochIdx, finalEpochDur int64) error {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	if finalEpochDur != int64(m.EpochDuration()) {
		return fmt.Errorf("schedule_suspend epoch duration %d mismatches market duration %d",
			finalEpochDur, m.EpochDuration())
	}
	switch m.pendingLifecycleAction {
	case db.MarketPendingNone, db.MarketPendingSuspend:
	default:
		return fmt.Errorf("schedule_suspend rejected in pending lifecycle action %d", m.pendingLifecycleAction)
	}
	if m.lifecycleState == db.MarketStateSuspended {
		return fmt.Errorf("schedule_suspend rejected for suspended market %s", m.name)
	}
	if m.currentEpoch != nil {
		// With an epoch live: final epoch must be strictly after current, and
		// a pending suspend already at the current epoch cannot be moved.
		if m.pendingLifecycleAction == db.MarketPendingSuspend && m.pendingLifecycleEpochIdx == m.currentEpoch.Epoch {
			return fmt.Errorf("schedule_suspend rejected: market %s final epoch %d is already closing",
				m.name, m.currentEpoch.Epoch)
		}
		if finalEpochIdx <= m.currentEpoch.Epoch {
			return fmt.Errorf("schedule_suspend final epoch %d is not after current epoch %d",
				finalEpochIdx, m.currentEpoch.Epoch)
		}
		return nil
	}
	soonestFinalIdx := m.activeEpochIdx
	if soonestFinalIdx == 0 && m.startEpochIdx > 0 {
		soonestFinalIdx = m.startEpochIdx - 1
	}
	if soonestFinalIdx > 0 && finalEpochIdx < soonestFinalIdx {
		return fmt.Errorf("schedule_suspend final epoch %d is before current schedulable epoch %d",
			finalEpochIdx, soonestFinalIdx)
	}
	return nil
}

// ResumeEpoch is the first epoch index at or after asSoonAs in the live
// duration unit. The scheduled index must stay in that unit; submitMarketResume
// refuses if configuredParams.epochDur disagrees. Zero if the market is already
// running.
func (m *Market) ResumeEpoch(asSoonAs time.Time) (startEpochIdx int64) {
	// Only allow scheduling a resume if the market is not running.
	if m.Running() {
		return
	}

	dur := m.liveParams.Load().epochDur

	now := time.Now().UnixMilli()
	nextEpochIdx := 1 + now/dur

	ms := asSoonAs.UnixMilli()
	startEpochIdx = max(1+ms/dur, nextEpochIdx)
	return
}

// ScheduleResumeEvent builds a schedule_resume event for the caller to
// apply through mesh. Fails if the market is already running.
func (m *Market) ScheduleResumeEvent(asSoonAs time.Time) (*mesh.Event, int64, time.Time, error) {
	startEpochIdx := m.ResumeEpoch(asSoonAs)
	if startEpochIdx == 0 {
		return nil, 0, time.Time{}, fmt.Errorf("unable to resume market %s at time %v", m.name, asSoonAs)
	}
	epochDur := m.liveParams.Load().epochDur
	startTime := time.UnixMilli(epochDur * startEpochIdx)
	event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionScheduleResume, m.name, startEpochIdx, epochDur)
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	return meshEvent, startEpochIdx, startTime, nil
}

// Status describes the operation state of the Market.
type Status struct {
	Running       bool
	EpochDuration uint64 // to compute times from epoch inds
	ActiveEpoch   int64
	StartEpoch    int64
	SuspendEpoch  int64
	PersistBook   *bool
	Base, Quote   uint32
	LotSize       uint64
	RateStep      uint64
	ParcelSize    uint32
}

// LifecyclePhase is the market's suspend/resume control phase.
type LifecyclePhase uint8

const (
	LifecyclePhaseUnknown    LifecyclePhase = iota
	LifecyclePhaseRunning                   // live; suspend may be scheduled
	LifecyclePhaseSuspended                 // parked; resume may be scheduled
	LifecyclePhaseSuspending                // final epoch closed; suspend event pending
)

// persistBookForStatusLocked reports whether PersistBook should appear in
// Status: only once a suspend/resume decision exists. Caller holds epochMtx.
func (m *Market) persistBookForStatusLocked() *bool {
	if !m.persistBookSet {
		return nil
	}
	if m.suspendEpochIdx == 0 &&
		m.pendingLifecycleAction != db.MarketPendingResume &&
		m.lifecycleState != db.MarketStateSuspended {
		return nil
	}
	persist := m.persistBook
	return &persist
}

// Status returns the current operating state of the Market.
func (m *Market) Status() *Status {
	m.epochMtx.Lock()
	defer m.epochMtx.Unlock()
	return &Status{
		Running:       m.Running(),
		EpochDuration: m.EpochDuration(),
		LotSize:       m.LotSize(),
		RateStep:      m.RateStep(),
		ParcelSize:    m.ParcelSize(),
		ActiveEpoch:   m.activeEpochIdx,
		StartEpoch:    m.startEpochIdx,
		SuspendEpoch:  m.suspendEpochIdx,
		PersistBook:   m.persistBookForStatusLocked(),
		Base:          m.base,
		Quote:         m.quote,
	}
}

// LifecyclePhase reports this market's suspend/resume control phase.
func (m *Market) LifecyclePhase() LifecyclePhase {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	liveEpoch := m.currentEpoch != nil || m.activeEpochIdx > 0
	switch {
	case m.lifecycleState == db.MarketStateRunning &&
		(m.pendingLifecycleAction == db.MarketPendingNone || m.pendingLifecycleAction == db.MarketPendingSuspend) &&
		liveEpoch:
		return LifecyclePhaseRunning
	case m.lifecycleState == db.MarketStateRunning && m.pendingLifecycleAction == db.MarketPendingSuspendDrain:
		return LifecyclePhaseSuspending
	case m.lifecycleState == db.MarketStateSuspended &&
		(m.pendingLifecycleAction == db.MarketPendingNone || m.pendingLifecycleAction == db.MarketPendingResume):
		return LifecyclePhaseSuspended
	default:
		return LifecyclePhaseUnknown
	}
}

// Running indicates is the market is accepting new orders. This will return
// false when suspended, but false does not necessarily mean Run has stopped
// since a start epoch may be set. Note that this method is of limited use and
// communicating subsystems shouldn't rely on the result for correct operation
// since a market could start or stop. Rather, they should infer or be informed
// of market status rather than rely on this.
//
// TODO: Instead of using Running in OrderRouter and DEX, these types should
// track statuses (known suspend times).
func (m *Market) Running() bool {
	return m.running.Load()
}

// EpochDuration is the market's epoch duration in milliseconds.
func (m *Market) EpochDuration() uint64 {
	return uint64(m.liveParams.Load().epochDur)
}

// startParams is what this market_started should carry.
//
// If we are only finishing a suspend (already draining, or the last trading
// epoch already passed), return liveParams. A new lot size here would cancel
// booked orders for a run that never opens. Otherwise return the file.
func (m *Market) startParams() marketRun {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	live := *m.liveParams.Load()
	switch m.pendingLifecycleAction {
	case db.MarketPendingSuspendDrain:
		return live
	case db.MarketPendingSuspend:
		if currentMarketEpochIdx(live.epochDur) > m.pendingLifecycleEpochIdx {
			return live
		}
	}
	return m.configuredParams
}

// MarketBuyBuffer is an origin-only funding heuristic, not a run parameter.
func (m *Market) MarketBuyBuffer() float64 {
	return m.marketBuyBuffer
}

// LotSize is the market's lot size in units of the base asset.
func (m *Market) LotSize() uint64 {
	return m.liveParams.Load().LotSize
}

// RateStep is the market's rate step in units of the quote asset.
func (m *Market) RateStep() uint64 {
	return m.liveParams.Load().RateStep
}

func (m *Market) minimumRate() uint64 {
	return m.liveParams.Load().MinimumRate
}

func (m *Market) maxUserCancelsPerEpoch() uint32 {
	return m.liveParams.Load().MaxUserCancelsPerEpoch
}

// Base is the base asset ID.
func (m *Market) Base() uint32 {
	return m.base
}

// Quote is the quote asset ID.
func (m *Market) Quote() uint32 {
	return m.quote
}

// SetUnbookNotifier is called for unbooks outside an event applier's own
// book-router notify: at-fault SwapDone and orders_revoked.
func (m *Market) SetUnbookNotifier(f func(*order.LimitOrder)) {
	m.unbookMtx.Lock()
	m.unbookNotifier = f
	m.unbookMtx.Unlock()
}

// notifyUnbooked delivers an unbooked order to the registered unbook notifier,
// if any.
func (m *Market) notifyUnbooked(lo *order.LimitOrder) {
	m.unbookMtx.RLock()
	notify := m.unbookNotifier
	m.unbookMtx.RUnlock()
	if notify != nil {
		notify(lo)
	}
}

func marketOrderError(err error) *msgjson.Error {
	code := msgjson.UnknownMarketError
	switch {
	case errors.Is(err, ErrInternalServer), errors.Is(err, errEpochOrderStorage):
		code = msgjson.RPCInternalError
		log.Errorf("Market order submission failed: %v", err)
	case errors.Is(err, ErrMarketNotRunning):
		code = msgjson.MarketNotRunningError
	case errors.Is(err, ErrQuantityTooHigh):
		code = msgjson.OrderQuantityTooHigh
	case errors.Is(err, ErrInvalidRate), errors.Is(err, ErrInvalidCommitment), errors.Is(err, ErrInvalidOrder):
		code = msgjson.OrderParameterError
	default:
		log.Debugf("Market order submission failed: %v", err)
	}
	return mesh.ClientError(err, code, "%v", err)
}

// resendResultWindow must exceed the client's worst-case ladder span (ten
// attempts of up to fundingTxWait+1min each plus ~1min of backoff, ~21min)
// so a late resend cannot outlive it and mint a second life.
const resendResultWindow = 30 * time.Minute

// sameOrderAs reports whether incoming is the same order as stored: stamped
// with stored's server time, it must serialize to the same order ID. A hit
// leaves that stamp on incoming; a miss clears it.
func sameOrderAs(incoming, stored order.Order) bool {
	incoming.SetTime(time.UnixMilli(stored.Time()))
	if incoming.ID() == stored.ID() {
		return true
	}
	// Restore the unstamped state on a miss.
	incoming.SetTime(time.Time{})
	return false
}

// ResendOfKnownOrder answers a client sending the same order again. Call it
// before other submission checks: those reject the resend because of the
// original order (locked coins, a live commitment). If the order is still
// live, return the stored result. If it was archived recently, return a
// retired error so the client stops tracking it. If the lookup fails, return
// TryAgainLater; continuing would refuse an order we may already have taken.
func (m *Market) ResendOfKnownOrder(ctx context.Context, rec *orderRecord, completion *mesh.CommandCompletion) (handled bool, rpcErr *msgjson.Error) {
	commit := rec.order.Commitment()

	m.epochMtx.RLock()
	oid, found := m.epochCommitments[commit]
	epochOrd := m.epochOrders[oid]
	m.epochMtx.RUnlock()
	if found && epochOrd != nil {
		if !sameOrderAs(rec.order, epochOrd) {
			return false, nil
		}
		return true, m.completeStoredOrderResult(ctx, rec, completion)
	}

	// The active-table hit is unique (live commits are), but the archived
	// window can hold several lives of a legally reused commitment; identity
	// must be checked against every one.
	candidates, err := m.storage.OrdersWithCommit(ctx, m.base, m.quote, commit,
		time.Now().Add(-resendResultWindow))
	if err != nil {
		log.Errorf("Resend lookup for commitment %v failed: %v", commit, err)
		return true, msgjson.NewError(msgjson.TryAgainLaterError,
			"order resend lookup unavailable; retry the request")
	}
	for _, cand := range candidates {
		if !sameOrderAs(rec.order, cand.Order) {
			continue
		}
		switch cand.Status {
		case order.OrderStatusEpoch, order.OrderStatusBooked:
			return true, m.completeStoredOrderResult(ctx, rec, completion)
		default:
			// A success answer would have the client track a dead order.
			return true, msgjson.NewError(msgjson.UnknownOrderError,
				"order %v with this commitment was already accepted and retired", cand.Order.ID())
		}
	}
	return false, nil
}

// completeStoredOrderResult delivers the stored result. rec must already be
// stamped (sameOrderAs). No event is emitted.
func (m *Market) completeStoredOrderResult(ctx context.Context, rec *orderRecord, completion *mesh.CommandCompletion) *msgjson.Error {
	respMsg, err := m.orderResponse(rec)
	if err != nil {
		log.Errorf("failed to create msgjson.Message for resent order %v response: %v", rec.order.ID(), err)
		return msgjson.NewError(msgjson.RPCInternalError, "%v", ErrMalformedOrderResponse)
	}
	result, err := orderResultFromResponse(respMsg)
	if err != nil {
		return msgjson.NewError(msgjson.RPCInternalError, "failed to build order result: %v", err)
	}
	log.Debugf("Answering resend of accepted order %v with its stored result.", rec.order.ID())
	if err := completion.Complete(ctx, result); err != nil {
		// Delivery failure only: the client resends again and this path
		// answers again.
		log.Errorf("failed to deliver stored order result for %v: %v", rec.order.ID(), err)
	}
	return nil
}

func (m *Market) stampedOrderAcceptedEvent(rec *orderRecord) (*mesh.Event, *msgjson.OrderResult, *msgjson.Error) {
	sTime := time.Now().Truncate(time.Millisecond).UTC()
	rec.order.SetTime(sTime)
	log.Tracef("Received order %v at %v", rec.order, sTime)

	if err := m.validateOrderAcceptedPreEvent(rec.order); err != nil {
		return nil, nil, marketOrderError(err)
	}

	respMsg, err := m.orderResponse(rec)
	if err != nil {
		log.Errorf("failed to create msgjson.Message for order %v, msgID %v response: %v",
			rec.order, rec.msgID, err)
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "%v", ErrMalformedOrderResponse)
	}
	result, err := orderResultFromResponse(respMsg)
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "failed to build order result: %v", err)
	}
	event, err := mesh.NewEvent(meshevents.NewOrderAcceptedEvent(rec.order))
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "failed to build accepted order event: %v", err)
	}
	return event, result, nil
}

func (m *Market) stampedSuspendedCancelEvent(rec *orderRecord) (*mesh.Event, *msgjson.OrderResult, *msgjson.Error) {
	co, ok := rec.order.(*order.CancelOrder)
	if !ok {
		return nil, nil, marketOrderError(ErrInvalidOrder)
	}
	if cancelable, _, err := m.CancelableBy(co.TargetOrderID, co.AccountID); !cancelable {
		return nil, nil, marketOrderError(err)
	}
	m.bookMtx.Lock()
	target := m.book.Order(co.TargetOrderID)
	m.bookMtx.Unlock()
	if target == nil {
		return nil, nil, marketOrderError(ErrTargetNotCancelable)
	}

	sTime := time.Now().Truncate(time.Millisecond).UTC()
	co.SetTime(sTime)
	respMsg, err := m.orderResponse(rec)
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "%v", ErrMalformedOrderResponse)
	}
	result, err := orderResultFromResponse(respMsg)
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "failed to build order result: %v", err)
	}
	dur := int64(m.EpochDuration())
	epochIdx := time.Now().UnixMilli() / dur
	matchServerTime := time.Now().Truncate(time.Millisecond).UTC()
	event, err := mesh.NewEvent(meshevents.NewSuspendedCancelEvent(m.name, m.base,
		m.quote, co, target, epochIdx, dur, m.getFeeRate(m.Base(), m.baseFeeFetcher),
		m.getFeeRate(m.Quote(), m.quoteFeeFetcher), matchServerTime))
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.RPCInternalError, "failed to build suspended cancel event: %v", err)
	}
	return event, result, nil
}

// validateOrderAcceptedPreEvent performs trade eligibility checks before the
// authoritative order_accepted event is created. This function should not be
// called as part of the event applier.
func (m *Market) validateOrderAcceptedPreEvent(ord order.Order) error {
	if m.orderAtOrAfterPendingSuspendBoundary(ord) {
		return ErrMarketNotRunning
	}
	if ord.Type() == order.CancelOrderType {
		return nil
	}
	oid := ord.ID()
	if _, tier := m.auth.AcctStatus(ord.User()); tier < 1 {
		log.Debugf("Account %v with tier %d not allowed to submit order %v", ord.User(), tier, oid)
		return ErrSuspendedAccount
	}
	return m.validateOrderAcceptedParcelLimit(ord)
}

func (m *Market) orderAtOrAfterPendingSuspendBoundary(ord order.Order) bool {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()
	return m.orderAtOrAfterPendingSuspendBoundaryLocked(ord)
}

func (m *Market) orderAtOrAfterPendingSuspendBoundaryLocked(ord order.Order) bool {
	switch m.pendingLifecycleAction {
	case db.MarketPendingSuspend, db.MarketPendingSuspendDrain:
	default:
		return false
	}
	if m.pendingLifecycleEpochIdx == 0 || m.pendingLifecycleEpochDur == 0 {
		return false
	}
	return ord.Time() >= (m.pendingLifecycleEpochIdx+1)*m.pendingLifecycleEpochDur
}

func (m *Market) validateOrderAcceptedParcelLimit(ord order.Order) error {
	if ord.Type() == order.CancelOrderType {
		return nil
	}
	likelyTaker, baseQty := m.analysisHelpers()
	orderWeight := baseQty(ord)
	if likelyTaker(ord) {
		orderWeight *= 2
	}
	user := ord.User()
	calcParcels := func(settlingWeight uint64) float64 {
		return m.parcels(user, settlingWeight+orderWeight)
	}
	ok, err := m.checkParcelLimit(user, time.UnixMilli(ord.Time()).UTC(), calcParcels)
	if err != nil {
		// Reputation load failed: propagate, do not treat as over-limit.
		return fmt.Errorf("parcel limit reputation load for order %v: %w: %w", ord.ID(), ErrInternalServer, err)
	}
	if !ok {
		oid := ord.ID()
		log.Debugf("Received order %s that pushed user over the parcel limit", oid)
		return ErrQuantityTooHigh
	}
	return nil
}

// AcceptOrderCommand runs the order on the master: a byte-identical resend
// is answered from store, a cancel on a suspended market emits
// suspended_cancel, otherwise order_accepted (restamped if the epoch closed).
func (m *Market) AcceptOrderCommand(ctx context.Context, rec *orderRecord, completion *mesh.CommandCompletion) *msgjson.Error {
	if err := m.validateOrder(rec.order); err != nil {
		log.Debugf("AcceptOrderCommand: Invalid order received from user %v with commitment %v: %v",
			rec.order.User(), rec.order.Commitment(), err)
		return marketOrderError(err)
	}

	if !m.Running() {
		if rec.order.Type() != order.CancelOrderType {
			log.Infof("AcceptOrderCommand: Market stopped with an order in submission (commitment %v).",
				rec.order.Commitment())
			return msgjson.NewError(msgjson.MarketNotRunningError, "%v", ErrMarketNotRunning)
		}
		if handled, rpcErr := m.acceptSuspendedCancel(ctx, rec, completion); handled {
			return rpcErr
		}
		// The market resumed while acquiring resumeSubmitMtx; take the normal
		// running-market path below.
	}

	commit := rec.order.Commitment()
	m.epochMtx.RLock()
	otherOID, found := m.epochCommitments[commit]
	m.epochMtx.RUnlock()
	if found {
		// The live commitment can belong to this exact payload: an identical
		// in-flight duplicate whose first life applied after the router's
		// resend lookup ran. Answer idempotently; refuse only a true mismatch.
		if handled, rpcErr := m.ResendOfKnownOrder(ctx, rec, completion); handled {
			return rpcErr
		}
		log.Debugf("Received order with commitment %x also used in previous order %v!",
			commit, otherOID)
		return marketOrderError(ErrInvalidCommitment)
	}

	// Make two attempts to apply the order accepted event, to handle the case
	// where the order is stamped to go into an epoch that was already closed.
	for attempt := 0; attempt < 2; attempt++ {
		event, result, rpcErr := m.stampedOrderAcceptedEvent(rec)
		if rpcErr != nil {
			return rpcErr
		}
		if err := completion.Emit(ctx, event, func() any { return result }); err != nil {
			if attempt == 0 && errors.Is(err, ErrEpochMissed) {
				log.Debugf("Restamping order %v after missed epoch during event apply", rec.order.ID())
				continue
			}
			if db.IsErrReusedCommit(err) {
				// Make sure there wasn't another concurrent identical duplicate.
				if handled, rpcErr := m.ResendOfKnownOrder(ctx, rec, completion); handled {
					return rpcErr
				}
				return marketOrderError(ErrInvalidCommitment)
			}
			return marketOrderError(err)
		}
		return nil
	}

	panic("unreachable order acceptance retry state")
}

// acceptSuspendedCancel handles a cancel order submitted while the market is
// not accepting orders. If the market is suspended (with no pending action or
// a pending resume), the cancel is emitted as a suspended_cancel event. It
// reports handled=false without emitting when the market resumed while
// acquiring resumeSubmitMtx, in which case the caller takes the normal
// running-market path.
func (m *Market) acceptSuspendedCancel(ctx context.Context, rec *orderRecord, completion *mesh.CommandCompletion) (handled bool, rpcErr *msgjson.Error) {
	m.resumeSubmitMtx.RLock()
	defer m.resumeSubmitMtx.RUnlock()

	if m.Running() {
		return false, nil
	}
	m.epochMtx.RLock()
	suspended := m.lifecycleState == db.MarketStateSuspended &&
		(m.pendingLifecycleAction == db.MarketPendingNone || m.pendingLifecycleAction == db.MarketPendingResume)
	m.epochMtx.RUnlock()
	if !suspended {
		return true, msgjson.NewError(msgjson.MarketNotRunningError, "%v", ErrMarketNotRunning)
	}
	event, result, rpcErr := m.stampedSuspendedCancelEvent(rec)
	if rpcErr != nil {
		// Once an identical duplicate applied, CancelableBy no longer sees
		// the target it unbooked.
		if handled, resendErr := m.ResendOfKnownOrder(ctx, rec, completion); handled {
			return true, resendErr
		}
		return true, rpcErr
	}
	if err := completion.Emit(ctx, event, func() any { return result }); err != nil {
		// Make sure there wasn't another concurrent identical duplicate.
		if handled, resendErr := m.ResendOfKnownOrder(ctx, rec, completion); handled {
			return true, resendErr
		}
		return true, marketOrderError(err)
	}
	return true, nil
}

// matchNotifications creates a pair of msgjson.Match from a match, stamped
// with the given server time.
func matchNotifications(match order.Match, serverTime time.Time) (makerMsg *msgjson.Match, takerMsg *msgjson.Match) {
	stamp := uint64(serverTime.UnixMilli())
	return &msgjson.Match{
			OrderID:      idToBytes(match.Maker.ID()),
			MatchID:      idToBytes(match.ID()),
			Quantity:     match.Quantity,
			Rate:         match.Rate,
			Address:      order.ExtractAddress(match.Taker),
			ServerTime:   stamp,
			FeeRateBase:  match.FeeRateBase,
			FeeRateQuote: match.FeeRateQuote,
			Side:         uint8(order.Maker),
		}, &msgjson.Match{
			OrderID:      idToBytes(match.Taker.ID()),
			MatchID:      idToBytes(match.ID()),
			Quantity:     match.Quantity,
			Rate:         match.Rate,
			Address:      order.ExtractAddress(match.Maker),
			ServerTime:   stamp,
			FeeRateBase:  match.FeeRateBase,
			FeeRateQuote: match.FeeRateQuote,
			Side:         uint8(order.Taker),
		}
}

func (m *Market) sendSuspendedCancelMatchRequest(user account.AccountID, match *order.Match, serverTime time.Time) {
	if match == nil {
		return
	}
	makerMsg, takerMsg := matchNotifications(*match, serverTime)
	m.auth.Sign(makerMsg)
	m.auth.Sign(takerMsg)
	msgs := []msgjson.Signable{makerMsg, takerMsg}
	req, err := msgjson.NewRequest(comms.NextID(), msgjson.MatchRoute, msgs)
	if err != nil {
		log.Errorf("Failed to create suspended cancel match request: %v", err)
		return
	}
	if err = m.auth.RequestIfLocal(user, req, func(_ comms.Link, resp *msgjson.Message) {
		m.processMatchAcksForCancel(user, resp)
	}); err != nil {
		log.Errorf("Failed to send suspended cancel match request: %v", err)
	}
}

// processMatchAcksForCancel is called when receiving a response to a match
// request for a cancel order. Nothing is done other than logging and verifying
// that the response is in the correct format.
//
// This is currently only used for cancel orders that happen while the market is
// suspended, but may be later used for all cancel orders.
func (m *Market) processMatchAcksForCancel(user account.AccountID, msg *msgjson.Message) {
	var acks []msgjson.Acknowledgement
	err := msg.UnmarshalResult(&acks)
	if err != nil {
		m.respondError(msg.ID, user, msgjson.RPCParseError,
			fmt.Sprintf("error parsing match request acknowledgment: %v", err))
		return
	}
	// The acknowledgment for both the taker and maker should come from the same user.
	expectedNumAcks := 2
	if len(acks) != expectedNumAcks {
		m.respondError(msg.ID, user, msgjson.AckCountError,
			fmt.Sprintf("expected %d acknowledgements, got %d", expectedNumAcks, len(acks)))
		return
	}
	log.Debugf("processMatchAcksForCancel: 'match' ack received from %v", user)
}

// MidGap returns the mid-gap market rate, which is ths rate halfway between the
// best buy order and the best sell order in the order book. If one side has no
// orders, the best order rate on other side is returned. If both sides have no
// orders, 0 is returned.
func (m *Market) MidGap() uint64 {
	_, mid, _ := m.rates()
	return mid
}

func (m *Market) rates() (bestBuyRate, mid, bestSellRate uint64) {
	bestBuy, bestSell := m.book.Best()
	if bestBuy == nil {
		if bestSell == nil {
			return
		}
		return 0, bestSell.Rate, bestSell.Rate
	} else if bestSell == nil {
		return bestBuy.Rate, bestBuy.Rate, math.MaxUint64
	}
	mid = (bestBuy.Rate + bestSell.Rate) / 2 // note downward bias on truncate
	return bestBuy.Rate, mid, bestSell.Rate
}

// CoinLocked checks if a coin is locked. The asset is specified since we should
// not assume that a CoinID for one asset cannot be made to match another
// asset's CoinID.
func (m *Market) CoinLocked(asset uint32, coin coinlock.CoinID) bool {
	switch {
	case asset == m.base && m.coinLockerBase != nil:
		return m.coinLockerBase.CoinLocked(coin)
	case asset == m.quote && m.coinLockerQuote != nil:
		return m.coinLockerQuote.CoinLocked(coin)
	default:
		panic(fmt.Sprintf("invalid utxo-based asset %d for market %s", asset, m.name))
	}
}

// Cancelable determines if an order is a limit order with time-in-force
// standing that is in either the epoch queue or in the order book.
func (m *Market) Cancelable(oid order.OrderID) bool {
	// All book orders are standing limit orders.
	if m.book.HaveOrder(oid) {
		return true
	}

	// Check the active epochs (includes current and next).
	m.epochMtx.RLock()
	ord := m.epochOrders[oid]
	m.epochMtx.RUnlock()

	if lo, ok := ord.(*order.LimitOrder); ok {
		return lo.Force == order.StandingTiF
	}
	return false
}

// CancelableBy determines if an order is cancelable by a certain account. This
// means: (1) an order in the book or epoch queue, (2) type limit with
// time-in-force standing (implied for book orders), and (3) AccountID field
// matching the provided account ID.
func (m *Market) CancelableBy(oid order.OrderID, aid account.AccountID) (bool, time.Time, error) {
	// All book orders are standing limit orders.
	if lo := m.book.Order(oid); lo != nil {
		if lo.AccountID == aid {
			return true, lo.ServerTime, nil
		}
		return false, time.Time{}, ErrCancelNotPermitted
	}

	// Check the active epochs (includes current and next).
	m.epochMtx.RLock()
	ord := m.epochOrders[oid]
	m.epochMtx.RUnlock()

	if ord == nil {
		return false, time.Time{}, ErrTargetNotActive
	}

	lo, ok := ord.(*order.LimitOrder)
	if !ok {
		return false, time.Time{}, ErrTargetNotCancelable
	}
	if lo.Force != order.StandingTiF {
		return false, time.Time{}, ErrTargetNotCancelable
	}
	if lo.AccountID != aid {
		return false, time.Time{}, ErrCancelNotPermitted
	}
	return true, lo.ServerTime, nil
}

// spentFundingOrders scans the funding coins of the given unfilled book orders
// and returns the orders whose funding is spent. This observes chain state, so
// it must only run on the mesh master; the conclusion is recorded with an
// orders_revoked event rather than mutating any state here.
func (m *Market) spentFundingOrders(assetID uint32, unfilled []*order.LimitOrder) (spent []*order.LimitOrder) {
	checkUnspent := func(assetID uint32, coinID []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return m.swapper.CheckUnspent(ctx, assetID, coinID)
	}

orders:
	for _, lo := range unfilled {
		log.Tracef("Checking %d funding coins for order %v", len(lo.Coins), lo.ID())
		for i := range lo.Coins {
			err := checkUnspent(assetID, lo.Coins[i])
			if err == nil {
				continue // unspent, check next coin
			}

			if !errors.Is(err, asset.CoinNotFoundError) {
				// other failure (timeout, coinID decode, RPC, etc.)
				log.Errorf("Unexpected error checking coinID %v for order %v: %v",
					lo.Coins[i], lo, err)
				continue orders
				// NOTE: This does not revoke orders since this is likely to be
				// a configuration or node issue.
			}

			// Final fill amount check in case it was matched after we pulled
			// the list of unfilled orders from the book.
			if lo.Filled() == 0 {
				log.Warnf("Coin %s not unspent for unfilled order %v. "+
					"Revoking the order.", fmtCoinID(assetID, lo.Coins[i]), lo)
				spent = append(spent, lo)
			}
			continue orders
		}
	}
	return
}

func (m *Market) applySettledMatch(ord order.Order, match *order.Match) {
	oid := ord.ID()
	m.bookMtx.Lock()
	defer m.bookMtx.Unlock()

	settling, found := m.settling[oid]
	if !found {
		return
	}
	if settling < match.Quantity {
		log.Errorf("Finished swap %v (qty %d) for order %v larger than current settling (%d) amount.",
			match.ID(), match.Quantity, oid, settling)
		settling = 0
	} else {
		settling -= match.Quantity
	}

	lo, limit := ord.(*order.LimitOrder)
	if settling > 0 || (limit && lo.Force == order.StandingTiF && m.book.HaveOrder(oid)) {
		m.settling[oid] = settling
		return
	}
	delete(m.settling, oid)
}

// SwapDone applies the market's in-memory swap-done projection after the DB
// event transaction has already recorded durable swap-done effects. faulted
// means this order side caused a match failure and should be unbooked/revoked
// in memory.
func (m *Market) SwapDone(ord order.Order, match *order.Match, faulted bool) {
	if !faulted {
		m.applySettledMatch(ord, match)
		return
	}

	oid := ord.ID()
	m.bookMtx.Lock()
	settling, found := m.settling[oid]
	if !found {
		m.bookMtx.Unlock()
		return
	}
	if settling < match.Quantity {
		log.Errorf("Finished swap %v (qty %d) for order %v larger than current settling (%d) amount.",
			match.ID(), match.Quantity, oid, settling)
	}

	lo, limit := ord.(*order.LimitOrder)
	delete(m.settling, oid)
	var removed bool
	if limit {
		_, removed = m.book.Remove(oid)
	}
	m.bookMtx.Unlock()
	if !limit {
		return
	}

	m.unlockOrderCoins(lo)
	if removed {
		m.sendRevokeOrderNote(oid, lo.User())
		m.notifyUnbooked(lo)
	}
}

// CheckUnfilled submits orders_revoked for booked orders whose funding coins
// are spent (uncounted cancellation). Master-only; observes chain state.
func (m *Market) CheckUnfilled(assetID uint32, user account.AccountID) (revoked []*order.LimitOrder) {
	base, quote := m.base, m.quote
	var unfilled []*order.LimitOrder
	switch assetID {
	case base:
		// Sell orders are funded by the base asset.
		unfilled = m.book.UnfilledUserSells(user)
	case quote:
		// Buy orders are funded by the quote asset.
		unfilled = m.book.UnfilledUserBuys(user)
	default:
		return
	}

	spent := m.spentFundingOrders(assetID, unfilled)
	if len(spent) == 0 {
		return
	}

	oids := make([]order.OrderID, 0, len(spent))
	for _, lo := range spent {
		oids = append(oids, lo.ID())
	}
	event, err := mesh.NewEvent(meshevents.NewOrdersRevokedForOrdersEvent(m.name, oids,
		meshevents.OrderRevokeReasonFundingSpent, time.Now().UTC()))
	if err != nil {
		log.Errorf("Failed to build orders_revoked event for %d spent-funding orders on market %s: %v",
			len(spent), m.name, err)
		return
	}
	if _, err := m.mesh.ApplyEvent(context.Background(), event); err != nil {
		log.Errorf("Failed to apply orders_revoked event for %d spent-funding orders on market %s: %v",
			len(spent), m.name, err)
		return
	}
	return spent
}

// BookedUsers returns the accounts owning booked orders on this market, with
// their booked order counts.
func (m *Market) BookedUsers() map[account.AccountID]int {
	return m.book.Users()
}

// AccountPending sums the orders quantities that pay to or from the specified
// account address.
func (m *Market) AccountPending(acctAddr string, assetID uint32) (qty, lots uint64, redeems int) {
	base, quote := m.base, m.quote
	if (assetID != base && assetID != quote) ||
		(assetID == m.base && m.coinLockerBase != nil) ||
		(assetID == m.quote && m.coinLockerQuote != nil) {

		return
	}

	midGap := m.MidGap()
	if midGap == 0 {
		midGap = m.RateStep()
	}

	lotSize := m.LotSize()
	switch assetID {
	case base:
		m.iterateBaseAccount(acctAddr, func(trade *order.Trade, rate uint64) {
			r := trade.Remaining()
			if trade.Sell {
				qty += r
				lots += r / lotSize
			} else {
				if rate == 0 { // market buy
					redeems += int(calc.QuoteToBase(midGap, r) / lotSize)
				} else {
					redeems += int(r / lotSize)
				}
			}
		})
	case quote:
		m.iterateQuoteAccount(acctAddr, func(trade *order.Trade, rate uint64) {
			r := trade.Remaining()
			if trade.Sell {
				redeems += int(r / lotSize)
			} else {
				if rate == 0 { // market buy
					qty += r
					lots += calc.QuoteToBase(midGap, r) / lotSize
				} else {
					qty += calc.BaseToQuote(midGap, r)
					lots += r / lotSize
				}
			}
		})
	}
	return
}

func (m *Market) iterateBaseAccount(acctAddr string, f func(*order.Trade, uint64)) {
	m.epochMtx.RLock()
	for _, epOrd := range m.epochOrders {
		if epOrd.Type() == order.CancelOrderType || epOrd.Trade().BaseAccount() != acctAddr {
			continue
		}
		var rate uint64
		if lo, is := epOrd.(*order.LimitOrder); is {
			rate = lo.Rate
		}
		f(epOrd.Trade(), rate)

	}
	m.epochMtx.RUnlock()
	m.book.IterateBaseAccount(acctAddr, func(lo *order.LimitOrder) {
		f(lo.Trade(), lo.Rate)
	})
}

func (m *Market) iterateQuoteAccount(acctAddr string, f func(*order.Trade, uint64)) {
	m.epochMtx.RLock()
	for _, epOrd := range m.epochOrders {
		if epOrd.Type() == order.CancelOrderType || epOrd.Trade().QuoteAccount() != acctAddr {
			continue
		}
		var rate uint64
		if lo, is := epOrd.(*order.LimitOrder); is {
			rate = lo.Rate
		}
		f(epOrd.Trade(), rate)
	}
	m.epochMtx.RUnlock()
	m.book.IterateQuoteAccount(acctAddr, func(lo *order.LimitOrder) {
		f(lo.Trade(), lo.Rate)
	})
}

// Book retrieves the market's cached order book and the last applied book
// epoch index. The epoch can be non-zero while the market is not accepting
// orders, such as after event replay or a committed-but-not-opened startup
// failure. Use Running or Status for order-acceptance state.
func (m *Market) Book() (epoch int64, buys, sells []*order.LimitOrder) {
	// NOTE: it may be desirable to cache the response.
	m.bookMtx.Lock()
	buys = m.book.BuyOrders()
	sells = m.book.SellOrders()
	epoch = m.bookEpochIdx
	m.bookMtx.Unlock()
	return
}

// Run drives the market's epoch loop on the acting master. Call
// SetMeshService first. marketStartupDone, if set, receives the startup
// outcome; success can still have order acceptance closed if the market
// starts suspended. The unbook notifier stays registered after Run returns.
func (m *Market) Run(ctx context.Context, marketStartupDone func(error)) {
	reportMarketStartup := func(err error) {
		if marketStartupDone != nil {
			marketStartupDone(err)
		}
	}

	// Prevent multiple incantations of Run.
	if !atomic.CompareAndSwapUint32(&m.up, 0, 1) {
		log.Errorf("Run: Market not stopped!")
		reportMarketStartup(Error("market already running"))
		return
	}
	defer atomic.StoreUint32(&m.up, 0)

	driver := newMarketEpochDriver(m)
	ready := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		driver.run(ctx, ready)
	}()

	reportMarketStartup(<-ready)
	<-done
}

func (m *Market) coinsLocked(o order.Order) ([]order.CoinID, uint32) {
	if o.Type() == order.CancelOrderType {
		return nil, 0
	}

	locker := m.coinLockerQuote
	assetID := m.quote
	if o.Trade().Trade().Sell {
		locker = m.coinLockerBase
		assetID = m.base
	}

	if locker == nil { // Not utxo-based
		return nil, 0
	}

	// Check if this order is known by the locker.
	lockedCoins := locker.OrderCoinsLocked(o.ID())
	if len(lockedCoins) > 0 {
		return lockedCoins, assetID
	}

	// Check the individual coins.
	for _, coin := range o.Trade().Coins {
		if locker.CoinLocked(coin) {
			lockedCoins = append(lockedCoins, coin)
		}
	}
	return lockedCoins, assetID
}

func (m *Market) lockOrderCoins(o order.Order) bool {
	if o.Type() == order.CancelOrderType {
		return true
	}

	if o.Trade().Sell {
		if m.coinLockerBase != nil {
			return len(m.coinLockerBase.LockOrdersCoins([]order.Order{o})) == 0
		}
	} else if m.coinLockerQuote != nil {
		return len(m.coinLockerQuote.LockOrdersCoins([]order.Order{o})) == 0
	}
	return true
}

func (m *Market) unlockOrderCoins(o order.Order) {
	if o.Type() == order.CancelOrderType {
		return
	}

	if o.Trade().Sell {
		if m.coinLockerBase != nil {
			m.coinLockerBase.UnlockOrderCoins(o.ID())
		}
	} else if m.coinLockerQuote != nil {
		m.coinLockerQuote.UnlockOrderCoins(o.ID())
	}
}

func (m *Market) analysisHelpers() (
	likelyTaker func(ord order.Order) bool,
	baseQty func(ord order.Order) uint64,
) {
	bestBuy, midGap, bestSell := m.rates()
	likelyTaker = func(ord order.Order) bool {
		lo, ok := ord.(*order.LimitOrder)
		if !ok || lo.Force == order.ImmediateTiF {
			return true
		}
		// Must cross the spread to be a taker (not so conservative).
		switch {
		case midGap == 0:
			return false // empty market: could be taker, but assume not
		case lo.Sell:
			return lo.Rate <= bestBuy
		default:
			return lo.Rate >= bestSell
		}
	}
	baseQty = func(ord order.Order) uint64 {
		if ord.Type() == order.CancelOrderType {
			return 0
		}
		qty := ord.Trade().Quantity
		if ord.Type() == order.MarketOrderType && !ord.Trade().Sell {
			// Market buy qty is in quote asset. Convert to base.
			if midGap == 0 {
				qty = m.LotSize() // no orders on the book; call it 1 lot
			} else {
				qty = calc.QuoteToBase(midGap, qty)
			}
		}
		return qty
	}
	return
}

// ParcelSize is the market's parcel size.
func (m *Market) ParcelSize() uint32 {
	return m.liveParams.Load().ParcelSize
}

// Parcels calculates the total parcels for the market with the specified
// settling quantity. Parcels is used as part of order validation for global
// parcel limits. Parcels is not called for the market for which the order is
// for, which will use m.checkParcelLimit to validate in processOrder.
func (m *Market) Parcels(user account.AccountID, settlingQty uint64) float64 {
	return m.parcels(user, settlingQty)
}

func (m *Market) parcels(user account.AccountID, addParcelWeight uint64) float64 {
	likelyTaker, baseQty := m.analysisHelpers()
	var takerQty, makerQty uint64
	m.epochMtx.RLock()
	for _, epOrd := range m.epochOrders {
		if epOrd.User() != user || epOrd.Type() == order.CancelOrderType {
			continue
		}

		// Even if standing, may count as taker for purposes of taker qty limit.
		if likelyTaker(epOrd) {
			takerQty += baseQty(epOrd)
		} else {
			makerQty += baseQty(epOrd)
		}
	}
	m.epochMtx.RUnlock()

	bookedBuyAmt, bookedSellAmt, _, _ := m.book.UserOrderTotals(user)
	makerQty += bookedBuyAmt + bookedSellAmt
	return calc.Parcels(makerQty+addParcelWeight, takerQty, m.LotSize(), m.ParcelSize())
}

func (m *Market) validateOrderAcceptedEvent(ord order.Order, book *msgBook) (*validatedOrderAcceptedEvent, error) {
	oid := ord.ID()

	// Accepted orders must be valid and not already booked.
	if err := m.validateOrder(ord); err != nil {
		return nil, err
	}
	if m.book.HaveOrder(oid) {
		return nil, fmt.Errorf("replicated accepted order %v is already booked", oid)
	}

	// Resolve the epoch selected by the order's server time.
	epochIdx, epochDur, epoch, err := m.acceptedOrderEpoch(ord)
	if err != nil {
		return nil, err
	}

	// Allow idempotent replay before checking coin locks.
	alreadyApplied, err := m.checkAcceptedOrderEpochState(ord, epoch)
	if err != nil {
		return nil, err
	}

	// Compute the persisted epoch gap for cancels.
	epochGap := db.EpochGapNA
	if co, ok := ord.(*order.CancelOrder); ok {
		epochGap, err = m.cancelOrderEpochGap(co, epochIdx, epochDur)
		if err != nil {
			log.Debugf("Cancel order %v (account=%v) target order %v: %v",
				co, co.AccountID, co.TargetOrderID, err)
			return nil, err
		}
	}

	if !alreadyApplied {
		// Re-check parcel limits against the local epoch/book projection.
		if err := m.validateOrderAcceptedParcelLimit(ord); err != nil {
			return nil, err
		}

		// Check for locked coins; lock them later during memory apply.
		if lockedCoins, assetID := m.coinsLocked(ord); len(lockedCoins) > 0 {
			return nil, fmt.Errorf("order %v submitted with already-locked %s coins: %v",
				ord.ID(), dex.BipIDSymbol(assetID), fmtCoinIDs(assetID, lockedCoins))
		}
	}

	// Return validated apply inputs without mutating memory.
	return &validatedOrderAcceptedEvent{
		ord:            ord,
		mkt:            m,
		book:           book,
		epochIdx:       epochIdx,
		epochDur:       epochDur,
		epochGap:       epochGap,
		alreadyApplied: alreadyApplied,
	}, nil
}

// checkAcceptedOrderEpochState inspects epoch memory for a replicated accepted
// order, reporting whether the order was already applied (idempotent replay)
// or conflicts with existing epoch state, and enforcing the cancel-specific
// per-epoch limits for new cancel orders.
func (m *Market) checkAcceptedOrderEpochState(ord order.Order, epoch *EpochQueue) (alreadyApplied bool, err error) {
	oid := ord.ID()
	commit := ord.Commitment()

	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()

	if existing, found := m.epochOrders[oid]; found {
		if existing.Commitment() == commit {
			return true, nil
		}
		return false, fmt.Errorf("replicated accepted order %v conflicts with existing epoch order", oid)
	}
	if otherOID, commitFound := m.epochCommitments[commit]; commitFound && otherOID != oid {
		return false, fmt.Errorf("replicated accepted order %v conflicts with commitment already used by %v", oid, otherOID)
	}

	// Apply cancel-specific epoch checks.
	if co, ok := ord.(*order.CancelOrder); ok && epoch != nil {
		if eco := epoch.CancelTargets[co.TargetOrderID]; eco != nil {
			log.Debugf("Received cancel order %v targeting %v, but already have %v.",
				co, co.TargetOrderID, eco)
			return false, ErrDuplicateCancelOrder
		}
		if nc := epoch.UserCancels[co.AccountID]; nc >= m.maxUserCancelsPerEpoch() {
			log.Debugf("Received cancel order %v targeting %v, but user already has %d cancel orders in this epoch.",
				co, co.TargetOrderID, nc)
			return false, ErrTooManyCancelOrders
		}
	}
	return false, nil
}

func (m *Market) cancelOrderEpochGap(co *order.CancelOrder, epochIdx, epochDur int64) (int32, error) {
	cancelable, loTime, err := m.CancelableBy(co.TargetOrderID, co.AccountID)
	if !cancelable {
		return 0, err
	}
	return int32(epochIdx - loTime.UnixMilli()/epochDur), nil
}

func (m *Market) acceptedOrderEpoch(ord order.Order) (epochIdx, epochDur int64, epoch *EpochQueue, err error) {
	sTime := time.UnixMilli(ord.Time())
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()

	if m.currentEpoch == nil {
		return 0, 0, nil, fmt.Errorf("order_accepted with no active epoch on market %s", m.name)
	}
	if m.orderAtOrAfterPendingSuspendBoundaryLocked(ord) {
		return 0, 0, nil, ErrMarketNotRunning
	}
	if m.currentEpoch.IncludesTime(sTime) {
		return m.currentEpoch.Epoch, m.currentEpoch.Duration, m.currentEpoch, nil
	}
	if m.nextEpoch != nil && m.nextEpoch.IncludesTime(sTime) {
		return m.nextEpoch.Epoch, m.nextEpoch.Duration, m.nextEpoch, nil
	}
	return 0, 0, nil, ErrEpochMissed
}

// applyOrderAcceptedMemory projects an accepted order into the in-memory
// market state. Validation and DB persistence have already succeeded.
func (m *Market) applyOrderAcceptedMemory(accepted *validatedOrderAcceptedEvent) {
	ord := accepted.ord
	oid := ord.ID()

	if !m.lockOrderCoins(ord) {
		// TODO(mesh): figure out whether this should be handled another way. During testing,
		// panic so we know if this supposedly impossible state can happen.
		panic(fmt.Sprintf("failed to lock accepted order %v coins during memory apply", oid))
	}

	m.epochMtx.Lock()
	epoch := m.acceptedOrderEpochForMemoryLocked(accepted.epochIdx, accepted.epochDur)
	m.insertEpochOrderLocked(epoch, ord)
	m.epochMtx.Unlock()
}

func (m *Market) acceptedOrderEpochForMemoryLocked(epochIdx, epochDur int64) *EpochQueue {
	if m.currentEpoch != nil && m.currentEpoch.Epoch == epochIdx {
		return m.currentEpoch
	}
	if m.nextEpoch != nil && m.nextEpoch.Epoch == epochIdx {
		return m.nextEpoch
	}

	panic(fmt.Sprintf("accepted order epoch %d is not current or next", epochIdx))
}

func idToBytes(id [order.OrderIDSize]byte) []byte {
	return id[:]
}

// respondError sends an rpcError to a user.
func (m *Market) respondError(id uint64, user account.AccountID, code int, errMsg string) {
	log.Debugf("sending error to user %v, code: %d, msg: %s", user, code, errMsg)
	msg, err := msgjson.NewResponse(id, nil, &msgjson.Error{
		Code:    code,
		Message: errMsg,
	})
	if err != nil {
		log.Errorf("error creating error response with message '%s': %v", msg, err)
	}
	if err := m.auth.Send(user, msg); err != nil {
		log.Infof("Failed to send %s error response (code = %d, msg = %s) to user %v: %v",
			msg.Route, code, errMsg, user, err)
	}
}

// preimage request-response handling data
type piData struct {
	ord      order.Order
	preimage chan *order.Preimage
}

// handlePreimageResp is to be used in the response callback function provided
// to AuthManager.Request for the preimage route.
func (m *Market) handlePreimageResp(msg *msgjson.Message, reqData *piData) {
	sendPI := func(pi *order.Preimage) {
		reqData.preimage <- pi
	}

	var piResp msgjson.PreimageResponse
	resp, err := msg.Response()
	if err != nil {
		sendPI(nil)
		m.respondError(msg.ID, reqData.ord.User(), msgjson.RPCParseError,
			fmt.Sprintf("error parsing preimage notification response: %v", err))
		return
	}
	if resp.Error != nil {
		log.Warnf("Client failed to handle preimage request: %v", resp.Error)
		sendPI(nil)
		return
	}
	err = json.Unmarshal(resp.Result, &piResp)
	if err != nil {
		sendPI(nil)
		m.respondError(msg.ID, reqData.ord.User(), msgjson.RPCParseError,
			fmt.Sprintf("error parsing preimage response payload result: %v", err))
		return
	}

	// Validate preimage length.
	if len(piResp.Preimage) != order.PreimageSize {
		sendPI(nil)
		m.respondError(msg.ID, reqData.ord.User(), msgjson.InvalidPreimage,
			fmt.Sprintf("invalid preimage length (%d byes)", len(piResp.Preimage)))
		return
	}

	// Check that the preimage is the hash of the order commitment.
	var pi order.Preimage
	copy(pi[:], piResp.Preimage)
	piCommit := pi.Commit()
	if reqData.ord.Commitment() != piCommit {
		sendPI(nil)
		oc := reqData.ord.Commitment()
		m.respondError(msg.ID, reqData.ord.User(), msgjson.PreimageCommitmentMismatch,
			fmt.Sprintf("preimage hash %x does not match order commitment %x",
				piCommit[:], oc[:]))
		return
	}

	// The preimage is good.
	log.Tracef("Good preimage received for order %v: %x", reqData.ord, pi)

	sendPI(&pi)
}

// collectPreimages solicits preimages from the owners of each of the orders in
// the provided queue with a 'preimage' ntfn/request via AuthManager.Request,
// and returns the preimages contained in the client responses. This function
// can block for up to the preimage deadline (piTimeout below) to allow clients
// time to respond. Clients that fail to respond, or respond with invalid data
// (see handlePreimageResp), are counted as misses.
func (m *Market) collectPreimages(orders []order.Order) (cSum []byte, ordersRevealed []*matcher.OrderRevealed, misses []order.Order) {
	// Compute the commitment checksum for the order queue.
	cSum = matcher.CSum(orders)

	// Two-thirds of an epoch, capped at 20s, so one unresponsive owner cannot
	// delay the close by a full epoch.
	piTimeout := min(20*time.Second, 2*time.Duration(m.EpochDuration())*time.Millisecond/3)
	preimages := make(map[order.Order]chan *order.Preimage, len(orders))
	for _, ord := range orders {
		// Make the 'preimage' request.
		commit := ord.Commitment()
		piReqParams := &msgjson.PreimageRequest{
			OrderID:        idToBytes(ord.ID()),
			Commitment:     commit[:],
			CommitChecksum: cSum,
		}
		req, err := msgjson.NewRequest(comms.NextID(), msgjson.PreimageRoute, piReqParams)
		if err != nil {
			// This is likely an impossible condition, and it is not the
			// client's fault, but the close must give every order a
			// disposition: an order that is neither revealed nor missed
			// would strand in epoch status. Count it as a miss.
			log.Errorf("error creating preimage request: %v", err)
			misses = append(misses, ord)
			continue
		}

		// The client's preimage response comes back via a channel, where nil
		// indicates client failure to respond, either due to disconnection or
		// no action.
		piChan := make(chan *order.Preimage, 1) // buffer so the link's in handler does not block

		reqData := &piData{
			ord:      ord,
			preimage: piChan,
		}

		// Failure to respond in time or an async link write error is a miss,
		// signalled by a nil pointer. Request errors returned by
		// RequestWithTimeout instead register a miss immediately.
		miss := func() { piChan <- nil }

		// Send the preimage request to the order's owner.
		err = m.auth.RequestWithTimeout(ord.User(), req, func(_ comms.Link, msg *msgjson.Message) {
			m.handlePreimageResp(msg, reqData) // sends on piChan
		}, piTimeout, miss)
		if err != nil {
			if errors.Is(err, ws.ErrPeerDisconnected) || errors.Is(err, auth.ErrUserNotConnected) {
				log.Debugf("Preimage request failed, client gone: %v", err)
			} else {
				// We may need a way to identify server connectivity problems so
				// clients are not penalized when it is not their fault. For
				// now, log this at warning level since the error is not novel.
				log.Warnf("Preimage request failed: %v", err)
			}

			// Register the miss now, no channel receive for this order.
			misses = append(misses, ord)
			continue
		}

		log.Tracef("Preimage request sent for order %v", ord)
		preimages[ord] = piChan
	}

	// Receive preimages from response channels.
	for ord, pic := range preimages {
		pi := <-pic
		if pi == nil {
			misses = append(misses, ord)
		} else {
			ordersRevealed = append(ordersRevealed, &matcher.OrderRevealed{
				Order:    ord,
				Preimage: *pi,
			})
		}
	}

	return
}

func (m *Market) enqueueEpoch(eq *epochPump, epoch *EpochQueue) bool {
	// Enqueue the epoch for matching when preimage collection is completed and
	// it is this epoch's turn.
	rq := eq.Insert(epoch)
	if rq == nil {
		// should not happen if cycleEpoch considers when the halt began.
		log.Errorf("failed to enqueue an epoch into a halted epoch pump")
		return false
	}

	orders := epoch.OrderSlice()

	// Start preimage collection.
	go func() {
		cSum, ordersRevealed, misses := m.collectPreimages(orders)
		if len(orders) > 0 {
			log.Infof("Collected %d valid order preimages, missed %d. Commit checksum: %x",
				len(ordersRevealed), len(misses), cSum)
		}
		missRevokeTime := time.Now().Truncate(time.Millisecond).UTC()
		rq.complete(cSum, ordersRevealed, misses, missRevokeTime)
	}()

	return true
}

func sortOrdersByID(ords []order.Order) {
	sort.Slice(ords, func(i, j int) bool {
		idi, idj := ords[i].ID(), ords[j].ID()
		return bytes.Compare(idi[:], idj[:]) < 0
	})
}

func ordersInEpoch(ords []order.Order, epochIdx, epochDur int64) []order.Order {
	epoch := NewEpoch(epochIdx, epochDur)
	filtered := make([]order.Order, 0, len(ords))
	for _, ord := range ords {
		if ord == nil {
			continue
		}
		if epoch.IncludesTime(time.UnixMilli(ord.Time())) {
			filtered = append(filtered, ord)
		}
	}
	sortOrdersByID(filtered)
	return filtered
}

func orderIDs(ords []order.Order) []order.OrderID {
	ids := make([]order.OrderID, 0, len(ords))
	for _, ord := range ords {
		ids = append(ids, ord.ID())
	}
	return ids
}

// finalCloseMatchesPendingSuspendLocked reports whether an advance_epoch final
// close (OpenedEpochIdx == 0) matches the market's pending suspend. m.epochMtx
// must be held.
func (m *Market) finalCloseMatchesPendingSuspendLocked(event *meshevents.AdvanceEpochEvent) bool {
	return m.pendingLifecycleAction == db.MarketPendingSuspend &&
		m.pendingLifecycleEpochIdx == event.ClosedEpochIdx &&
		m.pendingLifecycleEpochDur == event.EpochDur
}

func (m *Market) validateAdvanceEpochState(event *meshevents.AdvanceEpochEvent) ([]order.Order, error) {
	m.epochMtx.RLock()
	defer m.epochMtx.RUnlock()

	if m.currentEpoch == nil {
		return nil, fmt.Errorf("advance_epoch with no active epoch on market %s", m.name)
	}
	if m.currentEpoch.Epoch != event.ClosedEpochIdx {
		return nil, fmt.Errorf("advance_epoch closed epoch %d does not match current epoch %d",
			event.ClosedEpochIdx, m.currentEpoch.Epoch)
	}
	if event.OpenedEpochIdx == 0 {
		if !m.finalCloseMatchesPendingSuspendLocked(event) {
			return nil, fmt.Errorf("advance_epoch final close without matching pending suspend")
		}
	} else if m.nextEpoch != nil && m.nextEpoch.Epoch != event.OpenedEpochIdx {
		return nil, fmt.Errorf("advance_epoch opened epoch %d does not match next epoch %d",
			event.OpenedEpochIdx, m.nextEpoch.Epoch)
	}
	closedOrders := m.currentEpoch.OrderSlice()
	sortOrdersByID(closedOrders)
	for _, ord := range closedOrders {
		oid := ord.ID()
		if _, found := m.epochOrders[oid]; !found {
			return nil, fmt.Errorf("advance_epoch closed order %v is not in epoch memory", oid)
		}
	}
	return closedOrders, nil
}

// insertEpochOrderLocked inserts an order into the given epoch queue and the
// market's epoch index maps. m.epochMtx must be held for writing.
func (m *Market) insertEpochOrderLocked(epoch *EpochQueue, ord order.Order) {
	epoch.Insert(ord)
	m.epochOrders[ord.ID()] = ord
	m.epochCommitments[ord.Commitment()] = ord.ID()
}

// lockEpochOrderCoins locks the funding coins of orders reconstructed into
// epoch memory from durable storage, rolling back every lock if any fails.
func (m *Market) lockEpochOrderCoins(ords []order.Order) error {
	var locked []order.Order
	for _, ord := range ords {
		if !m.lockOrderCoins(ord) {
			for _, lockedOrd := range locked {
				m.unlockOrderCoins(lockedOrd)
			}
			return fmt.Errorf("failed to lock epoch order %v coins", ord.ID())
		}
		locked = append(locked, ord)
	}
	return nil
}

// applyAdvanceEpochEvent advances epoch memory after an advance_epoch event.
func (m *Market) applyAdvanceEpochEvent(event *meshevents.AdvanceEpochEvent, closedOrders []order.Order) error {
	m.bookMtx.Lock()
	if event.OpenedEpochIdx == 0 {
		// Final close: no next epoch. Leave the last opened book epoch in place.
	} else if event.OpenedEpochIdx <= m.bookEpochIdx {
		log.Errorf("market %s advance_epoch opened %d but book epoch is already %d",
			m.name, event.OpenedEpochIdx, m.bookEpochIdx)
	} else {
		m.bookEpochIdx = event.OpenedEpochIdx
	}
	m.bookMtx.Unlock()

	m.epochMtx.Lock()
	for _, ord := range closedOrders {
		// Closed orders leave epoch memory but keep their funding coin locks:
		// the epoch_processed apply unlocks misses and non-booked outcomes,
		// and if processing never happens because of a crash, the next
		// startup's market_started epoch revokes dispose of them and unlock
		// their coins explicitly.
		oid := ord.ID()
		delete(m.epochOrders, oid)
		delete(m.epochCommitments, ord.Commitment())
	}

	if event.OpenedEpochIdx == 0 {
		m.currentEpoch = nil
		m.nextEpoch = nil
		m.activeEpochIdx = 0
		m.pendingLifecycleAction = db.MarketPendingSuspendDrain
		m.epochMtx.Unlock()
		m.running.Store(false)
		m.wakeLifecycleDriver()
		return nil
	}
	if m.nextEpoch != nil && m.nextEpoch.Epoch == event.OpenedEpochIdx {
		m.currentEpoch = m.nextEpoch
	} else {
		m.currentEpoch = NewEpoch(event.OpenedEpochIdx, event.EpochDur)
	}
	m.nextEpoch = NewEpoch(event.OpenedEpochIdx+1, event.EpochDur)
	m.activeEpochIdx = event.OpenedEpochIdx
	acceptOrders := m.lifecycleState == db.MarketStateRunning &&
		m.pendingLifecycleAction != db.MarketPendingSuspendDrain
	m.epochMtx.Unlock()
	if acceptOrders {
		m.running.Store(true)
	}
	return nil
}

func (m *Market) sendRevokeOrderNote(oid order.OrderID, user account.AccountID) {
	// Send revoke_order notification to order owner.
	route := msgjson.RevokeOrderRoute
	log.Infof("Sending a '%s' notification to %v for order %v", route, user, oid)
	revMsg := &msgjson.RevokeOrder{
		OrderID: oid.Bytes(),
	}
	m.auth.Sign(revMsg)
	revNtfn, err := msgjson.NewNotification(route, revMsg)
	if err != nil {
		log.Errorf("Failed to create %s notification for order %v: %v", route, oid, err)
	} else {
		if err = m.auth.SendIfLocal(user, revNtfn); err != nil {
			log.Debugf("Failed to send %s notification to user %v: %v", route, user, err)
		}
	}
}

// sendNoMatchNote sends the nomatch notification to an order owner. Epoch
// cancels disposed of by market_started fail exactly like an unmatched
// cancel, so their owners get the same note the normal epoch pipeline sends.
func (m *Market) sendNoMatchNote(oid order.OrderID, user account.AccountID) {
	msg, err := noMatchMessage(oid)
	if err != nil {
		log.Errorf("Failed to create nomatch notification for order %v: %v", oid, err)
		return
	}
	if err := m.auth.SendIfLocal(user, msg); err != nil {
		log.Debugf("Failed to send nomatch notification to user %v: %v", user, err)
	}
}

// sendPenaltyNote sends a signed penalty notification to the order owner
// after their booked orders were revoked because their tier dropped below the
// trading threshold. The timestamp comes from the orders_revoked event so
// every node builds an identical note; SendIfLocal delivers it only from the
// node hosting the user's connection.
func (m *Market) sendPenaltyNote(user account.AccountID, penaltyTime time.Time) {
	penaltyNote := &msgjson.PenaltyNote{
		Penalty: &msgjson.Penalty{
			Rule: account.NoRule,
			Time: uint64(penaltyTime.UnixMilli()),
			Details: "Ordering has been suspended for this account. " +
				"Post additional bond to offset violations.",
		},
	}
	m.auth.Sign(penaltyNote)
	note, err := msgjson.NewNotification(msgjson.PenaltyRoute, penaltyNote)
	if err != nil {
		log.Errorf("Failed to create penalty notification for user %v: %v", user, err)
		return
	}
	if err := m.auth.SendIfLocal(user, note); err != nil {
		log.Debugf("Failed to send penalty notification to user %v: %v", user, err)
	}
}

// applyOrderRevokedMemory applies the in-memory projection of an
// orders_revoked event target after the DB event transaction has recorded the
// durable revocation: the order is removed from the book and settling
// tracking, its funding coins are unlocked, the owner is sent a revoke_order
// notification, and orderbook subscribers are sent an unbook_order
// notification.
func (m *Market) applyOrderRevokedMemory(lo *order.LimitOrder) {
	oid := lo.ID()
	m.bookMtx.Lock()
	_, removed := m.book.Remove(oid)
	delete(m.settling, oid) // no order completion credit in SwapDone for revoked orders
	m.bookMtx.Unlock()

	m.unlockOrderCoins(lo)

	if !removed {
		log.Errorf("orders_revoked target %v was not on the %s book", oid, m.name)
		return
	}

	m.sendRevokeOrderNote(oid, lo.User())
	m.notifyUnbooked(lo)
}

// getFeeRate gets the fee rate for an asset.
func (m *Market) getFeeRate(assetID uint32, f FeeFetcher) uint64 {
	// Do not block indefinitely waiting for fetcher.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rate := f.SwapFeeRate(ctx)
	if ctx.Err() != nil { // timeout, try last known rate
		rate = f.LastRate()
		log.Warnf("Failed to get latest fee rate for %v. Using last known rate %d.",
			dex.BipIDSymbol(assetID), rate)
	}
	rate = m.ScaleFeeRate(assetID, rate)
	if rate > f.MaxFeeRate() || rate == 0 {
		rate = f.MaxFeeRate()
	}
	return rate
}

type epochProcessedResult struct {
	seed        []byte
	matches     []*order.MatchSet
	revealed    []*matcher.OrderRevealed
	misses      []order.Order
	failed      []*matcher.OrderRevealed
	doneOK      []*matcher.OrderRevealed
	partial     []*matcher.OrderRevealed
	booked      []*matcher.OrderRevealed
	nomatched   []*matcher.OrderRevealed
	unbooked    []*order.LimitOrder
	updates     *matcher.OrdersUpdated
	stats       *matcher.MatchCycleStats
	spot        *msgjson.Spot
	matchReport [][2]int64

	dbUpdate       *db.EpochProcessedUpdate
	dbLog          *db.EventLogEntry
	updateLastRate bool
}

// processReadyEpoch publishes the authoritative epoch_processed event for a
// closed epoch whose preimage collection has completed. A returned error
// means the epoch could not be closed and the market run must stop.
func (m *Market) processReadyEpoch(ctx context.Context, epoch *readyEpoch) error {
	// Ensure the epoch has actually completed preimage collection. This can
	// only fail if the epochPump malfunctioned.
	select {
	case <-epoch.ready:
	default:
		return fmt.Errorf("preimage collection not yet complete for epoch %d", epoch.Epoch)
	}

	// Abort epoch processing if there was a fatal DB backend error.
	if err := m.storage.LastErr(); err != nil {
		return fmt.Errorf("aborting epoch processing on account of failing DB: %w", err)
	}

	// Get the base and quote fee rates.
	// NOTE: We might consider moving this before the match cycle and abandoning
	// the match cycle when no fee rate can be found (on mainnet). The only
	// hesitation there is that it makes certain maintenance tasks longer but
	// that's not unexpected in the world of cryptocurrency exchanges. It also
	// makes it harder to fire up a private DEX server to conduct a private
	// trade, but even in that case, I wouldn't want to be matching at the
	// fallback MaxFeeRate when the justifiable network rate is much lower. I do
	// remember some minor discussion of this at some point in the past, but I'd
	// like to bring it back.
	feeRateBase := m.getFeeRate(m.Base(), m.baseFeeFetcher)
	feeRateQuote := m.getFeeRate(m.Quote(), m.quoteFeeFetcher)
	matchTime := time.Now().Truncate(time.Millisecond).UTC()

	event, err := mesh.NewEvent(meshevents.NewEpochProcessedEvent(m.name, epoch.Epoch, epoch.Duration,
		matchTime, feeRateBase, feeRateQuote, m.lastRate, epoch.cSum, epoch.ordersRevealed, epoch.misses,
		epoch.missRevokeTime))
	if err != nil {
		return fmt.Errorf("failed to build epoch processed event for epoch %d: %w", epoch.Epoch, err)
	}
	res, err := m.mesh.ApplyEvent(ctx, event)
	if err != nil {
		return fmt.Errorf("failed to apply epoch processed event for epoch %d: %w", epoch.Epoch, err)
	}

	// The applier records the epoch-processed result on the apply context, and
	// ApplyEvent returns it here; the emitting master hands the new matches to
	// the swapper. A replicated apply on a slave runs the same applier but no
	// one consumes the result.
	result, _ := res.(*epochProcessedResult)
	if result == nil || len(result.matches) == 0 {
		return nil
	}

	log.Debugf("Negotiating %d matches for epoch %d:%d", len(result.matches),
		epoch.Epoch, epoch.Duration)
	m.swapper.RequestMatchAcks(result.matches)
	return nil
}

func (m *Market) applyEpochProcessedEvent(applyCtx *mesh.EventApplyContext, event *meshevents.EpochProcessedEvent, raw *mesh.Event) (*epochProcessedResult, error) {
	m.bookMtx.Lock()
	result, err := m.buildEpochProcessedUpdate(event)
	if err != nil {
		m.bookMtx.Unlock()
		return nil, err
	}
	dbLog, err := m.storage.ApplyEpochProcessedEvent(
		applyCtx, dbEventLogMeta(applyCtx.Position, raw), m.auth.ReputationOutcomePolicy(), result.dbUpdate)
	if err != nil {
		m.bookMtx.Unlock()
		return nil, err
	}
	result.dbLog = dbLog
	m.applyEpochProcessedBookMutationLocked(event, result)
	m.bookMtx.Unlock()
	m.epochMtx.Lock()
	if event.EpochIdx > m.processedEpochIdx {
		m.processedEpochIdx = event.EpochIdx
	}
	m.epochMtx.Unlock()
	m.wakeClosureWaiter()
	m.finalizePreimageMisses(result.misses)
	if err := m.applyEpochProcessedProjection(uint64(event.EpochIdx), result); err != nil {
		return nil, &mesh.CommittedEventApplyError{
			Applied: result.dbLog,
			Err:     err,
		}
	}
	return result, nil
}

// finalizePreimageMisses does the in-memory half of preimage-miss handling
// after the epoch_processed DB apply revoked the misses: release each miss's
// funding coins and tell its owner.
func (m *Market) finalizePreimageMisses(misses []order.Order) {
	for _, ord := range misses {
		oid, user := ord.ID(), ord.User()
		log.Infof("No preimage received for order %v from user %v. Recording violation and revoking order.",
			oid, user)
		m.unlockOrderCoins(ord)
		go m.sendRevokeOrderNote(oid, user)
	}
}

func cloneOrderForMatching(ord order.Order) (order.Order, error) {
	switch o := ord.(type) {
	case *order.LimitOrder:
		return &order.LimitOrder{
			P:     o.P,
			T:     *o.T.Copy(),
			Rate:  o.Rate,
			Force: o.Force,
		}, nil
	case *order.MarketOrder:
		return &order.MarketOrder{
			P: o.P,
			T: *o.T.Copy(),
		}, nil
	case *order.CancelOrder:
		return &order.CancelOrder{
			P:             o.P,
			TargetOrderID: o.TargetOrderID,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported order type %T", ord)
	}
}

func cloneLimitOrderForMatching(ord *order.LimitOrder) (*order.LimitOrder, error) {
	cpy, err := cloneOrderForMatching(ord)
	if err != nil {
		return nil, err
	}
	lo, ok := cpy.(*order.LimitOrder)
	if !ok {
		return nil, fmt.Errorf("cloned limit order %v as %T", ord.ID(), cpy)
	}
	return lo, nil
}

func cloneRevealedOrdersForMatching(revealed []*matcher.OrderRevealed) ([]*matcher.OrderRevealed, error) {
	cpy := make([]*matcher.OrderRevealed, 0, len(revealed))
	for _, or := range revealed {
		if or == nil {
			return nil, fmt.Errorf("nil revealed order")
		}
		ord, err := cloneOrderForMatching(or.Order)
		if err != nil {
			return nil, err
		}
		cpy = append(cpy, &matcher.OrderRevealed{
			Order:    ord,
			Preimage: or.Preimage,
		})
	}
	return cpy, nil
}

func (m *Market) matchingBookSnapshotLocked() (*book.Book, error) {
	shadow := book.New(m.LotSize(), 0)
	for _, side := range [][]*order.LimitOrder{m.book.BuyOrders(), m.book.SellOrders()} {
		for _, ord := range side {
			cpy, err := cloneLimitOrderForMatching(ord)
			if err != nil {
				return nil, err
			}
			if !shadow.Insert(cpy) {
				return nil, fmt.Errorf("failed to insert cloned book order %v", ord.ID())
			}
		}
	}
	return shadow, nil
}

func stampEpochMatchSets(event *meshevents.EpochProcessedEvent, matches []*order.MatchSet) {
	for _, ms := range matches {
		ms.Epoch.Idx = uint64(event.EpochIdx)
		ms.Epoch.Dur = uint64(event.EpochDur)
		ms.FeeRateBase = event.FeeRateBase
		ms.FeeRateQuote = event.FeeRateQuote
	}
}

func applyEpochStatsLastRate(stats *matcher.MatchCycleStats, lastRate uint64) {
	if stats.EndRate != 0 {
		return
	}
	stats.EndRate = lastRate
	stats.StartRate = lastRate
	stats.HighRate = lastRate
	stats.LowRate = lastRate
}

func epochMatchReport(matches []*order.MatchSet) [][2]int64 {
	matchReport := make([][2]int64, 0, len(matches))
	var lastRate uint64
	var lastSide bool
	for _, matchSet := range matches {
		for _, match := range matchSet.Matches() {
			t := match.Taker.Trade()
			if t == nil {
				continue
			}
			if match.Rate != lastRate || t.Sell != lastSide {
				matchReport = append(matchReport, [2]int64{int64(match.Rate), 0})
				lastRate, lastSide = match.Rate, t.Sell
			}
			if t.Sell {
				matchReport[len(matchReport)-1][1] += int64(match.Quantity)
			} else {
				matchReport[len(matchReport)-1][1] -= int64(match.Quantity)
			}
		}
	}
	return matchReport
}

func (m *Market) buildEpochProcessedUpdate(event *meshevents.EpochProcessedEvent) (*epochProcessedResult, error) {
	ordersRevealed, err := event.OrdersRevealed()
	if err != nil {
		return nil, err
	}
	misses, err := event.MissedOrders()
	if err != nil {
		return nil, err
	}

	planningRevealed, err := cloneRevealedOrdersForMatching(ordersRevealed)
	if err != nil {
		return nil, err
	}
	shadowBook, err := m.matchingBookSnapshotLocked()
	if err != nil {
		return nil, err
	}
	seed, matches, _, failed, doneOK, partial, booked, nomatched, unbooked, updates, stats := m.matcher.Match(shadowBook, planningRevealed)
	stampEpochMatchSets(event, matches)

	if len(ordersRevealed) > 0 {
		log.Infof("Matching complete for market %v epoch %d:"+
			" %d matches (%d partial fills), %d completed OK (not booked),"+
			" %d booked, %d unbooked, %d failed",
			m.name, event.EpochIdx,
			len(matches), len(partial), len(doneOK),
			len(booked), len(unbooked), len(failed),
		)
	}

	// Store data in epochs table, including matchTime so that cancel execution
	// times can be obtained from the DB for cancellation rate computation.
	oidsRevealed := make([]order.OrderID, 0, len(planningRevealed))
	for _, or := range planningRevealed {
		oidsRevealed = append(oidsRevealed, or.Order.ID())
	}
	oidsMissed := make([]order.OrderID, 0, len(misses))
	for _, om := range misses {
		oidsMissed = append(oidsMissed, om.ID())
	}

	// If there were no matches, we need to persist that last rate from the last
	// match recorded.
	applyEpochStatsLastRate(stats, event.LastRate)

	matchTime := event.MatchTimeTime()
	epochResults := &db.EpochResults{
		MktBase:        m.base,
		MktQuote:       m.quote,
		Idx:            event.EpochIdx,
		Dur:            event.EpochDur,
		MatchTime:      matchTime.UnixMilli(),
		CSum:           event.CSum,
		Seed:           seed,
		OrdersRevealed: oidsRevealed,
		OrdersMissed:   oidsMissed,
		MatchVolume:    stats.MatchVolume,
		QuoteVolume:    stats.QuoteVolume,
		BookBuys:       stats.BookBuys,
		BookBuys5:      stats.BookBuys5,
		BookBuys25:     stats.BookBuys25,
		BookSells:      stats.BookSells,
		BookSells5:     stats.BookSells5,
		BookSells25:    stats.BookSells25,
		HighRate:       stats.HighRate,
		LowRate:        stats.LowRate,
		StartRate:      stats.StartRate,
		EndRate:        stats.EndRate,
	}

	dbMatches := make([]*order.Match, 0)
	for _, matchSet := range matches {
		dbMatches = append(dbMatches, matchSet.Matches()...)
	}

	missRevokeTime := event.MissRevokeTimeTime()
	missUpdates := make([]*db.PreimageMissUpdate, 0, len(misses))
	for _, ord := range misses {
		missUpdates = append(missUpdates, &db.PreimageMissUpdate{
			Order:      ord,
			RevokeTime: missRevokeTime,
		})
	}
	revealUpdates := make([]*db.PreimageRevealUpdate, 0, len(ordersRevealed))
	for _, or := range ordersRevealed {
		revealUpdates = append(revealUpdates, &db.PreimageRevealUpdate{
			Order:    or.Order,
			Preimage: or.Preimage,
		})
	}

	dbUpdate := &db.EpochProcessedUpdate{
		Epoch:           epochResults,
		Misses:          missUpdates,
		Reveals:         revealUpdates,
		TradesBooked:    updates.TradesBooked,
		TradesPartial:   updates.TradesPartial,
		TradesCompleted: updates.TradesCompleted,
		TradesCanceled:  updates.TradesCanceled,
		TradesFailed:    updates.TradesFailed,
		CancelsFailed:   updates.CancelsFailed,
		CancelsExecuted: updates.CancelsExecuted,
		Matches:         dbMatches,
	}

	return &epochProcessedResult{
		seed:           seed,
		matches:        matches,
		revealed:       ordersRevealed,
		misses:         misses,
		failed:         failed,
		doneOK:         doneOK,
		partial:        partial,
		booked:         booked,
		nomatched:      nomatched,
		unbooked:       unbooked,
		updates:        updates,
		stats:          stats,
		matchReport:    epochMatchReport(matches),
		dbUpdate:       dbUpdate,
		updateLastRate: stats.EndRate != event.LastRate || len(matches) > 0,
	}, nil
}

func matchSetsEqual(a, b []*order.MatchSet) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aSet, bSet := a[i], b[i]
		if aSet.Taker.ID() != bSet.Taker.ID() ||
			aSet.Epoch != bSet.Epoch ||
			aSet.FeeRateBase != bSet.FeeRateBase ||
			aSet.FeeRateQuote != bSet.FeeRateQuote ||
			aSet.Total != bSet.Total ||
			len(aSet.Makers) != len(bSet.Makers) ||
			len(aSet.Amounts) != len(bSet.Amounts) ||
			len(aSet.Rates) != len(bSet.Rates) {
			return false
		}
		for j := range aSet.Makers {
			if aSet.Makers[j].ID() != bSet.Makers[j].ID() {
				return false
			}
		}
		for j := range aSet.Amounts {
			if aSet.Amounts[j] != bSet.Amounts[j] {
				return false
			}
		}
		for j := range aSet.Rates {
			if aSet.Rates[j] != bSet.Rates[j] {
				return false
			}
		}
	}
	return true
}

func (m *Market) applyEpochProcessedBookMutationLocked(event *meshevents.EpochProcessedEvent, result *epochProcessedResult) {
	seed, matches, _, failed, doneOK, partial, booked, nomatched, unbooked, updates, stats := m.matcher.Match(m.book, result.revealed)
	if !bytes.Equal(seed, result.seed) {
		panic("live epoch match seed mismatch")
	}
	stampEpochMatchSets(event, matches)
	if !matchSetsEqual(matches, result.matches) {
		panic("live epoch match result mismatch")
	}
	applyEpochStatsLastRate(stats, event.LastRate)

	result.matches = matches
	result.failed = failed
	result.doneOK = doneOK
	result.partial = partial
	result.booked = booked
	result.nomatched = nomatched
	result.unbooked = unbooked
	result.updates = updates
	result.stats = stats
	result.matchReport = epochMatchReport(matches)

	canceled := make([]order.OrderID, 0)
	for _, ms := range matches {
		for _, match := range ms.Matches() {
			if co, ok := match.Taker.(*order.CancelOrder); ok {
				canceled = append(canceled, co.TargetOrderID)
				continue
			}
			m.settling[match.Taker.ID()] += match.Quantity
			m.settling[match.Maker.ID()] += match.Quantity
		}
	}
	for _, oid := range canceled {
		// There may still be swaps settling, but we don't care anymore because
		// there is no completion credit on a canceled order.
		delete(m.settling, oid)
	}
}

func (m *Market) applyEpochProcessedProjection(epochID uint64, result *epochProcessedResult) error {
	if result.updateLastRate {
		m.lastRate = result.stats.EndRate
	}

	// Unlock passed but not booked order (e.g. matched market and immediate
	// orders) coins were locked upon order receipt in processOrder and must be
	// unlocked now since they do not go on the book.
	for _, k := range result.doneOK {
		m.unlockOrderCoins(k.Order)
	}

	// Unlock unmatched (failed) order coins.
	for _, fo := range result.failed {
		m.unlockOrderCoins(fo.Order)
	}

	// Booked order coins were locked upon receipt by processOrder, and remain
	// locked until they are either: unbooked by a future match that completely
	// fills the order, unbooked by a matched cancel order, or (unimplemented)
	// unbooked by another Market mechanism such as client disconnect or ban.

	// Unlock unbooked order coins.
	for _, ubo := range result.unbooked {
		m.unlockOrderCoins(ubo)
	}

	// The matcher already removed these orders from the book, but make the final
	// state explicit for replay.
	for _, ord := range result.unbooked {
		m.bookMtx.Lock()
		m.book.Remove(ord.ID())
		m.bookMtx.Unlock()
	}

	// Update the API data collector.
	spot, err := m.dataCollector.ReportEpoch(m.Base(), m.Quote(), epochID, result.stats)
	if err != nil {
		log.Errorf("Error updating API data collector: %v", err)
	}
	result.spot = spot

	if len(result.matches) > 0 {
		if err := m.swapper.TrackMatches(result.matches); err != nil {
			return err
		}
	}
	return nil
}

// validateOrder uses db.ValidateOrder to ensure that the provided order is
// valid for the current market with epoch order status.
func (m *Market) validateOrder(ord order.Order) error {
	// First check the order commitment before bothering the Market's run loop.
	c0 := order.Commitment{}
	if ord.Commitment() == c0 {
		// Note that OrderID may not be valid if ServerTime has not been set.
		return ErrInvalidCommitment
	}

	if !db.ValidateOrder(ord, order.OrderStatusEpoch, &dex.MarketInfo{
		Base:    m.base,
		Quote:   m.quote,
		LotSize: m.LotSize(),
	}) {
		return ErrInvalidOrder // non-specific
	}

	if lo, is := ord.(*order.LimitOrder); is && lo.Rate < m.minimumRate() {
		return ErrInvalidRate
	}

	return nil
}

// orderResponse signs the order data and prepares the OrderResult to be sent to
// the client.
func (m *Market) orderResponse(oRecord *orderRecord) (*msgjson.Message, error) {
	// Add the server timestamp.
	stamp := uint64(oRecord.order.Time())
	oRecord.req.Stamp(stamp)

	// Sign the serialized order request.
	m.auth.Sign(oRecord.req)

	// Prepare the OrderResult, including the server signature and time stamp.
	oid := oRecord.order.ID()
	res := &msgjson.OrderResult{
		Sig:        oRecord.req.SigBytes(),
		OrderID:    oid[:],
		ServerTime: stamp,
	}

	// Encode the order response as a message for the client.
	return msgjson.NewResponse(oRecord.msgID, res, nil)
}

// SetFeeRateScale sets a swap fee scale factor for the given asset.
// SetFeeRateScale should be called regardless of whether the Market is
// suspended.
func (m *Market) SetFeeRateScale(assetID uint32, scale float64) {
	m.feeScalesMtx.Lock()
	switch assetID {
	case m.base:
		m.feeScales.base = scale
	case m.quote:
		m.feeScales.quote = scale
	default:
		log.Errorf("Unknown asset ID %d for market %d-%d",
			assetID, m.base, m.quote)
	}
	m.feeScalesMtx.Unlock()
}

// ScaleFeeRate scales the provided fee rate with the given asset's swap fee
// rate scale factor, which is 1.0 by default.
func (m *Market) ScaleFeeRate(assetID uint32, feeRate uint64) uint64 {
	if feeRate == 0 {
		return feeRate // no idea if this is sensible for any asset, but ok
	}
	var feeScale float64
	m.feeScalesMtx.RLock()
	switch assetID {
	case m.base:
		feeScale = m.feeScales.base
	default:
		feeScale = m.feeScales.quote
	}
	m.feeScalesMtx.RUnlock()
	if feeScale == 0 {
		return feeRate
	}
	if feeScale < 1 {
		log.Warnf("Using fee rate scale of %f < 1.0 for asset %d", feeScale, assetID)
	}
	// It started non-zero, so don't allow it to go to zero.
	return uint64(math.Max(1.0, math.Round(float64(feeRate)*feeScale)))
}

type accountStats struct {
	qty, lots uint64
	redeems   int
}

type accountCounter map[string]*accountStats

func (a accountCounter) add(addr string, qty, lots uint64, redeems int) {
	stats, found := a[addr]
	if !found {
		stats = new(accountStats)
		a[addr] = stats
	}
	stats.qty += qty
	stats.lots += lots
	stats.redeems += redeems
}

// SubscribeMMSnapshots subscribes or unsubscribes a user from per-epoch market
// making snapshots for this market.
func (m *Market) SubscribeMMSnapshots(user account.AccountID, unsub bool) {
	m.mmSnapshotMtx.Lock()
	if unsub {
		delete(m.mmSnapshotSubs, user)
		log.Debugf("User %v unsubscribed from MM snapshots for %s", user, m.name)
	} else {
		m.mmSnapshotSubs[user] = struct{}{}
		log.Debugf("User %v subscribed to MM snapshots for %s", user, m.name)
	}
	m.mmSnapshotMtx.Unlock()
}

// sendMMSnapshots builds and sends signed epoch snapshots to all MM snapshot
// subscribers after the book has been updated for the given epoch.
func (m *Market) sendMMSnapshots(epochIdx, epochDur int64) {
	m.mmSnapshotMtx.RLock()
	if len(m.mmSnapshotSubs) == 0 {
		m.mmSnapshotMtx.RUnlock()
		return
	}
	subs := make(map[account.AccountID]struct{}, len(m.mmSnapshotSubs))
	for acctID := range m.mmSnapshotSubs {
		subs[acctID] = struct{}{}
	}
	m.mmSnapshotMtx.RUnlock()

	// Hold bookMtx to get an atomic snapshot of the book state.
	m.bookMtx.Lock()
	bestBuy, bestSell := m.book.Best()
	var bestBuyRate, bestSellRate uint64
	if bestBuy != nil {
		bestBuyRate = bestBuy.Rate
	}
	if bestSell != nil {
		bestSellRate = bestSell.Rate
	}

	buyOrders := m.book.BuyOrders()
	sellOrders := m.book.SellOrders()
	m.bookMtx.Unlock()

	base, quote := m.Base(), m.Quote()
	mktID := m.name
	msgEpochIdx := uint64(epochIdx)
	msgEpochDur := uint64(epochDur)

	// Build per-account order lists in a single pass over the book.
	type acctOrders struct {
		buys, sells []msgjson.SnapOrder
	}
	acctMap := make(map[account.AccountID]*acctOrders, len(subs))
	for _, lo := range buyOrders {
		if _, ok := subs[lo.AccountID]; ok {
			ao := acctMap[lo.AccountID]
			if ao == nil {
				ao = &acctOrders{}
				acctMap[lo.AccountID] = ao
			}
			ao.buys = append(ao.buys, msgjson.SnapOrder{
				Rate: lo.Rate,
				Qty:  lo.Remaining(),
			})
		}
	}
	for _, lo := range sellOrders {
		if _, ok := subs[lo.AccountID]; ok {
			ao := acctMap[lo.AccountID]
			if ao == nil {
				ao = &acctOrders{}
				acctMap[lo.AccountID] = ao
			}
			ao.sells = append(ao.sells, msgjson.SnapOrder{
				Rate: lo.Rate,
				Qty:  lo.Remaining(),
			})
		}
	}

	for acctID := range subs {
		ao := acctMap[acctID]
		var buys, sells []msgjson.SnapOrder
		if ao != nil {
			buys, sells = ao.buys, ao.sells
		}

		// Sort by rate ascending for deterministic serialization.
		sort.Slice(buys, func(i, j int) bool { return buys[i].Rate < buys[j].Rate })
		sort.Slice(sells, func(i, j int) bool { return sells[i].Rate < sells[j].Rate })

		const maxSnapOrders = 1<<16 - 1 // uint16 max
		if len(buys) > maxSnapOrders {
			log.Warnf("Truncating %d buy orders to %d for snapshot", len(buys), maxSnapOrders)
			buys = buys[:maxSnapOrders]
		}
		if len(sells) > maxSnapOrders {
			log.Warnf("Truncating %d sell orders to %d for snapshot", len(sells), maxSnapOrders)
			sells = sells[:maxSnapOrders]
		}

		snap := &msgjson.MMEpochSnapshot{
			MarketID:   mktID,
			Base:       base,
			Quote:      quote,
			EpochIdx:   msgEpochIdx,
			EpochDur:   msgEpochDur,
			AccountID:  acctID[:],
			BuyOrders:  buys,
			SellOrders: sells,
			BestBuy:    bestBuyRate,
			BestSell:   bestSellRate,
		}
		m.auth.Sign(snap)

		msg, err := msgjson.NewNotification(msgjson.MMEpochSnapshotRoute, snap)
		if err != nil {
			log.Errorf("Failed to create mm_epoch_snapshot notification: %v", err)
			continue
		}
		if err := m.auth.Send(acctID, msg); err != nil {
			log.Debugf("Failed to send mm_epoch_snapshot to %v (removing subscription): %v", acctID, err)
			m.mmSnapshotMtx.Lock()
			delete(m.mmSnapshotSubs, acctID)
			m.mmSnapshotMtx.Unlock()
		}
	}
}
