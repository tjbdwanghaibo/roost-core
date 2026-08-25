# Entity Sync 生产契约

Entity Sync 只保留一套实现：Entity 持有 observer-free 内容状态，`entitysync.SubscriptionCoordinator` 持有 subscriber membership，kit 的 room replication 负责帧调度和 transport admission。

## 写入与锁

- 业务只调用 `EntityBase.MarkSyncDirty` 或 `MarkSyncFullDirty`。
- packer 总是在 Entity mutex 内执行，返回 `FrozenSyncPayload`；返回后业务不得再持有可变 payload 引用。
- Entity 不保存 player/session/observer/history，避免生命周期和网络背压反向污染业务实体。

## Flush

- 单 subject 主动 flush 调用 `SubscriptionCoordinator.FlushSubject(ctx, base.Sync())`；房间 tick 使用 `DistributeBatch` 将本帧全部 dirty subject 一次 admission。
- coordinator 对 subject 使用分片串行屏障；批量 admission 按 subject/订阅者稳定排序，能够由下游合并为每个接收者一份全局帧。
- `ReliableEnvelopeSink.AdmitEnvelopes` 必须整批成功或返回错误，禁止部分 admission。
- admission 前由 `PreparedSubjectSyncBatch` 一次预留全部 prepared state，单项 `Commit/Abort` 随即失效；整个批次 admission 成功后在统一锁序下整批提交，失败整批 abort，杜绝下游已接纳而 Entity 只提交一部分。

## 订阅

Subscribe 先安装 pending membership，再生成对应 profile 的完整 snapshot；snapshot admission 成功后才转 active。Unsubscribe 的 leave envelope 同样使用可靠 admission。LOD、权限和阵营视图通过有限的 `SyncProfile` 表达，不能把 subscriber ID 放进 Entity packer。

## 关闭与容量

生产房间由 kit `RoomManager` 统一控制总房间数、全局 subject/subscriber 预算和空闲回收。房间销毁调用 `RoomReplication.Close(ctx)`；sink 必须提供有界队列、背压、session sequence、ACK/history 和 transport retry。Entity 清理会关闭唯一的 sync state，不存在 AsyncSync、observer sync 或兼容双写。
