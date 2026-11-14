# Entity Sync 生产契约

[双模式正式接入](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)已实施：默认周期同步，`ModeOnChange` 提供锁内冻结、解锁/确认后唤醒、50ms 兜底。两者共用下述交付契约；正式启动注入 `NestOptionWithEntitySync`，生成 DAO 的变化由框架收集。

Entity Sync 只有一套实现，四层（ARCH-10 / M-13 / M-14，2026-09-22）：

| 层 | 包 | 职责 |
| --- | --- | --- |
| 内容 | `framework/entity` | `SubjectSyncState`：版本、脏位、packer、CommitLSN、Namespace；`PrepareViews` 一次锁内捕获 |
| 机制 | `framework/sync/entitysync` | `Manager`：全部 subject 与会话，subject 自己持有订阅者表，每 tick 给每个会话一到多帧，门槛，held/ready，两种失败 |
| 组织 | `framework/sync/entitysync/policy` | 谁订谁：`Interest`（视野：空间视野按距离、关系视野按成员）、`Group`（成员全互见）、`Direct`（显式绑定）；生命周期实现 `SelfView`（玩家看自己）与 `GroupView`（成员看组实体，与自身视图共用会话）；各实例通过独立 `SubscriptionSource` 持有订阅 |
| 应用 | 业务启动与接入层 | `Transport` 适配、会话生命周期（进场 held → ready → 离场）、地图尺寸 / 半径 / 关系这类只有游戏知道的事 |

每层只知道下一层：entity 不知道 session，Manager 不知道为什么有人订阅，policy 不碰帧与传输，应用不写聚合规则。

## 写入与锁

- 正式 Nest 接入自动消费生成实体的 `TakeEntitySyncChanges()`；客户端与服务间 dirty 独立。手写同步字段使用 `EntityBase.MarkSyncDirty`，在正式事务作用域内暂存到成功准入。EntityBase 不提供显式 Publish；消息总线发布是独立的 SyncBus 能力。
- setter 只标脏，不触发逐字段发送。on_change 在成功准入且 Guard 仍持有 Entity 锁时冻结内容；全部 Entity 解锁且提交确认后才允许发送。periodic 在周期 Flush 时捕获。
- packer 总是在 Entity mutex 内执行（周期捕获或准入冻结），返回 `FrozenSyncPayload`；返回后业务不得再持有可变 payload 引用。
- Entity 不保存 player/session/observer/history；订阅者表在 Manager 的 subject 对象里，Entity 只有内容。

## 按实体类型的同步节奏（2026-11-09）

`ManagerConfig.TypeSchedules` 以 `SubjectSyncState.SubjectKind()`（正式实体的 EntityKind）匹配，不按 namespace 或订阅来源匹配；同一 namespace 内不同实体类型可以采用不同节奏。构造时复制配置，未配置的类型保留 Manager 原有行为。

例如在正确接入 Nest 提交观察者的 Manager 中：

```go
entitysync.ManagerConfig{
    Mode: entitysync.ModeOnChange, // Player / Monster 等未覆盖类型
    Interval: 50 * time.Millisecond, // Manager 到期检查及其他兜底的粒度
    TypeSchedules: map[uint32]entitysync.TypeSchedule{
        uint32(terrain_chunk.EntityKindTerrainChunk): {
            Mode: entitysync.ModePeriodic, Interval: time.Second,
        },
    },
    // Transport、DurableWatermark 等仍按原接入设置。
}
```

显式 periodic 要求 Interval > 0；显式 on_change 要求 Interval = 0 且 Manager 本身为 on_change，避免混用全局发送预算时钟。启动仍需要正式 `NestOptionWithEntitySync` 绑定生产者，不能只调用 BindSyncProducer 冒充提交接入。**game-demo 的生产 NewScene 已启用：Player/Monster 等类型 on_change，TerrainChunk 的脏增量周期 1 秒。** 服务通过 `wiring/nest.NestSyncObserverProvider` 在 Nest 构造前提供观察者，Scene 创建后接入；禁止与 EntitySyncSetup/self_sync 或显式观察者同时配置。历史功能/测量夹具的无观察者构造器仍保留原 periodic 行为，其历史带宽结果不代表新生产模式。

- 周期类型提交时不冻结内容。首次变脏起计时，连续修改不续期；常规 Flush 在到期前不捕获/发送 live 增量。仅脏对象保留在 pending 中，由现有 Manager tick 检查，不遍历空闲实体。实际交付还受 tick 粒度、持久门槛、预算和网络影响，不是硬实时期限。
- 首次全量、恢复/profile 变更的全量及 remove 不受该周期等待。快照仍受既有预算与提交/持久门槛。周期内即时快照使用 `SubjectSyncState.PrepareSnapshotViews`，只向需要快照的订阅交付，不清除待发变化、不确认 DAO 变化树；窗口内只为本轮能拿到快照的订阅捕获（未 Hold、未超额暂缓、冷创建预算已入选），只剩 Hold 会话（入场后等 Ready）需要快照时不捕获，避免每次 Flush 重打包、推进版本并让老观察者到期改发 Full（2026-11-09 审查修复，`TestTypeScheduleHeldSnapshotDoesNotRepackWhileWaiting`）。
- **即时快照的代价**：若它捕获了待发变化并成功提交，会推进内容版本；下一次普通周期捕获改发 Full，保证旧订阅和新订阅都收到正确状态，避免把快照之前的列表操作再当作新增量应用。新玩家频繁进入编辑热点可能增加全量量，应单独测量，不保证所有场景带宽下降。
- 普通显式 Flush 遵守类型周期；Drain / Stop 绕过周期等待，仍保留持久水位、发送预算、重试和 context 规则，不承诺客户端必达。失败不重设本批到期时间；正常捕获完成后若仍有捕获期间的新写入，则保留脏数据并从结算时启动下一批窗口。
- 每次首次变脏的时间不同，自然分散检查；同拍大批变脏仍可能集中到期。第一版未引入时间轮：脏集合在每次 Flush 被检查，事务观察者也可能发出合并唤醒，但周期对象不因此冻结/编码。后续优化该扫描与唤醒前先测量，不能宣称已经消除全部调度成本。
- 原有 DAO 变化树预算不变，1 秒积累可能触发整树回退；状态同步合并最终值，不提供逐操作事件交付。

## tick

- `Manager.Flush` 是一次交付调度：为每个 pending subject（脏、订阅变更、退役）调用 `PrepareViews`，复用有效冻结内容或在 Entity 锁内重新捕获。
  给在线者的 delta 与给新订阅者的快照同一版本、同一 CommitLSN；同次捕获内每个不同 profile 只 pack 一次。
- on_change 的冻结槽每个 subject 一份，新冻结整份替换旧的；冻下的就是此刻一次普通捕获会得到的内容（增量从当前版本起，带全部未确认的变化），**已有冻结或同一 subject 的捕获在途时也不升成全量**（2026-11-10 起，修前一律升成 Full/Resync，见 CHANGELOG）。在途捕获提交后版本前进，这份增量的基线过时：Flush 校验基线后不复用，在 Entity 锁内按新基线重新捕获，只带确认之后的变化（列表操作不重放）；在途捕获被作废时它原样可用。在途捕获已交付到与冻结相同的内容代际时，过时冻结随提交归还字节。所以已有基线的观察者只在状态本身要求时收全量（`fullDirty`、换 profile、恢复），不因提交与交付的时序收全量；代价是交付在途时提交的变化要多一次锁内捕获。
- 同 tick 的 `(subject, profile, full/delta)` 组件只编码一次，各会话外层帧仍独立编码；编码缓存不跨 tick、重试或版本复用。on_change 可跨调度保存有界的冻结内容，复用前校验内容代际、基线与 Profile，和组件编码缓存分开。
- 持久化门槛按 `CommitLSN` 挡**整个 subject**：任一捕获高于 `DurableWatermark()` 则本 tick 不发、脏位与待发快照保留、下 tick 重试。
- 捕获按会话聚合成帧（快照 = ObjectCreate，或已持有对象则 ObjectUpdate 带 Full；delta = ObjectUpdate；离开 = ObjectRemove），
  每会话在**副本**上编码、逐帧 `Transport.Push`；每一帧成功即采纳它对应的时钟与 ObjectRef 表，全部帧成功后结算订阅（→ 在线 @version / 删除）。
  同一 tick 先处理 remove 再分配新对象，避免满容量时合法替换被 subject ID 顺序误拒。
- 调用方 context 取消时整轮中止并保留重试，不视为会话故障。传输失败分两类：`ErrRetryLater`（整体不可用）→ 全部 prepared abort、全部重新 pending、无人受罚；已准入帧不能撤销，
  本 tick 有帧准入的会话保留其时钟，受影响且仍订阅的内容下次改发 Full 恢复基线；其他 → 该会话 `loseSession`
  （关闭、遍历 subject 删订阅、`SessionLost` 回调政策）。
- 所有会话处理完后整批 `Commit`（普通捕获的脏位按代际清，即时独立快照保留脏位）；退役且无订阅者的 subject 被遗忘。

## 订阅

`Manager.Subscribe(session, subject, profile)` / `Unsubscribe` 操作默认来源。需要叠加政策时，为每个独立所有者保留一个 `manager.NewSubscriptionSource()`，使用其 Subscribe/Unsubscribe。同来源重复订阅幂等，换 profile 替换该来源；不同来源独立释放。先按 ProfilePriorities（缺省取 LOD）排名，再按 LOD、Key、SchemaVersion 升序选一个生效 profile，优先级改变才发全量，低优先级变化不出帧。profile 只表达业务已授权的视图，不代替权限判断。

最后一个来源离开时，对已持有对象或首次 create 在途的会话保留 remove 意图；从未收到且不在途的直接删除。`Unregister` 是实体退役，会释放所有来源，最后一个 remove 后才遗忘 subject。撤订阅不等待网络通知成功。LOD、权限和阵营视图通过有限的 `SyncProfile` 表达，不能把 subscriber ID 放进 Entity packer。

Push 期间若换 profile、撤订或退役，旧捕获只结算匹配的订阅 revision，不能覆盖新意图。

## 会话

`OpenSession(id)` 之后才能订阅；`OpenHeldSession(id)` 开一个 **held** 的会话——可订阅、不出帧——`ReadySession(id)` 之后第一帧是新 epoch 的 FrameFull。
客户端在登录应答之后才装解码器的部署用它消掉首帧竞态（demo：`enter_game` held，客户端发 `scene_ready`）。`HoldSession(id)` 让一个在收帧的会话重新开始（重连、客户端重置）。
Hold → Ready 的结果是：接收端的对象表等于 Ready 时仍在的订阅。新 epoch 的 Full 让上一 epoch 的对象全部作废，所以 held 期间（或 Ready 之后、新 Full 之前）撤掉的订阅不欠 remove；撤到一个订阅都不剩时，这一 lifetime 交付过帧的会话会收到一个不含对象的 Full（`session.owesEmptyFull`），接收端照样清空，从未收过帧的会话不发。因此“暂停发布 → 读权限 → Apply → Ready”也可以用 Hold，代价是 Ready 后全部对象按全量重发；接收端收到 Full 时要先清空旧对象（`entitysync.Decoder` / `frame.Receiver` 的约定）。2026-10-21 之前没有对象时不发帧，接收端会一直留着最后撤掉的对象。
`CloseSession(id)` 丢会话状态并从每个 subject 删掉它的订阅，不欠任何帧。会话 id 由政策定义
（demo 用 player id，接入层对该玩家的全部连接扇出），只要求稳定、唯一。`Transport` 可选实现 `SessionLifecycle` 以跟随开关。
旧 Push 仅可采纳到仍匹配的会话状态；Hold/重开后旧帧不覆盖新状态，旧错误也不能关闭新会话。Transport 必须把已开始的 Push 固定到原连接，已准入字节不能撤回。

Manager 维护每个 session lifetime 的 subjects 反向索引，Hold/Ready/Close 只遍历相关 subject；政策另行持有可见关系，二者的生命周期和职责不同。

## Namespace

`EntitySyncBuilderParam.Namespace`（codegen 标记 `syncNamespace=`）随该 subject 的每个组件下发，是客户端唯一的分流键——帧头的 StreamID 恒为常量，
房间与区域是政策不是标签。服务间的同步总线是另一条轴：契约 `framework/sync/syncbus`，实现 `framework/sync/syncbus/driver`，Wiring 的 `SyncBusMod`。裸标识符会被 codegen 拒绝（RR-20260918-07）。

## 组织方式（`framework/sync/entitysync/policy`）

- `Interest`：`AOI`（格索引 + 半径滞回 + band，原 `spatial.InterestManager`，2026-09-23 搬进 policy）上的视野（D19）：空间视野（玩家主视野与业务的空间视野，一个视野是 AOI 的一个观察者）与关系视野（显式实体集合），玩家与视野多对多；按 (玩家, 目标) 聚合成一份订阅——第一个持有的视野订、最后一个释放的视野撤，层级包含取高、不包含取并集；
  (LOD 层, 距离档) → profile（`BandLayer(level, band)`，视野换层时按新层重给整个可见集，缺省只看档；见 [guide/04-sync“视野参数”](guide/04-sync.md#谁看见谁视野viewd19)）；全部写入口只登记一条有序日志（不做 AOI 计算、不等批次），由 Manager 的政策阶段（ApplySync）或 Interest 的应用线程（ApplyAsync）按调用顺序执行并说给 Manager，被拒的 subscribe 每次结算再说，直到被接受或 pair 释放（政策阶段的拒绝交给 `InterestConfig.OnRefusal`；测试与工具可用 `Apply()` 在调用方线程排空并结算，返回 `Refusal`，`Retry` 供调用方分日志级别）。
- `Group`：subject 集合 × 成员集合全互见，各自上限。AddSubject 失败会回滚本组订阅、允许重试；Manager 注册保留。RemoveSubject/Close 只释放本组来源，不再退役实体；实体销毁由应用调用 Manager.Unregister，成员会话也由应用开关。同 ID 会话重开后再次 Join 会重新提交已有订阅，不增加成员数量。不叫 Room：`lockstep.Match` 是对局执行对象，而这里只是一个集合。
- `Interest.Close` 释放本实例持有的订阅，不影响其他政策。
- `Direct`：`Bind / Unbind` 一对；每个 Direct 实例是独立来源，重复 Bind 不累加。
- `SelfView` / `GroupView`：建在上面这些机制上的生命周期实现，见下一节。会话由调用方自己开关时用 `Group`；会话归自身视图（`access.player.self_sync`）时用 `GroupView`。

## 按组同步与 self_sync 共用连接（Groups）

2026-10-31 起（派对房间纵切发现的缺口）。“某类实体（房间、队伍、副本）的同步视图推给组里的成员”的正式入口是 `wiring/selfsync.Groups`（生命周期在 `policy.GroupView`），生成工程在 roost.yaml 写 `access.player.sync_groups: [party_room]`。

**为什么不直接用 `Group` 或自建 Manager。** 同一进程打开 self_sync 后：

1. Nest 只有一个 EntitySync 观察者位置。业务再经 `NestOptionWithEntitySync` 塞一个自建 Manager 会被 Nest Mod 自己的选项**静默覆盖**（收不到提交捕获与回滚撤回，periodic 下毫无报错）。现在 `wiring/nest.Mod.Provide` 拒绝这种装配（`ErrEntitySyncObserverConflict`）。两个 Manager 推同一条连接还要两个推送编号、两套 epoch，客户端要分别维护两个 `entitysync.Decoder`。
2. 把 `Group` 挂到 self_sync 的 Manager 上（`Mod.EntitySync()`）在 Manager 与线格式层面可以共存：同一会话、同一帧里两个对象，客户端按每个更新的命名空间 + 视图键分流。但会话归 `SelfView`：登录后第一个政策阶段之前 `Join` 被拒（`session is not open`）；断线重连时会话关闭、组订阅随之删除，`Group` 仍记着成员，重连后房间**静默断供**（实测：重连后第一帧只有 `player/self`，下一周期无推送）。业务拿不到“会话何时重开”的时机，没法补 `Join`。

**做法。** `GroupView` 与 `SelfView` 同一写法、装在同一个 Manager 上、政策阶段排在 `SelfView` 之后：

- 成员关系是事实。`Join / Leave / Disband / EntityLoaded` 只在锁下记事实、不调用 Manager、不等待，可在快池调用；政策阶段按净事实一次做完：组实体在内存就 `Register`（重复是空操作，重新加载的新状态接上并强制全量），会话已开的成员 `SubscribeStamped`，没开的（离线、断线宽限内、会话还没打开）留在 waiting、每个政策阶段复查。
- 框架丢掉订阅时经 `SubscriptionSourceHooks.Released` 删簿记（戳比较，同 `Direct`），成员仍在组里就在会话重开的那个阶段重新订阅；卸载撤销后重新登记经 `Resubmit` 交还。所以断线重连、顶号（Hold/Ready）、组实体卸载重载都不需要业务再调用。
- `GroupView` 拥有组实体的登记：`Disband` 退订全部成员并 `Unregister`，同 ID 再成组走 remove-before-create。
- `wiring/selfsync.Groups` 只做装配与提交边界：`Config.Groups` 交给 `selfsync.New`，Nest Mod Init 时随自身视图装上；组实体加载按 kind 转交 `EntityLoaded`；`Join / Leave / Disband` 在 Nest 事务里调用时推迟到提交之后（`nest.AfterCommit`），事务回滚就不发生，事务外调用立即记下。组 ID 是组实体 ID，成员是玩家 ID（自身视图的会话 ID，玩家实体 ID 用 `entity.GetUniqueIDFromEntityID` 换）。

**线上怎么区分两路。** 同一条玩家连接、同一个推送编号（`msgid.MsgSelfSync`）、同一个会话、同一个 epoch：一帧里可以同时有玩家自己与房间两个对象，客户端按每个更新自带的 `Namespace`（实体标记的 `syncNamespace`）与 `Profile.Key` 分流——自身视图是 Player 的命名空间 + `self`，组视图是组实体的命名空间 + `GroupsConfig.Profile.Key`。只用一个 `entitysync.Decoder`；重连后的 Full 同时带回两路（`wiring/selfsync` 的 `TestGroupsShareTheSelfSyncConnection` 走真实 TCP）。

取舍与确认过程见 [FEATURE-2026-10-31-room-sync-groups](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-31-room-sync-groups.md)。

**限制。** 离线成员每个政策阶段被复查一次（一次 `SubscribeStamped` 失败返回），代价与“有离线成员的组”的成员数成正比；成员关系不持久；`Groups` 只能装在 self_sync 的 Manager 上（会话归自身视图），没打开 self_sync 的进程用 `Group` 并自己开关会话。`GroupsConfig.Profile.Key` 必填：组视图的字段权限由 packer 按视图键决定。

## 线格式（帧协议 2 / 信封 v3）

一帧就是一个 `framework/sync/frame.Frame`：`Epoch/Tick` 是该会话的时钟，`StreamID` 恒为流常量 1，`SchemaVersion` 来自 `ManagerConfig.FrameSchemaVersion`；
Epoch 在一个 Manager 内单调递增、跨会话 ID 共享：每次 `OpenSession` 与 `HoldSession` 都取下一个值，客户端不能假设首帧 epoch 为 1。同一会话 ID 关掉又打开、新 lifetime 的帧落到同一个接收端时，新全量因此总是比接收端已收过的帧 epoch 更大，会被接受而不是当旧包丢掉。框架自带的政策（自身视图、game-demo 场景）不在原连接上重开：推送在活着的连接上被拒时传输关闭该玩家的连接（[impl/04-sync §2C](impl/04-sync.md#2c-自身视图selfview的生命周期)），这一条是 Manager 自身的防御。
每个对象一个组件，数据是 `EncodeSubjectUpdate` 编出的 subject 信封（`WireVersion` 3）。全部走可靠通道；datagram 分片不再用于实体同步。

**帧（`frame.ProtocolVersion` 2）**：魔数 `RF`、协议版本、帧类型各 1～2 字节，其余整数一律是最短 uvarint（非最短编码当损坏拒绝）：StreamID、Epoch、Tick、增量帧的 `Tick−BaseTick`、SchemaVersion、对象数；对象是操作（1 字节）、Ref.ID、Ref.Generation、**只有 create 带** Archetype、非 remove 的组件数；组件是操作、TypeID，set 再带 SchemaVersion、长度与数据。帧头的上限是 `frame.MaxHeaderBytes`、每对象 / 每组件头的上限是 `MaxObjectHeaderBytes` / `MaxComponentHeaderBytes`，Manager 分帧按这些上限预留，实际帧只会更短。

**subject 信封（`entitysync.WireVersion` 3）**：首字节是标志（全量、带 mask、带 reason）。
- 全量：subject ID、Kind、Version、BaseVersion、[Mask]、[Reason]、Codec、LOD、Profile.SchemaVersion、命名空间、视图键（`default` 写成空串）、载荷。
- 增量：BaseVersion、`Version−BaseVersion`（≥1）、[Mask]、[Reason]、载荷。subject ID、Kind、命名空间、视图键、Codec、LOD、视图 schema 都不在线上，由接收端从同一 ObjectRef（会话内的短 ID）、同一组件类型上次收到的全量继承。

增量能省掉身份的依据：同一会话里 ObjectRef 与 subject 一一对应，换 subject 必然先 remove 再 create（create 一定是全量）；BaseVersion 仍逐个对象核对，接收端持有的版本不等于 BaseVersion 就拒绝，所以丢帧、乱序、重放照样过不了校验。一个视图（命名空间 + 视图键）的 packer 必须始终用同一个 Codec，增量才能沿用全量的 Codec。

**Codec 一致由服务端保证（2026-11-10）**：增量沿用基线全量的 Codec，所以 Manager 按（会话, subject）记下会话持有的基线 Codec（最近一次交付并结算的全量的载荷 Codec；增量不改它，空载荷的增量也不改，与 `Decoder` 相同）。一份非空的非全量更新的 Codec 与之不同时，不发这份增量：这个会话的订阅转回待快照（基线作废），subject 排进下一次 Flush，会话收到当前状态的全量（`Reason = Resync`，带新 Codec），之后的增量按新 Codec 解；同一份增量照常发给基线已是新 Codec 的会话。代价是这个会话晚一个 Flush 看到这次变化、多收一份全量；计 `entitysync_codec_resyncs_total{namespace}`、`ManagerStats.CodecResyncs`，限频 Warn（[OBSERVABILITY](OBSERVABILITY.md)）。不在 packer 注册或打包时拒绝：Codec 只有打包之后才知道，换 Codec（例如载荷格式升级）本身合法，拒绝打包会让这个 subject 的同步停住；按会话判断也只让真正持有旧 Codec 基线的会话多收一次全量。正常为 0，仓库里的 packer（生成的 DAO packer、self_sync、demo、测试）同一视图都只用一个 Codec（`TestADeltaInAnotherCodecThanTheBaselineGoesOutAsAFull`）。

**客户端**：增量信封不能单独解码，必须用有状态的 `entitysync.Decoder`（每连接一个）：`Decode(frameBytes)` 先过 `frame.Receiver` 的流校验（epoch、tick、基线），再按对象表解每个信封，返回 `ReceivedFrame`（对象的 `SubjectID`、每组件的 `SubjectSyncUpdate` 与线上字节数）。旧 epoch 或已收过的 tick 返回 `ErrStaleFrame`（跳过即可）；其他错误之后 Decoder 只接受新的全量。全量帧先清空旧对象。C# 是 `SubjectUpdate.Decode(data, baseline)`（`EntityViews` 自动传上一次的更新），C++ 是 `roost::sync_receiver`（按它的对象表传基线）。

DAO 同步 PB 的增量补丁不再带 `entity_id`（生成的 `MarshalSyncDelta` / `MarshalSyncChanges`），只有全量带；`ApplySync*` 在增量上接受缺省的 0，带了不同的 ID 照旧拒绝（[DAO-SYNC-PROTOBUF](DAO-SYNC-PROTOBUF.md)）。一步移动的典型字节（game-demo 主视野，玩家或怪走一步、只改 pos_x）：帧头 10 + 对象头 4 + 组件头 4 + 信封 4 + DAO 补丁 8 = 30 字节，同一帧每多一个对象 20 字节；修前是 140 / 108 字节。逐字节拆解与前后对照见 [sync-delta-bytes 证据](../maintenance/evidence/sync-delta-bytes-20261110/README.md)。

组帧以**完整 EntitySync 更新包**为最小单元，一个外层帧可装多个完整实体包。对象数量或字节上限不足时，将下一个实体包整体放入下一帧；不会截断 payload。单包超过硬上限明确失败。Transport 可选实现 `FrameSizeLimiter` 声明最大帧大小，AsyncTransport 适配器自动转发单消息限制；自定义包装需扣除自身头部开销并转发能力。

`ManagerConfig.SnapshotBudget` 可设置每 Flush 的全量对象数、实体包字节数和每会话全量对象数。只延后尚待建立/恢复基线的快照，增量（包括 full-dirty 更新）和 remove 不限流；未发快照不结算订阅，下次捕获最新内容。会话和实体均轮转。字节预算不计外层帧头，单实体超软预算允许独占一次额度以保证进度，硬上限始终有效。数量额度先于捕获筛选；字节额度在捕获后按实际包长检查。这是快照调度与准入预算，不是严格 CPU 时间上限；默认关闭。配置了 `ManagerConfig.Namespaces`（见下“按命名空间分开计”）时，它只管没列出的命名空间。

`AsyncTransportConfig.MaxResidentReliableBytes` 限制全会话排队与在途字节总和，`MaxReliableAge` 从入队开始限制总年龄，默认均关闭。超限拒绝属于会话背压；过期终止该会话发送并清理剩余队列，通过 OnError 通知业务。异步失败应与业务的会话关闭/重建流程对接。不能丢弃旧增量后直接发送新增量。[完整实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。

## 按命名空间分开计（Namespaces）

2026-11-08 起（维护者决定，可编辑地形方案 D15：“地形的对象上限与快照预算不影响玩家和怪”）。`ManagerConfig.Namespaces map[string]NamespaceLimits` 给列出的同步命名空间单独计两样东西，没列出的命名空间沿用 `Limits.MaxObjects` 与 Manager 级 `SnapshotBudget`：

- `NamespaceLimits.MaxObjects`（必须为正）：一个会话可同时持有的该命名空间对象数。每一类各自计：没列出的那一类仍是 `Limits.MaxObjects`，不因加了一类而放宽；会话的对象引用表合起来可持有 `Limits.MaxObjects + Σ MaxObjects`（不超过 65535）。
- `NamespaceLimits.SnapshotBudget`：该命名空间单独的冷创建预算，与 Manager 级预算、其他命名空间互不占额度；每一路各自轮转会话与实体（游标、两类来源交替）、各自计字节软上限与窗口，RetryLater 中断准入时每一路只给实际尝试过的会话计费、游标不越过该路计划里未尝试的会话。零值表示该命名空间不限快照（不继承 Manager 级预算）。
- 分类键是 subject 的 `Namespace`（codegen `syncNamespace`），不是订阅来源：命名空间是对象自己的属性，一个对象经几个来源订阅也只占一个引用、只属于一类；它也是客户端的分流键。类别在登记时按状态的命名空间确定，`Rebind` 不改变。
- **超出一类的上限**按下面[超额降级](#超额降级会话持有对象超过上限)处理（2026-11-08 起，D17）：只暂缓该类放不下的新对象、不关会话，计数与告警点名命名空间（没列出的那一类标签为 `*`）；另一类照常。政策的上限（该命名空间所用 Interest 的 `AOIConfig.MaxVisible`）应不大于它的 `MaxObjects`，否则会一直在边界上暂缓、收紧。同一 tick 先 remove 再 create，满额的类里换对象不超限；Hold/Ready、关闭重开从零计；退役（remove-before-create）先归还名额。
- **帧与客户端**：每帧对象数仍按 `Limits.MaxObjects` 切分，只有引用表上限变成各类之和，所以接收端（Go `entitysync.Decoder` 的 `frame.Limits`、C# `SyncFrame.Decode` 的 `maxObjects`）照旧按 `Limits.MaxObjects` 配置，不需要知道总数；客户端若显式设对象表容量（C# `EntityViews` 的 `maxObjects`，缺省 10000），不得小于各类之和。
- **默认不变**：不配置（nil）时与没有这一项逐字节相同（既有用例不改断言即通过）；列出某个命名空间也不改变其他命名空间的任何字节（`TestNamespaceLimitsLeaveUnlistedNamespacesByteIdentical`）。实现：`framework/sync/entitysync/namespaces.go`（类别与持有计数）、`snapshot_budget.go`（`snapshotLane`，每路一份额度、游标与待快照索引）。

game-demo 的场景：玩家 / 怪 / 场景沿用 100、不限快照，`terrain` 命名空间上限 64、每会话每次 Flush 至多 4 个全量（`demo/internal/service/game/terrain_sync.go.tmpl` 的 `terrainNamespaceLimits`）。

## 超额降级（会话持有对象超过上限）

2026-11-08 起（维护者决定，可编辑地形方案 D17：“超额降级：减少同步对象，不断开”）。会话持有的对象超过上限（`Limits.MaxObjects`，或按命名空间的 `NamespaceLimits.MaxObjects`）时**不关会话**，减少同步给它的对象。实现在 `framework/sync/entitysync/over_limit.go`（Manager）与 `policy/interest_over_limit.go`（Interest）。

- **准入**（`admitWithinLimits`，捕获之后、发送 / 快照预算与组帧之前）：按会话、按类别数“已持有 − 本轮 remove + 本轮新对象”。不超出时什么都不做——未超额的会话帧逐字节不变（`TestUnderTheLimitFramesAreByteIdentical`，对照 b4699758 的 SHA-256）。超出的类里，新对象按**钉住在前、Profile 优先级小的在前（`ProfilePriorities`，未配置按 LOD；Interest 的距离档即 LOD）、实体 ID 小的在前**排序，前“空位”个照常创建，其余本轮不创建：对象要么完整持有、要么完全不在客户端，不截断。没创建的订阅保持“待创建”并标为**超额暂缓**。remove 先于 create 的规则照旧，同一 tick 里先腾出的位置算作空位。只比较本轮捕获到的新对象：被快照预算、发送预算、持久化门槛挡下的不占位，之后到来时按当时的空位准入。
- **已持有的对象 Manager 不挤掉**：它不知道距离，挤掉还会在线上产生 remove / create 往返；新旧之间的取舍由政策收到通知后退订完成。唯一例外是**钉住**的订阅（`SubscriptionSource.SubscribePinned`；`SelfView` 与 Interest 的 self——玩家看自己——都钉住）：放不下时挤掉同类里优先级最低（Profile 优先级最大、同级取实体 ID 最大）的未钉住对象，同一帧先 remove 再 create，被挤掉的订阅转为暂缓并通知（`Evicted`）。同类全是钉住的对象时钉住的订阅也只能暂缓（`pinned_blocked`，配置错误）。
- **通知政策**：`SubscriptionSourceHooks.Deferred func([]DeferredSubscription)`（`Session`、`Subject`、`Namespace`、`Evicted`），在下一次政策阶段按来源分批交付（释放、重新提交之后，各政策 Apply 之前）。订阅进入暂缓时通知一次，仍在暂缓里每隔约 1 秒（`overLimitRenotifyInterval`）再通知一次：政策让出的位置可能被别的占了，或它退订的是还没捕获的订阅（快照预算下常见），只通知一次会停在“想要的比放得下的多”上。
- **暂缓的订阅**不参与捕获、不进快照预算索引（`PendingSnapshots` 不计）、不让 `Drain` 等待。会话的这一类出现空位时（本会话的帧准入后持有数低于上限）`releaseOverLimit` 按同一排序放回前“空位”个，下一次 Flush 重新准入；`HoldSession` 从零计，全部放回，Ready 后按排序重新准入（钉住的排最前）。暂缓的订阅都是政策持有的订阅，政策不收紧也不会无界增长。
- **各政策的行为**：
  - **Interest**（有距离）：收到通知后收紧该玩家视野的可见上限、从最远的开始退订，腾出的位置由 Manager 换成最近的暂缓对象，所以最终可见集是它按距离选出的最近 N 个（`TestInterestShrinksAFullSessionToTheNearest`）。每条通知要这名玩家在一个空间视野里腾出一个位置：优先这对 pair 所在、只挂给这名玩家的空间视野，其次玩家的主视野，再次 pair 所在的共享空间视野（会让挂接它的玩家一起少看）；关系视野、块覆盖、self 没有距离，只来自它们的 pair 由主视野让位（`TestInterestRelationPairMakesTheMainViewGiveWay`）。离开视野的成员只有在这名玩家已没有别的来源（块覆盖、关系视野、别的空间视野、self）持有它时才算腾出位置，所以上限从最远的开始一直往下减，直到要的位置都腾出来或上限到 0（2026-11-14 修正：此前只减通知数，最远的成员同时经块覆盖被看见时一个位置也没腾出，冷却到期又撤掉收紧，暂缓的对象永远到不了客户端；`TestInterestTightensPastMembersHeldByOtherSources`）。上限到 0 仍不够时（无距离来源本身就超过会话上限），剩下的订阅留在 Manager 的超额暂缓里，有空位时恢复。放宽：每次结算先在当前上限内补位；收紧后冷却 1 秒，到期时上限已不起作用（补位后仍不满）就撤掉、回到 `MaxVisible`，否则上调一档（`MaxVisible` 的 1/8，至少 1）。滞回：上调后冷却期内又被通知，冷却加倍（至多 32 秒）；安然度过则减半。失败的试探只是 Manager 内部的一次暂缓（新进入的目标还没创建就被退订），不上线。`AOICluster` 不接这条回馈。
  - **Group / Direct / GroupView**（无距离）：不收紧，不登记回调；订阅暂缓、会话照旧，有空位时按（Profile 优先级、实体 ID）恢复（`TestGroupOverLimitWaitsForRoom`）。
  - **SelfView** 与 Interest 的 self：钉住，永不暂缓（`TestInterestSelfIsNeverDeferred`、`TestOverLimitPinnedSubscriptionEvictsTheLowestPriority`）。
- **可观测**：`entitysync_over_limit_deferred_total{namespace, reason}`（`deferred` 新对象暂缓 / `evicted` 为钉住让位 / `pinned_blocked`）、`entitysync_over_limit_waiting{namespace}`、`ManagerStats.OverLimitWaiting / OverLimitDeferred / OverLimitEvicted`（`AuditStats` 逐项核对）、`CheckHealth` 在 `OverLimitWaiting > 0` 时降级（消息带 `over_limit_waiting=N`）、限频 Warn `entitysync: a session is over its object limit …`（每个 Manager 10 秒至多一条，带被省略的次数）；Interest 侧 `entitysync_interest_over_limit_total{action}`（`tighten` / `raise` / `lift` / `untightened`）、`entitysync_interest_limited_views`。正常都为 0；持续不为 0 说明政策的上限（`MaxVisible` 之和 + 关系视野 + 自身）与会话上限不匹配。
- **契约变化**：`SessionLost` 不再出现 `object_limit`（`SessionLossObjectLimit` 删除；编码时的持有上限错误只可能来自内部不一致，归 `encode_failed`）；`too_large`、`encode_failed`、`push_refused` 不变；客户端与帧格式不变。

## 容量与观测

`ManagerConfig.MaxSubjects / MaxSessions / MaxSubscribersPerSubject`，`Limits.MaxObjects` 同时是一个会话可持有的 subject 数（配置了 `Namespaces` 时是没列出的那一类的数，列出的命名空间各按自己的 `MaxObjects`）；
`Subscribe` 不按它拒绝，超出时按上节“超额降级”暂缓放不下的新对象，会话不关；
`SessionLost` 的原因总是 `*SessionLoss`：`LossReason(cause)` 是低基数分类——`push_refused`（传输拒收这一帧）、
`too_large`（单实体包超过 `MaxFrameBytes` / `MaxComponentBytes`）、`encode_failed`（其他编码错误），`Err` 是原错误。后两种没有帧交给传输，连接没有拒收任何东西：
会话 ID 就是玩家 ID 的 Manager 用 `wiring/selfsync.DisconnectLostPlayer` 断开该玩家（自身视图自动接上，game-demo 场景同样调用；维护者 2026-10-22 决定 A），
客户端重连、从新 epoch 的全量开始，计 `player.sync.session_lost_disconnect.total{reason}`；推送拒收时 `PushPlayerFrame` 已在传输里断开过，不再断第二次；
持有量仍应由业务用政策的上限（`AOIConfig.MaxVisible` 等）压在会话上限以内，超额降级只是兜底（`TestOneFramePerSessionPerTickSplitOnlyAtTheObjectLimit`）；
`Stats()` 与 `CheckHealth()`（80% 降级、满或关闭为 Fail；有超额暂缓时降级）；指标 `entitysync_frames_admitted_total`、`entitysync_sessions_lost_total`、
`entitysync_durability_gate_deferred_total`、`entitysync_over_limit_*`。

## 停止与关闭

`Start(ctx)` 的 context 取消会取消周期 Push；`Stop(ctx)` 取消周期循环，退出后以本次 context 做最后一次 Flush。停止未完成时 Start 返回 `ErrManagerStopping`，不会启动第二个循环。等待 Flush 的门闩可取消；Stop 超时不代表后台传输已经退出。`Close(ctx)` 超时后保留 closing，拒绝新注册、开会话与 Start，允许再次 Close 完成清理。Transport 必须响应 Push context。

性能基准入口见 [Sync 基准说明](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-BENCHMARKS.md)，本轮实现与验收见[收尾记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-COMPLETION-2026-09-23.md)。

## Profile 配置补充（2026-09-23）

字段白名单、显式优先级、Interest 来源视图及兼容说明见 [Sync 视图配置](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-PROFILES.md)。当前接入以[Sync设计与使用](guide/04-sync.md)为准，不承诺旧线格式兼容。Block空间组织见[Interest Block AOI](INTEREST-BLOCK-AOI.md)。
