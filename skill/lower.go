package skill

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type loweringContext struct {
	artifacts      *compileArtifacts
	program        *Program
	memory         map[string]MemoryIndex
	input          map[string]uint16
	nextEffect     EffectIndex
	nextRandomSite RandomSiteIndex
}

type lowerScope map[string]LocalIndex

func Compile(definition *Definition, environment CompileEnvironment) (*Program, []Diagnostic) {
	artifacts, diagnostics := compileToArtifacts(definition, environment)
	if artifacts == nil || diagnosticsHaveErrors(diagnostics) {
		return nil, diagnostics
	}
	return lowerProgram(artifacts), diagnostics
}

func lowerProgram(artifacts *compileArtifacts) *Program {
	if artifacts == nil || artifacts.ir == nil || !artifacts.lowerReady || artifacts.types.types == nil || artifacts.graph.Index == nil {
		panic("skill: lowerProgram called without complete compiler artifacts")
	}
	program := &Program{
		id:                        artifacts.ir.id,
		name:                      artifacts.ir.name,
		description:               artifacts.ir.description,
		compilerSemanticsRevision: artifacts.metadata.CompilerSemanticsRevision,
		authority:                 artifacts.authority.identity,
		activationKind:            artifacts.ir.activation.kind,
		cooldownScope:             artifacts.ir.activation.cooldownScope,
		cooldownTicks:             artifacts.ir.cooldownTicks,
		globalCooldownTicks:       artifacts.ir.globalCooldownTicks,
		limits:                    artifacts.limits,
	}
	context := loweringContext{artifacts: artifacts, program: program, memory: make(map[string]MemoryIndex), input: make(map[string]uint16)}
	context.lowerInput()
	context.lowerAbilityProperties()
	context.lowerProcessProperties()
	context.lowerState()
	context.lowerMemory()
	context.lowerCastAndCosts()
	program.gameplayTags = append([]GameplayTagHandle(nil), artifacts.gameplay.skillTags...)
	context.lowerPhases()
	context.lowerSnapshots()
	context.lowerQuantities()
	context.lowerRandomSites()
	context.lowerEventPlans()
	program.visuals = append([]visualProgram(nil), artifacts.visual.entries...)
	program.castVisual, program.hasCastVisual = artifacts.visual.castIndex, artifacts.visual.hasCast
	program.visualCatalogRevision, program.visualCatalogDigest = artifacts.metadata.VisualRevision, artifacts.metadata.VisualDigest
	if initial, ok := artifacts.graph.Index[artifacts.ir.initialPhase]; ok {
		program.initialPhase = PhaseIndex(initial)
	}
	if len(program.operations) != len(artifacts.identity.Operations) {
		panic(fmt.Sprintf("skill: lowering produced %d operations for %d identities", len(program.operations), len(artifacts.identity.Operations)))
	}
	if int(context.nextRandomSite) != len(artifacts.identity.RandomSites) {
		panic(fmt.Sprintf("skill: lowering consumed %d random sites for %d identities", context.nextRandomSite, len(artifacts.identity.RandomSites)))
	}
	program.identity.sourceDocumentDigest = artifacts.metadata.SourceDocumentDigest
	program.identity.gameplayDigest = digestGameplayProgram(program)
	program.identity.presentationDigest = digestPresentationProgram(program, artifacts.metadata)
	return program
}

func (c *loweringContext) lowerProcessProperties() {
	policies := append([]ProcessPropertyPolicy(nil), c.artifacts.environmentProcessProperties()...)
	sort.Slice(policies, func(left, right int) bool { return policies[left].Handle < policies[right].Handle })
	for _, policy := range policies {
		var operations uint8
		for _, operation := range policy.Operations {
			operations |= uint8(1 << lowerProcessNumericOperation(operation))
		}
		processKinds := make([]processPropertyProcessKind, len(policy.ProcessKinds))
		for index, processKind := range policy.ProcessKinds {
			processKinds[index] = lowerProcessPropertyProcessKind(processKind)
		}
		slotBindings := make([]processPropertySlotBindingProgram, len(policy.SlotBindings))
		for index, binding := range policy.SlotBindings {
			slotBindings[index] = lowerProcessPropertySlotBinding(binding)
		}
		c.program.processProperties = append(c.program.processProperties, processPropertyProgram{handle: policy.Handle, key: lowerProcessPropertyKey(policy.Key), minimum: policy.Minimum, maximum: policy.Maximum, interpolation: processNumericLinearInteger, rounding: processNumericTruncateTowardZero, allowedOperationsMask: operations, processKinds: processKinds, slotBindings: slotBindings})
	}
}

func (artifacts *compileArtifacts) environmentProcessProperties() []ProcessPropertyPolicy {
	return artifacts.processProperties
}

func lookupProcessPropertyPolicyByArtifacts(artifacts *compileArtifacts, key string) (ProcessPropertyPolicy, bool) {
	for _, policy := range artifacts.processProperties {
		if policy.Key == key {
			return policy, true
		}
	}
	return ProcessPropertyPolicy{}, false
}

func lowerProcessNumericOperation(operation string) processNumericOperation {
	switch operation {
	case "set":
		return processNumericSet
	case "add":
		return processNumericAdd
	case "mul_bp":
		return processNumericMulBP
	default:
		panic("skill: unsupported process numeric operation")
	}
}

func lowerProcessPropertyKey(key string) processPropertyKey {
	switch key {
	case "speed":
		return processPropertySpeed
	case "radius":
		return processPropertyRadius
	case "arc_height":
		return processPropertyArcHeight
	case "turn_rate_mdeg_per_tick":
		return processPropertyTurnRateMDegPerTick
	case "angular_speed_mdeg_per_tick":
		return processPropertyAngularSpeedMDegPerTick
	case "offset_amplitude":
		return processPropertyOffsetAmplitude
	case "offset_radius":
		return processPropertyOffsetRadius
	case "return_speed_bp":
		return processPropertyReturnSpeedBP
	case "collision_force":
		return processPropertyCollisionForce
	default:
		panic("skill: unsupported process property key")
	}
}

func lowerProcessPropertyProcessKind(kind string) processPropertyProcessKind {
	switch kind {
	case "dash":
		return processPropertyProcessDash
	case "orbit":
		return processPropertyProcessOrbit
	case "projectile":
		return processPropertyProcessProjectile
	case "area":
		return processPropertyProcessArea
	default:
		panic("skill: unsupported process property process kind")
	}
}

func lowerProcessPropertySlotBinding(binding ProcessPropertySlotBinding) processPropertySlotBindingProgram {
	stage := map[string]processPropertySlotStage{"trajectory": processPropertySlotTrajectory, "steering": processPropertySlotSteering, "offset": processPropertySlotOffset, "completion": processPropertySlotCompletion, "collision": processPropertySlotCollision}[binding.Stage]
	variant := map[string]processPropertySlotVariant{"linear": processPropertyVariantLinear, "path": processPropertyVariantPath, "parabola": processPropertyVariantParabola, "orbit": processPropertyVariantOrbit, "tracking": processPropertyVariantTracking, "zigzag": processPropertyVariantZigzag, "circular": processPropertyVariantCircular, "boomerang": processPropertyVariantBoomerang, "present": processPropertyVariantPresent}[binding.Variant]
	field := map[string]processPropertySlotField{"speed": processPropertyFieldSpeed, "radius": processPropertyFieldRadius, "height": processPropertyFieldHeight, "turn_rate_mdeg_per_tick": processPropertyFieldTurnRateMDegPerTick, "angular_speed": processPropertyFieldAngularSpeed, "amplitude": processPropertyFieldAmplitude, "return_speed_bp": processPropertyFieldReturnSpeedBP, "force": processPropertyFieldForce}[binding.Field]
	if stage == 0 || variant == 0 || field == 0 {
		panic("skill: unsupported process property slot binding")
	}
	return processPropertySlotBindingProgram{stage: stage, variant: variant, field: field}
}

func (c *loweringContext) lowerAbilityProperties() {
	c.program.abilityControl = abilityControlProgram{selectableTags: append([]GameplayTagHandle(nil), c.artifacts.ability.selectableTags...), ownerRelations: append([]string(nil), c.artifacts.ability.ownerRelations...)}
	properties := make([]resolvedAbilityProperty, 0, len(c.artifacts.ability.properties))
	for _, property := range c.artifacts.ability.properties {
		properties = append(properties, property)
	}
	sort.Slice(properties, func(left, right int) bool { return properties[left].handle < properties[right].handle })
	for _, property := range properties {
		policy := property.policy
		c.program.abilityProperties = append(c.program.abilityProperties, abilityPropertyProgram{handle: property.handle, name: policy.Property, typ: policy.ValueType, mutable: policy.Mutable, minimum: policy.Minimum, maximum: policy.Maximum, maximumMutation: policy.MaximumMutation, maximumDuration: policy.MaximumDurationTicks})
	}
}

func (c *loweringContext) lowerState() {
	for _, name := range sortedStateNames(c.artifacts.ir.persistentState) {
		declaration := c.artifacts.ir.persistentState[name]
		minimum, maximum := int64(0), int64(0)
		if declaration.minimum != nil {
			minimum = *declaration.minimum
		}
		if declaration.maximum != nil {
			maximum = *declaration.maximum
		}
		c.program.states = append(c.program.states, stateSlotProgram{
			slot: c.artifacts.state.slots[name], name: name, typ: declaredStateType(declaration.declaredType), scope: declaration.scope,
			defaultValue: c.lowerValue(declaration.defaultValue, nil), minimum: minimum, maximum: maximum,
			enumValues: append([]string(nil), declaration.enumValues...), durationTicks: declaration.durationTicks,
			maximumDurationTicks: declaration.maximumDurationTicks, onWrite: declaration.onWrite, clearOn: append([]string(nil), declaration.clearOn...),
		})
	}
}

func (c *loweringContext) lowerCastAndCosts() {
	policy := c.artifacts.ir.activation.policy
	window := c.artifacts.ir.activation.castWindow
	c.program.cast = castWindowProgram{
		windupTicks: window.windupTicks, commitTick: window.commitTick, recoveryTicks: window.recoveryTicks,
		hasWindupExpression: window.hasWindupExpression, windupTicksMin: window.windupTicksMin, windupTicksMax: window.windupTicksMax,
		hasRecoveryExpression: window.hasRecoveryExpression, recoveryTicksMin: window.recoveryTicksMin, recoveryTicksMax: window.recoveryTicksMax,
		movement: window.movement, turning: window.turning, refundBeforeCommit: window.refundBeforeCommit, concurrent: window.concurrent,
		mode: policy.mode, pulseIntervalTicks: policy.pulseIntervalTicks, maxDurationTicks: policy.maxDurationTicks,
		maxChargeTicks: policy.maxChargeTicks, minChargeBP: policy.minChargeBP, autoRelease: policy.autoRelease,
		maxStock: policy.maxStock, rechargeTicks: policy.rechargeTicks, initialStock: policy.initialStock,
	}
	if window.hasWindupExpression {
		c.program.cast.windupExpression = c.lowerValue(window.windupExpression, nil)
	}
	if window.hasRecoveryExpression {
		c.program.cast.recoveryExpression = c.lowerValue(window.recoveryExpression, nil)
	}
	for _, key := range window.interruptTags {
		if handle, ok := c.artifacts.authority.tags[key]; ok {
			c.program.cast.interruptTags = append(c.program.cast.interruptTags, handle)
		}
	}
	for _, cost := range c.artifacts.ir.costs {
		c.program.costs = append(c.program.costs, costProgram{resource: lookupResourceHandle(c.artifacts, cost.resource), amount: c.lowerValue(cost.amount, nil)})
	}
	for _, cost := range policy.sustainCosts {
		c.program.cast.sustainCosts = append(c.program.cast.sustainCosts, costProgram{resource: lookupResourceHandle(c.artifacts, cost.resource), amount: c.lowerValue(cost.amount, nil)})
	}
}

func (c *loweringContext) lowerSnapshots() {
	paths := make([]string, 0, len(c.artifacts.snapshots.reads))
	for path := range c.artifacts.snapshots.reads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		plan := c.artifacts.snapshots.reads[path]
		c.program.snapshots = append(c.program.snapshots, attributeSnapshotProgram{slot: plan.SnapshotSlot, entity: c.lowerValue(plan.Entity, nil), attribute: plan.Attribute, point: plan.Snapshot})
	}
	sort.SliceStable(c.program.snapshots, func(i, j int) bool { return c.program.snapshots[i].slot < c.program.snapshots[j].slot })
}

func (c *loweringContext) lowerInput() {
	names := make([]string, 0, len(c.artifacts.input.Slots))
	for name := range c.artifacts.input.Slots {
		names = append(names, name)
	}
	sort.Strings(names)
	input := c.artifacts.input
	c.program.input = inputProgram{
		kind: input.Kind, maximumRange: input.MaximumRange, hasMaximumRange: input.HasMaximumRange,
		minimumLength: input.MinimumLength, maximumLength: input.MaximumLength,
		maximumPathPoints: input.MaximumPathPoints, maximumPathLength: input.MaximumPathLength, minimumSegmentLength: input.MinimumSegmentLength,
		clampPolicy: input.ClampPolicy, simplificationPolicy: input.SimplificationPolicy,
	}
	for _, port := range []InputPort{InputPortDirectionChanged, InputPortTargetChanged} {
		if input.UpdatePorts[port] {
			c.program.input.updatePorts = append(c.program.input.updatePorts, port)
		}
	}
	for _, name := range names {
		index := uint16(len(c.program.input.slots))
		c.input[name] = index
		c.program.input.slots = append(c.program.input.slots, inputSlotProgram{name: name, typ: c.artifacts.input.Slots[name]})
	}
}

func (c *loweringContext) lowerMemory() {
	names := make([]string, 0, len(c.artifacts.ir.memory))
	for name := range c.artifacts.ir.memory {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		declaration := c.artifacts.ir.memory[name]
		index := MemoryIndex(len(c.program.memory))
		c.memory[name] = index
		typ := declaredMemoryType(declaration.declaredType)
		if _, optional := declaration.defaultValue.(*nullValueIR); optional {
			typ.Optional = true
		}
		c.program.memory = append(c.program.memory, memorySlotProgram{index: index, name: name, typ: typ})
	}
	for index, name := range names {
		declaration := c.artifacts.ir.memory[name]
		c.program.memory[index].defaultValue = c.lowerValue(declaration.defaultValue, nil)
	}
}

func (c *loweringContext) lowerPhases() {
	for phaseIndex, phase := range c.artifacts.ir.phases {
		compiled := phaseProgram{index: PhaseIndex(phaseIndex), id: phase.id, timeoutTicks: phase.timeoutTicks}
		for _, event := range phaseEventFlows(phase.events) {
			if event.flow == nil {
				continue
			}
			operation, present := c.lowerFlow(event.flow, nil)
			root := rootProgram{index: RootIndex(len(c.program.roots)), phase: compiled.index, event: event.name, operation: operation, hasOperation: present}
			c.program.roots = append(c.program.roots, root)
			compiled.roots = append(compiled.roots, root.index)
		}
		c.program.phases = append(c.program.phases, compiled)
	}
}

type namedFlow struct {
	name string
	flow flowIR
}

func phaseEventFlows(events phaseEventsIR) []namedFlow {
	return []namedFlow{
		{"enter", events.enter}, {"recast", events.recast}, {"cancel", events.cancel},
		{"direction_changed", events.directionChanged}, {"target_changed", events.targetChanged},
		{"timeout", events.timeout}, {"release", events.release}, {"pulse", events.pulse},
	}
}

func (c *loweringContext) lowerFlow(flow flowIR, scope lowerScope) (OperationIndex, bool) {
	if flow == nil {
		return 0, false
	}
	index := OperationIndex(len(c.program.operations))
	header := operationHeader{index: index, sourcePath: flow.sourceRef().Path}
	if int(index) < len(c.artifacts.identity.Operations) && c.artifacts.identity.Operations[index].Path != header.sourcePath {
		panic(fmt.Sprintf("skill: operation identity mismatch at %d", index))
	}
	c.program.operations = append(c.program.operations, nil)
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		operation := sequenceOperation{operationHeader: header}
		for _, child := range typed.steps {
			childIndex, present := c.lowerFlow(child, scope)
			if present {
				operation.children = append(operation.children, childIndex)
			}
		}
		c.program.operations[index] = operation
	case *parallelFlowIR:
		operation := parallelOperation{operationHeader: header}
		for _, branch := range typed.branches {
			branchIndex, present := c.lowerFlow(branch, cloneLowerScope(scope))
			if present {
				operation.branches = append(operation.branches, branchIndex)
			}
		}
		c.program.operations[index] = operation
	case *ifFlowIR:
		thenIndex, _ := c.lowerFlow(typed.thenFlow, cloneLowerScope(scope))
		elseIndex, hasElse := c.lowerFlow(typed.elseFlow, cloneLowerScope(scope))
		c.program.operations[index] = branchOperation{operationHeader: header, condition: c.lowerValue(typed.condition, scope), thenOperation: thenIndex, elseOperation: elseIndex, hasElse: hasElse}
	case *repeatFlowIR:
		childScope := cloneLowerScope(scope)
		local := c.allocateLocal(typed.index.Name, quantityType(quantityCount))
		childScope[typed.index.Name] = local
		body, _ := c.lowerFlow(typed.body, childScope)
		c.program.operations[index] = repeatOperation{operationHeader: header, times: c.lowerValue(typed.times, scope), intervalTicks: typed.intervalTicks, indexLocal: local, body: body}
	case *waitFlowIR:
		then, _ := c.lowerFlow(typed.then, cloneLowerScope(scope))
		c.program.operations[index] = waitOperation{operationHeader: header, ticks: typed.ticks, then: then}
	case *selectFlowIR:
		selectorIndex := SelectorIndex(len(c.program.selectors))
		c.program.selectors = append(c.program.selectors, selectorProgram{})
		selector := selectorProgram{index: selectorIndex, element: typed.selectPlan.elementType, shape: shapeProgramName(typed.selectPlan.shape), limit: typed.selectPlan.limit, from: c.lowerValue(typed.selectPlan.from, scope), shapePlan: c.lowerShape(typed.selectPlan.shape, scope), filters: c.lowerFilters(typed.selectPlan.filters, scope)}
		if typed.selectPlan.order != nil {
			selector.order = selectOrderProgram{by: typed.selectPlan.order.by, direction: typed.selectPlan.order.direction}
			if typed.selectPlan.order.by == "random" {
				selector.randomSite, selector.hasRandomSite = c.nextRandomSite, true
				c.nextRandomSite++
			}
		}
		childScope := cloneLowerScope(scope)
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			selector.consumerMode = consumerOne
			selector.consumerLocal = c.allocateLocal(consume.local.Name, selectionValueType(typed.selectPlan.elementType))
			childScope[consume.local.Name] = selector.consumerLocal
			selector.consumerRoot, selector.hasConsumer = c.lowerFlow(consume.then, childScope)
		case *selectEachConsumeIR:
			selector.consumerMode = consumerEach
			selector.consumerLocal = c.allocateLocal(consume.local.Name, selectionValueType(typed.selectPlan.elementType))
			childScope[consume.local.Name] = selector.consumerLocal
			selector.consumerRoot, selector.hasConsumer = c.lowerFlow(consume.body, childScope)
		}
		selector.emptyRoot, selector.hasEmpty = c.lowerFlow(typed.onEmpty, cloneLowerScope(scope))
		c.program.selectors[selectorIndex] = selector
		c.program.operations[index] = queryOperation{operationHeader: header, selector: selectorIndex}
	case *effectFlowIR:
		c.program.operations[index] = c.lowerEffect(header, typed, scope)
	case *gotoFlowIR:
		phase, ok := c.artifacts.graph.Index[typed.phase]
		if !ok {
			panic("skill: unresolved goto reached lower")
		}
		c.program.operations[index] = gotoOperation{operationHeader: header, phase: PhaseIndex(phase)}
	case *finishFlowIR:
		c.program.operations[index] = finishOperation{operationHeader: header, reason: typed.reason}
	default:
		panic(fmt.Sprintf("skill: unsupported flow %T", flow))
	}
	return index, true
}

func (c *loweringContext) lowerShape(shape shapeIR, scope lowerScope) shapeProgram {
	result := shapeProgram{kind: shapeProgramName(shape)}
	add := func(values ...valueIR) {
		for _, value := range values {
			result.values = append(result.values, c.lowerValue(value, scope))
		}
	}
	switch typed := shape.(type) {
	case *circleShapeIR:
		add(typed.radius)
	case *ringShapeIR:
		add(typed.innerRadius, typed.outerRadius)
	case *coneShapeIR:
		add(typed.rangeValue, typed.angleDeg, typed.direction)
	case *lineShapeIR:
		add(typed.length, typed.width, typed.direction)
	case *rectangleShapeIR:
		add(typed.length, typed.width, typed.direction)
	case *raycastShapeIR:
		add(typed.length, typed.direction)
		result.collision = c.lowerCollision(typed.collision)
	case *chainShapeIR:
		add(typed.hopRange)
		result.maxTargets, result.allowRepeat, result.hopIntervalTicks = typed.maxTargets, typed.allowRepeat, typed.hopIntervalTicks
	case *pathShapeIR:
		add(typed.points)
	case *nearestValidShapeIR:
		add(typed.searchRadius)
		result.collision = c.lowerCollision(typed.collision)
	}
	return result
}

func (c *loweringContext) lowerCollision(keys []string) []CollisionLayerHandle {
	result := make([]CollisionLayerHandle, 0, len(keys))
	for _, key := range keys {
		result = append(result, c.artifacts.authority.collision[key])
	}
	return result
}

func (c *loweringContext) lowerFilters(filters []filterIR, scope lowerScope) []filterProgram {
	result := make([]filterProgram, 0, len(filters))
	for _, filter := range filters {
		switch typed := filter.(type) {
		case *flagFilterIR:
			result = append(result, filterProgram{kind: typed.kind})
		case *relationFilterIR:
			result = append(result, filterProgram{kind: "relation", relation: typed.relation})
		case *statusFilterIR:
			result = append(result, filterProgram{kind: typed.kind, status: lookupStatusHandle(c.artifacts, typed.status)})
		case *attributeCompareFilterIR:
			result = append(result, filterProgram{kind: "attribute_compare", attribute: lookupAttributeHandle(c.artifacts, typed.attribute), operation: typed.op, value: c.lowerValue(typed.value, scope)})
		case *gameplayTagFilterIR:
			result = append(result, filterProgram{kind: typed.kind, tag: c.artifacts.authority.tags[typed.tag]})
		case *lineOfSightFilterIR:
			result = append(result, filterProgram{kind: "line_of_sight", collision: c.lowerCollision(typed.collision)})
		case *abilityTagFilterIR:
			result = append(result, filterProgram{kind: "ability_tag", tag: c.artifacts.authority.tags[typed.tag]})
		case *abilitySlotFilterIR:
			result = append(result, filterProgram{kind: "ability_slot", slot: typed.slot})
		case *ownedSourceSkillFilterIR:
			result = append(result, filterProgram{kind: "source_skill", text: typed.skill})
		case *ownedSourceCastFilterIR:
			result = append(result, filterProgram{kind: "source_cast", cast: typed.cast})
		case *ownedSpawnTickFilterIR:
			result = append(result, filterProgram{kind: typed.kind, tick: typed.tick})
		case *ownedUnitTemplateFilterIR:
			result = append(result, filterProgram{kind: "unit_template", template: c.artifacts.authority.unitTemplates[typed.template]})
		case *ownedEntityTagFilterIR:
			result = append(result, filterProgram{kind: "entity_tag", tag: c.artifacts.authority.tags[typed.tag]})
		case *statusInstanceFilterIR:
			filter := filterProgram{kind: typed.kind, text: typed.text, operation: typed.operation}
			if typed.status != "" {
				filter.status = lookupStatusHandle(c.artifacts, typed.status)
			}
			if typed.kind == "status_tag" {
				filter.tag = c.artifacts.authority.tags[typed.text]
			}
			if typed.value != nil {
				filter.value = c.lowerValue(typed.value, scope)
			}
			result = append(result, filter)
		}
	}
	return result
}

func (c *loweringContext) lowerEffect(header operationHeader, flow *effectFlowIR, scope lowerScope) operation {
	effectIndex := c.nextEffect
	c.nextEffect++
	continuations := effectContinuations{}
	if index, found := c.artifacts.visual.bySourcePath[flow.effect.sourceRef().Path]; found {
		continuations.visual, continuations.hasVisual = index, true
	}
	if flow.hasResultLayout {
		continuations.result = cloneResultLayout(flow.resultLayout)
	}
	resultScope := cloneLowerScope(scope)
	if flow.result != nil && flow.result.local != nil {
		resultType := effectResultReferenceType(flow.result.layout, resultOutcomeAny)
		continuations.resultLocal = c.allocateLocal(flow.result.local.Name, resultType)
		continuations.hasResultLocal = true
		resultScope[flow.result.local.Name] = continuations.resultLocal
	}
	if flow.result != nil {
		continuations.success, continuations.hasSuccess = c.lowerFlow(flow.result.success, cloneLowerScope(resultScope))
		continuations.failure, continuations.hasFailure = c.lowerFlow(flow.result.failure, cloneLowerScope(resultScope))
	}
	if flow.callbacks != nil || flow.process != nil {
		continuations.processTemplate = ProcessTemplateIndex(len(c.program.processTemplates))
		continuations.hasProcess = true
		template := processTemplateProgram{index: continuations.processTemplate}
		if flow.process != nil {
			template.durationTicks = flow.process.durationTicks
			template.intervalTicks = flow.process.intervalTicks
			template.emitLeaveOnStop = flow.process.emitLeaveOnStop
			if visual, found := c.artifacts.visual.bySourcePath[flow.process.source.Path]; found {
				template.visual, template.hasVisual = visual, true
			}
			if flow.process.area != nil {
				area := c.lowerSelectorPlan(*flow.process.area, scope)
				template.area = &area
			}
			template.motion = c.lowerMotionProgram(flow.process.motion, scope)
			template.numericTracks = make([]numericTrackProgram, len(flow.process.numericTracks))
			for index, track := range flow.process.numericTracks {
				policy, found := lookupProcessPropertyPolicyByArtifacts(c.artifacts, track.property)
				if !found {
					panic("skill: process property was not resolved")
				}
				template.numericTracks[index] = numericTrackProgram{property: policy.Handle, operation: lowerProcessNumericOperation(track.operation), value: c.lowerValue(track.value, scope), overTicks: track.overTicks}
			}
		}
		if flow.callbacks != nil {
			callbackEvents := []namedFlow{
				{"enter", flow.callbacks.enter},
				{"hit", flow.callbacks.hit},
				{"cancel", flow.callbacks.cancel},
				{"collision", flow.callbacks.collision},
				{"transition", flow.callbacks.transition},
				{"end", flow.callbacks.end},
				{"target_lost", flow.callbacks.targetLost},
				{"tick", flow.callbacks.tick},
				{"leave", flow.callbacks.leave},
			}
			for _, callback := range callbackEvents {
				if operationIndex, present := c.lowerFlow(callback.flow, cloneLowerScope(scope)); present {
					template.callbacks = append(template.callbacks, processCallbackProgram{event: callback.name, operation: operationIndex})
				}
			}
		}
		c.program.processTemplates = append(c.program.processTemplates, template)
	}
	switch effect := flow.effect.(type) {
	case *captureSnapshotEffectIR:
		profile, found := c.artifacts.temporal.profiles[effect.source.Path]
		if !found {
			panic("skill: temporal profile was not resolved")
		}
		return captureSnapshotOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), profile: profile.handle}
	case *restoreSnapshotEffectIR:
		return restoreSnapshotOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), snapshot: c.lowerValue(effect.snapshot, scope), onBlocked: effect.onBlocked}
	case *damageEffectIR:
		semantics := c.artifacts.gameplay.damage[effect.source.Path]
		return damageOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), amount: c.lowerValue(effect.amount, scope), damageType: semantics.DamageType, element: semantics.Element, combatTags: append([]GameplayTagHandle(nil), semantics.CombatTags...), canCritical: effect.canCritical}
	case *healEffectIR:
		return healOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), amount: c.lowerValue(effect.amount, scope)}
	case *shieldEffectIR:
		return shieldOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), amount: c.lowerValue(effect.amount, scope), durationTicks: effect.durationTicks}
	case *addStatusEffectIR:
		return statusOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), status: lookupStatusHandle(c.artifacts, effect.status), durationTicks: effect.durationTicks, stacks: effect.stacks, maxStacks: pointerIntValue(effect.maxStacks)}
	case *removeStatusEffectIR:
		return statusOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), status: lookupStatusHandle(c.artifacts, effect.status), remove: true}
	case *modifyStatusInstanceEffectIR:
		operation := modifyStatusInstanceOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, status: c.lowerValue(effect.status, scope), operation: effect.operation, ownershipPolicy: effect.ownershipPolicy}
		if effect.value != nil {
			operation.value, operation.hasValue = c.lowerValue(effect.value, scope), true
		}
		if effect.target != nil {
			operation.target, operation.hasTarget = c.lowerValue(effect.target, scope), true
		}
		return operation
	case *attributeModifierEffectIR:
		return attributeModifierOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), attribute: lookupAttributeHandle(c.artifacts, effect.attribute), operation: effect.operation, value: c.lowerValue(effect.value, scope), durationTicks: effect.durationTicks, stackPolicy: effect.stackPolicy, maxStacks: effect.maxStacks}
	case *resourceEffectIR:
		return resourceOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), amount: c.lowerValue(effect.amount, scope), resource: lookupResourceHandle(c.artifacts, effect.resource), operation: effect.operation}
	case *setMemoryEffectIR:
		return memoryOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, memory: c.memory[effect.name], operation: "set", value: c.lowerValue(effect.value, scope)}
	case *addMemoryEffectIR:
		return memoryOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, memory: c.memory[effect.name], operation: "add", value: c.lowerValue(effect.value, scope)}
	case *clearMemoryEffectIR:
		return memoryOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, memory: c.memory[effect.name], operation: "clear"}
	case *modifyStateEffectIR:
		operation := stateOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, state: c.lowerStateReference(effect.state), binding: c.lowerStateBinding(effect.owner, effect.subject, effect.teamOf, scope), operation: effect.operation, durationTicks: effect.durationTicks, expiryPolicy: effect.expiryPolicy}
		if effect.value != nil {
			operation.value = c.lowerValue(effect.value, scope)
			operation.hasValue = true
		}
		return operation
	case *modifyAbilityStateEffectIR:
		property := c.artifacts.ability.properties[effect.property]
		return abilityStateOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, owner: c.lowerValue(effect.owner, scope), ability: c.lowerValue(effect.ability, scope), property: property.handle, propertyName: effect.property, operation: effect.operation, value: c.lowerValue(effect.value, scope), durationTicks: effect.durationTicks}
	case *modifyProcessEffectIR:
		policy, found := lookupProcessPropertyPolicyByArtifacts(c.artifacts, effect.property)
		if !found {
			panic("skill: process property was not resolved")
		}
		return modifyProcessOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, process: c.lowerValue(effect.process, scope), property: policy.Handle, operation: lowerProcessNumericOperation(effect.operation), value: c.lowerValue(effect.value, scope), overTicks: effect.overTicks}
	case *spawnEffectIR:
		overrides := make([]spawnAttributeOverrideProgram, len(effect.attributeOverrides))
		for index, override := range effect.attributeOverrides {
			overrides[index] = spawnAttributeOverrideProgram{attribute: c.artifacts.authority.attributes[override.attribute], value: c.lowerValue(override.value, scope)}
		}
		parameters := make([]spawnParameterBindingProgram, len(effect.parameterBindings))
		for index, binding := range effect.parameterBindings {
			parameters[index] = spawnParameterBindingProgram{name: binding.name, value: c.lowerValue(binding.value, scope)}
		}
		return spawnOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, template: c.artifacts.authority.unitTemplates[effect.template], position: c.lowerValue(effect.position, scope), count: effect.count, durationTicks: effect.durationTicks, attributeOverrides: overrides, parameterBindings: parameters}
	case *entityCommandEffectIR:
		operation := entityCommandOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), command: effect.command, behavior: effect.behavior}
		if effect.position != nil {
			operation.position, operation.hasPosition = c.lowerValue(effect.position, scope), true
		}
		if effect.targetEntity != nil {
			operation.targetEntity, operation.hasTargetEntity = c.lowerValue(effect.targetEntity, scope), true
		}
		return operation
	case *teleportEffectIR:
		return teleportOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope), destination: c.lowerValue(effect.destination, scope), onBlocked: effect.onBlocked}
	case *knockbackEffectIR:
		return motionImpulseOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, kind: "knockback", target: c.lowerValue(effect.target, scope), origin: c.lowerValue(effect.from, scope), distance: c.lowerValue(effect.distance, scope)}
	case *pullEffectIR:
		return motionImpulseOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, kind: "pull", target: c.lowerValue(effect.target, scope), origin: c.lowerValue(effect.toward, scope), distance: c.lowerValue(effect.distance, scope)}
	case *stopMovementEffectIR:
		return stopMovementOperation{operationHeader: header, effectContinuations: continuations, effectIndex: effectIndex, target: c.lowerValue(effect.target, scope)}
	default:
		panic(fmt.Sprintf("skill: unsupported effect %T", flow.effect))
	}
}

func (c *loweringContext) lowerSelectorPlan(plan selectIR, scope lowerScope) selectorProgram {
	selector := selectorProgram{element: plan.elementType, shape: shapeProgramName(plan.shape), limit: plan.limit, from: c.lowerValue(plan.from, scope), shapePlan: c.lowerShape(plan.shape, scope), filters: c.lowerFilters(plan.filters, scope)}
	if plan.order != nil {
		selector.order = selectOrderProgram{by: plan.order.by, direction: plan.order.direction}
	}
	return selector
}

func (c *loweringContext) lowerMotionProgram(value motionIR, scope lowerScope) *motionProgram {
	motion, ok := value.(*canonicalMotionIR)
	if !ok || motion == nil {
		return nil
	}
	result := &motionProgram{offsets: make([]motionOffsetProgram, len(motion.offsets))}
	switch frame := motion.frame.(type) {
	case worldFrameIR:
		result.frame = worldMotionFrameProgram{}
	case followFrameIR:
		result.frame = followMotionFrameProgram{target: c.lowerValue(frame.target, scope)}
	default:
		panic("skill: unsupported motion frame during lowering")
	}
	switch steering := motion.steering.(type) {
	case nil:
		result.steering = fixedMotionSteeringProgram{}
	case trackingSteeringIR:
		result.steering = trackingMotionSteeringProgram{target: c.lowerValue(steering.target, scope), durationTicks: steering.durationTicks}
	default:
		panic("skill: unsupported motion steering during lowering")
	}
	switch trajectory := motion.trajectory.(type) {
	case stationaryTrajectoryIR:
		result.trajectory = stationaryMotionTrajectoryProgram{}
	case linearTrajectoryIR:
		result.trajectory = linearMotionTrajectoryProgram{speed: c.lowerValue(trajectory.speed, scope)}
	case pathTrajectoryIR:
		result.trajectory = pathMotionTrajectoryProgram{points: c.lowerValue(trajectory.points, scope), speed: c.lowerValue(trajectory.speed, scope)}
	case orbitTrajectoryIR:
		result.trajectory = orbitMotionTrajectoryProgram{anchor: c.lowerValue(trajectory.anchor, scope), radius: c.lowerValue(trajectory.radius, scope), angularSpeed: c.lowerValue(trajectory.angularSpeed, scope)}
	case parabolaTrajectoryIR:
		result.trajectory = parabolaMotionTrajectoryProgram{destination: c.lowerValue(trajectory.destination, scope), height: c.lowerValue(trajectory.height, scope), durationTicks: trajectory.durationTicks}
	default:
		panic("skill: unsupported motion trajectory during lowering")
	}
	for index, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagOffsetIR:
			result.offsets[index] = zigzagMotionOffsetProgram{amplitude: c.lowerValue(typed.amplitude, scope), periodTicks: typed.periodTicks}
		case circularOffsetIR:
			result.offsets[index] = circularMotionOffsetProgram{radius: c.lowerValue(typed.radius, scope), angularSpeed: c.lowerValue(typed.angularSpeed, scope)}
		default:
			panic("skill: unsupported motion offset during lowering")
		}
	}
	if motion.collision != nil {
		response := motionCollisionStop
		switch motion.collision.response {
		case "reflect":
			response = motionCollisionReflect
		case "pierce":
			response = motionCollisionPierce
		}
		result.collision = &motionCollisionProgram{layers: c.lowerCollision(motion.collision.layers), response: response, maxReflects: motion.collision.maxReflects, maxPierces: motion.collision.maxPierces}
	}
	if motion.carry != nil {
		result.carry = &motionCarryProgram{target: c.lowerValue(motion.carry.target, scope)}
	}
	switch completion := motion.completion.(type) {
	case endCompletionIR:
		result.completion = endMotionCompletionProgram{}
	case pauseThenEndCompletionIR:
		result.completion = pauseThenEndMotionCompletionProgram{pauseTicks: completion.pauseTicks}
	case boomerangCompletionIR:
		result.completion = boomerangMotionCompletionProgram{maxReturnTicks: completion.maxReturnTicks}
	default:
		panic("skill: unsupported motion completion during lowering")
	}
	return result
}

func (c *loweringContext) lowerValue(value valueIR, scope lowerScope) programValue {
	switch typed := value.(type) {
	case *nullValueIR:
		return nullProgramValue{typ: typed.valueType()}
	case *intValueIR:
		return intProgramValue{value: typed.value, typ: typed.valueType()}
	case *boolValueIR:
		return boolProgramValue{value: typed.value}
	case *stringValueIR:
		return stringProgramValue{value: typed.value}
	case *referenceValueIR:
		return c.lowerReference(typed, scope)
	case *expressionValueIR:
		result := expressionProgramValue{op: typed.op, typ: typed.resolvedType}
		for _, argument := range typed.args {
			result.args = append(result.args, c.lowerValue(argument, scope))
		}
		return result
	case *attributeReadValueIR:
		plan, planned := c.artifacts.snapshots.reads[typed.source.Path]
		if !planned {
			// A read the snapshot pass never visited would lower to attribute
			// handle 0 and silently read the wrong (or no) attribute at
			// runtime — the walkValues traversal must cover every value site.
			panic("skill: attribute read at " + typed.source.Path + " has no snapshot plan")
		}
		return attributeReadProgramValue{entity: c.lowerValue(typed.entity, scope), attribute: plan.Attribute, snapshot: plan.Snapshot, snapshotSlot: plan.SnapshotSlot, typ: typed.resolvedType}
	case *stateReadValueIR:
		return stateReadProgramValue{state: c.lowerStateReference(typed.state), binding: c.lowerStateBinding(typed.owner, typed.subject, typed.teamOf, scope), snapshot: typed.snapshot, typ: typed.resolvedType}
	case *abilityStateReadValueIR:
		property := c.artifacts.ability.properties[typed.property]
		return abilityStateReadProgramValue{owner: c.lowerValue(typed.owner, scope), ability: c.lowerValue(typed.ability, scope), property: property.handle, name: typed.property, snapshot: typed.snapshot, typ: typed.resolvedType}
	default:
		panic(fmt.Sprintf("skill: unsupported value %T", value))
	}
}

func (c *loweringContext) lowerStateReference(name string) stateReferenceProgram {
	plan, found := c.artifacts.state.plans[name]
	if !found {
		panic("skill: unresolved state reference")
	}
	return stateReferenceProgram{
		shared: plan.shared, slot: plan.slot, typ: plan.typ, scope: plan.scope,
		defaultValue: c.lowerValue(plan.defaultValue, nil), minimum: plan.minimum, maximum: plan.maximum,
		durationTicks: plan.durationTicks, maximumDurationTicks: plan.maximumDurationTicks,
		onWrite: plan.onWrite, clearOn: append([]string(nil), plan.clearOn...),
	}
}

func (c *loweringContext) lowerStateBinding(owner, subject, teamOf valueIR, scope lowerScope) stateBindingProgram {
	binding := stateBindingProgram{}
	if owner != nil {
		binding.owner, binding.hasOwner = c.lowerValue(owner, scope), true
	}
	if subject != nil {
		binding.subject, binding.hasSubject = c.lowerValue(subject, scope), true
	}
	if teamOf != nil {
		binding.teamOf, binding.hasTeamOf = c.lowerValue(teamOf, scope), true
	}
	return binding
}

func (c *loweringContext) lowerReference(reference *referenceValueIR, scope lowerScope) programValue {
	if name, field, found := resolveIndexedReference(reference.reference, "$memory.", memoryIndexMap(c.memory)); found {
		return referenceProgramValue{kind: referenceMemory, index: name, field: field, typ: reference.resolvedType}
	}
	if name, field, found := resolveIndexedReference(reference.reference, "$local.", localIndexMap(scope)); found {
		return referenceProgramValue{kind: referenceLocal, index: name, field: field, resultField: reference.resultField, typ: reference.resolvedType}
	}
	if index, found := c.input[reference.reference]; found {
		return referenceProgramValue{kind: referenceInput, index: index, typ: reference.resolvedType}
	}
	return referenceProgramValue{kind: referenceBuiltin, builtin: reference.reference, typ: reference.resolvedType}
}

func resolveIndexedReference(reference, prefix string, indexes map[string]uint16) (uint16, string, bool) {
	if !strings.HasPrefix(reference, prefix) {
		return 0, "", false
	}
	remainder := strings.TrimPrefix(reference, prefix)
	best := ""
	for name := range indexes {
		if (remainder == name || strings.HasPrefix(remainder, name+".")) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return 0, "", false
	}
	field := ""
	if remainder != best {
		field = strings.TrimPrefix(remainder, best+".")
	}
	return indexes[best], field, true
}

func memoryIndexMap(values map[string]MemoryIndex) map[string]uint16 {
	result := make(map[string]uint16, len(values))
	for name, index := range values {
		result[name] = uint16(index)
	}
	return result
}

func localIndexMap(values lowerScope) map[string]uint16 {
	result := make(map[string]uint16, len(values))
	for name, index := range values {
		result[name] = uint16(index)
	}
	return result
}

func (c *loweringContext) allocateLocal(name string, typ valueType) LocalIndex {
	index := LocalIndex(len(c.program.locals))
	c.program.locals = append(c.program.locals, localSlotProgram{index: index, name: name, typ: typ})
	return index
}

func cloneLowerScope(scope lowerScope) lowerScope {
	result := make(lowerScope, len(scope)+1)
	for name, index := range scope {
		result[name] = index
	}
	return result
}

func (c *loweringContext) lowerQuantities() {
	paths := make([]string, 0, len(c.artifacts.types.types))
	for path := range c.artifacts.types.types {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		typ := c.artifacts.types.types[path]
		if typ.Base == valueKindInt {
			c.program.quantities = append(c.program.quantities, quantityProgram{path: path, typ: typ, minimum: -int64(^uint64(0)>>1) - 1, maximum: int64(^uint64(0) >> 1), proved: true})
		}
	}
}

func (c *loweringContext) lowerRandomSites() {
	for _, site := range c.artifacts.identity.RandomSites {
		c.program.randomSites = append(c.program.randomSites, randomSiteProgram{index: RandomSiteIndex(site.ID), kind: "selection_order", invocationBound: site.InvocationBound})
	}
}

func (c *loweringContext) lowerEventPlans() {
	if c.artifacts.proc.plan != nil {
		c.program.eventPlans = append(c.program.eventPlans, eventPlanProgram{filter: c.artifacts.proc.plan.Filter, proc: *c.artifacts.proc.plan})
	}
}

func shapeProgramName(shape shapeIR) string {
	switch shape.(type) {
	case *singleShapeIR:
		return "single"
	case *circleShapeIR:
		return "circle"
	case *ringShapeIR:
		return "ring"
	case *coneShapeIR:
		return "cone"
	case *lineShapeIR:
		return "line"
	case *rectangleShapeIR:
		return "rectangle"
	case *raycastShapeIR:
		return "raycast"
	case *chainShapeIR:
		return "chain"
	case *pathShapeIR:
		return "path"
	case *nearestValidShapeIR:
		return "nearest_valid"
	case *abilitySetShapeIR:
		return "ability_set"
	case *statusSetShapeIR:
		return "status_set"
	case *ownedEntitiesShapeIR:
		return "owned_entities"
	default:
		return "invalid"
	}
}

func pointerIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func lookupStatusHandle(artifacts *compileArtifacts, key string) StatusHandle {
	return artifacts.authority.statuses[key]
}

func lookupAttributeHandle(artifacts *compileArtifacts, key string) AttributeHandle {
	return artifacts.authority.attributes[key]
}

func lookupResourceHandle(artifacts *compileArtifacts, key string) ResourceHandle {
	return artifacts.authority.resources[key]
}

func digestGameplayProgram(program *Program) string {
	payload, err := json.Marshal(gameplayProgramDigestPayload(program))
	if err != nil {
		panic(err)
	}
	return stableDigest("roost.skill/v2/gameplay-program", payload)
}

func digestPresentationProgram(program *Program, metadata compileMetadata) string {
	payload, err := json.Marshal(struct {
		GameplayDigest, Name, Description, VisualRevision, VisualDigest string
		Visuals                                                         []VisualView
	}{program.identity.gameplayDigest, program.name, program.description, metadata.VisualRevision, metadata.VisualDigest, inspectVisuals(program)})
	if err != nil {
		panic(err)
	}
	return stableDigest("roost.skill/v2/presentation-program", payload)
}
