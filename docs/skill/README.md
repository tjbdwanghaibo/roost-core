# 技能系统（roost-core/skill，原 roost-skill）文档导航

本文档目录按使用角色组织。Go 的稳定核心包路径是
`github.com/tjbdwanghaibo/roost-core/skill`；JSON wire schema
`roost.skill/v2` 和编译器语义 `skillv2-compiler-2` 是独立的持久协议版本，
不会再体现在 Go 目录名中。

## 术语：衍生物（Spawn）

技能施放时生成、之后每个 tick 由技能驱动的东西——飞行物、法术场 / 光环、召唤物、光束、位移、环绕——叫**衍生物（Spawn）**。
DSL 里 `spawn` 效果生成实体，挂在同一个 effect flow 上的 `spawn` 定义（`kind` 为 projectile / area / summon / beam / dash / orbit）就是在这些实体上运行的衍生物，`on` 是它的回调。

2026-10-06 起（维护者第十三轮决定）原来的“进程”（process）全量改名为 Spawn，不保留旧名：

| 旧名 | 新名 |
| --- | --- |
| effect flow 的 `"process"`、`modify_process`（`"process"` 参数）、`$process` | `"spawn"`、`modify_spawn`（`"spawn"` 参数）、`$spawn` |
| 快照点与求值上下文 `process_start` / `process_step` / `process_callback` | `spawn_start` / `spawn_step` / `spawn_callback` |
| 编译环境 `process_properties` / `process_kinds` / `motion.process_trajectory_pairs` | `spawn_properties` / `spawn_kinds` / `motion.spawn_trajectory_pairs` |
| state mutation `process_upsert` / `process_remove`，表现 `process_start` / `process_update` / `process_signal` / `process_stop` | `spawn_upsert` / `spawn_remove`，`spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop` |
| Host `StepProcess` / `StopProcess`、`ProcessStepCommand` / `ProcessStopCommand` / `ProcessHostState` / `ProcessID` | `StepSpawn` / `StopSpawn`、`SpawnStepCommand` / `SpawnStopCommand` / `SpawnHostState` / `SpawnID` |
| `RuntimeOptions.ProcessStopRetryBackoff` / `ProcessStopRetryLimit` / `MaxStopPendingProcesses` / `MaxOwnedProcesses*` | `SpawnStopRetryBackoff` / `SpawnStopRetryLimit` / `MaxStopPendingSpawns` / `MaxOwnedSpawns*` |
| 指标 `skill.process.stop_retry_exhausted.total` / `skill.process.stop_pending_dropped.total` | `skill.spawn.stop_retry_exhausted.total` / `skill.spawn.stop_pending_dropped.total` |
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
