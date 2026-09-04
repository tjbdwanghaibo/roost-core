# roost-service：从业务仓抽取通用服务的方案

目标：把业务仓 `cube` 里与玩法无关的公共服务（account、rank、chat、match、global、
mail 等）抽到一个新仓 `roost-service`，作为 roost 框架的**通用服务层**。

三条准则贯穿全文：

1. 用 roost 最新的接口与基建，不搬旧接法；
2. 不无脑拆——先修既有的设计问题与实现 bug，再抽；
3. 尽量抽象通用能力，特别专有的留接口。

## 一、现状盘点（已核实）

`cube/service/` 共 14 个目录。按"能否成为通用服务"分类：

| 目录 | 代码/测试行数 | 判断 | 依据 |
| --- | --- | --- | --- |
| `global` | 5939 / 1679 | **强候选** | 跨服路由绑定 + game 租约 + 跨服活动协调；**零** cube 玩法依赖 |
| `match_group` | 2096 / 592 | **强候选** | 匹配与分组编排 |
| `account` | 1816 / 1099 | **强候选** | 账号↔角色映射、角色会话令牌 |
| `mail` | 1798 / 659 | **候选** | 正文/附件/领取状态；领取是两阶段 |
| `rank` | 1302 / 665 | **强候选** | 榜单提交与查询 |
| `admin_gateway` | 1238 / 428 | 候选 | GM 命令路由 + 审计 |
| `chat` | 783 / 664 | **强候选** | 频道消息与历史 |
| `instance` | 609 / 373 | **候选** | 副本会话分配 |
| `alliance` | 468 / 79 | **不整体拆** | 见第四节 |
| `platform` | 209 / 174 | 候选 | 渠道鉴权与充值回调 |
| `rpcclient` | 208 / 249 | **应进 roost-kit** | 通用 bus RPC 客户端，非服务 |
| `web` | 127 / 53 | 不拆 | 业务 HTTP 壳 |
| `rpcresp` | 28 / 31 | **应进 roost-kit** | errcode 约定 |
| `robot` | 11208 / 3874 | 不拆 | 已有 `roost-core/robot` 承载通用部分 |

### 已有的设计意图文档就是判据

`cube/docs/business-boundary.md` 的 "Public Service 边界"（第 207–223 行）已经写下了
这些服务应有的不变量。这份文档给了准则 2 一个现成的**判据**：拿文档写下的不变量
去对照实现，凡是不一致的，在抽取时修正而不是搬运。已核实的不变量包括：

- "正式实现使用 Redis/Mongo/NATS，通过 app.Registry 注入；缺少必需 capability 时服务应该启动失败"；
- "单测可以在 `_test.go` 内定义轻量 fake，但**生产包不保留内存版 source of truth**"；
- "route binding 迁移必须走 **epoch CAS**，不使用进程内锁作为跨实例一致性保证"；
- "aggregation 和 participant progress 关键状态**必须走 CAS store**，participant progress
  必须先通过 request ledger **CAS reserve** 再更新"；
- "Instance 会话必须保留幂等信息，Enter 使用 `RequestID` 做请求幂等"；
- "运行时不再维护额外无界 map cache"；
- "业务失败必须使用稳定 errcode，生产代码不直接返回泛用 `CodeError`"；
- "`instance.client_mode=local` 仅用于测试或单进程调试，**不提供 RPC 失败后的本地 fallback**"。

## 二、准则 1：这些服务当前绕过了大部分 roost 基建

在 `cube/service` 与 `cube/server` 范围内统计"使用该基建的非测试文件数"：

| roost 基建 | 使用文件数 | 抽取时的处理 |
| --- | --- | --- |
| `dataengine` | 0 | **不适用**。它在 `Provide` 里硬要求 service-scoped entity access（`dataengine/mod.go:192`），无实体的服务用不上。不要为了"用上最新基建"硬套 |
| `saga` | 0 | **应当采用**。mail 的 `ReserveClaim`/`CommitClaim`/`CancelClaim` 与 match 的成组提交都是手写两阶段；saga 不需要实体，`NewMod(definitions...)` 即可 |
| `redis.IVersionedLock` / `IFencedVersionedLock` | 0 | **应当采用**。租约与归属判定当前无 fence |
| `featureflag` / `failurelog` | 0 | 建议采用，运维可观测性 |
| `bus.CallReliable` | 1 | 至少一次语义的调用应显式选用 |
| `cache.ReadThroughStore` | 间接 | 热读统一走它（已带 `RemoteTimeout` 上界） |
| `manager` mod | — | 新增能力：服务内的内存单例统一由它管生命周期 |
| `internal/registry` + `//roost:register` | — | 静态注册统一入口 |

`core/index` 是**进程内**泛型索引（`Index[K,V]`），不是 Mongo 二级索引，不要误当查询能力。

## 三、准则 2：抽取前必须修掉的问题（已核实，非推测）

### 3.1 系统性缺陷：CAS 是可选的，因此形同没有

`cube/service/global` 的五个 store 都定义成同一形状的接口，同时暴露

- 无条件写 `SetXxx(ctx, value) error`
- 读-改-写 `UpdateXxx(ctx, key, mutate) (…)`

Redis 实现的 `Update` 走 `fredis.CompareAndSet`（例如
`activity_aggregation_store.go:211`），**而 DAO 实现的 `Update` 是读后无条件写**
（`activity_aggregation_store.go:315-327`，末行 `return next, true, s.SetActivityAggregation(ctx, next)`）。
两者满足同一接口，类型系统无法区分。核实结果：

| store | CAS 路径 | 读后无条件写的 Update |
| --- | --- | --- |
| `activity_aggregation_store` | 1 | 1 |
| `activity_participant_store` | 1 | 1（`:244`） |
| `activity_progress_request_store` | 1 | 1 |
| `route_store` | 1 | 1 |
| `game_lease_store` | **0** | — |

后果：配置成 DAO store 时，并发的聚合推进/参与者进度更新会互相覆盖，而文档明写
"必须走 CAS store"。**根因是接口设计**，不是某一处实现疏漏。

**修法**：契约里只保留版本化的变更路径，删掉无条件 `SetXxx`，让非 CAS 实现**无法编译**。
这需要先在 roost-kit 补一个通用原语（见第五节），core 现有的 `redis.CompareAndSet`
只是单次比较，每个服务各自手写重试循环正是问题的来源。

### 3.2 `game_lease` 的版本字段是装饰性的

`global/game_lease_service.go:14-65` 的 `ReportGameHeartbeat`：

```go
if existing, ok, err := store.GetGameLease(ctx, gameSID); err != nil { … } else if ok {
    lease.Version = nextActivityVersion(existing.Version, now)   // 算了下一个版本
}
if err := store.SetGameLease(ctx, lease); err != nil { … }        // 但无条件写
```

读-改-写且无 CAS，`game_lease_store` 也没有任何 CAS 路径。后果：并发心跳互相覆盖；
更糟的是**上一个 game incarnation 的迟到心跳能覆盖当前活跃租约**（把自己的
`StartedAt`/`Load` 写回去），因为没有任何检查确认写入方仍是持有者。

**修法**：租约改用 `redis.IFencedVersionedLock`——它的语义正是"带 fence 的归属"，
而不是拿一个业务字段模拟版本。

### 3.3 生产包里的内存 source of truth

文档明令 "生产包不保留内存版 source of truth"，但 `service/account/memory_store.go:18`
的 `NewMemoryStore()` 在生产包里。`global`/`match_group` 的 `NewLocalXxx` 需要逐个甄别：
作为 `NewCachedWaitingQueueStoreWithTTL(local, remote, ttl)` 的 L1 缓存是合理的，
作为唯一真相源不是。

**修法**：抽取后统一为"接口 + 后端实现 + 可选 read-through 缓存"，测试替身放
`_test.go` 或独立的 `xxxtest` 包。

### 3.4 存储契约覆盖不均

| 服务 | 内存 | Redis | Mongo |
| --- | --- | --- | --- |
| `account` | ✓ | ✓ | ✓ |
| `global` | ✓（每个 store） | ✓ | — |
| `match_group` | ✓ | ✓ + cached 包装 | — |
| `rank` | — | **仅 Redis** | — |
| `mail` | — | — | 仅 Mongo |
| `chat` | — | — | 无导出 Store |

`rank` 只有 Redis 实现，意味着无法脱离 Redis 做单测或本地跑；`chat` 没有导出的存储
契约。抽取时应统一成同一形状的存储契约，这本身就是"抽象通用功能"的一部分。

### 3.5 同一类缺陷在 account 再现，确认是跨服务的系统性问题

`account` 的 `Store` 之外另有 `RoleCompareAndSwapStore` 可选接口。`MemoryStore`
**没有**实现它，于是 `service.go:405-419` 走 `!casOK` 分支：`GetRole` → 计算 →
`UpsertRole`，中间不持锁。而 `MemoryStore` 是**默认** store。后果是并发
`SelectRole`/`UpsertRoleInfo` 丢更新，且这个降级在编译期不可见——`MongoStore` 有
`var _ RoleCompareAndSwapStore = (*MongoStore)(nil)` 断言，`MemoryStore` 没有。

与 3.1 合并看：**"CAS 是可选能力"这个设计选择本身就是缺陷来源**。两个互不相关的
服务、两组不同的作者，都因为契约允许非 CAS 实现而产出了丢更新的代码路径。

**roost-service 的第一条设计约束**：状态变更的契约里只有版本化路径，没有无条件写；
非 CAS 实现不可能存在，因为它无法满足接口。

### 3.6 account / platform 的重点问题（共 23 项，此处列必须在抽取前解决的）

| 级别 | 问题 | 位置 | 后果 |
| --- | --- | --- | --- |
| 严重 | `LoginAccount` **不做任何身份验证**：拿 `{channel, open_id}` 就返回账号，随后 `SelectRole` 为该账号下任意 `player_id` 签发会话令牌。唯一的门是一个仓库级共享 HMAC，且非生产环境未配置密钥时 `accountClientAuthRequired` 返回 false。签名只覆盖 body，无 timestamp/nonce，抓包可永久重放 | `account/service.go:111`、`server/account_service.go:244,311` | 完整账号接管 |
| 严重 | 默认 `PlayerIDAllocator` 是**每进程从 1 开始的计数器**，而 Mongo-无-Redis 的配置组合会选到它；`CreateRole` 用 `ReplaceOne` 落库，**命中既有 role 直接覆盖** | `account/service.go:441`、`mongo_store.go:238`、`server/account_service.go:75-92` | 重启后建角色静默摧毁既有玩家数据 |
| 高 | `UpsertRoleInfo` 绕过 `CreateRole` 的全部不变量：不 `ReserveName`、不校验账号存在、不校验服务器可用、不查角色数上限；`mergeRole` 允许改写 `AccountID`（角色可被"过户"）与 `Name`（旧名预留成孤儿，新名未预留可重复） | `account/service.go:353-378,446-481` | 重名、孤儿预留、角色窃取 |
| 高 | 每服角色数上限是**非原子的检查后写**，且 `idx_role_account_server` **故意不是唯一索引** | `account/service.go:208-248`、`mongo_store.go:225` | 一服多角色，破坏所有 `roles[0]` 假设 |
| 高 | Redis 的角色 CAS **比较整个 JSON blob**。正确性依赖 `marshal(unmarshal(x)) == x` 逐字节相等；给 `RoleInfo` 加一个非 omitempty 字段，滚动发布期间新版本进程读到旧值就再也 CAS 不上 | `account/redis_store.go:66-83` | 滚动发布期间全体玩家的 `SelectRole` 失败 |
| 中 | `CreateRole` 跨两个 collection 写且**无事务**，补偿动作的错误被丢弃（`_ = s.store.ReleaseName(...)`）；`NewMongoStore` 只接受 `IDatabase`，结构上无法开 session | `account/service.go:226-251`、`mongo_store.go:29` | 名字被永久烧掉，无代码路径可释放 |
| 中 | `CodeError` 因 `iota` 位置实际等于 **2**，而所有解码失败分支返回 `errcode.CodeInternal` = **1**，无常量对应。本该拦住它的架构守卫 `assertDirectoryNoSourcePattern` 恰好**漏列了 account 与 platform** | `account/types.go:24-37`、`tests/architecture/boundary_test.go:273` | 客户端按 `CodeError` 匹配永不命中 |
| 中 | `platform` **完全没有可观测性**：`DeliverRecharge` 失败时错误值被整个丢弃换成常量 reason，两个包内没有任何 `slog`/metrics/failurelog。`RechargeCallbackRequest.RequestID` 被接收、转发、然后从未使用——不去重、不关联、连日志都没打 | `platform/service.go:152-155` | 支付发货失败在服务端不留痕迹 |
| 中 | 充值发货是至少一次（JetStream `CallReliable`），但幂等只存在于三跳之外的 game 侧 `OrderID` 去重；`platform` 自己无状态、无幂等 | `rpcclient/client.go:136` | ack 丢失的重投或重放的 HTTP 回调可重复发货 |

其余（Redis `ReleaseName` 的 TOCTOU 删错人预留、role+index 非原子导致角色存在但不在
索引里、TTL 只覆盖记录不覆盖索引与名字键、Mongo `version,omitempty` 使 version=0
的角色永远 CAS 不上、三个索引无任何查询会用到、`ensureIndexes` 用
`context.Background()` 无超时阻塞启动、`ListServers` 全表扫且每次 `CreateRole` 都调、
`getJSON` 用错误文本子串判断 key 不存在）在抽取时一并处理。

### 3.7 测试里被"钉死"的错误行为

抽取时要特别注意：有些测试把 bug **当作期望行为固定下来了**，直接搬运会连错误一起继承。

- `TestUpsertRoleInfoRetriesRedisCASConflictAndMergesLatestRole` 断言
  `resp.Role.Power != 120`——它固化的正是**丢更新**：并发写入的 `Power: 200` 被较旧的
  请求覆盖。名字叫 "MergesLatestRole"，行为恰好相反。
- `TestRedisStoreCompareAndSwapRoleUsesVersionedValueCAS` 名字声称按版本 CAS，
  而生产代码比较的是整个 JSON blob；测试实际跑的是测试文件里手写的 Lua 复现品。
- account 的 Mongo 替身用 `encoding/json` 而非 bson 编码文档、用 `fmt.Sprint` 相等
  做匹配，因此类型盲；且用 `PlayerID: 1001` 这种小值——真实的
  `entity.BuildEntityID` 值约 2^52，经 JSON `float64` 会变成 `4.5e+15`，`_id` 过滤器
  就匹配不上了。**替身在真实输入下会失效**，所以它通过并不说明什么。
- 两个包里没有任何 `go func` 或面向 `-race` 的测试，全部并发类问题零覆盖。

### 3.8 rank / chat / match / instance 的重点问题

深读四个服务共得 62 项发现。此处只列改变抽取方案的：

**rank**

| 级别 | 问题 | 位置 |
| --- | --- | --- |
| 严重 | `GetTop` 的 `Limit == 0` 被翻译成 `ZREVRANGE key 0 -1`——**读整个榜**，然后每个成员一次 `HGET`、全部 JSON 上总线。`normalizeRange` 只夹大值不夹 0，而玩家协议路径上 `Limit` 从未被填默认值。一个登录玩家一个包即可触发；board/scope 也完全由客户端指定且无鉴权 | `redis_store.go:144`、`record.go:62` |
| 严重 | **battle-settled 的积分累加不幂等**：用 `UpdateAdd` 且 `Version: 0`，于是版本守卫被整个跳过；durable consumer 的 `MaxDeliver 20` 重投会把同一场战斗的 `RankDelta` 加 2–20 次，**榜单永久错误且无修复路径**。同文件的 arena consumer 用的是 `UpdateSet` + `Version`，是对的——说明这个不对称是无意的 | `domain_event_consumer.go:52`、`redis_store.go:59,65` |
| 严重 | `ArchiveSeason` 在**第一个短页**就静默截断。`GetTop` 遇到 ZSET 有成员而 hash 无字段时会跳过并仍占一个 rank 名额，于是返回短页；而 `RemoveScore` 在 hash 字段已消失时提前返回，把 ZSET 成员永久留下。归档是 `ResetBoard` 之后**唯一**的记录，元数据还照写完整的 `EntryCount`——**静默永久数据丢失** | `redis_store.go:254,116,168` |
| 高 | 成功的提交可能被报成失败：CAS 写入后另起一次 `ZRevRank`，并发 `RemoveScore` 会让它返回 `ErrNil`。调用方按失败入 outbox 重试，于是**已被移除的玩家重新出现在榜上** | `redis_store.go:78,493` |
| 高 | 赛季结算**先发奖励邮件再归档**，而邮件幂等键里含 `entry.Rank`。归档失败重跑时分数已变，同一玩家换个 rank 就换个 `RequestID`，邮件去重失效 → **重复发奖**。rank 没有"冻结榜单"原语可用 | `game/controllers/arena/controller.go:228,369` |
| 高 | **rank 全包零 errcode**：连"board id 为空"这种纯客户端错误都返回 `CodeInternal` / `"server error"`。chat 明显认真做了这件事（16 个 errcode 并有测试钉住），rank 完全没做 | `types.go:32` |
| 中 | `Tie` 字段从未被任何生产者写入，于是并列时第三排序键生效：**player ID 小的永远排前面**，所有榜单、永久 | `record.go:44` |
| 中 | `GetTop`/`GetAround` 的排名与分数来自 2–4 次互不同步的读，分页在并发提交下漂移；`ArchiveSeason` 边写边遍历活榜——"赛季快照"不是快照 | `redis_store.go:149,206` |

**chat**

| 级别 | 问题 | 位置 |
| --- | --- | --- |
| 严重 | 客户端传来的 `Trusted` 布尔是系统消息的**全部授权**：置 true 即可关掉 caller==sender 校验、解锁 `SystemTemplate`、绕过频道策略与历史鉴权。而**生产代码从未设置过它**——纯攻击面，零合法用户 | `service.go:234,346`、`policy.go:44` |
| 高 | 私聊历史**查不到自己发出的消息**，且无法按对话方过滤（`TargetID` 被强制等于 caller）。基于这个 API **做不出可用的 1:1 聊天** | `chat.go:103`、`service.go:367` |
| 高 | 无保留策略、无游标：没有 TTL 索引（且时间戳存成 `int64` 毫秒而非 BSON date，加不了）、`History` 只有 `Limit` 且夹到 200。重连会重复拉同一批并**永久丢失**离线期间超出 200 的部分；集合无界增长 | `chat.go:130,99` |
| 高 | 全局序列号是**单文档热点**且与插入不绑定：每条消息两次 Mongo 往返、全部写序列化在一个文档上；`Publish` **没有任何幂等键**，而部署用的是 JetStream `max_deliver: 5` → 重投即重复消息 | `chat.go:157,88` |
| 中 | 排序用**进程墙钟**而不是它自己已经分配的单调 ID。多副本时钟偏移会打乱历史顺序 | `chat.go:108` |
| 中 | `MongoHub.Publish` 无条件用 `trusted = true` 做校验——`Hub` 是可复用的存储接缝，任何未来的直接使用者都静默失去这道检查 | `chat.go:75` |

**match_group**（**全部通用**，cube 残留只有 key 前缀与 errcode 段）

| 级别 | 问题 | 位置 |
| --- | --- | --- |
| 严重 | 一个玩家可被同时提交进两场匹配：`TicketID` **由客户端提供**且被直接当身份用，去重只按 ticket ID，没有任何 per-subject 唯一性检查。同一玩家可在队列里出现两次，被塞进两个 `Match`，甚至同一个 match 里出现两次 | `service.go:544,125,487` |
| 严重 | 成组提交是**非原子多写，且队列已被截断**：先弹出队首并落库剩余 → 重读 → 建组 → 写 match → 逐个改 ticket 状态。中途任何失败/崩溃都让这批玩家**从队列消失但状态仍是 waiting**，无 reaper、`ExpiresAt` 从未被读，客户端重试会命中幂等分支拿到一个空 match——**永远等下去** | `service.go:409-459` |
| 严重 | 版本号是**每进程从 1 开始的计数器**（`s.nextSeq()`）而不是时钟。跨副本、以及同副本重启后的版本号互不可比；而 `fcache.VersionStale` 在判定为陈旧时**静默丢写并返回 nil**，于是 cancel 回 `CodeOK` 且 `Status: canceled`，Redis 里却仍是 `matched` | `service.go:122,638`、`redis_models.go:350` |
| 严重 | ID 是 `ticket-1`/`match-1` 这样的进程内序号，跨副本必然碰撞。后果之一：`EnqueueTicket` 的幂等分支会**返回别人的 ticket** 并回 `CodeOK` | `service.go:634,133` |
| 严重 | 队列的跨副本读-改-写无 CAS，唯一串行化是进程内 `sync.Mutex`；而 `BusClient.EnqueueTicket` **按设计 round-robin 到不同副本**，队列 key 也无副本/路由维度 → 两个副本读到同一队列、各建一个 match | `service.go:531`、`client.go:45` |
| 高 | ticket/assignment 的取消、查询、关闭**无任何调用方身份**，配合可猜的 `ticket-<n>` ID，任何客户端可取消他人 ticket、读他人 Subject（分数/战力）、关闭进行中的 assignment | `types.go:134,148,179` |
| 高 | 远端 I/O 全程持进程级 mutex，且每次入队对队列里每个 ticket 各读一次（最多 8 次重试 × N）→ 入队 O(N)、服务整体 O(N²)，队列长度无上限 | `service.go:130,463` |
| 中 | 重试循环只对 `ErrWaitingQueueConflict` 重试，而**没有任何真实 store 会返回它**——生产中循环恒执行一次，代码读起来像有乐观并发控制，实际没有 | `service.go:386` |
| — | **超时完全没有实现**：`ExpiresAt` 写了、传了，全仓无一处读它，包内没有任何 ticker 或 goroutine | 全包 |
| — | 队列这一半在生产中**没有任何玩法调用方**——唯一调用者是同样没被调用的 pass-through controller | — |

**instance**：重度耦合，本质是 cube 适配器。它的"端口"类型 `Runtime`/`EnterParam`
直接就是 `manager/instance_mgr` 的结构体，因此接口**并没有反转依赖**；`replica.go`
还直接伸进活的场景图（`gameaccess.Scene`、`scene.SceneAgent().ServerID()`）。
另有：`RequestID` 为空时幂等键返回 `""` 使幂等**整体失效**且无任何容量核算 → 反复
`Enter` 可无界分配副本场景，且旧 run 因 presence 被覆盖而**永远无法 Leave、场景永不销毁**；
`AttachReplica` 失败路径**没有补偿**（`CreateReplica` 路径有），副本与场景一起泄漏；
`DeadlineAt` 算了、发给客户端了，**全仓无一处与时钟比较**。

### 3.9 六个系统性模式（这是方案的真正结论）

62 项发现不是 62 个独立问题。它们收敛到六个模式，每一个都在**多个互不相关的服务里
被不同作者各自重新犯了一次**：

| 模式 | 出现处 |
| --- | --- |
| **CAS/版本是可选的或假的** | account：比较整个 JSON blob；`MemoryStore` 不实现 CAS 接口而它是默认 store。global：DAO 变体的 `Update` 读后无条件写（4 个 store）；`game_lease` 版本字段纯装饰。match：版本是进程内计数器。rank：`Version: 0` 时守卫被整个跳过 |
| **静默丢弃却报告成功** | global DAO 覆盖写。match：`VersionStale` 丢写返回 nil，cancel 回 OK 而库里仍是 matched；重读不足 groupSize 时返回 `Match{}, nil` 而队列已截断。rank：`ArchiveSeason` 首个短页即截断，元数据照写完整计数 |
| **至少一次路径上没有幂等** | rank battle-settled（`UpdateAdd` + `Version 0`，重投累加 2–20 次）。chat `Publish` 无幂等键 + JetStream `max_deliver 5`。platform 充值发货自身无状态。match 入队幂等按客户端 ID |
| **身份/权限来自客户端** | chat 的 `Trusted` 布尔。match 的 `TicketID` 与全部 ticket 操作无 caller。account 仅凭 `{channel, open_id}` 登录。rank 的 board/scope 无鉴权 |
| **一个玩家包能触发无界读** | rank `GetTop(Limit: 0)` 读整榜。match 入队 O(N) 持全局锁。account `ListServers` 每次建角色都全表扫 |
| **零可观测性** | match、instance、platform 三个包**没有一行** slog/metric；rank 无 errcode 因此所有失败都是 `CodeInternal`。所有静默丢弃路径在生产中不可发现 |

还有一个横切的测试问题：**多处测试把 bug 当期望行为钉死了**（account 的
"MergesLatestRole" 断言的正是丢更新；chat 有两条测试对 `Trusted` 断言了相反的模型，
而生产走的是有漏洞的那条；match 的重试测试跑的是生产中不可达的路径；rank 的
"UsesAtomicScript" 只断言"调过 Eval"）。更严重的是 **rank 的生产读路径从未被执行过**
——测试替身的 `Pipeline()` 返回 nil，所有测试都走 fallback 分支，而 S4 的成因就在
真实的 pipelined 路径里。

**因此这不是一次抽取，而是一次带参考实现的重新设计。** 无脑搬运会把这六个模式
复制成八份共享库代码——而共享库里的缺陷比业务仓里的贵得多。

## 四、准则 3：抽什么、留什么接口

判据不是"名字听起来通用"，而是**签名里有没有玩法概念**。

| 服务 | 判定 | 依据 |
| --- | --- | --- |
| `global` | **重新实现**（骨架可参考） | 零玩法依赖，跨服协调价值最高；但 5 个 store 的 CAS 全靠约定 |
| `match` | **重新设计** | 逻辑全通用，但队列这一半在生产中无调用方且带 10 个严重缺陷。**没有迁移压力，正是从头设计对的机会** |
| `rank` | **重新实现** | 95% 通用（唯一耦合是 `domain_event_consumer.go`），但幂等、归档、错误码要重做 |
| `chat` | **重新设计** | 机制通用、载荷 schema 耦合；且当前无实时投递、无保留、私聊 API 不可用 |
| `account` | **重新实现** | 目录与令牌通用，但身份验证必须从"无"变成必填接口 |
| `mail` | 抽取 + 改造 | 两阶段领取改用 saga |
| `platform` | 抽取 + 补齐 | 骨架通用，补可观测性与自身幂等 |
| `instance` | **不抽** | 端口类型即 manager 结构体，未反转依赖；直接操作活场景图。只抽其中的会话原语 |
| `alliance` | **不整体抽** | `Dispatch` 30 多个字段是 SLG 玩法（`DonateTech`/`MapTerritory`/`DeclareWar`/`StartExpedition`）。只抽 `Directory` 的 reserve/commit/cancel 原语 |
| `rpcclient` + `rpcresp` | **进 roost-kit** | 通用 bus RPC 客户端与 errcode 约定，是基建不是服务 |
| `robot`、`web` | 不抽 | 通用部分已在 `roost-core/robot`；web 是业务 HTTP 壳 |

### 必须留成接口的（特别专有的）

| 接口 | 为什么不能有默认实现 |
| --- | --- |
| `IdentityVerifier` | 渠道令牌校验各家不同。**不能有默认实现**——当前"不校验"就是把缺失当默认，直接导致账号接管 |
| `PlayerIDAllocator` | 接缝已存在且正确，但当前默认值是每进程计数器。**删掉默认值**，未注入即启动失败 |
| `NameValidator` | 长度、字符集、敏感词按项目而定 |
| 角色档案字段 | `Level`/`Power`/`AllianceID`/`Avatar` 是玩法进度。改 `Profile json.RawMessage` 或类型参数 |
| `EventMapper`（榜单分数来源） | rank 现在直接 import cube 的 domain_event 并解 `payload.Result.Settlement`。改为 `func(Event) (BoardKey, Score, UpdateMode, bool)` 由业务注册 |
| `ChannelPolicy` | 接缝已存在；耦合在那个读 `cube:alliance:member` 投影的实现里，它应该在业务侧 |
| `BodyValidator`（消息体） | `PositionShare`/`BattleReportShare`/`AllianceInvite` 是玩法实体。改 `Body json.RawMessage` + 按 `MessageType` 注册校验器 |
| 匹配评分与队伍规则 | 通用的是队列、状态机、超时、成组提交的原子性；MMR 与平衡规则留接口 |
| `RechargeDeliverer` | 已是接口，只需把 `MethodDeliverRecharge` 常量移回业务侧 |

## 五、前置条件：先修 core、再补 kit

抽取不能直接开始。有四个缺口会让 roost-service 无法把事情做对，其中第一个在 core。

### 5.1 core：`cache` 的 `Stale` 语义会静默丢写（**已修，见 core CHANGELOG**）

`roost-core/cache/ref_hmap.go:137-145`（`redis_json.go` 同形）：

```go
if s.cfg.StoreConfig.Stale != nil {
    old, ok, err := s.Get(ctx, key)          // 读
    if err != nil { return err }
    if ok && s.cfg.StoreConfig.Stale(old, value) {
        return nil                           // ← 丢弃写入，却报告成功
    }
}
return s.writeHashes(ctx, plan, writes)      // 写
```

两个问题，都是框架级的：

1. **读与写是两次独立往返**，中间没有 CAS——所以这个"陈旧检查"本身就有竞态，
   等版本的并发写两边都会通过。
2. **判定陈旧后返回 `nil`**，调用方无法区分"已写入"与"被丢弃"。

第 3.9 节里 match 那条"cancel 回 `CodeOK` 且 `Status: canceled`，而 Redis 里仍是
`matched`"，成因就是这里。**它是"静默丢弃却报告成功"这个模式的框架级源头。**

已按此修复：新增 `cache.ErrStaleWrite`，三处写入点不再静默返回 `nil`；`StaleFunc`
的并发语义写进文档（`AtomicLocalStore` 原子，两个 Redis store 的谓词仅供参考，需要
"败者必须被拒绝"时用 `redis.CompareAndSet`）。顺带修了同包的另一条：`LayeredStore`
的 `ttl <= 0` 从"永不过期"改为"不从 L1 供读"——漏配一个配置键（`viper.GetDuration`
返回 0）此前会得到一个永不回源的 L1，这正是 match 那条 S6 的成因。

**因此第 5.2 节的 CAS 原语仍然必要**：修好的 `Stale` 只是让丢弃可见，它在 Redis
store 上依然不是并发控制。

### 5.2 kit：通用的版本化 CAS store（**已完成**：`roost-kit/versionstore`）

core 只有单次的 `redis.CompareAndSet`，每个服务各自手写"读-改-CAS-重试"循环——
这正是 3.1/3.5/3.9 的来源。在 roost-kit 补一个泛型原语：

```go
// 契约里没有无条件写：非 CAS 实现无法满足它。
type Versioned[T any] struct { Value T; Version uint64 }

type Store[K comparable, T any] interface {
    Get(ctx context.Context, key K) (Versioned[T], bool, error)
    // Update 内部完成 CAS 重试；mutate 可能被调用多次，必须是纯函数。
    // 冲突耗尽返回可区分的错误，不静默成功。
    Update(ctx context.Context, key K, mutate func(T, bool) (T, bool, error)) (T, bool, error)
    Delete(ctx context.Context, key K, expect Versioned[T]) error
}
```

三条硬约束，每一条对应一个已确认的缺陷：

- **比较版本号，不比较值**——account 比较整个 JSON blob，滚动发布期间序列化漂移
  会让全体玩家写不进去；
- **版本必须单调且跨副本可比**（用存储侧递增或时钟，不用进程内计数器）——match
  用 `s.nextSeq()` 从 1 开始，跨副本与重启后都不可比；
- **零版本不能等价于"跳过检查"**——rank 的 `Version: 0` 让守卫被整个跳过，
  于是重投累加。

另配一个内存实现供测试。

### 5.3 kit：把 `rpcclient` / `rpcresp` 提升进 roost-kit（**已完成**：`roost-kit/servicerpc`）

`cube/service/rpcclient`（bus RPC + etcd 发现 + round-robin + lightweight/jetstream
传输选择 + 响应状态约定）零业务耦合，是**基建**而不是服务。它属于 roost-kit，
让 roost-service 与业务仓共用一份。`rpcresp`（errcode 约定）随它一起。

顺带修两个已确认的问题：`BusClient` 的 round-robin 是 match 跨副本竞争的**设计成因**
（同一队列 key 的连续入队被有意分发到不同副本），因此客户端要支持**按 key 亲和**
的 picker；`rpcclient` 层还要能表达"这次调用需要幂等键"。

### 5.4 kit：导出可复用的 Mongo 测试替身（**已完成**：`roost-kit/mongo/mongotest`）

roost-kit 的 `internal/mongofake` 是**求值 filter** 的内存 Mongo（1628 行、13 条自测），
但在 `internal/` 下**跨模块不可 import**。而 cube 各服务的替身质量正是测试失效的原因：
account 的 Mongo 替身用 `encoding/json` 编码、`fmt.Sprint` 匹配，类型盲且在真实
`entity.BuildEntityID`（约 2^52）下会因 `float64` 精度失配；rank 的 Redis 替身
`Pipeline()` 返回 nil，导致**生产读路径从未被执行**，而那条路径里就藏着归档截断的成因。

把它导出为 `roost-kit/mongo/mongotest`，Redis 侧同样需要一个求值的替身（含 `Eval`
与 `Pipeline`）。这是 roost-service 测试可信度的前提：CAS 过滤器、唯一索引冲突、
事务回滚、pipeline 语义，只有求值型替身能测。

## 六、仓库形态

沿用 `roost-skill` 的先例：顶层一个包一个服务，外加 `docs` / `examples` / `integration`。

```
roost-service/
  account/   chat/   global/   mail/   match/   platform/   rank/
  directory/     ← 通用原语：唯一键预留 + 独占归属 + 两阶段提交
  session/       ← 从 instance 抽出的原语：幂等 Enter + 归属 + 终态 + 超时执行
  servicetest/   ← 跨服务共用的测试夹具
  docs/  examples/  integration/
```

每个服务包内保持 cube 已验证可行的形状（`types.go` / `client.go` / `service.go` /
`rpc.go` / `store.go` / `redis_store.go` / `admin.go`），但加三条包级约束：

1. **不放内存版 source of truth**（`business-boundary.md` 明令禁止，account 违反了）。
   测试替身放 `_test.go` 或 `servicetest`。
2. **每个服务必须定义自己的 errcode 段**，纯客户端错误不得返回 `CodeInternal`
   （rank 全包违反了这条，chat 做对了）。
3. **每个服务必须有 metrics**：队列深度、匹配率、CAS 冲突率、丢弃计数。
   第 3.9 节所有"静默"路径之所以能长期存在，就是因为 match/instance/platform
   三个包里没有一行 slog 或 metric。

## 七、分步落地

每一步独立可回退，顺序是"文档不变量 → 测试 → 实现"，不是先搬后修。

1. ~~修 core 的 `Stale` 语义~~（5.1，**已完成**）。
2. ~~补 kit 的三个前置~~（5.2–5.4，**已完成**）。实施中多出一项收获：
   `versionstore` 的并发测试暴露出"竞争下耗尽重试预算即报冲突"这个缺陷——正是本文
   3.8 节记在 rank 名下的那一条——于是给重试补了指数退避与全抖动。**测试先于实现
   写、并且真的并发，才会在共享原语里发现它。**
3. ~~`directory` 原语 + `rank`~~（**已完成**）。实施中两项发现：
   - `directory` 的"同 owner 幂等"意味着**它不是互斥锁**。一个 owner 自己 race 时
     所有人拿到同一个 claim 全部通过——这对"可以重复请求的名字"是对的语义，对
     "只准一次尝试"是错的。需要后者时用 `versionstore.Create`。已写进接口文档。
   - 用 Go 重新实现 Lua 语义的测试替身，**无法发现脚本文本自身的缺陷**——改脚本
     字符串对它没有影响。rank 的并发缺陷是真实 Redis 抓到、替身抓不到的，因此加了
     `//go:build integration` 的真实 Redis 测试，并把脚本写得尽量小。
4. ~~`match`~~（**已完成**，从头设计）。核心决定是**把整个队列状态放进一个
   `versionstore` 条目**：提交因此天然是一次 CAS，那三个"弹出队首后非原子多写"的
   缺陷在结构上消失。代价是单队列吞吐受限于一个 key 的竞争，这正是
   `servicerpc.KeyAffinityPicker` 存在的理由。成组提交没有用 saga——一次 CAS 已经
   给出原子性，引入 saga 只会增加活动部件。
5. ~~`account`~~（**已完成**，重写）。名字唯一性用 `directory` 的两阶段 claim，
   因此"崩溃烧掉名字"由 claim 过期解决，不需要跨 collection 事务。
   而"每服一角色"**不能**用 `directory`（见第 3 步的发现），改用
   `versionstore.Create` 的仅插入语义。`RolesPerServer > 1` 构造时直接拒绝——
   接受一个配置项却执行别的，就是让配置项变成谎言。
6. **`global`**（路由 + 租约**已完成**，跨服活动协调进行中）。一处方案修正：
   `game_lease` **没有**改用 `redis.IFencedVersionedLock`——那是"临界区加锁 + fence
   令牌"的形状，而租约是带续期的长期归属，不是围住一段临界区。fence 这个概念对、
   锁这个机制不对，所以改用版本化状态里的 **incarnation 令牌**：续期必须出示它。
   `business-boundary.md` 那几条 CAS 不变量现在由 `versionstore` 的契约保证——
   它没有无条件写，非 CAS 实现不可能存在。
7. **`chat`**（进行中，与第 6 步的活动协调并行）。补存储契约、游标分页、保留策略、
   发布幂等键、opaque body + 注册式校验器；删掉 `Trusted` 字段——信任只能来自
   传输层身份，表达为独立的特权入口而不是请求上的一个布尔。
8. **`mail` + `platform` + `session` 原语**。✅ 已完成（30 / 24 / 26 条单测，
   43 条变异验证）。三条计划修正：

   - mail 的两阶段领取**没有改用 saga**。saga 编排的是"多个步骤各自可补偿"，而这里
     真正的缺陷不是缺补偿，是**幂等键由客户端提供、且租约过期后可被另一个键抢占**。
     换成 saga 不会修掉它。改法是把 claim token 变成服务端生成、对同一封邮件恒定的
     值——于是重试拿回同一个键，任何按它去重的发货侧必然收敛，租约过期只多一次投递
     尝试而不是多一次发放。
   - mail 另外发现一条本文档未记录的严重缺陷：`listItems` 夹住的是**返回条数**而不是
     **读取次数**。它循环取 `limit*4` 条信封、每封各读一次状态，直到凑满一页，**循环
     次数无上界**。一个删除过很多邮件的玩家能把一个协议包变成不确定次数的往返。这是
     "一个玩家包触发无界读"模式的第四次独立重现，也是最隐蔽的一次——它看起来有上界。
   - session 从 instance 抽出时确认了本文档第 3.8 节记录的四条，并补上一条：
     `saveSession` 在 store 为 nil 时 `return nil`——为一次没发生的写报告成功。

   顺带修了 kit 的一处契约缺口：`versionstore.Store.Create` 在冲突时返回的是**零值**
   而不是它撞上的那个值，接口文档没写。误信返回值会让"每 owner 独占"的 claim 退化成
   完全不独占（零值的 run id 是 `""`，看起来像一个指向不存在 run 的孤儿 claim，于是每
   个竞争者都"解决"掉一个活着的 claim 并接手）。已在接口写明并用测试钉住两个实现。

9. **存储后端 + `integration/`**。✅ 已完成。计划外的收获是一条**判断修正**：
   第六节把 `redis_store.go` 列为每个包都要写的文件，实际上**七个包一行都不用写**
   ——它们的状态就是 `versionstore.Store[K,V]`，kit 的 `NewRedisStore` 就是该契约的
   生产实现。每个包只加一个薄构造器把它装起来，零存储逻辑。

   真正需要写后端的只有两处，都不是 versionstore 能表达的：`rank` 的 sorted set
   （早已完成），和 `mail` 的 `EnvelopeStore`——信封写一次不再改、读路径是批量。

   薄构造器唯一拥有的职责是 **key 命名空间**，这一条由 `integration/` 实测：驱动
   九个包，`SCAN` 回读活 keyspace，断言 19 个声明的命名空间各自恰好收到写。十条
   "故意撞前缀"的变异全部变红——其中一条最初是绿的，因为 account 的 `acct:` 与
   `role:` 撞在一起也不冲突（一个 key 是字符串账号 ID、另一个是数字 playerID，
   **只是恰好格式不同**）。这既是潜在隐患，也暴露命名空间测试当时只覆盖三个包。

   另修一处自己的缺陷：`EnvelopeStore` 的 key TTL 用 `time.Until` 读墙钟，与 Service
   的注入时钟不一致，导致 Service 用固定时钟（每个测试、以及任何回放/补投任务）时
   每封信封写下去就已过期。

10. **装配面：每个服务一个 `app.Mod`**。✅ 已完成。第九节说的"业务逻辑包 + 一个
    `app.Mod`（注册 capability）"是让这些包**在进程内可用**的最小充分条件；
    `client.go`/`rpc.go` 只有跨进程消费才需要，因此排在后面。

    实施中一条**设计判断**值得记下来：Mod 只能拥有**基建接线**（Redis 客户端来自
    registry、key 前缀与 TTL 来自配置）。凡是配置给不出的东西都是构造参数、必填、
    无默认——`account` 的 `IdentityVerifier`、`platform` 的
    `Verifier`/`PlayerResolver`/`Deliverer`、`session` 的 `Releaser`、`chat` 的
    `ChannelPolicy`/`SystemAuthenticator`、`directory` 的 `Normalizer`。理由不是风格：
    第 3.6 节记的 `platform` 鉴权缺失，**缺席本身看起来就像一个默认值**。给这些东西
    一个默认值，就是把漏洞装回去。

    key 前缀也必填无默认：默认值在每个部署里都是同一个字符串，共用一个 Redis 的两套
    部署会静默共享状态。这一条由实测保证——before/after 快照回读整个 keyspace，断言
    每个服务的新 key 落在配置 root 下**且落在自己的子命名空间里**。后半句是必要的：
    读错别人的 config key 仍然落在 root 下，那条变异一度是绿的。

    另有两条变异一度是绿的，都是测试自身的问题：一条扫 key 用了固定 pattern，会捡到
    上一次运行的遗留（改成快照差分）；一条是 platform 的会话令牌**自证**——把 payment
    secret 接到 session secret 的位置，签发与校验仍然自洽，只有对着配置里的值独立验签
    才抓得到。

11. **errcode 接线**。✅ 已完成，是 RPC 面的前置条件。

    发现：九个包的 error code **此前全部没有接上 sentinel**，任何 RPC 边界上每个
    错误都会变成 `CodeInternal`——正是第 3.8 节记在 rank 名下的那条缺陷。两处例外
    要说清：`chat` 与 `global` 各有一张手写的 `errors.Is` 映射表，第一次审计的 grep
    没匹配到。

    改法：把 sentinel 本身变成 `errcode.Define(...)`，于是 `errcode.ClientError` 穿透
    任意层 `fmt.Errorf` 找到 code，**所有调用点零改动**。那两张手写表删掉——逐
    sentinel 的表是第二份清单，新增的错误会从上面静默掉下去。仍需函数的唯一理由是
    **外部 sentinel**（`versionstore.ErrConflict` 不带本包 code，而 CAS 耗尽是调用方
    能据以重试的真实结果）。

    每个包的 `CodeStoreFailed` 都删了：未分类的错误诚实地报 `CodeInternal`，
    为任何未分类的 bug 回答"存储失败"是把猜测当成诊断。`directory` 补了
    530101-530106 段（它此前只有 sentinel、零 code）。`global` 段里留一个**有文档的
    洞 570111**——活动码不向下重编号，因为它们在已发布版本里可观测，改值会破坏按码
    匹配的客户端。

12. **跨进程面：接口 + 生成的传输层**。✅ 已完成（九个服务，`directory` 故意除外）。

    原计划写的是"每个包手写 `client.go` / `rpc.go`"。**这条被两次修正推翻了**，
    两次都是用户提出来的，都对：

    - 第一次：*"之前的 core 中 bus 没有 client 的概念吗？还需要再重新造一个 rpc 和
      client 吗？"* —— 不需要。`bus.IBus` 已经有
      `Call`/`CallTo`/`CallWithTimeout`/`CallAsync`/`HandleRpc`，`kit/servicerpc` 已经
      有带 `ResponseStatusProvider` 和 `KeyAffinityPicker` 的通用 `BusClient`。这一步
      要做的不是造传输层，是**在既有传输层上定契约**。
    - 第二次：*"手写的格式符合预期，但我希望有个 codegen"* —— mail 那一份手写的四件套
      成了参照实现，然后由 `roost-codegen` 的 **`servicerpc` 生成器**从**手写的接口
      本身**生成（不另立 def 文件）：线上类型、handler 注册、打字的 client、`Server`、
      `ClientMod`、capability 包装。手写一遍再生成，比直接写生成器好，因为参照实现
      是拿来对比的基准。

    从接口生成，而不是从一份单独的 IDL 生成，是这个生成器唯一值得存在的理由：它省的
    打字量不多，但让**接口与传输层漂移在结构上不可能**，而不是可被检测到。

    还有一条计划里没有的**结构后果**：`registry` 里放的不能是具体类型。`Value:
    mail.Mail(service)` 这种调用点转换**不管用**——Go 存进 `any` 的动态类型仍然是
    `*Service`，`app.Lookup[*mail.Service]` 照样成功。所以 capability 是一个**包装
    类型**，只满足接口。这一条由九个包一起断言（接口能查到、具体类型查不到），反向
    变异确认变红。

    接口刻意比进程内 API 小，每个省略都有理由；两条最有信息量的省略不是我定的，是
    生成器的规则**顶出来的**：

    - `platform.ValidateSession` 没有 `ctx`。生成器要求首参是 `context.Context`，于是
      它自动落在接口外——而这个"限制"恰好是对的：它不做任何 I/O，只用本进程已有的
      密钥重算一个 MAC，做成一次往返意味着全集群的会话校验排在一个进程后面。
    - `chat.ChannelRef` 的 key 字段是故意未导出的（ref 只能来自 `Resolve`），生成器
      按"未导出字段"点名拒绝并指出是 `ref.key`。它根本过不了总线：任何 codec 都会
      静默丢掉 key，对面拿到的 ref 指向空而不是报错。

    每个服务手写一个 `run(ctx)` 钩子，"没有周期性工作"也要显式写出来。这条的价值在
    写下来之后才显出来：`match`/`session` 的过期扫描、`platform` 的发货重试、`chat`
    的保留期裁剪、`global/activity` 的宽限窗推进——**四个服务此前都有"写了字段但全
    代码库没人读"的截止时间**，这正是第 3.9 节那条模式。而 `rank`/`account`/`global`
    答"没有"的理由三条各不相同，其中 `global` 的最反直觉：过期租约不是"被占着"，
    而且删版本化 key 会让版本从 1 重来，续约要过的 fence 就变成跟一个刚归零的版本
    比大小。

13. **`global` 拆成两个包**。✅ 已完成，破坏性。

    这一步计划里没有，是生成器顶出来的：生成文件在包级别声明十几个固定名字，一个包
    里两个被标记的接口就是每个都声明两次。而这条规则只是把一件本文第六节就写下的事
    说出来——`app.Service` **每进程一个**，所以 `global` 那两个 capability
    （`service.global` / `service.global.activity`）从来就是两个独立部署的东西。

    拆的时候确认了它们**不共享任何类型、任何 store**：活动侧一个 routing 符号都没用。
    也就是说这个包一直是两个包住在一起。

    顺带修掉两处历史包袱：`global` 错误码段在 570111 的"永久的洞"——那个洞完全是两个
    服务从一个号段发号造成的，现在两边各自连续（5701xx / 6201xx）；以及"Mod 叫什么"
    和"它发布什么"重新变成一件事（原先一个 Mod 发两个 capability，活动那个只能写成
    一行字面量）。

14. **剩余：`admin.go`**（运维面）。未开始。

    需要它的地方是真实存在的运维死路，不是补齐对称性：`platform` 里尝试耗尽的订单
    （玩家付了钱、任何重试都不会发货）、`session` 释放失败后残留的 claim、`mail` 满
    了的邮箱、`global` 失效的租约、`global/activity` 里 audit 溢出的活动。这些都是
    "只有人能决定怎么办"的状态，而它们现在**在接口外**——这是对的（同一条总线上的
    任何进程都不该能关服、能抹掉一个排行榜），但也意味着必须另有一条路能到。

## 八、验收标准

每一步都要满足，否则不进下一步：

- `gofmt` / `go vet` / `go test` 全绿，关键包 `-race`；
- **每个修掉的 bug 都有一条回归测试，且经过"临时回退修复 → 测试变红"验证**。
  这条不能省：第 3.9 节末尾列的那些"把 bug 钉死的测试"就是省了这一步的产物；
- 测试不得使用不求值的替身（rank 的 `Pipeline() -> nil` 让生产路径零覆盖）；
- 并发类不变量必须有并发测试。cube 的这四个服务里**没有一个** `go func` 或
  `-race` 导向的测试，而它们的严重缺陷全是并发问题；
- 发布态 `GOWORK=off` 可独立构建；**已满足**（core v1.11.2 + kit v1.11.2）：
  `GOWORK=off` 下 build / vet / vet -tags integration / test / test -race 全绿，
  真实 Redis 集成测试同样全绿；
- 每个服务的 errcode 段完整，纯客户端错误不返回 `CodeInternal`；
- **集成测试要真的跑起来。** `integration/` 没有 `REDIS_ADDR` 时全部 `t.Skip`，
  于是"`go test -tags integration` 显示 ok"可以是完全没跑。第 12 步就是因为这个
  抓到一件事：四个包里断言**具体类型**的集成子测试长期"绿着"，真的连上 Redis 那天
  同时红掉四条。所以验收命令是
  `REDIS_ADDR=... go test -race -tags integration ./integration/`，并且要确认
  测试数不是零。

## 九、与业务仓的关系

`cube` 的迁移是**逐服务替换**，不是一次切换。roost-service 的每个服务先与 cube 的
实现并存，业务侧通过已有的 `game/controllers/<module>.FromRegistry` 接缝换 client
实现，验证后删除 cube 侧实现。

注意 roost-service 的服务**不是** `roost.yaml` 里的 `services`（那是进程入口）。
每个服务提供：业务逻辑包 + 一个 `app.Mod`（注册 capability），而 `app.Service`
进程壳留在业务仓——因为"哪些服务同进程部署"是部署决策，不是库的决策。
`roost add service` 生成的壳里装配它们。

最后一条，来自 3.9 的教训：**迁移过程中不要把 cube 的测试一起搬过来**。
有多处测试把缺陷当期望行为固定了，搬运会连错误一起继承。每个服务的测试重写。
