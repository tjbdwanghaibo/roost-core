# Skill 当前实现学习手册

阅读准备：先了解 [Skill 的职责与例子](../framework/guide/08-skill.md)；不熟悉DAO、事务、Host等概念时，从 [入门手册](../GETTING-STARTED.md) 开始。本篇保留具体接口与示例，适合在接入或定位问题时查阅。

> 当前升级口径（2026-10-08）：单仓单模块，Runtime checkpoint=10；旧版拒绝。旧文中的版本号与三仓发布/可灰度说法仅适用于当时的改名阶段，本轮先停旧再起新。见 [当前架构与迁移](../framework/guide/08-skill.md)。

本文对应当前稳定 `skill` 包实现。目标是帮助新读者从一份
JSON 技能定义，顺着真实调用链走到确定性的世界交互，而不是先被大量
`wire_*`、`ir_*`、`program_*` 文件淹没。

## 1. 先建立全局心智模型

Skill 是一个受限的、可验证的技能 DSL。它不直接执行 JSON；每次执行都必须
经过四层边界：

```text
Skill JSON
  -> Parse（严格 Wire 定义）
  -> Compile（IR、静态证明、诊断）
  -> Lower（不可变 Program）
  -> Runtime（确定性调度）
  -> Host（唯一允许读写世界的接口）
```

外部系统不应绕过这些边界：

| 需求 | 正确入口 | 不应做的事 |
| --- | --- | --- |
| 接收技能 JSON | `Parse` | 直接反序列化到运行时 Program |
| 构建可执行技能 | `Compile` | 在 Runtime 内解释 JSON 或字符串键 |
| 执行主动技能 | `Runtime.Activate` | 直接调用 Host 写世界 |
| 执行被动技能 | `Runtime.ActivatePassive` | 把被动技能当主动技能施放 |
| 给 UI/组合系统提供信息 | `Inspect*` | 访问 Program 私有执行字段 |
| 修改游戏世界 | `Host` 的查询/命令接口 | 在编译器或 Inspector 修改世界 |

这套分层同时承担三个职责：解析边界保证输入封闭；编译器保证定义可执行且有界；
Runtime 只消费已证明的 Program，并通过 Host 保持世界权威性。

## 2. 推荐阅读顺序（约 3～5 小时）

### 第一轮：一次最小施放（30 分钟）

从 [simple_damage.json](../../skill/testdata/simple_damage.json) 开始，只做一件事：
输入目标，对目标造成物理伤害，然后结束。

按以下顺序跳转：

1. [parse.go](../../skill/parse.go) 的 `Parse`：确认 schema、严格对象解码与顶层定义。
2. [lower.go](../../skill/lower.go) 的 `Compile`：它是公开的编译入口。
3. [runtime.go](../../skill/runtime.go) 的 `NewRuntime`、`Activate` 和 `startLocked`：理解 Cast 的创建。
4. [executor.go](../../skill/executor.go)：查看一个 Program operation 如何被执行。
5. [memory_host_effect.go](../../skill/memory_host_effect.go)：以 `MemoryHost` 为例观察伤害如何真正提交。
6. [acceptance_test.go](../../skill/acceptance_test.go)：该测试把 fixture 走完
   Parse → Compile → Inspect → Activate → Advance → 终态检查。

完成这一轮后，应能回答：伤害量从 JSON 中何时成为强类型值？为什么 Runtime 不需要
再解析 `"physical"`？为什么世界写入最终只发生在 Host？

### 第二轮：编译器如何拒绝不安全定义（60 分钟）

阅读 [compile.go](../../skill/compile.go)。`compileToArtifactsInternal`
列出的 Pass 顺序就是当前编译语义的主目录：

1. `normalize`：Wire Definition 转为封闭 IR，并记录源路径。
2. `shape`：校验 Flow、Effect、Select、Spawn 的基本结构。
3. `authority_capability`：把属性、资源、状态、模板、标签等字符串解析到
   `CompileEnvironment` 提供的权威 Handle，并验证能力目录。
4. `gameplay_tags`、`input_state`、`temporal`：验证标签类别、施放输入、
   Persistent/Shared State 与时间快照。
5. `type_snapshot`、`effect_result_scope`：检查值类型、
   单位/快照点、Effect Result 的作用域与字段。
6. `graph`、`memory`、`lifetime_ownership`：证明 phase/flow 可达、Memory 已初始化、
   生命周期和拥有关系有界。
7. `motion`、`event_proc`、`identity_random`、`budget`：限制过程运动、被动递归、
   身份/随机位点以及最坏执行预算。
8. `visual`：验证表现资产只引用目录中允许的类别与主题。
9. `lower`：仅标记全部静态证明已完成；真正的 Program lowering 在公开 `Compile` 中发生。

不要把这些 Pass 当作可以随意交换的 lint 集合。后续 Pass 假定前置 Pass 已将名称、
类型和作用域收窄，例如 `motion` 依赖已解析的能力和类型信息，`budget` 依赖图与过程
调用边界。

引用在哪里能读，不在各个 Pass 里各写一份：[eval_contexts.go](../../skill/eval_contexts.go) 的求值上下文表
（施法流程、memory 默认值、cast_start / phase_start / spawn_start 采样、衍生物（Spawn）每一步、衍生物回调、状态默认值 ×
`$input.*` / `$memory.*` / `$local.*` / `$caster` / `$cast.*` / `$owner` / `$event.*` …）是唯一来源：`type_snapshot`
按表生成每个位点的作用域，Runtime 求值时查同一张表，表外引用报 `ErrReferenceOutOfContext`。给定义加新的值位点或新的
求值上下文时先改表，`eval_contexts_table_test.go` 会要求每个格子补正例 / 反例（引用表与快照点表都逐格守）。
格子只有“可用”与“不可用”两种：值会随衍生物移交或读写位置变化的格子一律不可用（O33，维护者第七轮决定），不可用格的说明写原因与“改用 …”，诊断原样带出。作者侧的规则与改写对照见 [施法语义 · 引用在哪里能读](skill-casting-and-combat.md#引用在哪里能读求值上下文)。
[方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。

建议配合阅读：

- `compile_*_test.go`：静态拒绝用例；
- [compile_environment.go](../../skill/compile_environment.go)：默认目录和限制；
- [canonical_definition.go](../../skill/canonical_definition.go)：源定义的稳定摘要；
- [diagnostic.go](../../skill/diagnostic.go)：诊断代码和稳定排序约定。

练习：把 `simple_damage.json` 的 `damage_type` 改成不存在的键，再运行对应测试或
验收测试，观察诊断产生在 authority/capability 边界，而不是在运行时。

### 第三轮：从 IR 到 Program（40 分钟）

Wire 层与 IR 层回答“用户写了什么”；Program 层回答“Runtime 可以只按索引执行什么”。

阅读顺序如下：

1. `wire_*.go`：JSON 允许的封闭语法。尤其是 `wire_definition.go`、
   `wire_flow.go`、`wire_effect.go`、`wire_input.go`。
2. `ir*.go` 和 `compile_normalize.go`：标准化后的强类型中间表示。
3. [lower.go](../../skill/lower.go)：把名称解析成 Handle、MemoryIndex、
   LocalIndex、OperationIndex，并收集 snapshots、random sites、event plans。
4. `program_*.go`：Program 内部的执行指令和索引布局。
5. [inspect.go](../../skill/inspect.go)：唯一推荐给外部消费者的 Program
   观察面。

关键不变量：Program 是编译结果，Runtime 不应重新解析 DSL 字段或回头查询源 JSON。
所有可变状态都属于 cast、scheduler 或 Host；Program 自身是跨施放共享的只读计划。

## 3. Runtime：把 Program 变成确定性行为

Runtime 的核心类型位于 [runtime.go](../../skill/runtime.go)：

- `RuntimeOptions`：匹配种子、任务/trace 限制等运行时配置；
- `CastInput`：主动施放输入的统一载体；
- `castInstance`：每次施放自己的 phase、memory、可见 world revision、状态；
- `Runtime`：casts、cooldown、scheduler、被动事件账本和 trace 缓冲。

一次主动施放的时间线如下：

```text
Activate
  -> Start / startLocked
  -> 输入标准化与 Cast Window 准备
  -> 进入当前 phase 的 enter flow
  -> executor 执行 operation，必要时向 scheduler 注册后续任务
  -> Advance(tick) 以稳定顺序消费到期任务
  -> Finish / Cancel / Release，清理 spawn 和 cast 状态
```

重点文件：

- `runtime_input.go`：位置、目标、双点、拖拽、路径输入的运行时校验与归一化；
- [runtime_cast_window.go](../../skill/runtime_cast_window.go)：windup、
  commit、recovery、`Cancel` 和 `Release`；
- [scheduler.go](../../skill/scheduler.go)：`Advance`、稳定排序和任务执行；
- `runtime_dispatch.go`、`runtime_event.go`：phase 事件与 spawn 信号如何进入 flow；
- [runtime_proc.go](../../skill/runtime_proc.go)：`ActivatePassive` 与
  递归/同根事件保护；
- `runtime_state.go`、`runtime_ability.go`、`runtime_temporal.go`：状态、能力控制、
  快照等专用操作的运行时桥接。

### 一个重要区分：主动与被动

主动技能从 `Activate(program, CastInput)` 开始。被动技能没有用户施放输入，应由事件
驱动 `ActivatePassive(program, EventContext)` 入队，再由 `Advance` 执行。

当前 fixture 验收已经显式区分二者：被动 fixture 使用 `ActivatePassive` 并给出事件根、
owner、source、target（以及需要时的 `Result: "kill"`），主动 fixture 才检查 Cast 的
终态。这是新增被动 fixture 时最容易遗漏的一点。

## 4. Host：世界权威边界

`Host` 不是测试替身专用接口，而是 Runtime 与游戏世界之间的正式边界。Runtime 通过它：

- 读取属性、位置和实体信息；
- 执行选择查询；
- 支付成本；
- 提交伤害、治疗、状态、生成、移动等 Effect；
- 推进/停止 Spawn；
- 读写持久或共享 State；
- 读取事件流和世界 revision。

建议先读 [host.go](../../skill/host.go)，再读 `host_*.go` 中的命令和
结果类型，最后读 `memory_host*.go`。`MemoryHost` 是可重复的参考实现和测试世界，
不是生产服务器的替代品。

World revision 是关键防线：Runtime 的 query/command 会携带期望 revision，Host 负责
拒绝已失效读取或提交。因而不要缓存 Host 返回的可变对象，再在后续 tick 假设其仍然有效。

**Host 能力表**（B3 ③，2026-10-07，[方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)）：Host 用
`HostCapabilityProvider` 声明能读 / 支持的取值（属性、资源、衍生物 kind、motion 步骤、只交给 Host 的数值字段、资源与
修正 operation、召唤物），环境的 `Host` 段与它对齐。三处各管一件事：

- 编译器（`compile_host_capability.go`）从 IR 收集需求，只经 `HostCapabilityTableOf` 读表，表外报
  `HOST_CAPABILITY_MISSING`；需求排序去重后写进 `Program.hostRequirements`。新增会向 Host 要新取值的 DSL 构造时，
  在这里加收集、在 `hostCapabilityColumns` 加列（封闭集合只登记在那里）。
- Runtime（`runtime_host_capability.go`）在 Program 第一次使用时核对需求在 Host 的表里。能力表是 `skill.Host` 接口的一部分（`HostCapabilities()`，收尾时并入，[RR-20261006-39](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/bugfix/RR-20261006-39.md)）：每个 Host 都必须声明，没有“没实现就跳过”的分支；包装别的 Host 的类型（`RecordingHost` / `ReplayHost` 的做法）转发被包装者的表，不要自己另写一份。空表表示什么都不支持。
- Host 对表外的属性 / 资源报错；`CheckHostCapabilities` 按声明逐项调用 Host。`MemoryHost` 声明全部能力（属性 /
  资源两列取配置的 catalog）；`combatcomponent.HostAdapter` 声明战斗那部分（可读属性、有映射的资源、资源
  operation，接了 `StatusBridge` 再加修正），运动 / 衍生物 / 召唤物由业务声明后 `MergeHostCapabilities`。

守卫：`TestNoHostCapabilitySkipBranch`（skill 源码里不许再对 `HostCapabilityProvider` 做类型断言）、`TestRuntimeAsksHostOnlyForCompiledRequirements`（Runtime 发给 Host 的取值都在需求里——改 Runtime 向 Host
发的东西时它会先红）、`TestCompilerConsultsTheTableForEveryRequirement`、`TestCompilerReadsHostCapabilitiesOnlyThroughTheTable`、
`TestHostOnlySpawnNumericFieldsMatchRuntime`。

**`StopSpawn` 必须幂等**（Host 契约，2026-10-06）：停一个已经停掉或 Host 不认识的衍生物要成功返回、不产生第二次
副作用（`MemoryHost` 返回当前 revision、不发事件）。只有衍生物在世界里确实还在运行时才返回错误。原因：宿主停止
失败时——不论来自哪个停止入口：施法失败或施法里的停止、衍生物启动失败的清理、tick 驱动的回收、`RemoveProgram`、
`Shutdown`（停止入口统一，2026-10-07）——Runtime 把衍生物标成 `stop_pending`，之后的 tick 按退避重试同一个停止
（`RuntimeOptions.SpawnStopRetryBackoff` 默认 4 tick、每次失败翻倍到最多 64 倍，`SpawnStopRetryLimit` 默认 10 次）；
`Shutdown` / `RemoveProgram` 对待停止的衍生物也会立即再停一次。宿主实际停掉了却报失败的，会再收到一次停止。
`Shutdown` 停不下的衍生物同样留成 `stop_pending` 并写进 checkpoint，宿主侧要准备好在 `Shutdown` 之后（继续推进或
恢复之后）再收到这些停止；回调与区域离开信号只在第一次停止请求时执行，重试只调 `StopSpawn`。到上限后 Runtime 不再自动重试，计
`skill.spawn.stop_retry_exhausted.total` 并写一条 Warn 日志，记录保留；待停止条目最多
`MaxStopPendingSpawns`（默认 256）条，超限时最早的已告警条目（没有就是最早仍在重试的）改为 `abandoned`：
Runtime 不再重试、不再请求停止它（`Shutdown` / `RemoveProgram` 也不再停），计 `skill.spawn.abandoned.total` 并写
Error 日志；宿主侧要靠比赛结束 / 程序移除（`RemoveOwnedEntitiesForMatchEnd` / `ByProgram`，照常调用）或自己的清理
收掉这类衍生物。记录保留到 `Advance` 末尾超出 `MaxAbandonedSpawns`（默认 1024）时才删（`skill.spawn.abandoned_pruned.total`）。
细节见
[施法语义](skill-casting-and-combat.md) 的“失败终态”、`skill/runtime_spawn_stop.go`（状态迁移表与停止入口清单）与
[停止入口统一方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md)。

## 5. 过程与高级能力的阅读地图

以下表按功能给出最小切入 fixture 与主要实现位置：

| 能力 | 先看 fixture | 主要代码 |
| --- | --- | --- |
| Cast policy/window | `toggle_aura`、`hold_beam`、`charge_projectile`、`ammo_burst`、`cast_window_interrupt` | `runtime_cast_policy.go`、`runtime_cast_window.go` |
| 输入约束 | `path_projectile`、`two_point_wall`、`portal_pair` | `wire_input.go`、`compile_input.go`、`runtime_input.go` |
| 运动/过程 | `carry_dash`、`tracking_boomerang`、`beam`、`projectile_area` | `spawn*.go`、`spawn_motion.go`、`compile_motion.go` |
| Area 成员事件 | `area_heal`、`area_membership`、`entity_scoped_aura` | `spawn_area.go`、`area_test.go` |
| 数值与快照 | `dynamic_numeric`、`attribute_scaling_snapshot` | `compile_quantity.go`、`compile_snapshot.go`、`runtime_eval.go` |
| 状态 | `status_modifier`、`status_cleanse`、`status_steal` | `compile_status.go`、`runtime_select.go`、`memory_host_status.go` |
| State/能力控制 | `persistent_mark`、`shared_state_combo`、`cooldown_refund`、`ability_disable` | `compile_state.go`、`runtime_state.go`、`runtime_ability.go` |
| Owned Entity | `owned_trap`、`owned_pet_command` | `compile_owned_entity.go`、`runtime_owned_entity.go`、`memory_host_owned_entity.go` |
| Temporal/Result | `temporal_rewind`、`effect_result_kill_branch` | `compile_temporal.go`、`runtime_temporal.go`、`runtime_effect_result.go` |
| Passive proc | `passive_counter`、`passive_proc_guard`、`ammo_on_kill` | `compile_proc.go`、`runtime_proc.go` |

所有 36 个 fixture 位于 [testdata](../../skill/testdata)。
`acceptance_test.go` 使用目录发现机制：新增 JSON 若没有明确的输入、推进 tick、release
或 passive 配置，测试会失败。因此 fixture 既是可运行示例，也是变更清单。

## 6. Inspector 与 skillcompose：只读的上层消费

不要让 UI、提示词生成或技能组合层了解 Runtime 私有 operation。它们应通过
`Inspect`、`InspectMetrics`、`InspectInputLayout`、`InspectSelections`、
`InspectEffectResults` 等只读视图消费 Program。

[skillcompose](../../skill/skillcompose) 展示了这一原则：

1. `profile_extract.go` 从 Inspector 提取 `SkillProfile`；
2. `contract_builder.go` 将多个 profile 和 caller policy 收紧为 composition contract；
3. `validator.go` 核对已授予的功能、预算，并检查操作摘要是否存在有效果的出口；摘要不包含控制流边，不能证明运行时因果可达；
4. `prompt_view.go` 只派生提示词需要的稳定投影。

阅读 `skillcompose` 时，若发现需要访问 Program 未公开字段，应优先新增一个稳定的
Inspector 视图，而不是打破包边界。

## 7. 可观测性、回放和提示词契约

### Trace

[trace.go](../../skill/trace.go) 维护有界、被动的 `TraceEvent` 缓冲。
执行路径只记录事件；调用方在安全的外部时机调用 `FlushTrace`，才把缓冲发送给
`TraceSink`。sink 失败不会反向影响游戏逻辑，也不会丢弃尚未成功发送的事件。

### Record / Replay

[replay.go](../../skill/replay.go) 提供测试与排障适配器：

- `RecordingHost` 保存 Host 调用的顺序、请求调试键、类型化结果、错误和 revision；
- `ReplayHost` 仅在下一次调用的种类与请求一致时返回已录制结果；
- `AssertComplete` 用于发现未消费记录。

它适合确定性测试，不是生产世界模型。回放不匹配会触发 `ErrReplayMismatch`（接口中
不能返回错误的方法会 panic），因此测试中应在外层捕获并报告上下文。

### Prompt

系统提示词位于 [ai-skill-system-prompt.md](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/skill/ai-skill-system-prompt.md)。
其中 fenced JSON 示例由 `prompt_test.go` 解析并编译。修改 prompt 中的 DSL 示例时，
必须一并运行该测试，避免文档说法与实际 wire contract 漂移。

## 8. 新功能的正确开发顺序

以新增一种 Effect 为例，按以下顺序实现，通常最少返工：

1. 明确能力目录和 Host 命令/结果；先确认它是世界写入、查询还是纯 Program 元数据。
2. 在 Wire 层加入严格解码，同时拒绝未知字段。
3. 在 IR 与 normalize 中显式建模，不使用 `map[string]any` 逃逸类型检查。
4. 在合适的 compile pass 中验证 capability、类型、作用域、生命周期和预算。
5. 在 lowering 中把名称降为 Handle、索引或具体 Program operation。
6. 在 executor/runtime 中调用 Host，并处理成功、预期失败和 Host 合约失败。
7. 通过 Inspector 公开确实需要被上层读取的稳定事实。
8. 添加：解析拒绝测试、编译诊断测试、Runtime 成功/失败测试、一个具有独立行为的 fixture。
9. 若提示词包含该能力，添加或更新可编译 JSON 示例。

不要只添加 Wire JSON 分支：那会使 DSL “能解析但不能证明/执行”。也不要只给
`MemoryHost` 加行为：那会制造一个没有语言入口的世界命令。

## 9. 验证流程

在 PowerShell 中（缓存目录可按本机情况调整）执行：

```powershell
$env:GOCACHE = 'D:\whb_s\cube\.tmp-skill-gocache'

# 先验证所有 fixture 的 Parse -> Compile -> Inspect -> Run 路径。
go test ./skill -run TestAllFixturesParseCompileInspectAndRun -count=1

# 再做包级回归和静态检查。
go test ./skill ./skillcompose -count=1
go vet ./skill ./skillcompose

# 改动 Runtime、Scheduler、Trace 或 Host 并发边界时必须执行。
go test -race ./skill ./skillcompose -count=1

# 修改 parser 或 Wire decode 时执行短时 fuzz。
go test -run=^$ -fuzz=FuzzParseGeneratedNeverPanics -fuzztime=10s ./skill

# 扩大到 gameplay 子树，并检查补丁空白错误。
go test ./game/gameplay/... -count=1
git diff --check
```

测试失败时，先按层定位：`Parse` 失败看 Wire；有 Diagnostic 看对应 pass；Program
inspection 不符看 lowering；cast 状态不符看 Runtime/Scheduler；世界结果不符看
Host command/result 和 revision。这样通常不需要在整条链路中盲目断点。

## 10. Visual 与 Sync 的继续阅读路线

完成 Runtime 主链后，按以下顺序继续：

1. `wire_visual.go`、`compile_visual.go`、`program_visual.go`：Visual 从严格 wire 到
   canonical manifest；
2. `presentation.go`、`presentation_assets.go`：静态 mount、动态生命周期事件和客户端
   资源解析边界；
3. `runtime_sync.go`、`runtime_mutation.go`、`runtime_value_json.go`：权威全量状态、规范
   mutation reducer 和游标过期；
4. `skillsync/skillsync.go`：三类业务 record 如何进入通用 Packet；
5. `skillsync/visibility.go`、`coordinator.go`、`outbox.go`：observer 过滤、source cursor、
   durable pending 和自动恢复；
6. `skillsync/applier.go`、`schema.go`：Epoch/schema/sequence/manifest 校验和事务应用；
7. `presentation_recovery.go`、`presentation_asset_cache.go`：持续表现恢复、可信目录、
   preload/fallback/ref-count/unload；
8. `roost-core/syncstream`：Epoch、WAL/checkpoint、ACK 裁剪、replay/full fallback 和生命周期；
9. `roost-core/syncstream`：确认发布、gzip、分片、SHA-256、有界重组和背压。

建议按四个小实验验证自己确实理解了实现：

1. 在 `runtime_sync_test.go` 里从旧 snapshot 折叠全部 `StateMutation`，比较两份规范 JSON；
2. 在 `skillsync_test.go` 让事务 Consumer 第一次 Commit 失败，确认 Epoch/sequence 都不推进，
   第二次重试只生效一次；
3. 在 `syncstream_test.go` ACK 并裁剪所有 retained packet，重启 journal 后继续 Append，确认
   Sequence/BaseSequence 没有回退；
4. 运行 `integration/sync-e2e`，沿调用栈观察确认失败、磁盘恢复、分片重组、Applier 和 ACK
   清理。这个测试是三仓边界是否真正契合的最终阅读入口。

## 11. 生产化增量：从写入到恢复的完整闭环

这一轮实现把 Visual 与 Sync 从“功能可用”推进到“进程可重启、容量可证明、访问默认
拒绝”。建议按下面的因果顺序阅读，而不是按文件名字母顺序阅读。

### 11.1 写时提交 authoritative mutation

先读 `runtime_mutation.go` 的 `beginStateMutationLocked` 与
`commitStateMutationsLocked`，再搜索所有调用点。每个公开写入口都遵守同一个临界区模板：

```go
runtime.mutex.Lock()
defer runtime.mutex.Unlock()
runtime.beginStateMutationLocked()
defer runtime.commitStateMutationsLocked()
```

因此写方法返回时，canonical `StateMutation` 已产生；`StateDeltas` 和 `StateSnapshot`
只是读取，不再在 flush/read 路径扫描整个 Runtime。直接修改外部扩展状态的 Host 必须调用
`CaptureExternalState` 建立明确提交边界。学习时运行
`TestStateMutationsAreCommittedBeforeWriteReturns`，并尝试在一次写入后连续读取两次 delta，
确认第二次读取不会产生隐式新 mutation。

### 11.2 Runtime checkpoint/restore

入口是 `runtime_checkpoint.go`：

- `Runtime.Checkpoint()` 在 Runtime 锁内生成稳定排序的权威镜像；
- `RuntimeCheckpoint` 带格式版本、payload 和 SHA-256；
- `RestoreRuntime` 严格拒绝未知字段、尾随数据、超大 payload、checksum 错误；
- Host 的 `CurrentRevision` 与 `AuthorityIdentity` 必须和镜像完全一致；
- `ProgramResolver` 返回的 Program 必须同时匹配 id、gameplay digest、compiler semantics
  和 authority；
- cast/spawn、frame、scheduler heap、随机调用计数、cooldown、ammo、policy、proc ledger、
  ability overlay 及所有递增 ID 都会恢复；
- trace、presentation queue、state delivery queue 属于观察/投递状态，不进入 gameplay 镜像；
  恢复后消费者先取 full state/presentation snapshot。

正确恢复顺序是：先恢复同一修订的世界 Host（数据库或世界快照），再装载所有被引用的
immutable Program，最后调用 `RestoreRuntime`，成功后才开放流量。任何一步不匹配都不能
“尽量恢复”。阅读 `TestRuntimeCheckpointRestoresActiveTimelineDeterministically`，它在施法
窗口中途保存，恢复后分别推进到 commit/execute/recovery，逐点比较权威 Host 状态。

### 11.3 VisualPlanCache 的两级引用模型

`presentation_asset_cache.go` 现在有两层生命周期：plan entry 引用一组 asset key，全局
asset entry 持有 preload 状态和引用数。不同 Program 使用相同 asset key 时只加载一次；
最后一个 plan 释放才 unload。相同 key 若对应不同 descriptor/fallback，会返回
`ErrVisualAssetCollision`，避免错误内容被缓存命中。

缓存命中时会在释放全局锁之前预留 plan 引用，避免“Acquire 正要返回、旧 lease 同时把资源
unload”的窗口。不同 primary key 若最终落到同一个 fallback key，也通过 alias 引用同一个
全局 fallback entry，不会重复加载或提前卸载。

`InvalidateCatalog` 先对整批目标做 preflight，再统一变更；只要有一个 plan 正在引用、加载
未完成或目录 digest 冲突，整次失效不产生部分结果。重点测试：共享资产最后引用释放、并发
加载、目录原子失效、digest collision。

### 11.4 Visibility 是封闭字段集合，不是补丁式过滤

`skillsync/visibility.go` 的 `VisibilityField` 是当前可同步字段的封闭枚举。构造快照时逐组
拷贝，而不是先浅拷贝再删字段，因此顶层新增 Runtime 字段默认不可见；嵌套结构并非逐字段白名单，新增嵌套字段必须同步审查投影。`DefaultDenyFields`
开启后，调用方必须通过 `FieldVisible(observer, field, handle)` 明确放行。

嵌套 `RuntimeValue` 使用 `RedactRuntimeValue` 递归处理 entity、entity list、hit、ability、
status、effect-result 等引用；不可见标量变为同类型 missing，列表过滤不可见成员。空间信息
可以单独用 `RedactSpatial` 清除 motion、position/path/direction，opaque token 用
`RedactOpaque` 控制。可见性 evaluator 返回错误时 Coordinator fail-closed，并记录失败。

### 11.5 Outbox、observer 与 schema 的有界生命周期

`skillsync/outbox.go` 同时限制全局 packet 数、JSON 字节数、每 observer/stream/epoch 数量和
最老 pending age。限制也会在重启装载时执行，不能靠重启绕过。`PutBatch` 先去重并整体
预检，持久化全部成功后才更新内存；ACK 批量删除失败时内存保持不变。发布筛选只保留
`MaxPublishBatch` 个候选，并对选中的 packet 设置进程内 publishing 占用，避免并发重试重复发布。
ACK 使用 observer/stream/epoch 索引，只扫描该 stream 的有界 pending，而不是扫描全局队列；
非事务 store 发生部分删除失败时，Coordinator 会立即用 retained History 补回缺失记录。

`file_outbox.go` 的记录带版本与 checksum，文件名由 packet identity 派生。启动时同时
验证 JSON 结构、checksum、文件名身份和 Packet 基本不变量；旧 record/packet-only 文件会
直接导致启动失败，不保留双读迁移逻辑。生产监控至少告警 `PendingBytes` 和
`OldestPendingAge`。

Coordinator 的 observer/key 锁使用引用计数，最后调用者退出即回收。`CloseObserver` 先把
observer 标为 closed，再按稳定顺序取得其所有 view 锁，先清理 durable outbox，成功后才
清理 History 和 cursor；关闭
后发布会返回 `ErrObserverClosed`，只有显式 `OpenObserver` 才能重新接入，防止迟到 goroutine
复活旧 session。

`skillsync.SchemaRegistry` 是有向迁移图：`Register` 添加单步迁移，`MigrationPath` 选择确定性
最短链，`Seal` 在启动完成后冻结配置。Applier 可直接把 registry 作为 `SchemaMigrator`；每步
函数收到携带当前 source version 的 Packet，nil payload、缺失路径、重复边和 sealed 后修改
都会明确失败。Schema 切换仍必须由 full packet 建立新链，迁移器不改变网络序列语义。

## 12. 新实现的详细测试流程

### 12.1 开发内环

```powershell
# 修改 checkpoint/runtime
go test ./skill -run 'TestRuntimeCheckpoint|TestStateMutations' -count=1

# 修改 visibility/outbox/schema/coordinator
go test ./skillsync -run 'Test(RuntimeVisibility|Outbox|FileOutbox|SchemaRegistry|CoordinatorReclaims)' -count=1

# 包级回归
go test ./skill ./skillcompose ./skillsync -count=1
go vet ./skill ./skillcompose ./skillsync
```

### 12.2 恢复与故障注入

逐项执行并保留日志/指标证据：

1. 在 preparing、committed、spawn running 三类时点 checkpoint，恢复后推进相同 tick，比较
   StateSnapshot、Host 结果和后续 checkpoint payload；
2. 修改 checkpoint version、payload、checksum、Host revision、authority 和 Program digest，
   每项必须在返回 Runtime 前失败；
3. 截断、篡改、重命名 `.packet` 文件，Outbox 启动必须失败且不得忽略坏文件；
4. 注入 batch delete/store write 失败，确认 pending/ACK 指标和内存集合不发生部分提交；
5. 填满 packet/byte/per-stream 任一上限，下一次 Put 必须返回
   `ErrOutboxCapacityExceeded`；构造过龄记录必须返回 `ErrOutboxPendingTooOld`；
6. 构造 1→2→4 与 1→3→5→4，确认 registry 永远选择前者；删除边、返回 nil、seal 后注册均
   必须失败；
7. 并发 publish/close/reopen，同一 closed observer 不得被迟到发布复活，最终 view lock 数为 0；
8. 两个 plan 共享 asset，逐个释放，只有最后一次释放发生 unload；目录失效任一目标忙时全批
   不变；
9. 用 nested effect-result/entity-list 覆盖 visibility，确认未显式列出的新字段默认不出现在
   observer snapshot/delta。

### 12.3 发布门槛

```powershell
go test ./... -count=1
go vet ./...
go test -race ./... -count=1
go test ./skill -run TestAllFixturesParseCompileInspectAndRun -count=1
go test -run=^$ -fuzz=FuzzParseGeneratedNeverPanics -fuzztime=30s ./skill

cd integration/sync-e2e
go test ./... -count=1
$env:ROOST_SYNC_SOAK='1'
$env:ROOST_SYNC_SOAK_DURATION='30m'
go test ./... -run TestProtocolSoak -count=1 -timeout 35m
```

还要在与生产一致的 broker、文件系统和数据库上跑一次：真实确认发布、进程 kill -9、WAL
恢复、Outbox 重放、客户端 ACK、历史裁剪、Runtime checkpoint 恢复。普通内存总线测试不能
替代这一步。验收标准是无数据缺口、无重复业务提交、无 observer 串流、无无限增长，且所有
失败都映射到已配置告警。

完整生产接入、发布与故障注入流程见
[Visual 与数据同步生产指南](visual-sync-production-guide.md)。

## 2026-10-07 契约边界补充

- skillcompose 的 SHA-256 digest 只校验内容自洽，不是带密钥签名。合同应由可信服务保存/提供；客户端提交的 Grants、Budgets 即使摘要匹配也不能当授权。Constraints / Obligations / Packages 目前只校验格式，业务策略须另行执行。
- `GraphFromProfile` 是操作数组摘要；有效果出口的检查不替代正式编译与控制流分析。
- C8 保留公开扩展 API（`RegisterProgram` / `PublishManifest` / `ExportMetrics`、`DefaultCatalog`、`Metrics.Bounded`）；仓内没有调用不等于对外废弃。manifest 注册/发送属于接入方责任。
- `Critical=false` 不是丢包许可，presentation 当前仍经过严格 sequence 链、ACK 和 outbox 重发；丢包需 replay/reset。
