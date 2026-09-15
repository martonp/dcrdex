//go:build pgonline

package pg

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/meshevents"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

var tPubKey = []byte{
	0x02, 0x04, 0x98, 0x8a, 0x49, 0x8d, 0x5d, 0x19, 0x51, 0x4b, 0x21, 0x7e, 0x87,
	0x2b, 0x4d, 0xbd, 0x1c, 0xf0, 0x71, 0xd3, 0x65, 0xc4, 0x87, 0x9e, 0x64, 0xed,
	0x59, 0x19, 0x88, 0x1c, 0x97, 0xeb, 0x19,
}

var tAcctID = account.AccountID{
	0x0a, 0x99, 0x12, 0x20, 0x5b, 0x2c, 0xba, 0xb0, 0xc2, 0x5c, 0x2d, 0xe3, 0x0b,
	0xda, 0x90, 0x74, 0xde, 0x0a, 0xe2, 0x3b, 0x06, 0x54, 0x89, 0xa9, 0x91, 0x99,
	0xba, 0xd7, 0x63, 0xf1, 0x02, 0xcc,
}

func tNewAccount(t *testing.T) *account.Account {
	acct, err := account.NewAccountFromPubKey(tPubKey)
	if err != nil {
		t.Fatalf("error creating account from pubkey: %v", err)
	}
	if acct.ID != tAcctID {
		t.Fatalf("unexpected account ID. wanted %x, got %x", tAcctID, acct.ID)
	}
	return acct
}

func tAccountFromPrivKey(t *testing.T, keyByte byte) *account.Account {
	t.Helper()

	var priv [account.PrivKeySize]byte
	priv[len(priv)-1] = keyByte
	acct, err := account.NewAccountFromPubKey(secp256k1.PrivKeyFromBytes(priv[:]).PubKey().SerializeCompressed())
	if err != nil {
		t.Fatalf("error creating account from privkey byte %d: %v", keyByte, err)
	}
	return acct
}

func tAssertBond(t *testing.T, got, want *db.Bond) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bond = %+v, want %+v", got, want)
	}
}

func tBondPostedEvent(acct *account.Account, bond *db.Bond) *meshevents.BondPostedEvent {
	return meshevents.NewBondPostedEvent(acct, &meshevents.Bond{
		Version:  bond.Version,
		AssetID:  bond.AssetID,
		CoinID:   bond.CoinID,
		Amount:   bond.Amount,
		Strength: bond.Strength,
		LockTime: bond.LockTime,
	})
}

func tBondPostedTip(t *testing.T, prevTip []byte, seq uint64, event []byte, acct *account.Account, bond *db.Bond) []byte {
	t.Helper()

	posted := tBondPostedEvent(acct, bond)
	txData, err := posted.EventTxData()
	if err != nil {
		t.Fatalf("EventTxData error: %v", err)
	}
	req, err := newEventLogAppend(&db.EventLogMeta{Seq: seq, Event: event}, meshevents.EventKindBondPosted, txData)
	if err != nil {
		t.Fatalf("newEventLogAppend error: %v", err)
	}
	return eventLogHash(prevTip, seq, req.entry.Kind, req.entry.Event, req.entry.TxData)
}

func TestApplyBondPostedEvent(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	ctx := context.Background()
	calls := captureRepListener(t)
	acct := tNewAccount(t)
	bond := &db.Bond{
		Version:  1,
		AssetID:  AssetDCR,
		CoinID:   []byte("apply-bond-posted-coin"),
		Amount:   5_0000_0000,
		Strength: 5,
		LockTime: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}
	posted := tBondPostedEvent(acct, bond)
	// Only the latest outcome in each class should be returned.
	seedReputationOutcome(t, ctx, acct.ID, randomReputationOrderID(), db.OutcomeClassPreimage, db.OutcomePreimageSuccess)
	seedReputationOutcome(t, ctx, acct.ID, randomReputationMatchID(), db.OutcomeClassMatch, db.OutcomeSwapSuccess)
	seedReputationOutcome(t, ctx, acct.ID, randomReputationOrderID(), db.OutcomeClassOrder, db.OutcomeOrderComplete)
	preimageID := randomReputationOrderID()
	matchID := randomReputationMatchID()
	orderID := randomReputationOrderID()
	seedReputationOutcome(t, ctx, acct.ID, preimageID, db.OutcomeClassPreimage, db.OutcomePreimageMiss)
	seedReputationOutcome(t, ctx, acct.ID, matchID, db.OutcomeClassMatch, db.OutcomeNoSwapAsTaker)
	seedReputationOutcome(t, ctx, acct.ID, orderID, db.OutcomeClassOrder, db.OutcomeOrderCanceled)
	seedReputationOutcome(t, ctx, tAccountFromPrivKey(t, 3).ID, orderID, db.OutcomeClassOrder, db.OutcomeOrderComplete)

	applyBondPosted := func(meta *db.EventLogMeta) *db.BondPostedResult {
		t.Helper()

		res, err := archie.ApplyBondPostedEvent(ctx, meta, posted, 1, 1, 1)
		if err != nil {
			t.Fatalf("ApplyBondPostedEvent error: %v", err)
		}
		if res == nil || res.Log == nil {
			t.Fatalf("ApplyBondPostedEvent returned nil result or log: %+v", res)
		}
		if len(res.Bonds) != 1 {
			t.Fatalf("returned %d bonds, want 1", len(res.Bonds))
		}
		tAssertBond(t, res.Bonds[0], bond)
		if len(res.Preimages) != 1 || res.Preimages[0].OrderID != preimageID || !res.Preimages[0].Miss {
			t.Fatalf("unexpected returned preimage outcomes: %+v", res.Preimages)
		}
		if len(res.Matches) != 1 || res.Matches[0].MatchID != matchID || res.Matches[0].MatchOutcome != db.OutcomeNoSwapAsTaker {
			t.Fatalf("unexpected returned match outcomes: %+v", res.Matches)
		}
		if len(res.Orders) != 1 || res.Orders[0].OrderID != orderID || !res.Orders[0].Canceled {
			t.Fatalf("unexpected returned order outcomes: %+v", res.Orders)
		}
		return res
	}

	requireLog := func(res *db.BondPostedResult, seq uint64, event, tip []byte) {
		t.Helper()

		wantTxData, err := posted.EventTxData()
		if err != nil {
			t.Fatalf("EventTxData error: %v", err)
		}
		if res.Log.Seq != seq || res.Log.Kind != meshevents.EventKindBondPosted || !bytes.Equal(res.Log.Event, event) {
			t.Fatalf("log = %+v, want seq %d kind %q event %q", res.Log, seq, meshevents.EventKindBondPosted, event)
		}
		if !bytes.Equal(res.Log.TipHash, tip) {
			t.Fatalf("log tip = %x, want %x", res.Log.TipHash, tip)
		}
		if !bytes.Equal(res.Log.TxData, wantTxData) {
			t.Fatalf("tx data = %x, want %x", res.Log.TxData, wantTxData)
		}
	}

	requireStoredBond := func() {
		t.Helper()

		storedAcct, bonds, err := archie.Account(ctx, acct.ID, time.Unix(bond.LockTime-1, 0))
		if err != nil {
			t.Fatalf("Account error: %v", err)
		}
		if storedAcct == nil {
			t.Fatalf("account %v was not stored", acct.ID)
		}
		if !bytes.Equal(storedAcct.PubKey.SerializeCompressed(), acct.PubKey.SerializeCompressed()) {
			t.Fatalf("stored pubkey = %x, want %x", storedAcct.PubKey.SerializeCompressed(), acct.PubKey.SerializeCompressed())
		}
		if len(bonds) != 1 {
			t.Fatalf("stored %d bonds, want 1", len(bonds))
		}
		tAssertBond(t, bonds[0], bond)
	}

	// A new bond creates the account if necessary, stores the bond, and appends
	// the first bond_posted event log entry.
	firstEvent := []byte("bond-posted-event")
	firstTip := tBondPostedTip(t, nil, 1, firstEvent, acct, bond)
	firstRes := applyBondPosted(&db.EventLogMeta{Event: firstEvent})
	if !firstRes.BondAdded {
		t.Fatalf("BondAdded = false, want true")
	}
	requireLog(firstRes, 1, firstEvent, firstTip)
	requireStoredBond()
	requireRepListenerCall(t, *calls, 1, acct.ID)

	// Reapplying the same account/bond is idempotent for the bond table, but
	// the replicated event is still recorded in the event log.
	duplicateEvent := []byte("bond-posted-duplicate")
	duplicateTip := tBondPostedTip(t, firstRes.Log.TipHash, 2, duplicateEvent, acct, bond)
	duplicateRes := applyBondPosted(&db.EventLogMeta{
		Seq:             2,
		Event:           duplicateEvent,
		ExpectedTipHash: duplicateTip,
	})
	if duplicateRes.BondAdded {
		t.Fatalf("duplicate BondAdded = true, want false")
	}
	requireLog(duplicateRes, 2, duplicateEvent, duplicateTip)
	requireStoredBond()
	requireRepListenerCall(t, *calls, 2, acct.ID) // duplicate commit still notifies

	// A bond coin already associated with this account cannot be replayed for a
	// different account, and the rejected apply must not advance the log.
	otherAcct := tAccountFromPrivKey(t, 2)
	_, err := archie.ApplyBondPostedEvent(ctx, &db.EventLogMeta{
		Seq:   3,
		Event: []byte("bond-posted-conflict"),
	}, tBondPostedEvent(otherAcct, bond), 10, 10, 10)
	if err == nil {
		t.Fatalf("conflicting ApplyBondPostedEvent succeeded")
	}
	storedOtherAcct, otherBonds, err := archie.Account(ctx, otherAcct.ID, time.Unix(bond.LockTime-1, 0))
	if err != nil {
		t.Fatalf("Account error: %v", err)
	}
	if storedOtherAcct != nil || len(otherBonds) != 0 {
		t.Fatalf("conflicting apply stored account %v bonds %v", storedOtherAcct, otherBonds)
	}
	frontier, err := archie.EventLogFrontier(ctx)
	if err != nil {
		t.Fatalf("EventLogFrontier error: %v", err)
	}
	if frontier.Seq != 2 || !bytes.Equal(frontier.TipHash, duplicateRes.Log.TipHash) {
		t.Fatalf("frontier = (%d, %x), want (2, %x)", frontier.Seq, frontier.TipHash, duplicateRes.Log.TipHash)
	}
	requireRepListenerCall(t, *calls, 2, acct.ID) // rejected apply: no notify
}
