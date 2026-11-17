# 大厅服 lobby

开房间场景（房主的客户端就是 host）的大厅服，现行契约。方案与决定见 [FEATURE-2026-11-16-lobby-relay](../maintenance/FEATURE-2026-11-16-lobby-relay.md) §4.2、§4.3、§4.6.4 第 7 步、§4.6.5、§4.7.1。

代码：`service/lobby`（业务与 HTTP 接口）、`wiring/lobby`（Mod 与 Server）、`service/relay/token`（relay 令牌，lobby 签发、relay 校验）。

## 1. 职责与边界

- 只做三件事：让玩家找到房间（建房、列房）；把玩家连到同一个 relay 实例（给地址、签 relay 令牌）；跟着 relay 的报告维护房间目录（人数、房主、关房）。
- 不做玩法校验，只看人数、密码和房主身份。玩法逻辑只在房主客户端。
- 进程无状态，可以多开。房间目录在 Redis，每条记录带 TTL，由 relay 的报告续期。
- 一个 lobby 服务可以服务多个游戏（`lobby.games`），房间按 `game` 隔开；`mode` 只是标签。

## 2. HTTP 接口

全部是 JSON。除 OPTIONS 预检外，每个请求都要带 `Authorization: Bearer gt1...`（account 签发的游戏账号 token）；请求里的 `game` 必须是 `lobby.games` 里的游戏，且等于 token 里的游戏。

| 接口 | 请求 | 答复 |
|---|---|---|
| `POST /rooms` | `{game, mode, max_members?, name?, password?, attrs?, transport?}` | `{room, relay_addr, token, expires_at}`，调用者是房主 |
| `GET /rooms?game=&mode=&limit=` | `limit` 缺省 50，上限 100 | `{rooms: [room]}` |
| `POST /rooms/{id}/join` | `{game, password?, transport?}` | `{room, relay_addr, token, expires_at}` |
| `POST /rooms/{id}/leave` | `{game}` | `{room}` |
| `POST /rooms/{id}/state` | `{game, state?, attrs?}`，只有当前房主能调 | `{room}` |

字段：
- `room`：`{id, game, mode, name, max_members, members, has_password, state, host, host_epoch, attrs, created_at_unix}`。`state` 是 `open`（可加入）或 `started`（已开始）。密码只存哈希，不回给客户端。
- `max_members` 含房主；不写取 `lobby.games.<game>.max_members`，不能超过它。
- `attrs` 是自定义属性，不超过 1KB，原样存、原样给，lobby 不解析。
- `mode` 与游戏名只许字母、数字、`_`、`-`，1～64 个字符（它们进 Redis 索引键）。`name` 至多 64 个字符，`password` 至多 64 字节。
- `transport` 是 `tcp`（缺省）或 `ws`。`relay_addr` 按它返回：tcp 是 `host:port`，ws 是完整的 `ws://` / `wss://` URL。
- `token` 是 relay 令牌，`expires_at` 是它的过期时刻（Unix 秒）。

列表只列同时满足下面几条的房间：同 game、同 mode；可加入；未满；未关闭；未过期；relay 已经报告过（房主连上了 relay）。新建的排在前面。

错误答 `{error, message}`：

| 状态 | error | 情形 |
|---|---|---|
| 400 | `bad_request` | 参数不合法 |
| 401 | `unauthorized` | 没带 token；token 无效、过期；token 的游戏与请求不符；账号不在或被封 |
| 403 | `game_not_served` / `not_host` / `wrong_password` / `kicked` | 游戏不在 `lobby.games` 里；不是房主；密码不对；被房主踢出过这个房间（`ErrKicked`） |
| 404 | `room_not_found` | 房间不存在、已过期、已关闭，或属于别的游戏 |
| 409 | `room_full` / `room_started` / `transport_unavailable` | 满员；已开始；房间所在的 relay 没有这种接入方式 |
| 413 | `too_large` | 请求体超过 16KiB |
| 503 | `no_relay` / `unavailable` | 没有可用的 relay；Redis、account、etcd 不可达（细节只进日志） |

### 2.1 加入、离开与房主

- **join 对已在房间里的人是幂等的**，房主也一样：不再查密码、人数和“已开始”，不算新人（满员也放行），席位预留期重新算，重新签一张令牌。断线重连就走这条路。
- **席位**（一条规则 `Room.holdsSeat`，join、relay 报告、验权都按它算）：
  - 席位包括 relay 报告在线的人、房主本人，以及预留期内的人。
  - 预留期：签了令牌、还没连上 relay 的人，席位留 relay 令牌寿命再加 15s。过期又没连上，就让出席位。
  - **没开局**（`open`，含从 `started` 改回 `open`）：不在线、又过了预留期，就让出席位。
  - **已开始**（`started`）：relay 报告过连上的人（记录里的 `connected`）掉线后，席位一直留着，预留过期也不让出，直到他主动离开、被踢或房间关闭（方案 §6 第 12 条）。他再 join 就是回到原座：不受“已开始”限制、不算新人，签新令牌，relay 验权也通过。掉线不回来的人一直占着位子，要房主踢掉才空出来。
  - 开局那一刻还在预留期、从没连上 relay 的人不算掉线成员：照没开局的规则，预留过期就让出；之后再 join 是新人，答 `room_started`。原因：relay 上没有他的座位，房主踢不到他，留着就永远空不出来。
  - 已开始的房间里，新账号 join 照旧答 `room_started`。
- **离开**：
  - 成员离开：立即让出席位，并经 NATS 让 relay 断开他（§5.2 的 `leave` 命令）。
  - 成员在 relay 上发 `leave` 控制帧离开：relay 报 `room.member_left`、原因 `left`，lobby 清掉他的 `connected`，席位按没开局的规则让出（已开始的房间也不留）。
  - 房主离开：目录里的房主不变，由 relay 走换 host（方案 §4.6.1）；新房主经 `room.host_changed` 报告回来。lobby 同时清掉他的 `connected`，换完 host 后他不再是房主，席位按没开局的规则让出（已开始的房间也不给主动离开的人留座）。
  - 房间还没被 relay 报告过（房主从没连上）时，房主一离开就关房。
  - 不在房间里的人调 leave 是空操作。
- **被踢**：房主在 relay 上踢人后，relay 报 `room.member_left`、原因 `kicked`；lobby 把账号记进房间记录的被踢名单 `kicked`、让出它的席位。名单随房间记录一起过期。之后这个账号 join 答 `kicked`，relay 的验权也拒绝它（§5.2），手里没过期的令牌也用不了。lobby 的名单是权威；relay 本地也记一份，房间存续期间有效，同一个 relay 上被踢的人再连直接收到 `Closed{kicked}`，relay 重启后由 lobby 的名单挡住（`Closed{lobby_rejected}`）（方案 §4.4、§6 第 11 条）。
- **令牌角色**：按 Redis 里的当前房主填，房主拿 `host`，其他人拿 `member`。lobby 与 relay 短暂不一致时以 relay 为准：房间建好以后，relay 不信令牌里的角色（方案 §4.6.5）。

### 2.2 CORS

`lobby.cors_origins` 列出允许跨域调用的游戏页面 Origin，`"*"` 表示任何 Origin。凭证是 `Authorization` 头而不是 cookie，所以放开 Origin 不会让别的站点借用玩家的登录。
- Origin 在列表里：答复带 `Access-Control-Allow-Origin: <Origin>`；预检答 204，允许 `GET, POST`，允许的请求头是 `Authorization, Content-Type`。
- Origin 不在列表里：答复没有 CORS 头，预检答 403。

## 3. 鉴权

lobby 不持有 account 的密钥，也不自己验签。

- 收到请求后，把 token 交给 account 的 `ValidateGameToken` RPC。
- 返回的 `GameAccount` 经 `RequireGame(请求的 game)` 核对游戏。
- `wiring/lobby.AccountTokens` 负责转换结果：`session_invalid`、`identity_denied`、`account_missing`、`game_invalid` 都答 401；其他错误（account 不可达）答 503。
- `service/lobby.GameTokenValidator` 是 lobby 里的最小接口；单测用替身，也可以由 `lobby.NewMod(validator)` 注入。

## 4. 房间目录（Redis）

| 键 | 内容 |
|---|---|
| `<key_prefix>:room:<id>` | 房间记录（versionstore 信封 + JSON），PX = `lobby.room_ttl`，每次写入重置 |
| `<key_prefix>:rooms:<game>:<mode>` | 可列出房间的索引（ZSET，分数是负的建房秒），与记录在同一个脚本里增删 |

- **写入**：每次修改都走 versionstore 的 compare-and-set。加入、离开、改状态、relay 报告之间没有“读、判断、写”的窗口。
- **Redis Cluster**：记录与索引在同一个脚本里，`key_prefix` 要带 hash tag，例如 `roost:{party}:lobby`。
- **过期**：
  - relay 不再报告（崩溃、房间关闭）时，记录随 TTL 消失。
  - 迟到的报告不会让过期的房间复活：Update 遇到不存在的房间直接返回。
  - 房间号是 64 位随机数，从不复用。所以列表读到记录已经不在的索引项时，直接删掉它。
- **关闭的房间**：标成已关闭并撤出索引，记录等 TTL 到期。

## 5. 与 relay 的契约

### 5.1 relay 实例发现（etcd）

- 键：`<lobby.relay_registry_prefix><instance>`，例如 `/party/relays/relay-1`。
- 值：JSON `{"instance":"relay-1","tcp_addr":"10.0.0.5:7100","ws_addr":"wss://relay1.example.com/relay","rooms":12}`。
- 约定：
  - 键挂在 relay 进程的租约上，进程没了，键随之消失；
  - `instance` 等于键的最后一段，只许字母、数字、`_`、`-`，1～64 个字符；它也是 relay 令牌里的实例名和命令主题的最后一段；
  - 没开哪种接入，就不写对应的地址字段；
  - `rooms` 是当前房间数，变化时更新（relay 侧合并成每秒最多一次即可）。
- 选法：每次建房读一次前缀，选有所要接入方式、`rooms` 最少的实例；房间数相同时取实例名最小的。
- 坏条目（解不开、实例名与键不符、实例名不合写法）跳过并记日志。

### 5.2 房间事件与命令（NATS）

`<subject>` 是 `lobby.subject`，relay 侧配置同一个值。

| 方向 | 主题 | 消息 |
|---|---|---|
| relay → lobby | `<subject>.events` | `RoomEvent`；lobby 用队列组 `lobby` 订阅，多个实例只有一个处理 |
| lobby → relay | `<subject>.relay.<instance>` | `RelayCommand`；relay 实例订阅自己名字的主题 |
| relay → lobby（request） | `<subject>.verify` | `RelayVerifyRequest` → `RelayVerifyReply`；lobby 用队列组 `lobby` 订阅，一个实例答复 |

`RoomEvent`：

```json
{"type":"room.heartbeat","room":"<id>","relay":"relay-1","seq":7,"online":["acc-1","acc-2"],"host":"acc-1","epoch":0,"account":"","reason":""}
```

- `type`：
  - `room.heartbeat`：定时发，间隔要明显短于 `lobby.room_ttl`，推荐 `room_ttl/3`；
  - `room.member_joined` / `room.member_left`：成员进出时立即发，`account` 是进出的人；
  - `room.host_changed`：换 host 完成时立即发（方案 §4.6.4 第 7 步）；
  - `room.closed`：房间关闭时发。
- 除 `room.closed` 外，每一条都带全量快照：`online` 是此刻连着的账号（含房主），`host`、`epoch` 是 relay 的当前房主与 `host_epoch`。建房时的房主是 epoch 0，每换一次 host 加 1。
- `seq`：同一房间内由 relay 严格递增，从 1 开始。
- `reason`：`member_left` / `closed` 的原因名；lobby 只认 `member_left` 上的 `kicked`（被踢）与 `left`（主动离开），其余原因（`disconnected`、`idle_timeout` 等）都按掉线处理。
- `host` / `epoch` 是 relay 上最近一个已就绪的房主：relay 选新房主的过程中仍报旧房主与旧 epoch，新房主回了 host_ready 才发 `room.host_changed`（新 epoch）。
- `relay` 必须等于房间记录里的实例，不等就整条忽略。

lobby 的处理：
- 房主按 epoch 做 CAS，只接受更大的 epoch，与消息类型无关：心跳也带着，丢了一条 `host_changed` 下一次心跳补上。之后 `/rooms/{id}/state` 只认新房主，令牌角色也跟着变。
- 在线名单只认 `seq` 更大的报告；迟到的旧报告不改名单，只续 TTL。
- `member_left` 带 `kicked`：账号记进被踢名单、让出席位，与 `seq` 无关（踢出是永久的事实，迟到也照记）；之后 relay 的在线名单里再出现它也不补回席位。
- `member_left` 带 `left`：清掉这个账号的 `connected`（主动离开）。只认 `seq` 更大的：迟到的旧 `left` 不能清掉回座后又连上的人。
- 按名单重算席位：在线的记下 `connected`、清掉预留；不在线的按 §2.1 的席位规则决定留不留；名单里有、目录里没有的人补上（relay 是“谁连着”的权威）。
- 每一条被接受的报告都写一次记录，也就续了一次 TTL。房间已经不在、已关闭、或不是这个 relay 的报告，静默忽略。

`RelayCommand`：`{"type":"leave","room":"<id>","account":"<账号>"}`。relay 把这个账号移出房间，等同于他自己 leave；是房主时 relay 走换 host。命令是尽力而为，发不出去只记日志：席位已经在目录里让出；客户端调完 leave 也应自己断开 relay 连接（单纯断开在已开始的房间里只算掉线，见 §2.1）。

**验权**（方案 §4.3）：relay 每条连接（含重连、含建房的第一条）在本地验签之后都发一条 request 到 `<subject>.verify`：

```json
{"room":"<id>","account":"<账号>","relay":"relay-1","create":false}
```

lobby 读房间记录（只读，不写），按顺序核对，答复 `{"ok":true}` 或 `{"ok":false,"reason":"<原因>"}`：

| 原因 | 情形 |
|---|---|
| `room_not_found` | 房间不存在、已过期或已关闭 |
| `wrong_relay` | 房间不分在发请求的 relay 实例上 |
| `kicked` | 账号在被踢名单里 |
| `not_host` | `create` 为 true（relay 上还没有这个房间），账号却不是记录里的房主 |
| `not_member` | 账号不占席位：没 join，或已离开、席位已按 §2.1 让出 |
| `unavailable` | lobby 读 Redis 出错；relay 按 lobby 不可用处理 |
| `bad_request` | 请求解不开或缺字段 |

- 只核对能不能进房，不核对角色：换 host 之后目录里的房主可能比 relay 晚一步，角色以 relay 为准（方案 §4.6.5）。
- relay 那边：lobby 拒绝 → `Closed{lobby_rejected}`；`unavailable`、超时（`relay.admit_timeout`）、没有 lobby 订阅 → `Closed{lobby_unavailable}`。都拒绝这条连接（失败即关闭）。
- 代码：`service/lobby.VerifyRelayConnection` / `HandleRelayVerifyData`，`wiring/lobby.Server` 订阅并把答复发到请求的回信主题。

一个 `subject` 对应一份房间目录，也就是一个 `key_prefix`。两个 lobby 服务共用 relay 时，必须共用 `key_prefix` 与 `subject`，各自只服务自己的游戏。

### 5.3 relay 令牌

格式：`rt1.<过期秒>.<relay 实例>.<房间号>.<账号>.<host|member>.<最大人数>.<签名>`。
- 实例、房间号、账号三段是 base64url。
- 签名是 HMAC-SHA256，带域前缀 `roost.relay.token\n`。
- 寿命 `lobby.token_ttl`，缺省 30s。

`service/relay/token` 的接口：
- `Sign(key, Claims) (string, error)`；
- `Verify(key, token, relay实例, now) (Claims, error)`：令牌写的实例不是校验方自己就无效，各种无效一律 `ErrInvalid`；
- `KeyFromEnv(name)`：读密钥，至少 16 字节；
- `Claims{Relay, Room, Account, Role, MaxMembers, ExpiresAt}`。

方案 §4.3 的格式没有列“最大人数”一段，这里按 §4.4“房间最大人数以 lobby 给的为准，放在令牌里”补上。

## 6. 配置与装配

```yaml
lobby:
  listen: 0.0.0.0:8090                 # 缺省 127.0.0.1:8090
  key_prefix: roost:{party}:lobby       # 必填
  games:                                # 至少一个
    party: {max_members: 8}
  room_ttl: 30s                         # 1s..10m
  token_ttl: 30s                        # relay 令牌寿命，1s..10m
  token_key_env: ROOST_RELAY_TOKEN_KEY  # 必填；与 relay.token_key_env 指向同一个变量
  relay_registry_prefix: /party/relays/ # 必填；与 relay 侧同一个值
  subject: roost.party.lobby            # 必填；与 relay 侧同一个值
  cors_origins: [https://play.example.com]
```

进程装配（`framework: lobby` 托管服务）：
- Mods：`redis`、`nats`、`etcd`、`account.NewClientMod()`、`lobby.NewMod(nil)`；
- Service：`lobby.NewServer()`。

各部分的职责：
- Mod：读配置，从环境变量读令牌密钥（没设、不足 16 字节都起不来），装配后登记 `*lobby.Lobby`（能力名 `service.lobby`）。Mod 不起 goroutine。
- `Server.Serve`：先订阅 `<subject>.events`，再绑定 `lobby.listen`，开始处理 HTTP。
- `Server.Shutdown`：先退订，再停 HTTP，并在停机预算内等在途请求结束。
- HTTP 的超时用 `httpserver` 的缺省值：读请求头 5s、读请求 15s、写响应 15s、空闲 60s。

## 7. 限制

- 选 relay 用的是 etcd 里的房间数，它由 relay 定时更新，有延迟。几个 lobby 实例同时建房，可能都选中同一个实例。
- 索引里过期房间的项只在列表时清理；没人列的 game/mode，它的索引会留着这些项（每项一个房间号）。
- 一个 `subject` 只能对应一份房间目录（§5.2 末尾）；两个目录用了同一个 `subject`，事件会被另一个目录的实例收走、丢掉。
