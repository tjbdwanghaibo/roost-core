package bus

import (
	"context"
	"errors"
	"fmt"
	fctx "github.com/tjbdwanghaibo/cube-core/fctx"
	"github.com/tjbdwanghaibo/cube-core/metrics"
	"github.com/tjbdwanghaibo/cube-core/nats"
	"github.com/tjbdwanghaibo/cube-core/worker"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// IBus is the high-level messaging API for business code.
type IBus interface {
	// --- Send ---

	// Send sends a message to a specific server's module.
	Send(toSid int32, toModule string, msg any) error

	// SendByType sends a message to a specific service type instance.
	SendByType(svcType string, toSid int32, toModule string, msg any) error

	// Broadcast broadcasts to all instances of a service type.
	Broadcast(svcType string, toModule string, msg any) error

	// BroadcastAll broadcasts to all servers.
	BroadcastAll(toModule string, msg any) error

	// --- RPC ---

	// Call performs a synchronous RPC.
	Call(ctx context.Context, svcType string, method string, req any, resp any) error

	// CallTo performs a synchronous RPC against a specific service instance.
	CallTo(ctx context.Context, svcType string, sid int32, method string, req any, resp any) error

	// CallWithTimeout performs a synchronous RPC with timeout.
	CallWithTimeout(svcType string, method string, req any, resp any, timeout time.Duration) error

	// CallAsync performs an asynchronous RPC with callback.
	CallAsync(svcType string, method string, req any, cb func(resp []byte, err error))

	// --- Handler Registration ---

	// Handle registers a handler for a module message.
	Handle(module string, msgName string, handler HandlerFunc) error

	// HandleRpc registers a handler for an RPC method.
	HandleRpc(method string, handler RpcHandlerFunc) error
}

// Config configures the Bus.
type Config struct {
	Sid       int32  // current server id
	SvcType   string // current service type
	Prefix    string // subject prefix
	WorkerNum int    // dispatcher worker count, default: 8
	QueueCap  int    // per-worker queue capacity, default: 1024
	Reliable  ReliableConfig
}

// Bus implements IBus using NATS IClient and IRpc.
type Bus struct {
	cfg     Config
	client  nats.IClient
	rpc     nats.IRpc
	codec   Codec
	subject *nats.SubjectBuilder
	msgSeq  atomic.Uint64

	reliable ReliableStore
	jsRPC    *jetStreamRPC

	// handler registry
	mu          sync.RWMutex
	handlers    map[string]HandlerFunc    // "module:msgName" → handler
	rpcHandlers map[string]RpcHandlerFunc // "method" → handler

	// dispatcher pool for incoming messages
	lifeMu   sync.Mutex
	pool     *worker.Pool[*incomingTask]
	ctx      atomic.Pointer[context.Context] // read lock-free by dispatch paths
	cancel   context.CancelFunc
	started  bool // a successful Start happened; guarded by lifeMu
	stopped  bool // a started Bus was stopped; guarded by lifeMu
	stopping bool
	stopDone chan struct{}
	stopErr  error

	// subscriptions
	subs []nats.ISubscription
}

// New creates a Bus instance. Call Start() to begin receiving messages.
func New(client nats.IClient, rpc nats.IRpc, codec Codec, cfg Config) *Bus {
	if cfg.WorkerNum <= 0 {
		cfg.WorkerNum = 8
	}
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = 1024
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "cube"
	}
	if codec == nil {
		codec = JSONCodec{}
	}
	return &Bus{
		cfg:         cfg,
		client:      client,
		rpc:         rpc,
		codec:       codec,
		subject:     nats.NewSubjectBuilder(cfg.Prefix),
		handlers:    make(map[string]HandlerFunc),
		rpcHandlers: make(map[string]RpcHandlerFunc),
	}
}

// EnableReliable attaches the durable idempotency/DLQ store used for async
// messages. It is intentionally explicit so services can fail startup when the
// configured reliable store is unavailable.
func (b *Bus) EnableReliable(store ReliableStore, cfg ReliableConfig) {
	if b == nil || store == nil {
		return
	}
	cfg = cfg.normalize()
	cfg.Enabled = true
	b.cfg.Reliable = cfg
	b.reliable = store
}

func (b *Bus) DeadLetters(ctx context.Context, query DeadLetterQuery) ([]DeadLetterEntry, error) {
	store, ok := b.deadLetterStore()
	if !ok {
		return nil, fmt.Errorf("bus: reliable store does not support dead letter queries")
	}
	return store.ListDeadLetters(ctx, query)
}

func (b *Bus) RequeueDeadLetters(ctx context.Context, query DeadLetterQuery) (int64, error) {
	store, ok := b.deadLetterStore()
	if !ok {
		return 0, fmt.Errorf("bus: reliable store does not support dead letter requeue")
	}
	if b == nil || b.client == nil {
		return 0, fmt.Errorf("bus: nats client is nil")
	}
	entries, err := store.ListDeadLetters(ctx, query)
	if err != nil {
		return 0, err
	}
	deleter, canDeleteEntry := store.(ReliableDeadLetterEntryDeleter)
	var requeued int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return requeued, err
		}
		msg := entry.toNatsMsg(entry.requeueMsgID())
		raw, err := b.codec.Marshal(msg)
		if err != nil {
			return requeued, fmt.Errorf("bus: marshal dead letter %s: %w", entry.MsgID, err)
		}
		if err := b.client.Publish(b.deadLetterSubject(entry), raw); err != nil {
			return requeued, fmt.Errorf("bus: requeue dead letter %s: %w", entry.MsgID, err)
		}
		requeued++
		if canDeleteEntry {
			if _, err := deleter.DeleteDeadLetters(ctx, query, []DeadLetterEntry{entry}); err != nil {
				return requeued, fmt.Errorf("bus: delete requeued dead letter %s: %w", entry.MsgID, err)
			}
		}
	}
	if !canDeleteEntry {
		normalized := query.normalize()
		if !normalized.isWholeBucket() {
			return requeued, fmt.Errorf("bus: reliable store does not support partial dead letter deletion")
		}
		if _, err := store.PurgeDeadLetters(ctx, normalized); err != nil {
			return requeued, err
		}
	}
	metrics.IncCounter("bus_dead_letter_requeue_total", metrics.Labels{
		"module": query.Module,
		"msg":    query.MsgName,
	}, requeued)
	return requeued, nil
}

func (b *Bus) PurgeDeadLetters(ctx context.Context, query DeadLetterQuery) (int64, error) {
	store, ok := b.deadLetterStore()
	if !ok {
		return 0, fmt.Errorf("bus: reliable store does not support dead letter purge")
	}
	n, err := store.PurgeDeadLetters(ctx, query)
	if err == nil {
		metrics.IncCounter("bus_dead_letter_purge_total", metrics.Labels{
			"module": query.Module,
			"msg":    query.MsgName,
		}, n)
	}
	return n, err
}

func (b *Bus) deadLetterStore() (ReliableDeadLetterStore, bool) {
	if b == nil || b.reliable == nil {
		return nil, false
	}
	store, ok := b.reliable.(ReliableDeadLetterStore)
	return store, ok
}

func (b *Bus) deadLetterSubject(entry DeadLetterEntry) string {
	switch nats.BroadcastType(entry.Broadcast) {
	case nats.BroadcastAll:
		return b.subject.All()
	case nats.BroadcastServerType:
		return b.subject.ServiceAll(b.cfg.SvcType)
	default:
		if entry.ToSid != 0 {
			return b.subject.Server(entry.ToSid)
		}
		return b.subject.Server(b.cfg.Sid)
	}
}

// Start subscribes to subjects and starts the dispatcher worker pool.
//
// A Bus does not restart: Stop tears down every subscription, including RPC
// subjects registered through HandleRpc, and Start only rebuilds the base
// subjects. Restarting would therefore silently lose RPC subscriptions, so it
// is rejected instead. Create a new Bus if a fresh instance is needed.
// Start builds the base subscriptions and the dispatch pool. A failure part
// way through cleans up, and that cleanup drains the pool — which is why the
// draining happens here, outside the lifecycle lock: a worker's business
// handler may call back into Handle/HandleRpc/Stop, all of which take that
// lock, and waiting for such a handler while holding it deadlocks with no
// deadline. StopWithContext was restructured for exactly this reason; the
// Start-failure path has to follow the same rule.
func (b *Bus) Start() error {
	drain, err := b.startLocked()
	if drain != nil {
		return errors.Join(err, drain())
	}
	return err
}

// startLocked holds the lifecycle lock for the whole start attempt. On
// failure it returns a drain closure the caller must run after the lock is
// released; on success the closure is nil.
func (b *Bus) startLocked() (func() error, error) {
	b.lifeMu.Lock()
	defer b.lifeMu.Unlock()
	if b.pool != nil {
		return nil, nil
	}
	if b.stopped || b.stopping {
		return nil, fmt.Errorf("bus: cannot restart a stopped bus")
	}

	// Start worker pool
	runCtx, cancel := context.WithCancel(context.Background())
	b.ctx.Store(&runCtx)
	b.cancel = cancel
	b.pool = worker.NewPool[*incomingTask](worker.PoolConfig{
		Name:      "bus",
		WorkerNum: b.cfg.WorkerNum,
		QueueCap:  b.cfg.QueueCap,
	}, b.handleTask)
	b.pool.Start()

	// Subscribe to own subjects
	subs := []struct {
		subject string
		queue   string
	}{
		{b.subject.Server(b.cfg.Sid), ""},
		{b.subject.ServiceInstance(b.cfg.SvcType, b.cfg.Sid), ""},
		{b.subject.ServiceAll(b.cfg.SvcType), ""},
		{b.subject.All(), ""},
	}

	for _, s := range subs {
		sub, err := b.client.Subscribe(s.subject, b.onMessage)
		if err != nil {
			drain := b.detachLocked()
			return drain, fmt.Errorf("bus: subscribe %s: %w", s.subject, err)
		}
		b.subs = append(b.subs, sub)
		slog.Info("bus: subscribed", "subject", s.subject)
	}

	b.started = true
	return nil, nil
}

// Stop unsubscribes and stops the worker pool.
func (b *Bus) Stop() {
	_ = b.StopWithContext(context.Background())
}

func (b *Bus) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b.lifeMu.Lock()
	if b.stopDone != nil {
		done := b.stopDone
		b.lifeMu.Unlock()
		select {
		case <-done:
			b.lifeMu.Lock()
			err := b.stopErr
			b.lifeMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.stopping = true
	b.stopDone = make(chan struct{})
	done := b.stopDone
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.ctx.Store(nil)
	if b.started {
		b.started = false
		b.stopped = true
	}
	subs := b.subs
	b.subs = nil
	pool := b.pool
	b.pool = nil
	b.lifeMu.Unlock()

	err := b.stopResources(ctx, subs, pool)
	b.lifeMu.Lock()
	b.stopErr = err
	close(done)
	b.lifeMu.Unlock()
	return err
}

func (b *Bus) stopResources(ctx context.Context, subs []nats.ISubscription, pool *worker.Pool[*incomingTask]) error {
	var err error
	for _, sub := range subs {
		if sub == nil || !sub.IsValid() {
			continue
		}
		if unsubscribeErr := sub.Unsubscribe(); unsubscribeErr != nil {
			slog.Warn("bus: unsubscribe failed", "err", unsubscribeErr)
			err = errors.Join(err, unsubscribeErr)
		}
	}
	if pool != nil {
		err = errors.Join(err, pool.StopWithContext(ctx))
	}
	b.stopJetStreamRPCSubscriptions()
	return err
}

// detachLocked cancels the run context and takes the subscriptions and pool
// out of the Bus, returning the closure that actually tears them down. The
// split matters: unsubscribing and draining the pool must happen with the
// lifecycle lock released (see Start), while the state transition must happen
// under it. Called with lifeMu held.
func (b *Bus) detachLocked() func() error {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.ctx.Store(nil)
	// Only a Bus that completed Start becomes non-restartable; cleanup of a
	// failed Start leaves the Bus eligible for a retry.
	if b.started {
		b.started = false
		b.stopped = true
	}
	subs := b.subs
	b.subs = nil
	pool := b.pool
	b.pool = nil
	return func() error {
		return b.stopResources(context.Background(), subs, pool)
	}
}

// --- IBus: Send ---

func (b *Bus) Send(toSid int32, toModule string, msg any) error {
	data, err := b.encodeMsg(toSid, toModule, msgName(msg), msg, nats.BroadcastNone)
	if err != nil {
		return err
	}
	return b.client.Publish(b.subject.Server(toSid), data)
}

func (b *Bus) SendByType(svcType string, toSid int32, toModule string, msg any) error {
	data, err := b.encodeMsg(toSid, toModule, msgName(msg), msg, nats.BroadcastNone)
	if err != nil {
		return err
	}
	return b.client.Publish(b.subject.ServiceInstance(svcType, toSid), data)
}

func (b *Bus) Broadcast(svcType string, toModule string, msg any) error {
	data, err := b.encodeMsg(0, toModule, msgName(msg), msg, nats.BroadcastServerType)
	if err != nil {
		return err
	}
	return b.client.Publish(b.subject.ServiceAll(svcType), data)
}

func (b *Bus) BroadcastAll(toModule string, msg any) error {
	data, err := b.encodeMsg(0, toModule, msgName(msg), msg, nats.BroadcastAll)
	if err != nil {
		return err
	}
	return b.client.Publish(b.subject.All(), data)
}

// --- IBus: RPC ---

func (b *Bus) Call(ctx context.Context, svcType string, method string, req any, resp any) error {
	return b.callSubject(ctx, b.subject.Rpc(svcType, method), method, 0, req, resp)
}

func (b *Bus) CallTo(ctx context.Context, svcType string, sid int32, method string, req any, resp any) error {
	if sid == 0 {
		return b.Call(ctx, svcType, method, req, resp)
	}
	return b.callSubject(ctx, b.subject.RpcInstance(svcType, sid, method), method, sid, req, resp)
}

func (b *Bus) CallReliable(ctx context.Context, svcType string, method string, req any, resp any) error {
	return b.callReliableSubject(ctx, b.subject.Rpc(svcType, method), method, 0, req, resp)
}

func (b *Bus) CallToReliable(ctx context.Context, svcType string, sid int32, method string, req any, resp any) error {
	if sid == 0 {
		return b.CallReliable(ctx, svcType, method, req, resp)
	}
	return b.callReliableSubject(ctx, b.subject.RpcInstance(svcType, sid, method), method, sid, req, resp)
}

func (b *Bus) callSubject(ctx context.Context, subject string, method string, toSid int32, req any, resp any) error {
	if b == nil || b.rpc == nil {
		return fmt.Errorf("bus: rpc client is nil")
	}
	payload, err := b.codec.Marshal(req)
	if err != nil {
		return fmt.Errorf("bus: marshal req: %w", err)
	}
	now := time.Now()
	requestID := b.nextMsgID()
	wire, err := b.codec.Marshal(&nats.NatsMsg{
		FromSid: b.cfg.Sid, ToSid: toSid, MsgName: method, Payload: payload,
		SessionId: requestID, MsgID: requestID, Attempt: 1, CreatedAt: now.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("bus: marshal rpc request envelope: %w", err)
	}
	data, err := b.rpc.Call(ctx, subject, wire)
	if err != nil {
		return err
	}
	return decodeRPCResponse(b.codec, data, resp)
}

func (b *Bus) callReliableSubject(ctx context.Context, subject string, method string, toSid int32, req any, resp any) error {
	if b == nil || b.jsRPC == nil {
		return fmt.Errorf("bus: jetstream rpc is not enabled")
	}
	payload, err := b.codec.Marshal(req)
	if err != nil {
		return fmt.Errorf("bus: marshal req: %w", err)
	}
	data, err := b.callJetStreamRPC(ctx, subject, method, toSid, payload)
	if err != nil {
		return err
	}
	return decodeRPCResponse(b.codec, data, resp)
}

func (b *Bus) CallWithTimeout(svcType string, method string, req any, resp any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(fctx.BaseContext(), timeout)
	defer cancel()
	return b.Call(ctx, svcType, method, req, resp)
}

func (b *Bus) CallAsync(svcType string, method string, req any, cb func(resp []byte, err error)) {
	if cb == nil {
		return
	}
	if b == nil || b.rpc == nil {
		cb(nil, fmt.Errorf("bus: rpc client is nil"))
		return
	}
	payload, err := b.codec.Marshal(req)
	if err != nil {
		cb(nil, fmt.Errorf("bus: marshal req: %w", err))
		return
	}
	requestID := b.nextMsgID()
	wire, err := b.codec.Marshal(&nats.NatsMsg{
		FromSid: b.cfg.Sid, MsgName: method, Payload: payload,
		SessionId: requestID, MsgID: requestID, Attempt: 1, CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		cb(nil, fmt.Errorf("bus: marshal rpc request envelope: %w", err))
		return
	}
	subject := b.subject.Rpc(svcType, method)
	b.rpc.CallAsync(subject, wire, func(data []byte, callErr error) {
		if callErr != nil {
			cb(nil, callErr)
			return
		}
		resp, decodeErr := decodeRPCResponseBytes(b.codec, data)
		cb(resp, decodeErr)
	})
}

// --- IBus: Handler Registration ---

func (b *Bus) Handle(module string, msgName string, handler HandlerFunc) error {
	if b == nil {
		return fmt.Errorf("bus: nil bus")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if module == "" || msgName == "" {
		return fmt.Errorf("bus: message handler module and name are required")
	}
	if handler == nil {
		return fmt.Errorf("bus: message handler %s:%s is nil", module, msgName)
	}
	key := module + ":" + msgName
	if _, exists := b.handlers[key]; exists {
		return fmt.Errorf("bus: duplicate message handler %s", key)
	}
	b.handlers[key] = handler
	return nil
}

func (b *Bus) HandleRpc(method string, handler RpcHandlerFunc) error {
	if b == nil {
		return fmt.Errorf("bus: nil bus")
	}
	b.lifeMu.Lock()
	unavailable := b.stopping || b.stopped
	b.lifeMu.Unlock()
	if unavailable {
		return fmt.Errorf("bus: cannot register rpc handler while stopping or stopped")
	}
	b.mu.Lock()
	if method == "" {
		b.mu.Unlock()
		return fmt.Errorf("bus: rpc method is empty")
	}
	if handler == nil {
		b.mu.Unlock()
		return fmt.Errorf("bus: rpc handler for %s is nil", method)
	}
	if _, exists := b.rpcHandlers[method]; exists {
		b.mu.Unlock()
		return fmt.Errorf("bus: duplicate rpc handler %s", method)
	}
	b.rpcHandlers[method] = handler
	b.mu.Unlock()

	if b.jetStreamRPCEnabled() {
		if err := b.subscribeJetStreamRPCMethod(method); err != nil {
			b.mu.Lock()
			delete(b.rpcHandlers, method)
			b.mu.Unlock()
			return err
		}
		return nil
	}

	if _, err := b.subscribeLightweightRPCMethod(method); err != nil {
		b.mu.Lock()
		delete(b.rpcHandlers, method)
		b.mu.Unlock()
		return err
	}
	return nil
}

func (b *Bus) subscribeLightweightRPCMethod(method string) ([]nats.ISubscription, error) {
	if b == nil || b.client == nil {
		return nil, nil
	}
	// Subscribe to service RPC subject with queue group and to this sid's
	// direct RPC subject. The former balances new work across service
	// instances; the latter supports sticky follow-up calls.
	subject := b.subject.Rpc(b.cfg.SvcType, method)
	b.lifeMu.Lock()
	defer b.lifeMu.Unlock()
	if b.stopping || b.stopped {
		return nil, fmt.Errorf("bus: cannot register rpc handler while stopping or stopped")
	}
	sub, err := b.client.QueueSubscribe(subject, b.cfg.SvcType+"_rpc", b.onRpcMessage)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe rpc %s: %w", method, err)
	}
	instanceSubject := b.subject.RpcInstance(b.cfg.SvcType, b.cfg.Sid, method)
	instanceSub, err := b.client.Subscribe(instanceSubject, b.onRpcMessage)
	if err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("bus: subscribe rpc %s: %w", instanceSubject, err)
	}
	subs := []nats.ISubscription{sub, instanceSub}
	b.subs = append(b.subs, subs...)
	return subs, nil
}

// --- Internal ---

func (b *Bus) encodeMsg(toSid int32, toModule, name string, msg any, broadcast nats.BroadcastType) ([]byte, error) {
	payload, err := b.codec.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("bus: marshal: %w", err)
	}
	natsMsg := &nats.NatsMsg{
		FromSid:   b.cfg.Sid,
		ToSid:     toSid,
		ToModule:  toModule,
		MsgName:   name,
		Payload:   payload,
		Broadcast: broadcast,
	}
	if b.reliable != nil && b.cfg.Reliable.Enabled {
		natsMsg.MsgID = b.nextMsgID()
		natsMsg.Attempt = 1
		natsMsg.CreatedAt = time.Now().UnixMilli()
	}
	return b.codec.Marshal(natsMsg)
}

func (b *Bus) onMessage(msg *nats.Msg) {
	natsMsg := &nats.NatsMsg{}
	if err := b.codec.Unmarshal(msg.Data, natsMsg); err != nil {
		slog.Error("bus: decode nats msg failed", "err", err)
		return
	}
	task := &incomingTask{
		natsMsg: natsMsg,
		isRpc:   false,
	}
	// Hash by module name for ordering guarantee within same module
	key := hashString(natsMsg.ToModule)
	b.dispatchTask(key, task)
}

func (b *Bus) onRpcMessage(msg *nats.Msg) {
	natsMsg := &nats.NatsMsg{}
	if err := b.codec.Unmarshal(msg.Data, natsMsg); err != nil || natsMsg.MsgName == "" || natsMsg.SessionId == "" {
		slog.Error("bus: invalid rpc request envelope", "subject", msg.Subject, "err", err)
		if msg.Reply != "" {
			data, encodeErr := encodeRPCFailure(b.codec, fmt.Errorf("bus: invalid rpc request envelope"))
			if encodeErr == nil {
				_ = b.rpc.Reply(msg.Reply, data)
			}
		}
		return
	}
	task := &incomingTask{
		natsMsg:      natsMsg,
		isRpc:        true,
		replySubject: msg.Reply,
	}
	key := hashString(natsMsg.MsgName)
	b.dispatchTask(key, task)
}

func (b *Bus) extractRpcMethod(subject string) string {
	if b != nil && b.subject != nil {
		prefix := b.subject.Rpc(b.cfg.SvcType, "")
		if strings.HasPrefix(subject, prefix) {
			method := strings.TrimPrefix(subject, prefix)
			sidPrefix := fmt.Sprintf("%d.", b.cfg.Sid)
			if strings.HasPrefix(method, sidPrefix) {
				return strings.TrimPrefix(method, sidPrefix)
			}
			return method
		}
	}
	return extractMethod(subject)
}

func (b *Bus) dispatchTask(key int64, task *incomingTask) {
	if task == nil || task.natsMsg == nil {
		slog.Warn("bus: drop empty task")
		return
	}
	b.lifeMu.Lock()
	pool := b.pool
	b.lifeMu.Unlock()
	if pool == nil {
		task.OnRelease()
		slog.Warn("bus: drop message because dispatcher is not running")
		b.deadLetter(task.natsMsg, "dispatcher not running")
		return
	}
	if err := pool.Dispatch(key, task); err != nil {
		slog.Error("bus: dispatch task failed",
			"module", task.natsMsg.ToModule,
			"msg", task.natsMsg.MsgName,
			"msg_id", task.natsMsg.MsgID,
			"err", err,
		)
		metrics.IncCounter("bus_dispatch_drop_total", metrics.Labels{
			"module": task.natsMsg.ToModule,
			"msg":    task.natsMsg.MsgName,
			"reason": err.Error(),
		}, 1)
		b.deadLetter(task.natsMsg, "dispatch failed: "+err.Error())
	}
}

func (b *Bus) handleTask(task *incomingTask) {
	if task.isRpc {
		b.dispatchRpc(task)
	} else {
		b.dispatchMsg(task)
	}
}

func (b *Bus) dispatchMsg(task *incomingTask) {
	b.mu.RLock()
	key := task.natsMsg.ToModule + ":" + task.natsMsg.MsgName
	handler, ok := b.handlers[key]
	b.mu.RUnlock()

	if !ok {
		slog.Warn("bus: no handler", "module", task.natsMsg.ToModule, "msg", task.natsMsg.MsgName)
		b.deadLetter(task.natsMsg, ErrNoHandler.Error())
		return
	}
	if !b.beginConsume(task.natsMsg) {
		return
	}
	start := time.Now()
	defer func() {
		metrics.ObserveDuration("bus_dispatch_duration", metrics.Labels{
			"module": task.natsMsg.ToModule,
			"msg":    task.natsMsg.MsgName,
		}, time.Since(start))
		if r := recover(); r != nil {
			reason := fmt.Sprintf("panic: %v", r)
			slog.Error("bus: handler panic",
				"module", task.natsMsg.ToModule,
				"msg", task.natsMsg.MsgName,
				"msg_id", task.natsMsg.MsgID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			b.deadLetter(task.natsMsg, reason)
		}
	}()

	ctx := &MsgContext{
		FromSid:   task.natsMsg.FromSid,
		ToModule:  task.natsMsg.ToModule,
		MsgName:   task.natsMsg.MsgName,
		MsgID:     task.natsMsg.MsgID,
		Attempt:   task.natsMsg.Attempt,
		CreatedAt: task.natsMsg.CreatedAt,
		Payload:   task.natsMsg.Payload,
		base:      b.baseContext(),
		codec:     b.codec,
	}
	_, release := fctx.NewContext(fctx.WithBase(ctx.Context()), fctx.WithSource("bus"), fctx.WithHandler(key))
	defer release()
	handler(ctx)
	if err := b.finishConsume(task.natsMsg); err != nil {
		slog.Error("bus: finish consume failed", "msg_id", task.natsMsg.MsgID, "err", err)
		b.deadLetter(task.natsMsg, "finish consume failed: "+err.Error())
	}
	metrics.IncCounter("bus_dispatch_total", metrics.Labels{
		"module": task.natsMsg.ToModule,
		"msg":    task.natsMsg.MsgName,
	}, 1)
}

func (b *Bus) dispatchRpc(task *incomingTask) {
	method := task.natsMsg.MsgName
	b.mu.RLock()
	handler, ok := b.rpcHandlers[method]
	b.mu.RUnlock()

	if !ok {
		slog.Warn("bus: no rpc handler", "method", method)
		if task.replySubject != "" {
			errResp, encodeErr := encodeRPCFailure(b.codec, fmt.Errorf("%w: %s", ErrNoHandler, method))
			if encodeErr == nil {
				if replyErr := b.rpc.Reply(task.replySubject, errResp); replyErr != nil {
					slog.Error("bus: reply rpc no-handler failed", "method", method, "err", replyErr)
				}
			}
		}
		return
	}

	ctx := &RpcContext{
		MsgContext: MsgContext{
			FromSid:   task.natsMsg.FromSid,
			ToModule:  task.natsMsg.ToModule,
			MsgName:   method,
			MsgID:     task.natsMsg.MsgID,
			Attempt:   task.natsMsg.Attempt,
			CreatedAt: task.natsMsg.CreatedAt,
			Payload:   task.natsMsg.Payload,
			base:      b.baseContext(),
			codec:     b.codec,
		},
		Method:       method,
		ReplySubject: task.replySubject,
	}

	_, release := fctx.NewContext(fctx.WithBase(ctx.Context()), fctx.WithSource("bus_rpc"), fctx.WithHandler(method))
	defer release()
	var resp any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
				slog.Error("bus: rpc handler panic", "method", method, "err", r, "stack", string(debug.Stack()))
			}
		}()
		resp, err = handler(ctx)
	}()
	if task.replySubject != "" {
		if err != nil {
			errResp, encErr := encodeRPCFailure(b.codec, err)
			if encErr != nil {
				slog.Error("bus: marshal rpc error response failed", "method", method, "err", encErr)
				return
			}
			if replyErr := b.rpc.Reply(task.replySubject, errResp); replyErr != nil {
				slog.Error("bus: reply rpc error failed", "method", method, "err", replyErr)
			}
		} else {
			data, encErr := encodeRPCSuccess(b.codec, resp)
			if encErr != nil {
				slog.Error("bus: marshal rpc resp failed", "method", method, "err", encErr)
				return
			}
			if replyErr := b.rpc.Reply(task.replySubject, data); replyErr != nil {
				slog.Error("bus: reply rpc response failed", "method", method, "err", replyErr)
			}
		}
	}
}

func (b *Bus) nextMsgID() string {
	seq := b.msgSeq.Add(1)
	return fmt.Sprintf("%s:%d:%d:%d", b.cfg.SvcType, b.cfg.Sid, time.Now().UnixNano(), seq)
}

func (b *Bus) consumer() ReliableConsumer {
	if b == nil {
		return ReliableConsumer{}
	}
	return ReliableConsumer{ServiceType: b.cfg.SvcType, Sid: b.cfg.Sid}
}

func (b *Bus) baseContext() context.Context {
	if b != nil {
		if ctx := b.ctx.Load(); ctx != nil {
			return *ctx
		}
	}
	return context.Background()
}

func (b *Bus) beginConsume(msg *nats.NatsMsg) bool {
	if b == nil || b.reliable == nil || !b.cfg.Reliable.Enabled || msg == nil || msg.MsgID == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := b.reliable.BeginConsume(ctx, b.consumer(), msg)
	if err != nil {
		slog.Error("bus: reliable begin consume failed",
			"module", msg.ToModule,
			"msg", msg.MsgName,
			"msg_id", msg.MsgID,
			"err", err,
		)
		b.deadLetter(msg, "begin consume failed: "+err.Error())
		return false
	}
	if !ok {
		metrics.IncCounter("bus_duplicate_total", metrics.Labels{
			"module": msg.ToModule,
			"msg":    msg.MsgName,
		}, 1)
		slog.Debug("bus: duplicate message skipped", "msg_id", msg.MsgID, "module", msg.ToModule, "msg", msg.MsgName)
		return false
	}
	return true
}

func (b *Bus) finishConsume(msg *nats.NatsMsg) error {
	if b == nil || b.reliable == nil || !b.cfg.Reliable.Enabled || msg == nil || msg.MsgID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return b.reliable.FinishConsume(ctx, b.consumer(), msg)
}

func (b *Bus) deadLetter(msg *nats.NatsMsg, reason string) {
	if b == nil || b.reliable == nil || !b.cfg.Reliable.Enabled || msg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.reliable.DeadLetter(ctx, b.consumer(), msg, reason); err != nil {
		slog.Error("bus: write dead letter failed",
			"module", msg.ToModule,
			"msg", msg.MsgName,
			"msg_id", msg.MsgID,
			"reason", reason,
			"err", err,
		)
		return
	}
	metrics.IncCounter("bus_dead_letter_total", metrics.Labels{
		"module": msg.ToModule,
		"msg":    msg.MsgName,
	}, 1)
}

// --- incomingTask implements worker.Task ---

type incomingTask struct {
	natsMsg      *nats.NatsMsg
	isRpc        bool
	replySubject string
}

func (t *incomingTask) OnRelease() {}

// --- helpers ---

func msgName(msg any) string {
	if n, ok := msg.(interface{ MsgName() string }); ok {
		return n.MsgName()
	}
	return fmt.Sprintf("%T", msg)
}

func hashString(s string) int64 {
	var h int64
	for _, c := range s {
		h = h*31 + int64(c)
	}
	return h
}

func extractMethod(subject string) string {
	// Subject format: {prefix}.rpc.{service}.{method}
	// Extract last segment
	for i := len(subject) - 1; i >= 0; i-- {
		if subject[i] == '.' {
			return subject[i+1:]
		}
	}
	return subject
}

// Ensure Bus implements IBus at compile time.
var _ IBus = (*Bus)(nil)

// ErrNoHandler is returned when no handler is registered for a message.
var ErrNoHandler = errors.New("bus: no handler registered")
