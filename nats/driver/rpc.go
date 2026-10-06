package driver

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/worker"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	gonats "github.com/nats-io/nats.go"
)

// RPCClient implements fnats.IRpc using the underlying Client.
type RPCClient struct {
	client *Client
	policy fnats.RetryPolicy

	// async RPC state
	pending    sync.Map // sessionId → *pendingCall
	sessionId  atomic.Int64
	stopped    atomic.Bool
	pool       *worker.Pool[*rpcTask]
	stopOnce   sync.Once
	stopDone   chan struct{}
	callbackMu sync.Mutex // freezes pending admission and terminal accounting against Stop
	callbacks  sync.WaitGroup

	// Test seams, nil in production: testBeforeDrain runs on the stop task
	// before it ranges pending; testAfterClaim runs inside finishPending's
	// critical section after the entry is removed and before it is counted.
	testBeforeDrain func()
	testAfterClaim  func()
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
	// arrivals is both the exactly-once guard and the observation of it.
	arrivals atomic.Int32
	done     func()
}

// complete runs the terminal callback exactly once.
//
// Every task reaches here twice on the happy path: worker.Worker.safeHandle
// calls the pool handler and then `defer task.OnRelease()`, which is the
// pool's ownership-release protocol, not an anomaly. A rejected or draining
// admission takes the OnRelease half alone, which is why that half has to be
// able to run the callback by itself. So there is no "duplicate completion"
// event to observe from in here — the second arrival is unconditional — and
// the exactly-once property is asserted directly instead, by counting
// callback invocations (see rpc_test.go). An earlier attempt to publish it as
// a metric was a gauge set to zero at construction and never touched again;
// it has been removed rather than left as an assertion that cannot fail.
func (t *rpcTask) complete() {
	if t == nil {
		return
	}
	if t.arrivals.Add(1) > 1 {
		return
	}
	if t.done != nil {
		defer t.done()
	}
	if t.cb != nil {
		t.cb(t.resp, t.err)
	}
}

// OnRelease is also the admission-failure fallback. worker.Pool.Dispatch
// releases a task when the callback queue is closed or full; completing here
// guarantees that an accepted RPC result never loses its terminal callback.
// The once guard makes the normal handler + release path exactly-once.
func (t *rpcTask) OnRelease() { t.complete() }

func NewRPCClient(client *Client, policy fnats.RetryPolicy, cbWorkerNum int) *RPCClient {
	if cbWorkerNum <= 0 {
		cbWorkerNum = 4
	}
	rc := &RPCClient{
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
	return rc
}

func (r *RPCClient) Call(ctx context.Context, subject string, req []byte) ([]byte, error) {
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

func (r *RPCClient) CallWithTimeout(subject string, req []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Call(ctx, subject, req)
}

// errRPCStopped 是 RPC 客户端停止之后的 CallAsync 结果，停止时仍在途的调用与停止之后发起的调用相同：
// 同时 errors.Is 到 fnats.ErrCancelled（旧结果，按它判断的调用方不变）与 fnats.ErrClosed（关闭之后的
// 调用统一返回已关闭，RR-20261006-10 口径）。RPC 客户端的“已停止”是它自己的 stopped（回调池的生命
// 周期），连接的已关闭判据在 client 上（见 Client 的类型注释）。
var errRPCStopped = fmt.Errorf("%w: %w", fnats.ErrCancelled, fnats.ErrClosed)

// CallAsync 的回调恰好一次：RPC 已停止回 errRPCStopped；连接已关闭回 fnats.ErrClosed（入口检查，或
// 订阅收件箱 / 发布在 Close 之后失败）；在途调用由回复、5s 超时（这时连接已关闭则为 fnats.ErrClosed）
// 或 Stop（errRPCStopped）三者之一终结。
func (r *RPCClient) CallAsync(subject string, req []byte, cb fnats.RpcCallback) {
	if r.stopped.Load() {
		if cb != nil {
			cb(nil, errRPCStopped)
		}
		return
	}
	if err := r.client.admit(); err != nil {
		if cb != nil {
			cb(nil, err)
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
			cb(nil, fmt.Errorf("rpc: subscribe inbox: %w", r.client.wrapError(err)))
		}
		return
	}
	sub.AutoUnsubscribe(1)
	pc := &pendingCall{cb: cb, sub: sub, startedAt: time.Now()}
	r.callbackMu.Lock()
	if r.stopped.Load() {
		r.callbackMu.Unlock()
		pc.closeResources()
		if cb != nil {
			cb(nil, errRPCStopped) // 停止先于登记，这次调用从未在途
		}
		return
	}
	r.pending.Store(sid, pc)
	metrics.IncCounter("nats.rpc.started.total", nil, 1)
	metrics.AddGauge("nats.rpc.pending", nil, 1)
	r.callbackMu.Unlock()
	pc.setTimer(time.AfterFunc(5*time.Second, func() { r.expirePending(sid) }))
	if r.stopped.Load() {
		r.finishPending(sid, nil, errRPCStopped)
		return
	}

	// Publish request with reply subject
	if err := r.client.natsConn().PublishRequest(subject, inbox, req); err != nil {
		r.finishPending(sid, nil, fmt.Errorf("rpc: publish: %w", r.client.wrapError(err)))
	}
}

// expirePending 是在途调用的 5s 超时：连接这时已关闭（只关了连接、没停 RPC，收件箱随连接关闭，回复
// 不会再来）按 fnats.ErrClosed 终结，否则 fnats.ErrTimeout。
func (r *RPCClient) expirePending(sid int64) {
	err := fnats.ErrTimeout
	if r.client.closed() {
		err = fnats.ErrClosed
	}
	r.finishPending(sid, nil, err)
}

func (r *RPCClient) Reply(replySubject string, resp []byte) error {
	return r.client.Publish(replySubject, resp)
}

// Stop requests the stop and, when it is the first request, waits without a
// deadline for the drain. Once a stop has been requested it returns
// immediately, as it did before NC-09: RR-20261004-07（复审 S2）—— callback
// 里的 Stop() 若去等唯一停止任务，而停止任务正在等这个 callback 退出，就会
// 永久互等。需要继续等待同一次排空的调用方用 StopWithContext。
func (r *RPCClient) Stop() {
	if r == nil || r.stopped.Load() {
		return
	}
	_ = r.StopWithContext(context.Background())
}

// StopWithContext closes admission once and bounds the caller's wait.
// RR-20261004-NC-09：取消等待不能丢弃 callback；同一 client 只有一个
// 停止任务负责 pending 的同步 fallback 与 pool 排空，后续调用继续等它。
// Callback 内再次停止须用可取消的 ctx，不能同步等待自身退出。
func (r *RPCClient) StopWithContext(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.stopOnce.Do(func() {
		r.callbackMu.Lock()
		r.stopped.Store(true)
		r.callbackMu.Unlock()
		r.stopDone = make(chan struct{})
		go r.drainCallbacks()
	})
	select {
	case <-r.stopDone:
		return nil
	default:
	}
	select {
	case <-r.stopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *RPCClient) drainCallbacks() {
	defer close(r.stopDone)
	if r.testBeforeDrain != nil {
		r.testBeforeDrain()
	}
	r.pending.Range(func(key, _ any) bool {
		if sid, ok := key.(int64); ok {
			// 队列拒绝时 fallback 在这里同步执行。隔离其 panic，后续
			// pending 仍须取消；阻塞则由这个唯一停止任务保留责任。
			goroutine.SafeFunc(func() { r.finishPending(sid, nil, errRPCStopped) })
		}
		return true
	})
	// A reply/timeout may already have removed its pending entry, so Range
	// skipped it. finishPending removes the entry and only then counts it in
	// callbacks, both inside callbackMu; a removal Range missed may therefore
	// still be uncounted here. Taking callbackMu once waits out any such
	// critical section, so its Add happens before Wait (RR-20261004-07, 复审
	// S1). stopped is already set, so no new pending can appear afterwards and
	// one barrier is enough. A claimed callback still running its synchronous
	// queue-rejection fallback is counted and stays part of the drain.
	r.callbackMu.Lock()
	r.callbackMu.Unlock() //nolint:staticcheck // empty critical section is the barrier
	r.callbacks.Wait()
	if r.pool != nil {
		_ = r.pool.StopWithContext(context.Background())
	}
}

// finishPending is the only terminal transition after a call enters pending.
// LoadAndDelete elects exactly one winner among reply, timeout, publish error,
// and Stop; losers perform no callback or resource cleanup a second time.
func (r *RPCClient) finishPending(sid int64, resp []byte, err error) bool {
	r.callbackMu.Lock()
	value, ok := r.pending.LoadAndDelete(sid)
	if !ok {
		r.callbackMu.Unlock()
		return false
	}
	pc, ok := value.(*pendingCall)
	if !ok || pc == nil {
		r.callbackMu.Unlock()
		return false
	}
	if r.testAfterClaim != nil {
		r.testAfterClaim()
	}
	r.callbacks.Add(1)
	r.callbackMu.Unlock()
	metrics.AddGauge("nats.rpc.pending", nil, -1)
	metrics.IncCounter("nats.rpc.completed.total", nil, 1)
	if !pc.startedAt.IsZero() {
		metrics.ObserveHistogram("nats.rpc.callback.latency", nil, time.Since(pc.startedAt))
	}
	pc.closeResources()
	r.dispatchCallback(sid, &rpcTask{cb: pc.cb, resp: resp, err: err, done: r.callbacks.Done})
	return true
}

func (r *RPCClient) dispatchCallback(key int64, task *rpcTask) {
	if r.pool == nil {
		task.OnRelease()
		return
	}
	if err := r.pool.Dispatch(key, task); err != nil {
		metrics.IncCounter("nats.rpc.queue_rejected.total", nil, 1)
		// Dispatch transfers ownership even on rejection; rpcTask.OnRelease
		// completes the callback synchronously, so the terminal result is not
		// lost. The caller only needs to avoid a second completion here.
		return
	}
}

func (r *RPCClient) isRetryable(err error) bool {
	return err == fnats.ErrTimeout || err == fnats.ErrNoResponders
}

func (r *RPCClient) nextInterval(attempt int) time.Duration {
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

var _ fnats.IRpc = (*RPCClient)(nil)
