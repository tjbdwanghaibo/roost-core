package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// AssemblyConfig 是配置解析后 saga 运行时需要的全部内容：协调器选项（布局、本进程拥有的分区、写者身份、快照与准入参数）与
// 启动中继（Nest effect 里的启动意图 → 分区 in.start）的持久消费者。由 wiring 的 Mod 填写。
type AssemblyConfig struct {
	Engine Options
	Starts NestStartConsumerConfig
	// ColdCopy 非 nil 时装配冷副本（设计 §8，选项 C-A）：每个进程都能回落读（Engine.Get / List），拥有分区的进程另跑投影
	// （ColdCopyProjector）。nil 表示关闭（缺省）。
	ColdCopy *ColdCopySettings
}

// Assembly 在构造时建立协调器并登记定义；Start 先恢复再服务（建流与桶、重放拥有的分区、开始处理输入），再订阅启动中继；
// Stop 先排空中继，再按三步停机停协调器。Kit 只解析配置、发布能力和转发生命周期。
type Assembly struct {
	Engine *Engine

	js  fnats.IJetStream
	cfg AssemblyConfig
	// coldCopy 是冷副本投影（只在冷副本开启且本进程拥有分区时存在）。
	coldCopy *ColdCopyProjector

	lifecycleMu sync.Mutex
	stateMu     sync.RWMutex
	startSub    fnats.IJetStreamSubscription
	running     bool
}

// Assemble 构建协调器并登记定义，此时还不访问 JetStream。log、raw、js 来自同一条 NATS 连接（wiring/nats 的
// nats.log、nats.raw、nats.jetstream）。
func Assemble(log fnats.JetStreamLog, raw fnats.RawClient, js fnats.IJetStream, cfg AssemblyConfig, definitions ...Definition) (*Assembly, error) {
	if log == nil || raw == nil || js == nil {
		return nil, errors.New("saga: JetStream log, raw NATS client and JetStream client are required")
	}
	var coldCopy *ColdCopy
	if cfg.ColdCopy != nil {
		settings := cfg.ColdCopy.withDefaults()
		if err := settings.validate(); err != nil {
			return nil, err
		}
		cold, err := NewColdCopy(settings.Client, settings.Database, settings.Collection)
		if err != nil {
			return nil, err
		}
		coldCopy = cold
		cfg.Engine.ColdCopy = coldCopy
	}
	engine, err := NewEngine(log, raw, cfg.Engine)
	if err != nil {
		return nil, err
	}
	for i := range definitions {
		if err := engine.Register(definitions[i]); err != nil {
			return nil, fmt.Errorf("saga: register definition: %w", err)
		}
	}
	a := &Assembly{Engine: engine, js: js, cfg: cfg}
	if coldCopy != nil && len(cfg.Engine.Owned) > 0 {
		projector, err := NewColdCopyProjector(coldCopy, js, log, engine.layout, *cfg.ColdCopy)
		if err != nil {
			return nil, err
		}
		a.coldCopy = projector
	}
	return a, nil
}

// Start 启动协调器（返回时拥有的分区都已重放完、开始处理输入；不拥有分区的进程只建好发起方一侧），再订阅启动中继。
// ctx 约束基础设施调用与重放；之后运行到 Stop。失败时已启动的部分全部停掉。只能成功一次（协调器不能重启，停了要重建）。
func (a *Assembly) Start(ctx context.Context) error {
	if a == nil || a.Engine == nil || a.js == nil {
		return errors.New("saga: not assembled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.running {
		return nil
	}
	if err := a.Engine.Start(ctx); err != nil {
		return err
	}
	// 冷副本投影在协调器之后起：流由协调器建好；在它建好 durable 之前，分区清理因为找不到它而不清理（purge）。
	if a.coldCopy != nil {
		if err := a.coldCopy.Start(ctx); err != nil {
			return errors.Join(fmt.Errorf("saga: start cold copy: %w", err), a.Engine.Stop(ctx))
		}
	}
	// 中继的回调 ctx 不随启动 ctx 结束：消费者运行到 Stop 排空。
	startSub, err := SubscribeNestStarts(context.WithoutCancel(ctx), a.js, a.cfg.Starts, a.Engine)
	if err != nil {
		var stopErr error
		if a.coldCopy != nil {
			stopErr = a.coldCopy.Stop(ctx)
		}
		stopErr = errors.Join(stopErr, a.Engine.Stop(ctx))
		return errors.Join(fmt.Errorf("saga: subscribe Nest starts: %w", err), stopErr)
	}
	a.stateMu.Lock()
	a.startSub, a.running = startSub, true
	a.stateMu.Unlock()
	return nil
}

// Stop 先排空启动中继（在 ctx 内等它关闭；超时强制停止并返回 ctx 错误，可以用新 ctx 再调用），再停冷副本投影（排空在途的
// 批），最后停协调器（三步停机，见 Engine.Stop）。重复调用返回 nil。
func (a *Assembly) Stop(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.stateMu.RLock()
	startSub := a.startSub
	a.stateMu.RUnlock()
	if startSub != nil {
		startSub.Drain()
		select {
		case <-startSub.Closed():
		case <-ctx.Done():
			startSub.Stop()
			return ctx.Err()
		}
		a.stateMu.Lock()
		a.startSub = nil
		a.stateMu.Unlock()
	}
	if a.coldCopy != nil {
		if err := a.coldCopy.Stop(ctx); err != nil {
			return err
		}
	}
	if a.Engine == nil {
		return nil
	}
	if err := a.Engine.Stop(ctx); err != nil {
		return err
	}
	a.stateMu.Lock()
	a.running = false
	a.stateMu.Unlock()
	return nil
}

// Running 报告协调器是否已启动且没有停。
func (a *Assembly) Running() bool {
	if a == nil {
		return false
	}
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.running
}

// ConsumersClosed 报告启动中继是否缺失或已退出：中继停了，经 EmitStart 提交的启动意图没人转发。
func (a *Assembly) ConsumersClosed() bool {
	if a == nil {
		return true
	}
	a.stateMu.RLock()
	startSub := a.startSub
	a.stateMu.RUnlock()
	if startSub == nil {
		return true
	}
	select {
	case <-startSub.Closed():
		return true
	default:
		return false
	}
}

// RunError 返回协调器 fail-stop 的原因（某个分区被隔离、发布结果未知超预算、流满等）；没有时返回 nil。fail-stop 同时经
// Options.Failure 报给 App 的 RuntimeFailure，进程整体停机。
func (a *Assembly) RunError() error {
	if a == nil || a.Engine == nil {
		return nil
	}
	return a.Engine.FailureCause()
}
