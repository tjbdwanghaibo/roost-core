package rank

import "context"

// run is the periodic work the rank process does.
//
// It does none. Rank has no expiry to sweep and no retry queue to drain: a
// score is written under compare-and-swap and stays until it is replaced,
// removed, or the board is reset — and a reset is an operator action, not
// something a ticker decides.
//
// Saying so explicitly is why the hook is required rather than defaulted. A
// generated blocking default would have answered the question by not asking
// it, and "does this service need a sweep" is a question worth asking once per
// service: match, session and platform each answered yes.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
