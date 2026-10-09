# Roost v1.25 文档

当前版本v1.25.0，包含目录分类、Gate与Block AOI；重档性能失败为已接受的发布限制。设计篇写职责、使用、状态与限制；实现篇给出契约、逐包源码和回归入口。

第一次接触框架，请先读 [入门手册](../GETTING-STARTED.md)。下面每组先读“设计与使用”；只有需要定位或修改代码时才打开实现篇的源码/测试目录。

## 核心三大模块

| 模块 | 说明 | 实现 |
| --- | --- | --- |
| 核心：Nest 调度与实体 | [设计与使用](guide/02-nest-entity.md) | [实现与源码](impl/02-nest-entity.md) |
| 核心：DataEngine 持久化 | [设计与使用](guide/03-dataengine.md) | [实现与源码](impl/03-dataengine.md) |
| 核心：Sync、Lockstep 与客户端 | [设计与使用](guide/04-sync.md) | [实现与源码](impl/04-sync.md) |

[Interest 的 Block AOI 与手写空间组件](INTEREST-BLOCK-AOI.md)：已接入正式提交门，支持多块被观察、有界事实队列、并行观察者计算与内容发布屏障；无Guard业务回调/生成器改动。功能通过，15分钟常规通过、重档失败。

## 次核心模块

[网关现状与独立 Gate 设计](GATEWAY.md)：嵌入与独立 Gate 的正式接入、已实施 API 与验证边界；新增能力随v1.25.0交付。

[Gate 设计评审与实施方案](GATEWAY-IMPLEMENTATION.md)：具体目录、绑定身份、NATS 转发、统一发送、Nest/Sync 接线及验收条件，Gate/Game、MessagePack、广播与Sync/Lockstep已实施；本机功能通过，性能及多进程/跨机完整验收未收口。

[包目录分类、Wiring 与 Service 收敛方案](PACKAGE-REORGANIZATION.md)：Framework/Infra/Gameplay/Service/Wiring 分类、Session 含义、运行实现迁出接线、便捷接入和同轮 Gate；目录与 Service/Wiring 拆分已实施并通过全仓验证；Gate 分阶段实施中。

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
- [最终保留文档完整清单](../maintenance/DOCUMENTS.md)
- [文档与代码一致性核对](../maintenance/CONSISTENCY.md)
- [已知限制与待验证能力](../maintenance/KNOWN-LIMITS.md)
- [性能基线](../maintenance/PERFORMANCE.md)
- [全部 Go 包映射](PACKAGES.md)
- [游戏 Skill 专项手册](../skill/README.md)
- [客户端接入](../../client/README.md)
- [Codegen 参数与语法手册](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)
