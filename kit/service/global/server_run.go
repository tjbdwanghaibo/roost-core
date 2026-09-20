package global

import "context"

// run is the periodic work the global routing process does.
//
// It does none, and this one deserves the explanation more than rank's did,
// because a lease looks exactly like something a sweep should reclaim.
//
// It is not, because a lease's expiry is a FIELD this package reads on every
// access — that is why the lease store deliberately has no key TTL. An expired
// lease is not held: AcquireLease takes it, RenewLease refuses the stale
// incarnation, and Lease reports it as expired. Nothing a sweep could do is
// not already done at the moment of use, and a sweep that deleted expired
// lease KEYS would be actively harmful: deleting a versioned entry restarts it
// at version 1, and the incarnation fence a renewal must pass becomes a
// comparison against a version that just reset.
//
// The contrast worth holding is with the activity service, which was split out
// of this package and DOES need a sweep. There, a grace window that closed
// must be aggregated by someone, and if nobody looks, the games waiting on the
// result wait forever. Here, nobody is waiting on a reclamation.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
