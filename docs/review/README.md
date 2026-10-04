# Roost 持续 Review 与学习记录

[10-04 第二批四项修复](REVIEW-2026-10-04-noncore-03.md)：Manager 清理/一次启动、Admin schema、Ops 排空 4/4 已修、声明场景验证，35正式叶子/独立项与11包race/vet通过，未发版。[记录](../bugfix/README.md) · [证据](../bugfix/evidence/noncore-bugfix-20261004-02/README.md) · [进度](PROGRESS.md)。下方原第二批“未修”为历史时点。

[10-04 N01 生命周期与 Ops](REVIEW-2026-10-04-noncore-02.md)：确认[四个新未修问题](../bug/REVIEW-2026-10-04-noncore-02.md)，11 fail / 5 正常 pass / 1 仅观察，六包 race/vet 通过。N01 15/15 生产源码已读，场景仍部分完成；下一步 N02 与 N01 具名余项。[学习/实施建议](IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md) · [证据](evidence/noncore-review-20261004-02/README.md) · [进度](PROGRESS.md)。

[10-04 App/HTTP 四项修复](REVIEW-2026-10-04-noncore-01.md)：RR-20261003-NC-01～04 4/4 已修、声明场景验证，未发版。45 个新增正式场景和原 review 场景通过，11 包 race/vet 通过。[修复记录](../bugfix/README.md) · [证据](../bugfix/evidence/noncore-bugfix-20261004-01/README.md) · [进度](PROGRESS.md)。下方 10-03 “未修”为历史时点。

[10-03 非三大核心第一批](REVIEW-2026-10-03-noncore-01.md)：App/HTTP 确认[四个未修 P2](../bug/REVIEW-2026-10-03-noncore-01.md)，12 失败反例/18 通过控制；七包 race/vet 通过。[15 单元后续范围与完成时间计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [机制学习](IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md) · [进度](PROGRESS.md) · [可复跑证据/当前清单](evidence/noncore-review-20261003-01/README.md)。Service 复用完成矩阵，Codegen 接续缺口；Nest/Sync/DataEngine 主域由另一线接续。

[10-01 B29 性能对照](REVIEW-2026-10-01-b29.md)：6 组 RR 修复（10 对提交）修前 / 修后 benchstat n=10 交替 + E / F 端到端，无新退化；RR-16 / 28-11 / 74 / 25 / 30 的代价如实记录。

[10-01 B 线修复独立复审](REVIEW-2026-10-01-bline-audit-service-1.md)：service 前半（account / mail / platform）16 条——通过 15、缺陷 1（→ RR-20261001-02），12 组回退探针全部变红；[codegen 12 条](REVIEW-2026-10-01-bline-audit-codegen.md)——通过 11、缺陷 1（→ RR-20261001-03 回归）+ 文档 1（→ RR-20261001-04），17 个回归在 v1.17.2 上独立复证为红，9 个生成器的退役只删带自身生成头的文件；另发现 ci Redis job 漏设四个门变量（→ RR-20261001-01，已修）。[service 后半 + redis driver 21 条](REVIEW-2026-10-01-bline-audit-service-2.md)——通过 20、缺陷 1（→ RR-20261001-05，activity pending 名额永不回收）、4 组回退探针全部变红，疑点分流到 W-2026-10-01-03 / 04。

[09-30 Codegen 第六轮修复/验收](REVIEW-2026-09-30-codegen-06.md)：[RR-CG-12](../bugfix/RR-20260930-CG-12.md)/[CG-13](../bugfix/RR-20260930-CG-13.md)/[CG-14](../bugfix/RR-20260930-CG-14.md) 3/3 修复、具名场景通过，未发版。全 Codegen race/vet、glsvet、正式 deps 消费与 DAO/Entity 双模式 Sync 通过；shell 项明确跳过。[证据](../bugfix/evidence/codegen-bugfix-20260930-12-14/README.md) · [机制更新](IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md) · [进度](PROGRESS.md)。

[09-30 Codegen 第五轮](REVIEW-2026-09-30-codegen-05.md)：深入 cfggen 分组/复合 schema/消费者与依赖合仓回写，新增[三个未修 P2](../bug/REVIEW-2026-09-30-codegen-05.md)。正式 CLI 与独立消费者、依赖 overlay 四个反例/四个控制已归档；Codegen 包回归和定向 race 通过，shell 检查明确跳过。[实现学习](IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md) · [证据](evidence/codegen-review-20260930-05/README.md) · [进度](PROGRESS.md)。

[09-30 Codegen 第四轮修复与验证](REVIEW-2026-09-30-codegen-04.md)：RR-06～10 原触发修复，独立工程的 `roost generate --check` 退役判定与消费者编译通过；[修复入口](../bugfix/README.md) · [机制更新](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md) · [进度](PROGRESS.md)。

[09-30 Codegen 第三轮](REVIEW-2026-09-30-codegen-03.md)：[RR-04 Entity 修复](../bugfix/RR-20260930-04.md)、[RR-05 Nest 修复](../bugfix/RR-20260930-05.md)按声明场景通过；主生成器入口盘点及五个隔离 CLI 新反例见 [RR-06～10](../bug/REVIEW-2026-09-30-codegen-03.md)，未修。[实现机制](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md) · [证据](evidence/codegen-review-20260930-03/README.md) · [进度](PROGRESS.md)。整体 Codegen 尚未收敛。
[09-30 问题存档](ARCHIVE-2026-09-30.md)：v1.17.0 之后两条工作线逐编号状态、OPEN-ITEMS / REMAINING 去向、仍开放事项、证据索引与不一致清单。存档时 B 线的 RR-20260930-04/05 尚未修复；当前修复状态以上方第三轮记录为准。C01 24 小时长稳改由 fable 执行：[C01-RUNBOOK](C01-RUNBOOK-2026-09-30.md)。

[09-30 Codegen 第二轮](REVIEW-2026-09-30-codegen-02.md)：[RR-01/02 原触发修复](../bugfix/RR-20260930-01.md)并核对 Protocol 上层提交；[新 RR-04/05 Entity/Nest 退役旧文件](../bug/REVIEW-2026-09-30-codegen-02.md)只记录未实施。[Protocol 修复](../bugfix/RR-20260930-02.md) · [复跑](evidence/codegen-review-20260930-02/README.md) · [机制更新](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md) · [进度](PROGRESS.md)。

[09-30 Codegen 第一轮](REVIEW-2026-09-30-codegen-01.md)：当前生成器在 Core `codegen/`；审查暂存/提交、servicerpc、protocol 与 DAO 退役对照。确认两个未修 P2：RPC 标记删除后 `-check` 假通过、协议定义清空后旧产物保留。[问题/实施交接](../bug/REVIEW-2026-09-30-codegen-01.md) · [机制学习](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md) · [复现](evidence/codegen-review-20260930-01/README.md) · [进度](PROGRESS.md)。

[09-30 Service 第十三轮真实代理/HA/积压/热点](REVIEW-2026-09-30-services-13.md)：原 Toxiproxy 3 项从 skip 变成真实代理下 race/count2 6 pass；Session/Match 写后丢回复、六 owner 无 WAIT 故障接管、Redis 游标跨进程、Match 四客户端一分钟样本均有实际证据。无新增确认生产 bug，外部资源与跨机器/长稳继续待验。[机制](IMPLEMENTATION-SERVICE-FAULT-AND-CONTENTION.md) · [复跑](evidence/service-review-20260930-02/README.md) · [进度](PROGRESS.md)。

[09-30 Service 第十二轮故障/容量专项](REVIEW-2026-09-30-services-12.md)：跨进程强杀恢复、3主3从 Redis 单 master 切换、250 owner 积压轮转、Match 历史 64～16384 与短时热点样本已在隔离本机实测；无新增确认生产 bug。Toxiproxy 3 项仍 skip，真实外部资源/跨机器 HA/长稳仍待验。[机制学习](IMPLEMENTATION-SERVICE-CRASH-HA-AND-CAPACITY.md) · [复跑](evidence/service-review-20260930-01/README.md) · [进度](PROGRESS.md)。

[09-29第九批bugfix/第十一轮完成](REVIEW-2026-09-29-services-11.md)：RR-34已修/声明场景已验，后台维护与关闭邻接范围无新确认缺陷；**Service本阶段10/10域主链完成**，100生产路径内容不变复用，RR-01～34逐批台账关闭。19包951pass叶子/3Toxiproxy skip分列；[当前矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [机制学习](IMPLEMENTATION-SERVICE-PIPELINE-AND-REVIEW-CLOSURE.md) · [复跑](../bugfix/evidence/service-bugfix-20260929-09/README.md)。下方未修是历史，设计实施/真实环境事项不冒称已验。

[09-29第八批bugfix/第十轮](REVIEW-2026-09-29-services-10.md)：[RR-33修复验证](../bugfix/RR-20260929-33.md)，正式Mod无tag跨3owner分页；新增[RR-34 Pipeline首缺失掩盖写错误](../bug/REVIEW-2026-09-29-services-10.md)，真实两后端反例、仅交接。[机制](IMPLEMENTATION-SERVICE-MAIL-BATCH-AND-PIPELINE-ERRORS.md) · [进度](PROGRESS.md) · [复跑](evidence/service-review-20260929-10/README.md)。10域主链有界整理保持完成；100路径97复用/3变化，不冒称无bug。


[09-29 第七批bugfix与第九轮专项收口](REVIEW-2026-09-29-services-09.md)：RR-31/32 **2/2修复验证**；新增[RR-33 Mail Cluster分页](../bug/REVIEW-2026-09-29-services-09.md)未修。10域主链、100路径核算及具名本机专项完成有界整理；外部资源/HA/长稳另算。[机制/归档方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md) · [完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [复跑](evidence/service-review-20260929-09/README.md) · [修复](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md)。

[09-29 第六批 bugfix + 第八轮 review](REVIEW-2026-09-29-services-08.md)：RR-28/29/30 **3/3 修复验证**，原15/15转绿；正式定向43叶子、完整17包/826事件通过。继续发现 [Activity Cluster dispatch、购买 catalog 重试覆盖两项新 P2](../bug/REVIEW-2026-09-29-services-08.md)，仅记录未实施。[修复兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md) · [复跑](evidence/service-review-20260929-08/README.md) · [机制](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md#第六批修复后的实现与第八轮学习) · [进度](PROGRESS.md)。下方“未修”保留历史时点。

[09-29 Service 第七轮](REVIEW-2026-09-29-services-07.md)：维护竞争与 Cluster 专项，新增 [RR-28 P1、RR-29/30 P2](../bug/REVIEW-2026-09-29-services-07.md)，均未修；真实 Redis/三 master Cluster 15 叶子中 9 正常控制通过、6 反例失败，四包268既有事件通过。[机制学习](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md) · [复跑](evidence/service-review-20260929-07/README.md) · [最新矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)。原第五批4/4仍已关闭，HA/强杀/真实资金/容量未全验。

[09-29 Service 第六轮](REVIEW-2026-09-29-services-06.md)：第五批 4/4 缺陷已修并验证，修后邻接范围无新增确认问题；[最新完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)、[实际机制](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md#第五批修复后的实际实现)。10 域主链收口，容量/真实资源/HA/强杀/旧数据等专项未完全验证。[复跑与清单](evidence/service-review-20260929-06/README.md)。

[09-29 Service 第五轮与阶段收口](REVIEW-2026-09-29-services-05.md)：源码 b336ce62；[10 域完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)、100 路径逐项归类；[RR-25/26/27 与旧活动残余](../bug/REVIEW-2026-09-29-services-05.md)均未实施。新 42 次叶子执行，18 控制通过、24 反例预期失败、无 skip/build-fail；12 次生成检查通过。[学习](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md) · [复跑](evidence/service-review-20260929-05/README.md)。真实 allocator/HA/强杀/性能留专项，不计为完成。

[09-29 Service 第四批实施](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md)：RR-23/24 与未领取删除残余已修；提交 b336ce62 已推送，801 测试及子测试回归通过，未发版。

[09-29 Service 第四轮](REVIEW-2026-09-29-services-04.md)：基线 `bdbb61bc`，Platform 外部未知结果/回包所有权、Mail 删除终态、Match 提交恢复和 Session 资源链；2 个新增 RR、1 个旧问题删除残余。[问题](../bug/REVIEW-2026-09-29-services-04.md) · [学习](IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md) · [复跑](evidence/service-review-20260929-04/README.md) · [进度](PROGRESS.md)。现有 794 事件全过，新 16 个叶子执行保留 7 个失败反例；只改文档。

[09-29 Service 第三轮实施](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)：RR-19～22 的原触发已修复、正式回归/Redis/生成消费者通过，794 测试事件、16 包通过；[最新机制](IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md#本批修复后的实际实现)。旧数据与外部系统仍有明确边界，下方保留 review 时点。

[09-29 Service 第三轮](REVIEW-2026-09-29-services-03.md)：基线 `83c04243`；新增 4 个确认问题（3 P2、1 P3）：建角未知提交、旧取消解除新预约、Directory 删除重建 ABA、Memory Profile 输出别名。[问题与实施交接](../bug/REVIEW-2026-09-29-services-03.md) · [复跑附件](evidence/service-review-20260929-03/README.md) · [实现学习](IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md) · [进度](PROGRESS.md)。16 包已有回归通过；新增 25 个叶子场景已执行，未修生产代码。

[09-29 Service 修复与接手](../bugfix/SERVICE-BUGFIX-2026-09-29.md)：两轮 18 项新问题 + 旧正常 Finish ABA 已实施；正式行为回归、Memory/真实 Redis、编译和生成检查完成。旧数据迁移、外部系统对账与未测窗口单独保留，未发版。修复流程见 [roost-bugfix](../agent-skills/roost-bugfix/SKILL.md)。下方为修复前历史 review。

[09-29 Service 第二轮](REVIEW-2026-09-29-services-02.md)：基线 `e10dd3f1`，身份编码、迁移 lease、Attach 管理字段、提交未知和失败恢复；8 个新增已复现问题（6 P2、2 P3），另补旧 ABA 的真实 Redis 新触发，1 项完成重叠仅列观察。7 包原有 race/integration 全过，425 个测试/子测试无 skip。[问题交接](../bug/REVIEW-2026-09-29-services-02.md) · [复跑](../bug/REPRO-2026-09-29-services-02.md) · [机制](IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md)。本轮索引调用超时，源码补证；仅文档。

[09-29 Core 全 service 域审查](REVIEW-2026-09-29-services.md)：最新 `6b73289c`，10 个服务域主链完成；9 项动态反例 + 1 项源码接线缺口（2 P1、7 P2、1 P3）。原有服务 race/真实 Redis 集成与 12 次生成一致性检查通过，不代表全部故障交错已验证。[问题交接](../bug/REVIEW-2026-09-29-services.md) · [复现](../bug/REPRO-2026-09-29-services.md) · [机制与实施方案](IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md) · [逐路径证据](evidence/service-review-20260929/inventory.csv)。只改文档。

[09-29 B30 复测与调查](REVIEW-2026-09-29-b30.md)：正式装配 80 TPS × 30 分钟在写许可 128 下失败，非回归，是 Mongo 停顿超过许可余量；256 时通过（单样本）；隔离环境 mongo-3 数据文件缺失待处理。

[09-28 v1.17.2 之后剩余项](REMAINING-2026-09-28.md)：发版后的结论清单——OPEN-ITEMS 93 条中已完成 73、已接受 / 保留 14、剩余 6（长稳、性能对照、Remote 容量、端到端补测、Linux fsync、图谱刷新），另从记录与审计整理 48 条 N 项；发版后 framework-compat 有一处生成 TCP 测试偶发（N70）。

[09-28 第八轮独立审计](REVIEW-2026-09-28-audit8.md)：`d66eeeb` 上 RR-20260928-07～11 红绿成立；登记 RR-20260928-12、13（P4）与文档疑点。

[09-28 第七轮独立审计](REVIEW-2026-09-28-audit7.md)：`611dab1` 上 RR-20260927-29～35、RR-20260928-01～06 红绿成立（RR-20260928-03 部分成立）；登记 RR-20260928-08。

[09-27 第六轮独立审计](REVIEW-2026-09-27-audit6.md)：`001b03b` 上 RR-20260927-01～28 红绿成立（25、27 部分成立）；登记 RR-20260927-29～32，直接更正三处文档。

[09-27 第五轮独立审计](REVIEW-2026-09-27-audit5.md)：v1.17.1 中 RR-81～85、RR-73/80 补修红绿成立；登记 RR-20260927-21，更正 CHANGELOG 一处误写。

[09-27 未完成 / 未验证条目清单](OPEN-ITEMS-2026-09-27.md)：v1.17.1 之后全部残留去重归档（A 缺陷 13、B 待验证 38、C 需决定 / 本地做不了 33、D 已接受 39、E 可关闭 27 组），附处理计划与进度。

[09-27 第四轮修复后独立审计](REVIEW-2026-09-27-audit4.md)：`8d06d80` 上 RR-73～80 红绿成立（75、79 部分成立）；登记 RR-20260926-84、85，残留追加在原 RR。

[09-27 第三轮修复后独立审计](REVIEW-2026-09-27-audit3.md)：`736bdb2` 上 RR-64～72 红绿全部成立；登记 RR-20260926-73～80。

[09-27 第二轮修复后独立审计](REVIEW-2026-09-27-audit2.md)：`7706419` 上 RR-48～63 红绿全部成立；登记 RR-20260926-64～72。

[09-26 修复合并后独立审计](REVIEW-2026-09-26-audit.md)：`2330d57` 上 RR-30、33～47 三路独立红绿审计全部成立（RR-42 部分成立）；登记 RR-20260926-48～63。

[09-26 v1.17.0 疑点核实](REVIEW-2026-09-26-v1170-triage.md)：此前全部“疑点”逐条核实，登记 RR-20260926-33～47（8 条 P2、7 条 P3），9 条判为非问题，附性能对照。

[09-26 修复复验](REVIEW-2026-09-26-fix-verification.md)：`985d5ba` 对 `aaada47`，红绿对照 + 门禁 + 故障矩阵 20/21；登记 RR-20260926-25～32（1 条 P1、7 条 P2）与 11 条复核残留，结论仍不能发布。

[09-26 新增复审修复](REVIEW-2026-09-26-release-fixes.md)：RR-10～24 的修复、10 项负对照、正式链路、兼容变化和未验证边界。

[09-26 上线前复审](REVIEW-2026-09-26-release.md)：main `aaada47` 对 v1.16.1，发布门禁 + Nest/Sync/DataEngine/Remote/demo 五路；登记 RR-20260926-10～24（3 条 P1 级、12 条 P2，原审查时未修复；当前见后续修复记录），附[复现](../bug/REPRO-2026-09-26-02.md)；结论：不能直接发布。

[2026-09-26 三模块复审核实与修复](REVIEW-2026-09-26-followup.md)：RR-03～09、负对照、正式链路验证与规则补强。


[09-26 核心优化复审](REVIEW-2026-09-26-core-optimization.md)：main `a22c5a5`，Nest / Sync / DataEngine+Remote 三路只读审查；登记 RR-20260926-03～06（均 P2、未修复），附[复现](../bug/REPRO-2026-09-26.md)；观测与文档类疑点列在记录内。

[09-25 Sync 四项最终收尾](../feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md#最终实现与未采纳的实验)：冷创建客户端观测及 RR-13 窗口漂移修复完成；11 包 race、静态检查、生成工程和离线统计回归通过。正常 1%/5% p99 2.342/3.857ms，原门禁通过；集中恢复仍有超标及历史单次 FlushFailures 记录，未宣称严格 50ms 全部验收通过。1000 个客户端当前 AOI 集合诊断核验通过，完成上界 3.601 秒；小窗口实验无收益已撤回，未增加配置。

[09-25 Sync 长尾修复 RR-12](../bugfix/RR-20260925-12.md)：现有对象 Full 更新不再与冷创建共用恢复额度；定向修前红/修后绿、11 包 race/vet、glsvet 和正式生成工程通过。追加四轮集中恢复，1% 两轮通过，5% 一轮通过、一轮冷创建最大 54.283ms；没有再观测到数百毫秒长尾，仍不宣称严格 50ms 全部达标。详见下方报告末节。

[09-25 main 上的 Sync 后续评估](../feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md)：三个开发分支已包含在 main；已实施有界阶段追踪、独立快照等待与 Profile 需求缓存；11 包 race/vet、生成工程及 12 轮正式负载完成。正常 1%/5% 四轮严格门禁通过，集中恢复八轮仍有全量长尾，保留失败记录。

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
