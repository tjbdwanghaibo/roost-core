package nest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/dataengine"
)

// DurabilityPolicy is independent from rollback policy. Rollback controls
// failures before the commit point; durability controls when the commit point
// is acknowledged.
type DurabilityPolicy = dataengine.Durability

const (
	DurabilityMemory DurabilityPolicy = iota
	DurabilityAsync
	DurabilityStrict
	// DurabilityPipelined splits admission: Enqueue (in-lock) is the only
	// rejection point, fsync happens out of lock, and success is externalized
	// (reply, AfterCommit) only after the commit ticket resolves durable.
	// See NEST_PIPELINED_COMMIT.md for the full contract.
	DurabilityPipelined
)

func ParseDurabilityPolicy(value string) (DurabilityPolicy, error) {
	switch value {
	case "", "memory":
		return DurabilityMemory, nil
	case "async":
		return DurabilityAsync, nil
	case "strict":
		return DurabilityStrict, nil
	case "pipelined":
		return DurabilityPipelined, nil
	default:
		return DurabilityMemory, fmt.Errorf("nest: unsupported durability policy %q", value)
	}
}

// TransactionID is sortable by its process-random prefix and local sequence.
// It avoids a crypto/rand call on every hot-path transaction.
type TransactionID = dataengine.TransactionID

var transactionIDState struct {
	prefix [8]byte
	seq    atomic.Uint64
}

func init() {
	if _, err := rand.Read(transactionIDState.prefix[:]); err != nil {
		binary.BigEndian.PutUint64(transactionIDState.prefix[:], uint64(time.Now().UnixNano()))
	}
}

func newTransactionID() TransactionID {
	var id TransactionID
	copy(id[:8], transactionIDState.prefix[:])
	binary.BigEndian.PutUint64(id[8:], transactionIDState.seq.Add(1))
	return id
}

// These aliases keep existing Nest and generated callers source compatible
// while the durable model is owned by the Data Engine package.
type EntityMutation = dataengine.Mutation
type Effect = dataengine.Effect
type CommitRecord = dataengine.CommitRecord

// CommitFence identifies one WAL record for acknowledgement.
type CommitFence struct {
	TransactionID TransactionID
	Segment       uint64
	Offset        int64
}

// CommitWAL is implemented by infrastructure packages. Append is the commit
// point. Replay must return unacknowledged records in original append order.
type CommitWAL interface {
	Append(ctx context.Context, record CommitRecord) (CommitFence, error)
	Ack(ctx context.Context, fence CommitFence) error
	Replay(ctx context.Context, consume func(CommitFence, CommitRecord) error) error
}

// TransactionCommitter performs durable admission and arranges replay/outbox
// delivery. It must never report failure after it has durably accepted a
// record; post-commit delivery failures are retried out of band.
type TransactionCommitter interface {
	Commit(ctx context.Context, record CommitRecord) error
}

// TransactionReleaseNotifier is an optional committer capability. Nest calls
// it after all entity locks have been released, allowing replay workers to
// avoid racing the normal checkpoint path in the committing process.
type TransactionReleaseNotifier interface {
	TransactionReleased(TransactionID)
}

// CommitTicket resolves when an enqueued record becomes durable. Err is nil
// on success or ErrCommitIndeterminate when the fsync outcome is unknown; no
// other error is legal — every rejectable condition must be reported
// synchronously by Enqueue while the caller still holds entity locks and can
// roll back.
type CommitTicket interface {
	LSN() uint64
	Done() <-chan struct{}
	Err() error
}

// PipelinedTransactionCommitter is an optional committer capability backing
// DurabilityPipelined. Enqueue is called with entity locks held: it must
// perform ALL rejectable validation and buffer admission synchronously
// (rejecting instead of waiting under backpressure — the caller holds locks),
// assign the LSN, and return; the group-commit worker resolves the ticket.
// DurableLSN is the monotone watermark: every record with LSN <= it is
// durable (prefix durability). See NEST_PIPELINED_COMMIT.md.
type PipelinedTransactionCommitter interface {
	TransactionCommitter
	Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error)
	DurableLSN() uint64
}

// CommitParticipant materializes a final after-image/delta exactly once at
// the end of a successful handler. Implementations must not clear dirty or
// patch metadata while preparing the record.
type CommitParticipant interface {
	PrepareCommit(tx *RollbackTx) error
}

func cloneMutation(m EntityMutation) EntityMutation {
	return dataengine.CloneMutation(m)
}

func cloneEffect(effect Effect) Effect {
	return dataengine.CloneEffect(effect)
}

func validateCommitRecord(record CommitRecord) error {
	return dataengine.ValidateCommitRecord(record)
}
