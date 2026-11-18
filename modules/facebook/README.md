# facebook：每名玩家一个伴生的社交实体——任何 game 都能投递的邀请收件箱

Player 只归它的承载 game 写，别的 game 写不了它的档，所以“把一份邀请交给连在别的 game 上、或此刻离线的玩家”不能写在 Player 上。facebook 给每名玩家配一个伴生的 **Facebook 实体**：共享模式的 Remote 实体（与 team 的 Team 一样归 game 服务），号由玩家号直接推出，任何 game 进程都能在 Nest 事务里写它。邀请记进收件人的 Facebook，再告知收件人连着的那个 game 进程推送。

## 能力

- **实体与号**：`game/entities/facebook` 的 `Facebook`（`category=entity.EntityCategoryRemote remote=managed service=<game> ownership=shared`，共享 kind：第一次写它时 Remote 写准入一条插入就把所有权记录写成共享的，两个 game 同时第一次写都成功，没有“先独占认领、再切共享”的中间态；不能退出共享模式）。号是 `entity.BuildEntityID(玩家号, facebook.EntityKindFacebook)`——与 Player 同一个唯一号、不同 kind，不要发号器，只用 `GetOrCreate`，不用 `Create`。号与承载 sid 无关：玩家换服（rehome）时 Facebook 不用搬。
- **状态**（`db/def/facebook.go`）：`FacebookDao`（集合 `facebook`，主库），只落库。`Invites` 是收件箱：发出邀请的实体完整号（如队伍号）→ `FacebookInvite{From 邀请人玩家号, ExpireAt 过期时刻}`。
- **组件** `Facebook`（`game/components/facebook/facebook.go`，对外接口 `view.Facebook`，handler 收 `view.IFacebookEntity`）：`Note(source, from, expireAt, now) bool`、`Invites(now) []view.FacebookInvite`。规则常量 `MaxInvites = 64` 归工程所有。
- **Nest handler**（`game/handler/`，都只锁一个 Facebook）：`handlerFacebookNote`（`rollback=undo`，持久级别取缺省 async：回复时记录已写进本进程 WAL，Mongo 权威要等投影，别的 game 立刻查这个收件箱可能还看不到；告知照常发出，收件人凭推送里的来源号照样能接受）、`handlerFacebookInvites`（只读，`durability=memory`）。没有客户端协议：查询由用它的功能经自己的协议给出（team 的 `TeamInvites`）。
- **服务端入口**（`game/modules/facebook`）
  - `EntityID(player)`：玩家的 Facebook 号。
  - `Ensure(ctx, registry, player)`：把 Facebook 装进本进程内存（`GetOrCreate`，没有就新建一个空的），返回号。不认领、不切共享（共享 kind 由第一次写入认领成共享）。要 I/O，只在 Nest handler 之外调用。
  - `Deliver(ctx, registry, nestClient, Invite{To, Source, From, ExpireAt})`：一份**已经在来源实体上成交**的邀请投给 `To`：`Ensure` → Nest 记进收件箱 → 告知（本进程直接交给登记的处理，并在同步总线 `Topic` 主题上广播给别的 game 进程）。告知不看记录成没成（推送带着来源号，收件人照样能接受）；返回记录这一步的错误（没记下是 `ErrNotRecorded`），调用方只记日志。
  - `Invites(ctx, nestClient, player)`：读收件箱里还没过期的邀请；Facebook 还没有所有权记录（从没投递过）时只读一次所有权就答空、不经 Nest（省一次写准入）；记录在、还没有提交（别人第一次写到一半）时答空而不是报错。
- **登记点**：`OnInvite(kind, Notify)`——来源实体种类为 `kind` 的邀请投递之后、在每个收到告知的 game 进程上调用的处理（team 登记队伍的：被邀请人连在本进程就推 `TeamInvited`）。只在 `init` 登记，nil、重复登记 panic。处理在投递或总线回调的 goroutine 里执行，不在实体锁里，自己判断收件人是否连在本进程。
- **运行时**（清单 `runtime.start`）：`Start` 订阅同步总线的 `facebook` 主题（JetStream 时用可确认订阅、DeliverNew，不回放旧告知；普通 NATS 至多一次），订阅登记为 App 的外部输入，停机第一步退订并排空。
- **配置与错误码**：没有配置键与错误码。

**日志**：投递、广播、推送的失败记 Debug（邀请已在来源实体上成交，投递只是告知）；收件箱只读不记成功日志。

## 能力边界

- **收件箱是提示，不是权威**：邀请成立与否以来源实体为准，接受时由来源实体的事务核对；收件箱里的记录可能已经被接受、撤销或满员，用它的功能查询时要逐条核对（team 的 `TeamInvites` 就这样做）。
- **投递在来源事务之后**：不在同一笔事务里。确保、记录、广播任何一步失败都不撤销来源上的邀请；重发邀请会再投一次，推送可能重复，客户端按来源号合并。
- **告知是广播**：每次投递向全部 game 进程广播一条，收件人连着的那个进程推送；没有按“收件人在哪个 game”路由。代价是每次投递 game 进程数个接收者各判一次在线。普通 NATS 至多一次，丢了由收件箱补查；收到时已过期的丢弃。
- **谁建 Facebook**：第一次要写它的 game（目前是发邀请的一方）在事务之外装进内存，第一次写入建立共享的所有权记录。邀请一个从没登录过、甚至不存在的玩家号也会建出一个 Facebook（每个号至多一个、只有一份收件箱）。两个 game 同时第一次投给同一个人都记得上；第一次写的 game 认领之后、提交之前停了，留下的是一条共享的空记录，别的 game 照常写（2026-11-18 起，之前是“认领 → 切共享”两步，会有 `ErrOpening`）。
- **数量上限**：收件箱至多 64 条（各来源合计）；记满时新的不记、已有的可以续期；过期的在下一次记录时清掉；同一来源的新邀请只延长、不缩短，恰好到期那一刻算过期。
- **不做的事**：没有拒绝邀请、已读、离线推送补发（登录后由客户端查询）、好友申请等别的社交记录。
- **前提**：需要 nest 与 remote_entity 两个 Mod（安装时自动补上 remote_entity，它带上同步总线）；运行时需要 Mongo、Redis、NATS。

## 复用方式

- **需求正好是它**：通常不单独装，由 team 依赖带上。要别的功能也跨 game 投邀请：在来源事务提交之后调 `facebook.Deliver`，在 `init` 里用 `facebook.OnInvite(<来源实体种类>, …)` 登记告知处理，查询时调 `facebook.Invites` 并按来源种类过滤、到来源实体上核对。
- **需求多一点**：改收件箱上限改 `game/components/facebook/facebook.go` 的 `MaxInvites`；收件箱以外的社交记录（好友申请、关注）加在 `FacebookDao` 上并写对应的 handler，同样只锁一个 Facebook。
- **不要改**：`FacebookDao` 的 store 键与集合名、`EntityCategoryRemote` 的实体类别与 `ownership=shared`（去掉它第一次写就把 Facebook 独占给一个 game，别的 game 再也投递不进来）、号由玩家号推出的规则（换了就不再一一对应）。
- **不适合直接用的情形**：只在一个进程里、不要落库的提醒（直接推送即可）；需要强一致“邀请成功 ⇒ 收件箱一定有”的业务（把 Facebook 放进来源的同一笔事务，代价是多一个共享 Remote 的分布式锁）。

## 联动

- team 依赖它（`requires: [facebook]`）：队伍邀请提交之后经 `Deliver` 投给被邀请人，`TeamInvites` 读收件箱，`OnInvite` 推 `TeamInvited`。facebook 没有 `with/` 目录。

## 验证范围

- `roost module verify facebook`：9 条场景（`^TestFacebook`，包 `./game/handler` 4 条、`./game/modules/facebook` 5 条）。handler 场景用真实 Nest 引擎与生成的 DAO / Sender，提交器、Remote 外部锁与权威回执是内存替身：记录与列出、恰好到期即失效、提交被拒不留记录、非法输入零写入、有界与清理、迟到的旧告知不缩短续期、记录是一次 Remote 写。模块场景用替身验编排：确保只装进内存（不认领、不切共享）、Facebook 的 kind 声明为共享 kind、投递先记后告知且失败照样告知、广播内容、别的 game 广播来的告知交给登记的处理而过期 / 坏 / 键不符的不交、停机关输入后不再处理、没打开的收件箱不经 Nest 答空。共享 kind 的并发首写与首写后崩溃在框架层验（`framework/remoteentity` 的 `TestSharedKind*`，真实 Mongo / Redis 的 `TestRealSharedKindFirstWritesFromTwoProcesses`）。
- 真实依赖：`scripts/test-alliance-e2e.sh` 的 `TestAllianceE2ESocialTeamInvite`（定向入口 `scripts/test-social-real.sh`）：生成 game-demo，两个 game 进程、真实 Mongo / Redis / NATS，A 在 game 1000 邀请连在 game 1001 的 B：B 收到推送、查得到并用投递来的号接受；邀请时离线的 C 再登录后查到并接受。证据见 `docs/maintenance/evidence/social-facebook-entity-20261117/`。
