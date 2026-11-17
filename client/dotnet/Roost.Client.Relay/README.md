# Roost.Client.Relay：开房间场景的 C# SDK

lobby 的 HTTP 调用（`LobbyClient`）与 relay 的 TCP 连接（`RelayClient`）。`netstandard2.1`、可空引用、警告即错误；与 `Roost.Client` 一样不带第三方依赖（lobby 的 JSON 用内置的最小读写）。三份 SDK 的共同说明见 [client/README](../../README.md#开房间-sdklobby--relay)。

```csharp
var lobby = new LobbyClient("https://lobby.example.com", gt1);
var ticket = await lobby.CreateRoomAsync(new CreateRoomRequest { Game = "party", Mode = "duel" }); // 成员用 JoinRoomAsync
var client = new RelayClient(lobby, new RelayClientOptions { Game = "party", ReconnectAttempts = 5 });
client.GameFrame += frame => { /* 房主：frame.Slot 是成员槽位；成员：来自房主 */ };
client.RoomFrame += frame => { };
client.MemberJoined += member => { };            // 只有房主收到
client.Disconnected += d => { };                 // d.Reconnecting 为 false 是最后一条
var welcome = await client.ConnectAsync(ticket); // relay 拒绝时 RelayClosedException
// 引擎主线程每帧：client.Pump();
client.SendGame(RelayCodec.All, 101, payload);   // 房主：全体或某个槽位；成员：只到房主
client.SendRoom(3, 7, chat);
client.Kick(3); client.Mute(3, true);            // 只有房主，否则 InvalidOperationException
await client.LeaveAsync();                       // relay 上发 Leave、再经 lobby 离开、Dispose；只断开用 Dispose

// 换 host（方案 §4.6）：回调都经 Pump
var options = new RelayClientOptions { Game = "party", SnapshotState = () => game.HostOnlyState() }; // 当房主时每 3s 推一次
client.OnYouAreHost(e => { game.TakeOver(e.Source, e.State, e.Members); client.HostReady(e.Epoch); }); // 或 AutoHostReady = true
client.OnHostChanging(epoch => { });             // 发给房主的帧被丢了；SDK 不重发，游戏决定
client.OnHostChanged(e => { });                  // 全体：新房主就绪
client.OnHostResumed(e => { });                  // 成员：房主在宽限期内回来了
client.OnYouAreMember(epoch => { });             // 被换下的旧房主、超时的候选
client.OnRejected(r => { });                     // 快照 / 转交被拒
client.Transfer(3, handoff); client.SetSuccessors(new uint[] { 3, 2 }); client.Snapshot(state); // 只有房主
var role = client.Role;                          // Slot、IsHost、IsCandidate、HostSlot、Epoch
```

- **线程**：网络在后台收发；全部通知只由 `Pump` 在调用它的线程派发（与 `GameClient` 相同），回调抛错交给 `Error`。`Send*` 可以从任意线程调用，没有连接或写失败返回 false。积压超过 `MaxPendingEvents` 时断开连接。`Dispose` 之后不再通知。
- **lobby 错误**：`LobbyException`（`Status`、`ErrorName`、`Code`、`IsTemporary`）；被踢的账号再 join 是 403、`Code == LobbyErrorCode.Kicked`。
- **心跳、重连、扩展点**：与 Go 版相同（成员 10s Ping、房主与候选 `HostPingInterval` 600ms、30s 读超时；`RejoinAsync` / `ReconnectAttempts` 先调 lobby 的 join 拿新令牌，开局后掉线也能回原座；遇到 `LobbyRejected` 停、`LobbyUnavailable` 按退避再试；`OnControl`、`SendControl`）。读超时由心跳任务检查，不依赖 `ReadAsync` 的取消（Unity Mono 上不一定生效）。
- **换 host**：角色规则、快照 seq 接续（不低于 `epoch << 32`）、压缩（`RelayCompressors.Deflate` 缺省，与 Go 的 `relay.Deflate` 互通；实现 `IRelayCompressor` 可换 LZ4 / zstd）、收帧上限、被踢（房主可踢离线座位，收到第二条 `MemberLeft{Kicked}`）都与 [Go 版](../../relay/README.md) 相同。差别：定时快照在 `Pump` 里向游戏要状态（`SnapshotState`），`AutoHostReady` 在 `OnYouAreHost` 回调（Pump 里）返回后回；`Snapshot` 返回 `Sent` / `TooSoon` / `NotConnected`，不是房主或压缩后太大抛异常。
- **只连 TCP**：ticket 的地址是 `ws://` 时 `ConnectAsync` 抛 `ArgumentException`。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
dotnet run --project client/dotnet/Roost.Client.Relay.Tests
dotnet build-server shutdown
```

先核对 [relay-packets.json](../../spec/relay-packets.json) 金样与拒绝规则、用本地 TcpListener 扮 relay 核对不认识的操作码与换 host 的 13 条（经 SDK 的公开方法发、经分发路径收），再 `go test -c ./client/relay` 并以 `ROOST_RELAYPEER_SERVE=1 ROOST_RELAYPEER_WS=0` 起对端（真实 relay 与 lobby），跑 lobby 错误、建房 / 加入 / 游戏通道往返 / 房间通道直达 / 禁言 / 踢人 / 离开 / 改状态、断线重连，以及换 host（转交、断线后候选拿快照、宽限期内回来、候选顺序、快照节奏、定时快照、房主心跳、重连遇到 lobby 拒绝或不可用、开局后重连与踢离线座位）。`ROOST_RELAY_TESTS_ONLY=<名字片段>` 只跑名字含它的真实对端用例。需要本机 Go 工具链。没有在 Unity Editor / IL2CPP 上运行过。
