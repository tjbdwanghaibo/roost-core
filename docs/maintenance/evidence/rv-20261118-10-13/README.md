# RV-20261118-10～13 修复证据（2026-11-18）

问题来源：审查报告 `V156-MAIN-REVIEW-20261118.md`（审查分支 `fix/core-game-server-review` 上 2026-11-18 的“复验 v1.56.0 五项修复”一次提交，证据目录 `docs/maintenance/evidence/v156-main-review-20261118/`）。修复基于 main `bf68b3b8`。每项先写回归用例、在修前代码上跑红，再修，修后跑绿，并做一次变异确认用例抓得住。命令都在仓库根目录、`GOWORK=off` 下执行，以退出码判断。

## RV-20261118-10（P3）CORS Origin 的前导零端口

根因：`infra/network/httpserver/cors.go` 的 `CheckCORSOrigins` 只比较 `scheme://Host` 是否与原文相同、只拒绝字面的 `:443` / `:80`；`:0443`、`:08443`、空端口 `…:`、非规范数字主机（`127.1`、`010.0.0.1`、展开写法的 IPv6）都能通过，而浏览器按 WHATWG URL 序列化 Origin（去掉前导零与默认端口、把数字主机改写成规范形式），白名单逐字节比较时对不上，预检 403。

修法：按浏览器的序列化重新拼出 Origin（主机走新的 `httpserver.BrowserHost`，端口转成十进制整数、限 1～65535、默认端口省掉），与原文不同就拒绝并给出该写的值；浏览器解析不了的主机（`192.0.2.300`）拒绝。`portal.cors_origins` 与 `lobby.cors_origins` 都调它。

回归：`infra/network/httpserver/cors_test.go` 的 `TestCheckCORSOriginsAcceptsOnlyWhatTheBrowserSerializes`。断言是“通过检查的写法，用浏览器实际会发的 Origin 预检必须 204”，浏览器写法取自 Node 的 `new URL(...).origin`（WHATWG 实现，见下方核对）。

```
GOWORK=off go test -count=1 -run 'TestCheckCORSOrigins' ./infra/network/httpserver
```

修前（新用例 + 修前 `cors.go`）退出码 1，原文：

```
--- FAIL: TestCheckCORSOriginsAcceptsOnlyWhatTheBrowserSerializes (0.00s)
    cors_test.go:152: "https://[::1]:08443" passed the config check but the browser sends Origin "https://[::1]:8443" and the preflight answers 403
    cors_test.go:152: "http://127.1:5173" passed the config check but the browser sends Origin "http://127.0.0.1:5173" and the preflight answers 403
    cors_test.go:152: "https://0x7f.0.0.1" passed the config check but the browser sends Origin "https://127.0.0.1" and the preflight answers 403
    cors_test.go:152: "https://[2001:db8:0:0:0:0:0:1]" passed the config check but the browser sends Origin "https://[2001:db8::1]" and the preflight answers 403
    cors_test.go:152: "https://play.example.com:0443" passed the config check but the browser sends Origin "https://play.example.com" and the preflight answers 403
    cors_test.go:152: "https://play.example.com:08443" passed the config check but the browser sends Origin "https://play.example.com:8443" and the preflight answers 403
    cors_test.go:152: "http://localhost:080" passed the config check but the browser sends Origin "http://localhost" and the preflight answers 403
    cors_test.go:152: "https://play.example.com:" passed the config check but the browser sends Origin "https://play.example.com" and the preflight answers 403
    cors_test.go:152: "https://010.0.0.1" passed the config check but the browser sends Origin "https://8.0.0.1" and the preflight answers 403
    cors_test.go:152: "https://[::ffff:192.0.2.30]" passed the config check but the browser sends Origin "https://[::ffff:c000:21e]" and the preflight answers 403
    cors_test.go:167: "https://192.0.2.300": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:167: "https://192.0.2.30.1.1": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:167: "https://1.2.3.4.example.1": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:167: "https://play.example.com:65536": err = <nil>, want refused naming the key (a browser cannot parse it)
    cors_test.go:172: "https://play.example.com:0" passed; port 0 serves no page
FAIL
```

修后：`ok  github.com/tjbdwanghaibo/roost-core/infra/network/httpserver`（退出码 0），原有的 `TestCheckCORSOriginsRefusesWhatABrowserNeverSends`（含 RV-20261118-08 的默认端口）与 `TestCORSAllowsOnlyListedOriginsThroughTheEngine` 不变、照常通过。

变异：把 `serialized += ":" + strconv.Itoa(number)` 改回 `serialized += ":" + port`（端口按原文拼），用例变红：

```
    cors_test.go:152: "https://[::1]:08443" passed the config check but the browser sends Origin "https://[::1]:8443" and the preflight answers 403
    cors_test.go:152: "https://play.example.com:08443" passed the config check but the browser sends Origin "https://play.example.com:8443" and the preflight answers 403
```

## RV-20261118-11（P2）TS Gate 在 Connecting 监听器里断开仍取票到超时

根因：`client/ts/src/gate/connection.ts` 的 `connect()` 先同步 `setState(Connecting)`，监听器里 `disconnect()` 已经中止了这次的生命周期 `AbortController`；随后 `open()` 才给它挂 `abort` 监听（`addEventListener`），中止事件已经发过、收不到，预算信号不中止，于是仍调用取票函数、传入未中止的信号，`connect()` 等到 `connectTimeoutMs` 以 `GateTimeoutError` 结束。调用方的 `signal` 已经先看了 `aborted`，生命周期信号漏了同样的检查。

修法：`open()` 里生命周期信号已中止就直接中止预算（与调用方信号相同的写法）；调用取票函数前预算已中止就抛出中止原因（`GateClosedError`），不再取票。C# `Connection`：状态变化只在 `Pump` 里派发，没有同步回调，不会出现“等到超时”；共同的部分是“已取消仍调用取票函数”（别的线程在 `ConnectAsync` 放锁之后 `Disconnect`，或调用方传入已取消的令牌），对齐为 `OpenAsync` 调用取票函数前 `ThrowIfCancellationRequested`。

回归：
- TS：`client/ts/test/gate-timers.test.ts` 的 `disconnect while connecting ends connect at once with GateClosedError, even from the Connecting listener`。两种断开时机（connect 返回 Promise 之后、Connecting 监听器里）都断言：不推进虚拟时间就以 `GateClosedError` 结束、状态 Disconnected、没有遗留计时器；监听器里断开时取票函数调用 0 次，另一种时机取票函数收到的信号被中止。
- C#：`client/dotnet/Roost.Client.Tests/DisposalTests.cs` 的 `TestGameClientDisposal` 末尾一段：传入已取消的令牌，`ConnectAsync` 当场以取消结束、取票函数调用 0 次、回到 Disconnected。

```
cd client/ts && npx tsc -p tsconfig.test.json && node --test --test-name-pattern="disconnect while connecting" dist-test/test/gate-timers.test.js
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json
```

修前 TS（退出码 1）：

```
✖ disconnect while connecting ends connect at once with GateClosedError, even from the Connecting listener (3.00825ms)
  AssertionError [ERR_ASSERTION]: from the Connecting listener: connect ended with still pending at 0ms, want GateClosedError at once
```

（同一用例里“connect 返回之后断开”的对照一支在修前就通过，与审查的对照组一致。）

修前 C#（退出码 134）：

```
Unhandled exception. System.Exception: the ticket provider must not be called after cancellation (called 1 times)
   at Program.Check(Boolean ok, String message) in .../client/dotnet/Roost.Client.Tests/Program.cs:line 13
   at Program.TestGameClientDisposal() in .../client/dotnet/Roost.Client.Tests/DisposalTests.cs:line 77
```

（C# 修前 `ConnectAsync` 已经当场结束，`opening.IsCompleted` 的断言通过，只有“仍调用取票函数”这一条红。）

修后：TS gate 用例 `ℹ pass 29 ℹ fail 0`；`scripts/test-client-ts.sh` 退出码 0（`ℹ tests 103 ℹ pass 103 ℹ fail 0`，Go 侧 `ok infra/network/gateway`）；C# `PASS GameClient state/reset/failure disposal boundaries`，`scripts/test-client-dotnet.sh` 退出码 0。

变异（TS，各自单独）：
1. 去掉“生命周期已中止就中止预算”：红在 `connect ended with still pending at 0ms, want GateClosedError at once`。
2. 去掉“取票前预算已中止就抛出”：红在 `the ticket provider is not called after disconnect (called with aborted=true)`。

C# 的变异就是去掉新加的 `ThrowIfCancellationRequested` 一行，即上面的修前红。

## RV-20261118-12（P3）pretag 范围漏选 SDK 验收脚本自身

根因：`scripts/pretag-scope.py` 的 SDK 触发前缀只有 SDK 目录与共同对端，没有 `scripts/test-client-ts.sh` / `scripts/test-client-dotnet.sh` 本身；只改其中一个脚本时对应门 `run_ts` / `run_dotnet` 为 0。

修法：两个验收脚本各自列为自己 SDK 门的触发项。新增自检 `scripts/pretag_scope_test.py`：把脚本里的 `run`（git / go 调用）换成按参数作答的假函数、在空临时目录里跑 `main()`，检查“只改某一类文件”时选中什么；根包 `TestPretagScopeSelfTest`（`ci_paths_test.go`）运行它。

```
python3 scripts/pretag_scope_test.py
GOWORK=off go test -count=1 -run '^TestPretagScopeSelfTest$' .
```

修前（新自检 + 修前 `pretag-scope.py`）退出码 1：

```
FAIL ['scripts/test-client-ts.sh']: run_ts='0', want '1' (all: {'scope': 'affected', 'go_packages': "('example.test/scope')", 'genre_scripts': '()', 'run_dotnet': '0', 'run_ts': '0'})
FAIL ['scripts/test-client-dotnet.sh']: run_dotnet='0', want '1' (all: {'scope': 'affected', 'go_packages': "('example.test/scope')", 'genre_scripts': '()', 'run_dotnet': '0', 'run_ts': '0'})
```

修后：`PASS pretag-scope selection`（退出码 0）；根包 `--- PASS: TestPretagScopeSelfTest (0.06s)`。对照用例（SDK 源码触发、只改文档不触发、`client/spec/` 两边都触发、改 `pretag-scope.py` 本身整跑）修前修后都通过。

变异：只去掉 dotnet 那一条触发，自检红在 `['scripts/test-client-dotnet.sh']: run_dotnet='0', want '1'`。

## RV-20261118-13（P3）WebSocket 地址接受越界 IPv4

根因：`service/account/presence.go` 的 `CheckWebSocketAddr` 用 Go 的 `url.Parse`，它不解释主机，`ws://192.0.2.300:7080/ws` 有主机、端口合法即通过；浏览器（Unity WebGL、TS SDK 缺省传输）按 WHATWG URL 解析，末段是数字的主机一律按 IPv4 解析，越界即解析失败、无法建连。`portal.websocket_servers`（模板 `parseWebSocketServers`）、`login.presence.ws_addr`（模板 `login_presence.go.tmpl` 的 `ValidateConfig` 与推导地址）、account 收上报都调这一个函数，所以只改它。

修法：新增 `httpserver.BrowserHost`（WHATWG 的“末段是数字”判断 + IPv4 解析 + 方括号 IPv6 检查与序列化），`CheckWebSocketAddr` 调它：浏览器解析不了的拒绝；浏览器会改写的 IPv4 写法（`127.1`、`010.0.0.1` 在浏览器里是八进制、`0x7f.0.0.1`、末尾带点）拒绝并给出点分十进制——C# / 原生客户端未必按同样的规则读它们。方括号 IPv6 只要求合法、不带区域号（展开写法各家读法相同，不强求压缩）；DNS 名不限。

回归：`service/account/presence_test.go` 的 `TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse`。

```
GOWORK=off go test -count=1 -run 'TestCheckWebSocketAddr' ./service/account
```

修前（新用例 + 修前 `presence.go`）退出码 1：

```
--- FAIL: TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse (0.00s)
    presence_test.go:167: "ws://192.0.2.300:7080/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://192.0.2.30.1.1/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://256.0.0.1/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://09.1.1.1/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://game.1:7080/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://game.0x1f/ws" passed; a browser cannot parse it
    presence_test.go:167: "ws://[fe80::1%25en0]:7080/ws" passed; a browser cannot parse it
    presence_test.go:178: "ws://010.0.0.1/ws": err = <nil>, want refused suggesting "8.0.0.1" (what browsers rewrite it to)
    presence_test.go:178: "ws://0x7f.0.0.1/ws": err = <nil>, want refused suggesting "127.0.0.1" (what browsers rewrite it to)
    presence_test.go:178: "ws://192.0.2.30.:7080/ws": err = <nil>, want refused suggesting "192.0.2.30" (what browsers rewrite it to)
    presence_test.go:178: "ws://127.1:7080/ws": err = <nil>, want refused suggesting "127.0.0.1" (what browsers rewrite it to)
FAIL
```

修后：`ok  github.com/tjbdwanghaibo/roost-core/service/account`（退出码 0）；合法写法（`192.0.2.30`、`0.0.0.0`、`[::1]`、`[2001:db8::1]` 与其展开写法、DNS 名、`1e3.example`、`2048.example.com`、末尾带点的 DNS 名）原样通过。

变异：把 `BrowserHost` 的错误丢掉（`browserHost, _ := …`），用例红在 `"ws://[fe80::1%25en0]:7080/ws" passed; a browser cannot parse it`（越界 IPv4 仍被第二道“写法与浏览器不同”拦下，所以只红 IPv6 一条；两道都去掉即修前状态，全红）。

## 浏览器写法的核对

用例里“浏览器会发 / 会解析成什么”取自 Node v26.7.0 的 WHATWG `URL`（与浏览器同一标准）：

```
node -e 'for (const u of [...]) { try { const x = new URL(u); console.log(u, "=>", x.origin) } catch { console.log(u, "=> INVALID") } }'
```

结果摘录：`https://play.example.com:0443 => https://play.example.com`、`https://play.example.com:08443 => https://play.example.com:8443`、`http://127.1:5173 => http://127.0.0.1:5173`、`https://010.0.0.1 => https://8.0.0.1`、`https://[::ffff:192.0.2.30] => https://[::ffff:c000:21e]`、`https://play.example.com:0 => https://play.example.com:0`、`ws://192.0.2.300:7080/ws`、`ws://game.1:7080/ws`、`ws://09.1.1.1/ws`、`ws://[zz]/ws`、`https://[1.2.3.4]`、`https://[::1%25en0]:8443` 均为 INVALID；`ws://1e3.example/ws`、`ws://example.1e3/ws` 合法（末段不是数字）。

## 汇总验证

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go build ./...` | 退出码 0 |
| `GOWORK=off go vet`（httpserver、webroute、ops、service/account、service/lobby、wiring 下依赖它们的包） | 退出码 0 |
| `GOWORK=off go test -count=1` 同上 13 个包 | 退出码 0 |
| `scripts/test-client-ts.sh` | 退出码 0（103 / 103，gateway ok） |
| `scripts/test-client-dotnet.sh` | 退出码 0（57 条 PASS，含新加的取消后不取票；gateway 两项 ok，生成工程上的 C# 客户端 `codegen/internal/roost` ok） |
| `GOWORK=off go test -count=1 .` | 退出码 0（含新加的 `TestPretagScopeSelfTest`） |

没有跑：模块门禁 `scripts/test-modules-generated.sh` 与 `codegen/internal/roost`（portal / login 模板与场景数没改；生成工程经 core 调同一个检查函数，模板测试用的地址都是合法写法，已逐个核对）；全仓 `go test ./...`；真实浏览器（只用 Node 的 WHATWG 实现核对写法）。
