// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/meter"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/matcher"
)

var (
	// The coin waiter will initially query for transaction data every
	// fastRecheckInterval, but will eventually taper to taperedRecheckInterval.
	fastRecheckInterval    = time.Second * 3
	taperedRecheckInterval = time.Second * 30
	// minBlockPeriod is the minimum delay between block-triggered
	// confirmation/inaction checks. This helps with limiting notification
	// bursts when blocks are generated closely together (e.g. in Ethereum
	// occasionally several blocks are generated in a single second).
	minBlockPeriod = time.Second * 10
)

func unixMsNow() time.Time {
	return time.Now().Truncate(time.Millisecond).UTC()
}

func makerTaker(isMaker bool) string {
	if isMaker {
		return "maker"
	}
	return "taker"
}

// AuthManager handles client-related actions, including authorization and
// communications.
type AuthManager interface {
	Route(string, func(account.AccountID, *msgjson.Message) *msgjson.Error)
	Auth(user account.AccountID, msg, sig []byte) error
	Sign(...msgjson.Signable)
	Send(account.AccountID, *msgjson.Message) error
	Request(account.AccountID, *msgjson.Message, func(comms.Link, *msgjson.Message)) error
	RequestWithTimeout(user account.AccountID, req *msgjson.Message, handlerFunc func(comms.Link, *msgjson.Message),
		expireTimeout time.Duration, expireFunc func()) error
	SwapSuccess(user account.AccountID, mmid db.MarketMatchID, value uint64, refTime time.Time)
	Inaction(user account.AccountID, misstep db.Outcome, mmid db.MarketMatchID, matchValue uint64, refTime time.Time, oid order.OrderID)
}

// Storage updates match data in what is presumably a database.
type Storage interface {
	db.SwapArchiver
	LastErr() error
	Fatal() <-chan struct{}
	Order(oid order.OrderID, base, quote uint32) (order.Order, order.OrderStatus, error)
	CancelOrder(*order.LimitOrder) error
	InsertMatch(match *order.Match) error
}

// swapStatus is information related to the completion or incompletion of each
// sequential step of the atomic swap negotiation process. Each user has their
// own swapStatus.
type swapStatus struct {
	// The asset to which the user broadcasts their swap transaction.
	swapAsset   uint32
	redeemAsset uint32

	swapSearching   uint32 // atomic
	redeemSearching uint32 // atomic

	mtx sync.RWMutex
	// The time that the swap coordinator sees the transaction.
	swapTime time.Time
	swap     *asset.Contract
	// The time that the transaction receives its SwapConf'th confirmation.
	swapConfirmed time.Time
	// The time that the swap coordinator sees the user's redemption
	// transaction.
	redeemTime time.Time
	redemption asset.Coin
}

// String satisfies the Stringer interface for pretty printing. The swapStatus
// RWMutex should be held for reads when using.
func (ss *swapStatus) String() string {
	return fmt.Sprintf("swapAsset: %d, redeemAsset: %d, swapTime: %v, swap: %v, swapConfirmed: %v, redeemTime: %v, redemption: %v",
		ss.swapAsset, ss.redeemAsset, ss.swapTime, ss.swap, ss.swapConfirmed, ss.redeemTime, ss.redemption)
}

func (ss *swapStatus) startSwapSearch() bool {
	return atomic.CompareAndSwapUint32(&ss.swapSearching, 0, 1)
}

func (ss *swapStatus) endSwapSearch() {
	atomic.StoreUint32(&ss.swapSearching, 0)
}

func (ss *swapStatus) startRedeemSearch() bool {
	return atomic.CompareAndSwapUint32(&ss.redeemSearching, 0, 1)
}

func (ss *swapStatus) endRedeemSearch() {
	atomic.StoreUint32(&ss.redeemSearching, 0)
}

func (ss *swapStatus) swapConfTime() time.Time {
	ss.mtx.RLock()
	defer ss.mtx.RUnlock()
	return ss.swapConfirmed
}

func (ss *swapStatus) contractState() (known, confirmed bool) {
	ss.mtx.RLock()
	defer ss.mtx.RUnlock()
	return ss.swap != nil, !ss.swapConfirmed.IsZero()
}

func (ss *swapStatus) redeemSeenTime() time.Time {
	ss.mtx.RLock()
	defer ss.mtx.RUnlock()
	return ss.redeemTime
}

// matchTracker embeds an order.Match and adds some data necessary for tracking
// the match negotiation.
type matchTracker struct {
	mtx sync.RWMutex // Match.Sigs and Match.Status
	*order.Match
	time        time.Time // the match request time, not epoch close
	matchTime   time.Time // epoch close time
	makerStatus *swapStatus
	takerStatus *swapStatus
	// Per-match swap addresses from each party's match acknowledgement.
	// Each match requires a unique swap address to prevent a counterparty
	// from presenting a single on-chain contract as fulfilling multiple
	// matches. This primarily protects the taker: without unique
	// addresses, a malicious maker could reuse one contract across
	// matches. If a party reuses their own address they only weaken
	// their own protection, so the server validates that an address is
	// provided and is valid for the asset, but does not enforce
	// uniqueness across matches.
	makerSwapAddr         string
	takerSwapAddr         string
	counterPartyAddrsSent bool
}

// expiredBy returns true if the lock time of either party's *known* swap is
// before the reference time e.g. time.Now().
func (mt *matchTracker) expiredBy(ref time.Time) bool {
	mSwap, tSwap := mt.makerStatus.swap, mt.takerStatus.swap
	return (tSwap != nil && tSwap.LockTime.Before(ref)) ||
		(mSwap != nil && mSwap.LockTime.Before(ref))
}

// A blockNotification is used internally when an asset.Backend reports a new
// block.
type blockNotification struct {
	time    time.Time
	assetID uint32
	err     error
}

// A stepActor is a structure holding information about one party of a match.
// stepActor is used with the stepInformation structure, which is used for
// sequencing swap negotiation.
type stepActor struct {
	user account.AccountID
	// swapAsset is the asset to which this actor broadcasts their swap tx.
	swapAsset uint32
	isMaker   bool
	order     order.Order
	// The swapStatus from the Match. Could be either the
	// (matchTracker).makerStatus or (matchTracker).takerStatus, depending on who
	// this actor is.
	status *swapStatus
}

// String satisfies the Stringer interface for pretty printing. The swapStatus
// RWMutex should be held for reads when using for a.status reads.
func (a stepActor) String() string {
	return fmt.Sprintf("user: %v, swapAsset: %v, isMaker: %v, order: %v, status: {%v}",
		a.user, a.swapAsset, a.isMaker, a.order, a.status)
}

// stepInformation holds information about the current state of the swap
// negotiation. A new stepInformation should be generated with (Swapper).step at
// every step of the negotiation process.
type stepInformation struct {
	match *matchTracker
	// The actor is the user info for the user who is expected to be broadcasting
	// a swap or redemption transaction next.
	actor stepActor
	// counterParty is the user that is not expected to be acting next.
	counterParty stepActor
	// asset is the asset backend for swapAsset.
	asset *asset.BackedAsset
	// isBaseAsset will be true if the current step involves a transaction on the
	// match market's base asset blockchain, false if on quote asset's blockchain.
	isBaseAsset bool
	step        order.MatchStatus
	nextStep    order.MatchStatus
	// checkVal holds the trade amount in units of the currently acting asset,
	// and is used to validate the swap transaction details.
	checkVal uint64
}

// SwapperAsset is a BackedAsset with an optional CoinLocker.
type SwapperAsset struct {
	*asset.BackedAsset
	Locker coinlock.CoinLocker // should be *coinlock.AssetCoinLocker
}

// Swapper handles order matches by handling authentication and inter-party
// communications between clients, or 'users'. The Swapper authenticates users
// (vua AuthManager) and validates transactions as they are reported.
type Swapper struct {
	// coins is a map to all the Asset information, including the asset backends,
	// used by this Swapper.
	coins map[uint32]*SwapperAsset
	// storage is a Database backend.
	storage Storage
	// authMgr is an AuthManager for client messaging and authentication.
	authMgr AuthManager
	// swapDone is callback for reporting a swap outcome.
	swapDone func(oid order.Order, match *order.Match, fail bool)

	// The matches maps and the contained matches are protected by the matchMtx.
	matchMtx    sync.RWMutex
	matches     map[order.MatchID]*matchTracker
	userMatches map[account.AccountID]map[order.MatchID]*matchTracker
	acctMatches map[uint32]map[string]map[order.MatchID]*matchTracker

	// activeCoinsMtx protects activeCoinIDs, matchCoinIDs,
	// activeSecretHashes, and matchSecretHashes. This is a separate lock
	// from matchMtx to avoid contention on the global match lock during
	// processInit's dedup checks.
	activeCoinsMtx sync.Mutex
	// activeCoinIDs maps composite keys of hex-encoded CoinID and contract
	// data to the match using them. The composite key (coinID:contract)
	// prevents the same on-chain contract from being used in multiple
	// matches simultaneously. Using a composite key instead of bare CoinID
	// is necessary because EVM assets batch multiple swaps into a single
	// transaction, sharing the same txHash (CoinID) across matches while
	// each swap has a unique contract (version + locator/secretHash).
	activeCoinIDs map[string]order.MatchID
	// matchCoinIDs is the reverse of activeCoinIDs: it maps match IDs to the
	// keys registered in activeCoinIDs for that match. This allows
	// O(1) cleanup in deleteMatch instead of iterating all activeCoinIDs.
	matchCoinIDs map[order.MatchID][]string
	// activeSecretHashes maps hex-encoded secret hashes to the match using
	// them. This prevents a maker from reusing the same secret hash across
	// multiple matches, which would allow them to grief takers (the taker's
	// client-side dedup would reject the audit, penalizing the taker).
	activeSecretHashes map[string]order.MatchID
	// matchSecretHashes is the reverse map for O(1) cleanup in deleteMatch.
	matchSecretHashes map[order.MatchID][]string

	// The broadcast timeout.
	bTimeout time.Duration
	// txWaitExpiration is the longest the Swapper will wait for a coin waiter.
	txWaitExpiration time.Duration
	// Expected locktimes for maker and taker swaps.
	lockTimeTaker time.Duration
	lockTimeMaker time.Duration
	// latencyQ is a queue for coin waiters to deal with network latency.
	latencyQ *wait.TaperingTickerQueue

	// handlerMtx should be read-locked for the duration of the comms route
	// handlers (handleInit and handleRedeem) and Negotiate. This blocks
	// shutdown until any coin waiters are registered with latencyQ. It should
	// be write-locked before setting the stop flag.
	handlerMtx sync.RWMutex
	// stop is used to prevent new handlers from starting coin waiters. It is
	// set to true during shutdown of Run.
	stop bool
}

// Config is the swapper configuration settings. A Config instance is the only
// argument to the Swapper constructor.
type Config struct {
	// Assets is a map to all the asset information, including the asset backends,
	// used by this Swapper.
	Assets map[uint32]*SwapperAsset
	// AuthManager is the auth manager for client messaging and authentication.
	AuthManager AuthManager
	// A database backend.
	Storage Storage
	// BroadcastTimeout is how long the Swapper will wait for expected swap
	// transactions following new blocks.
	BroadcastTimeout time.Duration
	// TxWaitExpiration is the longest the Swapper will wait for a coin waiter.
	// This could be thought of as the maximum allowable backend latency.
	TxWaitExpiration time.Duration
	// LockTimeTaker is the locktime Swapper will use for auditing taker swaps.
	LockTimeTaker time.Duration
	// LockTimeMaker is the locktime Swapper will use for auditing maker swaps.
	LockTimeMaker time.Duration
	// NoResume indicates that the swapper should not resume active swaps.
	NoResume bool
	// AllowPartialRestore indicates if it is acceptable to load only some of
	// the active swaps if the Swapper's asset configuration lacks assets
	// required to load them all.
	AllowPartialRestore bool
	// SwapDone registers a match with the DEX manager (or other consumer) for a
	// given order as being finished.
	SwapDone func(oid order.Order, match *order.Match, fail bool)
}

// NewSwapper is a constructor for a Swapper.
func NewSwapper(cfg *Config) (*Swapper, error) {
	for _, asset := range cfg.Assets {
		if asset.MaxFeeRate == 0 {
			return nil, fmt.Errorf("max fee rate of 0 is invalid for asset %q", asset.Symbol)
		}
	}

	acctMatches := make(map[uint32]map[string]map[order.MatchID]*matchTracker)
	for _, a := range cfg.Assets {
		if _, ok := a.Backend.(asset.AccountBalancer); ok {
			acctMatches[a.ID] = make(map[string]map[order.MatchID]*matchTracker)
		}
	}

	authMgr := cfg.AuthManager
	swapper := &Swapper{
		coins:              cfg.Assets,
		storage:            cfg.Storage,
		authMgr:            authMgr,
		swapDone:           cfg.SwapDone,
		latencyQ:           wait.NewTaperingTickerQueue(fastRecheckInterval, taperedRecheckInterval),
		matches:            make(map[order.MatchID]*matchTracker),
		userMatches:        make(map[account.AccountID]map[order.MatchID]*matchTracker),
		acctMatches:        acctMatches,
		activeCoinIDs:      make(map[string]order.MatchID),
		matchCoinIDs:       make(map[order.MatchID][]string),
		activeSecretHashes: make(map[string]order.MatchID),
		matchSecretHashes:  make(map[order.MatchID][]string),
		bTimeout:           cfg.BroadcastTimeout,
		txWaitExpiration:   cfg.TxWaitExpiration,
		lockTimeTaker:      cfg.LockTimeTaker,
		lockTimeMaker:      cfg.LockTimeMaker,
	}

	// Ensure txWaitExpiration is not greater than broadcast timeout setting.
	if swapper.txWaitExpiration > swapper.bTimeout {
		swapper.txWaitExpiration = swapper.bTimeout
	}

	if !cfg.NoResume {
		err := swapper.restoreActiveSwaps(cfg.AllowPartialRestore)
		if err != nil {
			return nil, err
		}
	}

	// The swapper is only concerned with two types of client-originating
	// method requests.
	authMgr.Route(msgjson.InitRoute, swapper.handleInit)
	authMgr.Route(msgjson.RedeemRoute, swapper.handleRedeem)

	return swapper, nil
}

// addMatch registers a match. The matchMtx must be locked.
func (s *Swapper) addMatch(mt *matchTracker) {
	mid := mt.ID()
	s.matches[mid] = mt

	// Add the match to both maker's and taker's match maps.
	maker, taker := mt.Maker.User(), mt.Taker.User()
	for _, user := range []account.AccountID{maker, taker} {
		userMatches, found := s.userMatches[user]
		if !found {
			s.userMatches[user] = map[order.MatchID]*matchTracker{
				mid: mt,
			}
		} else {
			userMatches[mid] = mt // may overwrite for self-match (ok)
		}
		if maker == taker {
			break
		}
	}

	addAcctMatch := func(matches map[string]map[order.MatchID]*matchTracker, acctAddr string, mt *matchTracker) {
		acctMatches := matches[acctAddr]
		if acctMatches == nil {
			acctMatches = make(map[order.MatchID]*matchTracker, 1)
			matches[acctAddr] = acctMatches
		}
		acctMatches[mt.ID()] = mt
	}

	if s.acctMatches[mt.Maker.Base()] != nil {
		acctMatches := s.acctMatches[mt.Maker.Base()]
		addAcctMatch(acctMatches, mt.Maker.BaseAccount(), mt)
		addAcctMatch(acctMatches, mt.Taker.Trade().BaseAccount(), mt)
	}
	if s.acctMatches[mt.Maker.Quote()] != nil {
		acctMatches := s.acctMatches[mt.Maker.Quote()]
		addAcctMatch(acctMatches, mt.Maker.QuoteAccount(), mt)
		addAcctMatch(acctMatches, mt.Taker.Trade().QuoteAccount(), mt)
	}
}

// deleteMatch unregisters a match. The matchMtx must be locked.
func (s *Swapper) deleteMatch(mt *matchTracker) {
	mid := mt.ID()
	delete(s.matches, mid)

	// Clean up dedup maps for this match's swap contracts.
	s.activeCoinsMtx.Lock()
	for _, coinID := range s.matchCoinIDs[mid] {
		delete(s.activeCoinIDs, coinID)
	}
	delete(s.matchCoinIDs, mid)
	for _, sh := range s.matchSecretHashes[mid] {
		delete(s.activeSecretHashes, sh)
	}
	delete(s.matchSecretHashes, mid)
	s.activeCoinsMtx.Unlock()

	// Unlock the maker and taker order coins. May be redundant if processBlock
	// confirmed both swaps, but premature/quick counterparty actions that
	// advance match status first prevent that.
	s.unlockOrderCoins(mt.Maker)
	s.unlockOrderCoins(mt.Taker)

	// Remove the match from both maker's and taker's match maps.
	maker, taker := mt.Maker.User(), mt.Taker.User()
	for _, user := range []account.AccountID{maker, taker} {
		userMatches, found := s.userMatches[user]
		if !found {
			// Should not happen if consistently using addMatch.
			log.Errorf("deleteMatch: No matches for user %v found!", user)
			continue
		}
		delete(userMatches, mid)
		if len(userMatches) == 0 {
			delete(s.userMatches, user)
		}
		if maker == taker {
			break
		}
	}

	deleteAcctMatch := func(matches map[string]map[order.MatchID]*matchTracker, acctAddr string, mt *matchTracker) {
		acctMatches := matches[acctAddr]
		if acctMatches == nil {
			return
		}
		delete(acctMatches, mt.ID())
		if len(acctMatches) == 0 {
			delete(matches, acctAddr)
		}
	}

	if s.acctMatches[mt.Maker.Base()] != nil {
		acctMatches := s.acctMatches[mt.Maker.Base()]
		deleteAcctMatch(acctMatches, mt.Maker.BaseAccount(), mt)
		deleteAcctMatch(acctMatches, mt.Taker.Trade().BaseAccount(), mt)
	}
	if s.acctMatches[mt.Maker.Quote()] != nil {
		acctMatches := s.acctMatches[mt.Maker.Quote()]
		deleteAcctMatch(acctMatches, mt.Maker.QuoteAccount(), mt)
		deleteAcctMatch(acctMatches, mt.Taker.Trade().QuoteAccount(), mt)
	}
}

// UnsettledQuantity sums up the settling quantity per market for a user. Part
// of the market.MatchSwapper interface.
func (s *Swapper) UnsettledQuantity(user account.AccountID) map[[2]uint32]uint64 {
	s.matchMtx.RLock()
	defer s.matchMtx.RUnlock()
	marketQuantities := make(map[[2]uint32]uint64)
	userMatches, found := s.userMatches[user]
	if !found {
		return marketQuantities
	}
	for _, mt := range userMatches {
		mt.mtx.RLock()
		matchStatus := mt.Status
		mt.mtx.RUnlock()
		if mt.Maker.AccountID == user {
			if matchStatus >= order.MakerRedeemed {
				continue
			}
		} else if matchStatus >= order.TakerSwapCast {
			continue
		}
		mktID := [2]uint32{mt.Maker.BaseAsset, mt.Maker.QuoteAsset}
		marketQuantities[mktID] += mt.Quantity
	}
	return marketQuantities
}

// pendingAccountStats is used to sum in-process match stats for the
// AccountStats method.
type pendingAccountStats struct {
	acctAddr string
	assetID  uint32
	swaps    uint64
	qty      uint64
	redeems  int
}

func newPendingAccountStats(acctAddr string, assetID uint32) *pendingAccountStats {
	return &pendingAccountStats{
		acctAddr: acctAddr,
		assetID:  assetID,
	}
}

func (p *pendingAccountStats) addMatch(mt *matchTracker) {
	p.addOrder(mt, mt.Maker, order.MakerSwapCast, order.MakerRedeemed)
	p.addOrder(mt, mt.Taker, order.TakerSwapCast, order.MatchComplete)
}

func (p *pendingAccountStats) addOrder(mt *matchTracker, ord order.Order, swappedStatus, redeemedStatus order.MatchStatus) {
	trade := ord.Trade()
	if ord.Base() == p.assetID && trade.BaseAccount() == p.acctAddr {
		if trade.Sell {
			if mt.Status < swappedStatus {
				p.qty += mt.Quantity
				p.swaps++
			}
		} else if mt.Status < redeemedStatus {
			p.redeems++
		}
	}
	if ord.Quote() == p.assetID && trade.QuoteAccount() == p.acctAddr {
		if !trade.Sell {
			if mt.Status < swappedStatus {
				p.qty += calc.BaseToQuote(mt.Rate, mt.Quantity)
				p.swaps++ // The swap is expected to occur in 1 transaction.
			}
		} else if mt.Status < redeemedStatus {
			p.redeems++
		}
	}
}

// AccountStats is part of the MatchNegotiator interface to report in-process
// match information for a asset account address.
func (s *Swapper) AccountStats(acctAddr string, assetID uint32) (qty, swaps uint64, redeems int) {
	stats := newPendingAccountStats(acctAddr, assetID)
	s.matchMtx.RLock()
	defer s.matchMtx.RUnlock()
	acctMatches := s.acctMatches[assetID]
	if acctMatches == nil {
		return // How?
	}
	for _, mt := range acctMatches[acctAddr] {
		stats.addMatch(mt)
	}
	return stats.qty, stats.swaps, stats.redeems
}

// ChainsSynced will return true if both specified asset's backends are synced.
func (s *Swapper) ChainsSynced(base, quote uint32) (bool, error) {
	b, found := s.coins[base]
	if !found {
		return false, fmt.Errorf("no backend found for %d", base)
	}
	baseSynced, err := b.Backend.Synced()
	if err != nil {
		return false, fmt.Errorf("error checking sync status for %d: %w", base, err)
	}
	if !baseSynced {
		return false, nil
	}
	q, found := s.coins[quote]
	if !found {
		return false, fmt.Errorf("no backend found for %d", base)
	}
	quoteSynced, err := q.Backend.Synced()
	if err != nil {
		return false, fmt.Errorf("error checking sync status for %d: %w", quote, err)
	}
	return quoteSynced, nil
}

func (s *Swapper) restoreActiveSwaps(allowPartial bool) error {
	// Load active swap data from DB.
	swapData, err := s.storage.ActiveSwaps()
	if err != nil {
		return err
	}
	log.Infof("Loaded swap data for %d active swaps.", len(swapData))
	if len(swapData) == 0 {
		return nil
	}

	// Check that the required assets backends are available.
	missingAssets := make(map[uint32]bool)
	checkAsset := func(id uint32) {
		if s.coins[id] == nil && !missingAssets[id] {
			log.Warnf("Unable to find backend for asset %d with active swaps.", id)
			missingAssets[id] = true
		}
	}
	for _, sd := range swapData {
		checkAsset(sd.Base)
		checkAsset(sd.Quote)
	}

	if len(missingAssets) > 0 && !allowPartial {
		return fmt.Errorf("missing backend for asset with active swaps")
	}

	// Load the matchTrackers, calling the Contract and Redemption asset.Backend
	// methods as needed.

	type swapStatusData struct {
		SwapAsset       uint32 // from market schema and takerSell bool
		RedeemAsset     uint32
		SwapTime        int64  // {a,b}ContractTime
		ContractCoinOut []byte // {a,b}ContractCoinID
		ContractScript  []byte // {a,b}Contract
		RedeemTime      int64  // {a,b}RedeemTime
		RedeemCoinIn    []byte // {a,b}aRedeemCoinID
		// SwapConfirmTime is not stored in the DB, so use time.Now() if the
		// contract has reached SwapConf.
	}

	translateSwapStatus := func(ss *swapStatus, ssd *swapStatusData, cpSwapCoin []byte) error {
		ss.swapAsset, ss.redeemAsset = ssd.SwapAsset, ssd.RedeemAsset

		swapCoin := ssd.ContractCoinOut
		if len(swapCoin) > 0 {
			assetID := ssd.SwapAsset
			swapAsset := s.coins[assetID]
			swap, err := swapAsset.Backend.Contract(swapCoin, ssd.ContractScript)
			if err != nil {
				return fmt.Errorf("unable to find swap out coin %x for asset %d: %w", swapCoin, assetID, err)
			}
			ss.swap = swap
			ss.swapTime = time.UnixMilli(ssd.SwapTime)

			swapConfs, err := swap.Confirmations(context.Background())
			if err != nil {
				log.Warnf("No swap confirmed time for %v: %v", swap, err)
			} else if swapConfs >= int64(swapAsset.SwapConf) {
				// We don't record the time at which we saw the block that got
				// the swap to SwapConf, so give the user extra time.
				ss.swapConfirmed = time.Now().UTC()
			}
		}

		if redeemCoin := ssd.RedeemCoinIn; len(redeemCoin) > 0 {
			assetID := ssd.RedeemAsset
			redeem, err := s.coins[assetID].Backend.Redemption(redeemCoin, cpSwapCoin, ssd.ContractScript)
			if err != nil {
				return fmt.Errorf("unable to find redeem in coin %x for asset %d: %w", redeemCoin, assetID, err)
			}
			ss.redemption = redeem
			ss.redeemTime = time.UnixMilli(ssd.RedeemTime)
		}

		return nil
	}

	s.matches = make(map[order.MatchID]*matchTracker, len(swapData))
	s.userMatches = make(map[account.AccountID]map[order.MatchID]*matchTracker)
	for _, sd := range swapData {
		if missingAssets[sd.Base] {
			log.Warnf("Dropping match %v with no backend available for base asset %d", sd.ID, sd.Base)
			continue
		}
		if missingAssets[sd.Quote] {
			log.Warnf("Dropping match %v with no backend available for quote asset %d", sd.ID, sd.Quote)
			continue
		}
		// Load the maker's order.LimitOrder and taker's order.Order. WARNING:
		// This is a different Order instance from whatever Market or other
		// subsystems might have. As such, the mutable fields or accessors of
		// mutable data should not be used.
		taker, _, err := s.storage.Order(sd.MatchData.Taker, sd.Base, sd.Quote)
		if err != nil {
			log.Errorf("Failed to load taker order: %v", err)
			continue
		}
		if taker.ID() != sd.MatchData.Taker {
			log.Errorf("Failed to load order %v, computed ID %v instead", sd.MatchData.Taker, taker.ID())
			continue
		}
		maker, _, err := s.storage.Order(sd.MatchData.Maker, sd.Base, sd.Quote)
		if err != nil {
			log.Errorf("Failed to load taker order: %v", err)
			continue
		}
		if maker.ID() != sd.MatchData.Maker {
			log.Errorf("Failed to load order %v, computed ID %v instead", sd.MatchData.Maker, maker.ID())
			continue
		}
		makerLO, ok := maker.(*order.LimitOrder)
		if !ok {
			log.Errorf("Maker order was not a limit order: %T", maker)
			continue
		}

		match := &order.Match{
			Taker:        taker,
			Maker:        makerLO,
			Quantity:     sd.Quantity,
			Rate:         sd.Rate,
			FeeRateBase:  sd.BaseRate,
			FeeRateQuote: sd.QuoteRate,
			Epoch:        sd.Epoch,
			Status:       sd.Status,
			Sigs: order.Signatures{ // not really needed
				MakerMatch:  sd.SwapData.SigMatchAckMaker,
				TakerMatch:  sd.SwapData.SigMatchAckTaker,
				MakerAudit:  sd.SwapData.ContractAAckSig,
				TakerAudit:  sd.SwapData.ContractBAckSig,
				TakerRedeem: sd.SwapData.RedeemAAckSig,
			},
		}

		mid := sd.MatchData.ID
		if mid != match.ID() { // serialization is order IDs, qty, and rate
			log.Errorf("Failed to load Match %v, computed ID %v instead", mid, match.ID())
			continue
		}

		// Check and skip matches for missing assets.
		makerSwapAsset, makerRedeemAsset := sd.Base, sd.Quote // maker selling -> their swap asset is base
		if sd.TakerSell {                                     // maker buying -> their swap asset is quote
			makerSwapAsset, makerRedeemAsset = sd.Quote, sd.Base
		}
		if missingAssets[makerSwapAsset] {
			log.Infof("Skipping match %v with missing asset %d backend", mid, makerSwapAsset)
			continue
		}
		if missingAssets[makerRedeemAsset] {
			log.Infof("Skipping match %v with missing asset %d backend", mid, makerRedeemAsset)
			continue
		}

		epochCloseTime := match.Epoch.End()
		mt := &matchTracker{
			Match:                 match,
			time:                  epochCloseTime.Add(time.Minute), // not quite, just be generous
			matchTime:             epochCloseTime,
			makerStatus:           &swapStatus{}, // populated by translateSwapStatus
			takerStatus:           &swapStatus{},
			makerSwapAddr:         sd.SwapData.MakerSwapAddr,
			takerSwapAddr:         sd.SwapData.TakerSwapAddr,
			counterPartyAddrsSent: sd.SwapData.MakerSwapAddr != "" && sd.SwapData.TakerSwapAddr != "",
		}

		makerStatus := &swapStatusData{
			SwapAsset:       makerSwapAsset,
			RedeemAsset:     makerRedeemAsset,
			SwapTime:        sd.SwapData.ContractATime,
			ContractCoinOut: sd.SwapData.ContractACoinID,
			ContractScript:  sd.SwapData.ContractA,
			RedeemTime:      sd.SwapData.RedeemATime,
			RedeemCoinIn:    sd.SwapData.RedeemACoinID,
		}
		takerStatus := &swapStatusData{
			SwapAsset:       makerRedeemAsset,
			RedeemAsset:     makerSwapAsset,
			SwapTime:        sd.SwapData.ContractBTime,
			ContractCoinOut: sd.SwapData.ContractBCoinID,
			ContractScript:  sd.SwapData.ContractB,
			RedeemTime:      sd.SwapData.RedeemBTime,
			RedeemCoinIn:    sd.SwapData.RedeemBCoinID,
		}

		if err := translateSwapStatus(mt.makerStatus, makerStatus, takerStatus.ContractCoinOut); err != nil {
			log.Errorf("Loading match %v failed: %v", mid, err)
			continue
		}
		if err := translateSwapStatus(mt.takerStatus, takerStatus, makerStatus.ContractCoinOut); err != nil {
			log.Errorf("Loading match %v failed: %v", mid, err)
			continue
		}

		log.Infof("Resuming swap %v in status %v", mid, mt.Status)
		s.addMatch(mt)

		// Register swap contracts in the dedup maps using composite
		// keys of CoinID and contract data.
		for _, cs := range []struct {
			coinOut  []byte
			contract []byte
		}{
			{makerStatus.ContractCoinOut, makerStatus.ContractScript},
			{takerStatus.ContractCoinOut, takerStatus.ContractScript},
		} {
			if len(cs.coinOut) > 0 {
				dedupKey := fmt.Sprintf("%x:%x", cs.coinOut, cs.contract)
				s.activeCoinIDs[dedupKey] = mid
				s.matchCoinIDs[mid] = append(s.matchCoinIDs[mid], dedupKey)
			}
		}

		// Register the maker's secret hash in the dedup maps.
		if mt.makerStatus.swap != nil && len(mt.makerStatus.swap.SecretHash) > 0 {
			secretHashHex := fmt.Sprintf("%x", mt.makerStatus.swap.SecretHash)
			s.activeSecretHashes[secretHashHex] = mid
			s.matchSecretHashes[mid] = append(s.matchSecretHashes[mid], secretHashHex)
		}
	}

	// Revoke pre-upgrade matches that lack per-match swap addresses
	// introduced in PerMatchAddrVersion. Matches at NewlyMatched or
	// MakerSwapCast cannot proceed without addresses, so revoke them
	// without fault. Matches at TakerSwapCast or later already have both
	// contracts on-chain and don't need the addresses to finish.
	//
	// NOTE: For EVM assets, surviving TakerSwapCast+ matches may still
	// have v0 contract data. The server's ETH backend only binds one
	// contract version, so verifying their redeem coins will fail unless
	// the operator uses evm-protocol-overrides.json to keep v0 active
	// until those swaps complete. See server/asset/eth/eth.go.
	var toRevoke []*matchTracker
	for _, mt := range s.matches {
		if mt.makerSwapAddr != "" || mt.takerSwapAddr != "" {
			continue
		}
		if mt.Status != order.NewlyMatched && mt.Status != order.MakerSwapCast {
			continue
		}
		toRevoke = append(toRevoke, mt)
	}
	for _, mt := range toRevoke {
		log.Infof("Revoking pre-upgrade match %v (status %v): no per-match swap addresses", mt.ID(), mt.Status)
		s.deleteMatch(mt)
		s.failMatch(mt, false, false) // no fault
	}

	// Live coin waiters are abandoned on Swapper shutdown. When a client
	// reconnects or their init request times out, they will resend it.

	return nil
}

// Run is the main Swapper loop. It's primary purpose is to update transaction
// confirmations when new blocks are mined, and to trigger inaction checks.
func (s *Swapper) Run(ctx context.Context) {
	// Permit internal cancel on anomaly such as storage failure.
	ctxMaster, cancel := context.WithCancel(ctx)

	// Graceful shutdown first allows active incoming messages to be handled,
	// blocks more incoming messages in the handler functions, stops the helper
	// goroutines (latency queue used by the handlers, and the block ntfn
	// receiver), and finally the main loop via the mainLoop channel.
	var wgHelpers, wgMain sync.WaitGroup
	ctxHelpers, cancelHelpers := context.WithCancel(context.Background())
	mainLoop := make(chan struct{}) // close after helpers stop for graceful shutdown
	defer func() {
		// Stop handlers receiving messages and queueing latency Waiters.
		s.handlerMtx.Lock() // block until active handlers return
		s.stop = true       // prevent new handlers from starting waiters
		// NOTE: could also do authMgr.Route(msgjson.{InitRoute,RedeemRoute}, shuttingDownHandler)
		s.handlerMtx.Unlock()

		// Stop the latencyQ of Waiters and the block update goroutines that
		// send to the main loop.
		cancelHelpers()
		wgHelpers.Wait()

		// Now that handlers AND the coin waiter queue are stopped, the
		// liveWaiters can be accessed without locking.

		// Stop the main loop if there was no internal error.
		close(mainLoop)
		wgMain.Wait()
	}()

	// Start a listen loop for each asset's block channel. Normal shutdown stops
	// this before the main loop since this sends to the main loop.
	blockNotes := make(chan *blockNotification, 32*len(s.coins))
	addAsset := func(assetID uint32, blockSource <-chan *asset.BlockUpdate) {
		errOut, errIn := meter.DelayedRelay(ctxHelpers, minBlockPeriod, 32)
		wgHelpers.Add(1)
		go func() {
			defer wgHelpers.Done()
			for {
				select {
				case blk, ok := <-blockSource:
					if !ok {
						log.Errorf("Asset %d has closed the block channel.", assetID)
						return
					}

					select {
					case errIn <- blk.Err:
					default: // if blocking, the relay is either metering anyway or spewing errors
					}

				case blkErr, ok := <-errOut: // nils are metered and aggregated
					if !ok { // relay stopped
						return
					}
					select {
					case <-ctxHelpers.Done():
						return
					case blockNotes <- &blockNotification{
						time:    time.Now().UTC(),
						assetID: assetID,
						err:     blkErr,
					}:
					}

				case <-ctxHelpers.Done():
					return
				}
			}
		}()
	}
	for assetID, lockable := range s.coins {
		addAsset(assetID, lockable.Backend.BlockChannel(32))
	}

	// Start the queue of coinwaiters for the init and redeem handlers. The
	// handlers must be stopped/blocked before stopping this.
	wgHelpers.Add(1)
	go func() {
		s.latencyQ.Run(ctxHelpers)
		wgHelpers.Done()
	}()

	log.Debugf("Swapper started with %v broadcast timeout and %v tx wait expiration.", s.bTimeout, s.txWaitExpiration)

	// Block-based inaction checks are started with Timers, and run in the main
	// loop to avoid locks and WaitGroups.
	bcastBlockTrigger := make(chan uint32, 32*len(s.coins))
	scheduleInactionCheck := func(assetID uint32) {
		time.AfterFunc(s.bTimeout, func() {
			// TODO: This pattern would still send the block trigger half of the
			// time if the ctxMaster is canceled.
			if ctxMaster.Err() != nil {
				return
			}
			select {
			case bcastBlockTrigger <- assetID: // all checks run in main loop
			case <-ctxMaster.Done():
			}
		})
	}

	// On startup, schedule an inaction check for each asset. Ideally these
	// would start bTimeout after the best block times.
	for assetID := range s.coins {
		scheduleInactionCheck(assetID)
	}

	// Event-based action checks are started with a single ticker. Each of the
	// events, e.g. match request, could start a timer, but this is simpler and
	// allows batching the match checks.
	bcastEventTrigger := bufferedTicker(ctxMaster, s.bTimeout/4)

	processBlockWithTimeout := func(block *blockNotification) {
		ctxTime, cancelTimeCtx := context.WithTimeout(ctxMaster, 5*time.Second)
		defer cancelTimeCtx()
		s.processBlock(ctxTime, block)
	}

	// Main loop can stop on internal error via cancel(), or when the caller
	// cancels the parent context triggering graceful shutdown.
	wgMain.Add(1)
	go func() {
		defer wgMain.Done()
		defer cancel() // ctxMaster for anomalous return
		for {
			select {
			case <-s.storage.Fatal():
				return
			case block := <-blockNotes:
				if block.err != nil {
					var connectionErr asset.ConnectionError
					if errors.As(block.err, &connectionErr) {
						// Connection issues handling can be triggered here.
						log.Errorf("connection error detected for %d: %v", block.assetID, block.err)
					} else {
						log.Errorf("asset %d is reporting a block notification error: %v", block.assetID, block.err)
					}
					continue
				}

				// processBlock will update confirmation times in the swapStatus
				// structs.
				processBlockWithTimeout(block)

				// Schedule an inaction check for matches that involve this
				// asset, as they could be expecting user action within bTimeout
				// of this event.
				scheduleInactionCheck(block.assetID)

			case assetID := <-bcastBlockTrigger:
				// There was a new block for this asset bTimeout ago.
				s.checkInactionBlockBased(assetID)

			case <-bcastEventTrigger:
				// Inaction checks that are not relative to blocks.
				s.checkInactionEventBased()

			case <-mainLoop:
				return
			}
		}
	}()

	// Wait for caller cancel or anomalous return from main loop.
	<-ctxMaster.Done()
}

// bufferedTicker creates a "ticker" that periodically sends on the returned
// channel, which has a buffer of length 1 and thus suitable for use in a select
// with other events that might cause a regular Ticker send to be dropped.
func bufferedTicker(ctx context.Context, dur time.Duration) chan struct{} {
	buffered := make(chan struct{}, 1) // only need 1 since back-to-back is pointless
	go func() {
		ticker := time.NewTicker(dur)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				buffered <- struct{}{}
			case <-ctx.Done():
				return
			}
		}
	}()
	return buffered
}

func (s *Swapper) tryConfirmSwap(ctx context.Context, status *swapStatus, confTime time.Time) (final bool) {
	if known, confirmed := status.contractState(); !known {
		return // no swap yet to confirm
	} else if confirmed {
		return true // already confirmed
	}

	// Swap known means status.swap is set, and that it will not be replaced
	// because we are gating processInit with the swapSearching semaphore.
	confs, err := status.swap.Confirmations(ctx)
	if err != nil {
		log.Warnf("Unable to get confirmations for swap tx %v: %v", status.swap.TxID(), err)
		return
	}

	status.mtx.Lock()
	defer status.mtx.Unlock()
	if !status.swapConfirmed.IsZero() { // in case a concurrent check already marked it
		return true
	}

	swapConf := s.coins[status.swapAsset].SwapConf // swapStatus exists, therefore swapAsset is in the map
	if confs >= int64(swapConf) {
		log.Debugf("Swap %v (%s) has reached %d confirmations (%d required)",
			status.swap, dex.BipIDSymbol(status.swapAsset), confs, swapConf)
		status.swapConfirmed = confTime.UTC()
		final = true
	}
	return
}

func (s *Swapper) matchSlice() []*matchTracker {
	s.matchMtx.RLock()
	defer s.matchMtx.RUnlock()
	matches := make([]*matchTracker, 0, len(s.matches))
	for _, match := range s.matches {
		matches = append(matches, match)
	}
	return matches
}

// processBlock scans the matches and updates a swapConfirmed time if the
// required confirmations are reached. Once a relevant transaction has the
// requisite number of confirmations, the next-to-act has only duration
// (Swapper).bTimeout to broadcast the next transaction in the settlement
// sequence. The timeout is not evaluated here, but in (Swapper).checkInaction.
// This method simply sets swapConfirmed in the last actor's swapStatus.
func (s *Swapper) processBlock(ctx context.Context, block *blockNotification) {
	for _, match := range s.matchSlice() {
		s.confirmMatchForBlock(ctx, match, block)
	}
}

// confirmMatchForBlock updates swapConfirmed for one match if this block's
// asset is the side waiting on confirmations.
func (s *Swapper) confirmMatchForBlock(ctx context.Context, match *matchTracker, block *blockNotification) {
	if match.makerStatus.swapAsset != block.assetID &&
		match.takerStatus.swapAsset != block.assetID {
		return
	}

	// Lock the matchTracker so the following checks and updates are atomic
	// with respect to Status.
	match.mtx.RLock()
	defer match.mtx.RUnlock()

	switch match.Status {
	case order.MakerSwapCast:
		if match.makerStatus.swapAsset != block.assetID {
			return
		}
		// If the maker has broadcast their transaction, the taker's
		// broadcast timeout starts once the maker's swap has SwapConf
		// confs.
		s.tryConfirmSwap(ctx, match.makerStatus, block.time)
	case order.TakerSwapCast:
		if match.takerStatus.swapAsset != block.assetID {
			return
		}
		// If the taker has broadcast their transaction, the maker's
		// broadcast timeout (for redemption) starts once the taker's swap
		// has SwapConf confs.
		s.tryConfirmSwap(ctx, match.takerStatus, block.time)
	}
}

// failMatch revokes the match and marks the swap as done for accounting
// purposes. If userFault is false, there will be no penalty, such as if the
// failure is because a swap tx lock time expired before required confirmations
// were reached. status is the decision-time match status.
func (s *Swapper) failMatch(match *matchTracker, status order.MatchStatus, userFault bool, takerAddrFault bool) {
	if err := s.submitMatchFailed(context.Background(), match, status, userFault, takerAddrFault); err != nil {
		if errors.Is(err, errMatchFailedRaceLost) {
			log.Debugf("match_failed proposal for match %v lost the commit-order race: %v", match.ID(), err)
			return
		}
		log.Warnf("failed to apply match_failed event for match %v: %v", match.ID(), err)
	}
}

func (s *Swapper) submitMatchFailed(ctx context.Context, match *matchTracker, status order.MatchStatus, userFault bool, takerAddrFault bool) error {
	if s.mesh == nil {
		return fmt.Errorf("swapper mesh service is not configured")
	}
	if !s.matchTracked(match) {
		return errMatchFailedRaceLost
	}
	event, err := s.newMatchFailedEvent(match, status, userFault, takerAddrFault)
	if err != nil {
		return err
	}
	if _, err := s.mesh.ApplyEvent(ctx, event); err != nil {
		return err
	}
	return nil
}

func swapContractKey(coinID, contract []byte) string {
	return fmt.Sprintf("%x:%x", coinID, contract)
}

func secretHashKey(secretHash []byte) string {
	return fmt.Sprintf("%x", secretHash)
}

// checkSwapContractDedup rejects contracts or maker secret hashes already tied
// to another active match before the swap contract event is emitted/applied.
func (s *Swapper) checkSwapContractDedup(matchID order.MatchID, coinID, contract, secretHash []byte, maker bool) error {
	s.activeCoinsMtx.Lock()
	defer s.activeCoinsMtx.Unlock()
	dedupKey := swapContractKey(coinID, contract)
	if existingMatch, exists := s.activeCoinIDs[dedupKey]; exists && existingMatch != matchID {
		return errSwapContractInUse
	}
	if maker && len(secretHash) > 0 {
		secretHashHex := secretHashKey(secretHash)
		if existingMatch, exists := s.activeSecretHashes[secretHashHex]; exists && existingMatch != matchID {
			return errSecretHashInUse
		}
	}
	return nil
}

// registerSwapContractDedup records the accepted contract keys so reuse by
// another active match is rejected while same-match ownership is not a conflict.
func (s *Swapper) registerSwapContractDedup(matchID order.MatchID, coinID, contract, secretHash []byte, maker bool) {
	s.activeCoinsMtx.Lock()
	defer s.activeCoinsMtx.Unlock()
	dedupKey := swapContractKey(coinID, contract)
	if _, already := s.activeCoinIDs[dedupKey]; !already {
		s.matchCoinIDs[matchID] = append(s.matchCoinIDs[matchID], dedupKey)
	}
	s.activeCoinIDs[dedupKey] = matchID
	if maker && len(secretHash) > 0 {
		secretHashHex := secretHashKey(secretHash)
		if _, already := s.activeSecretHashes[secretHashHex]; !already {
			s.matchSecretHashes[matchID] = append(s.matchSecretHashes[matchID], secretHashHex)
		}
		s.activeSecretHashes[secretHashHex] = matchID
	}
}

type fail struct {
	match *matchTracker
	// status is the detector's decision-time match status, captured under match.mtx.
	status order.MatchStatus
	fault  bool
	// takerAddrFault overrides the default fault attribution for
	// NewlyMatched timeouts. When true, the taker is blamed instead of
	// the maker because the taker failed to provide their per-match
	// swap address in time.
	takerAddrFault bool
}

// checkInactionEventBased scans the swapStatus structures, checking for actions
// that are expected in a time frame relative to another event that is not a
// confirmation time. If a client is found to have not acted when required, a
// match may be revoked and a penalty assigned to the user. This includes
// matches in NewlyMatched that have not received a maker swap following the
// match request, and in MakerRedeemed that have not received a taker redeem
// following the redemption request triggered by the makers redeem.
//
// For NewlyMatched timeouts, if the taker has not yet provided their per-match
// swap address (takerSwapAddr is empty), the taker is faulted instead of the
// maker, since the maker cannot broadcast their swap without the taker's
// address.
func (s *Swapper) checkInactionEventBased() {
	// Do not revoke for inaction while later master workers (the markets) are
	// still starting: clients may be unable to act, or even connect, until the
	// node is fully up. MarketsReady re-floors the deadlines when the wait ends.
	if !s.marketsReady.Load() {
		return
	}
	// If the DB is failing, do not penalize or attempt to start revocations.
	if err := s.storage.LastErr(); err != nil {
		log.Errorf("DB in failing state.")
		return
	}

	var failures []fail

	// Do time.Since(event) with the same now time for each match.
	now := time.Now()
	tooOld := func(evt time.Time) bool {
		return now.Sub(evt) >= s.bTimeout
	}

	checkMatch := func(match *matchTracker) {
		// Lock entire matchTracker so the following is atomic with respect to
		// Status.
		match.mtx.Lock()
		defer match.mtx.Unlock()

		log.Tracef("checkInactionEventBased: match %v (%v)", match.ID(), match.Status)

		failMatch := func(fault bool) {
			failures = append(failures, fail{match: match, status: match.Status, fault: fault})
		}

		switch match.Status {
		case order.NewlyMatched:
			// Maker has not broadcast their swap. They have until match time
			// plus bTimeout.
			if tooOld(match.time) {
				if match.takerSwapAddr == "" {
					// The taker failed to provide their per-match swap
					// address in time, preventing the maker from
					// swapping. Fault the taker, not the maker.
					log.Infof("Revoking match %v at NewlyMatched: taker did not provide per-match address in time", match.ID())
					failures = append(failures, fail{match: match, status: match.Status, fault: true, takerAddrFault: true})
				} else {
					failMatch(true)
				}
			}
		case order.MakerSwapCast:
			// If the taker contract's expected lock time would be in the past,
			// revoke this match with no penalty.
			expectedTakerLockTime := match.matchTime.Add(s.lockTimeTaker)
			if expectedTakerLockTime.Before(now) {
				log.Infof("Revoking match %v at %v because the expected taker swap locktime would be in the past (%v).",
					match.ID(), match.Status, expectedTakerLockTime)
				failMatch(false)
			} else if match.expiredBy(now) {
				// The taker's contract should expire first, but also check the
				// lock time of the maker's known swap.
				log.Warnf("Revoking match %v at %v because maker's published contract has expired.",
					match.ID(), match.Status) // WRN because taker's should expire first
				failMatch(false)
			}
		case order.TakerSwapCast:
			// If either published contract's lock time is already passed,
			// revoke with no penalty because the swap cannot complete safely.
			if match.expiredBy(now) {
				log.Infof("Revoking match %v at %v because at least one published contract has expired.",
					match.ID(), match.Status)
				failMatch(false)
			}
		case order.MakerRedeemed:
			// If the maker has redeemed, the taker can redeem immediately, so
			// check the timeout against the time the Swapper received the
			// maker's `redeem` request (and sent the taker's 'redemption').
			if tooOld(match.makerStatus.redeemSeenTime()) { // rlocks swapStatus.mtx
				failMatch(true)
			}
		}
		// MatchComplete: deleted in swap_redemption_recorded applier, not here.
	}

	// Collect failures while match state is stable.
	s.matchMtx.Lock()
	for _, match := range s.matches {
		checkMatch(match)
	}
	s.matchMtx.Unlock()

	// Emit the authoritative failure events after releasing matchMtx. The
	// applier performs final deletion and side effects.
	for _, fail := range failures {
		s.failMatch(fail.match, fail.status, fail.fault, fail.takerAddrFault)
	}
}

// checkInactionBlockBased scans the swapStatus structures relevant to the
// specified asset. If a client is found to have not acted when required, a
// match may be revoked and a penalty assigned to the user. This includes
// matches in MakerSwapCast that have not received a taker swap after the
// maker's swap reaches the required confirmation count, and in TakerSwapCast
// that have not received a maker redeem after the taker's swap reaches the
// required confirmation count.
func (s *Swapper) checkInactionBlockBased(assetID uint32) {
	// See checkInactionEventBased: no inaction revocations until the markets
	// have reported ready.
	if !s.marketsReady.Load() {
		return
	}
	// If the DB is failing, do not penalize or attempt to start revocations.
	if err := s.storage.LastErr(); err != nil {
		log.Errorf("DB in failing state.")
		return
	}

	var failures []fail
	// Do time.Since(event) with the same now time for each match.
	now := time.Now()
	tooOld := func(evt time.Time) bool {
		// If the time is not set (zero), it has not happened yet (not too old).
		return !evt.IsZero() && now.Sub(evt) >= s.bTimeout
	}

	checkMatch := func(match *matchTracker) {
		if match.makerStatus.swapAsset != assetID && match.takerStatus.swapAsset != assetID {
			return
		}

		// Lock entire matchTracker so the following is atomic with respect to
		// Status.
		match.mtx.Lock()
		defer match.mtx.Unlock()

		log.Tracef("checkInactionBlockBased: asset %d, match %v (%v)",
			assetID, match.ID(), match.Status)

		failMatch := func() {
			// Fail the match, and assign fault if lock times are not passed.
			failures = append(failures, fail{match: match, status: match.Status, fault: !match.expiredBy(now)})
		}

		switch match.Status {
		case order.MakerSwapCast:
			if tooOld(match.makerStatus.swapConfTime()) { // rlocks swapStatus.mtx
				failMatch()
			}
		case order.TakerSwapCast:
			if tooOld(match.takerStatus.swapConfTime()) {
				failMatch()
			}
		}
	}

	// Collect failures while match state is stable.
	s.matchMtx.Lock()
	for _, match := range s.matches {
		checkMatch(match)
	}
	s.matchMtx.Unlock()

	// Emit the authoritative failure events after releasing matchMtx. The
	// applier performs final deletion and side effects.
	for _, fail := range failures {
		s.failMatch(fail.match, fail.status, fail.fault, fail.takerAddrFault)
	}
}

// respondError sends an rpcError to a user.
func (s *Swapper) respondError(id uint64, user account.AccountID, code int, errMsg string) {
	log.Debugf("Error going to user %v, code: %d, msg: %s", user, code, errMsg)
	msg, err := msgjson.NewResponse(id, nil, &msgjson.Error{
		Code:    code,
		Message: errMsg,
	})
	if err != nil {
		log.Errorf("Failed to create error response with message '%s': %v", msg, err)
		return // this should not be possible, but don't pass nil msg to Send
	}
	if err := s.authMgr.Send(user, msg); err != nil {
		log.Infof("Unable to send error response (code = %d, msg = %s) to disconnected user %v: %q",
			code, errMsg, user, err)
	}
}

// step creates a stepInformation structure for the specified match. A new
// stepInformation should be created for every client communication. The user
// is also validated as the actor. An error is returned if the user has not
// acknowledged their previous DEX requests.
func (s *Swapper) step(user account.AccountID, matchID order.MatchID) (*stepInformation, *msgjson.Error) {
	s.matchMtx.RLock()
	match, found := s.matches[matchID]
	s.matchMtx.RUnlock()
	if !found {
		return nil, &msgjson.Error{
			Code:    msgjson.RPCUnknownMatch,
			Message: "unknown match ID",
		}
	}

	// Get the step-related information for both parties.
	var isBaseAsset bool
	var actor, counterParty stepActor
	var nextStep order.MatchStatus
	maker, taker := match.Maker, match.Taker

	// Lock for Status and Sigs.
	match.mtx.RLock()
	defer match.mtx.RUnlock()

	// maker sell: base swap, quote redeem
	// taker buy: quote swap, base redeem

	// maker buy: quote swap, base redeem
	// taker sell: base swap, quote redeem

	// Maker broadcasts the swap contract. Sequence: NewlyMatched ->
	// MakerSwapCast -> TakerSwapCast -> MakerRedeemed -> MatchComplete
	switch match.Status {
	case order.NewlyMatched, order.TakerSwapCast:
		counterParty.order, actor.order = taker, maker
		actor.status = match.makerStatus
		counterParty.status = match.takerStatus
		actor.user = maker.User()
		counterParty.user = taker.User()
		actor.isMaker = true
		if match.Status == order.NewlyMatched {
			nextStep = order.MakerSwapCast
			isBaseAsset = maker.Sell // maker swap: base asset if sell
			if len(match.Sigs.MakerMatch) == 0 {
				log.Debugf("swap %v at status %v missing MakerMatch signature(s) expected before NewlyMatched->MakerSwapCast",
					match.ID(), match.Status)
			}
		} else /* TakerSwapCast */ {
			nextStep = order.MakerRedeemed
			isBaseAsset = !maker.Sell // maker redeem: base asset if buy
			if len(match.Sigs.MakerAudit) == 0 {
				log.Debugf("Swap %v at status %v missing MakerAudit signature(s) expected before TakerSwapCast->MakerRedeemed",
					match.ID(), match.Status)
			}
		}
	case order.MakerSwapCast, order.MakerRedeemed:
		counterParty.order, actor.order = maker, taker
		actor.status = match.takerStatus
		counterParty.status = match.makerStatus
		actor.user = taker.User()
		counterParty.user = maker.User()
		counterParty.isMaker = true
		if match.Status == order.MakerSwapCast {
			nextStep = order.TakerSwapCast
			isBaseAsset = !maker.Sell // taker swap: base asset if sell (maker buy)
			if len(match.Sigs.TakerMatch) == 0 {
				log.Debugf("Swap %v at status %v missing TakerMatch signature(s) expected before MakerSwapCast->TakerSwapCast",
					match.ID(), match.Status)
			}
			if len(match.Sigs.TakerAudit) == 0 {
				log.Debugf("Swap %v at status %v missing TakerAudit signature(s) expected before MakerSwapCast->TakerSwapCast",
					match.ID(), match.Status)
			}
		} else /* MakerRedeemed */ {
			nextStep = order.MatchComplete
			// Inactive/deleted when this redeem is recorded.
			isBaseAsset = maker.Sell // taker redeem: base asset if buy (maker sell)
			if len(match.Sigs.TakerRedeem) == 0 {
				log.Debugf("Swap %v at status %v missing TakerRedeem signature(s) expected before MakerRedeemed->MatchComplete",
					match.ID(), match.Status)
			}
		}
	default:
		return nil, &msgjson.Error{
			Code:    msgjson.SettlementSequenceError,
			Message: "unknown settlement sequence identifier",
		}
	}

	// Verify that the user specified is the actor for this step.
	if actor.user != user { // NOTE: self-trade slips past this
		return nil, &msgjson.Error{
			Code:    msgjson.SettlementSequenceError,
			Message: "expected other party to act",
		}
	}

	// Set the actors' swapAsset and the swap contract checkVal.
	var checkVal uint64
	if isBaseAsset {
		actor.swapAsset = maker.BaseAsset
		counterParty.swapAsset = maker.QuoteAsset
		checkVal = match.Quantity
	} else {
		actor.swapAsset = maker.QuoteAsset
		counterParty.swapAsset = maker.BaseAsset
		checkVal = matcher.BaseToQuote(maker.Rate, match.Quantity)
	}

	return &stepInformation{
		match:        match,
		actor:        actor,
		counterParty: counterParty,
		// By the time a match is created, the presence of the asset in the map
		// has already been verified.
		asset:       s.coins[actor.swapAsset].BackedAsset,
		isBaseAsset: isBaseAsset,
		step:        match.Status,
		nextStep:    nextStep,
		checkVal:    checkVal,
	}, nil
}

// authUser verifies that the msgjson.Signable is signed by the user. This
// method uses stored account data when the user is connected to the peer node.
func (s *Swapper) authUser(user account.AccountID, params msgjson.Signable) *msgjson.Error {
	// Authorize the user.
	msg := params.Serialize()
	err := s.authMgr.VerifyUserSig(user, msg, params.SigBytes())
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.SignatureError,
			Message: "error authenticating init params",
		}
	}
	return nil
}

// messageAcker is information needed to process the user's
// msgjson.Acknowledgement.
type messageAcker struct {
	user    account.AccountID
	match   *matchTracker
	params  msgjson.Signable
	isMaker bool
	isAudit bool
}

// processAck processes a msgjson.Acknowledgement to the audit, redemption, and
// revoke_match requests, validating the signature and updating the
// (order.Match).Sigs record. This is required by processInit, processRedeem,
// and revoke. Match Acknowledgements are handled by processMatchAck.
func (s *Swapper) processAck(msg *msgjson.Message, acker *messageAcker) {
	ack := new(msgjson.Acknowledgement)
	err := msg.UnmarshalResult(ack)
	if err != nil {
		s.respondError(msg.ID, acker.user, msgjson.RPCParseError, fmt.Sprintf("error parsing acknowledgment: %v", err))
		return
	}
	// Note: ack.MatchID unused, but could be checked against acker.match.ID().

	// Check the signature.
	sigMsg := acker.params.Serialize()
	err = s.authMgr.VerifyUserSig(acker.user, sigMsg, ack.Sig)
	if err != nil {
		s.respondError(msg.ID, acker.user, msgjson.SignatureError,
			fmt.Sprintf("signature validation error: %v", err))
		return
	}

	switch acker.params.(type) {
	case *msgjson.Audit, *msgjson.Redemption:
	default:
		log.Warnf("unrecognized ack type %T", acker.params)
		return
	}
	if !s.matchTracked(acker.match) {
		log.Debugf("Ignoring acknowledgement from user %v for untracked match %v", acker.user, acker.match.ID())
		return
	}

	if acker.isAudit {
		log.Debugf("Received contract 'audit' acknowledgement from user %v (%s) for match %v (%v)",
			acker.user, makerTaker(acker.isMaker), acker.match.Match.ID(), acker.match.Status)
		event, err := newAuditAckRecordedEvent(acker.match, acker.isMaker, ack.Sig)
		if err != nil {
			log.Errorf("error creating audit ack recorded event: %v", err)
			s.respondError(msg.ID, acker.user, msgjson.RPCInternalError, "internal server error")
			return
		}
		if _, err := s.mesh.ApplyEvent(context.Background(), event); err != nil {
			mesh.LogApplyFailure(log, err, "error applying audit ack recorded event for match %v: %v",
				acker.match.Match.ID(), err)
			msgErr := mesh.ClientError(err, msgjson.RPCInternalError, "internal server error")
			s.respondError(msg.ID, acker.user, msgErr.Code, msgErr.Message)
			return
		}
		return
	}

	// It's a redemption ack.
	log.Debugf("Received 'redemption' acknowledgement from user %v (%s) for match %v (%s)",
		acker.user, makerTaker(acker.isMaker), acker.match.Match.ID(), acker.match.Status)

	event, err := newRedemptionAckRecordedEvent(acker.match, acker.isMaker, ack.Sig)
	if err != nil {
		log.Errorf("error creating redemption ack recorded event: %v", err)
		s.respondError(msg.ID, acker.user, msgjson.RPCInternalError, "internal server error")
		return
	}
	if _, err := s.mesh.ApplyEvent(context.Background(), event); err != nil {
		mesh.LogApplyFailure(log, err, "error applying redemption ack recorded event for match %v: %v",
			acker.match.Match.ID(), err)
		msgErr := mesh.ClientError(err, msgjson.RPCInternalError, "internal server error")
		s.respondError(msg.ID, acker.user, msgErr.Code, msgErr.Message)
		return
	}
}

// processInit processes the `init` RPC request, which is used to inform the DEX
// of a newly broadcast swap transaction. Once the transaction is seen and
// audited by the Swapper, the counter-party is informed with an 'audit'
// request. This method is run as a coin waiter, hence the return value
// indicates if future attempts should be made to check coin status.
func (s *Swapper) processInit(ctx context.Context, completion *mesh.CommandCompletion, params *msgjson.Init, stepInfo *stepInformation) wait.TryDirective {
	actor, counterParty := stepInfo.actor, stepInfo.counterParty
	failMsg := func(msgErr *msgjson.Error) wait.TryDirective {
		actor.status.endSwapSearch()
		if err := completion.Fail(ctx, msgErr); err != nil {
			log.Errorf("failed to send init command error for user %v: %v", actor.user, err)
		}
		return wait.DontTryAgain
	}
	fail := func(code int, format string, args ...any) wait.TryDirective {
		return failMsg(msgjson.NewError(code, format, args...))
	}

	// Validate the swap contract
	chain := stepInfo.asset.Backend
	contract, err := chain.Contract(params.CoinID, params.Contract)
	if err != nil {
		if errors.Is(err, asset.CoinNotFoundError) {
			return wait.TryAgain
		}
		actor.status.mtx.RLock()
		log.Warnf("Contract error encountered for match %s, actor %s using coin ID %v and contract %v: %v",
			stepInfo.match.ID(), actor, params.CoinID, params.Contract, err)
		actor.status.mtx.RUnlock()
		return fail(msgjson.ContractError, "contract error encountered: %v", err)
	}

	// Enforce the prescribed swap fee rate, but only if the swap is not already
	// confirmed.
	swapConfs := func() int64 { // not executed if adequate fee rate
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		confs, err := contract.Confirmations(ctx)
		if err != nil {
			log.Warnf("Failed to get confirmations on swap tx %v: %v", contract.TxID(), err)
			confs = 0 // should be already
		}
		return confs
	}
	reqFeeRate := stepInfo.match.FeeRateQuote
	if stepInfo.isBaseAsset {
		reqFeeRate = stepInfo.match.FeeRateBase
	}

	if !chain.ValidateFeeRate(contract.Coin, reqFeeRate) {
		confs := swapConfs()
		if confs < 1 {
			return fail(msgjson.ContractError, "low tx fee")
		}
		log.Infof("Swap txn %v (%s) with low fee rate (%v required), accepted with %d confirmations.",
			contract, stepInfo.asset.Symbol, reqFeeRate, confs)
	}
	// Use the per-match swap address from the counterparty's match ack.
	stepInfo.match.mtx.RLock()
	var expectedAddr string
	if counterParty.isMaker {
		expectedAddr = stepInfo.match.makerSwapAddr
	} else {
		expectedAddr = stepInfo.match.takerSwapAddr
	}
	stepInfo.match.mtx.RUnlock()
	if expectedAddr == "" {
		return fail(msgjson.ContractError,
			"counterparty per-match address not yet received for match %v", stepInfo.match.ID())
	}
	if contract.SwapAddress != expectedAddr {
		return fail(msgjson.ContractError,
			"incorrect recipient. expected %s. got %s", expectedAddr, contract.SwapAddress)
	}

	// Swap contract dedup check. The event applier authoritatively registers
	// the keys after all validation succeeds.
	// The composite key of CoinID and contract data is used so that EVM
	// batched swaps (same txHash, different contract/locator) are not
	// incorrectly rejected.
	mid := stepInfo.match.ID()
	if err := s.checkSwapContractDedup(mid, params.CoinID, params.Contract, contract.SecretHash, actor.isMaker); err != nil {
		return fail(msgjson.ContractError, "%v", err)
	}

	if contract.Value() != stepInfo.checkVal {
		return fail(msgjson.ContractError,
			"contract error. expected contract value to be %d, got %d", stepInfo.checkVal, contract.Value())
	}
	if !actor.isMaker && !bytes.Equal(contract.SecretHash, counterParty.status.swap.SecretHash) {
		return fail(msgjson.ContractError,
			"incorrect secret hash. expected %x. got %x", contract.SecretHash, counterParty.status.swap.SecretHash)
	}

	reqLockTime := encode.DropMilliseconds(stepInfo.match.matchTime.Add(s.lockTimeTaker))
	if actor.isMaker {
		reqLockTime = encode.DropMilliseconds(stepInfo.match.matchTime.Add(s.lockTimeMaker))
	}
	if contract.LockTime.Before(reqLockTime) {
		return fail(msgjson.ContractError,
			"contract error. expected lock time >= %s, got %s", reqLockTime, contract.LockTime)
	} else if remain := time.Until(contract.LockTime); remain < 0 {
		fail(msgjson.ContractError, "contract is correct, but lock time passed %s ago", remain)
		// Revoke the match proactively before checkInaction gets to it.
		if s.matchTracked(stepInfo.match) {
			s.failMatch(stepInfo.match, stepInfo.step, false, false) // no fault
		} // else it's already revoked
		return wait.DontTryAgain // and don't tell counterparty of expired contract they should not redeem
	}

	// Update the match.
	swapTime := unixMsNow()
	matchID := stepInfo.match.Match.ID()

	if !s.matchTracked(stepInfo.match) {
		log.Errorf("Contract txn located after match was revoked (match id=%v, maker=%v)",
			matchID, actor.isMaker)
		return fail(msgjson.ContractError, "match already revoked due to inaction")
	}

	event, err := newSwapContractRecordedEvent(stepInfo, params, contract, swapTime)
	if err != nil {
		log.Errorf("error creating swap contract recorded event: %v", err)
		return fail(msgjson.RPCInternalError, "internal server error")
	}
	if err := completion.Emit(ctx, event, func() any {
		s.authMgr.Sign(params)
		return &msgjson.Acknowledgement{
			MatchID: matchID[:],
			Sig:     params.Sig,
		}
	}); err != nil {
		if errors.Is(err, errSwapContractInUse) || errors.Is(err, errSecretHashInUse) {
			log.Errorf("error applying swap contract recorded event: %v", err)
			return fail(msgjson.ContractError, "%v", err)
		}
		mesh.LogApplyFailure(log, err, "error applying swap contract recorded event for match %v: %v", matchID, err)
		return failMsg(mesh.ClientError(err, msgjson.RPCInternalError, "internal server error"))
	}

	log.Debugf("processInit: valid contract %v (%s) received at %v from user %v (%s) for match %v, "+
		"swapStatus %v => %v", contract, stepInfo.asset.Symbol, swapTime, actor.user,
		makerTaker(actor.isMaker), matchID, stepInfo.step, stepInfo.nextStep)

	// Contract now recorded and will be used to reject backward progress
	// (duplicate or malicious requests client might still send after this point).
	actor.status.endSwapSearch()
	s.requestAudit(stepInfo, params, contract, swapTime)

	return wait.DontTryAgain
}

func (s *Swapper) requestAudit(stepInfo *stepInformation, params *msgjson.Init, contract *asset.Contract, swapTime time.Time) {
	counterParty := stepInfo.counterParty
	matchID := stepInfo.match.Match.ID()
	// Prepare an 'audit' request for the counter-party.
	auditParams := &msgjson.Audit{
		OrderID:  idToBytes(counterParty.order.ID()),
		MatchID:  matchID[:],
		Time:     uint64(swapTime.UnixMilli()),
		CoinID:   params.CoinID,
		Contract: params.Contract,
		TxData:   contract.TxData,
	}
	s.sendAuditRequest(stepInfo.match, counterParty.user, counterParty.isMaker, auditParams)
}

// sendAuditRequest signs and sends a contract 'audit' request to the
// recipient, registering the acknowledgement callback. The match mtx should
// NOT be held.
func (s *Swapper) sendAuditRequest(match *matchTracker, recipient account.AccountID, recipientIsMaker bool, auditParams *msgjson.Audit) {
	match.mtx.Lock()
	if recipientIsMaker {
		match.lastMakerAudit = time.Now()
	} else {
		match.lastTakerAudit = time.Now()
	}
	match.mtx.Unlock()

	s.authMgr.Sign(auditParams)
	notification, err := msgjson.NewRequest(comms.NextID(), msgjson.AuditRoute, auditParams)
	if err != nil {
		// This is likely an impossible condition.
		log.Errorf("error creating audit request: %v", err)
		return
	}

	matchID := match.ID()
	// Set up the acknowledgement for the callback.
	ack := &messageAcker{
		user:    recipient,
		match:   match,
		params:  auditParams,
		isMaker: recipientIsMaker,
		isAudit: true,
	}
	// Send the 'audit' request to the counter-party.
	log.Debugf("sending contract 'audit' request to counterparty %v (%s) "+
		"for match %v", ack.user, makerTaker(ack.isMaker), matchID)
	// The counterparty will audit the contract by retrieving it, which may
	// involve them waiting for up to the broadcast timeout before responding,
	// so the user gets at least s.bTimeout to the request.
	err = s.authMgr.RequestWithTimeout(ack.user, notification, func(_ comms.Link, resp *msgjson.Message) {
		s.processAck(resp, ack) // resp.ID == notification.ID
	}, s.bTimeout, func() {
		log.Infof("Timeout waiting for contract 'audit' request acknowledgement from user %v (%s) for match %v",
			ack.user, makerTaker(ack.isMaker), matchID)
	})
	if err != nil {
		log.Debugf("Couldn't send 'audit' request to user %v (%s) for match %v: %v",
			ack.user, makerTaker(ack.isMaker), matchID, err)
	}
}

// processRedeem processes a 'redeem' command from a client. processRedeem does
// not perform user authentication, which is handled in executeRedeem before
// processRedeem is invoked. This method is run as a coin waiter.
func (s *Swapper) processRedeem(ctx context.Context, completion *mesh.CommandCompletion, params *msgjson.Redeem, stepInfo *stepInformation) wait.TryDirective {
	// TODO(consider): Extract secret from initiator's (maker's) redemption
	// transaction. The Backend would need a method identify the component of
	// the redemption transaction that contains the secret and extract it. In a
	// UTXO-based asset, this means finding the input that spends the output of
	// the counterparty's contract, and process that input's signature script
	// with FindKeyPush. Presently this is up to the clients and not stored with
	// the server.

	// Make sure that the expected output is being spent.
	actor, counterParty := stepInfo.actor, stepInfo.counterParty
	failMsg := func(msgErr *msgjson.Error) wait.TryDirective {
		actor.status.endRedeemSearch()
		if err := completion.Fail(ctx, msgErr); err != nil {
			log.Errorf("failed to send redeem command error for user %v: %v", actor.user, err)
		}
		return wait.DontTryAgain
	}
	fail := func(code int, format string, args ...any) wait.TryDirective {
		return failMsg(msgjson.NewError(code, format, args...))
	}

	counterParty.status.mtx.RLock()
	cpContract := counterParty.status.swap.ContractData
	cpSwapCoin := counterParty.status.swap.ID()
	cpSwapStr := counterParty.status.swap.String()
	counterParty.status.mtx.RUnlock()

	// Get the transaction.
	match := stepInfo.match
	matchID := match.ID()
	chain := stepInfo.asset.Backend
	if !chain.ValidateSecret(params.Secret, cpContract) {
		log.Infof("Secret validation failed (match id=%v, maker=%v, secret=%v)",
			matchID, actor.isMaker, params.Secret)
		return fail(msgjson.InvalidRequestError, "secret validation failed")
	}
	redemption, err := chain.Redemption(params.CoinID, cpSwapCoin, cpContract)
	// If there is an error, don't return an error yet, since it could be due to
	// network latency. Instead, queue it up for another check.
	if err != nil {
		if errors.Is(err, asset.CoinNotFoundError) {
			return wait.TryAgain
		}
		actor.status.mtx.RLock()
		log.Warnf("Redemption error encountered for match %s, actor %s, using coin ID %v to satisfy contract at %x: %v",
			stepInfo.match.ID(), actor, params.CoinID, cpSwapCoin, err)
		actor.status.mtx.RUnlock()
		return fail(msgjson.RedemptionError, "redemption error encountered: %v", err)
	}

	redeemTime := unixMsNow()
	event, err := newSwapRedemptionRecordedEvent(stepInfo, params, redemption, redeemTime)
	if err != nil {
		log.Errorf("error creating swap redemption recorded event: %v", err)
		return fail(msgjson.RPCInternalError, "internal server error")
	}

	// NOTE: redemption.FeeRate is not checked since the counterparty is not
	// inconvenienced by slow confirmation of the redemption.

	if err := completion.Emit(ctx, event, func() any {
		// Redemption now recorded and will be used to reject backward progress
		// from duplicate or malicious requests after this point.
		actor.status.endRedeemSearch()
		s.authMgr.Sign(params)
		return &msgjson.Acknowledgement{
			MatchID: matchID[:],
			Sig:     params.Sig,
		}
	}); err != nil {
		mesh.LogApplyFailure(log, err, "error applying swap redemption recorded event for match %v: %v", matchID, err)
		return failMsg(mesh.ClientError(err, msgjson.RPCInternalError, "internal server error"))
	}

	log.Debugf("processRedeem: valid redemption %v (%s) spending contract %s received at %v from %v (%s) for match %v, "+
		"swapStatus %v => %v", redemption, stepInfo.asset.Symbol, cpSwapStr, redeemTime, actor.user,
		makerTaker(actor.isMaker), matchID, stepInfo.step, stepInfo.nextStep)
	s.requestRedemption(stepInfo, params, redeemTime)
	return wait.DontTryAgain
}

func (s *Swapper) requestRedemption(stepInfo *stepInformation, params *msgjson.Redeem, redeemTime time.Time) {
	match := stepInfo.match
	matchID := match.ID()
	counterParty := stepInfo.counterParty

	// Inform the counterparty, even though the maker doesn't really care about
	// the taker's redeem details.
	rParams := &msgjson.Redemption{
		Redeem: msgjson.Redeem{
			OrderID: idToBytes(counterParty.order.ID()),
			MatchID: matchID[:],
			CoinID:  params.CoinID,
			Secret:  params.Secret,
		},
		Time: uint64(redeemTime.UnixMilli()),
	}
	s.sendRedemptionRequest(match, counterParty.user, counterParty.isMaker, rParams,
		time.Until(redeemTime.Add(s.bTimeout)))
}

// sendRedemptionRequest signs and sends a 'redemption' request to the
// recipient, registering the acknowledgement callback. The match mtx should
// NOT be held.
func (s *Swapper) sendRedemptionRequest(match *matchTracker, recipient account.AccountID, recipientIsMaker bool, rParams *msgjson.Redemption, expireIn time.Duration) {
	if !recipientIsMaker { // the sweep only re-sends the taker's request
		match.mtx.Lock()
		match.lastRedeem = time.Now()
		match.mtx.Unlock()
	}

	s.authMgr.Sign(rParams)
	redemptionReq, err := msgjson.NewRequest(comms.NextID(), msgjson.RedemptionRoute, rParams)
	if err != nil {
		log.Errorf("error creating redemption request: %v", err)
		return
	}

	matchID := match.ID()
	// Send the redemption request.
	log.Debugf("sending 'redemption' request to counterparty %v (%s) "+
		"for match %v", recipient, makerTaker(recipientIsMaker), matchID)

	// Set up the redemption acknowledgement callback.
	ack := &messageAcker{
		user:    recipient,
		match:   match,
		params:  rParams,
		isMaker: recipientIsMaker,
		// isAudit: false,
	}

	// The counterparty does not need to actually locate the redemption txn,
	// so use the default request timeout.
	err = s.authMgr.RequestWithTimeout(ack.user, redemptionReq, func(_ comms.Link, resp *msgjson.Message) {
		s.processAck(resp, ack) // resp.ID == notification.ID
	}, expireIn, func() {
		log.Infof("Timeout waiting for 'redemption' request from user %v (%s) for match %v",
			ack.user, makerTaker(ack.isMaker), matchID)
	})
	if err != nil {
		log.Debugf("Couldn't send 'redemption' request to user %v (%s) for match %v: %v",
			ack.user, makerTaker(ack.isMaker), matchID, err)
	}
}

// recordedSettlementSide loads the match row and reports whether the sender
// is the maker. A lookup error must not fall through to step(); that would
// refuse a recorded resend as unknown.
func (s *Swapper) recordedSettlementSide(user account.AccountID, matchID order.MatchID,
	orderID msgjson.Bytes) (sd *db.SwapDataFull, isMaker bool, err error) {
	sd, err = s.storage.SwapDataFullByID(matchID)
	if err != nil {
		log.Errorf("Resend lookup for match %v failed: %v", matchID, err)
		return nil, false, err
	}
	if sd == nil {
		return nil, false, nil
	}
	switch {
	case user == sd.MakerAcct && bytes.Equal(orderID, sd.Maker[:]):
		return sd, true, nil
	case user == sd.TakerAcct && bytes.Equal(orderID, sd.Taker[:]):
		return sd, false, nil
	}
	return nil, false, nil
}

func settlementLookupUnavailable() (bool, *msgjson.Error) {
	return true, msgjson.NewError(msgjson.TryAgainLaterError,
		"settlement resend lookup unavailable; retry the request")
}

func (s *Swapper) reAckSettlement(cmdCtx *mesh.CommandContext, matchID order.MatchID, params msgjson.Signable) (bool, *msgjson.Error) {
	s.authMgr.Sign(params)
	if err := cmdCtx.Completion.Complete(cmdCtx.Context, &msgjson.Acknowledgement{
		MatchID: matchID[:],
		Sig:     params.SigBytes(),
	}); err != nil {
		// Delivery failure only: the client resends again and this path
		// answers again.
		log.Errorf("failed to deliver settlement re-ack for match %v: %v", matchID, err)
	}
	return true, nil
}

// reAckRecordedInit answers an init that already matches this side's recorded
// contract. Call it before step(). A field mismatch is not handled, so it
// never gets a signature.
func (s *Swapper) reAckRecordedInit(cmdCtx *mesh.CommandContext, user account.AccountID,
	matchID order.MatchID, params *msgjson.Init) (bool, *msgjson.Error) {
	sd, isMaker, err := s.recordedSettlementSide(user, matchID, params.OrderID)
	if err != nil {
		return settlementLookupUnavailable()
	}
	if sd == nil {
		return false, nil
	}
	coinID, contract := sd.ContractACoinID, sd.ContractA
	if !isMaker {
		coinID, contract = sd.ContractBCoinID, sd.ContractB
	}
	if len(coinID) == 0 || !bytes.Equal(coinID, params.CoinID) || !bytes.Equal(contract, params.Contract) {
		return false, nil
	}
	log.Debugf("Re-acking recorded contract for match %v (%s)", matchID, makerTaker(isMaker))
	return s.reAckSettlement(cmdCtx, matchID, params)
}

// reAckRecordedRedeem answers a redeem that already matches this side's
// recorded coin. Both sides use the maker's secret (RedeemASecret).
func (s *Swapper) reAckRecordedRedeem(cmdCtx *mesh.CommandContext, user account.AccountID,
	matchID order.MatchID, params *msgjson.Redeem) (bool, *msgjson.Error) {
	sd, isMaker, err := s.recordedSettlementSide(user, matchID, params.OrderID)
	if err != nil {
		return settlementLookupUnavailable()
	}
	if sd == nil {
		return false, nil
	}
	coinID := sd.RedeemACoinID
	if !isMaker {
		coinID = sd.RedeemBCoinID
	}
	if len(coinID) == 0 || !bytes.Equal(coinID, params.CoinID) || !bytes.Equal(sd.RedeemASecret, params.Secret) {
		return false, nil
	}
	log.Debugf("Re-acking recorded redeem for match %v (%s)", matchID, makerTaker(isMaker))
	return s.reAckSettlement(cmdCtx, matchID, params)
}

// settlementMayBeRecorded is true when the tracker is gone or the match has
// moved past NewlyMatched, so a recorded payload is possible.
func (s *Swapper) settlementMayBeRecorded(matchID order.MatchID) bool {
	s.matchMtx.RLock()
	match, tracked := s.matches[matchID]
	s.matchMtx.RUnlock()
	if !tracked {
		return true
	}
	match.mtx.RLock()
	defer match.mtx.RUnlock()
	return match.Status != order.NewlyMatched
}

// executeInit handles the 'init' command, which is used to inform the DEX of a
// newly broadcast swap transaction. The Init message includes the swap contract
// script and the CoinID of the contract.
func (s *Swapper) executeInit(cmdCtx *mesh.CommandContext) *msgjson.Error {
	user, msg := cmdCtx.Request.User, cmdCtx.Request.Msg
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock() // block shutdown until registered with latencyQ
	if s.stop {
		return msgjson.NewError(msgjson.TryAgainLaterError, "The swapper is stopping. Try again later.")
	}

	params := new(msgjson.Init)
	err := msg.Unmarshal(&params)
	if err != nil || params == nil {
		return msgjson.NewError(msgjson.RPCParseError, "Error decoding 'init' method params")
	}

	// Verify the user's signature of params.
	rpcErr := s.authUser(user, params)
	if rpcErr != nil {
		return rpcErr
	}

	log.Debugf("handleInit: 'init' received from user %v for match %v, order %v",
		user, params.MatchID, params.OrderID)

	if len(params.MatchID) != order.MatchIDSize {
		return msgjson.NewError(msgjson.RPCParseError, "Invalid 'matchid' in 'init' message")
	}

	var matchID order.MatchID
	copy(matchID[:], params.MatchID)

	if s.settlementMayBeRecorded(matchID) {
		if handled, rpcErr := s.reAckRecordedInit(cmdCtx, user, matchID, params); handled {
			return rpcErr
		}
	}

	stepInfo, rpcErr := s.step(user, matchID)
	if rpcErr != nil {
		return rpcErr
	}

	// init requests should only be sent when contracts are still required, in
	// the correct sequence, and by the correct party.
	switch stepInfo.step {
	case order.NewlyMatched, order.MakerSwapCast:
		// Ensure we only start one coin waiter for this swap. This is an atomic
		// CAS, so it must ultimately be followed by endSwapSearch().
		if !stepInfo.actor.status.startSwapSearch() {
			// Not really a sequence error since they are still the "actor".
			return msgjson.NewError(msgjson.DuplicateRequestError, "already received a swap contract, search in progress")
		}
	default:
		return msgjson.NewError(msgjson.SettlementSequenceError, "swap contract already provided")
	}

	// Validate the coinID and contract script before starting a coin waiter.
	coinStr, err := stepInfo.asset.Backend.ValidateCoinID(params.CoinID)
	if err != nil {
		stepInfo.actor.status.endSwapSearch() // not gonna start the search
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return msgjson.NewError(msgjson.ContractError, "invalid contract coinID or script")
	}
	err = stepInfo.asset.Backend.ValidateContract(params.Contract)
	if err != nil {
		stepInfo.actor.status.endSwapSearch() // not gonna start the search
		log.Debugf("ValidateContract (asset %v, coin %v) failure: %v", stepInfo.asset.Symbol, coinStr, err)
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return msgjson.NewError(msgjson.ContractError, "invalid swap contract")
	}

	// TODO: consider also checking recipient of contract here, but it is also
	// checked in processInit. Note that value cannot be checked as transaction
	// details, which includes the coin/output value, are not yet retrieved.

	// Search for the transaction for the full txWaitExpiration, even if it goes
	// past the inaction deadline. processInit recognizes when it is revoked.
	expireTime := time.Now().Add(s.txWaitExpiration).UTC()
	log.Debugf("Allowing until %v (%v) to locate contract from %v (%v), match %v, tx %s (%s)",
		expireTime, time.Until(expireTime), makerTaker(stepInfo.actor.isMaker),
		stepInfo.step, matchID, coinStr, stepInfo.asset.Symbol)

	// Since we have to consider broadcast latency of the asset's network, run
	// this as a coin waiter.
	s.latencyQ.Wait(&wait.Waiter{
		Expiration: expireTime,
		TryFunc: func() wait.TryDirective {
			return s.processInit(context.Background(), cmdCtx.Completion, params, stepInfo)
		},
		ExpireFunc: func() {
			stepInfo.actor.status.endSwapSearch() // allow init retries
			// NOTE: We may consider a shorter expire time so the client can
			// receive warning that there may be node or wallet connectivity
			// trouble while they still have a chance to fix it.
			if err := cmdCtx.Completion.Fail(context.Background(),
				msgjson.NewError(msgjson.TransactionUndiscovered, "failed to find contract coin %v", coinStr)); err != nil {
				log.Errorf("failed to send init timeout error for user %v: %v", user, err)
			}
		},
	})
	return nil
}

// handleInit routes init requests through the mesh command service.
func (s *Swapper) handleInit(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	return s.mesh.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: commandKindInit,
		User: user,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			return s.authMgr.Send(user, resp)
		},
	})
}

// executeRedeem handles the 'redeem' command. Most of the work is performed by
// processRedeem, but the request is parsed and user is authenticated first.
func (s *Swapper) executeRedeem(cmdCtx *mesh.CommandContext) *msgjson.Error {
	user, msg := cmdCtx.Request.User, cmdCtx.Request.Msg
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock() // block shutdown until registered with latencyQ
	if s.stop {
		return msgjson.NewError(msgjson.TryAgainLaterError, "The swapper is stopping. Try again later.")
	}

	params := new(msgjson.Redeem)
	err := msg.Unmarshal(&params)
	if err != nil || params == nil {
		return msgjson.NewError(msgjson.RPCParseError, "Error decoding 'redeem' request payload")
	}

	rpcErr := s.authUser(user, params)
	if rpcErr != nil {
		return rpcErr
	}

	log.Debugf("handleRedeem: 'redeem' received from %v for match %v, order %v",
		user, params.MatchID, params.OrderID)

	if len(params.MatchID) != order.MatchIDSize {
		return msgjson.NewError(msgjson.RPCParseError, "Invalid 'matchid' in 'redeem' message")
	}

	var matchID order.MatchID
	copy(matchID[:], params.MatchID)

	if s.settlementMayBeRecorded(matchID) {
		if handled, rpcErr := s.reAckRecordedRedeem(cmdCtx, user, matchID, params); handled {
			return rpcErr
		}
	}

	stepInfo, rpcErr := s.step(user, matchID)
	if rpcErr != nil {
		return rpcErr
	}

	// redeem requests should only be sent when all contracts have been
	// received, in the correct sequence, and by the correct party.
	switch stepInfo.step {
	case order.TakerSwapCast, order.MakerRedeemed:
		// Ensure we only start one coin waiter for this redeem. This is an
		// atomic CAS, so it must ultimately be followed by endRedeemSearch().
		if !stepInfo.actor.status.startRedeemSearch() {
			return msgjson.NewError(msgjson.DuplicateRequestError, "already received a redeem transaction, search in progress")
		}
	default:
		// Too early to redeem (e.g. contracts not both in yet). A finished
		// match is already off the map, so a retry fails earlier as unknown.
		return msgjson.NewError(msgjson.SettlementSequenceError, "swap contracts not yet received")
	}

	// Validate the redeem coin ID before starting a wait. This does not
	// check the blockchain, but does ensure the CoinID can be decoded for the
	// asset before starting up a coin waiter.
	coinStr, err := stepInfo.asset.Backend.ValidateCoinID(params.CoinID)
	if err != nil {
		stepInfo.actor.status.endRedeemSearch()
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return msgjson.NewError(msgjson.ContractError, "invalid 'redeem' parameters")
	}

	// Search for the transaction for the full txWaitExpiration, even if it goes
	// past the inaction deadline. processRedeem recognizes when it is revoked.
	expireTime := time.Now().Add(s.txWaitExpiration).UTC()
	log.Debugf("Allowing until %v (%v) to locate redeem from %v (%v), match %v, tx %s (%s)",
		expireTime, time.Until(expireTime), makerTaker(stepInfo.actor.isMaker),
		stepInfo.step, matchID, coinStr, stepInfo.asset.Symbol)

	// Since we have to consider latency, run this as a coin waiter.
	s.latencyQ.Wait(&wait.Waiter{
		Expiration: expireTime,
		TryFunc: func() wait.TryDirective {
			return s.processRedeem(context.Background(), cmdCtx.Completion, params, stepInfo)
		},
		ExpireFunc: func() {
			stepInfo.actor.status.endRedeemSearch()
			// NOTE: We may consider a shorter expire time so the client can
			// receive warning that there may be node or wallet connectivity
			// trouble while they still have a chance to fix it.
			if err := cmdCtx.Completion.Fail(context.Background(),
				msgjson.NewError(msgjson.TransactionUndiscovered, "failed to find redeemed coin %v", coinStr)); err != nil {
				log.Errorf("failed to send redeem timeout error for user %v: %v", user, err)
			}
		},
	})
	return nil
}

// handleRedeem routes redeem requests through the mesh command service.
func (s *Swapper) handleRedeem(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	return s.mesh.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: commandKindRedeem,
		User: user,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			return s.authMgr.Send(user, resp)
		},
	})
}

// revoke sends the 'revoke_match' notification to locally connected clients.
// Replicated event appliers must not proxy these notifications because both
// mesh nodes apply the same event.
func (s *Swapper) revoke(match *matchTracker) {
	route := msgjson.RevokeMatchRoute
	log.Infof("Sending a '%s' notification to each client for match %v",
		route, match.ID())

	sendRev := func(mid order.MatchID, ord order.Order) {
		msg := &msgjson.RevokeMatch{
			OrderID: ord.ID().Bytes(),
			MatchID: mid[:],
		}
		s.authMgr.Sign(msg)
		ntfn, err := msgjson.NewNotification(route, msg)
		if err != nil {
			log.Errorf("Failed to create '%s' notification for user %v, match %v: %v",
				route, ord.User(), mid, err)
			return
		}
		if err = s.authMgr.SendIfLocal(ord.User(), ntfn); err != nil {
			log.Debugf("Failed to send '%s' notification to user %v, match %v: %v",
				route, ord.User(), mid, err)
		}
	}

	mid := match.ID()
	sendRev(mid, match.Taker)
	sendRev(mid, match.Maker)
}

func (s *Swapper) validateMatchAcks(user account.AccountID, msg *msgjson.Message, matches []*messageAcker) ([]meshevents.MatchAckRecord, *msgjson.Error) {
	// NOTE: acks must be in same order as matches []*messageAcker.
	var acks []msgjson.Acknowledgement
	err := msg.UnmarshalResult(&acks)
	if err != nil {
		return nil, msgjson.NewError(msgjson.RPCParseError, "error parsing match request acknowledgment: %v", err)
	}
	if len(matches) != len(acks) {
		return nil, msgjson.NewError(msgjson.AckCountError, "expected %d acknowledgements, got %d", len(matches), len(acks))
	}

	log.Debugf("processMatchAcks: 'match' ack received from %v for %d matches",
		user, len(matches))

	records := make([]meshevents.MatchAckRecord, 0, len(matches))
	for i, matchInfo := range matches {
		ack := &acks[i]
		match := matchInfo.match

		// Cancel matches don't involve swaps, so they are never swap-tracked
		// and cannot be revoked for inaction.
		isCancelMatch := match.Taker.Type() == order.CancelOrderType

		matchID := match.ID()
		if !isCancelMatch && !s.matchTracked(match) {
			return nil, msgjson.NewError(msgjson.RPCUnknownMatch, "match %v already revoked due to inaction", matchID)
		}
		if !bytes.Equal(ack.MatchID, matchID[:]) {
			return nil, msgjson.NewError(msgjson.IDMismatchError, "unexpected match ID at acknowledgment index %d", i)
		}
		sigMsg := matchInfo.params.Serialize()
		err = s.authMgr.VerifyUserSig(user, sigMsg, ack.Sig)
		if err != nil {
			log.Warnf("processMatchAcks: 'match' ack for match %v from user %v, "+
				" failed sig verification: %v", matchID, user, err)
			return nil, msgjson.NewError(msgjson.SignatureError, "signature validation error: %v", err)
		}

		// No per-match address is needed for a cancel match. Only validate
		// and store addresses for trade matches.
		ackAddr := ack.Address

		if !isCancelMatch {
			// First recorded address wins: coerce any re-ack to it — it
			// already passed validation when first accepted. Validate the
			// submitted address only when nothing is recorded yet.
			match.mtx.RLock()
			recordedAddr := match.takerSwapAddr
			if matchInfo.isMaker {
				recordedAddr = match.makerSwapAddr
			}
			match.mtx.RUnlock()
			if recordedAddr != "" {
				if ackAddr != recordedAddr {
					log.Warnf("validateMatchAcks: user %v (maker=%v) re-acked match %v with address %q, "+
						"keeping recorded %q",
						user, matchInfo.isMaker, matchID, ackAddr, recordedAddr)
					ackAddr = recordedAddr
				}
			} else {
				// No address recorded yet: validate the submitted one.
				if ackAddr == "" {
					return nil, msgjson.NewError(msgjson.OrderParameterError, "missing per-match swap address for match %v", matchID)
				}
				// The user's swap address is on their redeem asset (the chain
				// where the counterparty's contract pays them).
				redeemAssetID := match.takerStatus.redeemAsset
				if matchInfo.isMaker {
					redeemAssetID = match.makerStatus.redeemAsset
				}
				swapperAsset := s.coins[redeemAssetID]
				if swapperAsset == nil || !swapperAsset.Backend.CheckSwapAddress(ackAddr) {
					return nil, msgjson.NewError(msgjson.OrderParameterError, "invalid per-match swap address %q for asset %d", ackAddr, redeemAssetID)
				}
			}
		}

		records = append(records, meshevents.MatchAckRecord{
			MatchID: matchID,
			Base:    match.Maker.BaseAsset,
			Quote:   match.Maker.QuoteAsset,
			Maker:   matchInfo.isMaker,
			Cancel:  isCancelMatch,
			Sig:     append(dex.Bytes(nil), ack.Sig...),
			Address: ackAddr,
		})
		log.Debugf("processMatchAcks: storing valid 'match' ack signature from %v (maker=%v) "+
			"for match %v", user, matchInfo.isMaker, matchID)
	}

	return records, nil
}

// For the 'match' request, the user returns a msgjson.Acknowledgement array
// with signatures for each match ID. The match acknowledgements were requested
// from each matched user in RequestMatchAcks.
func (s *Swapper) processMatchAcks(user account.AccountID, msg *msgjson.Message, matches []*messageAcker) {
	records, msgErr := s.validateMatchAcks(user, msg, matches)
	if msgErr != nil {
		s.respondError(msg.ID, user, msgErr.Code, msgErr.Message)
		return
	}
	if len(records) == 0 {
		return
	}
	event, err := newMatchAcksRecordedEvent(unixMsNow(), records)
	if err != nil {
		log.Errorf("error creating match acks recorded event: %v", err)
		s.respondError(msg.ID, user, msgjson.RPCInternalError, "internal server error")
		return
	}
	if _, err := s.mesh.ApplyEvent(context.Background(), event); err != nil {
		mesh.LogApplyFailure(log, err, "error applying match acks recorded event for user %v: %v", user, err)
		msgErr := mesh.ClientError(err, msgjson.RPCInternalError, "internal server error")
		s.respondError(msg.ID, user, msgErr.Code, msgErr.Message)
	}
}

// sendCounterPartyAddresses sends a CounterPartyAddress notification to each
// side of a match, delivering the counterparty's per-match swap address. This
// is called after both sides have acknowledged the match with per-match
// addresses. The match mtx should NOT be held.
func (s *Swapper) sendCounterPartyAddresses(match *matchTracker) {
	s.sendCounterPartyAddress(match, match.Maker.User(), true)
	s.sendCounterPartyAddress(match, match.Taker.User(), true)
	log.Debugf("Sent %s notifications for match %v (maker addr -> taker, taker addr -> maker)",
		msgjson.CounterPartyAddressRoute, match.ID())
}

// UserConnected re-sends counterparty_address notes for matches with both
// per-match addresses, and on the acting master re-issues the user's still-
// pending match/audit/redemption requests.
func (s *Swapper) UserConnected(user account.AccountID) {
	s.matchMtx.RLock()
	userMatches := make([]*matchTracker, 0, len(s.userMatches[user]))
	for _, mt := range s.userMatches[user] {
		userMatches = append(userMatches, mt)
	}
	s.matchMtx.RUnlock()

	isMaster := s.master.Load()

	for _, mt := range userMatches {
		mt.mtx.RLock()
		bothReady := mt.makerSwapAddr != "" && mt.takerSwapAddr != ""
		mt.mtx.RUnlock()
		if bothReady {
			s.sendCounterPartyAddress(mt, user, true)
		}
		if isMaster {
			s.resendPendingRequestsNow(mt, &user)
		}
	}
}

// sendCounterPartyAddress sends the counterparty's per-match swap address to
// user and stamps the recipient side's last-CPA time. The match mtx should
// NOT be held. localOnly selects SendIfLocal; the tick passes false (Send)
// because the user may be on the slave.
func (s *Swapper) sendCounterPartyAddress(match *matchTracker, user account.AccountID, localOnly bool) {
	send := s.authMgr.Send
	if localOnly {
		send = s.authMgr.SendIfLocal
	}
	match.mtx.Lock()
	if user == match.Maker.User() {
		match.lastMakerCPA = time.Now()
	}
	if user == match.Taker.User() {
		match.lastTakerCPA = time.Now()
	}
	makerAddr := match.makerSwapAddr
	takerAddr := match.takerSwapAddr
	match.mtx.Unlock()

	mid := match.ID()
	route := msgjson.CounterPartyAddressRoute

	// Determine which side(s) the user is on and send the counterparty's
	// address. In self-trade scenarios (maker == taker), the user needs
	// notifications for both sides.
	type addrNotification struct {
		orderID order.OrderID
		address string
	}
	var toSend []addrNotification
	if user == match.Maker.User() {
		toSend = append(toSend, addrNotification{match.Maker.ID(), takerAddr})
	}
	if user == match.Taker.User() {
		toSend = append(toSend, addrNotification{match.Taker.ID(), makerAddr})
	}
	if len(toSend) == 0 {
		return
	}

	for _, an := range toSend {
		cpa := &msgjson.CounterPartyAddress{
			OrderID: an.orderID.Bytes(),
			MatchID: mid[:],
			Address: an.address,
		}
		s.authMgr.Sign(cpa)
		ntfn, err := msgjson.NewNotification(route, cpa)
		if err != nil {
			log.Errorf("Failed to create %s notification for %v, match %v: %v",
				route, user, mid, err)
			continue
		}
		if err = send(user, ntfn); err != nil {
			log.Debugf("Failed to send %s to %v, match %v: %v",
				route, user, mid, err)
		}
	}
}

// CheckUnspent attempts to verify a coin ID for a given asset by retrieving the
// corresponding asset.Coin. If the coin is not found or spent, an
// asset.CoinNotFoundError is returned. CheckUnspent returns immediately with
// no error if the requested asset is not a utxo-based asset.
func (s *Swapper) CheckUnspent(ctx context.Context, assetID uint32, coinID []byte) error {
	backend := s.coins[assetID]
	if backend == nil {
		return fmt.Errorf("unknown asset %d", assetID)
	}
	outputTracker, is := backend.Backend.(asset.OutputTracker)
	if !is {
		return nil
	}
	return outputTracker.VerifyUnspentCoin(ctx, coinID)
}

// LockOrdersCoins locks the backing coins for the provided orders.
func (s *Swapper) LockOrdersCoins(orders []order.Order) {
	// Separate orders according to the asset of their locked coins.
	assetCoinOrders := make(map[uint32][]order.Order, len(orders))
	for _, ord := range orders {
		// Identify the asset of the locked coins.
		asset := ord.Quote()
		if ord.Trade().Sell {
			asset = ord.Base()
		}
		assetCoinOrders[asset] = append(assetCoinOrders[asset], ord)
	}

	for asset, orders := range assetCoinOrders {
		s.lockOrdersCoins(asset, orders)
	}
}

func (s *Swapper) lockOrdersCoins(assetID uint32, orders []order.Order) {
	swapperAsset := s.coins[assetID]
	if swapperAsset == nil {
		log.Errorf(fmt.Sprintf("lockOrderCoins called for unknown asset %d", assetID))
		return
	}
	if swapperAsset.Locker == nil {
		return
	}

	if failed := swapperAsset.Locker.LockOrdersCoins(orders); len(failed) > 0 {
		for _, ord := range failed {
			log.Errorf("failed to lock swap coins for order %v (asset %d)", ord.ID(), assetID)
		}
	}
}

// LockCoins locks coins of a given asset. The OrderID is used for tracking.
func (s *Swapper) LockCoins(asset uint32, coins map[order.OrderID][]order.CoinID) {
	swapperAsset := s.coins[asset]
	if swapperAsset == nil {
		panic(fmt.Sprintf("Unable to lock coins for asset %d", asset))
	}
	if swapperAsset.Locker == nil {
		return
	}

	if failed := swapperAsset.Locker.LockCoins(coins); len(failed) > 0 {
		for oid, coinIDs := range failed {
			log.Errorf("failed to lock swap coins for order %v (asset %d): %d coins", oid, asset, len(coinIDs))
		}
	}
}

// unlockOrderCoins is not exported since only the Swapper knows when to unlock
// coins (when funding coins are spent in a fully-confirmed contract).
func (s *Swapper) unlockOrderCoins(ord order.Order) {
	assetID := ord.Quote()
	if ord.Trade().Sell {
		assetID = ord.Base()
	}

	s.unlockOrderIDCoins(assetID, ord.ID())
}

func (s *Swapper) unlockOrderIDCoins(assetID uint32, oid order.OrderID) {
	swapperAsset := s.coins[assetID]
	if swapperAsset == nil {
		log.Errorf(fmt.Sprintf("unlockOrderIDCoins called for unknown asset %d", assetID))
		return
	}
	if swapperAsset.Locker == nil {
		return
	}

	swapperAsset.Locker.UnlockOrderCoins(oid)
}

// matchNotifications creates a pair of msgjson.Match from a matchTracker.
func matchNotifications(match *matchTracker) (makerMsg *msgjson.Match, takerMsg *msgjson.Match) {
	// NOTE: If we decide that msgjson.Match should just have a
	// "FeeRateBaseSwap" field, this could be set according to the
	// swapStatus.swapAsset field:
	//
	// base, quote := match.Maker.BaseAsset, match.Maker.QuoteAsset
	// feeRate := func(assetID uint32) uint64 {
	// 	if assetID == match.Maker.BaseAsset {
	// 		return match.FeeRateBase
	// 	}
	// 	return match.FeeRateQuote
	// }
	// FeeRateMakerSwap := feeRate(match.makerStatus.swapAsset)

	// If the taker order is a cancel, omit the maker (trade) order's address
	// since it is dead weight. Consider omitting the numeric fields too.
	var makerAddr string
	if match.Taker.Type() != order.CancelOrderType {
		makerAddr = order.ExtractAddress(match.Maker)
	}

	stamp := uint64(match.matchTime.UnixMilli())
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
			Address:      makerAddr,
			ServerTime:   stamp,
			FeeRateBase:  match.FeeRateBase,
			FeeRateQuote: match.FeeRateQuote,
			Side:         uint8(order.Taker),
		}
}

// readMatches translates a slice of raw matches from the market manager into
// a slice of matchTrackers.
func readMatches(matchSets []*order.MatchSet) []*matchTracker {
	// The initial capacity guess here is a minimum, but will avoid a few
	// reallocs.
	nowMs := unixMsNow()
	matches := make([]*matchTracker, 0, len(matchSets))
	for _, matchSet := range matchSets {
		for _, match := range matchSet.Matches() {
			maker := match.Maker
			base, quote := maker.BaseAsset, maker.QuoteAsset
			var makerSwapAsset, takerSwapAsset uint32
			if maker.Sell {
				makerSwapAsset = base
				takerSwapAsset = quote
			} else {
				makerSwapAsset = quote
				takerSwapAsset = base
			}

			matches = append(matches, &matchTracker{
				Match:     match,
				time:      nowMs,
				matchTime: match.Epoch.End(),
				makerStatus: &swapStatus{
					swapAsset:   makerSwapAsset,
					redeemAsset: takerSwapAsset,
				},
				takerStatus: &swapStatus{
					swapAsset:   takerSwapAsset,
					redeemAsset: makerSwapAsset,
				},
			})
		}
	}
	return matches
}

// TrackMatches applies in-memory swapper state for already-persisted matches.
// Called by the epoch_processed applier on every node.
func (s *Swapper) TrackMatches(matchSets []*order.MatchSet) error {
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock()
	if s.stop {
		return fmt.Errorf("TrackMatches called on stopped swapper")
	}

	supportedMatchSets := matchSets[:0]
	swapOrders := make([]order.Order, 0, 2*len(matchSets))
	for _, match := range matchSets {
		supportedMatchSets = append(supportedMatchSets, match)

		if match.Taker.Type() == order.CancelOrderType {
			continue
		}

		swapOrders = append(swapOrders, match.Taker)
		for _, maker := range match.Makers {
			swapOrders = append(swapOrders, maker)
		}
	}
	s.LockOrdersCoins(swapOrders)

	s.trackMatches(readMatches(supportedMatchSets))
	return nil
}

func (s *Swapper) trackMatches(matches []*matchTracker) {
	toMonitor := make([]*matchTracker, 0, len(matches))
	for _, match := range matches {
		if match.Taker.Type() == order.CancelOrderType {
			continue
		}
		toMonitor = append(toMonitor, match)
	}

	// Add the matches to the matches/userMatches maps.
	s.matchMtx.Lock()
	for _, match := range toMonitor {
		s.addMatch(match)
	}
	s.matchMtx.Unlock()
}

// RequestMatchAcks sends match requests on the emitting master. TrackMatches
// must already have registered non-cancel matches.
func (s *Swapper) RequestMatchAcks(matchSets []*order.MatchSet) {
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock()
	if s.stop {
		log.Errorf("RequestMatchAcks called on stopped swapper. Match requests not sent.")
		return
	}

	userMatches := make(map[account.AccountID][]*messageAcker)
	// addUserMatch signs a match notification message, and adds the data
	// required to process the acknowledgment to the userMatches map.
	addUserMatch := func(acker *messageAcker) {
		s.authMgr.Sign(acker.params)
		userMatches[acker.user] = append(userMatches[acker.user], acker)
	}

	matches := readMatches(matchSets)
	ackMatches := make([]*matchTracker, 0, len(matches))
	s.matchMtx.RLock()
	for _, match := range matches {
		if match.Taker.Type() != order.CancelOrderType {
			tracked := s.matches[match.ID()]
			if tracked == nil {
				log.Errorf("RequestMatchAcks: match %v was not registered", match.ID())
				continue
			}
			match = tracked
		}
		ackMatches = append(ackMatches, match)
	}
	s.matchMtx.RUnlock()

	for _, match := range ackMatches {
		// Create an acker for maker and taker, sharing the same matchTracker.
		match.mtx.Lock()
		now := time.Now()
		match.lastMakerMatch, match.lastTakerMatch = now, now
		match.mtx.Unlock()
		makerMsg, takerMsg := matchNotifications(match) // msgjson.Match for each party
		addUserMatch(&messageAcker{
			user:    match.Maker.User(),
			match:   match,
			params:  makerMsg,
			isMaker: true,
			// isAudit: false,
		})
		addUserMatch(&messageAcker{
			user:    match.Taker.User(),
			match:   match,
			params:  takerMsg,
			isMaker: false,
			// isAudit: false,
		})
	}

	// Send the user match notifications.
	for user, matches := range userMatches {
		// msgs is a slice of msgjson.Match created by newMatchAckers
		// (matchNotifications) for all makers and takers.
		msgs := make([]msgjson.Signable, 0, len(matches))
		for _, m := range matches {
			msgs = append(msgs, m.params)
		}

		// Solicit match acknowledgments. Each Match is signed in addUserMatch.
		req, err := msgjson.NewRequest(comms.NextID(), msgjson.MatchRoute, msgs)
		if err != nil {
			log.Errorf("error creating match notification request: %v", err)
			// Should never happen, but the client can still use match_status.
			continue
		}

		// Copy the loop variables for capture by the match acknowledgement
		// response handler.
		u, m := user, matches
		log.Debugf("RequestMatchAcks: sending 'match' ack request to user %v for %d matches",
			u, len(m))

		// Send the request.
		err = s.authMgr.Request(u, req, func(_ comms.Link, resp *msgjson.Message) {
			s.processMatchAcks(u, resp, m)
		})
		if err != nil {
			log.Infof("Failed to send %v request to %v. The match will be returned in the connect response.",
				req.Route, u)
		}
	}
}

func idToBytes(id [order.OrderIDSize]byte) []byte {
	return id[:]
}
