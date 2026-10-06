# N09 skill 第三批：Parse + Compile 拒绝路径（game-demo 启动链）、skillcompose、process motion / area / numeric、VisualPlanCache

2026-10-05，基线 `45d4bc1c`（origin/main；skill 自第二批修复 `f37a94e3` 起未变），独立 worktree 分支 `revn09c`，NC 编号段 150～159（本批用 150～154）。上一批：[第一批](REVIEW-2026-10-05-n09-batch1.md) · [第二批](REVIEW-2026-10-05-n09-batch2.md)。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。

图谱：项目 `Users-whb-roost-roost-core`，generation 2026-09-30T11:28:30Z。本批引用的 `skill/parse*.go`、`compile*.go`、`diagnostic.go`、`process_motion.go`、`process_area.go`、`process_numeric.go`、`presentation_asset_cache.go`、`skill/skillcompose/*`、`codegen/internal/roost/add_skill.go` coverage 为 `no_recorded_issue / metadata_match`；这些文件在 09-30 之后没有提交，但第一、二批改过的 `runtime*.go` / `scheduler.go` 晚于 generation，一律按当前源码读取。`phaseRootOperation`、`phaseTimeoutTask`、`processNumericBoundValue` 的全部调用点用 `rg` 补证（图谱只用于定位）。

## 1. 本批审过的场景

| 编号 | 场景 | 读过的源码 / 跑过的 | 结论 |
| --- | --- | --- | --- |
| P1 | game-demo 启动链：`service.go.tmpl` Init → `skills.CompileAll` → `skill.Parse` + `skill.Compile`；`HandleSkillCatalog` 的懒编译 | `demo/internal/service/game/service.go.tmpl:51-70`、`demo/game/controllers/player/skill_catalog.go.tmpl`、`codegen/internal/roost/add_skill.go` 全文 | 解析错误、error diagnostic、重复 ID、无 Program 都在 Init 返回错误、进程不起；错误信息带文件名，compile 错误带 JSON 路径（只报第一条 error） |
| P2 | `roost add skill` 生成的骨架与 game-demo 的 `fireball.json` 是否合法 | 用当前 CLI 生成项目（`project new` + `add skill`）、骨架与 fireball 内容逐字过 Parse + Compile | 合法、零诊断。`project new` 把名字规整为 snake（`game_` → `game`），`add` 名字经 `toSnake` 不会产生 `__`；但仓内没有任何测试对生成的定义跑 Parse + Compile（O19） |
| P3 | Parse 拒绝路径：限额、重复键、尾随数据、未知字段、类型错误、语法错误 | `parse.go`、`parse_duplicate.go` 全文；`wire_flow.go:1-120`、`value.go:70-175`、`wire_visual.go`；12 种坏输入探针 | **NC-150**：大小写变体绕过重复键 / 未知字段拒绝；错误路径前缀不完整（O13） |
| P4 | Compile 的 error diagnostic：标识符、phase 图、fallthrough、类型、预算、形状 | `compile.go`、`compile_normalize.go:1-210`、`compile_shape.go`、`compile_lifetime.go`、`compile_budget.go:180-199`、`compile_typecheck.go:190-260, 465-577`；37 fixture × 每个 tick 字段置 -1 的扫描 | **NC-151**：recast / timeout 事件与 `timeout_ticks` 只有编译、没有运行；**NC-152**：tick 非负规则分散、五处缺口 |
| P5 | skillcompose：BuildContract → ValidateContract / CanonicalContract → DeriveContractPromptView → ExtractProfile → ValidateCandidate | `skill/skillcompose/` 15 个文件全文 | **NC-154**：空 / 重复 source 无诊断；因果图与来源校验的设计局限见 O14 |
| P6 | process_area：成员进出、tick、停止时 leave、host 错误清理、轮换有界 | `process_area.go` 全文、`area_test.go:80-215` | 无确认缺陷；`$event.enter_count` 恒为 1（O15） |
| P7 | process_motion：frame → steering → trajectory → offsets → collision → carry → completion → signals 各阶段；boomerang 暂停 / 返回；CORDIC | `process_motion.go` 全文、`motion_test.go:195-320` | 无确认缺陷；reflect / pierce 计数语义（O16） |
| P8 | process_numeric：属性绑定唯一性（compile 与 runtime 两份 binding 表）、track 插值与溢出、modify_process | `process_numeric.go` 全文、`compile_typecheck.go:520-577` | 编译期 `processPropertyBindingCount` 与运行期 `processNumericBindingCount` / `processNumericBinding` 三份表逐分支对照一致；Host 收到的 numeric 快照与运动实际取值可能不同源（O17） |
| P9 | VisualPlanCache：plan / asset 两级引用、fallback alias、空闲淘汰、目录失效、取消 | `presentation_asset_cache.go` 全文、`presentation_asset_cache_test.go`；并发探针 | **NC-153**：共享加载用第一个调用者的 ctx；O18 |

探针 `zz_probe_n09c*_test.go`（skill、skillcompose 两包）跑完删除，结论落为正式回归（修复时）。

## 2. 确认缺陷

| 编号 | 等级 | 一句话 |
| --- | --- | --- |
| [NC-150](../bug/RR-20261005-NC-150.md) | P2 | 严格 Parse 被大小写不敏感匹配绕过：`id` + `ID` 同时接受、后者生效，`Cooldown_Ticks` 当作 `cooldown_ticks` |
| [NC-151](../bug/RR-20261005-NC-151.md) | P2 | phase 的 `on.recast` / `on.timeout` 与 `timeout_ticks` 只编译不执行；`timeout_ticks > 0` 让 fallthrough 通过编译、tap 技能每次施法 ErrProgramInvariant |
| [NC-152](../bug/RR-20261005-NC-152.md) | P3 | cooldown / phase timeout / repeat interval / add_status 时长 / chain hop 间隔的负值能编译；负 wait 的诊断落在 `$` |
| [NC-153](../bug/RR-20261005-NC-153.md) | P3 | VisualPlanCache 共享加载用第一个调用者的 ctx，它取消后其他等待者也失败 |
| [NC-154](../bug/RR-20261005-NC-154.md) | P3 | ValidateCandidate 对空 / 重复 source 判 invalid 却无诊断 |

## 3. 观察与设计建议（不登记 RR，不改行为）

- **O13 Parse 错误的路径前缀不完整**。Parse 的位置是各 `decode*` 手工拼的前缀：`decodePhaseEvents`（`parse.go:287-292`）不带事件名，`decodeFlows` 不带 `steps` / `branches`，于是 `phases: [0].on: [1]: json: cannot unmarshal string into Go struct field .ticks` 看不出是哪个事件、哪一层。Compile 诊断有完整 JSON 路径（`$.phases[0].on.enter.phase`），两套位置格式不同。没有文档承诺 Parse 的路径格式，记为建议：Parse 错误改用与诊断相同的 `$…` 路径。
- **O14 skillcompose 的“因果”与“来源”都是调用方声明的**。`GraphFromProfile`（`graph.go:24-39`）把 operation 列表连成一条链，任何一个 damage / heal / spawn 都算“到达 sink”，`OperationView.Children` 被 `ExtractProfile` 丢弃；sink 不含 status / shield / resource 等效果，纯 buff 技能的原样拷贝也会 `CAUSAL_DISCONNECTED`。`ExtractProfile` 产出的 profile 的 `Sources` / `FeatureOrigins` 指向它自己，不可能通过 `ValidateCandidate`，调用方必须手填来源，校验器无从核实。`DerivePromptView` 不经合同 digest 校验仍是导出函数（doc.go 要求只暴露 `DeriveContractPromptView`）；`Catalog` / `FeatureDescriptor` / `Metrics.Bounded` 仓内无调用方。skillcompose 仓内无正式调用方，按设计局限记录，等维护者决定是补成真正的图分析还是收窄 API。
- **O15 area 的 `$event.enter_count` 恒为 1**。离开即删成员状态（`process_area.go:28-32`，为了内存有界，`TestAreaMembershipRotatingMembersRemainBounded` 钉住），再次进入从 1 计；字段名暗示累计进入次数。建议文档写明，或改名 / 删除。（10-06 第十二轮决定：保持，写进作者文档，[记录](../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)）
- **O16 reflect / pierce 的计数语义**。`max_reflects: N` 实际反弹 N−1 次，第 N 次碰撞翻转方向、发 transition 并在同一 tick 结束（`process_motion.go:187-201`）；pierce 同理，且 `max_reflects: 1` 产生一次“反弹即结束”。`TestMotionCollisionAndCompletionAreBounded` 钉住“消耗预算后停止”，属有意设计，建议在 schema 文档写明“N 是碰撞预算，不是反弹次数”。（10-06 第十二轮决定：保持，写进作者文档，[记录](../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)）
- **O17 numeric 快照与运动取值不同源**。未被 track 绑定的属性在 `ProcessStepCommand.Numeric` 里报告的是进程初始化时求值的 base（`process_numeric.go:217-240`），而运动每 tick 重新求值表达式（`resolveProcessNumeric` → `evalInt`，`process_motion.go:416-421`）；speed 等用动态表达式时 Host 看到的速度与实际位移不一致。parabola speed、tracking turn rate、boomerang return speed、collision force 的 base 恒为 0（Host 自管）。未登记：Host 对 `Numeric` 的契约没有文档。（10-06 第十二轮决定：保持，写进作者文档，[记录](../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)）
- **O18 取消的 Acquire 不触发空闲淘汰**。`cancelAcquireReservation` 故意不淘汰（`presentation_asset_cache.go:162-163` 注释），最后一个引用因取消离开时条目以 idle 状态留在缓存，直到下一次 `Release` 才按 `MaxIdlePlans` 淘汰；`MaxIdlePlans: 0` 时资源会被保留到下一次释放。
- **O19 生成的技能定义没有 Parse + Compile 测试**。`TestAddSkillUsesStablePackageAndNeutralDefinition` 只做字符串包含检查，game-demo 的 `fireball.json` 只在生成工程启动和机器人里被编译；skill 包改编译规则时 codegen 测试看不见。修复时加控制用例。
- **O20 checkpoint 仍接受 `phase_timeout` 任务**。live 代码不产生它（NC-151），恢复出来执行时只会 `ErrProgramInvariant`；建议在决定 NC-151 的方向 A / B 后一并处理（实现或在恢复时拒绝）。（维护者 B3④ 保持方向 B；10-06 收尾第 3 批改为恢复时拒绝：[RR-20261006-03](../bugfix/RR-20261006-03.md)）
- **O1～O12（前两批）**：本批没有找到新的触发路径，不改行为，等维护者决定。

## 4. 未审 / 未验证

- 编译器其余 pass（`compile_authority.go`、`compile_environment.go`、`compile_capability.go`、`compile_owned_entity.go`、`compile_status.go`、`compile_visual.go`、`compile_random.go`、`compile_snapshot.go`、`compile_temporal.go`、`compile_memory.go`、`compile_graph.go`、`compile_effect_result.go`、`compile_motion.go` 后半）只按拒绝路径抽读，没有逐分支审；`lower.go`、`program_digest.go` 未审。
- `presentation_assets.go`（`ResolvePresentationPlan`）、`presentation.go` 本批只作为 VisualPlanCache 的输入读接口。
- 没有在生成工程里实际启动 game 服（Init 的 fail-fast 由源码与生成产物判定，P2 用 CLI 生成后对定义单独 Parse + Compile）；外部依赖未使用。

## 5. 方向判断

“同一事实走几条路径，每条路径各写一份规则”本批第三次出现，形态是**编译器与 Runtime 之间**：

- NC-151：phase 事件集合在编译侧（parse / normalize / lower / lifetime）有一份，Runtime 的派发点是另一份，两份不一致且没有任何东西把它们连起来；`timeout_ticks` 在编译侧是 fallthrough 的出口，在 Runtime 侧不存在。
- NC-152：“作者写的 tick 必须非负”分散在 shape / typecheck / motion / budget 四个 pass 里按字段各写一条，新字段默认没有规则；负值溢出成 MaxInt64 后由预算 pass 在 `$` 报错，等于靠副作用兜底。
- NC-150：重复键检查与字段匹配是两套比较规则（字节 vs 大小写折叠）。
- 第一、二批：终止路径收尾（NC-110～112）、终态保留（NC-117）、可见性三条下发路径（NC-114 / 115）。

判断：根因不是某个分支漏写，而是 skill 的“编译器证明 Program 可执行”这一前提没有机械保证——编译器接受的形状集合与 Runtime 能执行的集合各自维护。建议方向（代价递增）：

1. 本批做的最小收敛：拒绝 Runtime 不派发的事件、把 tick 非负规则集中到 shape pass 一处、decode 层统一做大小写精确匹配；
2. 让 lower 产出的 root 事件集合与 Runtime 派发表来自同一个常量表（编译期遇到表外事件即错误），新增事件必须同时加派发；
3. 给每个 fixture 跑“编译通过 ⇒ 在 MemoryHost 上可启动且不返回 ErrProgramInvariant”的性质测试（现有 acceptance 只覆盖 37 个手写形状），把 `ErrProgramInvariant` 视为编译器缺陷的信号。

## 6. 停点与下一批入口

已审：数据/属性方向里 game-demo 启动链实际经过的 Parse + Compile 拒绝路径、skillcompose 全部、process motion / area / numeric、VisualPlanCache。N09 下一批（第四批）建议：

1. 编译器其余 pass 的逐分支审（§4 列表），优先 `compile_motion.go` 后半、`compile_owned_entity.go`、`compile_status.go`，每个 pass 对照 Runtime 的执行点核对“编译接受 ⇒ 运行可执行”；
2. `presentation_assets.go` / `presentation.go`、`observability.go`、`schema.go` 迁移图；
3. 维护者决定 NC-151 方向 A（实现 phase timeout / recast）或保持 B，以及 O1 / O2 / O5 / O12 / O14 后回到对应子包。

## 7. 修复与验证（审查提交之后追加）

审查提交 `7a874663`（`docs(review)`）之后按授权修复，单独一笔 `fix(skill,skillcompose)`。每条一个修复单元：[NC-150](../bugfix/RR-20261005-NC-150.md)、[NC-151](../bugfix/RR-20261005-NC-151.md)、[NC-152](../bugfix/RR-20261005-NC-152.md)、[NC-153](../bugfix/RR-20261005-NC-153.md)、[NC-154](../bugfix/RR-20261005-NC-154.md)。NC-151 采用方向 B（编译期 fail-closed），方向 A（实现 phase 计时与 recast）留给维护者。

| 命令（`GOWORK=off`，模块根） | 结果 |
| --- | --- |
| 新增 6 个正式用例文件修前：NC-150 10 子用例、NC-151 4 条、NC-152 6 子用例、NC-153 2 条、NC-154 3 子用例 | 全部 FAIL，原文见各修复记录 |
| 同一组修前控制：`TestParseKeepsNameKeyedMapsAndCanonicalKeys`、`TestCompileAcceptsZeroTicks`、`TestVisualPlanCacheWaiterSeesRealLoadFailureAndOwnCancellation`、`TestGeneratedSkillDefinitionsCompileWithoutDiagnostics` | 修前修后都 ok |
| 修后全部 | ok |
| `gofmt -l skill` | 空 |
| `go vet ./skill/... && go test -race -count=3 ./skill/...` | skill / combat / combatcomponent / skillcompose / skillsync 全部 ok |
| `skill/examples`：`go build ./...`、`go run ./fireball`；`skill/integration/sync-e2e`：`go test -count=1 ./...` | 通过 |
| `go build ./... && go vet ./...`；`go test -count=1 .`；`go test -count=1 ./codegen/internal/roost -run Skill` | 通过 |

组合契约复核：NC-150 的新错误只经 `Parse` / `ParseGenerated` / `RuntimeValue.UnmarshalJSON` 返回——game-demo 的 CompileAll 把它当启动失败（本来就处理解析错误），checkpoint 恢复与 skillsync Applier 的 RuntimeValue JSON 由 Go `Marshal` 产生、字段名逐字一致（skillsync / combatcomponent / checkpoint 往返用例 race×3 通过）。NC-151 / 152 的新诊断走既有 CompileAll 分支；首次出现的 warning 进入 `Catalog.Diagnostics`，game-demo 机器人断言 `Warnings == 0`，生成的骨架与 fireball 均为 0（控制用例钉住）。NC-153 的重试只在别人的取消之后发生，失败条目本来就删除，plan 层用例断言释放后 asset 引用归零。测试辅助里 5 处依赖 `timeout_ticks` 豁免的 JSON 改为 `wait + finish` 结尾，`recast_combo.json` 删除（README / 实现指南的 fixture 数 37 → 36）。没有改生成形状（模板、生成器未动），未跑生成工程编译；外部依赖（Mongo / Redis / NATS）本批没有用到（v1.20.1 发版故障矩阵运行期间未触碰共享环境）。T-248。
