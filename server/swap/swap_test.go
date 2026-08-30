// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/matcher"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const (
	ABCID  = 123
	XYZID  = 789
	ACCTID = 456
)

var (
	testCtx      context.Context
	acctTemplate = account.AccountID{
		0x22, 0x4c, 0xba, 0xaa, 0xfa, 0x80, 0xbf, 0x3b, 0xd1, 0xff, 0x73, 0x15,
		0x90, 0xbc, 0xbd, 0xda, 0x5a, 0x76, 0xf9, 0x1e, 0x60, 0xa1, 0x56, 0x99,
		0x46, 0x34, 0xe9, 0x1c, 0xaa, 0xaa, 0xaa, 0xaa,
	}
	acctCounter      uint32
	dexPrivKey       *secp256k1.PrivateKey
	tBcastTimeout    time.Duration
	txWaitExpiration time.Duration
)

type tUser struct {
	sig      []byte
	sigHex   string
	acct     account.AccountID
	addr     string
	lbl      string
	matchIDs []order.MatchID
}

func tickMempool() {
	time.Sleep(fastRecheckInterval * 3 / 2)
}

func timeOutMempool() {
	time.Sleep(txWaitExpiration * 3 / 2)
}

func dirtyEncode(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		fmt.Printf("dirtyEncode error for input '%s': %v", s, err)
	}
	return b
}

// A new tUser with a unique account ID, signature, and address.
func tNewUser(lbl string) *tUser {
	intBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(intBytes, acctCounter)
	acctID := account.AccountID{}
	copy(acctID[:], acctTemplate[:])
	copy(acctID[account.HashSize-4:], intBytes)
	addr := strconv.Itoa(int(acctCounter))
	sig := []byte{0xab} // Just to differentiate from the addr.
	sig = append(sig, intBytes...)
	sigHex := hex.EncodeToString(sig)
	acctCounter++
	return &tUser{
		sig:    sig,
		sigHex: sigHex,
		acct:   acctID,
		addr:   addr,
		lbl:    lbl,
	}
}

type TRequest struct {
	req      *msgjson.Message
	respFunc func(comms.Link, *msgjson.Message)
}

// This stub satisfies AuthManager.
type TAuthManager struct {
	mtx          sync.Mutex
	authErr      error
	verifyErr    error
	verifyErrSet bool
	privkey      *secp256k1.PrivateKey
	reqs         map[account.AccountID][]*TRequest
	resps        map[account.AccountID][]*msgjson.Message
	ntfns        map[account.AccountID][]*msgjson.Message
	newNtfn      chan struct{}
	suspensions  map[account.AccountID]account.Rule
	newSuspend   chan struct{}
	swapID       uint64
	// Use swapReceived if you need to synchronize error responses to init
	// requests.
	swapReceived chan struct{}
	auditReq     chan struct{}
	redeemID     uint64
	// Use redeemReceived if you need to synchronize error responses to redeem
	// requests.
	redeemReceived chan struct{}
	redemptionReq  chan struct{}
	// ntfnLocal is the SendIfLocal-vs-Send flag of the last notification per user+route.
	ntfnLocal map[account.AccountID]map[string]bool
}

func newTAuthManager() *TAuthManager {
	// Reuse any previously generated dex server private key.
	if dexPrivKey == nil {
		dexPrivKey, _ = secp256k1.GeneratePrivateKey()
	}
	return &TAuthManager{
		privkey:     dexPrivKey,
		reqs:        make(map[account.AccountID][]*TRequest),
		ntfns:       make(map[account.AccountID][]*msgjson.Message),
		resps:       make(map[account.AccountID][]*msgjson.Message),
		suspensions: make(map[account.AccountID]account.Rule),
		ntfnLocal:   make(map[account.AccountID]map[string]bool),
	}
}

func (m *TAuthManager) Send(user account.AccountID, msg *msgjson.Message) error {
	return m.recordSend(user, msg, false)
}

func (m *TAuthManager) SendIfLocal(user account.AccountID, msg *msgjson.Message) error {
	return m.recordSend(user, msg, true)
}

func (m *TAuthManager) recordSend(user account.AccountID, msg *msgjson.Message, local bool) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if msg.Route == "" {
		m.resps[user] = append(m.resps[user], msg)
		if m.redeemReceived != nil && msg.ID == m.redeemID {
			m.redeemReceived <- struct{}{}
		}
		if m.swapReceived != nil && msg.ID == m.swapID {
			m.swapReceived <- struct{}{}
		}
		return nil
	}
	if m.ntfnLocal[user] == nil {
		m.ntfnLocal[user] = make(map[string]bool)
	}
	m.ntfnLocal[user][msg.Route] = local
	m.ntfns[user] = append(m.ntfns[user], msg)
	if m.newNtfn != nil {
		select {
		case m.newNtfn <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *TAuthManager) ntfnWasLocal(user account.AccountID, route string) (local, ok bool) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	local, ok = m.ntfnLocal[user][route]
	return
}

func (m *TAuthManager) Request(user account.AccountID, msg *msgjson.Message,
	f func(comms.Link, *msgjson.Message)) error {
	return m.RequestWithTimeout(user, msg, f, time.Hour, func() {})
}

func (m *TAuthManager) RequestWithTimeout(user account.AccountID, msg *msgjson.Message,
	f func(comms.Link, *msgjson.Message), _ time.Duration, _ func()) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	tReq := &TRequest{
		req:      msg,
		respFunc: f,
	}
	l := m.reqs[user]
	if l == nil {
		l = make([]*TRequest, 0, 1)
	}
	m.reqs[user] = append(l, tReq)
	switch {
	case m.auditReq != nil && msg.Route == msgjson.AuditRoute:
		m.auditReq <- struct{}{}
	case m.redemptionReq != nil && msg.Route == msgjson.RedemptionRoute:
		m.redemptionReq <- struct{}{}
	}
	return nil
}
func (m *TAuthManager) Sign(signables ...msgjson.Signable) {
	for _, signable := range signables {
		hash := sha256.Sum256(signable.Serialize())
		sig := ecdsa.Sign(m.privkey, hash[:])
		signable.SetSig(sig.Serialize())
	}
}
func (m *TAuthManager) Suspended(user account.AccountID) (found, suspended bool) {
	var rule account.Rule
	rule, found = m.suspensions[user]
	suspended = rule != account.NoRule
	return // TODO: test suspended account handling (no trades, just cancels)
}
func (m *TAuthManager) VerifyUserSig(user account.AccountID, msg, sig []byte) error {
	if m.verifyErrSet {
		return m.verifyErr
	}
	return m.authErr
}
func (m *TAuthManager) Route(string,
	func(account.AccountID, *msgjson.Message) *msgjson.Error) {
}

func (m *TAuthManager) ReputationOutcomePolicy() *db.ReputationOutcomePolicy {
	return &db.ReputationOutcomePolicy{PreimageLimit: 40, MatchLimit: 60, OrderLimit: 100, FreeCancelThreshold: 2}
}

func (m *TAuthManager) penalize(id account.AccountID, rule account.Rule) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.suspensions[id] = rule
	if m.newSuspend != nil {
		m.newSuspend <- struct{}{}
	}
}

func (m *TAuthManager) flushPenalty(user account.AccountID) (found bool, rule account.Rule) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	rule, found = m.suspensions[user]
	if found {
		delete(m.suspensions, user)
	}
	return
}

// pop front
func (m *TAuthManager) popReq(id account.AccountID) *TRequest {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	reqs := m.reqs[id]
	if len(reqs) == 0 {
		return nil
	}
	req := reqs[0]
	m.reqs[id] = reqs[1:]
	return req
}

// push front
func (m *TAuthManager) pushReq(id account.AccountID, req *TRequest) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.reqs[id] = append([]*TRequest{req}, m.reqs[id]...)
}

func (m *TAuthManager) getNtfn(id account.AccountID, route string, payload any) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	msgs := m.ntfns[id]
	for i, msg := range msgs {
		if msg.Route == route {
			m.ntfns[id] = append(msgs[:i], msgs[i+1:]...)
			return msg.Unmarshal(payload)
		}
	}
	return fmt.Errorf("no %s notification", route)
}

func (m *TAuthManager) hasNtfn(id account.AccountID, route string) bool {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	for _, msg := range m.ntfns[id] {
		if msg.Route == route {
			return true
		}
	}
	return false
}

// push front
func (m *TAuthManager) pushResp(id account.AccountID, msg *msgjson.Message) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.resps[id] = append([]*msgjson.Message{msg}, m.resps[id]...)
}

// pop front
func (m *TAuthManager) popResp(id account.AccountID) (msg *msgjson.Message, resp *msgjson.ResponsePayload) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	msgs := m.resps[id]
	if len(msgs) == 0 {
		return
	}
	msg = msgs[0]
	m.resps[id] = msgs[1:]
	resp, _ = msg.Response()
	return
}

type TStorage struct {
	mtx sync.Mutex

	// activeSwaps and orders shape the DB state RestoreActiveSwaps restores from,
	// mimicking a node started on a snapshot-seeded (or crash-recovered)
	// database with in-flight matches.
	activeSwaps []*db.SwapDataFull
	orders      map[order.OrderID]order.Order

	// swapDataByID backs SwapDataFullByID for resend re-ack tests.
	swapDataByID    map[order.MatchID]*db.SwapDataFull
	swapDataByIDErr error

	matchAcksRecordedUpdates  []*db.MatchAcksRecordedUpdate
	applyMatchAcksRecordedErr error
	swapContracts             []*db.SwapContract
	auditAcks                 []*db.AuditAck
	redemptionAcks            []*db.RedemptionAck
	saveContractErr           error
	saveAuditAckSigErr        error
	redemptions               []*db.SwapRedemption
	applyRedemptionAckErr     error
	applyRedemptionErr        error
	matchFailedEvents         int
	matchFailedUpdates        []*db.MatchFailedUpdate
	inactiveMatches           []inactiveMatchWrite
	setMatchInactiveErr       error

	fatalMtx sync.RWMutex
	fatal    chan struct{}
	fatalErr error
}

type inactiveMatchWrite struct {
	mid     db.MarketMatchID
	forgive bool
}

func (ts *TStorage) LastErr() error {
	ts.fatalMtx.RLock()
	defer ts.fatalMtx.RUnlock()
	return ts.fatalErr
}

func (ts *TStorage) Fatal() <-chan struct{} {
	ts.fatalMtx.RLock()
	defer ts.fatalMtx.RUnlock()
	return ts.fatal
}

func (ts *TStorage) fatalBackendErr(err error) {
	ts.fatalMtx.Lock()
	if ts.fatal == nil {
		ts.fatal = make(chan struct{})
		close(ts.fatal)
	}
	ts.fatalErr = err // consider slice and append
	ts.fatalMtx.Unlock()
}

func (ts *TStorage) Order(oid order.OrderID, base, quote uint32) (order.Order, order.OrderStatus, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	if ts.orders != nil {
		ord, found := ts.orders[oid]
		if !found {
			return nil, order.OrderStatusUnknown, db.ArchiveError{Code: db.ErrUnknownOrder}
		}
		return ord, order.OrderStatusExecuted, nil
	}
	return nil, order.OrderStatusUnknown, nil // not loading swaps
}
func (ts *TStorage) ActiveSwaps() ([]*db.SwapDataFull, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	return ts.activeSwaps, nil
}

func (ts *TStorage) SwapDataFullByID(mid order.MatchID) (*db.SwapDataFull, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	if ts.swapDataByIDErr != nil {
		return nil, ts.swapDataByIDErr
	}
	if ts.swapDataByID == nil {
		return nil, nil
	}
	return ts.swapDataByID[mid], nil
}
func (ts *TStorage) EventLogFrontier(context.Context) (*db.EventLogPosition, error) {
	return &db.EventLogPosition{}, nil
}
func (ts *TStorage) EventLogEntriesAfter(context.Context, uint64, int) ([]*db.EventLogEntry, error) {
	return nil, nil
}

func (ts *TStorage) ApplyMatchAcksRecordedEvent(_ context.Context, _ *db.EventLogMeta, update *db.MatchAcksRecordedUpdate) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()

	ts.matchAcksRecordedUpdates = append(ts.matchAcksRecordedUpdates, update)
	if ts.applyMatchAcksRecordedErr != nil {
		return nil, ts.applyMatchAcksRecordedErr
	}
	return new(db.EventLogEntry), nil
}

func (ts *TStorage) ApplySwapContractRecordedEvent(_ context.Context, _ *db.EventLogMeta, contract *db.SwapContract) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	if ts.saveContractErr != nil {
		return nil, ts.saveContractErr
	}
	ts.swapContracts = append(ts.swapContracts, contract)
	return new(db.EventLogEntry), nil
}

func (ts *TStorage) ApplyAuditAckRecordedEvent(_ context.Context, _ *db.EventLogMeta, ack *db.AuditAck) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	if ts.saveAuditAckSigErr != nil {
		return nil, ts.saveAuditAckSigErr
	}
	ts.auditAcks = append(ts.auditAcks, ack)
	return new(db.EventLogEntry), nil
}

func (ts *TStorage) ApplySwapRedemptionRecordedEvent(_ context.Context, _ *db.EventLogMeta, _ *db.ReputationOutcomePolicy, redemption *db.SwapRedemption) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	ts.redemptions = append(ts.redemptions, redemption)
	if ts.applyRedemptionErr != nil {
		return nil, ts.applyRedemptionErr
	}
	return new(db.EventLogEntry), nil
}
func (ts *TStorage) ApplyRedemptionAckRecordedEvent(_ context.Context, _ *db.EventLogMeta, ack *db.RedemptionAck) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	if ts.applyRedemptionAckErr != nil {
		return nil, ts.applyRedemptionAckErr
	}
	ts.redemptionAcks = append(ts.redemptionAcks, ack)
	return new(db.EventLogEntry), nil
}

func (ts *TStorage) ApplyMatchFailedEvent(_ context.Context, _ *db.EventLogMeta, _ *db.ReputationOutcomePolicy, update *db.MatchFailedUpdate) (*db.EventLogEntry, error) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	ts.matchFailedEvents++
	ts.matchFailedUpdates = append(ts.matchFailedUpdates, update)
	if ts.setMatchInactiveErr != nil {
		return nil, ts.setMatchInactiveErr
	}
	details, _ := db.MatchFailureReasonDetails(update.Reason)
	ts.inactiveMatches = append(ts.inactiveMatches, inactiveMatchWrite{
		mid:     update.MID,
		forgive: !details.UserFault(),
	})
	return new(db.EventLogEntry), nil
}

type redeemKey struct {
	redemptionCoin       string
	counterpartySwapCoin string
}

// This stub satisfies asset.Backend.
type TBackend struct {
	mtx             sync.RWMutex
	contracts       map[string]*asset.Contract
	contractErr     error
	fundsErr        error
	redemptions     map[redeemKey]asset.Coin
	redemptionErr   error
	bChan           chan *asset.BlockUpdate // to trigger processBlock and eventually (after up to BroadcastTimeout) checkInaction depending on block time
	lbl             string
	invalidFeeRate  bool
	rejectSwapAddrs bool
}

func newTBackend(lbl string) TBackend {
	return TBackend{
		bChan:       make(chan *asset.BlockUpdate, 5),
		lbl:         lbl,
		contracts:   make(map[string]*asset.Contract),
		redemptions: make(map[redeemKey]asset.Coin),
		fundsErr:    asset.CoinNotFoundError,
	}
}

func newUTXOBackend(lbl string) *TUTXOBackend {
	return &TUTXOBackend{TBackend: newTBackend(lbl)}
}

func newAccountBackend(lbl string) *TAccountBackend {
	return &TAccountBackend{newTBackend(lbl)}
}

func (a *TBackend) Contract(coinID, redeemScript []byte) (*asset.Contract, error) {
	a.mtx.RLock()
	defer a.mtx.RUnlock()
	if a.contractErr != nil {
		return nil, a.contractErr
	}
	contract, found := a.contracts[string(coinID)]
	if !found || contract == nil {
		return nil, asset.CoinNotFoundError
	}

	return contract, nil
}
func (a *TBackend) Redemption(redemptionID, cpSwapCoinID, contractData []byte) (asset.Coin, error) {
	a.mtx.RLock()
	defer a.mtx.RUnlock()
	if a.redemptionErr != nil {
		return nil, a.redemptionErr
	}
	redeem, found := a.redemptions[redeemKey{string(redemptionID), string(cpSwapCoinID)}]
	if !found || redeem == nil {
		return nil, asset.CoinNotFoundError
	}
	return redeem, nil
}
func (a *TBackend) ValidateCoinID(coinID []byte) (string, error) {
	return "", nil
}
func (a *TBackend) ValidateContract(contract []byte) error {
	return nil
}
func (a *TBackend) BlockChannel(size int) <-chan *asset.BlockUpdate  { return a.bChan }
func (a *TBackend) FeeRate(context.Context) (uint64, error)          { return 10, nil }
func (a *TBackend) CheckSwapAddress(string) bool                     { return !a.rejectSwapAddrs }
func (a *TBackend) Connect(context.Context) (*sync.WaitGroup, error) { return nil, nil }
func (a *TBackend) ValidateSecret(secret, contract []byte) bool      { return true }
func (a *TBackend) Synced() (bool, error)                            { return true, nil }
func (a *TBackend) TxData([]byte) ([]byte, error) {
	return nil, nil
}

func (a *TBackend) setContractErr(err error) {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	a.contractErr = err
}
func (a *TBackend) setContract(contract *asset.Contract, resetErr bool) {
	a.mtx.Lock()
	a.contracts[string(contract.ID())] = contract
	if resetErr {
		a.contractErr = nil
	}
	a.mtx.Unlock()
}

func (a *TBackend) setRedemptionErr(err error) {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	a.redemptionErr = err
}
func (a *TBackend) setRedemption(redeem asset.Coin, cpSwap asset.Coin, resetErr bool) {
	a.mtx.Lock()
	a.redemptions[redeemKey{string(redeem.ID()), string(cpSwap.ID())}] = redeem
	if resetErr {
		a.redemptionErr = nil
	}
	a.mtx.Unlock()
}
func (*TBackend) Info() *asset.BackendInfo {
	return &asset.BackendInfo{}
}
func (a *TBackend) ValidateFeeRate(asset.Coin, uint64) bool {
	return !a.invalidFeeRate
}

type TUTXOBackend struct {
	TBackend
	funds asset.FundingCoin
}

func (a *TUTXOBackend) FundingCoin(_ context.Context, coinID, redeemScript []byte) (asset.FundingCoin, error) {
	a.mtx.RLock()
	defer a.mtx.RUnlock()
	return a.funds, a.fundsErr
}

func (a *TUTXOBackend) VerifyUnspentCoin(_ context.Context, coinID []byte) error { return nil }

type TAccountBackend struct {
	TBackend
}

var _ asset.AccountBalancer = (*TAccountBackend)(nil)

func (b *TAccountBackend) AccountBalance(addr string) (uint64, error) {
	return 0, nil
}

func (b *TAccountBackend) ValidateSignature(addr string, pubkey, msg, sig []byte) error {
	return nil
}

func (a *TAccountBackend) InitTxSize() uint64 { return 100 }

func (b *TAccountBackend) RedeemSize() uint64 {
	return 21_000
}

// This stub satisfies asset.Transaction, used by asset.Backend.
type TCoin struct {
	mtx       sync.RWMutex
	id        []byte
	confs     int64
	confsErr  error
	auditAddr string
	auditVal  uint64
	feeRate   uint64
}

func (coin *TCoin) Confirmations(context.Context) (int64, error) {
	coin.mtx.RLock()
	defer coin.mtx.RUnlock()
	return coin.confs, coin.confsErr
}

func (coin *TCoin) Addresses() []string {
	return []string{coin.auditAddr}
}

func (coin *TCoin) setConfs(confs int64) {
	coin.mtx.Lock()
	defer coin.mtx.Unlock()
	coin.confs = confs
}

func (coin *TCoin) Auth(pubkeys, sigs [][]byte, msg []byte) error { return nil }
func (coin *TCoin) ID() []byte                                    { return coin.id }
func (coin *TCoin) TxID() string                                  { return hex.EncodeToString(coin.id) }
func (coin *TCoin) Value() uint64                                 { return coin.auditVal }
func (coin *TCoin) SpendSize() uint32                             { return 0 }
func (coin *TCoin) String() string                                { return hex.EncodeToString(coin.id) /* not txid:vout */ }

func (coin *TCoin) FeeRate() uint64 {
	return coin.feeRate
}

func TNewAsset(backend asset.Backend, assetID uint32) *asset.BackedAsset {
	return &asset.BackedAsset{
		Backend: backend,
		Asset: dex.Asset{
			ID:         assetID,
			Symbol:     "qwe",
			MaxFeeRate: 120, // not used by Swapper other than prohibiting zero
			SwapConf:   2,
		},
	}
}

var testMsgID uint64

func nextID() uint64 {
	return atomic.AddUint64(&testMsgID, 1)
}

func tNewResponse(id uint64, resp []byte) *msgjson.Message {
	msg, _ := msgjson.NewResponse(id, json.RawMessage(resp), nil)
	return msg
}

// testRig is the primary test data structure.
type testRig struct {
	abc           *asset.BackedAsset
	abcNode       *TUTXOBackend
	xyz           *asset.BackedAsset
	xyzNode       *TUTXOBackend
	acctAsset     *asset.BackedAsset
	acctNode      *TAccountBackend
	auth          *TAuthManager
	swapper       *Swapper
	swapperWaiter *dex.StartStopWaiter
	swapperDone   chan struct{}
	storage       *TStorage
	matches       *tMatchSet
	matchInfo     *tMatch
}

type tSwapMesh struct {
	err        error
	commandErr *msgjson.Error
	reqs       []mesh.CommandRequest
	events     []*mesh.Event
	applier    map[string]mesh.EventApplier
}

func (m *tSwapMesh) ExecuteCommand(_ context.Context, req mesh.CommandRequest) *msgjson.Error {
	m.reqs = append(m.reqs, req)
	return m.commandErr
}

func (m *tSwapMesh) ApplyEvent(ctx context.Context, event *mesh.Event) (any, error) {
	cpy := &mesh.Event{
		Kind:    event.Kind,
		Payload: append([]byte(nil), event.Payload...),
	}
	m.events = append(m.events, cpy)
	if m.err != nil {
		return nil, m.err
	}
	if m.applier == nil {
		return nil, nil
	}
	applier := m.applier[cpy.Kind]
	if applier == nil {
		return nil, fmt.Errorf("unsupported test swap event %q", cpy.Kind)
	}
	applyCtx := &mesh.EventApplyContext{Context: ctx}
	_, err := applier(applyCtx, cpy)
	return applyCtx.Result(), err
}

func matchAcksRecordedUpdates(storage *TStorage) []*db.MatchAcksRecordedUpdate {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]*db.MatchAcksRecordedUpdate(nil), storage.matchAcksRecordedUpdates...)
}

func notificationCount(auth *TAuthManager, route string) int {
	auth.mtx.Lock()
	defer auth.mtx.Unlock()
	var n int
	for _, msgs := range auth.ntfns {
		for _, msg := range msgs {
			if msg.Route == route {
				n++
			}
		}
	}
	return n
}

func tNewUnstartedRig(matchInfo *tMatch) *testRig {
	return tNewUnstartedRigWithStorage(matchInfo, nil)
}

// tNewUnstartedRigWithStorage builds an unstarted rig, letting the caller
// shape the storage stub before RestoreActiveSwaps restores from it.
func tNewUnstartedRigWithStorage(matchInfo *tMatch, prepStorage func(*TStorage)) *testRig {
	rig := tBuildUnstartedRig(matchInfo, prepStorage)
	if err := rig.swapper.RestoreActiveSwaps(false); err != nil {
		panic(err.Error())
	}
	return rig
}

// tBuildUnstartedRig builds an unstarted rig without restoring active swaps,
// so tests can exercise RestoreActiveSwaps failures directly.
func tBuildUnstartedRig(matchInfo *tMatch, prepStorage func(*TStorage)) *testRig {
	storage := &TStorage{}
	if prepStorage != nil {
		prepStorage(storage)
	}
	authMgr := newTAuthManager()

	abcBackend := newUTXOBackend("abc")
	xyzBackend := newUTXOBackend("xyz")
	acctBackend := newAccountBackend("acct")

	abcAsset := TNewAsset(abcBackend, ABCID)
	abcCoinLocker := coinlock.NewAssetCoinLocker()

	xyzAsset := TNewAsset(xyzBackend, XYZID)
	xyzCoinLocker := coinlock.NewAssetCoinLocker()

	acctAsset := TNewAsset(acctBackend, ACCTID)

	swapper, err := NewSwapper(&Config{
		Assets: map[uint32]*SwapperAsset{
			ABCID:  {abcAsset, abcCoinLocker},
			XYZID:  {xyzAsset, xyzCoinLocker},
			ACCTID: {BackedAsset: acctAsset}, // no coin locker for account based asset.
		},
		Storage:          storage,
		AuthManager:      authMgr,
		BroadcastTimeout: tBcastTimeout,
		TxWaitExpiration: txWaitExpiration,
		LockTimeTaker:    dex.LockTimeTaker(dex.Testnet),
		LockTimeMaker:    dex.LockTimeMaker(dex.Testnet),
		SwapDone:         func(order.Order, *order.Match, bool) {},
	})
	if err != nil {
		panic(err.Error())
	}

	return &testRig{
		abc:       abcAsset,
		abcNode:   abcBackend,
		xyz:       xyzAsset,
		xyzNode:   xyzBackend,
		acctAsset: acctAsset,
		acctNode:  acctBackend,
		auth:      authMgr,
		swapper:   swapper,
		storage:   storage,
		matchInfo: matchInfo,
	}
}

func tNewTestRig(matchInfo *tMatch) (*testRig, func()) {
	rig := tNewUnstartedRig(matchInfo)
	swapper := rig.swapper
	storage := rig.storage

	swapperDone := make(chan struct{})
	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		Commands:       swapper.Commands(),
		Events:         swapper.Events(),
		EventLogReader: storage,
		OnHalt:         func(error) {},
		MasterWorkers: []mesh.MasterWorker{{
			Name: "Swapper",
			Run: func(ctx context.Context, reportReady func(error)) {
				defer close(swapperDone)
				swapper.Run(ctx, reportReady)
			},
		}, {
			// Mirrors the production sentinel that enables the inaction
			// checks once the last market has reported ready.
			Name: "MarketsReady",
			Run: func(ctx context.Context, reportReady func(error)) {
				swapper.MarketsReady()
				reportReady(nil)
				<-ctx.Done()
			},
		}},
		Logger: dex.Disabled,
	})
	if err != nil {
		panic(err.Error())
	}
	swapper.SetMeshService(meshSvc)
	ssw := dex.NewStartStopWaiter(meshSvc)
	ssw.Start(testCtx)
	if err := meshSvc.WaitUntilReadyForComms(testCtx); err != nil {
		panic(err.Error())
	}
	cleanup := func() {
		ssw.Stop()
		ssw.WaitForShutdown()
	}

	rig.swapperWaiter = ssw
	rig.swapperDone = swapperDone
	return rig, cleanup
}

func TestNewSwapper(t *testing.T) {
	newConfig := func() *Config {
		abcBackend := newUTXOBackend("abc")
		xyzBackend := newUTXOBackend("xyz")
		return &Config{
			Assets: map[uint32]*SwapperAsset{
				ABCID: {BackedAsset: TNewAsset(abcBackend, ABCID), Locker: coinlock.NewAssetCoinLocker()},
				XYZID: {BackedAsset: TNewAsset(xyzBackend, XYZID), Locker: coinlock.NewAssetCoinLocker()},
			},
			Storage:          &TStorage{},
			AuthManager:      newTAuthManager(),
			BroadcastTimeout: tBcastTimeout,
			TxWaitExpiration: txWaitExpiration,
			LockTimeTaker:    dex.LockTimeTaker(dex.Testnet),
			LockTimeMaker:    dex.LockTimeMaker(dex.Testnet),
			SwapDone:         func(order.Order, *order.Match, bool) {},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "valid",
		},
		{
			name: "missing swap done applier",
			mutate: func(cfg *Config) {
				cfg.SwapDone = nil
			},
			wantErr: "swap-done applier is not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newConfig()
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			swapper, err := NewSwapper(cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewSwapper error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSwapper error: %v", err)
			}
			if swapper.mesh != nil {
				t.Fatalf("NewSwapper configured mesh service")
			}
		})
	}
}

func (rig *testRig) applyMatchesAndRequestAcks(t *testing.T, matchSets ...*order.MatchSet) {
	t.Helper()
	if err := rig.swapper.TrackMatches(matchSets); err != nil {
		t.Fatalf("ApplyMatches: %v", err)
	}
	rig.swapper.RequestMatchAcks(matchSets)
}

func (rig *testRig) getTracker() *matchTracker {
	rig.swapper.matchMtx.Lock()
	defer rig.swapper.matchMtx.Unlock()
	return rig.swapper.matches[rig.matchInfo.matchID]
}

// waitChans waits on the specified channels sequentially in the order given.
// nil channels are ignored.
func (rig *testRig) waitChans(tag string, chans ...chan struct{}) error {
	for _, c := range chans {
		if c == nil {
			continue
		}
		select {
		case <-c:
		case <-time.After(time.Second):
			return fmt.Errorf("waiting on %q timed out", tag)
		}
	}
	return nil
}

// perMatchAddrs builds a map from match ID to per-match address for the given
// user by scanning all match infos, or just rig.matchInfo when rig.matches is
// not set.
func (rig *testRig) perMatchAddrs(user *tUser, isMaker bool) map[order.MatchID]string {
	addrs := make(map[order.MatchID]string)
	matchInfos := []*tMatch{rig.matchInfo}
	if rig.matches != nil {
		matchInfos = rig.matches.matchInfos
	}
	for _, mi := range matchInfos {
		if isMaker && mi.maker == user {
			addrs[mi.matchID] = mi.makerPerMatchAddr
		} else if !isMaker && mi.taker == user {
			addrs[mi.matchID] = mi.takerPerMatchAddr
		}
	}
	return addrs
}

// Maker: Acknowledge the servers match notification.
func (rig *testRig) ackMatch_maker(checkSig bool) (err error) {
	matchInfo := rig.matchInfo
	err = rig.ackMatch(matchInfo.maker, matchInfo.makerOID, matchInfo.taker.addr,
		rig.perMatchAddrs(matchInfo.maker, true))
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if !bytes.Equal(tracker.Sigs.MakerMatch, matchInfo.maker.sig) {
			return fmt.Errorf("expected maker audit signature '%x', got '%x'", matchInfo.maker.sig, tracker.Sigs.MakerMatch)
		}
	}
	return nil
}

// Taker: Acknowledge the servers match notification.
func (rig *testRig) ackMatch_taker(checkSig bool) error {
	matchInfo := rig.matchInfo
	err := rig.ackMatch(matchInfo.taker, matchInfo.takerOID, matchInfo.maker.addr,
		rig.perMatchAddrs(matchInfo.taker, false))
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if !bytes.Equal(tracker.Sigs.TakerMatch, matchInfo.taker.sig) {
			return fmt.Errorf("expected taker audit signature '%x', got '%x'", matchInfo.taker.sig, tracker.Sigs.TakerMatch)
		}
	}
	return nil
}

func (rig *testRig) ackMatch(user *tUser, oid order.OrderID, counterAddr string, matchAddrs map[order.MatchID]string) error {
	req := rig.auth.popReq(user.acct)
	if req == nil {
		return fmt.Errorf("failed to find match notification for %s", user.lbl)
	}
	if req.req.Route != msgjson.MatchRoute {
		return fmt.Errorf("expected method '%s', got '%s'", msgjson.MatchRoute, req.req.Route)
	}
	err := rig.checkMatchNotification(req.req, oid, counterAddr)
	if err != nil {
		return err
	}
	// The maker and taker would sign the notifications and return a list of
	// authorizations, including per-match swap addresses.
	resp := tNewResponse(req.req.ID, tAckArrWithAddrs(user, user.matchIDs, matchAddrs))
	req.respFunc(nil, resp) // e.g. processMatchAcks, may Send resp on error
	return nil
}

// Helper to check the match notifications.
func (rig *testRig) checkMatchNotification(msg *msgjson.Message, oid order.OrderID, counterAddr string) error {
	matchInfo := rig.matchInfo
	var notes []*msgjson.Match
	err := json.Unmarshal(msg.Payload, &notes)
	if err != nil {
		fmt.Printf("checkMatchNotification unmarshal error: %v\n", err)
	}
	var notification *msgjson.Match
	for _, n := range notes {
		if bytes.Equal(n.MatchID, matchInfo.matchID[:]) {
			notification = n
			break
		}
		if err = checkSigS256(n, rig.auth.privkey.PubKey()); err != nil {
			return fmt.Errorf("incorrect server signature: %w", err)
		}
	}
	if notification == nil {
		return fmt.Errorf("did not find match ID %s in match notifications", matchInfo.matchID)
	}
	if notification.OrderID.String() != oid.String() {
		return fmt.Errorf("expected order ID %s, got %s", oid, notification.OrderID)
	}
	if notification.Quantity != matchInfo.qty {
		return fmt.Errorf("expected order quantity %d, got %d", matchInfo.qty, notification.Quantity)
	}
	if notification.Rate != matchInfo.rate {
		return fmt.Errorf("expected match rate %d, got %d", matchInfo.rate, notification.Rate)
	}
	if notification.Address != counterAddr {
		return fmt.Errorf("expected match address %s, got %s", counterAddr, notification.Address)
	}
	return nil
}

// Helper to check the swap status for specified user.
// Swap counterparty usually gets a request from server notifying that it's time
// for his turn. Synchronize with that before checking status change.
func (rig *testRig) ensureSwapStatus(tag string, wantStatus order.MatchStatus, waitOn ...chan struct{}) error {
	if err := rig.waitChans(tag, waitOn...); err != nil {
		return err
	}
	tracker := rig.getTracker()
	status := tracker.Status
	if status != wantStatus {
		return fmt.Errorf("unexpected swap status %d after maker swap notification, wanted: %s", status, wantStatus)
	}
	return nil
}

// Can be used to ensure that a non-error response is returned from the swapper.
func (rig *testRig) checkServerResponseSuccess(user *tUser) error {
	msg, resp := rig.auth.popResp(user.acct)
	if msg == nil {
		return fmt.Errorf("unexpected nil response to %s's request", user.lbl)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s swap rpc error. code: %d, msg: %s", user.lbl, resp.Error.Code, resp.Error.Message)
	}
	return nil
}

// Can be used to ensure that an error response is returned from the swapper.
func (rig *testRig) checkServerResponseFail(user *tUser, code int, grep ...string) error {
	msg, resp := rig.auth.popResp(user.acct)
	if msg == nil {
		return fmt.Errorf("no response for %s", user.lbl)
	}
	if resp.Error == nil {
		return fmt.Errorf("no error for %s", user.lbl)
	}
	if resp.Error.Code != code {
		return fmt.Errorf("wrong error code for %s. expected %d, got %d", user.lbl, code, resp.Error.Code)
	}
	if len(grep) > 0 && !strings.Contains(resp.Error.Message, grep[0]) {
		return fmt.Errorf("error missing the message %q", grep[0])
	}
	return nil
}

// swapRecipient returns the per-match swap address for the given side if one
// has been stored on the match tracker (via the ack flow), otherwise falls back
// to the order-level address.
func (rig *testRig) swapRecipient(isMaker bool) string {
	matchInfo := rig.matchInfo
	tracker := rig.getTracker()
	tracker.mtx.RLock()
	defer tracker.mtx.RUnlock()
	if isMaker {
		// Maker's swap pays to the taker's address.
		if tracker.takerSwapAddr != "" {
			return tracker.takerSwapAddr
		}
		return matchInfo.taker.addr
	}
	// Taker's swap pays to the maker's address.
	if tracker.makerSwapAddr != "" {
		return tracker.makerSwapAddr
	}
	return matchInfo.maker.addr
}

// Maker: Send swap transaction (swap init request).
func (rig *testRig) sendSwap_maker(expectSuccess bool) (err error) {
	matchInfo := rig.matchInfo
	swap, err := rig.sendSwap(matchInfo.maker, matchInfo.makerOID, rig.swapRecipient(true))
	if err != nil {
		return fmt.Errorf("error sending maker swap request: %w", err)
	}
	matchInfo.db.makerSwap = swap
	if expectSuccess {
		err = rig.ensureSwapStatus("server received our swap -> counterparty got audit request",
			order.MakerSwapCast, rig.auth.swapReceived, rig.auth.auditReq)
		if err != nil {
			return fmt.Errorf("ensure swap status: %w", err)
		}
		err = rig.checkServerResponseSuccess(matchInfo.maker)
		if err != nil {
			return fmt.Errorf("check server response success: %w", err)
		}
	}
	return nil
}

// Taker: Send swap transaction (swap init request).
func (rig *testRig) sendSwap_taker(expectSuccess bool) (err error) {
	matchInfo := rig.matchInfo
	swap, err := rig.sendSwap(matchInfo.taker, matchInfo.takerOID, rig.swapRecipient(false))
	if err != nil {
		return fmt.Errorf("error sending taker swap request: %w", err)
	}

	matchInfo.db.takerSwap = swap
	if expectSuccess {
		err = rig.ensureSwapStatus("server received our swap -> counterparty got audit request",
			order.TakerSwapCast, rig.auth.swapReceived, rig.auth.auditReq)
		if err != nil {
			return fmt.Errorf("ensure swap status: %w", err)
		}
		err = rig.checkServerResponseSuccess(matchInfo.taker)
		if err != nil {
			return fmt.Errorf("check server response success: %w", err)
		}
	}
	return nil
}

func (rig *testRig) sendSwap(user *tUser, oid order.OrderID, recipient string) (*tSwap, error) {
	matchInfo := rig.matchInfo
	swap := tNewSwap(matchInfo, oid, recipient, user)
	if isQuoteSwap(user, matchInfo.match) {
		rig.xyzNode.setContract(swap.coin, false)
	} else {
		rig.abcNode.setContract(swap.coin, false)
	}
	rig.auth.swapID = swap.req.ID
	rpcErr := rig.swapper.handleInit(user.acct, swap.req)
	if rpcErr != nil {
		resp, _ := msgjson.NewResponse(swap.req.ID, nil, rpcErr)
		_ = rig.auth.Send(user.acct, resp)
		return nil, fmt.Errorf("%s swap rpc error. code: %d, msg: %s", user.lbl, rpcErr.Code, rpcErr.Message)
	}
	return swap, nil
}

// Taker: Process the 'audit' request from the swapper. The request should be
// acknowledged separately with ackAudit_taker.
func (rig *testRig) auditSwap_taker() error {
	matchInfo := rig.matchInfo
	req := rig.auth.popReq(matchInfo.taker.acct)
	matchInfo.db.takerAudit = req
	if req == nil {
		return fmt.Errorf("failed to find audit request for taker after maker's init")
	}
	return rig.auditSwap(req.req, matchInfo.takerOID, matchInfo.db.makerSwap, "taker", matchInfo.taker)
}

// Maker: Process the 'audit' request from the swapper. The request should be
// acknowledged separately with ackAudit_maker.
func (rig *testRig) auditSwap_maker() error {
	matchInfo := rig.matchInfo
	req := rig.auth.popReq(matchInfo.maker.acct)
	matchInfo.db.makerAudit = req
	if req == nil {
		return fmt.Errorf("failed to find audit request for maker after taker's init")
	}
	return rig.auditSwap(req.req, matchInfo.makerOID, matchInfo.db.takerSwap, "maker", matchInfo.maker)
}

// checkSigS256 checks that the message's signature was created with the
// private key for the provided secp256k1 public key.
func checkSigS256(msg msgjson.Signable, pubKey *secp256k1.PublicKey) error {
	signature, err := ecdsa.ParseDERSignature(msg.SigBytes())
	if err != nil {
		return fmt.Errorf("error decoding secp256k1 Signature from bytes: %w", err)
	}
	msgB := msg.Serialize()
	hash := sha256.Sum256(msgB)
	if !signature.Verify(hash[:], pubKey) {
		return fmt.Errorf("secp256k1 signature verification failed")
	}
	return nil
}

func (rig *testRig) auditSwap(msg *msgjson.Message, oid order.OrderID, swap *tSwap, tag string, user *tUser) error {
	if msg == nil {
		return fmt.Errorf("no %s 'audit' request from DEX", user.lbl)
	}

	if msg.Route != msgjson.AuditRoute {
		return fmt.Errorf("expected method '%s', got '%s'", msgjson.AuditRoute, msg.Route)
	}
	var params *msgjson.Audit
	err := json.Unmarshal(msg.Payload, &params)
	if err != nil {
		return fmt.Errorf("error unmarshaling audit params: %w", err)
	}
	if err = checkSigS256(params, rig.auth.privkey.PubKey()); err != nil {
		return fmt.Errorf("incorrect server signature: %w", err)
	}
	if params.OrderID.String() != oid.String() {
		return fmt.Errorf("%s : incorrect order ID in auditSwap, expected '%s', got '%s'", tag, oid, params.OrderID)
	}
	matchID := rig.matchInfo.matchID
	if params.MatchID.String() != matchID.String() {
		return fmt.Errorf("%s : incorrect match ID in auditSwap, expected '%s', got '%s'", tag, matchID, params.MatchID)
	}
	if params.Contract.String() != swap.contract {
		return fmt.Errorf("%s : incorrect contract. expected '%s', got '%s'", tag, swap.contract, params.Contract)
	}
	if !bytes.Equal(params.TxData, swap.coin.TxData) {
		return fmt.Errorf("%s : incorrect tx data. expected '%s', got '%s'", tag, swap.coin.TxData, params.TxData)
	}
	return nil
}

// Maker: Acknowledge the DEX 'audit' request.
func (rig *testRig) ackAudit_maker(checkSig bool) error {
	maker := rig.matchInfo.maker
	err := rig.ackAudit(maker, rig.matchInfo.db.makerAudit)
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if !bytes.Equal(tracker.Sigs.MakerAudit, maker.sig) {
			return fmt.Errorf("expected taker audit signature '%x', got '%x'", maker.sig, tracker.Sigs.MakerAudit)
		}
	}
	return nil
}

// Taker: Acknowledge the DEX 'audit' request.
func (rig *testRig) ackAudit_taker(checkSig bool) error {
	taker := rig.matchInfo.taker
	err := rig.ackAudit(taker, rig.matchInfo.db.takerAudit)
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if !bytes.Equal(tracker.Sigs.TakerAudit, taker.sig) {
			return fmt.Errorf("expected taker audit signature '%x', got '%x'", taker.sig, tracker.Sigs.TakerAudit)
		}
	}
	return nil
}

func (rig *testRig) ackAudit(user *tUser, req *TRequest) error {
	if req == nil {
		return fmt.Errorf("no %s 'audit' request from DEX", user.lbl)
	}
	req.respFunc(nil, tNewResponse(req.req.ID, tAck(user, rig.matchInfo.matchID)))
	return nil
}

// Maker: Redeem taker's swap transaction.
func (rig *testRig) redeem_maker(expectSuccess bool) error {
	matchInfo := rig.matchInfo
	redeem, err := rig.redeem(matchInfo.maker, matchInfo.makerOID)
	if err != nil {
		return fmt.Errorf("error sending maker redeem request: %w", err)
	}
	matchInfo.db.makerRedeem = redeem
	if expectSuccess {
		err = rig.ensureSwapStatus("server received our redeem -> counterparty got redemption request",
			order.MakerRedeemed, rig.auth.redeemReceived, rig.auth.redemptionReq)
		if err != nil {
			return fmt.Errorf("ensure swap status: %w", err)
		}
		err = rig.checkServerResponseSuccess(matchInfo.maker)
		if err != nil {
			return fmt.Errorf("check server response success: %w", err)
		}
	}
	return nil
}

// Taker: Redeem maker's swap transaction.
func (rig *testRig) redeem_taker(expectSuccess bool) error {
	matchInfo := rig.matchInfo
	redeem, err := rig.redeem(matchInfo.taker, matchInfo.takerOID)
	if err != nil {
		return fmt.Errorf("error sending taker redeem request: %w", err)
	}
	matchInfo.db.takerRedeem = redeem
	if expectSuccess {
		if err := rig.waitChans("server received our redeem and we got a redemption note", rig.auth.redeemReceived, rig.auth.redemptionReq); err != nil {
			return err
		}
		if tracker := rig.getTracker(); tracker != nil {
			return fmt.Errorf("expected completed match to be removed, found it in status %v", tracker.Status)
		}
		err = rig.checkServerResponseSuccess(matchInfo.taker)
		if err != nil {
			return fmt.Errorf("check server response success: %w", err)
		}
	}
	return nil
}

func (rig *testRig) redeem(user *tUser, oid order.OrderID) (*tRedeem, error) {
	matchInfo := rig.matchInfo
	redeem := tNewRedeem(matchInfo, oid, user)
	if isQuoteSwap(user, matchInfo.match) {
		// do not clear redemptionErr yet
		rig.abcNode.setRedemption(redeem.coin, redeem.cpSwapCoin, false)
	} else {
		rig.xyzNode.setRedemption(redeem.coin, redeem.cpSwapCoin, false)
	}
	rig.auth.redeemID = redeem.req.ID
	rpcErr := rig.swapper.handleRedeem(user.acct, redeem.req)
	if rpcErr != nil {
		resp, _ := msgjson.NewResponse(redeem.req.ID, nil, rpcErr)
		_ = rig.auth.Send(user.acct, resp)
		return nil, fmt.Errorf("%s swap rpc error. code: %d, msg: %s", user.lbl, rpcErr.Code, rpcErr.Message)
	}
	return redeem, nil
}

// Taker: Acknowledge the DEX 'redemption' request.
func (rig *testRig) ackRedemption_taker(checkSig bool) error {
	matchInfo := rig.matchInfo
	err := rig.ackRedemption(matchInfo.taker, matchInfo.takerOID, matchInfo.db.makerRedeem)
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if !bytes.Equal(tracker.Sigs.TakerRedeem, matchInfo.taker.sig) {
			return fmt.Errorf("expected taker redemption signature '%x', got '%x'", matchInfo.taker.sig, tracker.Sigs.TakerRedeem)
		}
	}
	return nil
}

// Maker: Acknowledge the DEX 'redemption' request.
func (rig *testRig) ackRedemption_maker(checkSig bool) error {
	matchInfo := rig.matchInfo
	err := rig.ackRedemption(matchInfo.maker, matchInfo.makerOID, matchInfo.db.takerRedeem)
	if err != nil {
		return err
	}
	if checkSig {
		tracker := rig.getTracker()
		if tracker != nil {
			return fmt.Errorf("expected match to be removed, found it, in status %v", tracker.Status)
		}
	}
	return nil
}

func (rig *testRig) ackRedemption(user *tUser, oid order.OrderID, redeem *tRedeem) error {
	if redeem == nil {
		return fmt.Errorf("nil redeem info")
	}
	req := rig.auth.popReq(user.acct)
	if req == nil {
		return fmt.Errorf("failed to find redemption request for %s", user.lbl)
	}
	err := rig.checkRedeem(req.req, oid, redeem.coin.ID(), user.lbl)
	if err != nil {
		return err
	}
	req.respFunc(nil, tNewResponse(req.req.ID, tAck(user, rig.matchInfo.matchID)))
	return nil
}

func (rig *testRig) checkRedeem(msg *msgjson.Message, oid order.OrderID, coinID []byte, tag string) error {
	var params *msgjson.Redemption
	err := json.Unmarshal(msg.Payload, &params)
	if err != nil {
		return fmt.Errorf("error unmarshaling redeem params: %w", err)
	}
	if err = checkSigS256(params, rig.auth.privkey.PubKey()); err != nil {
		return fmt.Errorf("incorrect server signature: %w", err)
	}
	if params.OrderID.String() != oid.String() {
		return fmt.Errorf("%s : incorrect order ID in checkRedeem, expected '%s', got '%s'", tag, oid, params.OrderID)
	}
	matchID := rig.matchInfo.matchID
	if params.MatchID.String() != matchID.String() {
		return fmt.Errorf("%s : incorrect match ID in checkRedeem, expected '%s', got '%s'", tag, matchID, params.MatchID)
	}
	if !bytes.Equal(params.CoinID, coinID) {
		return fmt.Errorf("%s : incorrect coinID in checkRedeem. expected '%x', got '%x'", tag, coinID, params.CoinID)
	}
	return nil
}

func makeCancelOrder(limitOrder *order.LimitOrder, user *tUser) *order.CancelOrder {
	return &order.CancelOrder{
		P: order.Prefix{
			AccountID:  user.acct,
			BaseAsset:  limitOrder.BaseAsset,
			QuoteAsset: limitOrder.QuoteAsset,
			OrderType:  order.CancelOrderType,
			ClientTime: unixMsNow(),
			ServerTime: unixMsNow(),
		},
		TargetOrderID: limitOrder.ID(),
	}
}

func makeLimitOrder(qty, rate uint64, user *tUser, makerSell bool) *order.LimitOrder {
	return &order.LimitOrder{
		P: order.Prefix{
			AccountID:  user.acct,
			BaseAsset:  ABCID,
			QuoteAsset: XYZID,
			OrderType:  order.LimitOrderType,
			ClientTime: time.UnixMilli(1566497654000),
			ServerTime: time.UnixMilli(1566497655000),
		},
		T: order.Trade{
			Sell:     makerSell,
			Quantity: qty,
			Address:  user.addr,
		},
		Rate: rate,
	}
}

func makeMarketOrder(qty uint64, user *tUser, makerSell bool) *order.MarketOrder {
	return &order.MarketOrder{
		P: order.Prefix{
			AccountID:  user.acct,
			BaseAsset:  ABCID,
			QuoteAsset: XYZID,
			OrderType:  order.LimitOrderType,
			ClientTime: time.UnixMilli(1566497654000),
			ServerTime: time.UnixMilli(1566497655000),
		},
		T: order.Trade{
			Sell:     makerSell,
			Quantity: qty,
			Address:  user.addr,
		},
	}
}

func limitLimitPair(makerQty, takerQty, makerRate, takerRate uint64, maker, taker *tUser, makerSell bool) (*order.LimitOrder, *order.LimitOrder) {
	return makeLimitOrder(makerQty, makerRate, maker, makerSell), makeLimitOrder(takerQty, takerRate, taker, !makerSell)
}

func marketLimitPair(makerQty, takerQty, rate uint64, maker, taker *tUser, makerSell bool) (*order.LimitOrder, *order.MarketOrder) {
	return makeLimitOrder(makerQty, rate, maker, makerSell), makeMarketOrder(takerQty, taker, !makerSell)
}

func tLimitPair(makerQty, takerQty, matchQty, makerRate, takerRate uint64, makerSell bool) *tMatchSet {
	set := new(tMatchSet)
	maker, taker := tNewUser("maker"), tNewUser("taker")
	makerOrder, takerOrder := limitLimitPair(makerQty, takerQty, makerRate, takerRate, maker, taker, makerSell)
	return set.add(tMatchInfo(maker, taker, matchQty, makerRate, makerOrder, takerOrder))
}

func tPerfectLimitLimit(qty, rate uint64, makerSell bool) *tMatchSet {
	return tLimitPair(qty, qty, qty, rate, rate, makerSell)
}

func tMarketPair(makerQty, takerQty, rate uint64, makerSell bool) *tMatchSet {
	set := new(tMatchSet)
	maker, taker := tNewUser("maker"), tNewUser("taker")
	makerOrder, takerOrder := marketLimitPair(makerQty, takerQty, rate, maker, taker, makerSell)
	return set.add(tMatchInfo(maker, taker, makerQty, rate, makerOrder, takerOrder))
}

func tPerfectLimitMarket(qty, rate uint64, makerSell bool) *tMatchSet {
	return tMarketPair(qty, qty, rate, makerSell)
}

func tCancelPair() *tMatchSet {
	set := new(tMatchSet)
	user := tNewUser("user")
	qty := uint64(1e8)
	rate := uint64(1e8)
	makerOrder := makeLimitOrder(qty, rate, user, true)
	cancelOrder := makeCancelOrder(makerOrder, user)
	return set.add(tMatchInfo(user, user, qty, rate, makerOrder, cancelOrder))
}

// tMatch is the match info for a single match. A tMatch is typically created
// with tMatchInfo.
type tMatch struct {
	match    *order.Match
	matchID  order.MatchID
	makerOID order.OrderID
	takerOID order.OrderID
	qty      uint64
	rate     uint64
	maker    *tUser
	taker    *tUser
	// Per-match swap addresses provided in match acks.
	makerPerMatchAddr string
	takerPerMatchAddr string
	// secretHash is the secret hash for this match, used in swap contracts.
	secretHash []byte
	db         struct {
		makerRedeem *tRedeem
		takerRedeem *tRedeem
		makerSwap   *tSwap
		takerSwap   *tSwap
		makerAudit  *TRequest
		takerAudit  *TRequest
	}
}

func makeAck(mid order.MatchID, sig []byte, addr string) msgjson.Acknowledgement {
	return msgjson.Acknowledgement{
		MatchID: mid[:],
		Sig:     sig,
		Address: addr,
	}
}

func tAck(user *tUser, matchID order.MatchID) []byte {
	b, _ := json.Marshal(makeAck(matchID, user.sig, ""))
	return b
}

// tAckArrWithAddrs builds acknowledgements for a user, using matchAddrs to
// supply per-match addresses for each match ID.
func tAckArrWithAddrs(user *tUser, matchIDs []order.MatchID, matchAddrs map[order.MatchID]string) []byte {
	ackArr := make([]msgjson.Acknowledgement, 0, len(matchIDs))
	for _, matchID := range matchIDs {
		ackArr = append(ackArr, makeAck(matchID, user.sig, matchAddrs[matchID]))
	}
	b, _ := json.Marshal(ackArr)
	return b
}

func tMatchInfo(maker, taker *tUser, matchQty, matchRate uint64, makerOrder *order.LimitOrder, takerOrder order.Order) *tMatch {
	match := &order.Match{
		Taker:        takerOrder,
		Maker:        makerOrder,
		Quantity:     matchQty,
		Rate:         matchRate,
		FeeRateBase:  42,
		FeeRateQuote: 62,
		Epoch: order.EpochID{ // make Epoch.End() be now, Idx and Dur not important per se
			Idx: uint64(time.Now().UnixMilli()),
			Dur: 1,
		}, // Need Epoch set for lock time
	}
	mid := match.ID()
	maker.matchIDs = append(maker.matchIDs, mid)
	taker.matchIDs = append(taker.matchIDs, mid)
	return &tMatch{
		match:             match,
		qty:               matchQty,
		rate:              matchRate,
		matchID:           mid,
		makerOID:          makerOrder.ID(),
		takerOID:          takerOrder.ID(),
		maker:             maker,
		taker:             taker,
		makerPerMatchAddr: fmt.Sprintf("maker-pmaddr-%s", mid),
		takerPerMatchAddr: fmt.Sprintf("taker-pmaddr-%s", mid),
		secretHash:        encode.RandomBytes(32),
	}
}

// Matches are submitted to the swapper in small batches, one for each taker.
type tMatchSet struct {
	matchSet   *order.MatchSet
	matchInfos []*tMatch
}

// Add a new match to the tMatchSet.
func (set *tMatchSet) add(matchInfo *tMatch) *tMatchSet {
	match := matchInfo.match
	if set.matchSet == nil {
		set.matchSet = &order.MatchSet{Taker: match.Taker}
	}
	if set.matchSet.Taker.User() != match.Taker.User() {
		fmt.Println("!!!tMatchSet taker mismatch!!!")
	}
	ms := set.matchSet
	ms.Epoch = matchInfo.match.Epoch
	ms.Makers = append(ms.Makers, match.Maker)
	ms.Amounts = append(ms.Amounts, matchInfo.qty)
	ms.Rates = append(ms.Rates, matchInfo.rate)
	ms.Total += matchInfo.qty
	// In practice, a MatchSet's fee rate is used to set the individual match
	// fee rates via (*MatchSet).Matches in ApplyMatches/RequestMatchAcks.
	ms.FeeRateBase = matchInfo.match.FeeRateBase
	ms.FeeRateQuote = matchInfo.match.FeeRateQuote
	set.matchInfos = append(set.matchInfos, matchInfo)
	return set
}

// Either a market or limit order taker, and any number of makers.
func tMultiMatchSet(matchQtys, rates []uint64, makerSell bool, isMarket bool) *tMatchSet {
	var sum uint64
	for _, v := range matchQtys {
		sum += v
	}
	// Taker order can be > sum of match amounts
	taker := tNewUser("taker")
	var takerOrder order.Order
	if isMarket {
		takerOrder = makeMarketOrder(sum*5/4, taker, !makerSell)
	} else {
		takerOrder = makeLimitOrder(sum*5/4, rates[0], taker, !makerSell)
	}
	set := new(tMatchSet)
	now := uint64(time.Now().UnixMilli())
	for i, v := range matchQtys {
		maker := tNewUser("maker" + strconv.Itoa(i))
		// Alternate market and limit orders
		makerOrder := makeLimitOrder(v, rates[i], maker, makerSell)
		matchInfo := tMatchInfo(maker, taker, v, rates[i], makerOrder, takerOrder)
		matchInfo.match.Epoch.Idx = now
		matchInfo.match.Epoch.Dur = 1
		set.add(matchInfo)
	}
	return set
}

// tSwap is the information needed for spoofing a swap transaction.
type tSwap struct {
	coin     *asset.Contract
	req      *msgjson.Message
	contract string
}

var (
	tConfsSpoofer     int64
	tValSpoofer       uint64 = 1
	tRecipientSpoofer        = ""
	tLockTimeSpoofer  time.Time
)

func tNewSwap(matchInfo *tMatch, oid order.OrderID, recipient string, user *tUser) *tSwap {
	auditVal := matchInfo.qty
	if isQuoteSwap(user, matchInfo.match) {
		auditVal = matcher.BaseToQuote(matchInfo.rate, matchInfo.qty)
	}
	coinID := randBytes(36)
	coin := &TCoin{
		feeRate:   1,
		confs:     tConfsSpoofer,
		auditAddr: recipient + tRecipientSpoofer,
		auditVal:  auditVal * tValSpoofer,
		id:        coinID,
	}

	contract := &asset.Contract{
		Coin:        coin,
		SwapAddress: recipient + tRecipientSpoofer,
		TxData:      encode.RandomBytes(100),
		SecretHash:  matchInfo.secretHash,
	}

	contract.LockTime = encode.DropMilliseconds(matchInfo.match.Epoch.End().Add(dex.LockTimeTaker(dex.Testnet)))
	if user == matchInfo.maker {
		contract.LockTime = encode.DropMilliseconds(matchInfo.match.Epoch.End().Add(dex.LockTimeMaker(dex.Testnet)))
	}

	if !tLockTimeSpoofer.IsZero() {
		contract.LockTime = tLockTimeSpoofer
	}

	script := "01234567" + user.sigHex
	req, _ := msgjson.NewRequest(nextID(), msgjson.InitRoute, &msgjson.Init{
		OrderID: oid[:],
		MatchID: matchInfo.matchID[:],
		// We control what the backend returns, so the txid doesn't matter right now.
		CoinID: coinID,
		//Time:     uint64(time.Now().UnixMilli()),
		Contract: dirtyEncode(script),
	})

	return &tSwap{
		coin:     contract,
		req:      req,
		contract: script,
	}
}

func isQuoteSwap(user *tUser, match *order.Match) bool {
	makerSell := match.Maker.Sell
	isMaker := user.acct == match.Maker.User()
	return (isMaker && !makerSell) || (!isMaker && makerSell)
}

func randBytes(len int) []byte {
	bytes := make([]byte, len)
	rand.Read(bytes)
	return bytes
}

// tRedeem is the information needed to spoof a redemption transaction.
type tRedeem struct {
	req        *msgjson.Message
	coin       *TCoin
	cpSwapCoin *asset.Contract
}

func tNewRedeem(matchInfo *tMatch, oid order.OrderID, user *tUser) *tRedeem {
	coinID := randBytes(36)
	req, _ := msgjson.NewRequest(nextID(), msgjson.RedeemRoute, &msgjson.Redeem{
		OrderID: oid[:],
		MatchID: matchInfo.matchID[:],
		CoinID:  coinID,
		//Time:    uint64(time.Now().UnixMilli()),
	})
	var cpSwapCoin *asset.Contract
	switch user.acct {
	case matchInfo.maker.acct:
		cpSwapCoin = matchInfo.db.takerSwap.coin
	case matchInfo.taker.acct:
		cpSwapCoin = matchInfo.db.makerSwap.coin
	default:
		panic("wrong user")
	}
	return &tRedeem{
		req:        req,
		coin:       &TCoin{id: coinID},
		cpSwapCoin: cpSwapCoin,
	}
}

// Create a closure that will call t.Fatal if an error is non-nil.
func makeEnsureNilErr(t *testing.T) func(error) {
	return func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMain(m *testing.M) {
	fastRecheckInterval = time.Millisecond * 40
	taperedRecheckInterval = time.Millisecond * 40
	txWaitExpiration = time.Millisecond * 200
	tBcastTimeout = txWaitExpiration * 5
	minBlockPeriod = tBcastTimeout / 3
	logger := dex.StdOutLogger("TEST", dex.LevelTrace)
	UseLogger(logger)
	db.UseLogger(logger)
	matcher.UseLogger(logger)
	comms.UseLogger(logger)
	var shutdown func()
	testCtx, shutdown = context.WithCancel(context.Background())
	code := m.Run()
	shutdown()
	os.Exit(code)
}

func TestFatalStorageErr(t *testing.T) {
	rig, cleanup := tNewTestRig(nil)
	defer cleanup()

	// Test a fatal storage backend error that causes the main loop to return
	// first, the anomalous shutdown route.
	rig.storage.fatalBackendErr(errors.New("backend error"))

	select {
	case <-rig.swapperDone:
	case <-time.After(time.Second):
		t.Fatalf("swapper worker did not stop after fatal storage error")
	}
}

func testSwap(t *testing.T, rig *testRig) {
	t.Helper()
	ensureNilErr := makeEnsureNilErr(t)

	sendBlock := func(node *TBackend) {
		node.bChan <- &asset.BlockUpdate{Err: nil}
	}

	// Step through the negotiation process. No errors should be generated.
	var takerAcked bool
	for _, matchInfo := range rig.matches.matchInfos {
		rig.matchInfo = matchInfo
		ensureNilErr(rig.ackMatch_maker(true))
		if !takerAcked {
			ensureNilErr(rig.ackMatch_taker(true))
			takerAcked = true
		}

		// Assuming market's base asset is abc, quote is xyz
		makerSwapAsset, takerSwapAsset := rig.xyz, rig.abc
		if matchInfo.match.Maker.Sell {
			makerSwapAsset, takerSwapAsset = rig.abc, rig.xyz
		}

		ensureNilErr(rig.sendSwap_maker(true))
		ensureNilErr(rig.auditSwap_taker())
		ensureNilErr(rig.ackAudit_taker(true))
		matchInfo.db.makerSwap.coin.Coin.(*TCoin).setConfs(int64(makerSwapAsset.SwapConf))
		sendBlock(&makerSwapAsset.Backend.(*TUTXOBackend).TBackend)
		ensureNilErr(rig.sendSwap_taker(true))
		ensureNilErr(rig.auditSwap_maker())
		ensureNilErr(rig.ackAudit_maker(true))
		matchInfo.db.takerSwap.coin.Coin.(*TCoin).setConfs(int64(takerSwapAsset.SwapConf))
		sendBlock(&takerSwapAsset.Backend.(*TUTXOBackend).TBackend)
		ensureNilErr(rig.redeem_maker(true))
		ensureNilErr(rig.ackRedemption_taker(true))
		ensureNilErr(rig.redeem_taker(true))
		ensureNilErr(rig.ackRedemption_maker(true))
	}
}

func TestSwaps(t *testing.T) {
	rig, cleanup := tNewTestRig(nil)
	defer cleanup()

	rig.auth.auditReq = make(chan struct{}, 1)
	rig.auth.redeemReceived = make(chan struct{}, 2)
	rig.auth.redemptionReq = make(chan struct{}, 2)

	for _, makerSell := range []bool{true, false} {
		sellStr := " buy"
		if makerSell {
			sellStr = " sell"
		}
		t.Run("perfect limit-limit match"+sellStr, func(t *testing.T) {
			rig.matches = tPerfectLimitLimit(uint64(1e8), uint64(1e8), makerSell)
			rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)
			testSwap(t, rig)
		})
		t.Run("perfect limit-market match"+sellStr, func(t *testing.T) {
			rig.matches = tPerfectLimitMarket(uint64(1e8), uint64(1e8), makerSell)
			rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)
			testSwap(t, rig)
		})
		t.Run("imperfect limit-market match"+sellStr, func(t *testing.T) {
			// only requirement is that maker val > taker val.
			rig.matches = tMarketPair(uint64(10e8), uint64(2e8), uint64(5e8), makerSell)
			rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)
			testSwap(t, rig)
		})
		t.Run("imperfect limit-limit match"+sellStr, func(t *testing.T) {
			rig.matches = tLimitPair(uint64(10e8), uint64(2e8), uint64(2e8), uint64(5e8), uint64(5e8), makerSell)
			rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)
			testSwap(t, rig)
		})
		for _, isMarket := range []bool{true, false} {
			marketStr := " limit"
			if isMarket {
				marketStr = " market"
			}
			t.Run("three match set"+sellStr+marketStr, func(t *testing.T) {
				matchQtys := []uint64{uint64(1e8), uint64(9e8), uint64(3e8)}
				rates := []uint64{uint64(10e8), uint64(11e8), uint64(12e8)}
				// one taker, 3 makers => 4 'match' requests
				rig.matches = tMultiMatchSet(matchQtys, rates, makerSell, isMarket)

				rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)
				testSwap(t, rig)
			})
		}
	}
}

func TestReAckRecordedSettlement(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	maker, taker := matchInfo.maker, matchInfo.taker
	matchID := matchInfo.matchID

	contract := randBytes(50)
	contractCoin := randBytes(36)
	redeemCoin := randBytes(36)
	secret := randBytes(32)

	rig.storage.mtx.Lock()
	rig.storage.swapDataByID = map[order.MatchID]*db.SwapDataFull{
		matchID: {
			Base:  ABCID,
			Quote: XYZID,
			MatchData: &db.MatchData{
				ID:        matchID,
				Maker:     matchInfo.makerOID,
				MakerAcct: maker.acct,
				Taker:     matchInfo.takerOID,
				TakerAcct: taker.acct,
			},
			SwapData: &db.SwapData{
				ContractACoinID: contractCoin,
				ContractA:       contract,
				RedeemBCoinID:   redeemCoin,
				RedeemASecret:   secret,
			},
		},
	}
	rig.storage.mtx.Unlock()

	sendInit := func(user *tUser, oid order.OrderID, coinID, contractBytes []byte) *msgjson.Error {
		t.Helper()
		req, err := msgjson.NewRequest(nextID(), msgjson.InitRoute, &msgjson.Init{
			OrderID:  oid[:],
			MatchID:  matchID[:],
			CoinID:   coinID,
			Contract: contractBytes,
		})
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		return rig.swapper.handleInit(user.acct, req)
	}
	sendRedeem := func(user *tUser, oid order.OrderID, coinID, secretBytes []byte) *msgjson.Error {
		t.Helper()
		req, err := msgjson.NewRequest(nextID(), msgjson.RedeemRoute, &msgjson.Redeem{
			OrderID: oid[:],
			MatchID: matchID[:],
			CoinID:  coinID,
			Secret:  secretBytes,
		})
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		return rig.swapper.handleRedeem(user.acct, req)
	}
	requireAck := func(t *testing.T, user *tUser) {
		t.Helper()
		msg, resp := rig.auth.popResp(user.acct)
		if msg == nil {
			t.Fatal("no re-ack response delivered")
		}
		if resp.Error != nil {
			t.Fatalf("re-ack response error: %v", resp.Error)
		}
		ack := new(msgjson.Acknowledgement)
		if err := json.Unmarshal(resp.Result, ack); err != nil {
			t.Fatalf("Acknowledgement decode: %v", err)
		}
		if !bytes.Equal(ack.MatchID, matchID[:]) {
			t.Fatalf("ack match ID = %x, want %v", []byte(ack.MatchID), matchID)
		}
		if len(ack.Sig) == 0 {
			t.Fatal("re-ack has no signature")
		}
	}

	t.Run("maker init resend re-acks", func(t *testing.T) {
		if rpcErr := sendInit(maker, matchInfo.makerOID, contractCoin, contract); rpcErr != nil {
			t.Fatalf("init resend error: %v", rpcErr)
		}
		requireAck(t, maker)
	})

	t.Run("init with different contract is not re-acked", func(t *testing.T) {
		rpcErr := sendInit(maker, matchInfo.makerOID, contractCoin, randBytes(50))
		if rpcErr == nil || rpcErr.Code != msgjson.RPCUnknownMatch {
			t.Fatalf("init error = %v, want RPCUnknownMatch", rpcErr)
		}
	})

	t.Run("wrong party is not re-acked", func(t *testing.T) {
		rpcErr := sendInit(taker, matchInfo.makerOID, contractCoin, contract)
		if rpcErr == nil || rpcErr.Code != msgjson.RPCUnknownMatch {
			t.Fatalf("init error = %v, want RPCUnknownMatch", rpcErr)
		}
	})

	t.Run("taker redeem resend after deletion re-acks", func(t *testing.T) {
		if rpcErr := sendRedeem(taker, matchInfo.takerOID, redeemCoin, secret); rpcErr != nil {
			t.Fatalf("redeem resend error: %v", rpcErr)
		}
		requireAck(t, taker)
	})

	t.Run("redeem with wrong secret is not re-acked", func(t *testing.T) {
		rpcErr := sendRedeem(taker, matchInfo.takerOID, redeemCoin, randBytes(32))
		if rpcErr == nil || rpcErr.Code != msgjson.RPCUnknownMatch {
			t.Fatalf("redeem error = %v, want RPCUnknownMatch", rpcErr)
		}
	})

	t.Run("lookup failure answers retryable", func(t *testing.T) {
		rig.storage.mtx.Lock()
		rig.storage.swapDataByIDErr = errors.New("db down")
		rig.storage.mtx.Unlock()
		defer func() {
			rig.storage.mtx.Lock()
			rig.storage.swapDataByIDErr = nil
			rig.storage.mtx.Unlock()
		}()
		rpcErr := sendRedeem(taker, matchInfo.takerOID, redeemCoin, secret)
		if rpcErr == nil || rpcErr.Code != msgjson.TryAgainLaterError {
			t.Fatalf("redeem error = %v, want TryAgainLaterError", rpcErr)
		}
	})

	t.Run("tracked NewlyMatched match gates the lookup", func(t *testing.T) {
		tracker := &matchTracker{Match: matchInfo.match}
		rig.swapper.matchMtx.Lock()
		rig.swapper.matches[matchID] = tracker
		rig.swapper.matchMtx.Unlock()
		defer func() {
			rig.swapper.matchMtx.Lock()
			delete(rig.swapper.matches, matchID)
			rig.swapper.matchMtx.Unlock()
		}()
		if rig.swapper.settlementMayBeRecorded(matchID) {
			t.Fatal("NewlyMatched tracked match must not consult the resend lookup")
		}
		tracker.Match.Status = order.MakerSwapCast
		defer func() { tracker.Match.Status = order.NewlyMatched }()
		if !rig.swapper.settlementMayBeRecorded(matchID) {
			t.Fatal("advanced tracked match must consult the resend lookup")
		}
	})
}

func TestInvalidFeeRate(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 1)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	rig.abcNode.invalidFeeRate = true

	// The error will be generated by the chainWaiter thread, so will need to
	// check the response.
	if err := rig.sendSwap_maker(false); err != nil {
		t.Fatal(err)
	}
	timeOutMempool()
	if err := rig.waitChans("invalid fee rate", rig.auth.swapReceived); err != nil {
		t.Fatalf("error waiting for response: %v", err)
	}
	// Should have an rpc error.
	msg, resp := rig.auth.popResp(matchInfo.maker.acct)
	if msg == nil {
		t.Fatalf("no response for missing tx after timeout")
	}
	if resp.Error == nil {
		t.Fatalf("no rpc error for erroneous maker swap %v", resp.Error)
	}
}

func TestTxWaiters(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.auditReq = make(chan struct{}, 1)
	rig.auth.redeemReceived = make(chan struct{}, 1)
	rig.auth.redemptionReq = make(chan struct{}, 1)
	rig.auth.swapReceived = make(chan struct{}, 1)

	ensureNilErr := makeEnsureNilErr(t)
	dummyError := fmt.Errorf("test error")

	sendBlock := func(node *TBackend) {
		node.bChan <- &asset.BlockUpdate{Err: nil}
	}

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	// Get the MatchNotifications that the swapper sent to the clients and check
	// the match notification length, content, IDs, etc.
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatal(err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatal(err)
	}

	// Set a non-latency error.
	rig.abcNode.setContractErr(dummyError)
	rig.sendSwap_maker(false)
	if err := rig.waitChans("maker contract error", rig.auth.swapReceived); err != nil {
		t.Fatalf("error waiting for maker swap error response: %v", err)
	}
	msg, _ := rig.auth.popResp(matchInfo.maker.acct)
	if msg == nil {
		t.Fatalf("no response for erroneous maker swap")
	}

	// Set an error for the maker's swap asset
	rig.abcNode.setContractErr(asset.CoinNotFoundError)
	// The error will be generated by the chainWaiter thread, so will need to
	// check the response.
	if err := rig.sendSwap_maker(false); err != nil {
		t.Fatal(err)
	}

	// Duplicate init request should get rejected.
	dupSwapReq := *matchInfo.db.makerSwap.req
	dupSwapReq.ID = nextID() // same content but new request ID
	rpcErr := rig.swapper.handleInit(matchInfo.maker.acct, &dupSwapReq)
	if rpcErr == nil {
		t.Fatal("should have rejected the duplicate init request")
	}
	if rpcErr.Code != msgjson.DuplicateRequestError {
		t.Errorf("duplicate init request expected code %d, got %d",
			msgjson.DuplicateRequestError, rpcErr.Code)
	}

	// Now timeout the initial search.
	timeOutMempool()
	if err := rig.waitChans("maker mempool timeout error", rig.auth.swapReceived); err != nil {
		t.Fatalf("error waiting for maker swap error response: %v", err)
	}
	// Should have an rpc error.
	msg, resp := rig.auth.popResp(matchInfo.maker.acct)
	if msg == nil {
		t.Fatalf("no response for missing tx after timeout")
	}
	if resp.Error == nil {
		t.Fatalf("no rpc error for erroneous maker swap")
	}

	rig.abcNode.setContractErr(nil)
	// Everything should work now.
	if err := rig.sendSwap_maker(true); err != nil {
		t.Fatal(err)
	}
	matchInfo.db.makerSwap.coin.Coin.(*TCoin).setConfs(int64(rig.abc.SwapConf))
	sendBlock(&rig.abc.Backend.(*TUTXOBackend).TBackend)
	if err := rig.auditSwap_taker(); err != nil {
		t.Fatal(err)
	}
	if err := rig.ackAudit_taker(true); err != nil {
		t.Fatal(err)
	}
	// Non-latency error.
	rig.xyzNode.setContractErr(dummyError)
	rig.sendSwap_taker(false)
	if err := rig.waitChans("taker contract error", rig.auth.swapReceived); err != nil {
		t.Fatalf("error waiting for taker swap error response: %v", err)
	}
	msg, _ = rig.auth.popResp(matchInfo.taker.acct)
	if msg == nil {
		t.Fatalf("no response for erroneous taker swap")
	}
	// For the taker swap, simulate latency.
	rig.xyzNode.setContractErr(asset.CoinNotFoundError)
	ensureNilErr(rig.sendSwap_taker(false))
	// Wait a tick
	tickMempool()
	// There should not be a response yet.
	msg, _ = rig.auth.popResp(matchInfo.taker.acct)
	if msg != nil {
		t.Fatalf("unexpected response for latent taker swap")
	}
	// Clear the error.
	rig.xyzNode.setContractErr(nil)
	tickMempool()
	if err := rig.waitChans("taker mempool timeout error", rig.auth.swapReceived); err != nil {
		t.Fatalf("error waiting for taker timeout error response: %v", err)
	}

	msg, resp = rig.auth.popResp(matchInfo.taker.acct)
	if msg == nil {
		t.Fatalf("no response for ok taker swap")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error for ok taker swap. code: %d, msg: %s",
			resp.Error.Code, resp.Error.Message)
	}
	matchInfo.db.takerSwap.coin.Coin.(*TCoin).setConfs(int64(rig.xyz.SwapConf))
	sendBlock(&rig.xyz.Backend.(*TUTXOBackend).TBackend)

	ensureNilErr(rig.auditSwap_maker())
	ensureNilErr(rig.ackAudit_maker(true))

	// Set a transaction error for the maker's redemption.
	rig.xyzNode.setRedemptionErr(asset.CoinNotFoundError)

	ensureNilErr(rig.redeem_maker(false))
	tickMempool()
	tickMempool()
	msg, _ = rig.auth.popResp(matchInfo.maker.acct)
	if msg != nil {
		t.Fatalf("unexpected response for latent maker redeem")
	}
	// Clear the error.
	rig.xyzNode.setRedemptionErr(nil)
	tickMempool()
	if err := rig.waitChans("maker redemption error", rig.auth.redeemReceived); err != nil {
		t.Fatalf("error waiting for taker timeout error response: %v", err)
	}
	msg, resp = rig.auth.popResp(matchInfo.maker.acct)
	if msg == nil {
		t.Fatalf("no response for erroneous maker redeem")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error for erroneous maker redeem. code: %d, msg: %s",
			resp.Error.Code, resp.Error.Message)
	}
	// Back to the taker, but let it timeout first, and then rebroadcast.
	// Get the tracker now, since it will be removed from the match dict if
	// everything goes right
	tracker := rig.getTracker()
	ensureNilErr(rig.ackRedemption_taker(true))
	rig.abcNode.setRedemptionErr(asset.CoinNotFoundError)
	ensureNilErr(rig.redeem_taker(false))
	timeOutMempool()
	if err := rig.waitChans("taker redemption timeout", rig.auth.redeemReceived); err != nil {
		t.Fatalf("error waiting for taker timeout error response: %v", err)
	}
	msg, _ = rig.auth.popResp(matchInfo.taker.acct)
	if msg == nil {
		t.Fatalf("no response for erroneous taker redeem")
	}
	rig.abcNode.setRedemptionErr(nil)
	ensureNilErr(rig.redeem_taker(true))
	tickMempool()
	tickMempool()
	ensureNilErr(rig.ackRedemption_maker(true))
	// Set the number of confirmations on the redemptions.
	matchInfo.db.makerRedeem.coin.setConfs(int64(rig.xyz.SwapConf))
	matchInfo.db.takerRedeem.coin.setConfs(int64(rig.abc.SwapConf))
	// send a block through for either chain to trigger a completion check.
	rig.xyzNode.bChan <- &asset.BlockUpdate{Err: nil}
	tickMempool()
	if tracker.Status != order.MatchComplete {
		t.Fatalf("match not marked as complete: %d", tracker.Status)
	}
	// Make sure that the tracker is removed from swappers match map.
	if rig.getTracker() != nil {
		t.Fatalf("matchTracker not removed from swapper's match map")
	}
}

func TestBroadcastTimeouts(t *testing.T) {
	rig, cleanup := tNewTestRig(nil)
	defer cleanup()

	rig.auth.newSuspend = make(chan struct{}, 1)
	rig.auth.auditReq = make(chan struct{}, 1)
	rig.auth.redemptionReq = make(chan struct{}, 1)
	// Buffered; Send is non-blocking if this fills.
	rig.auth.newNtfn = make(chan struct{}, 20)

	ensureNilErr := makeEnsureNilErr(t)
	sendBlock := func(node *TBackend) {
		node.bChan <- &asset.BlockUpdate{Err: nil}
	}

	ntfnWait := func(timeout time.Duration) {
		t.Helper()
		select {
		case <-rig.auth.newNtfn:
		case <-time.After(timeout):
			t.Fatalf("no notification received")
		}
	}

	checkRevokeMatch := func(user *tUser, i int) {
		t.Helper()
		rev := new(msgjson.RevokeMatch)
		err := rig.auth.getNtfn(user.acct, msgjson.RevokeMatchRoute, rev)
		if err != nil {
			t.Fatalf("failed to get revoke_match ntfn: %v", err)
		}
		if err = checkSigS256(rev, rig.auth.privkey.PubKey()); err != nil {
			t.Fatalf("incorrect server signature: %v", err)
		}
		if !bytes.Equal(rev.MatchID, rig.matchInfo.matchID[:]) {
			t.Fatalf("unexpected revocation match ID for %s at step %d. expected %s, got %s",
				user.lbl, i, rig.matchInfo.matchID, rev.MatchID)
		}

		// TODO: expect revoke order for at-fault user
	}

	// tryExpire waits for a BroadcastTimeout failure and then checks that a
	// revoke_match message is sent to both users. Planned inaction records the
	// reputation outcome, but does not unbook through Penalize.
	tryExpire := func(i, j int, step order.MatchStatus, jerk, victim *tUser, node *TBackend) bool {
		t.Helper()
		if i != j {
			return false
		}
		// Sending a block through should schedule an inaction check after duration
		// BroadcastTimeout.
		sendBlock(node)
		deadline := time.After(rig.swapper.bTimeout * 3)
		for !rig.auth.hasNtfn(jerk.acct, msgjson.RevokeMatchRoute) ||
			!rig.auth.hasNtfn(victim.acct, msgjson.RevokeMatchRoute) {
			select {
			case <-rig.auth.newNtfn:
			case <-deadline:
				t.Fatalf("no revoke_match notification")
			}
		}
		checkRevokeMatch(jerk, i)
		checkRevokeMatch(victim, i)
		return true
	}
	// Run a timeout test after every important step.
	for i := 0; i <= 3; i++ {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true) // same orders, different users
		matchInfo := set.matchInfos[0]
		rig.matchInfo = matchInfo
		rig.applyMatchesAndRequestAcks(t, set.matchSet)
		// Step through the negotiation process. No errors should be generated.
		// TODO: timeout each match ack, not block based inaction.

		ensureNilErr(rig.ackMatch_maker(true))
		ensureNilErr(rig.ackMatch_taker(true))

		// Drain counterparty_address notifications generated by the ack flow.
		ntfnWait(time.Second)
		ntfnWait(time.Second)

		// Timeout waiting for maker swap.
		if tryExpire(i, 0, order.NewlyMatched, matchInfo.maker, matchInfo.taker, &rig.abcNode.TBackend) {
			continue
		}

		ensureNilErr(rig.sendSwap_maker(true))

		// Pull the server's 'audit' request from the comms queue.
		ensureNilErr(rig.auditSwap_taker())

		// NOTE: timeout on the taker's audit ack response itself does not cause
		// a revocation. The taker not broadcasting their swap when maker's swap
		// reaches swapconf plus bTimeout is the trigger.
		ensureNilErr(rig.ackAudit_taker(true))

		// Maker's swap reaches swapConf.
		matchInfo.db.makerSwap.coin.Coin.(*TCoin).setConfs(int64(rig.abc.SwapConf))
		sendBlock(&rig.abcNode.TBackend) // tryConfirmSwap
		// With maker swap confirmed, inaction happens bTimeout after
		// swapConfirmed time.
		if tryExpire(i, 1, order.MakerSwapCast, matchInfo.taker, matchInfo.maker, &rig.xyzNode.TBackend) {
			continue
		}

		ensureNilErr(rig.sendSwap_taker(true))

		// Pull the server's 'audit' request from the comms queue
		ensureNilErr(rig.auditSwap_maker())

		// NOTE: timeout on the maker's audit ack response itself does not cause
		// a revocation. The maker not broadcasting their redeem when taker's
		// swap reaches swapconf plus bTimeout is the trigger.
		ensureNilErr(rig.ackAudit_maker(true))

		// Taker's swap reaches swapConf.
		matchInfo.db.takerSwap.coin.Coin.(*TCoin).setConfs(int64(rig.xyz.SwapConf))
		sendBlock(&rig.xyzNode.TBackend)
		// With taker swap confirmed, inaction happens bTimeout after
		// swapConfirmed time.
		if tryExpire(i, 2, order.TakerSwapCast, matchInfo.maker, matchInfo.taker, &rig.xyzNode.TBackend) {
			continue
		}

		ensureNilErr(rig.redeem_maker(true))

		// Pull the server's 'redemption' request from the comms queue
		ensureNilErr(rig.ackRedemption_taker(true))

		// Maker's redeem reaches swapConf. Not necessary for taker redeem.
		// matchInfo.db.makerRedeem.coin.setConfs(int64(rig.xyz.SwapConf))
		// sendBlock(rig.xyzNode)
		if tryExpire(i, 3, order.MakerRedeemed, matchInfo.taker, matchInfo.maker, &rig.abcNode.TBackend) {
			continue
		}

		// Next is redeem_taker... not a block-based inaction.

		return
	}
}
func TestSigErrors(t *testing.T) {
	dummyError := fmt.Errorf("test error")
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.auditReq = make(chan struct{}, 1)
	rig.auth.redemptionReq = make(chan struct{}, 1)
	rig.auth.redeemReceived = make(chan struct{}, 1)
	rig.auth.swapReceived = make(chan struct{}, 1)

	rig.applyMatchesAndRequestAcks(t, set.matchSet) // pushes a new req to m.reqs
	ensureNilErr := makeEnsureNilErr(t)

	// We need a way to restore the state of the queue after testing an auth error.
	var tReq *TRequest
	var msg *msgjson.Message
	apply := func(user *tUser) {
		if msg != nil {
			rig.auth.pushResp(user.acct, msg)
		}
		if tReq != nil {
			rig.auth.pushReq(user.acct, tReq)
		}
	}
	stash := func(user *tUser) {
		msg, _ = rig.auth.popResp(user.acct)
		tReq = rig.auth.popReq(user.acct)
		apply(user) // put them back now that we have a copy
	}
	testAction := func(stepFunc func(bool) error, user *tUser, failChans ...chan struct{}) {
		t.Helper()
		// First do it with an auth error.
		rig.auth.authErr = dummyError
		stash(user) // make a copy of this user's next req/resp pair
		// The error will be pulled from the auth manager.
		_ = stepFunc(false) // popReq => req.respFunc(..., tNewResponse()) => send error to client (m.resps)
		if err := rig.waitChans("testAction", failChans...); err != nil {
			t.Fatalf("testAction failChan wait error: %v", err)
		}
		// ensureSigErr makes sure that the specified user has a signature error
		// response from the swapper.
		ensureNilErr(rig.checkServerResponseFail(user, msgjson.SignatureError))
		// Again with no auth error to go to the next step.
		rig.auth.authErr = nil
		apply(user) // restore the initial live request
		ensureNilErr(stepFunc(true))
	}
	maker, taker := matchInfo.maker, matchInfo.taker
	// 1 live match, 2 pending client acks
	testAction(rig.ackMatch_maker, maker)
	testAction(rig.ackMatch_taker, taker)
	testAction(rig.sendSwap_maker, maker, rig.auth.swapReceived)
	ensureNilErr(rig.auditSwap_taker())
	testAction(rig.ackAudit_taker, taker)
	testAction(rig.sendSwap_taker, taker, rig.auth.swapReceived)
	ensureNilErr(rig.auditSwap_maker())
	testAction(rig.ackAudit_maker, maker)
	testAction(rig.redeem_maker, maker, rig.auth.redeemReceived)
	testAction(rig.ackRedemption_taker, taker)
	testAction(rig.redeem_taker, taker, rig.auth.redeemReceived)
	testAction(rig.ackRedemption_maker, maker)
}

func TestMalformedSwap(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 1)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)
	ensureNilErr := makeEnsureNilErr(t)

	ensureNilErr(rig.ackMatch_maker(true))
	ensureNilErr(rig.ackMatch_taker(true))

	ensureErr := func(tag string) {
		t.Helper()
		ensureNilErr(rig.sendSwap_maker(false))
		ensureNilErr(rig.waitChans(tag, rig.auth.swapReceived))
	}

	// Bad contract value
	tValSpoofer = 2
	ensureErr("bad maker contract")
	ensureNilErr(rig.checkServerResponseFail(matchInfo.maker, msgjson.ContractError))
	tValSpoofer = 1
	// Bad contract recipient
	tRecipientSpoofer = "2"
	ensureErr("bad recipient")
	ensureNilErr(rig.checkServerResponseFail(matchInfo.maker, msgjson.ContractError))
	tRecipientSpoofer = ""
	// Bad locktime
	tLockTimeSpoofer = time.Unix(1, 0)
	ensureErr("bad locktime")
	ensureNilErr(rig.checkServerResponseFail(matchInfo.maker, msgjson.ContractError))
	tLockTimeSpoofer = time.Time{}

	// Low fee, unconfirmed
	rig.abcNode.invalidFeeRate = true
	ensureErr("low fee")
	ensureNilErr(rig.checkServerResponseFail(matchInfo.maker, msgjson.ContractError))

	// Works with low fee, but now a confirmation.
	tConfsSpoofer = 1
	defer func() { tConfsSpoofer = 0 }()
	ensureNilErr(rig.sendSwap_maker(true))
}

func TestRetriesDuringSwap(t *testing.T) {
	rig, cleanup := tNewTestRig(nil)
	defer cleanup()

	ensureNilErr := makeEnsureNilErr(t)
	// retryUntilSuccess will retry action until success or timeout.
	retryUntilSuccess := func(action func() error, onSuccess, onFail func()) {
		startWaitingTime := time.Now()
		for {
			err := action()
			if err == nil {
				// We are done, finally got a retry attempt that worked.
				onSuccess()
				break
			}
			onFail()

			if time.Since(startWaitingTime) > 10*time.Second {
				t.Fatalf("timed out retrying, err: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	match := tPerfectLimitLimit(uint64(1e8), uint64(1e8), false)
	rig.matches = match
	rig.matchInfo = match.matchInfos[0]
	rig.auth.swapReceived = make(chan struct{}, 1)
	rig.auth.auditReq = make(chan struct{}, 1)
	rig.auth.redeemReceived = make(chan struct{}, 1)
	rig.auth.redemptionReq = make(chan struct{}, 1)
	rig.applyMatchesAndRequestAcks(t, rig.matches.matchSet)

	ensureNilErr(rig.ackMatch_maker(true))
	ensureNilErr(rig.ackMatch_taker(true))

	ensureNilErr(rig.sendSwap_maker(true))
	ensureNilErr(rig.auditSwap_taker())
	tracker := rig.getTracker()
	// We're "rewinding time" back NewlyMatched status to be able to retry sending
	// the same init request again.
	tracker.Status = order.NewlyMatched
	retryUntilSuccess(func() error {
		// We might get a couple of duplicate init request errors here, in that case
		// we simply retry request later.
		// We need to wait for swapSearching semaphore (it coordinates init request
		// handling) to clear, so our retry request should eventually work (it has
		// adequate fee and 0 confs).
		return rig.sendSwap_maker(false)
	}, func() {
		// On success, executes once.
		ensureNilErr(rig.ensureSwapStatus("server received our swap -> counterparty got audit request",
			order.MakerSwapCast, rig.auth.swapReceived, rig.auth.auditReq))
		ensureNilErr(rig.checkServerResponseSuccess(rig.matchInfo.maker))
		ensureNilErr(rig.auditSwap_taker())
	}, func() {
		// On every failure.
		ensureNilErr(rig.ensureSwapStatus("server received our swap",
			order.NewlyMatched, rig.auth.swapReceived))
		ensureNilErr(rig.checkServerResponseFail(rig.matchInfo.maker, msgjson.DuplicateRequestError))
	})

	ensureNilErr(rig.ackAudit_taker(true))
	ensureNilErr(rig.sendSwap_taker(true))
	ensureNilErr(rig.auditSwap_maker())
	tracker = rig.getTracker()
	// We're "rewinding time" back MakerSwapCast status to be able to retry sending
	// the same init request again.
	tracker.Status = order.MakerSwapCast
	retryUntilSuccess(func() error {
		// We might get a couple of duplicate init request errors here, in that case
		// we simply retry request later.
		// We need to wait for swapSearching semaphore (it coordinates init request
		// handling) to clear, so our retry request should eventually work (it has
		// adequate fee and 0 confs).
		return rig.sendSwap_taker(false)
	}, func() {
		// On success, executes once.
		ensureNilErr(rig.ensureSwapStatus("server received our swap -> counterparty got audit request",
			order.TakerSwapCast, rig.auth.swapReceived, rig.auth.auditReq))
		ensureNilErr(rig.checkServerResponseSuccess(rig.matchInfo.taker))
		ensureNilErr(rig.auditSwap_maker())
	}, func() {
		// On every failure.
		ensureNilErr(rig.ensureSwapStatus("server received our swap",
			order.MakerSwapCast, rig.auth.swapReceived))
		ensureNilErr(rig.checkServerResponseFail(rig.matchInfo.taker, msgjson.DuplicateRequestError))
	})

	ensureNilErr(rig.ackAudit_maker(true))

	ensureNilErr(rig.redeem_maker(true))
	ensureNilErr(rig.ackRedemption_taker(true))
	tracker = rig.getTracker()
	// We're "rewinding time" back TakerSwapCast status to be able to retry sending
	// the same redeem request again.
	tracker.Status = order.TakerSwapCast
	retryUntilSuccess(func() error {
		// We might get a couple of duplicate redeem request errors here, in that case
		// we simply retry request later.
		// We need to wait for redeemSearching semaphore (it coordinates redeem request
		// handling) to clear, so our retry request should eventually work.
		return rig.redeem_maker(false)
	}, func() {
		// On success, executes once.
		ensureNilErr(rig.ensureSwapStatus("server received our redeem -> counterparty got redemption request",
			order.MakerRedeemed, rig.auth.redeemReceived, rig.auth.redemptionReq))
		ensureNilErr(rig.checkServerResponseSuccess(rig.matchInfo.maker))
		ensureNilErr(rig.ackRedemption_taker(true))
	}, func() {
		// On every failure.
		ensureNilErr(rig.ensureSwapStatus("server received our redeem",
			order.TakerSwapCast, rig.auth.redeemReceived))
		ensureNilErr(rig.checkServerResponseFail(rig.matchInfo.maker, msgjson.DuplicateRequestError))
	})

	ensureNilErr(rig.redeem_taker(true))
	// Retry after success: match is gone.
	err := rig.redeem_taker(false)
	if err == nil {
		t.Fatalf("expected 2nd redeem request to fail after 1st one succeeded")
	}
	ensureNilErr(rig.waitChans("server received our redeem", rig.auth.redeemReceived))
	ensureNilErr(rig.checkServerResponseFail(rig.matchInfo.taker, msgjson.RPCUnknownMatch))

	tickMempool()
	tickMempool()
	ensureNilErr(rig.ackRedemption_maker(true)) // no-op; match already removed

	err = rig.redeem_taker(false)
	if err == nil {
		t.Fatalf("expected 2nd redeem request to fail after 1st one succeeded")
	}

	ensureNilErr(rig.checkServerResponseFail(rig.matchInfo.taker, msgjson.RPCUnknownMatch))
}

func TestBadParams(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()
	rig.applyMatchesAndRequestAcks(t, set.matchSet)
	swapper := rig.swapper
	match := rig.getTracker()
	user := matchInfo.maker
	acker := &messageAcker{
		user:    user.acct,
		match:   match,
		isMaker: true,
		isAudit: true,
	}
	ensureNilErr := makeEnsureNilErr(t)

	ackArr := make([]*msgjson.Acknowledgement, 0)
	matches := []*messageAcker{
		{match: rig.getTracker()},
	}

	encodedAckArray := func() json.RawMessage {
		b, _ := json.Marshal(ackArr)
		return json.RawMessage(b)
	}

	// Invalid result.
	msg, _ := msgjson.NewResponse(1, nil, nil)
	msg.Payload = json.RawMessage(`{"result":?}`)
	swapper.processAck(msg, acker)
	ensureNilErr(rig.checkServerResponseFail(user, msgjson.RPCParseError))
	swapper.processMatchAcks(user.acct, msg, []*messageAcker{})
	ensureNilErr(rig.checkServerResponseFail(user, msgjson.RPCParseError))

	msg, _ = msgjson.NewResponse(1, encodedAckArray(), nil)
	swapper.processMatchAcks(user.acct, msg, matches)
	ensureNilErr(rig.checkServerResponseFail(user, msgjson.AckCountError))
}

// TestAckMeshUnavailable verifies that an ack whose recorded event cannot be
// applied while the mesh is temporarily unavailable is answered with
// TryAgainLater instead of an internal server error.
func TestAckMeshUnavailable(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig := tNewUnstartedRig(matchInfo)
	if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
		t.Fatalf("TrackMatches: %v", err)
	}
	rig.swapper.SetMeshService(&tSwapMesh{err: fmt.Errorf("drain in progress: %w", mesh.ErrUnavailable)})
	ensureNilErr := makeEnsureNilErr(t)
	user := matchInfo.maker

	acker := &messageAcker{
		user:    user.acct,
		match:   rig.getTracker(),
		params:  &msgjson.Audit{},
		isMaker: true,
		isAudit: true,
	}
	ack := &msgjson.Acknowledgement{MatchID: matchInfo.matchID[:], Sig: user.sig}
	msg, err := msgjson.NewResponse(1, ack, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	rig.swapper.processAck(msg, acker)
	ensureNilErr(rig.checkServerResponseFail(user, msgjson.TryAgainLaterError))
}

func tMakerMatchAckRecord(matchInfo *tMatch) meshevents.MatchAckRecord {
	return meshevents.MatchAckRecord{
		MatchID: matchInfo.matchID,
		Base:    matchInfo.match.Maker.BaseAsset,
		Quote:   matchInfo.match.Maker.QuoteAsset,
		Maker:   true,
		Sig:     append(dex.Bytes(nil), matchInfo.maker.sig...),
		Address: matchInfo.makerPerMatchAddr,
	}
}

func tTakerMatchAckRecord(matchInfo *tMatch) meshevents.MatchAckRecord {
	return meshevents.MatchAckRecord{
		MatchID: matchInfo.matchID,
		Base:    matchInfo.match.Maker.BaseAsset,
		Quote:   matchInfo.match.Maker.QuoteAsset,
		Sig:     append(dex.Bytes(nil), matchInfo.taker.sig...),
		Address: matchInfo.takerPerMatchAddr,
	}
}

func tCancelMatchAckRecord(matchInfo *tMatch) meshevents.MatchAckRecord {
	return meshevents.MatchAckRecord{
		MatchID: matchInfo.matchID,
		Base:    matchInfo.match.Maker.BaseAsset,
		Quote:   matchInfo.match.Maker.QuoteAsset,
		Maker:   true,
		Cancel:  true,
		Sig:     append(dex.Bytes(nil), matchInfo.maker.sig...),
	}
}

func tMatchAcksRecordedUpdate(records []meshevents.MatchAckRecord) *db.MatchAcksRecordedUpdate {
	acks := make([]*db.MatchAck, 0, len(records))
	for _, record := range records {
		acks = append(acks, &db.MatchAck{
			MID: db.MarketMatchID{
				MatchID: record.MatchID,
				Base:    record.Base,
				Quote:   record.Quote,
			},
			Maker:   record.Maker,
			Cancel:  record.Cancel,
			Sig:     append([]byte(nil), record.Sig...),
			Address: record.Address,
		})
	}
	return &db.MatchAcksRecordedUpdate{Acks: acks}
}

func TestProcessMatchAcksEmitsEvent(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	tMesh := new(tSwapMesh)
	rig.swapper.SetMeshService(tMesh)
	rig.auth.authErr = errors.New("user is not locally connected")
	rig.auth.verifyErrSet = true

	req := rig.auth.popReq(matchInfo.maker.acct)
	if req == nil {
		t.Fatalf("no match request sent")
	}
	resp := tNewResponse(req.req.ID, tAckArrWithAddrs(matchInfo.maker, matchInfo.maker.matchIDs,
		map[order.MatchID]string{matchInfo.matchID: matchInfo.makerPerMatchAddr}))
	req.respFunc(nil, resp)

	if len(tMesh.events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(tMesh.events))
	}
	event, err := meshevents.DecodeMatchAcksRecordedEvent(tMesh.events[0].Payload)
	if err != nil {
		t.Fatalf("DecodeMatchAcksRecordedEvent error: %v", err)
	}
	if event.AckTime == 0 {
		t.Fatalf("empty event ack time")
	}
	wantRecords := []meshevents.MatchAckRecord{tMakerMatchAckRecord(matchInfo)}
	if !reflect.DeepEqual(event.Records, wantRecords) {
		t.Fatalf("wrong match ack event records.\nwant: %#v\n got: %#v", wantRecords, event.Records)
	}
	if msg, _ := rig.auth.popResp(matchInfo.maker.acct); msg != nil {
		t.Fatalf("unexpected error response: %v", msg)
	}
}

func TestApplySwapContractRecordedEvent(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]

	newSwapEvent := func(t *testing.T, rig *testRig, user *tUser, oid order.OrderID, recipient string, maker bool, status order.MatchStatus) *mesh.Event {
		t.Helper()
		swap := tNewSwap(matchInfo, oid, recipient, user)
		if isQuoteSwap(user, matchInfo.match) {
			rig.xyzNode.setContract(swap.coin, false)
		} else {
			rig.abcNode.setContract(swap.coin, false)
		}
		var init msgjson.Init
		if err := swap.req.Unmarshal(&init); err != nil {
			t.Fatalf("unmarshal init: %v", err)
		}
		event, err := mesh.NewEvent(&meshevents.SwapContractRecordedEvent{
			MatchID:     matchInfo.matchID,
			Base:        matchInfo.match.Maker.BaseAsset,
			Quote:       matchInfo.match.Maker.QuoteAsset,
			Maker:       maker,
			Status:      status,
			CoinID:      append(dex.Bytes(nil), init.CoinID...),
			CoinTxID:    swap.coin.TxID(),
			CoinString:  swap.coin.String(),
			Value:       swap.coin.Value(),
			FeeRate:     swap.coin.FeeRate(),
			Contract:    append(dex.Bytes(nil), init.Contract...),
			SwapAddress: swap.coin.SwapAddress,
			SecretHash:  append(dex.Bytes(nil), swap.coin.SecretHash...),
			LockTime:    swap.coin.LockTime.UnixMilli(),
			TxData:      append(dex.Bytes(nil), swap.coin.TxData...),
			SwapTime:    unixMsNow().UnixMilli(),
		})
		if err != nil {
			t.Fatalf("swap contract event: %v", err)
		}
		return event
	}

	tests := []struct {
		name      string
		user      *tUser
		oid       order.OrderID
		recipient string
		maker     bool
		status    order.MatchStatus
	}{
		{
			name:      "maker swap contract",
			user:      matchInfo.maker,
			oid:       matchInfo.makerOID,
			recipient: matchInfo.takerPerMatchAddr,
			maker:     true,
			status:    order.MakerSwapCast,
		},
		{
			name:      "taker swap contract",
			user:      matchInfo.taker,
			oid:       matchInfo.takerOID,
			recipient: matchInfo.makerPerMatchAddr,
			status:    order.TakerSwapCast,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, cleanup := tNewTestRig(matchInfo)
			defer cleanup()
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("ApplyMatches error: %v", err)
			}
			if !tt.maker {
				// The taker's contract lands after the maker's has advanced
				// the match.
				tracker := rig.getTracker()
				tracker.mtx.Lock()
				tracker.Status = order.MakerSwapCast
				tracker.mtx.Unlock()
			}

			event := newSwapEvent(t, rig, tt.user, tt.oid, tt.recipient, tt.maker, tt.status)
			recorded, err := meshevents.DecodeSwapContractRecordedEvent(event.Payload)
			if err != nil {
				t.Fatalf("decode swap contract event: %v", err)
			}
			_, err = rig.swapper.Events()[meshevents.EventKindSwapContractRecorded](&mesh.EventApplyContext{Context: context.Background()}, event)
			if err != nil {
				t.Fatalf("apply swap contract: %v", err)
			}

			tracker := rig.getTracker()
			contracts := storedSwapContracts(rig.storage)
			wantContract := &db.SwapContract{
				MID: db.MarketMatchID{
					MatchID: recorded.MatchID,
					Base:    recorded.Base,
					Quote:   recorded.Quote,
				},
				Maker:     recorded.Maker,
				Contract:  recorded.Contract,
				CoinID:    recorded.CoinID,
				Timestamp: recorded.SwapTime,
			}
			if !reflect.DeepEqual(contracts, []*db.SwapContract{wantContract}) {
				t.Fatalf("wrong stored swap contracts.\nwant: %#v\n got: %#v", []*db.SwapContract{wantContract}, contracts)
			}
			if tracker.Status != tt.status {
				t.Fatalf("wrong tracker status. want %v, got %v", tt.status, tracker.Status)
			}
			if tt.maker {
				if tracker.makerStatus.swap == nil {
					t.Fatalf("maker swap was not recorded")
				}
			} else if tracker.takerStatus.swap == nil {
				t.Fatalf("taker swap was not recorded")
			}
		})
	}
}
func TestApplyAuditAckRecordedEvent(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]

	tests := []struct {
		name  string
		maker bool
		sig   []byte
	}{
		{
			name:  "maker audit ack",
			maker: true,
			sig:   matchInfo.maker.sig,
		},
		{
			name: "taker audit ack",
			sig:  matchInfo.taker.sig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, cleanup := tNewTestRig(matchInfo)
			defer cleanup()
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("ApplyMatches error: %v", err)
			}

			tracker := rig.getTracker()
			event, err := newAuditAckRecordedEvent(tracker, tt.maker, tt.sig)
			if err != nil {
				t.Fatalf("audit ack event: %v", err)
			}
			recorded, err := meshevents.DecodeAuditAckRecordedEvent(event.Payload)
			if err != nil {
				t.Fatalf("decode audit ack event: %v", err)
			}
			_, err = rig.swapper.Events()[meshevents.EventKindAuditAckRecorded](&mesh.EventApplyContext{Context: context.Background()}, event)
			if err != nil {
				t.Fatalf("apply audit ack: %v", err)
			}

			acks := storedAuditAcks(rig.storage)
			wantAck := &db.AuditAck{
				MID: db.MarketMatchID{
					MatchID: recorded.MatchID,
					Base:    recorded.Base,
					Quote:   recorded.Quote,
				},
				Maker: recorded.Maker,
				Sig:   recorded.Sig,
			}
			if !reflect.DeepEqual(acks, []*db.AuditAck{wantAck}) {
				t.Fatalf("wrong stored audit acks.\nwant: %#v\n got: %#v", []*db.AuditAck{wantAck}, acks)
			}
			if tt.maker {
				if !bytes.Equal(tracker.Sigs.MakerAudit, tt.sig) {
					t.Fatalf("maker audit sig not recorded")
				}
			} else if !bytes.Equal(tracker.Sigs.TakerAudit, tt.sig) {
				t.Fatalf("taker audit sig not recorded")
			}
		})
	}
}

// seedRecordedContracts puts the match in the both-contracts-recorded state
// the swap_contract_recorded applies leave: fixture swaps registered with the
// backends, both swap statuses populated, and the match at TakerSwapCast.
func seedRecordedContracts(t *testing.T, rig *testRig, matchInfo *tMatch, stamp time.Time) {
	t.Helper()
	tracker := rig.getTracker()
	makerSwap := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.takerPerMatchAddr, matchInfo.maker)
	takerSwap := tNewSwap(matchInfo, matchInfo.takerOID, matchInfo.makerPerMatchAddr, matchInfo.taker)
	matchInfo.db.makerSwap = makerSwap
	matchInfo.db.takerSwap = takerSwap
	for _, side := range []struct {
		user *tUser
		swap *tSwap
	}{{matchInfo.maker, makerSwap}, {matchInfo.taker, takerSwap}} {
		if isQuoteSwap(side.user, matchInfo.match) {
			rig.xyzNode.setContract(side.swap.coin, false)
		} else {
			rig.abcNode.setContract(side.swap.coin, false)
		}
		var init msgjson.Init
		if err := side.swap.req.Unmarshal(&init); err != nil {
			t.Fatalf("unmarshal init: %v", err)
		}
		contract := *side.swap.coin
		contract.ContractData = append(dex.Bytes(nil), init.Contract...)
		status := tracker.takerStatus
		if side.user == matchInfo.maker {
			status = tracker.makerStatus
		}
		status.mtx.Lock()
		status.swap = &contract
		status.swapTime = stamp
		status.mtx.Unlock()
	}
	tracker.mtx.Lock()
	tracker.Status = order.TakerSwapCast
	tracker.mtx.Unlock()
}

// seedMakerRedemption advances the seeded match to the maker-redeemed state
// the maker's swap_redemption_recorded apply leaves.
func seedMakerRedemption(rig *testRig, matchInfo *tMatch, stamp time.Time) {
	tracker := rig.getTracker()
	tracker.makerStatus.mtx.Lock()
	tracker.makerStatus.redemption = &TCoin{id: randBytes(36)}
	tracker.makerStatus.redeemTime = stamp
	tracker.makerStatus.secret = encode.RandomBytes(32)
	tracker.makerStatus.mtx.Unlock()
	tracker.mtx.Lock()
	tracker.Status = order.MakerRedeemed
	tracker.mtx.Unlock()
}

func newTestRedemptionEvent(t *testing.T, rig *testRig, matchInfo *tMatch, user *tUser, oid order.OrderID, maker bool, status order.MatchStatus, stamp time.Time) (*mesh.Event, dex.Bytes, dex.Bytes) {
	t.Helper()
	redeem := tNewRedeem(matchInfo, oid, user)
	if isQuoteSwap(user, matchInfo.match) {
		rig.abcNode.setRedemption(redeem.coin, redeem.cpSwapCoin, false)
	} else {
		rig.xyzNode.setRedemption(redeem.coin, redeem.cpSwapCoin, false)
	}
	var params msgjson.Redeem
	if err := redeem.req.Unmarshal(&params); err != nil {
		t.Fatalf("unmarshal redeem: %v", err)
	}
	coinID := append(dex.Bytes(nil), params.CoinID...)
	var secret dex.Bytes
	if maker {
		secret = append(dex.Bytes(nil), params.Secret...)
	}
	event, err := mesh.NewEvent(&meshevents.SwapRedemptionRecordedEvent{
		MatchID:    matchInfo.matchID,
		Base:       matchInfo.match.Maker.BaseAsset,
		Quote:      matchInfo.match.Maker.QuoteAsset,
		Maker:      maker,
		Status:     status,
		CoinID:     coinID,
		Secret:     secret,
		RedeemTime: stamp.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("swap redemption event: %v", err)
	}
	return event, coinID, secret
}

func applyTestRedemption(t *testing.T, rig *testRig, matchInfo *tMatch, user *tUser, oid order.OrderID, maker bool, status order.MatchStatus, stamp time.Time) {
	t.Helper()
	event, _, _ := newTestRedemptionEvent(t, rig, matchInfo, user, oid, maker, status, stamp)
	if _, err := rig.swapper.Events()[meshevents.EventKindSwapRedemptionRecorded](&mesh.EventApplyContext{Context: context.Background()}, event); err != nil {
		t.Fatalf("apply setup redemption: %v", err)
	}
}

func storedSwapContracts(storage *TStorage) []*db.SwapContract {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]*db.SwapContract(nil), storage.swapContracts...)
}

func storedAuditAcks(storage *TStorage) []*db.AuditAck {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]*db.AuditAck(nil), storage.auditAcks...)
}

func storedRedemptions(storage *TStorage) []*db.SwapRedemption {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]*db.SwapRedemption(nil), storage.redemptions...)
}

func redemptionAckRecords(storage *TStorage) []*db.RedemptionAck {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]*db.RedemptionAck(nil), storage.redemptionAcks...)
}

func testMarketMatchID(matchInfo *tMatch) db.MarketMatchID {
	return db.MarketMatchID{
		MatchID: matchInfo.matchID,
		Base:    matchInfo.match.Maker.BaseAsset,
		Quote:   matchInfo.match.Maker.QuoteAsset,
	}
}

func TestApplySwapRedemptionRecordedEvent(t *testing.T) {
	storageErr := errors.New("storage error")
	redeemTime := time.UnixMilli(1670000000123).UTC()
	checkRedemption := func(t *testing.T, redemption *db.SwapRedemption, mid db.MarketMatchID, maker bool, coinID, secret dex.Bytes, stamp time.Time) {
		t.Helper()
		if redemption == nil ||
			redemption.MID != mid ||
			redemption.Maker != maker ||
			!bytes.Equal(redemption.CoinID, coinID) ||
			!bytes.Equal(redemption.Secret, secret) ||
			redemption.Timestamp != stamp.UnixMilli() {
			t.Fatalf("wrong redemption: %#v", redemption)
		}
	}

	tests := []struct {
		name                     string
		maker                    bool
		storageErr               bool
		skipMakerRedemptionSetup bool
		missingMatch             bool
		localStatus              order.MatchStatus
		validationErr            bool
		wantStatus               order.MatchStatus
		wantErr                  bool
	}{
		{
			name:       "maker redemption",
			maker:      true,
			wantStatus: order.MakerRedeemed,
		},
		{
			name:       "maker redemption storage error returns before memory update",
			maker:      true,
			storageErr: true,
			wantStatus: order.TakerSwapCast,
			wantErr:    true,
		},
		{
			name:       "taker redemption",
			wantStatus: order.MatchComplete,
		},
		{
			name:                     "taker redemption rejects wrong local status",
			skipMakerRedemptionSetup: true,
			validationErr:            true,
			wantStatus:               order.TakerSwapCast,
			wantErr:                  true,
		},
		{
			name:          "maker redemption rejects wrong local status",
			maker:         true,
			localStatus:   order.MakerRedeemed,
			validationErr: true,
			wantStatus:    order.MakerRedeemed,
			wantErr:       true,
		},
		{
			name:       "taker redemption storage error returns before memory update",
			storageErr: true,
			wantStatus: order.MakerRedeemed,
			wantErr:    true,
		},
		{
			name:          "maker redemption rejects unknown match",
			maker:         true,
			missingMatch:  true,
			validationErr: true,
			wantStatus:    order.TakerSwapCast,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start from a matched pair with both swap contracts recorded.
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig, cleanup := tNewTestRig(matchInfo)
			defer cleanup()
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("ApplyMatches error: %v", err)
			}
			seedRecordedContracts(t, rig, matchInfo, redeemTime)
			if !tt.maker && !tt.skipMakerRedemptionSetup {
				// A taker redemption lands only after the maker's has
				// advanced the match.
				seedMakerRedemption(rig, matchInfo, redeemTime)
			}

			// Capture the market-side completion projection without involving the market fixture.
			var swapDone []swapDoneCall
			rig.swapper.swapDone = func(ord order.Order, _ *order.Match, faulted bool) {
				swapDone = append(swapDone, swapDoneCall{ord.ID(), faulted})
			}
			if tt.localStatus != 0 {
				tracker := rig.getTracker()
				tracker.mtx.Lock()
				tracker.Status = tt.localStatus
				tracker.mtx.Unlock()
			}
			if tt.storageErr {
				rig.storage.mtx.Lock()
				rig.storage.applyRedemptionErr = storageErr
				rig.storage.mtx.Unlock()
			}

			// Register dedup entries so a successful taker apply frees them.
			dedupCoinID, dedupContract, dedupSecretHash := randBytes(36), randBytes(50), randBytes(32)
			rig.swapper.registerSwapContractDedup(matchInfo.matchID, dedupCoinID, dedupContract, dedupSecretHash, true)
			var otherMatchID order.MatchID
			otherMatchID[0] = ^matchInfo.matchID[0]
			if err := rig.swapper.checkSwapContractDedup(otherMatchID, dedupCoinID, dedupContract, dedupSecretHash, true); err == nil {
				t.Fatalf("dedup entries not registered")
			}

			// Capture tracker first: successful taker apply deletes it.
			tracker := rig.getTracker()
			if tt.missingMatch {
				rig.swapper.matchMtx.Lock()
				rig.swapper.deleteMatch(tracker)
				rig.swapper.matchMtx.Unlock()
			}
			user, oid, status := matchInfo.taker, matchInfo.takerOID, order.MatchComplete
			if tt.maker {
				user, oid, status = matchInfo.maker, matchInfo.makerOID, order.MakerRedeemed
			}
			event, coinID, secret := newTestRedemptionEvent(t, rig, matchInfo, user, oid, tt.maker, status, redeemTime)
			_, err := rig.swapper.Events()[meshevents.EventKindSwapRedemptionRecorded](&mesh.EventApplyContext{Context: context.Background()}, event)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected apply error")
				}
			} else if err != nil {
				t.Fatalf("apply redemption: %v", err)
			}

			// Taker apply deletes the match; other shapes leave it.
			wantDeleted := tt.missingMatch || (!tt.maker && !tt.wantErr)
			if gone := rig.getTracker() == nil; gone != wantDeleted {
				t.Fatalf("match deleted = %v, want %v", gone, wantDeleted)
			}
			dedupErr := rig.swapper.checkSwapContractDedup(otherMatchID, dedupCoinID, dedupContract, dedupSecretHash, true)
			if wantDeleted != (dedupErr == nil) {
				t.Fatalf("dedup freed = %v, want %v", dedupErr == nil, wantDeleted)
			}
			if tracker.Status != tt.wantStatus {
				t.Fatalf("wrong tracker status. want %v, got %v", tt.wantStatus, tracker.Status)
			}

			// The storage fixture should receive the exact event transaction update.
			acks := redemptionAckRecords(rig.storage)
			if len(acks) != 0 {
				t.Fatalf("unexpected redeem ack writes: %#v", acks)
			}
			redemptions := storedRedemptions(rig.storage)
			wantRedemptions := 1
			if tt.validationErr {
				wantRedemptions = 0
			}
			if len(redemptions) != wantRedemptions {
				t.Fatalf("wrong redemption count: %#v", redemptions)
			}
			mid := testMarketMatchID(matchInfo)
			if wantRedemptions != 0 {
				checkRedemption(t, redemptions[0], mid, tt.maker, coinID, secret, redeemTime)
			}

			// Successful storage apply updates the acting side's in-memory redemption state.
			statusRecord := tracker.takerStatus
			wantOID := matchInfo.takerOID
			if tt.maker {
				statusRecord = tracker.makerStatus
				wantOID = matchInfo.makerOID
			}
			statusRecord.mtx.RLock()
			recorded := statusRecord.redemption != nil && !statusRecord.redeemTime.IsZero()
			statusRecord.mtx.RUnlock()
			if !tt.wantErr && !recorded {
				t.Fatalf("redemption memory was not recorded")
			}
			if tt.wantErr && recorded {
				t.Fatalf("redemption memory recorded despite storage error")
			}
			// Market completion is projected only after the DB event transaction succeeds.
			wantSwapDone := []swapDoneCall{{wantOID, false}}
			if tt.wantErr {
				wantSwapDone = nil
			}
			if !reflect.DeepEqual(swapDone, wantSwapDone) {
				t.Fatalf("wrong swapDone calls.\nwant: %#v\n got: %#v",
					wantSwapDone, swapDone)
			}
		})
	}
}
func TestApplyRedemptionAckRecordedEvent(t *testing.T) {
	storageErr := errors.New("storage error")

	tests := []struct {
		name            string
		maker           bool
		storageErr      bool
		wantErr         bool
		missingMatch    bool
		wantStoredAck   bool
		wantDeleted     bool
		wantMakerAckSig bool
	}{
		{
			name:          "maker redemption ack after match removal",
			maker:         true,
			missingMatch:  true,
			wantStoredAck: true,
			wantDeleted:   true,
		},
		{
			// Defensive path: live match should still be deleted.
			name:            "maker redemption ack with live match",
			maker:           true,
			wantStoredAck:   true,
			wantDeleted:     true,
			wantMakerAckSig: true,
		},
		{
			name:          "taker redemption ack",
			wantStoredAck: true,
		},
		{
			// Benign race: the taker's ack of the maker's redemption can
			// arrive after the taker's own redeem deleted the match.
			name:          "taker redemption ack after match removal",
			missingMatch:  true,
			wantStoredAck: true,
			wantDeleted:   true,
		},
		{
			name:       "taker redemption ack storage error returns error before memory update",
			storageErr: true,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("ApplyMatches error: %v", err)
			}
			if tt.storageErr {
				rig.storage.mtx.Lock()
				rig.storage.applyRedemptionAckErr = storageErr
				rig.storage.mtx.Unlock()
			}

			tracker := rig.getTracker()
			eventTracker := tracker
			sig := matchInfo.taker.sig
			if tt.maker {
				sig = matchInfo.maker.sig
			}
			eventSig := append(dex.Bytes(nil), sig...)
			wantAck := &db.RedemptionAck{
				MID:   testMarketMatchID(matchInfo),
				Maker: tt.maker,
				Sig:   eventSig,
			}
			event, err := newRedemptionAckRecordedEvent(tracker, tt.maker, sig)
			if err != nil {
				t.Fatalf("redemption ack event: %v", err)
			}
			if tt.missingMatch {
				rig.swapper.matchMtx.Lock()
				rig.swapper.deleteMatch(tracker)
				rig.swapper.matchMtx.Unlock()
			}

			_, err = rig.swapper.Events()[meshevents.EventKindRedemptionAckRecorded](&mesh.EventApplyContext{Context: context.Background()}, event)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected apply error")
				}
			} else if err != nil {
				t.Fatalf("apply redemption ack: %v", err)
			}

			tracker = rig.getTracker()
			if tt.wantDeleted {
				if tracker != nil {
					t.Fatalf("expected match deletion")
				}
			} else if tracker == nil {
				t.Fatalf("unexpected match deletion")
			}

			acks := redemptionAckRecords(rig.storage)
			wantAcks := []*db.RedemptionAck(nil)
			if tt.wantStoredAck {
				wantAcks = []*db.RedemptionAck{wantAck}
			}
			if !reflect.DeepEqual(acks, wantAcks) {
				t.Fatalf("wrong stored redemption acks.\nwant: %#v\n got: %#v", wantAcks, acks)
			}

			if !tt.maker && !tt.wantErr && !tt.wantDeleted && !bytes.Equal(tracker.Sigs.TakerRedeem, matchInfo.taker.sig) {
				t.Fatalf("taker redeem ack sig not recorded")
			}
			if tt.wantErr && tracker != nil && len(tracker.Sigs.TakerRedeem) > 0 {
				t.Fatalf("taker redeem ack sig recorded despite storage error")
			}
			if tt.wantMakerAckSig && !bytes.Equal(eventTracker.Sigs.MakerRedeem, matchInfo.maker.sig) {
				t.Fatalf("maker redeem ack sig not recorded")
			}
		})
	}
}

func TestApplyMatchAcksRecordedEvent(t *testing.T) {
	storageErr := errors.New("storage error")
	ackTime := time.UnixMilli(1670000000123).UTC()

	applyEvent := func(t *testing.T, rig *testRig, records []meshevents.MatchAckRecord, eventTime time.Time) error {
		t.Helper()

		event, err := newMatchAcksRecordedEvent(eventTime, records)
		if err != nil {
			t.Fatalf("newMatchAcksRecordedEvent error: %v", err)
		}
		applier := rig.swapper.Events()[meshevents.EventKindMatchAcksRecorded]
		if applier == nil {
			t.Fatalf("missing %q event applier", meshevents.EventKindMatchAcksRecorded)
		}
		_, err = applier(&mesh.EventApplyContext{Context: context.Background()}, event)
		return err
	}

	requireStorageUpdates := func(t *testing.T, rig *testRig, wantRecords ...[]meshevents.MatchAckRecord) {
		t.Helper()

		updates := matchAcksRecordedUpdates(rig.storage)
		if len(updates) != len(wantRecords) {
			t.Fatalf("storage updates count = %d, want %d", len(updates), len(wantRecords))
		}
		for i, records := range wantRecords {
			wantUpdate := tMatchAcksRecordedUpdate(records)
			if !reflect.DeepEqual(updates[i], wantUpdate) {
				t.Fatalf("storage update %d wrong. want %+v, got %+v", i, wantUpdate, updates[i])
			}
		}
	}

	trackerFor := func(rig *testRig, matchInfo *tMatch) *matchTracker {
		rig.swapper.matchMtx.Lock()
		defer rig.swapper.matchMtx.Unlock()
		return rig.swapper.matches[matchInfo.matchID]
	}

	requireAckState := func(t *testing.T, tracker *matchTracker, wantMaker *tUser, wantMakerAddr string, wantTaker *tUser, wantTakerAddr string) {
		t.Helper()
		if tracker == nil {
			t.Fatalf("nil match tracker")
		}

		tracker.mtx.RLock()
		gotMakerSig := append([]byte(nil), tracker.Sigs.MakerMatch...)
		gotTakerSig := append([]byte(nil), tracker.Sigs.TakerMatch...)
		gotMakerAddr := tracker.makerSwapAddr
		gotTakerAddr := tracker.takerSwapAddr
		tracker.mtx.RUnlock()

		if wantMaker != nil {
			if !bytes.Equal(gotMakerSig, wantMaker.sig) || gotMakerAddr != wantMakerAddr {
				t.Fatalf("maker ack not applied. sig=%x addr=%q", gotMakerSig, gotMakerAddr)
			}
		} else if len(gotMakerSig) != 0 || gotMakerAddr != "" {
			t.Fatalf("unexpected maker ack state. sig=%x addr=%q", gotMakerSig, gotMakerAddr)
		}

		if wantTaker != nil {
			if !bytes.Equal(gotTakerSig, wantTaker.sig) || gotTakerAddr != wantTakerAddr {
				t.Fatalf("taker ack not applied. sig=%x addr=%q", gotTakerSig, gotTakerAddr)
			}
		} else if len(gotTakerSig) != 0 || gotTakerAddr != "" {
			t.Fatalf("unexpected taker ack state. sig=%x addr=%q", gotTakerSig, gotTakerAddr)
		}
	}

	t.Run("maker ack", func(t *testing.T) {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
		matchInfo := set.matchInfos[0]
		rig, cleanup := tNewTestRig(matchInfo)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
			t.Fatalf("ApplyMatches error: %v", err)
		}
		records := []meshevents.MatchAckRecord{tMakerMatchAckRecord(matchInfo)}
		if err := applyEvent(t, rig, records, ackTime); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		requireStorageUpdates(t, rig, records)
		requireAckState(t, trackerFor(rig, matchInfo), matchInfo.maker, matchInfo.makerPerMatchAddr, nil, "")
		if got := notificationCount(rig.auth, msgjson.CounterPartyAddressRoute); got != 0 {
			t.Fatalf("counterparty address notifications = %d, want 0", got)
		}
	})

	t.Run("same user acks different matches", func(t *testing.T) {
		const qty, rate = uint64(1e8), uint64(1e8)
		sharedUser := tNewUser("shared")
		makerCounterparty := tNewUser("maker-counterparty")
		takerCounterparty := tNewUser("taker-counterparty")

		makerOrder := makeLimitOrder(qty, rate, sharedUser, true)
		takerOrder := makeLimitOrder(qty, rate, makerCounterparty, false)
		makerSideMatch := tMatchInfo(sharedUser, makerCounterparty, qty, rate, makerOrder, takerOrder)
		makerSideSet := new(tMatchSet).add(makerSideMatch)

		makerOrder = makeLimitOrder(qty, rate, takerCounterparty, true)
		takerOrder = makeLimitOrder(qty, rate, sharedUser, false)
		takerSideMatch := tMatchInfo(takerCounterparty, sharedUser, qty, rate, makerOrder, takerOrder)
		takerSideSet := new(tMatchSet).add(takerSideMatch)

		rig, cleanup := tNewTestRig(makerSideMatch)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{makerSideSet.matchSet, takerSideSet.matchSet}); err != nil {
			t.Fatalf("ApplyMatches error: %v", err)
		}
		records := []meshevents.MatchAckRecord{
			tMakerMatchAckRecord(makerSideMatch),
			tTakerMatchAckRecord(takerSideMatch),
		}
		if err := applyEvent(t, rig, records, ackTime); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		requireStorageUpdates(t, rig, records)
		requireAckState(t, trackerFor(rig, makerSideMatch), sharedUser, makerSideMatch.makerPerMatchAddr, nil, "")
		requireAckState(t, trackerFor(rig, takerSideMatch), nil, "", sharedUser, takerSideMatch.takerPerMatchAddr)
		if got := notificationCount(rig.auth, msgjson.CounterPartyAddressRoute); got != 0 {
			t.Fatalf("counterparty address notifications = %d, want 0", got)
		}
	})

	t.Run("counterparty addresses sent after second side ack", func(t *testing.T) {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
		matchInfo := set.matchInfos[0]
		rig, cleanup := tNewTestRig(matchInfo)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
			t.Fatalf("ApplyMatches error: %v", err)
		}
		// The maker's ack is already recorded, as its earlier apply left it.
		tracker := trackerFor(rig, matchInfo)
		tracker.mtx.Lock()
		tracker.Sigs.MakerMatch = matchInfo.maker.sig
		tracker.makerSwapAddr = matchInfo.makerPerMatchAddr
		tracker.mtx.Unlock()

		takerRecords := []meshevents.MatchAckRecord{tTakerMatchAckRecord(matchInfo)}
		secondAckTime := ackTime.Add(time.Second)
		if err := applyEvent(t, rig, takerRecords, secondAckTime); err != nil {
			t.Fatalf("taker event error: %v", err)
		}

		requireStorageUpdates(t, rig, takerRecords)
		requireAckState(t, tracker, matchInfo.maker, matchInfo.makerPerMatchAddr, matchInfo.taker, matchInfo.takerPerMatchAddr)

		tracker.mtx.RLock()
		gotSent := tracker.counterPartyAddrsSent
		gotTime := tracker.time
		tracker.mtx.RUnlock()
		if !gotSent {
			t.Fatalf("counterPartyAddrsSent = false, want true")
		}
		if !gotTime.Equal(secondAckTime) {
			t.Fatalf("match time = %v, want %v", gotTime, secondAckTime)
		}
		if got := notificationCount(rig.auth, msgjson.CounterPartyAddressRoute); got != 2 {
			t.Fatalf("counterparty address notifications = %d, want 2", got)
		}
	})

	t.Run("cancel ack", func(t *testing.T) {
		set := tCancelPair()
		matchInfo := set.matchInfos[0]
		rig, cleanup := tNewTestRig(matchInfo)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
			t.Fatalf("ApplyMatches error: %v", err)
		}
		records := []meshevents.MatchAckRecord{tCancelMatchAckRecord(matchInfo)}
		if err := applyEvent(t, rig, records, ackTime); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		requireStorageUpdates(t, rig, records)
		if tracker := trackerFor(rig, matchInfo); tracker != nil {
			t.Fatalf("cancel match unexpectedly has a tracker")
		}
	})

	t.Run("divergent re-ack apply keeps recorded address", func(t *testing.T) {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
		matchInfo := set.matchInfos[0]
		rig, cleanup := tNewTestRig(matchInfo)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
			t.Fatalf("TrackMatches error: %v", err)
		}
		tracker := trackerFor(rig, matchInfo)
		tracker.mtx.Lock()
		tracker.takerSwapAddr = matchInfo.takerPerMatchAddr
		tracker.mtx.Unlock()

		// Divergent apply (e.g. old event log) must not displace the address.
		rig.swapper.applyMatchAckUpdates(ackTime, []matchAckApply{{
			record: meshevents.MatchAckRecord{
				MatchID: matchInfo.matchID,
				Base:    tracker.Maker.BaseAsset,
				Quote:   tracker.Maker.QuoteAsset,
				Maker:   false,
				Sig:     matchInfo.taker.sig,
				Address: "legacy-divergent-addr",
			},
			match: tracker,
		}})
		tracker.mtx.RLock()
		takerAddr := tracker.takerSwapAddr
		tracker.mtx.RUnlock()
		if takerAddr != matchInfo.takerPerMatchAddr {
			t.Fatalf("divergent apply displaced taker addr: got %q, want %q",
				takerAddr, matchInfo.takerPerMatchAddr)
		}
	})

	t.Run("storage error", func(t *testing.T) {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
		matchInfo := set.matchInfos[0]
		rig, cleanup := tNewTestRig(matchInfo)
		defer cleanup()

		if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
			t.Fatalf("ApplyMatches error: %v", err)
		}
		rig.storage.applyMatchAcksRecordedErr = storageErr
		records := []meshevents.MatchAckRecord{tMakerMatchAckRecord(matchInfo)}
		if err := applyEvent(t, rig, records, ackTime); err == nil {
			t.Fatalf("expected storage error")
		}

		requireStorageUpdates(t, rig, records)
		requireAckState(t, trackerFor(rig, matchInfo), nil, "", nil, "")
	})
}
func TestCancel(t *testing.T) {
	set := tCancelPair()
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()
	rig.applyMatchesAndRequestAcks(t, set.matchSet)
	// There should be no matchTracker
	if rig.getTracker() != nil {
		t.Fatalf("found matchTracker for a cancellation")
	}
	// The user should have two match requests.
	user := matchInfo.maker
	req := rig.auth.popReq(user.acct)
	if req == nil {
		t.Fatalf("no match request sent")
	}
	matchNotes := make([]*msgjson.Match, 0)
	err := json.Unmarshal(req.req.Payload, &matchNotes)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(matchNotes) != 2 {
		t.Fatalf("expected 2 match notification, got %d", len(matchNotes))
	}
	for _, match := range matchNotes {
		if err = checkSigS256(match, rig.auth.privkey.PubKey()); err != nil {
			t.Fatalf("incorrect server signature: %v", err)
		}
	}
	makerNote, takerNote := matchNotes[0], matchNotes[1]
	if makerNote.OrderID.String() != matchInfo.makerOID.String() {
		t.Fatalf("expected maker ID %s, got %s", matchInfo.makerOID, makerNote.OrderID)
	}
	if takerNote.OrderID.String() != matchInfo.takerOID.String() {
		t.Fatalf("expected taker ID %s, got %s", matchInfo.takerOID, takerNote.OrderID)
	}
	if makerNote.MatchID.String() != takerNote.MatchID.String() {
		t.Fatalf("match ID mismatch. %s != %s", makerNote.MatchID, takerNote.MatchID)
	}

	// Ack both notifications. Cancel matches are never swap-tracked, but the
	// ack sigs must still be recorded.
	resp := tNewResponse(req.req.ID,
		tAckArrWithAddrs(user, []order.MatchID{matchInfo.matchID, matchInfo.matchID}, nil))
	req.respFunc(nil, resp)
	if msg, r := rig.auth.popResp(user.acct); msg != nil {
		t.Fatalf("unexpected error response to cancel acks: %+v", r.Error)
	}
	updates := matchAcksRecordedUpdates(rig.storage)
	if len(updates) != 1 {
		t.Fatalf("expected 1 match acks storage update, got %d", len(updates))
	}
	acks := updates[0].Acks
	if len(acks) != 2 {
		t.Fatalf("expected 2 recorded acks, got %d", len(acks))
	}
	for i, wantMaker := range []bool{true, false} {
		ack := acks[i]
		if !ack.Cancel || ack.Maker != wantMaker {
			t.Fatalf("ack %d: Cancel = %v, Maker = %v, want true, %v", i, ack.Cancel, ack.Maker, wantMaker)
		}
		if !bytes.Equal(ack.Sig, user.sig) {
			t.Fatalf("ack %d: wrong sig", i)
		}
		if ack.Address != "" {
			t.Fatalf("ack %d: unexpected address %q", i, ack.Address)
		}
	}
	if rig.getTracker() != nil {
		t.Fatalf("cancel match gained a tracker after acks")
	}
}

// TestMatchAcksMixedTradeCancel acks a batch holding both a trade match and a
// cancel match for the same user, and verifies that the cancel entries do not
// poison the trade acks.
func TestMatchAcksMixedTradeCancel(t *testing.T) {
	qty, rate := uint64(1e8), uint64(1e8)
	user, taker := tNewUser("user"), tNewUser("taker")

	makerOrder, takerOrder := limitLimitPair(qty, qty, rate, rate, user, taker, true)
	tradeSet := new(tMatchSet).add(tMatchInfo(user, taker, qty, rate, makerOrder, takerOrder))
	tradeInfo := tradeSet.matchInfos[0]

	canceledOrder := makeLimitOrder(qty, rate, user, true)
	cancelSet := new(tMatchSet).add(tMatchInfo(user, user, qty, rate,
		canceledOrder, makeCancelOrder(canceledOrder, user)))
	cancelInfo := cancelSet.matchInfos[0]

	rig, cleanup := tNewTestRig(tradeInfo)
	defer cleanup()
	rig.applyMatchesAndRequestAcks(t, tradeSet.matchSet, cancelSet.matchSet)

	req := rig.auth.popReq(user.acct)
	if req == nil {
		t.Fatalf("no match request sent")
	}
	var notes []*msgjson.Match
	if err := json.Unmarshal(req.req.Payload, &notes); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(notes) != 3 {
		t.Fatalf("expected 3 match notifications, got %d", len(notes))
	}

	// Ack in request order: trade maker, then the cancel's maker and taker.
	ackArr := []msgjson.Acknowledgement{
		makeAck(tradeInfo.matchID, user.sig, tradeInfo.makerPerMatchAddr),
		makeAck(cancelInfo.matchID, user.sig, ""),
		makeAck(cancelInfo.matchID, user.sig, ""),
	}
	b, _ := json.Marshal(ackArr)
	req.respFunc(nil, tNewResponse(req.req.ID, b))
	if msg, r := rig.auth.popResp(user.acct); msg != nil {
		t.Fatalf("unexpected error response to mixed acks: %+v", r.Error)
	}

	tracker := rig.getTracker()
	tracker.mtx.RLock()
	makerSig := tracker.Sigs.MakerMatch
	makerAddr := tracker.makerSwapAddr
	tracker.mtx.RUnlock()
	if !bytes.Equal(makerSig, user.sig) {
		t.Fatalf("trade maker match sig not recorded")
	}
	if makerAddr != tradeInfo.makerPerMatchAddr {
		t.Fatalf("trade maker swap address = %q, want %q", makerAddr, tradeInfo.makerPerMatchAddr)
	}

	updates := matchAcksRecordedUpdates(rig.storage)
	if len(updates) != 1 {
		t.Fatalf("expected 1 storage update, got %d", len(updates))
	}
	var trades, cancels int
	for _, ack := range updates[0].Acks {
		if ack.Cancel {
			cancels++
		} else {
			trades++
		}
	}
	if trades != 1 || cancels != 2 {
		t.Fatalf("recorded %d trade / %d cancel acks, want 1 / 2", trades, cancels)
	}
}

func TestAccountTracking(t *testing.T) {
	const lotSize uint64 = 1e8
	const rate = 2e10
	const lots = 5
	const qty = lotSize * lots
	makerAddr := "maker"
	takerAddr := "taker"

	trackedSet := tPerfectLimitLimit(qty, rate, true)
	matchInfo := trackedSet.matchInfos[0]

	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	var baseAsset, quoteAsset uint32 = ACCTID, XYZID

	addOrder := func(matchSet *order.MatchSet, makerSell bool) {
		maker, taker := matchSet.Makers[0], matchSet.Taker
		taker.Prefix().BaseAsset = baseAsset
		maker.BaseAsset = baseAsset
		taker.Prefix().QuoteAsset = quoteAsset
		maker.QuoteAsset = quoteAsset
		maker.Coins = []order.CoinID{[]byte(makerAddr)}
		taker.Trade().Coins = []order.CoinID{[]byte(takerAddr)}
		taker.Trade().Address = takerAddr
		maker.Address = makerAddr
		if makerSell {
			taker.Trade().Sell = false
			maker.Sell = true
		} else {
			taker.Trade().Sell = true
			maker.Sell = false
		}
		rig.applyMatchesAndRequestAcks(t, matchSet)
	}

	checkStats := func(addr string, expQty, expSwaps uint64, expRedeems int) {
		t.Helper()
		q, s, r := rig.swapper.AccountStats(addr, ACCTID)
		if q != expQty {
			t.Fatalf("wrong quantity. wanted %d, got %d", expQty, q)
		}
		if s != expSwaps {
			t.Fatalf("wrong swaps. wanted %d, got %d", expSwaps, s)
		}
		if r != expRedeems {
			t.Fatalf("wrong redeems. wanted %d, got %d", expRedeems, r)
		}
	}

	addOrder(trackedSet.matchSet, true)
	checkStats(makerAddr, qty, 1, 0)
	checkStats(takerAddr, 0, 0, 1)

	tracker := rig.getTracker()
	tracker.Status = order.MakerSwapCast
	checkStats(makerAddr, 0, 0, 0)
	checkStats(takerAddr, 0, 0, 1)

	tracker.Status = order.MatchComplete
	checkStats(takerAddr, 0, 0, 0)

	rig.swapper.matchMtx.Lock()
	rig.swapper.deleteMatch(tracker)
	rig.swapper.matchMtx.Unlock()
	if len(rig.swapper.acctMatches[ACCTID]) > 0 {
		t.Fatalf("account tracking not deleted for removed match")
	}

	// Do 3 maker buys, 2 maker sells, and check the numbers.
	for i := 0; i < 5; i++ {
		set := tPerfectLimitLimit(qty, rate, i > 2)
		addOrder(set.matchSet, i > 2)
	}

	checkStats(makerAddr, qty*2, 2, 3)
	checkStats(takerAddr, qty*3, 3, 2)

	quoteAsset, baseAsset = baseAsset, quoteAsset
	set := tPerfectLimitLimit(qty, rate, false)

	addOrder(set.matchSet, false)
	checkStats(makerAddr, qty*2+calc.BaseToQuote(rate, qty), 3, 3)
	checkStats(takerAddr, qty*3, 3, 3)
}

func TestPerMatchSwapAddresses(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.newNtfn = make(chan struct{}, 4)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	tracker := rig.getTracker()

	// Maker acks with per-match address (included automatically via ack flow).
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("maker ack: %v", err)
	}

	tracker.mtx.RLock()
	if tracker.makerSwapAddr != matchInfo.makerPerMatchAddr {
		t.Fatalf("expected maker swap addr %q, got %q", matchInfo.makerPerMatchAddr, tracker.makerSwapAddr)
	}
	// Taker hasn't acked yet, so no counterparty addrs should be sent.
	if tracker.counterPartyAddrsSent {
		t.Fatalf("counterPartyAddrsSent set before taker ack")
	}
	tracker.mtx.RUnlock()

	// Taker acks with per-match address.
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("taker ack: %v", err)
	}

	tracker.mtx.RLock()
	if tracker.takerSwapAddr != matchInfo.takerPerMatchAddr {
		t.Fatalf("expected taker swap addr %q, got %q", matchInfo.takerPerMatchAddr, tracker.takerSwapAddr)
	}
	if !tracker.counterPartyAddrsSent {
		t.Fatalf("counterPartyAddrsSent not set after both acks")
	}
	tracker.mtx.RUnlock()

	// Both acked. Check that counterparty_address notifications were sent.
	// Two notifications: one to maker (with taker's addr) and one to taker
	// (with maker's addr).
	select {
	case <-rig.auth.newNtfn:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first counterparty_address notification")
	}
	select {
	case <-rig.auth.newNtfn:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second counterparty_address notification")
	}

	// Check the notification contents.
	var makerCPA msgjson.CounterPartyAddress
	err := rig.auth.getNtfn(matchInfo.maker.acct, msgjson.CounterPartyAddressRoute, &makerCPA)
	if err != nil {
		t.Fatalf("maker counterparty_address notification error: %v", err)
	}
	if makerCPA.Address != matchInfo.takerPerMatchAddr {
		t.Fatalf("maker received counterparty addr %q, expected %q", makerCPA.Address, matchInfo.takerPerMatchAddr)
	}

	var takerCPA msgjson.CounterPartyAddress
	err = rig.auth.getNtfn(matchInfo.taker.acct, msgjson.CounterPartyAddressRoute, &takerCPA)
	if err != nil {
		t.Fatalf("taker counterparty_address notification error: %v", err)
	}
	if takerCPA.Address != matchInfo.makerPerMatchAddr {
		t.Fatalf("taker received counterparty addr %q, expected %q", takerCPA.Address, matchInfo.makerPerMatchAddr)
	}
}

func TestPerMatchAddressInProcessInit(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 1)
	rig.auth.auditReq = make(chan struct{}, 1)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	// Ack both sides. Per-match addresses are included automatically via the
	// ack flow (tMatchInfo populates makerPerMatchAddr/takerPerMatchAddr).
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("taker ack: %v", err)
	}

	// Verify the per-match addresses were stored on the tracker.
	tracker := rig.getTracker()
	tracker.mtx.RLock()
	if tracker.takerSwapAddr != matchInfo.takerPerMatchAddr {
		t.Fatalf("expected taker per-match addr %q on tracker, got %q",
			matchInfo.takerPerMatchAddr, tracker.takerSwapAddr)
	}
	tracker.mtx.RUnlock()

	// Maker sends swap using sendSwap_maker, which picks up the taker's
	// per-match address from the tracker via swapRecipient.
	if err := rig.sendSwap_maker(true); err != nil {
		t.Fatalf("sendSwap_maker with per-match addr failed: %v", err)
	}
}

func TestPerMatchAddressWrongAddr(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 1)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	// Ack both sides with per-match addresses via the ack flow.
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("taker ack: %v", err)
	}

	// Maker sends swap with wrong address (order-level addr instead of
	// per-match addr).
	swap := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.taker.addr, matchInfo.maker)
	rig.abcNode.setContract(swap.coin, false)
	rig.auth.swapID = swap.req.ID
	rpcErr := rig.swapper.handleInit(matchInfo.maker.acct, swap.req)
	if rpcErr != nil {
		// Immediate error means synchronous rejection.
		return
	}
	// May also be an async error via the coin waiter.
	timeOutMempool()
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for swap response")
	}
	if err := rig.checkServerResponseFail(matchInfo.maker, msgjson.ContractError, "incorrect recipient"); err != nil {
		t.Fatalf("expected contract error for wrong address: %v", err)
	}
}

func TestCoinIDDedup(t *testing.T) {
	// Create two independent matches with different makers and takers.
	qty := uint64(1e8)
	rate := uint64(1e8)

	maker1, taker1 := tNewUser("maker1"), tNewUser("taker1")
	makerOrder1 := makeLimitOrder(qty, rate, maker1, true)
	takerOrder1 := makeLimitOrder(qty, rate, taker1, false)
	matchInfo1 := tMatchInfo(maker1, taker1, qty, rate, makerOrder1, takerOrder1)
	set1 := new(tMatchSet).add(matchInfo1)

	maker2, taker2 := tNewUser("maker2"), tNewUser("taker2")
	makerOrder2 := makeLimitOrder(qty, rate, maker2, true)
	takerOrder2 := makeLimitOrder(qty, rate, taker2, false)
	matchInfo2 := tMatchInfo(maker2, taker2, qty, rate, makerOrder2, takerOrder2)
	set2 := new(tMatchSet).add(matchInfo2)

	rig, cleanup := tNewTestRig(matchInfo1)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 2)
	rig.auth.auditReq = make(chan struct{}, 2)

	rig.applyMatchesAndRequestAcks(t, set1.matchSet)
	rig.applyMatchesAndRequestAcks(t, set2.matchSet)

	// Ack both matches.
	rig.matchInfo = matchInfo1
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("match1 maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("match1 taker ack: %v", err)
	}
	rig.matchInfo = matchInfo2
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("match2 maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("match2 taker ack: %v", err)
	}

	// Send swap for match 1 using taker's per-match address.
	rig.matchInfo = matchInfo1
	swap1 := tNewSwap(matchInfo1, matchInfo1.makerOID, matchInfo1.takerPerMatchAddr, matchInfo1.maker)
	rig.abcNode.setContract(swap1.coin, false)
	rig.auth.swapID = swap1.req.ID
	rpcErr := rig.swapper.handleInit(matchInfo1.maker.acct, swap1.req)
	if rpcErr != nil {
		t.Fatalf("match1 swap init failed: %v", rpcErr.Message)
	}
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for match1 swap response")
	}
	if err := rig.checkServerResponseSuccess(matchInfo1.maker); err != nil {
		t.Fatalf("match1 swap should succeed: %v", err)
	}

	// Try to use the same CoinID and contract for match 2 - this should fail.
	rig.matchInfo = matchInfo2
	reusedCoinID := swap1.coin.ID() // same coin
	swap2 := tNewSwap(matchInfo2, matchInfo2.makerOID, matchInfo2.takerPerMatchAddr, matchInfo2.maker)
	// Override the CoinID and contract to reuse match1's.
	var init1 msgjson.Init
	swap1.req.Unmarshal(&init1)
	var init2 msgjson.Init
	swap2.req.Unmarshal(&init2)
	init2.CoinID = reusedCoinID
	init2.Contract = init1.Contract // same contract data
	swap2.req, _ = msgjson.NewRequest(swap2.req.ID, msgjson.InitRoute, &init2)
	// Also set the contract on the backend for the reused coin.
	swap2.coin.Coin.(*TCoin).id = reusedCoinID
	rig.abcNode.setContract(swap2.coin, false)
	rig.auth.swapID = swap2.req.ID
	rpcErr = rig.swapper.handleInit(matchInfo2.maker.acct, swap2.req)
	if rpcErr != nil {
		// Synchronous rejection.
		if !strings.Contains(rpcErr.Message, "already in use") {
			t.Fatalf("expected 'already in use' error, got: %s", rpcErr.Message)
		}
		return
	}

	timeOutMempool()
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for match2 swap response")
	}
	if err := rig.checkServerResponseFail(matchInfo2.maker, msgjson.ContractError, "already in use"); err != nil {
		t.Fatalf("expected CoinID reuse error: %v", err)
	}
}

func TestCoinIDDedupSameMatch(t *testing.T) {
	// Verify that the same CoinID can be retried for the same match.
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 2)
	rig.auth.auditReq = make(chan struct{}, 2)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("taker ack: %v", err)
	}

	// First init should succeed using taker's per-match address.
	swap := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.takerPerMatchAddr, matchInfo.maker)
	rig.abcNode.setContract(swap.coin, false)
	rig.auth.swapID = swap.req.ID
	rpcErr := rig.swapper.handleInit(matchInfo.maker.acct, swap.req)
	if rpcErr != nil {
		t.Fatalf("first init failed: %v", rpcErr.Message)
	}
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first swap response")
	}
	if err := rig.checkServerResponseSuccess(matchInfo.maker); err != nil {
		t.Fatalf("first init should succeed: %v", err)
	}

	// Retry same CoinID for the same match (e.g. client retries) - the
	// status check in step() should catch that we're past the init step.
	// The dedup check itself should pass since existingMatch == match.MatchID.
	swap2 := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.takerPerMatchAddr, matchInfo.maker)
	// Copy the same CoinID.
	var init2 msgjson.Init
	swap2.req.Unmarshal(&init2)
	init2.CoinID = swap.coin.ID()
	swap2.req, _ = msgjson.NewRequest(swap2.req.ID, msgjson.InitRoute, &init2)
	rpcErr = rig.swapper.handleInit(matchInfo.maker.acct, swap2.req)
	// This should fail because the match status has already advanced, but the
	// CoinID dedup itself should not be the reason - it should be a step error.
	if rpcErr == nil {
		t.Fatal("expected error for duplicate init on already-swapped match")
	}
	if strings.Contains(rpcErr.Message, "already in use") {
		t.Fatal("CoinID dedup should not reject same-match retries")
	}
}

func inactiveMatchWrites(storage *TStorage) []inactiveMatchWrite {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	return append([]inactiveMatchWrite(nil), storage.inactiveMatches...)
}

type swapDoneCall struct {
	oid  order.OrderID
	fail bool
}

func TestApplyMatchFailedEvent(t *testing.T) {
	// A taker address fault is only expressible at NewlyMatched.
	if _, err := matchFailureReason(order.MakerSwapCast, true, true); err == nil {
		t.Fatalf("matchFailureReason accepted a taker address fault outside NewlyMatched")
	}

	type doneSpec struct {
		processMaker bool // maker-side completion runs (not at MakerRedeemed)
		makerFault   bool
		takerFault   bool
	}
	tests := []struct {
		name           string
		status         order.MatchStatus
		reason         meshevents.MatchFailureReason
		selfTrade      bool
		missingMatch   bool
		storageErr     bool
		marketMismatch bool
		statusMismatch bool
		wantApplyErr   bool
		wantApplied    bool
		wantForgive    bool
		wantDone       *doneSpec
	}{
		{
			name:        "newly matched maker fault",
			status:      order.NewlyMatched,
			reason:      meshevents.MatchFailureMakerNoSwap,
			wantApplied: true,
			wantDone:    &doneSpec{processMaker: true, makerFault: true},
		},
		{
			name:        "newly matched taker address fault",
			status:      order.NewlyMatched,
			reason:      meshevents.MatchFailureTakerNoAddress,
			wantApplied: true,
			wantDone:    &doneSpec{processMaker: true, takerFault: true},
		},
		{
			name:        "maker swap cast",
			status:      order.MakerSwapCast,
			reason:      meshevents.MatchFailureTakerNoSwap,
			wantApplied: true,
			wantDone:    &doneSpec{processMaker: true, takerFault: true},
		},
		{
			name:        "taker swap cast",
			status:      order.TakerSwapCast,
			reason:      meshevents.MatchFailureMakerNoRedeem,
			wantApplied: true,
			wantDone:    &doneSpec{processMaker: true, makerFault: true},
		},
		{
			name:        "maker redeemed",
			status:      order.MakerRedeemed,
			reason:      meshevents.MatchFailureTakerNoRedeem,
			wantApplied: true,
			wantDone:    &doneSpec{takerFault: true},
		},
		{
			name:        "no user fault",
			status:      order.MakerSwapCast,
			reason:      meshevents.MatchFailureNoFaultMakerSwapCast,
			wantApplied: true,
			wantForgive: true,
			wantDone:    &doneSpec{processMaker: true},
		},
		{
			name:        "self trade no inaction",
			status:      order.NewlyMatched,
			reason:      meshevents.MatchFailureMakerNoSwap,
			selfTrade:   true,
			wantApplied: true,
			wantDone:    &doneSpec{processMaker: true, makerFault: true},
		},
		{
			name:         "storage error leaves match",
			status:       order.NewlyMatched,
			reason:       meshevents.MatchFailureMakerNoSwap,
			storageErr:   true,
			wantApplyErr: true,
		},
		{
			name:         "missing match errors under event log",
			status:       order.NewlyMatched,
			reason:       meshevents.MatchFailureMakerNoSwap,
			missingMatch: true,
			wantApplyErr: true,
		},
		{
			name:           "market mismatch has no side effects",
			status:         order.NewlyMatched,
			reason:         meshevents.MatchFailureMakerNoSwap,
			marketMismatch: true,
			wantApplyErr:   true,
		},
		{
			name:           "status mismatch errors before storage",
			status:         order.NewlyMatched,
			reason:         meshevents.MatchFailureMakerNoSwap,
			statusMismatch: true,
			wantApplyErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			if tt.selfTrade {
				user := tNewUser("self")
				qty, rate := uint64(1e8), uint64(1e8)
				makerOrder := makeLimitOrder(qty, rate, user, true)
				takerOrder := makeLimitOrder(qty, rate, user, false)
				set = new(tMatchSet).add(tMatchInfo(user, user, qty, rate, makerOrder, takerOrder))
			}
			matchInfo := set.matchInfos[0]
			rig, cleanup := tNewTestRig(matchInfo)
			defer cleanup()

			var tracker *matchTracker
			if !tt.missingMatch {
				if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
					t.Fatalf("ApplyMatches error: %v", err)
				}
				tracker = rig.getTracker()
				tracker.mtx.Lock()
				if tt.statusMismatch {
					tracker.Status = order.MakerSwapCast
				} else {
					tracker.Status = tt.status
				}
				tracker.mtx.Unlock()
			}
			if tt.storageErr {
				rig.storage.setMatchInactiveErr = errors.New("storage error")
			}

			var swapDone []swapDoneCall
			rig.swapper.swapDone = func(ord order.Order, _ *order.Match, faulted bool) {
				swapDone = append(swapDone, swapDoneCall{ord.ID(), faulted})
			}

			base, quote := uint32(ABCID), uint32(XYZID)
			if tracker != nil {
				base, quote = tracker.Maker.BaseAsset, tracker.Maker.QuoteAsset
			}
			if tt.marketMismatch {
				base++
			}
			failTime := time.Now().Truncate(time.Millisecond).UTC().UnixMilli()
			event, err := mesh.NewEvent(&meshevents.MatchFailedEvent{
				MatchID:  matchInfo.matchID,
				Base:     base,
				Quote:    quote,
				FailTime: failTime,
				Reason:   tt.reason,
			})
			if err != nil {
				t.Fatalf("NewEvent error: %v", err)
			}
			applier := rig.swapper.Events()[meshevents.EventKindMatchFailed]
			if applier == nil {
				t.Fatalf("missing %q event applier", meshevents.EventKindMatchFailed)
			}
			_, err = applier(&mesh.EventApplyContext{Context: context.Background()}, event)
			if tt.wantApplyErr {
				if err == nil {
					t.Fatalf("expected apply error")
				}
			} else if err != nil {
				t.Fatalf("apply match_failed error: %v", err)
			}

			if tt.wantApplied && rig.getTracker() != nil {
				t.Fatalf("match was not deleted")
			}
			if !tt.wantApplied && !tt.missingMatch && rig.getTracker() == nil {
				t.Fatalf("match was unexpectedly deleted")
			}

			inactive := inactiveMatchWrites(rig.storage)
			wantInactive := 0
			wantMatchFailedEvents := 0
			if tt.wantApplied {
				wantInactive = 1
			}
			if tt.wantApplied || tt.storageErr {
				wantMatchFailedEvents = 1
			}
			rig.storage.mtx.Lock()
			matchFailedEvents := rig.storage.matchFailedEvents
			matchFailedUpdates := append([]*db.MatchFailedUpdate(nil), rig.storage.matchFailedUpdates...)
			rig.storage.mtx.Unlock()
			if matchFailedEvents != wantMatchFailedEvents {
				t.Fatalf("expected %d match_failed storage attempts, got %d",
					wantMatchFailedEvents, matchFailedEvents)
			}
			if len(matchFailedUpdates) != wantMatchFailedEvents {
				t.Fatalf("expected %d match_failed updates, got %d",
					wantMatchFailedEvents, len(matchFailedUpdates))
			}
			if len(matchFailedUpdates) > 0 {
				update := matchFailedUpdates[len(matchFailedUpdates)-1]
				if update.Reason != db.MatchFailureReason(tt.reason) || update.FailTimeMS != failTime {
					t.Fatalf("wrong match_failed update facts: %+v", update)
				}
			}
			if len(inactive) != wantInactive {
				t.Fatalf("expected %d inactive match writes, got %d", wantInactive, len(inactive))
			}
			if tt.wantApplied {
				wantMID := db.MarketMatchID{
					MatchID: matchInfo.matchID,
					Base:    base,
					Quote:   quote,
				}
				if inactive[0].mid != wantMID || inactive[0].forgive != tt.wantForgive {
					t.Fatalf("wrong inactive write. got mid=%v forgive=%v, want mid=%v forgive=%v",
						inactive[0].mid, inactive[0].forgive, wantMID, tt.wantForgive)
				}
			}

			var wantSwapDone []swapDoneCall
			if tt.wantDone != nil {
				if tt.wantDone.processMaker {
					wantSwapDone = append(wantSwapDone, swapDoneCall{matchInfo.makerOID, tt.wantDone.makerFault})
				}
				wantSwapDone = append(wantSwapDone, swapDoneCall{matchInfo.takerOID, tt.wantDone.takerFault})
			}
			if !reflect.DeepEqual(swapDone, wantSwapDone) {
				t.Fatalf("wrong swapDone calls.\nwant: %#v\n got: %#v", wantSwapDone, swapDone)
			}

			rig.auth.mtx.Lock()
			penalties := len(rig.auth.suspensions)
			rig.auth.mtx.Unlock()
			if penalties != 0 {
				t.Fatalf("planned match_failed inaction should not unbook through penalties; got %d penalties", penalties)
			}
			wantRevokeMatches := 0
			if tt.wantApplied {
				wantRevokeMatches = 2
			}
			if revokeMatches := notificationCount(rig.auth, msgjson.RevokeMatchRoute); revokeMatches != wantRevokeMatches {
				t.Fatalf("expected %d revoke_match notifications, got %d", wantRevokeMatches, revokeMatches)
			}
		})
	}
}

func TestRunStartupRepairs(t *testing.T) {
	contractCoinID := randBytes(36)
	contractData := encode.RandomBytes(32)
	contractTxData := encode.RandomBytes(50)
	redeemCoinID := randBytes(36)
	redeemSecret := encode.RandomBytes(32)

	tests := []struct {
		name           string
		status         order.MatchStatus
		makerAddr      string
		takerAddr      string
		makerContract  bool     // maker's contract recorded in the swap status
		takerContract  bool     // taker's contract recorded in the swap status
		makerAuditSig  bool     // maker's audit ack sig already recorded
		takerAuditSig  bool     // taker's audit ack sig already recorded
		makerRedeem    bool     // maker's redemption recorded in the swap status
		takerRedeemSig bool     // taker's redemption ack sig already recorded
		wantMaker      []string // request routes re-issued to the maker
		wantTaker      []string // request routes re-issued to the taker
	}{
		{
			// The shape left by a master crash after epoch_processed but
			// before RequestMatchAcks: both sides get the match request
			// re-issued instead of the match being revoked.
			name:      "newly matched without addresses",
			status:    order.NewlyMatched,
			wantMaker: []string{msgjson.MatchRoute},
			wantTaker: []string{msgjson.MatchRoute},
		},
		{
			name:      "maker swap cast without addresses",
			status:    order.MakerSwapCast,
			wantMaker: []string{msgjson.MatchRoute},
			wantTaker: []string{msgjson.MatchRoute},
		},
		{
			// Half-acked: only the missing side is re-requested.
			name:      "newly matched half-acked by maker",
			status:    order.NewlyMatched,
			makerAddr: "maker-addr",
			wantTaker: []string{msgjson.MatchRoute},
		},
		{
			name:      "maker swap cast half-acked by taker",
			status:    order.MakerSwapCast,
			takerAddr: "taker-addr",
			wantMaker: []string{msgjson.MatchRoute},
		},
		{
			// Match acks are moot once both contracts are on-chain.
			name:   "taker swap cast without addresses, nothing re-issued",
			status: order.TakerSwapCast,
		},
		{
			name:   "complete without addresses, nothing re-issued",
			status: order.MatchComplete,
		},
		{
			// The shape left by a crash after the maker's contract was
			// recorded but before the taker acked its audit request.
			name:          "audit re-issue to taker",
			status:        order.MakerSwapCast,
			makerAddr:     "maker-addr",
			takerAddr:     "taker-addr",
			makerContract: true,
			wantTaker:     []string{msgjson.AuditRoute},
		},
		{
			name:          "audit re-issue to both",
			status:        order.TakerSwapCast,
			makerAddr:     "maker-addr",
			takerAddr:     "taker-addr",
			makerContract: true,
			takerContract: true,
			wantMaker:     []string{msgjson.AuditRoute},
			wantTaker:     []string{msgjson.AuditRoute},
		},
		{
			name:          "audits already acked, nothing re-issued",
			status:        order.TakerSwapCast,
			makerAddr:     "maker-addr",
			takerAddr:     "taker-addr",
			makerContract: true,
			takerContract: true,
			makerAuditSig: true,
			takerAuditSig: true,
		},
		{
			// The shape left by a crash after the maker's redemption was
			// recorded but before the taker acked the secret delivery.
			name:          "redemption re-issue to taker",
			status:        order.MakerRedeemed,
			makerAddr:     "maker-addr",
			takerAddr:     "taker-addr",
			makerContract: true,
			takerContract: true,
			makerAuditSig: true,
			takerAuditSig: true,
			makerRedeem:   true,
			wantTaker:     []string{msgjson.RedemptionRoute},
		},
		{
			name:           "redemption already acked, nothing re-issued",
			status:         order.MakerRedeemed,
			makerAddr:      "maker-addr",
			takerAddr:      "taker-addr",
			makerContract:  true,
			takerContract:  true,
			makerAuditSig:  true,
			takerAuditSig:  true,
			makerRedeem:    true,
			takerRedeemSig: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("TrackMatches error: %v", err)
			}
			// Stamp all event-relative state well in the past, as the previous
			// master would have.
			staleTime := time.Now().Add(-2 * tBcastTimeout)
			tracker := rig.getTracker()
			tracker.mtx.Lock()
			tracker.Status = tt.status
			tracker.makerSwapAddr = tt.makerAddr
			tracker.takerSwapAddr = tt.takerAddr
			if tt.makerAuditSig {
				tracker.Sigs.MakerAudit = matchInfo.maker.sig
			}
			if tt.takerAuditSig {
				tracker.Sigs.TakerAudit = matchInfo.taker.sig
			}
			if tt.takerRedeemSig {
				tracker.Sigs.TakerRedeem = matchInfo.taker.sig
			}
			tracker.time = staleTime
			tracker.mtx.Unlock()
			if tt.makerContract {
				tracker.makerStatus.mtx.Lock()
				tracker.makerStatus.swap = &asset.Contract{
					Coin:         &TCoin{id: contractCoinID},
					ContractData: contractData,
					TxData:       contractTxData,
				}
				tracker.makerStatus.swapTime = staleTime
				tracker.makerStatus.mtx.Unlock()
			}
			if tt.takerContract {
				tracker.takerStatus.mtx.Lock()
				tracker.takerStatus.swap = &asset.Contract{
					Coin:         &TCoin{id: randBytes(36)},
					ContractData: encode.RandomBytes(32),
					TxData:       encode.RandomBytes(50),
				}
				tracker.takerStatus.swapTime = staleTime
				tracker.takerStatus.mtx.Unlock()
			}
			if tt.makerRedeem {
				tracker.makerStatus.mtx.Lock()
				tracker.makerStatus.redemption = &TCoin{id: redeemCoinID}
				tracker.makerStatus.redeemTime = staleTime
				tracker.makerStatus.secret = redeemSecret
				tracker.makerStatus.mtx.Unlock()
			}

			tMesh := &tSwapMesh{applier: rig.swapper.Events()}
			rig.swapper.SetMeshService(tMesh)
			promoTime := time.Now()
			if err := rig.swapper.runStartupRepairs(context.Background()); err != nil {
				t.Fatalf("runStartupRepairs error: %v", err)
			}

			// Startup repairs re-issue requests; they never revoke.
			if len(tMesh.events) != 0 {
				t.Fatalf("startup repair emitted %d events, want 0", len(tMesh.events))
			}
			if rig.getTracker() == nil {
				t.Fatalf("match was deleted by startup repair")
			}

			// The stale event-relative deadline bases are floored at the
			// promotion time so re-issued requests get a full bTimeout.
			tracker.mtx.RLock()
			flooredTime := tracker.time
			tracker.mtx.RUnlock()
			if flooredTime.Before(promoTime) {
				t.Fatalf("match time %v not floored at promotion time %v", flooredTime, promoTime)
			}
			if tt.makerRedeem {
				if rt := tracker.makerStatus.redeemSeenTime(); rt.Before(promoTime) {
					t.Fatalf("redeem time %v not floored at promotion time %v", rt, promoTime)
				}
			}

			requestRoutes := func(user account.AccountID) []string {
				rig.auth.mtx.Lock()
				defer rig.auth.mtx.Unlock()
				var routes []string
				for _, req := range rig.auth.reqs[user] {
					routes = append(routes, req.req.Route)
				}
				return routes
			}
			if got := requestRoutes(matchInfo.maker.acct); !reflect.DeepEqual(got, tt.wantMaker) {
				t.Fatalf("maker requests = %v, want %v", got, tt.wantMaker)
			}
			if got := requestRoutes(matchInfo.taker.acct); !reflect.DeepEqual(got, tt.wantTaker) {
				t.Fatalf("taker requests = %v, want %v", got, tt.wantTaker)
			}

			// Verify the re-issued request payloads. Only taker-bound
			// requests are checked: a maker-bound audit would carry the
			// taker's contract, which is seeded with anonymous fixtures.
			for _, route := range tt.wantTaker {
				req := rig.auth.popReq(matchInfo.taker.acct)
				switch route {
				case msgjson.MatchRoute:
					if err := rig.checkMatchNotification(req.req, matchInfo.takerOID, matchInfo.maker.addr); err != nil {
						t.Fatalf("re-issued match request: %v", err)
					}
				case msgjson.AuditRoute:
					var params msgjson.Audit
					if err := req.req.Unmarshal(&params); err != nil {
						t.Fatalf("unmarshal re-issued audit request: %v", err)
					}
					if !bytes.Equal(params.MatchID, matchInfo.matchID[:]) {
						t.Fatalf("audit request match ID = %x, want %x", params.MatchID, matchInfo.matchID[:])
					}
					if params.OrderID.String() != matchInfo.takerOID.String() {
						t.Fatalf("audit request order ID = %s, want %s", params.OrderID, matchInfo.takerOID)
					}
					if !bytes.Equal(params.CoinID, contractCoinID) {
						t.Fatalf("audit request coin ID = %x, want %x", params.CoinID, contractCoinID)
					}
					if !bytes.Equal(params.Contract, contractData) {
						t.Fatalf("audit request contract = %x, want %x", params.Contract, contractData)
					}
					if !bytes.Equal(params.TxData, contractTxData) {
						t.Fatalf("audit request tx data = %x, want %x", params.TxData, contractTxData)
					}
				case msgjson.RedemptionRoute:
					var params msgjson.Redemption
					if err := req.req.Unmarshal(&params); err != nil {
						t.Fatalf("unmarshal re-issued redemption request: %v", err)
					}
					if !bytes.Equal(params.MatchID, matchInfo.matchID[:]) {
						t.Fatalf("redemption request match ID = %x, want %x", params.MatchID, matchInfo.matchID[:])
					}
					if params.OrderID.String() != matchInfo.takerOID.String() {
						t.Fatalf("redemption request order ID = %s, want %s", params.OrderID, matchInfo.takerOID)
					}
					if !bytes.Equal(params.CoinID, redeemCoinID) {
						t.Fatalf("redemption request coin ID = %x, want %x", params.CoinID, redeemCoinID)
					}
					if !bytes.Equal(params.Secret, redeemSecret) {
						t.Fatalf("redemption request secret = %x, want %x", params.Secret, redeemSecret)
					}
				}
			}
		})
	}
}

// TestReackKeepsRecordedAddress checks that a re-ack with any other
// address — divergent, empty, or backend-rejected — keeps the recorded
// address in memory and in the stored event, without validating the
// submitted one.
func TestReackKeepsRecordedAddress(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reack  string
		reject bool // backend rejects all addresses; coercion must skip validation
	}{
		{name: "divergent", reack: "divergent-taker-addr"},
		{name: "empty", reack: ""},
		{name: "invalid", reack: "backend-rejected-addr", reject: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("TrackMatches: %v", err)
			}
			rig.swapper.SetMeshService(&tSwapMesh{applier: rig.swapper.Events()})
			tracker := rig.getTracker()
			taker := matchInfo.taker

			tracker.mtx.Lock()
			tracker.makerSwapAddr = matchInfo.makerPerMatchAddr
			tracker.takerSwapAddr = matchInfo.takerPerMatchAddr
			tracker.Sigs.MakerMatch = matchInfo.maker.sig
			tracker.Sigs.TakerMatch = taker.sig
			tracker.counterPartyAddrsSent = true
			tracker.mtx.Unlock()

			if tt.reject {
				rig.abcNode.rejectSwapAddrs = true
				rig.xyzNode.rejectSwapAddrs = true
			}

			rig.swapper.resendMatchRequest(tracker, false)
			req := rig.auth.popReq(taker.acct)
			if req == nil || req.req.Route != msgjson.MatchRoute {
				t.Fatal("no match re-request")
			}
			req.respFunc(nil, tNewResponse(req.req.ID, tAckArrWithAddrs(taker,
				[]order.MatchID{matchInfo.matchID},
				map[order.MatchID]string{matchInfo.matchID: tt.reack})))
			if msg, resp := rig.auth.popResp(taker.acct); msg != nil {
				t.Fatalf("unexpected error response: %+v", resp)
			}

			tracker.mtx.RLock()
			got := tracker.takerSwapAddr
			tracker.mtx.RUnlock()
			if got != matchInfo.takerPerMatchAddr {
				t.Fatalf("taker addr = %q, want %q", got, matchInfo.takerPerMatchAddr)
			}
			updates := matchAcksRecordedUpdates(rig.storage)
			if len(updates) != 1 || len(updates[0].Acks) != 1 ||
				updates[0].Acks[0].Address != matchInfo.takerPerMatchAddr {
				t.Fatalf("stored re-ack = %+v, want %q", updates, matchInfo.takerPerMatchAddr)
			}
		})
	}
}

// requireMatchFailedEvent decodes a single emitted match_failed event and
// checks its match ID and reason.
func requireMatchFailedEvent(t *testing.T, events []*mesh.Event, matchID order.MatchID, reason meshevents.MatchFailureReason) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("match_failed events = %d, want 1", len(events))
	}
	event, err := meshevents.DecodeMatchFailedEvent(events[0].Payload)
	if err != nil {
		t.Fatalf("DecodeMatchFailedEvent error: %v", err)
	}
	if event.MatchID != matchID || event.Reason != reason {
		t.Fatalf("match_failed event = %#v, want match %v reason %d", event, matchID, reason)
	}
}

func TestCheckInactionEventBased(t *testing.T) {
	cases := []struct {
		name        string
		status      order.MatchStatus
		makerRedeem bool // the deadline base is the maker's redeem time
		wantReason  meshevents.MatchFailureReason
	}{
		{
			name:       "newly matched maker swap overdue",
			status:     order.NewlyMatched,
			wantReason: meshevents.MatchFailureMakerNoSwap,
		},
		{
			name:        "maker redeemed taker redeem ack overdue",
			status:      order.MakerRedeemed,
			makerRedeem: true,
			wantReason:  meshevents.MatchFailureTakerNoRedeem,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("TrackMatches error: %v", err)
			}
			tracker := rig.getTracker()
			tracker.mtx.Lock()
			tracker.Status = tc.status
			tracker.makerSwapAddr = "maker-addr"
			tracker.takerSwapAddr = "taker-addr"
			tracker.counterPartyAddrsSent = true
			if tc.makerRedeem {
				tracker.Sigs.MakerAudit = matchInfo.maker.sig
				tracker.Sigs.TakerAudit = matchInfo.taker.sig
			}
			// Start with a burned deadline: the pre-MarketsReady check must
			// stay inert, and MarketsReady re-floors it to a fresh budget.
			tracker.time = time.Now().Add(-tBcastTimeout)
			tracker.mtx.Unlock()
			if tc.makerRedeem {
				tracker.makerStatus.mtx.Lock()
				tracker.makerStatus.redemption = &TCoin{id: randBytes(36)}
				tracker.makerStatus.redeemTime = time.Now().Add(-tBcastTimeout)
				tracker.makerStatus.secret = encode.RandomBytes(32)
				tracker.makerStatus.mtx.Unlock()
			}
			tMesh := &tSwapMesh{applier: rig.swapper.Events()}
			rig.swapper.SetMeshService(tMesh)
			// The inaction checks are inert until the markets report ready.
			rig.swapper.checkInactionEventBased()
			if len(tMesh.events) != 0 {
				t.Fatalf("match faulted before MarketsReady")
			}
			rig.swapper.MarketsReady()

			// No fault inside the full broadcast-timeout window.
			rig.swapper.checkInactionEventBased()
			if len(tMesh.events) != 0 {
				t.Fatalf("match faulted before the broadcast timeout elapsed")
			}

			// A fault once the window elapses.
			if tc.makerRedeem {
				tracker.makerStatus.mtx.Lock()
				tracker.makerStatus.redeemTime = time.Now().Add(-tBcastTimeout)
				tracker.makerStatus.mtx.Unlock()
			} else {
				tracker.mtx.Lock()
				tracker.time = time.Now().Add(-tBcastTimeout)
				tracker.mtx.Unlock()
			}
			rig.swapper.checkInactionEventBased()
			requireMatchFailedEvent(t, tMesh.events, matchInfo.matchID, tc.wantReason)
		})
	}
}

// TestMarketsReady verifies that MarketsReady re-floors the event-relative
// deadlines, so a wait on market startup cannot burn the clients'
// broadcast-timeout budget.
func TestMarketsReady(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig := tNewUnstartedRig(matchInfo)
	if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
		t.Fatalf("TrackMatches error: %v", err)
	}
	tracker := rig.getTracker()
	stale := time.Now().Add(-tBcastTimeout) // deadline already burned
	tracker.mtx.Lock()
	tracker.Status = order.NewlyMatched
	tracker.time = stale
	tracker.mtx.Unlock()
	tracker.makerStatus.mtx.Lock()
	tracker.makerStatus.redeemTime = stale
	tracker.makerStatus.swapConfirmed = stale
	tracker.makerStatus.mtx.Unlock()
	tracker.takerStatus.mtx.Lock()
	tracker.takerStatus.swapConfirmed = stale
	tracker.takerStatus.mtx.Unlock()

	tMesh := &tSwapMesh{applier: rig.swapper.Events()}
	rig.swapper.SetMeshService(tMesh)

	// The stalled-startup scenario: the deadline is burned, but the checks
	// are inert until the markets report ready.
	rig.swapper.checkInactionEventBased()
	if len(tMesh.events) != 0 {
		t.Fatalf("match faulted before MarketsReady")
	}

	before := time.Now()
	rig.swapper.MarketsReady()

	tracker.mtx.RLock()
	gotTime := tracker.time
	tracker.mtx.RUnlock()
	if gotTime.Before(before) {
		t.Fatalf("match time not re-floored: %v", gotTime)
	}
	tracker.makerStatus.mtx.RLock()
	gotRedeem := tracker.makerStatus.redeemTime
	gotMakerConf := tracker.makerStatus.swapConfirmed
	tracker.makerStatus.mtx.RUnlock()
	if gotRedeem.Before(before) {
		t.Fatalf("redeem time not re-floored: %v", gotRedeem)
	}
	if gotMakerConf.Before(before) {
		t.Fatalf("maker swap confirmation time not re-floored: %v", gotMakerConf)
	}
	tracker.takerStatus.mtx.RLock()
	gotTakerConf := tracker.takerStatus.swapConfirmed
	tracker.takerStatus.mtx.RUnlock()
	if gotTakerConf.Before(before) {
		t.Fatalf("taker swap confirmation time not re-floored: %v", gotTakerConf)
	}

	// The burned budget did not fault the match.
	rig.swapper.checkInactionEventBased()
	if len(tMesh.events) != 0 {
		t.Fatalf("match faulted despite the re-floored deadline")
	}
}

// TestCheckInactionBlockBasedGate verifies that the confirmation-relative
// inaction check is inert until MarketsReady, and that the re-floored
// confirmation time grants a fresh broadcast-timeout budget.
func TestCheckInactionBlockBasedGate(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig := tNewUnstartedRig(matchInfo)
	if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
		t.Fatalf("TrackMatches error: %v", err)
	}
	tracker := rig.getTracker()
	stale := time.Now().Add(-tBcastTimeout)
	tracker.mtx.Lock()
	tracker.Status = order.MakerSwapCast
	tracker.makerSwapAddr = "maker-addr"
	tracker.takerSwapAddr = "taker-addr"
	tracker.counterPartyAddrsSent = true
	tracker.mtx.Unlock()
	swapAsset := tracker.makerStatus.swapAsset
	tracker.makerStatus.mtx.Lock()
	tracker.makerStatus.swap = &asset.Contract{
		Coin:         &TCoin{id: randBytes(36)},
		ContractData: encode.RandomBytes(32),
		TxData:       encode.RandomBytes(50),
		LockTime:     time.Now().Add(time.Hour), // not expired: inaction is a fault
	}
	tracker.makerStatus.swapTime = stale
	tracker.makerStatus.swapConfirmed = stale
	tracker.makerStatus.mtx.Unlock()

	tMesh := &tSwapMesh{applier: rig.swapper.Events()}
	rig.swapper.SetMeshService(tMesh)

	// Inert while the markets are still starting, despite the burned budget.
	rig.swapper.checkInactionBlockBased(swapAsset)
	if len(tMesh.events) != 0 {
		t.Fatalf("match faulted before MarketsReady")
	}

	// MarketsReady re-floors the confirmation time: still no fault.
	rig.swapper.MarketsReady()
	rig.swapper.checkInactionBlockBased(swapAsset)
	if len(tMesh.events) != 0 {
		t.Fatalf("match faulted despite the re-floored confirmation time")
	}

	// A budget burned after MarketsReady faults the taker.
	tracker.makerStatus.mtx.Lock()
	tracker.makerStatus.swapConfirmed = time.Now().Add(-tBcastTimeout)
	tracker.makerStatus.mtx.Unlock()
	rig.swapper.checkInactionBlockBased(swapAsset)
	requireMatchFailedEvent(t, tMesh.events, matchInfo.matchID, meshevents.MatchFailureTakerNoSwap)
}

// TestMatchFailureReasonMirrorsDB ensures the catalog-local wire enum stays in
// lockstep with the db reason enum it mirrors, since meshevents cannot import
// server/db.
func TestMatchFailureReasonMirrorsDB(t *testing.T) {
	pairs := []struct {
		wire meshevents.MatchFailureReason
		db   db.MatchFailureReason
	}{
		{meshevents.MatchFailureReasonInvalid, db.MatchFailureReasonInvalid},
		{meshevents.MatchFailureNoFaultNewlyMatched, db.MatchFailureNoFaultNewlyMatched},
		{meshevents.MatchFailureNoFaultMakerSwapCast, db.MatchFailureNoFaultMakerSwapCast},
		{meshevents.MatchFailureNoFaultTakerSwapCast, db.MatchFailureNoFaultTakerSwapCast},
		{meshevents.MatchFailureNoFaultMakerRedeemed, db.MatchFailureNoFaultMakerRedeemed},
		{meshevents.MatchFailureMakerNoSwap, db.MatchFailureMakerNoSwap},
		{meshevents.MatchFailureTakerNoAddress, db.MatchFailureTakerNoAddress},
		{meshevents.MatchFailureTakerNoSwap, db.MatchFailureTakerNoSwap},
		{meshevents.MatchFailureMakerNoRedeem, db.MatchFailureMakerNoRedeem},
		{meshevents.MatchFailureTakerNoRedeem, db.MatchFailureTakerNoRedeem},
	}
	for _, pair := range pairs {
		if uint8(pair.wire) != uint8(pair.db) {
			t.Fatalf("wire reason %d != db reason %d", pair.wire, pair.db)
		}
		if meshevents.ValidMatchFailureReason(pair.wire) == (pair.db == db.MatchFailureReasonInvalid) {
			t.Fatalf("wire reason %d validity disagrees with db reason set", pair.wire)
		}
		if _, ok := db.MatchFailureReasonDetails(pair.db); ok != meshevents.ValidMatchFailureReason(pair.wire) {
			t.Fatalf("wire reason %d validity disagrees with db details for %d", pair.wire, pair.db)
		}
	}
}

func TestFailMatch(t *testing.T) {
	tests := []struct {
		name       string
		applyErr   error
		callTwice  bool
		advanceTo  order.MatchStatus // status move between decision and propose
		wantEvents int
	}{
		{
			name:       "emits and applies",
			wantEvents: 1,
		},
		{
			name:       "apply error leaves match tracked",
			applyErr:   errors.New("apply failed"),
			callTwice:  true,
			wantEvents: 2,
		},
		{
			name:       "duplicate after apply does not emit",
			callTwice:  true,
			wantEvents: 1,
		},
		{
			name:       "stale decision status is rejected, not repurposed",
			advanceTo:  order.MakerRedeemed,
			wantEvents: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("TrackMatches error: %v", err)
			}
			tracker := rig.getTracker()
			tracker.mtx.Lock()
			tracker.Status = order.TakerSwapCast
			tracker.mtx.Unlock()

			tMesh := &tSwapMesh{err: tt.applyErr, applier: rig.swapper.Events()}
			rig.swapper.SetMeshService(tMesh)
			if tt.advanceTo != 0 {
				tracker.mtx.Lock()
				tracker.Status = tt.advanceTo
				tracker.mtx.Unlock()
			}
			rig.swapper.failMatch(tracker, order.TakerSwapCast, true, false)
			if tt.callTwice {
				rig.swapper.failMatch(tracker, order.TakerSwapCast, true, false)
			}

			if len(tMesh.events) != tt.wantEvents {
				t.Fatalf("match_failed events = %d, want %d", len(tMesh.events), tt.wantEvents)
			}
			event, err := meshevents.DecodeMatchFailedEvent(tMesh.events[0].Payload)
			if err != nil {
				t.Fatalf("DecodeMatchFailedEvent error: %v", err)
			}
			if event.MatchID != matchInfo.matchID || event.Reason != meshevents.MatchFailureMakerNoRedeem {
				t.Fatalf("match_failed event = %#v, want match %v reason %d",
					event, matchInfo.matchID, meshevents.MatchFailureMakerNoRedeem)
			}
			applied := tt.applyErr == nil && tt.advanceTo == 0
			tracked := rig.swapper.matches[matchInfo.matchID] != nil
			if applied && tracked {
				t.Fatalf("failed match still tracked")
			}
			if !applied && !tracked {
				t.Fatalf("live match was untracked")
			}
		})
	}
}

func TestUserConnectedResend(t *testing.T) {
	// Route selection and re-issued request payloads are owned by
	// TestRunStartupRepairs; this test owns what UserConnected adds: the
	// per-user filter, the counterparty_address resend, and the master gate.
	clearComms := func(rig *testRig) {
		rig.auth.mtx.Lock()
		rig.auth.reqs = make(map[account.AccountID][]*TRequest)
		rig.auth.ntfns = make(map[account.AccountID][]*msgjson.Message)
		rig.auth.mtx.Unlock()
	}

	cases := []struct {
		name      string
		master    bool
		bothAcked bool   // both per-match addresses recorded
		wantRoute string // request re-issued to the reconnecting taker
		wantCPA   bool   // reconnecting maker gets a counterparty_address notification
	}{
		{name: "both acked resends only the counterparty address", master: true, bothAcked: true, wantCPA: true},
		{name: "pending ack re-issues only to the pending user", master: true, wantRoute: msgjson.MatchRoute},
		{name: "off master re-issues nothing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			var rig *testRig
			if tc.master {
				var cleanup func()
				rig, cleanup = tNewTestRig(matchInfo)
				defer cleanup()
				rig.applyMatchesAndRequestAcks(t, set.matchSet)
			} else {
				rig = tNewUnstartedRig(matchInfo)
				if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
					t.Fatalf("TrackMatches error: %v", err)
				}
			}
			tracker := rig.getTracker()
			tracker.mtx.Lock()
			tracker.makerSwapAddr = "maker-addr" // the taker's ack is missing...
			if tc.bothAcked {
				tracker.takerSwapAddr = "taker-addr" // ...unless both sides acked
				tracker.counterPartyAddrsSent = true
			}
			tracker.mtx.Unlock()
			clearComms(rig)

			// The maker reconnects first: nothing is pending for the maker,
			// so no request is re-issued; with both addresses recorded the
			// maker gets the counterparty_address notification — and only the
			// maker, the taker sees nothing until it reconnects itself.
			rig.swapper.UserConnected(matchInfo.maker.acct)
			if req := rig.auth.popReq(matchInfo.maker.acct); req != nil {
				t.Fatalf("unexpected %s request re-issued to maker", req.req.Route)
			}
			var cpa msgjson.CounterPartyAddress
			err := rig.auth.getNtfn(matchInfo.maker.acct, msgjson.CounterPartyAddressRoute, &cpa)
			if tc.wantCPA {
				if err != nil {
					t.Fatalf("no counterparty_address resend on reconnect: %v", err)
				}
				if cpa.Address != "taker-addr" {
					t.Fatalf("resent addr = %q, want %q", cpa.Address, "taker-addr")
				}
			} else if err == nil {
				t.Fatalf("unexpected counterparty_address notification")
			}
			rig.auth.mtx.Lock()
			takerComms := len(rig.auth.reqs[matchInfo.taker.acct]) + len(rig.auth.ntfns[matchInfo.taker.acct])
			rig.auth.mtx.Unlock()
			if takerComms != 0 {
				t.Fatalf("maker reconnect reached the taker: %d messages", takerComms)
			}

			// The taker reconnects: at most the taker's own pending request
			// is re-issued.
			rig.swapper.UserConnected(matchInfo.taker.acct)
			if tc.wantRoute != "" {
				req := rig.auth.popReq(matchInfo.taker.acct)
				if req == nil || req.req.Route != tc.wantRoute {
					t.Fatalf("no %s request re-issued to taker on reconnect", tc.wantRoute)
				}
			}
			if req := rig.auth.popReq(matchInfo.taker.acct); req != nil {
				t.Fatalf("unexpected %s request to taker", req.req.Route)
			}
			if req := rig.auth.popReq(matchInfo.maker.acct); req != nil {
				t.Fatalf("unexpected %s request to maker", req.req.Route)
			}
		})
	}
}

// newStaleRig makes a rig with one tracked match at status and the given
// per-match addresses. Event times are aged a full bTimeout so only last-send
// stamps can hold a re-send back. Contracts, audit sigs, and the maker redeem
// follow from status.
func newStaleRig(t *testing.T, status order.MatchStatus, makerAddr, takerAddr string) (*testRig, *matchTracker, *tMatchSet) {
	t.Helper()
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig := tNewUnstartedRig(matchInfo)
	if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
		t.Fatalf("TrackMatches error: %v", err)
	}
	tracker := rig.getTracker()
	aged := time.Now().Add(-rig.swapper.bTimeout)
	tracker.mtx.Lock()
	tracker.Status = status
	tracker.makerSwapAddr = makerAddr
	tracker.takerSwapAddr = takerAddr
	tracker.time = aged
	if status == order.TakerSwapCast || status == order.MakerRedeemed {
		tracker.Sigs.MakerAudit = matchInfo.maker.sig
		tracker.Sigs.TakerAudit = matchInfo.taker.sig
	}
	tracker.mtx.Unlock()
	tContract := func() *asset.Contract {
		return &asset.Contract{
			Coin:         &TCoin{id: randBytes(36)},
			ContractData: encode.RandomBytes(32),
			TxData:       encode.RandomBytes(50),
		}
	}
	switch status {
	case order.MakerSwapCast, order.TakerSwapCast, order.MakerRedeemed:
		tracker.makerStatus.mtx.Lock()
		tracker.makerStatus.swap = tContract()
		tracker.makerStatus.swapTime = aged
		if status == order.MakerRedeemed {
			tracker.makerStatus.redemption = &TCoin{id: randBytes(36)}
			tracker.makerStatus.redeemTime = aged
			tracker.makerStatus.secret = encode.RandomBytes(32)
		}
		tracker.makerStatus.mtx.Unlock()
	}
	if status == order.TakerSwapCast || status == order.MakerRedeemed {
		tracker.takerStatus.mtx.Lock()
		tracker.takerStatus.swap = tContract()
		tracker.takerStatus.swapTime = aged
		tracker.takerStatus.mtx.Unlock()
	}
	return rig, tracker, set
}

func popRoutes(auth *TAuthManager, user account.AccountID) (routes []string) {
	for {
		req := auth.popReq(user)
		if req == nil {
			return
		}
		routes = append(routes, req.req.Route)
	}
}

func drainStaleComms(rig *testRig, set *tMatchSet) {
	maker, taker := set.matchInfos[0].maker.acct, set.matchInfos[0].taker.acct
	popRoutes(rig.auth, maker)
	popRoutes(rig.auth, taker)
	_ = rig.auth.getNtfn(maker, msgjson.CounterPartyAddressRoute, new(msgjson.CounterPartyAddress))
	_ = rig.auth.getNtfn(taker, msgjson.CounterPartyAddressRoute, new(msgjson.CounterPartyAddress))
}

func requireQuiet(t *testing.T, rig *testRig, set *tMatchSet) {
	t.Helper()
	maker, taker := set.matchInfos[0].maker.acct, set.matchInfos[0].taker.acct
	if got := popRoutes(rig.auth, maker); len(got) != 0 {
		t.Fatalf("maker routes = %v, want none", got)
	}
	if got := popRoutes(rig.auth, taker); len(got) != 0 {
		t.Fatalf("taker routes = %v, want none", got)
	}
	if err := rig.auth.getNtfn(maker, msgjson.CounterPartyAddressRoute, new(msgjson.CounterPartyAddress)); err == nil {
		t.Fatalf("unexpected maker CPA")
	}
	if err := rig.auth.getNtfn(taker, msgjson.CounterPartyAddressRoute, new(msgjson.CounterPartyAddress)); err == nil {
		t.Fatalf("unexpected taker CPA")
	}
}

// requireCPA requires a counterparty_address note with address want, sent
// with Send (not SendIfLocal). An empty want requires no note.
func requireCPA(t *testing.T, auth *TAuthManager, side string, user account.AccountID, want string) {
	t.Helper()
	var cpa msgjson.CounterPartyAddress
	err := auth.getNtfn(user, msgjson.CounterPartyAddressRoute, &cpa)
	if want == "" {
		if err == nil {
			t.Fatalf("unexpected %s CPA %q", side, cpa.Address)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s CPA: %v", side, err)
	}
	if cpa.Address != want {
		t.Fatalf("%s CPA addr = %q, want %q", side, cpa.Address, want)
	}
	if local, ok := auth.ntfnWasLocal(user, msgjson.CounterPartyAddressRoute); !ok || local {
		t.Fatalf("%s CPA used SendIfLocal, want Send", side)
	}
}

func TestResendStaleRequests(t *testing.T) {
	tests := []struct {
		name                       string
		status                     order.MatchStatus
		makerAddr, takerAddr       string
		wantMaker, wantTaker       []string
		wantMakerCPA, wantTakerCPA string
	}{
		{
			name:      "newly matched, taker address missing",
			status:    order.NewlyMatched,
			makerAddr: "maker-addr",
			wantTaker: []string{msgjson.MatchRoute},
		},
		{
			name:         "both addresses, newly matched, CPA to maker only",
			status:       order.NewlyMatched,
			makerAddr:    "maker-addr",
			takerAddr:    "taker-addr",
			wantMakerCPA: "taker-addr",
		},
		{
			name:         "maker swap cast, taker audit missing",
			status:       order.MakerSwapCast,
			makerAddr:    "maker-addr",
			takerAddr:    "taker-addr",
			wantTaker:    []string{msgjson.AuditRoute},
			wantTakerCPA: "maker-addr",
		},
		{
			name:      "maker redeemed, taker redeem ack missing",
			status:    order.MakerRedeemed,
			makerAddr: "maker-addr",
			takerAddr: "taker-addr",
			wantTaker: []string{msgjson.RedemptionRoute},
		},
		{
			name:      "taker swap cast, nothing pending",
			status:    order.TakerSwapCast,
			makerAddr: "maker-addr",
			takerAddr: "taker-addr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, _, set := newStaleRig(t, tt.status, tt.makerAddr, tt.takerAddr)
			maker, taker := set.matchInfos[0].maker.acct, set.matchInfos[0].taker.acct
			rig.swapper.resendStaleRequests()
			if got := popRoutes(rig.auth, maker); !slices.Equal(got, tt.wantMaker) {
				t.Fatalf("maker routes = %v, want %v", got, tt.wantMaker)
			}
			if got := popRoutes(rig.auth, taker); !slices.Equal(got, tt.wantTaker) {
				t.Fatalf("taker routes = %v, want %v", got, tt.wantTaker)
			}
			requireCPA(t, rig.auth, "maker", maker, tt.wantMakerCPA)
			requireCPA(t, rig.auth, "taker", taker, tt.wantTakerCPA)
		})
	}
}

// TestStaleSeededByOriginalSend verifies that the original match, audit,
// redemption, and counterparty_address sends stamp the last-send times, so
// the tick does not duplicate a request that just went out.
func TestStaleSeededByOriginalSend(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		status               order.MatchStatus
		makerAddr, takerAddr string
		seed                 func(*testRig, *matchTracker, *tMatchSet)
	}{
		{
			name:      "audit and CPA",
			status:    order.MakerSwapCast,
			makerAddr: "maker-addr",
			takerAddr: "taker-addr",
			seed: func(rig *testRig, tr *matchTracker, _ *tMatchSet) {
				rig.swapper.resendAuditRequest(tr, false)
				rig.swapper.sendCounterPartyAddresses(tr)
			},
		},
		{
			name:      "redemption",
			status:    order.MakerRedeemed,
			makerAddr: "maker-addr",
			takerAddr: "taker-addr",
			seed: func(rig *testRig, tr *matchTracker, _ *tMatchSet) {
				rig.swapper.resendRedemptionRequest(tr)
			},
		},
		{
			name: "match", // NewlyMatched, no addresses
			seed: func(rig *testRig, _ *matchTracker, set *tMatchSet) {
				rig.swapper.RequestMatchAcks([]*order.MatchSet{set.matchSet})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, tr, set := newStaleRig(t, tc.status, tc.makerAddr, tc.takerAddr)
			tc.seed(rig, tr, set)
			drainStaleComms(rig, set)
			rig.swapper.resendStaleRequests()
			requireQuiet(t, rig, set)
		})
	}
}

func TestCPAStampPerSide(t *testing.T) {
	tests := []struct {
		name                       string
		status                     order.MatchStatus
		churnTaker                 bool // which side reconnects before the tick
		wantMakerCPA, wantTakerCPA string
		wantTaker                  []string // routes the tick re-issues to the taker
	}{
		{
			name:         "newly matched, taker reconnect churn, CPA to maker",
			status:       order.NewlyMatched,
			churnTaker:   true,
			wantMakerCPA: "taker-addr",
		},
		{
			name:         "maker swap cast, maker reconnect churn, CPA to taker",
			status:       order.MakerSwapCast,
			wantTakerCPA: "maker-addr",
			wantTaker:    []string{msgjson.AuditRoute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, _, set := newStaleRig(t, tt.status, "maker-addr", "taker-addr")
			maker, taker := set.matchInfos[0].maker.acct, set.matchInfos[0].taker.acct

			// The waiting side reconnects; the CPA re-send must stamp only
			// that side.
			churn := maker
			if tt.churnTaker {
				churn = taker
			}
			rig.swapper.UserConnected(churn)
			drainStaleComms(rig, set)

			// The tick must still repair the to-act side.
			rig.swapper.resendStaleRequests()
			if got := popRoutes(rig.auth, maker); len(got) != 0 {
				t.Fatalf("maker routes = %v, want none", got)
			}
			if got := popRoutes(rig.auth, taker); !slices.Equal(got, tt.wantTaker) {
				t.Fatalf("taker routes = %v, want %v", got, tt.wantTaker)
			}
			requireCPA(t, rig.auth, "maker", maker, tt.wantMakerCPA)
			requireCPA(t, rig.auth, "taker", taker, tt.wantTakerCPA)

			// The tick's send stamped its recipient's side.
			rig.swapper.resendStaleRequests()
			requireQuiet(t, rig, set)
		})
	}
}

// TestDuplicateAckResponses answers a re-issued request twice; the second,
// duplicate ack must be tolerated without an error response or state change.
func TestDuplicateAckResponses(t *testing.T) {
	cases := []struct {
		name        string
		route       string
		makerRedeem bool // maker redeemed; the pending ack is the redemption's
	}{
		{name: "audit ack", route: msgjson.AuditRoute},
		{name: "redemption ack", route: msgjson.RedemptionRoute, makerRedeem: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
			matchInfo := set.matchInfos[0]
			rig := tNewUnstartedRig(matchInfo)
			if err := rig.swapper.TrackMatches([]*order.MatchSet{set.matchSet}); err != nil {
				t.Fatalf("TrackMatches error: %v", err)
			}
			taker := matchInfo.taker
			tracker := rig.getTracker()
			tracker.mtx.Lock()
			tracker.makerSwapAddr = matchInfo.makerPerMatchAddr
			tracker.takerSwapAddr = matchInfo.takerPerMatchAddr
			tracker.counterPartyAddrsSent = true
			if tc.makerRedeem {
				tracker.Sigs.MakerAudit = matchInfo.maker.sig
				tracker.Sigs.TakerAudit = taker.sig
			}
			tracker.mtx.Unlock()
			seedRecordedContracts(t, rig, matchInfo, time.Now())
			if tc.makerRedeem {
				seedMakerRedemption(rig, matchInfo, time.Now())
			}

			// Re-issue the pending request, as a startup repair would, and
			// answer it twice.
			tMesh := &tSwapMesh{applier: rig.swapper.Events()}
			rig.swapper.SetMeshService(tMesh)
			if err := rig.swapper.runStartupRepairs(context.Background()); err != nil {
				t.Fatalf("runStartupRepairs error: %v", err)
			}
			req := rig.auth.popReq(taker.acct)
			if req == nil || req.req.Route != tc.route {
				t.Fatalf("no re-issued %s request for taker", tc.route)
			}
			resp := tNewResponse(req.req.ID, tAck(taker, matchInfo.matchID))
			req.respFunc(nil, resp)
			if msg, r := rig.auth.popResp(taker.acct); msg != nil {
				t.Fatalf("error response to first ack: %+v", r)
			}
			req.respFunc(nil, resp) // the duplicate
			if msg, r := rig.auth.popResp(taker.acct); msg != nil {
				t.Fatalf("error response to duplicate ack: %+v", r)
			}

			tracker.mtx.RLock()
			sig := tracker.Sigs.TakerAudit
			if tc.route == msgjson.RedemptionRoute {
				sig = tracker.Sigs.TakerRedeem
			}
			tracker.mtx.RUnlock()
			if !bytes.Equal(sig, taker.sig) {
				t.Fatalf("taker %s ack sig not recorded after duplicate", tc.route)
			}
		})
	}
}

func TestDeleteMatchCleansUpCoinIDs(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	tracker := rig.getTracker()

	// Directly register CoinIDs in the dedup maps to simulate what
	// processInit does, avoiding the async coin waiter complexity.
	fakeCoinID1 := "aabbccdd"
	fakeCoinID2 := "eeff0011"
	mid := matchInfo.matchID

	rig.swapper.activeCoinsMtx.Lock()
	rig.swapper.activeCoinIDs[fakeCoinID1] = mid
	rig.swapper.activeCoinIDs[fakeCoinID2] = mid
	rig.swapper.matchCoinIDs[mid] = []string{fakeCoinID1, fakeCoinID2}
	rig.swapper.activeCoinsMtx.Unlock()

	// Verify they're registered.
	rig.swapper.activeCoinsMtx.Lock()
	if _, exists := rig.swapper.activeCoinIDs[fakeCoinID1]; !exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("CoinID1 not registered")
	}
	rig.swapper.activeCoinsMtx.Unlock()

	// Delete the match and verify cleanup. deleteMatch requires matchMtx.
	rig.swapper.matchMtx.Lock()
	rig.swapper.deleteMatch(tracker)
	rig.swapper.matchMtx.Unlock()

	rig.swapper.activeCoinsMtx.Lock()
	if _, exists := rig.swapper.activeCoinIDs[fakeCoinID1]; exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("CoinID1 not cleaned up after deleteMatch")
	}
	if _, exists := rig.swapper.activeCoinIDs[fakeCoinID2]; exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("CoinID2 not cleaned up after deleteMatch")
	}
	if _, exists := rig.swapper.matchCoinIDs[mid]; exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("matchCoinIDs entry not cleaned up after deleteMatch")
	}
	rig.swapper.activeCoinsMtx.Unlock()
}

func TestSecretHashDedup(t *testing.T) {
	// A malicious maker reusing the same secret hash across two matches
	// should be rejected by the server.
	qty := uint64(1e8)
	rate := uint64(1e8)

	maker1, taker1 := tNewUser("maker1"), tNewUser("taker1")
	makerOrder1 := makeLimitOrder(qty, rate, maker1, true)
	takerOrder1 := makeLimitOrder(qty, rate, taker1, false)
	matchInfo1 := tMatchInfo(maker1, taker1, qty, rate, makerOrder1, takerOrder1)
	set1 := new(tMatchSet).add(matchInfo1)

	maker2, taker2 := tNewUser("maker2"), tNewUser("taker2")
	makerOrder2 := makeLimitOrder(qty, rate, maker2, true)
	takerOrder2 := makeLimitOrder(qty, rate, taker2, false)
	matchInfo2 := tMatchInfo(maker2, taker2, qty, rate, makerOrder2, takerOrder2)
	set2 := new(tMatchSet).add(matchInfo2)

	rig, cleanup := tNewTestRig(matchInfo1)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 2)
	rig.auth.auditReq = make(chan struct{}, 2)

	rig.applyMatchesAndRequestAcks(t, set1.matchSet)
	rig.applyMatchesAndRequestAcks(t, set2.matchSet)

	// Ack both matches.
	rig.matchInfo = matchInfo1
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("match1 maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("match1 taker ack: %v", err)
	}
	rig.matchInfo = matchInfo2
	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("match2 maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("match2 taker ack: %v", err)
	}

	// Send swap for match 1.
	rig.matchInfo = matchInfo1
	swap1 := tNewSwap(matchInfo1, matchInfo1.makerOID, matchInfo1.takerPerMatchAddr, matchInfo1.maker)
	rig.abcNode.setContract(swap1.coin, false)
	rig.auth.swapID = swap1.req.ID
	rpcErr := rig.swapper.handleInit(matchInfo1.maker.acct, swap1.req)
	if rpcErr != nil {
		t.Fatalf("match1 swap init failed: %v", rpcErr.Message)
	}
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for match1 swap response")
	}
	if err := rig.checkServerResponseSuccess(matchInfo1.maker); err != nil {
		t.Fatalf("match1 swap should succeed: %v", err)
	}

	// Send swap for match 2 with the same secret hash as match 1.
	rig.matchInfo = matchInfo2
	swap2 := tNewSwap(matchInfo2, matchInfo2.makerOID, matchInfo2.takerPerMatchAddr, matchInfo2.maker)
	// Override the secret hash on the contract stored in the backend.
	swap2.coin.SecretHash = swap1.coin.SecretHash
	rig.abcNode.setContract(swap2.coin, false)
	rig.auth.swapID = swap2.req.ID
	rpcErr = rig.swapper.handleInit(matchInfo2.maker.acct, swap2.req)
	if rpcErr != nil {
		// Synchronous rejection.
		if !strings.Contains(rpcErr.Message, "already in use") {
			t.Fatalf("expected 'already in use' error, got: %s", rpcErr.Message)
		}
		return
	}

	timeOutMempool()
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for match2 swap response")
	}
	if err := rig.checkServerResponseFail(matchInfo2.maker, msgjson.ContractError, "already in use"); err != nil {
		t.Fatalf("expected secret hash reuse error: %v", err)
	}
}

func TestSecretHashDedupSameMatch(t *testing.T) {
	// Same secret hash retried for the same match should not be rejected
	// by the dedup check.
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.auth.swapReceived = make(chan struct{}, 2)
	rig.auth.auditReq = make(chan struct{}, 2)

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	if err := rig.ackMatch_maker(true); err != nil {
		t.Fatalf("maker ack: %v", err)
	}
	if err := rig.ackMatch_taker(true); err != nil {
		t.Fatalf("taker ack: %v", err)
	}

	// First init should succeed.
	swap := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.takerPerMatchAddr, matchInfo.maker)
	rig.abcNode.setContract(swap.coin, false)
	rig.auth.swapID = swap.req.ID
	rpcErr := rig.swapper.handleInit(matchInfo.maker.acct, swap.req)
	if rpcErr != nil {
		t.Fatalf("first init failed: %v", rpcErr.Message)
	}
	select {
	case <-rig.auth.swapReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first swap response")
	}
	if err := rig.checkServerResponseSuccess(matchInfo.maker); err != nil {
		t.Fatalf("first init should succeed: %v", err)
	}

	// Retry same match with same secret hash - dedup should not reject.
	swap2 := tNewSwap(matchInfo, matchInfo.makerOID, matchInfo.takerPerMatchAddr, matchInfo.maker)
	swap2.coin.SecretHash = swap.coin.SecretHash
	var init2 msgjson.Init
	swap2.req.Unmarshal(&init2)
	init2.CoinID = swap.coin.ID()
	swap2.req, _ = msgjson.NewRequest(swap2.req.ID, msgjson.InitRoute, &init2)
	rpcErr = rig.swapper.handleInit(matchInfo.maker.acct, swap2.req)
	if rpcErr == nil {
		t.Fatal("expected error for duplicate init on already-swapped match")
	}
	if strings.Contains(rpcErr.Message, "already in use") {
		t.Fatal("secret hash dedup should not reject same-match retries")
	}
}

func TestDeleteMatchCleansUpSecretHashes(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]
	rig, cleanup := tNewTestRig(matchInfo)
	defer cleanup()

	rig.applyMatchesAndRequestAcks(t, set.matchSet)

	tracker := rig.getTracker()
	mid := matchInfo.matchID

	// Register secret hashes in the dedup maps.
	fakeHash := "abcdef0123456789"
	rig.swapper.activeCoinsMtx.Lock()
	rig.swapper.activeSecretHashes[fakeHash] = mid
	rig.swapper.matchSecretHashes[mid] = []string{fakeHash}
	rig.swapper.activeCoinsMtx.Unlock()

	// Delete the match.
	rig.swapper.matchMtx.Lock()
	rig.swapper.deleteMatch(tracker)
	rig.swapper.matchMtx.Unlock()

	// Verify cleanup.
	rig.swapper.activeCoinsMtx.Lock()
	if _, exists := rig.swapper.activeSecretHashes[fakeHash]; exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("secret hash not cleaned up after deleteMatch")
	}
	if _, exists := rig.swapper.matchSecretHashes[mid]; exists {
		rig.swapper.activeCoinsMtx.Unlock()
		t.Fatal("matchSecretHashes entry not cleaned up after deleteMatch")
	}
	rig.swapper.activeCoinsMtx.Unlock()
}

// tSeedShapedStorage populates the storage stub the way a snapshot-seeded (or
// crash-restarted) database looks for an active match: an active match row at
// the given status with the given recorded per-match addresses (empty when
// never acked), plus the order rows the restore must load.
func tSeedShapedStorage(matchInfo *tMatch, status order.MatchStatus, makerAddr, takerAddr string) func(*TStorage) {
	match := matchInfo.match
	return func(ts *TStorage) {
		ts.orders = map[order.OrderID]order.Order{
			matchInfo.makerOID: match.Maker,
			matchInfo.takerOID: match.Taker,
		}
		ts.activeSwaps = []*db.SwapDataFull{{
			Base:  ABCID,
			Quote: XYZID,
			MatchData: &db.MatchData{
				ID:            matchInfo.matchID,
				Taker:         matchInfo.takerOID,
				TakerAcct:     matchInfo.taker.acct,
				TakerAddr:     matchInfo.taker.addr,
				TakerSell:     !match.Maker.T.Sell,
				Maker:         matchInfo.makerOID,
				MakerAcct:     matchInfo.maker.acct,
				MakerAddr:     matchInfo.maker.addr,
				Epoch:         match.Epoch,
				Quantity:      match.Quantity,
				Rate:          match.Rate,
				BaseRate:      match.FeeRateBase,
				QuoteRate:     match.FeeRateQuote,
				Active:        true,
				Status:        status,
				MakerSwapAddr: makerAddr,
				TakerSwapAddr: takerAddr,
			},
			SwapData: &db.SwapData{
				MakerSwapAddr: makerAddr,
				TakerSwapAddr: takerAddr,
			},
		}}
	}
}

// TestRestoreActiveSwaps checks storage to memory: every active match row
// restores a tracker, including one with no swap data yet — the mesh catch-up
// livelock regression, where the missing tracker made every post-anchor
// replicated event for the match fail with "unknown match".
func TestRestoreActiveSwaps(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]

	cases := []struct {
		name      string
		status    order.MatchStatus
		makerAddr string
		takerAddr string
	}{
		{name: "newly matched without swap data", status: order.NewlyMatched},
		{name: "half acked", status: order.NewlyMatched, makerAddr: "maker-addr"},
		{name: "maker swap cast fully acked", status: order.MakerSwapCast, makerAddr: "maker-addr", takerAddr: "taker-addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := tNewUnstartedRigWithStorage(matchInfo, tSeedShapedStorage(matchInfo, tc.status, tc.makerAddr, tc.takerAddr))
			tracker := rig.getTracker()
			if tracker == nil {
				t.Fatalf("no tracker restored for the active match")
			}
			tracker.mtx.RLock()
			status, makerAddr, takerAddr := tracker.Status, tracker.makerSwapAddr, tracker.takerSwapAddr
			tracker.mtx.RUnlock()
			if status != tc.status || makerAddr != tc.makerAddr || takerAddr != tc.takerAddr {
				t.Fatalf("restored tracker = %v %q/%q, want %v %q/%q",
					status, makerAddr, takerAddr, tc.status, tc.makerAddr, tc.takerAddr)
			}
		})
	}
}

// TestRestoreActiveSwapsStrict verifies that a load failure for any active
// match fails the restore instead of silently dropping the tracker.
func TestRestoreActiveSwapsStrict(t *testing.T) {
	set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
	matchInfo := set.matchInfos[0]

	cases := []struct {
		name    string
		corrupt func(*TStorage)
	}{
		{name: "missing taker order", corrupt: func(ts *TStorage) {
			delete(ts.orders, matchInfo.takerOID)
		}},
		{name: "missing maker order", corrupt: func(ts *TStorage) {
			delete(ts.orders, matchInfo.makerOID)
		}},
		{name: "taker order ID mismatch", corrupt: func(ts *TStorage) {
			ts.orders[matchInfo.takerOID] = matchInfo.match.Maker
		}},
		{name: "maker order ID mismatch", corrupt: func(ts *TStorage) {
			ts.orders[matchInfo.makerOID] = matchInfo.match.Taker
		}},
		{name: "corrupt match row", corrupt: func(ts *TStorage) {
			ts.activeSwaps[0].MatchData.Quantity++
		}},
		{name: "missing swap contract", corrupt: func(ts *TStorage) {
			ts.activeSwaps[0].SwapData.ContractACoinID = []byte{0xde, 0xad}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := tBuildUnstartedRig(matchInfo, func(ts *TStorage) {
				tSeedShapedStorage(matchInfo, order.MakerSwapCast, "maker-addr", "taker-addr")(ts)
				tc.corrupt(ts)
			})
			if err := rig.swapper.RestoreActiveSwaps(false); err == nil {
				t.Fatalf("restore succeeded with corrupt storage")
			}
		})
	}
}

func TestRestoreActiveSwapsCoinLocks(t *testing.T) {
	t.Run("seeds both orders", func(t *testing.T) {
		maker, taker := tNewUser("maker"), tNewUser("taker")
		makerOrder := makeLimitOrder(uint64(1e8), 5e7, maker, true)
		makerOrder.T.Coins = []order.CoinID{randBytes(36), randBytes(36)}
		takerOrder := makeLimitOrder(uint64(1e8), 5e7, taker, false)
		takerOrder.T.Coins = []order.CoinID{randBytes(36)}
		matchInfo := tMatchInfo(maker, taker, uint64(1e8), 5e7, makerOrder, takerOrder)

		rig := tNewUnstartedRigWithStorage(matchInfo,
			tSeedShapedStorage(matchInfo, order.MakerSwapCast, "maker-addr", "taker-addr"))
		swapper := rig.swapper

		for _, coin := range makerOrder.Trade().Coins {
			if !swapper.coins[ABCID].Locker.CoinLocked(coin) {
				t.Fatalf("maker funding coin not locked after restore")
			}
		}
		for _, coin := range takerOrder.Trade().Coins {
			if !swapper.coins[XYZID].Locker.CoinLocked(coin) {
				t.Fatalf("taker funding coin not locked after restore")
			}
		}
	})

	t.Run("conflict fails restore", func(t *testing.T) {
		sharedCoin := order.CoinID(randBytes(36))
		newMatch := func(tag string, qty uint64) *tMatch {
			maker, taker := tNewUser("maker"+tag), tNewUser("taker"+tag)
			makerOrder := makeLimitOrder(qty, 5e7, maker, true)
			makerOrder.T.Coins = []order.CoinID{sharedCoin}
			takerOrder := makeLimitOrder(qty, 5e7, taker, false)
			takerOrder.T.Coins = []order.CoinID{randBytes(36)}
			return tMatchInfo(maker, taker, qty, 5e7, makerOrder, takerOrder)
		}
		mi1, mi2 := newMatch("1", uint64(1e8)), newMatch("2", uint64(2e8))

		rig := tBuildUnstartedRig(mi1, func(ts *TStorage) {
			tSeedShapedStorage(mi1, order.MakerSwapCast, "maker-addr", "taker-addr")(ts)
			var ts2 TStorage
			tSeedShapedStorage(mi2, order.MakerSwapCast, "maker-addr", "taker-addr")(&ts2)
			ts.activeSwaps = append(ts.activeSwaps, ts2.activeSwaps...)
			for oid, ord := range ts2.orders {
				ts.orders[oid] = ord
			}
		})
		if err := rig.swapper.RestoreActiveSwaps(false); err == nil {
			t.Fatalf("restore succeeded with two orders claiming one funding coin")
		}
	})
}

// TestProcessBlockScansAllMatches verifies that a block notification updates
// every match on the block asset even when the scan also holds matches on
// unrelated assets. The decoy count makes an aborted scan fail the test with
// near certainty regardless of map iteration order.
func TestProcessBlockScansAllMatches(t *testing.T) {
	set := tMultiMatchSet([]uint64{1e8, 2e8, 3e8}, []uint64{5e7, 5e7, 5e7}, true, false)
	rig := tNewUnstartedRig(set.matchInfos[0])
	swapper := rig.swapper
	now := time.Now().UTC()

	// Confirmable matches: maker swap cast on the abc side with SwapConf confs.
	trackers := make([]*matchTracker, 0, len(set.matchInfos))
	for _, mi := range set.matchInfos {
		mi.match.Status = order.MakerSwapCast
		mt := &matchTracker{
			Match:     mi.match,
			time:      now,
			matchTime: now,
			makerStatus: &swapStatus{
				swapAsset:   ABCID,
				redeemAsset: XYZID,
				swap: &asset.Contract{
					Coin: &TCoin{id: randBytes(36), confs: int64(rig.abc.SwapConf)},
				},
				swapTime: now,
			},
			takerStatus: &swapStatus{swapAsset: XYZID, redeemAsset: ABCID},
		}
		swapper.addMatch(mt)
		trackers = append(trackers, mt)
	}

	// Decoy matches touching neither side of the block asset.
	base := set.matchInfos[0].match
	for i := 0; i < 12; i++ {
		decoy := &order.Match{
			Maker:    base.Maker,
			Taker:    base.Taker,
			Quantity: base.Quantity + uint64(i+1),
			Rate:     base.Rate,
			Epoch:    base.Epoch,
			Status:   order.NewlyMatched,
		}
		swapper.addMatch(&matchTracker{
			Match:       decoy,
			time:        now,
			matchTime:   now,
			makerStatus: &swapStatus{swapAsset: XYZID, redeemAsset: ACCTID},
			takerStatus: &swapStatus{swapAsset: ACCTID, redeemAsset: XYZID},
		})
	}

	swapper.processBlock(context.Background(), &blockNotification{time: now, assetID: ABCID})

	for i, mt := range trackers {
		if mt.makerStatus.swapConfTime().IsZero() {
			t.Fatalf("match %d not confirmed by the block scan", i)
		}
	}
}

func TestUnlocksAreEventDriven(t *testing.T) {
	t.Run("confirm does not unlock, delete does", func(t *testing.T) {
		set := tPerfectLimitLimit(uint64(1e8), uint64(1e8), true)
		mi := set.matchInfos[0]
		rig := tNewUnstartedRig(mi)
		swapper := rig.swapper
		now := time.Now().UTC()

		maker := mi.match.Maker
		maker.T.Coins = []order.CoinID{randBytes(36), randBytes(36)}
		locker := swapper.coins[ABCID].Locker
		if failed := locker.LockOrdersCoins([]order.Order{maker}); len(failed) > 0 {
			t.Fatalf("failed to lock maker order coins")
		}
		for _, coin := range maker.Trade().Coins {
			if !locker.CoinLocked(coin) {
				t.Fatalf("maker order coins not locked after LockOrdersCoins")
			}
		}

		mi.match.Status = order.MakerSwapCast
		mt := &matchTracker{
			Match:     mi.match,
			time:      now,
			matchTime: now,
			makerStatus: &swapStatus{
				swapAsset:   ABCID,
				redeemAsset: XYZID,
				swap: &asset.Contract{
					Coin: &TCoin{id: randBytes(36), confs: int64(rig.abc.SwapConf)},
				},
				swapTime: now,
			},
			takerStatus: &swapStatus{swapAsset: XYZID, redeemAsset: ABCID},
		}
		swapper.addMatch(mt)

		swapper.processBlock(context.Background(), &blockNotification{time: now, assetID: ABCID})
		if mt.makerStatus.swapConfTime().IsZero() {
			t.Fatalf("swap not confirmed by the block")
		}
		for _, coin := range maker.Trade().Coins {
			if !locker.CoinLocked(coin) {
				t.Fatalf("chain-driven confirmation unlocked maker order coins")
			}
		}

		swapper.deleteMatch(mt)
		for _, coin := range maker.Trade().Coins {
			if locker.CoinLocked(coin) {
				t.Fatalf("match deletion did not unlock maker order coins")
			}
		}
	})

	t.Run("shared order stays locked until last match", func(t *testing.T) {
		set := tMultiMatchSet([]uint64{1e8, 2e8}, []uint64{5e7, 5e7}, true, false)
		rig := tNewUnstartedRig(set.matchInfos[0])
		swapper := rig.swapper
		now := time.Now().UTC()

		takerOrd := set.matchInfos[0].match.Taker
		takerOrd.Trade().Coins = []order.CoinID{randBytes(36)}

		trackers := make([]*matchTracker, 0, 2)
		for _, mi := range set.matchInfos {
			mt := &matchTracker{
				Match:       mi.match,
				time:        now,
				matchTime:   now,
				makerStatus: &swapStatus{swapAsset: ABCID, redeemAsset: XYZID},
				takerStatus: &swapStatus{swapAsset: XYZID, redeemAsset: ABCID},
			}
			swapper.addMatch(mt)
			trackers = append(trackers, mt)
		}
		swapper.LockOrdersCoins([]order.Order{takerOrd})

		locker := swapper.coins[XYZID].Locker // buy-side taker funds with the quote asset
		deleteTracker := func(mt *matchTracker) {
			swapper.matchMtx.Lock()
			swapper.deleteMatch(mt)
			swapper.matchMtx.Unlock()
		}

		deleteTracker(trackers[0])
		for _, coin := range takerOrd.Trade().Coins {
			if !locker.CoinLocked(coin) {
				t.Fatalf("shared taker order unlocked while another active match references it")
			}
		}

		deleteTracker(trackers[1])
		for _, coin := range takerOrd.Trade().Coins {
			if locker.CoinLocked(coin) {
				t.Fatalf("taker order still locked after its last active match was deleted")
			}
		}
	})
}
