package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

var (
	// ErrEntityAggregateNotFound 同时满足 errors.Is(err, entity.ErrAuthorityEntityNotFound)：卸载后重载据此
	// 立即向订阅者退回 remove，而不是当作可重试的读取失败。
	ErrEntityAggregateNotFound error = aggregateNotFoundError{}
	ErrEntityAggregateCorrupt        = errors.New("dataengine repository: entity aggregate is incomplete or corrupt")
)

type aggregateNotFoundError struct{}

func (aggregateNotFoundError) Error() string {
	return "dataengine repository: entity aggregate not found"
}

func (aggregateNotFoundError) Is(target error) bool {
	return target == entity.ErrAuthorityEntityNotFound
}

// RecoveryGate 报告启动恢复是否完成；未完成时仓库拒绝冷加载。实现若还提供
// WaitEntityProjection，冷加载前会先等该实体的在途投影。
type RecoveryGate interface {
	Ready() bool
}

// entityLoadFlight 是同一实体并发冷加载共享的一次加载。
type entityLoadFlight struct {
	done  chan struct{}
	value entity.IThreadSafeEntity
	err   error
}

// EntityRepository 从 Store 冷加载完整实体聚合并发布到 EntityManager（entity.AggregateLoader）。
type EntityRepository struct {
	manager *entity.EntityManager
	store   coredata.Store
	gate    RecoveryGate

	flightMu sync.Mutex
	flights  map[int64]*entityLoadFlight

	hookMu      sync.RWMutex
	nextHookID  uint64
	loadedHooks map[uint64]func(entity.IThreadSafeEntity)
}

// OnEntityLoaded 注册在聚合从 Mongo 加载并发布之后调用的回调，与发布在同一次本地执行里
// （Nest 慢阶段下即快池续行）。用途是让依附在旧对象上的协作者接上重载出来的新对象：
// 被跳过的原生步骤驱逐实体后，Sync 用它把原订阅者重新绑定到新对象并强制全量。
// 回调不能阻塞、不能做 I/O；返回的函数注销回调。
func (repository *EntityRepository) OnEntityLoaded(hook func(entity.IThreadSafeEntity)) func() {
	if repository == nil || hook == nil {
		return func() {}
	}
	repository.hookMu.Lock()
	defer repository.hookMu.Unlock()
	if repository.loadedHooks == nil {
		repository.loadedHooks = make(map[uint64]func(entity.IThreadSafeEntity))
	}
	repository.nextHookID++
	id := repository.nextHookID
	repository.loadedHooks[id] = hook
	return func() {
		repository.hookMu.Lock()
		delete(repository.loadedHooks, id)
		repository.hookMu.Unlock()
	}
}

func (repository *EntityRepository) runLoadedHooks(loaded entity.IThreadSafeEntity) {
	repository.hookMu.RLock()
	hooks := make([]func(entity.IThreadSafeEntity), 0, len(repository.loadedHooks))
	for _, hook := range repository.loadedHooks {
		hooks = append(hooks, hook)
	}
	repository.hookMu.RUnlock()
	for _, hook := range hooks {
		// 实体已经发布；回调失败只影响它自己的协作者，不能把一次成功的加载变成失败。
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Error("dataengine repository: entity loaded hook panicked", "entity", loaded.ID(), "panic", recovered)
				}
			}()
			hook(loaded)
		}()
	}
}

// NewEntityRepository 创建实体仓库；manager 与 store 必填，gate 为 nil 时不检查恢复状态。
func NewEntityRepository(manager *entity.EntityManager, store coredata.Store, gate RecoveryGate) (*EntityRepository, error) {
	if manager == nil || store == nil {
		return nil, errors.New("dataengine repository: entity manager and store are required")
	}
	return &EntityRepository{
		manager: manager, store: store, gate: gate,
		flights: make(map[int64]*entityLoadFlight),
	}, nil
}

// LoadEntity 先复用已加载实体；冷加载等待恢复与投影屏障，完整聚合 schema 校验通过后才发布。
// 同一实体共享首个调用者发起的加载；其他等待者取消只退出自身等待，不取消该次共享加载。
// 冷加载包含 I/O，不允许从快池进入；初始化和发布经 RunLocal 返回本地执行阶段。
func (repository *EntityRepository) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	if repository == nil || repository.manager == nil || repository.store == nil {
		return nil, coredata.ErrStoreRequired
	}
	if repository.gate != nil && !repository.gate.Ready() {
		return nil, coredata.ErrRecoveryIncomplete
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fullID, err := entity.NormalizeFullID(id, kind)
	if err != nil {
		return nil, err
	}
	if loaded := repository.manager.Get(fullID); loaded != nil {
		return loaded, nil
	}
	if err := fctx.BlockingError("dataengine.EntityRepository.LoadEntity"); err != nil {
		panic(fmt.Errorf("%w: entity %d: %w", entity.ErrColdLoadInLogic, fullID, err))
	}
	if entity.LoadedEntitiesOnly(ctx) {
		return nil, fmt.Errorf("%w: entity %d", entity.ErrColdLoadInLogic, fullID)
	}

	repository.flightMu.Lock()
	if flight := repository.flights[fullID]; flight != nil {
		repository.flightMu.Unlock()
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &entityLoadFlight{done: make(chan struct{})}
	repository.flights[fullID] = flight
	repository.flightMu.Unlock()

	// 结束 flight 必须放在 defer 里（与 entity.ManagerAccess 相同）：loadAggregate 中的
	// 解码器或 OnInitFinish panic 时，否则没人关闭 flight，之后对该聚合的每次加载都会
	// 等到各自 ctx 到期。仓库可被直接调用、不只经过 Nest，Nest 边界的 recover 覆盖不到这里。
	settled := false
	defer func() {
		if !settled {
			flight.value, flight.err = nil, fmt.Errorf(
				"entity repository: loading aggregate %d panicked: %v", fullID, recover())
		}
		repository.flightMu.Lock()
		delete(repository.flights, fullID)
		close(flight.done)
		repository.flightMu.Unlock()
		if !settled {
			panic(flight.err)
		}
	}()

	if gate, ok := repository.gate.(interface {
		WaitEntityProjection(context.Context, int64) error
	}); ok {
		if err := gate.WaitEntityProjection(ctx, fullID); err != nil {
			flight.err = err
			settled = true
			return nil, err
		}
	}
	flight.value, flight.err = repository.loadAggregate(ctx, fullID, kind)
	settled = true
	return flight.value, flight.err
}

// loadedDAO 是一致性读取得到的一个 DAO 文档及其已解出的持久化 payload。
type loadedDAO struct {
	dao     entity.DaoInterface
	doc     coredata.RawDocument
	payload []byte
	schema  uint32
}

// loadAggregate 冷加载一个聚合：一致性读取全部 DAO → 校验 schema → 水合 → 回到本地执行阶段发布。
func (repository *EntityRepository) loadAggregate(ctx context.Context, fullID int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	if loaded := repository.manager.Get(fullID); loaded != nil {
		return loaded, nil
	}
	builder := entity.GetEntityBuilderParam(kind)
	if builder == nil {
		return nil, fmt.Errorf("dataengine repository: no builder for entity kind %d", kind)
	}
	if builder.NoPersist || len(builder.DaoBuilders) == 0 {
		return nil, fmt.Errorf("%w: entity kind %d has no persistent DAO", ErrEntityAggregateNotFound, kind)
	}

	// 先验证完整聚合的 schema，任何不匹配都不能水合、发布或触发写回。
	loaded, remoteVector, err := repository.readAggregate(ctx, builder, fullID)
	if err != nil {
		return nil, err
	}
	if err := checkAggregateSchemas(loaded, fullID); err != nil {
		return nil, err
	}
	// 所有 DAO 校验并恢复版本后才发布 Entity，避免外部看到半加载状态。
	daos, err := hydrateAggregate(loaded, fullID)
	if err != nil {
		return nil, err
	}
	param := &entity.EntityCreateParam{
		IsCreate: false, Category: builder.Category, Kind: kind, Id: fullID,
		Dao: daos, Lifetime: builder.Lifetime,
	}
	// 是否托管看 kind 在注册表里的实际策略，不看 builder 自带的 RemotePolicy：kind 定义声明 managed、手写 builder 省略
	// 策略时注册表按“部分重复声明”接受，全部 Remote 路径都按托管处理，这里也必须恢复版本信封。
	// 历史：RR-20260926-60、RR-20260926-71。
	if entity.GetEntityKindRemotePolicy(kind).RemoteManaged() {
		if remoteVector.StateVersion == 0 {
			return nil, fmt.Errorf("%w: remote entity %d has no version envelope", ErrEntityAggregateCorrupt, fullID)
		}
		param.RemoteRestore = &remoteVector
	}
	var created entity.IThreadSafeEntity
	var createErr error
	// 冷加载 I/O 在慢池；初始化回调和发布 Entity 回到快池。
	dispatchErr := entity.RunLocal(ctx, func() {
		if created, createErr = repository.manager.Create(param); createErr == nil {
			repository.runLoadedHooks(created)
		}
	})
	err = errors.Join(createErr, dispatchErr)
	if err != nil {
		if errors.Is(err, entity.ErrEntityExists) {
			if existing := repository.manager.Get(fullID); existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return created, nil
}

// checkAggregateSchemas 在水合前校验每个 DAO 的存储 schema 等于当前运行时 schema；
// 任何不匹配都不能水合、发布或触发写回。
func checkAggregateSchemas(loaded []loadedDAO, fullID int64) error {
	for _, item := range loaded {
		descriptor, ok := item.dao.(coredata.Descriptor)
		if !ok {
			return fmt.Errorf("%w: resource=%s does not implement Descriptor", ErrEntityAggregateCorrupt, item.dao.CollName())
		}
		if descriptor.SchemaVersion() != item.schema {
			return fmt.Errorf("%w: resource=%s entity=%d stored_schema=%d target_schema=%d", coredata.ErrSchemaMismatch, item.dao.CollName(), fullID, item.schema, descriptor.SchemaVersion())
		}
	}
	return nil
}

// hydrateAggregate 把 payload 与持久版本恢复进各 DAO，并确认解码出的身份属于本实体。
func hydrateAggregate(loaded []loadedDAO, fullID int64) (map[string]entity.DaoInterface, error) {
	daos := make(map[string]entity.DaoInterface, len(loaded))
	for _, item := range loaded {
		hydrator, ok := item.dao.(entity.PersistedDaoLoader)
		if !ok {
			return nil, fmt.Errorf("%w: resource=%s does not implement PersistedDaoLoader", ErrEntityAggregateCorrupt, item.dao.CollName())
		}
		if err := hydrator.RestorePersisted(item.payload, item.schema, item.doc.Version); err != nil {
			return nil, fmt.Errorf("dataengine repository: restore %s/%d: %w", item.dao.CollName(), fullID, err)
		}
		if item.dao.Id() != fullID {
			return nil, fmt.Errorf("%w: resource=%s decoded id=%d want=%d", ErrEntityAggregateCorrupt, item.dao.CollName(), item.dao.Id(), fullID)
		}
		daos[item.dao.CollName()] = item.dao
	}
	return daos, nil
}

// readAggregate 在一次一致性读里读取聚合的全部 DAO 文档：全部缺失或全部墓碑视为不存在，
// 部分存在视为损坏；同时取版本最高的 Remote 版本信封。
func (repository *EntityRepository) readAggregate(ctx context.Context, builder *entity.EntityBuilderParam, fullID int64) ([]loadedDAO, entity.RemoteVersionVector, error) {
	var loaded []loadedDAO
	var remoteVector entity.RemoteVersionVector
	err := repository.store.ReadConsistent(ctx, func(readCtx context.Context) error {
		// ReadConsistent 运行在 ISession.WithTransaction 里，驱动遇到
		// TransientTransactionError 或提交结果未知时会重调本回调。所有累加量必须在这里
		// 重置：跨尝试保留 loaded 会让第二次尝试撞上重复资源检查，把健康数据报成损坏聚合。
		loaded = make([]loadedDAO, 0, len(builder.DaoBuilders))
		remoteVector = entity.RemoteVersionVector{}
		missing, tombstones := 0, 0
		for index, buildDAO := range builder.DaoBuilders {
			if buildDAO == nil {
				return fmt.Errorf("%w: DAO builder %d is nil", ErrEntityAggregateCorrupt, index)
			}
			dao := buildDAO()
			if dao == nil || dao.CollName() == "" {
				return fmt.Errorf("%w: DAO builder %d returned invalid DAO", ErrEntityAggregateCorrupt, index)
			}
			// 先绑定聚合身份，恢复时拒绝属于其他 Entity 的 payload。
			dao.SetId(fullID)
			for _, existing := range loaded {
				if existing.dao.CollName() == dao.CollName() {
					return fmt.Errorf("%w: duplicate DAO resource %q", ErrEntityAggregateCorrupt, dao.CollName())
				}
			}
			scope := coredata.ResolveDatabaseScope(dao)
			docs, err := repository.store.Load(readCtx, coredata.LoadSpec{
				Database: dao.DbName(), Scope: scope, Resource: dao.CollName(), Filter: map[string]any{"_id": fullID}, BatchSize: 1,
			})
			if err != nil {
				return fmt.Errorf("dataengine repository: load %s/%d: %w", dao.CollName(), fullID, err)
			}
			if len(docs) == 0 {
				missing++
				continue
			}
			if len(docs) != 1 || docs[0].Key.ID != fullID {
				return fmt.Errorf("%w: resource=%s entity=%d documents=%d", ErrEntityAggregateCorrupt, dao.CollName(), fullID, len(docs))
			}
			doc := docs[0]
			if doc.Deleted {
				tombstones++
				continue
			}
			payload, schema, err := persistedPayload(doc)
			if err != nil {
				return fmt.Errorf("%w: resource=%s entity=%d: %v", ErrEntityAggregateCorrupt, dao.CollName(), fullID, err)
			}
			loaded = append(loaded, loadedDAO{dao: dao, doc: doc, payload: payload, schema: schema})
			if doc.Enveloped && doc.Version >= remoteVector.StateVersion {
				remoteVector = entity.RemoteVersionVector{
					StateVersion: doc.Version, MarkerEpoch: doc.MarkerEpoch,
					LockFence: doc.LockFence, RouteEpoch: doc.RouteEpoch,
				}
			}
		}
		total := len(builder.DaoBuilders)
		switch {
		case missing == total || tombstones == total:
			return fmt.Errorf("%w: entity=%d", ErrEntityAggregateNotFound, fullID)
		case len(loaded) != total:
			return fmt.Errorf("%w: entity=%d live=%d missing=%d tombstones=%d", ErrEntityAggregateCorrupt, fullID, len(loaded), missing, tombstones)
		}
		return nil
	})
	return loaded, remoteVector, err
}

var _ entity.AggregateLoader = (*EntityRepository)(nil)
