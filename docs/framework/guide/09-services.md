# 次核心：Service 领域能力：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/09-services.md) · [模块总目录](../README.md)

## 先理解这一块

账号、邮件、聊天、排行等能力具有自己的数据和接口。Service篇说明如何在本进程使用它们，或者把它们拆成单独进程。

owner 是持有服务数据的一方，client 是调用方；领域实现与接口在 service，配置和进程接线在 wiring。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 实际目录归属

| 服务 | 领域实现位置 | 主要责任 |
| --- | --- | --- |
| mail | service/mail | 邮件信封、邮箱、发送意图与领取状态 |
| match | service/match | 排队、分组、匹配结果与取消 |
| session | service/session | 一次运行及关联资源、释放和清扫 |
| account | service/account | 渠道身份验证、会话令牌、选角与建角计划 |
| platform | service/platform | 平台身份和相关管理能力 |
| global | service/global | 静态角色/区服路由 |
| activity | service/activity | 活动组、窗口、协调与结果投递 |
| chat | service/chat | 频道、消息保留与读取 |
| rank | service/rank | 排行更新、查询和幂等 |
| directory | service/directory | 全局名字目录的预留/提交/释放 |

十个领域及服务运行逻辑统一在 service，Wiring 提供本地 Mod、远端 ClientMod 和进程接线；旧领域 alias 已删除。directory 是嵌入能力，game-demo 托管其余九个框架服务，加 game 共十个进程。

## 1A. Session 运行会话

`service/session` 管理副本、试炼等一次运行的记录，领域对象叫 `Run`。它记录 owner、运行种类、请求幂等 ID、期限、终态和外部资源引用。

业务调用 `Enter` 建立运行；分配场景或其他资源后，通过 `Attach` 登记引用；`Finish`、`Leave` 和到期清扫走正式释放流程。资源的含义与具体释放由业务提供的 `Releaser` 实现，配置必须包含该协作者。服务不负责模拟场景、战斗或 Room。

Gate Session 表示网络连接与绑定；Account token 表示登录认证；Sync SessionID 表示同步生命周期。这些身份均与 Run 独立。不能用玩家断线自动代表副本结束，也不能用释放某个 Run 替代关闭玩家连接。

源码入口：[Session 接口](../../../service/session/session_rpc.go)、[Run/Resource](../../../service/session/types.go)、[配置与释放流程](../../../service/session/service.go)、[待清扫入口](../../../service/session/sweep_source.go)。周期运行由领域 Server 持有；Wiring 只构造 runtime 并委托生命周期。

Gate 尚未成为上述目录中的独立服务。当前已提供 `infra/network/gateway` 边界工具和嵌入 Game 的 TCP 接入实现；后续的跨进程路由、回推和会话恢复见[网关现状与独立 Gate 设计](../GATEWAY.md)。

## 1B. Mail 全服邮件

`AudienceBroadcast`（全服 `""` / 单服 / 公会）怎样到达每个邮箱由 `Config.Broadcast`（`mail.Deliverer`）决定；缺省 `nil` 拒绝广播。框架内置一种策略 `mail.NewLazyBroadcast`（维护者 2026-10-20 决定，DESIGN-TODO D17）：全局邮件只存一份信封，个人邮箱拉取合并。

- **接入一行**：生成骨架 `Broadcast()` 返回 `mail.NewLazyBroadcast(mail.LazyBroadcastConfig{})`；owner Mod 给它同一 `key_prefix` 下的 Redis 账本（`<key_prefix>:bcast:ledger` 与版本提示 `<key_prefix>:bcast:version`）。直接 `mail.New` 时自己给 `Index`（`NewRedisBroadcastIndex` / 测试用 `NewMemoryBroadcastIndex`）。
- **全局邮件 ID 与账本**：广播发出时 `Deliver` 把它登记进账本，分配单调递增的全局邮件 ID（`GlobalMail.ID`）。账本是一个版本化文档，“分配 ID 与登记”在同一次 compare-and-set 里完成，所以账本头以下的 ID 全部可见，水位可以只是一个数字。登记按信封 id 幂等，发送重放安全。
- **拉取时机**：登录端点调用一次 `Mail.PullBroadcasts(ctx, playerID, BroadcastProfile)`；`List` / `Summary` 前再检查一次，账本头不高于玩家水位时只花一次 O(1) 的版本读（进程内缓存账本，版本提示没变不重读）。没有调用过 `PullBroadcasts` 的玩家（没有画像）不合并。没配置 LazyBroadcast 时 `PullBroadcasts` 只返回摘要，登录端点可以无条件调用。
- **画像**：`BroadcastProfile{Scopes, RegisteredAtUnix, Attributes}` 由登录端点给出、记在邮箱里，`List` / `Summary` 用最近一次的画像。`Scopes` 用 `mail.ServerScope(sid)`、`mail.GuildScope(guildID)`（发件方用同一写法填 `SendRequest.Scope`）；公会是业务提供的成员关系：入会 / 退会后要再调一次 `PullBroadcasts`，否则按旧画像合并、水位越过新公会已发出的邮件。`RegisteredAtUnix` 必填，取 account 的 `Role.CreatedAtUnix`（game-demo 由认证器写进 `svcaccount.RoleCreatedAtClaim`，`enter_game` 读出）。
- **合并**：每个玩家一个水位（`Mailbox.BroadcastWatermark`，已合并到的最大全局邮件 ID）。合并在邮箱的同一次 compare-and-set 里把 ID 大于水位、范围匹配、未过期、未撤回、满足条件的全局邮件写成条目（键是广播信封 id，`Entry.GlobalID` / `Item.GlobalID` 记全局邮件 ID），并把水位推进到账本头。并发登录 + List、重复拉取只合并一次；已读 / 已领 / 删除记在个人这条上，附件沿用恒定 token 只领一次；删除后条目即使被保留策略清掉，水位也挡住再次合并。合并来的范围邮件领取时不必再给 scope（条目带 `GlobalID` 即为收件凭证）。邮箱满（全是未读）时合并停在放不下的那封之前、水位不越过它，腾出位置后补上，并计 `refused:merge_broadcast:mailbox_full`。
- **条件**：内置“注册时间早于发送时间”（严格早于，按秒），新注册的玩家收不到旧补偿；业务谓词 `LazyBroadcastConfig.Eligible(profile, mail)` 在其后过滤，输入放进 `Attributes`，必须是纯函数、不做 I/O（它在 compare-and-set 回调里，可能执行多次）。被判为不满足的邮件水位越过它，之后不再重新判断。
- **有效期**：全局邮件过期后不再合并；已合并的那份按个人邮件的有效期处理——条目的过期就是信封自己的 `ExpiresAtUnix`（与全局邮件相同），撤回或从账本回收都不影响它。
- **撤回**：`Mail.RevokeBroadcast(mailID)`：未合并的不再合并，已合并的不动；重复撤回返回 false，账本里没有（不是 lazy 广播或已过期）报 `ErrMailMissing`。
- **账本上界与回收**：未过期的全局邮件最多 `LazyBroadcastConfig.MaxActive` 条（缺省 `DefaultMaxActiveBroadcasts` = 1000），超过时发布以 `ErrBroadcastFull`（590116）拒绝，发送方看到 `ErrConflict` 并可重试；过期记录在下一次发布 / 撤回时从账本移除，撤回的记录保留到过期（挡住发送重放再次登记）。账本因此至多是“上界条数 + 上次写入后才过期的条数”。

源码入口：[broadcast.go](../../../service/mail/broadcast.go)、[service.go](../../../service/mail/service.go) 的 `PullBroadcasts` / `refreshMailbox`、[mail_mod.go](../../../wiring/mail/mail_mod.go)。剩余边界见 [KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)。

## 2. owner 与调用方

owner Mod 持有存储并发布公开接口与 .local；Server 注册总线 handler 并运行周期工作。调用方使用 ClientMod/BusClient，只依赖公开契约。同进程也通过同一能力接口；业务不要依赖把公开接口断言成具体 Service。

RPC 生成物分传输半与装配半：接口包持有 wire/handler/BusClient，wiring 持有接线 Server/ClientMod/OwnerCapabilities。运维 Admin、Sweep、ForceRelease 等本地能力不应自动暴露成任意总线调用。affinity 需要 discovery 支撑，缺失时不能装成“看似有效”的路由。

## 3. 存储与幂等

这些服务多数以 Redis/versionstore 存储自己的记录，不是玩家 DAO WAL 的旁路。key_prefix 必填且按环境隔离；rank 的 Lua 与 mail 信封等有独立原语。写操作用请求 ID/令牌/状态条件保证业务重试，传输超时和业务拒绝通过不同错误表达。

versionstore 写结果未知仍需调用方保留不确定状态，不能假设每种服务都能自动恢复所有未知结果。重试时必须保持同一个幂等身份和输入摘要；禁止以新 ID 反复发起可能已经生效的扣发。

## 4. 关键业务边界

account 的身份验证、玩家号分配、名字校验由接入方提供，不能用默认信任身份或进程内计数器替代。一个账号一区服一角色由 slot insert-only/建角计划约束；每个 slot 与角色都带建角身份（计划 ID / `CreationID`），缺计划的 slot、缺 `CreationID` 的角色不被推断，分别以 `ErrConflict` / `ErrNotResolvable` 拒绝并交线下处理；账号 ID 只用转义渠道后的形态（`Identity.AccountID`）。SelectRole 的 accountID 属可信服务边界，外部请求必须先绑定已认证身份。

**account 的别名合服**（DESIGN-TODO D15，维护者 2026-10-20 决定）：`GameServer.MergedInto` 非 0 表示该服已并入另一服。角色的 `ServerID` 是原服（建角时写定、slot 与名字都按它，永不改写），返回给调用方的 `Role` / `Session` 另带只读的 `ServingServerID`（承载服），由 account 内唯一的解析点 `servingServer` 填入：没有合服时等于原服，已并入时是 `MergedInto`。记录永远只有一跳（`MergeServer` 拍平：并入时把原来并入来源服的服一并改指新的承载服，目标服自己不能已并入）；解析遇到承载服缺失报 `ErrServerInvalid`、遇到第二跳报 `ErrServerMergeCycle`，角色登录失败而不猜一个服。存储里的角色记录不带 `ServingServerID`。消费者一律按承载服路由与驻留：Gate 的 `AccountAuthenticator` 用它填 `Principal.ServerID`，嵌入 TCP 的认证器写进 `ServerIDClaim`，原服在 `OriginServerIDClaim`（显示规则由业务决定）。已并入的服 `CreateRole` 一律 `ErrServerClosed`（与状态无关）；`UpsertServer` 不碰合服字段。`MergeServer` / `UnmergeServer` 是 `Admin` 上的本地方法（不上总线，game-demo 经 `cmd/accountctl merge-server / unmerge-server / show-server` 调用），备注必填、写进服务器记录；每条记录按版本条件写，跨记录不是原子的：写入顺序保证中途停下不会出现两跳，结束时复核，并发合服造成的两跳报 `ErrServerMergeCycle` 并给出修复命令。`Accounts.GetServer` 是只读的总线方法：装配了 account、承载玩家的 game 进程启动时经 `wiring/account.ServingGuardMod` 用它确认本服没有被并入（已并入则拒绝启动，DataEngine Mod 排在它之后）。跟着 sid 走的数据与运维步骤见 [PLAYER-MIGRATION §6](../PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)（[service/account/merge.go](../../../service/account/merge.go)、`merge_test.go`）。

**match 的已结束记录只保留固定时长**（维护者 2026-10-19 方案 A）：一个队列的全部状态（等待票、已结束的票、比赛、排队请求记录）是一个版本化值，Redis 存储每次 Enqueue / Commit / Sweep 都整体读写它。已成局 / 取消 / 过期的票、已提交的比赛与排队请求记录从结束起至少保留 `match.retention`（缺省 10 分钟，是票期限缺省值的两倍，覆盖匹配器与客户端的重试和玩家轮询结果），之后在下一次写入或 Sweep 时移除；比赛与它的票同时移除，等待中的票不论等了多久都不动。保留期内的读票、读比赛、同一 requestID 的 Enqueue 重放、同一票集合的 Commit 重放都返回原结果；超期后读到不存在，Commit 重放报 `ErrTicketMissing`（被移除的票 ID 不会回到等待状态，不会成第二局），同一 requestID 的 Enqueue 是一次新的排队。状态上限因此是“等待票（≤ `MaxQueueLength`）+ 保留期内的成局量”（[store.go](../../../service/match/store.go)、[queue_store.go](../../../service/match/queue_store.go) 的 `pruneResolvedLocked`）。开局后的长期身份（Match ID → 房间宿主 → 结算实体）由业务保存，匹配记录不是对局档案。

**match 的大队列按 Partition 分键**（维护者 2026-10-19 批准）：上面说的“一个队列”是一个分区——`Queue{Mode, GroupSize, Partition}` 的 Redis 键是 `<key_prefix>:queue:{<Mode>:<GroupSize>}:<Partition>`（`match.RedisKey`），整体读写与单键争用只发生在一个分区内，Enqueue / Cancel / Ticket / Candidates / Commit / Sweep 都只碰自己的分区，调用按 `Queue.Key()` 路由。所以大队列的推荐用法是按业务维度拆分区：段位档（MMR 区间）、地区（延迟域）、或 sid 组（game-demo 只为本进程服务的玩家组局，按 sid 分区正好对应）；不同分区的票不会被单分区 Commit 放进同一局。规模建议：本机 loopback Redis 实测（2026-10-19，机器有后台负载，单客户端串行）每张等待票约 400 字节，分区里 100 / 1000 / 3000 张等待票时单次 Enqueue 约 0.9 / 6.8 / 18.8 ms——耗时随分区状态线性增长，同一分区的写串行，所以常态等待票宜保持在几百、最多一千张左右；`MaxQueueLength`（10000）是拒绝上限，不是工作点。代价：分区越细，单分区凑齐一局越慢；每个分区要各自写进 `match.sweep_queues`（`mode:size:partition`）才有后台过期扫描；hash tag 是逻辑队列，Redis Cluster 下一个逻辑队列的全部分区在同一个 slot（同一节点），分区降低的是单次读写量与键争用，不把一个逻辑队列摊到多个节点（不同逻辑队列仍分散）。

**跨分区成局用 `CommitAcross`**：需要“放宽匹配”、一局票来自同一逻辑队列（Mode + GroupSize 相同、只差 Partition）的几个分区时，调用 `CommitAcross(ctx, TicketRefs)`（每张票带自己的分区 Queue）。它在一次多键比较并设置里读全部涉及的分区（Redis 上是一个多键 Lua 脚本，`fredis.CompareAndSetAll` / `versionstore.MultiUpdater`），校验每张票仍在自己的分区等待、主体不重复，然后在每个涉及的分区写入同一场比赛并标记票已匹配——要么所有分区同时生效，要么一个分区也不改；Mode 或 GroupSize 不同的分区以 `ErrQueueInvalid` 拒绝，全部票在一个分区时等同于 Commit。语义与 Commit 一致：保留期内同一票集合（顺序可不同）的重放返回原 Match，超期报 `ErrTicketMissing`。比赛在每个涉及的分区各存一份完整副本（`Match.TicketPartitions` 记每张票的分区），所以用任一涉及分区的 Queue 调 `Match`、或用票自己的分区调 `Ticket` 再按 MatchID 读，都能找到它；保留期清理按分区各自进行，每个分区里“比赛与本分区的票同进同出”，截止点相同（成局时刻 + `match.retention`），保留期内所有分区都还在，超期后各分区在各自下一次写入或 Sweep 时移除。何时用：只在主分区凑不齐、需要放宽的少数局使用；常规成局仍走单分区 Commit——一次 CommitAcross 读写几个分区的完整状态，并与这些分区上的单分区操作竞争，按逻辑队列路由（`TicketRefs.LogicalKey()`）。键名加 hash tag 不迁移旧键（尚未上线，CHANGELOG 写明）；`key_prefix` 自带 hash tag 也可以（整个匹配服务一个 slot），但第一对花括号不能为空，`NewRedisStore` 会拒绝（[store.go](../../../service/match/store.go) 的 `CommitAcross` 注释，`TestCommitAcrossIsAllOrNothing`、`TestCommitAcrossRacesSinglePartitionOperations`、`TestCrossPartitionCommitRunsOnRedisCluster`）。

Player 静态绑定 sid，不自动做动态玩家租约迁移。directory 名字域是全局域；activity groups_file 明确组与 game 列表；每个开窗条目都带持久计划，没有计划的条目与超过 `MaxPendingActivities` 的窗口按畸形数据拒绝（报告并保留名额 / sweep 返回 `ErrConflict`），由运维 `RemoveMalformedWindowEntry` 或线下修复，不自动回收、不轮转扫描。activity 的观察重试不自动耗尽失联投递，无法恢复的目标按人工 ExhaustDispatch 等管理流程收口。

**名字规则属于业务**（维护者 2026-10-17 决定）：角色名、公会名等的长度上限、敏感词、全服唯一都是业务规则，框架层不处理，生成的工程与 game-demo 也不内置（game-demo 的 `guild.ValidateName` 只拒绝空、全空白、控制字符与非法 UTF-8）。需要全服唯一时用 `service/directory` 的两阶段占名，不要用“先查有没有、再写”的两步做法（并发下两个请求都看到“没有”）：

- 装配：`wiring/directory.NewMod(normalize, reporter)`，依赖 redis Mod；`directory.key_prefix` 与 `directory.reservation_ttl` 必填、没有缺省。`normalize` 决定哪些写法算同一个名字（`directory.NormalizeLower` 去首尾空白并折叠大小写），是产品决定、不是配置项。业务从 registry 用 `app.Lookup[directory.Directory](r, mods.ModDirectory)` 取能力。
- 顺序：先 `Reserve(ctx, name, owner, ttl)` 占名（别人持有时 `ErrKeyTaken`，可回给客户端“名字已被占用”）→ 再做创建（保留 ID、建实体、事务）→ 成功后 `Commit(claim)` 转为永久；创建失败 `Cancel(claim)` 释放（重复 Cancel 成功，回滚可以重试）。改名、解散走 `Release(name, owner)`，只有持有者能释放。
- 预约带过期：`ttl`（缺省 `reservation_ttl`）内没有 Commit 的预约自动失效，调用方在 Reserve 与 Commit 之间崩溃时名字到期释放，不会被永久占死。`ttl` 要长于从 Reserve 到 Commit 的最长路径；创建已生效而 Commit 没做成（结果未知、进程崩溃）时，预约过期后名字可能被别人拿走，业务按自己的对账决定怎样处理。
- 同一 owner 重复 Reserve 拿到同一个 claim（重试不多占），所以 directory 不是互斥锁：“只允许一次尝试”要用 insert-only 的版本化状态（见 [包注释](../../../service/directory/directory.go)）。

## 5. 接入检查

分别验证 owner、本地调用、跨进程调用、重复请求、回复丢失、恢复与停止。业务期限使用业务时钟，系统令牌/裁剪/网络超时使用系统时钟。亚秒期限不能通过整数秒截断后改变行为。默认服务指标可统一关闭，但不能把关闭指标当成关闭周期清扫。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
