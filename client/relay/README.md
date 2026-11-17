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
client.Leave(ctx)                                      // 经 lobby 离开再断开；只断开用 Close
```

- **lobby 错误**：非 200 返回 `*LobbyError{Status, Code, Message}`，`errors.Is(err, relay.ErrRoomFull)` 按错误名判断；`Temporary()` 是 5xx。
- **线程**：方法可以从任意 goroutine 调用；回调都在连接的读 goroutine 上依次调用，可以在回调里 Send / Kick / Close，但不能等 `Done()`。回调慢会拖住收帧，进而被 relay 当慢连接断开。
- **心跳**：`PingInterval` 缺省 10s，Ping 载荷是 8 字节单调时钟纳秒，Pong 原样带回算 `RTT()`；`ReadTimeout`（缺省 30s）内一帧都没收到就当断线。
- **重连**：`Rejoin(ctx)` 或自动重连都先调 lobby 的 join（幂等）拿新令牌再连 relay。只在 `Disconnected`（读写出错、超时，没收到 Closed）或 `idle_timeout` 时自动重连；每次尝试遇到 lobby 4xx 或 relay 的终止性 Closed 就停，`token_rejected` 会换新令牌再试。成员重连后槽位可能变（`OnReconnected` 给新的 Welcome）。
- **只连 TCP**：ticket 的 `RelayAddr` 是 `ws://` 时 Connect 直接报错；向 lobby 要 `transport: tcp`（缺省）。
- **扩展点**：`OnControl(op, fn)` 收任意控制帧操作码（内置处理过的也交给它）；不认识又没注册的丢弃。换 host 的控制帧（64～127）下一批加进 `controlHandlers`，回调位置 `OnYouAreHost` / `OnHostChanging` / `OnHostChanged` / `OnHostResumed` 已留好，这一批不触发；发控制帧用 `SendControl`。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh   # 房间目录用这个 Redis；没设时用内存目录（日志写明）
go test -race ./client/relay
```

`peer_test.go` 在测试进程里起真实的 `service/relay` 与 `service/lobby`（account、NATS、etcd 换成进程内的等价物）。同一个测试二进制以 `ROOST_RELAYPEER_SERVE=1` 运行时不跑用例、只当对端，C# 与 TS 的测试用它。
