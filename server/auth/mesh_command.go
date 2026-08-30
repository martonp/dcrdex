// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

const (
	commandKindPostBond           = "postbond"
	commandKindCreatePrepaidBonds = "create_prepaid_bonds"
	commandKindForgiveReputation  = "forgive_reputation"
)

type reputationForgivenessRequest struct {
	AccountID account.AccountID                     `json:"accountID"`
	Scope     meshevents.ReputationForgivenessScope `json:"scope"`
	MatchID   order.MatchID                         `json:"matchID"`
}

type reputationForgivenessResult struct {
	Forgiven bool `json:"forgiven"`
	Unbanned bool `json:"unbanned"`
}

// Commands returns the mesh command handlers. Client routes submit them
// with ExecuteCommand; they do not write the DB themselves. The master
// runs them; a slave forwards.
func (auth *AuthManager) Commands() map[string]mesh.CommandExecutor {
	return map[string]mesh.CommandExecutor{
		commandKindPostBond:           auth.executePostBond,
		commandKindCreatePrepaidBonds: auth.executeCreatePrepaidBonds,
		commandKindForgiveReputation:  auth.executeForgiveReputation,
	}
}

func (auth *AuthManager) executeForgiveReputation(cmdCtx *mesh.CommandContext) *msgjson.Error {
	var req reputationForgivenessRequest
	if err := cmdCtx.Request.Msg.Unmarshal(&req); err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing reputation forgiveness request")
	}
	if req.AccountID != cmdCtx.Request.User {
		return msgjson.NewError(msgjson.RPCInternalError, "reputation forgiveness command account mismatch")
	}

	event, rpcErr := auth.reputationForgivenessEvent(&req)
	if rpcErr != nil {
		return rpcErr
	}
	meshEvent, err := mesh.NewEvent(event)
	if err != nil {
		return msgjson.NewError(msgjson.RPCInternalError, "failed to encode reputation forgiveness event")
	}

	if err = cmdCtx.Completion.Emit(cmdCtx.Context, meshEvent, nil); err != nil {
		mesh.LogApplyFailure(log, err, "Failed to apply reputation forgiveness event for account %v: %v", req.AccountID, err)
		return mesh.ClientError(err, msgjson.RPCInternalError, "failed to apply reputation forgiveness: %v", err)
	}
	return nil
}

func (auth *AuthManager) reputationForgivenessEvent(req *reputationForgivenessRequest) (*meshevents.ReputationForgivenEvent, *msgjson.Error) {
	if req == nil {
		return nil, msgjson.NewError(msgjson.RPCParseError, "nil reputation forgiveness request")
	}
	var zeroAccount account.AccountID
	if req.AccountID == zeroAccount {
		return nil, msgjson.NewError(msgjson.RPCParseError, "reputation forgiveness request missing account")
	}
	var zeroMID order.MatchID
	switch req.Scope {
	case meshevents.ReputationForgivenessScopeUser:
		if req.MatchID != zeroMID {
			return nil, msgjson.NewError(msgjson.RPCParseError, "user-scope reputation forgiveness request specifies match")
		}
		return &meshevents.ReputationForgivenEvent{
			AccountID: req.AccountID,
			Scope:     req.Scope,
		}, nil
	case meshevents.ReputationForgivenessScopeMatch:
		if req.MatchID == zeroMID {
			return nil, msgjson.NewError(msgjson.RPCParseError, "match-scope reputation forgiveness request missing match")
		}
		matchID := req.MatchID
		return &meshevents.ReputationForgivenEvent{
			AccountID: req.AccountID,
			Scope:     req.Scope,
			MatchID:   &matchID,
		}, nil
	default:
		return nil, msgjson.NewError(msgjson.RPCParseError, "invalid reputation forgiveness scope %d", req.Scope)
	}
}
