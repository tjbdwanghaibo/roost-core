# 核心：Sync、Lockstep 与客户端：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/04-sync.md) · [模块总目录](../README.md)

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

### 玩家只看自己：自身视图

最常见的需求是“客户端收到自己的金币、道具变化”。生成工程在 `roost.yaml` 的 `access.player` 上写 `self_sync: true`（默认关），生成器把 Nest Mod 换成 `NewModWithEntitySync` 并装上 `wiring/selfsync`；Player 的标记缺省按 `sync=true`，没写 `subjectPacker=` 时生成器写默认 packer（视图 `self` 包含 DAO 全部 sync 字段），业务只给要看的字段标 `sync`；要裁剪字段或别的视图时自己写 packer 并在标记写 `subjectPacker=`。步骤、生成物与客户端解码见 [PLAYER-ACCESS §7](../PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)。

行为：鉴权 ACK 之后登记连接，下一个 tick 开会话、绑定 player→自己（`policy.Direct`，视图 `self`），第一帧全量，之后提交的变化是增量；实体还不在内存时等它出现；断线重连、同一玩家的新连接都从全量开始，全部连接断开后关会话、注销 subject。生命周期全部在 core 的 [`policy.SelfView`](../../../framework/sync/entitysync/policy/self_view.go)：回调只记事实，Flush 的政策阶段一次对齐，所以登录、断线、顶号与 Flush 的交错不需要业务处理。多人视图（附近的人、队友）仍用下面的 Interest。

慢消费者：**跟不上就断开**。一帧被活着的连接拒收时，传输（`wiring/selfsync.PushPlayerFrame`）关闭这名玩家的全部连接、Manager 关会话，客户端重连从全量开始；SelfView 不在原连接上重开。整个进程的出站驻留预算耗尽是例外：不是谁慢，整轮重试、保留脏状态，连接与会话都不动。自建 Manager、会话 ID 就是玩家 ID 的场景（如 game-demo 的场景）用同一个函数做 Transport。规则与理由见 [impl/04-sync §2C](../impl/04-sync.md#2c-自身视图selfview的生命周期)、[GATEWAY §4](../GATEWAY.md#慢消费者跟不上就断开)。

两条由框架强制、业务不用对齐的规则：一帧的上限取接入层上限（嵌入 TCP 的 `player_access.tcp.max_payload_bytes`、独立 Gate 下 Game Ingress 的载荷上限），在 Manager 创建前定下，所以超过接入层上限的帧不会推出去；一个 `SubjectSyncState` 只能登记在一个 Manager，第二个 Manager 登记时得到 `entitysync.ErrSubjectRegisteredElsewhere`（点名已登记它的 Manager），所以工程里另有自建 Manager 登记 Player 时不能同时打开 self_sync。

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

状态全量、增量、remove 走可靠有序通道。Push 返回 nil 仅表示传输接纳；ErrRetryLater 保持脏状态等待重试。两种玩家接入都是有界异步出站：嵌入 TCP 的 `PushSync*` / `PushLockstep*` 只进入该连接的出站队列，独立 Gate 走 Game 有序出站口；读得太慢的客户端被关闭，不会拖住同一次 Flush / Tick 里排在它后面的会话；整个进程的出站驻留预算（独立 Gate 下 Game Ingress 与嵌入 TCP 各一个）耗尽时推送被拒、不关连接，Sync 整轮重试（[GATEWAY §4 嵌入 TCP 的出站](../GATEWAY.md#嵌入-tcp-的出站)）。基线缺口、epoch 切换或未知对象必须请求/等待全量恢复，客户端不继续在错误基线上应用 delta。会话替换与旧异步回调必须按代际核对。

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

### 经嵌入 TCP 追帧：随链路放慢

追帧页由 Match 在后台协程逐页发送（`launchSend` 每页起一个 goroutine），每会话同时最多一页在途；`CatchupSendTimeout`（默认 2s）经 ctx 限制一页的发送时长，超时与其他发送失败一起计入 `CatchupMaxFailures`（默认连续 8 次放弃）。这套限速依赖可靠通道在链路慢时让发送变慢——KCP / QUIC 的窗口满了发送就阻塞。

`lockstep.TCPSender` 的两条通道走同一条连接的出站队列，2026-10-16 起按是否等写出分开（维护者决定方向 A；此前 `1251e7f3` 把嵌入 TCP 推送改为只准入后，两条通道都不等写出，追帧失去链路背压）：

- 实时通道 `SendDatagram`（Tick 给每个接收者的当前帧）接 `TCPRuntime.PushLockstepSession`，只做出站准入、几乎立即返回。Tick 逐接收者串行发送，等写出会让一个停读连接拖住同一 Tick 的所有人；停读连接只会因为自己的出站上限被关闭（[GATEWAY §4](../GATEWAY.md#嵌入-tcp-的出站)）。
- 可靠通道 `SendReliable`（追帧页与追帧失败通知）接 `TCPRuntime.PushLockstepSessionWritten`，入队并等写出。慢链路上一页要等写协程把它写进 socket 才返回，下一页随后才发，追帧速度降到链路速度；该连接里属于追帧的积压只有在途那一页（加上至多一页已被写协程取走、等待超时后仍在写的页），不会因追帧超过出站上限被断开。
- 嵌入 TCP 的全进程出站驻留预算（`player_access.tcp.outbound.resident_bytes`，2026-10-20 起）耗尽时，两条通道的这一帧都当场被拒（`gateway.ErrResidentBudgetExhausted`），连接不关闭、不等预算：实时帧对这个接收者按数据报丢失处理（`Tick` 返回带这个错误的结果，下一个广播包的冗余帧或客户端追帧补上），追帧页计一次追帧失败（`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted`）。
- 一页在 `CatchupSendTimeout` 内没写出：还在排队就撤回、不再写出，已在写的照常写完；计一次失败，下一 Tick 从同一游标重发（客户端按帧 ID 去重）。连续 `CatchupMaxFailures` 次后放弃这次追帧，`Tick` 返回 “abandoned after N failures”（链上带 `context.DeadlineExceeded`），会话恢复实时广播；之后它的实时帧仍只入队，链路依旧跟不上时由出站上限或 `write_timeout` 关闭连接，客户端重连后再追帧。

阻塞边界：

- 等写出只发生在 `launchSend` 起的 goroutine 上，不在调用 `Tick` 的协程，也不在 Nest 快池 / 业务 worker 上；`TCPSender` 发送前的 `fctx.BlockingError` 守卫保留，比赛驱动放进业务 worker 会在任何副作用前被拒绝。
- 每页各自一个 goroutine、各自一条连接的写协程，一个慢连接的等待不占其他会话的追帧。`Tick` 对本次发起的所有页最多共等 `CatchupSendWait`（默认 5ms，共用一个计时器），没写完的页交给后续 `Tick` 收取、不再等；所以一个停读的追帧会话最多让发起它那一页的 `Tick` 晚 `CatchupSendWait`。实时广播在 `Tick` 里先于追帧发出，不受影响。
- 回归（[tcp_test.go](../../../framework/sync/lockstep/tcp_test.go)，真实回环 TCP）：`TestACatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（约 8 MiB 历史，客户端先停读再慢读，追帧完成且连接不断）、`TestACatchUpPageTheLinkCannotTakeTimesOutWithoutHoldingOtherSessions`（停读连接的页按 `CatchupSendTimeout` 超时并放弃，同时另一连接的追帧与实时帧照常，`Tick` 不被拖住）、`TestALiveBroadcastOverEmbeddedTCPNeverWaitsForAStalledConnection`（停读连接上实时广播只入队）。

接线：`lockstep.NewTCPSender(messageID, maxPayload, resolve, push, pushWritten)` 的最后两个参数分别是实时与追帧的发送函数，生产接 `runtime.PushLockstepSession` 与 `runtime.PushLockstepSessionWritten`（生成工程用 `Runtime.TCPRuntime`）；接反了，要么实时广播被停读连接拖住，要么追帧又失去链路背压。

生成 game-demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`，2026-10-20 维护者决定）也经 `TCPSender`：`battleLinks` 在嵌入 TCP 下把追帧接到 `Runtime.TCPRuntime.PushLockstepSessionWritten`，实时帧经 `battleLiveLane` 只准入、单个接收者发送失败只记 Debug（冗余帧或追帧补上）。10102 推送的是 Lockstep 包本身（flags=5，不带 PB 包装与 battle id），按接入层 SessionID 寻址：座位在第一条 `BattleInput` 到达的连接上加入（换连接则换一个新的 Lockstep 会话 ID），没说过话的座位不收帧，晚加入的座位靠追帧补齐。回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt` 在生成工程 `internal/service/game` 里，与上面包内用例同一场景。

仍只准入的路径（[KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)“Lockstep 追帧链路背压（独立 Gate）”，维护者 2026-10-20 决定暂不做）：

- 独立 Gate（`wiring/gate.Transport`）：两条通道都经 `GameIngress.PushBound` 进 Game 侧有界队列，再经 NATS 交给 Gate 侧队列与 `WritePacket`；`SendReliable` 返回 nil 只表示 Game 本地准入，追帧同样不随链路放慢。慢链路上 Game 侧队列满时准入返回 `ErrReliableBackpressure`（计一次追帧失败，每 Tick 一次），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时 Gate 关闭连接。要等写出需要 Gate 在 socket 写出后再回执，是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。game-demo 换成独立 Gate 部署（`accessplayertcp.NewIngressMod`）时没有 `TCPRuntime`，`battleLinks` 让追帧页也只准入。

## 7. 公共客户端协议

RS v2 的 16 字节头保留 MsgID、Seq、PayloadSize；Flags bit0 是 push，bits1..2 区分 PB=0、Sync=1、Lockstep=2，值3及保留位拒绝。Lockstep 上行 flags=4，push=5。MsgID 0 的心跳不能携带业务类型标记。

C# 客户端提供 TCP、Sync receiver、Lockstep codec/assembler，Unity 提供主线程适配源码。Go wire 与生成 TCP 共享头验证；业务载荷类型须与注册路由一致。正式 Go/C# 及 Unity 源码测试不代表 Unity/Godot/Unreal 实机已验收；原生 C++ 客户端不在本版交付范围。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
