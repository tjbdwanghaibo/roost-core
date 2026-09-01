package nestwal

import (
	"context"

	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

// Runtime groups the WAL and recovery committer so application wiring stays
// to one Nest option and one shutdown call.
type Runtime struct {
	WAL       *WAL
	Committer *Committer
}

func OpenRuntime(walOptions Options, applier MutationApplier, publisher EffectPublisher, committerOptions CommitterOptions) (*Runtime, error) {
	wal, err := Open(walOptions)
	if err != nil {
		return nil, err
	}
	committer, err := NewCommitter(wal, applier, publisher, committerOptions)
	if err != nil {
		_ = wal.Close(context.Background())
		return nil, err
	}
	return &Runtime{WAL: wal, Committer: committer}, nil
}

func (r *Runtime) NestOption() corenest.NestOption {
	if r == nil {
		return corenest.NestOptionWithTransactionCommitter(nil)
	}
	return corenest.NestOptionWithTransactionCommitter(r.Committer)
}

// DurableWatermark returns the pipelined-commit watermark source for
// externalization gates:
//
//	coordinator.SetDurableWatermark(runtime.DurableWatermark())
func (r *Runtime) DurableWatermark() func() uint64 {
	if r == nil || r.Committer == nil {
		return func() uint64 { return 0 }
	}
	return r.Committer.DurableLSN
}

func (r *Runtime) Flush(ctx context.Context) error {
	if r == nil || r.Committer == nil {
		return nil
	}
	return r.Committer.Flush(ctx)
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.Committer == nil {
		return nil
	}
	return r.Committer.Shutdown(ctx)
}
