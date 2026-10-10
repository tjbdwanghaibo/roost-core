package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

// 234 在本包其他注册中未使用；init 只注册一次，重复运行测试不覆盖全局注册表。
const awaitLongKind entity.EntityKind = 234

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: awaitLongKind, Category: 1, BusinessPool: entity.BusinessPoolLong})
}

func awaitFixture(t *testing.T) (*NestMgr, int64, int64, int64) {
	t.Helper()
	getter := newMockGetter()
	p := mustBuildCastID(t, 70001, 1, awaitLongKind)
	a := mustBuildCastID(t, 70002, 1, nestLocalKind)
	b := mustBuildCastID(t, 70003, 1, nestLocalKind)
	for _, id := range []int64{p, a, b} {
		getter.Add(newMockEntity(id, 1))
	}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithBusinessPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return mgr, p, a, b
}
func awaitRegister(mgr *NestMgr, name string, fn BaseHandler) {
	mgr.MustRegisterHandlerWithMeta(NewHandlerName(name), fn, HandlerMeta{Durability: DurabilityMemory})
}
func awaitStart(t *testing.T, mgr *NestMgr) {
	t.Helper()
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessPoolsShareAllTargetTails(t *testing.T) {
	mgr, p, a, b := awaitFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	awaitRegister(mgr, "hold", func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !fctx.InLongWorker() || !entity.GetEntityGuard().GuardedEntity(es[0]) || !entity.GetEntityGuard().GuardedEntity(es[1]) {
			t.Error("mixed targets must hold both guards on long worker")
		}
		close(entered)
		<-release
		return nil, nil
	})
	awaitRegister(mgr, "probe", func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !fctx.InFastWorker() {
			t.Error("short target on wrong pool")
		}
		return es[0].GUId(), nil
	})
	awaitStart(t, mgr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := mgr.DispatchMulti(ctx, NewHandlerName("hold"), []int64{p, a}, nil); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	blocked, ch := GenSyncMsg(MsgTypeSingle)
	blocked.Name = "probe"
	blocked.Tid = a
	if err := mgr.dispatcher.TrySendMsg(blocked); err != nil {
		t.Fatal(err)
	}
	if got, err := mgr.Request(ctx, NewHandlerName("probe"), b, nil); err != nil || got != b {
		t.Fatalf("unrelated short message blocked: %v %v", got, err)
	}
	select {
	case <-ch:
		t.Fatal("shared target passed unfinished predecessor")
	default:
	}
	if mgr.Stats().Queue.Fast.BlockedOnPredecessor != 1 {
		t.Fatal("dependency was not retained in queue")
	}
	close(release)
	if got := stagedWait(t, ch); got != a {
		t.Fatal(got)
	}
}

func TestAwaitReleasesTailAndRepliesOnlyAfterResume(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "short", true: "long"}[long], func(t *testing.T) {
			mgr, p, a, _ := awaitFixture(t)
			id := a
			if long {
				id = p
			}
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var observed atomic.Bool
			awaitRegister(mgr, "await", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				return nil, Await(func(context.Context) (int, error) {
					if !fctx.InIOWorker() || entity.CurrentGuardScope() != nil {
						t.Error("work must run without Guard in IO")
					}
					if _, err := CastOne[*mockEntity](id); !errors.Is(err, fctx.ErrGuardInIOWorker) {
						t.Errorf("IO cast=%v", err)
					}
					close(entered)
					<-release
					return 42, nil
				}, func(e *mockEntity, value int, err error) error {
					if err != nil {
						return err
					}
					if value != 42 || e.GUId() != id || !entity.GetEntityGuard().GuardedEntity(e) || fctx.InLongWorker() != long {
						t.Error("invalid resume ownership or result")
					}
					observed.Store(true)
					return nil
				})
			})
			awaitRegister(mgr, "probe", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) { return "progress", nil })
			awaitStart(t, mgr)
			msg, ch := GenSyncMsg(MsgTypeSingle)
			msg.Name = "await"
			msg.Tid = id
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			stagedSignal(t, entered)
			select {
			case r := <-ch:
				t.Fatalf("premature reply %v", r)
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if got, err := mgr.Request(ctx, NewHandlerName("probe"), id, nil); err != nil || got != "progress" {
				t.Fatalf("tail/guard not released: %v %v", got, err)
			}
			close(release)
			if got := stagedWait(t, ch); got != nil {
				t.Fatal(got)
			}
			if !observed.Load() {
				t.Fatal("reply before resume")
			}
		})
	}
}

func TestAwaitDynamicTargetsAndWaitGuards(t *testing.T) {
	mgr, p, a, b := awaitFixture(t)
	awaitRegister(mgr, "dynamic", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if _, err := CastOne[*mockEntity](b); !errors.Is(err, ErrCastUndeclaredTarget) {
			t.Errorf("undeclared cast=%v", err)
		}
		if err := mgr.RunLocal(context.Background(), func() { t.Error("must not run") }); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
			t.Error(err)
		}
		return nil, Await(func(context.Context) (int64, error) { return p, nil }, func(first, second *mockEntity, result int64, err error) error {
			if err != nil {
				return err
			}
			if first.GUId() != a || second.GUId() != p || result != p || !fctx.InLongWorker() {
				t.Error("dynamic targets lost order/long routing")
			}
			if err := mgr.RunLocal(context.Background(), func() { t.Error("long pool must not wait") }); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
				t.Error(err)
			}
			if _, err := mgr.Request(context.Background(), NewHandlerName("dynamic"), a, nil); !errors.Is(err, ErrSyncInHandler) {
				t.Error(err)
			}
			return nil
		}, WithResumeTargets(func(result int64) []int64 { return []int64{a, result} }))
	})
	awaitStart(t, mgr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := mgr.Request(ctx, NewHandlerName("dynamic"), a, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitInvalidSignatureDoesNotStartWork(t *testing.T) {
	mgr, _, a, _ := awaitFixture(t)
	var ran atomic.Bool
	awaitRegister(mgr, "invalid", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, Await(func(context.Context) (int, error) { ran.Store(true); return 1, nil }, func(*mockEntity, string, error) error { return nil })
	})
	awaitStart(t, mgr)
	if _, err := mgr.Request(context.Background(), NewHandlerName("invalid"), a, nil); !errors.Is(err, ErrAwaitSignature) {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("work ran for invalid resume")
	}
}

func TestAwaitShutdownWaitsForIOAndReportsResumeRejection(t *testing.T) {
	mgr, _, id, _ := awaitFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	awaitRegister(mgr, "stop", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, Await(func(context.Context) (int, error) { close(entered); <-release; return 1, nil }, func(*mockEntity, int, error) error { t.Error("resume after stop"); return nil })
	})
	awaitStart(t, mgr)
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "stop"
	msg.Tid = id
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := mgr.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must retain IO until true completion: %v", err)
	}
	close(release)
	got := stagedWait(t, ch)
	if err, ok := got.(error); !ok || !errors.Is(err, ErrNestStopped) {
		t.Fatalf("resume rejection reply=%v", got)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitErrorAndPanicReachDefaultResume(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panics], func(t *testing.T) {
			mgr, _, id, _ := awaitFixture(t)
			cause := errors.New("rank unavailable")
			awaitRegister(mgr, "error", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				return nil, Await(func(context.Context) (int, error) {
					if panics {
						panic(cause)
					}
					return 0, cause
				}, func(_ *mockEntity, _ int, err error) error {
					if !errors.Is(err, cause) {
						t.Errorf("lost work error %v", err)
					}
					return err
				})
			})
			awaitStart(t, mgr)
			if _, err := mgr.Request(context.Background(), NewHandlerName("error"), id, nil); !errors.Is(err, cause) {
				t.Fatal(err)
			}
		})
	}
}

func TestAwaitReservationIsBoundedAndCanceledOnHandlerFailure(t *testing.T) {
	mgr, _, id, _ := awaitFixture(t)
	var ran atomic.Bool
	cause := errors.New("handler failed after scheduling")
	awaitRegister(mgr, "abort", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if err := Await(func(context.Context) (int, error) { ran.Store(true); return 0, nil }, func(*mockEntity, int, error) error { return nil }); err != nil {
			return nil, err
		}
		return nil, cause
	})
	awaitStart(t, mgr)
	if _, err := mgr.Request(context.Background(), NewHandlerName("abort"), id, nil); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("work started for failed segment")
	}
	q := mgr.dispatcher.queue
	for range 9 {
		if err := q.reserveAwait(); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.reserveAwait(); err == nil {
		t.Fatal("unbounded Await admission")
	}
	for range 9 {
		q.cancelAwait()
	}
}

// 两种生产者必须共用同一个预算，已经运行的 I/O 同样消耗额度。
func TestAwaitReservationsCompeteWithRunningAndOrdinaryIO(t *testing.T) {
	q := newDispatchQueue("reserve", WorkerPoolConfig{1, 1}, WorkerPoolConfig{1, 1}, nil, nil)
	q.started = true // 不启动消费，确定性控制准入与运行状态。
	if err := q.reserveAwait(); err != nil {
		t.Fatal(err)
	}
	msg := queueMessage(1)
	defer msg.OnRelease()
	if err := q.admit(msg, true); err != nil {
		t.Fatal(err)
	}
	extra := queueMessage(2)
	defer extra.OnRelease()
	if err := q.admit(extra, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("ordinary I/O stole reservation: %v", err)
	}
	if err := q.reserveAwait(); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("Await overbooked queued I/O: %v", err)
	}
	q.cancelAwait()
	q.take(dispatchSlowLane)
	if err := q.reserveAwait(); err != nil {
		t.Fatal(err)
	}
	if err := q.reserveAwait(); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("Await ignored running I/O: %v", err)
	}
	q.cancelAwait()
}

func TestAwaitCanChainAndCancelWithoutResuming(t *testing.T) {
	mgr, _, id, _ := awaitFixture(t)
	var applied atomic.Int32
	awaitRegister(mgr, "chain", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, Await(func(context.Context) (int, error) { return 4, nil }, func(_ *mockEntity, value int, err error) error {
			if err != nil {
				return err
			}
			return Await(func(context.Context) (int, error) { return value + 1, nil }, func(e *mockEntity, value int, err error) error {
				if err != nil {
					return err
				}
				if !entity.GetEntityGuard().GuardedEntity(e) {
					t.Error("chained resume lost Guard")
				}
				applied.Store(int32(value))
				return nil
			})
		})
	})
	entered := make(chan struct{})
	awaitRegister(mgr, "cancel", func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, Await(func(ctx context.Context) (int, error) { close(entered); <-ctx.Done(); return 9, ctx.Err() }, func(*mockEntity, int, error) error { applied.Store(9); return nil })
	})
	awaitStart(t, mgr)
	if _, err := mgr.Request(context.Background(), NewHandlerName("chain"), id, nil); err != nil || applied.Load() != 5 {
		t.Fatalf("chain result %d %v", applied.Load(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := mgr.Request(ctx, NewHandlerName("cancel"), id, nil); result <- err }()
	stagedSignal(t, entered)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Request did not finish")
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applied.Load() != 5 {
		t.Fatal("canceled work resumed")
	}
}
