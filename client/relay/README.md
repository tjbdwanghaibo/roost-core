# client/relay：开房间场景的 Go SDK

lobby 的 HTTP 调用（`Lobby`）与 relay 的 TCP 连接（`Client`）。方案 FEATURE-2026-11-16-lobby-relay §4.5；契约见 [LOBBY](../../docs/framework/LOBBY.md)、[RELAY](../../docs/framework/RELAY.md)；三份 SDK 的共同说明见 [client/README](../README.md#开房间-sdklobby--relay)。

帧编解码直接用 `client/wire`（`EncodeRelay` / `ReadRelay`），不另起一套。

```go
lobby := relay.NewLobby("https://lobby.example.com", relay.StaticGameToken(gt1), nil)
ticket, err := lobby.CreateRoom(ctx, relay.CreateRoomRequest{Game: "party", Mode: "duel"}) // 房主；成员用 JoinRoom
client := relay.NewClient(lobby, relay.Options{
	Game:      "party",
	Reconnect: relay.ReconnectPolicy{MaxAttempts: 5}, // 缺省 0：不自动重连
})
client.OnGame(func(f relay.Frame) { /* 房主：f.Slot 是成员槽位；成员：来自房主 */ })
client.OnRoom(func(f relay.Frame) { /* 聊天 / 表情 / 语音，f.Slot 是发送者 */ })
client.OnMemberJoined(func(m relay.Member) {})        // 只有房主收到
client.OnDisconnected(func(d relay.Disconnect) {})    // d.Reconnecting 为 false 是最后一条
welcome, err := client.Connect(ctx, ticket)            // 首帧发令牌，等 Welcome；relay 拒绝时 *ClosedError
client.SendGame(relay.All, 101, payload)               // 房主：全体成员或某个槽位；成员：只到房主
client.SendRoom(3, 7, chat)                            // 直达槽位 3（房主也可以）
client.Kick(3); client.Mute(3, true)                   // 只有房主；否则 ErrNotRoomHost
client.Leave(ctx)                                      // relay 上发 Leave、再经 lobby 离开、断开；只断开用 Close
```

换 host（方案 §4.6，契约 [RELAY §2.3、§4A](../../docs/framework/RELAY.md)）：

```go
client := relay.NewClient(lobby, relay.Options{
	Game:          "party",
	SnapshotState: func() []byte { return game.HostOnlyState() }, // 当房主时 SDK 每 3s 推一次（压缩后）
})
client.OnYouAreHost(func(e relay.YouAreHost) {             // relay 选自己当新房主
	game.TakeOver(e.Source, e.State, e.Members)             // State 已解压；交接数据或快照，或没有（StateNone）
	_ = client.HostReady(e.Epoch)                           // 准备好再回；Options.AutoHostReady 让 SDK 回调后自动回
})
client.OnHostChanging(func(epoch uint64) {})               // 发给房主的帧被丢了；SDK 不重发，游戏决定
client.OnHostChanged(func(e relay.HostEvent) {})           // 全体：新房主就绪（e.HostSlot、e.Account）
client.OnHostResumed(func(e relay.HostEvent) {})           // 成员：房主在宽限期内回来了
client.OnYouAreMember(func(epoch uint64) {})               // 被换下的旧房主、超时的候选
client.OnRejected(func(r relay.Rejected) {})               // 快照 / 转交被拒（连接保留）
client.Transfer(3, handoff)                                // 房主：主动转交给槽位 3
client.SetSuccessors([]uint32{3, 2})                       // 房主：候选顺序
client.Snapshot(state)                                     // 房主：随时手动推一份
role := client.Role()                                      // Slot、Host、Candidate、HostSlot、Epoch
```

- **lobby 错误**：非 200 返回 `*LobbyError{Status, Code, Message}`，`errors.Is(err, relay.ErrRoomFull)` 按错误名判断；`Temporary()` 是 5xx。
- **线程**：方法可以从任意 goroutine 调用；回调都在连接的读 goroutine 上依次调用，可以在回调里 Send / Kick / Close，但不能等 `Done()`。回调慢会拖住收帧，进而被 relay 当慢连接断开。
- **心跳**：成员按 `PingInterval`（缺省 10s），房主与候选按 `HostPingInterval`（缺省 600ms，短于 relay `host_idle_timeout` 缺省 2s 的三分之一）；角色一变立即按新间隔。Ping 载荷是 8 字节单调时钟纳秒，Pong 原样带回算 `RTT()`；`ReadTimeout`（缺省 30s）内一帧都没收到就当断线。
- **重连**：`Rejoin(ctx)` 或自动重连都先调 lobby 的 join（幂等）拿新令牌再连 relay；开局后掉线的成员席位还在，join 照样放行、回到原槽位。只在 `Disconnected`（读写出错、超时，没收到 Closed）或 `idle_timeout` 时自动重连；每次尝试遇到 lobby 4xx（含被踢 `ErrKicked`）或 relay 的终止性 Closed（含 `lobby_rejected`）就停，`token_rejected` 换新令牌再试，`lobby_unavailable`（relay 向 lobby 验权失败）按退避再试。
- **被踢**：记在 lobby。被踢的账号再 join 答 403 `kicked`（`errors.Is(err, relay.ErrKicked)`）；拿还没过期的旧令牌直连 relay 收到 `Closed{kicked}`（relay 本地记着）。房主也能踢有座位但已离线的槽位，房主会收到这个槽位的第二条 `MemberLeft`（原因 kicked）；SDK 不维护离线座位表，游戏自己维护的话收到 kicked 就删座位。
- **角色**（`Role()`）：只按 relay 的控制帧更新——Welcome；`YouAreHost` → 候选；`HostChanged` 槽位是自己 → 房主，否则成员；`YouAreMember` → 成员（房主槽位 0，还在选人）；`HostResumed` → 房主槽位。`HostChanging` 不改角色。Welcome 不带 epoch，连上后还没见过换 host 时 `Epoch` 是 0。`IsHost()`、踢人、禁言、`Transfer`、`SetSuccessors`、`Snapshot` 都以已就绪的房主为准（候选回 HostReady、收到 HostChanged 之后）。
- **快照**：`Snapshot(state)` 经 `Compressor`（缺省 `relay.Deflate`，RFC 1951 原始 deflate，与 C# `DeflateStream` 互通；可换成 LZ4 / zstd 的实现，`NoCompression` 不压）压缩，seq 由 SDK 递增；离上一份不到 `SnapshotMinInterval`（缺省 1s）不发（`ErrSnapshotTooSoon`），压缩后超过 `SnapshotMaxBytes`（缺省 256 KiB，要与 relay 一致）不发（`ErrStateTooBig`）。设了 `SnapshotState` 时当房主每 `SnapshotInterval`（缺省 3s）推一次，在心跳 goroutine 上调用、与其他回调并发。新房主的 seq 从 `YouAreHost` 里的 seq 往上接，并且不低于 `epoch<<32`（主动转交的 `YouAreHost` 不带 seq，relay 可能还存着旧房主更大的 seq）。交接数据（`Transfer`）同样压缩；`YouAreHost.State` 是解压后的，解压失败时 `StateErr` 非空。房间里每个客户端要用同一种 `Compressor`。
- **收帧上限**：取 `MaxPayloadBytes` 与 `SnapshotMaxBytes + 16 KiB` 的大值，装得下带满快照的 `YouAreHost`。
- **离开**：`Leave(ctx)` 先在 relay 上发 `Leave`（房主离开不等宽限期，立即换 host），再经 lobby 让出席位，最后 `Close`。
- **只连 TCP**：ticket 的 `RelayAddr` 是 `ws://` 时 Connect 直接报错；向 lobby 要 `transport: tcp`（缺省）。
- **扩展点**：`OnControl(op, fn)` 收任意控制帧操作码（内置处理过的也交给它）；不认识又没注册的丢弃；发控制帧用 `SendControl`。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh   # 房间目录用这个 Redis；没设时用内存目录（日志写明）
go test -race ./client/relay
```

`peer_test.go` 在测试进程里起真实的 `service/relay` 与 `service/lobby`（account、NATS、etcd 换成进程内的等价物；relay 向 lobby 的验权直接调 `lobby.HandleRelayVerifyData`）。同一个测试二进制以 `ROOST_RELAYPEER_SERVE=1` 运行时不跑用例、只当对端，C# 与 TS 的测试用它；`POST <lobby_url>/harness/admission {"mode":"ok|reject|down"}` 切换验权结果（测重连遇到 `lobby_rejected` / `lobby_unavailable`）。换 host 的用例在 `host_test.go`。
