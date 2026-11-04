# 功能设计：联盟 / 公会服务（2026-11-04）

**状态：方案已定、实施中（维护者 2026-11-04 定方向：“先写方案，按推荐”；同日“未声明归属的 kind 按 A，其他 5 条也按推荐，开始实施”）。现行公会行为与它的限制以 [KNOWN-LIMITS“Remote”](KNOWN-LIMITS.md#remote) 为准，各阶段落地情况记在第 7 节。**

登记在 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。基线 `e12e3dc8`（origin/main）。codebase-memory 本轮连不上，第 1 节的现状结论全部按源码与 `rg` 核对，引用写到文件与函数。

## 0. 问题与维护者原话

game-demo 的 `Guild` 是 Remote 托管实体，但从不进入共享模式：第一笔写由建会的进程认领独占所有权，另一个 game 进程的 `JoinGuild` 在写准入被拒（`writer fenced`，KNOWN-LIMITS“game-demo 的公会跨进程写会被拒”）。要让多个 game 进程上的玩家使用同一个公会，有三条路：Remote 共享模式（每笔写一次 Redis 分布式锁）、业务里直接做 Redis CAS、把公会放进一个独立服务。

维护者 2026-11-04 的方向：

- “alliance entity 都在这个 service 中，他还是一个 remote entity，但是 player 可以使用 remote mirror 获取只读数据，写必须走 service”。
- 理由：逻辑清楚、性能可以、联盟能力更强。
- 不在业务里直接拿 Redis CAS（“太山寨”），也不再用 Remote 共享模式做公会。

## 决定一览

推荐项已由维护者确认，下表即决定；备选写在第 3 节“未采用的方案”。

| # | 事项 | 决定 |
| --- | --- | --- |
| S | 服务形状 | 独立 roost App（Nest + DataEngine + Remote），全部 Alliance / Guild 实体由服务**独占**（owner-routed）：不切共享模式、没有分布式锁。同一实例内多个联盟可以进同一笔 Nest 事务（宣战、合并、外交）；联盟的定时逻辑、仓库、排行放在服务里 |
| W | 写 | game 进程经 RPC 调服务（申请、审批、踢人、捐献、转让盟主……）；权限与规则在服务里按权威判；每个请求带 RequestID，按它幂等；超时按“结果未知”，用同一 RequestID 重查；写 RPC 返回新状态与版本 |
| R | 读 | 玩家侧经 Remote Mirror 读只读快照（成员、信息、科技），最终一致；任何写权限都不得依据 Mirror 判断 |
| 1 | 成员关系 | 唯一权威在联盟，**Player 上不存 GuildID**；game 进程经 Mirror 或一次查询得知。要改玩家数据的操作（建盟扣钱、捐献、领联盟奖励）走 JetStream Saga 跨进程步骤 |
| 2 | 分片与接管 | 按联盟 ID 分片到多个服务实例，复用 ownerroute 的“路由提示 + 持久 fence”分工；崩溃接管靠 App 单实例锁 + DataEngine 恢复；扩缩容用 `TransferRemoteOwnership` 按分片迁移 |
| 3 | 生成器 | 现有生成器能声明“带 Nest / DataEngine / Remote 的业务服务”，缺的是“实体归哪个服务写”“按分片路由的 RPC”“只读 Mirror Mod”三件；做这三件最小增量，不新建一整套“实体服务”生成器 |
| 4 | 新鲜度与写后可见 | Mirror 读取有明确的陈旧上限；写 RPC 返回新状态和版本，界面用返回值立刻显示，之后的 Mirror 读带这个版本做单调读 |
| E | 与现有的关系 | `e12e3dc8` 的多写者修复保留（共享模式与所有权往返仍是正式能力）；跨 Base Team（原型分支 `feat/seamless-world-prototype`）先不动；KNOWN-LIMITS 的公会条目改为指向本方案 |

## 1. 现状核实

### 1.1 game-demo 公会现在怎么做

| 功能 | 现在的实现 | 位置 |
| --- | --- | --- |
| 建会 `found_guild` | 端点先 `ValidateName`，读 Player 的 `GuildID` 做 (玩家, 名字) 幂等，Mongo 计数器 `$inc` 发号，`GuildLifecycle.Create` 本地创建实体，再 `MultiSync_FoundGuild` 一笔事务写 Guild 与 Player | `demo/game/controllers/player/guild.go.tmpl` 的 `HandleFoundGuild`；`guild_ids.go.tmpl` 的 `nextGuildID` |
| 入会 `join_guild` | `MultiSync_JoinGuild`：Guild + Player 一笔事务，Player 已在别的公会时拒绝，已是成员按成功回放 | `HandleJoinGuild`；`demo/game/handler/join_guild.go.tmpl` |
| 查询 `guild_info` | 按 ID 时 `Sync_GuildInfo` 在实体锁内读；“我的公会”先读 Player 的 `GuildID` | `HandleGuildInfo` |
| 成员与上限 | `GuildDao.Members` 一个 map，`MaxMembers = 50`，整文档读写 | `demo/db/def/guild.go.tmpl`、`demo/game/entities/guild/roster_component.go.tmpl` |
| Player 侧 | `PlayerDao.GuildID` 是 `persist,sync`，也在附近玩家看到的 `near` 视图里 | `demo/db/def/player.go.tmpl`、`demo/game/entities/player/sync_packer.go.tmpl` |
| GM | `gm.guild.info`、`gm.guild.join`（从本进程写） | `demo/internal/service/game/gm.go.tmpl` |
| 其他引用 | `sidmerge` 把公会集合列为 Remote 托管、合服不搬；loadtest 场景 `found_guild` / `guild_info`；mail `GuildScope`、聊天组频道、场景关系视野只在注释里提到公会 | `demo/cmd/sidmerge/main.go.tmpl`、`demo/loadtest/scenarios/demo.yaml.tmpl`、`enter_game.go.tmpl`、`collaborators.go.tmpl`、`scene.go.tmpl` |

模板注释（`entity.go.tmpl`、`protocol/def/guild.go.tmpl`、GM 说明）仍写着“谁拿到分布式锁谁就是这笔事务的 owner、任意进程都能写”，与行为不符；迁移时一并改掉。

### 1.2 Remote 写准入：独占模式本来就不经 Redis 锁

- `framework/remoteentity/batch.go` 的 `beginWrite`：写门 → 所有权读锁 → 共享模式才取 Redis 分布式锁（`lockSharedWrite`）；独占模式走 `WriteAuthority.GrantWrite`。
- `framework/remoteentity/mongo_authority.go` 的 `GrantWrite`：每笔写一次 Mongo `FindOneAndUpdate`，按 `_owner_sid` 过滤并递增 `_grant_fence`，回复丢失时按 token 回查。所有权已转走的进程在这一步被拒；同一个 sid 的新旧两代进程过滤条件相同，它们靠单实例锁分开（1.5），不靠这一步。
- `confirmWriteOwnership`：实体没有所有权记录时，**第一个写它的进程**经 `ClaimOwnership` 认领。这就是 demo 跨进程写被拒的原因，也是第 2.10 节要加“实体归属服务”守卫的原因：别的服务的进程一旦先写到一个新实体，就会把它认领走。
- `framework/remoteentity/ownership.go` 的 `TransferRemoteOwnership`：只能由**当前 owner** 发起（`expected.OwnerSid != m.localSid` 时 fence），先 Draining 排空本地写，再 CAS 转给新 sid、推进 marker 与 route 代际，旧实例置 Fenced。新 owner 第一次写时从权威冷加载（`e12e3dc8` 起落后副本先卸载再重载）。回归：`batch_test.go` 的 `TestTransferRemoteOwnershipFencesPreviousOwner`，真实依赖 `TestRealRemoteOwnershipClaimAndTransfer`、`TestRealRemoteProcessKillRecoversLease`（[实现篇 §5](../framework/impl/05-remote-mirror.md#5-回归入口)）。
- 多实体写：一笔事务里的多个 Remote 实体经 `IRemoteAtomicBatchCommitter` 在同一个 Mongo 事务提交，不支持时 fail-fast（[REMOTE_ENTITY“存储事务”](../framework/REMOTE_ENTITY.md#存储事务)）。所以“同一实例内多个联盟一笔事务”现有能力已经覆盖。
- 现有 Remote 负载样本（strict、2 实体 × 2 DAO、1000 会话 / 1 万实体、80TPS × 15 分钟，单机）：服务端 p50 / p99 117 / 195 ms（[remote-80tps-20261029](../performance/benchmarks/remote-80tps-20261029/README.md)）。这是现有唯一的 Remote 写延迟样本，不是联盟服务的容量结论。

### 1.3 Mirror 读取

- 两种声明：`//roost:mirror entityKind=… coll=…` 的只读 DTO（生成 `New<DTO>Reader`，读实体的缺省快照，`codegen/internal/entity/mirror.go`，样例 `codegen/internal/entity/testdata/remoteflow/guild_summary.go`）；DAO 字段上的 `remoteview` 投影加 Nest 请求里的 `RemoteViewRef`（I/O 段预取，[guide/05 §6](../framework/guide/05-remote-mirror.md#6-普通-entity-写入--remote-view-读取)）。
- 一致性档位（[REMOTE_ENTITY“读取”](../framework/REMOTE_ENTITY.md#读取)）：Cached 允许缓存版本；Monotonic 要求不低于调用方给的版本，不满足时回源一次；Linearizable 直读权威。`RemoteMirrorReader.Read(ctx, id, consistency, after)`（`framework/entity/remote_mirror.go`）只有读，没有变化回调。
- 陈旧上限：`remote_entity.cached_max_staleness`，不配时等于 `snapshot_cache_ttl`，缺省 30s（`framework/remoteentity/config.go`、`wiring/remoteentity/config.go`）。超过上限的 L1 条目先经 L2 或权威重新确认，确认不了就不交出。不覆盖 L2 本身落后于权威（owner 写 L2 失败或结果未知时，最长到 `snapshot_l2_ttl`）。
- 推送：读者登记兴趣后，owner 提交经 outbox 发布到 L2 并按兴趣推送（JetStream 模式才有推送，普通 NATS 只按需读）。兴趣表是广播软状态：每个节点存全集群兴趣，缺省总上限 `snapshot_interest_subs = 262144`，每个读者进程 `snapshot_interest_per_consumer` 缺省为总量 / 16（16384）；超出配额的 key 没有推送，只按需读取。
- 只读进程的装配：`wiring/remoteentity.NewRemoteMirrorMod`（只装 `SnapshotClient`，没有写 Manager、Mongo backend 与分布式锁），业务用 `MirrorSource(registry)` 读。
- 已有测量（2026-10-06，v1.21 时期，macOS 单机私有依赖，[Mirror 第 6 步本机替代](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)）：L1 命中约 0.7µs、L2 回源约 25µs、权威回源约 125µs（p99 约 1µs / 51µs / 270µs）；提交到只读方读到约 30ms；扇出 1 → 100 个读者、一轮 10 个 key 约 1.8 → 25ms；推送中断时退化到陈旧上限回源。之后 Remote 链路有改动，这些数字只作量级参考，不是当前基线。

### 1.4 服务 RPC

- `infra/network/servicerpc/client.go` 的 `BusClient.Call`：能用 `CallTo` 指定 sid；必须在 I/O 段调用（`fctx.BlockingError`），不能在 Nest handler 里同步调。
- `infra/network/servicerpc/affinity.go` 的 `KeyAffinityPicker`：按 key 的哈希对**当前发现到的实例集合**取模。注释写明它不是正确性机制，实例增减时映射跟着变。所以它不能直接当联盟分片表用：映射变了，请求就落到不是 owner 的实例上。
- NATS RPC 客户端在超时与 no responders 时按策略自动重试（`infra/network/nats/driver/rpc.go` 的 `isRetryable`），同一请求可能送达多次；写 RPC 必须按 RequestID 幂等。
- 普通 Bus RPC 不把调用方截止时间传到接收端，接收端要给整次处理自己的预算（[SLG §7.7](../framework/SLG-INTEGRATION.md#77-进阶深化联合链路的接法要点)“跨实体 / 跨服投递”一行）；JetStream RPC 才按调用方截止建立处理期限。

### 1.5 App 单实例锁与接管

`framework/app/singleton.go`：同一服务类型 + sid 同一时刻只有一个进程跑 Mod；在第一个 Mod Init 之前取锁（别人持有就等），按 `singleton.renew_interval`（缺省 3s）续期，失锁经 RuntimeFailure fail-stop；`singleton.ttl` 缺省 15s（`config_schema.go` 的 `singletonConfig`）。`SingletonLiveness` 只读回答“这些 sid 里哪些有进程持锁”。DataEngine WAL 按 sid 分目录（[生成器 §5“按 sid 区分的本地状态”](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)），同 sid 重启从自己的 WAL 恢复。

### 1.6 Saga 与 Remote

- 原生步骤不能改 Remote 实体：`Complete` 遇到带 Remote 写批次的事务返回 `ErrNativeStepRemoteWrite`（`framework/saga/step_receipt.go`），生成器也拒绝 `remote=managed` 的回执宿主（[SAGA.md 原生步骤执行契约第 7 条](../framework/SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定2026-10-28-决定-2a-改为回执在实体里)）。
- 调另一个服务的步骤用 Mongo 步骤，被调服务按 `IdempotencyKey` 幂等。game-demo 送礼的 `deliver`（发邮件，`RequestID = "gift:" + IdempotencyKey`）就是这个形状（`demo/internal/service/game/gift_saga.go.tmpl`）。
- 事务里 `Emit` 会把 durability 升到 strict（`framework/nest/rollback.go` 的 `NewRollbackTx` 注释）；同一条 WAL 记录里的 Remote 写与 effect 在投影时进同一个 Mongo 事务（`framework/dataengine/engine/mongo_projection.go`）。Remote + Emit 有 Nest 单元用例（`framework/nest/remote_commit_test.go`），真实依赖下“Remote 实体 handler 里 EmitStart / Emit”没有端到端用例。
- 容量：原生正向 128 并发约 1050 笔/秒（单机，[SAGA.md“跨服交易”](../framework/SAGA.md#跨服交易使用约束)）。

### 1.7 ownerroute

`infra/network/ownerroute/route.go` 的 `Router`：`Resolver` 给出 owner sid，本地执行或经 `Transport` 单向发送（`BusTransport.Send`，没有回复）。仓库里唯一的使用者是 demo 送礼 saga 的步骤转交。指南的定位是“路由提示不能替代持久 fence 校验”（[guide/14](../framework/guide/14-foundation.md)）。本方案沿用这个分工：分片表只是路由提示，持久 owner 与 fence 才是权威；需要回复的写走 servicerpc，不用 `BusTransport`。

### 1.8 生成器现状

| 能力 | 现状 | 位置 |
| --- | --- | --- |
| 声明一个带实体的业务服务 | **有**。`roost.yaml` 的 `services.<name>.mods` 可以写 `nest`、`dataengine`、`remote_entity`；`roost add service alliance -mods …`；同一工作目录用 `--sid` 起多个实例，WAL 按 sid 分目录 | [生成器 §4～§5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#4-roostyaml) |
| 框架托管服务（account、mail、match、rank 等） | 九个都是 Redis 存储（`Depends: redis, nats`），没有 Nest / DataEngine，不能当“实体服务”的样板 | `codegen/internal/roost/framework_services.go` 的 `frameworkCatalog`；`service/*` 不导入 `framework/nest` |
| 本工程自己的 RPC | **有**。`roost add rpc Alliance -service alliance` 生成接口、实现骨架、owner Mod、两半生成代码；调用方写 `uses_rpcs`；可加 `//roost:rpc affinity=<param>` | `codegen/internal/roost/add_rpc.go` |
| 实体归哪个服务写 | **没有**。实体与 Nest handler 全工程一份（`internal/registry` 聚合，bootstrap 一个 `EntityAccess`），每个带 Nest 的服务都注册全部 kind；`remoteRuntimeHosts` 的注释直说“实体由哪个服务写在静态上看不出” | `codegen/internal/roost/render.go`、`generate.go` 的 `remoteRuntimeHosts` |
| 按分片路由 | **没有**。只有上面的哈希亲和 | 1.4 |
| 只读 Mirror Mod | **没有**。Mod 目录里没有 `RemoteMirrorMod`，配置说明写着“手工接入 Mirror Mod” | `codegen/internal/roost/catalog.go`、`kitconfig_gen.go` |
| 只读进程的 `RemoteViewRef` 预取 | **没接**。Nest 有 `NestOptionWithRemoteSnapshotResolver`，wiring 的 Nest Mod 不会从 `MirrorSource` 取 | `framework/nest/nest.go`、`wiring/nest` |
| Remote 实体的持久定时器 | **拒绝**。`roost add timer` 对 Remote 托管与 noPersist 实体报错（D11 方案 C 未做） | `codegen/internal/roost/add_timer.go`；KNOWN-LIMITS“按实体的持久定时器” |
| Remote 实体的心跳 | 独占模式支持；共享模式下一拍停 | [DESIGN-TODO“本轮收敛”](DESIGN-TODO.md#本轮收敛) |
| Saga 骨架 | **有**。`roost add saga -native -entity Player` 给 Player 加回执；Mongo 步骤的步骤体手写 | [生成器 §6](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#6-日常业务脚手架) |

game-demo 拓扑：一个业务服务 `game`（第二个进程用 `deploy/dev/second-game.sh`，sid 1001）加九个托管框架服务，共十个进程（[demo README](../../demo/README.md#本地实跑)）。

## 2. 方案

### 2.1 服务形状与拓扑

```
 客户端 ──TCP──▶ game 进程（sid 1000 / 1001 …，Player 按 sid 静态归属）
                   │  写：RPC（RequestID，I/O 段调用）        读：Remote Mirror（只读快照）
                   ▼                                          ▲
              alliance 服务实例（sid 3000 / 3001 …）  ── outbox 发布 ── L2 / 推送
                   │  Nest + DataEngine（WAL，按 sid）+ Remote（独占，owner-routed）
                   ▼
               Mongo 权威（Remote 元数据、联盟 DAO、成员索引 DAO、请求回执）
```

- alliance 是工程里的一个业务服务（不是框架托管服务），mods：`nest`、`dataengine`、`remote_entity`、`nats`、`etcd`（注册发现），需要定时逻辑时加 `heartbeat` 或本地实体定时器（2.8）。
- game 进程不再装 `remote_entity`，改装只读的 Mirror Mod（2.10 第 3 项），因此 game 进程**没有能力**写联盟实体；写只能走 RPC。
- 缺省部署一个 alliance 实例：所有联盟在同一进程，任何多联盟操作都能一笔事务。容量不够时才按 2.7 分片。

### 2.2 实体与数据

| 实体 | 键 | 内容 | 说明 |
| --- | --- | --- | --- |
| `Alliance`（Remote 托管） | 联盟 ID | 名字、盟主、成员表（玩家 → 职位、加入时间、静态原服 sid）、科技、仓库、请求回执 | 成员关系的唯一权威 |
| `AllianceMember`（Remote 托管） | 玩家 ID 派生 | 所在联盟、待定的入会预约、退盟冷却、每日捐献计数、请求回执 | “一人一盟”的守卫与玩家维度的联盟状态，见 2.5；**不是**第二份成员权威，与联盟成员表不一致时以联盟为准 |
| `AllianceShard`（普通本地实体，每实例一个） | 实例 sid | 联盟定时任务的持久节点 | 承载 `roost add timer`，见 2.8 |

- **联盟 ID 编码分片槽位**：`ID = 序号 × SlotCount + slot`。路由只按 ID 计算，不查表；`AllianceMember` 的槽位按玩家 ID 取模。序号用每个槽位一个 Mongo 计数器（沿用 demo `nextGuildID` 的 `$inc` + majority 写确认），发号失败留空号不回收。
- **请求回执放在被改的实体里**：`Requests map[RequestID]回执`（结果摘要 + 时间），随业务修改同一事务提交与回滚，同一 RequestID 再来时回放原结果；保留期与上限按 Saga 回执的做法（`saga.receipt_retention` 缺省 168h，每次提交顺手剪过期项）。第一个实施版本放在服务骨架里，等第二个实体服务出现再考虑上升为框架件。
- **成员表放一个文档**：沿用 demo 的整文档读写，人数上限是真实限制（见第 6 节第 1 条）。

### 2.3 写：RPC 契约

**接口形状**（`roost add rpc Alliance -service alliance` 生成，方法按业务增加）：`Found`、`Apply`、`Approve`、`Join`（无审批的直入）、`Leave`、`Kick`、`Transfer`、`Donate`、`ClaimReward`、`Info`（权威读，给 GM 与写后核对）、`Result(target, requestID)`（按 RequestID 查回执）。每个写方法的参数都含 `requestID`、调用者玩家 ID、调用者原服 sid；调用者身份来自 game 进程的已鉴权会话，不来自客户端字段。

**服务端处理**：RPC handler 在 I/O 段，用自己的处理预算（1.4）调生成的 `Sync_*` Sender 进 Nest；handler 在实体锁内先查回执，再按权威判权限与规则，写回执与修改，返回新状态。

**答复分类**（客户端与 game 端点据此决定显示与重试）：

| 答复 | 什么时候 | 调用方怎么做 |
| --- | --- | --- |
| 成功 + 新状态 + 版本 | 提交已确认，或回执回放 | 界面直接用返回值；后续 Mirror 读带这个版本（2.4） |
| 业务拒绝（带错误码） | 权限不足、已满、已在别的联盟、名字非法等 | 显示原因，不重试 |
| 可重试，未执行 | 服务未就绪（启动恢复中、handler 未注册，表现为 no responders）、所有权转移中、写准入在本实例发现自己不是 owner | 原 RequestID 重试；“不是 owner”带新 owner sid，调用方改投一次（2.7） |
| 结果未知 | RPC 超时、`ErrRemotePersistenceIndeterminate`、处理预算到期 | 客户端显示“处理中”；game 端点用原 RequestID 调 `Result` 或原样重发，直到得到成功或拒绝 |

**写后立刻看到**：写 RPC 返回本次提交后的联盟视图（与 Mirror DTO 同一形状）和实体版本 `StateVersion`。需要“写完立刻看到”的界面用返回值，不去读 Mirror。

### 2.4 读：Mirror

- game 进程读两类快照：联盟（成员列表、信息、科技、公告）与自己的 `AllianceMember`（我在哪个盟、职位）。缺省用 `//roost:mirror` 只读 DTO，读实体缺省快照，版本就是实体的 `StateVersion`，与写 RPC 返回的版本可比。`remoteview` 字段投影只在需要按字段裁剪时用；视图版本只在视图字段变化时推进（guide/05 §6），不能拿写 RPC 返回的实体版本去要求它。
- 玩家 handler 里要用联盟数据（如科技加成）时用 `RemoteViewRef` 在 I/O 段预取，handler 内不回源（guide/05 §6 的现行规则）。只读进程要先补上 Nest 的快照来源接线（2.10 第 3 项）。
- **一致性选择**：普通展示用 Cached；刚写完之后用 Monotonic，`after` 取写 RPC 返回的版本，读到的版本不会比自己写的旧；需要权威时调服务的 `Info`，不用 Mirror 的 Linearizable（只读 loader 不一定支持）。
- **新鲜度边界**：推送正常时提交后几十毫秒量级可见（1.3 的历史样本约 30ms）；推送中断、owner 刚重启、读者不在推送配额内时，退化为 `cached_max_staleness` 回源，最长约等于这个值（缺省 30s）；L2 写失败或结果未知时最长到 `snapshot_l2_ttl`。game 进程（读者）的配置建议把 `cached_max_staleness` 写成 5s（陈旧上限越短，回源越多；阶段 5 量回源量后再定）。
- **写权限不依据 Mirror**：端点可以用 Mirror 做便宜的预检（例如“已在联盟”直接提示），但最终判定一律在服务里按权威做，预检与服务结论冲突时以服务为准。
- **容量边界**：兴趣表全集群共用。每个在线玩家读自己的 `AllianceMember` 会占兴趣条目：10 万在线时接近缺省总上限的一半，单个 game 进程超过 16384 个 key 后超出部分不推送、只按需读。阶段 5 按目标在线数测，必要时调 `snapshot_interest_per_consumer`，或让成员快照只在登录与收到通知时读、不长期持有兴趣。

### 2.5 成员关系与“一人一盟”（决定 1）

- **权威**：联盟的成员表。Player 上不存 `GuildID`。
- **一人一盟是跨联盟的约束**：两个联盟可能在不同实例，同一玩家同时被两个联盟批准时，单个联盟的事务挡不住。用 `AllianceMember` 做守卫，两步完成，执行方是处理批准的联盟所在实例：
  1. 在玩家的 `AllianceMember` 上预约“加入联盟 A，RequestID = r”（已在别的联盟或有别的未完成预约就拒绝）；
  2. 在联盟 A 的事务里加成员，写回执 r；同一事务 `Emit` 一个“成员已变化”effect；
  3. effect 消费者在玩家的 `AllianceMember` 上把预约确认成“在联盟 A”。第 2 步被明确拒绝时释放预约。
  预约带期限；期限到了仍未确认，守卫方先向联盟 A 的权威问“玩家是否在成员表里”，按权威确认或释放，不按超时直接释放。两个实体在同一实例时三步合成一笔多实体事务。退盟、踢人反向：先改成员表，再由 effect 清 `AllianceMember`。
- **game 进程怎么知道“我在哪个盟”**：`AllianceMember` 的 ID 由玩家 ID 算出，直接读它的 Mirror，再按其中的联盟 ID 读联盟 Mirror；不需要查找服务。
- **附近玩家头顶的公会**：`PlayerDao.GuildID` 改为 `dao:"nopersist,sync"` 的显示字段，登录时从 Mirror 填，收到成员变化通知时更新。它只用于显示与同步，任何写判断都不读它。
- **在线通知**：成员变化 effect 的消费者按成员记录里的原服 sid 经 Bus 发给该 game 进程（尽力投递）；game 进程更新显示字段并推客户端。玩家不在线或消息丢失时，下次登录从 Mirror 补齐。

### 2.6 要改玩家数据的操作：Saga（决定 1）

规则：Player 在 game 进程，联盟在 alliance 服务，两边是两个事务域。改 Player 的步骤是 Player 上的原生步骤（回执在 Player，demo 已有 `SagaReceipts`）；改联盟的步骤是 Mongo 步骤，步骤体调 alliance RPC，`RequestID = IdempotencyKey`（原生步骤不能写 Remote，1.6）。Saga 从 game 进程的 Player handler 里 `EmitStart` 启动；端点回复“已受理”与交易号，结果经 `Result` / 通知确认（[SAGA.md“跨服交易”](../framework/SAGA.md#跨服交易使用约束)）。

| 操作 | 步骤 | 补偿 |
| --- | --- | --- |
| 建盟扣钱 | ① Player 扣建盟费（原生）② alliance `Found`（Mongo 步骤 → RPC） | ① 退款。② 被拒（名字非法、已在联盟）时补偿 ① |
| 捐献 | ① Player 扣资源（原生）② alliance `Donate` 记入联盟资金与个人贡献 | ① 退款 |
| 领联盟奖励 | ① alliance `ClaimReward` 在 `AllianceMember` 上标记已领（Mongo 步骤 → RPC）② Player 发奖（原生） | ② 被拒（背包满）时补偿 ①：撤销标记 |

参考做法：SLG 联盟活动的“原服奖励义务”（[SLG §7.3](../framework/SLG-INTEGRATION.md#73-状态归谁怎样调用)）是同一个思路——先持久接收义务，再可重试地到账，Player 侧靠永久回执去重。这里用 Saga 把这两步与补偿交给框架，不手写义务账本。

**demo 现有公会功能的对应**：

| 现有 | 迁移后 | 走哪条路 |
| --- | --- | --- |
| `found_guild` | 加建盟费用（覆盖“扣钱失败回滚”验收）；端点只做名字预检与 Mirror 预检，`EmitStart` 建盟 Saga | Saga |
| `join_guild` | `Join(requestID)` RPC（demo 没有审批，直入），返回新状态 | RPC |
| 新增 `leave_guild` | `Leave(requestID)` RPC；验证守卫释放与通知 | RPC |
| `guild_info`（按 ID） | 联盟 Mirror，Cached | Mirror |
| `guild_info`（我的） | `AllianceMember` Mirror → 联盟 Mirror | Mirror |
| `gm.guild.info` / `gm.guild.join` | 移到 alliance 进程自己的 GM 命令（ops `/admin/execute`），读用权威 | 服务内 |
| `PlayerDao.GuildID`（persist） | `nopersist,sync` 显示字段 | Mirror + 通知 |
| `nextGuildID`（game 里的 Mongo 计数器） | 移到服务，按槽位发号 | 服务内 |
| 错误码 `guild_*` | 保留；增加“服务忙 / 改投 / 结果未知”三类 | RPC 答复 |
| 响应里的 `ServedBySID` | 改为处理请求的 alliance 实例 sid | RPC |
| loadtest / Robot `found_guild`、`guild_info` | 增加两个 game 进程上的玩家加入同一公会 | 验收 |
| `sidmerge` 的 Remote 托管集合 | 公会集合归 alliance 服务，合服仍不搬 | 不变 |
| mail `GuildScope`、聊天组频道、场景关系视野的注释 | 成员来源改为 Mirror / 成员变化通知 | 注释 |

### 2.7 分片、接管与迁移（决定 2）

**分片数怎么配**：

- `alliance.slots`：槽位总数，全部进程相同、上线后不改（编码在联盟 ID 里），推荐 256。
- `alliance.shards`：槽位区间到 sid 的表，例如 `["3000:0-127", "3001:128-255"]`。规则只声明一次（配置结构体 + tag），运行期加载时校验覆盖全部槽位、区间不重叠、sid 不重复；game 与 alliance 两类服务读同一个键，doctor 的跨服务配置检查核对两边一致。
- 一个实例时表里只有一行 `3000:0-255`。

**路由**：调用方按 `slot(ID)` 查表得到 sid，`CallTo` 这个 sid。表只是提示：真正的 owner 是 Remote 的持久所有权记录，写准入照常 fence。实例收到不归自己的请求时（表过期、迁移中），读 `GetRemoteOwnership` 得到当前 owner，答复“改投 sid X”；调用方改投一次，仍不对就按可重试失败回复。

**崩溃接管**（kill -9、宿主机故障）：

1. 同一个 sid 由进程管理拉起（同机重启，或换机后挂上同一块 WAL 卷；与 game sid 同一个运维模型，[PLAYER-MIGRATION](../framework/PLAYER-MIGRATION.md#2-步骤)）。
2. 新进程在第一个 Mod Init 前等单实例锁：旧进程崩溃时最多等 `singleton.ttl`（缺省 15s）；旧进程只是卡住时，新进程要等锁过期才启动，旧进程按续期期限判定失锁并 fail-stop（Nest 经 OnFail 围栏），两代不同时写（单实例锁的保证，1.5）。
3. DataEngine 从本 sid 的 WAL 恢复未投影的记录，Remote outbox 补发未发布的快照；RPC 的 owner Mod 依赖 Nest（2.10 第 4 项），总线 handler 在这之后才注册，之前的请求得到 no responders。
4. 所有权记录仍指向这个 sid，不需要转移。

**接管窗口内写请求的答复**：进程不在时，NATS 回 no responders（未投递，“可重试，未执行”）；崩溃那一刻在途的请求是超时（“结果未知”，原 RequestID 重查）；新进程恢复期间 handler 还没注册，同样是 no responders（“可重试，未执行”）。game 端点把前后两类合成“服务忙，请稍后”，把超时显示为“处理中”。只读不受影响：Mirror 从 L2 继续读，陈旧上限内读到崩溃前最后发布的版本。

**扩缩容迁移**（例如 1 个实例扩到 2 个）：

1. 起新实例 3001（此时表里还没有它，它不接写）。
2. 在旧 owner 3000 上执行运维命令“把槽位 128–255 转给 3001”：逐个联盟 `TransferRemoteOwnership(id, 3001)`，每个先排空已准入的写；转移期间这个联盟的写答复“可重试”，转完的答复“改投 3001”。只有当前 owner 能发起转移，所以命令跑在旧实例里。
3. 滚动更新各进程的 `alliance.shards`；更新前的调用方靠“改投”继续工作。
4. 3001 第一次写到某个联盟时从 Mongo 冷加载。缩容反向。

迁移只转移活着的 owner；死掉的 owner 只能按“崩溃接管”同 sid 拉起，不能由别的 sid 接走（`TransferRemoteOwnership` 要求本地是 owner）。

### 2.8 定时逻辑、仓库、排行

- **定时**：Remote 托管实体不能挂 `roost add timer`（1.8）。每个实例一个普通本地实体 `AllianceShard`（按 sid 归属，用现有实体定时器），节点记“哪个联盟、什么时候、做什么”；到期回调用异步 Sender 给对应联盟发 Nest 消息。联盟在线期间的周期推进（例如活动阶段）可以用独占模式下的心跳。迁移槽位时，旧实例把这些联盟的节点删掉、新实例在冷加载后补建（规则在骨架里写死）。Remote 实体原生持久定时器（D11 方案 C）不在本方案范围。
- **仓库**：联盟 DAO 的字段，写走 RPC；存入 / 取出涉及 Player 时按 2.6 走 Saga。
- **排行**：联盟内排行（贡献）在联盟 DAO 里算；跨联盟排行用 `service/rank`，由联盟事务 `Emit` 的 effect 推进，不在锁内调 RPC。

### 2.9 多联盟事务

- 同一实例上的多个联盟：一笔多实体 Nest 事务（Remote 原子批次，1.2）。宣战、外交、合并在缺省单实例部署下都是这种。
- 分到不同实例时：用两步（发起方记意图 → 对方受理 → 发起方确认），每步带 RequestID，最终一致。需要原子的操作（合并）先把参与的联盟所在槽位迁到同一实例，再一笔事务。
- 是否要求跨实例的强一致外交，是产品选择，见第 6 节第 4 条。

### 2.10 生成器：最小增量（决定 3）

现有生成器已经能做：声明 alliance 业务服务及其 mods、多 sid 实例、`roost add rpc` 的服务与调用方、Remote 托管实体与 DAO、`//roost:mirror` DTO、Player 原生步骤 Saga。只补下面四件，不新建一整套“实体服务”生成器：

1. **实体归属服务**：实体标记加 `service=<name>`（或 `roost.yaml` 里 `services.<name>.entities`），只声明一次。生成器据此只在该服务注册这个 kind 的 Nest handler 与 lifecycle；运行期 Remote Manager 拿到“本服务可写的 kind”列表，对不在列表里的 kind 拒绝认领与写许可（不靠调用方自觉，防止别的服务的进程把新实体认领走，1.2）。doctor 检查“声明了归属的 kind 被别的服务的 Sender 调用”。
   - **未声明归属的 kind 保持现行为**（维护者 2026-11-04 选 A）：任何带 Nest 的服务都能写，只有声明了 `service=` 的 kind 受守卫约束；Player 的静态 sid 归属、跨 Base Team 的共享模式、SLG 联盟夹具都不受影响。声明位置用实体注解 `service=<name>`，不在 `roost.yaml` 再写一份。
2. **按分片路由的 RPC**：`//roost:rpc shard=<param>`（参数是联盟 ID 或玩家 ID），生成的客户端读 `alliance.slots` / `alliance.shards` 算 sid、`CallTo`、处理一次“改投”。框架侧在 `infra/network/servicerpc` 加按槽位表选 sid 的 picker 与“改投”答复码；配置声明与校验一次。
3. **只读 Mirror Mod**：Mod 目录加 `remote_mirror`（`wiring/remoteentity.NewRemoteMirrorMod`），依赖 `redis`、`syncbus`、`mongo`；带 Nest 的服务装了它时，wiring 的 Nest Mod 用 `MirrorSource` 接上 `NestOptionWithRemoteSnapshotResolver`，使 `RemoteViewRef` 在只读进程可用。
4. **写 RPC 的约定**：`roost add rpc` 增加 `-entity-service` 选项，骨架方法带 `requestID` 参数、`Result` 查询、处理预算与 2.3 的四类答复；owner Mod 依赖 `nest`（现在的 owner Mod 只依赖 `nats`，`add_rpc.go` 的 `renderRPCMod`），保证 DataEngine 恢复与 Nest 启动之后才在总线上接请求；请求回执的 DAO 字段与读写方法由同一选项生成到实体上。

game-demo 是参考实现，不改成生成产物；以上四件生成器能力在 demo 迁移里手工用到同一套接口，生成器另有自己的生成工程用例（阶段 2）。

### 2.11 与现有的关系

- `e12e3dc8` 的多写者修复保留：共享模式、所有权转走又转回仍是正式能力，SLG 联盟夹具、`gameplay/heartbeat` 的共享模式用例与原型分支都在用。本方案只是公会不再用共享模式。
- 跨 Base Team（原型分支 `feat/seamless-world-prototype` 的 `team_roster_cross_base`）先不动；联盟服务成型后再评估是否迁入同类服务。
- KNOWN-LIMITS 的“game-demo 的公会跨进程写会被拒”改为指向本方案（方案已定、未实施）；实施完成后删掉那条。

## 3. 未采用的方案

| 方案 | 不采用的原因 |
| --- | --- |
| Remote 共享模式做公会 | 维护者 2026-11-04 不再采用。每笔写一次 Redis 分布式锁往返，多个写者各持一份内存副本，靠写准入卸载重载对齐（`e12e3dc8` 才修好）；联盟内多实体事务与定时逻辑都要跨进程协调 |
| 业务里直接用 Redis CAS（`versionstore` 一类） | 维护者原话“太山寨”。成员、权限的条件更新散在业务代码里，与 Mongo 权威、Mirror 发布、Nest 事务不在一条链路上 |
| 联盟做普通本地实体（不是 Remote），服务自己发布快照 | 失去 Mirror 的发布、L2 水位与推送，失去 `TransferRemoteOwnership` 迁移与 fence；要重做一遍 Remote 已有的东西 |
| Player 上保留持久 `GuildID`（与联盟同事务写） | 联盟与 Player 不在同一进程后，两边只能经 Saga 同步，两份事实会分叉；决定 1 选联盟为唯一权威 |
| 用 `KeyAffinityPicker` 当分片表 | 映射随发现到的实例集合变化，实例重启或扩容时请求落到非 owner 上（1.4） |
| `saga.coordinators` 式的“sid 列表取模”分配 | 增减一个实例会让大部分槽位换 owner；显式区间表只移动要迁的槽位 |
| 用 `service/directory` 预约做“一人一盟” | 预约在 Redis、带过期，与 Mongo 里的成员权威是两个存储；`AllianceMember` 守卫在同一套 Remote 链路里，可以和联盟一起迁移、一起恢复 |
| 新建完整的“实体服务”生成器（一条命令出全部骨架） | 现有命令已覆盖大部分，缺的只是 2.10 的四件；整套生成器要维护第二份服务模板 |
| 服务内用 Durability memory（直写 Mongo、不要 WAL 卷） | 带 effect / Saga 启动的事务仍会升到 strict（1.6），WAL 卷省不掉；与 game sid 用同一个“按 sid 的 WAL 卷”运维模型更简单。是否对纯联盟写单独用 memory，留到阶段 5 量完延迟再定 |

## 4. 实施拆分

估时按一名熟悉仓库的实施者计，含测试与文档，不含等待评审。

| 阶段 | 内容 | 验收 | 估时 |
| --- | --- | --- | --- |
| 1 框架 | ① Remote Manager 的“本服务可写 kind”守卫（认领与写许可前拒绝）；② `servicerpc` 槽位表 picker、“改投”答复码与配置声明；③ wiring Nest Mod 在装了只读 Mirror Mod 时接上快照来源；④ Remote 实体 handler 里 `Emit` 的真实依赖用例（1.6 缺的那条） | 每项先红后绿；`framework/remoteentity`、`framework/nest`、`infra/network/servicerpc`、`wiring/*` 受影响包 `-race`；跨包改动 push 前全量 `GOWORK=off go test ./...` | 4～6 天 |
| 2 生成器 | 2.10 的四件：实体 `service=`、`//roost:rpc shard=`、`remote_mirror` Mod、`add rpc -entity-service`；doctor 与 `--check-config` 共用同一份声明 | 生成器包测试；临时生成工程 build / vet / test；`go generate ./...` 后 porcelain 干净 | 4～5 天 |
| 3 服务骨架 | demo 内的 alliance 服务：`Alliance`、`AllianceMember`、`AllianceShard`，RPC 接口与 handler，请求回执，成员变化 effect 与通知，槽位转移运维命令 | 服务包单元测试：回执回放、权限、一人一盟两步与期限后按权威收尾、多联盟一笔事务 | 3～4 天 |
| 4 demo 迁移 | 2.6 的对应表全部落地；game 去掉 `remote_entity`、装只读 Mirror；dev-run 加 alliance 实例；Robot / loadtest 场景；注释改正 | 重新生成 game-demo，build / vet / test、`roost id check`、`roost generate --check` 干净；`make dev-smoke` 通过 | 3～4 天 |
| 5 测试与端到端 | 新脚本（或扩展 `scripts/test-demo-generated.sh`）在真实依赖上跑：**两个 game 进程上的玩家加入同一公会**并互相读到；**alliance 实例 kill -9** 后同 sid 拉起，窗口内请求只得到“可重试 / 结果未知”，原 RequestID 重查后结论唯一、无重复入会；**分片迁移** 1 → 2 实例，迁移中持续写不丢、不重复，旧表调用方经改投成功；**Saga 扣钱失败回滚**：建盟被拒后金币退回，Player 扣款步骤后 kill -9 game 进程，恢复后只扣一次、退一次；一人一盟：两个联盟并发批准同一玩家只成功一个。另做单实例写 RPC 延迟与 Mirror 回源量的短测（≤ 30 分钟一轮） | 全部 `-race`；证据进 `docs/performance/benchmarks/<label>/`；Linux 与跨机不在本轮验收 | 5～7 天 |
| 6 文档 | guide/05、guide/09 增加“实体服务”一节；demo WALKTHROUGH；KNOWN-LIMITS 删旧条目、登记剩余边界；CHANGELOG；功能版发版前写 `docs/release/vX.Y.0-NOTES.md`（存储格式变化按未上线规则：不兼容、需清空公会数据） | `scripts/check-doc-anchors.py` 0 broken；根包 `GOWORK=off go test -count=1 .` | 1～2 天 |

合计约 20～28 天。阶段 1、2 可以并行；3 依赖 1；4 依赖 2、3；5 依赖 4。

## 5. 风险与未验证项

- **Remote 实体 handler 里 `EmitStart` / `Emit`**：阶段 1 第 ④ 项已补真实依赖端到端（`remoteflow` 的 `TestGeneratedRemoteHandlerEmitsEffectsWithTheRemoteWrite`，2026-11-04 macOS 私有依赖通过）：effect 与 Remote 写同一条提交记录投影、各发布一次，失败不提交。本用例只覆盖单进程 strict 与 memory 升 strict，不含 Remote handler 提交后 kill -9、WAL 重放时 effect 的去重（阶段 5 的 Saga 用例覆盖）。
- **写 RPC 延迟**：唯一的 Remote 写样本是 strict p50 / p99 约 117 / 195 ms（1.2），加上 RPC 一跳与 Mongo `GrantWrite`。公会操作可以接受，但没有联盟服务自己的测量。
- **Mirror 数字是历史值**：1.3 的读延迟与推送延迟来自 2026-10-06（v1.21 时期），之后 Remote 链路改过；阶段 5 只做短测，不做长稳。
- **视图版本与单调读**：写 RPC 返回的是实体版本；`remoteview` 视图的水位只在视图字段变化时推进。缺省用读实体缺省快照的 `//roost:mirror` DTO 避开这个问题；若业务改用字段视图做写后读，要先在阶段 5 核对 Monotonic 对视图 scope 的判定。
- **兴趣表容量**：每个在线玩家一个 `AllianceMember` 兴趣时，大规模在线会接近缺省上限（2.4），没有测。
- **成员表文档大小**：整文档读写，人数上限越大每笔写越重；上千人联盟没有测（第 6 节第 1 条）。
- **一人一盟的两步**：期限、权威收尾与 effect 重投的组合是本方案新写的协议，阶段 3 用单元测试、阶段 5 用并发批准与 kill -9 验证；之前不能宣称“一人一盟”成立。
- **槽位表滚动更新期间**：调用方的旧表靠“改投”一次，改投目标也在转移中时答复可重试；高频写联盟在迁移时会有秒级不可写。
- **接管依赖 WAL 卷**：换机拉起同 sid 必须带上原 WAL 卷，或先确认 WAL 已排空；与 game sid 相同的部署约束，部署文档要写明。
- **平台**：只在 macOS 本机验收；Linux 复跑与跨机不在本轮。

## 6. 剩余问题的决定（维护者 2026-11-04：全部按推荐，即每条的 A）

下面五条当时列为待定，维护者 2026-11-04 全部选 A（推荐项），按 A 实施；B / C 保留作记录。

1. **联盟人数上限**。成员表是一个文档。
   - A（推荐）：上限可配置，缺省 100、硬上限 500；超过就拒绝。代价：千人级联盟要另做。
   - B：成员拆到 `AllianceMember`、联盟只存计数与名单分页。代价：成员列表变成跨实体读，审批与踢人都要两步，复杂度明显上升。
2. **跨服公会是否在范围内**（不同 game 服 / 服组的玩家能否进同一公会）。
   - A（推荐）：在范围内。一个部署一个联盟服务，联盟可以有任意 game sid 的玩家，这正是独立服务的好处；成员记录带原服 sid 用于通知。代价：合服、分服时联盟不随某个服走，运维要知道这一点。
   - B：按服组隔离，联盟带服组标识，跨组加入拒绝。代价：多一层校验；需要时以后可以从 A 收紧到 B。
3. **“一人一盟”的实现**（2.5，本方案推导出的设计，维护者此前没有定过）。
   - A（推荐）：`AllianceMember` 守卫 + 两步（预约 → 联盟加成员 → effect 确认），期限到了按联盟权威收尾。
   - B：用 `service/directory` 按玩家预约。代价：多一个 Redis 存储与过期语义，不随 Remote 一起迁移恢复。
   - C：允许一人多盟（不做守卫）。代价：产品语义变化。
4. **跨实例的多联盟操作**（2.9）。
   - A（推荐）：同实例一笔事务；跨实例的宣战、外交走两步最终一致；合并这类必须原子的先迁到同一实例。缺省单实例部署下全部是一笔事务。
   - B：要求跨实例也原子。代价：要在服务内再实现跨实例两阶段提交，框架现在没有。
5. **在线通知的可靠性**（2.5 的被踢、入会通知）。
   - A（推荐）：尽力投递，登录时从 Mirror 补齐。
   - B：每个玩家一个可靠收件箱，未读通知持久化、登录时补投。代价：多一个持久结构与清理规则。

## 7. 实施记录

按第 4 节的阶段推进，每个阶段合入 main 时在这里补一行：提交、验收结果、与方案的偏差。

| 阶段 | 状态 | 提交 | 备注 |
| --- | --- | --- | --- |
| 1 框架 | 本地完成，待维护者复核推送 | `f5544d37`（①②）、`6b949c30`（③，`0281f183` 补 Mod 依赖名检查）、`1323a763`（④） | 验收（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 22000）：受影响包 `-race`、全量 `go test ./...`、根包、glsvet、doc anchors 0 broken、`test-remote-generated.sh` 全部 PASS（MirrorLocal 两项按门变量 SKIP）、`test-demo-generated.sh` PASS、`test-remote-matrix.sh` 21 格 PASS。① 守卫按维护者 A 做成“别的服务的 kind”黑名单：`coreremote.Config.ForeignKinds` / `wiring/remoteentity.WithForeignKinds`，不是 2.10 原文的“本服务可写 kind”白名单；只经 ModOption 传入、不加 YAML 键（声明只在实体注解一处，阶段 2 生成）。拒绝点覆盖写准入与 `beginOwnershipTransition` 的全部所有权操作，哨兵 `entity.ErrRemoteKindNotOwned`。② `servicerpc.SlotTable` / `SlotTableConfig` / `CallSlotChecked` / `Redirect` / `Retryable` / `Classify`，新框架码 `errcode.CodeRetryable = 4`、`CodeRedirect = 5`；Remote 写错误到答复的映射放在 `wiring/remoteentity.RPCWriteAnswer`（infra 不能依赖 framework）。`SlotTableConfig` 的 `slots` 是整数、不能用 `shared` tag，跨服务一致性核对留给阶段 2 的 doctor。新建实体时服务端按 `table.OwnerOf` 判断槽位归属留给阶段 3（guide/09 2A 边界）。③ `nest.MirrorSnapshotResolver`，wiring Nest Mod 在没有写 Manager、有 `ModRemoteMirror` 时接上。④ 端到端首跑即绿，未发现缺陷。“恢复完成前不接请求”框架已由 App 启动顺序保证（DataEngine Start 排空 WAL），owner Mod 依赖 `nest` 由阶段 2 生成器声明 |
| 2 生成器 | 进行中 | — | — |
| 3 服务骨架 | 未开始 | — | 依赖 1 |
| 4 demo 迁移 | 未开始 | — | 依赖 2、3 |
| 5 测试与端到端 | 未开始 | — | 依赖 4 |
| 6 文档 | 未开始 | — | — |
