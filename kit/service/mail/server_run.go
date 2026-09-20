package mail

import "context"

// run is the periodic work the mail process does.
//
// It does none, and saying so explicitly is the point of the hook being
// required rather than defaulted. Envelopes expire by Redis key ttl and
// mailboxes evict inline on delivery, so there is nothing for a ticker to do —
// and a generated blocking default would have answered that question by not
// asking it.
//
// One gap this makes visible, recorded rather than fixed: a mailbox entry
// whose envelope has expired stays in the mailbox until eviction pressure
// removes it. List counts those as list.missing_envelope, so they are
// observable, but nothing prunes them. A sweep for that would go here, driven
// by a ticker bounded by this context.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
