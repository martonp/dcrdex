// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"context"
	"fmt"

	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

// MeshService is the part of mesh.Service this package calls:
// ExecuteCommand for client requests, ApplyEvent for replicated state.
type MeshService interface {
	ExecuteCommand(context.Context, mesh.CommandRequest) *msgjson.Error
	ApplyEvent(context.Context, *mesh.Event) (any, error)
	ProxyClientMessage(context.Context, *mesh.ClientProxyMessage) error
}

// wireBond converts a stored bond to its meshevents wire form.
func wireBond(bond *db.Bond) *meshevents.Bond {
	return &meshevents.Bond{
		Version:  bond.Version,
		AssetID:  bond.AssetID,
		CoinID:   bond.CoinID,
		Amount:   bond.Amount,
		Strength: bond.Strength,
		LockTime: bond.LockTime,
	}
}

// dbBond converts a replicated wire bond to its storage form.
func dbBond(bond *meshevents.Bond) *db.Bond {
	return &db.Bond{
		Version:  bond.Version,
		AssetID:  bond.AssetID,
		CoinID:   bond.CoinID,
		Amount:   bond.Amount,
		Strength: bond.Strength,
		LockTime: bond.LockTime,
	}
}

// dbEventLogMeta builds the storage event-log metadata for a replicated mesh
// event.
func dbEventLogMeta(meta *db.EventLogPosition, event *mesh.Event) *db.EventLogMeta {
	if event == nil {
		return nil
	}
	logMeta := &db.EventLogMeta{
		Event: append([]byte(nil), event.Payload...),
	}
	if meta != nil {
		logMeta.Seq = meta.Seq
		logMeta.ExpectedTipHash = append([]byte(nil), meta.TipHash...)
	}
	return logMeta
}

// Events returns the mesh event appliers. Each applies an already-decided
// event to this node's state and event log.
func (auth *AuthManager) Events() map[string]mesh.EventApplier {
	return map[string]mesh.EventApplier{
		meshevents.EventKindBondPosted: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			posted, err := meshevents.DecodeBondPostedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return auth.applyBondPostedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), posted)
		},
		meshevents.EventKindPrepaidBondsCreated: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			created, err := meshevents.DecodePrepaidBondsCreatedEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return auth.applyPrepaidBondsCreatedEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), created)
		},
		meshevents.EventKindReputationForgiven: func(applyCtx *mesh.EventApplyContext, event *mesh.Event) (*db.EventLogEntry, error) {
			forgiven, err := meshevents.DecodeReputationForgivenEvent(event.Payload)
			if err != nil {
				return nil, err
			}
			return auth.applyReputationForgivenEvent(applyCtx, dbEventLogMeta(applyCtx.Position, event), forgiven)
		},
	}
}

func (auth *AuthManager) applyReputationForgivenEvent(applyCtx *mesh.EventApplyContext, logMeta *db.EventLogMeta, event *meshevents.ReputationForgivenEvent) (*db.EventLogEntry, error) {
	ctx := applyCtx.Context
	result, err := auth.storage.ApplyReputationForgivenEvent(ctx, logMeta, event)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Log == nil {
		return nil, fmt.Errorf("storage returned nil reputation forgiveness result")
	}

	forgivenessResult := &reputationForgivenessResult{
		Forgiven: result.Forgiven,
	}
	afterRep, _, refreshErr := auth.eventReputationFromDB(ctx, event.AccountID)
	if refreshErr == nil {
		forgivenessResult.Unbanned = afterRep != nil && afterRep.EffectiveTier() > 0
	} else {
		log.Errorf("failed to refresh reputation after forgiveness for account %v: %v", event.AccountID, refreshErr)
	}
	applyCtx.SetResult(forgivenessResult)
	return result.Log, nil
}

func (auth *AuthManager) eventReputationFromDB(ctx context.Context, user account.AccountID) (*account.Reputation, int32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(ctx, reputationEventRefreshTimeout)
	defer cancel()
	return auth.reputationFromDB(readCtx, user)
}

func (auth *AuthManager) finalizePostBondResult(acctID account.AccountID, result *msgjson.PostBondResult) {
	result.Reputation = auth.ComputeUserReputation(acctID)
	if len(result.SigBytes()) == 0 {
		auth.Sign(result)
	}
}

func (auth *AuthManager) submitBondPostedEvent(ctx context.Context, completion *mesh.CommandCompletion, acct *account.Account, bond *db.Bond, postBondResult *msgjson.PostBondResult) error {
	meshEvent, err := mesh.NewEvent(meshevents.NewBondPostedEvent(acct, wireBond(bond)))
	if err != nil {
		return err
	}
	if len(postBondResult.SigBytes()) == 0 && auth.signer == nil {
		return fmt.Errorf("bond result missing signature and signer not configured")
	}

	err = completion.Emit(ctx, meshEvent, func() any {
		auth.finalizePostBondResult(acct.ID, postBondResult)
		return postBondResult
	})

	return err
}

func (auth *AuthManager) applyPrepaidBondsCreatedEvent(ctx context.Context, logMeta *db.EventLogMeta, event *meshevents.PrepaidBondsCreatedEvent) (*db.EventLogEntry, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return auth.storage.ApplyPrepaidBondsCreatedEvent(ctx, logMeta, event)
}
