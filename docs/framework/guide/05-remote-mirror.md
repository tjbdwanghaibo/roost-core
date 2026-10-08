# 05 Remote Entity 与 Mirror 说明

**2026-10-08现行契约更新：** 下方按v1.23.0行号描述保留历史。当前投影只持久Applied并唤醒outbox，finalizer只等待确认与释放；`RecoverOutbox`是唯一发布协调者，页内按全部Entity依赖有界并行（`outbox_publish_workers`默认8、上限64），失败保留Applied，停止排空在途。`ApplyRemoteCommits`成功不承诺发布，需`FlushRemoteTransaction`确认。兴趣广播改为有界后台队列，不占ReadSnapshot预算。已被本轮实现取代的“投影/ finalizer直接发布”及发布阻塞疑点不再代表当前逻辑。详见[正式契约](../../../REMOTE_ENTITY.md)、[方案](../../feature/REFACTOR-2026-10-08-outbox-single-publisher.md)与[RR-47](../../bugfix/RR-20261008-47.md)。


> 本篇是框架整体文档 05 分区的**说明文档**，面向业务作者与运维。实现细节、不变量强制点和 review 检查点见 [实现文档](../impl/05-remote-mirror.md)。
>
> 源码基准：tag `v1.23.0`（`28912cd6`）。文中 `path:line` 都按这个 tag。本篇没有依赖 codebase-memory 图谱（图谱可能落后于 tag），结论全部按 tag 源码直接读取；标“推断 / 未验证”的地方没有用测试或探针证实。

## 速览

- Remote 实体是**跨进程共享、所有权可以迁移**的实体：写只发生在当前写者（owner 或共享锁持有者）上，经 Nest 事务与 DataEngine WAL 进入 Mongo；别的进程只读它的**不可变快照**（进程内 L1、Redis 共享 L2、Mongo 权威三层）。只读服务经 Mirror 只拿到“按完整 key 读快照”这一项能力。
- 最重要的保证：写许可的权威在 Mongo（Redis 只协调竞争），每笔提交在同一个 Mongo 事务里校验四维版本向量（StateVersion 严格 `base+1`，MarkerEpoch / RouteEpoch / LockFence 只前进）；读出口交出的快照一定满足完整 key、未过期、不早于调用方给的观察 token，非线性读还要在 `cached_max_staleness` 内被 L2 或权威确认过。结果不确定时一律带 `entity.ErrRemotePersistenceIndeterminate`，框架不猜、不回滚已可能提交的写。
- 最容易踩的坑：把 `Cached` 读当成最新值（L2 落后时上界是 `snapshot_l2_ttl + cached_max_staleness`）；以为普通 NATS 上也有快照推送（只有 JetStream 有）；兴趣配额满的 key 没有推送、只能按需读；把 bus 的“可靠消息”当成至少一次投递（它是 core NATS 至多一次 + 消费端去重 + 死信）；JetStream RPC 的 handler 不幂等（broker 会重投）；同一进程同时装 `RemoteEntityMod` 与 `RemoteMirrorMod`（启动即报能力冲突）。

本篇覆盖的包：

| 包路径 | 职责 |
| --- | --- |
| `remoteentity/` | 写管理器（准入、批次、finalizer、所有权）、Mongo 权威提交 `MongoCommitter`、版本锁、共享 L2、`SnapshotClient`（读 / 兴趣 / 复制接收）、装配 `Assemble` |
| `entity/`（Remote 部分：`remote_protocol.go`、`remote_manager.go`、`remote_snapshot*.go`、`remote_mirror.go`、`remote_view.go`、`entity_remote.go`） | 契约：`RemoteCommit`、四维版本向量、所有权状态机、批次接口；快照缓存 `RemoteSnapshotCache`（L1 + `admitLocked`）；只读契约与 DTO reader |
| `kit/remoteentity/` | `RemoteEntityMod`（owner 进程）、`RemoteMirrorMod`（只读服务）、`remote_entity.*` 配置声明、`MirrorSource` |
| `sync/syncbus/mirror/` | 通用复制器 `Replicator`：按 key / 版本把 envelope 发到同步总线、订阅后交给 `Store` |
| `ownerroute/` | 按 owner sid 分派命令的泛型路由器（本机执行或经 bus 转发） |
| `bus/` | 服务间消息与 RPC：模块消息、可靠消费（SETNX 去重 + 死信）、轻量 RPC、JetStream 持久 RPC、method 标签上界 |
| `nats/`、`nats/driver/` | NATS 契约与驱动：连接、JetStream、request-reply RPC；驱动自持唯一的“已关闭”状态 |
| `kit/nats/` | `NatsMod`：读 `nats.*`、装配驱动与 bus、可靠存储、DLQ 运维命令 |
| `codegen/internal/entity/`（`mirror.go`） | `//roost:mirror` 只读 DTO 的生成器 |

---

## 1. 定位与边界

**一句话**：Remote 分区让一个实体能被多个进程按版本安全地写（同一时刻只有一个合法写者）并被任意进程按有界陈旧的快照读；bus / nats 是它和其他服务共用的服务间通信层。

| 负责 | 不负责 |
| --- | --- |
| Remote 写准入：写门、所有权读锁、共享模式的 Redis 版本锁、Mongo 持久写许可 | 实体锁、调度、提交点与回复判别表（02 nest） |
| 冻结 `RemoteCommit`，在 Mongo 同一事务里 CAS 元数据、写 DAO、快照与 outbox 回执 | WAL、投影器、ack checkpoint（03 dataengine，本分区只是它的 Remote 适配器） |
| 提交后发布：L2 CAS、L1 准入、按兴趣推送；finalizer 按持久结论收尾 | 客户端同步与同步总线本身（04 sync：`syncbus`、JetStream 驱动、`Subscription` 排空） |
| 快照读（Cached / Monotonic / Linearizable）、L2 水位、首载缓冲、兴趣代际与配额 | 跨 database 的多步业务一致性（06 saga） |
| Mirror：只读契约、`SnapshotClient`、`RemoteMirrorMod`、`//roost:mirror` DTO | 生成器与 CLI 的整体（12 codegen） |
| ownerroute、bus（消息 / RPC / 可靠消费）、nats 驱动 | 指标面板、告警规则（11 可观测） |

跨分区引用：App 生命周期、单实例锁与停机预算见 [01 app 与生命周期](./01-app-lifecycle.md)；Nest 事务、Remote 批次在消息生命周期里的位置见 [02 nest 调度与实体](./02-nest-entity.md)；WAL 投影与 Remote 投影窗口见 [03 dataengine](./03-dataengine.md)；同步总线见 [04 sync 分区](04-sync.md)；配置严格读取见 [07 配置](07-config.md)；生成器见 [12 分区](12-codegen.md)。

[↑ 速览](#速览) · [实现文档 §1](../impl/05-remote-mirror.md#1-包与文件地图)

---

## 2. 核心概念与术语

### 2.1 三种角色

| 角色 | 装配 | 能做什么 |
| --- | --- | --- |
| owner 进程 | `kitremote.NewRemoteEntityMod(sid, WithMongoStorage(loader))` + DataEngine Mod | 写（Nest handler 声明 Remote 目标）、读、所有权迁移；提交后发布快照 |
| 同进程读者 | 不另装 Mod：`kitremote.MirrorSource(registry)` 拿到 owner 的 `SnapshotClient` | 只读 |
| 只读服务 | `kitremote.NewRemoteMirrorMod(sid)` | 只读；没有写 Manager、锁、finalizer，不需要 Mongo 原子 backend |

两个 Mod 都把只读能力登记在同一个名字 `mods.ModRemoteMirror`（`"remote_entity.mirror"`，`kit/mods/name.go:38`），所以读 DTO 的代码在两处完全一样；同一进程只能有一个（`kit/remoteentity/remote_entity_mod.go:138`、`kit/remoteentity/remote_mirror_mod.go:147`）。

### 2.2 一次 Remote 写经过的阶段

| 阶段 | 在哪里 | 说明 |
| --- | --- | --- |
| ① 准入 | Nest 慢池，`PrepareRemoteWriteBatch`（`remoteentity/batch.go:48`） | 取写额度、按 ID 排序逐个取写门、刷新所有权、（共享模式）取 Redis 锁、取 Mongo 写许可、必要时从权威加载实体 |
| ② 定稿 | 实体锁内，`FinalizeLocked`（`remoteentity/batch.go:338`） | 生成代码把本事务改过的 DAO 冻结成整文档 mutation 与快照，填入租约的四维向量 |
| ③ 持久 | 按持久模式：memory 直接写 Mongo；async / strict / pipelined 随 WAL 记录准入，由投影器写 Mongo | Mongo 同一事务里：元数据 CAS、DAO 整文档、快照文档、事务记录（状态 Applied） |
| ④ 发布 | 投影器或 finalizer 调 `ApplyRemoteCommits`（`remoteentity/transaction_manager.go:601`） | 确认活实体、L2 CAS + L1 准入、按兴趣推送；标记事务 Committed |
| ⑤ 收尾 | `Close`（`remoteentity/batch.go:628`）或 finalizer | 释放写门、所有权读锁、Redis 锁（按新版本写回版本缓存）、写额度；拿到持久结论后执行提交后工作 |

### 2.3 术语表

| 术语 | 含义 | 出处 |
| --- | --- | --- |
| Managed Remote 实体 | `//roost:entity ... remote=managed` 生成的实体，kind 注册为 `RemotePolicyManaged` | `codegen/internal/entity/testdata/remote/guild.go` |
| `RemoteCommit` | 一次事务里一个实体的冻结提交：Base / Next 版本、三个代际、整文档 mutation、删除目标、快照、失效 key | `entity/remote_protocol.go:234` |
| 四维版本向量 | `StateVersion`（内容版本）、`MarkerEpoch`（所有权代）、`LockFence`（写许可代）、`RouteEpoch`（路由代） | `entity/remote_protocol.go:90` |
| owner-routed / shared | 两种写模式：独占 owner 直接写；共享模式先取 Redis 版本锁再取 Mongo 许可 | `entity/remote_protocol.go:107`、`remoteentity/batch.go:156-200` |
| 写许可（WriteGrant） | Mongo `_remote_entity_meta` 上一次 FindAndModify：递增 `_grant_fence`、写一次性 token、返回当前版本 | `remoteentity/mongo_authority.go:96` |
| 所有权状态 | `local_owned / sharing / shared / draining / fenced / recovering / quarantined`，转换受状态表约束 | `entity/remote_protocol.go:30-88` |
| 隔离（quarantine） | 持久结论没拿到或被拒绝时把活实体冻住，直到重新加载 | `remoteentity/transaction_manager.go:912` |
| finalizer | 后台收尾 worker：只按持久结论（Committed / Rejected）释放资源与执行提交后工作 | `remoteentity/transaction_manager.go:188-313` |
| L1 / L2 / 权威 | 进程内有界缓存 / Redis 共享快照层（水位权威）/ Mongo `_remote_entity_snapshots` | `entity/remote_snapshot.go:127-150` |
| 确认时刻 `confirmedAt` | L2 或权威最近一次担保“没有更新的版本或删除”的时刻；0 = 未确认 | `entity/remote_snapshot.go:212-219` |
| `cached_max_staleness` | 非线性读能交出的条目距确认时刻的上限 | `remoteentity/config.go:40-44` |
| 墓碑 | L2 上带版本删除留下的 `deleted_version`，挡住不新于它的快照写 | `remoteentity/snapshot_l2.go:88-101` |
| 观察 token | `RemoteObservation{MarkerEpoch, RouteEpoch, StateVersion}`，读结果带出、可作下一次读的下限 | `entity/remote_mirror.go:37` |
| 兴趣（interest） | consumer 对某个 key 的软状态租约，owner 只推送有兴趣的 key | `entity/remote_protocol.go:509`、`remoteentity/interest.go` |
| 代际（generation） | 同一 consumer 对同一 key 的 renew / release 的单调序号；release 留撤销水位 | `remoteentity/interest.go:30-44` |
| 溢出水位 | 兴趣表满放不下撤销水位时，每个 consumer 一个的“代际上限 + key 指纹位图” | `remoteentity/interest.go:85-129` |
| 首载缓冲 | 某 key 权威加载在途时到达的复制消息先缓冲、加载后按序重放 | `entity/remote_snapshot.go:194-200` |
| 推送（push） | 快照复制消息经可确认订阅（JetStream DeliverNew）送到 consumer | `remoteentity/snapshot_client.go:444-501` |
| Mirror DTO | `//roost:mirror` 标记的普通 struct，生成 spec / 解码 / reader | `codegen/internal/entity/mirror.go:19-32` |
| 可靠消息（bus） | 带 MsgID 的模块消息：消费端 SETNX 去重、失败进死信；投递本身仍是 core NATS | `bus/reliable.go:52-69` |
| 持久 RPC | `CallReliable`：请求与响应都经 JetStream 流，broker 按 AckWait 重投 | `bus/jetstream_rpc.go:320` |

[↑ 速览](#速览) · [实现文档 §2](../impl/05-remote-mirror.md#2-关键类型与数据结构)

---

## 3. 设计原因

### 3.1 写权威在 Mongo，Redis 只协调

Redis 异步复制，切主可能丢掉锁或 fence 计数，所以正式写许可放在 Mongo majority 写的 `_remote_entity_meta` 上：每次写取一次许可（`GrantWrite`，一次 FindAndModify 递增 `_grant_fence`），提交时在同一 Mongo 事务里按 `_ver = BaseVersion`、三个代际与许可 fence 做 CAS（`remoteentity/mongo_committer.go:414-438`）。Redis 版本锁只用来避免多个进程同时进入临界区（`remoteentity/doc.go:9-11`）。旧的弱校验开关与迁移入口已删除，不支持的元数据拒绝启动（[RR-20260925-02](../../bugfix/RR-20260925-02.md)、[REMOTE-AUTHORITY](../../feature/REMOTE-AUTHORITY-2026-09-25.md)）。

### 3.2 一条写链路：随 WAL 进 Mongo，发布与持久分开确认

Remote 提交是 DataEngine WAL 记录里的一个 mutation（`nest/msg.go:55-66`），投影器在写普通 DAO 的同一个 Mongo 事务里调 `ApplyRemoteCommitsInTransaction`（`dataengine/engine/mongo_projection.go:117-124`）；事务提交后再做发布（`:156-160`）。Mongo 里的事务记录先是 Applied（未发布），发布成功后标 Committed（`remoteentity/mongo_committer.go:142`、`:311-327`），未发布的记录不过期，启动时 `RecoverOutbox` 先补发（`remoteentity/assemble.go:192-197`）。这样“已持久”与“已发布”是两个可分别恢复的事实。

### 3.3 结果未知交给持久结论，不猜

Remote 写可能卡在任何一步：WAL 已准入但投影未到、Mongo 已提交但发布失败、回复丢失。框架的规则是：没有持久结论就不释放写门、不执行提交后工作；finalizer 回源 `CommitStatus` 拿结论（`remoteentity/transaction_manager.go:253-313`）。memory 模式没有 WAL，回源读不到时用同一事务 `_id` 写一条持久拒绝，由唯一索引在“拒绝”与“迟到提交”之间裁决（`:448-471`、`remoteentity/mongo_committer.go:480-507`）。

### 3.4 L2 是快照水位的唯一权威（维护者第三轮 B2）

> B2 | 按推荐：共享 L2 为快照水位权威，L1 只是有界副本；Cached 读最大陈旧时间写成配置与契约

（出处：`docs/review/DECISIONS-PENDING-2026-10-05.md` 第三轮。）之前“谁是最新版本 / 是否已删除”散在五处各补一个入口；现在新值一律先在 L2 上做版本 CAS（删除走带版本删除留墓碑），L1 只记 L2 接受或已持有的值并带确认时刻（`entity/remote_snapshot.go:127-146`、`:942-961`）。L2 不可用时不拒绝缓存（Cached 读不回源，拒绝缓存等于 L2 一断就全部未找到），而是记为未确认、读时重新确认（[B2 方案](../../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)）。

### 3.5 推送只在能确认订阅的总线上开（Mirror 第 4 步）

> Mirror 第 4 步 | 按推荐：推送订阅依赖 JetStream，`sync/syncbus/driver` 按主题加 DeliverNew 消费，保证确认订阅之后的发布不被静默丢掉；没开 JetStream 时退化为按需读取（Cached 按 `cached_max_staleness` 回源），显式检测并记日志；……所有缓存写入仍只经 `admitLocked`

（出处：DECISIONS-PENDING 第九轮。）普通 NATS 是至多一次，推送可能静默丢失，所以干脆不订阅快照主题，正确性只靠陈旧上限（`remoteentity/snapshot_client.go:444-501`）。

### 3.6 兴趣容量按 consumer 配额（O4）

> O4 | 按推荐：兴趣容量按节点计数（每个 consumer 节点各有配额），满了明确拒绝、可识别错误、日志与指标，续期失败时消费方感知并退化为按需读取；配额作为配置项，A4 严格读取并登记

（出处：DECISIONS-PENDING 第九轮。）兴趣是广播软状态，每个节点存全集群的租约；按 consumer 计配额让一个 consumer 用满不影响别人，而且判定只取决于该 consumer 自己的消息，它在本地就能预知被拒（`remoteentity/interest.go:61-66`）。

### 3.7 只读方只拿读能力（Mirror 第 1～3、5 步）

维护者原则（[MIRROR-STEPS-1-3](../../feature/MIRROR-STEPS-1-3-2026-10-06.md) §1）：“由底层统一兜住，结构简单易懂，使用方手写代码尽量少”。所以：只读能力是一个接口 `entity.RemoteSnapshotReadOnly`（`entity/remote_mirror.go:87`）；全部读出口共用 `RemoteSnapshotCache.Read` 与同一个 `Covers` 判定；Manager 的读 / 兴趣 / 复制接收提取成 `SnapshotClient`，owner 与只读服务共用一份协议；只读方接入是一个 DTO 加一行装配（[MIRROR-STEP-5](../../feature/MIRROR-STEP-5-2026-10-06.md)）。

### 3.8 Mirror 第 6 步本机观察的四项决定

| 编号 | 维护者决定（原文） | 现状 |
| --- | --- | --- |
| O-M6-1 | “按推荐：同 sid 重启的 owner 启动时广播‘请重新续租兴趣’，只读方收到后立即续租，推送不等 15s” | owner 启动后发 `remote_entity_interest_refresh`（`remoteentity/assemble.go:176-182`） |
| O-M6-3 | “按推荐：只对 L2 删除墓碑写入加 `WAIT`（等副本确认后返回），缩小切主时墓碑未复制导致的删除短暂复活窗口” | `remoteentity/snapshot_l2.go:324-351` |
| O-M6-5 | “保持；O-M6-5 owner 启动遇 Mongo 选举做有界重试” | `EnsureIndexes` 遇换主错误码最多 10 次、间隔 1s（`mongo/driver/collection.go:259-305`），属 03 分区驱动 |
| O-M6-6 | “按推荐：同 sid 新进程（已持 App 单实例锁）启动时立即接管上一代同 sid 进程留下的 Remote 实体锁（按锁记录的进程代际令牌判定，只接管‘同 sid、旧代际’）” | 取锁 Lua 里当场接管（`remoteentity/versioned_lock_lua.go:12-44`） |

（出处：DECISIONS-PENDING 第十～十二轮；观察原文见 [MIRROR-M6-OBSERVATIONS](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)。）O-M6-2（结果未知的既有契约）、O-M6-4（静默断线靠 ping 发现）维护者定为不改。

### 3.9 L2 落后于权威：保持，写明上界（第十二轮）

维护者原话：“B 类的都按照推荐即可，mongo 的延迟可以分析下”，本行推荐是“保持，写明上界”。owner 写 L2 失败时不加后台补写队列，上界见 §7.3。

### 3.10 bus 与 nats 的几条取舍

- **可靠消息只做去重和死信**（第十二轮“保持现状，写明契约”，契约在 `bus/reliable.go:55-69`）：没有带 token 的认领；handler 执行中崩溃留下的 `processing` 会让同 ID 的后续投递被当成重复，直到 InboxTTL。需要至少一次 + 幂等的业务用 saga 或 JetStream 持久 RPC。
- **RPC method 标签有界**（RR-20261006-19）：被调方只用本进程注册过的方法名，其余记 `_unregistered`；调用方每个 Bus 至多 256 个方法名，其余记 `_other`（`bus/jetstream_rpc.go:664-716`）。
- **nats 驱动自持唯一的“已关闭”状态**（第十三轮维护者方向调整，“按推荐 A”）：同一不变量“Close 之后都是已关闭”被修了三次之后，改为驱动自己的 `closed` 标志是唯一判据（`nats/driver/client.go:25-31`，[方案](../../feature/REFACTOR-2026-10-06-nats-driver-closed-state.md)）。

[↑ 速览](#速览) · [实现文档 §3](../impl/05-remote-mirror.md#3-主流程)

---

## 4. 怎么用

### 4.0 最小可运行示例

仓库里完整的两进程样例是生成工程夹具 `codegen/internal/entity/testdata/remoteflow/`：

| 文件 | 内容 |
| --- | --- |
| `guild.go` | owner 实体：`//roost:entity entityKind=EntityKindGuild remote=managed`，一个 DAO `remote_guilds` |
| `guild_summary.go` | 只读 DTO：`//roost:mirror entityKind=EntityKindGuild coll=remote_guilds`，只声明 `name`、`members` |
| `mirror_test.go` | `TestGeneratedRemoteMirrorGuildSummary`（`:287`）：owner 经正式链路提交，子进程只装 `SyncBusMod + RemoteMirrorMod` 经生成的 reader 读；JetStream 与普通 NATS 两种模式 |
| `vault.go`、`flow_test.go` | 两个 DAO 的 Managed 实体、四种持久模式的 Nest handler（`flow_test.go:311-331`）、真实 Mongo / Redis / NATS 端到端 |

生成的只读产物长这样（`codegen/internal/entity/testdata/remote/guild_summary_gen_wire.go`）：`GuildSummaryMirrorSpec`、`DecodeGuildSummary`、`NewGuildSummaryReader`。运行：

```bash
ROOST_REMOTE_RUN='^TestGeneratedRemoteMirror' scripts/test-remote-generated.sh   # 需要共享隔离环境，见 03 分区“测试与门禁”
```

### 4.1 声明 Managed Remote 实体

1. 注册 kind 时 `RemotePolicy: entity.RemotePolicyManaged`（`codegen/internal/entity/testdata/remoteflow/guild.go:9-11`）。
2. 实体嵌入 `*entity.RemoteEntityBase` 与 `entity.DaoManager`，DAO 用 `dao:"<coll>"` 标签；标记 `//roost:entity entityKind=... remote=managed`。
3. DAO 必须 `dbscope=global`（缺省）。按服选库的 DAO 在生成期、registry 校验、`Assemble` / `Start`、以及进 WAL 之前四处被同一个错误拒绝：`entity.ErrRemoteManagedServerScopedDAO`（`remoteentity/batch.go:412-432`，[RR-20260927-09](../../bugfix/RR-20260927-09.md)）。
4. 生成代码提供 `BuildRemoteCommitLocked`（整文档 + 全量快照，`codegen/internal/entity/gen.go:583`）、`AcknowledgeRemoteCommit`（幂等且并发安全，`:654`）、`RollbackRemoteCommit`（no-op，`:673`）。手写参与者必须自己满足“重复确认同一版本返回 nil、并发安全”（`entity/remote_protocol.go:386-395`）。
5. 应用只需要提供 loader（`entity.IRemoteEntityLoader`，最好同时实现本机查找 `IRemoteEntityLocalLookup` 与卸载 `IRemoteEntityUnloader`），交给 `kitremote.WithMongoStorage(loader)`（`kit/remoteentity/remote_entity_mod.go:46`）。不实现卸载时，被持久拒绝的实例只能隔离到业务自行重新加载。

### 4.2 在 Nest handler 里写

Remote 目标在消息的 `Tid / Tids / GroupTIds` 里出现时，Nest 在慢池里先 `PrepareRemoteWriteBatch`，handler 照常改 DAO，事务结束时定稿、提交（`nest/nest_dispatch.go:613-639`、`nest/msg.go:40-123`）。限制：

- 广播消息不能写 Remote 实体（`nest.ErrRemoteBroadcastUnsupported`）。
- 带 Remote 批次的消息里不能开嵌套独立事务（`nest.ErrNestedTransactionInRemoteMessage`）。
- 一批最多 `max_write_batch` 个实体；同时在途的写批次受 `max_concurrent_writes` 约束，满了直接 `ErrRemoteOverloaded`，不排队（`remoteentity/transaction_manager.go:127-139`）。
- 准入等写门、取锁、取许可、加载共用一个 `op_timeout` 预算（`remoteentity/batch.go:87-89`、`:115-130`）。

按持久模式的行为（`remoteentity/batch.go:462-534`，Durability 编号见 `nest/transaction.go:20-28`）：

| 持久模式 | Remote 部分何时进 Mongo | 回复何时发出 | 结果未知 / 拒绝时 |
| --- | --- | --- | --- |
| memory（0） | `Commit` 里同步 `ApplyRemoteCommits`（自己的 Mongo 事务） | Mongo 提交并发布之后 | 明确拒绝：同步回滚、隔离、`Close` 后卸载重载；未知：finalizer 回源，读不到就写持久拒绝裁决 |
| async（1） | WAL 准入后由投影器写 | WAL 准入之后（`Commit` 只给推测回执） | `Close` 把写门交给 finalizer，等投影器结论（≤ `finalize_projection_timeout`），之后回源 |
| strict（2） | 投影器写 | 投影器写 Mongo **并发布**之后 | 一律交 finalizer；回复带 `ErrRemotePersistenceIndeterminate`，只有明确拒绝不带 |
| pipelined（3） | 同 strict（带 Remote 批次的消息不走 pipelined 早释放，`nest/execution.go:184-188`） | 同 strict | 同 strict |

回复怎么判（细节见 [02 nest](./02-nest-entity.md) 的判别表）：

| 回复里的哨兵 | 意思 | 能否整笔重试 |
| --- | --- | --- |
| 无错误 | 本地与 Remote 都已按模式确认 | — |
| `entity.ErrRemotePersistenceIndeterminate` | Remote 可能已提交 | 否：按业务幂等键读回再决定 |
| `nest.ErrRemotePartRejected` | 本地已提交、Remote 明确没写 | 否：只补做 Remote 部分 |
| `entity.ErrRemoteEntityReloading` | 上一次被拒绝的实例正在卸载重载 | 可以（它包裹 `ErrRemoteFenced`） |
| `entity.ErrRemoteFenced` / `ErrRemoteOwnerTransition` | 本进程不是合法写者 / 所有权迁移中 | 视业务：通常转发给 owner（见 §4.6） |
| `entity.ErrRemoteOverloaded` | 写额度、批次上限、wrapper 容量 | 可以（退避） |

提交后工作（Sync Confirm、AfterCommit）只在拿到持久结论后在 Nest 快池执行一次；Nest 已停机或 fence 时不执行，计 `remote_entity.deferred_outcome_not_run_total`（`remoteentity/transaction_manager.go:338-366`）。

### 4.3 所有权操作

`IRemoteEntityManager` 上的五个方法（`entity/remote_protocol.go:499-505`、`remoteentity/ownership.go`）：

| 方法 | 作用 | 失败时 |
| --- | --- | --- |
| `GetRemoteOwnership` | 读权威所有权 | — |
| `ClaimRemoteOwnership` | 首次认领（Mongo 插入；本机已是 owner 时幂等） | 别的 sid 赢了：`ErrRemoteFenced` |
| `EnterRemoteSharedMode` / `LeaveRemoteSharedMode` | 独占 ↔ 共享（共享时每次写都取 Redis 锁） | 回复丢失时按权威重读裁决；权威也读不到则冻结为 `recovering`，直到下一次权威读成功 |
| `TransferRemoteOwnership(id, newSid)` | 迁移给别的 sid（RouteEpoch +1，旧 owner 变 `fenced`） | 同上 |

所有权迁移和写准入共用写门与所有权锁，迁移期间本机写排队。marker 缓存 `marker_cache_ttl`（core 500ms）内不重读权威（`remoteentity/wrapper.go:231-240`）。

### 4.4 读快照

三种一致性（`entity/remote_snapshot.go:641-685`）：

| 一致性 | 行为 | 适合 |
| --- | --- | --- |
| `Cached` | 只读缓存：L1 已确认且在陈旧上限内就交出；否则读 L2 重新确认；L1、L2 都没有就是“未找到”，**不回源** | 高频展示、AOI 属性 |
| `Monotonic` | 同上，但不满足下限或未命中时按 `(key, After)` 合并回源权威一次 | 需要“不旧于我上次看到的” |
| `Linearizable` | 每次读权威，不合并；只在 loader 声明线性化时开放，否则 `ErrRemoteReadUnsupported` | 结算前校验（或改发 owner 命令） |

读的入口：

- Nest handler 声明 `RemoteAccess`（Cached / Monotonic / Linearizable，`allow_stale` 时 Cached 不把下限交给读出口，由 `Accepts` 判定，`nest/remote_access.go:128-132`）。
- 业务代码：`entity.RemoteSnapshotReadOnly.ReadSnapshot(ctx, RemoteSnapshotRead{Key, Consistency, After})`；Manager 与 `SnapshotClient` 都实现它。
- DTO：`entity.NewRemoteMirrorReader(source, spec, decode)` 或生成的 `New<DTO>Reader(source)`（`entity/remote_mirror.go:112-156`）。解码拿到的是字节副本，改 DTO 不污染缓存；读侧再核对 key、schema、codec。

观察 token：读结果带 `Observation()`；作为下一次读的 `After` 时，epoch 更新的快照满足旧 token（不论版本），epoch 相同时比版本，**一新一旧**返回 `ErrRemoteObservationIncomparable`（`entity/remote_mirror.go:54-71`），调用方应重读并以新 token 为准。

每次 `ReadSnapshot` 都顺带续租这个 key 的兴趣（剩余不足一半才真正广播，`remoteentity/snapshot_client.go:258-309`）；续租失败不影响这次读。

### 4.5 只读服务（Mirror DTO）

1. DTO 放在生成器扫描的目录：

   ```go
   //roost:mirror entityKind=EntityKindGuild coll=remote_guilds
   type GuildSummary struct {
       Name    string `bson:"name"`    // 按 owner DAO 的 bson 键写，只写要读的字段
       Members int64  `bson:"members"`
   }
   ```

   `entityKind` 与 `coll` 都必填；DTO 不能有嵌入字段或 `dao:` / `comp:` 标签（`codegen/internal/entity/mirror.go:127-184`）。旧的 `remote=mirror` / `lifetime=mirror_cache` 报迁移错误（`codegen/internal/entity/parse.go:316-322`）。
2. 生成的 spec：`Scope = RemoteSnapshotScope(coll)`、`Schema = RemoteSnapshotSchema(kind, scope)`、`Codec = 1`，与 owner 生成的提交快照同一规则；`Tenant`、`Policy` 为 0（多租户 / 多 profile 要手改 spec）。
3. 服务装 `kitremote.NewRemoteMirrorMod(sid)`；读取：

   ```go
   source, err := kitremote.MirrorSource(registry) // Provide 之后（服务 Init / Mod Start）
   reader, err := NewGuildSummaryReader(source)
   v, found, err := reader.Read(ctx, guildID, entity.RemoteReadCached, entity.RemoteObservation{})
   ```

4. `sid` 是兴趣的 consumer 身份，必须与 owner 及其他只读服务都不同。缺省权威 loader 是只读 Mongo loader，不声明线性化；`WithMirrorLoader(loader, linearizable)` 可换（`kit/remoteentity/remote_mirror_mod.go:55-59`）。
5. `RemoteMirrorMod` 的停机预算 `remote_entity.mirror.shutdown_timeout` 不计入生成器算出的 `shutdown.total_timeout`，手工接入要一并调大总预算与部署宽限期（`kit/remoteentity/remote_mirror_mod.go:173-180`）。

### 4.6 ownerroute：把命令送到 owner

`ownerroute.Router[C, K, R]`（`ownerroute/route.go:23-72`）按 `KeyOf(cmd)` 查路由：owner 是本机就调 `Executor`，否则经 `Transport.Send` 发给 owner sid。`BusTransport` 用 `bus.SendByType(serviceType, sid, module, cmd)`，接收端用 `RegisterBusHandler` 解码后执行（`ownerroute/bus.go`）。仓库里的用法是 game-demo 赠礼 saga 的转交（`demo/internal/service/game/gift_saga.go.tmpl:165-169`、`:277-297`）。注意：

- 这是**单向**消息：接收端执行失败只记 Warn，发送方收不到结果；需要结果的命令自己设计回执或改用 RPC。
- 消息走 core NATS（至多一次），可靠消费开着时接收端按 MsgID 去重（§4.7）。
- `BusTransport.Send` 不使用调用方的 ctx（`ownerroute/bus.go:21`）。

### 4.7 bus：模块消息与 RPC

| API | 传输 | 语义 |
| --- | --- | --- |
| `Send(toSid, module, msg)` | `<prefix>.srv.<sid>` | 发给某 sid 的**所有**服务类型进程 |
| `SendByType(svcType, sid, module, msg)` | `<prefix>.svc.<type>.<sid>` | 定向到某类型的某实例 |
| `Broadcast(svcType, module, msg)` / `BroadcastAll` | `.svc.<type>.all` / `.srv.all` | 广播 |
| `Handle(module, msgName, h)` | — | 注册；同 module 的消息按 module 哈希到固定 worker，**同 module FIFO、跨 module 并行**（`bus/bus.go:739-741`） |
| `Call / CallTo / CallWithTimeout / CallAsync` | core NATS request-reply | 轻量 RPC；缺省不重试（`nats/rpc.go:35-42`）；每次尝试最多 5s，与调用方截止取先到者（`nats/driver/rpc.go:162-166`），`CallAsync` 固定 5s（`:240`） |
| `CallReliable / CallToReliable` | JetStream 请求流 + 响应流 | 持久 RPC，需 `nats.rpc.transport: jetstream`；请求带调用方截止时刻，过期请求不执行 |
| `HandleRpc(method, h)` | 开 JetStream RPC 时只注册持久通道 | 两端 transport 必须一致；不一致时轻量调用被请求流截获，调用方得到 `bus.ErrRPCCapturedByJetStream`，服务端不执行（`bus/rpc_error.go:13-19`） |

可靠消费（`nats.reliable.enabled: true`，需要 Redis Mod）：每条消息带 MsgID；消费端处理前 `SETNX inbox:<consumer>:<id> processing`，处理完改写 `done`；键已存在就当重复跳过。**这不是至少一次投递**：消息本身仍是 core NATS 发布；崩溃留下的 `processing` 让同 ID 的重投被跳过直到 InboxTTL（缺省 24h）。失败（无 handler、panic、去重存储出错）进死信，运维用 `bus.dlq.list / bus.dlq.requeue / bus.dlq.purge`（`bus/admin.go:11-13`）；重投用新 ID `requeue:<条目摘要>`，会再执行一次，handler 要自己幂等（`bus/reliable.go:55-69`）。

JetStream 持久 RPC 的 handler **必须幂等**：broker 在 AckWait（缺省 10s）内没收到 ack 就重投，`MaxDeliver` 缺省 5；handler 被停机打断时消息交还 broker 由其他实例处理（`bus/jetstream_rpc.go:520-525`）。

### 4.8 直接用 nats 驱动时

- `Assembly.Close(ctx)` 之后，每个导出方法返回 `fnats.ErrClosed`（可 `errors.Is`），`Connected()` 为 false；与 Close 并发的调用要么在 Close 之前完成，要么返回 `ErrClosed`（`nats/driver/client.go:25-31`）。
- 重复 Close 返回 nil；只有“排空失败被硬关”的那一次返回 `ErrClosedUndrained`（`nats/driver/assembly.go:72-105`）。
- 先停 bus 再关 Assembly（bus 持有订阅）；kit `NatsMod` 已按这个顺序（`kit/nats/nats_mod.go:202-247`）。

[↑ 速览](#速览) · [实现文档 §3](../impl/05-remote-mirror.md#3-主流程)

---

## 5. 配置

所有键由 kit 配置声明严格读取（A4 ①），写错一次报全（[07 配置分区](07-config.md)）。下表“core 缺省”是 `remoteentity.DefaultConfig()`（`remoteentity/config.go:79-114`），kit 对数字与时长键“0 或不配置 = 用 core 缺省”（`kit/remoteentity/config.go:84-88`）；“生成模板”是新生成工程配置里写出的值（kit 声明的 `example`，`codegen/internal/roost/kitconfig_gen.go:262-302`），**两者不同时以部署实际的配置为准**。

### 5.1 `remote_entity.*` 写入段（只有 `RemoteEntityMod` 读）

| 键 | core 缺省 | 生成模板 | 含义 / 校验 |
| --- | --- | --- | --- |
| `lock_ttl` | 24h | 15s | 共享锁租约；没开单实例锁时，强杀后同 sid 重启要等它过期才能写（§7.2） |
| `lock_key` | `e` | 不写 | 锁身份；Redis Cluster 下必须带非空 hash tag，否则启动报错（`kit/remoteentity/config.go:120-133`）；不能滚动修改 |
| `retry_count` / `retry_delay` | 5 / 100ms | 3 / 100ms | 取锁重试 |
| `op_timeout` | 30s | 3s | 一次准入 / 所有权操作的总预算；也是启动时建索引与 outbox 恢复各自的上限 |
| `unlock_retry_count` / `unlock_retry_interval` / `version_ttl` | 5 / 100ms / 24h | 同 | 释放锁与版本缓存 |
| `finalize_retry_interval` | 500ms | 500ms | finalizer 退避起点（上限 5s） |
| `finalize_projection_timeout` | 30s | 不写 | async / strict / pipelined 等投影器结论的上限，超期回源 |
| `max_write_batch` | 100 | 64 | 一个批次的实体上限 |
| `max_concurrent_writes` | 128（kit 声明 `default:"128"`） | 不写 | 在途写批次上限，满了直接 `ErrRemoteOverloaded`；**0 = 取 `async_finalize_capacity`**（`remoteentity/transaction_manager.go:95-100`） |
| `async_finalize_capacity` / `async_finalize_workers` | 4096 / 16 | 同 | finalizer 队列与 worker |
| `transaction_track_limit` / `transaction_track_ttl` | 65536 / 10m | 100000 / 10m | 事务 tracker 容量（只淘汰已结束的） |
| `wrapper_capacity` / `wrapper_idle_ttl` | 65536 / 5m | 同 | 每实体协调单元 |
| `mongo.database` | `remote_entity` | 同 | 元数据、事务记录、快照文档所在库 |
| `mongo.transaction_ttl` | 0 → 7 天 | 168h | 只对已发布的事务记录生效；未发布的不过期 |

### 5.2 快照段（`RemoteEntityMod` 与 `RemoteMirrorMod` 共用一份声明，`kit/remoteentity/config.go:20-38`）

| 键 | core 缺省 | 生成模板 | 含义 / 校验 |
| --- | --- | --- | --- |
| `snapshot_cache_shards` / `_entries` / `_bytes` | 64 / 65536 / 256MiB | 64 / 10000 / 268435456 | L1 容量 |
| `snapshot_cache_ttl` | 30s | 30s | L1 条目存活；删除标记同 |
| `cached_max_staleness` | 不配 = `snapshot_cache_ttl`（再为 0 时 30s） | 30s | 非线性读的陈旧上限；配置了必须为正（`min:"1ns"`） |
| `snapshot_l2_ttl` | 5m | 10m | L2 键 TTL（快照与墓碑都按它续期）；有 L2 时必须为正 |
| `snapshot_l2_key_prefix` | 空 | 空 | 多部署共用 Redis db 时必须各配不同值；不能含空白与 `{}` |
| `snapshot_l2_tombstone_wait_replicas` | 1（core `Config{}` 零值为 0 = 关） | 1 | 墓碑写后 WAIT 几个副本；负数报错 |
| `snapshot_l2_tombstone_wait_timeout` | 50ms | 50ms | 要等副本时必须在 (0, 1s] |
| `snapshot_interest_ttl` | 30s | 30s | 兴趣租约；剩余不足一半才续 |
| `snapshot_interest_keys` | 65536 | 10000 | 本机（consumer 侧）兴趣表 key 上限 |
| `snapshot_interest_subs` | 262144 | 100000 | 每节点兴趣表条目上限（租约 + 撤销水位） |
| `snapshot_interest_per_consumer` | 0 = subs/16（16384） | 0 = subs/16（6250） | 每个 consumer 的租约配额；必须在 `[0, snapshot_interest_subs]` |
| `marker_cache_ttl` | 500ms | 2s | 所有权缓存 |
| `snapshot_load_timeout` | 2s | 3s | 单次权威加载 / L2 调用上限 |
| `snapshot_max_waiters` | 256 | 4096 | 同一合并加载的等待者上限 |
| （无 kit 键）`Config.SnapshotReplicaBuffer` | 64 | — | 首载缓冲条数 / key |

### 5.3 `remote_entity.mirror.*`（只有 `RemoteMirrorMod` 读）

| 键 | 缺省 | 含义 |
| --- | --- | --- |
| `shutdown_timeout` | 5s（kit 声明；`Stop()` 无预算时同值） | 只读 Mod 的停机预算（`StopBudget`） |

### 5.4 影响 Remote 行为的其他段

| 键 | kit 缺省 | 生成模板 | 影响 |
| --- | --- | --- | --- |
| `syncbus.transport` | `nats` | `jetstream` | `nats` 时**没有快照推送**，读取只靠陈旧上限回源 |
| `syncbus.publish_timeout` | 0 → 5s | 3s | 推送与兴趣广播的发布上限（也是 §7.2 第 6 条的阻塞上限） |
| `singleton.enabled` | false | 见 01 分区 | 开了才有 O-M6-6 锁接管 |
| `sid` | 必填 | — | owner 的 ownership 身份 / 只读服务的 consumer 身份；0 启动报错 |

### 5.5 `nats.*`（`kit/nats/nats_mod.go:43-69`）

| 键 | 缺省 | 含义 |
| --- | --- | --- |
| `nats.url` / `nats.prefix` / `nats.worker_num` | `nats://localhost:4222` / `roost` / 8 | 连接、主题前缀、bus 派发 worker（每 worker 队列 1024，不可配） |
| `nats.ignore_discovered_servers` | false | 只连配置地址（代理 / NAT / 故障注入） |
| `nats.reliable.enabled` / `prefix` / `inbox_ttl` / `dlq_ttl` | false / `roost:bus` / 24h / 7d | 可靠消费；死信每个 `(module, msg)` 列表最多 10000 条（不可配） |
| `nats.rpc.transport` | `core` | `jetstream` / `js` 开持久 RPC |
| `nats.rpc.ack_wait` / `max_deliver` / `request_ttl` / `call_timeout` | 10s / 5 / 30s / 5s | 持久 RPC 重投与超时 |
| `nats.rpc.request_stream` / `response_stream` / `stream_max_age` / `duplicates` / `replicas` / `max_bytes` / `setup_timeout` | `ROOST_RPC_REQUESTS` / `ROOST_RPC_RESPONSES` / 30m / = request_ttl / 0 / 0 / 5s | 流配置 |

连接层的重连（1s、无限次）、ping（20s）、排空超时（10s）来自 `fnats.DefaultConfig`（`nats/config.go:17-25`），kit 不暴露。

[↑ 速览](#速览) · [实现文档 §7](../impl/05-remote-mirror.md#7-持久化--协议格式)

---

## 6. 运行与运维

### 6.1 启动与停机顺序

owner 进程（`remoteentity/assemble.go:138-205`）：

1. 校验依赖；再校验一次 Remote DAO 的库范围（`Assemble` 之后注册的实体也覆盖）。
2. `SnapshotClient.Start(bus)`：JetStream 上订阅快照（DeliverNew）、兴趣、兴趣续租请求三个主题；普通 NATS 上只订阅兴趣并记 Warn。
3. 推送开着时广播一次“请重新续租兴趣”（O-M6-1），失败只 Warn。
4. 封存依赖；`EnsureRemoteStorage`（建索引；存在非权威元数据时拒绝启动）；`RecoverOutbox` 补发全部 Applied 事务；启动 finalizer。

DataEngine Mod 在开了 Remote 投影时排在 `RemoteEntityMod` 之后（`kit/dataengine/mod.go:88-94`），所以停机时投影器先停、Remote 后停。`Assembly.Stop`：先停 finalizer（未拿到结论的收尾项交还资源、实体保持隔离），再三步停 `SnapshotClient`；ctx 到期返回 ctx 错误，之后再调用继续等同一批（`remoteentity/assemble.go:207-238`）。停过的 Assembly 不能再 Start。

只读服务：`RemoteMirrorMod.Start` 只启动客户端；`StopWithContext` 关读准入、取消在途权威加载、退订，在 ctx 内等已准入的工作（`kit/remoteentity/remote_mirror_mod.go:194-211`）。

### 6.2 健康

| 健康项 | Fail | Degraded | OK 信息 |
| --- | --- | --- | --- |
| `remote_entity`（`kit/remoteentity/remote_entity_mod.go:160-179`） | 释放失败（fatal）、本机兴趣表满、活跃事务数达上限 | 写额度用满 | `snapshot_push`、`interest_refused`、写额度与容量 |
| `remote_mirror`（`kit/remoteentity/remote_mirror_mod.go:215-229`） | 已停止 | 本机兴趣表满 | `snapshot_push`、`interest_refused`、`local_interests`、`bootstrap_overflows` |
| `nats`（`kit/nats/nats_mod.go:125-136`） | 未初始化、未连接 | — | `connected` |

`snapshot_push=false` 与 `interest_refused>0` 都是显式的按需读取，不算故障。

### 6.3 指标

| 指标 | 说明 |
| --- | --- |
| `remote_entity.remote.prepare_total{result,batch}` / `prepare_latency`、`write_gate_wait` | 写准入 |
| `remote_entity.write_admission_rejected_total`、`remote_entity_write_gate_timeout_total` | 写额度满、等写门超时 |
| `remote_entity.remote.apply_total{result}` / `apply_latency` | 提交与发布 |
| `remote_entity.finalize_retry_total`、`finalize_status_read_total`、`quarantine_error_total`、`deferred_outcome_not_run_total{outcome}`、`unresolved_resolved_total{state}` | finalizer |
| `remote_entity.release_failure_total` | 释放锁失败（触发 fatal） |
| `remote_entity.lock_takeover_total` | O-M6-6 接管次数 |
| `remote_entity.remote.read_total{result,consistency}` / `read_latency` | 快照读 |
| `remote_entity.snapshot_push_enabled{sid}` | 推送开关 |
| `remote_entity.snapshot_bootstrap_overflow_total`、`snapshot_bootstrap_replay_failed_total` | 首载缓冲 |
| `remote_entity.snapshot_replica_historic_dropped_total` | 丢弃的过老复制快照（O5） |
| `remote_entity.snapshot_l2_tombstone_wait_total{result}` | `confirmed / short / no_replicas / error / skipped` |
| `remote_entity.remote.interest_rejected_total{reason}`、`interest_renew_refused_total{reason}` | O4 配额 / 表满 |
| `remote_entity.remote.interest_refresh_{sent,requests,renewed}_total` | O-M6-1 |
| `bus_dispatch_total` / `bus_dispatch_duration` / `bus_duplicate_total` / `bus_dead_letter_total` / `bus_dispatch_drop_total` | 模块消息 |
| `bus_rpc_call_total` / `bus_rpc_request_total` / `bus_rpc_pending` / `bus_rpc_pending_total` / `bus_rpc_consumer_delivery` | 持久 RPC（method 标签有界） |
| `nats.rpc.pending` / `started` / `completed` / `queue_rejected` / `callback.latency`、`nats.jetstream.terminal.total`、`nats.jetstream.settle_failures.total`、`nats.subscription.handler_panic.total` | 驱动 |

### 6.4 日志（运维常见）

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `remote_entity: snapshot push disabled: the sync bus cannot confirm subscriptions (JetStream required); …` | Warn | 普通 NATS，按需读取 |
| `remote_entity: snapshot interest refused; …` | Warn（每表 10s 一条） | O4 拒绝 |
| `remote snapshot: bootstrap buffer overflowed; …` | Warn | 首载缓冲溢出，加载后整体回源 |
| `remote_entity: snapshot tombstone written on the Redis primary but not confirmed by its replicas; …` | Warn（每 store 10s 一条） | WAIT 不足 / 出错 |
| `remote_entity: could not ask consumers to renew their snapshot interest; …` | Warn | O-M6-1 发送失败 |
| `remote_entity: took over a shared lock left by the previous process of this sid …` | Info（每进程首次） | O-M6-6 |
| `remote_entity: fast pool refused the deferred post-commit work; …` | Warn | 提交后工作没执行（Nest 已停或 fence） |
| `bus: more distinct jetstream rpc methods called than the metrics label limit; …` | Warn（一次） | 调用方方法名超过 256 |

### 6.5 错误速查

| 错误 | 来源 | 意思 |
| --- | --- | --- |
| `entity.ErrRemotePersistenceIndeterminate` | 写 | Remote 可能已提交，不要盲目重放 |
| `entity.ErrRemoteRejected` | 写 | 明确未提交（含持久拒绝、坏输入） |
| `entity.ErrRemoteFenced` / `ErrRemoteEntityReloading` | 写 | 不是合法写者 / 被拒实例正在重载（可重试） |
| `entity.ErrRemoteVersionConflict` | 写 / 读 | 版本冲突；读路径上是“同版本异值”的一致性错误 |
| `entity.ErrRemoteOverloaded`（含 `ErrInterestQuotaExceeded`、`ErrInterestRegistryFull`） | 写 / 兴趣 | 容量 |
| `entity.ErrRemoteSnapshotStale` | 读 | Cached 有值但低于下限；Monotonic 回源后仍不满足 |
| `entity.ErrRemoteObservationIncomparable` | 读 | token 与快照 epoch 一新一旧 |
| `entity.ErrRemoteReadUnsupported` | 读 | 读者不提供 Linearizable |
| `remoteentity.ErrSnapshotClientStopped` / `ErrAssemblyStopped` | 读 / 生命周期 | 已停止 |
| `bus.ErrNoHandler`、`bus.ErrRPCCapturedByJetStream`、`bus.ErrJetStreamRPCUnavailable` | bus | 无 handler / 两端 RPC transport 不一致 / 没开持久 RPC |
| `fnats.ErrTimeout` / `ErrNoResponders` / `ErrClosed` / `ErrCancelled` | nats | 请求超时 / 无响应者 / 已关闭 / 取消（RPC 停止时同时 `errors.Is` 到 Cancelled 与 Closed） |
| `driver.ErrClosedUndrained` | nats | 排空失败被硬关（只报一次） |

### 6.6 常见故障 → TROUBLESHOOTING

| 现象 | 行 |
| --- | --- |
| 某实体每次写都 `versioned lock not acquired`，持续到 `lock_ttl` | [T-207](../../TROUBLESHOOTING.md) |
| 启动日志 `snapshot push disabled`，读到新版本变慢 | T-272 |
| 某服读 Remote 快照总慢半拍，`interest refused` | T-273 |
| 生成器报 `remote=mirror no longer generates an entity`；或 `capability "remote_entity.mirror" already registered` | T-274 |
| 删除 Remote 实体返回 `remote acknowledgement identity mismatch`（v1.21.0 及之前） | T-276 |
| 墓碑 WAIT 不足的 Warn | T-277 |
| owner 同 sid 重启后只读方慢几秒 | T-278 |
| 启动建索引撞 Mongo 选举 | T-282 |

[↑ 速览](#速览) · [实现文档 §6](../impl/05-remote-mirror.md#6-失败与不确定结果处理)

---

## 7. 保证与不保证

### 7.1 保证

1. **单一合法写者**：每笔 Remote 提交在 Mongo 同一事务里校验 `_ver = BaseVersion`、`_owner_epoch`、`_owner_route`、`_grant_fence` 与非空许可 token，`MatchedCount` 必须等于实体数，否则整批回滚（`remoteentity/mongo_committer.go:414-438`）。暂停后恢复的旧写者、旧路由写者都被拒。
2. **多实体原子**：一个事务里的多个 Remote 实体（以及同记录里的普通 DAO、回执、effect）在一个 Mongo 事务里提交（`dataengine/engine/mongo_projection.go:80-142`）；不支持原子批次的后端启动即失败，不退化成逐实体提交。
3. **事务 ID 幂等**：同一 `RemoteTransactionID` 的重放返回原回执；内容不同（digest 不同）明确拒绝（`remoteentity/mongo_committer.go:155-167`）。
4. **不早于持久结论释放**：写门、Redis 锁、写额度只在 Committed（已发布）或 Rejected（已回滚隔离）之后交还；停机排空拿不到结论时也交还，但实体保持隔离（`remoteentity/transaction_manager.go:229-243`）。
5. **提交后工作至多一次**，只在快池执行，只在持久结论之后。
6. **快照读后置条件**：完整 key 一致、未过自身 `ExpiresAt`、满足 `After`；非线性读另要求 `now − confirmedAt ≤ cached_max_staleness`，确认不了返回错误，不交出旧值（`entity/remote_mirror.go:84-89`、`entity/remote_snapshot.go:641-699`）。
7. **不回退、不复活**：L1 与 L2 用同一条排序规则（epoch 新者胜、同 epoch 比版本、删除挡住不新于它的快照，`entity/remote_snapshot.go:592-617`、`remoteentity/snapshot_l2.go:43-101`）；过老的复制快照（发布超过 `snapshot_l2_ttl/2`）不准入（`remoteentity/syncer.go:126-134`）。
8. **推送确认后不静默丢**：JetStream DeliverNew durable 订阅返回即确认，之后发布的快照 AckExplicit 投递（04 分区驱动）。
9. **兴趣**：renew / release 在同一 key 条带锁内分配代际；release 之后迟到的更旧 renew 不复活租约，表满时由溢出水位兜住（`remoteentity/interest.go:159-307`）。
10. **nats 驱动 Close**：Close 之后每个导出方法返回 `fnats.ErrClosed`，新增导出方法不登记守卫表就红（`TestEveryExportedDriverMethodHasAClosedStateCheck`，`nats/driver/closed_state_guard_promises_test.go:194`）。

### 7.2 不保证与已知限制

1. **`Cached` 不回源**：L1 与 L2 都没有该 key 时返回“未找到”；L2 读不到（断网）且 L1 也没有时同样是“未找到”而不是错误（`entity/remote_snapshot.go:792-795`）。需要区分“不存在”与“L2 故障”的读用 Monotonic。
2. **没有 L2 的装配**（测试、单进程）里 L1 自己是水位，删除标记受 L1 容量淘汰，迟到旧消息可能复活。
3. **普通 NATS 没有推送**；兴趣被拒的 key 也没有推送；两者都只靠陈旧上限按需读取。
4. **首载缓冲溢出且第一次加载失败时**，缓冲已丢、不会整体回源（`entity/remote_snapshot.go:413-419`）：之后这个 key 的新鲜度只靠陈旧上限。推断：只影响推送提前量，不影响正确性。
5. **跨主机时钟**：O5 历史过滤（发布时刻 vs 接收时刻）与 O-M6-1 请求过期都用两端 `time.Now()` 比较，窗口留了一半给偏差，多主机未验证（外部验证 E02）。
6. **兴趣广播与读同步**（疑点，待闭环）：`ReadSnapshot` 每次都先取 key 所在条带锁做续租判断，需要广播时在锁内同步发布（`remoteentity/snapshot_client.go:194`、`:263-265`、`:299`）。本篇编写时用临时探针在 tag 上实测：同条带（64 条之一）另一个 key 的 L1 命中读被一次 500ms 的慢广播拖了约 450ms。JetStream 卡住时单次发布最长 `syncbus.publish_timeout`（缺省 5s，`sync/syncbus/driver/jetstream.go:30`、`:179`），发布失败还会撤掉本机兴趣、下一次读再试。
7. **发布失败会卡住 owner 的投影**（疑点，待闭环，推断 / 未验证）：投影器写完 Mongo 后同步发布；有 consumer 对该 key 有兴趣、而同步总线发布失败时，`ApplyRemoteCommits` 返回结果未知，投影器按退避重试这条记录，其后所有 WAL 记录（包括普通 DAO）都等它（`dataengine/engine/mongo_projection.go:156-159`、`remoteentity/transaction_manager.go:661-665`）。Mongo 里该事务已是 Applied，outbox 恢复本可以单独补发。
8. **JetStream RPC 可能并发重复执行**（疑点，推断 / 未验证）：handler 的期限取调用方截止时刻，没有 in-progress ack；调用方截止长于 `nats.rpc.ack_wait` 且 handler 跑得比 AckWait 久时，broker 会把请求重投给另一个实例（`bus/jetstream_rpc.go:471-476`、`:550-562`，`nats/driver/jetstream.go:88-90`）。kit 没有像 saga 消费者那样校验 `call_timeout < ack_wait`。
9. **owner 删除仍是删文档**：Mongo 里没有带版本的删除墓碑，只读 loader 读到删除是 `found=false`，防复活靠 L2 墓碑；墓碑 WAIT 只缩小切主窗口，副本在确认前断开仍会丢（要彻底避免需 `min-replicas-to-write`）。
10. **锁 TTL**：没开 App 单实例锁时，同 sid 被强杀后重启的第一笔写要等上一代的锁过 `lock_ttl`（core 缺省 24h，生成模板 15s）；这期间写方报 `versioned lock not acquired` 或请求截止为结果未知，不要朴素重试。
11. **bus 可靠消息至多一次**：见 §4.7；死信重投会再执行。
12. **轻量 RPC 单次尝试最多 5s**：调用方 ctx 更长也没用（`nats/driver/rpc.go:162-166`）。
13. **ownerroute 是单向的**，执行失败发送方不知道。

### 7.3 L2 落后于权威的上界

owner 提交后写 L2 失败或结果未知时，L2 最长落后权威 `snapshot_l2_ttl`（从旧值最后一次写进 L2 算起），读者在那之后最多再交出 `cached_max_staleness`：

| 配置来源 | `snapshot_l2_ttl` | `cached_max_staleness` | 上界 |
| --- | --- | --- | --- |
| core `DefaultConfig` | 5m | 30s（= `snapshot_cache_ttl`） | 约 5m30s |
| 生成工程配置模板 | 10m | 30s | 约 10m30s |

“最后一次写进 L2”包括同值重写（CAS 脚本对同版本同值也 `PEXPIRE`，`remoteentity/snapshot_l2.go:69-70`）：L1 冷的节点在 O5 窗口内收到旧值的复制消息会把旧值写回并续期，所以从旧值发布起算的最坏上界约 `1.5 × snapshot_l2_ttl + cached_max_staleness`。通常更早修好：owner 自己的 L1 条目未确认，下一次读它时把新版本 CAS 进 L2；推送开着时每个收到复制消息的节点都会写 L2；这个实体的下一笔提交也会写 L2。`Linearizable` 不受影响。

### 7.4 需要外部验证的项

本机做不了或不能代表生产的，统一在 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)：E01（Linux 内核网络）、E02（跨主机分区与时钟偏差）、E06（JetStream 多节点 HA）、E08（多机 Redis Cluster）、E10（Redis 异步复制丢写）、E13（多主机强杀）、E14（Remote outbox“已提交、发布前崩溃”精确注入）、E15 / E16（长时间容量、大规模扇出）。

[↑ 速览](#速览) · [实现文档 §4](../impl/05-remote-mirror.md#4-不变量清单)

---

## 8. 相关文档

- 本分区实现文档：[impl/05-remote-mirror.md](../impl/05-remote-mirror.md)
- 其他分区：[01 app 与生命周期](./01-app-lifecycle.md)、[02 nest 调度与实体](./02-nest-entity.md)、[03 dataengine](./03-dataengine.md)、[04 sync](04-sync.md)、[06 saga](06-saga.md)、[09 kit 服务](09-kit-services.md)（game-demo 赠礼转交）、[11 可观测](11-observability.md)
- 快速参考：[USER_GUIDE §6 Remote Entity](../../USER_GUIDE.md)、[REMOTE_ENTITY.md](../../../REMOTE_ENTITY.md)（较旧，部分描述与源码不一致，以本篇为准）、[TROUBLESHOOTING](../../TROUBLESHOOTING.md)
- 方案与决定：[B2 水位](../../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)、[Mirror 1～3](../../feature/MIRROR-STEPS-1-3-2026-10-06.md)、[Mirror 4 与 O4](../../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)、[Mirror 5](../../feature/MIRROR-STEP-5-2026-10-06.md)、[Mirror 6 本机替代](../../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)、[M6 观察](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)、[PLAN-REMOTE-POLICY-MIRROR](../../review/PLAN-REMOTE-POLICY-MIRROR.md)、[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md)
- 发版记录：[v1.23.0 说明 REM / DRV 部分](../../release/v1.23.0/guide-saga-drv-dao-rem.md)
- 执行契约：[roost-coding](../../agent-skills/roost-coding/SKILL.md)

[↑ 速览](#速览)

### 2026-10-08 兴趣协议收敛

兴趣发布与接收必须带非零 Generation、完整 Key 和 ConsumerSID；空 payload Delete 明确拒绝。无代际旧发布端不再兼容。撤销水位和乱序保护继续生效。
