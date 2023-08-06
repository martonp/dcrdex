// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
	"go.etcd.io/bbolt"
)

const (
	eventTypeOrdersPlaced   = "OrderPlaced"
	eventTypeOrderCancelled = "OrderCanceled"
	eventTypeMatch          = "Match"
	eventTypeFeesConfirmed  = "FeesConfirmed"
)

func versionedBytes(v byte) encode.BuildyBytes {
	return encode.BuildyBytes{v}
}

// An Event is a record of an event that occurred during the run of a
// market maker bot.
type Event struct {
	Type        string      `json:"type"`
	TimeStamp   int64       `json:"timeStamp"`
	OrderIDs    []dex.Bytes `json:"orderIDs"`
	MatchID     dex.Bytes   `json:"matchIDs"`
	FundingTxID dex.Bytes   `json:"fundingTxID"`
	BaseDelta   int64       `json:"baseDelta"`
	QuoteDelta  int64       `json:"quoteDelta"`
	BaseFees    uint64      `json:"baseFees"`
	QuoteFees   uint64      `json:"quoteFees"`
}

func encodeOrderIDs(orderIDs []dex.Bytes) []byte {
	bb := versionedBytes(0)
	for _, orderID := range orderIDs {
		bb = bb.AddData(orderID)
	}
	return bb
}

func decodeOrderIDs(b []byte) ([]dex.Bytes, error) {
	_, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	orderIDs := make([]dex.Bytes, 0, len(pushes))
	for _, push := range pushes {
		orderIDs = append(orderIDs, push)
	}
	return orderIDs, nil
}

func (e *Event) encode() []byte {
	return versionedBytes(0).
		AddData([]byte(e.Type)).
		AddData(itob(uint64(e.TimeStamp))).
		AddData(encodeOrderIDs(e.OrderIDs)).
		AddData(e.MatchID).
		AddData(e.FundingTxID).
		AddData(itob(uint64(e.BaseDelta))).
		AddData(itob(uint64(e.QuoteDelta))).
		AddData(itob(e.BaseFees)).
		AddData(itob(e.QuoteFees))
}

func decodeEvent(b []byte) (*Event, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	if ver != 0 {
		return nil, fmt.Errorf("unknown version %d", ver)
	}
	if len(pushes) != 9 {
		return nil, fmt.Errorf("expected 9 pushes, got %d", len(pushes))
	}

	timeStamp := int64(binary.BigEndian.Uint64(pushes[1]))
	orderIDs, err := decodeOrderIDs(pushes[2])
	if err != nil {
		return nil, err
	}
	baseDelta := int64(binary.BigEndian.Uint64(pushes[5]))
	quoteDelta := int64(binary.BigEndian.Uint64(pushes[6]))
	baseFees := binary.BigEndian.Uint64(pushes[7])
	quoteFees := binary.BigEndian.Uint64(pushes[8])

	return &Event{
		Type:        string(pushes[0]),
		TimeStamp:   timeStamp,
		OrderIDs:    orderIDs,
		MatchID:     pushes[3],
		FundingTxID: pushes[4],
		BaseDelta:   baseDelta,
		QuoteDelta:  quoteDelta,
		BaseFees:    baseFees,
		QuoteFees:   quoteFees,
	}, nil
}

// RunStats represents the overall stats of a run of a market maker bot.
type RunStats struct {
	BaseChange  int64  `json:"baseChange"`
	QuoteChange int64  `json:"quoteChange"`
	BaseFees    uint64 `json:"baseFees"`
	QuoteFees   uint64 `json:"quoteFees"`
	// FiatGainLoss is the fiat value of the changes in the quote asset
	// and base asset, minus the fees paid. It represents the change in
	// fiat value this bot produced for the user compared to not running
	// the bot at all.``
	FiatGainLoss float64 `json:"fiatGainLoss"`
}

func (rs *RunStats) encode() []byte {
	return versionedBytes(0).
		AddData(itob(uint64(rs.BaseChange))).
		AddData(itob(uint64(rs.QuoteChange))).
		AddData(itob(rs.BaseFees)).
		AddData(itob(rs.QuoteFees)).
		AddData(itob(uint64(fiatToInt64(rs.FiatGainLoss))))
}

func decodeRunStats(b []byte) (*RunStats, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	if ver != 0 {
		return nil, fmt.Errorf("unknown version %d", ver)
	}
	if len(pushes) != 5 {
		return nil, fmt.Errorf("expected 5 pushes, got %d", len(pushes))
	}

	return &RunStats{
		BaseChange:   int64(binary.BigEndian.Uint64(pushes[0])),
		QuoteChange:  int64(binary.BigEndian.Uint64(pushes[1])),
		BaseFees:     binary.BigEndian.Uint64(pushes[2]),
		QuoteFees:    binary.BigEndian.Uint64(pushes[3]),
		FiatGainLoss: uintToFiat(int64(binary.BigEndian.Uint64(pushes[4]))),
	}, nil
}

// RunOverview contains the config used to run a market maker bot
// and the resulting stats it produced.
type RunOverview struct {
	Cfg   *BotConfig `json:"config"`
	Stats *RunStats  `json:"stats"`
}

// eventLogDB is an interface for a db that stores events and stats for a
// runs of the market maker bot. Each run is identified by its start time.
// The db stores the configs used for each run, the fiat rates at the time
// of the run, and all the events that occurred during the run.
type eventLogDB interface {
	// storeNewRun should be called when the market maker is started. It stores
	// the configs used for the run, and the initial fiat rates.
	storeNewRun(startTime int64, cfgs []*BotConfig, fiatRates map[uint32]float64) error
	// storeEvent should be called when an event occurs during the run. runFinalized is a
	// boolean flag that indicates whether all the orders that have been placed have either
	// been canceled without matches, if there were matches, then all fees were confirmed.
	// If runFinalized is false when retrieving data, the logs can be completed by querying
	// the fees paid for the orders that were not fully logged.
	storeEvent(startTime int64, market *MarketWithHost, event *Event, updatedStats *RunStats, runFinalized bool) error
	// storeFiatRates should be periodically updated during a run to keep a log of the fiat
	// rates during the run.
	storeFiatRates(startTime int64, rates map[uint32]float64) error
	// allRuns returns the start times of all past market maker runs.
	allRuns() ([]int64, error)
	// runOverviews returns the stats and configs for all the markets that were run.
	runOverviews(startTime int64) (map[string]*RunOverview, error)
	// fiatRates returns the last fiat rates stored for a run.
	fiatRates(startTime int64) (map[uint32]float64, error)
	// runConfig returns the configuration for a specific market on a specific run.
	runConfig(startTime int64, market *MarketWithHost) (*BotConfig, error)
	// runLogs returns all the events and stats for a specific market on a specific run.
	runLogs(startTime int64, market *MarketWithHost) (events []*Event, stats *RunStats, runFinalized bool, err error)
}

// boltEventLogDB implements eventLogDB. Each run, identified by its start time,
// has a top level bucket.
type boltEventLogDB struct {
	db *bbolt.DB
}

var _ eventLogDB = (*boltEventLogDB)(nil)

var (
	// top level below start time bucket
	marketsBucket   = []byte("markets")
	fiatRatesBucket = []byte("fiat_rates")

	// within each market bucket
	cfgKey       = []byte("config")
	eventsBucket = []byte("events")
	statsKey     = []byte("stats")
	finalizedKey = []byte("finalized")
)

func itob(i uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, i)
	return b
}

func btob(b bool) []byte {
	if b {
		return []byte{1}
	}
	return []byte{0}
}

func boolFromBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return b[0] == 1
}

func fiatToInt64(f float64) int64 {
	return int64(math.Round(f * 100))
}

func uintToFiat(i int64) float64 {
	return float64(i) / 100
}

// bucketPutter enables chained calls to (*bbolt.Bucket).Put with error
// deferment.
type bucketPutter struct {
	bucket *bbolt.Bucket
	putErr error
}

// newBucketPutter is a constructor for a bucketPutter.
func newBucketPutter(bkt *bbolt.Bucket) *bucketPutter {
	return &bucketPutter{bucket: bkt}
}

// put calls Put on the underlying bucket. If an error has been encountered in a
// previous call to push, nothing is done. put enables the *bucketPutter to
// enable chaining.
func (bp *bucketPutter) put(k, v []byte) *bucketPutter {
	if bp.putErr != nil {
		return bp
	}
	bp.putErr = bp.bucket.Put(k, v)
	return bp
}

// Return any push error encountered.
func (bp *bucketPutter) err() error {
	return bp.putErr
}

func (db *boltEventLogDB) storeNewRun(startTime int64, cfgs []*BotConfig, fiatRates map[uint32]float64) error {
	return db.db.Update(func(tx *bbolt.Tx) error {
		runBucket, err := tx.CreateBucket(itob(uint64(startTime)))
		if err != nil {
			return err
		}

		fiatBucket, err := runBucket.CreateBucket(fiatRatesBucket)
		if err != nil {
			return err
		}
		for assetID, rate := range fiatRates {
			err = fiatBucket.Put(itob(uint64(assetID)), itob(uint64(fiatToInt64(rate))))
			if err != nil {
				return err
			}
		}

		marketsBucket, err := runBucket.CreateBucket(marketsBucket)
		if err != nil {
			return err
		}

		for _, cfg := range cfgs {
			mktWithHost := &MarketWithHost{
				Host:  cfg.Host,
				Base:  cfg.BaseAsset,
				Quote: cfg.QuoteAsset,
			}
			botBucket, err := marketsBucket.CreateBucket(mktWithHost.Serialize())
			if err != nil {
				return err
			}
			cfgBytes, err := json.Marshal(cfg)
			if err != nil {
				return err
			}
			if newBucketPutter(botBucket).
				put(cfgKey, cfgBytes).
				put(finalizedKey, btob(true)).
				put(statsKey, (&RunStats{}).encode()).
				err() != nil {
				return err
			}
		}

		return nil
	})
}

func (db *boltEventLogDB) storeEvent(startTime int64, mkt *MarketWithHost, event *Event, updatedStats *RunStats, runFinalized bool) error {
	return db.db.Update(func(tx *bbolt.Tx) error {
		runBucket, err := tx.CreateBucketIfNotExists(itob(uint64(startTime)))
		if err != nil {
			return err
		}

		marketsBucket, err := runBucket.CreateBucketIfNotExists(marketsBucket)
		if err != nil {
			return err
		}
		botBucket, err := marketsBucket.CreateBucketIfNotExists(mkt.Serialize())
		if err != nil {
			return err
		}
		eventsBucket, err := botBucket.CreateBucketIfNotExists(eventsBucket)
		if err != nil {
			return err
		}
		// This returns an error only if the Tx is closed or not writeable.
		// That can't happen in an Update() call so I ignore the error check.
		eventID, _ := eventsBucket.NextSequence()
		eventsBucket.Put(itob(eventID), event.encode())

		botBucket.Put(finalizedKey, btob(runFinalized))
		botBucket.Put(statsKey, updatedStats.encode())

		return nil
	})
}

func (db *boltEventLogDB) runConfig(startTime int64, mkt *MarketWithHost) (*BotConfig, error) {
	var cfg *BotConfig
	err := db.db.View(func(tx *bbolt.Tx) error {
		runBucket := tx.Bucket(itob(uint64(startTime)))
		if runBucket == nil {
			return fmt.Errorf("no run bucket for %d", startTime)
		}

		marketsBucket := runBucket.Bucket(marketsBucket)
		if marketsBucket == nil {
			return fmt.Errorf("no markets bucket for %d", startTime)
		}

		botBucket := marketsBucket.Bucket(mkt.Serialize())
		if botBucket == nil {
			return fmt.Errorf("no bot bucket for %s", mkt.String())
		}

		cfgBytes := botBucket.Get(cfgKey)
		if cfgBytes == nil {
			return fmt.Errorf("no config found for %s", mkt.String())
		}

		return json.Unmarshal(cfgBytes, &cfg)
	})
	return cfg, err
}

func (db *boltEventLogDB) runLogs(startTime int64, mkt *MarketWithHost) ([]*Event, *RunStats, bool, error) {
	var events []*Event
	var stats *RunStats
	var runFinalized bool

	err := db.db.View(func(tx *bbolt.Tx) error {
		runBucket := tx.Bucket(itob(uint64(startTime)))
		if runBucket == nil {
			return fmt.Errorf("no run bucket for %d", startTime)
		}

		marketsBucket := runBucket.Bucket(marketsBucket)
		if marketsBucket == nil {
			return fmt.Errorf("no markets bucket for %d", startTime)
		}

		botBucket := marketsBucket.Bucket(mkt.Serialize())
		if botBucket == nil {
			return fmt.Errorf("no bot bucket for %s", mkt.String())
		}

		statsB := botBucket.Get(statsKey)
		if statsB == nil {
			return fmt.Errorf("no stats found for %s", mkt.String())
		}
		var err error
		stats, err = decodeRunStats(statsB)
		if err != nil {
			return err
		}

		runFinalized = boolFromBytes(botBucket.Get(finalizedKey))

		eventsBucket := botBucket.Bucket(eventsBucket)
		if eventsBucket == nil {
			return nil
		}
		numEvents := eventsBucket.Sequence()
		events = make([]*Event, 0, numEvents)
		return eventsBucket.ForEach(func(_, v []byte) error {
			event, err := decodeEvent(v)
			if err != nil {
				return err
			}
			events = append(events, event)
			return nil
		})
	})
	if err != nil {
		return nil, nil, false, err
	}

	return events, stats, runFinalized, nil
}

func (db *boltEventLogDB) storeFiatRates(startTime int64, rates map[uint32]float64) error {
	return db.db.Update(func(tx *bbolt.Tx) error {
		runBucket, err := tx.CreateBucketIfNotExists(itob(uint64(startTime)))
		if err != nil {
			return err
		}

		fiatBucket, err := runBucket.CreateBucketIfNotExists(fiatRatesBucket)
		if err != nil {
			return err
		}

		for assetID, rate := range rates {
			err = fiatBucket.Put(itob(uint64(assetID)), itob(uint64(fiatToInt64(rate))))
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func (db *boltEventLogDB) fiatRates(startTime int64) (map[uint32]float64, error) {
	rates := make(map[uint32]float64)
	err := db.db.View(func(tx *bbolt.Tx) error {
		runBucket := tx.Bucket(itob(uint64(startTime)))
		if runBucket == nil {
			return fmt.Errorf("no run bucket for %d", startTime)
		}

		fiatBucket := runBucket.Bucket(fiatRatesBucket)
		if fiatBucket == nil {
			return nil
		}

		return fiatBucket.ForEach(func(k, v []byte) error {
			assetID := binary.BigEndian.Uint64(k)
			rates[uint32(assetID)] = uintToFiat(int64(binary.BigEndian.Uint64(v)))
			return nil
		})
	})

	return rates, err
}

func (db *boltEventLogDB) runOverviews(startTime int64) (map[string]*RunOverview, error) {
	overviews := map[string]*RunOverview{}
	err := db.db.View(func(tx *bbolt.Tx) error {
		runBucket := tx.Bucket(itob(uint64(startTime)))
		if runBucket == nil {
			return fmt.Errorf("no run bucket for %d", startTime)
		}

		marketsBucket := runBucket.Bucket(marketsBucket)

		return marketsBucket.ForEach(func(k, _ []byte) error {
			mkt := new(MarketWithHost)
			err := mkt.Deserialize(k)
			if err != nil {
				return err
			}
			botBucket := marketsBucket.Bucket(k)
			if botBucket == nil {
				return nil
			}

			statsB := botBucket.Get(statsKey)
			if statsB == nil {
				return fmt.Errorf("no stats found for %s", mkt.String())
			}
			stats, err := decodeRunStats(statsB)
			if err != nil {
				return err
			}

			cfgBytes := botBucket.Get(cfgKey)
			if cfgBytes == nil {
				return fmt.Errorf("no config found for %s", mkt.String())
			}
			cfg := new(BotConfig)
			err = json.Unmarshal(cfgBytes, cfg)
			if err != nil {
				return err
			}

			overviews[mkt.String()] = &RunOverview{
				Cfg:   cfg,
				Stats: stats,
			}

			return nil
		})
	})

	return overviews, err
}

func (db *boltEventLogDB) allRuns() ([]int64, error) {
	var runs []int64
	err := db.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(k []byte, _ *bbolt.Bucket) error {
			runs = append(runs, int64(binary.BigEndian.Uint64(k)))
			return nil
		})
	})
	return runs, err
}

func newEventLogDB(dbPath string) (eventLogDB, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}

	return &boltEventLogDB{
		db: db,
	}, nil
}

type eventLog struct {
	mtx           sync.Mutex
	events        []*Event
	startTime     int64
	baseChange    int64
	quoteChange   int64
	baseFees      uint64
	quoteFees     uint64
	baseFiatRate  float64
	quoteFiatRate float64
	log           dex.Logger
	// only non-finalized orders exist in this map. If the value is true, it
	// means the order has matches, and it will not be finalized until
	// all fees are confirmed. If the value is false, an order cancellation
	// will finalize the order.
	nonFinalOrders map[order.OrderID]bool
	db             eventLogDB
	notify         func(n core.Notification)

	host          string
	baseID        uint32
	quoteID       uint32
	baseUnitInfo  dex.UnitInfo
	quoteUnitInfo dex.UnitInfo
}

func (el *eventLog) sendEventNote(event *Event) {
	note := newBotEventNote(el.host, el.baseID, el.quoteID, event, el.statsInternal())
	el.notify(note)
}

func (el *eventLog) logOrdersPlaced(orderIDs []dex.Bytes, fundingTx *core.FundingTx, sell bool) {
	el.mtx.Lock()
	defer el.mtx.Unlock()

	var baseFees, quoteFees uint64
	var fundingTxID dex.Bytes
	if fundingTx != nil {
		if sell {
			baseFees += fundingTx.Fees
			el.baseFees += fundingTx.Fees
		} else {
			quoteFees += fundingTx.Fees
			el.quoteFees += fundingTx.Fees
		}
		fundingTxID = fundingTx.ID
	}

	for _, orderID := range orderIDs {
		var oid order.OrderID
		copy(oid[:], orderID)
		el.nonFinalOrders[oid] = false
	}

	event := &Event{
		Type:        eventTypeOrdersPlaced,
		TimeStamp:   time.Now().Unix(),
		OrderIDs:    orderIDs,
		FundingTxID: fundingTxID,
		BaseFees:    baseFees,
		QuoteFees:   quoteFees,
	}

	mkt := &MarketWithHost{
		Host:  el.host,
		Base:  el.baseID,
		Quote: el.quoteID,
	}

	err := el.db.storeEvent(el.startTime, mkt, event, el.statsInternal(), false)
	if err != nil {
		el.log.Errorf("error storing event: %v", err)
	}

	el.events = append(el.events, event)
	el.sendEventNote(event)
}

func (el *eventLog) logOrderCanceled(orderID dex.Bytes) {
	event := &Event{
		Type:      eventTypeOrderCancelled,
		TimeStamp: time.Now().Unix(),
		OrderIDs:  []dex.Bytes{orderID},
	}

	el.mtx.Lock()
	defer el.mtx.Unlock()

	var oid order.OrderID
	copy(oid[:], orderID)
	if hasMatches := el.nonFinalOrders[oid]; !hasMatches {
		delete(el.nonFinalOrders, oid)
	}

	allOrdersFinalized := len(el.nonFinalOrders) == 0
	mkt := &MarketWithHost{
		Host:  el.host,
		Base:  el.baseID,
		Quote: el.quoteID,
	}
	err := el.db.storeEvent(el.startTime, mkt, event, el.statsInternal(), allOrdersFinalized)
	if err != nil {
		el.log.Errorf("error storing event: %v", err)
	}

	el.events = append(el.events, event)
	el.sendEventNote(event)
}

func (el *eventLog) logMatch(orderID dex.Bytes, matchID dex.Bytes, sell bool, qty, rate uint64) {
	var baseDelta, quoteDelta int64
	if sell {
		baseDelta -= int64(qty)
		quoteDelta += int64(calc.BaseToQuote(rate, qty))
	} else {
		baseDelta += int64(qty)
		quoteDelta -= int64(calc.BaseToQuote(rate, qty))
	}

	el.baseChange += baseDelta
	el.quoteChange += quoteDelta

	event := &Event{
		Type:       eventTypeMatch,
		TimeStamp:  time.Now().Unix(),
		OrderIDs:   []dex.Bytes{orderID},
		MatchID:    matchID,
		BaseDelta:  baseDelta,
		QuoteDelta: quoteDelta,
	}

	var oid order.OrderID
	copy(oid[:], orderID)

	el.mtx.Lock()
	defer el.mtx.Unlock()

	el.nonFinalOrders[oid] = true

	mkt := &MarketWithHost{
		Host:  el.host,
		Base:  el.baseID,
		Quote: el.quoteID,
	}
	err := el.db.storeEvent(el.startTime, mkt, event, el.statsInternal(), false)
	if err != nil {
		el.log.Errorf("error storing event: %v", err)
	}

	el.events = append(el.events, event)
	el.sendEventNote(event)
}

func (el *eventLog) logFeesConfirmed(orderID dex.Bytes, swapFees, redeemFees uint64, sell bool) {
	var baseFees, quoteFees uint64
	if sell {
		baseFees = swapFees
		quoteFees = redeemFees
	} else {
		baseFees = redeemFees
		quoteFees = swapFees
	}

	el.baseFees += baseFees
	el.quoteFees += quoteFees

	event := &Event{
		Type:      eventTypeFeesConfirmed,
		TimeStamp: time.Now().Unix(),
		OrderIDs:  []dex.Bytes{orderID},
		BaseFees:  baseFees,
		QuoteFees: quoteFees,
	}

	var oid order.OrderID
	copy(oid[:], orderID)

	el.mtx.Lock()
	defer el.mtx.Unlock()

	delete(el.nonFinalOrders, oid)

	allOrdersFinalized := len(el.nonFinalOrders) == 0
	mkt := &MarketWithHost{
		Host:  el.host,
		Base:  el.baseID,
		Quote: el.quoteID,
	}
	err := el.db.storeEvent(el.startTime, mkt, event, el.statsInternal(), allOrdersFinalized)
	if err != nil {
		el.log.Errorf("error storing event: %v", err)
	}
	el.sendEventNote(event)
}

func (el *eventLog) updateFiatRates(base float64, quote float64) {
	el.baseFiatRate = base
	el.quoteFiatRate = quote
}

func (el *eventLog) logsAndStats() ([]*Event, *RunStats) {
	el.mtx.Lock()
	defer el.mtx.Unlock()

	copiedEvents := make([]*Event, len(el.events))
	copy(copiedEvents, el.events)

	return copiedEvents, el.statsInternal()
}

func calcFiatGainLoss(baseChange, quoteChange int64, baseFees, quoteFees uint64, baseFiatRate, quoteFiatRate float64,
	baseConvFactor, quoteConvFactor uint64) float64 {
	changeInBaseConventional := float64(baseChange-int64(baseFees)) / float64(baseConvFactor)
	changeInQuoteConventional := float64(quoteChange-int64(quoteFees)) / float64(quoteConvFactor)
	changeInBaseFiat := changeInBaseConventional * baseFiatRate
	changeInQuoteFiat := changeInQuoteConventional * quoteFiatRate
	return changeInBaseFiat + changeInQuoteFiat
}

func (el *eventLog) statsInternal() *RunStats {
	baseConvFactor := el.baseUnitInfo.Conventional.ConversionFactor
	quoteConvFactor := el.quoteUnitInfo.Conventional.ConversionFactor

	return &RunStats{
		BaseChange:  el.baseChange,
		QuoteChange: el.quoteChange,
		BaseFees:    el.baseFees,
		QuoteFees:   el.quoteFees,
		FiatGainLoss: calcFiatGainLoss(el.baseChange, el.quoteChange, el.baseFees, el.quoteFees,
			el.baseFiatRate, el.quoteFiatRate, baseConvFactor, quoteConvFactor),
	}
}

func (el *eventLog) stats() *RunStats {
	el.mtx.Lock()
	defer el.mtx.Unlock()

	return el.statsInternal()
}

func newEventLog(host string, baseID, quoteID uint32, startTime int64, db eventLogDB, log dex.Logger, notify func(core.Notification)) (*eventLog, error) {
	baseUnitInfo, err := asset.UnitInfo(baseID)
	if err != nil {
		return nil, err
	}

	quoteUnitInfo, err := asset.UnitInfo(quoteID)
	if err != nil {
		return nil, err
	}

	return &eventLog{
		baseUnitInfo:   baseUnitInfo,
		quoteUnitInfo:  quoteUnitInfo,
		host:           host,
		baseID:         baseID,
		quoteID:        quoteID,
		startTime:      startTime,
		log:            log,
		db:             db,
		nonFinalOrders: make(map[order.OrderID]bool),
		notify:         notify,
	}, nil
}

// finalizeArchivedEventLog takes a list of events and loops through them to
// see if any of the orders placed by the bot have not yet been finalized. If
// the bot was turned off before an order with matches had its fees confirmed,
// the order will be looked up, and if the fees are confirmed, a fee confirmation
// event will be added to the log and the run stats updated accordingly.
func finalizeArchivedEventLog(db eventLogDB, core clientCore, startTime int64, mkt *MarketWithHost, events []*Event, stats *RunStats, log dex.Logger) ([]*Event, *RunStats, bool, error) {
	nonFinalizedOrders := make(map[order.OrderID]bool)
	for _, event := range events {
		switch event.Type {
		case eventTypeOrdersPlaced:
			for _, orderID := range event.OrderIDs {
				var oid order.OrderID
				copy(oid[:], orderID)
				nonFinalizedOrders[oid] = false
			}
		case eventTypeOrderCancelled:
			for _, orderID := range event.OrderIDs {
				var oid order.OrderID
				copy(oid[:], orderID)
				if hasMatches := nonFinalizedOrders[oid]; !hasMatches {
					delete(nonFinalizedOrders, oid)
				}
			}
		case eventTypeMatch:
			for _, orderID := range event.OrderIDs {
				var oid order.OrderID
				copy(oid[:], orderID)
				nonFinalizedOrders[oid] = true
			}
		case eventTypeFeesConfirmed:
			for _, orderID := range event.OrderIDs {
				var oid order.OrderID
				copy(oid[:], orderID)
				delete(nonFinalizedOrders, oid)
			}
		}
	}
	if len(nonFinalizedOrders) == 0 {
		log.Warnf("no non-finalized orders found for %d - %s", startTime, mkt.String())
		return events, stats, true, nil
	}

	fiatRates, err := db.fiatRates(startTime)
	if err != nil {
		return nil, nil, false, fmt.Errorf("error getting fiat rates: %v", err)
	}

	baseUnitInfo, err := asset.UnitInfo(mkt.Base)
	if err != nil {
		return nil, nil, false, err
	}

	quoteUnitInfo, err := asset.UnitInfo(mkt.Quote)
	if err != nil {
		return nil, nil, false, err
	}

	updatedEvents := make([]*Event, 0, len(events)+len(nonFinalizedOrders))
	updatedEvents = append(updatedEvents, events...)

	updatedStats := *stats
	allFinalized := true
	var i int = -1

	for oid, hasMatches := range nonFinalizedOrders {
		i++

		var oidCopy order.OrderID
		copy(oidCopy[:], oid[:])

		o, err := core.Order(oid[:])
		if err != nil {
			log.Errorf("error getting order %s: %v", oid, err)
			allFinalized = false
			continue
		}

		if o.Status < order.OrderStatusExecuted {
			allFinalized = false
			continue
		}

		if !hasMatches {
			event := &Event{
				Type:      eventTypeOrderCancelled,
				TimeStamp: time.Now().Unix(),
				OrderIDs:  []dex.Bytes{oidCopy[:]},
			}
			db.storeEvent(startTime, mkt, event, &updatedStats, i == len(nonFinalizedOrders)-1 && allFinalized)
			updatedEvents = append(updatedEvents, event)
		} else if o.AllFeesConfirmed {
			var baseFees, quoteFees uint64
			if o.Sell {
				baseFees = o.FeesPaid.Swap
				quoteFees = o.FeesPaid.Redemption
			} else {
				baseFees = o.FeesPaid.Redemption
				quoteFees = o.FeesPaid.Swap
			}

			updatedStats.BaseFees += baseFees
			updatedStats.QuoteFees += quoteFees
			updatedStats.FiatGainLoss = calcFiatGainLoss(updatedStats.BaseChange, updatedStats.QuoteChange,
				updatedStats.BaseFees, updatedStats.QuoteFees, fiatRates[mkt.Base], fiatRates[mkt.Quote],
				baseUnitInfo.Conventional.ConversionFactor, quoteUnitInfo.Conventional.ConversionFactor)

			event := &Event{
				Type:      eventTypeFeesConfirmed,
				TimeStamp: time.Now().Unix(),
				OrderIDs:  []dex.Bytes{oidCopy[:]},
				BaseFees:  baseFees,
				QuoteFees: quoteFees,
			}
			err = db.storeEvent(startTime, mkt, event, &updatedStats, i == len(nonFinalizedOrders)-1 && allFinalized)
			if err != nil {
				return nil, nil, false, fmt.Errorf("error storing event: %v", err)
			}

			updatedEvents = append(updatedEvents, event)
		} else {
			allFinalized = false
		}
	}

	return updatedEvents, stats, allFinalized, nil
}
