# 文档清单与整理规则

根目录只保留 README、CHANGELOG、AGENTS 和 LICENSE；正文按下表归类。功能契约、性能证据和发布状态分别维护，避免把过期计划当现行要求。当前入口以[文档首页](../README.md)为准，维护规则见[维护手册 §6](README.md#6-维护文档的规则)。

| 位置 | 内容 |
| --- | --- |
| `docs/framework` | 设计、实现、专项契约与包映射 |
| `docs/performance` | 性能报告、复跑条件及可移植样本（按日期或版本固定，不随代码改写结论） |
| `docs/maintenance` | 升级、故障、已知限制、外部验收和文档治理 |
| `docs/release` | 按版本固定的发布说明与功能验收；`history/` 是合仓前各仓的只读变更记录 |
| `docs/skill` | 游戏内 Skill 使用与实现参考，不是 agent 规范 |
| 代码附近 | 各包 README、codegen 用户文档、demo 模板说明、脚本手册 |

## 入口

- [README](../../README.md)：仓库介绍、目录职责、核心执行边界与验证命令
- [AGENTS](../../AGENTS.md)：维护 agent 入口与 codebase-memory 用法
- [CHANGELOG](../../CHANGELOG.md)：唯一维护的变更记录（`[Unreleased]` 是未发版变化与升级要求）
- [docs/README.md](../README.md)：文档首页
- [docs/GETTING-STARTED.md](../GETTING-STARTED.md)：第一次认识和使用 Roost

## docs/framework

- [README.md](../framework/README.md)：模块目录（核心三块、次核心、其他包与专项契约）
- [PACKAGES.md](../framework/PACKAGES.md)：全包源码索引
- [PACKAGE-REORGANIZATION.md](../framework/PACKAGE-REORGANIZATION.md)：包目录分类、Wiring 与 Service 收敛方案（随 v1.25.0 交付）
- [RUNTIME_EXECUTION_MODEL.md](../framework/RUNTIME_EXECUTION_MODEL.md)：业务执行模型
- [NEST-AWAIT.md](../framework/NEST-AWAIT.md)：Nest short/long/io 三池、匿名异步恢复、池命名迁移表
- [NEST-OPTIMIZATION.md](../framework/NEST-OPTIMIZATION.md)：Nest 热路径优化设计、已保留改动及延期候选
- [NEST_TRANSACTION_WAL.md](../framework/NEST_TRANSACTION_WAL.md)：Nest 事务、WAL 与 Outbox
- [NEST_PIPELINED_COMMIT.md](../framework/NEST_PIPELINED_COMMIT.md)：Pipelined 提交（DurabilityPipelined）
- [ENTITY_SYNC.md](../framework/ENTITY_SYNC.md)：Entity Sync 生产契约
- [SYNC-OPTIMIZATION.md](../framework/SYNC-OPTIMIZATION.md)：Controller 生成边界与 Sync（Flush、AOI、Relation、Lockstep）局部优化及同机基准
- [INTEREST-BLOCK-AOI.md](../framework/INTEREST-BLOCK-AOI.md)：Interest 的 Block AOI 与手写空间组件
- [PLAYER-ACCESS.md](../framework/PLAYER-ACCESS.md)：玩家协议、Controller、生成 Sender 与 Nest handler
- [GATEWAY.md](../framework/GATEWAY.md)：Gate 与 Game 接入
- [GATEWAY-IMPLEMENTATION.md](../framework/GATEWAY-IMPLEMENTATION.md)：独立 Gate 设计评审与实施方案
- [REMOTE_ENTITY.md](../framework/REMOTE_ENTITY.md)：Remote Entity 生产协议
- [SAGA.md](../framework/SAGA.md)：Saga 契约
- [OBSERVABILITY.md](../framework/OBSERVABILITY.md)：可观测性规范与指标清单（根包门禁核对指标名）
- `guide/00`～`guide/14`：各模块“设计与使用”，其中 [guide/02](../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试) 含回复错误判别表
- `impl/00`～`impl/14`：各模块“实现与维护”（必须保持的契约、源码与回归入口）

| 编号 | 模块 | 设计与使用 | 实现与维护 |
| --- | --- | --- | --- |
| 00 | 总览与端到端设计 | [guide](../framework/guide/00-overview.md) | [impl](../framework/impl/00-overview.md) |
| 01 | App 与生命周期 | [guide](../framework/guide/01-app-lifecycle.md) | [impl](../framework/impl/01-app-lifecycle.md) |
| 02 | Nest 调度与实体 | [guide](../framework/guide/02-nest-entity.md) | [impl](../framework/impl/02-nest-entity.md) |
| 03 | DataEngine 持久化 | [guide](../framework/guide/03-dataengine.md) | [impl](../framework/impl/03-dataengine.md) |
| 04 | Sync、Lockstep 与客户端 | [guide](../framework/guide/04-sync.md) | [impl](../framework/impl/04-sync.md) |
| 05 | Remote Entity 与 Mirror | [guide](../framework/guide/05-remote-mirror.md) | [impl](../framework/impl/05-remote-mirror.md) |
| 06 | Saga 长事务 | [guide](../framework/guide/06-saga.md) | [impl](../framework/impl/06-saga.md) |
| 07 | 配置、数据表与热更 | [guide](../framework/guide/07-config.md) | [impl](../framework/impl/07-config.md) |
| 08 | 游戏技能、战斗与空间 | [guide](../framework/guide/08-skill.md) | [impl](../framework/impl/08-skill.md) |
| 09 | Service 领域能力 | [guide](../framework/guide/09-services.md) | [impl](../framework/impl/09-services.md) |
| 10 | 时间与定时器 | [guide](../framework/guide/10-time.md) | [impl](../framework/impl/10-time.md) |
| 11 | 观测、安全与运维 | [guide](../framework/guide/11-observability.md) | [impl](../framework/impl/11-observability.md) |
| 12 | Codegen 与工程工具 | [guide](../framework/guide/12-codegen.md) | [impl](../framework/impl/12-codegen.md) |
| 13 | Wiring 装配 | [guide](../framework/guide/13-wiring.md) | [impl](../framework/impl/13-wiring.md) |
| 14 | 其他包与公共基础设施 | [guide](../framework/guide/14-foundation.md) | [impl](../framework/impl/14-foundation.md) |

## docs/performance

- [README.md](../performance/README.md)：性能验证与复跑入口
- [STABLE-v1.24.0.md](../performance/STABLE-v1.24.0.md)：v1.24.0 稳定版性能验收
- [GATE-AOI.md](../performance/GATE-AOI.md)：Gate、Nest 与 Block AOI 性能验证
- [NEST-DISPATCH-BENCHMARK.md](../performance/NEST-DISPATCH-BENCHMARK.md)：Nest 共享调度与按 ID 分片的对照基准（池名为当时的 fast/slow）
- [V1270-HEAVY-30M-20261014.md](../performance/V1270-HEAVY-30M-20261014.md)：v1.27.0 发布后同参数 30 分钟重档复测（通过）与 15 分钟样本对照
- [V1260-HEAVY-20261013.md](../performance/V1260-HEAVY-20261013.md)：v1.26.0 发布后同参数 15 分钟重档复测（通过）与上一样本对照
- [REVIEW-20261012.md](../performance/REVIEW-20261012.md)：2026-10-12 新增提交的修复与性能验证、合批测试修复证据
- [NEST-HOT-PATH-20261013.md](../performance/NEST-HOT-PATH-20261013.md)：Nest 六项局部优化的分批对照、矩阵与配置入口修复
- [benchmarks/](../performance/benchmarks/)：随仓原始样本（大型本地产物归忽略的 `artifacts/`）

## docs/maintenance

- [COMMIT-REVIEW.md](COMMIT-REVIEW.md)：新增提交的分批范围、独立验证、问题复现与接续入口
- [REVIEW-COMMITS-20261014.csv](REVIEW-COMMITS-20261014.csv)：固定范围内 249 个非合并提交的路径与审查状态

- [README.md](README.md)：维护手册（版本与兼容、升级回退、日常验证、故障、发版、文档规则、测试与生成代码、可读性要求）
- [DESIGN-TODO.md](DESIGN-TODO.md)：待维护者决定的设计事项（Lockstep 历史保留、SyncBus 兜底、Drain 放弃出口、schema 迁移流程、Player 跨机迁移）
- [KNOWN-LIMITS.md](KNOWN-LIMITS.md)：未解决问题与验证边界的唯一位置
- [EXTERNAL-VERIFICATION.md](EXTERNAL-VERIFICATION.md)：需要外部环境的验证清单
- [CONSISTENCY.md](CONSISTENCY.md)：v1.24.1 文档整理时的文档—代码一致性核对（按当时源码固定）
- [ACCESS-SYNC-FOLLOWUPS.md](ACCESS-SYNC-FOLLOWUPS.md)：玩家接入现状、Session 服务可选化与 Sync 命名后续事项
- [DOCUMENTS.md](DOCUMENTS.md)：本清单

## docs/release

- [v1.27.0-NOTES.md](../release/v1.27.0-NOTES.md)：v1.27.0 发布说明（playeraccess 改名与升级步骤、Remote WAL 级 Rejected 精简）
- [v1.27.0-IMPLEMENTATION.md](../release/v1.27.0-IMPLEMENTATION.md)：v1.27.0 提交、位置、可达性梳理与红绿证据
- [v1.27.0-VALIDATION.md](../release/v1.27.0-VALIDATION.md)：v1.27.0 发布验收
- [v1.26.1-NOTES.md](../release/v1.26.1-NOTES.md)：v1.26.1 补丁说明（安全、go.work、demo 模板、CI 修复）
- [v1.26.1-IMPLEMENTATION.md](../release/v1.26.1-IMPLEMENTATION.md)：v1.26.1 各项提交、位置与红绿证据
- [v1.26.1-VALIDATION.md](../release/v1.26.1-VALIDATION.md)：v1.26.1 发布验收
- [v1.26.0-NOTES.md](../release/v1.26.0-NOTES.md)：v1.26.0 发布说明（三池与 Await、Sync 优化、旧兼容移除与全仓整理；升级要求速查、维护者决定、最低版本）
- [v1.26.0-IMPLEMENTATION.md](../release/v1.26.0-IMPLEMENTATION.md)：v1.26.0 实现与证据（与说明篇同编号：提交、源码位置、守卫、红绿、review 检查点）
- [v1.26.0-VALIDATION.md](../release/v1.26.0-VALIDATION.md)：v1.26.0 发布验收（本地命令、pretag、故障矩阵、真实 tag 消费者、15 分钟性能）
- [v1.25.0-NOTES.md](../release/v1.25.0-NOTES.md)：v1.25.0 发布说明（目录分类、Gate 与 Block AOI）
- [v1.25.0-IMPLEMENTATION.md](../release/v1.25.0-IMPLEMENTATION.md)：v1.25.0 实现与验证
- [v1.25.0-VALIDATION.md](../release/v1.25.0-VALIDATION.md)：v1.25.0 功能验收
- [v1.24.1-NOTES.md](../release/v1.24.1-NOTES.md) / [v1.24.1-IMPLEMENTATION.md](../release/v1.24.1-IMPLEMENTATION.md)：v1.24.1 文档维护版
- [v1.24.0-NOTES.md](../release/v1.24.0-NOTES.md) / [v1.24.0-IMPLEMENTATION.md](../release/v1.24.0-IMPLEMENTATION.md)：v1.24.0 稳定基线
- [history/codegen-CHANGELOG.md](../release/history/codegen-CHANGELOG.md)：roost-codegen 合仓前的变更记录（只读，原 `codegen/CHANGELOG.md`）
- [history/wiring-CHANGELOG.md](../release/history/wiring-CHANGELOG.md)：roost-kit 合仓前的变更记录（只读，原 `wiring/CHANGELOG.md`）
- [history/service-CHANGELOG.md](../release/history/service-CHANGELOG.md)：roost-service 合仓前的变更记录（只读，原 `wiring/docs/history/SERVICE_CHANGELOG.md`）

## docs/skill

- [README.md](../skill/README.md)：游戏 Skill 文档导航
- [skill.md](../skill/skill.md)：稳定 Skill API 与最小接入（`gameplay/skill` 的文档测试读取它）
- [skill-casting-and-combat.md](../skill/skill-casting-and-combat.md)：施法语义、求值上下文、既定语义与战斗内容电池
- [skill-implementation-guide.md](../skill/skill-implementation-guide.md)：Skill 当前实现学习手册
- [skill-testing-guide.md](../skill/skill-testing-guide.md)：Skill 阅读与测试指南
- [visual-sync-production-guide.md](../skill/visual-sync-production-guide.md)：Skill Visual 与数据同步生产指南

## 代码附近的文档

- [codegen/README.md](../../codegen/README.md)：生成器入口与快速上手（文档分工表）
- [codegen/docs/CODEGEN_REFERENCE.zh-CN.md](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)：命令与标记参考（tablegen 错误文本引用其 §9.1）
- [codegen/docs/PROJECT_GENERATOR.zh-CN.md](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md)：工程生成、roost.yaml、Mod 装配、升级与部署生成物
- [codegen/docs/BEGINNER_WORKBOOK.zh-CN.md](../../codegen/docs/BEGINNER_WORKBOOK.zh-CN.md)：小白逐步操作手册
- [codegen/docs/NEST_RUNTIME.zh-CN.md](../../codegen/docs/NEST_RUNTIME.zh-CN.md)：生成的 Sender 与 handler 的执行模型
- [codegen/docs/PLAYER_ACCESS_TCP.zh-CN.md](../../codegen/docs/PLAYER_ACCESS_TCP.zh-CN.md)：玩家 TCP 接入
- [codegen/docs/CFGGEN_META.zh-CN.md](../../codegen/docs/CFGGEN_META.zh-CN.md)：cfggen schema 字段级参考
- [codegen/docs/DATA_FLOW.zh-CN.md](../../codegen/docs/DATA_FLOW.zh-CN.md)：生成工程的六条数据路径
- [codegen/docs/CI_CD_IMPLEMENTATION.zh-CN.md](../../codegen/docs/CI_CD_IMPLEMENTATION.zh-CN.md)：仓库工作流、发布列车与生成工程流水线
- [demo/README.md](../../demo/README.md)：game-demo 模板的生成、本地实跑与常见问题
- [demo/WALKTHROUGH.md](../../demo/WALKTHROUGH.md)：game-demo 功能逐项说明
- [gameplay/skill/README.md](../../gameplay/skill/README.md)：Skill 包入口、快速启动、JSON 参考与核心概念
- [framework/sync/README.md](../../framework/sync/README.md)：Sync 子包职责与设计记录入口
- [client/README.md](../../client/README.md)：客户端协议与 C# SDK
- [wiring/README.md](../../wiring/README.md)：Wiring 入口
- [wiring/SERVICES.md](../../wiring/SERVICES.md)：通用服务层的设计约束与各服务取舍
- [wiring/scripts/integration/README.md](../../wiring/scripts/integration/README.md)：集成环境脚本
- [scripts/README.md](../../scripts/README.md)：仓库级脚本与性能工具手册
- [infra/network/nats/driver/README.md](../../infra/network/nats/driver/README.md)、[infra/storage/mongo/driver/README.md](../../infra/storage/mongo/driver/README.md)、[infra/storage/redis/driver/README.md](../../infra/storage/redis/driver/README.md)：驱动的结果未知与 Close 契约

## 删除与历史追溯

开发过程资料（旧 docs/bug、bugfix、review、feature、agent-skills、USER_GUIDE、TROUBLESHOOTING、交接文档）已于 v1.24.1 整理时移入 Git 历史，按固定提交追溯，不在 main 上重建这些目录：

- [v1.24.1 清理前完整 docs](https://github.com/tjbdwanghaibo/roost-core/tree/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs)：旧 RR、bugfix、review、feature、USER_GUIDE、TROUBLESHOOTING 与 agent 规范；现行文档引用它们一律用 `9d955fb0` 固定提交链接，不能用 main 旧路径；
- [旧生产就绪清单](https://github.com/tjbdwanghaibo/roost-core/blob/f25527db2f036b1a790ad4577ad865c1396666be/PRODUCTION_READINESS.md) 与 [历史框架评估](https://github.com/tjbdwanghaibo/roost-core/blob/f25527db2f036b1a790ad4577ad865c1396666be/ROOST_FRAMEWORK_ASSESSMENT.md)：已从根目录删除，不作为当前依据；
- USER_GUIDE §4 的回复错误判别表已按现行错误值重写进 [guide/02 §6](../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)；
- 维护手册 §7 曾按日期累积的测试整理记录，收敛前的原文见[固定提交](https://github.com/tjbdwanghaibo/roost-core/blob/ea776b4a977f29661932896b11aea80dd00ea3a0/docs/maintenance/README.md)。

v1.24.1 历史清理基线：原 docs 跟踪 3070 个文件，整理时保留/新建 50 个、删除旧路径 3034 个；这是当时统计，不是当前文件数。源码历史注释里的 RR 编号可从 Git 追溯，不为文档搬迁改动生产逻辑。删除文件留下的空目录只按确认为空后清理，不递归删除本地资料。
