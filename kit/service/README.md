# roost-service

roost 框架的**通用服务层**：与玩法无关的公共服务，作为库提供给业务仓装配。

每个服务是一个包，内含请求/响应类型、`Client` 接口与 bus 实现、服务端实现、
存储契约与后端实现、admin 命令注册。业务仓装配它们；`app.Service` 进程壳留在业务仓，
因为"哪些服务同进程部署"是部署决策，不是库的决策。

## 设计约束

这些约束来自对一个生产业务仓公共服务层的逐文件审计（62 项发现，收敛到六个反复出现
的模式）。它们不是风格偏好，每一条都对应一类已确认的缺陷。完整依据见
[roost-core docs/ROOST_SERVICE_EXTRACTION.md](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/ROOST_SERVICE_EXTRACTION.md)。

1. **状态变更只有版本化路径。** 共享状态走 `roost-kit/versionstore`：契约里没有
   无条件写，因此非 CAS 实现无法存在。不自己手写"读-改-写"。
2. **生产包不保留内存版 source of truth。** 测试替身放 `_test.go` 或 `servicetest`。
3. **每个服务定义自己的 errcode 段**，纯客户端错误不返回 `CodeInternal`。
   实现方式是把 sentinel 本身变成 `errcode.Define(Code…, …)`，于是
   `errcode.ClientError` 能穿透任意层 `fmt.Errorf` 找到 code，**不需要一张
   逐 sentinel 的映射表**——那种表是第二份清单，新增的错误会从上面静默掉下去
   （`chat` 与 `global` 原本各有一张，已删）。反过来，**未分类的错误诚实地报
   `CodeInternal`**：本包没有"存储失败"这类兜底码，因为那是把猜测当成诊断。
   九个包各有一条测试，把「段内连续、个数精确、外部 sentinel 显式映射、
   未分类即 Internal」钉住。
4. **至少一次路径必须幂等**，幂等键由服务端生成或经 ledger 预留，不采信客户端。
5. **身份与权限不来自请求体。** 信任只能来自传输层身份。
6. **每个服务必须有 metrics**：队列深度、CAS 冲突率、丢弃计数。静默路径之所以能长期
   存在，就是因为没有信号。上报走 `servicemetrics`：**全仓一套词汇、一个 `Reporter`
   接口**，方法按事件命名（`Accepted`/`Refused`/`Replayed`/`Dropped`/`Conflict`/`Depth`）
   而不是字符串键的 `Count`，写错事件名是编译错误而不是一条没人看的指标。
   `nil` 是合法的，含义是"不上报"，且永远不会让操作失败——因此每个调用点都可以是
   无条件的，不存在"某个分支忘了报"。
   这一条本身也验证过：每个包都有一条测试断言上报点**真的被走到**，并经"去掉上报即
   变红"确认。约束不能只写在 README 里——本仓存在的理由就是"靠人记住的约束会失效"。
7. **任何列表接口都有上界**，且上界不能被 `0` 绕过。
8. **测试用求值型替身**（`roost-kit/mongo/mongotest`），并发不变量必须有并发测试。
9. **每个修掉的缺陷都有一条经"回退修复即变红"验证过的回归测试。**
10. **求值型替身也有边界，要说清楚。** 用 Go 重新实现 Lua 脚本语义的替身，无法发现
    脚本文本自身的缺陷——改脚本字符串对它没有影响。这部分由 `//go:build integration`
    的真实 Redis 测试覆盖，并因此把脚本写得尽量小。`rank` 的并发缺陷正是真实 Redis
    抓到、替身抓不到的。

## 六个反复出现的缺陷模式

十条约束不是凭空定的。对一个生产业务仓公共服务层做逐文件审计得到 62 项发现，它们
收敛到六个模式——每一个都在**多个互不相关的服务里被不同作者各自重新犯了一次**。
这解释了为什么约束写在契约层面而不是写成规范：靠人记住的东西，六次里失效了六次。

| 模式 | 独立出现处 |
| --- | --- |
| CAS/版本是可选的或假的 | 账号服务比较整个 JSON blob，且默认 store 根本没实现 CAS 接口；跨服服务 4 个 store 的 DAO 变体读后无条件写；匹配服务用进程内计数器当版本；榜单服务版本为 0 时守卫被整个跳过 |
| 静默丢弃却报告成功 | 取消操作回 OK 而存储里仍是 matched；归档首个短页即截断而元数据照写完整计数；重读不足时返回成功而队列已截断 |
| 至少一次路径无幂等 | 榜单重投累加 2–20 次；聊天发布无幂等键而传输是 `max_deliver 5`；充值发货自身无状态 |
| 身份/权限来自客户端 | 聊天的 `Trusted` 布尔；匹配的客户端提供 ticket ID；登录仅凭 `{channel, open_id}` |
| 一个玩家包触发无界读 | 榜单 `Limit: 0` 读整榜；匹配入队 O(N) 持全局锁；建角色每次全表扫服务器列表 |
| 零可观测性 | 匹配、副本、平台三个包没有一行 slog 或 metric，因此上面所有"静默"路径在生产中不可发现 |

还有一条横切的：**多处测试把缺陷当期望行为钉死了**。所以第 9 条约束（每个修掉的缺陷
都要经"回退修复即变红"验证）不是形式主义——省掉它就是那些测试的来源。

完整依据见 [roost-core docs/ROOST_SERVICE_EXTRACTION.md](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/ROOST_SERVICE_EXTRACTION.md)。

## 包

| 包 | 职责 | 单测 | 变异验证 |
| --- | --- | --- | --- |
| `directory/` | 唯一键预留的两阶段提交原语（**注意：同 owner 幂等，因此不是互斥锁**——需要"只准一次尝试"用 `versionstore.Create`） | 21 | 6 |
| `rank/` | 榜单提交与查询。排名所需的一切编进 sorted-set 的 member，因此读一页一次往返、排名与分数不可能不一致 | 35 | 8 |
| `match/` | 匹配：Mod、sweep 循环、`Matchmaker` RPC 与生成传输；**领域实现（队列状态机、原子成组提交、Redis store、Grouping）在 roost-core/service/match**，此处别名（M-06）；`Matchmaker` 接口与传输半在 core，kit 只有装配半（M-11） | 29 | 10 |
| `account/` | 账号与角色目录、角色会话令牌。`IdentityVerifier`/`PlayerIDAllocator`/`NameValidator` 必填且**无默认实现** | 32 | 11 |
| `global/` | 跨服路由绑定（epoch CAS 迁移）、游戏服租约（incarnation fence） | 26 | 13† |
| `global/activity/` | 跨服活动协调：首个 notify 起算的宽限窗、先预留后应用的进度 ledger、拒绝即审计、带 ACK 令牌的结果投递 | 47 | 13† |
| `chat/` | 频道消息：发布/历史/保留。`PublishRequest` 里**没有**发送者或可信字段；系统消息只能经 `PublishSystem` + 服务端签发的 `SystemToken` | 39 | 8 |
| `mail/` | 邮件：Mod、`Mail` RPC 与生成传输、server run；**领域实现（信封、邮箱状态机、三段式领取、Redis stores、错误码段）在 roost-core/service/mail**，此处别名（M-07）；`Mail` RPC 接口与传输半（wire / handler 表 / `BusClient`）也在 core，kit 从 core 的接口生成装配半 `mail_rpc_assembly_gen.go`（M-11）。claim token 由服务端生成且对同一封邮件恒定——重试换不出新的幂等键 | 6 | 4 |
| `platform/` | 渠道边缘：凭证换会话、支付回调换恰好一次发货。**验签是必要而不充分的**，订单是仅插入的持久记录 | 51 | 21 |
| `session/` | 有界 run 原语：Mod、`Session` RPC 与生成传输、server run；**领域实现（幂等 Enter、每 owner 一个活 run、资源恰好释放一次、Admin 操作面、Redis stores）在 roost-core/service/session**，此处别名（M-08）；`Session` 接口与传输半在 core，kit 只有装配半（M-11） | 6 | 3 |
| `servicemetrics/` | 全仓共享的上报 seam 与测试用 `Recorder`——**实现已在 roost-core/servicemetrics**，此处是别名包（M-06） | 8 | 3 |
| `servicemods/` | capability 名字表与各 Mod 共用的配置读取 | 12 | — |
| `integration/` | 跨服务的真实后端测试：十个包在一个活 Redis 上装起来跑通、key 命名空间不冲突、以及**整套 Mod 生命周期端到端** | 67 真实 Redis | 30 |

† `global/activity/` 从 `global/` 拆出（见下），拆分前那 13 条变异验证是合并记录的，
没有事后拆开归属——凭印象分摊会得到一个看起来精确其实是编的数字。

合计 430 条单测 + 67 条真实 Redis 集成测试，`-race` 全绿（单测与集成测试都跑过 `-race`）。

**集成测试默认是跳过的**：没有 `REDIS_ADDR` 时 `integration/` 全部 `t.Skip`。这不是
细节——本轮就是因为这个，`session`/`rank`/`match`/`platform` 四个包里断言**具体类型**
的四条集成子测试长期"绿着"，直到真的连上 Redis 那天同时红掉四条。所以：

```bash
REDIS_ADDR=127.0.0.1:6379 go test -race -tags integration ./integration/
```

传输层重新生成（`roost-codegen` 以 **tool 依赖**接在 go.mod 里，所以 `GOWORK=off`
发布态下同样能跑）：

```bash
go generate ./...
```

`-check` 给 CI 用:生成并逐字节比对，任何差异非零退出（它一度只跑拒绝规则、
不比对生成物，于是永远退出 0——见 roost-codegen CHANGELOG）：

```bash
go tool servicerpc -dir ./mail -check
```
发布态 `GOWORK=off` 下 build / vet / test / -race 与集成测试同样全绿。

「变异验证」= 把修复逐条回退、确认对应测试变红的次数。不编译的变异不算——它什么都
没证明。

## 跨进程：接口 + 生成的传输层

服务会各自独立成进程,所以 registry 里放的**不能是具体类型**。放的是**接口**,两种实现都满足它:拥有者进程放 `*Service`,其他进程放 `BusClient`。调用方永远 `app.Lookup[mail.Mail]`,不知道也不需要知道对面在哪 —— 同一份业务代码在合并部署与拆分部署下都不用改。

传输层由 **`roost-codegen` 的 `servicerpc` 生成器**从**手写的接口本身**生成(不另立 def 文件):线上类型、handler 注册、打字的 client、`Server`、`ClientMod`、capability 包装。

从接口生成让**漂移结构上不可能**,而不是被检测到 —— 这是这个生成器唯一值得存在的理由,它省的打字量不多。接口刻意比进程内 API 小(mail 是 8 个方法而不是 11 个),每个省略都有理由。

生成器**拒绝**的东西和它生成的一样重要:未命名的参数/返回值(没有字段名可用)、`time.Duration`/`time.Time` 上线(纳秒整数抓包读不懂、非 Go 调用方造不出)、**未导出字段**(codec 会静默丢掉 —— `chat.SystemToken` 的授权就在那里)、未导出类型、递归超过 8 层。校验会**递归进包内结构体**,因为最可能真实发生的形状是"Duration 藏在整体传递的结构体里"。

另外两条拒绝规则是**包级别**的,都由真实事故推出来:

- **生成的名字与包里已有的声明撞名 → 拒绝。** `account` 有个 `type Server struct`(服务器列表里的一行),而生成的传输层也叫 `Server`(进程壳),两个文件放一起编不过。编译器说的是"Server redeclared in this block"并指向生成的文件——它告诉你重复在哪,不告诉你为什么在那儿、以及哪一边能动。生成的那边不能动:`pkg.Server`/`pkg.CapabilityName` 在每个服务里都要读法一致。所以这里点名拒绝,并说清该改哪个。撞名的若是**函数或常量**更危险:它能在什么都编不过之前先改掉整个包的含义。
- **一个包里两个 `//roost:rpc` 接口 → 拒绝。** 生成文件在包级别声明十几个固定名字,两份就是每个都声明两次。这条不是绕不过去的限制,而是把一件早就成立的事说出来:`app.Service` 每进程一个,所以两个被标记的接口就是两个独立部署的东西,而两个独立部署的东西应该是两个包。备选方案(按接口给生成名字加前缀)会让 `pkg.Server` 在有些包里叫 `pkg.FooServer`,把成本转给每个服务的每个读者,只为省这一次拆包。

`global` 正是被第二条拒掉的,拆包见下节。

每个服务只手写一个 `run(ctx)` 钩子,连"没有周期性工作"也要显式写出来 —— 生成一个默认的阻塞实现等于用不问来回答这个问题。写下来之后每个服务的答案都不一样,而且答案本身有信息量:

| 服务 | `run(ctx)` 做什么 | 为什么 |
| --- | --- | --- |
| `match` | 15s 扫过期 ticket | 被替换的实现写了 `ExpiresAt`、转发了它,**全代码库没有一处读它** |
| `session` | 30s 扫过期 run | 同上:截止时间要有人读才算存在 |
| `platform` | 30s 重试发货失败的订单 | 订单有 `Attempts`/`NextAttemptAtUnix`,但除非有人调 `AttemptDelivery` 否则永不推进——对这个包来说就是"玩家付了钱没拿到货" |
| `chat` | 5min 按保留期裁剪 | 被替换的实现把时间戳存成毫秒 int64,连 TTL 都不可能;`Prune` 存在但没人调是同一个结果 |
| `global/activity` | 20s 推进过期活动 + 重试到期投递 | 宽限窗是唯一能在某个 game 服永不通知时收尾的东西 |
| `rank` | 无 | 分数在 CAS 下写入、不会自己过期;`Reset` 清空整榜,是运维动作不是 ticker 的决定 |
| `account` | 无 | 会话带 `ExpiresAtUnix`,**每次 `ValidateSession` 都在读**;没有任何东西被"占着" |
| `global` | 无 | 租约同理——过期租约不是"被占着":`AcquireLease` 直接拿走。而且删版本化 key 会让版本从 1 重来,续约要过的 fence 就变成跟一个刚归零的版本比大小 |

### 接口比进程内 API 小,每个省略都有理由

| 服务 | 上总线 | 不上总线,以及为什么 |
| --- | --- | --- |
| `mail` | 8 / 11 | `Deliver`/`Mailbox` 是拥有者内部路径 |
| `rank` | 6 / 7 | `Reset` 清空整个榜——任何服务进程都能抹掉一个排行榜 |
| `session` | 6 / 7 | `Sweep` 是拥有者的周期性工作 |
| `match` | 7 / 8 | 同上 |
| `platform` | 3 / 5 | `ValidateSession` **没有 ctx**——它不做任何 I/O,只用本进程已有的密钥重算一个 MAC。把它做成一次往返 = 全集群的会话校验都排在一个进程后面。`AttemptDelivery` 是重试,归 `run` |
| `account` | 6 / 7 | `UpsertServer` 是控制面写:它改变**所有玩家能登进哪些服**。同一条总线上的任何进程都能关服,不行 |
| `chat` | 5 / 8 | `Resolve` 没 ctx,而且 `ChannelRef` 的 key 字段是**未导出的**(这是故意的:ref 只能来自 `Resolve`)——所以它根本过不了总线,任何 codec 都会静默丢掉 key,对面拿到的 ref 指向空。生成器就是按这条拒的。`Prune`/`Stats` 继承了这一点,而且本身是维护动作 |
| `global` | 9 / 9 | 唯一一个全放出去的。`Bind` 是**仅插入**的,第二次 `Bind` 输在 `Create` 上被拒——那次碰撞就是 fence,它只能建立不存在的绑定,动不了活着的游戏服。三个迁移步骤各带 `expectEpoch`,在 CAS 内部校验 |
| `global/activity` | 9 / 14 | `AdvanceExpired`/`DueDispatches`/`AttemptDispatch` 是拥有者的周期性工作;`NotifyAudits`/`AuditOverflow` 是诊断面,回答的是运维的问题不是 game 的问题 |
| `directory` | 0 | **故意不给 RPC 面**:它是被 `account` 嵌入的原语,不是服务 |

`chat.PublishSystem` **在**接口里,这条值得单独说,因为这个包存在的主要理由就是删掉一个"客户端传来的 `Trusted` bool 是系统消息的全部授权"。把系统路径放上总线的答案是:**关于信任的判断没有任何一部分上线**。`SystemPublishRequest` 带频道、actor 标签、类型、正文、幂等键,**不带令牌**;令牌是在**拥有者**那边、由部署提供的 `SystemAuthenticator` 从 handler 自己 ctx 里的传输身份铸出来的。所以总线情形是**fail closed**:如果某个部署的总线不带 authenticator 能背书的调用方身份,它就签不出令牌,`PublishSystem` 直接以 `ErrSystemDenied` 拒绝。**缺一块拼图产生的是拒绝,不是许可**——那个 `Trusted` bool 恰好搞反了这件事。

有一件事**是**被信任的,应该说出来而不是留在暗处:`Publish`/`History`/`Conversation`/`Scrollback` 都把调用方身份作为**参数**收下,所以跨进程时是**调用方进程**在声明发送者/观看者是谁。进程内这个参数来自会话;跨进程它来自持有会话的那个进程——一个已经认证过玩家的网关。chat 信它。这是内部总线的信任模型,不特属于这个方法或这个服务,也正是总线不能从部署外可达的原因。这个设计买到的是:身份是独立的**实参**而不是请求体的字段,所以一个原样转发的玩家包造不出身份来。

## 运维面：`admin.go`

三个服务有**真正的死路** —— 自动路径已经放弃、而在此之前**没有任何代码路径能改变它**
的终态。不是补齐对称性，每一条都点名了后果：

| 服务 | 死路 | 操作 |
| --- | --- | --- |
| `platform` | 订单尝试耗尽 → **玩家付了钱、货永远不发**。`AttemptDelivery` 正确地拒绝它（否则预算就不是预算），唯一痕迹是一行 `slog.Error` —— 那不是工作队列，也活不过日志轮转 | `ReopenDelivery`（重回重试队列）、`SettleOutOfBand`（已退款/已人工发货） |
| `global/activity` | dispatch 尝试耗尽 → 某个 game 服**永远收不到**它的玩家参与过的活动结果。两条自动路径都拒绝它：`AttemptDispatch` 不再发，`AckDispatch` 拒绝迟到的确认 | `ReopenDispatch` |
| `session` | Releaser 永远不可能成功的资源（副本被带外删了、id 从来无效）→ 与暂时故障**完全无法区分**，sweep 永远重试 | `ForceRelease` |

`session` 那条的后果比看起来严重得多，而且从 `Run.Live` 上**看不出来**：

```
Enter → owner 的 claim 被占 → resolveClaim → run 不 live
      → resolve() 释放资源 → 释放失败
      → resolveClaim 返回错误 → claim 永不释放
```

claim 是**故意**在清理成功之后才释放的（"claim 绝不能比清理活得更久"，这是对的），
代价就是:一个永远无法成功的释放 = 一个**永远进不去的玩家**。我一开始把这条说反了
（"owner 不会被挡，因为 Live 不看 pending 资源"）—— 挡住 Enter 的是 claim，不是 Live。
现在有测试钉住真实行为。

### 三条共同的约束

- **不上总线。** 三个 `Admin` 都**没有** `//roost:rpc` 标记。它们比各自接口上的任何
  方法都危险（一个能造成二次发放、一个重投结果、一个宣称外部资源已消失），而总线不带
  这些服务能验证的调用方身份。只有**拥有者进程**摸得到 —— 人怎么摸到那个进程（内部
  listener、对着同一个 Redis 的 CLI、部署自己认证的运维 RPC）是部署决策，做在凭据所在
  的地方。这一条由集成测试双向钉住:公开 capability **不能**满足 `Admin`，owner-only
  capability **必须**满足。
- **note 必填、无默认。** 一次在已付款订单上、没有记录理由的干预是**无法复核**的 ——
  下一个看这条记录的人只知道有人改过，查不到为什么、也查不到是否有意。
- **不做枚举。** 这是对我自己规划时一个说法的**更正**。"找不到那些耗尽的订单"听起来像
  问题，其实不是:**支付渠道手里有权威清单**（每家都出对账报表），运维真正问的是"渠道
  说收了钱的这些单，我们发货了吗" —— `Service.Order` 已经按 id 回答了。在这里建索引是
  重复一份本服务并不拥有的事实来源，而且它必须写在设置终态的那次 CAS 之外，于是它可以
  和它索引的记录不一致。真正的缺口窄得多:**拿到 id 之后没有路可走**。

### 两条最容易反过来做错的

- **重开时 ACK 令牌不换。** game 服可能已经收到并应用了结果，只是确认丢了（响应丢包、
  两步之间重启），之后 dispatch 耗尽。重开会把同一份结果再投一次,而 game 唯一能去重
  的键就是令牌。换一个新令牌 = 把同一份结果换个身份递过去,**恰好坑掉那些做对了事的
  调用方**。这是 mail 的 claim token 那条规则:服务端生成、对同一个东西恒定，重试换不出
  新的幂等键。
- **`settled` 是第三个终态，不是复用 `delivered`。** 一次把人工退款算成已发货的对账，
  会报出本服务并没有完成的履约。"发货方成功了"/"我们放弃了"/"有人在别处解决了"是关于
  这笔钱的三个不同事实，合并任意两个都会让支付账目不再可审计。

顺手抓到一个自己引入的 bug:加了 `DeliverySettled` 之后，`AttemptDelivery` 会让它落进
"不到期"分支并报 `ErrDeliveryHeld` —— "a delivery is in flight: order o1 until 0"。
重试钩子把这个当成正常，于是**一笔已退款的订单会被每 tick 重试到永远**，而那句胡话是
唯一线索。现在它有自己的 `ErrOrderSettled`。

### 已核实**不是**死路的（我规划时说错的两条）

- `global` 卡在 migrating:`AbortMigration` 就能救回 —— 状态必须是 `RouteMigrating`,
  epoch 从 `Resolve` 拿，两个条件都满足，而且它**本来就在**跨进程接口上。
- `mail` 满了的邮箱 / `chat` 超期未裁剪:前者是有文档的上界（淘汰最旧），后者是存储成本。
  都不是"没有任何路可走"。

## 装配：每个服务一个 `app.Mod`

每个服务提供**业务逻辑包 + 一个 `app.Mod`**，一个 Mod 一个 capability。`app.Service`
进程壳留在业务仓——"哪些服务同进程部署"是部署决策，不是库的决策。

`global` 曾经是例外：一个 Mod 注册两个 capability（路由/租约 + 跨服活动协调），于是
"这个 Mod 叫什么"和"它发布什么"是两件事，活动那个 capability 只能写成一行字面量。
生成器的"一个包一个接口"规则把这件事顶到台面上，于是它被拆成了 `global/` 与
`global/activity/`：两个 Mod、两份配置段（`global:` / `activity:`）、两个错误码段
（5701xx / 6201xx）、两个进程壳。**两者不共享任何类型、任何 store**——拆之前就已经
不共享了，只是同住一个 Go 包里看不出来。

这次拆分是**破坏性变更**（`global.ActivityService` → `activity.Service`，活动的错误码
5701xx → 6201xx），需要 roost-service 的一个大版本。顺手修掉的一件事：`global` 的错误码
段原先在 570111 有一个"永久的洞"，那个洞完全是两个服务从一个号段里发号造成的，现在
两边各自连续。

Mod 只拥有**基建接线**：Redis 客户端来自 registry，key 前缀与各 TTL 来自配置。
凡是配置给不出的东西——`account` 的 `IdentityVerifier`、`platform` 的
`Verifier`/`PlayerResolver`/`Deliverer`、`session` 的 `Releaser`、`chat` 的
`ChannelPolicy`/`SystemAuthenticator`、`directory` 的 `Normalizer`——都是**构造参数、
必填、无默认**，`Init` 会点名拒绝。

这一条不是风格。被替换的实现里，`platform` 的鉴权**根本不存在**，而那个缺席看起来
就像一个默认值;`account` 的 playerID 分配器默认是每进程从 1 开始的计数器。**给这些
东西一个默认值，就是把漏洞装回去。** 所以每个 Mod 都有一条"缺协作者即拒绝启动"的
测试。

key 前缀同样**必填且无默认**：默认值在每个部署里都是同一个字符串，于是共用一个 Redis
的两套部署会静默共享状态。`integration/` 用 before/after 快照回读整个 keyspace，断言
每个服务的新 key 都落在配置的 root 下**且落在自己的子命名空间里**——后半句是必要的：
读错别人的 config key 仍然落在 root 下，只有按服务断言才抓得到（那条变异一度是绿的）。

## 存储：复用，不重写

七个包**没有一行自己的存储代码**。它们的状态就是 `versionstore.Store[K,V]`，而
`roost-kit` 已经提供了这个契约的生产实现 `versionstore.NewRedisStore`：CAS 走
`roost-core/redis.CompareAndSet`、带全抖动指数退避、仅插入的 `Create`、版本校验的
`Delete`。每个包只有一个薄构造器（`redis_store.go`，几十行，零存储逻辑）。

这不是省事，是第 3.1 节那条系统性缺陷的结构性答案：被替换的实现里每个关注点都有
**四个手写 store**——Redis 变体的 `Update` 用 CAS，DAO 变体的 `Update` 读后无条件写
——两者满足同一个接口，于是类型系统分不出它们，配了哪个就决定文档写的不变量成不成立。
现在只有一个实现，而它的契约里没有无条件写。

薄构造器唯一拥有的职责是 **key 命名空间**：多个 store 共用一个 prefix 且不能互撞。
这一条由 `integration/` 里的实测保证——把九个包都驱动一遍，然后 `SCAN` 回读活
keyspace，断言每个声明的命名空间都恰好收到了写。十条"故意撞前缀"的变异全部变红。

真正需要写后端的只有两处，都不是 versionstore 能表达的：`rank` 的 sorted set，和
`mail` 的 `EnvelopeStore`——信封写一次不再改，且读路径是**批量**（一次
`IRedis.MGet`），这正是那个契约有 `GetMany` 的全部理由。
