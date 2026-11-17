# 七层负载均衡后取真实 IP（交接 C3，选项 A）实施证据（2026-11-17）

方案原文：[open-work-c3-forwarded-ip-20261117](../open-work-c3-forwarded-ip-20261117/README.md)。维护者 2026-11-17 定：选项 A（`wsconn.Config` 加可信代理列表，`Conn.RemoteAddr()` 返回客户端地址），规则照方案“推荐 A 的规则”1～5，安全三点照方案；relay 不加按 IP 限额、不暴露这一项；PROXY protocol 不做。

## 做了什么

- `infra/network/wsconn`
  - `Config.TrustedProxies []string`（CIDR 或单个地址）与 `Config.ForwardedFallback func(*http.Request, reason string)`；`ValidateTrustedProxies`，`Config.Validate` / `Listen` / `NewListener` 都经它拒绝写错的列表。
  - `ServeHTTP` 升级后按规则定出客户端地址（`forwardedClient`）：列表为空不看头；对端不在列表里不看头；对端可信时 `X-Forwarded-For` 多行按顺序拼接、从右往左剥掉可信项，第一个不可信的是客户端；头缺失 / 为空（`missing`）、某一项不是地址（`invalid`）、全部可信（`all_trusted`）退回对端地址并调 `ForwardedFallback`。只认 `X-Forwarded-For`。
  - `Conn.RemoteAddr()` 返回客户端地址（`*net.TCPAddr`，端口 0），`Conn.PeerAddr()` 总是 TCP 对端。
  - 拒绝：全零前缀（`0.0.0.0/0`、`::/0`）、IPv4 映射的 IPv6 前缀（`::ffff:0:0/96` 即全部 IPv4，要写成 IPv4）、空项与带空白的项、解析不出的项。
- `infra/network/gateway`
  - `WebSocketConfig.TrustedProxies string`（逗号分隔，保持 `TCPConfig` 可比较）、`Proxies()`；`WebSocketConfig.Validate(section)` 收拢 websocket 段的检查（path / allowed_origins 只在 Addr 非空时查，trusted_proxies 写了就查），`ValidateTCPConfig` 调它。
  - `webSocketConfig()` 把列表交给 wsconn；`ForwardedFallback` 计 `player_tcp_websocket_forwarded_fallback_total{reason}` 并写 Debug 日志（带对端地址）。
  - 按 IP 上限拒绝时写 Debug 日志，`client`（计数用的客户端地址）与 `peer`（TCP 对端，`peerAddr`）一起写；指标不变（`player_tcp_connection_rejected_total{reason="per_ip"}`）。`remoteHost` 没动：它读的 `RemoteAddr()` 已是客户端地址。
- 生成器 `codegen/internal/roost`（只碰 player_tcp / websocket / gate 的渲染）
  - `playerWebSocketDeclaration` 加 `TrustedProxies string config:"trusted_proxies"`（无缺省、不是 starter 键，生成器不写）。键是 `player_access.tcp.websocket.trusted_proxies`、`player_access.gate.websocket.trusted_proxies`。
  - 嵌入 TCP（`configFromViper`）与独立 Gate（`options.TCP.WebSocket=`）透传它（`renderPlayerWebSocketConfig`，两处同一个表达式）。
  - 生成的 `tcpConfig` / `gateConfig` 在声明了 websocket 时多一个 `ValidateConfig`，调 `gateway.WebSocketConfig.Validate("<段>.")`。理由：生成器不能 import 运行时（根包 `dependency_boundary_test.go` 的 `layerViolation`，codegen 只许 import 三个叶子包），规则只能留在 wsconn 一处，经生成的进程检查；配置结构体的 `ValidateConfig` 由声明的 `Decode` 调用，App 启动前（任何 Mod Init 之前）、进程 `--check-config`、`roost config check` 与 doctor 的 `config-check` 走的都是它，错误点名本段的键（Gate 的是 `player_access.gate.websocket.trusted_proxies`）。没声明 websocket 的工程生成结果不变（`TestPlayerWebSocketTransportPassesItsKeysThrough` 断言纯 TCP 的 server_gen.go 里没有 WebSocket 字样）。
  - 生成工程的接入说明（`docs/PLAYER_ACCESS_TCP.zh-CN.md` 的“WebSocket 接入”一节，`renderPlayerWebSocketGuide`）写可信代理的配法、规则、被拒的写法与回退指标。
- 文档：`docs/framework/GATEWAY.md` §6（wsconn 的 Config / RemoteAddr / PeerAddr、配置、生成工程、“部署注意”整段改写成规则 1～5 与安全三点、回归列表）、`OBSERVABILITY.md`（新指标）、`RELAY.md` §6（relay 没有按 IP 限额，靠令牌、帧率与人数上限）、`CAPABILITIES.md`、方案文档顶部加实施指向。
- relay：代码没改。`wiring/relay` 构造的 `wsconn.Config` 不设 `TrustedProxies`，请求头一律不看（规则 1）；`wiring/relay` 的 race 测试照常通过。

## 配置名的选择

用 `websocket.trusted_proxies`（完整键 `player_access.tcp.websocket.trusted_proxies` / `player_access.gate.websocket.trusted_proxies`），不用 `ws_trusted_proxies`：玩家接入现有的写法就是 websocket 子段（`player_access.{tcp,gate}.websocket.{addr,path,allowed_origins}`，`withWebSocketSection` 生成），`ws_` 前缀是 relay 的平铺写法（`relay.ws_allowed_origins`），放在玩家接入里会和同段的另外三个键不一致。

## 先红后绿

修前失败原样见同目录文件。红都落在承诺本身上（地址取值、上限计数、检查结论），不是编译错误：先加了无行为的字段与方法桩，让用例编译通过再看断言。

- [red-wsconn.txt](red-wsconn.txt)：`TestATrustedProxyForwardsTheClientAddress` 七个子用例 `RemoteAddr host = 127.0.0.1, want 1.2.3.4 / 1.1.1.1 / 2001:db8::1`；`TestATrustedPeerWithoutAUsableHeaderFallsBackToThePeer` 六个子用例 `ForwardedFallback was not called`；`TestTrustedProxiesAreValidated` 八项被接受、`Listen accepted a trusted proxy list that trusts everyone`。`TestTheHeaderIsIgnoredUnlessThePeerIsATrustedProxy` 修前就绿（现状本来不看头），它是给变异 M1 用的守卫；它同时确认了方案“尚未确认”第 2 条：修前 `Conn.RemoteAddr()` 实际返回 TCP 对端 127.0.0.1（库的 NetConn 取 Hijack 出来的 net.Conn 的对端）。
- [red-gateway.txt](red-gateway.txt)：Gate 端到端 `client 2.2.2.2 behind the trusted proxy, limit 1 per IP: failed to get reader: failed to read frame header: EOF`（同一代理后第二个不同客户端被按 IP 上限拒绝）；`TestWebSocketConfigIsValidated...` 不点名 `websocket.trusted_proxies`、`trusted_proxies ::/0: <nil>`。
- [red-codegen.txt](red-codegen.txt)：生成器实现撤回、运行时实现保留时，`TestGeneratedProjectRefusesTrustedProxiesThatTrustEveryone` 的 `config-check:gate` / `config-check:game` 在 trusted_proxies 写成 `10.0.0.0/8, 0.0.0.0/0` / `::/0` 时都是 `ok ... passes the process's --check-config`（doctor 放过）；渲染与说明的两条用例同样红。

方案“测试”一节 1～6 的对应：1 → `TestATrustedProxyForwardsTheClientAddress/one_hop`（IPv6 对端另有 `TestATrustedIPv6ProxyForwardsTheClientAddress`，修后加的，本机有 `::1` 时实跑通过）；2 → `TestTheHeaderIsIgnoredUnlessThePeerIsATrustedProxy`（不可信对端、列表为空）；3 → `.../through_a_second_trusted_proxy`、`.../a_forged_leftmost_entry`（`9.9.9.9, 1.1.1.1, 10.0.0.5` → 1.1.1.1）；4 → `TestATrustedPeerWithoutAUsableHeaderFallsBackToThePeer`（缺失、为空、不是地址，回调计数一次）；5 → `TestWebSocketPerIPLimitCountsTheForwardedClientBehindATrustedProxy`（真实 WebSocket 连接经同一 TCPServer：同一可信代理后 1.1.1.1 与 2.2.2.2 两条都认证成功、上限 1，同一个 1.1.1.1 的第二条被拒且 `per_ip` 加 1；可信对端不带头的两条按代理地址计数、第二条被拒、回退指标加 2；不配可信代理时客户端自带的头不起作用）；6 → 下面的变异。

## 变异（逐条，撤掉后用例变红，见 [mutations.txt](mutations.txt)）

| 编号 | 变异 | 变红的用例 |
| --- | --- | --- |
| M1 | 去掉“对端必须可信”的判断 | `TestTheHeaderIsIgnoredUnlessThePeerIsATrustedProxy/an_untrusted_peer`：`RemoteAddr host = 1.2.3.4, want the TCP peer 127.0.0.1` |
| M2 | 改成取最左一项 | `TestATrustedProxyForwardsTheClientAddress/a_forged_leftmost_entry`、`/the_header_on_two_lines`：`9.9.9.9, want 1.1.1.1` |
| M3a | 规则 4 退回但不报告（不计指标） | wsconn `ForwardedFallback was not called`；gateway `forwarded fallbacks counted = 0, want 2` |
| M3b | 规则 4 改成放行（取不出时每条连接各算一个地址，不按代理地址计数） | wsconn `RemoteAddr host = 240.0.x.y, want the TCP peer 127.0.0.1`；gateway `a second connection without X-Forwarded-For was not counted on the proxy's address` |
| M4 | 不拒绝全零前缀 | wsconn `trusted proxy "0.0.0.0/0" was accepted`、`Listen accepted ...`；gateway `trusted_proxies ::/0: <nil>` |
| M5 | Gate 不把 trusted_proxies 交给 wsconn | gateway 端到端 `client 2.2.2.2 ... EOF` |

M1 第一次的写法删掉了 `peer` 的使用、编译失败，不算红；改成 `if err != nil || false && !isTrusted(...)` 重跑，红在断言上（mutations.txt 是重跑后的结果）。生成器那一侧的“变异”就是上面的 red-codegen：撤掉生成的 `ValidateConfig` 与透传，doctor 放过 `0.0.0.0/0`。

## 验证命令与结果

```
GOWORK=off go test -race -count=1 ./infra/network/wsconn/ ./infra/network/gateway/ ./wiring/gate/ ./wiring/relay/   # 全部 ok（green-runtime.txt）
GOWORK=off go test -race -count=1 -timeout 60m -run 'WebSocket|TrustedProx|PlayerTCP|Gate|ConfigCheck|PlayerAccess' ./codegen/internal/roost/   # ok 255.8s（green-codegen.txt；没加 -v，其余用例有没有按环境跳过没逐条看，新增的三条另用 -v 确认实跑）
GOWORK=off go vet ./infra/network/wsconn/ ./infra/network/gateway/ ./wiring/gate/ ./wiring/relay/ ./codegen/internal/roost/   # 通过
GOWORK=off go test -count=1 -run 'Boundary|SharedConfig|Layer|ForbiddenCoreImport|Pillar|DoNotLinkDrivers' .   # 根包依赖边界 ok
```

没有跑全量 `go test ./...` 与 pretag（按批次规则留给协调者）。

## 实施里自己定的细节（规则 1～5 没说到的，均取更严或更窄的一边）

1. 列表项可以写单个地址（按 /32、/128），不只 CIDR。
2. 每一项都在可信列表里（`all_trusted`）时退回对端地址并计指标，不取最左一项：此时分不出哪一项是客户端自己填的。代价是内网客户端经负载均衡来时按负载均衡计数。
3. `X-Forwarded-For` 的项带端口（`1.2.3.4:5555`、`[2001:db8::1]:80`）也认；IPv4 映射的 IPv6 地址还原成 IPv4（与 TCP 对端写法一致，同一客户端一个计数键）。
4. trusted_proxies 写了就检查，即使 `websocket.addr` 为空（监听没开）；path / allowed_origins 仍只在 addr 非空时检查（原行为）。
5. 拒绝 IPv4 映射的 IPv6 前缀（不只全零前缀）：`::ffff:0:0/96` 等于全部 IPv4，是全零前缀的另一种写法。
6. 被拒连接与回退的日志用 Debug（与现有“限流断开”同级）：按 IP 拒绝由对端触发，用 Warn 会让一个来源刷爆日志。指标不受影响。

## 尚未确认

- 没有经真实七层负载均衡（nginx / 云 ALB）演练：测试里“代理”是 127.0.0.1 直接带头拨号；负载均衡是否追加而不是覆盖 / 原样转发 `X-Forwarded-For` 取决于部署，文档已写明要求。
- 浏览器端到端没做（与 GATEWAY §6 现有说明一致，放到 Web 端演练）。
- 生成工程的嵌入 TCP（不带 Gate）只经同一个 `tcpConfig` 类型间接覆盖：编译检查的工程是“独立 Gate + Game Ingress”，Game Ingress 读的就是 `tcpConfig`；嵌入 TCP 的启动路径没有单独编译跑一遍。
- 独立 Gate 运行期 `ValidateTCPConfig` 的错误文本仍写 `player_access.tcp.websocket.*`（原有现象，Gate 也用它）；生成的 `gateConfig.ValidateConfig` 在 Init 读配置时先报，点名的是 `player_access.gate.websocket.*`，所以用户看到的是正确的键。

## 建议写进 CHANGELOG `[Unreleased]` 的文字（由协调者统一写）

- **七层负载均衡后取真实 IP（WebSocket 接入，交接 C3 选项 A）**：`wsconn.Config.TrustedProxies` / `gateway.WebSocketConfig.TrustedProxies`，生成工程的键 `player_access.tcp.websocket.trusted_proxies`、`player_access.gate.websocket.trusted_proxies`（逗号分隔的 CIDR 或单个地址）。不写时行为不变（不读请求头，按 TCP 对端计数）。写上负载均衡的地址后，对端在列表里的连接按 `X-Forwarded-For` 从右往左第一个不可信的地址计 `max_connections_per_ip`、交给鉴权器；取不出时退回对端地址并计 `player_tcp_websocket_forwarded_fallback_total{reason=missing|invalid|all_trusted}`。只认 `X-Forwarded-For`。`0.0.0.0/0`、`::/0` 与 IPv4 映射的 IPv6 前缀被拒：运行时 `ValidateTCPConfig`、生成工程的启动前检查 / `--check-config` / `roost config check` / doctor `config-check`（生成的 `tcpConfig`、`gateConfig` 多了 `ValidateConfig`）。
- **行为变化**：`wsconn.Conn.RemoteAddr()` 在配了可信代理且对端可信时返回转发的客户端地址（端口 0），TCP 对端改由 `Conn.PeerAddr()` 给出；按 IP 拒绝的连接写 Debug 日志（`client`、`peer`）。relay 不开放这一项、没有按 IP 的连接限额。PROXY protocol 不做。
- **升级**：已生成的工程要 `roost project sync` 重新生成 `server_gen.go` 才读得到这个键（只对声明了 websocket 传输的工程）。

## 建议改 KNOWN-LIMITS 的文字（由协调者统一写）

“WebSocket 接入（2026-11-17）”一节里“七层负载均衡后面所有连接按同一个 IP 计数”一条改为：

- **七层负载均衡后要配可信代理才按客户端计数**：不写 `websocket.trusted_proxies` 时连接的对端是负载均衡，`max_connections_per_ip` 把经它来的全部 WebSocket 连接算作一个 IP；写上负载均衡自己的地址后按 `X-Forwarded-For` 从右往左第一个不可信的地址计数（规则见 GATEWAY §6“部署注意”）。只认 `X-Forwarded-For`（`Forwarded`、`X-Real-IP` 不读），四层负载均衡的 PROXY protocol 不支持，玩家 TCP 监听不读任何转发信息。relay 没有按 IP 的连接限额，保护靠令牌、每连接帧率与房间人数上限。没有经真实七层负载均衡演练（尚未确认）。
