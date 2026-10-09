# 施法语义与战斗内容电池（v1.4+）

阅读准备：先了解 [Skill 的职责与例子](../framework/guide/08-skill.md)；不熟悉DAO、事务、Host等概念时，从 [入门手册](../GETTING-STARTED.md) 开始。本篇保留具体接口与示例，适合在接入或定位问题时查阅。

本文档覆盖 v1.4 引入、v1.5 定稿的三组能力：施法互斥与全局冷却、施法窗口表达式化、`combat`/`combatcomponent` 战斗内容电池，以及随之而来的编译器语义修订迁移说明。

## 迁移说明（必读）

- **编译器语义修订升级为 `skillv2-compiler-2`。** 新增的 `concurrent`、`global_cooldown_ticks`、窗口表达式字段进入 gameplay digest，同一定义在新旧版本编译出的 digest 不同。v1.2.x 产生的 checkpoint、回放记录与 skillcompose 契约在新版本下**无法解析**（会得到明确错误而非静默失败）：升级时需要全量重编译技能定义、丢弃旧 checkpoint（或先在旧版本完成排空）并重算契约摘要。
- **Go 模块路径已改为 `github.com/tjbdwanghaibo/roost-core/gameplay/skill`**（与仓库名一致，不再使用 `/v2` major 路径）。自 `v1.5.0` tag 起可直接 `go get`；wire schema 仍是 `roost.skill/v2`，技能定义 JSON 不受影响。

## 施法互斥与全局冷却

- **施法互斥**：默认情况下，同一 caster 在已有施法窗口（windup / commit / recovery 阶段）内发起新的主动施法会得到 `ErrCasterBusy`。技能可在激活声明上用 `"concurrent": true` 退出互斥。proc / 被动触发的施法不受互斥与 GCD 限制。
- **失败终态**：`Cancel` / `Interrupt` / `Release` 在已经改动 cast 之后出错（cancel 回调失败、Release 重新进入窗口时付费失败等），以及排程任务失败，cast 都进入 failed 终态：撤掉它的全部排程任务、停衍生物（Spawn）、释放 toggle / hold / charge 的 policy 槽位、不再占施法窗口；错误照常返回。对 failed cast 再调这三个 API 返回 `ErrCastInputRejected`。未提交就失败的 `Start` 返回 `(0, err)`，不留下任何排程工作（RR-20261005-NC-110～112）。
- **停不下来的衍生物由 Runtime 收尾**（维护者 2026-10-06，RR-20261006-21 后续；2026-10-07 停止入口统一）：任何要停衍生物（minion、area、飞行物等）的地方——施法失败终态、goto / Cancel / Interrupt / 施法收尾、衍生物启动失败的清理、tick 驱动的到期 / 失效回收、`RemoveProgram`、`Shutdown`——都走同一个停止函数、同一套状态。宿主 `StopSpawn` 失败时，错误照常返回这一次的调用方（`Start` 返回 `(cast ID, err)`，cast 留作 failed、ID 不复用），衍生物标成 `stop_pending`：Runtime 不再推进它（不步进、不派发信号、不再跑回调），在之后的 tick 按退避重试停止——第一次在 `SpawnStopRetryBackoff`（默认 4）tick 之后，每失败一次间隔翻倍、最多 64 倍；成功后衍生物进入 `cancelled`，cast 不再被钉住，按 `CompletedCastLimit` 连同记录回收。移交后的衍生物到期 / 失效、或 lifecycle 实体消失时宿主拒绝停止，也一样转为 `stop_pending`，那一次的 `Advance` 返回错误，之后 Runtime 照常前进（RR-20261006-31；之前每次 `Advance` 都卡在这个衍生物上，整个 Runtime 的 tick 不动）。`Shutdown` / `RemoveProgram` 停不下的衍生物也一样转为 `stop_pending`、写进 checkpoint（之前留成 running、列在 `OwnedSpawns` 里等调用方重试）：它们返回第一个错误，之后继续 `Advance`、或 `Checkpoint` 后在新进程恢复再 `Advance`，Runtime 都会按原来的重试时刻接着停；再调一次 `Shutdown` / `RemoveProgram` 会立即再请求一次。`Shutdown` 不在原地同步重试（Runtime 按 tick 确定性推进、没有 ctx）。程序被移除后它的待停止衍生物照样由 Runtime 重试，重试不执行程序代码。被拒绝的停止只在第一次请求时跑回调与区域离开信号，之后的重试只重发宿主停止（RR-20261006-32：之前施法里的停止被拒后会再停一次、cancel 回调跑两遍）。失败的重试达到 `SpawnStopRetryLimit`（默认 10）次后不再自动重试：计 `skill.spawn.stop_retry_exhausted.total`、写一条 Warn 日志，记录保留（`RetentionStats().StopRetryExhaustedSpawns`），`Shutdown` / `RemoveProgram` 仍会再停一次。待停止条目最多 `MaxStopPendingSpawns`（默认 256）条，超限时最早的已告警条目（没有就是最早仍在重试的）改为 `abandoned`（已放弃，维护者第十三轮“待停止上限”选 B）：记录保留，不再重试、不再推进，不钉住 cast、不占待停止与 owned 名额，`Shutdown` / `RemoveProgram` 不再停它；计 `skill.spawn.abandoned.total`、写一条点名 spawn / cast / owner 的 Error 日志，客户端看到一次 `spawn_update` / `spawn_upsert`（status `abandoned`），不发 `spawn_remove`——宿主那边可能还在运行它，宿主侧清理靠比赛结束 / 程序移除。已放弃的记录最多 `MaxAbandonedSpawns`（默认 1024）条，只在 `Advance` 末尾删最早的（`skill.spawn.abandoned_pruned.total`、Warn 日志，这时 state mutation 才发 `spawn_remove`）；任何循环中途都不删记录（之前超限直接删，`Shutdown` / `RemoveProgram` 曾因此空指针 panic，RR-20261006-34）。待停止与已放弃状态写进 checkpoint（格式版本 4 起；当前版本 7：衍生物记录只存一份，恢复时按 `status` / `handed_off` 重新分区），恢复后按原时刻继续重试。
  - **客户端看到什么**：state mutation 依次是 `spawn_upsert`（`status: stop_pending`）→ 停掉后 `spawn_upsert`（`status: cancelled`）→ cast 回收时 `spawn_remove`；表现在进入待停止时多一条 `spawn_update`（`SpawnStatus: stop_pending`），停掉时 `spawn_stop`；待停止期间 presentation reset 仍带着这个衍生物（宿主侧还在）。重试次数与时刻不进同步状态，不会每次重试都发 mutation。`OwnedSpawns` 只列 Runtime 仍在推进的衍生物，待停止的看 `StateSnapshot().Spawns` 或 `RetentionStats()`。
- **全局冷却**：定义顶层的 `"global_cooldown_ticks": N`。**从 commit tick 起算**（不是 Activate 时刻）：施法提交时把 caster 置入 N tick 的全局冷却，期间任何技能的主动施法返回 `ErrGlobalCooldownActive`。多次提交取最晚到期时间。
- 全局冷却以保留程序 id `"$gcd"` 作为一条普通冷却条目存在：`StateSnapshot().Cooldowns`、增量 mutation 与 checkpoint 都能直接看到它，客户端按普通冷却渲染即可。

```json
{
  "cooldown_ticks": 40,
  "global_cooldown_ticks": 8,
  "activation": {"type": "active", "policy": {"mode": "tap"}, "concurrent": false}
}
```

## 施法窗口表达式（攻速缩放施法时间）

`cast_window` 的 windup 与 recovery 可以由表达式在运行时求值：

```json
{
  "cast_window": {
    "windup_ticks_expression": {"op": "scale_bp", "args": [10,
      {"read_attribute": {"entity": "$caster", "attribute": "attack_haste_bp", "snapshot": "current"}}]},
    "windup_ticks_min": 2,
    "windup_ticks_max": 10,
    "commit_tick": 2,
    "recovery_ticks": 3
  }
}
```

规则：

- 表达式与字面量互斥（同时给出 `windup_ticks` 与表达式是编译错误）；表达式必须声明 `*_min`/`*_max` 边界，结果在运行时**钳制**进边界——编译期的最坏情形（预算证明、commit 先于 execute 的不变量 `commit_tick <= windup_ticks_min`）因此始终成立。
- windup 表达式在窗口准备时采样一次；recovery 表达式在恢复阶段开始时采样一次（能看到执行后的状态）。
- 表达式量纲必须是 ticks；`scale_bp` 的第二个参数必须是 basis_points 量纲。

### 宿主如何提供攻速属性

属性目录条目的类型与量纲通过导出常量书写，扩展环境后需要重封 digest：

```go
environment := skill.DefaultCompileEnvironment()
environment.Gameplay.Attributes.Entries = append(environment.Gameplay.Attributes.Entries,
    skill.AttributeCatalogEntry{
        Handle: 40, Key: "attack_haste_bp",
        ValueType: skill.ValueKindInt, Quantity: skill.QuantityBasisPoints,
        Readable: true, Snapshots: []string{"current"}, ModifierOperations: []string{"add"},
        Minimum: 0, Maximum: 20000, Rounding: "toward_zero",
    })
environment.Digest = skill.AuthorityDigest(environment) // 重封，否则 ENVIRONMENT_INVALID
```

Host 的 `Read` 返回值用 `skill.AttributeRuntimeValue(catalog, handle, value)` / `skill.ResourceRuntimeValue(value)` 构造，量纲自动取自目录。

## 引用在哪里能读（求值上下文）

一个值写在定义的哪里，决定它在哪个**求值上下文**里求值、能读哪些引用。完整的表在 [`skill/eval_contexts.go`](../../gameplay/skill/eval_contexts.go)（每格一句语义；编译诊断原样带出这句话，诊断里会点名上下文与表项，能替代的写“改用 …”），设计见[方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。写技能时记住这五行就够：

| 写在哪里 | 上下文 | 能读 | 不能读 |
| --- | --- | --- | --- |
| phase 流程与 effect result 分支、costs / sustain costs、windup / recovery 表达式、召唤效果（`summon`）的 `position` / 属性覆盖 / 参数、衍生物 numeric track 的初值 | `cast_flow` | `$input.*`、`$memory.*`、`$local.*`、`$caster`、`$primary_target`、`$ability.self`、`$cast.*`；快照 `cast_start` / `phase_start` | `$owner`、`$lifecycle_entity`、`$spawn`、`$event.*`；`spawn_start` |
| memory 默认值 | `memory_default` | `$input.*`、`$caster`、`$primary_target`、`$ability.self`、`$cast.*`（都是 Activate 时的值）；`cast_start` | 别的 `$memory`、`$local`；`phase_start` |
| 衍生物**每一步重新求值**的字段：area 选择、follow / tracking / carry 目标、path 点、orbit 锚点、parabola 目的地、没有绑定到衍生物数值属性的数值字段 | `spawn_step` | `$caster`、`$caster.position`、`$cast.mode` | 其余施法引用与衍生物引用；全部缓存快照 |
| 衍生物的 `on.*` 回调 | `spawn_callback` | `$owner`、`$owner.position`、`$lifecycle_entity`、`$spawn`、`$event.*`、回调自己的 `$local.*`；`spawn_start` | 施法的一切（`$input`、`$memory`、`$caster`、`$primary_target`、`$ability.self`、`$cast.*`）；`cast_start` / `phase_start` |
| 持久状态默认值 | `state_default` | 字面量、`$caster`、`$caster.position`、`$cast.mode` | 其余引用；全部缓存快照 |

衍生物字段在启动那一步用施法求值，之后每一步用移交后的衍生物求值，所以只能读两边都求得出、且值一样的引用；状态默认值在读 / 写这条状态的地方求值，那里可能是施法流程、衍生物字段或衍生物回调，所以只能读在所有这些地方都一样的引用。

### 衍生物字段与状态默认值里的施法引用：编译期拒绝（O33，v1.21.0）

维护者第七轮决定（2026-10-06）。下面这些写法此前能编译、运行也不报错，但值会随衍生物移交或读写位置悄悄变掉；现在编译期报 `INPUT_UNAVAILABLE`（引用）或 `ATTRIBUTE_SNAPSHOT_INVALID`（快照），升级后按右列改写：

| 写法 | 以前实际得到的值 | 改成 |
| --- | --- | --- |
| 衍生物字段里的 `$primary_target`（例如 area 的 `from`） | 启动那一步是施法目标，之后每一步是衍生物的 lifecycle 实体 | 召唤效果的 `position` 在施法流程里求一次，用 `$input.target.position` 把 lifecycle 实体放到目标处，再在回调里以 `$lifecycle_entity` / `$event.target` 为准（例如 `on.tick` 里 `select` from `$lifecycle_entity` 代替 area 选择）。每一步跟随一个移动中的施法目标没有等价写法：移交后的衍生物不持有施法目标 |
| 衍生物字段里的 `$cast.charge_bp` / `$cast.release_reason` / `$cast.pulse_index` / `$cast.stock` / `$cast.max_stock` | 启动那一步是施法的值，之后是零值 | 衍生物 `numeric_tracks` 的初值（衍生物启动时用施法求一次，例如 `{"op":"scale_bp","args":[10,"$cast.charge_bp"]}`） |
| 衍生物字段里的 `$cast.elapsed_ticks` | 之后每一步是当前 tick | 衍生物自己的计时：numeric track 加回调里 `modify_spawn` 的 `over_ticks`，或回调里的 `$event.tick` / `$event.membership_ticks` |
| 衍生物字段里的 `$ability.self` | 之后 handle 为 0 | 在施法流程里读写技能状态 |
| 衍生物字段里的 `cast_start` / `phase_start` 读取 | 之后退化为 `current` | numeric track 的初值；要衍生物启动时的值，在回调里用 `spawn_start`；否则直接写 `current` |
| 状态默认值里的 `$primary_target`、`$ability.self`、`$cast.*`（`$cast.mode` 除外）、`cast_start` / `phase_start` 读取 | 在施法里读写时是施法的值，在衍生物回调 / 衍生物字段里读写时是 lifecycle 实体 / handle 0 / 零值 / `current` | 默认值用字面量或 `$caster`，在施法流程里用 `modify_state` 把同一个表达式的值写入（表达式类型就是状态类型，总能写进去）；快照也可以直接改成 `current` |
| memory 默认值里的 `phase_start` 读取 | Activate 时的值：memory 初始化早于第一个 phase，也早于 costs 与 windup，与 `cast_start` 读到的是同一个值 | `cast_start`（同一个值）；要 phase 开始时的值，就在 phase 流程里读 `phase_start` |

### 保持现状的三处语义（O34～O36）

维护者第七轮决定保持行为不变、写明：

- **costs / windup 里的 `phase_start`（O34）**：`phase_start` 是“最近一次进入的 phase 开始时”的值。costs 与 windup 表达式在进入第一个 phase **之前**求值（非 charge 模式在 Activate 时；`refund_before_commit` 的 costs 在 commit 那一刻），那时还没有 phase 开始的值，读到的是**求值那一刻**的值（等同 `current`）。charge 模式的 costs / windup 在 release 时求值，那时已在 phase 里，读到的是当前 phase 开始时的值；sustain costs 每个 pulse 求值，同样是当前 phase 开始时的值。要“施法开始时”的值请写 `cast_start`。
- **衍生物回调里的 `self_ability` / `not_self_ability` 过滤（O35）**：移交后的衍生物没有技能句柄，回调里的技能选择拿 handle 0 比较——`self_ability` 永远不匹配、`not_self_ability` 匹配全部技能。要按“本技能”筛选，请在施法流程里做。
- **衍生物回调里的 `$caster`（O36）**：Runtime 其实求得出（= 衍生物的 owner，即同一个施法者），但编译期按表拒绝，统一写 `$owner`（`$caster.position` 同理写 `$owner.position`）。

### 衍生物、运动与 temporal 的既定语义（O15～O17、O27、O28）

维护者第十二轮决定（2026-10-06）保持行为不变、写明：

- **area 的 `$event.enter_count` 恒为 1（O15）**：成员离开区域时它的成员状态随即删除（为了让轮换进出的成员不无限占内存），再次进入从 1 重新计数。所以 `enter_count` 不是“累计进入次数”，在 enter / tick / leave 回调里读到的都是 1。要累计某个实体的进入次数，在回调里用持久状态自己记。
- **`max_reflects: N` 实际反弹 N−1 次（O16）**：N 是**碰撞预算**，不是反弹次数。每次碰撞消耗 1；预算没用完时翻转方向、发 transition 并继续飞；第 N 次碰撞同样翻转方向、发 transition，然后在同一 tick 结束运动。所以 `max_reflects: 1` 是“碰到就结束”，想要弹 k 次再结束写 `k+1`。`max_pierces: N` 同理：穿过前 N−1 个，第 N 次碰撞时结束。
- **Host 拿到的 numeric 快照与运动实际取值不同源（O17）**：`SpawnStepCommand.Numeric` 里，被 numeric track 绑定到衍生物数值属性的字段是衍生物当前值；**没有绑定**的字段报告的是衍生物启动时求一次的值，而运动每一步按 `spawn_step` 列重新求值表达式（见上文求值上下文表）。速度等字段写成随时间变化的表达式时，Host 在快照里看到的速度与实际位移用的速度可能不同。parabola 的 speed、tracking 的转向速率、boomerang 的回程速度、碰撞力在快照里的基值恒为 0（由 Host 自己管理）。Host 需要“这一步实际用的值”时，让技能用 numeric track 绑定该字段，或从运动步骤命令本身取位置 / 位移，不要依赖未绑定字段的快照。
- **restore 的 `on_blocked` 与 profile 策略冲突时必然失败（O27）**：参考宿主 `MemoryHost` 的 temporal restore 里，`on_blocked` 为空时用快照 profile 的 `BlockedPositionPolicy`；写了且与 profile 不同，恢复返回预期失败 `policy_rejected`（走 `result.failure`），不会按 `on_blocked` 覆盖 profile。编译期只检查取值合法——token 可以经持久状态跨施法传递，profile 在编译期不一定可知。所以 `on_blocked` 实际只是“与 profile 一致”的断言：一般不写，写就写成与 profile 相同的值。自己实现 temporal 的 Host 应保持同一口径（`TestTemporalPassBranches` 钉住）。
- **spawn_start 读取的实体按启动时的事件求值（O28）**：衍生物回调里 `read_attribute` 写 `snapshot: "spawn_start"` 时，整个读取（包括 `framework/entity`）在衍生物启动那一刻求值，那时的 `$event` 是启动事件（`$event.target` 是 lifecycle 实体），不是每次回调的事件。所以 `{"entity":"$event.target","snapshot":"spawn_start"}` 读到的是 lifecycle 实体启动时的值，不是本次回调目标的值。这与 `cast_start` “整个读取在采样点求值”的口径一致。要按回调目标读，用 `snapshot: "current"`。

### 编译期收紧与诊断文案（O22、O29，v1.23.0）

维护者第十二轮决定（2026-10-06）：

- **minion 衍生物不写 `duration_ticks`（O22）**：minion 衍生物（`kind: "minion"`，2026-10-07 前叫 `summon`）的寿命就是召唤效果的 `duration_ticks`（编译期要求为正），衍生物自己的 `duration_ticks` 从来不被读取，以前负数也能编译。现在 minion 上写 `duration_ticks`（非 0）报 `MOTION_INVALID`；`area`、`interval_ticks`、`emit_leave_on_stop` 同样拒绝（minion 不做成员检测，以前写了 `area` 编译通过、运行期启动即 `ErrProgramInvariant`）。**升级**：删掉 minion 衍生物上的这几个字段，行为不变。
- **result 分支里可以启动不带回调的衍生物（O29，只改文案）**：effect result 分支与 status 实例选择的消费流程不能挂起（`wait`、带 `interval_ticks` 的 `repeat`），也不能启动带 `on` 回调的衍生物；召唤效果加不带 `on` 的衍生物一直可以编译，执行时照常启动衍生物。诊断文案改为 “cannot suspend (wait, repeat with interval_ticks) or start a spawn with on callbacks”，以前写的 “cannot suspend or start a spawn” 与规则不符。

## combat：零依赖战斗内容电池

`combat` 包是可复用的确定性战斗数学，零外部依赖：

- `AttributeSet`：base + 修饰器（flat 求和 + rateBP 加性求和），`Grant`/`Revoke` 完全可逆、结果与授予顺序无关。
- `BuffContainer`：叠层（refresh / extend / ignore / independent 策略，independent 每次应用都是独立计时的新实例）、驱散标签、免疫标签、韧性减时（`SetTenacityBP`）、`MaxDurationTicks` 时长上限（在韧性缩放之后钳制），可 `LinkAttributes` 让 buff 修饰器自动物化为属性授予。
- `ResolveDamage`：twelve_stage_v1 十二段伤害管线（命中回避 → 抗性穿透 → 元素/增伤/减伤 BP → 暴击 → 上下限 → 护盾吸收 → 死亡防护钩子 → 吸血）。随机性外置：闪避/暴击等以预掷事实传入。所有 BP 段夹取非负。
- skill 的 `MemoryHost` 直接运行在这份代码上：参考实现与生产实现共享同一份数学。

**buff 与 skill status 的分工**：skill 的 status 是技能程序可见的世界目录语义（选择器过滤、combat hook 的载体），由 Host 拥有；`combat.BuffContainer` 是宿主实体侧的属性/时效容器。典型宿主用 status 承载技能系统语义，用 BuffContainer 承载数值聚合，两者在 Host 的 `Apply(StatusCommand)` 实现里桥接。

**AttributeSet → Combatant 投影**：伤害管线读取的是 `Combatant` 平铺字段，buff 与属性修饰只改 `AttributeSet`，两者之间要有一个投影。自己持有 `AttributeSet` 与 `Combatant` 的宿主可以用 `Observe` 回调投影；用 `combatcomponent` 的宿主用组件的投影入口，见下文“属性投影（O2）”。

## StatusBridge：status 命令落到 combat 容器

`combatcomponent.StatusBridge` 把 skill 的 status 域效果命令（`StatusCommand` / `RemoveStatusCommand` / `DispelStatusCommand` / `AttributeModifierCommand`）标准化地落到 combat 容器上，消灭"status 与 buff 双体系各自实现"的第三套状态系统：

- catalog 的 `StatusCatalogEntry` 驱动映射：`MaxStacks`/`RefreshPolicy`（refresh/extend/ignore/replace）/`DispelCategory`（作为驱散 Tag）/`TenacityPolicy: scale_duration`/`MaximumDurationTicks`/`AttributeModifiers` 全部翻译为 `combat.BuffSpec`；
- 免疫标签是世界事实，经 `HasGameplayTag` 回调判定（nil 则关闭该检查）；
- `AttributeModifierCommand` 以保留 BuffID（`AttributeModifierBuffID`）+ `BuffIndependent` 策略应用——每条命令独立计时，与 MemoryHost 的逐条 modifier 实例语义一致；
- 事件词表与 MemoryHost 相同（`status_applied` / `status_immune` / `status_removed` / `status_dispelled` / `attribute_modifier_applied`），proc 过滤器两边行为一致；
- 挂进 `HostAdapter.Status` 字段后，`HostAdapter.Apply` 自动把 status 域命令转给桥（同时 `ResourceCommand` 也由 adapter 落到映射属性上，语义与 MemoryHost 相同：spend 原子校验、no-op 不推进 revision）。
- **实例句柄操作**（`ModifyStatusInstanceCommand`：偷取/转移/复制、层数与时长编辑）同样由桥处理，授权矩阵与操作门控照搬 MemoryHost（SourceOwnership、Dispellable、Copyable/Transferable/Stealable、DurationOperations 白名单、MaximumDurationTicks 钳制）。**实例寻址契约：`StatusInstanceRef` 的 opaque id 就是 `combat.BuffInstanceID`**——宿主在 Select 返回 status 实例时用 `skill.NewStatusInstanceID(uint64(instance.Instance))` 发放句柄。护盾类 status 的盾值搬移不在桥内（护盾池在 `Combatant.Shield`，由宿主的 damage/shield 面管理）。

**一处有意的语义差异**：`mul_bp` 修饰在桥/AttributeSet 中按 **基点增量加性叠加**（两个 ×1.2 = +40%，顺序无关、事务回滚可精确逆转），而 MemoryHost 是乘性链（= +44%）。一个游戏只选一种宿主语义并保持一致。

## Runtime 不在事务里（B4）

维护者 2026-10-06 决定（B4）：`skill.Runtime` 的状态**不进** Nest 事务，保持现状。这是“事务内会改的状态一律进 DAO”（[A1](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）的明确例外：Runtime 是自带锁、调度器和投递缓冲的独立执行引擎，逐笔事务做 checkpoint 或把它拆成 DAO 字段的代价都远大于收益。

**约束**：在 nest handler 里推进 Runtime（`Start` / `Activate` / `Advance` / `Cancel` / `Release` / `ActivatePassive` …）之后，handler 失败或提交被拒，DAO 回滚，Runtime 不回退：

| 回退 | 不回退 |
| --- | --- |
| 经 `HostAdapter` / `StatusBridge` 改的战斗 DAO：法力等资源（`PayCosts`）、血量、护盾、属性修饰、buff | 冷却与全局冷却（commit 时写入）、ammo 库存与充能排程、cast 状态与排程任务、owned 衍生物、ability 状态覆盖、proc 账本与同根事件计数、state mutation 流与 presentation 缓冲、Runtime 观察到的 revision；业务 `RevisionSource.CommitEffect` 推进的 revision 与追加的事件 |

典型后果是“法力已回滚、技能已进冷却”。框架不做补偿，业务按这个前提设计：

1. **先校验、后推进**。会让 handler 返回 error 的业务检查（目标归属、背包、等级、业务冷却等）全部放在调用 Runtime 之前；Runtime 调用之后 handler 不再因业务原因失败。
2. **扣费交给 Runtime 的提交路径**。技能的 costs 由 Runtime 在 commit 时经 `Host.PayCosts` 原子支付，顺序是“支付 → ammo 扣减 → 冷却”（`runtime_cast_window.go` `commitCast`）。支付失败时 cast 不提交、冷却与 ammo 不动（启动阶段失败的 cast 直接删除，NC-110）。不要在 Runtime 之外先手工扣法力再施法。
3. **失败用 Runtime 自己的终态表达**。Host 命令的“预期失败”（目标无效、被免疫等）返回带失败结果的 `EffectResult`，走定义里的 `result.failure` 分支；Host 返回 error 时 Runtime 把 cast 记为 `CastFailed`（`failCastLocked`），冷却按已提交处理。两种情况 DAO 与 Runtime 对“这次施法发生过、结果如何”的看法一致，不需要 handler 失败来表达。
4. **提交被拒 / 结果未知**（WAL 或 Remote 拒绝）只能由业务处理：必须严格一致的玩法，可以在提交确认后再推进 Runtime（handler 外的 `HostAdapter` 每条命令走 `RunDetachedTransaction`，彼此不原子，见 N09 O4），或在失败时用 `Checkpoint` / `RestoreRuntime` 恢复（全量序列化，成本高，投递缓冲不进 checkpoint）。业务的 `RevisionSource` 若把事件写进需要与 DAO 一致的流，应在提交确认后再发布。

glsvet 的 A1 提示只看组件方法里的 undo 登记与组件自身字段的写，Runtime 不是组件，不会命中，无需豁免。`CombatComponent` 的字段只有 DAO 句柄与投影函数（`ProjectAttributes` 装上的行为），两者都不提示；自己写组件时，事务会改的状态放进 DAO，确属缓存的字段标 `//roost:cache`（见 [A1 字段写提示](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)）。

## 确定性掷点（暴击/闪避概率 → 事实）

伤害管线只接受预掷事实（`Dodge`/`ForceCritical`…）。`combat.ChanceRoll` / `combat.RollValue` 是产生这些事实的标准方式：

```go
// 同一 key + purpose + 坐标永远得到同一结果——副本与回放位一致。
crit := combat.ChanceRoll(matchSeed, "crit", critChanceBP,
    uint64(event.RootEventID), uint64(event.EventID), uint64(event.EffectIndex), uint64(target))
```

推荐坐标取效果命令 Event 上的 `RootEventID`/`EventID`/`EffectIndex` 加目标实体——每个伤害实例独立掷点且可复现；不同 purpose（"crit"/"dodge"）在同一坐标下相互独立。

## combatcomponent：roost-core 集成

`combatcomponent` 把 combat 电池接入 roost-core 实体模型：

- `CombatDao`：持有全部战斗状态，实现 `entity.DaoInterface` + `dataengine.Tracker` 契约 + `entity.PersistedDaoLoader`（BSON + schema 版本）与 nest 状态回滚接口；undo 策略下由 DAO 自己按字段掩码（vitals / attributes / buffs）登记逆操作并标脏，与生成 DAO 的 setter 同形。
- `CombatComponent`：只持有 DAO，全部 mutator 经 DAO 改状态，自己不登记 undo（回滚统一走 DAO，[A1](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）——handler 失败或提交被拒后，两种回滚策略下实体字节一致。
- **Runtime 不在事务里**（维护者决定 B4，见上文“Runtime 不在事务里（B4）”）：Nest 回滚只撤回 DAO；`skill.Runtime` 自己的状态不回退。
- `HostAdapter`：提供业务 Host 可委托的战斗面（自身不实现完整 `skill.Host`，Apply/Read 返回 handled 供业务路由）（damage/heal/shield 命令、attribute/resource 读取、原子 PayCosts），事件词表与 MemoryHost 一致（`damage_resolved`、`combat_hook_*`、`shield_absorbed`…），proc 过滤器在两种宿主上行为相同。`Select`/`StepSpawn`/空间查询/召唤物仍由业务 Host 实现。`HostAdapter.HostCapabilities()` 声明它负责的那部分 Host 能力表（Catalog 里可读的属性、`ResourceAttribute` 映射到的资源、四种资源 operation，接了 `Status` 再加 add / mul_bp 修正）；业务把自己负责的部分（衍生物 kind、motion 步骤、召唤物）用 `skill.MergeHostCapabilities` 合进来声明。Catalog 外或不可读的属性读取返回 `skill.ErrHostCapabilityMissing`（以前读出 0）。见 [B3 ③ 方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)。

### 属性投影（O2，v1.23.0）

维护者第十二轮决定（2026-10-06）：组件给投影入口，投影逻辑交给业务。以前经 `StatusBridge` / `ApplyBuff` 加的护甲等修饰只对 `HostAdapter.Read`（属性读取）可见，伤害管线读的 `Combatant.Armor` 不变，buff 对伤害没有任何效果。

业务写一个函数说明“哪个属性写到哪个伤害字段”，构造组件后装一次：

```go
const (
    attrArmor      combat.AttributeID = 1
    attrMagicResist combat.AttributeID = 2
    attrDamageTaken combat.AttributeID = 3 // 基点，10000 = 100%
)

func projectCombat(attribute func(combat.AttributeID) int64, c *combat.Combatant) {
    c.Armor = attribute(attrArmor)
    c.MagicResistance = attribute(attrMagicResist)
    c.DamageTakenBP = attribute(attrDamageTaken)
}

// 实体工厂里：
component := combatcomponent.NewCombatComponent(dao)
component.ProjectAttributes(projectCombat)
```

- **什么时候投影**：装上时一次；实体建好（新建或从存储加载，`OnInitFinish`）一次；之后每个改属性来源的 mutator（`InitCombatant`、`SetAttributeBase` / `SetAttributeBounds`、`ApplyBuff`、`RemoveBuff`、`SetBuffStacks`、`AdoptBuff`、`DispelBuffs`、`TickBuffs`，以及经它们落地的 `StatusBridge` 命令和资源命令）末尾、在同一事务里再投影一次。伤害读到的字段总是当前属性的结果。
- **回滚**：投影写的是 DAO 的 vitals（`FieldVitals`），与源字段同一笔逆操作 / 快照（[A1](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md) 的派生值规则）；handler 失败或提交被拒时随 DAO 回到事务开始时的值，不需要业务写任何回滚代码。加载与构造时不在事务里，直接写内存、不产生持久写；投影结果与现值相同时什么都不做。
- **投影函数的约束**：纯函数，只读传入的属性、只写由属性决定的字段（Armor、MagicResistance、Penetration、各 `*BP`、`MaxHealth` 等），不要改 `Health` / `Shield` / `Alive` 这类战斗过程状态；被投影的字段以投影为准，`InitCombatant` 里给的值会被覆盖。
- 完整可运行示例：`skill/examples/statusbridge`（破甲 status 让护甲 40 → 20，伤害随之变化，驱散后恢复）。


## B2 接入契约更正（2026-10-07）

- `ChanceRoll` 只返回确定性布尔值，不替 Host 注入本次伤害。Combatant 的 Dodge/ForceCritical 是持久 vitals，写一次会影响后续全部伤害；业务需要每伤害坐标重新计算，在本次结算结束后恢复临时事实，或以本次调用专属 `combat.Hooks` 实现暴击覆盖。statusbridge 示例恢复 ForceCritical 时读取结算后的 vitals，避免覆盖吸血等变化。HostAdapter 本身不提供每次概率注入器。
- `TickBuffs(now)` 由业务的 Nest 心跳/定时 handler 在实体锁内驱动；now 必须和 StatusBridge.CurrentTick 使用同一业务 tick。Runtime.Advance 不会扫描其他 DAO。掉线/重启后的首次推进也应清理过期 buff；该调用产生的持久和 Sync 脏位跟随本次事务提交。
- state 回滚只覆盖已声明并由 Nest 捕获的实体。伤害来源（含吸血）、目标、copy/transfer 的目的实体必须一并通过 RequestMulti 的目标或 Cast 动态纳入事务。Resolver 只返回已持锁对象，不是随意获取跨实体对象的入口；绕开这条约束的 DAO 不承诺 state 回滚。正式两种策略 × handler 错误/提交拒绝回归验证了真实吸血和 AdoptBuff 目的实体恢复，不能把该测试解释成未捕获对象也有保护。
- ResourceRead/PayCosts/ResourceCommand 都使用映射属性的 base 作为可支付资源池；AttributeRead 读取包含 buff 的 Current。资源修饰不会凭空制造可消费余额。
- 属性修饰聚合使用精确整数桶，撤销可逆、与施加顺序无关；Current 进入固定点公式时钳位。持续时间 0 表示永久，refresh 永久实例不转成 1 tick；重施永久规格清除旧到期，层数按新上限钳位。
