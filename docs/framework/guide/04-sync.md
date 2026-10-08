# 04 sync（说明）

> 配套实现文档：[impl/04-sync.md](../impl/04-sync.md)（代码结构、时序图与状态机、不变量与守卫测试、并发与锁序、review 检查点）。
> 源码基准：tag `v1.23.0`（`28912cd6`），文中 `path:line` 都按这个 tag；生成代码的行号是**生成器模板源码**的行号（例如 `codegen/internal/roost/render_player_tcp.go` 里 Go 字符串的行号），不是生成出来的文件的行号。
> 图谱说明：codebase-memory 的索引代际是 2026-09-30。`sync/entitysync/*`、`syncstream/*`、`sync/lockstep/*` 与 tag 一致（`metadata_match`）；`sync/syncbus/subscription.go` 不在图谱里（`not_tracked`），`sync/syncbus/driver/jetstream.go`、`kit/syncbus/mod.go`、`cache/mirror.go`、`kit/nest/entity_sync.go`、`codegen/internal/roost/player_tcp_config.go` 为 `metadata_changed`。本篇只用图谱定位，结论全部按 tag 源码直接读取。

## 速览

- **这一块是什么**：sync 是 roost core 三块基础之一（另两块是 nest 调度和 dataengine），负责“把状态从一个地方复制到另一个地方”。它有两条互不相干的轴：**服务 → 客户端**（实体复制 `entitysync` + 组织政策 `policy` + 帧格式 `frame` + 传输 `nettransport`，以及并列的帧同步模型 `lockstep`、生成的玩家 TCP 接入层），和**服务 ↔ 服务**（总线契约 `syncbus` + NATS / JetStream 驱动 + 副本复制 `ReplicaSyncer` / `PatchSyncer`）。`syncstream` 是给 skill 用的有序持久流，挂在总线上。
- **最重要的保证**：① 每个会话每个 tick 收到的帧，内容版本一致（同一捕获里 delta 和快照同版本、同 `CommitLSN`）；② 开了持久化水位门控时，**客户端永远看不到服务端还可能丢掉的状态**——整个 subject 一起等水位（`sync/entitysync/flush.go:203`）；③ 一个会话的传输失败只关它自己，`ErrRetryLater` 让整个 tick 重来、谁也不受罚；④ 已准入的帧不撤回，部分成功后受影响订阅改发全量重建基线；⑤ 总线退订 `Subscription.Unsubscribe(ctx)` 返回 nil 就是“没有在途回调、也不会再有新回调”（A3 ②，`sync/syncbus/subscription.go:92`）。
- **最容易踩的坑**：① 订阅不等于授权——profile 只表达业务已经授权的视图，权限判断在政策之外；② 客户端装好解码器之前就开会话，首帧会和登录应答赛跑，要用 `OpenHeldSession` + `ReadySession`；③ 政策阶段（`RegisterPolicy` 的处理器、`Resubmit` / `Released` 回调、`SessionLost`、`RegisterAfterRetirement` 的 done）都跑在持有 `flushGate` 的 Flush 里，**不得调用 Flush、不得阻塞**；④ `Interest` 的排队事实（`QueueMove`）指向的 id 在应用前被 `Leave` / `Hide` 掉，会让整个 Manager 的每一次 Flush 都失败（本篇写作时发现，见 §7.3 与[实现文档 §10.2](../impl/04-sync.md#102-本篇写作时发现的源码疑点待闭环)）；⑤ 玩家 TCP 接入层里 controller 把业务错误当 `error` 返回会**直接断连**，业务失败必须编码成响应里的 `Code`；⑥ 普通 NATS 驱动是最多一次，要“订阅确认后至少一次”必须用 JetStream 的 `SubscribeLive`。

### 本篇覆盖的包

| 包 | 职责 |
| --- | --- |
| `sync/entitysync` | 实体复制机制：进程一个 `Manager`，subject 自己持有订阅者，按会话组帧、逐帧准入，prepare / commit 两阶段，held / ready 会话，持久化水位门控，快照预算，卸载后重载的接口 |
| `sync/entitysync/policy` | 组织：谁订谁——`Interest`（AOI + 关系来源 + 排队事实）、`Group`（全互见）、`Direct`（显式绑定）、`AOI` / `AOICluster`、profile 选择 |
| `sync/frame` | entitysync 状态帧的线格式（`Encode` / `Decode` / `Limits`），不知道会话和传输 |
| `sync/nettransport` | 传输：UDP（AEAD）/ KCP / QUIC、`AsyncTransport`（每会话有界可靠队列，只有可靠一条通道）、`SessionID` |
| `sync/lockstep` | 帧同步（输入帧）：`Room`、`Sequencer`、`History`、`DesyncDetector`、冗余广播线格式、客户端 `FrameAssembler` |
| `sync/syncbus` | 服务间总线契约：`ISyncBus`、`ILiveSubscriber`、`*Subscription`（排空退订，A3 ②）、`DeliveryIDs`、`PatchSyncer` |
| `sync/syncbus/driver` | `NewNatsSyncBus`（普通 NATS，最多一次）与 `NewJetStreamSyncBus`（持久、确认、durable 消费者） |
| `sync/syncbus/mirror` | 总线上的副本信封（upsert / delete） |
| `kit/syncbus` | `SyncBusMod`：按配置二选一装配驱动，登记能力 `syncbus` |
| `syncstream` | 有序持久流：`History`（序号、ACK、Resync / Recover）、`FileHistoryJournal`（checkpoint + WAL）、`Publisher` / `Subscribe*` / `BufferedPublisher` |
| `cache`（`ReplicaSyncer`） | 进程内副本缓存经总线同步（`cache/mirror.go`）；cache 的其余部分归 03 分区 |
| `gateway` | 与传输无关的请求边界契约（`Principal` / `Session`）与中间件（`RateLimit` / `Timeout` / `Recover`） |
| 生成的玩家接入层 | `codegen/internal/roost/render_player_tcp.go`、`render_access.go`、`player_tcp_config.go` 生成的 `access.player` / `access.player.tcp` 两个 Mod、`player_agent` 协议注册表 |
| `kit/nest`（entitysync 接线部分） | `NewModWithEntitySync`、`sync.entity.*` 配置、自动水位、卸载后重载接线；Nest 本身归 02 分区 |

跨分区：实体内容层 `entity/subject_sync.go`、提交边界 `entity.SyncMutation` 与 Nest 的 `NestOptionWithEntitySync` 在 [02 nest 与实体](02-nest-entity.md)；`DurableLSN`、pipelined 提交与 WAL 在 [03 dataengine](03-dataengine.md)；`remoteentity` 的快照推送（用 `ILiveSubscriber`）与 Mirror 在 [05 remote 与 Mirror](05-remote-mirror.md)；skillsync（syncstream 的唯一生产消费者）在 [08 skill](08-skill.md)；配置规则在 [07 配置](07-config.md)；指标总表与仪表盘在 [11 可观测](11-observability.md)；`roost add access player` / `add transport tcp` 生成器在 [12 codegen](12-codegen.md)；App 生命周期与三步停机在 [01 app](01-app-lifecycle.md)。

---

## 1. 定位与边界

sync 只做一件事：**把状态从一处复制到另一处**（ARCH-12 的判定标准，[ARCH-12 §1](../../bugfix/ARCH-12-sync-package-layout.md)）。`sync/` 目录本身没有 Go 文件，只是收纳（`sync/README.md:3`）。

```
服务 → 客户端                                   服务 ↔ 服务
policy ──Subscribe──▶ entitysync ──▶ frame        syncbus/mirror ──▶ syncbus ◀── syncbus/driver
                          │                        cache.ReplicaSyncer / PatchSyncer / syncstream / remoteentity
                          └──Transport──▶ nettransport ◀── lockstep
entity（内容层，块外）◀── entitysync        spatial（几何基建）◀── policy
```

（依赖方向见 `sync/README.md:18`。）

**负责**

- 实体状态的服务 → 客户端复制：订阅关系、每 tick 的捕获与组帧、交付失败的分类处理、持久化水位门控、快照预算、会话 held / ready / 重连、实体退役与卸载重载期间的订阅保持。
- 谁订谁的组织方式：距离兴趣（AOI）、关系（队伍 / 好友 / 自己）、集合全互见、显式绑定，以及它们叠加时选哪个视图。
- 两种客户端同步模型：状态同步（entitysync）和帧同步（lockstep）。
- 服务间的发布订阅总线：契约、两种驱动、排空退订、副本复制。
- 生成工程里玩家的 TCP 接入：监听、握手鉴权、连接限额、dispatch 超时、推送、会话关闭事件、停机排空。

**不负责**

- **实体内容**：版本、脏位、packer、`CommitLSN` 归实体（`entity/subject_sync.go:187`），entitysync 只调 `PrepareViews` 拿冻结的内容。
- **权限**：profile 只在“业务已授权的视图”之间选一个，不扩大授权（`sync/entitysync/manager.go:32`）。谁能看谁由业务和政策决定。
- **房间 / 区域**：它们是政策，不是帧上的标签；帧头 `RoomID` 恒为流常量 1（`sync/entitysync/wire.go:20`）。
- **跨服实体协议**：`remoteentity` 是 dataengine 块的消费者（05 分区），只借用 syncbus 的能力。
- **客户端解码之后的事**：Manager 不知道客户端是否真的应用了帧；“准入”指 `Transport.Push` 返回 nil。

→ [实现文档](../impl/04-sync.md)对应：§1 包与文件地图。

## 2. 核心概念与术语

### 2.1 实体复制（entitysync）

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| Manager | 一个进程（通常）一个，拥有全部 subject 与会话；每 tick 把脏 subject 变成每个会话自己的帧 | `sync/entitysync/manager.go:117` |
| subject | 一个实体的同步状态 `*entity.SubjectSyncState` 加上它的订阅者表。**订阅者表在 subject 上**，是“谁收它”的唯一真相（ARCH-10） | `sync/entitysync/subject.go:45` |
| session（会话） | 一个接收者：它的帧时钟（epoch / tick）与它已持有对象的引用表。会话 id 由政策定义（demo 用玩家 id），只要求稳定、唯一 | `sync/entitysync/session.go:54`、`sync/entitysync/transport.go:14` |
| subscription（订阅） | 一个会话对一个 subject 的状态：要哪个 profile、下一帧欠什么（快照 / 增量 / remove）、持有哪个版本 | `sync/entitysync/subject.go:29` |
| 订阅状态 `kindSnapshot` / `kindLive` / `kindLeaving` | 等全量 / 持有 `baseVersion`、下一帧发增量 / 欠一个 `ObjectRemove` | `sync/entitysync/subject.go:15` |
| 订阅来源（SubscriptionSource） | 一个独立的订阅所有者。同来源重复订阅幂等、换 profile 即替换；不同来源各自释放；最后一个来源离开才退订 | `sync/entitysync/subscriptions.go:271` |
| profile（视图）/ LOD | `SyncProfile{Key, LOD, SchemaVersion}`，一个有限的视图（LOD、权限、阵营投影）。**不含订阅者身份**；空 Key 规范为 `default` | `entity/subject_sync.go:50` |
| 视图优先级 | `ManagerConfig.ProfilePriorities` 越小越优先，未配置的按 LOD；平局按 LOD、Key、SchemaVersion | `sync/entitysync/subject.go:162` |
| held / ready 会话 | held：已开、可订阅、不出帧；`ReadySession` 之后第一帧是新 epoch 的全量帧 | `sync/entitysync/subscriptions.go:81`、`:144` |
| 准入（admitted） | `Transport.Push` 返回 nil：这一帧已交给传输，不能撤回 | `sync/entitysync/transport.go:23` |
| `ErrRetryLater` | 传输整体不可用（启动中 / 停机中）：整个 tick 作废、全部保持脏、谁也不罚 | `sync/entitysync/errors.go:38` |
| 持久化水位门控 | `ManagerConfig.DurableWatermark`：capture 的 `CommitLSN` 高于水位时整个 subject 本 tick 不发，下 tick 重试 | `sync/entitysync/manager.go:40`、`sync/entitysync/flush.go:203` |
| 两种模式 | `ModePeriodic`（默认，按 `Interval` 合并检查）/ `ModeOnChange`（锁内冻结、解锁且提交确认后唤醒，`Interval` 兜底） | `sync/entitysync/mode.go:14` |
| 快照预算 | `SnapshotBudget`：限制客户端**尚未持有**对象的创建（冷创建），增量、remove、已持有对象的全量替换不占额度；默认关闭 | `sync/entitysync/snapshot_budget.go:119` |
| 退役（retiring） | `Unregister` 之后：每个持有对象的订阅者欠一个 remove，全部发出后 subject 被遗忘（remove-before-create） | `sync/entitysync/manager.go:452` |
| 卸载重载（RR-59） | 实体被仅内存卸载后，若仍有订阅者，框架从权威重载并 `Rebind`；重载不了就退回 remove | `sync/entitysync/manager.go:545`、`entity/unload_resync.go` |
| 政策阶段 | 每次 Flush 捕获之前、持有 `flushGate` 时：先交付释放通知，再交付重新提交，再跑 `RegisterPolicy` 的处理器 | `sync/entitysync/policy_queue.go:45` |
| namespace | `syncNamespace=` 生成标记，随 subject 的每个组件下发，是客户端唯一的分流键 | `sync/entitysync/wire.go:31` |

### 2.2 组织政策（policy）

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| Interest | AOI + 任意多个关系来源聚合成每个 (observer, subject) 一份订阅：第一个来源认领时订阅，最后一个放手时退订；band → profile | `sync/entitysync/policy/interest.go:113` |
| AOI | 格子索引的增量兴趣层：进入 / 离开半径滞回、band、`MaxVisible` 淘汰、观察者格子预算。非并发安全，只在 Interest 锁内用 | `sync/entitysync/policy/aoi.go:179` |
| RelationSource | 集合驱动的关系来源（team、friends、self），band 固定为 0 | `sync/entitysync/policy/source.go:36` |
| 排队事实（QueueMove / QueueRelation） | Nest handler 在实体锁内入队的兴趣事实，提交确认后在政策阶段才应用；回滚 / 被拒的丢弃 | `sync/entitysync/policy/interest_queue.go:24` |
| Refusal | Manager 拒绝的订阅；Interest 每次 Apply 都会再说，直到被接受或 pair 释放 | `sync/entitysync/policy/interest.go:58` |
| Group | 成员集合 × 实体集合全互见，带上限、部分失败回滚。**不叫 Room**（`lockstep.Room` 是战斗房间） | `sync/entitysync/policy/group.go:50` |
| Direct | 逐对 `Bind` / `Unbind`，带绑定表、接 `Released` 戳 | `sync/entitysync/policy/direct.go:40` |
| AOICluster | 把多个区域的 AOI 拼成无缝空间；仓内只有测试使用 | `sync/entitysync/policy/aoi_cluster.go:91` |

### 2.3 服务间总线与流

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| `SyncMsg` | 总线线格式：`MessageID`（投递身份）、Topic、Key、Version、Data（nil 表示删除）、FromSid、分片与编码字段 | `sync/syncbus/sync.go:6` |
| `ISyncBus` / `ILiveSubscriber` | 发布 + 订阅 / 可确认订阅：`SubscribeLive` 返回 nil 后每条消息至少一次交给 handler；普通 NATS 不提供 | `sync/syncbus/sync.go:57`、`:52` |
| `*Subscription` | 一次订阅的句柄；排空在这里实现一次、所有驱动共用；`Unsubscribe(ctx)` 按三步停机 | `sync/syncbus/subscription.go:21` |
| 投递 ctx | `Subscription.Deliver` 交给 handler 的 ctx，带“正在执行这个订阅的回调”标记；handler 里退订自己必须传它 | `sync/syncbus/sync.go:22` |
| syncstream History | 每个 (Observer, Stream) 一条序号链；ACK、Resync（判断需不需要全量）、Recover（补一个全量） | `syncstream/syncstream.go:113` |
| FileHistoryJournal | History 的 checkpoint + WAL 持久化，组提交、尾部截断、fail-stop | `syncstream/file_journal.go:23` |

### 2.4 帧同步与接入

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| lockstep Room | 输入帧同步房间：收输入、按 tick 切帧、冗余广播、追帧、关键帧哈希裁决。单所有者、无锁 | `sync/lockstep/room.go:128` |
| 迟到折入 | 帧号小于当前帧的输入改写到当前帧；同一输入在 64 帧地平线内重传幂等 | `sync/lockstep/sequencer.go:216`、`:234` |
| 追帧（catch-up） | 重连 / 缺口时从历史分页经可靠通道补发，期间不发实时 datagram | `sync/lockstep/room.go:354` |
| `access.player` / `access.player.tcp` | 生成的两个 Mod：协议注册表 + WriteGate / TCP 监听、握手、会话表、推送 | `codegen/internal/roost/render_access.go:358`、`codegen/internal/roost/render_player_tcp.go:146` |
| dispatch 超时 | 每个请求的等待上限；**只限制等待，不限制不配合 ctx 的 handler** | `codegen/internal/roost/render_player_tcp.go:843` |

→ [实现文档](../impl/04-sync.md)对应：§2 关键类型与数据结构。

## 3. 设计原因

| 取舍 | 结论 | 出处 |
| --- | --- | --- |
| room / AOI / 直连各有一套同步？ | **一套机制**：一个 Manager，subject 持有订阅者，room / AOI 降为“谁订谁”的政策。之前三层（coordinator / room / sink）各记一份订阅、各有失败政策 | [ARCH-10](../../bugfix/ARCH-10-sync-manager.md)（维护者 2026-09-22 拍板）、[M-13](../../bugfix/M-13-entitysync-manager.md)、[M-14](../../bugfix/M-14-sync-policy-and-ready.md) |
| 每层知道什么 | 四层，每层只知道下一层：entity 不知道 session，Manager 不知道为什么有人订阅，policy 不碰帧与传输，应用不写聚合规则 | `ENTITY_SYNC.md:5` |
| 帧是每会话的还是每房间的 | 每会话：`Epoch/Tick` 是会话自己的时钟，一个会话的失败只是它自己的失败；同 tick 的 `(subject, profile, full/delta)` 组件只编码一次，外层帧各自编码 | `sync/entitysync/wire.go:10`、[ARCH-11](../../bugfix/ARCH-11-view-priority-and-shared-encoding.md) |
| 状态帧能不能走可丢弃的 datagram | 不能：roost 的 delta 不是“自包含的最新状态”，被替换掉的 delta 是再也不会产生的变化。`AsyncTransport` 只留可靠通道 | [M-18](../../bugfix/M-18-async-transport-reliable-only.md)、RR-20260920-02 |
| 客户端会不会看到可能丢失的状态 | 不会（开了水位时）：pipelined 提交在 WAL 持久之前就确认，content 可能比崩溃后剩下的新；整个 subject 等水位 | U-0206、RR-20260926-35；`entity/subject_sync.go:176` 注释 |
| 水位谁接 | kit 自动接：业务没指定时装 committer 的 `DurableLSN`；committer 不支持 pipelined 时返回 `MaxUint64`（同步持久提交不产生 CommitLSN，门控无效果）。选自动接线而不是缺失时拒绝启动，是为了不让未手工接线的部署起不来 | `kit/nest/entity_sync.go:46`、`:72` |
| 首帧和登录应答赛跑 | 用 held 会话：先订阅、不出帧，客户端装好解码器后 `ReadySession`，第一帧是新 epoch 全量 | `sync/entitysync/subscriptions.go:81`、M-14 |
| 传输失败怎么分 | 两类：`ErrRetryLater`（谁也不怪，整 tick 重来）/ 其他（只关这个会话，`SessionLost` 告诉政策）。之前一个死会话撤不掉订阅会让全房间停发 | `sync/entitysync/errors.go:33`、RR-20260922-01 |
| 部分成功怎么办 | 已准入的帧不撤回，会话时钟保留；受影响且仍订阅的内容下次改发全量重建基线 | `sync/entitysync/flush.go:356` |
| Manager 要不要知道“会话 → subjects” | 只为恢复与关闭维护一份生命期反向索引（含待全量、待 remove），不用它做政策决定；政策的可见集归政策 | `sync/entitysync/session.go:47`、[资源预算与会话恢复](../../feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md) |
| 多个政策同时订同一对 | 每个政策实例一个独立来源，最后一个来源离开才退订；选优先级最高的一个视图，**不合并字段、不扩大授权** | RR-20260923-07（M-19）、`sync/entitysync/manager.go:32` |
| 框架替政策撤销 / 丢掉的订阅 | 政策并不知道：卸载重载不了而撤销的，同 id 重新登记后交还政策（Resubmit）；会话关闭 / 业务注销丢掉的，通知政策（Released，带戳只删更早的） | RR-20260926-70 / 78 / 79 / 85 |
| 快照预算限什么 | 只限冷创建；增量、remove、已持有对象的全量不限流。软预算，不是严格带宽上限；默认关闭 | `sync/entitysync/snapshot_budget.go:111`、[快照调度](../../feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md) |
| 冷创建公平性 | 新入场与 Hold 后恢复轮流使用额度；没有冷创建的会话按 ID 升序尝试——传输持续 RetryLater 时低 ID 总是先试 | 维护者 2026-09-27 接受（OPEN-ITEMS C29，RR-20260926-05），`sync/README.md:85` |
| 总线退订 | 排空在 `syncbus.Subscription` 实现一次，所有驱动共用；`Unsubscribe(ctx)` 返回 nil 才能释放 handler 的依赖 | A3 ②（[方案](../../feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md)） |
| 至少一次还是至多一次 | 普通 NATS 至多一次；需要推送一致性的调用方类型断言 `ILiveSubscriber`，拿不到就显式退化（例如 remoteentity 退化为按需读取） | `sync/syncbus/sync.go:44` |
| 帧同步为什么单所有者无锁 | 房间由一个 goroutine 驱动（demo 每场战斗一个），确定性来自“帧字节是提交的确定性函数” | `sync/lockstep/room.go:9`、`sync/lockstep/sequencer.go:313` |
| 玩家接入为什么是生成代码 | 接入层要按工程的协议、鉴权、配置定制；core 只给契约类型（`gateway.Principal` / `Session`）。auth.go 默认 fail-closed，`roost config enable player-tcp` 拒绝骨架 | `codegen/internal/roost/render_player_tcp.go:42`、`codegen/internal/roost/cli.go:399` |
| dispatch 超时与 Nest | 回退到 `nest.request_timeout`，避免传输层截止比 Nest 调用自己的预算还短（无理由截断） | RR-20260926-36、`codegen/internal/roost/render_player_tcp.go:246` |

→ [实现文档](../impl/04-sync.md)对应：§3 主流程、§9 历史与重要修复。

## 4. 怎么用

### 4.1 实体复制的最小装配

有两种装配，选一种。

**A. kit 装配（Manager 由 Nest Mod 管）**：`kit/nest.NewModWithEntitySync`（`kit/nest/entity_sync.go:19`）。

```go
nestMod := kitnest.NewModWithEntitySync(entityAccess, kitnest.EntitySyncSetup{
	Config: entitysync.ManagerConfig{
		Transport: myTransport,             // 必填：一次 Push 是一个会话的一帧
		SessionLost: onSessionLost,         // 可选：传输拒绝帧、Manager 关会话时告诉业务 / 政策（不得调用 Flush）
	},
	Configure: func(c *entitysync.ManagerConfig) { /* 最后覆盖，例如 c.SnapshotBudget */ },
})
// Init 之后：nestMod.EntitySync() 拿到 *entitysync.Manager，安装 Interest、登记实体、开会话
```

它做的事（`kit/nest/nest_mod.go`）：

| 阶段 | 动作 | 位置 |
| --- | --- | --- |
| Init | 合并配置：`Config` → 配置文件 `sync.entity.*`（非零才覆盖）→ `Configure`；没指定 `DurableWatermark` 时装上转发函数（Provide 之前返回 0、committer 不支持 pipelined 返回 `MaxUint64`）；`NewManager` | `kit/nest/entity_sync.go:25` |
| Provide | 把 Manager 作为 `NestOptionWithEntitySync` 交给 Nest（Nest 构造时 `BindSyncProducer`）；构造后接上水位来源 | `kit/nest/nest_mod.go:203`、`nest/nest.go:428`、`kit/nest/nest_mod.go:207` |
| Start | `Manager.Start` → `OnEntityLoaded(rebindEntitySync)`（DataEngine 支持时）→ `ConfigureUnloadResync`（getter 支持时）→ 启动 Nest；任一步失败逆序撤销 | `kit/nest/nest_mod.go:229` |
| StopWithContext | ①停卸载重载 worker 与 Nest（在 ctx 内等）→ ②任一没停完就返回、保留 → ③`Stop`（最后一次 Flush）→ `Drain` → `Close` | `kit/nest/nest_mod.go:310` |

**B. 业务自建（demo 的做法）**：game-demo 的 scene 自己 `entitysync.NewManager`，自己接水位、卸载重载、`OnEntityLoaded`（`demo/internal/service/game/scene.go.tmpl:254`、`:321`）。生成工程的 bootstrap 用的是普通 `kitnest.NewMod(EntityAccess)`，不是 `NewModWithEntitySync`。

```go
manager, err := entitysync.NewManager(entitysync.ManagerConfig{
	Transport:        sceneLane{transport},   // 一次 Push = 玩家 TCP 上的一次推送（msg 10103）
	Interval:         50 * time.Millisecond,
	DurableWatermark: dataEngine.DurableLSN, // 从 registry 查 mods.ModDataEngine
	SessionLost:      scene.sessionLost,
})
interest, err := policy.NewInterest(policy.InterestConfig{
	Manager: manager,
	AOI: policy.AOIConfig{Bounds: mapRect, BlockSize: 100, EnterRadius: 120, LeaveRadius: 150},
	Session: sessionFor,        // entity id → SessionID 的唯一换算
	Relations: []string{"team"},
})
// Start：ConfigureUnloadResync(manager) → OnEntityLoaded(rebindLoaded) → manager.Start(ctx)
// Close：停 resync → 摘钩子 → interest.Close() → manager.Close(ctx)
```

真实代码：`demo/internal/service/game/scene.go.tmpl:254`（构造）、`:321`（Start）、`:396`（Close）、`:454`（Join）。Transport 的错误分类（`ErrTransportUnavailable` → `ErrRetryLater`，其他 → 关会话）在 `:181`。

纯 Go 装配（不经 kit）还要自己把 Manager 交给 Nest：`nest.NestOptionWithEntitySync(manager)`（`nest/nest.go:317`），否则 `ModeOnChange` 的 `Start` 会报 `on_change requires a bound sync commit producer`（`sync/entitysync/manager.go:690`）。示例见 [sync-modes 实施记录](../../feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。

### 4.2 让实体可同步：生成标记与 packer

```go
//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryPlayer sync=true syncNamespace="player" subjectPacker=NewPlayerSyncPacker
```

（真实标记：`demo/game/entities/player/entity.go.tmpl:53`。）

- `sync=true` 让生成器给 builder 写 `Sync: entity.EntitySyncBuilderParam{Enabled: true, Namespace: ..., PackerFactory: ...}`（`codegen/internal/entity/gen.go:410`），并在业务没手写时生成 `TakeEntitySyncChanges()`（`:530`）：Nest 成功准入时自动消费 DAO 的客户端脏位，业务不用在末尾 Publish / Flush。
- `syncNamespace` 只接受字符串字面量或带包名的常量；裸标识符被拒（RR-20260918-07，`codegen/internal/entity/parse.go:767`）。
- 两种 packer 标记（`subjectPacker` / 旧拼写 `syncPacker`）互斥；没写 `sync=true` 写 packer 报错（`codegen/internal/entity/parse.go:249`）。
- DAO 字段 `dao:"sync"` 进入 `MarshalSync(mask)`（只含 mask 命中的字段，BSON）/ `ApplySync`（`codegen/internal/dao/template_dao.go:709`）。字段位只在本次生成的 schema 内稳定，不是 ABI。
- 多 DAO 实体：实现 `entity.SyncChangeMapper` 显式映射字段位；否则发 Full，避免两个 DAO 的 bit 0 被合并（`entity/sync_commit.go:291`）。
- 手写同步字段：`EntityBase.MarkSyncDirty(mask)` / `MarkSyncFullDirty(reason)`（`entity/entity_base.go:214`），在正式事务作用域内暂存到成功准入。

**视图（profile）**：用 `entity.NewSyncViewSet` 列出视图、`entity.NewMaskedSyncPacker` 按白名单编码（`entity/sync_view.go:32`、`:86`）。snapshot 按视图白名单编码，delta 编码 `mask & view.Fields`；未知视图报 `ErrSyncViewUnknown`，不回退。demo 的 packer：`demo/game/entities/player/sync_packer.go.tmpl`（default / near / far 三个视图）。视图的 `Priority` 必须等于 Manager 的排名（`ManagerConfig.ProfilePriorities` 里配的值，未配置时等于 LOD），`Interest` 构造时与每次订阅时都会检查（`sync/entitysync/subject.go:140`、`sync/entitysync/policy/profiles.go:16`）。

字段白名单、显式优先级、来源视图的配置细节见 [Sync 视图配置](../../feature/SYNC-PROFILES.md)。

### 4.3 会话与订阅

```go
_ = manager.OpenHeldSession(sid)          // 登录时：可订阅、不出帧
_ = manager.Register(player.Sync())       // 登记 subject（或 RegisterAfterRetirement，见 4.6）
interest.Enter(playerID, at)              // 政策决定订阅
// …客户端装好解码器后发 scene_ready…
_ = manager.ReadySession(sid)             // 下一 tick 发新 epoch 的全量帧
// 重连 / 客户端重置：
_ = manager.HoldSession(sid)              // 全部订阅回到“欠全量”，等 ReadySession
// 离开：
_ = manager.Unregister(subjectID)         // 退役：持有对象的会话先收到 remove
manager.CloseSession(sid)                 // 丢会话状态，不欠任何帧
```

| 调用 | 语义 | 可能的错误 |
| --- | --- | --- |
| `OpenSession(id)` | 幂等；Transport 实现 `SessionLifecycle` 时，`SessionOpened` 成功后才对订阅 / Flush 可见；不等待，可在快池调用 | `ErrSessionClosing`（同 id 旧发送仍在退出）、`ErrSessionOpening`（另一次打开在等确认）——两者 `SessionOpenRetryable` 为真，**重试的退避由调用方安排在快池之外**（demo：25ms 起翻倍、上限 1s、至多 8 次）；`ErrSessionLimit`、`ErrManagerClosed` |
| `OpenHeldSession` / `HoldSession` / `ReadySession` | 见上 | `ErrSessionUnknown` |
| `CloseSession(id)` | 从每个 subject 删订阅，不欠 remove；政策应已自己忘掉它，这是兜底 | — |
| `Subscribe` / `Unsubscribe`（默认来源） | 同来源重复订阅幂等、换 profile 发全量；最后一个来源离开：持有对象或首次 create 在途的会话欠 remove，其他直接删除 | `ErrSessionUnknown`、`ErrSubjectNotRegistered`、`ErrSubjectRetiring`、`ErrSubscriberLimit`、`ErrSubscriptionNotFound` |
| `NewSubscriptionSource()` / `...WithResubmit` / `...WithHooks` | 每个独立所有者保留一个来源令牌（不要每次新建） | — |

真实用法：demo `Join`（`demo/internal/service/game/scene.go.tmpl:454`）——已开的会话 `HoldSession`，没有的 `OpenHeldSession`，可重试错误进退避；`Ready`（`:544`）；`sessionLost`（`:732`）。

### 4.4 选一种政策

| 政策 | 用于 | 关键 API | 失效后的恢复 |
| --- | --- | --- | --- |
| `Interest` | 开放世界：距离 + 关系 | 手动：`Enter` / `Move` / `Leave` / `Show` / `Hide` / `Relation(name).Set` 然后 `Apply()`；排队：`QueueMove` / `QueueRelation`（在 handler 的实体锁内调用，提交确认后才生效） | `SessionLost` 后会话重开：`Resubscribe(observer)`；卸载重载撤销：自动 Resubmit |
| `Group` | 小集合全互见（队伍频道、副本） | `AddSubject` / `RemoveSubject` / `Join` / `Leave` / `Close` | 只有 Resubmit；**会话被 Manager 关闭后重开，没有恢复路径**（见 §7.3） |
| `Direct` | 一对一显式绑定 | `Bind` / `Unbind` | Resubmit + Released（带戳只删更早的绑定） |

要点：

- `Interest.Apply()` 返回 `[]Refusal`：被拒的订阅每次 Apply 再说，直到被接受或 pair 释放；`Refusal.Retry` 供调用方分日志级别（`sync/entitysync/policy/interest.go:379`）。排队模式下 Refusal 被丢弃、不记日志（`sync/entitysync/policy/interest_queue.go:108`）。
- 排队事实（`QueueMove`）仓内唯一的生产式用法在 `scripts/perf/sync-aoi/nest.go:85`；demo 用的是手动模式（`Moved` 在提交后用请求坐标更新，`demo/internal/service/game/scene.go.tmpl:575`）。
- `Group.Close` / `Interest.Close` 只释放本实例的订阅，不注销实体；实体销毁由应用调用 `Manager.Unregister`（RR-20260923-07）。
- 自定义政策：用 `manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{Resubmit, Released})` 拿来源，`manager.RegisterPolicy(apply, pending)` 安装政策阶段处理器（`sync/entitysync/policy_queue.go:18`）。三条硬约束：不得调用 Flush、不得阻塞、不得读取未冻结的业务字段。

### 4.5 两种同步模式

| | `periodic`（默认） | `on_change` |
| --- | --- | --- |
| 何时捕获 | 每 `Interval`（默认 50ms）Flush 时在实体锁内捕获 | 成功准入且 Guard 仍持有实体锁时冻结；全部实体解锁且提交确认后 `WakeSync` 唤醒；`Interval` 兜底积压与恢复 |
| 快照预算窗口 | 每次 Flush 重置 | 同一个 `Interval` 窗口共享 |
| 冻结内存 | — | `max_frozen_bytes`（默认 64MiB）；超出时该 subject 留脏、下次重捕获 |
| 前提 | — | 必须有绑定的提交生产者（Nest option 自动绑定） |

两种模式共用内容、订阅、版本、线协议（`sync/entitysync/mode.go:11`）。第一版按 Manager 配置、启动后固定；回退方式是停接流、排空、以 periodic 重启。`Interval` 只是合并检查周期，**不是端到端延迟上限**（`sync/entitysync/manager.go:17`）。

### 4.6 实体退役、同 tick 重连、卸载后重载

- **退役**：`Unregister(id)` 后订阅该 subject 被拒（`ErrSubjectRetiring`）；全部 remove 发出（或最后一个订阅者会话关闭）后 subject 被遗忘，之后同 id 才能重新登记——客户端永远先收 remove 再收 create。
- **同一 tick 内离开又回来**：`RegisterAfterRetirement(state, done)`——退役中时把登记排到退役完成那一步，`done` 恰好报告一次（nil / `ErrRegistrationCancelled` / `ErrManagerClosed`）；每个 subject 至多一个排队、后到的替换先到的（`sync/entitysync/manager.go:403`）。`done` 跑在 Manager 自己的调用路径上，不得阻塞、不得调用 Flush，要做事就交给自己的 goroutine（demo：`registeredLater`，`demo/internal/service/game/scene.go.tmpl:518`）。
- **事务内新建后被回滚**：Nest 在仍持有实体锁时调用 `RetractSyncSubject`，只撤这一个状态对象的登记，语义同 `Unregister`（RR-20260926-35、RR-20260927-28）。
- **仅内存卸载**（DataEngine 驱逐被 lease fence 跳过的原生步骤、Remote 持久拒绝后的实例）：状态立即关闭、subject 保持登记、订阅者保留原对象；`ManagerAccess.ConfigureUnloadResync` 在快池之外从权威重载并 `Rebind`，订阅者收到同一对象的全量；权威里没有、重试用尽或队列满时退回 remove。kit 的 `NewModWithEntitySync` 自动接线，自建 Manager 要自己接（demo `Start`）。配置见 §5.2。

### 4.7 客户端解码

一次推送就是一个完整的 `frame.Frame`：`entitysync.DecodeFrame(data, limits)`，对每个对象的组件调 `entitysync.DecodeSubjectUpdate`（`sync/entitysync/wire.go:78`、`:114`）。按 `Namespace` 分流到实体类型，按 `ObjectRef`（id + generation）维护本地对象表；`ObjectCreate` 建、`ObjectUpdate` 带 Full 时整份替换、不带时按 `Mask` 应用增量、`ObjectRemove` 删。机器人客户端实现：`scripts/perf/sync-aoi/client.go`。

### 4.8 服务间总线（syncbus）

装配：在服务的 Mod 列表里放 `kitsyncbus.NewSyncBusMod(0)`（生成器在 `codegen/internal/roost/catalog.go:74` 写入，依赖 nats Mod），然后从 Registry 取 `syncbus.ISyncBus`（能力名 `mods.ModSyncBus`）。

```go
bus := app.MustLookup[syncbus.ISyncBus](registry, mods.ModSyncBus)

sub, err := bus.Subscribe("my_topic", func(ctx context.Context, msg *syncbus.SyncMsg) error {
	// msg.Data == nil 表示删除；按 msg.Version 自己丢弃旧消息
	return apply(msg)        // 返回错误只记日志、不重投（JetStream 下照样 ACK）
})
// 停机：三步。返回 nil 后 handler 用到的依赖才能释放；超时返回 ctx 错误，可用新 ctx 重试
if err := sub.Unsubscribe(ctx); err != nil { return err }

// 需要“订阅确认之后至少一次”时（只有 JetStream 有）：
if live, ok := bus.(syncbus.ILiveSubscriber); ok {
	sub, err = live.SubscribeLive("my_topic", handler)
} else {
	// 显式退化，例如改成按需读取并记日志（remoteentity/snapshot_client.go 的做法）
}

err = bus.Publish(&syncbus.SyncMsg{MessageID: ids.Next(), Topic: "my_topic", Key: id, Version: v, Data: payload})
```

规则：

- **handler 里退订自己**必须把投递 ctx（或它的派生）传给 `Unsubscribe`，否则会等自己：带期限的 ctx 到期报错，`Background` 会一直等（`sync/syncbus/subscription.go:87`）。
- **每次发布一个新的 `MessageID`**（`syncbus.NewDeliveryIDs(kind).Next()`，`sync/syncbus/delivery_id.go:33`）：JetStream 用它作去重键（`Nats-Msg-Id`），它与业务 `Version` 无关。用版本拼去重键会让同版本的第二条被吞掉（U-0009）。
- **发布可能结果不确定**：JetStream 发布超时后消息可能已经入流；重试会换新 `MessageID`，订阅方要按版本 / 序号准入，不能靠 broker 去重。
- **本服消息不回送**：两种驱动都跳过 `FromSid == LocalSid` 的消息（`sync/syncbus/driver/jetstream.go:256`、`sync/syncbus/driver/nats.go:98`）。
- 有 ctx 的发布用 `syncbus.IContextPublisher` 断言；两种驱动都实现。

**两种驱动的语义差别**

| | `transport: nats`（默认） | `transport: jetstream` |
| --- | --- | --- |
| 投递 | 至多一次；同 topic 的多个本地订阅各自一个 NATS 订阅 | 持久：每个 (topic, sid) 一个 durable consumer，在本地按订阅 id 顺序扇出给全部本地订阅者（RR-20260916-03） |
| handler 出错 / panic / 反序列化失败 | 记日志 | 记日志并 **ACK**（不重投） |
| `SubscribeLive` | 不支持 | `DeliverNew` 的独立 durable（`<topic>.live`） |
| 总线停止 | 无停止状态，靠 NATS Mod drain | `StopWithContext`：关投递准入（之后到达的 NAK 交还 broker）、停 consumer、在 ctx 内等在途回调；之后拒绝 Subscribe，**Publish 仍可用** |
| 投递并发 | 每个订阅一个 goroutine | 同一 consumer 内串行；一个慢 handler 拖住该 topic 后面所有投递 |

**副本复制**

- `mirror.New(bus, topic, store)` / `mirror.NewLive(...)`：`Envelope{topic,key,version,op,payload,updated_at}` 的 upsert / delete，外层身份为准、内层不一致拒绝（`sync/syncbus/mirror/envelope.go:69`）。remoteentity 的快照、兴趣、续租三条主题都用它（05 分区）。
- `cache.NewReplicaSyncer(bus, cache.ReplicaConfig{Store, Topic, KeyOf, VersionOf, UpdatedAtOf, DeleteKeyOf})`：把副本写进 `cache.Store`，写入前校验 payload 的 key / version 与信封一致（`cache/mirror.go:88`）。`Stop(ctx)` 等在途写完。
- `syncbus.NewPatchSyncer(bus, PatchSyncerConfig{Topic, LocalSid, KeyOf, VersionOf, WithKey, HasData, Apply})`：瞬时 patch 的跨服复制，JSON，**没有删除语义**（Data 为空的消息被忽略，`sync/syncbus/patch_syncer.go:120`）。

`ReplicaSyncer` 与 `PatchSyncer` 在仓内**没有生产调用方**，只有测试；用之前读 §7.3 的已知限制。

### 4.9 有序持久流（syncstream）

syncstream 是**块外基建**（ARCH-12 定性），唯一的生产消费者是 `skill/skillsync`（08 分区）。它给每个 (Observer, Stream) 一条序号连续的包链，配 ACK、断线续传判断与持久化：

```go
journal, err := syncstream.NewFileHistoryJournal(dir, 0)           // 0：随机 epoch
history, err := syncstream.NewHistoryWithJournal(syncstream.HistoryOptions{
	MaxPacketsPerStream: 1024, MaxPayloadBytes: 1 << 20, MaxStreams: 100000,
	IdleTTL: 30 * time.Minute, PruneAcknowledged: true,
}, journal)                                                          // Load → Import，不自动做首个 checkpoint
packet, err := history.Append(syncstream.Packet{Observer: o, Stream: s, Full: true, Payload: p}) // 先写 WAL 再改内存
err = history.AcknowledgeEpoch(o, s, packet.Epoch, packet.Sequence)
result, err := history.Recover(syncstream.ResyncRequest{...}, provider) // 需要全量时锁外调 provider 再补一个 Full
err = history.Checkpoint()                                            // 宿主自己定期调用，核心不自动做
pub, err := syncstream.NewPublisherWithOptions(bus, syncstream.PublisherOptions{RequireConfirmation: true, CompressionThreshold: 4 << 10, MaxFrameBytes: 256 << 10})
sub, err := syncstream.SubscribeWithOptions(bus, topic, syncstream.SubscribeOptions{RequireChecksum: true}, handler)
```

真实用法：`skill/integration/sync-e2e/e2e_test.go:75`；生产基线见 [visual-sync 生产指南](../../skill/visual-sync-production-guide.md)（其中部分路径与说法已过时，见 §8 与[实现文档 §10.3](../impl/04-sync.md#103-文档与源码不一致)）。

要点：

- `Record` 返回 nil 即 write + fsync 已完成；Write / Sync / checkpoint 发布任一出错即 **fail-stop**（`ErrHistoryJournalFailed`），之后只能重开（`syncstream/file_journal.go:332`）。仓内没有代码自动重开或升级为 App 级 fail-stop。
- **checkpoint 要宿主定期调用**：核心不自动做，WAL 只增不减，启动回放时间与内存都与 WAL 大小成正比（仓内只有 e2e 测试调用）。
- `Recover` 只在 History 未变时提交（全局 `revision`），否则返回 `ErrRecoverStale`，调用方应重试。
- 订阅端是信任边界：分片数、重组字节、解码字节、checksum、严格 JSON 都有上限 / 校验（`syncstream/publisher.go:250`）。

### 4.10 帧同步（lockstep）

帧同步与状态同步并列，用于战斗这类“同一输入、各端确定性模拟”的玩法。一个 `Room` 由**一个 goroutine** 独占驱动：

```go
room, err := lockstep.NewRoom(lockstep.RoomConfig{
	Sequencer:          lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}, SubmitWindow: 2, MaxInputBytes: 8},
	RedundancyDepth:    3,           // 每个广播包带最近 3 帧，自愈 2 个连续丢包
	CatchupBatchFrames: 8,           // 追帧每 tick 一页；1 被拒（永远追不上）
	Datagrams:          datagramSender,  // 必填：实时广播
	Reliable:           reliableSender,  // 可选：追帧页
	OnDesync:           func(v lockstep.DesyncVerdict) { /* 记录离群玩家 */ },
})
_ = room.Attach(player, session)               // 一个会话只服务一个接收者
_, _ = room.SubmitInput(player, frame, payload) // 迟到的折入当前帧；64 帧内重传幂等
frame, err := room.Tick(ctx)                    // 每逻辑帧调一次：切帧、进历史、冗余广播、推进追帧
_ = room.StartCatchup(player, from)             // 重连：从 from 起经可靠通道分页补发
_ = room.ReportHash(player, frameID, hash)      // 关键帧哈希；过 quorum 的多数派裁决离群者
room.TrimHistory(keep); room.TrimHashReports(before)   // 历史与哈希表都要调用方回收
```

真实用法：`demo/internal/service/game/battle.go.tmpl`（每场战斗一个 goroutine，命令通道容量 256，`submit` 非阻塞，满了返回 `ErrBattleBusy`）；参数在 `demo/game/battle/battle.go.tmpl`；客户端是 `robot/lockstep.go` 的 `LockstepBot`（`FrameAssembler` 去重、按序放帧）。

注意：

- 广播在 `Tick` 里同步发送；追帧页在房间 goroutine 之外发送，每会话至多一页在途、每页限时 `CatchupSendTimeout`（2s），`Tick` 对本 tick 发起的页最多共等 `CatchupSendWait`（5ms）。`Reliable` 直接接 KCP / QUIC、ctx 不带期限也不会被慢客户端卡住；自定义 `ReliableSender` 须并发安全（v1.23.1 起，RR-20261006-63）。
- `NewRoom` 的最坏包预算是**载荷**上限：不写 `MaxDatagramBytes` 时取发送器声明的值（`nettransport.DatagramPayloadLimiter`：KCP / QUIC 取配置，UDP 取包上限减 32 字节信封），发送器不声明时 1200；写了超过声明值的被拒（v1.23.1 起，RR-20261006-62）。
- 哈希裁决：有哈希达到 quorum（缺省过半）就按多数裁；全部座位已报、有分歧且无哈希达到 quorum（2 人房哈希不同、2:2、全不同）时裁 `NoMajority`，全部座位都是离群者，应按整局分叉处理（v1.23.1 起，RR-20261006-64）。

### 4.11 玩家 TCP 接入层

玩家接入层是**生成代码**，core 只提供契约类型 `gateway.Principal` / `gateway.Session`（`gateway/gateway.go:17`、`:30`）与 App 生命周期。

**开通步骤**（12 分区讲生成器本身）：

```bash
roost add access player --service game     # 生成 access.player Mod 与 player_agent 协议注册表，给服务补 nest Mod
roost add transport tcp                     # 生成 access.player.tcp Mod、auth.go 骨架、playerprobe，补 player_access.tcp 配置（enabled: false）
# 实现 internal/access/player/tcp/auth.go 的 Authenticator（骨架默认拒绝一切）
roost config enable player-tcp              # 先用 AST 判断 auth.go 已不是骨架，再只改 enabled 这一个标量
roost project doctor --workflow player-tcp
read -rsp 'ticket: ' ROOST_PLAYER_TOKEN; export ROOST_PLAYER_TOKEN
go run ./cmd/playerprobe -addr 127.0.0.1:7000   # 只验证握手
```

或者直接生成 game-demo：`roost project new planet -module example.com/planet -mods configdata,mongo,nats,dataengine,nest -template game-demo`（与 CI 的 `.github/workflows/demo-publish.yml` 相同）。

**生成出来的文件**

| 文件 | 归属 | 内容 |
| --- | --- | --- |
| `internal/access/player/tcp/server_gen.go` | 生成器 | Mod `access.player.tcp`：配置、监听、握手、会话表、读循环、推送、`OnSessionClosed`、停机 |
| `internal/access/player/tcp/auth.go` | **业务**（`project sync` 不覆盖） | `Authenticator.Authenticate(ctx, ticket, remoteAddr) (gateway.Principal, error)` |
| `internal/access/player/mod_gen.go` | 生成器 | Mod `access.player`：建协议注册表、装 WriteGate 中间件、注册协议后 Seal |
| `game/player_agent/runtime_gen.go` | 生成器 | `ProtocolRegistry`（`Dispatch` / `Encode` / `RegisterProtocol` / `RegisterNotify`） |
| `cmd/playerprobe/main.go` | 生成器 | 握手冒烟 |
| `docs/PLAYER_ACCESS_TCP.zh-CN.md` | 生成器 | 接入指南 |

**写 controller 的硬规则**：任何业务失败都**编码成响应里的 `Code`**（`errcode.Define` 定义），不要以 `error` 离开 controller——读循环对 dispatch 返回的 error 一律断连、只打 Debug 日志（`codegen/internal/roost/render_player_tcp.go:859`）。样板：`demo/game/controllers/player/add_item.go.tmpl`。WriteGate 拒绝、未知消息号、解码失败、handler panic 也走这条断连路径。

**一次请求的路径**

```
accept ─(全局槽 max_connections、单 IP 上限)─▶ 握手槽(max_handshakes，等待计入 handshake_timeout)
  ─▶ 读鉴权帧(≤ max_handshake_bytes) ─▶ Authenticate ─▶ 写 ack ─▶ 登记会话(同 SessionID 替换旧会话)
  ─▶ 读循环：每帧刷新 idle_timeout；sequence 严格递增
       ─▶ Dispatch(ctx = 连接 ctx + dispatch_timeout) ─▶ 中间件 Recover → WriteGate → handler
       ─▶ 有响应：用连接 ctx + write_timeout 写回；dispatch 返回 error：断连
```

- **dispatch 超时只限制等待**：handler 不配合 ctx 时，读循环一直等它返回；同一连接同一时刻至多一个在途请求，所以同连接请求天然保序（`codegen/internal/roost/render_player_tcp.go:843`）。
- **登录**（demo `EnterGame`，msg 10002）在 dispatch 预算之上再叠一段 `login_timeout`，用尽回 `login_timeout`（100021）而不是断连（`demo/game/controllers/player/enter_game.go.tmpl:143`）。
- **推送**：`Runtime.PushPlayer(ctx, playerID, messageID, value)` 对该玩家的每个连接逐个写；只要有一个收到且没有“写前拒绝”就返回 nil（RR-20260926-52）。写了一半或 write_timeout 到期的连接被关；调用方截止导致 0 字节写出算写前拒绝、不关连接（RR-20260926-68）。传输未启动 / 已停：`ErrTransportUnavailable`——entitysync 的 Transport 要把它包成 `ErrRetryLater`（demo `sceneLane.Push`）。
- **会话关闭事件**：`Runtime.OnSessionClosed(fn)`，专属 goroutine 顺序派发、每个订阅者单独 recover；队列 256，**满了丢弃**（计数 `player_tcp_session_closed_dropped_total`），消费者必须有对账兜底（demo 的 scene / presence 先查 `ActiveSessions(pid) > 0`）。
- **踢下线**：`Runtime.CloseSessions(pid, reason)`，会取消该连接在途的 dispatch（`context.Cause` 是 reason）。
- **多端**：同一 `SessionID` 才互相替换；demo 认证器每个连接铸造新 SessionID（`demo/internal/access/player/tcp/auth.go.tmpl:97`），所以同一玩家允许多个连接同时在线。
- **没有心跳和限流**：帧协议没有 ping，只收推送的客户端会被 `idle_timeout`（默认 90s）踢掉，客户端要周期性发一个已注册的请求；生成层不按玩家 / 消息限流，`gateway.RateLimit` 的类型与 `player_agent` 中间件不兼容、也没有注入点（见 §7.3）。

→ [实现文档](../impl/04-sync.md)对应：§3 主流程、§7 协议格式。

## 5. 配置

配置键都由读它的 Mod 声明，App 在任何 Mod Init 之前按声明检查类型、枚举、`min`（规则见 [07 分区](07-config.md)）；范围类的业务校验在 Mod Init 里。

### 5.1 `sync.entity.*`（kit/nest，只在 `NewModWithEntitySync` 装配时生效）

声明在 `kit/nest/nest_mod.go:116`；`ConfigSchema` 总是声明它们，但只有 `syncSetup != nil` 时才读（`kit/nest/entity_sync.go:26`）。优先级：`EntitySyncSetup.Config` → 配置文件（非零才覆盖）→ `EntitySyncSetup.Configure`。

| 键 | 类型 / 默认 | 范围与校验 |
| --- | --- | --- |
| `sync.entity.mode` | string，缺省 `periodic` | 枚举 `periodic` / `on_change`（声明 + `ParseSyncMode`，`sync/entitysync/mode.go:19`） |
| `sync.entity.interval` | duration，0 取 `DefaultInterval` 50ms | `min:"0"`；负数在 `NewManager` 拒绝（`sync/entitysync/manager.go:211`） |
| `sync.entity.max_frozen_bytes` | int64，0 取 64MiB | `min:"0"`；只对 `on_change` 有意义 |

### 5.2 `nest.unload_resync.*` 与 `nest.entity_load_timeout`（卸载后重载，RR-59）

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `nest.unload_resync.workers` | 0 → 4 | 后台重载 worker 上限；队列空就退出 |
| `nest.unload_resync.attempts` | 0 → 5 | 单个实体的重载尝试次数，退避 100ms → 2s（不可配） |
| `nest.unload_resync.queue_capacity` | 0 → 4096 | 队列满时**立即**退回 remove（计 overflow） |
| `nest.entity_load_timeout` | 0 → 框架默认 | 一次共享冷加载的上限；非 0 时 getter 必须支持 `ConfigureLoadTimeout` |

声明：`kit/nest/nest_mod.go:109`；默认值：`entity/unload_resync.go:70`。最坏延迟的估算公式在两处写法不同（`kit/nest/nest_mod.go:113` 的 help 与 `entity/unload_resync.go:51` 的注释），见[实现文档 §10.3](../impl/04-sync.md#103-文档与源码不一致)。

### 5.3 `syncbus.*`（kit/syncbus）

声明在 `kit/syncbus/mod.go:25`。A4 ① 起只读 `syncbus` 段，旧 `room:` / `sync:` 段不再读取（`kit/syncbus/mod.go:22`）。nats 模式下 JetStream 专属键被静默忽略。

| 键 | 默认 | 范围 / 说明 |
| --- | --- | --- |
| `syncbus.transport` | `nats` | 枚举 `nats` / `jetstream` / `js`；写错 Init 失败（RR-20260926-12） |
| `syncbus.prefix` | `roost.room`（Mod 缺省，`kit/syncbus/mod.go:20`）；生成配置写 `roost.sync` | 主题前缀，subject 为 `<prefix>.<topic>` |
| `syncbus.stream` | 空 → 由 prefix 派生 | `roost.sync` / `roost.room` → `ROOST_SYNC`；其他 prefix 大写并把 `.` 换 `_`，非常规字符加摘要（`sync/syncbus/driver/jetstream.go:364`）。只在要沿用已有流与 consumer 游标时写 |
| `syncbus.storage` | 空 → `file` | 枚举 `file` / `memory` |
| `syncbus.ack_wait` | 0 → 10s | handler 超过它才返回会被重投 |
| `syncbus.max_deliver` | 0 → 5 | 到达后 **Term**（计 `nats.jetstream.terminal.total`） |
| `syncbus.stream_max_age` | 0 → 30m | 流内消息保留时长；过期的消息 live 订阅也收不到 |
| `syncbus.duplicates` | 0 → 2m | broker 去重窗口 |
| `syncbus.replicas` / `syncbus.max_bytes` | 0 → 透传服务端缺省 | |
| `syncbus.setup_timeout` | 0 → 5s | 建流 / 建 consumer 的期限 |
| `syncbus.publish_timeout` | 0 → 5s | 单次发布等 PubAck 的上限 |

不可配置：`MaxAckPending`（服务端缺省 1000）、`NakBackoff`、consumer 的 `InactiveThreshold`、预取批量（nats.go 缺省 500）。

### 5.4 `player_access.tcp.*`（生成的接入层）

声明在 `codegen/internal/roost/player_tcp_config.go:31`（唯一来源），范围检查在生成的 `validateConfig`（`codegen/internal/roost/render_player_tcp.go:266`）——App 预检只查类型，范围在 Mod Init 时检查。

| 键 | 默认 | 范围 |
| --- | --- | --- |
| `enabled` | false | `roost config enable / disable player-tcp` 只改这一个标量 |
| `addr` | `0.0.0.0:7000` | host:port；部署模板端口写死 7000，改了要手工同步 |
| `max_connections` | 10000 | 1..1e6 |
| `max_connections_per_ip` | 128 | >0 且 ≤ max_connections |
| `max_handshakes` | 1024 | >0 且 ≤ max_connections |
| `max_handshake_bytes` | 8192 | 1..65536 |
| `max_payload_bytes` | 1048576 | 1..16MiB |
| `handshake_timeout` | 5s | (0, 1m]；含等握手槽的时间 |
| `idle_timeout` | 90s | (0, 24h]；只由入站帧刷新 |
| `write_timeout` | 5s | (0, 1m] |
| `shutdown_timeout` | 10s | (0, 5m]；同时是这个 Mod 的 `StopBudget`，计入生成的 `shutdown.total_timeout`（RR-20260927-05）。**写 0 不会回落到 10s，进程拒绝启动**（见 §7.3） |
| `dispatch_timeout` | 不写或 0：取 `nest.request_timeout`，再取 3s | (0, 5m] |
| `login_timeout` | 不写或 0：取 `min(2s, dispatch_timeout)` | >0 且 ≤ dispatch_timeout |

注意：`roost add transport tcp` 补键时会把 `dispatch_timeout` 写成显式值（等于当时的 `nest.request_timeout` 或 3s），之后调大 `nest.request_timeout` 不会跟着变（见 §7.3）。

### 5.5 Go 结构配置（不在配置文件里）

| 结构 | 关键字段与默认 | 位置 |
| --- | --- | --- |
| `entitysync.ManagerConfig` | `Transport`（必填）、`Mode`、`Interval` 50ms、`MaxFrozenBytes` 64MiB、`ProfilePriorities`（重复报错）、`SnapshotBudget`（默认关）、`Limits`（0 取 `frame.DefaultLimits`）、`DurableWatermark`、`MaxSubjects` / `MaxSessions` 65536、`MaxSubscribersPerSubject` 1024、帧上的线常量、`SessionLost`、`Trace` | `sync/entitysync/manager.go:22` |
| `frame.Limits` | `MaxObjects` 100（也是一个会话可持有的 subject 数）、`MaxComponentsPerObject` 64、`MaxComponentBytes` 64KiB、`MaxFrameBytes` 4MiB；Transport 实现 `FrameSizeLimiter` 时取较小者 | `sync/frame/frame_limits.go:27`、`sync/entitysync/manager.go:233` |
| `entitysync.SnapshotBudget` | `MaxObjects` / `MaxBytes` / `PerSessionObjects`，0 不限 | `sync/entitysync/snapshot_budget.go:119` |
| `policy.InterestConfig` | `MaxQueuedFacts` 0 → 65536；`SourceProfiles`、`ViewSets`、`Relations`（不能空、不能重复、不能用保留名）、`SelfVisible`（nil 视为 true） | `sync/entitysync/policy/interest.go:23` |
| `policy.AOIConfig` | `EnterRadius > 0`、`LeaveRadius ∈ [Enter, 2^62)`、`Bands` 严格递增且 > 0、`BlockSize > 0`、`MaxObserverBlocks` 0 → 1024（构造期按最坏格数检查）、`MaxVisible` | `sync/entitysync/policy/aoi.go:47` |
| `nettransport.AsyncTransportConfig` | `MaxSessions` 4096、`ReliableQueueSize` 256、`MaxReliableBytes` 1MiB、`SendTimeout` 250ms（≤0 取默认、无法关闭）；`MaxQueuedReliableBytes` / `MaxResidentReliableBytes` / `MaxReliableAge` 0 表示关闭、负数拒绝 | `sync/nettransport/channel.go:29` |
| `lockstep.RoomConfig` | `RedundancyDepth` 3（≤64）、`MaxDatagramBytes` 取发送器声明值（否则 1200）、`HashQuorum` players/2+1、`CatchupBatchFrames` 32（1 被拒、>64 截断）、`CatchupMaxFailures` 8、`CatchupSendTimeout` 2s、`CatchupSendWait` 5ms | `sync/lockstep/room.go:46` |
| `syncstream.HistoryOptions` | `MaxPacketsPerStream` 256、`SchemaVersion` 1、其余 0 不限；`PruneAcknowledged` 会写进 ACK 记录 | `syncstream/syncstream.go:89` |

demo 的常量：scene Interval 50ms、进入 / 离开半径 120 / 150、关系 `team`、地图 1000×1000、BlockSize 100（`demo/internal/service/game/scene.go.tmpl:43`）；战斗 TickRate 30ms、45 帧、关键帧间隔 15、SubmitWindow 2、MaxInputBytes 8、冗余 3、追帧 8（`demo/game/battle/battle.go.tmpl:25`）。

## 6. 运行与运维

### 6.1 指标

| 来源 | 指标 | 含义 / 怎么看 |
| --- | --- | --- |
| entitysync | `entitysync_flush_duration`（histogram） | 每次 Flush 的耗时（含政策阶段、捕获、编码、准入） |
| | `entitysync_frames_admitted_total` | 已准入的帧数 |
| | `entitysync_sessions_lost_total` | 因传输拒绝帧被 Manager 关闭的会话数（不含 `CloseSession`） |
| | `entitysync_durability_gate_deferred_total` | 因水位未到被整体暂缓的 subject 次数；持续增长 = DataEngine 持久化落后 |
| | `entitysync_full_captures_total{reason=dirty\|resync\|schema\|other}` | 全量捕获的原因（固定标签集） |
| entity | `entity.unload_resync.backlog`（gauge） | 卸载重载队列积压（进程内全部接线之和） |
| syncbus | 自身无指标；底层 `nats.jetstream.terminal.total{reason=permanent\|max_deliver}`、`nats.jetstream.settle_failures.total{op}`、`nats.subscription.handler_panic.total` | `terminal` 增长 = 有消息被 Term、永久丢失 |
| syncstream | `syncstream_epoch` / `_streams` / `_retained` / `_dropped` / `_pending`；`roost_sync_packets_published` / `_frames_published` / `_publish_failures` / `_queue_backpressure` / `_queue_depth_total` | 只在宿主调用 `ExportMetrics(sink)` 时输出；`roost_sync_queue_depth_total` 实际是累计入队次数，不是深度 |
| lockstep | `frame.total`、`input.late.total`、`input.rejected.total{reason}`、`catchup.frames.total`、`desync.total` | **没有 `lockstep.` 前缀、没有房间标签**——与 `OBSERVABILITY.md` 写的 `lockstep.*` 不一致（见 §7.3） |
| 玩家 TCP | `player_tcp_connections`（gauge）、`player_tcp_connection_rejected_total{reason?}`、`player_tcp_auth_failure_total{reason?}`、`player_tcp_frame_error_total`、`player_tcp_dispatch_duration`、`player_tcp_dispatch_timeout_total`、`player_tcp_dispatch_error_total`、`player_tcp_push_total`、`player_tcp_push_error_total`、`player_tcp_push_no_session_total`、`player_tcp_push_closed_total`、`player_tcp_write_error_total`、`player_tcp_session_closed_dropped_total`、`player_tcp_session_closed_callback_panics_total` | `dispatch_error_total` 非零不只是“端点把业务错误当 error 返回”，还包括 WriteGate 拒绝、未知消息号、解码失败、panic、ctx 取消 |
| demo | `scene_session_reopen_failed_total{reason=exhausted\|refused}` | 玩家留在场景里但没有自己的视图 |

`Manager.Stats()` / `Counters()`（只读原子量）/ `AuditStats()`（遍历订阅真相，静止时核对用）给出更细的数：`HeldSessions`、`PendingSnapshots`、`WaitingSnapshotSubjects`、`OldestSnapshotWait`、`FlushFailures`、`FrozenBytes` 等（`sync/entitysync/manager.go:902`、`sync/entitysync/counters.go:7`）。`LastError()` 是最近一次 Flush 的失败原因——**周期循环吞掉 Flush 的错误**（`sync/entitysync/manager.go:737`），失败只能从 `FlushFailures` / `LastError` 看。

### 6.2 健康与就绪

| 检查项 | 判定 | 注册方 |
| --- | --- | --- |
| `sync`（syncbus） | `bus != nil` 即 OK，消息是 transport 名；不反映 NATS 连通性和 consumer 状态 | `kit/syncbus/mod.go:191` |
| `Manager.CheckHealth` | 容量 ≥80% Degraded；满或已关闭 Fail；不看 flush 失败 | **仓内没有任何生产代码注册它**（kit 的 Nest Mod 只注册 `nest`，demo 的 `Scene.CheckHealth` 没有被登记）。`TROUBLESHOOTING.md` T-267 与 D1 方案把 entitysync 列为 Degraded 来源，实际 `/readyz` 看不到它（见 §7.3） |

就绪语义（Degraded 算就绪）见 [01 §6.3](01-app-lifecycle.md#63-readiness)。

### 6.3 日志（按出现位置）

| 位置 | 日志 |
| --- | --- |
| kit/nest | Info `entity sync started`（mode、interval）；Warn `entity sync: reloaded entity was not rebound` |
| entity 卸载重载 | Warn `entity: unload resync queue is full; subscribers get a remove instead of a reload`；Info `entity: unloaded entity cannot be reloaded for its subscribers; sending remove`；Warn `...kept failing; sending remove` |
| policy | Warn `policy: direct binding could not be resubmitted ...`、`policy: group could not resubmit ...`（Interest 本身不记日志） |
| syncbus | Info `syncbus mod: started`（transport、prefix、stream）/ `syncbus mod: stopped`；Warn `syncbus mod: stop interrupted`；`jetstream sync: unmarshal failed` / `handler panic`（Error）/ `handler error`；`nats sync: ...` 同类 |
| 玩家 TCP | Info `player tcp listener started|disabled`；Error `player tcp accept`；**Debug** `player tcp dispatch failed`（默认 Info 级别看不到，handler panic 也只有这一条、没有栈）；Error `player tcp: session-close subscriber panicked`（带栈） |
| lockstep / nettransport / syncstream | 包内不打日志 |

### 6.4 常见故障 → TROUBLESHOOTING

| 现象 | 行 |
| --- | --- |
| 进了场景却一帧收不到（held 会话没 Ready） | T-174 |
| 一个客户端断线，同场景排在它后面的客户端这一 tick 都收不到 | T-173 |
| 少数客户端某字段永久不更新 | T-154 |
| 断线后仍留在场景 / 在线列表 | T-137 |
| 接入层 `deliverClosed` panic 带崩进程 | T-141 |
| pipelined 部署把未持久内容发给客户端 / 恢复后客户端见过的状态在 WAL 里找不到 | T-127、T-100 |
| `/readyz` 200 但 `degraded_dependencies` 有 entitysync | T-267（注意上面 6.2 的注册缺口） |
| 生成工程 player TCP 第一次 Stop 超时、第二次立即返回 | T-243 |
| 停机日志 `syncbus stop: context deadline exceeded` | T-251 |
| JetStream 同 topic 多个同步器各自只收到一部分 | T-103 |
| 不同 prefix 的总线共用一个 Stream | T-104 |
| JetStream 状态流 / 副本偶发少一条 | T-17 |
| 同一条 JetStream 消息被反复收到 | T-36 |
| remoteentity 启动报 `snapshot push disabled ... (JetStream required)` | T-272 |
| syncstream 第二次启动 `decode WAL` / `ErrInvalidSnapshot` | T-105、T-106 |
| syncstream 接收端 `MaxFrameBytes` 调小后单片帧仍放行 | T-96 |
| lockstep 一次性操作执行两次 / 一直追帧 / 输入数解码失败 / 内存随时长增长 | T-87、T-88、T-90、T-91 |
| 升级后 `cannot find package .../entitysync` 或 `AsyncTransport has no field SendDatagram` | T-175、T-176 |

[TROUBLESHOOTING.md](../../TROUBLESHOOTING.md)。

## 7. 保证与不保证

### 7.1 保证

| 保证 | 条件 / 出处 |
| --- | --- |
| 同一 tick 给某会话的 delta 与给新订阅者的快照同一版本、同一 `CommitLSN`；同次捕获内每个 profile 只 pack 一次 | `sync/entitysync/flush.go:185`、`ENTITY_SYNC.md:27` |
| 开了水位门控时，`CommitLSN` 高于水位的 subject 整体不发（快照与增量都不发），脏位与待发快照保留 | `sync/entitysync/flush.go:203` |
| remove-before-create：退役期间订阅被拒，全部 remove 发出后同 id 才能重新登记 | `sync/entitysync/manager.go:452`、`:403` |
| 同 tick 先处理 remove 再分配新对象，满容量时合法替换不会因 subject id 顺序被误拒 | `sync/entitysync/session.go:143` |
| 单个实体包不会被截断；放不下就进下一帧，单包超过硬上限明确失败（关该会话） | `sync/entitysync/session.go:204` |
| 一个会话的传输失败只关它自己；`ErrRetryLater` 整 tick 作废、谁也不罚；调用方 ctx 取消整轮中止、不算会话故障 | `sync/entitysync/flush.go:353` |
| 已准入帧不撤回；部分成功后，受影响且仍订阅的内容下次改发全量 | `sync/entitysync/flush.go:422` |
| Hold / 重开后，旧 Push 不能覆盖新会话状态，旧错误不能关闭新会话 | `sync/entitysync/subscriptions.go:237`、`:196` |
| 撤订阅不等网络通知成功；Push 期间换 profile / 撤订 / 退役，旧捕获只结算匹配 revision 的订阅 | `sync/entitysync/flush.go:445` |
| syncbus 退订 / 总线停止返回 nil 后没有在途回调；超时返回 ctx 错误、保留状态、可重试 | `sync/syncbus/subscription.go:92`、`sync/syncbus/driver/jetstream.go:323` |
| syncstream：`Record` 返回 nil 即持久；结果不确定即 fail-stop；最新一代无效就拒绝启动、不静默回退 | `syncstream/file_journal.go:224`、`:138` |
| lockstep：帧字节是提交的确定性函数；64 帧地平线内重传幂等；追帧期间不发实时 datagram | `sync/lockstep/sequencer.go:313`、`:234`、`sync/lockstep/room.go:305` |
| 玩家 TCP：鉴权通过（非零 PlayerID、非空 SessionID）才进入读循环；会话关闭会取消该连接在途的 dispatch；Stop 超时保留 server、重试继续等 | `codegen/internal/roost/render_player_tcp.go:818`、`:1138`、`:711` |

### 7.2 不保证

- **不保证端到端延迟**：`Interval` 是合并检查周期；快照预算是软预算；20Hz 恢复窗口不保证冷对象 50ms 内到达（`sync/README.md:77`）。
- **不保证客户端收到**：准入 = `Push` 返回 nil；`Drain` 也不承诺客户端已接收字节，传输队列由其拥有者排空（`sync/entitysync/drain.go:8`）。
- **不保证冷创建公平**：没有冷创建的会话按 ID 升序尝试，传输持续 RetryLater 时低 ID 总是先试（维护者接受）。
- **普通 NATS 总线不保证送达**；JetStream 的 `SubscribeLive` 也会在这些路径丢消息：流 `MaxAge` / `MaxBytes` 淘汰、`MaxDeliver` 后 Term、反序列化失败被 ACK、退订窗口里被 ACK（`sync/syncbus/sync.go:46` 的注释没有列出这些例外）。
- **不保证 handler 恰好一次**：JetStream 是至少一次，handler 超过 `ack_wait` 会重投；发布超时后重试会出现重复。订阅方必须按版本 / 序号幂等。
- **ReplicaSyncer 不保证删除的顺序**：删除不带版本比较，乱序时可能误删新值或复活旧值（`cache/mirror.go:92`）。
- **dispatch 超时不终止 handler**；超时后 Nest 已接纳的写可能仍会生效（推断，未核对 nest 的取消语义），客户端重试的幂等由各 handler 自己负责。
- **玩家 TCP 停机窗口**：`Service.Shutdown` 期间 listener 仍开，新连接可以照常握手和登录（[v1.23.0 实现分册](../../release/v1.23.0/impl-app-own-clk-ops-tool.md) 记为维持现状）。
- **需要外部验证**：sync-aoi 性能基线（`scripts/perf/sync-aoi.sh`）、生成工程双模式脚本（`scripts/test-sync-modes-generated.sh`，不在 CI）、真实 JetStream 行为（kit/syncbus integration、remoteentity 的 JetStream integration）。

### 7.3 已知限制（本篇写作时从源码发现，待闭环）

下列每条在[实现文档 §10.2](../impl/04-sync.md#102-本篇写作时发现的源码疑点待闭环)有触发条件、后果、证据和置信度。这里只列对业务作者和运维有直接影响的：

| 编号 | 限制 | 怎么避开（在修复之前） |
| --- | --- | --- |
| F04-1 | `Interest` 排队事实指向的 id 在应用前被 `Leave` / `Hide`，之后**每一次** Flush 都失败（全部 subject 停发），`Stop` / `Drain` 也报错 | 用 `QueueMove` 的工程，在 `Leave` / `Hide` 之前确认该 id 没有未应用的事实；监控 `Stats().FlushFailures` |
| F04-2 | 排队模式下存在永久被拒的 pair（会话已关、subject 已注销）时 `Drain` 永不完成，kit 停机走不到 `Close` | 会话关闭时同步从政策里移除该观察者 |
| F04-3 | `Manager.CheckHealth` 没有任何生产注册方，容量满 / 已关闭在 `/readyz` 不可见 | 业务自己 `health.Registry.Register("entitysync", manager)` |
| F04-4 | `Group` 的成员会话被 Manager 关闭后重开，没有恢复路径 | 重开后对该成员 `Leave` 再 `Join` |
| F04-5 | lockstep 指标名缺 `lockstep.` 前缀，按 `OBSERVABILITY.md` 配的“desync 非零即事故”告警永远不触发 | v1.23.1 已修复（RR-20261006-61） |
| F04-6 | lockstep 广播预算 1232 字节不扣传输开销（KCP / QUIC 1200、UDP 加密 +32） | v1.23.1 已修复（RR-20261006-62） |
| F04-7 | lockstep `Tick` 被单个慢追帧客户端阻塞（`Reliable` 接 KCP / QUIC 且 ctx 无期限） | v1.23.1 已修复（RR-20261006-63） |
| F04-8 | 2 人房间默认 quorum = 2，desync 永远裁不出 | v1.23.1 已修复（RR-20261006-64） |
| F04-9 | 机器人遇到不足以溢出缓冲的缺口不主动追帧，局会卡到超时 | v1.23.1 已修复（RR-20261006-65） |
| F04-10 | 玩家 TCP：WriteGate 拒绝 / handler panic 断连且只有 Debug 日志、没有栈 | 线上排障临时开 Debug，或在 handler 内自己 recover 并记日志 |
| F04-11 | 玩家 TCP `shutdown_timeout: 0s` 被 doctor 当 10s 报 OK，实际进程拒绝启动 | 不要写 0 |
| F04-12 | 玩家 TCP `dispatch_timeout` 被生成器写成显式值，之后不跟随 `nest.request_timeout` | 调 `nest.request_timeout` 时同步改它 |
| F04-13 | 玩家 TCP 没有心跳、没有请求限流 | 客户端周期性发请求；限流放在 handler 或认证器 |
| F04-14 | syncbus：退订窗口 / 停机窗口内的消息可能被 ACK 而未处理，或被立即 NAK 到 `max_deliver` 后 Term | 业务按 durable 语义设计时不要依赖“退订后剩余消息留给下一个实例” |
| F04-15 | syncstream：首次 checkpoint 之前第一条 WAL 写了半截，History 永久打不开（要人工删 `wal-1`）；核心不自动 checkpoint | 创建 History 后立即 `Checkpoint()` 一次，并定期调用 |
| F04-16 | `ReplicaSyncer` 删除不比较版本；`KeyOf` / `DeleteKeyOf` 为 nil 时静默不做事 | 当前无生产调用方；启用前先补齐 |

→ [实现文档](../impl/04-sync.md)对应：§4 不变量清单、§6 失败处理、§10 review 检查点。

## 8. 相关文档

| 文档 | 用途 |
| --- | --- |
| [impl/04-sync.md](../impl/04-sync.md) | 本篇的实现文档 |
| [sync/README.md](../../../sync/README.md) | 包地图、依赖方向、源码阅读顺序、快照调度要点 |
| [ENTITY_SYNC.md](../../../ENTITY_SYNC.md) | 实体复制的生产契约（部分段落已过时，见实现文档 §10.3） |
| [ARCH-10](../../bugfix/ARCH-10-sync-manager.md)、[M-13](../../bugfix/M-13-entitysync-manager.md)、[M-14](../../bugfix/M-14-sync-policy-and-ready.md) | 为什么是一个 Manager、四层、held / ready |
| [ARCH-11](../../bugfix/ARCH-11-view-priority-and-shared-encoding.md)、[SYNC-PROFILES](../../feature/SYNC-PROFILES.md) | 多视图优先级、共享编码、字段白名单 |
| [ARCH-12](../../bugfix/ARCH-12-sync-package-layout.md)、[M-18](../../bugfix/M-18-async-transport-reliable-only.md) | 包结构与“状态帧只走可靠通道” |
| [双模式实施记录](../../feature/IMPLEMENTATION-2026-09-24-sync-modes.md) | periodic / on_change、事实队列、kit 装配 |
| [快照调度](../../feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md)、[资源预算与会话恢复](../../feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md) | 快照预算、恢复公平性 |
| [Sync 基准](../../feature/SYNC-BENCHMARKS.md)、[AOI 1000 / 10000](../../feature/SYNC-AOI-1000-10000-2026-09-23.md) | 性能基线与压测参数 |
| [A3 ② 排空退订](../../feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md)、[NATS 驱动关闭状态](../../feature/REFACTOR-2026-10-06-nats-driver-closed-state.md) | 总线退订与停机 |
| [visual-sync 生产指南](../../skill/visual-sync-production-guide.md) | syncstream 的生产基线（skill 视角） |
| [GAME_DEMO_TEMPLATE](../../feature/GAME_DEMO_TEMPLATE.md)、`demo/README.md` | game-demo 的接入层、scene、battle |
| [01 app 与生命周期](01-app-lifecycle.md) | 三步停机、停机预算、readiness |
| [02 nest 与实体](02-nest-entity.md)、[03 dataengine](03-dataengine.md)、[05 remote 与 Mirror](05-remote-mirror.md)、[08 skill](08-skill.md)、[11 可观测](11-observability.md)、[12 codegen](12-codegen.md) | 跨分区内容 |

## v1.23.1 当前口径补充（2026-10-08，未发布）

F04-D逐组更正、TCP心跳/限流、Group重连及健康注册见B8。状态走可靠有序；RR-25仍待确认。 原正文保留v1.23.0证据。完整对应表见 [B8文档收口](../../review/B8-DOCUMENTATION-CLOSURE-2026-10-08.md)。

## RR-25 有限投递收口

同topic创建/退役中返回ErrSubscriptionBusy；旧consumer Closed与在途回调结束后才能重订。默认总投递次数5，1/2/4/8秒退避；无handler准入不ACK，业务错误策略保持。到限可终止，状态快照或事件对账由所属业务恢复；详见 [有界方案](../../feature/REFACTOR-2026-10-08-rr25-bounded-delivery.md)。
