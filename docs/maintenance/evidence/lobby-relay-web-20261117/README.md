# 大厅 / relay 第 3 批 web 组证据（2026-11-17）

TS SDK（`client/ts`）接上换 host，加 Web 端房主辅助（方案 FEATURE-2026-11-16-lobby-relay §4.6、§4.7.2～§4.7.5）。用法与契约见 [client/ts/README](../../../../client/ts/README.md)。

## 做了什么

- `src/codec.ts`：操作码 64～69、80～84，原因码 15 lobby_rejected、16 lobby_unavailable；YouAreHost / HostChanged / epoch / Snapshot / Successors / Rejected 的编解码（与 `client/wire/relay.go` 同一布局）。
- `src/client.ts`：`builtinControl` 加 64～69 的处理与强类型回调（`onYouAreHost` / `onYouAreMember` / `onHostChanging` / `onHostChanged` / `onHostResumed` / `onRejected` / `onSnapshotSent`）；角色 `hostRole`（member / candidate / host）、`epoch`、`hostChanging`、`onlineMembers`；发送 `transfer`、`setSuccessors`、`pushSnapshot`、`hostReady`、`sendLeave`；`autoHostReady`（缺省关，同 Go）；快照 seq `max(本地, YouAreHost seq, epoch<<32) + 1`；定时快照（缺省 3s）与最小间隔（缺省 1s，`SnapshotTooSoonError`）；房主心跳（600ms 内没发过帧就补 Ping）；`pulse()`；收帧上限 `max(maxPayloadBytes, snapshotMaxBytes + 64 KiB)`；重连遇 `lobby_unavailable` 退避、遇 `lobby_rejected` / `kicked` 停。
- `src/compress.ts`：`Compressor` 接口；缺省 raw deflate（`CompressionStream('deflate-raw')`，没有时动态加载 `node:zlib`）；`noCompression`；解压上限 16 MiB。
- `src/lobby.ts`：错误码 `kicked`（403）。
- `src/web.ts`：`HostRules` 规则接口、`FixedStepLoop`（按经过时间补帧、补帧上限）、`LocalRunner` / `WorkerRunner` + `serveRules`（Worker）、`HostSessionStore`（sessionStorage）、`WebHost`（规则 ↔ RelayClient、visibilitychange 让出、刷新恢复）。浏览器 API 都可注入。
- `demo/`：最小演示页（`index.html`、`demo.js`、Worker `rules.worker.js`、规则 `rules.js`）与本地服务 `serve.mjs`（起对端、发静态文件、`/lobby` 同源代理）。

## 测试（`npm-test.txt`）

`cd client/ts && source ~/.roost-it/roost-dataengine-it/env.sh && npm test`：51 个用例全过，对端是 `go test -c ./client/relay`（sdkhost 组 ed1392b2 的 peer，含 relay 修复 dd78962f），Redis 房间目录。

| 文件 | 覆盖 |
|---|---|
| `golden.test.ts` | 金样 27 条（含换 host 13 条）逐字节编码、字段解码、截断与非法来源 |
| `host.test.ts`（假 relay） | YouAreHost → 候选 → HostReady → HostChanged{self}；手动 hostReady；回调抛错不回；转交、候选、Leave 的字节与金样相同；HostChanging / HostResumed / Rejected；收帧上限；房主心跳；定时快照与 seq 连续；最小间隔；seq 规则；踢离线座位的第二条 MemberLeft；重连遇 lobby_unavailable / lobby_rejected / Closed{kicked} / 403 kicked |
| `compress.test.ts` | 往返、解压炸弹（16 MiB 上限）、CompressionStream ↔ zlib 互通、**TS ↔ Go compress/flate 双向互通**（测试临时编一个只用标准库的 Go 小程序）、没有 CompressionStream 时退到 zlib、可替换 |
| `web.test.ts` | 补帧与上限；LocalRunner；真实 `MessageChannel` 上的 Worker 代理；sessionStorage 存档；假 document 下的让出（满 5s、回来取消并重新计时、候选第一个在线、没人在线留下、手机立即）；刷新恢复（宽限期内仍是房主、seq 接续；过了宽限期存档作废；别的账号不用） |
| `relay.test.ts`（真实 relay + lobby） | 原有生命周期 / 重连 / 心跳（踢人后 rejoin 改为 lobby 的 403 kicked；房主断开改为换 host），新增：主动转交（交接数据、HostChanged、转发方向、lobby 跟着换房主）；房主断网 → HostChanging → 宽限期后候选拿到快照、seq 接续不被拒；空闲房主 5.5s 内不被换掉；刷新页面（WebHost + sessionStorage，宽限期内仍是房主、成员收到 HostResumed、状态恢复）；踢人与旧令牌（同 relay 是 Closed{kicked}，离开后的旧令牌是 lobby_rejected）；开局后掉线自动重连回原座、开局后新人 room_started、踢离线座位；验权故障注入 down 退避重试后恢复、reject 停止 |

## 变异（`mutations.txt`）

17 处（含修复的 M17），每处撤一行（或让一个条件失效），受影响的用例全部变红，恢复后全绿：seq 不接 YouAreHost / 不取 epoch<<32 下限、房主心跳用成员间隔、收帧上限不放大、lobby_unavailable 不重试、kicked 也重连、补帧不设上限、回到前台不取消让出、刷新后不从存档恢复、存档不存快照、候选不看在线、不回 HostReady、不限快照最小间隔、当上房主不重置间隔、解压不设上限、解压用压缩流。M2（心跳）同时让真实 relay 的“空闲房主 5s 内不被换掉”变红；M7、M11 同时让真实 relay 的刷新用例变红。

## 修复：主动转交后新房主的规则不跑（协调者在演示页复现）

- **现象**：A 建房、B 加入，A 转交给槽位 2。B 已经是 host、epoch 1，但 tick 停在转交那一刻，之后没有快照，sessionStorage 里也没有存档，刷新后回不来。走快照来源（宽限期后 A 接手）这条路时一切正常。
- **根因**：`RelayClient` 的每个 `onXxx` 只有一个槽位，`WebHost` 原来在 `src/web.ts` 的 `wire()` 里也用 `client.onYouAreHost` / `onYouAreMember` / `onHostChanged` / `onSnapshotSent` / `onGame` / `onReconnected` / `onDisconnected` 注册自己的处理。演示页 `demo.js` 随后在同一个 `webHost.client` 上注册日志回调，把 WebHost 的处理全部顶掉了：
  - B 收到 YouAreHost 时规则没有 `start`；`autoHostReady` 照样回了 HostReady，所以 B 显示为 host。
  - 定时快照的 `take` 看到规则没在跑就不推。
  - 存档本来挂在 `onSnapshotSent` 上，也被顶掉了。
- **为什么快照来源那条路看起来正常**：A 转交后收到 YouAreMember，但它停规则的处理同样被顶掉，A 的规则一直在跑；A 再次接手时规则本来就在跑。A 的存档同样没写。这条路只是看起来正常。
- **修法**：
  - `RelayClient` 加一组内部监听：`[internalHooks]`，键是一个不从 index 导出的 Symbol。内部监听在业务回调之前调用。
  - `WebHost` 改走内部监听，不再占业务的 `onXxx` 槽位。业务在 `webHost.client` 上注册什么回调都不影响 WebHost。
  - 演示页的转交按钮改用 `webHost.handOff(slot)`。这里没有 bug，只是统一走 WebHost。
- **先红**：新用例 `relay.test.ts` 的 “transfer between WebHost pages that also register client callbacks …”。用真实 relay，两页都像演示页一样在 client 上注册日志回调。修前的失败原文：`AssertionError [ERR_ASSERTION]: timed out waiting for B's rules keep stepping after the takeover`。修后通过，并且断言了以下几点：
  - B 的规则从交接数据开始继续 step；
  - A 看到 B 的广播；
  - B 定时推快照，并且存进了 sessionStorage；
  - B 刷新后在宽限期内回来仍是房主，规则从存档恢复后继续 step。
  - `web.test.ts` 另加一条假 relay 的同类用例。
- **变异 M17**：把 WebHost 改回占业务槽位的旧接法，上面两条用例都变红。
- **演示页**：
  - 显示规则循环在不在跑、最近一次接手（epoch、来源、快照 seq、年龄）、最近一次快照、tick 多久前更新、广播者槽位；
  - 加了“模拟隐藏 / 模拟回到前台”两个调试按钮：覆盖 `document.visibilityState`，再派发 `visibilitychange`。

## 没验证的

- 没有在真实浏览器里跑（Worker、`CompressionStream`、`visibilitychange`、`sessionStorage` 都用 node 的等价物或假的）。浏览器演练：`cd client/ts && npm ci && npm run build && source ~/.roost-it/roost-dataengine-it/env.sh && node demo/serve.mjs`，开 `http://127.0.0.1:8787/demo/` 的两三个标签页；本组只用 curl 和 node 冒烟过：页面与 `dist/index.js` 200、经代理拿到测试 gt1、建房拿到 ws 地址。
- 浏览器后台限流对 Worker 计时器的实际表现（方案也说只是缓解）。

## 建议写进 CHANGELOG 的文字（协调者统一写）

- TS SDK（client/ts）接上换 host：解 64～69 成强类型回调、角色状态、`transfer` / `setSuccessors` / `pushSnapshot` / `hostReady` / `sendLeave`，快照 seq 取 `max(本地, YouAreHost seq, epoch<<32)+1`，缺省 raw deflate 压缩（与 Go、C# 互通，解压上限 16 MiB），房主 600ms 心跳，收帧上限放大；被踢记在 lobby（403 `kicked`），重连遇 `lobby_unavailable` 退避、遇 `lobby_rejected` / `kicked` 停。
- 新增 Web 端房主辅助 `WebHost`：规则模块 `HostRules` 可在 Worker 里跑（`WorkerRunner` / `serveRules`），按经过时间补帧（有上限），页面隐藏满 `hiddenHandoffMs` 让出房主，sessionStorage 存快照、刷新后宽限期内回来仍是房主并恢复状态；`client/ts/demo` 有最小演示页。
- 行为变化：`onYouAreHost` 等回调参数从原始帧换成解好的结构；`LobbyErrorCode` 加 `kicked`。

## 建议写进 KNOWN-LIMITS（框架限制）

- Web 房主在后台标签页靠 Worker 撑住只是缓解：浏览器不保证 Worker 计时器不被限流，手机后台页随时被冻结，冻结后走 relay 的断线换 host。
- 前台成员的“我在前台”上报没有专门的控制帧（relay 不转发未知控制帧）；`WebHost` 让出时按房主声明的候选顺序取第一个在线成员，候选顺序由游戏维护（例如成员经游戏通道报前台状态）。
