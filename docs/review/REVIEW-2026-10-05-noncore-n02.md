# N02 续审：不配合回调、容量、业务鉴权与跨模块配置

2026-10-05，分支 `revn02`，基线 `50e9a4e853641f348c879b85c282af9c5813f656`（origin/main）。接续[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) N02 行：8 个生产源文、正式 Webroute CLI/HTTP 消费/退役 URL 与 NC-05～07 已在[第三批](REVIEW-2026-10-04-noncore-04.md)/[修复](REVIEW-2026-10-04-noncore-05.md)完成，本轮不重做。新增 [NC-80](../bug/RR-20261005-NC-80.md)（P2）、[NC-81](../bug/RR-20261005-NC-81.md)（P3）、[NC-83](../bug/RR-20261005-NC-83.md)（P2）已修复、声明场景验证，未发版；[NC-82](../bug/RR-20261005-NC-82.md)（P3）已复现、未修，需维护者选择。[证据](../bugfix/evidence/noncore-bugfix-20261005-n02/README.md)。

## 图谱与源码

Tier 2 Verify，项目 `Users-whb-roost-roost-core`，共享 generation 停在 `2026-09-30T11:28:30Z`。`check_index_coverage` 对 12 个证据路径均 no_recorded_issue，其中 httpserver/server.go、httpclient/client.go、webroute/route.go、gateway/middleware.go、security/ratelimit.go、kit/ops/ops_mod.go、render_access.go 为 metadata_changed，全部以当前源码完整读取补证；`trace_path(JSON, inbound, depth 2)` 得 15 个调用者（httpserver 4、kit/ops 6、webroute 5），与 `rg` 的生产 import 清单（kit/ops、kit/service/account、kit/service/platform、codegen render_access/render_player_tcp、demo 模板）一致。未刷新/重建共享索引。

N02 包在仓内的真实消费者：`httpserver` ← Kit Ops 与用户手写的 Webroute 装配；`webroute` ← 生成的 `RegisterRoutes`（`docs/STATIC_REGISTRATION.md` 写明由业务手写调用）；`gateway` ← 生成的 player access（`Principal`/`Session`/`ErrUnauthenticated`），**`gateway.Chain/RateLimit/Recover/Timeout` 与 `security.RateLimiter` 仓内无装配方**；`security` token/签名 ← account/platform Service；`httpclient` 仓内无生产消费者。

## 已核对项

| 范围 | 所有权 / 数据链 | 结果 | 证据 |
| --- | --- | --- | --- |
| 生成 Webroute 响应 | `//roost:web` → webroute CLI → `RegisterRoutes` → Registrar → Engine → `WriteResult` → `httpserver.JSON` → 回环 HTTP → httpclient | NC-80：业务已执行，NaN 结果 200 空体，client `nil`；修后 500 | `webroute/route.go:173`、`httpserver/server.go:218`（修后）、`httpclient/client.go:200` |
| Kit Ops 管理面 | Init(viper)→Provide(app.NewRegistry)→Start(真实 listener)→`/admin/execute`→`admin.Registry.Execute`→`writeJSON` | NC-80 同根因；不配合 ctx 的命令：Stop 超时保留 server、命令返回后重试排空、listener 关闭（控制，复用 NC-04 契约） | `kit/ops/ops_mod.go:291,355` |
| Engine recover | `requestContextMiddleware` → `recoverMiddleware` → chi → handler | NC-81：开始后 panic 拼接成完整 200、ErrAbortHandler 被改写；修后中止/传播，Hijack/Flush/ResponseController 透传 | `httpserver/server.go:297,334` |
| 生成 TCP 接入停机 | Mod.StopWithContext → Server.Stop → listener/连接关闭 → wait → stopLifecycle | NC-83：超时后所有权丢失、重试假成功、阻塞订阅者不受 ctx；修后保留直到排空 | 生成器 `render_player_tcp.go:383,533,671` |
| 连接容量 | accept → connectionSlots → per-IP → handshakeSlots → Authenticate | 认证回调不配合：`max_handshakes` 满后第三条在 handshake_timeout(200ms) 被拒、超 `max_connections` 立即关闭、回调返回后名额归还；per-IP 满拒绝、关闭后恢复 | `tcp_review_test.go.txt` |
| 请求容量 | readFrameLimit → MaxPayloadBytes；Dispatch 单请求 DispatchTimeout | 超限帧关闭连接（控制）；DispatchTimeout 只界定等待不界定工作，与 RR-20260926-36 注释一致 | 同上 |
| 业务鉴权 | 真实 `account.Service`：Login→CreateRole→SelectRole 签票据 → `session:<pid>:<token>` 真实 TCP 握手 → demo `applicationAuthenticator` → `ValidateSession`（`security.VerifySessionToken`+角色/slot 校验）→ Principal(角色 PlayerID, server_id claim) → Dispatch | 正常 Dispatch 看到角色 PlayerID 与 sid 1300；冒用他人 pid、篡改、无前缀、空 token、负 pid、过期 6 种均在握手阶段关闭且未进 Dispatch | `auth_review_test.go.txt`；`kit/service/account/service.go:262,301` |
| gateway 中间件 | Chain(Recover, RequireAuthenticated, RateLimit) | NC-82：单主体占满全局 key 表致他人被限流，满表 O(N) 锁内扫描；Timeout 遇不配合 endpoint 只能在其返回后结算（控制，符合注释） | `gateway/middleware.go:38`、`security/ratelimit.go:105-135` |
| 跨模块配置 | `player_access.tcp.dispatch_timeout` ← `nest.request_timeout`；`login_timeout ≤ dispatch_timeout`；`shutdown_timeout` → StopBudget（RR-20260927-05）；`account.session_ttl`；`ops.*` | 生成测试已覆盖前三项（本轮生成工程实跑通过）；下列观察 O1/O2 | 生成 `server_gen_test.go`、`configs/service/config.game.yaml` |

## 观察与设计建议（非 RR）

- **O1 Ops 写超时与管理命令期限**：Ops 用 httpserver 默认 15s WriteTimeout，不可配置，`/admin/execute` 不给命令设期限。实测（WriteTimeout=100ms、handler 300ms）handler 照常完成且 ctx 未取消，客户端只得到 `EOF`：长命令的结果对运维“未知”，重试可能重复执行（GM 命令有 trace_id 幂等，其他命令未必）。没有既有承诺被打破，记观察；建议 Ops 给命令 ctx 设略短于 WriteTimeout 的期限或开放 `ops.*_timeout` 配置，并在 admin 文档写明“传输失败 ≠ 未执行”。[证据](../bugfix/evidence/noncore-bugfix-20261005-n02/observation-writetimeout.txt)。属 N01/Kit Ops，未改。
- **O2 TCP 派生上限与错误文本**：只设置 `max_connections`（≤1000）而缺 `max_handshakes`/`max_connections_per_ip` 时，默认 1024/128 违反 `≤ max_connections`，启动以不点名字段的 `addr, limits and timeouts are outside safe bounds` 拒绝。生成配置与 doctor 都要求全部键，显式矛盾也应拒绝，故非 RR；建议错误点名字段。
- **O3 会话票据**：无状态 HMAC，TTL 内可在多连接重复使用，会话建立后不再随 TTL 失效；`security` 注释已说明“签名正确不表示未撤销”。撤销/单次使用属业务策略，N06 S1 account 同行。
- **O4 demo SessionID**：`demo-<pid>-<UnixNano>`，同一玩家同一时钟刻度的两次握手可得到相同 SessionID；registerSession 会替换旧会话，旧连接的关闭事件会以同一 SessionID 发布。时钟分辨率相关、未复现，demo 应用文件，记观察。
- **O5 httpclient**：`io.ReadAll` 不限响应体大小，仓内无消费者；接入外部服务前建议加可选上限。
- **O6 未装配的中间件**：gateway 中间件与 RateLimiter 无仓内使用方，生成 access 用自己的 `player_agent.Middleware`。NC-82 的影响因此仅限外部采用者。

## 外部验证清单（未执行）

| 项 | 需要 | 应证明 |
| --- | --- | --- |
| 真实网关 / 反向代理（nginx、云 LB）前置 Engine | 部署环境 | 500 编码失败与中止连接被如实转给客户端，不被重试成成功；HTTP/2 RST_STREAM 呈现 |
| 真实游戏客户端 / robot 走 TCP 接入 | 客户端或 robot + 生成工程进程 | 握手被拒/容量满时客户端的重连退避与提示；Stop 期间的断开与重连 |
| 大量卡死认证回调的资源占用 | 有界压测机 | FD/goroutine/内存随 max_handshakes、max_connections 有界，回调恢复后归还 |
| 真实 account RPC（NATS）跨进程 ValidateSession | 隔离 NATS + 账号服务进程 | 远端超时/不可用时握手 fail-closed、握手名额按 handshake_timeout 释放 |

## 方向判断

- **生成 TCP 接入层**：近期同一文件已有 RR-20260918-06、RR-20260919-03、RR-20260926-36/52/68、RR-20260927-02/05、RR-20260928-15，本轮 NC-83。多数是“连接/会话/请求的生命周期边界”上的单点修补，NC-83 又是一次“停止状态与排空结果混在一个指针里”。信号是**同一类不变量（关闭所有权）在不同模块重复被打破**：Ops 的 RR-20261004-NC-04 与本轮 TCP 接入。判断为实现层面的模式缺失而非方向错误：每个停止入口各自用“置空字段”表达已停。建议把“发起关闭（幂等）/ 在 ctx 内等待排空（可重试）/ 排空后释放”写进 roost-coding 的生命周期复审要点作为统一模式，新增停止入口按此三段式审；暂不建议抽公共框架类型（各处资源不同，抽象收益低）。
- **httpserver/webroute**：NC-03/04（10-03）、NC-07（10-04）、本轮 NC-80/81，均为“业务副作用边界前后的结果判定”。修复在减少分支（先编码后写、按是否已开始决定），没有增加状态/重试，未见方向问题。
- **gateway/security 限流**：NC-05 后又出 NC-82，同一 key 容量模型。因为仓内无使用方，建议在首次接入前定 per-owner 上限语义（见 NC-82 选项），否则不要推荐使用。

## 停点

N02 仍为场景部分完成：本轮补齐不配合回调（认证/命令/订阅者/endpoint）、连接与请求容量、业务鉴权全链、跨模块配置主项；余项为 NC-82 待选择、O1/O2 建议、Dispatch handler 不配合的单独用例、HTTP/2 与外部清单。
