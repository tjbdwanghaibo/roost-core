# RV-20261119-01～02 修复证据（2026-11-19）

问题来源：审查报告 `MAIN-REVIEW-20261119.md`（审查分支 `fix/core-game-server-review` 上 2026-11-19 的“复核四项修复并记录 Unity 取消与 portal IPv6 问题”一次提交，证据目录 `docs/maintenance/evidence/main-review-20261119/`；该分支未合入 main）。修复基于 main `2d15a1e5`。每项先写回归用例、在修前代码上跑红，再修，修后跑绿，并做一次变异确认用例抓得住。命令都在仓库根目录执行，Go 命令带 `GOWORK=off`，以退出码判断。代码图索引（codebase-memory-mcp）本轮连不上，定位按当前源码与 `rg`。

## RV-20261119-01（P2）Unity 浏览器桥接：调用前已取消仍打开浏览器连接，连接任务挂起

根因：`client/unity/Runtime/BrowserWebSocket.cs` 的 `ConnectAsync` 不看令牌、也不看自己是否已 Dispose，直接 `js.Open` 并加入轮询列表。两个 SDK 都用 `token.Register(transport.Dispose)` 做连接超时 / 取消（`RelayClient.DialAsync`、`TcpSession.ConnectAsync`）；令牌在调用前就已取消时，`Register` 当场执行 `Dispose`——此时还没有句柄，什么都没关，只标了 `disposed`。随后的 `ConnectAsync` 照样打开句柄：之后 `Poll` 见 `disposed` 直接返回、`Dispose` 见 `disposed` 也直接返回，句柄没人关、连接任务永远等不到结果。

Gate 路径的核对：`GameClient` / `Connection.ConnectAsync` 不受影响——`Connection.OpenAsync` 在取票前、建连接前各检查一次令牌（RV-20261118-11 加的），已取消的令牌走不到 `TcpSession.ConnectAsync`，单线程（WebGL）下两次检查之间没有别人能取消。但公开的 `TcpSession.ConnectAsync(IGateTransport, …)` 不经这层检查，交给它已取消的令牌与 `BrowserWebSocket` 时同样挂起（修前红，见下）。

修法（只改桥接，SDK 不变）：
- `ConnectAsync`：令牌已取消时返回已取消的任务、对象已 Dispose 时返回 `ObjectDisposedException`，都不打开句柄；同一实例第二次 `ConnectAsync` 抛 `InvalidOperationException`（一个实例只连一次，与 `IGateTransport` 约定相同，避免旧句柄被覆盖）。
- `Poll`：连接中途令牌取消（调用方没有在取消时 Dispose 的情形）时以取消结束连接任务并 `Dispose`（关句柄）；之后调用方再 Dispose 不会重复关。
- 结果：浏览器句柄只在 `Dispose` 里关，任何路径下 Open 与 Close 配对，连接任务一定结束。
- `IGateTransport` / `IRelayTransport` 的约定注释补一条：Dispose 可能先于 ConnectAsync（令牌调用前已取消），之后的 ConnectAsync 不得再打开连接。自带的 TCP / `ClientWebSocket` 传输在 Dispose 之后连接就抛异常，本来就满足。

回归：`client/dotnet/Roost.Client.Tests/BrowserBridgeTests.cs` 的 `TestBrowserBridgeCancellation`（链接 Unity 包里的正式 `BrowserWebSocket.cs`，用只数 Open / Close、一直停在“连接中”的假浏览器一侧）。七个场景，每个断言连接任务以取消结束、Open 与 Close 次数相等（调用前已取消的场景要求 Open 0 次）；每个场景单独记结果，一次列出全部失败：
1. relay（PumpDriven `RelayClient`），令牌调用前已取消——审查的原场景；
2. relay，`ConnectAsync` 返回后取消——审查的对照组；
3. Gate（PumpDriven `GameClient`），令牌调用前已取消；
4. Gate，连接中取消；
5. 桥接自身，没人 Dispose，连接中取消，之后再 Dispose 不重复关；
6. 公开的 `TcpSession.ConnectAsync`（线程池调度），令牌调用前已取消。

```
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json
```

修前（新用例 + 修前 `BrowserWebSocket.cs`）退出码 134，原文：

```
Unhandled exception. System.Exception: browser bridge cancellation:
  relay, token canceled before ConnectAsync: the connect task did not end within 1s of pumping (status=WaitingForActivation, open=1 close=0)
  bridge alone, canceled while connecting: the connect task did not end within 1s of pumping (status=WaitingForActivation, open=1 close=0)
  TcpSession, token canceled before ConnectAsync: the connect task ends (status=WaitingForActivation, open=1 close=0)
   at Program.Check(Boolean ok, String message) in .../client/dotnet/Roost.Client.Tests/Program.cs:line 13
   at Program.TestBrowserBridgeCancellation() in .../client/dotnet/Roost.Client.Tests/BrowserBridgeTests.cs:line 204
```

与审查一致：原场景 Open 1 次、Close 0 次、任务挂起；relay 的对照组与 Gate 经 `GameClient` 的两个场景修前就通过。“桥接自身按令牌结束”是本轮补的保证（SDK 总会 Dispose，直接用这个类的调用方不一定），不是审查报告的场景。

修后：`PASS browser bridge cancellation (relay / gate / TcpSession / bridge alone): canceled connects end canceled, every Open has a Close`，全部用例退出码 0。

变异：删掉 `ConnectAsync` 开头的两行检查（已取消 → 取消、已 Dispose → `ObjectDisposedException`），保留 `Poll` 的取消处理，退出码 134，原场景与 `TcpSession` 场景变红：

```
Unhandled exception. System.Exception: browser bridge cancellation:
  relay, token canceled before ConnectAsync: the connect task did not end within 1s of pumping (status=WaitingForActivation, open=1 close=0)
  TcpSession, token canceled before ConnectAsync: the connect task ends (status=WaitingForActivation, open=1 close=0)
```

## RV-20261119-02（P3）portal 接受不带方括号的 IPv6 WebSocket 地址

根因：`infra/network/httpserver/browser_host.go` 的 `BrowserHost` 只对以 `[` 开头的主机走 IPv6 解析，其余交给“末段是数字就按 IPv4”的判断；`2001:db8::1` 末段不是数字，被当成 DNS 名原样返回。Go 的 `url.Parse` 对 `ws://2001:db8::1:7080/ws` 不报错（把最后一个冒号之前当主机、`7080` 当端口），所以 `svcaccount.CheckWebSocketAddr`（`portal.websocket_servers`、`login.presence.ws_addr`、account 收上报）放行。浏览器（WHATWG URL）读到第一个冒号就当端口开始，`db8::1:7080` 不是端口，解析失败。

CORS Origin（`CheckCORSOrigins`，`portal.cors_origins`、`lobby.cors_origins`）的核对：用 go1.27 的缺省设置不受影响——Go 的 `url.Parse` 只对 http / https 执行严格冒号检查（`net/url` 的 GODEBUG `urlstrictcolons`），`https://2001:db8::1:8443` 在 `url.Parse` 就报 `invalid port`，被“不是 origin”拒绝；ws / wss 不在严格范围内，所以只有 WebSocket 地址漏过。但这条保护来自标准库的缺省设置：`GODEBUG=urlstrictcolons=0` 时 CORS 同样放行（修前红，见下）。规则放进 `BrowserHost` 之后两条路径都不再依赖它。

修法：`BrowserHost` 对不在方括号里、却含冒号的主机报错，提示把 IPv6 写在方括号里（`[2001:db8::1]:7080`）。合法 IPv4、方括号 IPv6、DNS 名、空端口（`game.example:`）不变。

浏览器期望用 Node 的 WHATWG URL 核对（node v26.7.0，`new URL(s)`）：

```
"ws://2001:db8::1:7080/ws" ERR_INVALID_URL
"ws://2001:db8::1/ws" ERR_INVALID_URL
"ws://::1:7080/ws" ERR_INVALID_URL
"ws://[2001:db8::1]:7080/ws" ok host=[2001:db8::1]:7080
"ws://[::1]/ws" ok host=[::1]
"ws://192.0.2.30:7080/ws" ok host=192.0.2.30:7080
"ws://game.example:7080/ws" ok host=game.example:7080
"ws://game.example:/ws" ok host=game.example
"https://2001:db8::1:8443" ERR_INVALID_URL
"https://2001:db8::1" ERR_INVALID_URL
"https://::1" ERR_INVALID_URL
"https://[2001:db8::1]:8443" ok origin=https://[2001:db8::1]:8443
```

回归：
- `service/account/presence_test.go` 的 `TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse` 加三条：`ws://2001:db8::1:7080/ws`、`wss://2001:db8::1/ws`、`ws://::1:7080/ws` 拒绝且错误提示方括号；原有的合法写法（含方括号 IPv6、DNS 名）照常接受。
- `infra/network/httpserver/cors_test.go`：新 `TestBrowserHostRefusesIPv6WithoutBrackets` 直接构造 `url.URL`（不经 `url.Parse`，不依赖 GODEBUG），ws 与 https 两种 scheme 下不带方括号的 IPv6 都拒绝，合法主机返回值不变；`TestCheckCORSOriginsAcceptsOnlyWhatTheBrowserSerializes` 的拒绝表加 `https://2001:db8::1:8443`、`https://2001:db8::1`、`http://::1:5173`。

```
GOWORK=off go test -count=1 -run 'TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse|TestCheckCORSOrigins' ./service/account ./infra/network/httpserver
GOWORK=off go test -count=1 -run 'TestBrowserHost|TestCheckCORSOrigins' ./infra/network/httpserver
GODEBUG=urlstrictcolons=0 GOWORK=off go test -count=1 -run 'TestCheckCORSOrigins' ./infra/network/httpserver
```

修前，WebSocket 地址（退出码 1）：

```
--- FAIL: TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse (0.00s)
    presence_test.go:173: "ws://2001:db8::1:7080/ws": err = <nil>; want refused, telling to put the IPv6 address in brackets
    presence_test.go:173: "wss://2001:db8::1/ws": err = <nil>; want refused, telling to put the IPv6 address in brackets
    presence_test.go:173: "ws://::1:7080/ws": err = <nil>; want refused, telling to put the IPv6 address in brackets
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/service/account	0.578s
ok  	github.com/tjbdwanghaibo/roost-core/infra/network/httpserver	1.066s
```

（同一次运行里 CORS 的新拒绝项修前就通过：缺省设置下 `url.Parse` 已拒绝，见上文。）

修前，`BrowserHost` 本身（退出码 1）：

```
--- FAIL: TestBrowserHostRefusesIPv6WithoutBrackets (0.00s)
    cors_test.go:196: ws://2001:db8::1:7080: got "2001:db8::1", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: https://2001:db8::1:7080: got "2001:db8::1", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: ws://2001:db8::1: got "2001:db8:", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: https://2001:db8::1: got "2001:db8:", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: ws://::1:5173: got "::1", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: https://::1:5173: got "::1", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: ws://fe80::1: got "fe80:", <nil>; want refused, telling to put the IPv6 address in brackets
    cors_test.go:196: https://fe80::1: got "fe80:", <nil>; want refused, telling to put the IPv6 address in brackets
```

修前，CORS 在 `GODEBUG=urlstrictcolons=0` 下（退出码 1）：

```
--- FAIL: TestCheckCORSOriginsAcceptsOnlyWhatTheBrowserSerializes (0.00s)
    cors_test.go:169: "https://2001:db8::1:8443": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:169: "https://2001:db8::1": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:169: "http://::1:5173": err = <nil>, want refused naming the key (a browser cannot parse it)
```

修后：上面三条命令都是 `ok`（退出码 0），缺省设置与 `GODEBUG=urlstrictcolons=0` 下都通过。

变异：把检查改成 `strings.HasPrefix(host, ":")`（只拦以冒号开头的主机），退出码 1，`TestBrowserHostRefusesIPv6WithoutBrackets` 的 `2001:db8::1…`、`fe80::1` 各项与 `TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse` 的 `ws://2001:db8::1:7080/ws`、`wss://2001:db8::1/ws` 变红（`::1:5173` 一项被弱化的检查拦住，照常通过）。

## 验证

修后在本机（macOS，go1.27.3、.NET 10.0.302、node v26.7.0）跑，均看退出码：

| 命令 | 结果 |
|---|---|
| `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` | 0（含新用例与原有桥接、Gate、relay 用例） |
| `bash scripts/test-client-dotnet.sh` | 0（金样与自测、地形、跨 Gate 放弃握手、C# 经 WebSocket 连嵌入 TCP 与独立 Gate、jslib 分帧、relay 对 Go relay 924 项、生成工程 TCP） |
| `GOWORK=off go vet ./infra/network/httpserver ./service/account` | 0 |
| `GOWORK=off go test -count=1 -race ./infra/network/httpserver ./service/account` | 0 |
| `GODEBUG=urlstrictcolons=0 GOWORK=off go test -count=1 -run 'TestBrowserHost\|TestCheckCORSOrigins\|TestCheckWebSocketAddr' ./infra/network/httpserver ./service/account` | 0 |
| `GOWORK=off go test -count=1 ./wiring/lobby ./wiring/account`（`CheckCORSOrigins` 的调用方） | 0 |
| `GOWORK=off go test -count=1 .`（根门禁） | 0 |
| `GOWORK=off go test -count=1 -run 'TestModulePortal\|TestModuleLogin\|WebSocket' ./codegen/internal/roost` | 1：`TestModulePortalInstallsOnTheHostedAccountService` 与 `TestModulePortalInstallsOnALobbyProjectWithoutGame` 在“装完再 `roost project sync` 不改工程”一步失败；在修前基线 main `2d15a1e5` 的干净检出上同样两条、同样位置失败，与本轮改动无关。前一条在这一步之前的生成工程模块场景（portal 21、login 35 等计数）通过 |

## 没有验证的部分

- Unity Editor、IL2CPP、真实浏览器里没有重跑：桥接的 C# 一侧用 .NET 链接正式源码 + 假的浏览器一侧验证，jslib 本身没改。`Task.FromCanceled`、`TaskCompletionSource.TrySetCanceled(CancellationToken)` 都在 .NET Standard 2.1 里，Unity 托管包没有重新打包编译。
- 生成工程的真实进程（account 带 `portal.websocket_servers` 启动、`--check-config` 退出码）没有重跑；规则只在 `CheckWebSocketAddr` / `BrowserHost` 一处，生成的 portal / login 代码直接调用它。
- 已经写了不带方括号 IPv6 的工程，升级后启动与 `--check-config` 会报错并提示写成 `[…]`；这类地址浏览器本来就连不上。
