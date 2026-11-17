# @roost/relay-client：开房间场景的 TS SDK（浏览器）

lobby 的 HTTP 调用（`LobbyClient`，用 `fetch`）、relay 的 WebSocket 连接（`RelayClient`，含换 host）与 Web 端房主辅助（`WebHost`）。方案 FEATURE-2026-11-16-lobby-relay §4.6、§4.7；relay 现行契约见 [RELAY](../../docs/framework/RELAY.md)；三份 SDK 的共同说明见 [client/README](../README.md#开房间-sdklobby--relay)。运行时没有依赖；`ws` 只在 node 测试里代替浏览器的全局 `WebSocket`。

```ts
import { ALL, LobbyClient, RelayClient } from "@roost/relay-client";

const lobby = new LobbyClient("https://lobby.example.com", gt2);               // 跨域要 lobby.cors_origins 放行页面 Origin
const ticket = await lobby.createRoom({ game: "party", mode: "duel" });        // transport 缺省 ws；成员用 joinRoom
const client = new RelayClient(lobby, {
  game: "party",
  reconnect: { maxAttempts: 5 },
  snapshot: { take: () => rules.snapshot(), intervalMs: 3000 },               // 当房主时定时推快照（可选）
});
client.onGame((f) => {});            // 房主：f.slot 是成员槽位；成员：来自房主
client.onMemberJoined((m) => {});    // 只有房主收到
client.onYouAreHost((t) => { rules.restore(t.state); client.hostReady(); });  // 或开 autoHostReady
client.onHostChanged((h) => {});     // h.self：自己是不是新房主
client.onDisconnected((d) => {});    // d.reconnecting 为 false 是最后一条
const welcome = await client.connect(ticket); // relay 拒绝时 RelayClosedError
client.sendGame(ALL, 101, payload);
client.setSuccessors([3, 2]);        // 房主：候选顺序（显式声明后 WebHost 不再自动排）
client.setForeground(false);         // 成员：告诉房主自己切到后台（换 host、重连后 SDK 自动重发）
client.onMemberPresence((p) => {});  // 房主：p.slot、p.foreground
await client.transfer(3, handoff);   // 房主：主动转交，交接数据压缩后随帧带走
await client.pushSnapshot();         // 房主：手动推一份快照（关键时刻）
```

## 帧、心跳与重连

- **帧**：每条 WebSocket 二进制消息发一帧；收的时候按字节流解（`FrameReader`），一条消息几帧、一帧拆在几条消息里都能读。编码与 [relay-packets.json](../spec/relay-packets.json) 逐字节相同（含换 host 的 13 条）。
- **收帧上限**：`max(maxPayloadBytes, snapshotMaxBytes + 64 KiB)`。`YouAreHost` 可以比 relay 的 `max_payload_bytes` 大（带满快照加成员名单），`snapshotMaxBytes` 要等于 relay 的 `snapshot_max_bytes`（缺省 256 KiB）。
- **心跳**：成员每 `pingIntervalMs`（缺省 10s）发一个 Ping；房主（含等 `HostReady` 的候选）在 `hostPingIntervalMs`（缺省 600ms）内没发过任何帧就补一个 Ping——relay 的 `host_idle_timeout` 缺省 2s。`readTimeoutMs`（缺省 30s）内一帧都没收到就当断线。后台标签页的主线程计时器被限流时，`pulse()` 让外部（Worker 的消息）驱动心跳。
- **重连**：`rejoin()` / `reconnect.maxAttempts` 先调 lobby 的 join 拿新令牌。连接自己断了、relay 判空闲超时、`token_rejected`、`lobby_unavailable`（relay 向 lobby 验权失败）按退避重试；被踢、`lobby_rejected`、房间关闭、被顶号、离开、relay 停机都不重连。被踢的人：lobby 的 join 答 403 `kicked`；拿没过期的旧令牌直连，同一个 relay 上答 `Closed{kicked}`（relay 自己的踢出记录），relay 重启后才是 `Closed{lobby_rejected}`，两种都终止自动重连。
- **只连 WebSocket**：ticket 的地址不是 `ws://` / `wss://` 时 `connect` 报错。

## 换 host

| relay 发来 | SDK 做什么 | 回调 |
|---|---|---|
| `YouAreHost` | 角色变 `candidate`，快照 seq 抬到 `max(本地, 帧里的 seq, epoch << 32)`；解压状态后调回调。游戏准备好后调 `hostReady()`；开了 `autoHostReady`（缺省关，与 Go 的 `AutoHostReady` 相同）时回调返回（Promise 完成）就自动回 `HostReady{epoch}`，回调抛错则不回。候选在 `host_ready_timeout` 内不回，relay 换下一个 | `onYouAreHost(takeover)`：`epoch`、`source`、`snapshotSeq`、`snapshotAgeMs`、`members`、`state`（解压后，来源为 None 或解压失败时 null）、`rawState` |
| `HostChanged` | 自己是新房主：角色变 `host`、开定时快照；不是：降为 `member`；候选顺序清空（`successorsDeclared` 复位）；设过前台状态的重发一次 `Presence` 给新房主 | `onHostChanged({epoch, slot, account, self})` |
| `MemberPresence` | 只有房主收到：成员的前台状态 | `onMemberPresence({slot, foreground})` |
| `YouAreMember` | 角色变 `member`、停定时快照 | `onYouAreMember(epoch)` |
| `HostChanging` | `hostChanging` 置位，游戏暂停发给房主的帧（SDK 不替它丢） | `onHostChanging(epoch)` |
| `HostResumed` | `hostChanging` 复位 | `onHostResumed({epoch, slot})` |
| `Rejected` | 连接保留 | `onRejected({op, code, seq})` |

发出：`transfer(slot, handoff)`、`setSuccessors(slots)`、`pushSnapshot(data?)`、`hostReady(epoch?)`、`sendLeave()`（`Leave` 控制帧，relay 回 `Closed{left}`）、`setForeground(foreground)`。状态：`hostRole`、`epoch`、`hostChanging`、`snapshotSeq`、`successors`、`successorsDeclared`（游戏这一任房主期间显式调过 `setSuccessors`）、`onlineMembers`（当房主时）。

- **epoch**：`Welcome` 带着房间此刻的 epoch（13 字节，[RELAY §2.2](../../docs/framework/RELAY.md)；选人阶段进房的拿到新的一轮），`connect` / 重连收到它就更新 `epoch`，换过 host 之后才进房或重连回来的客户端不用等下一次换 host。
- **前台上报**：`setForeground(foreground)` 发 `Presence`，relay 只转给当前房主（`onMemberPresence`）；SDK 记下状态，收到 `HostChanged`、每次连上（含自动重连、`rejoin`）都自动重发，从没调过不发；没有连接时返回 false，状态仍记下。`WebHost` 按页面可见性自动调它（见下）。

- **快照 seq**：每份加 1，房间内跨房主递增。收到 `YouAreHost` 时取 `max(本地, YouAreHost 的 seq, epoch << 32)`，下一份再加 1（与 Go / C# 相同；relay 不管来源都在 `YouAreHost` 里带上它存着的最新 seq，`epoch << 32` 保证大于以前任何房主发过的 seq）；刷新页面后 `raiseSnapshotSeq` 从存档往上接。
- **快照节奏**：`snapshot.intervalMs` 缺省 3s；离上一份不到 `snapshotMinIntervalMs`（缺省 1s，同 relay 的 `snapshot_min_interval`）的 `pushSnapshot` 不发、不占 seq，抛 `SnapshotTooSoonError`；当上房主时重新计算。手动推一份后定时推送从那一刻重新计时。
- **踢人**：房主可以踢有座位但不在线的槽位，relay 回 `MemberLeft{kicked}`（对已离线的人是第二条 `MemberLeft`）。SDK 只维护在线成员表，不维护离线座位，收到就照常交给 `onMemberLeft`。
- **开局后掉线**：lobby 给开局后掉线的成员保留席位，再 join 回到原座（满员也放行），所以开局后的自动重连照常可用；开局后新来的人 join 答 `room_started`。
- **压缩**：快照与交接数据透明压缩后上线，到了 `YouAreHost` 透明解压。`compressor` 缺省 `defaultCompressor()`：RFC 1951 原始 deflate，浏览器用自带的 `CompressionStream('deflate-raw')`，没有时退到 `node:zlib`（与 Go 的 `compress/flate`、C# 的 `DeflateStream` 字节互通）；`noCompression` 不压缩。实现 `Compressor` 接口可以换成 LZ4、zstd（游戏自带库，见下）。解压上限 `MAX_STATE_BYTES`（16 MiB，防解压炸弹）；压缩后（含标识）超过 `snapshotMaxBytes` 的本地就拒绝（不发给 relay）。
- **压缩方式标识**：非空的快照与交接数据在线上是 `[compressor.id 1B][压缩后的字节]`（`packState` / `unpackState`，[RELAY §2.3](../../docs/framework/RELAY.md)，relay 不解析）。收到时按标识选解压方式：自己的 `compressor`，或内置的不压缩（0）、raw deflate（1）——内置的两种谁都解得开；游戏自带的压缩方式，房间里每个客户端都要配上，否则新房主的 `takeover.stateError` 是 `UnknownCompressionError`（`method` 是收到的标识）；标识对、内容坏是解压错误。sessionStorage 存档存的也是带标识的字节。
- **接入自己的 zstd / LZ4**：SDK 不带第三方依赖（维护者 2026-11-17 定，不要为它加 npm 依赖到 SDK 里）。游戏自带库，实现 `Compressor`、`id` 取 2～255。下面的代码原样在 `test/compress.test.ts` 里编译并走一遍（`ZstdCodec` 是代表库的接口桩）：

  ```ts
  /** 代表游戏自带的 zstd 库（例如某个 zstd 的 wasm 包）；SDK 不带它。 */
  interface ZstdCodec {
    compress(data: Uint8Array): Uint8Array;
    /** 解压，至多解出 maxSize 字节（库自己能限就在库里限）。 */
    decompress(data: Uint8Array, maxSize: number): Uint8Array;
  }

  /** 把它接成 Compressor。id 取 2：0、1 归内置的不压缩与 raw deflate，LZ4 照样接、取 3。 */
  function zstdCompressor(codec: ZstdCodec): Compressor {
    return {
      name: "zstd",
      id: 2,
      compress: async (data) => codec.compress(data),
      decompress: async (data, limit) => {
        const out = codec.decompress(data, limit + 1);
        if (out.length > limit) throw new Error(`relay: inflated state exceeds ${limit} bytes`);
        return out;
      },
    };
  }

  const client = new RelayClient(lobby, { game: "party", compressor: zstdCompressor(codec) });
  ```

  要不要换：deflate 与 zstd 在真实状态上的耗时与大小差距没有测过，先量自己的状态再决定。

## Web 端房主辅助（`WebHost`）

玩法规则写成纯 TS 模块 `HostRules`（`restore` / `input` / `step` / `snapshot`），不碰网络与计时器；`WebHost` 把它接到 `RelayClient` 上：

```ts
// 主线程
const host = new WebHost({
  lobby, account, relay: { game: "party", reconnect: { maxAttempts: 5 } },
  runner: new WorkerRunner(new Worker(new URL("./rules.worker.js", import.meta.url), { type: "module" })),
  hiddenHandoffMs: isMobileUserAgent(navigator.userAgent) ? 0 : 5000,
});
host.onFrame((f) => render(f));      // 成员：房主的帧；房主：自己规则的输出（目标为全体或自己）
await host.resume() ?? await host.create({ game: "party", mode: "duel" });
host.input(1, payload);              // 房主交给规则，成员发给房主
// rules.worker.js
serveRules(new MyRules(), { stepMs: 50 });
```

- **Worker**：`serveRules` 在 Worker 里跑规则循环，`WorkerRunner` 在主线程代理它；不用 Worker 时用 `LocalRunner`。桌面浏览器对 Worker 的计时器基本不限流（不是规范保证，只是缓解）；Worker 每一步的消息顺带调 `client.pulse()`，主线程计时器被限流时房主心跳照常。
- **补帧**：`FixedStepLoop` 按 `performance.now()` 的实际经过时间推进固定步长，一次最多补 `maxCatchUpSteps` 步（缺省 1 秒的步数），多出的时间丢掉不追。
- **前台上报**：每个页面（房主、成员都一样）创建时和每次 `visibilitychange` 都 `setForeground(可见)`。
- **候选顺序**：房主收到成员的上报、成员进出时，游戏这一任没有显式 `setSuccessors` 的，`WebHost` 自动声明“前台的按槽位升序在前，其余按槽位升序在后”（顺序没变不重发；一份上报都没有时不发，这时与 relay 的兜底规则相同）；游戏显式声明过的以游戏为准。房主断线、来不及转交时 relay 按这个顺序选人；relay 自己选人不看前台状态。
- **页面隐藏**：房主页面隐藏满 `hiddenHandoffMs`（缺省 5s，手机推荐 0）还没回来，就把房主连同规则当前状态 `transfer` 给候选里第一个在线的成员（游戏显式声明了候选时按声明的；没有时按上一条的前台优先顺序，没有上报就是在线成员里槽位最小的）；没有可转交的成员就留在原地。回到前台不自动抢回。
- **刷新页面**：每推一份快照就把房间、账号、seq 与压缩后的快照存进 `sessionStorage`（`HostSessionStore`）。刷新后 `resume()` 用存档里的房间向 lobby join、连 relay：在 `host_grace` 内回来仍是房主，规则从存档恢复、seq 接着存档；宽限期过了以成员身份回来，存档作废。relay 不回传快照（方案 §4.7.4）。
- **回调槽位**：`RelayClient` 的每个 `onXxx` 只有一个槽位（与 Go、C# 相同），后注册的顶掉先注册的。`WebHost` 不占这些槽位，走 SDK 的内部监听（在业务回调之前调用），所以业务可以在 `webHost.client` 上照常注册任何回调。
- `handOff(target?)`：不给目标时按上面的规则选，给了就转交给它（必须在线），交接数据是规则当前的状态。
- 浏览器 API（`document`、`sessionStorage`、计时器、时钟、Worker）都可以注入，测试用假的。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
cd client/ts && npm ci && npm test
```

`npm test` 先 `tsc`，再用 node 自带的 test runner 跑：

- 金样（27 条逐字节）与拒绝规则、字节流解码、压缩往返与互通；
- 假 relay 下的换 host（`YouAreHost` → `HostReady` → `HostChanged`、转交、候选、快照间隔与 seq 接续、房主心跳、收帧上限、`lobby_rejected` / `lobby_unavailable` / 403 `kicked` 的重连）；
- Web 辅助（补帧与上限、真实 `MessageChannel` 上的 Worker 代理、假 `document` 下的让出、假 `sessionStorage` 下的刷新恢复）；
- 与 Go 互通：测试临时编一个只用 `compress/flate` 的小程序，TS 压的 Go 解得开、Go 压的 TS 解得开；
- 真实对端：`go test -c ./client/relay` 并以 `ROOST_RELAYPEER_SERVE=1` 起 relay 与 lobby（`ROOST_RELAYPEER_BIN` 指向事先编好的对端时直接用它），经 `ws` 包跑 lobby 错误、房间生命周期、断线重连、心跳，以及主动转交、房主断线后按候选换 host 并交出快照、宽限期内刷新回来仍是房主、空闲房主 5s 内不被换掉、被踢与离开后的旧令牌、开局后掉线重连回原座与踢离线座位、验权故障注入（`/harness/admission` 的 down 退避重试、reject 停止）。

需要本机 Go 工具链。没有在真实浏览器里跑过。

## 浏览器演示

```sh
cd client/ts && npm ci && npm run build
source ~/.roost-it/roost-dataengine-it/env.sh
node demo/serve.mjs            # 起对端（真实 relay + lobby），发静态文件，/lobby 同源代理到对端的 lobby
# 浏览器开 http://127.0.0.1:8787/demo/ ，开两三个标签页
```

第一个页面“建房”，其余页面填房间号“加入”；“点一下”的计数由房主页 Worker 里的规则（`demo/rules.js`）维护并广播。页面上显示规则循环在不在跑、最近一次接手（epoch、来源、快照 seq）、最近一次快照、tick 与它多久前更新、广播者槽位。可以看：手动“转交”、把房主页切到后台 5s（让出给候选；后台页仍报 visible 的环境用“模拟隐藏”按钮）、刷新房主页（5s 内回来仍是房主，计数从 sessionStorage 恢复）、关掉房主页（5s 后成员当上新房主，计数从 relay 存的快照恢复）。
