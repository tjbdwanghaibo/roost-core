package nats

import (
	"errors"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"github.com/tjbdwanghaibo/cube-core/obs"
	"github.com/tjbdwanghaibo/cube-core/worker"
	"sync/atomic"
	"testing"
	"time"
)

func metricValue(t *testing.T, registry *obs.Registry, name string) int64 {
	t.Helper()
	for _, metric := range registry.Snapshot() {
		if metric.Name == name {
			return metric.Value
		}
	}
	return 0
}

func TestRpcClientStopCancelsPendingCalls(t *testing.T) {
	r := &rpcClient{}
	r.pool = worker.NewPool[*rpcTask](worker.PoolConfig{
		Name:      "rpc_test",
		WorkerNum: 1,
		QueueCap:  8,
	}, func(task *rpcTask) {
		task.complete()
	})
	r.pool.Start()

	done := make(chan error, 1)
	timer := time.AfterFunc(time.Hour, func() {})
	r.pending.Store(int64(7), &pendingCall{
		cb: func(resp []byte, err error) {
			done <- err
		},
		timer: timer,
	})

	r.Stop()
	r.Stop()

	select {
	case err := <-done:
		if !errors.Is(err, fnats.ErrCancelled) {
			t.Fatalf("callback err = %v, want %v", err, fnats.ErrCancelled)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled callback")
	}
	if _, ok := r.pending.Load(int64(7)); ok {
		t.Fatal("expected pending call to be cleared")
	}
	if timer.Stop() {
		t.Fatal("expected timer to be stopped by Stop")
	}
}

func TestRpcClientDispatchCallbackCompletesWhenPoolRejects(t *testing.T) {
	previousRegistry := obs.DefaultRegistry()
	registry := obs.NewRegistry()
	obs.SetDefaultRegistry(registry)
	t.Cleanup(func() { obs.SetDefaultRegistry(previousRegistry) })

	// An unstarted pool rejects admission. Regression: Dispatch used to call
	// OnRelease on this path, but rpcTask.OnRelease was empty, silently losing
	// the RPC terminal callback.
	r := &rpcClient{
		pool: worker.NewPool[*rpcTask](worker.PoolConfig{
			Name:      "rpc_rejected",
			WorkerNum: 1,
			QueueCap:  1,
		}, func(task *rpcTask) {
			task.complete()
		}),
	}

	done := make(chan error, 2)
	task := &rpcTask{cb: func(_ []byte, err error) { done <- err }, err: fnats.ErrTimeout}
	r.dispatchCallback(1, task)
	// A later defensive release must not duplicate the callback.
	task.OnRelease()

	select {
	case err := <-done:
		if !errors.Is(err, fnats.ErrTimeout) {
			t.Fatalf("callback err = %v, want %v", err, fnats.ErrTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rejected callback")
	}
	select {
	case err := <-done:
		t.Fatalf("duplicate callback: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if rejected := metricValue(t, registry, "nats.rpc.queue_rejected.total"); rejected != 1 {
		t.Fatalf("queue rejected metric = %d, want 1", rejected)
	}
}

func TestRpcClientCallAsyncAfterStopCancelsImmediately(t *testing.T) {
	r := &rpcClient{}
	r.stopped.Store(true)

	done := make(chan error, 1)
	r.CallAsync("subject", nil, func(resp []byte, err error) {
		done <- err
	})

	select {
	case err := <-done:
		if !errors.Is(err, fnats.ErrCancelled) {
			t.Fatalf("callback err = %v, want %v", err, fnats.ErrCancelled)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled callback")
	}
}

func TestRpcClientPendingHasSingleTerminalWinner(t *testing.T) {
	r := &rpcClient{}
	done := make(chan error, 2)
	r.pending.Store(int64(11), &pendingCall{cb: func(_ []byte, err error) { done <- err }})

	if !r.finishPending(11, nil, fnats.ErrTimeout) {
		t.Fatal("first terminal transition did not claim pending call")
	}
	if r.finishPending(11, nil, fnats.ErrCancelled) {
		t.Fatal("second terminal transition claimed completed call")
	}
	r.Stop()

	select {
	case err := <-done:
		if !errors.Is(err, fnats.ErrTimeout) {
			t.Fatalf("callback err = %v, want timeout winner", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal callback")
	}
	select {
	case err := <-done:
		t.Fatalf("duplicate terminal callback: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRpcClientCancelsOneHundredThousandPendingExactlyOnce(t *testing.T) {
	const calls = 100_000
	previousRegistry := obs.DefaultRegistry()
	registry := obs.NewRegistry()
	obs.SetDefaultRegistry(registry)
	t.Cleanup(func() { obs.SetDefaultRegistry(previousRegistry) })

	r := &rpcClient{}
	r.pool = worker.NewPool[*rpcTask](worker.PoolConfig{
		Name:      "rpc_100k_cancel",
		WorkerNum: 8,
		QueueCap:  256,
	}, func(task *rpcTask) {
		task.complete()
	})
	r.pool.Start()

	counts := make([]atomic.Uint32, calls)
	for i := range counts {
		index := i
		r.pending.Store(int64(i+1), &pendingCall{
			startedAt: time.Now(),
			cb: func(_ []byte, err error) {
				if !errors.Is(err, fnats.ErrCancelled) {
					t.Errorf("callback[%d] err = %v, want %v", index, err, fnats.ErrCancelled)
				}
				counts[index].Add(1)
			},
		})
		obs.IncCounter("nats.rpc.started.total", nil, 1)
		obs.AddGauge("nats.rpc.pending", nil, 1)
	}

	r.Stop()
	r.Stop()

	remaining := 0
	r.pending.Range(func(_, _ any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("pending calls after Stop = %d, want 0", remaining)
	}
	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Fatalf("callback[%d] count = %d, want 1", i, got)
		}
	}
	started := metricValue(t, registry, "nats.rpc.started.total")
	completed := metricValue(t, registry, "nats.rpc.completed.total")
	pending := metricValue(t, registry, "nats.rpc.pending")
	if started != calls || completed != calls || pending != 0 || started != completed+pending {
		t.Fatalf("RPC conservation failed: started=%d completed=%d pending=%d", started, completed, pending)
	}
	if duplicate := metricValue(t, registry, "nats.rpc.duplicate_completion"); duplicate != 0 {
		t.Fatalf("duplicate completion = %d, want 0", duplicate)
	}
}
