# ARCH-10：实体同步统一为一个 SyncManager，room / AOI 降为组织方式

- 单元：待实施，分三批，实施时各占一个 M 编号（从 M-13 起）；不占 U，不计缺陷
- 仓库：roost-core `entitysync` + `room`（+ `statesync` 帧头）+ `demo` 模板 `internal/service/game/scene.go.tmpl`
- 来源：维护者 2026-09-22 提出方向；**合并 ARCH-08（room 同步的分层收敛）与 ARCH-09（非房间的实体复制路径）**，
  两条的候选正文保留在 `docs/bug/WANTED.md` W-2026-09-22-04 / -05 / -06
- 前提决定（维护者 09-22）：**尚未上线，线上帧头语义可以改**——`statesync` 帧头里的 RoomID / Epoch / Tick 不构成兼容性约束

## 一句话

实体同步本质是一件事：一个 subject 的内容变化，送到订阅它的会话。room 和 AOI 都只是"谁订谁"的组织方式，
不该各自持有订阅表、各自决定失败后怎么办。今天三层（coordinator / room / sink）各记一份订阅、各有一套失败政策，
RR-20260915-04 / -05、RR-20260922-01、U-0278 都长在层与层的缝里。

## 对象模型

| 对象 | 数量 | 持有什么 | 从哪来 |
| --- | --- | --- | --- |
| **Subject** | 每个 `sync=true` 实体一个 | 今天的 `SubjectSyncState` 原样（脏位代际、Prepare/Commit/Abort、CommitLSN、按 profile 一次 Prepare）**+ 私有订阅者表** `session → {profile, contentVersion, 待发快照}` | `entity/subject_sync.go` 不动；订阅者表来自 coordinator 的 `subscriptions`（按 subject 切开） |
| **SyncManager** | 进程一个 | subject 注册表（按实体 id）、脏集与 tick、`session → subjects` 反向索引（派生，可重建）、持久化水位门槛、预算总量、subject 的组织标签（room id / namespace） | coordinator 的 `bySubject`、room 的 `subjects` / `dirty` / `retiring` / budget、`RoomManager` 的 tick 与 sweep |
| **SessionSink** | 每会话一个 | 这个会话的 ObjectRef 空间、出站序号、reliable/datagram 通道选择、慢消费者判定；每 tick 为这个会话拼**一帧**：它订阅的所有脏 subject 的 delta + 新订阅的快照 + 退订的 remove | `RoomTransportSink` 的 `roomObjectRefs` / `encodeFrame` / `admitWithSlowConsumerPolicy`，去掉 roomID 维度；`RoomEnvelopeSink` 的序号 |
| **Policy** | 每种组织方式一个 | 只回答"谁该订谁、订哪个 profile"，产出 subscribe / unsubscribe 调 manager；room = 一个标签 + 一份预算 + 关房时对其 subjects 全部 retire；AOI = 今天 demo 的 `InterestSystem`；直接订阅 = 实体 → 指定会话 | `RoomBroadcaster` 的公开 API 与 demo 的 `Scene` |

一句话的所有权规则：**subject 是订阅的真相，manager 的反向索引是派生物，SessionSink 只知道会话自己。** 订阅、退订、退役、剔除都是状态变更，
"通知对方"搭该会话的下一帧走——不存在"投不到所以撤不掉"这一类耦合。

## 保留的不变量（实施时的契约清单）

下面这些 promise test 描述的承诺必须在新形状下重新落地（文件名可变、承诺不能丢）：

- `entitysync/subscription_promises_test.go`（入口守卫）、`durability_gate_promises_test.go`（门槛覆盖全部出口，CommitLSN 随内容捕获）、
  `unreachable_subscriber_promises_test.go`（不可达订阅者不阻塞撤订阅——新形状下应该由构造保证，测试改成"会话丢了，subject 的订阅者表与反向索引同步清空"）
- `room/delta_durability_promises_test.go`（普通 delta 走 reliable）、`durable_watermark_promises_test.go`（水位源可接入）、
  `eviction_notice_promises_test.go`（剔除通知不随批次失败丢失——新形状下剔除是 manager.ForgetSession 一步）、
  `downstream_handover_promises_test.go`（替换 sink 的交接——若 SessionSink 局部化后仍有"替换传输"需求）、
  `guards_promises_test.go`、`sweep_interval_promises_test.go`、`unreachable_subscriber_promises_test.go`（U-0277）、
  `jetstream_*`（房间间 ISyncBus，**不在本线范围**，原样保留）
- `TestSubscriptionCoordinatorSharesProfilePayload` / `Benchmark…100Subscribers`：每 profile 一次 Prepare 的共享 payload 不能退化
- `TestSubscriptionCoordinatorDistributeBatchIsAtomic`：原子性的粒度从"一个房间一批"改为"一个会话一帧"，测试改写为"一帧内所有 subject 的 delta 要么都在要么都不在"
- demo `scene_test.go.tmpl` 全部用例（含 U-0267 / U-0278 的两组）

## 帧头的新语义

`statesync` 帧头今天是 `RoomID / Epoch / Tick / BaseTick / SchemaVersion`，`ApplyDelta` 要求 base 与 current 的 RoomID、Epoch 一致且 Tick 递增（`delta.go:20,134`）；
`roomWireClock` 把**房间级帧号**拆成 epoch / tick。新形状下：

- `Epoch` = 会话代际（重连、强制全量时加一），`Tick` = 该会话的帧序号，`BaseTick` = 上一帧序号——全部是 SessionSink 私有的，客户端逻辑不变（仍是"同 epoch、tick 递增"）；
- `RoomID` 字段改为 subject 的组织标签（namespace / room 标签），只供客户端分流，不参与连续性校验——这正是 `syncTopic` / `Namespace` 的实际用途（ARCH-09 的去向）；
- 字段布局不变，只改语义；如果想把校验里的 RoomID 比较去掉，`delta.go` 两处各删一个条件。

## 分三批

1. **manager 替代 coordinator + envelope sink，藏在现有 `RoomBroadcaster` API 后面。** 订阅表搬进 subject，反向索引进 manager，
   envelope sink 的 roomID 键消失（ARCH-08 A）。验收：room / entitysync 全部 promise test 绿，`RoomBroadcasterStats` 语义不变，
   16 机器人冷跑（含断线 churn）绿。此时 `entitysync` 包可以并入 room 或改名为 `sync`（ARCH-08 B）。
2. **SessionSink 局部化，每会话一帧。** transport sink 的 `(room, session)` 键消失，FailBatch / Evict 变成会话级：一个会话的失败只影响它自己，
   U-0278 的 lane 分流逻辑退化成"推不到就 ForgetSession"。帧头按上节改。demo 的 `sceneLane.AdmitBatch` 从"一批多会话"变成"一会话一帧"。
3. **room 降为 policy，补 AOI 与直接订阅两种 policy。** `RoomBroadcaster` / `RoomManager` 的公开 API 收成 `RoomPolicy`（标签、预算、关房 retire、每房复制周期若仍需要）；
   demo 的 `Scene` 只剩 policy 装配；`syncTopic` 标记的文档改为"客户端分流用的 namespace"。ARCH-09 在此关闭。

每一批独立可发版；第 1 批之后 RR-20260922-01 类的问题在结构上已不可能再出现。

## 要拍板但可以后拍的

- 每房不同的 `ReplicationInterval` / `IdleTTL` 保留为 policy 级参数，还是全局一个 tick？（建议：全局 tick，policy 只决定订阅；需要低频的房间用 profile 的 LOD 限频。）
- 订阅时的快照同步投递还是随下一 tick？（建议：随 tick，并给 policy 一个"会话 ready 之后再开始"的钩子——顺带消掉首帧竞态。）
- 反向索引是否持久保存校验（定期对账 subject 表 vs 索引）？（建议：只在 debug 构建对账，正式路径单向派生。）

## 风险

- **锁拓扑**：manager 不能一把锁。subject 分片（沿用 coordinator 的 `subjectOps` 条带）+ 会话分片；tick 先按 subject 取脏、Prepare，再按会话聚合出帧。聚合这一步是新的热点，第 1 批就要有基准（沿用 `Benchmark…100Subscribers` 的形状扩到 N 会话 × M subject）。
- **一帧过大**：一个会话订阅很多 subject 时，一 tick 一帧可能超过 `MaxFrameBytes`；SessionSink 需要按上限切成多帧但保持同 tick（statesync 已有分片与限额机制，复用）。
- **退役语义**：subject 退役时 remove 搭每个订阅者的下一帧走，若该会话此后再也没有帧（它只订了这一个 subject），remove 也要保证被发出——"有待发内容就出帧"是 SessionSink 的规则，不能只在有 delta 时出帧。
- **与合仓的排期**：roost-consolidate 仍在进行，本线排在它之后；两者都动 room / entitysync 的话不要同批。

## 未做

本记录只定形状与分批，未写代码。ARCH-08 / ARCH-09 的独立条目关闭，并入本记录。
