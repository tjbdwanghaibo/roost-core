# account_auth：account 的登录协作者（开发登录、渠道入口、玩家号计数器）

`roost module add account_auth` 替换托管 account 服务生成的拒绝桩 `services/account/collaborators.go`：生成的桩拒绝每一次登录与建角，doctor 的 `collaborators:account` 报 FAIL；装上之后 account 有了开发登录、留给工程的渠道入口和玩家号分配器。它只写托管 account 的那个包，不碰任何业务服务，所以**不带 game 的开房间工程**（`roost project new <名> -template lobby`：只托管 account、lobby、relay）也能装。

2026-11-17 从 [login](../login/README.md) 拆出来（之前这几个文件是 login 的一部分，而 login 要改玩家 TCP 鉴权与 game 包，不带 game 的工程装不上）。login 与 [portal](../portal/README.md) 都依赖它；文件内容、测试名与拆分之前逐字节相同。

## 能力

`services/account/` 下的五个文件：

| 文件 | 内容 |
| --- | --- |
| `collaborators.go` | 把下面几项交给 account Mod：`Verifier()`、`Allocator()`、`NameRules()`（1～32 字符、不含空白，只有 `CreateRole` 那条路用得到）、`Metrics()` |
| `login_dev.go` | 开发登录：渠道 `dev`（`DevChannel`），凭据等于配置里的 `login.dev_token`；没配置、生产配置下都是关的 |
| `login_channels.go` | 工程自己的渠道入口 `verifyChannel`，缺省全部拒绝 |
| `login_player_id.go` | 玩家号：account 的 Redis 里 `<account.key_prefix>:player_id` 的 INCR 计数器，从 100001 起，全服一个，不分服 |
| `login_test.go` | 场景（`^TestLogin`，6 条） |

- **状态：** 无。账号、角色映射都在 account 服务的存储里。
- **配置键：** `login.dev_token`（写在 `configs/service/config.account.yaml`，必须以 `dev-` 开头；生产配置里写了它 account 进程不启动）。
- **错误码：** 无。拒绝用 account 自己的码（`ErrIdentityDenied` 560102 等）。

## 能力边界

- **不做渠道验签。** 装上、什么都不配的 account 仍然拒绝每一次登录；`verifyChannel` 缺省一个渠道都没接。doctor 的 `collaborators:account` 变 OK 只说明“不再是拒绝桩”。
- **开发令牌不是身份验证。** 知道 `login.dev_token` 的人可以用任何账号名登录。生产拒绝靠配置的环境键（`env: prod` 等）加 `dev-` 前缀；没写环境键的预发环境带着它就是全员可冒充。
- **不给客户端登录入口。** account 只在内部总线上，客户端够不着；要装 portal，或者由工程自己的入口代客户端调 account。
- **玩家号计数器与 account 的数据同生共死。** Redis 被清、或者换了 `account.key_prefix` 却没搬数据，会从头再数、发出重复的号。不带 game 的工程只登录、拿游戏账号 token，不建角，计数器不会被调用。
- **前提：** 工程托管 account（`services.<名字>.framework: account`）。有业务服务时安装器在同一次安装里声明它并加进业务服务的 `uses`（清单的 `needs.uses: [account]`）；没有业务服务的工程必须已经托管 account（`-template lobby` 自带）。
- **装不上的情形：** `services/account/collaborators.go` 被业务改过（不再是没改过的生成桩）时拒绝安装、工程不变。
- **已知限制：** 与 login 相同的那几条（开发令牌、渠道、计数器），见 `docs/maintenance/KNOWN-LIMITS.md` 的“login 的边界”。

## 复用方式

```sh
roost module add account_auth     # 只要 account 的协作者
roost module add portal           # 不带 game 的开房间工程：连同 account_auth 一起装，客户端经 HTTP 登录、拿游戏账号 token
roost module add login portal     # 有 game 的工程：握手的认证器、入口，account_auth 跟着装
roost module verify
```

装完要自己做的（`roost module plan / add` 的 `todo:` 行）：

1. 开发登录（只在开发配置里）：`configs/service/config.account.yaml` 末尾加 `login:` 下的 `dev_token: dev-随便什么`。
2. 上线前接渠道：在 `services/account/login_channels.go` 的 `verifyChannel` 里按 `identity.Channel` 分支接平台验签。必须守住三条：验不过返回包着 `svcaccount.ErrIdentityDenied` 的错误；问不到渠道返回包着 `svcaccount.ErrVerifierUnavailable` 的错误；返回渠道确认的 `OpenID`，不是客户端提交的那个。
3. 给客户端一个登录入口：`roost module add portal`。
4. 凭证变更时吊销游戏账号 token（交接 C6，维护者 2026-11-17）：account 没有改密码 / 换绑的路径——身份由渠道确认，密码在渠道那一侧。工程的渠道或账号中心在玩家改密码、换绑、报告 token 泄露时，经总线调 account 的 `RevokeGameTokens(ctx, accountID)`（`svcaccount.Accounts`，账号号就是 `Identity{Channel, OpenID}.AccountID()`）：这个账号此前签发的游戏账号 token（gt2）全部失效，之后签发的照常可用。运维手动吊销用 login 模块的 `accountctl revoke-game-tokens`。

需求比它多一点：把区服编进玩家号改 `login_player_id.go` 的 `Allocate`（有玩家之后不要调小 `firstPlayerID`）；换名字规则改 `collaborators.go` 的 `NameRules`。工程已经有自己的 account 协作者时不要装它（装不上），可以借鉴的是“拒绝”与“问不到渠道”分成两种错误、返回渠道确认的身份。

## 联动

没有 `with/`。依赖它的模块：login（握手的认证器、玩家 TCP 接入、`cmd/accountctl`）、portal（HTTP 登录入口）。

## 验证范围

`roost module verify account_auth` 跑 6 条场景（`^TestLogin`，`services/account`）：开发令牌（对、错、关、生产配置拒绝）、渠道缺省全拒、计数器的键与 Redis 出错不发号。account 是真实的 `svcaccount.Service`（内存存储），Redis 是只实现 INCR 的替身。真实进程：2026-11-17 在 `-template lobby` 工程上起 account、lobby、relay 三个进程，经 portal 用开发登录拿游戏账号 token 走完大厅与 relay（`docs/maintenance/evidence/lobby-relay-portal-20261117/`）。
