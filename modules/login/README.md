# login：登录鉴权——玩家 TCP 握手把 account 签发的票据交回 account 校验

`roost module add login` 把玩家 TCP 握手接到托管的 account 服务上：客户端出示 account 签发的票据，认证器把它交给 account 校验，通过后把角色的归属写进会话。分工是：票据统一由 account 签发和校验；account 映射的只是账号 ↔ 玩家号；角色名与玩家数据由游戏服建。所以这个模块里没有任何密钥，也不碰名字。

## 能力

### 握手：两种票据（规则在 `game/login/login.go`）

| 票据 | 从哪来 | 认证器做什么 | 会话上多带的 claims |
| --- | --- | --- | --- |
| `enter:<进服票据>` | account 的 `IssueEnterTicket(账号, 服)`；[portal](../portal/README.md) 模块的进服端点给的就是拼好的这一串 | 原样交给 account 的 `EnterServer`：验票据，按“账号 + 服”解析玩家号，**没有映射就新建**（所以这一步可能写） | `enter_ticket=1`；映射是这一次握手新建的再带 `new_role=1` |
| `session:<player_id>:<token>` | account 的 `SelectRole` 返回的 `Session.Token`（名字在 `CreateRole` 里起过的那条路） | 交给 account 的 `ValidateSession`。玩家号只是“问哪一个角色”的提示，身份取自 account 返回的角色 | 无 |

`Authenticator.Authenticate(ctx, ticket)` 有三种结果：

| 结果 | 返回 | 客户端该做什么 |
| --- | --- | --- |
| 通过 | `gateway.Principal`：玩家号、承载服、每次握手一个新的会话号、claims | 见“握手之后” |
| 拒绝：格式不对；票据验不过（签名、过期、别的域的票据、会话密钥已轮换）；账号不在或被封；服没登记；新号进不了非 open 的服；account 的回答对不上票据 | `gateway.ErrUnauthenticated` | 重新登录、重新要票据 |
| 判断不了：account 不可达、超时、存储出错、对账中的冲突、没见过的码 | `login.ErrAccountUnavailable` | 稍后拿同一张票据重试 |

两种失败在连接上都只是被关掉（握手失败不带原因）。**握手成功与否只由 account 决定，不等游戏服**；认证器也不拒绝“不该由本服服务”的角色——那要给客户端一个码，由进入游戏的那一步答。

辅助函数（都在 `game/login/login.go`）：`Ticket(playerID, token)` 与 `EnterTicket(ticket)` 拼握手票据，`ParseTicket` 与 `ParseEnterTicket` 拆（只看形状），`MaxTicketBytes`（8192，超过的不问 account 直接拒绝）。

### 会话里记的 claims（`Claims(role)` 与 `authenticateEnter`）

| 键 | 含义 | 谁读 |
| --- | --- | --- |
| `svcaccount.ServerIDClaim`（`server_id`） | 承载服：现在由哪个 game sid 服务这个角色 | `Admit`、rehome 的进入前检查、mailbox 的全服邮件范围 |
| `svcaccount.OriginServerIDClaim`（`origin_server_id`） | 原服：角色绑定的服；没合服时与承载服相同 | mailbox（合服前发给原服的全服邮件） |
| `svcaccount.RoleCreatedAtClaim`（`role_created_at`） | 映射创建时间，Unix 秒 | mailbox（“注册早于发送才收得到”） |
| `svcaccount.BindEpochClaim`（`bind_epoch`） | 绑定代数，每换一次服加一 | rehome |
| `svcaccount.TransferClaim`（`transfer`） | 只在换服在途时有 | rehome |
| `svcaccount.NewRoleClaim`（`new_role`） | 只在这一次进服票据握手新建了映射时有。只是提示 | 无人依赖；玩家数据要不要新建由游戏服按自己的档判断 |
| `login.EnterTicketClaim`（`enter_ticket`） | 只在用进服票据握手的会话上有 | 与 enter 的联动（“先新建”） |

claims 是握手那一刻的快照。读承载服用 `ServingServer(claims)`。工程要多记别的事实（渠道、设备）就在 `Claims` 里加键；已有的键不要改名。

### 握手之后：会话激活后的告知（`game/login/login_active.go`）

握手成功、会话已经能收推送之后，认证器把这条会话交给别的功能登记的告知：

```go
login.OnSessionActive("enter", func(ctx context.Context, registry *app.Registry, session login.Active) {
    // session.PlayerID / SessionID / Claims；后台执行，ctx 至多 5 秒（NoticeTimeout）
})
```

- 只在 `init` 里登记；空名、nil、重名 panic；按名字顺序执行。没有登记任何告知的工程这一步什么都不做。
- 在后台执行、有界：`Notifier` 同时至多处理 `MaxPendingNotices`（256）条会话，超过的直接丢弃；进程退出不等在途的告知；告知 panic 只记 Error。所以它只能是“告知”，任何规则都不能只在这里执行。
- 接线：生成的传输 Mod 认认证器的可选接口 `SessionObserver`（生成文件 `internal/access/player/tcp/server_gen.go`），模块的 `auth.go` 实现了它的 `SessionActive`。嵌入 TCP 是鉴权 ACK 写出、自身视图登记之后调用；独立 Gate 是 Game Ingress 的激活通知（`OnActivated`）调用。两种部署下告知拿到的都是这条会话认证出来的 claims。

### 进入前的判断：`Admit`（`game/login/login_admit.go`）

`Admit(self int32, playerID int64, claims map[string]string) error`：会话记的承载服就是本进程的 `sid` 返回 nil；是别的服、或者没有记，返回 `login_elsewhere`（带 `owner_sid`）。只读、不问任何服务。换服在途的角色这里不拒，那是 rehome 模块的事。

### account 这一侧（`internal/service/account/`）

| 文件 | 内容 |
| --- | --- |
| `collaborators.go` | 把下面几项交给 account Mod：`Verifier()`、`Allocator()`、`NameRules()`（1～32 字符、不含空白，只有 `CreateRole` 那条路用得到）、`Metrics()` |
| `login_dev.go` | 开发登录：渠道 `dev`（`DevChannel`），凭据等于配置里的 `login.dev_token`；没配置、生产配置下都是关的 |
| `login_channels.go` | 工程自己的渠道入口 `verifyChannel`，缺省全部拒绝 |
| `login_player_id.go` | 玩家号：account 的 Redis 里 `<account.key_prefix>:player_id` 的 INCR 计数器，从 100001 起，全服一个，不分服 |

### 运维命令 `cmd/accountctl`（`cmd/accountctl/main.go`）

```sh
go run ./cmd/accountctl upsert-server -sid 1000        # 登记并开放，名字缺省 <业务服务名>-1000
go run ./cmd/accountctl show-server -sid 1000
go run ./cmd/accountctl list-servers                    # 已登记的全部区服
go run ./cmd/accountctl upsert-server -sid 1000 -name 一服 -status maintenance   # 整条覆盖：只改状态也要带上名字
```

它用 account 的存储配置直连 Redis（登记区服不上总线）：`-redis` / `-redis-password` / `-redis-db` / `-redis-cluster` 对应 `config.account.yaml` 的 `redis.*`，`-prefix` 对应 `account.key_prefix`，缺省值就是生成的开发配置里的值。`-status` 取 `open`、`maintenance`、`full`、`closed`，只有 open 的服能建新角色。要加自己的子命令不用改 `main.go`：在 `cmd/accountctl` 里另写一个文件，用 `init` 调 `registerCommand(名字, 用法, 函数)`，函数拿到的是 account 服务本身。

### 状态、配置、错误码

- **状态：** 无。模块不往任何实体上加字段；账号、角色映射、区服记录都在 account 服务的存储里。
- **配置键：** `login.dev_token`（写在 `configs/service/config.account.yaml`，必须以 `dev-` 开头；生产配置里写了它 account 进程不启动）。握手的认证器要求承载玩家接入的业务服务在 `roost.yaml` 的 `uses` 里有 `account`。
- **错误码：** `login_elsewhere`（`internal/errors/login.go`）：这条会话的角色不由本服承载，客户端重新选角。握手失败没有码。

**日志**：握手认证成功，记录玩家、承载服、握手方式与是否新建映射，不记录票据和 claims。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/login/login.go`, `internal/access/player/tcp/auth.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做渠道验签。** 装上模块、什么都不配的 account 仍然拒绝每一次登录；`login_channels.go` 的 `verifyChannel` 缺省一个渠道都没接。`roost project doctor` 的 `collaborators:account` 变 OK 只说明“不再是拒绝桩”。
- **开发令牌不是身份验证。** 知道 `login.dev_token` 的人可以用任何账号名登录。生产拒绝靠配置的环境键（`env: prod` 等）加 `dev-` 前缀；没写环境键的预发环境带着它就是全员可冒充。
- **不给客户端登录入口。** account 只在内部总线上，客户端够不着；要装 portal 模块，或者由工程自己的入口代客户端调 account。
- **不建玩家数据、不管角色名。** 进服票据握手只在 account 里建“账号 ↔ 玩家号”的映射。新号握手就占了这个账号在这个服的名额、用掉一个玩家号，不等游戏服建成；握手之后不新建就离开的账号，下次来还是那个玩家号。
- **票据不能单张吊销。** 进服票据与会话令牌的有效期都是 `account.session_ttl`（缺省 30 分钟）。封号对进服票据立即生效（`EnterServer` 每次都再读账号），对已签发的会话令牌不生效。要立刻踢人用 `entity.kick`；要让全部在外的票据作废，轮换 `account.session_secret`。
- **claims 是快照。** 握手之后才合服、换服的，要等下一次握手才看得见。没装 rehome 时换服在途的角色不被源服拒绝。
- **告知是尽力而为。** 同时至多 256 条会话、超过的丢弃、每条会话的全部告知合计至多 5 秒、进程退出不等。
- **玩家号计数器与 account 的数据同生共死。** Redis 被清、或者换了 `account.key_prefix` 却没搬数据，会从头再数、发出重复的号。Redis 出错时不发号（建角失败可以重试）。
- **`accountctl` 的前缀填错不会报错**，只是写进了没人读的键里；登记之后用 `list-servers` 与一次真实的握手确认。合服、换服的人工兜底不在这个最小命令里。
- **装不上的情形：** 模块接管 `internal/access/player/tcp/auth.go` 与 `internal/service/account/collaborators.go`，只在它们仍是没改过的生成内容时替换，业务改过任何一个就拒绝安装、工程不变（全有或全无）；工程里已经有自己的 `cmd/accountctl/main.go` 时也装不上。
- **独立 Gate 部署只做过一次单机真实进程演练**（account、走 Game Ingress 的 game、Gate 三个进程，真实 NATS / Redis / Mongo / etcd）。生成器不生成 Gate 进程，接法是：
  - Game：`bootstrap.NewWithOptions(bootstrap.Options{GameIngress: &options})`，`options.Authenticator = accessplayertcp.NewAuthenticator()`（模块 `auth.go` 的 `NewAuthenticator`，claims 与会话激活后的告知都靠它），`options.RegisterDiscovery = true`，再给 `options.Ready` 与 `options.AdvertiseAddr`；配置里 `player_access.tcp.enabled: false`。
  - Gate：工程自己加一个服务，Mod 是 NATS、`kitetcd.NewEtcdMod(kitetcd.WithoutServiceRegistration())`、account 的 `NewClientMod()`、`gatewiring.NewGateMod(options)`。不带认证器时用 `wiring/gate` 缺省的 `AccountAuthenticator`，它认 `enter:` 与 `session:` 两种票据。
  - 两端的 `options.Config.Namespace` 相同，都开单实例锁（`singleton.enabled`）。
  - 与嵌入 TCP 不同的地方：每次握手 account 的 `EnterServer` 执行两次（Gate 一次，Game Ingress 绑定时用同一张票据再认证一次；第二次读到已建好的映射，所以 Game 侧的会话上没有 `new_role`）。别的服的票据在 Gate 上握手直接被断开，客户端收不到 `login_elsewhere`。
  - 没有跑：多个 Gate、Gate 或 Game 重启之后的重连、跨机、业务服务不叫 `game` 的工程。
- **已知限制：** 以上各条与 game-demo 接入后的取舍见 `docs/maintenance/KNOWN-LIMITS.md` 的“login 的边界”“game-demo 接入登录链路的边界”。

## 复用方式

- **需求正好是它：**

  ```sh
  roost module plan login
  roost module add login enter     # enter 不是依赖；两个都装着时多一组联动
  roost module verify
  ```

  安装器不代办、要自己做的几步（`roost module plan / add login` 的输出里也列着，清单的 `next`）：
  1. 打开监听：`roost config enable player-tcp`。生产示例配置仍是关的，上线时自己开。
  2. 开发登录（只在开发配置里）：在 `configs/service/config.account.yaml` 末尾加 `login:` 下的 `dev_token: dev-随便什么`。
  3. 登记区服：account 里要有状态是 open 的服，新号才进得去，登录入口的清单才不是空的；服号就是 game 进程的 `sid`。本机用 `make dev-run`（`deploy/dev/run.sh start`）起全部服务时脚本会自动登记；别的环境在 account 与它的 Redis 起来之后执行一次 `go run ./cmd/accountctl upsert-server -sid <sid>`。
  4. 给客户端一个登录入口：`roost module add portal`（HTTP 的登录、清单与推荐、进服三个端点），或者由工程自己的入口代客户端调 account 的 `Login` → `IssueEnterTicket`（或 `CreateRole` → `SelectRole`）。
  5. 上线前接渠道：在 `internal/service/account/login_channels.go` 的 `verifyChannel` 里按 `identity.Channel` 分支接平台验签。必须守住三条：验不过返回包着 `svcaccount.ErrIdentityDenied` 的错误；问不到渠道返回包着 `svcaccount.ErrVerifierUnavailable` 的错误；返回渠道确认的 `OpenID`，不是客户端提交的那个。
- **需求比它多一点：**
  - 多记会话事实：改 `game/login/login.go` 的 `Claims`。
  - 把区服编进玩家号：改 `login_player_id.go` 的 `Allocate`。有玩家之后不要调小 `firstPlayerID`。
  - 换名字规则（只影响 `CreateRole` 那条路）：改 `collaborators.go` 的 `NameRules`。
  - 加运维子命令：在 `cmd/accountctl` 另写文件调 `registerCommand`，不改 `main.go`（game-demo 这样加了 `merge-server` / `unmerge-server` / `show-role` / `transfer-role`）。
  - 不要改握手票据的两种写法与已有 claims 的键名：portal、rehome、mailbox 的联动按它们读写。
- **别的功能要在握手后对会话说一句话：** `login.OnSessionActive("名字", notice)`。只放告知。
- **没装 enter、自己写进入流程：** 在加载 Player 之前调 `login.Admit(self, playerID, claims)`，返回错误就不要继续，把错误的码答给客户端。
- **客户端工具、机器人拼握手票据：** `login.EnterTicket(ticket)` 或 `login.Ticket(playerID, token)`。
- **只适合借鉴的情形：** 工程已经有自己的 account collaborators 或自己的认证体系时装不上。值得借鉴的是把“拒绝”与“判断不了”分成两种错误（`refusedByAccount` 按码判断，问不到不当成坏票据），以及握手不拒绝归属、把带码的结论留到进入那一步。

## 联动

本模块的 `with/`：

- `with/enter`（`game/login/login_enter.go`）接三件事：
  1. 进入前检查 `gameenter.OnEntering("login", …)`：调 `Admit`，承载服不是本进程的 `sid`（或没有记）答 `login_elsewhere`，不加载也不创建 Player。与 rehome 一起装时两条检查按名字顺序执行，`login` 在前。
  2. `gameenter.RequireCreateFirst("login", EnterTicketSession)`：用进服票据握手的会话“先新建、再进入”——`EnterGame` 不代建，没建成答 `enter_need_create`；客户端发 `EnterCreate{name}` 建成之后再发 `EnterGame`。用会话令牌握手的会话照旧由 `EnterGame` 加载或创建。
  3. `OnSessionActive("enter", …)`：会话激活后调 enter 模块的 `Announce`，推 `EnterStatus{code, reason, need_create}`；本服不该服务的会话推完断开。

  客户端的完整顺序（新号）：

  ```
  握手 enter:<进服票据>            -> ACK
  （推送）EnterStatus               -> need_create=true
  EnterCreate{name}                 -> code=0（名字不合规 / 被占：换个名字再发；enter_created：早就建成了）
  EnterGame                         -> first=true
  ```

别的模块：

- `portal` 依赖本模块（`requires: [login]`），它的进服端点用 `EnterTicket` 拼握手票据；`portal/with/enter` 有一条从 HTTP 登录到进入游戏的整链路场景。
- `mailbox/with/enter`、`rehome/with/enter` 读本模块写进会话的 claims（不经 `with/login`）。

没有别的模块带 `with/login`。

## 验证范围

`roost module verify` 跑 32 条场景（`^TestLogin`：`game/login` 20 条、`internal/access/player/tcp` 2 条、`internal/service/account` 6 条、`cmd/accountctl` 4 条），与 enter 的联动另 6 条（`^TestWithLoginEnter`）。全部是离线的：

- 认证器的场景用真实的 `account.Service` 加内存存储：两种票据的通过与拒绝、过期、伪造、合服后的承载服、换服在途、并发握手只建一个映射、“拒绝”与“判断不了”的区分。
- 告知的场景验证后台执行、上限与登记错误 panic；“独立 Gate 下用绑定时的 Principal”是对 `SessionActive` 的直接调用，不是真实的 Gate。
- account 一侧验证开发令牌（对、错、关、生产配置拒绝）、渠道缺省全拒、计数器的键与出错不发号。
- `cmd/accountctl` 的子命令在内存存储上跑，连接旗标只验到“读对了”；真正去连 Redis 的那一段（`openService`）没有被场景执行。
- 与 enter 的联动里游戏服的档是内存替身；新建与进入的事务在 enter 模块自己的场景里验证。
- 没有经真实的 TCP、总线、Redis 与持久层的场景。真实进程上做过的是手工演练（单机 `make dev-run` 的自动登记与三步登录、一次独立 Gate 三进程演练），没有做成可重复的脚本，记录在 KNOWN-LIMITS 与 CHANGELOG。
