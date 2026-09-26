package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-53：纯本地事务提交成功后发生的释放 / 回调错误同样包 ErrAfterCommitFailed（保留原因链）；未提交路径不带。

type localHookFixture struct {
	id      int64
	e       *rollbackTestEntity
	owner   *entity.EntityManager
	hookErr error
	armed   bool
}

func newLocalHookFixture(t *testing.T, unique int64) *localHookFixture {
	t.Helper()
	id, e := newAsyncPilotEntity(t, unique, 10)
	f := &localHookFixture{id: id, e: e, owner: entity.NewEntityManager(), hookErr: errors.New("release hook failed")}
	if err := f.owner.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
		if f.armed {
			f.armed = false
			panic(f.hookErr)
		}
	}))
	return f
}

// REPRO-2026-09-26-05 §2b：纯本地 strict 事务持久提交后 release hook panic。
func TestLocalReleaseHookPanicAfterStrictCommitCarriesSentinel(t *testing.T) {
	f := newLocalHookFixture(t, 9980)
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("rr53_local_release_hook")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		old := f.e.dao.Value
		RecordUndo(f.e.dao, 1, func() error { f.e.dao.Value = old; return nil })
		f.e.dao.Value++
		f.armed = true
		return "ok", MarkPersist(f.e.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	_, err := mgr.Request(context.Background(), name, f.id, nil)
	committed := len(committer.record.Mutations) > 0
	if !committed || f.e.dao.Value != 11 {
		t.Fatalf("fixture: committed=%v value=%d, want a durable commit to 11", committed, f.e.dao.Value)
	}
	if !errors.Is(err, ErrAfterCommitFailed) || !errors.Is(err, f.hookErr) {
		t.Fatalf("committed=%v value=%d reply=%v afterCommit=%v hook=%v: a committed local transaction must report ErrAfterCommitFailed with the hook cause",
			committed, f.e.dao.Value, err, errors.Is(err, ErrAfterCommitFailed), errors.Is(err, f.hookErr))
	}
}

// memory 快路径 handler 成功即已提交（内存）；随后的 release hook 失败同样带哨兵。
func TestMemoryHandlerReleaseHookPanicCarriesSentinel(t *testing.T) {
	f := newLocalHookFixture(t, 9983)
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("rr53_memory_release_hook")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		f.e.dao.Value++
		f.armed = true
		return "ok", nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	_, err := mgr.Request(context.Background(), name, f.id, nil)
	if !errors.Is(err, ErrAfterCommitFailed) || !errors.Is(err, f.hookErr) {
		t.Fatalf("reply=%v afterCommit=%v hook=%v", err, errors.Is(err, ErrAfterCommitFailed), errors.Is(err, f.hookErr))
	}
}

// 未提交路径：handler 报错、提交被明确拒绝后 release hook panic，回复不得带哨兵。
func TestUncommittedLocalReleaseHookPanicHasNoSentinel(t *testing.T) {
	for i, tc := range []struct {
		name       string
		handlerErr error
		commitErr  error
	}{
		{"handler_failed", errors.New("business failed"), nil},
		{"commit_rejected", nil, errors.New("wal rejected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalHookFixture(t, 9986+int64(i))
			committer := &recordingCommitter{err: tc.commitErr}
			mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName("rr53_uncommitted_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				old := f.e.dao.Value
				RecordUndo(f.e.dao, 1, func() error { f.e.dao.Value = old; return nil })
				f.e.dao.Value++
				f.armed = true
				if tc.handlerErr != nil {
					return nil, tc.handlerErr
				}
				return "ok", MarkPersist(f.e.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			_, err := mgr.Request(context.Background(), name, f.id, nil)
			if err == nil || errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v: an uncommitted transaction must fail without ErrAfterCommitFailed", err)
			}
			if f.e.dao.Value != 10 {
				t.Fatalf("value=%d, want rollback to 10", f.e.dao.Value)
			}
		})
	}
}

// pipelined：WAL 持久后 release hook panic（inline 在 dispatch 回复，queued / saturated 由完成池回复）带哨兵；
// ticket 结果未知不是“已提交”，不带。
func TestPipelinedReleasePanicAfterDurableCarriesSentinel(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		indeterminate bool
	}{
		{"inline", false}, {"queued", false}, {"saturated", false},
		{"inline", true}, {"queued", true}, {"saturated", true},
	} {
		label := tc.mode
		if tc.indeterminate {
			label += "_indeterminate"
		}
		t.Run(label, func(t *testing.T) {
			id, ent := newAsyncPilotEntity(t, 10040, 10)
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
			if tc.mode != "inline" {
				engine.completions = newCompletionPump(1, 1)
				engine.completions.fence = engine.Fence
				if tc.mode == "queued" {
					engine.completions.start()
				} else {
					engine.completions.queue <- pipelinedCompletion{}
				}
			}
			name := NewHandlerName("rr53_pipelined_release_" + label)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				old := ent.dao.Value
				RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil })
				ent.dao.Value = 20
				return "committed", MarkPersist(ent.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
			msg := &Msg{Type: MsgTypeSingle, Name: name.String(), Tid: id, RetChan: make(chan any, 2)}
			NestDispatch(engine, msg)
			if tc.mode == "queued" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := engine.completions.stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			select {
			case result := <-msg.RetChan:
				err, _ = result.(error)
			default:
				t.Fatal("accepted transaction lost its reply")
			}
			if !errors.Is(err, releaseErr) {
				t.Fatalf("reply=%v, want the release failure", err)
			}
			if got := errors.Is(err, ErrAfterCommitFailed); got == tc.indeterminate {
				t.Fatalf("reply=%v: errors.Is(ErrAfterCommitFailed)=%v, want %v (indeterminate=%v)", err, got, !tc.indeterminate, tc.indeterminate)
			}
		})
	}
}
