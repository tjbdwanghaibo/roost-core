package entity

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// IThreadSafeEntity is the full entity interface for the nest framework.
// Business entities embed EntityBase and implement this interface.
// Component/DAO wiring is handled by generated code, NOT by this interface.
type IThreadSafeEntity interface {
	IThreadSafeEntityBase

	AutoPersist() bool

	IsRemoved() bool
	SetRemoved()
	Touch() bool
	UnTouch()

	ClearBase()
	IsClear() bool

	Base() *EntityBase

	// OnInitFinish is called after all components are initialized.
	// Override in concrete entity to perform post-initialization logic.
	OnInitFinish(param *EntityCreateParam) error

	// OnDestroy is called when the entity is removed.
	// Called after all components' OnDestroy. Override in concrete entity for cleanup.
	OnDestroy(reason EntityDestroyReason)
}

// IThreadSafeEntityBase is the minimal entity identity interface.
type IThreadSafeEntityBase interface {
	ID() int64
	UniqueID() int64
	StorageID() int64
	GUId() int64
	GetEntityCategory() EntityCategory
	GetEntityKind() EntityKind
	GetMutex() lock.Mutex
}

// Getter retrieves entities by ID.
// Getter 必须保留 context；LoadedEntitiesOnly 为真（快 worker 或显式约束的 ctx）时
// 仅允许内存读取，冷目标返回可 errors.Is 判别的 ErrColdLoadInLogic，不能执行 I/O、
// 不能等待加载，也不能 panic：冷缺失本身不阻塞，业务可以据此降级（RR-20260926-26）。
// 在快 worker 上返回的错误同时包裹 fctx.ErrBlockingInFastWorker。Nest 统一准入不调用
// Get 判断冷热，只用可选的 LoadedChecker（RR-20260926-47）。
type Getter interface {
	Get(context.Context, int64, EntityCategory) (IThreadSafeEntity, error)
	GetMany(context.Context, []int64, []EntityCategory) ([]IThreadSafeEntity, error)
}

// LoadedChecker 是 Getter 的可选能力：不做任何加载的只读查询，Nest 统一准入用它判断声明目标
// 是否需要慢阶段预加载（RR-20260926-25 / 47）。准入在发送方 goroutine（包括快 worker 和唯一的
// 延迟派发 goroutine）上执行，实现不得做 I/O、等待在途加载、Touch 实体或阻塞。
// Getter 未实现时，准入不判冷、不自动转慢，也不在准入后发现冷目标时原位转慢；冷目标按
// LoadedEntitiesOnly 契约返回 ErrColdLoadInLogic，需要预加载时业务使用显式 Slow。
type LoadedChecker interface {
	// IsLoaded 报告完整 ID 的实体是否无需加载即可读取：已在内存中返回 true；不在内存且
	// 有可用加载器时返回 false；没有加载器时也返回 true——慢阶段同样读不到，缺失交由派发阶段
	// 按不存在处理。
	IsLoaded(id int64) bool
}

// AggregateLoader reconstructs a complete entity from persistent DAO
// documents and publishes it atomically through the owning EntityManager.
type AggregateLoader interface {
	LoadEntity(context.Context, int64, EntityKind) (IThreadSafeEntity, error)
}

// IDirty tracks modification state.
type IDirty interface {
	Dirty() bool
	SelfClean()
}

// DaoInterface is the DAO contract for entity persistence.
type DaoInterface interface {
	Id() int64
	SetId(int64)
	DbName() string
	CollName() string
	Dirty() IDirty
	CleanDirty()
}

// DatabaseScopedDao optionally declares how its logical database name is
// resolved by the storage service.
type DatabaseScope uint8

const (
	DatabaseGlobal DatabaseScope = iota
	DatabaseServer
)

type DatabaseScopedDao interface {
	DbScope() DatabaseScope
}

// PersistedDaoLoader is implemented by generated DAOs. RestorePersisted owns
// schema migration, BSON decoding and dirty/version tracker restoration so the
// storage layer never needs reflection or knowledge of generated fields.
type PersistedDaoLoader interface {
	RestorePersisted(raw []byte, schemaVersion uint32, version uint64) error
}

// Guardable is implemented by entities that expose DAO instances for
// persistence, remote save, and dirty-mask tracking.
type Guardable interface {
	RangeDao(func(DaoInterface))
}

// EntityCreateParam holds parameters for entity creation.
type EntityCreateParam struct {
	IsCreate bool
	Category EntityCategory
	Kind     EntityKind
	// Id is normalized to the full EntityID before the entity is built.
	// Explicit Id values must already be full EntityIDs. Use UniqueID only at
	// entity creation/loading boundaries that own raw unique-number generation.
	Id             int64
	UniqueID       int64
	OwnerId        int64
	OwnerCategory  EntityCategory
	BelongId       int64
	BelongCategory EntityCategory
	ExclusiveId    int64
	Lifetime       EntityLifetime
	Dao            map[string]DaoInterface
	Sync           *EntitySyncCreateParam
	Param          any
	// Mutex is injected by the owning EntityManager. Generated constructors
	// pass it into the entity base constructor.
	Mutex lock.Mutex
	// RemoteRestore is set only by the persistent aggregate repository. BuildEntity
	// installs it before OnInitFinish and before the manager publishes the entity.
	RemoteRestore *RemoteVersionVector
}

func (param *EntityCreateParam) NormalizeID(kind EntityKind) error {
	if param == nil {
		return fmt.Errorf("entity create param is nil")
	}
	if kind == EntityKindNone {
		kind = param.Kind
	}
	if kind == EntityKindNone {
		return fmt.Errorf("entity kind must not be none")
	}
	if param.Kind != EntityKindNone && param.Kind != kind {
		return fmt.Errorf("entity kind mismatch: param=%d builder=%d", param.Kind, kind)
	}
	category, err := ResolveEntityKindCategory(kind)
	if err != nil {
		return err
	}
	if param.Category != EntityCategoryNone && param.Category != category {
		return fmt.Errorf("entity category mismatch: param=%d kind=%d category=%d", param.Category, kind, category)
	}
	param.Kind = kind
	param.Category = category

	if param.Id != 0 {
		return param.setFullID(param.Id, kind)
	}
	if param.UniqueID != 0 {
		return param.setRawID(param.UniqueID, kind)
	}
	if !param.IsCreate {
		return fmt.Errorf("entity load requires full Id or UniqueID")
	}
	return ErrIDGeneratorRequired
}

func (param *EntityCreateParam) setRawID(uniqueID int64, kind EntityKind) error {
	id, err := BuildEntityID(uniqueID, kind)
	if err != nil {
		return err
	}
	meta := ResolveEntityID(id)
	param.UniqueID = uniqueID
	param.Id = id
	param.Category = meta.Category
	param.Kind = meta.Kind
	return nil
}

func (param *EntityCreateParam) setFullID(id int64, kind EntityKind) error {
	fullID, err := NormalizeFullID(id, kind)
	if err != nil {
		return err
	}
	meta := ResolveEntityID(fullID)
	param.Id = fullID
	param.UniqueID = meta.UniqueID
	param.Category = meta.Category
	param.Kind = meta.Kind
	return nil
}

func (param *EntityCreateParam) StorageID() int64 {
	if param == nil {
		return 0
	}
	if param.Id != 0 {
		return ResolveEntityID(param.Id).FullID
	}
	if param.UniqueID != 0 {
		id, err := BuildEntityID(param.UniqueID, param.Kind)
		if err != nil {
			return 0
		}
		return id
	}
	return 0
}

func (param *EntityCreateParam) FullID() int64 {
	if param == nil {
		return 0
	}
	return param.Id
}
