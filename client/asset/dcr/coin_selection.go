// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dcr

import (
	"strconv"
	"strings"

	"decred.org/dcrdex/dex/calc"
	dexdcr "decred.org/dcrdex/dex/networks/dcr"
)

// sendEnough generates a function that can be used as the enough argument to
// the fund method when creating transactions to send funds. If fees are to be
// subtracted from the inputs, set subtract so that the required amount excludes
// the transaction fee. If change from the transaction should be considered
// immediately available (not mixing), set reportChange to indicate this and the
// returned enough func will return a non-zero excess value. Otherwise, the
// enough func will always return 0, leaving only unselected UTXOs to cover any
// required reserves.
func sendEnough(amt, feeRate uint64, subtract bool, baseTxSize uint32, reportChange bool) func(sum uint64, inputSize uint32, unspent *compositeUTXO) (bool, uint64) {
	return func(sum uint64, inputSize uint32, unspent *compositeUTXO) (bool, uint64) {
		total := sum + toAtoms(unspent.rpc.Amount)
		txFee := uint64(baseTxSize+inputSize+unspent.input.Size()) * feeRate
		req := amt
		if !subtract { // add the fee to required
			req += txFee
		}
		if total < req {
			return false, 0
		}
		excess := total - req
		if subtract && excess > txFee {
			excess -= txFee // total - (req + txFee), without underflow
		}
		if !reportChange || dexdcr.IsDustVal(dexdcr.P2PKHOutputSize, excess, feeRate) {
			excess = 0
		}
		return true, excess
	}
}

// orderEnough generates a function that can be used as the enough argument to
// the fund method. If change from a split transaction will be created AND
// immediately available (not mixing), set reportChange to indicate this and the
// returned enough func will return a non-zero excess value reflecting this
// potential spit tx change. Otherwise, the enough func will always return 0,
// leaving only unselected UTXOs to cover any required reserves.
func orderEnough(val, lots, feeRate uint64, reportChange bool) func(sum uint64, size uint32, unspent *compositeUTXO) (bool, uint64) {
	return func(sum uint64, size uint32, unspent *compositeUTXO) (bool, uint64) {
		reqFunds := calc.RequiredOrderFundsAlt(val, uint64(size+unspent.input.Size()), lots,
			dexdcr.InitTxSizeBase, dexdcr.InitTxSize, feeRate)
		total := sum + toAtoms(unspent.rpc.Amount) // all selected utxos

		if total >= reqFunds { // that'll do it
			// change = total - (val + swapTxnsFee)
			excess := total - reqFunds // reqFunds = val + swapTxnsFee
			if !reportChange || dexdcr.IsDustVal(dexdcr.P2PKHOutputSize, excess, feeRate) {
				excess = 0
			}
			return true, excess
		}
		return false, 0
	}
}

func sumUTXOs(set []*compositeUTXO) (tot uint64) {
	for _, utxo := range set {
		tot += toAtoms(utxo.rpc.Amount)
	}
	return tot
}

type solution struct {
	amount  uint64
	indexes string
}

func existingSolution(amt uint64, curr int, solutions map[int]map[uint64]*solution) *solution {
	if _, ok := solutions[curr]; !ok {
		return nil
	}
	return solutions[curr][amt]
}

func storeSolution(amt uint64, curr int, amount uint64, indexes string, solutions map[int]map[uint64]*solution) {
	if _, ok := solutions[curr]; !ok {
		solutions[curr] = make(map[uint64]*solution)
	}
	solutions[curr][amt] = &solution{amount, indexes}
}

func leastOverFundHelper(amt uint64, curr int, utxos []*compositeUTXO, solutions map[int]map[uint64]*solution) (total uint64, indexString string) {
	if curr >= len(utxos) {
		return 1 << 62, ""
	}

	if existing := existingSolution(amt, curr, solutions); existing != nil {
		return existing.amount, existing.indexes
	}

	currAtoms := toAtoms(utxos[curr].rpc.Amount)
	if currAtoms >= amt {
		storeSolution(amt, curr, currAtoms, strconv.Itoa(curr), solutions)
		return currAtoms, strconv.Itoa(curr)
	}

	amountWithCurr, outputsWithCurr := leastOverFundHelper(amt-currAtoms, curr+1, utxos, solutions)
	amountWithoutCurr, outputsWithoutCurr := leastOverFundHelper(amt, curr+1, utxos, solutions)

	if amountWithCurr+currAtoms < amountWithoutCurr {
		indexes := strconv.Itoa(curr) + "," + outputsWithCurr
		storeSolution(amt, curr, amountWithCurr+currAtoms, strconv.Itoa(curr)+","+outputsWithCurr, solutions)
		return amountWithCurr + currAtoms, indexes
	}

	storeSolution(amt, curr, amountWithoutCurr, outputsWithoutCurr, solutions)
	return amountWithoutCurr, outputsWithoutCurr
}

func leastOverFund(amt uint64, utxos []*compositeUTXO) []*compositeUTXO {
	if amt == 0 || sumUTXOs(utxos) < amt {
		return nil
	}

	solutions := make(map[int]map[uint64]*solution)
	_, outputs := leastOverFundHelper(amt, 0, utxos, solutions)
	outputsArray := strings.Split(outputs, ",")
	returnOutputs := make([]*compositeUTXO, len(outputsArray))

	for i, output := range outputsArray {
		index, _ := strconv.Atoi(output)
		returnOutputs[i] = utxos[index]
	}

	return returnOutputs
}

// utxoSetDiff performs the setdiff(set,sub) of two UTXO sets. That is, any
// UTXOs that are both sets are removed from the first. The comparison is done
// *by pointer*, with no regard to the values of the compositeUTXO elements.
func utxoSetDiff(set, sub []*compositeUTXO) []*compositeUTXO {
	var availUTXOs []*compositeUTXO
avail:
	for _, utxo := range set {
		for _, kept := range sub {
			if utxo == kept { // by pointer
				continue avail
			}
		}
		availUTXOs = append(availUTXOs, utxo)
	}
	return availUTXOs
}
