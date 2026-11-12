# 登录鉴权 login

`roost module add login` 把玩家 TCP 握手接到托管的 account 服务上：客户端出示 account 签发的票据，认证器把它交给 account 校验，通过后把角色的归属写进会话。它接管两个生成器留下的“一律拒绝”的桩——`internal/access/player/tcp/auth.go` 与 `internal/service/account/collaborators.go`；只在它们仍是没改过的生成内容时替换，业务改过任何一个就拒绝安装、工程不变。

```sh
roost module plan login
roost module add login enter     # enter 不是依赖；两个都装着时多一组联动（见“与 enter 一起装”）
roost module verify
```

分工（维护者 2026-11-12 定）：**票据统一由 account 签发和校验**；account 映射的只是**账号 id ↔ 角色 id**；**角色名与玩家数据由游戏服建**。所以这个模块里没有任何密钥，也不碰名字。

## 握手

客户端在握手帧里出示两种票据之一，规则在 `game/login/login.go`：

| 票据 | 从哪来 | 认证器做什么 |
| --- | --- | --- |
| `enter:<进服票据>` | account 的 `IssueEnterTicket(账号, 服)`——[portal](../portal/README.md) 的 `/portal/enter` 给的就是拼好的这一串 | 交给 account 的 `EnterServer`：验票据，按“账号 + 服”解析玩家号，**没有映射就新建**（所以这一步可能写）。会话带 `enter_ticket=1`；映射是这一次新建的再带 `new_role=1` |
| `session:<player_id>:<token>` | account 的 `SelectRole` 返回的 `Session.Token`（名字在 `CreateRole` 里起过的那条老路，game-demo 与配方在用） | 交给 account 的 `ValidateSession`。玩家号只是提示，身份取自 account 返回的角色 |

| 结果 | 认证器返回 | 客户端该做什么 |
| --- | --- | --- |
| 通过 | `Principal`：玩家号、承载服、每次握手一个新的会话号、claims | 见“握手之后” |
| 拒绝：格式不对；票据验不过（签名、过期、别的域的票据、会话密钥已轮换）；账号不在或被封；服没登记；新号进不了非 open 的服；account 的回答对不上票据 | `gateway.ErrUnauthenticated` | 重新登录、重新要票据 |
| 判断不了：account 不可达、超时、存储出错、对账中的冲突 | `login.ErrAccountUnavailable` | 稍后拿同一张票据重试 |

两种失败在连接上都只是被关掉（握手失败不带原因）；区别在日志（拒绝 Debug、判断不了 Warn，都只记票据的长度）与返回的错误。**握手成功与否只由 account 决定，不等游戏服**。

会话里记的 claims：`server_id` 承载服、`origin_server_id` 原服、`role_created_at` 映射创建时间（Unix 秒）、`bind_epoch` 绑定代数、`transfer`（只在换服在途时有）——键是 `service/account` 的常量；`enter_ticket`（`login.EnterTicketClaim`，只在进服票据的握手上有）、`new_role`（`svcaccount.NewRoleClaim`，只在这一次握手新建了映射时有）。mailbox + enter、rehome + enter 的联动读的就是它们。`new_role` 只是提示：独立 Gate 部署下 Gate 与 Game Ingress 各认证一次，Game 那一次看到的总是“已有映射”；要不要新建玩家数据由游戏服按自己的档判断。

认证器不拒绝“不该由本服服务”的角色——握手失败客户端拿不到原因，这要给客户端一个码。没装 enter 的工程在自己的进入流程里调 `login.Admit`。

## 握手之后：会话激活后的告知

握手成功、会话已经能收推送之后，认证器把这条会话交给别的功能登记的告知（`game/login/login_active.go`）：

```go
login.OnSessionActive("enter", func(ctx context.Context, registry *app.Registry, session login.Active) {
    // session.PlayerID / SessionID / Claims；后台执行，ctx 至多 5 秒
})
```

- 接线：生成的传输 Mod 认认证器的可选接口 `SessionObserver`（`internal/access/player/tcp/server_gen.go`）。嵌入 TCP 是鉴权 ACK 写出、自身视图登记之后；独立 Gate 是 Game Ingress 的 `OnActive` 之后。没有登记任何告知的工程这一步什么都不做。
- 告知在后台执行、**有界**：同时至多 256 条会话，超过的直接丢弃（登录洪峰时宁可少一次告知）；进程退出不等在途的告知。所以它只能是“告知”，任何规则都不能只在这里执行。
- 独立 Gate 下激活回调只带玩家号与会话号、不带 claims：认证器把每次认证成功的 claims 按玩家号记 30 秒（至多 4096 名），激活时取回；取不到就不告知。见“边界”。

## 与 enter 一起装

联动文件 `game/login/login_enter.go` 接三件事：

1. **进入前检查**：会话记的承载服不是本进程的 `sid`（或没有记）答 `login_elsewhere`，不加载也不创建 Player。换服在途的判断归 rehome 模块。
2. **进服票据握手的会话先新建、再进入**（`gameenter.RequireCreateFirst`）：这样的会话发 `EnterGame` 时游戏服不代建，玩家在本服没建成（没有档，或者上次建到一半）答 `enter_need_create`、什么都不写；客户端发 `EnterCreate{name}` 建成之后再发 `EnterGame`（那一次的 `first` 为 true）。角色名的规则在游戏服：装了 profile 就是它的名字规则，装了 naming 再加全服唯一，都没装时名字没人看。用会话令牌握手的会话照旧由 `EnterGame` 加载或创建。
3. **会话激活后推 `EnterStatus{code, reason, need_create}`**：
   - `code = 0`：`need_create` 为 true 就先发 `EnterCreate`，否则直接发 `EnterGame`；
   - `code ≠ 0`：本服此刻不该服务这名玩家（`login_elsewhere`、`rehome_moving`……就是他发 `EnterGame` 会得到的那个码），服务器随后断开他在本进程的连接。

   判断复用进入前检查与玩家自己的档，没有第二套规则。**它只是告知，不是关口**：推送可能丢、可能过时，客户端等一两秒没收到就直接发 `EnterGame`，需要新建时得到 `enter_need_create`、再发 `EnterCreate`——结论以这两个请求的应答为准。

客户端的完整顺序（新号）：

```
握手 enter:<进服票据>            -> ACK
（推送）EnterStatus               -> need_create=true
EnterCreate{name}                 -> code=0（名字不合规 / 被占：换个名字再发；enter_created：早就建成了）
EnterGame                         -> first=true
```

## 安装之后还要做的（安装器不代办）

`roost module plan / add login` 的输出里也列着这几步（`todo:` 行，清单的 `next`）。

1. **打开监听**：`roost config enable player-tcp`（鉴权已经不是拒绝桩，这一步现在能过）。生产示例配置仍是关的，上线时自己开。
2. **开发登录**（只在开发配置里）：在 `configs/service/config.account.yaml` 末尾加

   ```yaml
   login:
     dev_token: dev-随便什么
   ```

   令牌必须以 `dev-` 开头。不加这两行，account 拒绝每一次登录（与装模块之前一样安全）。生产配置（`env: prod` 等）里写了它 account 进程不启动，`roost config check --production` 也会点名。
3. **登记区服**：account 里要有状态是 open 的服，新号才进得去；服号就是 game 进程的 `sid`。模块带了一个最小的运维命令做这件事。本机用 `make dev-run`（`deploy/dev/run.sh start`）起全部服务时不用手工执行：脚本发现工程里有 `cmd/accountctl`，会在服务都起来之后按 `config.account.yaml` 的 Redis 与键前缀登记 `$SID`（缺省 1000）。别的环境、或者单独起进程时，在 account 与它的 Redis 起来之后执行一次：

   ```sh
   go run ./cmd/accountctl upsert-server -sid 1000        # 登记并开放，名字缺省 <业务服务名>-1000
   go run ./cmd/accountctl show-server -sid 1000
   go run ./cmd/accountctl list-servers                    # 已登记的全部区服（account 的 ListServers）
   go run ./cmd/accountctl upsert-server -sid 1000 -name 一服 -status maintenance   # 整条覆盖：只改状态也要带上名字
   ```

   它用 account 的存储配置直连 Redis（登记区服是不上总线的控制面写）：`-redis` / `-redis-password` / `-redis-db` / `-redis-cluster` 对应 `config.account.yaml` 的 `redis.*`，`-prefix` 对应 `account.key_prefix`，缺省值就是生成的开发配置里的值。旗标的名字与 game-demo 的同名命令相同（`deploy/dev/run.sh` 就是按这些名字调它的），别的环境照那个环境的配置填；前缀填错不会报错，只是写进了没人读的键里，登记之后用 `list-servers` 与一次真实的握手确认。合服、换服的人工兜底不在这个最小命令里。要加自己的子命令不用改 `main.go`：在 `cmd/accountctl` 里另写一个文件，用 `init` 调 `registerCommand(名字, 用法, 函数)`，函数拿到的是 account 服务本身；与自带的三个命令重名、重复登记会 panic。roost-core 的 game-demo 这样加了 `merge-server` / `unmerge-server` / `show-role` / `transfer-role`（`demo/cmd/accountctl/operator.go.tmpl`）。
4. **给客户端一个登录入口**：account 只在内部总线上，客户端够不着。装 [portal](../portal/README.md) 模块（`roost module add portal`）就有了 HTTP 的登录、清单与推荐、进服三个端点，进服的结果里就是握手要出示的 `enter:<进服票据>`；也可以由工程自己的入口（平台回调、自建网关）代客户端调 `Login` → `IssueEnterTicket`（或老路 `CreateRole` → `SelectRole`）。
5. **上线前接渠道**：在 `internal/service/account/login_channels.go` 的 `verifyChannel` 里接平台验签。缺省一个渠道都没接。登录 account 的参数是渠道与渠道里的用户标识（`Identity{Channel, OpenID, Credential}`）。

## account 这一侧

| 文件 | 内容 |
| --- | --- |
| `collaborators.go` | 把下面几项交给 account Mod；名字规则与生成桩相同（只有老路 `CreateRole` 用得到） |
| `login_dev.go` | 开发登录：渠道 `dev`，凭据等于 `login.dev_token`；没配置、生产配置下都是关的 |
| `login_channels.go` | 工程自己的渠道入口 `verifyChannel`，缺省全部拒绝 |
| `login_player_id.go` | 玩家号：account 的 Redis 里 `<account.key_prefix>:player_id` 的 INCR 计数器，从 100001 起，全服一个 |

运维命令在 `cmd/accountctl/main.go`（`upsert-server`、`show-server`、`list-servers`），用的是同一份 collaborators。

## 边界

- 票据都是无状态的签名：单张不能吊销，最长到它的有效期（进服票据与会话令牌是 `account.session_ttl`，缺省 30 分钟）。封号对**进服票据**立即生效（`EnterServer` 每次都再读账号），对已签发的**会话令牌**不生效。要立刻踢人用 `entity.kick` 断开连接；要让全部在外的票据作废，轮换 `account.session_secret`。
- claims 是握手那一刻的快照。握手之后才合服、换服的，要等下一次握手才看得见；换服期间靠 rehome 模块的驻留屏障。
- 新号握手就在 account 里建了映射（占了这个账号在这个服的名额、用掉一个玩家号），不等游戏服建成。握手之后不新建就离开的账号，下次来还是那个玩家号，游戏服仍告知要新建。
- **独立 Gate 部署只做过一次单机真实进程演练**（2026-11-12：account、走 Game Ingress 的 game、Gate 三个进程，真实 NATS / Redis / Mongo / etcd，整条链路与各种拒绝，记录在仓库 CHANGELOG）。生成器不生成 Gate 进程，接法是：
  - Game：`bootstrap.NewWithOptions(bootstrap.Options{GameIngress: &options})`，`options.Authenticator = accessplayertcp.NewAuthenticator()`（claims 与会话激活后的告知都靠它），`options.RegisterDiscovery = true`，再给 `options.Ready` 与 `options.AdvertiseAddr`；配置里 `player_access.tcp.enabled: false`。`RegisterDiscovery` 为 true 时生成的 bootstrap 让这个服务的 etcd Mod 不再自动登记（`playerDiscovery`），发现里 game 的记录改由 Game Ingress 写（服务类型固定是 `game`，地址是 `options.AdvertiseAddr`）。
  - Gate：工程自己加一个服务，Mod 是 NATS、`kitetcd.NewEtcdMod(kitetcd.WithoutServiceRegistration())`、account 的 `NewClientMod()`、`gatewiring.NewGateMod(options)`（`options.TCP.Enabled`、监听地址、`Ready`、`RegisterDiscovery`）。不带认证器时用 `wiring/gate` 缺省的 `AccountAuthenticator`，它认 `enter:` 与 `session:` 两种票据。
  - 两端的 `options.Config.Namespace` 相同，都开单实例锁（`singleton.enabled`）。
  - 与嵌入 TCP 不同的地方：每次握手 account 的 `EnterServer` 执行两次（Gate 一次，Game Ingress 绑定时用同一张票据再认证一次；第二次读到已建好的映射，所以 Game 侧的会话上没有 `NewRoleClaim`）。别的服的票据在 Gate 上握手直接被断开（Gate 按承载服找不到 game），客户端收不到 `login_elsewhere`。激活回调不带 Principal，告知要的 claims 靠 Game Ingress 绑定时的那次认证按玩家号记 30 秒取回（至多 4096 名；取不到就不告知，客户端直接发 `EnterGame` 仍然走得通）——更干净的做法是让 gateway 的激活回调带上 Principal，没有做。
  - 没有跑：多个 Gate、Gate 或 Game 重启之后的重连、跨机、业务服务不叫 `game` 的工程。
- `EnterStatus` 里“断开”断的是这名玩家在本进程的全部会话（传输没有只断一条会话的入口），并且是推出去 300 毫秒之后才断——慢的连接可能没收到那一帧就被断开。
- 装了 login 之后 `roost project doctor` 的 `collaborators:account` 是 OK，但这只说明“不再是拒绝桩”：没接渠道的生产环境仍然没有人登录得了。
- 场景是离线的：真实的 `account.Service`（内存存储）加认证器；与 enter 的联动里游戏服的档是内存替身，新建与进入的事务在 enter 模块自己的场景里用真实的 Nest（替身提交器）验证。没有经真实的 TCP、总线、Redis 与持久层。`cmd/accountctl` 的场景里，子命令在内存存储上跑，连接旗标只验到“读对了”；真正去连 Redis 的那一段（`openService`）与 `deploy/dev/run.sh` 的自动登记没有被执行过。
- 工程里已经有自己的 `cmd/accountctl/main.go` 时模块装不上（安装器不覆盖业务文件）：先把自己的那个挪开或改名。
