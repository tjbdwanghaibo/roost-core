package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// AssemblyDeps 是 Remote Entity 依赖的能力。Redis 总是必需（协调锁与快照 L2）；持久所有权随提交存储。后端要么直接
// 给出，要么在 Mongo 上由 Loader 构建。
type AssemblyDeps struct {
	Redis   fredis.IRedis
	Mongo   fmongo.IMongo
	Loader  entity.IRemoteEntityLoader
	Backend entity.IRemoteEntityBackend
	OnFatal func(error)
	// Incarnation 是本进程持有的 App 单实例锁的身份：非 nil 时，第一次取某个实体的共享锁就接管同 sid
	// 上一代进程留下的锁，不等 lock_ttl。只有持有单实例锁的进程才能传；nil（singleton.enabled=false）时按 TTL 等待。
	Incarnation *ProcessIncarnation
}

// MongoBackendConfig 配置由 Loader 构建的 Mongo committer。
type MongoBackendConfig struct {
	Database       string
	TransactionTTL time.Duration
}

// Assembly 负责 Manager 的构造、依赖封存、存储初始化、outbox 恢复与复制生命周期。wiring 的 Mod 只解析配置、发布三项
// 能力并转发生命周期调用。
type Assembly struct {
	Manager     *Manager
	LockFactory fredis.IVersionedLockFactory
	AtomicStore AtomicCommitStore

	cfg *Config

	// 启停持有整个操作的所有权；等待者可取消，但不能提前回收持有者的资源。
	lifecycleMu   sync.Mutex
	operationDone chan struct{}
	started       bool
	stopping      bool
}

// ErrAssemblyStopped 表示 finalizer 已进入不可逆的停止流程；重新运行须重新 Assemble。
var ErrAssemblyStopped = errors.New("remote_entity: assembly is stopping or stopped")

// ErrRemoteManagedServerScopedDAO 表示 remote=managed 实体注册了按服选库（dbscope=sid）的 DAO。
// 与 entity.ErrRemoteManagedServerScopedDAO 是同一个值：entity.ValidateEntityRegistry 与 Remote 提交的 WAL 准入前
// 用同一哨兵报告同一规则，errors.Is 判断任选其一。
var ErrRemoteManagedServerScopedDAO = entity.ErrRemoteManagedServerScopedDAO

// Assemble 构建 Manager 及其锁工厂、快照 L2、后端与所有权存储。localSid 必须非零：它是所有权的 fence。
func Assemble(deps AssemblyDeps, cfg *Config, localSid int32, mongoCfg MongoBackendConfig) (*Assembly, error) {
	if deps.Redis == nil {
		return nil, errors.New("remote_entity: redis client is required")
	}
	if localSid == 0 {
		return nil, errors.New("remote_entity: non-zero sid is required for ownership fencing")
	}
	if cfg == nil {
		cfg = DefaultConfig()
	}
	// 快照段与只读方 NewSnapshotClient 同一套校验；正式装配总带 Redis L2。
	if err := validateSnapshotConfig(cfg, true); err != nil {
		return nil, err
	}
	if err := entity.ValidateRemoteManagedDaoScopes(entity.GetAllEntityBuilders()); err != nil {
		return nil, err
	}
	backend := deps.Backend
	if backend == nil && deps.Loader != nil {
		if deps.Mongo == nil {
			return nil, errors.New("remote_entity: mongo client is required for the Mongo storage backend")
		}
		if mongoCfg.Database == "" {
			mongoCfg.Database = "remote_entity"
		}
		storage := NewMongoCommitter(deps.Mongo, mongoCfg.Database, localSid, mongoCfg.TransactionTTL)
		built, err := NewBackend(deps.Loader, storage)
		if err != nil {
			return nil, err
		}
		backend = built
	}
	if backend == nil {
		return nil, errors.New("remote_entity: atomic storage backend is required")
	}
	atomicStore, _ := backend.(AtomicCommitStore)
	if atomicStore == nil {
		return nil, errors.New("remote_entity: backend must support caller-owned atomic transactions")
	}
	provider, ok := backend.(WriteAuthorityProvider)
	if !ok {
		return nil, errors.New("remote_entity: backend must provide durable write authority")
	}
	authority := provider.WriteAuthority()
	if authority == nil {
		return nil, errors.New("remote_entity: backend must provide durable write authority")
	}
	snapshotL2, err := NewSnapshotL2StoreFromConfig(deps.Redis, cfg)
	if err != nil {
		return nil, err
	}
	lockFactory := NewVersionedLockFactory(deps.Redis, authority)
	if deps.Incarnation != nil {
		incarnation, err := newLockIncarnation(localSid, *deps.Incarnation)
		if err != nil {
			return nil, fmt.Errorf("remote_entity: process incarnation: %w", err)
		}
		lockFactory.incarnation = incarnation
	}
	manager := NewManager(lockFactory, cfg, localSid, snapshotL2)
	if err := manager.LockFactoryError(); err != nil {
		return nil, err
	}
	if deps.OnFatal != nil {
		manager.SetFatalHandler(deps.OnFatal)
	}
	manager.SetBackend(backend)
	manager.SetOwnershipStore(authority)
	return &Assembly{Manager: manager, LockFactory: lockFactory, AtomicStore: atomicStore, cfg: cfg}, nil
}

// Start 校验并封存依赖，在 bus 上绑定并启动快照与兴趣复制，后端支持时初始化 Remote 存储，恢复 outbox 并启动
// finalizer。任何一步失败都会再停掉复制。ctx 是有界的存储与恢复调用的父 ctx（每次调用受 cfg.OpTimeout 限制）。
// 重复启动幂等；失败重试复用首次绑定的 bus，替换 bus 需要新的 Assembly。
func (a *Assembly) Start(ctx context.Context, bus fsyncbus.ISyncBus) error {
	if a == nil || a.Manager == nil {
		return errors.New("remote_entity: not assembled")
	}
	if bus == nil {
		return errors.New("remote_entity: sync bus is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer a.releaseLifecycle()
	if a.stopping {
		return ErrAssemblyStopped
	}
	if a.started {
		return nil
	}
	if err := a.Manager.ValidateDependencies(); err != nil {
		return err
	}
	// Assemble 之后注册的实体（含手写 DAO）在开始接收写入前再校验一次。
	if err := entity.ValidateRemoteManagedDaoScopes(entity.GetAllEntityBuilders()); err != nil {
		return err
	}
	// 快照复制归 SnapshotClient：第一次启动绑定 bus，失败重试只重订阅，不改动已封存的依赖。
	snapshots := a.Manager.snapshots
	if err := snapshots.Start(bus); err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			snapshots.unsubscribe()
		}
	}()
	// 同 sid 重启的 owner 兴趣表是空的（兴趣主题从 durable 游标续读，不重放已确认的兴趣）。订阅已确认，
	// 之后发出的续租一定能到达，此时请求只读方立即重新续租，推送不必等下一个续租周期；放在存储初始化与 outbox
	// 恢复之前，让续租尽早回来。失败只记日志，退化到原来的续租周期。
	if err := snapshots.requestInterestRefresh(ctx); err != nil {
		slog.Warn("remote_entity: could not ask consumers to renew their snapshot interest; pushes resume on their regular renewal",
			"sid", snapshots.consumerSID, "err", err)
	}
	a.Manager.SealDependencies()
	if initializer, ok := a.Manager.Backend().(entity.IRemoteStorageInitializer); ok {
		storageCtx, cancel := context.WithTimeout(ctx, a.cfg.OpTimeout)
		err := initializer.EnsureRemoteStorage(storageCtx)
		cancel()
		if err != nil {
			return err
		}
	}
	recoverCtx, cancel := context.WithTimeout(ctx, a.cfg.OpTimeout)
	err := a.Manager.RecoverOutbox(recoverCtx)
	cancel()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.Manager.StartFinalizer()
	started = true
	a.started = true
	return nil
}

// Stop 在 ctx 内先停 finalizer，再停快照客户端。超时时复制仍为已接受的工作保持可用，之后的 Stop 完成清理。
// 快照客户端按三步停止：关闭准入并退订，然后在 ctx 内等待复制处理与已准入的 L2 / 权威 / 兴趣调用结束；超时返回
// ctx 错误，之后的 Stop 再次等待。
func (a *Assembly) Stop(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer a.releaseLifecycle()
	// finalizer 是单次生命周期；一旦开始停止，同一个 Assembly 就不能再次启动。
	a.stopping = true
	if a.Manager != nil {
		if err := a.Manager.StopFinalizer(ctx); err != nil {
			return err
		}
	}
	if a.Manager != nil {
		if err := a.Manager.snapshots.Stop(ctx); err != nil {
			return err
		}
	}
	a.started = false
	return nil
}

func (a *Assembly) acquireLifecycle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.lifecycleMu.Lock()
		if a.operationDone == nil {
			a.operationDone = make(chan struct{})
			a.lifecycleMu.Unlock()
			return nil
		}
		done := a.operationDone
		a.lifecycleMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *Assembly) releaseLifecycle() {
	a.lifecycleMu.Lock()
	close(a.operationDone)
	a.operationDone = nil
	a.lifecycleMu.Unlock()
}
