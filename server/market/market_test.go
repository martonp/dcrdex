// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/order/test"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/book"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/matcher"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
	"decred.org/dcrdex/server/swap"
)

type TArchivist struct {
	mtx                  sync.Mutex
	poisonEpochOrder     order.Order
	bookedOrders         []*order.LimitOrder
	epochOrders          []epochOrderWrite
	orderAcceptedUpdates []*db.OrderAcceptedUpdate
	marketStartedUpdates []*db.MarketStartedUpdate
	advanceEpochUpdates  []*db.AdvanceEpochUpdate
	lifecycle            *db.MarketLifecycle
	lifecycleUpdates     []*db.MarketLifecycleUpdate
	lastErr              error
	poisonEpochProcessed bool
	epochProcessed       []*db.EpochProcessedUpdate
	epochInserted        chan struct{}
	suspendedCancels     []*db.SuspendedCancelUpdate
	ordersRevokedUpdates []*db.OrdersRevokedUpdate
	commitOrders         []db.CommitOrder
	commitOrdersErr      error
}

type tMesh struct {
	err        *msgjson.Error
	publishErr error

	calls int
	req   mesh.CommandRequest
	user  account.AccountID
	msg   *msgjson.Message

	entries []*mesh.Event
	events  map[string]mesh.EventApplier
}

type applyEventMesh struct {
	apply func(context.Context, *mesh.Event) error
}

func (m *applyEventMesh) ExecuteCommand(context.Context, mesh.CommandRequest) *msgjson.Error {
	return nil
}

func (m *applyEventMesh) ApplyEvent(ctx context.Context, event *mesh.Event) (any, error) {
	return nil, m.apply(ctx, event)
}

func (t *tMesh) ExecuteCommand(_ context.Context, req mesh.CommandRequest) *msgjson.Error {
	t.calls++
	t.req = req
	t.user = req.User
	t.msg = req.Msg
	return t.err
}

func (t *tMesh) ApplyEvent(ctx context.Context, event *mesh.Event) (any, error) {
	if t.publishErr != nil {
		return nil, t.publishErr
	}
	cpy := cloneTestMeshEvent(event)
	if cpy == nil || t.events == nil {
		return nil, nil
	}
	t.entries = append(t.entries, cpy)
	applier := t.events[cpy.Kind]
	if applier == nil {
		return nil, fmt.Errorf("unsupported test mesh event %q", cpy.Kind)
	}
	applyCtx := &mesh.EventApplyContext{Context: ctx}
	_, err := applier(applyCtx, cpy)
	return applyCtx.Result(), err
}

func newTMesh(mkt *Market, authMgr *TAuth) *tMesh {
	mktName := mkt.name
	bookRouter := NewBookRouter(map[string]BookSource{
		mktName: mkt,
	}, &tFeeSource{}, func(string, comms.MsgHandler) {})
	return &tMesh{
		events: Events(map[string]*Market{
			mktName: mkt,
		}, bookRouter, authMgr.SendIfLocal, nil),
	}
}

func cloneTestMeshEvent(event *mesh.Event) *mesh.Event {
	if event == nil {
		return nil
	}
	return &mesh.Event{
		Kind:    event.Kind,
		Payload: append([]byte(nil), event.Payload...),
	}
}

func mustMarshal(v any) []byte {
	payload, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal mesh event payload: %v", err))
	}
	return payload
}

type meshEventBuilder interface {
	event() (*mesh.Event, error)
}

// encoderBuilder adapts a canonical meshevents event to the test rig's
// meshEventBuilder.
type encoderBuilder struct{ e mesh.EventEncoder }

func (b encoderBuilder) event() (*mesh.Event, error) { return mesh.NewEvent(b.e) }

func newOrderAcceptedEvent(ord order.Order) meshEventBuilder {
	return encoderBuilder{meshevents.NewOrderAcceptedEvent(ord)}
}

// advanceEpochEvent and epochProcessedPayload alias the canonical event types
// so tests can build and mutate wire payloads with local names.
type advanceEpochEvent = meshevents.AdvanceEpochEvent

type epochProcessedPayload = meshevents.EpochProcessedEvent

func newMarketStartedEvent(marketName string, currentEpochIdx, epochDur int64, runParams meshevents.MarketRunParams,
	revocationTime time.Time, bookedRevokes []*db.StartupOrderRevoke) meshEventBuilder {

	return encoderBuilder{meshevents.NewMarketStartedEvent(marketName, currentEpochIdx, epochDur, runParams,
		revocationTime, encodeStartupOrderRevokes(bookedRevokes))}
}

func newEpochProcessedMeshEvent(marketName string, epochIdx, epochDur int64, matchTime time.Time, cSum []byte,
	ordersRevealed []*matcher.OrderRevealed, misses []order.Order, missRevokeTime time.Time) *mesh.Event {

	event, err := mesh.NewEvent(meshevents.NewEpochProcessedEvent(marketName, epochIdx, epochDur,
		matchTime, 10, 10, 0, cSum, ordersRevealed, misses, missRevokeTime))
	if err != nil {
		panic(err)
	}
	return event
}

type marketEventRig struct {
	mkt        *Market
	storage    *TArchivist
	auth       *TAuth
	bookRouter *BookRouter
	events     map[string]mesh.EventApplier
	cleanup    func()
}

func newMarketEventRig(t *testing.T, opts ...any) *marketEventRig {
	t.Helper()
	mkt, storage, auth, cleanup, err := newTestMarket(opts...)
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	mktName := mkt.name
	bookRouter := NewBookRouter(map[string]BookSource{
		mktName: mkt,
	}, &tFeeSource{}, func(string, comms.MsgHandler) {})
	return &marketEventRig{
		mkt:        mkt,
		storage:    storage,
		auth:       auth,
		bookRouter: bookRouter,
		events: Events(map[string]*Market{
			mktName: mkt,
		}, bookRouter, auth.SendIfLocal, nil),
		cleanup: cleanup,
	}
}

type epochProcessedTestSwapper struct {
	trackErr error
	tracked  []*order.MatchSet
	acked    []*order.MatchSet
}

func (s *epochProcessedTestSwapper) TrackMatches(matchSets []*order.MatchSet) error {
	s.tracked = append(s.tracked, matchSets...)
	return s.trackErr
}

func (s *epochProcessedTestSwapper) RequestMatchAcks(matchSets []*order.MatchSet) {
	s.acked = append(s.acked, matchSets...)
}

func (s *epochProcessedTestSwapper) CheckUnspent(context.Context, uint32, []byte) error {
	return nil
}

func (s *epochProcessedTestSwapper) ChainsSynced(uint32, uint32) (bool, error) {
	return true, nil
}

type countingChainsSyncedSwapper struct {
	epochProcessedTestSwapper
	synced bool
	err    error
	calls  atomic.Int32
}

func (s *countingChainsSyncedSwapper) ChainsSynced(uint32, uint32) (bool, error) {
	s.calls.Add(1)
	return s.synced, s.err
}

func (rig *marketEventRig) apply(t *testing.T, event meshEventBuilder) *mesh.Event {
	t.Helper()
	entry, err := rig.applyResult(t, event)
	if err != nil {
		t.Fatalf("ApplyEvent(%q) error: %v", entry.Kind, err)
	}
	return entry
}

func (rig *marketEventRig) applyErr(t *testing.T, event meshEventBuilder) error {
	t.Helper()
	_, err := rig.applyResult(t, event)
	return err
}

func (rig *marketEventRig) applyResult(t *testing.T, event meshEventBuilder) (*mesh.Event, error) {
	t.Helper()
	entry, err := event.event()
	if err != nil {
		t.Fatalf("event error: %v", err)
	}
	applier := rig.events[entry.Kind]
	if applier == nil {
		t.Fatalf("missing event applier for %q", entry.Kind)
	}
	_, err = applier(&mesh.EventApplyContext{Context: context.Background()}, entry)
	return entry, err
}

func (rig *marketEventRig) submitMarketStarted(t *testing.T, currentEpochIdx int64) {
	t.Helper()
	rig.apply(t, newMarketStartedEvent(rig.mkt.name, currentEpochIdx, int64(rig.mkt.EpochDuration()),
		rig.mkt.configuredParams.MarketRunParams, time.UnixMilli(1).UTC(), nil))
}

func (rig *marketEventRig) subscribeBook(t *testing.T) *TLink {
	t.Helper()
	link, sub := newSubscriber(mkt3)
	if err := rig.bookRouter.handleOrderBook(link, sub); err != nil {
		t.Fatalf("handleOrderBook: %v", err)
	}
	_ = link.getSend() // initial order book response
	return link
}

type submitOrderRPCError struct {
	msgErr *msgjson.Error
}

func (e submitOrderRPCError) Error() string {
	return e.msgErr.Message
}

func (e submitOrderRPCError) Is(target error) bool {
	if target == nil {
		return false
	}
	if target == ErrInternalServer {
		return e.msgErr.Code == msgjson.RPCInternalError || e.msgErr.Code == msgjson.RPCInternal ||
			strings.Contains(e.msgErr.Message, errEpochOrderStorage.Error())
	}
	return strings.Contains(e.msgErr.Message, target.Error())
}

func submitOrderCommand(t *testing.T, mkt *Market, auth *TAuth, rec *orderRecord) error {
	t.Helper()

	var kind string
	switch rec.order.Type() {
	case order.LimitOrderType:
		kind = commandKindLimit
	case order.MarketOrderType:
		kind = commandKindMarket
	case order.CancelOrderType:
		kind = commandKindCancel
	default:
		t.Fatalf("unknown order type %v", rec.order.Type())
	}

	tm, ok := mkt.mesh.(*tMesh)
	if !ok {
		t.Fatalf("test market mesh has type %T, not *tMesh", mkt.mesh)
	}
	svc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Commands: map[string]mesh.CommandExecutor{
			kind: func(cmd *mesh.CommandContext) *msgjson.Error {
				return mkt.AcceptOrderCommand(cmd.Context, rec, cmd.Completion)
			},
		},
		Events: tm.events,
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}
	msg, err := msgjson.NewRequest(rec.msgID, kind, nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	msgErr := svc.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: kind,
		User: rec.order.User(),
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			return auth.Send(rec.order.User(), resp)
		},
	})
	if msgErr != nil {
		return submitOrderRPCError{msgErr: msgErr}
	}
	return nil
}

type epochOrderWrite struct {
	ord      order.Order
	epochIdx int64
	epochDur int64
	epochGap int32
}

type acceptedLimitSnapshot struct {
	epochOrderID     order.OrderID
	hasEpochOrder    bool
	commitOrderID    order.OrderID
	hasCommitment    bool
	coinLocked       bool
	haveBookOrder    bool
	epochOrderWrites []epochOrderSnapshot
}

type epochOrderSnapshot struct {
	orderID  order.OrderID
	epochIdx int64
	epochDur int64
	epochGap int32
}

func (ta *TArchivist) Close() error { return nil }
func (ta *TArchivist) LastErr() error {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	return ta.lastErr
}
func (ta *TArchivist) setLastErr(err error) {
	ta.mtx.Lock()
	ta.lastErr = err
	ta.mtx.Unlock()
}
func (ta *TArchivist) Fatal() <-chan struct{} { return nil }
func (ta *TArchivist) Order(oid order.OrderID, base, quote uint32) (order.Order, order.OrderStatus, error) {
	return nil, order.OrderStatusUnknown, errors.New("boom")
}
func (ta *TArchivist) SwapDataFullByID(order.MatchID) (*db.SwapDataFull, error) {
	return nil, nil
}
func (ta *TArchivist) OrdersWithCommit(_ context.Context, _, _ uint32, commit order.Commitment, _ time.Time) ([]db.CommitOrder, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	if ta.commitOrdersErr != nil {
		return nil, ta.commitOrdersErr
	}
	var out []db.CommitOrder
	for _, stored := range ta.commitOrders {
		if stored.Order.Commitment() == commit {
			out = append(out, stored)
		}
	}
	return out, nil
}
func (ta *TArchivist) BookOrders(base, quote uint32) ([]*order.LimitOrder, error) {
	ta.mtx.Lock()
	bookedOrders := ta.bookedOrders
	ta.mtx.Unlock()
	return bookedOrders, nil
}
func (ta *TArchivist) EpochOrders(base, quote uint32) ([]order.Order, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ords := make([]order.Order, 0, len(ta.epochOrders))
	for _, write := range ta.epochOrders {
		ords = append(ords, write.ord)
	}
	return ords, nil
}
func (ta *TArchivist) MarketMatches(base, quote uint32) ([]*db.MatchDataWithCoins, error) {
	return nil, nil
}
func (ta *TArchivist) UserOrderStatuses(aid account.AccountID, base, quote uint32, oids []order.OrderID) ([]*db.OrderStatus, error) {
	return nil, errors.New("boom")
}
func (ta *TArchivist) ActiveUserOrderStatuses(aid account.AccountID) ([]*db.OrderStatus, error) {
	return nil, errors.New("boom")
}
func (ta *TArchivist) OrderStatus(order.Order) (order.OrderStatus, order.OrderType, int64, error) {
	return order.OrderStatusUnknown, order.UnknownOrderType, -1, errors.New("boom")
}
func (ta *TArchivist) EventLogFrontier(context.Context) (*db.EventLogPosition, error) {
	return new(db.EventLogPosition), nil
}

func (ta *TArchivist) EventLogEntriesAfter(context.Context, uint64, int) ([]*db.EventLogEntry, error) {
	return nil, nil
}

func (ta *TArchivist) ApplyOrderAcceptedEvent(_ context.Context, _ *db.EventLogMeta, update *db.OrderAcceptedUpdate) (*db.EventLogEntry, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	if ta.poisonEpochOrder != nil && update.Order.ID() == ta.poisonEpochOrder.ID() {
		return nil, errors.New("barf")
	}
	ta.orderAcceptedUpdates = append(ta.orderAcceptedUpdates, update)
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplyMarketStartedEvent(_ context.Context, _ *db.EventLogMeta, update *db.MarketStartedUpdate) (*db.EventLogEntry, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.marketStartedUpdates = append(ta.marketStartedUpdates, update)
	// Project lifecycle so MarketLifecycle() returns the post-start row the
	// memory applier reloads. Epoch order status is not projected here: tests
	// seed epochOrders for EpochOrders() reads and assert the recorded update.
	next, changed, err := db.ProjectMarketStartedLifecycle(ta.lifecycle, update)
	if err != nil {
		return nil, err
	}
	if changed {
		ta.lifecycle = next
	}
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) MarketLifecycle(string) (*db.MarketLifecycle, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	if ta.lifecycle == nil {
		return nil, nil
	}
	cpy := *ta.lifecycle
	if ta.lifecycle.PersistBook != nil {
		persist := *ta.lifecycle.PersistBook
		cpy.PersistBook = &persist
	}
	return &cpy, nil
}
func (ta *TArchivist) ApplyMarketLifecycleEvent(_ context.Context, _ *db.EventLogMeta, update *db.MarketLifecycleUpdate) (*db.MarketLifecycleApplyResult, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.lifecycleUpdates = append(ta.lifecycleUpdates, update)
	next, err := db.ProjectMarketLifecycle(ta.lifecycle, update)
	if err != nil {
		return nil, err
	}
	ta.lifecycle = next
	// The pg applier derives the purge set from the order tables and echoes
	// the revokes it applied; this mock has no order tables, so it echoes the
	// full requested revoke set and derives no purges.
	return &db.MarketLifecycleApplyResult{
		Log:           new(db.EventLogEntry),
		Lifecycle:     next,
		ResumeRevokes: update.ResumeRevokes,
	}, nil
}
func (ta *TArchivist) ApplyAdvanceEpochEvent(_ context.Context, _ *db.EventLogMeta, update *db.AdvanceEpochUpdate) (*db.EventLogEntry, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.advanceEpochUpdates = append(ta.advanceEpochUpdates, update)
	// Mirror the real applier's lifecycle projection (including the pending
	// suspend final-close transition to MarketPendingSuspendDrain), but only
	// when a test has seeded a lifecycle row.
	if ta.lifecycle != nil {
		next, changed, err := db.ProjectAdvanceEpochLifecycle(ta.lifecycle, update)
		if err != nil {
			return nil, err
		}
		if changed {
			ta.lifecycle = next
		}
	}
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplyOrdersRevokedEvent(_ context.Context, _ *db.EventLogMeta, _ *db.ReputationOutcomePolicy, update *db.OrdersRevokedUpdate) (*db.EventLogEntry, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.ordersRevokedUpdates = append(ta.ordersRevokedUpdates, update)
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) failOnEpochOrder(ord order.Order) {
	ta.mtx.Lock()
	ta.poisonEpochOrder = ord
	ta.mtx.Unlock()
}
func (ta *TArchivist) ApplyEpochProcessedEvent(_ context.Context, _ *db.EventLogMeta, _ *db.ReputationOutcomePolicy, update *db.EpochProcessedUpdate) (*db.EventLogEntry, error) {
	ta.mtx.Lock()
	if ta.poisonEpochProcessed {
		ta.mtx.Unlock()
		return nil, errors.New("epoch processed storage failure")
	}
	// Advance the mock's closure cursor the way the real applier does, so a
	// later advance_epoch or suspend sees the same row Postgres would. Skip
	// when there is no row: applier tests use arbitrary epoch indexes.
	if ta.lifecycle != nil {
		next, err := db.ProjectEpochProcessedLifecycle(ta.lifecycle, ta.lifecycle.Market, update.Epoch.Idx, update.Epoch.Dur)
		if err != nil {
			ta.mtx.Unlock()
			return nil, err
		}
		ta.lifecycle = next
	}
	ta.epochProcessed = append(ta.epochProcessed, update)
	epochInserted := ta.epochInserted
	ta.mtx.Unlock()
	if epochInserted != nil {
		epochInserted <- struct{}{}
	}
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplySuspendedCancelEvent(_ context.Context, _ *db.EventLogMeta, update *db.SuspendedCancelUpdate) (*db.SuspendedCancelApplyResult, error) {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.suspendedCancels = append(ta.suspendedCancels, update)
	var target *order.LimitOrder
	for _, lo := range ta.bookedOrders {
		if lo.ID() == update.TargetOrderID {
			target = lo
			break
		}
	}
	if target == nil && update.Match != nil {
		target = update.Match.Maker
	}
	return &db.SuspendedCancelApplyResult{
		Log:         new(db.EventLogEntry),
		Cancel:      update.Cancel,
		TargetOrder: target,
		Match:       update.Match,
	}, nil
}
func (ta *TArchivist) LastEpochRate(base, quote uint32) (rate uint64, err error) {
	return 1, nil
}
func (ta *TArchivist) BookOrder(lo *order.LimitOrder) error {
	ta.mtx.Lock()
	defer ta.mtx.Unlock()
	ta.bookedOrders = append(ta.bookedOrders, lo)
	return nil
}

// SwapArchiver for Swapper
func (ta *TArchivist) ActiveSwaps() ([]*db.SwapDataFull, error) { return nil, nil }
func (ta *TArchivist) CompletedAndAtFaultMatchStats(aid account.AccountID, lastN int) ([]*db.MatchOutcome, error) {
	return nil, nil
}
func (ta *TArchivist) AllActiveUserMatches(account.AccountID) ([]*db.MatchData, error) {
	return nil, nil
}
func (ta *TArchivist) MatchStatuses(aid account.AccountID, base, quote uint32, matchIDs []order.MatchID) ([]*db.MatchStatus, error) {
	return nil, nil
}
func (ta *TArchivist) ApplyMatchAcksRecordedEvent(context.Context, *db.EventLogMeta, *db.MatchAcksRecordedUpdate) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplySwapContractRecordedEvent(context.Context, *db.EventLogMeta, *db.SwapContract) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplyAuditAckRecordedEvent(context.Context, *db.EventLogMeta, *db.AuditAck) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplySwapRedemptionRecordedEvent(context.Context, *db.EventLogMeta, *db.ReputationOutcomePolicy, *db.SwapRedemption) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplyRedemptionAckRecordedEvent(context.Context, *db.EventLogMeta, *db.RedemptionAck) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) ApplyMatchFailedEvent(context.Context, *db.EventLogMeta, *db.ReputationOutcomePolicy, *db.MatchFailedUpdate) (*db.EventLogEntry, error) {
	return new(db.EventLogEntry), nil
}
func (ta *TArchivist) LoadEpochStats(uint32, uint32, []*candles.Cache) error { return nil }

type TCollector struct{}

var collectorSpot = &msgjson.Spot{
	Stamp: rand.Uint64(),
}

func (tc *TCollector) ReportEpoch(base, quote uint32, epochIdx uint64, stats *matcher.MatchCycleStats) (*msgjson.Spot, error) {
	return collectorSpot, nil
}

type tFeeFetcher struct {
	maxFeeRate uint64
}

func (*tFeeFetcher) FeeRate(context.Context) uint64 {
	return 10
}

func (f *tFeeFetcher) MaxFeeRate() uint64 {
	return f.maxFeeRate
}

func (f *tFeeFetcher) LastRate() uint64 {
	return 10
}

func (f *tFeeFetcher) SwapFeeRate(context.Context) uint64 {
	return 10
}

type tBalancer struct {
	reqs map[string]int
}

func newTBalancer() *tBalancer {
	return &tBalancer{make(map[string]int)}
}

func (b *tBalancer) CheckBalance(acctAddr string, assetID, redeemAssetID uint32, qty, lots uint64, redeems int) bool {
	b.reqs[acctAddr]++
	return true
}

func (b *tBalancer) CheckReserved(acctAddr string, assetID uint32) bool {
	return b.CheckBalance(acctAddr, assetID, assetID, 0, 0, 0)
}

func randomOrderID() order.OrderID {
	pk := randomBytes(order.OrderIDSize)
	var id order.OrderID
	copy(id[:], pk)
	return id
}

func snapshotAcceptedLimit(mkt *Market, storage *TArchivist, oid order.OrderID, commit order.Commitment, coin order.CoinID) acceptedLimitSnapshot {
	mkt.epochMtx.RLock()
	epochOrd, hasEpochOrder := mkt.epochOrders[oid]
	commitOrderID, hasCommitment := mkt.epochCommitments[commit]
	mkt.epochMtx.RUnlock()

	mkt.bookMtx.Lock()
	haveBookOrder := mkt.book.HaveOrder(oid)
	mkt.bookMtx.Unlock()

	writes := make([]epochOrderSnapshot, 0, len(storage.epochOrders))
	for _, write := range storage.epochOrders {
		var writeOID order.OrderID
		if write.ord != nil {
			writeOID = write.ord.ID()
		}
		writes = append(writes, epochOrderSnapshot{
			orderID:  writeOID,
			epochIdx: write.epochIdx,
			epochDur: write.epochDur,
			epochGap: write.epochGap,
		})
	}

	var epochOrderID order.OrderID
	if epochOrd != nil {
		epochOrderID = epochOrd.ID()
	}
	return acceptedLimitSnapshot{
		epochOrderID:     epochOrderID,
		hasEpochOrder:    hasEpochOrder,
		commitOrderID:    commitOrderID,
		hasCommitment:    hasCommitment,
		coinLocked:       mkt.CoinLocked(mkt.Base(), coin),
		haveBookOrder:    haveBookOrder,
		epochOrderWrites: writes,
	}
}

func assertAcceptedLimitSnapshotsEqual(t *testing.T, got, want acceptedLimitSnapshot) {
	t.Helper()
	if got.epochOrderID != want.epochOrderID || got.hasEpochOrder != want.hasEpochOrder {
		t.Fatalf("epoch order mismatch. got %v/%t, want %v/%t",
			got.epochOrderID, got.hasEpochOrder, want.epochOrderID, want.hasEpochOrder)
	}
	if got.commitOrderID != want.commitOrderID || got.hasCommitment != want.hasCommitment {
		t.Fatalf("commitment mismatch. got %v/%t, want %v/%t",
			got.commitOrderID, got.hasCommitment, want.commitOrderID, want.hasCommitment)
	}
	if got.coinLocked != want.coinLocked {
		t.Fatalf("coin lock mismatch. got %t, want %t", got.coinLocked, want.coinLocked)
	}
	if got.haveBookOrder != want.haveBookOrder {
		t.Fatalf("book order mismatch. got %t, want %t", got.haveBookOrder, want.haveBookOrder)
	}
	if len(got.epochOrderWrites) != len(want.epochOrderWrites) {
		t.Fatalf("epoch storage write count = %d, want %d", len(got.epochOrderWrites), len(want.epochOrderWrites))
	}
	for i := range got.epochOrderWrites {
		if got.epochOrderWrites[i] != want.epochOrderWrites[i] {
			t.Fatalf("epoch storage write %d mismatch. got %+v, want %+v", i, got.epochOrderWrites[i], want.epochOrderWrites[i])
		}
	}
}

const (
	tUserTier, tUserScore, tMaxScore = int64(1), int32(30), int32(60)
)

var parcelLimit = float64(calcParcelLimit(tUserTier, tUserScore, tMaxScore))

// tMasterLockers is the swap-side lockers behind the market's book lockers.
type tMasterLockers struct {
	base, quote *coinlock.MasterCoinLocker
}

func newTestMarket(opts ...any) (*Market, *TArchivist, *TAuth, func(), error) {
	// The DEX will make MasterCoinLockers for each asset.
	masterLockerBase := coinlock.NewMasterCoinLocker()
	bookLockerBase := masterLockerBase.Book()
	swapLockerBase := masterLockerBase.Swap()

	masterLockerQuote := coinlock.NewMasterCoinLocker()
	bookLockerQuote := masterLockerQuote.Book()
	swapLockerQuote := masterLockerQuote.Swap()

	epochDurationMSec := uint64(500) // 0.5 sec epoch duration
	storage := &TArchivist{}
	var balancer Balancer

	baseAsset, quoteAsset := assetDCR, assetBTC

	for _, opt := range opts {
		switch optT := opt.(type) {
		case *TArchivist:
			storage = optT
		case *tMasterLockers:
			optT.base, optT.quote = masterLockerBase, masterLockerQuote
		case [2]*asset.BackedAsset:
			baseAsset, quoteAsset = optT[0], optT[1]
			if baseAsset.ID == assetETH.ID || baseAsset.ID == assetMATIC.ID {
				bookLockerBase = nil
			}
			if quoteAsset.ID == assetETH.ID || quoteAsset.ID == assetMATIC.ID {
				bookLockerQuote = nil
			}
		case *tBalancer:
			balancer = optT
		}

	}

	authMgr := &TAuth{
		sends:            make([]*msgjson.Message, 0),
		preimagesByMsgID: make(map[uint64]order.Preimage),
		preimagesByOrdID: make(map[string]order.Preimage),
	}

	var (
		mkt      *Market
		swapDone func(ord order.Order, match *order.Match, faulted bool)
	)
	swapperCfg := &swap.Config{
		Assets: map[uint32]*swap.SwapperAsset{
			assetDCR.ID:   {BackedAsset: assetDCR, Locker: swapLockerBase},
			assetBTC.ID:   {BackedAsset: assetBTC, Locker: swapLockerQuote},
			assetETH.ID:   {BackedAsset: assetETH},
			assetMATIC.ID: {BackedAsset: assetMATIC},
		},
		Storage:          storage,
		AuthManager:      authMgr,
		BroadcastTimeout: 10 * time.Second,
		TxWaitExpiration: 5 * time.Second,
		LockTimeTaker:    dex.LockTimeTaker(dex.Testnet),
		LockTimeMaker:    dex.LockTimeMaker(dex.Testnet),
		SwapDone: func(ord order.Order, match *order.Match, faulted bool) {
			swapDone(ord, match, faulted)
		},
	}
	swapper, err := swap.NewSwapper(swapperCfg)
	if err != nil {
		panic(err.Error())
	}
	if err := swapper.RestoreActiveSwaps(false); err != nil {
		panic(err.Error())
	}

	mbBuffer := 1.1
	mktInfo, err := dex.NewMarketInfo(baseAsset.ID, quoteAsset.ID,
		dcrLotSize, btcRateStep, epochDurationMSec, mbBuffer)
	if err != nil {
		return nil, nil, nil, func() {}, fmt.Errorf("dex.NewMarketInfo() failure: %w", err)
	}

	mkt, err = NewMarket(&Config{
		MarketInfo:      mktInfo,
		Storage:         storage,
		Swapper:         swapper,
		AuthManager:     authMgr,
		FeeFetcherBase:  &tFeeFetcher{baseAsset.MaxFeeRate},
		CoinLockerBase:  bookLockerBase,
		FeeFetcherQuote: &tFeeFetcher{quoteAsset.MaxFeeRate},
		CoinLockerQuote: bookLockerQuote,
		DataCollector:   new(TCollector),
		Balancer:        balancer,
		CheckParcelLimit: func(_ account.AccountID, _ time.Time, f MarketParcelCalculator) (bool, error) {
			parcels := f(0)
			return parcels <= parcelLimit, nil
		},
	})
	if err != nil {
		return nil, nil, nil, func() {}, fmt.Errorf("Failed to create test market: %w", err)
	}
	if err := mkt.LoadState(); err != nil {
		return nil, nil, nil, func() {}, fmt.Errorf("Failed to load test market state: %w", err)
	}
	mkt.SetMeshService(newTMesh(mkt, authMgr))

	swapDone = mkt.SwapDone

	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		Commands:       swapper.Commands(),
		Events:         swapper.Events(),
		EventLogReader: storage,
		OnHalt:         func(error) {},
		MasterWorkers: []mesh.MasterWorker{{
			Name: "Swapper",
			Run:  swapper.Run,
		}},
		Logger: dex.Disabled,
	})
	if err != nil {
		return nil, nil, nil, func() {}, fmt.Errorf("mesh.NewService: %w", err)
	}
	swapper.SetMeshService(meshSvc)
	ssw := dex.NewStartStopWaiter(meshSvc)
	ssw.Start(testCtx)
	if err := meshSvc.WaitUntilReadyForComms(testCtx); err != nil {
		return nil, nil, nil, func() {}, err
	}
	cleanup := func() {
		ssw.Stop()
		ssw.WaitForShutdown()
	}

	return mkt, storage, authMgr, cleanup, nil
}

func newStartupLockTestMarket(storage *TArchivist, baseLocker, quoteLocker coinlock.CoinLocker) (*Market, error) {
	mktInfo, err := dex.NewMarketInfo(assetDCR.ID, assetBTC.ID, dcrLotSize, btcRateStep, 500, 1.1)
	if err != nil {
		return nil, err
	}
	mkt, err := NewMarket(&Config{
		MarketInfo:      mktInfo,
		Storage:         storage,
		Swapper:         &epochProcessedTestSwapper{},
		CoinLockerBase:  baseLocker,
		CoinLockerQuote: quoteLocker,
		DataCollector:   new(TCollector),
	})
	if err != nil {
		return nil, err
	}
	if err := mkt.LoadState(); err != nil {
		return nil, err
	}
	return mkt, nil
}

func TestMarket_NewMarket_BookOrders(t *testing.T) {
	rnd.Seed(12)
	randCoinDCR := func() []byte {
		coinID := make([]byte, 36)
		rnd.Read(coinID[:])
		return coinID
	}

	loBuy := makeLO(buyer3, mkRate3(0.8, 1.0), randLots(10), order.StandingTiF)
	loBuy.FillAmt = dcrLotSize // partial fill to cover the utxo check alternate path
	loBuy.Coins = []order.CoinID{randCoinDCR()}
	loSell := makeLO(seller3, mkRate3(1.0, 1.2), randLots(10)+1, order.StandingTiF)
	fundingCoinDCR := randCoinDCR()
	loSell.Coins = []order.CoinID{fundingCoinDCR}
	// let VerifyUnspentCoin find the unfilled sell's coin as unspent
	oRig.dcr.addUTXO(&msgjson.Coin{ID: fundingCoinDCR}, 1234)

	cases := []struct {
		name   string
		booked []*order.LimitOrder
	}{
		{name: "empty book"},
		{name: "booked orders load, unfilled coins lock", booked: []*order.LimitOrder{loBuy, loSell}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storage := &TArchivist{}
			for _, lo := range tc.booked {
				_ = storage.BookOrder(lo) // the stub does not error
			}
			mkt, _, _, cleanup, err := newTestMarket(storage)
			if err != nil {
				t.Fatalf("newTestMarket failure: %v", err)
			}
			defer cleanup()

			wantBook := make(map[order.OrderID]bool, len(tc.booked))
			var wantBuys, wantSells int
			for _, lo := range tc.booked {
				wantBook[lo.ID()] = true
				if lo.Sell {
					wantSells++
				} else {
					wantBuys++
				}
			}
			_, buys, sells := mkt.Book()
			if len(buys) != wantBuys || len(sells) != wantSells {
				t.Fatalf("market booked %d buys and %d sells, want %d/%d",
					len(buys), len(sells), wantBuys, wantSells)
			}
			for _, lo := range append(buys, sells...) {
				if !wantBook[lo.ID()] {
					t.Errorf("unexpected booked order %v", lo.ID())
				}
			}
			// Filled booked orders stay locked too.
			for _, lo := range tc.booked {
				assetID := mkt.Quote()
				if lo.Sell {
					assetID = mkt.Base()
				}
				for _, coin := range lo.Coins {
					if !mkt.CoinLocked(assetID, []byte(coin)) {
						t.Errorf("booked order %v coin %x not locked", lo.ID(), coin)
					}
				}
			}
		})
	}
}

func TestMarket_SwapLockedCoinsRejectOrders(t *testing.T) {
	lockers := &tMasterLockers{}
	rig := newMarketEventRig(t, lockers)
	defer rig.cleanup()
	mkt := rig.mkt
	epochDur := int64(mkt.EpochDuration())
	epochIdx := int64(1234)
	rig.submitMarketStarted(t, epochIdx)

	coin := order.CoinID([]byte{0x0a, 0x0b, 0x0c})
	inSwap := makeLO(seller3, mkRate3(1.0, 1.2), 2, order.StandingTiF)
	inSwap.Coins = []order.CoinID{coin}
	if failed := lockers.base.Swap().LockOrdersCoins([]order.Order{inSwap}); len(failed) > 0 {
		t.Fatalf("test setup failed to lock the in-swap coin")
	}

	lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	lo.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
	lo.Coins = []order.CoinID{coin}
	err := rig.applyErr(t, newOrderAcceptedEvent(lo))
	if err == nil || !strings.Contains(err.Error(), "already-locked") {
		t.Fatalf("apply error = %v, want already-locked coin error", err)
	}
}

func TestMarket_NewMarket_DuplicateBookedCoinLockRollbackAcrossAssets(t *testing.T) {
	baseCoin := order.CoinID([]byte{0x10, 0x20, 0x30})
	sharedQuoteCoin := order.CoinID([]byte{0x40, 0x50, 0x60})

	var sell, buyA, buyB *order.LimitOrder
	var conflict *order.LimitOrder
	for i := 0; i < 1000; i++ {
		sell = makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		sell.Coins = []order.CoinID{baseCoin}
		buyA = makeLO(buyer3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		buyA.Coins = []order.CoinID{sharedQuoteCoin}
		buyB = makeLO(buyer3, mkRate3(1.1, 1.3), 1, order.StandingTiF)
		buyB.Coins = []order.CoinID{sharedQuoteCoin}

		maxOrder := sell
		for _, lo := range []*order.LimitOrder{buyA, buyB} {
			maxID, oid := maxOrder.ID(), lo.ID()
			if bytes.Compare(maxID[:], oid[:]) < 0 {
				maxOrder = lo
			}
		}
		if maxOrder.ID() == buyA.ID() || maxOrder.ID() == buyB.ID() {
			conflict = maxOrder
			break
		}
	}
	if conflict == nil {
		t.Fatalf("failed to generate deterministic cross-asset rollback order IDs")
	}

	storage := &TArchivist{}
	for _, lo := range []*order.LimitOrder{sell, buyA, buyB} {
		if err := storage.BookOrder(lo); err != nil {
			t.Fatalf("BookOrder error: %v", err)
		}
	}

	baseLocker := coinlock.NewAssetCoinLocker()
	quoteLocker := coinlock.NewAssetCoinLocker()
	_, err := newStartupLockTestMarket(storage, baseLocker, quoteLocker)
	if err == nil {
		t.Fatalf("NewMarket succeeded with duplicate quote booked coins")
	}
	if !strings.Contains(err.Error(), conflict.ID().String()) {
		t.Fatalf("NewMarket error = %v, want conflicting order %v", err, conflict.ID())
	}
	if baseLocker.CoinLocked(baseCoin) {
		t.Fatalf("base coin remains locked after quote-side constructor failure")
	}
	if quoteLocker.CoinLocked(sharedQuoteCoin) {
		t.Fatalf("quote coin remains locked after quote-side constructor failure")
	}
	if len(storage.marketStartedUpdates) != 0 {
		t.Fatalf("market started update count = %d, want 0", len(storage.marketStartedUpdates))
	}
}

func TestMarket_Book(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	rnd.Seed(0)

	for i := 0; i < 8; i++ {
		lo := makeLO(buyer3, mkRate3(0.8, 1.0), randLots(10), order.StandingTiF)
		if !mkt.book.Insert(lo) {
			t.Fatalf("Failed to Insert order into book.")
		}
		lo = makeLO(seller3, mkRate3(1.0, 1.2), randLots(10), order.StandingTiF)
		if !mkt.book.Insert(lo) {
			t.Fatalf("Failed to Insert order into book.")
		}
	}

	bestBuy, bestSell := mkt.book.Best()

	marketRate := mkt.MidGap()
	mktRateWant := (bestBuy.Rate + bestSell.Rate) / 2
	if marketRate != mktRateWant {
		t.Errorf("Market rate expected %d, got %d", mktRateWant, marketRate)
	}

	_, buys, sells := mkt.Book()
	if buys[0] != bestBuy {
		t.Errorf("Incorrect best buy order. Got %v, expected %v",
			buys[0], bestBuy)
	}
	if sells[0] != bestSell {
		t.Errorf("Incorrect best sell order. Got %v, expected %v",
			sells[0], bestSell)
	}
}

func TestSwapDone(t *testing.T) {
	matchQty := uint64(dcrLotSize)
	concurrentSettling := matchQty / 2

	newOrderAndMatch := func(force order.TimeInForce) (*order.LimitOrder, *order.Match) {
		ord := makeLO(seller3, mkRate3(1.0, 1.2), 2, force)
		taker := makeLO(buyer3, mkRate3(1.0, 1.2), 2, order.ImmediateTiF)
		match := &order.Match{
			Taker:    taker,
			Maker:    ord,
			Quantity: matchQty,
			Rate:     ord.Rate,
		}
		return ord, match
	}

	tests := []struct {
		name              string
		force             order.TimeInForce
		faulted           bool
		initial           uint64
		hasSettling       bool
		booked            bool
		extraBeforeApply  uint64
		wantSettling      uint64
		wantSettlingFound bool
	}{
		{
			name:              "remaining settling is not complete",
			force:             order.ImmediateTiF,
			initial:           matchQty * 2,
			hasSettling:       true,
			extraBeforeApply:  concurrentSettling,
			wantSettling:      matchQty + concurrentSettling,
			wantSettlingFound: true,
		},
		{
			name:        "nothing settling or booked clears settling",
			force:       order.ImmediateTiF,
			initial:     matchQty,
			hasSettling: true,
		},
		{
			name:              "applies current settling",
			force:             order.ImmediateTiF,
			initial:           matchQty,
			hasSettling:       true,
			extraBeforeApply:  concurrentSettling,
			wantSettling:      concurrentSettling,
			wantSettlingFound: true,
		},
		{
			name:              "booked standing order is not complete",
			force:             order.StandingTiF,
			initial:           matchQty,
			hasSettling:       true,
			booked:            true,
			wantSettlingFound: true,
		},
		{
			name:        "missing settling entry is no-op",
			force:       order.ImmediateTiF,
			hasSettling: false,
		},
		{
			name:        "insufficient settling quantity clears settling",
			force:       order.ImmediateTiF,
			initial:     matchQty - 1,
			hasSettling: true,
		},
		{
			name:        "faulted booked order is revoked",
			force:       order.StandingTiF,
			faulted:     true,
			booked:      true,
			hasSettling: true,
			initial:     matchQty,
		},
		{
			name:    "faulted missing settling entry is no-op",
			force:   order.ImmediateTiF,
			faulted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ord, match := newOrderAndMatch(tt.force)
			mkt := &Market{
				settling: make(map[order.OrderID]uint64),
				auth:     &TAuth{},
			}
			if tt.hasSettling {
				mkt.settling[ord.ID()] = tt.initial
			}
			if tt.booked {
				mkt.book = book.New(dcrLotSize, 0)
				if !mkt.book.Insert(ord) {
					t.Fatalf("book order")
				}
			}

			if tt.extraBeforeApply > 0 {
				mkt.settling[ord.ID()] += tt.extraBeforeApply
			}
			mkt.SwapDone(ord, match, tt.faulted)

			got, found := mkt.settling[ord.ID()]
			if found != tt.wantSettlingFound {
				t.Fatalf("settling found = %t, want %t", found, tt.wantSettlingFound)
			}
			if found && got != tt.wantSettling {
				t.Fatalf("settling = %d, want %d", got, tt.wantSettling)
			}
			if tt.booked {
				if onBook, want := mkt.book.HaveOrder(ord.ID()), !tt.faulted; onBook != want {
					t.Fatalf("order on book = %t, want %t", onBook, want)
				}
			}
		})
	}
}

func TestMarket_RunInitialEpochState(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	dur := int64(mkt.EpochDuration())
	startEpoch := currentEpochWithHeadroom(t, dur)
	mkt.startEpochIdx = startEpoch + 2

	tm := mkt.mesh.(*tMesh)
	advanceCh := captureAdvanceEvents(t, tm, 1)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	startupDone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		mkt.Run(ctx, func(err error) {
			if err != nil {
				startupDone <- err
				return
			}
			if len(tm.entries) != 1 || tm.entries[0].Kind != meshevents.EventKindMarketStarted {
				startupDone <- fmt.Errorf("startup events = %v, want one market_started", tm.entries)
				return
			}
			startupDone <- nil
		})
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	select {
	case err := <-startupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for market_started startup callback")
	}

	waitForOrderAdmission(t, mkt)
	status := mkt.Status()
	if !status.Running || status.ActiveEpoch != startEpoch || status.StartEpoch != startEpoch {
		t.Fatalf("status = running %v active %d start %d, want true/%d/%d",
			status.Running, status.ActiveEpoch, status.StartEpoch, startEpoch, startEpoch)
	}
	mkt.epochMtx.RLock()
	currentEpoch, nextEpoch := mkt.currentEpoch, mkt.nextEpoch
	mkt.epochMtx.RUnlock()
	if currentEpoch == nil || currentEpoch.Epoch != startEpoch {
		t.Fatalf("current epoch = %v, want %d", currentEpoch, startEpoch)
	}
	if nextEpoch == nil || nextEpoch.Epoch != startEpoch+1 {
		t.Fatalf("next epoch = %v, want %d", nextEpoch, startEpoch+1)
	}
	select {
	case advance := <-advanceCh:
		t.Fatalf("advance_epoch before next epoch start: closed/opened = %d/%d",
			advance.ClosedEpochIdx, advance.OpenedEpochIdx)
	default:
	}
	select {
	case advance := <-advanceCh:
		if advance.ClosedEpochIdx != startEpoch || advance.OpenedEpochIdx != startEpoch+1 {
			t.Fatalf("advance_epoch closed/opened = %d/%d, want %d/%d",
				advance.ClosedEpochIdx, advance.OpenedEpochIdx, startEpoch, startEpoch+1)
		}
	case <-time.After(time.Duration(mkt.EpochDuration())*time.Millisecond + 250*time.Millisecond):
		t.Fatalf("timed out waiting for first advance_epoch")
	}
	cancel()
}

func captureAdvanceEvents(t *testing.T, tm *tMesh, capacity int) chan *advanceEpochEvent {
	t.Helper()
	advanceCh := make(chan *advanceEpochEvent, capacity)
	applyAdvance := tm.events[meshevents.EventKindAdvanceEpoch]
	tm.events[meshevents.EventKindAdvanceEpoch] = func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
		advance, err := meshevents.DecodeAdvanceEpochEvent(event.Payload)
		if err != nil {
			return nil, err
		}
		select {
		case advanceCh <- advance:
		default:
		}
		return applyAdvance(applyCtx, event)
	}
	return advanceCh
}

func currentEpochWithHeadroom(t *testing.T, dur int64) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Duration(dur) * time.Millisecond)
	for time.Now().Before(deadline) {
		nowMS := time.Now().UnixMilli()
		if nowMS%dur < dur/4 {
			return nowMS / dur
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for epoch headroom")
	return 0
}

func waitForOrderAdmission(t *testing.T, mkt *Market) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mkt.Running() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for order admission")
}

func setTestMarketLifecycle(mkt *Market, storage *TArchivist, lc *db.MarketLifecycle) {
	cpy := *lc
	cpy.PersistBook = cloneBool(lc.PersistBook)
	storage.mtx.Lock()
	storage.lifecycle = &cpy
	storage.mtx.Unlock()
	if err := mkt.applyMarketLifecycleRow(&cpy); err != nil {
		panic(err)
	}
}

func testLifecycleActionCount(storage *TArchivist, action db.MarketLifecycleAction) int {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	var n int
	for _, update := range storage.lifecycleUpdates {
		if update.Action == action {
			n++
		}
	}
	return n
}

func lastLifecyclePersist(storage *TArchivist, action db.MarketLifecycleAction) *bool {
	storage.mtx.Lock()
	defer storage.mtx.Unlock()
	for i := len(storage.lifecycleUpdates) - 1; i >= 0; i-- {
		if storage.lifecycleUpdates[i].Action == action {
			return storage.lifecycleUpdates[i].PersistBook
		}
	}
	return nil
}

func TestMarket_RunPendingResumeRescheduleWaitsForNewEpoch(t *testing.T) {
	mkt, storage, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	dur := int64(mkt.EpochDuration())
	nowEpoch := currentEpochWithHeadroom(t, dur)
	firstResumeEpoch := nowEpoch + 2
	rescheduledResumeEpoch := firstResumeEpoch + 2
	persist := true
	setTestMarketLifecycle(mkt, storage, &db.MarketLifecycle{
		Market:          mkt.name,
		State:           db.MarketStateSuspended,
		StartEpochIdx:   firstResumeEpoch,
		StartEpochDur:   dur,
		PendingAction:   db.MarketPendingResume,
		PendingEpochIdx: firstResumeEpoch,
		PendingEpochDur: dur,
		PersistBook:     &persist,
		RunParams:       mkt.configuredParams.MarketRunParams,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	startupDone := make(chan error, 1)
	go func() {
		defer close(runDone)
		mkt.Run(ctx, func(err error) { startupDone <- err })
	}()
	defer func() {
		cancel()
		<-runDone
	}()

	select {
	case err := <-startupDone:
		if err != nil {
			t.Fatalf("startup error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for suspended startup")
	}

	reschedule := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionScheduleResume,
		mkt.name, rescheduledResumeEpoch, dur)
	rescheduleEvent, err := mesh.NewEvent(reschedule)
	if err != nil {
		t.Fatalf("build schedule_resume event: %v", err)
	}
	if _, err := mkt.mesh.ApplyEvent(context.Background(), rescheduleEvent); err != nil {
		t.Fatalf("apply schedule_resume event: %v", err)
	}

	oldResumeDeadline := time.UnixMilli(firstResumeEpoch*dur + dur/2)
	if wait := time.Until(oldResumeDeadline); wait > 0 {
		time.Sleep(wait)
	}
	if n := testLifecycleActionCount(storage, db.MarketLifecycleActionResume); n != 0 {
		t.Fatalf("resume event count after old pending epoch = %d, want 0", n)
	}

	newResumeDeadline := time.UnixMilli(rescheduledResumeEpoch*dur + dur/2)
	for time.Now().Before(newResumeDeadline) {
		if testLifecycleActionCount(storage, db.MarketLifecycleActionResume) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := testLifecycleActionCount(storage, db.MarketLifecycleActionResume); n != 1 {
		t.Fatalf("resume event count after rescheduled epoch = %d, want 1", n)
	}
}

func TestMarket_RunUnsyncedStartupDoesNotPublishEventsOrReportReady(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	dur := int64(mkt.EpochDuration())
	nowEpoch := currentEpochWithHeadroom(t, dur)
	startEpoch := nowEpoch - 2
	mkt.epochMtx.Lock()
	mkt.startEpochIdx = startEpoch - 100
	mkt.currentEpoch = NewEpoch(startEpoch, dur)
	mkt.nextEpoch = NewEpoch(startEpoch+1, dur)
	mkt.epochMtx.Unlock()

	swapper := &countingChainsSyncedSwapper{synced: false}
	mkt.swapper = swapper

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	startupDone := make(chan error, 1)
	go func() {
		defer close(runDone)
		mkt.Run(ctx, func(err error) { startupDone <- err })
	}()
	defer func() {
		cancel()
		<-runDone
	}()

	deadline := time.After(time.Second)
	for swapper.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for ChainsSynced call")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := swapper.calls.Load(); got != 1 {
		t.Fatalf("ChainsSynced calls = %d, want 1", got)
	}

	select {
	case err := <-startupDone:
		t.Fatalf("startup callback while unsynced: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-runDone:
		t.Fatalf("market run returned before cancellation")
	default:
	}
	tm := mkt.mesh.(*tMesh)
	if len(tm.entries) != 0 {
		t.Fatalf("event count while unsynced = %d, want 0", len(tm.entries))
	}
	cancel()
	select {
	case err := <-startupDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error after cancellation = %v, want context.Canceled", err)
		}
	case <-runDone:
		t.Fatalf("market run returned before reporting cancellation")
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for canceled startup callback")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatalf("market run did not stop after canceled startup")
	}
	status := mkt.Status()
	if status.Running || status.ActiveEpoch != 0 {
		t.Fatalf("market status after canceled startup = running %v active %d, want false/0",
			status.Running, status.ActiveEpoch)
	}
}

func TestMarket_Run(t *testing.T) {
	// This test exercises the Market's main loop, which cycles the epochs and
	// queues (or not) incoming orders.

	// Create the market.
	mkt, storage, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
		cleanup()
		return
	}
	epochDurationMSec := int64(mkt.EpochDuration())
	// This test wants to know when epoch order matching booking is done.
	storage.epochInserted = make(chan struct{}, 1)
	// and when handlePreimage is done.
	auth.handlePreimageDone = make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check that start is delayed by an unsynced backend. Tell the Market to
	// start
	atomic.StoreUint32(&oRig.dcr.synced, 0)
	nowEpochIdx := time.Now().UnixMilli()/epochDurationMSec + 1

	unsyncedEpochIdx := nowEpochIdx + 1
	unsyncedEpochTime := time.UnixMilli(unsyncedEpochIdx * epochDurationMSec)

	startEpochIdx := unsyncedEpochIdx + 1
	startEpochTime := time.UnixMilli(startEpochIdx * epochDurationMSec)

	var wg sync.WaitGroup
	wg.Add(1)
	mkt.startEpochIdx = unsyncedEpochIdx
	go func() {
		defer wg.Done()
		mkt.Run(ctx, nil)
	}()

	// Make an order for the first epoch.
	clientTimeMSec := startEpochIdx*epochDurationMSec + 10 // 10 ms after epoch start
	lots := 1
	qty := uint64(dcrLotSize * lots)
	rate := uint64(1000) * dcrRateStep
	aid := test.NextAccount()
	pi := test.RandomPreimage()
	commit := pi.Commit()
	limit := &msgjson.LimitOrder{
		Prefix: msgjson.Prefix{
			AccountID:  aid[:],
			Base:       dcrID,
			Quote:      btcID,
			OrderType:  msgjson.LimitOrderNum,
			ClientTime: uint64(clientTimeMSec),
			Commit:     commit[:],
		},
		Trade: msgjson.Trade{
			Side:     msgjson.SellOrderNum,
			Quantity: qty,
			Coins:    []*msgjson.Coin{},
			Address:  btcAddr,
		},
		Rate: rate,
		TiF:  msgjson.StandingOrderNum,
	}

	newLimit := func() *order.LimitOrder {
		return &order.LimitOrder{
			P: order.Prefix{
				AccountID:  aid,
				BaseAsset:  limit.Base,
				QuoteAsset: limit.Quote,
				OrderType:  order.LimitOrderType,
				ClientTime: time.UnixMilli(clientTimeMSec),
				Commit:     commit,
			},
			T: order.Trade{
				Coins:    []order.CoinID{},
				Sell:     true,
				Quantity: limit.Quantity,
				Address:  limit.Address,
			},
			Rate:  limit.Rate,
			Force: order.StandingTiF,
		}
	}

	parcelQty := uint64(dcrLotSize)
	maxMakerQty := parcelQty * uint64(parcelLimit)
	maxTakerQty := maxMakerQty / 2

	var msgID uint64
	nextMsgID := func() uint64 { msgID++; return msgID }
	newOR := func() *orderRecord {
		return &orderRecord{
			msgID: nextMsgID(),
			req:   limit,
			order: newLimit(),
		}
	}

	storMsgPI := func(id uint64, pi order.Preimage) {
		auth.piMtx.Lock()
		auth.preimagesByMsgID[id] = pi
		auth.piMtx.Unlock()
	}

	oRecord := newOR()
	storMsgPI(oRecord.msgID, pi)
	//auth.Send will update preimagesByOrderID

	// Submit order before market starts running
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err == nil {
		t.Error("order successfully submitted to stopped market")
	}
	if !errors.Is(err, ErrMarketNotRunning) {
		t.Fatalf(`expected ErrMarketNotRunning ("%v"), got "%v"`, ErrMarketNotRunning, err)
	}

	mktStatus := mkt.Status()
	if mktStatus.Running {
		t.Fatalf("Market should not be running yet")
	}

	halfEpoch := time.Duration(epochDurationMSec/2) * time.Millisecond

	<-time.After(time.Until(unsyncedEpochTime.Add(halfEpoch)))

	if mkt.Running() {
		t.Errorf("market running with an unsynced backend")
	}

	atomic.StoreUint32(&oRig.dcr.synced, 1)

	<-time.After(time.Until(startEpochTime.Add(halfEpoch)))
	<-storage.epochInserted

	if !mkt.Running() {
		t.Errorf("market not running after backend sync finished")
	}

	// Submit again.
	limit.Quantity = dcrLotSize

	oRecord = newOR()
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err != nil {
		t.Fatal(err)
	}

	// Let the epoch cycle and the fake client respond with its preimage
	// (handlePreimageResp done)...
	<-auth.handlePreimageDone
	// and for matching to complete (in processReadyEpoch).
	<-storage.epochInserted

	// Submit an immediate taker sell (taker) over user taker limit

	piSell := test.RandomPreimage()
	commitSell := piSell.Commit()
	oRecordSell := newOR()
	limit.Commit = commitSell[:]
	loSell := oRecordSell.order.(*order.LimitOrder)
	loSell.P.Commit = commitSell
	loSell.Force = order.ImmediateTiF // likely taker
	loSell.Quantity = maxTakerQty     // one lot already booked

	storMsgPI(oRecordSell.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecordSell)
	if err == nil {
		t.Fatal("should have rejected too large likely-taker")
	}

	// Submit a taker buy that is over user taker limit
	// loSell := oRecord.order.(*order.LimitOrder)
	piBuy := test.RandomPreimage()
	commitBuy := piBuy.Commit()
	oRecordBuy := newOR()
	limit.Commit = commitBuy[:]
	loBuy := oRecordBuy.order.(*order.LimitOrder)
	loBuy.P.Commit = commitBuy
	loBuy.Sell = false
	loBuy.Quantity = maxTakerQty // One lot already booked
	// rate matches with the booked sell = likely taker

	storMsgPI(oRecordBuy.msgID, piBuy)
	err = submitOrderCommand(t, mkt, auth, oRecordBuy)
	if err == nil {
		t.Fatal("should have rejected too large likely-taker")
	}

	// Submit a likely taker with an acceptable limit
	loSell.Quantity = maxTakerQty - dcrLotSize // the limit

	storMsgPI(oRecordSell.msgID, piSell)
	err = submitOrderCommand(t, mkt, auth, oRecordSell)
	if err != nil {
		t.Fatalf("should have allowed that likely-taker: %v", err)
	}

	// Another in the same epoch will push over the limit
	loBuy.Quantity = dcrLotSize // just one lot
	storMsgPI(oRecordBuy.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecordBuy)
	if err == nil {
		t.Fatalf("should have rejected too likely-taker that pushed the limit with existing epoch status takers")
	}

	// Submit a valid cancel order.
	loID := oRecord.order.ID()
	piCo := test.RandomPreimage()
	commit = piCo.Commit()
	cancelTime := time.Now().UnixMilli()
	cancelMsg := &msgjson.CancelOrder{
		Prefix: msgjson.Prefix{
			AccountID:  aid[:],
			Base:       dcrID,
			Quote:      btcID,
			OrderType:  msgjson.CancelOrderNum,
			ClientTime: uint64(cancelTime),
			Commit:     commit[:],
		},
		TargetID: loID[:],
	}

	newCancel := func() *order.CancelOrder {
		return &order.CancelOrder{
			P: order.Prefix{
				AccountID:  aid,
				BaseAsset:  limit.Base,
				QuoteAsset: limit.Quote,
				OrderType:  order.CancelOrderType,
				ClientTime: time.UnixMilli(cancelTime),
				Commit:     commit,
			},
			TargetOrderID: loID,
		}
	}
	co := newCancel()

	coRecord := orderRecord{
		msgID: nextMsgID(),
		req:   cancelMsg,
		order: co,
	}

	// Cancel order w/o permission to cancel target order (the limit order from
	// above that is now booked)
	cancelTime++
	otherAccount := test.NextAccount()
	cancelMsg.ClientTime = uint64(cancelTime)
	cancelMsg.AccountID = otherAccount[:]
	coWrongAccount := newCancel()
	piBadCo := test.RandomPreimage()
	commitBadCo := piBadCo.Commit()
	coWrongAccount.Commit = commitBadCo
	coWrongAccount.AccountID = otherAccount
	coWrongAccount.ClientTime = time.UnixMilli(cancelTime)
	cancelMsg.Commit = commitBadCo[:]
	coRecordWrongAccount := orderRecord{
		msgID: nextMsgID(),
		req:   cancelMsg,
		order: coWrongAccount,
	}

	// Submit the invalid cancel order first because it would be caught by the
	// duplicate check if we do it after the valid one is submitted.
	storMsgPI(coRecordWrongAccount.msgID, piBadCo)
	err = submitOrderCommand(t, mkt, auth, &coRecordWrongAccount)
	if err == nil {
		t.Errorf("An invalid order was processed, but it should not have been.")
	} else if !errors.Is(err, ErrCancelNotPermitted) {
		t.Errorf(`expected ErrCancelNotPermitted ("%v"), got "%v"`, ErrCancelNotPermitted, err)
	}

	// Valid cancel order
	storMsgPI(coRecord.msgID, piCo)
	err = submitOrderCommand(t, mkt, auth, &coRecord)
	if err != nil {
		t.Fatalf("Failed to submit order: %v", err)
	}

	// Duplicate cancel order
	piCoDup := test.RandomPreimage()
	commit = piCoDup.Commit()
	cancelTime++
	cancelMsg.ClientTime = uint64(cancelTime)
	cancelMsg.Commit = commit[:]
	coDup := newCancel()
	coDup.Commit = commit
	coDup.ClientTime = time.UnixMilli(cancelTime)
	coRecordDup := orderRecord{
		msgID: nextMsgID(),
		req:   cancelMsg,
		order: coDup,
	}
	storMsgPI(coRecordDup.msgID, piCoDup)
	err = submitOrderCommand(t, mkt, auth, &coRecordDup)
	if err == nil {
		t.Errorf("An duplicate cancel order was processed, but it should not have been.")
	} else if !errors.Is(err, ErrDuplicateCancelOrder) {
		t.Errorf(`expected ErrDuplicateCancelOrder ("%v"), got "%v"`, ErrDuplicateCancelOrder, err)
	}

	// Let the epoch cycle and the fake client respond with its preimage
	// (handlePreimageResp done)..
	<-auth.handlePreimageDone
	// and for matching to complete (in processReadyEpoch).
	<-storage.epochInserted

	cancel()
	wg.Wait()
	cleanup()

	// Test duplicate order (commitment) with a new Market.
	mkt, storage, auth, cleanup, err = newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	storage.epochInserted = make(chan struct{}, 1)
	auth.handlePreimageDone = make(chan struct{}, 1)

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		mkt.Run(ctx, nil)
	}()
	waitForOrderAdmission(t, mkt)

	// fresh oRecord
	oRecord = newOR()
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err != nil {
		t.Fatalf("first order: %v", err)
	}
	firstOrder := oRecord.order
	firstID := firstOrder.ID()
	firstServerTime := firstOrder.Time()
	for auth.getSend() != nil {
	}

	oRecord = newOR()
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err != nil {
		t.Fatalf("identical resend: %v", err)
	}
	msg := auth.getSend()
	if msg == nil {
		t.Fatal("identical resend delivered no result")
	}
	var res msgjson.OrderResult
	if err := msg.UnmarshalResult(&res); err != nil {
		t.Fatalf("identical resend result: %v", err)
	}
	gotID, err := order.IDFromBytes(res.OrderID)
	if err != nil {
		t.Fatalf("identical resend order id: %v", err)
	}
	if gotID != firstID {
		t.Errorf("identical resend order ID = %v, want %v", gotID, firstID)
	}
	if res.ServerTime != uint64(firstServerTime) {
		t.Errorf("identical resend server time = %d, want %d", res.ServerTime, firstServerTime)
	}

	oRecord = newOR()
	oRecord.order.(*order.LimitOrder).Quantity *= 2
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err == nil {
		t.Errorf("A duplicate commitment was processed, but it should not have been.")
	} else if !errors.Is(err, ErrInvalidCommitment) {
		t.Errorf(`expected ErrInvalidCommitment ("%v"), got "%v"`, ErrInvalidCommitment, err)
	}

	// Send an order with a bad lot size.
	oRecord = newOR()
	oRecord.order.(*order.LimitOrder).Quantity += mkt.configuredParams.LotSize / 2
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err == nil {
		t.Errorf("An invalid order was processed, but it should not have been.")
	} else if !errors.Is(err, ErrInvalidOrder) {
		t.Errorf(`expected ErrInvalidOrder ("%v"), got "%v"`, ErrInvalidOrder, err)
	}

	// Rate too low. The live floor is the run's adopted parameter set, so
	// pin a floor above the order's rate there.
	oRecord = newOR()
	setTestRunMinimumRate(t, mkt, oRecord.order.(*order.LimitOrder).Rate+1)
	storMsgPI(oRecord.msgID, pi)
	if err = submitOrderCommand(t, mkt, auth, oRecord); !errors.Is(err, ErrInvalidRate) {
		t.Errorf("An invalid rate was accepted, but it should not have been.")
	}
	setTestRunMinimumRate(t, mkt, 0)

	// Let the epoch cycle and the fake client respond with its preimage
	// (handlePreimageResp done)..
	<-auth.handlePreimageDone
	// and for matching to complete (in processReadyEpoch).
	<-storage.epochInserted

	storage.mtx.Lock()
	storage.commitOrdersErr = errors.New("db down")
	storage.mtx.Unlock()
	handled, rpcErr := mkt.ResendOfKnownOrder(ctx, newOR(), nil)
	if !handled || rpcErr == nil || rpcErr.Code != msgjson.TryAgainLaterError {
		t.Fatalf("resend lookup failure = (%v, %v), want handled TryAgainLater", handled, rpcErr)
	}

	storage.mtx.Lock()
	storage.commitOrdersErr = nil
	storage.commitOrders = []db.CommitOrder{{Order: firstOrder, Status: order.OrderStatusRevoked}}
	storage.mtx.Unlock()
	handled, rpcErr = mkt.ResendOfKnownOrder(ctx, newOR(), nil)
	if !handled || rpcErr == nil || rpcErr.Code != msgjson.UnknownOrderError {
		t.Fatalf("archived resend = (%v, %v), want handled UnknownOrderError", handled, rpcErr)
	}

	// Submit an order with a Commitment known to the DB.
	// NOTE: disabled since the OrderWithCommit check in Market.processOrder is disabled too.
	// oRecord = newOR()
	// oRecord.order.SetTime(time.Now()) // This will register a different order ID with the DB in the next statement.
	// storage.failOnCommitWithOrder(oRecord.order)
	// storMsgPI(oRecord.msgID, pi)
	// err = submitOrderCommand(t, mkt, auth, oRecord) // Will re-stamp the order, but the commit will be the same.
	// if err == nil {
	// 	t.Errorf("A duplicate order was processed, but it should not have been.")
	// } else if !errors.Is(err, ErrInvalidCommitment) {
	// 	t.Errorf(`expected ErrInvalidCommitment ("%v"), got "%v"`, ErrInvalidCommitment, err)
	// }

	// Submit an order with a zero commit.
	oRecord = newOR()
	oRecord.order.(*order.LimitOrder).Commit = order.Commitment{}
	storMsgPI(oRecord.msgID, pi)
	err = submitOrderCommand(t, mkt, auth, oRecord)
	if err == nil {
		t.Errorf("An order with a zero Commitment was processed, but it should not have been.")
	} else if !errors.Is(err, ErrInvalidCommitment) {
		t.Errorf(`expected ErrInvalidCommitment ("%v"), got "%v"`, ErrInvalidCommitment, err)
	}

	// Submit an order that breaks storage somehow.
	// tweak the order's commitment+preimage so it's not a dup.
	oRecord = newOR()
	pi = test.RandomPreimage()
	commit = pi.Commit()
	lo := oRecord.order.(*order.LimitOrder)
	lo.Commit = commit
	limit.Commit = commit[:] // oRecord.req
	storMsgPI(oRecord.msgID, pi)
	storage.failOnEpochOrder(lo) // force storage to fail on this order
	if err = submitOrderCommand(t, mkt, auth, oRecord); !errors.Is(err, ErrInternalServer) {
		t.Errorf(`expected ErrInternalServer ("%v"), got "%v"`, ErrInternalServer, err)
	}

	cancel()
	wg.Wait()
	cleanup()
}

func TestMarket_enqueueEpoch(t *testing.T) {
	mkt, _, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("Failed to create test market: %v", err)
		return
	}
	defer cleanup()

	rnd.Seed(0) // deterministic random data

	// Book orders need no preimages.
	for i := 0; i < 8; i++ {
		lo := makeLO(buyer3, mkRate3(0.8, 1.0), randLots(10), order.StandingTiF)
		if !mkt.book.Insert(lo) {
			t.Fatalf("Failed to Insert order into book.")
		}
		lo = makeLO(seller3, mkRate3(1.0, 1.2), randLots(10), order.StandingTiF)
		if !mkt.book.Insert(lo) {
			t.Fatalf("Failed to Insert order into book.")
		}
	}

	bestBuy, bestSell := mkt.book.Best()
	bestBuyRate := bestBuy.Rate
	bestBuyQuant := bestBuy.Quantity * 3 // tweak for new shuffle seed without changing csum
	bestSellID := bestSell.ID()

	var epochIdx, epochDur int64 = 123413513, mkt.configuredParams.epochDur
	eq := NewEpoch(epochIdx, epochDur)
	lo, loPI := makeLORevealed(seller3, bestBuyRate-dcrRateStep, bestBuyQuant, order.StandingTiF)
	co, coPI := makeCORevealed(buyer3, bestSellID)
	eq.Insert(lo)
	eq.Insert(co)

	cSum, _ := hex.DecodeString("4859aa186630c2b135074037a8db42f240bbbe81c1361d8783aa605ed3f0cf90")

	eq2 := NewEpoch(epochIdx, epochDur)
	co2, co2PI := makeCORevealed(buyer3, randomOrderID())
	lo2, _ := makeLORevealed(seller3, bestBuyRate-dcrRateStep, bestBuyQuant, order.ImmediateTiF)
	eq2.Insert(co2)
	eq2.Insert(lo2) // lo2 will not be in preimage map (miss)

	cSum2, _ := hex.DecodeString("a64ee6372a49f9465910ca0b556818dbc765f3c7fa21d5f40ab25bf4b73f45ed") // includes both commitments, including the miss

	auth.piMtx.Lock()
	auth.preimagesByOrdID[lo.UID()] = loPI
	auth.preimagesByOrdID[co.UID()] = coPI
	auth.preimagesByOrdID[co2.UID()] = co2PI
	// No lo2 (miss)
	auth.piMtx.Unlock()

	var wg sync.WaitGroup
	defer wg.Wait() // wait for the following epoch pipeline goroutines

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the following epoch pipeline goroutines

	// This test does not start the entire market, so manually start the epoch
	// queue pump, and a goroutine to receive ready (preimage collection
	// completed) epochs and start matching, etc.
	ePump := newEpochPump()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ePump.Run(ctx)
	}()

	goForIt := make(chan struct{}, 1)

	wg.Add(1)
	go func() {
		defer close(goForIt)
		defer wg.Done()
		for ep := range ePump.ready {
			t.Logf("processReadyEpoch: %d orders revealed\n", len(ep.ordersRevealed))

			// Preimage collection is complete; close the epoch.
			if err := mkt.processReadyEpoch(ctx, ep); err != nil {
				t.Errorf("processReadyEpoch error: %v", err)
			}
			goForIt <- struct{}{}
		}
	}()

	tests := []struct {
		name         string
		epoch        *EpochQueue
		wantCSum     []byte
		wantRevealed []order.Order
		wantMissed   []order.Order
	}{
		{
			name:         "ok book unbook",
			epoch:        eq,
			wantCSum:     cSum,
			wantRevealed: []order.Order{lo, co},
		},
		{
			name:         "ok no matches or book updates, one miss",
			epoch:        eq2,
			wantCSum:     cSum2,
			wantRevealed: []order.Order{co2},
			wantMissed:   []order.Order{lo2},
		},
		{
			name:  "ok empty queue",
			epoch: NewEpoch(epochIdx, epochDur),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tMesh := mkt.mesh.(*tMesh)
			startEntries := len(tMesh.entries)
			mkt.enqueueEpoch(ePump, tt.epoch)
			// Wait for preimage collection and epoch processing.
			<-goForIt

			entries := tMesh.entries[startEntries:]
			if len(entries) != 1 {
				kinds := make([]string, 0, len(entries))
				for _, entry := range entries {
					kinds = append(kinds, entry.Kind)
				}
				t.Fatalf("event entries = %v, want [%q]",
					kinds, meshevents.EventKindEpochProcessed)
			}

			processed, err := meshevents.DecodeEpochProcessedEvent(entries[0].Payload)
			if err != nil {
				t.Fatalf("decode epoch processed event: %v", err)
			}
			if processed.EpochIdx != epochIdx || processed.EpochDur != epochDur {
				t.Fatalf("processed epoch = %d:%d, want %d:%d",
					processed.EpochIdx, processed.EpochDur, epochIdx, epochDur)
			}
			if !bytes.Equal(processed.CSum, tt.wantCSum) {
				t.Fatalf("processed csum = %x, want %x", processed.CSum, tt.wantCSum)
			}
			revealed, err := processed.OrdersRevealed()
			if err != nil {
				t.Fatalf("processed revealed orders: %v", err)
			}
			revealedIDs := make(map[order.OrderID]struct{}, len(revealed))
			for _, revealed := range revealed {
				revealedIDs[revealed.Order.ID()] = struct{}{}
			}
			if len(revealedIDs) != len(tt.wantRevealed) {
				t.Fatalf("revealed orders = %d, want %d", len(revealedIDs), len(tt.wantRevealed))
			}
			for _, ord := range tt.wantRevealed {
				if _, found := revealedIDs[ord.ID()]; !found {
					t.Fatalf("revealed orders missing %v", ord.ID())
				}
			}
			missed, err := processed.MissedOrders()
			if err != nil {
				t.Fatalf("processed missed orders: %v", err)
			}
			if len(missed) != len(tt.wantMissed) {
				t.Fatalf("missed orders = %d, want %d", len(missed), len(tt.wantMissed))
			}
			for i, ord := range tt.wantMissed {
				if missed[i].ID() != ord.ID() {
					t.Fatalf("missed order %d = %v, want %v", i, missed[i].ID(), ord.ID())
				}
			}
		})
	}

	cancel()
}

// TestWaitForEpochClosures checks the advancer-side closure watermark: an
// advance within the allowed closure lag proceeds immediately, one past it
// blocks until the watermark moves, and context cancellation unblocks it.
func TestWaitForEpochClosures(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("Failed to create test market: %v", err)
	}
	defer cleanup()
	d := &marketEpochDriver{m: mkt}

	setWatermark := func(idx int64) {
		mkt.epochMtx.Lock()
		mkt.processedEpochIdx = idx
		mkt.epochMtx.Unlock()
		mkt.wakeClosureWaiter()
	}
	setWatermark(10)

	// Closing 10+db.MaxUnprocessedClosedEpochs is exactly at the limit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.waitForEpochClosures(ctx, 10+db.MaxUnprocessedClosedEpochs); err != nil {
		t.Fatalf("waitForEpochClosures at the lag limit: %v", err)
	}

	// One epoch further must wait for the watermark.
	done := make(chan error, 1)
	go func() { done <- d.waitForEpochClosures(ctx, 11+db.MaxUnprocessedClosedEpochs) }()
	select {
	case err := <-done:
		t.Fatalf("waitForEpochClosures did not wait (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	setWatermark(11)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForEpochClosures after watermark move: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("waitForEpochClosures still waiting after watermark move")
	}

	// Context cancellation unblocks a stuck wait.
	go func() { done <- d.waitForEpochClosures(ctx, 20) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("cancelled waitForEpochClosures returned no error")
		}
	case <-time.After(time.Second):
		t.Fatalf("waitForEpochClosures ignored context cancellation")
	}
}

// TestMarket_enqueueEpochCompletesReady checks that preimage collection
// completes the pump entry without publishing any mesh event of its own: the
// collection outcome only travels in the epoch_processed event built later.
func TestMarket_enqueueEpochCompletesReady(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("Failed to create test market: %v", err)
	}
	defer cleanup()

	ePump := newEpochPump()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ePump.Run(ctx)
	}()
	defer wg.Wait()
	defer cancel()

	epoch := NewEpoch(123413514, mkt.configuredParams.epochDur)
	if !mkt.enqueueEpoch(ePump, epoch) {
		t.Fatalf("enqueueEpoch returned false")
	}

	select {
	case ep, ok := <-ePump.ready:
		if !ok {
			t.Fatalf("epoch pump closed before emitting the epoch")
		}
		select {
		case <-ep.ready:
		default:
			t.Fatalf("ready epoch channel was not closed")
		}
		if ep.missRevokeTime.IsZero() {
			t.Fatalf("ready epoch has no miss revoke time")
		}
	case <-time.After(time.Second):
		t.Fatalf("epoch pump blocked waiting for preimage collection")
	}
	if entries := mkt.mesh.(*tMesh).entries; len(entries) != 0 {
		t.Fatalf("preimage collection published %d mesh events, want 0", len(entries))
	}
}

func TestMarket_handlePreimageResp(t *testing.T) {
	randomCommit := func() (com order.Commitment) {
		rnd.Read(com[:])
		return
	}

	newOrder := func() (*order.LimitOrder, order.Preimage) {
		qty := uint64(dcrLotSize * 10)
		rate := uint64(1000) * dcrRateStep
		return makeLORevealed(seller3, rate, qty, order.StandingTiF)
	}

	authMgr := &TAuth{}
	mkt := &Market{
		auth:    authMgr,
		storage: &TArchivist{},
	}

	piMsg := &msgjson.PreimageResponse{
		Preimage: msgjson.Bytes{},
	}
	msg, _ := msgjson.NewResponse(5, piMsg, nil)

	runAndReceive := func(msg *msgjson.Message, dat *piData) *order.Preimage {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			mkt.handlePreimageResp(msg, dat)
			wg.Done()
		}()
		piRes := <-dat.preimage
		wg.Wait()
		return piRes
	}

	// 1. bad Message.Type: RPCParseError
	msg.Type = msgjson.Request // should be Response
	lo, pi := newOrder()
	dat := &piData{lo, make(chan *order.Preimage)}
	piRes := runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	// Inspect the servers rpc error response message.
	respMsg := authMgr.getSend()
	if respMsg == nil {
		t.Fatalf("no error response")
	}
	resp, _ := respMsg.Response()
	msgErr := resp.Error
	// Code 1, Message about parsing response and invalid type (1 is not response)
	if msgErr.Code != msgjson.RPCParseError {
		t.Errorf("Expected error code %d, got %d", msgjson.RPCParseError, msgErr.Code)
	}
	wantMsgPrefix := "error parsing preimage notification response"
	if !strings.Contains(msgErr.Message, wantMsgPrefix) {
		t.Errorf("Expected error message %q, got %q", wantMsgPrefix, msgErr.Message)
	}

	// 2. empty preimage from client: InvalidPreimage
	msg, _ = msgjson.NewResponse(5, piMsg, nil)
	//lo, pi := newOrder()
	dat = &piData{lo, make(chan *order.Preimage)}
	piRes = runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	respMsg = authMgr.getSend()
	if respMsg == nil {
		t.Fatalf("no error response")
	}
	resp, _ = respMsg.Response()
	msgErr = resp.Error
	// 30 invalid preimage length (0 byes)
	if msgErr.Code != msgjson.InvalidPreimage {
		t.Errorf("Expected error code %d, got %d", msgjson.InvalidPreimage, msgErr.Code)
	}
	if !strings.Contains(msgErr.Message, "invalid preimage length") {
		t.Errorf("Expected error message %q, got %q",
			"invalid preimage length (0 bytes)",
			msgErr.Message)
	}

	// 3. correct preimage length, commitment mismatch
	//lo, pi := newOrder()
	lo.Commit = randomCommit() // break the commitment
	dat = &piData{
		ord:      lo,
		preimage: make(chan *order.Preimage),
	}
	piMsg = &msgjson.PreimageResponse{
		Preimage: pi[:],
	}

	msg, _ = msgjson.NewResponse(5, piMsg, nil)
	piRes = runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	respMsg = authMgr.getSend()
	if respMsg == nil {
		t.Fatalf("no error response")
	}
	resp, _ = respMsg.Response()
	msgErr = resp.Error
	// 30 invalid preimage length (0 byes)
	if msgErr.Code != msgjson.PreimageCommitmentMismatch {
		t.Errorf("Expected error code %d, got %d",
			msgjson.PreimageCommitmentMismatch, msgErr.Code)
	}
	if !strings.Contains(msgErr.Message, "does not match order commitment") {
		t.Errorf("Expected error message of the form %q, got %q",
			"preimage hash {hash} does not match order commitment {commit}",
			msgErr.Message)
	}

	// 4. correct preimage and commit
	lo.Commit = pi.Commit() // fix the commitment
	dat = &piData{
		ord:      lo,
		preimage: make(chan *order.Preimage),
	}
	piMsg = &msgjson.PreimageResponse{
		Preimage: pi[:],
	}

	piRes = runAndReceive(msg, dat)
	if piRes == nil {
		t.Errorf("Expected preimage %x, got <nil>", pi)
	} else if *piRes != pi {
		t.Errorf("Expected preimage %x, got %x", pi, *piRes)
	}

	// no response this time (no error)
	respMsg = authMgr.getSend()
	if respMsg != nil {
		t.Fatalf("got error response: %d %q", respMsg.Type, string(respMsg.Payload))
	}

	// 5. client classified server request as invalid: InvalidRequestError
	msg, _ = msgjson.NewResponse(5, nil, msgjson.NewError(msgjson.InvalidRequestError, "invalid request"))
	lo, pi = newOrder()
	dat = &piData{lo, make(chan *order.Preimage)}
	piRes = runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	// Inspect the servers rpc error response message.
	respMsg = authMgr.getSend()
	if respMsg != nil {
		t.Fatalf("server is not expected to respond with anything")
	}

	// 6. payload is not msgjson.PreimageResponse, unmarshal still succeeds, but PI is nil
	notaPiMsg := new(msgjson.OrderBookSubscription)
	msg, _ = msgjson.NewResponse(5, notaPiMsg, nil)
	dat = &piData{lo, make(chan *order.Preimage)}
	piRes = runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	respMsg = authMgr.getSend()
	if respMsg == nil {
		t.Fatalf("no error response")
	}
	resp, _ = respMsg.Response()
	msgErr = resp.Error
	// 30 invalid preimage length (0 byes)
	if msgErr.Code != msgjson.InvalidPreimage {
		t.Errorf("Expected error code %d, got %d", msgjson.InvalidPreimage, msgErr.Code)
	}
	if !strings.Contains(msgErr.Message, "invalid preimage length") {
		t.Errorf("Expected error message %q, got %q",
			"invalid preimage length (0 bytes)",
			msgErr.Message)
	}

	// 7. payload unmarshal error
	msg, _ = msgjson.NewResponse(5, piMsg, nil)
	msg.Payload = json.RawMessage(`{"result":1}`) // ResponsePayload with invalid Result
	dat = &piData{lo, make(chan *order.Preimage)}
	piRes = runAndReceive(msg, dat)
	if piRes != nil {
		t.Errorf("Expected <nil> preimage, got %v", piRes)
	}

	respMsg = authMgr.getSend()
	if respMsg == nil {
		t.Fatalf("no error response")
	}
	resp, _ = respMsg.Response()
	msgErr = resp.Error
	// Code 1, Message about parsing response payload and invalid type (1 is not response)
	if msgErr.Code != msgjson.RPCParseError {
		t.Errorf("Expected error code %d, got %d", msgjson.RPCParseError, msgErr.Code)
	}
	// wrapped json.UnmarshalFieldError
	wantMsgPrefix = "error parsing preimage response payload result"
	if !strings.Contains(msgErr.Message, wantMsgPrefix) {
		t.Errorf("Expected error message %q, got %q", wantMsgPrefix, msgErr.Message)
	}
}

func TestMarket_MarketStartup_AccountBased(t *testing.T) {
	testAccountAssets(t, true, false)
	testAccountAssets(t, false, true)
	testAccountAssets(t, true, true)
}

func testAccountAssets(t *testing.T, base, quote bool) {
	storage := &TArchivist{}
	balancer := newTBalancer()
	const numPerSide = 10
	ords := make([]*order.LimitOrder, 0, numPerSide*2)

	baseAsset, quoteAsset := assetDCR, assetBTC
	if base {
		baseAsset = assetETH
	}
	if quote {
		quoteAsset = assetMATIC
	}

	for i := 0; i < numPerSide*2; i++ {
		writer := test.RandomWriter()
		writer.Market = &test.Market{
			Base:    baseAsset.ID,
			Quote:   quoteAsset.ID,
			LotSize: dcrLotSize,
		}
		writer.Sell = i%2 == 0
		ord := makeLO(writer, mkRate3(0.8, 1.0), randLots(10), order.StandingTiF)
		if (ord.Sell && base) || (!ord.Sell && quote) { // eth-funded order needs a account address coin.
			ord.Coins = []order.CoinID{[]byte(test.RandomAddress())}
		}
		ords = append(ords, ord)
		storage.BookOrder(ord)
	}

	mkt, storage, _, cleanup, err := newTestMarket(storage, balancer, [2]*asset.BackedAsset{baseAsset, quoteAsset})
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	for _, lo := range ords {
		if base && balancer.reqs[lo.BaseAccount()] != 0 {
			t.Fatalf("constructor requested base balance for order")
		}
		if quote && balancer.reqs[lo.QuoteAccount()] != 0 {
			t.Fatalf("constructor requested quote balance for order")
		}
	}

	if _, err := mkt.submitMarketStarted(context.Background()); err != nil {
		t.Fatalf("submitMarketStarted error: %v", err)
	}
	if len(storage.marketStartedUpdates) != 1 {
		t.Fatalf("market started update count = %d, want 1", len(storage.marketStartedUpdates))
	}
	if len(storage.marketStartedUpdates[0].BookedRevokes) != 0 {
		t.Fatalf("startup cleanup unexpectedly revoked %d orders", len(storage.marketStartedUpdates[0].BookedRevokes))
	}
	for _, lo := range ords {
		if base && balancer.reqs[lo.BaseAccount()] == 0 {
			t.Fatalf("base balance not requested for order")
		}
		if quote && balancer.reqs[lo.QuoteAccount()] == 0 {
			t.Fatalf("quote balance not requested for order")
		}
	}
}

func TestMarket_AccountPending(t *testing.T) {
	storage := &TArchivist{}
	writer := test.RandomWriter()
	writer.Market = &test.Market{
		Base:    assetETH.ID,
		Quote:   assetMATIC.ID,
		LotSize: dcrLotSize,
	}

	const rate = btcRateStep * 100
	const sellLots = 10
	const buyLots = 20
	ethAddr := test.RandomAddress()
	maticAddr := test.RandomAddress()

	writer.Sell = true
	lo := makeLO(writer, rate, sellLots, order.StandingTiF)
	lo.Coins = []order.CoinID{[]byte(ethAddr)}
	lo.Address = maticAddr
	storage.BookOrder(lo)

	writer.Sell = false
	lo = makeLO(writer, rate, buyLots, order.StandingTiF)
	lo.Coins = []order.CoinID{[]byte(maticAddr)}
	lo.Address = ethAddr
	storage.BookOrder(lo)

	mkt, _, _, cleanup, err := newTestMarket(storage, newTBalancer(), [2]*asset.BackedAsset{assetETH, assetMATIC})
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	checkPending := func(tag string, addr string, assetID uint32, expQty, expLots uint64, expRedeems int) {
		t.Helper()
		qty, lots, redeems := mkt.AccountPending(addr, assetID)
		if qty != expQty {
			t.Fatalf("%s: wrong quantity: wanted %d, got %d", tag, expQty, qty)
		}
		if lots != expLots {
			t.Fatalf("%s: wrong lots: wanted %d, got %d", tag, expLots, lots)
		}
		if redeems != expRedeems {
			t.Fatalf("%s: wrong redeems: wanted %d, got %d", tag, expRedeems, redeems)
		}
	}

	checkPending("booked-only-eth", ethAddr, assetETH.ID, sellLots*dcrLotSize, sellLots, buyLots)

	quoteQty := calc.BaseToQuote(rate, buyLots*dcrLotSize)
	checkPending("booked-only-matic", maticAddr, assetMATIC.ID, quoteQty, buyLots, sellLots)

	const epochSellLots = 5
	writer.Sell = true
	lo = makeLO(writer, rate, epochSellLots, order.StandingTiF)
	lo.Coins = []order.CoinID{[]byte(ethAddr)}
	lo.Address = maticAddr
	mkt.epochOrders[lo.ID()] = lo
	const totalSellLots = sellLots + epochSellLots
	checkPending("with-epoch-sell-eth", ethAddr, assetETH.ID, totalSellLots*dcrLotSize, totalSellLots, buyLots)
	checkPending("with-epoch-sell-matic", maticAddr, assetMATIC.ID, quoteQty, buyLots, totalSellLots)

	// Market buy order.
	midGap := mkt.MidGap()
	mktBuyQty := quoteQty + calc.BaseToQuote(midGap, dcrLotSize/2)
	writer.Sell = false
	mo := makeMO(writer, 0)
	mo.Quantity = mktBuyQty
	mo.Coins = []order.CoinID{[]byte(maticAddr)}
	mo.Address = ethAddr
	mkt.epochOrders[mo.ID()] = mo
	redeems := int(totalSellLots)
	totalBuyLots := buyLots + calc.QuoteToBase(midGap, mktBuyQty)/dcrLotSize
	totalQty := quoteQty + mktBuyQty
	checkPending("with-epoch-market-buy-matic", maticAddr, assetMATIC.ID, totalQty, totalBuyLots, redeems)
	checkPending("with-epoch-market-buy-eth", ethAddr, assetETH.ID, totalSellLots*dcrLotSize, totalSellLots, int(totalBuyLots))
}

func TestSubscribeMMSnapshots(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	user := test.NextAccount()

	// Subscribe.
	mkt.SubscribeMMSnapshots(user, false)
	mkt.mmSnapshotMtx.RLock()
	_, ok := mkt.mmSnapshotSubs[user]
	mkt.mmSnapshotMtx.RUnlock()
	if !ok {
		t.Fatal("user not in mmSnapshotSubs after subscribe")
	}

	// Unsubscribe.
	mkt.SubscribeMMSnapshots(user, true)
	mkt.mmSnapshotMtx.RLock()
	_, ok = mkt.mmSnapshotSubs[user]
	mkt.mmSnapshotMtx.RUnlock()
	if ok {
		t.Fatal("user still in mmSnapshotSubs after unsubscribe")
	}
}

func TestSendMMSnapshots(t *testing.T) {
	mkt, _, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	// No subscribers — no messages should be sent.
	epoch := &readyEpoch{
		EpochQueue: NewEpoch(100, 500),
	}
	auth.sendsMtx.Lock()
	auth.sends = auth.sends[:0]
	auth.sendsMtx.Unlock()

	mkt.sendMMSnapshots(epoch.Epoch, epoch.Duration)

	auth.sendsMtx.Lock()
	nSends := len(auth.sends)
	auth.sendsMtx.Unlock()
	if nSends != 0 {
		t.Fatalf("expected 0 sends with no subscribers, got %d", nSends)
	}

	// Subscribe two users.
	user1 := buyer3.Acct
	user2 := seller3.Acct

	mkt.SubscribeMMSnapshots(user1, false)
	mkt.SubscribeMMSnapshots(user2, false)

	// Insert buy orders for user1 and sell orders for user2.
	buyRate1 := uint64(2_500_000_000)
	buyRate2 := uint64(2_600_000_000)
	sellRate1 := uint64(2_700_000_000)
	sellRate2 := uint64(2_800_000_000)

	loBuy1 := makeLO(buyer3, buyRate1, 1, order.StandingTiF)
	loBuy1.AccountID = user1
	loBuy2 := makeLO(buyer3, buyRate2, 2, order.StandingTiF)
	loBuy2.AccountID = user1

	loSell1 := makeLO(seller3, sellRate1, 1, order.StandingTiF)
	loSell1.AccountID = user2
	loSell2 := makeLO(seller3, sellRate2, 2, order.StandingTiF)
	loSell2.AccountID = user2

	mkt.bookMtx.Lock()
	for _, lo := range []*order.LimitOrder{loBuy1, loBuy2, loSell1, loSell2} {
		if !mkt.book.Insert(lo) {
			t.Fatalf("failed to insert order into book")
		}
	}
	mkt.bookMtx.Unlock()

	auth.sendsMtx.Lock()
	auth.sends = auth.sends[:0]
	auth.sendsMtx.Unlock()

	mkt.sendMMSnapshots(epoch.Epoch, epoch.Duration)

	auth.sendsMtx.Lock()
	sends := make([]*msgjson.Message, len(auth.sends))
	copy(sends, auth.sends)
	auth.sendsMtx.Unlock()

	if len(sends) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(sends))
	}

	// Each send should be an MMEpochSnapshotRoute notification.
	for _, msg := range sends {
		if msg.Route != msgjson.MMEpochSnapshotRoute {
			t.Fatalf("expected route %s, got %s", msgjson.MMEpochSnapshotRoute, msg.Route)
		}
		var snap msgjson.MMEpochSnapshot
		if err := msg.Unmarshal(&snap); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if snap.EpochIdx != uint64(epoch.Epoch) {
			t.Fatalf("expected epochIdx %d, got %d", epoch.Epoch, snap.EpochIdx)
		}
		if snap.EpochDur != uint64(epoch.Duration) {
			t.Fatalf("expected epochDur %d, got %d", epoch.Duration, snap.EpochDur)
		}
		acct := account.AccountID{}
		copy(acct[:], snap.AccountID)
		if acct == user1 {
			// user1 has buy orders only.
			if len(snap.BuyOrders) != 2 {
				t.Fatalf("user1: expected 2 buy orders, got %d", len(snap.BuyOrders))
			}
			if len(snap.SellOrders) != 0 {
				t.Fatalf("user1: expected 0 sell orders, got %d", len(snap.SellOrders))
			}
			// Verify orders are sorted by rate ascending.
			if snap.BuyOrders[0].Rate >= snap.BuyOrders[1].Rate {
				t.Fatalf("user1: buy orders not sorted by rate ascending: %d >= %d",
					snap.BuyOrders[0].Rate, snap.BuyOrders[1].Rate)
			}
		} else if acct == user2 {
			// user2 has sell orders only.
			if len(snap.BuyOrders) != 0 {
				t.Fatalf("user2: expected 0 buy orders, got %d", len(snap.BuyOrders))
			}
			if len(snap.SellOrders) != 2 {
				t.Fatalf("user2: expected 2 sell orders, got %d", len(snap.SellOrders))
			}
			// Verify orders are sorted by rate ascending.
			if snap.SellOrders[0].Rate >= snap.SellOrders[1].Rate {
				t.Fatalf("user2: sell orders not sorted by rate ascending: %d >= %d",
					snap.SellOrders[0].Rate, snap.SellOrders[1].Rate)
			}
		} else {
			t.Fatalf("unexpected accountID: %x", snap.AccountID)
		}

		// BestBuy and BestSell should reflect the book.
		if snap.BestBuy == 0 {
			t.Fatal("BestBuy should be non-zero")
		}
		if snap.BestSell == 0 {
			t.Fatal("BestSell should be non-zero")
		}
	}

	// Subscriber with no orders should get empty order lists.
	user3 := test.NextAccount()
	mkt.SubscribeMMSnapshots(user3, false)

	auth.sendsMtx.Lock()
	auth.sends = auth.sends[:0]
	auth.sendsMtx.Unlock()

	mkt.sendMMSnapshots(epoch.Epoch, epoch.Duration)

	auth.sendsMtx.Lock()
	sends = make([]*msgjson.Message, len(auth.sends))
	copy(sends, auth.sends)
	auth.sendsMtx.Unlock()

	// 3 subscribers now.
	if len(sends) != 3 {
		t.Fatalf("expected 3 sends, got %d", len(sends))
	}
	// Find user3's snapshot.
	for _, msg := range sends {
		var snap msgjson.MMEpochSnapshot
		if err := msg.Unmarshal(&snap); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		acct := account.AccountID{}
		copy(acct[:], snap.AccountID)
		if acct == user3 {
			if len(snap.BuyOrders) != 0 {
				t.Fatalf("user3: expected 0 buy orders, got %d", len(snap.BuyOrders))
			}
			if len(snap.SellOrders) != 0 {
				t.Fatalf("user3: expected 0 sell orders, got %d", len(snap.SellOrders))
			}
		}
	}
}

func TestSendMMSnapshotsAutoUnsub(t *testing.T) {
	mkt, _, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	user1 := buyer3.Acct
	user2 := seller3.Acct

	mkt.SubscribeMMSnapshots(user1, false)
	mkt.SubscribeMMSnapshots(user2, false)

	// Make Send fail for user1.
	auth.sendErrForMtx.Lock()
	if auth.sendErrFor == nil {
		auth.sendErrFor = make(map[account.AccountID]error)
	}
	auth.sendErrFor[user1] = errors.New("disconnected")
	auth.sendErrForMtx.Unlock()

	epoch := &readyEpoch{
		EpochQueue: NewEpoch(100, 500),
	}
	mkt.sendMMSnapshots(epoch.Epoch, epoch.Duration)

	// user1 should have been auto-unsubscribed.
	mkt.mmSnapshotMtx.RLock()
	_, user1Found := mkt.mmSnapshotSubs[user1]
	_, user2Found := mkt.mmSnapshotSubs[user2]
	mkt.mmSnapshotMtx.RUnlock()
	if user1Found {
		t.Fatal("user1 should have been auto-unsubscribed after Send failure")
	}
	if !user2Found {
		t.Fatal("user2 should still be subscribed")
	}

	// user2 should have received a message.
	auth.sendsMtx.Lock()
	nSends := len(auth.sends)
	auth.sendsMtx.Unlock()
	if nSends != 1 {
		t.Fatalf("expected 1 send (user2 only), got %d", nSends)
	}
}

func TestSendMMSnapshotsBothSides(t *testing.T) {
	mkt, _, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	// Single user with orders on both sides of the book.
	user := buyer3.Acct
	mkt.SubscribeMMSnapshots(user, false)

	buyRate := uint64(2_500_000_000)
	sellRate := uint64(2_700_000_000)

	loBuy := makeLO(buyer3, buyRate, 1, order.StandingTiF)
	loBuy.AccountID = user
	loSell := makeLO(seller3, sellRate, 1, order.StandingTiF)
	loSell.AccountID = user

	mkt.bookMtx.Lock()
	if !mkt.book.Insert(loBuy) {
		t.Fatal("failed to insert buy order")
	}
	if !mkt.book.Insert(loSell) {
		t.Fatal("failed to insert sell order")
	}
	mkt.bookMtx.Unlock()

	auth.sendsMtx.Lock()
	auth.sends = auth.sends[:0]
	auth.sendsMtx.Unlock()

	epoch := &readyEpoch{
		EpochQueue: NewEpoch(100, 500),
	}
	mkt.sendMMSnapshots(epoch.Epoch, epoch.Duration)

	auth.sendsMtx.Lock()
	sends := make([]*msgjson.Message, len(auth.sends))
	copy(sends, auth.sends)
	auth.sendsMtx.Unlock()

	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sends))
	}

	var snap msgjson.MMEpochSnapshot
	if err := sends[0].Unmarshal(&snap); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(snap.BuyOrders) != 1 {
		t.Fatalf("expected 1 buy order, got %d", len(snap.BuyOrders))
	}
	if len(snap.SellOrders) != 1 {
		t.Fatalf("expected 1 sell order, got %d", len(snap.SellOrders))
	}
	if snap.BuyOrders[0].Rate != buyRate {
		t.Fatalf("expected buy rate %d, got %d", buyRate, snap.BuyOrders[0].Rate)
	}
	if snap.SellOrders[0].Rate != sellRate {
		t.Fatalf("expected sell rate %d, got %d", sellRate, snap.SellOrders[0].Rate)
	}
}

func TestMarket_lockOrderCoins(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	coin1 := order.CoinID([]byte{0x01, 0x02, 0x03})
	coin2 := order.CoinID([]byte{0x04, 0x05, 0x06})

	// Sell order uses base asset locker.
	sellOrder := makeLO(seller3, mkRate3(0.8, 1.0), 1, order.StandingTiF)
	sellOrder.Coins = []order.CoinID{coin1}

	// Buy order uses quote asset locker.
	buyOrder := makeLO(buyer3, mkRate3(0.8, 1.0), 1, order.StandingTiF)
	buyOrder.Coins = []order.CoinID{coin2}

	// Locking fresh coins should succeed.
	if !mkt.lockOrderCoins(sellOrder) {
		t.Fatal("lockOrderCoins failed for sell order with unlocked coins")
	}
	if !mkt.lockOrderCoins(buyOrder) {
		t.Fatal("lockOrderCoins failed for buy order with unlocked coins")
	}

	// Unlock so we can reuse the coins.
	mkt.unlockOrderCoins(sellOrder)
	mkt.unlockOrderCoins(buyOrder)

	// Pre-lock coin1 in the base book locker under a different order.
	mkt.coinLockerBase.LockCoins(map[order.OrderID][]order.CoinID{
		randomOrderID(): {coin1},
	})

	// Pre-lock coin2 in the quote book locker under a different order.
	mkt.coinLockerQuote.LockCoins(map[order.OrderID][]order.CoinID{
		randomOrderID(): {coin2},
	})

	// lockOrderCoins should now fail for both since their coins are
	// already locked.
	if mkt.lockOrderCoins(sellOrder) {
		t.Fatal("lockOrderCoins should have failed for sell order with locked coins")
	}
	if mkt.lockOrderCoins(buyOrder) {
		t.Fatal("lockOrderCoins should have failed for buy order with locked coins")
	}

	// Cancel orders always succeed regardless of coin lock state.
	co := makeCO(seller3, randomOrderID())
	if !mkt.lockOrderCoins(co) {
		t.Fatal("lockOrderCoins should always succeed for cancel orders")
	}
}

func TestMarketOrderAcceptedCommand(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	// AcceptOrderCommand only accepts new orders once the market is running.
	mkt.running.Store(true)

	const requestID = uint64(42)
	lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	user := lo.User()
	commit := lo.Commitment()
	rec := &orderRecord{
		order: lo,
		req: &msgjson.LimitOrder{
			Prefix: msgjson.Prefix{
				AccountID:  user[:],
				Base:       lo.Base(),
				Quote:      lo.Quote(),
				OrderType:  msgjson.LimitOrderNum,
				ClientTime: uint64(lo.ClientTime.UnixMilli()),
				Commit:     commit[:],
			},
		},
		msgID: requestID,
	}

	orderFromAcceptedEvent := func(t *testing.T, event *mesh.Event) order.Order {
		t.Helper()
		if event.Kind != meshevents.EventKindOrderAccepted {
			t.Fatalf("wrong event kind. got %q, want %q", event.Kind, meshevents.EventKindOrderAccepted)
		}
		accepted, err := meshevents.DecodeOrderAcceptedEvent(event.Payload)
		if err != nil {
			t.Fatalf("DecodeOrderAcceptedEvent error: %v", err)
		}
		ord, err := accepted.Order()
		if err != nil {
			t.Fatalf("accepted order error: %v", err)
		}
		return ord
	}

	var events []*mesh.Event
	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Commands: map[string]mesh.CommandExecutor{
			commandKindLimit: func(cmd *mesh.CommandContext) *msgjson.Error {
				return mkt.AcceptOrderCommand(cmd.Context, rec, cmd.Completion)
			},
		},
		Events: map[string]mesh.EventApplier{
			meshevents.EventKindOrderAccepted: func(_ *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
				cpy := cloneTestMeshEvent(event)
				events = append(events, cpy)
				if len(events) == 1 {
					time.Sleep(2 * time.Millisecond)
					return nil, ErrEpochMissed
				}
				return &db.EventLogEntry{
					Seq:     uint64(len(events)),
					Kind:    cpy.Kind,
					Event:   append([]byte(nil), cpy.Payload...),
					TipHash: []byte{byte(len(events))},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}

	msg, err := msgjson.NewRequest(requestID, msgjson.LimitRoute, nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	var response *msgjson.Message
	rpcErr := meshSvc.ExecuteCommand(context.Background(), mesh.CommandRequest{
		Kind: commandKindLimit,
		User: user,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			if response != nil {
				t.Fatalf("multiple command responses")
			}
			response = resp
			return nil
		},
	})
	if rpcErr != nil {
		t.Fatalf("ExecuteCommand error: %v", rpcErr)
	}
	if response == nil {
		t.Fatalf("missing command response")
	}

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	firstOrder := orderFromAcceptedEvent(t, events[0])
	secondOrder := orderFromAcceptedEvent(t, events[1])
	if firstOrder.Time() == secondOrder.Time() {
		t.Fatalf("retry did not restamp order time")
	}
	if firstOrder.ID() == secondOrder.ID() {
		t.Fatalf("retry did not create a new order ID")
	}

	var result msgjson.OrderResult
	if err := response.UnmarshalResult(&result); err != nil {
		t.Fatalf("response result: %v", err)
	}
	resultOrderID, err := order.IDFromBytes(result.OrderID)
	if err != nil {
		t.Fatalf("response order id: %v", err)
	}
	if resultOrderID != secondOrder.ID() {
		t.Fatalf("response order id = %v, want retried order id %v", resultOrderID, secondOrder.ID())
	}
}

func seedPendingResumeState(mkt *Market) {
	mkt.epochMtx.Lock()
	mkt.lifecycleState = db.MarketStateSuspended
	mkt.pendingLifecycleAction = db.MarketPendingResume
	mkt.pendingLifecycleEpochIdx = 123
	mkt.pendingLifecycleEpochDur = int64(mkt.EpochDuration())
	mkt.persistBook = true
	mkt.persistBookSet = true
	mkt.epochMtx.Unlock()
}

func suspendedCancelRecord(t *testing.T, mkt *Market, storage *TArchivist, msgID uint64) (*order.LimitOrder, order.CoinID, *orderRecord) {
	t.Helper()
	coin := order.CoinID([]byte{0x61, 0x62, 0x63})
	target := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	target.Coins = []order.CoinID{coin}
	mkt.bookMtx.Lock()
	inserted := mkt.book.Insert(target)
	mkt.bookMtx.Unlock()
	if !inserted {
		t.Fatalf("failed to insert target order")
	}
	if !mkt.lockOrderCoins(target) {
		t.Fatalf("failed to lock target order coins")
	}
	if err := storage.BookOrder(target); err != nil {
		t.Fatalf("BookOrder error: %v", err)
	}

	targetID := target.ID()
	aid := target.User()
	cancelTime := time.Now().UnixMilli()
	pi := test.RandomPreimage()
	commit := pi.Commit()
	cancelMsg := &msgjson.CancelOrder{
		Prefix: msgjson.Prefix{
			AccountID:  aid[:],
			Base:       target.Base(),
			Quote:      target.Quote(),
			OrderType:  msgjson.CancelOrderNum,
			ClientTime: uint64(cancelTime),
			Commit:     commit[:],
		},
		TargetID: targetID[:],
	}
	co := &order.CancelOrder{
		P: order.Prefix{
			AccountID:  aid,
			BaseAsset:  target.Base(),
			QuoteAsset: target.Quote(),
			OrderType:  order.CancelOrderType,
			ClientTime: time.UnixMilli(cancelTime),
			Commit:     commit,
		},
		TargetOrderID: targetID,
	}
	rec := &orderRecord{
		order: co,
		req:   cancelMsg,
		msgID: msgID,
	}
	return target, coin, rec
}

func TestMarketAcceptOrderCommandSuspendedCancelPendingResume(t *testing.T) {
	mkt, storage, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	seedPendingResumeState(mkt)

	target, coin, rec := suspendedCancelRecord(t, mkt, storage, 42)
	targetID := target.ID()

	if err := submitOrderCommand(t, mkt, auth, rec); err != nil {
		t.Fatalf("submit suspended cancel while pending resume: %v", err)
	}
	if len(storage.suspendedCancels) != 1 {
		t.Fatalf("suspended cancel updates = %d, want 1", len(storage.suspendedCancels))
	}
	if storage.suspendedCancels[0].TargetOrderID != targetID {
		t.Fatalf("suspended cancel target = %v, want %v", storage.suspendedCancels[0].TargetOrderID, targetID)
	}
	if mkt.book.HaveOrder(targetID) {
		t.Fatalf("target order remains booked")
	}
	if mkt.CoinLocked(mkt.Base(), coin) {
		t.Fatalf("target order coin remains locked")
	}
	auth.sendsMtx.Lock()
	sends := len(auth.sends)
	auth.sendsMtx.Unlock()
	if sends != 1 {
		t.Fatalf("order responses = %d, want 1", sends)
	}
}

func TestMarketAcceptOrderCommandSuspendedCancelBlocksDuringResumePreparation(t *testing.T) {
	mkt, storage, auth, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	seedPendingResumeState(mkt)

	_, _, rec := suspendedCancelRecord(t, mkt, storage, 43)

	mkt.resumeSubmitMtx.Lock()
	errCh := make(chan error, 1)
	go func() {
		errCh <- submitOrderCommand(t, mkt, auth, rec)
	}()

	select {
	case err := <-errCh:
		t.Fatalf("suspended cancel completed during resume preparation with err = %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	storage.mtx.Lock()
	n := len(storage.suspendedCancels)
	storage.mtx.Unlock()
	if n != 0 {
		t.Fatalf("suspended cancel updates during resume preparation = %d, want 0", n)
	}

	mkt.resumeSubmitMtx.Unlock()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("submit suspended cancel after resume preparation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("suspended cancel did not complete after resume preparation")
	}
	if len(storage.suspendedCancels) != 1 {
		t.Fatalf("suspended cancel updates = %d, want 1", len(storage.suspendedCancels))
	}
}

func TestApplyOrderAcceptedEvent(t *testing.T) {
	type applyCase struct {
		name           string
		ord            order.Order
		epochGap       int32
		lockedCoin     order.CoinID
		wantCancelable bool
		wantOrderType  uint8
	}

	coinAssetID := func(mkt *Market, ord order.Order) uint32 {
		if ord.Trade().Sell {
			return mkt.Base()
		}
		return mkt.Quote()
	}

	requireOrderApplied := func(t *testing.T, mkt *Market, ord order.Order, lockedCoin order.CoinID, wantCancelable bool) {
		t.Helper()
		oid := ord.ID()
		if got := mkt.epochOrders[oid]; got == nil || got.ID() != oid {
			t.Fatalf("epochOrders entry mismatch. got %v, want %v", got, oid)
		}
		if got := mkt.epochCommitments[ord.Commitment()]; got != oid {
			t.Fatalf("epochCommitments entry mismatch. got %v, want %v", got, oid)
		}
		if lockedCoin != nil && !mkt.CoinLocked(coinAssetID(mkt, ord), lockedCoin) {
			t.Fatalf("accepted order coin was not locked")
		}
		if got := mkt.Cancelable(oid); got != wantCancelable {
			t.Fatalf("cancelable = %t, want %t", got, wantCancelable)
		}
		if mkt.book.HaveOrder(oid) {
			t.Fatalf("accepted order %v should not be in the booked order book", oid)
		}
	}

	requireOrderAcceptedUpdate := func(t *testing.T, update *db.OrderAcceptedUpdate, tt applyCase, epochIdx, epochDur int64) {
		t.Helper()
		oid := tt.ord.ID()
		if update.Order == nil || update.Order.ID() != oid {
			t.Fatalf("order accepted update order mismatch. got %v, want %v", update.Order, oid)
		}
		if update.EpochIdx != epochIdx {
			t.Fatalf("order accepted update epoch idx = %d, want %d", update.EpochIdx, epochIdx)
		}
		if update.EpochDur != epochDur {
			t.Fatalf("order accepted update epoch dur = %d, want %d", update.EpochDur, epochDur)
		}
		if update.EpochGap != tt.epochGap {
			t.Fatalf("order accepted update epoch gap = %d, want %d", update.EpochGap, tt.epochGap)
		}
	}

	requireOrderRejected := func(t *testing.T, mkt *Market, storage *TArchivist, ord order.Order, wantStorageWrites int) {
		t.Helper()
		if got := len(storage.orderAcceptedUpdates); got != wantStorageWrites {
			t.Fatalf("order accepted updates = %d, want %d", got, wantStorageWrites)
		}
		mkt.epochMtx.RLock()
		_, inserted := mkt.epochOrders[ord.ID()]
		mkt.epochMtx.RUnlock()
		if inserted {
			t.Fatalf("rejected order %v was inserted into epoch memory", ord.ID())
		}
	}

	t.Run("rejects order with no active epoch", func(t *testing.T) {
		// Startup seeding rebuilds epoch memory for every running market
		// before any event applies, so an order_accepted reaching a market
		// with no current epoch is an invariant violation and must fail
		// without any durable or memory effect.
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		link := rig.subscribeBook(t)
		mkt, storage := rig.mkt, rig.storage
		epochDur := int64(mkt.EpochDuration())
		epochIdx := int64(1234)
		lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		lo.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
		lo.Coins = []order.CoinID{[]byte{0x01, 0x02, 0x03}}

		err := rig.applyErr(t, newOrderAcceptedEvent(lo))
		if err == nil || !strings.Contains(err.Error(), "no active epoch") {
			t.Fatalf("apply error = %v, want no active epoch", err)
		}
		requireOrderRejected(t, mkt, storage, lo, 0)
		requireNoBookNoteFromLink(t, link)
	})

	t.Run("applies order types", func(t *testing.T) {
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		link := rig.subscribeBook(t)
		mkt, storage := rig.mkt, rig.storage
		limitCoin := order.CoinID([]byte{0x01, 0x02, 0x03})
		marketCoin := order.CoinID([]byte{0x41, 0x42, 0x43})
		epochDur := int64(mkt.EpochDuration())
		epochIdx := int64(1234)
		rig.submitMarketStarted(t, epochIdx)
		lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		lo.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
		lo.Coins = []order.CoinID{limitCoin}
		mo := makeMO(seller3, 1)
		mo.SetTime(time.UnixMilli(epochIdx*epochDur + 2))
		mo.Coins = []order.CoinID{marketCoin}
		co := makeCO(seller3, lo.ID())
		co.SetTime(time.UnixMilli(epochIdx*epochDur + 3))

		tests := []applyCase{
			{
				name:           "limit",
				ord:            lo,
				epochGap:       db.EpochGapNA,
				lockedCoin:     limitCoin,
				wantCancelable: true,
				wantOrderType:  msgjson.LimitOrderNum,
			},
			{
				name:          "market",
				ord:           mo,
				epochGap:      db.EpochGapNA,
				lockedCoin:    marketCoin,
				wantOrderType: msgjson.MarketOrderNum,
			},
			{
				name:          "cancel",
				ord:           co,
				epochGap:      0,
				wantOrderType: msgjson.CancelOrderNum,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rig.apply(t, newOrderAcceptedEvent(tt.ord))
				requireOrderApplied(t, mkt, tt.ord, tt.lockedCoin, tt.wantCancelable)
				requireEpochNoteFromLink(t, link, mkt, tt.ord, epochIdx, tt.wantOrderType)
			})
		}

		if len(storage.orderAcceptedUpdates) != len(tests) {
			t.Fatalf("order accepted updates = %d, want %d", len(storage.orderAcceptedUpdates), len(tests))
		}
		for i, tt := range tests {
			t.Run("stored "+tt.name, func(t *testing.T) {
				requireOrderAcceptedUpdate(t, storage.orderAcceptedUpdates[i], tt, epochIdx, epochDur)
			})
		}
	})

	t.Run("duplicate cancel event appends storage without reprojecting memory", func(t *testing.T) {
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		link := rig.subscribeBook(t)
		mkt, storage := rig.mkt, rig.storage
		epochDur := int64(mkt.EpochDuration())
		epochIdx := int64(1234)
		rig.submitMarketStarted(t, epochIdx)
		lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		lo.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
		co := makeCO(seller3, lo.ID())
		co.SetTime(time.UnixMilli(epochIdx*epochDur + 2))

		rig.apply(t, newOrderAcceptedEvent(lo))
		requireEpochNoteFromLink(t, link, mkt, lo, epochIdx, msgjson.LimitOrderNum)
		rig.apply(t, newOrderAcceptedEvent(co))
		requireEpochNoteFromLink(t, link, mkt, co, epochIdx, msgjson.CancelOrderNum)

		rig.apply(t, newOrderAcceptedEvent(co))
		if got, want := len(storage.orderAcceptedUpdates), 3; got != want {
			t.Fatalf("storage writes = %d, want %d", got, want)
		}
		if got := storage.orderAcceptedUpdates[len(storage.orderAcceptedUpdates)-1].EpochGap; got != 0 {
			t.Fatalf("duplicate cancel stored epoch gap = %d, want 0", got)
		}
		requireNoBookNoteFromLink(t, link)
	})

	t.Run("locked coin rejected before db apply", func(t *testing.T) {
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		mkt, storage := rig.mkt, rig.storage
		epochDur := int64(mkt.EpochDuration())
		epochIdx := int64(1234)
		rig.submitMarketStarted(t, epochIdx)
		coin := order.CoinID([]byte{0x0a, 0x0b, 0x0c})
		lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		lo.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
		lo.Coins = []order.CoinID{coin}
		if !mkt.lockOrderCoins(lo) {
			t.Fatalf("test setup failed to lock order coin")
		}

		err := rig.applyErr(t, newOrderAcceptedEvent(lo))
		if err == nil || !strings.Contains(err.Error(), "already-locked") {
			t.Fatalf("apply error = %v, want already-locked coin error", err)
		}
		requireOrderRejected(t, mkt, storage, lo, 0)
	})

	t.Run("rejects parcel limit before mutation", func(t *testing.T) {
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		mkt, storage := rig.mkt, rig.storage
		mkt.checkParcelLimit = func(_ account.AccountID, _ time.Time, calcParcels MarketParcelCalculator) (bool, error) {
			return calcParcels(0) <= 1, nil
		}

		epochDur := int64(mkt.EpochDuration())
		epochIdx := int64(2345)
		rig.submitMarketStarted(t, epochIdx)
		first := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		first.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
		first.Coins = []order.CoinID{[]byte{0x01, 0x11, 0x21}}
		second := makeLO(seller3, mkRate3(1.1, 1.3), 1, order.StandingTiF)
		second.SetTime(time.UnixMilli(epochIdx*epochDur + 2))
		secondCoin := order.CoinID([]byte{0x02, 0x12, 0x22})
		second.Coins = []order.CoinID{secondCoin}

		rig.apply(t, newOrderAcceptedEvent(first))

		if err := rig.applyErr(t, newOrderAcceptedEvent(second)); !errors.Is(err, ErrQuantityTooHigh) {
			t.Fatalf("second apply error = %v, want %v", err, ErrQuantityTooHigh)
		}
		requireOrderRejected(t, mkt, storage, second, 1)
		if mkt.CoinLocked(mkt.Base(), secondCoin) {
			t.Fatalf("rejected order coin was locked")
		}
	})
}

func TestApplyMarketStartedEvent(t *testing.T) {
	rig := newMarketEventRig(t)
	defer rig.cleanup()
	mkt, storage, auth := rig.mkt, rig.storage, rig.auth
	mktName := mkt.name
	epochDur := int64(mkt.EpochDuration())
	const seededEpochIdx int64 = 42
	startedEpochIdx := seededEpochIdx + 100 // the restart jumps the epoch

	// Warm state: a queued trade and cancel seeded from storage, plus a
	// booked order in the market and router books.
	epochCoin := order.CoinID{0xa4, 0xb5, 0xc6}
	epochLO := epochStampedLO(t, seededEpochIdx, epochDur, 1, epochCoin)
	epochCO := epochStampedCO(t, epochLO.ID(), seededEpochIdx, epochDur, 2)
	seedEpochOrder(storage, epochLO, seededEpochIdx, epochDur)
	seedEpochOrder(storage, epochCO, seededEpochIdx, epochDur)
	seedRunningLifecycle(t, mkt, seededEpochIdx, epochDur)
	mkt.epochMtx.RLock()
	oldCurrent, oldNext := mkt.currentEpoch, mkt.nextEpoch
	mkt.epochMtx.RUnlock()

	bookedCoin := order.CoinID{0xa1, 0xb2, 0xc3}
	booked := makeLO(seller3, mkRate3(1.0, 1.2), 2, order.StandingTiF)
	booked.Coins = []order.CoinID{bookedCoin}
	bookStandingOrder(t, rig, booked)
	book := rig.bookRouter.books[mktName]
	book.mtx.Lock()
	book.running = true
	book.mtx.Unlock()
	link := rig.subscribeBook(t)

	revocationTime := time.UnixMilli(123456789).UTC()
	startedEvent := meshevents.NewMarketStartedEvent(mktName, startedEpochIdx, epochDur,
		mkt.configuredParams.MarketRunParams, revocationTime,
		[]meshevents.StartupOrderRevokeRecord{
			meshevents.NewStartupOrderRevokeRecord(booked, meshevents.StartupOrderRevokeReasonLotSizeIncompatible),
		})
	startedEvent.EpochRevokes = []meshevents.StartupOrderRevokeRecord{
		meshevents.NewStartupOrderRevokeRecord(epochLO, meshevents.StartupOrderRevokeReasonEpochAbandoned),
		meshevents.NewStartupOrderRevokeRecord(epochCO, meshevents.StartupOrderRevokeReasonEpochAbandoned),
	}
	rig.apply(t, encoderBuilder{startedEvent})

	// The stored update mirrors the event.
	if len(storage.marketStartedUpdates) != 1 {
		t.Fatalf("market started updates = %d, want 1", len(storage.marketStartedUpdates))
	}
	update := storage.marketStartedUpdates[0]
	if update.Market != mktName || update.RevocationTime != revocationTime {
		t.Fatalf("update market/time = %q/%v, want %q/%v",
			update.Market, update.RevocationTime, mktName, revocationTime)
	}
	if update.CurrentEpochIdx != startedEpochIdx || update.EpochDur != epochDur {
		t.Fatalf("market started epoch = %d:%d, want %d:%d",
			update.CurrentEpochIdx, update.EpochDur, startedEpochIdx, epochDur)
	}
	if len(update.BookedRevokes) != 1 || update.BookedRevokes[0].Order.ID() != booked.ID() ||
		update.BookedRevokes[0].Reason != meshevents.StartupOrderRevokeReasonLotSizeIncompatible {
		t.Fatalf("booked revokes mismatch: %+v", update.BookedRevokes)
	}

	// Every revoked order left the book with its coins unlocked. Storage
	// epochOrders is a seed for EpochOrders() reads, not a projected table.
	requireRevokedOrderGone(t, mkt, booked)
	requireRevokedOrderGone(t, mkt, epochLO)

	// Epoch memory replaced wholesale: fresh empty queues at the started
	// epoch, old pointers not reused, indexes emptied, cursors moved.
	mkt.epochMtx.RLock()
	if mkt.currentEpoch == nil || mkt.currentEpoch.Epoch != startedEpochIdx ||
		mkt.currentEpoch.Duration != epochDur || len(mkt.currentEpoch.Orders) != 0 {
		t.Fatalf("current epoch = %v, want empty %d:%d", mkt.currentEpoch, startedEpochIdx, epochDur)
	}
	if mkt.nextEpoch == nil || mkt.nextEpoch.Epoch != startedEpochIdx+1 ||
		mkt.nextEpoch.Duration != epochDur || len(mkt.nextEpoch.Orders) != 0 {
		t.Fatalf("next epoch = %v, want empty %d:%d", mkt.nextEpoch, startedEpochIdx+1, epochDur)
	}
	if mkt.currentEpoch == oldCurrent || mkt.nextEpoch == oldNext {
		t.Fatalf("market_started reused seeded epoch queues")
	}
	if len(mkt.epochOrders) != 0 || len(mkt.epochCommitments) != 0 {
		t.Fatalf("queued orders survived market_started: orders=%d commitments=%d",
			len(mkt.epochOrders), len(mkt.epochCommitments))
	}
	if mkt.startEpochIdx != startedEpochIdx || mkt.activeEpochIdx != startedEpochIdx {
		t.Fatalf("epoch cursors = %d/%d, want %d", mkt.startEpochIdx, mkt.activeEpochIdx, startedEpochIdx)
	}
	mkt.epochMtx.RUnlock()
	if bookEpoch, _, _ := mkt.Book(); bookEpoch != startedEpochIdx {
		t.Fatalf("market book epoch = %d, want %d", bookEpoch, startedEpochIdx)
	}
	if routerEpoch := book.epoch(); routerEpoch != startedEpochIdx {
		t.Fatalf("router book epoch = %d, want %d", routerEpoch, startedEpochIdx)
	}

	// Notes: an unbook for the booked order; revoke notes for the booked
	// order and the epoch trade; a nomatch for the epoch cancel; no penalty.
	unbookMsg := link.getSend()
	if unbookMsg == nil || unbookMsg.Route != msgjson.UnbookOrderRoute {
		t.Fatalf("unbook route = %v, want %q", unbookMsg, msgjson.UnbookOrderRoute)
	}
	var unbookNote msgjson.UnbookOrderNote
	if err := json.Unmarshal(unbookMsg.Payload, &unbookNote); err != nil {
		t.Fatalf("unbook note: %v", err)
	}
	bookedID := booked.ID()
	if !bytes.Equal(unbookNote.OrderID, bookedID[:]) {
		t.Fatalf("unbook order id = %x, want %x", unbookNote.OrderID, bookedID)
	}
	revokeIDs, nomatchIDs := collectOwnerNotes(t, auth)
	if len(revokeIDs) != 2 || !revokeIDs[booked.ID()] || !revokeIDs[epochLO.ID()] {
		t.Fatalf("revoke notes = %v, want %v and %v", revokeIDs, booked.ID(), epochLO.ID())
	}
	if len(nomatchIDs) != 1 || !nomatchIDs[epochCO.ID()] {
		t.Fatalf("nomatch notes = %v, want %v", nomatchIDs, epochCO.ID())
	}

	t.Run("pending suspend start keeps the suspend and opens the final epoch", func(t *testing.T) {
		rig := newMarketEventRig(t)
		defer rig.cleanup()
		mkt, storage := rig.mkt, rig.storage
		epochDur := int64(mkt.EpochDuration())
		finalEpochIdx := int64(42)
		row := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingSuspend, finalEpochIdx, epochDur)
		setTestMarketLifecycle(mkt, storage, row)
		lo := epochStampedLO(t, finalEpochIdx, epochDur, 1, order.CoinID{0xa7, 0xb8})
		seedEpochOrder(storage, lo, finalEpochIdx, epochDur)
		if err := mkt.seedEpochMemory(row); err != nil {
			t.Fatalf("seedEpochMemory: %v", err)
		}
		startedEvent := meshevents.NewMarketStartedEvent(mkt.name, finalEpochIdx, epochDur,
			mkt.configuredParams.MarketRunParams, time.UnixMilli(123456789).UTC(), nil)
		startedEvent.EpochRevokes = []meshevents.StartupOrderRevokeRecord{
			meshevents.NewStartupOrderRevokeRecord(lo, meshevents.StartupOrderRevokeReasonEpochAbandoned),
		}
		rig.apply(t, encoderBuilder{startedEvent})
		mkt.epochMtx.RLock()
		defer mkt.epochMtx.RUnlock()
		if mkt.pendingLifecycleAction != db.MarketPendingSuspend {
			t.Fatalf("pending action = %v, want %v", mkt.pendingLifecycleAction, db.MarketPendingSuspend)
		}
		if mkt.currentEpoch == nil || mkt.currentEpoch.Epoch != finalEpochIdx || len(mkt.currentEpoch.Orders) != 0 {
			t.Fatalf("current epoch = %v, want empty %d", mkt.currentEpoch, finalEpochIdx)
		}
	})

}

// TestSubmitMarketResumeRefusesDurationChange pins the resume freeze: the
// scheduled resume epoch is expressed in the old duration unit, so a master
// whose configured duration changed must refuse to resume; the duration can
// change only at the next fresh market start.
func TestSubmitMarketResumeRefusesDurationChange(t *testing.T) {
	rig := newMarketEventRig(t)
	defer rig.cleanup()
	mkt, storage := rig.mkt, rig.storage

	oldDur := mkt.configuredParams.epochDur * 2 // scheduled under different config
	pendingIdx := int64(500)
	persist := true
	setTestMarketLifecycle(mkt, storage, &db.MarketLifecycle{
		Market:          mkt.name,
		State:           db.MarketStateSuspended,
		StartEpochIdx:   pendingIdx,
		StartEpochDur:   oldDur,
		PendingAction:   db.MarketPendingResume,
		PendingEpochIdx: pendingIdx,
		PendingEpochDur: oldDur,
		PersistBook:     &persist,
		RunParams:       mkt.configuredParams.MarketRunParams,
	})

	err := mkt.submitMarketResume(context.Background(), pendingIdx, oldDur)
	if err == nil || !strings.Contains(err.Error(), "revert the configured duration") {
		t.Fatalf("resume across duration change error = %v, want refusal", err)
	}
}

func TestApplyMarketResumeEvent(t *testing.T) {
	newPendingResumeRig := func(t *testing.T) (*marketEventRig, int64) {
		t.Helper()
		rig := newMarketEventRig(t)
		t.Cleanup(rig.cleanup)
		mkt := rig.mkt
		epochDur := int64(mkt.EpochDuration())
		startEpochIdx := int64(88)
		persist := true
		setTestMarketLifecycle(mkt, rig.storage, &db.MarketLifecycle{
			Market:          mkt.name,
			State:           db.MarketStateSuspended,
			StartEpochIdx:   startEpochIdx,
			StartEpochDur:   epochDur,
			PendingAction:   db.MarketPendingResume,
			PendingEpochIdx: startEpochIdx,
			PendingEpochDur: epochDur,
			PersistBook:     &persist,
			RunParams:       mkt.configuredParams.MarketRunParams,
		})
		return rig, startEpochIdx
	}

	t.Run("unbooks listed revokes", func(t *testing.T) {
		rig, startEpochIdx := newPendingResumeRig(t)
		mkt := rig.mkt
		lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		lo.Coins = []order.CoinID{[]byte{0xd1, 0xe2, 0xf3}}
		bookStandingOrder(t, rig, lo)
		book := rig.bookRouter.books[mkt.name]
		book.mtx.Lock()
		book.running = true
		book.mtx.Unlock()
		link := rig.subscribeBook(t)

		event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionResume, mkt.name, startEpochIdx, int64(mkt.EpochDuration()))
		resumeParams := mkt.configuredParams.MarketRunParams
		event.RunParams = &resumeParams
		event.Timestamp = time.UnixMilli(123456789).UTC().UnixMilli()
		event.ResumeRevokes = []meshevents.StartupOrderRevokeRecord{
			meshevents.NewStartupOrderRevokeRecord(lo, meshevents.StartupOrderRevokeReasonFundingCoinSpent),
		}
		rig.apply(t, encoderBuilder{event})

		unbookMsg := link.getSend()
		if unbookMsg == nil || unbookMsg.Route != msgjson.UnbookOrderRoute {
			t.Fatalf("first route = %v, want %q", unbookMsg, msgjson.UnbookOrderRoute)
		}
		var unbookNote msgjson.UnbookOrderNote
		if err := json.Unmarshal(unbookMsg.Payload, &unbookNote); err != nil {
			t.Fatalf("unbook note: %v", err)
		}
		oid := lo.ID()
		if !bytes.Equal(unbookNote.OrderID, oid[:]) {
			t.Fatalf("unbook order id = %x, want %x", unbookNote.OrderID, oid)
		}
		resumeMsg := link.getSend()
		if resumeMsg == nil || resumeMsg.Route != msgjson.ResumptionRoute {
			t.Fatalf("second route = %v, want %q", resumeMsg, msgjson.ResumptionRoute)
		}
		requireRevokedOrderGone(t, mkt, lo)
		book.mtx.RLock()
		_, inMsgBook := book.orders[lo.ID()]
		book.mtx.RUnlock()
		if inMsgBook {
			t.Fatalf("resume-revoked order remains in msgBook")
		}
	})

	t.Run("lot size change requires revoke coverage", func(t *testing.T) {
		rig, startEpochIdx := newPendingResumeRig(t)
		mkt := rig.mkt
		oldParams := mkt.configuredParams.MarketRunParams
		stranded := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
		stranded.Coins = []order.CoinID{[]byte{0x81, 0x01}}
		bookStandingOrder(t, rig, stranded)
		kept := makeLO(seller3, mkRate3(1.1, 1.3), 2, order.StandingTiF)
		kept.Coins = []order.CoinID{[]byte{0x81, 0x02}}
		bookStandingOrder(t, rig, kept)

		newParams := oldParams
		newParams.LotSize = oldParams.LotSize * 2
		event := meshevents.NewMarketLifecycleEvent(meshevents.LifecycleActionResume, mkt.name, startEpochIdx, int64(mkt.EpochDuration()))
		event.Timestamp = time.UnixMilli(123456789).UTC().UnixMilli()
		event.RunParams = &newParams
		if err := rig.applyErr(t, encoderBuilder{event}); err == nil || !strings.Contains(err.Error(), "incompatible with lot size") {
			t.Fatalf("uncovered resume apply error = %v, want lot-size coverage rejection", err)
		}
		if mkt.LotSize() != oldParams.LotSize {
			t.Fatalf("rejected resume changed the adopted lot size")
		}

		event.ResumeRevokes = []meshevents.StartupOrderRevokeRecord{
			meshevents.NewStartupOrderRevokeRecord(stranded, meshevents.StartupOrderRevokeReasonLotSizeIncompatible),
		}
		rig.apply(t, encoderBuilder{event})
		if mkt.LotSize() != newParams.LotSize {
			t.Fatalf("adopted lot size = %d, want %d", mkt.LotSize(), newParams.LotSize)
		}
		if mkt.book.HaveOrder(stranded.ID()) {
			t.Fatalf("stranded order remained booked")
		}
		if mkt.book.Order(kept.ID()) == nil {
			t.Fatalf("compatible order left the book")
		}
	})
}

func TestApplyOrdersRevokedEvent(t *testing.T) {
	rig := newMarketEventRig(t)
	defer rig.cleanup()

	mkt, storage, auth := rig.mkt, rig.storage, rig.auth
	mktName := mkt.name

	var unbooked []*order.LimitOrder
	mkt.SetUnbookNotifier(func(lo *order.LimitOrder) {
		unbooked = append(unbooked, lo)
	})

	coin1 := order.CoinID([]byte{0xa1, 0xb2, 0xc3})
	lo1 := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	lo1.Coins = []order.CoinID{coin1}
	coin2 := order.CoinID([]byte{0xd4, 0xe5, 0xf6})
	lo2 := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	lo2.Coins = []order.CoinID{coin2}
	// ghost is never booked, so the applier must skip it without any note.
	ghost := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)

	mkt.bookMtx.Lock()
	if !mkt.book.Insert(lo1) || !mkt.book.Insert(lo2) {
		mkt.bookMtx.Unlock()
		t.Fatalf("failed to insert booked orders")
	}
	mkt.bookMtx.Unlock()
	if !mkt.lockOrderCoins(lo1) || !mkt.lockOrderCoins(lo2) {
		t.Fatalf("failed to lock booked order coins")
	}

	revokeTime := time.UnixMilli(123456789).UTC()
	event := meshevents.NewOrdersRevokedForOrdersEvent(mktName, []order.OrderID{lo1.ID(), lo2.ID(), ghost.ID()},
		meshevents.OrderRevokeReasonFundingSpent, revokeTime)
	rig.apply(t, encoderBuilder{event})

	// The DB was told to revoke exactly the two booked orders, for the right
	// reason and at the right time.
	if len(storage.ordersRevokedUpdates) != 1 {
		t.Fatalf("orders revoked updates = %d, want 1", len(storage.ordersRevokedUpdates))
	}
	update := storage.ordersRevokedUpdates[0]
	if update.Reason != meshevents.OrderRevokeReasonFundingSpent {
		t.Fatalf("update reason = %v, want %v", update.Reason, meshevents.OrderRevokeReasonFundingSpent)
	}
	if !update.RevokeTime.Equal(revokeTime) {
		t.Fatalf("update revoke time = %v, want %v", update.RevokeTime, revokeTime)
	}
	if len(update.Orders) != 2 {
		t.Fatalf("update orders = %d, want 2", len(update.Orders))
	}
	gotOrders := map[order.OrderID]bool{}
	for _, ord := range update.Orders {
		gotOrders[ord.ID()] = true
	}
	if !gotOrders[lo1.ID()] || !gotOrders[lo2.ID()] {
		t.Fatalf("update orders = %v, want %v and %v", gotOrders, lo1.ID(), lo2.ID())
	}

	// The in-memory book and coin locks reflect the revocation.
	requireRevokedOrderGone(t, mkt, lo1)
	requireRevokedOrderGone(t, mkt, lo2)

	// Each owner got a revoke_order notification.
	revokedIDs := map[order.OrderID]bool{}
	for i := 0; i < 2; i++ {
		msg := auth.getSend()
		if msg == nil || msg.Route != msgjson.RevokeOrderRoute {
			t.Fatalf("revoke_order notification %d route = %v, want %q", i, msg, msgjson.RevokeOrderRoute)
		}
		var note msgjson.RevokeOrder
		if err := json.Unmarshal(msg.Payload, &note); err != nil {
			t.Fatalf("revoke_order note: %v", err)
		}
		var oid order.OrderID
		copy(oid[:], note.OrderID)
		revokedIDs[oid] = true
	}
	if !revokedIDs[lo1.ID()] || !revokedIDs[lo2.ID()] {
		t.Fatalf("revoke_order notifications = %v, want %v and %v", revokedIDs, lo1.ID(), lo2.ID())
	}

	// The unbook notifier fired for each revoked order.
	if len(unbooked) != 2 {
		t.Fatalf("unbook notifications = %d, want 2", len(unbooked))
	}
	unbookedIDs := map[order.OrderID]bool{}
	for _, lo := range unbooked {
		unbookedIDs[lo.ID()] = true
	}
	if !unbookedIDs[lo1.ID()] || !unbookedIDs[lo2.ID()] {
		t.Fatalf("unbook notifications = %v, want %v and %v", unbookedIDs, lo1.ID(), lo2.ID())
	}
}

func TestApplyAdvanceEpochEvent(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*marketEventRig, *advanceEpochEvent)
		wantErr bool
	}{
		{
			name: "applies event",
		},
		{
			name: "zero duration",
			mutate: func(_ *marketEventRig, event *advanceEpochEvent) {
				event.EpochDur = 0
			},
			wantErr: true,
		},
		{
			name: "opened epoch skips closed epoch",
			mutate: func(_ *marketEventRig, event *advanceEpochEvent) {
				event.OpenedEpochIdx = event.ClosedEpochIdx + 2
			},
			wantErr: true,
		},
		{
			name: "unknown market",
			mutate: func(_ *marketEventRig, event *advanceEpochEvent) {
				event.Market = "unknown_market"
			},
			wantErr: true,
		},
		{
			name: "missing book",
			mutate: func(rig *marketEventRig, event *advanceEpochEvent) {
				delete(rig.bookRouter.books, event.Market)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newMarketEventRig(t)
			defer rig.cleanup()
			mkt := rig.mkt
			epochDur := int64(mkt.EpochDuration())
			const closedEpochIdx int64 = 10
			openedEpochIdx := closedEpochIdx + 1
			closedCoin := order.CoinID([]byte{0x11, 0x12, 0x13})
			closedBCoin := order.CoinID([]byte{0x14, 0x15, 0x16})
			nextCoin := order.CoinID([]byte{0x21, 0x22, 0x23})
			closed := epochStampedLO(t, closedEpochIdx, epochDur, 1, closedCoin)
			closedB := epochStampedLO(t, closedEpochIdx, epochDur, 2, closedBCoin)
			next := epochStampedLO(t, openedEpochIdx, epochDur, 1, nextCoin)
			rig.storage.mtx.Lock()
			rig.storage.epochOrders = []epochOrderWrite{
				{ord: closed, epochIdx: closedEpochIdx, epochDur: epochDur},
				{ord: closedB, epochIdx: closedEpochIdx, epochDur: epochDur},
				{ord: next, epochIdx: openedEpochIdx, epochDur: epochDur},
			}
			rig.storage.mtx.Unlock()
			seedRunningLifecycle(t, mkt, closedEpochIdx, epochDur)

			book := rig.bookRouter.books[mkt.name]
			if book == nil {
				t.Fatalf("missing bookrouter book for market %q", mkt.name)
			}
			book.setEpoch(closedEpochIdx)

			// requireEpochState layers the router epoch and cancelability
			// checks onto the seeded-state contract: dequeued orders keep
			// their funding locks until their epoch is processed.
			requireEpochState := func(epochIdx int64, queued, dequeued []order.Order) {
				t.Helper()
				requireSeededState(t, mkt, epochIdx, queued, dequeued)
				if got := book.epoch(); got != epochIdx {
					t.Fatalf("bookrouter epoch = %d, want %d", got, epochIdx)
				}
				for _, ord := range queued {
					if !mkt.Cancelable(ord.ID()) {
						t.Fatalf("queued order %v should be cancelable", ord.ID())
					}
				}
				for _, ord := range dequeued {
					if mkt.Cancelable(ord.ID()) {
						t.Fatalf("dequeued order %v unexpectedly cancelable", ord.ID())
					}
				}
			}

			event := &advanceEpochEvent{
				Market:         mkt.name,
				ClosedEpochIdx: closedEpochIdx,
				OpenedEpochIdx: openedEpochIdx,
				EpochDur:       epochDur,
			}
			if tt.mutate != nil {
				tt.mutate(rig, event)
			}

			if tt.wantErr {
				if err := rig.applyErr(t, encoderBuilder{event}); err == nil {
					t.Fatalf("expected validation error")
				}
				requireEpochState(closedEpochIdx, []order.Order{closed, closedB, next}, nil)
				return
			}

			entry := rig.apply(t, encoderBuilder{event})
			if bytes.Contains(entry.Payload, []byte("closedOrderIDs")) {
				t.Fatalf("advance_epoch payload unexpectedly contains closedOrderIDs: %s", entry.Payload)
			}
			requireEpochState(openedEpochIdx, []order.Order{next}, []order.Order{closed, closedB})
			if got := len(rig.storage.advanceEpochUpdates); got != 1 {
				t.Fatalf("advance epoch updates = %d, want 1", got)
			}
			requireOrderIDs(t, rig.storage.advanceEpochUpdates[0].ClosedOrderIDs, closed.ID(), closedB.ID())
		})
	}
}

func TestScheduleSuspendEvent(t *testing.T) {
	const current int64 = 40
	rig := newMarketEventRig(t)
	defer rig.cleanup()
	mkt := rig.mkt
	epochDur := int64(mkt.EpochDuration())
	row := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, current, epochDur)
	setTestMarketLifecycle(mkt, rig.storage, row)

	applyScheduled := func(event *mesh.Event) error {
		_, err := rig.events[event.Kind](&mesh.EventApplyContext{Context: context.Background()}, event)
		return err
	}

	event, susp, err := mkt.ScheduleSuspendEvent(time.UnixMilli(1), true)
	if err != nil {
		t.Fatalf("ScheduleSuspendEvent: %v", err)
	}
	if susp.Idx != current+1 {
		t.Fatalf("suspend epoch = %d, want %d", susp.Idx, current+1)
	}
	if err := applyScheduled(event); err != nil {
		t.Fatalf("ASAP schedule_suspend rejected: %v", err)
	}

	rig.apply(t, encoderBuilder{&advanceEpochEvent{
		Market:         row.Market,
		ClosedEpochIdx: current,
		OpenedEpochIdx: current + 1,
		EpochDur:       epochDur,
	}})

	event, _, err = mkt.ScheduleSuspendEvent(time.UnixMilli(1), true)
	if err != nil {
		t.Fatalf("ScheduleSuspendEvent after advance: %v", err)
	}
	if err := applyScheduled(event); err == nil {
		t.Fatalf("re-schedule accepted while final epoch %d is closing", current+1)
	}
}

func TestLifecycleCommands(t *testing.T) {
	rig := newMarketEventRig(t)
	defer rig.cleanup()
	mkt := rig.mkt
	epochDur := int64(mkt.EpochDuration())
	const current int64 = 40
	row := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, current, epochDur)
	setTestMarketLifecycle(mkt, rig.storage, row)

	meshSvc, err := mesh.NewService(&mesh.ServiceConfig{
		EventLogReader: emptyEventLogReader{},
		OnHalt:         func(error) {},
		Commands:       LifecycleCommands(map[string]*Market{mkt.name: mkt}),
		Events:         rig.events,
	})
	if err != nil {
		t.Fatalf("NewService error: %v", err)
	}

	wantEnd := time.UnixMilli((current + 2) * epochDur) // end of epoch current+1
	susp, err := ExecuteScheduleSuspend(context.Background(), meshSvc, mkt.name, time.Time{}, true)
	if err != nil {
		t.Fatalf("ExecuteScheduleSuspend: %v", err)
	}
	if susp.Idx != current+1 {
		t.Fatalf("suspend epoch = %d, want %d", susp.Idx, current+1)
	}
	if !susp.End.Equal(wantEnd) {
		t.Fatalf("suspend end = %v, want %v", susp.End, wantEnd)
	}
	if p := lastLifecyclePersist(rig.storage, db.MarketLifecycleActionScheduleSuspend); p == nil || !*p {
		t.Fatalf("first schedule_suspend persist = %v, want true", p)
	}

	_, err = ExecuteScheduleSuspend(context.Background(), meshSvc, "nope_btc", time.Time{}, true)
	if err == nil {
		t.Fatal("expected unknown market error")
	}

	purge := false
	persistFalse, err := ExecuteScheduleSuspend(context.Background(), meshSvc, mkt.name, time.Time{}, purge)
	if err != nil {
		t.Fatalf("ExecuteScheduleSuspend persist=false: %v", err)
	}
	if persistFalse.Idx != current+1 || !persistFalse.End.Equal(wantEnd) {
		t.Fatalf("reschedule epoch/end = %d %v, want %d %v", persistFalse.Idx, persistFalse.End, current+1, wantEnd)
	}
	if p := lastLifecyclePersist(rig.storage, db.MarketLifecycleActionScheduleSuspend); p == nil || *p {
		t.Fatalf("second schedule_suspend persist = %v, want false", p)
	}

	suspended := seedLifecycleRow(db.MarketStateSuspended, db.MarketPendingNone, current, epochDur)
	setTestMarketLifecycle(mkt, rig.storage, suspended)
	startEpoch, startTime, err := ExecuteScheduleResume(context.Background(), meshSvc, mkt.name, time.Time{})
	if err != nil {
		t.Fatalf("ExecuteScheduleResume: %v", err)
	}
	if startEpoch <= 0 {
		t.Fatalf("resume epoch=%d", startEpoch)
	}
	wantStart := time.UnixMilli(epochDur * startEpoch)
	if !startTime.Equal(wantStart) {
		t.Fatalf("resume time = %v, want %v", startTime, wantStart)
	}
	if testLifecycleActionCount(rig.storage, db.MarketLifecycleActionScheduleResume) != 1 {
		t.Fatalf("schedule_resume was not applied")
	}
}

func requireOrderIDs(t *testing.T, got []order.OrderID, want ...order.OrderID) {
	t.Helper()
	want = append([]order.OrderID(nil), want...)
	sort.Slice(want, func(i, j int) bool {
		return bytes.Compare(want[i][:], want[j][:]) < 0
	})
	if len(got) != len(want) {
		t.Fatalf("order IDs = %d, want %d (%v)", len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order ID %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func requireEpochNoteFromLink(t *testing.T, link *TLink, mkt *Market, ord order.Order, epochIdx int64, wantOrderType uint8) {
	t.Helper()
	note := getEpochNoteFromLink(t, link)
	if note.MarketID != mkt.name {
		t.Fatalf("epoch note market = %q, want %q", note.MarketID, mkt.name)
	}
	if note.Epoch != uint64(epochIdx) {
		t.Fatalf("epoch note epoch = %d, want %d", note.Epoch, epochIdx)
	}
	if note.Seq == 0 {
		t.Fatalf("expected non-zero epoch note sequence")
	}
	if note.OrderType != wantOrderType {
		t.Fatalf("epoch note order type = %d, want %d", note.OrderType, wantOrderType)
	}
	oid := ord.ID()
	if !bytes.Equal(note.OrderID, oid[:]) {
		t.Fatalf("epoch note order id = %x, want %x", note.OrderID, oid)
	}

	switch ord := ord.(type) {
	case *order.LimitOrder:
		if note.Rate != ord.Rate {
			t.Fatalf("limit note rate = %d, want %d", note.Rate, ord.Rate)
		}
		tif := uint8(msgjson.StandingOrderNum)
		if ord.Force == order.ImmediateTiF {
			tif = msgjson.ImmediateOrderNum
		}
		if note.TiF != tif {
			t.Fatalf("limit note tif = %d, want %d", note.TiF, tif)
		}
	case *order.CancelOrder:
		if !bytes.Equal(note.TargetID, ord.TargetOrderID[:]) {
			t.Fatalf("cancel note target = %x, want %x", note.TargetID, ord.TargetOrderID)
		}
	}
}

func requireNoBookNoteFromLink(t *testing.T, link *TLink) {
	t.Helper()
	link.mtx.Lock()
	defer link.mtx.Unlock()
	if len(link.sends) != 0 {
		t.Fatalf("unexpected book notification")
	}
}

func epochStampedLO(t *testing.T, epochIdx, epochDur, offset int64, coin order.CoinID) *order.LimitOrder {
	t.Helper()
	lo := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	lo.SetTime(time.UnixMilli(epochIdx*epochDur + offset))
	lo.Coins = []order.CoinID{coin}
	return lo
}

func epochStampedCO(t *testing.T, targetID order.OrderID, epochIdx, epochDur, offset int64) *order.CancelOrder {
	t.Helper()
	co := makeCO(seller3, targetID)
	co.SetTime(time.UnixMilli(epochIdx*epochDur + offset))
	return co
}

// bookStandingOrder locks a standing order's funding coins and inserts it
// into the market book and the router's book projection, as the router's
// startup snapshot or booked-order apply would in production.
func bookStandingOrder(t *testing.T, rig *marketEventRig, lo *order.LimitOrder) {
	t.Helper()
	mkt := rig.mkt
	if !mkt.lockOrderCoins(lo) {
		t.Fatalf("failed to lock book order coins")
	}
	mkt.bookMtx.Lock()
	inserted := mkt.book.Insert(lo)
	mkt.bookMtx.Unlock()
	if !inserted {
		t.Fatalf("failed to insert book order %v", lo.ID())
	}
	rig.bookRouter.books[mkt.name].insert(lo)
}

// requireRevokedOrderGone checks that a revoked order has left the market
// book and that its funding coins are unlocked in the side-appropriate locker.
func requireRevokedOrderGone(t *testing.T, mkt *Market, lo *order.LimitOrder) {
	t.Helper()
	if mkt.book.HaveOrder(lo.ID()) {
		t.Fatalf("revoked order %v remains in market book", lo.ID())
	}
	assetID := mkt.Quote()
	if lo.Sell {
		assetID = mkt.Base()
	}
	for _, coin := range lo.Coins {
		if mkt.CoinLocked(assetID, []byte(coin)) {
			t.Fatalf("revoked order %v coin %x remains locked", lo.ID(), coin)
		}
	}
}

// collectOwnerNotes drains the auth feed and collects revoke_order and
// nomatch notes by order ID, failing on any penalty note.
func collectOwnerNotes(t *testing.T, auth *TAuth) (revokes, nomatches map[order.OrderID]bool) {
	t.Helper()
	revokes = make(map[order.OrderID]bool)
	nomatches = make(map[order.OrderID]bool)
	for msg := auth.getSend(); msg != nil; msg = auth.getSend() {
		switch msg.Route {
		case msgjson.RevokeOrderRoute:
			var note msgjson.RevokeOrder
			if err := json.Unmarshal(msg.Payload, &note); err != nil {
				t.Fatalf("revoke note unmarshal: %v", err)
			}
			var oid order.OrderID
			copy(oid[:], note.OrderID)
			revokes[oid] = true
		case msgjson.NoMatchRoute:
			var note msgjson.NoMatch
			if err := json.Unmarshal(msg.Payload, &note); err != nil {
				t.Fatalf("nomatch note unmarshal: %v", err)
			}
			var oid order.OrderID
			copy(oid[:], note.OrderID)
			nomatches[oid] = true
		case msgjson.PenaltyRoute:
			t.Fatalf("unexpected penalty note")
		}
	}
	return revokes, nomatches
}

// seedLifecycleRow builds a lifecycle row shaped like the given state for
// seeding tests, mirroring the field combinations the real projections
// produce.
// TestRunParamsAdoption pins the market_started parameter era: the applied
// event's parameters — not this node's markets.json — decide validation,
// the book's lot size, and the reported config, exactly as they would on a
// replay after a config change or on a slave with different config.
func TestRunParamsAdoption(t *testing.T) {
	rig := newMarketEventRig(t)
	defer rig.cleanup()
	mkt := rig.mkt
	cfgLotSize := mkt.configuredParams.LotSize

	// Before the first start, the getters report process config.
	if mkt.LotSize() != cfgLotSize {
		t.Fatalf("pre-start lot size = %d, want config %d", mkt.LotSize(), cfgLotSize)
	}

	// The started event pins a parameter set that differs from config.
	epochDur := int64(mkt.EpochDuration())
	epochIdx := int64(2345)
	runParams := mkt.configuredParams.MarketRunParams
	runParams.LotSize = cfgLotSize * 2
	runParams.MaxUserCancelsPerEpoch = 1
	rig.apply(t, newMarketStartedEvent(mkt.name, epochIdx, epochDur, runParams,
		time.UnixMilli(1).UTC(), nil))

	if mkt.LotSize() != runParams.LotSize {
		t.Fatalf("adopted lot size = %d, want %d", mkt.LotSize(), runParams.LotSize)
	}
	if mkt.maxUserCancelsPerEpoch() != 1 {
		t.Fatalf("adopted cancel cap = %d, want 1", mkt.maxUserCancelsPerEpoch())
	}
	if got := mkt.book.LotSize(); got != runParams.LotSize {
		t.Fatalf("book lot size = %d, want %d", got, runParams.LotSize)
	}
	if got := mkt.Status().EpochDuration; got != uint64(epochDur) {
		t.Fatalf("status epoch duration = %d, want %d", got, epochDur)
	}

	// One config lot is not a multiple of the adopted lot size: the order
	// that this node's markets.json would accept is rejected.
	badLO := makeLO(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
	badLO.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
	badLO.Coins = []order.CoinID{[]byte{0x71, 0x01}}
	if err := rig.applyErr(t, newOrderAcceptedEvent(badLO)); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("config-lot order apply error = %v, want %v", err, ErrInvalidOrder)
	}

	// Two config lots make one adopted lot and apply cleanly.
	goodLO := makeLO(seller3, mkRate3(1.0, 1.2), 2, order.StandingTiF)
	goodLO.SetTime(time.UnixMilli(epochIdx*epochDur + 2))
	goodLO.Coins = []order.CoinID{[]byte{0x71, 0x02}}
	rig.apply(t, newOrderAcceptedEvent(goodLO))
	goodLO2 := makeLO(seller3, mkRate3(1.1, 1.3), 2, order.StandingTiF)
	goodLO2.SetTime(time.UnixMilli(epochIdx*epochDur + 3))
	goodLO2.Coins = []order.CoinID{[]byte{0x71, 0x03}}
	rig.apply(t, newOrderAcceptedEvent(goodLO2))

	// The adopted cancel cap of one: the first cancel applies, a second
	// cancel against a different target is capped.
	co := makeCO(seller3, goodLO.ID())
	co.SetTime(time.UnixMilli(epochIdx*epochDur + 4))
	rig.apply(t, newOrderAcceptedEvent(co))
	co2 := makeCO(seller3, goodLO2.ID())
	co2.SetTime(time.UnixMilli(epochIdx*epochDur + 5))
	if err := rig.applyErr(t, newOrderAcceptedEvent(co2)); !errors.Is(err, ErrTooManyCancelOrders) {
		t.Fatalf("capped cancel apply error = %v, want %v", err, ErrTooManyCancelOrders)
	}
}

func TestLoadStateAdoptsRowParams(t *testing.T) {
	storage := &TArchivist{}
	const epochDur int64 = 500 // newTestMarket's epoch duration
	row := seedLifecycleRow(db.MarketStateSuspended, db.MarketPendingNone, 40, epochDur)
	row.RunParams.LotSize = dcrLotSize * 2
	storage.lifecycle = row
	compatible := makeLO(seller3, mkRate3(1.0, 1.2), 2, order.StandingTiF)
	compatible.Coins = []order.CoinID{[]byte{0x82, 0x01}}
	storage.bookedOrders = []*order.LimitOrder{compatible}

	mkt, _, _, cleanup, err := newTestMarket(storage)
	if err != nil {
		t.Fatalf("newTestMarket: %v", err)
	}
	defer cleanup()

	if mkt.LotSize() != dcrLotSize*2 {
		t.Fatalf("adopted lot size = %d, want %d", mkt.LotSize(), dcrLotSize*2)
	}
	if got := mkt.book.LotSize(); got != uint64(dcrLotSize*2) {
		t.Fatalf("book lot size = %d, want %d", got, dcrLotSize*2)
	}
	if mkt.book.Order(compatible.ID()) == nil {
		t.Fatalf("row-compatible order was not rebooked")
	}
}

// TestLoadStateRejectsIncompatibleBookedOrder pins that a booked DB row
// which does not fit the logged lot size is a LoadState error, not a skip.
func TestLoadStateRejectsIncompatibleBookedOrder(t *testing.T) {
	storage := &TArchivist{}
	const epochDur int64 = 500
	row := seedLifecycleRow(db.MarketStateSuspended, db.MarketPendingNone, 40, epochDur)
	row.RunParams.LotSize = dcrLotSize * 2
	storage.lifecycle = row
	incompatible := makeLO(seller3, mkRate3(1.1, 1.3), 1, order.StandingTiF)
	incompatible.Coins = []order.CoinID{[]byte{0x82, 0x02}}
	storage.bookedOrders = []*order.LimitOrder{incompatible}

	_, _, _, _, err := newTestMarket(storage)
	if err == nil || !strings.Contains(err.Error(), "failed to insert stored booked order") {
		t.Fatalf("LoadState err = %v, want insert failure", err)
	}
}

// setTestRunMinimumRate pins a rate floor in the market's adopted run
// parameters, the only floor live admission reads.
func setTestRunMinimumRate(t *testing.T, mkt *Market, rate uint64) {
	t.Helper()
	run := mkt.liveParams.Load()
	if run == nil {
		t.Fatalf("market has no adopted run parameters")
	}
	cpy := *run
	cpy.MinimumRate = rate
	mkt.liveParams.Store(&cpy)
}

func seedLifecycleRow(state db.MarketState, pending db.MarketPendingAction, epochIdx, epochDur int64) *db.MarketLifecycle {
	persist := true
	lc := &db.MarketLifecycle{
		Market:        "dcr_btc", // matches newTestMarket
		State:         state,
		StartEpochIdx: epochIdx - 3,
		StartEpochDur: epochDur,
		PendingAction: pending,
		PersistBook:   &persist,
		// Matches newTestMarket's config, so adoption pins the same values
		// the tests' orders are built for.
		RunParams: meshevents.MarketRunParams{
			LotSize:                dcrLotSize,
			RateStep:               btcRateStep,
			ParcelSize:             1,
			MaxUserCancelsPerEpoch: math.MaxUint32,
		},
	}
	switch {
	case state == db.MarketStateSuspended:
		lc.FinalEpochIdx, lc.FinalEpochDur = epochIdx, epochDur
		lc.ProcessedEpochIdx = epochIdx
	case pending == db.MarketPendingSuspend:
		lc.FinalEpochIdx, lc.FinalEpochDur = epochIdx, epochDur
		lc.PendingEpochIdx, lc.PendingEpochDur = epochIdx, epochDur
		lc.ActiveEpochIdx = epochIdx
		lc.ProcessedEpochIdx = epochIdx - 1
	case pending == db.MarketPendingSuspendDrain:
		lc.FinalEpochIdx, lc.FinalEpochDur = epochIdx, epochDur
		lc.PendingEpochIdx, lc.PendingEpochDur = epochIdx, epochDur
		lc.ProcessedEpochIdx = epochIdx - 1
	default: // running, nothing pending
		lc.PersistBook = nil
		lc.ActiveEpochIdx = epochIdx
		lc.ProcessedEpochIdx = epochIdx - 1
	}
	return lc
}

// seedRunningLifecycle restores a running lifecycle row with the given epoch
// cursor and seeds the market's epoch memory from the rig's storage mock,
// mirroring what LoadState does at startup.
func seedRunningLifecycle(t *testing.T, mkt *Market, activeEpochIdx, epochDur int64) {
	t.Helper()
	lc := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, activeEpochIdx, epochDur)
	mkt.restoreMarketLifecycle(lc)
	if err := mkt.seedEpochMemory(lc); err != nil {
		t.Fatalf("seedEpochMemory: %v", err)
	}
}

func TestSeedEpochMemory(t *testing.T) {
	const epochDur int64 = 500 // newTestMarket's epoch duration
	const active int64 = 40

	noCursor := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, active, epochDur)
	noCursor.ActiveEpochIdx = 0
	badDur := seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, active, epochDur)
	badDur.StartEpochDur = epochDur * 2
	stray := epochStampedLO(t, active+2, epochDur, 1, order.CoinID{0x60, 0x02})
	leftover := epochStampedLO(t, active, epochDur, 1, order.CoinID{0x60, 0x03})

	pending := epochStampedLO(t, active-1, epochDur, 0, order.CoinID{0x30, 0x01}) // closed, awaiting epoch_processed
	curStart := epochStampedLO(t, active, epochDur, 0, order.CoinID{0x31, 0x01})
	curEnd := epochStampedLO(t, active+1, epochDur, -1, order.CoinID{0x31, 0x02})
	nextStart := epochStampedLO(t, active+1, epochDur, 0, order.CoinID{0x32, 0x01})
	curCancel := epochStampedCO(t, curStart.ID(), active, epochDur, 2)
	finalLO := epochStampedLO(t, active, epochDur, 1, order.CoinID{0x60, 0x04})
	drainA := epochStampedLO(t, active, epochDur, 1, order.CoinID{0x71, 0x01})
	drainB := epochStampedLO(t, active, epochDur, 2, order.CoinID{0x71, 0x02})
	conLO := epochStampedLO(t, active, epochDur, 1, order.CoinID{0x60, 0x05})

	cases := []struct {
		name      string
		row       *db.MarketLifecycle
		orders    []epochOrderWrite
		construct bool   // load through market construction instead of a direct seed
		wantErr   string // non-empty: loading must fail with this

		wantCurrent  int64         // expected active epoch, 0 = no epochs seeded
		wantQueued   []order.Order // seeded into the epoch queues and indexes
		wantUnqueued []order.Order // coin-locked but kept out of the queues
	}{{
		name:    "rejects running row without cursor",
		row:     noCursor,
		wantErr: "no active epoch cursor",
	}, {
		name:        "seeds with log duration on config mismatch",
		row:         badDur,
		wantCurrent: active,
	}, {
		name:    "rejects order beyond the next epoch window",
		row:     seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, active, epochDur),
		orders:  []epochOrderWrite{{ord: stray, epochIdx: active + 2, epochDur: epochDur}},
		wantErr: "beyond the next epoch window",
	}, {
		name:    "rejects suspended market with epoch orders",
		row:     seedLifecycleRow(db.MarketStateSuspended, db.MarketPendingNone, active, epochDur),
		orders:  []epochOrderWrite{{ord: leftover, epochIdx: active, epochDur: epochDur}},
		wantErr: "storage is inconsistent",
	}, {
		name: "partitions windows around the cursor",
		row:  seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, active, epochDur),
		orders: []epochOrderWrite{
			{ord: pending, epochIdx: active - 1, epochDur: epochDur},
			{ord: curStart, epochIdx: active, epochDur: epochDur},
			{ord: curEnd, epochIdx: active, epochDur: epochDur},
			{ord: curCancel, epochIdx: active, epochDur: epochDur},
			{ord: nextStart, epochIdx: active + 1, epochDur: epochDur},
		},
		wantCurrent:  active,
		wantQueued:   []order.Order{curStart, curEnd, curCancel, nextStart},
		wantUnqueued: []order.Order{pending},
	}, {
		name:        "pending suspend seeds the final epoch with an empty next queue",
		row:         seedLifecycleRow(db.MarketStateRunning, db.MarketPendingSuspend, active, epochDur),
		orders:      []epochOrderWrite{{ord: finalLO, epochIdx: active, epochDur: epochDur}},
		wantCurrent: active,
		wantQueued:  []order.Order{finalLO},
	}, {
		name: "drain seeds no queues but locks the final epoch orders",
		row:  seedLifecycleRow(db.MarketStateRunning, db.MarketPendingSuspendDrain, active, epochDur),
		orders: []epochOrderWrite{
			{ord: drainA, epochIdx: active, epochDur: epochDur},
			{ord: drainB, epochIdx: active, epochDur: epochDur},
		},
		wantUnqueued: []order.Order{drainA, drainB},
	}, {
		name:        "construction seeds a running row",
		construct:   true,
		row:         seedLifecycleRow(db.MarketStateRunning, db.MarketPendingNone, active, epochDur),
		orders:      []epochOrderWrite{{ord: conLO, epochIdx: active, epochDur: epochDur}},
		wantCurrent: active,
		wantQueued:  []order.Order{conLO},
	}, {
		name:      "a seeding error fails market construction",
		construct: true,
		row:       noCursor,
		wantErr:   "no active epoch cursor",
	}, {
		name:      "no lifecycle row is never started",
		construct: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mkt *Market
			var loadErr error
			if tc.construct {
				storage := &TArchivist{}
				storage.lifecycle = tc.row
				storage.epochOrders = tc.orders
				var cleanup func()
				mkt, _, _, cleanup, loadErr = newTestMarket(storage)
				if loadErr == nil {
					defer cleanup()
				}
			} else {
				rig := newMarketEventRig(t)
				defer rig.cleanup()
				mkt = rig.mkt
				rig.storage.mtx.Lock()
				rig.storage.epochOrders = tc.orders
				rig.storage.mtx.Unlock()
				mkt.restoreMarketLifecycle(tc.row)
				loadErr = mkt.seedEpochMemory(tc.row)
			}
			if tc.wantErr != "" {
				if loadErr == nil || !strings.Contains(loadErr.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", loadErr, tc.wantErr)
				}
				return
			}
			if loadErr != nil {
				t.Fatalf("loading failed: %v", loadErr)
			}
			if mkt.Running() {
				t.Fatalf("loading opened order admission")
			}
			requireSeededState(t, mkt, tc.wantCurrent, tc.wantQueued, tc.wantUnqueued)
		})
	}
}

// requireSeededState asserts the epoch-memory contract: the epoch windows
// and cursors sit at wantCurrent, exactly the queued orders are indexed —
// each in the queue its stamp selects, cancels with their bookkeeping —
// the unqueued orders stay out of the queues, and every trade's funding
// coins are locked under its own order ID in the side-appropriate locker.
func requireSeededState(t *testing.T, mkt *Market, wantCurrent int64, queued, unqueued []order.Order) {
	t.Helper()
	epochDur := int64(mkt.EpochDuration())

	requireEpochWindow := func(label string, epoch *EpochQueue, wantIdx int64) {
		t.Helper()
		if epoch == nil || epoch.Epoch != wantIdx || epoch.Duration != epochDur {
			t.Fatalf("%s epoch = %v, want %d/%d", label, epoch, wantIdx, epochDur)
		}
	}
	// An order's stamp picks its queue: wantCurrent's window is the current
	// queue, the following window is next.
	queueFor := func(ord order.Order) *EpochQueue {
		if ord.Time()/epochDur == wantCurrent {
			return mkt.currentEpoch
		}
		return mkt.nextEpoch
	}

	mkt.epochMtx.RLock()

	// The epoch windows and the active cursor.
	if wantCurrent == 0 {
		if mkt.currentEpoch != nil || mkt.nextEpoch != nil || mkt.activeEpochIdx != 0 {
			t.Fatalf("epochs = %v/%v (active %d), want none",
				mkt.currentEpoch, mkt.nextEpoch, mkt.activeEpochIdx)
		}
	} else {
		requireEpochWindow("current", mkt.currentEpoch, wantCurrent)
		requireEpochWindow("next", mkt.nextEpoch, wantCurrent+1)
		if mkt.activeEpochIdx != wantCurrent {
			t.Fatalf("active epoch = %d, want %d", mkt.activeEpochIdx, wantCurrent)
		}
	}

	// Exactly the queued orders are indexed, each in its stamp-selected
	// queue, cancels with their bookkeeping.
	if len(mkt.epochOrders) != len(queued) {
		t.Fatalf("epoch order index has %d orders, want %d", len(mkt.epochOrders), len(queued))
	}
	for _, ord := range queued {
		epoch := queueFor(ord)
		if epoch.Orders[ord.ID()] == nil {
			t.Fatalf("order %v missing from the epoch %d queue", ord.ID(), epoch.Epoch)
		}
		if mkt.epochOrders[ord.ID()] == nil {
			t.Fatalf("order %v missing from the epoch order index", ord.ID())
		}
		if oid := mkt.epochCommitments[ord.Commitment()]; oid != ord.ID() {
			t.Fatalf("commitment index for order %v = %v", ord.ID(), oid)
		}
		if co, ok := ord.(*order.CancelOrder); ok {
			if epoch.CancelTargets[co.TargetOrderID] == nil {
				t.Fatalf("cancel %v missing target bookkeeping", co.ID())
			}
			if epoch.UserCancels[co.AccountID] == 0 {
				t.Fatalf("cancel %v missing user cancel bookkeeping", co.ID())
			}
		}
	}

	// The unqueued orders stay out of the queues and indexes.
	for _, ord := range unqueued {
		if mkt.epochOrders[ord.ID()] != nil {
			t.Fatalf("order %v was seeded into the epoch queues", ord.ID())
		}
		if oid, found := mkt.epochCommitments[ord.Commitment()]; found {
			t.Fatalf("order %v commitment still indexed to %v", ord.ID(), oid)
		}
	}
	mkt.epochMtx.RUnlock()

	// The book epoch cursor follows the active epoch.
	if wantCurrent != 0 {
		mkt.bookMtx.Lock()
		bookEpochIdx := mkt.bookEpochIdx
		mkt.bookMtx.Unlock()
		if bookEpochIdx != wantCurrent {
			t.Fatalf("bookEpochIdx = %d, want %d", bookEpochIdx, wantCurrent)
		}
	}

	// Every trade's funding coins are locked under its own order ID in the
	// side-appropriate locker, so later unlocks by order ID find them.
	for _, group := range [][]order.Order{queued, unqueued} {
		for _, ord := range group {
			trade := ord.Trade()
			if trade == nil {
				continue // cancels fund nothing
			}
			locker := mkt.coinLockerQuote
			if trade.Sell {
				locker = mkt.coinLockerBase
			}
			locked := make(map[string]bool)
			for _, coin := range locker.OrderCoinsLocked(ord.ID()) {
				locked[string(coin)] = true
			}
			for _, coin := range trade.Coins {
				if !locked[string(coin)] {
					t.Fatalf("coin %v of order %v is not locked under the order's ID", coin, ord.ID())
				}
			}
		}
	}
}

// TestApplyEpochProcessedPreimageOutcomes exercises the preimage half of the
// epoch_processed apply: reveal and miss updates reach storage, a miss's
// funding coins unlock only after a successful DB apply, and bad payloads
// (unknown market, commitment mismatch) or storage failures reject the event.
func TestApplyEpochProcessedPreimageOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*marketEventRig, *mesh.Event)
		wantErr bool
	}{
		{
			name: "success",
		},
		{
			name:    "unknown market",
			wantErr: true,
			mutate: func(_ *marketEventRig, event *mesh.Event) {
				var payload epochProcessedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("unmarshal event: %v", err)
				}
				payload.Market = "unknown_market"
				event.Payload = mustMarshal(&payload)
			},
		},
		{
			name:    "preimage commitment mismatch",
			wantErr: true,
			mutate: func(_ *marketEventRig, event *mesh.Event) {
				var payload epochProcessedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("unmarshal event: %v", err)
				}
				if len(payload.Revealed) != 1 {
					t.Fatalf("revealed records = %d, want 1", len(payload.Revealed))
				}
				payload.Revealed[0].Preimage[0] ^= 0xff
				event.Payload = mustMarshal(&payload)
			},
		},
		{
			name:    "db failure",
			wantErr: true,
			mutate: func(rig *marketEventRig, _ *mesh.Event) {
				rig.storage.poisonEpochProcessed = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newMarketEventRig(t)
			defer rig.cleanup()
			mkt, storage := rig.mkt, rig.storage

			revealed, pi := makeLORevealed(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
			missed, _ := makeLORevealed(seller3, mkRate3(1.2, 1.4), 1, order.StandingTiF)
			missedCoin := order.CoinID([]byte{0x51, 0x52, 0x53})
			missed.Coins = []order.CoinID{missedCoin}
			if !mkt.lockOrderCoins(missed) {
				t.Fatalf("failed to lock missed order coins")
			}
			if !mkt.CoinLocked(mkt.Base(), missedCoin) {
				t.Fatalf("missed order coin not locked before apply")
			}

			matchTime := time.UnixMilli(123456789000).UTC()
			missRevokeTime := time.UnixMilli(123456789999).UTC()
			event := newEpochProcessedMeshEvent(mkt.name, 9876, int64(mkt.EpochDuration()), matchTime,
				matcher.CSum([]order.Order{revealed, missed}),
				[]*matcher.OrderRevealed{{
					Order:    revealed,
					Preimage: pi,
				}},
				[]order.Order{missed}, missRevokeTime)
			if tt.mutate != nil {
				tt.mutate(rig, event)
			}

			applier := rig.events[event.Kind]
			if applier == nil {
				t.Fatalf("missing applier for %q", event.Kind)
			}
			_, err := applier(&mesh.EventApplyContext{Context: context.Background()}, event)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected apply error")
				}
				if len(storage.epochProcessed) != 0 {
					t.Fatalf("epoch processed updates = %d, want 0", len(storage.epochProcessed))
				}
				if !mkt.CoinLocked(mkt.Base(), missedCoin) {
					t.Fatalf("missed order coin unlocked before successful DB apply")
				}
				return
			}

			if err != nil {
				t.Fatalf("apply epoch processed event error: %v", err)
			}
			if len(storage.epochProcessed) != 1 {
				t.Fatalf("epoch processed updates = %d, want 1", len(storage.epochProcessed))
			}
			resultUpdate := storage.epochProcessed[0]
			if len(resultUpdate.Reveals) != 1 {
				t.Fatalf("preimage reveal updates = %d, want 1", len(resultUpdate.Reveals))
			}
			revealUpdate := resultUpdate.Reveals[0]
			if revealUpdate.Order.ID() != revealed.ID() || revealUpdate.Preimage != pi {
				t.Fatalf("preimage reveal update = %+v", revealUpdate)
			}
			if len(resultUpdate.Misses) != 1 {
				t.Fatalf("preimage miss updates = %d, want 1", len(resultUpdate.Misses))
			}
			missUpdate := resultUpdate.Misses[0]
			if missUpdate.Order.ID() != missed.ID() || !missUpdate.RevokeTime.Equal(missRevokeTime) {
				t.Fatalf("preimage miss update = %+v", missUpdate)
			}
			if mkt.CoinLocked(mkt.Base(), missedCoin) {
				t.Fatalf("missed order coin still locked after apply")
			}
		})
	}
}

func TestApplyEpochProcessedEvent(t *testing.T) {
	const epochIdx int64 = 4321

	type revealedOrder struct {
		ord order.Order
		pi  order.Preimage
	}

	type updateCounts struct {
		booked, partial, completed, canceled int
		failed, cancelsFailed, cancelsDone   int
		matches                              int
	}

	type applyWant struct {
		counts  updateCounts
		tracked int
	}

	requireCoinState := func(t *testing.T, mkt *Market, asset uint32, coin order.CoinID, locked bool, label string) {
		t.Helper()
		if got := mkt.CoinLocked(asset, coin); got != locked {
			t.Fatalf("%s coin locked = %v, want %v", label, got, locked)
		}
	}

	requireNoBookSends := func(t *testing.T, link *TLink) {
		t.Helper()
		link.mtx.Lock()
		defer link.mtx.Unlock()
		if len(link.sends) != 0 {
			t.Fatalf("unexpected book notifications = %d", len(link.sends))
		}
	}

	requireNoAuthSends := func(t *testing.T, auth *TAuth) {
		t.Helper()
		auth.sendsMtx.Lock()
		defer auth.sendsMtx.Unlock()
		if len(auth.sends) != 0 {
			t.Fatalf("unexpected auth notifications = %d", len(auth.sends))
		}
	}

	requireRoute := func(t *testing.T, msg *msgjson.Message, route string) {
		t.Helper()
		if msg == nil {
			t.Fatalf("missing %q notification", route)
		}
		if msg.Route != route {
			t.Fatalf("notification route = %q, want %q", msg.Route, route)
		}
	}

	requireMatchProof := func(t *testing.T, link *TLink, mkt *Market, preimages ...order.Preimage) {
		t.Helper()
		msg := link.getSend()
		requireRoute(t, msg, msgjson.MatchProofRoute)
		var note msgjson.MatchProofNote
		if err := json.Unmarshal(msg.Payload, &note); err != nil {
			t.Fatalf("match proof note: %v", err)
		}
		if note.MarketID != mkt.name || note.Epoch != uint64(epochIdx) {
			t.Fatalf("match proof market/epoch = %q/%d, want %q/%d",
				note.MarketID, note.Epoch, mkt.name, epochIdx)
		}
		if len(note.Preimages) != len(preimages) {
			t.Fatalf("match proof preimages = %d, want %d", len(note.Preimages), len(preimages))
		}
		for i := range preimages {
			if !bytes.Equal(note.Preimages[i], preimages[i][:]) {
				t.Fatalf("match proof preimage %d = %x, want %x", i, note.Preimages[i], preimages[i])
			}
		}
		if len(note.Misses) != 0 {
			t.Fatalf("match proof misses = %d, want 0", len(note.Misses))
		}
	}

	requireNoMatch := func(t *testing.T, auth *TAuth, ord order.Order) {
		t.Helper()
		msg := auth.getSend()
		requireRoute(t, msg, msgjson.NoMatchRoute)
		var note msgjson.NoMatch
		if err := json.Unmarshal(msg.Payload, &note); err != nil {
			t.Fatalf("nomatch note: %v", err)
		}
		oid := ord.ID()
		if !bytes.Equal(note.OrderID, oid[:]) {
			t.Fatalf("nomatch order id = %x, want %x", note.OrderID, oid)
		}
	}

	requireNoteOrder := func(t *testing.T, label string, mkt *Market, marketID string, orderID msgjson.Bytes, ord order.Order) {
		t.Helper()
		if marketID != mkt.name {
			t.Fatalf("%s market = %q, want %q", label, marketID, mkt.name)
		}
		oid := ord.ID()
		if !bytes.Equal(orderID, oid[:]) {
			t.Fatalf("%s order id = %x, want %x", label, orderID, oid)
		}
	}

	requireCounts := func(t *testing.T, update *db.EpochProcessedUpdate, want updateCounts) {
		t.Helper()
		got := updateCounts{
			booked:        len(update.TradesBooked),
			partial:       len(update.TradesPartial),
			completed:     len(update.TradesCompleted),
			canceled:      len(update.TradesCanceled),
			failed:        len(update.TradesFailed),
			cancelsFailed: len(update.CancelsFailed),
			cancelsDone:   len(update.CancelsExecuted),
			matches:       len(update.Matches),
		}
		if got != want {
			t.Fatalf("epoch processed counts = %+v, want %+v", got, want)
		}
	}

	requireOrderID := func(t *testing.T, got order.Order, want order.Order) {
		t.Helper()
		if got == nil || got.ID() != want.ID() {
			t.Fatalf("order = %v, want %v", got, want.ID())
		}
	}

	epochProcessedEvent := func(mkt *Market, revealed ...revealedOrder) meshEventBuilder {
		orders := make([]order.Order, 0, len(revealed))
		revealedOrders := make([]*matcher.OrderRevealed, 0, len(revealed))
		for _, ro := range revealed {
			orders = append(orders, ro.ord)
			revealedOrders = append(revealedOrders, &matcher.OrderRevealed{
				Order:    ro.ord,
				Preimage: ro.pi,
			})
		}
		epochDur := int64(mkt.EpochDuration())
		return encoderBuilder{meshevents.NewEpochProcessedEvent(mkt.name, epochIdx, epochDur,
			time.UnixMilli(epochIdx*epochDur+100), 10, 10, mkt.lastRate, matcher.CSum(orders),
			revealedOrders, nil, time.Time{})}
	}

	tests := []struct {
		name              string
		bookedLots        uint64 // pre-booked standing order (0 = none)
		bookedSell        bool
		epochKind         string // "standing", "immediate" (crosses the booked maker), "cancel" (targets the booked order)
		fault             string // "" | "storage" (uncommitted failure) | "track" (committed failure)
		want              applyWant
		wantEpochBooked   bool   // the epoch trade lands on the book with its funding lock kept
		wantBookedPartial bool   // the booked maker stays with the match quantity filled and settling
		wantBookedGone    bool   // the booked order leaves the book with its coin unlocked
		midNote           string // book-feed note between the match proof and epoch report
		wantNoMatchNote   bool
	}{
		{
			name:            "books unmatched standing order",
			epochKind:       "standing",
			want:            applyWant{counts: updateCounts{booked: 1}},
			wantEpochBooked: true,
			midNote:         "book",
			wantNoMatchNote: true,
		},
		{
			name:              "partial trade updates remaining and tracks match",
			bookedLots:        2,
			epochKind:         "immediate",
			want:              applyWant{counts: updateCounts{partial: 1, completed: 1, matches: 1}, tracked: 1},
			wantBookedPartial: true,
			midNote:           "remaining",
		},
		{
			name:           "cancel match unbooks target",
			bookedLots:     1,
			bookedSell:     true,
			epochKind:      "cancel",
			want:           applyWant{counts: updateCounts{canceled: 1, cancelsDone: 1, matches: 1}, tracked: 1},
			wantBookedGone: true,
			midNote:        "unbook",
		},
		{
			name:      "storage failure does not mutate live book",
			epochKind: "standing",
			fault:     "storage",
		},
		{
			name:           "track failure is committed apply error",
			bookedLots:     1,
			epochKind:      "immediate",
			fault:          "track",
			want:           applyWant{counts: updateCounts{completed: 2, matches: 1}, tracked: 1},
			wantBookedGone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newMarketEventRig(t)
			defer rig.cleanup()
			swapper := new(epochProcessedTestSwapper)
			mkt, storage := rig.mkt, rig.storage
			mkt.swapper = swapper
			epochDur := int64(mkt.EpochDuration())

			var booked *order.LimitOrder
			if tt.bookedLots > 0 {
				if tt.bookedSell {
					booked = makeLO(seller3, mkRate3(1.0, 1.2), tt.bookedLots, order.StandingTiF)
					booked.Coins = []order.CoinID{{0x61, 0x62, 0x63}}
				} else {
					booked = makeLO(buyer3, mkRate3(1.0, 1.2), tt.bookedLots, order.StandingTiF)
					booked.Coins = []order.CoinID{{0x41, 0x42, 0x43}}
				}
				booked.SetTime(time.UnixMilli(epochIdx*epochDur - 10))
				bookStandingOrder(t, rig, booked)
			}
			link := rig.subscribeBook(t)

			// The epoch order is seeded directly in the closed-but-unprocessed
			// state that applying advance_epoch leaves: funding coin locked,
			// out of the epoch queues, active epoch already at epochIdx+1.
			var epochOrd order.Order
			var pi order.Preimage
			switch tt.epochKind {
			case "standing":
				lo, loPI := makeLORevealed(seller3, mkRate3(1.0, 1.2), 1, order.StandingTiF)
				lo.Coins = []order.CoinID{{0x31, 0x32, 0x33}}
				epochOrd, pi = lo, loPI
			case "immediate":
				lo, loPI := makeLORevealed(seller3, booked.Rate-dcrRateStep, 1, order.ImmediateTiF)
				lo.Coins = []order.CoinID{{0x51, 0x52, 0x53}}
				epochOrd, pi = lo, loPI
			case "cancel":
				epochOrd, pi = makeCORevealed(seller3, booked.ID())
			}
			epochOrd.SetTime(time.UnixMilli(epochIdx*epochDur + 1))
			seedEpochOrder(storage, epochOrd, epochIdx, epochDur)
			seedRunningLifecycle(t, mkt, epochIdx+1, epochDur)
			rig.bookRouter.books[mkt.name].setEpoch(epochIdx + 1)

			var matchQty uint64
			if booked != nil {
				if tt.epochKind == "cancel" {
					matchQty = booked.Quantity
				} else {
					matchQty = epochOrd.Trade().Quantity
				}
			}

			switch tt.fault {
			case "storage":
				storage.poisonEpochProcessed = true
			case "track":
				swapper.trackErr = errors.New("track failed")
			}

			err := rig.applyErr(t, epochProcessedEvent(mkt, revealedOrder{epochOrd, pi}))
			switch tt.fault {
			case "":
				if err != nil {
					t.Fatalf("apply epoch_processed: %v", err)
				}
			case "storage":
				if err == nil || !strings.Contains(err.Error(), "epoch processed storage failure") {
					t.Fatalf("apply error = %v, want storage failure", err)
				}
			case "track":
				var committedErr *mesh.CommittedEventApplyError
				if !errors.As(err, &committedErr) {
					t.Fatalf("apply error = %T %[1]v, want CommittedEventApplyError", err)
				}
			}

			// The stored update, its order identities, and swapper tracking.
			if tt.fault == "storage" {
				if len(storage.epochProcessed) != 0 {
					t.Fatalf("epoch processed updates = %d, want 0", len(storage.epochProcessed))
				}
				if len(swapper.tracked) != 0 {
					t.Fatalf("tracked match sets = %d, want 0", len(swapper.tracked))
				}
			} else {
				if len(storage.epochProcessed) != 1 {
					t.Fatalf("epoch processed updates = %d, want 1", len(storage.epochProcessed))
				}
				update := storage.epochProcessed[0]
				requireCounts(t, update, tt.want.counts)
				if update.Epoch == nil || update.Epoch.Idx != epochIdx {
					t.Fatalf("epoch update = %+v, want idx %d", update.Epoch, epochIdx)
				}
				if len(swapper.tracked) != tt.want.tracked {
					t.Fatalf("tracked match sets = %d, want %d", len(swapper.tracked), tt.want.tracked)
				}
				c := tt.want.counts
				if c.booked == 1 {
					requireOrderID(t, update.TradesBooked[0], epochOrd)
				}
				if c.partial == 1 {
					requireOrderID(t, update.TradesPartial[0], booked)
				}
				if c.canceled == 1 {
					requireOrderID(t, update.TradesCanceled[0], booked)
				}
				if c.cancelsDone == 1 && update.CancelsExecuted[0].ID() != epochOrd.ID() {
					t.Fatalf("executed cancel = %v, want %v", update.CancelsExecuted[0].ID(), epochOrd.ID())
				}
				switch c.completed {
				case 1:
					requireOrderID(t, update.TradesCompleted[0], epochOrd)
				case 2:
					requireOrderID(t, update.TradesCompleted[0], booked)
					requireOrderID(t, update.TradesCompleted[1], epochOrd)
				}
				if c.matches == 1 {
					match := update.Matches[0]
					if match == nil {
						t.Fatalf("nil stored match")
					}
					if match.Taker.ID() != epochOrd.ID() || match.Maker.ID() != booked.ID() {
						t.Fatalf("match taker/maker = %v/%v, want %v/%v",
							match.Taker.ID(), match.Maker.ID(), epochOrd.ID(), booked.ID())
					}
					if match.Quantity != matchQty {
						t.Fatalf("match qty = %d, want %d", match.Quantity, matchQty)
					}
				}
			}

			// The epoch order's book presence and funding lock.
			if trade := epochOrd.Trade(); trade != nil {
				if got := mkt.book.HaveOrder(epochOrd.ID()); got != tt.wantEpochBooked {
					t.Fatalf("epoch order on book = %t, want %t", got, tt.wantEpochBooked)
				}
				wantLocked := tt.wantEpochBooked || tt.fault == "storage"
				requireCoinState(t, mkt, mkt.Base(), trade.Coins[0], wantLocked, "epoch order")
			}
			if tt.wantEpochBooked {
				if !mkt.Cancelable(epochOrd.ID()) {
					t.Fatalf("booked order %v should be cancelable", epochOrd.ID())
				}
				mkt.epochMtx.RLock()
				_, inOrders := mkt.epochOrders[epochOrd.ID()]
				_, inCommits := mkt.epochCommitments[epochOrd.Commitment()]
				mkt.epochMtx.RUnlock()
				if inOrders || inCommits {
					t.Fatalf("booked order %v still present in the epoch indexes", epochOrd.ID())
				}
			}

			// The pre-booked order's fate.
			if booked != nil {
				bookedAsset := mkt.Quote()
				if booked.Sell {
					bookedAsset = mkt.Base()
				}
				switch {
				case tt.wantBookedPartial:
					if !mkt.book.HaveOrder(booked.ID()) {
						t.Fatalf("partially filled maker %v missing from market book", booked.ID())
					}
					if booked.Filled() != matchQty {
						t.Fatalf("maker filled = %d, want %d", booked.Filled(), matchQty)
					}
					requireCoinState(t, mkt, bookedAsset, booked.Coins[0], true, "maker")
					if got := mkt.settling[booked.ID()]; got != matchQty {
						t.Fatalf("maker settling = %d, want %d", got, matchQty)
					}
					if got := mkt.settling[epochOrd.ID()]; got != matchQty {
						t.Fatalf("taker settling = %d, want %d", got, matchQty)
					}
				case tt.wantBookedGone:
					if mkt.book.HaveOrder(booked.ID()) {
						t.Fatalf("order %v still in market book", booked.ID())
					}
					requireCoinState(t, mkt, bookedAsset, booked.Coins[0], false, "booked order")
					if tt.epochKind == "cancel" {
						if mkt.Cancelable(booked.ID()) {
							t.Fatalf("canceled target %v should not be cancelable", booked.ID())
						}
						if _, found := mkt.settling[booked.ID()]; found {
							t.Fatalf("canceled target %v still settling", booked.ID())
						}
					}
				}
			}

			// Feed and owner notifications: nothing on a failed apply,
			// otherwise match proof, the per-outcome book note, and the
			// epoch report, plus a nomatch for an unmatched trade.
			if tt.fault != "" {
				requireNoBookSends(t, link)
				requireNoAuthSends(t, rig.auth)
				return
			}
			requireMatchProof(t, link, mkt, pi)
			switch tt.midNote {
			case "book":
				note := getBookNoteFromLink(t, link)
				requireNoteOrder(t, "book note", mkt, note.MarketID, note.OrderID, epochOrd)
			case "remaining":
				note := getUpdateRemainingNoteFromLink(t, link)
				requireNoteOrder(t, "update remaining", mkt, note.MarketID, note.OrderID, booked)
				if note.Remaining != booked.Remaining() {
					t.Fatalf("update remaining amount = %d, want %d", note.Remaining, booked.Remaining())
				}
			case "unbook":
				note := getUnbookNoteFromLink(t, link)
				requireNoteOrder(t, "unbook", mkt, note.MarketID, note.OrderID, booked)
			}
			msg := link.getSend()
			requireRoute(t, msg, msgjson.EpochReportRoute)
			if tt.wantNoMatchNote {
				requireNoMatch(t, rig.auth, epochOrd)
			} else {
				requireNoAuthSends(t, rig.auth)
			}
		})
	}
}

// useApplierMesh routes the market's own event submissions through the rig's
// appliers, so they run the same projection every node applies.
func (rig *marketEventRig) useApplierMesh(t *testing.T) {
	t.Helper()
	rig.mkt.SetMeshService(&applyEventMesh{apply: func(ctx context.Context, event *mesh.Event) error {
		applier := rig.events[event.Kind]
		if applier == nil {
			return fmt.Errorf("unsupported test mesh event %q", event.Kind)
		}
		_, err := applier(&mesh.EventApplyContext{Context: ctx}, cloneTestMeshEvent(event))
		return err
	}})
}

func seedEpochOrder(storage *TArchivist, ord order.Order, epochIdx, epochDur int64) {
	storage.mtx.Lock()
	storage.epochOrders = append(storage.epochOrders, epochOrderWrite{
		ord:      ord,
		epochIdx: epochIdx,
		epochDur: epochDur,
	})
	storage.mtx.Unlock()
}

func TestSubmitMarketStarted(t *testing.T) {
	type leftoverEpochOrderSpec struct {
		offset int64 // epochs relative to the current clock epoch
		cancel bool  // targets the previously seeded trade
		coin   order.CoinID
	}
	cases := []struct {
		name           string
		pendingSuspend bool // running row with a suspend pending at suspendOffset
		suspendOffset  int64
		leftoverOrders []leftoverEpochOrderSpec
		wantOpenEpoch  bool
	}{
		{
			name: "running market revokes all leftover epoch orders and opens an epoch",
			leftoverOrders: []leftoverEpochOrderSpec{
				{offset: -900, coin: order.CoinID{0x11, 0x12}},
				{offset: -900, coin: order.CoinID{0x13, 0x14}},
				{offset: -900, cancel: true},
				{offset: 0, coin: order.CoinID{0x31, 0x32}},
			},
			wantOpenEpoch: true,
		},
		{
			name:           "pending suspend past the final epoch finalizes without opening an epoch",
			pendingSuspend: true,
			suspendOffset:  -5,
			leftoverOrders: []leftoverEpochOrderSpec{{offset: -5, coin: order.CoinID{0x51, 0x52}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMarketEventRig(t)
			defer rig.cleanup()
			rig.useApplierMesh(t)
			mkt, storage := rig.mkt, rig.storage
			epochDur := int64(mkt.EpochDuration())

			if tc.pendingSuspend {
				finalIdx := currentMarketEpochIdx(epochDur) + tc.suspendOffset
				persist := true
				setTestMarketLifecycle(mkt, storage, &db.MarketLifecycle{
					Market:          mkt.name,
					State:           db.MarketStateRunning,
					StartEpochIdx:   finalIdx - 5,
					StartEpochDur:   epochDur,
					FinalEpochIdx:   finalIdx,
					FinalEpochDur:   epochDur,
					PendingAction:   db.MarketPendingSuspend,
					PendingEpochIdx: finalIdx,
					PendingEpochDur: epochDur,
					PersistBook:     &persist,
					RunParams:       mkt.configuredParams.MarketRunParams,
				})
			}

			var leftover []order.Order
			var lastTrade *order.LimitOrder
			for i, spec := range tc.leftoverOrders {
				idx := currentMarketEpochIdx(epochDur) + spec.offset
				var ord order.Order
				if spec.cancel {
					ord = epochStampedCO(t, lastTrade.ID(), idx, epochDur, int64(i+1))
				} else {
					lastTrade = epochStampedLO(t, idx, epochDur, int64(i+1), spec.coin)
					ord = lastTrade
				}
				seedEpochOrder(storage, ord, idx, epochDur)
				leftover = append(leftover, ord)
			}

			startupEpoch, err := mkt.submitMarketStarted(context.Background())
			if err != nil {
				t.Fatalf("submitMarketStarted error: %v", err)
			}
			if tc.wantOpenEpoch && startupEpoch == 0 {
				t.Fatalf("startup epoch = 0, want nonzero")
			}
			if len(storage.marketStartedUpdates) != 1 {
				t.Fatalf("market started updates = %d, want 1", len(storage.marketStartedUpdates))
			}
			requireEpochRevokeSet(t, storage.marketStartedUpdates[0], leftover)
			requireStartupResumedNothing(t, mkt, storage)

			mkt.epochMtx.RLock()
			currentEpoch := mkt.currentEpoch
			mkt.epochMtx.RUnlock()
			if (currentEpoch != nil) != tc.wantOpenEpoch {
				t.Fatalf("current epoch = %v, want opened %t", currentEpoch, tc.wantOpenEpoch)
			}
			if got := mkt.lifecycleFinalizingSuspend(); got != tc.pendingSuspend {
				t.Fatalf("finalizing suspend = %t, want %t", got, tc.pendingSuspend)
			}
		})
	}
}

// requireEpochRevokeSet checks that the event's epoch revokes cover exactly
// the given orders, sorted by ID, all with the penalty-free reason.
func requireEpochRevokeSet(t *testing.T, update *db.MarketStartedUpdate, want []order.Order) {
	t.Helper()
	if len(update.EpochRevokes) != len(want) {
		t.Fatalf("epoch revokes = %d, want %d", len(update.EpochRevokes), len(want))
	}
	revokedIDs := make(map[order.OrderID]bool, len(update.EpochRevokes))
	for i, revoke := range update.EpochRevokes {
		if revoke.Reason != meshevents.StartupOrderRevokeReasonEpochAbandoned {
			t.Fatalf("epoch revoke %d reason = %d, want %d", i,
				revoke.Reason, meshevents.StartupOrderRevokeReasonEpochAbandoned)
		}
		oid := revoke.Order.ID()
		revokedIDs[oid] = true
		if i > 0 {
			prev := update.EpochRevokes[i-1].Order.ID()
			if string(prev[:]) >= string(oid[:]) {
				t.Fatalf("epoch revokes are not sorted by order ID")
			}
		}
	}
	for _, ord := range want {
		if !revokedIDs[ord.ID()] {
			t.Fatalf("epoch order %v missing from epoch revokes", ord.ID())
		}
	}
}

// requireStartupResumedNothing checks that startup resumed nothing: no
// preimage collection or epoch processing ran, and market epoch memory is
// empty.
func requireStartupResumedNothing(t *testing.T, mkt *Market, storage *TArchivist) {
	t.Helper()
	if len(storage.epochProcessed) != 0 {
		t.Fatalf("startup ran the epoch pipeline: processed=%d", len(storage.epochProcessed))
	}
	mkt.epochMtx.RLock()
	memCount := len(mkt.epochOrders)
	mkt.epochMtx.RUnlock()
	if memCount != 0 {
		t.Fatalf("epoch memory orders = %d, want 0", memCount)
	}
}
