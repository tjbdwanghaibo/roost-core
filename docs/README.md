# Roost 文档中心

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

Roost 是面向 Linux 生产环境的通用 Go 游戏服务器框架。运行时由 `roost-core`（契约 + 实现 + 技能系统）与 `roost-kit`（装配层 + 通用服务）组成，项目与样板代码由 `roost-codegen` 生成。文档按阅读者的目标分为三级，没必要从头读到尾。

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
- [roost-kit 组件与实现](https://github.com/tjbdwanghaibo/roost-kit/blob/main/README.md)
- [技能系统（core/skill）](skill/README.md)
- [通用服务（kit/service）](https://github.com/tjbdwanghaibo/roost-kit/blob/main/service/README.md)
- [roost-codegen 生成器](https://github.com/tjbdwanghaibo/roost-codegen/blob/main/README.md)

## 版本基线

当前正式 tag 组合为：`roost-core v1.15.8`、`roost-kit v1.14.10`、`roost-codegen v1.15.15`（2026-09-19 第十二次发版：codegen 收敛 09-19 审查的四条 P1（会话关闭订阅者 panic 带走进程、顶层单指针 nested 字段的所有权交接、待发货索引一条坏订单饿死整页、付费 grant 过期删除造成永久少发货），外加 handler 空白参数名与 chat presence 残余补修，以及 game-demo 第十六批（限时活动：World 的持久计时器 + global/activity 跨服聚合）。**RR-20260919-02（nested 别名）与 RR-20260919-04（订单与待办索引不原子）本版未修**，理由与要先定的契约见 `docs/bugfix/README.md` 末尾；2026-09-19 第十一次发版：kit 给 platform 的协作者加 `RegistryBound`（与 account 同形：collaborator 在 app 之前构造，拿不到 registry，而 U-0234 留给部署的待发货索引需要同一个 Redis 并要把订单读回来判终态），codegen 是 game-demo 第十五批——付费订单走通 game / platform 两个进程、待发货索引的参考实现，外加托管服务可选协作者的生成接线与 platform 配置块缺失的两个 secret；2026-09-19 第十次发版：codegen 补丁——U-0245（新建的 DAO 不接嵌套回调，第一次存盘前的嵌套写入悄悄丢掉，数据丢失类）与 game-demo 第十四批（装备栏 / 数据版本迁移 / `schema=N`）；2026-09-18 第九次发版：一轮 bugfix 收敛 review 第三轮登记的全部六条——邮件账本按信封过期时刻而不是固定保留期（P1）、运行期实体 id 带进程 sid（P1）、顶层 DAO 容器的回调所有权（P1）、会话关闭生命周期事件、syncTopic 歧义写法改为拒绝、AOI 单观察者订阅预算；另含 ARCH-06 同步字段词汇表与 game-demo 第十三批（地图 / AOI / 刷怪）。**codegen v1.15.11 有一个启动缺陷**（spawner 从错误的位置读 sid，启用 game-demo 的工程起不来），v1.15.12 修掉，不要用 v1.15.11；2026-09-18 第八次发版：一轮 bugfix 收敛 review 登记的全部八条——attribute 运行时进 core 并让 attribute feature 真正可用（B8 随之完成）、saga 原生完成消费者、room 持久化水位入口、platform 后台重试候选入口、dungeon 奖励幂等（P1）、battle 宽限期、sync=true 生成物、嵌套 DAO 脏传播；2026-09-17 第七次 v1.15.6 / v1.14.7 / v1.15.9：saga 步骤拒绝 / 重试用尽后的补偿能落到 Mongo 存储（U-0225）、U-0219～U-0224 一并随本版发出，codegen 侧是 game-demo 的送礼 saga / GM 运维面 / `add mod` 补配置段；2026-09-16 第六次 v1.15.5 / v1.14.6 / v1.15.8：Mail / Session / Matchmaker RPC 接口与传输半进 core，kit 从 core 接口生成装配半；同日第五次 v1.15.4 / v1.14.5 / v1.15.7：mail / session / manager 下沉 core、生成传输拆两半；v1.15.3 / v1.14.4 / v1.15.6 同日稍早，v1.15.2 / v1.14.3 / v1.15.4 于 2026-09-09；roost-skill / roost-service 已于 2026-09-08 并入 core / kit 并归档）。kit v1.14.4 含破坏性改动 `match.NewMod(reporter)`，须与 codegen v1.15.5 同版本升级。发布顺序固定为 core → kit → codegen，后一层只能依赖前一层已存在的正式 tag；roost-codegen 的 `ci/framework-release.yaml` 是这组版本的机器可校验记录。正式项目不得依赖 `@latest`、伪版本或本地 `replace`。
