package skill

import (
	"errors"
	"fmt"
	"strings"
)

// 求值上下文表（维护者第五轮决定，方案见
// docs/feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md）。
//
// 一个值位点在 Runtime 里总是在某个“求值上下文”里求值：施法流程、memory 默认值、三个缓存型
// 快照的采样点、spawn 进程的每一步、进程回调、持久状态默认值。各上下文能读到的引用不同
// （移交后的进程没有施法的输入 / memory / 局部变量，施法流程没有 `$owner` / `$event.*`，
// 采样点上还没有流程局部变量……）。此前编译期只有一套作用域（施法作用域 + 回调作用域），
// 其余规则按 pass 各写一份（NC-211 / NC-220 / NC-221 / NC-223 / NC-224），每加一个上下文
// 或字段就要有人记得在编译器里补一条，第五批之前已经出了五次问题。
//
// 现在这一张表是唯一来源：
//   - 编译期：typeChecker 按位点所在的上下文从表里生成作用域（scopeFor），引用不在作用域里时
//     诊断指向表项；缓存型快照点能不能在读取处用、它的实体能不能在采样点求出，也查这张表
//     （evalSnapshotTable / checkCachedRead）。
//   - lower：引用的行号随 referenceProgramValue 带到 Program（不进 digest）。
//   - Runtime：castInstance.evalContext 记着当前上下文，evalReference 先查表，表外引用返回
//     ErrReferenceOutOfContext（指向表项），不再落到 ErrProgramInvariant。
//
// 改表的规则：加一行或一个上下文，必须同时在 eval_contexts_table_test.go 里给每个格子补
// 正例 / 反例（或写明“没有该类型的位点”），守卫测试会逐格检查。
//
// 格子只有两种结论：可用（值就是 semantics 写的那样，任何时刻都成立）或不可用（编译期拒绝）。
// 第五批 O33 记下的“可用但值漂移”格子——进程字段 / 状态默认值里的 `$primary_target`、
// `$ability.self`、`$cast.*`（`$cast.mode` 除外）与 cast_start / phase_start 读取，以及 memory
// 默认值里的 phase_start——维护者第七轮决定全部改为编译期拒绝（方案 §8）。不可用格的 semantics
// 写明原因，能替代的写“改用 …”，诊断原样带出。

// evalContext 是 Runtime 求值时所处的上下文。零值是施法流程：普通 castInstance 不需要设置。
type evalContext uint8

const (
	// evalCastFlow：施法流程。phase 事件流程与 effect result 分支、costs / sustain costs、
	// windup / recovery 表达式、spawn 的位置 / 属性覆盖 / 参数，以及 spawn 进程的 numeric
	// track 初值与绑定到进程数值属性的 motion 字段（进程启动时用施法求一次）。
	evalCastFlow evalContext = iota
	// evalMemoryDefault：memory 默认值。Activate 按槽位顺序求值，早于 cast_start 采样。
	evalMemoryDefault
	// evalCastStartCapture：cast_start 读取的实体，在 cast_start 采样点（memory 初始化之后、
	// 任何流程之前）求值，之后读缓存。
	evalCastStartCapture
	// evalPhaseStartCapture：phase_start 读取的实体，在每次进入 phase、执行 enter 之前求值。
	evalPhaseStartCapture
	// evalProcessStartCapture：process_start 读取的实体，在 spawn 进程启动时、脱离施法的进程
	// 上下文里求值（captureOwnedProcessSnapshots）。
	evalProcessStartCapture
	// evalProcessStep：spawn 进程每一步重新求值的字段（area 选择、follow / tracking / carry
	// 目标、path 点、orbit 锚点、parabola 目的地、未绑定到进程数值属性的数值字段）。启动那一步
	// 用施法本身求值，之后每一步用移交后的进程（detachedProcessCast）求值，表里的引用必须在
	// 两者里都能求出。
	evalProcessStep
	// evalProcessCallback：spawn 的 `on.*` 回调。启动时与移交后都在进程上下文里执行。
	evalProcessCallback
	// evalStateDefault：持久状态的默认值。在读 / 写这条状态的位置求值，那个位置可能是上面
	// 任何一个上下文，所以只能用在所有上下文里都求得出的引用。
	evalStateDefault
	evalContextCount
)

var evalContextNames = [evalContextCount]string{
	evalCastFlow:            "cast_flow",
	evalMemoryDefault:       "memory_default",
	evalCastStartCapture:    "cast_start",
	evalPhaseStartCapture:   "phase_start",
	evalProcessStartCapture: "process_start",
	evalProcessStep:         "process_step",
	evalProcessCallback:     "process_callback",
	evalStateDefault:        "state_default",
}

func (context evalContext) String() string {
	if context < evalContextCount {
		return evalContextNames[context]
	}
	return fmt.Sprintf("eval_context(%d)", uint8(context))
}

// isCapture 报告上下文是不是缓存型快照的采样点：那里只求读取的实体。
func (context evalContext) isCapture() bool {
	return context == evalCastStartCapture || context == evalPhaseStartCapture || context == evalProcessStartCapture
}

// evalAvailability 是一个格子的结论。
type evalAvailability uint8

const (
	// evalUnavailable：编译期拒绝，Runtime 遇到返回 ErrReferenceOutOfContext。
	evalUnavailable evalAvailability = iota
	// evalAvailable：可用，值就是 semantics 写的那样。
	evalAvailable
)

type evalCell struct {
	availability evalAvailability
	semantics    string
}

func evalAvail(semantics string) evalCell {
	return evalCell{availability: evalAvailable, semantics: semantics}
}
func evalNone(reason string) evalCell {
	return evalCell{availability: evalUnavailable, semantics: reason}
}

func (cell evalCell) usable() bool { return cell.availability == evalAvailable }

// evalReferenceRow 是表的一行：一个引用（或一类引用）在每个上下文里的结论。
type evalReferenceRow struct {
	// name 是表里显示的名字。builtin 行是精确名字（`$caster`），也是投射的根
	// （`$event.target.position` 归到 `$event.target`）；prefix 行以 `.*` 结尾。
	name   string
	prefix bool
	// typ 是 builtin 行的类型；prefix 行的类型由输入布局、memory 声明或流程决定。
	typ valueType
	// castModes 非空时，这一行只在这些施法模式下存在（`$cast.charge_bp` 等）。
	castModes []castMode
	// areaOnly：只在 area 进程的回调里存在。
	areaOnly bool
	// entity：值可以是实体。采样点只求读取的实体，非实体行在采样列里没有位点。
	entity bool
	cells  [evalContextCount]evalCell
}

type evalReferenceRowIndex uint8

// evalRowUnknown 表示引用不在表里（类型检查已经拒绝；Runtime 遇到同样报表外引用）。
const evalRowUnknown evalReferenceRowIndex = 0xff

const (
	// evalRowUnset 是 referenceProgramValue.row 的零值：手写的 Program 值没有经过 lower，
	// Runtime 按 builtin 名字现查（evalReferenceRowOf）。表的 0 号位置留空。
	evalRowUnset evalReferenceRowIndex = iota
	evalRowInput
	evalRowMemory
	evalRowLocal
	evalRowCaster
	evalRowCasterPosition
	evalRowPrimaryTarget
	evalRowAbilitySelf
	evalRowCastMode
	evalRowCastElapsedTicks
	evalRowCastChargeBP
	evalRowCastReleaseReason
	evalRowCastPulseIndex
	evalRowCastStock
	evalRowCastMaxStock
	evalRowOwner
	evalRowOwnerPosition
	evalRowLifecycleEntity
	evalRowProcess
	evalRowEventSource
	evalRowEventOwner
	evalRowEventTarget
	evalRowEventTick
	evalRowEventMembershipTicks
	evalRowEventEnterCount
	evalRowCount
)

var (
	evalEntityType   = valueType{Base: valueKindEntity}
	evalPositionType = valueType{Base: valueKindPosition}
)

// 施法流程以外、仍然是“施法时刻”的格子共用的说明。
const (
	evalNoLocalsYet       = "采样点上还没有流程局部变量（RR-20261005-NC-220）"
	evalNoCastInProcess   = "移交后的进程没有施法的输入、memory 与局部变量（RR-20261005-NC-224）"
	evalCallbackOnly      = "只在 spawn 进程的回调与 process_start 采样里有"
	evalNotInMemoryInit   = "memory 默认值按槽位顺序求值，读另一个 memory 会读到未初始化的槽位（RR-20261005-NC-280）"
	evalNotInStateDefault = "状态默认值在读 / 写处求值，那里可能是进程回调或进程字段，没有施法的输入与 memory（RR-20261005-NC-281）"
	// 以下是 O33 收紧的格子（维护者第七轮决定）：此前编译通过、值随进程移交或求值位置漂移。
	evalCastStateInStep = "进程字段每一步重新求值，移交后的进程没有施法状态（此前移交后是零值重算，O33 编译期拒绝）；" +
		"改用 numeric track 的初值（进程启动时用施法求一次），随时间变化用回调里 modify_process 的 over_ticks"
	evalCastStateInState = "状态默认值也在进程回调 / 进程字段里求值，那里没有施法状态（此前是零值重算，O33 编译期拒绝）；" +
		evalWriteStateInCast
	evalCastSnapshotInStep = "进程字段每一步重新求值，移交后的进程没有施法的快照（此前移交后退化为 current，O33 编译期拒绝）；" +
		"改用 numeric track 的初值（进程启动时用施法求一次）；要进程启动时的值，在回调里用 process_start；否则用 current"
	evalCastSnapshotInState = "状态默认值也在进程回调 / 进程字段里求值，那里没有施法的快照（此前退化为 current，O33 编译期拒绝）；" +
		"改用 current，或在施法流程里用 modify_state 把同一个表达式的值写入（默认值用字面量）"
	// evalWriteStateInCast：状态默认值的表达式类型就是状态的类型，所以同一个表达式总能在施法流程里
	// 求值后用 modify_state 写入。
	evalWriteStateInCast = "改用：在施法流程里用 modify_state 把同一个表达式的值写入（默认值用字面量）"
)

// evalReferenceTable 是求值上下文表本身。行的顺序就是 evalReferenceRowIndex。
var evalReferenceTable = [evalRowCount]evalReferenceRow{
	evalRowInput: {name: "$input.*", prefix: true, entity: true, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法输入（target_changed / direction_changed 会更新对应槽位）"),
		evalMemoryDefault:       evalAvail("施法输入"),
		evalCastStartCapture:    evalAvail("施法输入"),
		evalPhaseStartCapture:   evalAvail("进入 phase 时的施法输入"),
		evalProcessStartCapture: evalNone(evalNoCastInProcess),
		evalProcessStep:         evalNone(evalNoCastInProcess),
		evalProcessCallback:     evalNone(evalNoCastInProcess),
		evalStateDefault:        evalNone(evalNotInStateDefault),
	}},
	evalRowMemory: {name: "$memory.*", prefix: true, entity: true, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法 memory 的当前值"),
		evalMemoryDefault:       evalNone(evalNotInMemoryInit),
		evalCastStartCapture:    evalAvail("memory 的默认值（采样早于任何流程）；可缺省的 memory 不行（RR-20261005-NC-282）"),
		evalPhaseStartCapture:   evalAvail("进入 phase 时 memory 的值；可缺省的 memory 不行（RR-20261005-NC-282）"),
		evalProcessStartCapture: evalNone(evalNoCastInProcess),
		evalProcessStep:         evalNone(evalNoCastInProcess),
		evalProcessCallback:     evalNone(evalNoCastInProcess),
		evalStateDefault:        evalNone(evalNotInStateDefault),
	}},
	evalRowLocal: {name: "$local.*", prefix: true, entity: true, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("作用域内的 select / repeat / effect result 局部变量"),
		evalMemoryDefault:       evalNone("施法启动时还没有流程局部变量"),
		evalCastStartCapture:    evalNone(evalNoLocalsYet),
		evalPhaseStartCapture:   evalNone(evalNoLocalsYet),
		evalProcessStartCapture: evalNone(evalNoLocalsYet),
		evalProcessStep:         evalNone(evalNoCastInProcess),
		evalProcessCallback:     evalAvail("回调自己的 select / repeat / effect result 局部变量（不能捕获施法的局部变量）"),
		evalStateDefault:        evalNone("状态默认值没有流程局部变量"),
	}},
	evalRowCaster: {name: "$caster", typ: evalEntityType, entity: true, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法者"),
		evalMemoryDefault:       evalAvail("施法者"),
		evalCastStartCapture:    evalAvail("施法者"),
		evalPhaseStartCapture:   evalAvail("施法者"),
		evalProcessStartCapture: evalNone("进程上下文里用 $owner"),
		evalProcessStep:         evalAvail("施法者（移交后是进程的 owner，即同一个施法者）"),
		evalProcessCallback:     evalNone("进程回调里统一用 $owner。Runtime 其实求得出（= 进程的 owner，即同一个施法者），编译期按表拒绝（O36），改用 $owner"),
		evalStateDefault:        evalAvail("读写这条状态的施法者；在进程里是进程的 owner"),
	}},
	evalRowCasterPosition: {name: "$caster.position", typ: evalPositionType, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法者求值时刻的位置"),
		evalMemoryDefault:       evalAvail("施法者 Activate 时的位置"),
		evalCastStartCapture:    evalAvail("（采样点只求实体）"),
		evalPhaseStartCapture:   evalAvail("（采样点只求实体）"),
		evalProcessStartCapture: evalNone("进程上下文里用 $owner.position"),
		evalProcessStep:         evalAvail("施法者每一步时的位置"),
		evalProcessCallback:     evalNone("进程回调里统一用 $owner（O36），改用 $owner.position"),
		evalStateDefault:        evalAvail("读写这条状态的施法者（进程里是 owner）的位置"),
	}},
	evalRowPrimaryTarget: {name: "$primary_target", typ: valueType{Base: valueKindEntity, Optional: true}, entity: true, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法的主目标（可缺省；target_changed 会更新）"),
		evalMemoryDefault:       evalAvail("Activate 时的主目标"),
		evalCastStartCapture:    evalNone("可缺省：采样时没有 exists 守卫，缺省即失败（RR-20261005-NC-282）"),
		evalPhaseStartCapture:   evalNone("可缺省：采样时没有 exists 守卫，缺省即失败（RR-20261005-NC-282）"),
		evalProcessStartCapture: evalNone(evalCallbackOnly),
		evalProcessStep: evalNone("进程字段每一步重新求值，移交后的进程没有施法的主目标（此前移交后漂移成 lifecycle 实体，O33 编译期拒绝）；" +
			"改用施法流程里求一次的 spawn position（如 $input.target.position）把 lifecycle 实体放到目标处，" +
			"或在回调里用 $lifecycle_entity / $event.target（如 on.tick 里 select from $lifecycle_entity 代替 area 选择）"),
		evalProcessCallback: evalNone("进程回调里没有施法的主目标，改用 $lifecycle_entity / $event.target"),
		evalStateDefault: evalNone("状态默认值也在进程回调 / 进程字段里求值，那里没有施法的主目标（此前漂移成 lifecycle 实体，O33 编译期拒绝）；" +
			"改用 $caster 作默认值，在施法流程里用 modify_state 写入主目标"),
	}},
	evalRowAbilitySelf: {name: "$ability.self", typ: valueType{Base: valueKindAbility}, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法者身上的本技能"),
		evalMemoryDefault:       evalAvail("施法者身上的本技能"),
		evalCastStartCapture:    evalAvail("（采样点只求实体）"),
		evalPhaseStartCapture:   evalAvail("（采样点只求实体）"),
		evalProcessStartCapture: evalNone(evalCallbackOnly),
		evalProcessStep: evalNone("进程字段每一步重新求值，移交后的进程没有技能句柄（此前移交后 handle 为 0，O33 编译期拒绝）；" +
			"改用在施法流程里读写技能状态"),
		evalProcessCallback: evalNone("进程回调里没有技能句柄（技能选择的 self_ability / not_self_ability 过滤在这里比较的是 handle 0，O35）；" +
			"改用在施法流程里读写技能状态"),
		evalStateDefault: evalNone("状态默认值也在进程回调 / 进程字段里求值，那里没有技能句柄（此前 handle 为 0，O33 编译期拒绝）；" +
			evalWriteStateInCast),
	}},
	evalRowCastMode: castStateRow("$cast.mode", valueType{Base: valueKindString}, nil, evalAvail("施法模式（Program 常量，任何时刻相同）"), evalAvail("施法模式（Program 常量）")),
	evalRowCastElapsedTicks: castStateRow("$cast.elapsed_ticks", quantityType(quantityTicks), nil,
		evalNone("进程字段每一步重新求值，移交后的进程没有施法计时（此前移交后从 tick 0 算、即当前 tick，O33 编译期拒绝）；"+
			"改用进程自己的计时：numeric track 加回调里 modify_process 的 over_ticks，或回调里的 $event.tick / $event.membership_ticks"),
		evalNone("状态默认值也在进程回调 / 进程字段里求值，那里没有施法计时（此前在进程里是当前 tick，O33 编译期拒绝）；"+evalWriteStateInCast)),
	evalRowCastChargeBP:         castStateRow("$cast.charge_bp", quantityType(quantityBasisPoints), []castMode{castModeCharge}, evalNone(evalCastStateInStep), evalNone(evalCastStateInState)),
	evalRowCastReleaseReason:    castStateRow("$cast.release_reason", valueType{Base: valueKindString}, []castMode{castModeCharge}, evalNone(evalCastStateInStep), evalNone(evalCastStateInState)),
	evalRowCastPulseIndex:       castStateRow("$cast.pulse_index", quantityType(quantityCount), []castMode{castModeHold, castModeToggle}, evalNone(evalCastStateInStep), evalNone(evalCastStateInState)),
	evalRowCastStock:            castStateRow("$cast.stock", quantityType(quantityCount), []castMode{castModeAmmo}, evalNone(evalCastStateInStep), evalNone(evalCastStateInState)),
	evalRowCastMaxStock:         castStateRow("$cast.max_stock", quantityType(quantityCount), []castMode{castModeAmmo}, evalNone(evalCastStateInStep), evalNone(evalCastStateInState)),
	evalRowOwner:                processRow("$owner", evalEntityType, true, false, evalAvail("进程的 owner（启动它的施法者）"), evalAvail("进程的 owner")),
	evalRowOwnerPosition:        processRow("$owner.position", evalPositionType, false, false, evalAvail("（采样点只求实体）"), evalAvail("owner 求值时刻的位置")),
	evalRowLifecycleEntity:      processRow("$lifecycle_entity", evalEntityType, true, false, evalAvail("spawn 出的 lifecycle 实体"), evalAvail("spawn 出的 lifecycle 实体")),
	evalRowProcess:              processRow("$process", valueType{Base: valueKindProcess}, false, false, evalAvail("（采样点只求实体）"), evalAvail("当前进程（modify_process 只认它）")),
	evalRowEventSource:          processRow("$event.source", evalEntityType, true, false, evalAvail("进程启动事件的 source（O28：不是之后每次回调的事件）"), evalAvail("触发本次回调的事件的 source")),
	evalRowEventOwner:           processRow("$event.owner", evalEntityType, true, false, evalAvail("进程启动事件的 owner（O28）"), evalAvail("触发本次回调的事件的 owner")),
	evalRowEventTarget:          processRow("$event.target", evalEntityType, true, false, evalAvail("进程启动事件的 target，即 lifecycle 实体（O28）"), evalAvail("本次回调的目标（area 成员、命中目标；没有信号目标时是 lifecycle 实体）")),
	evalRowEventTick:            processRow("$event.tick", quantityType(quantityTicks), false, false, evalAvail("（采样点只求实体）"), evalAvail("本次回调的 tick")),
	evalRowEventMembershipTicks: processRow("$event.membership_ticks", quantityType(quantityTicks), false, true, evalAvail("（采样点只求实体）"), evalAvail("area 成员已在区域内的 tick 数（只在 area 进程）")),
	evalRowEventEnterCount:      processRow("$event.enter_count", quantityType(quantityCount), false, true, evalAvail("（采样点只求实体）"), evalAvail("area 成员进入次数（只在 area 进程）")),
}

// castStateRow 生成 `$cast.*` 行：施法流程与施法启动可用；采样点只求实体；进程字段与
// 状态默认值按 step / state 格子；进程回调与 process_start 没有。
func castStateRow(name string, typ valueType, modes []castMode, step, state evalCell) evalReferenceRow {
	return evalReferenceRow{name: name, typ: typ, castModes: modes, cells: [evalContextCount]evalCell{
		evalCastFlow:            evalAvail("施法状态的当前值（castModes 之外的模式里不存在）"),
		evalMemoryDefault:       evalAvail("Activate 时的施法状态"),
		evalCastStartCapture:    evalAvail("（采样点只求实体）"),
		evalPhaseStartCapture:   evalAvail("（采样点只求实体）"),
		evalProcessStartCapture: evalNone(evalCallbackOnly),
		evalProcessStep:         step,
		evalProcessCallback:     evalNone("进程回调里没有施法状态"),
		evalStateDefault:        state,
	}}
}

// processRow 生成只在进程回调（以及 process_start 采样）里存在的行。
func processRow(name string, typ valueType, entity, areaOnly bool, capture, callback evalCell) evalReferenceRow {
	outside := evalNone(evalCallbackOnly)
	return evalReferenceRow{name: name, typ: typ, entity: entity, areaOnly: areaOnly, cells: [evalContextCount]evalCell{
		evalCastFlow:            outside,
		evalMemoryDefault:       outside,
		evalCastStartCapture:    outside,
		evalPhaseStartCapture:   outside,
		evalProcessStartCapture: capture,
		evalProcessStep:         evalNone("进程字段在启动那一步用施法求值，那里没有进程与事件（RR-20261005-NC-224 同一机制）"),
		evalProcessCallback:     callback,
		evalStateDefault:        evalNone(evalNotInStateDefault),
	}}
}

// evalReferenceRowFor 把引用文本映射到表的行。投射（`$event.target.position`、
// `$input.target.position`、`$local.hit.entity`）归到它的根；返回的 field 是根之后的部分
// （精确匹配某个 builtin 行时为空）。
func evalReferenceRowFor(reference string) (evalReferenceRowIndex, string, bool) {
	for index, row := range evalReferenceTable {
		if row.name != "" && row.prefix && strings.HasPrefix(reference, strings.TrimSuffix(row.name, "*")) {
			return evalReferenceRowIndex(index), "", true
		}
	}
	best, bestLength, field := evalRowUnknown, 0, ""
	for index, row := range evalReferenceTable {
		if row.name == "" || row.prefix {
			continue
		}
		if reference == row.name {
			return evalReferenceRowIndex(index), "", true
		}
		if strings.HasPrefix(reference, row.name+".") && len(row.name) > bestLength {
			best, bestLength, field = evalReferenceRowIndex(index), len(row.name), strings.TrimPrefix(reference, row.name+".")
		}
	}
	return best, field, best != evalRowUnknown
}

func (index evalReferenceRowIndex) row() (evalReferenceRow, bool) {
	if index == evalRowUnset || index >= evalRowCount {
		return evalReferenceRow{}, false
	}
	return evalReferenceTable[index], true
}

// presentIn 报告这一行在这个 Program 形状里是否存在（施法模式、area 进程）。
func (row evalReferenceRow) presentIn(mode castMode, processKind string) bool {
	if len(row.castModes) > 0 {
		found := false
		for _, candidate := range row.castModes {
			found = found || candidate == mode
		}
		if !found {
			return false
		}
	}
	return !row.areaOnly || processKind == "area"
}

// ErrReferenceOutOfContext：Runtime 在某个求值上下文里遇到表里不可用（或不在表里）的引用。
// 编译期按同一张表拒绝这类定义，所以它出现就说明编译器漏了位点——看错误里点名的表项，
// 补的是编译器（或表），而不是在 Runtime 加兜底分支。
var ErrReferenceOutOfContext = errors.New("skill: reference is not available in this evaluation context")

// evalContextAllows 是 Runtime 侧的查表：一次数组下标，热路径上不分配。
func evalContextAllows(context evalContext, row evalReferenceRowIndex) bool {
	return context < evalContextCount && row != evalRowUnset && row < evalRowCount && evalReferenceTable[row].cells[context].usable()
}

func referenceOutOfContextError(context evalContext, row evalReferenceRowIndex, reference string) error {
	entry, found := row.row()
	if !found {
		return fmt.Errorf("%w: %q has no row in the evaluation context table (skill/eval_contexts.go)", ErrReferenceOutOfContext, reference)
	}
	return fmt.Errorf("%w: %s in %s (evaluation context table row %s: %s)", ErrReferenceOutOfContext, reference, context, entry.name, entry.cells[context].semantics)
}

// switchEvalContext 切换 cast 的求值上下文并返回原值，调用方用 defer / 显式调用还原。
// 上下文只在一次求值期间有效，不进 checkpoint。
func (cast *castInstance) switchEvalContext(context evalContext) evalContext {
	previous := cast.evalContext
	cast.evalContext = context
	return previous
}

// 缓存型快照点的表：读取写在某个上下文里时，能不能用这个快照点。实体在哪里求值由
// snapshotCaptureContext 给出，实体本身按引用表的采样列检查（checkCachedRead）。
// current / each_tick / on_hit / on_event 在读取处求值，不在这张表里。
var evalSnapshotTable = map[snapshotPoint][evalContextCount]evalCell{
	snapshotCastStart: {
		evalCastFlow:        evalAvail("施法开始时的值"),
		evalMemoryDefault:   evalAvail("施法开始时的值（memory 默认值与采样同一 tick）"),
		evalProcessStep:     evalNone(evalCastSnapshotInStep),
		evalProcessCallback: evalNone("进程回调读不到施法的快照，改用 process_start（本进程启动时的值）或 current"),
		evalStateDefault:    evalNone(evalCastSnapshotInState),
	},
	snapshotPhaseStart: {
		evalCastFlow: evalAvail("最近一次进入的 phase 开始时的值。costs / windup 在进入第一个 phase 之前求值时（非 charge 模式的 Activate；" +
			"refund_before_commit 时 costs 在 commit 那一刻），还没有 phase 开始的值，读到的是求值那一刻的值（O34）；charge 模式在 release 时求值，" +
			"那时已在 phase 里，读到的是当前 phase 开始时的值"),
		evalMemoryDefault: evalNone("memory 初始化早于第一个 phase（也早于 costs 与 windup），这里没有 phase 开始时的值，此前读到的只是 Activate 时的值、" +
			"与 cast_start 相同（O33 编译期拒绝）；改用 cast_start（同一个值），要 phase 开始时的值就在 phase 流程里读 phase_start"),
		evalProcessStep:     evalNone(evalCastSnapshotInStep),
		evalProcessCallback: evalNone("进程回调读不到施法的快照，改用 process_start（本进程启动时的值）或 current"),
		evalStateDefault:    evalNone(evalCastSnapshotInState),
	},
	snapshotProcessStart: {
		evalCastFlow:        evalNone("process_start 只在 owned 进程启动时采样，施法流程里读到的不是进程启动时的值（RR-20261005-NC-220）"),
		evalMemoryDefault:   evalNone("process_start 只在进程回调里有"),
		evalProcessStep:     evalNone("process_start 只在进程回调里有"),
		evalProcessCallback: evalAvail("本进程启动时的值"),
		evalStateDefault:    evalNone("process_start 只在进程回调里有"),
	},
}

// snapshotCaptureContext 返回缓存型快照点的采样上下文。
func snapshotCaptureContext(point snapshotPoint) (evalContext, bool) {
	switch point {
	case snapshotCastStart:
		return evalCastStartCapture, true
	case snapshotPhaseStart:
		return evalPhaseStartCapture, true
	case snapshotProcessStart:
		return evalProcessStartCapture, true
	}
	return 0, false
}
