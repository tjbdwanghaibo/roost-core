# 方案：七层负载均衡后的真实 IP（交接 C3，2026-11-17）

状态：**方案，未实施**。等维护者定了再做。源码行号按 main 的 `742ccafc`。

## 先说一个和交接文档不一致的地方

交接文档写“Gate 和 relay 共用的 `infra/network/wsconn` 都要改”。核对的结果：

- **按 IP 限连接数只在 Gate 里有。** `infra/network/gateway/tcp.go:333-334` 用 `remoteHost(connection.RemoteAddr())` 做键计数，超过 `MaxConnectionsPerIP` 就拒绝。
- **relay 没有按 IP 的限额。** `service/relay` 的 `Config`（`service/relay/relay.go:63-104`）里没有连接数或按 IP 的字段；在 `service/relay` 与 `wiring/relay` 里搜 `per_ip`、`PerIP`、`MaxConnections`、`RemoteAddr` 都没有结果。

所以这一项对 relay 来说不是“修一个算错的限额”，而是“relay 要不要有这个限额”，是另一个决定（见最后）。

## 现状

- Gate 的 TCP 与 WebSocket 连接走同一个接受循环，取 IP 的方式相同：`remoteHost` 把 `RemoteAddr()` 拆出主机部分（`infra/network/gateway/tcp.go:851-860`）。缺省上限 128（`infra/network/gateway/tcp_config.go:105`）。
- WebSocket 握手在 `wsconn` 的 `ServeHTTP` 里完成，这时手里有完整的 HTTP 请求（`infra/network/wsconn/wsconn.go:192-224`），能读到请求头；升级之后交出去的 `Conn` 只带连接本身，请求头不再可见。`Conn` 的注释写明“RemoteAddr 是 TCP 对端地址；前置七层负载均衡时是负载均衡的地址”（`wsconn.go:283`）。
- 仓库里没有任何 Go 代码读 `X-Forwarded-For`、`X-Real-IP` 或 PROXY protocol（全仓搜索只在文档和 `node_modules` 里出现）。
- 文档已如实登记：`docs/framework/GATEWAY.md:148`、`docs/maintenance/KNOWN-LIMITS.md:376`、生成器的配置说明 `codegen/internal/roost/player_tcp_config.go:99`，都写的是“按负载均衡的地址计数，要调大”。

实际后果：经七层负载均衡接入时，这个限额要么形同虚设（调得很大），要么把正常玩家挡在外面（没调）。它防不住单个来源占满连接。

## 选项

| | 做法 | 改动范围 | 代价 |
| --- | --- | --- | --- |
| A（推荐） | `wsconn.Config` 加可信代理列表（CIDR）。握手时如果直连对端在列表里，就从 `X-Forwarded-For` 取客户端地址；`Conn.RemoteAddr()` 返回这个地址 | 只改 `wsconn` 和配置；Gate 的 `remoteHost` 不用动 | `RemoteAddr()` 的含义变了（不再一定是 TCP 对端），日志里要能同时看到两个地址 |
| B | `wsconn.Conn` 另加一个 `ClientAddr()`，`RemoteAddr()` 不变；Gate 判断连接有没有这个方法 | `wsconn` 和 Gate 的接受循环都改 | 含义清楚，但每个用到地址的地方（限额、日志、指标）都要记得换 |
| C | 不读请求头，只支持四层负载均衡加 PROXY protocol | TCP 与 WebSocket 都受益 | 要加解析（标准库没有），对七层负载均衡无效，解决的不是登记的那个问题 |

### 推荐 A 的规则

1. 列表为空（缺省）：**完全不看请求头**，行为和现在一样。否则任何客户端都能自己带一个头来冒充别的地址、绕过限额。
2. 直连对端不在列表里：不看请求头，用对端地址。
3. 直连对端在列表里：把 `X-Forwarded-For` 的各项从右往左看，跳过在列表里的，第一个不在列表里的就是客户端地址。
4. 对端可信但头缺失、为空或解析不出合法地址：退回对端地址（这些连接仍然按负载均衡的地址计数，方向上是更严，不是更松），并计一个指标。
5. 只认 `X-Forwarded-For`。`Forwarded`（RFC 7239）和 `X-Real-IP` 这一轮不做。

### 安全上要守住的三点

- 不配可信代理时请求头一律忽略（规则 1）。
- 可信列表写得太宽（例如 `0.0.0.0/0`）等于让所有人伪造：配置校验要拒绝全零前缀，doctor 也查。
- 从右往左剥，不能取最左边一项（最左边是客户端自己填的）。

## 影响面

- `infra/network/wsconn`：`Config` 加字段与校验；`ServeHTTP` 里算地址；`Conn` 覆盖 `RemoteAddr()`（选项 A）。
- Gate：配置段 `player_tcp` 的 WebSocket 部分加一项（建议叫 `ws_trusted_proxies`，名字待定）；`codegen/internal/roost/player_tcp_config.go` 的配置结构与说明、`render_player_tcp.go` 的校验用例、doctor。
- relay：`wiring/relay/relay_mod.go` 也构造 `wsconn.Config`（`:163`），加不加这一项取决于 relay 要不要按 IP 限额。
- 文档：GATEWAY.md §部署注意、KNOWN-LIMITS 一“WebSocket 接入”对应的一条、生成工程的部署文档。
- 日志与指标：被拒的连接现在只计 `reason=per_ip`；建议日志里同时带对端地址和解析出的客户端地址。

## 测试（先红后绿）

1. `wsconn`：可信对端加 `X-Forwarded-For: 1.2.3.4` → `RemoteAddr()` 的主机是 `1.2.3.4`（现在是对端地址，先红）。
2. 不可信对端带同样的头 → 仍是对端地址。列表为空 → 仍是对端地址。
3. 多跳：`1.1.1.1, 10.0.0.5`，`10.0.0.0/8` 可信 → `1.1.1.1`；客户端伪造 `9.9.9.9, 1.1.1.1` → 仍是 `1.1.1.1`。
4. 头缺失、为空、不是地址 → 对端地址，指标加一。
5. Gate 端到端：同一个可信代理后面两个不同客户端地址，上限设 1，两条都能连；同一个客户端地址的第二条被拒。
6. 变异：去掉“对端必须可信”的判断 → 第 2 步红；改成取最左一项 → 第 3 步红；规则 4 改成放行不计数 → 第 4、5 步红。

## 需要维护者定的点

1. 选 A 还是 B（`RemoteAddr()` 的含义要不要变）。
2. 配置项的名字和位置；Gate 与 relay 是否共用同一个写法。
3. 只认 `X-Forwarded-For`，还是也认 `Forwarded` / `X-Real-IP`。
4. 四层负载均衡的 PROXY protocol 做不做（另一项工作）。
5. **relay 要不要有按 IP 的连接限额**。现在没有；relay 的保护靠令牌、每连接帧率和房间人数上限。

## 尚未确认

- Gate 把 `wsconn` 的监听接进接受循环的具体位置没有逐行读（只确认了取 IP 的那一处对两种连接相同）。
- `Conn.RemoteAddr()` 现在实际返回什么（内嵌的是库的 `NetConn`），只读了注释，没有跑代码核对。
- relay 的日志里有没有记对端地址，没有查。
