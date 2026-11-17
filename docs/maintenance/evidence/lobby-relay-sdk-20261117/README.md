# 大厅 / relay：客户端 SDK 第 1 批（sdk 组，2026-11-17）

方案 [FEATURE-2026-11-16-lobby-relay](../../FEATURE-2026-11-16-lobby-relay.md) §4.5“客户端 SDK”与 §4.7.5 的基础部分：三份 SDK（Go、C#、TS）的 lobby HTTP 调用、relay 连接、心跳与断线重连、控制帧扩展点。换 host 的控制帧不在这一批。

## 交付

| SDK | 位置 | 接入 relay |
|---|---|---|
| Go | `client/relay`（`Lobby`、`Client`；帧编解码直接用 `client/wire`） | TCP |
| C# | `client/dotnet/Roost.Client.Relay`（netstandard2.1、Nullable、TreatWarningsAsErrors，无第三方依赖）+ `Roost.Client.Relay.Tests` | TCP |
| TS | `client/ts`（`@roost/relay-client`，运行时无依赖；devDependencies 锁确切版本：typescript 5.9.3、ws 8.23.0、@types/ws 8.18.1、@types/node 24.19.1） | WebSocket，每条二进制消息一帧，收的时候按字节流解 |

文档：`client/README.md` 新增“开房间 SDK（lobby + relay）”一节；`client/relay/README.md`、`client/dotnet/Roost.Client.Relay/README.md`、`client/ts/README.md`。

## 测试对端

`client/relay/peer_test.go`：在测试进程里起真实的 `service/relay` 与 `service/lobby`。Go 的用例直接调；C# 与 TS 的用例 `go test -c ./client/relay`，以 `ROOST_RELAYPEER_SERVE=1` 把测试二进制当对端进程（stdout 一行地址 JSON，stdin 关闭即退出，不留孤儿进程）。

- 房间目录：真实 Redis（`~/.roost-it`，127.0.0.1:17379），键前缀每次唯一，退出时删掉。下面三份输出都是 `room directory redis`。
- 替换的只有三处外部依赖：account 的 `ValidateGameToken` → 对端签的测试 gt1（`POST /harness/game-token`，经 lobby 的 `GameTokenValidator` 接口）；relay → lobby 的 NATS 报告 → 进程内按序投递（仍经 `HandleRelayEventData` 解 JSON）；lobby → relay 的 leave 命令 → 直接调 `relay.Server.Leave`；etcd 的 relay 广播 → 固定一条。
- 放在 `_test.go`：对端要链接 Redis 驱动，非装配层只有测试能链接驱动（根包 `TestCoreContractsDoNotLinkDrivers`）。

## 用例

三份都覆盖：建房（带密码、属性、人数上限）、列房（房主连上 relay 之后才列出）、加入、游戏通道往返（成员只到房主、房主按槽位 / 全体，游戏位原样带过）、房间通道直达某个槽位与全体、禁言（房间帧被丢、游戏帧照常，用同连接的顺序做负断言）、踢人（被踢者 `Closed{kicked}` 且不重连、房主 `MemberLeft{kicked}`、被踢账号再进被 relay 拒绝）、经 lobby 离开（`MemberLeft{left}`）、只有房主能改状态、开始后新人进不来、lobby 错误映射（unauthorized / game_not_served / room_not_found / bad_request / room_full / wrong_password / not_host / room_started / no_relay）、伪造令牌 `Closed{token_rejected}`、断线重连（本地断网 → `Disconnected` 且 reconnecting → 恰好一次 lobby join 拿新令牌 → 同一槽位回来 → 双向收发）、房主断开关房后成员不重连、心跳测到 RTT、不认识的控制帧操作码（64）丢弃或交给 `OnControl`、连接不断。Go 另有 relay `idle_timeout` 很短时发心跳的连接活着、不发心跳的被 `idle_timeout` 断开并重连回来。

金样：三份 SDK 对 `client/spec/relay-packets.json` 的每一条编出的字节相同（Go 经 SDK 的 `encodeFrame` / `encodeToken`，C# `RelayCodec`，TS `encodeFrame` / `encodeToken`），relay 发来的控制帧解出预期字段；C# 与 TS 另核对与 Go `ValidateRelay` 相同的拒绝规则，TS 核对字节流解码不依赖消息边界。金样以后追加（换 host）不会让这些用例变红：只要求本 SDK 认识的条目都在。

## 命令与结果（HEAD 3cbaf0e5 + 本组文件，在干净工作树里跑；主检出当时 `service/relay` 有 host 组未完成的改动编不过）

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
go test -race -count=1 -v ./client/relay/                                   # go-test.txt：6 个用例全过
(cd client/dotnet && dotnet run --project Roost.Client.Relay.Tests -- <root>)  # cs-test.txt：125 checks passed
(cd client/ts && npm ci && npm test)                                         # ts-test.txt：9 pass 0 fail
GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .                         # root-link-doc.txt：ok
dotnet build-server shutdown
```

`go test -race -count=20 ./client/relay/` 也全过（31s）。开发中有一次 `./client/relay/...` 输出 FAIL 但被 `tail` 截掉了原因，之后 `-count=8`、6 次单跑、`-count=20` 都没再出现；当时 relay 组正在改 `service/relay`，原因尚未确认。

## 变异（撤掉关键的一行，用例变红，再恢复）

| 变异 | 结果 | 文件 |
|---|---|---|
| Go：数据帧不带游戏位 | 金样、生命周期红 | go-mutations.txt |
| Go：房间帧写成游戏通道 | 金样、生命周期红 | 同上 |
| Go：心跳不发 Ping | 心跳用例红（被 idle_timeout 断开） | 同上 |
| Go：断线（Disconnected）不重连 | 重连用例红 | 同上 |
| Go：重连不经 lobby 拿新令牌 | 重连用例红 | 同上 |
| Go：`LobbyError.Is` 不比错误名 | lobby 错误用例红 | 同上 |
| Go：不认识的操作码断开连接 | 不认识操作码用例红 | 同上 |
| C#：通道位移位错 / Welcome 角色反了 / 丢游戏位 / 断线不重连 / 不发 Ping | 全红（金样、welcome 载荷、重连、RTT） | cs-mutations.txt |
| TS：通道位移位错 / Welcome 角色反了 / 字节流不等满帧 / 断线不重连 / 不发 Ping / 重连不经 lobby | 全红 | ts-mutations.txt |

C# 的变异是在测试对端搬进 `peer_test.go` 之前跑的（当时对端是 `client/relay/internal/cmd/relaypeer`，代码相同、只是位置不同）；SDK 被变异的行没有变。

## 拿不准、留给下一批的

- 换 host 的回调位置 `OnYouAreHost` / `OnHostChanging` / `OnHostChanged` / `OnHostResumed` 这一批只是登记口，不触发；参数暂时是原始帧，host 组定编码表后换成解好的结构。
- 维护者 bba60f19 定了“被踢记录在 lobby，relay 每次连接向 lobby 验权”：实施后被踢账号重新 join 可能在 lobby 就被拒，三份用例里“被踢账号再进被 relay 拒绝（Closed{kicked}）”那一步要跟着改成断言 lobby 的错误。
- TS SDK 只做了 §4.7.5 的 lobby 调用、WebSocket 连接与帧编解码；快照推送与压缩、Worker 规则循环、补帧、`visibilitychange` 让出房主、`sessionStorage` 存快照依赖换 host，下一批做。没有在真实浏览器、Unity Editor / IL2CPP 里跑过。
- 自动重连缺省关闭（与 Roost.Client 的 AutoReconnect 一致）；只在连接自己断掉或 `idle_timeout` 时重连，`token_rejected` 会换新令牌再试一次。

## 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（由协调者统一写）

- CHANGELOG：新增开房间场景的三份客户端 SDK——Go `client/relay`（TCP）、C# `Roost.Client.Relay`（TCP，netstandard2.1，无第三方依赖）、TS `client/ts`（浏览器 WebSocket）：lobby 的建房 / 列房 / 加入 / 离开 / 改状态与错误映射，relay 的令牌首帧、游戏 / 房间通道、成员上下线、踢人禁言、心跳测 RTT、断线后经 lobby join 重新要令牌的重连；三份对 `client/spec/relay-packets.json` 逐字节一致。
- KNOWN-LIMITS（框架限制）：TS SDK 没有在真实浏览器里跑过、C# SDK 没有在 Unity 里跑过；换 host 的回调这一批只登记不触发。
