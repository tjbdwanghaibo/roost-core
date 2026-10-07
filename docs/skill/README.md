# 技能系统（roost-core/skill，原 roost-skill）文档导航

本文档目录按使用角色组织。Go 的稳定核心包路径是
`github.com/tjbdwanghaibo/roost-core/skill`；JSON wire schema
`roost.skill/v2` 和编译器语义 `skillv2-compiler-2` 是独立的持久协议版本，
不会再体现在 Go 目录名中。

## 术语：召唤物（Summon）与衍生物（Spawn）

两个概念，名字不要混用：

| | 召唤物（Summon） | 衍生物（Spawn） |
| --- | --- | --- |
| 是什么 | 用单位模板（`UnitTemplateCatalog`）在场景里生成的真实单位：陷阱、宠物、图腾、墙等，归施法者所有 | 技能施放时生成、之后每个 tick 由技能驱动的东西：飞行物、法术场 / 光环、光束、位移、环绕，以及驱动召唤物的随从（minion） |
| 谁管 | 宿主：单位的存活、指令、到期与清理在业务 Host 里，Runtime 只经事务创建它、经指令指挥它 | Runtime：Runtime 逐 tick 推进（从召唤后的下一个 tick 起，施法结束前后都一样，RR-20261006-51）、派发信号、执行回调，宿主只执行每一步运动与停止 |
| DSL | 效果 `{"type":"summon","template":…,"count":…,"duration_ticks":…}`；`{"type":"dismiss","target":…}`；`issue_entity_command`；owned 选择 `summoned_before` / `summoned_after`、排序 `summon_tick` / `summon_sequence` | effect flow 上的 `"spawn": {"kind": …}` 与 `on` 回调；`$spawn`、`modify_spawn`、`spawn_start`、`spawn_step`、`spawn_callback` |
| Host | `OwnedEntityRuntimeHost.PreviewOwnedSummon` / `CommitOwnedSummon` / `RollbackOwnedSummon`，命令 `SummonCommand`，结果 `SummonEffectResult` | `Host.StepSpawn(SpawnStepCommand, SpawnHostState)` / `StopSpawn(SpawnStopCommand, SpawnHostState)` |
| 同步 | 单位本身按业务实体同步，skill 不发它的 mutation | state mutation `spawn_upsert` / `spawn_remove`，表现 `spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop` |

两者常一起出现：召唤效果可以带一个衍生物定义，衍生物以召唤出的单位为 lifecycle 实体运行，例如
`{"flow":"effect","effect":{"type":"summon","template":"deployable.trap",…},"spawn":{"kind":"area",…},"on":{"enter":…}}`
是“放一个陷阱，陷阱周围是一个 area 衍生物”。衍生物的 `kind` 为 projectile / area / minion / beam / dash / orbit；`minion` 只跟着召唤物活（寿命取召唤效果的 `duration_ticks`），不做运动与成员检测。

2026-10-07 起（维护者第十三轮“skill 生成宿主实体改名”）生成宿主单位那一套改叫 Summon，衍生物的 `kind: "summon"` 改为 `kind: "minion"`，不保留旧名：

| 旧名 | 新名 |
| --- | --- |
| 效果 `"type":"spawn"`、`"type":"despawn"`；指令与单位模板生命周期策略 `despawn` | `"type":"summon"`、`"type":"dismiss"`；`dismiss` |
| owned 选择 `spawned_before` / `spawned_after`、排序 `spawn_tick` / `spawn_sequence`；effect result 类型 `spawn_result` | `summoned_before` / `summoned_after`、`summon_tick` / `summon_sequence`；`summon_result` |
| 衍生物 `"kind":"summon"` | `"kind":"minion"` |
| Host `PreviewOwnedSpawn(SpawnCommand) (OwnedSpawnPreview, error)`、`CommitOwnedSpawn` / `RollbackOwnedSpawn(OwnedSpawnTransactionID)` | `PreviewOwnedSummon(SummonCommand) (OwnedSummonPreview, error)`、`CommitOwnedSummon` / `RollbackOwnedSummon(OwnedSummonTransactionID)` |
| `SpawnEffectResult`、`SpawnAttributeOverride`、`SpawnParameterBinding`、`OwnedSpawnTickFilter`、`OwnedEntityMetadata.SpawnTick` / `SpawnSequence`、`UnitTemplateCatalogEntry.MaximumSpawnCount` | `SummonEffectResult`、`SummonAttributeOverride`、`SummonParameterBinding`、`OwnedSummonTickFilter`、`SummonTick` / `SummonSequence`、`MaximumSummonCount` |
| MemoryHost 事件 `owned_entity_spawned` / `owned_entity_spawn_rolled_back` / `owned_entity_despawned` | `owned_entity_summoned` / `owned_entity_summon_rolled_back` / `owned_entity_dismissed` |
| checkpoint 版本 4 | 版本 5（cast 值里的 `spawn_result` 改为 `summon_result`），旧版本拒绝恢复 |

完整对照与逐个判定（`SpawnCommandMeta`、`OwnedSpawns`、`OwnedSpawnSnapshot` 等为什么属于衍生物、不改）见
[重构记录](../feature/REFACTOR-2026-10-07-skill-summon-rename.md)。

<a id="术语衍生物spawn"></a>
2026-10-06 起（维护者第十三轮决定）原来的“进程”（process）全量改名为 Spawn，不保留旧名：

| 旧名 | 新名 |
| --- | --- |
| effect flow 的 `"process"`、`modify_process`（`"process"` 参数）、`$process` | `"spawn"`、`modify_spawn`（`"spawn"` 参数）、`$spawn` |
| 快照点与求值上下文 `process_start` / `process_step` / `process_callback` | `spawn_start` / `spawn_step` / `spawn_callback` |
| 编译环境 `process_properties` / `process_kinds` / `motion.process_trajectory_pairs` | `spawn_properties` / `spawn_kinds` / `motion.spawn_trajectory_pairs` |
| state mutation `process_upsert` / `process_remove`，表现 `process_start` / `process_update` / `process_signal` / `process_stop` | `spawn_upsert` / `spawn_remove`，`spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop` |
| Host `StepProcess` / `StopProcess`、`ProcessStepCommand` / `ProcessStopCommand` / `ProcessHostState` / `ProcessID` | `StepSpawn` / `StopSpawn`、`SpawnStepCommand` / `SpawnStopCommand` / `SpawnHostState` / `SpawnID` |
| `RuntimeOptions.ProcessStopRetryBackoff` / `ProcessStopRetryLimit` / `MaxStopPendingProcesses` / `MaxOwnedProcesses*` | `SpawnStopRetryBackoff` / `SpawnStopRetryLimit` / `MaxStopPendingSpawns` / `MaxOwnedSpawns*` |
| 指标 `skill.process.stop_retry_exhausted.total` / `skill.process.stop_pending_dropped.total` | `skill.spawn.stop_retry_exhausted.total` / `skill.spawn.stop_pending_dropped.total`（10-07 待停止上限选 B 起改为 `skill.spawn.abandoned.total`，超限不再删记录，见[分区方案 §11](../feature/REFACTOR-2026-10-07-skill-spawn-partition.md#11-待停止上限改为已放弃分区维护者第十三轮待停止上限选-b2026-10-07)） |
| checkpoint 版本 3（`processes`、`owned_processes`、`next_process_id` …） | 版本 4（`spawns`、`owned_spawns`、`next_spawn_id` …），旧版本拒绝恢复 |

完整对照（全部导出 / 未导出标识符、JSON 字段、文件名）与没改的部分（`proc` 是被动“触发”，不是 process）见
[重构记录](../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md)。

## 新接入项目

1. [稳定 Skill API 与最小接入](skill.md)
2. [施法语义与战斗内容电池](skill-casting-and-combat.md)
3. [架构、Host 边界与同步流程](architecture-and-migration.md)
4. [Visual 与数据同步生产指南](visual-sync-production-guide.md)

## 框架维护者

1. [当前实现学习手册](skill-implementation-guide.md)
2. [阅读与测试清单](skill-testing-guide.md)
3. [生产门槛](production-readiness.md)

## 升级与生成

- 从旧 `/skillv2` Go 包迁移：[稳定包破坏性升级手册](breaking-upgrade-skill-package.md)
- AI 生成约束：[system prompt](ai-skill-system-prompt.md) 与
  [user prompt](ai-skill-user-prompt.md)

## 文档事实来源

- Go API：源码注释与 `go doc github.com/tjbdwanghaibo/roost-core/skill`
- JSON 字段：`skill/wire_*.go`、严格 parser 和 `skill/testdata/*.json`
- 运行语义：`skill/runtime*.go`、`skill/host*.go` 与验收测试
- 生产约束：`production-readiness.md` 和 CI；README 只提供入口，不覆盖这些门槛

修改公开类型、wire 字段、checkpoint、同步 schema 或 Host 契约时，必须在同一提交中
更新对应文档、fixture、迁移说明和测试。
