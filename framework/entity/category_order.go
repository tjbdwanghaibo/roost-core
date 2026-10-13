package entity

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
)

// EntityCategoryRemote 是框架保留的分类，用于权威状态处于分布式所有权保护之下的 kind。
//
// 分类的取值就是它的锁序等级，取锁从低到高。Remote 固定在最低一级，这一级有物理原因而非业务约定：Remote 托管
// 实体的所有权锁在派发开始时取得，若在已持本地实体锁时再取它，就会持着那把本地锁跨一次网络往返，让该实体的
// 其他使用者都卡在一次后端抖动之后。
//
// 因此应用分类从 EntityCategoryRemote + 1 开始，按必须取锁的顺序编号。
const EntityCategoryRemote EntityCategory = 1

// 推荐的分类。取值是锁序等级，取锁从低到高，框架按下面的顺序设计：
//
//	Remote        分布式所有权；必须最先，见上
//	World         世界 / 场景对象，被许多玩家争用
//	PlayerScoped  玩家的对外表示，先于玩家本身取锁
//	Player        玩家实体本身
//	Other         其他
//
// 它们只是常量而非强制：分类是完整的 uint8，项目可以在它们之间插入自己的取值，或接在 Other 之后。强制的只有
// Remote 托管 kind 必须属于 EntityCategoryRemote，由 ValidateEntityRegistry 检查。
const (
	EntityCategoryWorld        EntityCategory = EntityCategoryRemote + 1
	EntityCategoryPlayerScoped EntityCategory = EntityCategoryRemote + 2
	EntityCategoryPlayer       EntityCategory = EntityCategoryRemote + 3
	EntityCategoryOther        EntityCategory = EntityCategoryRemote + 4
)

// EntityCategoryUnknown 是本进程未链接的 kind 的 ID 所得的等级，为保留值：不能有 kind 注册在其中。
//
// 排在最后是保守的选择：等级只用于比较，持有最后一级之后不能再取任何锁，所以本进程无法判断的 ID 永远不会成为
// 另一次取锁被判为乱序的原因。
const EntityCategoryUnknown EntityCategory = 255

// EntityCategoryDef 声明一个分类。
//
// 没有顺序字段：分类的取值就是它的顺序。Name 只是让日志与错误能写“world”而不是“2”。
type EntityCategoryDef struct {
	Category EntityCategory
	Name     string
}

// categoryTaxonomy 是已声明的分类集合。整体替换，无锁读者总是看到一个自洽的集合。
type categoryTaxonomy struct {
	names map[EntityCategory]string
}

var (
	categoryMu sync.Mutex // 只串行写者
	taxonomy   atomic.Pointer[categoryTaxonomy]
	// lockRankByKind 是注册时为每个 kind 推导的锁序等级。0 表示未推导（kind 未注册），此时 GetEntityGroup
	// 按 ID 的 Remote 位或 EntityCategoryUnknown 定级。
	lockRankByKind [1 << EntityKindBits]atomic.Uint32
)

// RegisterEntityCategories 为应用的分类命名，供日志、错误与 ValidateEntityRegistry 使用。它不建立锁序：
// 分类的取值无论是否在这里命名都是它的等级。
func RegisterEntityCategories(defs ...EntityCategoryDef) error {
	if len(defs) == 0 {
		return fmt.Errorf("entity: no categories to register")
	}
	categoryMu.Lock()
	defer categoryMu.Unlock()

	next := &categoryTaxonomy{names: make(map[EntityCategory]string, len(defs))}
	if current := taxonomy.Load(); current != nil {
		maps.Copy(next.names, current.names)
	}
	for _, def := range defs {
		if def.Category == EntityCategoryNone {
			return fmt.Errorf("entity: category must not be none")
		}
		if def.Category == EntityCategoryUnknown {
			return fmt.Errorf("entity: category %d is reserved for kinds this process does not link", EntityCategoryUnknown)
		}
		if existing, ok := next.names[def.Category]; ok && existing != def.Name {
			return fmt.Errorf("entity: category %d already declared as %q, new name %q", def.Category, existing, def.Name)
		}
		next.names[def.Category] = def.Name
	}
	taxonomy.Store(next)

	// kind 可能在分类声明之前就已注册，所以重新推导全部等级，而不只是新的。
	registryMu.Lock()
	defer registryMu.Unlock()
	for kind := range lockRankByKind {
		refreshLockRankLocked(EntityKind(kind))
	}
	return nil
}

// MustRegisterEntityCategories 是 RegisterEntityCategories 的 panic 版本。
func MustRegisterEntityCategories(defs ...EntityCategoryDef) {
	if err := RegisterEntityCategories(defs...); err != nil {
		panic(err)
	}
}

// EntityCategoryName 返回分类的声明名（用于诊断），其次是推荐分类的名字，都没有时返回十进制取值。
func EntityCategoryName(category EntityCategory) string {
	if declared := taxonomy.Load(); declared != nil {
		if name, ok := declared.names[category]; ok && name != "" {
			return name
		}
	}
	if name, ok := recommendedCategoryNames[category]; ok {
		return name
	}
	return fmt.Sprintf("%d", category)
}

// recommendedCategoryNames 让未声明时的诊断也可读。它只是 EntityCategoryName 的兜底，不声明任何东西，
// 所以 ValidateEntityRegistry 的集合级检查仍是可选的。
var recommendedCategoryNames = map[EntityCategory]string{
	EntityCategoryRemote:       "remote",
	EntityCategoryWorld:        "world",
	EntityCategoryPlayerScoped: "player_scoped",
	EntityCategoryPlayer:       "player",
	EntityCategoryOther:        "other",
	EntityCategoryUnknown:      "unknown",
}

// refreshLockRankLocked 推导一个 kind 的锁序等级。调用方持 registryMu。
//
// 声明分类不是前提：等级就是 kind 注册时的分类，不论应用是否给它命名。声明只增加名字，并让
// ValidateEntityRegistry 检查集合。
func refreshLockRankLocked(kind EntityKind) {
	entry := kindEntryOf(kind)
	if entry == nil {
		lockRankByKind[kind].Store(0)
		return
	}
	rank := entry.category
	if entry.policy.RemoteManaged() {
		// 强制而非信任：声明在其他分类的托管 kind 否则会排在本地实体之后取锁，那是唯一绝不允许的顺序。
		// ValidateEntityRegistry 仍把这种声明报为错误，让错误可见。
		rank = EntityCategoryRemote
	}
	lockRankByKind[kind].Store(uint32(rank))
}

// lockRankOf 返回 kind 推导出的等级；未推导时返回 false。
func lockRankOf(kind EntityKind) (int, bool) {
	if rank := lockRankByKind[kind].Load(); rank != 0 {
		return int(rank), true
	}
	return 0, false
}

// ValidateEntityRegistry 按已声明的分类检查注册表，一次报告全部问题。在启动末尾调用。
//
// 它只校验、不冻结注册表：之后的注册仍然合法，可以再次调用本函数。
//
// 声明分类是可选的：未声明时跳过集合级检查，逐 kind 的不变量照常检查。
func ValidateEntityRegistry() error {
	declared := taxonomy.Load()

	var problems []error
	if declared != nil {
		minDeclared := EntityCategory(0)
		for category := range declared.names {
			if minDeclared == 0 || category < minDeclared {
				minDeclared = category
			}
		}
		if _, ok := declared.names[EntityCategoryRemote]; !ok {
			problems = append(problems, fmt.Errorf("category %d (remote) is not declared, so nothing pins the rank that must be acquired first", EntityCategoryRemote))
		} else if minDeclared != EntityCategoryRemote {
			problems = append(problems, fmt.Errorf("category %d (%s) ranks before remote; remote must be the lowest declared category", minDeclared, EntityCategoryName(minDeclared)))
		}
	}

	for kind := 0; kind < len(lockRankByKind); kind++ {
		entry := kindEntryOf(EntityKind(kind))
		if entry == nil {
			continue
		}
		if entry.category == EntityCategoryUnknown {
			problems = append(problems, fmt.Errorf("kind %d is in category %d, which is reserved for kinds this process does not link", kind, EntityCategoryUnknown))
		}
		if declared != nil {
			if _, ok := declared.names[entry.category]; !ok {
				problems = append(problems, fmt.Errorf("kind %d is in category %d, which is not declared", kind, entry.category))
			}
		}
		if entry.policy.RemoteManaged() && entry.category != EntityCategoryRemote {
			problems = append(problems, fmt.Errorf("kind %d is remote-managed but declared in category %d (%s); it must be in category %d (remote), which is the rank acquired first",
				kind, entry.category, EntityCategoryName(entry.category), EntityCategoryRemote))
		}
		// 与 Remote 装配期同一条规则（见 ValidateRemoteManagedDaoScopes）：注册期就拒绝，不让不装 Remote 的进程带着
		// 这个组合启动、要到装配 Remote 时才暴露。
		if entry.builder != nil {
			problems = append(problems, remoteManagedDaoScopeProblems(entry.builder)...)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("entity registry is inconsistent: %w", joinCategoryProblems(problems))
}

// ErrRemoteManagedServerScopedDAO 表示 remote=managed 实体使用了按服选库（dbscope=sid）的 DAO。
// 托管实体可由任一进程写入、所有权可迁移，而 Remote 提交按提交方 sid 选库、加载按本服 sid 选库，两者不一致会让实体
// 在跨服写或迁移后读到旧版本 / 缺失而不可写。ValidateEntityRegistry、Remote 装配（remoteentity.Assemble / Start，
// 其 ErrRemoteManagedServerScopedDAO 就是这个值）与 Remote 提交的 WAL 准入前都用它拒绝。
var ErrRemoteManagedServerScopedDAO = errors.New("remote_entity: remote-managed entity DAO must use dbscope=global")

// ValidateRemoteManagedDaoScopes 拒绝 remote=managed 实体注册 dbscope=sid 的 DAO：逐个实例化
// builders 注册的 DAO 工厂读取 DbScope，未声明 DbScope 的 DAO 按 global。每个违规 DAO 一条错误，用 errors.Join 汇总，
// 均可 errors.Is ErrRemoteManagedServerScopedDAO。
//
// 是否托管看 kind 在注册表里的实际策略（GetEntityKindRemotePolicy），不看 builder 自带的 RemotePolicy：kind 定义声明
// managed、手写 builder 省略 RemotePolicy 时注册表按“部分重复声明”接受，全部 Remote 路径都按 managed 处理，校验也
// 必须按 managed。
func ValidateRemoteManagedDaoScopes(builders []*EntityBuilderParam) error {
	var problems []error
	for _, builder := range builders {
		problems = append(problems, remoteManagedDaoScopeProblems(builder)...)
	}
	return errors.Join(problems...)
}

// remoteManagedDaoScopeProblems 实例化托管 kind 的每个 DAO，返回使用 dbscope=sid 的违规项。
func remoteManagedDaoScopeProblems(builder *EntityBuilderParam) []error {
	if builder == nil || !GetEntityKindRemotePolicy(builder.Kind).RemoteManaged() {
		return nil
	}
	var problems []error
	for i, build := range builder.DaoBuilders {
		if build == nil {
			continue
		}
		dao := build()
		if dao == nil {
			continue
		}
		scoped, ok := dao.(DatabaseScopedDao)
		if !ok || scoped.DbScope() != DatabaseServer {
			continue
		}
		problems = append(problems, fmt.Errorf("%w: kind=%d dao[%d] %T collection=%q uses dbscope=sid; declare the DAO with dbscope=global (//roost:dao ... dbscope=global) so every process commits and loads the same database",
			ErrRemoteManagedServerScopedDAO, builder.Kind, i, dao, dao.CollName()))
	}
	return problems
}

// joinCategoryProblems 把问题串成一个错误（单行，保留每个原因的 errors.Is）。
func joinCategoryProblems(problems []error) error {
	joined := problems[0]
	for _, problem := range problems[1:] {
		joined = fmt.Errorf("%w; %w", joined, problem)
	}
	return joined
}
