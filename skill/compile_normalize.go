package skill

import (
	"fmt"
	"regexp"
	"sort"
)

func sortedStringKeys(values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type normalizer struct {
	sources     sourceMap
	diagnostics []Diagnostic
	nextScopeID int
}

func normalizeDefinition(definition *Definition) (*skillIR, sourceMap, []Diagnostic) {
	n := &normalizer{nextScopeID: 1}
	if definition == nil {
		n.error("NORMALIZE_NIL_DEFINITION", "$", "definition is nil")
		return nil, n.sources, n.diagnostics
	}
	n.checkIdentifier(definition.ID, "$.id")
	result := &skillIR{
		source:              n.source("$"),
		schema:              definition.Schema,
		id:                  definition.ID,
		name:                definition.Name,
		description:         definition.Description,
		gameplayTags:        append([]string(nil), definition.GameplayTags...),
		activation:          n.normalizeActivation(definition.Activation, "$.activation"),
		input:               n.normalizeInput(definition.InputSchema, "$.input_schema"),
		cooldownTicks:       definition.CooldownTicks,
		globalCooldownTicks: definition.GlobalCooldownTicks,
		costs:               n.normalizeCosts(definition.Costs, "$.costs"),
		memory:              n.normalizeMemory(definition.Memory),
		persistentState:     n.normalizePersistentState(definition.PersistentState),
		initialPhase:        definition.InitialPhase,
		phases:              make([]phaseIR, len(definition.Phases)),
	}
	if definition.Presentation != nil {
		result.presentation = &skillPresentationIR{iconKeywords: append([]string(nil), definition.Presentation.IconKeywords...), cast: normalizeVisual(definition.Presentation.Cast)}
	}
	for index, phase := range definition.Phases {
		path := fmt.Sprintf("$.phases[%d]", index)
		n.checkIdentifier(phase.ID, path+".id")
		result.phases[index] = phaseIR{source: n.source(path), id: phase.ID, timeoutTicks: phase.TimeoutTicks, events: n.normalizePhaseEvents(phase.On, path+".on")}
	}
	return result, n.sources, append([]Diagnostic(nil), n.diagnostics...)
}

func (n *normalizer) source(path string) sourceRef { return n.sources.add(path) }

func (n *normalizer) error(code DiagnosticCode, path, message string) {
	n.diagnostics = append(n.diagnostics, Diagnostic{Code: code, Severity: DiagnosticError, Path: path, Message: message})
}

func (n *normalizer) checkIdentifier(value, path string) {
	if !identifierPattern.MatchString(value) {
		n.error("NORMALIZE_INVALID_IDENTIFIER", path, fmt.Sprintf("invalid identifier %q", value))
	}
}

func (n *normalizer) normalizeActivation(value ActivationDefinition, path string) activationIR {
	result := activationIR{source: n.source(path)}
	switch typed := value.(type) {
	case ActiveActivationDefinition:
		result.kind = "active"
		result.policy = n.normalizePolicy(typed.Policy, path+".policy")
		result.castWindow = castWindowIR{movement: "allowed", turning: "allowed", refundBeforeCommit: true}
		result.castWindow.concurrent = typed.Concurrent
		if typed.CastWindow != nil {
			window := typed.CastWindow
			result.castWindow = castWindowIR{
				windupTicks: window.WindupTicks, commitTick: window.CommitTick, recoveryTicks: window.RecoveryTicks,
				windupTicksMin: window.WindupTicksMin, windupTicksMax: window.WindupTicksMax,
				recoveryTicksMin: window.RecoveryTicksMin, recoveryTicksMax: window.RecoveryTicksMax,
				movement: window.Movement, turning: window.Turning,
				interruptTags: append([]string(nil), window.InterruptTags...), refundBeforeCommit: window.RefundBeforeCommit,
				concurrent: typed.Concurrent,
			}
			if window.WindupTicksExpression.Node != nil {
				result.castWindow.hasWindupExpression = true
				result.castWindow.windupExpression = n.normalizeValue(window.WindupTicksExpression, path+".cast_window.windup_ticks_expression")
			}
			if window.RecoveryTicksExpression.Node != nil {
				result.castWindow.hasRecoveryExpression = true
				result.castWindow.recoveryExpression = n.normalizeValue(window.RecoveryTicksExpression, path+".cast_window.recovery_ticks_expression")
			}
			if result.castWindow.movement == "" {
				result.castWindow.movement = "allowed"
			}
			if result.castWindow.turning == "" {
				result.castWindow.turning = "allowed"
			}
		}
	case PassiveActivationDefinition:
		result.kind = typed.Type
		result.cooldownScope = typed.CooldownScope
		result.policy = castPolicyIR{mode: castModeTap}
		result.eventFilter = eventFilterIR{requiredTags: append([]string(nil), typed.EventFilter.RequiredTags...), excludedTags: append([]string(nil), typed.EventFilter.ExcludedTags...), elements: append([]string(nil), typed.EventFilter.Elements...), damageTypes: append([]string(nil), typed.EventFilter.DamageTypes...), results: append([]string(nil), typed.EventFilter.Results...)}
		result.procPolicy = procPolicyIR{maxDepth: typed.ProcPolicy.MaxDepth, allowSelfTrigger: typed.ProcPolicy.AllowSelfTrigger, oncePerRootEvent: typed.ProcPolicy.OncePerRootEvent}
	default:
		n.error("NORMALIZE_ACTIVATION_VARIANT", path, fmt.Sprintf("unsupported activation %T", value))
	}
	return result
}

func (n *normalizer) normalizePolicy(value CastPolicyDefinition, path string) castPolicyIR {
	switch typed := value.(type) {
	case TapPolicyDefinition:
		return castPolicyIR{mode: castModeTap}
	case TogglePolicyDefinition:
		return castPolicyIR{mode: castModeToggle, pulseIntervalTicks: typed.PulseIntervalTicks, maxDurationTicks: typed.MaxDurationTicks, sustainCosts: n.normalizeCosts(typed.SustainCosts, path+".sustain_costs")}
	case ChargePolicyDefinition:
		return castPolicyIR{mode: castModeCharge, maxChargeTicks: typed.MaxChargeTicks, minChargeBP: typed.MinChargeBP, autoRelease: typed.AutoRelease}
	case AmmoPolicyDefinition:
		return castPolicyIR{mode: castModeAmmo, maxStock: typed.MaxStock, rechargeTicks: typed.RechargeTicks, initialStock: typed.InitialStock}
	case HoldPolicyDefinition:
		return castPolicyIR{mode: castModeHold, pulseIntervalTicks: typed.PulseIntervalTicks, maxDurationTicks: typed.MaxDurationTicks, sustainCosts: n.normalizeCosts(typed.SustainCosts, path+".sustain_costs")}
	default:
		n.error("NORMALIZE_POLICY_VARIANT", path, fmt.Sprintf("unsupported policy %T", value))
		return castPolicyIR{mode: castModeTap}
	}
}

func (n *normalizer) normalizeInput(value InputSchemaDefinition, path string) inputIR {
	result := inputIR{source: n.source(path)}
	switch typed := value.(type) {
	case NoneInputSchemaDefinition:
		result.kind = inputNone
	case DirectionInputSchemaDefinition:
		result.kind, result.maximumRange = inputDirection, copyInt64Pointer(typed.MaximumRange)
	case PositionInputSchemaDefinition:
		result.kind, result.maximumRange, result.clampPolicy = inputPosition, copyInt64Pointer(typed.MaximumRange), typed.ClampPolicy
	case EntityInputSchemaDefinition:
		result.kind, result.maximumRange = inputEntity, copyInt64Pointer(typed.MaximumRange)
	case DirectionPositionInputSchemaDefinition:
		result.kind, result.maximumRange, result.clampPolicy = inputDirectionPosition, copyInt64Pointer(typed.MaximumRange), typed.ClampPolicy
	case EntityPositionInputSchemaDefinition:
		result.kind, result.maximumRange, result.clampPolicy = inputEntityPosition, copyInt64Pointer(typed.MaximumRange), typed.ClampPolicy
	case TwoPointInputSchemaDefinition:
		result.kind, result.maximumRange, result.minimumLength, result.maximumLength, result.clampPolicy = inputTwoPoint, int64Pointer(typed.MaximumRange), typed.MinimumLength, typed.MaximumLength, typed.ClampPolicy
	case DragInputSchemaDefinition:
		result.kind, result.maximumRange, result.minimumLength, result.maximumLength, result.clampPolicy = inputDrag, int64Pointer(typed.MaximumRange), typed.MinimumLength, typed.MaximumLength, typed.ClampPolicy
	case PathInputSchemaDefinition:
		result.kind, result.maximumPoints, result.maximumTotalLength, result.minimumSegmentLength, result.simplificationPolicy, result.clampPolicy = inputPath, typed.MaximumPoints, typed.MaximumTotalLength, typed.MinimumSegmentLength, typed.SimplificationPolicy, typed.ClampPolicy
	default:
		n.error("NORMALIZE_INPUT_VARIANT", path, fmt.Sprintf("unsupported input %T", value))
	}
	return result
}

func copyInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func int64Pointer(value int64) *int64 { return &value }

func (n *normalizer) normalizeCosts(values []Cost, path string) []costIR {
	result := make([]costIR, len(values))
	for index, cost := range values {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		result[index] = costIR{source: n.source(itemPath), resource: cost.Resource, amount: n.normalizeValue(cost.Amount, itemPath+".amount")}
	}
	return result
}

func (n *normalizer) normalizeMemory(values map[string]MemoryDeclaration) map[string]memoryDeclarationIR {
	result := make(map[string]memoryDeclarationIR, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, name := range keys {
		declaration := values[name]
		path := "$.memory." + name
		n.checkIdentifier(name, path)
		result[name] = memoryDeclarationIR{source: n.source(path), name: name, declaredType: declaration.Type, defaultValue: n.normalizeValue(declaration.Default, path+".default")}
	}
	return result
}

func (n *normalizer) normalizePersistentState(values map[string]PersistentStateDefinition) map[string]stateDeclarationIR {
	result := make(map[string]stateDeclarationIR, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, name := range keys {
		declaration := values[name]
		path := "$.persistent_state." + name
		n.checkIdentifier(name, path)
		result[name] = stateDeclarationIR{
			source: n.source(path), name: name, declaredType: declaration.Type, scope: StateScope(declaration.Scope),
			defaultValue: n.normalizeValue(declaration.Default, path+".default"), minimum: copyInt64Pointer(declaration.Minimum), maximum: copyInt64Pointer(declaration.Maximum),
			enumValues: append([]string(nil), declaration.Values...), durationTicks: declaration.Lifetime.DurationTicks,
			maximumDurationTicks: declaration.Lifetime.MaximumDurationTicks, onWrite: declaration.Lifetime.OnWrite,
			clearOn: append([]string(nil), declaration.Lifetime.ClearOn...),
		}
	}
	return result
}

func (n *normalizer) normalizeValue(value Value, path string) valueIR {
	source := n.source(path)
	switch typed := value.Node.(type) {
	case NullValueDefinition:
		return &nullValueIR{source: source}
	case IntValueDefinition:
		return &intValueIR{source: source, value: typed.Value}
	case BoolValueDefinition:
		return &boolValueIR{source: source, value: typed.Value}
	case StringValueDefinition:
		return &stringValueIR{source: source, value: typed.Value}
	case ReferenceValueDefinition:
		return &referenceValueIR{source: source, reference: typed.Reference}
	case ExpressionValueDefinition:
		args := make([]valueIR, len(typed.Args))
		for i, arg := range typed.Args {
			args[i] = n.normalizeValue(arg, fmt.Sprintf("%s.args[%d]", path, i))
		}
		return &expressionValueIR{source: source, op: typed.Op, args: args}
	case AttributeReadValueDefinition:
		return &attributeReadValueIR{source: source, entity: n.normalizeValue(typed.Entity, path+".read_attribute.entity"), attribute: typed.Attribute, snapshot: typed.Snapshot}
	case StateReadValueDefinition:
		return &stateReadValueIR{source: source, state: typed.State, owner: n.normalizeOptionalValue(typed.Owner, path+".read_state.owner"), subject: n.normalizeOptionalValue(typed.Subject, path+".read_state.subject"), teamOf: n.normalizeOptionalValue(typed.TeamOf, path+".read_state.team_of"), snapshot: typed.Snapshot}
	case AbilityStateReadValueDefinition:
		return &abilityStateReadValueIR{source: source, owner: n.normalizeValue(typed.Owner, path+".read_ability_state.owner"), ability: n.normalizeValue(typed.Ability, path+".read_ability_state.ability"), property: typed.Property, snapshot: typed.Snapshot}
	default:
		n.error("NORMALIZE_VALUE_VARIANT", path, fmt.Sprintf("unsupported value %T", value.Node))
		return &nullValueIR{source: source}
	}
}

func (n *normalizer) normalizeOptionalValue(value *Value, path string) valueIR {
	if value == nil {
		return nil
	}
	return n.normalizeValue(*value, path)
}

func normalizeVisual(value *VisualRef) *visualIR {
	if value == nil {
		return nil
	}
	return &visualIR{category: value.Category, theme: value.Theme, elements: append([]string(nil), value.Elements...)}
}

func (n *normalizer) normalizePhaseEvents(value PhaseEventsDefinition, path string) phaseEventsIR {
	return phaseEventsIR{
		enter: n.normalizeOptionalFlow(value.Enter, path+".enter"), recast: n.normalizeOptionalFlow(value.Recast, path+".recast"), cancel: n.normalizeOptionalFlow(value.Cancel, path+".cancel"), directionChanged: n.normalizeOptionalFlow(value.DirectionChanged, path+".direction_changed"), targetChanged: n.normalizeOptionalFlow(value.TargetChanged, path+".target_changed"), timeout: n.normalizeOptionalFlow(value.Timeout, path+".timeout"), release: n.normalizeOptionalFlow(value.Release, path+".release"), pulse: n.normalizeOptionalFlow(value.Pulse, path+".pulse"),
	}
}

func (n *normalizer) normalizeOptionalFlow(value FlowDefinition, path string) flowIR {
	if value == nil {
		return nil
	}
	return n.normalizeFlow(value, path)
}

func (n *normalizer) normalizeFlow(value FlowDefinition, path string) flowIR {
	source := n.source(path)
	switch typed := value.(type) {
	case SequenceFlowDefinition:
		steps := make([]flowIR, len(typed.Steps))
		for i, child := range typed.Steps {
			steps[i] = n.normalizeFlow(child, fmt.Sprintf("%s.steps[%d]", path, i))
		}
		return &sequenceFlowIR{source: source, steps: steps}
	case ParallelFlowDefinition:
		branches := make([]flowIR, len(typed.Branches))
		for i, child := range typed.Branches {
			branches[i] = n.normalizeFlow(child, fmt.Sprintf("%s.branches[%d]", path, i))
		}
		return &parallelFlowIR{source: source, branches: branches}
	case IfFlowDefinition:
		return &ifFlowIR{source: source, condition: n.normalizeValue(typed.Condition, path+".condition"), thenFlow: n.normalizeFlow(typed.Then, path+".then"), elseFlow: n.normalizeOptionalFlow(typed.Else, path+".else")}
	case RepeatFlowDefinition:
		n.checkIdentifier(typed.IndexAs, path+".index_as")
		local := localSymbol{Name: typed.IndexAs, ScopeID: n.allocateScope()}
		return &repeatFlowIR{source: source, times: n.normalizeValue(typed.Times, path+".times"), intervalTicks: typed.IntervalTicks, index: local, body: n.normalizeFlow(typed.Do, path+".do")}
	case WaitFlowDefinition:
		return &waitFlowIR{source: source, ticks: typed.Ticks, then: n.normalizeFlow(typed.Then, path+".then")}
	case SelectFlowDefinition:
		return &selectFlowIR{source: source, selectPlan: n.normalizeSelect(typed.Select, path+".select"), consume: n.normalizeConsume(typed.Consume, path+".consume"), onEmpty: n.normalizeOptionalFlow(typed.OnEmpty, path+".on_empty")}
	case EffectFlowDefinition:
		return &effectFlowIR{source: source, effect: n.normalizeEffect(typed.Effect, path+".effect"), result: n.normalizeEffectResult(typed.Result, path+".result"), callbacks: n.normalizeCallbacks(typed.On, path+".on"), process: n.normalizeProcess(typed.Process, path+".process")}
	case GotoFlowDefinition:
		return &gotoFlowIR{source: source, phase: typed.Phase}
	case FinishFlowDefinition:
		return &finishFlowIR{source: source, reason: typed.Reason}
	default:
		n.error("NORMALIZE_FLOW_VARIANT", path, fmt.Sprintf("unsupported flow %T", value))
		return &finishFlowIR{source: source, reason: "invalid"}
	}
}

func (n *normalizer) allocateScope() int { value := n.nextScopeID; n.nextScopeID++; return value }

func (n *normalizer) normalizeConsume(value SelectConsumeDefinition, path string) selectConsumeIR {
	source := sourcedIR{source: n.source(path)}
	switch typed := value.(type) {
	case SelectOneConsumeDefinition:
		n.checkIdentifier(typed.As, path+".as")
		return &selectOneConsumeIR{sourcedIR: source, local: localSymbol{Name: typed.As, ScopeID: n.allocateScope()}, then: n.normalizeFlow(typed.Then, path+".then")}
	case SelectEachConsumeDefinition:
		n.checkIdentifier(typed.As, path+".as")
		return &selectEachConsumeIR{sourcedIR: source, local: localSymbol{Name: typed.As, ScopeID: n.allocateScope()}, body: n.normalizeFlow(typed.Do, path+".do")}
	default:
		n.error("NORMALIZE_CONSUME_VARIANT", path, fmt.Sprintf("unsupported consume %T", value))
		return &selectOneConsumeIR{sourcedIR: source}
	}
}

func (n *normalizer) normalizeEffectResult(value *EffectResultDefinition, path string) *effectResultIR {
	if value == nil {
		return nil
	}
	result := &effectResultIR{source: n.source(path), success: n.normalizeOptionalFlow(value.Success, path+".success"), failure: n.normalizeOptionalFlow(value.Failure, path+".failure")}
	if value.As != nil {
		n.checkIdentifier(*value.As, path+".as")
		local := localSymbol{Name: *value.As, ScopeID: n.allocateScope()}
		result.local = &local
	}
	return result
}

func (n *normalizer) normalizeCallbacks(value *ProcessCallbacksDefinition, path string) *processCallbacksIR {
	if value == nil {
		return nil
	}
	return &processCallbacksIR{tick: n.normalizeOptionalFlow(value.Tick, path+".tick"), hit: n.normalizeOptionalFlow(value.Hit, path+".hit"), collision: n.normalizeOptionalFlow(value.Collision, path+".collision"), end: n.normalizeOptionalFlow(value.End, path+".end"), cancel: n.normalizeOptionalFlow(value.Cancel, path+".cancel"), transition: n.normalizeOptionalFlow(value.Transition, path+".transition"), targetLost: n.normalizeOptionalFlow(value.TargetLost, path+".target_lost"), enter: n.normalizeOptionalFlow(value.Enter, path+".enter"), leave: n.normalizeOptionalFlow(value.Leave, path+".leave")}
}

func (n *normalizer) normalizeSelect(value SelectDefinition, path string) selectIR {
	filters := make([]filterIR, len(value.Filters))
	for i, filter := range value.Filters {
		filters[i] = n.normalizeFilter(filter, fmt.Sprintf("%s.filters[%d]", path, i))
	}
	var order *selectOrderIR
	if value.Order != nil {
		order = &selectOrderIR{by: value.Order.By, direction: value.Order.Direction}
	}
	return selectIR{source: n.source(path), from: n.normalizeValue(value.From, path+".from"), elementType: normalizeElementType(value.Kind), shape: n.normalizeShape(value.Shape, path+".shape"), filters: filters, order: order, limit: value.Limit}
}

func normalizeElementType(value string) selectionElementType {
	switch value {
	case "entity":
		return selectionEntity
	case "position":
		return selectionPosition
	case "hit":
		return selectionHit
	case "path":
		return selectionPath
	case "ability":
		return selectionAbility
	case "status_instance":
		return selectionStatusInstance
	default:
		return 0
	}
}

func (n *normalizer) normalizeShape(value ShapeDefinition, path string) shapeIR {
	source := sourcedIR{source: n.source(path)}
	switch typed := value.(type) {
	case SingleShapeDefinition:
		return &singleShapeIR{sourcedIR: source}
	case CircleShapeDefinition:
		return &circleShapeIR{sourcedIR: source, radius: n.normalizeValue(typed.Radius, path+".radius")}
	case RingShapeDefinition:
		return &ringShapeIR{sourcedIR: source, innerRadius: n.normalizeValue(typed.InnerRadius, path+".inner_radius"), outerRadius: n.normalizeValue(typed.OuterRadius, path+".outer_radius")}
	case ConeShapeDefinition:
		return &coneShapeIR{sourcedIR: source, rangeValue: n.normalizeValue(typed.Range, path+".range"), angleDeg: n.normalizeValue(typed.AngleDeg, path+".angle_deg"), direction: n.normalizeValue(typed.Direction, path+".direction")}
	case LineShapeDefinition:
		return &lineShapeIR{sourcedIR: source, length: n.normalizeValue(typed.Length, path+".length"), width: n.normalizeValue(typed.Width, path+".width"), direction: n.normalizeValue(typed.Direction, path+".direction")}
	case RectangleShapeDefinition:
		return &rectangleShapeIR{sourcedIR: source, length: n.normalizeValue(typed.Length, path+".length"), width: n.normalizeValue(typed.Width, path+".width"), direction: n.normalizeValue(typed.Direction, path+".direction")}
	case RaycastShapeDefinition:
		return &raycastShapeIR{sourcedIR: source, length: n.normalizeValue(typed.Length, path+".length"), direction: n.normalizeValue(typed.Direction, path+".direction"), collision: append([]string(nil), typed.Collision...)}
	case ChainShapeDefinition:
		return &chainShapeIR{sourcedIR: source, hopRange: n.normalizeValue(typed.HopRange, path+".hop_range"), maxTargets: typed.MaxTargets, allowRepeat: typed.AllowRepeat, hopIntervalTicks: typed.HopIntervalTicks}
	case PathShapeDefinition:
		return &pathShapeIR{sourcedIR: source, points: n.normalizeValue(typed.Points, path+".points")}
	case NearestValidShapeDefinition:
		return &nearestValidShapeIR{sourcedIR: source, searchRadius: n.normalizeValue(typed.SearchRadius, path+".search_radius"), collision: append([]string(nil), typed.Collision...)}
	case AbilitySetShapeDefinition:
		return &abilitySetShapeIR{sourcedIR: source}
	case StatusSetShapeDefinition:
		return &statusSetShapeIR{sourcedIR: source}
	case OwnedEntitiesShapeDefinition:
		return &ownedEntitiesShapeIR{sourcedIR: source}
	default:
		n.error("NORMALIZE_SHAPE_VARIANT", path, fmt.Sprintf("unsupported shape %T", value))
		return &singleShapeIR{sourcedIR: source}
	}
}

func (n *normalizer) normalizeFilter(value FilterDefinition, path string) filterIR {
	source := sourcedIR{source: n.source(path)}
	switch typed := value.(type) {
	case FlagFilterDefinition:
		return &flagFilterIR{sourcedIR: source, kind: typed.Type}
	case RelationFilterDefinition:
		return &relationFilterIR{sourcedIR: source, relation: typed.Value}
	case StatusFilterDefinition:
		return &statusFilterIR{sourcedIR: source, kind: typed.Type, status: typed.Status}
	case AttributeCompareFilterDefinition:
		return &attributeCompareFilterIR{sourcedIR: source, attribute: typed.Attribute, op: typed.Op, value: n.normalizeValue(typed.Value, path+".value")}
	case GameplayTagFilterDefinition:
		return &gameplayTagFilterIR{sourcedIR: source, kind: typed.Type, tag: typed.Tag}
	case LineOfSightFilterDefinition:
		return &lineOfSightFilterIR{sourcedIR: source, collision: append([]string(nil), typed.Collision...)}
	case AbilityTagFilterDefinition:
		return &abilityTagFilterIR{sourcedIR: source, tag: typed.Tag}
	case AbilitySlotFilterDefinition:
		return &abilitySlotFilterIR{sourcedIR: source, slot: typed.Slot}
	case OwnedSourceSkillFilterDefinition:
		return &ownedSourceSkillFilterIR{sourcedIR: source, skill: typed.Skill}
	case OwnedSourceCastFilterDefinition:
		return &ownedSourceCastFilterIR{sourcedIR: source, cast: typed.Cast}
	case OwnedSpawnTickFilterDefinition:
		return &ownedSpawnTickFilterIR{sourcedIR: source, kind: typed.Type, tick: typed.Tick}
	case OwnedUnitTemplateFilterDefinition:
		return &ownedUnitTemplateFilterIR{sourcedIR: source, template: typed.Template}
	case OwnedEntityTagFilterDefinition:
		return &ownedEntityTagFilterIR{sourcedIR: source, tag: typed.Tag}
	case StatusInstanceFilterDefinition:
		return &statusInstanceFilterIR{sourcedIR: source, kind: typed.Type, status: typed.Status, text: typed.Text, operation: typed.Op, value: n.normalizeOptionalValue(typed.Value, path+".value")}
	default:
		n.error("NORMALIZE_FILTER_VARIANT", path, fmt.Sprintf("unsupported filter %T", value))
		return &flagFilterIR{sourcedIR: source}
	}
}

func (n *normalizer) normalizeEffect(value EffectDefinition, path string) effectIR {
	source := n.source(path)
	switch typed := value.(type) {
	case CaptureSnapshotEffectDefinition:
		return &captureSnapshotEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), profile: typed.Profile}
	case RestoreSnapshotEffectDefinition:
		return &restoreSnapshotEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), snapshot: n.normalizeValue(typed.Snapshot, path+".snapshot"), onBlocked: typed.OnBlocked}
	case DamageEffectDefinition:
		return &damageEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), amount: n.normalizeValue(typed.Amount, path+".amount"), damageType: typed.DamageType, element: typed.Element, combatTags: append([]string(nil), typed.CombatTags...), canCritical: typed.CanCritical, visual: normalizeVisual(typed.Visual)}
	case HealEffectDefinition:
		return &healEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), amount: n.normalizeValue(typed.Amount, path+".amount"), visual: normalizeVisual(typed.Visual)}
	case ShieldEffectDefinition:
		return &shieldEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), amount: n.normalizeValue(typed.Amount, path+".amount"), durationTicks: typed.DurationTicks, visual: normalizeVisual(typed.Visual)}
	case AddStatusEffectDefinition:
		return &addStatusEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), status: typed.Status, durationTicks: typed.DurationTicks, stacks: typed.Stacks, maxStacks: copyIntPointer(typed.MaxStacks), visual: normalizeVisual(typed.Visual)}
	case RemoveStatusEffectDefinition:
		return &removeStatusEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), status: typed.Status, visual: normalizeVisual(typed.Visual)}
	case ModifyStatusInstanceEffectDefinition:
		return &modifyStatusInstanceEffectIR{source: source, status: n.normalizeValue(typed.Status, path+".status"), operation: typed.Operation, value: n.normalizeOptionalValue(typed.Value, path+".value"), target: n.normalizeOptionalValue(typed.Target, path+".target"), ownershipPolicy: typed.OwnershipPolicy}
	case AttributeModifierEffectDefinition:
		return &attributeModifierEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), attribute: typed.Attribute, operation: typed.Operation, value: n.normalizeValue(typed.Value, path+".value"), durationTicks: typed.DurationTicks, stackPolicy: typed.StackPolicy, maxStacks: typed.MaxStacks, visual: normalizeVisual(typed.Visual)}
	case ResourceEffectDefinition:
		return &resourceEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), amount: n.normalizeValue(typed.Amount, path+".amount"), resource: typed.Resource, operation: typed.Operation, visual: normalizeVisual(typed.Visual)}
	case SetMemoryEffectDefinition:
		return &setMemoryEffectIR{source: source, name: typed.Name, value: n.normalizeValue(typed.Value, path+".value")}
	case AddMemoryEffectDefinition:
		return &addMemoryEffectIR{source: source, name: typed.Name, value: n.normalizeValue(typed.Value, path+".value")}
	case ClearMemoryEffectDefinition:
		return &clearMemoryEffectIR{source: source, name: typed.Name}
	case TeleportEffectDefinition:
		return &teleportEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), destination: n.normalizeValue(typed.Destination, path+".destination"), onBlocked: typed.OnBlocked, visual: normalizeVisual(typed.Visual)}
	case KnockbackEffectDefinition:
		return &knockbackEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), from: n.normalizeValue(typed.From, path+".from"), distance: n.normalizeValue(typed.Distance, path+".distance"), visual: normalizeVisual(typed.Visual)}
	case PullEffectDefinition:
		return &pullEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), toward: n.normalizeValue(typed.Toward, path+".toward"), distance: n.normalizeValue(typed.Distance, path+".distance"), visual: normalizeVisual(typed.Visual)}
	case StopMovementEffectDefinition:
		return &stopMovementEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), visual: normalizeVisual(typed.Visual)}
	case ModifyStateEffectDefinition:
		return &modifyStateEffectIR{source: source, state: typed.State, owner: n.normalizeOptionalValue(typed.Owner, path+".owner"), subject: n.normalizeOptionalValue(typed.Subject, path+".subject"), teamOf: n.normalizeOptionalValue(typed.TeamOf, path+".team_of"), operation: typed.Operation, value: n.normalizeOptionalValue(typed.Value, path+".value"), durationTicks: typed.DurationTicks, expiryPolicy: typed.ExpiryPolicy}
	case ModifyAbilityStateEffectDefinition:
		return &modifyAbilityStateEffectIR{source: source, owner: n.normalizeValue(typed.Owner, path+".owner"), ability: n.normalizeValue(typed.Ability, path+".ability"), property: typed.Property, operation: typed.Operation, value: n.normalizeValue(typed.Value, path+".value"), durationTicks: typed.DurationTicks}
	case ModifyProcessEffectDefinition:
		return &modifyProcessEffectIR{source: source, process: n.normalizeValue(typed.Process, path+".process"), property: typed.Property, operation: typed.Operation, value: n.normalizeValue(typed.Value, path+".value"), overTicks: typed.OverTicks}
	case SpawnEffectDefinition:
		overrideKeys := sortedStringKeys(typed.AttributeOverrides)
		overrides := make([]spawnAttributeOverrideIR, 0, len(overrideKeys))
		for _, key := range overrideKeys {
			overrides = append(overrides, spawnAttributeOverrideIR{attribute: key, value: n.normalizeValue(typed.AttributeOverrides[key], path+".attribute_overrides."+key)})
		}
		parameterKeys := sortedStringKeys(typed.ParameterBindings)
		parameters := make([]spawnParameterBindingIR, 0, len(parameterKeys))
		for _, key := range parameterKeys {
			parameters = append(parameters, spawnParameterBindingIR{name: key, value: n.normalizeValue(typed.ParameterBindings[key], path+".parameter_bindings."+key)})
		}
		return &spawnEffectIR{source: source, template: typed.Template, position: n.normalizeValue(typed.Position, path+".position"), count: typed.Count, durationTicks: typed.DurationTicks, attributeOverrides: overrides, parameterBindings: parameters}
	case DespawnEffectDefinition:
		return &entityCommandEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), command: "despawn"}
	case IssueEntityCommandEffectDefinition:
		return &entityCommandEffectIR{source: source, target: n.normalizeValue(typed.Target, path+".target"), command: typed.Command, position: n.normalizeOptionalValue(typed.Position, path+".position"), targetEntity: n.normalizeOptionalValue(typed.TargetEntity, path+".target_entity"), behavior: typed.Behavior}
	default:
		n.error("NORMALIZE_EFFECT_VARIANT", path, fmt.Sprintf("unsupported effect %T", value))
		return &clearMemoryEffectIR{source: source}
	}
}

func copyIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
