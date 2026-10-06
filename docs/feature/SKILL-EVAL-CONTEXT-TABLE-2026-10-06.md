# skill 求值上下文表：编译期与 Runtime 共用的“每个上下文能读哪些引用”

2026-10-06，维护者第五轮决定（`docs/review/DECISIONS-PENDING-2026-10-05.md` 末表“skill 求值上下文表”），分支 `revn09f`，基线 `0aeb5ab6`。来由：[N09 第五批方向判断](../review/REVIEW-2026-10-06-n09-batch5.md#6-方向判断)。同批的审查记录：[N09 第六批](../review/REVIEW-2026-10-06-n09-batch6.md)。

## 1. 目标

“编译器认可的写法”与“Runtime 能执行的写法”各自维护，第五批之前已出五次问题（NC-211、NC-220、NC-221、NC-223、NC-224），形态集中在**求值上下文**：编译期只有一套作用域（施法作用域 + 回调作用域），Runtime 至少有五种求值上下文，二者之间没有对照表，每加一个上下文或字段就要有人记得在编译器里补一条规则。

目标：一张表（代码里的单一来源，同 B3 的 `phase_events.go`）写明每种求值上下文能读哪些引用、值是什么；编译期的作用域从表生成，Runtime 求值时查同一张表，表外引用报错点名表项；按 pass 各写一份的检查收拢到表上。

不做（维护者决定）：NC-224 方向 B（进程启动时冻结施法输入）。第五批 O33 的“移交后语义漂移”起初在表里写明语义、保持现状（~ 格子）；维护者第七轮决定改为编译期拒绝，见 §8。

## 2. 表在哪里、怎么用

`skill/eval_contexts.go`：

- `evalContext`：8 个求值上下文（零值是施法流程）。比决定里列的五种多三个——审表时发现 memory 默认值、持久状态默认值是独立的求值点（NC-280 / NC-281），进程回调与进程每一步也要分开（回调有 `$owner` / `$event.*`，进程字段没有）。
- `evalReferenceTable`：每行一个引用（或一类：`$input.*`、`$memory.*`、`$local.*`），每格 `evalAvailable` / `evalUnavailable` 加一句中文语义（“这里的值是什么”或“为什么没有、改用什么”）。第五批起曾有第三种 `evalDrifting`（可用但值漂移），第七轮决定后删除（§8）。投射（`$event.target.position`）归到根所在的行。
- `evalSnapshotTable`：三个缓存型快照点（cast_start / phase_start / process_start）能写在哪个上下文里；读取的实体在采样上下文里求值，按引用表的采样列检查。

使用方：

| 位置 | 做什么 |
| --- | --- |
| `compile_typecheck.go` `scopeFor(context)` | 按表生成作用域（施法模式、area 进程决定 `$cast.*` 与 area 事件字段是否存在）；memory 默认值、状态默认值、phase 流程 / costs / window、进程回调、进程每一步的字段各用自己的上下文 |
| `compile_typecheck.go` `referenceType` | 表里有这一行但该上下文不可用：`INPUT_UNAVAILABLE`，消息点名上下文、表项与原因 |
| `compile_typecheck.go` `checkCachedRead` | 缓存型快照点能否写在读取所在的上下文；实体里的每个引用在采样上下文可用、且不可缺省（`ATTRIBUTE_SNAPSHOT_INVALID`） |
| `lower.go` `lowerReference` | 引用带上表的行号（`referenceProgramValue.row`，不进 digest）；builtin 与输入槽位的投射拆成根 + field |
| `runtime_eval.go` `evalReference` | 先查 `cast.evalContext` 的格子，不可用返回 `ErrReferenceOutOfContext`（点名上下文与表项），不再落到 `ErrProgramInvariant` |
| 求值点 | `captureSnapshots` 切到采样上下文；Activate 的 memory 默认值；`evalStateDefault`；`startEntityProcess` 启动那一步与 `detachedProcessCast(process, evalProcessStep / evalProcessCallback)`；`initializeProcessNumeric` 切回施法流程 |

收拢掉的按 pass 检查：`compile_owned_entity.go` 的 `validateDetachedProcessFields`（NC-224 的前缀黑名单）、`validateDetachedCallbacks` 里的引用 / 局部变量 / cast_start 快照检查（`detachedReferenceAllowed`、`detachedLocalName`）、`compile_snapshot.go` 的 `snapshotCapturableWhereRead` / `readsInsideProcessCallbacks`（NC-220）。回调不能 finish / goto / wait / 递归建进程 / 改 memory 这些控制流规则不是引用可用性，留在 owned entity pass。

## 3. 表（由 `evalReferenceTable` 生成；✓ 可用，✗ 不可用；第七轮之前的 ~ 漂移格子现为 ✗，见 §8）

| 引用 | cast_flow | memory_default | cast_start | phase_start | process_start | process_step | process_callback | state_default |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `$input.*` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$memory.*` | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$local.*` | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ |
| `$caster` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$caster.position` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$primary_target` | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| `$ability.self` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.mode` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$cast.elapsed_ticks` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.charge_bp` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.release_reason` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.pulse_index` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.stock` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$cast.max_stock` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$owner` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$owner.position` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$lifecycle_entity` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$process` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.source` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.owner` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.target` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.tick` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.membership_ticks` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| `$event.enter_count` | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ |
| 快照 `cast_start` | ✓ | ✓ | — | — | — | ✗ | ✗ | ✗ |
| 快照 `phase_start` | ✓ | ✗ | — | — | — | ✗ | ✗ | ✗ |
| 快照 `process_start` | ✗ | ✗ | — | — | — | ✗ | ✓ | ✗ |

每格的语义与理由见代码里的 semantics 字符串。第七轮之前的 ~ 格子（第五批 O33，记录原语义；现在全部编译期拒绝，§8）：

- 进程每一步（process_step）：`$primary_target` 启动那一步是施法的主目标、移交后是 lifecycle 实体；`$cast.elapsed_ticks` 移交后从 tick 0 算（即当前 tick）；其余 `$cast.*` 移交后是零值重算；`$ability.self` 移交后 handle 为 0；进程字段里的 cast_start / phase_start 读取移交后退化为 current。
- 状态默认值（state_default）：同一默认值在施法里与在进程回调 / 进程字段里读写时取值不同（同上几项）。
- memory 默认值里的 phase_start 读到的是 Activate 时的值（memory 初始化早于第一个 phase）。

第五批时这些格子保持现状（编译通过、Runtime 不失败）。第七轮维护者决定收紧：格子改成 `evalNone`、守卫补反例，没有改别处（§8）。

## 4. 兼容

- 今天能编译的定义：除下面四类新拒绝 / 新放行外，接受集合不变；gameplay digest 不变（`row` 不进 digest；精确名字的 builtin、输入引用 lower 结果逐字段不变）。
- 新拒绝（各有修前失败证据，见 NC 记录）：memory 默认值读另一个 memory（NC-280，此前编译结果取决于 map 顺序）；持久状态默认值读 `$input` / `$memory`（NC-281）；cast_start / phase_start 读取的实体是可缺省的引用（NC-282）。
- 新放行（此前编译通过但每次 Runtime 失败，或 B3 起 lower 失败）：`$primary_target.position`、`$lifecycle_entity.position`、`$event.*.position`、`$input.target.position` 等投射（NC-283）。
- 诊断码变化：进程回调里的施法引用（如 `$caster`）此前由类型检查报 `REFERENCE_UNKNOWN`，现在报 `INPUT_UNAVAILABLE` 并点名表项；回调里的 cast_start / phase_start 读取此前由 owned entity pass 报 `INPUT_UNAVAILABLE`（路径为读取本身），现在报 `ATTRIBUTE_SNAPSHOT_INVALID`（路径 `.read_attribute.snapshot`）。拒绝的集合不变。
- 新导出错误 `skill.ErrReferenceOutOfContext`：只有编译器漏了位点时才会出现；不包裹 `ErrProgramInvariant`。
- 不改线格式、checkpoint、生成形状。`castInstance.evalContext` 是求值期间的临时状态，不进 checkpoint。

## 5. 守卫

- `skill/eval_contexts_table_test.go`：
  - `TestEvalContextTableEveryCellHasACase`：每一行有 fixture，每一格有语义，且有用例或写明“没有该类型的位点”的理由；加行 / 加上下文不补用例即失败。
  - `TestEvalContextTableCellsAgreeWithCompilerAndRuntime`：192 格逐格——可用格（第七轮之前含漂移格）：编译无 error、施法推进 10 tick 不出错、位点之后的见证效果执行；不可用格：编译被拒、诊断点名该上下文。143 格有用例，49 格是“采样点只求实体 / 进程字段没有该类型位点”。
  - `TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows`：全部 192 格的 Runtime 一半——不可用格 `ErrReferenceOutOfContext`（点名上下文与行、不是 `ErrProgramInvariant`），可用格不被查表拒绝。
  - `TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity`：O33 的漂移值就是表里写的那样。第七轮改为 `TestProcessStepPrimaryTargetIsRejectedAtCompileTime`（断言编译被拒），另加快照点表守卫与 O33 格子 / 替代写法用例，见 §8。
- 性质测试 `compile_mutation_property_test.go`：Runtime 返回 `ErrReferenceOutOfContext` 同样算失败；按表补 3 个种子（memory 默认值、在施法流程与回调里都被读的状态默认值、投射引用），引用位点的变异候选加上表里每一行的引用及实体行的 `.position` 投射。把 state_default 列的 `$input.*` 临时改成可用，新种子当场报 `seed.state_default_context $.persistent_state.who.default=$input.target: … immutable program invariant failed`。

## 6. 验证

见 [N09 第六批 §5](../review/REVIEW-2026-10-06-n09-batch6.md#5-修复与验证)。

## 7. 实施状态

已实施（分支 `revn09f`）。未完成：无。~ 格子第七轮已收紧为 ✗（§8）；`$caster` 在进程回调里 Runtime 其实求得出（= owner），表维持编译期 ✗（O36，写进作者文档）。

## 8. 维护者第七轮决定：O33 收紧为编译期拒绝，O34～O36 写进作者文档（2026-10-06，分支 `o33`）

决定见 `docs/review/DECISIONS-PENDING-2026-10-05.md` 末尾“第七轮”表。基线 `4da5e7ea`。代码读取以当前源码为准（codebase-memory 共享 generation 停在 09-30，未刷新）。

### 8.1 O33：漂移格子全部改为不可用

- `skill/eval_contexts.go`：§3 原 ~ 的 21 格改成 `evalNone`——引用表 process_step / state_default 两列的 `$primary_target`、`$ability.self`、`$cast.elapsed_ticks`、`$cast.charge_bp`、`$cast.release_reason`、`$cast.pulse_index`、`$cast.stock`、`$cast.max_stock`（16 格），快照点表 process_step / state_default 两列的 cast_start、phase_start（4 格），以及 memory_default 列的 phase_start（1 格，判断见 8.2）。`evalDrifting` / `evalDrift` 删除，格子只剩可用 / 不可用；`usable()` 改为 `== evalAvailable`。
- 每个新拒绝格的 semantics 写三件事：为什么没有（移交后的进程 / 进程里读写的状态默认值没有施法的这一项）、此前实际得到的值、“改用 …”。类型检查的诊断原样带出 semantics，所以诊断点名上下文、表项与替代写法，例如：
  `reference "$primary_target" is not available in evaluation context process_step: 进程字段每一步重新求值，移交后的进程没有施法的主目标（此前移交后漂移成 lifecycle 实体，O33 编译期拒绝）；改用施法流程里求一次的 spawn position（如 $input.target.position）把 lifecycle 实体放到目标处，或在回调里用 $lifecycle_entity / $event.target（如 on.tick 里 select from $lifecycle_entity 代替 area 选择） (evaluation context table row $primary_target)`。
  快照诊断补上表项：`… (evaluation context snapshot table row cast_start)`（此前只写 `snapshot table`）。
- Runtime 不用改：`evalReference` 本来就查同一张表，这些格子在 Runtime 里同样返回 `ErrReferenceOutOfContext`（只有编译器漏位点时才会出现）。
- 替代写法逐条验证过能编译、能跑（`TestO33AlternativesCompileAndRun`）。验证时排除了两条看似可行的写法：绑定到进程数值属性的 motion 字段只收字面量（`MOTION_INVALID`），不能放施法期的值；`set_memory` 不能存属性读取（量纲不符），所以 memory 默认值的替代不写“先存进 memory”。另发现 null 默认值的实体状态用 `modify_state set` 在 MemoryHost 上类型不匹配（null 默认值按 `valueKindNull` lower，`applyStateOperation` 比较 Base），与 O33 无关，已另开任务，替代写法用 `$caster` 默认值避开（已登记并修复：[RR-20261006-02](../bugfix/RR-20261006-02.md)，收尾第 3 批）。

### 8.2 memory 默认值里的 phase_start：拒绝

判断：不合理，拒绝。

- 名字承诺的是“phase 开始时的值”，但 memory 在 Activate 里初始化，早于第一个 phase，也早于 costs 支付与 windup（`runtime.go` Activate 先求 memory 默认值，`prepareCastWindow` 付费、排 windup，`executeCast` 进第一个 phase 时才采 phase_start）。有 windup 时第一个 phase 晚若干 tick 才开始，期间属性可能被 costs 或别的效果改掉，作者以为拿到的是 phase 开始时的值，实际是 Activate 时的值。
- 那个值与 memory 默认值里的 cast_start 是同一个（同一 tick、都在任何流程之前读当前值），拒绝不损失任何表达力：改写 cast_start 得到同样的值，要真正的 phase 开始时的值就在 phase 流程里读 phase_start。
- 改成“可用”只是给 cast_start 起了个会误导的别名；拒绝让名字与值一致。

### 8.3 O34～O36：行为不变，写明

- 作者文档：[施法语义 · 引用在哪里能读](../skill/skill-casting-and-combat.md#引用在哪里能读求值上下文)（五个上下文能读 / 不能读、O33 改写对照、O34～O36）；AI 生成约束 `ai-skill-system-prompt.md` 同步进程字段 / 状态默认值 / memory 默认值的规则；实现手册补一句“格子只有两种”。
- 表的 semantics：O34 写在快照点表 phase_start 的 cast_flow 格（最近一次进入的 phase 开始时的值；costs / windup 在第一个 phase 之前求值时读到求值那一刻的值；charge 模式在 release 时求值，读到当前 phase 开始时的值）；O35 写在 `$ability.self` 的 process_callback 格；O36 写在 `$caster` / `$caster.position` 的 process_callback 格（Runtime 求得出，编译期按表拒绝，改用 `$owner`）。

### 8.4 守卫与红绿

- `TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity` 改为 `TestProcessStepPrimaryTargetIsRejectedAtCompileTime`：同一份定义（area 的 `from` 写 `$primary_target`）编译被拒，`INPUT_UNAVAILABLE` 落在 `.area.from`，消息含 `process_step`、`row $primary_target` 与“改用”。
- `TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative`：上面 21 格逐格——表里不可用、semantics 有“改用”、有位点的格子编译被拒且诊断点名上下文、表项与替代写法。
- 新增快照点表守卫 `TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime`：3 个快照点 × 5 个非采样上下文 = 15 格逐格，可用格编译无 error、施法 10 tick、见证效果执行；不可用格 `ATTRIBUTE_SNAPSHOT_INVALID` 点名上下文与表项（此前快照点表没有逐格守卫）。进程字段只有实体 / 位置 / 距离 / 角度类型的位点，默认目录里没有距离量纲的属性，守卫用一个测试属性 `reach`。
- 引用表守卫 `TestEvalContextTableCellsAgreeWithCompilerAndRuntime` 的反例改为同时要求诊断点名表项；新拒绝的 16 个引用格自动变成反例（原来 `$ability.self` 与非 bp 的 `$cast.*` 在 process_step 是“可用但没有该类型的位点”，现在按不可用放进 area `from`，编译被拒）。
- 修前（基线 `4da5e7ea`，只加测试）：
  ```
  --- FAIL: TestProcessStepPrimaryTargetIsRejectedAtCompileTime
      $primary_target in a process field compiled; it drifts to the lifecycle entity after handoff (O33)
  --- FAIL: TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative
      row $primary_target context process_step: usable=true semantics "启动那一步是施法的主目标；移交后是进程的 lifecycle 实体（O33）"; O33 cells are rejected and name an alternative
      …（21 格各一行）
      snapshot phase_start context memory_default: usable=true semantics "Activate 时的值：memory 初始化早于第一个 phase 开始"; …
  --- FAIL: TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime/process_start/cast_flow
      no ATTRIBUTE_SNAPSHOT_INVALID diagnostic naming cast_flow and row process_start: … (evaluation context snapshot table)
  ```
  修前快照守卫里原 ~ 的格子都按“可用”通过了正例（编译、施法、见证效果执行），证明位点确实到达；失败的只是诊断不点名表项。修后全部通过。
- 性质测试：48 个种子（第六批前的 45 个 + 第六批 3 个）全部编译、施法通过，没有种子用到漂移引用，无需改写；`compiled_and_run` 8758 → 8725（33 个变异落在新拒绝的格子上，现在编译期拒绝），digest 性质 `compared=147` 不变。
- 仓库里的 fixture（`skill/testdata` 36 个）、`skill/examples`（fireball / combat / statusbridge）、`roost add skill` 骨架、game-demo 的 `fireball.json.tmpl` 都不用漂移格子（只在施法流程里用 `$ability.self` / `$primary_target` / cast_start），无需改动。

### 8.5 兼容

- **新拒绝**（今天能编译、升级后报错）：进程每一步重新求值的字段里的 `$primary_target`、`$ability.self`、`$cast.*`（`$cast.mode` 除外）、cast_start / phase_start 读取；持久状态默认值里的同一组引用与快照读取；memory 默认值里的 phase_start 读取。诊断码 `INPUT_UNAVAILABLE`（引用）/ `ATTRIBUTE_SNAPSHOT_INVALID`（快照）。按作者文档的对照改写。
- 接受集合的其余部分不变；gameplay digest 不变（没有改 lower 与 digest）；不改线格式、checkpoint、生成形状。
- 快照诊断消息多了 `row <point>`。

### 8.6 验证

`gofmt -l` 空；`GOWORK=off go vet ./skill ./kit/service/account`、`go test -race -count=3 ./skill ./kit/service/account` 通过；`go build ./... && go vet ./...`、根包 `go test -count=1 .`、`go test -run Skill ./codegen/internal/roost` 通过。未改 nest / entity / dataengine / sync，不跑 glsvet；未改生成形状，不重生成 game-demo。

### 8.7 实施状态

已实施（分支 `o33`）。未完成：无。另开任务：null 默认值实体状态的 `modify_state set` 类型不匹配（8.1）。
