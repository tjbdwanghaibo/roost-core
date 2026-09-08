package skill

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

type AttributeCatalog struct {
	Revision string
	Entries  []AttributeCatalogEntry
}
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

type ResourceCatalog struct {
	Revision string
	Entries  []ResourceCatalogEntry
}
type ResourceCatalogEntry struct {
	Handle           ResourceHandle
	Key              string
	Minimum, Maximum int64
}

type StatusCatalog struct {
	Revision string
	Entries  []StatusCatalogEntry
}
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
type StatusAttributeModifier struct {
	Attribute AttributeHandle
	Operation string
	Value     int64
}

type UnitTemplateCatalog struct {
	Revision string
	Entries  []UnitTemplateCatalogEntry
}
type UnitTemplateCatalogEntry struct {
	Handle                                               UnitTemplateHandle
	Key, OwnerPolicy, ReplacementPolicy, ControlProfile  string
	OwnerDeathPolicy, SkillRemovedPolicy, MatchEndPolicy string
	MaximumPerOwner                                      int
	MaximumPerSourceSkill                                int
	MaximumPerTeam                                       int
	MaximumSpawnCount                                    int
	MaximumLifetimeTicks                                 Tick
	Commands                                             []string
	Behaviors                                            []string
	GameplayTags                                         []GameplayTagHandle
	AllowedAttributeOverrides                            []UnitTemplateAttributeOverridePolicy
	Parameters                                           []UnitTemplateParameterPolicy
	DynamicCollider                                      bool
}

type UnitTemplateAttributeOverridePolicy struct {
	Attribute        AttributeHandle
	Minimum, Maximum int64
}

type UnitTemplateParameterPolicy struct {
	Name             string
	ValueType        valueKind
	Quantity         quantityKind
	Minimum, Maximum int64
}

type CollisionLayerCatalog struct {
	Revision string
	Entries  []CollisionLayerCatalogEntry
}
type CollisionLayerCatalogEntry struct {
	Handle CollisionLayerHandle
	Key    string
}

type DamageTypeCatalog struct {
	Revision string
	Entries  []DamageTypeCatalogEntry
}
type DamageTypeCatalogEntry struct {
	Handle DamageTypeHandle
	Key    string
}

type ElementCatalog struct {
	Revision string
	Entries  []ElementCatalogEntry
}
type ElementCatalogEntry struct {
	Handle             ElementHandle
	Key, MatchupPolicy string
}

func (c ElementCatalog) WithTestRevision(revision string) ElementCatalog {
	c.Revision = revision
	c.Entries = append([]ElementCatalogEntry(nil), c.Entries...)
	return c
}

type GameplayTagClass uint8

const (
	GameplayTagDeclarable GameplayTagClass = 1 << iota
	GameplayTagCompilerDerived
	GameplayTagRuntimeOnly
	GameplayTagTargetQueryable
)

type GameplayTagCatalog struct {
	Revision string
	Entries  []GameplayTagCatalogEntry
}
type GameplayTagCatalogEntry struct {
	Handle  GameplayTagHandle
	Key     string
	Classes GameplayTagClass
}

type CombatPolicyCatalog struct {
	Revision      string
	DamageTypes   []DamageTypeHandle
	FormulaPolicy string
}

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

type AbilityControlCatalog struct {
	Revision       string
	SelectableTags []GameplayTagHandle
	OwnerRelations []string
	Properties     []AbilityPropertyPolicy
}
type AbilityPropertyPolicy struct {
	Property                          string
	ValueType                         valueKind
	Mutable                           bool
	Minimum, Maximum, MaximumMutation int64
	MaximumDurationTicks              Tick
}

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

type MotionCapabilityCatalog struct {
	Revision               string
	ProcessTrajectoryPairs []MotionProcessTrajectoryPair
	VariantCapabilities    []MotionVariantCapability
	EnabledSlots           []string
	HostFeatures           []string
	MaximumSpeed           int64
	MaximumDistance        int64
	MaximumAngularSpeed    int64
	MaximumTrackingTicks   Tick
}

type MotionProcessTrajectoryPair struct{ Process, Trajectory string }
type MotionVariantCapability struct {
	Process, Trajectory                           string
	Frames, Steering, Offsets, CollisionResponses []string
	Carry                                         bool
	Completions                                   []string
}
type ProcessPropertyCatalog struct {
	Revision   string
	Properties []ProcessPropertyPolicy
}
type ProcessPropertyHandle uint16
type ProcessPropertySlotBinding struct {
	Stage, Variant, Field string
}
type ProcessPropertyPolicy struct {
	Handle           ProcessPropertyHandle
	Key              string
	Minimum, Maximum int64
	ProcessKinds     []string
	Operations       []string
	Interpolation    string
	Rounding         string
	SlotBindings     []ProcessPropertySlotBinding
}
type VisualCatalog struct {
	Revision, Digest string
	Themes           []string // compact compatibility projection of Categories.
	Categories       map[string]VisualCategoryDescriptor
	Elements         map[string]VisualElementDescriptor
	Limits           VisualLimits
}

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

func (c VisualCatalog) WithTestRevision(revision string) VisualCatalog {
	c.Revision = revision
	c.Digest = digestStrings("visual", revision, c.Themes)
	c.Themes = append([]string(nil), c.Themes...)
	c.Categories = cloneVisualCategories(c.Categories)
	c.Elements = cloneVisualElements(c.Elements)
	return c
}

type NumericAuthority struct {
	WorldDistanceScale, MillidegreesPerDegree, BasisPointsScale, SignedIntegerBits int
	TickUnit, DefaultRounding                                                      string
}

type CompileLimits struct {
	MaxPhases, MaxFlowNodes, MaxFlowDepth, MaxValueNodes, MaxRepeat, MaxTargets, MaxProcesses, MaxSchedules, MaxMutations                                                                                                                                                                                                                                                                                                                                                              int
	MaxLifetimeTicks                                                                                                                                                                                                                                                                                                                                                                                                                                                                   Tick
	MaxMotionOffsets, MaxReflects, MaxPierces, MaxCarryTargets, MaxVisualRefs, MaxGameplayTags, MaxProcDepth, MaxEventsPerRoot, MaxRandomSites, MaxPassiveActivationsPerTick, MaxAreaMembers, MaxStatusStacks, MaxPersistentStates, MaxStateInstancesPerOwner, MaxAbilitySelections, MaxAbilityMutations, MaxOwnedEntities, MaxOwnedProcesses, MaxStatusSelections, MaxStatusMutations, MaxEffectResultSlots, MaxLocalFrames, MaxInputPathPoints, MaxTemporalSnapshots, MaxStringBytes int
	MaxInputPathLength                                                                                                                                                                                                                                                                                                                                                                                                                                                                 int64
	MaxTemporalSnapshotAge                                                                                                                                                                                                                                                                                                                                                                                                                                                             Tick
}

type CompileEnvironment struct {
	CompilerSemanticsRevision string
	Revision                  string
	Digest                    string
	Limits                    CompileLimits
	Numeric                   NumericAuthority
	Gameplay                  GameplayCatalog
	Motion                    MotionCapabilityCatalog
	ProcessProperties         ProcessPropertyCatalog
	Visual                    VisualCatalog
}

func DefaultCompileEnvironment() CompileEnvironment {
	environment := CompileEnvironment{
		CompilerSemanticsRevision: "skillv2-compiler-2", Revision: "gameplay-default-1",
		Limits: defaultCompileLimits(), Numeric: NumericAuthority{WorldDistanceScale: 1000, MillidegreesPerDegree: 1000, BasisPointsScale: 10000, SignedIntegerBits: 64, TickUnit: "logical_tick", DefaultRounding: "half_away_from_zero"},
		Gameplay: defaultGameplayCatalog(), Motion: defaultMotionCapabilityCatalog(), ProcessProperties: defaultProcessPropertyCatalog(), Visual: defaultVisualCatalog(),
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
		"attachment": {"shield", "status", "attribute_modifier"}, "movement": {"teleport", "knockback", "pull", "stop_movement"}, "projectile": {"projectile"}, "beam": {"beam"}, "area": {"area"}, "summon": {"spawn"},
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

func defaultProcessPropertyCatalog() ProcessPropertyCatalog {
	operations := func() []string { return []string{"set", "add", "mul_bp"} }
	policy := func(handle ProcessPropertyHandle, key string, minimum, maximum int64, processKinds []string, bindings ...ProcessPropertySlotBinding) ProcessPropertyPolicy {
		return ProcessPropertyPolicy{Handle: handle, Key: key, Minimum: minimum, Maximum: maximum, ProcessKinds: processKinds, Operations: operations(), Interpolation: "linear_integer", Rounding: "truncate_toward_zero", SlotBindings: bindings}
	}
	return ProcessPropertyCatalog{Revision: "process-2", Properties: []ProcessPropertyPolicy{
		policy(1, "speed", 0, 100000, []string{"dash", "projectile", "area"}, ProcessPropertySlotBinding{"trajectory", "linear", "speed"}, ProcessPropertySlotBinding{"trajectory", "path", "speed"}, ProcessPropertySlotBinding{"trajectory", "parabola", "speed"}),
		policy(2, "radius", 0, 1000000, []string{"orbit"}, ProcessPropertySlotBinding{"trajectory", "orbit", "radius"}),
		policy(3, "arc_height", 0, 1000000, []string{"projectile"}, ProcessPropertySlotBinding{"trajectory", "parabola", "height"}),
		policy(4, "turn_rate_mdeg_per_tick", 0, 360000, []string{"projectile"}, ProcessPropertySlotBinding{"steering", "tracking", "turn_rate_mdeg_per_tick"}),
		policy(5, "angular_speed_mdeg_per_tick", -360000, 360000, []string{"orbit", "projectile"}, ProcessPropertySlotBinding{"trajectory", "orbit", "angular_speed"}, ProcessPropertySlotBinding{"offset", "circular", "angular_speed"}),
		policy(6, "offset_amplitude", 0, 1000000, []string{"projectile"}, ProcessPropertySlotBinding{"offset", "zigzag", "amplitude"}),
		policy(7, "offset_radius", 0, 1000000, []string{"orbit", "projectile"}, ProcessPropertySlotBinding{"offset", "circular", "radius"}),
		policy(8, "return_speed_bp", 0, 100000, []string{"projectile"}, ProcessPropertySlotBinding{"completion", "boomerang", "return_speed_bp"}),
		policy(9, "collision_force", 0, 1000000, []string{"dash", "projectile", "area"}, ProcessPropertySlotBinding{"collision", "present", "force"}),
	}}
}

func defaultMotionCapabilityCatalog() MotionCapabilityCatalog {
	projectile := func(trajectory string) MotionVariantCapability {
		return MotionVariantCapability{Process: "projectile", Trajectory: trajectory, Frames: []string{"world", "follow"}, Steering: []string{"fixed", "tracking"}, Offsets: []string{"zigzag", "circular"}, CollisionResponses: []string{"stop", "reflect", "pierce"}, Carry: true, Completions: []string{"end", "pause_then_end", "boomerang"}}
	}
	return MotionCapabilityCatalog{
		Revision: "motion-2",
		ProcessTrajectoryPairs: []MotionProcessTrajectoryPair{
			{"dash", "linear"}, {"orbit", "orbit"}, {"projectile", "linear"}, {"projectile", "path"}, {"projectile", "parabola"}, {"area", "stationary"}, {"area", "linear"}, {"beam", "stationary"},
		},
		VariantCapabilities: []MotionVariantCapability{
			{Process: "dash", Trajectory: "linear", Frames: []string{"world", "follow"}, Steering: []string{"fixed"}, CollisionResponses: []string{"stop"}, Carry: true, Completions: []string{"end", "pause_then_end"}},
			{Process: "orbit", Trajectory: "orbit", Frames: []string{"world"}, Offsets: []string{"circular"}, Completions: []string{"end", "pause_then_end"}},
			projectile("linear"), projectile("path"), projectile("parabola"),
			{Process: "area", Trajectory: "stationary", Frames: []string{"world", "follow"}, Completions: []string{"end", "pause_then_end"}},
			{Process: "area", Trajectory: "linear", Frames: []string{"world", "follow"}, Steering: []string{"fixed"}, CollisionResponses: []string{"stop"}, Completions: []string{"end", "pause_then_end"}},
			{Process: "beam", Trajectory: "stationary", Frames: []string{"world", "follow"}, Completions: []string{"end"}},
		},
		EnabledSlots: []string{"frame", "steering", "offsets", "collision", "carry", "completion"}, HostFeatures: []string{"carry"}, MaximumSpeed: 100000, MaximumDistance: 100000, MaximumAngularSpeed: 100000, MaximumTrackingTicks: 36000,
	}
}

func defaultCompileLimits() CompileLimits {
	return CompileLimits{MaxPhases: 32, MaxFlowNodes: 1024, MaxFlowDepth: 64, MaxValueNodes: 4096, MaxRepeat: 64, MaxTargets: 128, MaxProcesses: 128, MaxSchedules: 2048, MaxMutations: 4096, MaxLifetimeTicks: 36000, MaxMotionOffsets: 8, MaxReflects: 16, MaxPierces: 32, MaxCarryTargets: 8, MaxVisualRefs: 256, MaxGameplayTags: 256, MaxProcDepth: 8, MaxEventsPerRoot: 1024, MaxRandomSites: 256, MaxPassiveActivationsPerTick: 256, MaxAreaMembers: 128, MaxStatusStacks: 128, MaxPersistentStates: 64, MaxStateInstancesPerOwner: 1024, MaxAbilitySelections: 64, MaxAbilityMutations: 128, MaxOwnedEntities: 128, MaxOwnedProcesses: 128, MaxStatusSelections: 128, MaxStatusMutations: 256, MaxEffectResultSlots: 128, MaxLocalFrames: 4096, MaxInputPathPoints: 64, MaxInputPathLength: 100000, MaxTemporalSnapshots: 16, MaxTemporalSnapshotAge: 3600, MaxStringBytes: 4096}
}

func defaultGameplayCatalog() GameplayCatalog {
	attributes := []AttributeCatalogEntry{
		{Handle: 1, Key: "health", ValueType: valueKindInt, Quantity: quantityCombatAmount, Readable: true, Snapshots: []string{"cast_start", "phase_start", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 1000000, Rounding: "half_away_from_zero"},
		{Handle: 2, Key: "ability_power", ValueType: valueKindInt, Quantity: quantityCombatAmount, Readable: true, Snapshots: []string{"cast_start", "phase_start", "process_start", "each_tick", "on_hit", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 1000000, Rounding: "half_away_from_zero"},
		{Handle: 3, Key: "move_speed", ValueType: valueKindInt, Quantity: quantitySpeedWorldPerTick, Readable: true, Snapshots: []string{"cast_start", "phase_start", "process_start", "each_tick", "current"}, ModifierOperations: []string{"add", "mul_bp"}, Minimum: 0, Maximum: 100000, Rounding: "toward_zero"},
	}
	return GameplayCatalog{
		Attributes: AttributeCatalog{Revision: "attributes-1", Entries: attributes},
		Resources:  ResourceCatalog{Revision: "resources-1", Entries: []ResourceCatalogEntry{{Handle: 1, Key: "mana", Minimum: 0, Maximum: 100000}}},
		Statuses: StatusCatalog{Revision: "statuses-1", Entries: []StatusCatalogEntry{
			{Handle: 1, Key: "slow", Category: "control", RefreshPolicy: "refresh", MaxStacks: 1, DispelCategory: "debuff", TenacityPolicy: "scale_duration", SourceOwnership: "source", RemovalPolicy: "expire", Polarity: "negative", DispelPriority: 10, AttributeModifiers: []StatusAttributeModifier{{Attribute: 3, Operation: "mul_bp", Value: 8000}}, PeriodicPolicy: "none", Dispellable: true, Copyable: true, Transferable: true, Stealable: true, DurationOperations: []string{"add_duration", "set_duration", "mul_duration_bp", "refresh"}, MaximumDurationTicks: 600},
			{Handle: 2, Key: "shield", Category: "shield", RefreshPolicy: "stack", MaxStacks: 1, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "source", RemovalPolicy: "consume", Polarity: "positive", DispelPriority: 20, PeriodicPolicy: "none", Dispellable: true, Transferable: true, DurationOperations: []string{"add_duration", "set_duration", "mul_duration_bp", "refresh"}, MaximumDurationTicks: 600},
		}},
		UnitTemplates: UnitTemplateCatalog{Revision: "units-1", Entries: []UnitTemplateCatalogEntry{{Handle: 1, Key: "deployable.trap", OwnerPolicy: "owner", OwnerDeathPolicy: "despawn", SkillRemovedPolicy: "despawn", MatchEndPolicy: "despawn", MaximumPerOwner: 8, MaximumPerSourceSkill: 8, MaximumPerTeam: 8, MaximumSpawnCount: 8, MaximumLifetimeTicks: 600, ReplacementPolicy: "replace_oldest", ControlProfile: "trap-basic", Commands: []string{"hold_position", "despawn"}, Behaviors: []string{"armed"}, GameplayTags: []GameplayTagHandle{1}}}},
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
