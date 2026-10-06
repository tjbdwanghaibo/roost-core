# skill 求值上下文表：编译期与 Runtime 共用的“每个上下文能读哪些引用”

2026-10-06，维护者第五轮决定（`docs/review/DECISIONS-PENDING-2026-10-05.md` 末表“skill 求值上下文表”），分支 `revn09f`，基线 `0aeb5ab6`。来由：[N09 第五批方向判断](../review/REVIEW-2026-10-06-n09-batch5.md#6-方向判断)。同批的审查记录：[N09 第六批](../review/REVIEW-2026-10-06-n09-batch6.md)。

## 1. 目标

“编译器认可的写法”与“Runtime 能执行的写法”各自维护，第五批之前已出五次问题（NC-211、NC-220、NC-221、NC-223、NC-224），形态集中在**求值上下文**：编译期只有一套作用域（施法作用域 + 回调作用域），Runtime 至少有五种求值上下文，二者之间没有对照表，每加一个上下文或字段就要有人记得在编译器里补一条规则。

目标：一张表（代码里的单一来源，同 B3 的 `phase_events.go`）写明每种求值上下文能读哪些引用、值是什么；编译期的作用域从表生成，Runtime 求值时查同一张表，表外引用报错点名表项；按 pass 各写一份的检查收拢到表上。

不做（维护者决定）：NC-224 方向 B（进程启动时冻结施法输入）。第五批 O33 的“移交后语义漂移”在表里写明语义、保持现状（见 §3 的 ~ 格子）。

## 2. 表在哪里、怎么用

`skill/eval_contexts.go`：

- `evalContext`：8 个求值上下文（零值是施法流程）。比决定里列的五种多三个——审表时发现 memory 默认值、持久状态默认值是独立的求值点（NC-280 / NC-281），进程回调与进程每一步也要分开（回调有 `$owner` / `$event.*`，进程字段没有）。
- `evalReferenceTable`：每行一个引用（或一类：`$input.*`、`$memory.*`、`$local.*`），每格 `evalAvailable` / `evalDrifting` / `evalUnavailable` 加一句中文语义（“这里的值是什么”或“为什么没有”）。投射（`$event.target.position`）归到根所在的行。
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

## 3. 表（由 `evalReferenceTable` 生成；✓ 可用，~ 可用但值漂移，✗ 不可用）

| 引用 | cast_flow | memory_default | cast_start | phase_start | process_start | process_step | process_callback | state_default |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `$input.*` | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$memory.*` | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `$local.*` | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ✓ | ✗ |
| `$caster` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$caster.position` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$primary_target` | ✓ | ✓ | ✗ | ✗ | ✗ | ~ | ✗ | ~ |
| `$ability.self` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.mode` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✗ | ✓ |
| `$cast.elapsed_ticks` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.charge_bp` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.release_reason` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.pulse_index` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.stock` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
| `$cast.max_stock` | ✓ | ✓ | ✓ | ✓ | ✗ | ~ | ✗ | ~ |
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
| 快照 `cast_start` | ✓ | ✓ | — | — | — | ~ | ✗ | ~ |
| 快照 `phase_start` | ✓ | ~ | — | — | — | ~ | ✗ | ~ |
| 快照 `process_start` | ✗ | ✗ | — | — | — | ✗ | ✓ | ✗ |

每格的语义与理由见代码里的 semantics 字符串。~ 格子（第五批 O33）：

- 进程每一步（process_step）：`$primary_target` 启动那一步是施法的主目标、移交后是 lifecycle 实体；`$cast.elapsed_ticks` 移交后从 tick 0 算（即当前 tick）；其余 `$cast.*` 移交后是零值重算；`$ability.self` 移交后 handle 为 0；进程字段里的 cast_start / phase_start 读取移交后退化为 current。
- 状态默认值（state_default）：同一默认值在施法里与在进程回调 / 进程字段里读写时取值不同（同上几项）。
- memory 默认值里的 phase_start 读到的是 Activate 时的值（memory 初始化早于第一个 phase）。

这些格子保持现状（编译通过、Runtime 不失败）；要收紧时把格子改成 `evalNone` 并在守卫用例里补反例即可，不需要改别处。

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
  - `TestEvalContextTableCellsAgreeWithCompilerAndRuntime`：192 格逐格——可用 / 漂移格：编译无 error、施法推进 10 tick 不出错、位点之后的见证效果执行；不可用格：编译被拒、诊断点名该上下文。143 格有用例，49 格是“采样点只求实体 / 进程字段没有该类型位点”。
  - `TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows`：全部 192 格的 Runtime 一半——不可用格 `ErrReferenceOutOfContext`（点名上下文与行、不是 `ErrProgramInvariant`），可用格不被查表拒绝。
  - `TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity`：O33 的漂移值就是表里写的那样。
- 性质测试 `compile_mutation_property_test.go`：Runtime 返回 `ErrReferenceOutOfContext` 同样算失败；按表补 3 个种子（memory 默认值、在施法流程与回调里都被读的状态默认值、投射引用），引用位点的变异候选加上表里每一行的引用及实体行的 `.position` 投射。把 state_default 列的 `$input.*` 临时改成可用，新种子当场报 `seed.state_default_context $.persistent_state.who.default=$input.target: … immutable program invariant failed`。

## 6. 验证

见 [N09 第六批 §5](../review/REVIEW-2026-10-06-n09-batch6.md#5-修复与验证)。

## 7. 实施状态

已实施（分支 `revn09f`）。未完成：无。后续可选：把 ~ 格子收紧为 ✗（需维护者定，见 §3）；`$caster` 在进程回调里 Runtime 其实求得出（= owner），表维持编译期现状 ✗。
