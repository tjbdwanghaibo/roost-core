package session

import "context"

// The transport for Session is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir . -emit transport

// Session is the cross-process contract: what ANOTHER process may ask of the
// session service.
//
// Sweep is not here, and that is the judgement in this file. It is the owning
// process's own periodic work — it resolves expired runs and releases their
// resources — and it belongs in the Server's run hook, where a context bounds
// it and its failure is the process's failure. A peer that could drive another
// service's sweep would be a peer deciding that service's cadence, and two
// peers driving it would be two sweeps racing over the same claims.
//
//roost:rpc service_type=session capability=service.session
type Session interface {
	// Enter opens a run for an owner. Idempotent per RequestID, which is
	// required: without it a retry allocates a second run, and the older one
	// becomes unreachable.
	Enter(ctx context.Context, ownerID int64, req EnterRequest) (run Run, err error)

	// Attach records an external resource on a run, so releasing it later is
	// walking a list rather than a hand-written unwind per failure path.
	Attach(ctx context.Context, ownerID int64, runID string, resource Resource) (run Run, err error)

	// Finish resolves a run to a terminal state the caller chooses and
	// releases its resources.
	Finish(ctx context.Context, ownerID int64, runID string, state State, outcome string) (run Run, err error)

	// Leave abandons a run.
	Leave(ctx context.Context, ownerID int64, runID string, reason string) (run Run, err error)

	// Get reads a run. An elapsed deadline reads as expired before anything
	// sweeps, so a caller never sees a run that is only open because nothing
	// has resolved it yet.
	Get(ctx context.Context, ownerID int64, runID string) (run Run, found bool, err error)

	// Current returns an owner's live run, if any. This is the index whose
	// absence made a leaked run unreachable in the implementation this
	// replaces.
	Current(ctx context.Context, ownerID int64) (run Run, found bool, err error)
}

var _ Session = (*Service)(nil)
