package nats

import (
	"context"
	"fmt"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"github.com/tjbdwanghaibo/cube-core/worker"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	gonats "github.com/nats-io/nats.go"
)

// rpcClient implements fnats.IRpc using the underlying natsClient.
type rpcClient struct {
	client *natsClient
	policy fnats.RetryPolicy

	// async RPC state
	pending   sync.Map // sessionId → *pendingCall
	sessionId atomic.Int64
	stopped   atomic.Bool
	pool      *worker.Pool[*rpcTask]
}

type pendingCall struct {
	cb    fnats.RpcCallback
	timer *time.Timer
	sub   *gonats.Subscription
}

// rpcTask carries an async callback execution for the worker pool.
type rpcTask struct {
	cb   fnats.RpcCallback
	resp []byte
	err  error
}

func (t *rpcTask) OnRelease() {}

func newRpcClient(client *natsClient, policy fnats.RetryPolicy, cbWorkerNum int) *rpcClient {
	if cbWorkerNum <= 0 {
		cbWorkerNum = 4
	}
	rc := &rpcClient{
		client: client,
		policy: policy,
	}
	rc.pool = worker.NewPool[*rpcTask](worker.PoolConfig{
		Name:      "rpc_cb",
		WorkerNum: cbWorkerNum,
		QueueCap:  256,
	}, func(task *rpcTask) {
		task.cb(task.resp, task.err)
	})
	rc.pool.Start()
	return rc
}

func (r *rpcClient) Call(ctx context.Context, subject string, req []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < r.policy.MaxAttempts; attempt++ {
		if attempt > 0 {
			wait := r.nextInterval(attempt - 1)
			select {
			case <-ctx.Done():
				return nil, fnats.ErrCancelled
			case <-time.After(wait):
			}
		}

		// Per-attempt timeout: use context deadline or default 5s
		timeout := 5 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, fnats.ErrCancelled
			}
			if remaining < timeout {
				timeout = remaining
			}
		}

		resp, err := r.client.Request(subject, req, timeout)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Only retry on recoverable errors
		if !r.isRetryable(err) {
			return nil, err
		}
		slog.Debug("rpc: retrying", "subject", subject, "attempt", attempt+1, "err", err)
	}
	return nil, fmt.Errorf("rpc: %s failed after %d attempts: %w", subject, r.policy.MaxAttempts, lastErr)
}

func (r *rpcClient) CallWithTimeout(subject string, req []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Call(ctx, subject, req)
}

func (r *rpcClient) CallAsync(subject string, req []byte, cb fnats.RpcCallback) {
	if r.stopped.Load() {
		cb(nil, fnats.ErrCancelled)
		return
	}

	// Generate unique inbox for this request
	inbox := r.client.natsConn().NewRespInbox()

	// Set up timeout
	timeout := 5 * time.Second
	sid := r.sessionId.Add(1)

	timer := time.AfterFunc(timeout, func() {
		if v, ok := r.pending.LoadAndDelete(sid); ok {
			pc := v.(*pendingCall)
			if pc.sub != nil {
				_ = pc.sub.Unsubscribe()
			}
			r.dispatchCallback(sid, &rpcTask{cb: pc.cb, err: fnats.ErrTimeout})
		}
	})

	// Subscribe to inbox
	sub, err := r.client.natsConn().Subscribe(inbox, func(msg *gonats.Msg) {
		if v, ok := r.pending.LoadAndDelete(sid); ok {
			pc := v.(*pendingCall)
			if pc.timer != nil {
				pc.timer.Stop()
			}
			if pc.sub != nil {
				_ = pc.sub.Unsubscribe()
			}
			r.dispatchCallback(sid, &rpcTask{cb: pc.cb, resp: msg.Data})
		}
	})
	if err != nil {
		timer.Stop()
		cb(nil, fmt.Errorf("rpc: subscribe inbox: %w", err))
		return
	}
	sub.AutoUnsubscribe(1)
	r.pending.Store(sid, &pendingCall{cb: cb, timer: timer, sub: sub})
	if r.stopped.Load() {
		if v, ok := r.pending.LoadAndDelete(sid); ok {
			pc := v.(*pendingCall)
			if pc.timer != nil {
				pc.timer.Stop()
			}
			if pc.sub != nil {
				_ = pc.sub.Unsubscribe()
			}
		}
		cb(nil, fnats.ErrCancelled)
		return
	}

	// Publish request with reply subject
	if err := r.client.natsConn().PublishRequest(subject, inbox, req); err != nil {
		if v, ok := r.pending.LoadAndDelete(sid); ok {
			pc := v.(*pendingCall)
			if pc.timer != nil {
				pc.timer.Stop()
			}
			if pc.sub != nil {
				_ = pc.sub.Unsubscribe()
			}
		}
		cb(nil, fmt.Errorf("rpc: publish: %w", err))
	}
}

func (r *rpcClient) Reply(replySubject string, resp []byte) error {
	return r.client.Publish(replySubject, resp)
}

func (r *rpcClient) Stop() {
	if !r.stopped.CompareAndSwap(false, true) {
		return
	}

	r.pending.Range(func(key, value any) bool {
		r.pending.Delete(key)
		pc, ok := value.(*pendingCall)
		if !ok || pc == nil {
			return true
		}
		if pc.timer != nil {
			pc.timer.Stop()
		}
		if pc.sub != nil {
			_ = pc.sub.Unsubscribe()
		}
		if pc.cb != nil {
			sid, _ := key.(int64)
			r.dispatchCallback(sid, &rpcTask{cb: pc.cb, err: fnats.ErrCancelled})
		}
		return true
	})

	if r.pool != nil {
		r.pool.Stop()
	}
}

func (r *rpcClient) dispatchCallback(key int64, task *rpcTask) {
	if r.pool == nil {
		task.OnRelease()
		return
	}
	r.pool.Dispatch(key, task)
}

func (r *rpcClient) isRetryable(err error) bool {
	return err == fnats.ErrTimeout || err == fnats.ErrNoResponders
}

func (r *rpcClient) nextInterval(attempt int) time.Duration {
	interval := time.Duration(float64(r.policy.BaseInterval) * math.Pow(r.policy.Multiplier, float64(attempt)))
	if interval > r.policy.MaxInterval {
		interval = r.policy.MaxInterval
	}
	// ±25% jitter
	jitter := interval / 4
	if jitter > 0 {
		interval += time.Duration(rand.Int63n(int64(jitter)*2) - int64(jitter))
	}
	return interval
}

var _ fnats.IRpc = (*rpcClient)(nil)
