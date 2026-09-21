package activity

import "context"

// The transport for Coordinator is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .

// Coordinator is the cross-process contract: what ANOTHER process may ask of
// the activity coordination service.
//
// Five of the Service methods are not here, in two groups.
//
// AdvanceExpired and DueDispatches are the owning process's own periodic work:
// back-stopping an aggregation whose grace window closed, and enumerating what
// is outstanding for metrics. They are what the Server's run hook drives.
// Exposing them would let any process on the bus advance activities it does
// not own, on a cadence nobody configured.
//
// AttemptDispatch USED to be in that group, and moving it out is the whole of
// RR-20260919-10. It was the sweep that called it — which spends one of the
// delivery budget's attempts and returns the payload — and there is no
// transport on the service side, so the payload went nowhere and a game that
// was merely down found its result "retried" into exhausted. The taker is the
// game, so the game is who calls it: an attempt is now spent exactly when
// somebody takes the payload. It carries the caller's own gameSID, the same
// trust model NotifyPhase already has.
//
// NotifyAudits and AuditOverflow are the diagnostic surface: why was this
// game's notification refused, and how many audits were dropped. They answer
// an operator's question, not a game's, and they belong with the operator
// tools rather than on the path a game server calls in a loop.
//
// Every method carries an affinity key derived from the group, and that is the
// load-bearing decision here rather than a tuning choice. A group's PENDING
// WINDOW is one versioned entry — it is what bounds the group's open
// activities and what OpenActivity must be admitted through — so every
// activity in a group commits through the same compare-and-set. Round-robin
// routing across replicas turns that into cross-replica contention on one key.
// Routing by group keeps it in one process, the same reason match routes by
// queue.
//
// The key is Key.Group() rather than Key.String(): routing per ACTIVITY would
// spread one group's window traffic across instances, which is the contention
// this is meant to avoid, and is the mistake that looks correct because the
// activity is the more obvious unit.
//
//roost:rpc service_type=activity capability=service.global.activity
type Coordinator interface {
	// OpenActivity opens an activity for a named set of game servers. It is
	// admitted through the group's pending window, so a group cannot
	// accumulate unbounded open activities.
	//
	//roost:rpc affinity=key.Group()
	OpenActivity(ctx context.Context, key Key, expectedGameSIDs []int32) (activity Activity, err error)

	// LookupActivity reads one activity.
	//
	//roost:rpc affinity=key.Group()
	LookupActivity(ctx context.Context, key Key) (activity Activity, found bool, err error)

	// PendingActivities lists a group's activities that are not finished. It
	// is the only enumerable index of them, and it is bounded.
	//
	//roost:rpc affinity=groupID
	PendingActivities(ctx context.Context, groupID string, limit int) (keys []Key, err error)

	// NotifyPhase records that one game server reached the phase. This
	// service never advances an activity from its own clock; a game's
	// notification is what starts the grace window.
	//
	//roost:rpc affinity=key.Group()
	NotifyPhase(ctx context.Context, key Key, gameSID int32) (activity Activity, err error)

	// ApplyProgress adds a participant's progress, exactly once per
	// requestID. The reservation is claimed insert-only before the score
	// moves, so a redelivery cannot count twice.
	//
	//roost:rpc affinity=key.Group()
	ApplyProgress(ctx context.Context, key Key, participantID string, requestID string, delta ProgressDelta) (participant Participant, err error)

	// LookupParticipant reads one participant's accumulated progress.
	//
	//roost:rpc affinity=key.Group()
	LookupParticipant(ctx context.Context, key Key, participantID string) (participant Participant, found bool, err error)

	// Reservation reads what one requestID already did. It is how a caller
	// that lost a response learns whether its progress landed, instead of
	// retrying blind or giving up.
	//
	//roost:rpc affinity=key.Group()
	Reservation(ctx context.Context, key Key, participantID string, requestID string) (reservation ProgressReservation, found bool, err error)

	// OwedDispatches lists the activities whose result this game server still
	// owes an ack for, due now, oldest first.
	//
	// It is the entry point a game server drains on start and on a timer, and
	// it exists because the alternative is guessing: before it, a game had to
	// compute activity ids from its own clock and look each one up, which
	// stops working the moment it is down longer than one window — the
	// obligation stays recorded and becomes permanently unreachable
	// (RR-20260919-10).
	//
	// The index is per (group, game), so this routes by group like every
	// other call about an activity — a game that belongs to several groups
	// drains each of them, and it knows which ones it belongs to because a
	// binding is how it got there.
	//
	//roost:rpc affinity=groupID
	OwedDispatches(ctx context.Context, groupID string, gameSID int32, limit int) (keys []Key, err error)

	// LookupDispatch reads the result delivery owed to one game server.
	//
	//roost:rpc affinity=key.Group()
	LookupDispatch(ctx context.Context, key Key, gameSID int32) (dispatch Dispatch, found bool, err error)

	// AttemptDispatch takes the result owed to one game server and spends one
	// of the delivery attempts.
	//
	// It returns the payload and the ack token. A caller that loses the reply
	// asks again: the attempt is spent either way (that is what makes the
	// budget a budget), but the result is unchanged and the token is the
	// same, so nothing is delivered twice.
	//
	//roost:rpc affinity=key.Group()
	AttemptDispatch(ctx context.Context, key Key, gameSID int32) (dispatch Dispatch, err error)

	// AckDispatch is a game server confirming it applied the result. The
	// token is required, so an ack cannot be forged from the activity key
	// alone and a stale ack cannot close a redelivered dispatch.
	//
	//roost:rpc affinity=key.Group()
	AckDispatch(ctx context.Context, key Key, gameSID int32, token string) (dispatch Dispatch, err error)
}

var _ Coordinator = (*Service)(nil)
