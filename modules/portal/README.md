# 登录入口 portal

`roost module add portal` 给工程一个**客户端够得着的登录入口**：三个 HTTP 端点，代客户端去问只在内部总线上的 account 服务。它依赖 [login](../login/README.md)（握手校验与 account 的 collaborators），装它时 login 跟着装；业务服务没有 web Mod 时安装器先补上（`roost add mod web -service <服务>`，连同 `web.*` 配置段）。

```sh
roost module plan portal
roost module add portal enter     # enter 不是依赖；两个都装着时多两条整条链路的场景
roost module verify
```

装了 login、portal、enter 之后，客户端从渠道身份走到“进入游戏”只需要：

```
POST /portal/login        {channel, open_id, credential}        -> account_ticket + 区服清单（每个服上自己的角色）
POST /portal/create-role  {account_ticket, server_id, name}     -> 角色            （清单里已有角色时跳过）
POST /portal/select-role  {account_ticket, player_id}           -> ticket + tcp_addr
连 tcp_addr，握手帧出示 ticket（session:<player_id>:<token>）   -> 认证通过
发 EnterGame                                                     -> 进入
```

入口自己不存任何东西，也不认证任何人：身份由 account 向渠道确认，角色归谁由 account 判断。入口只守一件事——见下面的“账号票据”。

## 安装之后

零配置就能用：路由随业务服务进程登记到 web Mod，监听 `web.addr`（开发配置缺省 `127.0.0.1:8080`）。要走通整条链路，还要做 login 的那几步（[login 的“安装之后还要做的”](../login/README.md#安装之后还要做的安装器不代办)）：打开玩家 TCP 监听、写开发令牌、登记区服。`roost module plan / add` 的输出里列着它们（`todo:` 行）。

本机走一遍（各服务与依赖起来之后；`dev-local` 是 `config.account.yaml` 里 `login.dev_token` 的值）：

```sh
go run ./cmd/accountctl upsert-server -sid 1000

curl -s -X POST http://127.0.0.1:8080/portal/login \
  -H 'Content-Type: application/json' \
  -d '{"channel":"dev","open_id":"alice","credential":"dev-local"}'
# {"ok":true,"account_ticket":"pt1.…","account_ticket_expires_at":…,"servers":[{"server_id":1000,"registered":true,"name":"game-1000","status":"open","role":null}]}

curl -s -X POST http://127.0.0.1:8080/portal/create-role \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"pt1.…","server_id":1000,"name":"Alice"}'
# {"ok":true,"role":{"player_id":100001,"name":"Alice","server_id":1000,"serving_server_id":1000,"created_at":…}}

curl -s -X POST http://127.0.0.1:8080/portal/select-role \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"pt1.…","player_id":100001}'
# {"ok":true,"ticket":"session:100001:…","player_id":100001,"server_id":1000,"serving_server_id":1000,"expires_at":…,"tcp_addr":"127.0.0.1:7000","tcp_addr_source":"request_host"}

read -rsp "ticket: " ROOST_PLAYER_TOKEN; export ROOST_PLAYER_TOKEN; echo
go run ./cmd/playerprobe -addr 127.0.0.1:7000      # 只验证握手；之后的 EnterGame 由客户端发
```

## 端点

都是 `POST`，请求与响应都是 JSON。请求体上限 16 KiB（超过答 413）；不认识的字段被忽略。成功是 200 与 `"ok": true`；失败的形状是 web Mod 的缺省映射：

| 情况 | HTTP | 响应体 |
| --- | --- | --- |
| 业务拒绝（入口的、或 account 的） | 400 | `{"ok":false,"code":<业务码>,"reason":"…"}`，客户端按 `code` 分支 |
| 请求体不是 JSON、类型不对 | 400 | `{"ok":false,"error":"decode request"}` |
| 请求体过大 | 413 | `{"ok":false,"error":"request body too large"}` |
| account 给不出结论（不可达、超时、存储出错），或它的回答对不上问题 | 500 | `{"ok":false,"code":1,"reason":"server error"}`；原因只进服务端日志 |

入口自己的业务码（码值安装时分配，见 `internal/errors/portal.go`）：

| 名字 | 含义 | 客户端该做什么 |
| --- | --- | --- |
| `portal_ticket_invalid` | 账号票据不能用：过期、被改过、不是这个入口的密钥签的、格式不对——一律这一个码 | 重新登录 |
| `portal_request_invalid` | 必填字段没填、字段超过长度上限、服号或玩家号不是正数 | 改请求；这样的请求没有去问 account |

account 的拒绝原样带着它的码回来（`service/account` 的常量），常见的：`560102` 渠道拒绝了凭据（含开发登录没开、渠道没接、账号被封）、`560104` 服没登记、`560105` 服不是 open 或已并入别的服、`560106` 名字不合规则、`560107` 名字被占、`560108` 这个服上已有角色（或另一个名字的建角还没完成）、`560109` 没有这个角色、`560110` 不是你的角色、`560113` 存储竞争或记录在对账，重试。

### POST /portal/login

| 请求字段 | 上限 | 说明 |
| --- | --- | --- |
| `channel` | 32 字节 | 渠道。开发登录是 `dev`；其余是工程在 `internal/service/account/login_channels.go` 接的 |
| `open_id` | 128 字节 | 渠道里的用户标识。只是提示：账号以渠道确认的身份为准 |
| `credential` | 8 KiB | 交给渠道校验的凭据（开发登录是 `login.dev_token` 的值）。不记日志、不回显 |

响应：

```json
{
  "ok": true,
  "account_ticket": "pt1.1800000600.ZGV2OmFsaWNl.…",
  "account_ticket_expires_at": 1800000600,
  "servers": [
    {"server_id": 1000, "registered": true, "name": "game-1000", "status": "open",
     "role": {"player_id": 100001, "name": "Alice", "server_id": 1000, "serving_server_id": 1000, "created_at": 1800000000}},
    {"server_id": 1001, "registered": true, "name": "game-1001", "status": "closed", "merged_into": 1000, "role": null}
  ]
}
```

- `servers` 按服号升序，是 `portal.servers` 里配置的服；没有配置时只有本进程自己的服。
- `registered` 为 false：清单里有、account 里没登记（没跑过 `accountctl upsert-server`），这样的服建不了角。
- `status` 是 account 里的状态（`open` / `maintenance` / `full` / `closed`）。只有 `open` 能建角；已有角色在任何状态下都能选。
- `merged_into` 非 0：这个服已并入那个服。这里的角色照常选，由那个服承载；不能在这里建新角色。
- `role` 是本账号在这个服上的角色（account 的 `RoleOnServer`），没有是 `null`。换了设备、重装之后客户端就是靠它认回自己的角色。`transferring_to` 只在角色正在换服时有。
- 清单里任何一个服问不到（不是“没登记”“没有角色”，而是 account 出错）整个登录失败，不给一份缺了角色的清单。

### POST /portal/create-role

请求 `{"account_ticket": "…", "server_id": 1000, "name": "Alice"}`（`name` 至多 128 字节；真正的名字规则是 account 的 `NameRules`）。响应 `{"ok": true, "role": {…}}`，`role` 的形状同上。

同一个名字重试是安全的：account 把它接到同一个建角计划上，回同一个角色（回复丢了的那一次也算）。换一个名字不会多得一个角色。不限制 `server_id` 必须在清单里——能不能在那个服建角由 account 的区服记录决定。

### POST /portal/select-role

请求 `{"account_ticket": "…", "player_id": 100001}`。响应：

| 字段 | 说明 |
| --- | --- |
| `ticket` | 握手帧里出示的票据 `session:<player_id>:<token>`，原样使用 |
| `player_id`、`server_id`、`serving_server_id` | 角色、它绑定的服（原服）、现在承载它的服。**客户端要连的是承载服** |
| `expires_at` | 握手票据的过期时刻（Unix 秒，`account.session_ttl`，缺省 30 分钟）。过期前可以反复握手（断线重连），过期后重新选角 |
| `tcp_addr` | 承载服的玩家 TCP 地址 `host:port`；可能是空串 |
| `tcp_addr_source` | 地址怎么来的；地址为空时为什么没有（下表） |
| `transferring_to` | 只在角色正在换服时有：目标服。票据照发，进入游戏时由 rehome 模块决定放不放行 |

| `tcp_addr_source` | 含义 |
| --- | --- |
| `config` | `portal.servers` 里给承载服写的地址 |
| `listen` | 没有配置清单，承载服是本服：本进程玩家 TCP 的监听地址（`player_access.tcp.addr`），它绑在一个具体的主机上 |
| `request_host` | 同上，但监听绑的是通配地址（生成的配置缺省 `0.0.0.0:7000`）：主机取客户端访问入口用的那个（HTTP 的 Host），端口取监听端口 |
| `unlisted_server` | 地址为空：配置了清单而承载服不在里面，或者没有配置而承载服不是本服（合服、换服之后） |
| `tcp_disabled` | 地址为空：承载服是本服，但玩家 TCP 监听没开（`roost config enable player-tcp`） |
| `unknown` | 地址为空：监听绑通配地址，而请求的 Host 不是一个能用的主机名 |

地址为空时选角照样成功——票据是好的，只是入口不知道那个服的地址。拿票据连错了服，握手会通过，进入游戏时被 login 的进入前检查以 `login_elsewhere` 拒绝。

## 账号票据

account 的 `CreateRole` / `SelectRole` 把 accountID 当作“可信调用方已经认证过的身份”。入口就是那个调用方，所以**它从不从请求里读 accountID**：登录成功后签发一张短期的账号票据，后两个端点只认票据里的账号。

```
pt1.<过期时刻 Unix 秒>.<base64url(accountID)>.<base64url(HMAC-SHA256(密钥, 前三段))>
```

- 账号取自 account 的 `Login` 返回的账号（渠道确认过的身份），不是请求里的 `open_id`。
- 签名覆盖版本、过期时刻与账号；校验先验签名（定长时间比较）再看内容。过期、被改、别的密钥签的、格式不对都是同一个 `portal_ticket_invalid`，响应一模一样。
- 有效期 `portal.ticket_ttl`，缺省 10 分钟，按服务器的系统时间算（不跟业务时间的偏移走）。
- 票据不是一次性的：有效期内可以反复用（建角之后接着选角、选角重试）。它不落库、不能单张吊销；要让在外的票据立刻全部作废，换 `portal.ticket_secret`。
- 票据、凭据、握手令牌不进日志，也不进错误文本。

密钥 `portal.ticket_secret`：

| 配置 | 行为 |
| --- | --- |
| 没写（开发配置） | 每个进程启动时随机生成一把并记一条 Info。进程重启后在外的票据失效；另一个进程不认这张票据 |
| 写了、不足 32 字节 | 进程不启动（不分环境） |
| 生产配置（`env` / `app.env` / `environment` 是 `prod` / `production`）里为空，或以 `dev-` 开头 | 进程不启动 |
| 生产配置、32 字节以上 | 启动。同一个入口地址后面的多个进程必须用同一把 |

拿 A 账号的票据选 B 账号的角色：account 核对存储里角色的归属，答 `560110`，入口原样拒绝，B 的角色没有被碰。

## 配置

写在业务服务的配置里（`configs/service/config.<服务>.yaml`），全部可以不写：

```yaml
portal:
  ticket_secret: ""          # 见上；生产必填，至少 32 字节
  ticket_ttl: 10m            # 10s ～ 24h
  servers:                   # 区服清单：服号 -> 客户端连得到的玩家 TCP 地址；至多 64 个
    "1000": 203.0.113.7:7000
    "1001": 203.0.113.8:7000
```

| 键 | 缺省 | 说明 |
| --- | --- | --- |
| `portal.ticket_secret` | 空 | 账号票据的签名密钥 |
| `portal.ticket_ttl` | `10m` | 账号票据的有效期 |
| `portal.servers` | 空 | 配置了就以它为准：登录时列这些服，选角时只给这里写的地址。键是正的服号，值是带主机的 `host:port` |
| `web.*` | 见 web Mod | 监听地址、超时、请求体上限。生产环境 `web.addr` 不是回环地址时必须同时写 `web.allow_public_addr: true` |
| `player_access.tcp.enabled` / `.addr` | 见玩家 TCP | 入口只读它们，用来在没有配置清单时回答本服的地址 |

`portal.*` 由入口在启动时自己读（`game/portal` 的 `Start`），不在 App 的配置声明里：`--print-config` 不打印这一段，`--check-config` 与 `roost config check` 不检查它，写错了要等进程启动到入口这一步才报错退出。

## 文件

| 文件 | 内容 |
| --- | --- |
| `game/portal/portal.go` | 启动入口 `Start`（清单的 `runtime`）、三个端点、请求与响应的形状 |
| `game/portal/portal_ticket.go` | 账号票据的签发与校验 |
| `game/portal/portal_servers.go` | 区服清单与“该连哪个地址” |
| `game/portal/portal_config.go` | `portal.*` 的声明与启动时的检查 |
| `internal/errors/portal.go` | 两个业务码 |
| `game/portal/portal_enter_test.go` | 只在装了 enter 时有：整条链路的场景 |

握手票据由 `game/login` 的 `Ticket` 拼，入口不另写一遍格式。

## 边界

- **没有限流，也没有防撞库**。三个端点谁都能调；`/portal/login` 每次都会去问渠道。对外暴露时放在有限流的反向代理后面。
- **明文**。凭据与票据都在请求体里；web Mod 不终止 TLS。生产环境 web Mod 缺省只绑回环地址，对外要放在终止 TLS 的代理后面并显式写 `web.allow_public_addr: true`。生成的生产示例配置里 `web.addr` 是占位值，上线前要改。
- **`request_host` 是猜的**。入口在反向代理、负载均衡后面时 Host 是代理的主机，玩家 TCP 未必在那里；上线的部署写 `portal.servers`。这个主机只回给发请求的客户端自己。
- **入口跟着业务服务进程走**。清单里别的服的状态与角色都是向 account 问的，与那个服的进程是否活着无关；入口不知道某个服此刻连不连得上。
- **登录的开销随清单长度增长**：每个服问 account 两次（区服记录、角色），顺序执行。清单上限 64；区服更多的工程要自己做带分页的区服列表。
- **封号不让已签发的票据失效**。account 在 `Login` 时拒绝被封的账号，但封号之前签出去的账号票据在有效期内仍能选角（account 的 `SelectRole` 不查封号），握手令牌同理到 `account.session_ttl`。要立刻踢人用 `entity.kick`，要让全部在外的令牌作废，轮换密钥。
- **建角计划还没发布的服在清单里是 `role: null`**。上一次建角的回复丢了、角色还没发布时，清单看不到它；用原来的名字再建一次会接上，换一个名字在原计划还能完成时得到 `560108`。
- 场景是离线的：真实的 `account.Service`（内存存储）、真实的路由引擎与登录模块的认证器，没有经真实的监听端口、总线、Redis 与 TCP。
