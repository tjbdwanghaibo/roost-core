package skill

// AttributeHandle 等是 catalog 条目的数字句柄：属性、资源、状态、单位模板、碰撞层、伤害类型、元素、玩法标签、
// 共享状态与时间快照 profile。句柄进 gameplay digest，跨版本保持稳定。
type AttributeHandle uint16
type ResourceHandle uint16
type StatusHandle uint16
type UnitTemplateHandle uint16
type CollisionLayerHandle uint16
type DamageTypeHandle uint16
type ElementHandle uint16
type GameplayTagHandle uint16
type SharedStateHandle uint16
type TemporalProfileHandle uint16

// AttributeCatalog 是属性目录；每个 *Catalog 都带 Revision，条目按句柄唯一。
type AttributeCatalog struct {
	Revision string
	Entries  []AttributeCatalogEntry
}

// AttributeCatalogEntry 描述一个属性：键、值类型与量纲、是否可读、允许的快照点与修正操作、上下界与取整。
type AttributeCatalogEntry struct {
	Handle             AttributeHandle
	Key                string
	ValueType          valueKind
	Quantity           quantityKind
	Readable           bool
	Snapshots          []string
	ModifierOperations []string
	Minimum            int64
	Maximum            int64
	Rounding           string
}

// ResourceCatalog 是资源目录。
type ResourceCatalog struct {
	Revision string
	Entries  []ResourceCatalogEntry
}

// ResourceCatalogEntry 描述一个资源及其上下界。
type ResourceCatalogEntry struct {
	Handle           ResourceHandle
	Key              string
	Minimum, Maximum int64
}

// StatusCatalog 是状态（buff）目录。
type StatusCatalog struct {
	Revision string
	Entries  []StatusCatalogEntry
}

// StatusCatalogEntry 描述一个状态的叠层、刷新、驱散、韧性、来源归属、免疫、属性修正与实例操作策略。
type StatusCatalogEntry struct {
	Handle                                                                                                 StatusHandle
	Key, Category, RefreshPolicy, DispelCategory, TenacityPolicy, SourceOwnership, RemovalPolicy, Polarity string
	MaxStacks, DispelPriority                                                                              int
	ImmunityTags                                                                                           []GameplayTagHandle
	GameplayTags                                                                                           []GameplayTagHandle
	AttributeModifiers                                                                                     []StatusAttributeModifier
	PeriodicPolicy                                                                                         string
	Dispellable, Copyable, Transferable, Stealable                                                         bool
	CombatHooks                                                                                            []string
	DurationOperations                                                                                     []string
	MaximumDurationTicks                                                                                   Tick
}

// StatusAttributeModifier 是状态带的一个属性修正。
type StatusAttributeModifier struct {
	Attribute AttributeHandle
	Operation string
	Value     int64
}

// UnitTemplateCatalog 是召唤物单位模板目录。
type UnitTemplateCatalog struct {
	Revision string
	Entries  []UnitTemplateCatalogEntry
}

// UnitTemplateCatalogEntry 描述一个单位模板及其召唤策略。
type UnitTemplateCatalogEntry struct {
	Handle                                               UnitTemplateHandle
	Key, OwnerPolicy, ReplacementPolicy, ControlProfile  string
	OwnerDeathPolicy, SkillRemovedPolicy, MatchEndPolicy string
	MaximumPerOwner                                      int
	MaximumPerSourceSkill                                int
	MaximumPerTeam                                       int
	MaximumSummonCount                                   int
	MaximumLifetimeTicks                                 Tick
	Commands                                             []string
	Behaviors                                            []string
	GameplayTags                                         []GameplayTagHandle
	AllowedAttributeOverrides                            []UnitTemplateAttributeOverridePolicy
	Parameters                                           []UnitTemplateParameterPolicy
	DynamicCollider                                      bool
}

// UnitTemplateAttributeOverridePolicy 是模板允许覆盖的一个属性及其范围。
type UnitTemplateAttributeOverridePolicy struct {
	Attribute        AttributeHandle
	Minimum, Maximum int64
}

// UnitTemplateParameterPolicy 是模板接受的一个具名参数。
type UnitTemplateParameterPolicy struct {
	Name             string
	ValueType        valueKind
	Quantity         quantityKind
	Minimum, Maximum int64
}

// CollisionLayerCatalog 是碰撞层目录，CollisionLayerCatalogEntry 是其中一项。
type CollisionLayerCatalog struct {
	Revision string
	Entries  []CollisionLayerCatalogEntry
}
type CollisionLayerCatalogEntry struct {
	Handle CollisionLayerHandle
	Key    string
}

// DamageTypeCatalog 是伤害类型目录，DamageTypeCatalogEntry 是其中一项。
type DamageTypeCatalog struct {
	Revision string
	Entries  []DamageTypeCatalogEntry
}
type DamageTypeCatalogEntry struct {
	Handle DamageTypeHandle
	Key    string
}

// ElementCatalog 是元素目录，ElementCatalogEntry 是其中一项。
type ElementCatalog struct {
	Revision string
	Entries  []ElementCatalogEntry
}
type ElementCatalogEntry struct {
	Handle             ElementHandle
	Key, MatchupPolicy string
}

// WithTestRevision 返回改了 Revision 的副本，供测试构造不同版本的 catalog。
func (c ElementCatalog) WithTestRevision(revision string) ElementCatalog {
	c.Revision = revision
	c.Entries = append([]ElementCatalogEntry(nil), c.Entries...)
	return c
}

// GameplayTagClass 是玩法标签的类别。
type GameplayTagClass uint8

const (
	GameplayTagDeclarable GameplayTagClass = 1 << iota
	GameplayTagCompilerDerived
	GameplayTagRuntimeOnly
	GameplayTagTargetQueryable
)

// GameplayTagCatalog 是玩法标签目录，GameplayTagCatalogEntry 是其中一项。
type GameplayTagCatalog struct {
	Revision string
	Entries  []GameplayTagCatalogEntry
}
type GameplayTagCatalogEntry struct {
	Handle  GameplayTagHandle
	Key     string
	Classes GameplayTagClass
}

// CombatPolicyCatalog 是战斗策略的取值范围。
type CombatPolicyCatalog struct {
	Revision      string
	DamageTypes   []DamageTypeHandle
	FormulaPolicy string
}

// SharedStateCatalog 是跨技能共享的持久状态目录，SharedStateCatalogEntry 是其中一项。
type SharedStateCatalog struct {
	Revision string
	Entries  []SharedStateCatalogEntry
}
type SharedStateCatalogEntry struct {
	Handle               SharedStateHandle
	Key, Scope           string
	MaximumDurationTicks Tick
	ValueType            valueKind
	Minimum, Maximum     int64
}

// AbilityControlCatalog 是技能实例控制相关的目录。
type AbilityControlCatalog struct {
	Revision       string
	SelectableTags []GameplayTagHandle
	OwnerRelations []string
	Properties     []AbilityPropertyPolicy
}

// AbilityPropertyPolicy 描述技能实例的一个可读 / 可改属性。
type AbilityPropertyPolicy struct {
	Property                          string
	ValueType                         valueKind
	Mutable                           bool
	Minimum, Maximum, MaximumMutation int64
	MaximumDurationTicks              Tick
}

// TemporalSnapshotCatalog 是时间快照 profile 目录，TemporalSnapshotProfile 是其中一项。
type TemporalSnapshotCatalog struct {
	Revision string
	Entries  []TemporalSnapshotProfile
}
type TemporalSnapshotProfile struct {
	Handle                                            TemporalProfileHandle
	Key                                               string
	Fields                                            []string
	MaximumAgeTicks                                   Tick
	MaximumPerOwner                                   int
	RestorePolicy, EventPolicy, BlockedPositionPolicy string
	AllowRevive                                       bool
}

// GameplayCatalog 汇总编译环境里的全部玩法目录。
type GameplayCatalog struct {
	Attributes    AttributeCatalog
	Resources     ResourceCatalog
	Statuses      StatusCatalog
	UnitTemplates UnitTemplateCatalog
	Collision     CollisionLayerCatalog
	DamageTypes   DamageTypeCatalog
	Elements      ElementCatalog
	Tags          GameplayTagCatalog
	Combat        CombatPolicyCatalog
	SharedStates  SharedStateCatalog
	Abilities     AbilityControlCatalog
	Temporal      TemporalSnapshotCatalog
}

// MotionCapabilityCatalog 是运动能力目录：允许的衍生物种类、轨迹组合与各运动变体。
type MotionCapabilityCatalog struct {
	Revision             string
	SpawnTrajectoryPairs []MotionSpawnTrajectoryPair
	VariantCapabilities  []MotionVariantCapability
	MaximumSpeed         int64
	MaximumDistance      int64
	MaximumAngularSpeed  int64
	MaximumTrackingTicks Tick
}

// MotionSpawnTrajectoryPair 是一对允许组合的衍生物种类与轨迹；MotionVariantCapability 是一个运动变体的能力。
type MotionSpawnTrajectoryPair struct{ Spawn, Trajectory string }
type MotionVariantCapability struct {
	Spawn, Trajectory                             string
	Frames, Steering, Offsets, CollisionResponses []string
	Carry                                         bool
	Completions                                   []string
}

// SpawnPropertyCatalog 是衍生物数值属性目录。
type SpawnPropertyCatalog struct {
	Revision   string
	Properties []SpawnPropertyPolicy
}

// SpawnPropertyHandle 是衍生物数值属性的句柄；SpawnPropertySlotBinding 与 SpawnPropertyPolicy 描述它的绑定与策略。
type SpawnPropertyHandle uint16
type SpawnPropertySlotBinding struct {
	Stage, Variant, Field string
}
type SpawnPropertyPolicy struct {
	Handle           SpawnPropertyHandle
	Key              string
	Minimum, Maximum int64
	SpawnKinds       []string
	Operations       []string
	Interpolation    string
	Rounding         string
	SlotBindings     []SpawnPropertySlotBinding
}

// VisualCatalog 是表现目录：主题、类别、元素与上限，Digest 由内容算出。
type VisualCatalog struct {
	Revision, Digest string
	Themes           []string // compact compatibility projection of Categories.
	Categories       map[string]VisualCategoryDescriptor
	Elements         map[string]VisualElementDescriptor
	Limits           VisualLimits
}

// VisualLimits 等描述表现目录的上限与各项描述符。
type VisualLimits struct {
	MaxIconKeywords, MaxVisualRefs, MaxElementsPerRef, MaxCategoryBytes, MaxThemeBytes, MaxElementBytes, MaxKeywordBytes, MaxManifestEntries int
}
type VisualElementDescriptor struct{ Key string }
type VisualCategoryDescriptor struct {
	Key    string
	Themes map[string]VisualThemeDescriptor
}
type VisualThemeDescriptor struct {
	Key              string
	AllowedEffects   []string
	AllowedElements  []string
	RequiredElements int
	ClientPackageKey string
}

// WithTestRevision 返回改了 Revision（并重算 Digest）的副本，供测试构造不同版本的 catalog。
func (c VisualCatalog) WithTestRevision(revision string) VisualCatalog {
	c.Revision = revision
	c.Digest = digestStrings("visual", revision, c.Themes)
	c.Themes = append([]string(nil), c.Themes...)
	c.Categories = cloneVisualCategories(c.Categories)
	c.Elements = cloneVisualElements(c.Elements)
	return c
}

// NumericAuthority 是编译环境约定的数值刻度：距离、角度、基点、整数位宽、tick 单位与默认取整。
type NumericAuthority struct {
	WorldDistanceScale, MillidegreesPerDegree, BasisPointsScale, SignedIntegerBits int
	TickUnit, DefaultRounding                                                      string
}

// CompileLimits 是编译期的各项预算上限。
type CompileLimits struct {
	MaxPhases, MaxFlowNodes, MaxFlowDepth, MaxValueNodes, MaxRepeat, MaxTargets, MaxSpawns, MaxSchedules, MaxMutations                                                                                                                                                                                                                                                                                                                                                              int
	MaxLifetimeTicks                                                                                                                                                                                                                                                                                                                                                                                                                                                                Tick
	MaxMotionOffsets, MaxReflects, MaxPierces, MaxCarryTargets, MaxVisualRefs, MaxGameplayTags, MaxProcDepth, MaxEventsPerRoot, MaxRandomSites, MaxPassiveActivationsPerTick, MaxAreaMembers, MaxStatusStacks, MaxPersistentStates, MaxStateInstancesPerOwner, MaxAbilitySelections, MaxAbilityMutations, MaxOwnedEntities, MaxOwnedSpawns, MaxStatusSelections, MaxStatusMutations, MaxEffectResultSlots, MaxLocalFrames, MaxInputPathPoints, MaxTemporalSnapshots, MaxStringBytes int
	MaxInputPathLength                                                                                                                                                                                                                                                                                                                                                                                                                                                              int64
	MaxTemporalSnapshotAge                                                                                                                                                                                                                                                                                                                                                                                                                                                          Tick
}

// CompileEnvironment 是编译所依据的权威环境：语义修订、目录、能力表与上限，Digest 由 AuthorityDigest 封装。
type CompileEnvironment struct {
	CompilerSemanticsRevision string
	Revision                  string
	Digest                    string
	Limits                    CompileLimits
	Numeric                   NumericAuthority
	Gameplay                  GameplayCatalog
	Motion                    MotionCapabilityCatalog
	SpawnProperties           SpawnPropertyCatalog
	// Host 是 Host 取值能力表里业务声明的部分，随环境下发、算进 authority digest；
	// 完整的表用 HostCapabilityTableOf 取。
	Host   HostCapabilityCatalog
	Visual VisualCatalog
}

// DefaultCompileEnvironment 返回内置的默认编译环境（默认 catalog、全量能力表与默认上限）。
func DefaultCompileEnvironment() CompileEnvironment {
	environment := CompileEnvironment{
		CompilerSemanticsRevision: "skillv2-compiler-2", Revision: "gameplay-default-1",
		Limits: defaultCompileLimits(), Numeric: NumericAuthority{WorldDistanceScale: 1000, MillidegreesPerDegree: 1000, BasisPointsScale: 10000, SignedIntegerBits: 64, TickUnit: "logical_tick", DefaultRounding: "half_away_from_zero"},
		Gameplay: defaultGameplayCatalog(), Motion: defaultMotionCapabilityCatalog(), SpawnProperties: defaultSpawnPropertyCatalog(), Host: FullHostCapabilityCatalog(), Visual: defaultVisualCatalog(),
	}
	environment.Visual.Digest = digestStrings("visual", environment.Visual.Revision, environment.Visual.Themes)
	environment.Digest = authorityDigest(environment)
	return environment
}

func defaultVisualCatalog() VisualCatalog {
	elements := []string{"default", "fire", "ice", "thunder", "holy", "dark", "nature", "wind", "earth", "water", "arcane"}
	result := VisualCatalog{Revision: "visual-1", Themes: []string{"default"}, Categories: make(map[string]VisualCategoryDescriptor), Elements: make(map[string]VisualElementDescriptor), Limits: VisualLimits{MaxIconKeywords: 5, MaxVisualRefs: 64, MaxElementsPerRef: 2, MaxCategoryBytes: 32, MaxThemeBytes: 64, MaxElementBytes: 32, MaxKeywordBytes: 32, MaxManifestEntries: 64}}
	for _, element := range elements {
		result.Elements[element] = VisualElementDescriptor{Key: element}
	}
	for category, effects := range map[string][]string{
		"cast": {"cast"}, "impact": {"damage", "heal", "resource", "shield", "status", "attribute_modifier", "teleport", "knockback", "pull", "stop_movement"},
		"attachment": {"shield", "status", "attribute_modifier"}, "movement": {"teleport", "knockback", "pull", "stop_movement"}, "projectile": {"projectile"}, "beam": {"beam"}, "area": {"area"}, "summon": {"summon"},
	} {
		result.Categories[category] = VisualCategoryDescriptor{Key: category, Themes: map[string]VisualThemeDescriptor{"default": {Key: "default", AllowedEffects: append([]string(nil), effects...), AllowedElements: append([]string(nil), elements...), RequiredElements: 1, ClientPackageKey: "client.visual." + category + ".default"}}}
	}
	result.Digest = digestStrings("visual", result.Revision, result.Themes)
	return result
}

func cloneVisualCategories(source map[string]VisualCategoryDescriptor) map[string]VisualCategoryDescriptor {
	result := make(map[string]VisualCategoryDescriptor, len(source))
	for key, category := range source {
		clone := VisualCategoryDescriptor{Key: category.Key, Themes: make(map[string]VisualThemeDescriptor, len(category.Themes))}
		for themeKey, theme := range category.Themes {
			theme.AllowedEffects = append([]string(nil), theme.AllowedEffects...)
			theme.AllowedElements = append([]string(nil), theme.AllowedElements...)
			clone.Themes[themeKey] = theme
		}
		result[key] = clone
	}
	return result
}

func cloneVisualElements(source map[string]VisualElementDescriptor) map[string]VisualElementDescriptor {
	result := make(map[string]VisualElementDescriptor, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func defaultSpawnPropertyCatalog() SpawnPropertyCatalog {
	operations := func() []string { return []string{"set", "add", "mul_bp"} }
	policy := func(handle SpawnPropertyHandle, key string, minimum, maximum int64, spawnKinds []string, bindings ...SpawnPropertySlotBinding) SpawnPropertyPolicy {
		return SpawnPropertyPolicy{Handle: handle, Key: key, Minimum: minimum, Maximum: maximum, SpawnKinds: spawnKinds, Operations: operations(), Interpolation: "linear_integer", Rounding: "truncate_toward_zero", SlotBindings: bindings}
	}
	return SpawnPropertyCatalog{Revision: "spawn-2", Properties: []SpawnPropertyPolicy{
		policy(1, "speed", 0, 100000, []string{"dash", "projectile", "area"}, SpawnPropertySlotBinding{"trajectory", "linear", "speed"}, SpawnPropertySlotBinding{"trajectory", "path", "speed"}, SpawnPropertySlotBinding{"trajectory", "parabola", "speed"}),
		policy(2, "radius", 0, 1000000, []string{"orbit"}, SpawnPropertySlotBinding{"trajectory", "orbit", "radius"}),
		policy(3, "arc_height", 0, 1000000, []string{"projectile"}, SpawnPropertySlotBinding{"trajectory", "parabola", "height"}),
		policy(4, "turn_rate_mdeg_per_tick", 0, 360000, []string{"projectile"}, SpawnPropertySlotBinding{"steering", "tracking", "turn_rate_mdeg_per_tick"}),
		policy(5, "angular_speed_mdeg_per_tick", -360000, 360000, []string{"orbit", "projectile"}, SpawnPropertySlotBinding{"trajectory", "orbit", "angular_speed"}, SpawnPropertySlotBinding{"offset", "circular", "angular_speed"}),
		policy(6, "offset_amplitude", 0, 1000000, []string{"projectile"}, SpawnPropertySlotBinding{"offset", "zigzag", "amplitude"}),
		policy(7, "offset_radius", 0, 1000000, []string{"orbit", "projectile"}, SpawnPropertySlotBinding{"offset", "circular", "radius"}),
		policy(8, "return_speed_bp", 0, 100000, []string{"projectile"}, SpawnPropertySlotBinding{"completion", "boomerang", "return_speed_bp"}),
		policy(9, "collision_force", 0, 1000000, []string{"dash", "projectile", "area"}, SpawnPropertySlotBinding{"collision", "present", "force"}),
	}}
}

func defaultMotionCapabilityCatalog() MotionCapabilityCatalog {
	projectile := func(trajectory string) MotionVariantCapability {
		return MotionVariantCapability{Spawn: "projectile", Trajectory: trajectory, Frames: []string{"world", "follow"}, Steering: []string{"fixed", "tracking"}, Offsets: []string{"zigzag", "circular"}, CollisionResponses: []string{"stop", "reflect", "pierce"}, Carry: true, Completions: []string{"end", "pause_then_end", "boomerang"}}
	}
	return MotionCapabilityCatalog{
		Revision: "motion-2",
		SpawnTrajectoryPairs: []MotionSpawnTrajectoryPair{
			{"dash", "linear"}, {"orbit", "orbit"}, {"projectile", "linear"}, {"projectile", "path"}, {"projectile", "parabola"}, {"area", "stationary"}, {"area", "linear"}, {"beam", "stationary"},
		},
		VariantCapabilities: []MotionVariantCapability{
			{Spawn: "dash", Trajectory: "linear", Frames: []string{"world", "follow"}, Steering: []string{"fixed"}, CollisionResponses: []string{"stop"}, Carry: true, Completions: []string{"end", "pause_then_end"}},
			{Spawn: "orbit", Trajectory: "orbit", Frames: []string{"world"}, Offsets: []string{"circular"}, Completions: []string{"end", "pause_then_end"}},
			projectile("linear"), projectile("path"), projectile("parabola"),
			{Spawn: "area", Trajectory: "stationary", Frames: []string{"world", "follow"}, Completions: []string{"end", "pause_then_end"}},
			{Spawn: "area", Trajectory: "linear", Frames: []string{"world", "follow"}, Steering: []string{"fixed"}, CollisionResponses: []string{"stop"}, Completions: []string{"end", "pause_then_end"}},
			{Spawn: "beam", Trajectory: "stationary", Frames: []string{"world", "follow"}, Completions: []string{"end"}},
		},
		MaximumSpeed: 100000, MaximumDistance: 100000, MaximumAngularSpeed: 100000, MaximumTrackingTicks: 36000,
	}
}

func defaultCompileLimits() CompileLimits {
	return CompileLimits{MaxPhases: 32, MaxFlowNodes: 1024, MaxFlowDepth: 64, MaxValueNodes: 4096, MaxRepeat: 64, MaxTargets: 128, MaxSpawns: 128, MaxSchedules: 2048, MaxMutations: 4096, MaxLifetimeTicks: 36000, MaxMotionOffsets: 8, MaxReflects: 16, MaxPierces: 32, MaxCarryTargets: 8, MaxVisualRefs: 256, MaxGameplayTags: 256, MaxProcDepth: 8, MaxEventsPerRoot: 1024, MaxRandomSites: 256, MaxPassiveActivationsPerTick: 256, MaxAreaMembers: 128, MaxStatusStacks: 128, MaxPersistentStates: 64, MaxStateInstancesPerOwner: 1024, MaxAbilitySelections: 64, MaxAbilityMutations: 128, MaxOwnedEntities: 128, MaxOwnedSpawns: 128, MaxStatusSelections: 128, MaxStatusMutations: 256, MaxEffectResultSlots: 128, MaxLocalFrames: 4096, MaxInputPathPoints: 64, MaxInputPathLength: 100000, MaxTemporalSnapshots: 16, MaxTemporalSnapshotAge: 3600, MaxStringBytes: 4096}
}

func defaultGameplayCatalog() GameplayCatalog {
	attributes := []AttributeCatalogEntry{
		{Handle: 1, Key: "health", ValueType: valueKindInt, Quantity: quantityCombatAmount, Readable: true, Snapshots: []string{"cast_start", "phase_start", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 1000000, Rounding: "half_away_from_zero"},
		{Handle: 2, Key: "ability_power", ValueType: valueKindInt, Quantity: quantityCombatAmount, Readable: true, Snapshots: []string{"cast_start", "phase_start", "spawn_start", "each_tick", "on_hit", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 1000000, Rounding: "half_away_from_zero"},
		{Handle: 3, Key: "move_speed", ValueType: valueKindInt, Quantity: quantitySpeedWorldPerTick, Readable: true, Snapshots: []string{"cast_start", "phase_start", "spawn_start", "each_tick", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 100000, Rounding: "toward_zero"},
	}
	return GameplayCatalog{
		Attributes: AttributeCatalog{Revision: "attributes-1", Entries: attributes},
		Resources:  ResourceCatalog{Revision: "resources-1", Entries: []ResourceCatalogEntry{{Handle: 1, Key: "mana", Minimum: 0, Maximum: 100000}}},
		Statuses: StatusCatalog{Revision: "statuses-1", Entries: []StatusCatalogEntry{
			{Handle: 1, Key: "slow", Category: "control", RefreshPolicy: "refresh", MaxStacks: 1, DispelCategory: "debuff", TenacityPolicy: "scale_duration", SourceOwnership: "source", RemovalPolicy: "expire", Polarity: "negative", DispelPriority: 10, AttributeModifiers: []StatusAttributeModifier{{Attribute: 3, Operation: "mul_bp", Value: 8000}}, PeriodicPolicy: "none", Dispellable: true, Copyable: true, Transferable: true, Stealable: true, DurationOperations: []string{"add_duration", "set_duration", "mul_duration_bp", "refresh"}, MaximumDurationTicks: 600},
			{Handle: 2, Key: "shield", Category: "shield", RefreshPolicy: "stack", MaxStacks: 1, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "source", RemovalPolicy: "consume", Polarity: "positive", DispelPriority: 20, PeriodicPolicy: "none", Dispellable: true, Transferable: true, DurationOperations: []string{"add_duration", "set_duration", "mul_duration_bp", "refresh"}, MaximumDurationTicks: 600},
		}},
		UnitTemplates: UnitTemplateCatalog{Revision: "units-1", Entries: []UnitTemplateCatalogEntry{{Handle: 1, Key: "deployable.trap", OwnerPolicy: "owner", OwnerDeathPolicy: "dismiss", SkillRemovedPolicy: "dismiss", MatchEndPolicy: "dismiss", MaximumPerOwner: 8, MaximumPerSourceSkill: 8, MaximumPerTeam: 8, MaximumSummonCount: 8, MaximumLifetimeTicks: 600, ReplacementPolicy: "replace_oldest", ControlProfile: "trap-basic", Commands: []string{"hold_position", "dismiss"}, Behaviors: []string{"armed"}, GameplayTags: []GameplayTagHandle{1}}}},
		Collision:     CollisionLayerCatalog{Revision: "collision-1", Entries: []CollisionLayerCatalogEntry{{Handle: 1, Key: "terrain"}}},
		DamageTypes:   DamageTypeCatalog{Revision: "damage-1", Entries: []DamageTypeCatalogEntry{{Handle: 1, Key: "physical"}, {Handle: 2, Key: "magic"}, {Handle: 3, Key: "true"}}},
		Elements:      ElementCatalog{Revision: "elements-1", Entries: []ElementCatalogEntry{{Handle: 1, Key: "neutral", MatchupPolicy: "neutral"}, {Handle: 2, Key: "fire", MatchupPolicy: "elemental"}}},
		Tags:          GameplayTagCatalog{Revision: "tags-1", Entries: []GameplayTagCatalogEntry{{Handle: 1, Key: "spell", Classes: GameplayTagDeclarable | GameplayTagTargetQueryable}, {Handle: 2, Key: "projectile", Classes: GameplayTagCompilerDerived}, {Handle: 3, Key: "critical", Classes: GameplayTagRuntimeOnly}}},
		Combat:        CombatPolicyCatalog{Revision: "combat-1", DamageTypes: []DamageTypeHandle{1, 2, 3}, FormulaPolicy: "twelve_stage_v1"},
		SharedStates:  SharedStateCatalog{Revision: "state-1", Entries: []SharedStateCatalogEntry{{Handle: 1, Key: "shared.combo", Scope: "owner", MaximumDurationTicks: 300, ValueType: valueKindInt, Minimum: 0, Maximum: 10}}},
		Abilities: AbilityControlCatalog{Revision: "ability-1", SelectableTags: []GameplayTagHandle{1}, OwnerRelations: []string{"self"}, Properties: []AbilityPropertyPolicy{
			{Property: "cooldown_remaining_ticks", ValueType: valueKindInt, Mutable: true, Minimum: 0, Maximum: 36000, MaximumMutation: 36000},
			{Property: "cooldown_total_ticks", ValueType: valueKindInt, Minimum: 0, Maximum: 36000},
			{Property: "ammo_stock", ValueType: valueKindInt, Mutable: true, Minimum: 0, Maximum: 999, MaximumMutation: 999},
			{Property: "ammo_max_stock", ValueType: valueKindInt, Minimum: 0, Maximum: 999},
			{Property: "enabled", ValueType: valueKindBool, Mutable: true, Minimum: 0, Maximum: 1, MaximumMutation: 1, MaximumDurationTicks: 36000},
			{Property: "cast_active", ValueType: valueKindBool, Minimum: 0, Maximum: 1},
			{Property: "last_commit_tick", ValueType: valueKindInt, Minimum: 0, Maximum: 1 << 62},
			{Property: "last_finish_tick", ValueType: valueKindInt, Minimum: 0, Maximum: 1 << 62},
		}},
		Temporal: TemporalSnapshotCatalog{Revision: "temporal-1", Entries: []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.position_health", Fields: []string{"position", "health"}, MaximumAgeTicks: 600, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: "fail"}}},
	}
}
