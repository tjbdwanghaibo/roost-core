package nats

import (
	"errors"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"github.com/tjbdwanghaibo/cube-core/worker"
	"testing"
	"time"
)

func TestRpcClientStopCancelsPendingCalls(t *testing.T) {
	r := &rpcClient{}
	r.pool = worker.NewPool[*rpcTask](worker.PoolConfig{
		Name:      "rpc_test",
		WorkerNum: 1,
		QueueCap:  8,
	}, func(task *rpcTask) {
		task.cb(task.resp, task.err)
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
