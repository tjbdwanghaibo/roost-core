# 文档目录与维护规则

当前版本只看 [CHANGELOG](../../CHANGELOG.md)。文档首页是 [docs/README](../README.md)，初次使用从 [入门手册](../GETTING-STARTED.md) 开始。

| 位置 | 权威内容 |
| --- | --- |
| [framework](../framework/README.md) | 当前架构、模块契约；guide 面向使用，impl 面向维护；十种玩法手册包含接入与验收边界；[能力清单](../framework/CAPABILITIES.md)按需求条目索引全部能力 |
| [performance](../performance/README.md) | 每类负载最新有效基线与必要样本；被替代的报告固定到 Git 历史 |
| [maintenance](README.md) | 维护、发布、升级与故障处置 |
| [KNOWN-LIMITS](KNOWN-LIMITS.md) | 分“框架本身的限制”（设计边界、有意不做）与“遗留问题”（未做到位、未验收、尚未确认）两部分；已修复条目只留一行结论，2026-11-13 整理时移出的原文在 [evidence/known-limits-history-20261113](evidence/known-limits-history-20261113/README.md) |
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

业务模块与配方的[方案](FEATURE-2026-11-10-business-modules.md)（2026-11-10）：第 2 期（安装器与两个模块）已实施，现行说明以 [modules/README](../../modules/README.md)、[命令参考“业务模块”](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module) 与 [KNOWN-LIMITS“业务模块”](KNOWN-LIMITS.md#业务模块roost-module) 为准；配方与现有玩法、game-demo 的迁移未实施，那部分仍以 [AGENTS](../../AGENTS.md#游戏接入-skill) 与 [roost-genre skill](../agent-skills/roost-genre/SKILL.md) 为准，不能把方案后几期当作当前能力。

单个玩家换服的[方案](FEATURE-2026-11-11-player-rehome.md)（2026-11-11）：设计定稿、一期实施中；account 的换服接口、DataEngine 的“交出实体”与模块 `rehome`。

业务模块的[待办交接](HANDOFF-2026-11-11-module-backlog.md)（2026-11-11）：换服之外还要做的事——安装器的两个缺口、从 game-demo 继续迁的业务、场景、task 的扩展、验证欠账；给接手的 agent。

业务模块的[后续需求](HANDOFF-2026-11-12-module-requirements.md)（2026-11-12）：登录线之外还要做的模块与联动——运维命令、充值发货、task 扩成核心系统、组队 / 匹配 / 送礼 / 活动 / 公会、现有模块的后续与验证欠账；每项的需求、依赖与验收口径，给接手的 agent。

两个配方接入登录链路的[交接](HANDOFF-2026-11-12-recipes-login.md)（2026-11-12）：配方 casual、turn-pvp 换用模块 login / portal / servertime 的任务书，含一份未验证的半成品从哪里接、不要碰哪些文件、只跑哪些检查。

给现有业务模块补业务层日志的[交接](HANDOFF-2026-11-12-module-logging.md)（2026-11-12）：规则（玩家相关的操作要记、高频不记、只用框架的日志）、在哪里打、逐模块要补什么、验证与交付。

修业务模块里已知问题的[交接](HANDOFF-2026-11-12-module-fixes.md)（2026-11-12）：四组问题清单（场景与世界、联盟与活动、社交与战斗、零散项）、修到什么程度、一份未验证的半成品从哪里接、验证与交付。

该交接的[逐项修复与验收](evidence/module-fixes-20261112/README.md)：草稿校验、Terrain 恢复代际、邀请与联盟入口、死亡后路径、客户端访问与审计，含真实 TCP / 存储链路与未修的 Scene 启动接线限制。

App 生命周期“停机先关外部输入”的[方案](FEATURE-2026-11-04-app-lifecycle.md)（2026-11-04）：现行契约以 [App 生命周期设计篇](../framework/guide/01-app-lifecycle.md) 为准，方案保留盘点、决定与实施记录。
