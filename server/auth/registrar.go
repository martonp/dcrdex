// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"bytes"
	"context"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

var (
	// The coin waiters will query for transaction data every recheckInterval.
	recheckInterval = time.Second * 5
	// txWaitExpiration is the longest the AuthManager will wait for a coin
	// waiter. This could be thought of as the maximum allowable backend latency.
	// TODO: Reconcile this with the client postbond response timeout and the
	// mesh pending-command timeout so delayed postbond responses cannot expire at
	// one layer while another layer is still waiting.
	txWaitExpiration = 2 * time.Minute
)

// bondKey creates a unique map key for a bond by its asset ID and coin ID.
func bondKey(assetID uint32, coinID []byte) string {
	return string(append(encode.Uint32Bytes(assetID), coinID...))
}

func (auth *AuthManager) registerBondWaiter(key string) bool {
	auth.bondWaiterMtx.Lock()
	defer auth.bondWaiterMtx.Unlock()
	if _, found := auth.bondWaiterIdx[key]; found {
		return false
	}
	auth.bondWaiterIdx[key] = struct{}{}
	return true
}

func (auth *AuthManager) removeBondWaiter(key string) {
	auth.bondWaiterMtx.Lock()
	delete(auth.bondWaiterIdx, key)
	auth.bondWaiterMtx.Unlock()
}

// handlePreValidateBond handles the 'prevalidatebond' request.
//
// The request payload includes the user's account public key and the serialized
// bond post transaction itself (not just the txid).
//
// The parseBondTx function is used to validate the transaction, and extract
// bond details (amount and lock time) and the account ID to which it commits.
// This also checks that the account commitment corresponds to the user's public
// key provided in the payload. If these requirements are satisfied, the client
// will receive a PreValidateBondResult in the response. The user should then
// proceed to broadcast the bond and use the 'postbond' route once it reaches
// the required number of confirmations.
func (auth *AuthManager) handlePreValidateBond(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
	preBond := new(msgjson.PreValidateBond)
	err := msg.Unmarshal(&preBond)
	if err != nil || preBond == nil {
		return msgjson.NewError(msgjson.BondError, "error parsing prevalidatebond request")
	}

	assetID := preBond.AssetID
	bondAsset, ok := auth.bondAssets[assetID]
	if !ok {
		return msgjson.NewError(msgjson.BondError, "%s does not support bonds", dex.BipIDSymbol(assetID))
	}

	// Create an account.Account from the provided pubkey.
	acct, err := account.NewAccountFromPubKey(preBond.AcctPubKey)
	if err != nil {
		return msgjson.NewError(msgjson.BondError, "error parsing account pubkey: %v", err)
	}
	acctID := acct.ID

	// Authenticate the message for the supposed account.
	sigMsg := preBond.Serialize()
	err = checkSigS256(sigMsg, preBond.SigBytes(), acct.PubKey)
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.SignatureError,
			Message: "signature error: " + err.Error(),
		}
	}

	// A bond's lockTime must be after bondExpiry from now.
	lockTimeThresh := time.Now().Add(auth.bondExpiry)

	// Decode raw tx, check fee output (0) and account commitment output (1).
	bondCoinID, amt, lockTime, commitAcct, err :=
		auth.parseBondTx(assetID, preBond.Version, preBond.RawTx /*, postBond.Data*/)
	if err != nil {
		return msgjson.NewError(msgjson.BondError, "invalid bond transaction: %v", err)
	}
	if amt < int64(bondAsset.Amt) {
		return msgjson.NewError(msgjson.BondError, "insufficient bond amount %d, needed %d", amt, bondAsset.Amt)
	}
	if lockTime < lockTimeThresh.Unix() {
		return msgjson.NewError(msgjson.BondError, "insufficient lock time %d, needed at least %d", lockTime, lockTimeThresh.Unix())
	}

	// Must be equal to account ID computed from pubkey in the PayFee message.
	if commitAcct != acctID {
		return msgjson.NewError(msgjson.BondError, "invalid bond transaction - account commitment does not match pubkey")
	}

	bondStr := coinIDString(assetID, bondCoinID)
	bondAssetSym := dex.BipIDSymbol(assetID)
	log.Debugf("Validated prospective bond txn output %s (%s) paying %d for user %v",
		bondStr, bondAssetSym, amt, acctID)

	expireTime := time.Unix(lockTime, 0).Add(-auth.bondExpiry)
	preBondRes := &msgjson.PreValidateBondResult{
		AccountID: acctID[:],
		AssetID:   assetID,
		Amount:    uint64(amt),
		Expiry:    uint64(expireTime.Unix()),
	}
	preBondRes.SetSig(auth.SignMsg(append(preBondRes.Serialize(), preBond.RawTx...)))

	resp, err := msgjson.NewResponse(msg.ID, preBondRes, nil)
	if err != nil { // shouldn't be possible
		return msgjson.NewError(msgjson.RPCInternalError, "internal encoding error")
	}
	err = conn.Send(resp)
	if err != nil {
		log.Warnf("Error sending prevalidatebond result to user %v: %v", acctID, err)
		if err = auth.Send(acctID, resp); err != nil {
			log.Warnf("Error sending prevalidatebond result to account %v: %v", acctID, err)
		}
	}
	return nil
}

// handlePostBond handles the 'postbond' request.
//
// The checkBond function is used to locate the bond transaction on the network,
// and verify the amount, lockTime, and account to which it commits.
//
// A 'postbond' request should not be made until the bond transaction has been
// broadcasted and reaches the required number of confirmations.
func (auth *AuthManager) handlePostBond(conn comms.Link, msg *msgjson.Message) *msgjson.Error {
	_, acct, rpcErr := parsePostBond(msg)
	if rpcErr != nil {
		return rpcErr
	}

	req := mesh.CommandRequest{
		Kind: commandKindPostBond,
		User: acct.ID,
		Msg:  msg,
		Respond: func(resp *msgjson.Message) error {
			if err := conn.Send(resp); err == nil {
				return nil
			}
			return auth.Send(acct.ID, resp)
		},
	}

	return auth.mesh.ExecuteCommand(context.Background(), req)
}

func parsePostBond(msg *msgjson.Message) (*msgjson.PostBond, *account.Account, *msgjson.Error) {
	postBond := new(msgjson.PostBond)
	err := msg.Unmarshal(&postBond)
	if err != nil || postBond == nil {
		return nil, nil, msgjson.NewError(msgjson.BondError, "error parsing postbond request")
	}

	// Create an account.Account from the provided pubkey.
	acct, err := account.NewAccountFromPubKey(postBond.AcctPubKey)
	if err != nil {
		return nil, nil, msgjson.NewError(msgjson.BondError, "error parsing account pubkey: %v", err)
	}
	return postBond, acct, nil
}

func (auth *AuthManager) executePostBond(cmdCtx *mesh.CommandContext) *msgjson.Error {
	req := cmdCtx.Request
	postBond, acct, rpcErr := parsePostBond(req.Msg)
	if rpcErr != nil {
		return rpcErr
	}
	if acct.ID != req.User {
		return msgjson.NewError(msgjson.AuthenticationError, "account mismatch")
	}

	acctID := acct.ID
	assetID := postBond.AssetID

	// Authenticate the message for the supposed account.
	sigMsg := postBond.Serialize()
	err := checkSigS256(sigMsg, postBond.SigBytes(), acct.PubKey)
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.SignatureError,
			Message: "signature error: " + err.Error(),
		}
	}

	if assetID == account.PrepaidBondID {
		return auth.executePrepaidPostBond(cmdCtx, acct, postBond)
	}

	bondAsset, ok := auth.bondAssets[assetID]
	if !ok {
		return msgjson.NewError(msgjson.BondError, "%s does not support bonds", dex.BipIDSymbol(assetID))
	}

	// A bond's lockTime must be after bondExpiry from now.
	lockTimeThresh := time.Now().Add(auth.bondExpiry)

	bondVer, bondCoinID := postBond.Version, postBond.CoinID
	checkCtx, cancel := context.WithTimeout(cmdCtx.Context, 20*time.Second)
	defer cancel()
	amt, lockTime, confs, commitAcct, err := auth.checkBond(checkCtx, assetID, bondVer, bondCoinID)
	if err != nil {
		return msgjson.NewError(msgjson.BondError, "invalid bond transaction: %v", err)
	}
	if amt < int64(bondAsset.Amt) {
		return msgjson.NewError(msgjson.BondError, "insufficient bond amount %d, needed %d", amt, bondAsset.Amt)
	}
	if lockTime < lockTimeThresh.Unix() {
		return msgjson.NewError(msgjson.BondError, "insufficient lock time %d, needed at least %d", lockTime, lockTimeThresh.Unix())
	}

	// Must be equal to account ID computed from pubkey in the PayFee message.
	if commitAcct != acctID {
		return msgjson.NewError(msgjson.BondError, "invalid bond transaction - account commitment does not match pubkey")
	}

	strength := uint32(uint64(amt) / bondAsset.Amt)

	// All good. The client gets a PostBondResult (no error) unless the confirms
	// check has an unexpected error or times out.
	expireTime := time.Unix(lockTime, 0).Add(-auth.bondExpiry)
	postBondRes := &msgjson.PostBondResult{
		AccountID:  acctID[:],
		AssetID:    assetID,
		Amount:     uint64(amt),
		Expiry:     uint64(expireTime.Unix()),
		Strength:   strength,
		BondID:     bondCoinID,
		Reputation: auth.ComputeUserReputation(acctID),
	}

	// See if the account exists, and get known unexpired bonds.
	dbAcct, bonds, err := auth.storage.Account(acctID, lockTimeThresh)
	if err != nil {
		log.Errorf("Account read failed for user %v in postbond: %v", acctID, err)
		return msgjson.NewError(msgjson.RPCInternalError, "failed to retrieve account")
	}

	bondStr := coinIDString(assetID, bondCoinID)
	bondAssetSym := dex.BipIDSymbol(assetID)

	// See if we already have this bond in DB.
	for _, bond := range bonds {
		if bond.AssetID == assetID && bytes.Equal(bond.CoinID, bondCoinID) {
			log.Debugf("Found existing bond %s (%s) committing %d for user %v",
				bondStr, bondAssetSym, amt, acctID)

			if len(postBondRes.SigBytes()) == 0 && auth.signer == nil {
				return msgjson.NewError(msgjson.RPCInternalError, "failed to sign bond result")
			}
			auth.finalizePostBondResult(acct.ID, postBondRes)
			if err := cmdCtx.Completion.Complete(context.Background(), postBondRes); err != nil {
				return msgjson.NewError(msgjson.RPCInternalError, "failed to complete bond result")
			}

			return nil
		}
	}

	dbBond := &db.Bond{
		Version:  postBond.Version,
		AssetID:  assetID,
		CoinID:   bondCoinID,
		Amount:   amt,
		Strength: strength,
		LockTime: lockTime,
	}

	// Either store the bond or start a block waiter to activate the bond and
	// respond with a PostBondResult when it is fully-confirmed.
	bondIDKey := bondKey(assetID, bondCoinID)
	if !auth.registerBondWaiter(bondIDKey) {
		// Waiter already running! They'll get a response to their first
		// request, or find out on connect if the bond was activated.
		return msgjson.NewError(msgjson.BondAlreadyConfirmingError, "bond already submitted")
	}

	newAcct := dbAcct == nil
	reqConfs := int64(bondAsset.Confs)

	if confs >= reqConfs {
		// No need to call checkFee again in a waiter.
		log.Debugf("Activating new bond %s (%s) committing %d for user %v", bondStr, bondAssetSym, amt, acctID)
		rpcErr := auth.storeBondAndRespond(cmdCtx.Completion, dbBond, acct, newAcct, postBondRes)
		auth.removeBondWaiter(bondIDKey) // after storing it
		return rpcErr
	}

	// The user should have submitted only when the bond was confirmed, so we
	// only expect to wait for asset network latency.
	log.Debugf("Found new bond %s (%s) committing %d for user %v. Confirming...",
		bondStr, bondAssetSym, amt, acctID)
	ctxTry, cancelTry := context.WithTimeout(context.Background(), txWaitExpiration) // prevent checkBond RPC hangs
	auth.latencyQ.Wait(&wait.Waiter{
		Expiration: time.Now().Add(txWaitExpiration),
		TryFunc: func() wait.TryDirective {
			res := auth.waitBondConfs(ctxTry, cmdCtx.Completion, dbBond, acct, reqConfs, newAcct, postBondRes)
			if res == wait.DontTryAgain {
				auth.removeBondWaiter(bondIDKey)
				cancelTry()
			}
			return res
		},
		ExpireFunc: func() {
			auth.removeBondWaiter(bondIDKey)
			cancelTry()
			// User may retry postbond periodically or on reconnect.
		},
	})
	// NOTE: server restart cannot restart these waiters, so user must resubmit
	// their postbond after their request times out.

	return nil
}

func (auth *AuthManager) storeBondAndRespond(completion *mesh.CommandCompletion, bond *db.Bond, acct *account.Account,
	newAcct bool, postBondRes *msgjson.PostBondResult) *msgjson.Error {
	acctID := acct.ID
	assetID, coinID := bond.AssetID, bond.CoinID
	bondStr := coinIDString(assetID, coinID)
	bondAssetSym := dex.BipIDSymbol(assetID)

	if newAcct {
		log.Infof("Creating new user account %v, posted first bond in %v (%s)",
			acctID, bondStr, bondAssetSym)
	} else {
		log.Infof("Adding bond for existing user account %v, with bond in %v (%s)",
			acctID, bondStr, bondAssetSym)
	}

	err := auth.submitBondPostedEvent(context.Background(), completion, acct, bond, postBondRes)
	if err != nil {
		mesh.LogApplyFailure(log, err, "Failure while storing bond for acct %v (new = %v): %v", acct, newAcct, err)
		return mesh.ClientError(err, msgjson.RPCInternalError, "failed to store bond")
	}
	rep := postBondRes.Reputation

	// Integrate active bonds and score to report tier.
	log.Infof("Bond accepted: acct %v locked %d in %v. Bond total %d, tier %d",
		acctID, bond.Amount, coinIDString(bond.AssetID, coinID), rep.BondedTier, rep.EffectiveTier())

	return nil
}

// applyBondPostedEvent validates the event, applies the durable account/bond
// mutation in one storage transaction, then updates only local memory.
func (auth *AuthManager) applyBondPostedEvent(ctx context.Context, logMeta *db.EventLogMeta, event *meshevents.BondPostedEvent) (*db.EventLogEntry, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	acct, err := event.PostedAccount()
	if err != nil {
		return nil, err
	}

	result, err := auth.storage.ApplyBondPostedEvent(ctx, logMeta, &db.BondPostedUpdate{
		Acct: acct,
		Bond: dbBond(event.Bond),
	})
	if err != nil {
		return nil, err
	}
	return result.Log, nil
}

func (auth *AuthManager) executePrepaidPostBond(cmdCtx *mesh.CommandContext, acct *account.Account, postBond *msgjson.PostBond) *msgjson.Error {
	if postBond.Version != 0 {
		return msgjson.NewError(msgjson.BondError, "unsupported pre-paid bond version %d", postBond.Version)
	}
	const prepaidBondIDLength = 16
	coinID := postBond.CoinID
	if len(coinID) != prepaidBondIDLength {
		return msgjson.NewError(msgjson.BondError, "invalid pre-paid bond id length %d", len(coinID))
	}

	lockTimeThresh := time.Now().Add(auth.bondExpiry)
	dbAcct, bonds, err := auth.storage.Account(acct.ID, lockTimeThresh)
	if err != nil {
		log.Errorf("Account read failed for user %v in prepaid postbond: %v", acct.ID, err)
		return msgjson.NewError(msgjson.RPCInternalError, "failed to retrieve account")
	}

	for _, bond := range bonds {
		if bond.AssetID == account.PrepaidBondID && bytes.Equal(bond.CoinID, coinID) {
			expireTime := time.Unix(bond.LockTime, 0).Add(-auth.bondExpiry)
			postBondRes := &msgjson.PostBondResult{
				AccountID:  acct.ID[:],
				AssetID:    account.PrepaidBondID,
				Amount:     uint64(bond.Amount),
				Expiry:     uint64(expireTime.Unix()),
				Strength:   bond.Strength,
				BondID:     coinID,
				Reputation: auth.ComputeUserReputation(acct.ID),
			}
			if len(postBondRes.SigBytes()) == 0 && auth.signer == nil {
				return msgjson.NewError(msgjson.RPCInternalError, "failed to sign bond result")
			}
			auth.finalizePostBondResult(acct.ID, postBondRes)
			if err := cmdCtx.Completion.Complete(context.Background(), postBondRes); err != nil {
				return msgjson.NewError(msgjson.RPCInternalError, "failed to complete bond result")
			}
			return nil
		}
	}

	bondIDKey := bondKey(account.PrepaidBondID, coinID)
	if !auth.registerBondWaiter(bondIDKey) {
		return msgjson.NewError(msgjson.BondAlreadyConfirmingError, "bond already submitted")
	}
	defer auth.removeBondWaiter(bondIDKey)

	strength, lockTimeI, err := auth.storage.FetchPrepaidBond(coinID)
	if err != nil {
		return msgjson.NewError(msgjson.BondError, "unknown or already spent pre-paid bond: %v", err)
	}

	lockTime := time.Unix(lockTimeI, 0)
	expireTime := lockTime.Add(-auth.bondExpiry)
	if time.Until(expireTime) < time.Hour*24 {
		return msgjson.NewError(msgjson.BondError, "pre-paid bond is too old")
	}

	postBondRes := &msgjson.PostBondResult{
		AccountID:  acct.ID[:],
		AssetID:    account.PrepaidBondID,
		Amount:     0,
		Strength:   strength,
		Expiry:     uint64(expireTime.Unix()),
		BondID:     coinID,
		Reputation: auth.ComputeUserReputation(acct.ID),
	}

	dbBond := &db.Bond{
		Version:  postBond.Version,
		AssetID:  account.PrepaidBondID,
		CoinID:   coinID,
		Amount:   0,
		Strength: strength,
		LockTime: lockTimeI,
	}

	newAcct := dbAcct == nil
	return auth.storeBondAndRespond(cmdCtx.Completion, dbBond, acct, newAcct, postBondRes)
}

// waitBondConfs waits for a validated bond transaction to reach reqConfs, then
// stores/publishes the bond event. Direct requests receive a PostBondResult from
// this node; forwarded requests get their response after the slave applies the
// replicated event.
func (auth *AuthManager) waitBondConfs(ctx context.Context, completion *mesh.CommandCompletion, bond *db.Bond, acct *account.Account,
	reqConfs int64, newAcct bool, postBondRes *msgjson.PostBondResult) wait.TryDirective {
	assetID, coinID := bond.AssetID, bond.CoinID
	amt, _, confs, _, err := auth.checkBond(ctx, assetID, bond.Version, coinID)
	if err != nil {
		// This is unexpected because we already validated everything, so
		// hopefully this is a transient failure such as RPC connectivity.
		log.Warnf("Unexpected error checking bond coin: %v", err)
		return wait.TryAgain
	}
	if confs < reqConfs {
		return wait.TryAgain
	}
	acctID := acct.ID

	// Verify the bond amount as a spot check. This should be redundant with the
	// parseBondTx checks. If it disagrees, there is a bug in the fee asset
	// backend, and the operator will need to intervene.
	if amt != bond.Amount {
		log.Errorf("checkFee: account %v fee coin %x pays %d; expected %d",
			acctID, coinID, amt, bond.Amount)
		return wait.DontTryAgain
	}

	// Store and respond
	log.Debugf("Activating new bond %s (%s) committing %d for user %v",
		coinIDString(assetID, coinID), dex.BipIDSymbol(assetID), amt, acctID)
	rpcErr := auth.storeBondAndRespond(completion, bond, acct, newAcct, postBondRes)
	if rpcErr != nil {
		if err := completion.Fail(ctx, rpcErr); err != nil {
			log.Errorf("Failed to send postbond error response for user %v: %v", acctID, err)
		}
	}

	return wait.DontTryAgain
}
