package nats

import (
	"context"
	"fmt"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"github.com/tjbdwanghaibo/cube-core/obs"
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
	cb        fnats.RpcCallback
	startedAt time.Time

	mu       sync.Mutex
	timer    *time.Timer
	sub      *gonats.Subscription
	finished bool
}

func (p *pendingCall) setTimer(timer *time.Timer) {
	if p == nil || timer == nil {
		return
	}
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		timer.Stop()
		return
	}
	p.timer = timer
	p.mu.Unlock()
}

func (p *pendingCall) closeResources() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.finished = true
	timer := p.timer
	p.timer = nil
	sub := p.sub
	p.sub = nil
	p.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if sub != nil {
		_ = sub.Unsubscribe()
	}
}

// rpcTask carries an async callback execution for the worker pool.
type rpcTask struct {
	cb   fnats.RpcCallback
	resp []byte
	err  error
	once sync.Once
}

func (t *rpcTask) complete() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		if t.cb != nil {
			t.cb(t.resp, t.err)
		}
	})
}

// OnRelease is also the admission-failure fallback. worker.Pool.Dispatch
// releases a task when the callback queue is closed or full; completing here
// guarantees that an accepted RPC result never loses its terminal callback.
// The once guard makes the normal handler + release path exactly-once.
func (t *rpcTask) OnRelease() { t.complete() }

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
		task.complete()
	})
	rc.pool.Start()
	obs.SetGauge("nats.rpc.duplicate_completion", nil, 0)
	return rc
}

func (r *rpcClient) Call(ctx context.Context, subject string, req []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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

		// Bound each transport attempt while still allowing caller cancellation
		// to interrupt the in-flight NATS request immediately.
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := r.client.requestWithContext(attemptCtx, subject, req)
		cancel()
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
		if cb != nil {
			cb(nil, fnats.ErrCancelled)
		}
		return
	}

	// Generate unique inbox for this request
	inbox := r.client.natsConn().NewRespInbox()

	sid := r.sessionId.Add(1)

	// Subscribe to inbox
	sub, err := r.client.natsConn().Subscribe(inbox, func(msg *gonats.Msg) {
		r.finishPending(sid, msg.Data, nil)
	})
	if err != nil {
		if cb != nil {
			cb(nil, fmt.Errorf("rpc: subscribe inbox: %w", err))
		}
		return
	}
	sub.AutoUnsubscribe(1)
	pc := &pendingCall{cb: cb, sub: sub, startedAt: time.Now()}
	r.pending.Store(sid, pc)
	obs.IncCounter("nats.rpc.started.total", nil, 1)
	obs.AddGauge("nats.rpc.pending", nil, 1)
	pc.setTimer(time.AfterFunc(5*time.Second, func() {
		r.finishPending(sid, nil, fnats.ErrTimeout)
	}))
	if r.stopped.Load() {
		r.finishPending(sid, nil, fnats.ErrCancelled)
		return
	}

	// Publish request with reply subject
	if err := r.client.natsConn().PublishRequest(subject, inbox, req); err != nil {
		r.finishPending(sid, nil, fmt.Errorf("rpc: publish: %w", err))
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
		if sid, ok := key.(int64); ok {
			r.finishPending(sid, nil, fnats.ErrCancelled)
		}
		return true
	})

	if r.pool != nil {
		r.pool.Stop()
	}
}

// finishPending is the only terminal transition after a call enters pending.
// LoadAndDelete elects exactly one winner among reply, timeout, publish error,
// and Stop; losers perform no callback or resource cleanup a second time.
func (r *rpcClient) finishPending(sid int64, resp []byte, err error) bool {
	value, ok := r.pending.LoadAndDelete(sid)
	if !ok {
		return false
	}
	pc, ok := value.(*pendingCall)
	if !ok || pc == nil {
		return false
	}
	obs.AddGauge("nats.rpc.pending", nil, -1)
	obs.IncCounter("nats.rpc.completed.total", nil, 1)
	if !pc.startedAt.IsZero() {
		obs.ObserveHistogram("nats.rpc.callback.latency", nil, time.Since(pc.startedAt))
	}
	pc.closeResources()
	r.dispatchCallback(sid, &rpcTask{cb: pc.cb, resp: resp, err: err})
	return true
}

func (r *rpcClient) dispatchCallback(key int64, task *rpcTask) {
	if r.pool == nil {
		task.OnRelease()
		return
	}
	if err := r.pool.Dispatch(key, task); err != nil {
		obs.IncCounter("nats.rpc.queue_rejected.total", nil, 1)
		// Dispatch transfers ownership even on rejection; rpcTask.OnRelease
		// completes the callback synchronously, so the terminal result is not
		// lost. The caller only needs to avoid a second completion here.
		return
	}
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
