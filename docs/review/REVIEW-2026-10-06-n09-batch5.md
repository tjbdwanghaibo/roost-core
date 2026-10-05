# N09 skill 第五批：random / snapshot / temporal / graph / effect_result / proc / quantity 逐分支对照 Runtime / Host；直接分支用例；B4 文档约束

2026-10-06，基线 `57c0b3b6`（origin/main，含 B3 `023eb276`），独立 worktree 分支 `revn09e`，NC 编号段 220～229（本批用 220～224）。上一批：[第一批](REVIEW-2026-10-05-n09-batch1.md) · [第二批](REVIEW-2026-10-05-n09-batch2.md) · [第三批](REVIEW-2026-10-05-n09-batch3.md) · [第四批](REVIEW-2026-10-05-n09-batch4.md)。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。B3 的 lower fail-fast 与事件派发表见 [B3 方案](../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)；B3 ③（Host 能力表）按决定留到下个大版本，本批不做。

图谱：项目 `Users-whb-roost-roost-core`，generation 2026-09-30T11:28:30Z（共享 generation 未刷新）。本批引用的 `compile_random.go`、`compile_snapshot.go`、`compile_temporal.go`、`compile_graph.go`、`compile_effect_result.go`、`compile_proc.go`、`compile_quantity.go`、`runtime_random.go`、`runtime_temporal.go`、`runtime_effect_result.go`、`runtime_proc.go`、`memory_host_temporal.go` coverage 为 `no_recorded_issue / metadata_match`；`lower.go` 为 `metadata_changed`（B3 改过），按当前源码读取。全部结论以 worktree 当前源码与探针为准。

## 1. 本批审过的场景

| 编号 | pass | 对照的执行点 | 结论 |
| --- | --- | --- | --- |
| C1 | random（`compile_random.go`、`compile_identity.go`） | `lower.go` 随机位点分配顺序（与 `walkPhaseFlows` / 回调表顺序逐项对齐）、`runtime_select.go` executeQuery 的打分与 invocation 计数、`runtime_random.go`、owned 进程的 randomKey / invocation 复制（`process_owned.go`）、checkpoint 字段 | 无确认缺陷；area 成员顺序不能随机（`compile_motion.go`）。O30 |
| C2 | snapshot（`compile_snapshot.go`） | `runtime_eval.go` captureSnapshots / shouldCacheSnapshot、`runtime.go` cast_start 采样、`executor.go` phase_start 采样、`process_owned.go` captureOwnedProcessSnapshots、`lower.go` lowerSnapshots | **NC-220**（缓存型采样点不按采样上下文检查）、**NC-223**（B3 回归：局部变量实体的读取编译失败）；O28 |
| C3 | temporal（`compile_temporal.go`） | `runtime_temporal.go`、`memory_host_temporal.go` 捕获 / 恢复 / 策略、`compile_authority.go` profile 校验、persistent snapshot_token | 无确认缺陷；O27 |
| C4 | graph（`compile_graph.go`） | `executor.go` executeCast 的 phase 迁移上限、goto 在 result 分支 / 取消事件里的派发（`executor.go:296-306`） | 无确认缺陷 |
| C5 | effect_result（`compile_effect_result.go`） | `executor.go` 各效果分支是否调 resolve*、`runtime_effect_result.go` executeEffectResultRoot（挂起即 ErrProgramInvariant）、flowSuspend 的全部产生点（只有 wait 与带间隔的 repeat） | 无确认缺陷；结果布局与 resolve 调用一一对应（resource / memory / motion impulse / modify_process / stop_movement 无布局、也不 resolve）。审到 effect flow 结构时发现 **NC-222**（非 spawn 效果上的 process / on 被丢弃）；O29 |
| C6 | proc（`compile_proc.go`） | `runtime_proc.go` executePassiveActivation 的压制顺序、passiveCastInput、eventMatchesFilter | **NC-221**（max_depth 0、非 none / entity 输入的被动永不触发）；O32 |
| C7 | quantity（`compile_quantity.go`、`compile_typecheck.go` 的 infer / expressionType） | `runtime_eval.go` evalExpression（只有 add 与 scale_bp 在运行期比较量纲）、`runtime_value.go` CheckedAdd / CheckedScaleBP、effect result 字段量纲校验、`lower.go` lowerQuantities | 无确认缺陷：未知量纲只出现在 numeric track / modify_process 的期望类型上（那里运行期只取整数）；42 个种子的类型表里没有未知量纲的 int。O31 |
| C8 | 性质测试补种子（第四批局限“新增效果 / 字段时应同时加种子”） | 3 个新种子：局部变量实体的 current 读取、spawn 回调里的 process_start、entity 输入的被动 | 新种子变异出 **NC-224**（进程字段读施法输入，下一 tick ErrProgramInvariant）；修后单点 25631 变异 / 7968 编译并施法，`SKILL_MUTATION_FULL=1` 52359 / 20615，全部通过 |
| C9 | 直接分支用例（第四批性质测试只是间接覆盖） | 见 §4 | 已补 `skill/compile_pass_branches_test.go` 与 `compile_capture_context_promises_test.go` |

探针 `zz_probe_n09e*_test.go` 跑完删除，结论落为正式回归。

## 2. 确认缺陷

| 编号 | 等级 | 一句话 |
| --- | --- | --- |
| [NC-220](../bug/RR-20261005-NC-220.md) | P2 | 缓存型快照点不按采样上下文检查：phase 流程里的 process_start 有 owned spawn 时每次施法 ErrProgramInvariant，没有时退化成第一次读到的值；实体是局部变量时落到 LOWER_UNRESOLVED at `$` |
| [NC-221](../bug/RR-20261005-NC-221.md) | P3 | max_depth 0、输入不是 none / entity 的被动能编译，每次触发都被静默压制 |
| [NC-222](../bug/RR-20261005-NC-222.md) | P3 | 非 spawn 效果上的 process / on 能编译，Runtime 从不启动进程、回调从不执行 |
| [NC-223](../bug/RR-20261005-NC-223.md) | P2 | **B3 回归（v1.20.2）**：lowerSnapshots 用空作用域 lower 全部计划的实体，“对每个选中目标读它自己的属性”编译失败 |
| [NC-224](../bug/RR-20261005-NC-224.md) | P2 | spawn 进程每一步重新求值的字段（area 选择、tracking / follow 目标等）读 `$input` / `$memory` / `$local` 能编译，下一 tick 起 ErrProgramInvariant |

## 3. 观察与设计建议（不登记 RR，不改行为）

- **O27 restore 的 `on_blocked` 与 profile 策略不同时必然失败**。`memory_host_temporal.go:58-63`：空值取 profile 的策略，非空且不同则 `policy_rejected` 预期失败。编译期只查取值合法，token 可经 persistent state 跨施法传递，profile 在编译期不一定可知。`on_blocked` 实际只能当“与 profile 一致”的断言用；建议文档写明，或允许它覆盖 profile（语义变化，需维护者定）。已由 `TestTemporalPassBranches` 钉住当前行为。
- **O28 process_start 的实体在启动事件上下文里求值**。回调里 `entity: "$event.target"` 取的是启动事件的 target（lifecycle 实体），不是每次回调的目标。与 cast_start “整个读取在采样点求值”的口径一致，NC-220 不改；建议文档写明，作者要按回调目标读时用 current。
- **O29 effect result 分支里的 spawn + 无回调进程能编译并执行**。`effectResultBranchMaySuspend` 只把带 `on` 的效果当作“启动进程”，诊断文案写 “cannot suspend or start a process”。分支执行时进程照常启动，没有失败；文案与规则不一致，按设计取舍二选一。
- **O30 同一施法的多个 owned 进程随机顺序相同**。进程复用施法的 randomKey，invocation 从 0 计（`process_owned.go:37`），打分不含进程 ID：一次 spawn count 2 的两个陷阱在同一步对同一候选集得到同一顺序。确定性成立；是否算“随机”取决于设计意图。`RandomSite.InvocationBound` 也不乘进程的步数（只作 Inspect / digest 元数据）。
- **O31 量纲“证明”是全 int64 区间**。`lowerQuantities` 对每个 int 路径写 `minimum=MinInt64, maximum=MaxInt64, proved=true`，Inspect 与 digest 都带着它；numeric track / modify_process 的值不检查量纲（期望类型的量纲是未知）。建议改名或删字段，或做真正的区间推导。
- **O32 proc 的 `event_filter.results` 不是封闭集合**。与 O24（relation）同形：字符串原样比较 `EventContext.Result`，写错时 filter 静默全部不匹配。
- **O33 进程字段在移交后的语义漂移（不失败）**。NC-224 只拒绝会让求值失败的引用。`$primary_target` 启动那一步是施法目标、之后是 lifecycle 实体；`$cast.elapsed_ticks` 之后从 tick 0 算；`$ability.self` 之后 handle 为 0；进程字段里的 cast_start / phase_start 读取之后退化成 current。属于 NC-224 方向 B（冻结施法上下文）一并决定的范围。
- **O1～O26（前四批）、NC-151 / NC-213 方向 A**：本批不改这些行为，等维护者决定。

## 4. 直接分支用例

`skill/compile_pass_branches_test.go`（新）：

| pass | 用例 | 内容 |
| --- | --- | --- |
| random | `TestRandomPassBranches` | 位点 InvocationBound 按 repeat × select-each 放大（[1 3 2]）；area 随机顺序拒绝；同种子同顺序、limit 在洗牌之后截断 |
| temporal | `TestTemporalPassBranches` | 未知 profile、非法 on_blocked 拒绝；on_blocked 为 fail / 空 / stay 时分别走 success / success / failure 分支（O27） |
| graph | `TestGraphPassBranchesRunEveryReachablePhase` | a → b（result 分支里 goto）→ c，每个 phase 的 enter 各执行一次 |
| effect_result | `TestEffectResultPassBranches` | 无布局效果、挂起分支、带间隔 repeat、带回调的进程效果拒绝；结果槽位预算；result 分支里的 spawn + 进程能启动（O29） |
| proc | `TestProcPassResultFilterBranches` | results 过滤匹配时施法、不匹配时压制为 filter |
| quantity | `TestQuantityPassBranches` | ticks + count 拒绝；字面量取期望量纲并执行；量纲证明为全区间（O31） |

snapshot 与 NC-220～224 的承诺用例在 `skill/compile_capture_context_promises_test.go`。

## 5. 未审 / 未验证

- `compile_ability.go`、`compile_input.go`、`compile_state.go`、`compile_tags.go`、`compile_optional.go` 前五批都没有逐分支读；`compile_typecheck.go` 的作用域模型只在本批涉及的位置（回调作用域、进程字段）核对过。
- 没有接真实 Host（Mongo / Redis / NATS 不涉及）；combatcomponent 不实现 temporal / spawn，相关结论只对 MemoryHost 与 Runtime 成立。
- NC-224 方向 B（冻结施法上下文）未实施；O27～O33 未改。

## 6. 方向判断

“编译器接受的集合与 Runtime 能执行的集合各自维护”第五次出现，本批的形态集中在**求值上下文**：

| 批次 | 形态 | 编号 |
| --- | --- | --- |
| 第四批 | 移交后 area 回调 finish 是不变量失败 | NC-211 |
| 本批 | 缓存型快照的采样上下文与读取处不同 | NC-220 |
| 本批 | 进程字段按施法作用域检查、按进程上下文求值 | NC-224（O33 是同一机制的不失败形态） |
| 本批 | 被动的施法上下文只来自事件 | NC-221 |
| 本批 | 快照计划用空作用域 lower（B3 把漏洞变成编译失败） | NC-223 |

判断：B3 的 fail-fast 起了作用——NC-223 是它暴露出来的“lower 用错作用域”，而不是新的静默错误；但 B3 自己的回归种子没有以局部变量为实体的读取，所以发布前没看见。根因仍是前提问题：类型检查用一套作用域（`typeScope`：施法作用域 + 回调作用域），Runtime 有至少五种求值上下文（施法流程、cast_start、phase_start、进程启动、移交后的进程步 / 回调），二者之间没有对照表；每加一个上下文或字段，就要有人记得在编译器里补一条规则。建议（代价递增）：

1. 本批做的：按上下文逐条补规则，性质测试补“上下文”种子（新种子当场找出 NC-224）。
2. 把“求值上下文 → 可用引用”做成一张表：编译期给每个值位点标上下文（类型检查时已知道是回调、进程字段还是 phase 流程），引用可用性查同一张表；Runtime 的 `detachedProcessCast` / `evalReference` 的 builtin 分支按同一张表断言。代价中等，不改线格式与 digest。
3. NC-224 方向 B：进程启动时冻结施法输入（`ProcessInstance.inputs` / checkpoint 字段已存在、目前不填），让 path 投射物、落点投掷可用，并顺带消掉 O33 的漂移。语义扩展，需维护者决定。

## 7. B4：skill Runtime 状态不进事务（维护者决定，本批落实文档）

按决定保持现状，只写清约束：handler 失败或提交被拒回滚后，Runtime 的冷却、ammo、cast 状态、proc 账本、state mutation 流与 revision（以及业务 `RevisionSource.CommitEffect` 推进的 revision 和事件）都不回退。写入 [skill-casting-and-combat.md](../skill/skill-casting-and-combat.md)“Runtime 不在事务里（B4）”一节、[skill README](../../skill/README.md) 的 combatcomponent 一段、[roost-coding 执行契约](../agent-skills/roost-coding/SKILL.md) A1 条的例外说明。glsvet 的 A1 提示只看组件方法里的 undo 登记，skill 各包零提示（`cmd/glsvet` 新增 `TestSkillPackagesGetNoComponentUndoHint` 钉住），不需要豁免，只在 `componentUndoHints` 注释里写明 B4 例外与理由。

## 8. 停点与下一批入口

已审：编译器 random / snapshot / temporal / graph / effect_result / proc / quantity 逐分支对照 Runtime / Host；直接分支用例与性质测试种子已补。N09 剩余入口：

1. `compile_ability.go`、`compile_input.go`、`compile_state.go`、`compile_tags.go`、`compile_optional.go` 逐分支；`compile_typecheck.go` 作用域模型对照 Runtime 求值上下文（若维护者采纳方向判断第 2 条，先做表再审）；
2. 维护者决定：NC-224 方向 B、方向判断第 2 条、O27～O33，以及前四批的 O1～O26、NC-151 / NC-213 方向 A；
3. 性质测试的种子随新效果 / 新上下文补充。

## 9. 修复与验证（审查之后追加）

审查记录与修复同批提交：`5c04726f`（`fix(skill)`，NC-220～224 与本记录）；B4 文档 `62cec54e`（`docs(skill)`）。每条一个修复单元：[NC-220](../bugfix/RR-20261005-NC-220.md)、[NC-221](../bugfix/RR-20261005-NC-221.md)、[NC-222](../bugfix/RR-20261005-NC-222.md)、[NC-223](../bugfix/RR-20261005-NC-223.md)、[NC-224](../bugfix/RR-20261005-NC-224.md)。

| 命令（`GOWORK=off`，模块根） | 结果 |
| --- | --- |
| 新增正式用例修前：NC-220 6、NC-221 5、NC-222 3、NC-223 4、NC-224 5 个子用例；性质测试 1 条（NC-224） | 全部 FAIL，原文见各修复记录 |
| 同组控制：`TestSnapshotsCapturedAtTheirPointStillRun`、`TestPassivesWithEventInputsStillActivate`、`TestOwnedProcessFieldsReadingTheCasterStillRun` | 修前修后都 ok |
| NC-223 回归证据：同一用例文件放到 `023eb276^` 的临时 worktree | 四个子用例 ok |
| digest 不变：42 个既有种子在 origin/main 与本分支的 gameplay / presentation digest 逐一比对（临时探针） | 全部相同 |
| `go test -count=1 ./skill/...`；`go test -race -count=3 ./skill/...` | skill / combat / combatcomponent / skillcompose / skillsync 全部 ok |
| `SKILL_MUTATION_FULL=1 go test -run TestCompiledMutations ./skill` | ok（52359 变异 / 20615 编译并施法） |
| `gofmt -l skill cmd`；`go vet ./skill/... ./cmd/glsvet`；`go test -race -count=3 ./cmd/glsvet` | 空；通过 |
| `skill/examples`：`go build ./...`、`go run ./fireball`；`skill/integration/sync-e2e`：`go test -count=1 ./...` | 通过 |
| `go build ./... && go vet ./...`；根包 `go test -count=1 .`；`go test -count=1 ./codegen/internal/roost -run Skill` | 通过 |

既有测试随规则收紧调整（断言意图不变）：`motion_test.go` 的 path 用例改为直接驱动 `advanceMotionPath`、parabola 目的地改为 `$caster.position`、“未初始化的 motion memory 目标”改为断言 `INPUT_UNAVAILABLE`（见 NC-224 修复记录）。

组合契约复核：新诊断只经 `Compile` 返回，game-demo 的 `CompileAll` 本来就把 error 诊断当启动失败；生成骨架与 fireball 零诊断（`TestGeneratedSkillDefinitionsCompileWithoutDiagnostics` 通过）。NC-223 只改 lower 取实体的来源，今天能编译的定义输出逐字段不变（digest 比对）。没有改生成形状，未生成 game-demo 工程；没有用到外部依赖。T-265。
