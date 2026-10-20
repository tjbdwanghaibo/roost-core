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
3. Lockstep 席位来自认证 session；历史默认按 `HistoryRetainFrames`（3600 帧）在 Tick 末尾自动裁剪，越界追帧以 `ErrCatchupUnservable` 失败并发 C9 通知，客户端终止、不自动重同步。
4. 有限投递、流保留与业务 ACK 语义不能等同 exactly-once。
5. `Manager.Drain` 不放弃任何条目；ctx 结束时返回带卡住条目摘要的 `*DrainStalledError`，`errors.Is` ctx 错误不变。

## 2A. 冻结、发送和客户端权限实现走读

[mode.go](../../../framework/sync/entitysync/mode.go)的CaptureSync先取得当前subject及授权profiles，确认对象仍有效，调用SubjectSyncState.FreezeSyncViews；替换后的旧state不能留下可发送冻结物。reserveFrozen以CAS计入总字节，返回一次性释放函数，避免重试/取消双重释放。WakeSync合并通知，丢一个唤醒不等于丢pending事实。

[flush.go](../../../framework/sync/entitysync/flush.go)负责取pending、准备subject视图（prepareSubscribersLocked整理订阅者与跳过的会话）、按会话构建帧并逐帧交付（pushSessionFrames）、传输准入后结算；capturedAbove检查CommitLSN水位，requireSnapshotsAfterRetry处理部分会话已经接纳后的恢复。必须在实际Push结果之后结算基线，不能先标clean再尝试发送。

[command.go](../../../framework/sync/lockstep/command.go)的HandleCommand须在Match串行handler中运行：先拒绝closed/非法command，再查sessionOwners；旁观者只走SpectatorCatchup，普通玩家分别走StartCatchup/ReportHash/SubmitInput。没有seat字段让客户端覆盖服务端绑定，也没有把发送成功当模拟已执行的业务ACK。

[match.go](../../../framework/sync/lockstep/match.go)的Tick顺序是：切帧、追加历史、结算上次的追帧页、实时广播、发本次追帧页，最后retainWindow裁历史（hash报告按64帧步长）。裁剪必须在追帧页发出之后：页面发出前已编码成独立字节，先裁会让“从最早帧追帧”在同一Tick被判越界。startCatchup把起点0当帧1核对，越界时登记追帧状态并立即返回`*CatchupUnservableError`；pumpCatchup发现游标早于`History.start()`时把状态标为unservable、发C9通知（作为该会话最后一次可靠发送，结算后删除状态）。客户端侧[assembler.go](../../../framework/sync/lockstep/assembler.go)的Ingest按首字节0xC9识别通知，[robot/lockstep.go](../../../robot/lockstep.go)与C# LockstepClient随即终止。

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

12 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [aoi.go](../../../framework/sync/entitysync/policy/aoi.go) | `InterestEventKind`、`InterestEvent`、`AOIConfig`、`AOI` |
| [aoi_cluster.go](../../../framework/sync/entitysync/policy/aoi_cluster.go) | `AreaID`、`AOICluster` |
| [direct.go](../../../framework/sync/entitysync/policy/direct.go) | `Direct` |
| [self_view.go](../../../framework/sync/entitysync/policy/self_view.go) | `SelfProfile`、`SelfViewConfig`、`SelfView` |
| [group.go](../../../framework/sync/entitysync/policy/group.go) | `GroupConfig`、`Group` |
| [interest.go](../../../framework/sync/entitysync/policy/interest.go) | `InterestConfig`、`Refusal`、`Interest` |
| [interest_queue.go](../../../framework/sync/entitysync/policy/interest_queue.go) | 函数/方法或内部实现；见源码 |
| [profiles.go](../../../framework/sync/entitysync/policy/profiles.go) | 函数/方法或内部实现；见源码 |
| [source.go](../../../framework/sync/entitysync/policy/source.go) | `Source`、`RelationSource` |

### sync/frame

3 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../framework/sync/frame/codec.go) | 函数/方法或内部实现；见源码 |
| [frame_limits.go](../../../framework/sync/frame/frame_limits.go) | `Limits`、`ObjectRef`、`SnapshotMeta`、`Kind`、`ObjectOperation`、`ComponentOperation`、`ComponentDelta`、`ObjectDelta`、`Frame` |
| [receiver.go](../../../framework/sync/frame/receiver.go) | `Receiver` |

### sync/lockstep

8 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembler.go](../../../framework/sync/lockstep/assembler.go) | `FrameAssembler` |
| [command.go](../../../framework/sync/lockstep/command.go) | `Operation`、`Command` |
| [desync.go](../../../framework/sync/lockstep/desync.go) | `DesyncDetector`、`DesyncVerdict` |
| [history.go](../../../framework/sync/lockstep/history.go) | `History` |
| [match.go](../../../framework/sync/lockstep/match.go) | `MatchConfig`、`Match` |
| [sequencer.go](../../../framework/sync/lockstep/sequencer.go) | `PlayerID`、`FrameID`、`Input`、`Frame`、`SequencerConfig`、`Sequencer` |
| [tcp.go](../../../framework/sync/lockstep/tcp.go) | `TCPSender` |
| [wire.go](../../../framework/sync/lockstep/wire.go) | `RedundantEncoder`、`CatchupUnservableError` |

### sync/nettransport

9 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [channel.go](../../../framework/sync/nettransport/channel.go) | `SendError`、`ErrorHandler`、`AsyncTransportConfig`、`AsyncTransport`、`AsyncTransportStats` |
| [counters.go](../../../framework/sync/nettransport/counters.go) | `AsyncTransportCounters` |
| [kcp_transport.go](../../../framework/sync/nettransport/kcp_transport.go) | `KCPTransportConfig`、`KCPTransport`、`KCPTransportStats`、`KCPDatagramHandler` |
| [quic_transport.go](../../../framework/sync/nettransport/quic_transport.go) | `QUICTransportConfig`、`QUICTransport`、`QUICTransportStats` |
| [sender.go](../../../framework/sync/nettransport/sender.go) | `DatagramSender`、`DatagramPayloadLimiter`、`DatagramBatchSender`、`ReliableSender`、`CompositeTransport` |
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

- [aoi_test.go](../../../framework/sync/entitysync/policy/aoi_test.go)：`TestInterestEnterLeaveWithHysteresis`、`TestInterestBandsAndObserverMovement`、`TestInterestMaxVisibleEvictsFarthest`、`TestInterestObserverIsAlsoSubjectSymmetry`、`TestInterestDeterministicEventStream`；其余 23 项见文件
- [aoi_cluster_movement_test.go](../../../framework/sync/entitysync/policy/aoi_cluster_movement_test.go)：`TestClusterMaxVisibleReselectsOnSameBandMovement`、`TestClusterMovementCapacityMatchesDistanceOrder`、`TestClusterCapacityStaysNearestAmongCandidates`（Cluster 容量在同档移动后仍是候选内最近 N 个）
- [benchmark_test.go](../../../framework/sync/entitysync/policy/benchmark_test.go)
- [interest_test.go](../../../framework/sync/entitysync/policy/interest_test.go)：`TestObservedBlocksKeepOnePositionAndSubscription`、`TestSpatialAndBlockHandoverDoesNotRecreateBaseline`、`TestCoverageCommitRollbackRejectAndLifetime`、`TestCoverageLateObserverBudgetAndVisibilityLimit`、`TestFactAdmissionDoesNotWaitForPolicyLock`；其余 16 项见文件
- [policy_test.go](../../../framework/sync/entitysync/policy/policy_test.go)：`TestEnteringSubscribesToYourself`、`TestDistanceSubscribesReleasesAndDoesNotChurnOnTheBoundary`、`TestARelationKeepsASubscriptionDistanceDropped`、`TestLeavingReleasesBothDirections`、`TestARefusedSubscribeIsSaidAgainUntilItIsTaken`；其余 13 项见文件
- [relation_index_test.go](../../../framework/sync/entitysync/policy/relation_index_test.go)：`TestRelationDeltaMatchesFullReplacement`、`TestRelationForgetAfterReplacementClearAndReadd`、`TestRelationSetMatchesMembershipModel`、`TestRelationSetWorkspaceConcurrentLifecycle`
- [self_view_test.go](../../../framework/sync/entitysync/policy/self_view_test.go)：`TestSelfViewSendsAFullSnapshotThenDeltas`、`TestSelfViewWaitsForTheEntityToAppear`、`TestSelfViewReconnectGetsAFullSnapshot`、`TestSelfViewReplacedConnectionKeepsTheView`、`TestSelfViewWaitsForTheConnectionToCloseAfterALoss`、`TestSelfViewEveryIsolatedLossIsRecoveredByTheReconnect`、`TestSelfViewLossRacingANewConnection`、`TestSelfViewCountsPlayersOnlineWithoutASession`、`TestSelfViewUnloadRetractionIsResubmitted`、`TestSelfViewRolledBackCreateIsBoundAgain`、`TestSelfViewCloseDuringFlush`、`TestSelfViewLoginAndDisconnectRace`、`TestNewSelfViewRefusesAnIncompleteConfig`
- [resubmit_test.go](../../../framework/sync/entitysync/policy/resubmit_test.go)：`TestInterestResubmitsASubscriptionTheFrameworkRetracted`、`TestInterestDoesNotResubmitAPairItReleasedWhileTheSubjectWasAway`、`TestEachPolicyResubmitsItsOwnRetractedSubscription`、`TestGroupAndDirectResubmitAfterRetraction`、`TestGroupResubmitsAfterASecondRetractionBeforeDelivery`；其余 5 项见文件

### sync/frame

- [benchmark_test.go](../../../framework/sync/frame/benchmark_test.go)
- [codec_test.go](../../../framework/sync/frame/codec_test.go)：`TestEncodeFrameRefusesEachMalformedDelta`、`TestFrameCodecEnforcesLimitsAndRefusesCorruptBytes`、`TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry`、`TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating`
- [receiver_test.go](../../../framework/sync/frame/receiver_test.go)：`TestReceiverGapRequiresFullAndIgnoresOldEpoch`、`TestReceiverApplicationFailureRequiresRecovery`、`TestReceiverPanicDoesNotLeaveUsableBaselineOrBusyReceiver`、`TestReceiverResetInsideCallbackCannotRestoreOldBaseline`、`TestReceiverRejectsStreamAndSchemaSwitchAndMalformedFrame`

### sync/lockstep

- [command_test.go](../../../framework/sync/lockstep/command_test.go)：`TestCommandCodecAndOwnership`、`TestTCPSenderRejectsShortWorkerBeforeResolverOrSocket`、`TestMatchCommandUsesCurrentAuthenticatedBinding`、`TestTCPSenderBothLanesUseSameRouteAndBudget`
- [tcp_test.go](../../../framework/sync/lockstep/tcp_test.go)（Match 经 TCPSender 接真实嵌入 TCP）：`TestACatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`、`TestACatchUpPageTheLinkCannotTakeTimesOutWithoutHoldingOtherSessions`、`TestALiveBroadcastOverEmbeddedTCPNeverWaitsForAStalledConnection`
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
