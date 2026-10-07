package skill

// runHostCapabilityCheck 收集定义在运行期会向 Host 要的每一项能力（B3 ③），逐项对照环境的
// Host 能力表：表外的项报 HOST_CAPABILITY_MISSING 并点名；全部需求排序去重后交给 lower 写进
// Program（hostRequirements），Runtime 用同一份列表核对 Host 声明的表。
// 表只覆盖这些可配置列；基础 Apply/StateStore 的 damage/heal/shield/status/temporal/state
// 命令仍是 Host 接口契约，需业务 Host 的行为集成测试，不会被这张表逐命令自检。
//
// 编译器对 Host 能力的判断只经 HostCapabilityTableOf 读取（守卫
// TestCompilerReadsHostCapabilitiesOnlyThroughTheTable）；这里收集的项必须覆盖 Runtime 实际
// 发给 Host 的全部取值（守卫 TestRuntimeAsksHostOnlyForCompiledRequirements）。
func runHostCapabilityCheck(context *compileContext) {
	ir := context.artifacts.ir
	if ir == nil {
		return
	}
	requirements := collectHostRequirements(ir)
	table := HostCapabilityTableOf(context.environment)
	for _, requirement := range requirements {
		if !table.Has(requirement.capability) {
			context.addDiagnostic(DiagnosticHostCapabilityMissing, requirement.path, "environment host capability table lacks "+requirement.capability.String())
		}
	}
	context.artifacts.hostRequirements = sortedHostCapabilities(requirements)
}

func collectHostRequirements(ir *skillIR) []hostRequirement {
	var requirements []hostRequirement
	need := func(kind HostCapabilityKind, key, path string) {
		requirements = append(requirements, hostRequirement{capability: HostCapability{Kind: kind, Key: key}, path: path})
	}
	// 属性读取：read_attribute（含快照与 cast window 表达式），Host.Read(AttributeRead)。
	ir.walkValues(func(value valueIR) {
		if read, ok := value.(*attributeReadValueIR); ok {
			need(HostCapabilityAttribute, read.attribute, read.source.Path+".read_attribute.attribute")
		}
	})
	// 资源：cost 与 sustain cost 经 Host.PayCosts。
	for _, cost := range ir.costs {
		need(HostCapabilityResource, cost.resource, cost.source.Path+".resource")
	}
	for _, cost := range ir.activation.policy.sustainCosts {
		need(HostCapabilityResource, cost.resource, cost.source.Path+".resource")
	}
	selectPlan := func(plan *selectIR) {
		if _, owned := plan.shape.(*ownedEntitiesShapeIR); owned {
			need(HostCapabilitySummon, "", plan.source.Path+".shape")
		}
		for _, filter := range plan.filters {
			// attribute_compare 由 Host 在 Select 里读属性比较。
			if compare, ok := filter.(*attributeCompareFilterIR); ok {
				need(HostCapabilityAttribute, compare.attribute, compare.source.Path+".attribute")
			}
		}
	}
	ir.walkFlows(func(flow flowIR) {
		switch typed := flow.(type) {
		case *selectFlowIR:
			selectPlan(&typed.selectPlan)
		case *effectFlowIR:
			collectEffectHostRequirements(typed.effect, need)
			if typed.spawn != nil {
				collectSpawnHostRequirements(typed.spawn, need, selectPlan)
			}
		}
	})
	return requirements
}

func collectEffectHostRequirements(effect effectIR, need func(HostCapabilityKind, string, string)) {
	switch typed := effect.(type) {
	case *resourceEffectIR:
		need(HostCapabilityResource, typed.resource, typed.source.Path+".resource")
		need(HostCapabilityResourceOperation, typed.operation, typed.source.Path+".operation")
	case *attributeModifierEffectIR:
		need(HostCapabilityModifierOperation, typed.operation, typed.source.Path+".operation")
	case *summonEffectIR:
		need(HostCapabilitySummon, "", typed.source.Path)
	case *entityCommandEffectIR:
		need(HostCapabilitySummon, "", typed.source.Path)
	case *modifySpawnEffectIR:
		if containsString(hostOnlySpawnNumericFields, typed.property) {
			need(HostCapabilitySpawnNumericField, typed.property, typed.source.Path+".property")
		}
	}
}

func collectSpawnHostRequirements(spawn *spawnIR, need func(HostCapabilityKind, string, string), selectPlan func(*selectIR)) {
	path := spawn.source.Path
	need(HostCapabilitySpawnKind, spawn.kind, path+".kind")
	if spawn.area != nil {
		selectPlan(spawn.area)
	}
	for _, track := range spawn.numericTracks {
		if containsString(hostOnlySpawnNumericFields, track.property) {
			need(HostCapabilitySpawnNumericField, track.property, track.source.Path+".property")
		}
	}
	motion, ok := spawn.motion.(*canonicalMotionIR)
	if !ok {
		return
	}
	// 与 Runtime 发出的 motion 步骤一一对应（stepSpawnMotion，spawn_motion.go）：frame、steering
	// （交出解析后的方向）、offsets（交出加完偏移的位置）、completion 每个运动衍生物每步都发，
	// 不论定义写没写 steering / offsets；collision 与 carry 只在定义写了时发。以前 motion catalog
	// 的 enabled_slots 只在定义写了 steering / offsets 时才查，关掉这两个槽位的环境照样编译出
	// 运动衍生物，Runtime 每步仍发这两种步骤（RR-20261006-37）。
	need(HostCapabilityMotionStep, "frame", path+".motion.frame")
	need(HostCapabilityMotionStep, "steering", path+".motion.steering")
	need(HostCapabilityMotionStep, "offsets", path+".motion.offsets")
	need(HostCapabilityMotionStep, "completion", path+".motion.completion")
	if motion.collision != nil {
		need(HostCapabilityMotionStep, "collision", path+".motion.collision")
	}
	if motion.carry != nil {
		need(HostCapabilityMotionStep, "carry", path+".motion.carry")
	}
}
