# @roost/client：浏览器 TS SDK

一个包、按子路径导入（维护者 2026-11-18 D1，方案 [FEATURE-2026-11-18-ts-gate-client](../../docs/maintenance/FEATURE-2026-11-18-ts-gate-client.md)）：

| 子路径 | 做什么 | 运行时依赖 |
|---|---|---|
| `@roost/client/gate` | 连玩家接入（Gate / game 的玩家入口）：RS v2 帧、带连接代号的握手、心跳、超时、自动重连、请求 / 应答、推送、校时 | 无 |
| `@roost/client/protobuf` | 用 protobuf-es 生成的 schema 收发业务消息（`roost generate` 的 `client_export` 声明 `ts` 时导出 `*_pb.ts`） | `@bufbuild/protobuf` 2.17.0（peer，精确版本） |
| `@roost/client/relay` | 开房间：lobby 的 HTTP 调用（`LobbyClient`）、relay 的 WebSocket 连接（`RelayClient`，含换 host） | 无 |
| `@roost/client/web` | Web 端房主辅助（`WebHost`、Worker 里的规则循环） | 无 |
| `@roost/client` | 根路径：gate、relay、web 全部导出（不含 protobuf，免得只用 relay 的工程也要装 `@bufbuild/protobuf`） | 无 |

三份 SDK（C#、Go、TS）的共同说明见 [client/README](../README.md)；帧格式同 [client/wire](../wire/)。只在浏览器里跑过 Chromium 内核（Claude 内置浏览器面板，见“浏览器冒烟”）；Safari、Firefox、手机浏览器与小游戏没有验证过（KNOWN-LIMITS）。

**交付**：不发 npm（D2）。在仓库里 `cd client/ts && npm ci && npm run build && npm pack`，把得到的 `roost-client-<版本>.tgz` 放进游戏工程（或 `"@roost/client": "file:../roost-core/client/ts"`）；用 protobuf 子路径的工程另装 `@bufbuild/protobuf@2.17.0`（精确版本，与导出的 `*_pb.ts` 一致，`roost client sync --lang ts` 的报告会提示）。要求 node 22 以上（只在构建 / 测试时；`@bufbuild/protoc-gen-es` 2.17.0 要求 node 22）。

## 连玩家接入（gate 子路径）

```ts
import { ConnectionState, GameClient } from "@roost/client/gate";
import { request, subscribe } from "@roost/client/protobuf";
import { MessageIds } from "./protocol/message_ids.js";          // roost client sync --lang ts 拷来的
import { ErrorCodes } from "./protocol/error_codes.js";
import { PingRequestSchema, PingResponseSchema, ScoreNoticeSchema } from "./protocol/protocol_pb.js";

const client = new GameClient({
  webSocketUrl: entry.gate,                 // 完整的 ws:// / wss:// 地址，由调用方给（portal 的 enter 结果，"transport":"ws"）
  autoReconnect: true,                      // 缺省 false
});
client.on("stateChanged", (state) => show(ConnectionState[state]));
client.on("reconnected", (r) => r.error ? warn(r.error) : resync());   // 重连不重发业务请求
client.on("error", (e) => console.error(e));                           // 回调里抛出的错误
subscribe(client, MessageIds.PushScore, ScoreNoticeSchema, (notice) => show(notice.score));  // 每个推送号一个订阅者

await client.connection.connect(async (signal) => (await portal.enter(signal)).ticket);   // 每次握手（含自动重连）取一次票
request(client, MessageIds.RequestPing, MessageIds.ResponsePing, PingRequestSchema, { nonce: 1n }, PingResponseSchema,
  (response) => { if (response.code === ErrorCodes.HelloBusy) retryLater(); },
  (error) => warn(error));                 // GateTimeoutError / GateClosedError / GateAbortedError / GateProtocolError
client.dispose();
```

- **两层**：`Connection` 管连接（`connect(ticket | 取票函数, signal?)`、`requestAsync`、`beginRequest`、`notify`、`probeClockAsync`、`disconnect`、`dispose`，状态 `state` / `lastError` / `connectionVersion` / `heartbeatRoundTrips`，事件 `stateChanged` / `reconnected` / `push` / `error`）；`GameClient` 在它上面做业务入口（`subscribe`、`request`、`probeClock`，事件 `error` / `stateChanged` / `reconnected` / `unhandledPush`）。只要原始字节时直接用 `Connection`，或 `GameClient` 的 `subscribe` / `request` 自己传解析函数。
- **回调时机**：浏览器是单线程事件循环，帧到了就在 WebSocket 的 `message` 事件里同步分发，不需要 C# 那样的 `Pump`（D6）。同一条 WebSocket 消息里的应答与推送，回调形式（`GameClient.request`、protobuf 的 `request`）严格按线上顺序调用；Promise 形式（`requestAsync`）的后续在这条消息的推送都交出去之后才运行（微任务）。推送回调里 `disconnect()` 后，这条消息剩下的帧丢弃。
- **握手**：第一帧是认证信封 `0x00 'G'` + 8 字节连接代号 + 票据（票据 1～8182 字节 UTF-8，不能以 NUL 开头），连接代号取 `max(上次 + 1, Date.now() × 10⁶)`（bigint），同一个 `Connection` 的每次握手严格递增，Gate 据此丢弃旧连接的迟到握手；握手应答必须是空的成功控制帧。
- **心跳**：缺省每 15s 一次（从上一次应答算起），独占一个槽位、不占请求名额；`heartbeatTimeoutMs`（缺省 5s）内没应答就当断线。不心跳会被 `idle_timeout`（缺省 90s）断开。后台标签页的计时器被浏览器限流或冻结时心跳跟着停，回到前台靠重连（KNOWN-LIMITS）。
- **连接中途断开**：`connect()` 还没完成时 `disconnect()` / `dispose()`（含在同步派发的 `Connecting` 状态监听器里调用），`connect()` 当场以 `GateClosedError` 结束，不等连接超时；已在取票时取票函数收到的 `signal` 被中止，还没取票的不再调用取票函数（调用方的 `signal` 已中止时同样不调用）。C# `Connection` 相同：已取消就不调用取票函数（2026-11-18，RV-20261118-11）。
- **超时与上限**：`connectTimeoutMs`（取票 + 握手，缺省 10s）、`requestTimeoutMs`（缺省 10s）、`maxPendingRequests`（缺省 64，超出直接失败）、`maxPayloadBytes`（缺省 1 MiB，收发两向，与 Gate 的 `max_payload_bytes` 一致）。
- **重连**：`autoReconnect` 开着时，连接意外断开后按 `reconnectInitialDelayMs`（1s）起、翻倍、`reconnectMaxDelayMs`（30s）封顶、乘 0.5～1 的抖动退避，最多 `maxReconnectAttempts`（8）次；每次重新取票、换新的连接代号；每次尝试发一个 `reconnected` 事件（`attempt`、`connectionVersion`、`error`、`willRetry`）。没完成的请求在断开时以 `GateClosedError` 失败，不自动重发。
- **校时**：`probeClockAsync` / `GameClient.probeClock` 发 `RCN1` 探测，得到 `ClockSample`（本地发出 / 收到、服务端收到 / 发出）；`NetworkClock` 与 C# 同一算法（取往返最小的样本，`ready`、`roundTrip`、`uncertainty`）。
- **只走 WebSocket**（D8）：地址必须是 `ws://` / `wss://` 开头的完整 URL；页面来源要在 Gate 的 `allowed_origins` 里（为空时所有浏览器都被拒，[GATEWAY §6](../../docs/framework/GATEWAY.md)）。传输层是 `GateTransport` 接口（`connect` / `send` / `close` / `onData` / `onClose`），缺省 `BrowserWebSocketTransport`；微信、抖音小游戏的传输以后按这个接口加（D7），上层不变。
- **还没有**：自身视图同步（Sync 推送目前交给 `unhandledPush`，第 2 轮）、配置表读取（第 3 轮）、Lockstep（以后，D10）。

## protobuf 子路径

`request(client, 请求号, 应答号, 请求 schema, 请求消息, 应答 schema, received, failed, signal?)`、`requestAsync(connection, …)`、`subscribe(client, 推送号, schema, received)`、`notify(connection, 消息号, schema, 消息)`。schema 是 protobuf-es 生成的 `XxxSchema`；请求消息可以是普通对象（`MessageInitShape`）。64 位整数是 `bigint`（D5），以 int64 为键的 map 键是字符串。导出由服务端工程的 `roost generate` 完成（`client_export.languages` 写 `ts`，见 `roost help client`），游戏工程用 `roost client sync --from <服务端> --lang ts --to <目录>` 拷进来：`protocol_pb.ts`、`dao_sync_pb.ts`、`message_ids.ts`（`MessageIds` 常量对象）、`error_codes.ts`（`ErrorCodes` 常量对象与 `errorCodeName` / `errorCodeMessage`）。

## 开房间（relay 子路径）

lobby 的 HTTP 调用（`LobbyClient`，用 `fetch`）、relay 的 WebSocket 连接（`RelayClient`，含换 host）；Web 端房主辅助（`WebHost`）在 `@roost/client/web`。方案 FEATURE-2026-11-16-lobby-relay §4.6、§4.7；relay 现行契约见 [RELAY](../../docs/framework/RELAY.md)；三份 SDK 的共同说明见 [client/README](../README.md#开房间-sdklobby--relay)。2026-11-18 起包名从 `@roost/relay-client` 改为 `@roost/client`，从 `@roost/client/relay` 导入（不留旧名，D1）。

```ts
import { ALL, LobbyClient, RelayClient } from "@roost/client/relay";

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
const { roomGone } = await client.leave(); // 经 lobby 离开、close；房间已关（lobby 答 room_not_found）也算离开成功，roomGone 为 true
```

### relay：帧、心跳与重连

- **帧**：每条 WebSocket 二进制消息发一帧；收的时候按字节流解（`FrameReader`），一条消息几帧、一帧拆在几条消息里都能读。编码与 [relay-packets.json](../spec/relay-packets.json) 逐字节相同（含换 host 的 13 条）。
- **收帧上限**：`max(maxPayloadBytes, snapshotMaxBytes + 64 KiB)`。`YouAreHost` 可以比 relay 的 `max_payload_bytes` 大（带满快照加成员名单），`snapshotMaxBytes` 要等于 relay 的 `snapshot_max_bytes`（缺省 256 KiB）。
- **心跳**：成员每 `pingIntervalMs`（缺省 10s）发一个 Ping；房主（含等 `HostReady` 的候选）在 `hostPingIntervalMs`（缺省 600ms）内没发过任何帧就补一个 Ping——relay 的 `host_idle_timeout` 缺省 2s。`readTimeoutMs`（缺省 30s）内一帧都没收到就当断线。后台标签页的主线程计时器被限流时，`pulse()` 让外部（Worker 的消息）驱动心跳。
- **重连**：`rejoin()` / `reconnect.maxAttempts` 先调 lobby 的 join 拿新令牌。连接自己断了、relay 判空闲超时、`token_rejected`、`lobby_unavailable`（relay 向 lobby 验权失败）按退避重试；被踢、`lobby_rejected`、房间关闭、被顶号、离开、relay 停机都不重连。被踢的人：lobby 的 join 答 403 `kicked`；拿没过期的旧令牌直连，同一个 relay 上答 `Closed{kicked}`（relay 自己的踢出记录），relay 重启后才是 `Closed{lobby_rejected}`，两种都终止自动重连。
- **只连 WebSocket**：ticket 的地址不是 `ws://` / `wss://` 时 `connect` 报错。

### relay：换 host

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
- **快照属于发起时的任期**：`pushSnapshot` 在取状态（`snapshot.take()`）和压缩之前记下当前连接与 `epoch`，异步结束、分配 seq 之前核对；期间换了连接或房主任期（例如转出去又转回来、断线重连）就返回 `undefined`，不发、不占 seq、不触发 `onSnapshotSent`（与 Go / C# 相同，见 RELAY.md §4A“快照”）。几次推送可以并发（游戏连着调，或 `take` 慢于 `intervalMs` 时定时推送叠上来），以发起顺序为准、后发起的胜出：比它晚发起的一份已经发出时，这一份同样返回 `undefined`、不发、不占 seq。
- **转交属于发起时的任期**：`transfer` 在压缩交接数据之前记下当前连接与 `epoch`，压缩完核对；期间已不是房主、换了连接或房主任期（例如转出去又转回来、断线重连）就返回 `false`，不发出（与 Go / C# 相同）。发出的 `Transfer` 帧带着这个 `epoch`，到达 relay 前任期又变了的，relay 回 `Rejected{StaleEpoch}`（`onRejected`），房间不变（RELAY.md §4A）。
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
source ~/.roost-it/roost-dataengine-it/env.sh      # relay 真实对端的房间目录用 Redis；没有时用内存
./scripts/test-client-ts.sh                        # 仓库根目录：npm ci（需要时）、build、npm test、Go 集成两项
ROOST_TS_FULL=1 ROOST_PROTOC=<protoc 31.1> ./scripts/test-client-ts.sh   # 另跑真实工具链（protoc + protoc-gen-es + tsc）
```

`npm test` 先 `tsc`，再用 node 自带的 test runner 跑：

- gate：RS v2 金样（[packets.json](../spec/packets.json)，与 Go、C# 同一份；认证信封、校时样本）与拒绝规则；对假玩家接入（`ws` 起的 WebSocket 服务）的连接、握手（连接代号递增）、心跳与心跳超时、连接 / 请求超时、请求名额、自动重连（退避、重新取票、`reconnected` 事件）、推送顺序与推送回调里断开、迟到应答丢弃、非空握手应答失败；假计时器下的心跳节奏与超时；protobuf 子路径对 dao 生成器金样 proto 的收发（`test/gen/dao_sync_pb.ts` 由 protoc 31.1 + protoc-gen-es 2.17.0 生成，完整模式核对它没过期）；
- relay：金样（27 条逐字节）与拒绝规则、字节流解码、压缩往返与互通；
- 假 relay 下的换 host（`YouAreHost` → `HostReady` → `HostChanged`、转交、候选、快照间隔与 seq 接续、房主心跳、收帧上限、`lobby_rejected` / `lobby_unavailable` / 403 `kicked` 的重连）；
- Web 辅助（补帧与上限、真实 `MessageChannel` 上的 Worker 代理、假 `document` 下的让出、假 `sessionStorage` 下的刷新恢复）；
- 与 Go 互通：测试临时编一个只用 `compress/flate` 的小程序，TS 压的 Go 解得开、Go 压的 TS 解得开；
- 真实对端：`go test -c ./client/relay` 并以 `ROOST_RELAYPEER_SERVE=1` 起 relay 与 lobby（`ROOST_RELAYPEER_BIN` 指向事先编好的对端时直接用它），经 `ws` 包跑 lobby 错误、房间生命周期、断线重连、心跳，以及主动转交、房主断线后按候选换 host 并交出快照、宽限期内刷新回来仍是房主、空闲房主 5s 内不被换掉、被踢与离开后的旧令牌、最后一人离开（房间已关时离开算成功）、开局后掉线重连回原座与踢离线座位、验权故障注入（`/harness/admission` 的 down 退避重试、reject 停止）。

需要本机 Go 工具链。relay 与 web 子路径的自动用例只在 node 里跑，没有在真实浏览器里跑过；gate 子路径跑过浏览器冒烟（见最后一节）。

Go 集成（`infra/network/gateway`，`ROOST_TS_GATE_DRIVER` 指向 `test/gate-driver.mjs`）：`TestTheTSClientWorksOverWebSocket` 连真实玩家接入，`…ThroughAStandaloneGate` 经独立 Gate；两项都核对请求 / 应答、推送、校时、页面冻结超过 `idle_timeout` 后自动重连且连接代号递增。`test/mutate.mjs`、`test/mutate-integration.sh` 是变异检查（改坏一处、用例必须变红），证据见 [evidence/ts-gate-client-r1-20261118](../../docs/maintenance/evidence/ts-gate-client-r1-20261118/README.md)。

## 浏览器演示

```sh
cd client/ts && npm ci && npm run build
source ~/.roost-it/roost-dataengine-it/env.sh
node demo/serve.mjs            # 起对端（真实 relay + lobby），发静态文件，/lobby 同源代理到对端的 lobby
# 浏览器开 http://127.0.0.1:8787/demo/ ，开两三个标签页
```

第一个页面“建房”，其余页面填房间号“加入”；“点一下”的计数由房主页 Worker 里的规则（`demo/rules.js`）维护并广播。页面上显示规则循环在不在跑、最近一次接手（epoch、来源、快照 seq）、最近一次快照、tick 与它多久前更新、广播者槽位。可以看：手动“转交”、把房主页切到后台 5s（让出给候选；后台页仍报 visible 的环境用“模拟隐藏”按钮）、刷新房主页（5s 内回来仍是房主，计数从 sessionStorage 恢复）、关掉房主页（5s 后成员当上新房主，计数从 relay 存的快照恢复）。

## 浏览器冒烟（gate）

```sh
cd client/ts && npm ci && npm run build
node demo/serve-gate-smoke.mjs   # 起玩家接入对端（infra/network/gateway 的 TestServeAWebSocketPlayerAccessForTheBrowserSmoke），发静态文件
# 浏览器开 http://127.0.0.1:8791/ ，自动跳到 demo/gate-smoke.html?ws=<对端>&hold=<idle_timeout + 10s>
```

页面只做连接、握手、心跳保持超过 `idle_timeout`（缺省 90s，页面保持 100s），日志以 `SMOKE` 开头，最后一行 `SMOKE PASS held 100s on one connection, N heartbeat round trips` 或 `SMOKE FAIL …`。保持期间页面要在前台：后台标签页的计时器被浏览器限流，心跳会停。
