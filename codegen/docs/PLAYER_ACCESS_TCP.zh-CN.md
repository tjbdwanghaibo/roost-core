# Player TCP 接入、主动推送与生产约束

`access.player` 是 transport-neutral 的协议分发层；TCP 是显式、可组合的接入实现。它们分开声明，
因此测试可以绕过 socket，未来也能增加其他 transport，而 Entity/Component/Nest handler 不受影响。

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
| 2 | 1 | version | 当前为 1 |
| 3 | 1 | flags | 请求/响应为 0；服务端主动推送为 1 |
| 4 | 4 | message_id | 0 仅用于首帧鉴权 |
| 8 | 4 | sequence | 非零；客户端命令按单连接递增 |
| 12 | 4 | payload_length | 分配内存前检查配置上限 |
| 16 | N | payload | 鉴权 token 或生成的协议 PB |

连接后的第一帧必须是 `message_id=0` 的鉴权帧。服务端成功时返回相同 sequence 的空鉴权响应；
失败直接断开。业务请求按连接串行 Dispatch，响应沿用请求 sequence。倒序、重复 sequence、保留 flag、
未知协议、解码失败或内部业务错误都关闭连接；稳定业务错误应由项目 endpoint 编码为明确 errcode。

服务端主动推送使用 flag 1 和每个 Session 独立递增的服务端 sequence，不占用请求/响应 sequence 空间。
客户端必须分别维护两个方向的序列语义。

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
    max_connections: 10000
    max_connections_per_ip: 128
    max_handshakes: 1024
    max_handshake_bytes: 8192
    max_payload_bytes: 1048576
    handshake_timeout: 5s
    idle_timeout: 90s
    write_timeout: 5s
    shutdown_timeout: 10s
```

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
预算。Linux 同时配置 `nofile`、listen backlog、conntrack 和 LB idle timeout；LB idle 应略大于应用值。

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

- `PushPlayer`：协议只编码一次，发布到玩家全部已认证会话；
- `PushSession`：只投递指定 SessionID；
- `ActiveSessions`：返回当前玩家在线会话数；
- `ErrSessionNotFound`：玩家/会话离线；
- `ErrTransportUnavailable`：listener 未启动或正在关闭。

内存推送不是可靠事务。必须送达的奖励、交易、邮件或跨服事件应先写持久化记录/outbox，再把在线推送
作为低延迟提示；客户端重连后从权威状态补齐。

## 6. 性能与鲁棒性

- `ProtocolRegistry` Seal 后通过 atomic immutable snapshot 无锁分发；
- 每连接一个顺序读循环，同玩家命令不额外并行争锁，跨玩家由 Nest 并行；
- payload 在读取长度后才分配，小于 64 KiB 的 buffer 有界复用；
- 写操作受 Session mutex 和 deadline 保护，慢客户端产生同步背压，不创建无界写队列；
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
