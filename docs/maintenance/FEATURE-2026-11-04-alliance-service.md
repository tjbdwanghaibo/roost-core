# 功能设计：联盟 / 公会服务（2026-11-04）

**状态：方案已定、实施中（维护者 2026-11-04 定方向：“先写方案，按推荐”；同日“未声明归属的 kind 按 A，其他 5 条也按推荐，开始实施”）。同日维护者改写决定 2：分片保留，路由改为“实体加载时在 Redis 登记带过期的路由 + RPC 标为路由 RPC”，不用槽位表（决定一览、2.7）。现行公会行为与它的限制以 [KNOWN-LIMITS“Remote”](KNOWN-LIMITS.md#remote) 为准，各阶段落地情况记在第 7 节。**

登记在 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。基线 `e12e3dc8`（origin/main）；路由改写按 `40a6cacf`（origin/main，阶段 1、2 已合入）核对。codebase-memory 两次都连不上，第 1 节的现状结论全部按源码与 `rg` 核对，引用写到文件与函数。

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
| 2 | 分片与路由 | **维护者 2026-11-04 改写，替换原“槽位表”**。原话要点：“存在分片，可以 service 启动时候就 load entity，entity 在 load 时候注册路由信息到 redis 中，rpc 标记是路由 rpc，底层就会去找，起服 load entity 是 entity 类型的一个策略”；“如果没有规则就不加载，需要 entity 显示加规则”；“新建实体就走 rpc 的随机路由”；“redis 的记录可以使用带 expired time 的”。即：保留多实例，**不在 ID 里编码槽位、不要分片表、业务 RPC 不写 `shard=`**；实体在某实例加载（冷加载或预加载）时把“实体 ID → sid + 所有权代际”登记到 Redis，带 TTL，owner 批量续期，卸载 / 迁走时按值条件删除，kill -9 靠过期；RPC 只标“路由 RPC”，框架按本地缓存 → Redis → 持久所有权记录查找 owner，都没有就答“实体不存在”；Redis 只是提示，写准入仍由所有权记录 + fence 裁决，不对就改投（沿用 ownerroute 的“路由提示 + 持久 fence”分工）；新建实体走随机路由；起服预加载是实体类型的显式策略（`preload=startup`），不声明就只按需冷加载。崩溃接管靠 App 单实例锁 + DataEngine 恢复；迁移用 `TransferRemoteOwnership`，扩容不自动再均衡（2.7） |
| 3 | 生成器 | 现有生成器能声明“带 Nest / DataEngine / Remote 的业务服务”，缺的是“实体归哪个服务写”“路由 RPC 与预加载策略”（2026-11-04 起替换原“按分片路由的 RPC”）“只读 Mirror Mod”三件；做这三件最小增量，不新建一整套“实体服务”生成器 |
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
- 陈旧上限：`remote_entity.cached_max_staleness`，不配时等于 `snapshot_cache_ttl`，缺省 30s（`framework/remoteentity/config.go`、`wiring/remoteentity/config.go`）；game-demo 的 game 进程（联盟的 Mirror 读者）生成为 5s（阶段 5 跟进）。超过上限的 L1 条目先经 L2 或权威重新确认，确认不了就不交出。不覆盖 L2 本身落后于权威（owner 写 L2 失败或结果未知时，最长到 `snapshot_l2_ttl`）。
- 推送：读者登记兴趣后，owner 提交经 outbox 发布到 L2 并按兴趣推送（JetStream 模式才有推送，普通 NATS 只按需读）。兴趣表是广播软状态：每个节点存全集群兴趣，缺省总上限 `snapshot_interest_subs = 262144`，每个读者进程 `snapshot_interest_per_consumer` 缺省为总量 / 16（16384）；超出配额的 key 没有推送，只按需读取。
- 只读进程的装配：`wiring/remoteentity.NewRemoteMirrorMod`（只装 `SnapshotClient`，没有写 Manager、Mongo backend 与分布式锁），业务用 `MirrorSource(registry)` 读。
- 已有测量（2026-10-06，v1.21 时期，macOS 单机私有依赖，[Mirror 第 6 步本机替代](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)）：L1 命中约 0.7µs、L2 回源约 25µs、权威回源约 125µs（p99 约 1µs / 51µs / 270µs）；提交到只读方读到约 30ms；扇出 1 → 100 个读者、一轮 10 个 key 约 1.8 → 25ms；推送中断时退化到陈旧上限回源。之后 Remote 链路有改动，这些数字只作量级参考，不是当前基线。

### 1.4 服务 RPC

- `infra/network/servicerpc/client.go` 的 `BusClient.Call`：能用 `CallTo` 指定 sid；必须在 I/O 段调用（`fctx.BlockingError`），不能在 Nest handler 里同步调。
- `infra/network/servicerpc/affinity.go` 的 `KeyAffinityPicker`：按 key 的哈希对**当前发现到的实例集合**取模。注释写明它不是正确性机制，实例增减时映射跟着变。所以它不能直接当联盟分片表用：映射变了，请求就落到不是 owner 的实例上。
- NATS RPC 客户端在超时与 no responders 时按策略自动重试（`infra/network/nats/driver/rpc.go` 的 `isRetryable`），同一请求可能送达多次；写 RPC 必须按 RequestID 幂等。
- 普通 Bus RPC 不把调用方截止时间传到接收端，接收端要给整次处理自己的预算（[SLG §7.7](../framework/SLG-INTEGRATION.md#77-进阶深化联合链路的接法要点)“跨实体 / 跨服投递”一行）；JetStream RPC 才按调用方截止建立处理期限。
- **随机路由有两条现成路径**（2026-11-04 按 `40a6cacf` 核实）：
  - **队列组**（生成客户端的缺省）：没有路由标记的方法经生成的 `call` 走 `CallChecked(ctx, 0, …)` → `Bus.Call`，发到服务级主题 `<prefix>.rpc.<svc>.<method>`（`codegen/internal/servicerpc/template.go`）。接收端在 `infra/network/bus/bus.go` 注册 handler 时同时订阅这个主题（NATS 队列组 `<svc>_rpc`）和本 sid 的实例主题，NATS 在**已注册 handler 的实例**里任选一个。恢复中、handler 还没注册的实例不在候选里。
  - **服务发现 + picker**：`servicerpc.NewDiscoveredBusClient` + `CallDiscoveredChecked`，经 etcd 发现实例，`PickServer` 用 picker 选 sid 再 `CallTo`，发现与调用共用一份预算；picker 缺省是 `RoundRobinPicker`（按调用序号对候选取模）。生成客户端只有 `affinity=` 方法走这条（`callByAffinity`，picker 是 `KeyAffinityPicker`，没有 key 时回退 `RoundRobinPicker`）。etcd 里登记了、但 handler 还没注册的实例也可能被选中，得到 no responders。
  - 所以“生成客户端用 `CallDiscoveredChecked`”不准确：生成客户端的缺省随机路由是队列组。2.3 推荐创建类 RPC 沿用队列组。
- **阶段 1 已落地的槽位表**（`infra/network/servicerpc/slots.go`）：`SlotTable`、`ParseSlotTable`、`SlotTableConfig`（`slots` / `shards`）、`CallSlotChecked`、`MaxSlots`。按 2026-11-04 路由决定移除（第 4 节 1R）；同一文件里的 `Redirect`、`Retryable`、`RedirectProvider`、`Classify` 与框架码 `CodeRetryable` / `CodeRedirect` 保留，路由 RPC 继续用。

### 1.5 App 单实例锁与接管

`framework/app/singleton.go`：同一服务类型 + sid 同一时刻只有一个进程跑 Mod；在第一个 Mod Init 之前取锁（别人持有就等），按 `singleton.renew_interval`（缺省 3s）续期，失锁经 RuntimeFailure fail-stop；`singleton.ttl` 缺省 15s（`config_schema.go` 的 `singletonConfig`）。`SingletonLiveness` 只读回答“这些 sid 里哪些有进程持锁”。DataEngine WAL 按 sid 分目录（[生成器 §5“按 sid 区分的本地状态”](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)），同 sid 重启从自己的 WAL 恢复。

### 1.6 Saga 与 Remote

- 原生步骤不能改 Remote 实体：`Complete` 遇到带 Remote 写批次的事务返回 `ErrNativeStepRemoteWrite`（`framework/saga/step_receipt.go`），生成器也拒绝 `remote=managed` 的回执宿主（[SAGA.md 原生步骤执行契约第 7 条](../framework/SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定2026-10-28-决定-2a-改为回执在实体里)）。
- 调另一个服务的步骤用 Mongo 步骤，被调服务按 `IdempotencyKey` 幂等。game-demo 送礼的 `deliver`（发邮件，`RequestID = "gift:" + IdempotencyKey`）就是这个形状（`demo/internal/service/game/gift_saga.go.tmpl`）。
- 事务里 `Emit` 会把 durability 升到 strict（`framework/nest/rollback.go` 的 `NewRollbackTx` 注释）；同一条 WAL 记录里的 Remote 写与 effect 在投影时进同一个 Mongo 事务（`framework/dataengine/engine/mongo_projection.go`）。Remote + Emit 有 Nest 单元用例（`framework/nest/remote_commit_test.go`），真实依赖下“Remote 实体 handler 里 EmitStart / Emit”没有端到端用例。
- 容量：原生正向 128 并发约 1050 笔/秒（单机，[SAGA.md“跨服交易”](../framework/SAGA.md#跨服交易使用约束)）。

### 1.7 ownerroute

`infra/network/ownerroute/route.go` 的 `Router`：`Resolver` 给出 owner sid，本地执行或经 `Transport` 单向发送（`BusTransport.Send`，没有回复）。仓库里唯一的使用者是 demo 送礼 saga 的步骤转交。指南的定位是“路由提示不能替代持久 fence 校验”（[guide/14](../framework/guide/14-foundation.md)）。本方案沿用这个分工：Redis 里的路由登记（2.7）只是路由提示，持久 owner 与 fence 才是权威；需要回复的写走 servicerpc，不用 `BusTransport`。

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

### 1.9 路由登记与预加载相关的现状（2026-11-04 补）

- **持久所有权记录**：控制库的 `_remote_entity_meta` 集合（`framework/remoteentity/mongo_committer.go`），`_id` 是完整 Entity ID，字段 `_owner_sid`、`_owner_epoch`（MarkerEpoch，每次所有权变更加一，`mongo_authority.go` 的 `changeOwnership`）、`_owner_route`（RouteEpoch，转移时加一，`TransferExpected`）。`GetRemoteOwnership` 只在写 Manager 上（`framework/remoteentity/ownership.go`），只装 Mirror Mod 的进程现在读不到它。这个集合没有 `_owner_sid` 索引（`EnsureIndexes` 只给 `_remote_entity_transactions` 建了索引）。kind 在 ID 的低 10 位（category 2 位 + kind 8 位，`framework/entity/idgen.go`），按 kind 范围查 `_id` 做不到。
- **Redis**：Remote Mod 已经依赖 Redis（共享模式的分布式锁、L2 快照）。L2 键是 `remote_entity:snapshot:*`，可以加部署前缀 `remote_entity.snapshot_l2_key_prefix`（同一个 Redis 库上的多个部署必须设不同的值，不允许 hash tag，`wiring/remoteentity/config.go`）。按值比较的 Lua 脚本已有先例（`framework/remoteentity/versioned_lock_lua.go`）。只读进程的 Mirror Mod 依赖 `redis`、`mongo`、`syncbus`，两个存储都连得上。
- **起服加载实体：框架没有这个能力**。`framework/dataengine/load.go` 的 `Loader.LoadAll` 是通用资源加载器：按 `LoadTemplate`（库、资源、依赖、过滤、`OnLoad` 回调）分层流式读文档，交给回调处理。正式代码里没有调用者，只有 `load_test.go` / `validate_test.go` 在用。它不按所有权过滤，不把文档放进 Remote Manager 或 Nest 实体生命周期，也不登记路由。Remote 实体现在只在第一次写准入时从权威冷加载（1.2）。
- **Remote 实体不会空闲卸载**：`framework/remoteentity` 里没有按空闲时间卸载的逻辑。已加载的集合只在转移出去、写准入拒绝后卸载或进程退出时缩小。

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
  - **2026-11-04 合入无缝大世界原型后（待维护者确认）**：game 为分区场景的组队 `Team`（`service=game`、共享模式，成员在不同 game 进程上）重新装 `remote_entity`。联盟实体对 game 仍然不可写，保证改由第 2.10 第 1 项的归属守卫给出（`ForeignKinds`：认领与写准入返回 `entity.ErrRemoteKindNotOwned`），而不是 game 没有写运行时；取舍与另一种做法（Team 也归一个实体服务）见 [无缝大世界方案 §14](FEATURE-2026-11-03-seamless-world.md#14-合入-main2026-11-04)。
- 缺省部署一个 alliance 实例：所有联盟在同一进程，任何多联盟操作都能一笔事务。容量不够时才加实例，按 2.7 路由。

### 2.2 实体与数据

| 实体 | 键 | 内容 | 说明 |
| --- | --- | --- | --- |
| `Alliance`（Remote 托管） | 联盟 ID | 名字、盟主、成员表（玩家 → 职位、加入时间、静态原服 sid）、科技、仓库、请求回执 | 成员关系的唯一权威 |
| `AllianceMember`（Remote 托管） | 玩家 ID 派生 | 所在联盟、待定的入会预约、退盟冷却、每日捐献计数、请求回执 | “一人一盟”的守卫与玩家维度的联盟状态，见 2.5；**不是**第二份成员权威，与联盟成员表不一致时以联盟为准 |
| ~~`AllianceShard`（普通本地实体，每实例一个）~~ | ~~实例 sid~~ | ~~联盟定时任务的持久节点~~ | 2026-11-05 删除：唯一用途是预约期限，改在进程内布置（2.5B） |

- **联盟 ID 不编码槽位**（2026-11-04 路由决定）：联盟 ID 由服务内一个 Mongo 计数器发号，沿用 demo `nextGuildID` 的 `$inc` + majority 写确认，发号失败留空号不回收。ID 与哪个实例持有无关。`AllianceMember` 的 ID 由玩家 ID 加 kind 得出。哪个实例持有某个实体只看持久所有权记录，调用方经 2.7 的路由查找找到它。建盟频率低，单个计数器的争用可以忽略。
- **请求回执放在被改的实体里**：`Requests map[RequestID]回执`（结果摘要 + 时间），随业务修改同一事务提交与回滚，同一 RequestID 再来时回放原结果；保留期与上限按 Saga 回执的做法（`saga.receipt_retention` 缺省 168h，每次提交顺手剪过期项）。第一个实施版本放在服务骨架里，等第二个实体服务出现再考虑上升为框架件。
- **成员表放一个文档**：沿用 demo 的整文档读写，人数上限是真实限制（见第 6 节第 1 条）。

### 2.3 写：RPC 契约

**接口形状**（`roost add rpc Alliance -service alliance` 生成，方法按业务增加）：`Found`、`Apply`、`Approve`、`Join`（无审批的直入）、`Leave`、`Kick`、`Transfer`、`Donate`、`ClaimReward`、`Info`（权威读，给 GM 与写后核对）、`Result(target, requestID)`（按 RequestID 查回执）。每个写方法的参数都含 `requestID`、调用者玩家 ID、调用者原服 sid；调用者身份来自 game 进程的已鉴权会话，不来自客户端字段。

**两类方法**（2026-11-04 路由决定）：

- **路由 RPC**（标 `//roost:rpc route`，2.10 第 2 项）：对已有实体的写与权威读，包括 `Apply`、`Approve`、`Join`、`Leave`、`Kick`、`Transfer`、`Donate`、`ClaimReward`、`Info`、`Result`，以及实例之间对 `AllianceMember` 的守卫调用（2.5）。
  - 路由键缺省是方法的 `target` 参数，推荐取**完整 Entity ID**：kind 已经在 ID 里，Redis 键与所有权记录的 `_id` 直接用它，标记不用再写 kind。备选是业务号（unique id）加标记上的 `kind=`，代价是每个方法多一处声明。
  - 客户端查找顺序：① 本进程路由缓存；② Redis 路由登记；③ 读持久所有权记录，有 owner 就发给 owner，owner 收到后冷加载并登记；④ 都没有就答“实体不存在”，请求不发出去。细节见 2.7。
  - 查找结果只是提示，最终由服务端裁决。
- **创建类 RPC**（`Found`）：不标路由，走现成的随机路由。推荐沿用生成客户端缺省的队列组（1.4）：NATS 只在已注册 handler 的实例里选，恢复中的实例不会接到新建请求。备选是 `NewDiscoveredBusClient` + 缺省 `RoundRobinPicker`（`CallDiscoveredChecked`），它要多一次 etcd 发现，还可能选中 handler 未就绪的实例。接到请求的实例新建实体，经首写的 `ClaimOwnership` 认领所有权，再登记路由。
- **随机路由下的创建幂等**：同一个 RequestID 重发时可能落到别的实例，所以回执不能放在新实体上，那时它还不存在。回执放在按稳定 ID 路由的实体上。建盟放在发起者的 `AllianceMember`（2.5），流程如下：
  1. 实例先经路由 RPC 在发起者的 `AllianceMember` 上记下“RequestID → 本次发出的联盟 ID”（同时就是一人一盟的预约）；
  2. 再建联盟。

  重发落到别的实例时会读到这条记录，就沿用同一个联盟 ID 继续：所有权认领是原子 CAS，两个实例并发时只有一个成为 owner，另一个按改投走。所以不会建出第二个联盟。以后别的实体服务的创建类方法也按这条规则：幂等锚在一个按稳定 ID 路由的实体上，或由调用方在路由之前定下新实体 ID。

**服务端前置检查**（路由 RPC 在进 Nest 之前，框架件，与 `RPCWriteAnswer` 放在一处）：

- 实体已在本地加载、所有权是本 sid：直接处理。
- 否则读持久所有权记录：
  - 没有记录：答“实体不存在”。非创建类方法不得进入写准入，否则首写会把一个不存在的 ID 认领走（1.2）。
  - owner 是本 sid：冷加载、登记路由，再处理。
  - owner 是别的 sid：答“改投”（`servicerpc.Redirect`）。
- 进 Nest 之后，写准入照常按所有权记录 + fence 裁决。阶段 1 的 `RPCWriteAnswer` 把 fenced 映射为改投，前置检查只是省掉一次白进 Nest。

**服务端处理**：RPC handler 在 I/O 段，用自己的处理预算（1.4）调生成的 `Sync_*` Sender 进 Nest；handler 在实体锁内先查回执，再按权威判权限与规则，写回执与修改，返回新状态。

**答复分类**（客户端与 game 端点据此决定显示与重试）：

| 答复 | 什么时候 | 调用方怎么做 |
| --- | --- | --- |
| 成功 + 新状态 + 版本 | 提交已确认，或回执回放 | 界面直接用返回值；后续 Mirror 读带这个版本（2.4） |
| 业务拒绝（带错误码） | 权限不足、已满、已在别的联盟、名字非法等；实体不存在（路由查找与服务端前置检查都没有所有权记录） | 显示原因，不重试 |
| 可重试，未执行 | 服务未就绪（启动恢复中、预加载未完成、handler 未注册，表现为 no responders）、所有权转移中、本实例不是 owner（前置检查或写准入发现） | 原 RequestID 重试；“不是 owner”带 owner sid，调用方改投一次；no responders 时调用方重新查找一次路由（2.7） |
| 结果未知 | RPC 超时、`ErrRemotePersistenceIndeterminate`、处理预算到期 | 客户端显示“处理中”；game 端点用原 RequestID 调 `Result` 或原样重发，直到得到成功或拒绝 |

**写后立刻看到**：写 RPC 返回本次提交后的联盟视图（与 Mirror DTO 同一形状）和实体版本 `StateVersion`。需要“写完立刻看到”的界面用返回值，不去读 Mirror。

### 2.4 读：Mirror

- game 进程读两类快照：联盟（成员列表、信息、科技、公告）与自己的 `AllianceMember`（我在哪个盟、职位）。缺省用 `//roost:mirror` 只读 DTO，读实体缺省快照，版本就是实体的 `StateVersion`，与写 RPC 返回的版本可比。`remoteview` 字段投影只在需要按字段裁剪时用；视图版本只在视图字段变化时推进（guide/05 §6），不能拿写 RPC 返回的实体版本去要求它。
- 玩家 handler 里要用联盟数据（如科技加成）时用 `RemoteViewRef` 在 I/O 段预取，handler 内不回源（guide/05 §6 的现行规则）。只读进程要先补上 Nest 的快照来源接线（2.10 第 3 项）。
- **一致性选择**：普通展示用 Cached；刚写完之后用 Monotonic，`after` 取写 RPC 返回的版本，读到的版本不会比自己写的旧；需要权威时调服务的 `Info`，不用 Mirror 的 Linearizable（只读 loader 不一定支持）。
- **新鲜度边界**：推送正常时提交后几十毫秒量级可见（1.3 的历史样本约 30ms）；推送中断、owner 刚重启、读者不在推送配额内时，退化为 `cached_max_staleness` 回源，最长约等于这个值（缺省 30s）；L2 写失败或结果未知时最长到 `snapshot_l2_ttl`。game 进程（读者）的生成配置把 `cached_max_staleness` 写成 5s（阶段 5 跟进；陈旧上限越短，回源越多）。阶段 5 实测（[benchmarks/alliance-20261104](../performance/benchmarks/alliance-20261104/README.md)）：推送正常时回源量正好是“在读的不同 key 数 / 陈旧上限”，与读频率无关（21 个 key、60s：30s 时 42 次 Redis HGET，5s 时 252 次）；5s 时 10 万在线约 2 万次/秒 HGET。game 读者用 5s（阶段 5 跟进已改生成配置）。
- **写权限不依据 Mirror**：端点可以用 Mirror 做便宜的预检（例如“已在联盟”直接提示），但最终判定一律在服务里按权威做，预检与服务结论冲突时以服务为准。
- **容量边界**：兴趣表全集群共用。每个在线玩家读自己的 `AllianceMember` 会占兴趣条目：10 万在线时接近缺省总上限的一半，单个 game 进程超过 16384 个 key 后超出部分不推送、只按需读。阶段 5 按目标在线数测，必要时调 `snapshot_interest_per_consumer`，或让成员快照只在登录与收到通知时读、不长期持有兴趣。

### 2.5 成员关系与“一人一盟”（决定 1）

- **权威**：联盟的成员表。Player 上的 `GuildID` 只是玩家自己的副本（2026-11-05 起持久，见下文），不是权威。
- **一人一盟是跨联盟的约束**：两个联盟可能在不同实例，同一玩家同时被两个联盟批准时，单个联盟的事务挡不住。用 `AllianceMember` 做守卫，两步完成，执行方是处理批准的联盟所在实例：
  1. 在玩家的 `AllianceMember` 上预约“加入联盟 A，RequestID = r”（已在别的联盟或有别的未完成预约就拒绝）；
  2. 在联盟 A 的事务里加成员，写回执 r；同一事务 `Emit` 一个“成员已变化”effect；
  3. effect 消费者在玩家的 `AllianceMember` 上把预约确认成“在联盟 A”。第 2 步被明确拒绝时释放预约。
  预约带期限；期限到了仍未确认，守卫方先向联盟 A 的权威问“玩家是否在成员表里”，按权威确认或释放，不按超时直接释放。两个实体在同一实例时三步合成一笔多实体事务。退盟、踢人反向：先改成员表，再由 effect 清 `AllianceMember`。（2026-11-05 起第 3 步与退盟的清除改为直接调用 + 惰性自愈，不再用 effect，见 2.5A。）
- **跨实例照旧两步，新增的实例间调用一律走路由 RPC**（2026-11-04 路由决定）：`AllianceMember` 按玩家 ID，可能和联盟在不同实例，阶段 3 已实现的“预约 → 确认 → 期限到了按联盟权威收尾”保留。联盟实例对守卫的调用（预约、确认、释放、清除）与守卫方向联盟问权威，都按目标的完整 ID 走路由 RPC（2.3），不再按槽位表找实例。
- **守卫还不存在时**（玩家从没入过盟）：由发起调用的联盟实例在本地新建并认领，入会预约与联盟在同一实例，常见情况就是一笔多实体事务。认领是原子 CAS：两个实例并发新建同一个守卫时只有一个成为 owner，输的一方在写准入被 fence，按改投改走路由 RPC。
- **game 进程怎么知道“我在哪个盟”**（2026-11-05 改，原为读守卫的 Mirror）：守卫不再同步给 game（`AllianceMember` 声明 `snapshots=none`），game 只镜像联盟。“在哪个盟”是玩家自己的副本 `PlayerDao.GuildID`（`persist,sync`）：玩家自己的入会 / 退会 / 建盟成功后在玩家事务里写（`ShowGuild`；建盟在 `guild_op_status` 看到 saga 完成时写），在线时收到成员通知更新，登录时对照该联盟的 Mirror 名单清掉已不在的；**副本为 0 时登录向 alliance 问一次 `Membership`**（2026-11-05 晚，决定 D，见 2.5B），补上离线时被批准的入会。它只用于显示与建议性预检，任何写判断都在 alliance 服务按名单做。
- **附近玩家头顶的公会**：同一个 `PlayerDao.GuildID`（sync）。
- **在线通知**（2026-11-05 晚改，原为 best_effort effect）：写答复之后由 alliance 服务层直接按成员记录里的原服 sid 经 Bus 发给该 game 进程（尽力投递，失败只记日志）；game 进程更新副本并推客户端。见 2.5B。

### 2.5A 守卫是名单的缓存：惰性自愈（2026-11-05）

维护者 2026-11-05 决定：联盟服务里不用可靠 effect 收尾守卫（原第 2、3 步的“成员已变化”effect 与期限的可靠通知），跨实例的守卫收尾也做成自愈。框架层 effect 的 reliable / best_effort 与 entity / none 照做，reliable 仍是缺省（[impl/03](../framework/impl/03-dataengine.md#effect-持久性reliable--best_effort与消费顺序entity--none)）。

**不变量**：守卫是名单（`Alliance.Members`，权威）的缓存，只允许“过时地占着”，绝不允许“过时地空着”。写成式子：玩家 P 在联盟 X 的名单里、座位的 `JoinRequestID` 是 J ⇒ P 的守卫是 joined(X, J) 或 pending(X, J)。反过来不要求：守卫可以占着一个名单里已经没有 P 的联盟（收尾丢了），由下一个碰到它的请求纠正。

**怎么做**（`internal/rpc/alliance` 的 `healGuard`，在服务层的 I/O 段里，不在守卫 handler 里——问权威是 RPC，快池上的 Nest handler 不能等）：

1. 同实例：名单与守卫在一个多实体事务里改，没有可丢的收尾（也不再产生任何可靠 effect）。
2. 跨实例两步成功后服务直接调守卫的 `ResolveGuard` / `ClearGuard`（`finishGuard`，尽力而为；丢了计 `alliance.guard_finish_lost_total`）。
3. Join / Found 的守卫检查（`deferHeld`：守卫被别的联盟或请求占着时 handler 不记回执、答 `GuardHeld`）、`ReserveGuard`、`ClaimReward`（`verifyGuard`）、迁移前的过期预约（`guardPending`）、预约期限的 best_effort 通知（`HandleReservationDue`）碰到守卫被 (X, R) 占着时：
   - 向 X 问 `Settle(X, R, P, decide)`（一个联盟事务）：答 P 现在是否坐在 R 名下（`Seated`）、R 有没有结论（`Found`）。`decide` 在“守卫是 joined”或“预约已过期限”时为真：X 从没定论的 R 记成 `alliance_expired`（P 坐在 R 名下而回执已清理的，记成功），之后同一 R 的准入只回放结论；期限内的预约 `decide` 为假，只问不写——那次入会可能还在路上，截断它会把两个联盟并发批准变成“过期”。X 还没有所有权记录（建盟在途）时，不定论的问话直接答“没有结论”，不去认领。
   - pending(X, R)：有结论或已坐下才 `ResolveGuard(R, joined=Seated)`；都没有就保持占着（照原判断答“已在入会中”）。joined(X, R) 而没坐下：`ClearGuard(X, R)`；坐着就不动。
   - `ResolveGuard` / `ClearGuard` 是条件事务：守卫仍是观察到的 (X, R) 才改。之后照原判断重做一次（`deferHeld=false`）。
4. 预约期限：~~`AllianceShard` 的持久定时器到期发一条 best_effort 的 `alliance.reservation_due`，消费者做第 3 步~~（2026-11-05 晚改为作出预约的实例在进程内布置，到期直接做第 3 步，见 2.5B）；丢了由下一个碰到守卫的请求做。

**为什么清空 / 释放守卫之后不会“过时地空着”**（P 坐在 X 名下 ⇒ 守卫占着 X）：

- 清 joined(X, R) 或释放 pending(X, R) 的前提是：在 X 的一个事务里读到“P 不坐在 R 名下”，且 R 在 X 上已有结论（deciding Settle 当场记下，或 X 早就定论；不定论的问话只在 R 已有回执时才释放）。有结论之后，X 上任何以 R 为请求号的准入都只回放这个结论，不会再让 P 坐到 R 名下。
- P 要坐到 X 的另一个请求 R' 名下：同实例入会要在同一个事务里看到守卫空着；跨实例入会要守卫为 R' 预约着。守卫一直是 (X, R) 到条件事务改它为止，这两条都不可能，所以“读到没坐下”到“条件事务清守卫”之间 P 不会坐进 X。
- 条件事务只在守卫仍是 (X, R) 时生效：两个实例并发自愈同一个守卫，后到的看到守卫已变（或已空、或被新的预约占着）就什么也不改；自愈与直接收尾并发同理。并发 Join：都要经过守卫（同实例事务或预约），守卫上的事务按实体锁串行，自愈只把 (X, R) 换成空，不会让两个入会同时看到空守卫后都成功——看到空守卫的那一个在自己的事务里占住它，另一个再看到的是它。
- 唯一假设（自愈之前就有）：回执清理（`alliance.receipt_retention`）之后不再用同一个请求号重试。

回归（`internal/rpc/alliance`）：`TestGuardHealsAfterALostCrossInstanceClear`（修前：`join after a lost clear: … player is already in an alliance or joining one`）、`TestGuardHealsALostReservationResolution`（坐下了确认丢失：下一次 Join 经 `Settle` 确认守卫，修前 `guard … PendingAllianceID:1 … want confirmed in 1`；没坐下：期限内的 Join 被拒、预约不被截断，过期限后 Join 经 `Settle` 定论为过期、释放并加入别的联盟，迟到的准入答 `alliance_expired`）、`TestGuardHealingNeverLeavesASeatedPlayerUnguarded`（两实例并发入退盟、三分之一的直接收尾被丢：一人至多一盟、在名单里则守卫占着该联盟；变异“自愈无条件清 joined”时红：`player 300 is a member of [1 2]`）、`TestOnePlayerOneAllianceAcrossInstances`（期限内不定论的约束：曾因自愈截断在途入会红过一次，答 `alliance_expired`）、`TestSameInstanceMembershipRecordsNoReliableEffect`（修前：`same-instance found / join / leave recorded reliable effects [alliance.member_changed ×3], want none`）；e2e 的一人一盟断言改为“在名单里 ⇒ 守卫占着该联盟”立即判错 + settle 窗口内收敛。

### 2.5B 联盟不用 effect（2026-11-05 晚）

维护者 2026-11-05 决定：**alliance 相关逻辑全部不用 effect**，reliable 与 best_effort 都不用（框架保留这两种模式给其他服务，[impl/03](../framework/impl/03-dataengine.md#effect-持久性reliable--best_effort与消费顺序entity--none) 不变）。原来还在用的三条与它们的替代：

| 原 effect | 原用途 | 现在 | 丢失时的兜底 |
| --- | --- | --- | --- |
| `alliance.member_changed`（best_effort，admit / remove / found / establish） | 成员通知 | 写的 handler 在答复里带上本次改动（`Answer.Member`：玩家、原服 sid、joined / left / kicked，只在这次真的执行且成功时有，回放没有）；服务层在 `Sync_` 调用返回之后、答复调用方之前 `notify`：一次 Bus publish（`SendByType`，不等 game），失败计 `alliance.member_notice_lost_total` 并记日志 | 在线：下一条通知；离线：登录补齐（下文 D） |
| `alliance.reservation_due`（best_effort，`AllianceShard` 定时器） | 预约期限到了按联盟权威收尾 | 作出预约的实例（守卫的 owner）在 `ReserveGuard` 答复后、守卫仍为本请求预约着时，在**进程内**布置期限（`followUps`，`time.AfterFunc`）；到期读守卫，仍是这条预约就 `healGuard`；联盟暂不可达则按 5s 起倍增、最多 1 分钟重试；`ResolveGuard` 后守卫不再持有时撤掉 | 期限不持久：随进程丢失（崩溃、重启）的，由下一个碰到守卫的请求收尾——Join / Found / ReserveGuard / ClaimReward、登录的 `Membership`、transfer；正确性本来就由 2.5A 的自愈保证，期限只是主动收尾的优化 |
| `alliance.war_declared`（reliable） | 跨实例宣战时对方一侧 | 见下文“宣战” | 持久标记 + 运行中重试 + 启动扫描 |

对应删除：三个 effect 定义、三个消费者与它们的 durable / 队列组名（`alliance-member-notice`、`alliance-reservation-heal`、`alliance-war-declared`），生成器 `demo_alliance.go` 里的三条 `add effect` 与 `AllianceShard` 的实体 / DAO / 定时器 / 生命周期，`internal/service/alliance` 不再订阅任何 effect。项目未部署，不迁移旧 durable：升级时旧 JetStream 消费者与 `alliance_shard` 集合直接不再使用，需要时由运维删除。

**服务层的后续工作（`followUps`，`internal/rpc/alliance/background.go`）**：写答复之后、要等时钟或重试的工作（预约期限、宣战的再次确认、启动扫描）在服务进程内各自的 goroutine 里跑，不在 Nest 池上；同一个 key 只布一次。停机按三步：`Service.Stop(ctx)` 关准入、撤掉全部已布置的、取消在跑的上下文并在 ctx 内等它们结束；alliance 进程的 `app.Service.Shutdown` 调它——App 先关全部输入，再等 Shutdown，最后才停 Nest 与 Bus。这里的东西都不持久，也都不是正确性的前提（各自的兜底见上表与下文）。

**为什么预约期限放进程内，而不是“守卫自己的计时器”或“按玩家哈希到多个 shard”**（决定 F 一并解决）：

- 守卫是 Remote 托管实体，框架拒绝在它上面挂持久定时器（`codegen/internal/entity/timer_host.go`：所有权在进程间迁移时期限的语义没有定义），“守卫自己的持久计时器”不是现有能力。
- 按玩家哈希到 N 个本地 shard：每次预约 / 确认仍是“守卫 + 一个 shard”的多实体事务（多一个本地 WAL 写），同一 shard 上的入会照样串行（只是 N 条线），还要 N 的配置、shard 的建立与迁移规则，并且定时器在 shard 锁里触发后仍需“AfterCommit → 交给服务层”的桥。换来的只是“崩溃后期限还会准点再触发”。
- 正确性不依赖期限（2.5A：守卫只许过时地占着，碰到就自愈），所以持久化期限只买到“崩溃后不用等下一次碰到”。进程内期限让每个守卫写都只锁守卫本身：不同玩家的预约 / 确认之间没有任何共享实体。
- 如果以后要“崩溃后也准点”，再按第二种做（或给框架补 Remote 实体的期限能力），不要回到“每实例一个 shard”。

**宣战（跨实例，不用 effect，敌方 `Wars` 最终一定补上）**：

1. 宣战方 `handlerAllianceDeclareWarDetached` 在同一事务里记 `Wars[enemy]`、回执和**待对方确认**的持久标记 `WarAcks[requestID] = enemyID`（联盟 DAO 字段，`war_acks`）。答复带 `WarPending`（新执行或回放时标记仍在）。
2. 答复之后服务层立即（`followUps`，不拖慢宣战的答复）调敌方 owner 的 `AcknowledgeWar(requestID, enemy, declarer)`（路由 RPC）。
3. 对方给出**最终答复**（记下了、或按规则拒绝——拒绝时宣战方保留自己一侧，沿用第 6 节第 4 条 A 的“两步最终一致、不原子”）后，宣战方用条件事务 `handlerAllianceWarAcknowledged` 按请求号删掉标记（已删则什么也不改）。
4. 答复不是最终（对方实例不可达、未执行、结果未知）或删标记失败：标记还在，由下面的驱动再问。

持久的重试驱动选的是“运行中重试 + 启动扫描”，不是“下次触达该联盟时惰性补发”——后者对一个没人再碰的联盟没有上界：

- **运行中**：宣战方 owner 进程按 5s 起倍增、最多 1 分钟重试，直到对方给出最终答复；
- **重启后**：联盟是 `preload=startup`，owner 起服时全部加载；alliance Mod 在注册 handler 之后跑一次 `ScanWars`（列出本实例拥有的联盟，逐个读 `WarAcks`），对每个未清的标记再问一次。扫描在后台（`followUps`），不挡起服。代价（2026-11-06 实测，[基准页](../performance/benchmarks/alliance-warscan-20261106/README.md)）：每次起服每个联盟一次 Nest 读，而这个读是 Remote 写准入、要向所有权记录取一次写许可（一次 Mongo `findAndModify`，只读 handler 也一样，见 [KNOWN-LIMITS](KNOWN-LIMITS.md) 对应条），外加每 1 000 个联盟一页所有权列表查询；原来逐个读，1 万个联盟约 2 分 50 s 后最后一个标记才被补问，改为每页 8 个并发读（`scanConcurrency`）后 1 万 26.5 s、5 万 2 分 4 s；
- **重发宣战**：同一请求号的 `DeclareWar` 回放时标记仍在，也会再问；
- **不迁移**：有未清标记的联盟 `alliance.transfer` 不搬（`TransferReport.Pending`），保证一直在问的就是持有标记的那个 owner；守卫不再因为有预约而不搬（它的期限在进程内、经路由 RPC 收尾，在哪个实例都能收）。

最坏延迟：对方可达时是一次 RPC；对方实例不可达时，是它恢复后最多一个重试间隔（≤ 1 分钟）；宣战方 owner 崩溃时，是它同 sid 拉起、预加载完成之后的一次扫描。前提：标记所在的 sid 最终会再起来（App 单实例锁保证同一 sid 只有一个进程）。

**确认迟到 / 重复无害**：`AcknowledgeWar` 在敌方联盟上按请求号记回执，同一请求号再来只回放（`Replayed`），不会重复记 `Wars` 也不会改结论；回执清理（`alliance.receipt_retention`，缺省 168h）之后才到的重复确认会执行 `CheckWar`，因为已在交战而被拒、只写一条拒绝回执，同样不改状态（demo 没有“停战”操作；以后加停战时，停战与迟到确认的先后要另行约束）。删标记是按请求号的条件事务，重试、回放、扫描并发收尾同一条标记只删一次。同实例宣战仍是一笔两边事务，没有标记。

**登录补齐（决定 D）**：新增路由 RPC `Membership(target=GuardTarget(player))`：读守卫；守卫被占着就先 `healGuard`（名单里已没有的 joined 清掉、名单已安排座位的预约确认、过了期限的预约定论），再读一次，答 `State.AllianceID`（0 = 不在任何联盟；还在途的预约算 0）。从没入过盟的玩家没有守卫，路由检查答“实体不存在”。game 的 `EnterGame` 在 `refreshGuild` 里：副本不为 0 照旧对照 Mirror；副本为 0 时调 `Membership`（`guildLookupTimeout` = 1s 上界，在玩家接入的请求 goroutine 上，不在 Nest 池），拿到联盟号就在玩家事务里 `ShowGuild` 写回；失败（不可达、超时）副本保持 0、记 Warn，登录照常。代价：副本为 0 的玩家每次登录一次路由查找（多数是从没入过盟的玩家：调用方查路由缓存 → Redis → 所有权记录点读后答“不存在”，请求不发出）。

**不变量（去 effect 之后不变）**：一人至多一盟；守卫是名单的缓存，只许过时地占着、绝不过时地空着（2.5A 的论证不依赖 effect，也不依赖期限）；成员通知、期限、宣战再确认都在写**答复之后**由服务层做，没有任何一项在 Nest handler 里做 I/O。“玩家在名单里但守卫空着”的交错：所有清空 / 释放守卫的写仍只来自 `ResolveGuard` / `ClearGuard` 的条件事务与同实例事务，与 2.5A 相同，本次没有新增清空守卫的路径（期限到期走的仍是 `healGuard`）。

回归（`internal/rpc/alliance` 与 `game/controllers/player`）：

- `TestAllianceWritesRecordNoEffect`：两个实例上建盟、同 / 跨实例入退、批准、踢人、同 / 跨实例宣战、预约过期限，任何实例记下 effect（reliable 或 best_effort）即红；同时要求通知按顺序直接发出、敌方一侧与过期预约在没有 effect 的情况下收尾。修前（基线 4998e9d1 的生成工程，只加一个空的 `runFollowUps` 桩）：`instance 3000: alliance writes recorded effects [alliance.war_declared (reliable) alliance.member_changed (best_effort) ×6]`、`instance 3001: … [alliance.member_changed (best_effort) ×3]`、`the enemy's side of the cross-instance war was not recorded without an effect: b's wars []`、`the reservation past its deadline was not settled without an effect: guard {… PendingRequestID:reserve-205 …}`、`membership notices sent straight from the service: got []`。
- `TestStartupWarScanReadsAlliancesConcurrently`（2026-11-06）：假 Remote 挡住每个 Alliance 写准入，同时在等的个数须达到 `scanConcurrency`（8）且不超过。修前（只在测试侧补常量）：`the start-up scan had 1 alliance reads waiting for their write grant at once (at most 1), want 8`。真实进程的量见 `TestAllianceE2EWarScanStartup`（`ALLIANCE_E2E_PERF=1`）。
- `TestLostWarAcknowledgmentIsMadeUp`：第一次确认丢失后运行中重试补上；丢失后宣战方“重启”（已布置的全部丢掉）由 `ScanWars` 补上；重复宣战与重复确认是回放、敌方只记一次；有等待的联盟不迁移。修前不适用（基线靠可靠 effect）；变异验证：`ScanWars` 改成空函数时红（`after the restart scan: e's wars [], a's waits [{war-a-e 3}]`），去掉运行中重试时红（`after the retry: b's wars [], a's waits [{war-a-b 2}]`）。
- `TestReservationsOfDifferentPlayersDoNotWaitOnEachOther`（决定 F，确定性：用写准入闸门把 301 的预约卡在它的事务里、锁已持有，302 的预约必须在此期间完成）。修前：`302's reservation waited for 301's: remote.1: server error`（基线的 `handlerGuardReserve` 是 `[守卫, AllianceShard]` 多实体事务，302 排在 shard 锁后 2s 超时）。
- `TestReservationDeadlineIsSettledByTheAlliance`（改为进程内期限：到期前不跑、联盟不可达时不释放并重布、可达后释放、迟到的批准答 `alliance_expired`、已坐下的确认；期限随“重启”丢失后由下一次批准收尾）、`TestTransferOwnershipMovesWhatThisInstanceOwns`（原 `…KeepsReservedGuards`：有预约的守卫现在照常搬，搬走后旧实例上的期限仍经路由收尾）。
- `TestMembershipAnswersTheRostersAlliance`：成员答联盟号、名单里已没有的 joined 被清并答 0、已坐下而确认丢失的预约被确认、没有守卫答“实体不存在”。
- `TestLoginLooksUpAGuildJoinedWhileOffline`（game 端点）：副本为 0 时问一次（带上界）并写回；没有守卫、预约在途、服务不可达都保持 0；副本不为 0 不问。修前：`a guild joined while offline: 0 lookups, last call "handlerPlayerGuild" []`。
- 机器人场景 `guild_offline_join`（2026-11-06）：机器人以自己的身份经建盟 Saga 建开放公会，第二个身份登录一次后断开；运维命令 `gm.player.join_guild`（game 进程，按玩家代发 `Join`）在 game 仍有该玩家会话时不入会、答 `joined=false`，机器人重试它——“已离线”由这一步保证，不靠等待；再登录后不带公会号读“我的公会”须是建盟者的公会且含本人。e2e `TestAllianceE2ERobotOfflineJoin` 在真实进程上跑 4 个机器人，`make dev-smoke` 跑第二轮。变异：`refreshGuild` 副本为 0 时不调 `lookupGuild`，4 轮全红（`my guild answers response code 100018: guild not found`），恢复后 3 轮全绿。
- e2e：`TestAllianceE2EGamePlayers` 加 ①b（E 下线期间被拉进公会，再登录后“我的公会”直接可读）；`TestAllianceE2EOneAllianceUnderRace` 的收敛检查改为前半个窗口只等进程内期限，之后用 `Membership` 模拟“下一个碰到守卫的请求”，并记下需要碰的个数（kill -9 守卫方时期限随进程丢失）。

**实测**（同机与 `4998e9d1` 交替 3 轮，[基准页“联盟不用 effect”](../performance/benchmarks/alliance-join-1000-20261105/README.md#联盟不用-effect2026-11-05-晚)）：同实例 `Join` / `Leave` 每次的 Mongo 命令数不变（这条路径修前已不写 effect）；跨实例 `Join` 少两次 shard 文档写，64 并发成功 9.3～11.2 → 29.0～32.8 次/秒、p50 4.1～4.7 s → 0.34～0.38 s，16 并发 p50 1.49～1.61 s → 0.26～0.28 s。修后跨实例仍有成批 3 s 超时、同一实体随后约 30 s 写不进：已定位为 Remote outbox 代发不通知写者进程（框架，未修，[KNOWN-LIMITS](KNOWN-LIMITS.md) 对应条给了 A / B / C），修前跨实例 `Leave` 偶发的 3 s 超时按同一原因理解。

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
| `nextGuildID`（game 里的 Mongo 计数器） | 移到服务，一个计数器发号（不按槽位，2.2） | 服务内 |
| 错误码 `guild_*` | 保留；增加“服务忙 / 改投 / 结果未知”三类 | RPC 答复 |
| 响应里的 `ServedBySID` | 改为处理请求的 alliance 实例 sid | RPC |
| loadtest / Robot `found_guild`、`guild_info` | 增加两个 game 进程上的玩家加入同一公会 | 验收 |
| `sidmerge` 的 Remote 托管集合 | 公会集合归 alliance 服务，合服仍不搬 | 不变 |
| mail `GuildScope`、聊天组频道、场景关系视野的注释 | 成员来源改为 Mirror / 成员变化通知 | 注释 |

### 2.7 分片、接管与迁移（决定 2）

**2026-11-04 起的做法**（维护者改写决定 2，原槽位表做法见第 3 节）：多个实例分担实体，但某个实体归哪个实例，只由 Remote 的持久所有权记录决定。Redis 里另登记一份带过期的路由提示，让调用方大多数时候不用读 Mongo。不在 ID 里编码槽位，没有分片配置，业务 RPC 不写 `shard=`。

**哪些 kind 登记路由**：声明了 `service=` 的 Remote 托管 kind。这份列表生成器已经有（阶段 2 的 `ForeignKinds` 来自同一次扫描），经 ModOption 交给本服务的 Remote Mod，不加 YAML 键。

**路由登记（Redis，推荐值）**：

| 项 | 推荐 | 理由 |
| --- | --- | --- |
| 键 | `<部署前缀>:remote_entity:route:<完整 Entity ID>`。部署前缀沿用 `remote_entity.snapshot_l2_key_prefix`，不加 hash tag | 每个实体一个键，脚本只碰一个键，Redis Cluster 下按键自然分散。共用一个 Redis 的部署本来就必须设不同的前缀（1.9），不新增配置键 |
| 值 | `<owner sid>:<MarkerEpoch>` | MarkerEpoch 是所有权记录里每次所有权变更都加一的代际（1.9），比较它就知道两条登记谁新。同 sid 崩溃重启时代际不变，登记值也不变 |
| TTL | 30s | kill -9 后过期前的登记仍指向同一个 sid，同 sid 拉起后它本来就是对的；真正错的登记只来自“迁走后旧 owner 没删掉”，而那时新 owner 的登记带更新的代际会覆盖它。所以 TTL 不需要很短，主要用来回收没人续的键 |
| 续期 | owner 每 10s 对本实例已加载、所有权是本 sid 的登记 kind 实体批量续期，用 pipeline，每批不超过 1000 个键 | TTL 内能续 3 次，偶发一次失败不会掉登记 |
| 登记脚本 | 实体在本实例加载完成后执行（冷加载、预加载、新建认领后都算）：键不存在、或已存代际不大于自己的代际，才 `SET … PX`；已存的代际更新就不写，记一次指标 | 迟到的旧 owner 登记盖不掉新 owner |
| 续期脚本 | 值和自己的完全相同才 `PEXPIRE`，否则返回“不同”或“不存在” | 续期不重建键：续期批次在转移之前拼好、转移之后才执行时，不会把已删掉的旧登记写回去。返回“不存在”（Redis 丢键，或曾长时间连不上）时，先确认本地所有权仍是本 sid，再走登记脚本；返回“不同”时不再续这个键 |
| 删除脚本 | 值相同才 `DEL`；在卸载、转移出去（进入 Draining 后、Fenced 时）、进程正常停机时执行 | 只删自己写的，旧进程删不掉新 owner 的登记 |
| kill -9 | 不删，靠 TTL 过期 | 见 TTL 一行 |

三个脚本沿用 Remote 分布式锁按值比较的 Lua 写法（1.9）。

**续期量估算**：

- 每个实例每 10s 续一遍自己已加载的登记 kind 实体。一万个联盟是每秒 1000 次 `PEXPIRE`，每批 1000 个键时约每秒 1 次 pipeline 往返。
- `AllianceMember` 按需加载，Remote 实体又不会空闲卸载（1.9），加载集合会随“做过入会 / 退盟的玩家”一直增长。再按 5 万个估：合计每秒 6000 次、约 6 次 pipeline 往返，对单个 Redis 是小负载。
- 以上是估算。阶段 5 实测：续期量就是已登记实体数——预加载 1000 / 10000 个联盟后每 10s 续 1000 / 10000 次（每批不超过 1000 个键一次 pipeline），与估算一致；守卫按需加载后同样每个每 10s 一次，长时间运行后的守卫加载量没有测。如果 `AllianceMember` 的加载量远大于这个数，再评估守卫的空闲卸载或更长的 TTL。

**续期或登记失败**：只记日志和指标，不影响写准入，因为准入看的是所有权记录。登记缺失期间，调用方回退去读所有权记录，每次多一次 Mongo 点读。

**调用方查找与缓存**（框架件，game 进程与 alliance 实例共用）：

- **顺序**：本地缓存 → Redis `GET` → 按 `_id` 点读 `_remote_entity_meta` → 都没有就答“实体不存在”。读到 owner 就发给它，owner 冷加载并登记。只读进程没有写 Manager，需要 Mirror Mod 新增一个只读的所有权查询（1.9）。alliance 实例直接用写 Manager 的 `GetRemoteOwnership`。
- **缓存**：按实体 ID 存 (sid, MarkerEpoch)，有效期 30s（与 Redis TTL 相同），条数有上限（推荐 65536，LRU）。缓存只是提示，正确性靠改投，所以有效期长一点只是偶尔多一跳。
  - 收到改投答复：把缓存改成改投目标，在同一份预算内改投一次。
  - 收到 no responders（NATS 客户端已按策略重试过，1.4）：删掉这条缓存，重新查一次 Redis / 所有权记录。查到不同的 sid 才在同一份预算内再发一次，否则按“可重试，未执行”返回。缩容后旧缓存指向已经下线的 sid，靠这一条纠正。
- **负缓存**：推荐不做“实体不存在”的负缓存，要做也不超过 1s。刚建好的实体会被别的进程立刻访问（建盟后马上有人入会），负缓存会让它们在有效期内看不见这个实体；而查一个不存在的实体本来只多一次 Mongo 点读。

**起服预加载（实体类型的显式策略）**：

- **声明**：实体注解 `preload=startup`（2.10 第 2 项）。没有声明就不预加载，只按需冷加载。
- **范围**：所有权记录的 owner 是本 sid 的该 kind 实体。框架现在没有这个能力（1.9），要新增：
  - 给 `_remote_entity_meta` 加 `{_owner_sid: 1}` 索引，按 `_owner_sid` 查；kind 用 ID 低位按位过滤，不改元数据文档格式。
  - Remote Manager 增加“只加载、不写”的入口，批量冷加载，限制并发。
  - 加载完成后批量登记路由。
- **时机**：在 DataEngine 恢复与 Nest 启动之后、RPC owner Mod 在总线上注册 handler 之前。App 按依赖顺序 Start，预加载做成 Remote Mod 的启动步骤，或一个单独的 Mod，让路由 RPC 的 owner Mod 依赖它。预加载完成前的请求得到 no responders（“可重试，未执行”），和恢复期间一样。
- **失败**：
  - 查询失败：启动失败，由进程管理拉起重试，与 DataEngine 恢复失败的处理一致。
  - 单个实体加载失败：记错误和指标，跳过它。这个实体留给按需冷加载，到时同样会报错并答给调用方，不让一个坏文档挡住整个服务。
- **用在哪**：数量可控、需要常驻的类型。联盟要靠独占模式心跳推进活动阶段时，实体必须已经加载，所以 `Alliance` 声明预加载。`AllianceMember`（每个玩家一个）不预加载。

**新建实体**：走创建类 RPC 的随机路由（2.3）。接到的实例新建实体、认领、登记。新实例一加入服务就进了随机路由的候选集合，所以扩容后新建的实体自然会分到新实例。

**崩溃接管**（kill -9、宿主机故障）：

1. 同一个 sid 由进程管理拉起（同机重启，或换机后挂上同一块 WAL 卷；与 game sid 同一个运维模型，[PLAYER-MIGRATION](../framework/PLAYER-MIGRATION.md#2-步骤)）。
2. 新进程在第一个 Mod Init 前等单实例锁：旧进程崩溃时最多等 `singleton.ttl`（缺省 15s）；旧进程只是卡住时，新进程要等锁过期才启动，旧进程按续期期限判定失锁并 fail-stop（Nest 经 OnFail 围栏），两代不同时写（单实例锁的保证，1.5）。
3. DataEngine 从本 sid 的 WAL 恢复未投影的记录，Remote outbox 补发未发布的快照；RPC 的 owner Mod 依赖 Nest（2.10 第 4 项），总线 handler 在这之后才注册，之前的请求得到 no responders。
4. 所有权记录仍指向这个 sid，不需要转移。
5. 声明了预加载的类型在启动时重新加载并登记（代际不变，登记值和崩溃前一样）。没有预加载的类型，第一次请求经旧登记（未过期时仍指向同一个 sid）或回退读所有权记录找到本实例，冷加载并登记。

**接管窗口内写请求的答复**：

- 进程不在时，NATS 回 no responders：未投递，属于“可重试，未执行”。
- 崩溃那一刻在途的请求超时：属于“结果未知”，用原 RequestID 重查。
- 新进程恢复或预加载期间 handler 还没注册：同样是 no responders，“可重试，未执行”。

game 端点把 no responders 显示为“服务忙，请稍后”，把超时显示为“处理中”。只读不受影响：Mirror 从 L2 继续读，陈旧上限内读到崩溃前最后发布的版本。

**迁移（运维命令）**：

1. 在当前 owner 上执行运维命令，把指定的实体（ID 列表，或“本实例持有的 N 个”）转给目标 sid。命令逐个调用 `TransferRemoteOwnership(id, to)`，每个先排空已准入的写。只有当前 owner 能发起转移，所以命令跑在旧实例里。枚举“本实例持有的”与预加载用同一个按 `_owner_sid` 的查询。
2. 转移期间，这个实体的写答“可重试”。转完后旧 owner 按值条件删除自己的登记；之后发到旧 owner 的请求经前置检查答“改投”。
3. 新 owner 不预先加载。第一次请求经改投或回退读所有权记录到达，新 owner 冷加载并登记；新代际会盖掉任何迟到的旧登记。转移命令完成后通知新 owner 立即加载（对预加载类型有用）是可选项，推荐先不做。
4. 联盟的定时节点与有未决预约的守卫随迁移一起处理（2.8；阶段 3 已经把有未决预约的守卫报为 pending，过期后重跑）。

迁移窗口内，调用方的缓存或 Redis 仍指向旧 owner 时会多一跳改投；旧 owner 已经退出时得到 no responders，调用方重新查找。

**扩容**：新实例加入后只承接新建实体（随机路由）。推荐**不自动再均衡**已有实体：自动再均衡要决定迁哪些、什么时候迁，每次迁移还有秒级不可写窗口。需要时用上面的运维命令按需转。代价是老实例的负载要等运维迁走，或随联盟解散自然下降。

**缩容**：

1. 先让要下线的实例不再接新建。推荐让它退出创建类方法的队列组，实例主题照常接路由 RPC；这要求 bus 提供按方法退出队列组的入口，目前没有，列在第 4 节 1R。备选：创建类方法答改投到另一个实例，生成客户端的创建调用也处理一次改投。
2. 用同一条运维命令把它持有的全部实体转走。
3. 确认持有数为 0 后再停。

不转走就停掉的实例，它的实体只能等同一个 sid 被拉起。

迁移只转移活着的 owner；死掉的 owner 只能按“崩溃接管”同 sid 拉起，不能由别的 sid 接走（`TransferRemoteOwnership` 要求本地是 owner）。登记过期后，调用方回退读到的仍是这个死掉的 sid，请求一直得到 no responders（“可重试，未执行”），直到它被拉起。

### 2.8 定时逻辑、仓库、排行

- **定时**：Remote 托管实体不能挂 `roost add timer`（1.8）。~~每个实例一个普通本地实体 `AllianceShard`（按 sid 归属，用现有实体定时器），节点记“哪个联盟、什么时候、做什么”；到期回调用异步 Sender 给对应联盟发 Nest 消息。~~ 2026-11-05 起联盟服务没有持久定时器：唯一用到的预约期限改在进程内（2.5B），`AllianceShard` 删除。以后真有需要持久、准点的联盟定时任务时，再按“到期 → AfterCommit 把 I/O 交给服务层”的方式加回一个本地实体，并按联盟分散，不要回到“每实例一个、写路径都锁它”。联盟在线期间的周期推进（例如活动阶段）可以用独占模式下的心跳。用运维命令迁移联盟时（2.7），旧实例把这些联盟的节点删掉，新实例在冷加载后补建（规则在骨架里写死）。Remote 实体原生持久定时器（D11 方案 C）不在本方案范围。
- **仓库**：联盟 DAO 的字段，写走 RPC；存入 / 取出涉及 Player 时按 2.6 走 Saga。
- **排行**：联盟内排行（贡献）在联盟 DAO 里算；跨联盟排行用 `service/rank`，由联盟事务 `Emit` 的 effect 推进，不在锁内调 RPC。

### 2.9 多联盟事务

- 同一实例上的多个联盟：一笔多实体 Nest 事务（Remote 原子批次，1.2）。宣战、外交、合并在缺省单实例部署下都是这种。
- 分到不同实例时：用两步（发起方记意图 → 对方受理 → 发起方确认），每步带 RequestID，最终一致。需要原子的操作（合并）先用运维命令把参与的联盟迁到同一实例（2.7），再一笔事务。宣战的两步（2026-11-05 晚起不用 effect）：发起方同一事务记 `Wars` 与持久的“待对方确认”（`WarAcks`），答复后服务层直接调对方的 `AcknowledgeWar`，对方最终答复后条件事务清标记；细节与最终一致的论证见 2.5B。
- 是否要求跨实例的强一致外交，是产品选择，见第 6 节第 4 条。

### 2.10 生成器：最小增量（决定 3）

现有生成器已经能做：声明 alliance 业务服务及其 mods、多 sid 实例、`roost add rpc` 的服务与调用方、Remote 托管实体与 DAO、`//roost:mirror` DTO、Player 原生步骤 Saga。只补下面四件，不新建一整套“实体服务”生成器：

1. **实体归属服务**：实体标记加 `service=<name>`（或 `roost.yaml` 里 `services.<name>.entities`），只声明一次。生成器据此只在该服务注册这个 kind 的 Nest handler 与 lifecycle；运行期 Remote Manager 拿到“本服务可写的 kind”列表，对不在列表里的 kind 拒绝认领与写许可（不靠调用方自觉，防止别的服务的进程把新实体认领走，1.2）。doctor 检查“声明了归属的 kind 被别的服务的 Sender 调用”。
   - **未声明归属的 kind 保持现行为**（维护者 2026-11-04 选 A）：任何带 Nest 的服务都能写，只有声明了 `service=` 的 kind 受守卫约束；Player 的静态 sid 归属、跨 Base Team 的共享模式、SLG 联盟夹具都不受影响。声明位置用实体注解 `service=<name>`，不在 `roost.yaml` 再写一份。
2. **路由 RPC 标记与预加载注解**（2026-11-04 路由决定，替换原 `//roost:rpc shard=<param>` 与槽位配置）：
   - `//roost:rpc route`：方法级标记。路由键缺省是名为 `target` 的 `int64` 参数（完整 Entity ID，2.3），用别的参数时写 `route=<param>`。生成的客户端经框架的路由查找（2.7）`CallTo` owner，处理一次改投，以及 no responders 后的一次重新查找；生成的 handler 先做服务端前置检查（2.3）。不标的方法照旧走队列组随机路由，创建类方法就属于这一类。不需要任何分片配置键，doctor 也不再核对表。
   - 实体注解 `preload=startup`：生成器把这些 kind 经 ModOption 交给本服务的 Remote Mod 作为预加载列表（与 `WithForeignKinds` 同一种传法，不加 YAML 键）。只能写在 `remote=managed` 且声明了 `service=` 的实体上，否则生成期报错。没写就不预加载。
   - 登记路由的 kind 列表（声明了 `service=` 的 Remote 托管 kind）由同一次扫描交给 Remote Mod（2.7）。
   - 阶段 2 已落地、按新决定移除：`shard=` 的解析与校验、生成客户端的 `callBySlot` 与 `NewBusClient` 的 `*servicerpc.SlotTable` 参数、ClientMod 嵌入的 `servicerpc.SlotTableConfig`（`<service_type>.slots` / `.shards` 两个配置键）、doctor 的 `rpc-slots:<rpc>`（`codegen/internal/roost/slot_tables.go`）。响应里的 `RedirectSid` 与生成 handler 把改投 owner 写进响应的逻辑保留。
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
| 槽位表：ID 编码槽位 + 部署配置的“槽位区间 → sid”表（原决定 2，阶段 1 ②、阶段 2 ② 已实现） | 维护者 2026-11-04 改为按实体在 Redis 登记路由（2.7），放弃的原因：① 槽位数编码在 ID 里，上线后不能改；② 区间表要在所有调用方和服务实例之间保持一致，滚动更新期间靠改投兜底，doctor 也只能核对配置文件；③ 扩缩容以槽位区间为单位迁移，粒度粗，新实例要等运维分配槽位才能接新建；④ 新建实体前要先判断槽位归属（`table.OwnerOf`），否则表过期的实例会把别的槽位的新实体认领走（guide/09 §2A 的边界），守卫与联盟按各自 ID 取模、常常分到不同实例。改成按实体登记后，ID 与部署无关，新实例直接承接新建，迁移粒度是单个实体，没有要同步的部署配置。代价：多一份 Redis 软状态和续期流量；登记缺失时第一次访问多一次 Mongo 点读；起服预加载要新增框架能力 |
| 用 `KeyAffinityPicker` 当路由 | 映射随发现到的实例集合变化，实例重启或扩容时请求落到非 owner 上（1.4） |
| `saga.coordinators` 式的“sid 列表取模”分配 | 增减一个实例会让大部分实体换 owner；按实体登记只移动运维指定的实体 |
| 扩容后自动再均衡已有实体 | 要决定迁哪些、什么时候迁，每次迁移有秒级不可写窗口；扩容后新实例先承接新建，已有实体用运维命令按需迁（2.7） |
| 路由登记不带过期、只靠 owner 显式删除 | kill -9 的 owner 删不掉自己的登记，换 sid 下线的实例会留下永久的错误提示；带 TTL 的登记在没人续期时自己消失（维护者“redis 的记录可以使用带 expired time 的”） |
| 用 `service/directory` 预约做“一人一盟” | 预约在 Redis、带过期，与 Mongo 里的成员权威是两个存储；`AllianceMember` 守卫在同一套 Remote 链路里，可以和联盟一起迁移、一起恢复 |
| 新建完整的“实体服务”生成器（一条命令出全部骨架） | 现有命令已覆盖大部分，缺的只是 2.10 的四件；整套生成器要维护第二份服务模板 |
| 服务内用 Durability memory（直写 Mongo、不要 WAL 卷） | 带 effect / Saga 启动的事务仍会升到 strict（1.6），WAL 卷省不掉；与 game sid 用同一个“按 sid 的 WAL 卷”运维模型更简单。是否对纯联盟写单独用 memory，留到阶段 5 量完延迟再定（阶段 5 只量了 strict：`Join` 单并发 p50 86～115ms，memory 没有对照，仍按 strict） |

## 4. 实施拆分

估时按一名熟悉仓库的实施者计，含测试与文档，不含等待评审。2026-11-04 路由决定之后重排：阶段 1、2 已合入 main，其中的槽位表部分由 1R、2R 移除；阶段 3 的本地分支按旧槽位方案实现，改造后再推送。

| 阶段 | 内容 | 验收 | 估时 |
| --- | --- | --- | --- |
| 1 框架 | **已合入 main**（第 7 节）：① Remote Manager 的外来 kind 守卫；② `servicerpc` 改投 / 可重试答复、四类判定，以及槽位表（槽位表由 1R 移除）；③ wiring Nest Mod 在装了只读 Mirror Mod 时接上快照来源；④ Remote 实体 handler 里 `Emit` 的真实依赖用例 | — | 已完成 |
| 2 生成器 | **已合入 main**（第 7 节）：① 实体 `service=`、② `//roost:rpc shard=`（由 2R 移除）、③ `remote_mirror` Mod、④ `add rpc -entity-service` | — | 已完成 |
| 1R 框架：路由改造 | ① **Redis 路由登记**：登记 / 续期 / 删除三个按值比较的脚本；Remote Manager 在加载完成、新建认领、转移出去、卸载、正常停机时调用；按批 pipeline 的续期循环；登记 kind 列表的 ModOption（`framework/remoteentity`、`wiring/remoteentity`）。② **路由查找**：`servicerpc` 定义查找接口与 `CallRoutedChecked`（缓存、改投一次、no responders 后重新查找一次），实现放在 Remote 一侧（缓存 → Redis → 所有权记录）；Mirror Mod 提供只读的所有权查询。③ **服务端前置检查**：放在 `wiring/remoteentity`，与 `RPCWriteAnswer` 同处，非创建类方法不让不存在的实体进入写准入。④ **起服预加载**：`_remote_entity_meta` 的 `{_owner_sid: 1}` 索引、按 owner 与 kind 枚举、Remote Manager 的“只加载不写”入口、预加载步骤排在 owner Mod 注册 handler 之前、预加载 kind 的 ModOption。⑤ **移除槽位表**：`SlotTable`、`ParseSlotTable`、`SlotTableConfig`、`CallSlotChecked`、`MaxSlots`。保留 `Redirect` / `Retryable` / `RedirectProvider` / `Classify`、框架码、`RPCWriteAnswer`、实体归属守卫、Mirror 接 Nest。guide/09 §2A 改写为“路由 RPC 与改投”。CHANGELOG `[Unreleased]` 里槽位表与 `shard=` 两条没有发过版，直接改写，不写“移除”。⑥ **缩容入口**：bus 按方法退出队列组 | 每项先红后绿，红要落在承诺上：迟到的旧 owner 删除或续期不影响新 owner 的登记；kill -9 后登记按 TTL 过期；Redis 不可用时路由回退所有权记录、写准入不受影响；非创建类请求不会认领不存在的实体；预加载只加载 owner 是本 sid 的实体，完成前不接请求；no responders 后改投到新 owner。受影响包 `-race`，push 前全量 `GOWORK=off go test ./...`；真实依赖用 `scripts/mirror-local.sh`，脚本在 Redis Cluster 上也跑一遍 | 4～6 天 |
| 2R 生成器：路由改造 | `//roost:rpc route` / `route=<param>`（生成客户端走 `CallRoutedChecked`，生成 handler 调前置检查）；实体注解 `preload=startup` 及其校验；登记 kind 与预加载 kind 的 ModOption 接线；移除 `shard=`、`callBySlot`、ClientMod 的槽位配置、doctor `rpc-slots:<rpc>`；`add rpc -entity-service` 骨架的注释改成“`target` 是完整 Entity ID，写方法标 `route`”；PROJECT_GENERATOR §6 改写 | 生成器包测试；临时生成工程（game + 两个 alliance 实例）build / vet / test，`go generate ./...` 后 porcelain 干净，`roost id check`、`roost generate --check` 通过；`test-genres` / `test-remote` / `test-demo-generated` 通过 | 2～3 天 |
| 3 服务骨架（改造本地分支 `feat/alliance-p3-service`，改完再推送） | 去掉槽位编码 ID（`alliancerule.AllianceUniqueID`、按槽位的计数器改为一个计数器）；运维命令 `alliance.transfer_slots` 改为 `alliance.transfer`（ID 列表或数量，跑在 owner 上，保留“有未决预约的守卫报 pending”）；`Peers` 的按槽位 BusClient 改为路由 RPC；Found 前的 `table.OwnerOf` 判断改为随机路由 + 发起者 `AllianceMember` 上的 RequestID 记录（2.3）；“表说归我才服务”改用框架前置检查；`Result(allianceID)` / `GuardResult(playerID)` 可合并为按完整 ID 的 `Result(target)`；`Alliance` 声明 `preload=startup`；配置去掉 `alliance.slots` / `.shards` | 原 12 个用例按新路由重跑，保留每条的红证据；新增两条：创建请求重发落到另一个实例时不建第二个联盟；守卫并发新建只有一个 owner | 2～3 天 |
| 4 demo 迁移 | 2.6 的对应表全部落地；game 去掉 `remote_entity`、装只读 Mirror；dev-run 加两个 alliance 实例；Robot / loadtest 场景；注释改正 | 重新生成 game-demo，build / vet / test、`roost id check`、`roost generate --check` 干净；`make dev-smoke` 通过 | 3～4 天 |
| 5 测试与端到端 | 新脚本（或扩展 `scripts/test-demo-generated.sh`）在真实依赖上跑：**两个 game 进程上的玩家加入同一公会**并互相读到；**实例扩容**：1 个实例扩到 2 个后连续建盟，新联盟分布到两个实例，已有联盟不动；**运维迁移**：把一批联盟转到新实例，迁移中持续写不丢、不重复，持有旧缓存或旧登记的调用方经改投成功；**alliance 实例 kill -9**：窗口内请求只得到“可重试 / 结果未知”，原 RequestID 重查后结论唯一、无重复入会；不拉起时它的登记在 TTL 后过期，同 sid 拉起后预加载类型重新登记，未预加载类型由第一个请求冷加载并登记；**Redis 停掉期间**写照常（回退所有权记录），恢复后续期重建登记；**Saga 扣钱失败回滚**：建盟被拒后金币退回，Player 扣款步骤后 kill -9 game 进程，恢复后只扣一次、退一次；**一人一盟**：两个实例上的联盟并发批准同一玩家，只成功一个。另做短测（每轮不超过 30 分钟）：单实例写 RPC 延迟、Mirror 回源量、续期量与查找回退率 | 全部 `-race`；证据进 `docs/performance/benchmarks/<label>/`；Linux 与跨机不在本轮验收 | 5～7 天 |
| 6 文档 | guide/05、guide/09 增加“实体服务”一节（含路由登记与预加载）；REMOTE_ENTITY 写路由登记的契约；demo WALKTHROUGH；KNOWN-LIMITS 删旧条目、登记剩余边界；CHANGELOG；功能版发版前写 `docs/release/vX.Y.0-NOTES.md`（存储格式变化按未上线规则：不兼容，需清空公会数据） | `scripts/check-doc-anchors.py` 0 broken；根包 `GOWORK=off go test -count=1 .` | 1～2 天 |

剩余合计约 17～25 天（1R 4～6、2R 2～3、3 改造 2～3、4 3～4、5 5～7、6 1～2）。1R、2R 可以并行，前提是先定下 `CallRoutedChecked`、前置检查与两个 ModOption 的接口；3 依赖 1R、2R；4 依赖 3；5 依赖 4。阶段 3 的本地分支改造完成、维护者复核之前不推送。

## 5. 风险与未验证项

- **Remote 实体 handler 里 `EmitStart` / `Emit`**：阶段 1 第 ④ 项已补真实依赖端到端（`remoteflow` 的 `TestGeneratedRemoteHandlerEmitsEffectsWithTheRemoteWrite`，2026-11-04 macOS 私有依赖通过）：effect 与 Remote 写同一条提交记录投影、各发布一次，失败不提交。本用例只覆盖单进程 strict 与 memory 升 strict，不含 Remote handler 提交后 kill -9、WAL 重放时 effect 的去重（阶段 5 的 Saga 用例覆盖）。
- **写 RPC 延迟**：唯一的 Remote 写样本是 strict p50 / p99 约 117 / 195 ms（1.2），加上 RPC 一跳与 Mongo `GrantWrite`。公会操作可以接受。阶段 5 实测（带路由缓存、单并发，本机有后台负载）：`Join` p50 86～115ms、p99 131～235ms，`Info` p50 9～17ms；同一实例上同一方法串行（KNOWN-LIMITS“实体服务的 RPC 在一个实例上按方法串行”），并发只增加排队。同日已修：总线按顺序键派发，不同联盟并行、同一联盟按到达顺序（[benchmarks/rpc-dispatch-20261104](../performance/benchmarks/rpc-dispatch-20261104/README.md)）。
- **Mirror 数字是历史值**：1.3 的读延迟与推送延迟来自 2026-10-06（v1.21 时期），之后 Remote 链路改过；阶段 5 只做短测，不做长稳。
- **视图版本与单调读**：写 RPC 返回的是实体版本；`remoteview` 视图的水位只在视图字段变化时推进。缺省用读实体缺省快照的 `//roost:mirror` DTO 避开这个问题；若业务改用字段视图做写后读，要先核对 Monotonic 对视图 scope 的判定（阶段 5 只验证了 DTO 路径：写答复版本作 `MinVersion` 后两个 game 进程都读到；字段视图没有核对）。
- **兴趣表容量**：每个在线玩家一个 `AllianceMember` 兴趣时，大规模在线会接近缺省上限（2.4），没有测。
- **成员表文档大小**：整文档读写，人数上限越大每笔写越重；上千人联盟没有测（第 6 节第 1 条）。
- **一人一盟的两步**：期限、权威收尾与 effect 重投的组合是本方案新写的协议，阶段 3 用单元测试、阶段 5 用并发批准与 kill -9 验证。阶段 5 已在真实进程上验证（`TestAllianceE2EOneAllianceUnderRace`，五组各 24 人全部恰好一个成功、没有残留预约），覆盖范围见 §7 阶段 5 行。
- **Redis 不可用时的退化**：登记与续期失败只影响路由效率。缓存未命中的请求回退读所有权记录，每次多一次 Mongo 点读，延迟上升；写准入不受影响。Redis 恢复后，续期返回“不存在”会触发重新登记。阶段 5 实测：空路由缓存的调用方每次多 1 次 Mongo 点读，p50 14ms → 125ms（多出的是 go-redis 拨号重试）。**这条只对短于单实例锁失锁期限的断开成立**：单实例锁连同一个 Redis，断开约 12s 后实例 fail-stop（App 级“失锁即停”，同 sid 拉起后恢复），见 KNOWN-LIMITS。
- **路由抖动**：迁移、续期失败、kill -9 后登记过期再重新登记，这些期间同一个实体的请求可能多一跳改投，或多一次重新查找。改投只跟一次；改投目标也在转移中时答“可重试”。高频写的联盟在迁移时仍有秒级不可写。
- **预加载的起服时间与内存**：两者随“owner 是本 sid 的该 kind 实体数”线性增长。按每个联盟文档约 20KB 估，一万个约 200MB，起服要从 Mongo 读同样的量。阶段 5 用演示联盟（只有几个成员）实测：1000 / 10000 个预加载 1.7 / 13.7s，常驻内存 65 / 123 MiB；大联盟没有测。这个策略只给数量可控的类型用，`AllianceMember` 不预加载。
- **续期流量与加载集合只增不减**：续期量是“已加载的登记 kind 实体数 / 10s”。`AllianceMember` 按需加载，Remote 实体又没有空闲卸载（1.9），长时间运行后加载集合只增不减（2.7 的估算）。
- **随机路由下的创建幂等**：建盟的回执锚在发起者的 `AllianceMember` 上（2.3），这是本方案新写的协议，阶段 3 改造时补用例。
- **死掉的 owner**：不拉起同 sid，它的实体就转不走。登记过期后调用方仍从所有权记录查到它，请求一直是“可重试”，这与原方案相同。
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
   - A（推荐）：`AllianceMember` 守卫 + 两步（预约 → 联盟加成员 → effect 确认），期限到了按联盟权威收尾。（2026-11-05 起确认改为直接调用 + 惰性自愈，不用 effect：2.5A、2.5B。）
   - B：用 `service/directory` 按玩家预约。代价：多一个 Redis 存储与过期语义，不随 Remote 一起迁移恢复。
   - C：允许一人多盟（不做守卫）。代价：产品语义变化。
4. **跨实例的多联盟操作**（2.9）。
   - A（推荐）：同实例一笔事务；跨实例的宣战、外交走两步最终一致；合并这类必须原子的先迁到同一实例。缺省单实例部署下全部是一笔事务。
   - B：要求跨实例也原子。代价：要在服务内再实现跨实例两阶段提交，框架现在没有。
5. **在线通知的可靠性**（2.5 的被踢、入会通知）。
   - A（推荐）：尽力投递，登录时从 Mirror 补齐。（2026-11-05 晚：通知由服务层直接发；离线时的入会在登录时经 `Membership` 补齐，2.5B。）
   - B：每个玩家一个可靠收件箱，未读通知持久化、登录时补投。代价：多一个持久结构与清理规则。

## 7. 实施记录

按第 4 节的阶段推进，每个阶段合入 main 时在这里补一行：提交、验收结果、与方案的偏差。

| 阶段 | 状态 | 提交 | 备注 |
| --- | --- | --- | --- |
| 1 框架 | 已合入 main；②里的槽位表按 2026-11-04 路由决定由第 4 节 1R 移除 | `f5544d37`（①②）、`6b949c30`（③，`0281f183` 补 Mod 依赖名检查）、`1323a763`（④） | 验收（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 22000）：受影响包 `-race`、全量 `go test ./...`、根包、glsvet、doc anchors 0 broken、`test-remote-generated.sh` 全部 PASS（MirrorLocal 两项按门变量 SKIP）、`test-demo-generated.sh` PASS、`test-remote-matrix.sh` 21 格 PASS。① 守卫按维护者 A 做成“别的服务的 kind”黑名单：`coreremote.Config.ForeignKinds` / `wiring/remoteentity.WithForeignKinds`，不是 2.10 原文的“本服务可写 kind”白名单；只经 ModOption 传入、不加 YAML 键（声明只在实体注解一处，阶段 2 生成）。拒绝点覆盖写准入与 `beginOwnershipTransition` 的全部所有权操作，哨兵 `entity.ErrRemoteKindNotOwned`。② `servicerpc.SlotTable` / `SlotTableConfig` / `CallSlotChecked` / `Redirect` / `Retryable` / `Classify`，新框架码 `errcode.CodeRetryable = 4`、`CodeRedirect = 5`；Remote 写错误到答复的映射放在 `wiring/remoteentity.RPCWriteAnswer`（infra 不能依赖 framework）。`SlotTableConfig` 的 `slots` 是整数、不能用 `shared` tag，跨服务一致性核对留给阶段 2 的 doctor。新建实体时服务端按 `table.OwnerOf` 判断槽位归属留给阶段 3（guide/09 2A 边界）。③ `nest.MirrorSnapshotResolver`，wiring Nest Mod 在没有写 Manager、有 `ModRemoteMirror` 时接上。④ 端到端首跑即绿，未发现缺陷。“恢复完成前不接请求”框架已由 App 启动顺序保证（DataEngine Start 排空 WAL），owner Mod 依赖 `nest` 由阶段 2 生成器声明 |
| 2 生成器 | 已合入 main；② `shard=` 按 2026-11-04 路由决定由第 4 节 2R 移除 | `c1ff3ade`（① service=）、`3e791441`（③ remote_mirror）、`57eaa127`（④ -entity-service）、`28f93073`（② shard= 解析）、`89255aef`（接阶段 1）、`9a94d338`（CLI 白名单与 rpc 文件头修复） | 验收（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 38000）：gofmt、build、vet、根包、全量 `go test ./...`、doc anchors 0 broken、`test-sync-modes` / `test-genres`（13 类 passed）/ `test-remote` / `test-mmorpg` / `test-demo-generated` 全部通过；新生成工程（game + alliance、Remote 托管 Guild `service=alliance`、`add rpc -entity-service` + `shard=`）build / vet / test、`go generate ./...` 后 porcelain 干净、`roost id check`、`roost generate --check` 通过。偏差：① 归属 handler 由生成的 `NewOwnedNestHandlersMod` 在 Provide 时注册到引擎实例（不登记 hotcode），`ForeignKinds` 只含 Remote 托管 kind；② `shard=` 参数是编码槽位的业务号（unique id），一个服务不混用 `shard=` 与 `affinity=`，槽位表一致性由 doctor `rpc-slots:<rpc>` 按配置文件核对（slots 是整数用不了 shared tag），生成器不自动往配置里写表；④ 请求回执的实体字段按 §2.2 留给阶段 3，不由 `-entity-service` 生成；答复码用阶段 1 的框架码，工程只分配 `ErrRequestInvalid`。顺带修复 `roost generate` 与 `//go:generate` 互改 rpc 文件头（既有缺陷） |
| 路由决定变更 | 方案已改写 | 本行所在的文档提交 | 2026-11-04 维护者改写决定 2：分片保留，不用槽位表；路由改为实体加载时在 Redis 登记带过期的“实体 ID → sid + 代际”，RPC 标为路由 RPC，由框架按缓存 → Redis → 所有权记录查找，新建实体走随机路由，起服预加载是实体类型的显式策略（`preload=startup`）。改动：决定一览第 2 行、1.4（核实随机路由：生成客户端缺省是队列组，不是 `CallDiscoveredChecked`）、新增 1.9、2.2、2.3、2.5、2.7、2.10 第 2 项、第 3 节、第 4 节（新增 1R / 2R，阶段 3 改为改造）、第 5 节。guide/09 §2A、PROJECT_GENERATOR §6 与 CHANGELOG `[Unreleased]` 仍按槽位表描述，在 1R / 2R 实施时随代码改 |
| 1R 框架：路由改造 | 本地分支 `feat/alliance-1r-framework`，未推送，待维护者复核 | `0b62a11e`（②⑤⑥）、`dc4b83b7`（①②③④）、本行所在的文档提交 | 验收（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 26000）：每项先红后绿，修前失败文本记在两个代码提交的说明里（修前 = 未实现承诺的桩；Lua 的红在真实 Redis 单机与 3 主 3 从 Cluster 上各跑一遍）。受影响包 `-race`（servicerpc、bus、redis、remoteentity、wiring/remoteentity、wiring/mods、wiring/nest、codegen/servicerpc）与真实依赖 `-race`（路由脚本、TTL 过期、按 owner 枚举、两个 Manager 之间转移与 kill -9、bus 退出队列组）通过；全量 `GOWORK=off go test ./...` 只有根包两个文档门禁（文档未同步时）失败，同步后根包通过；glsvet 无输出；doc anchors 0 broken；`go generate ./...` 后 porcelain 干净；`test-remote-generated.sh` PASS（MirrorLocal 三项按门变量 SKIP）、`test-demo-generated.sh` PASS、`test-remote-matrix.sh` 21 格 PASS。对外 API：① `coreremote.Config.RouteKinds` / `PreloadKinds`，`wiring/remoteentity.WithRouteKinds` / `WithPreloadKinds`（Init 校验：已注册的 Remote 托管 kind、不在 `WithForeignKinds` 里）；`coreremote.RouteStore`（键 `<snapshot_l2_key_prefix>:remote_entity:route:<ID>`，值 `<sid>:<MarkerEpoch>`，三个按值比较的 Lua，代际按十进制字符串比较）、Manager 上的登记 / 续期循环 / 删除（全部在实体写门下）；`infra/storage/redis.IPipeline.Eval`（续期与停机删除按批 pipeline，Cluster 上按键路由）。② `servicerpc.RouteResolver`（`LookupRoute(ctx, entityID) (int32, error)`）、`servicerpc.EntityNotFound`、`(*BusClient).CallRoutedChecked`（缓存在每个 `BusClient` 里）；Remote 一侧 `coreremote.RouteLookup` + `wiring/remoteentity.RouteSource(registry)`（能力 `mods.ModRemoteRoute`，owner 进程由 `RemoteEntityMod`、只读进程由装了 mongo 的 `RemoteMirrorMod` 经 `coreremote.MongoOwnershipReader` 提供）。③ `wiring/remoteentity.RPCRouteCheck(ctx, owners, localSID, id) error`（框架一步是 `Manager.ServeRoutedRequest`）。④ `{_owner_sid: 1}` 索引（`EnsureRemoteStorage`）、`MongoCommitter.ListOwnedRemoteEntities`（kind 用 `_id` 对 1024 取模过滤）、`Manager.PreloadOwnedEntities`、`wiring/remoteentity.NewRemotePreloadMod()`（`mods.ModRemoteEntityPreload`）。⑤ 槽位表全部删除；codegen 只删引用：`//roost:rpc shard=` 生成期报“已移除，改用 //roost:rpc route”，模板里 `callBySlot`、`SlotTable` 参数、ClientMod 的 `SlotTableConfig`、`RedirectSid` / `RedirectSID()` / `redirectSID(err)` 随 `.Shard` 一起删除，doctor `rpc-slots` 删除。⑥ `(*bus.Bus).LeaveRPCQueue(method)` / `bus.RPCQueueLeaver`。偏差：查找缓存放在 `BusClient` 里（每个客户端一份），查找接口本身不缓存；预加载做成单独的 Mod（`remote_entity` 排在 Nest 之前，不能在它的 Start 里加载），预加载 kind 仍经 `RemoteEntityMod` 的 ModOption 传入，声明了却没装预加载 Mod 时配置检查报错；登记失败的实体仍记在登记表里，由续期的“不存在”分支重登；转移失败恢复原模式时不立即补登，由下一次写准入或前置检查重登；`LeaveRPCQueue` 只支持轻量 RPC、退出后不能重新加入；CHANGELOG 里阶段 2 的 `shard=` 一条留给 2R 改写成 `route` |
| 2R 生成器 | 本地分支 `feat/alliance-2r-codegen` 已完成（rebase 到 1R `f240ae75`），未推送，待维护者复核 | `6f32321f`（独立部分）、`641b341b`（路由是方法级策略）、`018d164d`（接 1R）、`4fe4812c`（骨架 Result 的 `code` 与封套 `Code` 同名）、本行所在的提交 | `//roost:rpc route` / `route=<param>`：参数须 `int64`，同一方法不与 `affinity=` 同标；路由是方法级策略（维护者 2026-11-04），一个服务可以同时有 route、affinity 与不标的方法，标了 route 的方法要求拥有者托管 Remote 托管 kind，不标 route 的 rpc（例如 mail）不受影响。`-entity-service` 不记进 `roost.yaml`，所以按拥有者的实体判断，与第 4 节“仅 -entity-service 或拥有者声明了 Remote 托管实体”相比只保留后一条。`preload=startup` 只能写在 `remote=managed` 且有 `service=` 的实体上；`ownership_gen.go` 的 `RouteKinds` / `PreloadKinds`；bootstrap 给归属服务的 remote_entity 加 `WithRouteKinds` / `WithPreloadKinds`，有预加载 kind 的装 `NewRemotePreloadMod()`。生成的 `BusClient` 带 `servicerpc.RouteResolver`，route 方法走 `CallRoutedChecked`，构造参数按服务用到的策略；ClientMod 经 `RouteSource` 取查找。服务端前置检查做成包内 `RouteChecker` 接口（`RegisterHandlers` 要求服务实现 `CheckRoute`，骨架调 `RPCRouteCheck`），没有让生成的 transport 直接 import wiring。`-entity-service` 的 owner Mod 依赖 remote_entity，经 `OptionalDependsOn` 排在 `ModRemoteEntityPreload` 之后，Start 时 `SetOwnership`。**行为变化**：`-entity-service` 要求拥有者已托管 Remote 实体 kind。调用 route 方法的服务缺 remote_entity / remote_mirror、owner Mod 不排在预加载之后时 `roost generate` 拒绝；只跑 `go generate` 不做这些工程级检查。`shard=` 报已移除，删 `callBySlot`、槽位表参数、ClientMod 槽位配置、doctor `rpc-slots`。`LeaveRPCQueue` 生成器不暴露（缩容由业务按需调）。顺带修复阶段 2 遗留缺陷：骨架 `Result` 的结果 `code` 与响应封套 `Code` 同名，msgpack 报 `already has field=Code`；现已生成期拒绝这类同名，骨架结果改名为 `outcome`。验收：`go test ./codegen/...`、临时生成工程（game + alliance）build / vet / test、`go generate` 后 porcelain 干净、`id check`、`generate --check`、doctor；同一工程的真实进程端到端（mirror-local）：一个 App 里装真实 owner Mod 与预加载 Mod，3000 个 owner=3000 的 Guild，预加载 167～379ms 内 31～69 次请求全部 no responders，预加载完成后约 2～4ms 才首次得到答复（owner Mod 在预加载完成后约 25µs 启动）。两个实例 3000 / 3001：查找答旧 owner 3000，经改投到 3001 拿到 3001 的答复，停掉 3001 后同一请求 no responders。`test-genres-generated.py`（13 类全部 passed）、`test-remote` / `test-demo` / `test-sync-modes` / `test-mmorpg-generated.sh` 通过（mirror-local 专项与 Redis Cluster 子用例按脚本设计跳过）；全量 `go vet ./...` 通过；全量 `go test ./...` 只有 `scripts/perf/sync-aoi` 的 `TestHeartbeatPatrolReachesClientsAndTheAOI` 在全量负载下跳拍失败（inflight 5），单独 `-count=3` 通过，本分支没有改到它；根包、`check-doc-anchors`（211 个文件）通过；仓库 `go generate ./...` 后 porcelain 干净 |
| 3 服务骨架 | 已合入 main（按路由决定改造后，维护者复核时把本地的槽位表实现与改造压成一个提交，保证每个提交可编译） | `ddb56fbc`（服务骨架，含路由 RPC 改造）、本行所在的文档提交；生成器修正 `b26d4341`、`cc57105f` | **结构**（game-demo 模板，步骤在 `codegen/internal/roost/demo_alliance.go`）：业务服务 `alliance` 独占 `Alliance`（`preload=startup`）、`AllianceMember`（按需冷加载）（Remote 托管、`service=alliance`）与每实例一个的本地实体 `AllianceShard`（预约期限的定时器宿主）；规则在 `game/alliancerule`（回执规则、答复形状）与 `roster_component.go` / `guard_component.go`，Nest handler 在 `game/handler/alliance_*.go`，RPC 在 `internal/rpc/alliance`，进程侧（配置声明、三个 effect 消费者、运维命令）在 `internal/service/alliance`。**RPC 标记**：`//roost:rpc route`（键是完整 Entity ID `target`，`AllianceTarget` / `GuardTarget` 给出；其余 ID 都是业务号）——Apply、Approve、Reject、Join、Leave、Kick、Transfer、Donate、DeclareWar、Info、Guard、Result，以及实例之间的 Establish、ReserveGuard、ResolveGuard、ClearGuard、AcknowledgeWar、Settle；不标的只有 Found（队列组随机路由）。生成的 handler 先调 `Service.CheckRoute`（`RPCRouteCheck`）；实例之间的调用一律经生成的路由 `BusClient`（`RouteSource`），发给自己也走总线。**发号**：一个 Mongo 计数器（`_alliance_id_sequence` / `alliance`，`$inc` + majority，动态半区），没有槽位、没有路由配置键。**建盟幂等**（§2.3）：回执锚在发起者的 AllianceMember。守卫归本实例或还没有所有权记录时，一笔多实体事务建新联盟（两者都由本实例认领，联盟也记同一 RequestID）；守卫在别的实例（或并发认领输了）时，ReserveGuard 把 RequestID 绑到一个联盟 ID（重放给出第一次绑定的），再 Establish 该联盟：没有所有权记录就在本实例建并认领，有就经路由 RPC 到 owner，回执重放。守卫上的成功回执一律经 Establish 收尾，所以“只预约、没建成”的第一次投递也由重发补完。**一人一盟**：同实例（按所有权记录：本实例持有或还不存在）一笔多实体事务；守卫不存在时由调用方实例本地新建、写准入认领，原子 CAS 只留一个 owner，输的一方被 fence、什么都没提交，改走两步（预约 → 加成员 → effect 确认；期限到守卫方按联盟 `Settle` 收尾，联盟不可达时 effect 重投、不按时钟释放）。期限到时联盟 ID 若从没被建（建盟实例预约后停了），`Settle` 在本实例建一个空联盟并记 alliance_expired，迟到的同一请求被拒。退盟 / 踢人：守卫归本实例时同一事务清，否则由 effect 经路由 RPC 清（守卫不存在时不新建）。宣战：对方没有所有权记录答 alliance_missing（预检，不记回执）。**运维**：`alliance.transfer`（`{ids: [完整实体 ID], to_sid}` 或 `{count, to_sid}`，跑在 owner 上，逐个 `TransferRemoteOwnership`，有未决预约的守卫报 pending，别人的实体报 failed）；`alliance.info` / `alliance.guard` 经路由 RPC，在哪个实例上跑都到 owner。**验收**（2026-11-04 macOS）：生成工程 `internal/rpc/alliance` 15 个用例 `-race`（`-count=10` 稳定）：原 13 个按路由重写，加 2 个新用例；多个实例在一个测试进程里，真实 Nest、生成的 sender / 分发、handler 与规则，加上生成的 `BusClient` 与生成的 handler（前置检查、改投、msgpack 编解码）经替身总线，整个集群一张所有权表（首写原子认领、别人 fence）。红落在承诺上（每条把对应机制去掉一行再跑，原文抄在 `ddb56fbc` 的提交说明里）：回执查找去掉→`replayed apply … Replayed:false … want the recorded success replayed`；盟主检查去掉→`a member approves: err = <nil> (outcome ok)`；前置检查改成 `return nil`→不存在的联盟答 `remote.1: server error (outcome unknown), want … entity_not_found`，过期路由的实例 `entered Nest for an alliance it does not own: 1 write admissions`；人数上限去掉→`join a full alliance: err = <nil>`；`max:"500"` 去掉→`501 loaded: err=<nil>`；同实例 / 跨实例守卫检查去掉→`player 200 / 201: both approvals succeeded`；期限按时钟释放→`reservation settled without the alliance`；同实例宣战走两步→`war recorded on one side only: a=[2] b=[]`；effect 不确认对方→`the enemy side was not recorded from the effect`；答复映射去掉→`reloading: outcome unknown … want retryable`；pending 检查去掉→守卫 105 被转走；**新增①** Found 用本次发的号而不用守卫绑定的→`retry on 3001: reply {Alliance:{AllianceID:2 … LeaderID:300} Replayed:false ServedBySID:3001}, want alliance 1 replayed`；**新增②** 守卫并发新建（两个实例的写准入在认领处对齐）、输的一方不改走两步→`player 500: approval 1 answered remote.4: not executed … (outcome retryable), want success or alliance_membership`。重新生成的 game-demo：build、vet、`go test ./...`、`go generate ./...` 后 porcelain 干净、`roost id check`、`roost generate --check`、`roost config check --all`、`project doctor` 全 OK（`shutdown:alliance` 80s / 85s：多了预加载 Mod）；`go test ./codegen/...` 通过。**真实两实例冒烟**（私有依赖 `mirror-local.sh` 偏移 30000 + 自起 etcd，生成的 game-demo 二进制起 `alliance --sid 3000/3001`，调用方用 `RouteLookup`（Redis 登记 → `_remote_entity_meta`）+ 生成的 `BusClient`）：Found 经队列组两边都落（3 / 3），每个联盟归接到它的实例、Redis 登记 `<sid>:1`、TTL 约 30s；同一 RequestID 的 Found 重发 6 次都回放同一个联盟；Join 由联盟 owner 答复；固定答错 sid 的调用方经前置检查改投到 owner；守卫在 3000、联盟在 3001 的入会两步经真实 effect 收尾；kill -9 3001 后它的登记仍在、请求答可重试，30s 后登记过期、请求仍可重试；同 sid 拉起后预加载重新登记联盟（值不变；日志里预加载 listed 5 / loaded 5，完成后约 2.6ms owner Mod 才启动），守卫的登记在第一个请求冷加载后才出现。第一轮在高负载下 3000 起服建 effect consumer 超时退出（见 KNOWN-LIMITS“联盟服务冒烟里的两条观察”），之后两轮都通过。仓库检查：gofmt、build、vet、glsvet、根包、全量 `go test ./...`（本轮 `sync-aoi` 未出现偶发失败）、doc anchors 0 broken、`go generate ./...` 后 porcelain 干净、`test-demo-generated.sh`、`test-remote-generated.sh`（MirrorLocal 三项按门变量 SKIP）、`test-genres-generated.py` 13 类 passed；冒烟源码与日志未入库。**与方案的偏差**：①Result 按 target 的 kind 合并（守卫上是 Found / ReserveGuard 的回执）；`ResultReply.Code` 改名 `Outcome`，`GuardReply` 加 `AllianceID`（ReserveGuard 绑定的联盟）。②新增路由方法 `Establish`（两步建盟的第二步）。③不存在的联盟由路由查找 / 前置检查答框架的 `entity_not_found`，去掉 槽位版本里的存储探测与 `alliance_missing` 映射。④`alliance.transfer` 按数量时扫 DAO 集合再逐个读所有权：1R 的按 `_owner_sid` 枚举（`ListOwnedRemoteEntities`）没有经 Manager 对外暴露，没为此改框架。⑤2R 留在 scratchpad 的两条端到端草稿（预加载完成前不接请求、过期 owner 改投）没有并入仓库：要生成工程、起真实进程、直接往 `_remote_entity_meta` 写记录（耦合内部格式），代价大；本轮冒烟覆盖了改投与预加载重新登记，草稿与冒烟一起留给阶段 5 的真实依赖脚本。旧偏差仍成立：每日捐献计数在联盟席位里、没有 ClaimReward；写返回的版本 = 准入基线 + 1；有未决预约的守卫不随迁移走；只新增宣战作为多联盟操作；dev 配置的 WAL 目录 `data/wal/alliance`；生产 compose 里 game 的 sid 变为 1004。**留给阶段 4**：game 侧调用（端点、`uses_rpcs`、只读 Mirror 与 `//roost:mirror` DTO、ClientMod 经 `RouteSource` 取查找）、MemberNotice 的接收与推送、建盟扣钱 / 捐献 / 领奖的 Saga、迁移现有公会、dev-run 多实例；缩容时让实例退出 Found 的队列组（`LeaveRPCQueue`）没有接到运维命令。**未验证**：Remote 真实依赖下的并发认领与期限收尾、kill -9 时在途请求的“结果未知”重查（阶段 5）。 |
| 4 demo 迁移 | 已合入 main（维护者复核时 rebase 到停机撤 handler 修复 `4947c62a` 之上，复核：全量 `go test ./...`、根包、`test-demo-generated.sh`、`test-remote-generated.sh`、`test-genres-generated.py` 13 类通过） | `76b232fc`（生成器修复：dataengine 投影按服务判断）、`225953b3`（生成器修复：doctor 计入 Mirror 停机预算）、`95a65e55`（demo 迁移）、本行所在的文档提交 | **落地**（§2.6 对应表逐项）：game 去掉 `remote_entity`、装只读 `remote_mirror`（`demo.go` 换掉缺省 Mod），`uses_rpcs: [alliance]`（`demoGameUsesAlliance` 走 `commitManifestSync`，bootstrap 同时装上 `rpcalliance` ClientMod，查找经 `RouteSource`）。`found_guild`：端点只做名字预检（`alliancerule.ValidateName`）与守卫 Mirror 预检，`StartGuildOp` 在玩家锁内 `EmitStart` 建盟 Saga；`join_guild` / 新增 `leave_guild`：一次路由 RPC，请求号 = 玩家 + 会话 + 帧序号，答复带新状态、版本与 `ServedBySID`（处理请求的 alliance 实例）；`guild_info`：守卫 Mirror → 联盟 Mirror（`//roost:mirror` DTO `AllianceMirror` / `AllianceMemberMirror`，`View` 与 `alliancerule.View` 同形），客户端带写答复的 `Version` 作 `MinVersion`（Monotonic 的 `after`）；`gm.guild.*` 删除，用 alliance 的 `alliance.info` / `alliance.guard`；`PlayerDao.GuildID` 改 `nopersist,sync`（登录 `refreshGuild` 从 Mirror 填）；发号已在阶段 3 移到服务，game 的 `guild_ids` 删除；`ServedBySID` 改为 alliance 实例 sid；sidmerge 的 Remote 托管集合改为 `alliance` / `alliance_member`；mail `GuildScope`、聊天组频道、场景关系视野注释改为 Mirror / 通知。**三个 Saga**（`internal/service/<game>/alliance_saga.go`，运行时 `game-alliance`，转交模块 `game.alliance.handoff`）：建盟 = ① `found-debit` 扣 `guild.found_cost`（原生）② `found`（Mongo → `Found`），补偿 ① `found-refund` 退费、② `found-undo` 保留；捐献 = ① `donate-debit` ② `donate`（`Donate`），补偿同形；领奖 = ① `claim`（Mongo → 新路由方法 `ClaimReward`，守卫上按周期记领取，奖励由服务按 `alliance.reward_*` 答出、经 `Completion.Data` 交给下一步）② `reward-grant`（原生发奖），补偿 ① `claim-undo`（`ReleaseReward` 只撤这一次）、② `reward-take-back`。Player 步骤 `durability=pipelined`、只在绑定 sid 上跑（准入 / 转交同送礼）。失败语义：业务拒绝 → 不可重试失败、协调器补偿；没执行 / 结果未知 → 返回错误、步骤回滚重投、同一请求号再问，真执行过的由服务回执答出。**通知**：`internal/service/<game>/alliance_notice.go` 用 `ownerroute.RegisterBusHandler` 收 `alliance` 模块的 `alliance.member_notice`，只处理本进程在线的玩家：`ShowGuild`（离开只清正显示的那个）+ 推 `GuildNoticePush`（10104），尽力投递，登录补齐。**多实例与缩容**：`deploy/dev/second-alliance.sh`（sid 1001、ops 9210、advertise 9020、WAL `data/wal/alliance-1001`）；运维命令 `alliance.stop_founding` 调 `bus.RPCQueueLeaver.LeaveRPCQueue("alliance.Found")`，与 `alliance.transfer` 组成缩容三步（脚本头）。**Robot**：场景加道具 9001 → `found_guild` → 轮询 `guild_op_status` → `guild_info` → 捐献 → 轮询 → 领奖 → 轮询；动作另有 `join_guild` / `leave_guild` / `expect_guild_op`。**先红后绿**（14 条，原文抄在 `95a65e55` 与两个修复提交的说明里）：每周期只领一次、只撤自己的领取、GuildID 不落库、建盟被拒退费、撤销指定领取、通知到在线成员、离线成员不动、晚到的退盟不盖新公会、写后单调读、名字非法不扣费、答复映射、背包满发奖不可重试、拓扑（`demo_test`）、真实 NATS 上退出 Found 队列组；两个生成器修复各一条。**验收**（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 37000——36000 已被别的进程占用——加自起 etcd）：重新生成的 game-demo build、vet、`go test ./...`、`roost id check`、`roost generate --check`、`roost config check --all`、`project doctor` 94 项全 OK（`shutdown:game` 125s / 130s，`config-schema:game` 7 个业务键）、`go generate ./...` 后 porcelain 干净；仓库 gofmt、build、vet、根包、doc anchors 0 broken；全量 `GOWORK=off go test ./...` 除 `codegen/internal/roost` 外全部通过，该包在全量并发负载下撞到 `go test` 缺省 10 分钟超时（`panic: test timed out after 10m0s`，没有用例失败），单独以 `-timeout 30m` 重跑 564s 通过（该包本轮三次分别 426s / 564s / 610s，已贴近缺省超时，与本阶段新增的 game-demo 生成量有关，未另做处理）；本轮 `scripts/perf/sync-aoi` 的 `TestHeartbeatPatrolReachesClientsAndTheAOI` 在第一次全量里跳拍失败（inflight 4），第二次全量通过，本阶段没有改到它；`test-demo-generated.sh`（含新挂的 `TestStopFoundingSendsNewAlliancesToTheOtherInstances`）、`test-remote-generated.sh`（MirrorLocal 三项按门变量 SKIP）、`test-genres-generated.py` 13 类 passed、`test-mmorpg-generated.sh`、`test-sync-modes-generated.sh` 通过。**dev-run 冒烟**（重新生成的 game-demo，配置改指私有依赖、库名加前缀、JetStream 流上限调小到 256MiB 以适配私有 NATS 的 4GiB 存储）：十一个进程 + 第二个 alliance 全部 ready；`make dev-smoke`（2 人）通过，`found_guild` / `donate_guild` / `claim_guild_reward` 各 2 次、`guild_op_status` 7 次全部 ok；再跑 10 人：建盟分到两个实例（1001 拥有 3 个）；在 1001 上 `alliance.stop_founding` 后 6 人新建的联盟全部落到 1000；`alliance.transfer {"count":100,"to_sid":1000}` 搬走 6 个实体（联盟与守卫），再跑一次搬 0 个；停掉 1001 后原属它的联盟由 1000 答复、资金不变；再跑 4 人全部成功；全部进程日志 0 条 ERROR。冒烟第一次起服撞到生成器缺陷（game 的 dataengine 打开了 Remote 投影，`capability "remote_entity" not found`），修复见 `76b232fc`；loadtest 手写编解码漏了新消息，已补。**与方案的偏差**：①Mirror 读一律 Monotonic（Cached 未命中不回源，第一次读没见过的公会答“不存在”）；②错误码 `guild_name` / `guild_exists` / `guild_full` 删除，改答服务的 `alliance_*`（不再保留两套码），新增 `guild_moved`（100024）、`guild_pending`（100025），`guild_busy` / `guild_missing` 保留；③金币用道具 9001（`PlayerDao.Gold` 没有来源）；④新增路由方法 `ClaimReward` / `ReleaseReward`、答复 `RewardReply`、错误码 `alliance_reward`（100042）、守卫字段 `RewardPeriod` / `RewardRequestID`；奖励定义在服务一侧（`alliance.reward_item_id` 1002 / `reward_count` 5 / `reward_period` 24h，原型缺省）；⑤建盟 / 捐献的迟到成功保留结果并记 Warn（KNOWN-LIMITS）；⑥运维命令名 `alliance.stop_founding`；⑦`cached_max_staleness` 没改成 5s（留阶段 5 量回源后定）；⑧Robot 场景没有“两个 game 进程上的玩家加入同一公会”（每个机器人只建自己的），留阶段 5。**留给阶段 5 的入口**：`second-alliance.sh` 与 `alliance.stop_founding` / `alliance.transfer`（扩缩容与迁移）、`second-game.sh`（第二个 game）、loadtest 的 `join_guild` / `leave_guild` / `guild_info`（`MinVersion`）/ `guild_op_status` 动作、`scripts/test-demo-generated.sh` 已挂真实 NATS 用例 |
| 5 测试与端到端 | 本地提交在分支 `test/alliance-p5`，待维护者复核（未 push） | `ef4b9d7c`（脚本与用例）、本行所在的文档提交 | **入口**：`scripts/test-alliance-e2e.sh`（生成当前检出的 game-demo，把 `scripts/testdata/alliance-e2e/*_test.go.txt` 复制进工程的 `e2e/` 运行；只对 `scripts/mirror-local.sh up` 起的私有环境跑，脚本自起私有 etcd；用例用生成的二进制起真实进程，测试进程只经生成的路由 `BusClient`、玩家 TCP（account 真实登录）与运维 `/admin` 驱动，故障用 kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理；不直接写 `_remote_entity_meta`、Redis 登记或 WAL）。2R 的两条草稿与阶段 3 的冒烟并入：预加载顺序改为先经 `Found` 建 500 个联盟、正常停机再同 sid 拉起（原草稿直接往所有权集合种记录），过期 owner 改投改为 `alliance.transfer` 后用带旧缓存与固定答旧 owner 的两种调用方验证（原草稿种一条 owner=3001 的记录）。**场景与结论**（2026-11-04 macOS，私有依赖偏移 32000，本机后台负载 load average 15～30）：① **预加载顺序**通过（`TestAllianceE2EPreloadOrder`：2000 个联盟时预加载 2.0s，期间 342 次探测全部 no responders，完成后约 30ms 第一次得到答复，owner Mod 晚于预加载完成启动）。② **两个 game 进程**通过（`TestAllianceE2EGamePlayers`：game 1000 上的玩家经建盟 Saga 建盟、扣道具 9001 一次；game 1001 上的玩家入会，写答复版本作 `MinVersion` 后两边 Mirror 都读到 2 人；盟主在 game 1000 收到 `GuildNoticePush{Change: joined}`）。③ **一人一盟**通过（`TestAllianceE2EOneAllianceUnderRace`，`reservation_ttl` 5s，每组 24 人：守卫不存在时两实例并发新建、守卫在 3000 时同实例事务对两步、两个不开放联盟的盟主并发批准、两步进行中 kill -9 守卫方、kill -9 联盟方，五组全部“恰好一个成功、成员表与守卫一致、没有残留预约”；联盟方被杀期间守卫方的 `alliance-reservation-due` 按 effect 重投 Settle（日志里 no responders 后重投），没有按时钟释放）。④ **kill -9 与接管**通过（`TestAllianceE2ECrashTakeoverAndScaleIn`：200 个并发入会发出 400ms 后 kill -9 联盟 owner，答复全部是“结果未知”；同 sid 拉起后按原 RequestID 查 `Result`、重发，全部得到确定答复，每人恰好入会一次；kill -9 后登记仍在、请求可重试，约 28s 登记过期后仍可重试；拉起后预加载恢复的登记值与崩溃前相同；`alliance.transfer` 之后带旧缓存的调用方经改投到新 owner，期间 0 次回退读所有权；缩容三步后原属 3001 的 10 个联盟与 200 个守卫由 3000 答复、成员不丢）。⑤ **Saga 只扣一次、只退一次**通过：捐献 50 资金 +50、道具 −50；建盟与 RPC 入会同时发生 6 次，6 次都走到服务拒绝 → 补偿退费、道具不变；捐献时把公会 owner SIGSTOP，等 Player 扣款步骤完成后 kill -9 玩家所在的 game 1001 与冻结的 owner，同 sid 拉起后 Saga 完成、资金 +30、道具 −30 各一次；建盟时两个 alliance 实例都冻结、扣款后 kill -9 两个实例，拉起后只建出一个盟主是该玩家的联盟、扣 100 一次。⑥ **Redis 不可用**（`TestAllianceE2ERedisOutage`，进程与调用方都经 toxiproxy 的 redis 代理）：断开 6s 内路由 RPC 全部成功，空路由缓存的调用方每次 1 次 Mongo 点读（Redis 正常时 0 次），调用方 p50 14ms → 125ms（go-redis 拨号重试的时间）；跨实例的两步入会照常完成；断开期间删掉的登记在恢复后约 5s 由续期“不存在”重新登记。**与方案不同**：断开超过约 12s，两个 alliance 实例都因单实例锁续不上而 fail-stop（`singleton lock lost; fail-stop`，exit 1）——单实例锁连的是同一个 Redis，所以方案 §5 的“登记失败只影响路由效率”只对短于单实例锁失锁期限（`ttl − guard`）的断开成立；用例把这一点钉成断言并在恢复后同 sid 拉起。⑦ **短测**（`ALLIANCE_E2E_PERF=1`，数字见 [benchmarks/alliance-20261104](../performance/benchmarks/alliance-20261104/README.md)）：路由 RPC（带缓存、单并发）`Info` p50 9～17ms、`Join` p50 86～115ms，同一实例同一方法串行（`Info` 约 100 次/秒、`Join` 约 13 次/秒、`Found` 约 9 个/秒）；预加载 1000 / 2000 / 10000 个联盟 1.7 / 2.0 / 13.7s，预加载完常驻内存 65 / 71 / 123 MiB；续期量 = 已登记实体数 / 10s（1000 → 1000、10000 → 10000）；Mirror 回源 = 在读的不同 key 数 / `cached_max_staleness`（21 个 key、60s：30s 时 42 次、5s 时 252 次 Redis HGET，与读频率无关），结论是 game 读者可以用 5s（10 万在线约 2 万次/秒 HGET），推荐改 5s，生成配置没改、待维护者确认。**发现的问题**（没有改框架，方向待维护者定，KNOWN-LIMITS 已登记）：①建盟被拒留下只有所有权记录、没有数据的联盟号，重启后预加载对它记 ERROR、对它的请求一直答可重试（根因 `routedOwnership` 没认 `ErrAuthorityEntityNotFound`）；②实体服务的 RPC 在一个实例上按方法串行（bus 按方法名哈希选 worker）；③`alliance.transfer {count}` 在上万实体时超过 `ops.admin_timeout`；④Redis 断开超过单实例锁期限时实例 fail-stop（上面 ⑥）。**与方案的偏差**：①Mirror 回源只能用 Redis `HGET` 调用增量间接量（Mirror 没有按来源计数的指标）；②10k 预加载的联盟是四个实例并发建、再 `alliance.transfer` 到一个实例（单实例建盟约 9 个/秒，1 万个要 18 分钟）；③`Durability memory`（§3）没有量，仍按 strict；④Robot 场景仍是每个机器人只建自己的公会，两个 game 进程的用例用 Go 客户端驱动玩家。**验收**：`scripts/test-alliance-e2e.sh` 从干净生成到跑完五个用例本机 400s 通过（另在迭代中各跑过多轮）；短测 `TestAllianceE2EShortPerf`（1000 一轮、10000 一轮，后者约 20 分钟）与 `TestAllianceE2EMirrorRevalidation`（约 5 分钟）各一轮；仓库 gofmt、`go build ./...`、`go vet ./...`、根包、doc anchors 0 broken、全量 `GOWORK=off go test -count=1 -timeout 30m ./...` 881s 通过（`codegen/internal/roost` 865s）、`scripts/test-demo-generated.sh` 377s 通过；私有依赖用完 `mirror-local.sh clean`。**未验证**：Linux；跨机；Redis Cluster 下的路由登记；兴趣表容量（10 万在线的 `AllianceMember` 兴趣）；上千人联盟；JetStream RPC 的 owner（alliance 只用轻量 RPC）；Robot 两进程场景；接入夜间故障矩阵（脚本需要私有环境的 toxiproxy 与 etcd，是否挂进 `dataengine-env.sh test` 待定）。 |
| 5 跟进 | 本地分支 `fix/remote-empty-claim`，未推送，待维护者复核 | `9a1d9ee4`（空认领）、`117e45ef`（按 owner 枚举与 transfer）、`fe4a1153`（game 的陈旧上限 5s）、本行所在的文档提交 | 维护者 2026-11-04：逻辑 bug 修完、基本逻辑跑通后合进 main。① **空认领**（阶段 5 发现 ①）按推荐 A 在框架统一语义：所有权记录是本 sid、权威里没有聚合 = 本实例拥有、业务上还不存在的实体。`routedOwnership` 认 `ErrAuthorityEntityNotFound`（空认领：前置检查答由本实例服务、不登记；预加载计 `Empty`）与 `ErrAuthorityEntityDeleted`（删过档：前置检查答 `entity_deleted`，预加载计 `Deleted`）；Nest 对 Remote 声明目标写准入里的冷加载结果同样套 `entityStateError`（修前原样返回，生成 RPC 答 `remote.1 server error`，第一次端到端绿跑时暴露）。偏离派单的一点：前置检查不直接答 `entity_not_found`，而是交给业务——派单的做法会让经路由 RPC 到达 owner 的创建类请求（`ReserveGuard`、`Establish`、`Guard`，都先 `GetOrCreate`）永久失败，例如玩家的守卫被别的实例空认领后再也入不了那个实例之外的联盟；交给业务后读请求同样得到确定的 `entity_not_found`。“回滚时撤销认领”评估后未采用（提交前崩溃同样留下空认领；撤销是新的所有权 CAS，结果未知时照样留下）。② **`alliance.transfer {count}`**（发现 ③）：`Manager.ListOwnedRemoteEntities` / `entity.RemoteOwnedEntityLister` 对外暴露按 owner 枚举，demo 改用它分页列出、按命令期限分批（`more`）；删去 DAO 集合扫描。③ **game 的 `cached_max_staleness` 5s**。**验证**（2026-11-04 macOS，私有依赖 `scripts/mirror-local.sh` 偏移 33500）：框架回归 `TestEmptyClaimIsAnOwnedEntityThatDoesNotExist`、nest `TestRemoteTargetLoadStateBecomesAClientCode`、wiring `TestRPCRouteCheckAnswersFromTheOwnershipRecord`（删过档）先红后绿，修前文本在 `9a1d9ee4` 的提交说明里；端到端 `TestAllianceE2ERejectedFoundingAfterRestart` 修前 `preload after restart: listed=3 loaded=2 empty=0 failed=1`，修后 `empty=1 failed=0`、重启后 `Info` 答 `entity_not_found`（重启前答 100031）、全程 0 条 ERROR。**transfer 耗时**（`TestAllianceE2EShortPerf`，四个实例共约 2 万个实体，三个实例各把持有的全部转给 3000，缺省 `ops.admin_timeout` 10s）：修前在缺省期限下第一次调用就 `scan alliance_member: context deadline exceeded`，放宽到 600s 后 15447 个用 5m57s；修后 15439 个用 5m10s，43 次调用（37 次在期限前停下答 `more`），最慢一次 8.1s，全部在期限内答复。按 owner 枚举本身：真实 Mongo 上 4 万条所有权记录里列出本实例的 1 万条约 55～80ms，余下时间都是逐个 `TransferRemoteOwnership`（约 19ms 一个）。同一轮 1 万个联盟的预加载 11.7s，与阶段 5 的 13.7s 同量级。 |
| 6 文档 | 未开始 | — | — |
