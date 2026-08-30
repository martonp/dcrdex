// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"context"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/market"
	"decred.org/dcrdex/server/mesh"
	"decred.org/dcrdex/server/meshevents"
)

const (
	// miaSweepInterval is how often the master re-evaluates booked users.
	// Shorter than peerConnectedQueryInterval so local/tier changes are
	// noticed without extra mesh traffic.
	miaSweepInterval = 15 * time.Second

	// peerConnectedQueryInterval is the minimum time between peer
	// client_connected polls. Between polls, sweeps reuse the cached answer.
	peerConnectedQueryInterval = time.Minute

	// peerConnectedQueryTimeout bounds one peer connectivity query.
	peerConnectedQueryTimeout = 30 * time.Second
)

// presenceMeshService is the mesh surface the unbooker needs.
type presenceMeshService interface {
	QueryClientConnected(ctx context.Context, users []account.AccountID) ([]account.AccountID, error)
	ApplyEvent(context.Context, *mesh.Event) (any, error)
}

// bookedUserSource reports accounts with booked orders on one market.
type bookedUserSource interface {
	BookedUsers() map[account.AccountID]int
}

// tierSource reports local connectivity and reputation for a user.
// On a reputation load failure, return an error; do not report tier 0.
type tierSource interface {
	AcctRepStatus(user account.AccountID) (connected bool, rep *account.Reputation, err error)
}

// presenceUnbooker is a master worker that revokes booked orders for
// tier < 1 users and for users absent from every mesh node longer than the
// MIA timeout. Peer connectivity is pulled via client_connected (at most once
// per peerConnectedQueryInterval); a failed query means no peer clients.
// Conclusions are recorded as orders_revoked events.
type presenceUnbooker struct {
	log     dex.Logger
	markets map[string]bookedUserSource
	tiers   tierSource
	timeout time.Duration
	mesh    presenceMeshService

	// Master sweep state (runMaster goroutine only).
	absentSince   map[account.AccountID]time.Time
	peerConnected map[account.AccountID]struct{}
	lastQuery     time.Time
	lastQueryOK   bool
}

func newPresenceUnbooker(log dex.Logger, markets map[string]*market.Market, tiers tierSource, miaTimeout time.Duration) *presenceUnbooker {
	bookSources := make(map[string]bookedUserSource, len(markets))
	for name, mkt := range markets {
		bookSources[name] = mkt
	}
	return newPresenceUnbookerForSources(log, bookSources, tiers, miaTimeout)
}

func newPresenceUnbookerForSources(log dex.Logger, markets map[string]bookedUserSource, tiers tierSource, miaTimeout time.Duration) *presenceUnbooker {
	return &presenceUnbooker{
		log:         log,
		markets:     markets,
		tiers:       tiers,
		timeout:     miaTimeout,
		absentSince: make(map[account.AccountID]time.Time),
	}
}

// setMesh must be called before the master worker runs.
func (p *presenceUnbooker) setMesh(mesh presenceMeshService) {
	p.mesh = mesh
}

// masterWorker returns the unbooker sweep. Register it after market workers
// so books are restored before the first sweep.
func (p *presenceUnbooker) masterWorker() mesh.MasterWorker {
	return mesh.MasterWorker{
		Name: "Unbooker",
		Run:  p.runMaster,
	}
}

// runMaster runs the MIA sweep until ctx is canceled. On promotion, absence
// and peer-query clocks restart; the first sweep is local-only so readiness
// is not blocked on the peer. Wrongly armed clocks clear on the first
// successful peer answer, or via the pre-fire re-query at fire time.
func (p *presenceUnbooker) runMaster(ctx context.Context, reportReady func(error)) {
	now := time.Now()
	p.absentSince = make(map[account.AccountID]time.Time)
	p.peerConnected = nil
	p.lastQuery = now
	p.lastQueryOK = false

	p.sweep(ctx, now)
	reportReady(nil)

	ticker := time.NewTicker(miaSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.sweep(ctx, time.Now())
		case <-ctx.Done():
			return
		}
	}
}

// queryPeerConnected returns the peer's connected subset. ok is false when
// the query fails; callers treat that as no peer connectivity.
func (p *presenceUnbooker) queryPeerConnected(ctx context.Context, users []account.AccountID) (map[account.AccountID]struct{}, bool) {
	queryCtx, cancel := context.WithTimeout(ctx, peerConnectedQueryTimeout)
	defer cancel()
	connected, err := p.mesh.QueryClientConnected(queryCtx, users)
	if err != nil {
		p.log.Debugf("Peer connectivity query for %d users failed: %v", len(users), err)
		return nil, false
	}
	set := make(map[account.AccountID]struct{}, len(connected))
	for _, user := range connected {
		set[user] = struct{}{}
	}
	return set, true
}

// refreshPeerConnected refreshes the peer-connected cache when due. A failed
// query still advances lastQuery (empty set) so a down peer is not hammered.
func (p *presenceUnbooker) refreshPeerConnected(ctx context.Context, now time.Time, candidates []account.AccountID) {
	if len(candidates) == 0 || now.Sub(p.lastQuery) < peerConnectedQueryInterval {
		return
	}
	p.lastQuery = now
	p.peerConnected, p.lastQueryOK = p.queryPeerConnected(ctx, candidates)
}

type pendingRevoke struct {
	user   account.AccountID
	reason meshevents.OrderRevokeReason
}

func (p *presenceUnbooker) sweep(ctx context.Context, now time.Time) {
	booked := p.bookedUsers()
	p.pruneAbsent(booked)

	tierRevokes, candidates := p.classifyBooked(booked)
	p.refreshPeerConnected(ctx, now, candidates)
	miaFires := p.confirmMIAFires(ctx, now, p.advanceMIA(candidates, now))

	p.applyRevokes(ctx, now, booked, tierRevokes, miaFires)
}

func (p *presenceUnbooker) bookedUsers() map[account.AccountID]int {
	booked := make(map[account.AccountID]int)
	for _, mkt := range p.markets {
		for user, count := range mkt.BookedUsers() {
			booked[user] += count
		}
	}
	return booked
}

func (p *presenceUnbooker) pruneAbsent(booked map[account.AccountID]int) {
	for user := range p.absentSince {
		if booked[user] == 0 {
			delete(p.absentSince, user)
		}
	}
}

// classifyBooked returns immediate reputation unbooks and MIA candidates.
// Locally connected users clear their MIA clock.
//
// If reputation cannot be loaded, skip the reputation unbook this sweep. MIA
// still applies. A real bad tier is caught on a later sweep when the load
// works. If the load keeps failing, the user stays exempt; we warn once per
// sweep.
func (p *presenceUnbooker) classifyBooked(booked map[account.AccountID]int) (tierRevokes []account.AccountID, candidates []account.AccountID) {
	var loadErrs, bondExpired int
	var lastErr error
	for user := range booked {
		connected, rep, err := p.tiers.AcctRepStatus(user)
		if err != nil {
			loadErrs++
			lastErr = err
		} else if shouldUnbookForReputation(rep) {
			delete(p.absentSince, user)
			tierRevokes = append(tierRevokes, user)
			continue
		} else if rep.EffectiveTier() < 1 {
			bondExpired++
		}
		if connected {
			delete(p.absentSince, user)
			continue
		}
		candidates = append(candidates, user)
	}
	if bondExpired > 0 {
		p.log.Debugf("%d of %d booked users are below tier 1 from bond expiry only; not unbooking for reputation.",
			bondExpired, len(booked))
	}
	if loadErrs > 0 {
		p.log.Warnf("Reputation unavailable for %d of %d booked users; skipping their tier check this sweep. Last error: %v",
			loadErrs, len(booked), lastErr)
	}
	return tierRevokes, candidates
}

// shouldUnbookForReputation reports whether booked orders should be revoked
// immediately from reputation. nil rep (unknown account): yes. Tier < 1
// with no score penalties is bond expiry only: no.
func shouldUnbookForReputation(rep *account.Reputation) bool {
	return rep == nil || (rep.EffectiveTier() < 1 && rep.Penalties > 0)
}

// advanceMIA arms/clears MIA clocks against the cached peer set and returns
// users whose deadline is due. No mesh I/O.
func (p *presenceUnbooker) advanceMIA(candidates []account.AccountID, now time.Time) []account.AccountID {
	var fires []account.AccountID
	for _, user := range candidates {
		if _, found := p.peerConnected[user]; found {
			delete(p.absentSince, user)
			continue
		}
		since, tracked := p.absentSince[user]
		if !tracked {
			p.absentSince[user] = now
			continue
		}
		if now.Sub(since) >= p.timeout {
			fires = append(fires, user)
		}
	}
	return fires
}

// confirmMIAFires re-queries the fire set unless this sweep already got a
// successful full-candidate refresh. Rescues are folded into peerConnected;
// lastQuery is not updated (fire-set only).
func (p *presenceUnbooker) confirmMIAFires(ctx context.Context, now time.Time, fires []account.AccountID) []account.AccountID {
	if len(fires) == 0 || (p.lastQuery.Equal(now) && p.lastQueryOK) {
		return fires
	}
	fresh, ok := p.queryPeerConnected(ctx, fires)
	if !ok {
		return fires
	}
	confirmed := make([]account.AccountID, 0, len(fires))
	for _, user := range fires {
		if _, found := fresh[user]; found {
			delete(p.absentSince, user)
			continue
		}
		confirmed = append(confirmed, user)
	}
	if len(fresh) == 0 {
		return confirmed
	}
	if p.peerConnected == nil {
		p.peerConnected = make(map[account.AccountID]struct{}, len(fresh))
	}
	for user := range fresh {
		p.peerConnected[user] = struct{}{}
	}
	return confirmed
}

// applyRevokes emits orders_revoked. Failed applies are retried next sweep.
func (p *presenceUnbooker) applyRevokes(ctx context.Context, now time.Time, booked map[account.AccountID]int, tierRevokes, miaFires []account.AccountID) {
	revokes := make([]pendingRevoke, 0, len(tierRevokes)+len(miaFires))
	for _, user := range tierRevokes {
		revokes = append(revokes, pendingRevoke{user: user, reason: meshevents.OrderRevokeReasonPenalty})
	}
	for _, user := range miaFires {
		revokes = append(revokes, pendingRevoke{user: user, reason: meshevents.OrderRevokeReasonDisconnected})
	}

	for _, rev := range revokes {
		if ctx.Err() != nil {
			return
		}
		switch rev.reason {
		case meshevents.OrderRevokeReasonPenalty:
			p.log.Infof("Revoking booked orders of user %v: tier below 1 (%d booked orders).",
				rev.user, booked[rev.user])
		case meshevents.OrderRevokeReasonDisconnected:
			p.log.Infof("Revoking booked orders of user %v: disconnected from the mesh for over %v (%d booked orders).",
				rev.user, p.timeout, booked[rev.user])
		}
		event, err := mesh.NewEvent(meshevents.NewOrdersRevokedForUserEvent(rev.user, rev.reason, now.UTC()))
		if err != nil {
			p.log.Errorf("Failed to build orders_revoked event for user %v (reason %d): %v",
				rev.user, rev.reason, err)
			continue
		}
		if _, err := p.mesh.ApplyEvent(ctx, event); err != nil {
			p.log.Errorf("Failed to apply orders_revoked event for user %v (reason %d): %v",
				rev.user, rev.reason, err)
			continue
		}
		if rev.reason == meshevents.OrderRevokeReasonDisconnected {
			delete(p.absentSince, rev.user)
		}
	}
}
