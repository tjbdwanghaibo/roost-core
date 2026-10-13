package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// AssemblyConfig 是进程解析完配置后构建 DataEngine 所需的全部参数；
// wiring 的 Mod 从配置声明填充，测试直接填写。
type AssemblyConfig struct {
	Mongo        MongoStoreConfig
	WAL          nestwal.Options
	Projector    ProjectorOptions
	Outbox       OutboxWorkerOptions
	EffectPrefix string
	EffectStream fnats.JetStreamConfig
	Pipelined    PipelinedRuntimeConfig
}

// ValidateReceiptRetention 校验事务去重标记的有效 TTL 大于 WAL 未确认年龄告警窗口。
// Mongo 的 TTL 按整秒存储，必须按相同精度比较。MaxUnackedAge 只是健康阈值，
// 不是日志失效时间；停机超过 TTL 的旧 WAL 仍须人工核对，不能据此宣称可安全重放。
func (cfg AssemblyConfig) ValidateReceiptRetention() error {
	ttl := cfg.Mongo.TransactionReceiptTTL
	if ttl <= 0 {
		ttl = defaultReceiptTTL
	}
	seconds, err := ttlSeconds(ttl)
	if err != nil {
		return fmt.Errorf("dataengine.transaction_receipt_ttl: %w", err)
	}
	window := cfg.WAL.MaxUnackedAge
	if window <= 0 {
		window = nestwal.DefaultOptions("").MaxUnackedAge
	}
	if time.Duration(seconds)*time.Second <= window {
		return fmt.Errorf("dataengine.transaction_receipt_ttl (%s, whole seconds) must exceed dataengine.wal.max_unacked_age (%s)", time.Duration(seconds)*time.Second, window)
	}
	return nil
}

// AssemblyDeps 是 DataEngine 依赖的外部能力。启用 Remote 投影时 RemoteStore 与
// RemoteManager 必须同时设置，且 RemoteManager 须实现 entity.RemoteCommitApplier。
type AssemblyDeps struct {
	Mongo         fmongo.IMongo
	JetStream     fnats.IJetStream
	Access        *entity.ManagerAccess
	RemoteStore   RemoteProjectionStore
	RemoteManager entity.IRemoteEntityManager
	OnFatal       func(error)
	// LocalExecutor 把需要 Entity 锁的框架步骤交给业务池（wiring 转发 Nest 绑定的 RunLocal）。
	// 为 nil 时驱逐等本地步骤按 entity.RunLocal 的约定就地执行。
	LocalExecutor func(func()) error
}

// Assembly 负责 DataEngine 的构建顺序与失败回滚：Assemble 时构建 Store；Start 时
// 依次打开 WAL、Projector、Outbox 与 Runtime，后一步失败时关闭已打开的组件。
// wiring 的 Mod 只转发生命周期调用并读取 Runtime()。
type Assembly struct {
	Store *MongoStore

	deps AssemblyDeps
	cfg  AssemblyConfig

	lifecycleGate operationGate
	runtimeMu     sync.RWMutex
	runtime       *Runtime
}

// Assemble 校验依赖并构建 MongoStore，配置了 Remote 投影时一并绑定；此时不打开、不写入任何资源。
func Assemble(deps AssemblyDeps, cfg AssemblyConfig) (*Assembly, error) {
	if err := cfg.ValidateReceiptRetention(); err != nil {
		return nil, err
	}
	if deps.Access == nil || deps.Access.Manager() == nil {
		return nil, errors.New("dataengine: service-scoped entity access is required")
	}
	if deps.Mongo == nil {
		return nil, errors.New("dataengine: mongo client is required")
	}
	if deps.JetStream == nil {
		return nil, errors.New("dataengine: jetstream client is required")
	}
	if (deps.RemoteStore == nil) != (deps.RemoteManager == nil) {
		return nil, errors.New("dataengine: remote projection needs both the remote manager and its atomic store")
	}
	store, err := NewMongoStore(deps.Mongo, cfg.Mongo)
	if err != nil {
		return nil, err
	}
	if deps.RemoteManager != nil {
		applier, ok := deps.RemoteManager.(entity.RemoteCommitApplier)
		if !ok {
			return nil, errors.New("dataengine: remote manager has no commit applier")
		}
		if err := store.SetRemoteProjection(deps.RemoteStore, applier); err != nil {
			return nil, err
		}
	}
	return &Assembly{Store: store, deps: deps, cfg: cfg}, nil
}

// Start 确保 Mongo 基础设施与 effect 流存在，打开 WAL，再按依赖顺序启动 Projector、
// Outbox 与 Runtime。任一步失败即停止已打开的组件；ctx 同时约束启动和失败清理，
// 清理未完成时 Runtime() 保留剩余资源，交给后续 Shutdown 继续回收。
func (a *Assembly) Start(ctx context.Context) (err error) {
	if a == nil || a.Store == nil {
		return errors.New("dataengine: not assembled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.lifecycleGate.acquire(ctx); err != nil {
		return err
	}
	defer a.lifecycleGate.release()
	if current := a.Runtime(); current != nil {
		if current.Ready() {
			return nil
		}
		return fmt.Errorf("%w: complete Assembly.Shutdown before restarting", ErrRuntimeStopped)
	}
	if err := a.Store.EnsureInfrastructure(ctx); err != nil {
		return fmt.Errorf("dataengine: ensure mongo infrastructure: %w", err)
	}
	if err := a.deps.JetStream.EnsureStream(ctx, a.cfg.EffectStream); err != nil {
		return fmt.Errorf("dataengine: ensure effect stream: %w", err)
	}
	wal, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		return err
	}
	// 先登记本次尝试拥有的资源。失败清理同样受 ctx 限制；未完成时保留
	// Runtime 给 Shutdown 重试，不能在 deadline 到期后丢失 WAL 的所有权。
	owned := &Runtime{WAL: wal}
	defer func() {
		if err == nil {
			return
		}
		if cleanupErr := owned.stop(ctx, false); cleanupErr != nil {
			a.runtimeMu.Lock()
			a.runtime = owned
			a.runtimeMu.Unlock()
			err = errors.Join(err, fmt.Errorf("dataengine: startup cleanup incomplete: %w", cleanupErr))
		}
	}()
	projector, err := NewProjector(wal, a.Store, a.cfg.Projector)
	if err != nil {
		return err
	}
	owned.Projector = projector
	if a.deps.LocalExecutor != nil {
		projector.BindLocalExecutor(a.deps.LocalExecutor)
	}
	outboxStore, err := NewMongoOutboxStore(a.Store)
	if err != nil {
		return err
	}
	publisher := &jetStreamOutboxPublisher{client: a.deps.JetStream, prefix: a.cfg.EffectPrefix}
	outbox, err := NewOutboxWorker(outboxStore, publisher, a.cfg.Outbox)
	if err != nil {
		return err
	}
	owned.Outbox = outbox
	runtime, err := NewRuntime(a.Store, wal, projector, outbox, a.deps.Access, a.deps.RemoteManager, a.deps.OnFatal, a.cfg.Pipelined)
	if err != nil {
		return err
	}
	owned = runtime
	if err := runtime.Start(ctx); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	a.runtime = runtime
	a.runtimeMu.Unlock()
	return nil
}

// Runtime 返回运行中的 Runtime，或启动失败后等待清理的未就绪 Runtime；
// Start 之前与 Shutdown 成功之后为 nil。
func (a *Assembly) Runtime() *Runtime {
	if a == nil {
		return nil
	}
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	return a.runtime
}

// Shutdown 停止 Runtime（先 Projector 后 Outbox），全部完成后才忘掉它；Start 之前或
// 已完成关闭后调用不做任何事。
//
// ctx 在组件仍在排空时到期，Shutdown 返回错误并保留 Runtime，重试会等同一批组件。
// 若无论结果都丢弃 Runtime，重试会拿到 nil 并报告成功，而 outbox worker 仍在运行，
// 调用方随后释放底层连接就会从活着的 worker 下面抽走它们。Runtime.Shutdown 记得哪些
// 组件已停止，重试不会重复 Flush 或 Close。
// 历史：RR-20260909-03。
func (a *Assembly) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.lifecycleGate.acquire(ctx); err != nil {
		return err
	}
	defer a.lifecycleGate.release()
	a.runtimeMu.RLock()
	runtime := a.runtime
	a.runtimeMu.RUnlock()
	if runtime == nil {
		return nil
	}
	if err := runtime.Shutdown(ctx); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	if a.runtime == runtime {
		a.runtime = nil
	}
	a.runtimeMu.Unlock()
	return nil
}

// jetStreamOutboxPublisher publishes staged effects to JetStream under the
// effect prefix, with the effect ID as the broker deduplication key.
type jetStreamOutboxPublisher struct {
	client fnats.IJetStream
	prefix string
}

func (publisher *jetStreamOutboxPublisher) Publish(ctx context.Context, item OutboxItem) error {
	if publisher == nil || publisher.client == nil || item.Effect.ID == "" || item.Effect.Topic == "" {
		return errors.New("dataengine outbox: invalid JetStream publish")
	}
	payload, err := json.Marshal(nestwal.EffectEnvelope{
		TransactionID: item.TransactionID, EffectID: item.Effect.ID, Topic: item.Effect.Topic,
		Key: item.Effect.Key, Headers: item.Effect.Headers, Payload: item.Effect.Payload,
	})
	if err != nil {
		return err
	}
	_, err = publisher.client.Publish(ctx, publisher.prefix+"."+strings.Trim(item.Effect.Topic, "."), payload, fnats.JetStreamPublishOptions{MsgID: item.Effect.ID})
	return err
}
