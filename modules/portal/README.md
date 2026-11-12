# 登录入口 portal

`roost module add portal` 给工程一个**客户端够得着的登录入口**：三个 HTTP 端点，代客户端去问只在内部总线上的 account 服务。它就是常说的 manifest（清单）那一层，缺省与 account 跑在同一个进程里——入口挂在**托管 account 服务的进程**上，不挂在某个 game 服上。

它依赖 [login](../login/README.md)（握手时把票据交给 account 的认证器），装它时 login 跟着装；account 服务没有 web Mod 时安装器先补上（`roost add mod web -service <托管 account 的服务>`，连同 `web.*` 配置段）。业务服务（game）不因此多出任何 HTTP 监听。

```sh
roost module plan portal
roost module add portal enter     # enter 不是依赖；两个都装着时多两条整条链路的场景
roost module verify
```

## 整条链路

```
POST /portal/login    {channel, open_id, credential}   -> 账号票据
POST /portal/servers  {account_ticket}                  -> 区服清单（地址、负载、本账号有没有角色）+ 推荐的服
POST /portal/enter    {account_ticket, server_id}       -> 握手票据 enter:<进服票据> + 该连的地址
连那个地址（Gate，或直连的 game），握手帧出示握手票据    -> 认证器问 account：这个账号在这个服上是哪一名玩家，没有就新建映射
（game 推送 EnterStatus）                                -> 本服能不能进、要不要先新建（只是告知）
要新建：发 EnterCreate{name}                             -> game 建成这名玩家：建档、起名、初始发放
发 EnterGame                                             -> 进入
```

握手之后的三步属于 [login](../login/README.md#与-enter-一起装) 与 enter 两个模块（装了 enter 才有）；没装 enter 的工程握手之后走自己的进入流程。

四个角色各管什么：

| 角色 | 管什么 | 在这套实现里是谁 |
| --- | --- | --- |
| account（游戏的账号服，全局、无状态、可水平扩展） | 渠道身份 → 账号；**账号 id ↔ 角色 id** 的映射（一个账号在一个服上一名玩家）；全部票据的签发与校验；区服记录 | 托管的 `service/account`，状态在 Redis |
| manifest（清单） | 登录之后给清单与推荐（要问 account 才知道是新号还是旧号）、选定之后换进服票据与连接地址；无状态的 HTTP | 本模块，缺省与 account 同进程 |
| gate | 握手：把进服票据交给 account，得到玩家号与归属；握手成功与否只看 account，不等 game | login 模块的认证器（嵌入 TCP 时在 game 进程里，独立 Gate 时在 Gate 上） |
| game | 会话激活之后告诉客户端要不要先新建；新建（建档、**角色名**、初始发放）与进入各是一个请求 | enter 模块的 `EnterStatus` / `EnterCreate` / `EnterGame`，名字规则来自 profile / naming |

入口是很薄的一层：HTTP 入参检查、调 account、区服目录与推荐规则。它不认证任何人（身份由 account 向渠道确认），**不持有任何密钥**（票据由 account 签发与校验），不建角、不选角，也不知道玩家号。它只守一件事：**账号只取自 account 校验过的账号票据**，绝不取自请求。

### 把 manifest 拆成独立服务

入口用到的只有 account 在总线上的接口（`Login`、`IssueAccountTicket`、`ValidateAccountTicket`、`ListServers`、`RoleOnServer`、`IssueEnterTicket`），不依赖“与 account 同进程”：

- 新建一个业务服务，`uses: [account]`，加 web Mod；在它的 `Init` 里调 `portal.Start(registry)`（它按能力名取 account——同进程时是本地服务，拆出去之后是总线客户端，代码不用改）。原来装在 account 进程上的那一行接线从 `roost.yaml` 的模块记录里拿掉（`modules.portal.runtime.framework`），再 `roost project sync`。
- 两个进程之间**不需要共享任何密钥**：票据是 account 的，manifest 每次都把票据交回 account 校验。manifest 的副本数与 account 的副本数互不相干。
- 区服目录如果用的是进程内的注册表，上报要送到 manifest 的每个副本（见下面“动态目录”）。

## 安装之后

零配置就能启动、登录、拿清单、换进服票据——只是没有连接地址（清单与进服结果里 `addr` 为空）。要走通整条链路：

1. login 的那几步（[login 的“安装之后还要做的”](../login/README.md#安装之后还要做的安装器不代办)）：打开玩家 TCP 监听、写开发令牌、登记区服。
2. 给区服配地址：在托管 account 的服务的配置里写 `portal.servers`（见“配置”），或者接上动态目录。

`roost module plan / add` 的输出里列着它们（`todo:` 行）。

本机走一遍（各服务与依赖起来之后；入口在 account 进程的 `web.addr` 上，新建的 game 模板工程是 `127.0.0.1:8081`；`dev-local` 是 `config.account.yaml` 里 `login.dev_token` 的值）：

```sh
go run ./cmd/accountctl upsert-server -sid 1000        # make dev-run 会自动做这一步

curl -s -X POST http://127.0.0.1:8081/portal/login \
  -H 'Content-Type: application/json' \
  -d '{"channel":"dev","open_id":"alice","credential":"dev-local"}'
# {"ok":true,"account_ticket":"at1.…","account_ticket_expires_at":…}

curl -s -X POST http://127.0.0.1:8081/portal/servers \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"at1.…"}'
# {"ok":true,"recommended_server_id":1000,"servers":[{"server_id":1000,"name":"game-1000","status":"open","addr":"127.0.0.1:7000","addr_source":"directory","load":0,"role":null}]}

curl -s -X POST http://127.0.0.1:8081/portal/enter \
  -H 'Content-Type: application/json' \
  -d '{"account_ticket":"at1.…","server_id":1000}'
# {"ok":true,"ticket":"enter:et1.…","server_id":1000,"serving_server_id":1000,"expires_at":…,"addr":"127.0.0.1:7000","addr_source":"directory"}

read -rsp "ticket: " ROOST_PLAYER_TOKEN; export ROOST_PLAYER_TOKEN; echo
go run ./cmd/playerprobe -addr 127.0.0.1:7000      # 只验证握手；之后的 EnterStatus / EnterCreate / EnterGame 要客户端（或 Robot）收发
```

## 端点

都是 `POST`，请求与响应都是 JSON。请求体上限 16 KiB（超过答 413）；不认识的字段被忽略。成功是 200 与 `"ok": true`；失败的形状是 web Mod 的缺省映射：

| 情况 | HTTP | 响应体 |
| --- | --- | --- |
| 业务拒绝（入口的、或 account 的） | 400 | `{"ok":false,"code":<业务码>,"reason":"…"}`，客户端按 `code` 分支 |
| 请求体不是 JSON、类型不对 | 400 | `{"ok":false,"error":"decode request"}` |
| 请求体过大 | 413 | `{"ok":false,"error":"request body too large"}` |
| account 或区服目录给不出结论（不可达、超时、存储出错），或 account 的回答对不上问题 | 500 | `{"ok":false,"code":1,"reason":"server error"}`；原因只进服务端日志 |

入口自己的业务码只有一个（码值安装时分配，见 `internal/errors/portal.go`）：`portal_request_invalid`——必填字段没填、字段超过长度上限、服号不是正数；这样的请求没有去问 account。

其余的拒绝都是 account 的码原样带回（`service/account` 的常量）：`560111` 票据无效（过期、被改过、不是 account 签的、拿别的票据冒充——一律这一个码，重新登录）、`560102` 渠道拒绝了凭据（含开发登录没开、渠道没接）或账号被封、`560103` 账号不存在、`560104` 服没登记。

### POST /portal/login

| 请求字段 | 上限 | 说明 |
| --- | --- | --- |
| `channel` | 32 字节 | 渠道。开发登录是 `dev`；其余是工程在 `internal/service/account/login_channels.go` 接的 |
| `open_id` | 128 字节 | 渠道里的用户标识。只是提示：账号以渠道确认的身份为准 |
| `credential` | 8 KiB | 交给渠道校验的凭据（开发登录是 `login.dev_token` 的值）。不记日志、不回显 |

响应只有账号票据：`{"ok": true, "account_ticket": "at1.…", "account_ticket_expires_at": 1800000600}`。票据由 account 签发（`IssueAccountTicket`），有效期 `account.ticket_ttl`（缺省 10 分钟），期内可以重复使用；过期了重新登录。

### POST /portal/servers

请求 `{"account_ticket": "…"}`。响应：

```json
{
  "ok": true,
  "recommended_server_id": 1000,
  "servers": [
    {"server_id": 1000, "name": "game-1000", "status": "open", "addr": "203.0.113.7:7000", "addr_source": "directory", "load": 40, "tags": ["new"],
     "role": {"player_id": 100001, "serving_server_id": 1000, "created_at": 1800000000, "last_login_at": 1800000000}},
    {"server_id": 1001, "name": "game-1001", "status": "closed", "merged_into": 1000, "addr": "203.0.113.7:7000", "addr_source": "directory", "load": 40, "role": null}
  ]
}
```

- `servers` 是 account 登记过的全部区服（`ListServers`），按服号升序，至多 256 个。目录里有、account 没登记的服不列出。
- `status` 是 account 里的状态（`open` / `maintenance` / `full` / `closed`）。新号只进得了 `open` 的服；已有角色的在任何状态下都能进。
- `merged_into` 非 0：这个服已并入那个服。这里的老角色照常进，由那个服承载。
- `addr` 是进这个服该连的地址——它的**承载服**在目录里的地址；`addr_source` 是 `directory`，目录里没有时 `addr` 为空、`addr_source` 是 `none`。`load`、`tags` 是目录给的，入口不解释。
- `role` 是本账号在这个服上的角色（account 的 `RoleOnServer`），没有是 `null`——这就是“新号还是旧号”。角色名不在这里，名字是游戏服的数据。`transferring_to` 只在角色正在换服时有。
- `recommended_server_id` 是推荐规则挑的服，0 表示没有推荐。
- account、目录、任何一个服的角色记录问不到（不是“没有角色”，而是出错），整个请求失败，不给一份缺了角色或缺了地址的清单。

### POST /portal/enter

请求 `{"account_ticket": "…", "server_id": 1000}`。响应：

| 字段 | 说明 |
| --- | --- |
| `ticket` | 握手帧里出示的票据 `enter:<进服票据>`，原样使用 |
| `server_id`、`serving_server_id` | 选中的服、承载它的服（选中的服并入了别的服时两者不同）。**客户端连的是承载服** |
| `expires_at` | 进服票据的过期时刻（Unix 秒，`account.session_ttl`，缺省 30 分钟）。过期前可以反复握手（断线重连），过期后重新要一张 |
| `addr`、`addr_source` | 承载服的地址；目录里没有时为空、`none`——票据照发，只是入口不知道该连哪里 |

入口在这一步**不建角、不选角**：进服票据只绑定账号与服（account 的 `IssueEnterTicket`）。这个账号在那个服上是哪一名玩家、要不要新建映射，是握手时认证器问 account 的事（`EnterServer`）。所以：

- 服号必须是 account 登记过的服，否则带着 `560104` 拒绝；被封的账号拿不到票据。
- 区服的状态这里不拦：维护中的服，老号照常能进；新号拿得到票据，但握手会被 account 按建角的规则拒绝（连接被关掉）。客户端该按清单里的 `status` 与 `role` 自己判断，不要把新号送去非 `open` 的服。
- 同一张账号票据对同一个服反复 `enter` 不会多出玩家：映射的唯一性在 account。

## 区服目录

“有哪些服、叫什么、开没开、并没并”是 account 的数据；“客户端连哪里、现在多忙”不是——那通常是 Gate 的地址，由部署决定、会随扩缩容变化。目录（`Directory`）是入口在这一半上的扩展点：

```go
type Entry struct {
    ServerID int32     // 区服号，正数
    Addr     string    // 客户端该连的地址 host:port，主机不能为空
    Load     int       // 多忙，刻度工程自己定，不小于 0
    Tags     []string  // 标签，至多 8 个、每个至多 32 字节
}

type Directory interface {
    Entries(ctx context.Context) ([]Entry, error)
}
```

- 返回错误 = “现在问不到”，入口答 500。
- 不成样子的条目与重复的服号（留第一个）由入口丢掉并记一条 Warn，那个服在清单里就是没有地址；坏数据不会让请求失败，也不会被带给客户端。至多认 256 个条目。
- 用哪个目录在 `game/portal/portal_rules.go` 的 `newDirectory` 里选，**那个文件归工程所有**。

缺省是静态目录：配置 `portal.servers`（`NewStaticDirectory`）。

### 动态目录

`Registry` 是一张进程内的、带过期的注册表：

```go
dynamic := portal.NewRegistry(nil)
err := dynamic.Announce(portal.Entry{ServerID: 1000, Addr: "203.0.113.7:7000", Load: 137}, 30*time.Second) // 登记或续期
dynamic.Withdraw(1000)                                                                                      // 正常下线
directory := portal.Overlay(configured, dynamic)                                                            // 登记的盖住配置的，过期之后回到配置
```

到期那一刻起条目不再给出（进程死了、网络断了，它的地址自己从清单里消失）；期限至多 10 分钟，条目至多 256 个，过期的不占名额；并发安全。**这里只有机制，登记从哪里来由工程接**，入口不替工程订阅任何东西。三种常见接法：

| 接法 | 怎么做 | 注意 |
| --- | --- | --- |
| Gate / game 定时经内部总线上报 | 在 `newDirectory` 里用 `registry` 取总线，订阅工程自己的主题；收到一条就 `Announce`（期限取上报间隔的两三倍），下线通知调 `Withdraw`；返回的 `stop` 里取消订阅 | 入口多副本时用广播主题，不要用只投给一个副本的队列组 |
| 读服务发现 | 不用注册表，自己实现 `Directory`：`Entries` 里读发现结果并带上缓存（入口每个请求都会问一次） | 托管 account 的进程缺省没有 etcd Mod，要自己加（`roost add mod etcd -service <服务>`）；发现里登记的要是客户端连得到的地址，不是内网地址 |
| 运维手工登记 | 把 `Announce` / `Withdraw` 挂到工程的运维入口上（例如 ops 的 admin 命令） | 手工登记没有心跳，期限要给足，或者干脆写 `portal.servers` |

**注册表是每个进程一张**。入口多副本时每个副本各有各的表：上报要让每个副本都收到，否则同一个客户端两次请求打到两个副本，看到的清单不一样。不想这样就实现一个读共享存储（Redis、服务发现）的 `Directory`。

## 推荐规则

`game/portal/portal_rules.go` 的 `recommend`，输入是清单（每个服的状态、是否并入别的服、地址、负载、标签、本账号有没有角色与最近登录时间），输出推荐的服号。缺省规则：

- **老号**（至少在一个服有角色）：推荐最近登录过的那个角色所在的服；登录时间相同取服号大的。不看那个服现在的状态与负载。
- **新号**：在状态是 `open`、没有并入别的服的服里推荐服号最大的（最新开的）；有地址的服优先于没有地址的。

想按负载分流、按标签或按渠道导量，改这个函数；规则数据也放在这个文件里。规则返回一个不在清单里的服号时入口按没有推荐处理。

## 票据

入口不签也不验任何票据，全部交给 account（`service/account/tickets.go`、`enter.go`）：

| 票据 | 谁签 | 谁验 | 寿命 |
| --- | --- | --- | --- |
| 账号票据 `at1.…` | account 的 `IssueAccountTicket`（入口在 `Login` 之后调） | account 的 `ValidateAccountTicket`（入口每个带票据的请求都调） | `account.ticket_ttl`，缺省 10 分钟 |
| 进服票据 `et1.…`（握手时写作 `enter:et1.…`） | account 的 `IssueEnterTicket`（入口的 `/portal/enter` 调） | account 的 `EnterServer`（握手的认证器调） | `account.session_ttl`，缺省 30 分钟 |

两种票据与角色会话令牌用的是同一把 `account.session_secret`，各有各的域，互相冒充不了；都是无状态的签名，期内可重复使用、不能单张吊销，换 `account.session_secret` 同时作废全部在外的票据。封号对已经发出去的票据立即生效：校验账号票据与凭票进服时 account 都会再读一次账号。

## 配置

写在**托管 account 的服务**的配置里（`configs/service/config.account.yaml`），可以不写：

```yaml
portal:
  servers:                   # 静态的区服目录：服号 -> 客户端连得到的地址；至多 256 个
    "1000": 203.0.113.7:7000
    "1001": 203.0.113.8:7000
```

| 键 | 缺省 | 说明 |
| --- | --- | --- |
| `portal.servers` | 空 | 服号 → 带主机的 `host:port`（通常是 Gate）。有哪些服由 account 说了算，这里只给地址；写了一个 account 没登记的服不会让它出现在清单里 |
| `web.*` | 见 web Mod | 入口的监听地址、超时、请求体上限。开发配置的端口是 8080 + 这个服务的端口块号；生产环境 `web.addr` 不是回环地址时必须同时写 `web.allow_public_addr: true` |
| `account.ticket_ttl` | `10m` | 账号票据的有效期（account Mod 的键，不写进生成的配置） |
| `account.session_ttl` / `account.session_secret` | 见 account | 进服票据的有效期与全部票据的签名密钥 |

`portal.servers` 由入口在启动时自己读（`game/portal` 的 `Start`），不在 App 的配置声明里：`--print-config` 不打印这一段，`--check-config` 与 `roost config check` 不检查它，写错了要等进程启动到入口这一步才报错退出。

## 文件

| 文件 | 内容 |
| --- | --- |
| `game/portal/portal.go` | 启动入口 `Start`（清单的 `runtime`，在托管 account 的进程里启动）、三个端点、请求与响应的形状 |
| `game/portal/portal_rules.go` | **归工程所有**：用哪个区服目录（`newDirectory`）、推荐规则（`recommend`） |
| `game/portal/portal_directory.go` | 区服目录的接口、静态目录、`Overlay` |
| `game/portal/portal_registry.go` | 动态注册表 |
| `game/portal/portal_config.go` | `portal.servers` 的声明与启动时的检查 |
| `internal/errors/portal.go` | 入口自己的业务码 |
| `game/portal/portal_enter_test.go` | 只在装了 enter 时有：整条链路的场景（登录 → 清单 → 进服 → 握手 → 告知 → 新建 → 进入） |

握手票据由 `game/login` 的 `EnterTicket` 拼，入口不另写一遍格式。

## 边界

- **没有限流，也没有防撞库**。三个端点谁都能调；`/portal/login` 每次都会去问渠道。对外暴露时放在有限流的反向代理后面。
- **明文**。凭据与票据都在请求体里；web Mod 不终止 TLS。生产环境 web Mod 缺省只绑回环地址，对外要放在终止 TLS 的代理后面并显式写 `web.allow_public_addr: true`。生成的生产示例配置里 `web.addr` 是占位值，上线前要改。
- **清单的开销随区服数增长**：每次 `/portal/servers` 是 1 次清单（account 读 1 + N 条记录）加每个服 1 次角色查询，顺序执行，没有缓存。区服很多、请求很密的部署该把 manifest 拆出去并自己加缓存，或者换成分页的清单。
- **一条坏记录会挡住清单**：某个服上本账号的 slot 与角色对不上（`560113`）或合服别名两跳时，整个请求失败。
- **进程内的注册表是每个副本一张**，见“动态目录”。静态配置没有这个问题。
- **新号被送去非 open 的服**时拿得到进服票据，握手才被拒，而握手失败客户端拿不到原因。客户端要看清单里的 `status`。
- **账号 ↔ 玩家号的映射在握手时才建**：拿了进服票据而没有去握手的新号，在 account 里没有角色，清单里仍是 `role: null`。反过来，握手过而没有在游戏服新建的账号，清单里已经有 `role`（“老号”），推荐规则把那个服当作他玩过的服；游戏服那边仍告知要新建。清单里的 `role` 说的是 account 的映射，不是游戏服的玩家数据。
- 入口挂在 account 进程上依赖生成的 bootstrap 里那一行 `kitmodulehost.NewMod(...)`：自己手写 bootstrap 的工程要照着加。
- 场景是离线的：真实的 `account.Service`（内存存储）、真实的路由引擎与登录模块的认证器，与 enter 的联动里游戏服的档是内存替身；没有经真实的监听端口、总线、Redis 与 TCP。真实进程只做过一次单机演练（2026-11-12：一个 account 进程加一个嵌入 TCP 的 game 进程，整条链路与各种拒绝，记录在仓库 CHANGELOG）；独立 Gate、account 多副本、动态注册表都没有经真实进程。
