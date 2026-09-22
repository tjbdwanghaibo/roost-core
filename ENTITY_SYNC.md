# Entity Sync 生产契约

Entity Sync 只有一套实现（ARCH-10 / M-13，2026-09-22）：Entity 持有 observer-free 的内容状态（`entity.SubjectSyncState`），
`entitysync.Manager`（进程一个）持有全部 subject 与会话，subject 自己持有订阅者表，每 tick 给每个会话一帧。
room、AOI、直接绑定只是"谁订谁"的政策，调 `Subscribe / Unsubscribe`。

## 写入与锁

- 业务只调用 `EntityBase.MarkSyncDirty` 或 `MarkSyncFullDirty`（生成的 `PublishSyncDirty()`）。
- packer 总是在 Entity mutex 内执行（`PrepareTick`），返回 `FrozenSyncPayload`；返回后业务不得再持有可变 payload 引用。
- Entity 不保存 player/session/observer/history；订阅者表在 Manager 的 subject 对象里，Entity 只有内容。

## tick

- `Manager.Flush` 是一个 tick：每个 pending subject（脏、订阅变更、退役）在其锁内 `PrepareTick(deltaProfiles, snapshotProfiles)` 一次——
  给在线者的 delta 与给新订阅者的快照同一版本、同一 CommitLSN；每个不同 profile 只 pack 一次。
- 持久化门槛按 `CommitLSN` 挡**整个 subject**：任一捕获高于 `DurableWatermark()` 则本 tick 不发、脏位与待发快照保留、下 tick 重试。
- 捕获按会话聚合成帧（快照 = ObjectCreate，或已持有对象则 ObjectUpdate 带 Full；delta = ObjectUpdate；离开 = ObjectRemove），
  每会话在**副本**上编码、逐帧 `Transport.Push`；成功才采纳会话的时钟与 ObjectRef 表并结算订阅（→ 在线 @version / 删除）。
- 传输只有两种失败：`ErrRetryLater`（整体不可用）→ 全部 prepared abort、全部重新 pending、无人受罚；其他 → 该会话 `loseSession`
  （关闭、遍历 subject 删订阅、`SessionLost` 回调政策）。
- 所有会话处理完后整批 `Commit`（脏位按代际清）；退役且无订阅者的 subject 被遗忘。

## 订阅

`Subscribe(session, subject, profile)` 只改 subject 的订阅者表（kind = 需要快照），快照随下一 tick 走；换 profile 同样重发全量。
`Unsubscribe` 对已持有对象的会话欠一个 remove（随下一 tick），对从未收到的会话直接删。`Unregister` 让所有订阅者进入"正在离开"，
最后一个 remove 发出后 subject 被遗忘；退役中的 subject 拒绝新订阅。撤订阅**不以通知到对方为前提**——通知搭帧走，撤是状态变更。
LOD、权限和阵营视图通过有限的 `SyncProfile` 表达，不能把 subscriber ID 放进 Entity packer。

## 会话

`OpenSession(id)` 之后才能订阅；`CloseSession(id)` 丢会话状态并从每个 subject 删掉它的订阅，不欠任何帧。会话 id 由政策定义
（demo 用 player id，接入层对该玩家的全部连接扇出），只要求稳定、唯一。`Transport` 可选实现 `SessionLifecycle` 以跟随开关。
Manager **不建**"会话 → subjects"反向索引：这份知识归政策（AOI 的可见集）；关闭会话时遍历 subject 是兜底。

## 线格式（v2）

一帧就是一个 statesync 帧：`Epoch/Tick` 是该会话的时钟，`RoomID` 恒为流常量 1，`SchemaVersion` 来自 `ManagerConfig.FrameSchemaVersion`；
每个对象一个组件，数据是 `EncodeSubjectUpdate`（subject id、namespace、profile、version/base、mask、payload）。
客户端 `entitysync.DecodeFrame` → 每组件 `DecodeSubjectUpdate`。全部走可靠通道；datagram 分片不再用于实体同步。

## 容量与观测

`ManagerConfig.MaxSubjects / MaxSessions / MaxSubscribersPerSubject`，`Limits.MaxObjects` 同时是一个会话可持有的 subject 数；
`Stats()` 与 `CheckHealth()`（80% 降级、满或关闭为 Fail）；指标 `entitysync_frames_admitted_total`、`entitysync_sessions_lost_total`、
`entitysync_durability_gate_deferred_total`。
