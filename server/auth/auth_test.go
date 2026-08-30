// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	ordertest "decred.org/dcrdex/dex/order/test"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func noop() {}

type emptyEventLogReader struct{}

func (emptyEventLogReader) EventLogFrontier(context.Context) (*db.EventLogPosition, error) {
	return &db.EventLogPosition{}, nil
}

func (emptyEventLogReader) EventLogEntriesAfter(context.Context, uint64, int) ([]*db.EventLogEntry, error) {
	return nil, nil
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	rand.Read(b)
	return b
}

var randomMatchID = ordertest.RandomMatchID

type ratioData struct {
	oidsCompleted  []order.OrderID
	timesCompleted []int64
	oidsCancels    []order.OrderID
	oidsCanceled   []order.OrderID
	timesCanceled  []int64
	epochGaps      []int32
}

// TStorage satisfies the Storage interface
type TStorage struct {
	acctInfo                  *db.Account
	acctInfoErr               error
	acct                      *account.Account
	matches                   []*db.MatchData
	matchStatuses             []*db.MatchStatus
	userMatchOutcomes         []*db.MatchOutcome
	reputationPreimages       []*db.PreimageOutcome
	reputationMatches         []*db.MatchResult
	reputationOrders          []*db.OrderOutcome
	reputationErr             error
	getUserReputationData     func(context.Context, account.AccountID, int, int, int) ([]*db.PreimageOutcome, []*db.MatchResult, []*db.OrderOutcome, error)
	orderStatuses             []*db.OrderStatus
	acctErr                   error
	accountReadErr            error
	regAddr                   string
	regAsset                  uint32
	bonds                     []*db.Bond
	applyBondPosted           func(context.Context, *db.EventLogMeta, *db.BondPostedUpdate) (*db.BondPostedResult, error)
	bondPostedMeta            *db.EventLogMeta
	bondPostedUpdate          *db.BondPostedUpdate
	prepaidBonds              map[string]*meshevents.PrepaidBond
	applyPrepaidBondsCreated  func(context.Context, *db.EventLogMeta, *meshevents.PrepaidBondsCreatedEvent) (*db.EventLogEntry, error)
	prepaidBondsCreatedMeta   *db.EventLogMeta
	prepaidBondsCreatedUpdate *meshevents.PrepaidBondsCreatedEvent
	applyReputationForgiven   func(context.Context, *db.EventLogMeta, *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error)
	reputationForgivenMeta    *db.EventLogMeta
	reputationForgivenUpdate  *meshevents.ReputationForgivenEvent
	repInputsListener         func(users ...account.AccountID)
	ratio                     ratioData
}

func (s *TStorage) SetReputationInputsListener(listener func(users ...account.AccountID)) {
	s.repInputsListener = listener
}

func (s *TStorage) notifyRepInputs(users ...account.AccountID) {
	if s.repInputsListener != nil {
		s.repInputsListener(users...)
	}
}

func (s *TStorage) AccountInfo(account.AccountID) (*db.Account, error) {
	return s.acctInfo, s.acctInfoErr
}
func (s *TStorage) Account(acct account.AccountID, lockTimeThresh time.Time) (*account.Account, []*db.Bond, error) {
	if s.accountReadErr != nil {
		return nil, nil, s.accountReadErr
	}
	// Mirror the DB query: only bonds at or past the lock-time threshold.
	bonds := make([]*db.Bond, 0, len(s.bonds))
	for _, bond := range s.bonds {
		if bond.LockTime >= lockTimeThresh.Unix() {
			bonds = append(bonds, bond)
		}
	}
	return s.acct, bonds, nil
}
func (s *TStorage) setBondTier(tier uint32) {
	s.bonds = []*db.Bond{{Strength: tier, LockTime: time.Now().Unix() * 2}}
}
func (s *TStorage) CreateAccountWithBond(acct *account.Account, bond *db.Bond) error {
	s.acct = acct
	if acct != nil && acct.PubKey != nil {
		s.acctInfo = &db.Account{
			AccountID: acct.ID,
			Pubkey:    acct.PubKey.SerializeCompressed(),
		}
	}
	if bond != nil {
		s.AddBond(acct.ID, bond)
	}
	return nil
}
func (s *TStorage) AddBond(acct account.AccountID, bond *db.Bond) error {
	if bond == nil {
		return nil
	}
	for _, existing := range s.bonds {
		if existing.AssetID == bond.AssetID && bytes.Equal(existing.CoinID, bond.CoinID) {
			return nil
		}
	}
	s.bonds = append(s.bonds, bond)
	return nil
}
func (s *TStorage) ApplyBondPostedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.BondPostedUpdate) (*db.BondPostedResult, error) {
	s.bondPostedMeta = meta
	s.bondPostedUpdate = update
	var result *db.BondPostedResult
	var err error
	if s.applyBondPosted != nil {
		result, err = s.applyBondPosted(ctx, meta, update)
	} else {
		result = &db.BondPostedResult{
			BondAdded: true,
			Log:       testBondPostedLog(meta, update),
		}
	}
	if err != nil || result == nil {
		return result, err
	}
	if result.BondAdded {
		if err := s.CreateAccountWithBond(update.Acct, update.Bond); err != nil {
			return result, err
		}
		if update.Bond.AssetID == account.PrepaidBondID && s.prepaidBonds != nil {
			delete(s.prepaidBonds, string(update.Bond.CoinID))
		}
	}
	s.notifyRepInputs(update.Acct.ID) // committed apply, including duplicate-bond replay
	return result, nil
}

func testBondPostedLog(meta *db.EventLogMeta, update *db.BondPostedUpdate) *db.EventLogEntry {
	if update == nil || meta == nil {
		return nil
	}
	seq := meta.Seq
	if seq == 0 {
		seq = 1
	}
	tipHash := append([]byte(nil), meta.ExpectedTipHash...)
	if len(tipHash) == 0 {
		tipHash = make([]byte, db.EventLogTipHashSize)
		tipHash[len(tipHash)-1] = byte(seq)
	}
	txData, _ := update.EventTxData()
	return &db.EventLogEntry{
		Seq:     seq,
		Kind:    meshevents.EventKindBondPosted,
		Event:   append([]byte(nil), meta.Event...),
		TxData:  txData,
		TipHash: tipHash,
	}
}
func (s *TStorage) ApplyReputationForgivenEvent(ctx context.Context, meta *db.EventLogMeta, update *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
	s.reputationForgivenMeta = meta
	s.reputationForgivenUpdate = update
	if s.applyReputationForgiven != nil {
		result, err := s.applyReputationForgiven(ctx, meta, update)
		if err == nil || errors.As(err, new(*db.EventCommitUnknownError)) {
			s.notifyRepInputs(update.AccountID)
		}
		return result, err
	}
	s.notifyRepInputs(update.AccountID)
	return &db.ReputationForgivenResult{
		Forgiven: true,
		Log:      testReputationForgivenLog(meta, update),
	}, nil
}
func testReputationForgivenLog(meta *db.EventLogMeta, update *meshevents.ReputationForgivenEvent) *db.EventLogEntry {
	if update == nil || meta == nil {
		return nil
	}
	seq := meta.Seq
	if seq == 0 {
		seq = 1
	}
	tipHash := append([]byte(nil), meta.ExpectedTipHash...)
	if len(tipHash) == 0 {
		tipHash = make([]byte, db.EventLogTipHashSize)
		tipHash[len(tipHash)-1] = byte(seq)
	}
	txData, _ := update.EventTxData()
	return &db.EventLogEntry{
		Seq:     seq,
		Kind:    meshevents.EventKindReputationForgiven,
		Event:   append([]byte(nil), meta.Event...),
		TxData:  txData,
		TipHash: tipHash,
	}
}
func (s *TStorage) FetchPrepaidBond(coinID []byte) (uint32, int64, error) {
	if s.prepaidBonds != nil {
		bond := s.prepaidBonds[string(coinID)]
		if bond == nil {
			return 0, 0, fmt.Errorf("pre-paid bond not found")
		}
		return bond.Strength, bond.LockTime, nil
	}
	return 1, time.Now().Add(time.Hour * 48).Unix(), nil
}
func (s *TStorage) ApplyPrepaidBondsCreatedEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.PrepaidBondsCreatedEvent) (*db.EventLogEntry, error) {
	s.prepaidBondsCreatedMeta = meta
	s.prepaidBondsCreatedUpdate = event
	if s.applyPrepaidBondsCreated != nil {
		return s.applyPrepaidBondsCreated(ctx, meta, event)
	}
	if s.prepaidBonds == nil {
		s.prepaidBonds = make(map[string]*meshevents.PrepaidBond)
	}
	for _, bond := range event.Bonds {
		cpy := *bond
		cpy.CoinID = append([]byte(nil), bond.CoinID...)
		s.prepaidBonds[string(cpy.CoinID)] = &cpy
	}
	return testPrepaidBondsCreatedLog(meta, event), nil
}
func testPrepaidBondsCreatedLog(meta *db.EventLogMeta, event *meshevents.PrepaidBondsCreatedEvent) *db.EventLogEntry {
	if event == nil || meta == nil {
		return nil
	}
	seq := meta.Seq
	if seq == 0 {
		seq = 1
	}
	tipHash := append([]byte(nil), meta.ExpectedTipHash...)
	if len(tipHash) == 0 {
		tipHash = make([]byte, db.EventLogTipHashSize)
		tipHash[len(tipHash)-1] = byte(seq)
	}
	txData, _ := event.EventTxData()
	return &db.EventLogEntry{
		Seq:     seq,
		Kind:    meshevents.EventKindPrepaidBondsCreated,
		Event:   append([]byte(nil), meta.Event...),
		TxData:  txData,
		TipHash: tipHash,
	}
}
func (s *TStorage) CompletedAndAtFaultMatchStats(aid account.AccountID, lastN int) ([]*db.MatchOutcome, error) {
	return s.userMatchOutcomes, nil
}
func (s *TStorage) UserMatchFails(aid account.AccountID, lastN int) ([]*db.MatchFail, error) {
	return nil, nil
}
func (s *TStorage) UserOrderStatuses(aid account.AccountID, base, quote uint32, oids []order.OrderID) ([]*db.OrderStatus, error) {
	return s.orderStatuses, nil
}
func (s *TStorage) ActiveUserOrderStatuses(aid account.AccountID) ([]*db.OrderStatus, error) {
	var activeOrderStatuses []*db.OrderStatus
	for _, orderStatus := range s.orderStatuses {
		if orderStatus.Status == order.OrderStatusEpoch || orderStatus.Status == order.OrderStatusBooked {
			activeOrderStatuses = append(activeOrderStatuses, orderStatus)
		}
	}
	return activeOrderStatuses, nil
}
func (s *TStorage) AllActiveUserMatches(account.AccountID) ([]*db.MatchData, error) {
	return s.matches, nil
}
func (s *TStorage) MatchStatuses(aid account.AccountID, base, quote uint32, matchIDs []order.MatchID) ([]*db.MatchStatus, error) {
	return s.matchStatuses, nil
}
func (s *TStorage) CreateAccount(acct *account.Account, assetID uint32, addr string) error {
	s.regAddr = addr
	s.regAsset = assetID
	return s.acctErr
}
func (s *TStorage) setRatioData(dat *ratioData) {
	s.ratio = *dat
}

func (s *TStorage) GetUserReputationData(ctx context.Context, user account.AccountID, pimgSz, matchSz, orderSz int) ([]*db.PreimageOutcome, []*db.MatchResult, []*db.OrderOutcome, error) {
	if s.getUserReputationData != nil {
		return s.getUserReputationData(ctx, user, pimgSz, matchSz, orderSz)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
	}
	if s.reputationErr != nil {
		return nil, nil, nil, s.reputationErr
	}
	pimgs := append([]*db.PreimageOutcome(nil), s.reputationPreimages...)
	matches := append([]*db.MatchResult(nil), s.reputationMatches...)
	ords := append([]*db.OrderOutcome(nil), s.reputationOrders...)
	if len(pimgs) > pimgSz {
		pimgs = pimgs[len(pimgs)-pimgSz:]
	}
	if len(matches) > matchSz {
		matches = matches[len(matches)-matchSz:]
	}
	if len(ords) > orderSz {
		ords = ords[len(ords)-orderSz:]
	}
	return pimgs, matches, ords, nil
}

// TSigner satisfies the Signer interface
type TSigner struct {
	mtx sync.Mutex
	sig *ecdsa.Signature
	//privKey *secp256k1.PrivateKey
	pubkey *secp256k1.PublicKey
}

// tDefaultSig backs any TSigner whose test has not set a signature: the
// reputation-inputs hook signs score notes from its own goroutine, so Sign
// must never return nil and must not race the tests' setSig calls.
var tDefaultSig = func() *ecdsa.Signature {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		panic(err)
	}
	return ecdsa.Sign(priv, randBytes(32))
}()

// Maybe actually change this to an ecdsa.Sign with a private key instead?
func (s *TSigner) Sign(hash []byte) *ecdsa.Signature {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.sig == nil {
		return tDefaultSig
	}
	return s.sig
}

func (s *TSigner) setSig(sig *ecdsa.Signature) {
	s.mtx.Lock()
	s.sig = sig
	s.mtx.Unlock()
}

func (s *TSigner) PubKey() *secp256k1.PublicKey { return s.pubkey }

type tReq struct {
	msg      *msgjson.Message
	respFunc func(comms.Link, *msgjson.Message)
}

// tRPCClient satisfies the comms.Link interface.
type TRPCClient struct {
	id         uint64
	ip         dex.IPKey
	addr       string
	sendErr    error
	sendRawErr error
	requestErr error
	banished   bool
	// mtx guards sends and reqs: the reputation-inputs hook sends from its
	// own goroutine while tests poll getSend/getReq.
	mtx    sync.Mutex
	sends  []*msgjson.Message
	reqs   []*tReq
	on     uint32
	closed chan struct{}
}

func (c *TRPCClient) ID() uint64    { return c.id }
func (c *TRPCClient) IP() dex.IPKey { return c.ip }
func (c *TRPCClient) Addr() string  { return c.addr }
func (c *TRPCClient) Authorized()   {}
func (c *TRPCClient) Send(msg *msgjson.Message) error {
	c.mtx.Lock()
	c.sends = append(c.sends, msg)
	c.mtx.Unlock()
	return c.sendErr
}
func (c *TRPCClient) SendRaw(b []byte) error {
	if c.sendRawErr != nil {
		return c.sendRawErr
	}
	msg, err := msgjson.DecodeMessage(b)
	if err != nil {
		return err
	}
	c.mtx.Lock()
	c.sends = append(c.sends, msg)
	c.mtx.Unlock()
	return nil
}
func (c *TRPCClient) SendError(id uint64, msg *msgjson.Error) {
}
func (c *TRPCClient) Request(msg *msgjson.Message, f func(comms.Link, *msgjson.Message), _ time.Duration, _ func()) error {
	c.mtx.Lock()
	c.reqs = append(c.reqs, &tReq{
		msg:      msg,
		respFunc: f,
	})
	c.mtx.Unlock()
	return c.requestErr
}
func (c *TRPCClient) RequestRaw(msgID uint64, rawMsg []byte, f func(comms.Link, *msgjson.Message), expireTime time.Duration, expire func()) error {
	return nil
}

func (c *TRPCClient) Done() <-chan struct{} {
	return c.closed
}
func (c *TRPCClient) Disconnect() {
	if atomic.CompareAndSwapUint32(&c.on, 0, 1) {
		close(c.closed)
	}
}
func (c *TRPCClient) Banish() { c.banished = true }
func (c *TRPCClient) getReq() *tReq {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if len(c.reqs) == 0 {
		return nil
	}
	req := c.reqs[0]
	c.reqs = c.reqs[1:]
	return req
}
func (c *TRPCClient) getSend() *msgjson.Message {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if len(c.sends) == 0 {
		return nil
	}
	msg := c.sends[0]
	c.sends = c.sends[1:]
	return msg
}
func (c *TRPCClient) sendCount() int {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return len(c.sends)
}

func (c *TRPCClient) CustomID() string {
	return ""
}

func (c *TRPCClient) SetCustomID(string) {}

var tClientID uint64

func tNewRPCClient() *TRPCClient {
	tClientID++
	return &TRPCClient{
		id:     tClientID,
		ip:     dex.NewIPKey("123.123.123.123"),
		addr:   "addr",
		closed: make(chan struct{}),
	}
}

var tAcctID uint64

func newAccountID() account.AccountID {
	tAcctID++
	ib := make([]byte, 8)
	binary.BigEndian.PutUint64(ib, tAcctID)
	var acctID account.AccountID
	copy(acctID[len(acctID)-8:], ib)
	return acctID
}

type tUser struct {
	conn    *TRPCClient
	acctID  account.AccountID
	privKey *secp256k1.PrivateKey
}

// makes a new user with its own account ID, tRPCClient
func tNewUser(t *testing.T) *tUser {
	t.Helper()
	conn := tNewRPCClient()
	privKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("error generating private key: %v", err)
	}
	acctID := account.NewID(privKey.PubKey().SerializeCompressed())
	return &tUser{
		conn:    conn,
		acctID:  acctID,
		privKey: privKey,
	}
}

func (u *tUser) randomSignature() *ecdsa.Signature {
	return ecdsa.Sign(u.privKey, randBytes(32))
}

type testRig struct {
	mgr     *AuthManager
	storage *TStorage
	signer  *TSigner
}

type tMesh struct {
	mtx            sync.Mutex
	executeErr     *msgjson.Error
	executeHook    func(context.Context, mesh.CommandRequest) *msgjson.Error
	executedReq    mesh.CommandRequest
	executeCount   int
	proxiedErr     error
	proxiedUser    account.AccountID
	proxiedMsg     *msgjson.Message
	proxiedTimeout time.Duration
	proxiedDeliver bool
	proxyCount     int
	proxyReady     chan struct{}
	proxyWait      <-chan struct{}
	publishErr     error
	events         []*mesh.Event
}

func (m *tMesh) ExecuteCommand(ctx context.Context, req mesh.CommandRequest) *msgjson.Error {
	m.executeCount++
	m.executedReq = req
	if m.executeHook != nil {
		return m.executeHook(ctx, req)
	}
	return m.executeErr
}

func (m *tMesh) ProxyClientMessage(_ context.Context, req *mesh.ClientProxyMessage) error {
	if m.proxyWait != nil {
		<-m.proxyWait
	}
	m.proxyCount++
	m.proxiedUser = req.User
	m.proxiedMsg = req.Msg
	m.proxiedTimeout = time.Duration(req.TimeoutMS) * time.Millisecond
	m.proxiedDeliver = req.DeliverToClient
	if m.proxyReady != nil {
		select {
		case m.proxyReady <- struct{}{}:
		default:
		}
	}
	return m.proxiedErr
}

func setTestMeshService(svc MeshService) func() {
	prev := rig.mgr.mesh
	rig.mgr.SetMeshService(svc)
	return func() {
		rig.mgr.SetMeshService(prev)
	}
}

func (m *tMesh) ApplyEvent(_ context.Context, event *mesh.Event) (any, error) {
	if m.publishErr != nil {
		return nil, m.publishErr
	}
	cpy := &mesh.Event{
		Kind:    event.Kind,
		Payload: append([]byte(nil), event.Payload...),
	}
	m.mtx.Lock()
	m.events = append(m.events, cpy)
	m.mtx.Unlock()
	return nil, nil
}

func (m *tMesh) publishedEvents() []*mesh.Event {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	events := make([]*mesh.Event, len(m.events))
	copy(events, m.events)
	return events
}

var rig *testRig

type tSignable struct {
	b   []byte
	sig []byte
}

func (s *tSignable) SetSig(b []byte)  { s.sig = b }
func (s *tSignable) SigBytes() []byte { return s.sig }
func (s *tSignable) Serialize() []byte {
	return s.b
}

func signMsg(priv *secp256k1.PrivateKey, msg []byte) []byte {
	hash := sha256.Sum256(msg)
	sig := ecdsa.Sign(priv, hash[:])
	return sig.Serialize()
}

func tNewConnect(user *tUser) *msgjson.Connect {
	return &msgjson.Connect{
		AccountID:  user.acctID[:],
		APIVersion: 0,
		Time:       uint64(time.Now().UnixMilli()),
	}
}

func tNewPostBondRequest(t *testing.T, user *tUser, assetID uint32, coinID []byte) (*msgjson.Message, *msgjson.PostBond) {
	t.Helper()
	postBond := &msgjson.PostBond{
		AcctPubKey: user.privKey.PubKey().SerializeCompressed(),
		AssetID:    assetID,
		Version:    0,
		CoinID:     coinID,
	}
	postBond.SetSig(signMsg(user.privKey, postBond.Serialize()))
	msg, err := msgjson.NewRequest(comms.NextID(), msgjson.PostBondRoute, postBond)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	return msg, postBond
}

func newEventTestAuthManager(t *testing.T) (*AuthManager, *TStorage) {
	t.Helper()
	storage := &TStorage{}
	dexKey, err := secp256k1.ParsePubKey(tDexPubKeyBytes)
	if err != nil {
		t.Fatalf("ParsePubKey error: %v", err)
	}
	authMgr := NewAuthManager(&Config{
		Storage:    storage,
		Signer:     &TSigner{pubkey: dexKey},
		BondExpiry: 86400,
		BondAssets: map[string]*msgjson.BondAsset{
			"dcr": {
				Version: 0,
				ID:      42,
				Confs:   uint32(tBondConfs),
				Amt:     tRegFee * 10,
			},
		},
		BondTxParser:    tParseBondTx,
		CancelThreshold: 0.9,
		TxDataSources:   make(map[uint32]TxDataSource),
		Route:           func(string, comms.MsgHandler) {},
	})
	authMgr.ctx = t.Context()
	return authMgr, storage
}

func TestRepCacheStorageListener(t *testing.T) {
	authMgr, storage := newEventTestAuthManager(t)
	if storage.repInputsListener == nil {
		t.Fatal("NewAuthManager did not register a reputation-inputs listener with storage")
	}

	user := testAcctID(0xbb)
	ctx := context.Background()
	var calls int32
	for i := 0; i < 2; i++ {
		if _, err := authMgr.rep.get(ctx, user, fixedRepFetcher(5, &calls)); err != nil {
			t.Fatalf("rep.get error: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("fetches before notification = %d, want 1 (second get cached)", calls)
	}

	storage.notifyRepInputs(user)
	if _, err := authMgr.rep.get(ctx, user, fixedRepFetcher(5, &calls)); err != nil {
		t.Fatalf("rep.get after notification error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetches after notification = %d, want 2 (entry invalidated)", calls)
	}
}

// TestUserReputationAt: a wall-clock-expired bond still counts at a
// pre-expiry as-of.
func TestUserReputationAt(t *testing.T) {
	authMgr, storage := newEventTestAuthManager(t)
	user := testAcctID(0xac)
	storage.acct = &account.Account{ID: user}
	// The bond's lock time passed an hour ago: expired at the wall clock,
	// live at any as-of more than bondExpiry before the lock time.
	lockTime := time.Now().Add(-time.Hour)
	storage.bonds = []*db.Bond{{Strength: 2, LockTime: lockTime.Unix()}}

	tier, _, _, err := authMgr.UserReputationAt(user, lockTime.Add(-authMgr.bondExpiry).Add(-time.Minute))
	if err != nil {
		t.Fatalf("UserReputationAt error: %v", err)
	}
	if tier != 2 {
		t.Fatalf("tier at pre-expiry as-of = %d, want 2", tier)
	}

	tier, _, _, err = authMgr.UserReputationAt(user, time.Now())
	if err != nil {
		t.Fatalf("UserReputationAt error: %v", err)
	}
	if tier != 0 {
		t.Fatalf("wall-clock tier = %d, want 0 for expired bond", tier)
	}
}

func TestAcctRepStatus(t *testing.T) {
	authMgr, storage := newEventTestAuthManager(t)
	user := testAcctID(0xaa)
	storage.acct = &account.Account{ID: user}
	storage.setBondTier(1)

	connected, rep, err := authMgr.AcctRepStatus(user)
	if err != nil {
		t.Fatalf("AcctRepStatus error: %v", err)
	}
	if connected {
		t.Fatal("reported connected with no client connection")
	}
	if rep == nil || rep.EffectiveTier() != 1 {
		t.Fatalf("rep = %+v, want effective tier 1", rep)
	}

	// Score-load failure is an error, not tier 0.
	storage.reputationErr = errors.New("db down")
	authMgr.rep.invalidate(user)
	if _, rep, err = authMgr.AcctRepStatus(user); err == nil {
		t.Fatal("no error from failed score load")
	} else if rep != nil {
		t.Fatal("non-nil rep with load error")
	}

	// On load error, connected is still reported correctly.
	authMgr.connMtx.Lock()
	authMgr.users[user] = &clientInfo{}
	authMgr.connMtx.Unlock()
	if connected, _, err = authMgr.AcctRepStatus(user); !connected || err == nil {
		t.Fatalf("AcctRepStatus on load error = (connected %v, err %v), want connected with error", connected, err)
	}
	authMgr.connMtx.Lock()
	delete(authMgr.users, user)
	authMgr.connMtx.Unlock()

	// Account-read failure is also an error.
	storage.reputationErr = nil
	storage.accountReadErr = errors.New("db down")
	if _, _, err = authMgr.AcctRepStatus(user); err == nil {
		t.Fatal("no error from failed account read")
	}

	// Unknown account: nil rep, nil error.
	storage.accountReadErr = nil
	storage.acct = nil
	authMgr.rep.invalidate(user)
	if _, rep, err = authMgr.AcctRepStatus(user); err != nil || rep != nil {
		t.Fatalf("unknown account: rep %+v, err %v", rep, err)
	}
}

func eventsWithCapture(authMgr *AuthManager, captured *[]*mesh.Event) map[string]mesh.EventApplier {
	events := authMgr.Events()
	for kind, apply := range events {
		kind, apply := kind, apply
		events[kind] = func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			*captured = append(*captured, &mesh.Event{
				Kind:    event.Kind,
				Payload: append([]byte(nil), event.Payload...),
			})
			return apply(applyCtx, event)
		}
	}
	return events
}

func extractConnectResult(t *testing.T, msg *msgjson.Message) *msgjson.ConnectResult {
	t.Helper()
	if msg == nil {
		t.Fatalf("no response from 'connect' request")
	}
	resp, _ := msg.Response()
	result := new(msgjson.ConnectResult)
	err := json.Unmarshal(resp.Result, result)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	return result
}

func queueUser(t *testing.T, user *tUser) *msgjson.Message {
	t.Helper()
	rig.storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}
	connect := tNewConnect(user)
	sigMsg := connect.Serialize()
	sig := signMsg(user.privKey, sigMsg)
	connect.SetSig(sig)
	msg, _ := msgjson.NewRequest(comms.NextID(), msgjson.ConnectRoute, connect)
	return msg
}

func connectUser(t *testing.T, user *tUser) *msgjson.Message {
	t.Helper()
	return tryConnectUser(t, user, false)
}

func tryConnectUser(t *testing.T, user *tUser, wantErr bool) *msgjson.Message {
	t.Helper()
	connect := queueUser(t, user)
	err := rig.mgr.handleConnect(user.conn, connect)
	if (err != nil) != wantErr {
		t.Fatalf("handleConnect: wantErr=%v, got err=%v", wantErr, err)
	}

	// Check the response.
	respMsg := user.conn.getSend()
	if respMsg == nil {
		t.Fatalf("no response from 'connect' request")
	}
	if respMsg.ID != connect.ID {
		t.Fatalf("'connect' response has wrong ID. expected %d, got %d", connect.ID, respMsg.ID)
	}
	return respMsg
}

func makeEnsureErr(t *testing.T) func(rpcErr *msgjson.Error, tag string, code int) {
	return func(rpcErr *msgjson.Error, tag string, code int) {
		t.Helper()
		if rpcErr == nil {
			t.Fatalf("no error for %s ID", tag)
		}
		if rpcErr.Code != code {
			t.Fatalf("wrong error code for %s. expected %d, got %d: %s",
				tag, code, rpcErr.Code, rpcErr.Message)
		}
	}
}

func waitFor(pred func() bool, timeout time.Duration) (fail bool) {
	tStart := time.Now()
	for {
		if pred() {
			return false
		}
		if time.Since(tStart) > timeout {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var (
	tBondConfs       int64 = 5
	tParseBondTxAcct account.AccountID
	tParseBondTxErr  error
)

func tParseBondTx(assetID uint32, ver uint16, rawTx []byte) (bondCoinID []byte, amt int64,
	lockTime int64, acct account.AccountID, err error) {
	return nil, 0, time.Now().Add(time.Minute).Unix(), tParseBondTxAcct, tParseBondTxErr
}

const (
	tRegFee       uint64 = 500_000_000
	tDexPubKeyHex string = "032e3678f9889206dcea4fc281556c9e543c5d5ffa7efe8d11118b52e29c773f27"
)

var tDexPubKeyBytes = []byte{
	0x03, 0x2e, 0x36, 0x78, 0xf9, 0x88, 0x92, 0x06, 0xdc, 0xea, 0x4f, 0xc2,
	0x81, 0x55, 0x6c, 0x9e, 0x54, 0x3c, 0x5d, 0x5f, 0xfa, 0x7e, 0xfe, 0x8d,
	0x11, 0x11, 0x8b, 0x52, 0xe2, 0x9c, 0x77, 0x3f, 0x27,
}

var tRoutes = make(map[string]comms.MsgHandler)

func TestMain(m *testing.M) {
	doIt := func() int {
		UseLogger(dex.StdOutLogger("AUTH_TEST", dex.LevelTrace))
		ctx, shutdown := context.WithCancel(context.Background())
		defer shutdown()
		storage := &TStorage{}
		// secp256k1.PrivKeyFromBytes
		dexKey, _ := secp256k1.ParsePubKey(tDexPubKeyBytes)
		signer := &TSigner{pubkey: dexKey}
		authMgr := NewAuthManager(&Config{
			Storage:    storage,
			Signer:     signer,
			BondExpiry: 86400,
			BondAssets: map[string]*msgjson.BondAsset{
				"dcr": {
					Version: 0,
					ID:      42,
					Confs:   uint32(tBondConfs),
					Amt:     tRegFee * 10,
				},
			},
			BondTxParser:    tParseBondTx,
			CancelThreshold: 0.9,
			TxDataSources:   make(map[uint32]TxDataSource),
			Route: func(route string, handler comms.MsgHandler) {
				tRoutes[route] = handler
			},
		})
		meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
			EventLogReader: emptyEventLogReader{},
			OnHalt:         func(error) {},
			Logger:         dex.Disabled,
		})
		if err != nil {
			fmt.Printf("NewService error: %v\n", err)
			return 1
		}
		authMgr.SetMeshService(meshSvc)
		cm := dex.NewConnectionMaster(authMgr)
		cm.Connect(ctx)
		defer cm.Disconnect()
		rig = &testRig{
			storage: storage,
			signer:  signer,
			mgr:     authMgr,
		}
		return m.Run()
	}

	os.Exit(doIt())
}

func userMatchData(takerUser account.AccountID) (*db.MatchData, *order.UserMatch) {
	var baseRate, quoteRate uint64 = 123, 73
	side := order.Taker
	takerSell := true
	feeRateSwap := baseRate // user is selling

	anyID := newAccountID()
	var mid order.MatchID
	copy(mid[:], anyID[:])
	anyID = newAccountID()
	var oid order.OrderID
	copy(oid[:], anyID[:])
	takerUserMatch := &order.UserMatch{
		OrderID:     oid,
		MatchID:     mid,
		Quantity:    1,
		Rate:        2,
		Address:     "makerSwapAddress", // counterparty
		Status:      order.MakerRedeemed,
		Side:        side,
		FeeRateSwap: feeRateSwap,
	}

	var oid2 order.OrderID
	anyID = newAccountID()
	copy(oid2[:], anyID[:])
	matchData := &db.MatchData{
		ID:            mid,
		Taker:         oid,
		TakerAcct:     takerUser,
		TakerAddr:     "takerSwapAddress",
		TakerSell:     takerSell,
		Maker:         oid2,
		MakerAcct:     newAccountID(),
		MakerAddr:     takerUserMatch.Address,
		MakerSwapAddr: takerUserMatch.Address, // per-match address
		TakerSwapAddr: "takerSwapAddress",     // per-match address
		Epoch: order.EpochID{
			Dur: 10000,
			Idx: 132412342,
		},
		Quantity:  takerUserMatch.Quantity,
		Rate:      takerUserMatch.Rate,
		BaseRate:  baseRate,
		QuoteRate: quoteRate,
		Active:    true,
		Status:    takerUserMatch.Status,
	}

	//matchTime := matchData.Epoch.End()
	return matchData, takerUserMatch
}

func TestGraceLimit(t *testing.T) {
	tests := []struct {
		name      string
		thresh    float64
		wantLimit int
	}{
		{"0.99 => 99", 0.99, 99}, // 98.99999999999991
		{"0.98 => 49", 0.98, 49}, // 48.99999999999996
		{"0.96 => 24", 0.96, 24}, // 23.99999999999998
		{"0.95 => 19", 0.95, 19}, // 18.999999999999982
		{"0.9 => 9", 0.9, 9},     // 9.000000000000002
		{"0.875 => 7", 0.875, 7}, // exact
		{"0.8 => 4", 0.8, 4},     // 4.000000000000001
		{"0.75 => 3", 0.75, 3},   // exact
		{"0.5 => 1", 0.5, 1},     // exact
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &AuthManager{
				cancelThresh: tt.thresh,
			}
			got := auth.GraceLimit()
			if got != tt.wantLimit {
				t.Errorf("incorrect grace limit. got %d, want %d", got, tt.wantLimit)
			}
		})
	}
}

func randomOrderID() (oid order.OrderID) {
	copy(oid[:], encode.RandomBytes(32))
	return
}

var reputationDBID int64

func nextReputationDBID() int64 {
	reputationDBID++
	return reputationDBID
}

func newMatchResult(status order.MatchStatus, fail bool) *db.MatchResult {
	outcome := db.OutcomeSwapSuccess
	if fail {
		outcome = matchStatusToOutcome(status)
	}
	return &db.MatchResult{
		DBID:         nextReputationDBID(),
		MatchID:      randomMatchID(),
		MatchOutcome: outcome,
	}
}

func newPreimageOutcome(miss bool) *db.PreimageOutcome {
	return &db.PreimageOutcome{
		DBID:    nextReputationDBID(),
		OrderID: randomOrderID(),
		Miss:    miss,
	}
}

func setViolations() (wantScore int32) {
	rig.storage.reputationMatches = []*db.MatchResult{
		newMatchResult(order.NewlyMatched, true),
		newMatchResult(order.MatchComplete, false), // success
		newMatchResult(order.NewlyMatched, true),
		newMatchResult(order.MakerSwapCast, true), // noSwapAsTaker at index 3
		newMatchResult(order.TakerSwapCast, true),
		newMatchResult(order.MakerRedeemed, false), // success (for maker)
		newMatchResult(order.MakerRedeemed, true),
		newMatchResult(order.MatchComplete, false), // success
		newMatchResult(order.MatchComplete, false), // success
	}
	rig.storage.reputationPreimages = []*db.PreimageOutcome{newPreimageOutcome(true)}
	for range rig.storage.reputationMatches {
		rig.storage.reputationPreimages = append(rig.storage.reputationPreimages, newPreimageOutcome(false))
	}
	return 4*matchCompletedScore + 1*preimageMissScore +
		2*noSwapAsMakerScore + noSwapAsTakerScore + noRedeemAsMakerScore + 1*noRedeemAsTakerScore
}

func clearViolations() {
	rig.storage.reputationPreimages = nil
	rig.storage.reputationMatches = nil
	rig.storage.reputationOrders = nil
}

func TestAuthManager_loadUserScore(t *testing.T) {
	// Spot test with all violations set
	wantScore := setViolations()
	defer clearViolations()
	user := tNewUser(t)
	score, err := rig.mgr.loadUserScoreContext(context.Background(), user.acctID)
	if err != nil {
		t.Fatal(err)
	}
	if score != wantScore {
		t.Errorf("wrong score. got %d, want %d", score, wantScore)
	}

	// add one NoSwapAsTaker (match inactive at MakerSwapCast)
	rig.storage.reputationMatches = append(rig.storage.reputationMatches,
		newMatchResult(order.MakerSwapCast, true))
	wantScore += noSwapAsTakerScore

	score, err = rig.mgr.loadUserScoreContext(context.Background(), user.acctID)
	if err != nil {
		t.Fatal(err)
	}
	if score != wantScore {
		t.Errorf("wrong score. got %d, want %d", score, wantScore)
	}

	tests := []struct {
		name           string
		user           account.AccountID
		matchOutcomes  []*db.MatchResult
		preimageMisses []*db.PreimageOutcome
		wantScore      int32
	}{
		{
			name: "negative",
			user: user.acctID,
			matchOutcomes: []*db.MatchResult{
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
			},
			wantScore: 4,
		},
		{
			name:          "nuthin",
			user:          user.acctID,
			matchOutcomes: []*db.MatchResult{},
			wantScore:     0,
		},
		{
			name: "balance",
			user: user.acctID,
			matchOutcomes: []*db.MatchResult{
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
			},
			preimageMisses: []*db.PreimageOutcome{
				newPreimageOutcome(true),
				newPreimageOutcome(true),
			},
			wantScore: 0,
		},
		{
			name: "tipping red",
			user: user.acctID,
			matchOutcomes: []*db.MatchResult{
				newMatchResult(order.NewlyMatched, true),
				newMatchResult(order.MakerSwapCast, true),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.NewlyMatched, true),
				newMatchResult(order.MakerRedeemed, true),
				newMatchResult(order.MatchComplete, false),
				newMatchResult(order.MatchComplete, false),
			},
			preimageMisses: []*db.PreimageOutcome{
				newPreimageOutcome(true),
				newPreimageOutcome(false),
			},
			wantScore: 2*noSwapAsMakerScore + 1*noSwapAsTakerScore + 0*noRedeemAsMakerScore +
				1*noRedeemAsTakerScore + 1*preimageMissScore + 5*matchCompletedScore,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig.storage.reputationMatches = tt.matchOutcomes
			rig.storage.reputationPreimages = tt.preimageMisses
			score, err := rig.mgr.loadUserScoreContext(context.Background(), tt.user)
			if err != nil {
				t.Fatalf("got err: %v", err)
			}
			if score != tt.wantScore {
				t.Errorf("incorrect user score. got %d, want %d", score, tt.wantScore)
			}
		})
	}
}

func TestConnect(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())

	// Before connecting, put an activeOrder and activeMatch in storage.
	matchData, userMatch := userMatchData(user.acctID)
	matchTime := matchData.Epoch.End()

	rig.storage.orderStatuses = []*db.OrderStatus{
		{
			ID:     userMatch.OrderID,
			Status: order.OrderStatusBooked,
		},
	}
	defer func() { rig.storage.orderStatuses = nil }()

	rig.storage.matches = []*db.MatchData{matchData}
	defer func() { rig.storage.matches = nil }()

	epochGaps := []int32{1} // penalized

	rig.storage.setRatioData(&ratioData{
		oidsCompleted:  []order.OrderID{{0x1}},
		timesCompleted: []int64{1234},
		oidsCancels:    []order.OrderID{{0x2}},
		oidsCanceled:   []order.OrderID{{0x1}},
		timesCanceled:  []int64{1235},
		epochGaps:      epochGaps,
	}) // 1:1 = 50%
	defer rig.storage.setRatioData(&ratioData{}) // clean slate

	// TODO: update tests now that there is are no close/ban and unban
	// operations, instead an integral tier.

	// TODO: update tests now that cancel ratio is part of the score equation
	// rather than a hard close operation.

	/* cancel ratio stuff

	// Close account on connect with failing cancel ratio, and no grace period.
	rig.mgr.cancelThresh = 0.2 // thresh below actual ratio, and no grace period with total/(1+total) = 2/3 = 0.66... > 0.2
	tryConnectUser(t, user, false)
	if rig.storage.closedID != user.acctID {
		t.Fatalf("Expected account %v to be closed on connect, got %v", user.acctID, rig.storage.closedID)
	}

	// Make it a free cancel.
	rig.storage.closedID = account.AccountID{} // unclose the account in db
	epochGaps[0] = 2
	connectUser(t, user)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed with free cancels, but it was.", user)
	}
	epochGaps[0] = 1

	// Try again just meeting cancel ratio.
	rig.storage.closedID = account.AccountID{} // unclose the account in db
	rig.mgr.cancelThresh = 0.6                 // passable threshold for 1 cancel : 1 completion (0.5)

	connectUser(t, user)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed on connect, but it was.", user)
	}

	// Add another cancel, bringing cancels to 2, completions 1 for a ratio of
	// 2:1 (2/3 = 0.666...), and total/(1+total) = 3/4 = 0.75 > thresh (0.6), so
	// no grace period.
	rig.storage.ratio.oidsCanceled = append(rig.storage.ratio.oidsCanceled, order.OrderID{0x3})
	rig.storage.ratio.oidsCancels = append(rig.storage.ratio.oidsCancels, order.OrderID{0x4})
	rig.storage.ratio.timesCanceled = append(rig.storage.ratio.timesCanceled, 12341234)
	rig.storage.ratio.epochGaps = append(rig.storage.ratio.epochGaps, 1)

	tryConnectUser(t, user, false)
	if rig.storage.closedID != user.acctID {
		t.Fatalf("Expected account %v to be closed on connect, got %v", user.acctID, rig.storage.closedID)
	}

	// Make one a free cancel.
	rig.storage.closedID = account.AccountID{} // unclose the account in db
	rig.storage.ratio.epochGaps[1] = 2
	connectUser(t, user)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed with free cancels, but it was.", user)
	}
	rig.storage.ratio.epochGaps[1] = 0

	// Try again just meeting cancel ratio.
	rig.storage.closedID = account.AccountID{} // unclose the account in db
	rig.mgr.cancelThresh = 0.7                 // passable threshold for 2 cancel : 1 completion (0.6666..)

	tryConnectUser(t, user, false)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed on connect, but it was.", user)
	}

	// Test the grace period (threshold <= total/(1+total) and no completions)
	// 2 cancels, 0 success, 2 total
	rig.mgr.cancelThresh = 0.7             // 2/(1+2) = 0.66.. < threshold
	rig.storage.ratio.timesCompleted = nil // no completions
	rig.storage.ratio.oidsCompleted = nil
	tryConnectUser(t, user, false)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed on connect, but it was.", user)
	}

	// 3 cancels, 0 success, 3 total => rate = 1.0, exceeds threshold
	rig.mgr.cancelThresh = 0.75 // 3/(1+3) == threshold, still in grace period
	rig.storage.ratio.oidsCanceled = append(rig.storage.ratio.oidsCanceled, order.OrderID{0x4})
	rig.storage.ratio.oidsCancels = append(rig.storage.ratio.oidsCancels, order.OrderID{0x5})
	rig.storage.ratio.timesCanceled = append(rig.storage.ratio.timesCanceled, 12341239)
	rig.storage.ratio.epochGaps = append(rig.storage.ratio.epochGaps, 1)

	tryConnectUser(t, user, false)
	if rig.storage.closedID == user.acctID {
		t.Fatalf("Expected account %v to NOT be closed on connect, but it was.", user)
	}

	*/

	// Connect with a violation score above revocation threshold.
	wantScore := setViolations()
	defer clearViolations()

	if wantScore > rig.mgr.penaltyThreshold {
		t.Fatalf("test score of %v is not at least the revocation threshold of %v, revise the test", wantScore, rig.mgr.penaltyThreshold)
	}

	// Test loadUserScore while here.
	_, err := rig.mgr.loadUserScoreContext(context.Background(), user.acctID)
	if err != nil {
		t.Fatal(err)
	}

	// if score != wantScore {
	// 	t.Errorf("wrong score. got %d, want %d", score, wantScore)
	// }

	// No error, but Penalize account that was not previously closed.
	tryConnectUser(t, user, false)

	makerSwapCastIdx := 3
	rig.storage.reputationMatches = append(rig.storage.reputationMatches[:makerSwapCastIdx], rig.storage.reputationMatches[makerSwapCastIdx+1:]...)
	wantScore -= noSwapAsTakerScore
	if wantScore <= rig.mgr.penaltyThreshold {
		t.Fatalf("test score of %v is not more than the penalty threshold of %v, revise the test", wantScore, rig.mgr.penaltyThreshold)
	}
	_, err = rig.mgr.loadUserScoreContext(context.Background(), user.acctID)
	if err != nil {
		t.Fatal(err)
	}
	// if score != wantScore {
	// 	t.Errorf("wrong score. got %d, want %d", score, wantScore)
	// }

	// Connect the user.
	before := time.Now().Add(rig.mgr.bondExpiry).Unix()
	respMsg := connectUser(t, user)
	after := time.Now().Add(rig.mgr.bondExpiry).Unix()
	cResp := extractConnectResult(t, respMsg)
	if cResp.Reputation.BondExpiryThreshold < before || cResp.Reputation.BondExpiryThreshold > after {
		t.Fatalf("bond expiry threshold = %d, want between %d and %d", cResp.Reputation.BondExpiryThreshold, before, after)
	}
	if len(cResp.ActiveOrderStatuses) != 1 {
		t.Fatalf("no active orders")
	}
	msgOrder := cResp.ActiveOrderStatuses[0]
	if msgOrder.ID.String() != userMatch.OrderID.String() {
		t.Fatal("active order ID mismatch: ", msgOrder.ID.String(), " != ", userMatch.OrderID.String())
	}
	if msgOrder.Status != uint16(order.OrderStatusBooked) {
		t.Fatal("active order Status mismatch: ", msgOrder.Status, " != ", order.OrderStatusBooked)
	}
	if len(cResp.ActiveMatches) != 1 {
		t.Fatalf("no active matches")
	}
	msgMatch := cResp.ActiveMatches[0]
	if msgMatch.OrderID.String() != userMatch.OrderID.String() {
		t.Fatal("active match OrderID mismatch: ", msgMatch.OrderID.String(), " != ", userMatch.OrderID.String())
	}
	if msgMatch.MatchID.String() != userMatch.MatchID.String() {
		t.Fatal("active match MatchID mismatch: ", msgMatch.MatchID.String(), " != ", userMatch.MatchID.String())
	}
	if msgMatch.Quantity != userMatch.Quantity {
		t.Fatal("active match Quantity mismatch: ", msgMatch.Quantity, " != ", userMatch.Quantity)
	}
	if msgMatch.Rate != userMatch.Rate {
		t.Fatal("active match Rate mismatch: ", msgMatch.Rate, " != ", userMatch.Rate)
	}
	if msgMatch.Address != userMatch.Address {
		t.Fatal("active match Address mismatch: ", msgMatch.Address, " != ", userMatch.Address)
	}
	if msgMatch.Status != uint8(userMatch.Status) {
		t.Fatal("active match Status mismatch: ", msgMatch.Status, " != ", userMatch.Status)
	}
	if msgMatch.Side != uint8(userMatch.Side) {
		t.Fatal("active match Side mismatch: ", msgMatch.Side, " != ", userMatch.Side)
	}
	if msgMatch.FeeRateQuote != matchData.QuoteRate {
		t.Fatal("active match quote fee rate mismatch: ", msgMatch.FeeRateQuote, " != ", matchData.QuoteRate)
	}
	if msgMatch.FeeRateBase != matchData.BaseRate {
		t.Fatal("active match base fee rate mismatch: ", msgMatch.FeeRateBase, " != ", matchData.BaseRate)
	}
	if msgMatch.ServerTime != uint64(matchTime.UnixMilli()) {
		t.Fatal("active match time mismatch: ", msgMatch.ServerTime, " != ", uint64(matchTime.UnixMilli()))
	}

	// Send a request to the client.
	type tPayload struct {
		A int
	}
	a5 := &tPayload{A: 5}
	reqID := comms.NextID()
	msg, err := msgjson.NewRequest(reqID, "request", a5)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	var responded bool
	rig.mgr.Request(user.acctID, msg, func(comms.Link, *msgjson.Message) {
		responded = true
	})
	req := user.conn.getReq()
	if req == nil {
		t.Fatalf("no request")
	}
	var a tPayload
	err = json.Unmarshal(req.msg.Payload, &a)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if a.A != 5 {
		t.Fatalf("wrong value for A. expected 5, got %d", a.A)
	}
	// Respond to the DEX's request.
	msg = &msgjson.Message{ID: reqID}
	req.respFunc(user.conn, msg)
	if !responded {
		t.Fatalf("responded flag not set")
	}

	reuser := &tUser{
		acctID:  user.acctID,
		privKey: user.privKey,
		conn:    tNewRPCClient(),
	}
	connectUser(t, reuser)
	a10 := &tPayload{A: 10}
	msg, _ = msgjson.NewRequest(comms.NextID(), "request", a10)
	err = rig.mgr.RequestWithTimeout(reuser.acctID, msg, func(comms.Link, *msgjson.Message) {}, time.Minute, func() {})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	// The a10 message should be in the new connection
	if user.conn.getReq() != nil {
		t.Fatalf("old connection received a request after reconnection")
	}
	if reuser.conn.getReq() == nil {
		t.Fatalf("new connection did not receive the request")
	}
}

func TestAccountErrors(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connect := queueUser(t, user)

	// Put a match in storage
	matchData, userMatch := userMatchData(user.acctID)
	matchTime := matchData.Epoch.End()
	rig.storage.matches = []*db.MatchData{matchData}

	rig.mgr.handleConnect(user.conn, connect)
	rig.storage.matches = nil

	// Check the response.
	respMsg := user.conn.getSend()
	result := extractConnectResult(t, respMsg)
	if len(result.ActiveMatches) != 1 {
		t.Fatalf("expected 1 match, received %d", len(result.ActiveMatches))
	}
	match := result.ActiveMatches[0]
	if match.OrderID.String() != userMatch.OrderID.String() {
		t.Fatal("wrong OrderID: ", match.OrderID, " != ", userMatch.OrderID)
	}
	if match.MatchID.String() != userMatch.MatchID.String() {
		t.Fatal("wrong MatchID: ", match.MatchID, " != ", userMatch.OrderID)
	}
	if match.Quantity != userMatch.Quantity {
		t.Fatal("wrong Quantity: ", match.Quantity, " != ", userMatch.OrderID)
	}
	if match.Rate != userMatch.Rate {
		t.Fatal("wrong Rate: ", match.Rate, " != ", userMatch.OrderID)
	}
	if match.Address != userMatch.Address {
		t.Fatal("wrong Address: ", match.Address, " != ", userMatch.OrderID)
	}
	if match.Status != uint8(userMatch.Status) {
		t.Fatal("wrong Status: ", match.Status, " != ", userMatch.OrderID)
	}
	if match.Side != uint8(userMatch.Side) {
		t.Fatal("wrong Side: ", match.Side, " != ", userMatch.OrderID)
	}
	if match.FeeRateQuote != matchData.QuoteRate {
		t.Fatal("wrong quote fee rate: ", match.FeeRateQuote, " != ", matchData.QuoteRate)
	}
	if match.FeeRateBase != matchData.BaseRate {
		t.Fatal("wrong base fee rate: ", match.FeeRateBase, " != ", matchData.BaseRate)
	}
	if match.ServerTime != uint64(matchTime.UnixMilli()) {
		t.Fatal("wrong match time: ", match.ServerTime, " != ", uint64(matchTime.UnixMilli()))
	}
	// Make a violation score above penalty threshold reflected by the DB.
	score := setViolations()
	defer clearViolations()

	rig.mgr.removeClient(rig.mgr.user(user.acctID)) // disconnect first, NOTE that link.Disconnect is async
	user.conn = tNewRPCClient()                     // disconnect necessitates new conn ID
	rpcErr := rig.mgr.handleConnect(user.conn, connect)
	if rpcErr != nil {
		t.Fatalf("should be no error for closed account")
	}
	client := rig.mgr.user(user.acctID)
	rig.storage.setBondTier(1)
	if client == nil {
		t.Fatalf("client not found")
	}
	initPenaltyThresh := rig.mgr.penaltyThreshold
	defer func() { rig.mgr.penaltyThreshold = initPenaltyThresh }()
	rig.mgr.penaltyThreshold = score
	if _, tier := rig.mgr.AcctStatus(user.acctID); tier > 0 {
		t.Errorf("client should have been tier 0")
	}

	// Raise the penalty threshold to ensure automatic reinstatement.
	rig.mgr.penaltyThreshold = score - 1

	rig.mgr.removeClient(rig.mgr.user(user.acctID)) // disconnect first, NOTE that link.Disconnect is async
	user.conn = tNewRPCClient()                     // disconnect necessitates new conn ID
	rpcErr = rig.mgr.handleConnect(user.conn, connect)
	if rpcErr != nil {
		t.Fatalf("should be no error for closed account")
	}
	client = rig.mgr.user(user.acctID)
	if client == nil {
		t.Fatalf("client not found")
	}
	if _, tier := rig.mgr.AcctStatus(user.acctID); tier < 1 {
		t.Errorf("client should have unbanned automatically")
	}

}

func TestRoute(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)

	var translated account.AccountID
	rig.mgr.Route("testroute", func(id account.AccountID, msg *msgjson.Message) *msgjson.Error {
		translated = id
		return nil
	})
	f := tRoutes["testroute"]
	if f == nil {
		t.Fatalf("'testroute' not registered")
	}
	rpcErr := f(user.conn, nil)
	if rpcErr != nil {
		t.Fatalf("rpc error: %s", rpcErr.Message)
	}
	if translated != user.acctID {
		t.Fatalf("account ID not set")
	}

	// Run the route with an unknown client. Should be an UnauthorizedConnection
	// error.
	foreigner := tNewUser(t)
	rpcErr = f(foreigner.conn, nil)
	if rpcErr == nil {
		t.Fatalf("no error for unauthed user")
	}
	if rpcErr.Code != msgjson.UnauthorizedConnection {
		t.Fatalf("wrong error for unauthed user. expected %d, got %d",
			msgjson.UnauthorizedConnection, rpcErr.Code)
	}
}

func TestHandlePostBondSubmitsCommand(t *testing.T) {
	tests := []struct {
		name    string
		assetID uint32
		coinID  []byte
	}{
		{
			name:    "normal bond",
			assetID: 42,
			coinID:  []byte{0x01, 0x02, 0x03},
		},
		{
			name:    "pre-paid bond",
			assetID: account.PrepaidBondID,
			coinID:  bytes.Repeat([]byte{0x01}, 16),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tNewUser(t)
			meshReq := &tMesh{}
			restore := setTestMeshService(meshReq)
			defer restore()

			msg, _ := tNewPostBondRequest(t, user, tt.assetID, tt.coinID)

			rpcErr := rig.mgr.handlePostBond(user.conn, msg)
			if rpcErr != nil {
				t.Fatalf("handlePostBond error: %v", rpcErr)
			}
			if meshReq.executeCount != 1 {
				t.Fatalf("wrong execute count. got %d, want 1", meshReq.executeCount)
			}
			if meshReq.executedReq.User != user.acctID {
				t.Fatalf("wrong command user. got %v, want %v", meshReq.executedReq.User, user.acctID)
			}
			if meshReq.executedReq.Msg != msg {
				t.Fatalf("executed wrong message")
			}
			if meshReq.executedReq.Kind != commandKindPostBond {
				t.Fatalf("wrong command kind. got %q, want %q", meshReq.executedReq.Kind, commandKindPostBond)
			}
			if user.conn.sendCount() != 0 {
				t.Fatalf("unexpected immediate postbond response")
			}
		})
	}
}

func TestHandlePrepaidPostBondForwardedResponse(t *testing.T) {
	user := tNewUser(t)
	coinID := bytes.Repeat([]byte{0x02}, 16)
	meshReq := &tMesh{
		executeHook: func(_ context.Context, req mesh.CommandRequest) *msgjson.Error {
			if req.Kind != commandKindPostBond {
				return msgjson.NewError(msgjson.RPCInternalError, "wrong command kind")
			}
			result := &msgjson.PostBondResult{
				AccountID: user.acctID[:],
				AssetID:   account.PrepaidBondID,
				BondID:    coinID,
				Strength:  2,
			}
			resp, err := msgjson.NewResponse(req.Msg.ID, result, nil)
			if err != nil {
				return msgjson.NewError(msgjson.RPCInternalError, "response encode error: %v", err)
			}
			if err := req.Respond(resp); err != nil {
				return msgjson.NewError(msgjson.RPCInternalError, "response delivery error: %v", err)
			}
			return nil
		},
	}
	restore := setTestMeshService(meshReq)
	defer restore()

	msg, _ := tNewPostBondRequest(t, user, account.PrepaidBondID, coinID)
	if rpcErr := rig.mgr.handlePostBond(user.conn, msg); rpcErr != nil {
		t.Fatalf("handlePostBond error: %v", rpcErr)
	}

	sent := user.conn.getSend()
	if sent == nil {
		t.Fatal("no forwarded postbond response")
	}
	result := decodePostBondResult(t, sent)
	if result.AssetID != account.PrepaidBondID || !bytes.Equal(result.BondID, coinID) {
		t.Fatalf("wrong forwarded pre-paid response: %+v", result)
	}
}

func decodePostBondResult(t *testing.T, msg *msgjson.Message) *msgjson.PostBondResult {
	t.Helper()
	resp, err := msg.Response()
	if err != nil {
		t.Fatalf("postbond response decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("postbond response error: %v", resp.Error)
	}
	var result msgjson.PostBondResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("postbond result decode: %v", err)
	}
	return &result
}

func TestCreatePrepaidBonds(t *testing.T) {
	t.Run("local command emits event and stores tokens", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		var capturedEvents []*mesh.Event
		meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
			EventLogReader: emptyEventLogReader{},
			OnHalt:         func(error) {},
			Commands:       authMgr.Commands(),
			Events:         eventsWithCapture(authMgr, &capturedEvents),
			Logger:         dex.Disabled,
		})
		if err != nil {
			t.Fatalf("NewService error: %v", err)
		}
		authMgr.SetMeshService(meshSvc)

		coinIDs, err := authMgr.CreatePrepaidBonds(2, 3, int64((48 * time.Hour).Seconds()))
		if err != nil {
			t.Fatalf("CreatePrepaidBonds error: %v", err)
		}
		if len(coinIDs) != 2 {
			t.Fatalf("created %d ids, want 2", len(coinIDs))
		}
		for _, coinID := range coinIDs {
			if len(coinID) != 16 {
				t.Fatalf("pre-paid bond id length = %d, want 16", len(coinID))
			}
			bond := storage.prepaidBonds[string(coinID)]
			if bond == nil {
				t.Fatalf("pre-paid bond %x was not stored", coinID)
			}
			if bond.Strength != 3 {
				t.Fatalf("stored strength = %d, want 3", bond.Strength)
			}
			if time.Until(time.Unix(bond.LockTime, 0).Add(-authMgr.bondExpiry)) < 47*time.Hour {
				t.Fatalf("stored lock time too short: %v", time.Unix(bond.LockTime, 0))
			}
		}
		if len(capturedEvents) != 1 || capturedEvents[0].Kind != meshevents.EventKindPrepaidBondsCreated {
			t.Fatalf("captured events = %+v, want one prepaid_bonds_created event", capturedEvents)
		}
	})

	t.Run("forwarded raw response is decoded", func(t *testing.T) {
		authMgr, _ := newEventTestAuthManager(t)
		wantCoinIDs := [][]byte{bytes.Repeat([]byte{0x03}, 16), bytes.Repeat([]byte{0x04}, 16)}
		authMgr.SetMeshService(&tMesh{
			executeHook: func(_ context.Context, req mesh.CommandRequest) *msgjson.Error {
				raw, err := json.Marshal(&createPrepaidBondsResult{CoinIDs: wantCoinIDs})
				if err != nil {
					return msgjson.NewError(msgjson.RPCInternalError, "marshal error: %v", err)
				}
				resp, err := msgjson.NewResponse(req.Msg.ID, json.RawMessage(raw), nil)
				if err != nil {
					return msgjson.NewError(msgjson.RPCInternalError, "response encode error: %v", err)
				}
				if err := req.Respond(resp); err != nil {
					return msgjson.NewError(msgjson.RPCInternalError, "response delivery error: %v", err)
				}
				return nil
			},
		})

		gotCoinIDs, err := authMgr.CreatePrepaidBonds(2, 4, 3600)
		if err != nil {
			t.Fatalf("CreatePrepaidBonds error: %v", err)
		}
		if !bytes.Equal(gotCoinIDs[0], wantCoinIDs[0]) || !bytes.Equal(gotCoinIDs[1], wantCoinIDs[1]) {
			t.Fatalf("coin ids = %x, want %x", gotCoinIDs, wantCoinIDs)
		}
	})

}

func TestExecutePostBond(t *testing.T) {
	const (
		assetID  = 42
		strength = 1
	)

	tests := []struct {
		name           string
		coinID         []byte
		existingBond   bool
		delayedConfirm bool
		wantEvent      bool
	}{
		{
			name:      "confirmed new bond emits event",
			coinID:    []byte{0x04, 0x05, 0x06},
			wantEvent: true,
		},
		{
			name:         "existing bond completes without event",
			coinID:       []byte{0x0a, 0x0b, 0x0c},
			existingBond: true,
		},
		{
			name:           "delayed confirmation responds after event",
			coinID:         []byte{0x07, 0x08, 0x09},
			delayedConfirm: true,
			wantEvent:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tNewUser(t)
			authMgr, storage := newEventTestAuthManager(t)
			authMgr.signer.(*TSigner).setSig(user.randomSignature())

			// Build the bond that checkBond will report for this command.
			lockTime := time.Now().Add(48 * time.Hour).Unix()
			amount := int64(tRegFee * 10)
			bond := &db.Bond{
				Version:  0,
				AssetID:  assetID,
				CoinID:   tt.coinID,
				Amount:   amount,
				Strength: strength,
				LockTime: lockTime,
			}
			if tt.existingBond {
				// Existing bonds are acknowledged without publishing another event.
				acct := &account.Account{
					ID:     user.acctID,
					PubKey: user.privKey.PubKey(),
				}
				if err := storage.CreateAccountWithBond(acct, bond); err != nil {
					t.Fatalf("CreateAccountWithBond error: %v", err)
				}
			}

			// Capture published events while still routing them through the real applier.
			var capturedEvents []*mesh.Event
			meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
				EventLogReader: emptyEventLogReader{},
				OnHalt:         func(error) {},
				Commands:       authMgr.Commands(),
				Events:         eventsWithCapture(authMgr, &capturedEvents),
				Logger:         dex.Disabled,
			})
			if err != nil {
				t.Fatalf("NewService error: %v", err)
			}

			var confirmed chan struct{}
			if tt.delayedConfirm {
				// Start under-confirmed, then close confirmed so the bond waiter can finish.
				confirmed = make(chan struct{})
				t.Cleanup(func() { authMgr.removeBondWaiter(bondKey(assetID, tt.coinID)) })

				queueCtx, shutdownQueue := context.WithCancel(context.Background())
				t.Cleanup(shutdownQueue)
				authMgr.latencyQ = wait.NewTickerQueue(time.Millisecond)
				go authMgr.latencyQ.Run(queueCtx)
			}

			// Mock the asset backend's bond lookup.
			authMgr.checkBond = func(_ context.Context, gotAssetID uint32, ver uint16, gotCoinID []byte) (amt, gotLockTime, confs int64, acct account.AccountID, err error) {
				if gotAssetID != assetID {
					t.Fatalf("wrong asset id. got %d", gotAssetID)
				}
				if ver != 0 {
					t.Fatalf("wrong bond version. got %d", ver)
				}
				if !bytes.Equal(gotCoinID, tt.coinID) {
					t.Fatalf("wrong coin id. got %x, want %x", gotCoinID, tt.coinID)
				}
				if !tt.delayedConfirm {
					confs = tBondConfs
				} else {
					select {
					case <-confirmed:
						confs = tBondConfs
					default:
					}
				}
				return amount, lockTime, confs, user.acctID, nil
			}

			// Execute the postbond command and collect the client response.
			responses := make(chan *msgjson.Message, 1)
			msg, _ := tNewPostBondRequest(t, user, assetID, tt.coinID)
			rpcErr := meshSvc.ExecuteCommand(context.Background(), mesh.CommandRequest{
				Kind: commandKindPostBond,
				User: user.acctID,
				Msg:  msg,
				Respond: func(resp *msgjson.Message) error {
					responses <- resp
					return nil
				},
			})
			if rpcErr != nil {
				t.Fatalf("Execute postbond command error: %v", rpcErr)
			}

			var sent *msgjson.Message
			// Confirmed and existing bonds respond now; delayed bonds wait for the queue.
			select {
			case sent = <-responses:
				if tt.delayedConfirm {
					t.Fatalf("unexpected response before delayed confirmation: %v", sent)
				}
			default:
				if !tt.delayedConfirm {
					t.Fatal("no immediate postbond response")
				}
			}

			if tt.delayedConfirm {
				// The waiter emits the event and responds after the bond reaches confirmations.
				close(confirmed)
				select {
				case sent = <-responses:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for delayed postbond response")
				}
			}

			// All successful postbond commands return a signed result.
			if sent == nil {
				t.Fatal("no postbond result delivered")
			}
			if sent.ID != msg.ID {
				t.Fatalf("wrong response id. got %d, want %d", sent.ID, msg.ID)
			}
			resp, err := sent.Response()
			if err != nil {
				t.Fatalf("postbond response decode: %v", err)
			}
			var result msgjson.PostBondResult
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				t.Fatalf("postbond result decode: %v", err)
			}
			if len(result.SigBytes()) == 0 {
				t.Fatalf("postbond result was not signed")
			}
			if result.Reputation == nil {
				t.Fatalf("postbond result did not include reputation")
			}
			if tt.existingBond && result.Reputation.BondedTier != int64(strength) {
				t.Fatalf("postbond reputation = %+v, want bonded tier %d", result.Reputation, strength)
			}

			// New bonds publish exactly one event; existing bonds only return the result.
			wantEvents := 0
			if tt.wantEvent {
				wantEvents = 1
			}
			if len(capturedEvents) != wantEvents {
				t.Fatalf("captured %d bond posted events, want %d", len(capturedEvents), wantEvents)
			}
			if !tt.wantEvent {
				return
			}

			// The event payload should describe the accepted bond.
			if capturedEvents[0].Kind != meshevents.EventKindBondPosted {
				t.Fatalf("wrong event kind %q for bond posted", capturedEvents[0].Kind)
			}
			posted, err := meshevents.DecodeBondPostedEvent(capturedEvents[0].Payload)
			if err != nil {
				t.Fatalf("DecodeBondPostedEvent error: %v", err)
			}
			if posted.Account == nil || posted.Account.AccountID != user.acctID {
				t.Fatalf("wrong event account in payload")
			}
			if posted.Bond == nil || posted.Bond.AssetID != assetID || !bytes.Equal(posted.Bond.CoinID, tt.coinID) {
				t.Fatalf("wrong event bond in payload")
			}
		})
	}
}

func hasBondWaiter(authMgr *AuthManager, key string) bool {
	authMgr.bondWaiterMtx.Lock()
	defer authMgr.bondWaiterMtx.Unlock()
	_, found := authMgr.bondWaiterIdx[key]
	return found
}

func executePrepaidPostBondForTest(t *testing.T, authMgr *AuthManager, user *tUser, coinID []byte, capturedEvents *[]*mesh.Event) (*msgjson.Message, *msgjson.Error) {
	t.Helper()
	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Commands:       authMgr.Commands(),
		Events:         eventsWithCapture(authMgr, capturedEvents),
		Logger:         dex.Disabled,
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}

	responses := make(chan *msgjson.Message, 1)
	msg, _ := tNewPostBondRequest(t, user, account.PrepaidBondID, coinID)
	rpcErr := meshSvc.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: commandKindPostBond,
		User: user.acctID,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			responses <- resp
			return nil
		},
	})
	select {
	case resp := <-responses:
		return resp, rpcErr
	default:
		return nil, rpcErr
	}
}

func TestExecutePrepaidPostBond(t *testing.T) {
	user := tNewUser(t)
	validLockTime := time.Now().Add(72 * time.Hour).Unix()
	validCoinID := bytes.Repeat([]byte{0x05}, 16)

	t.Run("new pre-paid bond emits bond posted event", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		authMgr.signer.(*TSigner).setSig(user.randomSignature())
		storage.prepaidBonds = map[string]*meshevents.PrepaidBond{
			string(validCoinID): {
				CoinID:   validCoinID,
				Strength: 4,
				LockTime: validLockTime,
			},
		}
		authMgr.checkBond = func(context.Context, uint32, uint16, []byte) (int64, int64, int64, account.AccountID, error) {
			t.Fatal("checkBond called for pre-paid bond")
			return 0, 0, 0, account.AccountID{}, nil
		}

		var capturedEvents []*mesh.Event
		resp, rpcErr := executePrepaidPostBondForTest(t, authMgr, user, validCoinID, &capturedEvents)
		if rpcErr != nil {
			t.Fatalf("Execute postbond command error: %v", rpcErr)
		}
		if resp == nil {
			t.Fatal("no pre-paid postbond response")
		}
		result := decodePostBondResult(t, resp)
		if result.AssetID != account.PrepaidBondID || result.Amount != 0 || result.Strength != 4 || !bytes.Equal(result.BondID, validCoinID) {
			t.Fatalf("wrong pre-paid postbond result: %+v", result)
		}
		if len(result.SigBytes()) == 0 {
			t.Fatal("pre-paid postbond result was not signed")
		}
		if len(capturedEvents) != 1 || capturedEvents[0].Kind != meshevents.EventKindBondPosted {
			t.Fatalf("captured events = %+v, want one bond_posted event", capturedEvents)
		}
		if storage.bondPostedUpdate == nil || storage.bondPostedUpdate.Bond == nil ||
			storage.bondPostedUpdate.Bond.AssetID != account.PrepaidBondID ||
			!bytes.Equal(storage.bondPostedUpdate.Bond.CoinID, validCoinID) {
			t.Fatalf("wrong bond posted storage update: %+v", storage.bondPostedUpdate)
		}
		if storage.prepaidBonds[string(validCoinID)] != nil {
			t.Fatalf("pre-paid token was not consumed")
		}
		if hasBondWaiter(authMgr, bondKey(account.PrepaidBondID, validCoinID)) {
			t.Fatalf("pre-paid bond waiter was not removed after success")
		}
	})

	t.Run("same account retry completes without event or token", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		authMgr.signer.(*TSigner).setSig(user.randomSignature())
		acct := &account.Account{
			ID:     user.acctID,
			PubKey: user.privKey.PubKey(),
		}
		if err := storage.CreateAccountWithBond(acct, &db.Bond{
			AssetID:  account.PrepaidBondID,
			CoinID:   validCoinID,
			Strength: 4,
			LockTime: validLockTime,
		}); err != nil {
			t.Fatalf("CreateAccountWithBond error: %v", err)
		}
		storage.prepaidBonds = map[string]*meshevents.PrepaidBond{}

		var capturedEvents []*mesh.Event
		resp, rpcErr := executePrepaidPostBondForTest(t, authMgr, user, validCoinID, &capturedEvents)
		if rpcErr != nil {
			t.Fatalf("Execute postbond command error: %v", rpcErr)
		}
		if resp == nil {
			t.Fatal("no retry postbond response")
		}
		result := decodePostBondResult(t, resp)
		if result.Reputation == nil || result.Reputation.BondedTier != 4 {
			t.Fatalf("retry reputation = %+v, want bonded tier 4", result.Reputation)
		}
		if len(capturedEvents) != 0 {
			t.Fatalf("same-account retry emitted %d events, want 0", len(capturedEvents))
		}
	})

	t.Run("unknown token releases waiter", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		authMgr.signer.(*TSigner).setSig(user.randomSignature())
		storage.prepaidBonds = map[string]*meshevents.PrepaidBond{}

		var capturedEvents []*mesh.Event
		resp, rpcErr := executePrepaidPostBondForTest(t, authMgr, user, validCoinID, &capturedEvents)
		if resp != nil {
			t.Fatalf("unexpected pre-paid postbond response: %v", resp)
		}
		if rpcErr == nil || rpcErr.Code != msgjson.BondError {
			t.Fatalf("unknown token error = %v, want BondError", rpcErr)
		}
		if hasBondWaiter(authMgr, bondKey(account.PrepaidBondID, validCoinID)) {
			t.Fatalf("pre-paid bond waiter was not removed after unknown token")
		}
	})
}

func TestRequestWithTimeoutProxiesThroughMesh(t *testing.T) {
	user := tNewUser(t)
	req, err := msgjson.NewRequest(comms.NextID(), msgjson.PreimageRoute, map[string]string{"market": "dcr_btc"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	origID := req.ID
	origRoute := req.Route
	origPayload := append([]byte(nil), req.Payload...)
	resp, err := msgjson.NewResponse(req.ID, map[string]string{"status": "ok"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}

	proxyReady := make(chan struct{}, 1)
	meshReq := &tMesh{proxyReady: proxyReady}
	restore := setTestMeshService(meshReq)
	defer restore()

	got := make(chan *msgjson.Message, 1)
	expired := make(chan struct{}, 1)
	err = rig.mgr.RequestWithTimeout(user.acctID, req, func(conn comms.Link, msg *msgjson.Message) {
		if conn != nil {
			t.Errorf("expected nil proxied conn, got %T", conn)
		}
		got <- msg
	}, 123*time.Millisecond, func() {
		expired <- struct{}{}
	})
	if err != nil {
		t.Fatalf("RequestWithTimeout error: %v", err)
	}

	select {
	case <-proxyReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxied request")
	}

	req.ID = comms.NextID()
	req.Route = "mutated_route"
	req.Payload = []byte(`{"market":"ltc_btc"}`)

	if err := rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User: user.acctID,
		Msg:  resp,
	}); err != nil {
		t.Fatalf("ProxyClientMessage response error: %v", err)
	}

	select {
	case msg := <-got:
		if msg == resp {
			t.Fatalf("proxied response used mesh-owned message pointer")
		}
		if msg.ID != resp.ID {
			t.Fatalf("proxied response id = %d, want %d", msg.ID, resp.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxied callback")
	}

	select {
	case <-expired:
		t.Fatal("unexpected expire callback")
	default:
	}

	if meshReq.proxyCount != 1 {
		t.Fatalf("proxy count = %d, want 1", meshReq.proxyCount)
	}
	if meshReq.proxiedUser != user.acctID {
		t.Fatalf("proxied user = %v, want %v", meshReq.proxiedUser, user.acctID)
	}
	if meshReq.proxiedMsg == req {
		t.Fatalf("proxied request used caller-owned message pointer")
	}
	if meshReq.proxiedMsg.ID != origID {
		t.Fatalf("proxied request id = %d, want %d", meshReq.proxiedMsg.ID, origID)
	}
	if meshReq.proxiedMsg.Route != origRoute {
		t.Fatalf("proxied route = %q, want %q", meshReq.proxiedMsg.Route, origRoute)
	}
	if !bytes.Equal(meshReq.proxiedMsg.Payload, origPayload) {
		t.Fatalf("proxied payload = %s, want %s", meshReq.proxiedMsg.Payload, origPayload)
	}
	if meshReq.proxiedTimeout != 123*time.Millisecond {
		t.Fatalf("proxied timeout = %v, want %v", meshReq.proxiedTimeout, 123*time.Millisecond)
	}
}

type reputationForgivenessTestEnv struct {
	user     *tUser
	authMgr  *AuthManager
	storage  *TStorage
	captured []*mesh.Event
	meshSvc  *mesh.Service
}

func newReputationForgivenessTestEnv(t *testing.T) *reputationForgivenessTestEnv {
	t.Helper()
	user := tNewUser(t)
	authMgr, storage := newEventTestAuthManager(t)
	storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}
	storage.bonds = []*db.Bond{{Strength: 1, LockTime: time.Now().Add(48 * time.Hour).Unix()}}
	env := &reputationForgivenessTestEnv{
		user:    user,
		authMgr: authMgr,
		storage: storage,
	}
	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Commands:       authMgr.Commands(),
		Events:         eventsWithCapture(authMgr, &env.captured),
		Logger:         dex.Disabled,
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}
	env.meshSvc = meshSvc
	return env
}

func (env *reputationForgivenessTestEnv) userRequest() *reputationForgivenessRequest {
	return &reputationForgivenessRequest{
		AccountID: env.user.acctID,
		Scope:     meshevents.ReputationForgivenessScopeUser,
	}
}

func (env *reputationForgivenessTestEnv) matchRequest(matchID order.MatchID) *reputationForgivenessRequest {
	return &reputationForgivenessRequest{
		AccountID: env.user.acctID,
		Scope:     meshevents.ReputationForgivenessScopeMatch,
		MatchID:   matchID,
	}
}

func (env *reputationForgivenessTestEnv) execute(t *testing.T, user account.AccountID, req *reputationForgivenessRequest) (*reputationForgivenessResult, *msgjson.Error) {
	t.Helper()
	msg, err := msgjson.NewRequest(comms.NextID(), commandKindForgiveReputation, req)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	responses := make(chan *msgjson.Message, 1)
	rpcErr := env.meshSvc.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: commandKindForgiveReputation,
		User: user,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			responses <- resp
			return nil
		},
	})
	if rpcErr != nil {
		return nil, rpcErr
	}
	select {
	case resp := <-responses:
		var result reputationForgivenessResult
		if err := resp.UnmarshalResult(&result); err != nil {
			t.Fatalf("UnmarshalResult error: %v", err)
		}
		return &result, nil
	default:
		t.Fatalf("no reputation forgiveness response")
		return nil, nil
	}
}

func executeForgivenessWrapper(t *testing.T, authMgr *AuthManager, user account.AccountID, timeout time.Duration) (*reputationForgivenessResult, error) {
	t.Helper()
	return authMgr.executeReputationForgivenessCommand(context.Background(), &reputationForgivenessRequest{
		AccountID: user,
		Scope:     meshevents.ReputationForgivenessScopeUser,
	}, timeout)
}

func decodeReputationForgivenTestEvent(t *testing.T, event *meshevents.ReputationForgivenEvent) *meshevents.ReputationForgivenEvent {
	t.Helper()
	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	decoded, err := meshevents.DecodeReputationForgivenEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	return decoded
}

func TestExecuteForgiveReputation(t *testing.T) {
	t.Run("user scope emits event with captured storage result", func(t *testing.T) {
		env := newReputationForgivenessTestEnv(t)
		env.storage.applyReputationForgiven = func(_ context.Context, meta *db.EventLogMeta, update *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
			return &db.ReputationForgivenResult{
				Forgiven: false,
				Log:      testReputationForgivenLog(meta, update),
			}, nil
		}

		result, rpcErr := env.execute(t, env.user.acctID, env.userRequest())
		if rpcErr != nil {
			t.Fatalf("ExecuteCommand error: %v", rpcErr)
		}
		if result.Forgiven {
			t.Fatalf("Forgiven = true, want exact captured false")
		}
		if !result.Unbanned {
			t.Fatalf("Unbanned = false, want true")
		}
		if len(env.captured) != 1 || env.captured[0].Kind != meshevents.EventKindReputationForgiven {
			t.Fatalf("captured events = %+v, want one reputation forgiveness event", env.captured)
		}
		if env.storage.reputationForgivenUpdate == nil || env.storage.reputationForgivenUpdate.Scope != meshevents.ReputationForgivenessScopeUser ||
			env.storage.reputationForgivenUpdate.AccountID != env.user.acctID {
			t.Fatalf("wrong storage update: %+v", env.storage.reputationForgivenUpdate)
		}
	})

	t.Run("match scope emits bare match event", func(t *testing.T) {
		env := newReputationForgivenessTestEnv(t)
		matchID := randomMatchID()

		result, rpcErr := env.execute(t, env.user.acctID, env.matchRequest(matchID))
		if rpcErr != nil {
			t.Fatalf("ExecuteCommand error: %v", rpcErr)
		}
		if !result.Forgiven || !result.Unbanned {
			t.Fatalf("result = %+v, want forgiven and unbanned", result)
		}
		if env.storage.reputationForgivenUpdate == nil || env.storage.reputationForgivenUpdate.Match() != matchID {
			t.Fatalf("wrong storage update: %+v", env.storage.reputationForgivenUpdate)
		}
		if len(env.captured) != 1 {
			t.Fatalf("captured %d events, want 1", len(env.captured))
		}
		event, err := meshevents.DecodeReputationForgivenEvent(env.captured[0].Payload)
		if err != nil {
			t.Fatalf("DecodeReputationForgivenEvent error: %v", err)
		}
		if event.Scope != meshevents.ReputationForgivenessScopeMatch || event.MatchID == nil || *event.MatchID != matchID {
			t.Fatalf("wrong event payload: %+v", event)
		}
	})

	t.Run("match scope storage rejection returns error", func(t *testing.T) {
		env := newReputationForgivenessTestEnv(t)
		env.storage.applyReputationForgiven = func(context.Context, *db.EventLogMeta, *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
			return nil, errors.New("match is not eligible")
		}
		matchID := randomMatchID()

		_, rpcErr := env.execute(t, env.user.acctID, env.matchRequest(matchID))
		if rpcErr == nil {
			t.Fatalf("ExecuteCommand succeeded for rejected match")
		}
		const wantErr = "failed to apply reputation forgiveness: match is not eligible"
		if rpcErr.Message != wantErr {
			t.Fatalf("ExecuteCommand error = %q, want %q", rpcErr.Message, wantErr)
		}
		if env.storage.reputationForgivenUpdate == nil || env.storage.reputationForgivenUpdate.AccountID != env.user.acctID ||
			env.storage.reputationForgivenUpdate.Match() != matchID {
			t.Fatalf("wrong rejected storage update: %+v", env.storage.reputationForgivenUpdate)
		}
		if len(env.captured) != 1 {
			t.Fatalf("captured %d events, want attempted event", len(env.captured))
		}
	})

	t.Run("account mismatch rejected", func(t *testing.T) {
		env := newReputationForgivenessTestEnv(t)
		other := tNewUser(t)
		_, rpcErr := env.execute(t, other.acctID, env.userRequest())
		if rpcErr == nil {
			t.Fatalf("ExecuteCommand succeeded for account mismatch")
		}
	})
}

func TestReputationForgivenessCommandWrapper(t *testing.T) {
	user := tNewUser(t)

	t.Run("execute error", func(t *testing.T) {
		authMgr, _ := newEventTestAuthManager(t)
		wantErr := msgjson.NewError(msgjson.RPCInternalError, "mesh failed")
		authMgr.SetMeshService(&tMesh{executeErr: wantErr})
		_, err := executeForgivenessWrapper(t, authMgr, user.acctID, time.Second)
		if err != wantErr {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})

	t.Run("synchronous execution observes timeout", func(t *testing.T) {
		authMgr, _ := newEventTestAuthManager(t)
		authMgr.SetMeshService(&tMesh{
			executeHook: func(ctx context.Context, _ mesh.CommandRequest) *msgjson.Error {
				<-ctx.Done()
				return nil
			},
		})
		_, err := executeForgivenessWrapper(t, authMgr, user.acctID, time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("executeReputationForgivenessCommand error = %v, want deadline exceeded", err)
		}
	})

	t.Run("malformed result", func(t *testing.T) {
		authMgr, _ := newEventTestAuthManager(t)
		authMgr.SetMeshService(&tMesh{
			executeHook: func(_ context.Context, req mesh.CommandRequest) *msgjson.Error {
				resp, err := msgjson.NewResponse(req.Msg.ID, map[string]string{"forgiven": "not-bool"}, nil)
				if err != nil {
					t.Fatalf("NewResponse error: %v", err)
				}
				if err := req.Respond(resp); err != nil {
					t.Fatalf("Respond error: %v", err)
				}
				return nil
			},
		})
		_, err := executeForgivenessWrapper(t, authMgr, user.acctID, time.Second)
		if err == nil {
			t.Fatalf("executeReputationForgivenessCommand succeeded with malformed result")
		}
	})
}

func TestApplyBondPostedEvent(t *testing.T) {
	newTestEvent := func(t *testing.T, user *tUser, bond *db.Bond) *meshevents.BondPostedEvent {
		t.Helper()
		payload, err := (&meshevents.BondPostedEvent{
			Account: &meshevents.BondPostedAccount{
				AccountID: user.acctID,
				Pubkey:    user.privKey.PubKey().SerializeCompressed(),
			},
			Bond: wireBond(bond),
		}).Encode()
		if err != nil {
			t.Fatalf("Encode error: %v", err)
		}
		decoded, err := meshevents.DecodeBondPostedEvent(payload)
		if err != nil {
			t.Fatalf("DecodeBondPostedEvent error: %v", err)
		}
		return decoded
	}

	addTestClient := func(t *testing.T, authMgr *AuthManager, acct *account.Account, conn comms.Link) *clientInfo {
		t.Helper()
		client := &clientInfo{
			acct:         acct,
			conn:         conn,
			respHandlers: make(map[uint64]*respHandler),
		}
		authMgr.addClient(client)
		t.Cleanup(func() {
			authMgr.removeClient(client)
		})
		return client
	}

	t.Run("bond added applies storage update", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		acct := &account.Account{
			ID:     user.acctID,
			PubKey: user.privKey.PubKey(),
		}
		addTestClient(t, authMgr, acct, user.conn)
		bond := &db.Bond{
			Version:  0,
			AssetID:  42,
			CoinID:   []byte{0x09, 0x08, 0x07},
			Amount:   int64(tRegFee * 10),
			Strength: 1,
			LockTime: time.Now().Add(48 * time.Hour).Unix(),
		}
		tipHash := bytes.Repeat([]byte{0x7a}, db.EventLogTipHashSize)
		logMeta := &db.EventLogMeta{
			Seq:             7,
			Event:           []byte("bond-posted-event"),
			ExpectedTipHash: tipHash,
		}

		applied, err := authMgr.applyBondPostedEvent(context.Background(), logMeta, newTestEvent(t, user, bond))
		if err != nil {
			t.Fatalf("applyBondPostedEvent error: %v", err)
		}
		if storage.bondPostedUpdate == nil {
			t.Fatalf("bond posted update was not sent to storage")
		}
		if storage.bondPostedUpdate.Acct == nil || storage.bondPostedUpdate.Acct.ID != user.acctID {
			t.Fatalf("wrong storage account")
		}
		if storage.bondPostedUpdate.Bond == nil ||
			storage.bondPostedUpdate.Bond.AssetID != bond.AssetID ||
			!bytes.Equal(storage.bondPostedUpdate.Bond.CoinID, bond.CoinID) ||
			storage.bondPostedUpdate.Bond.Strength != bond.Strength {
			t.Fatalf("storage got wrong bond")
		}
		if applied == nil || applied.Seq != logMeta.Seq || applied.Kind != meshevents.EventKindBondPosted ||
			!bytes.Equal(applied.Event, logMeta.Event) || !bytes.Equal(applied.TipHash, tipHash) {
			t.Fatalf("wrong applied event: %+v", applied)
		}
	})

	t.Run("already applied returns no applied event", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		acct := &account.Account{
			ID:     user.acctID,
			PubKey: user.privKey.PubKey(),
		}
		addTestClient(t, authMgr, acct, user.conn)
		bond := &db.Bond{
			Version:  0,
			AssetID:  42,
			CoinID:   []byte{0x09, 0x08, 0x07},
			Amount:   int64(tRegFee * 10),
			Strength: 1,
			LockTime: time.Now().Add(48 * time.Hour).Unix(),
		}
		storage.applyBondPosted = func(context.Context, *db.EventLogMeta, *db.BondPostedUpdate) (*db.BondPostedResult, error) {
			return &db.BondPostedResult{}, nil
		}

		applied, err := authMgr.applyBondPostedEvent(context.Background(), nil, newTestEvent(t, user, bond))
		if err != nil {
			t.Fatalf("applyBondPostedEvent error: %v", err)
		}
		if applied != nil {
			t.Fatalf("applied event = %+v, want nil", applied)
		}
	})

	t.Run("storage error", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		bond := &db.Bond{
			Version:  0,
			AssetID:  42,
			CoinID:   []byte{0x09, 0x08, 0x07},
			Amount:   int64(tRegFee * 10),
			Strength: 1,
			LockTime: time.Now().Add(48 * time.Hour).Unix(),
		}
		wantErr := fmt.Errorf("storage failed")
		storage.applyBondPosted = func(context.Context, *db.EventLogMeta, *db.BondPostedUpdate) (*db.BondPostedResult, error) {
			return nil, wantErr
		}

		_, err := authMgr.applyBondPostedEvent(context.Background(), nil, newTestEvent(t, user, bond))
		if !errors.Is(err, wantErr) {
			t.Fatalf("applyBondPostedEvent error = %v, want %v", err, wantErr)
		}
	})

	t.Run("account id mismatch rejected before storage", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		other := tNewUser(t)
		bond := &db.Bond{
			Version:  0,
			AssetID:  42,
			CoinID:   []byte{0x09, 0x08, 0x07},
			Amount:   int64(tRegFee * 10),
			Strength: 1,
			LockTime: time.Now().Add(48 * time.Hour).Unix(),
		}
		event := newTestEvent(t, user, bond)
		event.Account.AccountID = other.acctID

		if _, err := authMgr.applyBondPostedEvent(context.Background(), nil, event); err == nil {
			t.Fatalf("applyBondPostedEvent succeeded with mismatched account id")
		}
		if storage.bondPostedUpdate != nil {
			t.Fatalf("storage was called for invalid event")
		}
	})
}

func TestApplyReputationForgivenEvent(t *testing.T) {
	t.Run("applies storage update and captures result", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}
		update := &meshevents.ReputationForgivenEvent{
			AccountID: user.acctID,
			Scope:     meshevents.ReputationForgivenessScopeUser,
		}
		tipHash := bytes.Repeat([]byte{0x7b}, db.EventLogTipHashSize)
		logMeta := &db.EventLogMeta{
			Seq:             9,
			Event:           []byte("reputation-forgiven-event"),
			ExpectedTipHash: tipHash,
		}
		storage.applyReputationForgiven = func(_ context.Context, meta *db.EventLogMeta, update *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
			return &db.ReputationForgivenResult{
				Forgiven: false,
				Log:      testReputationForgivenLog(meta, update),
			}, nil
		}
		applyCtx := &mesh.EventApplyContext{Context: context.Background()}

		applied, err := authMgr.applyReputationForgivenEvent(applyCtx, logMeta, decodeReputationForgivenTestEvent(t, update))
		if err != nil {
			t.Fatalf("applyReputationForgivenEvent error: %v", err)
		}
		if storage.reputationForgivenUpdate == nil || storage.reputationForgivenUpdate.AccountID != user.acctID {
			t.Fatalf("storage update not applied: %+v", storage.reputationForgivenUpdate)
		}
		if applied == nil || applied.Seq != logMeta.Seq || applied.Kind != meshevents.EventKindReputationForgiven ||
			!bytes.Equal(applied.Event, logMeta.Event) || !bytes.Equal(applied.TipHash, tipHash) {
			t.Fatalf("wrong applied event: %+v", applied)
		}
		if captured, _ := applyCtx.Result().(*reputationForgivenessResult); captured == nil || captured.Forgiven {
			t.Fatalf("command result = %+v, want captured false result", captured)
		}
	})

	t.Run("post reputation read timeout returns applied log", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		update := &meshevents.ReputationForgivenEvent{
			AccountID: user.acctID,
			Scope:     meshevents.ReputationForgivenessScopeUser,
		}

		var readCalls atomic.Int32
		storage.getUserReputationData = func(ctx context.Context, _ account.AccountID, _, _, _ int) ([]*db.PreimageOutcome, []*db.MatchResult, []*db.OrderOutcome, error) {
			readCalls.Add(1)
			<-ctx.Done()
			return nil, nil, nil, ctx.Err()
		}

		baseCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		applyCtx := &mesh.EventApplyContext{Context: baseCtx}

		start := time.Now()
		applied, err := authMgr.applyReputationForgivenEvent(applyCtx, &db.EventLogMeta{Event: []byte("post-read-timeout")}, decodeReputationForgivenTestEvent(t, update))
		if err != nil {
			t.Fatalf("applyReputationForgivenEvent error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("applyReputationForgivenEvent returned after %v, want bounded timeout", elapsed)
		}
		if applied == nil || applied.Kind != meshevents.EventKindReputationForgiven {
			t.Fatalf("applied event = %+v, want reputation forgiveness log", applied)
		}
		if readCalls.Load() != 1 {
			t.Fatalf("reputation reads = %d, want 1 post-apply Unbanned load", readCalls.Load())
		}
		if result, _ := applyCtx.Result().(*reputationForgivenessResult); result == nil || !result.Forgiven || result.Unbanned {
			t.Fatalf("command result = %+v, want forgiven with unknown unbanned state", result)
		}
	})

	t.Run("unknown account after apply does not panic", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		user := tNewUser(t)
		authMgr.signer.(*TSigner).setSig(user.randomSignature())
		update := &meshevents.ReputationForgivenEvent{
			AccountID: user.acctID,
			Scope:     meshevents.ReputationForgivenessScopeUser,
		}
		storage.applyReputationForgiven = func(_ context.Context, meta *db.EventLogMeta, update *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
			storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}
			storage.bonds = []*db.Bond{{Strength: 1, LockTime: time.Now().Add(48 * time.Hour).Unix()}}
			return &db.ReputationForgivenResult{
				Forgiven: true,
				Log:      testReputationForgivenLog(meta, update),
			}, nil
		}
		if _, err := authMgr.applyReputationForgivenEvent(&mesh.EventApplyContext{Context: context.Background()}, &db.EventLogMeta{Event: []byte("unknown-acct")}, decodeReputationForgivenTestEvent(t, update)); err != nil {
			t.Fatalf("applyReputationForgivenEvent error: %v", err)
		}
	})

	// The Validate matrix is covered in meshevents; this only proves an
	// invalid event is rejected before storage is touched.
	t.Run("validation rejects bad events before storage", func(t *testing.T) {
		authMgr, storage := newEventTestAuthManager(t)
		zeroAccount := &meshevents.ReputationForgivenEvent{Scope: meshevents.ReputationForgivenessScopeUser}
		payload, err := zeroAccount.Encode()
		if err != nil {
			t.Fatalf("Encode error: %v", err)
		}
		applier := authMgr.Events()[meshevents.EventKindReputationForgiven]
		if _, err := applier(&mesh.EventApplyContext{Context: context.Background()}, &mesh.Event{Payload: payload}); err == nil {
			t.Fatalf("applying invalid reputation_forgiven event succeeded")
		}
		if storage.reputationForgivenUpdate != nil {
			t.Fatalf("storage was called for invalid event")
		}
	})
}

func TestRequestViaMeshFailureAndTimeout(t *testing.T) {
	user := tNewUser(t)
	req, err := msgjson.NewRequest(comms.NextID(), msgjson.PreimageRoute, map[string]string{"market": "dcr_btc"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}

	expired := make(chan struct{}, 1)
	meshReq := &tMesh{
		proxiedErr: errors.New("proxy failed"),
	}
	restore := setTestMeshService(meshReq)
	err = rig.mgr.RequestWithTimeout(user.acctID, req, func(comms.Link, *msgjson.Message) {
		t.Fatalf("unexpected proxied request callback")
	}, time.Second, func() {
		expired <- struct{}{}
	})
	restore()
	if err != nil {
		t.Fatalf("RequestWithTimeout error: %v", err)
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy failure expiration")
	}

	req, err = msgjson.NewRequest(comms.NextID(), msgjson.PreimageRoute, map[string]string{"market": "dcr_btc"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	meshReq = &tMesh{}
	restore = setTestMeshService(meshReq)
	err = rig.mgr.RequestWithTimeout(user.acctID, req, func(comms.Link, *msgjson.Message) {
		t.Fatalf("unexpected proxied request callback")
	}, time.Millisecond, func() {
		expired <- struct{}{}
	})
	restore()
	if err != nil {
		t.Fatalf("RequestWithTimeout error: %v", err)
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy request expiration")
	}

	acct, err := account.NewAccountFromPubKey(user.privKey.PubKey().SerializeCompressed())
	if err != nil {
		t.Fatalf("NewAccountFromPubKey error: %v", err)
	}
	client := &clientInfo{
		acct:         acct,
		conn:         user.conn,
		respHandlers: make(map[uint64]*respHandler),
	}
	rig.mgr.addClient(client)
	defer func() {
		if rig.mgr.user(user.acctID) != nil {
			rig.mgr.removeClient(client)
		}
	}()

	lateResp, err := msgjson.NewResponse(req.ID, map[string]string{"status": "late"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	if err := rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User: user.acctID,
		Msg:  lateResp,
	}); err != nil {
		t.Fatalf("late ProxyClientMessage error: %v", err)
	}
	if user.conn.getSend() != nil {
		t.Fatalf("late client response was delivered to the client")
	}
}

func TestProxyClientMessageResponseDelivery(t *testing.T) {
	user := tNewUser(t)
	acct, err := account.NewAccountFromPubKey(user.privKey.PubKey().SerializeCompressed())
	if err != nil {
		t.Fatalf("NewAccountFromPubKey error: %v", err)
	}
	client := &clientInfo{
		acct:         acct,
		conn:         user.conn,
		respHandlers: make(map[uint64]*respHandler),
	}
	rig.mgr.addClient(client)
	defer func() {
		if rig.mgr.user(user.acctID) != nil {
			rig.mgr.removeClient(client)
		}
	}()

	resp, err := msgjson.NewResponse(comms.NextID(), map[string]string{"status": "ok"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	if err := rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User:            user.acctID,
		Msg:             resp,
		DeliverToClient: true,
	}); err != nil {
		t.Fatalf("ProxyClientMessage response delivery error: %v", err)
	}
	sent := user.conn.getSend()
	if sent == nil {
		t.Fatal("server response was not delivered to the client")
	}
	if sent.ID != resp.ID {
		t.Fatalf("sent response id = %d, want %d", sent.ID, resp.ID)
	}

	resp, err = msgjson.NewResponse(comms.NextID(), map[string]string{"status": "fail"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	sendErr := errors.New("send failed")
	user.conn.sendErr = sendErr
	err = rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User:            user.acctID,
		Msg:             resp,
		DeliverToClient: true,
	})
	user.conn.sendErr = nil
	if !errors.Is(err, sendErr) {
		t.Fatalf("ProxyClientMessage send error = %v, want %v", err, sendErr)
	}
}

func TestProxyClientMessageRequest(t *testing.T) {
	user := tNewUser(t)
	acct, err := account.NewAccountFromPubKey(user.privKey.PubKey().SerializeCompressed())
	if err != nil {
		t.Fatalf("NewAccountFromPubKey error: %v", err)
	}
	rig.mgr.addClient(&clientInfo{
		acct:         acct,
		conn:         user.conn,
		respHandlers: make(map[uint64]*respHandler),
	})
	defer rig.mgr.removeClient(rig.mgr.user(user.acctID))

	req, err := msgjson.NewRequest(comms.NextID(), msgjson.PreimageRoute, map[string]string{"market": "dcr_btc"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}

	proxyReady := make(chan struct{}, 1)
	meshReq := &tMesh{proxyReady: proxyReady}
	restore := setTestMeshService(meshReq)
	defer restore()

	err = rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User:      user.acctID,
		Msg:       req,
		TimeoutMS: uint64(time.Second / time.Millisecond),
	})
	if err != nil {
		t.Fatalf("ProxyClientMessage request error: %v", err)
	}

	var localReq *tReq
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		localReq = user.conn.getReq()
		if localReq != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if localReq == nil {
		t.Fatal("no local proxied request sent")
	}
	if localReq.msg.ID == req.ID {
		t.Fatalf("proxied local request id was not rewritten")
	}
	if localReq.msg.Route != req.Route {
		t.Fatalf("proxied local request route = %q, want %q", localReq.msg.Route, req.Route)
	}

	localResp, err := msgjson.NewResponse(localReq.msg.ID, map[string]string{"status": "ok"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	localReq.respFunc(user.conn, localResp)

	select {
	case <-proxyReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxied client response")
	}

	if meshReq.proxyCount != 1 {
		t.Fatalf("proxy count = %d, want 1", meshReq.proxyCount)
	}
	if meshReq.proxiedUser != user.acctID {
		t.Fatalf("proxied response user = %v, want %v", meshReq.proxiedUser, user.acctID)
	}
	if meshReq.proxiedMsg == nil {
		t.Fatal("nil proxied response")
	}
	if meshReq.proxiedMsg.ID != req.ID {
		t.Fatalf("proxied response id = %d, want %d", meshReq.proxiedMsg.ID, req.ID)
	}
	resp, err := meshReq.proxiedMsg.Response()
	if err != nil {
		t.Fatalf("proxied Response error: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("proxied response result unmarshal error: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("unexpected proxied response payload: %#v", payload)
	}
}

func TestAuth(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)

	msgBytes := randBytes(50)
	sigBytes := signMsg(user.privKey, msgBytes)
	err := rig.mgr.Auth(user.acctID, msgBytes, sigBytes)
	if err != nil {
		t.Fatalf("unexpected auth error: %v", err)
	}

	foreigner := tNewUser(t)
	sigBytes = signMsg(user.privKey, msgBytes)
	err = rig.mgr.Auth(foreigner.acctID, msgBytes, sigBytes)
	if err == nil {
		t.Fatalf("no auth error for foreigner")
	}

	msgBytes = randBytes(50)
	err = rig.mgr.Auth(user.acctID, msgBytes, sigBytes)
	if err == nil {
		t.Fatalf("no error for wrong message")
	}
}

func TestSign(t *testing.T) {
	sig1 := tNewUser(t).randomSignature()
	sig1Bytes := sig1.Serialize()
	rig.signer.setSig(sig1)
	s := &tSignable{b: randBytes(25)}
	rig.mgr.Sign(s)
	if !bytes.Equal(sig1Bytes, s.SigBytes()) {
		t.Fatalf("incorrect signature. expected %x, got %x", sig1.Serialize(), s.SigBytes())
	}

	// Try two at a time
	s2 := &tSignable{b: randBytes(25)}
	rig.mgr.Sign(s, s2)
}

func TestSend(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)
	foreigner := tNewUser(t)

	type tA struct {
		A int
	}
	payload := &tA{A: 5}
	resp, _ := msgjson.NewResponse(comms.NextID(), payload, nil)
	payload = &tA{A: 10}
	req, _ := msgjson.NewRequest(comms.NextID(), "testroute", payload)

	// Send a message to a foreigner
	rig.mgr.Send(foreigner.acctID, resp)
	if foreigner.conn.getSend() != nil {
		t.Fatalf("message magically got through to foreigner")
	}
	if user.conn.getSend() != nil {
		t.Fatalf("foreigner message sent to authed user")
	}

	// Mesh-aware Send proxies to non-local users.
	meshReq := &tMesh{}
	restore := setTestMeshService(meshReq)
	if err := rig.mgr.Send(foreigner.acctID, resp); err != nil {
		t.Fatalf("mesh Send error: %v", err)
	}
	restore()
	if meshReq.proxyCount != 1 {
		t.Fatalf("mesh proxy count = %d, want 1", meshReq.proxyCount)
	}
	if meshReq.proxiedUser != foreigner.acctID {
		t.Fatalf("mesh proxied user = %v, want %v", meshReq.proxiedUser, foreigner.acctID)
	}
	if meshReq.proxiedMsg == resp {
		t.Fatalf("mesh Send used caller-owned message pointer")
	}
	if meshReq.proxiedMsg.ID != resp.ID {
		t.Fatalf("mesh proxied msg id = %d, want %d", meshReq.proxiedMsg.ID, resp.ID)
	}
	if !meshReq.proxiedDeliver {
		t.Fatalf("mesh Send did not mark response for client delivery")
	}

	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Logger:         dex.Disabled,
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}
	restore = setTestMeshService(meshSvc)
	err = rig.mgr.Send(foreigner.acctID, resp)
	if !errors.Is(err, ErrUserNotConnected) {
		t.Fatalf("single-server mesh Send error = %v, want %v", err, ErrUserNotConnected)
	}
	expired := make(chan struct{}, 1)
	err = rig.mgr.RequestWithTimeout(foreigner.acctID, req, func(comms.Link, *msgjson.Message) {
		t.Fatalf("unexpected single-server mesh request callback")
	}, time.Millisecond, func() {
		expired <- struct{}{}
	})
	restore()
	if err != nil {
		t.Fatalf("single-server mesh RequestWithTimeout error: %v", err)
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for single-server mesh request expire callback")
	}

	// Now send to the user
	rig.mgr.Send(user.acctID, resp)
	msg := user.conn.getSend()
	if msg == nil {
		t.Fatalf("no message for authed user")
	}
	tr := new(tA)
	r, _ := msg.Response()
	err = json.Unmarshal(r.Result, tr)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if tr.A != 5 {
		t.Fatalf("expected A = 5, got A = %d", tr.A)
	}

	// Local-only send to a foreigner should be a no-op, not an error.
	if err := rig.mgr.SendIfLocal(foreigner.acctID, resp); err != nil {
		t.Fatalf("SendIfLocal error for foreigner: %v", err)
	}
	if foreigner.conn.getSend() != nil {
		t.Fatalf("SendIfLocal magically got through to foreigner")
	}

	if err := rig.mgr.SendIfLocal(user.acctID, resp); err != nil {
		t.Fatalf("SendIfLocal error for user: %v", err)
	}
	msg = user.conn.getSend()
	if msg == nil {
		t.Fatalf("no SendIfLocal message for authed user")
	}

	// Send a request to a foreigner
	rig.mgr.Request(foreigner.acctID, req, func(comms.Link, *msgjson.Message) {})
	if foreigner.conn.getReq() != nil {
		t.Fatalf("request magically got through to foreigner")
	}
	if user.conn.getReq() != nil {
		t.Fatalf("foreigner request sent to authed user")
	}

	// Send a request to an authed user.
	rig.mgr.Request(user.acctID, req, func(comms.Link, *msgjson.Message) {})
	treq := user.conn.getReq()
	if treq == nil {
		t.Fatalf("no request for user")
	}

	tr = new(tA)
	err = json.Unmarshal(treq.msg.Payload, tr)
	if err != nil {
		t.Fatalf("request unmarshal error: %v", err)
	}
	if tr.A != 10 {
		t.Fatalf("expected A = 10, got A = %d", tr.A)
	}
}

func TestSendPeerClientNotConnected(t *testing.T) {
	foreigner := tNewUser(t)
	resp, err := msgjson.NewResponse(comms.NextID(), map[string]string{"status": "ok"}, nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}

	// Delivering side: mesh needs only ErrClientNotConnected for the wire map.
	err = rig.mgr.HandleProxiedClientMessage(context.Background(), &mesh.ClientProxyMessage{
		User:            foreigner.acctID,
		Msg:             resp,
		DeliverToClient: true,
	})
	if !errors.Is(err, mesh.ErrClientNotConnected) {
		t.Fatalf("proxied delivery error = %v, want mesh.ErrClientNotConnected", err)
	}

	// Sending side: auth.Send maps the peer sentinel to ErrUserNotConnected.
	restore := setTestMeshService(&tMesh{proxiedErr: fmt.Errorf("%w to peer", mesh.ErrClientNotConnected)})
	defer restore()
	err = rig.mgr.Send(foreigner.acctID, resp)
	if !errors.Is(err, ErrUserNotConnected) {
		t.Fatalf("Send error = %v, want ErrUserNotConnected", err)
	}
}

func TestConnectErrors(t *testing.T) {
	user := tNewUser(t)
	rig.storage.acct = nil
	rig.signer.setSig(user.randomSignature())

	ensureErr := makeEnsureErr(t)

	// Test an invalid json payload
	msg, err := msgjson.NewRequest(comms.NextID(), "testreq", nil)
	if err != nil {
		t.Fatalf("NewRequest error for invalid payload: %v", err)
	}
	msg.Payload = []byte(`?`)
	rpcErr := rig.mgr.handleConnect(user.conn, msg)
	ensureErr(rpcErr, "invalid payload", msgjson.RPCParseError)

	connect := tNewConnect(user)
	encodeMsg := func() {
		msg, err = msgjson.NewRequest(comms.NextID(), "testreq", connect)
		if err != nil {
			t.Fatalf("NewRequest error for bad account ID: %v", err)
		}
	}
	// connect with an invalid ID
	connect.AccountID = []byte{0x01, 0x02, 0x03, 0x04}
	encodeMsg()
	rpcErr = rig.mgr.handleConnect(user.conn, msg)
	ensureErr(rpcErr, "invalid account ID", msgjson.AuthenticationError)
	connect.AccountID = user.acctID[:]

	// user unknown to storage
	encodeMsg()
	rpcErr = rig.mgr.handleConnect(user.conn, msg)
	ensureErr(rpcErr, "account unknown to storage", msgjson.AccountNotFoundError)
	rig.storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}

	// bad signature
	connect.SetSig([]byte{0x09, 0x08})
	encodeMsg()
	rpcErr = rig.mgr.handleConnect(user.conn, msg)
	ensureErr(rpcErr, "bad signature", msgjson.SignatureError)

	// A send error should not return an error, but the client should not be
	// saved to the map.
	// need to "register" the user first
	msgBytes := connect.Serialize()
	connect.SetSig(signMsg(user.privKey, msgBytes))
	encodeMsg()
	user.conn.sendErr = fmt.Errorf("test error")
	rpcErr = rig.mgr.handleConnect(user.conn, msg)
	if rpcErr != nil {
		t.Fatalf("non-nil msgjson.Error after send error: %s", rpcErr.Message)
	}
	user.conn.sendErr = nil
	if rig.mgr.user(user.acctID) != nil {
		t.Fatalf("user registered with send error")
	}
	// clear the response
	if user.conn.getSend() == nil {
		t.Fatalf("no response to clear")
	}

	// success
	rpcErr = rig.mgr.handleConnect(user.conn, msg)
	if rpcErr != nil {
		t.Fatalf("error for good connect: %s", rpcErr.Message)
	}
	// clear the response
	if user.conn.getSend() == nil {
		t.Fatalf("no response to clear")
	}
}

func TestHandleResponse(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)
	foreigner := tNewUser(t)
	unknownResponse, err := msgjson.NewResponse(comms.NextID(), 10, nil)
	if err != nil {
		t.Fatalf("error encoding unknown response: %v", err)
	}

	// test foreigner. Really just want to make sure that this returns before
	// trying to run a nil handler function, which would panic.
	rig.mgr.handleResponse(foreigner.conn, unknownResponse)

	// test for a missing handler
	rig.mgr.handleResponse(user.conn, unknownResponse)
	m := user.conn.getSend()
	if m == nil {
		t.Fatalf("no error sent for unknown response")
	}
	resp, _ := m.Response()
	if resp.Error == nil {
		t.Fatalf("error not set in response for unknown response")
	}
	if resp.Error.Code != msgjson.UnknownResponseID {
		t.Fatalf("wrong error code for unknown response. expected %d, got %d",
			msgjson.UnknownResponseID, resp.Error.Code)
	}

	// Check that expired response handlers are removed from the map.
	client := rig.mgr.user(user.acctID)
	if client == nil {
		t.Fatalf("client not found")
	}

	newID := comms.NextID()
	client.logReq(newID, func(comms.Link, *msgjson.Message) {},
		0, func() { t.Log("expired (ok)") })
	// Wait until response handler expires.
	if waitFor(func() bool {
		client.mtx.Lock()
		defer client.mtx.Unlock()
		return len(client.respHandlers) == 0
	}, 10*time.Second) {
		t.Fatalf("expected 0 response handlers, found %d", len(client.respHandlers))
	}
	client.mtx.Lock()
	if client.respHandlers[newID] != nil {
		t.Fatalf("response handler should have been expired")
	}
	client.mtx.Unlock()

	// After logging a new request, there should still be exactly one response handler
	// present. A short sleep is added to give a chance for clean-up running in a
	// separate go-routine to finish before we continue asserting on the result.
	newID = comms.NextID()
	client.logReq(newID, func(comms.Link, *msgjson.Message) {}, time.Hour, noop)
	time.Sleep(time.Millisecond)
	client.mtx.Lock()
	if len(client.respHandlers) != 1 {
		t.Fatalf("expected 1 response handler, found %d", len(client.respHandlers))
	}
	if client.respHandlers[newID] == nil {
		t.Fatalf("wrong response handler left after cleanup cycle")
	}
	client.mtx.Unlock()
}

func TestAuthManagerReputationOutcomePolicy(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)

	policy := rig.mgr.ReputationOutcomePolicy()
	if policy.PreimageLimit != scoringOrderLimit {
		t.Fatalf("PreimageLimit = %d, want %d", policy.PreimageLimit, scoringOrderLimit)
	}
	if policy.MatchLimit != ScoringMatchLimit {
		t.Fatalf("MatchLimit = %d, want %d", policy.MatchLimit, ScoringMatchLimit)
	}
	if policy.OrderLimit != cancelThreshWindow {
		t.Fatalf("OrderLimit = %d, want %d", policy.OrderLimit, cancelThreshWindow)
	}
	if policy.FreeCancelThreshold != freeCancelThreshold {
		t.Fatalf("FreeCancelThreshold = %d, want %d", policy.FreeCancelThreshold, freeCancelThreshold)
	}
}

func TestMatchStatus(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)

	rig.storage.matchStatuses = []*db.MatchStatus{{
		Status:    order.MakerSwapCast,
		IsTaker:   true,
		MakerSwap: []byte{0x01},
	}}

	tTxData := encode.RandomBytes(5)
	rig.mgr.txDataSources[0] = func([]byte) ([]byte, error) {
		return tTxData, nil
	}

	reqPayload := []msgjson.MatchRequest{{MatchID: encode.RandomBytes(32)}}

	req, _ := msgjson.NewRequest(1, msgjson.MatchStatusRoute, reqPayload)

	getStatus := func() *msgjson.MatchStatusResult {
		msgErr := rig.mgr.handleMatchStatus(user.conn, req)
		if msgErr != nil {
			t.Fatalf("handleMatchStatus error: %v", msgErr)
		}

		resp := user.conn.getSend()
		if resp == nil {
			t.Fatalf("no matches sent")
		}

		statuses := []msgjson.MatchStatusResult{}
		err := resp.UnmarshalResult(&statuses)
		if err != nil {
			t.Fatalf("UnmarshalResult error: %v", err)
		}
		if len(statuses) != 1 {
			t.Fatalf("expected 1 match, got %d", len(statuses))
		}
		return &statuses[0]
	}

	// As taker in MakerSwapCast, we expect tx data.
	status := getStatus()
	if !bytes.Equal(status.MakerTxData, tTxData) {
		t.Fatalf("wrong maker tx data. exected %x, got %s", tTxData, status.MakerTxData)
	}

	// As maker, we don't expect any tx data.
	rig.storage.matchStatuses[0].IsTaker = false
	rig.storage.matchStatuses[0].IsMaker = true
	if len(getStatus().TakerTxData) != 0 {
		t.Fatalf("got tx data as maker in MakerSwapCast")
	}

	// As maker in TakerSwapCast, we do expect tx data.
	rig.storage.matchStatuses[0].Status = order.TakerSwapCast
	rig.storage.matchStatuses[0].TakerSwap = []byte{0x01}
	txData := getStatus().TakerTxData
	if !bytes.Equal(txData, tTxData) {
		t.Fatalf("wrong taker tx data. exected %x, got %s", tTxData, txData)
	}

	reqPayload[0].MatchID = []byte{}
	req, _ = msgjson.NewRequest(1, msgjson.MatchStatusRoute, reqPayload)
	msgErr := rig.mgr.handleMatchStatus(user.conn, req)
	if msgErr == nil {
		t.Fatalf("no error for bad match ID")
	}
}

func TestOrderStatus(t *testing.T) {
	user := tNewUser(t)
	rig.signer.setSig(user.randomSignature())
	connectUser(t, user)

	rig.storage.orderStatuses = []*db.OrderStatus{{}}

	reqPayload := []msgjson.OrderStatusRequest{
		{
			OrderID: encode.RandomBytes(order.OrderIDSize),
		},
	}

	req, _ := msgjson.NewRequest(1, msgjson.OrderStatusRoute, reqPayload)

	msgErr := rig.mgr.handleOrderStatus(user.conn, req)
	if msgErr != nil {
		t.Fatalf("handleOrderStatus error: %v", msgErr)
	}

	resp := user.conn.getSend()
	if resp == nil {
		t.Fatalf("no orders sent")
	}

	var statuses []*msgjson.OrderStatus
	err := resp.UnmarshalResult(&statuses)
	if err != nil {
		t.Fatalf("UnmarshalResult error: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 order, got %d", len(statuses))
	}

	reqPayload[0].OrderID = []byte{}
	req, _ = msgjson.NewRequest(1, msgjson.OrderStatusRoute, reqPayload)
	msgErr = rig.mgr.handleOrderStatus(user.conn, req)
	if msgErr == nil {
		t.Fatalf("no error for bad order ID")
	}
}

func Test_checkSigS256(t *testing.T) {
	sig := []byte{0x30, 0, 0x02, 0x01, 9, 0x2, 0x01, 10}
	ecdsa.ParseDERSignature(sig) // panic on line 132: sigStr[2] != 0x02 after trimming to sigStr[:(1+2)]

	sig = []byte{0x30, 1, 0x02, 0x01, 9, 0x2, 0x01, 10}
	ecdsa.ParseDERSignature(sig) // panic on line 139: rLen := int(sigStr[index]) with index=3 and len = 3
}

func TestReputationInputsNotify(t *testing.T) {
	authMgr, storage := newEventTestAuthManager(t)
	user := tNewUser(t)
	storage.acct = &account.Account{ID: user.acctID, PubKey: user.privKey.PubKey()}
	storage.setBondTier(1)
	storage.reputationMatches = []*db.MatchResult{{
		DBID: 1, MatchID: randomMatchID(), MatchOutcome: db.OutcomeSwapSuccess,
	}}
	authMgr.signer.(*TSigner).setSig(user.randomSignature())
	authMgr.connMtx.Lock()
	authMgr.users[user.acctID] = &clientInfo{
		acct: &account.Account{ID: user.acctID},
		conn: user.conn,
	}
	authMgr.connMtx.Unlock()

	storage.notifyRepInputs(user.acctID)
	var msg *msgjson.Message
	deadline := time.Now().Add(time.Second)
	for msg == nil && time.Now().Before(deadline) {
		msg = user.conn.getSend()
		if msg == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if msg == nil {
		t.Fatal("no score_changed notification")
	}
	if msg.Route != msgjson.ScoreChangeRoute {
		t.Fatalf("route = %s, want %s", msg.Route, msgjson.ScoreChangeRoute)
	}
	note := new(msgjson.ScoreChangedNotification)
	if err := msg.Unmarshal(note); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if note.Reputation.Score != 1 {
		t.Fatalf("score = %d, want 1", note.Reputation.Score)
	}
	if note.Reputation.BondExpiryThreshold == 0 {
		t.Fatal("missing bond expiry threshold")
	}
}
