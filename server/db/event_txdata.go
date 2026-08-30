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

func int32Bytes(v int32) []byte {
	return encode.Uint32Bytes(uint32(v))
}

func boolBytes(v bool) []byte {
	if v {
		return encode.ByteTrue
	}
	return encode.ByteFalse
}

func optionalBoolTxData(v *bool) []byte {
	if v == nil {
		return encode.BuildyBytes{0}.AddData(encode.ByteFalse)
	}
	return encode.BuildyBytes{0}.AddData(encode.ByteTrue).AddData(boolBytes(*v))
}

func orderTypeBytes(t order.OrderType) []byte {
	return []byte{byte(t)}
}

func orderIDListTxData(ids []order.OrderID) []byte {
	b := encode.BuildyBytes{0}
	for _, id := range ids {
		b = b.AddData(id[:])
	}
	return b
}

// idListTxData encodes the order IDs of a list of orders of any concrete or
// interface order type. The name describes the element type in the error
// returned for a nil element.
func idListTxData[T interface {
	comparable
	order.Order
}](ords []T, name string) ([]byte, error) {
	var zero T
	b := encode.BuildyBytes{0}
	for _, ord := range ords {
		if ord == zero {
			return nil, fmt.Errorf("nil %s", name)
		}
		oid := ord.ID()
		b = b.AddData(oid[:])
	}
	return b, nil
}

func partialFillListTxData(ords []*order.LimitOrder) ([]byte, error) {
	b := encode.BuildyBytes{0}
	for _, ord := range ords {
		if ord == nil {
			return nil, fmt.Errorf("nil partial fill order")
		}
		oid := ord.ID()
		b = b.AddData(encode.BuildyBytes{0}.
			AddData(oid[:]).
			AddData(encode.Uint64Bytes(ord.Trade().Filled())))
	}
	return b, nil
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
	return b, nil
}

func matchListTxData(matches []*order.Match) ([]byte, error) {
	b := encode.BuildyBytes{0}
	for _, match := range matches {
		if match == nil {
			return nil, fmt.Errorf("nil match")
		}
		id := match.ID()
		b = b.AddData(id[:])
	}
	return b, nil
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

// EventTxData returns the versioned transaction data recorded in the event log
// for an order_accepted event.
func (u *OrderAcceptedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil order accepted update")
	}
	if u.Order == nil {
		return nil, fmt.Errorf("nil accepted order")
	}
	return encode.BuildyBytes{0}.
		AddData(u.Order.Serialize()).
		AddData(int64Bytes(u.EpochIdx)).
		AddData(int64Bytes(u.EpochDur)).
		AddData(int32Bytes(u.EpochGap)), nil
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

// EventTxData returns the versioned transaction data recorded in the event log
// for a market_lifecycle event.
func (u *MarketLifecycleUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil market lifecycle update")
	}
	resumeRevokes, err := startupOrderRevokesTxData(u.ResumeRevokes)
	if err != nil {
		return nil, err
	}
	return encode.BuildyBytes{0}.
		AddData(encode.Uint16Bytes(uint16(u.Action))).
		AddData([]byte(u.Market)).
		AddData(encode.Uint32Bytes(u.Base)).
		AddData(encode.Uint32Bytes(u.Quote)).
		AddData(int64Bytes(u.EpochIdx)).
		AddData(int64Bytes(u.EpochDur)).
		AddData(optionalBoolTxData(u.PersistBook)).
		AddData(int64Bytes(u.Timestamp.UnixMilli())).
		AddData(resumeRevokes), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for an advance_epoch event.
func (u *AdvanceEpochUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil advance epoch update")
	}
	return encode.BuildyBytes{0}.
		AddData([]byte(u.Market)).
		AddData(int64Bytes(u.ClosedEpochIdx)).
		AddData(int64Bytes(u.OpenedEpochIdx)).
		AddData(int64Bytes(u.EpochDur)).
		AddData(orderIDListTxData(u.ClosedOrderIDs)), nil
}

func preimageMissTxData(miss *PreimageMissUpdate) ([]byte, error) {
	if miss == nil {
		return nil, fmt.Errorf("nil preimage miss update")
	}
	if miss.Order == nil {
		return nil, fmt.Errorf("nil preimage miss order")
	}
	return encode.BuildyBytes{0}.
		AddData(miss.Order.Serialize()).
		AddData(int64Bytes(miss.RevokeTime.UnixMilli())), nil
}

func preimageRevealTxData(reveal *PreimageRevealUpdate) ([]byte, error) {
	if reveal == nil {
		return nil, fmt.Errorf("nil preimage reveal update")
	}
	if reveal.Order == nil {
		return nil, fmt.Errorf("nil preimage reveal order")
	}
	return encode.BuildyBytes{0}.
		AddData(reveal.Order.Serialize()).
		AddData(reveal.Preimage[:]), nil
}

func epochResultsTxData(epoch *EpochResults) []byte {
	if epoch == nil {
		return nil
	}
	return encode.BuildyBytes{0}.
		AddData(encode.Uint32Bytes(epoch.MktBase)).
		AddData(encode.Uint32Bytes(epoch.MktQuote)).
		AddData(int64Bytes(epoch.Idx)).
		AddData(int64Bytes(epoch.Dur)).
		AddData(int64Bytes(epoch.MatchTime)).
		AddData(epoch.CSum).
		AddData(epoch.Seed).
		AddData(orderIDListTxData(epoch.OrdersRevealed)).
		AddData(orderIDListTxData(epoch.OrdersMissed)).
		AddData(encode.Uint64Bytes(epoch.MatchVolume)).
		AddData(encode.Uint64Bytes(epoch.QuoteVolume)).
		AddData(encode.Uint64Bytes(epoch.BookBuys)).
		AddData(encode.Uint64Bytes(epoch.BookBuys5)).
		AddData(encode.Uint64Bytes(epoch.BookBuys25)).
		AddData(encode.Uint64Bytes(epoch.BookSells)).
		AddData(encode.Uint64Bytes(epoch.BookSells5)).
		AddData(encode.Uint64Bytes(epoch.BookSells25)).
		AddData(encode.Uint64Bytes(epoch.HighRate)).
		AddData(encode.Uint64Bytes(epoch.LowRate)).
		AddData(encode.Uint64Bytes(epoch.StartRate)).
		AddData(encode.Uint64Bytes(epoch.EndRate))
}

// EventTxData returns the versioned transaction data recorded in the event log
// for an epoch_processed event.
func (u *EpochProcessedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil epoch processed update")
	}
	if u.Epoch == nil {
		return nil, fmt.Errorf("nil epoch results")
	}
	misses := encode.BuildyBytes{0}
	for _, miss := range u.Misses {
		missBytes, err := preimageMissTxData(miss)
		if err != nil {
			return nil, err
		}
		misses = misses.AddData(missBytes)
	}
	reveals := encode.BuildyBytes{0}
	for _, reveal := range u.Reveals {
		revealBytes, err := preimageRevealTxData(reveal)
		if err != nil {
			return nil, err
		}
		reveals = reveals.AddData(revealBytes)
	}
	tradesBooked, err := idListTxData(u.TradesBooked, "limit order")
	if err != nil {
		return nil, err
	}
	tradesPartial, err := partialFillListTxData(u.TradesPartial)
	if err != nil {
		return nil, err
	}
	tradesCompleted, err := idListTxData(u.TradesCompleted, "order")
	if err != nil {
		return nil, err
	}
	tradesCanceled, err := idListTxData(u.TradesCanceled, "limit order")
	if err != nil {
		return nil, err
	}
	tradesFailed, err := idListTxData(u.TradesFailed, "order")
	if err != nil {
		return nil, err
	}
	cancelsFailed, err := idListTxData(u.CancelsFailed, "cancel order")
	if err != nil {
		return nil, err
	}
	cancelsExecuted, err := idListTxData(u.CancelsExecuted, "cancel order")
	if err != nil {
		return nil, err
	}
	matches, err := matchListTxData(u.Matches)
	if err != nil {
		return nil, err
	}
	return encode.BuildyBytes{0}.
		AddData(epochResultsTxData(u.Epoch)).
		AddData(misses).
		AddData(reveals).
		AddData(tradesBooked).
		AddData(tradesPartial).
		AddData(tradesCompleted).
		AddData(tradesCanceled).
		AddData(tradesFailed).
		AddData(cancelsFailed).
		AddData(cancelsExecuted).
		AddData(matches), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a suspended_cancel event.
func (u *SuspendedCancelUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil suspended cancel update")
	}
	if u.Cancel == nil {
		return nil, fmt.Errorf("nil suspended cancel order")
	}
	if u.Match == nil {
		return nil, fmt.Errorf("nil suspended cancel match")
	}
	if u.Match.Status != order.MatchComplete {
		return nil, fmt.Errorf("suspended cancel match status %d, want %d", u.Match.Status, order.MatchComplete)
	}
	mid := u.Match.ID()
	makerID := u.Match.Maker.ID()
	takerID := u.Match.Taker.ID()
	return encode.BuildyBytes{0}.
		AddData([]byte(u.Market)).
		AddData(encode.Uint32Bytes(u.Base)).
		AddData(encode.Uint32Bytes(u.Quote)).
		AddData(u.Cancel.Serialize()).
		AddData(u.TargetOrderID[:]).
		AddData(u.TargetAccount[:]).
		AddData(boolBytes(u.TargetSell)).
		AddData(int64Bytes(u.EpochIdx)).
		AddData(int64Bytes(u.EpochDur)).
		AddData(encode.Uint64Bytes(u.FeeRateBase)).
		AddData(encode.Uint64Bytes(u.FeeRateQuote)).
		AddData(int64Bytes(u.MatchServerTime.UnixMilli())).
		AddData(mid[:]).
		AddData(makerID[:]).
		AddData(takerID[:]).
		AddData(encode.Uint64Bytes(u.Match.Quantity)).
		AddData(encode.Uint64Bytes(u.Match.Rate)).
		AddData([]byte{byte(u.Match.Status)}), nil
}

// midTxData starts a version-0 tx-data encoding with the MatchID/Base/Quote
// triple shared by every per-match event encoder.
func midTxData(mid MarketMatchID) encode.BuildyBytes {
	return encode.BuildyBytes{0}.
		AddData(mid.MatchID[:]).
		AddData(encode.Uint32Bytes(mid.Base)).
		AddData(encode.Uint32Bytes(mid.Quote))
}

// ackTxData is the shared tx-data encoding of the audit_ack_recorded and
// redemption_ack_recorded events.
func ackTxData(mid MarketMatchID, maker bool, sig []byte) []byte {
	return midTxData(mid).
		AddData(boolBytes(maker)).
		AddData(sig)
}

func matchAckTxData(ack *MatchAck) []byte {
	if ack == nil {
		return nil
	}
	return midTxData(ack.MID).
		AddData(boolBytes(ack.Maker)).
		AddData(boolBytes(ack.Cancel)).
		AddData(ack.Sig).
		AddData([]byte(ack.Address))
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a match_acks_recorded event.
func (u *MatchAcksRecordedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil match acks recorded update")
	}
	b := encode.BuildyBytes{0}
	for _, ack := range u.Acks {
		b = b.AddData(matchAckTxData(ack))
	}
	return b, nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a swap_contract_recorded event.
func (c *SwapContract) EventTxData() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil swap contract")
	}
	return midTxData(c.MID).
		AddData(boolBytes(c.Maker)).
		AddData(c.Contract).
		AddData(c.CoinID).
		AddData(int64Bytes(c.Timestamp)), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for an audit_ack_recorded event.
func (a *AuditAck) EventTxData() ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("nil audit ack")
	}
	return ackTxData(a.MID, a.Maker, a.Sig), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a swap_redemption_recorded event.
func (r *SwapRedemption) EventTxData() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("nil swap redemption")
	}
	return midTxData(r.MID).
		AddData(boolBytes(r.Maker)).
		AddData(r.CoinID).
		AddData(r.Secret).
		AddData(int64Bytes(r.Timestamp)), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a redemption_ack_recorded event.
func (a *RedemptionAck) EventTxData() ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("nil redemption ack")
	}
	return ackTxData(a.MID, a.Maker, a.Sig), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for an orders_revoked event.
func (u *OrdersRevokedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil orders revoked update")
	}
	orders, err := idListTxData(u.Orders, "limit order")
	if err != nil {
		return nil, err
	}
	return encode.BuildyBytes{0}.
		AddData([]byte{byte(u.Reason)}).
		AddData(int64Bytes(u.RevokeTime.UnixMilli())).
		AddData(orders), nil
}

// EventTxData returns the versioned transaction data recorded in the event log
// for a match_failed event.
func (u *MatchFailedUpdate) EventTxData() ([]byte, error) {
	if u == nil {
		return nil, fmt.Errorf("nil match failed update")
	}
	return midTxData(u.MID).
		AddData(int64Bytes(u.FailTimeMS)).
		AddData([]byte{byte(u.Reason)}), nil
}
