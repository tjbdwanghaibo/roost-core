# portal：客户端登录入口（四个 HTTP 端点，挂在托管 account 的进程上）

`portal` 给工程一个**客户端够得着的登录入口**：四个 HTTP 端点，代客户端去问只在内部总线上的 account 服务（游戏的账号服）。它就是常说的 manifest（清单）那一层：登录拿账号票据 → 凭票拿服务与区服清单与推荐 → 选定一个服换进服票据与该连的地址；开房间类游戏（大厅 + relay，没有区服）另凭票换一张**游戏账号 token**，大厅凭它认人。入口缺省挂在**托管 account 服务的进程**上，不挂在某个 game 服上；业务服务（game）不因此多出任何 HTTP 监听。

入口只依赖 account 这一侧（[account_auth](../account_auth/README.md)：account 的登录协作者），不 import 任何 game 包，所以**不带 game 的开房间工程**（`roost project new <名> -template lobby`：只托管 account、lobby、relay）也能装：`roost module add portal` 一条命令。那种工程里客户端用的是 `/portal/login` 带 `game`（或 `/portal/game-token`）拿游戏账号 token（gt2），再去大厅；account 没有登记任何区服，所以 `/portal/servers` 的区服清单是空的、推荐的服是 0，`/portal/enter` 对任何服号都带着 account 的 `560104`（server_invalid）拒绝。

## 能力

**客户端协议**：没有 Nest / PB 协议，只有四个 HTTP 端点（`services/account/portal/portal.go`；都是 `POST`、JSON；字段与响应的细节见后面的“端点”一节）。

| 端点 | 请求 | 响应 | 做什么 |
| --- | --- | --- | --- |
| `POST /portal/login` | `{channel, open_id, credential, game?, enter?}` | `{ok, account_ticket, account_ticket_expires_at, game_token?, game_token_expires_at?, enter?}` | 让 account 向渠道确认身份并落到 `game` 这个游戏的账号（`Login`；不带 `game` 时是 `account.default_game`，只有一个游戏时就是它），再向它要一张账号票据（`IssueAccountTicket`）；带了 `game` 时再凭票要一张这个游戏的游戏账号 token（`IssueGameToken`）；带了 `enter` 时再替它进服（指定的服或推荐的服），`enter` 里是与 `/portal/enter` 相同的进服结果 |
| `POST /portal/servers` | `{account_ticket, service?, transport?}` | `{ok, recommended_server_id, transport, services[]?, servers[]}` | 区服清单：每个服的名字、所属服务、状态、是否并入别的服、所请求连接方式的地址、支持的连接方式（`transports`）、负载、标签、本账号在那里有没有角色；外加推荐的服（只在支持所请求方式的服里挑）。配置了 `account.services` 时另有服务清单（`ListServices`），`service` 只列那个 game 服务的服 |
| `POST /portal/enter` | `{account_ticket, server_id, transport?}` | `{ok, ticket, server_id, serving_server_id, expires_at, addr, addr_source, transport}` | 向 account 要一张进服票据（`IssueEnterTicket`），连同承载服在所请求连接方式下的地址交给客户端；`ticket` 是握手时原样出示的 `enter:<进服票据>` |
| `POST /portal/game-token` | `{account_ticket, game}` | `{ok, game, game_token, expires_at}` | 凭账号票据向 account 换一张**票据自己那个游戏**的游戏账号 token（`IssueGameToken`，`gt2.…`，刷新用；别的游戏答 `560120`）；开房间类游戏的大厅（lobby）请求里带它，大厅经 account 的 `ValidateGameToken` 认人 |

**连接方式**（维护者 2026-11-18）：玩家接入同时有 TCP 与 WebSocket（浏览器、Unity WebGL 用后者，[GATEWAY §6](../../docs/framework/GATEWAY.md#6-websocket-接入)）。客户端在 `/portal/servers`、`/portal/enter` 与 `/portal/login` 的 `enter` 里写 `transport`：`"tcp"`（缺省）或 `"ws"`，入口只给那种方式的地址——TCP 是 `host:port`，WebSocket 是完整的 `ws://` / `wss://` URL（前面常有终结 TLS 的负载均衡，路径也由部署决定）——并在响应里回显 `transport`。推荐只在支持所请求方式的服里挑；要进的服没有这种方式的地址时答 `portal_transport_unavailable`，不回落到别的方式。不写 `transport` 的客户端拿到的地址与以前相同。入口仍然只是 HTTP 服务，不代理任何连接。

**服务端入口**（`services/account/portal`）

- `portal.Start(registry) (stop func(context.Context) error, err error)`：模块的启动入口（清单的 `runtime`）。读配置、取本进程的 account 服务与 web Mod、让规则文件选区服目录、登记三条路由。任何一样不对进程不启动。
- `portal.New(Options{Accounts, Directory, Recommend}) (*Portal, error)` 与 `(*Portal).RegisterRoutes(reg)`：自己装配（把 manifest 拆成独立服务、测试）时用。
- 区服目录的构件：`NewStaticDirectory(addrs)`、`NewStaticTransportDirectory(addrs, wsAddrs)`、`NewPresenceDirectory(accounts)`（account 的带过期区服入口登记表，见“动态目录”）、`Overlay(directories...)`、`DirectoryFunc`。

**扩展点**（都在归工程所有的 `services/account/portal/portal_rules.go`）

- `newDirectory(registry, configured, reported) (Directory, stop, error)`：入口启动时调一次，决定用哪个**区服目录**（每个服该连哪个地址、多忙）。缺省 `Overlay(configured, reported)`：account 登记表里 game 上报的地址盖住配置 `portal.servers` / `portal.websocket_servers` 的，没有上报（或已过期）的回到配置；按连接方式分开叠（上报没带 WebSocket 地址时仍用配置的）。别的来源（服务发现）改这里，见“区服目录”与“动态目录”。
- `recommend(servers []ServerChoice) int32`：**推荐规则**（导量），每次 `/portal/servers` 调一次。见“推荐规则”。
- `Directory` 接口（`services/account/portal/portal_directory.go`）：`Entries(ctx) ([]Entry, error)`，工程可以自己实现。

**状态**：无。入口不存任何东西，也不持有任何密钥；票据全部由 account 签发与校验。动态地址的登记表在 account 那边（共享存储），入口每次请求读一次。

**配置**（写在**托管 account 的服务**的配置里，`configs/service/config.account.yaml`；细节见“配置”一节）

- `portal.servers`：静态的区服目录，服号 → 客户端经 TCP 连得到的 `host:port`。可以不写。
- `portal.websocket_servers`：静态区服目录的 WebSocket 那一半，服号 → 客户端经 WebSocket 连得到的完整 URL（`ws://` / `wss://`）。可以不写；没写的服不支持 `transport: ws`。
- `portal.services`：服务的地址，服务名（`account.services` 里的）→ 客户端连得到的地址（例如大厅的 HTTP 地址），清单的 `services` 里原样带上。可以不写。
- `portal.cors_origins`：允许在浏览器里跨域调用四个端点的网页 Origin（游戏页面、替游戏登录的外壳页面）。不写 = 不允许跨域（见“浏览器跨域调用”）。
- 游戏账号 token 服务哪些游戏、各自的寿命，服务清单有哪些服务，是 account 的键（`account.games`、`account.services`，见 [服务篇“account 的游戏账号 token 与多个服务”](../../docs/framework/guide/09-services.md#4-关键业务边界)）。
- 动态地址不在这里配：写在 **game 服务**的配置里（`login.presence.addr`，WebSocket 是 `login.presence.ws_addr`，login 模块的 manager 读），game 定期报给 account。
- `web.*`：入口的监听地址、超时、请求体上限（web Mod 的键）。
- 票据的有效期与签名密钥是 account 的键（`account.ticket_ttl`、`account.session_ttl`、`account.session_secret`）。

**错误码**（`internal/errors/portal.go`）：入口自己的两个——`portal_request_invalid`：必填字段没填、字段超过长度上限、服号不是正数、`transport` 不是 `tcp` / `ws`、`service` 不是 `account.services` 里的 game 服务，除最后一种外这样的请求没有去问 account；`portal_transport_unavailable`（2026-11-18）：要进的服（它的承载服）没有所请求连接方式的地址（例如要 `ws` 而那个服只有 TCP 地址），客户端换一个清单里 `transports` 含这种方式的服，或改用别的方式。其余的拒绝是 account 的码原样带回：`560111` 票据无效、`560102` 渠道拒绝凭据或账号被封、`560103` 账号不存在、`560104` 服没登记、`560120` 游戏不在 `account.games` 里、登录没带游戏而 account 没有缺省游戏、或换 token 的游戏不是票据的游戏。

**日志**：登录与签发进服票据确认，记录账号、区服；渠道拒绝记 Info。角色尚未分配，因此此处使用 account_id，不造 player_id；内部失败只记录错误码，防止渠道错误携带凭据。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `services/account/portal/portal.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **账号号是这个游戏的**（维护者 2026-11-18）：一套 account 服务多个游戏，同一个人在每个游戏里是不同的账号号；账号票据绑定游戏，清单里的角色、进服票据、游戏账号 token 都是这个游戏账号的。入口拿不到别的游戏的账号号（account 的总线接口不返回它们，`Login` 交回的账号记录上也没有身份键）；入口自己能看到的只有客户端这次登录请求里的渠道与 open id。
- **入口只守一件事**：账号只取自 account 校验过的账号票据，绝不取自请求。它不认证任何人（身份由 account 向渠道确认）、不签也不验任何票据、**不建角不选角**、不知道玩家号、不知道角色名（名字是游戏服的数据）。
- **没有限流，也没有防撞库**。四个端点谁都能调；`/portal/login` 每次都会去问渠道。对外暴露时放在有限流的反向代理后面。
- **跨域只按 Origin 白名单答 CORS 头**（`portal.cors_origins`，2026-11-18）。不在名单里的 Origin 拿不到 CORS 头、预检答 403，但请求本身照常处理（浏览器读不到响应；非浏览器客户端不受 CORS 约束）——白名单不是访问控制，限流与拒绝来源放在反向代理上。从不发 `Access-Control-Allow-Credentials`：入口不用 cookie。
- **明文**。凭据与票据都在请求体里；web Mod 不终止 TLS。生产环境 web Mod 缺省只绑回环地址，对外要放在终止 TLS 的代理后面并显式写 `web.allow_public_addr: true`。生成的生产示例配置里 `web.addr` 是占位值，上线前要改。
- **上限**：请求体 16 KiB（`MaxRequestBytes`）；`channel` 32 字节、`open_id` 128 字节、`credential` 8 KiB、账号票据 1024 字节、`game` 与 `service` 32 字节（`MaxNameBytes`）；区服至多 1024 个（`MaxEntries`，与 account 的区服上限 `svcaccount.MaxServers` 一致，2026-11-17 由 256 放宽）；目录条目的地址至多 255 字节、标签至多 8 个、每个至多 32 字节；`portal.services` 至多 64 条；account 登记表一次登记的期限至多 600 秒（`svcaccount.MaxPresenceTTLSeconds`），至多 1024 个没过期的服。
- **清单的开销随区服数增长**：每次 `/portal/servers` 是 1 次清单（account 读 1 + N 条记录）、1 次登记表（account 读一个 Redis 哈希）加每个服 1 次角色查询，顺序执行，没有缓存；`/portal/enter` 也多 1 次登记表。区服很多、请求很密的部署该把 manifest 拆出去并自己加缓存，或者换成分页的清单。
- **清单要么完整要么失败**：account、目录（含 account 的登记表）、任何一个服的角色记录问不到（不是“没有角色”，而是出错），整个请求失败，不给一份缺了角色或缺了地址的清单——登记表问不到时也不回落到静态配置冒充全部。某个服上本账号的 slot 与角色对不上（`560113`）或合服别名两跳时也是整个请求失败。
- **票据不能单张吊销**：账号票据、进服票据与游戏账号 token 都是无状态的签名，有效期内可以重复使用；封号对已经发出去的票据立即生效（account 每次校验都再读账号）。游戏账号 token（`gt2`）另可按人整代吊销：account 的 `RevokeGameTokens` 由账号号找到这个人的身份，把身份上的 token 代数加一，这个人所有游戏在此之前签发的全部失效（运维用 login 模块的 `accountctl revoke-game-tokens`；交接 C6，维护者 2026-11-17）。改密码不用另外调它：渠道验证器在 `Verified.CredentialVersion` 里返回凭证版本，玩家改密之后第一次经 `/portal/login` 登录（任何一个游戏）时 account 发现版本变了就自动加一、所有游戏一起（维护者 2026-11-18，接法见 [account_auth](../account_auth/README.md) 第 4 条）；手动接口仍在，用于当场吊销。入口没有登出接口，不触发吊销；吊销不影响账号票据与进服票据，所以账号票据（`at2`，寿命 `account.ticket_ttl`）还在有效期内时仍能换到同一个游戏的新 gt2。封号在身份级：封一个人就是封他所有游戏的账号（login 模块的 `accountctl ban`）。
- **游戏账号 token 只回答“哪个账号、哪个游戏”**：不带区服、不带角色。登录带了 `game` 而游戏不在 `account.games` 里时整个登录失败（不交出账号票据）。要玩另一个游戏就带那个游戏重新登录，那是另一个账号号；`/portal/game-token` 只能给票据自己的游戏换。入口不核对 token 用在哪个大厅：大厅拿 `ValidateGameToken` 返回的游戏与自己服务的游戏比较（`GameAccount.RequireGame`）。同一张账号票据对同一个服反复 `enter` 不会多出玩家——映射的唯一性在 account。
- **区服状态在 `/portal/enter` 不拦**：维护中的服老号照常能进；新号被送去非 `open` 的服时拿得到进服票据，握手才被拒，而握手失败客户端拿不到原因。客户端要按清单里的 `status` 与 `role` 自己判断。
- **账号 ↔ 玩家号的映射在握手时才建**：拿了进服票据而没有去握手的新号，在 account 里没有角色，清单里仍是 `role: null`。反过来，握手过而没有在游戏服新建的账号，清单里已经有 `role`（“老号”），推荐规则把那个服当作他玩过的服；游戏服那边仍告知要新建。清单里的 `role` 说的是 account 的映射，不是游戏服的玩家数据。
- **地址可以是空的**：零配置时清单与进服结果里 `addr` 为空、`addr_source` 是 `none`，票据照发（`transport` 是 `tcp` 时，与以前相同）。`transport: ws` 不这样：没有 WebSocket 地址的服在清单里照常列出、`addr` 为空，但不会被推荐，进它答 `portal_transport_unavailable`——浏览器拿到空地址或 TCP 地址都连不上。
- **WebSocket 地址只有一个**：一个服（承载服）给一个 WebSocket URL，入口不在几个负载均衡地址之间挑；要多个入口时由负载均衡自己分。WebSocket 的握手、票据、限额与 TCP 相同，入口给的进服票据两种方式都能用。
- **动态地址的新鲜度**：game 每 `login.presence.interval`（缺省 10 秒）报一次，期限是三倍（30 秒）。进程死了、断网了，它的地址最多还会被发出去一个期限；正常停机时立刻撤销。上报者自报服号，account 不核对（总线只在内网，调用方可信）。没写 `login.presence.addr` 的 game 不上报，那个服只有静态地址。上报里的 WebSocket 地址（`login.presence.ws_addr`，或开着嵌入的 WebSocket 监听时由 `addr` 的主机加监听端口与路径得出）与 TCP 地址一起来、一起过期；不带 WebSocket 地址的上报（含旧进程的、Redis 里旧的条目）等同只支持 TCP，那个服的 WebSocket 地址仍取 `portal.websocket_servers`。
- **`portal.servers` / `portal.websocket_servers` / `portal.services` / `portal.cors_origins` 在 account 进程的配置声明里**（清单的 `config:`，D19 B，结构体是 `services/account/portal/portal_config.go` 的 `Config`）：`--print-config` 打印、`go run ./cmd/account --check-config` 与 `roost config check --service account` 按声明与 `ValidateConfig`（服号写法、地址带主机、WebSocket 地址是 ws / wss 的 URL、条数上限）检查，写错了在检查时就报、进程不启动；入口仍在 Start 里读它。这几个键都没有缺省值也没有 example，安装器不往配置里写段。
- **前提**：依赖 `account_auth`（account 的登录协作者；开发登录、渠道入口、玩家号计数器都在那里），不依赖 `login`：`/portal/enter` 给的握手票据 `enter:<进服票据>` 由入口自己拼（`EnterTicketPrefix`），与 login 的认证器认的前缀是两边各写一份的约定，两个都装着时由联动场景 `TestWithPortalLogin` 核对；握手那一侧要 login（或工程自己的认证器）。托管 account 的服务要有 web Mod（安装时代办）；工程里没有托管 account 的服务时安装器在同一次安装里声明它（带上 web Mod），名字 account 被业务服务占着时拒绝安装。入口挂在 account 进程上依赖生成的 bootstrap 里那一行 `kitmodulehost.NewMod(...)`，自己手写 bootstrap 的工程要照着加。account 的 `ListServers`、`ListServices`、`ListServerPresence`、`RoleOnServer`、`IssueAccountTicket`、`ValidateAccountTicket`、`IssueEnterTicket`、`IssueGameToken`、`EnterServer` 是总线接口：manifest 拆出去之后比 account 进程新时请求会失败，要先升 account。
- **已知限制**：[KNOWN-LIMITS“portal 的边界”](../../docs/maintenance/KNOWN-LIMITS.md)，以及同一处的“login 的边界”“game-demo 接入登录链路的边界”（demo 渠道经入口对外、生成依赖没发布的 `wiring/modulehost`）。

## 复用方式

**需求正好是它**

```sh
roost module plan portal
roost module add login portal enter   # 有 game 的工程：握手的认证器（login）、入口、进入游戏；三个都装着时多整条链路的场景
roost module verify
```

不带 game 的开房间工程（`-template lobby`）只装入口：

```sh
roost module add portal   # 连同 account_auth；account 没有 web Mod 时前置命令 roost add mod web -service account 先补上
# configs/service/config.account.yaml 末尾加 login.dev_token: dev-<随便什么>（开发登录）
roost project doctor      # collaborators:account 通过，0 FAIL
```

装它时 [account_auth](../account_auth/README.md) 跟着装（不再带 login：握手的认证器、玩家 TCP 接入、`cmd/accountctl` 要 `roost module add login`）；account 服务没有 web Mod 时安装器先补上（`roost add mod web -service <托管 account 的服务>`，连同 `web.*` 配置段，开发端口是 8080 + 那个服务的端口块号）；工程还没有 account 时（`-template game` 缺省不托管框架服务）安装器声明它，web 直接写进声明，配置生成时就带 `web.*` 段。零配置就能启动、登录、拿清单、换进服票据，只是没有连接地址。要走通整条链路还要做（`roost module plan / add` 的输出里列着，`todo:` 行）：

1. 写开发令牌（account_auth 的 `todo:`）；有 game 时再做 login 的那几步（[login 的“安装之后还要做的”](../login/README.md#复用方式)）：打开玩家 TCP 监听、登记区服。
2. 给区服配地址：在 game 服务的配置里写 `login.presence.addr`（game 定期上报，见“动态目录”），或在托管 account 的服务的配置里写静态的 `portal.servers`。用 `roost add gate` 改成独立 Gate 的工程，命令已把 `login.presence.addr` 写成 Gate 的地址（Gate 接过原来的客户端端口，已有的 `portal.servers` 不用改）；`roost project doctor` 的 gate:client-address 检查开发配置里这两处都等于 Gate 的 `player_access.gate.advertise_addr`。
3. 有浏览器客户端（`roost add transport websocket`，玩家接入写了 `player_access.{tcp,gate}.websocket.addr`）时，再给区服配 WebSocket 地址：game 配置写 `login.presence.ws_addr`（`ws://` / `wss://` 的完整 URL），或 account 配置写 `portal.websocket_servers`。嵌入 TCP 的 game 开着 WebSocket 监听、写了 `login.presence.addr` 时可以不写：上报取 `ws://<login.presence.addr 的主机>:<监听端口><路径，缺省 /ws>`（主机取给客户端的地址，不取监听的 `0.0.0.0`）。前面有终结 TLS 的负载均衡（`wss`）、端口被映射、或玩家接入在独立 Gate 上（game 看不到 Gate 的 WebSocket 配置）时要自己写。`roost add gate` 与 `make dev-run` 不写 WebSocket 地址（生成器也不写 `websocket.addr`，开监听时才写），doctor 不核对它。

**需求比它多一点**

- 区服地址要动态（扩缩容、上报负载）：缺省已经接上——game 配 `login.presence.addr` 就行；负载的刻度、标签要换时改 login 的 `game/managers/login_presence.go`，来源要换（服务发现）时改 `portal_rules.go` 的 `newDirectory`。
- 导量（按负载、标签、渠道分流）：改 `portal_rules.go` 的 `recommend`，规则数据也放在那个文件里。
- 登录渠道：不在这里，在 account_auth 模块留给工程的 `services/account/login_channels.go`。
- 限流、TLS：放在反向代理上。
- 清单要缓存或分页：把 manifest 拆成独立服务（见下），在自己的服务里包一层。
- 不要改的文件：`portal.go`（端点与“账号只取自票据”的检查）、`portal_directory.go`、`portal_config.go`。

**把 manifest 拆成独立服务**

入口用到的只有 account 在总线上的接口（`Login`、`IssueAccountTicket`、`ValidateAccountTicket`、`ListServers`、`ListServices`、`ListServerPresence`、`RoleOnServer`、`IssueEnterTicket`、`IssueGameToken`），不依赖“与 account 同进程”：

- 新建一个业务服务，`uses: [account]`，加 web Mod；在它的 `Init` 里调 `portal.Start(registry)`（它按能力名取 account——同进程时是本地服务，拆出去之后是总线客户端，代码不用改）。原来装在 account 进程上的那一行接线从 `roost.yaml` 的模块记录里拿掉（`modules.portal.runtime.framework`），再 `roost project sync`。
- 两个进程之间**不需要共享任何密钥**：票据是 account 的，manifest 每次都把票据交回 account 校验。manifest 的副本数与 account 的副本数互不相干。
- 动态地址的登记表在 account（共享存储）里，manifest 多副本、拆出去之后都读同一张，不用把上报送到每个副本。

**别的代码要用它的数据或动作**

- 运维或 Gate 要登记 / 撤销一个服的地址：调 account 的总线接口 `ReportServerPresence(ServerPresence{ServerID, Addr, Load, Tags}, ttlSeconds)` / `WithdrawServerPresence(serverID)`（与 game 的上报写同一张表，后到的为准）。
- game 服的业务代码没有理由调入口：入口在另一个进程里，账号与角色的事实直接问 account。

**不适合直接用、只适合借鉴的情形**

- 账号体系不在 roost 的 account 服务里（自有账号中心）：端点形状与“目录 + 推荐规则”的拆法可以借鉴，票据要换成自己的。
- 客户端不走 HTTP（登录也走长连接）：借鉴三步的顺序与“入口不持有密钥、每次把票据交回账号服校验”的做法。

## 联动

- `portal` + `login`（`with/login/services/account/portal/portal_login_test.go`）：**只有测试，不加运行时代码**。入口给的握手票据交给 login 真实的认证器：认得出、承载服 / 原服 / 新建标记与 account 一致、不带前缀不认。守的是入口与认证器各写一份的 `enter:` 前缀。
- `portal` + `enter`，**还要装着 login**（清单 `with.enter.requires: [login]`；三个里最后装上的那一个装上时写）（`with/enter/services/account/portal/portal_enter_test.go`）：**只有测试，不加运行时代码**。整条链路的场景：HTTP 登录 → 清单与推荐 → 进服票据 → 握手（login 的认证器）→ 游戏服告知要不要先新建 → 新建（角色名在游戏服）→ 进入；以及入口给的地址与进入时认的是同一个承载服。
- 别的模块没有 `with/portal`。握手之后的三步（`EnterStatus` / `EnterCreate` / `EnterGame`）属于 `login` + `enter` 的联动；没装 enter 的工程握手之后走自己的进入流程。

## 验证范围

- `roost module verify portal`：21 条场景（`services/account/portal`）；与 `login` 的联动 1 条，与 `enter`（还要 login）的联动 2 条。
- 场景是离线的：真实的 `account.Service`（内存存储，票据由它签、由它验）、真实的路由引擎（不开端口）；握手那一侧直接问 account 的 `EnterServer`（认证器做的就是这件事；联动场景里用 login 真实的认证器）；只有“account 给不出结论”“account 答非所问”用替身。与 enter 的联动里游戏服的档是内存替身。没有经真实的监听端口、总线、Redis 与 TCP。
- 覆盖：从渠道身份走到 account 认的进服票据、账号只取自票据（请求里夹带账号不起作用）、超长与畸形请求不去问 account、account 的拒绝与故障分开回答（400 带码 / 500）、account 答非所问时不往下传、清单按 account 与目录拼出、推荐先看角色再看最新的开放服、进服给票据与承载服地址、缺省目录读 account 的登记表（两个入口副本看到同一份、上报盖住配置、停报到期回到配置、撤销立刻生效、登记表问不到答 500）、坏目录条目被丢弃、零配置在 account 进程里启动、缺参数时构造失败；游戏账号 token 的两条拿法（`/portal/game-token` 与登录时带 `game`，账号只取自票据或渠道确认）与各种拒绝（游戏不在 `account.games`、票据无效或拿 gt2 / et1 冒充、封号、畸形请求不去问 account、account 问不到答 500、account 答了别的游戏的 token 答 500）；按服务列出（服务清单与 `portal.services` 的地址、服上的 `service`、按 `service` 筛选与筛选后的推荐、非 game 服务不能筛选、没有配置 `account.services` 的工程与以前一样）；浏览器跨域（`portal.cors_origins` 里的 Origin 四个端点预检 204、响应带 Allow-Origin，别的 Origin 预检 403、响应不带，不写时不允许跨域，浏览器发不出的写法在配置检查与启动时拒绝）；连接方式（2026-11-18：不写 `transport` 与写 `tcp` 相同、`ws` 拿 URL 且票据能握手、清单的 `transports`、推荐按方式过滤（老号的角色服没有 WebSocket 地址时改推有的服、工程规则推荐了不支持的服按没有推荐、一个都没有时为 0）、指定的服不支持答 `portal_transport_unavailable` 不回落、非法 `transport` 不去问 account、`/portal/login` 的 `enter` 同样按方式、`portal.websocket_servers` 的写法在启动时检查、上报的 WebSocket 地址与配置按方式叠、目录条目里坏的 WebSocket 地址整条丢掉）。CORS 头本身的规则在 core 的 `infra/network/httpserver`（`TestCORS*`）。登记表本身的过期 / 上限 / 校验在 core 的 `service/account`（`TestServerPresence*`，可走真实 Redis），game 一侧的上报在 login 模块（`TestLoginPresence*`）。
- 真实进程只做过一次单机演练（2026-11-12：一个 account 进程加一个嵌入 TCP 的 game 进程，整条链路与各种拒绝，记录在仓库 CHANGELOG）；game-demo 与两个配方另各做过 `make dev-run`、curl 三步与真实 TCP 的登录 / 新建 / 重连；game-demo 的这一段有可重复的脚本（仓库的 `scripts/test-demo-login.sh`：`make dev-run`、curl 三步、机器人 `-login portal` 新号与老号，最后经独立 Gate 再跑一遍）。已并入别的服的老角色也有脚本（`scripts/test-demo-merge.sh`：合服后原服 1001 的角色走三步，清单标出 `merged_into`、地址给承载服，1001 的进服票据在承载服 1000 上嵌入 TCP 与经独立 Gate 各握手一次，进到原角色、背包延续）。
- `scripts/test-demo-login.sh` 的 casual + naming 阶段增加两个 account 进程：A 签发账号票据，B 校验并选服、签发进服票据；停掉 A 后 B 独立完成同一角色的重连。两个 game sid 共用正式 Redis Directory，验证建角、重名拒绝与换名；嵌入 TCP 和双 Gate 都走真实入口。
- 没有验证：动态地址经真实进程（game 上报 → NATS → account → Redis → 入口）、换服中的角色、生产配置、跨机。可重复步骤见[登录线证据](../../docs/maintenance/evidence/v147-login-20261114.md)。

## 整条链路

```
POST /portal/login    {channel, open_id, credential}   -> 账号票据
POST /portal/servers  {account_ticket}                  -> 区服清单（地址、负载、本账号有没有角色）+ 推荐的服
POST /portal/enter    {account_ticket, server_id}       -> 握手票据 enter:<进服票据> + 该连的地址
（浏览器：三步都加 transport: "ws"，地址是 ws:// / wss:// 的 URL）
（开房间类游戏没有区服：POST /portal/game-token {account_ticket, game} -> 游戏账号 token，拿去大厅）
（三步并一步：POST /portal/login {…, enter: {}} 或 {…, enter: {server_id}} -> 账号票据 + 握手票据 + 该连的地址）
连那个地址（Gate，或直连的 game），握手帧出示握手票据    -> 认证器问 account：这个账号在这个服上是哪一名玩家，没有就新建映射
（game 推送 EnterStatus）                                -> 本服能不能进、要不要先新建（只是告知）
要新建：发 EnterCreate{name}                             -> game 建成这名玩家：建档、起名、初始发放
发 EnterGame                                             -> 进入
```

握手之后的三步属于 [login](../login/README.md#联动) 与 enter 两个模块（装了 enter 才有）。

四个角色各管什么：

| 角色 | 管什么 | 在这套实现里是谁 |
| --- | --- | --- |
| account（游戏的账号服，全局、无状态、可水平扩展） | 渠道身份 → 账号；**账号 id ↔ 角色 id** 的映射（一个账号在一个服上一名玩家）；全部票据的签发与校验；区服记录 | 托管的 `service/account`，状态在 Redis |
| manifest（清单） | 登录之后给清单与推荐（要问 account 才知道是新号还是旧号）、选定之后换进服票据与连接地址；无状态的 HTTP | 本模块，缺省与 account 同进程 |
| gate | 握手：把进服票据交给 account，得到玩家号与归属；握手成功与否只看 account，不等 game | login 模块的认证器（嵌入 TCP 时在 game 进程里，独立 Gate 时在 Gate 上） |
| game | 会话激活之后告诉客户端要不要先新建；新建（建档、**角色名**、初始发放）与进入各是一个请求 | enter 模块的 `EnterStatus` / `EnterCreate` / `EnterGame`，名字规则来自 profile / naming |

### 本机走一遍

各服务与依赖起来之后；入口在 account 进程的 `web.addr` 上，开发端口是 8080 + account 的端口块号（`roost.yaml` 的 `services.account.port_block`）：新建的 game 模板工程直接装 portal、以及两个配方里 account 是第一个托管服务，块号 1，即 `127.0.0.1:8081`；`dev-local` 是 `config.account.yaml` 里 `login.dev_token` 的值：

```sh
go run ./cmd/accountctl upsert-server -sid 1000        # make dev-run 会自动做这一步

curl -s -X POST http://127.0.0.1:8081/portal/login \
  -H 'Content-Type: application/json' \
  -d '{"channel":"dev","open_id":"alice","credential":"dev-local"}'
# {"ok":true,"account_ticket":"at2.…","account_ticket_expires_at":…}

curl -s -X POST http://127.0.0.1:8081/portal/servers \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"at2.…"}'
# {"ok":true,"recommended_server_id":1000,"transport":"tcp","servers":[{"server_id":1000,"name":"game-1000","status":"open","addr":"127.0.0.1:7000","addr_source":"directory","transports":["tcp"],"load":0,"role":null}]}

curl -s -X POST http://127.0.0.1:8081/portal/enter \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"at2.…","server_id":1000}'
# {"ok":true,"ticket":"enter:et1.…","server_id":1000,"serving_server_id":1000,"expires_at":…,"addr":"127.0.0.1:7000","addr_source":"directory","transport":"tcp"}

read -rsp "ticket: " ROOST_PLAYER_TOKEN; export ROOST_PLAYER_TOKEN; echo
go run ./cmd/playerprobe -addr 127.0.0.1:7000      # 只验证握手；之后的 EnterStatus / EnterCreate / EnterGame 要客户端（或 Robot）收发
```

## 浏览器跨域调用

网页里的游戏（或替游戏登录的外壳页面）与入口不同源时，浏览器在跨域 `POST` JSON 之前先发预检（`OPTIONS`）。入口对四个端点都登记了 `OPTIONS`：Origin 在 `portal.cors_origins` 里答 204（`Access-Control-Allow-Origin` 回写这个 Origin，`Access-Control-Allow-Methods: POST`，`Access-Control-Allow-Headers: Content-Type`，缓存 600 秒），否则 403；`POST` 的响应对名单里的 Origin 带 `Access-Control-Allow-Origin`，每个响应都带 `Vary: Origin`。不写 `portal.cors_origins` 时与以前一样只服务同源页面、原生客户端与服务端调用。

```yaml
portal:
  cors_origins:
    - https://play.example.com    # 示例：以调用入口的网页实际的 Origin 为准
```

预检路由用的是 web 路由登记的 `OPTIONS`（`webroute` 2026-11-18 起接受 GET、POST、OPTIONS），CORS 头的规则在 core 的 `infra/network/httpserver.CORS`；做法与大厅的 `lobby.cors_origins` 相同（入口另外不放行 `null` Origin、在读配置时检查 Origin 的写法）。

## 端点

都是 `POST`，请求与响应都是 JSON。请求体上限 16 KiB（超过答 413）；不认识的字段被忽略。成功是 200 与 `"ok": true`；失败的形状是 web Mod 的缺省映射：

| 情况 | HTTP | 响应体 |
| --- | --- | --- |
| 业务拒绝（入口的、或 account 的） | 400 | `{"ok":false,"code":<业务码>,"reason":"…"}`，客户端按 `code` 分支 |
| 请求体不是 JSON、类型不对 | 400 | `{"ok":false,"error":"decode request"}` |
| 请求体过大 | 413 | `{"ok":false,"error":"request body too large"}` |
| account 或区服目录给不出结论（不可达、超时、存储出错），或 account 的回答对不上问题 | 500 | `{"ok":false,"code":1,"reason":"server error"}`；原因只进服务端日志 |

account 的码（`service/account` 的常量）：`560111` 票据无效（过期、被改过、不是 account 签的、拿别的票据冒充——一律这一个码，重新登录）、`560102` 渠道拒绝了凭据（含开发登录没开、渠道没接）或账号被封、`560103` 账号不存在、`560104` 服没登记、`560120` 游戏不在 `account.games` 里。票据为空或超过 1024 字节时入口不去问 account，直接按 `560111` 回答。

### POST /portal/login

| 请求字段 | 上限 | 说明 |
| --- | --- | --- |
| `channel` | 32 字节 | 渠道。开发登录是 `dev`；其余是工程在 `services/account/login_channels.go` 接的 |
| `open_id` | 128 字节 | 渠道里的用户标识。只是提示：账号以渠道确认的身份为准——拿自己的凭据配上别人的 `open_id`，得到的仍是自己的账号 |
| `credential` | 8 KiB | 交给渠道校验的凭据（开发登录是 `login.dev_token` 的值）。不记日志、不回显 |
| `game` | 32 字节，可选 | 登录到哪个游戏（`account.games` 里的）。不写时是 account 的缺省游戏（`account.default_game`；`account.games` 只有一个游戏时就是它），没有缺省游戏时答 `560120`。写了就在答复里一并给一张这个游戏的游戏账号 token |
| `enter` | 可选 | 登录时顺带进服，省掉 `/portal/servers` 与 `/portal/enter` 两个来回：`{"server_id": 1001}` 进那个服；`{}`（不写 `server_id`）进推荐的服，与 `/portal/servers` 同一条推荐规则，可加 `"service"`（32 字节）只在那个 game 服务的服里挑。可加 `"transport"`（`tcp` 缺省、`ws`）：地址是这种方式的，进推荐的服时只在支持它的服里挑。服号为负、服号与 `service` 同时写、`service` 过长、`transport` 不是 `tcp` / `ws` 时答 `portal_request_invalid`，在问 account 之前就拒绝 |

响应：`{"ok": true, "account_ticket": "at2.…", "account_ticket_expires_at": 1800000600}`；带了 `game` 时另有 `"game_token": "gt2.…", "game_token_expires_at": 1800007200`。票据有效期 `account.ticket_ttl`（缺省 10 分钟），期内可以重复使用；过期了重新登录。游戏账号 token 的寿命是 `account.games.<游戏>.token_ttl`。`game` 不在 `account.games` 里时整个登录答 `560120`，不给账号票据。账号票据绑定登录的那个游戏。

带了 `enter` 时另有 `"enter": {"ticket": "enter:et1.…", "server_id": 1001, "serving_server_id": 1001, "expires_at": …, "addr": "…", "addr_source": "directory", "transport": "tcp"}`，字段与 `/portal/enter` 的响应相同（没有自己的 `ok`）；账号票据照样给，之后换服、断线后进服照常凭它调 `/portal/enter`。
- 推荐规则一个服也没推荐时登录照样成功，只是没有 `enter`：客户端改走 `/portal/servers` 让玩家自己选。
- 进服失败（服号不是 account 登记过的服 `560104`、`service` 不是 game 服务、写的服没有所请求方式的地址 `portal_transport_unavailable`、account 问不到）整个登录失败、不给账号票据，与 `game` 拿不到 token 时一样；渠道凭据若是一次性的，客户端要重新向渠道取。
- 进推荐的服时入口先算一遍清单（开销同一次 `/portal/servers`：每个服一次角色查询）。

### POST /portal/servers

请求 `{"account_ticket": "…"}`，可选 `"service": "slg"`（只列这个 game 服务的服）、`"transport": "ws"`（地址给 WebSocket 的，缺省 `tcp`）。响应（`transport` 是 `tcp` 时）：

```json
{
  "ok": true,
  "recommended_server_id": 1000,
  "transport": "tcp",
  "services": [
    {"name": "lobby", "kind": "lobby", "addr": "https://lobby.example.com"},
    {"name": "relay", "kind": "relay", "addr": ""},
    {"name": "slg", "kind": "game", "addr": ""}
  ],
  "servers": [
    {"server_id": 1000, "name": "game-1000", "service": "slg", "status": "open", "addr": "203.0.113.7:7000", "addr_source": "directory", "transports": ["tcp", "ws"], "load": 40, "tags": ["new"],
     "role": {"player_id": 100001, "serving_server_id": 1000, "created_at": 1800000000, "last_login_at": 1800000000}},
    {"server_id": 1001, "name": "game-1001", "service": "slg", "status": "closed", "merged_into": 1000, "addr": "203.0.113.7:7000", "addr_source": "directory", "transports": ["tcp", "ws"], "load": 40, "role": null}
  ]
}
```

- `services` 只在配置了 `account.services` 时有：account 的 `ListServices`（按名字升序），`kind` 是 `game` / `lobby` / `relay`，`addr` 来自配置 `portal.services`（没配是空串；game 服务的地址在各个区服上）。没有配置 `account.services` 的工程没有这一项、服上也没有 `service`，与以前完全相同。
- `servers` 是 account 登记过的全部区服（`ListServers`），按服号升序，至多 1024 个；写了 `service` 时只有那个 game 服务的，推荐也只在其中挑。`service` 不是 `account.services` 里的 game 服务（lobby、relay、不认识的名字，或者工程没有配置 `account.services`）答 `portal_request_invalid`。目录里有、account 没登记的服不列出。
- `service` 是这个服归属的 game 服务（account 的 `GameServer.Service`；记录没写时归到唯一的 game 服务）。所有 game 服务的区服共用一个全局号段。
- `status` 是 account 里的状态（`open` / `maintenance` / `full` / `closed`）。新号只进得了 `open` 的服；已有角色的在任何状态下都能进。
- `merged_into` 非 0：这个服已并入那个服。这里的老角色照常进，由那个服承载。
- `addr` 是进这个服该连的地址——它的**承载服**在目录里、所请求连接方式下的地址（`transport: ws` 时是 `wss://play.example.org/ws` 这样的 URL）；`addr_source` 是 `directory`，目录里没有时 `addr` 为空、`addr_source` 是 `none`。`transports` 是承载服在目录里有地址的连接方式（`tcp`、`ws`，按这个顺序；目录里没有时是 `[]`），客户端据此挑能连的服。`load`、`tags` 是目录给的，入口不解释。
- `transport` 回显这份清单的地址是哪种方式的。
- `role` 是本账号在这个服上的角色（account 的 `RoleOnServer`），没有是 `null`——这就是“新号还是旧号”。`transferring_to` 只在角色正在换服时有。
- `recommended_server_id` 是推荐规则挑的服，0 表示没有推荐。`transport: ws` 时推荐规则只看得到有 WebSocket 地址的服（入口过滤），一个都没有时是 0。

### POST /portal/enter

请求 `{"account_ticket": "…", "server_id": 1000}`，可选 `"transport": "ws"`（缺省 `tcp`）。响应：

| 字段 | 说明 |
| --- | --- |
| `ticket` | 握手帧里出示的票据 `enter:<进服票据>`，原样使用 |
| `server_id`、`serving_server_id` | 选中的服、承载它的服（选中的服并入了别的服时两者不同）。**客户端连的是承载服** |
| `expires_at` | 进服票据的过期时刻（Unix 秒，`account.session_ttl`，缺省 30 分钟）。过期前可以反复握手（断线重连），过期后重新要一张 |
| `addr`、`addr_source` | 承载服在所请求连接方式下的地址（`tcp` 是 `host:port`，`ws` 是 `ws://` / `wss://` 的 URL）。`tcp` 而目录里没有时为空、`none`——票据照发，只是入口不知道该连哪里；`ws` 而承载服没有 WebSocket 地址时整个请求答 `portal_transport_unavailable`、不给票据，不回落到 TCP |
| `transport` | 回显 `addr` 是哪种连接方式的：`tcp` 或 `ws` |

浏览器的例子：

```sh
curl -s -X POST http://127.0.0.1:8081/portal/enter \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"at2.…","server_id":1000,"transport":"ws"}'
# {"ok":true,"ticket":"enter:et1.…","server_id":1000,"serving_server_id":1000,"expires_at":…,"addr":"wss://play.example.org/ws","addr_source":"directory","transport":"ws"}
# 这个服没有 WebSocket 地址时：{"ok":false,"code":<portal_transport_unavailable 的码>,"reason":"the server has no address for the requested connection type"}
```

进服票据只绑定账号与服，与连接方式无关：同一张票据经 TCP 或 WebSocket 握手都行。这个账号在那个服上是哪一名玩家、要不要新建映射，是握手时认证器问 account 的事（`EnterServer`）。服号必须是 account 登记过的服，否则带着 `560104` 拒绝；被封的账号拿不到票据。

### POST /portal/game-token

请求 `{"account_ticket": "…", "game": "party"}`（`game` 必填，至多 32 字节）。响应 `{"ok": true, "game": "party", "game_token": "gt2.<过期秒>.party.<账号>.<代数>.<签名>", "expires_at": 1800007200}`。

- 账号只取自账号票据（account 在 `IssueGameToken` 里校验它）；被封的账号拿不到（`560102`）；`game` 不在 `account.games` 里、或不是票据的游戏答 `560120`（2026-11-18：不能拿 A 游戏的票据换 B 游戏的 token）。
- 寿命是 `account.games.<游戏>.token_ttl`（至多 24h），期内可以重复使用；过期了凭新的账号票据再换一张。
- 大厅请求里带它（方案约定 `Authorization: Bearer gt2.…`）；大厅不持有密钥，经 account 的 `ValidateGameToken` 校验，拿返回的游戏与自己服务的游戏比较。A 游戏的 token 进不了 B 游戏的大厅。
- 游戏账号 token 不能当账号票据用（拿清单、进服票据、再换 token 都答 `560111`），反过来也一样。

## 区服目录

“有哪些服、叫什么、开没开、并没并”是 account 的数据；“客户端连哪里、现在多忙”不是——那通常是 Gate 的地址，由部署决定、会随扩缩容变化。目录（`Directory`）是入口在这一半上的扩展点：

```go
type Entry struct {
    ServerID int32     // 区服号，正数
    Addr     string    // 客户端经 TCP 该连的地址 host:port，主机不能为空；空 = 没有 TCP 地址
    WSAddr   string    // 客户端经 WebSocket 该连的 ws:// / wss:// URL；空 = 没有 WebSocket 地址（两者至少一个）
    Load     int       // 多忙，刻度工程自己定，不小于 0
    Tags     []string  // 标签，至多 8 个、每个至多 32 字节
}

type Directory interface {
    Entries(ctx context.Context) ([]Entry, error)
}
```

- 入口每个要地址的请求都问一次目录，所以实现要快：读内存，或者自己带缓存；要并发安全。
- 返回错误 = “现在问不到”，入口答 500。
- 不成样子的条目（含两个地址都没有、`WSAddr` 不是 ws / wss 的 URL）与重复的服号（留第一个）由入口丢掉并记一条 Warn，那个服在清单里就是没有地址；坏数据不会让请求失败，也不会被带给客户端。至多认 1024 个条目。
- 用哪个目录在 `services/account/portal/portal_rules.go` 的 `newDirectory` 里选，**那个文件归工程所有**。

静态目录：配置 `portal.servers` 与 `portal.websocket_servers`（`NewStaticTransportDirectory`；只有 TCP 的 `NewStaticDirectory`）。一个服可以只出现在其中一边。静态数据写错（地址不成样子、服号不是正数、超过 1024 个服）在启动时就报错。

`Overlay` 按连接方式分开叠：同一个服在后一层有 TCP 地址就用它的、没有就留前一层的，WebSocket 地址同样；负载与标签取后一层的。

### 动态目录

动态地址的登记表在 **account** 里（维护者 D23）：game 进程经总线定期上报“本服现在连这个地址、这么忙”，account 放进一张带过期的登记表（`service/account` 的 `ReportServerPresence` / `WithdrawServerPresence` / `ListServerPresence`，Redis 里一个哈希，过期用 Redis 自己的时钟判断）；入口每个要地址的请求读一次（`NewPresenceDirectory`）。缺省目录是 `Overlay(静态配置, 登记表)`：

```
game（login 模块的 manager NewLoginPresence）
  每 login.presence.interval（缺省 10s）：ReportServerPresence({sid, login.presence.addr, ws_addr, 在线连接数, login.presence.tags}, 期限 = 三倍间隔)
  正常停机：WithdrawServerPresence(sid)
        │ NATS（account 的总线接口，投给任意一个 account 副本）
        ▼
account：登记表（Redis 哈希 <prefix>:presence，到期的不给出）
        ▲ ListServerPresence（每次 /portal/servers、/portal/enter）
入口（任意副本）：Overlay(portal.servers + portal.websocket_servers, 登记表) → 清单与进服结果的 addr / transports / load / tags
```

- 到期那一刻起条目不再给出：进程死了、断网了，它的地址最多还会被发出去一个期限（缺省 30 秒），之后那个服回到静态配置的地址（没有就没有地址）。
- 登记表在共享存储里：入口多副本、manifest 拆成独立服务，读到的都是同一张，不用把上报送到每个副本。
- game 一侧只要配 `login.presence.addr`（客户端连得到的 `host:port`，通常是 Gate；监听地址 `0.0.0.0` 与独立 Gate 部署时进程猜不出来，所以必须写）；不写就不报。WebSocket 地址是 `login.presence.ws_addr`（`ws://` / `wss://` 的 URL）；不写而本进程开着嵌入的 WebSocket 监听（`player_access.tcp.enabled` 为真、`player_access.tcp.websocket.addr` 非空）时取 `ws://<addr 的主机>:<监听端口><player_access.tcp.websocket.path，缺省 /ws>`，独立 Gate 部署（嵌入 TCP 关着）时得不出，要写。负载是那个 game 进程的在线连接数（独立 Gate 部署时是经 Gate 进来的会话数）。细节见 [login 的“区服入口上报”](../login/README.md)。
- 至多 1024 个没过期的服（满了之后新的服号被拒、已登记的照常续期）；期限至多 600 秒；条目的地址、负载、标签与目录条目同样的上限，account 在上报时就拒绝不成样子的。
- 别的来源：读服务发现时自己实现 `Directory`（`Entries` 里读发现结果并带上缓存——入口每个请求都会问一次；托管 account 的进程缺省没有 etcd Mod，要自己加；发现里登记的要是客户端连得到的地址），在 `newDirectory` 里换掉或叠上去。运维手工登记直接调 account 的 `ReportServerPresence`（期限至多 600 秒，要持续有效就写 `portal.servers`）。

## 推荐规则

`services/account/portal/portal_rules.go` 的 `recommend`，输入是清单（`ServerChoice`：每个服的状态、是否并入别的服、地址、负载、标签、本账号有没有角色与最近登录时间），输出推荐的服号。缺省规则：

- **老号**（至少在一个服有角色）：推荐最近登录过的那个角色所在的服；登录时间相同取服号大的。不看那个服现在的状态与负载。
- **新号**：在状态是 `open`、没有并入别的服的服里推荐服号最大的（最新开的）；有地址的服优先于没有地址的。

想按负载分流、按标签或按渠道导量，改这个函数；规则数据也放在这个文件里。规则返回一个不在交给它的服里的服号时入口按没有推荐处理。

客户端要 `ws` 时，入口只把有 WebSocket 地址的服交给 `recommend`（`ServerChoice.Addr` 是所请求方式的地址，`Transports` 是这个服支持的方式）：老号的角色所在的服没有 WebSocket 地址时，缺省规则改推有的服里最新开的那个（维护者 2026-11-18 定的“推荐只在支持所请求方式的服里挑”）。规则不用自己判断连接方式。

## 票据

入口不签也不验任何票据，全部交给 account（`service/account/tickets.go`、`enter.go`）：

| 票据 | 谁签 | 谁验 | 寿命 |
| --- | --- | --- | --- |
| 账号票据 `at2.…`（绑定游戏） | account 的 `IssueAccountTicket`（入口在 `Login` 之后调） | account 的 `ValidateAccountTicket`（入口每个带票据的请求都调） | `account.ticket_ttl`，缺省 10 分钟 |
| 进服票据 `et1.…`（握手时写作 `enter:et1.…`） | account 的 `IssueEnterTicket`（入口的 `/portal/enter` 调） | account 的 `EnterServer`（握手的认证器调） | `account.session_ttl`，缺省 30 分钟 |
| 游戏账号 token `gt2.…` | account 的 `IssueGameToken`（入口的 `/portal/game-token` 与带 `game` 的登录调） | account 的 `ValidateGameToken`（大厅调） | `account.games.<游戏>.token_ttl`，至多 24h |

三种票据与角色会话令牌用的是同一把 `account.session_secret`，各有各的域，互相冒充不了；换 `account.session_secret` 同时作废全部在外的票据。

## 配置

写在**托管 account 的服务**的配置里（`configs/service/config.account.yaml`），可以不写：

```yaml
portal:
  servers:                   # 静态的区服目录：服号 -> 客户端经 TCP 连得到的地址；至多 1024 个
    "1000": 203.0.113.7:7000
    "1001": 203.0.113.8:7000
  websocket_servers:         # 服号 -> 客户端经 WebSocket 连得到的完整 URL（浏览器用）；至多 1024 个
    "1000": wss://play.example.org/ws
  services:                  # 服务的地址：服务名（account.services 里的）-> 客户端连得到的地址；至多 64 个
    lobby: https://lobby.example.com
  cors_origins:              # 允许跨域调用的网页 Origin；不写 = 不允许跨域
    - https://www.example.com
```

| 键 | 缺省 | 说明 |
| --- | --- | --- |
| `portal.servers` | 空 | 服号 → 带主机的 `host:port`（通常是 Gate）。键必须是正的服号的规范十进制写法（`"+7"`、`"007"` 不算），只写端口的地址（`":7000"`）不行。有哪些服由 account 说了算，这里只给地址；写了一个 account 没登记的服不会让它出现在清单里 |
| `portal.websocket_servers` | 空 | 服号 → `ws://` 或 `wss://` 的完整 URL（有主机，浏览器要能解析：末段是数字的主机必须是点分十进制 IPv4，`192.0.2.300`、`game.1` 浏览器解析不了、`127.1` 浏览器会改写，都拒绝，方括号里是不带区域号的 IPv6，RV-20261118-13；写了端口时 1～65535；可带路径与查询串；不带用户名密码与 `#片段`；不含空白；至多 255 字节）。键的写法同 `portal.servers`。客户端请求 `transport: ws` 时给它；没写的服不支持 WebSocket。TLS 由前置负载均衡终结时写 `wss://负载均衡的主机/路径`。规则在 core 的 `svcaccount.CheckWebSocketAddr`，account 收上报与 login 的 `login.presence.ws_addr` 用同一条 |
| `portal.services` | 空 | 服务名 → 地址（非空、至多 255 字节、不含空白；形式由客户端与那个服务约定，入口不解释）。名字的写法同 `account.services`；写了一个 account 没列出的服务不会让它出现在清单里。与 `portal.servers` 一样在 account 进程的配置声明里（见“能力边界”），`--check-config` 能查出写法错误 |
| `portal.cors_origins` | 空（不允许跨域） | 允许在浏览器里跨域调用四个端点的网页 Origin：`scheme://host[:port]`，http 或 https，主机名小写，不带路径与结尾的 `/`，也不写默认端口（https 的 443、http 的 80：浏览器发的 Origin 不带它）。只收浏览器发出的那一种写法：端口不带前导零（`:08443` 浏览器发成 `:8443`，`:0443` 发成不带端口）、在 1～65535，数字主机写成点分十进制 IPv4（`127.1` 浏览器发成 `127.0.0.1`）或浏览器的 IPv6 压缩写法，浏览器解析不了的（`192.0.2.300`）不收。写成 `https://www.example.com/`、`https://www.example.com:443`、`https://www.example.com:08443` 这类浏览器发的 Origin 永远对不上，所以配置检查就拒绝，并给出该写的值（RV-20261118-08 / 10）；`"*"` 表示任何 Origin（`null` 除外）。在 App 的配置声明里（`--check-config` 查写法） |
| `account.games` / `account.default_game` / `account.services` | 生成的工程写一个以工程名命名的游戏 | account Mod 的键：这套 account 服务的游戏（`games.<游戏>.token_ttl`，至少一个）、登录不带游戏时落到哪个（只有一个游戏时不用写）、接受本账号体系的服务（`services.<服务>.kind`，不写时与只有 game 区服的工程相同） |
| `web.*` | 见 web Mod | 入口的监听地址、超时、请求体上限。开发配置的端口是 8080 + 这个服务的端口块号；生产环境 `web.addr` 不是回环地址时必须同时写 `web.allow_public_addr: true` |
| `account.ticket_ttl` | `10m` | 账号票据的有效期（account Mod 的键，不写进生成的配置） |
| `account.session_ttl` / `account.session_secret` | 见 account | 进服票据的有效期与全部票据的签名密钥 |

## 文件

| 文件 | 内容 |
| --- | --- |
| `services/account/portal/portal.go` | 启动入口 `Start`（清单的 `runtime`，在托管 account 的进程里启动）、四个端点、请求与响应的形状 |
| `services/account/portal/portal_rules.go` | **归工程所有**：用哪个区服目录（`newDirectory`）、推荐规则（`recommend`） |
| `services/account/portal/portal_directory.go` | 区服目录的接口、静态目录、account 登记表的目录（`NewPresenceDirectory`）、`Overlay` |
| `services/account/portal/portal_config.go` | `portal.servers`、`portal.websocket_servers`、`portal.services`、`portal.cors_origins` 的声明与启动时的检查 |
| `internal/errors/portal.go` | 入口自己的业务码 |
| `services/account/portal/portal_login_test.go` | 只在装了 login 时有：入口的握手票据交给 login 的认证器核对 |
| `services/account/portal/portal_enter_test.go` | 只在 enter 与 login 都装着时有：整条链路的场景（登录 → 清单 → 进服 → 握手 → 告知 → 新建 → 进入） |
