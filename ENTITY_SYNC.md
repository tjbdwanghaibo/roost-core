# Entity Sync 生产契约

[双模式正式接入](docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)已实施：默认周期同步，`ModeOnChange` 提供锁内冻结、解锁/确认后唤醒、50ms 兜底。两者共用下述交付契约；正式启动注入 `NestOptionWithEntitySync`，生成 DAO 的变化由框架收集。

Entity Sync 只有一套实现，四层（ARCH-10 / M-13 / M-14，2026-09-22）：

| 层 | 包 | 职责 |
| --- | --- | --- |
| 内容 | `entity` | `SubjectSyncState`：版本、脏位、packer、CommitLSN、Namespace；`PrepareTick` 一次锁内捕获 |
| 机制 | `sync/entitysync` | `Manager`：全部 subject 与会话，subject 自己持有订阅者表，每 tick 给每个会话一到多帧，门槛，held/ready，两种失败 |
| 组织 | `sync/entitysync/policy` | 谁订谁：`Interest`（距离 + 关系源）、`Group`（成员全互见）、`Direct`（显式绑定）；各实例通过独立 `SubscriptionSource` 持有订阅 |
| 应用 | 业务启动与接入层 | `Transport` 适配、会话生命周期（进场 held → ready → 离场）、地图尺寸 / 半径 / 关系这类只有游戏知道的事 |

每层只知道下一层：entity 不知道 session，Manager 不知道为什么有人订阅，policy 不碰帧与传输，应用不写聚合规则。

## 写入与锁

- 正式 Nest 接入自动消费生成实体的 `TakeEntitySyncChanges()`；客户端与服务间 dirty 独立。手写同步字段使用 `EntityBase.MarkSyncDirty` / `MarkSyncFullDirty`，在正式事务作用域内暂存到成功准入。旧的显式 Publish 接口继续兼容。
- setter 只标脏，不触发逐字段发送。on_change 在成功准入且 Guard 仍持有 Entity 锁时冻结内容；全部 Entity 解锁且提交确认后才允许发送。periodic 在周期 Flush 时捕获。
- packer 总是在 Entity mutex 内执行（周期捕获或准入冻结），返回 `FrozenSyncPayload`；返回后业务不得再持有可变 payload 引用。
- Entity 不保存 player/session/observer/history；订阅者表在 Manager 的 subject 对象里，Entity 只有内容。

## tick

- `Manager.Flush` 是一次交付调度：为每个 pending subject（脏、订阅变更、退役）调用 `PrepareViews`，复用有效冻结内容或在 Entity 锁内重新捕获。
  给在线者的 delta 与给新订阅者的快照同一版本、同一 CommitLSN；同次捕获内每个不同 profile 只 pack 一次。
- 同 tick 的 `(subject, profile, full/delta)` 组件只编码一次，各会话外层帧仍独立编码；编码缓存不跨 tick、重试或版本复用。on_change 可跨调度保存有界的冻结内容，复用前校验内容代际、基线与 Profile，和组件编码缓存分开。
- 持久化门槛按 `CommitLSN` 挡**整个 subject**：任一捕获高于 `DurableWatermark()` 则本 tick 不发、脏位与待发快照保留、下 tick 重试。
- 捕获按会话聚合成帧（快照 = ObjectCreate，或已持有对象则 ObjectUpdate 带 Full；delta = ObjectUpdate；离开 = ObjectRemove），
  每会话在**副本**上编码、逐帧 `Transport.Push`；每一帧成功即采纳它对应的时钟与 ObjectRef 表，全部帧成功后结算订阅（→ 在线 @version / 删除）。
  同一 tick 先处理 remove 再分配新对象，避免满容量时合法替换被 subject ID 顺序误拒。
- 调用方 context 取消时整轮中止并保留重试，不视为会话故障。传输失败分两类：`ErrRetryLater`（整体不可用）→ 全部 prepared abort、全部重新 pending、无人受罚；已准入帧不能撤销，
  本 tick 有帧准入的会话保留其时钟，受影响且仍订阅的内容下次改发 Full 恢复基线；其他 → 该会话 `loseSession`
  （关闭、遍历 subject 删订阅、`SessionLost` 回调政策）。
- 所有会话处理完后整批 `Commit`（脏位按代际清）；退役且无订阅者的 subject 被遗忘。

## 订阅

`Manager.Subscribe(session, subject, profile)` / `Unsubscribe` 操作默认来源。需要叠加政策时，为每个独立所有者保留一个 `manager.NewSubscriptionSource()`，使用其 Subscribe/Unsubscribe。同来源重复订阅幂等，换 profile 替换该来源；不同来源独立释放。按 LOD、Key、SchemaVersion 升序选一个生效 profile，优先级改变才发全量，低优先级变化不出帧。profile 只表达业务已授权的视图，不代替权限判断。

最后一个来源离开时，对已持有对象或首次 create 在途的会话保留 remove 意图；从未收到且不在途的直接删除。`Unregister` 是实体退役，会释放所有来源，最后一个 remove 后才遗忘 subject。撤订阅不等待网络通知成功。LOD、权限和阵营视图通过有限的 `SyncProfile` 表达，不能把 subscriber ID 放进 Entity packer。

Push 期间若换 profile、撤订或退役，旧捕获只结算匹配的订阅 revision，不能覆盖新意图。

## 会话

`OpenSession(id)` 之后才能订阅；`OpenHeldSession(id)` 开一个 **held** 的会话——可订阅、不出帧——`ReadySession(id)` 之后第一帧是新 epoch 的 FrameFull。
客户端在登录应答之后才装解码器的部署用它消掉首帧竞态（demo：`enter_game` held，客户端发 `scene_ready`）。`HoldSession(id)` 让一个在收帧的会话重新开始（重连、客户端重置）。
`CloseSession(id)` 丢会话状态并从每个 subject 删掉它的订阅，不欠任何帧。会话 id 由政策定义
（demo 用 player id，接入层对该玩家的全部连接扇出），只要求稳定、唯一。`Transport` 可选实现 `SessionLifecycle` 以跟随开关。
旧 Push 仅可采纳到仍匹配的会话状态；Hold/重开后旧帧不覆盖新状态，旧错误也不能关闭新会话。Transport 必须把已开始的 Push 固定到原连接，已准入字节不能撤回。

Manager **不建**"会话 → subjects"反向索引：这份知识归政策（AOI 的可见集）；关闭会话时遍历 subject 是兜底。

## Namespace

`EntitySyncBuilderParam.Namespace`（codegen 标记 `syncNamespace=`）随该 subject 的每个组件下发，是客户端唯一的分流键——帧头的 RoomID 恒为常量，
房间与区域是政策不是标签。服务间的同步总线是另一条轴：契约 `sync/syncbus`，实现 `sync/syncbus/driver`，kit 的 `SyncBusMod`。裸标识符会被 codegen 拒绝（RR-20260918-07）。

## 组织方式（`sync/entitysync/policy`）

- `Interest`：`AOI`（格索引 + 半径滞回 + band，原 `spatial.InterestManager`，2026-09-23 搬进 policy）+ 任意多个 `RelationSource`（队伍、好友、self）聚合成一个 (observer, subject) 一份订阅——第一个来源订、最后一个来源撤；
  band → profile；`Apply()` 把变化说给 Manager，被拒的 subscribe 每次 Apply 再说，直到被接受或 pair 释放（`Refusal.Retry` 供调用方分日志级别）。
- `Group`：subject 集合 × 成员集合全互见，各自上限。AddSubject 失败会回滚本组订阅、允许重试；Manager 注册保留。RemoveSubject/Close 只释放本组来源，不再退役实体；实体销毁由应用调用 Manager.Unregister，成员会话也由应用开关。不叫 Room：`lockstep.Room` 是战斗房间，而这里只是一个集合。
- `Interest.Close` 释放本实例持有的订阅，不影响其他政策。
- `Direct`：`Bind / Unbind` 一对；每个 Direct 实例是独立来源，重复 Bind 不累加。

## 线格式（v2）

一帧就是一个 `sync/frame.Frame`：`Epoch/Tick` 是该会话的时钟，`RoomID` 恒为流常量 1，`SchemaVersion` 来自 `ManagerConfig.FrameSchemaVersion`；
每个对象一个组件，数据是 `EncodeSubjectUpdate`（subject id、namespace、profile、version/base、mask、payload）。
客户端 `entitysync.DecodeFrame` → 每组件 `DecodeSubjectUpdate`。全部走可靠通道；datagram 分片不再用于实体同步。

组帧以**完整 EntitySync 更新包**为最小单元，一个外层帧可装多个完整实体包。对象数量或字节上限不足时，将下一个实体包整体放入下一帧；不会截断 payload。单包超过硬上限明确失败。Transport 可选实现 `FrameSizeLimiter` 声明最大帧大小，AsyncTransport 适配器自动转发单消息限制；自定义包装需扣除自身头部开销并转发能力。

`ManagerConfig.SnapshotBudget` 可设置每 Flush 的全量对象数、实体包字节数和每会话全量对象数。只延后尚待建立/恢复基线的快照，增量（包括 full-dirty 更新）和 remove 不限流；未发快照不结算订阅，下次捕获最新内容。会话和实体均轮转。字节预算不计外层帧头，单实体超软预算允许独占一次额度以保证进度，硬上限始终有效。数量额度先于捕获筛选；字节额度在捕获后按实际包长检查。这是快照调度与准入预算，不是严格 CPU 时间上限；默认关闭。

`AsyncTransportConfig.MaxResidentReliableBytes` 限制全会话排队与在途字节总和，`MaxReliableAge` 从入队开始限制总年龄，默认均关闭。超限拒绝属于会话背压；过期终止该会话发送并清理剩余队列，通过 OnError 通知业务。异步失败应与业务的会话关闭/重建流程对接。不能丢弃旧增量后直接发送新增量。[完整实施记录](docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。

## 容量与观测

`ManagerConfig.MaxSubjects / MaxSessions / MaxSubscribersPerSubject`，`Limits.MaxObjects` 同时是一个会话可持有的 subject 数；
`Stats()` 与 `CheckHealth()`（80% 降级、满或关闭为 Fail）；指标 `entitysync_frames_admitted_total`、`entitysync_sessions_lost_total`、
`entitysync_durability_gate_deferred_total`。

## 停止与关闭

`Start(ctx)` 的 context 取消会取消周期 Push；`Stop(ctx)` 取消周期循环，退出后以本次 context 做最后一次 Flush。停止未完成时 Start 返回 `ErrManagerStopping`，不会启动第二个循环。等待 Flush 的门闩可取消；Stop 超时不代表后台传输已经退出。`Close(ctx)` 超时后保留 closing，拒绝新注册、开会话与 Start，允许再次 Close 完成清理。Transport 必须响应 Push context。

性能基准入口见 [Sync 基准说明](docs/feature/SYNC-BENCHMARKS.md)，本轮实现与验收见[收尾记录](docs/feature/SYNC-COMPLETION-2026-09-23.md)。

## Profile 配置补充（2026-09-23）

字段白名单、显式优先级、Interest 来源视图及兼容说明见 [Sync 视图配置](docs/feature/SYNC-PROFILES.md)。旧 SyncProfile 线格式和 20Hz 调度保留；新的配置可逐项接入。
