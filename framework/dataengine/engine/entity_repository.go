package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

var (
	// ErrEntityAggregateNotFound 同时满足 errors.Is(err, entity.ErrAuthorityEntityNotFound)：卸载后重载据此
	// 立即向订阅者退回 remove，而不是当作可重试的读取失败。
	ErrEntityAggregateNotFound error = aggregateNotFoundError{}
	// ErrEntityAggregateDeleted：聚合的全部 DAO 都只剩墓碑（删过档）。满足 errors.Is(err, entity.ErrAuthorityEntityDeleted)，
	// 不满足 ErrEntityAggregateNotFound / entity.ErrAuthorityEntityNotFound：维护者 2026-10-17 决定 D10 方案 B，
	// 删档后该 ID 不可再用，“不存在就创建”的代码遇到它不会去创建（理由见 entity.ErrAuthorityEntityDeleted）。
	ErrEntityAggregateDeleted error = aggregateDeletedError{}
	ErrEntityAggregateCorrupt       = errors.New("dataengine repository: entity aggregate is incomplete or corrupt")
)

type aggregateNotFoundError struct{}

func (aggregateNotFoundError) Error() string {
	return "dataengine repository: entity aggregate not found"
}

func (aggregateNotFoundError) Is(target error) bool {
	return target == entity.ErrAuthorityEntityNotFound
}

type aggregateDeletedError struct{}

func (aggregateDeletedError) Error() string {
	return "dataengine repository: entity aggregate was deleted (tombstones only); its id cannot be used again"
}

func (aggregateDeletedError) Is(target error) bool {
	return target == entity.ErrAuthorityEntityDeleted
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
// （Nest I/O 准备阶段下即业务池续行）。用途是让依附在旧对象上的协作者接上重载出来的新对象：
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
// 冷加载包含 I/O，不允许从业务池进入；初始化和发布经 RunLocal 返回本地执行阶段。
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

// remoteTombstoneReader 由 Store 可选实现（MongoStore 转给 Remote 原子存储）：报告 Remote 托管实体是否删过档。
type remoteTombstoneReader interface {
	RemoteEntityDeleted(ctx context.Context, id int64) (bool, error)
}

// loadedDAO 是一致性读取得到的一个 DAO 文档及其已解出的持久化 payload。filled 为真时库里没有这个 DAO 的文档，
// dao 是 builder 给出的初值（版本 0），不校验 schema、不水合，见 readAggregate。
type loadedDAO struct {
	dao     entity.DaoInterface
	doc     coredata.RawDocument
	payload []byte
	schema  uint32
	filled  bool
}

// metricAggregateDAOFilled 计冷加载时按初值补上的 DAO（标签 kind、collection）。
const metricAggregateDAOFilled = "dataengine.aggregate.dao_filled.total"

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
	// 冷加载 I/O 在 io 池；初始化回调和发布 Entity 回到业务池。
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
	// 只为真正发布出去的补空计数与告警：校验 / 水合失败或输给并发加载时这次补空没有生效。
	reportFilledDAOs(loaded, kind, fullID)
	return created, nil
}

// checkAggregateSchemas 在水合前校验每个 DAO 的存储 schema 等于当前运行时 schema；
// 任何不匹配都不能水合、发布或触发写回。
func checkAggregateSchemas(loaded []loadedDAO, fullID int64) error {
	for _, item := range loaded {
		if item.filled {
			continue
		}
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
		if item.filled {
			daos[item.dao.CollName()] = item.dao
			continue
		}
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

// readAggregate 在一次一致性读里读取聚合的全部 DAO 文档并分类（D10 与维护者 2026-10-30 的补空决定）：
//   - 全部缺失：从未创建，ErrEntityAggregateNotFound（Remote 托管实体先查 Remote 元数据墓碑，删过档为 Deleted）；
//   - 没有活文档、有墓碑（其余缺失）：删过档，ErrEntityAggregateDeleted——删档之后给 kind 新增的 DAO 本来就没有墓碑，
//     不能因此把它当成部分存在、补空复活；
//   - 活文档与墓碑混在一起：损坏，ErrEntityAggregateCorrupt；
//   - 至少一个活文档、其余缺失（没有墓碑）：存量实体缺了上线后新增的 DAO，按 builder 给出的初值补上（filled，版本 0，
//     由这个实体的下一笔持久提交以整文档写出，见 nest includeUnpersistedDAOs）。Remote 托管实体还要查 Remote 元数据
//     墓碑，删过档的不补、按损坏拒绝。
//
// 同时取版本最高的 Remote 版本信封。
func (repository *EntityRepository) readAggregate(ctx context.Context, builder *entity.EntityBuilderParam, fullID int64) ([]loadedDAO, entity.RemoteVersionVector, error) {
	var loaded []loadedDAO
	var remoteVector entity.RemoteVersionVector
	remoteManaged := entity.GetEntityKindRemotePolicy(builder.Kind).RemoteManaged()
	err := repository.store.ReadConsistent(ctx, func(readCtx context.Context) error {
		// ReadConsistent 运行在 ISession.WithTransaction 里，驱动遇到
		// TransientTransactionError 或提交结果未知时会重调本回调。所有累加量必须在这里
		// 重置：跨尝试保留 loaded 会让第二次尝试撞上重复资源检查，把健康数据报成损坏聚合。
		loaded = make([]loadedDAO, 0, len(builder.DaoBuilders))
		remoteVector = entity.RemoteVersionVector{}
		var missing []entity.DaoInterface
		live, tombstones := 0, 0
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
			for _, existing := range missing {
				if existing.CollName() == dao.CollName() {
					return fmt.Errorf("%w: duplicate DAO resource %q", ErrEntityAggregateCorrupt, dao.CollName())
				}
			}
			scope := coredata.ResolveDatabaseScope(dao)
			// 墓碑也要读到：全部墓碑是“删过档”而不是“不存在”（D10），活文档与墓碑混在一起是损坏。
			docs, err := repository.store.Load(readCtx, coredata.LoadSpec{
				Database: dao.DbName(), Scope: scope, Resource: dao.CollName(), Filter: map[string]any{"_id": fullID}, BatchSize: 1,
				IncludeDeleted: true,
			})
			if err != nil {
				return fmt.Errorf("dataengine repository: load %s/%d: %w", dao.CollName(), fullID, err)
			}
			if len(docs) == 0 {
				missing = append(missing, dao)
				continue
			}
			if len(docs) != 1 || docs[0].Key.ID != fullID {
				return fmt.Errorf("%w: resource=%s entity=%d documents=%d", ErrEntityAggregateCorrupt, dao.CollName(), fullID, len(docs))
			}
			doc := docs[0]
			if doc.Deleted {
				tombstones++
				// 占位，保证重复资源检查覆盖墓碑；分类结束前一定返回错误，不会被水合。
				loaded = append(loaded, loadedDAO{dao: dao, doc: doc})
				continue
			}
			payload, schema, err := persistedPayload(doc)
			if err != nil {
				return fmt.Errorf("%w: resource=%s entity=%d: %v", ErrEntityAggregateCorrupt, dao.CollName(), fullID, err)
			}
			live++
			loaded = append(loaded, loadedDAO{dao: dao, doc: doc, payload: payload, schema: schema})
			if doc.Enveloped && doc.Version >= remoteVector.StateVersion {
				remoteVector = entity.RemoteVersionVector{
					StateVersion: doc.Version, MarkerEpoch: doc.MarkerEpoch,
					LockFence: doc.LockFence, RouteEpoch: doc.RouteEpoch,
				}
			}
		}
		total := len(builder.DaoBuilders)
		remoteDeleted := false
		if remoteManaged && tombstones == 0 && len(missing) > 0 {
			// Remote 托管实体删档时数据文档被物理删除，墓碑只在 Remote 元数据里（_remote_entity_meta._deleted）；
			// 同一次一致性读里查它，删过档与从未创建才能区分（D10），删过档的部分聚合也不能补空复活。
			if reader, ok := repository.store.(remoteTombstoneReader); ok {
				deleted, err := reader.RemoteEntityDeleted(readCtx, fullID)
				if err != nil {
					return fmt.Errorf("dataengine repository: remote tombstone of %d: %w", fullID, err)
				}
				remoteDeleted = deleted
			}
		}
		switch {
		// 从未创建与删过档是两个哨兵（D10 方案 B）：前者可以创建（客户端码 entity_not_found），
		// 后者这个 ID 不可再用（entity_deleted）。错误文本仍带 state，便于运维直接看出是哪一种。
		case len(missing) == total && remoteDeleted:
			return fmt.Errorf("%w: entity=%d state=deleted remote=true", ErrEntityAggregateDeleted, fullID)
		case len(missing) == total:
			return fmt.Errorf("%w: entity=%d state=absent", ErrEntityAggregateNotFound, fullID)
		case live == 0:
			return fmt.Errorf("%w: entity=%d state=deleted tombstones=%d missing=%d", ErrEntityAggregateDeleted, fullID, tombstones, len(missing))
		case tombstones > 0:
			return fmt.Errorf("%w: entity=%d live=%d missing=%d tombstones=%d", ErrEntityAggregateCorrupt, fullID, live, len(missing), tombstones)
		case remoteDeleted:
			return fmt.Errorf("%w: entity=%d live=%d missing=%d but the remote metadata says deleted", ErrEntityAggregateCorrupt, fullID, live, len(missing))
		}
		for _, dao := range missing {
			loaded = append(loaded, loadedDAO{dao: dao, filled: true})
		}
		return nil
	})
	return loaded, remoteVector, err
}

// reportFilledDAOs 为冷加载时按初值补上的 DAO 打 WARN 并计数：上线后给 kind 新增 DAO 时每个存量实体第一次加载各一次，
// 之后由这个实体的下一笔持久提交写出文档、不再出现；新增 DAO 之外的出现（或同一实体反复出现）说明丢写 / 误删 / 投影缺口，
// 要人核对。日志只带 kind、实体 ID 与集合名，不带业务数据；指标标签是 kind 与集合名（低基数）。
func reportFilledDAOs(loaded []loadedDAO, kind entity.EntityKind, fullID int64) {
	var collections []string
	for _, item := range loaded {
		if !item.filled {
			continue
		}
		collections = append(collections, item.dao.CollName())
		metrics.IncCounter(metricAggregateDAOFilled, metrics.Labels{"kind": strconv.Itoa(int(kind)), "collection": item.dao.CollName()}, 1)
	}
	if len(collections) > 0 {
		slog.Warn("dataengine repository: entity aggregate is missing DAO documents; filled them with initial values (expected once per stored entity after a DAO is added to its kind; otherwise check for lost writes or deletes)",
			"kind", kind, "entity", fullID, "collections", collections)
	}
}

var _ entity.AggregateLoader = (*EntityRepository)(nil)
