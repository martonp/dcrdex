// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
)

var _ db.ReputationArchiver = (*Archiver)(nil)

// SetReputationInputsListener implements db.ReputationArchiver.
func (a *Archiver) SetReputationInputsListener(listener func(users ...account.AccountID)) {
	a.repListenerMtx.Lock()
	defer a.repListenerMtx.Unlock()
	if a.repListener != nil {
		panic("reputation inputs listener already registered")
	}
	a.repListener = listener
}

func commitMayHaveLanded(err error) bool {
	return err == nil || errors.As(err, new(*db.EventCommitUnknownError))
}

// notifyRepInputsOnCommit fires the reputation inputs listener if needed.
func (a *Archiver) notifyRepInputsOnCommit(err error, users ...account.AccountID) {
	if len(users) == 0 || !commitMayHaveLanded(err) {
		return
	}
	a.repListenerMtx.RLock()
	listener := a.repListener
	a.repListenerMtx.RUnlock()
	if listener != nil {
		listener(users...)
	}
}

func (a *Archiver) GetUserReputationData(
	ctx context.Context,
	user account.AccountID,
	pimgSz, matchSz, orderSz int, /* pre-allocation sizes */
) ([]*db.PreimageOutcome, []*db.MatchResult, []*db.OrderOutcome, error) {
	rows, err := a.queries.selectPoints.QueryContext(ctx, user)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error querying reputation points: %w", err)
	}
	defer rows.Close()

	pimgs := make([]*db.PreimageOutcome, 0, pimgSz)
	matches := make([]*db.MatchResult, 0, matchSz)
	orders := make([]*db.OrderOutcome, 0, orderSz)

	for rows.Next() {
		var dbID int64
		var link order.OrderID
		var outcomeClass db.OutcomeClass
		var outcome db.Outcome
		if err := rows.Scan(&dbID, &link, &outcomeClass, &outcome); err != nil {
			return nil, nil, nil, fmt.Errorf("error scanning points row: %w", err)
		}
		switch outcomeClass {
		case db.OutcomeClassPreimage:
			pimgs = append(pimgs, &db.PreimageOutcome{
				DBID:    dbID,
				OrderID: link,
				Miss:    outcome == db.OutcomePreimageMiss,
			})
		case db.OutcomeClassMatch:
			var mid order.MatchID
			copy(mid[:], link[:])
			matches = append(matches, &db.MatchResult{
				DBID:         dbID,
				MatchID:      mid,
				MatchOutcome: outcome,
			})
		case db.OutcomeClassOrder:
			orders = append(orders, &db.OrderOutcome{
				DBID:     dbID,
				OrderID:  link,
				Canceled: outcome == db.OutcomeOrderCanceled,
			})
		}
	}
	if rows.Err() != nil {
		return nil, nil, nil, fmt.Errorf("error iterating points rows: %w", rows.Err())
	}
	if len(pimgs) > pimgSz {
		pimgs = pimgs[len(pimgs)-pimgSz:]
	}
	if len(matches) > matchSz {
		matches = matches[len(matches)-matchSz:]
	}
	if len(orders) > orderSz {
		orders = orders[len(orders)-orderSz:]
	}
	return pimgs, matches, orders, nil
}

func (a *Archiver) insertPoints(
	ctx context.Context,
	dbe sqlQueryer,
	user account.AccountID,
	link [32]byte,
	outcomeClass db.OutcomeClass,
	outcome db.Outcome,
) (dbID int64, _ error) {
	var oid order.OrderID // need a sql.Scanner
	copy(oid[:], link[:])
	stmt := fmt.Sprintf(internal.InsertPoints, a.tables.points)
	return dbID, dbe.QueryRowContext(ctx, stmt, user, oid, outcomeClass, outcome).Scan(&dbID)
}

// applyUserForgivenessTx applies user-scoped reputation forgiveness by
// deleting every non-success outcome for the account, exactly as the legacy
// ForgiveUser did. It reports whether any rows were deleted.
func (a *Archiver) applyUserForgivenessTx(ctx context.Context, tx *sql.Tx, accountID account.AccountID) (forgiven bool, err error) {
	stmt := fmt.Sprintf(internal.ForgiveUser, a.tables.points)
	res, err := tx.ExecContext(ctx, stmt, accountID, db.OutcomeSwapSuccess, db.OutcomePreimageSuccess, db.OutcomeOrderComplete)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// applyMatchForgivenessTx applies match-scoped reputation forgiveness. As with
// the legacy ForgiveMatchFail, the match row is marked forgiven if it is
// inactive, with the burden on the operator to ensure the match should
// actually be forgiven. The account's failure points for the match are also
// deleted so the forgiveness is reflected in the conduct score. It reports
// whether a match row was updated.
func (a *Archiver) applyMatchForgivenessTx(ctx context.Context, tx *sql.Tx, accountID account.AccountID, matchID order.MatchID) (forgiven bool, err error) {
	for schema := range a.markets {
		stmt := fmt.Sprintf(internal.ForgiveMatchFail, fullMatchesTableName(a.dbName, schema))
		res, err := tx.ExecContext(ctx, stmt, matchID)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		if n > 0 { // at most one market has the match, matchid is the primary key
			forgiven = true
			break
		}
	}
	if !forgiven {
		return false, nil
	}

	stmt := fmt.Sprintf(internal.ForgiveMatchFailures, a.tables.points)
	var link order.OrderID // need a sql driver Valuer
	copy(link[:], matchID[:])
	if _, err := tx.ExecContext(ctx, stmt, accountID, link, db.OutcomeClassMatch, db.OutcomeSwapSuccess); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyReputationForgivenEvent applies the reputation_forgiven event in one
// database transaction.
func (a *Archiver) ApplyReputationForgivenEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.ReputationForgivenEvent) (*db.ReputationForgivenResult, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	txData, err := event.EventTxData()
	if err != nil {
		return nil, err
	}
	accountID := event.AccountID
	matchID := event.Match()

	var forgiven bool
	logEntry, err := a.applyEventTx(ctx, meta, meshevents.EventKindReputationForgiven, txData, func(tx *sql.Tx) error {
		var err error
		switch event.Scope {
		case meshevents.ReputationForgivenessScopeUser:
			forgiven, err = a.applyUserForgivenessTx(ctx, tx, accountID)
		case meshevents.ReputationForgivenessScopeMatch:
			forgiven, err = a.applyMatchForgivenessTx(ctx, tx, accountID, matchID)
		default:
			err = fmt.Errorf("invalid reputation forgiveness scope %d", event.Scope)
		}
		return err
	})
	a.notifyRepInputsOnCommit(err, accountID)
	if err != nil {
		return nil, err
	}

	return &db.ReputationForgivenResult{
		Forgiven: forgiven,
		Log:      logEntry,
	}, nil
}

type reputationClassKey struct {
	user  account.AccountID
	class db.OutcomeClass
}

type reputationPreimageOutcome struct {
	user account.AccountID
	oid  order.OrderID
	miss bool
}

type reputationMatchOutcome struct {
	user    account.AccountID
	mid     db.MarketMatchID
	outcome db.Outcome
}

type reputationOrderOutcome struct {
	user            account.AccountID
	oid             order.OrderID
	penalizedCancel bool
}

type reputationOutcomeBatch struct {
	preimages []*reputationPreimageOutcome
	matches   []*reputationMatchOutcome
	orders    []*reputationOrderOutcome
}

func reputationClassKeys(updates *reputationOutcomeBatch) []reputationClassKey {
	if updates == nil {
		return nil
	}
	keys := make(map[reputationClassKey]struct{})
	for _, update := range updates.preimages {
		if update != nil {
			keys[reputationClassKey{user: update.user, class: db.OutcomeClassPreimage}] = struct{}{}
		}
	}
	for _, update := range updates.matches {
		if update != nil {
			keys[reputationClassKey{user: update.user, class: db.OutcomeClassMatch}] = struct{}{}
		}
	}
	for _, update := range updates.orders {
		if update != nil {
			keys[reputationClassKey{user: update.user, class: db.OutcomeClassOrder}] = struct{}{}
		}
	}
	distinct := make([]reputationClassKey, 0, len(keys))
	for key := range keys {
		distinct = append(distinct, key)
	}
	return distinct
}

func reputationLimit(policy *db.ReputationOutcomePolicy, class db.OutcomeClass) int {
	if policy == nil {
		return 0
	}
	switch class {
	case db.OutcomeClassPreimage:
		return policy.PreimageLimit
	case db.OutcomeClassMatch:
		return policy.MatchLimit
	case db.OutcomeClassOrder:
		return policy.OrderLimit
	default:
		return 0
	}
}

func validMatchOutcome(outcome db.Outcome) bool {
	switch outcome {
	case db.OutcomeSwapSuccess, db.OutcomeNoSwapAsMaker, db.OutcomeNoSwapAsTaker,
		db.OutcomeNoRedeemAsMaker, db.OutcomeNoRedeemAsTaker, db.OutcomeNoAddrAsTaker:
		return true
	default:
		return false
	}
}

func validateReputationOutcomeUpdates(policy *db.ReputationOutcomePolicy, updates *reputationOutcomeBatch) ([]reputationClassKey, error) {
	if updates == nil {
		return nil, fmt.Errorf("nil reputation outcome updates")
	}
	var zeroUser account.AccountID
	var zeroOID order.OrderID
	var zeroMID order.MatchID
	for i, update := range updates.preimages {
		if update == nil {
			return nil, fmt.Errorf("nil preimage reputation update at index %d", i)
		}
		if update.user == zeroUser {
			return nil, fmt.Errorf("zero user in preimage reputation update")
		}
		if update.oid == zeroOID {
			return nil, fmt.Errorf("zero order id in preimage reputation update")
		}
	}
	for i, update := range updates.matches {
		if update == nil {
			return nil, fmt.Errorf("nil match reputation update at index %d", i)
		}
		if update.user == zeroUser {
			return nil, fmt.Errorf("zero user in match reputation update")
		}
		if update.mid.MatchID == zeroMID {
			return nil, fmt.Errorf("zero match id in match reputation update")
		}
		if !validMatchOutcome(update.outcome) {
			return nil, fmt.Errorf("invalid match reputation outcome %d", update.outcome)
		}
	}
	for i, update := range updates.orders {
		if update == nil {
			return nil, fmt.Errorf("nil order reputation update at index %d", i)
		}
		if update.user == zeroUser {
			return nil, fmt.Errorf("zero user in order reputation update")
		}
		if update.oid == zeroOID {
			return nil, fmt.Errorf("zero order id in order reputation update")
		}
	}
	keys := reputationClassKeys(updates)
	if len(keys) == 0 {
		return nil, nil
	}
	for _, key := range keys {
		if reputationLimit(policy, key.class) <= 0 {
			return nil, fmt.Errorf("missing reputation outcome limit for class %d", key.class)
		}
	}
	return keys, nil
}

func outcomeBatchUsers(updates *reputationOutcomeBatch) []account.AccountID {
	keys := reputationClassKeys(updates)
	seen := make(map[account.AccountID]struct{}, len(keys))
	users := make([]account.AccountID, 0, len(keys))
	for _, key := range keys {
		if _, found := seen[key.user]; found {
			continue
		}
		seen[key.user] = struct{}{}
		users = append(users, key.user)
	}
	return users
}

// applyRepEventTx is applyEventTx for events that write reputation outcomes.
// apply stages the batch; outcomes are inserted in-tx and the rep-inputs
// listener is notified after commit. Call insertReputationOutcomeRows only here.
func (a *Archiver) applyRepEventTx(ctx context.Context, meta *db.EventLogMeta, kind string, txData []byte,
	policy *db.ReputationOutcomePolicy, apply func(*sql.Tx, *reputationOutcomeBatch) error) (*db.EventLogEntry, error) {

	batch := new(reputationOutcomeBatch)
	logEntry, err := a.applyEventTx(ctx, meta, kind, txData, func(tx *sql.Tx) error {
		if err := apply(tx, batch); err != nil {
			return err
		}
		return a.insertReputationOutcomeRows(tx, policy, batch)
	})
	if commitMayHaveLanded(err) {
		a.notifyRepInputsOnCommit(err, outcomeBatchUsers(batch)...)
	}
	return logEntry, err
}

// insertReputationOutcomeRows writes and prunes outcome rows for the batch.
func (a *Archiver) insertReputationOutcomeRows(
	dbe *sql.Tx,
	policy *db.ReputationOutcomePolicy,
	updates *reputationOutcomeBatch,
) error {
	keys, err := validateReputationOutcomeUpdates(policy, updates)
	if err != nil || len(keys) == 0 {
		return err
	}
	failDBErr := func(err error) error {
		a.fatalBackendErr(err)
		return err
	}
	for _, update := range updates.preimages {
		outcome := db.OutcomePreimageSuccess
		if update.miss {
			outcome = db.OutcomePreimageMiss
		}
		if _, err := a.insertPoints(a.ctx, dbe, update.user, update.oid, db.OutcomeClassPreimage, outcome); err != nil {
			return failDBErr(err)
		}
	}
	for _, update := range updates.matches {
		if _, err := a.insertPoints(a.ctx, dbe, update.user, update.mid.MatchID, db.OutcomeClassMatch, update.outcome); err != nil {
			return failDBErr(err)
		}
	}
	for _, update := range updates.orders {
		outcome := db.OutcomeOrderComplete
		if update.penalizedCancel {
			outcome = db.OutcomeOrderCanceled
		}
		if _, err := a.insertPoints(a.ctx, dbe, update.user, update.oid, db.OutcomeClassOrder, outcome); err != nil {
			return failDBErr(err)
		}
	}

	pruneStmt := fmt.Sprintf(internal.PrunePointsPastLimit, a.tables.points)
	for _, key := range keys {
		if _, err := dbe.Exec(pruneStmt, key.user, key.class, reputationLimit(policy, key.class)); err != nil {
			return failDBErr(err)
		}
	}
	return nil
}
