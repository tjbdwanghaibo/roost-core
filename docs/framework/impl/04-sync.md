# 核心：Sync、Lockstep 与客户端：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/04-sync.md)

## 如何阅读

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/sync`、`framework/sync/syncstream`、`infra/network/gateway`、`client/wire`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 持久水位和 Guard 冻结共同约束状态可见性。
2. 客户端状态缺口恢复全量，不在失效 base 上继续应用。
3. Lockstep 席位来自认证 session；历史默认按 `HistoryRetainFrames`（3600 帧）在 Tick 末尾自动裁剪。落后会话按 `MatchConfig.Recovery` 恢复：catchup（没配快照来源时的缺省）越界以 `ErrCatchupUnservable` 失败并发 C9 v2 通知、客户端终止；auto / snapshot 越界改发业务快照（CA 01 分块）再续帧，只有快照路线最终失败才通知终止。`Tick` 从不等快照来源，恢复中的会话钉住历史但有上限（§2D）。
4. 有限投递、流保留与业务 ACK 语义不能等同 exactly-once。
5. `Manager.Drain` 不放弃任何条目；ctx 结束时返回带卡住条目摘要的 `*DrainStalledError`，`errors.Is` ctx 错误不变。

## 2A. 冻结、发送和客户端权限实现走读

[mode.go](../../../framework/sync/entitysync/mode.go)的CaptureSync先取得当前subject及授权profiles，确认对象仍有效，调用SubjectSyncState.FreezeSyncViews；替换后的旧state不能留下可发送冻结物。reserveFrozen以CAS计入总字节，返回一次性释放函数，避免重试/取消双重释放。WakeSync合并通知，丢一个唤醒不等于丢pending事实。

[flush.go](../../../framework/sync/entitysync/flush.go)负责取pending、准备subject视图（prepareSubscribersLocked整理订阅者与跳过的会话）、按会话构建帧并逐帧交付（pushSessionFrames）、传输准入后结算；capturedAbove检查CommitLSN水位，requireSnapshotsAfterRetry处理部分会话已经接纳后的恢复。必须在实际Push结果之后结算基线，不能先标clean再尝试发送。

[command.go](../../../framework/sync/lockstep/command.go)的HandleCommand须在Match串行handler中运行：先拒绝closed/非法command，再查sessionOwners；旁观者只走SpectatorCatchup，普通玩家分别走StartCatchup/ReportHash/SubmitInput。没有seat字段让客户端覆盖服务端绑定，也没有把发送成功当模拟已执行的业务ACK。

[match.go](../../../framework/sync/lockstep/match.go)的Tick顺序是：切帧、追加历史、结算上次的追帧页、实时广播、发本次追帧页，最后retainWindow裁历史（hash报告按64帧步长）。裁剪必须在追帧页发出之后：页面发出前已编码成独立字节，先裁会让“从最早帧追帧”在同一Tick被判越界。startCatchup把起点0当帧1核对，越界时登记追帧状态并立即返回`*CatchupUnservableError`；pumpCatchup发现游标早于`History.start()`时把状态标为unservable、发C9通知（作为该会话最后一次可靠发送，结算后删除状态）。客户端侧[assembler.go](../../../framework/sync/lockstep/assembler.go)的Receive / Ingest按首字节0xC9识别通知，[robot/lockstep.go](../../../robot/lockstep.go)与C# LockstepClient随即终止。以上是catchup模式；auto / snapshot模式下越界与“游标被窗口越过”都转入快照阶段，实现见§2D。

[drain.go](../../../framework/sync/entitysync/drain.go)的Drain只在ctx结束时收集诊断：pending按ID升序，附Flush记录的deferral（原因、首次暂缓时间、最后错误，由settleDeferrals在Flush末尾维护：被暂缓的重新排队并保留首次时间，本轮处理过、没再暂缓的删除，forget/Close清空）；随后是waitingSnapshots、政策阶段的释放/重新提交通知与各政策的PolicyHooks.Stalls。诊断只读，不改变排空判定与重试；指标无标签。

[packet.go](../../../client/wire/packet.go)从flags解释Kind并在Header.Validate拒绝保留组合、超长载荷和非法心跳标记。头解码不解释业务payload，注册路由与Lockstep/Sync解码器仍各自校验。

## 2B. 工作分支的空间组件与政策发布顺序

[Block AOI实现说明](../INTEREST-BLOCK-AOI.md)给出锁与作用域。`policy.SpatialComponent`用手写writer写DAO，Queue*复用`SyncConditionFor`。`queueMu`不包住AOI计算；每批先写空间、再并行observer、最后归并所有来源，防止来源切换重建基线。

`SubjectSyncState.HoldSyncPublication`阻止内容越过尚未应用的政策事实；它只约束PrepareViews，不改变提交确认条件、不阻止锁内冻结。事实应用、丢弃、退出与关闭都必须释放，不能只在成功路径释放。

## 2C. 自身视图（SelfView）的生命周期

[self_view.go](../../../framework/sync/entitysync/policy/self_view.go) 是“玩家看自己状态”的唯一生命周期实现（DESIGN-TODO D6，维护者 2026-10-16 决定折中方案）；传输接线在 [wiring/selfsync](../../../wiring/selfsync/selfsync.go)，生成器只写调用（`access.player.self_sync`）。不变量与强制点：

- **回调只记事实**：`Activate` / `Deactivate` / `SessionLost` / `EntityLoaded` 只在 `SelfView.mu` 下改连接集合与标志、把玩家放进 pending，锁内不调用 Manager / Direct，不等待——可以在连接协程、Gate 控制协程、业务池（`EntityLoaded` 来自 DataEngine 的加载回调）里调用；不开 goroutine，不起定时器，不读时钟（政策阶段读一次 `time.Now` 只用于滞留指标）。
- **对齐只在政策阶段**：`apply` 由 `RegisterPolicy` 装在 Flush 开头（flushGate 串行），按玩家此刻的**净事实**对齐：有连接 → 会话已开（新连接到来则 `HoldSession` + `ReadySession`，新 epoch 全量）→ 内存里有实体就 `Register` → `Direct.Bind(player, subject, SelfProfile)`；没有连接 → `CloseSession` 再 `Unregister`。登录 / 断线 / 顶号 / 关闭与 Flush 的任意交错只影响事实的最终值，不会出现旧连接的关闭拆掉新连接的视图（`TestSelfViewReplacedConnectionKeepsTheView`、`TestSelfViewLoginAndDisconnectRace`）。
- **waiting**：在线但实体不在内存（首登新建、未加载）、或登记 / 绑定被拒的玩家，每个政策阶段复查一次（一次 `EntityManager.Get`，只读内存）。加载有 `EntityLoaded` 加速，新建靠复查（`TestSelfViewWaitsForTheEntityToAppear`）。拒绝原因只在变化时记日志。
- **框架撤销沿用 Direct**：卸载后重载不了退回 remove 时 Direct 保留绑定，实体再次加载、`Register` 之后由 Direct 重新提交（`TestSelfViewUnloadRetractionIsResubmitted`）；会话被关闭 / 丢掉、subject 被注销（事务内新建回滚）时 Direct 删除绑定并经同包的 `forgotten` 通知 SelfView 重新 Bind（`TestSelfViewRolledBackCreateIsBoundAgain`）。
- **推送被拒即断开，不在原连接上重开**（维护者 2026-10-17 批准“A + A'”，针对这条路径反复出缺陷的方向调整，见下）：传输统一用 [`wiring/selfsync.PushPlayerFrame`](../../../wiring/selfsync/selfsync.go) 分类推送结果——ctx 结束原样交还（Manager 自己整轮重试，不断开任何人）；接入层未就绪 / 已关闭（`gateway.ErrTransportUnavailable`、`nettransport.ErrTransportClosed`），或玩家**每条**连接都因整个进程的驻留字节预算耗尽而拒收（`nettransport.ErrResidentBudgetExhausted`）→ `entitysync.ErrRetryLater`，整轮重试、保留脏状态，连接与会话都不动；全部是 `gateway.ErrSessionNotFound` 且此刻也数不到连接（玩家没有连接）→ 原样交还，之后来的连接是新激活 / 新登录、自己打开会话；报“没有连接”却数得到连接时按下一类处理；其余拒收（可靠队列满、绑定失效、帧超限，或只有部分连接因预算拒收）→ 先 `PlayerRuntime.CloseSessions(player, ErrSyncPushRefused 包原错误)` 关闭该玩家的**全部**连接，再交还错误让 Manager 关同步会话。同步会话按玩家开，关掉之后留下的任何连接都会处在“连接活、同步会话死”的状态，所以一条不留；部分连接因预算拒收也关，因为收下这一帧的连接会把整轮重试里同一 tick 的帧当旧包丢掉、之后的增量基线对不上。`SessionLost` 之后 SelfView 只标 `selfBinding.lost`、不重开，等 `Deactivate` 收尾（注销 subject）；在那之前有新连接激活（`reset`）就清掉 `lost`、按正常激活打开新会话（新 epoch、全量）。回归：`TestSelfViewWaitsForTheConnectionToCloseAfterALoss`、`TestSelfViewEveryIsolatedLossIsRecoveredByTheReconnect`、`TestSelfViewLossRacingANewConnection`（policy），`TestTransportClassifiesRefusedPushes`、`TestSelfSyncOverTCPClosesAStalledReaderAndTheReconnectGetsAFull`，以及真实 Gate + Game Ingress + NATS 的 `TestSelfSyncOverGateRecoversAfterEveryRefusedPush`（12 次偶发背压，每次经断开重连恢复）、`TestSelfSyncOverGateRetriesTheTickWhenTheResidentBudgetIsExhausted`（`-tags integration`，需要 `ROOST_DATAENGINE_IT_NATS_URL`）。
- **关连接不阻塞**：`PushPlayerFrame` 在 Manager 的 Flush 里调用（同步循环协程，或 `Drain` / `Stop` 的调用方），不在 Nest 快池。嵌入 TCP 的 `closeSessions` 取读锁复制目标、`tcpSession.close` 先关 socket（打断正在进行的写）再取写锁记原因；Game Ingress 的 `CloseSessions` 取 binding 表与 `game.mu` 短锁、非阻塞投递 Gate 关闭通知、`queue.Remove` 只取消队列，激活回调在途时收尾放到已有的 `retire` 收尾协程（每个被关闭的绑定一个，既有行为，不是每帧一个）。关闭通知（嵌入 TCP 的 `Closed` 钩子、Ingress 的 `OnClosed`）异步送达，`Deactivate` 随后到。
- **进程驻留预算耗尽**：整轮重试时 Manager 作废本轮捕获、保留脏位（`markPending`，不唤醒），本轮已准入的会话下一轮收全量（`requireSnapshotsAfterRetry`），排在被拒会话之后的会话本轮不推送。业务提交不受影响：on_change 冻结内容受 `MaxFrozenBytes`（默认 64MiB）约束，超出时冻结被拒、脏位留给之后的捕获。发送侧每帧从入队起最多驻留 `outbound.max_age`（默认 250ms）：发不出去的绑定在到期时失败并被关闭、释放它占的预算，所以预算不会被停滞的绑定长期占住；持续耗尽只发生在健康流量持续超过发送能力时，表现是同步延迟上升与“已准入会话改收全量”的放大，见 KNOWN-LIMITS。嵌入 TCP 的进程驻留预算（`player_access.tcp.outbound.resident_bytes`，`gateway.ErrResidentBudgetExhausted`）走同一条整轮重试；那里没有 `max_age`，停读连接占着的预算要到它撞上每连接上限或 `write_timeout` 写超时才随写协程退出归还（[GATEWAY §4](../GATEWAY.md#嵌入-tcp-的出站)）。
- **Game Ingress 激活回调期间即可推送**：`OnActive` 里 `Activate` 会唤醒同步（on_change 立即 Flush），若绑定要等回调返回才算可推送，首帧推送得到 `ErrSessionNotFound`、会话被丢而连接活着，删除重开之后就再也打不开。[`playerResources`](../../../infra/network/gateway/player_runtime.go) 把激活回调执行中的绑定（`activationDone` 非空、未失败）也算作可推送：Gate 先物理写出 AuthACK、准备好接收出站才发 Activate（`Gate.AfterACK`），与按会话推送（`resourceForSession`）口径一致；激活失败的绑定随即关闭。回归 `TestAnIngressBindingIsPushableWhileItsActivationCallbackRuns`、`TestSelfSyncOverGateDeliversTheFirstFullWhenSyncRunsInsideActivation`。
- **滞留指标**：在线（至少一条连接）却没有打开的会话的玩家记在 `unopened`（何时起），每个政策阶段末尾数出超过 `selfStrandedPeriods`=3 个同步周期的人数（`SelfView.Stranded`）；`wiring/selfsync` 在 SelfView 之后登记的政策钩子把它写成 Gauge `entitysync.self_view.stranded_players`。正常为 0；持续不为 0 说明传输没有按约定关闭拒收的连接、会话打开被持续拒绝（Manager 会话数上限），或玩家实体的帧编码失败（Manager 不经传输丢会话，连接不被关闭，见 KNOWN-LIMITS）。回归 `TestSelfViewCountsPlayersOnlineWithoutASession`、`TestSelfSyncReportsPlayersOnlineWithoutASession`（`testing/synctest` 控制时间）。
- **Manager 的 epoch 单调递增保留为防御**：`OpenSession` 与 `HoldSession` 都取本 Manager 的下一个 epoch（`Manager.nextEpochLocked`），同一接收端即使收到新 lifetime 的帧也会接受其全量（`TestAReopenedSessionStartsAboveEveryEpochItsReceiverHasSeen`，entitysync）。
- **game-demo 场景同一规则**：场景的 lane（`demo/internal/service/game/scene.go.tmpl` 的 `sceneLane.Push`）直接调 `PushPlayerFrame`；`settle` 不再在丢会话之后重开（删除 `sessionDropped` 分支），玩家还连着就只可能是之后的新登录，由它的 `Join` 打开会话。保留的只有 `Join` 打开会话时“上一个会话还在关闭”（`SessionOpenRetryable`）的有界退避重试（`openLater` / `openLocked`，8 次），计数改名 `scene_session_open_failed_total{reason}`。回归 `TestAConnectionThatRefusesAFrameIsDisconnectedAndTheRejoinRestoresTheView`、`TestASessionOpenRefusedWhileTheOldOneClosesIsRetried`（生成的 game-demo）。
- **方向调整的缘由**：“推送被拒 → Manager 关同步会话 → 连接还活着 → 政策层自己重开”这条路径先后出过 5 次缺陷——demo 场景快速重连打不开（12d4f7ca，补有界退避）、放弃重开无痕迹（879b9094，补指标）、SelfView 初版朴素写法 5 组竞态（fc0a8315）、重开 epoch 不变被客户端静默丢弃与 on_change 紧循环耗尽（00d35150）、同一次激活里偶发丢会话第 9 次后视图永久消失（重开计数只在新激活清零）。根因是慢消费者策略在两层不一致：Manager 推送被拒只关同步会话，嵌入 TCP 与 Gate 侧队列满都关连接，唯独 Game Ingress 可靠队列背压只拒一帧、绑定保留。现在慢消费者规则在传输一处（`PushPlayerFrame` 与 `entitysync.AsyncTransport` 同一条），“连接活、同步会话死”不再是政策需要恢复的状态，恢复只走客户端重连这一条正常路径。
- **首个全量在登录回包之后**：嵌入 TCP 的激活在 `TCPHandshake.AfterACK`（ACK 已同步写出、连接已登记）里，推送进同一条 FIFO 出站队列；独立 Gate 是 Bind → ACK → Activate。`wiring/selfsync` 的 `TestSelfSyncFirstSnapshotFollowsTheLoginACK` 把一次 Flush 挤进握手回调里连跑 20 轮，客户端仍先读到 ACK。
- **所有权：一个内容状态一个 Manager**（2026-10-20，维护者批准）：SelfView 拥有其 Manager 里玩家实体这类 subject 的登记。脏通知器、冻结内容与在途捕获在 `SubjectSyncState` 上各只有一份，第二个 Manager 登记会换掉第一个的通知器（第一个 Manager 的订阅者从此收不到变化）。强制点在状态上：`SubjectSyncState.ClaimSyncOwner` / `ReleaseSyncOwner`（[subject_sync.go](../../../framework/entity/subject_sync.go)）记一个主人；`Manager.register` 在 `m.mu` 内、放进表之前认领（锁序 m.mu → 状态锁，与 `forget` 相同），`rebind` 在 `subj.mu` 内认领新状态、放开已关闭的旧状态，`forget` 与 `Close` 在清通知器的同一处释放。被拒时本 Manager 不变，返回 `ErrSubjectRegisteredElsewhere`，信息点名实体 / kind / 命名空间与持有它的 Manager（`ManagerConfig.Name`，`Manager.String`；wiring/nest 缺省 `nest`，自身视图 `self_sync`，game-demo 场景 `scene`）。同一个 Manager 重复登记仍是空操作；注销后要等退役完成（remove 交付或订阅者会话关闭，即 `forget`）才放开：退役期间旧 Manager 仍欠订阅者 remove、仍持有通知器，提前放开会让它的 `forget` 清掉新 Manager 的通知器。SelfView 登记被拒时留在 waiting，按错误文本去重记一次 Warn（`refused`）并计入滞留指标。`ConfigureUnloadResync` 的“已接线”拒绝同样点名已接上的 Manager，所以 game-demo 打开 self_sync 时启动在 `service game init` 失败、信息指向 `self_sync`。回归 `TestASubjectStateFeedsOneManager`（entitysync；修前第二个 Manager 登记成功，第一个 Manager 的订阅者对之后的变化收到 0 帧）。
- **帧上限取接入层上限**（2026-10-20）：Manager 的 `Limits.MaxFrameBytes` 在 `NewManager` 时经 `FrameSizeLimiter` 定下、之后不变，所以接入层上限必须在 Nest Mod Init 之前交给 `wiring/selfsync`。`wiring/nest.EntitySyncSetup` 新增 `ConfigSchema` / `LoadConfig`：Nest Mod 把前者并入自己的声明（App 在任何 Mod Init 之前检查一次，`--check-config` 同一处），在 `Configure` 与 `NewManager` 之前调后者。selfsync 以与生成的 player TCP Mod 逐字相同的声明读 `player_access.tcp.max_payload_bytes`（不同即 `configschema.Merge` 报冲突；codegen `TestPlayerTCPDeclarationAgreesWithKit` 经快照 `kitconfig_gen.go` 的 `selfsync` 组钉住）。独立 Gate 部署下生成的 `IngressMod.WithSelfSync` 在装配期调 `SelfSync.LimitFrameBytes(Config.MaxPayloadBytes)`（Manager 建好后再调返回错误，经 `IngressMod.Init` 报出）。`SelfSync.FrameLimit` 取 `Config.MaxFrameBytes`、配置上限、Ingress 上限中非零者的最小值，都没有时取 gateway 缺省。以前帧超过接入层上限时被接入层拒收 → `PushPlayerFrame` 关连接 → 重连全量同样超限，反复重连；现在单个玩家实体超过上限时在 Manager 内编码失败（KNOWN-LIMITS 第 ③ 条：不关连接、滞留指标告警）。回归 `TestSelfSyncFrameLimitFollowsTheTCPPayloadLimit`（真实 Nest Mod Init + 真实 TCP；修前读到 `EOF`、`push_refused{outcome=closed}` 0 → 1）、`TestSelfSyncFrameLimitTakesTheSmallestAccessLimit`（wiring/selfsync），生成工程里的 `TestSelfSyncFrameLimitFollowsTheAccessLimit`（`New()` 的配置检查 + 嵌入 TCP 与 IngressMod 两种接线，由 codegen `TestSelfSyncGeneratedProjectOverRealTCP` 运行）。

## 2D. Lockstep 快照恢复的实现契约

快照恢复（2026-10-20，DESIGN-TODO D1 方案 B）在 [snapshot.go](../../../framework/sync/lockstep/snapshot.go)（配置、判定、取快照、分块、钉住）、[snapshot_wire.go](../../../framework/sync/lockstep/snapshot_wire.go)（CA 01 编解码与客户端重组 `SnapshotReceiver`）、[match.go](../../../framework/sync/lockstep/match.go)（`catchupState` 与 `pumpCatchup` / `settleCatchup` 接线）、[wire.go](../../../framework/sync/lockstep/wire.go)（C9 v2）。设计、各期偏差与证据见[功能设计 §14](../../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#14-实施记录)；用法见[使用篇](../guide/04-sync.md#落后的会话怎样回来追帧快照或自动)。下面是修改时必须保持的东西。

**配置只解析一次**。`resolveRecovery` 在 `NewMatch` 里把 `Recovery` 零值解析成 catchup（没配 Provider）或 auto（配了），之后 `m.recovery` 不会是 `RecoveryUnset`；snapshot / auto 要求 Provider 与 `Reliable` 都非 nil；`MaxBytes` 不超过 `MaxSnapshotBytes`；可靠通道实现 `nettransport.ReliablePayloadLimiter` 时核对 `ChunkBytes + MaxSnapshotChunkHeaderBytes` 不超过 `MaxReliableBytes`；可靠通道实现 `nettransport.ReliablePacer` 且返回 true 时每 Tick 预算取 `BytesPerTick`，否则取 `UnpacedBytesPerTick`（只准入）。`m.snapshots` 只在配了 Provider 时非 nil；catchup 模式的代码路径与快照前逐字节相同，`retainWindow` 在没有恢复中会话时只多一次空 map 判断（热路径前后 benchstat 无差异，[样本](../../performance/benchmarks/lockstep-snapshot-p1-20261020/benchstat.txt)）。

**每会话一个状态机**（`catchupState`，只在 Match 的串行 owner 里改）：

| 阶段 | 字段 | 下一 Tick 的 `pumpCatchup` 做什么 | 钉住点（`recoveryPin`） |
| --- | --- | --- | --- |
| 追帧 / 续帧 | `snapshot == nil`；`recovered` 表示快照之后的续帧 | 发下一页 C7；游标早于窗口时 catchup 置 `unservable`，auto / snapshot 调 `beginSnapshot(SnapshotOutOfWindow)` | `recovered` 时钉游标，普通追帧不钉 |
| 等快照 | `snapshot != nil && snapshot.image == nil` | 不发送，只标记需要取快照 | 进入快照阶段时的窗口起点 `snapshot.pin` |
| 传快照 | `snapshot.image != nil` | `launchChunks` 从 `snapshot.offset` 发一批 | `image.Frame + 1` |
| 失败待通知 | `unservable != nil` | 发 C9 v2（这次恢复的最后一次可靠发送），结算后删除状态 | 不钉 |

- 进入快照阶段只有 `beginSnapshot` 一处：取消在途的追帧页（`state.drop()`）、清 `recovered`、计 `lockstep.snapshot.total{reason}`。调用点：`routeRecovery`（请求时判定，auto 超阈值时 `fallback = true`）、`pumpCatchup` 里游标被窗口越过、`startSnapshot`（宿主强制）。
- 幂等：`startCatchup` 在会话已有 `snapshot` 时直接返回 nil；`routeRecovery` 对 `from >= state.next` 的重复请求原样继续、不重新判定；`unservable` 在途时返回同一错误。
- 结束：`failRecovery` 把状态改成待通知（取消在途发送、清快照阶段）；`settleChunks` 传完后 `state.next = image.Frame + 1`、`recovered = true`，快照就在头部时直接删状态回到实时；续帧页追到头由 `settleCatchup` / `collectCatchups` 删状态。`Detach` / `DetachSession` / `DetachSpectator` 与同一席位 `Attach` 到新会话都经 `dropCatchup` 删状态并 `releaseIdleFetch`；状态处于快照恢复中（`snapshot != nil` 或 `recovered`，且不在待通知阶段）时计一次 `failed{reason="detached"}`（维护者 2026-10-20 决定 A，`TestMatchSnapshotRecoveryDetachIsCounted`：三个阶段、座位与旁观者各计 1；普通追帧、失败通知在途、`Match.Close` 计 0）。`Close` 不经 `dropCatchup`，取消全部在途发送与取快照，不计数（对局结束不是恢复失败）。

**取快照：Match 自己的 goroutine，Tick 不等**。

- `launchFetch` 在 `pumpCatchup` 末尾、本 Tick 有会话在等且 `m.fetch == nil` 且本 Tick 没刚收到结果时发起，每局同一时刻一次：请求范围按发起时的窗口定（`MinFrame = history.start() - 1`，`MaxFrame = servableHead()`，即 `history.Latest()`，配了已提交门时不超过已提交帧；`Reason` 取第一个等待会话的原因），ctx 是 `context.WithoutCancel(Tick ctx)` 加 `FetchTimeout`，结果经容量 1 的 channel 交回。
- `fetchSnapshot` 在调用 goroutine 内完成全部校验与 SHA-256：panic recover 成 `errSnapshotProviderPanic`（记 ERROR）、超过 `maxBytes` 为 `ErrSnapshotTooLarge`、空数据或帧号越界为 `ErrSnapshotInvalid`；`Tick` 只收结果。
- `collectFetch` 用 `select default` 非阻塞收取；到 `deadline` 仍没结果就按 `context.DeadlineExceeded` 结算、不再等那个 goroutine（Provider 不响应 ctx 时它可能还在跑，下一 Tick 可再发起一次）。
- `applyFetch` 把一次结果分给此刻所有等快照的会话：成功时每个会话拿同一个只读 `snapshotImage`、各自新的 `transfer` 编号（`m.lastTransfer` 单调递增）、偏移 0、失败计数清零；快照帧 + 1 早于当前窗口起点（只有宿主 `TrimHistory` 越过钉住点才会发生）按 `ErrSnapshotInvalid` 计该会话一次失败。失败时按错误定标签（too_large / invalid / 其余 unavailable）计一次 `failed`，然后逐会话：too_large 立即 `failRecovery(snapshot_too_large)`；`fallback` 且游标仍在窗口内的回退追帧（`state.snapshot = nil`，不计会话失败）；其余 `snapshotAttemptFailed`，达到 `CatchupMaxFailures` 时 `failRecovery(snapshot_unavailable)`。
- `releaseIdleFetch`：没有会话再等快照时取消在途的取快照、丢掉 `m.fetch`，几 MiB 的结果不挂在 Match 上。`pumpCatchup` 在本 Tick 没人等时、`pinnedTrim` 让会话过期后、`dropCatchup` 时调用。

**分块与重发**。

- `launchChunks` 在后台 goroutine 上从已确认偏移起逐块 `SendReliable`，直到本批确认字节达到预算或传完；每批至少一块，所以实际每批字节是预算向上取整到整块。整批受 `CatchupSendTimeout`（同一个 ctx），交回 `written`（确认写出的数据字节，失败时为失败前已成功的部分）。分块字节由 `snapshotImage.chunk` 每次重新编码，首块（偏移 0）带头部。
- `settleChunks`：`offset += written`；有进展清零失败计数并计 `lockstep.snapshot.bytes.total`；有错误计一次失败，达到 `CatchupMaxFailures` 时删状态、计 `failed{abandoned}`、不发通知（`Tick` 错误带 “snapshot transfer abandoned … the client must reconnect”），否则下一 Tick 从确认偏移重发。超时但其实已写出的块会被再发一次，客户端按 “同编号、偏移小于已收” 丢弃。
- 嵌入 TCP 的“确认写出”是 `PushLockstepSessionWritten` 返回 nil，即写进本机 socket 发送缓冲；独立 Gate 是 Game 本地准入。两者都不是客户端已收到。
- 客户端 `SnapshotReceiver.Accept` 是重组规则的唯一实现（Go 客户端与服务端测试共用，C# `LockstepAssembler` 逐条对照）：新编号且偏移 0 取代未完成的旧传输；新编号偏移非 0、偏移跳跃、越过 total、首块 total 超过客户端上限是坏包（`ErrFrameCorrupt`，丢弃进行中的传输）；同编号偏移小于已收（含已完成传输的重发）丢弃；到齐后核对 SHA-256，不符为 `ErrSnapshotCorrupt`（编号仍记着，之后的重发被丢弃）。

**钉住**。`retainWindow` 在有恢复中会话且配了快照时调 `pinnedTrim(keep)`：裁剪点 = min(正常裁剪点, 所有会话的 `recoveryPin`)，下限 `keep - PinFrames`；钉住点早于下限的会话 `failRecovery(recovery_expired)`、计 `failed{expired}`，释放钉住。宿主 `TrimHistory` 直接裁，不看钉住（之后 `applyFetch` 与续帧路径按上面的规则兜住）。

**客户端（Go）**。`FrameAssembler.Receive` 按首字节分流 C9 / CA / C7；交出快照后置 `hasUnapplied`，`Receive` 与 `IngestFrames` 在 `ResumeAfterSnapshot` 前都返回 `ErrSnapshotNotApplied`；传输进行中 `IngestFrames` 丢弃帧并计入 `Duplicates`。`ResumeAfterSnapshot` 清空整个乱序缓冲：只清 `<= frame` 时缓冲里的 `frame + 1` 永远不会被排空，`Gap()` 恒真、bot 周期性重发追帧（`TestFrameAssemblerResumeDropsBufferedFramesPastTheSnapshot`，C# 同一处理）。`robot.LockstepBot`：首字节 0xCA 且 `Receive` 报错一律终止（C7 坏包仍按“请求追帧”）；传输未到齐也算缺口未合（`checkGap` 看 `SnapshotInProgress`），推进中的分块把重试计数清零，避免分块数多于 `CatchupRetryPackets` 时重复请求；恢复时丢弃 pending 帧与 apply 游标，`FrameHasher.Reset(got)`。

**错误与指标**。取快照、分块、续帧、钉住的失败都进 `Tick` 返回的 joined error（auto 超阈值回退追帧不进），按会话写明 `catchup session N`；通知 C9 v2 的原因由 `CatchupUnservableError.Reason` 决定，`Reason` 为 0 按 history_trimmed 解释（`EncodeCatchupUnservable` 保留为 history_trimmed 的写法）。指标名是包常量 `MetricSnapshot` / `MetricSnapshotBytes` / `MetricSnapshotFailed`，由 `TestMatchSnapshotMetricNames` 与根包指标门禁钉住；`failed` 的 reason 取值：unavailable / too_large / invalid / abandoned / expired / detached，口径见 [OBSERVABILITY](../OBSERVABILITY.md)。

**已提交门**（`MatchConfig.CommittedFrame`，2026-10-20，[设计 §15](../../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#15-room-接入已提交门2026-10-20)）。不变量：配置后，可靠通道上发出的 C7 帧与快照首块的快照帧都 ≤ 发起发送时 `CommittedFrame()` 的值。强制点只有两处，都经 `servableHead()`（`min(history.Latest(), committed())`，nil 时就是 `Latest()`）：`pumpCatchup` 开头算一次 `head`，读页上界取 `min(catchupBatch, head - next + 1)`，`next > head` 且 `next <= Latest()` 的会话 `continue`（不删恢复状态、不发页）；`launchFetch(ctx, head)` 的 `MaxFrame = head`。第三处是活性：Room 这种每 Tick 同步提交的宿主，页面最多发到 `Latest() - 1`、游标停在 `Latest()`，`settleCatchup` 的“越过最新帧”永远不成立，所以 `collectCatchups` 在配了门时也检查空闲（无在途发送、非快照阶段、非失败通知）的会话，落进本 Tick 冗余包覆盖范围（`next + depth > Latest()`）就删状态、切回实时——之后它收的是宿主提交后才发的广播。没配门时三处与改动前逐字节相同（空闲会话照旧由 `pumpCatchup` 发页）。门只挡 Match 自己在后台发的东西；实时广播由宿主的 Datagrams 负责挡到提交之后。回归 `TestMatchCommittedFrameGatesCatchupAndSnapshots`（去掉门报 `catch-up page carried frames [15 ... 24], want 15..20` 与 `snapshot requests MaxFrame [201], want [200]`；去掉空闲切回实时报 `session did not return to live broadcasts once commits caught up`）；业务形态见 MOBA 夹具 Room 的两条用例。热路径对照见[样本](../../performance/benchmarks/lockstep-room-gate-20261020/benchstat.txt)（无差异）。

**修改检查**：改已提交门跑 `TestMatchCommittedFrameGatesCatchupAndSnapshots` 与 `scripts/test-moba-generated.sh`（Room 两条）；改判定先跑 `TestMatchRecoveryRouting`（三种模式 × 窗口内 / 超阈值 / 越界）与 catchup 模式的全部原有用例（必须不改即通过）；改取快照跑 `TestMatchSnapshotProviderRunsOffTheTick`、`TestMatchSnapshotFailureSemantics`、`TestMatchSnapshotFetchIsCancelledWhenNobodyWaits`；改分块跑 `TestMatchSnapshotChunkPacingAndResend`、`TestMatchSnapshotUnpacedLaneUsesTheFixedBudget`、两个 fuzz 与 `tcp_test.go` 的真实回环用例；改钉住跑 `TestMatchSnapshotPinsHistoryWithinBound` 与长局用例。并发相关加 `-race -count=2`。

## 2E. Interest 的视野（D19，在 D14 上扩展）

维护者 2026-10-20 决定（D14 A1 + B1，D19 七项），用法见 [guide/04-sync](../guide/04-sync.md#谁看见谁视野viewd19)。实现要守住的几条：

1. **三种编号不混用**。`Interest.held` 的键是 `(玩家 ID, 实体 ID)`，`Refusal.Observer` 是玩家 ID（= 会话 ID，`sessionOf` 是恒等换算）；业务视野按 `ViewID`（业务分配，0 保留给主视野）登记在 [view.go](../../../framework/sync/entitysync/policy/view.go) 的 `viewRegistry.views`，主视野按玩家登记在 `mains`；每个视野另有一个内部编号 `view.handle`（从 1 递增、不复用），它是空间视野在 `AOI.observers` 里的观察者编号、`InterestEvent.Observer` 的值，也是 `hold.layers` 的来源键 `contribution{view: handle, block}`（`view == 0` 是 self）。`AOI` / `AOICluster` 不知道玩家：观察者就是一个空间视野，自排除只比 `interestObserver.self`（视野跟随的实体），[aoi.go](../../../framework/sync/entitysync/policy/aoi.go) `evaluatePair`、`evaluateObserver` 候选筛选、[aoi_block.go](../../../framework/sync/entitysync/policy/aoi_block.go) `evaluateObserved` 三处。
2. **合并只有一处**。`contribute(pair, source, layer, enter)` 是 held 唯一的写入口：`hold.layers[source] = layer` 或删除，并把 pair 记进 `touched`；`applyLocked` 先 `syncViewsLocked`（应用登记表变更）、再 `drainAOILocked`（AOI 事件按 `spatialPlayers[handle]` 扇出给已生效的挂接玩家），最后按 `touched` 有序结算：`layers` 为空才 `Unsubscribe`，否则首次 `Subscribe`、层级变了重订。不变量：AOI 事件排空之后，玩家 p 对空间视野 h 的贡献恰好等于 h 当前的 `visible` / `blockVisible`——挂接（`seedSpatialLocked(enter)`）与解除（`seedSpatialLocked(!enter)`）都先 `drainAOILocked` 再按当前可见集写，所以解除不会漏删、也不会多删。
3. **登记表是视野事实的唯一位置**。`viewRegistry`（`viewMu`）保存视野定义、挂接、关系成员与 `memberOf` 反查；业务视野操作只取 `viewMu`，当场校验并改事实，再 `record` 一条 `viewOp`：关系变更在记录时按当时的玩家 × 成员展开成 `opRelation`（应用时不再读登记表，所以几次变更的先后就是调用顺序），空间变更记完整放置参数（`opSpatialPlace`，`positioned` 区分显式视点与“留在当前位置”）、挂接 / 解除 / 移除。`syncViewsLocked` 在每个持 `mu` 的入口开头调用（`Enter`、`Move`、`Leave`、`Hide`、`Apply`、`applyQueued` 的批次写阶段之前……），直接操作看到的视野与调用顺序一致；它不在 AOI 的批次写阶段内调用。锁序 `mu → viewMu`、`mu → queueMu`，`viewMu` 与 `queueMu` 不同时持有；`record` 只调 `Manager.WakeSync`（非阻塞）。
4. **跟随只在应用侧**。`followers[实体] = {handle}` 由 `placeViewLocked` 维护；`placeSubjectLocked`（`Enter` / `Show` / `Move`）与排队事实的应用在移动目标后调 `moveFollowersLocked`。跟随的实体在场时放置取实体位置；`subjectGone` 的排队事实（Leave / Hide 在应用前移走了目标）仍移动跟随者，与“先应用事实、再 Leave / Hide”逐项相同，只改额外覆盖的事实整条丢弃。
5. **层级合并**。`selectedProfile` 去重各视野给的层级；`layerCovers(a, b)` 在 `ViewSets` 的每个实体类型里都满足 `a.Fields ⊇ b.Fields` 才算包含（结果缓存在 `covers`，`ViewSets` 构造后不变）；被包含的去掉（字段相同的两个按 `Manager.CompareProfiles` 留一个），剩下的 `entity.UnionSyncProfiles`。并集身份由 [sync_view.go](../../../framework/entity/sync_view.go) 定义：`Lookup` 解析成字段并集、优先级取最小；`Manager.profilePriority`（[subject.go](../../../framework/sync/entitysync/subject.go)）对并集取各层级最优，与 `Lookup` 同一条规则，所以 `ValidateViewPriority` 对得上。Manager 的跨政策来源选择（Interest 与 Group / Direct / SelfView 同时持有一个 pair）不变，仍按优先级取一个。
6. **视野参数归一只有一处**（D14）。`AOIConfig.withViewDefaults` 补零值，`validate` 检查，`normalizeView` 是运行期唯一的夹取量化入口：`AddObserver` / `SetObserverView`、`Interest.Enter` / `Watch` / `SetObserverView`、`DefineSpatialView` / `SetSpatialView` 都经过它。`AOI.placeObserver` 是登记、移动、换视野的共同落点。
7. **上限的强制点**。`MaxVisible` / `MaxObserverBlocks` 在 AOI 里按观察者（= 视野）计；`MaxViewsPerPlayer`（含主视野）在 `viewRegistry.mainView` 与 `AttachView`，`MaxPlayersPerView` 在 `AttachView`，都在改事实之前拒绝并计 `entitysync_interest_view_limit_rejected_total{reason}`；`NewInterest` 按 `worstObserverBlocks × MaxViewsPerPlayer ≤ MaxPlayerBlocks` 核算。
8. **SetObserverView 与视野操作都不进提交门**。它们不入 `facts`、不持 `HoldSyncPublication`；`QueueMove` / `QueueSpatial` / `QueueObservedBlocks` 的提交门语义不变。排过事实或记过业务视野变更之后（`queueActive` / `registry.active`），Manager 的政策阶段 `settleQueued` 结算并重试，`queuedPending` 把未应用的视野变更计入 Drain。
9. **排队事实由应用线程应用，政策阶段只结算**（2026-10-21）。[interest_queue.go](../../../framework/sync/entitysync/policy/interest_queue.go) `runApplier` / `applyStep`：`queueFact` 只入队并非阻塞唤醒（`signalApplierLocked`，第一次时启动 goroutine）；一步在 `mu` 内按原取批规则（提交门、同 ID 不越过、`BatchSize` 不切开同一提交）应用到 AOI，应用完的事实进 `applied`，仍持发布屏障、仍计 `inflight`；`settleQueued`（政策钩子）与业务 `Apply` 走 `applyLocked`，结算后 `releaseAppliedLocked` 才释放屏障并退出预算，应用错误攒在 `applyErr` 由下一次结算返回。periodic 下提交确认没有通知，队里有未确认事实时按 1～8 ms 退避轮询；两步之间隔 `applyGather`（周期 1/10，on_change 取 `OnChangeBatchWindow`）。`wakeViews`（持 `viewMu` 调用）只读原子标志，不取 `queueMu`。`Close` 先关队列、停应用线程并等它退出（不持 `mu`），再释放 `applied` 与队列中事实的屏障。AOI 读阶段的“只评估移动过的目标”见 [Block AOI](../INTEREST-BLOCK-AOI.md)。回归：`TestFlushSettlesButDoesNotApplyQueuedFacts`、`TestApplierNeverAppliesRolledBackFactsAndKeepsOrder`、`TestApplierKeepsPerEntityOrderAcrossOutOfOrderConfirms`、`TestAppliedButUnsettledFactsKeepTheQueueBudget`、`TestAppliedFactHoldsContentUntilSettled`；AOI：`TestAOIBatchSubjectMovesDoNotRescanSettledObservers`、`TestAOIMovedOnlyEvaluationMatchesFullRecompute`。

回归：[view_test.go](../../../framework/sync/entitysync/policy/view_test.go)（多空间视野不互相退订、共享关系视野扇出、阵营视野、层级取高或并集与实际打包字段、按视野的 `MaxVisible`、挂接上限、跟随可改可空、移除视野、ID / 种类 / Leave、块预算），[policy_test.go](../../../framework/sync/entitysync/policy/policy_test.go) 的 D19 基线（`TestRelationMembershipSubscribesAndReleases`、`TestDistanceKeepsASubscriptionTheRelationDropped`、`TestDistanceBandsChangeTheSubscribedView`、`TestSelfVisibilityFollowsConfigAndLeave`、`TestTeamMembersSeeEachOtherAndLeaveCleansBothSides`，在旧 API 上写下、改结构后只改准备步骤）与 D14 的 `TestObserverAndSubjectIDSpacesStayApart`、`TestTwoObserversDoNotReleaseEachOthersSubscription`、`TestSetObserverView*`、`TestWatchSeesWithoutBeingSeen`、`TestQueuedMoveCarriesTheViewsThatFollowTheEntity`；[aoi_test.go](../../../framework/sync/entitysync/policy/aoi_test.go) `TestInterestUnionsTheLayersOfDifferentViews` 与 D14 的 AOI 用例；`framework/entity` 的 `TestSyncLayerUnionResolvesFieldsAndPriority`、`entitysync` 的 `TestUnionProfilePriorityIsItsBestLayer`。基准：`BenchmarkRelationViewMemberChange`、`BenchmarkRelationViewAttach`、`BenchmarkSpatialViewsPerPlayer`、`BenchmarkSharedSpatialView`（只量登记表与扇出，不含 Manager）；`scripts/perf/sync-aoi` 的 `-views-per-player` / `-alliance-size`。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### client/wire

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [packet.go](../../../client/wire/packet.go) | `PayloadKind`、`Header`、`Packet` |

### gateway

20 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gateway.go](../../../infra/network/gateway/gateway.go) | `Principal`、`Session`、`Request`、`Endpoint`、`EndpointFunc`、`Middleware` |
| [middleware.go](../../../infra/network/gateway/middleware.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync

16 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [counters.go](../../../framework/sync/entitysync/counters.go) | `ManagerCounters` |
| [drain.go](../../../framework/sync/entitysync/drain.go) | `DrainStall`、`DrainStalledError` |
| [errors.go](../../../framework/sync/entitysync/errors.go) | 函数/方法或内部实现；见源码 |
| [flush.go](../../../framework/sync/entitysync/flush.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../framework/sync/entitysync/manager.go) | `ManagerConfig`、`Manager`、`ManagerStats` |
| [mode.go](../../../framework/sync/entitysync/mode.go) | `SyncMode` |
| [policy_queue.go](../../../framework/sync/entitysync/policy_queue.go) | `PolicyHooks`、`RetractedSubscription`、`SubscriptionSourceHooks`、`ReleasedSubscription`、`SubscriptionStamp` |
| [session.go](../../../framework/sync/entitysync/session.go) | 函数/方法或内部实现；见源码 |
| [snapshot_budget.go](../../../framework/sync/entitysync/snapshot_budget.go) | `SnapshotBudget` |
| [snapshot_requests.go](../../../framework/sync/entitysync/snapshot_requests.go) | 函数/方法或内部实现；见源码 |
| [subject.go](../../../framework/sync/entitysync/subject.go) | 函数/方法或内部实现；见源码 |
| [subscriptions.go](../../../framework/sync/entitysync/subscriptions.go) | `SubscriptionSource` |
| [trace.go](../../../framework/sync/entitysync/trace.go) | `SyncTraceEvent`、`SyncTrace` |
| [transport.go](../../../framework/sync/entitysync/transport.go) | `SessionID`、`Transport`、`TransportFunc`、`SessionLifecycle`、`AsyncTransport`、`FrameSizeLimiter` |
| [wire.go](../../../framework/sync/entitysync/wire.go) | 函数/方法或内部实现；见源码 |
| [workspace.go](../../../framework/sync/entitysync/workspace.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync/policy

13 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [aoi.go](../../../framework/sync/entitysync/policy/aoi.go) | `InterestEventKind`、`InterestEvent`、`AOIConfig`、`ObserverView`、`AOI` |
| [aoi_cluster.go](../../../framework/sync/entitysync/policy/aoi_cluster.go) | `AreaID`、`AOICluster` |
| [direct.go](../../../framework/sync/entitysync/policy/direct.go) | `Direct` |
| [self_view.go](../../../framework/sync/entitysync/policy/self_view.go) | `SelfProfile`、`SelfViewConfig`、`SelfView` |
| [group.go](../../../framework/sync/entitysync/policy/group.go) | `GroupConfig`、`Group` |
| [interest.go](../../../framework/sync/entitysync/policy/interest.go) | `InterestConfig`、`Refusal`、`Interest` |
| [interest_queue.go](../../../framework/sync/entitysync/policy/interest_queue.go) | 函数/方法或内部实现；见源码 |
| [profiles.go](../../../framework/sync/entitysync/policy/profiles.go) | 函数/方法或内部实现；见源码 |
| [view.go](../../../framework/sync/entitysync/policy/view.go) | `ViewID`、`MainView`、视野错误 `ErrViewUnknown` / `ErrViewExists` / `ErrViewKind` / `ErrViewLimit` |

### sync/frame

3 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../framework/sync/frame/codec.go) | 函数/方法或内部实现；见源码 |
| [frame_limits.go](../../../framework/sync/frame/frame_limits.go) | `Limits`、`ObjectRef`、`SnapshotMeta`、`Kind`、`ObjectOperation`、`ComponentOperation`、`ComponentDelta`、`ObjectDelta`、`Frame` |
| [receiver.go](../../../framework/sync/frame/receiver.go) | `Receiver` |

### sync/lockstep

10 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembler.go](../../../framework/sync/lockstep/assembler.go) | `FrameAssembler`、`Delivery`、`SnapshotMismatchError` |
| [command.go](../../../framework/sync/lockstep/command.go) | `Operation`、`Command` |
| [desync.go](../../../framework/sync/lockstep/desync.go) | `DesyncDetector`、`DesyncVerdict` |
| [history.go](../../../framework/sync/lockstep/history.go) | `History` |
| [match.go](../../../framework/sync/lockstep/match.go) | `MatchConfig`、`Match` |
| [sequencer.go](../../../framework/sync/lockstep/sequencer.go) | `PlayerID`、`FrameID`、`Input`、`Frame`、`SequencerConfig`、`Sequencer` |
| [snapshot.go](../../../framework/sync/lockstep/snapshot.go) | `RecoveryMode`、`SnapshotReason`、`SnapshotRequest`、`Snapshot`、`SnapshotProvider`、`SnapshotProviderFunc`、`SnapshotConfig` |
| [snapshot_wire.go](../../../framework/sync/lockstep/snapshot_wire.go) | `SnapshotChunk`、`ReceivedSnapshot`、`SnapshotReceiver` |
| [tcp.go](../../../framework/sync/lockstep/tcp.go) | `TCPSender` |
| [wire.go](../../../framework/sync/lockstep/wire.go) | `RedundantEncoder`、`CatchupUnservableError`、`RecoveryFailureReason` |

### sync/nettransport

9 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [channel.go](../../../framework/sync/nettransport/channel.go) | `SendError`、`ErrorHandler`、`AsyncTransportConfig`、`AsyncTransport`、`AsyncTransportStats` |
| [counters.go](../../../framework/sync/nettransport/counters.go) | `AsyncTransportCounters` |
| [kcp_transport.go](../../../framework/sync/nettransport/kcp_transport.go) | `KCPTransportConfig`、`KCPTransport`、`KCPTransportStats`、`KCPDatagramHandler` |
| [quic_transport.go](../../../framework/sync/nettransport/quic_transport.go) | `QUICTransportConfig`、`QUICTransport`、`QUICTransportStats` |
| [sender.go](../../../framework/sync/nettransport/sender.go) | `DatagramSender`、`DatagramPayloadLimiter`、`DatagramBatchSender`、`ReliableSender`、`ReliablePacer`、`ReliablePayloadLimiter`、`CompositeTransport` |
| [session.go](../../../framework/sync/nettransport/session.go) | `SessionID`、`SessionInfo` |
| [transport.go](../../../framework/sync/nettransport/transport.go) | `Transport`、`DatagramBatchTransport`、`SessionTransport`、`TransportFunc` |
| [udp_crypto.go](../../../framework/sync/nettransport/udp_crypto.go) | `AEADSessionProtector` |
| [udp_transport.go](../../../framework/sync/nettransport/udp_transport.go) | `UDPTransportConfig`、`UDPTransport`、`UDPTransportStats`、`UDPReceiveHandler` |

### sync/syncbus

4 个实现文件、3 个测试文件（含示例 `example_test.go`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [delivery_id.go](../../../framework/sync/syncbus/delivery_id.go) | `DeliveryIDs` |
| [patch_syncer.go](../../../framework/sync/syncbus/patch_syncer.go) | `PatchSyncerConfig`、`PatchSyncer` |
| [subscription.go](../../../framework/sync/syncbus/subscription.go) | `Subscription` |
| [sync.go](../../../framework/sync/syncbus/sync.go) | `SyncMsg`、`Handler`、`IPublisher`、`IContextPublisher`、`ISubscriber`、`ILiveSubscriber`、`ISyncBus` |

### sync/syncbus/driver

2 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [jetstream.go](../../../framework/sync/syncbus/driver/jetstream.go) | `JetStreamSyncConfig` |
| [nats.go](../../../framework/sync/syncbus/driver/nats.go) | `Bus`（ISyncBus + StopWithContext，两种驱动都实现，SyncBusMod 在编译期依赖它） |

### sync/syncbus/mirror

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [envelope.go](../../../framework/sync/syncbus/mirror/envelope.go) | `Op`、`Envelope`、`Store`、`Replicator` |

### syncstream

8 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [file_durability_unix.go](../../../framework/sync/syncstream/file_durability_unix.go) | 函数/方法或内部实现；见源码 |
| [file_durability_windows.go](../../../framework/sync/syncstream/file_durability_windows.go) | 函数/方法或内部实现；见源码 |
| [file_journal.go](../../../framework/sync/syncstream/file_journal.go) | `FileHistoryJournal` |
| [lifecycle_journal.go](../../../framework/sync/syncstream/lifecycle_journal.go) | `HistoryMutationKind`、`HistoryMutation`、`HistoryTarget`、`HistoryJournal` |
| [observability.go](../../../framework/sync/syncstream/observability.go) | `MetricSink`、`HealthOptions`、`HealthStatus` |
| [observability_impl.go](../../../framework/sync/syncstream/observability_impl.go) | `PublisherHealthOptions`、`PublisherHealthStatus` |
| [publisher.go](../../../framework/sync/syncstream/publisher.go) | `ErrorHandler`、`ConfirmedSyncPublisher`、`Publisher`、`PublisherOptions`、`Handler`、`SubscribeOptions`、`PublisherMetrics`、`PacketPublisher`、`BufferedPublisherOptions`、`BufferedPublisherMetrics`、`BufferedPublisher` |
| [syncstream.go](../../../framework/sync/syncstream/syncstream.go) | `Observer`、`Stream`、`Packet`、`Sink`、`BatchSink`、`HistoryOptions`、`History`、`ResyncReason`、`ResyncRequest`、`ResyncResult`、`SnapshotProvider`、`StreamStatus`、`HistoryMetrics`、`HistorySnapshot`、`HistoryStreamSnapshot`、`HistoryStore` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Lockstep`、`Test.*Watermark`、`Test.*Session`、`Test.*Retry`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### client/wire

- [golden_test.go](../../../client/wire/golden_test.go)：`TestClientProtocolGolden`
- [packet_test.go](../../../client/wire/packet_test.go)：`TestPacketKindsAndGolden`、`TestPacketRefusesBadHeaderBeforeReadingPayload`

### gateway

- [binding_test.go](../../../infra/network/gateway/binding_test.go)：`TestConcurrentBindSharesOneOwnerAndOneIdentity`、`TestBindingActivationReplacementAndOldCloseAreIsolated`、`TestBindingTerminalCapacityIsReservedBeforeAdmission`、`TestSessionReplacementDoesNotCrossPlayerIdentity`、`TestAnIngressBindingIsPushableWhileItsActivationCallbackRuns`
- [gateway_test.go](../../../infra/network/gateway/gateway_test.go)：`TestChainOrderAndAuthentication`、`TestCompleteChannelEnvelopeMustFitActualLimits`、`TestGateStopRetriesShareOneDrainWaiter`
- [middleware_test.go](../../../infra/network/gateway/middleware_test.go)：`TestRateLimitByPlayerAndMessage`、`TestRateLimitBackstopHoldsWithoutLimiter`、`TestTimeoutAndRecover`、`TestRecoverIsolatesReporterFailure`、`TestRecoverIsolatesDiagnosticFailure`；其余 2 项见文件
- [packet_test.go](../../../infra/network/gateway/packet_test.go)：`TestBindingPacketHasCompleteIdentityAndCurrentFormat`、`TestBudgetConsumesElapsedTimeAndDoesNotRestartAtReceiver`、`TestSubjectSourceAndAudienceAreExplicit`
- [tcp_handshake_test.go](../../../infra/network/gateway/tcp_handshake_test.go)：`TestTCPBindFinishesBeforeACKAndActivationCanPushAfterACK`、`TestTCPRefusedOrPanickingBindNeverAcknowledgesAuthentication`、`TestTCPCloseInterruptsBlockedWriteAndPushSequencesFollowStream`
- [tcp_test.go](../../../infra/network/gateway/tcp_test.go)：`TestTCPRuntimeBelongsToOneServerBeforeStart`、`TestTCPDefaultRequestBudgetsAreAppliedAtConstruction`、`TestHeartbeatKeepsAuthenticatedPushConnectionAlive`、`TestPerConnectionTokenBucketIncludesHeartbeats`、`TestFrameRoundTrip`；其余 31 项见文件

### sync/entitysync

- [batch_window_test.go](../../../framework/sync/entitysync/batch_window_test.go)：`TestOnChangeBatchWindowDoesNotSlide`、`TestOnChangeBatchWindowTickAndExplicitFlush`、`TestOnChangeZeroWindowAndConfiguration`、`TestOnChangeBatchWindowCancelAndRestart`、`TestOnChangeBatchWindowPreservesDelivery`；其余 1 项见文件
- [benchmark_test.go](../../../framework/sync/entitysync/benchmark_test.go)
- [manager_lifecycle_test.go](../../../framework/sync/entitysync/manager_lifecycle_test.go)：`TestAsyncTransportRetriesTheTickWhenTheResidentBudgetIsExhausted`、`TestAReopenedSessionStartsAboveEveryEpochItsReceiverHasSeen`、`TestInFlightDeliveryCannotOverwriteNewIntent`、`TestStopAndCloseRespectDeadlineWhileAnotherFlushIsInFlight`、`TestStopCancelsPushAndPreventsRestartUntilItReturns`、`TestCloseReleasesStateAfterFinalFlushError`、`TestModeConfigurationRequiresProducerAndKeepsDefault`；其余 12 项见文件
- [manager_test.go](../../../framework/sync/entitysync/manager_test.go)：`TestEachSessionGetsASnapshotThenDeltasAndOnePackServesThemAll`、`TestAFailingSessionIsClosedAloneAndTheOthersStillReceive`、`TestRetryLaterAbandonsTheTickWithoutBlamingAnySession`、`TestUnsubscribeAndUnregisterOweARemoveOnlyToWhoHoldsTheObject`、`TestCloseSessionForgetsItsSubscriptionsWithoutAFrame`；其余 14 项见文件
- [resubmit_test.go](../../../framework/sync/entitysync/resubmit_test.go)：`TestRetractedSubscriptionsWithoutAPolicyAreNotRestored`、`TestRetractedPolicySubscriptionsAreHandedBackOnceAfterReRegistration`、`TestReleasedOrUnregisteredRetractionsAreNotHandedBack`、`TestResubmitInterruptedByASecondRetractionIsHandedBackAgain`、`TestQueuedResubmitReturnsToTheRetractionTableOnce`；其余 4 项见文件
- [retirement_test.go](../../../framework/sync/entitysync/retirement_test.go)：`TestRetractSyncSubjectRemovesHeldObjectsBeforeRecreate`、`TestRetractSyncSubjectDoesNotRetireALaterRegistration`、`TestRegisterAfterRetirementCompletesOnceTheRemoveIsOut`、`TestRegisterAfterRetirementCompletesWhenTheLastSubscriberCloses`、`TestAQueuedRegistrationIsBoundedAndCancellable`；其余 11 项见文件
- [snapshot_budget_test.go](../../../framework/sync/entitysync/snapshot_budget_test.go)：`TestProfileDowngradeReplacesClientFields`、`TestWholeEntityPacketsSplitAndRetainIndependentFrameState`、`TestWholeEntityTooLargeIsExplicitAndNeverTruncated`、`TestWholeEntitySplitRetryContinuesFrameAndContentBaselines`、`TestSnapshotBudgetRotatesAndDoesNotDelayLiveUpdates`；其余 14 项见文件
- [snapshot_scheduling_test.go](../../../framework/sync/entitysync/snapshot_scheduling_test.go)：`TestLargeSnapshotFiniteBacklog`、`TestLargeSnapshotStarvedUnderChurn`、`TestLargeSnapshotSingleSession`、`TestByteBlockedWindowDoesNotRecaptureUntilNextWindow`、`TestByteBlockedSnapshotsAreNotRecapturedEveryWindow`；其余 9 项见文件
- [subscription_test.go](../../../framework/sync/entitysync/subscription_test.go)：`TestSubscriptionIndexTracksAllRemovalPaths`、`TestSessionRecoveryDoesNotLockUnsubscribedSubjects`、`TestSubscriptionIndexConcurrentLifecycle`、`TestFlushDoesNotSendOldSubscriptionToReopenedSession`、`TestPendingSnapshotStillRemovesAnAlreadyDeliveredObject`；其余 5 项见文件

### sync/entitysync/policy

- [aoi_test.go](../../../framework/sync/entitysync/policy/aoi_test.go)：`TestInterestEnterLeaveWithHysteresis`、`TestInterestBandsAndObserverMovement`、`TestInterestMaxVisibleEvictsFarthest`、`TestInterestObserverIsAlsoSubjectSymmetry`、`TestInterestDeterministicEventStream`；其余 31 项见文件
- [aoi_cluster_movement_test.go](../../../framework/sync/entitysync/policy/aoi_cluster_movement_test.go)：`TestClusterMaxVisibleReselectsOnSameBandMovement`、`TestClusterMovementCapacityMatchesDistanceOrder`、`TestClusterCapacityStaysNearestAmongCandidates`（Cluster 容量在同档移动后仍是候选内最近 N 个）
- [benchmark_test.go](../../../framework/sync/entitysync/policy/benchmark_test.go)
- [interest_test.go](../../../framework/sync/entitysync/policy/interest_test.go)：`TestObservedBlocksKeepOnePositionAndSubscription`、`TestSpatialAndBlockHandoverDoesNotRecreateBaseline`、`TestCoverageCommitRollbackRejectAndLifetime`、`TestCoverageLateObserverBudgetAndVisibilityLimit`、`TestFactAdmissionDoesNotWaitForPolicyLock`；其余 13 项见文件
- [policy_test.go](../../../framework/sync/entitysync/policy/policy_test.go)：`TestEnteringSubscribesToYourself`、`TestDistanceSubscribesReleasesAndDoesNotChurnOnTheBoundary`、`TestARelationKeepsASubscriptionDistanceDropped`、`TestLeavingReleasesBothDirections`、`TestARefusedSubscribeIsSaidAgainUntilItIsTaken`；其余 24 项见文件
- [view_test.go](../../../framework/sync/entitysync/policy/view_test.go)：`TestOnePlayersSpatialViewsDoNotReleaseEachOther`、`TestSharedRelationViewFansMemberChangesOut`、`TestCampViewsShareEveryUnitsSightWithTheWholeCamp`、`TestLayersOfSeveralViewsTakeTheRicherOrTheUnion`、`TestMaxVisibleCountsPerView`；其余 5 项见文件
- [self_view_test.go](../../../framework/sync/entitysync/policy/self_view_test.go)：`TestSelfViewSendsAFullSnapshotThenDeltas`、`TestSelfViewWaitsForTheEntityToAppear`、`TestSelfViewReconnectGetsAFullSnapshot`、`TestSelfViewReplacedConnectionKeepsTheView`、`TestSelfViewWaitsForTheConnectionToCloseAfterALoss`、`TestSelfViewEveryIsolatedLossIsRecoveredByTheReconnect`、`TestSelfViewLossRacingANewConnection`、`TestSelfViewCountsPlayersOnlineWithoutASession`、`TestSelfViewUnloadRetractionIsResubmitted`、`TestSelfViewRolledBackCreateIsBoundAgain`、`TestSelfViewCloseDuringFlush`、`TestSelfViewLoginAndDisconnectRace`、`TestNewSelfViewRefusesAnIncompleteConfig`
- [resubmit_test.go](../../../framework/sync/entitysync/policy/resubmit_test.go)：`TestInterestResubmitsASubscriptionTheFrameworkRetracted`、`TestInterestDoesNotResubmitAPairItReleasedWhileTheSubjectWasAway`、`TestEachPolicyResubmitsItsOwnRetractedSubscription`、`TestGroupAndDirectResubmitAfterRetraction`、`TestGroupResubmitsAfterASecondRetractionBeforeDelivery`；其余 5 项见文件

### sync/frame

- [benchmark_test.go](../../../framework/sync/frame/benchmark_test.go)
- [codec_test.go](../../../framework/sync/frame/codec_test.go)：`TestEncodeFrameRefusesEachMalformedDelta`、`TestFrameCodecEnforcesLimitsAndRefusesCorruptBytes`、`TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry`、`TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating`
- [receiver_test.go](../../../framework/sync/frame/receiver_test.go)：`TestReceiverGapRequiresFullAndIgnoresOldEpoch`、`TestReceiverApplicationFailureRequiresRecovery`、`TestReceiverPanicDoesNotLeaveUsableBaselineOrBusyReceiver`、`TestReceiverResetInsideCallbackCannotRestoreOldBaseline`、`TestReceiverRejectsStreamAndSchemaSwitchAndMalformedFrame`

### sync/lockstep

- [command_test.go](../../../framework/sync/lockstep/command_test.go)：`TestCommandCodecAndOwnership`、`TestTCPSenderRejectsShortWorkerBeforeResolverOrSocket`、`TestMatchCommandUsesCurrentAuthenticatedBinding`、`TestTCPSenderBothLanesUseSameRouteAndBudget`
- [tcp_test.go](../../../framework/sync/lockstep/tcp_test.go)（Match 经 TCPSender 接真实嵌入 TCP）：`TestACatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`、`TestACatchUpPageTheLinkCannotTakeTimesOutWithoutHoldingOtherSessions`、`TestALiveBroadcastOverEmbeddedTCPNeverWaitsForAStalledConnection`、`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted`、`TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive`（4 MiB 快照，含只准入对照）、`TestRobotsRecoverFromASnapshotOverEmbeddedTCP`
- [snapshot_test.go](../../../framework/sync/lockstep/snapshot_test.go)（快照恢复）：`TestMatchCommittedFrameGatesCatchupAndSnapshots`（已提交门）、`TestMatchRecoveryRouting`、`TestMatchRecoveryConfigValidation`、`TestMatchSnapshotProviderRunsOffTheTick`、`TestMatchSnapshotFailureSemantics`、`TestMatchSnapshotChunkPacingAndResend`、`TestMatchSnapshotPinsHistoryWithinBound`、`TestMatchSnapshotUnpacedLaneUsesTheFixedBudget`、`TestFrameAssemblerResumeDropsBufferedFramesPastTheSnapshot`、`FuzzDecodeSnapshotChunk`、`FuzzSnapshotReassembly`；其余 13 项见文件
- [example_snapshot_test.go](../../../framework/sync/lockstep/example_snapshot_test.go)：`Example_snapshotRecovery`（服务器参考模拟发布检查点、断线超过窗口后经快照恢复，核对输出）
- [e2e_gate_test.go](../../../framework/sync/lockstep/e2e_gate_test.go)：`TestLockstepEndToEndGate`、`TestE2EBotShutdownWaitsForInFlightOutput`
- [history_long_test.go](../../../framework/sync/lockstep/history_long_test.go)：`TestMatchLongRunCatchupAfterRollingTrim`、`TestHistoryExtremeRangeAndTrim`、`TestHistoryLongRollingWindow`、`TestHistoryGradualShrinkReleasesBackingArray`、`TestMatchDefaultRetentionBoundsLongMatch`、`TestMatchAutoRetentionOvertakesStalledCatchup`、`TestMatchRetainFullHistoryKeepsEveryFrame`、`TestMatchHistoryRetainFramesValidation`
- [lockstep_test.go](../../../framework/sync/lockstep/lockstep_test.go)：`TestSequencerOptimisticLockingAndLateFolding`、`TestSequencerDeterministicFrameBytes`、`TestRedundancySurvivesPacketLoss`、`TestBroadcastCodecRoundTripAndFailFast`、`TestHistoryCatchupPagingAndSequenceGuard`；其余 13 项见文件
- [match_test.go](../../../framework/sync/lockstep/match_test.go)：`TestMatchTickBroadcastsRedundantFrames`、`TestMatchCatchupPagesHistoryThenGoesLive`、`TestMatchCatchupRequiresReliableLane`、`TestMatchDesyncVerdictSetSemantics`、`TestMatchTickSurvivesDeadSession`；其余 16 项见文件
- [submit_input_test.go](../../../framework/sync/lockstep/submit_input_test.go)：`TestSubmitInputPromiseReplayAfterCutIsIdempotent`、`TestSubmitInputPromiseOutOfOrderFutureFramesAreKept`、`TestSubmitInputPromiseExplicitInputStillOverridesFoldedPlaceholder`、`TestSubmitInputPromiseIdentityMemoryIsBoundedBetweenTicks`、`TestSubmitInputPromiseOutOfHorizonSubmissionDoesNotEvictAnIdentity`；其余 3 项见文件

### sync/nettransport

- [channel_test.go](../../../framework/sync/nettransport/channel_test.go)：`TestAsyncTransportReliableBackpressureKeepsAdmittedOrder`、`TestAsyncTransportReliableFailureIsTerminalAndHandlerPanicIsContained`、`TestAsyncTransportPreventsSessionIDReuseWhileOldSendDrains`、`TestTransportRejectsTypedNilDependencies`、`TestAsyncTransportCascadesSessionLifecycle`
- [governance_test.go](../../../framework/sync/nettransport/governance_test.go)：`TestConcurrentSessionsShareOneHardResidentBudget`、`TestGlobalResidentBudgetIncludesInFlightAndReleasesOnCancel`、`TestReliableAgeIncludesQueueWaitAndDoesNotSendExpiredSuffix`、`TestCloseDeadlineReleasesQueuedAndBusyBudget`、`TestRegisterSessionRefusesZeroDuplicateOverLimitAndClosed`；其余 4 项见文件
- [protocol_transport_test.go](../../../framework/sync/nettransport/protocol_transport_test.go)：`TestAEADSessionProtectorRejectsReplayAndTamper`、`TestAEADSessionProtectorNeverReusesNonceAfterSequenceExhaustion`、`TestUDPTransportEncryptedLoopback`、`TestQUICTransportDatagramAndReliableLoopback`、`TestKCPTransportOOBAndReliableLoopback`

### sync/syncbus

- [example_test.go](../../../framework/sync/syncbus/example_test.go)：`Example_reconcileAfterLoss`（有限投递兜底模板：序号缺口与周期对账触发快照替换，核对输出）
- [patch_syncer_test.go](../../../framework/sync/syncbus/patch_syncer_test.go)：`TestPatchSyncerPublishesAndAppliesRemotePatch`、`TestPatchSyncerRejectsMismatchedKey`、`TestPatchSyncerSkipsEmptyPatch`、`TestPatchSyncerRefusesIncompleteConfigurationAndZeroKeys`、`TestPatchSyncerStopWaitsForInFlightApply`
- [subscription_test.go](../../../framework/sync/syncbus/subscription_test.go)：`TestSubscriptionUnsubscribeStopContract`、`TestSubscriptionRefusesDeliveriesAfterUnsubscribe`、`TestSubscriptionSelfUnsubscribeWaitsOnlyForOthers`、`TestSubscriptionSelfUnsubscribeInReentrantDelivery`、`TestSubscriptionSelfUnsubscribeWithForeignContextTimesOut`；其余 3 项见文件

### sync/syncbus/driver

- [jetstream_contract_test.go](../../../framework/sync/syncbus/driver/jetstream_contract_test.go)：`TestDurableSyncNamePromiseDistinguishesPrefixes`、`TestDurableSyncNamePromiseKeepsTheDefaultPrefixNameStable`、`TestJetStreamSyncBusPromiseSameTopicSubscribersAllReceive`、`TestJetStreamSyncBusPromiseHandlerPanicIsIsolated`、`TestJetStreamSyncBusPromiseConcurrentFirstSubscribersShareOneConsumer`；其余 5 项见文件
- [jetstream_test.go](../../../framework/sync/syncbus/driver/jetstream_test.go)：`TestJetStreamSyncBusPublishesAndAcknowledgesHandlerError`、`TestJetStreamSyncBusSkipsSelfMessages`、`TestJetStreamSyncPublishHonorsCanceledContext`、`TestJetStreamSyncUnsubscribeRemovesTrackedSubscription`、`TestDurableSyncNameAvoidsSanitizationCollision`；其余 1 项见文件
- [lifecycle_test.go](../../../framework/sync/syncbus/driver/lifecycle_test.go)：`TestJetStreamSyncBusStopContract`、`TestUnsubscribeWaitsForInFlightHandler`、`TestUnsubscribeFromOwnHandlerDoesNotDeadlock`、`TestUnsubscribeStopContract`、`TestMirrorStopAndRestartWithOldDeliveryInFlight`
- [retirement_real_promises_test.go](../../../framework/sync/syncbus/driver/retirement_real_promises_test.go)：`TestRealRR25BoundedRetirement`、`TestRealRR25UnacknowledgedConnectionRecovery`
- [retirement_window_test.go](../../../framework/sync/syncbus/driver/retirement_window_test.go)：`TestLastUnsubscribeWindowDoesNotAcknowledgeUndelivered`、`TestRecreateWaitsForPreviousConsumerClosed`、`TestStoppingWindowKeepsDeliveryLimitAndBackoff`、`TestRR25HandlerOutcomeIsNotAdmission`、`TestRR25SelfUnsubscribeAndResubscribeDoesNotWaitForItself`；其余 5 项见文件

### sync/syncbus/mirror

- [envelope_test.go](../../../framework/sync/syncbus/mirror/envelope_test.go)：`TestReplicatorPublishesAndAppliesEnvelope`、`TestReplicatorRejectsForgedInnerIdentity`、`TestReplicatorHonorsCanceledPublishContext`、`TestPublishedMessagesCarryDistinctDeliveryIDs`、`TestReplicatorRefusesEachMalformedInboundMessage`；其余 3 项见文件
- [stop_test.go](../../../framework/sync/syncbus/mirror/stop_test.go)：`TestReplicatorStopContract`、`TestReplicatorStopWithContextWaitsForAdmittedHandlers`

### syncstream

- [adapter_test.go](../../../framework/sync/syncstream/adapter_test.go)：`TestPublisherAndSubscriberRoundTrip`、`TestSubscriberRejectsEnvelopeMismatch`、`TestEnqueueReportsPublishErrors`、`TestObserverAndPayloadGuards`、`TestBufferedPublisherSignalsBackpressureAndDrains`；其余 5 项见文件
- [history_contract_test.go](../../../framework/sync/syncstream/history_contract_test.go)：`TestHistoryPromiseRecreatedStreamDoesNotReuseSequences`、`TestHistoryPromiseRecreatedStreamSurvivesJournalReplay`、`TestHistoryOpensWhenTheFirstWALRecordIsTornBeforeAnyCheckpoint`、`TestHistoryReplayRestoresLastActivity`、`TestRecoverKeepsTheProvidersSchemaVersion`；其余 7 项见文件
- [journal_test.go](../../../framework/sync/syncstream/journal_test.go)：`TestFileHistoryJournalLoadFailsClosedOnEachIntegrityDefect`、`TestFileHistoryJournalRefusesRecordsAfterCloseAndFromOtherVersions`、`TestJournalPromiseIndeterminateSyncStopsTheJournal`、`TestJournalPromiseIndeterminatePublishStopsTheJournal`、`TestJournalPromisePreSideEffectFailureIsRetryable`；其余 4 项见文件
- [syncstream_test.go](../../../framework/sync/syncstream/syncstream_test.go)：`TestHistoryAppendReplayAndAck`、`TestHistoryDetectsGapSchemaMismatchAndClientAhead`、`TestFullPacketRepairsTruncatedHistory`、`TestObserverStreamsAreIsolated`、`TestRecoverAutomaticallyAppendsFullSnapshot`；其余 10 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
