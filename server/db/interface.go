// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/meshevents"
)

// EpochResults represents the outcome of epoch order processing, including
// preimage collection, and computation of commitment checksum and shuffle seed.
// MatchTime is the time at which order matching is executed.
type EpochResults struct {
	MktBase, MktQuote uint32
	Idx               int64
	Dur               int64
	MatchTime         int64
	CSum              []byte
	Seed              []byte
	OrdersRevealed    []order.OrderID
	OrdersMissed      []order.OrderID
	MatchVolume       uint64
	QuoteVolume       uint64
	BookBuys          uint64
	BookBuys5         uint64
	BookBuys25        uint64
	BookSells         uint64
	BookSells5        uint64
	BookSells25       uint64
	HighRate          uint64
	LowRate           uint64
	StartRate         uint64
	EndRate           uint64
}

// OrderStatus is the current status of an order.
type OrderStatus struct {
	ID     order.OrderID
	Status order.OrderStatus
}

// StartupOrderRevoke identifies an order revoked during market startup. The
// reason is the wire-level meshevents.StartupOrderRevokeReason carried by the
// market_started and market_lifecycle events.
type StartupOrderRevoke struct {
	Order  order.Order
	Reason meshevents.StartupOrderRevokeReason
}

// DEXArchivist is composed of lifecycle methods plus storage capability
// interfaces.
type DEXArchivist interface {
	// LastErr should returns any fatal or unexpected error encountered by the
	// archivist backend. This may be used to check if the database had an
	// unrecoverable error (disconnect, etc.).
	LastErr() error

	// Fatal provides select semantics like Context.Done when there is a fatal
	// backend error. Use LastErr to get the error.
	Fatal() <-chan struct{}

	// Close should gracefully shutdown the backend, returning when complete.
	Close() error

	// LastEpochRate gets the EndRate of the last EpochResults inserted for the
	// market. If the database is empty, no error and a rate of zero are
	// returned.
	LastEpochRate(b, q uint32) (uint64, error)

	// LoadEpochStats reads all market epoch history from the database.
	LoadEpochStats(uint32, uint32, []*candles.Cache) error
	LastCandleEndStamp(base, quote uint32, candleDur uint64) (uint64, error)
	InsertCandles(base, quote uint32, dur uint64, cs []*candles.Candle) error

	OrderArchiver
	AccountArchiver
	MatchArchiver
	SwapArchiver
	ReputationArchiver
	EventLogReader
	SnapshotStore
	EventSourcedStateChecker
}

// OrderArchiver is the interface required for storage and retrieval of all
// order data.
type OrderArchiver interface {
	// Order retrieves an order with the given OrderID, stored for the market
	// specified by the given base and quote assets.
	Order(oid order.OrderID, base, quote uint32) (order.Order, order.OrderStatus, error)

	// OrdersWithCommit searches a market's trade and cancel orders for the
	// given commitment. Active orders are searched unconditionally; archived
	// orders only when accepted at or after archivedCutoff. Commitment reuse
	// is legal across archived lives, so several rows can match. An empty
	// result with a nil error means no matching row.
	OrdersWithCommit(ctx context.Context, base, quote uint32, commit order.Commitment, archivedCutoff time.Time) ([]CommitOrder, error)

	// BookOrders returns all book orders for a market.
	BookOrders(base, quote uint32) ([]*order.LimitOrder, error)

	// EpochOrders returns all epoch orders for a market.
	EpochOrders(base, quote uint32) ([]order.Order, error)

	// UserOrderStatuses retrieves the statuses and filled amounts of the orders
	// with the provided order IDs for the given account in the market specified
	// by a base and quote asset.
	// The number and ordering of the returned statuses is not necessarily the
	// same as the number and ordering of the provided order IDs. It is not an
	// error if any or all of the provided order IDs cannot be found for the
	// given account in the specified market.
	UserOrderStatuses(aid account.AccountID, base, quote uint32, oids []order.OrderID) ([]*OrderStatus, error)

	// ActiveUserOrderStatuses retrieves the statuses and filled amounts of all
	// active orders for a user across all markets.
	ActiveUserOrderStatuses(aid account.AccountID) ([]*OrderStatus, error)

	// OrderStatus gets the status, ID, and filled amount of the given order.
	OrderStatus(order.Order) (order.OrderStatus, order.OrderType, int64, error)

	// ApplyOrderAcceptedEvent is called from the mesh event applier on every
	// node. It projects an already-decided order_accepted onto this node's
	// DB and event log.
	ApplyOrderAcceptedEvent(ctx context.Context, meta *EventLogMeta, update *OrderAcceptedUpdate) (*EventLogEntry, error)

	// ApplyMarketStartedEvent is called from the mesh event applier on every
	// node. It projects an already-decided market_started onto this node's
	// DB and event log.
	ApplyMarketStartedEvent(ctx context.Context, meta *EventLogMeta, update *MarketStartedUpdate) (*EventLogEntry, error)

	// MarketLifecycle retrieves the durable lifecycle projection for a market.
	MarketLifecycle(market string) (*MarketLifecycle, error)

	// ApplyMarketLifecycleEvent is called from the mesh event applier on every
	// node. It projects an already-decided market_lifecycle onto this node's
	// DB and event log.
	ApplyMarketLifecycleEvent(ctx context.Context, meta *EventLogMeta, update *MarketLifecycleUpdate) (*MarketLifecycleApplyResult, error)

	// ApplyAdvanceEpochEvent is called from the mesh event applier on every
	// node. It projects an already-decided advance_epoch onto this node's DB
	// and event log.
	ApplyAdvanceEpochEvent(ctx context.Context, meta *EventLogMeta, update *AdvanceEpochUpdate) (*EventLogEntry, error)

	// ApplyEpochProcessedEvent is called from the mesh event applier on every
	// node. It projects an already-decided epoch_processed (preimage outcomes
	// and match results) onto this node's DB and event log.
	ApplyEpochProcessedEvent(ctx context.Context, meta *EventLogMeta, policy *ReputationOutcomePolicy, update *EpochProcessedUpdate) (*EventLogEntry, error)

	// ApplySuspendedCancelEvent is called from the mesh event applier on every
	// node. It projects an already-decided suspended_cancel onto this node's
	// DB and event log.
	ApplySuspendedCancelEvent(ctx context.Context, meta *EventLogMeta, update *SuspendedCancelUpdate) (*SuspendedCancelApplyResult, error)

	// ApplyOrdersRevokedEvent is called from the mesh event applier on every
	// node. It revokes booked standing limits with a generated cancel and a
	// neutral order outcome. Server revocations never count toward the
	// owner's cancellation rate. Non-booked targets error.
	ApplyOrdersRevokedEvent(ctx context.Context, meta *EventLogMeta, policy *ReputationOutcomePolicy, update *OrdersRevokedUpdate) (*EventLogEntry, error)
}

// Account holds data returned by Accounts.
type Account struct {
	AccountID account.AccountID `json:"accountid"`
	Pubkey    dex.Bytes         `json:"pubkey"`
}

// Bond represents a time-locked fidelity bond posted by a user. Rows
// must never be pruned: UserReputationAt still needs expired bonds when
// replaying at a past as-of.
type Bond struct {
	Version  uint16
	AssetID  uint32
	CoinID   []byte
	Amount   int64
	Strength uint32 // Amount / <bond increment at time of acceptance>
	LockTime int64

	// Will we need to store asset-specific data, like the redeem script for
	// UTXO assets or a contract address or bond key for account assets? Or will
	// that info be conveyed by Version and CoinID?
	//
	// Data []byte
}

// AccountArchiver is the interface required for storage and retrieval of all
// account data.
type AccountArchiver interface {
	// Account retrieves the account for the given ID. A nil account with a nil
	// error means unknown; a non-nil error means existence could not be
	// determined and must not be treated as unknown. Bonds are active when
	// lockTime >= lockTimeThresh (typically time.Now().Add(bondExpiry)).
	Account(acctID account.AccountID, lockTimeThresh time.Time) (acct *account.Account, activeBonds []*Bond, err error)

	// ApplyBondPostedEvent is called from the mesh event applier on every
	// node. It projects an already-decided bond_posted onto this node's DB
	// and event log: creates the account if missing, stores the bond, and
	// for a prepaid bond consumes the matching token. Same-account
	// duplicates are already-applied; another account's duplicate key errors.
	ApplyBondPostedEvent(ctx context.Context, meta *EventLogMeta, update *BondPostedUpdate) (*BondPostedResult, error)

	// ApplyPrepaidBondsCreatedEvent is called from the mesh event applier on
	// every node. It stores the prepaid-bond tokens and the event-log row.
	ApplyPrepaidBondsCreatedEvent(ctx context.Context, meta *EventLogMeta, event *meshevents.PrepaidBondsCreatedEvent) (*EventLogEntry, error)

	FetchPrepaidBond(bondCoinID []byte) (strength uint32, lockTime int64, err error)

	// AccountInfo returns data for an account.
	AccountInfo(account.AccountID) (*Account, error)
}

type CommitOrder struct {
	Order  order.Order
	Status order.OrderStatus
}

// MatchData represents an order pair match, but with just the order IDs instead
// of the full orders. The actual orders may be retrieved by ID.
type MatchData struct {
	ID        order.MatchID
	Taker     order.OrderID
	TakerAcct account.AccountID
	// Deprecated: TakerAddr is the order-level address. Use TakerSwapAddr
	// for contract recipients.
	TakerAddr string
	TakerSell bool
	Maker     order.OrderID
	MakerAcct account.AccountID
	// Deprecated: MakerAddr is the order-level address. Use MakerSwapAddr
	// for contract recipients.
	MakerAddr string
	Epoch     order.EpochID
	Quantity  uint64
	Rate      uint64
	BaseRate  uint64
	QuoteRate uint64
	Active    bool              // match negotiation in progress, not yet completed or failed
	Status    order.MatchStatus // note that failed swaps, where Active=false, can have any status
	// MakerSwapAddr and TakerSwapAddr are per-match swap addresses from
	// each party's match acknowledgement. These are the addresses used for
	// swap contracts.
	MakerSwapAddr string
	TakerSwapAddr string
}

// MatchDataWithCoins pairs MatchData (embedded) with the encode swap and redeem
// coin IDs blobs for both maker and taker.
type MatchDataWithCoins struct {
	MatchData
	MakerSwapCoin   []byte
	MakerRedeemCoin []byte
	TakerSwapCoin   []byte
	TakerRedeemCoin []byte
}

// MatchStatus is the current status of a match, its known contracts and coin
// IDs, and its secret, if known.
type MatchStatus struct {
	ID            order.MatchID
	Status        order.MatchStatus
	MakerContract []byte
	TakerContract []byte
	MakerSwap     []byte
	TakerSwap     []byte
	MakerRedeem   []byte
	TakerRedeem   []byte
	Secret        []byte
	Active        bool
	TakerSell     bool
	IsTaker       bool
	IsMaker       bool
}

// SwapData contains the data generated by the clients during swap negotiation.
type SwapData struct {
	SigMatchAckMaker []byte
	SigMatchAckTaker []byte
	// MakerSwapAddr and TakerSwapAddr are per-match swap addresses provided
	// by each party in their match acknowledgement. These are used instead of
	// the order-level addresses to ensure each match has a unique contract.
	MakerSwapAddr   string
	TakerSwapAddr   string
	ContractA       []byte // contains the secret hash used by both parties
	ContractACoinID []byte
	ContractATime   int64
	ContractAAckSig []byte // B's signature of contract A data
	ContractB       []byte
	ContractBCoinID []byte
	ContractBTime   int64
	ContractBAckSig []byte // A's signature of contract B data
	RedeemACoinID   []byte
	RedeemASecret   []byte // the secret revealed in A's redeem, also used in B's redeem
	RedeemATime     int64
	RedeemAAckSig   []byte // B's signature of redeem A data
	RedeemBCoinID   []byte
	RedeemBTime     int64
}

// SwapDataFull combines a MatchData, SwapData, and the Base/Quote asset IDs.
type SwapDataFull struct {
	Base, Quote uint32
	*MatchData
	*SwapData
}

// MarketMatchID designates a MatchID for a certain market by the market's
// base-quote asset IDs.
type MarketMatchID struct {
	order.MatchID
	Base, Quote uint32 // market
}

type BondPostedUpdate struct {
	Acct *account.Account
	Bond *Bond
}

type BondPostedResult struct {
	BondAdded bool
	Log       *EventLogEntry
}

type OrderAcceptedUpdate struct {
	Order    order.Order
	EpochIdx int64
	EpochDur int64
	EpochGap int32
}

type MarketState int16

const (
	MarketStateRunning MarketState = iota + 1
	MarketStateSuspended
)

type MarketPendingAction int16

const (
	MarketPendingNone MarketPendingAction = iota
	MarketPendingSuspend
	MarketPendingSuspendDrain
	MarketPendingResume
)

type MarketLifecycleAction int16

const (
	MarketLifecycleActionInvalid MarketLifecycleAction = iota
	MarketLifecycleActionScheduleSuspend
	MarketLifecycleActionSuspend
	MarketLifecycleActionScheduleResume
	MarketLifecycleActionResume
)

// MarketLifecycle is the durable suspend/resume/epoch-cursor row projected
// by market_started, market_lifecycle, and advance_epoch events.
type MarketLifecycle struct {
	Market          string
	State           MarketState
	StartEpochIdx   int64
	StartEpochDur   int64
	FinalEpochIdx   int64
	FinalEpochDur   int64
	PendingAction   MarketPendingAction
	PendingEpochIdx int64
	PendingEpochDur int64
	PersistBook     *bool
	// ActiveEpochIdx is the current trading epoch for a running market,
	// and zero for when suspended or draining.
	ActiveEpochIdx int64
	// ProcessedEpochIdx is the latest epoch whose epoch_processed event has
	// applied. Closed epochs above it still await their close: up to
	// ActiveEpochIdx-1 while running, up to PendingEpochIdx while draining.
	ProcessedEpochIdx int64
	// RunParams are the log-pinned market parameters for the current run.
	RunParams meshevents.MarketRunParams
}

// SuspendTime is the wall-clock end of the pending final trading epoch. It is
// only meaningful while PendingAction is MarketPendingSuspend or
// MarketPendingSuspendDrain.
func (lc *MarketLifecycle) SuspendTime() time.Time {
	return time.UnixMilli((lc.PendingEpochIdx + 1) * lc.PendingEpochDur).UTC()
}

// ResumeTime is the wall-clock start of the pending resume epoch. It is only
// meaningful while PendingAction is MarketPendingResume.
func (lc *MarketLifecycle) ResumeTime() time.Time {
	return time.UnixMilli(lc.PendingEpochIdx * lc.PendingEpochDur).UTC()
}

// MarketLifecycleUpdate is a decoded market_lifecycle event.
type MarketLifecycleUpdate struct {
	Action MarketLifecycleAction
	Market string
	Base   uint32
	Quote  uint32
	// EpochIdx is the last trading epoch for suspend actions and the resume
	// epoch for resume actions.
	EpochIdx      int64
	EpochDur      int64
	PersistBook   *bool
	Timestamp     time.Time
	ResumeRevokes []*StartupOrderRevoke
	// RunParams re-pins the market run parameters; carried by resume only.
	RunParams *meshevents.MarketRunParams
}

// OpenEpochIdx is the first epoch a resume opens: the scheduled resume epoch,
// or the epoch at the master's decision time when the schedule already passed.
func (u *MarketLifecycleUpdate) OpenEpochIdx() int64 {
	if idx := u.Timestamp.UnixMilli() / u.EpochDur; idx > u.EpochIdx {
		return idx
	}
	return u.EpochIdx
}

// MarketLifecycleApplyResult is the stored outcome of a market_lifecycle event.
type MarketLifecycleApplyResult struct {
	Log           *EventLogEntry
	Lifecycle     *MarketLifecycle
	PurgeOrders   []order.OrderID
	ResumeRevokes []*StartupOrderRevoke
}

// MarketStartedUpdate is a decoded market_started event.
type MarketStartedUpdate struct {
	Market          string
	Base            uint32
	Quote           uint32
	CurrentEpochIdx int64
	EpochDur        int64
	RunParams       meshevents.MarketRunParams
	RevocationTime  time.Time
	BookedRevokes   []*StartupOrderRevoke
	// EpochRevokes must exactly match the active epoch orders, or apply rejects.
	EpochRevokes []*StartupOrderRevoke
}

// AdvanceEpochUpdate is the persistent state transition of an advance_epoch
// event. ClosedOrderIDs are the orders leaving the closed epoch.
type AdvanceEpochUpdate struct {
	Market         string
	ClosedEpochIdx int64
	OpenedEpochIdx int64
	EpochDur       int64
	ClosedOrderIDs []order.OrderID
}

// OrdersRevokedUpdate is the persistent state transition of an orders_revoked
// event. All orders must be booked standing limit orders. The revocation
// reason is the wire-level meshevents.OrderRevokeReason carried by the event.
type OrdersRevokedUpdate struct {
	Reason     meshevents.OrderRevokeReason
	RevokeTime time.Time
	Orders     []*order.LimitOrder
}

type ReputationOutcomePolicy struct {
	PreimageLimit       int
	MatchLimit          int
	OrderLimit          int
	FreeCancelThreshold int32
}

type PreimageMissUpdate struct {
	Order      order.Order
	RevokeTime time.Time
}

type PreimageRevealUpdate struct {
	Order    order.Order
	Preimage order.Preimage
}

type EpochProcessedUpdate struct {
	Epoch           *EpochResults
	Misses          []*PreimageMissUpdate
	Reveals         []*PreimageRevealUpdate
	TradesBooked    []*order.LimitOrder
	TradesPartial   []*order.LimitOrder
	TradesCompleted []order.Order
	TradesCanceled  []*order.LimitOrder
	TradesFailed    []order.Order
	CancelsFailed   []*order.CancelOrder
	CancelsExecuted []*order.CancelOrder
	Matches         []*order.Match
}

type SuspendedCancelUpdate struct {
	Market          string
	Base            uint32
	Quote           uint32
	Cancel          *order.CancelOrder
	TargetOrderID   order.OrderID
	TargetAccount   account.AccountID
	TargetSell      bool
	EpochIdx        int64
	EpochDur        int64
	FeeRateBase     uint64
	FeeRateQuote    uint64
	MatchServerTime time.Time
	Match           *order.Match
}

type SuspendedCancelApplyResult struct {
	Log         *EventLogEntry
	Cancel      *order.CancelOrder
	TargetOrder *order.LimitOrder
	Match       *order.Match
}

// MatchAck is a client acknowledgement of match data to persist for a match.
type MatchAck struct {
	MID     MarketMatchID
	Maker   bool
	Cancel  bool
	Sig     []byte
	Address string
}

type MatchAcksRecordedUpdate struct {
	Acks []*MatchAck
}

// AuditAck is a client acknowledgement of a counterparty's swap contract.
type AuditAck struct {
	MID   MarketMatchID
	Maker bool
	Sig   []byte
}

// RedemptionAck is a client acknowledgement of a counterparty's redemption.
type RedemptionAck struct {
	MID   MarketMatchID
	Maker bool
	Sig   []byte
}

// SwapContract is an on-chain swap contract to persist for a match.
type SwapContract struct {
	MID       MarketMatchID
	Maker     bool
	Contract  []byte
	CoinID    []byte
	Timestamp int64
}

// SwapRedemption is a redemption record to persist for a match.
type SwapRedemption struct {
	MID       MarketMatchID
	Maker     bool
	CoinID    []byte
	Secret    []byte
	Timestamp int64
}

type MatchFailureReason uint8

const (
	MatchFailureReasonInvalid MatchFailureReason = iota
	MatchFailureNoFaultNewlyMatched
	MatchFailureNoFaultMakerSwapCast
	MatchFailureNoFaultTakerSwapCast
	MatchFailureNoFaultMakerRedeemed
	MatchFailureMakerNoSwap
	MatchFailureTakerNoAddress
	MatchFailureTakerNoSwap
	MatchFailureMakerNoRedeem
	MatchFailureTakerNoRedeem
)

type MatchFailedUpdate struct {
	MID        MarketMatchID
	FailTimeMS int64
	Reason     MatchFailureReason
}

type ReputationForgivenResult struct {
	Forgiven bool
	Log      *EventLogEntry
}

const EventLogTipHashSize = sha256.Size

type EventLogMeta struct {
	// Seq is zero when storage should allocate the next durable event-log
	// sequence. It is non-zero when storage must enforce a master-assigned
	// sequence.
	Seq uint64
	// Event is the canonical mesh event payload stored in the event log.
	Event []byte
	// ExpectedTipHash is nil for master allocation. Slave and catch-up applies
	// set it so storage can verify the computed hash-chain tip before commit.
	ExpectedTipHash []byte
}

// EventLogEntry is a row in the event log.
type EventLogEntry struct {
	// Seq is the entry's sequence number. Stored entries start at 1 and
	// increase monotonically.
	Seq uint64
	// Kind identifies the type of the event.
	Kind string
	// Event is the encoded canonical mesh event payload.
	Event []byte
	// TxData is the encoded transaction data.
	TxData []byte
	// TipHash is the hash of the log through this entry.
	TipHash []byte
}

// SnapshotAnchorKind is the kind of the first entry in the event log that
// was initialized using a snapshot.
const SnapshotAnchorKind = "snapshot_anchor"

// MeshGenesisKind is the kind of the first entry in the event log of a database
// that was upgraded from a pre-mesh database. The seq of mesh genesis entries
// is always 1.
const MeshGenesisKind = "mesh_genesis"

// IsEventLogAnchorKind reports whether kind is a non-replayable event-log
// anchor.
func IsEventLogAnchorKind(kind string) bool {
	return kind == MeshGenesisKind || kind == SnapshotAnchorKind
}

// EventLogPosition identifies a position in the event log by sequence number
// and tip hash. A zero seq represents an empty log.
type EventLogPosition struct {
	Seq     uint64
	TipHash []byte
}

func (p *EventLogPosition) String() string {
	if p == nil {
		return "Position{nil}"
	}
	if p.Seq == 0 {
		return "Position{seq=0}"
	}
	return fmt.Sprintf("Position{seq=%d hash=%x}", p.Seq, p.TipHash)
}

// EventCommitUnknownError means the database could not confirm whether an
// event transaction committed.
type EventCommitUnknownError struct {
	Err error
}

func (e *EventCommitUnknownError) Error() string {
	if e == nil || e.Err == nil {
		return "event commit outcome unknown"
	}
	return fmt.Sprintf("event commit outcome unknown: %v", e.Err)
}

func (e *EventCommitUnknownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// EventLogDivergenceError means the tip hash at Seq differs from the expected
// hash.
type EventLogDivergenceError struct {
	Seq             uint64
	ExpectedTipHash []byte
	ActualTipHash   []byte
	Err             error
}

func (e *EventLogDivergenceError) Error() string {
	if e == nil {
		return "event log divergence"
	}
	if e.Err != nil {
		return fmt.Sprintf("event log divergence at seq %d: %v", e.Seq, e.Err)
	}
	return fmt.Sprintf("event log divergence at seq %d", e.Seq)
}

func (e *EventLogDivergenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SnapshotStore imports and exports a snapshot of the database state, required
// to sync a fresh database with a mesh peer.
type SnapshotStore interface {
	WriteSnapshot(ctx context.Context, w io.Writer) (*EventLogPosition, error)
	LoadSnapshot(ctx context.Context, r io.Reader) (*EventLogPosition, error)
}

// EventSourcedStateChecker probes and clears event-sourced tables.
type EventSourcedStateChecker interface {
	// HasNoEventSourcedState reports whether every event-sourced table is empty.
	HasNoEventSourcedState(ctx context.Context) (bool, error)
	// WipeEventSourcedState truncates the event log and all event projections.
	WipeEventSourcedState(ctx context.Context) error
}

// EventLogReader allows callers to read the event log.
type EventLogReader interface {
	// EventLogFrontier returns the latest entry's position, or a zero
	// position if the log is empty.
	EventLogFrontier(context.Context) (*EventLogPosition, error)

	// EventLogEntriesAfter returns up to limit entries with sequence numbers
	// greater than after, in increasing order.
	EventLogEntriesAfter(ctx context.Context, after uint64, limit int) ([]*EventLogEntry, error)
}

// MatchID constructs a MarketMatchID from an order.Match.
func MatchID(match *order.Match) MarketMatchID {
	return MarketMatchID{
		MatchID: match.ID(),
		Base:    match.Maker.BaseAsset, // same for taker's redeem as BaseAsset refers to the market
		Quote:   match.Maker.QuoteAsset,
	}
}

// MatchOutcome pairs an inactive match's status with a timestamp. In the case
// of a successful match for the user, this is when their redeem was received.
// In the case of an at-fault match failure for the user, this corresponds to
// the time of the previous match action. The previous action times are: match
// time, swap txn validated times, and initiator redeem validated time. Note
// that this does not directly correspond to match revocation times where
// inaction deadline references the time when the swap txns reach the required
// confirms. These times must match the reference times provided to the auth
// manager when registering new swap outcomes.
type MatchOutcome struct {
	Status      order.MatchStatus
	ID          order.MatchID
	Fail        bool // taker must reach MatchComplete, maker succeeds at MakerRedeemed
	Time        int64
	Value       uint64
	Base, Quote uint32 // the market
}

// MatchFail is a failed match and the effect on the user's score
type MatchFail struct {
	ID     order.MatchID
	Status order.MatchStatus
}

// MatchArchiver is the interface required for storage and retrieval of all
// match data.
type MatchArchiver interface {
	CompletedAndAtFaultMatchStats(aid account.AccountID, lastN int) ([]*MatchOutcome, error)
	UserMatchFails(aid account.AccountID, lastN int) ([]*MatchFail, error)
	AllActiveUserMatches(aid account.AccountID) ([]*MatchData, error)
	MarketMatches(base, quote uint32) ([]*MatchDataWithCoins, error)
	MarketMatchesStreaming(base, quote uint32, includeInactive bool, N int64, f func(*MatchDataWithCoins) error) (int, error)
	MatchStatuses(aid account.AccountID, base, quote uint32, matchIDs []order.MatchID) ([]*MatchStatus, error)
}

// SwapArchiver is the interface required for storage and retrieval of swap
// counterparty data.
//
// In the swap process, the counterparties are:
// - Initiator or party A on chain X. This is the maker in the DEX.
// - Participant or party B on chain Y. This is the taker in the DEX.
//
// For each match, a successful swap will generate the following data that must
// be stored:
//   - 5 client signatures. Both parties sign the data to acknowledge (1) the
//     match ack, and (2) the counterparty's contract script and contract
//     transaction. Plus the taker acks the maker's redemption transaction.
//   - 2 swap contracts and the associated transaction outputs (more generally,
//     coinIDs), one on each party's blockchain.
//   - 2 redemption transaction outputs (coinIDs).
//
// The event appliers that save this data are defined below in the order in
// which the data is expected from the parties.
type SwapArchiver interface {
	// ActiveSwaps loads the full details for all active swaps across all markets.
	ActiveSwaps() ([]*SwapDataFull, error)

	// SwapDataFullByID loads a match's row and swap data by match ID,
	// searching all markets, active or inactive. A nil result with a nil
	// error means no matching row.
	SwapDataFullByID(mid order.MatchID) (*SwapDataFull, error)

	// ApplyMatchAcksRecordedEvent is called from the mesh event applier on
	// every node. It projects already-decided match acks onto this node's DB
	// and event log.
	ApplyMatchAcksRecordedEvent(ctx context.Context, meta *EventLogMeta, update *MatchAcksRecordedUpdate) (*EventLogEntry, error)

	// ApplySwapContractRecordedEvent is called from the mesh event applier on
	// every node. It projects an already-decided swap contract onto this
	// node's DB and event log.
	ApplySwapContractRecordedEvent(ctx context.Context, meta *EventLogMeta, contract *SwapContract) (*EventLogEntry, error)

	// ApplyAuditAckRecordedEvent is called from the mesh event applier on
	// every node. It projects an already-decided contract audit ack onto
	// this node's DB and event log.
	ApplyAuditAckRecordedEvent(ctx context.Context, meta *EventLogMeta, ack *AuditAck) (*EventLogEntry, error)

	// ApplySwapRedemptionRecordedEvent is called from the mesh event applier
	// on every node. It projects an already-decided redemption onto this
	// node's DB and event log.
	ApplySwapRedemptionRecordedEvent(ctx context.Context, meta *EventLogMeta, policy *ReputationOutcomePolicy, redemption *SwapRedemption) (*EventLogEntry, error)

	// ApplyRedemptionAckRecordedEvent is called from the mesh event applier
	// on every node. Maker redemption acks have no persistent state beyond
	// the event-log row.
	ApplyRedemptionAckRecordedEvent(ctx context.Context, meta *EventLogMeta, ack *RedemptionAck) (*EventLogEntry, error)

	// ApplyMatchFailedEvent is called from the mesh event applier on every
	// node. It projects an already-decided match_failed onto this node's DB
	// and event log.
	ApplyMatchFailedEvent(ctx context.Context, meta *EventLogMeta, policy *ReputationOutcomePolicy, update *MatchFailedUpdate) (*EventLogEntry, error)
}

// ValidateOrder ensures that the order with the given status for the specified
// market is sensible. This function is in the database package because the
// concept of a valid order-status-market state is dependent on the semantics of
// order archival. The ServerTime may not be set yet, so the OrderID cannot be
// computed.
func ValidateOrder(ord order.Order, status order.OrderStatus, mkt *dex.MarketInfo) bool {
	// Orders with status OrderStatusUnknown should never reach the database.
	if status == order.OrderStatusUnknown {
		return false
	}

	// Bad MarketInfo!
	if mkt.Base == mkt.Quote {
		panic("MarketInfo specifies market with same base and quote assets")
	}

	return order.ValidateOrder(ord, status, mkt.LotSize) == nil
}

// EpochGapNA is a specifier for an epoch gap (epochs between limit order and
// cancel order) when such a designation doesn't apply in-context. For instance,
// revocations are treated in many places like cancel orders, but there is no
// reason to consider the epoch gap.
const EpochGapNA int32 = -1

// Reputation

// ReputationArchiver handles interactions with the reputation points table.
type ReputationArchiver interface {
	GetUserReputationData(ctx context.Context, user account.AccountID, pimgSz, matchSz, orderSz int) ([]*PreimageOutcome, []*MatchResult, []*OrderOutcome, error)
	// ApplyReputationForgivenEvent is called from the mesh event applier on
	// every node. It projects an already-decided reputation_forgiven onto
	// this node's DB and event log.
	ApplyReputationForgivenEvent(ctx context.Context, meta *EventLogMeta, event *meshevents.ReputationForgivenEvent) (*ReputationForgivenResult, error)
	// SetReputationInputsListener registers the single listener notified after
	// any commit that may have changed a user's reputation inputs, including
	// commits whose outcome is unknown. Must not block. Registering a second
	// listener panics.
	SetReputationInputsListener(func(users ...account.AccountID))
}

// OutcomeClass is the type of interaction for which the user's reputation
// score is affected.
type OutcomeClass int16

const (
	OutcomeClassInvalid OutcomeClass = iota
	OutcomeClassPreimage
	OutcomeClassOrder
	OutcomeClassMatch
)

type Outcome int16

const (
	OutcomeInvalid Outcome = iota
	OutcomeForgiven
	// Match Outcomes
	OutcomeSwapSuccess
	OutcomeNoSwapAsMaker
	OutcomeNoSwapAsTaker
	OutcomeNoRedeemAsMaker
	OutcomeNoRedeemAsTaker
	// Preimage
	OutcomePreimageSuccess
	OutcomePreimageMiss
	// Order cancel/complete
	OutcomeOrderComplete
	OutcomeOrderCanceled
	// OutcomeNoAddrAsTaker is recorded when a match times out at
	// NewlyMatched because the taker failed to provide their per-match
	// swap address in time, preventing the maker from broadcasting.
	OutcomeNoAddrAsTaker
)

func (o Outcome) String() string {
	switch o {
	case OutcomeForgiven:
		return "forgiveness"
	case OutcomePreimageMiss:
		return "preimage miss"
	case OutcomePreimageSuccess:
		return "preimage success"
	case OutcomeSwapSuccess:
		return "swap success"
	case OutcomeNoSwapAsMaker:
		return "no swap as maker"
	case OutcomeNoSwapAsTaker:
		return "no swap as taker"
	case OutcomeNoRedeemAsMaker:
		return "no redeem as maker"
	case OutcomeNoRedeemAsTaker:
		return "no redeem as taker"
	case OutcomeOrderCanceled:
		return "excessive cancels"
	case OutcomeOrderComplete:
		return "order complete"
	case OutcomeNoAddrAsTaker:
		return "no address as taker"
	case OutcomeInvalid:
		return "invalid violation"
	default:
		return "unknown violation"
	}
}

type Outcomer interface {
	Outcome() Outcome
	ID() int64
}

// PreimageOutcome is the outcome of preimage collection for an order.
type PreimageOutcome struct {
	DBID    int64
	OrderID order.OrderID
	Miss    bool
}

func (p *PreimageOutcome) Outcome() Outcome {
	if p.Miss {
		return OutcomePreimageMiss
	}
	return OutcomePreimageSuccess
}

func (p *PreimageOutcome) ID() int64 {
	return p.DBID
}

// MatchResult is the outcome of a swap.
type MatchResult struct {
	DBID         int64
	MatchID      order.MatchID
	MatchOutcome Outcome
}

func (m *MatchResult) Outcome() Outcome {
	return m.MatchOutcome
}

func (m *MatchResult) ID() int64 {
	return m.DBID
}

// OrderOutcome is the outcome of an order, either canceled before the epoch gap
// or not.
type OrderOutcome struct {
	DBID     int64
	OrderID  order.OrderID
	Canceled bool
}

func (o *OrderOutcome) Outcome() Outcome {
	if o.Canceled {
		return OutcomeOrderCanceled
	}
	return OutcomeOrderComplete
}

func (o *OrderOutcome) ID() int64 {
	return o.DBID
}
