# 次核心：Service 领域能力：设计与使用

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/09-services.md) · [模块总目录](../README.md)

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

## 2. owner 与调用方

owner Mod 持有存储并发布公开接口与 .local；Server 注册总线 handler 并运行周期工作。调用方使用 ClientMod/BusClient，只依赖公开契约。同进程也通过同一能力接口；业务不要依赖把公开接口断言成具体 Service。

RPC 生成物分传输半与装配半：接口包持有 wire/handler/BusClient，wiring 持有接线 Server/ClientMod/OwnerCapabilities。运维 Admin、Sweep、ForceRelease 等本地能力不应自动暴露成任意总线调用。affinity 需要 discovery 支撑，缺失时不能装成“看似有效”的路由。

## 3. 存储与幂等

这些服务多数以 Redis/versionstore 存储自己的记录，不是玩家 DAO WAL 的旁路。key_prefix 必填且按环境隔离；rank 的 Lua 与 mail 信封等有独立原语。写操作用请求 ID/令牌/状态条件保证业务重试，传输超时和业务拒绝通过不同错误表达。

versionstore 写结果未知仍需调用方保留不确定状态，不能假设每种服务都能自动恢复所有未知结果。重试时必须保持同一个幂等身份和输入摘要；禁止以新 ID 反复发起可能已经生效的扣发。

## 4. 关键业务边界

account 的身份验证、玩家号分配、名字校验由接入方提供，不能用默认信任身份或进程内计数器替代。一个账号一区服一角色由 slot insert-only/建角计划约束；每个 slot 与角色都带建角身份（计划 ID / `CreationID`），缺计划的 slot、缺 `CreationID` 的角色不被推断，分别以 `ErrConflict` / `ErrNotResolvable` 拒绝并交线下处理；账号 ID 只用转义渠道后的形态（`Identity.AccountID`）。SelectRole 的 accountID 属可信服务边界，外部请求必须先绑定已认证身份。

**match 的已结束记录只保留固定时长**（维护者 2026-10-19 方案 A）：一个队列的全部状态（等待票、已结束的票、比赛、排队请求记录）是一个版本化值，Redis 存储每次 Enqueue / Commit / Sweep 都整体读写它。已成局 / 取消 / 过期的票、已提交的比赛与排队请求记录从结束起至少保留 `match.retention`（缺省 10 分钟，是票期限缺省值的两倍，覆盖匹配器与客户端的重试和玩家轮询结果），之后在下一次写入或 Sweep 时移除；比赛与它的票同时移除，等待中的票不论等了多久都不动。保留期内的读票、读比赛、同一 requestID 的 Enqueue 重放、同一票集合的 Commit 重放都返回原结果；超期后读到不存在，Commit 重放报 `ErrTicketMissing`（被移除的票 ID 不会回到等待状态，不会成第二局），同一 requestID 的 Enqueue 是一次新的排队。状态上限因此是“等待票（≤ `MaxQueueLength`）+ 保留期内的成局量”；单键整体读写与单队列争用仍在，调用按队列 Key 路由（[store.go](../../../service/match/store.go)、[queue_store.go](../../../service/match/queue_store.go) 的 `pruneResolvedLocked`）。开局后的长期身份（Match ID → 房间宿主 → 结算实体）由业务保存，匹配记录不是对局档案。

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
