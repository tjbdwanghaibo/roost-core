package skill

type DiagnosticSeverity string
type Severity = DiagnosticSeverity
type DiagnosticCode string

const (
	DiagnosticError   DiagnosticSeverity = "error"
	DiagnosticWarning DiagnosticSeverity = "warning"
)

type Diagnostic struct {
	Code     DiagnosticCode
	Severity DiagnosticSeverity
	Path     string
	Message  string
}

const (
	DiagnosticEnvironmentInvalid       DiagnosticCode = "ENVIRONMENT_INVALID"
	DiagnosticEnvironmentLimits        DiagnosticCode = "ENVIRONMENT_LIMITS_INVALID"
	DiagnosticCatalogDuplicateHandle   DiagnosticCode = "CATALOG_DUPLICATE_HANDLE"
	DiagnosticCatalogNeutralElement    DiagnosticCode = "CATALOG_NEUTRAL_ELEMENT_REQUIRED"
	DiagnosticCatalogTagClass          DiagnosticCode = "CATALOG_TAG_CLASS_REQUIRED"
	DiagnosticCatalogReference         DiagnosticCode = "CATALOG_REFERENCE_INVALID"
	DiagnosticCatalogAttributePolicy   DiagnosticCode = "CATALOG_ATTRIBUTE_POLICY_INVALID"
	DiagnosticCatalogStatePolicy       DiagnosticCode = "CATALOG_STATE_POLICY_INVALID"
	DiagnosticCatalogAbilityPolicy     DiagnosticCode = "CATALOG_ABILITY_POLICY_INVALID"
	DiagnosticCatalogUnitPolicy        DiagnosticCode = "CATALOG_UNIT_POLICY_INVALID"
	DiagnosticCatalogTemporalPolicy    DiagnosticCode = "CATALOG_TEMPORAL_POLICY_INVALID"
	DiagnosticCatalogMotionPolicy      DiagnosticCode = "CATALOG_MOTION_POLICY_INVALID"
	DiagnosticCatalogHostPolicy        DiagnosticCode = "CATALOG_HOST_POLICY_INVALID"
	DiagnosticShapeInvalid             DiagnosticCode = "SHAPE_INVALID"
	DiagnosticCapabilityUnknown        DiagnosticCode = "CAPABILITY_UNKNOWN"
	DiagnosticGameplayTagPermission    DiagnosticCode = "GAMEPLAY_TAG_PERMISSION"
	DiagnosticGameplayElementInvalid   DiagnosticCode = "GAMEPLAY_ELEMENT_INVALID"
	DiagnosticAttributeSnapshotInvalid DiagnosticCode = "ATTRIBUTE_SNAPSHOT_INVALID"
	DiagnosticEventFilterConflict      DiagnosticCode = "EVENT_FILTER_CONFLICT"
	DiagnosticProcLimitExceeded        DiagnosticCode = "PROC_LIMIT_EXCEEDED"
	DiagnosticVisualNotDeployed        DiagnosticCode = "VISUAL_NOT_DEPLOYED"
	DiagnosticVisualInvalid            DiagnosticCode = "VISUAL_INVALID"
	DiagnosticTypeMismatch             DiagnosticCode = "TYPE_MISMATCH"
	DiagnosticReferenceUnknown         DiagnosticCode = "REFERENCE_UNKNOWN"
	DiagnosticInputUnavailable         DiagnosticCode = "INPUT_UNAVAILABLE"
	DiagnosticOptionalInvalid          DiagnosticCode = "OPTIONAL_INVALID"
	DiagnosticQuantityMismatch         DiagnosticCode = "QUANTITY_MISMATCH"
	DiagnosticPhaseDuplicate           DiagnosticCode = "PHASE_DUPLICATE"
	DiagnosticPhaseTargetMissing       DiagnosticCode = "PHASE_TARGET_MISSING"
	DiagnosticPhaseCycle               DiagnosticCode = "PHASE_CYCLE"
	DiagnosticPhaseUnreachable         DiagnosticCode = "PHASE_UNREACHABLE"
	DiagnosticPhaseInitialMissing      DiagnosticCode = "PHASE_INITIAL_MISSING"
	DiagnosticMemoryMaybeUninitialized DiagnosticCode = "MEMORY_MAYBE_UNINITIALIZED"
	DiagnosticLifecycleFallthrough     DiagnosticCode = "LIFECYCLE_FALLTHROUGH"
	DiagnosticLifecycleControlConflict DiagnosticCode = "LIFECYCLE_CONTROL_CONFLICT"
	DiagnosticBudgetExceeded           DiagnosticCode = "BUDGET_EXCEEDED"
	DiagnosticMotionInvalid            DiagnosticCode = "MOTION_INVALID"
	// DiagnosticLowerUnresolved：lower 查某个名字的槽位 / handle 查不到（B3 ①）。出现它说明
	// 前面某个 pass 接受了一个它没有解析的名字，是编译器缺陷；Program 不会被交出。
	DiagnosticLowerUnresolved DiagnosticCode = "LOWER_UNRESOLVED"
	// DiagnosticHostCapabilityMissing：定义用到的取值不在环境的 Host 能力表里（B3 ③），消息点名
	// 缺的那一项（如 `motion_step "collision"`、`summon`）。
	DiagnosticHostCapabilityMissing DiagnosticCode = "HOST_CAPABILITY_MISSING"
)
