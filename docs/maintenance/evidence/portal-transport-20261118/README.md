# 登录入口按连接方式给地址（2026-11-18）

维护者 2026-11-18 决定：玩家接入同时支持 TCP 与 WebSocket 之后，**客户端在请求里说明要用哪种连接方式，portal 只返回该方式的地址**；portal 仍然只是 HTTP 服务。基线 main `026c766d`（v1.56.0 之后，含 modules-async 一轮）。接口与配置的现行说明在 [portal 模块](../../../../modules/portal/README.md)、[login 模块](../../../../modules/login/README.md) 与 [GATEWAY §6](../../../framework/GATEWAY.md#6-websocket-接入)。本轮改了 core 的 `service/account`（区服入口登记多一个可选字段与它的检查函数）、`modules/portal`、`modules/login` 的区服入口上报，以及描述它们的现行文档；没有改 `client/`、`codegen/` 的生成逻辑（只改了 portal / login 的场景数断言）、`demo/`、skill、CHANGELOG 与 KNOWN-LIMITS。

## 1. 接口

- `/portal/servers`、`/portal/enter`、`/portal/login` 的 `enter` 多一个可选的 `transport`：`"tcp"`（缺省）或 `"ws"`，只认这两个小写写法，别的答 `portal_request_invalid`（不去问 account）。
- 响应字段不变，`addr` 就是所请求方式的地址（TCP 是 `host:port`，WebSocket 是完整的 `ws://` / `wss://` URL）；另加回显 `transport`（清单顶层与进服结果里）与清单每个服的 `transports`（这个服在目录里有地址的方式，`["tcp","ws"]` 的子集，没有时 `[]`）。
- 推荐只在支持所请求方式的服里挑：`ws` 时入口只把有 WebSocket 地址的服交给工程的 `recommend`，工程规则推荐了别的服按没有推荐处理。`tcp` 与以前完全相同（没有地址的服也交给规则）。
- 指定的服（它的承载服）没有所请求方式的地址：`ws` 时答新码 `portal_transport_unavailable`（400，不给票据，不回落到 TCP）；`tcp` 时与以前一样票据照发、`addr` 为空。
- 登记：静态的 `portal.websocket_servers`（account 配置，服号 → URL，在 account 进程的配置声明里，`--check-config` / `roost config check` 查写法）；动态的 `ServerPresence.WSAddr`（JSON `ws_addr`，可省略；不带它的旧条目等同只支持 TCP），game 一侧是 login 模块的 `login.presence.ws_addr`，没写而本进程开着嵌入的 WebSocket 监听时取 `ws://<login.presence.addr 的主机>:<websocket.addr 的端口><websocket.path，缺省 /ws>`。`Overlay` 按连接方式分开叠。URL 规则只在 `svcaccount.CheckWebSocketAddr` 一处：小写 ws / wss、有主机、写了端口时 1～65535、不带用户名密码与片段、不含空白、至多 255 字节。

## 2. 先红后绿

在基线上先加错误码声明（`portal_transport_unavailable`，只是声明，没有行为）与三条只经 HTTP / 配置的场景，用本检出的 `roost` 生成 `-template game` 工程、`go mod edit -replace` 指向本检出、`roost module add login portal enter servertime`，跑 `go test -count=1 -run TestPortal ./services/account/portal/`：

```
--- FAIL: TestPortalAnswersWithTheAddressOfTheRequestedTransport (0.00s)
    portal_test.go:2064: transport tcp: transport "", recommended 1001; want tcp and 1001 (the newest open server with an address, as before): {"ok":true,"recommended_server_id":1001,"servers":[{"server_id":1000,"name":"s1000","status":"open","addr":"203.0.113.7:7000","addr_source":"directory","load":0,"role":null},…
--- FAIL: TestPortalLoginEntersOverTheRequestedTransport (0.00s)
    portal_test.go:2187: bob, recommended, no transport: 200 {"ok":true,…,"enter":{"ticket":"enter:et1.…","server_id":1001,"serving_server_id":1001,"expires_at":1800001800,"addr":"203.0.113.8:7000","addr_source":"directory"}}
        ; want server 1001 at 203.0.113.8:7000 over tcp
--- FAIL: TestPortalChecksWebSocketAddressesAtStart (0.00s)
    portal_test.go:2241: a user and password: err = <nil>, want it to name portal.websocket_servers.1000
FAIL	example.com/red/services/account/portal	0.518s
```

前两条最先落在回显 `transport` 上（基线没有这个字段），具体承诺（ws 地址、推荐过滤、新码、不回落）由 §3 的变异逐条证明。login 的上报场景同样先红（`go test -run TestLoginPresence ./game/managers/`）：

```
--- FAIL: TestLoginPresenceReportsTheWebSocketAddress (0.02s)
    login_presence_test.go:229: explicit: reported {ServerID:1000 Addr:gate.example.test:7000 WSAddr: Load:0 Tags:[]}, want the WebSocket address "wss://play.example.test/ws"
```

`TestPortalReadsWebSocketAddressesFromThePresenceAndTheConfig` 与 `service/account` 的 `TestServerPresence*` 新增断言用到新字段（`Entry.WSAddr`、`ServerPresence.WSAddr`），在基线上编译不过，没有断言级的红；它们的承诺由变异 M2、M4、M5、M9、M10 证明。

实现之后（全新生成的工程，`roost module verify --json`）：

| 模块 / 联动 | 通过 | 跳过 |
| --- | --- | --- |
| portal | 21（原 17，加 4 条） | 0 |
| login | 35（原 34，加 1 条） | 0 |
| portal+login / portal+enter / login+enter | 1 / 2 / 6 | 0 |
| account_auth / enter / servertime / servertime+enter | 7 / 29 / 12 / 3 | 0 |

检查项 `go build ./...`、`go vet ./...`、`roost id check`、`roost generate --check` 全部通过。`modules/portal/module.yaml`、`modules/login/module.yaml` 的 `verify.pass`、`scripts/test-modules-generated.sh`（两处）与 `codegen/internal/roost/module_portal_test.go`（三处）同步为 21 / 35。

另：`roost config check --service account` 对写了 `portal.websocket_servers: {"1000": https://bad.example.test/ws}` 的配置报 `portal.websocket_servers.1000: the websocket address must be a ws:// or wss:// URL`（退出码 3），去掉后 `config is valid`。

## 3. 变异

每个变异在生成工程里改一处、跑 `-run 'TestPortal|TestWithPortal'`，再恢复：

| 变异 | 结果 |
| --- | --- |
| M1 推荐不按方式过滤（ws 时也把没有 WebSocket 地址的服交给规则） | 红：`TestPortalAnswersWith…` `transport "ws": recommended 1001, want 1002`；`TestPortalLoginEnters…`（frank 被送去只有 TCP 的服） |
| M2 要 ws 而没有地址时回落到 TCP 地址 | 红：`enter 1000 (TCP only) over ws: 200 …"addr":"203.0.113.7:7000"`；登录 enter 与上报过期后的进服同样红 |
| M3 非法 transport 当成 tcp | 红：`/portal/servers with transport "websocket": 200`；登录 enter 同样 |
| M4 Overlay 整条覆盖、不按方式叠 | 红：`both reported, tcp: server 1001 = "gate-b.example.test:7000 [tcp]", want "… [tcp,ws]"` |
| M5 地址不看 transport（总给 TCP 地址） | 红：四条场景都红 |
| M6 登录 enter 不把 transport 交给推荐 | 红：`carol, recommended, ws: 400 … portal_transport_unavailable` |
| M7 登录 enter 不把 transport 交给进服 | 红：`carol, recommended, ws` 拿到的是 TCP 地址 |
| M8 推荐结果只核对在清单里（不核对过滤后的集合） | 第一轮存活（缺省规则只看得到过滤后的服），补“工程规则总推荐只有 TCP 的服”一段后红：`transport "ws": recommended 1000, want 0` |
| M9 清单不带 transports | 红 |
| M10 Start 丢掉 portal.websocket_servers | 红 |

login 上报：去掉“嵌入 TCP 开着才推导”的条件 → `embedded TCP off (Gate): … WSAddr:ws://gate.example.test:7080/ws, want ""`；`ws_addr` 写错不报错 → `no host: Start = <nil>`。两处都恢复后通过。

## 4. 本检出上跑过的

- `GOWORK=off go test -count=1 ./service/account/`：ok；`go vet ./service/account/`：通过。
- `GOWORK=off go test -count=1 -run Portal ./codegen/internal/roost/`：`TestModulePortalInstallsOnALobbyProjectWithoutGame`、`TestModulePortalInstallsOnTheHostedAccountService` 通过。
- `GOWORK=off go test -count=1 .`（根包）：ok。
- 没跑：全量 pretag、模块矩阵（`scripts/test-modules-generated.sh`）、`go test ./...`。

## 5. 没有验证

- 真实进程：game 上报 `ws_addr` → NATS → account → Redis → 入口，没有经真实进程；`RedisPresenceStore` 存 `ws_addr` 只靠 JSON 编码（字段可省略），没有用真实 Redis 跑 `TestServerPresence*`（`ROOST_REVIEW3_BACKEND=redis`）。
- 浏览器 / Unity WebGL 拿入口给的 `wss://` 地址真的连上 Gate：要等 C# `Roost.Client` 的 WebSocket 传输，Web 端演练一起做。
- 独立 Gate 部署：game 看不到 Gate 的 WebSocket 配置，`login.presence.ws_addr` 要手写；`roost add gate` 不写它，doctor 的 gate:client-address 不核对 WebSocket 地址。
- 推导出的 `ws://` 地址在前面有终结 TLS 的负载均衡、端口被映射时是错的（浏览器在 https 页面里也不允许连 `ws://`），这种部署要写 `ws_addr` 或 `portal.websocket_servers`；没有检查拦住“生产里用了推导地址”。
