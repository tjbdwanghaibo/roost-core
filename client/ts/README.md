# @roost/relay-client：开房间场景的 TS SDK（浏览器）

lobby 的 HTTP 调用（`LobbyClient`，用 `fetch`）与 relay 的 WebSocket 连接（`RelayClient`）。方案 FEATURE-2026-11-16-lobby-relay §4.7.5 的基础部分；三份 SDK 的共同说明见 [client/README](../README.md#开房间-sdklobby--relay)。运行时没有依赖；`ws` 只在 node 测试里代替浏览器的全局 `WebSocket`。

```ts
import { ALL, LobbyClient, RelayClient } from "@roost/relay-client";

const lobby = new LobbyClient("https://lobby.example.com", gt1);               // 跨域要 lobby.cors_origins 放行页面 Origin
const ticket = await lobby.createRoom({ game: "party", mode: "duel" });        // transport 缺省 ws；成员用 joinRoom
const client = new RelayClient(lobby, { game: "party", reconnect: { maxAttempts: 5 } });
client.onGame((f) => {});            // 房主：f.slot 是成员槽位；成员：来自房主
client.onRoom((f) => {});
client.onMemberJoined((m) => {});    // 只有房主收到
client.onDisconnected((d) => {});    // d.reconnecting 为 false 是最后一条
const welcome = await client.connect(ticket); // relay 拒绝时 RelayClosedError
client.sendGame(ALL, 101, payload);
client.kick(3); client.mute(3, true);
await client.leave();
```

- **帧**：每条 WebSocket 二进制消息发一帧；收的时候按字节流解（`FrameReader`），一条消息几帧、一帧拆在几条消息里都能读。编码与 [relay-packets.json](../spec/relay-packets.json) 逐字节相同。
- **心跳、重连、扩展点**：与 Go 版相同（缺省 10s Ping、30s 读超时；`rejoin()` / `reconnect.maxAttempts` 先调 lobby 的 join 拿新令牌；`onControl`、`sendControl`；换 host 回调位置 `onYouAreHost` / `onHostChanging` / `onHostChanged` / `onHostResumed` 这一批不触发）。
- **只连 WebSocket**：ticket 的地址不是 `ws://` / `wss://` 时 `connect` 报错。
- 方案 §4.7.5 里的快照定时推送与压缩、Worker 规则循环、补帧、`visibilitychange` 让出房主、`sessionStorage` 存快照依赖换 host，下一批做。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
cd client/ts && npm ci && npm test
```

`npm test` 先 `tsc`，再用 node 自带的 test runner 跑：金样与拒绝规则、字节流解码、假 WebSocket 下不认识的操作码；然后 `go test -c ./client/relay` 并以 `ROOST_RELAYPEER_SERVE=1` 起对端（真实 relay 与 lobby，relay 开 WebSocket 监听），经 `ws` 包跑 lobby 错误、建房 / 加入 / 游戏通道往返 / 房间通道直达 / 禁言 / 踢人 / 离开 / 改状态、断线重连、心跳。需要本机 Go 工具链。没有在真实浏览器里跑过（浏览器演练在方案 §5 的 Web 端演练里做）。
