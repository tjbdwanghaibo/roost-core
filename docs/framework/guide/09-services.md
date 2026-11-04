# 次核心：Service 领域能力：设计与使用

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/09-services.md) · [模块总目录](../README.md)

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

十个领域及服务运行逻辑统一在 service，Wiring 提供本地 Mod、远端 ClientMod 和进程接线；旧领域 alias 已删除。directory 是嵌入能力，game-demo 托管其余九个框架服务，加 game 与 alliance（联盟服务骨架，见[联盟服务方案](../../maintenance/FEATURE-2026-11-04-alliance-service.md)）共十一个进程。

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

接收端期限：普通 Bus RPC（`Bus.HandleRpc` / `CallTo`）的 handler 拿到的 `RpcContext.Context()` 是接收端 Bus 的基础 context（[dispatchRpc](../../../infra/network/bus/bus.go)），只在 Bus 停止时取消；调用方的 deadline 不随请求传过来，调用方超时只是停止等待。JetStream RPC 按调用方截止建立处理期限。业务自注册的普通 RPC 要给整次处理自己的预算，预算过后的成功不回复成功、交给调用方按原身份重试；接法与真实 NATS 上的核对见 [SLG §7.7](../SLG-INTEGRATION.md#77-进阶深化联合链路的接法要点)（`receiverBudget`）。

接收端并发与顺序（[rpc_dispatch.go](../../../infra/network/bus/rpc_dispatch.go)，轻量 RPC 与 JetStream RPC 同一规则）：请求信封可以带顺序键 `OrderKey`（客户端经 `bus.WithOrderKey(ctx, key)` 设置）。带键的请求在接收实例上按键排队，同一个键的请求按到达顺序逐个进 handler，前一个答复发出之后才轮到下一个；不同键之间、不带键的请求之间互不等待，同一方法的请求也并行。路由 RPC 的键是目标实体 ID（`CallRoutedChecked` 自动设置），affinity 方法的键是 affinity 键（`CallDiscoveredChecked` 在 `KeyAffinityPicker` 取到键时设置），不标 route / affinity 的方法（创建类、mail 等）不带键、不承诺顺序。生成代码不用改。本实例在跑与排队的 RPC 请求合计不超过 `bus.Config.RPCMaxPending`（缺省 `nats.worker_num` × 队列容量 1024），超出的轻量 RPC 答 `CodeRetryable`（确定没执行），JetStream RPC 交还 broker 重投；RPC 不占模块消息的派发 worker。模块消息（`Bus.Handle`）仍按目标模块派到固定 worker，同一模块的消息按到达顺序逐个处理。JetStream RPC 的 consumer 回调推迟结算（`DeferSettle`）、交给同一套派发后立即返回，handler 返回、应答发布之后才确认，排队期间照常发 in-progress，排队时过了调用方期限的请求确认、不执行。顺序只在一个接收实例上成立：发往不同实例的、调用方并发发出的请求不在承诺之内。边界：同一实例上，一个键的 handler 同步调用发往本实例、同一个键的 RPC 会排在自己后面，等到调用方超时，业务不要在 handler 里对同一实体发同步 RPC 回到自己。此前按方法名哈希选派发 worker，同一方法在一个实例上完全串行（联盟阶段 5：`Info` 约 100 次/秒、`Join` 约 13 次/秒、`Found` 约 9 个/秒，不随实体数增长）；没有改成“按键哈希到固定 worker”，是因为 handler 同步等 Nest 并可能同步调别的实例（入会 `Join` 调守卫实例的 `ReserveGuard`），固定 worker 让无关的键共用 worker，两个实例互相调用时会跨实例互等到超时。

## 2A. 路由 RPC 与改投

实体服务（例如独占全部联盟的 alliance 服务，见[联盟服务方案](../../maintenance/FEATURE-2026-11-04-alliance-service.md#27-分片接管与迁移决定-2)）有多个实例时，某个实体归哪个实例只由 Remote 的持久所有权记录决定。实体 ID 不编码实例、没有分片配置；owner 加载实体时在 Redis 登记一份带过期的路由提示，调用方按它找 owner，找错了由服务端改投纠正。不用 `KeyAffinityPicker`：它按当前发现到的实例取模，实例增减时映射跟着变。

**两类方法**：

- **路由 RPC**：对已有实体的写与权威读。路由键是完整 Entity ID（kind 在 ID 里）。客户端用 `(*servicerpc.BusClient).CallRoutedChecked(ctx, resolver, entityID, method, req, resp, fallback)`，查找顺序是本客户端的路由缓存 → 查找接口 `servicerpc.RouteResolver`（Redis 登记 → 持久所有权记录）。都没有时答“实体不存在”（`servicerpc.EntityNotFound`，`errcode.CodeEntityNotFound`，业务拒绝），请求不发出去。
- **创建类 RPC**（新建实体）：不标路由，走生成客户端缺省的队列组（`Bus.Call`，NATS 只在已注册 handler 的实例里选）。接到的实例新建实体、经首写认领所有权、登记路由。创建的幂等锚在一个按稳定 ID 路由的实体上，或由调用方在路由之前定下新实体 ID（方案 2.3）。

**客户端（`infra/network/servicerpc`，`routed.go`）**：

- **缓存**：每个 `BusClient` 一份，按实体 ID 存 owner sid，有效期 `RouteCacheTTL`（30s，与 Redis 登记的 TTL 相同），最多 `RouteCacheCapacity`（65536）条、LRU 淘汰。缓存只是提示，正确性靠改投。不做“实体不存在”的负缓存：刚建好的实体会被别的进程立刻访问。
- **改投**：服务端答复 `CodeRedirect` 且响应实现 `RedirectProvider`（`RedirectSID() int32`）给出另一个 sid 时，缓存改成它，改投一次。改投后仍不对答复 `CodeRedirect`（归“可重试”）。
- **no responders**：目标 sid 没有这个方法的订阅（owner 下线、缩容后的旧缓存、恢复中；NATS 客户端已按策略重试过）时删掉缓存，经查找接口重新查一次，查到不同的 sid 才再发一次，否则原样返回（可重试，未执行）。
- 全部尝试共用 `BusClient` 的一份 timeout 预算，再次发送前把响应清零（`resp` 必须是指针）。必须在 I/O 段调用。查找接口的其他错误（Redis 与所有权记录都读不了）按 `Retryable` 返回。
- 指标：`servicerpc_route_lookup_total{service,source=cache|resolver}`、`servicerpc_route_redirect_total{service}`、`servicerpc_route_relookup_total{service}`。

**取用查找接口**：`wiring/remoteentity.RouteSource(registry)` 返回注册表里的 `mods.ModRemoteRoute`。owner 进程由 `RemoteEntityMod` 提供（所有权记录经写 Manager 读）；只读进程由 `RemoteMirrorMod` 提供（所有权记录经只读的 Mongo 查询读，需要 mongo Mod；用 `WithMirrorLoader` 换掉 Mongo 又没装 mongo 时没有这个能力）。

**服务端**：

- **前置检查**：路由 RPC 的 handler 在进 Nest 之前调 `wiring/remoteentity.RPCRouteCheck(ctx, owners, localSID, id)`（签名与 `RPCWriteAnswer` 同一风格，`owners` 是注册表里的 Remote Manager）。实体已在本地加载、所有权是本 sid：返回 nil，不做 I/O。否则读持久所有权记录：没有记录答 `servicerpc.EntityNotFound`（非创建类请求不得进入写准入，否则首写会把一个不存在的 ID 认领走）；owner 是本 sid 就冷加载、登记路由，返回 nil；owner 是别的 sid 答 `servicerpc.Redirect(owner)`；读不了或加载失败答 `Retryable`。进 Nest 之后写准入照常按所有权记录 + fence 裁决，前置检查只是省掉一次白进 Nest。
- **写的答复**：Remote 实体写的错误经 `wiring/remoteentity.RPCWriteAnswer(ctx, owners, localSID, id, err)` 转成答复：fence 时读一次权威所有权，owner 是别的 sid 就改投；重载、转移中、写额度满、准入版本冲突给可重试；结果未知原样返回。它读所有权要 I/O，只能在 RPC handler（I/O 段）里调。
- **服务端答复**：`servicerpc.Redirect(ownerSID, cause)`（owner 未知时退化为 `Retryable`）、`servicerpc.Retryable(cause)`、`servicerpc.EntityNotFound(id)`；响应的状态码照旧取 `servicerpc.Error(answer)`，改投目标取 `servicerpc.RedirectSID(answer)`。框架码 `errcode.CodeRetryable = 4`、`errcode.CodeRedirect = 5`。

**路由登记、预加载与缩容**（契约见 [REMOTE_ENTITY“路由登记”](../REMOTE_ENTITY.md#路由登记)）：

- 登记的 kind 经 `wiring/remoteentity.WithRouteKinds(kinds...)` 交给本服务的 `RemoteEntityMod`（生成器由实体注解 `service=` 推出，不是 YAML 键）。
- 起服预加载是实体类型的显式策略：`WithPreloadKinds(kinds...)` 加 `NewRemotePreloadMod()`；路由 RPC 的 owner Mod 把 `mods.ModRemoteEntityPreload` 写进 `OptionalDependsOn`，预加载完成前不注册 handler。
- 缩容：先让要下线的实例退出创建类方法的队列组（`(*bus.Bus).LeaveRPCQueue(method)`，经 `bus.RPCQueueLeaver` 断言取得；实例主题照常接路由 RPC），再把它持有的实体用 `TransferRemoteOwnership` 转走，确认持有数为 0 后停。只支持轻量 RPC；JetStream RPC 的服务级 consumer 是全部实例共享的 durable，返回 `bus.ErrRPCQueueLeaveUnsupported`。退出后不能重新加入，要重新加入就重启实例。

**四类答复**：`servicerpc.Classify(err)` 给出 `OutcomeOK`、`OutcomeRejected`（业务错误码与“实体不存在”，不重试）、`OutcomeRetryable`（确定没执行：no responders、`CodeRetryable`、`CodeRedirect`，原 RequestID 重试）、`OutcomeUnknown`（超时、取消、`CodeInternal`、传输或解码错误等一切拿不准的，原 RequestID 查结果或重发）。

**现有 NATS 客户端的自动重试**：轻量 RPC 在 no responders 时按 `nats.rpc` 重试策略退避重试，直到次数或调用期限用完（目标在期间恢复时请求就在这次调用里执行）；BusClient 总给调用带期限，单次尝试用满整个期限，超时不会在同一次调用里重发。但调用方自己的重试、JetStream RPC 的重投仍会让同一请求送达多次，所以写 RPC 必须按 RequestID 幂等，超时一律按结果未知。普通 Bus RPC 不把调用方截止时间传到接收端（上一节）。

**恢复完成前不接请求**：App 按依赖顺序逐个 Start Mod（共享 Mod 先于服务专属 Mod），DataEngine Mod 的 Start 在返回前把 WAL 里未投影的记录排空（`Runtime.drainAtStartup`，受 `dataengine.startup_timeout` 约束），Nest Mod 可选依赖 DataEngine 与 `remote_entity`，预加载 Mod 依赖 `remote_entity` 与 `nest`、可选依赖 DataEngine，它的 Start 跑完预加载才返回。所以实体服务的 RPC owner Mod 声明依赖 `nest`（并可选依赖预加载 Mod）时，它在 Start 里注册总线 handler 时恢复与预加载都已完成；之前的轻量 RPC 得到 no responders（“可重试，未执行”）。JetStream RPC 传输下请求先进流、handler 注册后才投递，不是 no responders。

**停机先撤 handler**：App 停机第一步（`Service.Shutdown` 与任何 Mod 停止之前）经 nats Mod 的 `CloseIngress` 调 `(*bus.Bus).CloseIngress`，一次撤下总线上全部已注册的 RPC 方法并停止接收模块消息（[App 生命周期 §4.1](01-app-lifecycle.md#41-停机第一步关外部输入)）。注册 handler 的是 owner Mod（Start 时）、生成的独立进程 `Server`（`Service.Init` 时）还是框架内置服务，都由这里统一撤，各自不必再撤。每个方法走 `WithdrawRpc` 的三步：关准入（之后到达本实例的请求不进 handler，答 `errcode.CodeRetryable`，确定没执行）→ 撤订阅（轻量 RPC 排空并退掉队列组与实例主题，之后发往本 sid 的得到 no responders、不指定 sid 的落到别的实例；排空把服务端处理退订之前已发出的请求照常交给上一步的答复，不丢成超时；集群里经别的节点转来、在退订兴趣传播完之前到达的请求仍会被服务端丢掉、调用方超时，任何订阅退掉都有这个窗口，见 KNOWN-LIMITS）→ 在停机预算内等已进 handler 的请求答复完。JetStream RPC 没有 no responders：停掉该方法的服务级 consumer（共享 durable 交给别的实例），本实例的 consumer 留到 Bus 停止，期间投递来的请求答 `CodeRetryable` 并确认。之后 `HandleRpc` 返回错误。出向的 `Send` / `Call` 与 JetStream RPC 应答 consumer 照常，直到 nats Mod 停止。派发拒绝（在跑与排队的请求到上限、调度器没在跑）的请求同样答 `CodeRetryable`。`WithdrawRpc` 仍可单独用来撤某个方法（缩容），重复撤下是空操作。没有这一步时 handler 一直挂到 nats Mod 停止，中间到达的请求进已停的 Nest，`ErrNestStopped` 经 `RPCWriteAnswer` 原样返回成 `CodeInternal`，调用方只能按结果未知处理（联盟服务两实例冒烟，2026-11-04）。

**多个实例同时建同一个 durable**：effect 消费者、服务级 RPC consumer、saga 步骤消费者都是全部实例共用的 durable，同时起服的实例会同时建它；JetStream 集群这时可能只回答其中一个请求（联盟服务两实例冒烟，失败方等满 nats.go 缺省 5s、`Service.Init` 报 `context deadline exceeded` 退出）。驱动 `JetStreamClient.Subscribe` 建消费者单次 2s，没有回答且调用方 ctx 还没结束时用同一配置重发（`CreateOrUpdate` 幂等），最多 4 次（`nats.jetstream.consumer_create_retry.total` 与 WARN 日志）。回归：`infra/network/nats/driver` 的 `TestSubscribeSendsTheConsumerCreateAgainWhenTheReplyIsLost`（替身模拟没有回答的请求）。

回归：`infra/network/servicerpc` 的 `routed_test.go`；`framework/remoteentity` 的 `route_test.go`、`route_redis_integration_test.go`；`wiring/remoteentity` 的 `route_test.go`、`rpc_answer_test.go`；`infra/network/bus` 的 `rpc_contract_test.go`（`TestLeaveRPCQueueDropsOnlyThatMethodsQueueSubscription`、`TestWithdrawRpcStopContract`、`TestWithdrawnRPCIsAnsweredRetryableAndNeverReachesTheHandler`、`TestWithdrawnJetStreamRPCStopsTheServiceConsumerAndAnswersRetryable`）与真实 NATS 的 `TestRealLeaveRPCQueueStopsNewQueueRequestsReachingTheInstance`、`TestRealWithdrawRpcAnswersEveryRequestDefinitely`；总线关输入与派发拒绝：`infra/network/bus` 的 `TestCloseIngressClosesEveryInputOfTheBus`、`TestRefusedRPCIsAnsweredAtOnceWhenTheQueueIsFull`；接收端并发与顺序：`infra/network/servicerpc` 的 `TestRoutedCallsToDifferentEntitiesRunInParallelAndOneEntityInArrivalOrder`、`TestAffinityAndRoutedCallsCarryAnOrderKey`，`infra/network/bus` 的 `TestRPCWithoutOrderKeyRunsConcurrentlyWithinOneMethod`、`TestJetStreamRPCDispatchesByOrderKeyAndSettlesAfterTheReply`；生成工程的停机：`codegen/internal/roost` 的 `TestGeneratedRPCOwnerHandlersAreWithdrawnWhenTheAppStops`；App 层：`framework/app` 的 `ingress_test.go`。

## 3. 存储与幂等

这些服务多数以 Redis/versionstore 存储自己的记录，不是玩家 DAO WAL 的旁路。key_prefix 必填且按环境隔离；rank 的 Lua 与 mail 信封等有独立原语。写操作用请求 ID/令牌/状态条件保证业务重试，传输超时和业务拒绝通过不同错误表达。

versionstore 写结果未知仍需调用方保留不确定状态，不能假设每种服务都能自动恢复所有未知结果。重试时必须保持同一个幂等身份和输入摘要；禁止以新 ID 反复发起可能已经生效的扣发。

## 4. 关键业务边界

account 的身份验证、玩家号分配、名字校验由接入方提供，不能用默认信任身份或进程内计数器替代。一个账号一区服一角色由 slot insert-only/建角计划约束；每个 slot 与角色都带建角身份（计划 ID / `CreationID`），缺计划的 slot、缺 `CreationID` 的角色不被推断，分别以 `ErrConflict` / `ErrNotResolvable` 拒绝并交线下处理；账号 ID 只用转义渠道后的形态（`Identity.AccountID`）。SelectRole 的 accountID 属可信服务边界，外部请求必须先绑定已认证身份。

**account 的别名合服**（DESIGN-TODO D15，维护者 2026-10-20 决定）：`GameServer.MergedInto` 非 0 表示该服已并入另一服。角色的 `ServerID` 是原服（建角时写定、slot 与名字都按它，永不改写），返回给调用方的 `Role` / `Session` 另带只读的 `ServingServerID`（承载服），由 account 内唯一的解析点 `servingServer` 填入：没有合服时等于原服，已并入时是 `MergedInto`。记录永远只有一跳（`MergeServer` 拍平：并入时把原来并入来源服的服一并改指新的承载服，目标服自己不能已并入）；解析遇到承载服缺失报 `ErrServerInvalid`、遇到第二跳报 `ErrServerMergeCycle`，角色登录失败而不猜一个服。存储里的角色记录不带 `ServingServerID`。消费者一律按承载服路由与驻留：Gate 的 `AccountAuthenticator` 用它填 `Principal.ServerID`，嵌入 TCP 的认证器写进 `ServerIDClaim`，原服在 `OriginServerIDClaim`（显示规则由业务决定）。已并入的服 `CreateRole` 一律 `ErrServerClosed`（与状态无关）；`UpsertServer` 不碰合服字段。`MergeServer` / `UnmergeServer` 是 `Admin` 上的本地方法（不上总线，game-demo 经 `cmd/accountctl merge-server / unmerge-server / show-server` 调用），备注必填、写进服务器记录；每条记录按版本条件写，跨记录不是原子的：写入顺序保证中途停下不会出现两跳，结束时复核，并发合服造成的两跳报 `ErrServerMergeCycle` 并给出修复命令。`Accounts.GetServer` 是只读的总线方法：装配了 account、承载玩家的 game 进程启动时经 `wiring/account.ServingGuardMod` 用它确认本服没有被并入（已并入则拒绝启动，DataEngine Mod 排在它之后）。跟着 sid 走的数据与运维步骤见 [PLAYER-MIGRATION §6](../PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)（[service/account/merge.go](../../../service/account/merge.go)、`merge_test.go`）。

**match 的已结束记录只保留固定时长**（维护者 2026-10-19 方案 A）：一个队列的全部状态（等待票、已结束的票、比赛、排队请求记录）是一个版本化值，Redis 存储每次 Enqueue / Commit / Sweep 都整体读写它。已成局 / 取消 / 过期的票、已提交的比赛与排队请求记录从结束起至少保留 `match.retention`（缺省 10 分钟，是票期限缺省值的两倍，覆盖匹配器与客户端的重试和玩家轮询结果），之后在下一次写入或 Sweep 时移除；比赛与它的票同时移除，等待中的票不论等了多久都不动。保留期内的读票、读比赛、同一 requestID 的 Enqueue 重放、同一票集合的 Commit 重放都返回原结果；超期后读到不存在，Commit 重放报 `ErrTicketMissing`（被移除的票 ID 不会回到等待状态，不会成第二局），同一 requestID 的 Enqueue 是一次新的排队。状态上限因此是“等待票（≤ `MaxQueueLength`）+ 保留期内的成局量”（[store.go](../../../service/match/store.go)、[queue_store.go](../../../service/match/queue_store.go) 的 `pruneResolvedLocked`）。开局后的长期身份（Match ID → 房间宿主 → 结算实体）由业务保存，匹配记录不是对局档案。

**match 的大队列按 Partition 分键**（维护者 2026-10-19 批准）：上面说的“一个队列”是一个分区——`Queue{Mode, GroupSize, Partition}` 的 Redis 键是 `<key_prefix>:queue:{<Mode>:<GroupSize>}:<Partition>`（`match.RedisKey`；弹性人数队列的人数段见下文），整体读写与单键争用只发生在一个分区内，Enqueue / Cancel / Ticket / Candidates / Commit / Sweep 都只碰自己的分区，调用按 `Queue.Key()` 路由。所以大队列的推荐用法是按业务维度拆分区：段位档（MMR 区间）、地区（延迟域）、或 sid 组（game-demo 只为本进程服务的玩家组局，按 sid 分区正好对应）；不同分区的票不会被单分区 Commit 放进同一局。规模建议：本机 loopback Redis 实测（2026-10-19，机器有后台负载，单客户端串行）每张等待票约 400 字节，分区里 100 / 1000 / 3000 张等待票时单次 Enqueue 约 0.9 / 6.8 / 18.8 ms——耗时随分区状态线性增长，同一分区的写串行，所以常态等待票宜保持在几百、最多一千张左右；`MaxQueueLength`（10000）是拒绝上限，不是工作点。代价：分区越细，单分区凑齐一局越慢；每个分区要各自写进 `match.sweep_queues`（`mode:size:partition`）才有后台过期扫描；hash tag 是逻辑队列，Redis Cluster 下一个逻辑队列的全部分区在同一个 slot（同一节点），分区降低的是单次读写量与键争用，不把一个逻辑队列摊到多个节点（不同逻辑队列仍分散）。

**跨分区成局用 `CommitAcross`**：需要“放宽匹配”、一局票来自同一逻辑队列（Mode、GroupSize 与弹性规则相同、只差 Partition）的几个分区时，调用 `CommitAcross(ctx, TicketRefs)`（每张票带自己的分区 Queue）。它在一次多键比较并设置里读全部涉及的分区（Redis 上是一个多键 Lua 脚本，`fredis.CompareAndSetAll` / `versionstore.MultiUpdater`），校验每张票仍在自己的分区等待、主体不重复，然后在每个涉及的分区写入同一场比赛并标记票已匹配——要么所有分区同时生效，要么一个分区也不改；Mode、GroupSize 或弹性规则不同的分区以 `ErrQueueInvalid` 拒绝，全部票在一个分区时等同于 Commit。语义与 Commit 一致：保留期内同一票集合（顺序可不同）的重放返回原 Match，超期报 `ErrTicketMissing`。比赛在每个涉及的分区各存一份完整副本（`Match.TicketPartitions` 记每张票的分区），所以用任一涉及分区的 Queue 调 `Match`、或用票自己的分区调 `Ticket` 再按 MatchID 读，都能找到它；保留期清理按分区各自进行，每个分区里“比赛与本分区的票同进同出”，截止点相同（成局时刻 + `match.retention`），保留期内所有分区都还在，超期后各分区在各自下一次写入或 Sweep 时移除。何时用：只在主分区凑不齐、需要放宽的少数局使用；常规成局仍走单分区 Commit——一次 CommitAcross 读写几个分区的完整状态，并与这些分区上的单分区操作竞争，按逻辑队列路由（`TicketRefs.LogicalKey()`）。键名加 hash tag 不迁移旧键（尚未上线，CHANGELOG 写明）；`key_prefix` 自带 hash tag 也可以（整个匹配服务一个 slot），但第一对花括号不能为空，`NewRedisStore` 会拒绝（[store.go](../../../service/match/store.go) 的 `CommitAcross` 注释，`TestCommitAcrossIsAllOrNothing`、`TestCommitAcrossRacesSinglePartitionOperations`、`TestCrossPartitionCommitRunsOnRedisCluster`）。

**match 的弹性人数成局**（2026-10-31，派对房间纵切发现的缺口）：`Queue.GroupSize` 是一局人数的上限；`MinSize` 非 0 时队列是弹性人数队列（`2 ≤ MinSize < GroupSize`）——凑满 `GroupSize` 立即成局，凑不满时组里最早一张票等满 `FillAfterSeconds`（整秒，按业务时钟；0 表示够 `MinSize` 就开）后 `MinSize..GroupSize-1` 人也可以成局。规则只有一处实现 `Queue.FormableSize(可用张数, 最早一张的等待)`：`FirstComeGrouping`（弹性队列要给 `NowUnix`）与 `ScoreWindowGrouping` 用它决定取几张，`Commit` / `CommitAcross` 在同一次比较并设置里按存储时钟（`Config.Now`，Mod 注入业务时钟）用它校验，过早的不满员局报 `ErrGroupNotReady`（550110）且状态不变；重放判定在它之前，已成的局在时钟回拨后重放仍返回原局；时钟回拨时等待按 0 计，不会因此提前开局；等待按 Unix 秒取整，与票期限同一精度（最多提前不到 1 秒）。“同一票集合并发只成一局”“同 requestID 幂等”与分区容量建议不变：大小不同、互相重叠的票集合争同一批票时只有一个提交成功，其余 `ErrConflict`。`MinSize` 与 `FillAfterSeconds` 是队列身份的一部分（与 `GroupSize` 一样在代码里声明，Mod 没有新配置键）：逻辑队列的人数段固定队列写 `8`、弹性队列写 `2-8@20s`（`Queue.LogicalKey()`，例如键 `<key_prefix>:queue:{party:2-8@20s}:eu`），不同规则的队列是不同的池；`match.sweep_queues` 条目用同一种写法（`party:2-8@20s:eu`）。`FillAfterSeconds` 不短于 `match.ticket_ttl` 的队列在 Enqueue 与启动（sweep 队列）时以 `ErrQueueInvalid` 拒绝。匹配器的“候选够不够一局”预判用 `queue.MinGroupSize()`，不要写死 `GroupSize`。两种存储跑同一段规则代码（Redis 存储是 versionstore 的通用比较并设置脚本），`TestFlexibleQueueFormsASmallGroupOnlyAfterTheFillWait`、`TestConcurrentFlexibleCommitsProduceExactlyOneMatch`、`TestCommitAcrossFollowsTheFlexibleQueueRule` 与真实 Redis 上的 `TestMatchFlexibleGroupingOnRedis`（`wiring/integration`）钉住。已开局房间的中途补位按业务层写法（房间票 + 玩家票在单独的补位队列里弹性成局，进房 handler 按座位拒绝超员，[派对手册 §6A](../PARTY-ROOM-INTEGRATION.md#6a-补位业务层写法)）；存储原生补位（不超员由存储保证）未提供，见 [FEATURE-2026-10-31 第 3 节](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-31-match-flexible-grouping.md#3-第二阶段中途补位backfill待维护者选择)方案 A。

**match 的保留期也计入分区容量**（2026-10-20 夜间测量，[夜间补测 §11](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md#11-第-11-项匹配队列容量与-redis-cluster-故障切换)）：上面的等待票建议只看等待票，保留期内的已结束记录同样在每次整体读写里（每名已成局玩家约 510 B），所以分区状态按“每秒成局人数 × 已过时间”增长、单次操作耗时随之线性上升。单分区“每秒成局人数 × `match.retention`”的持续上限约 **4500 人**（状态约 2.3 MB）；4 分区各每秒成局 10 人、保留期 10 分钟（每分区约 6000 人）时第 8 分钟崩溃：CAS 冲突比例升到 0.94、请求超时，同一 Redis 上无关命令的 PING P99 升到 0.6 s——Redis 单线程执行几 MB 值的比较并设置，会拖慢同实例上的其他服务。`CommitAcross` 与相邻分区匹配器抢同一批票时冲突约 49%（夹具有意的高竞争形态：10% 的局跨分区、只取相邻分区最早的票），跨分区成局要按冲突重试。建议：超过这个量就**按分区拆分**（把成局量摊到更多分区）或**缩短 `match.retention`**（代价是重放与读结果的窗口变短，至少覆盖匹配器与客户端的重试）。这是本机 loopback、同机压测端的单样本，不是目标负载，跨机 Redis、Redis Cluster 下的容量没有测（Cluster 主节点切换只做了正确性演练：约 1～2 秒连接错误、多键脚本无部分生效）。

Player 静态绑定 sid，不自动做动态玩家租约迁移。directory 名字域是全局域；activity groups_file 明确组与 game 列表；每个开窗条目都带持久计划，没有计划的条目与超过 `MaxPendingActivities` 的窗口按畸形数据拒绝（报告并保留名额 / sweep 返回 `ErrConflict`），由运维 `RemoveMalformedWindowEntry` 或线下修复，不自动回收、不轮转扫描。activity 的观察重试不自动耗尽失联投递，无法恢复的目标按人工 ExhaustDispatch 等管理流程收口。

**名字规则属于业务**（维护者 2026-10-17 决定）：角色名、公会名等的长度上限、敏感词、全服唯一都是业务规则，框架层不处理，生成的工程与 game-demo 也不内置（game-demo 的 `guild.ValidateName` 只拒绝空、全空白、控制字符与非法 UTF-8）。需要全服唯一时用 `service/directory` 的两阶段占名，不要用“先查有没有、再写”的两步做法（并发下两个请求都看到“没有”）：

- 装配：`wiring/directory.NewMod(normalize, reporter)`，依赖 redis Mod；`directory.key_prefix` 与 `directory.reservation_ttl` 必填、没有缺省。`normalize` 决定哪些写法算同一个名字（`directory.NormalizeLower` 去首尾空白并折叠大小写），是产品决定、不是配置项。业务从 registry 用 `app.Lookup[directory.Directory](r, mods.ModDirectory)` 取能力。
- 顺序：先 `Reserve(ctx, name, owner, ttl)` 占名（别人持有时 `ErrKeyTaken`，可回给客户端“名字已被占用”）→ 再做创建（保留 ID、建实体、事务）→ 成功后 `Commit(claim)` 转为永久；创建失败 `Cancel(claim)` 释放（重复 Cancel 成功，回滚可以重试）。改名、解散走 `Release(name, owner)`，只有持有者能释放。
- 预约带过期：`ttl`（缺省 `reservation_ttl`）内没有 Commit 的预约自动失效，调用方在 Reserve 与 Commit 之间崩溃时名字到期释放，不会被永久占死。`ttl` 要长于从 Reserve 到 Commit 的最长路径；创建已生效而 Commit 没做成（结果未知、进程崩溃）时，预约过期后名字可能被别人拿走，业务按自己的对账决定怎样处理。
- 同一 owner 重复 Reserve 拿到同一个 claim（重试不多占），所以 directory 不是互斥锁：“只允许一次尝试”要用 insert-only 的版本化状态（见 [包注释](../../../service/directory/directory.go)）。

## 5. 接入检查

分别验证 owner、本地调用、跨进程调用、重复请求、回复丢失、恢复与停止。业务期限使用业务时钟，系统令牌/裁剪/网络超时使用系统时钟。亚秒期限不能通过整数秒截断后改变行为。默认服务指标可统一关闭，但不能把关闭指标当成关闭周期清扫。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
