# Gate 与 Game 接入

v1.25.0已实现嵌入 Game 的 TCP 与独立 Gate 两种正式接入。两者共用 [TCPServer](../../infra/network/gateway/tcp.go)，业务都进入生成的 ProtocolRegistry → Nest Sender → Nest → Entity/DataEngine。新增独立接入随v1.25.0交付；本机性能和跨机验收边界见[功能验收](../release/v1.25.0-VALIDATION.md)与[性能报告](../performance/GATE-AOI.md)。

## 1. 职责与目录

| 位置 | 责任 |
| --- | --- |
| `infra/network/gateway` | TCP、鉴权边界、绑定、续期、有界转发/出站、广播、排空；不依赖 Nest/Entity/Service/Wiring |
| `wiring/gate` | App singleton、Account、发现、正式 dispatcher、Sync/Lockstep 的便捷接线 |
| `codegen/internal/roost` | 生成协议和配置、嵌入 TCP Mod、Game `NewIngressMod`；不生成另一份 TCP 运行循环 |
| `service/session` | 副本/试炼等运行记录，与 socket、Gate Binding、Sync SessionID 无关 |

独立部署的链路是客户端 RS v2 → Gate TCP → Core NATS → Game Ingress → 正式业务 handler。内部信封和默认 Bus/RPC 使用 MessagePack；PB、Sync、Lockstep 的客户端格式保持原样。Gate 不处理 Entity 业务，也不获取 Entity local 锁。

## 2. 正式接入方式

嵌入部署继续声明生成的 player TCP Mod。独立部署中，Gate 声明 `wiring/gate.NewGateMod(options)`；Game 声明生成的 `player/tcp.NewIngressMod(options)`，关闭该 Game 的本地 player TCP listener。两个 Mod 使用同一 capability 名，生成接线拒绝重复提供，不能在同一个 Game 同时装配两种入口。

`DefaultOptions()` 提供资源默认值。Gate 要明确设置 `TCP.Enabled=true`、监听地址及业务 `Ready`；Game 提供业务 `Ready`。Ready 只快速读取已维护的准入状态，不能每包执行 I/O。两端必须启用 App singleton，并依赖同一应用命名空间下的 NATS。

未提供 Authenticator 时，接线使用正式 Accounts.ValidateSession 校验签名 session 票据、PlayerID 与固定 Game SID。自定义 Authenticator 也必须返回可信固定路由；不是依据客户端自报 SID 随机选 Game。绑定阶段 Gate/Game 各自验证，后续数据包不重复查 Account。

使用发现时，两端设置 `RegisterDiscovery=true`，并装配 `etcd.NewEtcdMod(etcd.WithoutServiceRegistration())`。接入监听/订阅启动及 NATS Flush 完成后才登记候选；一个 Discovery 只有一个登记拥有者。Game 候选必须带 `access.incarnation` 与 `access.ingress_ready=true`。Gate 默认用 FixedGameResolver 只查固定 SID；也可以显式注入 ResolveGame。发现只是候选，真正的身份由 SingletonIdentityChecker 核对。

NATS ACL 必须限制每个进程的来源前缀和自己的 reply inbox。应用 payload 自报 GateID 不构成认证。正式权限配置和已测边界见[具体契约](GATEWAY-IMPLEMENTATION.md)；测试用私有 broker 和临时测试用户，不要求修改共享业务 broker。

## 3. 绑定与请求结果

完整绑定含 PlayerID、SessionID、Gate/Game SID 与 incarnation、连接 nonce、Game 签发 BindID。先 Bind，再物理写出 AuthACK，最后 Activate。OnActive/OnClosed 用 Game 分配的不复用数值 receiver 建立和关闭 Sync/Room 生命周期；旧关闭、旧出站失败不会解绑新连接。不同 SessionID 可属于同一玩家，没有框架隐式全局单点登录。

Gate Forward 只有一次传输尝试。预算包含 Gate 等待、NATS、Game 等待与正式 dispatcher；控制请求也有条目/字节上限和固定 worker，排队消耗原预算。

- `NotAdmitted`：确认未交给 dispatcher。
- `Completed`：内部执行及本地出站已准入；不是落库确认或客户端业务处理确认。
- `Unknown`：已交给 dispatcher 后的失败/超时，或者丢失回执；不能自动重放扣款/发奖等写请求。

## 4. Sync、Lockstep 与广播

`wiring/gate.Transport` 把 Entity Sync 和 Lockstep 接到 Game 同一个有序出站口。数值 receiver 发送时冻结完整 binding。Sync 保持原 profile/AOI/epoch/版本与 handler 完成后的 on_change + 20Hz 兜底；setter 只标脏。Push 成功只表示本地准入，异步下游失败关闭对应完整旧 binding 和 Sync lifetime。

Room 的 Attach/Detach/Input/Hash/Tick/Catchup 仍走业务的单一 Room owner。迟到断线使用 `Room.DetachSession(player, receiver)` 条件解绑；不能在 NATS callback 直接修改 Room 或 Entity。首期两种 Lockstep 发送接口最终都经 Gate TCP，不提供 UDP/KCP 承诺。

`gateway.BroadcastSender` 支持指定玩家、指定 Game 在线连接，以及由 system 身份发起 AllOnline。Game 只能广播自己拥有的连接；通用广播仅为 PB push，不把不同客户端的 Sync delta 当通用广播。每 Gate 返回准入统计与 completed/not_admitted/unknown；同 BroadcastID 在有限窗口内返回首次结果，不重复投递，换内容复用 ID 会被拒绝。统计按连接，包含同玩家多会话，不构成精确全局在线玩家数。

PB、Sync、Lockstep 共用每绑定 OutSeq 和有界出站，Gate 检查连续序号并复用唯一 socket writer。按完整协议包发送，不按字节截断 Sync 帧。重复只确认已准入前缀；缺口或持续发送失败关闭连接，重连后恢复全量/追帧。

### 慢消费者：跟不上就断开

三种接入对“这条连接收不下”的处理统一为断开连接，客户端重连后恢复全量 / 追帧（维护者 2026-10-17 批准）：

| 接入 | 收不下时 |
| --- | --- |
| 嵌入 TCP | 出站队列满或 `write_timeout` 内写不出：接入层当场关闭该连接（下一节）；全进程驻留预算 `player_access.tcp.outbound.resident_bytes` 耗尽不关连接，见下方例外 |
| 独立 Gate 的 Gate 侧 | 任何入队失败（每连接出站队列满，也包括 Gate 自己的 `outbound.resident_bytes` 耗尽）、帧超过 `outbound.max_age`、序号不连续、写 socket 时一帧写到一半写期限到期：关闭该连接。Lockstep 快照在慢链路上被断开实测就是最后一种（[guide/04-sync §6](guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)） |
| Game Ingress（Game 侧可靠队列） | 异步发送失败、帧超过 `outbound.max_age`：退役绑定并通知 Gate 关闭连接。入队背压（这条绑定的队列满）只把错误交还推送方：**Entity Sync 推送**由推送方关闭该玩家的全部连接（`wiring/selfsync.PushPlayerFrame`，自身视图与 game-demo 场景共用）；PB 推送仍由业务按返回的错误处理 |

**例外：Game 进程的驻留预算耗尽**（Game Ingress 的 `outbound.resident_bytes`，对应 `nettransport.ErrResidentBudgetExhausted`，它同时满足 `ErrReliableBackpressure`）不是哪个连接慢：Entity Sync 推送交还 `entitysync.ErrRetryLater`，Manager 整轮重试、保留脏状态，不断开任何人。预算不会被停滞的绑定长期占住——每帧从入队起最多驻留 `outbound.max_age`，到期发不出去的绑定被关闭、释放预算；持续耗尽只在健康流量持续超过发送能力时出现，代价是同步延迟上升（细节与放大见 [impl/04-sync §2C](impl/04-sync.md#2c-自身视图selfview的生命周期)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)）。只有同一玩家的部分连接因预算拒收时仍按“收不下”断开：收下这一帧的连接会把重试的同一 tick 当旧包丢掉。Gate 侧自己的驻留预算耗尽目前仍关闭连接，没有这个例外（KNOWN-LIMITS）。嵌入 TCP 的全进程驻留预算（`player_access.tcp.outbound.resident_bytes`，`gateway.ErrResidentBudgetExhausted`，2026-10-20 起）用同一个例外：`PushPlayerFrame` 把两个哨兵都当整轮重试，区别是停读连接占着的预算要到每连接上限或 `write_timeout` 才归还，没有 `max_age`（下一节）。

Game Ingress 的绑定在激活回调（`OnActive`）执行期间已可按玩家推送（`PushPlayer` / `PushSyncPlayer` / `ActiveSessions` / `CloseSessions` 都算上它）：Gate 先物理写出 AuthACK、准备好接收出站才发 Activate，激活回调里登记的同步若在回调返回前推送，不会得到“玩家没有连接”。激活失败的绑定随即关闭、不再算在内。

### 嵌入 TCP 的出站

嵌入 Game 的 TCP 没有 Gate 那一跳，出站同样有界、异步：每个连接一条出站队列和唯一写协程（[tcp_session.go](../../infra/network/gateway/tcp_session.go) 的 `enqueue` / `writeLoop`），PB 推送、Sync、Lockstep、读循环的回包与心跳 ACK 按入队顺序写出。`TCPRuntime.Push*` 只入队：返回 nil 表示该连接已准入，不表示已写出；准入只取一把不跨 I/O 的队列锁、不等 socket，所以 `Push*` 不需要 `fctx.BlockingError` 守卫，业务 worker、场景 Sync Flush、Lockstep Tick 都可以直接调用。payload 入队时复制，调用返回后可复用。

唯一例外是 `TCPRuntime.PushLockstepSessionWritten`（2026-10-16，`tcpSession.pushWritten`）：同一条队列，入队后等这一帧被写协程写出，只给 Lockstep 可靠通道用（`lockstep.TCPSender` 的 `SendReliable`：追帧页、快照分块与恢复失败通知），让追帧与快照随链路速度放慢。返回 nil 表示已写进 socket；写出失败或连接关闭立即返回 `errConnectionBroken`；ctx 结束时这一帧若还没被写协程取走就撤回（不写出、归还额度、推送序号不留空洞），已取走的照常写完，调用方都得到 ctx 的错误，连接不因此关闭。它不走读循环回包的直写快路径：调用方截止早于 `write_timeout` 时直写写出半帧会关闭连接。实时帧（Tick 的当前帧广播、场景 Sync）不得用它，见 [guide/04-sync §6](guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)。

慢连接策略与独立 Gate 一致：一帧入队会让该连接的“排队 + 在途”超过 1024 帧或 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节时，拒绝这一帧并当场关闭该连接（`player_tcp_outbound_overflow_total`），推送方得到 `errConnectionBroken` 并注销它；一帧在 `write_timeout` 内写不出、写出半帧或 socket 失败，由写协程关闭连接（`player_tcp_write_error_total`）。“在途”是写协程已从队列整批取走、还没写完的帧：帧数与字节都在入队时计入、写出一帧后才扣减，所以两个上限就是一条连接最多驻留的量（停读连接最多收下 1024 帧，第 1025 帧被拒）；读循环直写快路径上的那一帧不经队列、不计入。队列里剩下的帧随连接丢弃，客户端重连后恢复全量/追帧。因此逐会话串行推送的调用方不会被一个停读客户端每轮拖住 `write_timeout`（修复前默认每次 Flush / Tick 卡 5s）。每连接上限不开放配置键。帧数上限 1024 是维护者 2026-10-16 的决定：场景 Sync / Lockstep 每次每连接只推几帧，上千帧突发不现实；需要更大突发时再评估。

**全进程驻留预算**（2026-10-20，维护者批准）。每连接上限只防一个停读客户端；很多客户端同时停读（恶意连接或网络抖动）时，每条都没到自己的上限，进程却已驻留“连接数 × 每连接上限”（缺省最坏约 20 GiB），要等 `write_timeout` 才逐个关掉。`player_access.tcp.outbound.resident_bytes`（缺省 256 MiB，`TCPConfig.OutboundResidentBytes`）是一个 `TCPServer` 全部连接共用的上限，口径与独立 Gate 的 `outbound.resident_bytes` 相同：排队中 + 写协程已取走还没写完的字节（每帧按 payload + 16 字节头计）。

- 计账（[tcp_session.go](../../infra/network/gateway/tcp_session.go) 的 `outboundBudget`）：`enqueue` 在队列锁内先查每连接上限，再对全进程预算做原子比较交换预留；放不下就当场拒绝，**不等待**，所以场景 Sync Flush、Lockstep Tick 与业务 worker 调 `Push*` 仍不阻塞。每连接的 `queuedBytes` 与全进程计数在队列锁内同增同减。
- 拒绝：返回 `gateway.ErrResidentBudgetExhausted`（不带 `errConnectionBroken`），计 `player_tcp_outbound_budget_exhausted_total`，**不关闭连接**——不是这条连接慢。`PushPlayer*` 里它和 ctx 结束、payload 超限一样算“入队前拒绝”，其他连接收下也返回错误。上层：Entity Sync（`wiring/selfsync.PushPlayerFrame`，自身视图与 game-demo 场景）整轮重试，与 Game Ingress 的 `nettransport.ErrResidentBudgetExhausted` 同一规则（infra 不能依赖 framework，所以是两个哨兵）；Lockstep 实时帧对这个接收者丢失，按数据报丢失处理（下一个广播包的冗余帧或客户端追帧补上），追帧页 `PushLockstepSessionWritten` 立即返回错误、计一次追帧失败；PB 推送与业务异步 `Session.Reply` 把错误交给业务。
- 读循环自己的回包与心跳 ACK（`writeQueued`）计入驻留但不被拒：读循环等它写出才读下一个请求，每连接至多一帧；拒绝它只能结束读循环、断开一个正常读的客户端。驻留因此可能超出预算至多 `max_connections × (max_payload_bytes + 16)`。读循环直写快路径上的那一帧不经队列、不计入。
- 归还：写协程写出一帧、`pushWritten` 撤回还在排队的一帧时各归还这一帧；写协程退出（`Close`、写失败、服务器停止）时一次归还这条连接剩下的全部（队列里的帧与写失败那一批没写的帧），并置 `released`，之后的入队被拒——这是唯一的收尾点，每个字节恰好归还一次。停读连接占着的预算要到它撞上每连接上限被关，或 `write_timeout` 写超时，才随写协程退出归还；期间健康连接的推送可能被拒（Sync 延迟上升）。
- 缺省 256 MiB 的理由：独立 Gate 缺省 128 MiB 对应 4096 个连接，嵌入接入缺省 `max_connections` 是 10000，按比例约 312 MiB，取整到 256 MiB；正常负载下写协程很快把帧交给 socket，全进程驻留接近 0，10000 个连接各有约 26 KiB 同时在途才用满。超出时 Sync 整轮重试、不断开，代价是延迟。取值范围 `[max_payload_bytes + 16, 1 TiB]`，非法值在加载时按键名拒绝（生成 Mod 的 `configFromViper` 与 `NewTCPServer` 共用 `ValidateTCPConfig`）；代码构造的 `TCPConfig` 填 0 取缺省值。没有重档压测，见 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)。
- 回归：`TestStalledConnectionsTogetherStayWithinTheResidentBudget`（修前红：8 条停读连接合计驻留 8396800 字节、超过 1 MiB 预算、0 次拒绝）、`TestTheResidentBudgetIsReturnedExactlyOnceWhenClosesRaceTheWriteLoop`、`TestAPushAfterTheWriterExitedIsRefusedAndNotCounted`、`TestReadLoopRepliesAreCountedButNotRefusedByTheResidentBudget`、`TestOutboundResidentBytesIsValidatedByItsKey`（[tcp_test.go](../../infra/network/gateway/tcp_test.go)）；集成：`TestSelfSyncOverTCPRetriesTheTickWhenTheResidentBudgetIsExhausted`（[selfsync_test.go](../../wiring/selfsync/selfsync_test.go)）、`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted`（[lockstep tcp_test.go](../../framework/sync/lockstep/tcp_test.go)）。

顺序契约不变：服务推送的 RS 序号由写协程在写出时分配，等于实际 socket 写顺序；读循环的回包入队后等它写出才读下一个请求，handler 里先发的推送先于它的回包（队列空闲且没人在写时回包直写，省掉一次协程切换）；不同生产者之间以入队先后为序。业务可见的 `Session.Reply` 与推送一样只入队。独立 Gate 不走这条队列：它的 AsyncTransport 已按绑定排队，`WritePacket` 同步直写并把结果交回那条队列。回归：`TestAStalledConnectionDoesNotHoldPushesToOtherConnections`、`TestOutboundQueueKeepsPushReplyOrderAndPushSequences`、`TestOutboundFrameLimitCountsTheBatchBeingWritten`、`TestAWriteTimeoutInTheWriteLoopFailsTheReplyQueuedBehindIt`、`TestClosingASessionDuringConcurrentPushesLeavesNoGoroutine`、`TestAPushWaitingForItsWriteIsWithdrawnWhenItsContextEnds`、`TestAPushWaitingForItsWriteFailsAtOnceWhenTheConnectionCloses`（[tcp_test.go](../../infra/network/gateway/tcp_test.go)）。

协程与资源：每个已认证连接都有一个写协程，嵌入模式每连接 goroutine 由 1（读循环）变为 2（读循环 + 写协程）。独立 Gate 模式下 TCP 服务端同样为每连接启动写协程，但 Gate 的推送走 `WritePacket` 直写、不入这条队列，写协程通常空闲；Gate 模式的心跳 ACK 与嵌入模式一样走 `writeQueued`（队列空闲时直写，否则排在队列后面等写出）。连接返回时先关闭会话、等写协程退出，再释放连接槽并发布关闭事件，`Stop` 的排空因此也覆盖写协程。

指标口径：`player_tcp_outbound_budget_exhausted_total` 计因全进程驻留预算被拒的入队次数（不关连接，同一帧推给玩家的每条连接各计一次）。`player_tcp_outbound_overflow_total` 计被拒的入队次数，不是被关闭的连接数——队满那一帧关闭连接后，后续推送看到连接已关闭不再计这一项，但并发推送方可能在关闭前同时撞到上限，各计一次。`player_tcp_push_closed_total` 计推送路径（`PushPlayer` / `PushSession` 等）因 `errConnectionBroken` 注销并关闭连接的次数：溢出时计一次；写协程因写超时 / socket 失败自己关闭的连接，在读循环注销它之前若还有推送到达，这些推送同样会各计一次，没有推送到达则不计。`player_tcp_write_error_total` 计一次写 socket 失败（写协程、读循环直写、Gate 的 `WritePacket`）；关闭连接时正在进行的那次写会被打断并计入，所以一个停读连接因溢出被关闭时，三项通常各至少加一；连接已关闭后被拒绝的写不计入。

## 5. 停机、指标与验证

停止先关闭准入，再等已接纳 dispatcher、回调与出站真实排空，随后注销发现和关闭 TCP。超时保留实例及依赖，新 context 可继续等待；不把取消等待当事务已经停止。

`gate.channel.result_total` 与 `gate.channel.failure_total` 使用有限 role/phase/result 标签；Stats 返回当前绑定和驻留预算。不要把 PlayerID、SessionID、incarnation 或票据加入指标标签。

已验证正式生成 PB/Nest/双实体 DAO/文件 WAL/Sync、实际 NATS 双 Gate 双 Game、Lockstep 输入/追帧/重连、旧事件隔离、ACK 丢失、广播去重、控制容量与预算、真实 ACL、Stop 重试。测试使用本机隔离资源，鉴权权威、存储和进程范围按[功能验收记录](../release/v1.25.0-VALIDATION.md)逐项说明；不声称已完成独立多进程、真实 Mongo、Linux 实机或跨机完整矩阵。

完整配置、协议、阶段验收和未验收项见[设计与实施契约](GATEWAY-IMPLEMENTATION.md)。目录升级没有旧 import alias，旧工程必须重新生成并编译；稳定基线历史性能不能替代新增 Gate 的结果。
