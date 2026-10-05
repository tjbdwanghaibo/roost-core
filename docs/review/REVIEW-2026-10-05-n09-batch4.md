# N09 skill 第四批：编译器其余 pass 对照 Runtime 执行点、lower / program_digest、presentation、skillsync observability / schema；编译 ⇒ 可执行的变异性质测试

2026-10-05，基线 `c10cc9ac`（origin/main；`skill/` 自第三批修复 `bfd353c0` 起未变），独立 worktree 分支 `revn09d`，NC 编号段 210～219（本批用 210～216）。上一批：[第一批](REVIEW-2026-10-05-n09-batch1.md) · [第二批](REVIEW-2026-10-05-n09-batch2.md) · [第三批](REVIEW-2026-10-05-n09-batch3.md)。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。

图谱：项目 `Users-whb-roost-roost-core`，generation 2026-09-30T11:28:30Z（共享 generation 未刷新）。本批引用的 `compile_*.go`、`lower.go`、`program_digest.go`、`presentation*.go`、`process*.go`、`runtime_cast.go`、`runtime_select.go`、`memory_host_*.go`、`combatcomponent/*`、`skillsync/schema.go`、`observability.go` coverage 为 `no_recorded_issue / metadata_match`；`compile_shape.go`、`scheduler.go`、`skillsync/applier.go` 为 `metadata_changed`（前三批改过），按当前源码读取。图谱只用于定位，全部结论以 worktree 当前源码与探针为准。

## 1. 本批审过的场景

| 编号 | 场景 | 读过的源码 / 跑过的 | 结论 |
| --- | --- | --- | --- |
| C1 | compile_authority：环境校验（handle / key 唯一、引用、策略、motion / process property catalog）与 authority 表 | `compile_authority.go` 全文、`compile_capability.go` 全文；重复 key 探针 | **NC-212**：只查 handle 唯一不查 key 唯一，同一 key 在一次编译里被 first-wins / last-wins 两种查找解析 |
| C2 | compile_environment：默认 catalog、Visual catalog、limits | `compile_environment.go` 全文 | 默认环境自洽；Visual catalog 的 digest 不覆盖内容（O21） |
| C3 | compile_owned_entity：spawn 计数 / 时长 / 绑定、detached 回调限制、owned select | `compile_owned_entity.go` 全文；对照 `process_owned.go`（start / handoff / callback / reap）、`process.go:189-284` | **NC-211**：移交后 area 回调的 finish → ErrProgramInvariant |
| C4 | compile_status：status_instance select 的 filter / order / limit / consumer、实例修改操作 | `compile_status.go` 全文；对照 `runtime_select.go:168-260`、`wire_select.go:530-610` | 与 Runtime 派发表一致；但同一批 status / attribute / resource 名字在别处不查（**NC-214**） |
| C5 | compile_visual：visual 校验、intern、预算 | `compile_visual.go` 全文 | 无确认缺陷 |
| C6 | compile_motion 后半：process kind / area / 阶段变体 / collision / 字面量 | `compile_motion.go:100-395`；对照 `process_motion.go` | 无确认缺陷；summon 的 `duration_ticks` 不校验也不使用（O22） |
| C7 | 编译器其余 pass 与 Runtime 的字段对照 | 对 `program*.go` 每个字段在 Runtime / Host 侧（排除 lower / digest / inspect / compile*）找读取点的脚本；`memory_host_status.go`、`memory_host_effect.go`、`combatcomponent/status_bridge.go`、`adapter.go` 的拒绝分支 | **NC-213**：chain `allow_repeat` / `hop_interval_ticks`、attribute_modifier `stack_policy` / `max_stacks` 只编译不传；**NC-215**：两个参考 Host 都拒绝的取值能编译 |
| C8 | lower.go：名字 → 槽位 / handle 的查找 | `lower.go` 全文，逐个 map 查找核对编译期是否已拒绝未知键 | **NC-210**（memory 名字落到槽位 0）、**NC-214**（status / attribute / resource 名字落到 handle 0）；其余查找（tag、collision、unit template、phase、ability property、state）都有编译期检查 |
| C9 | program_digest.go：gameplay digest 是否覆盖 Program 执行内容 | `program_digest.go` 全文、`program*.go` 结构；变异性质测试第二部分（同 digest ⇒ 执行内容相同） | 无确认缺陷（单点变异与子树移植下未发现“同 digest、不同 Program”） |
| C10 | presentation / presentation_assets：计划、事件缓冲、PollPresentation、ResolvePresentationPlan | `presentation.go`、`presentation_assets.go` 全文；`runtime.go:276`（PresentationLimit 默认）、`runtime_checkpoint.go:826`（序号恢复） | 无确认缺陷；digest 保护见 O21 |
| C11 | skillsync observability：Health 阈值、ExportMetrics | `observability.go` 全文、`outbox.go` Metrics | 无确认缺陷（O23） |
| C12 | skillsync schema：协商、迁移图最短路径、Seal、Applier 准入 | `schema.go` 全文、`applier.go:100-200`；协商探针 | **NC-216**：一边空区间时 NegotiateSchema 仍返回版本；迁移图 BFS（按版本升序扩展、visited 首见即定）确定性成立 |
| C13 | 编译 ⇒ 可执行的性质测试（第三批方向判断第 3 条） | 见 §4 | 可用现有 fixture + 变异实现；修前红在 NC-210 / NC-211 |

探针 `zz_probe_n09d*_test.go`（skill、skillsync）跑完删除，结论落为正式回归与性质测试。

## 2. 确认缺陷

| 编号 | 等级 | 一句话 |
| --- | --- | --- |
| [NC-210](../bug/RR-20261005-NC-210.md) | P2 | memory 效果的 name 不查是否声明，lower 落到槽位 0：静默改写别的 memory，或每次施法 ErrProgramInvariant；bool memory 上的 add_memory 能编译 |
| [NC-211](../bug/RR-20261005-NC-211.md) | P2 | 施法先结束、area 进程已移交后，回调里的 finish 让 Advance 返回 ErrProgramInvariant 并中断这一 tick |
| [NC-212](../bug/RR-20261005-NC-212.md) | P3 | CompileEnvironment 不查 catalog key 唯一，同一 key 在一次编译里解析成两个条目 |
| [NC-213](../bug/RR-20261005-NC-213.md) | P3 | chain allow_repeat / hop_interval_ticks、attribute_modifier stack_policy / max_stacks 只编译、不传给 Host |
| [NC-214](../bug/RR-20261005-NC-214.md) | P2 | status / attribute / resource 名字在 effect、filter、cost 里不查 catalog，lower 兜底成 handle 0 |
| [NC-215](../bug/RR-20261005-NC-215.md) | P3 | 两个参考 Host 与 Runtime 都拒绝的取值能编译（modifier operation / 时长、status 时长 0、resource operation、负 cost、compare op） |
| [NC-216](../bug/RR-20261005-NC-216.md) | P3 | NegotiateSchema 在一边空区间时仍返回版本 |

## 3. 观察与设计建议（不登记 RR，不改行为）

- **O21 Visual catalog 的 digest 不覆盖内容**。`presentation_assets.go:23-25` 写“digest 保护 revision 标签被误复用的部署”，但默认环境与 `WithTestRevision` 的 digest 是 `digestStrings("visual", revision, Themes)`（`compile_environment.go:311`），不含 categories / elements / AllowedEffects / ClientPackageKey；`authorityDigest` 也不含 Visual；编译期不校验 `Visual.Digest`。业务自己填 digest 时保护是否成立取决于业务怎么算，仓内没有内容摘要函数。建议提供 `VisualCatalog` 的内容摘要并在编译期校验，或改注释说明 digest 由业务负责。
- **O22 summon 进程的 `duration_ticks` 不校验也不使用**。`compile_motion.go:118-123` 对 summon 提前返回，负数、0 都编译通过；运行期实体进程用 spawn 的 `duration_ticks`（`process_owned.go:24-27` 只在 spawn 时长 ≤0 时才用模板值，而 spawn 时长编译期要求为正），所以无运行后果。建议拒绝或在文档写明无效。
- **O23 skillsync Health 与 ExportMetrics 取两次快照**。`Coordinator.Metrics()` 与 `outbox.Metrics()` 分两次加锁读取，Health 的原因判断可能基于略有先后的两份数据；只影响监控读数，不登记。
- **O24 `relation` filter 的取值不是封闭集合**。`relation` 的 `value` 原样传给 Host（MemoryHost 比较 `MemoryEntity.Relation` 字符串），编译期不查；ability 的 owner_relations 是 self / ally / enemy 封闭集合，而 select 的 relation 没有对应的 catalog。写错时 filter 静默全部不匹配。是否定为封闭集合需要维护者决定。
- **O25 shield 的 `duration_ticks` 两个参考 Host 口径不同**。MemoryHost 要求正数（否则 `ErrHostContractViolation`），combatcomponent 的 `applyShield` 忽略时长；编译期不查。需要先定 shield 是否有时长语义，未纳入 NC-215。
- **O26 宽口径下的 Host 错误**。性质测试只把 ErrProgramInvariant 与 panic 当失败；宽口径探针（记录全部非输入类错误）找出的 NC-214 / NC-215 已登记。其余宽口径命中是输入形状不匹配、资源不足等预期结果。
- **O1～O20（前三批）与 NC-151 方向 A**：本批不改这些行为，等维护者决定。NC-213 采用与 NC-151 相同的方向 B（编译期 fail-closed），实现方向 A（chain 间隔 / 叠层）同样留给维护者。

## 4. 编译 ⇒ 可执行的性质测试：评估与落地

结论：**能用现有 fixture + 变异实现，已补为正式测试** `skill/compile_mutation_property_test.go`。

- 种子：`testdata` 的 36 个 fixture（复用 acceptance 的驱动配置）加 6 个内联种子，补上 fixture 没有覆盖的效果：shield、attribute_modifier、add / clear_memory、knockback / pull / stop_movement、modify_process，以及 NC-211 的原触发（施法先结束的 area）。
- 变异：删除对象键、数值改写（0 / ±1 / 2 / 7 / 100 / 100000 / 原值 +1 / ×3）、同名键的词表替换（从全部种子收集）外加 5 个不存在的名字、布尔翻转、数组删首 / 复制尾；`SKILL_MUTATION_FULL=1` 时加跨种子子树移植（flow 对象互换、同名键对象互换，并插入 finish / wait）。
- 驱动：编译无 error 的变异在 MemoryHost 上用 5 种方式施法（配置输入、实体输入、全输入、tick 2 Cancel、tick 1 Interrupt + 两个输入端口），tick 5 Release，推进到 40 tick 并 Checkpoint；被动技能推进到 20 tick。只把 ErrProgramInvariant 与 panic 当作失败（输入不匹配、Host 拒绝、资源不足是变异后定义的正常结果）。
- 第二个性质（只编译）：变异后 gameplay digest 不变时，Program 的执行内容必须不变（去掉 name / description / visual 后 `reflect.DeepEqual`），守住 `program_digest.go` 漏字段。
- 规模（基线代码）：42 个种子，单点变异 23566 个、编译通过 7564 个；只用 fixture 加子树移植时 31226 个、编译通过 14062 个。单点模式本机约 18 秒（两个性质合计）；`-short` 下运行期抽样 1/5。
- 修前结果：`charge_projectile.json drop $.memory.charged` → `activate[0]: skill: immutable program invariant failed`（NC-210）；`seed.area_handoff` 基线与其变异 → `advance[0] 3: …invariant failed`（NC-211）。宽口径（记录全部非输入类错误）另外找出 NC-214 / NC-215，但那些不是 ErrProgramInvariant，正式性质不覆盖，以各自的回归钉住。
- 局限：变异只覆盖种子已有结构附近的形状，多步组合（如“emit_leave_on_stop + 施法先结束 + leave 回调 finish”）需要显式种子；驱动只用 MemoryHost；不证明 Host 错误分支（O26）。新增效果 / 字段时应同时加种子。

## 5. 未审 / 未验证

- `compile_random.go`、`compile_snapshot.go`、`compile_temporal.go`、`compile_graph.go`、`compile_effect_result.go`、`compile_proc.go`、`compile_quantity.go` 本批只经性质测试覆盖（单点变异与移植），没有逐分支读。
- `lower.go` 只核对了名字查找与 process / effect lowering；quantity proof 与 random site 的 lowering 未逐行审。
- 没有接真实 Host（Mongo / Redis / NATS 不涉及）；combatcomponent 只读拒绝分支源码，性质测试只用 MemoryHost。Windows 未涉及。

## 6. 方向判断

“编译器接受的集合与 Runtime / Host 能执行的集合各自维护”本批第四次出现，而且换了形态：

| 批次 | 形态 | 编号 |
| --- | --- | --- |
| 第三批 | 事件 / 字段只编译不执行；tick 非负规则散落；字段名匹配两套规则 | NC-150 / 151 / 152 |
| 本批 | 名字解析用 map 零值兜底（memory 槽位 0、catalog handle 0） | NC-210 / 214 |
| 本批 | 字段只编译不传 Host | NC-213 |
| 本批 | Host 都拒绝的取值编译期不拒绝 | NC-215 |
| 本批 | 编译器允许的控制流在某个 Runtime 状态下是不变量失败 | NC-211 |
| 本批 | catalog 自身的唯一性只查了一半 | NC-212 |

判断：根因是**前提**问题，不是某个 pass 漏写一行。编译器的“静态证明”没有一个机械的对照物：lower 对查不到的名字静默给零值，Runtime 与 Host 各自再判一遍，三处规则谁也不知道谁。逐条补规则（本批的做法）能收敛已知形状，但下一个新效果 / 新字段默认又是“只编译”。建议方向（代价递增）：

1. 本批做的：补齐名字 / 取值规则、拒绝不执行的字段，并落地变异性质测试作为护栏（新增 ErrProgramInvariant 会在 CI 的普通 `go test` 里变红）。
2. lower 不再用 map 零值：查找失败一律 panic（“编译器放过了未知名字”是编译器缺陷），让遗漏在任何测试里立刻暴露，而不是变成 handle 0。代价小，但需要先确认所有查找前都有检查（本批已核对 lower 的全部查找）。
3. 把 Host 的取值约束（modifier operation、resource operation、时长）做成 Host 声明的能力表（像 motion catalog 那样随环境下发），编译器按表校验；参考 Host 用同一张表做运行期拒绝。这样“编译接受 ⇒ Host 接受”由同一份数据保证。代价：环境格式与 authority digest 变化，需要维护者决定。

## 7. 停点与下一批入口

已审：编译器剩余 pass（authority、environment、owned_entity、status、visual、motion 后半）、lower 的全部名字查找、program_digest、presentation / presentation_assets、skillsync observability / schema；性质测试落地。N09 剩余入口：

1. 第 5 节列出的 pass 逐分支审（random / snapshot / temporal / graph / effect_result / proc / quantity）；
2. 维护者决定：NC-151 / NC-213 方向 A、方向判断第 2 / 3 条、O21～O25，以及前三批的 O1～O20；
3. 性质测试的种子随新效果补充；宽口径（Host 错误）是否纳入正式性质，取决于方向判断第 3 条。

## 8. 修复与验证（审查提交之后追加）

审查提交 `caf9837e`（`docs(review)`）之后按授权修复，单独一笔 `fix(skill,skillsync)`。每条一个修复单元：[NC-210](../bugfix/RR-20261005-NC-210.md)、[NC-211](../bugfix/RR-20261005-NC-211.md)、[NC-212](../bugfix/RR-20261005-NC-212.md)、[NC-213](../bugfix/RR-20261005-NC-213.md)、[NC-214](../bugfix/RR-20261005-NC-214.md)、[NC-215](../bugfix/RR-20261005-NC-215.md)、[NC-216](../bugfix/RR-20261005-NC-216.md)。NC-213 采用方向 B（只接受默认值），方向 A 留给维护者；NC-211 采用方向 A（Runtime 处理移交后的 finish）。

| 命令（`GOWORK=off`，模块根） | 结果 |
| --- | --- |
| 新增正式用例修前：`compile_runtime_agreement_promises_test.go`（NC-210 5、NC-212 1、NC-213 4、NC-214 8、NC-215 7）、`area_handoff_finish_promises_test.go`（NC-211 2）、`skillsync/schema_negotiation_promises_test.go`（NC-216 1）、`compile_mutation_property_test.go`（性质 1） | 全部 FAIL，原文见各修复记录 |
| 同一组修前控制：`TestDeclaredMemoryEffectsWriteTheirOwnSlot`、`TestCatalogConsistentEffectsStillCompileAndRun`、`TestLiveAreaFinishStillFinishesTheCast`、NC-213 的两个默认值子用例、`TestCompiledMutationsChangeDigestWhenProgramChanges` | 修前修后都 ok |
| 修后全部；性质测试单点模式 23566 变异 / 7294 编译并施法，`SKILL_MUTATION_FULL=1` 47188 / 18652 | ok |
| `gofmt -l skill` | 空 |
| `go vet ./skill/... && go test -race -count=3 ./skill/...` | skill / combat / combatcomponent / skillcompose / skillsync 全部 ok（race 下性质测试 1/40 抽样；首次未抽样时 skill 包 race×3 超过 10 分钟默认超时，已改） |
| `skill/examples`：`go build ./...`、`go run ./fireball`；`skill/integration/sync-e2e`：`go test -count=1 ./...` | 通过 |
| `go build ./... && go vet ./...`；`go test -count=1 .`；`go test -count=1 ./codegen/internal/roost -run Skill` | 通过 |

既有测试的输入随规则收紧调整（断言不变）：`compile_lifetime_test.go` 的 parallel finish 与 `compile_budget_test.go` 的 repeat 超限原来用未声明的 `clear_memory x` 当填充，现在声明 `x`；NC-152 的控制 `TestCompileAcceptsZeroTicks` 里 add_status 时长从 0 改为 1（0 现在由 NC-215 拒绝，wait 0 / repeat 间隔 0 的控制不变）。

组合契约复核：新诊断只经 `Compile` 返回——game-demo 的 `CompileAll`（`service.go.tmpl:61`、`skill_catalog.go.tmpl:46`）把 error 诊断当启动失败，本来就处理；它们用的 `DefaultCompileEnvironment` 没有重复 key、生成的骨架与 fireball 零诊断（`TestGeneratedSkillDefinitionsCompileWithoutDiagnostics` 修后 ok）。NC-211 只改 Runtime 对“移交后 finish”的处理：施法存活时的路径（`ownerLive`）与此前完全相同，既有 area 用例全部通过；移交后的进程在同一次 `advanceOwnedProcesses` 内停止并从 `ownedProcesses` 删除，checkpoint 不会看到中间标记。NC-216 只影响 `NegotiateSchema`，`NewApplier` / `admit` 不调用它。没有改生成形状（模板、生成器未动），未生成 game-demo 工程；外部依赖（Mongo / Redis / NATS）本批没有用到。T-258。

