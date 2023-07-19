package mm

import (
	"bytes"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/order"
)

func randBytes(l int) []byte {
	b := make([]byte, l)
	rand.Read(b)
	return b
}

func eventsEqual(expected, actual *Event) bool {
	if expected.Type != actual.Type {
		return false
	}
	if expected.TimeStamp != actual.TimeStamp {
		return false
	}
	if len(expected.OrderIDs) != len(actual.OrderIDs) {
		return false
	}
	for i := range expected.OrderIDs {
		if !bytes.Equal(expected.OrderIDs[i], actual.OrderIDs[i]) {
			return false
		}
	}
	return bytes.Equal(expected.FundingTxID, actual.FundingTxID)
}

func TestEventLogDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db.db")
	db, err := newEventLogDB(dbPath)
	if err != nil {
		t.Fatalf("error creating event log db: %v", err)
	}

	var startTimeA int64 = 123456
	var startTimeB int64 = 234567

	mktA := &MarketWithHost{
		Host:  "hostA",
		Base:  1,
		Quote: 2,
	}
	mktB := &MarketWithHost{
		Host:  "hostB",
		Base:  3,
		Quote: 4,
	}

	eventA := &Event{
		Type:        eventTypeOrdersPlaced,
		TimeStamp:   567861,
		OrderIDs:    []dex.Bytes{randBytes(32)},
		FundingTxID: randBytes(32),
	}
	eventB := &Event{
		Type:        eventTypeOrdersPlaced,
		TimeStamp:   5678672,
		OrderIDs:    []dex.Bytes{randBytes(32)},
		FundingTxID: randBytes(32),
	}
	eventC := &Event{
		Type:        eventTypeOrdersPlaced,
		TimeStamp:   567882,
		OrderIDs:    []dex.Bytes{randBytes(32)},
		FundingTxID: randBytes(32),
	}
	eventD := &Event{
		Type:      eventTypeOrderCancelled,
		TimeStamp: 567893,
		OrderIDs:  []dex.Bytes{randBytes(32)},
	}

	statsA := &RunStats{
		BaseChange:   100,
		QuoteChange:  200,
		BaseFees:     300,
		QuoteFees:    400,
		FiatGainLoss: 500,
	}
	statsB := &RunStats{
		BaseChange:   1000,
		QuoteChange:  2000,
		BaseFees:     3000,
		QuoteFees:    4000,
		FiatGainLoss: 5000,
	}

	cfgA := &BotConfig{
		Host:             "hostA",
		BaseAsset:        1,
		QuoteAsset:       2,
		BaseBalanceType:  Amount,
		QuoteBalanceType: Percentage,
		BaseBalance:      100,
		QuoteBalance:     200,
		MMCfg: &MarketMakingConfig{
			GapStrategy: GapStrategyAbsolute,
		},
	}
	cfgB := &BotConfig{
		Host:             "hostB",
		BaseAsset:        3,
		QuoteAsset:       4,
		BaseBalanceType:  Percentage,
		QuoteBalanceType: Amount,
		BaseBalance:      300,
		QuoteBalance:     400,
		MMCfg: &MarketMakingConfig{
			GapStrategy: GapStrategyMultiplier,
		},
	}

	fiatRatesA := map[uint32]float64{
		1: 1.05,
		2: 2.01,
		3: 3.03,
		4: 4.02,
	}

	fiatRatesB := map[uint32]float64{
		1: 1.06,
		2: 2.02,
		3: 3.04,
		4: 4.03,
	}

	err = db.storeNewRun(startTimeA, []*BotConfig{cfgA, cfgB}, fiatRatesA)
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}

	logs, stats, finalized, err := db.runLogs(startTimeA, mktA)
	if err != nil {
		t.Fatalf("error getting run logs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected 0 logs, got %d", len(logs))
	}
	if !reflect.DeepEqual(stats, &RunStats{}) {
		t.Fatalf("expected stats %v, got %v", &RunStats{}, stats)
	}
	if !finalized {
		t.Fatalf("expected finalized to be true, got false")
	}

	overviews, err := db.runOverviews(startTimeA)
	if err != nil {
		t.Fatalf("error getting run overviews: %v", err)
	}
	if len(overviews) != 2 {
		t.Fatalf("expected 2 overviews, got %d", len(overviews))
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Cfg, cfgA) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktA.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Stats, &RunStats{}) {
		t.Fatalf("expected stats %v, got %v", &RunStats{}, overviews[mktA.String()].Stats)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Cfg, cfgB) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktB.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Stats, &RunStats{}) {
		t.Fatalf("expected stats %v, got %v", &RunStats{}, overviews[mktB.String()].Stats)
	}

	storedCfgA, err := db.runConfig(startTimeA, mktA)
	if err != nil {
		t.Fatalf("error getting bot config: %v", err)
	}
	if !reflect.DeepEqual(storedCfgA, cfgA) {
		t.Fatalf("expected cfg %v, got %v", cfgA, storedCfgA)
	}

	fiatRates, err := db.fiatRates(startTimeA)
	if err != nil {
		t.Fatalf("error getting fiat rates: %v", err)
	}
	if !reflect.DeepEqual(fiatRates, fiatRatesA) {
		t.Fatalf("expected fiat rates %v, got %v", fiatRatesA, fiatRates)
	}

	err = db.storeFiatRates(startTimeA, fiatRatesB)
	if err != nil {
		t.Fatalf("error storing fiat rates: %v", err)
	}
	fiatRates, err = db.fiatRates(startTimeA)
	if err != nil {
		t.Fatalf("error getting fiat rates: %v", err)
	}
	if !reflect.DeepEqual(fiatRates, fiatRatesB) {
		t.Fatalf("expected fiat rates %v, got %v", fiatRatesB, fiatRates)
	}

	err = db.storeNewRun(startTimeB, []*BotConfig{cfgA, cfgB}, fiatRatesB)
	if err != nil {
		t.Fatalf("error storing new run: %v", err)
	}

	runs, err := db.allRuns()
	if err != nil {
		t.Fatalf("error getting all runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0] != startTimeA && runs[1] != startTimeB {
		t.Fatalf("expected runs %d and %d, got %d and %d", startTimeA, startTimeB, runs[0], runs[1])
	}

	err = db.storeEvent(startTimeA, mktA, eventA, statsA, false)
	if err != nil {
		t.Fatalf("error storing event: %v", err)
	}
	err = db.storeEvent(startTimeA, mktA, eventB, statsB, false)
	if err != nil {
		t.Fatalf("error storing event: %v", err)
	}
	err = db.storeEvent(startTimeA, mktB, eventA, statsA, false)
	if err != nil {
		t.Fatalf("error storing event: %v", err)
	}
	err = db.storeEvent(startTimeB, mktB, eventC, statsB, false)
	if err != nil {
		t.Fatalf("error storing event: %v", err)
	}
	err = db.storeEvent(startTimeB, mktB, eventD, statsB, false)
	if err != nil {
		t.Fatalf("error storing event: %v", err)
	}

	logs, stats, finalized, err = db.runLogs(startTimeA, mktA)
	if err != nil {
		t.Fatalf("error getting run logs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}
	if !eventsEqual(logs[0], eventA) {
		t.Fatalf("expected event %v, got %v", eventA, logs[0])
	}
	if !eventsEqual(logs[1], eventB) {
		t.Fatalf("expected event %v, got %v", eventB, logs[1])
	}
	if !reflect.DeepEqual(stats, statsB) {
		t.Fatalf("expected stats %v, got %v", statsB, stats)
	}
	if finalized {
		t.Fatalf("expected finalized to be false, got true")
	}

	overviews, err = db.runOverviews(startTimeA)
	if err != nil {
		t.Fatalf("error getting run overviews: %v", err)
	}
	if len(overviews) != 2 {
		t.Fatalf("expected 2 overviews, got %d", len(overviews))
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Cfg, cfgA) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktA.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Stats, statsB) {
		t.Fatalf("expected stats %v, got %v", statsB, overviews[mktA.String()].Stats)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Cfg, cfgB) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktB.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Stats, statsA) {
		t.Fatalf("expected stats %v, got %v", statsA, overviews[mktB.String()].Stats)
	}

	overviews, err = db.runOverviews(startTimeB)
	if err != nil {
		t.Fatalf("error getting run overviews: %v", err)
	}
	if len(overviews) != 2 {
		t.Fatalf("expected 2 overviews, got %d", len(overviews))
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Cfg, cfgA) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktA.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktA.String()].Stats, &RunStats{}) {
		t.Fatalf("expected stats %v, got %v", &RunStats{}, overviews[mktA.String()].Stats)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Cfg, cfgB) {
		t.Fatalf("expected cfg %v, got %v", cfgA, overviews[mktB.String()].Cfg)
	}
	if !reflect.DeepEqual(overviews[mktB.String()].Stats, statsB) {
		t.Fatalf("expected stats %v, got %v", statsB, overviews[mktB.String()].Stats)
	}
}

func TestEventLogFinalization(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db.db")
	db, err := newEventLogDB(dbPath)
	if err != nil {
		t.Fatalf("error creating event log db: %v", err)
	}

	startTime := time.Now().UnixNano()

	mkt := &MarketWithHost{
		Host:  "host",
		Base:  42,
		Quote: 0,
	}
	fiatRates := map[uint32]float64{
		42: 15,
		0:  30000,
	}
	db.storeFiatRates(startTime, fiatRates)

	notify := func(core.Notification) {} // dummy handler
	eventLog, err := newEventLog("host", 42, 0, startTime, db, tLogger, notify)
	if err != nil {
		t.Fatalf("error creating event log: %v", err)
	}
	eventLog.updateFiatRates(fiatRates[42], fiatRates[0])

	var oid1, oid2, oid3, oid4 order.OrderID
	copy(oid1[:], randBytes(32))
	copy(oid2[:], randBytes(32))
	copy(oid3[:], randBytes(32))
	copy(oid4[:], randBytes(32))

	checkIfFinalized := func(expected bool) {
		t.Helper()
		_, _, finalized, err := db.runLogs(startTime, mkt)
		if err != nil {
			t.Fatalf("error getting run logs: %v", err)
		}
		if finalized != expected {
			t.Fatalf("expected finalized to be %v, got %v", expected, finalized)
		}
	}

	eventLog.logOrdersPlaced([]dex.Bytes{oid1[:]}, nil, false)
	checkIfFinalized(false)

	eventLog.logOrderCanceled(oid1[:])
	checkIfFinalized(true)

	eventLog.logOrdersPlaced([]dex.Bytes{oid2[:]}, nil, false)
	checkIfFinalized(false)
	eventLog.logMatch(oid2[:], randBytes(32), true, 100, 100)
	checkIfFinalized(false)
	eventLog.logOrderCanceled(oid2[:])
	checkIfFinalized(false)
	eventLog.logFeesConfirmed(oid2[:], 100, 100, false)
	checkIfFinalized(true)

	eventLog.logOrdersPlaced([]dex.Bytes{oid3[:]}, nil, false)
	eventLog.logOrdersPlaced([]dex.Bytes{oid4[:]}, nil, false)
	eventLog.logMatch(oid4[:], randBytes(32), true, 100, 100)
	checkIfFinalized(false)

	tc := newTCore()
	tc.ordersMap[oid3] = &core.Order{
		Status: order.OrderStatusExecuted,
	}
	tc.ordersMap[oid4] = &core.Order{
		Matches: []*core.Match{
			{
				Status: order.MatchConfirmed,
			},
		}}

	// One of the orders has no matches and is marked as executed,
	// so a cancel event should be added. The other order had a
	// match and the fees are not yet confirmed, so the log is
	// still not finalized.
	events, stats, _, err := db.runLogs(startTime, mkt)
	if err != nil {
		t.Fatalf("error getting run logs: %v", err)
	}
	updatedEvents, _, updatedFinalized, err := finalizeArchivedEventLog(db, tc, startTime, mkt, events, stats, tLogger)
	if err != nil {
		t.Fatalf("error finalizing archived event log: %v", err)
	}
	if updatedFinalized {
		t.Fatalf("expected finalized to be false, got true")
	}
	if len(updatedEvents) != len(events)+1 {
		t.Fatalf("expected %d events, got %d", len(events)+1, len(updatedEvents))
	}
	lastEvent := updatedEvents[len(updatedEvents)-1]
	if lastEvent.Type != eventTypeOrderCancelled {
		t.Fatalf("expected last event to be order cancelled, got %v", lastEvent.Type)
	}
	if !bytes.Equal(lastEvent.OrderIDs[0], oid3[:]) {
		t.Fatalf("expected last event to be for oid3, got %v", lastEvent.OrderIDs[0])
	}

	// Set the fees as confirmed for the order with a match.
	// The log should now be able to be finalized.
	tc.ordersMap[oid4].AllFeesConfirmed = true
	tc.ordersMap[oid4].FeesPaid = &core.FeeBreakdown{
		Swap:       2000,
		Redemption: 1000,
	}
	tc.ordersMap[oid4].Status = order.OrderStatusExecuted
	updatedEvents, _, updatedFinalized, err = finalizeArchivedEventLog(db, tc, startTime, mkt, updatedEvents, stats, tLogger)
	if err != nil {
		t.Fatalf("error finalizing archived event log: %v", err)
	}
	if !updatedFinalized {
		t.Fatalf("expected finalized to be true, got false")
	}
	if len(updatedEvents) != len(events)+2 {
		t.Fatalf("expected %d events, got %d", len(events)+2, len(updatedEvents))
	}
	lastEvent = updatedEvents[len(updatedEvents)-1]
	if lastEvent.Type != eventTypeFeesConfirmed && !bytes.Equal(lastEvent.OrderIDs[0], oid4[:]) {
		t.Fatalf("expected last event to be fees confirmed, got %v", lastEvent.Type)
	}
	retrievedLogs, updatedStats, finalized, err := db.runLogs(startTime, mkt)
	if err != nil {
		t.Fatalf("error getting run logs: %v", err)
	}
	if len(retrievedLogs) != len(updatedEvents) {
		t.Fatalf("expected %d events, got %d", len(updatedEvents), len(retrievedLogs))
	}
	if !finalized {
		t.Fatalf("expected finalized to be true, got false")
	}
	baseFeesChange := tc.ordersMap[oid4].FeesPaid.Redemption
	if updatedStats.BaseFees != stats.BaseFees+baseFeesChange {
		t.Fatalf("expected base fees to be %d, got %d", stats.BaseFees+tc.ordersMap[oid4].FeesPaid.Redemption, updatedStats.BaseFees)
	}
	quoteFeesChange := tc.ordersMap[oid4].FeesPaid.Swap
	if updatedStats.QuoteFees != stats.QuoteFees+quoteFeesChange {
		t.Fatalf("expected quote fees to be %d, got %d", stats.QuoteFees+tc.ordersMap[oid4].FeesPaid.Swap, updatedStats.QuoteFees)
	}
	fiatBaseFeesChange := float64(baseFeesChange) / 1e8 * fiatRates[42]
	fiatQuoteFeesChange := float64(quoteFeesChange) / 1e8 * fiatRates[0]
	expectedFiatGainLossChange := math.Round((fiatBaseFeesChange+fiatQuoteFeesChange)*100) / 100
	expectedFiatGainLossChange *= -1
	if expectedFiatGainLossChange != updatedStats.FiatGainLoss-stats.FiatGainLoss {
		t.Fatalf("unexpected change in fiat rates: expected %f - actual %f", expectedFiatGainLossChange,
			updatedStats.FiatGainLoss-stats.FiatGainLoss)
	}
}

func TestEncodeRunStats(t *testing.T) {
	stats := &RunStats{
		BaseChange:   100,
		QuoteChange:  200,
		BaseFees:     300,
		QuoteFees:    400,
		FiatGainLoss: 500,
	}

	encodedStats := stats.encode()
	decodedStats, err := decodeRunStats(encodedStats)
	if err != nil {
		t.Fatalf("error decoding run stats: %v", err)
	}

	if !reflect.DeepEqual(stats, decodedStats) {
		t.Fatalf("expected decoded stats %v, got %v", stats, decodedStats)
	}

	stats = &RunStats{
		BaseChange:   -300,
		QuoteChange:  -200,
		BaseFees:     300,
		QuoteFees:    400,
		FiatGainLoss: -800.12,
	}

	encodedStats = stats.encode()
	decodedStats, err = decodeRunStats(encodedStats)
	if err != nil {
		t.Fatalf("error decoding run stats: %v", err)
	}

	if !reflect.DeepEqual(stats, decodedStats) {
		t.Fatalf("expected decoded stats %v, got %v", stats, decodedStats)
	}
}

func TestEncodeEvent(t *testing.T) {
	event := &Event{
		Type:        eventTypeOrdersPlaced,
		TimeStamp:   567861,
		OrderIDs:    []dex.Bytes{randBytes(32), randBytes(32), randBytes(32)},
		MatchID:     randBytes(32),
		FundingTxID: randBytes(32),
		BaseDelta:   100,
		QuoteDelta:  200,
		BaseFees:    300,
		QuoteFees:   400,
	}

	encodedEvent := event.encode()
	decodedEvent, err := decodeEvent(encodedEvent)
	if err != nil {
		t.Fatalf("error decoding event: %v", err)
	}

	if !eventsEqual(event, decodedEvent) {
		t.Fatalf("expected decoded event %v, got %v", event, decodedEvent)
	}
}
