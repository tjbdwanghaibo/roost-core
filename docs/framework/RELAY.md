# 转发服 relay

开房间场景（房主的客户端就是 host）的转发服，现行契约。方案与决定见 [FEATURE-2026-11-16-lobby-relay](../maintenance/FEATURE-2026-11-16-lobby-relay.md) §4.3、§4.4、§4.6、§4.7.1、§4.7.4、§6 第 11 条；大厅一侧见 [LOBBY](LOBBY.md)。

代码：`service/relay`（转发核心）、`wiring/relay`（Mod 与 Server）、`client/wire/relay.go`（帧头编码，客户端 SDK 共用）、`service/relay/token`（relay 令牌，lobby 签发、relay 校验）。

## 1. 职责与边界

- 只转发、不做逻辑：玩法跑在房主客户端。relay 只读 `client/wire` 的帧头（通道、槽位），不解析数据帧的载荷。
- 不做帧同步、状态校验、反作弊、断线托管。
- 房间只活在被分配到的那个 relay 实例的内存里，不迁移；relay 进程崩溃时房间丢失，客户端看到连接断开，lobby 的房间记录随 TTL 消失。
- 房主走了换 host（本文 §4A，方案 §4.6）：relay 只管“谁是房主”、存房主推来的最新快照并在换 host 时原样交出去，不解析快照，不保证玩法状态连续。
- 每条连接进房前都向 lobby 验权（§3 第 2 步）；被踢的名单以 lobby 为准。

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
| 1 `Welcome` | relay → 刚进房的连接 | 自己的槽位 | `[角色 1B：1 房主 / 2 成员][房主槽位 4B 大端][epoch 8B 大端]`，固定 13 字节 |
| 2 `MemberJoined` | relay → 房主 | 成员槽位 | `[原因 1B][账号]`；原因 0 = 新加入，3 = 同一账号重连顶掉旧连接（槽位不变） |
| 3 `MemberLeft` | relay → 房主 | 成员槽位 | `[原因 1B][账号]` |
| 4 `Closed` | relay → 某条连接，随后断开 | 0 | `[原因 1B]`；发送队列满被断开的连接收不到 |
| 5 `Muted` | relay → 被禁言 / 解除的成员 | 自己的槽位 | `[1 禁言 / 0 解除]`；被禁言的账号重连后在 Welcome 之后再收到一次 |
| 16 `Kick` | 房主 → relay | 要踢的槽位（在线或有座位但离线的都行） | 无；该账号在房间存续期间不能再进 |
| 17 `Mute` | 房主 → relay | 要禁言的槽位 | `[1 禁言 / 0 解除]` |
| 32 `Ping` | 客户端 → relay | 0 | 不透明，至多 16 字节；重置空闲计时 |
| 33 `Pong` | relay → 客户端 | 0 | 原样带回 Ping 的载荷 |

- 成员发 `Kick` / `Mute` 不生效；不认识的操作码丢弃（留给协议扩展），64～127 是换 host 与前台上报（§2.3）。
- `Welcome` 里的房主槽位是当前房主的槽位（换 host 之后不一定是 1）；正在选新房主时为 0。epoch 是 relay 此刻的 host_epoch：选人阶段进房的连接拿到的是已经加过 1 的新一轮（与 `HostChanging` 的约定相同），所以换过 host 之后才进房、或重连回来的客户端不用等下一次换 host 就知道房间的 epoch，三份 SDK 收到 Welcome 时都按它更新 `Role` 的 epoch（2026-11-17 起；之前 Welcome 是 5 字节、不带 epoch，旧客户端连新 relay 会因长度不对按协议错误断开，不兼容）。
- 原因码：1 disconnected、2 kicked、3 replaced、4 room_closed（房主走了且选不出新房主）、5 frame_too_big、6 rate_limited、7 slow_consumer、8 idle_timeout、9 protocol、10 token_rejected、11 room_not_found、12 room_full、13 shutdown、14 left（经 lobby 的 leave 或 `Leave` 控制帧离开）、15 lobby_rejected（lobby 验权不让进）、16 lobby_unavailable（向 lobby 验权失败或超时）。

### 2.3 换 host 的控制帧

整数都是大端；`epoch` 是 8 字节的 host_epoch（建房时 0，每次换 host 加 1）。64～79 由 relay 发出，80～95 由客户端发出。金样在 `client/spec/relay-packets.json`（`relay_you_are_member` 起的条目；前台上报是 `relay_presence`、`relay_member_presence`）。

| 操作码 | 方向 | seq | 载荷 |
|---|---|---|---|
| 64 `YouAreMember` | relay → 被换下的旧房主、超时的候选 | 自己的槽位 | `[epoch 8B]` |
| 65 `YouAreHost` | relay → 新房主候选 | 自己的槽位 | 见下 |
| 66 `HostChanging` | relay → 发给房主的游戏帧被丢掉的成员 | 0 | `[epoch 8B]`；每次换 host 每个成员最多一条。宽限期内是当前 epoch，选人阶段是新 epoch |
| 67 `HostChanged` | relay → 房间里在线的每个人（含新房主） | 新房主槽位 | `[epoch 8B][新房主账号 UTF-8]` |
| 68 `HostResumed` | relay → 成员：房主在宽限期内回来了 | 房主槽位 | `[epoch 8B]` |
| 69 `Rejected` | relay → 发快照 / 转交的连接，连接保留 | 0 | `[被拒的操作码 1B][原因 1B][seq 8B]`；原因 1 too_big、2 too_soon、3 stale_seq、4 bad_target、5 not_host；seq 是被拒快照的 seq，转交为 0 |
| 70 `MemberPresence` | relay → 当前房主 | 上报的成员的槽位 | `[1 前台 / 0 后台]`，即成员 `Presence` 的载荷 |
| 80 `Transfer` | 房主 → relay | 目标槽位 | 交接数据（不透明，不超过 `snapshot_max_bytes`） |
| 81 `Leave` | 任何人 → relay | 0 | 无；relay 回 `Closed{left}` 后断开 |
| 82 `Successors` | 房主 → relay | 0 | 槽位列表，每个 4B，至多 256 个 |
| 83 `Snapshot` | 房主 → relay | 0 | `[seq 8B][快照字节]`；快照字节不超过 `snapshot_max_bytes` |
| 84 `HostReady` | 新房主候选 → relay | 0 | `[epoch 8B]`，等于 `YouAreHost` 里的 epoch |
| 85 `Presence` | 任何成员 → relay | 0 | `[1 前台 / 0 后台]`；别的长度或取值按协议错误断开（与 `Mute` 一致） |

`YouAreHost` 的载荷：

```
[epoch 8B][来源 1B：0 无 / 1 交接数据 / 2 快照][快照 seq 8B][快照年龄毫秒 4B][成员数 2B]
成员数 × { [槽位 4B][在线 1B：1 / 0][账号长度 2B][账号 UTF-8] }
[状态字节：剩下的全部]
```

- 成员名单是房间里有座位的账号（含离线的、含旧房主、不含被踢的），按槽位排列。
- 快照 seq 不管来源，都是 relay 存着的最新一份快照的 seq（还没有快照时为 0，见 §4A“快照”）。快照年龄（relay 收到它到发出这一帧的毫秒数，与时钟无关）只在来源为快照时给，新房主据此知道快照落后了多少；其余来源为 0。
- 这一帧可以大于 `max_payload_bytes`（最大约 `snapshot_max_bytes` + 成员名单）。客户端读 relay 帧的上限要按它放大。

**状态字节的压缩方式标识**（三份 SDK 之间的约定，relay 不解析；2026-11-17 起）：`Snapshot` 的快照字节、`Transfer` 的交接数据（也就是 `YouAreHost` 的状态字节），非空时是 `[压缩方式 1B][压缩后的字节]`，空的就是空（不带标识，收到时当空状态）。0 不压缩、1 RFC 1951 原始 deflate（三份 SDK 的缺省），2～255 归游戏自带的压缩方式（SDK README 的示例里 zstd 用 2、LZ4 用 3）。收到时按标识选解压方式：自己配的压缩方式，或内置的 0 / 1；标识不认识时新房主拿到明确的“不认识的压缩方式”错误（Go `ErrUnknownCompression`、C# `RelayUnknownCompressionException`、TS `UnknownCompressionError`），标识认识、内容坏时是解压错误；两种情况都当没有状态交给游戏。`snapshot_max_bytes` 按带标识的字节算。

**前台上报**（方案 §4.7.3；2026-11-17 起）：成员发 `Presence` 告诉房主自己在前台还是后台，relay 只检查格式，改写成 `MemberPresence{成员槽位}` 转给当前已就绪的房主；宽限期与选人阶段没有房主、或发送者就是房主时丢弃，relay 不记状态。这一帧计入每连接帧率限额（`frames_per_second`），不另设限流。SDK 把自己的状态记下，收到 `HostChanged`、以及每次连上（含自动重连）后自动重发，新房主不用问；从没设过时不发。房主一侧：TS 的 `WebHost` 在游戏没有显式 `setSuccessors` 时，自动把前台成员按槽位升序排在前面再声明 `Successors`（游戏声明过的以游戏为准），页面隐藏让出房主时也按这个顺序；Go / C# 只给 `SetForeground` 与 `OnMemberPresence`，不自动排。成员可以谎报，后果只是影响候选顺序。

## 3. 连接与房间

1. 接入后 `relay.auth_timeout` 内要送来第一帧：`message_id=0、flags=0`，载荷是 lobby 签的 `rt1` 令牌原文（不支持 Gate 的连接代号信封）。验签失败、过期、实例名不是本实例：`Closed{token_rejected}`。
2. **向 lobby 验权**（方案 §4.3，每条连接都做，重连也一样）。先按本地状态挡掉显然进不来的：房间不在而令牌不是 host、关过的房间号（`Closed{room_not_found}`）、本地记着被踢（`Closed{kicked}`）。然后经 NATS request 问 lobby（§5），带上房间、账号、本实例名，以及“这条连接要建房”（房间不在、令牌是 host）。lobby 只核对能不能进房，不核对角色。
   - lobby 拒绝：`Closed{lobby_rejected}`；
   - lobby 出错、`relay.admit_timeout`（缺省 2s）内没有应答、没有 lobby 在订阅：`Closed{lobby_unavailable}`。失败即关闭：lobby 不可用时新连接和重连都进不来，已连上的不受影响。
3. 房间不存在时，只有 host 令牌能建房：这条连接是房主，占槽位 1；房间人数上限取这张令牌里的值（含房主）。令牌过期前房主没到，房间就不存在，什么也不用清理。
4. 房间存在时，**角色按房间状态定**（方案 §4.6.5）：只有 relay 记着的当前房主连接是房主，不看令牌里的角色字段。换 host 之后旧房主拿着没过期的 host 令牌重连只是成员；宽限期内房主用同一账号回来仍是房主（§4A）。
5. 同一账号已在线：新连接顶掉旧连接（旧的收到 `Closed{replaced}`），槽位与角色不变；房主收到 `MemberJoined{replaced}`。lobby 不收到进出报告（在线名单没变）。正在等 `HostReady` 的候选被顶号时，`YouAreHost` 重新发给新连接、重新计时。
6. **槽位属于账号**，房间存续期间不变：成员下线后座位留着，同一账号回来拿回原来的槽位；新账号取下一个没用过的槽位，所以槽位小的就是进房早的（换 host 兜底选人靠这一点），房主声明的候选顺序也不会因为槽位复用指错人。在线人数（含房主；宽限期内含断线的房主）已到上限：`Closed{room_full}`。
7. 被踢：被踢的连接收到 `Closed{kicked}` 后断开，座位删掉；报告里 `room.member_left` 的原因是 `kicked`，lobby 据此把账号记进被踢名单（权威），之后 join 与验权都拒绝它。relay 本地也记一份，房间存续期间有效：同一个 relay 上被踢的人再连，直接收到 `Closed{kicked}`，不必等 lobby；relay 重启后本地记录没了，由 lobby 的名单挡住（`Closed{lobby_rejected}`）。
   - 有座位但不在线的槽位也能踢（方案 §6 第 12 条：已开始的房间里掉线的人一直占着 lobby 的席位，要房主踢掉才空出来）：同样记下账号、删座位、报 `member_left{kicked}`，房主收到 `MemberLeft{kicked}` 作为确认。槽位上没有座位（没人用过、已经踢过）时什么也不做。
   - 被踢的人不会再当候选：座位删了就不在 `YouAreHost` 的成员名单里；槽位不复用、账号进不来，这个槽位不会再有在线连接，房主声明的候选顺序（`Successors`）里留着它也会被跳过。
8. 房间结束：
   - 房主走了而没有在线成员：立即关房（`Closed{room_closed}`）；
   - 换 host 时候选都不行（都超时或断开）：关房，在线的人收到 `Closed{room_closed}`；
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
| `host_idle_timeout` | 2s | 房主连接断开（`idle_timeout`），进宽限期 | 方案 §4.6.3“relay 心跳超时，推荐 2s”；房主约每 0.6s 发一帧（一直在广播时不用另发 Ping） |
| `host_grace` | 5s | 期满换 host | 方案 §7 A |
| `host_ready_timeout` | 10s | 换下一个候选 | 方案 §4.6.4 第 5 步 |
| `snapshot_min_interval` | 1s | 拒收（`Rejected{too_soon}`） | 方案 §7 F；SDK 缺省每 3s 推一次 |
| `snapshot_max_bytes` | 256 KiB，至多 16 MiB | 不到两倍：拒收（`Rejected{too_big}`），连接保留；超过两倍：断开（`frame_too_big`） | 方案 §7 F，按压缩后字节算；转交的交接数据同一上限 |
| `admit_timeout` | 2s | 拒绝这条连接（`lobby_unavailable`） | 一次 Redis 读加两次 NATS 往返的数倍 |

- `send_queue_bytes` 至少要放得下一份带满快照的 `YouAreHost` 和一个满帧（`snapshot_max_bytes + max_payload_bytes + 16 KiB` 余量），否则候选会被当成慢连接断开。
- 快照与转交帧不受 `max_payload_bytes` 限制：读帧时先看帧头，这两种帧的上限是 `snapshot_max_bytes`；超过它但不到两倍的，载荷直接丢掉（不进内存）并回 `Rejected{too_big}`。WebSocket 单条消息的上限因此取 `max(max_payload_bytes, 2 × snapshot_max_bytes + 8) + 16`。

内存预算：每条连接最多驻留 `send_queue_bytes`；多个收件人共用同一份编码好的帧。每个房间另存最新一份快照，快照缓存的上限约为“房间数 × `snapshot_max_bytes`”（缺省 1000 个房间约 250 MiB）；换 host 期间再加一份交接数据。

空闲与令牌首帧的超时用每条连接一个计时器检查最后一帧的时间，不用读期限：WebSocket 接入在读期限到期时会直接关掉整条连接，relay 就来不及发 `Closed`。用计时器后 TCP 与 WebSocket 都先收到 `Closed{idle_timeout}` 再断开。

## 4A. 换 host（方案 §4.6）

每个房间有三种状态：**有房主**（正常转发）、**宽限期**（房主断线，等它回来）、**选人**（等候选回 `HostReady`）。房间的全部处理（连接进出、控制帧、两个计时器）都在房间锁内执行，所以方案 §4.6.4 的切换步骤是单线程的，步骤之间插不进别的事件。

**触发**

| 触发 | 宽限期 | 交给新房主的状态 |
|---|---|---|
| 房主发 `Transfer{slot, 交接数据}` | 不等；目标必须在线、不是自己，否则 `Rejected{bad_target}` | 交接数据（来源 1） |
| 房主发 `Leave`，或 lobby 发来 leave 命令 | 不等 | relay 存的最新快照（来源 2），没有就空（来源 0） |
| 房主断线：读写出错、`host_idle_timeout` 内没有任何帧、超限被断开 | 等 `host_grace`；没有在线成员就直接关房 | 同上 |

- 宽限期内同一账号重连回来（先向 lobby join 拿新令牌）：仍是房主，`Welcome{host}` 之后 relay 把此刻在线的成员逐个用 `MemberJoined` 再告诉它一遍；成员收到 `HostResumed`。epoch 不变，lobby 看到房主下线又上线（member_left / member_joined），不是 host_changed。浏览器刷新页面就是这条路（方案 §4.7.4）。
- 宽限期内 lobby 发来这个房主的 leave 命令：不再等，立即换 host。
- 宽限期内成员发给房主的游戏帧丢弃，发送者收到 `HostChanging`。

**切换步骤**（方案 §4.6.4）

1. 进入选人状态，epoch 加 1。
2. 主动转交时旧房主的连接还在：降为成员，收到 `YouAreMember{epoch}`；之后它发的游戏帧按成员帧处理（选人期间丢弃，之后转给新房主），踢人、禁言、快照、转交都不再生效。
3. 选人期间成员发给房主的游戏帧丢弃，发送者收到一次 `HostChanging{epoch}`；relay 不缓存，要不要重发由游戏决定。房间通道照常转发。
4. 选人：主动转交给目标；否则按房主最近一次 `Successors` 声明的顺序取第一个在线、这一轮没试过的成员；没有声明或候选都不在，取在线成员里槽位最小的（最早进房的）。旧房主不当这一轮的候选。给它发 `YouAreHost{epoch, 成员名单, 交接数据或快照}`。选人期间成员进出的 `MemberJoined` / `MemberLeft` 发给这个候选。
5. 候选在 `host_ready_timeout` 内回 `HostReady{epoch}`。超时：它收到 `YouAreMember{epoch}`，换下一个；断开：换下一个；都不行：关房。迟到的、epoch 不对的、不是当前候选发的 `HostReady` 忽略。
6. 收到 `HostReady`：它是新房主，房间里在线的每个人收到 `HostChanged{epoch, 槽位, 账号}`，转发恢复；候选顺序清空，由新房主重新声明。
7. 报告 lobby `room.host_changed`（§5），lobby 按 epoch 做 CAS。

**快照**（方案 §4.6.3）

- 只认当前房主发的（否则 `Rejected{not_host}`）；每个房间只存最新一份，relay 不压缩、不解压、不解析。
- seq 在房间内必须递增，跨房主也一样：新房主从 `YouAreHost` 里的快照 seq 往上接（否则 `Rejected{stale_seq}`）。`YouAreHost` 的快照 seq 不管来源是交接数据还是快照，都是 relay 存着的最新那份的 seq（2026-11-17 起；之前交接数据来源时为 0，主动转交后新房主的第一份快照会被拒）。Go 与 C# SDK 另外取 `max(本地, YouAreHost 的 seq, epoch<<32) + 1`，两条规则同时满足。
- 同一个房主的两份快照之间不少于 `snapshot_min_interval`（否则 `Rejected{too_soon}`）；换了房主重新算。
- 快照在换 host 之后仍保留，直到新房主推来更新的一份：新房主刚接手就又走了，下一个候选拿到的仍是最近的那份。

**停顿与丢失的估算**（断线的情况，缺省值）：发现断线约 2s（`host_idle_timeout`）+ 宽限期 5s + 新房主回 `HostReady` 的时间；丢失最后一份快照之后的变化，最多一个 SDK 推送间隔（3s）。

## 5. 与 lobby 的契约

两边写同一个 `subject` 与 registry 前缀（`lobby.subject` / `relay.subject`，`lobby.relay_registry_prefix` / `relay.registry_prefix`）。

| 方向 | 通道 | 内容 |
|---|---|---|
| relay → lobby | NATS `<subject>.events` | 房间报告（JSON，`service/relay.Report` = `service/lobby.RoomEvent`） |
| lobby → relay | NATS `<subject>.relay.<instance>` | 命令 `{"type":"leave","room","account"}`：把账号移出房间，是房主时关房 |
| relay → lobby | etcd `<registry_prefix><instance>` | `{"instance","tcp_addr","ws_addr","rooms"}`，挂在 relay 自己的租约上（`relay.registry_ttl`） |
| relay → lobby | NATS request `<subject>.verify` | 验权（§3 第 2 步）：请求 `{"room","account","relay","create"}`，答复 `{"ok":true}` 或 `{"ok":false,"reason":...}`；lobby 用队列组 `lobby` 订阅，一个实例答复 |

报告：

- 类型：`room.heartbeat`（建房时立即一条，之后每 `heartbeat_interval` 一条）、`room.member_joined`、`room.member_left`、`room.host_changed`（新房主回了 `HostReady`，`account`、`slot` 是新房主）、`room.closed`。房主断线进宽限期报 `member_left`（account 是房主），宽限期内回来报 `member_joined`。
- 每条带 `seq`（同一房间严格递增，从 1 开始）与全量快照 `online`（在线账号，按槽位排列，含房主）、`host`、`epoch`。`host` / `epoch` 是**最近一个已就绪的房主**：选人期间仍是旧房主与旧 epoch，新房主回 `HostReady` 之后才变；所以 lobby 只按 epoch 做 CAS 就不会被半途的状态带偏。lobby 只用序号更大的报告改写在线名单，所以丢一条、乱序都不要紧。
- `reason`（原因码的名字）：lobby 读 `member_left` 上的 `kicked`（把账号记进被踢名单）与 `left`（主动离开，已开始的房间也不再留席位，见 LOBBY.md §2.1）；其余按掉线处理，只给日志与排查。`slot` lobby 不读。
- 发送是尽力而为的（`PublishOnce`）：发不出去只记日志。

etcd 广播：Serve 开始后申请租约写键，房间数变了重写（每秒最多一次）；租约丢了重新申请；停机时撤销租约，键随之删除。

实例名 `relay.instance` 不写时取 `<server_type>-<sid>`；只许字母、数字、`_`、`-`，1～64 个字符。它是令牌里的 relay 实例、etcd 键与命令主题的最后一段。

## 6. 接线

```go
// 进程装配（生成器接入 frameworkCatalog 时照这个写；NATS、etcd 的 Mod 照常作为共享 Mod）
a.RegisterServer(relay.ServiceName, relay.NewServer(), relay.NewMod())
```

- `relay.NewMod()` 依赖 NATS（`nats.raw`）与 etcd（`etcd`）：Init 读 `relay.*` 与令牌密钥（`relay.token_key_env` 指向的环境变量，`token.KeyFromEnv`），Provide 建好 relay 并登记能力 `relay`（`*service/relay.Server`）。Mod 不起 goroutine。
- `service/relay.NewServer(config, reporter, admission)`：`admission`（`service/relay.Admission`）必填，wiring 接的是 NATS request（`<subject>.verify`），单测接替身。
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
  # 换 host 与验权（缺省值见第 4 节）
  host_idle_timeout: 2s
  host_grace: 5s
  host_ready_timeout: 10s
  snapshot_min_interval: 1s
  snapshot_max_bytes: 262144
  admit_timeout: 2s
```

WebSocket 单条消息上限取客户端可能发来的最大一帧（`service/relay.Config.MaxFrameBytes`，见第 4 节），升级请求的期限取 `auth_timeout`。TLS（wss）由前置负载均衡终结。

relay **没有按 IP 的连接限额**，也不读 `X-Forwarded-For`：WebSocket 监听不开放 `wsconn.Config.TrustedProxies`（维护者 2026-11-17：这一轮不加按 IP 限额），连接的对端地址就是 TCP 对端，经七层负载均衡时是负载均衡的地址。保护靠 lobby 签发的令牌（`auth_timeout` 内不出示合法令牌就断开）、每连接帧率（`frames_per_second`）与房间人数上限。Gate 的可信代理规则见 [GATEWAY §6“部署注意”](GATEWAY.md#6-websocket-接入)。

## 7. 本期限制与后续

- 语音走 TCP / WebSocket，丢一个包会卡住后面的包；需要时以后加 UDP 通道。
- 换 host 不保证玩法状态连续（方案 §4.6.6）：主动转交最完整；断线时丢最后一份快照之后的变化；不做投票、延迟测量选人、多人同时当 host。
- relay 选人时不看前台状态（交接 C2 的选项 3，维护者 2026-11-17 定这一轮不做）：房主来不及声明候选顺序就掉线时，兜底仍是槽位最小的在线成员；前台优先只经房主声明的 `Successors` 生效。
- lobby 不可用时新连接与重连都进不来（失败即关闭，方案 §4.3）；已连上的不受影响。
- 座位（账号 → 槽位）在房间存续期间只增不减（被踢的除外）：一个房间里进出过的不同账号越多，表越大；人数由 lobby 的席位与令牌限住，实际很小。
- relay 进程重启即丢房（维护者 2026-11-17）：旧进程上的房间不在新进程上重建。房主拿着还没过期的 host 令牌（或经 lobby 重新 join 拿的新令牌）连新进程请求建房，lobby 验权见房间已被 relay 报告过，答 `room_not_found`，连接收到 `Closed{lobby_rejected}`；成员的旧令牌对应的房间不存在，收到 `Closed{room_not_found}`；SDK 都不再重连。lobby 的目录在最后一条报告之后 `room_ttl` 内消失。只有房主、还没被 relay 报告过的房间（房主刚建、还没连上）不受影响。2026-11-17 多进程演练里，此前的“重建”会让比房主早到的成员被关在外面、报告序号从 1 重数（[证据](../maintenance/evidence/lobby-relay-multiprocess-20261117/README.md)）。
