# 文档目录与维护规则

当前版本只看 [CHANGELOG](../../CHANGELOG.md)。文档首页是 [docs/README](../README.md)，初次使用从 [入门手册](../GETTING-STARTED.md) 开始。

| 位置 | 权威内容 |
| --- | --- |
| [framework](../framework/README.md) | 当前架构、模块契约；guide 面向使用，impl 面向维护；十种玩法手册包含接入与验收边界 |
| [performance](../performance/README.md) | 每类负载最新有效基线与必要样本；被替代的报告固定到 Git 历史 |
| [maintenance](README.md) | 维护、发布、升级与故障处置 |
| [KNOWN-LIMITS](KNOWN-LIMITS.md) | 当前能力限制、未验证范围与尚未确认问题 |
| [DESIGN-TODO](DESIGN-TODO.md) | 尚待选择或排期的优化候选 |
| [EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) | 外部依赖与跨环境验证入口 |
| [CORE-REVIEW](CORE-REVIEW.md)、[Linux 验收续报](LINUX-FUNCTIONAL-20261102.md)、[v1.38～v1.40 功能复核](FEATURE-REVIEW-20261102.md)、[evidence](evidence/) | 2026-11 起的核心审查与 Linux / 功能复核报告及其精简证据；仍有效的限制以 KNOWN-LIMITS 为准 |
| [release](../release/README.md) | 固定版本的升级说明与验收；history 是合仓前只读变更记录 |
| [skill](../skill/README.md) | 游戏内技能系统文档，不是 agent skill |
| [AGENTS](../../AGENTS.md#游戏接入-skill) | Agent 维护入口与[生成游戏 skill](../agent-skills/roost-genre/SKILL.md) |
| 代码附近 | 包 README、codegen 手册、demo 和脚本说明 |

## 收敛规则

现行手册直接表达最终行为，不保留“旧设计 + 文末更正”的双重口径。已完成的分期、评审、提案与测试审计归 Git 历史；独有的现行约束先写入对应 guide / impl，未解决事项先登记 KNOWN-LIMITS 或 DESIGN-TODO，再删除过程稿。性能样本、红绿证据与历史发布记录不冒充当前验收。

历史文档引用使用完整提交固定的链接，当前规范使用仓内相对链接。删除时检查所有跟踪 Markdown 引用，不增加门禁豁免。未来维护不再新建按日期累积的已完成交接流水账。

## 已完成过程稿的去向

| 主题 | 当前入口 |
| --- | --- |
| Nest 心跳批量投递 | [Nest](../framework/impl/02-nest-entity.md)、[心跳](../framework/impl/10-time.md) |
| Timer 调度器与持久期限 | [时间与定时器](../framework/guide/10-time.md)、[实现](../framework/impl/10-time.md) |
| Interest 单写应用、快照恢复 | [Sync](../framework/guide/04-sync.md)、[实现](../framework/impl/04-sync.md) |
| Checkpoint 合并、按实体并行投影 | [DataEngine](../framework/guide/03-dataengine.md)、[实现](../framework/impl/03-dataengine.md) |
| Saga JetStream 与已放弃的 Redis 权威路线 | [Saga](../framework/SAGA.md)、[实现](../framework/impl/06-saga.md) |
| 别名合服 | [Player 迁移](../framework/PLAYER-MIGRATION.md) |
| 匹配弹性成局、业务层补位 | [Service](../framework/guide/09-services.md)、[派对房间](../framework/PARTY-ROOM-INTEGRATION.md) |
| 组同步与自身视图 | [Entity Sync](../framework/ENTITY_SYNC.md) |
| 接入与运行服务职责 | [Player 接入](../framework/PLAYER-ACCESS.md)、[Service](../framework/guide/09-services.md) |
| 已关闭 review 与测试审计 | [CHANGELOG](../../CHANGELOG.md)、对应 impl 的回归入口；剩余边界见 KNOWN-LIMITS |

删除的原文、早期文档清理索引和精确红绿记录均可从[整理前完整快照](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/)追溯；原有引用已固定到该快照。无观察者暂停 AI 尚未实施，其[设计稿](FEATURE-2026-10-22-ai-idle-without-observers.md)保留，不能当作当前能力。多进程无缝大世界同样只有[思路稿](FEATURE-2026-11-03-seamless-world.md)，没有实现。联盟 / 公会服务的[方案](FEATURE-2026-11-04-alliance-service.md)已定、未实施，现行公会行为仍以 KNOWN-LIMITS 为准。
