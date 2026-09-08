package skill

func Inspect(program *Program) ProgramView {
	if program == nil {
		return ProgramView{}
	}
	return ProgramView{
		ID: program.id, Name: program.name, Description: program.description,
		CompilerSemanticsRevision: program.compilerSemanticsRevision,
		Authority:                 program.authority, Identity: InspectIdentity(program),
		CooldownTicks: program.cooldownTicks, GlobalCooldownTicks: program.globalCooldownTicks, Cast: inspectCast(program), Costs: inspectCosts(program), GameplayTags: append([]GameplayTagHandle(nil), program.gameplayTags...), Input: inspectInput(program),
		Memory: inspectMemory(program), PersistentState: inspectPersistentState(program), Locals: inspectLocals(program), Phases: inspectPhases(program), Roots: inspectRoots(program),
		Operations: inspectOperations(program), Selectors: inspectSelectors(program),
		RandomSites: inspectRandomSites(program), Limits: InspectMetrics(program),
	}
}

func inspectCast(program *Program) CastWindowView {
	if program == nil {
		return CastWindowView{}
	}
	cast := program.cast
	return CastWindowView{
		WindupTicks: cast.windupTicks, CommitTick: cast.commitTick, RecoveryTicks: cast.recoveryTicks,
		HasWindupExpression: cast.hasWindupExpression, WindupTicksMin: cast.windupTicksMin, WindupTicksMax: cast.windupTicksMax,
		HasRecoveryExpression: cast.hasRecoveryExpression, RecoveryTicksMin: cast.recoveryTicksMin, RecoveryTicksMax: cast.recoveryTicksMax,
		Concurrent: cast.concurrent,
		Movement:   cast.movement, Turning: cast.turning, InterruptTags: append([]GameplayTagHandle(nil), cast.interruptTags...),
		RefundBeforeCommit: cast.refundBeforeCommit,
		Mode:               string(cast.mode), PulseIntervalTicks: cast.pulseIntervalTicks, MaxDurationTicks: cast.maxDurationTicks,
		MaxChargeTicks: cast.maxChargeTicks, MinChargeBP: cast.minChargeBP, AutoRelease: cast.autoRelease,
		MaxStock: cast.maxStock, RechargeTicks: cast.rechargeTicks, InitialStock: cast.initialStock, SustainCostCount: len(cast.sustainCosts),
	}
}

func inspectCosts(program *Program) []CostView {
	if program == nil {
		return nil
	}
	result := make([]CostView, len(program.costs))
	for index, cost := range program.costs {
		typ := programValueType(cost.amount)
		result[index] = CostView{Resource: cost.resource, Kind: valueKindName(typ.Base), Quantity: quantityKindName(typ.Quantity)}
	}
	return result
}

func programValueType(value programValue) valueType {
	switch typed := value.(type) {
	case nullProgramValue:
		return typed.typ
	case intProgramValue:
		return typed.typ
	case boolProgramValue:
		return valueType{Base: valueKindBool}
	case stringProgramValue:
		return valueType{Base: valueKindString}
	case referenceProgramValue:
		return typed.typ
	case expressionProgramValue:
		return typed.typ
	case attributeReadProgramValue:
		return typed.typ
	case stateReadProgramValue:
		return typed.typ
	case abilityStateReadProgramValue:
		return typed.typ
	default:
		return valueType{Base: valueKindInvalid}
	}
}

func InspectMetrics(program *Program) MetricSnapshot {
	if program == nil {
		return MetricSnapshot{}
	}
	return MetricSnapshot{ComputedLimits: program.limits}
}

func InspectInputLayout(program *Program) InputLayoutView { return inspectInput(program) }
func InspectSelections(program *Program) []SelectionView  { return inspectSelectors(program) }

func InspectEffectResults(program *Program) []EffectResultView {
	if program == nil {
		return nil
	}
	var result []EffectResultView
	for _, operation := range program.operations {
		continuations, effectIndex, ok := operationEffectContinuations(operation)
		if !ok || (!continuations.hasSuccess && !continuations.hasFailure) {
			continue
		}
		fields := make([]string, len(continuations.result.fields))
		for index, field := range continuations.result.fields {
			fields[index] = field.name
		}
		result = append(result, EffectResultView{EffectIndex: effectIndex, ResultType: string(continuations.result.typ), FieldHandles: fields, SuccessRoot: continuations.success, FailureRoot: continuations.failure, HasSuccess: continuations.hasSuccess, HasFailure: continuations.hasFailure})
	}
	return result
}

func InspectVisualManifest(program *Program) SkillVisualManifest {
	if program == nil {
		return SkillVisualManifest{}
	}
	entries := inspectVisuals(program)
	parts := []string{program.visualCatalogRevision, program.visualCatalogDigest}
	for _, entry := range entries {
		parts = append(parts, entry.Category, entry.Theme)
		parts = append(parts, entry.Elements...)
	}
	return SkillVisualManifest{Digest: digestStrings("roost.skill/v2/visual-manifest", program.visualCatalogRevision+"/"+program.visualCatalogDigest, parts), CatalogRevision: program.visualCatalogRevision, CatalogDigest: program.visualCatalogDigest, Entries: entries}
}

func InspectCombatSemantics(program *Program) CombatSemanticView {
	view := CombatSemanticView{}
	if program == nil {
		return view
	}
	for _, operation := range program.operations {
		if damage, ok := operation.(damageOperation); ok {
			view.DamageEffects++
			view.Damage = append(view.Damage, DamageSemanticView{EffectIndex: damage.effectIndex, DamageType: damage.damageType, Element: damage.element, CombatTags: append([]GameplayTagHandle(nil), damage.combatTags...), CanCritical: damage.canCritical})
		}
	}
	return view
}

func InspectEventPlans(program *Program) []EventPlanView {
	if program == nil {
		return nil
	}
	result := make([]EventPlanView, len(program.eventPlans))
	for index, plan := range program.eventPlans {
		result[index] = EventPlanView{RequiredTags: append([]GameplayTagHandle(nil), plan.filter.RequiredTags...), ExcludedTags: append([]GameplayTagHandle(nil), plan.filter.ExcludedTags...), Elements: append([]ElementHandle(nil), plan.filter.Elements...), DamageTypes: append([]DamageTypeHandle(nil), plan.filter.DamageTypes...), Results: append([]string(nil), plan.filter.Results...), MaxDepth: plan.proc.MaxDepth, MaxEventsPerRoot: plan.proc.MaxEventsPerRoot, AllowSelfTrigger: plan.proc.AllowSelfTrigger, OncePerRootEvent: plan.proc.OncePerRootEvent}
	}
	return result
}

func InspectStateLayouts(program *Program) StateLayoutView {
	return StateLayoutView{Memory: inspectMemory(program), PersistentState: inspectPersistentState(program), Locals: inspectLocals(program)}
}

func inspectPersistentState(program *Program) []PersistentStateView {
	if program == nil {
		return nil
	}
	result := make([]PersistentStateView, len(program.states))
	for index, state := range program.states {
		result[index] = PersistentStateView{
			Slot: state.slot, Name: state.name, Type: valueKindName(state.typ.Base), Scope: state.scope,
			Minimum: state.minimum, Maximum: state.maximum, EnumValues: append([]string(nil), state.enumValues...),
			DurationTicks: state.durationTicks, MaximumDurationTicks: state.maximumDurationTicks,
			OnWrite: state.onWrite, ClearOn: append([]string(nil), state.clearOn...),
		}
	}
	return result
}

func inspectLocals(program *Program) []LocalSlotView {
	if program == nil {
		return nil
	}
	result := make([]LocalSlotView, len(program.locals))
	for index, slot := range program.locals {
		result[index] = LocalSlotView{Index: slot.index, Kind: valueKindName(slot.typ.Base), Optional: slot.typ.Optional, Quantity: quantityKindName(slot.typ.Quantity)}
	}
	return result
}

func InspectAbilityControls(program *Program) AbilityControlView {
	view := AbilityControlView{}
	if program == nil {
		return view
	}
	for _, operation := range program.operations {
		if _, ok := operation.(abilityStateOperation); ok {
			view.Operations++
		}
	}
	return view
}
func InspectOwnedEntities(program *Program) OwnedEntityView {
	view := OwnedEntityView{}
	if program == nil {
		return view
	}
	for _, operation := range program.operations {
		switch operation.(type) {
		case spawnOperation, entityCommandOperation:
			view.Operations++
		}
	}
	return view
}

func InspectStatusOperations(program *Program) StatusOperationView {
	view := StatusOperationView{}
	if program == nil {
		return view
	}
	for _, operation := range program.operations {
		switch operation.(type) {
		case statusOperation, modifyStatusInstanceOperation:
			view.Operations++
		}
	}
	return view
}

func InspectTemporalProfiles(*Program) TemporalProfileView { return TemporalProfileView{} }

func InspectIdentity(program *Program) ProgramIdentityView {
	if program == nil {
		return ProgramIdentityView{}
	}
	return ProgramIdentityView{SourceDocumentDigest: program.identity.sourceDocumentDigest, GameplayDigest: program.identity.gameplayDigest, PresentationDigest: program.identity.presentationDigest}
}

func InspectQuantities(program *Program) []QuantityView    { return inspectQuantities(program) }
func InspectRandomSites(program *Program) []RandomSiteView { return inspectRandomSites(program) }

func inspectInput(program *Program) InputProgramView {
	if program == nil {
		return InputProgramView{}
	}
	input := program.input
	view := InputProgramView{
		Kind: string(input.kind), MaximumRange: input.maximumRange, HasMaximumRange: input.hasMaximumRange,
		MinimumLength: input.minimumLength, MaximumLength: input.maximumLength,
		MaximumPathPoints: input.maximumPathPoints, MaximumPathLength: input.maximumPathLength, MinimumSegmentLength: input.minimumSegmentLength,
		ClampPolicy: input.clampPolicy, SimplificationPolicy: input.simplificationPolicy,
		UpdatePorts: append([]InputPort(nil), input.updatePorts...), Slots: make([]InputSlotView, len(input.slots)),
	}
	for index, slot := range program.input.slots {
		view.Slots[index] = InputSlotView{Name: slot.name, Kind: valueKindName(slot.typ.Base), Optional: slot.typ.Optional, Quantity: quantityKindName(slot.typ.Quantity)}
	}
	return view
}

func inspectMemory(program *Program) []MemorySlotView {
	if program == nil {
		return nil
	}
	result := make([]MemorySlotView, len(program.memory))
	for index, slot := range program.memory {
		result[index] = MemorySlotView{Index: slot.index, Name: slot.name, Kind: valueKindName(slot.typ.Base), Optional: slot.typ.Optional, Quantity: quantityKindName(slot.typ.Quantity)}
	}
	return result
}

func inspectPhases(program *Program) []PhaseView {
	if program == nil {
		return nil
	}
	result := make([]PhaseView, len(program.phases))
	for index, phase := range program.phases {
		result[index] = PhaseView{Index: phase.index, ID: phase.id, TimeoutTicks: phase.timeoutTicks, Roots: append([]RootIndex(nil), phase.roots...)}
	}
	return result
}

func inspectRoots(program *Program) []RootView {
	if program == nil {
		return nil
	}
	result := make([]RootView, len(program.roots))
	for index, root := range program.roots {
		result[index] = RootView{Index: root.index, Phase: root.phase, Event: root.event, Operation: root.operation, HasOperation: root.hasOperation}
	}
	return result
}

func inspectOperations(program *Program) []OperationView {
	if program == nil {
		return nil
	}
	result := make([]OperationView, len(program.operations))
	for index, operation := range program.operations {
		view := OperationView{Index: OperationIndex(index), Kind: operationKind(operation)}
		switch typed := operation.(type) {
		case sequenceOperation:
			view.Children = append(view.Children, typed.children...)
		case parallelOperation:
			view.Children = append(view.Children, typed.branches...)
		case branchOperation:
			view.Children = append(view.Children, typed.thenOperation)
			if typed.hasElse {
				view.Children = append(view.Children, typed.elseOperation)
			}
		case repeatOperation:
			view.Children = append(view.Children, typed.body)
		case waitOperation:
			view.Children = append(view.Children, typed.then)
		case queryOperation:
			selector := program.selectors[typed.selector]
			if selector.hasConsumer {
				view.Children = append(view.Children, selector.consumerRoot)
			}
			if selector.hasEmpty {
				view.Children = append(view.Children, selector.emptyRoot)
			}
		case damageOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case healOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case shieldOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case statusOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case modifyStatusInstanceOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case attributeModifierOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case resourceOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case memoryOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case stateOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case abilityStateOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case teleportOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case motionImpulseOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		case stopMovementOperation:
			view.Children = continuationChildren(typed.effectContinuations)
		}
		result[index] = view
	}
	return result
}

func continuationChildren(value effectContinuations) []OperationIndex {
	var result []OperationIndex
	if value.hasSuccess {
		result = append(result, value.success)
	}
	if value.hasFailure {
		result = append(result, value.failure)
	}
	return result
}

func inspectSelectors(program *Program) []SelectionView {
	if program == nil {
		return nil
	}
	result := make([]SelectionView, len(program.selectors))
	for index, selector := range program.selectors {
		mode := "one"
		if selector.consumerMode == consumerEach {
			mode = "each"
		}
		result[index] = SelectionView{Index: selector.index, ElementKind: selectionElementName(selector.element), Shape: selector.shape, ShapeKind: selector.shape, Limit: selector.limit, ConsumerMode: mode, ConsumerRoot: selector.consumerRoot, EmptyRoot: selector.emptyRoot, OrderBy: selector.order.by, OrderDirection: selector.order.direction}
	}
	return result
}

func operationEffectContinuations(operation operation) (effectContinuations, EffectIndex, bool) {
	switch typed := operation.(type) {
	case damageOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case healOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case shieldOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case statusOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case modifyStatusInstanceOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case attributeModifierOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case resourceOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case memoryOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case stateOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case abilityStateOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case spawnOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case entityCommandOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case teleportOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case motionImpulseOperation:
		return typed.effectContinuations, typed.effectIndex, true
	case stopMovementOperation:
		return typed.effectContinuations, typed.effectIndex, true
	default:
		return effectContinuations{}, 0, false
	}
}

func selectionElementName(element selectionElementType) string {
	switch element {
	case selectionEntity:
		return "entity"
	case selectionPosition:
		return "position"
	case selectionHit:
		return "hit"
	case selectionPath:
		return "path"
	case selectionAbility:
		return "ability"
	case selectionStatusInstance:
		return "status_instance"
	default:
		return "invalid"
	}
}

func inspectQuantities(program *Program) []QuantityView {
	if program == nil {
		return nil
	}
	result := make([]QuantityView, len(program.quantities))
	for index, quantity := range program.quantities {
		result[index] = QuantityView{Path: quantity.path, Kind: valueKindName(quantity.typ.Base), Quantity: quantityKindName(quantity.typ.Quantity), Minimum: quantity.minimum, Maximum: quantity.maximum, Proved: quantity.proved}
	}
	return result
}

func inspectRandomSites(program *Program) []RandomSiteView {
	if program == nil {
		return nil
	}
	result := make([]RandomSiteView, len(program.randomSites))
	for index, site := range program.randomSites {
		result[index] = RandomSiteView{Index: site.index, Kind: site.kind, InvocationBound: site.invocationBound}
	}
	return result
}

func inspectVisuals(program *Program) []VisualView {
	if program == nil {
		return nil
	}
	result := make([]VisualView, len(program.visuals))
	for index, visual := range program.visuals {
		result[index] = VisualView{Index: visual.index, Category: visual.category, Theme: visual.theme, Elements: append([]string(nil), visual.elements...)}
	}
	return result
}
