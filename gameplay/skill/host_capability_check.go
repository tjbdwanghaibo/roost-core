package skill

import (
	"errors"
	"fmt"
)

// HostCapabilityProbe 是 CheckHostCapabilities 用的探针世界。检查会真的调用 Host：读属性 /
// 资源、付零费、施加中性资源/属性修正、步进并停止一个探针衍生物，
// 所以要在测试世界上跑，不要对线上世界跑。
type HostCapabilityProbe struct {
	// Entity：一个存活的实体，带声明的全部属性与资源（零值即可）。
	Entity EntityID
	// SpawnID：探针衍生物的 ID，不能与世界里已有的衍生物冲突；0 时用 1<<40。
	SpawnID SpawnID
}

// CheckHostCapabilities 是 Host 能力表的一致性检查（B3 ③）：按 Host 自己声明的表
// （Host.HostCapabilities）逐项调用 Host，确认声明的每一项都真的支持；属性与资源两列还反过来
// 核对表外的 key 被拒绝，而不是静默当成 0。catalog 用来把 key 换成 handle、核对属性量纲。
//
// 业务 Host 在自己的测试里调用一次即可（参考 skill/host_capability_promises_test.go 与
// combatcomponent 的同名测试）。返回的错误列出全部不一致项。
func CheckHostCapabilities(host Host, catalog GameplayCatalog, probe HostCapabilityProbe) error {
	if host == nil {
		return fmt.Errorf("%w: no host", ErrHostContractViolation)
	}
	if probe.SpawnID == 0 {
		probe.SpawnID = 1 << 40
	}
	table := host.HostCapabilities()
	checker := hostCapabilityChecker{host: host, catalog: catalog, table: table, probe: probe}
	for _, column := range hostCapabilityColumns {
		checker.column(column)
	}
	checker.cleanupSpawnProbe()
	return errors.Join(checker.failures...)
}

type hostCapabilityChecker struct {
	host         Host
	catalog      GameplayCatalog
	table        HostCapabilityTable
	probe        HostCapabilityProbe
	failures     []error
	probeStarted bool
	probeState   SpawnHostState
}

func (checker *hostCapabilityChecker) fail(capability HostCapability, format string, args ...any) {
	checker.failures = append(checker.failures, fmt.Errorf("host capability %s: %s", capability, fmt.Sprintf(format, args...)))
}

func (checker *hostCapabilityChecker) meta() QueryMeta {
	return QueryMeta{RequiredRevision: checker.host.CurrentRevision()}
}

func (checker *hostCapabilityChecker) commandMeta() CommandMeta {
	return CommandMeta{RequiredRevision: checker.host.CurrentRevision()}
}

// column 检查表的一列：声明的每一项调用一次 Host；attribute / resource 再核对表外被拒绝。
func (checker *hostCapabilityChecker) column(column hostCapabilityColumn) {
	if column.keys == nil {
		if checker.table.Summon {
			checker.summon()
		}
		return
	}
	declared := *column.keys(&checker.table)
	for _, key := range declared {
		capability := HostCapability{Kind: column.kind, Key: key}
		switch column.kind {
		case HostCapabilityAttribute:
			checker.attribute(capability, true)
		case HostCapabilityResource:
			checker.resource(capability, true)
		case HostCapabilitySpawnKind:
			checker.spawnKind(capability)
		case HostCapabilityMotionStep:
			checker.motionStep(capability)
		case HostCapabilitySpawnNumericField:
			checker.spawnNumericField(capability)
		case HostCapabilityResourceOperation:
			checker.resourceOperation(capability)
		case HostCapabilityModifierOperation:
			checker.modifierOperation(capability)
		default:
			checker.fail(capability, "column has no conformance probe")
		}
	}
	switch column.kind {
	case HostCapabilityAttribute:
		for _, entry := range checker.catalog.Attributes.Entries {
			if !containsString(declared, entry.Key) {
				checker.attribute(HostCapability{Kind: column.kind, Key: entry.Key}, false)
			}
		}
		checker.undeclaredAttributeHandle()
	case HostCapabilityResource:
		for _, entry := range checker.catalog.Resources.Entries {
			if !containsString(declared, entry.Key) {
				checker.resource(HostCapability{Kind: column.kind, Key: entry.Key}, false)
			}
		}
		checker.resource(HostCapability{Kind: column.kind, Key: "\x00undeclared"}, false)
	}
}

func (checker *hostCapabilityChecker) attributeEntry(key string) (AttributeCatalogEntry, bool) {
	for _, entry := range checker.catalog.Attributes.Entries {
		if entry.Key == key {
			return entry, true
		}
	}
	return AttributeCatalogEntry{}, false
}

func (checker *hostCapabilityChecker) resourceEntry(key string) (ResourceCatalogEntry, bool) {
	for _, entry := range checker.catalog.Resources.Entries {
		if entry.Key == key {
			return entry, true
		}
	}
	return ResourceCatalogEntry{}, false
}

// attribute：声明的属性要能读，值是 catalog 声明的量纲；没声明的要被拒绝。
func (checker *hostCapabilityChecker) attribute(capability HostCapability, declared bool) {
	entry, found := checker.attributeEntry(capability.Key)
	if !found {
		checker.fail(capability, "declared attribute is not in the gameplay catalog")
		return
	}
	read, err := checker.host.Read(ReadRequest{Meta: checker.meta(), Payload: AttributeRead{Entity: checker.probe.Entity, Attribute: entry.Handle}})
	switch {
	case declared && err != nil:
		checker.fail(capability, "declared but Read(AttributeRead) failed: %v", err)
	case declared && (read.Value.Type().Base != valueKindInt || read.Value.Type().Quantity != entry.Quantity):
		checker.fail(capability, "declared but Read(AttributeRead) returned %+v, want an int of the catalog quantity", read.Value.Type())
	case !declared && err == nil:
		checker.fail(capability, "not declared but Read(AttributeRead) answered %v instead of refusing", read.Value)
	}
}

// undeclaredAttributeHandle：catalog 外的 handle 一律被拒绝（以前两个参考 Host 都返回 0）。
func (checker *hostCapabilityChecker) undeclaredAttributeHandle() {
	var handle AttributeHandle = 1
	for _, entry := range checker.catalog.Attributes.Entries {
		if entry.Handle >= handle {
			handle = entry.Handle + 1
		}
	}
	capability := HostCapability{Kind: HostCapabilityAttribute, Key: fmt.Sprintf("<handle %d>", handle)}
	if read, err := checker.host.Read(ReadRequest{Meta: checker.meta(), Payload: AttributeRead{Entity: checker.probe.Entity, Attribute: handle}}); err == nil {
		checker.fail(capability, "outside the catalog but Read(AttributeRead) answered %v instead of refusing", read.Value)
	}
}

// resource：声明的资源要能读、能付零费；没声明的两者都要被拒绝。
func (checker *hostCapabilityChecker) resource(capability HostCapability, declared bool) {
	entry, _ := checker.resourceEntry(capability.Key)
	_, readErr := checker.host.Read(ReadRequest{Meta: checker.meta(), Payload: ResourceRead{Entity: checker.probe.Entity, Resource: capability.Key}})
	// 与 Runtime 的付费形状一致：只发编译后的 Handle，不能让名字替缺失实现兜底。
	_, payErr := checker.host.PayCosts(CostPayment{Meta: checker.commandMeta(), Entity: checker.probe.Entity, Entries: []CostEntry{{Handle: entry.Handle, Amount: 0}}})
	switch {
	case declared && readErr != nil:
		checker.fail(capability, "declared but Read(ResourceRead) failed: %v", readErr)
	case declared && payErr != nil:
		checker.fail(capability, "declared but PayCosts of zero failed: %v", payErr)
	case !declared && readErr == nil:
		checker.fail(capability, "not declared but Read(ResourceRead) answered instead of refusing")
	case !declared && payErr == nil:
		checker.fail(capability, "not declared but PayCosts accepted it instead of refusing")
	}
}

// resourceOperation：对第一个声明的资源施加不改变余额的这种 operation。
func (checker *hostCapabilityChecker) resourceOperation(capability HostCapability) {
	if len(checker.table.Resources) == 0 {
		checker.fail(capability, "declared without any declared resource to apply it to")
		return
	}
	key := checker.table.Resources[0]
	entry, _ := checker.resourceEntry(key)
	amount := int64(0)
	if capability.Key == "set" {
		read, err := checker.host.Read(ReadRequest{Meta: checker.meta(), Payload: ResourceRead{Entity: checker.probe.Entity, Resource: key}})
		if err != nil {
			checker.fail(capability, "cannot read resource %q to keep it unchanged: %v", key, err)
			return
		}
		amount, _ = read.Value.Int()
	}
	command := EffectCommand{Meta: checker.commandMeta(), Payload: ResourceCommand{Target: checker.probe.Entity, Resource: entry.Handle, Operation: capability.Key, Amount: amount}}
	if _, err := checker.host.Apply(command); err != nil {
		checker.fail(capability, "declared but Apply(ResourceCommand) failed: %v", err)
	}
}

// modifierOperation：加法 0、乘法 10000 BP 才是中性修正，乘法 0 会清空属性。
func (checker *hostCapabilityChecker) modifierOperation(capability HostCapability) {
	var target AttributeCatalogEntry
	for _, entry := range checker.catalog.Attributes.Entries {
		if containsString(entry.ModifierOperations, capability.Key) {
			target = entry
			break
		}
	}
	if target.Handle == 0 {
		checker.fail(capability, "no catalog attribute allows this modifier operation")
		return
	}
	value := int64(0)
	if capability.Key == "mul_bp" {
		value = 10000
	}
	command := EffectCommand{Meta: checker.commandMeta(), Payload: AttributeModifierCommand{SourceOwner: checker.probe.Entity, Target: checker.probe.Entity, Attribute: target.Handle, Operation: capability.Key, Value: value, DurationTicks: 1}}
	if _, err := checker.host.Apply(command); err != nil {
		checker.fail(capability, "declared but Apply(AttributeModifierCommand) failed: %v", err)
	}
}

// step 的探针归 checker 所有，不进入 Runtime 的业务衍生物状态机；检查结束自行清理。
func (checker *hostCapabilityChecker) step(step MotionStep, numeric SpawnNumericSnapshot) error {
	meta := SpawnCommandMeta{RequiredRevision: checker.host.CurrentRevision(), SpawnID: checker.probe.SpawnID}
	if !checker.probeStarted {
		checker.probeState.SpawnID = checker.probe.SpawnID
	}
	checker.probeStarted = true
	result, err := checker.host.StepSpawn(SpawnStepCommand{Meta: meta, Motion: step, Numeric: numeric}, checker.probeState)
	if err == nil {
		checker.probeState = result.State
	}
	return err
}

func (checker *hostCapabilityChecker) cleanupSpawnProbe() {
	if !checker.probeStarted {
		return
	}
	_, err := checker.host.StopSpawn(SpawnStopCommand{Meta: SpawnCommandMeta{RequiredRevision: checker.host.CurrentRevision(), SpawnID: checker.probe.SpawnID}}, checker.probeState)
	if err != nil {
		checker.fail(HostCapability{Kind: HostCapabilitySpawnKind, Key: "probe"}, "cleanup StopSpawn failed: %v", err)
	}
}

// spawnKind：Host 的 StepSpawn 不带 kind，非 minion 的 kind 只能核对基本步骤（static、
// trajectory、signals）；minion 要求同时声明并实现召唤物。
func (checker *hostCapabilityChecker) spawnKind(capability HostCapability) {
	if capability.Key == "minion" {
		if !checker.table.Summon {
			checker.fail(capability, "minion spawns require the summon capability")
		}
		return
	}
	for _, step := range []MotionStep{StaticMotionStep{}, TrajectoryMotionStep{}, SignalsMotionStep{}} {
		if err := checker.step(step, SpawnNumericSnapshot{}); err != nil {
			checker.fail(capability, "StepSpawn(%T) failed: %v", step, err)
		}
	}
}

func (checker *hostCapabilityChecker) motionStep(capability HostCapability) {
	steps := map[string]MotionStep{
		"frame": FrameMotionStep{}, "steering": SteeringMotionStep{}, "offsets": OffsetsMotionStep{},
		"collision": CollisionMotionStep{}, "carry": CarryMotionStep{}, "completion": CompletionMotionStep{},
	}
	step, found := steps[capability.Key]
	if !found {
		checker.fail(capability, "motion step has no conformance probe")
		return
	}
	if err := checker.step(step, SpawnNumericSnapshot{}); err != nil {
		checker.fail(capability, "declared but StepSpawn(%T) failed: %v", step, err)
	}
}

func (checker *hostCapabilityChecker) spawnNumericField(capability HostCapability) {
	var numeric SpawnNumericSnapshot
	switch capability.Key {
	case "turn_rate_mdeg_per_tick":
		numeric.TurnRateMDegPerTick = 1
	case "return_speed_bp":
		numeric.ReturnSpeedBP = 1
	case "collision_force":
		numeric.CollisionForce = 1
	default:
		checker.fail(capability, "spawn numeric field has no conformance probe")
		return
	}
	if err := checker.step(FrameMotionStep{}, numeric); err != nil {
		checker.fail(capability, "declared but StepSpawn with the field set failed: %v", err)
	}
}

func (checker *hostCapabilityChecker) summon() {
	capability := HostCapability{Kind: HostCapabilitySummon}
	owned, ok := checker.host.(OwnedEntityRuntimeHost)
	if !ok {
		checker.fail(capability, "declared but %T does not implement OwnedEntityRuntimeHost", checker.host)
		return
	}
	if _, alive := owned.OwnedEntity(0); alive {
		checker.fail(capability, "OwnedEntity(0) reported a live owned entity")
	}
}
