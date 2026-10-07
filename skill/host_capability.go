package skill

import (
	"fmt"
	"sort"
	"strconv"
)

// Host 取值能力表（维护者决定 B3 ③，docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md）。
//
// 以前编译器、Runtime、Host 各自维护一份“能读什么、支持什么”的集合：编译器按 catalog 与自己
// 写死的集合放行，Runtime 在施法中途才发现 Host 没实现召唤物接口（ErrHostContractViolation），
// Host 对没见过的属性 / 资源静默返回 0。现在三处共用一张表：
//
//   - 环境：CompileEnvironment.Host（HostCapabilityCatalog）随环境下发、算进 authority digest；
//     可读属性与资源两列直接取 Gameplay catalog（属性的 Readable、全部资源），不另抄一份。
//     HostCapabilityTableOf(environment) 把两部分拼成完整的表，是编译器唯一的读取入口。
//   - 编译器：按表拒绝表外的取值（HOST_CAPABILITY_MISSING，点名缺哪一项），并把 Program 用到的
//     能力列表（hostRequirements）交给 Runtime。
//   - Runtime：Program 第一次在这个 Runtime 上启动 / 注册 / 恢复时，核对 hostRequirements 都在
//     Host 声明的表里（HostCapabilityProvider），缺了返回 ErrHostCapabilityMissing。
//   - Host：MemoryHost 与 combatcomponent.HostAdapter 各自给出能力表，对表外的属性 / 资源报错而
//     不是返回 0；CheckHostCapabilities 对表里每一项调用 Host，核对它真的支持。

// HostCapabilityCatalog 是环境里由业务声明的那部分能力（catalog 推不出来的）。每一列都是封闭
// 集合的子集，封闭集合只在 hostCapabilityColumns 里写一次。
type HostCapabilityCatalog struct {
	Revision string
	// SpawnKinds：Host 能承载的衍生物 kind（dash / orbit / projectile / area / beam / minion）。
	SpawnKinds []string
	// MotionSteps：StepSpawn 接受的可选 motion 步骤（frame / steering / offsets / collision /
	// carry / completion）。trajectory、static 与 signals 是每个衍生物都会发的基本步骤，不列。
	MotionSteps []string
	// SpawnNumericFields：只由 Host 消费的衍生物数值字段（SpawnNumericSnapshot 里 Runtime 自己
	// 不用来算运动的：turn_rate_mdeg_per_tick / return_speed_bp / collision_force）。没声明的字段
	// 写 numeric_tracks / modify_spawn 会在编译期被拒——以前它们编译通过、Host 不读，静默不生效。
	SpawnNumericFields []string
	// ResourceOperations：resource 效果的 operation（set / add / spend / sub）。
	ResourceOperations []string
	// ModifierOperations：attribute_modifier 效果的 operation（add / mul_bp）。属性自己的
	// AttributeCatalogEntry.ModifierOperations 是玩法策略，两者都要满足。
	ModifierOperations []string
	// Summon 声明召唤生态支持：summon 事务需 OwnedEntityRuntimeHost；
	// 召唤物命令通过 Apply，owned_entities 通过 Select，不能只实现该可选接口。
	Summon bool
}

// HostCapabilityTable 描述可配置的能力列：可读属性与资源两列来自 Gameplay catalog，其余来自
// HostCapabilityCatalog。环境用 HostCapabilityTableOf 得到它；Host 用 HostCapabilityProvider 声明它。
type HostCapabilityTable struct {
	// Attributes：Host 能答 AttributeRead 的属性 key（read_attribute、快照、attribute_compare 过滤）。
	Attributes []string
	// Resources：Host 能付费、改动、读取的资源 key（cost、sustain cost、resource 效果）。
	Resources []string
	HostCapabilityCatalog
}

// HostCapabilityProvider 是 Host 声明能力表的接口，并入了 Host 接口：每个 Host 都必须声明
// （没有“未实现则跳过”的分支）。Runtime 在 Program 第一次使用时用它核对 Program 的能力需求；
// 包装型 Host 转发被包装 Host 的表（RecordingHost 转发并记录，ReplayHost 按记录回放）。
// 能力表在 Host 生命周期内不应变化。
type HostCapabilityProvider interface {
	HostCapabilities() HostCapabilityTable
}

// HostCapabilityKind 是能力表的列名，也是诊断与错误里点名的前缀。
type HostCapabilityKind string

const (
	HostCapabilityAttribute         HostCapabilityKind = "attribute"
	HostCapabilityResource          HostCapabilityKind = "resource"
	HostCapabilitySpawnKind         HostCapabilityKind = "spawn_kind"
	HostCapabilityMotionStep        HostCapabilityKind = "motion_step"
	HostCapabilitySpawnNumericField HostCapabilityKind = "spawn_numeric_field"
	HostCapabilityResourceOperation HostCapabilityKind = "resource_operation"
	HostCapabilityModifierOperation HostCapabilityKind = "modifier_operation"
	HostCapabilitySummon            HostCapabilityKind = "summon"
)

// HostCapability 是表里的一项：一列加一个 key（summon 列没有 key）。
type HostCapability struct {
	Kind HostCapabilityKind
	Key  string
}

func (capability HostCapability) String() string {
	if capability.Key == "" {
		return string(capability.Kind)
	}
	return string(capability.Kind) + " " + strconv.Quote(capability.Key)
}

// ErrHostCapabilityMissing：Program 需要的能力不在 Host 声明的表里，或 Host 被要求做表外的事。
// 它也是 ErrHostContractViolation。
var ErrHostCapabilityMissing = fmt.Errorf("%w: host capability missing", ErrHostContractViolation)

// hostCapabilityColumn 描述表的一列：在表里的位置与封闭集合。整张表的列只在这里登记一次，
// Has / Items / 环境校验 / 合并 / 守卫测试都遍历它。
type hostCapabilityColumn struct {
	kind HostCapabilityKind
	// keys 指向表里这一列；summon 列是布尔量，keys 为 nil。
	keys func(*HostCapabilityTable) *[]string
	// closed 是这一列的封闭集合；attribute / resource 由 catalog 定义，为 nil。
	closed []string
	// envPath 是这一列在环境 JSON 形状里的路径（只对 HostCapabilityCatalog 的列有意义）。
	envPath string
}

var hostCapabilityColumns = []hostCapabilityColumn{
	{kind: HostCapabilityAttribute, keys: func(table *HostCapabilityTable) *[]string { return &table.Attributes }},
	{kind: HostCapabilityResource, keys: func(table *HostCapabilityTable) *[]string { return &table.Resources }},
	{kind: HostCapabilitySpawnKind, keys: func(table *HostCapabilityTable) *[]string { return &table.SpawnKinds }, closed: []string{"dash", "orbit", "projectile", "area", "beam", "minion"}, envPath: "$.host.spawn_kinds"},
	{kind: HostCapabilityMotionStep, keys: func(table *HostCapabilityTable) *[]string { return &table.MotionSteps }, closed: []string{"frame", "steering", "offsets", "collision", "carry", "completion"}, envPath: "$.host.motion_steps"},
	{kind: HostCapabilitySpawnNumericField, keys: func(table *HostCapabilityTable) *[]string { return &table.SpawnNumericFields }, closed: hostOnlySpawnNumericFields, envPath: "$.host.spawn_numeric_fields"},
	{kind: HostCapabilityResourceOperation, keys: func(table *HostCapabilityTable) *[]string { return &table.ResourceOperations }, closed: []string{"set", "add", "spend", "sub"}, envPath: "$.host.resource_operations"},
	{kind: HostCapabilityModifierOperation, keys: func(table *HostCapabilityTable) *[]string { return &table.ModifierOperations }, closed: []string{"add", "mul_bp"}, envPath: "$.host.modifier_operations"},
	{kind: HostCapabilitySummon},
}

// hostOnlySpawnNumericFields：衍生物数值属性里 Runtime 不用来算运动、只经 SpawnNumericSnapshot
// 交给 Host 的那几个（spawnNumericBaseValue 对它们没有基础值）。守卫
// TestHostOnlySpawnNumericFieldsMatchRuntime 核对这张表与 Runtime 的实际用法一致。
var hostOnlySpawnNumericFields = []string{"turn_rate_mdeg_per_tick", "return_speed_bp", "collision_force"}

func hostCapabilityColumnOf(kind HostCapabilityKind) (hostCapabilityColumn, bool) {
	for _, column := range hostCapabilityColumns {
		if column.kind == kind {
			return column, true
		}
	}
	return hostCapabilityColumn{}, false
}

// HostCapabilityTableOf 返回环境的完整能力表：Gameplay catalog 里 Readable 的属性、全部资源，
// 加上 environment.Host。编译器只经这个函数读取 Host 能力。
func HostCapabilityTableOf(environment CompileEnvironment) HostCapabilityTable {
	table := HostCapabilityTable{HostCapabilityCatalog: cloneHostCapabilityCatalog(environment.Host)}
	table.Attributes, table.Resources = catalogHostCapabilities(environment.Gameplay)
	return table
}

// CatalogHostCapabilities 返回 Gameplay catalog 推出的两列（可读属性、资源），其余列为空。
// 给按 catalog 声明能力的 Host（如 combatcomponent.HostAdapter）用，与环境侧是同一条规则。
func CatalogHostCapabilities(catalog GameplayCatalog) HostCapabilityTable {
	attributes, resources := catalogHostCapabilities(catalog)
	return HostCapabilityTable{Attributes: attributes, Resources: resources}
}

// catalogHostCapabilities 是可读属性与资源两列的唯一推导：Readable 的属性 key、全部资源 key。
func catalogHostCapabilities(catalog GameplayCatalog) (attributes, resources []string) {
	attributes = make([]string, 0, len(catalog.Attributes.Entries))
	for _, entry := range catalog.Attributes.Entries {
		if entry.Readable {
			attributes = append(attributes, entry.Key)
		}
	}
	resources = make([]string, 0, len(catalog.Resources.Entries))
	for _, entry := range catalog.Resources.Entries {
		resources = append(resources, entry.Key)
	}
	return attributes, resources
}

// FullHostCapabilityCatalog 声明每一列封闭集合里的全部取值与召唤物。默认环境、MemoryHost
// 用它；只实现一部分的 Host 从它删减或自己列。
func FullHostCapabilityCatalog() HostCapabilityCatalog {
	catalog := HostCapabilityCatalog{Revision: "host-1", Summon: true}
	table := HostCapabilityTable{HostCapabilityCatalog: catalog}
	for _, column := range hostCapabilityColumns {
		if column.keys != nil && column.closed != nil {
			*column.keys(&table) = append([]string(nil), column.closed...)
		}
	}
	return table.HostCapabilityCatalog
}

func cloneHostCapabilityCatalog(catalog HostCapabilityCatalog) HostCapabilityCatalog {
	result := catalog
	result.SpawnKinds = append([]string(nil), catalog.SpawnKinds...)
	result.MotionSteps = append([]string(nil), catalog.MotionSteps...)
	result.SpawnNumericFields = append([]string(nil), catalog.SpawnNumericFields...)
	result.ResourceOperations = append([]string(nil), catalog.ResourceOperations...)
	result.ModifierOperations = append([]string(nil), catalog.ModifierOperations...)
	return result
}

// Has 报告表里有没有这一项。
func (table HostCapabilityTable) Has(capability HostCapability) bool {
	column, ok := hostCapabilityColumnOf(capability.Kind)
	if !ok {
		return false
	}
	if column.keys == nil {
		return table.Summon
	}
	return containsString(*column.keys(&table), capability.Key)
}

// Items 按列序列出表里的全部项（列内按声明顺序）。
func (table HostCapabilityTable) Items() []HostCapability {
	items := make([]HostCapability, 0)
	for _, column := range hostCapabilityColumns {
		if column.keys == nil {
			if table.Summon {
				items = append(items, HostCapability{Kind: column.kind})
			}
			continue
		}
		for _, key := range *column.keys(&table) {
			items = append(items, HostCapability{Kind: column.kind, Key: key})
		}
	}
	return items
}

// Missing 返回 required 里不在表中的项，保持 required 的顺序。
func (table HostCapabilityTable) Missing(required []HostCapability) []HostCapability {
	var missing []HostCapability
	for _, capability := range required {
		if !table.Has(capability) {
			missing = append(missing, capability)
		}
	}
	return missing
}

// MergeHostCapabilities 把几部分 Host 各自声明的表合成一张（取并集）：业务 Host 把
// combatcomponent.HostAdapter 的表与自己负责的部分（选择、运动、召唤物）合起来声明。
func MergeHostCapabilities(parts ...HostCapabilityTable) HostCapabilityTable {
	var result HostCapabilityTable
	for _, part := range parts {
		for _, column := range hostCapabilityColumns {
			if column.keys == nil {
				result.Summon = result.Summon || part.Summon
				continue
			}
			target := column.keys(&result)
			for _, key := range *column.keys(&part) {
				if !containsString(*target, key) {
					*target = append(*target, key)
				}
			}
		}
		if result.Revision == "" {
			result.Revision = part.Revision
		}
	}
	return result
}

// HostSupportsEnvironment 核对 Host 声明的表覆盖环境的表（启动时调用一次即可）：返回 Host 缺的
// 每一项。业务在装配时调用，缺项时拒绝启动，而不是等某个技能施法到一半。
func HostSupportsEnvironment(provider HostCapabilityProvider, environment CompileEnvironment) error {
	missing := provider.HostCapabilities().Missing(HostCapabilityTableOf(environment).Items())
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: host lacks %s", ErrHostCapabilityMissing, joinHostCapabilities(missing))
}

func joinHostCapabilities(items []HostCapability) string {
	text := ""
	for index, item := range items {
		if index > 0 {
			text += ", "
		}
		text += item.String()
	}
	return text
}

// validateHostCapabilityCatalog 校验环境里的能力声明：每列取值唯一、在封闭集合里；声明了
// minion 衍生物就必须声明召唤物。
func validateHostCapabilityCatalog(catalog HostCapabilityCatalog, diagnostics *[]Diagnostic) {
	if catalog.Revision == "" {
		appendDiagnostic(diagnostics, DiagnosticCatalogHostPolicy, "$.host.revision", "host capability catalog revision is required")
	}
	table := HostCapabilityTable{HostCapabilityCatalog: catalog}
	for _, column := range hostCapabilityColumns {
		if column.keys == nil || column.closed == nil {
			continue
		}
		values := *column.keys(&table)
		if !uniqueNonEmptyStrings(values) {
			appendDiagnostic(diagnostics, DiagnosticCatalogHostPolicy, column.envPath, "host capabilities must be unique and non-empty")
		}
		for index, value := range values {
			if !containsString(column.closed, value) {
				appendDiagnostic(diagnostics, DiagnosticCatalogHostPolicy, fmt.Sprintf("%s[%d]", column.envPath, index), fmt.Sprintf("%s %q is not a host capability", column.kind, value))
			}
		}
	}
	if containsString(catalog.SpawnKinds, "minion") && !catalog.Summon {
		appendDiagnostic(diagnostics, DiagnosticCatalogHostPolicy, "$.host.summon", "minion spawns require the summon capability")
	}
}

// hostRequirement 是编译期收集到的一项需求，带第一次用到它的源路径。
type hostRequirement struct {
	capability HostCapability
	path       string
}

// sortedHostCapabilities 按列序、key 排序并去重，作为 Program 的能力需求。
func sortedHostCapabilities(requirements []hostRequirement) []HostCapability {
	order := make(map[HostCapabilityKind]int, len(hostCapabilityColumns))
	for index, column := range hostCapabilityColumns {
		order[column.kind] = index
	}
	seen := make(map[HostCapability]bool, len(requirements))
	result := make([]HostCapability, 0, len(requirements))
	for _, requirement := range requirements {
		if !seen[requirement.capability] {
			seen[requirement.capability] = true
			result = append(result, requirement.capability)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return order[result[i].Kind] < order[result[j].Kind]
		}
		return result[i].Key < result[j].Key
	})
	return result
}
