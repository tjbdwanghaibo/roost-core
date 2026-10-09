# 文档与代码一致性核对

这张表回答“文档中的说法是否有代码依据”。初次学习请先看[入门手册](../GETTING-STARTED.md)；审查某条结论时，再沿表中的链接查看实现。

## 1. 基准与方法

运行时tag：v1.24.0 = 2fa1c7877b14c77b52e062bedcb3455cef8db0fb。清理前HEAD：9d955fb0df35f082dfc9be24c2f3a4524d437067。清理开始前的git diff v1.24.0 9d955fb0仅包含6份文档的发布回填，没有运行时差异。

结构发现使用codebase-memory项目roost-core；本机generation=2026-10-08T00:23:20Z，落后于发布。对2325个Go路径执行coverage：2297为metadata_changed，28为not_tracked。没有记录缺口不代表完整。图谱旧行号已发生漂移，因此文档不引用其旧行号作当前证据；源码/类型/测试索引全部直接读取当前跟踪文件建立。

文档合入 main 后，已请求对同一路径刷新索引。该次 MCP 调用等待 300 秒后超时，尚未确认新索引完成；不能用发出刷新请求代替完成证据。本文核对结论依赖上面说明的当前源码补证。

核对分三层：版本和目录清点；下表明确契约逐项对照；逐包源码与测试声明定位。第二层是有界关键契约核对，第三层是定位覆盖，均不代表逐函数语义review完成。文档删改不改变程序行为。

## 2. 已纠正和核实的具体条目

| 编号 | 主题 | 旧口径或需澄清点 | 现行结论 | 证据入口 |
| --- | --- | --- | --- | --- |
| C01 | 发布状态 | 框架文档仍写v1.23.1未发布 | v1.24.0 tag固定2fa1c787；main仅回填验收 | [v1.24.0发布记录](../release/v1.24.0-NOTES.md) |
| C02 | Nest提交 | pipelined只能手动注册/提交点混称 | 生成标记支持pipelined；准入与持久确认分开 | [codegen/internal/nest/parse.go](../../codegen/internal/nest/parse.go) |
| C03 | memory持久写 | 旧段落称静默忽略且不报错 | refuseMemoryPersistentWrite拒绝本地持久写 | [nest/rollback.go](../../framework/nest/rollback.go) |
| C04 | WAL | 旧格式/旧独立运行路径混用 | codecVersion=7，解码拒绝其他版本 | [nestwal/codec.go](../../framework/nestwal/codec.go) |
| C05 | DAO schema | 保留自动迁移流程 | 聚合所有DAO先校验当前schema再水合；自动迁移撤销 | [dataengine/engine/entity_repository.go](../../framework/dataengine/engine/entity_repository.go) |
| C06 | Sync模式 | setter立即发或没有变化驱动 | setter标脏，handler锁内冻结、确认后唤醒；保留periodic | [sync/entitysync/mode.go](../../framework/sync/entitysync/mode.go) |
| C07 | Sync水位 | SetDurableWatermark旧API | ManagerConfig.DurableWatermark，整subject门控 | [sync/entitysync/manager.go](../../framework/sync/entitysync/manager.go) |
| C08 | 客户端载荷 | Lockstep只预留/旧协议兼容 | RS v2已有PB/Sync/Lockstep类型和保留位验证 | [client/wire/packet.go](../../client/wire/packet.go) |
| C09 | Lockstep权限 | 客户端命令与身份来源需明确 | Command不携带seat，由当前session映射 | [sync/lockstep/command.go](../../framework/sync/lockstep/command.go) |
| C10 | 总线投递 | 无限重投/等待RR-25确认 | 默认MaxDeliver=5，生命周期退役/创建有互斥 | [sync/syncbus/driver/jetstream.go](../../framework/sync/syncbus/driver/jetstream.go) |
| C11 | Remote发布 | 唯一outbox发布者仍是方案 | 已随v1.24.0完成；正式路径按全部实体依赖调度 | [outbox_publish.go](../../framework/remoteentity/outbox_publish.go) |
| C12 | Service归属 | 需核实kit是否已全部只装配 | mail/match/session在service；其余七个领域仍在wiring/service | [wiring/account/service.go](../../service/account/service.go) |
| C13 | 通用迁移 | 自动DAO迁移与通用migration包需区分 | 通用Registry仍提供显式步骤；不等于Repository自动迁移 | [migration/migration.go](../../infra/storage/migration/migration.go) |
| C14 | Codegen最低版 | 最低core仍为v1.23.1 | minimumVersions.Core=v1.24.0；补丁不新增API | [codegen/internal/roost/manifest.go](../../codegen/internal/roost/manifest.go) |
| C15 | 发布门禁 | pretag没有生成漂移检查 | 脚本包含go generate及干净树检查 | [scripts/pretag.sh](../../scripts/pretag.sh) |
| C16 | 性能状态 | Remote/Sync仍暂停、旧性能失败作为当前结论 | v1.24.0已补一小时与声明负载；失败样本仍保留 | [docs/maintenance/PERFORMANCE.md](PERFORMANCE.md) |
| C17 | 未验证边界 | 需保留本机与跨机/引擎的验收边界 | 环境、平台、时长、模拟范围分别列出 | [docs/maintenance/KNOWN-LIMITS.md](KNOWN-LIMITS.md) |
| C18 | Skill模块名 | 使用手册把skill包路径写成Go module | Go module为roost-core；skill是其中的包 | [go.mod](../../go.mod)、[修正后的手册](../skill/skill.md) |
| C19 | TCP 手册补充核对 | 仓库手册写 RS v1，生成手册把 Lockstep 写成预留；消息号 0 仅用于鉴权 | 当前为 RS v2；PB/Sync/Lockstep 分类型；消息号 0 还用于鉴权后心跳。本次在 main 修正文档，不移动旧标签 | [公共包头](../../client/wire/packet.go)、[TCP 模板](../../codegen/internal/roost/render_player_tcp.go)、[说明与纠正清单](../framework/GATEWAY.md) |
| C20 | Gate 能力边界 | gateway 包与独立网关服务容易混称 | 已有嵌入式 TCP 接入；独立 Gate 路由、回推和集群会话方案仍待实施 | [现状与待实施设计](../framework/GATEWAY.md)、[服务目录](../../codegen/internal/roost/framework_services.go) |

## 3. 各模块结论

| 分区 | 核对深度 | 剩余限制 |
| --- | --- | --- |
| [总览与端到端设计](../framework/guide/00-overview.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [App 与生命周期](../framework/guide/01-app-lifecycle.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [核心：Nest 调度与实体](../framework/guide/02-nest-entity.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [核心：DataEngine 持久化](../framework/guide/03-dataengine.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [核心：Sync、Lockstep 与客户端](../framework/guide/04-sync.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [Remote Entity 与 Mirror](../framework/guide/05-remote-mirror.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [Saga 长事务](../framework/guide/06-saga.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [配置、数据表与热更](../framework/guide/07-config.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [游戏技能、战斗与空间](../framework/guide/08-skill.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [次核心：Service 领域能力](../framework/guide/09-services.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [时间与定时器](../framework/guide/10-time.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [观测、安全与运维](../framework/guide/11-observability.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [次核心：Codegen 与工程工具](../framework/guide/12-codegen.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [次核心：Wiring 装配](../framework/guide/13-wiring.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |
| [其他包与公共基础设施](../framework/guide/14-foundation.md) | 职责、正式入口、现行契约及源码/测试定位已整理 | 未对所有函数、故障组合和外部部署重新逐项验收 |

## 4. 结论的边界

可以确认：本轮列出的旧版本状态、关键API/格式、服务目录归属、能力已实现/未验证状态已统一到v1.24.0；当前源码及测试入口可直接跳转。不能确认：整个框架所有行为与全部说明已经由新一轮运行测试穷尽证明。专项Skill/Codegen/客户端手册保留详细使用信息，新增业务接入仍须编译与场景验收。

历史证据保留固定提交链接，正文不再靠“后面补充推翻前面”阅读。性能数据沿用既有发布记录，未重新运行的真实资源与长期验证如实列在KNOWN-LIMITS。v1.24.1 实际执行的命令及结果见[本版验收记录](../release/v1.24.1-IMPLEMENTATION.md)。

## 5. 文档清理后的测试接线

删除AI提示词后，原skill/prompt_test.go不能再要求提示词存在。现在由[documentation_test.go](../../gameplay/skill/documentation_test.go)校验保留的技能手册：至少包含一个完整JSON示例，且实际Parse/Compile通过。这是文档回归测试的迁移，没有修改Skill运行时。
