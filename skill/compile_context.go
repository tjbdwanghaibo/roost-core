package skill

type compilePass interface {
	name() string
	run(*compileContext)
}

type compilePassFunc struct {
	passName string
	runFunc  func(*compileContext)
}

func (p compilePassFunc) name() string                { return p.passName }
func (p compilePassFunc) run(context *compileContext) { p.runFunc(context) }

type compileContext struct {
	definition  *Definition
	environment CompileEnvironment
	artifacts   *compileArtifacts
	diagnostics []Diagnostic
}

type compileArtifacts struct {
	passOrder         []string
	ir                *skillIR
	sources           sourceMap
	shape             shapeArtifact
	authority         authorityArtifact
	gameplay          gameplayArtifact
	snapshots         snapshotArtifact
	proc              procArtifact
	input             InputLayout
	types             typedValueArtifact
	graph             PhaseGraph
	memory            memoryArtifact
	state             stateArtifact
	ability           abilityArtifact
	temporal          temporalArtifact
	processProperties []ProcessPropertyPolicy
	lifetimes         map[string]lifecycleFact
	identity          identityArtifact
	limits            ComputedLimits
	visual            visualArtifact
	metadata          compileMetadata
	lowerReady        bool
}

type visualArtifact struct {
	entries      []visualProgram
	bySourcePath map[string]VisualIndex
	castIndex    VisualIndex
	hasCast      bool
}

type compileMetadata struct {
	CompilerSemanticsRevision string
	SourceDocumentDigest      string
	VisualRevision            string
	VisualDigest              string
}

type InputLayout struct {
	Kind                 inputKind
	Slots                map[string]valueType
	MaximumRange         int64
	HasMaximumRange      bool
	MinimumLength        int64
	MaximumLength        int64
	MaximumPathPoints    int
	MaximumPathLength    int64
	MinimumSegmentLength int64
	ClampPolicy          string
	SimplificationPolicy string
	UpdatePorts          map[InputPort]bool
}
type typedValueArtifact struct{ types map[string]valueType }
type PhaseGraph struct {
	Index            map[string]int
	Adjacency        [][]int
	Reverse          [][]int
	TopologicalOrder []int
	Reachable        []bool
}
type memoryArtifact struct{ initializedAtEntry map[string]bool }
type stateArtifact struct {
	slots map[string]StateSlot
	plans map[string]resolvedStatePlan
}
type abilityArtifact struct {
	properties     map[string]resolvedAbilityProperty
	reads          map[string]AbilityReadPlan
	selectableTags []GameplayTagHandle
	ownerRelations []string
}
type temporalArtifact struct {
	profiles map[string]resolvedTemporalProfile
}
type resolvedTemporalProfile struct {
	handle                TemporalProfileHandle
	fields                []string
	allowRevive           bool
	eventPolicy           string
	blockedPositionPolicy string
}
type lifecycleFact struct {
	CanFallthrough bool
	MustTerminate  bool
	MaySuspend     bool
	MaxLifetime    Tick
	MaxSchedules   int
	MaxProcesses   int
}
type operationIdentity struct {
	Path  string
	Index int
}
type RandomSite struct {
	ID              int
	Path            string
	InvocationBound int
}
type identityArtifact struct {
	Operations  []operationIdentity
	RandomSites []RandomSite
}
type ComputedLimits struct {
	FlowNodes, FlowDepth, ValueNodes, Repeat, Targets, Processes, Schedules, Mutations, EventsPerRoot, RandomSites, PassiveActivationsPerTick       int
	AreaMembers, StatusStacks, StateInstances, AbilityMutations, OwnedEntities, OwnedProcesses, StatusMutations, InputPathPoints, TemporalSnapshots int
	LocalFrames                                                                                                                                     int
	LifetimeTicks                                                                                                                                   Tick
	InputPathLength                                                                                                                                 int64
}

type shapeArtifact struct{ checked bool }
type authorityArtifact struct {
	identity      AuthorityIdentity
	attributes    map[string]AttributeHandle
	resources     map[string]ResourceHandle
	statuses      map[string]StatusHandle
	collision     map[string]CollisionLayerHandle
	tags          map[string]GameplayTagHandle
	unitTemplates map[string]UnitTemplateHandle
}
type gameplayArtifact struct {
	skillTags []GameplayTagHandle
	damage    map[string]resolvedDamageSemantics
}
type resolvedDamageSemantics struct {
	DamageType DamageTypeHandle
	Element    ElementHandle
	CombatTags []GameplayTagHandle
}

type snapshotPoint string

const (
	snapshotCastStart    snapshotPoint = "cast_start"
	snapshotPhaseStart   snapshotPoint = "phase_start"
	snapshotProcessStart snapshotPoint = "process_start"
	snapshotEachTick     snapshotPoint = "each_tick"
	snapshotOnHit        snapshotPoint = "on_hit"
	snapshotOnEvent      snapshotPoint = "on_event"
	snapshotCurrent      snapshotPoint = "current"
)

type AttributeReadPlan struct {
	Entity       valueIR
	Attribute    AttributeHandle
	Snapshot     snapshotPoint
	SnapshotSlot int
}
type snapshotArtifact struct{ reads map[string]AttributeReadPlan }

type FilterPlan struct {
	RequiredTags, ExcludedTags []GameplayTagHandle
	Elements                   []ElementHandle
	DamageTypes                []DamageTypeHandle
	Results                    []string
}
type ProcPlan struct {
	Filter                             FilterPlan
	MaxDepth                           int
	AllowSelfTrigger, OncePerRootEvent bool
	MaxEventsPerRoot                   int
}
type procArtifact struct{ plan *ProcPlan }

func (c *compileContext) addDiagnostic(code DiagnosticCode, path, message string) {
	c.diagnostics = append(c.diagnostics, Diagnostic{Code: code, Severity: DiagnosticError, Path: path, Message: message})
}
func (c *compileContext) hasErrors() bool {
	for _, diagnostic := range c.diagnostics {
		if diagnostic.Severity == DiagnosticError {
			return true
		}
	}
	return false
}
