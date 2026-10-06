# N09 skill 第六批：求值上下文表（维护者第五轮决定）；按表审 ability / input / state / tags / optional 与类型检查作用域

2026-10-06，基线 `0aeb5ab6`（origin/main，含第五批 `5c04726f` 与第五轮决定记录），独立 worktree 分支 `revn09f`，NC 编号段 280～289（本批用 280～283）。上一批：[第五批](REVIEW-2026-10-06-n09-batch5.md)。方案：[求值上下文表](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。

图谱：项目 `Users-whb-roost-roost-core` 的共享 generation 停在 2026-09-30，落后于第五批改过的 `compile_snapshot.go` / `compile_owned_entity.go` / `lower.go`；本批全部结论以 worktree 当前源码通读与探针为准（探针 `zz_probe_n09f*_test.go` 跑完删除，结论落为正式回归）。

## 1. 先做表（决定 1）

“编译器认可的写法”和“Runtime 能执行的写法”在求值上下文上各自维护，第五批之前出过五次（NC-211、NC-220、NC-221、NC-223、NC-224）。本批把“每个求值上下文能读哪些引用、值是什么”做成 `skill/eval_contexts.go` 一张表，编译期作用域由表生成，Runtime 求值查同一张表，表外引用报 `ErrReferenceOutOfContext` 点名表项；NC-220 / NC-224 与 owned entity pass 里按 pass 各写一份的引用检查收拢到表上。细节、全表与兼容见[方案](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。

做表时逐格对照 Runtime 的求值点，找出四处缺陷（§2）：memory 默认值、状态默认值是决定里没列出的独立求值上下文，采样点不认读取处的 exists 守卫，投射引用在 lower / Runtime 没有对应。

## 2. 确认缺陷

| 编号 | 等级 | 一句话 | 所在 pass |
| --- | --- | --- | --- |
| [NC-280](../bug/RR-20261005-NC-280.md) | P2 | memory 默认值读另一个 memory：编译结果取决于 map 遍历顺序，通过时被读槽位排在后面则每次 Activate 类型不匹配 | input_state → typecheck（memory 声明） |
| [NC-281](../bug/RR-20261005-NC-281.md) | P2 | 持久状态默认值按施法作用域检查：`$input.target` 默认值在进程回调里读写时 ErrProgramInvariant | state / typecheck |
| [NC-282](../bug/RR-20261005-NC-282.md) | P2 | cast_start / phase_start 读取的实体是可缺省引用时，读取处的 exists 守卫在采样点不生效，缺省即施法失败 | snapshot / optional |
| [NC-283](../bug/RR-20261005-NC-283.md) | P2 | `$primary_target.position`、`$event.*.position`、`$lifecycle_entity.position` 编译通过、每次求值 ErrProgramInvariant；`$input.target.position` 从 B3 起 LOWER_UNRESOLVED | typecheck 投射 / lower |

## 3. 逐分支审（决定 2）：ability / input / state / tags / optional

| 编号 | pass | 对照的执行点 | 结论 |
| --- | --- | --- | --- |
| D1 | `compile_input.go` 每种 input 布局的槽位与类型 | `runtime_input.go` `freezeCastInput` 每个分支写的槽位（drag 的 `drag_length` 量纲 WorldDistance、path 的起止点）、`Input` 端口更新（direction_changed / target_changed 只在有对应槽位时开放，target_changed 同时改 `$primary_target`） | 一致；被动输入由 NC-221 收紧 |
| D2 | `compile_state.go` 作用域 / 寿命 / 类型 / 操作 | `runtime_state.go` 读写、`stateOperationAllowed` 与 host 的 int 边界 | 状态默认值的求值上下文 → **NC-281**；其余一致 |
| D3 | 类型检查的 memory 声明 | Activate 的 memory 初始化顺序 | **NC-280** |
| D4 | `compile_ability.go` 属性 / 操作 / 过滤 / 排序 | `runtime_ability.go` `readAbilityStateLocked` / `modifyAbilityStateLocked`（共用 `abilityOperationAllowed`）、`runtime_select.go` 技能过滤 | 一致；`self_ability` 过滤在进程里比较 handle 0（O35） |
| D5 | `compile_tags.go` 声明标签、伤害类型 / 元素 / 战斗标签 | lower 取 `gameplay.damage[path]`、Host 结算 | 一致 |
| D6 | `compile_optional.go` 与类型检查的可缺省规则（exists 收窄只作用于直接引用；memory 引用豁免交给 memory pass 的数据流） | 读取处与采样点 | 采样点不认收窄 → **NC-282**；memory 未初始化由 `MEMORY_MAYBE_UNINITIALIZED` 覆盖（探针确认） |
| D7 | `compile_typecheck.go` 作用域模型（`projectedReferenceType` 投射、回调作用域、进程字段） | `lowerReference`、`evalReference` | 投射 → **NC-283**；作用域模型改为表生成（§1） |

## 4. 观察（不登记 RR，不改行为）

- **O33（第五批）在表里写明**：进程每一步与状态默认值里的 `$primary_target`、`$ability.self`、`$cast.*`、cast_start / phase_start 读取移交后的值，见[方案 §3](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md#3-表由-evalreferencetable-生成可用--可用但值漂移-不可用)；守卫 `TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity` 钉住 `$primary_target` 的漂移值。要收紧时改格子即可。
- **O34 costs / windup 里的 phase_start 读取**：在第一个 phase 开始之前求值，读到的是求值那一刻的值（并写进缓存，随后被 phase 开始时的采样覆盖）。表的 cast_flow 格写明，不改。
- **O35 `self_ability` / `not_self_ability` 过滤在进程回调里比较 handle 0**：与 `$ability.self` 漂移同源（`detachedProcessCast` 不带技能句柄），回调里 `self_ability` 永不匹配、`not_self_ability` 全匹配。
- **O36 `$caster` 在进程回调里 Runtime 求得出**（= 进程 owner，即同一施法者），编译期按现状拒绝（让作者用 `$owner`）；表维持 ✗。
- **O37 account 判定表 `unadmitted other` 行的 free 列**：第五轮决定只放开 res-else 格，free 格（未 admitted、名字已无人持有）仍 `ErrRoleLimit`，与同名重试的 `taken-r` 仍不对称，留给维护者（见 B9 方案 §6）。
- O1～O32 不改行为。

## 5. 修复与验证

| 命令（`GOWORK=off`，模块根） | 结果 |
| --- | --- |
| 新增正式用例修前（临时 worktree `0aeb5ab6` + 用例文件）：NC-280 2、NC-281 2、NC-282 4、NC-283 6 个子用例 | 全部 FAIL，原文见各 bug 记录 |
| 同组控制：`TestMemoryDefaultsReadingTheCastStillRun`、`TestStateDefaultOfTheCasterRunsInCallbacks`、`TestGuardedOptionalReadsAtCurrentStillRun` | 修前修后都 ok |
| 表守卫：`TestEvalContextTableEveryCellHasACase`、`TestEvalContextTableCellsAgreeWithCompilerAndRuntime`（143 格有用例、49 格无该类型位点）、`TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows`（192 格）、`TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity`、`TestRuntimeReportsOutOfContextReferencesAgainstTheTable` | ok |
| 性质测试 `TestCompiledMutations*`（48 种子，31629 变异 / 8754 编译并施法）；`SKILL_MUTATION_FULL=1`（63900 / 23123） | ok |
| 种子有效性：state_default 列临时放开 `$input.*` | `seed.state_default_context $.persistent_state.who.default=$input.target: … immutable program invariant failed`（还原后 ok） |
| digest 不变：45 个既有种子在 `0aeb5ab6` 与本分支的 gameplay / presentation digest 逐一比对（临时探针） | 全部相同 |
| 既有 NC-220～224、NC-210 / 211 等全部 skill 用例 | 未改断言，全部 ok |
| `go test -count=1 ./skill/...`；`go test -race -count=3 ./skill/...` | 5 包全部 ok |
| `gofmt -l skill`；`go vet ./skill/...` | 空；通过 |
| `skill/examples`：`go build ./...`、`go run ./fireball`；`skill/integration/sync-e2e`：`go test ./...`；`go test ./codegen/internal/roost -run Skill` | 通过 |
| `go build ./... && go vet ./...`；根包 `go test -count=1 .` | 通过 |

未跑：glsvet（没有改 nest / entity / dataengine / sync）；codegen 生成形状未变，未生成 game-demo；没有外部依赖。

组合契约复核：新诊断只经 `Compile` 返回（game-demo 的 `CompileAll` 把 error 诊断当启动失败，见 T-269）；`ErrReferenceOutOfContext` 只在编译器漏位点时出现，性质测试把它当失败；`castInstance.evalContext` 在每个切换点用返回值还原（memory 默认值循环出错时整个 cast 丢弃），不进 checkpoint。

## 6. 方向判断

第五批判断的根因（编译期一套作用域、Runtime 多个求值上下文、没有对照表）本批按决定做成了表。表一做出来，逐格对照就又找出四处同一机制的缺陷（NC-280～283），且都是“多一个求值点 / 少一处认识”的形态——印证了前提问题判断，而不是实现细节。收益是以后加上下文或字段时，守卫会逼着补用例；剩下的风险在表的两侧接线：

1. 求值点必须切到正确的上下文（`switchEvalContext` 散在 6 处）。漏切只会让 Runtime 查错列：若查到更宽的列，表外引用落回 `ErrProgramInvariant`（性质测试会报），不会静默错算。
2. ~ 格子（O33）仍是“编译通过、值漂移”。维护者不做冻结（NC-224 方向 B），建议下一步二选一：把这些格子改成 ✗（作者只能用 `$caster` / `$owner` 一类不漂移的引用），或保持并在作者文档里列出。前者是行为收紧，需维护者定。

没有发现需要换掉表本身设计的证据。

## 7. 停点与下一批入口

已完成：决定 1（表、编译期与 Runtime 接线、收拢 NC-220 / NC-224 / owned entity 引用检查、守卫、性质测试按表补种子）；决定 2（ability / input / state / tags / optional 与类型检查作用域逐分支）；决定 3 见 B9 方案 §6（单独提交）。N09 剩余入口：维护者对 ~ 格子与 O34～O37 的决定；前几批 O1～O32、NC-151 / NC-213 方向 A；性质测试随新效果补种子（表的守卫已强制新行 / 新上下文的用例）。
