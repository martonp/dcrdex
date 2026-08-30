// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/db/driver/pg/internal"
	"decred.org/dcrdex/server/meshevents"
)

// Account retrieves the account pubkey and active bonds. A nil *account.Account
// with a nil error means the account is unknown. A non-nil error means the
// account's existence could not be determined; callers must not treat that as
// an unknown account.
func (a *Archiver) Account(aid account.AccountID, bondExpiry time.Time) (acct *account.Account, bonds []*db.Bond, err error) {
	acct, err = getAccount(a.db, a.tables.accounts, aid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("getAccount error: %w", err)
	}

	bonds, err = getBondsForAccount(a.db, a.tables.bonds, aid, bondExpiry.Unix())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		bonds = nil
	case err != nil:
		return nil, nil, fmt.Errorf("getBondsForAccount error: %w", err)
	}

	return acct, bonds, nil
}

// ApplyBondPostedEvent applies the auth bond_posted event in one transaction.
func (a *Archiver) ApplyBondPostedEvent(ctx context.Context, meta *db.EventLogMeta, update *db.BondPostedUpdate) (result *db.BondPostedResult, err error) {
	if update == nil {
		return nil, fmt.Errorf("nil bond posted update")
	}
	acct, bond := update.Acct, update.Bond
	if acct == nil {
		return nil, fmt.Errorf("nil bond posted account")
	}
	if bond == nil {
		return nil, fmt.Errorf("nil posted bond")
	}
	txData, err := update.EventTxData()
	if err != nil {
		return nil, err
	}

	result = new(db.BondPostedResult)
	logEntry, err := a.applyEventTx(ctx, meta, meshevents.EventKindBondPosted, txData, func(dbTx *sql.Tx) error {
		storedAcct, err := getAccount(dbTx, a.tables.accounts, acct.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err = createAccountForBond(dbTx, a.tables.accounts, acct); err != nil {
				return err
			}
		case err != nil:
			return err
		case storedAcct.PubKey == nil || !bytes.Equal(storedAcct.PubKey.SerializeCompressed(), acct.PubKey.SerializeCompressed()):
			return fmt.Errorf("bond_posted account pubkey mismatch for %v", acct.ID)
		}

		bondAcct, err := getBondAccount(dbTx, a.tables.bonds, bond.AssetID, bond.CoinID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case bondAcct == acct.ID:
			return nil
		default:
			return fmt.Errorf("bond_posted bond %x asset %d already belongs to account %v",
				bond.CoinID, bond.AssetID, bondAcct)
		}

		if bond.AssetID == account.PrepaidBondID {
			strength, lockTime, err := getPrepaidBond(dbTx, a.tables.prepaidBonds, bond.CoinID)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("bond_posted pre-paid bond %x not found", bond.CoinID)
			}
			if err != nil {
				return err
			}
			if strength != bond.Strength {
				return fmt.Errorf("bond_posted pre-paid bond %x strength mismatch: got %d, want %d",
					bond.CoinID, bond.Strength, strength)
			}
			if lockTime != bond.LockTime {
				return fmt.Errorf("bond_posted pre-paid bond %x lock time mismatch: got %d, want %d",
					bond.CoinID, bond.LockTime, lockTime)
			}
		}

		if err = addBond(dbTx, a.tables.bonds, acct.ID, bond); err != nil {
			return err
		}
		if bond.AssetID == account.PrepaidBondID {
			if err = deletePrepaidBond(dbTx, a.tables.prepaidBonds, bond.CoinID); err != nil {
				return err
			}
		}
		result.BondAdded = true
		return nil
	})
	a.notifyRepInputsOnCommit(err, acct.ID)
	if err != nil {
		return nil, err
	}
	result.Log = logEntry
	return result, nil
}

// ApplyPrepaidBondsCreatedEvent applies the auth prepaid_bonds_created event in
// one transaction.
func (a *Archiver) ApplyPrepaidBondsCreatedEvent(ctx context.Context, meta *db.EventLogMeta, event *meshevents.PrepaidBondsCreatedEvent) (*db.EventLogEntry, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	txData, err := event.EventTxData()
	if err != nil {
		return nil, err
	}

	return a.applyEventTx(ctx, meta, meshevents.EventKindPrepaidBondsCreated, txData, func(dbTx *sql.Tx) error {
		for _, bond := range event.Bonds {
			if err := insertPrepaidBond(dbTx, a.tables.prepaidBonds, bond); err != nil {
				return err
			}
		}
		return nil
	})
}

// AccountInfo returns data for an account.
func (a *Archiver) AccountInfo(aid account.AccountID) (*db.Account, error) {
	// bondExpiry time.Time and bonds return needed?
	stmt := fmt.Sprintf(internal.SelectAccountInfo, a.tables.accounts)
	acct := new(db.Account)
	if err := a.db.QueryRow(stmt, aid).Scan(&acct.AccountID, &acct.Pubkey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = db.ArchiveError{Code: db.ErrAccountUnknown}
		}
		return nil, err
	}
	return acct, nil
}

func (a *Archiver) FetchPrepaidBond(coinID []byte) (strength uint32, lockTime int64, err error) {
	return getPrepaidBond(a.db, a.tables.prepaidBonds, coinID)
}

// createAccountTables creates the account-related tables.
func createAccountTables(db sqlQueryExecutor) error {
	for _, c := range createAccountTableStatements {
		created, err := createTable(db, publicSchema, c.name)
		if err != nil {
			return err
		}
		if created {
			log.Tracef("Table %s created", c.name)
		}
	}

	for _, c := range createBondIndexesStatements {
		err := createIndexStmt(db, c.stmt, c.idxName, bondsTableName)
		if err != nil {
			return err
		}
	}

	return nil
}

// getAccount gets retrieves the account details, including the pubkey, a flag
// indicating if the account was created with a legacy fee address (not a
// fidelity bond), and a flag indicating if that legacy fee was paid.
func getAccount(dbe sqlQueryer, tableName string, aid account.AccountID) (acct *account.Account, err error) {
	var pubkey []byte
	stmt := fmt.Sprintf(internal.SelectAccount, tableName)
	err = dbe.QueryRow(stmt, aid).Scan(&pubkey)
	if err != nil {
		return
	}
	acct, err = account.NewAccountFromPubKey(pubkey)
	if err != nil {
		return
	}
	return
}

// createAccountForBond creates an entry for the account in the accounts table.
func createAccountForBond(dbe sqlExecutor, tableName string, acct *account.Account) error {
	stmt := fmt.Sprintf(internal.CreateAccountForBond, tableName)
	_, err := dbe.Exec(stmt, acct.ID, acct.PubKey.SerializeCompressed())
	return err
}

func addBond(dbe sqlExecutor, tableName string, aid account.AccountID, bond *db.Bond) error {
	stmt := fmt.Sprintf(internal.AddBond, tableName)
	_, err := dbe.Exec(stmt, bond.Version, bond.CoinID, bond.AssetID, aid,
		bond.Amount, bond.Strength, bond.LockTime)
	return err
}

func getBondAccount(dbe sqlQueryer, tableName string, assetID uint32, coinID []byte) (acct account.AccountID, err error) {
	stmt := fmt.Sprintf(internal.SelectBondAccount, tableName)
	err = dbe.QueryRow(stmt, coinID, assetID).Scan(&acct)
	return
}

func getPrepaidBond(dbe sqlQueryer, tableName string, coinID []byte) (strength uint32, lockTime int64, err error) {
	stmt := fmt.Sprintf(internal.SelectPrepaidBond, tableName)
	err = dbe.QueryRow(stmt, coinID).Scan(&strength, &lockTime)
	return
}

func deletePrepaidBond(dbe sqlExecutor, tableName string, coinID []byte) error {
	stmt := fmt.Sprintf(internal.DeletePrepaidBond, tableName)
	_, err := dbe.Exec(stmt, coinID)
	return err
}

func insertPrepaidBond(dbe sqlExecutor, tableName string, bond *meshevents.PrepaidBond) error {
	stmt := fmt.Sprintf(internal.InsertPrepaidBond, tableName)
	_, err := dbe.Exec(stmt, bond.CoinID, bond.Strength, bond.LockTime)
	return err
}

func getBondsForAccount(dbe sqlQueryer, tableName string, acct account.AccountID, bondExpiryTime int64) ([]*db.Bond, error) {
	stmt := fmt.Sprintf(internal.SelectActiveBondsForUser, tableName)
	rows, err := dbe.Query(stmt, acct, bondExpiryTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bonds []*db.Bond
	for rows.Next() {
		var bond db.Bond
		err = rows.Scan(&bond.Version, &bond.CoinID, &bond.AssetID,
			&bond.Amount, &bond.Strength, &bond.LockTime)
		if err != nil {
			return nil, err
		}
		bonds = append(bonds, &bond)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return bonds, nil
}
