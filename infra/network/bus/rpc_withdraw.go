package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

// errRPCWithdrawn 是撤下之后到达的请求得到的答复原因（包在 errcode.ErrRetryable 里，调用方看到 CodeRetryable）。
var errRPCWithdrawn = errors.New("bus: rpc handler withdrawn; this instance is stopping")

// RPCWithdrawer 是 WithdrawRpc 的能力接口：业务从注册表拿到的是 IBus，用类型断言取它（测试替身可以不实现）。
type RPCWithdrawer interface {
	WithdrawRpc(ctx context.Context, methods ...string) error
}

var _ RPCWithdrawer = (*Bus)(nil)

// WithdrawRpc 撤下本实例上 methods 的 RPC handler，是注册它们的 Mod 停止时的第一步：App 按依赖逆序停 Mod，
// 注册 handler 的 Mod 先于它依赖的 Mod（Nest、Remote 等）停止，撤下之后这些依赖才停，不会有请求在它们停了之后
// 才进 handler（联盟服务冒烟里 handler 一直挂到 nats Mod 停止，停机窗口里的请求进了已停的 Nest，答复成了
// “结果未知”）。三步：
//
//  1. 关准入：之后到达本实例的这些方法的请求不进 handler，答复 errcode.ErrRetryable（确定没有执行，调用方用同一
//     RequestID 重发；servicerpc.Classify 归为可重试）。
//  2. 撤订阅：轻量 RPC 排空并退掉服务级队列组与本 sid 实例主题的订阅（排空把服务端处理退订之前已发出的请求照常
//     交给上一步的答复，不丢成超时），之后不指定 sid 的 Call 落到别的实例，发往本 sid 的得到 no responders。
//     JetStream RPC 停掉这个方法的服务级 consumer（共享 durable 的投递交给别的实例）；本实例的 consumer 保留到
//     Bus 停止，期间投递来的请求都按第 1 步答复——JetStream 没有 no responders，留着它才能给调用方确定的答复。
//  3. 在 ctx 内等已经进 handler 的请求返回、答复发出。
//
// ctx 先结束返回 ctx 错误，准入保持关闭，还没退掉的订阅与在途计数都保留，用新 ctx 再调用继续同一次撤下；返回 nil
// 之后这些方法不再有 handler 在跑。重复调用返回 nil。撤下之后不能重新注册（重启实例）。Bus 停止中或已停止时
// 订阅归停止流程处理，这里只等在途。methods 里有本实例没注册的方法时返回错误、不做任何事。
func (b *Bus) WithdrawRpc(ctx context.Context, methods ...string) error {
	if b == nil {
		return fmt.Errorf("bus: nil bus")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	calls := make([]*operation.Lifetime, 0, len(methods))
	b.mu.RLock()
	for _, method := range methods {
		if _, ok := b.rpcHandlers[method]; !ok {
			b.mu.RUnlock()
			return fmt.Errorf("bus: rpc method %s has no handler on this instance", method)
		}
		if lifetime := b.rpcCalls[method]; lifetime != nil {
			calls = append(calls, lifetime)
		}
	}
	b.mu.RUnlock()
	return b.withdrawRPC(ctx, methods, calls)
}

// withdrawRPC 是 WithdrawRpc 的三步（methods 已核对过、calls 是它们的在途计数）。
func (b *Bus) withdrawRPC(ctx context.Context, methods []string, calls []*operation.Lifetime) error {
	for _, lifetime := range calls {
		lifetime.Stop()
	}
	if err := b.withdrawRPCSubscriptions(ctx, methods); err != nil {
		return err
	}
	for _, lifetime := range calls {
		if err := lifetime.Wait(ctx); err != nil {
			return fmt.Errorf("bus: withdraw rpc: handlers still running: %w", err)
		}
	}
	return nil
}

// withdrawRPCSubscriptions 是 WithdrawRpc 的第 2 步。轻量 RPC 的订阅并发排空（每个排空至少一次往返），排空成功的
// 才从记录与 b.subs 里拿掉，失败或超时的留给重试。
func (b *Bus) withdrawRPCSubscriptions(ctx context.Context, methods []string) error {
	if b.jetStreamRPCEnabled() {
		b.stopJetStreamRPCServiceConsumers(methods)
		return nil
	}
	b.lifeMu.Lock()
	if b.stopping || b.stopped {
		b.lifeMu.Unlock()
		return nil
	}
	var subs []nats.ISubscription
	for _, method := range methods {
		if owned := b.rpcSubs[method]; owned != nil {
			for _, sub := range []nats.ISubscription{owned.queue, owned.instance} {
				if sub != nil {
					subs = append(subs, sub)
				}
			}
		}
	}
	b.lifeMu.Unlock()

	errs := make([]error, len(subs))
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = drainRPCSubscription(ctx, sub)
		}()
	}
	wg.Wait()

	b.lifeMu.Lock()
	for i, sub := range subs {
		if errs[i] != nil {
			continue
		}
		for _, method := range methods {
			if owned := b.rpcSubs[method]; owned != nil {
				if owned.queue == sub {
					owned.queue = nil
				}
				if owned.instance == sub {
					owned.instance = nil
				}
			}
		}
		b.disownSubscriptionLocked(sub)
	}
	b.lifeMu.Unlock()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("bus: withdraw rpc subscriptions: %w", err)
	}
	slog.Info("bus: withdrew rpc handlers", "methods", methods, "svc", b.cfg.SvcType, "sid", b.cfg.Sid)
	return nil
}

// drainRPCSubscription 排空支持排空的订阅（driver 的订阅都支持）；不支持的（测试替身）退订。
func drainRPCSubscription(ctx context.Context, sub nats.ISubscription) error {
	if drainer, ok := sub.(interface{ DrainContext(context.Context) error }); ok {
		return drainer.DrainContext(ctx)
	}
	if !sub.IsValid() {
		return nil
	}
	return sub.Unsubscribe()
}

// disownSubscriptionLocked 把 sub 从 b.subs 里拿掉：停止时不再退订它一次。调用方持有 lifeMu。
func (b *Bus) disownSubscriptionLocked(sub nats.ISubscription) {
	for i, owned := range b.subs {
		if owned == sub {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			return
		}
	}
}

// rpcMethodCalls 返回方法的 handler 在途计数；没有（测试直接写 rpcHandlers）时为 nil，按一直准入处理。
func (b *Bus) rpcMethodCalls(method string) *operation.Lifetime {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.rpcCalls[method]
}

// withdrawnRPCError 是撤下之后到达的请求的答复：确定没有执行、可重试。
func withdrawnRPCError(method string) error {
	return errcode.Wrap(errcode.ErrRetryable, fmt.Errorf("%w: %s", errRPCWithdrawn, method))
}

// refuseWithdrawnRPC 给撤下之后到达的轻量 RPC 请求答复“可重试”。
func (b *Bus) refuseWithdrawnRPC(replySubject, method string) {
	if replySubject == "" || b.rpc == nil {
		return
	}
	data, err := encodeRPCFailure(b.codec, withdrawnRPCError(method))
	if err != nil {
		slog.Error("bus: marshal withdrawn rpc refusal failed", "method", method, "err", err)
		return
	}
	if err := b.rpc.Reply(replySubject, data); err != nil {
		slog.Error("bus: reply withdrawn rpc refusal failed", "method", method, "err", err)
	}
}

// CloseIngress 关闭这个总线的全部外部输入并在 ctx 内排空，是 App 停机的第一步（app.IngressCloser，由拥有总线的
// NatsMod 转调；App 生命周期方案 docs/maintenance/FEATURE-2026-11-04-app-lifecycle.md）：
//
//  1. 之后 HandleRpc 拒绝：这时注册的 handler 不会再被关闭。
//  2. 对全部已注册的 RPC 方法做 WithdrawRpc 的三步：之后到达的请求答 errcode.CodeRetryable 或 no responders（JetStream
//     RPC 的实例 consumer 留到 Bus 停止、期间答可重试），等已进 handler 的答复完。注册它们的是生成的 owner Mod、生成的
//     Server（Service.Init）还是内置服务，都由这里统一撤下，各自不必再撤。
//  3. 排空并退掉四个基础主题的订阅（模块消息的入口），然后关模块消息的准入、等已接收的（在跑的与在调度池里排队的）处理完。
//     之后的模块消息不进 handler：核心 NATS 不持久，等同于进程退出之后发来的消息（可靠模式下进死信）。
//
// 只停“接收”：Send / Call、JetStream RPC 的应答 consumer 照常，直到 Bus 停止（Service.Shutdown 与之后停止的 Mod 还要
// 发消息、调别的服务）。ctx 先结束返回 ctx 错误、准入保持关闭，用新 ctx 再调用继续等；返回 nil 之后不再有输入在处理。
// 重复调用返回 nil。
func (b *Bus) CloseIngress(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.lifeMu.Lock()
	b.ingressClosed = true
	b.lifeMu.Unlock()

	// 置位之后收集：之后的 HandleRpc 已被拒绝，这里拿到的就是全部（与置位并发、订阅还没完成的那一个会自己撤销登记）。
	b.mu.RLock()
	methods := make([]string, 0, len(b.rpcHandlers))
	calls := make([]*operation.Lifetime, 0, len(b.rpcHandlers))
	for method := range b.rpcHandlers {
		methods = append(methods, method)
		if lifetime := b.rpcCalls[method]; lifetime != nil {
			calls = append(calls, lifetime)
		}
	}
	b.mu.RUnlock()
	if len(methods) > 0 {
		if err := b.withdrawRPC(ctx, methods, calls); err != nil {
			return fmt.Errorf("bus: close inputs: %w", err)
		}
	}
	if err := b.drainMessageSubscriptions(ctx); err != nil {
		return fmt.Errorf("bus: close inputs: %w", err)
	}
	b.msgCalls.Stop()
	if err := b.msgCalls.Wait(ctx); err != nil {
		return fmt.Errorf("bus: close inputs: module messages still being handled: %w", err)
	}
	return nil
}

// drainMessageSubscriptions 并发排空四个基础主题的订阅，排空成功的从记录与 b.subs 里拿掉（Bus 停止时不再退订），失败或
// 超时的留给重试。Bus 停止中或已停止时订阅归停止流程处理。
func (b *Bus) drainMessageSubscriptions(ctx context.Context) error {
	b.lifeMu.Lock()
	if b.stopping || b.stopped {
		b.lifeMu.Unlock()
		return nil
	}
	subs := append([]nats.ISubscription(nil), b.msgSubs...)
	b.lifeMu.Unlock()
	if len(subs) == 0 {
		return nil
	}
	errs := make([]error, len(subs))
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = drainRPCSubscription(ctx, sub)
		}()
	}
	wg.Wait()
	b.lifeMu.Lock()
	for i, sub := range subs {
		if errs[i] != nil {
			continue
		}
		for j, owned := range b.msgSubs {
			if owned == sub {
				b.msgSubs = append(b.msgSubs[:j], b.msgSubs[j+1:]...)
				break
			}
		}
		b.disownSubscriptionLocked(sub)
	}
	b.lifeMu.Unlock()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("drain message subscriptions: %w", err)
	}
	return nil
}
