package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

type lifecycleRemoteBackend struct {
	*atomicTestBackend
	initialize func(context.Context) error
}

func (b *lifecycleRemoteBackend) EnsureRemoteStorage(ctx context.Context) error {
	return b.initialize(ctx)
}

type lifecycleRemoteBus struct {
	active atomic.Int32
	calls  atomic.Int32
}

func (*lifecycleRemoteBus) Publish(*fsyncbus.SyncMsg) error { return nil }
func (b *lifecycleRemoteBus) Subscribe(string, fsyncbus.Handler) (func(), error) {
	b.calls.Add(1)
	b.active.Add(1)
	return sync.OnceFunc(func() { b.active.Add(-1) }), nil
}
func newLifecycleAssembly(t *testing.T, init func(context.Context) error) (*Assembly, *lifecycleRemoteBus) {
	t.Helper()
	b := &lifecycleRemoteBackend{atomicTestBackend: &atomicTestBackend{newRemoteTestLoader()}, initialize: init}
	a, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: b}, DefaultConfig(), 7, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return a, &lifecycleRemoteBus{}
}
func safeAssemblyStart(a *Assembly, ctx context.Context, b *lifecycleRemoteBus) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Start panic: %v", p)
		}
	}()
	return a.Start(ctx, b)
}
func TestRemoteAssemblyDuplicateStart(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	for range 2 {
		if err := safeAssemblyStart(a, context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	if b.active.Load() != 2 || b.calls.Load() != 2 {
		t.Fatalf("subscriptions active=%d calls=%d", b.active.Load(), b.calls.Load())
	}
}
func TestRemoteAssemblyRetryAfterStorageFailure(t *testing.T) {
	var calls atomic.Int32
	failure := errors.New("storage unavailable")
	a, b := newLifecycleAssembly(t, func(context.Context) error {
		if calls.Add(1) == 1 {
			return failure
		}
		return nil
	})
	if err := safeAssemblyStart(a, context.Background(), b); !errors.Is(err, failure) {
		t.Fatalf("first Start: %v", err)
	}
	if b.active.Load() != 0 {
		t.Fatal("failed Start leaked subscriptions")
	}
	if err := safeAssemblyStart(a, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != 2 {
		t.Fatal("retry did not restore subscriptions")
	}
}
func TestRemoteAssemblyStopBeforeStartIsTerminal(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := safeAssemblyStart(a, context.Background(), b); err == nil {
		t.Fatal("Start succeeded with a stopped finalizer")
	}
	if b.active.Load() != 0 {
		t.Fatal("stopped assembly subscribed")
	}
}
func TestRemoteAssemblyCanceledStartDoesNotSubscribe(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := safeAssemblyStart(a, ctx, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start=%v", err)
	}
	if b.calls.Load() != 0 {
		t.Fatal("canceled start subscribed")
	}
}

func TestRemoteAssemblyConcurrentLifecycleWaitIsCancelable(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	a, b := newLifecycleAssembly(t, func(context.Context) error { close(entered); <-release; return nil })
	defer releaseOnce()
	result := make(chan error, 1)
	go func() { result <- safeAssemblyStart(a, context.Background(), b) }()
	awaitChan(t, entered, "storage initialization")
	for _, op := range []func(context.Context) error{
		func(ctx context.Context) error { return safeAssemblyStart(a, ctx, b) }, a.Stop,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := op(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued operation=%v", err)
		}
	}
	releaseOnce()
	if err := awaitChan(t, result, "Start completion"); err != nil {
		t.Fatal(err)
	}
	if b.calls.Load() != 2 {
		t.Fatal("concurrent Start rebound subscriptions")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != 0 {
		t.Fatal("Stop left subscriptions")
	}
}

func TestRemoteAssemblyStopTimeoutKeepsReplicationUntilDrained(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	// 模拟已被接纳但尚未完成的 finalizer 工作；Stop 不能提前拆走它使用的发布依赖。
	a.Manager.remote.retryWG.Add(1)
	finish := sync.OnceFunc(a.Manager.remote.retryWG.Done)
	defer finish()
	if err := safeAssemblyStart(a, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop=%v", err)
	}
	if b.active.Load() != 2 {
		t.Fatal("timed-out Stop discarded replication")
	}
	if err := safeAssemblyStart(a, context.Background(), b); !errors.Is(err, ErrAssemblyStopped) {
		t.Fatalf("restart=%v", err)
	}
	finish()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != 0 {
		t.Fatal("retry Stop leaked subscriptions")
	}
}
