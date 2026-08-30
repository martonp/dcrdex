// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"errors"
	"testing"

	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
)

func repTestOrderID(b byte) (oid order.OrderID) {
	oid[0] = b
	return
}

func TestOutcomeBatchUsers(t *testing.T) {
	if users := outcomeBatchUsers(nil); len(users) != 0 {
		t.Fatalf("nil batch users = %v, want none", users)
	}
	if users := outcomeBatchUsers(new(reputationOutcomeBatch)); len(users) != 0 {
		t.Fatalf("empty batch users = %v, want none", users)
	}

	userA, userB := randomAccountID(), randomAccountID()
	// userA in every class, userB once — each reported once.
	batch := &reputationOutcomeBatch{
		preimages: []*reputationPreimageOutcome{
			{user: userA, oid: repTestOrderID(1), miss: true},
			{user: userA, oid: repTestOrderID(2)},
		},
		matches: []*reputationMatchOutcome{
			{user: userA, mid: db.MarketMatchID{}, outcome: db.OutcomeSwapSuccess},
			{user: userB, mid: db.MarketMatchID{}, outcome: db.OutcomeNoSwapAsTaker},
		},
		orders: []*reputationOrderOutcome{
			{user: userA, oid: repTestOrderID(3), penalizedCancel: true},
		},
	}
	users := outcomeBatchUsers(batch)
	if len(users) != 2 {
		t.Fatalf("batch users = %v, want exactly {%v, %v}", users, userA, userB)
	}
	seen := map[account.AccountID]bool{userA: false, userB: false}
	for _, user := range users {
		counted, want := seen[user]
		if !want {
			t.Fatalf("unexpected batch user %v", user)
		}
		if counted {
			t.Fatalf("duplicate batch user %v", user)
		}
		seen[user] = true
	}
}

func TestNotifyRepInputsOnCommit(t *testing.T) {
	archiver := new(Archiver)
	user := randomAccountID()

	archiver.notifyRepInputsOnCommit(nil, user) // nil listener: no-op

	var got [][]account.AccountID
	archiver.SetReputationInputsListener(func(users ...account.AccountID) {
		got = append(got, users)
	})

	archiver.notifyRepInputsOnCommit(nil, user)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != user {
		t.Fatalf("committed notify = %v, want [[%v]]", got, user)
	}

	// Commit-unknown still notifies (safe over-invalidation).
	archiver.notifyRepInputsOnCommit(&db.EventCommitUnknownError{Err: errors.New("conn lost")}, user)
	if len(got) != 2 {
		t.Fatalf("commit-unknown notify count = %d, want 2", len(got))
	}

	archiver.notifyRepInputsOnCommit(errors.New("apply failed"), user) // definite failure
	archiver.notifyRepInputsOnCommit(nil)                              // no users
	if len(got) != 2 {
		t.Fatalf("notify count = %d after error/empty calls, want 2", len(got))
	}
}

func TestSetReputationInputsListenerOnce(t *testing.T) {
	archiver := new(Archiver)
	archiver.SetReputationInputsListener(func(...account.AccountID) {})
	defer func() {
		if recover() == nil {
			t.Fatal("second SetReputationInputsListener did not panic")
		}
	}()
	archiver.SetReputationInputsListener(func(...account.AccountID) {})
}
