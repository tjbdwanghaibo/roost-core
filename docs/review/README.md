# Roost 持续 Review 与学习记录

[2026-09-25 合并前验收](MERGE-2026-09-25.md)：累计核心模块优化、全模块检查与保留的性能边界。

09-25 [Nest慢池并发与队列实测](NEST-SLOW-WORKERS-2026-09-25.md)：10轮同源码对比；128/16通过100 TPS×120秒全量验收，256吞吐无明显收益且延迟增加，1024在120 TPS过载时大量超时。全局默认未改，保留所有失败样本及复跑入口。

09-25 [Nest 双池再次复核](NEST-FAST-SLOW-2026-09-25.md)：修复小等待队列提前拒绝与回滚 panic 锁泄漏，七包 race/vet、定向并发 ×20 通过。1024/16 调度容量已验证；复用上一轮性能基线，未将其视为本轮或 1024 档的新 TPS 证明。

09-25 Nest 双池：普通/广播/Cost/Remote 的业务统一进快池，慢池仅负责前后置 I/O；七包 race、21/21 故障矩阵通过，100 TPS ×120 秒零错误且全量校验通过，120 TPS 背压过载；[方案与本轮验证](../feature/REFACTOR-2026-09-25-nest-fast-slow.md)、[RR-07](../bugfix/RR-20260925-07.md)。

09-25 Remote 调度隔离与容量收口：获取/确认/释放进入独立慢池；本机 80 TPS × 10 分钟 48000 笔零错误、全量校验通过，90 TPS 长测 6 次超时，不作为稳定通过档位。race/vet 和 21/21 故障矩阵通过。[方案与完整数据](../feature/REFACTOR-2026-09-25-nest-remote-stages.md)、[RR-06](../bugfix/RR-20260925-06.md)。

[09-25 Remote 超时定位与 60 TPS 长稳](../feature/REFACTOR-2026-09-25-remote-throughput.md)：修复独立 Entity 串行投影导致的 Remote 确认与 Nest 排队超时；默认 8 路有界并行、同 Entity 顺序和 WAL 连续前缀确认保持。30 分钟 108000 笔全成功，59.997 TPS，p50/p95/p99=124/262/845ms；全量一致性、21/21 故障矩阵和五包 race/vet 通过，未部署。下方旧版 30 分钟失败证据保留。

[09-25 Remote 默认许可与诊断优化复测](../feature/REMOTE-UNIFIED-2026-09-25.md)：删除未发布旧协议分支、限制诊断开销；保留中间退化与失败样本，运行跟踪定位并移除发布阶段重复 Mongo 事务。21/21 故障矩阵与 60 秒全量负载校验通过；追加同代码 30 分钟测试未通过：实际 19.997 TPS、35996 成功/4 错误/0 丢弃，p50/p95/p99=55/646/1506ms，首错 nest: sync timeout，未执行最终一致性验收。24 小时未跑。

[09-25 Remote 持久权限修复与投影批量优化](../feature/REMOTE-AUTHORITY-2026-09-25.md)：修复 RR-26，正式写路径启用 Mongo 权威；21 组故障项目含修后补跑均有通过证据，保留初次失败。容量与当轮验收以此报告为准；未部署前提下的接口收敛见后续报告。
[Remote 容量、长稳与集群故障验收](../feature/REMOTE-ACCEPTANCE-2026-09-24.md)：正式 1000 业务 worker / 10000 实体负载、30 分钟实跑、24 小时复跑入口与有界故障矩阵；当前运行状态和最终数据以报告为准。

[09-24 Remote 第三轮](../feature/REFACTOR-2026-09-24-remote.md#第三轮进程故障与正式业务链路)：多进程强杀接管、Redis 网络隔离后旧 owner 拒绝解锁，以及正式生成多实体多 DAO → Nest → WAL/Mongo → NATS 接收通过；修复 RR-23 准入预算。以下条目保留各轮当时判断，最新范围以第三轮记录为准，尚非生产容量或完整故障矩阵认证。

[09-24 Remote 第二轮](../feature/REFACTOR-2026-09-24-remote.md#第二轮启停与锁代际)：修复 RR-19～22；双 Redis 客户端续租/交接、双 Manager 所有权争抢/转移和真实 Mongo/WAL 恢复通过。单进程验证不替代多进程故障和完整业务压测。

[09-24 Remote 事务链路优化](../feature/REFACTOR-2026-09-24-remote.md)：同包整理 tracker，消除满容量与 Stats 全表扫描；修复 RR-16/17/18。真实 Mongo/Redis/WAL 的多实体原子提交、发布失败恢复、确认丢失重放及冷 Manager 读快照通过。多节点所有权与完整业务压测尚待验收。

[09-24 DataEngine 剩余优先项实施](../feature/DATAENGINE-BATCH-2026-09-24.md)：多 DAO 批量、安全 checkpoint 合并、准入上限/健康预警已落地，修复 RR-14/15。36 样本同参数复测通过，pair/pipelined 最终落库约 58.5 倍；100/s 四 DAO 输入结束后约 29ms 排空。以下旧条目保留历史判断，最新状态以该记录为准。

[09-24 DataEngine 真实压测与截止评估](../feature/DATAENGINE-PRESSURE-2026-09-24.md)：36 个最终样本、50,688 笔计时事务，单/双/四 DAO 与热点场景逐文档校验通过；20/s 持续负载无增长积压，100/s 四 DAO 输入超过本机处理能力。功能正确性批次可收口，多 DAO 投影与 checkpoint 性能仍需优化；Remote 留后续。

[09-24 DataEngine 恢复验收](../feature/DATAENGINE-RECOVERY-2026-09-24.md)：100k 跨段积压、慢存储取消、6 类真实依赖故障与正式生成 DAO/Nest 三独立进程验证；修复 RR-13 慢发布退避。保留生产容量、任意点强杀和断电的验证边界。

[09-24 DataEngine 生命周期与重放分配优化](../feature/REFACTOR-2026-09-24-dataengine.md#8-生命周期与重放分配继续实施)：RR-11/12 修复启停回收和 Remote 永久冲突处理；空闲/held 重放分配下降约 96%/94%，8 包 race、真实依赖 6 项及公会重启回归通过。历史 WAL 与文档概念已澄清。

[09-24 DataEngine 继续实施](../feature/REFACTOR-2026-09-24-dataengine.md#7-2026-09-24-继续实施)：D1 主线拆分与中文契约已实施，RR-10 Outbox 启停修复；原始 WAL 定位历史 Remote 公会 ID 重启复用，持久化发号和真实 Mongo/WAL 恢复验证通过。D3 基准已建立，D2/D4 扩展矩阵尚未全部完成，历史冲突 WAL 未自动改写。

[09-24 Sync 最终截止复核](../feature/SYNC-COMPLETION-2026-09-23.md#2026-09-24-最终截止复核)：已授权优化与双模式正式接入完成，Nest 收尾后的核心链路、18 包 race 和独立生成工程验证完成；转入维护。保留 5% 变化率严格 50ms 长尾及真实部署验收边界，未发布。

[09-24 Nest 再收尾与消息吞吐](../feature/NEST-MSG-THROUGHPUT-2026-09-24.md)：复核并回归当前完成边界，新增独立进程 Dispatch/Request、热点/10000 实体分散压测，按真实完成消息数计量。

[09-24 Nest 截止复核](../feature/NEST-COMPLETION-2026-09-24.md#8-截止复核与收口结论)：补齐释放异常和内联/降级回调异常的完成责任，修复 RR-07/08；既定批次与本轮已确认问题已收口，转入维护。验证与线上验收边界见记录。

[09-24 Nest N1～N4 收尾验收](../feature/NEST-COMPLETION-2026-09-24.md)：事务主线与完成所有权、可选阶段观测、基于 profile 的热路径优化已落地，补充修复异步回调早于解锁和 Ticker 启停竞争。性能数据与验证范围见报告，未发布。

[09-24 Nest 常规调度收尾](../feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md#9-nest-常规调度收尾n2a)：single/multi/multiGroup 共用锁与执行收尾，保留顺序/缺失实体/分组语义；修复广播释放 panic 的引用泄漏与中断。Sync 保持现有契约，不作为本批重构中心。

[09-24 Nest 后续优化](../feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md#8-后续实施n1-与-cast-事务边界)：N1 同包职责整理已落地，主调度 1103 → 518 行；修复未接 Sync 时动态 Cast 的回滚/解锁/提交水位问题，相关 race 与生成工程通过。当时 N2～N4 未整体实施；最新完成状态见上方收尾验收，未发布。

[09-24 Entity Sync 双模式正式接入](../feature/IMPLEMENTATION-2026-09-24-sync-modes.md)：已实施，周期模式保持默认，变化触发模式锁内冻结、解锁/确认后发送、20Hz 兜底；正式 Nest / Kit / codegen / Interest 接入，相关 race 与生成最小工程验证通过，未发布。

[09-24 Nest 优化与即时状态同步方案](../feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)：保留首次审查与两个锁/提交问题的证据；双模式正式接入、N1 整理及 Cast 修复的后续状态见上方记录。

[09-24 Sync 六项实施](../feature/REFACTOR-2026-09-24-sync-six-items.md)：完整实体包组帧、profile 装配校验、快照预算、临时内存与 AOI 复用、队列治理和观测；详见实施记录中的验证与限制。

[09-24 Sync 剩余优化评估](SYNC-REMAINING-2026-09-24.md)：基于 A～E 完成后的代码重新采样；优先字节拆帧、profile 装配校验和全量突发控制，再考虑分配、AOI 查询与队列治理。本轮仅分析，未修改运行代码。

[09-23 Sync 后续优化实施](../feature/REFACTOR-2026-09-23-sync-next-steps.md)：A～E 已落地；AOI/Flush 减分配、会话引用按需复制、观测和异步负载、[字段视图与优先级](../feature/SYNC-PROFILES.md)。20Hz/1%、5% 同模型分配下降约 58%、47%；回归通过，未发布。

[09-23 目标规模 AOI 验证](../feature/SYNC-AOI-1000-10000-2026-09-23.md)：1000 玩家、10000 实体、每人约 50 可见，双进程；最新 20Hz/1%、5% 低变化率六轮数据校验通过，严格 50ms 最大延迟门禁仍未通过；用户认可当前容量。

[09-23 Sync 业务负载验证](../feature/SYNC-BUSINESS-LOAD-2026-09-23.md)：生成 demo 的真实事务、AOI、BSON 与回环 TCP；单独记录容量配置边界及测量范围。

[09-23 Sync 收尾：本轮范围已完成并验收，未发布](../feature/SYNC-COMPLETION-2026-09-23.md)：R1～R3、RR-01～07、M-19 多来源订阅、M-20 共享组件编码；[性能基准与取舍](../feature/SYNC-BENCHMARKS.md)。真实环境验证及网关多播单独跟进。

[09-23 Sync 优化审查与重构方案](../feature/REFACTOR-2026-09-23-sync-readability.md)：基线 `967bc69`，核对八包结构与导入，深入实体同步交付链路；确认并修复退订残留、部分交付重试、满容量替换三项问题，同包职责整理、Go API 更新与命名文档对齐三批已实施，验收见记录。

[09-20 修复验收、实时同步与所有权续审](REVIEW-2026-09-20.md)：Core `8c589a6` / Kit `19fb010` / Codegen `9bbac81`；六项新修复原触发通过，两个 Wanted 完成分流，新增[4个P1、1个P2](../bug/REVIEW-2026-09-20.md)，附[复现](../bug/REPRO-2026-09-20.md)和[增量传输、租约 fencing 与持久化类型边界](IMPLEMENTATION-DELTA-TRANSPORT-AND-LEASE-FENCING.md)。

[09-19 第二轮：实体加载、Nest 分派与 Activity 交接](REVIEW-2026-09-19-02.md)：Core `a2e8fa0` / Kit `5116f2a` / Codegen `fde74d1`；新确认[3个P1、1个P2](../bug/REVIEW-2026-09-19-02.md)，附[复现与证据](../bug/REPRO-2026-09-19-02.md)及[实体加载与活动交接机制](IMPLEMENTATION-ENTITY-LOAD-AND-ACTIVITY-HANDOFF.md)。

[09-19 新修复验收、K1 与 platform 支付续审](REVIEW-2026-09-19.md)：最终同步 Core `1947faa` / Kit `5116f2a` / Codegen `7297f92`；装备/迁移/U-0245 与支付正向门通过，RR-06 更正为部分修复；新增[6个P1](../bug/REVIEW-2026-09-19.md)及[复现](../bug/REPRO-2026-09-19.md)。后三项是订单/索引非原子、坏单阻塞整页和未履约 grant 过期删除。W-11 转 [ARCH-07 运行期配置所有权](IMPLEMENTATION-RUNTIME-CONFIG-OWNERSHIP.md)。

[09-18 第三轮修复验收、Wanted 分流与K1续审](REVIEW-2026-09-18-03.md)：RR-03/04 原根因独立通过；10条 Wanted 全部分流；新增[3个P1、3个P2](../bug/REVIEW-2026-09-18-03.md)及[复现](../bug/REPRO-2026-09-18-03.md)。[Scene 兴趣/身份/生命周期机制](IMPLEMENTATION-SCENE-INTEREST-IDENTITY-AND-LIFECYCLE.md) · [生成契约补充](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。

[09-18 第二轮修复验收与K1进度](REVIEW-2026-09-18-02.md)：8项旧RR原触发通过；独立46场景38过8失败，确认[两个新问题](../bug/REVIEW-2026-09-18-02.md)。[完整复现](../bug/REPRO-2026-09-18-02.md) · [回滚关系与幂等寿命机制](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。K1局部14场景已执行，K1/K2/K3均不标整体完成。

[核心功能分类与优先级](CORE-FUNCTION-CLASSIFICATION.md)：八个运行时功能域，Kit/Codegen 横向接入必查，按核心不变量而非文件数安排后续审查。

[审查覆盖统计与口径](COVERAGE-2026-09-18.md) · [逐文件清单](coverage/FILES.csv) · [引用证据](coverage/EVIDENCE.csv) · [复算脚本](coverage/measure.ps1)。区分文档触达、局部源码证据、整文件完成和行为场景覆盖。

[09-18 消费链与总体进度](REVIEW-2026-09-18.md) · [两个新 P2](../bug/REVIEW-2026-09-18.md) · [完整复现](../bug/REPRO-2026-09-18.md) · [机制补充](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。旧问题跳过验收；Wanted-05 已明确分流。

[09-17 第三轮新增 Feature / Wanted](REVIEW-2026-09-17-03.md) · [五项问题及实施交接](../bug/REVIEW-2026-09-17-03.md) · [完整复现](../bug/REPRO-2026-09-17-03.md) · [跨层契约机制](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。状态同步接入保留观察。

[09-17 第二轮 Wanted/验收](REVIEW-2026-09-17-02.md) · [问题](../bug/REVIEW-2026-09-17-02.md) · [23 场景源码](../bug/REPRO-2026-09-17-02.md) · [ARCH-05 七包边界](IMPLEMENTATION-SERVICE-PLACEMENT-AND-RECOVERY.md)。

[09-17 匹配身份与边界](REVIEW-2026-09-17.md) · [三个新问题](../bug/REVIEW-2026-09-17.md) · [三个最小复现](../bug/REPRO-2026-09-17.md) · [匹配身份、所有权与配对机制](IMPLEMENTATION-MATCH-IDENTITY-OWNERSHIP-AND-GROUPING.md)。用户未修复，本轮跳过旧验收。

[09-16 第五轮交接落实情况](REVIEW-2026-09-16-05.md) · [manager 两项新问题与复现](../bug/REVIEW-2026-09-16-05.md) · [服务与生命周期分层机制](IMPLEMENTATION-CORE-KIT-SERVICE-AND-MANAGER.md)。

[09-16 第四轮验收](REVIEW-2026-09-16-04.md) · [统一实施交接（七项验收、两项新问题与 Kit 分层）](../bug/REVIEW-2026-09-16-04.md)。

[09-16 第三轮真实 Broker](REVIEW-2026-09-16-03.md) · [本地扇出新问题](../bug/REVIEW-2026-09-16-03.md) · [35 场景完整代码](../bug/REPRO-2026-09-16-03.md) · [机制补充](IMPLEMENTATION-JETSTREAM-SYNCBUS-AND-LIFECYCLE.md)。

[09-16 第二轮 JetStream/NATS](REVIEW-2026-09-16-02.md) · [确认与生命周期机制](IMPLEMENTATION-JETSTREAM-SYNCBUS-AND-LIFECYCLE.md) · [新问题及观察](../bug/REVIEW-2026-09-16-02.md) · [32 场景代码](../bug/REPRO-2026-09-16-02.md)。

[09-16 Journal 故障与 SyncBus](REVIEW-2026-09-16.md) · [Patch/Delivery 机制](IMPLEMENTATION-SYNCBUS-PATCH-AND-DELIVERY.md) · [新问题](../bug/REVIEW-2026-09-16.md) · [28 场景](../bug/REPRO-2026-09-16.md)。

[09-15 第八轮快照交接](REVIEW-2026-09-15-08.md) · [两个新问题](../bug/REVIEW-2026-09-15-08.md) · [29 场景代码](../bug/REPRO-2026-09-15-08.md)。

[09-15 第七轮 History/Journal](REVIEW-2026-09-15-07.md) · [机制](IMPLEMENTATION-SYNCSTREAM-HISTORY-AND-JOURNAL.md) · [两个新问题](../bug/REVIEW-2026-09-15-07.md) · [20 场景](../bug/REPRO-2026-09-15-07.md)。

[09-15 第六轮生命周期与分片](REVIEW-2026-09-15-06.md) · [SyncStream 机制](IMPLEMENTATION-SYNCSTREAM-FRAGMENTS-AND-DELIVERY.md) · [新问题](../bug/REVIEW-2026-09-15-06.md) · [24 场景](../bug/REPRO-2026-09-15-06.md)。

[09-15 第五轮 Room 传输审查](REVIEW-2026-09-15-05.md) · [准入与剔除机制](IMPLEMENTATION-ROOM-ADMISSION-AND-EVICTION.md) · [新问题](../bug/REVIEW-2026-09-15-05.md) · [21 场景代码](../bug/REPRO-2026-09-15-05.md)。

[09-15 第四轮失败与生命周期](REVIEW-2026-09-15-04.md) · [观察项](../bug/REVIEW-2026-09-15-04.md) · [17 场景代码](../bug/REPRO-2026-09-15-04.md)。

[09-15 第三轮 EntitySync](REVIEW-2026-09-15-03.md) · [订阅与持久化机制](IMPLEMENTATION-ENTITYSYNC-SUBSCRIPTION-AND-DURABILITY.md) · [问题](../bug/REVIEW-2026-09-15-03.md) · [复现](../bug/REPRO-2026-09-15-03.md)。

[09-15 第二轮旧修复验收](REVIEW-2026-09-15-02.md) · [重叠准备新问题](../bug/REVIEW-2026-09-15-02.md) · [复现](../bug/REPRO-2026-09-15-02.md)。

[09-15 生命周期与容量边界](REVIEW-2026-09-15.md) · [问题](../bug/REVIEW-2026-09-15.md) · [复现](../bug/REPRO-2026-09-15.md)。

[第八轮 LOD、历史与提交交接](REVIEW-2026-09-14-08.md) · [新问题](../bug/REVIEW-2026-09-14-08.md) · [独立复现](../bug/REPRO-2026-09-14-08.md)。

[第七轮：恢复屏障与分片重组](REVIEW-2026-09-14-07.md) · [两项新问题](../bug/REVIEW-2026-09-14-07.md) · [复现](../bug/REPRO-2026-09-14-07.md)。

[第六轮](REVIEW-2026-09-14-06.md) · [StateSync 机制](IMPLEMENTATION-STATESYNC-BASELINE-AND-ACK.md) · [U-0199 漏审复盘](POSTMORTEM-LOCKSTEP-U0199.md)。

[09-14 第五轮](REVIEW-2026-09-14-05.md)：去重环验收、真实 TCP 追帧、新 Bot 跳帧问题 · [问题](../bug/REVIEW-2026-09-14-05.md) · [复现](../bug/REPRO-2026-09-14-05.md)。

[09-14 第四轮 lockstep 续审](REVIEW-2026-09-14-04.md) · [内存边界问题](../bug/REVIEW-2026-09-14-04.md) · [复现](../bug/REPRO-2026-09-14-04.md) · [lockstep / sync 覆盖与优先级](LOCKSTEP-AND-SYNC-COVERAGE.md)。

[09-14 第三轮 lockstep](REVIEW-2026-09-14-03.md) · [四项问题](../bug/REVIEW-2026-09-14-03.md) · [复现](../bug/REPRO-2026-09-14-03.md) · [输入与追帧机制](IMPLEMENTATION-LOCKSTEP-INPUT-AND-CATCHUP.md)。

[09-14 第二轮](REVIEW-2026-09-14-02.md)：WAL 修复验收、Activity 两个 P2 · [问题](../bug/REVIEW-2026-09-14-02.md) · [复现](../bug/REPRO-2026-09-14-02.md) · [Activity 机制](IMPLEMENTATION-ACTIVITY-WINDOW-AND-DISPATCH.md)。累计 26 轮、39 RR，详见 [进度](PROGRESS.md)。

[09-14 运行](REVIEW-2026-09-14.md) · [问题](../bug/REVIEW-2026-09-14.md) · [复现](../bug/REPRO-2026-09-14.md) · [未收敛工作表](OPEN-QUESTIONS.md)。

[进度统计](PROGRESS-SNAPSHOT-2026-09-13.md)：24 轮、36 RR；[第十轮运行](REVIEW-2026-09-13-10.md)、[问题](../bug/REVIEW-2026-09-13-10.md)。

[第九轮运行](REVIEW-2026-09-13-09.md) · [问题](../bug/REVIEW-2026-09-13-09.md) · [复现](../bug/REPRO-2026-09-13-09.md)。

[第八轮运行](REVIEW-2026-09-13-08.md) · [验收与复跑](../bug/REVIEW-2026-09-13-08.md) · [修复机制学习](IMPLEMENTATION-REPAIR-ACCEPTANCE.md)。

[第七轮运行](REVIEW-2026-09-13-07.md) · [问题](../bug/REVIEW-2026-09-13-07.md) · [复现](../bug/REPRO-2026-09-13-07.md)。

[09-13 第六轮运行](REVIEW-2026-09-13-06.md) · [生命周期机制](IMPLEMENTATION-MIRROR-LIFECYCLE.md) · [测试源码](../bug/REVIEW-2026-09-13-06.md)。

[09-13 第五轮：Mirror 实施就绪审查](REVIEW-2026-09-13-05.md) · [实施交接与六步验收](PLAN-REMOTE-POLICY-MIRROR.md)。

每轮先获取 core、kit、codegen 最新代码，再审查和记录，默认不修改代码。
长期流程由个人 skill `roost-review` 执行，可用 `$roost-review` 或“继续 Roost review”调用。

- [跨轮审查进度](PROGRESS.md)：源码基线、已验证场景、限制和下轮入口。
- [实现学习：锁与生命周期](IMPLEMENTATION-STATE-AND-LIFECYCLE.md)。
- [实现学习：领取、匹配重放与生成消费者](IMPLEMENTATION-SERVICE-REPLAY-AND-CODEGEN.md)。

| 日期 | 范围 | 结论 | 文档 |
| --- | --- | --- | --- |
| 2026-09-13 第四轮 | 六项修复、真实 Redis 上限与 expiry 返回路径 | 五项验收通过，一项部分修复 | [运行](REVIEW-2026-09-13-04.md)、[验收](../bug/REVIEW-2026-09-13-04.md)、[复现](../bug/REPRO-2026-09-13-04.md) |
| 2026-09-13 第三轮 | 真实 Redis/Lua、owner 转移回复丢失、计数域 | 新 P2/P3×2；旧两项真实补证；十个场景 | [运行](REVIEW-2026-09-13-03.md)、[实现](IMPLEMENTATION-REMOTE-REDIS-UNCERTAIN-OUTCOMES.md)、[问题](../bug/REVIEW-2026-09-13-03.md)、[复现](../bug/REPRO-2026-09-13-03.md) |
| 2026-09-13 第二轮 | L1/L2 冲突、回填交错、绝对过期与加载恢复 | 四项新 P2，八个临时场景，三包 race 通过 | [运行](REVIEW-2026-09-13-02.md)、[实现](IMPLEMENTATION-REMOTE-L1-L2-CONSISTENCY.md)、[问题](../bug/REVIEW-2026-09-13-02.md)、[复现](../bug/REPRO-2026-09-13-02.md) |
| 2026-09-13 | Remote 接收删除/兴趣/身份、加载取消、总线语义 | 四项新 P2；七个临时场景；七包 race 基线通过 | [运行](REVIEW-2026-09-13.md)、[实现](IMPLEMENTATION-REMOTE-REPLICA-ORDERING-AND-RECOVERY.md)、[问题](../bug/REVIEW-2026-09-13.md)、[复现](../bug/REPRO-2026-09-13.md) |
| 2026-09-12 第二轮 | RemotePolicy Mirror 与已有缓存/副本工具 | 尚无完整只读运行时；原语验证与复用方案完成 | [运行](REVIEW-2026-09-12-02.md)、[实现及方案](IMPLEMENTATION-REMOTE-POLICY-MIRROR.md)、[观察](../bug/REVIEW-2026-09-12-02.md) |
| 2026-09-12 | 饱和回退子进程；真实 WAL 释放、Ack/重开、Shutdown/Sync | 新两个 P2；旧 panic 崩溃风险实测确认；三个正向 WAL 场景通过 | [运行](REVIEW-2026-09-12.md)、[实现](IMPLEMENTATION-WAL-ADMISSION-DURABILITY-AND-SHUTDOWN.md)、[问题](../bug/REVIEW-2026-09-12.md) |
| 2026-09-11 第四轮 | completion 慢确认、回调 panic、Shutdown 重试 | 新 P2；慢完成重试通过；旧 Mail P3 仍复现 | [运行](REVIEW-2026-09-11-04.md)、[实现](IMPLEMENTATION-COMPLETION-FAILURE-AND-SHUTDOWN.md)、[问题](../bug/REVIEW-2026-09-11-04.md) |
| 2026-09-11 第三轮 | 四项修复、Remote/Nest 停机交错、Mail 拒绝原子性 | 四项验收通过；新 P3 一项 | [运行](REVIEW-2026-09-11-03.md)、[实现](IMPLEMENTATION-MAIL-RETENTION-AND-ATOMIC-REFUSAL.md)、[问题](../bug/REVIEW-2026-09-11-03.md) |
| 2026-09-11 第二轮 | Remote / Nest 停机与性能 | 新两个 P2 已复现 | [运行](REVIEW-2026-09-11-02.md)、[实现](IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)、[问题](../bug/REVIEW-2026-09-11-02.md) |
| 2026-09-11 | 八项修复独立验收；Mail 墓碑、事务 TTL | 八项原触发通过；新 Mail P2/P3 各一项 | [运行](REVIEW-2026-09-11.md)、[问题](../bug/REVIEW-2026-09-11.md)、[复现](../bug/REPRO-2026-09-11.md) |
| 2026-09-10 第三轮 | M-01～M-05、Remote 等待者、Skill 恢复、Mail 重试 | 新 3 P2 / 1 P3；含两个新增生成消费者编译缺陷 | [运行](REVIEW-2026-09-10-03.md)、[注册生成实现](IMPLEMENTATION-CATEGORY-REGISTRY-AND-GENERATION.md)、[恢复重放实现](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)、[问题](../bug/REVIEW-2026-09-10-03.md) |
| 2026-09-10 第二轮 | Remote 预分派、Mail 淘汰与重投 | 3 包 race 通过；新 P2：淘汰后重投生成新发奖 token | [运行](REVIEW-2026-09-10-02.md)、[实现](IMPLEMENTATION-REMOTE-PREPARE-AND-FINALIZE.md)、[问题](../bug/REVIEW-2026-09-10-02.md) |
| 2026-09-10 | Match 过期、Room 生命周期、Skill 取消 | 3 包 race 通过，2 个新增行为验收通过；1 个低频 P3 | [运行记录](REVIEW-2026-09-10.md)、[实现学习](IMPLEMENTATION-ROOM-AND-SKILL-SCHEDULING.md)、[问题](../bug/REVIEW-2026-09-10.md) |
| 2026-09-09 第四轮 | 三项修复验收；Guard/Cast、Mail、Match、Entity 输出 | 8 个相关包 race 通过；新增 2 个 P2；建立进度与机制文档 | [学习与验证](REVIEW-2026-09-09-04.md)、[问题](../bug/REVIEW-2026-09-09-04.md) |
| 2026-09-09 第三轮 | Data Engine/Saga 生命周期、Entity 消费者生成 | 新增 2 个 P2；相关基线测试通过，独立复现失败 | [学习与验证](REVIEW-2026-09-09-03.md)、[问题](../bug/REVIEW-2026-09-09-03.md) |
| 2026-09-09 第二轮 | 优先核验用户 bugfix 四项修复 | 原 3 条复现变绿；新增 Session claim ABA；文档版本仍有收尾 | [修复验收与学习](REVIEW-2026-09-09-02.md)、[问题](../bug/REVIEW-2026-09-09-02.md) |
| 2026-09-09 | 最新三仓架构、消费者生成、首轮问题复核 | 旧 3 个 P2 仍复现；新增接入文档 P2；源码消费者编译通过，pure-tag 验证受网络限制 | [评估/学习/验证](REVIEW-2026-09-09.md)、[问题](../bug/REVIEW-2026-09-09.md) |
| 2026-09-08 | core cache/versionstore；kit session；codegen consolidate | 3 个 P2 已复现、未修复，图谱补证待完成 | [学习/验证](REVIEW-2026-09-08.md)、[问题](../bug/REVIEW-2026-09-08.md) |

下一轮从运行记录的完整 SHA 做增量比较，再轮转未覆盖范围。关闭问题必须有独立修复
与验证依据，包测试成功不能代表全仓审计完成。
