# 08 skill 与战斗 说明

> 本篇是框架整体文档 08 分区的**说明文档**，面向技能作者、接入 Host 的业务开发与运维。代码结构、状态机、不变量强制点和 review 检查点见 [实现文档](../impl/08-skill.md)。
>
> 源码基准：tag `v1.23.0`（`28912cd6`），文中 `path:line` 都按这个 tag。codebase-memory 图谱的 generation 停在 2026-09-30，10 月新增的 `skill/spawn_table.go`、`runtime_spawn_stop.go`、`spawn_owned.go`、`host_capability*.go`、`eval_contexts.go` 等不在图里；本篇只用图谱定位，全部结论按 tag 源码直接读取。术语用 v1.23.0 现行名字：**衍生物（Spawn）**、**召唤物（Summon）**；`docs/release/v1.23.0/` 分册里改名之前的说法（进程 / process、`kind: "summon"`、checkpoint 版本 5 / 6 等）以本篇为准。

## 速览

- skill 是服务器权威的 2D 技能框架：技能 JSON 先经严格 **Parse**，再经 18 个静态 pass 的 **Compile** 降成不可变 **Program**，由确定性 **Runtime** 按 tick 执行；Runtime 对世界的一切读写只经 **Host** 接口。全程 int64 定点数学、HMAC 派生随机，同一输入位一致回放。
- 最重要的保证：**编译通过 ⇒ Runtime 与 Host 能执行**。引用在哪里能读只看一张求值上下文表（`skill/eval_contexts.go`）；phase 事件只看一张派发表（`skill/phase_events.go`）；Host 能读 / 能支持什么只看一张能力表（`skill/host_capability.go`，随环境进 authority digest）；lower 查不到名字就报 `LOWER_UNRESOLVED`，不交出 Program。
- 衍生物（飞行物、法术场、光束、位移、环绕、随从）的停止只有一个入口 `requestSpawnStop`：Host 拒绝停止时转入**待停止**，Runtime 按退避重试；待停止超过上限的最早一条转入**已放弃**（记录保留、告警），只在 `Advance` 末尾按上限清理。
- 最容易踩的坑：① Runtime **不在 Nest 事务里**（B4），handler 回滚只撤回战斗 DAO，冷却、ammo、cast、衍生物都不回退——先校验、后推进 Runtime；② 衍生物只能挂在**召唤效果（`summon`）**上，衍生物在**施法逻辑结束（移交）之前不逐 tick 推进**（§7.3，探针证实）；③ `StopSpawn` 必须幂等，Host 方法不得阻塞、不得重入 Runtime；④ 衍生物字段、回调、状态默认值里能读的引用很少，按 §4.1.4 的五行表写。
- 已知缺口（本篇写作时探针证实，见 §7.3）：G1 衍生物移交前的 tick 丢失；G2 运动衍生物启动时第一步之后的某一步被 Host 拒绝，Host 侧已登记的衍生物没人停（Runtime 没有记录）；G3 衍生物回调的事件丢掉根事件与 proc 深度。全部源码疑点见[实现文档 §11](../impl/08-skill.md#11-源码疑点与文档不一致)。

本篇覆盖的包：

| 包路径 | 职责 |
| --- | --- |
| `skill/`（编译器） | `Parse`（严格 wire）→ `Compile`（normalize / 18 个 pass / lower）→ 不可变 `Program`；`Inspect*` 只读视图；诊断 |
| `skill/`（Runtime） | cast 生命周期（窗口 / policy / 冷却 / GCD）、调度器、衍生物（Spawn）与召唤物（Summon）、被动与 proc、state mutation 与表现事件、checkpoint 版本 7 |
| `skill/`（Host 边界） | `Host` 接口、Host 能力表、`MemoryHost` 参考实现、`RecordingHost` / `ReplayHost` 调试替身 |
| `skill/skillsync` | 三条同步流（manifest / state / presentation）的记录、服务端 `Coordinator`（可见性、durable outbox、文件 outbox）、客户端 `Applier` |
| `skill/skillcompose` | 技能组合契约：只经 `Inspect*` 消费 Program，校验候选技能不越权 |
| `skill/combat` | 零依赖战斗数学：`AttributeSet`、`BuffContainer`、`ResolveDamage`（twelve_stage_v1）、`ChanceRoll` |
| `skill/combatcomponent` | combat 接入实体模型：`CombatDao`（A1 统一回滚）、`CombatComponent`（`ProjectAttributes` 投影）、`HostAdapter`、`StatusBridge` |
| `attribute/`、`spatial/` | 实体属性容器（代码生成用）与空间索引 / 寻路基础件（见 §1） |

---

## 1. 定位与边界

**一句话**：skill 把“技能写成什么样”与“技能怎么在世界里生效”分开——作者写受限的 JSON，编译器证明它能执行且有界，Runtime 确定性地执行，世界由业务 Host 掌握。

| 负责 | 不负责（设计决定，不是缺口，`skill/README.md` “Scope”） |
| --- | --- |
| 严格解析：未知字段、重复键、大小写变体键、尾随数据都拒绝 | 第三轴（高度、重力、体积碰撞）：宿主投影到 2D 后再回答查询 |
| 静态证明：名字解析到权威 handle、类型 / 量纲、作用域、生命周期、最坏预算、Host 能力 | 寻路 / navmesh：运动沿作者写的路径；“是否阻挡”问 Host |
| 确定性执行：定点数学、HMAC 随机、稳定调度、checkpoint 位一致恢复 | 客户端预测 / rollback：客户端只渲染表现事件、应用 state mutation |
| cast 生命周期、冷却 / GCD / ammo / charge / hold / toggle、被动 proc 账本 | 实体本身的存活、移动、伤害落地：都在 Host |
| 衍生物逐 tick 驱动、停止与重试；召唤物的事务式创建与指令 | 召唤物单位的同步（按业务实体同步，skill 不发它的 mutation） |
| 三条同步流的记录、可见性过滤、durable outbox | 网络传输本身（`syncstream` 与 JetStream 驱动在 04 sync） |
| 战斗数学（伤害管线、buff、属性）与实体侧 DAO | 业务的属性 → 伤害字段映射（`ProjectAttributes` 交给业务） |

**Runtime 不在事务里**（维护者决定 B4，`docs/skill/skill-casting-and-combat.md` “Runtime 不在事务里（B4）”）：这是“事务里会改的状态一律进 DAO”（A1）的明确例外。handler 失败或提交被拒时，`combatcomponent` 的 DAO 回滚，`skill.Runtime` 的冷却、ammo、cast、衍生物、proc 账本、state mutation 都不回退。

`attribute/` 与 `spatial/` 是本分区顺带覆盖的两个基础包：`attribute/` 是带脏跟踪的实体属性容器（`roost` 代码生成的属性组件用它），与 `skill/combat` 的 `AttributeSet` 不是同一个东西；`spatial/` 是格子阻挡索引、几何与 A* 寻路工具，skill 不 import 它（skill 不做寻路）。详见 §4.8。

跨分区：Nest 事务与 handler 见 [02 nest 与实体](./02-nest-entity.md)；DAO 统一回滚（A1）、`RunDetachedTransaction` 见 [03 DataEngine](./03-dataengine.md)；`syncstream` / JetStream 驱动见 [04 sync](./04-sync.md)；时钟与 tick 见 10 时间（<!-- pending: ./10-time.md -->`guide/10-time.md`）；指标与日志见 11 可观测（<!-- pending: ./11-observability.md -->`guide/11-observability.md`）；`roost add skill` 与 game-demo 见 12 codegen（<!-- pending: ./12-codegen.md -->`guide/12-codegen.md`）。

[↑ 速览](#速览) · [实现文档 §1](../impl/08-skill.md#1-包与文件地图)

---

## 2. 核心概念与术语

### 2.1 四层边界，一个方向

```text
技能 JSON ─Parse─▶ Definition ─Compile─▶ Program（不可变）─Runtime─▶ Host（唯一读写世界的接口）
          严格 wire          normalize + 18 pass + lower      确定性调度
```

| 对象 | 是什么 | 生命周期 |
| --- | --- | --- |
| `Definition` | 严格解析后的 wire 定义（`skill/wire_definition.go:5`），不是运行时对象 | 编译完即可丢弃 |
| `CompileEnvironment` | 权威目录与上限：属性 / 资源 / 状态 / 伤害类型 / 元素 / 标签 / 单位模板 / 衍生物属性 / Host 能力 / 视觉（`skill/compile_environment.go:291-304`）；带 `Revision` 与 `Digest`（authority digest） | 业务启动时构造，改了要重签 digest |
| `Program` | 编译产物：名字都降成了 handle / 槽位 / 下标，按索引执行；带三个 digest（源文档 / gameplay / presentation） | 跨施放共享、只读，可缓存 |
| `Runtime` | 确定性调度器：cast、冷却、衍生物、被动账本、state mutation、checkpoint（`skill/runtime.go:209-262`） | 一局（一个房间 / 场景）一个 |
| `Host` | 世界：实体、属性、位置、空间查询、效果落地、衍生物每一步与停止 | 业务实现；参考实现 `MemoryHost` |

身份与版本彼此独立（`skill/doc.go`）：Go import 是 `github.com/tjbdwanghaibo/roost-core/skill`；wire schema 是 `roost.skill/v2`（`skill/parse.go:12`）；编译器语义修订是 `skillv2-compiler-2`（`skill/runtime.go:27`），进 gameplay digest、checkpoint 与组合契约的校验。

### 2.2 衍生物（Spawn）与召唤物（Summon）

| | 召唤物（Summon） | 衍生物（Spawn） |
| --- | --- | --- |
| 是什么 | 用单位模板（`UnitTemplateCatalog`）在场景里生成的真实单位：陷阱、宠物、图腾、墙，归施法者所有 | 召唤效果上挂的、之后由技能逐 tick 驱动的东西：`kind` 为 `dash` / `orbit` / `projectile` / `area` / `beam` / `minion` |
| 谁管 | Host：单位的存活、指令、到期、清理 | Runtime：逐 tick 推进、派发信号、执行回调；Host 只执行每一步运动与停止 |
| DSL | 效果 `{"type":"summon",…}`、`{"type":"dismiss",…}`、`issue_entity_command`；owned 选择 `summoned_before` / `summoned_after`、排序 `summon_tick` / `summon_sequence` | effect flow 上的 `"spawn": {"kind": …}` 与 `"on": {…}` 回调；`$spawn`、`modify_spawn`、`spawn_start` / `spawn_step` / `spawn_callback` |
| Host 接口 | `OwnedEntityRuntimeHost.PreviewOwnedSummon` / `CommitOwnedSummon` / `RollbackOwnedSummon` | `Host.StepSpawn` / `Host.StopSpawn` |
| 同步 | 按业务实体同步 | state mutation `spawn_upsert` / `spawn_remove`；表现 `spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop` |

两者的关系是固定的：**衍生物只能写在召唤效果上**（编译期 `SHAPE_INVALID: only a summon effect starts a spawn`，`skill/compile_shape.go:95-104`），召唤出的单位就是衍生物的 **lifecycle 实体**。所以 Runtime 里所有衍生物的 `Scope` 都是 `entity`（唯一创建点 `startEntitySpawn`，`skill/spawn_owned.go:20-99`，只由 `executeOwnedSummon` 调用，`skill/runtime_owned_entity.go:61`）。`minion` 衍生物只跟着召唤物活：寿命取召唤效果的 `duration_ticks`，不做运动与成员检测，写了 `duration_ticks` / `area` / `interval_ticks` / `emit_leave_on_stop` 编译报 `MOTION_INVALID`（O22）。

`OwnedSpawns`、`OwnedSpawnSnapshot`、`SpawnCommandMeta` 这些名字里的 Spawn 指衍生物（被移交给 owner 的衍生物），不是召唤物，没有改名（见 `docs/feature/REFACTOR-2026-10-07-skill-summon-rename.md`）。

### 2.3 一次施放（cast）

| 术语 | 含义 |
| --- | --- |
| cast | 一次施放。`CastID` 单调递增；未提交就失败的启动会删除 cast 并还回 ID（`skill/runtime.go:479-495`） |
| 窗口阶段 `CastWindowStage` | `preparing`（前摇）→ `committed`（提交：付费、扣 ammo、进冷却 / GCD）→ `executing`（执行 phase 流程）→ `recovering`（后摇）→ `complete`；或 `cancelled`（`skill/runtime.go:117-126`） |
| cast 状态 `CastStatus` | `running` / `suspended`（等排程任务或 policy）/ `finished` / `failed`（`skill/runtime.go:108-115`） |
| policy | `tap`（默认）、`toggle`、`hold`、`charge`、`ammo`；toggle / hold 按 `pulse_interval_ticks` 发 `pulse`，到 `max_duration_ticks` 自动释放 |
| phase 与事件 | 定义是一个 phase 机；每个 phase 的 `on` 写事件流程。Runtime 派发 `enter` / `cancel` / `release` / `pulse` / `direction_changed` / `target_changed`；`recast` / `timeout` 能解析但编译期拒绝，非零 `timeout_ticks` 只给 warning（`skill/phase_events.go:25-38`） |
| GCD | 定义顶层 `global_cooldown_ticks`，从 **commit tick** 起算，存成保留程序 id `"$gcd"` 下的一条普通冷却（`skill/runtime_cast_window.go:8`、`:103-109`） |
| 互斥 | 同一施法者有 cast 处在 preparing / committed / recovering 时，新的主动施放返回 `ErrCasterBusy`；`"concurrent": true` 退出互斥；被动 / proc 触发的施放不受互斥与 GCD 限制（`skill/runtime.go:431-440`） |
| 移交（handoff） | 施法逻辑结束（`finish`，进入后摇）时，施法期间召出的衍生物从“施放中”移交给 owner（`skill/runtime_cast_window.go:143-152`、`skill/spawn_owned.go:206-243`）；之后衍生物与 cast 脱钩，按自己的寿命与回调运行 |

### 2.4 衍生物的五个分区

每条衍生物记录恰好在一个分区里，分区由记录字段（`Status` + 是否已移交）决定（`skill/spawn_table.go:25-53`）：

| 分区 | 状态 | 谁按它查 | 仍算“在宿主侧、由 Runtime 负责”（live） |
| --- | --- | --- | --- |
| 施放中 `spawnCasting` | `running`，未移交 | 施法收尾 / 打断 / 失败时停衍生物，移交，施法期间 lifecycle 失效回收 | 是 |
| 已移交 `spawnHandedOff` | `running`，已移交 | `OwnedSpawns(owner)`，逐 tick 推进，到期 / 失效回收 | 是 |
| 待停止 `spawnStopPending` | `stop_pending` | 退避重试，待停止上限，`RetentionStats` | 是 |
| 已停止 `spawnStopped` | `ended` / `cancelled` / `failed` | 只是历史：随 cast 回收、checkpoint、快照 | 否 |
| 已放弃 `spawnAbandoned` | `abandoned` | `Advance` 末尾按上限清理 | 否（Host 可能仍在运行，Runtime 不再负责） |

live 的衍生物钉住所属 cast（cast 不回收、ID 不复用）、占 owned 衍生物容量，`Shutdown` / `RemoveProgram` 要停它们（`skill/spawn_table.go:36`）。

### 2.5 求值上下文

一个值写在定义的哪里，决定它在哪个**求值上下文**里求值、能读哪些引用（`skill/eval_contexts.go:39-76`）：`cast_flow`（施法流程）、`memory_default`、`cast_start` / `phase_start` / `spawn_start`（三个缓存型快照的采样点）、`spawn_step`（衍生物每一步重新求值的字段）、`spawn_callback`（衍生物 `on.*` 回调）、`state_default`（持久状态默认值）。表的每一格只有“可用”与“不可用”两种结论；不可用的格子编译期拒绝，诊断原样带出这一格写的原因与“改用 …”。作者需要记的五行见 §4.1.4。

### 2.6 Host 能力表

编译器、Runtime、Host 共用一张“Host 能读什么、支持什么”的表（B3 ③），八列：可读属性、资源、衍生物 kind、motion 步骤、只交给 Host 的衍生物数值字段、资源 operation、属性修正 operation、召唤物。前两列取自 Gameplay catalog，后六列写在 `CompileEnvironment.Host`，整张表进 authority digest。定义用到表外取值时编译报 `HOST_CAPABILITY_MISSING`；Program 第一次在某个 Runtime 上用时，Runtime 核对它的需求都在 Host 声明的表里，缺了返回 `ErrHostCapabilityMissing`——在扣费之前。细节见 §4.4。

[↑ 速览](#速览) · [实现文档 §2](../impl/08-skill.md#2-关键类型与数据结构)

---

## 3. 设计原因

| 决定 | 内容 | 出处 |
| --- | --- | --- |
| 编译器是一串有序的证明 | 18 个 pass 固定顺序，后面的 pass 假定前面的已收窄名字、类型、作用域；任一 pass 出错就停（`skill/compile.go:16-42`）。不是可乱序的 lint 集合 | `skill/README.md` “编译管线” |
| lower 查不到名字即编译错误（B3 ①） | 所有“名字 → 槽位 / handle”查找走唯一入口 `resolveName`，查不到记 `LOWER_UNRESOLVED`，一次报出全部，`Compile` 不交出 Program。之前 lower 读 map 零值，前面的 pass 漏查一种名字时静默产出指向 handle 0 的 Program（NC-210 / NC-214） | `docs/feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md` |
| phase 事件派发表单一来源（B3 ②） | lower 只导出有派发点的事件；编译期拒绝没有派发点的事件；Runtime 派发点用同名常量。之前三处各写一份，出过“编译通过却永远不执行”（NC-150 / NC-151） | 同上 §3 |
| Host 能力表随环境下发（B3 ③） | 编译器、Runtime、Host 之前各自维护“支持什么”，施法到一半（已扣费）才发现 Host 不支持，或 Host 静默返回 0。现在一张表、进 digest，表外在编译期或准入时拒绝；`HostCapabilities()` 是 `Host` 接口的一部分，没有“未实现就跳过”的分支（RR-20261006-39） | `docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md` |
| 求值上下文表（第五轮）与 O33（第七轮） | 引用能不能读只看一张表，编译期与 Runtime 查同一张；值会随衍生物移交或读写位置“漂移”的格子一律编译期拒绝 | `docs/feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md` |
| Runtime 不在事务里（B4） | Runtime 是自带锁、调度器与投递缓冲的独立引擎；逐笔事务 checkpoint 或拆成 DAO 字段的代价远大于收益。约束交给业务：先校验后推进 | `docs/skill/skill-casting-and-combat.md` “Runtime 不在事务里（B4）” |
| 停止入口统一（2026-10-07） | 衍生物的所有停止入口只“请求停止”，Host 拒绝后的处理只在 `requestSpawnStop` 一处。之前散在入口里，一天出了六条 RR（RR-20261006-21～32） | `docs/feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md` |
| 衍生物按分区存放（维护者选 A） | “移交给谁”只由记录字段表达，删掉重复的 owned 表；只有 `setState` 改分区字段，源码守卫禁止别处写分区 map | `docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md` §1～§3 |
| 待停止上限改为“已放弃”（维护者选 B） | 超限不删记录，挪进已放弃分区；删记录只剩三个登记点，任何“取 ID 列表 → 逐个处理”的循环中途都不删（RR-20261006-34 的根因） | 同上 §10～§11 |
| process → Spawn、Spawn 效果 → Summon（破坏性，不留旧名） | “施放后由技能逐 tick 驱动的东西”统一叫衍生物；“生成真实单位”的效果叫召唤；衍生物 `kind: "summon"` 改为 `minion` | `docs/feature/REFACTOR-2026-10-06-skill-process-to-spawn.md`、`docs/feature/REFACTOR-2026-10-07-skill-summon-rename.md` |
| 不在 `Shutdown` 里同步重试停止 | Runtime 按 tick 推进、没有 ctx、不读墙钟；原地重试只会连打 Host，按墙钟等待会破坏回放确定性并阻塞调用线程 | `skill/runtime_spawn_stop.go:34-39` |
| 投影交业务（O2） | `CombatComponent.ProjectAttributes` 给入口，“哪个属性写到哪个伤害字段”由业务写；投影写在 DAO 的 vitals 上，随事务回滚 | `docs/feature/ROUND12-SKILL-CFGGEN-2026-10-06.md` §1.5 |
| 线上未部署，不做兼容 | checkpoint、环境格式、digest 变化一律升版本、旧版拒绝，排空后升级（维护者 2026-10-06） | `skill/runtime_checkpoint.go:16-29` |

[↑ 速览](#速览) · [实现文档 §3](../impl/08-skill.md#3-主流程)

---

## 4. 怎么用

### 4.1 写技能定义（作者视角）

#### 4.1.1 最小可运行定义

`skill/testdata/simple_damage.json`（全文 43 行）：输入一个实体目标，造成 10 点物理伤害，结束。

```json
{
  "schema": "roost.skill/v2",
  "id": "skill.test.simple_damage",
  "name": "Simple Damage",
  "description": "Deals physical damage to the input target.",
  "activation": {"type": "active", "policy": {"mode": "tap"}},
  "input_schema": {"type": "entity"},
  "cooldown_ticks": 0,
  "costs": [],
  "memory": {},
  "initial_phase": "cast",
  "phases": [{
    "id": "cast",
    "timeout_ticks": 0,
    "on": {"enter": {"flow": "sequence", "steps": [
      {"flow": "effect", "effect": {"type": "damage", "target": "$input.target", "amount": 10, "damage_type": "physical"}},
      {"flow": "finish", "reason": "done"}
    ]}}
  }]
}
```

带施法窗口、冷却、GCD 与法力消耗的完整链路（JSON → Compile → MemoryHost + Runtime，带输出）在 `skill/README.md` “B. 完整链路”，可运行工程是 `skill/examples/fireball`（`cd skill/examples && go run ./fireball`）。新项目可以用 `roost add skill <name>` 生成骨架（`codegen/internal/roost/add_skill.go:10-46`，写 `game/skills/<name>.json` 并在缺失时生成 `game/skills/catalog.go`）。

#### 4.1.2 定义的骨架

| 顶层字段 | 说明 |
| --- | --- |
| `schema` | 必须是 `"roost.skill/v2"` |
| `id` / `name` / `description` | 身份与元信息；`id` 就是 Program id，也是冷却 key |
| `activation` | `{"type":"active","policy":{"mode":…},"cast_window":{…},"concurrent":…}`，或被动 `passive_on_hit` / `passive_on_damaged` / `passive_on_kill` / `passive_on_status` / `passive_on_resource`（带 `event_filter`、`proc_policy`） |
| `input_schema` | `none` / `direction` / `position` / `entity` / `direction_position` / `entity_position` / `two_point` / `drag` / `path`；被动只能是 `none` / `entity`（NC-221） |
| `cooldown_ticks` / `global_cooldown_ticks` | 自身冷却与全局冷却（GCD 从 commit 起算） |
| `costs` | `[{"resource":…,"amount":…}]`，amount 可以是表达式 |
| `memory` / `persistent_state` | 施法内变量 / 跨施法持久状态 |
| `initial_phase` / `phases` | phase 机；每个 phase 的 `on` 写事件流程 |
| `gameplay_tags` / `presentation` | 标签（必须在目录里）、视觉声明 |

流程（`flow`）只有九种：`sequence`、`parallel`、`if`、`repeat`、`wait`、`select`、`effect`、`goto`、`finish`（`skill/wire_flow.go:78-224`）。效果类型是封闭集合（`skill/wire_effect.go:193` 起的 `decodeEffect`）：`damage`、`heal`、`shield`、`add_status`、`remove_status`、`modify_status_instance`、`attribute_modifier`、`resource`、`set_memory` / `add_memory` / `clear_memory`、`teleport`、`knockback`、`pull`、`stop_movement`、`modify_state`、`modify_ability_state`、`modify_spawn`、`summon`、`dismiss`、`issue_entity_command`、`capture_snapshot` / `restore_snapshot`。wire 层是封闭的，`skill/wire_*.go` 就是语法的穷举定义；字段名逐字匹配，大小写变体键被拒（NC-150）。

**付费时刻**：`cast_window.refund_before_commit` 为真（不写 `cast_window` 时的缺省值，`skill/compile_normalize.go:79`）时，costs 在 **commit** 那一刻支付，commit 前取消不付费；为假时 costs 在**前摇开始（Activate）**时支付，commit 前取消**不退**——Runtime 没有退款逻辑，“refund”是靠推迟付费实现的（`skill/runtime_cast_window.go:51-56`、`:88-93`）。

#### 4.1.3 衍生物与召唤物怎么写

放一个陷阱、陷阱周围是每 tick 治疗的法术场（`skill/testdata/area_heal.json` 的核心）：

```json
{"flow": "effect",
 "effect": {"type": "summon", "template": "deployable.trap", "position": "$caster.position", "count": 1, "duration_ticks": 3},
 "spawn": {"kind": "area", "duration_ticks": 3, "interval_ticks": 1,
   "area": {"from": "$caster", "kind": "entity", "shape": {"type": "circle", "radius": 8},
            "filters": [{"type": "targetable"}], "order": {"by": "stable_id", "direction": "asc"}, "limit": 2}},
 "on": {"tick": {"flow": "effect", "effect": {"type": "heal", "target": "$event.target", "amount": 1}}}}
```

| 要点 | 说明 |
| --- | --- |
| `spawn` / `on` 只能写在 `summon` 效果上 | 写在别的效果上编译报 `SHAPE_INVALID`（NC-222） |
| 运动衍生物 | `motion` 由 `frame` / `steering` / `trajectory` / `offsets` / `collision` / `carry` / `completion` 组成，见 `skill/testdata/tracking_boomerang.json`、`path_projectile.json`、`carry_dash.json`；Runtime 每一步固定发 frame / steering / trajectory / offsets / completion / signals，collision / carry 写了才发（`skill/spawn_motion.go:8-304`）；能力表里可选的只有 frame / steering / offsets / collision / carry / completion 六种 |
| 回调事件 | `tick`、`hit`、`collision`、`end`、`cancel`、`transition`、`target_lost`、`enter`、`leave`（`skill/wire_flow.go:272-282`） |
| 数值轨道 | `numeric_tracks: [{"property":"speed","operation":"add","value":10,"over_ticks":2}]` 让衍生物属性随时间变化（`skill/testdata/dynamic_numeric.json`）；回调里用 `modify_spawn` 改当前衍生物 |
| 碰撞预算 | `max_reflects: N` 是碰撞预算，实际反弹 N−1 次；`max_pierces` 同理（O16） |
| area 的 `$event.enter_count` | 恒为 1：成员离开时成员状态随即删除（O15） |
| 时间推进 | 衍生物在**施法逻辑结束（移交）之后**才逐 tick 推进；施法还在 `wait` / `repeat` / policy 等待时，已召出的衍生物停在启动那一步（§7.3 G1，探针证实）。要“边施法边生效”，召唤后立即 `finish`，用 `cast_window.recovery_ticks` 或衍生物自己的寿命表达持续时间 |

更多：`owned_trap` / `owned_pet_command`（召唤物与指令）、`beam` / `hold_beam`、`projectile_area`、`entity_scoped_aura`；全部 36 个 fixture 在 `skill/testdata/`，每个都被 `TestAllFixturesParseCompileInspectAndRun`（`skill/acceptance_test.go:21`）完整跑一遍。

#### 4.1.4 引用在哪里能读（五行表）

完整表在 `skill/eval_contexts.go:194-291`（每格一句语义，诊断原样带出）；作者记住这五行：

| 写在哪里 | 上下文 | 能读 | 不能读 |
| --- | --- | --- | --- |
| phase 流程与 effect result 分支、costs / sustain costs、windup / recovery 表达式、召唤效果的 `position` / 属性覆盖 / 参数、numeric track 初值 | `cast_flow` | `$input.*`、`$memory.*`、`$local.*`、`$caster`、`$primary_target`、`$ability.self`、`$cast.*`；快照 `cast_start` / `phase_start` | `$owner`、`$lifecycle_entity`、`$spawn`、`$event.*`；`spawn_start` |
| memory 默认值 | `memory_default` | `$input.*`、`$caster`、`$primary_target`、`$ability.self`、`$cast.*`；`cast_start` | 别的 `$memory`、`$local`；`phase_start` |
| 衍生物**每一步重新求值**的字段（area 选择、follow / tracking / carry 目标、path 点、orbit 锚点、parabola 目的地、未绑定 numeric 的数值字段） | `spawn_step` | `$caster`、`$caster.position`、`$cast.mode` | 其余施法引用与衍生物引用；全部缓存快照 |
| 衍生物 `on.*` 回调 | `spawn_callback` | `$owner`、`$owner.position`、`$lifecycle_entity`、`$spawn`、`$event.*`、回调自己的 `$local.*`；`spawn_start` | 施法的一切（含 `$caster`，改写 `$owner`，O36） |
| 持久状态默认值 | `state_default` | 字面量、`$caster`、`$caster.position`、`$cast.mode` | 其余引用；全部缓存快照 |

O33 收紧的写法与改写对照见 `docs/skill/skill-casting-and-combat.md` “衍生物字段与状态默认值里的施法引用：编译期拒绝”。

#### 4.1.5 常见诊断

诊断码定义在 `skill/diagnostic.go:19-63`，每条带 `Path`（如 `$.phases[0].on.enter.steps[0].effect`）。

| 诊断码 | 典型原因 | 怎么改 |
| --- | --- | --- |
| `CAPABILITY_UNKNOWN` | 名字不在目录里（属性、资源、状态、伤害类型、元素…）；写了 `on.recast` / `on.timeout`；非零 `timeout_ticks`（warning） | 换成目录里的 key，或让业务在环境里加；phase 只能以 `finish` / `goto` 结束 |
| `INPUT_UNAVAILABLE` / `ATTRIBUTE_SNAPSHOT_INVALID` | 引用或快照点在这个上下文不可用（§4.1.4） | 按诊断消息里“改用 …”改写 |
| `SHAPE_INVALID` | 结构不合法：`spawn` 不在 `summon` 上、空 sequence、负 tick、Host 都不支持的取值 | 按消息改 |
| `LIFECYCLE_FALLTHROUGH` | phase 的 enter 流程可能落空（Runtime 没有 phase 计时） | 末尾加 `finish` 或 `goto` |
| `MOTION_INVALID` | 运动参数越界；minion 衍生物写了 `duration_ticks` / `area` 等 | 删掉不被读取的字段 |
| `HOST_CAPABILITY_MISSING` | 定义用到这个项目的 Host 不支持的取值，消息点名（如 `environment host capability table lacks motion_step "collision"`） | 换写法，或让 Host 实现后在环境里声明 |
| `BUDGET_EXCEEDED` | 最坏情形预算（操作数、目标数、寿命）超上限 | 收紧 repeat / limit / 寿命 |
| `LOWER_UNRESOLVED` | 编译器缺陷：前面的 pass 接受了一个它没解析的名字 | 报给框架维护者，不是作者错误 |

### 4.2 编译与装配

```go
definition, err := skill.Parse(raw)                       // 严格 wire；DefaultParseLimits：1 MiB、深度 64
environment := skill.DefaultCompileEnvironment()          // 或业务自己的目录
program, diagnostics := skill.Compile(definition, environment)
// 有任何 DiagnosticError 时 program == nil
```

- 生成工程的做法（`roost add skill` 生成的 `game/skills/catalog.go`，`codegen/internal/roost/add_skill.go:57-121`）：`//go:embed *.json`，启动时 `CompileAll(environment)` 逐个 Parse + Compile，任一错误即启动失败（fail closed），按 `skill.Inspect(program).ID` 去重。game-demo 在服务 Init 与玩家 controller 各编译一次（`demo/internal/service/game/service.go.tmpl:69`、`demo/game/controllers/player/skill_catalog.go.tmpl:46`），**只编译、不运行 Runtime**——执行需要业务自己的 Host。
- 改了环境（加属性、改 Host 段）要重签：`environment.Digest = skill.AuthorityDigest(environment)`，否则 `ENVIRONMENT_INVALID`。authority digest 一变，旧 Program、checkpoint、组合契约都对不上，需要重编译。
- AI 生成的定义用 `ParseGenerated`（支持结构化拒绝 `Rejection`），网关可用 `ParseWithLimits` / `ParseGeneratedWithLimits` 收紧上限（`skill/parse.go:54-100`）。

### 4.3 运行 Runtime

```go
runtime := skill.NewRuntime(host, skill.RuntimeOptions{MatchSeed: seed})
castID, err := runtime.Activate(program, skill.CastInput{Caster: 1, Target: 2})
err = runtime.Advance(tick)          // tick 是绝对时刻，不能倒退（ErrReverseAdvance）
snapshot, _ := runtime.InspectCast(castID)
```

| 入口 | 作用 |
| --- | --- |
| `Activate` / `Start` | 主动施放（`skill/runtime.go:372-394`）；toggle 技能再次激活等于关闭 |
| `Advance(tick)` | 推进到绝对 tick，按（到期 tick, 稳定序）执行排程任务、推进已移交的衍生物、重试待停止的停止（`skill/scheduler.go:259-311`） |
| `Cancel` / `Interrupt(id, tag)` / `Release` | 取消（跑 phase 的 `on.cancel`）、按打断标签打断（不跑 `on.cancel`）、释放 charge / hold / toggle |
| `Input(castID, port, payload)` | 施法中更新方向 / 目标，派发 `direction_changed` / `target_changed`（`skill/runtime_input.go:7`） |
| `ActivatePassive(program, event)` / `QueueExternalEvent` / `RuntimeOptions.PassiveRouter` | 被动：按事件入队，在 `Advance` 里执行（§4.3.1） |
| `RegisterAbility` / `ReadAbilityState` / `ModifyAbilityState` | 技能栏位、ammo、能力覆盖 |
| `OwnedSpawns(owner)` | 已移交、仍在运行的衍生物（不含待停止 / 已放弃） |
| `RemoveProgram(id)` / `Shutdown()` | 停该程序 / 全部 live 衍生物并让 Host 清理 owned 实体；停不下的留待停止，返回第一个错误 |
| `Checkpoint()` / `RestoreRuntime(...)` | 权威镜像与恢复（§4.5） |
| `StateSnapshot` / `StateDeltas` / `StateEvents` / `PollPresentation` / `PresentationSnapshot` | 同步与表现的读出口（§4.6） |
| `RetentionStats()` | 有界状态的压力（待停止、已放弃、完成 cast 数…） |

**在 Nest handler 里用 Runtime（B4）**：先做全部会让 handler 失败的业务校验，再推进 Runtime；扣费交给 Runtime 的 commit 路径（`Host.PayCosts`），不要在 Runtime 之外先扣；失败用 Runtime 自己的终态（effect result 的失败分支、`CastFailed`）表达。提交被拒 / 结果未知只能由业务处理：要严格一致就在提交确认后再推进 Runtime，或失败时 `Checkpoint` / `RestoreRuntime`。细则见 `docs/skill/skill-casting-and-combat.md` “Runtime 不在事务里（B4）”。

#### 4.3.1 被动与 proc

被动技能（`activation.type` 为 `passive_on_*`）不由玩家施放：事件经 `ActivatePassive`、`QueueExternalEvent` 或 Host 事件 + `PassiveRouter` 进入，入队成排程任务，到期时依次检查（`skill/runtime_proc.go:61-124`）：

1. `event_filter` 不匹配 → 抑制（`filter`）；
2. 事件的 `ProcDepth` ≥ `proc_policy.max_depth` → `max_depth`；
3. 不允许自触发且事件来自本技能 → `self_trigger`；
4. `once_per_root_event` 且账本里已有（根事件, 施法者, gameplay digest）→ `once_per_root`；
5. 同根事件计数超过 `max_events_per_root` → `max_events_per_root`；
6. 本 tick 已执行 `MaxPassiveActivationsPerTick` 个 → `max_activations_per_tick`；
7. proc 账本满（`MaxProcLedgerEntries`）且腾不出 → `capacity`。

抑制会记一条 `passive_suppressed` 运行事件，不是错误。proc 触发的施放不受互斥与 GCD 限制，冷却 scope 为 `target` 时冷却记在事件目标上（`skill/runtime.go:450-452`）。fixture：`passive_counter`、`passive_proc_guard`、`ammo_on_kill`。

### 4.4 实现 Host

Runtime 对世界的一切读写都经 `Host`（`skill/host.go:40-53`）。业务实现它，或从 `MemoryHost` 改起。

| 方法组 | 方法 | Runtime 什么时候调 |
| --- | --- | --- |
| 身份与能力 | `AuthorityIdentity()`、`HostCapabilities()` | 每次 `Start` 核对 authority；Program 第一次使用时核对能力表 |
| 时间与修订 | `Advance(tick)`、`CurrentRevision()` | `Runtime.Advance` 推进到有工作的 tick 时 |
| 读世界 | `Read`（资源 / 位置 / 属性）、`Select`（形状 + 过滤 + 排序 + 上限） | 求值、目标选择、area 成员 |
| 效果落地 | `PayCosts`、`Apply(EffectCommand)` | commit（或前摇开始）付费；每个效果 |
| 衍生物 | `StepSpawn`、`StopSpawn` | 衍生物每一步；停止与重试 |
| 事件 | `Events(after)` | 每次效果后、每个推进的 tick；被动从这里触发 |
| 状态存储 | `ReadState` / `ModifyState` | 持久状态读写 |

可选接口（Runtime 用类型断言探测，`skill/host.go:55-90`、`skill/host_owned_entity.go:65-72`）：

| 接口 | 不实现的后果 |
| --- | --- |
| `OwnedEntityRuntimeHost`（召唤物事务、owned 实体查询与清理） | 能力表声明了 `Summon` 却不实现：summon 效果在扣费之后才报 `ErrHostContractViolation`；`RemoveProgram` / `Shutdown` 静默跳过召唤物清理 |
| `HostEventCompactor` | 事件不压缩，Host 自己负责事件队列有界 |
| `InputPositionResolver` | 位置输入不做阻挡修正 |
| `AbilityRelationProvider`、`RuntimeStateExtensionProvider` | 能力关系、状态快照扩展不可用 |

**Host 契约**（`skill/host.go:3-39`，Runtime 不做运行期检查，违反只会表现为死锁、卡顿或回放不一致）：

1. 所有 Host 方法都在 Runtime 锁内被调用，彼此严格串行。
2. **不得重入 Runtime**（`Start`、`Advance`、`Cancel`……）：锁不可重入，重入即死锁。世界对技能的反应放进 `Events`。
3. **不得阻塞**：不等 channel、不等别的会调 Runtime 的 goroutine 持有的锁、不做无界 I/O。一次阻塞会卡住这个 Runtime 上所有施法者。
4. 结果只取决于请求 revision 下的世界状态：不读墙钟、不依赖 map 遍历顺序。
5. **`StopSpawn` 必须幂等**：停一个已停或不认识的衍生物返回成功、不产生第二次副作用；只有衍生物仍在世界里运行时才返回错误（Runtime 会转待停止并重试）。
6. `HostCapabilities()` 声明支持什么；零值表等于什么都不支持。

**声明能力表**（B3 ③）。能力表八列：可读属性、资源（这两列由 Gameplay catalog 推出）、衍生物 kind、可选 motion 步骤、只交给 Host 的衍生物数值字段（`turn_rate_mdeg_per_tick` / `return_speed_bp` / `collision_force`）、资源 operation（`set` / `add` / `spend` / `sub`）、属性修正 operation（`add` / `mul_bp`）、召唤物（`skill/host_capability.go:109-123`）。

- 编译侧：`CompileEnvironment.Host` 写业务支持的子集（缺省 `FullHostCapabilityCatalog()` 全部声明），改了要重签 `environment.Digest = skill.AuthorityDigest(environment)`。
- Host 侧：`HostCapabilities()` 返回同一张表。按 catalog 声明的 Host 可以用 `skill.CatalogHostCapabilities(catalog)` 得到前两列（`skill/host_capability.go:144`）。
- 自检：在**测试世界**里调用 `skill.CheckHostCapabilities(host, catalog, probe)`（`skill/host_capability_check.go:24`），它会逐项真的调用 Host，确认声明的都能做、表外的 key 被拒绝。不要对线上世界跑：它会付零费、加一个持续 1 tick 的 `mul_bp` 零值修正（把属性乘成 0）、步进一个不停止的探针衍生物（[实现文档 §11 H5](../impl/08-skill.md#11-源码疑点与文档不一致)）。
- 能力表**不覆盖**：可选接口本身（除 summon 列）、status / damage / heal / shield / temporal / state 命令。这些由 Host 在 `Apply` 里自行拒绝。

**参考实现与调试替身**：

| 类型 | 用途 | 注意 |
| --- | --- | --- |
| `MemoryHost`（`skill/memory_host*.go`） | 参考实现、测试世界、示例 | 先 `ConfigureGameplayCatalog(catalog)`：未配置时声明了缺省资源，但只带 handle 的付费会失败（实现文档 §11 H6）。`mul_bp` 修正按乘法链叠加，与 `combatcomponent.StatusBridge` 的加法叠加不同（`skill/combatcomponent/status_bridge.go:31-35`），一个游戏只选一种 |
| `RecordingHost` / `ReplayHost`（`skill/replay.go`） | 录制一次运行的 Host 交互，再按顺序回放；不一致 panic（`ErrReplayMismatch`） | 只转发能力表，不转发可选接口：包装后 summon 施法会在扣费后失败（实现文档 §11 H2）。用于不含召唤物的回放调试 |
| `combatcomponent.HostAdapter` | 把战斗命令接到实体模型 | 不是完整 Host：`Apply` / `Read` 返回 `(result, handled, err)`，业务 Host 先交给它，`handled=false` 再自己处理（§4.7） |

### 4.5 checkpoint 与恢复

```go
checkpoint, err := runtime.Checkpoint()     // Version 7 + JSON payload + SHA-256
restored, err := skill.RestoreRuntime(host, options, checkpoint, resolver)
```

- **顺序**：先把世界 Host 恢复到同一 revision（数据库或世界快照），再准备好所有被引用的 Program（`ProgramResolver` 按 id + gameplay digest 返回），最后 `RestoreRuntime`，成功后才开放流量。任何一步不匹配都拒绝，不“尽量恢复”。
- **拒绝条件**：版本不是 7（`ErrCheckpointUnsupported`）；校验和不对、未知字段、重复键、尾随数据、超过 `CheckpointMaxBytes` / `CheckpointMaxRecords`（`ErrCheckpointCorrupt`）；Host 的 `CurrentRevision` 或 `AuthorityIdentity` 不同（`ErrCheckpointHostMismatch`）；resolver 返回的 Program id / digest / 语义 / authority 不符，或 Program 的能力需求不在 Host 的表里（`ErrCheckpointProgram`）。
- **随 checkpoint 走的选项**：衍生物容量、`MaxActiveCasts`、`CompletedCastLimit`、`RootEventLimit`、停止重试三项、`MaxAbandonedSpawns` 等以 checkpoint 为准，覆盖调用方传入的值（`skill/runtime_checkpoint.go:414-429`）。投递缓冲（trace、presentation、state mutation 队列）不进 checkpoint，恢复后消费者先取全量。
- **待停止 / 已放弃**写进 checkpoint，恢复后按原来的重试时刻继续。
- 同一 Runtime 状态总是写出相同字节（O7）。
- 升级：线上未部署，checkpoint 格式每变一次版本加一、旧版拒绝；**排空后再升级**。版本历史（3 → 7）见 `skill/runtime_checkpoint.go:16-28`。

### 4.6 同步到客户端（skillsync）

`skill/skillsync` 把 Runtime 的三个读出口投影成 `syncstream` 的有序包（传输与 History 在 [04 sync](./04-sync.md)）：

| 流（topic） | 内容 | 记录 |
| --- | --- | --- |
| `roost.skill.manifest` | 程序的表现计划（`InspectPresentationPlan`），客户端据此解析表现事件 | `manifest`（Full） |
| `roost.skill.state` | 权威状态：cast、冷却、技能资源、能力、衍生物、policy、持久状态 | `state_full`（Full）/ `state_delta`（`StateMutation`） |
| `roost.skill.presentation` | 一次性表现事件（cast / effect / `spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop`） | `presentation` / `presentation_reset`（Full） |

服务端最小用法（真实调用见 `skill/integration/sync-e2e/e2e_test.go:68-139`，仓内唯一的调用方）：

```go
projector, _ := skillsync.NewProjector(1)                 // wire schema 版本，业务定，不能为 0
coordinator, _ := skillsync.NewCoordinator(skillsync.CoordinatorOptions{
    Runtime: runtime, History: history, Publisher: publisher, Projector: projector,
    Visibility: skillsync.EntityVisibilityPolicy{Visible: canSee, DefaultDenyFields: true},
    Outbox: outbox, RequireDurableOutbox: true,
})
_ = coordinator.PublishSnapshot(observer, key)  // 新 observer：先全量
_ = coordinator.Flush(observer, key)            // 每个同步 tick：增量 + 表现
_ = coordinator.Acknowledge(observer, stream, epoch, seq) // 客户端 ACK
_ = coordinator.RetryPending(time.Now())        // 定时：重发未 ACK 的包
_ = coordinator.CloseObserver(observer)         // 断开
```

- **可见性必须给**：`EntityVisibilityPolicy` 按实体（`Visible`）和字段（`FieldVisible`，字段名见 `skill/skillsync/visibility.go:17-30`）过滤快照、增量和表现。生产用 `DefaultDenyFields: true`：未知 mutation kind 才会默认拒绝（否则放行，实现文档 §11 Y6）。隐藏 `clock` 只清快照里的 tick / revision，增量与表现仍带（Y4）。
- **durable outbox**：包在客户端 ACK 之前一直留在 outbox，按 5s 重发；`FileOutboxStore` 每包一个文件，崩溃后从 History 重建（`Reconcile`）。上限缺省 100000 包 / 256 MiB / 每流 4096 / 最老 24h（`skill/skillsync/outbox.go:127-155`）；最老的待 ACK 包超过 24h 时**所有 observer** 的发布都被拒（`ErrOutboxPendingTooOld`），要盯 `Health()` 的 `outbox_pending_age`。
- **客户端** `Applier`：按流检查序号连续，缺包即 `ErrSequenceGap`，要求重同步（`Coordinator.Recover`）。表现流丢包同样报 gap（文档里“可丢弃”的说法不成立，Y5）。
- **一个 Coordinator 对应一个 Runtime**：`Flush(observer, key)` 不按 key 过滤，同一 Runtime 上注册多个 key 时每个 key 都收到整个 Runtime 的状态与表现（Y16）。按房间 / 场景各建一个 Runtime + Coordinator。
- 已知问题：晚加入的 observer 会收到缓冲里最多 1024 条过期表现（Y1）；outbox 拒收时 History 会重复追加（Y3）。见[实现文档 §11.3](../impl/08-skill.md#11-源码疑点与文档不一致)。

### 4.7 战斗：combat 与 combatcomponent

三层，按需要选用：

| 层 | 是什么 | 什么时候用 |
| --- | --- | --- |
| `skill/combat` | 零依赖的定点战斗数学：`AttributeSet`（base + flat + rate 聚合，`clamp((base+Σflat)×max(0,10000+Σrate)/10000)`）、`BuffContainer`（Refresh / Extend / Ignore / Independent 叠层、韧性、免疫、驱散、到期）、`ResolveDamage`（十二阶段 `twelve_stage_v1`，`skill/combat/damage.go:29-33`）、`ChanceRoll`（HMAC 掷骰） | 任何 Host 都可以内嵌；MemoryHost 跑的就是这份代码 |
| `skill/combatcomponent` | 把战斗状态放进实体 DAO：`CombatDao`（A1 统一回滚）+ `CombatComponent`（事务内 mutator）+ `HostAdapter` / `StatusBridge`（把 skill 命令落到组件上） | 战斗状态要随 Nest 事务提交 / 回滚、要持久化 |
| 业务 Host | 组合以上两者，加上空间查询、运动、召唤物 | 生产 |

**十二阶段**：target_validity → immunity → avoidance → damage_type → penetration → element → modifiers → critical → caps → shield → health → aftermath。抗性 R 的减伤是 `10000/(10000+100R)`，比率按基点相乘，每一步饱和不回绕。闪避、暴击等随机结果是调用方预先掷好的布尔事实（`Combatant` 字段），管线本身没有随机。

**接入实体（A1）**：

```go
dao := combatcomponent.NewCombatDao(entityID, "game")
// 注册到实体的 DaoManager（实现了 entity.DaoInterface 与持久化加载接口）
component := combatcomponent.NewCombatComponent(dao)
component.ProjectAttributes(func(attr func(combat.AttributeID) int64, c *combat.Combatant) {
    c.Armor = attr(attrArmor)              // 纯函数：只写由属性决定的字段
    c.DamageTakenBP = attr(attrTakenBP)    // 不要改 Health / Shield / Alive
})
```

- 所有 mutator（`ApplyDamage`、`ApplyBuff`、`SetAttributeBase`……）**必须在 Nest 事务里调用**，事务外调用在改状态之前 panic。handler 失败或提交被拒时，DAO（vitals 含投影字段、属性 base / bounds、buff 及其属性加成、同步脏位）回到事务开始时的值（[03 DataEngine](./03-dataengine.md) 的 A1）。
- **投影交给业务（O2）**：伤害管线读的是 `Combatant` 的平铺字段，buff 只改 `AttributeSet`。`ProjectAttributes` 装的投影在加载时、以及每个改属性来源的 mutator 末尾、同一事务里重算并写进 vitals，随 DAO 一起回滚（`skill/combatcomponent/component.go:251-279`）。投影写低了 `MaxHealth` 不会顺带钳 `Health`。
- **buff 到期靠业务驱动**：框架里没有任何地方定时调用 `TickBuffs`，业务按自己的 tick 调。
- `HostAdapter` 在事务外被调用时，每条命令各开一个 `RunDetachedTransaction`（需要配置 `Committer`），命令之间不原子。`ResourceAttribute` 必须配置，nil 时资源命令与付费会 panic（实现文档 §11 C6）。
- **Runtime 不回退（B4）**：handler 回滚只撤回 DAO；Runtime 的冷却、ammo、cast、衍生物，以及 `RevisionSource.CommitEffect` 推进的 revision 与事件都不回退。

示例：`skill/examples/combat`（纯 combat）、`skill/examples/statusbridge`（StatusBridge + 投影 + `ChanceRoll` + `RunDetachedTransaction`），`cd skill/examples && go run ./statusbridge`。仓内 codegen 与 game-demo 模板目前没有装配 `CombatDao`。

### 4.8 skillcompose、attribute、spatial

**skillcompose**：给“由已有技能组合出新技能”（例如 AI 生成）的流程用的合同。

1. `ExtractProfile(program)` 只经 `skill.Inspect` 读 Program，得到特性（`effect.<操作>`、`select.<形状>`）与预算指标；
2. `BuildContract(profiles, authority, policy, caller)` 生成合同：来源技能（id + gameplay digest）、授予的特性与变换、预算（各来源之和，再按 policy / caller 收紧）、规范化 digest；
3. 只把 `DeriveContractPromptView(contract)` 交给生成器；
4. 生成的候选编译后，`ValidateCandidate(contract, profile)` 逐条核对：身份与 authority、来源集合、特性都被授予、变换合法、预算、因果连通。

限制：合同 digest 是无密钥 SHA-256，**不防篡改**——合同要放在生成方改不到的地方（实现文档 §11 Y7）；因果连通只认 `damage` / `heal` / `summon`（Y8）。仓内没有正式调用方。

**attribute**：实体属性容器，给代码生成用（`//roost:attribute` 标记由 codegen 的属性生成器生成强类型 profile，`codegen/internal/attribute/gen.go`）。`Container` 按层（`Base` / `Final` 或游戏自定义）存 profile，按层记脏位（`attribute/container.go:49-170`），带锁。game-demo 的玩家属性组件每次从 DAO 现建容器（`demo/game/entities/player/attribute_component.go.tmpl`）。它与 `skill/combat.AttributeSet` 是两套东西，互不引用。

**spatial**：饱和几何（128 位中间值的距离判断）、`GridTerrain`（稀疏阻挡格，越界视为阻挡）、四方向 A* `FindPath`（缺省最多访问 10000 个节点，`spatial/pathfind.go:26-67`）、`BlockIndex`（按块存 ID 的分块索引，查询升序）。使用方是 `sync/entitysync/policy/aoi.go` 与 game-demo 的场景运行时；skill 不 import 它（skill 不做寻路）。`FindPath` 对 `GridTerrain` 逐点加读锁，要一致视图得由调用方在整个搜索期间持锁（demo 就是这样做的，`demo/game/scene/runtime/terrain.go.tmpl`）。

[↑ 速览](#速览) · [实现文档 §3](../impl/08-skill.md#3-主流程)

---

## 5. 配置

skill 没有 `roost.yaml` 配置键：所有参数都是 Go 结构体，由业务在装配时给出。

### 5.1 `RuntimeOptions`（`skill/runtime.go:29-87`，缺省值 `:285-370`）

| 字段 | 缺省 | 说明 |
| --- | --- | --- |
| `MatchSeed` | 零值 | 对局级随机种子；HMAC 派生施法密钥。生产必须给真随机种子 |
| `SupportedCompilerSemanticsRevision` | `skillv2-compiler-2` | 只接受这个语义修订的 Program |
| `PassiveRouter` | nil | Host 事件 → 被动候选；nil 时 Host 事件不触发被动 |
| `MaxPassiveActivationsPerTick` | 256 | 每 tick 被动执行上限 |
| `MaxOwnedSpawns` / `…PerOwner` / `…PerProgram` / `…PerTemplate` | 128 / 同总量 | live 的 entity 衍生物容量（含待停止）；超出时召唤预检返回预期失败 `capacity_reached` |
| `TraceSink` / `TraceLimits` | — | 诊断 trace 的输出与上限 |
| `PresentationLimit` / `StateEventLimit` / `StateMutationLimit` / `RuntimeEventLimit` | 1024 / 2048 / 2048 / 4096 | 投递与诊断缓冲上限；超出丢最老的并计数，消费者游标过期后取全量 |
| `CompletedCastLimit` | 2048 | 保留可查询的终态 cast；仍被引用（排程任务、policy、live 衍生物）的不回收 |
| `RootEventLimit` | 8192 | 根事件计数表上限（once-per-root 语义） |
| `CheckpointMaxBytes` / `CheckpointMaxRecords` | 16 MiB / 200000（上限 64 MiB / 1000000） | checkpoint 写出与恢复的上限 |
| `MaxActiveCasts` / `MaxAbilities` / `MaxProcLedgerEntries` | 4096 / 10000 / 262144 | 超出返回 `ErrRuntimeCapacityExceeded` |
| `CastEventLimit` | 256 | 每个 cast 保留的诊断事件 |
| `SpawnStopRetryBackoff` | 4 tick | 待停止第一次重试的间隔；每失败一次翻倍，最多翻 6 次（64 倍） |
| `SpawnStopRetryLimit` | 10 | 失败的重试到这个次数后不再自动重试（exhausted），告警、记录保留 |
| `MaxStopPendingSpawns` | 256 | 待停止条目上限；超出时最早的 exhausted（没有就最早仍在重试的）转为已放弃 |
| `MaxAbandonedSpawns` | 1024 | 已放弃条目上限；只在 `Advance` 末尾删最早的 |

### 5.2 其余参数

| 位置 | 参数 | 说明 |
| --- | --- | --- |
| `skill.ParseLimits`（`skill/parse.go:30-32`） | `MaxBytes` 1 MiB、`MaxDepth` 64、`MaxTokens` 100000、`MaxStringBytes` 64 KiB、`MaxContainerEntries` 4096 | 解析前的形状上限 |
| `CompileEnvironment.Limits`（`skill/compile_environment.go:394-396`） | `MaxPhases` 32、`MaxFlowNodes` 1024、`MaxRepeat` 64、`MaxSpawns` 128、`MaxLifetimeTicks` 36000、`MaxProcDepth` 8 … | 编译期最坏预算的上界；与 `RuntimeOptions.MaxOwnedSpawns` 是两个不同的限制 |
| `CompileEnvironment.Host` | `FullHostCapabilityCatalog()`（缺省声明全部能力） | §4.4 |
| `skillsync.CoordinatorOptions`（`skill/skillsync/coordinator.go:46-55`） | `Visibility`（必填）、`MaxPacketsPerFlush` 256、`RequireDurableOutbox` | §4.6 |
| `skillsync.OutboxOptions`（`skill/skillsync/outbox.go:83-155`） | ACK 重发 5s、失败重试 100ms～30s、100000 包 / 256 MiB / 每流 4096、最老 24h、每批 512 | §4.6 |
| `skill.MemoryHostOptions`（`skill/memory_host.go:114`） | `CompactEvents`（缺省 false） | 只在 Runtime 是事件唯一消费者时打开 |

[↑ 速览](#速览) · [实现文档 §5](../impl/08-skill.md#5-并发)

---

## 6. 运行与运维

### 6.1 指标与日志

| 名字 | 类型 | 含义 | 来源 |
| --- | --- | --- | --- |
| `skill.spawn.stop_retry_exhausted.total` | 计数 | 待停止的衍生物重试到 `SpawnStopRetryLimit`，不再自动重试 | `skill/runtime_spawn_stop.go:56`；同时 Warn `spawn stop retries exhausted` |
| `skill.spawn.abandoned.total` | 计数 | 待停止超过 `MaxStopPendingSpawns`，最早的一条被放弃（宿主侧可能仍在运行） | `:59`；同时 Error `stop-pending spawn abandoned at MaxStopPendingSpawns`，带 `spawn_id` / `cast_id` / `owner` / `lifecycle_entity` |
| `skill.spawn.abandoned_pruned.total` | 计数 | 已放弃记录超过 `MaxAbandonedSpawns`，在 `Advance` 末尾删除 | `:60`；同时 Warn `abandoned spawn pruned` |
| `Runtime.RetentionStats()` | 拉取 | cast 数、完成 cast、根事件、proc 账本、运行事件及丢弃数、待停止 / 重试耗尽 / 已放弃衍生物数（`skill/runtime_retention.go:5-22`） | 业务定期导出 |
| `Coordinator.Health(options)` | 拉取 | `outbox_pending` / `outbox_pending_bytes` / `outbox_pending_age` / `publish_failures` / `visibility_failures`（`skill/skillsync/observability.go:23-37`） | 接 readyz 或告警 |
| `Coordinator.ExportMetrics(sink, labels)` | 推送 | `skillsync_published`、`skillsync_outbox_pending`、`skillsync_outbox_oldest_pending_seconds` 等（`skill/skillsync/observability.go:39-53`） | 业务提供 sink |

Runtime 的诊断还有 `InspectTrace()` / `FlushTrace()`（`RuntimeOptions.TraceSink`）、`RuntimeEvents()`（`passive_suppressed`、`owned_spawn_callback_*` 等）、`InspectCast(id)`。指标体系见 11 可观测（<!-- pending: ./11-observability.md -->`guide/11-observability.md`）。

### 6.2 常见故障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 启动编译技能失败，诊断 `CAPABILITY_UNKNOWN` / `INPUT_UNAVAILABLE` / `SHAPE_INVALID` / `LIFECYCLE_FALLTHROUGH` | 定义用了表外名字、不可用的引用或 Runtime 不执行的写法 | 按 §4.1.5 与诊断消息改；升级相关的见 [TROUBLESHOOTING](../../TROUBLESHOOTING.md) T-248、T-258、T-265、T-269、T-271 |
| 启动失败 `unsupported effect "spawn"`、`unknown field "process"`、`kind "summon"` | 旧名字（Spawn 效果改名 Summon、process 改名 Spawn） | T-288、T-289 |
| `HOST_CAPABILITY_MISSING` | 定义用到环境 Host 段没声明的取值 | 换写法，或 Host 实现后在 `CompileEnvironment.Host` 声明并重签 digest |
| `Start` 返回 `ErrHostCapabilityMissing` | Program 需求不在 Host 的 `HostCapabilities()` 里 | 让 Host 声明与环境一致；用 `CheckHostCapabilities` 在测试里自检 |
| summon 施法返回 `skill: host contract violation` | Host 没实现 `OwnedEntityRuntimeHost`（或实现了旧方法名、或被 `RecordingHost` 包装） | 实现 `PreviewOwnedSummon` 等；不要用包装器跑召唤物 |
| `skill.spawn.abandoned.total` 增长 | 宿主 `StopSpawn` 持续失败 | T-290：先修宿主停止失败；被放弃的衍生物靠比赛结束 / 程序移除清理 |
| `Advance` 每次都返回同一个错误，tick 不前进 | 某个 Host 事件派发失败，事件 cursor 停住（例如 `PassiveRouter` 返回 Host 不支持的 Program） | 实现文档 §11 H1；检查 `PassiveRouter` 返回的 Program 是否都经过同一 Host 的准入 |
| `RestoreRuntime` 返回 `checkpoint version is unsupported` | checkpoint 版本不是 7 | 排空后升级，不做兼容 |
| `RestoreRuntime` 返回 `checkpoint does not match host state` | 世界没有恢复到 checkpoint 的 revision，或 authority 变了 | 先恢复世界，再恢复 Runtime |
| skillsync `Health` 报 `outbox_pending_age`，随后所有 observer 都发不出去 | 某个流的包长期没被 ACK（客户端掉线未关、History 删流后 ACK 不回来） | 对掉线 observer 调 `CloseObserver`；见实现文档 §11 Y2 |
| 客户端 `ErrSequenceGap` | 丢包（含表现流） | 走 `Recover` 重同步 |

[↑ 速览](#速览) · [实现文档 §6](../impl/08-skill.md#6-失败与不确定结果处理)

---

## 7. 保证与不保证

### 7.1 保证

| 保证 | 依据 |
| --- | --- |
| 编译通过的 Program 不含查不到的名字；phase 事件都有派发点；引用都在其上下文可用；Host 需求都在环境能力表里 | 三张单一来源的表 + `resolveName`，见实现文档 §4 I1～I5 |
| Program 用到 Host 不支持的能力时，在第一次使用、扣费之前拒绝 | `admitHostCapabilitiesLocked` |
| 同一输入、同一 MatchSeed、同样的 Host 应答 ⇒ 位一致的结果；同一 Runtime 状态写出相同 checkpoint 字节 | 定点数学、HMAC 随机、排序遍历；`TestRuntimeCheckpointBytesAreDeterministic` |
| 衍生物的每个停止入口行为一致；Host 拒绝停止不会冻结 Runtime；待停止重试有界；任何循环中途不删记录 | 实现文档 §3.7、§4 I6～I11 |
| 未提交就失败的施放等于没有施放（任务撤销、ID 归还） | `skill/runtime.go:479-495` |
| 有界：活跃 cast、完成 cast、根事件、proc 账本、投递缓冲、待停止、已放弃、owned 衍生物都有上限 | §5.1 |
| combatcomponent 的战斗状态随 Nest 事务提交 / 回滚，投影随 DAO 回滚 | A1，实现文档 §3.14 |

### 7.2 不保证

- **Runtime 不在事务里（B4）**：Nest handler 失败不会回退冷却、ammo、cast、衍生物、proc 账本。
- **Host 契约不在运行期检查**：Host 阻塞、重入、非确定、`StopSpawn` 不幂等，后果都由调用方承担。
- **已放弃的衍生物 Runtime 不再负责**：宿主侧可能仍在运行，只能靠比赛结束 / 程序移除或宿主自己清理。
- **事件派发失败没有补偿**：Host 已提交的副作用（例如已扣费）在之后派发失败时不回退（实现文档 §11 H1）。
- skillsync 的合同、表现流“可丢弃”、`CloseObserver` 栅栏等说法在源码里不完全成立（实现文档 §11.3）。
- 需要外部验证：业务 Host 是否满足契约（用 `CheckHostCapabilities` 与自己的回放测试）；skillsync 在真实传输上的长稳（`skill/integration/sync-e2e`，`ROOST_SYNC_SOAK=1`）。

### 7.3 本篇写作时探针证实的缺口

以下三条在 tag `v1.23.0` 上用临时测试实跑证实，细节、位置与探针写法见[实现文档 §11.1](../impl/08-skill.md#11-源码疑点与文档不一致)：

| # | 现象 | 对作者 / 业务的影响 | 绕开 |
| --- | --- | --- | --- |
| G1（实现文档 R1） | 衍生物在施法逻辑结束（移交）之前不逐 tick 推进；寿命仍从召唤时算起，移交前的 tick 永久丢失 | 召唤后 `wait 3` 再 `finish` 的 6 tick 法术场只结算 4 次（tick 0、3、4、5） | 召唤后立即 `finish`，用 `cast_window.recovery_ticks` 或衍生物自己的寿命表达持续时间 |
| G2（R2） | 运动衍生物启动时，第一步之后的某一步被 Host 拒绝：召唤被回滚、Runtime 没有记录，Host 侧已登记的衍生物没人停 | Host 里残留一个“活着”的衍生物 | Host 的 `StepSpawn` 不要在启动阶段部分成功；或在 Host 侧把未提交召唤事务的衍生物一起清理 |
| G3（R3） | 衍生物回调里的效果事件丢掉施法的根事件与 proc 深度：每个衍生物自成一个根，`ProcDepth` 归零，同一回调效果每 tick 的事件 ID 相同 | `max_depth` 管不住经衍生物的 proc 链；`once_per_root` 对衍生物整个寿命只触发一次 | 被动的 proc 限制不要依赖穿过衍生物的深度；用冷却或 `max_activations_per_tick` 兜底 |

[↑ 速览](#速览) · [实现文档 §11](../impl/08-skill.md#11-源码疑点与文档不一致)

---

## 8. 相关文档

- 实现：[impl/08-skill.md](../impl/08-skill.md)
- 跨分区：[02 nest 与实体](./02-nest-entity.md)（handler、事务、快慢池）、[03 DataEngine](./03-dataengine.md)（A1 DAO 回滚、`RunDetachedTransaction`）、[04 sync](./04-sync.md)（`syncstream` History / Publisher）；[07 配置](./07-config.md)（cfggen 生成的技能表）、10 时间（<!-- pending: ./10-time.md -->`guide/10-time.md`）、11 可观测（<!-- pending: ./11-observability.md -->`guide/11-observability.md`）、12 codegen（<!-- pending: ./12-codegen.md -->`guide/12-codegen.md`，`roost add skill`、game-demo）
- 模块文档（快速参考，与本篇冲突时以本篇与源码为准）：[skill/README.md](../../../skill/README.md)、[docs/skill/README.md](../../skill/README.md)、[skill-casting-and-combat.md](../../skill/skill-casting-and-combat.md)、[skill-implementation-guide.md](../../skill/skill-implementation-guide.md)、[visual-sync-production-guide.md](../../skill/visual-sync-production-guide.md)
- 设计决定：[B3 lower fail-fast](../../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)、[B3-3 Host 能力表](../../feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)、[求值上下文表](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)、[停止入口统一](../../feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md)、[衍生物分区](../../feature/REFACTOR-2026-10-07-skill-spawn-partition.md)、[process → Spawn](../../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md)、[Summon 改名](../../feature/REFACTOR-2026-10-07-skill-summon-rename.md)、[ROUND12（投影交业务）](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)
- 故障：[TROUBLESHOOTING](../../TROUBLESHOOTING.md) T-248、T-258、T-265、T-269、T-271、T-288～T-290
