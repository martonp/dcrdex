// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"fmt"

	"decred.org/dcrdex/dex/encode"
)

func int64Bytes(v int64) []byte {
	return encode.Uint64Bytes(uint64(v))
}

func bondTxData(bond *Bond) []byte {
	if bond == nil {
		return nil
	}
	return encode.BuildyBytes{0}.
		AddData(encode.Uint16Bytes(bond.Version)).
		AddData(encode.Uint32Bytes(bond.AssetID)).
		AddData(bond.CoinID).
		AddData(int64Bytes(bond.Amount)).
		AddData(encode.Uint32Bytes(bond.Strength)).
		AddData(int64Bytes(bond.LockTime))
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a bond_posted event.
func (u *BondPostedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil bond posted update")
	}
	if u.Acct == nil {
		return nil, fmt.Errorf("nil bond posted account")
	}
	if u.Acct.PubKey == nil {
		return nil, fmt.Errorf("nil bond posted pubkey")
	}
	if u.Bond == nil {
		return nil, fmt.Errorf("nil posted bond")
	}
	return encode.BuildyBytes{0}.
		AddData(u.Acct.ID[:]).
		AddData(u.Acct.PubKey.SerializeCompressed()).
		AddData(bondTxData(u.Bond)), nil
}

