# 文档清单与整理规则

根目录保留 README、CHANGELOG、AGENTS 和 LICENSE；正文按下表归类。功能契约、性能证据和发布状态分别维护，避免把过期计划当现行要求。当前入口以[文档首页](../README.md)为准。

| 目录 | 内容 |
| --- | --- |
| framework | 设计、实现、专项契约与包映射 |
| performance | 性能报告、复跑条件及可移植样本 |
| maintenance | 升级、故障、已知限制、外部验收和文档治理 |
| release | 按版本固定的发布说明与功能验收 |
| skill | 游戏内Skill使用与实现参考，不是agent规范 |

## 当前docs文件

### framework

- [framework/ENTITY_SYNC.md](../framework/ENTITY_SYNC.md)：Entity Sync 生产契约
- [framework/GATEWAY-IMPLEMENTATION.md](../framework/GATEWAY-IMPLEMENTATION.md)：独立 Gate：设计评审与实施方案
- [framework/GATEWAY.md](../framework/GATEWAY.md)：Gate 与 Game 接入
- [framework/INTEREST-BLOCK-AOI.md](../framework/INTEREST-BLOCK-AOI.md)：Interest 的 Block AOI 与手写空间组件
- [framework/NEST_PIPELINED_COMMIT.md](../framework/NEST_PIPELINED_COMMIT.md)：Nest Pipelined Commit（DurabilityPipelined）设计与实施
- [framework/NEST_TRANSACTION_WAL.md](../framework/NEST_TRANSACTION_WAL.md)：Nest Transaction、WAL 与 Outbox 生产方案
- [framework/OBSERVABILITY.md](../framework/OBSERVABILITY.md)：roost 可观测性规范与指标清单
- [framework/PACKAGE-REORGANIZATION.md](../framework/PACKAGE-REORGANIZATION.md)：包目录分类、Wiring 与 Service 收敛方案
- [framework/PACKAGES.md](../framework/PACKAGES.md)：全包源码索引
- [framework/README.md](../framework/README.md)：Roost v1.25 文档
- [framework/REMOTE_ENTITY.md](../framework/REMOTE_ENTITY.md)：Remote Entity 生产协议
- [framework/RUNTIME_EXECUTION_MODEL.md](../framework/RUNTIME_EXECUTION_MODEL.md)：Roost 业务执行模型
- [framework/SAGA.md](../framework/SAGA.md)：Roost Saga
- [framework/guide/00-overview.md](../framework/guide/00-overview.md)：总览与端到端设计：设计与使用
- [framework/guide/01-app-lifecycle.md](../framework/guide/01-app-lifecycle.md)：App 与生命周期：设计与使用
- [framework/guide/02-nest-entity.md](../framework/guide/02-nest-entity.md)：核心：Nest 调度与实体：设计与使用
- [framework/guide/03-dataengine.md](../framework/guide/03-dataengine.md)：核心：DataEngine 持久化：设计与使用
- [framework/guide/04-sync.md](../framework/guide/04-sync.md)：核心：Sync、Lockstep 与客户端：设计与使用
- [framework/guide/05-remote-mirror.md](../framework/guide/05-remote-mirror.md)：Remote Entity 与 Mirror：设计与使用
- [framework/guide/06-saga.md](../framework/guide/06-saga.md)：Saga 长事务：设计与使用
- [framework/guide/07-config.md](../framework/guide/07-config.md)：配置、数据表与热更：设计与使用
- [framework/guide/08-skill.md](../framework/guide/08-skill.md)：游戏技能、战斗与空间：设计与使用
- [framework/guide/09-services.md](../framework/guide/09-services.md)：次核心：Service 领域能力：设计与使用
- [framework/guide/10-time.md](../framework/guide/10-time.md)：时间与定时器：设计与使用
- [framework/guide/11-observability.md](../framework/guide/11-observability.md)：观测、安全与运维：设计与使用
- [framework/guide/12-codegen.md](../framework/guide/12-codegen.md)：次核心：Codegen 与工程工具：设计与使用
- [framework/guide/13-wiring.md](../framework/guide/13-wiring.md)：次核心：Wiring 装配：设计与使用
- [framework/guide/14-foundation.md](../framework/guide/14-foundation.md)：其他包与公共基础设施：设计与使用
- [framework/impl/00-overview.md](../framework/impl/00-overview.md)：总览与端到端设计：实现与维护
- [framework/impl/01-app-lifecycle.md](../framework/impl/01-app-lifecycle.md)：App 与生命周期：实现与维护
- [framework/impl/02-nest-entity.md](../framework/impl/02-nest-entity.md)：核心：Nest 调度与实体：实现与维护
- [framework/impl/03-dataengine.md](../framework/impl/03-dataengine.md)：核心：DataEngine 持久化：实现与维护
- [framework/impl/04-sync.md](../framework/impl/04-sync.md)：核心：Sync、Lockstep 与客户端：实现与维护
- [framework/impl/05-remote-mirror.md](../framework/impl/05-remote-mirror.md)：Remote Entity 与 Mirror：实现与维护
- [framework/impl/06-saga.md](../framework/impl/06-saga.md)：Saga 长事务：实现与维护
- [framework/impl/07-config.md](../framework/impl/07-config.md)：配置、数据表与热更：实现与维护
- [framework/impl/08-skill.md](../framework/impl/08-skill.md)：游戏技能、战斗与空间：实现与维护
- [framework/impl/09-services.md](../framework/impl/09-services.md)：次核心：Service 领域能力：实现与维护
- [framework/impl/10-time.md](../framework/impl/10-time.md)：时间与定时器：实现与维护
- [framework/impl/11-observability.md](../framework/impl/11-observability.md)：观测、安全与运维：实现与维护
- [framework/impl/12-codegen.md](../framework/impl/12-codegen.md)：次核心：Codegen 与工程工具：实现与维护
- [framework/impl/13-wiring.md](../framework/impl/13-wiring.md)：次核心：Wiring 装配：实现与维护
- [framework/impl/14-foundation.md](../framework/impl/14-foundation.md)：其他包与公共基础设施：实现与维护

### performance

- [performance/GATE-AOI.md](../performance/GATE-AOI.md)：Gate、Nest 与 Block AOI 性能验证
- [performance/NEST-DISPATCH-BENCHMARK.md](../performance/NEST-DISPATCH-BENCHMARK.md)：Nest 共享调度与按 ID 分片的对照基准
- [performance/README.md](../performance/README.md)：性能验证与复跑
- [performance/STABLE-v1.24.0.md](../performance/STABLE-v1.24.0.md)：v1.24.0 稳定版性能验收

### maintenance

- [maintenance/CONSISTENCY.md](CONSISTENCY.md)：文档与代码一致性核对
- [maintenance/DOCUMENTS.md](DOCUMENTS.md)：最终保留文档清单
- [maintenance/EXTERNAL-VERIFICATION.md](EXTERNAL-VERIFICATION.md)：外部验证操作清单
- [maintenance/KNOWN-LIMITS.md](KNOWN-LIMITS.md)：当前已知限制与验证边界
- [maintenance/README.md](README.md)：v1.25 维护手册

### release

- [release/v1.24.0-IMPLEMENTATION.md](../release/v1.24.0-IMPLEMENTATION.md)：v1.24.0 实现与验收
- [release/v1.24.0-NOTES.md](../release/v1.24.0-NOTES.md)：v1.24.0 稳定基线说明
- [release/v1.24.1-IMPLEMENTATION.md](../release/v1.24.1-IMPLEMENTATION.md)：v1.24.1 实现与验收
- [release/v1.24.1-NOTES.md](../release/v1.24.1-NOTES.md)：v1.24.1 文档维护版
- [release/v1.25.0-IMPLEMENTATION.md](../release/v1.25.0-IMPLEMENTATION.md)：v1.25.0 实现与验证
- [release/v1.25.0-NOTES.md](../release/v1.25.0-NOTES.md)：v1.25.0：目录分类、Gate 与 Block AOI
- [release/v1.25.0-VALIDATION.md](../release/v1.25.0-VALIDATION.md)：v1.25.0 目录、Gate 与 Block AOI 功能验收

### skill

- [skill/README.md](../skill/README.md)：游戏 Skill 专项手册
- [skill/skill-casting-and-combat.md](../skill/skill-casting-and-combat.md)：施法语义与战斗内容电池（v1.4+）
- [skill/skill-implementation-guide.md](../skill/skill-implementation-guide.md)：Skill 当前实现学习手册
- [skill/skill-testing-guide.md](../skill/skill-testing-guide.md)：Skill 阅读与测试指南
- [skill/skill.md](../skill/skill.md)：稳定 Skill API 与最小接入
- [skill/visual-sync-production-guide.md](../skill/visual-sync-production-guide.md)：Skill Visual 与数据同步生产指南

入口另含 [README](../README.md) 与 [GETTING-STARTED](../GETTING-STARTED.md)。随仓性能原始数据见 [benchmarks](../performance/benchmarks/)，大型本地产物归忽略的 artifacts，不混入本清单。

## 删除与历史追溯

本轮删除根目录 `PRODUCTION_READINESS.md` 与 `ROOST_FRAMEWORK_ASSESSMENT.md`：前者包含旧多仓发布顺序、旧Remote权威/Sync口径，现行要求已归维护手册和各模块契约；后者是2026-08-27的历史比较，不作为当前架构依据。

- [旧生产就绪清单](https://github.com/tjbdwanghaibo/roost-core/blob/f25527db2f036b1a790ad4577ad865c1396666be/PRODUCTION_READINESS.md)
- [历史框架评估](https://github.com/tjbdwanghaibo/roost-core/blob/f25527db2f036b1a790ad4577ad865c1396666be/ROOST_FRAMEWORK_ASSESSMENT.md)
- [v1.24.1清理前完整docs](https://github.com/tjbdwanghaibo/roost-core/tree/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs)：旧RR、bugfix、review、feature与agent规范按固定提交追溯，不能用main旧路径。

v1.24.1历史清理基线：原docs跟踪3070个文件，整理时保留/新建50个、删除旧路径3034个；这是当时统计，不是当前文件数。本轮保留全部已有性能样本及失败结果。

各包README、codegen/docs、client/README和脚本运维手册继续就近维护。源码历史RR编号可从Git追溯，不为文档搬迁改动生产逻辑。删除文件留下的空目录只按确认为空后清理，不递归删除本地资料。
