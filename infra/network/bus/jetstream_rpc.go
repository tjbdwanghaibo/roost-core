package bus

import (
	"context"
	"errors"
	"fmt"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultJetStreamRPCRequestStream  = "ROOST_RPC_REQUESTS"
	defaultJetStreamRPCResponseStream = "ROOST_RPC_RESPONSES"
	defaultJetStreamRPCAckWait        = 10 * time.Second
	defaultJetStreamRPCMaxDeliver     = 5
	defaultJetStreamRPCRequestTTL     = 30 * time.Second
	defaultJetStreamRPCCallTimeout    = 5 * time.Second
	defaultJetStreamRPCStreamMaxAge   = 30 * time.Minute
	defaultJetStreamRPCSetupTimeout   = 5 * time.Second
)

var (
	ErrJetStreamRPCUnavailable = errors.New("bus: jetstream rpc unavailable")
	ErrJetStreamRPCExpired     = errors.New("bus: jetstream rpc request expired")

	// errJetStreamRPCStopping 让停止开始后才到达的投递回到 broker（driver 据此 NAK），
	// 由同服务的其他实例处理（RR-20261005-NC-90）。
	errJetStreamRPCStopping = errors.New("bus: jetstream rpc is stopping; delivery returned to the broker")
)

type JetStreamRPCConfig struct {
	RequestStream  string
	ResponseStream string
	Storage        fnats.JetStreamStorage
	AckWait        time.Duration
	MaxDeliver     int
	RequestTTL     time.Duration
	CallTimeout    time.Duration
	StreamMaxAge   time.Duration
	Duplicates     time.Duration
	Replicas       int
	MaxBytes       int64
	SetupTimeout   time.Duration
}

func (c JetStreamRPCConfig) normalize() JetStreamRPCConfig {
	if c.RequestStream == "" {
		c.RequestStream = defaultJetStreamRPCRequestStream
	}
	if c.ResponseStream == "" {
		c.ResponseStream = defaultJetStreamRPCResponseStream
	}
	if c.Storage == "" {
		c.Storage = fnats.JetStreamStorageFile
	}
	if c.AckWait <= 0 {
		c.AckWait = defaultJetStreamRPCAckWait
	}
	if c.MaxDeliver <= 0 {
		c.MaxDeliver = defaultJetStreamRPCMaxDeliver
	}
	if c.RequestTTL <= 0 {
		c.RequestTTL = defaultJetStreamRPCRequestTTL
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultJetStreamRPCCallTimeout
	}
	if c.StreamMaxAge <= 0 {
		c.StreamMaxAge = defaultJetStreamRPCStreamMaxAge
	}
	if c.Duplicates <= 0 {
		c.Duplicates = c.RequestTTL
	}
	if c.SetupTimeout <= 0 {
		c.SetupTimeout = defaultJetStreamRPCSetupTimeout
	}
	return c
}

type jetStreamRPC struct {
	js              fnats.IJetStream
	cfg             JetStreamRPCConfig
	pending         sync.Map // request id -> *pendingJetStreamRPCCall
	pendingCount    atomic.Int64
	pendingByMethod sync.Map // method label -> *atomic.Int64
	seq             atomic.Uint64

	// methodLabels is the set of caller-side method labels this Bus has
	// handed out (jetStreamRPCCallerMethodLabel, RR-20261006-19); labelMu
	// serializes adding to it so the bound holds under concurrent first calls.
	labelMu        sync.Mutex
	methodLabels   sync.Map // method -> struct{}
	methodLabelLen int
	labelOverflow  atomic.Bool

	mu          sync.Mutex
	subs        []fnats.IJetStreamSubscription // request consumers
	responseSub fnats.IJetStreamSubscription

	// RR-20261005-NC-90：请求 handler 在 nats.go 的 consume 回调 goroutine 里同步执行，
	// 不在 Bus 的 pool 里，pool 排空管不到它们；nats.go 的 ConsumeContext.Stop 也不等在途
	// 回调。handlers 是它们自己的准入与在途计数（共用的 operation.Lifetime，A3）：停止先关准入，
	// 再等在途归零，归零之前连接不能交还（Bus 的停止返回 ctx 错误、保留责任）。
	handlers operation.Lifetime
}

type pendingJetStreamRPCCall struct {
	resp chan []byte
}

func (b *Bus) EnableJetStreamRPC(js fnats.IJetStream, cfg JetStreamRPCConfig) error {
	if b == nil {
		return ErrJetStreamRPCUnavailable
	}
	if js == nil {
		return ErrJetStreamRPCUnavailable
	}
	cfg = cfg.normalize()
	rpc := &jetStreamRPC{js: js, cfg: cfg}
	b.jsRPC = rpc
	ctx, cancel := context.WithTimeout(context.Background(), cfg.SetupTimeout)
	defer cancel()
	if err := b.ensureJetStreamRPCStreams(ctx); err != nil {
		b.jsRPC = nil
		return err
	}
	if err := b.subscribeJetStreamRPCResponses(ctx); err != nil {
		b.jsRPC = nil
		return err
	}
	return nil
}

func (b *Bus) jetStreamRPCEnabled() bool {
	return b != nil && b.jsRPC != nil && b.jsRPC.js != nil
}

func (b *Bus) ensureJetStreamRPCStreams(ctx context.Context) error {
	if !b.jetStreamRPCEnabled() {
		return ErrJetStreamRPCUnavailable
	}
	cfg := b.jsRPC.cfg
	requestSubjects := []string{fmt.Sprintf("%s.rpc.>", b.cfg.Prefix)}
	responseSubjects := []string{fmt.Sprintf("%s.rpc_resp.>", b.cfg.Prefix)}
	if err := b.jsRPC.js.EnsureStream(ctx, fnats.JetStreamConfig{
		Name:       cfg.RequestStream,
		Subjects:   requestSubjects,
		Storage:    cfg.Storage,
		MaxAge:     cfg.StreamMaxAge,
		Duplicates: cfg.Duplicates,
		Replicas:   cfg.Replicas,
		MaxBytes:   cfg.MaxBytes,
	}); err != nil {
		return fmt.Errorf("bus: ensure jetstream rpc request stream: %w", err)
	}
	if err := b.jsRPC.js.EnsureStream(ctx, fnats.JetStreamConfig{
		Name:       cfg.ResponseStream,
		Subjects:   responseSubjects,
		Storage:    cfg.Storage,
		MaxAge:     cfg.StreamMaxAge,
		Duplicates: cfg.Duplicates,
		Replicas:   cfg.Replicas,
		MaxBytes:   cfg.MaxBytes,
	}); err != nil {
		return fmt.Errorf("bus: ensure jetstream rpc response stream: %w", err)
	}
	return nil
}

func (b *Bus) subscribeJetStreamRPCResponses(ctx context.Context) error {
	if !b.jetStreamRPCEnabled() {
		return ErrJetStreamRPCUnavailable
	}
	cfg := b.jsRPC.cfg
	name := sanitizeJetStreamRPCName(fmt.Sprintf("rpc_resp_%s_%d", b.cfg.SvcType, b.cfg.Sid))
	sub, err := b.jsRPC.js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        cfg.ResponseStream,
		Name:          name,
		Durable:       name,
		FilterSubject: b.subject.RpcResponseInbox(b.cfg.Sid),
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
	}, b.onJetStreamRPCResponse)
	if err != nil {
		return fmt.Errorf("bus: subscribe jetstream rpc responses: %w", err)
	}
	b.jsRPC.mu.Lock()
	b.jsRPC.responseSub = sub
	b.jsRPC.mu.Unlock()
	return nil
}

func (b *Bus) subscribeJetStreamRPCMethod(method string) error {
	if !b.jetStreamRPCEnabled() {
		return ErrJetStreamRPCUnavailable
	}
	b.lifeMu.Lock()
	defer b.lifeMu.Unlock()
	if b.stopping || b.stopped {
		return fmt.Errorf("bus: cannot register rpc handler while stopping or stopped")
	}
	cfg := b.jsRPC.cfg
	ctx, cancel := context.WithTimeout(context.Background(), cfg.SetupTimeout)
	defer cancel()

	serviceName := sanitizeJetStreamRPCName(fmt.Sprintf("rpc_%s_%s", b.cfg.SvcType, method))
	serviceSub, err := b.jsRPC.js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        cfg.RequestStream,
		Name:          serviceName,
		Durable:       serviceName,
		FilterSubject: b.subject.Rpc(b.cfg.SvcType, method),
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
	}, b.onJetStreamRPCRequest)
	if err != nil {
		return fmt.Errorf("bus: subscribe jetstream service rpc %s: %w", method, err)
	}

	instanceName := sanitizeJetStreamRPCName(fmt.Sprintf("rpc_%s_%d_%s", b.cfg.SvcType, b.cfg.Sid, method))
	instanceSub, err := b.jsRPC.js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        cfg.RequestStream,
		Name:          instanceName,
		Durable:       instanceName,
		FilterSubject: b.subject.RpcInstance(b.cfg.SvcType, b.cfg.Sid, method),
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
	}, b.onJetStreamRPCRequest)
	if err != nil {
		serviceSub.Stop()
		return fmt.Errorf("bus: subscribe jetstream instance rpc %s: %w", method, err)
	}
	b.addJetStreamRPCSubscription(serviceSub)
	b.addJetStreamRPCSubscription(instanceSub)
	return nil
}

func (b *Bus) addJetStreamRPCSubscription(sub fnats.IJetStreamSubscription) {
	if !b.jetStreamRPCEnabled() || sub == nil {
		return
	}
	b.jsRPC.mu.Lock()
	defer b.jsRPC.mu.Unlock()
	b.jsRPC.subs = append(b.jsRPC.subs, sub)
}

// stopJetStreamRPCRequests closes request admission and stops the request
// consumers. It runs first in a stop: a delivery that reaches the Bus after this
// goes back to the broker untouched, and stopping the consumers early keeps this
// instance from NAKing the same request round after round and burning its
// MaxDeliver. Handlers already running keep the connection; the stop waits for
// them with waitJetStreamRPCRequests (RR-20261005-NC-90).
func (b *Bus) stopJetStreamRPCRequests() {
	if !b.jetStreamRPCEnabled() {
		return
	}
	b.jsRPC.handlers.Stop()
	b.jsRPC.mu.Lock()
	subs := append([]fnats.IJetStreamSubscription(nil), b.jsRPC.subs...)
	b.jsRPC.subs = nil
	b.jsRPC.mu.Unlock()
	for _, sub := range subs {
		if sub != nil {
			sub.Stop()
		}
	}
}

// waitJetStreamRPCRequests waits within ctx for the admitted request handlers
// to return. A ctx error only means the caller stopped waiting; a later call
// waits for the same handlers again.
func (b *Bus) waitJetStreamRPCRequests(ctx context.Context) error {
	if !b.jetStreamRPCEnabled() {
		return nil
	}
	return b.jsRPC.handlers.Wait(ctx)
}

// stopJetStreamRPCResponses stops the response consumer and fails the calls
// still waiting for a response. It runs after the handlers drained, so a
// handler that makes a nested reliable call while it drains still gets its
// answer.
func (b *Bus) stopJetStreamRPCResponses() {
	if !b.jetStreamRPCEnabled() {
		return
	}
	b.jsRPC.mu.Lock()
	sub := b.jsRPC.responseSub
	b.jsRPC.responseSub = nil
	b.jsRPC.mu.Unlock()
	if sub != nil {
		sub.Stop()
	}
	// Ownership of a pending call is claimed by LoadAndDelete: whoever removes
	// the entry is the only party allowed to touch its channel. Closing the
	// value observed by Range directly would race a concurrent responder that
	// already claimed the same entry and is about to send.
	b.jsRPC.pending.Range(func(key, _ any) bool {
		value, ok := b.jsRPC.pending.LoadAndDelete(key)
		if !ok {
			return true
		}
		if pending, ok := value.(*pendingJetStreamRPCCall); ok && pending != nil {
			close(pending.resp)
		}
		return true
	})
}

func (b *Bus) callJetStreamRPC(ctx context.Context, subject string, method string, toSid int32, payload []byte) ([]byte, error) {
	if !b.jetStreamRPCEnabled() {
		return nil, ErrJetStreamRPCUnavailable
	}
	callCtx, cancel := b.jetStreamRPCCallContext(ctx)
	defer cancel()

	reqID := b.nextJetStreamRPCRequestID()
	pending := &pendingJetStreamRPCCall{resp: make(chan []byte, 1)}
	b.jsRPC.pending.Store(reqID, pending)
	// The wire carries the real method; only the metrics label is bounded.
	label := b.jetStreamRPCCallerMethodLabel(method)
	b.addJetStreamRPCPending(label, 1)
	defer func() {
		b.jsRPC.pending.Delete(reqID)
		b.addJetStreamRPCPending(label, -1)
	}()

	now := time.Now()
	deadlineAt := now.Add(b.jsRPC.cfg.RequestTTL).UnixMilli()
	if deadline, ok := callCtx.Deadline(); ok {
		deadlineAt = deadline.UnixMilli()
	}
	req := &fnats.NatsMsg{
		FromSid:      b.cfg.Sid,
		ToSid:        toSid,
		MsgName:      method,
		Payload:      payload,
		SessionId:    reqID,
		MsgID:        reqID,
		Attempt:      1,
		CreatedAt:    now.UnixMilli(),
		ReplySubject: b.subject.RpcResponse(b.cfg.Sid, reqID),
		DeadlineAt:   deadlineAt,
	}
	data, err := b.codec.Marshal(req)
	if err != nil {
		b.recordJetStreamRPCCall(label, "marshal_error", "marshal")
		return nil, fmt.Errorf("bus: marshal jetstream rpc req: %w", err)
	}
	if _, err := b.jsRPC.js.Publish(callCtx, subject, data, fnats.JetStreamPublishOptions{MsgID: reqID}); err != nil {
		b.recordJetStreamRPCCall(label, "publish_error", "publish")
		return nil, fmt.Errorf("bus: publish jetstream rpc %s: %w", subject, err)
	}
	select {
	case resp, ok := <-pending.resp:
		if !ok {
			b.recordJetStreamRPCCall(label, "cancel", "closed")
			return nil, fnats.ErrCancelled
		}
		b.recordJetStreamRPCCall(label, "ok", "")
		return resp, nil
	case <-callCtx.Done():
		err := jetStreamRPCContextError(callCtx)
		result := "cancel"
		reason := "cancel"
		if errors.Is(err, fnats.ErrTimeout) {
			result = "timeout"
			reason = "timeout"
		}
		b.recordJetStreamRPCCall(label, result, reason)
		return nil, err
	}
}

func (b *Bus) callJetStreamRPCAsync(subject string, method string, payload []byte, cb func(resp []byte, err error)) {
	go func() {
		ctx, cancel := context.WithTimeout(fctx.BaseContext(), b.jsRPC.cfg.CallTimeout)
		defer cancel()
		resp, err := b.callJetStreamRPC(ctx, subject, method, 0, payload)
		cb(resp, err)
	}()
}

func (b *Bus) jetStreamRPCCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.jsRPC.cfg.CallTimeout)
}

func (b *Bus) onJetStreamRPCResponse(_ context.Context, msg *fnats.JetStreamMsg) error {
	if msg == nil {
		return nil
	}
	requestID := b.extractJetStreamRPCResponseID(msg.Subject)
	if requestID == "" {
		slog.Warn("bus: drop jetstream rpc response without request id", "subject", msg.Subject)
		return nil
	}
	value, ok := b.jsRPC.pending.LoadAndDelete(requestID)
	if !ok {
		slog.Debug("bus: drop stale jetstream rpc response", "request_id", requestID, "subject", msg.Subject)
		return nil
	}
	pending, ok := value.(*pendingJetStreamRPCCall)
	if !ok || pending == nil {
		return nil
	}
	select {
	case pending.resp <- append([]byte(nil), msg.Data...):
	default:
		slog.Warn("bus: drop jetstream rpc response because pending channel is full", "request_id", requestID, "subject", msg.Subject)
	}
	return nil
}

func (b *Bus) onJetStreamRPCRequest(ctx context.Context, msg *fnats.JetStreamMsg) error {
	if msg == nil {
		return nil
	}
	if !b.jsRPC.handlers.Begin() {
		return errJetStreamRPCStopping
	}
	defer b.jsRPC.handlers.End()
	var req fnats.NatsMsg
	if err := b.codec.Unmarshal(msg.Data, &req); err != nil {
		return fmt.Errorf("bus: decode jetstream rpc req: %w", err)
	}
	if req.MsgName == "" {
		req.MsgName = b.extractRpcMethod(msg.Subject)
	}
	b.mu.RLock()
	handler, ok := b.rpcHandlers[req.MsgName]
	b.mu.RUnlock()
	// The method label is the registered method, never the envelope's
	// free-form name: a request naming no registered method is counted under
	// one label, so the served methods bound these series (RR-20261006-19).
	label := req.MsgName
	if !ok {
		label = jetStreamRPCUnregisteredMethod
	}
	if strings.TrimSpace(req.ReplySubject) == "" {
		// callJetStreamRPC always sets a reply subject. A request without one is a
		// lightweight Call / CallTo the request stream captured (its subjects cover
		// <prefix>.rpc.>): that caller already got the stream's PubAck as an error
		// and nobody can receive this answer, so it must not run — the old code ran
		// it with no deadline and dropped the reply (RR-20261005-NC-92).
		slog.Warn("bus: refuse jetstream rpc request without reply subject; the caller used the lightweight transport", "method", req.MsgName, "request_id", req.SessionId)
		b.recordJetStreamRPCRequest(label, "no_reply_subject")
		return nil
	}
	if msg.NumDelivered > 0 {
		metrics.SetGauge("bus_rpc_consumer_delivery", metrics.Labels{
			"transport": "jetstream",
			"method":    label,
		}, int64(msg.NumDelivered))
	}
	if req.DeadlineAt > 0 && time.Now().UnixMilli() > req.DeadlineAt {
		slog.Warn("bus: drop expired jetstream rpc request", "method", req.MsgName, "request_id", req.SessionId)
		b.recordJetStreamRPCRequest(label, "expired")
		return nil
	}
	processCtx, cancel := b.jetStreamRPCProcessContext(req)
	defer cancel()
	// handler 可能跑得比 AckWait 久（期限取调用方截止）：处理期间定期发 in-progress，broker 不会把这条
	// 重投给共享 durable 的另一个实例并发执行（RR-20261006-70）。
	defer b.keepJetStreamRPCInProgress(msg, processCtx)()

	if !ok {
		slog.Warn("bus: no jetstream rpc handler", "method", req.MsgName)
		b.recordJetStreamRPCRequest(label, "no_handler")
		// RR-20261004-NC-08：拒绝也要走客户端要求的版本 envelope。
		data, err := encodeRPCFailure(b.codec, fmt.Errorf("%w: %s", ErrNoHandler, req.MsgName))
		if err != nil {
			return err
		}
		return b.publishJetStreamRPCReply(req, data)
	}

	rpcCtx := &RpcContext{
		MsgContext: MsgContext{
			FromSid:   req.FromSid,
			ToModule:  req.ToModule,
			MsgName:   req.MsgName,
			MsgID:     req.MsgID,
			Attempt:   req.Attempt,
			CreatedAt: req.CreatedAt,
			Payload:   req.Payload,
			base:      processCtx,
			codec:     b.codec,
		},
		Method:       req.MsgName,
		ReplySubject: req.ReplySubject,
	}

	_, release := fctx.NewContext(fctx.WithBase(rpcCtx.Context()), fctx.WithSource("bus_jsrpc"), fctx.WithHandler(req.MsgName))
	defer release()
	var resp any
	var handlerErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				handlerErr = fmt.Errorf("panic: %v", r)
				slog.Error("bus: jetstream rpc handler panic", "method", req.MsgName, "err", r, "stack", string(debug.Stack()))
			}
		}()
		resp, handlerErr = handler(rpcCtx)
	}()

	if handlerErr != nil && errors.Is(processCtx.Err(), context.Canceled) {
		// processCtx 只会因 Bus 停止被取消（期限到达是 DeadlineExceeded）。handler 因此中断时
		// 不把“已取消”当业务结果回给调用方，消息交还 broker（NAK），由其他实例处理（NC-90）。
		b.recordJetStreamRPCRequest(label, "interrupted")
		return fmt.Errorf("bus: jetstream rpc %s interrupted by stop: %w", req.MsgName, handlerErr)
	}

	var data []byte
	var err error
	if handlerErr != nil {
		data, err = encodeRPCFailure(b.codec, handlerErr)
	} else {
		data, err = encodeRPCSuccess(b.codec, resp)
	}
	if err != nil {
		b.recordJetStreamRPCRequest(label, "marshal_error")
		return fmt.Errorf("bus: marshal jetstream rpc resp %s: %w", req.MsgName, err)
	}
	if err := b.publishJetStreamRPCReply(req, data); err != nil {
		b.recordJetStreamRPCRequest(label, "publish_error")
		return err
	}
	if handlerErr != nil {
		b.recordJetStreamRPCRequest(label, "handler_error")
	} else {
		b.recordJetStreamRPCRequest(label, "ok")
	}
	return nil
}

// keepJetStreamRPCInProgress 在 handler 执行期间每 AckWait/2 发一次 in-progress，直到返回的 stop 被调用或
// processCtx 结束（请求期限到达或 Bus 停止），返回 stop。
//
// 修前（RR-20261006-70）处理期间不发 in-progress：handler 期限取调用方截止，可以远长于 nats.rpc.ack_wait；
// AckWait 一到 broker 把未确认的请求重投，服务级 durable 在实例间共享，另一个实例（或本实例排队的副本）
// 再执行一次，服务端没有按 MsgID 去重——实测 ack_wait 1s、handler 2.5s 时同一请求执行 3 次。
// 心跳只持续到请求期限：期限之后调用方已放弃，停止心跳让 broker 重投，收到的实例按 DeadlineAt 判过期
// 直接确认、不执行。心跳失败（连接抖动）只记日志，退回 at-least-once。用定时器而不是每请求一个 goroutine。
func (b *Bus) keepJetStreamRPCInProgress(msg *fnats.JetStreamMsg, processCtx context.Context) (stop func()) {
	ackWait := b.jsRPC.cfg.AckWait
	if msg == nil || msg.InProgress == nil || ackWait <= 0 {
		return func() {}
	}
	interval := ackWait / 2
	var mu sync.Mutex
	stopped := false
	var timer *time.Timer
	mu.Lock() // 回调先取 mu 再读 timer：赋值在锁内，回调一定看到它
	defer mu.Unlock()
	timer = time.AfterFunc(interval, func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped || processCtx.Err() != nil {
			return
		}
		if err := msg.InProgress(); err != nil {
			metrics.IncCounter("bus_rpc_in_progress_failures_total", nil, 1)
			slog.Warn("bus: jetstream rpc in-progress ack failed; the broker may redeliver after ack wait", "subject", msg.Subject, "err", err)
		}
		timer.Reset(interval)
	})
	return func() {
		mu.Lock()
		stopped = true
		timer.Stop()
		mu.Unlock()
	}
}

func (b *Bus) jetStreamRPCProcessContext(req fnats.NatsMsg) (context.Context, context.CancelFunc) {
	base := b.baseContext()
	if req.DeadlineAt <= 0 {
		return base, func() {}
	}
	deadline := time.UnixMilli(req.DeadlineAt)
	if time.Now().After(deadline) {
		ctx, cancel := context.WithCancel(base)
		cancel()
		return ctx, func() {}
	}
	return context.WithDeadline(base, deadline)
}

// publishJetStreamRPCReply publishes the result of a handler that ran to the
// end. Its budget is the caller's deadline (AckWait when the request carries
// none), detached from the Bus lifetime: the old code published on processCtx,
// which the stop cancels first, so a handler that finished within the stop
// budget always lost its response and was redelivered (RR-20261005-NC-90).
func (b *Bus) publishJetStreamRPCReply(req fnats.NatsMsg, data []byte) error {
	var ctx context.Context
	var cancel context.CancelFunc
	if req.DeadlineAt > 0 {
		ctx, cancel = context.WithDeadline(context.Background(), time.UnixMilli(req.DeadlineAt))
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), b.jsRPC.cfg.AckWait)
	}
	defer cancel()
	return b.publishJetStreamRPCResponse(ctx, req.ReplySubject, req.SessionId, data)
}

func (b *Bus) publishJetStreamRPCResponse(ctx context.Context, replySubject string, requestID string, data []byte) error {
	if strings.TrimSpace(replySubject) == "" {
		return nil
	}
	if requestID == "" {
		requestID = b.nextJetStreamRPCRequestID()
	}
	_, err := b.jsRPC.js.Publish(ctx, replySubject, data, fnats.JetStreamPublishOptions{MsgID: requestID})
	if err != nil {
		return fmt.Errorf("bus: publish jetstream rpc response %s: %w", replySubject, err)
	}
	return nil
}

func (b *Bus) nextJetStreamRPCRequestID() string {
	seq := b.jsRPC.seq.Add(1)
	service := sanitizeJetStreamRPCName(b.cfg.SvcType)
	if service == "" {
		service = "svc"
	}
	return fmt.Sprintf("%s-%d-%d-%d", service, b.cfg.Sid, time.Now().UnixNano(), seq)
}

func (b *Bus) extractJetStreamRPCResponseID(subject string) string {
	prefix := b.subject.RpcResponse(b.cfg.Sid, "")
	if strings.HasPrefix(subject, prefix) {
		return strings.TrimPrefix(subject, prefix)
	}
	idx := strings.LastIndexByte(subject, '.')
	if idx < 0 || idx+1 >= len(subject) {
		return ""
	}
	return subject[idx+1:]
}

func sanitizeJetStreamRPCName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func (b *Bus) addJetStreamRPCPending(method string, delta int64) {
	if b == nil || b.jsRPC == nil {
		return
	}
	total := b.jsRPC.pendingCount.Add(delta)
	if total < 0 {
		total = 0
		b.jsRPC.pendingCount.Store(0)
	}
	methodCounter := b.jetStreamRPCMethodPendingCounter(method)
	methodValue := methodCounter.Add(delta)
	if methodValue < 0 {
		methodValue = 0
		methodCounter.Store(0)
	}
	metrics.SetGauge("bus_rpc_pending_requests", metrics.Labels{
		"transport": "jetstream",
	}, total)
	metrics.SetGauge("bus_rpc_pending", metrics.Labels{
		"transport": "jetstream",
		"method":    method,
	}, methodValue)
}

// Method labels of the reliable RPC metrics (RR-20261006-19).
//
// A method has no unregister: HandleRpc only adds, and nothing removes one
// method from a running Bus. So these series are not deleted; they are
// bounded instead, each side by what it can know:
//
//   - the served side labels a request with the method only when this process
//     registered a handler for it, and with jetStreamRPCUnregisteredMethod
//     otherwise. The envelope's MsgName comes from the peer, and before this
//     every name a peer sent became a new series. Bound: registered methods + 1.
//   - the calling side cannot know which methods exist on the callee, so each
//     Bus labels at most jetStreamRPCMethodLabelLimit distinct methods and
//     counts the rest under jetStreamRPCOtherMethod, logging the first one. The
//     call itself is unaffected. Bound: the limit + 1 (bus_rpc_pending,
//     bus_rpc_call_total, and the pendingByMethod counters).
//
// Every RPC client the framework generates calls generated method constants
// (57 across the repository today), far below the limit. 256 also keeps
// bus_rpc_call_total, with its handful of results per method, under the
// registry's per-metric cap of 2048, so the cap never drops a real method's
// series in favour of a stray one.
const (
	jetStreamRPCMethodLabelLimit   = 256
	jetStreamRPCOtherMethod        = "_other"
	jetStreamRPCUnregisteredMethod = "_unregistered"
)

// jetStreamRPCCallerMethodLabel is the label method's calling-side metrics
// carry.
func (b *Bus) jetStreamRPCCallerMethodLabel(method string) string {
	if b == nil || b.jsRPC == nil {
		return method
	}
	rpc := b.jsRPC
	if _, ok := rpc.methodLabels.Load(method); ok {
		return method
	}
	rpc.labelMu.Lock()
	defer rpc.labelMu.Unlock()
	if _, ok := rpc.methodLabels.Load(method); ok {
		return method
	}
	if rpc.methodLabelLen < jetStreamRPCMethodLabelLimit {
		rpc.methodLabels.Store(method, struct{}{})
		rpc.methodLabelLen++
		return method
	}
	if rpc.labelOverflow.CompareAndSwap(false, true) {
		slog.Warn("bus: more distinct jetstream rpc methods called than the metrics label limit; further new methods are counted under the overflow label",
			"limit", jetStreamRPCMethodLabelLimit, "label", jetStreamRPCOtherMethod, "first_overflow_method", method)
	}
	return jetStreamRPCOtherMethod
}

func (b *Bus) jetStreamRPCMethodPendingCounter(method string) *atomic.Int64 {
	if b == nil || b.jsRPC == nil {
		return &atomic.Int64{}
	}
	value, _ := b.jsRPC.pendingByMethod.LoadOrStore(method, &atomic.Int64{})
	counter, ok := value.(*atomic.Int64)
	if !ok || counter == nil {
		counter = &atomic.Int64{}
		b.jsRPC.pendingByMethod.Store(method, counter)
	}
	return counter
}

func (b *Bus) recordJetStreamRPCCall(method string, result string, reason string) {
	labels := metrics.Labels{
		"transport": "jetstream",
		"method":    method,
		"result":    result,
	}
	if reason != "" {
		labels["reason"] = reason
	}
	metrics.IncCounter("bus_rpc_call_total", labels, 1)
}

func (b *Bus) recordJetStreamRPCRequest(method string, result string) {
	metrics.IncCounter("bus_rpc_request_total", metrics.Labels{
		"transport": "jetstream",
		"method":    method,
		"result":    result,
	}, 1)
}

func jetStreamRPCContextError(ctx context.Context) error {
	if ctx == nil {
		return fnats.ErrCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fnats.ErrTimeout
	}
	return fnats.ErrCancelled
}
