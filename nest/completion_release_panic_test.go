package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type releaseFailureCommitter struct {
	*pipelinedTestCommitter
	outcome error
}

func (c *releaseFailureCommitter) Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error) {
	ticket, err := c.pipelinedTestCommitter.Enqueue(ctx, record)
	c.resolveAll(c.outcome)
	return ticket, err
}

// 直接走生产 NestDispatch 和 Guard；饱和用例在未启动的完成队列里占满唯一槽位，
// 确定性触发降级，不依赖生产者/消费者的运行速度。
func TestPipelinedReleasePanicStillCompletesAndReplies(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		indeterminate bool
	}{
		{"inline", false}, {"queued", false}, {"saturated", false},
		{"inline", true}, {"queued", true}, {"saturated", true},
	} {
		mode := tc.mode
		label := mode
		if tc.indeterminate {
			label += "_indeterminate"
		}
		t.Run(label, func(t *testing.T) {
			id, ent := newAsyncPilotEntity(t, 10020, 10)
			manager := entity.NewEntityManager()
			if err := manager.TryAdd(ent); err != nil {
				t.Fatal(err)
			}
			releaseErr := errors.New("release hook failed")
			defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic(releaseErr) })()
			getter := newMockGetter()
			getter.Add(ent)
			committer := &releaseFailureCommitter{pipelinedTestCommitter: newPipelinedTestCommitter(false)}
			if tc.indeterminate {
				committer.outcome = ErrCommitIndeterminate
			}
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			if mode != "inline" {
				engine.completions = newCompletionPump(1, 1)
				engine.completions.fence = engine.Fence
				if mode == "queued" {
					engine.completions.start()
				} else {
					engine.completions.queue <- pipelinedCompletion{}
				}
			}
			calls := 0
			name := NewHandlerName("release_panic_" + mode)
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				old := ent.dao.Value
				RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil })
				ent.dao.Value = 20
				AfterCommit(func() { calls++ })
				return "committed", MarkPersist(ent.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
			msg := &Msg{Type: MsgTypeSingle, Name: name.String(), Tid: id, RetChan: make(chan any, 2)}
			NestDispatch(engine, msg)
			if mode == "queued" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := engine.completions.stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-msg.RetChan:
				if err, ok := result.(error); !ok || !errors.Is(err, releaseErr) || !errors.Is(err, ErrEntityReleaseFailed) {
					t.Errorf("reply=%v, want release failure after completion", result)
				} else if errors.Is(err, ErrCommitIndeterminate) != tc.indeterminate {
					t.Errorf("reply=%v, indeterminate=%t", result, tc.indeterminate)
				}
			default:
				t.Error("accepted transaction lost its reply")
			}
			if len(msg.RetChan) != 0 {
				t.Error("duplicate reply")
			}
			wantCalls := 1
			if tc.indeterminate {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Errorf("AfterCommit calls=%d, want %d", calls, wantCalls)
			}
			if committer.released.IsZero() != tc.indeterminate {
				t.Error("incorrect release notification for ticket outcome")
			}
			if errors.Is(engine.FenceError(), ErrCommitIndeterminate) != tc.indeterminate {
				t.Error("incorrect fencing for ticket outcome")
			}
			if ent.dao.Value != 20 {
				t.Error("admitted state rolled back")
			}
			if entity.CurrentGuardScope() != nil {
				t.Error("Guard scope leaked")
			}
			if !ent.GetMutex().TryLock() {
				t.Fatal("entity remained locked")
			}
			ent.GetMutex().Unlock()
			if engine.completions != nil && len(engine.completions.chains) != 0 {
				t.Error("completion ordering link leaked")
			}
		})
	}
}

func TestPipelinedInlineCompletionReportsCallbackFailure(t *testing.T) {
	for _, mode := range []string{"inline", "queued", "saturated"} {
		t.Run(mode, func(t *testing.T) {
			id, ent := newAsyncPilotEntity(t, 10021, 10)
			getter := newMockGetter()
			getter.Add(ent)
			committer := newPipelinedTestCommitter(true)
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			if mode != "inline" {
				engine.completions = newCompletionPump(1, 1)
				if mode == "queued" {
					engine.completions.start()
				} else {
					engine.completions.queue <- pipelinedCompletion{}
				}
			}
			name := NewHandlerName("completion_callback_" + mode)
			callbacks := 0
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				AfterCommit(func() { panic("callback failed") })
				AfterCommit(func() { callbacks++ })
				return "committed", MarkPersist(ent.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
			msg := &Msg{Type: MsgTypeSingle, Name: name.String(), Tid: id, RetChan: make(chan any, 2)}
			NestDispatch(engine, msg)
			if mode == "queued" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := engine.completions.stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-msg.RetChan:
				if err, ok := result.(error); !ok || !errors.Is(err, ErrAfterCommitFailed) {
					t.Errorf("reply=%v, want ErrAfterCommitFailed", result)
				}
			default:
				t.Error("missing reply")
			}
			if callbacks != 1 || committer.released.IsZero() {
				t.Error("failure skipped later callbacks or release notification")
			}
		})
	}
}
