# lobby-relay ws 组：共用 WebSocket 接入层（2026-11-17）

方案：`docs/maintenance/FEATURE-2026-11-16-lobby-relay.md` §4.7.1、§6 第 9 条。现行契约写在 `docs/framework/GATEWAY.md` §6“WebSocket 接入”。

## 做了什么

- 新包 `infra/network/wsconn`：coder/websocket v1.8.15（2026-06-15 发布）接受升级，`websocket.NetConn(ctx, conn, websocket.MessageBinary)` 包成 `net.Conn`；`Listen` / `NewListener` 产出 `net.Listener`，relay 也能直接用。
- `gateway.TCPConfig.WebSocket{Addr, Path, AllowedOrigins}`：`Addr` 非空时 `TCPServer.Start` 多开一个 `wsconn.Listen`，连接进同一个 `acceptLoop` / `serveConnection`。握手、票据、连接代号、路由、出站预算、准入上限都没有改。只有写一帧时按连接类型分支（WebSocket 一帧写成一条消息）。
- 独立 Gate（`wiring/gate.Options.TCP.WebSocket`，`GateMod.WebSocketAddr()`）与嵌入 TCP 都能开；生成器加 `access.player.transports: [tcp, websocket]`（`roost add transport websocket`），把 `player_access.tcp.websocket.*` / `player_access.gate.websocket.*` 原样透传。

## 证据文件

| 文件 | 内容 |
| --- | --- |
| `mutations.txt` | 10 处变异（撤掉关键的一行），每处对应用例都变红，之后恢复 |
| `generator-compare.txt` | 不声明 websocket 的工程：HEAD 生成器与改后生成器逐字节相同（嵌入 TCP 94 个文件、独立 Gate 131 个文件）；声明之后的全部差异 |
| `generated-projects.txt` | 声明 websocket 的两种生成工程编译、vet、生成测试（含 `TestDeclaredDefaultsAreDefaultConfig`）通过；配置透传到监听；`roost config check` 认识新键、拼错的键被拒 |

## 命令与结果

源码树为 HEAD 加 ws 组改动（其他组的未提交改动不在内）：

```
go vet ./infra/network/gateway/ ./infra/network/wsconn/ ./wiring/gate/ ./codegen/internal/roost/      # 通过
go test -race -count=1 ./infra/network/gateway/ ./infra/network/wsconn/ ./wiring/gate/               # 全部 ok
go test -count=1 -run 'Gate|PlayerTCP|Transport|WebSocket|ConfigSchema|Manifest|Help|TestGeneratedPlayerTCPTestsWaitForTheSessionsTheyUse' ./codegen/internal/roost/   # ok（182s）
go test -count=1 -run 'TestClientSDKAgainstGeneratedPlayerTCP' ./codegen/internal/roost/             # ok（生成工程 + C#/Go 客户端链路）
```

表驱动用例（`-v`）的子用例都在 `tcp` 与 `websocket` 上各跑一遍并通过：鉴权首包与连接代号信封、时钟探针（心跳回包）、请求 / 应答与分发预算、服务器推送与停读连接被关、停读连接不拖住别人、一帧拆成多次写或多帧合在一次写、空闲超时。只在 WebSocket 上有的：Origin 被拒、超大消息被拒、一帧一条消息、配置校验与 Stop 关监听、会话 Close 打断阻塞写、调用方期限在写到一半时到期即断开。`wsconn` 包自己的用例覆盖读写期限、已过期的期限只拒绝不断开、Close 打断读写、挂载模式、未被 Accept 的升级被关、别的路径 404。

## 拿不准、留给协调者的点

1. **Origin 白名单为空时的缺省**：选了“拒绝所有带 Origin 的请求，同源也拒绝”，不带 Origin 的原生客户端放行。理由见 GATEWAY.md §6。如果想要“空白名单放行同源”（coder/websocket 的缺省），改 `wsconn.originAllowed` 一处即可。
2. **单条消息上限有 1 字节的余量**：库按流读，越过上限后的下一次读才失败，超出部分最多先读出 1 字节，之后连接以 1009 关闭。上限之内的帧照常处理。
3. **期限在读写进行中到期会关闭整条连接**：这是 coder/websocket NetConn 的行为。`writeFrame` 据 `net.ErrClosed` 把它算作连接坏了，不算拒绝。TCP 上同一情形（调用方期限到、一个字节都没写出）是拒绝、连接保留。
4. **AllowedOrigins 是逗号分隔的字符串**：`TCPConfig` 必须保持可比较，生成工程的 `TestDeclaredDefaultsAreDefaultConfig` 用 `!=` 比较整份配置。`Path` 的缺省 `/ws` 也因此不写进 `DefaultTCPConfig`，空串在使用处取 `/ws`。
5. **七层负载均衡后的按 IP 上限**：对端地址是负载均衡的地址，`max_connections_per_ip` 把全部 WebSocket 连接算作一个 IP。这一期不读 `X-Forwarded-For`，部署时要调大这个上限。
6. **生成器没有改动的部分**（为了保持未声明 websocket 时逐字节不变）：
   - 部署模板（k8s、docker）没有发布 WebSocket 端口；
   - 生成工程文档（`render_docs.go` 里的“当前支持 tcp”）没有改；
   - doctor 不检查 `websocket.addr` 的端口冲突（`listen` 标签不认空串）；
   - 没有声明 websocket 的工程写了 websocket 键，`config check` 不报错，doctor 的“没有读取方”会提示（与 gate 键在非 gate 工程的处理相同）。
7. **浏览器端到端没做**：方案说放到 Web 端演练。这里只用 Go 客户端（coder/websocket 的 `Dial`）验证。
8. 改了 `codegen/internal/roost/player_client_test.go` 的守卫用例：它按名字检查 gateway 测试“先等会话登记再用”。表驱动之后正文在小写的 `test*` 函数里、经 `dialAuthenticatedOver` / `stalledOver` 拨号，守卫跟着认这两种写法；断言没有放宽。

## 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（由协调者统一写）

- CHANGELOG（新功能）：
  - 玩家接入加 WebSocket，浏览器可连。新包 `infra/network/wsconn` 是共用适配器（coder/websocket，Gate、嵌入 TCP、relay 共用）。
  - `gateway.TCPConfig.WebSocket{Addr, Path, AllowedOrigins}` 打开后，WebSocket 连接走与 TCP 相同的会话路径。每条二进制消息装一个 RS v2 帧，服务端每帧一条消息。
  - Origin 白名单为空时拒绝所有带 Origin 的浏览器请求。
  - 生成器加 `roost add transport websocket`，嵌入 TCP 读 `player_access.tcp.websocket.*`，独立 Gate 读 `player_access.gate.websocket.*`。不声明的工程生成结果不变。
  - 新依赖 `github.com/coder/websocket v1.8.15`。
- KNOWN-LIMITS（框架限制）：
  - WebSocket 的 TLS 由前置负载均衡终结；
  - 七层负载均衡后按 IP 上限按负载均衡地址计数；
  - 期限在读写进行中到期会关闭整条连接；
  - 单条消息上限有 1 字节余量；
  - 生成的部署模板不发布 WebSocket 端口；
  - 浏览器端到端尚未验证（待 Web 端演练）。
