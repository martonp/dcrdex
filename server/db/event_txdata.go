// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"fmt"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
)

func int64Bytes(v int64) []byte {
	return encode.Uint64Bytes(uint64(v))
}

func orderTypeBytes(t order.OrderType) []byte {
	return []byte{byte(t)}
}

func startupOrderRevokesTxData(ords []*StartupOrderRevoke) ([]byte, error) {
	b := encode.BuildyBytes{0}
	for _, revoke := range ords {
		if revoke == nil {
			return nil, fmt.Errorf("nil startup order revoke")
		}
		ord := revoke.Order
		if ord == nil {
			return nil, fmt.Errorf("nil startup order revoke value")
		}
		oid := ord.ID()
		user := ord.User()
		b = b.AddData(encode.BuildyBytes{0}.
			AddData(oid[:]).
			AddData(user[:]).
			AddData(encode.Uint32Bytes(ord.Base())).
			AddData(encode.Uint32Bytes(ord.Quote())).
			AddData(orderTypeBytes(ord.Type())).
			AddData(ord.Serialize()).
			AddData([]byte{byte(revoke.Reason)}))
	}
	// TODO: Support revoking more orders.
	if len(b) > encode.MaxDataLen {
		return nil, fmt.Errorf("startup revocation data size %d exceeds maximum %d",
			len(b), encode.MaxDataLen)
	}
	return b, nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a market_started event.
func (u *MarketStartedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil market started update")
	}
	bookedRevokes, err := startupOrderRevokesTxData(u.BookedRevokes)
	if err != nil {
		return nil, err
	}
	epochRevokes, err := startupOrderRevokesTxData(u.EpochRevokes)
	if err != nil {
		return nil, err
	}
	return encode.BuildyBytes{0}.
		AddData([]byte(u.Market)).
		AddData(encode.Uint32Bytes(u.Base)).
		AddData(encode.Uint32Bytes(u.Quote)).
		AddData(int64Bytes(u.CurrentEpochIdx)).
		AddData(int64Bytes(u.EpochDur)).
		AddData(int64Bytes(u.RevocationTime.UnixMilli())).
		AddData(bookedRevokes).
		AddData(epochRevokes), nil
}

