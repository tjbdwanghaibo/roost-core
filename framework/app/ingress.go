package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// IngressCloser 由拥有外部输入的 Mod / Service 实现，或经 AddIngress 登记（运行中才建立的输入，例如 Service.Init 里
// 订阅的 JetStream 消费者）。外部输入是别的进程 / 客户端能主动送进来、会触发本进程业务处理的东西：总线 RPC 与模块消息、
// 消费者投递、运维命令。
//
// App 停机的第一步（service.stopping 之后、Service.Shutdown 与任何 Mod 停止之前）并发调用全部 CloseIngress：
// 任何外部输入在它用到的依赖停止之前已经关闭准入并排空（App 生命周期方案，docs/maintenance/FEATURE-2026-11-04-app-lifecycle.md）。
//
// 契约（三步停机的前两步）：
//   - 关准入，幂等：之后到达的输入不进处理，答复确定的“没执行、可重试”（总线 RPC 答 errcode.CodeRetryable 或 no
//     responders；消费者的消息留在流里交给别的实例或下次启动）；
//   - 在 ctx 内等已经接受的处理完；ctx 先结束返回 ctx 错误，用新 ctx 再调用继续等同一次排空。App 把 ctx 错误当作
//     停机不完整：不调 Service.Shutdown、不停 Mod、不释放单实例锁；
//   - 只停“接收”：出向能力（发消息、调别的服务）与对象本身的资源留到它自己的 Stop。
//
// 依赖序已经保证的输入（玩家 TCP、网关、实体定时器：它们的 Mod 依赖自己用到的 Mod，先停）不必实现。
type IngressCloser interface {
	CloseIngress(ctx context.Context) error
}

// IngressFunc 把一个函数当作 IngressCloser。
type IngressFunc func(ctx context.Context) error

// CloseIngress 调用 f。
func (f IngressFunc) CloseIngress(ctx context.Context) error { return f(ctx) }

// errIngressClosed 是停机第一步开始之后再 AddIngress 得到的错误：这时登记的输入不会再被关闭。
var errIngressClosed = errors.New("app: inputs are closed: the App is stopping")

type namedIngress struct {
	name   string
	closer IngressCloser
}

// AddIngress 登记一个运行中建立的外部输入，App 在停机第一步关闭它（见 IngressCloser）。name 用于日志，同一个 Registry 里
// 不能重复。停机第一步开始之后再登记返回错误：调用方应当撤掉刚建立的输入（App 不会再关它）。
func AddIngress(r *Registry, name string, closer IngressCloser) error {
	if r == nil || closer == nil || name == "" {
		return fmt.Errorf("app: AddIngress needs a registry, a name and a closer")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ingressClosed {
		return fmt.Errorf("%w: input %q", errIngressClosed, name)
	}
	for _, existing := range r.ingress {
		if existing.name == name {
			return fmt.Errorf("app: input %q already registered", name)
		}
	}
	r.ingress = append(r.ingress, namedIngress{name: name, closer: closer})
	return nil
}

// closingIngress 标记停机第一步已开始并返回登记的输入（之后 AddIngress 失败）。可重复调用：启动被打断时 App 关两次。
func (r *Registry) closingIngress() []namedIngress {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingressClosed = true
	return append([]namedIngress(nil), r.ingress...)
}

// closeIngress 是停机第一步：并发调用已 Start 的 Mod、Init 已返回的 Service（svc 非 nil 时）与 Registry 登记的全部
// IngressCloser，在 ctx 内等它们都返回。并发是为了让各入口几乎同时关准入，排空时间取最长的一个而不是相加。
//
// complete 为 false 表示有输入没在 ctx 内排空（返回了 ctx 错误、或到期还没返回）：在途处理仍可能用着依赖，调用方
// 不能接着停业务与 Mod。其他错误并入 err，不影响 complete。
func (run *appRun) closeIngress(ctx context.Context, registry *Registry, svc Service) (complete bool, err error) {
	var closers []namedIngress
	for _, mod := range append(append([]Mod(nil), run.startedSharedMods...), run.startedServiceMods...) {
		if closer, ok := mod.(IngressCloser); ok {
			closers = append(closers, namedIngress{name: "mod " + string(mod.Name()), closer: closer})
		}
	}
	if closer, ok := svc.(IngressCloser); ok && svc != nil {
		closers = append(closers, namedIngress{name: "service " + string(svc.Name()), closer: closer})
	}
	closers = append(closers, registry.closingIngress()...)
	if len(closers) == 0 {
		return true, nil
	}

	started := time.Now()
	slog.Info("closing inputs", "count", len(closers))
	type outcome struct {
		name     string
		err      error
		panicked bool
	}
	results := make(chan outcome, len(closers))
	for _, input := range closers {
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					results <- outcome{name: input.name, err: fmt.Errorf("panic: %v", recovered), panicked: true}
				}
			}()
			results <- outcome{name: input.name, err: input.closer.CloseIngress(ctx)}
		}()
	}
	complete = true
	for pending := len(closers); pending > 0; pending-- {
		select {
		case result := <-results:
			if result.err == nil {
				continue
			}
			err = errors.Join(err, fmt.Errorf("app: close input %s: %w", result.name, result.err))
			if result.panicked || stopIncomplete(result.err) {
				// panic 时不知道排空到了哪里，与没排空同样处理。
				complete = false
			}
		case <-ctx.Done():
			// 还没返回的输入可能不配合 ctx：App 杀不掉它，按停机不完整返回，它留在自己的 goroutine 里继续。
			return false, errors.Join(err, fmt.Errorf("app: %d input(s) did not drain within the shutdown budget: %w", pending, ctx.Err()))
		}
	}
	if err != nil {
		slog.Error("closing inputs failed", "duration", time.Since(started), "complete", complete, "err", err)
	} else {
		slog.Info("inputs closed", "duration", time.Since(started))
	}
	return complete, err
}
