// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package auth authenticates clients and tracks accounts, bonds, and
// reputation. Requests that change that state are mesh commands on the
// master; durable changes are events every node applies.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const (
	cancelThreshWindow = 100 // spec
	ScoringMatchLimit  = 60  // last N matches (success or at-fault fail) to be considered in swap inaction scoring
	scoringOrderLimit  = 40  // last N orders to be considered in preimage miss scoring

	maxIDsPerOrderStatusRequest = 10_000

	// freeCancelThreshold is the minimum number of epochs a user should wait before
	// placing a cancel order, if they want to avoid penalization. It is set to 2,
	// which means if a user places a cancel order in the same epoch as its limit
	// order, or the next epoch, the user will be penalized. This value is chosen
	// because it is the minimum value such that the order remains booked for at
	// least one full epoch and one full match cycle.
	freeCancelThreshold = 2
)

var (
	ErrUserNotConnected = dex.ErrorKind("user not connected")
)

const (
	reputationForgivenessCommandTimeout = 2 * time.Minute
	reputationEventRefreshTimeout       = 5 * time.Second
)

// Storage updates and fetches account-related data from what is presumably a
// database.
type Storage interface {
	// Account retrieves account info for the ID. A nil account with a nil error
	// means unknown; a non-nil error means existence could not be determined.
	// lockTimeThresh decides which bonds are still active.
	Account(account.AccountID, time.Time) (acct *account.Account, bonds []*db.Bond, err error)

	// ApplyBondPostedEvent applies the auth bond_posted event in one database
	// transaction.
	ApplyBondPostedEvent(context.Context, *db.EventLogMeta, *db.BondPostedUpdate) (*db.BondPostedResult, error)
	ApplyPrepaidBondsCreatedEvent(context.Context, *db.EventLogMeta, *meshevents.PrepaidBondsCreatedEvent) (*db.EventLogEntry, error)

	FetchPrepaidBond(bondCoinID []byte) (strength uint32, lockTime int64, err error)

	AccountInfo(aid account.AccountID) (*db.Account, error)

	UserOrderStatuses(aid account.AccountID, base, quote uint32, oids []order.OrderID) ([]*db.OrderStatus, error)
	ActiveUserOrderStatuses(aid account.AccountID) ([]*db.OrderStatus, error)
	CompletedAndAtFaultMatchStats(aid account.AccountID, lastN int) ([]*db.MatchOutcome, error)
	UserMatchFails(aid account.AccountID, lastN int) ([]*db.MatchFail, error)
	AllActiveUserMatches(aid account.AccountID) ([]*db.MatchData, error)
	MatchStatuses(aid account.AccountID, base, quote uint32, matchIDs []order.MatchID) ([]*db.MatchStatus, error)

	db.ReputationArchiver
}

// Signer signs messages. The message must be a 32-byte hash.
type Signer interface {
	Sign(hash []byte) *ecdsa.Signature
	PubKey() *secp256k1.PublicKey
}

// FeeChecker is a function for retrieving the details for a fee payment txn.
type FeeChecker func(assetID uint32, coinID []byte) (addr string, val uint64, confs int64, err error)

// BondCoinChecker is a function for locating an unspent bond, and extracting
// the amount, lockTime, and account ID. The confirmations of the bond
// transaction are also provided.
type BondCoinChecker func(ctx context.Context, assetID uint32, ver uint16,
	coinID []byte) (amt, lockTime, confs int64, acct account.AccountID, err error)

// BondTxParser parses a dex fidelity bond transaction and the redeem script of
// the first output of the transaction, which must be the actual bond output.
// The returned account ID is from the second output. This will become a
// multi-asset checker.
//
// NOTE: For DCR, and possibly all assets, the bond script is reconstructed from
// the null data output, and it is verified that the bond output pays to this
// script. As such, there is no provided bondData (redeem script for UTXO
// assets), but this may need for other assets.
type BondTxParser func(assetID uint32, ver uint16, rawTx []byte) (bondCoinID []byte,
	amt int64, lockTime int64, acct account.AccountID, err error)

// TxDataSource retrieves the raw transaction for a coin ID.
type TxDataSource func(coinID []byte) (rawTx []byte, err error)

// A respHandler is the handler for the response to a DEX-originating request. A
// respHandler has a time associated with it so that old unused handlers can be
// detected and deleted.
type respHandler struct {
	f      func(comms.Link, *msgjson.Message)
	expire *time.Timer
}

type proxyResponseKey struct {
	user account.AccountID
	id   uint64
}

// clientInfo represents a DEX client, including account information and last
// known comms.Link.
type clientInfo struct {
	acct         *account.Account
	conn         comms.Link
	respHandlers map[uint64]*respHandler
	mtx          sync.Mutex
}

func (client *clientInfo) rmHandler(id uint64) bool {
	client.mtx.Lock()
	defer client.mtx.Unlock()
	_, found := client.respHandlers[id]
	if found {
		delete(client.respHandlers, id)
	}
	return found
}

// logReq associates the specified response handler with the message ID.
func (client *clientInfo) logReq(id uint64, f func(comms.Link, *msgjson.Message), expireTime time.Duration, expire func()) {
	client.mtx.Lock()
	defer client.mtx.Unlock()
	doExpire := func() {
		// Delete the response handler, and call the provided expire function if
		// (*clientInfo).respHandler has not already retrieved the handler
		// function for execution.
		if client.rmHandler(id) {
			expire()
		}
	}
	client.respHandlers[id] = &respHandler{
		f:      f,
		expire: time.AfterFunc(expireTime, doExpire),
	}
}

// respHandler extracts the response handler from the respHandlers map. If the
// handler is found, it is also deleted from the map before being returned, and
// the expiration Timer is stopped.
func (client *clientInfo) respHandler(id uint64) *respHandler {
	client.mtx.Lock()
	defer client.mtx.Unlock()

	handler := client.respHandlers[id]
	if handler == nil {
		return nil
	}

	// Stop the expiration Timer. If the Timer fired after respHandler was
	// called, but we found the response handler in the map, clientInfo.expire
	// is waiting for the lock and will return false, thus preventing the
	// registered expire func from executing.
	handler.expire.Stop()
	delete(client.respHandlers, id)
	return handler
}

func (auth *AuthManager) rmProxyRespHandler(key proxyResponseKey) bool {
	auth.proxyRespMtx.Lock()
	defer auth.proxyRespMtx.Unlock()
	_, found := auth.proxyRespHandlers[key]
	if found {
		delete(auth.proxyRespHandlers, key)
	}
	return found
}

func (auth *AuthManager) registerProxyRespHandler(user account.AccountID, id uint64, f func(comms.Link, *msgjson.Message), expireTime time.Duration, expire func()) {
	key := proxyResponseKey{user: user, id: id}
	doExpire := func() {
		if auth.rmProxyRespHandler(key) {
			expire()
		}
	}
	auth.proxyRespMtx.Lock()
	defer auth.proxyRespMtx.Unlock()
	auth.proxyRespHandlers[key] = &respHandler{
		f:      f,
		expire: time.AfterFunc(expireTime, doExpire),
	}
}

func (auth *AuthManager) proxyRespHandler(user account.AccountID, id uint64) *respHandler {
	key := proxyResponseKey{user: user, id: id}
	auth.proxyRespMtx.Lock()
	defer auth.proxyRespMtx.Unlock()

	handler := auth.proxyRespHandlers[key]
	if handler == nil {
		return nil
	}
	handler.expire.Stop()
	delete(auth.proxyRespHandlers, key)
	return handler
}

// AuthManager authenticates clients, tracks sessions, and signs DEX
// messages. Connect stays a local route. Post-bond, prepaid bonds, and
// forgive are mesh commands on the master; the resulting state is
// applied as events on every node.
type AuthManager struct {
	wg          sync.WaitGroup
	ctx         context.Context
	storage     Storage
	signer      Signer
	parseBondTx BondTxParser
	checkBond   BondCoinChecker // fidelity bond amount, lockTime, acct, and confs
	route       func(route string, handler comms.MsgHandler)

	bondExpiry time.Duration // a bond is expired when time.Until(lockTime) < bondExpiry
	bondAssets map[uint32]*msgjson.BondAsset

	freeCancels      bool
	penaltyThreshold int32
	cancelThresh     float64

	// rep is an LRU of conduct scores and active bonds; storage invalidates post-commit.
	rep *repCache

	// latencyQ is a queue for fee coin waiters to deal with latency.
	latencyQ *wait.TickerQueue

	bondWaiterMtx sync.Mutex
	bondWaiterIdx map[string]struct{}

	connMtx sync.RWMutex
	users   map[account.AccountID]*clientInfo
	conns   map[uint64]*clientInfo

	// repNotifyMtx serializes the asynchronous reputation-change notes
	// spawned from the reputation-inputs listener.
	repNotifyMtx sync.Mutex

	txDataSources map[uint32]TxDataSource

	mesh MeshService

	proxyRespMtx      sync.Mutex
	proxyRespHandlers map[proxyResponseKey]*respHandler

	connectCallbackMtx sync.RWMutex
	connectCallbacks   []func(account.AccountID)
}

// violation badness
const (
	// preimage miss
	preimageMissScore    = -2 // book spoof, no match, no stuck funds
	preimageSuccessScore = 0

	// failure to act violations
	matchCompletedScore  = 1   // offsets the violations
	noSwapAsMakerScore   = -4  // no swap broadcast at NewlyMatched
	noAddrAsTakerScore   = -4  // taker failed to provide per-match address in time, blocking the maker
	noSwapAsTakerScore   = -11 // maker has contract stuck for 20 hrs
	noRedeemAsMakerScore = -7  // taker has contract stuck for 8 hrs
	noRedeemAsTakerScore = -1  // just dumb, counterparty not inconvenienced

	// cancel rate exceeds threshold
	excessiveCancelsScore = -5
	orderCompleteScore    = 0

	DefaultPenaltyThreshold = 20
)

type Outcome = db.Outcome

var outcomeScores = map[Outcome]int32{
	db.OutcomeForgiven: 1,

	// preimage results
	db.OutcomePreimageMiss:    preimageMissScore,
	db.OutcomePreimageSuccess: preimageSuccessScore,

	// match results
	db.OutcomeSwapSuccess:     matchCompletedScore,
	db.OutcomeNoSwapAsMaker:   noSwapAsMakerScore,
	db.OutcomeNoSwapAsTaker:   noSwapAsTakerScore,
	db.OutcomeNoRedeemAsMaker: noRedeemAsMakerScore,
	db.OutcomeNoRedeemAsTaker: noRedeemAsTakerScore,
	db.OutcomeNoAddrAsTaker:   noAddrAsTakerScore,

	// orders cancellations (completed/canceled)
	db.OutcomeOrderCanceled: excessiveCancelsScore,
	db.OutcomeOrderComplete: orderCompleteScore,

	db.OutcomeInvalid: 0,
}

// Config is the configuration settings for the AuthManager, and the only
// argument to its constructor.
type Config struct {
	// Storage is an interface for storing and retrieving account-related info.
	Storage Storage
	// Signer is an interface that signs messages. In practice, Signer is
	// satisfied by a secp256k1.PrivateKey.
	Signer Signer

	Route func(route string, handler comms.MsgHandler)

	// BondExpiry is the seconds remaining until a bond's LockTime at which
	// the bond is considered expired. Mesh nodes must share this value
	// (today: dex.BondExpiry); it is an input to committed-event verdicts.
	BondExpiry uint64
	// BondAssets indicates the supported bond assets and parameters.
	BondAssets map[string]*msgjson.BondAsset
	// BondTxParser performs rudimentary validation of a raw time-locked
	// fidelity bond transaction. e.g. dcr.ParseBondTx
	BondTxParser BondTxParser
	// BondChecker locates an unspent bond, and extracts the amount, lockTime,
	// and account ID, plus txn confirmations.
	BondChecker BondCoinChecker

	// TxDataSources are sources of tx data for a coin ID.
	TxDataSources map[uint32]TxDataSource

	CancelThreshold float64
	FreeCancels     bool

	// PenaltyThreshold defines the score deficit at which a user's bond is
	// revoked. Compat pins it between live peers; a solo log replay after
	// an operator edit is the remaining mismatch hazard.
	PenaltyThreshold uint32
}

// NewAuthManager is the constructor for an AuthManager.
func NewAuthManager(cfg *Config) *AuthManager {
	// A penalty threshold of 0 is not sensible, so have a default.
	penaltyThreshold := int32(cfg.PenaltyThreshold)
	if penaltyThreshold <= 0 {
		penaltyThreshold = DefaultPenaltyThreshold
	}
	// Invert sign for internal use.
	if penaltyThreshold > 0 {
		penaltyThreshold *= -1
	}
	// Re-key the maps for efficiency in AuthManager methods.
	bondAssets := make(map[uint32]*msgjson.BondAsset, len(cfg.BondAssets))
	for _, asset := range cfg.BondAssets {
		bondAssets[asset.ID] = asset
	}

	auth := &AuthManager{
		storage:           cfg.Storage,
		signer:            cfg.Signer,
		bondAssets:        bondAssets,
		bondExpiry:        time.Duration(cfg.BondExpiry) * time.Second,
		parseBondTx:       cfg.BondTxParser, // e.g. dcr's ParseBondTx
		checkBond:         cfg.BondChecker,  // e.g. dcr's BondCoin
		route:             cfg.Route,
		freeCancels:       cfg.FreeCancels,
		penaltyThreshold:  penaltyThreshold,
		cancelThresh:      cfg.CancelThreshold,
		rep:               newRepCache(repCacheCapacity, repCacheMaxAge),
		latencyQ:          wait.NewTickerQueue(recheckInterval),
		users:             make(map[account.AccountID]*clientInfo),
		conns:             make(map[uint64]*clientInfo),
		bondWaiterIdx:     make(map[string]struct{}),
		txDataSources:     cfg.TxDataSources,
		proxyRespHandlers: make(map[proxyResponseKey]*respHandler),
	}

	cfg.Storage.SetReputationInputsListener(func(users ...account.AccountID) {
		if len(users) == 0 {
			return
		}
		auth.rep.invalidate(users...)
		notifyUsers := append([]account.AccountID(nil), users...)
		go auth.notifyReputationInputsChanged(notifyUsers)
	})

	// Unauthenticated
	cfg.Route(msgjson.ConnectRoute, auth.handleConnect)
	cfg.Route(msgjson.PostBondRoute, auth.handlePostBond)
	cfg.Route(msgjson.PreValidateBondRoute, auth.handlePreValidateBond)
	cfg.Route(msgjson.MatchStatusRoute, auth.handleMatchStatus)
	cfg.Route(msgjson.OrderStatusRoute, auth.handleOrderStatus)
	return auth
}

// SetMeshService configures the mesh service. It must be set before the comms
// routes serve traffic.
func (auth *AuthManager) SetMeshService(mesh MeshService) {
	auth.mesh = mesh
}

// GraceLimit returns the number of initial orders allowed for a new user before
// the cancellation rate threshold is enforced.
func (auth *AuthManager) GraceLimit() int {
	// Grace period if: total/(1+total) <= thresh OR total <= thresh/(1-thresh).
	return int(math.Round(1e8*auth.cancelThresh/(1-auth.cancelThresh))) / 1e8
}

// Connect runs the AuthManager until the context is canceled. Satisfies the
// dex.Connector interface.
func (auth *AuthManager) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	auth.ctx = ctx
	auth.wg.Add(1)
	go func() {
		defer auth.wg.Done()
		auth.latencyQ.Run(ctx)
	}()

	auth.wg.Add(1)
	go func() {
		defer auth.wg.Done()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				hits, misses, invalidations, evictions := auth.rep.stats()
				log.Debugf("Reputation cache: %d hits, %d misses, %d invalidations, %d evictions",
					hits, misses, invalidations, evictions)
			case <-ctx.Done():
				return
			}
		}
	}()

	// TODO: wait for running comms route handlers and other DB writers.
	return &auth.wg, nil
}

// OnConnect registers a callback that is invoked after a user successfully
// connects (or reconnects). This allows other subsystems (e.g. the Swapper) to
// perform actions when a user comes online, such as re-sending missed
// notifications.
func (auth *AuthManager) OnConnect(f func(account.AccountID)) {
	auth.connectCallbackMtx.Lock()
	auth.connectCallbacks = append(auth.connectCallbacks, f)
	auth.connectCallbackMtx.Unlock()
}

// ConnectedAmong returns the subset of the given users that are currently
// connected to this node.
func (auth *AuthManager) ConnectedAmong(users []account.AccountID) []account.AccountID {
	var connected []account.AccountID
	auth.connMtx.RLock()
	defer auth.connMtx.RUnlock()
	for _, user := range users {
		if _, found := auth.users[user]; found {
			connected = append(connected, user)
		}
	}
	return connected
}

// Route registers an authenticated websocket handler and delivers the
// reply on this connection. State-changing routes submit a mesh command;
// they do not write the DB here.
func (auth *AuthManager) Route(route string, handler func(account.AccountID, *msgjson.Message) *msgjson.Error) {
	auth.route(route, func(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
		client := auth.conn(conn)
		if client == nil {
			return &msgjson.Error{
				Code:    msgjson.UnauthorizedConnection,
				Message: "cannot use route '" + route + "' on an unauthorized connection",
			}
		}
		msgErr := handler(client.acct.ID, msg)
		if msgErr != nil {
			log.Debugf("Handling of '%s' request for user %v failed: %v", route, client.acct.ID, msgErr)
		}
		return msgErr
	})
}

// Message signing and signature verification.

// checkSigS256 checks that the message's signature was created with the
// private key for the provided secp256k1 public key.
func checkSigS256(msg, sig []byte, pubKey *secp256k1.PublicKey) error {
	signature, err := ecdsa.ParseDERSignature(sig)
	if err != nil {
		return fmt.Errorf("error decoding secp256k1 Signature from bytes: %w", err)
	}
	hash := sha256.Sum256(msg)
	if !signature.Verify(hash[:], pubKey) {
		return fmt.Errorf("secp256k1 signature verification failed")
	}
	return nil
}

// Auth validates the signature/message pair with the users public key.
func (auth *AuthManager) Auth(user account.AccountID, msg, sig []byte) error {
	client := auth.user(user)
	if client == nil {
		return dex.NewError(ErrUserNotConnected, user.String())
	}
	return checkSigS256(msg, sig, client.acct.PubKey)
}

// VerifyUserSig validates the signature/message pair with the user's public
// key, using the live session when the user is connected and falling back to
// stored account data otherwise.
func (auth *AuthManager) VerifyUserSig(user account.AccountID, msg, sig []byte) error {
	client := auth.user(user)
	if client != nil {
		return checkSigS256(msg, sig, client.acct.PubKey)
	}

	acctInfo, err := auth.storage.AccountInfo(user)
	if err != nil {
		return err
	}
	if acctInfo == nil {
		return fmt.Errorf("account %s not found", user)
	}

	pubKey, err := secp256k1.ParsePubKey(acctInfo.Pubkey)
	if err != nil {
		return fmt.Errorf("error decoding secp256k1 public key: %w", err)
	}
	return checkSigS256(msg, sig, pubKey)
}

// SignMsg signs the message with the DEX private key, returning the DER encoded
// signature. SHA256 is used to hash the message before signing it.
func (auth *AuthManager) SignMsg(msg []byte) []byte {
	hash := sha256.Sum256(msg)
	return auth.signer.Sign(hash[:]).Serialize()
}

// Sign signs the msgjson.Signables with the DEX private key.
func (auth *AuthManager) Sign(signables ...msgjson.Signable) {
	for _, signable := range signables {
		sig := auth.SignMsg(signable.Serialize())
		signable.SetSig(sig)
	}
}

// Response and notification (non-request) messages

func (auth *AuthManager) send(client *clientInfo, msg *msgjson.Message) error {
	err := client.conn.Send(msg)
	if err != nil {
		log.Debugf("error sending on link: %v", err)
		// Remove client assuming connection is broken, requiring reconnect.
		auth.removeClient(client)
		// client.conn.Disconnect() // async removal
	}
	return err
}

// Send delivers a non-Request message to the user. Local clients are sent
// asynchronously. Others are proxied (blocking); not connected on either
// node is ErrUserNotConnected, other relay errors pass through.
func (auth *AuthManager) Send(user account.AccountID, msg *msgjson.Message) error {
	client := auth.user(user)
	if client == nil {
		ctx, cancel := context.WithTimeout(context.Background(), DefaultRequestTimeout)
		defer cancel()
		err := auth.mesh.ProxyClientMessage(ctx, &mesh.ClientProxyMessage{
			User:            user,
			Msg:             cloneMsg(msg),
			DeliverToClient: true,
		})
		if errors.Is(err, mesh.ErrClientProxyUnavailable) || errors.Is(err, mesh.ErrClientNotConnected) {
			log.Debugf("Send requested for disconnected user %v", user)
			return dex.NewError(ErrUserNotConnected, user.String())
		}
		return err
	}

	return auth.send(client, msg)
}

// SendIfLocal sends the message only if the user is connected to this node. A
// missing local client is treated as a no-op so callers can safely use it for
// mesh-replicated notifications that should only reach locally connected users.
func (auth *AuthManager) SendIfLocal(user account.AccountID, msg *msgjson.Message) error {
	client := auth.user(user)
	if client == nil {
		return nil
	}
	return auth.send(client, msg)
}

// Notify delivers a notification to the user on this node or the mesh peer.
// See msgjson.NewNotification. ErrUserNotConnected if the user is on neither
// node.
func (auth *AuthManager) Notify(acctID account.AccountID, msg *msgjson.Message) error {
	return auth.Send(acctID, msg)
}

// Requests

// DefaultRequestTimeout is the default timeout for requests to wait for
// responses from connected users after the request is successfully sent.
const DefaultRequestTimeout = 30 * time.Second

func (auth *AuthManager) requestLocal(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message),
	expireTimeout time.Duration, expire func()) error {

	client := auth.user(user)
	if client == nil {
		log.Debugf("Send requested for disconnected user %v", user)
		return dex.NewError(ErrUserNotConnected, user.String())
	}
	// log.Tracef("Registering '%s' request ID %d for user %v (auth clientInfo)", msg.Route, msg.ID, user)
	client.logReq(msg.ID, f, expireTimeout, expire)
	// auth.handleResponse checks clientInfo map and the found client's request
	// handler map, where the expire function should be found for msg.ID.
	err := client.conn.Request(msg, auth.handleResponse, expireTimeout, func() {})
	if err != nil {
		log.Debugf("error sending request ID %d: %v", msg.ID, err)
		// Remove the responseHandler registered by logReq and stop the expire
		// timer so that it does not eventually fire and run the expire func.
		// The caller receives a non-nil error to deal with it.
		client.respHandler(msg.ID) // drop the removed handler
		// Remove client assuming connection is broken, requiring reconnect.
		auth.removeClient(client)
		// client.conn.Disconnect() // async removal
	}
	return err
}

func cloneMsg(msg *msgjson.Message) *msgjson.Message {
	if msg == nil {
		return nil
	}
	cloned := *msg
	if msg.Payload != nil {
		cloned.Payload = append(json.RawMessage(nil), msg.Payload...)
	}
	if msg.Sig != nil {
		cloned.Sig = append(dex.Bytes(nil), msg.Sig...)
	}
	return &cloned
}

func (auth *AuthManager) requestViaMesh(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message),
	expireTimeout time.Duration, expire func()) error {
	if expireTimeout <= 0 {
		expireTimeout = DefaultRequestTimeout
	}
	proxiedMsg := cloneMsg(msg)
	route := proxiedMsg.Route
	timeoutMS := uint64(expireTimeout / time.Millisecond)

	auth.registerProxyRespHandler(user, proxiedMsg.ID, f, expireTimeout, expire)
	meshSvc := auth.mesh
	go func() {
		err := meshSvc.ProxyClientMessage(context.Background(), &mesh.ClientProxyMessage{
			User:      user,
			Msg:       proxiedMsg,
			TimeoutMS: timeoutMS,
		})
		if err != nil {
			log.Debugf("proxied request %q for user %v failed: %v", route, user, err)
			if auth.proxyRespHandler(user, proxiedMsg.ID) != nil {
				expire()
			}
		}
	}()

	return nil
}

func (auth *AuthManager) request(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message),
	expireTimeout time.Duration, expire func()) error {
	if auth.user(user) != nil {
		return auth.requestLocal(user, msg, f, expireTimeout, expire)
	}
	return auth.requestViaMesh(user, msg, f, expireTimeout, expire)
}

// Request sends the Request-type msgjson.Message to the client identified by
// the specified account ID, proxying through mesh when the user is not
// connected locally. The user must respond within DefaultRequestTimeout of
// the request. Late responses are not handled.
func (auth *AuthManager) Request(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message)) error {
	return auth.request(user, msg, f, DefaultRequestTimeout, func() {})
}

// RequestIfLocal sends the Request-type msgjson.Message only if the user is
// connected to this node. A missing local client is a no-op so mesh-replicated
// event appliers can avoid proxying duplicate requests to the peer.
func (auth *AuthManager) RequestIfLocal(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message)) error {
	if auth.user(user) == nil {
		return nil
	}
	return auth.requestLocal(user, msg, f, DefaultRequestTimeout, func() {})
}

// RequestWithTimeout sends the Request-type msgjson.Message to the client
// identified by the specified account ID, proxying through mesh when the user
// is not connected locally. If the user responds within expireTime of the
// request, the response handler is called, otherwise the expire function is
// called. If the response handler is called, it is guaranteed that the
// request Message.ID is equal to the response Message.ID (see handleResponse).
func (auth *AuthManager) RequestWithTimeout(user account.AccountID, msg *msgjson.Message, f func(comms.Link, *msgjson.Message),
	expireTimeout time.Duration, expire func()) error {
	return auth.request(user, msg, f, expireTimeout, expire)
}

// HandleProxiedClientMessage delivers a client message that arrived via the
// other mesh server. Match request IDs to responses here (mesh does not).
// If the user is not connected to this server, return mesh.ErrClientNotConnected.
func (auth *AuthManager) HandleProxiedClientMessage(_ context.Context, req *mesh.ClientProxyMessage) error {
	if req == nil {
		return fmt.Errorf("nil proxied client message")
	}
	if req.Msg == nil {
		return fmt.Errorf("nil proxied client message payload")
	}

	switch req.Msg.Type {
	case msgjson.Request:
		return auth.proxyClientRequest(req)
	case msgjson.Response:
		if handler := auth.proxyRespHandler(req.User, req.Msg.ID); handler != nil {
			respMsg := cloneMsg(req.Msg)
			go handler.f(nil, respMsg)
			return nil
		}
		if !req.DeliverToClient {
			log.Debugf("Dropping late proxied client response %d for user %v", req.Msg.ID, req.User)
			return nil
		}
		return auth.sendProxiedClientMessage(req.User, req.Msg)
	case msgjson.Notification:
		return auth.sendProxiedClientMessage(req.User, req.Msg)
	default:
		return fmt.Errorf("unsupported proxied client message type %d", req.Msg.Type)
	}
}

func (auth *AuthManager) proxyClientRequest(req *mesh.ClientProxyMessage) error {
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	originalID := req.Msg.ID
	localMsg := cloneMsg(req.Msg)
	localMsg.ID = comms.NextID()

	err := auth.requestLocal(req.User, localMsg, func(_ comms.Link, resp *msgjson.Message) {
		respMsg := cloneMsg(resp)
		if respMsg != nil {
			respMsg.ID = originalID
		}
		if respMsg == nil {
			log.Debugf("proxied client response %d for user %v was nil", originalID, req.User)
			return
		}
		meshSvc := auth.mesh
		go func() {
			err := meshSvc.ProxyClientMessage(context.Background(), &mesh.ClientProxyMessage{
				User: req.User,
				Msg:  respMsg,
			})
			if err != nil {
				log.Debugf("proxied client response %d for user %v failed: %v", originalID, req.User, err)
			}
		}()
	}, timeout, func() {
		log.Debugf("proxied client request %q for user %v timed out locally", req.Msg.Route, req.User)
	})
	if errors.Is(err, ErrUserNotConnected) {
		return mesh.ErrClientNotConnected
	}
	return err
}

func (auth *AuthManager) sendProxiedClientMessage(user account.AccountID, msg *msgjson.Message) error {
	client := auth.user(user)
	if client == nil {
		log.Debugf("Proxied send requested for disconnected user %v", user)
		return mesh.ErrClientNotConnected
	}
	return auth.send(client, msg)
}

func (auth *AuthManager) integrateOutcomes(
	matchOutcomes *latestOutcomes[*db.MatchResult],
	preimgOutcomes *latestOutcomes[*db.PreimageOutcome],
	orderOutcomes *latestOutcomes[*db.OrderOutcome],
) (score, successCount, piMissCount int32) {

	if matchOutcomes != nil {
		matchCounts := matchOutcomes.binViolations()
		for v, count := range matchCounts {
			score += outcomeScores[v] * int32(count)
		}
		successCount = int32(matchCounts[db.OutcomeSwapSuccess])
	}
	if preimgOutcomes != nil {
		counts := preimgOutcomes.binViolations()
		piMissCount = int32(counts[db.OutcomePreimageMiss])
		score += outcomeScores[db.OutcomePreimageMiss] * piMissCount
	}
	if !auth.freeCancels {
		counts := orderOutcomes.binViolations()
		successes, cancels := int32(counts[db.OutcomeOrderComplete]), int32(counts[db.OutcomeOrderCanceled])
		totalOrds := int(successes + cancels)
		if totalOrds > auth.GraceLimit() {
			cancelRate := float64(cancels) / float64(totalOrds)
			if cancelRate > auth.cancelThresh {
				score += outcomeScores[db.OutcomeOrderCanceled]
			}
		}
	}
	return
}

// UserReputationAt calculates some quantities related to the user's
// reputation, with bond expiry evaluated at asOf instead of the wall clock.
// Appliers pass the event's server time. Satisfies market.AuthManager.
func (auth *AuthManager) UserReputationAt(user account.AccountID, asOf time.Time) (tier int64, score, maxScore int32, err error) {
	maxScore = ScoringMatchLimit
	data, err := auth.rep.get(auth.ctx, user, auth.loadUserRepData)
	if err != nil {
		return
	}
	if !data.exists {
		return 0, data.score, maxScore, nil
	}
	r := auth.userReputation(data.bondTier(asOf.Add(auth.bondExpiry).Unix()), data.score)
	return r.EffectiveTier(), r.Score, maxScore, nil
}

// userReputation computes the breakdown of a user's tier and score.
func (auth *AuthManager) userReputation(bondTier int64, score int32) *account.Reputation {
	var penalties int32
	if score < 0 {
		penalties = score / auth.penaltyThreshold
	}
	return &account.Reputation{
		BondedTier: bondTier,
		Penalties:  uint16(penalties),
		Score:      score,
	}
}

func (auth *AuthManager) reputationFromDB(ctx context.Context, user account.AccountID) (*account.Reputation, int32, error) {
	data, err := auth.rep.get(ctx, user, auth.loadUserRepData)
	if err != nil {
		return nil, 0, err
	}
	if !data.exists {
		return nil, data.score, nil
	}
	bondExpiryThreshold := time.Now().Add(auth.bondExpiry).Unix()
	rep := auth.userReputation(data.bondTier(bondExpiryThreshold), data.score)
	rep.BondExpiryThreshold = bondExpiryThreshold
	return rep, data.score, nil
}

// loadUserRepData is the reputation cache fetch: score and active bonds from DB.
func (auth *AuthManager) loadUserRepData(ctx context.Context, user account.AccountID) (*repData, error) {
	score, err := auth.loadUserScoreContext(ctx, user)
	if err != nil {
		return nil, err
	}

	// Load every bond, not only those active now. Appliers evaluate the tier
	// at an earlier as-of, so a now-expired bond may still count.
	// Propagate account errors; a nil account is cached as exists=false.
	acct, bonds, err := auth.storage.Account(user, time.Time{})
	if err != nil {
		return nil, err
	}
	data := &repData{
		exists: acct != nil,
		score:  score,
		bonds:  make([]cachedBond, len(bonds)),
	}
	for i, bond := range bonds {
		data.bonds[i] = cachedBond{
			strength: bond.Strength,
			lockTime: bond.LockTime,
		}
	}
	return data, nil
}

// ComputeUserReputation computes the user's reputation from their active bonds
// and conduct score. Returns nil for an unknown user, and also (with the
// error only logged) when the reputation load fails; use AcctRepStatus to
// distinguish the two.
func (auth *AuthManager) ComputeUserReputation(user account.AccountID) *account.Reputation {
	r, _, err := auth.reputationFromDB(auth.ctx, user)
	if err != nil {
		log.Errorf("failed to load user reputation: %v", err)
		return nil
	}
	return r
}

// AcctRepStatus reports local connectivity and reputation. Unlike AcctStatus,
// a reputation load failure is returned as an error, not as tier 0. For an
// unknown account, rep and err are both nil.
func (auth *AuthManager) AcctRepStatus(user account.AccountID) (connected bool, rep *account.Reputation, err error) {
	connected = auth.user(user) != nil
	rep, _, err = auth.reputationFromDB(auth.ctx, user)
	return
}

// AcctStatus indicates if the user is presently connected and their tier.
func (auth *AuthManager) AcctStatus(user account.AccountID) (connected bool, tier int64) {
	if auth.user(user) != nil {
		connected = true
	}
	rep := auth.ComputeUserReputation(user)
	if rep != nil {
		tier = rep.EffectiveTier()
	}
	return
}

// ForgiveMatchFail submits a mesh forgive command for one match failure.
// A slave forwards it while established_slave. The durable change is
// applied as an event on every node.
func (auth *AuthManager) ForgiveMatchFail(user account.AccountID, mid order.MatchID) (forgiven, unbanned bool, err error) {
	result, err := auth.executeReputationForgivenessCommand(context.Background(), &reputationForgivenessRequest{
		AccountID: user,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   mid,
	}, reputationForgivenessCommandTimeout)
	if err != nil {
		return false, false, err
	}
	return result.Forgiven, result.Unbanned, nil
}

// CreatePrepaidBonds submits a mesh command to issue prepaid bond tokens.
// A slave forwards it while established_slave. The tokens are stored when
// every node applies the event.
func (auth *AuthManager) CreatePrepaidBonds(n int, strength uint32, durSecs int64) ([][]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("pre-paid bond count cannot be negative")
	}
	if n == 0 {
		return [][]byte{}, nil
	}
	if auth.mesh == nil {
		return nil, fmt.Errorf("mesh service not configured")
	}

	reqMsg, err := msgjson.NewRequest(comms.NextID(), commandKindCreatePrepaidBonds, &createPrepaidBondsRequest{
		N:        n,
		Strength: strength,
		DurSecs:  durSecs,
	})
	if err != nil {
		return nil, err
	}

	respC := make(chan *msgjson.Message, 1)
	ctx, cancel := context.WithTimeout(context.Background(), txWaitExpiration)
	defer cancel()

	if rpcErr := auth.mesh.ExecuteCommand(ctx, mesh.CommandRequest{
		Kind: commandKindCreatePrepaidBonds,
		Msg:  reqMsg,
		Respond: func(resp *msgjson.Message) error {
			select {
			case respC <- resp:
			default:
			}
			return nil
		},
	}); rpcErr != nil {
		return nil, rpcErr
	}

	select {
	case resp := <-respC:
		payload, err := resp.Response()
		if err != nil {
			return nil, err
		}
		if payload.Error != nil {
			return nil, payload.Error
		}
		var result createPrepaidBondsResult
		if err := json.Unmarshal(payload.Result, &result); err != nil {
			return nil, err
		}
		return result.CoinIDs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type createPrepaidBondsRequest struct {
	N        int    `json:"n"`
	Strength uint32 `json:"strength"`
	DurSecs  int64  `json:"durSecs"`
}

type createPrepaidBondsResult struct {
	CoinIDs [][]byte `json:"coinIDs"`
}

func (auth *AuthManager) executeCreatePrepaidBonds(cmdCtx *mesh.CommandContext) *msgjson.Error {
	var req createPrepaidBondsRequest
	if err := cmdCtx.Request.Msg.Unmarshal(&req); err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing create prepaid bonds request: %v", err)
	}
	if req.N < 0 {
		return msgjson.NewError(msgjson.RPCArgumentsError, "pre-paid bond count cannot be negative")
	}
	if req.N == 0 {
		if err := cmdCtx.Completion.Complete(cmdCtx.Context, &createPrepaidBondsResult{CoinIDs: [][]byte{}}); err != nil {
			return msgjson.NewError(msgjson.RPCInternalError, "failed to complete pre-paid bond creation")
		}
		return nil
	}

	coinIDs := make([][]byte, req.N)
	const prepaidBondIDLength = 16
	bonds := make([]*meshevents.PrepaidBond, req.N)
	for i := 0; i < req.N; i++ {
		coinIDs[i] = encode.RandomBytes(prepaidBondIDLength)
		bonds[i] = &meshevents.PrepaidBond{
			CoinID:   coinIDs[i],
			Strength: req.Strength,
		}
	}
	lockTime := time.Now().Add(auth.bondExpiry).Add(time.Duration(req.DurSecs) * time.Second).Unix()
	for _, bond := range bonds {
		bond.LockTime = lockTime
	}
	event, err := mesh.NewEvent(&meshevents.PrepaidBondsCreatedEvent{Bonds: bonds})
	if err != nil {
		return msgjson.NewError(msgjson.RPCInternalError, "failed to encode pre-paid bond creation event")
	}

	if err = cmdCtx.Completion.Emit(cmdCtx.Context, event, func() any {
		return &createPrepaidBondsResult{CoinIDs: coinIDs}
	}); err != nil {
		mesh.LogApplyFailure(log, err, "Failed to store pre-paid bonds: %v", err)
		return mesh.ClientError(err, msgjson.RPCInternalError, "failed to store pre-paid bonds")
	}
	return nil
}

// TODO: a way to manipulate/forgive cancellation rate violation.

// user gets the clientInfo for the specified account ID.
func (auth *AuthManager) user(user account.AccountID) *clientInfo {
	auth.connMtx.RLock()
	defer auth.connMtx.RUnlock()
	return auth.users[user]
}

// conn gets the clientInfo for the specified connection ID.
func (auth *AuthManager) conn(conn comms.Link) *clientInfo {
	auth.connMtx.RLock()
	defer auth.connMtx.RUnlock()
	return auth.conns[conn.ID()]
}

// notifyReputationInputsChanged reloads each user's reputation and sends a
// scorechanged note on the local session, if any. The storage hook must not
// block, so this runs on a goroutine after invalidate.
//
// TODO(mesh): The hook lists every user whose reputation *inputs* may have
// changed, including 0-point outcomes (preimage success, order complete,
// free cancel) and no-op bond/forgive applies. That is more notes than
// users whose score or tier actually moved.
func (auth *AuthManager) notifyReputationInputsChanged(users []account.AccountID) {
	auth.repNotifyMtx.Lock()
	defer auth.repNotifyMtx.Unlock()
	for _, user := range users {
		if auth.user(user) == nil {
			continue
		}
		rep, _, err := auth.eventReputationFromDB(context.Background(), user)
		if err != nil {
			log.Errorf("failed to load reputation after inputs change for account %v: %v", user, err)
			continue
		}
		if rep == nil {
			continue
		}
		auth.sendScoreChanged(user, rep)
	}
}

// sendScoreChanged sends a scorechanged notification to an account.
func (auth *AuthManager) sendScoreChanged(acctID account.AccountID, rep *account.Reputation) {
	note := &msgjson.ScoreChangedNotification{
		Reputation: *rep,
	}
	auth.Sign(note)
	resp, err := msgjson.NewNotification(msgjson.ScoreChangeRoute, note)
	if err != nil {
		log.Error("ScoreChangeRoute encoding error: %v", err)
		return
	}
	if err = auth.SendIfLocal(acctID, resp); err != nil {
		log.Warnf("Error sending score changed notification to account %v: %v", acctID, err)
		// The user will need to 'connect' to see their current tier and bonds.
	}
}

// addClient adds the client to the users and conns maps.
func (auth *AuthManager) addClient(client *clientInfo) {
	auth.connMtx.Lock()
	defer auth.connMtx.Unlock()
	user := client.acct.ID

	oldClient := auth.users[user]
	auth.users[user] = client

	connID := client.conn.ID()
	auth.conns[connID] = client

	// Now that the new conn ID is registered, disconnect any existing old link
	// unless it is the same.
	if oldClient != nil {
		oldConnID := oldClient.conn.ID()
		if oldConnID == connID {
			return // reused conn, just update maps
		}
		log.Warnf("User %v reauthorized from %v (id %d) with an existing connection from %v (id %d). Disconnecting the old one.",
			user, client.conn.Addr(), connID, oldClient.conn.Addr(), oldConnID)
		// When replacing with a new conn, manually deregister the old conn so
		// that when it disconnects it does not remove the new clientInfo.
		delete(auth.conns, oldConnID)
		oldClient.conn.Disconnect()
	}

	// When the conn goes down, automatically unregister the client.
	go func() {
		<-client.conn.Done()
		log.Debugf("Link down: id=%d, ip=%s.", client.conn.ID(), client.conn.Addr())
		auth.removeClient(client) // must stop if connID already removed
	}()
}

// removeClient unregisters the client from the users and conns maps. It is
// idempotent for a given conn ID.
func (auth *AuthManager) removeClient(client *clientInfo) {
	auth.connMtx.Lock()
	connID := client.conn.ID()
	if _, connFound := auth.conns[connID]; !connFound {
		// conn already removed manually when this user made a new connection.
		// This user is still in the users map, so return.
		auth.connMtx.Unlock()
		return
	}
	delete(auth.users, client.acct.ID)
	delete(auth.conns, connID)
	auth.connMtx.Unlock()
	client.conn.Disconnect() // in case not triggered by disconnect
}

func matchStatusToOutcome(s order.MatchStatus) Outcome {
	switch s {
	case order.NewlyMatched:
		return db.OutcomeNoSwapAsMaker
	case order.MakerSwapCast:
		return db.OutcomeNoSwapAsTaker
	case order.TakerSwapCast:
		return db.OutcomeNoRedeemAsMaker
	case order.MakerRedeemed:
		return db.OutcomeNoRedeemAsTaker
	case order.MatchComplete:
		return db.OutcomeSwapSuccess // should be caught by Fail==false
	default:
		return db.OutcomeInvalid
	}
}

// loadUserOutcomes returns the user's latest reputation outcomes from the
// reputation points table.
func (auth *AuthManager) loadUserOutcomes(user account.AccountID) (pimgs *latestOutcomes[*db.PreimageOutcome], matches *latestOutcomes[*db.MatchResult], ords *latestOutcomes[*db.OrderOutcome], err error) {
	return auth.loadUserOutcomesContext(auth.ctx, user)
}

func (auth *AuthManager) loadUserOutcomesContext(ctx context.Context, user account.AccountID) (pimgs *latestOutcomes[*db.PreimageOutcome], matches *latestOutcomes[*db.MatchResult], ords *latestOutcomes[*db.OrderOutcome], err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dbPimgs, dbMatches, dbOrds, err := auth.storage.GetUserReputationData(ctx, user, scoringOrderLimit, ScoringMatchLimit, cancelThreshWindow)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error loading reputation data for user %s: %w", user, err)
	}
	return newLatestOutcomes(dbPimgs, scoringOrderLimit),
		newLatestOutcomes(dbMatches, ScoringMatchLimit),
		newLatestOutcomes(dbOrds, cancelThreshWindow), nil
}

// MatchOutcome is a JSON-friendly version of db.MatchOutcome.
type MatchOutcome struct {
	ID     dex.Bytes `json:"matchID"`
	Status string    `json:"status"`
	Fail   bool      `json:"failed"`
	Stamp  int64     `json:"stamp"`
	Value  uint64    `json:"value"`
	BaseID uint32    `json:"baseID"`
	Quote  uint32    `json:"quoteID"`
}

// MatchFail is a failed match and the effect on the user's score.
type MatchFail struct {
	ID      dex.Bytes `json:"matchID"`
	Penalty uint32    `json:"penalty"`
}

// AccountMatchOutcomesN generates a list of recent match outcomes for a user.
func (auth *AuthManager) AccountMatchOutcomesN(user account.AccountID, n int) ([]*MatchOutcome, error) {
	dbOutcomes, err := auth.storage.CompletedAndAtFaultMatchStats(user, n)
	if err != nil {
		return nil, err
	}
	outcomes := make([]*MatchOutcome, len(dbOutcomes))
	for i, o := range dbOutcomes {
		outcomes[i] = &MatchOutcome{
			ID:     o.ID[:],
			Status: o.Status.String(),
			Fail:   o.Fail,
			Stamp:  o.Time,
			Value:  o.Value,
			BaseID: o.Base,
			Quote:  o.Quote,
		}
	}
	return outcomes, nil
}

func (auth *AuthManager) UserMatchFails(user account.AccountID, n int) ([]*MatchFail, error) {
	matchFails, err := auth.storage.UserMatchFails(user, n)
	if err != nil {
		return nil, err
	}
	fails := make([]*MatchFail, len(matchFails))
	for i, fail := range matchFails {
		matchStatus := matchStatusToOutcome(fail.Status)
		fails[i] = &MatchFail{
			ID:      fail.ID[:],
			Penalty: uint32(-1 * outcomeScores[matchStatus]),
		}
	}
	return fails, nil
}

func (auth *AuthManager) loadUserScoreContext(ctx context.Context, user account.AccountID) (int32, error) {
	latestPreimageResults, latestMatches, latestFinished, err := auth.loadUserOutcomesContext(ctx, user)
	if err != nil {
		return 0, err
	}

	score, _, _ := auth.integrateOutcomes(latestMatches, latestPreimageResults, latestFinished)
	return score, nil
}

// handleConnect is the handler for the 'connect' route. The user is authorized,
// a response is issued, and a clientInfo is created or updated.
func (auth *AuthManager) handleConnect(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
	connect := new(msgjson.Connect)
	err := msg.Unmarshal(&connect)
	if err != nil || connect == nil {
		return &msgjson.Error{
			Code:    msgjson.RPCParseError,
			Message: "error parsing connect request",
		}
	}
	if len(connect.AccountID) != account.HashSize {
		return &msgjson.Error{
			Code:    msgjson.AuthenticationError,
			Message: "authentication error. invalid account ID",
		}
	}
	var user account.AccountID
	copy(user[:], connect.AccountID[:])
	lockTimeThresh := time.Now().Add(auth.bondExpiry).Truncate(time.Second)
	acctInfo, bonds, err := auth.storage.Account(user, lockTimeThresh)
	if err != nil {
		log.Errorf("Account read failed for user %v on connect: %v", user, err)
		return &msgjson.Error{
			Code:    msgjson.RPCInternalError,
			Message: "failed to retrieve account",
		}
	}
	if acctInfo == nil {
		return &msgjson.Error{
			Code:    msgjson.AccountNotFoundError,
			Message: "no account found for account ID: " + connect.AccountID.String(),
		}
	}

	// Tier 0 accounts may connect to complete swaps, etc. but not place new
	// orders.

	// Authorize the account.
	sigMsg := connect.Serialize()
	err = checkSigS256(sigMsg, connect.SigBytes(), acctInfo.PubKey)
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.SignatureError,
			Message: "signature error: " + err.Error(),
		}
	}

	// Check to see if there is already an existing client for this account.
	respHandlers := make(map[uint64]*respHandler)
	oldClient := auth.user(acctInfo.ID)
	if oldClient != nil {
		oldClient.mtx.Lock()
		respHandlers = oldClient.respHandlers
		oldClient.mtx.Unlock()
	}

	latestPreimageResults, latestMatches, latestFinished, err := auth.loadUserOutcomes(user)
	if err != nil {
		log.Errorf("Failed to compute user %v score: %v", user, err)
		return &msgjson.Error{
			Code:    msgjson.RPCInternalError,
			Message: "DB error",
		}
	}
	score, successCount, piMissCount := auth.integrateOutcomes(latestMatches, latestPreimageResults, latestFinished)

	successScore := successCount * matchCompletedScore
	piMissScore := piMissCount * preimageMissScore
	// score = violationScore + piMissScore + successScore
	violationScore := score - piMissScore - successScore // work backwards as per above comment
	log.Debugf("User %v score = %d:%d (%d successes) - %d (violations) - %d (%d preimage misses) ",
		user, score, successScore, successCount, -violationScore, -piMissScore, piMissCount)

	client := &clientInfo{
		acct:         acctInfo,
		conn:         conn,
		respHandlers: respHandlers,
	}

	// Get the list of active orders for this user.
	activeOrderStatuses, err := auth.storage.ActiveUserOrderStatuses(user)
	if err != nil {
		log.Errorf("ActiveUserOrderStatuses(%v): %v", user, err)
		return &msgjson.Error{
			Code:    msgjson.RPCInternalError,
			Message: "DB error",
		}
	}

	msgOrderStatuses := make([]*msgjson.OrderStatus, 0, len(activeOrderStatuses))
	for _, orderStatus := range activeOrderStatuses {
		msgOrderStatuses = append(msgOrderStatuses, &msgjson.OrderStatus{
			ID:     orderStatus.ID.Bytes(),
			Status: uint16(orderStatus.Status),
		})
	}

	// Get the list of active matches for this user.
	matches, err := auth.storage.AllActiveUserMatches(user)
	if err != nil {
		log.Errorf("AllActiveUserMatches(%v): %v", user, err)
		return &msgjson.Error{
			Code:    msgjson.RPCInternalError,
			Message: "DB error",
		}
	}

	// There may be as many as 2*len(matches) match messages if the user matched
	// with themself, but this is likely to be very rare outside of tests.
	msgMatches := make([]*msgjson.Match, 0, len(matches))

	// msgMatchForSide checks if the user is on the given side of the match,
	// appending the match to the slice if so. The Address and Side fields of
	// msgjson.Match will differ depending on the side.
	msgMatchForSide := func(match *db.MatchData, side order.MatchSide) {
		var addr string
		var oid []byte
		switch {
		case side == order.Maker && user == match.MakerAcct:
			addr = match.TakerSwapAddr // counterparty's per-match address
			oid = match.Maker[:]
			// sell = !match.TakerSell
		case side == order.Taker && user == match.TakerAcct:
			addr = match.MakerSwapAddr // counterparty's per-match address
			oid = match.Taker[:]
			// sell = match.TakerSell
		default:
			return
		}

		msgMatches = append(msgMatches, &msgjson.Match{
			OrderID:      oid,
			MatchID:      match.ID[:],
			Quantity:     match.Quantity,
			Rate:         match.Rate,
			ServerTime:   uint64(match.Epoch.End().UnixMilli()),
			Address:      addr,
			FeeRateBase:  match.BaseRate,  // contract txn fee rate if user is selling
			FeeRateQuote: match.QuoteRate, // contract txn fee rate if user is buying
			Status:       uint8(match.Status),
			Side:         uint8(side),
		})
	}

	// For each db match entry, create at least one msgjson.Match.
	activeMatchIDs := make(map[order.MatchID]bool, len(matches))
	for _, match := range matches {
		activeMatchIDs[match.ID] = true
		msgMatchForSide(match, order.Maker)
		msgMatchForSide(match, order.Taker)
	}

	conn.Authorized()

	// Prepare bond info for response.
	var bondTier int64
	msgBonds := make([]*msgjson.Bond, 0, len(bonds))
	for _, bond := range bonds {
		// Double check the DB backend's thresholding.
		lockTime := time.Unix(bond.LockTime, 0)
		if lockTime.Before(lockTimeThresh) {
			log.Warnf("Loaded expired bond from DB (%v), lockTime %v is before %v",
				coinIDString(bond.AssetID, bond.CoinID), lockTime, lockTimeThresh)
			continue // will be expired on next prune
		}
		bondTier += int64(bond.Strength)
		expireTime := lockTime.Add(-auth.bondExpiry)
		msgBonds = append(msgBonds, &msgjson.Bond{
			Version:  bond.Version,
			Amount:   uint64(bond.Amount),
			Expiry:   uint64(expireTime.Unix()),
			CoinID:   bond.CoinID,
			AssetID:  bond.AssetID,
			Strength: bond.Strength, // Added with v2 reputation
		})
	}

	rep := auth.userReputation(bondTier, score)
	rep.BondExpiryThreshold = lockTimeThresh.Unix()

	// Sign and send the connect response.
	sig := auth.SignMsg(sigMsg)
	resp := &msgjson.ConnectResult{
		Sig:                 sig,
		ActiveOrderStatuses: msgOrderStatuses,
		ActiveMatches:       msgMatches,
		Score:               score,
		ActiveBonds:         msgBonds,
		Reputation:          rep,
	}
	respMsg, err := msgjson.NewResponse(msg.ID, resp, nil)
	if err != nil {
		log.Errorf("handleConnect prepare response error: %v", err)
		return &msgjson.Error{
			Code:    msgjson.RPCInternalError,
			Message: "internal error",
		}
	}

	err = conn.Send(respMsg)
	if err != nil {
		log.Error("Failed to send connect response: " + err.Error())
		return nil
	}

	log.Infof("Authenticated account %v from %v with %d active orders, %d active matches, tier = %v, "+
		"bond tier = %v, score = %v",
		user, conn.Addr(), len(msgOrderStatuses), len(msgMatches), rep.EffectiveTier(), bondTier, score)
	auth.addClient(client)

	auth.connectCallbackMtx.RLock()
	callbacks := auth.connectCallbacks
	auth.connectCallbackMtx.RUnlock()
	for _, cb := range callbacks {
		cb(user)
	}

	return nil
}

// handleResponse handles all responses for AuthManager registered routes,
// essentially wrapping response handlers and translating connection ID to
// account ID.
func (auth *AuthManager) handleResponse(conn comms.Link, msg *msgjson.Message) {
	client := auth.conn(conn)
	if client == nil {
		log.Errorf("response from unknown connection")
		return
	}
	handler := client.respHandler(msg.ID)
	if handler == nil {
		log.Debugf("(*AuthManager).handleResponse: unknown msg ID %d", msg.ID)
		errMsg, err := msgjson.NewResponse(msg.ID, nil,
			msgjson.NewError(msgjson.UnknownResponseID, "unknown response ID"))
		if err != nil {
			log.Errorf("failure creating unknown ID response error message: %v", err)
		} else {
			err := conn.Send(errMsg)
			if err != nil {
				log.Tracef("error sending response failure message: %v", err)
				auth.removeClient(client)
				// client.conn.Disconnect() // async removal
			}
		}
		return
	}
	handler.f(conn, msg)
}

// marketMatches is an index of match IDs associated with a particular market.
type marketMatches struct {
	base     uint32
	quote    uint32
	matchIDs map[order.MatchID]bool
}

// add adds a match ID to the marketMatches.
func (mm *marketMatches) add(matchID order.MatchID) bool {
	_, found := mm.matchIDs[matchID]
	mm.matchIDs[matchID] = true
	return !found
}

// idList generates a []order.MatchID from the currently indexed match IDs.
func (mm *marketMatches) idList() []order.MatchID {
	ids := make([]order.MatchID, 0, len(mm.matchIDs))
	for matchID := range mm.matchIDs {
		ids = append(ids, matchID)
	}
	return ids
}

// getTxData gets the tx data for the coin ID.
func (auth *AuthManager) getTxData(assetID uint32, coinID []byte) ([]byte, error) {
	txDataSrc, found := auth.txDataSources[assetID]
	if !found {
		return nil, fmt.Errorf("no tx data source for asset ID %d", assetID)
	}
	return txDataSrc(coinID)
}

// handleMatchStatus handles requests to the 'match_status' route.
func (auth *AuthManager) handleMatchStatus(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
	client := auth.conn(conn)
	if client == nil {
		return msgjson.NewError(msgjson.UnauthorizedConnection,
			"cannot use route 'match_status' on an unauthorized connection")
	}
	var matchReqs []msgjson.MatchRequest
	err := msg.Unmarshal(&matchReqs)
	if err != nil || matchReqs == nil /* null Payload */ {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing match_status request")
	}
	// NOTE: If len(matchReqs)==0 but not nil, Payload was `[]`, demanding a
	// positive response with `[]` in ResponsePayload.Result.

	mkts := make(map[string]*marketMatches)
	var count int
	for _, req := range matchReqs {
		mkt, err := dex.MarketName(req.Base, req.Quote)
		if err != nil {
			return msgjson.NewError(msgjson.InvalidRequestError, "market with base=%d, quote=%d is not known", req.Base, req.Quote)
		}
		if len(req.MatchID) != order.MatchIDSize {
			return msgjson.NewError(msgjson.InvalidRequestError, "match ID is wrong length: %s", req.MatchID)
		}
		mktMatches, found := mkts[mkt]
		if !found {
			mktMatches = &marketMatches{
				base:     req.Base,
				quote:    req.Quote,
				matchIDs: make(map[order.MatchID]bool),
			}
			mkts[mkt] = mktMatches
		}
		var matchID order.MatchID
		copy(matchID[:], req.MatchID)
		if mktMatches.add(matchID) {
			count++
		}
	}

	results := make([]*msgjson.MatchStatusResult, 0, count) // should be non-nil even for count==0
	for _, mm := range mkts {
		statuses, err := auth.storage.MatchStatuses(client.acct.ID, mm.base, mm.quote, mm.idList())
		// no results is not an error
		if err != nil {
			log.Errorf("MatchStatuses error: acct = %s, base = %d, quote = %d, matchIDs = %v: %v",
				client.acct.ID, mm.base, mm.quote, mm.matchIDs, err)
			return msgjson.NewError(msgjson.RPCInternalError, "DB error")
		}
		for _, status := range statuses {
			var makerTxData, takerTxData []byte
			var assetID uint32
			switch {
			case status.IsTaker && status.Status == order.MakerSwapCast:
				assetID = mm.base
				if status.TakerSell {
					assetID = mm.quote
				}
				makerTxData, err = auth.getTxData(assetID, status.MakerSwap)
				if err != nil {
					log.Errorf("failed to get maker tx data for %s %s: %v", dex.BipIDSymbol(assetID),
						coinIDString(assetID, status.MakerSwap), err)
					return msgjson.NewError(msgjson.RPCInternalError, "blockchain retrieval error")
				}
			case status.IsMaker && status.Status == order.TakerSwapCast:
				assetID = mm.quote
				if status.TakerSell {
					assetID = mm.base
				}
				takerTxData, err = auth.getTxData(assetID, status.TakerSwap)
				if err != nil {
					log.Errorf("failed to get taker tx data for %s %s: %v", dex.BipIDSymbol(assetID),
						coinIDString(assetID, status.TakerSwap), err)
					return msgjson.NewError(msgjson.RPCInternalError, "blockchain retrieval error")
				}
			}

			results = append(results, &msgjson.MatchStatusResult{
				MatchID:       status.ID.Bytes(),
				Status:        uint8(status.Status),
				MakerContract: status.MakerContract,
				TakerContract: status.TakerContract,
				MakerSwap:     status.MakerSwap,
				TakerSwap:     status.TakerSwap,
				MakerRedeem:   status.MakerRedeem,
				TakerRedeem:   status.TakerRedeem,
				Secret:        status.Secret,
				Active:        status.Active,
				MakerTxData:   makerTxData,
				TakerTxData:   takerTxData,
			})
		}
	}

	log.Tracef("%d results for %d requested match statuses, acct = %s",
		len(results), len(matchReqs), client.acct.ID)

	resp, err := msgjson.NewResponse(msg.ID, results, nil)
	if err != nil {
		log.Errorf("NewResponse error: %v", err)
		return msgjson.NewError(msgjson.RPCInternalError, "Internal error")
	}

	err = conn.Send(resp)
	if err != nil {
		log.Error("error sending match_status response: " + err.Error())
	}
	return nil
}

func (auth *AuthManager) ForgiveUser(user account.AccountID) error {
	_, err := auth.executeReputationForgivenessCommand(context.Background(), &reputationForgivenessRequest{
		AccountID: user,
		Scope:     meshevents.ReputationForgivenessScopeUser,
	}, reputationForgivenessCommandTimeout)
	return err
}

func (auth *AuthManager) executeReputationForgivenessCommand(ctx context.Context, req *reputationForgivenessRequest, timeout time.Duration) (*reputationForgivenessResult, error) {
	if auth.mesh == nil {
		return nil, fmt.Errorf("mesh service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = reputationForgivenessCommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msg, err := msgjson.NewRequest(comms.NextID(), commandKindForgiveReputation, req)
	if err != nil {
		return nil, err
	}

	responses := make(chan *msgjson.Message, 1)
	execErrs := make(chan *msgjson.Error, 1)
	go func() {
		execErrs <- auth.mesh.ExecuteCommand(cmdCtx, mesh.CommandRequest{
			Kind: commandKindForgiveReputation,
			User: req.AccountID,
			Msg:  msg,
			Respond: func(resp *msgjson.Message) error {
				select {
				case responses <- resp:
				default:
				}
				return nil
			},
		})
	}()

	resultFromResponse := func(resp *msgjson.Message) (*reputationForgivenessResult, error) {
		if resp == nil {
			return nil, fmt.Errorf("nil reputation forgiveness response")
		}
		var result reputationForgivenessResult
		if err := resp.UnmarshalResult(&result); err != nil {
			return nil, err
		}
		return &result, nil
	}

	var authDone <-chan struct{}
	if auth.ctx != nil {
		authDone = auth.ctx.Done()
	}

	for {
		select {
		case rpcErr := <-execErrs:
			if rpcErr != nil {
				return nil, rpcErr
			}
			if err := cmdCtx.Err(); err != nil {
				select {
				case resp := <-responses:
					return resultFromResponse(resp)
				default:
					return nil, err
				}
			}
			execErrs = nil
		case resp := <-responses:
			return resultFromResponse(resp)
		case <-authDone:
			return nil, auth.ctx.Err()
		case <-cmdCtx.Done():
			return nil, cmdCtx.Err()
		}
	}
}

// ReputationOutcomePolicy returns the reputation policy used when storage
// derives outcome updates from event facts.
func (auth *AuthManager) ReputationOutcomePolicy() *db.ReputationOutcomePolicy {
	return &db.ReputationOutcomePolicy{
		PreimageLimit:       scoringOrderLimit,
		MatchLimit:          ScoringMatchLimit,
		OrderLimit:          cancelThreshWindow,
		FreeCancelThreshold: freeCancelThreshold,
	}
}

// marketOrders is an index of order IDs associated with a particular market.
type marketOrders struct {
	base     uint32
	quote    uint32
	orderIDs map[order.OrderID]bool
}

// add adds a match ID to the marketOrders.
func (mo *marketOrders) add(oid order.OrderID) bool {
	_, found := mo.orderIDs[oid]
	mo.orderIDs[oid] = true
	return !found
}

// idList generates a []order.OrderID from the currently indexed order IDs.
func (mo *marketOrders) idList() []order.OrderID {
	ids := make([]order.OrderID, 0, len(mo.orderIDs))
	for oid := range mo.orderIDs {
		ids = append(ids, oid)
	}
	return ids
}

// handleOrderStatus handles requests to the 'order_status' route.
func (auth *AuthManager) handleOrderStatus(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
	client := auth.conn(conn)
	if client == nil {
		return msgjson.NewError(msgjson.UnauthorizedConnection,
			"cannot use route 'order_status' on an unauthorized connection")
	}

	var orderReqs []*msgjson.OrderStatusRequest
	err := msg.Unmarshal(&orderReqs)
	if err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing order_status request")
	}
	if len(orderReqs) == 0 { // includes null and [] Payload
		return msgjson.NewError(msgjson.InvalidRequestError, "no order id provided")
	}
	if len(orderReqs) > maxIDsPerOrderStatusRequest {
		return msgjson.NewError(msgjson.InvalidRequestError, "cannot request statuses for more than %v orders",
			maxIDsPerOrderStatusRequest)
	}

	mkts := make(map[string]*marketOrders)
	var uniqueReqsCount int
	for _, req := range orderReqs {
		mkt, err := dex.MarketName(req.Base, req.Quote)
		if err != nil {
			return msgjson.NewError(msgjson.InvalidRequestError, "market with base=%d, quote=%d is not known", req.Base, req.Quote)
		}
		if len(req.OrderID) != order.OrderIDSize {
			return msgjson.NewError(msgjson.InvalidRequestError, "order ID is wrong length: %s", req.OrderID)
		}
		mktOrders, found := mkts[mkt]
		if !found {
			mktOrders = &marketOrders{
				base:     req.Base,
				quote:    req.Quote,
				orderIDs: make(map[order.OrderID]bool),
			}
			mkts[mkt] = mktOrders
		}
		var oid order.OrderID
		copy(oid[:], req.OrderID)
		if mktOrders.add(oid) {
			uniqueReqsCount++
		}
	}

	results := make([]*msgjson.OrderStatus, 0, uniqueReqsCount)
	for _, mm := range mkts {
		orderStatuses, err := auth.storage.UserOrderStatuses(client.acct.ID, mm.base, mm.quote, mm.idList())
		// no results is not an error
		if err != nil {
			log.Errorf("OrderStatuses error: acct = %s, base = %d, quote = %d, orderIDs = %v: %v",
				client.acct.ID, mm.base, mm.quote, mm.orderIDs, err)
			return msgjson.NewError(msgjson.RPCInternalError, "DB error")
		}
		for _, orderStatus := range orderStatuses {
			results = append(results, &msgjson.OrderStatus{
				ID:     orderStatus.ID.Bytes(),
				Status: uint16(orderStatus.Status),
			})
		}
	}

	log.Tracef("%d results for %d requested order statuses, acct = %s",
		len(results), uniqueReqsCount, client.acct.ID)

	resp, err := msgjson.NewResponse(msg.ID, results, nil)
	if err != nil {
		log.Errorf("NewResponse error: %v", err)
		return msgjson.NewError(msgjson.RPCInternalError, "Internal error")
	}

	err = conn.Send(resp)
	if err != nil {
		log.Error("error sending order_status response: " + err.Error())
	}
	return nil
}

func coinIDString(assetID uint32, coinID []byte) string {
	s, err := asset.DecodeCoinID(assetID, coinID)
	if err != nil {
		return "unparsed:" + hex.EncodeToString(coinID)
	}
	return s
}
