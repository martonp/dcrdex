// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
	"github.com/lib/pq"
)

func (a *Archiver) matchTableName(match *order.Match) (string, error) {
	marketSchema, err := a.marketSchema(match.Maker.Base(), match.Maker.Quote())
	if err != nil {
		return "", err
	}
	return fullMatchesTableName(a.dbName, marketSchema), nil
}

// ActiveSwaps loads the full details for all active swaps across all markets.
func (a *Archiver) ActiveSwaps() ([]*db.SwapDataFull, error) {
	var sd []*db.SwapDataFull

	for schema, mkt := range a.markets {
		matchesTableName := fullMatchesTableName(a.dbName, schema)
		ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
		matches, swapData, err := activeSwaps(ctx, a.db, matchesTableName)
		cancel()
		if err != nil {
			return nil, err
		}

		for i := range matches {
			sd = append(sd, &db.SwapDataFull{
				Base:      mkt.Base,
				Quote:     mkt.Quote,
				MatchData: matches[i],
				SwapData:  swapData[i],
			})
		}
	}

	return sd, nil
}

func activeSwaps(ctx context.Context, dbe *sql.DB, tableName string) (matches []*db.MatchData, swapData []*db.SwapData, err error) {
	stmt := fmt.Sprintf(internal.RetrieveActiveMarketMatchesExtended, tableName)
	rows, err := dbe.QueryContext(ctx, stmt)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var m db.MatchData
		var sd db.SwapData

		var status uint8
		var baseRate, quoteRate sql.NullInt64
		var takerSell sql.NullBool
		var takerAddr, makerAddr sql.NullString
		var contractATime, contractBTime, redeemATime, redeemBTime sql.NullInt64

		err = rows.Scan(&m.ID, &takerSell,
			&m.Taker, &m.TakerAcct, &takerAddr,
			&m.Maker, &m.MakerAcct, &makerAddr,
			&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
			&baseRate, &quoteRate, &status,
			&sd.SigMatchAckMaker, &sd.SigMatchAckTaker,
			&sd.MakerSwapAddr, &sd.TakerSwapAddr,
			&sd.ContractACoinID, &sd.ContractA, &contractATime,
			&sd.ContractAAckSig,
			&sd.ContractBCoinID, &sd.ContractB, &contractBTime,
			&sd.ContractBAckSig,
			&sd.RedeemACoinID, &sd.RedeemASecret, &redeemATime,
			&sd.RedeemAAckSig,
			&sd.RedeemBCoinID, &redeemBTime)
		if err != nil {
			return nil, nil, err
		}

		// All are active.
		m.Active = true

		m.Status = order.MatchStatus(status)
		m.TakerSell = takerSell.Bool
		m.TakerAddr = takerAddr.String
		m.MakerAddr = makerAddr.String
		m.BaseRate = uint64(baseRate.Int64)
		m.QuoteRate = uint64(quoteRate.Int64)

		sd.ContractATime = contractATime.Int64
		sd.ContractBTime = contractBTime.Int64
		sd.RedeemATime = redeemATime.Int64
		sd.RedeemBTime = redeemBTime.Int64

		matches = append(matches, &m)
		swapData = append(swapData, &sd)
	}

	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	return
}

// CompletedAndAtFaultMatchStats retrieves the outcomes of matches that were (1)
// successfully completed by the specified user, or (2) failed with the user
// being the at-fault party. Note that the MakerRedeemed match status may be
// either a success or failure depending on if the user was the maker or taker
// in the swap, respectively, and the MatchOutcome.Fail flag disambiguates this.
func (a *Archiver) CompletedAndAtFaultMatchStats(aid account.AccountID, lastN int) ([]*db.MatchOutcome, error) {
	var outcomes []*db.MatchOutcome

	for schema, mkt := range a.markets {
		matchesTableName := fullMatchesTableName(a.dbName, schema)
		ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
		matchOutcomes, err := completedAndAtFaultMatches(ctx, a.db, matchesTableName, aid, lastN, mkt.Base, mkt.Quote)
		cancel()
		if err != nil {
			return nil, err
		}

		outcomes = append(outcomes, matchOutcomes...)
	}

	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].Time < outcomes[j].Time // ascending
	})
	if len(outcomes) > lastN {
		outcomes = outcomes[len(outcomes)-lastN:]
	}
	return outcomes, nil
}

// UserMatchFails retrieves up to the last n most recent failed and unforgiven
// match outcomes for the user.
func (a *Archiver) UserMatchFails(aid account.AccountID, lastN int) ([]*db.MatchFail, error) {
	var fails []*db.MatchFail

	for schema := range a.markets {
		matchesTableName := fullMatchesTableName(a.dbName, schema)
		ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
		marketFails, err := atFaultMatches(ctx, a.db, matchesTableName, aid, lastN)
		cancel()
		if err != nil {
			return nil, err
		}

		fails = append(fails, marketFails...)
	}

	if len(fails) > lastN {
		fails = fails[:lastN]
	}
	return fails, nil
}

func completedAndAtFaultMatches(ctx context.Context, dbe *sql.DB, tableName string,
	aid account.AccountID, lastN int, base, quote uint32) (outcomes []*db.MatchOutcome, err error) {
	stmt := fmt.Sprintf(internal.CompletedOrAtFaultMatchesLastN, tableName)
	rows, err := dbe.QueryContext(ctx, stmt, aid, lastN)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var status uint8
		var success bool
		var refTime sql.NullInt64
		var mid order.MatchID
		var value uint64
		err = rows.Scan(&mid, &status, &value, &success, &refTime)
		if err != nil {
			return
		}

		if !refTime.Valid {
			continue // should not happen as all matches will have an epoch time, but don't error
		}

		// A little seat belt in case the query returns inconsistent results
		// where success and status don't jive.
		switch order.MatchStatus(status) {
		case order.NewlyMatched, order.MakerSwapCast, order.TakerSwapCast:
			if success {
				log.Errorf("successfully completed match in status %v returned from DB", status)
				continue
			}
		// MakerRedeemed can be either depending on user role (maker/taker).
		case order.MatchComplete:
			if !success {
				log.Errorf("failed match in status %v returned from DB", status)
				continue
			}
		}

		outcomes = append(outcomes, &db.MatchOutcome{
			Status: order.MatchStatus(status),
			ID:     mid,
			Fail:   !success,
			Time:   refTime.Int64,
			Value:  value,
			Base:   base,
			Quote:  quote,
		})
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return
}

func atFaultMatches(ctx context.Context, dbe *sql.DB, tableName string, aid account.AccountID, lastN int) (fails []*db.MatchFail, err error) {
	stmt := fmt.Sprintf(internal.UserMatchFails, tableName)
	rows, err := dbe.QueryContext(ctx, stmt, aid, lastN)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var status uint8
		var mid order.MatchID
		err = rows.Scan(&mid, &status)
		if err != nil {
			return
		}

		fails = append(fails, &db.MatchFail{
			Status: order.MatchStatus(status),
			ID:     mid,
		})
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return
}

// activeUserMatches retrieves all active matches involving a user on the given
// market.
func activeUserMatches(ctx context.Context, dbe *sql.DB, tableName string, aid account.AccountID) ([]*db.MatchData, error) {
	stmt := fmt.Sprintf(internal.RetrieveActiveUserMatches, tableName)
	rows, err := dbe.QueryContext(ctx, stmt, aid)
	if err != nil {
		return nil, err
	}
	return rowsToMatchData(rows, false)
}

func rowsToMatchData(rows *sql.Rows, includeInactive bool) ([]*db.MatchData, error) {
	defer rows.Close()

	var (
		ms  []*db.MatchData
		err error
	)
	for rows.Next() {
		var m db.MatchData
		var status uint8
		var baseRate, quoteRate sql.NullInt64
		var takerSell sql.NullBool
		var takerAddr, makerAddr sql.NullString
		var makerSwapAddr, takerSwapAddr sql.NullString
		if includeInactive {
			// "active" column SELECTed.
			err = rows.Scan(&m.ID, &m.Active, &takerSell,
				&m.Taker, &m.TakerAcct, &takerAddr,
				&m.Maker, &m.MakerAcct, &makerAddr,
				&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
				&baseRate, &quoteRate, &status,
				&makerSwapAddr, &takerSwapAddr)
			if err != nil {
				return nil, err
			}
		} else {
			// "active" column not SELECTed.
			err = rows.Scan(&m.ID, &takerSell,
				&m.Taker, &m.TakerAcct, &takerAddr,
				&m.Maker, &m.MakerAcct, &makerAddr,
				&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
				&baseRate, &quoteRate, &status,
				&makerSwapAddr, &takerSwapAddr)
			if err != nil {
				return nil, err
			}
			// All are active.
			m.Active = true
		}
		m.Status = order.MatchStatus(status)
		m.TakerSell = takerSell.Bool
		m.TakerAddr = takerAddr.String
		m.MakerAddr = makerAddr.String
		m.MakerSwapAddr = makerSwapAddr.String
		m.TakerSwapAddr = takerSwapAddr.String
		m.BaseRate = uint64(baseRate.Int64)
		m.QuoteRate = uint64(quoteRate.Int64)

		ms = append(ms, &m)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return ms, nil
}

func (a *Archiver) marketMatches(base, quote uint32, includeInactive bool, N int64, f func(*db.MatchDataWithCoins) error) (int, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return 0, err
	}

	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)

	ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
	defer cancel()

	var rows *sql.Rows
	if includeInactive {
		stmt := fmt.Sprintf(internal.RetrieveMarketMatches, matchesTableName)
		if N <= 0 {
			N = math.MaxInt64
		}
		rows, err = a.db.QueryContext(ctx, stmt, N)
	} else {
		stmt := fmt.Sprintf(internal.RetrieveActiveMarketMatches, matchesTableName)
		rows, err = a.db.QueryContext(ctx, stmt) // no N
	}
	if err != nil {
		return 0, err
	}

	return rowsToMatchDataWithCoinsStreaming(rows, includeInactive, f)
}

// MarketMatches retrieves all active matches for a market.
func (a *Archiver) MarketMatches(base, quote uint32) ([]*db.MatchDataWithCoins, error) {
	var ms []*db.MatchDataWithCoins
	f := func(m *db.MatchDataWithCoins) error {
		ms = append(ms, m)
		return nil
	}
	_, err := a.marketMatches(base, quote, false, -1, f) // N ignored with only active
	if err != nil {
		return nil, err
	}
	return ms, nil
}

// MarketMatchesStreaming streams all active matches for a market into the
// provided function. If includeInactive, all matches are streamed. A limit may
// be specified, where <=0 means unlimited.
func (a *Archiver) MarketMatchesStreaming(base, quote uint32, includeInactive bool, N int64, f func(*db.MatchDataWithCoins) error) (int, error) {
	return a.marketMatches(base, quote, includeInactive, N, f)
}

func rowsToMatchDataWithCoinsStreaming(rows *sql.Rows, includeInactive bool, f func(*db.MatchDataWithCoins) error) (int, error) {
	defer rows.Close()

	var N int
	for rows.Next() {
		var m db.MatchDataWithCoins
		var status uint8
		var baseRate, quoteRate sql.NullInt64
		var takerSell sql.NullBool
		var takerAddr, makerAddr sql.NullString
		if includeInactive {
			// "active" column SELECTed.
			err := rows.Scan(&m.ID, &m.Active, &takerSell,
				&m.Taker, &m.TakerAcct, &takerAddr,
				&m.Maker, &m.MakerAcct, &makerAddr,
				&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
				&baseRate, &quoteRate, &status,
				&m.MakerSwapCoin, &m.TakerSwapCoin, &m.MakerRedeemCoin, &m.TakerRedeemCoin)
			if err != nil {
				return N, err
			}
		} else {
			// "active" column not SELECTed.
			err := rows.Scan(&m.ID, &takerSell,
				&m.Taker, &m.TakerAcct, &takerAddr,
				&m.Maker, &m.MakerAcct, &makerAddr,
				&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
				&baseRate, &quoteRate, &status,
				&m.MakerSwapCoin, &m.TakerSwapCoin, &m.MakerRedeemCoin, &m.TakerRedeemCoin)
			if err != nil {
				return N, err
			}
			// All are active.
			m.Active = true
		}
		m.Status = order.MatchStatus(status)
		m.TakerSell = takerSell.Bool
		m.TakerAddr = takerAddr.String
		m.MakerAddr = makerAddr.String
		m.BaseRate = uint64(baseRate.Int64)
		m.QuoteRate = uint64(quoteRate.Int64)

		if err := f(&m); err != nil {
			return N, err
		}
		N++
	}

	return N, rows.Err()
}

// AllActiveUserMatches retrieves a MatchData slice for active matches in all
// markets involving the given user. Swaps that have successfully completed or
// failed are not included.
func (a *Archiver) AllActiveUserMatches(aid account.AccountID) ([]*db.MatchData, error) {
	ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
	defer cancel()

	var matches []*db.MatchData
	for schema := range a.markets {
		matchesTableName := fullMatchesTableName(a.dbName, schema)
		mdM, err := activeUserMatches(ctx, a.db, matchesTableName, aid)
		if err != nil {
			return nil, err
		}

		matches = append(matches, mdM...)
	}

	return matches, nil
}

// MatchStatuses retrieves a *db.MatchStatus for every match in matchIDs for
// which there is data, and for which the user is at least one of the parties.
// It is not an error if a match ID in matchIDs does not match, i.e. the
// returned slice need not be the same length as matchIDs.
func (a *Archiver) MatchStatuses(aid account.AccountID, base, quote uint32, matchIDs []order.MatchID) ([]*db.MatchStatus, error) {
	ctx, cancel := context.WithTimeout(a.ctx, a.queryTimeout)
	defer cancel()

	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}

	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
	return matchStatusesByID(ctx, a.db, aid, matchesTableName, matchIDs)

}

func upsertMatch(dbe sqlExecutor, tableName string, match *order.Match) (int64, error) {
	var takerAddr string
	tt := match.Taker.Trade()
	if tt != nil {
		takerAddr = tt.SwapAddress()
	}

	// Cancel orders do not store taker or maker addresses, and are stored with
	// complete status with no active swap negotiation.
	if takerAddr == "" {
		stmt := fmt.Sprintf(internal.UpsertCancelMatch, tableName)
		return sqlExec(dbe, stmt, match.ID(),
			match.Taker.ID(), match.Taker.User(), // taker address remains unset/default
			match.Maker.ID(), match.Maker.User(), // as does maker's since it is not used
			match.Epoch.Idx, match.Epoch.Dur,
			int64(match.Quantity), int64(match.Rate), // quantity and rate may be useful for cancel statistics however
			int8(order.MatchComplete)) // status is complete
	}

	stmt := fmt.Sprintf(internal.UpsertMatch, tableName)
	return sqlExec(dbe, stmt, match.ID(), tt.Sell,
		match.Taker.ID(), match.Taker.User(), takerAddr,
		match.Maker.ID(), match.Maker.User(), match.Maker.Trade().SwapAddress(),
		match.Epoch.Idx, match.Epoch.Dur,
		int64(match.Quantity), int64(match.Rate),
		match.FeeRateBase, match.FeeRateQuote, int8(match.Status))
}

// MatchByID retrieves the match for the given MatchID.
func (a *Archiver) MatchByID(mid order.MatchID, base, quote uint32) (*db.MatchData, error) {
	marketSchema, err := a.marketSchema(base, quote)
	if err != nil {
		return nil, err
	}

	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
	matchData, err := matchByID(a.db, matchesTableName, mid)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.ArchiveError{Code: db.ErrUnknownMatch}
	}
	return matchData, err
}

func matchByID(dbe sqlQueryer, tableName string, mid order.MatchID) (*db.MatchData, error) {
	var m db.MatchData
	var status uint8
	var baseRate, quoteRate sql.NullInt64
	var takerAddr, makerAddr sql.NullString
	var takerSell sql.NullBool
	stmt := fmt.Sprintf(internal.RetrieveMatchByID, tableName)
	err := dbe.QueryRow(stmt, mid).
		Scan(&m.ID, &m.Active, &takerSell,
			&m.Taker, &m.TakerAcct, &takerAddr,
			&m.Maker, &m.MakerAcct, &makerAddr,
			&m.Epoch.Idx, &m.Epoch.Dur, &m.Quantity, &m.Rate,
			&baseRate, &quoteRate, &status)
	if err != nil {
		return nil, err
	}
	m.TakerSell = takerSell.Bool
	m.TakerAddr = takerAddr.String
	m.MakerAddr = makerAddr.String
	m.BaseRate = uint64(baseRate.Int64)
	m.QuoteRate = uint64(quoteRate.Int64)
	m.Status = order.MatchStatus(status)
	return &m, nil
}

// matchStatusesByID retrieves the []*db.MatchStatus for the requested matchIDs.
// See docs for MatchStatuses.
func matchStatusesByID(ctx context.Context, dbe *sql.DB, aid account.AccountID, tableName string, matchIDs []order.MatchID) ([]*db.MatchStatus, error) {
	stmt := fmt.Sprintf(internal.SelectMatchStatuses, tableName)
	pqArr := make(pq.ByteaArray, 0, len(matchIDs))
	for i := range matchIDs {
		pqArr = append(pqArr, matchIDs[i][:])
	}
	rows, err := dbe.QueryContext(ctx, stmt, aid, pqArr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := make([]*db.MatchStatus, 0, len(matchIDs))
	for rows.Next() {
		status := new(db.MatchStatus)
		err := rows.Scan(&status.TakerSell, &status.IsTaker, &status.IsMaker, &status.ID,
			&status.Status, &status.MakerContract, &status.TakerContract, &status.MakerSwap,
			&status.TakerSwap, &status.MakerRedeem, &status.TakerRedeem, &status.Secret, &status.Active)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return statuses, nil
}

// Swap Data
//
// In the swap process, the counterparties are:
// - Initiator or party A on chain X. This is the maker in the DEX.
// - Participant or party B on chain Y. This is the taker in the DEX.
//
// For each match, a successful swap will generate the following data that must
// be stored:
// - 5 client signatures. Both parties sign the data to acknowledge (1) the
//   match ack, and (2) the counterparty's contract script and contract
//   transaction. Plus, the taker acks the makers's redemption transaction.
// - 2 swap contracts and the associated transaction outputs (more generally,
//   coinIDs), one on each party's blockchain.
// - the secret hash from the initiator contract
// - the secret from from the initiator redeem
// - 2 redemption transaction outputs (coinIDs).
//
// The methods for saving this data are defined below in the order in which the
// data is expected from the parties.

func (a *Archiver) SwapDataFullByID(mid order.MatchID) (*db.SwapDataFull, error) {
	for _, mkt := range a.markets {
		md, err := a.MatchByID(mid, mkt.Base, mkt.Quote)
		if err != nil {
			if db.IsErrMatchUnknown(err) {
				continue
			}
			return nil, err
		}
		_, sd, err := a.SwapData(db.MarketMatchID{MatchID: mid, Base: mkt.Base, Quote: mkt.Quote})
		if err != nil {
			return nil, err
		}
		return &db.SwapDataFull{Base: mkt.Base, Quote: mkt.Quote, MatchData: md, SwapData: sd}, nil
	}
	return nil, nil
}

// SwapData retrieves the match status and all the SwapData for a match.
func (a *Archiver) SwapData(mid db.MarketMatchID) (order.MatchStatus, *db.SwapData, error) {
	marketSchema, err := a.marketSchema(mid.Base, mid.Quote)
	if err != nil {
		return 0, nil, err
	}

	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
	stmt := fmt.Sprintf(internal.RetrieveSwapData, matchesTableName)

	var sd db.SwapData
	var status uint8
	var contractATime, contractBTime, redeemATime, redeemBTime sql.NullInt64
	err = a.db.QueryRow(stmt, mid).
		Scan(&status,
			&sd.SigMatchAckMaker, &sd.SigMatchAckTaker,
			&sd.MakerSwapAddr, &sd.TakerSwapAddr,
			&sd.ContractACoinID, &sd.ContractA, &contractATime,
			&sd.ContractAAckSig,
			&sd.ContractBCoinID, &sd.ContractB, &contractBTime,
			&sd.ContractBAckSig,
			&sd.RedeemACoinID, &sd.RedeemASecret, &redeemATime,
			&sd.RedeemAAckSig,
			&sd.RedeemBCoinID, &redeemBTime)
	if err != nil {
		return 0, nil, err
	}

	sd.ContractATime = contractATime.Int64
	sd.ContractBTime = contractBTime.Int64
	sd.RedeemATime = redeemATime.Int64
	sd.RedeemBTime = redeemBTime.Int64

	return order.MatchStatus(status), &sd, nil
}

// updateMatchStmt executes a SQL statement with the provided arguments,
// choosing the market's matches table from the MarketMatchID. Exactly 1 table
// row must be updated, otherwise an error is returned.
func (a *Archiver) updateMatchStmtWithExecutor(dbe sqlExecutor, mid db.MarketMatchID, stmt string, args ...any) error {
	marketSchema, err := a.marketSchema(mid.Base, mid.Quote)
	if err != nil {
		return err
	}

	matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
	stmt = fmt.Sprintf(stmt, matchesTableName)
	N, err := sqlExec(dbe, stmt, args...)
	if err != nil { // not just no rows updated
		a.fatalBackendErr(err)
		return err
	}
	if N != 1 {
		return fmt.Errorf("updateMatchStmt: updated %d match rows for match %v, expected 1", N, mid)
	}
	return nil
}

// Match acknowledgement message signatures.

func (a *Archiver) saveMatchAck(dbe sqlExecutor, ack *db.MatchAck) error {
	if ack == nil {
		return fmt.Errorf("nil match ack")
	}
	sigStmt := internal.SetTakerMatchAckSig
	addrStmt := internal.SetTakerSwapAddr
	if ack.Maker {
		sigStmt = internal.SetMakerMatchAckSig
		addrStmt = internal.SetMakerSwapAddr
	}
	if err := a.updateMatchStmtWithExecutor(dbe, ack.MID, sigStmt, ack.MID.MatchID, ack.Sig); err != nil {
		return fmt.Errorf("saving match ack signature (match id=%v, maker=%v): %w",
			ack.MID.MatchID, ack.Maker, err)
	}
	if ack.Cancel {
		return nil
	}
	if err := a.updateMatchStmtWithExecutor(dbe, ack.MID, addrStmt, ack.MID.MatchID, ack.Address); err != nil {
		return fmt.Errorf("saving match ack address (match id=%v, maker=%v): %w",
			ack.MID.MatchID, ack.Maker, err)
	}
	return nil
}

// ApplyMatchAcksRecordedEvent records match acknowledgement signatures and swap
// addresses in one transaction.
func (a *Archiver) ApplyMatchAcksRecordedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.MatchAcksRecordedUpdate) (result *db.EventLogEntry, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil match acks recorded update")
	}
	if len(update.Acks) == 0 {
		return nil, fmt.Errorf("match_acks_recorded event has no acks")
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	return a.applyEventTx(ctx, meta, meshevents.EventKindMatchAcksRecorded, txData, func(tx *sql.Tx) error {
		for _, ack := range update.Acks {
			if err := a.saveMatchAck(tx, ack); err != nil {
				return err
			}
		}
		return nil
	})
}

// Swap contracts, and counterparty audit acknowledgement signatures.

func (a *Archiver) applySwapContractRecordedEvent(dbe sqlExecutor, contract *db.SwapContract) error {
	if contract == nil {
		return fmt.Errorf("nil swap contract")
	}
	stmt := internal.SetParticipantSwapData
	status := order.TakerSwapCast
	if contract.Maker {
		stmt = internal.SetInitiatorSwapData
		status = order.MakerSwapCast
	}
	return a.updateMatchStmtWithExecutor(dbe, contract.MID, stmt, contract.MID.MatchID,
		uint8(status), contract.CoinID, contract.Contract, contract.Timestamp)
}

// ApplySwapContractRecordedEvent records a swap contract in one transaction.
func (a *Archiver) ApplySwapContractRecordedEvent(ctx context.Context, meta *db.EventLogMeta, contract *db.SwapContract) (result *db.EventLogEntry, err error) {
	if contract == nil {
		return nil, fmt.Errorf("nil swap contract")
	}
	txData, err := contract.EventTxData()
	if err != nil {
		return nil, err
	}
	return a.applyEventTx(ctx, meta, meshevents.EventKindSwapContractRecorded, txData, func(tx *sql.Tx) error {
		return a.applySwapContractRecordedEvent(tx, contract)
	})
}

func (a *Archiver) applyAuditAckRecordedEvent(dbe sqlExecutor, ack *db.AuditAck) error {
	if ack == nil {
		return fmt.Errorf("nil audit ack")
	}
	stmt := internal.SetParticipantContractAuditSig
	if ack.Maker {
		stmt = internal.SetInitiatorContractAuditSig
	}
	return a.updateMatchStmtWithExecutor(dbe, ack.MID, stmt, ack.MID.MatchID, ack.Sig)
}

// ApplyAuditAckRecordedEvent records a contract audit acknowledgement in one
// transaction.
func (a *Archiver) ApplyAuditAckRecordedEvent(ctx context.Context, meta *db.EventLogMeta, ack *db.AuditAck) (result *db.EventLogEntry, err error) {
	if ack == nil {
		return nil, fmt.Errorf("nil audit ack")
	}
	txData, err := ack.EventTxData()
	if err != nil {
		return nil, err
	}
	return a.applyEventTx(ctx, meta, meshevents.EventKindAuditAckRecorded, txData, func(tx *sql.Tx) error {
		return a.applyAuditAckRecordedEvent(tx, ack)
	})
}

// Redemption transactions, and counterparty acknowledgement signatures.

func (a *Archiver) recordRedeemData(dbe sqlExecutor, redemption *db.SwapRedemption) error {
	if redemption == nil {
		return fmt.Errorf("nil swap redemption")
	}
	stmt := internal.SetParticipantRedeemData
	status := order.MatchComplete
	args := []any{redemption.MID.MatchID, uint8(status), redemption.CoinID, redemption.Timestamp}
	if redemption.Maker {
		stmt = internal.SetInitiatorRedeemData
		status = order.MakerRedeemed
		args = []any{redemption.MID.MatchID, uint8(status), redemption.CoinID, redemption.Secret, redemption.Timestamp}
	}
	return a.updateMatchStmtWithExecutor(dbe, redemption.MID, stmt, args...)
}

func validateSwapRedemption(redemption *db.SwapRedemption) error {
	if redemption == nil {
		return fmt.Errorf("nil swap redemption")
	}
	if redemption.MID.MatchID == (order.MatchID{}) {
		return fmt.Errorf("empty swap redemption match ID")
	}
	if redemption.MID.Base == 0 && redemption.MID.Quote == 0 {
		return fmt.Errorf("empty swap redemption market for match %v", redemption.MID.MatchID)
	}
	if redemption.Timestamp <= 0 {
		return fmt.Errorf("empty swap redemption time for match %v", redemption.MID.MatchID)
	}
	return nil
}

func swapRedemptionRequiredStatus(maker bool) order.MatchStatus {
	if maker {
		return order.TakerSwapCast
	}
	return order.MakerRedeemed
}

func orderHasOtherUnsettledMatch(dbe sqlQueryer, matchesTable string, matchID order.MatchID, oid order.OrderID) (bool, error) {
	stmt := fmt.Sprintf(internal.UnsettledOrderMatchExists, matchesTable, matchesTable)
	var exists bool
	err := dbe.QueryRow(stmt, matchID, oid, uint8(order.MakerRedeemed)).Scan(&exists)
	return exists, err
}

func (a *Archiver) applyOrderCompletionIfSettled(
	dbe sqlQueryExecutor,
	matchesTable string,
	repUpdates *reputationOutcomeBatch,
	mid db.MarketMatchID,
	oid order.OrderID,
	user account.AccountID,
	completeTimeMS int64,
) error {
	status, _, _, err := a.orderStatusByIDWithExecutor(dbe, oid, mid.Base, mid.Quote)
	if err != nil {
		return err
	}
	if status != orderStatusExecuted {
		return nil
	}
	hasUnsettled, err := orderHasOtherUnsettledMatch(dbe, matchesTable, mid.MatchID, oid)
	if err != nil {
		return err
	}
	if hasUnsettled {
		return nil
	}
	if err := a.setOrderCompleteTimeByID(dbe, oid, mid.Base, mid.Quote, completeTimeMS); err != nil {
		return err
	}
	repUpdates.orders = append(repUpdates.orders, &reputationOrderOutcome{
		user: user,
		oid:  oid,
	})
	return nil
}

// ApplySwapRedemptionRecordedEvent records a swap redemption and all DB side
// effects owned by the event in one transaction.
func (a *Archiver) ApplySwapRedemptionRecordedEvent(ctx context.Context, meta *db.EventLogMeta, policy *db.ReputationOutcomePolicy, redemption *db.SwapRedemption) (*db.EventLogEntry, error) {
	if err := validateSwapRedemption(redemption); err != nil {
		return nil, err
	}
	baseTxData, err := redemption.EventTxData()
	if err != nil {
		return nil, err
	}

	return a.applyRepEventTx(ctx, meta, meshevents.EventKindSwapRedemptionRecorded, baseTxData, policy, func(tx *sql.Tx, repUpdates *reputationOutcomeBatch) error {
		marketSchema, err := a.marketSchema(redemption.MID.Base, redemption.MID.Quote)
		if err != nil {
			return err
		}
		matchesTableName := fullMatchesTableName(a.dbName, marketSchema)

		matchData, err := matchByID(tx, matchesTableName, redemption.MID.MatchID)
		if errors.Is(err, sql.ErrNoRows) {
			err = db.ArchiveError{Code: db.ErrUnknownMatch}
		}
		if err != nil {
			return err
		}
		requiredStatus := swapRedemptionRequiredStatus(redemption.Maker)
		if matchData.Status != requiredStatus {
			return fmt.Errorf("swap redemption recorded event requires status %v, found %v for match %v",
				requiredStatus, matchData.Status, redemption.MID.MatchID)
		}
		if err := a.recordRedeemData(tx, redemption); err != nil {
			return err
		}

		actor, counterparty := matchData.TakerAcct, matchData.MakerAcct
		actorOrder := matchData.Taker
		if redemption.Maker {
			actor, counterparty = matchData.MakerAcct, matchData.TakerAcct
			actorOrder = matchData.Maker
		}
		if actor != counterparty {
			repUpdates.matches = append(repUpdates.matches, &reputationMatchOutcome{
				user:    actor,
				mid:     redemption.MID,
				outcome: db.OutcomeSwapSuccess,
			})
		}
		return a.applyOrderCompletionIfSettled(tx, matchesTableName, repUpdates,
			redemption.MID, actorOrder, actor, redemption.Timestamp)
	})
}

func (a *Archiver) applyRedemptionAckRecordedEvent(dbe sqlExecutor, ack *db.RedemptionAck) error {
	if ack == nil {
		return fmt.Errorf("nil redemption ack")
	}
	if ack.Maker {
		return nil
	}
	return a.updateMatchStmtWithExecutor(dbe, ack.MID, internal.SetParticipantRedeemAckSig,
		ack.MID.MatchID, ack.Sig)
}

// ApplyRedemptionAckRecordedEvent records a redemption acknowledgement in one
// transaction.
func (a *Archiver) ApplyRedemptionAckRecordedEvent(ctx context.Context, meta *db.EventLogMeta, ack *db.RedemptionAck) (result *db.EventLogEntry, err error) {
	if ack == nil {
		return nil, fmt.Errorf("nil redemption ack")
	}
	txData, err := ack.EventTxData()
	if err != nil {
		return nil, err
	}
	return a.applyEventTx(ctx, meta, meshevents.EventKindRedemptionAckRecorded, txData, func(tx *sql.Tx) error {
		return a.applyRedemptionAckRecordedEvent(tx, ack)
	})
}

// setMatchInactive flags the match as done/inactive.
func (a *Archiver) setMatchInactive(dbe sqlExecutor, mid db.MarketMatchID, forgive bool) error {
	if forgive {
		return a.updateMatchStmtWithExecutor(dbe, mid, internal.SetSwapDoneForgiven, mid.MatchID)
	}
	return a.updateMatchStmtWithExecutor(dbe, mid, internal.SetSwapDone, mid.MatchID)
}

func validateMatchFailedUpdate(update *db.MatchFailedUpdate) error {
	if update == nil {
		return fmt.Errorf("nil match failed update")
	}
	if update.MID.MatchID == (order.MatchID{}) {
		return fmt.Errorf("empty match_failed match ID")
	}
	if update.MID.Base == 0 && update.MID.Quote == 0 {
		return fmt.Errorf("empty match_failed market for match %v", update.MID.MatchID)
	}
	if update.FailTimeMS <= 0 {
		return fmt.Errorf("empty match_failed time for match %v", update.MID.MatchID)
	}
	if _, ok := db.MatchFailureReasonDetails(update.Reason); !ok {
		return fmt.Errorf("invalid match failure reason %d", update.Reason)
	}
	return nil
}

func (a *Archiver) applyMatchFailedOrderSide(
	dbe sqlQueryExecutor,
	matchesTable string,
	repUpdates *reputationOutcomeBatch,
	mid db.MarketMatchID,
	oid order.OrderID,
	user account.AccountID,
	faulted bool,
	failTimeMS int64,
) error {
	status, ordType, _, err := a.orderStatusByIDWithExecutor(dbe, oid, mid.Base, mid.Quote)
	if err != nil {
		return err
	}

	if faulted {
		if status != orderStatusBooked {
			return nil
		}
		if ordType != order.LimitOrderType {
			return fmt.Errorf("cannot revoke match_failed order %v in status %v with type %v", oid, status, ordType)
		}
		cancelID, err := a.revokeOrderByID(dbe, oid, user, mid.Base, mid.Quote, false, time.UnixMilli(failTimeMS).UTC())
		if err != nil {
			return err
		}
		repUpdates.orders = append(repUpdates.orders, &reputationOrderOutcome{
			user: user,
			oid:  cancelID,
		})
		return nil
	}

	if status != orderStatusExecuted {
		return nil
	}
	return a.applyOrderCompletionIfSettled(dbe, matchesTable, repUpdates, mid, oid, user, failTimeMS)
}

// ApplyMatchFailedEvent applies the match_failed event's persistent state
// transition in one transaction.
func (a *Archiver) ApplyMatchFailedEvent(ctx context.Context, meta *db.EventLogMeta, policy *db.ReputationOutcomePolicy, update *db.MatchFailedUpdate) (*db.EventLogEntry, error) {
	if err := validateMatchFailedUpdate(update); err != nil {
		return nil, err
	}
	baseTxData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	logEntry, err := a.applyRepEventTx(ctx, meta, meshevents.EventKindMatchFailed, baseTxData, policy, func(tx *sql.Tx, repUpdates *reputationOutcomeBatch) error {
		marketSchema, err := a.marketSchema(update.MID.Base, update.MID.Quote)
		if err != nil {
			return err
		}
		matchesTableName := fullMatchesTableName(a.dbName, marketSchema)
		matchData, err := matchByID(tx, matchesTableName, update.MID.MatchID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = db.ArchiveError{Code: db.ErrUnknownMatch}
			}
			return err
		}
		details, _ := db.MatchFailureReasonDetails(update.Reason)
		if matchData.Status != details.Status {
			return fmt.Errorf("match_failed reason %d requires status %v, found %v for match %v",
				update.Reason, details.Status, matchData.Status, update.MID.MatchID)
		}

		if err := a.setMatchInactive(tx, update.MID, !details.UserFault()); err != nil {
			return err
		}

		if details.UserFault() && matchData.MakerAcct != matchData.TakerAcct {
			user := matchData.TakerAcct
			if details.MakerFault() {
				user = matchData.MakerAcct
			}
			repUpdates.matches = append(repUpdates.matches, &reputationMatchOutcome{
				user:    user,
				mid:     update.MID,
				outcome: details.Outcome,
			})
		}

		if details.ProcessMaker() {
			if err := a.applyMatchFailedOrderSide(tx, matchesTableName, repUpdates, update.MID,
				matchData.Maker, matchData.MakerAcct, details.MakerFault(), update.FailTimeMS); err != nil {
				return err
			}
		}

		return a.applyMatchFailedOrderSide(tx, matchesTableName, repUpdates, update.MID,
			matchData.Taker, matchData.TakerAcct, details.TakerFault(), update.FailTimeMS)
	})
	if err != nil {
		return nil, err
	}
	return logEntry, nil
}
