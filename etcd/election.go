package etcd

import "context"

// IElection provides leader election for exclusive services.
type IElection interface {
	// Campaign starts a campaign to become leader.
	// Blocks until elected or context cancelled.
	Campaign(ctx context.Context, value string) error

	// Resign gives up leadership.
	Resign(ctx context.Context) error

	// Leader returns the current leader value.
	Leader(ctx context.Context) (string, error)

	// IsLeader returns true if this instance is the current leader.
	//
	// There is an inherent stale window: between the lease expiring on the
	// etcd server (another candidate can win) and this client observing the
	// session end, IsLeader still returns true. Leadership-sensitive writes
	// must therefore not trust this flag alone — carry the fencing token from
	// IFencedElection and let the write path compare tokens.
	IsLeader() bool

	// LeaderChan returns a channel that is closed when this instance loses leadership.
	LeaderChan() <-chan struct{}
}

// IFencedElection is an optional IElection capability. Fence returns the
// fencing token of the current leadership term: a value that increases
// monotonically across leadership changes of the same prefix. Downstream
// writes tag themselves with the token and reject anything older, which
// closes the IsLeader stale window — a deposed leader's writes lose the
// comparison once the new leader (holding a higher token) has written.
type IFencedElection interface {
	IElection

	// Fence returns the current term's fencing token. ok is false when this
	// instance does not believe it is the leader.
	Fence() (token int64, ok bool)
}

// IElectionFactory creates election instances for different election keys.
type IElectionFactory interface {
	// NewElection creates an election for the given prefix.
	// e.g. "/election/center" — all center candidates compete under this prefix.
	NewElection(prefix string) IElection
}
