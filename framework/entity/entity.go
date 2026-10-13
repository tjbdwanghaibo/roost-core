package entity

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// IThreadSafeEntity 是 Nest 框架使用的完整实体接口。业务实体嵌入 EntityBase 并实现它；组件 / DAO 的装配由生成代码完成，
// 不经这个接口。
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

	// OnInitFinish 在全部组件初始化之后调用；具体实体可覆盖以执行初始化后的逻辑。
	OnInitFinish(param *EntityCreateParam) error

	// OnDestroy 在实体移除时、全部组件的 OnDestroy 之后调用；具体实体可覆盖以执行清理。
	OnDestroy(reason EntityDestroyReason)
}

// IThreadSafeEntityBase 是实体身份的最小接口。
type IThreadSafeEntityBase interface {
	ID() int64
	UniqueID() int64
	StorageID() int64
	GUId() int64
	GetEntityCategory() EntityCategory
	GetEntityKind() EntityKind
	GetMutex() lock.Mutex
}

// Getter 按 ID 取实体。Getter 必须保留 context；LoadedEntitiesOnly 为真（业务 worker 或显式约束的 ctx）时
// 仅允许内存读取，冷目标返回可 errors.Is 判别的 ErrColdLoadInLogic，不能执行 I/O、
// 不能等待加载，也不能 panic：冷缺失本身不阻塞，业务可以据此降级。
// 在业务 worker 上返回的错误同时包裹 fctx.ErrBlockingInBusinessWorker。Nest 统一准入不调用
// Get 判断冷热，只用可选的 LoadedChecker。
type Getter interface {
	Get(context.Context, int64, EntityCategory) (IThreadSafeEntity, error)
	GetMany(context.Context, []int64, []EntityCategory) ([]IThreadSafeEntity, error)
}

// LoadedChecker 是 Getter 的可选能力：不做任何加载的只读查询，Nest 统一准入用它判断声明目标
// 是否需要 I/O 阶段预加载。准入在发送方 goroutine（包括业务 worker 和唯一的
// 延迟派发 goroutine）上执行，实现不得做 I/O、等待在途加载、Touch 实体或阻塞。
// Getter 未实现时，准入不判冷、不自动转慢，也不在准入后发现冷目标时原位转慢；冷目标按
// LoadedEntitiesOnly 契约返回 ErrColdLoadInLogic，需要预加载时业务使用显式 Slow。
type LoadedChecker interface {
	// IsLoaded 报告完整 ID 的实体是否无需加载即可读取：已在内存中返回 true；不在内存且
	// 有可用加载器时返回 false；没有加载器时也返回 true——I/O 准备阶段同样读不到，缺失交由派发阶段
	// 按不存在处理。
	IsLoaded(id int64) bool
}

// AggregateLoader 从持久化的 DAO 文档重建完整实体，并经所属 EntityManager 原子地发布。
type AggregateLoader interface {
	LoadEntity(context.Context, int64, EntityKind) (IThreadSafeEntity, error)
}

// IDirty 记录修改状态。
type IDirty interface {
	Dirty() bool
	SelfClean()
}

// DaoInterface 是实体持久化的 DAO 契约。
type DaoInterface interface {
	Id() int64
	SetId(int64)
	DbName() string
	CollName() string
	Dirty() IDirty
	CleanDirty()
}

// DatabaseScope 是 DAO 逻辑库名的解析方式：全局库，或按服（sid）选库。
type DatabaseScope uint8

const (
	DatabaseGlobal DatabaseScope = iota
	DatabaseServer
)

// DatabaseScopedDao 由 DAO 可选实现，声明存储服务如何解析它的逻辑库名；未实现按 DatabaseGlobal。
type DatabaseScopedDao interface {
	DbScope() DatabaseScope
}

// PersistedDaoLoader 由生成 DAO 实现。RestorePersisted 负责 schema 校验（只接受当前版本，不符时在解码前拒绝）、
// BSON 解码以及脏标记 / 版本 tracker 的恢复，存储层因此不需要反射，也不需要了解生成的字段。
type PersistedDaoLoader interface {
	RestorePersisted(raw []byte, schemaVersion uint32, version uint64) error
}

// Guardable 由暴露 DAO 实例的实体实现，供持久化、Remote 保存与脏掩码跟踪使用。
type Guardable interface {
	RangeDao(func(DaoInterface))
}

// EntityCreateParam 是创建或加载实体的参数。
type EntityCreateParam struct {
	IsCreate bool
	Category EntityCategory
	Kind     EntityKind
	// Id 在构建实体前规范为完整 EntityID；显式给出的 Id 必须已是完整 EntityID。UniqueID 只在自己生成原始唯一号的
	// 创建 / 加载边界使用。
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
	// Mutex 由所属 EntityManager 注入，生成的构造函数把它传给 EntityBase 构造函数。
	Mutex lock.Mutex
	// RemoteRestore 只由持久聚合仓库设置。BuildEntity 在 OnInitFinish 之前、Manager 发布实体之前装入它。
	RemoteRestore *RemoteVersionVector
}

// NormalizeID 按 kind（为 None 时取 param.Kind）补齐并校验 Kind / Category，把 Id 或 UniqueID 规范为完整 EntityID。
// 新建且两者都未给出时返回 ErrIDGeneratorRequired，由调用方生成 ID。
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

// setRawID 由唯一号与 kind 构造完整 ID 并回填参数。
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

// setFullID 校验并规范完整 ID，回填唯一号、分类与 kind。
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

// StorageID 返回持久化使用的完整 ID；无法确定时为 0。
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

// FullID 返回 Id（规范后即完整 EntityID）。
func (param *EntityCreateParam) FullID() int64 {
	if param == nil {
		return 0
	}
	return param.Id
}
