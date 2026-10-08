# Roost 文档中心

**2026-10-08 Lockstep客户端接线（main未发版）：**RS v2启用类型2（上行4、push5），正式ProtocolRegistry/生成TCP已接线，复用Room/C7广播与历史追帧；C#提供Command、有界Assembler与主线程模拟消费。没有旧包兼容，不代表Unity/Godot/Unreal实机、UDP性能或游戏确定性已验收。[接入说明](../client/README.md) · [实施/验证](feature/IMPLEMENTATION-2026-10-08-CLIENT-LOCKSTEP.md)。[验收/全仓基线失败交接](review/REVIEW-2026-10-08-client-lockstep-validation.md)。下方10-07“预留”保留历史时点。

**10-07 客户端协议第一批：**[共享RS v2协议、C#与Unity接入](../client/README.md)，PB/raw Sync已接线，Lockstep编号预留但当前拒绝；无旧包兼容。[实施方案与验证边界](feature/REFACTOR-2026-10-07-CLIENT-PROTOCOL.md)。这是客户端接入新增功能，历史非核心review进度保持原口径。

> 框架整体的说明与实现见 [框架文档](framework/README.md)（基准 v1.23.0）；内容冲突以框架文档与源码为准。

**10-05第27轮及完整接力：** [本轮修复/review](review/REVIEW-2026-10-05-noncore-27.md)、[后续全部review清单](review/REMAINING-REVIEW-HANDOFF-2026-10-05.md)、[进度](review/PROGRESS.md)。NC-41/42及RR-09残余已修、未发版；另一agent下一从N06 global RPC/Mod/App.Live增量接续，外部专项与核心线接口单列。

**10-04 N04修复与第三批：** [NC-16～20已修/正式消费者](review/REVIEW-2026-10-04-noncore-13.md)、[源码40/40与新审查](review/REVIEW-2026-10-04-noncore-14.md)、[五新未修RR](bug/REVIEW-2026-10-04-noncore-14.md)、[机制学习](review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)、[进度](review/PROGRESS.md)。未发版，源码阅读数不代表业务覆盖率。

**10-04 缓存修复与N04第二批：** [NC-13～15已修](review/REVIEW-2026-10-04-noncore-11.md)，41正式项/真实Redis/生成DAO消费者通过；[新审查](review/REVIEW-2026-10-04-noncore-12.md)累计39/40源文、场景部分完成，确认[NC-16～20五个未修问题](bug/REVIEW-2026-10-04-noncore-12.md)。[学习/实施方向](review/IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md) · [进度](review/PROGRESS.md)。未发版，下方为历史轮次。

**10-04 etcd修复与N04接续：** [NC-11/12修复](review/REVIEW-2026-10-04-noncore-09.md)两项已修、15正式生命周期场景通过；[N04缓存/Redis/Mongo/迁移](review/REVIEW-2026-10-04-noncore-10.md)20/40源文已读、场景部分完成，确认[三个新未修问题](bug/REVIEW-2026-10-04-noncore-10.md)，专属真实Redis7补测通过。[机制与实施交接](review/IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md) · [进度](review/PROGRESS.md) · [证据](review/evidence/noncore-review-20261004-10/README.md)。未发版，下方未修结论为历史时点。

**10-04 RPC修复与etcd接续：**[NC-08～10修复](review/REVIEW-2026-10-04-noncore-07.md)3/3已修、声明场景验证，28正式项/17原overlay/六包race/vet通过；[etcd/KitEtcd审查](review/REVIEW-2026-10-04-noncore-08.md)将N03清单源文推进到39/39、场景仍部分完成，确认[两个新未修P2](bug/REVIEW-2026-10-04-noncore-08.md)。[机制学习](review/IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md) · [进度](review/PROGRESS.md) · [新反例/复跑](review/evidence/noncore-review-20261004-08/README.md)。下一新范围N04，未发版。

**10-04 N03 通信 Review：**[RPC协议、预算与退出责任](review/REVIEW-2026-10-04-noncore-06.md)，N03清单源文25/39已读、场景部分完成；[NC-08～10三个新未修P2](bug/REVIEW-2026-10-04-noncore-06.md)与下方已修问题分开。17项4失败/13控制，六包race/vet通过，下一步剩余etcd主链与KitEtcd。[学习与实施方向](review/IMPLEMENTATION-MESSAGING-RPC-BUDGET-AND-TERMINAL-OWNERSHIP.md) · [复跑](review/evidence/noncore-review-20261004-06/README.md) · [进度](review/PROGRESS.md) · [计划](review/NONCORE-REVIEW-PLAN-2026-10-03.md)。

**10-04 请求链修复：**[NC-05～07 限流/Recover/路由模式](review/REVIEW-2026-10-04-noncore-05.md) 3/3已修、声明场景验证；30正式项、原审查及13消费者通过，最终20测试包race/vet完成，shell环境/原skip单列。[记录](bugfix/README.md) · [复跑](bugfix/evidence/noncore-bugfix-20261004-03/README.md) · [进度](review/PROGRESS.md)。未发版，下方未修结论保留历史。

**10-04 N02 接续 Review：**[请求边界与正式 Webroute 消费](review/REVIEW-2026-10-04-noncore-04.md)新增[三个未修 P2](bug/REVIEW-2026-10-04-noncore-04.md)：拒绝请求占限流 key、Recover 上报 panic、非法路径注册 panic。正常生成/真实 HTTP/退役控制通过，六包 race/vet 通过。[学习和建议](review/IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md) · [复跑](review/evidence/noncore-review-20261004-04/README.md) · [进度](review/PROGRESS.md) · [剩余计划](review/NONCORE-REVIEW-PLAN-2026-10-03.md)。

**10-04 第二批修复：**[Manager/Admin/Ops 四项修复与兼容](review/REVIEW-2026-10-04-noncore-03.md)，正式35项与11包race/vet通过，已修/声明场景验证，未发版。[逐项记录](bugfix/README.md) · [证据](bugfix/evidence/noncore-bugfix-20261004-02/README.md) · [进度](review/PROGRESS.md)。下方原第二批未修结论保留历史。

**10-04 继续 Review：**[N01 生命周期/Manager/Admin/Ops](review/REVIEW-2026-10-04-noncore-02.md) 新增[四个未修问题](bug/REVIEW-2026-10-04-noncore-02.md)，与下方已修四项分开。N01 生产源码 15/15 已读，新增 17 项含 11 个失败反例，场景仍部分完成。[机制和实施建议](review/IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md) · [复跑](review/evidence/noncore-review-20261004-02/README.md) · [进度](review/PROGRESS.md)。

**10-04 App/HTTP 修复：**[四项运行](review/REVIEW-2026-10-04-noncore-01.md)、[逐项记录](bugfix/README.md)、[红/绿与复跑](bugfix/evidence/noncore-bugfix-20261004-01/README.md)。原 NC-01～04 已修、具名场景验证，45 个新增正式场景和 11 包 race/vet 通过，未发版；下方“未修”是历史状态。[进度](review/PROGRESS.md)。

**10-03 非三大核心 Review：**[App/HTTP 四项未修 P2](bug/REVIEW-2026-10-03-noncore-01.md)、[运行与实证](review/REVIEW-2026-10-03-noncore-01.md)、[实现学习](review/IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md)、[15 单元后续计划与完成窗口](review/NONCORE-REVIEW-PLAN-2026-10-03.md)。Service 复用已有完成记录，Codegen 接续缺口；本批 12 失败反例/18 通过控制，七包 race/vet 通过。源码阶段与 bug 修复、外部验证分别管理。

**09-30 Codegen 第六轮：**[RR-CG-12～14 修复与正式消费者](review/REVIEW-2026-09-30-codegen-06.md) 3/3 已修、具名场景验证，未发版。[逐项修复](bugfix/README.md) · [进度](review/PROGRESS.md)。未把 Codegen 整体标为审查完成。

**09-30 Codegen 第五轮：**[cfggen 消费者与依赖自动迁移](review/REVIEW-2026-09-30-codegen-05.md)新增[三个未修 P2](bug/REVIEW-2026-09-30-codegen-05.md)；四个反例/四个控制归档，包回归与定向 race 通过。[机制学习](review/IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md) · [进度](review/PROGRESS.md)。只审查未改源码，整体尚未收敛。

**09-30 Codegen 第四轮：**[RR-06～10 五项修复与旧验证缺口复查](review/REVIEW-2026-09-30-codegen-04.md) · [逐项 bugfix](bugfix/README.md) · [进度](review/PROGRESS.md)。隔离业务工程生成、退役检查与消费者编译通过；旧 v1 表格归属、HTTP 启动消费和版本兼容仍需后续验证。未发版。

**09-30 Codegen 第三轮：**[RR-04 Entity 修复](bugfix/RR-20260930-04.md) · [RR-05 Nest 修复](bugfix/RR-20260930-05.md) · [五个新问题未修](bug/REVIEW-2026-09-30-codegen-03.md) · [审查矩阵与进度](review/REVIEW-2026-09-30-codegen-03.md)。主生成器入口已盘点，整体 Codegen 仍有具名缺口；未发版。

**09-30 Codegen 第二轮：**[RR-01 RPC 修复](bugfix/RR-20260930-01.md) · [RR-02 Protocol 修复](bugfix/RR-20260930-02.md) · [新 RR-04/05 待实施](bug/REVIEW-2026-09-30-codegen-02.md) · [运行/进度](review/REVIEW-2026-09-30-codegen-02.md)。原两项行为回归和隔离 CLI 已验证，继续 review 发现 Entity/Nest 输入删除后旧生成文件留存；未发版。

**09-30 Codegen 第一轮：**[运行/进度](review/REVIEW-2026-09-30-codegen-01.md) · [两个未修 P2 与实施建议](bug/REVIEW-2026-09-30-codegen-01.md) · [生成暂存与退役机制](review/IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md)。当前生成器位于 Core `codegen/`；删除 RPC 标记/最后协议定义的隔离反例已复现，生产源码未修改。上一轮 Service 实测及外部环境待办见下方归档。

**09-30 Service 第十三轮：**[真实 Toxiproxy 故障、多 owner HA、积压恢复与热点容量审查](review/REVIEW-2026-09-30-services-13.md)，[机制学习](review/IMPLEMENTATION-SERVICE-FAULT-AND-CONTENTION.md)、[复跑证据](review/evidence/service-review-20260930-02/README.md)。原 3 项网络测试已实际通过；外部资源、跨机器 HA 和生产长稳尚未验收。

**09-30 Service 专项：**[强杀恢复、Redis HA 与容量审查](review/REVIEW-2026-09-30-services-12.md)，[机制学习](review/IMPLEMENTATION-SERVICE-CRASH-HA-AND-CAPACITY.md)、[复跑证据](review/evidence/service-review-20260930-01/README.md)。本机列明场景已实测；Toxiproxy、真实外部资源与生产长稳仍未验收。

**09-29 Service本阶段review完成**：[第十一轮范围/逐批台账/后续设计](review/REVIEW-2026-09-29-services-11.md)，10/10域主链、100路径内容核算；[RR-34修复](bugfix/RR-20260929-34.md)原3/3转绿，正式两后端/整体回归通过，本轮无新确认缺陷。[完成矩阵](review/SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [机制学习](review/IMPLEMENTATION-SERVICE-PIPELINE-AND-REVIEW-CLOSURE.md)。3Toxiproxy skip/外部资源/HA/容量设计另列，未发布；下方保留历史。

**09-29 Service第八批/第十轮**：[RR-33 Mail跨槽分页已修/已验](bugfix/RR-20260929-33.md)，正式Mod跨3owner分页通过；新[RR-34 P2 Pipeline首缺失掩盖写错误](bug/REVIEW-2026-09-29-services-10.md)未实施。[运行](review/REVIEW-2026-09-29-services-10.md) · [机制学习](review/IMPLEMENTATION-SERVICE-MAIL-BATCH-AND-PIPELINE-ERRORS.md)。10域主链有界整理保持完成，外部专项另算；下方保留历史。


**09-29 Service第七批与第九轮收口**：[RR-31/32两项已修/已验](bugfix/SERVICE-BUGFIX-2026-09-29-07.md)，[10域本阶段主链/专项完成口径](review/REVIEW-2026-09-29-services-09.md)，[新RR-33 Mail Cluster页与现有Pipeline方案](bug/REVIEW-2026-09-29-services-09.md)未实施。[历史状态/履约学习](review/IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)。未发版，外部资源/HA/长稳另列。

**09-29 最新：先 bugfix，再 service review。**[RR-28/29/30 三项修复与升级说明](bugfix/SERVICE-BUGFIX-2026-09-29-06.md)，原15/15与正式43叶子通过，完整17包/826事件通过；继续发现[Activity Cluster/购买首次grant两个新P2](bug/REVIEW-2026-09-29-services-08.md)，未实施。[运行](review/REVIEW-2026-09-29-services-08.md) · [最新进度](review/PROGRESS.md) · [学习](review/IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md#第六批修复后的实现与第八轮学习)。下方旧状态保留其历史时点。

**09-29 Service 第七轮新增问题**：[待办清理竞争、Rank/Platform Cluster 准入三个确认缺陷与实施交接](bug/REVIEW-2026-09-29-services-07.md)，均未修复；[运行与进度](review/REVIEW-2026-09-29-services-07.md)、[机制学习](review/IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md)、[最新矩阵](review/SERVICE-REVIEW-COMPLETION-2026-09-29.md)。真实 Redis/Cluster 6 反例失败、9 正常控制通过；当前 service 未完全收敛，上轮4/4原触发修复仍有效。

**09-29 Service 修复后收敛结论**：[第六轮审查](review/REVIEW-2026-09-29-services-06.md) · [最新完成矩阵](review/SERVICE-REVIEW-COMPLETION-2026-09-29.md)。上轮 4/4 缺陷修复/声明场景通过，邻接范围无新增确认缺陷；10 域主链完成，真实外部系统/HA/强杀/性能及旧数据专项未完全验证。

**09-29 Service 第五批 bugfix**：[RR-25/26/27 与旧活动残余 4/4 修复](bugfix/SERVICE-BUGFIX-2026-09-29-05.md)。完整 16 包/830 事件回归与最终 Memory/Redis 定向通过；Activity 持久计划/保留容量、旧超容量扫描轮转、Chat 页内/尾部 Gap 已实现。owner 升级与旧数据边界另列，未发版。

**09-29 Service 修复后审查收口**：[10 域完成矩阵](review/SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [第五轮运行](review/REVIEW-2026-09-29-services-05.md) · [3 新问题及旧活动残余](bug/REVIEW-2026-09-29-services-05.md) · [容量/年龄/资源恢复学习](review/IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md)。第四批 3 项已修复并推送；新四项交接未实施。主链范围收口，外部系统/HA/强杀/性能专项仍未验证。

**09-29 Service 第四批修复完成**：[3 项实现与升级说明](bugfix/SERVICE-BUGFIX-2026-09-29-04.md)。未知发货保留证明、回包切片隔离、未领取删除身份保留；801 事件 service 回归通过，未发版。


**09-29 Service 第四轮 review**：[运行与进度](review/REVIEW-2026-09-29-services-04.md) · [2 新问题及旧删除残余](bug/REVIEW-2026-09-29-services-04.md) · [外部结果/删除/恢复学习](review/IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md) · [复跑](review/evidence/service-review-20260929-04/README.md)。已补 Memory/Redis 证据，当前 794 事件基础回归全过；新增安全反例仍失败，生产源码未改。

**09-29 最新实施**：[Service 第三轮 bugfix 与升级交接](bugfix/SERVICE-BUGFIX-2026-09-29-03.md)：RR-19～22 已修复，正式 Memory/Redis 回归通过；Mail 取消 API 新增 attempts、Directory/Slot 后端需保留 DeleteIf、旧状态需对账，未发版。下方历史 review 的“未实施”保留当时结论。

**09-29 Service 第三轮 review**：[运行与进度](review/REVIEW-2026-09-29-services-03.md) · [4 个新增问题及实施交接](bug/REVIEW-2026-09-29-services-03.md) · [提交未知/预约身份学习](review/IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md) · [复跑材料](review/evidence/service-review-20260929-03/README.md)。旧修复的已有回归通过，新增邻近问题尚未实施；本轮只改文档。

**09-29 最新修复**：[Service 两轮 bugfix 与实施接手](bugfix/SERVICE-BUGFIX-2026-09-29.md)，18 项新问题及旧正常 Finish ABA 已实施并通过定向回归；[roost-bugfix skill](agent-skills/roost-bugfix/SKILL.md) 已新增。尚未发版，升级/旧数据恢复边界见总记录。下方历史 review 的“未修复”不代表当前源码。

[09-29 Service 第二轮](review/REVIEW-2026-09-29-services-02.md)：新增 8 个可复现问题，补旧 Session ABA 的正常 Finish/真实 Redis 证据。[问题及修复方案](bug/REVIEW-2026-09-29-services-02.md) · [复跑](bug/REPRO-2026-09-29-services-02.md) · [进度](review/PROGRESS.md)。没有修改生产代码。

[09-29 Core 全 service 审查与实施交接](review/REVIEW-2026-09-29-services.md)：10 个服务域主链、9 项可复现问题和 1 项自动清理接线缺口；原有 race/Redis 集成与生成门禁通过。附[问题](bug/REVIEW-2026-09-29-services.md)、[复现](bug/REPRO-2026-09-29-services.md)、[设计机制](review/IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md)及[进度](review/PROGRESS.md)。仅文档，未修生产代码。

**维护与接手入口：[核心优化汇总与验收边界](CORE-OPTIMIZATION-HANDOFF.md) · [roost 写代码 skill](agent-skills/roost-coding/SKILL.md)**。汇总 Nest、Sync、DataEngine、Remote 历轮最终实现与可复跑证据；包含2026-09-26用户对当前Sync尾延迟的接受决定。下方历史记录保留各自时点状态。

**[三仓合一仓：给 review 的交接](feature/SINGLE_MODULE_MIGRATION.md)** · **[方案与阶段门禁](ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)**（2026-09-20 完成，core v1.16.0）：框架只剩一个仓库、一个 Go module、一个 tag；roost-kit 与 roost-codegen 已归档，旧 tag 仍可 pin。跨过这条边界的工程用 `roost project upgrade --consolidate` 改写 import。

[09-20 修复验收、实时同步与所有权续审](review/REVIEW-2026-09-20.md)：两个活动 Wanted 完成分流，确认 datagram 增量永久丢字段、Remote BSON uint64、player owner ABA/失主双写及 pending poison 前缀五项问题；附复现、进度和实施交接。未修改源码。

[09-19 第二轮实体加载与活动交接审查](review/REVIEW-2026-09-19-02.md)：新确认 singleflight panic 污染、Nest 缺失实体、pending 分页饥饿与 activity 交付断链；附复现、进度和基于现有框架原语的实施方案。未修改源码。

[09-18 第三轮修复验收、Wanted 分流与K1续审](review/REVIEW-2026-09-18-03.md)：RR-03/04 原根因通过；10条 Wanted 已收敛，新增6项问题与独立复现；补充 scene 兴趣/身份/生命周期和生成 DAO 所有权机制。未改源码。

[09-18 第二轮验收与K1审查](review/REVIEW-2026-09-18-02.md)：8项旧修复原触发通过；新发现nested撤销后漏提交、领取账本清理后旧副本重复发奖。问题、复现、机制与后续范围均已记录；未改源码。

[Review 覆盖率统计基线](review/COVERAGE-2026-09-18.md)：三仓主要源码 699 文件，历史报告路径触达 131 文件（18.74%）；附可复算清单与限制，不将引用率当成审完率。

[09-18 状态同步接入与总体进度](review/REVIEW-2026-09-18.md)：Wanted-05 已分流，新增生成配置不兼容与房间持久化屏障接线两个 P2；旧问题按用户未修复声明跳过验收。

[09-17 第三轮新增 Feature 审查](review/REVIEW-2026-09-17-03.md)：副本重复/错误发奖 P1、DAO 嵌套通知、attribute 契约、Saga 完成订阅、battle 宽限期五项新问题；Wanted 分流、完整复现与实施建议已归档。

[09-17 第二轮 Wanted 与修复验收](review/REVIEW-2026-09-17-02.md)：六服务及 directory 的 ARCH-05 交接、platform 自动重试新问题、匹配键升级限制。

[09-17 匹配新边界审查](review/REVIEW-2026-09-17.md)：新增队列键碰撞、分数计算溢出、内存结果共享三个问题，附最小复现与匹配机制学习文档；用户未修复，本轮跳过旧问题验收。

[09-16 实施交接验收](review/REVIEW-2026-09-16-05.md)：Recover/Grouping 与服务迁移已落实，真实 demo 两种依赖模式通过；新确认 manager 两项生命周期问题，保留 durable 升级待办。

[09-16 修复验收与 Kit 职责实施交接](bug/REVIEW-2026-09-16-04.md)：七份修复记录独立验收、两项新问题、Kit 默认 durable 迁移提醒，以及 Core/Kit/Codegen 分层实施清单。

[09-16 第三轮真实 Broker 审查](review/REVIEW-2026-09-16-03.md)：35 新场景，确认 JetStream 本地广播变分摊及分片无法重组，补真实重连/结算证据。

[09-16 第二轮 JetStream/NATS](review/REVIEW-2026-09-16-02.md)：32 新场景，Prefix 消费者身份冲突、停止交接观察，附确认/结算机制与复现。

[09-16 Journal 故障与 SyncBus](review/REVIEW-2026-09-16.md)：28 新场景，记录不确定错误处理问题和 PatchSyncer 交付契约。

[09-15 第八轮 History 快照交接](review/REVIEW-2026-09-15-08.md)：29 新场景，持久化替换与 Recover 并发捕获问题，附验证代码和方案。

[09-15 第七轮 History/ACK/Journal](review/REVIEW-2026-09-15-07.md)：20 新场景，确认流身份复用和 WAL 半尾续写问题，附复现与方案。

[09-15 第六轮生命周期与 SyncStream](review/REVIEW-2026-09-15-06.md)：24 新场景，确认 downstream 回调迁移问题，记录分片和业务交付语义。

[09-15 第五轮 Room 传输审查](review/REVIEW-2026-09-15-05.md)：21 新场景，确认剔除通知丢失问题，附准入机制和修复方向。

[09-15 第四轮 EntitySync 失败交接](review/REVIEW-2026-09-15-04.md)：17 新场景通过，记录分片阻塞观察与后续 sink 审查入口。

[09-15 第三轮 EntitySync 审查](review/REVIEW-2026-09-15-03.md)：跳过旧修复，新增订阅/分发绕过持久化水位问题。

[09-15 第二轮验收与交付边界](review/REVIEW-2026-09-15-02.md)：五项旧问题原场景通过，新发现 stale 提交后的客户端静默分叉。

[09-15 StateSync 生命周期审查](review/REVIEW-2026-09-15.md)：满容量合法替换被增删顺序误拒，新增问题与对照证据已记录。

[第八轮 StateSync LOD 续审](review/REVIEW-2026-09-14-08.md)：新增错相发送导致组件冻结；历史淘汰与 PreparedFrame 边界通过，旧问题未复核。

[第七轮 StateSync 续审](review/REVIEW-2026-09-14-07.md)：旧问题保持未修复，新增 ForceFull/旧 ACK 与单片限制问题，进度及机制已更新。

[第六轮：StateSync 与 Lockstep 漏审复盘](review/REVIEW-2026-09-14-06.md)。RR-09 已验收，RR-10 待修复；下文保留历史时点状态。

[第五轮 LockstepBot 与可靠追帧](review/REVIEW-2026-09-14-05.md)：输入身份上限已验收，消费者错误后跳帧待修复。

[Lockstep 与 Sync 审查状态](review/LOCKSTEP-AND-SYNC-COVERAGE.md)：先补 lockstep，再 statesync → entitysync → syncstream/syncbus；[第四轮结果](review/REVIEW-2026-09-14-04.md)。

[09-14 lockstep 专项](review/REVIEW-2026-09-14-03.md)：重传身份、追帧收敛、座位与协议配置；[实现学习](review/IMPLEMENTATION-LOCKSTEP-INPUT-AND-CATCHUP.md)。

[09-14 第二轮审查](review/REVIEW-2026-09-14-02.md)：WAL 关闭修复验收，Activity 窗口与派发两个新 P2；[实现学习](review/IMPLEMENTATION-ACTIVITY-WINDOW-AND-DISPATCH.md)。

[09-14 扩展审查](review/REVIEW-2026-09-14.md) · [未收敛工作表](review/OPEN-QUESTIONS.md)：WAL 关闭新问题、跨实例删除与真实共享模式恢复。

[Review 进度统计](review/PROGRESS-SNAPSHOT-2026-09-13.md) · [第十轮 LeaveShared 恢复审查](review/REVIEW-2026-09-13-10.md)。

[第九轮共享模式审查](review/REVIEW-2026-09-13-09.md)：新增 P2 EnterShared 回复丢失后独占写准入。

[第八轮修复验收](review/REVIEW-2026-09-13-08.md)：L2 残余、真实 Redis Transfer、真实 WAL、Mail、回调子进程通过。

[第七轮修复验收](review/REVIEW-2026-09-13-07.md)：过期与回填通过，删除/L2 冲突仍有残余。

[09-13 第六轮 Mirror 生命周期验证](review/REVIEW-2026-09-13-06.md)：停机、旧回调与失败重试。

[Mirror 实施交接](review/PLAN-REMOTE-POLICY-MIRROR.md)：复用现有工具的模块分工、协议选择、六步实施与验收。

Roost 是面向 Linux 生产环境的通用 Go 游戏服务器框架，**一个 Go module**：运行时（契约 + 实现 + 技能系统）在仓库根部，装配层与通用服务在 `kit/`，项目与样板代码由 `codegen/` 下的生成器产出（CLI：`go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest`）。文档按阅读者的目标分为三级，没必要从头读到尾。

## 第一级：完全新手

阅读 [五分钟快速开始](QUICKSTART.md)。目标是生成一个项目、启动依赖、运行一个 Service，并知道业务代码应该写在哪里。此级不要求理解 WAL、fence 或 Saga。跑不起来或看到不认识的错误时，先查[问题快速定位](TROUBLESHOOTING.md)：按症状索引，每行给出原因、看哪里、怎么处理。

## 第二级：有经验的游戏后端开发者

阅读 [开发者完整使用说明](USER_GUIDE.md)。它说明如何选择 Mod、组织 Service、编写 Entity/DAO/Nest handler、主动 Flush、使用 Remote Entity/Saga、选择状态同步或帧同步，并给出测试和运维边界。

## 第三级：框架维护者与生产负责人

- [09-13 第四轮修复验收](review/REVIEW-2026-09-13-04.md)：五项独立通过、expiry 部分修复与真实 Redis 溢出边界。

- [09-13 第三轮真实 Redis 审查](review/REVIEW-2026-09-13-03.md)：所有权转移未知结果、大计数边界与旧问题真实 Lua 验证。

- [09-13 第二轮多级缓存审查](review/REVIEW-2026-09-13-02.md)：冲突吞错、回填绕过校验、schema 与绝对过期，八个场景及复现源码。

- [09-13 Remote 接收与恢复审查](review/REVIEW-2026-09-13.md)：删除顺序、订阅代际、payload 身份与快照加载取消，四项新问题和完整复现。

- [RemotePolicy Mirror 审查与实现方案](review/IMPLEMENTATION-REMOTE-POLICY-MIRROR.md)：现有工具复用、只读边界、首次加载、墓碑与断线恢复，尚未实施。

- [09-12 扩展审查](review/REVIEW-2026-09-12.md)：completion 饱和崩溃与真实 WAL 截止时间、持久化屏障和恢复验证。

- [09-11 第四轮异步完成审查](review/REVIEW-2026-09-11-04.md)：回调异常新 P2、慢完成与重复 Shutdown 验证。

- [09-11 第三轮修复验收](review/REVIEW-2026-09-11-03.md)：四项修复通过，新增 Mail 拒绝原子性问题及实现学习。

- [Remote / Nest 专项审查](review/REVIEW-2026-09-11-02.md)：停机边界两个 P2、实现学习和性能微基准。

- [2026-09-11 修复验收与续跑](review/REVIEW-2026-09-11.md)：八项原触发通过，新增 Mail 墓碑期限与副本隔离问题。

- [2026-09-10 第三轮审查](review/REVIEW-2026-09-10-03.md)：M-01～M-05 增量、生成消费者、Remote/Skill/Mail 恢复边界。

- [2026-09-10 第二轮审查](review/REVIEW-2026-09-10-02.md)：Remote 预分派、Mail 去重保留及复现。

- [2026-09-10 Room/Skill/Match 审查](review/REVIEW-2026-09-10.md)：生命周期、取消与过期验证，附实现学习和复现。

- [持续代码 Review 与学习记录](review/README.md)：三仓最新基线、审查范围、验证和下一轮入口。
- [Review 跨轮进度](review/PROGRESS.md)：模块证据、源码基线、验证限制与后续范围。
- [第四轮扩大审查](review/REVIEW-2026-09-09-04.md)：三项修复独立验收、Match 与 Entity 输出新问题及实现学习。
- [2026-09-09 生命周期与生成消费者审查](review/REVIEW-2026-09-09-03.md)：停机完成语义、多实体生成及独立复现。
- [2026-09-09 Bugfix 独立验收](review/REVIEW-2026-09-09-02.md)：原问题复测、Session 清理竞态及学习记录。
- [2026-09-09 三仓架构评估](review/REVIEW-2026-09-09.md)：实现边界、可靠性证据、接入验证与下一阶段建议。
- [Review 问题索引](bug/README.md)：确认问题、复现证据及状态；默认只报告、不修改代码。
- [Core/Kit 实现下沉与缺陷收敛统一实施方案](CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)：迁移批次、历史审计衔接、测试门禁及跨仓验收。
- [Roost v2 收敛方案：五仓合三仓、实现下沉 Core](ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md)：目标形态、包映射表、P0～P6 分阶段门禁、在途改动处置。

- [实现原理与不变量](INTERNALS.md)：生命周期、锁、事务、WAL/Data Engine、远程实体、Saga、实时同步及失败语义。
- [生产部署手册](DEPLOYMENT.md)：Shell/systemd、Docker、Kubernetes、发布、回滚、备份、容量与故障演练。
- [薄弱点与路线图](ROADMAP.md)：哪些是发布阻断项，哪些是增强项，哪些不应进入框架核心。
- [多仓研发与发布](DEVELOPMENT_WORKSPACE.md)：go.work source-head 联调、`GOWORK=off` 发布门禁与版本顺序。
- [收敛覆盖账本](history/ledger.md)：bug 收敛的工作单元协议、包 × 缺陷类覆盖矩阵、待开单元与单元日志。

## 功能实施记录

- [game-demo 参考实现](feature/GAME_DEMO_TEMPLATE.md)：`-template game-demo` 六批实施的交接文档——机制、链路、验证命令、实跑步骤、发现并修掉的框架问题、环境陷阱、剩余工作。

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
- [装配层与 Mod（kit/）](../kit/README.md)
- [技能系统（core/skill）](skill/README.md)
- [通用服务（kit/service/）](../kit/service/README.md)
- [生成器（codegen/）](../codegen/README.md)
- [仓库级脚本（scripts/）](../scripts/README.md)

## 版本基线

**一个模块，一个 tag。** 当前正式版本见仓库根的 [CHANGELOG](../CHANGELOG.md) 顶部；
机器可校验的记录是 `codegen/ci/framework-release.yaml` 的 `release` 一行，
`scripts/pretag.sh` 在打 tag 之前比对它，release workflow 在 tag 之后再比对一次。

- 业务工程只 require `github.com/tjbdwanghaibo/roost-core` 一条，CLI 用
  `go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest`。
- **只有仓库根部（运行时）的改动进兼容承诺**；`kit/`、`codegen/`、`demo/` 的改动不构成框架行为变化，
  CHANGELOG 分节标明。要不要升级看分节，不要只看版本号跳了几位。
- 正式项目不得依赖 `@latest`、伪版本或本地 `replace`。
- 从 core v1.16.0 之前升上来要先跑 `roost project upgrade --consolidate`（见
  [三仓合一仓](ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)）；更早的 roost-skill / roost-service
  边界见 [五仓合三仓](ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md)。

> 这一节原来逐条罗列每次发版的内容，长到没人读、且停在 v1.15.11 不再更新。
> 发版历史属于 CHANGELOG，这里只留不会过期的规则（2026-09-21 重写）。
