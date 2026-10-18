# Roost 框架模块目录

当前版本v1.27.0（[发布说明](../release/v1.27.0-NOTES.md)）：生成工程 playeraccess 改名、Remote WAL 级 Rejected 精简；此前 Nest 三池与 Await、Sync 优化和旧兼容移除见 [v1.26.0](../release/v1.26.0-NOTES.md)。重档限制与复测见发布说明；之后未发版变化见 [CHANGELOG](../../CHANGELOG.md)。设计篇写职责、使用、状态与限制；实现篇给出契约、逐包源码和回归入口。

第一次接触框架，请先读 [入门手册](../GETTING-STARTED.md)。下面每组先读“设计与使用”；只有需要定位或修改代码时才打开实现篇的源码/测试目录。

## 学习文档：按游戏场景学框架

给准备用 core 写游戏服务端、想先看一条完整业务怎样落到框架上的工程师。先读[入门手册](../GETTING-STARTED.md)，再选一份手册；三份都分“学习”（框架怎么用、为什么这样设计、阅读顺序与可编译示例）与“验收”（每个脚本证明什么、不证明什么）两部分，学习时只读第一部分即可。

- [SLG 框架学习与验收](SLG-INTEGRATION.md)：从模块分工讲起，用建造 → 行军、取消退款、离线收益、两名玩家抢地的生成工程讲 Entity / DAO / Nest / 实体定时器的接法，以及联盟、跨服、活动、排行等路线的源码入口和通用接入原则。第一次学建议从这份开始。
- [MMORPG 框架学习与验收](MMORPG-INTEGRATION.md)：四种状态的归属，组队、托管交易、战斗与 Buff 的生成工程，同进程多地图切换与重连、静态 sid 跨服赠礼的接法；验收部分含真实 WAL / Mongo 两进程恢复与双 sid 起栈步骤。
- [MOBA 框架学习与验收](MOBA-INTEGRATION.md)：局内状态与局外事实的分工，匹配成局的幂等语义，Lockstep 的输入准入、冗余广播、断线追帧、历史窗口与分歧检测，服务器参考模拟判胜负后的多实体结算；5v5 三路夹具加比赛目录、先提交后广播的持久对局、battle host 代际与交接、原服奖励回执。验收部分含真实 loopback KCP 收敛、真实 WAL / Mongo 两进程结算恢复、活动对局 SIGKILL 后从 WAL 续赛与同机五进程 NATS 宿主交接 / 奖励重投。

## 核心三大模块

| 模块 | 说明 | 实现 |
| --- | --- | --- |
| 核心：Nest 调度与实体（含[回复错误判别表](guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)） | [设计与使用](guide/02-nest-entity.md) | [实现与源码](impl/02-nest-entity.md) |
| 核心：DataEngine 持久化 | [设计与使用](guide/03-dataengine.md) | [实现与源码](impl/03-dataengine.md) |
| 核心：Sync、Lockstep 与客户端 | [设计与使用](guide/04-sync.md) | [实现与源码](impl/04-sync.md) |

[Interest 的 Block AOI 与手写空间组件](INTEREST-BLOCK-AOI.md)：已接入正式提交门，支持多块被观察、有界事实队列、并行观察者计算与内容发布屏障；无Guard业务回调/生成器改动。功能通过；不同参数下的15分钟性能结果见[性能报告](../performance/GATE-AOI.md)。

[Nest 三池与 Await](NEST-AWAIT.md)：Kind 选择长短业务池、共享目标顺序、I/O 查询与匿名恢复、配置和迁移；随 v1.26.0 发布。

[Nest 消息热路径优化设计](NEST-OPTIMIZATION.md)：分组预分配、上下文、临时存储和前驱边六项局部优化已实施；目标视图缓存与指标预绑定延期，包含同机证据及验证边界。

## 次核心模块

[玩家协议处理教程](PLAYER-ACCESS.md)：用 demo 穿戴装备说明协议定义、Controller、生成 Sender 和 Nest handler，区分网络会话与 Session 运行服务。

[Controller 生成与 Sync 局部优化](SYNC-OPTIMIZATION.md)：自动补齐业务骨架的边界，以及 Flush、AOI、Relation、Lockstep 的实现和同机基准。

[网关现状与独立 Gate 设计](GATEWAY.md)：嵌入与独立 Gate 的正式接入、已实施 API 与验证边界；新增能力随v1.25.0交付。

[Gate 设计评审与实施方案](GATEWAY-IMPLEMENTATION.md)：具体目录、绑定身份、NATS 转发、统一发送、Nest/Sync 接线及验收条件，Gate/Game、MessagePack、广播与Sync/Lockstep已实施；本机功能通过，性能及多进程/跨机完整验收未收口。

[包目录分类、Wiring 与 Service 收敛方案](PACKAGE-REORGANIZATION.md)：Framework/Infra/Gameplay/Service/Wiring 分类、Session 含义、运行实现迁出接线、便捷接入和同轮 Gate；目录与 Service/Wiring 拆分已实施并通过全仓验证；Gate 已随v1.25.0交付。

| 模块 | 说明 | 实现 |
| --- | --- | --- |
| 次核心：Service 领域能力 | [设计与使用](guide/09-services.md) | [实现与源码](impl/09-services.md) |
| 次核心：Codegen 与工程工具 | [设计与使用](guide/12-codegen.md) | [实现与源码](impl/12-codegen.md) |
| 次核心：Wiring 装配 | [设计与使用](guide/13-wiring.md) | [实现与源码](impl/13-wiring.md) |

## 总览、扩展与其他包

| 模块 | 说明 | 实现 |
| --- | --- | --- |
| 总览与端到端设计 | [设计与使用](guide/00-overview.md) | [实现与源码](impl/00-overview.md) |
| App 与生命周期 | [设计与使用](guide/01-app-lifecycle.md) | [实现与源码](impl/01-app-lifecycle.md) |
| Remote Entity 与 Mirror | [设计与使用](guide/05-remote-mirror.md) | [实现与源码](impl/05-remote-mirror.md) |
| Saga 长事务 | [设计与使用](guide/06-saga.md) | [实现与源码](impl/06-saga.md) |
| 配置、数据表与热更 | [设计与使用](guide/07-config.md) | [实现与源码](impl/07-config.md) |
| 游戏技能、战斗与空间 | [设计与使用](guide/08-skill.md) | [实现与源码](impl/08-skill.md) |
| 时间与定时器 | [设计与使用](guide/10-time.md) | [实现与源码](impl/10-time.md) |
| 观测、安全与运维 | [设计与使用](guide/11-observability.md) | [实现与源码](impl/11-observability.md) |
| 其他包与公共基础设施 | [设计与使用](guide/14-foundation.md) | [实现与源码](impl/14-foundation.md) |

## 维护入口

- [维护、升级、故障处理](../maintenance/README.md)
- [文档清单](../maintenance/DOCUMENTS.md)
- [文档与代码一致性核对](../maintenance/CONSISTENCY.md)
- [已知限制与待验证能力](../maintenance/KNOWN-LIMITS.md)
- [性能报告与复跑](../performance/README.md)
- [全部 Go 包映射](PACKAGES.md)
- [游戏 Skill 专项手册](../skill/README.md)
- [客户端接入](../../client/README.md)
- [Codegen 入口与快速上手](../../codegen/README.md) · [命令与标记参考](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md) · [项目生成器](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md)
- [game-demo 模板](../../demo/README.md) · [功能逐项说明](../../demo/WALKTHROUGH.md)

## 专项契约参考

- [业务执行模型](RUNTIME_EXECUTION_MODEL.md)
- [Nest 事务与 WAL](NEST_TRANSACTION_WAL.md)
- [Pipelined 提交](NEST_PIPELINED_COMMIT.md)
- [Entity Sync](ENTITY_SYNC.md)
- [Remote Entity](REMOTE_ENTITY.md)
- [Saga](SAGA.md)
- [业务 schema 变更操作手册](SCHEMA-MIGRATION.md)：停写 → 备份 → 转换 → 校验 → 切换版本 → 回退，demo 离线迁移示例与真实 Mongo 演练
- [静态 Player 跨机迁移参考流程](PLAYER-MIGRATION.md)：按 sid 搬机的每一步、所用现有能力与检查点，demo 检查工具与本机演练
- [指标与告警清单](OBSERVABILITY.md)
