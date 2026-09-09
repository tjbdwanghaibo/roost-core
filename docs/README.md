# Roost 文档中心

Roost 是面向 Linux 生产环境的通用 Go 游戏服务器框架。运行时由 `roost-core`（契约 + 实现 + 技能系统）与 `roost-kit`（装配层 + 通用服务）组成，项目与样板代码由 `roost-codegen` 生成。文档按阅读者的目标分为三级，没必要从头读到尾。

## 第一级：完全新手

阅读 [五分钟快速开始](QUICKSTART.md)。目标是生成一个项目、启动依赖、运行一个 Service，并知道业务代码应该写在哪里。此级不要求理解 WAL、fence 或 Saga。跑不起来或看到不认识的错误时，先查[问题快速定位](TROUBLESHOOTING.md)：按症状索引，每行给出原因、看哪里、怎么处理。

## 第二级：有经验的游戏后端开发者

阅读 [开发者完整使用说明](USER_GUIDE.md)。它说明如何选择 Mod、组织 Service、编写 Entity/DAO/Nest handler、主动 Flush、使用 Remote Entity/Saga、选择状态同步或帧同步，并给出测试和运维边界。

## 第三级：框架维护者与生产负责人

- [持续代码 Review 与学习记录](review/README.md)：三仓最新基线、审查范围、验证和下一轮入口。
- [2026-09-09 三仓架构评估](review/REVIEW-2026-09-09.md)：实现边界、可靠性证据、接入验证与下一阶段建议。
- [Review 问题索引](bug/README.md)：确认问题、复现证据及状态；默认只报告、不修改代码。
- [Core/Kit 实现下沉与缺陷收敛统一实施方案](CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)：迁移批次、历史审计衔接、测试门禁及跨仓验收。
- [Roost v2 收敛方案：五仓合三仓、实现下沉 Core](ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md)：目标形态、包映射表、P0～P6 分阶段门禁、在途改动处置。

- [实现原理与不变量](INTERNALS.md)：生命周期、锁、事务、WAL/Data Engine、远程实体、Saga、实时同步及失败语义。
- [生产部署手册](DEPLOYMENT.md)：Shell/systemd、Docker、Kubernetes、发布、回滚、备份、容量与故障演练。
- [薄弱点与路线图](ROADMAP.md)：哪些是发布阻断项，哪些是增强项，哪些不应进入框架核心。
- [多仓研发与发布](DEVELOPMENT_WORKSPACE.md)：go.work source-head 联调、`GOWORK=off` 发布门禁与版本顺序。
- [收敛覆盖账本](history/ledger.md)：bug 收敛的工作单元协议、包 × 缺陷类覆盖矩阵、待开单元与单元日志。

## 专题文档

- [Nest 事务 WAL](../NEST_TRANSACTION_WAL.md)
- [Pipelined commit](../NEST_PIPELINED_COMMIT.md)
- [Entity Sync](../ENTITY_SYNC.md)
- [Remote Entity](../REMOTE_ENTITY.md)
- [Saga](../SAGA.md)
- [可观测性](../OBSERVABILITY.md)
- [运行模型](../RUNTIME_EXECUTION_MODEL.md)
- [生产就绪清单](../PRODUCTION_READINESS.md)
- [框架综合评估](../ROOST_FRAMEWORK_ASSESSMENT.md)
- [roost-kit 组件与实现](https://github.com/tjbdwanghaibo/roost-kit/blob/main/README.md)
- [技能系统（core/skill）](skill/README.md)
- [通用服务（kit/service）](https://github.com/tjbdwanghaibo/roost-kit/blob/main/service/README.md)
- [roost-codegen 生成器](https://github.com/tjbdwanghaibo/roost-codegen/blob/main/README.md)

## 版本基线

当前正式 tag 组合为：`roost-core v1.15.1`、`roost-kit v1.14.2`、`roost-codegen v1.15.3`（2026-09-09；roost-skill / roost-service 已于 2026-09-08 并入 core / kit 并归档）。发布顺序固定为 core → kit → codegen，后一层只能依赖前一层已存在的正式 tag；roost-codegen 的 `ci/framework-release.yaml` 是这组版本的机器可校验记录。正式项目不得依赖 `@latest`、伪版本或本地 `replace`。
