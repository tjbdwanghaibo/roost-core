# 转发服 relay

开房间场景（房主的客户端就是 host）的转发服，现行契约。方案与决定见 [FEATURE-2026-11-16-lobby-relay](../maintenance/FEATURE-2026-11-16-lobby-relay.md) §4.3、§4.4、§4.6.5、§4.7.1；大厅一侧见 [LOBBY](LOBBY.md)。

代码：`service/relay`（转发核心）、`wiring/relay`（Mod 与 Server）、`client/wire/relay.go`（帧头编码，客户端 SDK 共用）、`service/relay/token`（relay 令牌，lobby 签发、relay 校验）。

## 1. 职责与边界

- 只转发、不做逻辑：玩法跑在房主客户端。relay 只读 `client/wire` 的帧头（通道、槽位），不解析数据帧的载荷。
- 不做帧同步、状态校验、反作弊、断线托管。
- 房间只活在被分配到的那个 relay 实例的内存里，不迁移；relay 进程崩溃时房间丢失，客户端看到连接断开，lobby 的房间记录随 TTL 消失。
- 本期房主断开就关房、断开全部成员。换 host（§4.6）下一批接在 `room.hostLostLocked`。

## 2. 帧格式

与 Gate 同一个 16 字节包头 `RS | 版本 | flags | message_id | seq | 载荷长度`（[client/README](../../client/README.md)），区别只在两处：

| 字段 | relay 帧上的含义 |
|---|---|
| flags bits3..4 | 通道：`01` 游戏、`10` 房间、`11` 控制；`00` 只用于连接上的第一帧（令牌） |
| flags bits0..2 | 数据帧上归游戏（推送位、载荷类型），relay 原样带过；控制帧上必须为 0 |
| flags bits5..7 | 保留，必须为 0 |
| seq | 槽位：客户端发出的帧写**目标槽位**（0 = 全体），relay 转出的帧改写为**发送者槽位**；控制帧上是控制对象的槽位 |
| message_id | 数据帧上归游戏（可以为 0）；控制帧上是操作码（非 0） |

- Gate 的 `wire.Validate / Read / Write` 不认通道位：relay 帧发到 Gate 当非法包拒绝。relay 连接用 `wire.ReadRelay / WriteRelay / EncodeRelay / DecodeRelay`。
- 金样：`client/spec/relay-packets.json`，`client/wire` 的 `TestRelayProtocolGolden` 锁住；`client/spec/packets.json`（Gate 帧）没有变。C# / TS 的 relay SDK 交付时消费这一份。
- WebSocket 接入（`relay.ws_listen`）：relay 写给客户端的每条二进制消息恰好是一帧；客户端发来的帧拆成几条消息或几帧合在一条消息里都能读。帧格式、令牌、转发、限额与 TCP 完全相同。

### 2.1 转发规则

| 通道 | 发送者 | 目标（seq） | 收件人 |
|---|---|---|---|
| 游戏 | 成员 | 不看 | 只有房主，seq = 成员槽位 |
| 游戏 | 房主 | 某个槽位或 0 | 那个成员，或全体成员；seq = 房主槽位 |
| 房间 | 任何人 | 某个槽位或 0 | 那个人（房主也可以），或除自己以外的全体；seq = 发送者槽位 |

目标不在线的帧丢弃。被禁言账号的房间帧丢弃，游戏帧照常。

### 2.2 控制帧

| 操作码 | 方向 | seq | 载荷 |
|---|---|---|---|
| 1 `Welcome` | relay → 刚进房的连接 | 自己的槽位 | `[角色 1B：1 房主 / 2 成员][房主槽位 4B 大端]` |
| 2 `MemberJoined` | relay → 房主 | 成员槽位 | `[原因 1B][账号]`；原因 0 = 新加入，3 = 同一账号重连顶掉旧连接（槽位不变） |
| 3 `MemberLeft` | relay → 房主 | 成员槽位 | `[原因 1B][账号]` |
| 4 `Closed` | relay → 某条连接，随后断开 | 0 | `[原因 1B]`；发送队列满被断开的连接收不到 |
| 5 `Muted` | relay → 被禁言 / 解除的成员 | 自己的槽位 | `[1 禁言 / 0 解除]`；被禁言的账号重连后在 Welcome 之后再收到一次 |
| 16 `Kick` | 房主 → relay | 要踢的槽位 | 无；该账号在房间存续期间不能再进 |
| 17 `Mute` | 房主 → relay | 要禁言的槽位 | `[1 禁言 / 0 解除]` |
| 32 `Ping` | 客户端 → relay | 0 | 不透明，至多 16 字节；重置空闲计时 |
| 33 `Pong` | relay → 客户端 | 0 | 原样带回 Ping 的载荷 |

- 成员发 `Kick` / `Mute` 不生效；不认识的操作码丢弃（留给协议扩展），64～127 留给换 host。
- 原因码：1 disconnected、2 kicked、3 replaced、4 room_closed、5 frame_too_big、6 rate_limited、7 slow_consumer、8 idle_timeout、9 protocol、10 token_rejected、11 room_not_found、12 room_full、13 shutdown、14 left（经 lobby 的 leave 离开）。

## 3. 连接与房间

1. 接入后 `relay.auth_timeout` 内要送来第一帧：`message_id=0、flags=0`，载荷是 lobby 签的 `rt1` 令牌原文（不支持 Gate 的连接代号信封）。验签失败、过期、实例名不是本实例：`Closed{token_rejected}`。
2. 房间不存在时，只有 host 令牌能建房：这条连接是房主，占槽位 1；房间人数上限取这张令牌里的值（含房主）。成员令牌先到：`Closed{room_not_found}`。令牌过期前房主没到，房间就不存在，什么也不用清理。
3. 房间存在时，**角色按房间状态定**（§4.6.5）：账号等于当前房主就是房主，否则是成员，不看令牌里的角色字段。
4. 同一账号已在线：新连接顶掉旧连接（旧的收到 `Closed{replaced}`），槽位与角色不变；房主收到 `MemberJoined{replaced}`。lobby 不收到进出报告（在线名单没变）。
5. 新成员取 2 起最小的空槽位；在线人数（含房主）已到上限：`Closed{room_full}`。成员下线后槽位空出来，重连可能换槽位，房主按 `MemberJoined` 里的账号对应。
6. 房间结束：
   - 房主断开（或被 lobby 的 leave 移出、或因超限被断开）：关房，全部成员收到 `Closed{room_closed}`；
   - 只剩房主超过 `relay.empty_room_timeout`：关房（`Closed{idle_timeout}`）；
   - 关过的房间号在 `relay.closed_room_retention` 内不能重建，挡住还没过期的 host 令牌。

## 4. 限额与缺省值

| 配置 | 缺省 | 超限 | 取值理由 |
|---|---|---|---|
| `max_payload_bytes` | 64 KiB | 断开（`frame_too_big`） | 房主一次广播的全量状态够用；更大的数据由游戏分片 |
| `frames_per_second` | 200 | 断开（`rate_limited`） | 60Hz 游戏帧 + 房间帧 + 心跳的数倍；所有通道合计，突发量是一秒的额度 |
| `room_frames_per_second` | 30 | 丢这一帧，连接保留 | 聊天、表情、语音包（20ms 一包 = 50/s 时需调高）；语音不能挤掉游戏通道 |
| `room_bytes_per_second` | 64 KiB | 丢这一帧，连接保留 | 语音 16～64 kbps 的数倍；不能小于 `max_payload_bytes` |
| `send_queue_frames` | 1024 | 断开这一条（`slow_consumer`） | 排队 + 正在写的帧；放帧的一方从不等待 |
| `send_queue_bytes` | 4 MiB | 同上 | 至少两个满帧 |
| `idle_timeout` | 30s | 断开（`idle_timeout`） | 客户端约 10s 发一次 Ping |
| `auth_timeout` | 5s | 断开（`idle_timeout`） | |
| `write_timeout` | 10s | 断开这一条 | 一次写 socket 的期限 |
| `empty_room_timeout` | 10m | 关房 | 房主等人进房的上限 |
| `closed_room_retention` | 2m | — | 不短于 lobby 的令牌有效期（缺省 30s） |
| `heartbeat_interval` | 5s | — | 明显短于 `lobby.room_ttl`（推荐 room_ttl/3） |

内存预算：每条连接最多驻留 `send_queue_bytes`；多个收件人共用同一份编码好的帧。

空闲与令牌首帧的超时用每条连接一个计时器检查最后一帧的时间，不用读期限：WebSocket 接入在读期限到期时会直接关掉整条连接，relay 就来不及发 `Closed`。用计时器后 TCP 与 WebSocket 都先收到 `Closed{idle_timeout}` 再断开。

## 5. 与 lobby 的契约

两边写同一个 `subject` 与 registry 前缀（`lobby.subject` / `relay.subject`，`lobby.relay_registry_prefix` / `relay.registry_prefix`）。

| 方向 | 通道 | 内容 |
|---|---|---|
| relay → lobby | NATS `<subject>.events` | 房间报告（JSON，`service/relay.Report` = `service/lobby.RoomEvent`） |
| lobby → relay | NATS `<subject>.relay.<instance>` | 命令 `{"type":"leave","room","account"}`：把账号移出房间，是房主时关房 |
| relay → lobby | etcd `<registry_prefix><instance>` | `{"instance","tcp_addr","ws_addr","rooms"}`，挂在 relay 自己的租约上（`relay.registry_ttl`） |

报告：

- 类型：`room.heartbeat`（建房时立即一条，之后每 `heartbeat_interval` 一条）、`room.member_joined`、`room.member_left`、`room.closed`；`room.host_changed` 留给换 host。
- 每条带 `seq`（同一房间严格递增，从 1 开始）与全量快照 `online`（在线账号，按槽位排列，含房主）、`host`、`epoch`（本期恒为 0）。lobby 只用序号更大的报告改写在线名单，所以丢一条、乱序都不要紧。
- 附加字段 `slot`、`reason`（原因码的名字）只给日志与排查，lobby 不读。
- 发送是尽力而为的（`PublishOnce`）：发不出去只记日志。

etcd 广播：Serve 开始后申请租约写键，房间数变了重写（每秒最多一次）；租约丢了重新申请；停机时撤销租约，键随之删除。

实例名 `relay.instance` 不写时取 `<server_type>-<sid>`；只许字母、数字、`_`、`-`，1～64 个字符。它是令牌里的 relay 实例、etcd 键与命令主题的最后一段。

## 6. 接线

```go
// 进程装配（生成器接入 frameworkCatalog 时照这个写；NATS、etcd 的 Mod 照常作为共享 Mod）
a.RegisterServer(relay.ServiceName, relay.NewServer(), relay.NewMod())
```

- `relay.NewMod()` 依赖 NATS（`nats.raw`）与 etcd（`etcd`）：Init 读 `relay.*` 与令牌密钥（`relay.token_key_env` 指向的环境变量，`token.KeyFromEnv`），Provide 建好 relay 并登记能力 `relay`（`*service/relay.Server`）。Mod 不起 goroutine。
- `relay.NewServer()`（app.Service）：Init 开 TCP 监听（配了 `relay.ws_listen` 再开 WebSocket 监听，共用接入层 `infra/network/wsconn`）、订阅命令主题（失败就起不来）；Serve 接受连接、定时心跳、维护 etcd 广播；Shutdown 先停命令订阅，再关监听与全部房间（每条连接收到 `Closed{shutdown}`），最后撤销 etcd 租约。
- etcd Mod 的自动登记（`etcd.advertise_addr`）与 relay 的广播是两个键，互不影响。
- 监听：`service/relay.Server.Serve` 接受任意 `net.Listener`，可以对多个 listener 各调用一次，同一个房间里的连接可以来自不同的 listener（TCP 与 WebSocket 混在一起）。

### 6.1 配置

```yaml
relay:
  listen: ":7100"
  advertise_addr: "10.0.0.5:7100"        # 客户端连 TCP 的地址，生产不要写回环
  token_key_env: ROOST_RELAY_TOKEN_KEY   # 与 lobby 同一个变量
  subject: roost.party.lobby             # 与 lobby.subject 相同
  registry_prefix: /party/relays/        # 与 lobby.relay_registry_prefix 相同
  # instance、registry_ttl 与第 4 节的限额都有缺省值
  ws_listen: ":7101"                      # 可选：浏览器客户端
  ws_path: /relay                         # 缺省 /relay
  ws_advertise_addr: wss://relay1.example.com/relay   # 开了 ws_listen 就必填，经 etcd 广播为 ws_addr
  ws_allowed_origins: ["https://play.example.com"]    # 不带 Origin 的原生客户端总是放行；空表示拒绝所有浏览器页面
```

WebSocket 单条消息上限取一个满帧（`max_payload_bytes` + 16 字节帧头），升级请求的期限取 `auth_timeout`。TLS（wss）由前置负载均衡终结。

## 7. 本期限制与后续

- 换 host（§4.6）未做：房主断开即关房。接入点是 `room.hostLostLocked`；报告里的 `epoch` 与 `room.host_changed`、控制帧操作码 64～127 已经留好。
- 语音走 TCP / WebSocket，丢一个包会卡住后面的包；需要时以后加 UDP 通道。
- 槽位只在在线期间属于账号：成员掉线后重连可能拿到别的槽位。
