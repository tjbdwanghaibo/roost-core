# Roost 文档中心

Roost 是面向 Linux 生产环境的通用 Go 游戏服务器框架。运行时由 `cube-core`、`cube-kit`、`roost-skill` 组成，项目与样板代码由 `roost-codegen` 生成。文档按阅读者的目标分为三级，没必要从头读到尾。

## 第一级：完全新手

阅读 [五分钟快速开始](QUICKSTART.md)。目标是生成一个项目、启动依赖、运行一个 Service，并知道业务代码应该写在哪里。此级不要求理解 WAL、fence 或 Saga。

## 第二级：有经验的游戏后端开发者

阅读 [开发者完整使用说明](USER_GUIDE.md)。它说明如何选择 Mod、组织 Service、编写 Entity/DAO/Nest handler、主动 Flush、使用 Remote Entity/Saga、选择状态同步或帧同步，并给出测试和运维边界。

## 第三级：框架维护者与生产负责人

- [实现原理与不变量](INTERNALS.md)：生命周期、锁、事务、WAL/Data Engine、远程实体、Saga、实时同步及失败语义。
- [生产部署手册](DEPLOYMENT.md)：Shell/systemd、Docker、Kubernetes、发布、回滚、备份、容量与故障演练。
- [薄弱点与路线图](ROADMAP.md)：哪些是发布阻断项，哪些是增强项，哪些不应进入框架核心。
- [多仓研发与发布](DEVELOPMENT_WORKSPACE.md)：go.work source-head 联调、`GOWORK=off` 发布门禁与版本顺序。

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
- [roost-skill 技能系统](https://github.com/tjbdwanghaibo/roost-skill/blob/main/README.md)
- [roost-codegen 生成器](https://github.com/tjbdwanghaibo/roost-codegen/blob/main/README.md)

## 版本基线

当前已发布运行时组合为：`cube-core v1.8.0`、`cube-kit v1.8.0`、`roost-skill v1.7.0`。本轮新增生产部署生成能力属于 `roost-codegen v1.7.0` 源码基线；发布该版本后再在生产项目中固定使用。正式项目不得依赖 `@latest`、伪版本或本地 `replace`。
