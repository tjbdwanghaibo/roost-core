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

未提供 Authenticator 时，接线使用正式 Accounts.ValidateSession 校验签名 session 票据、PlayerID 与固定 Game SID；握手票据是 `enter:<进服票据>`（登录入口签发）时交给 Accounts.EnterServer，新号第一次握手由 account 建出账号与玩家号的映射（2026-11-12）。自定义 Authenticator 也必须返回可信固定路由；不是依据客户端自报 SID 随机选 Game。绑定阶段 Gate/Game 各自验证，后续数据包不重复查 Account。

使用发现时，两端设置 `RegisterDiscovery=true`，并装配 `etcd.NewEtcdMod(etcd.WithoutServiceRegistration())`。接入监听/订阅启动及 NATS Flush 完成后才登记候选；一个 Discovery 只有一个登记拥有者。Game 候选必须带 `access.incarnation` 与 `access.ingress_ready=true`。Gate 默认用 FixedGameResolver 只查固定 SID；也可以显式注入 ResolveGame。发现只是候选，真正的身份由 SingletonIdentityChecker 核对。

信任边界是内网加 Gate 的客户端鉴权（维护者 2026-11-05）：NATS 只供内部服务使用、只在内网可达，开连接级账号（`nats.username` / `password`）；Gate 有状态，只有鉴权并绑定成功的连接才能把数据经 NATS 送进 Game。按 subject 限制每个进程来源前缀与 reply inbox 的 NATS ACL 是可选加固，不是部署前提。不开 ACL 时，应用 payload 自报的 GateID 与 subject 只防误投、不防内网进程冒充——内网进程被攻破不在这个模型的防护范围内。可选 ACL 的权限配置和已测边界见[具体契约](GATEWAY-IMPLEMENTATION.md)；测试用私有 broker 和临时测试用户，不要求修改共享业务 broker。

## 3. 绑定与请求结果

完整绑定含 PlayerID、SessionID、Gate/Game SID 与 incarnation、连接 nonce、Game 签发 BindID。先 Bind，再物理写出 AuthACK，最后 Activate。OnActive/OnClosed 用 Game 分配的不复用数值 receiver 建立和关闭 Sync/Room 生命周期；`OnActivated`（可选，2026-11-12）在 OnActive 成功之后调用一次，多带 Game 在 Bind 时自己认证得到的 Principal（含 Claims）的副本——它只是通知，没有返回值、不能拒绝绑定，自身 panic 被恢复并计数；生成的 Game Ingress 用它把会话激活告诉认证器（`SessionObserver`）。旧关闭、旧出站失败不会解绑新连接。不同 SessionID 可属于同一玩家，没有框架隐式全局单点登录。

同一 SessionID 的新连接替换旧连接，新旧按连接被接受的先后定，不按握手完成的先后（2026-11-04 修复；修前后完成者胜出，客户端放弃握手立即重连时旧连接会把新连接踢掉）。每条连接被接受时取得本进程内递增的接受序号（`ConnectionSession.Admission`）；客户端还可以在认证帧里带一个**连接代号**（`ConnectionSession.Generation`，2026-11-05），用于接受序号不可比较的跨 Gate 情形：

- **嵌入 TCP**（以及 Gate 本地的连接表）：`TCPServer.registerSession` 在同一把锁里比较接受序号，已登记的是更晚接受的连接时，迟到完成的旧登录在 ack 之后被拒登记并断开，不关闭新连接（回归 `TestALoginThatFinishesLateDoesNotDisplaceANewerConnectionOfTheSameSession`）。同一进程内接受先后是精确的，不看连接代号。
- **独立 Gate、同一 Gate 实例**：Gate 把序号随 Bind 交给 Game（`ControlRequest.Admission`）。Game 绑定表 `bindingTable.finish` 对**同一 Gate 实例**（`Binding.Gate` 含进程代际）的同一 SessionID 按序号判新旧：已绑定的是更晚接受的连接时，迟到完成的旧 Bind 被拒（原因 `superseded by a newer connection of the session`）、不退役任何绑定，Gate 收到拒绝后不发 ack、关掉那条旧连接；旧连接先完成时，新连接的 Bind 照常替换它。判定与写入同一把锁，两次 Bind 无论怎样交错，留下的都是接受得更晚的那条（回归 `TestALateBindOfAnOlderConnectionDoesNotRetireTheNewerBindingOfTheSameSession`、`TestTheNewerBindOfASessionReplacesTheOlderThatFinishedFirst`）。连接代号不参与。
- **独立 Gate、跨 Gate 实例**（含同一 SID 的 Gate 重启后的新代际）：各 Gate 的序号各自编号、不可比较，Game 改比 Bind 带来的客户端连接代号（`ControlRequest.Generation`，内部协议版本 3）。以下三条同时成立时，迟到完成的 Bind 按上一条同样被拒、同样收尾：两边都带了代号；已绑定的那条代号更大；这次 Bind **到达 Game 早于**已绑定的那条被确认（两次握手在 Game 上重叠）。任一条不成立就是后完成者胜出，被替换的那条由它所在的 Gate 关闭（回归 `TestAcrossGatesALateBindWithAnOlderConnectionGenerationDoesNotRetireTheNewerBinding`、真实 NATS 的 `TestRealAcrossGatesALateOlderBindDoesNotDisplaceTheNewerConnection`、真实 C# 客户端放弃握手转连的 `TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection`；缺省代号 `TestAcrossGatesWithoutGenerationsTheLaterFinishedBindOfASessionWins`；绑定表各分支 `TestBindingTableOrdersASessionAcrossGatesByConnectionGeneration`）。

**连接代号**由客户端生成、服务端只比大小：

- **放在认证帧里**，不放登录票据：票据由登录服签发，同一会话重连通常复用同一张，带不出“哪次连接更晚”；代号是每次连接的客户端事实。认证帧载荷写成信封 `0x00 'G' <8 字节大端代号> <票据原文>`（`wire.EncodeAuth`），不带代号时载荷就是票据原文（行为与修前相同）。Go robot / loadtest（2026-11-05）、C# 与 C++ 运行库（2026-11-06，含 Unity 的 native 与托管两条路线、经 C ABI 的 Unreal）都每次连接自动带代号。`TCPServer` 在鉴权前剥掉信封（`wire.DecodeAuth`），业务 `Authenticator` 与 Game 的二次鉴权只看到票据原文；以 0x00 开头却不是完整信封的首包按坏帧拒绝。字节布局与非 Go 客户端的生成方法见[客户端说明](../../client/README.md#鉴权首包的连接代号)。
- **来源要在客户端重启后仍单调**：三套运行库都用 `max(上一个 + 1, 当前 Unix 纳秒)`：Go 的 `robot/transport.AuthPacket` 每次调用取 `NextConnectionGeneration`，C# 的 `TcpSession.ConnectAsync` 取 `AuthPayload.NextConnectionGeneration`，C++ 由 `roost_connect` 取。进程内严格递增、与时钟无关；重启后不需要持久状态也更大，因为重启至少耗时毫秒级、新进程从当时的时钟起算。纯内存计数器不行：重启后从 1 开始，新连接会输给旧连接。
- **时钟回拨的后果有界**：客户端重启前后时钟被往回拨时，新进程的代号可能小于旧进程。它只在与旧进程仍在途的握手重叠时被当成旧的拒掉一次（新连接被关，客户端重连一次即恢复）；已确认的旧绑定挡不住之后到达的 Bind。不会两条都断、也不会永久拒绝。
- **代号锁不死会话**：“到达早于确认”这一条让代号只裁决在 Game 上重叠的握手。一条带超大代号的绑定（客户端缺陷、或持票据者故意）确认之后，任何之后到达的 Bind 无论代号多小都照常替换它，所以正常的新连接替换旧连接不受影响；被它拒掉的只有确认时已经在途的那几次 Bind。比较只在同一 SessionID 内进行，碰不到别的会话。
- **覆盖面**：旧连接的 Bind 在新绑定确认之后才到达 Game（Gate 在发 Bind 之前就慢了：Gate 本地鉴权、`ResolveGame`、身份核对，或 NATS 积压），识别不出，结果与没有代号相同，见 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#同一-sessionid-的重连)。

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

- 演练入口（2026-11-14）：`ROOST_TCP_BUDGET_DRILL=1 go test -run TestOutboundBudgetDrill -v ./infra/network/gateway/`，真实回环 TCP 上健康客户端一直读、停读客户端握手后不读，按 `DRILL_*` 环境变量（健康 / 停读连接数、帧大小、每秒轮数、时长、预算、`write_timeout`）推送，报驻留峰值（包内直接读预算，生产没有驻留 Gauge）、预算拒绝、溢出关闭、写期限关闭与健康连接收到的帧。预算被停读连接占满时，健康连接的推送同样被拒，直到停读连接被 `write_timeout` 或每连接上限关掉——预算只保内存上界，不区分谁占着。
- 计账（[tcp_session.go](../../infra/network/gateway/tcp_session.go) 的 `outboundBudget`）：`enqueue` 在队列锁内先查每连接上限，再对全进程预算做原子比较交换预留；放不下就当场拒绝，**不等待**，所以场景 Sync Flush、Lockstep Tick 与业务 worker 调 `Push*` 仍不阻塞。每连接的 `queuedBytes` 与全进程计数在队列锁内同增同减。
- 拒绝：返回 `gateway.ErrResidentBudgetExhausted`（不带 `errConnectionBroken`），计 `player_tcp_outbound_budget_exhausted_total`，**不关闭连接**——不是这条连接慢。`PushPlayer*` 里它和 ctx 结束、payload 超限一样算“入队前拒绝”，其他连接收下也返回错误。上层：Entity Sync（`wiring/selfsync.PushPlayerFrame`，自身视图与 game-demo 场景）整轮重试，与 Game Ingress 的 `nettransport.ErrResidentBudgetExhausted` 同一规则（infra 不能依赖 framework，所以是两个哨兵）；Lockstep 实时帧对这个接收者丢失，按数据报丢失处理（下一个广播包的冗余帧或客户端追帧补上），追帧页 `PushLockstepSessionWritten` 立即返回错误、计一次追帧失败；PB 推送与业务异步 `Session.Reply` 把错误交给业务。
- 读循环自己的回包与心跳 ACK（`writeQueued`）计入驻留但不被拒：读循环等它写出才读下一个请求，每连接至多一帧；拒绝它只能结束读循环、断开一个正常读的客户端。驻留因此可能超出预算至多 `max_connections × (max_payload_bytes + 16)`。读循环直写快路径上的那一帧不经队列、不计入。
- 归还：写协程写出一帧、`pushWritten` 撤回还在排队的一帧时各归还这一帧；写协程退出（`Close`、写失败、服务器停止）时一次归还这条连接剩下的全部（队列里的帧与写失败那一批没写的帧），并置 `released`，之后的入队被拒——这是唯一的收尾点，每个字节恰好归还一次。停读连接占着的预算要到它撞上每连接上限被关，或 `write_timeout` 写超时，才随写协程退出归还；期间健康连接的推送可能被拒（Sync 延迟上升）。
- 缺省 256 MiB 的理由：独立 Gate 缺省 128 MiB 对应 4096 个连接，嵌入接入缺省 `max_connections` 是 10000，按比例约 312 MiB，取整到 256 MiB；正常负载下写协程很快把帧交给 socket，全进程驻留接近 0，10000 个连接各有约 26 KiB 同时在途才用满。超出时 Sync 整轮重试、不断开，代价是延迟。取值范围 `[max_payload_bytes + 16, 1 TiB]`，非法值在加载时按键名拒绝（生成 Mod 的 `configFromViper` 与 `NewTCPServer` 共用 `ValidateTCPConfig`）；代码构造的 `TCPConfig` 填 0 取缺省值。没有重档压测，见 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)。
- 回归：`TestStalledConnectionsTogetherStayWithinTheResidentBudget`（修前红：8 条停读连接合计驻留 8396800 字节、超过 1 MiB 预算、0 次拒绝）、`TestTheResidentBudgetIsReturnedExactlyOnceWhenClosesRaceTheWriteLoop`、`TestAPushAfterTheWriterExitedIsRefusedAndNotCounted`、`TestReadLoopRepliesAreCountedButNotRefusedByTheResidentBudget`、`TestOutboundResidentBytesIsValidatedByItsKey`（[tcp_test.go](../../infra/network/gateway/tcp_test.go)）；集成：`TestSelfSyncOverTCPRetriesTheTickWhenTheResidentBudgetIsExhausted`（[selfsync_test.go](../../wiring/selfsync/selfsync_test.go)）、`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted`（[lockstep tcp_test.go](../../framework/sync/lockstep/tcp_test.go)）。

顺序契约不变：服务推送的 RS 序号由写协程在写出时分配，等于实际 socket 写顺序；读循环的回包入队后等它写出才读下一个请求，handler 里先发的推送先于它的回包（队列空闲且没人在写时回包直写，省掉一次协程切换）；不同生产者之间以入队先后为序。业务可见的 `Session.Reply` 与推送一样只入队。独立 Gate 不走这条队列：它的 AsyncTransport 已按绑定排队，`WritePacket` 同步直写并把结果交回那条队列。回归：`TestAStalledConnectionDoesNotHoldPushesToOtherConnections`、`TestOutboundQueueKeepsPushReplyOrderAndPushSequences`、`TestOutboundFrameLimitCountsTheBatchBeingWritten`、`TestAWriteTimeoutInTheWriteLoopFailsTheReplyQueuedBehindIt`、`TestClosingASessionDuringConcurrentPushesLeavesNoGoroutine`、`TestAPushWaitingForItsWriteIsWithdrawnWhenItsContextEnds`、`TestAPushWaitingForItsWriteFailsAtOnceWhenTheConnectionCloses`（[tcp_test.go](../../infra/network/gateway/tcp_test.go)）。

协程与资源：每个已认证连接都有一个写协程，嵌入模式每连接 goroutine 由 1（读循环）变为 2（读循环 + 写协程）。独立 Gate 模式下 TCP 服务端同样为每连接启动写协程，但 Gate 的推送走 `WritePacket` 直写、不入这条队列，写协程通常空闲；Gate 模式的心跳 ACK 与嵌入模式一样走 `writeQueued`（队列空闲时直写，否则排在队列后面等写出）。连接返回时先从查找表移除会话、关闭会话、等写协程退出，再释放连接槽并发布关闭事件，`Stop` 的排空因此也覆盖写协程；订阅方（`OnSessionClosed`）收到关闭时，这条连接排队与在途的字节已经还给全进程驻留预算（2026-10-20 起；修前关闭事件先于关闭 socket 发布）。

指标口径：`player_tcp_outbound_budget_exhausted_total` 计因全进程驻留预算被拒的入队次数（不关连接，同一帧推给玩家的每条连接各计一次）。`player_tcp_outbound_overflow_total` 计被拒的入队次数，不是被关闭的连接数——队满那一帧关闭连接后，后续推送看到连接已关闭不再计这一项，但并发推送方可能在关闭前同时撞到上限，各计一次。`player_tcp_push_closed_total` 计推送路径（`PushPlayer` / `PushSession` 等）因 `errConnectionBroken` 注销并关闭连接的次数：溢出时计一次；写协程因写超时 / socket 失败自己关闭的连接，在读循环注销它之前若还有推送到达，这些推送同样会各计一次，没有推送到达则不计。`player_tcp_write_error_total` 计一次写 socket 失败（写协程、读循环直写、Gate 的 `WritePacket`）；关闭连接时正在进行的那次写会被打断并计入，所以一个停读连接因溢出被关闭时，三项通常各至少加一；连接已关闭后被拒绝的写不计入。

## 5. 停机、指标与验证

停止先关闭准入，再等已接纳 dispatcher、回调与出站真实排空，随后注销发现和关闭 TCP。超时保留实例及依赖，新 context 可继续等待；不把取消等待当事务已经停止。

`gate.channel.result_total` 与 `gate.channel.failure_total` 使用有限 role/phase/result 标签；Stats 返回当前绑定和驻留预算。不要把 PlayerID、SessionID、incarnation 或票据加入指标标签。

已验证正式生成 PB/Nest/双实体 DAO/文件 WAL/Sync、实际 NATS 双 Gate 双 Game、Lockstep 输入/追帧/重连、旧事件隔离、ACK 丢失、广播去重、控制容量与预算、真实 ACL、Stop 重试。测试使用本机隔离资源，鉴权权威、存储和进程范围按[功能验收记录](../release/v1.25.0-VALIDATION.md)逐项说明；不声称已完成独立多进程、真实 Mongo、Linux 实机或跨机完整矩阵。

完整配置、协议、阶段验收和未验收项见[设计与实施契约](GATEWAY-IMPLEMENTATION.md)。目录升级没有旧 import alias，旧工程必须重新生成并编译；稳定基线历史性能不能替代新增 Gate 的结果。

## 生成工程与标准 C# 客户端联合链路

自 v1.35.0 起生成 bootstrap 提供启动选项，默认仍是本地 TCP。独立 Gate 模式下，Game 选择内部 Ingress，复用同一业务协议注册与 Nest；不会同时注册本地 TCP Mod：

```go
options := gatewiring.DefaultOptions()
// 配置本环境的 namespace、鉴权器与业务就绪条件等，见上文 Gate 契约。
a, err := bootstrap.NewWithOptions(bootstrap.Options{GameIngress: &options})
if err == nil {
    err = a.Execute()
}
```

该选项不替代独立 Gate 服务的启动、NATS/单实例身份能力和鉴权配置。`GameIngress.RegisterDiscovery` 为 true 时，生成的 bootstrap 给承载玩家接入的服务换上不自动登记的 etcd Mod（`playerDiscovery`），发现候选只由 Game Ingress 登记；2026-11-12 之前两者都登记，进程启动即以 `etcd Discovery: service is already registered` 退出（真实进程演练发现，此前只有测试宿主里的 Game Ingress，没有用生成的 bootstrap 起过带发现登记的进程）。启用 `access.player.self_sync` 时，生成器把同一个 SelfSync 接到所选 Ingress；组同步仍共用它。没有该开关的业务继续显式配置自己的 Sync Manager。

联合验证入口：

```bash
bash scripts/test-gate-client.sh
```

需要 Go、.NET 10、nats-server。脚本创建只监听 loopback 的临时 NATS，随机端口，退出时仅关闭自己的进程。正式 CLI 生成 game-demo，C# 编译该工程的业务 proto、MessageIds、DAO proto 和 Apply，再经真实 TCP → Gate → NATS → 生成 Game Ingress → PB 注册处理器 → 生成 Nest Sender → 两实体 DAO → strict 本地 WAL → on_change → Gate → C# GameClient。

验证内容：无效票据拒绝、Full、AddExp PB 应答和 Delta、服务端取消订阅后的 Remove、手动重连后 Full 等于已提交状态；同时核对一次事务包含 Player/World 两实体且 WAL 只追加一次。Sync 的兜底间隔为一小时，因此更新不能靠 20Hz 定时扫描通过。默认 TCP 与 Gate 的启动选项分别验证。生成工程内 Go 联合测试启用 race。

C# 入口位于 `client/dotnet/Roost.Client.Demo`，使用 SDK 回调，不另写网络解码器。Remove 的触发是测试宿主收到客户端观察屏障后调用正式 Unsubscribe，不向生产业务协议添加测试控制消息。

边界：这是同机跨语言联合验证。鉴权器使用固定测试票据与 Principal，不代表 account/Redis 登录票生命周期验收；业务处理器绑定正式 AddExp 协议和 Sender，但宿主注入内存实体 getter 和记录投影器。WAL 为真实文件，Mongo 落库、崩溃恢复、跨机网络、Unity Editor/IL2CPP、多人负载均未由此验证。此 C# 入口依赖测试宿主的移除观察屏障，不是可直接连接任意生产地址的通用机器人。

本轮本机 macOS 验证（2026-10-31）：联合脚本最终运行通过，生成工程内两条测试均执行、零 SKIP，Go 侧启用 race；`go test ./codegen/internal/roost`（约 236 秒）、`go test -count=1 .`、C# SDK 自测与格式检查通过。未据此给出吞吐量或延迟性能结论。

同一宿主也可验证 native C++：`ROOST_GATE_CPP=1 ROOST_PROTOBUF_PREFIX=/path/to/protobuf bash scripts/test-gate-client.sh`。C++ 使用正式生成消息类型、通用 DAO 视图库与相同 AddExp 路由（由 Go 生成 msgid 传入），验证内容和存储/鉴权替身边界与上文一致；2026-10-31 本机 macOS 已通过。

### 真实账号、Gate 与存储联合验收

自 v1.35.0 提供 `bash scripts/test-gate-real.sh`。先用隔离集成环境启动 MongoDB 副本集、Redis、NATS，并在当前 shell source 该环境的 `env.sh`；脚本要求 `ROOST_DATAENGINE_IT_MONGO_URI`、`ROOST_DATAENGINE_IT_REDIS_ADDR`、`ROOST_DATAENGINE_IT_NATS_URL`，不会启动或停止共享服务。需要 Go 与 .NET SDK。

该入口沿用上面的生成工程与 C# 客户端，用正式 account Service、Redis Stores、NATS MessagePack RPC 完成 Login/CreateRole/SelectRole/ValidateSession，再把取得的会话票据交给 Gate 和 Game 鉴权。验证错误身份被拒、角色创建幂等、其他账号不能选择该角色；票据通过子进程环境传递，不打印到日志。上游身份签发器是每轮随机密钥的测试 HMAC verifier，不代表接入了 Steam/Apple 等第三方账号平台。

业务仍走正式 AddExp PB、Nest、两个生成 DAO、strict 文件 WAL 与 on_change；投影使用实际 MongoStore（副本集 majority+journal）。随后重建 account Service 验证 Redis 会话，关闭并重开 Mongo 客户端与 WAL/投影器，重复投影同一事务，核对两实体数据、版本及 schema 不变，并用生成的 RestorePersisted 从 Mongo 冷水合新 DAO。在线业务的实体 getter 仍由测试宿主提供，未验证生产 Repository 的进程启动装配。

2026-11-01 在 macOS arm64、MongoDB 8.0.32 隔离环境中，C# 和 C++ 两条链均通过，生成 Go 测试启用 race、零 SKIP，Full/Delta/Remove/重连与存储检查均执行。C++ 复跑方式：在同一环境中设置 `ROOST_GATE_REAL=1 ROOST_GATE_CPP=1 ROOST_PROTOBUF_PREFIX=<本机 protobuf 安装目录>`，运行 `go test -count=1 -v -timeout 10m -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost`，需要 CMake/C++ 工具链。

2026-11-12 复跑：game-demo 的经验与等级搬进 profile 模块之后这两个入口的夹具与演示客户端没有跟着改、一直编译不过，当日修复；C# 链在临时 NATS（`test-gate-client.sh`）与隔离环境（`test-gate-real.sh`）下都通过，C++ 链改了没有跑（本机没有 protobuf）。

这里的恢复是对象/驱动/WAL 重开及 DAO 冷水合，不是进程 SIGKILL、断电或 Redis 重启。未验证 Linux、跨机、Unity Editor/IL2CPP、其他设备、多玩家容量与第三方身份平台；该结果也不替代各玩法自己的真实持久恢复验证。

### Robot 混合诊断

当前 `scripts/perf/gate-mixed.sh` 已接真实 account/Redis/NATS、普通 PB 请求、正式 heartbeat、strict WAL/Mongo 和 PB EntitySync Full/Delta/Remove。它是单账号共享 World 热点，不是 MMO 容量工具；参数、失败样本和对照见 [当前性能验证](../performance/ROBOT-GATE-20261101.md)。1000 玩家/10000 Entity 的 Gate+AOI、1Hz/10Hz 和少量 Saga 资源干扰由既有 nest-game/Saga 基准分别统计；不将其称为每种玩法已完成生产验收。
