package global

import "context"

// run is the periodic work the global routing process does.
//
// It does none. Every change to a binding is a compare-and-set a caller makes,
// and nothing about a binding decays with time, so there is nothing for a
// sweep to reclaim.
//
// The contrast worth holding is with the activity service, which was split out
// of this package and DOES need a sweep. There, a grace window that closed
// must be aggregated by someone, and if nobody looks, the games waiting on the
// result wait forever. Here, nobody is waiting on a reclamation.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
