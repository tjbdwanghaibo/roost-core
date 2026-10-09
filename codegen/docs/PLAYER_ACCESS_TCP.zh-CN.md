# Player TCP 接入、主动推送与生产约束

`access.player` 是 transport-neutral 的协议分发层；TCP 是显式、可组合的接入实现。它们分开声明，
因此测试可以绕过 socket，未来也能增加其他 transport，而 Entity/Component/Nest handler 不受影响。

本文描述当前嵌入业务进程的 TCP 接入能力。独立 Gate 的现状、待补能力和实施顺序见[网关设计](../../docs/framework/GATEWAY.md)。当前正式生成的玩家接入传输仅支持 TCP。

## 1. 生成与所有权

```bash
roost add access player --service game
roost add transport tcp
```

第二条命令会更新 `roost.yaml` 的 `access.player.transports`，并生成：

| 路径 | 所有者 | 用途 |
| --- | --- | --- |
| `internal/access/player/tcp/server_gen.go` | codegen | listener、frame、Session、主动推送、Mod 生命周期 |
| `internal/access/player/tcp/server_gen_test.go` | codegen | 帧往返、分配前限包、Principal 隔离、主动推送测试 |
| `internal/access/player/tcp/auth.go` | 应用 | 校验登录票据；`project sync` 不覆盖 |
| `configs/examples/access.player.tcp.yaml` | codegen | 自动写入 Service 配置时使用的安全默认值参考 |
| `docs/PLAYER_ACCESS_TCP.zh-CN.md` | codegen | 当前版本对应的项目内说明 |

`auth.go` 默认返回 `gateway.ErrUnauthenticated`，listener 默认关闭。二者是独立的 fail-closed 防线，
防止脚手架刚生成就暴露一个信任客户端 PlayerID 的公网端口。

## 2. 帧协议

固定头为 16 字节，整数使用网络字节序：

| 偏移 | 长度 | 字段 | 规则 |
| --- | ---: | --- | --- |
| 0 | 2 | magic | ASCII `RS` |
| 2 | 1 | version | 当前为 2；旧格式直接拒绝 |
| 3 | 1 | flags | bit0 为推送；bits1..2 为载荷类型：0=PB、1=Sync、2=Lockstep |
| 4 | 4 | message_id | 0 用于首帧鉴权及鉴权后的心跳；业务消息必须非零 |
| 8 | 4 | sequence | 非零；客户端命令按单连接递增 |
| 12 | 4 | payload_length | 分配内存前检查配置上限 |
| 16 | N | payload | 鉴权 token、空心跳，或按类型编码的 PB/Sync/Lockstep 字节 |

类型值 3 和其余保留位必须拒绝。当前 TCP 上行接受 flags=0 的 PB 请求及 flags=4 的 Lockstep Notify，
后者必须在 ProtocolRegistry 显式注册，且不返回成功 ACK。Sync 上行在分发前拒绝。
载荷类别必须与消息号注册一致，不能通过修改类型位获得更多业务权限。

连接后的第一帧必须是 `message_id=0`、flags=0、非空 token 的鉴权帧。服务端成功时返回相同 sequence 的空鉴权响应；
失败直接断开。业务请求按连接串行 Dispatch，响应沿用请求 sequence。倒序、重复 sequence、保留 flag、
未知协议、解码失败或内部业务错误都关闭连接；稳定业务错误应由项目 endpoint 编码为明确 errcode。

每次 Dispatch 在连接 ctx 上派生一个 `dispatch_timeout` 截止的 ctx（RR-20260926-36）；Session 被关闭
（同 SessionID 重连替换、`CloseSessions`、网络错误）时取消连接 ctx，在途 Dispatch 随之结束，读循环、
连接槽和按 IP 计数随连接一起释放。截止只结束**等待**，不撤销 handler 已准入的事务：超时后结果可能已经
提交，客户端对非幂等请求应按幂等键重试或先查询。handler 在截止后仍返回的响应照常写回（用连接 ctx，
受 `write_timeout` 约束）。截止依赖 handler 遵守 ctx；不遵守 ctx 的阻塞调用不会被传输层抢占
（读循环不为每个请求起 goroutine）。超过预算的请求计入 `player_tcp_dispatch_timeout_total`。

服务端主动推送的 flags 分别为 PB=1、Sync=3、Lockstep=5，使用每个 Session 独立递增的服务端 sequence，不占用请求/响应 sequence 空间。
客户端必须分别维护两个方向的序列语义。

鉴权后的心跳使用 `message_id=0`、flags=0、空 payload，并占用正常递增的客户端 sequence。
开启心跳时，服务端返回相同序号的空 ACK。客户端应在 `idle_timeout` 到期前发送心跳或正常请求；
服务端单纯向客户端推送不会延长读期限。心跳服务端支持不代表每种客户端 SDK 已自动定时发送心跳。

## 3. 鉴权

在应用所有的 `auth.go` 中验证服务端签名、有效期、登录代次、设备绑定和重放信息，成功时返回非零
`PlayerID` 与非空 `SessionID`。不要接受客户端自报 PlayerID，不要把长期签名密钥写进仓库、镜像或
`roost.yaml`。生产环境从 Secret/KMS 注入密钥，并在 LB/sidecar 终止 TLS，或仅在受保护内网开放端口。

同一 SessionID 再次登录时，新连接替换旧连接；同一玩家可以同时保留多个不同 SessionID。Principal
的 Claims 在 transport 内复制，业务拿到的 map 不能反向修改会话身份。

## 4. 配置

`add transport tcp` 会自动把以下块以 disabled 状态加入开发配置和生产示例，不覆盖其他 YAML 内容：

```yaml
player_access:
  tcp:
    enabled: false
    addr: 0.0.0.0:7000
    heartbeat_enabled: true
    request_rate: 100
    request_burst: 200
    max_connections: 10000
    max_connections_per_ip: 128
    max_handshakes: 1024
    max_handshake_bytes: 8192
    max_payload_bytes: 1048576
    handshake_timeout: 5s
    idle_timeout: 90s
    write_timeout: 5s
    shutdown_timeout: 10s
    dispatch_timeout: 3s
    login_timeout: 2s
```

- `heartbeat_enabled`：是否接受鉴权后的空心跳，默认 true。关闭后收到消息号 0 的帧会断连。
- `request_rate/request_burst`：每连接令牌桶的持续速率和突发额度，默认每秒 100、突发 200。
  心跳与业务请求共用额度，超额断连；rate=0 显式关闭限流。重连建立新桶，不提供跨连接或集群限流保证。
- `dispatch_timeout`：单个请求的总预算。应与 `nest.request_timeout` 一致（handler 内一次 Nest 调用已有这个
  预算）；键缺省时取同一配置里的 `nest.request_timeout`，再缺省为 3s。`add transport tcp` 补键时按该服务配置
  已有的 `nest.request_timeout` 写入。上限 5m。
- `login_timeout`：登录请求里“把玩家接入本进程”（归属 Claim + 冷加载）可用的份额，必须不大于
  `dispatch_timeout`；缺省为 min(2s, dispatch_timeout)。传输层本身不使用它，只经 `Runtime.LoginTimeout()` 发布给
  登录端点（game-demo 的 EnterGame 超出时回 `login_timeout` 错误码，客户端重试）。

完成 `auth.go` 后执行 `roost config enable player-tcp`。命令会先确认鉴权不再是默认骨架，再只修改
`enabled` 标量；临时停流使用 `roost config disable player-tcp`，不需要手工编辑 YAML。

这只是“骨架已修改”检查，不是鉴权安全证明。生产配置需要显式指定文件：

```bash
roost config enable player-tcp --file configs/service/config.game.prod.yaml
```

上线前仍需安全评审，并覆盖伪造、过期、重放、错误登录代次和错误设备绑定测试。

生成的 `cmd/playerprobe` 可先验证真实 socket 与鉴权，无需自己拼帧：

```bash
read -rsp "ticket: " ROOST_PLAYER_TOKEN; export ROOST_PLAYER_TOKEN; echo
go run ./cmd/playerprobe -addr 127.0.0.1:7000
unset ROOST_PLAYER_TOKEN
```

探针拒绝命令行 token，避免票据进入进程列表；它只做连接和鉴权，业务协议仍应通过 endpoint 集成测试验证。

代码还设置了生产硬上限：最大 1,000,000 个连接、最大 16 MiB payload、握手/写超时不超过 1 分钟、
idle 不超过 24 小时、transport shutdown 不超过 5 分钟。应用总 shutdown timeout 必须大于 transport
预算。生成的 transport Mod 把 `shutdown_timeout` 声明为停机预算（`StopBudget()`，`app.ModStopBudgetProvider`，RR-20260927-05）：
App 在 `shutdown.total_timeout` 内按声明值给它排空连接的时间，生成的 `total_timeout` 已计入它（改大 `shutdown_timeout` 时
`roost project doctor` 按配置判定并给出建议值，total 要同步调大）；兼容 `Stop()` 路径同样用它。此前它不声明预算，只得到 3s 保底
（game-demo 的 game 服务约 3.7s），低于自己的 10s。Linux 同时配置 `nofile`、listen backlog、conntrack 和 LB idle timeout；LB idle 应略大于应用值。

## 5. 主动发布

从 App Registry 获取生成的 TCP Runtime：

```go
transport, ok := app.Lookup[*playertcp.Runtime](registry, playertcp.Name)
if !ok {
	return playertcp.ErrTransportUnavailable
}
if err := transport.PushPlayer(ctx, playerID, msgid.PlayerNotice, notice); err != nil {
	return err
}
```

- `PushPlayer`：协议只编码一次，发布到玩家全部已认证会话。某条连接写失败（写超时、连接重置、已关闭）时，
  这条连接立即注销并关闭（客户端看到断线、需重连；连接槽、按 IP 计数与会话关闭事件照常各释放一次），
  其余连接收到则返回 nil；没有任何连接收到时返回错误（此时这些连接都已关闭）。写之前就被拒绝的推送
  （ctx 已结束、payload 超限）不关闭连接并返回错误（RR-20260926-52）；调用方 ctx 的截止早于 `write_timeout`
  且在第一个字节写出前到期的推送同样是写前拒绝，返回 `context.DeadlineExceeded`、不关闭连接；写出部分字节，
  或 `write_timeout` 本身到期（即便一个字节都没写进），仍视为连接损坏并关闭（RR-20260926-68）；
- `PushSession`：只投递指定 SessionID，写失败同样关闭该会话；
- `PushSyncPlayer/PushSyncSession`：发送已经编码的原始 Sync 帧，flags=3，不经过 PB encoder；
- `PushLockstepPlayer/PushLockstepSession`：发送既有 Lockstep 广播或追帧字节，flags=5；
- `ActiveSessions`：返回当前玩家在线会话数；
- `ErrSessionNotFound`：玩家/会话离线；
- `ErrTransportUnavailable`：listener 未启动或正在关闭。

这些 Runtime 接口查找的是当前进程的连接，尚不是跨 Gate 的全局推送服务。PB 推送使用注册的 encoder；
Sync/Lockstep 调用方负责提供对应格式的合法字节，传输层继续检查外层包大小。

内存推送不是可靠事务。必须送达的奖励、交易、邮件或跨服事件应先写持久化记录/outbox，再把在线推送
作为低延迟提示；客户端重连后从权威状态补齐。

## 6. 性能与鲁棒性

- `ProtocolRegistry` Seal 后通过 atomic immutable snapshot 无锁分发；
- 每连接一个顺序读循环；同玩家多连接之间仍可能并发，由 Nest 和业务契约协调，不能把连接顺序当作全局玩家顺序；
- payload 在读取长度后才分配，小于 64 KiB 的 buffer 有界复用；
- 写操作受 Session mutex 和 deadline 保护，慢客户端产生同步背压，不创建无界写队列；写满 `write_timeout` 仍写不进的连接被关闭，不再拖住后续推送；
- accept 错误指数退避，满连接立即拒绝；TCP 开启 keepalive 与 no-delay；
- 登录票据使用独立 8 KiB 上限，鉴权并发和单 IP 连接都有硬上限；
- Response 和主动 Push 同样执行 payload 上限，不能绕过入站限包；
- 认证前连接也被跟踪，SIGTERM 会先停 listener、关闭全部连接、等待 goroutine；
- 重复 SessionID 替换和主动推送/断线交错均由锁与幂等注销保护。

core obs 自动记录 `player_tcp_connections`、鉴权/帧/dispatch/写/拒绝/推送错误与 dispatch histogram。
业务指标放在 Authenticator 或 Protocol middleware，禁止把 PlayerID/SessionID 作为标签制造高基数。

Docker/Kubernetes 模板会为 player TCP 所属 Service 声明 7000。Kubernetes 只允许带
`roost.tjbdwanghaibo.io/player-access=true` 标签的调用方命名空间访问该端口；更改监听端口时必须同步
更新 Service、LB、NetworkPolicy 和防火墙。

## 7. 发布门禁

```bash
roost project doctor --workflow player-tcp
roost generate --check
go test ./...
go test -race ./...
go vet ./...
make build
```

`player-tcp` doctor 会检查 transport 声明、生成 runtime、auth.go 是否仍是默认拒绝骨架，以及目标 Service
配置是否启用且具有正数连接/限包值。上线前另外压测半包/粘包、超大长度、慢头、错误 token、sequence
重放、客户端不读响应、多会话推送、一万连接、SIGTERM、LB 断连与重连。
