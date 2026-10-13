package entity

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// EntityBuilderFunc 按参数构建实体（生成的 NewXxx）；返回的实体必须是指针实现的 IThreadSafeEntity。
type EntityBuilderFunc func(param *EntityCreateParam) (IThreadSafeEntity, error)

// DaoBuilderFunc 创建一个新的 DAO 实例。
type DaoBuilderFunc func() DaoInterface

// EntityBuilderParam 是一个具体实体 kind 的构建配置。
type EntityBuilderParam struct {
	BusinessPool BusinessPool      // 业务池（short / long），启动时注册，运行时不变
	Category     EntityCategory    // 分类，即锁序等级
	Kind         EntityKind        // 具体实体定义
	Builder      EntityBuilderFunc // 实体构造函数（生成的 NewXxx）
	DaoBuilders  []DaoBuilderFunc  // DAO 工厂，实体的每个 DAO 一个
	LoadPriority int               // 越小越先加载
	NoPersist    bool              // 不自动持久化
	RemotePolicy RemotePolicy      // 显式的跨服策略
	Lifetime     EntityLifetime    // 内存 / 持久化生命周期策略
	Sync         EntitySyncBuilderParam
}

// --- 全局实体工厂注册表 ---

// entityKindEntry 是框架为一个 kind 保存的完整记录。发布后不可变：每次更新存入新副本，无锁读者总是看到一份自洽的
// 记录，而不是可能在两次写入之间被读到的几张 map。
type entityKindEntry struct {
	category     EntityCategory
	businessPool BusinessPool
	policy       RemotePolicy
	builder      *EntityBuilderParam // nil until RegisterEntityBuilder runs
}

// 注册表以 EntityKind（uint8）为键，所以是定长数组而不是 map，一次读取就是一次无锁的原子加载。
//
// 这很重要：锁序路径在持有实体锁时读它——排序待取锁的实体时每次比较调用两次 GetEntityGroup，maxLockedGroup 对
// 每把已持有的锁调用一次，广播分桶对每个 ID 调用一次。在那里取注册表锁不仅每次查询多一次加锁，还会形成
// “实体锁 → 注册表锁”的取锁边，让一次注册阻塞锁序判断。
var (
	registryMu  sync.Mutex // 只串行写者；读者无锁
	kindEntries [1 << EntityKindBits]atomic.Pointer[entityKindEntry]
)

// kindEntryOf 无锁读取 kind 的记录；未注册时为 nil。
func kindEntryOf(kind EntityKind) *entityKindEntry {
	return kindEntries[kind].Load()
}

// RegisterEntityBuilder 为具体实体 kind 登记 builder（同时登记 kind 定义）。在服务启动路径调用，重复登记或定义冲突时 panic。
func RegisterEntityBuilder(param *EntityBuilderParam) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if param.Kind == EntityKindNone {
		panic("entity builder kind must not be none")
	}
	normalizeBuilderPolicy(param)
	if err := registerEntityKindDefinitionsLocked([]EntityKindDef{{
		Kind:         param.Kind,
		BusinessPool: param.BusinessPool,
		Category:     param.Category,
		RemotePolicy: param.RemotePolicy,
	}}); err != nil {
		panic(err)
	}
	// 上面刚创建或校验过这条记录，它一定存在。
	entry := kindEntryOf(param.Kind)
	if entry.builder != nil {
		panic(fmt.Sprintf("duplicate entity builder for kind %d", param.Kind))
	}
	next := *entry
	next.builder = param
	kindEntries[param.Kind].Store(&next)
}

// RegisterEntityKindCategory 登记一个 kind 的分类。
func RegisterEntityKindCategory(kind EntityKind, category EntityCategory) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	return registerEntityKindDefinitionsLocked([]EntityKindDef{{Kind: kind, Category: category}})
}

// MustRegisterEntityKindCategory 是 RegisterEntityKindCategory 的 panic 版本。
func MustRegisterEntityKindCategory(kind EntityKind, category EntityCategory) {
	if err := RegisterEntityKindCategory(kind, category); err != nil {
		panic(err)
	}
}

// RegisterEntityKindCategories 整批登记 kind 的 category：先校验整批，全部通过才写入，出错时注册表不变。
func RegisterEntityKindCategories(defs ...EntityKindCategory) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	kindDefs := make([]EntityKindDef, len(defs))
	for i, def := range defs {
		kindDefs[i] = EntityKindDef{Kind: def.Kind, Category: def.Category}
	}
	return registerEntityKindDefinitionsLocked(kindDefs)
}

// RegisterEntityKindDefs 整批登记 kind 定义：先校验整批（含批内同一 kind 的多条定义），全部通过才写入；
// 出错时注册表与派生锁档保持调用前的样子。
func RegisterEntityKindDefs(defs ...EntityKindDef) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	return registerEntityKindDefinitionsLocked(defs)
}

// MustRegisterEntityKindDefs 是 RegisterEntityKindDefs 的 panic 版本。
func MustRegisterEntityKindDefs(defs ...EntityKindDef) {
	if err := RegisterEntityKindDefs(defs...); err != nil {
		panic(err)
	}
}

// EntityCategoryOfKind 返回 kind 注册的分类；未注册时返回 false。
func EntityCategoryOfKind(kind EntityKind) (EntityCategory, bool) {
	if entry := kindEntryOf(kind); entry != nil {
		return entry.category, true
	}
	return EntityCategoryNone, false
}

// ResolveEntityKindCategory 返回 kind 注册的分类；kind 为 None 或未注册时返回错误（后者包裹 ErrInvalidEntityID）。
func ResolveEntityKindCategory(kind EntityKind) (EntityCategory, error) {
	if kind == EntityKindNone {
		return EntityCategoryNone, fmt.Errorf("entity kind must not be none")
	}
	// EntityKind 是 uint8 而 EntityKindBits 为 8：每个取值都在掩码内，不存在“超出掩码的 kind”需要拒绝。
	category, ok := EntityCategoryOfKind(kind)
	if !ok {
		return EntityCategoryNone, fmt.Errorf("%w: kind %d category is not registered", ErrInvalidEntityID, kind)
	}
	return category, nil
}

// MustEntityCategoryOfKind 是 ResolveEntityKindCategory 的 panic 版本。
func MustEntityCategoryOfKind(kind EntityKind) EntityCategory {
	category, err := ResolveEntityKindCategory(kind)
	if err != nil {
		panic(err)
	}
	return category
}

// registerEntityKindDefinitionsLocked 整批应用 kind 定义（调用方持有 registryMu）。第一遍只在暂存副本上校验与合并，
// 批内后面的定义看到的是前面定义合并后的结果；全部通过后第二遍才发布记录并刷新派生锁档，被拒的批次不会把前半段
// （新 kind、策略升级、锁档）留在注册表里。读者无锁，第二遍逐 kind 发布，
// 每个 kind 的记录各自一致，与单条注册的可见性相同。
func registerEntityKindDefinitionsLocked(defs []EntityKindDef) error {
	staged := make(map[EntityKind]*entityKindEntry, len(defs))
	order := make([]EntityKind, 0, len(defs))
	for _, def := range defs {
		existing, seen := staged[def.Kind]
		if !seen {
			existing = kindEntryOf(def.Kind)
		}
		next, err := resolveEntityKindDefinition(existing, def)
		if err != nil {
			return err
		}
		if next == existing {
			continue
		}
		if !seen {
			order = append(order, def.Kind)
		}
		staged[def.Kind] = next
	}
	for _, kind := range order {
		kindEntries[kind].Store(staged[kind])
		refreshLockRankLocked(kind)
	}
	return nil
}

// resolveEntityKindDefinition 计算把 def 应用到 existing（nil 表示未登记）之后的记录，不写注册表。
// 返回 existing 本身表示无变化。
func resolveEntityKindDefinition(existing *entityKindEntry, def EntityKindDef) (*entityKindEntry, error) {
	kind := def.Kind
	category := def.Category
	if kind == EntityKindNone {
		return nil, fmt.Errorf("entity kind must not be none")
	}
	if category == EntityCategoryNone {
		return nil, fmt.Errorf("entity category must not be none for kind %d", kind)
	}
	if def.BusinessPool > BusinessPoolLong {
		return nil, fmt.Errorf("invalid business pool %d", def.BusinessPool)
	}
	if existing == nil {
		return &entityKindEntry{category: category, policy: def.RemotePolicy, businessPool: def.BusinessPool}, nil
	}
	if def.BusinessPool != BusinessPoolDefault && existing.businessPool != def.BusinessPool {
		if existing.businessPool != BusinessPoolDefault || existing.builder != nil {
			return nil, fmt.Errorf("entity kind %d business pool cannot change", kind)
		}
		next := *existing
		next.businessPool = def.BusinessPool
		existing = &next
	}
	if existing.category != category {
		return nil, fmt.Errorf("entity kind %d category mismatch: registered=%d new=%d", kind, existing.category, category)
	}
	// 只登记分类时策略为 none，所以 none 是“尚未知道”，之后的定义可以补上。反过来（已有策略、新定义为 none）是部分
	// 重复声明而不是降级请求，忽略而不拒绝。其余情况是两个来源不一致。
	switch {
	case existing.policy == def.RemotePolicy:
		return existing, nil
	case existing.policy == RemotePolicyNone:
		if builder := existing.builder; builder != nil {
			// builder 先注册时按 none 定下了默认生命周期；策略随后升级，生命周期必须与新策略相符，
			// 否则 kind 变成托管而 builder 的生命周期仍是本地默认值。先声明 kind 定义再注册
			// builder 时 normalizeBuilderPolicy 会按注册表的策略取默认生命周期。
			if err := ValidateEntityPolicy(kind, builder.NoPersist, def.RemotePolicy, builder.Lifetime); err != nil {
				return nil, fmt.Errorf("entity kind %d remote policy %d declared after its builder: %w", kind, def.RemotePolicy, err)
			}
		}
		next := *existing
		next.policy = def.RemotePolicy
		return &next, nil
	case def.RemotePolicy == RemotePolicyNone:
		return existing, nil
	default:
		return nil, fmt.Errorf("entity kind %d remote policy mismatch: registered=%d new=%d", kind, existing.policy, def.RemotePolicy)
	}
}

// GetEntityBuilderParam 返回 kind 登记的 builder；没有时为 nil。
func GetEntityBuilderParam(kind EntityKind) *EntityBuilderParam {
	if entry := kindEntryOf(kind); entry != nil {
		return entry.builder
	}
	return nil
}

// GetAllEntityBuilders 按 kind 顺序返回全部已登记的 builder。
func GetAllEntityBuilders() []*EntityBuilderParam {
	result := make([]*EntityBuilderParam, 0, 16)
	for i := range kindEntries {
		if entry := kindEntries[i].Load(); entry != nil && entry.builder != nil {
			result = append(result, entry.builder)
		}
	}
	return result
}

// ResetEntityRegistryForTest 清空整张 kind 注册表（每个 kind 的定义、builder 与派生锁档）。
//
// Deprecated: 不要再用。它清掉的是进程级的整张表，包括 init() 与 sync.Once 只注册一次的条目，
// 同一进程里的后续用例（含 go test -count>1 的第二轮）因此拿不到它们。entity 包自己的测试使用按用例快照并恢复
// 注册表的夹具；包外测试请让每个用例使用互不冲突的 kind，并用 sync.Once 保证只注册一次。
// 维护者 2026-09-27 决定加弃用注释、保留为导出，供业务测试使用；仓内没有调用方不是删除它的理由。
func ResetEntityRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i := range kindEntries {
		kindEntries[i].Store(nil)
		lockRankByKind[i].Store(0)
	}
}

// GetEntityKindRemotePolicy 返回 kind 在注册表里的实际 Remote 策略；未注册时为 RemotePolicyNone。
func GetEntityKindRemotePolicy(kind EntityKind) RemotePolicy {
	// builder 与 kind 定义共用这条记录，RegisterEntityBuilder 先声明 kind 再存 builder，所以“没有记录”也意味着
	// “没有 builder”，不需要另查 builder。
	if entry := kindEntryOf(kind); entry != nil {
		return entry.policy
	}
	return RemotePolicyNone
}

// IsEntityKindRemoteCapable 报告 kind 是否 Remote 可用（managed 或 mirror）。
func IsEntityKindRemoteCapable(kind EntityKind) bool {
	return GetEntityKindRemotePolicy(kind).RemoteCapable()
}

// IsEntityKindRemoteManaged 报告 kind 是否由本服务以 Remote 托管。
func IsEntityKindRemoteManaged(kind EntityKind) bool {
	return GetEntityKindRemotePolicy(kind).RemoteManaged()
}

// --- 实体构建 ---

// resolveEntityBuilder 取参数 kind 登记的 builder，并补齐 / 校验分类。
func resolveEntityBuilder(param *EntityCreateParam) (*EntityBuilderParam, error) {
	if param == nil {
		return nil, fmt.Errorf("entity create param is nil")
	}
	if param.Kind == EntityKindNone {
		return nil, fmt.Errorf("entity kind must not be none")
	}
	bp := GetEntityBuilderParam(param.Kind)
	if bp == nil {
		return nil, fmt.Errorf("no entity builder registered for category %d kind %d", param.Category, param.Kind)
	}
	if param.Category == EntityCategoryNone {
		param.Category = bp.Category
	} else if bp.Category != EntityCategoryNone && param.Category != bp.Category {
		return nil, fmt.Errorf("entity builder kind %d category mismatch: param=%d builder=%d", param.Kind, param.Category, bp.Category)
	}
	return bp, nil
}

// BuildEntity 用登记的 builder 构建实体，不加入 Manager、不取实体锁。供需要先完成构建、再决定如何发布到内存的
// 加载器使用。顺序：规范 ID → 新建时创建 DAO → builder（内部创建组件并 InitAll）→ 策略与指针校验 → 恢复 Remote
// 版本 → 生命周期与同步 → OnInitFinish。
func BuildEntity(param *EntityCreateParam) (IThreadSafeEntity, error) {
	bp, err := resolveEntityBuilder(param)
	if err != nil {
		return nil, err
	}

	if err := param.NormalizeID(param.Kind); err != nil {
		return nil, err
	}

	// 新建实体时创建 DAO。
	if param.IsCreate && param.Dao == nil {
		param.Dao = make(map[string]DaoInterface)
		for _, daoBuilder := range bp.DaoBuilders {
			dao := daoBuilder()
			dao.SetId(param.StorageID())
			param.Dao[dao.CollName()] = dao
		}
	}

	// 构建实体（组件在 Builder 内创建并 InitAll）。
	e, err := bp.Builder(param)
	if err != nil {
		return nil, fmt.Errorf("build entity category %d: %w", param.Category, err)
	}
	if err := validateBuiltEntityPolicy(bp, e); err != nil {
		return nil, err
	}
	// 注册的 builder 构建出的实体必须是指针：Guard 按实例比较，值类型实现进入 Guard 后才 panic 太晚。
	if err := requirePointerEntity(e); err != nil {
		return nil, fmt.Errorf("entity kind %d: %w", bp.Kind, err)
	}
	if param.RemoteRestore != nil {
		remote, ok := e.(IThreadSafeRemoteEntity)
		if !ok {
			return nil, fmt.Errorf("entity kind %d has remote restore state but is not remote-managed", param.Kind)
		}
		if err := remote.SetRemoteVersionVector(*param.RemoteRestore); err != nil {
			return nil, fmt.Errorf("entity kind %d restore remote version: %w", param.Kind, err)
		}
		if err := remote.TransitionRemoteOwnership(RemoteOwnershipRecovering); err != nil {
			return nil, fmt.Errorf("entity kind %d restore remote ownership: %w", param.Kind, err)
		}
	}
	if e.Base() != nil {
		e.Base().SetLifetime(resolveEntityLifetime(param, bp))
	}
	initEntitySync(e, param, bp)

	// 实体级的初始化后处理（全部组件就绪之后）。
	if err := e.OnInitFinish(param); err != nil {
		return nil, fmt.Errorf("entity OnInitFinish category %d: %w", param.Category, err)
	}
	return e, nil
}

// normalizeBuilderPolicy 按 kind 的实际策略补默认生命周期并校验（调用方持有 registryMu）。builder 省略 RemotePolicy 时，
// kind 可能已经由 kind 定义声明为 managed / mirror——注册表按“部分重复声明”接受，全部 Remote 路径都按注册表的策略处理，
// 这里也必须一致，否则托管 kind 得到本地实体的默认生命周期。
func normalizeBuilderPolicy(param *EntityBuilderParam) {
	if param == nil {
		return
	}
	policy := param.RemotePolicy
	if policy == RemotePolicyNone {
		if entry := kindEntryOf(param.Kind); entry != nil {
			policy = entry.policy
		}
	}
	if param.Lifetime == EntityLifetimeDefault {
		param.Lifetime = DefaultEntityLifetime(param.NoPersist, policy)
	}
	if err := ValidateEntityPolicy(param.Kind, param.NoPersist, policy, param.Lifetime); err != nil {
		panic(err)
	}
}

// IsRemoteCapable 报告 builder 的 kind 是否 Remote 可用：builder 自带的策略或 kind 在注册表里的实际策略。
func (param *EntityBuilderParam) IsRemoteCapable() bool {
	return param != nil && (param.RemotePolicy.RemoteCapable() || GetEntityKindRemotePolicy(param.Kind).RemoteCapable())
}

// validateBuiltEntityPolicy 按 kind 在注册表里的实际策略校验构建结果：kind 为 managed 而 builder 省略
// RemotePolicy 时，构建出的实体同样必须实现 IThreadSafeRemoteEntity。
func validateBuiltEntityPolicy(bp *EntityBuilderParam, e IThreadSafeEntity) error {
	if bp == nil || e == nil {
		return nil
	}
	if GetEntityKindRemotePolicy(bp.Kind).RemoteManaged() {
		if _, ok := e.(IThreadSafeRemoteEntity); !ok {
			return fmt.Errorf("entity kind %d remote=managed but does not implement IThreadSafeRemoteEntity", bp.Kind)
		}
	}
	return nil
}

// resolveEntityLifetime 取实体的生命周期：参数显式值，其次 builder 的值，最后按持久化与注册表策略取默认值。
func resolveEntityLifetime(param *EntityCreateParam, bp *EntityBuilderParam) EntityLifetime {
	if param != nil && param.Lifetime != EntityLifetimeDefault {
		return param.Lifetime
	}
	if bp != nil && bp.Lifetime != EntityLifetimeDefault {
		return bp.Lifetime
	}
	noPersist := false
	remotePolicy := RemotePolicyNone
	if bp != nil {
		noPersist = bp.NoPersist
		remotePolicy = GetEntityKindRemotePolicy(bp.Kind)
	}
	return DefaultEntityLifetime(noPersist, remotePolicy)
}

// initEntitySync 按参数（优先）或 builder 的同步配置启用实体同步。
func initEntitySync(e IThreadSafeEntity, param *EntityCreateParam, bp *EntityBuilderParam) {
	if e == nil || e.Base() == nil {
		return
	}
	if param != nil && param.Sync != nil {
		if !param.Sync.Enabled {
			e.Base().SetSyncState(nil)
			return
		}
		syncParam := *param.Sync
		if syncParam.EntityID == 0 {
			syncParam.EntityID = e.ID()
		}
		if syncParam.Packer == nil && bp != nil && bp.Sync.PackerFactory != nil {
			syncParam.Packer = bp.Sync.PackerFactory(e)
		}
		if syncParam.EntityKind == 0 {
			syncParam.EntityKind = uint32(e.GetEntityKind())
		}
		e.Base().EnableSync(syncParam)
		return
	}
	if bp == nil || !bp.Sync.Enabled {
		return
	}
	syncParam := bp.Sync.toCreateParam(e)
	e.Base().EnableSync(syncParam)
}

// EntityBusinessPoolOfKind 无锁读取；未声明保持短业务默认。
func EntityBusinessPoolOfKind(kind EntityKind) BusinessPool {
	if e := kindEntryOf(kind); e != nil && e.businessPool == BusinessPoolLong {
		return BusinessPoolLong
	}
	return BusinessPoolShort
}

// FreezeBusinessPools 固化已注册 Kind 的默认值，防止运行时更改执行位置。
func FreezeBusinessPools() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i := range kindEntries {
		if e := kindEntries[i].Load(); e != nil && e.businessPool == BusinessPoolDefault {
			n := *e
			n.businessPool = BusinessPoolShort
			kindEntries[i].Store(&n)
		}
	}
}
