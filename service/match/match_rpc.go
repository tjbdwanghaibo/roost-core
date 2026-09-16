package match

import "context"

// The transport for Match is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir . -emit transport

// Matchmaker is the cross-process contract: what ANOTHER process may ask of
// the match service.
//
// Not "Match": this package already has a Match, and it is a formed match —
// the thing the service produces. An interface and a value type of one name in
// one package is how a reader ends up looking at the wrong one, and the
// generator would also have shadowed the Match method with an embedded field
// of the same name.
//
// Every method carries an affinity key derived from the queue, and that is the
// load-bearing decision here rather than a tuning choice. The whole queue
// state is one versioned entry — which is what makes a group commit a single
// compare-and-set — so throughput on one queue is bounded by contention on one
// key. Round-robin routing across replicas turns that bound into cross-replica
// contention, and it is the DESIGN CAUSE of the contention the implementation
// this replaces suffered, not a performance problem it had. Routing a queue's
// traffic to one instance keeps the contention in one process.
//
// Sweep is not here: resolving expired tickets is the owning process's own
// periodic work, and it belongs in the Server's run hook.
//
//roost:rpc service_type=match capability=service.match
type Matchmaker interface {
	// Enqueue adds a subject to a queue. Idempotent per requestID.
	//
	//roost:rpc affinity=queue.Key()
	Enqueue(ctx context.Context, queue Queue, subject Subject, requestID string) (ticket Ticket, err error)

	// Cancel withdraws a ticket. The subject is required, so a caller cannot
	// cancel a ticket it does not own — the confirmed defect it answers took
	// the ticket id from the client and used it as identity.
	//
	//roost:rpc affinity=queue.Key()
	Cancel(ctx context.Context, queue Queue, ticketID string, subject Subject) (ticket Ticket, err error)

	// Ticket reads one ticket, checking ownership.
	//
	//roost:rpc affinity=queue.Key()
	Ticket(ctx context.Context, queue Queue, ticketID string, subject Subject) (ticket Ticket, found bool, err error)

	// Candidates reads a bounded window of waiting tickets, for a caller that
	// forms its own groups.
	//
	//roost:rpc affinity=queue.Key()
	Candidates(ctx context.Context, queue Queue, limit int) (tickets []Ticket, err error)

	// Commit forms a match from tickets in ONE compare-and-set, which is what
	// makes the batch atomic — the implementation this replaces wrote it in
	// three non-atomic steps and lost whole batches from the queue.
	//
	//roost:rpc affinity=queue.Key()
	Commit(ctx context.Context, queue Queue, ticketIDs []string) (match Match, err error)

	// Match reads one match.
	//
	//roost:rpc affinity=queue.Key()
	Match(ctx context.Context, queue Queue, matchID string) (match Match, found bool, err error)

	// QueueLength reports how many subjects are waiting.
	//
	//roost:rpc affinity=queue.Key()
	QueueLength(ctx context.Context, queue Queue) (length int, err error)
}

var _ Matchmaker = (Store)(nil)
