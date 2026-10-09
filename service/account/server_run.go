package account

import "context"

// run is the periodic work the account process does.
//
// It does none, and unlike rank — which has nothing time-bound at all — this
// one has an expiry and still needs no ticker, which is worth being precise
// about.
//
// A session carries ExpiresAtUnix, and ValidateSession enforces it on every
// read. That is the whole enforcement: an expired session is refused the next
// time it is used, so there is nothing a sweep would make true that is not
// already true. Sessions are not stored as records this package owns and must
// reclaim; they are signed tokens plus a role lookup.
//
// The contrast with session and match is the point. There, an entry sits in
// durable storage holding a resource — an occupancy slot, a place in a queue —
// and nothing reclaims it unless something looks. Here, nothing is held.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
