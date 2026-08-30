// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package meshevents

import (
	"bytes"
	"testing"

	"decred.org/dcrdex/server/account"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func testBondPostedAccount(t *testing.T) *account.Account {
	t.Helper()
	privBytes := make([]byte, 32)
	privBytes[31] = 0x01
	priv := secp256k1.PrivKeyFromBytes(privBytes)
	acct, err := account.NewAccountFromPubKey(priv.PubKey().SerializeCompressed())
	if err != nil {
		t.Fatalf("NewAccountFromPubKey error: %v", err)
	}
	return acct
}

func testPostedBond() *Bond {
	return &Bond{
		Version:  1,
		AssetID:  42,
		CoinID:   []byte{0xde, 0xad, 0xbe, 0xef},
		Amount:   123456789,
		Strength: 3,
		LockTime: 1720000000,
	}
}

// TestBondPostedEventWireJSON pins the wire payload to the JSON produced by
// the pre-catalog auth structs (db.Account/db.Bond), proving the conversion
// did not change the replicated bytes.
func TestBondPostedEventWireJSON(t *testing.T) {
	var acctID account.AccountID
	for i := range acctID {
		acctID[i] = byte(i + 1)
	}
	event := &BondPostedEvent{
		Account: &BondPostedAccount{
			AccountID: acctID,
			Pubkey:    []byte{0x02, 0xab, 0xcd, 0xef},
		},
		Bond: testPostedBond(),
	}
	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	// Golden bytes captured from the previous auth-local wire structs that
	// embedded db.Account and db.Bond.
	const golden = `{"account":{"accountid":"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20","pubkey":"02abcdef"},"bond":{"Version":1,"AssetID":42,"CoinID":"3q2+7w==","Amount":123456789,"Strength":3,"LockTime":1720000000}}`
	if string(payload) != golden {
		t.Fatalf("wire payload changed:\n got  %s\n want %s", payload, golden)
	}
}

func TestBondPostedEventEncodeDecode(t *testing.T) {
	acct := testBondPostedAccount(t)
	bond := testPostedBond()
	event := NewBondPostedEvent(acct, bond)
	payload, err := event.Encode()
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	got, err := DecodeBondPostedEvent(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if got.Account.AccountID != acct.ID {
		t.Fatalf("account id round trip mismatch: %v != %v", got.Account.AccountID, acct.ID)
	}
	if !bytes.Equal(got.Account.Pubkey, acct.PubKey.SerializeCompressed()) {
		t.Fatalf("pubkey round trip mismatch")
	}
	if !bytes.Equal(got.Bond.CoinID, bond.CoinID) || got.Bond.Version != bond.Version ||
		got.Bond.AssetID != bond.AssetID || got.Bond.Amount != bond.Amount ||
		got.Bond.Strength != bond.Strength || got.Bond.LockTime != bond.LockTime {
		t.Fatalf("bond round trip mismatch: %+v != %+v", got.Bond, bond)
	}
	parsed, err := got.PostedAccount()
	if err != nil {
		t.Fatalf("PostedAccount error: %v", err)
	}
	if parsed.ID != acct.ID {
		t.Fatalf("parsed account id mismatch: %v != %v", parsed.ID, acct.ID)
	}
}

func TestBondPostedEventValidate(t *testing.T) {
	acct := testBondPostedAccount(t)
	goodAcct := &BondPostedAccount{
		AccountID: acct.ID,
		Pubkey:    acct.PubKey.SerializeCompressed(),
	}
	var wrongID account.AccountID
	wrongID[0] = 0xff
	tests := []struct {
		name    string
		event   *BondPostedEvent
		wantErr bool
	}{
		{"ok", &BondPostedEvent{Account: goodAcct, Bond: testPostedBond()}, false},
		{"nil account", &BondPostedEvent{Bond: testPostedBond()}, true},
		{"nil bond", &BondPostedEvent{Account: goodAcct}, true},
		{"bad pubkey", &BondPostedEvent{Account: &BondPostedAccount{
			AccountID: acct.ID,
			Pubkey:    []byte{0x02, 0x01},
		}, Bond: testPostedBond()}, true},
		{"account id mismatch", &BondPostedEvent{Account: &BondPostedAccount{
			AccountID: wrongID,
			Pubkey:    acct.PubKey.SerializeCompressed(),
		}, Bond: testPostedBond()}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.event.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
