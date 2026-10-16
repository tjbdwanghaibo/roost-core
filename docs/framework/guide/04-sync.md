# 核心：Sync、Lockstep 与客户端：设计与使用

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/04-sync.md) · [模块总目录](../README.md)

## 先理解这一块

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

背包和位置通常看状态同步，按输入推进同一场战斗看Lockstep。两种方式解决不同问题，不能直接互换。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 子系统分工

当前主线将同步流、对局和空间区域分别命名为 `StreamID`、`lockstep.Match`、`AreaID`。这是随 v1.26.0 发布的源码 API 调整（不留旧别名），升级映射与验证见[维护记录](../../maintenance/ACCESS-SYNC-FOLLOWUPS.md)。流标识只在连接/同步上下文内有意义，Entity Sync 当前填入 1；不要把它当地图或副本 ID。

entitysync 维护 subject、session、subscription、profile 与每会话帧；policy 提供 Direct/Group/Interest/AOI 的可见关系。frame 定义帧格式，nettransport 负责发送能力与背压。syncbus/mirror 是服务间复制，syncstream 是带基线/增量与 journal 的流。lockstep 传输输入、帧与校验，不替业务保证跨引擎确定性。

工作分支已实现 [Block AOI 与手写 SpatialComponent](../INTEREST-BLOCK-AOI.md)：事实入队使用独立短锁，批次更新空间后并行计算observer；同一实体可在多个块被观察，实际位置仍唯一。内容发布必须同时满足提交门和政策事实已处理。接入不改Guard回调或生成器；完整重档性能尚待复验。

## 2. 状态同步流程

注册 subject → OpenSession（可 held）→ Subscribe → ReadySession → Flush/运行循环。held 会话可以建立订阅但不出帧；ready 后从新 epoch 全量开始。多个来源订阅同一 subject，各自持有引用，最后一个离开才真正取消。subject 保存订阅真相，session 保存其对象生命周期关系。

PrepareViews 汇总所需 profile；优先级配置先参与选择，再按确定顺序选择视图。视图必须限制字段可见范围，不能因空视图或缓存复用扩大权限。快照预算限制尚未持有对象的冷创建；增量、remove 和已持有对象的全量不能被当成同一种冷创建计费。

### 好友、队伍等关系如何更新

关系源必须先通过 Interest 配置声明，subject 和接收 session 也需要完成注册。已掌握完整成员表时用 `QueueRelation` 全量替换；只知道哪些成员加入、退出时，用 v1.26.0 新增的 `QueueRelationDelta`。例如好友列表有 1000 人，只增删 1 人时，不必再发送整张列表。

业务在已有 Nest handler 中先修改自己的关系 DAO，再调用以下入口；调用失败必须向 handler 返回错误。框架只维护同步可见关系，不替业务保存好友或队伍数据。

```go
// 已处于保护 player 的 Nest handler 内；team 是初始化时声明的关系源名。
if err := interest.QueueRelationDelta(player, "team", joinedIDs, leftIDs); err != nil {
    return err
}
```

两个 ID 切片在入队时复制，返回后可复用；空切片表示该方向无变化，两个都为空也仍走正常准入检查。单次调用中同一 ID 同时出现在两边时，退出优先；重复 ID 不重复发事件，零 ID 忽略。多次调用按同一实体的事实顺序执行，后一次全量替换会覆盖前面的增量结果。

只有提交确认且政策事实应用后才发布内容；回滚丢弃增量。Leave 会作废观察者自己的旧事实，Hide 只撤销被观察身份；退出 ID 会从尚未应用的增量中剔除，防止同 ID 重建时被旧新增/移除影响。移除 team 来源不会取消其他来源仍持有的订阅。

底层 `RelationSource.ApplyDelta` 是立即更新的政策 API，不参与事务确认；普通业务 handler 应用上面的排队入口。增量与全量对照、边界测试和剩余优化项见 [Sync 优化说明](../SYNC-OPTIMIZATION.md)。

## 3. periodic 与 on_change

periodic 按 Interval 合并检查；on_change 在正式 handler 提交边界汇总：setter 只标脏，持 Guard 冻结需要的视图，解锁且满足持久确认后唤醒。唤醒可合并，pending 才是事实来源，不能依赖每次通知恰好被接收。两种模式共用订阅、版本和线协议。

配置 DurableWatermark 时，冻结状态的 CommitLSN 超出水位则暂不发送。不要把 MarkDirty 的时刻当成客户端可见时刻，也不要把 on_change 描述成每个 setter 都立即发包。

### 可配置合批窗口（v1.26.0 新增）

希望将短时间内多次已提交更新集中处理，可以设置：

```yaml
sync:
  entity:
    mode: on_change
    interval: 50ms
    on_change_batch_window: 5ms
```

代码配置对应 `entitysync.ManagerConfig.OnChangeBatchWindow`。缺省为 0，保持原来的立即处理唤醒；正值从循环收到第一次唤醒时开始计时，后续通知不延长这次窗口。周期检查可提前触发，因此持续写入不会一直等下去。5ms 是示例取舍，不是端到端延迟保证：Flush 执行、队列、提交确认、网络和重试仍需要时间。

只有 on_change 能设置非零窗口；负值或 periodic 搭配非零值会拒绝启动。配置文件显式写 0 可覆盖代码中的非零窗口，`EntitySyncSetup.Configure` 最后执行，仍有最终覆盖权。启动日志会打印实际窗口。

手动 `Flush`、`Drain` 和 `Stop` 的最后一次 Flush 不等窗口；周期 Interval 继续作为兜底与原有快照预算窗口，不能把合批配置当成提高冷创建额度。该选项只改变发送循环时机，不减少 handler 的捕获次数，也不改变提交水位或可靠增量规则；冻结数据可能保留更久，应按实际内容测量峰值内存和延迟。单独执行 Flush 不会取消循环中已经安排的计时器，之后可能多一次空 Flush。

## 4. 背压与恢复

状态全量、增量、remove 走可靠有序通道。Push 返回 nil 仅表示传输接纳；ErrRetryLater 保持脏状态等待重试。两种玩家接入都是有界异步出站：嵌入 TCP 的 `PushSync*` / `PushLockstep*` 只进入该连接的出站队列，独立 Gate 走 Game 有序出站口；读得太慢的客户端被关闭，不会拖住同一次 Flush / Tick 里排在它后面的会话（[GATEWAY §4 嵌入 TCP 的出站](../GATEWAY.md#嵌入-tcp-的出站)）。基线缺口、epoch 切换或未知对象必须请求/等待全量恢复，客户端不继续在错误基线上应用 delta。会话替换与旧异步回调必须按代际核对。

### 停机排空（Drain）卡住时看什么

`Manager.Drain(ctx)` 循环 Flush 直到没有待处理条目：预算 / 水位暂缓等下个周期，传输失败直接返回错误。被永久暂缓或拒绝的条目（持久水位不再前进、提交确认一直不到、政策事实一直不能应用）会让 Drain 等到 ctx 结束；Drain 不会放弃它们（DESIGN-TODO D3，维护者 2026-10-15 决定只做诊断增强，行为不变）。ctx 结束时返回 `*entitysync.DrainStalledError`：

- `errors.Is(err, context.DeadlineExceeded / context.Canceled)` 照常成立；
- `Stalls` 最多列 `DrainStallLimit`（8）项、`Total` 是全部条目数。每项有来源（`subject` 待捕获、`snapshot` 等冷创建预算、`policy` / `interest` 政策阶段）、subject 与会话、最后一次暂缓或拒绝的原因（`commit_pending`、`durability_wait`、`prepare_failed`、`snapshot_budget`、`subscribe_refused`、`policy_pending` 等）、补充说明、已连续等待的时长和最后一次错误；
- 同时计 `entitysync_drain_timeout_total`，并把剩余条目数写入 `entitysync_drain_remaining_entries`（成功排空写 0）。

自定义政策用 `Manager.RegisterPolicy(entitysync.PolicyHooks{Apply, Pending, Stalls})` 登记；`Stalls` 可选，不提供时超时错误只报告“该政策仍有待处理事实”。Interest 的被拒订阅连续 10 次后不再阻挡 Drain（`entitysync_interest_retry_stalled_total`），所以出现在列表里的 `subscribe_refused` 都还在这个上界之内。

## 5. 服务间总线与流

SyncBusMod 的主题前缀缺省 `roost.sync`，与驱动缺省、生成配置一致；JetStream 流名显式 `syncbus.stream` 优先，否则由前缀推出（`roost.sync` → `ROOST_SYNC`，`zz.sync` → `ZZ_SYNC`），共用一个 NATS 的不同前缀各有各的流。没有按旧前缀的特例映射：要沿用已有的流与 durable 游标只能显式写 `syncbus.stream`；新流与已有流的 subjects 重叠时启动明确失败，不静默换流。

JetStream SyncBus 默认有限投递次数 5，不能宣称无限重试或绝不丢。topic 创建/退役期间拒绝冲突订阅，旧 consumer 和在途回调排空后才能重订。无 handler 准入不 ACK；业务错误与坏信封按各自规则结算。流容量、保留期限与次数上限均可能终止投递，恢复由快照或业务对账承担。

### SyncBus 的定位与丢失兜底

**定位（维护者 2026-10-15 决定）**：SyncBus 默认当作**可丢广播**。普通 NATS 驱动最多一次；JetStream 驱动在 MaxDeliver、MaxAge、MaxBytes 之内补投，超出就终止，终止只计数（`nats.jetstream.terminal.total{reason="max_deliver"}`，见[指标清单](../OBSERVABILITY.md#syncbus-有限重投与恢复rr-25)）并记日志，框架不自动修复订阅方。只用于“丢了也无妨或下一条会覆盖”的数据（在线提示、可重算的缓存失效通知）可以直接用；**需要最终一致的状态必须由业务做“丢失检测 → 拉快照对账”闭环**，不能只测重试成功。框架内的用法也遵守这一点：Remote 快照推送在拿不到可确认订阅时退化为按需读取，`PatchSyncer` 只承载瞬时补丁、不保存状态。

推荐写法（可编译、可运行的完整示例见 [syncbus/example_test.go](../../../framework/sync/syncbus/example_test.go) 的 `Example_reconcileAfterLoss`，随 `go test ./framework/sync/syncbus` 执行并核对输出）：

1. **版本是每个 key 的连续序号**：权威方每改一次该 key，`SyncMsg.Version` 加 1，增量与快照共用这一序列。`MessageID` 是投递身份，不能拿来判断丢失。
2. **序号缺口**：订阅方记录每个 key 已整体正确的版本 `applied`。`Version <= applied` 是旧消息或重投，丢弃；`Version == applied+1` 才应用增量；`Version > applied+1` 是缺口，停止应用这个 key 的增量，转去拉快照。
3. **版本落后 / 投递耗尽**：缺口只在后面还有消息时才看得见。末尾一条丢失（或订阅方停机期间超出保留期）必须靠**周期对账**发现：定时、启动或重订之后、收到投递终止告警之后，向权威方取各 key 的版本摘要，本地落后的逐个拉快照。
4. **拉快照对账**：快照来自权威方（RPC、权威存储、Remote 快照读等，由业务决定），按快照整体替换该 key 并把 `applied` 设为快照版本；快照版本可能已超过触发它的消息，之后到达的旧增量自然按第 2 步丢弃。拉取失败保持旧版本、等下一次触发，不能把“没拉到”当成已一致。

```go
// 订阅回调的核心判断（完整代码见 example_test.go）。
switch current := r.applied[msg.Key]; {
case msg.Version <= current:
    return nil // 旧消息或重投
case msg.Version > current+1:
    return r.resync(ctx, msg.Key) // 序号缺口：拉快照整体替换
}
r.values[msg.Key] += delta.Add
r.applied[msg.Key] = msg.Version
```

消息本身携带整体状态（而不是增量）时，缺口不会算错，只需“版本更大才覆盖”加第 3 步的周期对账。对账周期、摘要的分页与限流、权威方的快照负载都由业务按数据量决定。

框架级对账（订阅端检测到缺口或投递耗尽时自动向发布端请求快照，复用 Remote 快照 / Entity Sync 全量机制，并提供指标与告警）是**下一阶段候选**，尚未实施；需要先确认哪些业务在 SyncBus 上承载需要最终一致的状态，再定义快照请求协议、限流与发布端负载上限（[DESIGN-TODO D2](../../maintenance/DESIGN-TODO.md#d2-syncbus-有限投递的兜底)）。

syncstream 碎片 TTL 在新多片输入时清扫，不是独立定时器。FileJournal 直接并发 Append 可合批，但单一 History.Record 的写锁串行化不能据此承诺每业务记录减少 fsync。订阅停止、回调排空、journal 生命周期须一起关闭。

## 6. Lockstep

客户端 Command 提供 Input/Hash/Catchup；Match 根据当前 session 映射席位，不信任客户端自报 seat。Match 汇集输入、广播 C7 帧和历史追帧；历史与 hash 报告默认按保留窗口自动滚动裁剪（下节）。输入校验、hash 判定、重连追帧与游戏模拟分开。两人不形成多数时 NoMajority 不等于已经识别作弊者。

### 长局如何控制内存：历史保留窗口

`MatchConfig.HistoryRetainFrames` 决定保留多少帧（含最新帧），维护者 2026-10-15 决定默认开启自动裁剪（DESIGN-TODO D1 方案 C）：

| 配置 | 行为 |
| --- | --- |
| 0（默认） | `DefaultHistoryRetainFrames` = 3600 帧：60Hz 下 60 秒、30Hz 下 120 秒。以 10 席、每帧每席 8 字节估算约 1.5MB / 局，与对局长度无关 |
| 正数 | 自定义窗口，按“允许断线秒数 × 逻辑帧率”换算；必须不小于 `MinHistoryRetainFrames`（= `ReplayHorizon` = 64，落后这么多帧的客户端在 Sequencer 看来仍在局内，它的追帧必须可服务；也覆盖一整页追帧与最大冗余深度），否则 `NewMatch` 返回 `ErrMatchConfigInvalid` |
| `lockstep.RetainFullHistory`（-1） | 关闭自动裁剪：历史与 hash 报告随对局长度增长，宿主自己调用 `TrimHistory` / `TrimHashReports`（需要整局回放且不另行持久化时才这样配） |

裁剪在每次 `Tick` 末尾执行：先发本次追帧页（页面在发出前已编码成独立字节），再裁掉窗口之前的帧；hash 报告按 64 帧步长跟随同一窗口裁剪，所以报告表最多比窗口多一个步长。宿主仍可调用 `TrimHistory` / `TrimHashReports` 裁得更多（例如 hash 判定窗口比断线窗口短），但裁掉的帧找不回来。`History.ReadRange` 和 `Replay` 返回只读视图，仅在下一次 Append/裁剪前有效；需要长期保存回放时自行复制并持久化。

### 追帧越界：明确失败，不自动重同步

起点早于保留窗口的追帧不可能补齐，框架给出可判别的失败，客户端表现是“追帧失败、交由业务决定踢出或重开”，**不做全量重同步**（Lockstep 本身没有状态快照，方案 B 留待有具体游戏需求再定）：

- 服务端：`StartCatchup` / `SpectatorCatchup` / `HandleCommand(OpCatchup)` 立即返回 `*CatchupUnservableError`（`errors.Is(err, lockstep.ErrCatchupUnservable)`，字段 `From` 是客户端缺的第一帧、`Start` 是仍保留的最早帧）；起点 0 按帧 1 处理，同样核对。已在追帧的会话若页面一直发不出去、游标被窗口越过，下一次 `Tick` 的错误里带同一错误。两种情况都计 `lockstep.catchup.unservable.total`。
- 通知：下一次 `Tick` 经可靠通道、与追帧页同一路由给该会话发一条追帧失败通知（`C9 01 from start`，`EncodeCatchupUnservable`）。经 TCP Notify 发出的 Catchup 命令没有应答，客户端靠这条通知得知结果；通知发送失败只记进 `Tick` 的错误。
- Go 客户端：`FrameAssembler.Ingest` 收到通知返回 `*CatchupUnservableError`；`robot.LockstepBot` 随即终止（`ErrLockstepBotTerminal`，链上带 `ErrCatchupUnservable`），不再请求追帧。sink 直接把服务端的该错误交回时同样终止。
- C# SDK：`LockstepAssembler.Ingest` 抛 `LockstepCatchupUnservableException`（`RequestedFrom` / `HistoryStart`），`LockstepClient.HandlePushAsync` 把它记为 `Failure` 后重新抛出，之后的推送一律拒绝。业务据此踢出、回大厅或开新局；不能跳过缺帧继续模拟。

保留窗口越短越省内存，也越容易让慢追帧失效：断线时长的分布与 `lockstep.catchup.unservable.total` 一起决定是否要调大窗口。本机已验证一小时逻辑帧、默认窗口下的长局有界和滚动追帧；这不是一小时真实网络运行或跨引擎确定性的证明，测量见[优化说明](../SYNC-OPTIMIZATION.md)。

## 7. 公共客户端协议

RS v2 的 16 字节头保留 MsgID、Seq、PayloadSize；Flags bit0 是 push，bits1..2 区分 PB=0、Sync=1、Lockstep=2，值3及保留位拒绝。Lockstep 上行 flags=4，push=5。MsgID 0 的心跳不能携带业务类型标记。

C# 客户端提供 TCP、Sync receiver、Lockstep codec/assembler，Unity 提供主线程适配源码。Go wire 与生成 TCP 共享头验证；业务载荷类型须与注册路由一致。正式 Go/C# 及 Unity 源码测试不代表 Unity/Godot/Unreal 实机已验收；原生 C++ 客户端不在本版交付范围。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
