# 登录鉴权 login

`roost module add login` 把玩家 TCP 握手接到托管的 account 服务上：客户端出示 account 签发的会话令牌，认证器向 account 校验，通过后把角色的归属写进会话。它接管两个生成器留下的“一律拒绝”的桩——`internal/access/player/tcp/auth.go` 与 `internal/service/account/collaborators.go`；只在它们仍是没改过的生成内容时替换，业务改过任何一个就拒绝安装、工程不变。

```sh
roost module plan login
roost module add login enter     # enter 不是依赖；两个都装着时多一条进入前检查
roost module verify
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
3. **登记区服**：account 里要有开放的服，`CreateRole` 才建得了角；服号就是 game 进程的 `sid`。模块带了一个最小的运维命令做这件事（account 与它的 Redis 起来之后执行一次）：

   ```sh
   go run ./cmd/accountctl upsert-server -sid 1000        # 登记并开放，名字缺省 <业务服务名>-1000
   go run ./cmd/accountctl show-server -sid 1000
   go run ./cmd/accountctl upsert-server -sid 1000 -name 一服 -status maintenance   # 整条覆盖：只改状态也要带上名字
   ```

   它用 account 的存储配置直连 Redis（登记区服是不上总线的控制面写）：`-redis` / `-redis-password` / `-redis-db` / `-redis-cluster` 对应 `config.account.yaml` 的 `redis.*`，`-prefix` 对应 `account.key_prefix`，缺省值就是生成的开发配置里的值。别的环境照那个环境的配置填；前缀填错不会报错，只是写进了没人读的键里，登记之后用 `show-server` 与一次真实的建角确认。合服、换服的人工兜底不在这个最小命令里（照 roost-core `demo/cmd/accountctl` 加）。
4. **给客户端一个登录入口**：account 只在内部总线上，客户端够不着。装 [portal](../portal/README.md) 模块（`roost module add portal`）就有了 HTTP 的登录、建角、选角三个端点，选角的结果里就是下面“握手”要出示的票据；也可以由工程自己的入口（平台回调、自建网关）代客户端调 `Login` → `CreateRole` → `SelectRole`。
5. **上线前接渠道**：在 `internal/service/account/login_channels.go` 的 `verifyChannel` 里接平台验签。缺省一个渠道都没接。

## 握手

客户端在握手帧里出示

```
session:<player_id>:<token>
```

`token` 是 `SelectRole` 返回的 `Session.Token`，`player_id` 是那个角色的玩家号（十进制正整数）。玩家号只是提示，会话里的身份取自 account 校验后返回的角色。规则在 `game/login/login.go`：

| 结果 | 认证器返回 | 客户端该做什么 |
| --- | --- | --- |
| 通过 | `Principal`：玩家号、承载服、每次握手一个新的会话号、claims | 发 `EnterGame` |
| 拒绝：格式不对、令牌验不过（签名、过期、不是这名玩家的、会话密钥已轮换）、account 没有这个角色 | `gateway.ErrUnauthenticated` | 重新登录选角 |
| 判断不了：account 不可达、超时、存储出错 | `login.ErrAccountUnavailable` | 稍后拿同一张票据重试 |

两种失败在连接上都只是被关掉（握手失败不带原因）；区别在日志（拒绝 Debug、判断不了 Warn）与返回的错误。

会话里记的 claims（键是 `service/account` 的常量）：`server_id` 承载服、`origin_server_id` 原服、`role_created_at` 角色创建时间（Unix 秒）、`bind_epoch` 绑定代数、`transfer`（只在换服在途时有）。mailbox + enter、rehome + enter 的联动读的就是它们。

认证器不拒绝“不该由本服服务”的角色。装了 enter 时联动文件 `game/login/login_enter.go` 在加载 Player 之前比较承载服与本进程的 `sid`，不一致答 `login_elsewhere`；换服在途的判断归 rehome 模块。没装 enter 的工程在自己的进入流程里调 `login.Admit`。

## account 这一侧

| 文件 | 内容 |
| --- | --- |
| `collaborators.go` | 把下面几项交给 account Mod；名字规则与生成桩相同 |
| `login_dev.go` | 开发登录：渠道 `dev`，凭据等于 `login.dev_token`；没配置、生产配置下都是关的 |
| `login_channels.go` | 工程自己的渠道入口 `verifyChannel`，缺省全部拒绝 |
| `login_player_id.go` | 玩家号：account 的 Redis 里 `<account.key_prefix>:player_id` 的 INCR 计数器，从 100001 起，全服一个 |

运维命令在 `cmd/accountctl/main.go`（`upsert-server`、`show-server`），用的是同一份 collaborators。

## 边界

- 会话令牌是无状态的签名：单张令牌不能吊销，封号也不会让已签发的令牌失效，最长到 `account.session_ttl`（缺省 30 分钟）。要立刻踢人用 `entity.kick` 断开连接；要让全部在外的令牌作废，轮换 `account.session_secret`。
- claims 是握手那一刻的快照。握手之后才合服、换服的，要等下一次握手才看得见；换服期间靠 rehome 模块的驻留屏障。
- 装了 login 之后 `roost project doctor` 的 `collaborators:account` 是 OK，但这只说明“不再是拒绝桩”：没接渠道的生产环境仍然没有人登录得了。
- 场景是离线的：真实的 `account.Service`（内存存储）加认证器，没有经真实的 TCP、总线与 Redis。`cmd/accountctl` 的两条场景同样在内存存储上跑，连 Redis 的那一段（地址、口令、键前缀）没有被测到。
- 工程里已经有自己的 `cmd/accountctl/main.go` 时模块装不上（安装器不覆盖业务文件）：先把自己的那个挪开或改名。
