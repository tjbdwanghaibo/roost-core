# Redis 做 Remote / Saga 权威：方案审查与第 1 步协议草案（2026-10-26）

> **后续：设计已收敛为 [v2](DESIGN-2026-10-26-redis-authority-v2.md)**（维护者 2026-10-26 决定不考虑 Redis 切主、按 v2 直接实施）。本篇保留为审查记录，R1～R18 在 v2 §5 逐条处理。

**状态：审查与提案，未实施，不是现行契约。** 维护者 2026-10-26 要求先对方案做代码审查。本篇只读审查，不改产品代码；第 1 步协议与第 2 步记录结构都是草案，等维护者拍板后再登记到 DESIGN-TODO。

- **审查对象**：主检出未提交草稿（2026-10-26）`docs/framework/REDIS-COMMIT-AND-STORE-SERVICE.md`（下称“方案”，引用写“方案 §N”）。草稿从未入库，路线放弃后已于 2026-10-29 删除；它的方向与不变量见 [Redis 权威设计 v2](DESIGN-2026-10-26-redis-authority-v2.md)。
- **源码基线**：main `044f6086`。方案的文档基线是 `ce49b3c3`，两者之间只改了物理房间与 `framework/nest/try_batch.go` 的时间片，本篇涉及的 DataEngine / Saga / Remote / Redis 源码没有变化。
- **图谱**：codebase-memory 项目根是主检出（`ce49b3c3`）。本篇的结论以 worktree 当前源码为准，行号都按源码直接读出。
- **性能数据**：本地逐跳样本见 [Saga 单笔时间线](../performance/benchmarks/saga-timeline-20261023/README.md)。云库数据有两份：一份是主检出未提交样本 `saga-cloudmongo-20261026/` 与 `mongo-cloud-20261026/`（从未入库，2026-10-29 删除；正式数据见 [SAGA-CLOUD-MONGO-20261026](../performance/SAGA-CLOUD-MONGO-20261026.md)），另一份是协调方转述的中途数据（本机到云 Mongo RTT 约 61ms，服务端单次写 4～5ms；原生步骤一笔串行约 50 多次 Mongo 往返，并发 1 时一笔 3.9s；投影事务每条约 530ms，并发 16 起出现 Failed）。正式报告 `SAGA-CLOUD-MONGO-20261026.md` 在收尾时还没写出，凡是依赖它的结论都标了“待云库数据”。

## 0. 结论

1. **方向上的主要结论：瓶颈是“串行往返次数 × RTT”，不是 Mongo 本身。** 按源码数，一个原生步骤（含协调器推进）串行约 31～33 次 Mongo 往返；两步交易加启动链约 65～70 次。61ms × 64 ≈ 3.9s，与中途数据一致。投影事务按源码是 9 次往返，61ms × 9 ≈ 549ms，与实测“每条约 530ms”吻合（§6）。如果 Redis 也部署在同样的网络距离，方案能省下的只是往返次数：第 4 步完整落地后每步约 9～10 次，约为现在的 1/3。不换存储、只在 Mongo 链路上合并往返的候选做法（§6.4），预计能省下其中约 2/3。同地域部署对两种做法都是数量级的影响。
2. **持久性是最大的风险。** 现有 Remote 之所以能扛住 Redis 切主，是因为 fence 的权威在 Mongo。仓库里有一条生产准入测试，专门证明 Redis 切主后 fence 计数器会回退复用：`framework/remoteentity/cluster_fault_integration_test.go:317-470`，见 R1。方案 §6 把 ownership / grant / fence 迁进 Redis，等于拆掉这层保护；§8 只写了“必须给出”，没有给出选项。这一项必须由维护者拍板，见 §7。
3. **普通 Entity 的 RedisActive 状态机有 4 个按源码可以坐实的缺口**：
   - 两条提交链之间失去 pipelined 依赖的前缀持久性（R3）；
   - 首次进入只等“投影完成”不够，快路记录会在 checkpoint 之前就被判为完成（R4）；
   - 重启与冷加载的顺序要靠 Store Service 才能排出来（R9）；
   - 心跳批量作业运行在快池上（R10）。
   这一块应排在最后；能否继续做，取决于前几期的数据。
4. **分期顺序基本同意，但要改两处**：
   - 在第 1 步之后插入“第 1.5 步：不换存储、先减往返”，它的结果决定第 2～4 步还值不值得做；
   - 第 2 步只能迁协调器，不能迁步骤收件箱。原生步骤的执行权判定必须和实体提交处在同一个原子边界，在第 4 步之前它只能留在 Mongo；Mongo 步骤的状态文档则要永远留在 Mongo（R11）。

## 1. 问题清单（按严重度）

严重度：**S1** 不解决就不能实施，或会造成数据错误；**S2** 实施前必须写进协议；**S3** 漏项，补进协议和验收即可。

| 编号 | 严重度 | 问题 | 源码 / 依据 |
| --- | --- | --- | --- |
| R1 | S1 | Redis 主从（含 Sentinel / Cluster）不是共识系统。fence 计数器在“未复制即切主”时会回退并被复用；现在靠 Mongo 持久 fence 拒掉旧许可。方案 §6 把 fence 迁入 Redis，这层兜底就没了，§8 也没给 RPO 选项 | `framework/remoteentity/cluster_fault_integration_test.go:317-470`：断言 Redis 计数器回退（:456-458）、旧提交被 Mongo 拒绝（:459-468）；`framework/remoteentity/versioned_lock.go:150-170`（取锁后向 Mongo `GrantWrite` 取 fence）；`framework/remoteentity/mongo_authority.go:117-141`；`framework/remoteentity/mongo_committer.go:470-490`（提交以 `_grant_fence` 做 CAS） |
| R2 | S1 | 方案没有减少往返次数的设计。在同样的网络距离下，迁移 Redis 的收益只来自“一个 Lua 脚本替代 N 条命令的事务”；不换存储也能拿到其中大部分（§6） | §6 往返审计 |
| R3 | S1 | 两条提交链并存时，pipelined “锁外等持久、进程内允许读未落盘状态”的正确性论证失效。论证依赖“单日志按 LSN 前缀落盘”，跨本地 WAL 与 Redis 段没有这条性质。只读参与者也会造成跨链依赖：T2 读了 RedisActive 实体 A 的未确认状态，只写 Local 的 B，B 进本地 WAL；Redis 切主丢了 T1 后，T2 仍会落盘。方案 §4.2 说“可复用 pipelined 访问隔离”，不成立；§5.1 只把“持久写参与者”纳入同一个域 | `docs/framework/NEST_PIPELINED_COMMIT.md` §2 第 2～3 条；`framework/nestwal/wal.go:362-420`（Enqueue 的 LSN 是本地单日志） |
| R4 | S1 | 首次进入（方案 §4.1）若只“等本地 WAL 投影完成”，会留下一个崩溃窗口。单 mutation 快路记录不写事务标记，投影完成就唤醒等待方，之后才 checkpoint。如果 Store Service 在这期间把同一 DAO 的版本推进，进程又在 checkpoint 之前崩溃，重启重放这条旧记录会命中 `ErrProjectionConflict` fatal，进程起不来 | `framework/dataengine/engine/projector_replay.go:128-147`（先 `completeProjection`，后按 `replaySafe` checkpoint）；`framework/dataengine/engine/projection_plan.go:37-39`；`framework/dataengine/engine/mongo_projection.go:42-45`（快路不写标记）；`framework/dataengine/engine/mongo_store.go:249-311`（`mutationFilter` / `classifyNoMatch`） |
| R5 | S1 | 现有回执与墓碑的保留期按 Mongo 设计，缺省 720h（30 天），并且必须长于消息流的保留期。原样放进 Redis，内存放不下。以 30 笔/s、每笔 2 步、每步约 4 份 0.5～1KB 的记录估算，30 天约 300～600GB。方案 §7 的回收表没有区分“Redis 短窗”和“Mongo 长窗” | `wiring/dataengine/mod.go:130-131`；`wiring/saga/config.go:64`；`wiring/saga/mod.go:140`；`wiring/dataengine/effects.go:39-48,87-89`；`docs/framework/SAGA.md:471-473` |
| R6 | S2 | 方案没有选定 Redis 提交在哪里等网络。若在锁内等，就是在快池 worker 上等网络；strict 锁内 fsync 的豁免是本地盘、有界，不能自动延伸到网络和故障切换。若锁外等再拿结果，Lua 的拒绝会晚于放锁，又回到今天“跳过后驱逐”的形状。仓库里已有可复用的模式：Remote 消息在 io 阶段准备、业务池执行、io 阶段提交，写闸门跨网络持有 | `framework/nest/nest_dispatch.go:93-140`；`framework/nest/msg.go:80-120`；roost-coding“快池内不得阻塞等待”与显式豁免清单 |
| R7 | S2 | 多实体准备（方案 §5.1）只能在 io 阶段、针对显式声明的目标进行，动态 Cast 和未声明的目标准备不了。这种事务碰到 RedisActive 实体时，必须在 WAL 准入前拒绝并整体回滚，方案没写。另有传染效应：共享实体（公会、世界）一旦 RedisActive，每个与它同事务的成员都要 Entering | `framework/nest/nest_dispatch.go:60-71`（冷目标改走 io 池）；`framework/dataengine/engine/projector.go:571-610`（准入前拒绝的现有先例） |
| R8 | S2 | 持久级别在 Redis 链上的含义没有定义。async 现在只等写入、不等 fsync；在 Redis 上，Lua 的校验结果至少要 1 RTT 才能拿到，否则就没有拒绝点。memory 带 effect 时会升级为 strict，带持久写时直接拒绝 | `framework/nestwal/wal.go:314-360`；`framework/nest/rollback.go:341-361,589-636` |
| R9 | S2 | 恢复顺序与 Store Service 耦合。Game 启动时要先排空本地 WAL，实体才能加载；可 Closing 之后的本地记录建立在 Redis 段之上，Redis 段没投影完，这些记录的版本条件就不成立。冷加载、`entity.unload`、跳过后驱逐，现在都只等本进程的投影 | `framework/dataengine/engine/fenced_step.go:28-31`；`framework/dataengine/engine/entity_projection.go:88`；`framework/dataengine/engine/entity_unload.go:65-150` |
| R10 | S2 | 心跳和定时器会持续写 RedisActive 实体。心跳缺省是 async、走快池上的批量作业（每组 4 个、时间片 100µs），批内提交如果要走 Redis，就会在快池上等网络。在线玩家只要有持续的持久写，就满足不了“在途为空、投影追平”的 Closing 条件，会长期留在 Redis | `gameplay/heartbeat/host.go:62-69`；`framework/nest/try_batch.go`；`docs/maintenance/KNOWN-LIMITS.md` §实体心跳 |
| R11 | S2 | 方案 §6 写“协调状态、领取/重试 … 迁到 Redis”，又写“步骤执行预留与最终提交检查采用同一权威协议”，这对分期顺序有约束：原生步骤的租约 fence 必须和实体提交在同一个原子边界，所以第 4 步之前步骤状态文档只能留在 Mongo；Mongo 步骤的状态文档与业务写在同一个 Mongo 事务里，要永远留在 Mongo | `framework/dataengine/engine/mongo_projection.go:92-103`；`framework/saga/command_consumer.go:150-188` |
| R12 | S2 | 事务域和分区的说法自相矛盾，现有驱动也不支持。协调器是全局的，Remote 实体被多个 Game 共享，“按 Game 归属组 / 区服”的域装不下它们。`EvalBatchDurable`（同连接 WAITAOF）明确拒绝 Cluster；`EvalReplicated` 用的是 WAIT 而不是 WAITAOF，在 MOVED / ASK 或没有副本时会跳过等待，只适合 L2 墓碑这种尽力而为的场景，不能用于权威提交 | `infra/storage/redis/driver/client.go:367-405`（:389-391 拒绝 Cluster）；`infra/storage/redis/driver/replicated.go:14-56,61-117` |
| R13 | S2 | 合服（D15）的前置条件要扩展：现在要求 WAL 已排空、saga 全部终态、outbox 为空，新方案还要加上 Redis 段已关闭且投影完、Redis 协调器全终态、Store Service 无积压。按发起 sid 还是承载 sid 划分事务域，也没有说 | `docs/maintenance/FEATURE-2026-10-20-server-merge-alias.md` §6.3 第 3～4 条、§P3 |
| R14 | S2 | 删档墓碑（D10）的位置会变。删档投影是软删墓碑（`_deleted`），Remote 的“删过档”只记在 Remote 元数据里，冷加载靠它区分“删过档”和“从未创建”。新方案下，RedisActive 的删除和 Remote 的元数据都先落 Redis，冷加载必须先看 Redis，Store Service 要按墓碑投影 | `framework/dataengine/engine/mongo_store.go:231-247`；`framework/remoteentity/mongo_authority.go:66-80` |
| R15 | S3 | Saga 补偿、迟到成功与 Redis 段关闭交错时的规则没写。每个原生步骤（含补偿、迟到成功触发的补偿）都要让实体 Entering；Closing 的条件必须包含“没有已预留、未提交的步骤尝试”，否则这次尝试会撞上已关闭的段 | §4.3 |
| R16 | S3 | Remote 共享模式的多写者仲裁、ownership 迁移（EnterShared / LeaveShared / Transfer），现在都是 Mongo 上的 CAS。迁到 Redis 后要有等价的 Lua 实现，并且同样受 R1 的切主回退影响 | `framework/remoteentity/mongo_authority.go:142-180`；`framework/remoteentity/ownership.go` |
| R17 | S3 | 普通业务 effect（`EmitStart`、邮件等）在 RedisActive 实体上同样要进 Redis 的 outbox。方案 §7 只写了 completion 和 command | `framework/nest/rollback.go:341-361`；`framework/dataengine/engine/mongo_store.go:483-517` |
| R18 | S3 | 历史上有过 Redis 快照 WAL（遗留的 checkpoint 包，带 TTL 的键），2026-09-01 在 `66cfea12` 删除，改为本地文件 WAL。方案部分回到 Redis 提交日志，没有交代这次有什么不同 | `git show 66cfea12 --stat`（删除 `checkpoint/redis_wal.go`） |

## 2. 现状核对（方案 §2 逐条）

| 方案 §2 的说法 | 核对结果 | 依据 |
| --- | --- | --- |
| strict 成功表示 WAL fsync，不是 Mongo 已提交 | **正确**。补充两点：async 只等写入、不等 fsync；pipelined 在锁内入队，锁外等票据。带 Remote 批次、经 `Append` 提交的 pipelined 记录按 strict 等 fsync | `framework/nestwal/wal.go:314-360`（`requireSync: Durability >= Strict`）、`:362-420`；`docs/framework/NEST_PIPELINED_COMMIT.md` §5 范围限定 |
| async / pipelined 的区别不能被无声改变 | 正确，但**漏了 memory**：memory 加 effect 会升级为 strict，memory 写持久字段会被拒绝。方案 §4.2 也没有给出四个级别在 Redis 链上的映射（R8） | `framework/nest/rollback.go:341-361,589-636` |
| checkpoint 是已投影记录的确认 / 重放进度 | **正确，但不完整**：带标记的记录按条数或间隔合并确认（缺省 256 条或 20ms），单 mutation 快路立即确认；`completeProjection` 唤醒等待方的时刻早于 checkpoint（R4） | `framework/dataengine/engine/projector_replay.go:128-147`；`docs/maintenance/CONTRACT-2026-10-18-projector-checkpoint-merge.md` |
| 原生步骤的业务修改、receipt、lease fence control receipt、completion effect 在同一个 CommitRecord | **正确** | `framework/saga/native_step.go:62-87`（`Complete` 在 :70）；`docs/framework/SAGA.md:114-121` |
| Mongo 投影事务校验租约，可能跳过记录 | **正确**。校验是对操作状态文档的**条件写**，不是读，与“接替”写同一份文档，两者只能提交一个 | `framework/dataengine/engine/mongo_store.go:359-395`；`framework/dataengine/engine/mongo_projection.go:87-103` |
| completion 在投影后发布 | **正确**。effect 先在投影事务里暂存进 Mongo outbox，再由 outbox worker 认领、发布、删除；投影后会唤醒 worker，轮询只是兜底 | `framework/dataengine/engine/mongo_store.go:483-517`；`framework/dataengine/engine/outbox_worker.go:123-167` |
| 投影结果确定前有实体写屏障 | **正确**。屏障在 `reserve` 的同一个临界区里检查（`ErrFencedEntityPending`）；记录被跳过后，在业务池驱逐受影响的实体 | `framework/dataengine/engine/fenced_step.go:17-31,86-98`；`framework/dataengine/engine/projector.go:571-610` |
| Saga 生效裁决必须前移到 Redis 原子提交 | 方向正确，但**没有说裁决点在锁内还是锁外**（R6），也没写这会改变步骤收件箱的位置（R11） | — |
| 调度实验没有延迟收益（P99 约 556ms 对约 523ms） | 与草稿说法一致。本篇没有复跑；实验分支只作只读参考 | — |
| **漏：Remote 的现状** | 方案 §2 完全没写 Remote 的现状。现在 Remote 的数据权威和写权限权威都在 Mongo（元数据里的 owner / epoch / `_grant_fence` / `_ver`），Redis versioned lock 只用于竞争协调，L2 是快照水位权威。Durability 1/2/3 的 Remote 提交走本地 WAL，再由投影进 Mongo，冲突时 fatal、不产生 Rejected；Durability 0 由 io 阶段直接写 Mongo | `docs/framework/REMOTE_ENTITY.md` §存储事务、§持久拒绝；`docs/framework/impl/05-remote-mirror.md` §2；`framework/remoteentity/mongo_authority.go:13-19` |
| **漏：Redis 已有的持久确认能力** | `EvalDurable` / `EvalBatchDurable` 已经实现“脚本 + WAITAOF 同连接流水线”，但没有生产调用方，而且拒绝 Cluster；`EvalReplicated`（WAIT）的语义不够权威提交用（R12）。`9dd9806a` 之后，MOVED / ASK 被归为“确定没执行”，在既有预算内安全重发；回复丢失（EOF / 读超时）仍是结果未知 | `infra/storage/redis/client.go:88-125`；`infra/storage/redis/driver/README.md` §1、§2、§重定向预算 |
| **漏：可复用的现有屏障** | `entity.unload` 已经实现“业务池登记屏障 → 业务池外等投影 → 业务池卸载”，并用 `fctx.BlockingError` 防止在快池上调用。首次进入可以照这个形状做，但要等到 checkpoint，不只是投影完成（R4） | `framework/dataengine/engine/entity_unload.go:65-150,206-250` |
| **漏：原生步骤不能改 Remote** | 方案 §5.2 提到了，但没说明原因：生成 Entity 的 `RollbackRemoteCommit` 不保存跨实体前像，记录被跳过后无法撤销。如果新方案把裁决前移成“准入即裁决、不再跳过”，这个限制理论上可以放开，但需要独立验收 | `docs/framework/SAGA.md:144-153`；`framework/dataengine/engine/projector.go:571-577` |

## 3. 不变量逐条审查（方案 §3）

| # | 不变量 | 现有代码在哪里保证 / 会在哪里被破坏 | 新方案的强制点应该落在哪里 | 依赖的 Redis 能力与限制 |
| --- | --- | --- | --- | --- |
| 1 | 单一提交位置 | 现在每个 Game 只有一个 WAL，持有者独占目录（`framework/nestwal/lock_unix.go`），所以一条链天然成立。新方案下，提交位置成了实体的运行期状态，任何没走路由的写入口都会破坏它：心跳批量作业（R10）、动态 Cast（R7）、系统事务 `CommitSystem`（`framework/dataengine/engine/projector.go:249-285`，删档用它） | 路由必须放在 `Projector.reserve` 这一处（与屏障、卸载屏障同一个 `heldMu` 临界区，`projector.go:578-610`）：RedisActive 实体的记录**一律拒绝进本地 WAL**，返回可重试错误。不能只靠调用方自觉选路由 | 不依赖 Redis 能力，依赖 Nest / DataEngine 侧的唯一入口 |
| 2 | 单一生效点 | 现在的生效点是 Mongo 投影事务里的条件写（`mongo_store.go:359-395`），投影晚于租约就整笔跳过 | Redis Lua 在一个脚本里完成：核对操作状态（token、截止）→ 写实体变更、回执、completion 意图 → 推进段序号。持久确认之后，投影只做复制，不再裁决 | Lua 原子执行但**不回滚**：所有校验必须在第一条写之前完成。“达到规定持久确认”取决于 §7 的选项：选项 A/B 下，切主仍可能让已确认的生效点消失（R1） |
| 3 | 完整事务 | 现在一条 CommitRecord 就是原子单元，多 DAO、多实体都在一个 Mongo 事务里投影 | 一个事务的全部键必须在同一个哈希槽（hash tag），在执行修改之前就要判定能否同域，不能执行后才发现跨槽 | Cluster 不支持跨槽原子操作。现有 `EvalBatchDurable` 不支持 Cluster（R12）；首版只能是单主或 Sentinel |
| 4 | 未知不等于失败 | Redis 驱动契约：回复丢失属结果未知、不重放；MOVED / ASK 与“确定没执行”可以重发（`infra/storage/redis/driver/README.md`）。Mongo 提交结果未知时带 `ErrCommitResultUnknown` | 每次提交带稳定的事务 ID 与摘要；Lua 先查“这个 ID 已提交且摘要相同”就返回原结果。WAITAOF 超时或确认数不足要单独分类为“已执行、持久未确认”，保留未决，不回滚也不报成功 | WAITAOF 只统计**本连接**之前的写，必须与脚本同连接（现有 `EvalBatchDurable` 满足）。换连接重新 WAITAOF 只会立即返回，是假确认 |
| 5 | 先恢复再服务 | 现在启动恢复在实体可加载之前排空 WAL（`fenced_step.go:28-31`），冷加载会等本进程的在途投影 | 加载有 Redis 段的实体时，必须读 Mongo 底版，再按序回放 Redis 段的尾部（或者先等 Store Service 投影追平）；本地 WAL 里 Closing 之后的记录，要等对应 Redis 段投影完才能回放（R9） | 对 Redis 是有序读（Stream 的 XRANGE），要求键与段在同一个槽 |
| 6 | 先保存后回收 | 现在分三块：投影 checkpoint（WAL）、outbox 发布后删除（`outbox_worker.go:153-157`）、回执 TTL（720h）各自独立 | Redis 上至少要分开四个进度：①段日志的投影进度（Store Service 的 Mongo 已提交）；②outbox 的发布确认；③回执的 Redis 短窗；④关闭代际的保留。Redis 里删除的前提必须是“Mongo 已有”，回执要分层（R5） | 禁止对未投影数据设 TTL 或 `MAXLEN` 截断；淘汰策略必须是 `noeviction` |
| 7 | 普通业务不被扩大改造；nopersist 不被持久化 | nopersist 字段没有 mutation，不会进入任何提交记录（`framework/nest/rollback.go:653-676` 只检查 MarkPersist 登记的字段）。这一条在新方案下自然保持 | 不需要新的强制点，但验收要覆盖：实体在 RedisActive 期间重新加载后，nopersist 字段按冷加载规则重建 | — |

## 4. 与三大块执行契约的冲突

### 4.1 首次进入（方案 §4.1）

- 等待不能发生在快池：方案自己也写了，但触发 Entering 的入口有两类，要分开处理。
  - **步骤消费者**（`SubscribeDataEngineStep` 的回调 goroutine，不在 Nest worker 上）：可以照 `entity.unload` 的做法——业务池登记屏障，在回调 goroutine 里等，再回业务池安装状态（`entity_unload.go:65-150`）。
  - **普通 handler 因为同事务碰到 RedisActive 实体而需要让另一个实体进入**：只能在 io 阶段、针对声明目标准备（`framework/nest/nest_dispatch.go:93-118`），动态目标一律拒绝（R7）。
- 等待的终点必须是“这个实体的全部本地记录都已被 checkpoint 覆盖”，不是“已投影”（R4）。可行的做法有两种：在屏障下强制 checkpoint 一次；或者给每个实体登记最后的 LSN，等全局 ack 水位越过它。
- 首次进入要付一次投影等待。在中途数据下，投影事务约 530ms；如果前面还有积压，等待会更长。方案 §4.1 承认“不承诺完全不等待 Mongo”，但没给预算。需要在第 4 步的验收里报告 Entering 的等待分布。

### 4.2 每笔提交的确认与隔离（方案 §4.2）

两种做法只能选一种，方案没有选（R6）：

| 做法 | 快池 | 拒绝点 | 代价 |
| --- | --- | --- | --- |
| A. 锁内同步 Redis 提交（类似 strict） | 快池 worker 持锁等网络，属于**新的豁免**，要评审；Redis 切主时会等到超时（秒级），可能让快池饥饿 | 锁内，失败就在 Guard 内整体回滚，最干净 | 每笔持锁时间 ≥ 1 RTT + WAITAOF |
| B. 照 Remote 的做法：io 阶段提交，写闸门跨网络持有（推荐评估） | 快池不等；业务池只做准入与冻结，提交与确认在 io 阶段 | Lua 的拒绝发生在放锁之后：被拒的事务要像 Remote 持久拒绝那样，仅内存卸载并换代重载（`docs/framework/REMOTE_ENTITY.md` §持久拒绝；roost-coding“投影时被跳过 / 持久拒绝后的在线恢复”） | 闸门必须挡住**所有锁定该实体的事务**，不只是写它的事务（R3）；Sync 的 Confirm 要等确认之后 |

B 的拒绝窗口可以压到很小：执行前在 io 阶段先用 Lua 预检一次 fence / 版本，提交时再原子复核。预检通过之后，只有“期间执行权被接替”才会被拒，和今天租约过期被跳过的情况一样少见。

### 4.3 多实体准备（方案 §5.1）

- “不能持 A 的业务锁等 B 完成网络准备”在 B 做法下天然满足：所有准备都在 io 阶段，按稳定顺序进行，与 Remote 的 `prepareRemoteWriteBatch` 同一个位置（`nest_dispatch.go:104-108`）。
- 方案的“首版推荐”会让参与者全部进入 RedisActive，带来传染效应。建议协议写明：**只有原生步骤会主动触发 Entering**；普通 handler 碰到 RedisActive 实体又同时写 Local 实体时，首版直接返回可重试的拒绝，不自动传染。传染以后作为单独的优化项评估。
- Sync 冻结与交付：on_change 在成功准入后、放锁前冻结；全部锁释放并满足提交确认后才唤醒 Manager（roost-coding §Sync）。在 B 做法下，“满足提交确认”要改成“Redis 持久确认”。被拒时要沿用 Remote 拒绝的 Sync 规则（`SyncMutation.RejectEntities`，换代 Rebind）。

## 5. 漏项场景

| 场景 | 问题 | 建议写进协议的规则 |
| --- | --- | --- |
| Saga 补偿 / 迟到成功与段关闭交错 | 迟到成功会重开补偿（`docs/framework/SAGA.md:214-227`），补偿命令可能在实体已回到 Local 很久之后才到 | 每个原生步骤尝试（正向、补偿、迟到成功触发的补偿）都要经过 Entering；Closing 的条件加上“这个实体上没有已预留、未提交、租约未到期的步骤尝试”；被关闭段拒绝的尝试按可重试处理，交还租约 |
| 实体卸载 / 驱逐 | `entity.unload` 和跳过后驱逐都只等本进程投影（`entity_unload.go:110-118`、`fenced_step.go:108-196`） | RedisActive 实体首版拒绝 `entity.unload`，返回 `ErrEntityUnloadBusy`；被拒的事务仅内存卸载，重载时走第 5 条不变量的“Mongo + Redis 尾部”加载 |
| 删除 / 墓碑（D10） | 见 R14 | Redis 段的删除记录按软删墓碑投影；冷加载先查 Redis 段与 Remote 元数据；“删过档”的结论不能只看 Mongo |
| 合服（D15） | 见 R13 | sidmerge 前置检查增加：Redis 段全部关闭并投影完、协调器全终态、Store Service 积压为 0；首版事务域按承载 sid 划分，合服是离线操作，要求先排空 |
| Remote 共享模式 | 见 R16；心跳已禁止挂在共享实体上 | 迁移放到第 3 步，单独做故障注入；共享写的 fence 必须是持久且单调的（§7） |
| 心跳 / entitytimer 写 RedisActive 实体 | 见 R10 | 首版二选一：批量心跳遇到 RedisActive 实体时转成单目标作业（按 B 做法走 io 阶段）；或心跳回调禁止写持久字段，由维护者决定 |
| nopersist 字段 | 不受影响（第 3 节第 7 条） | 验收：在 RedisActive 期间重载后，派生值和 nopersist 字段重建 |
| 删档重建（D10，同 ID 重建） | 违反约束的新建实体，首个提交是“版本 0 的整文档 Put”，现在要到投影才发现冲突；Redis 段上同样要在 Lua 里拒绝，而不是到 Store Service 才 fatal | Lua 写入前检查 Remote 元数据墓碑与段内墓碑；Store Service 遇到版本冲突时暂停该段、报警（方案 §7 已写），并补一条：不能跳过 |
| DataEngine checkpoint 与 Redis 投影进度 | 两个进度彼此独立，又互相依赖（R4、R9） | Entering 等本地 checkpoint 覆盖；Closing 等 Redis 段投影进度等于提交进度，并写下“接续版本”；Game 启动时，Closing 之后的本地记录等 Redis 段投影完再回放 |
| 回执 TTL | 见 R5 | Redis 只留短窗（建议 ≥ 消息流 `MaxAge` 与 `AckWait × MaxDeliver` 中较小的那个有效窗口，由维护者定）；Mongo 留 720h；判定时先查 Redis、未命中再查 Mongo；只有在 Mongo 已有的前提下才删 Redis |
| 普通 effect | 见 R17 | RedisActive 实体上的全部 effect 进 Redis outbox，与 completion 同一套发布器 |

## 6. 往返次数审计

口径：一次往返指客户端发出到收到回复。Mongo 事务里每条命令一次，`commitTransaction` 一次（隐式会话不计）。“串行”指下一步依赖上一步的结果。计数按缺省配置：`CoordinatorBatch=3`、`PublisherBatch=1`（`framework/saga/engine.go:46`），实体一个 DAO（k=1），新的操作实例。重试、冲突、接替不计。

### 6.1 现状（源码计数）

**原生步骤，步骤侧（Game 进程）：**

| 环节 | Mongo | NATS | 是否串行 | 源码 |
| --- | ---: | ---: | --- | --- |
| Reserve 事务：读回执、读状态文档、插入状态文档、提交 | 4 | 0 | 串行（执行前必须拿到执行权） | `framework/saga/step_operation_inbox.go:140-258` |
| Nest handler + 本地 WAL fsync | 0 | 0 | 本地盘，约 5～8ms | `framework/nestwal/wal.go:314-360` |
| 投影事务：读标记、条件写租约、写 k 个 mutation、回执读+插、effect 读+插、插标记、提交 | 8+k = 9 | 0 | 串行；投影器对带回执 / effect 的记录**逐条**处理 | `framework/dataengine/engine/mongo_projection.go:27-160`；`framework/dataengine/engine/mongo_store.go:431-517` |
| outbox：Find 候选、FOAU 认领、发布等 PubAck、DeleteOne | 3 | 1 | 串行 | `framework/dataengine/engine/outbox_store.go:62-106`；`framework/dataengine/engine/outbox_worker.go:123-167` |
| 等回执（ack 路径，不在协调器关键路径上）：每个 PollInterval 读一次回执，再结算状态文档 | ≥2 | 0 | 后台，但占 Mongo 负载与 MaxAckPending | `framework/saga/dataengine_step_inbox.go:137-191`；`framework/saga/command_consumer.go:538-569` |
| **小计（关键路径）** | **16** | **1** | | |

**协调器推进一步：**

| 环节 | Mongo | NATS | 源码 |
| --- | ---: | ---: | --- |
| ClaimDue：FOAU 逐条领取，批量 3、只有 1 条到期时多一次空领 | 1～2 | 0 | `framework/saga/mongo_store.go:223-254` |
| 派发 Apply 事务：ReplaceOne、outbox DeleteMany、InsertOne、提交 | 4 | 0 | `framework/saga/mongo_store.go:261-371` |
| ClaimOutbox（批量 1）、发布等 PubAck、AckOutbox | 2 | 1 | `framework/saga/mongo_store.go:373-408`；`framework/saga/engine.go:867-926` |
| 命令经 JetStream 投递到步骤消费者 | 0 | 1 跳 | — |
| Complete：Get，加上 Apply 事务（读回执、读墓碑、ReplaceOne、插回执、插墓碑、删 outbox、提交） | 8 | 0 | `framework/saga/engine.go:438-500`；`framework/saga/mongo_store.go:261-371` |
| completion 经 JetStream 投递到结果消费者 | 0 | 1 跳 | — |
| **小计** | **15～16** | **1 + 2 跳** | |

- **每个原生步骤合计：Mongo 31～32 次、PubAck 2 次、JetStream 投递 2 跳，全部串行。** 两步交易再加上启动链（发起方投影暂存 effect、outbox 3 次加 PubAck、启动消费者 Create 1 次），约 65～70 次 Mongo。按 61ms 计约 4.0～4.3s；中途数据是 3.9s（约 64 次），一致。“50 多次”与本表的差别在于是否计入协调器和启动链，以正式报告的逐跳计数为准。
- **一个 Mongo 步骤**：步骤侧是 Reserve 4 次，加执行事务 k+3 次（业务写、条件写状态文档、插回执、提交，`framework/saga/command_consumer.go:150-188`），加 completion 的 PubAck 1 次，共 7+k = 8 次 Mongo + 1 次 NATS；协调器侧 15～16 次。每步合计约 23～24 次 Mongo。
- **投影吞吐**：带回执 / effect 的记录逐条投影（`docs/maintenance/KNOWN-LIMITS.md` §Saga），9 次往返 × 61ms ≈ 549ms，与中途数据“每条约 530ms”吻合。所以一个 Game 进程的原生步骤投影吞吐上限约为每秒 1.9 条——**这是并发 16 起出现 Failed 的直接解释，有待正式报告确认**。
- **Remote 写一次（strict，走 WAL）**：
  - 准入：Redis TryLock 1 次、Mongo GrantWrite 1 次、Redis refresh 1 次；
  - 投影事务：读标记 1、读 Remote 事务 1、元数据 BulkWrite 1、每个集合一次 BulkWrite（c ≥ 1）、插事务 1、插标记 1、提交 1；
  - 发布：L2 Eval 1、NATS 1、MarkPublished 1；
  - 解锁：Redis 1。
  合计约 Mongo 9+c、Redis 4、NATS 1（`framework/remoteentity/versioned_lock.go:112-182`、`framework/remoteentity/mongo_committer.go:167-202,364-380`、`framework/remoteentity/mongo_payload.go:14-60`）。

### 6.2 方案各阶段的往返估算

按每个 Lua 脚本与 WAITAOF 在同一连接上流水线发出、算作 1 次往返估算（WAITAOF 还要加上 fsync 和副本确认的时间，见 §7）。

| 阶段 | 每步往返（Redis / Mongo / NATS） | 与现状相比 |
| --- | --- | --- |
| 现状，原生步骤 | 0 / 31～32 / 2 + 2 跳 | — |
| 第 2 步：协调器迁 Redis（领取 1、派发 1、outbox 领取 + 确认 2、Complete 的 Get + Apply 2） | 6 / 16 / 2 + 2 跳 | 少约 10 次（约 30%）。步骤侧不变，因为收件箱留在 Mongo（R11） |
| 第 3 步：Remote 迁 Redis（取锁 + 许可合并 1、提交含快照 / 回执 / outbox 1、L2 同脚本或另 1、解锁或合并进提交 0～1） | 2～4 / 0 / 1 | Mongo 从 9+c 次全部移出关键路径 |
| 第 4 步：原生步骤走 RedisActive（Reserve 1、提交 1、completion 直接发布 1 NATS + 确认 1；首次进入另加段建立 1 和一次本地 checkpoint 等待） | 步骤侧 3 + 协调器 6 = 9～10 / 0 / 2 + 2 跳 | 少约 22 次（约 70%），首次进入例外 |

### 6.3 哪些必须串行，哪些可以合并

- **必须串行**：取得执行权在执行之前；执行在提交之前；提交确认在发布 completion 之前；completion 被接收在派发下一步之前。这四段是可靠性的骨架，任何方案都要保留。
- **可以合并成一次往返**：同一个事务里的多条写（Mongo 8.0 的跨集合 bulkWrite，或 Redis 的 Lua）；“读后插”的身份检查（可以用唯一索引冲突重跑代替，或用一次 `$in` 查询）；“接收结果”和“派发下一步”（同一份记录的连续两次状态转换）。
- **可以移出关键路径**：outbox 的 Find + 认领（提交后在进程内直接交接发布，outbox 只用于恢复）；等回执的轮询（同进程里有投影器的完成通知）。

### 6.4 第 1.5 步候选：不换存储，先减往返

每项都保持现有的持久语义（majority + j:true、事务、租约、幂等）。预期收益按 61ms × 省下的往返数估算，是关键路径上的理论值，以第 0 步的复测为准。

| 编号 | 改动 | 每步省下的往返 | 61ms 下每步约省 | 风险与需要的契约复核 |
| --- | --- | ---: | ---: | --- |
| O1 | **完成即派发**：Complete 的 Apply 事务里，直接把下一步写成 Waiting 并写入 outbox，省掉 ClaimDue 和单独的派发事务 | 4～5（事务里多 2 条写） | 0.25～0.3s，另外省掉协调器唤醒等待（本地均值 54ms） | Complete 没有协调器租约，只靠版本 CAS；要确认与重试退避、截止、迟到成功、ManualRequired 的规则不冲突（`judgeCompletion` 的统一规则） |
| O2 | **派发后直接发布**：派发事务插入 outbox 时就写好本进程的租约，提交后立即发布；发布器的轮询只用于恢复 | 1～2 | 0.06～0.12s | 发布器在租约到期后会重发（JetStream 按 MsgID 去重，收件箱幂等）；AckOutbox 可以并入下一次 Complete 的 `DeleteMany`（`framework/saga/mongo_store.go:364` 已经在关闭操作时删除），再省 1 次 |
| O3 | **Reserve 快路**：常见情况（还没有状态文档）用一次带条件的 upsert 完成；遇到冲突回退到现在的事务 | 约 3 | 约 0.18s | 要证明“没有状态文档就不可能已有本命令的回执”在 TTL 边界上也成立；需要契约复核 |
| O4 | **投影事务合并写**：mutation、回执、effect、标记用一次跨集合 bulkWrite 发出（服务端需 Mongo 8.0+，云库是 8.2.3）；“读后插”改为依赖开头的标记检查 + 唯一索引冲突重跑 | 9 → 约 4，省约 5 | 约 0.3s，**投影吞吐约翻倍** | 驱动版本是否支持跨集合 bulkWrite 需要核对；撞唯一键会中止事务，所以要保持“身份裁决在插入前”的语义（`framework/dataengine/engine/mongo_store.go:425-430` 的注释） |
| O5 | **effect 进程内交接**：投影提交后，把暂存的 effect 直接交给发布器，省掉 Find 和认领 | 2 | 约 0.12s | 交接丢失时靠轮询兜底；发布与确认的幂等不变 |
| O6 | **回执等待改成进程内通知，并在投影事务里结算状态文档**（租约条件写本来就写这份文档） | 关键路径 0，后台 ≥2 | 不缩短交易延迟，但降低 Mongo 负载与 MaxAckPending 占用 | 要改 fence 的 `Confirmation` 形状（`framework/dataengine/engine/mongo_store.go:378`）；需要契约复核 |
| O7 | **同地域部署**：Game、协调器与 Mongo 放在同一地域（RTT 从 61ms 降到约 1ms） | 全部往返 | 约 3.8s（两步交易） | 部署决定，不是代码改动。**第 0 步必须先测这一项**，否则无法判断 Redis 方案的收益 |

合计（O1～O5）：每个原生步骤约省 15～17 次往返，61ms 下约 0.9～1.0s；两步交易约从 3.9s 降到约 2s。第 4 步的 Redis 方案每步约省 22 次。也就是说，第 1.5 步不改变持久模型和 RPO，就能拿到 Redis 方案往返收益的约 2/3。投影吞吐（O4）是并发能力的第一瓶颈，第 1.5 步同样能直接改善。

## 7. 维护者需要拍板的选项

### 7.1 Redis 拓扑、确认条件与 RPO

现在的参照：Game 本地 WAL 在 strict 下进程崩溃 RPO 为 0；Game 主机或磁盘损毁时，会丢失尚未投影的 WAL。Saga 协调器与 Remote 在 Mongo（majority + j:true），单节点故障下 RPO 为 0。

| 选项 | 拓扑 | 确认条件 | RPO | 可用性 | 性能代价 |
| --- | --- | --- | --- | --- | --- |
| **D（推荐先做）** 不改权威 | Mongo 副本集（现状）+ 同地域部署 + 第 1.5 步 | 现状 | 现状 | 现状 | 每步省约 15～17 次往返 |
| A 单主 Redis | 单主，AOF `appendfsync always`；副本只做异步灾备，**不自动切换** | 同连接 `WAITAOF 1 0`（现有 `EvalBatchDurable` 支持） | 进程崩溃或重启：0。主机 / 磁盘损毁：丢失自上次异步复制或备份以来已确认的提交（秒级，无界）。人工提升副本按灾难恢复处理，要停写核对 | 主节点故障即全部 Redis 提交停写，直到人工恢复；不会出现 fence 回退（没有自动提升） | 每批 1 RTT 加 Redis 本机 fsync，可以组提交摊薄。比现在的 Game 本地 WAL 多一次网络往返，故障影响面从一个 Game 扩大到域内全部 Game |
| B Sentinel 自动切换 | 1 主 2 从，Sentinel，`min-replicas-to-write 1` | 同连接 `WAITAOF 1 1`（本机 fsync + 至少 1 个副本 fsync） | 单节点故障、且提升的是已确认副本：0。提升了未确认副本，或主与已确认副本同时故障：丢失尾部，fence 与段代际可能回退（`cluster_fault_integration_test.go:456-458` 已复现计数器回退）。必须另设持久单调的 epoch（放在 Mongo 或 etcd），每次切换后推进；所有提交都带 epoch，切换后旧 epoch 的段要核对后才能继续 | 自动切换（秒级），切换期间提交为“已执行、持久未确认”的未决 | 每批加 1 次副本往返和副本 fsync |
| B′ 同 B，等全部副本 | 同 B | `WAITAOF 1 2` | 单故障 0，双故障仍可能丢 | **任何一个副本不可用就停写** | 加最慢副本的往返 |
| C Cluster 多分片 | 每个分片同 B | 每个分片同 B | 同 B | 同 B，另有槽迁移窗口 | 需要新的驱动能力（`EvalBatchDurable` 现在拒绝 Cluster），事务必须单槽。**不建议首版** |

**推荐**：先做 D（第 0 步 + 第 1.5 步）。拿到同地域数据后，如果仍需要 Redis：协调器（第 2 步）用 B，加 Mongo 锚定的 epoch；Remote（第 3 步）**保留 Mongo 持久 fence 作为最终校验**（类似今天的做法），用 Redis 承载数据提交和日志，或者在接受 B 的双故障 RPO > 0 的前提下全部迁入 Redis。B 的“双故障可能丢已确认交易”是业务容忍度问题，必须维护者明确接受，不能由实施方默认。

### 7.2 其他需要决定的事项（各附推荐）

1. **持久级别在 Redis 链上的映射（R8）**。推荐：strict 和 pipelined 都等同连接的 WAITAOF 才对外可见；async 至少等 Lua 的结果（1 RTT，取得拒绝点）、不等 WAITAOF；memory 不变。代价是 async 写在 RedisActive 期间多 1 RTT。
2. **提交在哪里等网络（R6）**。推荐 B 做法：io 阶段提交，写闸门挡住所有锁定该实体的事务，被拒时仅内存卸载并换代重载。备选 A 做法：锁内同步等网络，需要新增快池豁免。
3. **传染（R7）**。推荐首版不传染：只有原生步骤触发 Entering，其他碰到 RedisActive 又写 Local 的事务返回可重试拒绝。
4. **心跳与定时器写 RedisActive 实体（R10）**。推荐：批量心跳遇到 RedisActive 实体时转成单目标、走 io 阶段；或者禁止在心跳里写持久字段，二选一。
5. **回执分层保留（R5）**。推荐：Redis 只保留短窗，Mongo 保留 720h；Redis 删除的前提是 Mongo 已有。
6. **saga ID 格式（第 2 步需要，见 §9.1）**。推荐：由框架统一铸造、内含分桶号；不再接受调用方自带的任意 ID。线上尚未部署，按不兼容处理。

## 8. 分期建议与验收门槛

| 步骤 | 内容 | 验收门槛（全部满足才进下一步） |
| --- | --- | --- |
| 0 测量 | 云库逐跳计数（§6.1 的表逐行对上）；**同地域**部署对照（Game / 协调器与 Mongo 同地域）；同地域下 Redis `WAITAOF 1 0` / `1 1` 与 Mongo majority 提交的延迟对照；投影器吞吐随 RTT 的变化 | 往返计数与源码审计的差异逐项解释；同地域与跨地域的 P50 / P99、成功 TPS、Failed 起点各至少两轮；原始样本进 `docs/performance/benchmarks/` |
| 1 协议契约 | 本篇 §3 / §4 / §5 的规则成文；§7 的选项拍板；第 2 步记录结构（§9）、状态转换表与崩溃恢复表评审通过；故障承诺写进现行文档草案 | 维护者确认 §7.1 选项与 §7.2 各项；崩溃恢复表的每一行都有对应的验收用例 |
| 1.5 减往返（可选，推荐） | §6.4 的 O1～O6 逐项实施，每项先红后绿，同机同配置前后对照 | 每项都有修前 / 修后对照；Saga 契约回归（`framework/saga` 全部测试与 completion 统一规则）全绿；原生步骤 15 分钟样本 P99 不退化。**按结果重新评估第 2～4 步是否还有必要** |
| 2 协调器 + Store Service | Redis `Store` 实现（`framework/saga/store.go:51-65` 接口不变）；Store Service 投影协调器记录、回执、墓碑；收件箱留在 Mongo | 现有 `Store` 契约测试全部改跑 Redis 实现并通过；§9.4 崩溃恢复表逐行用例；故障注入（主节点强杀、切主、网络分区、回复丢失）结果符合 §7 选定的承诺；30/s × 10 分钟正向与全补偿各两轮 |
| 3 Remote | 提交、ownership、共享模式迁移；保留或替换 Mongo 持久 fence 按 §7.1 决定 | `cluster_fault_integration_test.go` 两条用例在新协议下仍为绿，fence 不复用；`framework/remoteentity` 现有测试全部适配通过；共享模式迁移在真实 Redis 下无双推进 |
| 4 普通 Entity RedisActive | 状态机、路由、Entering / Closing、心跳、冷加载、重启顺序 | 方案 §10 验收矩阵全部通过，另加 R3 / R4 / R9 / R10 的专项用例（跨链只读依赖、Entering 期间崩溃、Closing 之后的本地记录在 Store Service 停机时重启、心跳批量路由）；原生步骤性能按方案 §10 分项报告 |

**对分期顺序的论证**：

- 第 0 步在前，是因为现有证据（调度实验无收益、往返审计）都表明，收益取决于部署距离和往返次数，而这两者还没有在同地域测过。
- 第 1.5 步放在第 2 步之前，是因为它不改 RPO，只改性能；如果它已经满足目标，第 2～4 步可以不做。
- 协调器先于 Remote 和普通 Entity：协调器有现成的 `Store` 接口，影响面最小，也能顺带验证 Redis 持久确认、Store Service、回执分层这几套共用机制。
- Remote 先于普通 Entity：Remote 已经有“io 阶段提交 + 写闸门 + finalizer + 持久拒绝后的恢复”这一整套机制，普通 Entity 的状态机正要借用它。
- 普通 Entity 最后：它是唯一会让同一个实体出现两条提交链的部分，风险最高（R3、R4、R9、R10）。

## 9. 第 2 步（Saga 协调器 + Store Service）记录结构草案

范围：只迁 `saga.Store`（协调器记录、命令 outbox、completion 回执、操作墓碑与迟到告警标记）。步骤收件箱（`_dataengine_step_operations`、`<收件箱>_operations`）与 DataEngine 回执**留在 Mongo**（R11）。

### 9.1 键设计与 hash tag

- **分桶**：桶号 `b = hash(Type + "\x00" + BusinessKey) mod B`，B 在部署时固定（建议 1024）；改 B 等于搬数据。一个 saga 的全部键都带 `{sg:<b>}` 标签，这样在单主 / Sentinel 下键名统一，以后上 Cluster 也能保证同一个 Apply 的键都在一个槽里。
- **saga ID**：由框架铸造为 `<b 的十六进制>-<随机或单调部分>`，Get / Complete 从 ID 直接解析出桶号；调用方自带的 ID 拒绝（§7.2 第 6 项）。

| 键 | 类型 | 内容 | 写入方 |
| --- | --- | --- | --- |
| `{sg:b}:rec:<sagaID>` | Hash | 记录各字段（与 `recordDoc` 同义）+ `version`、`lease_owner`、`lease_token`、`lease_until`、`start_digest`、`last_apply`（最近一次 Apply 的请求摘要，用于结果未知时查证） | Create / Apply / Claim |
| `{sg:b}:due` | ZSet | member = sagaID，score = `max(next_run_at, lease_until)` 毫秒；终态时移除 | Create / Apply / Claim |
| `{sg:b}:bk:<type>:<businessKey>` | String | sagaID。终态驱逐后仍保留到启动意图的最长重投窗口（effect 流 `max_age`，缺省 7 天） | Create |
| `{sg:b}:ob:<commandID>` | Hash | outbox 命令 + `next_attempt_at`、租约 | Apply / ClaimOutbox / Ack / Nack |
| `{sg:b}:obk:<operationKey>` | String | 当前排队的 commandID（实现“同一操作只留最新尝试”） | Apply |
| `{sg:b}:outbox` | ZSet | member = commandID，score = `max(next_attempt_at, lease_until)` | 同上 |
| `{sg:b}:cr:<commandID>` | String | completion 回执摘要；**Redis 短窗 TTL**，Mongo 长窗 | Apply |
| `{sg:b}:op:<operationKey>` | Hash | 墓碑：`saga_id`、`closure`、`late_alarms.r<代际>`；Redis 短窗，Mongo 长窗 | Apply / MarkLateSuccessAlarm |
| `{sg:b}:log` | Stream | 每次 Apply / Create 追加一条：after-image、回执、墓碑变化、`version` | 同一个 Lua |
| `{sg:b}:proj` | String | Store Service 已在 Mongo 提交到的 Stream ID | Store Service |
| `{sg:b}:epoch` | String | 桶的写代际，与 Mongo 中锚定的值一致（§7.1 选项 B） | 切换后的恢复流程 |

**容量预算**（按 30 笔/s 粗估）：非终态记录、outbox、`due` 与短窗回执是小常量。`bk` 保留 7 天约 1800 万个键，每个约 100B，约 1.8GB，必须纳入预算；可以缩短，但要同步约束启动意图的重投窗口。

### 9.2 事务身份、版本与 fence

- **记录版本**：Apply 的 `ExpectedVersion` 与 `ExpectedLease` 在 Lua 里比较，不匹配返回 `ErrConflict`，与 `store.go` 的契约相同。
- **请求摘要**：Lua 把 `last_apply = sha256(ApplyRequest)` 写进记录。回复丢失后，调用方用同一请求重试：如果记录已是 `ExpectedVersion+1` 且 `last_apply` 相同，就按已应用返回，不再报冲突。这一点需要新增，现在的 Mongo 实现依赖驱动重跑事务。
- **completion 回执**：按 `commandID + 摘要` 去重；摘要不同返回 `ErrIdentityConflict`。Redis 未命中时回查 Mongo（只发生在短窗过期之后）。
- **fence**：领取时 `lease_token` 加一；在选项 B 下，所有写都带桶的 `epoch`，旧 epoch 的请求一律拒绝。
- **Lua 规则**：全部校验（版本、租约、回执摘要、墓碑身份、键类型）在第一条写之前完成。校验之后的写只能因为内存或服务端错误失败；这类错误按“部分写入、停写该桶并报警”处理，不当作已回滚。

### 9.3 状态转换表（Redis 侧记录生命周期）

| 当前状态 | 事件 | 下一状态 | 原子写入（同一个 Lua） |
| --- | --- | --- | --- |
| 不存在 | Create（`bk` 不存在，或指向同一意图） | 活动 | `rec`、`due`、`bk`、`log` |
| 活动 | ClaimDue（到期且租约过期） | 活动（已领取） | `rec` 的租约字段、`lease_token+1`、`due` 的分数改为 `lease_until` |
| 活动 | Apply（派发） | 活动（Waiting） | `rec`、`due`；`ob`/`obk`/`outbox` 替换同一操作的旧尝试；`log` |
| 活动 | Apply（接收结果，可选连带派发下一步，O1） | 活动或终态 | `rec`、`cr`（短窗）、`op`（关闭）、清除该操作的 outbox；终态时移出 `due`；`log` |
| 终态 | Store Service 已投影越过它的 `log` 条目，且超过终态保留窗 | 已驱逐 | 删除 `rec`；保留 `bk`；回执和墓碑按各自的短窗过期 |
| 已驱逐 | 迟到的正向成功（需要重开） | 活动（重新装入） | 从 Mongo 读出记录，以 `version` 做 CAS 装入 Redis（不存在才写），再走 Apply |
| 任意 | `epoch` 不匹配 | 拒绝 | 不写 |

### 9.4 崩溃恢复表

| 崩溃 / 故障点 | 持久状态 | 恢复动作 | 不变量 |
| --- | --- | --- | --- |
| 协调器在 Lua 发出后、收到回复前崩溃 | 未知 | 新的领取者在租约过期后重新领取；如果 Lua 已执行，`version` 已加一，新领取者读到新版本并继续推进。原请求重试时靠 `last_apply` 查证 | 4 |
| Lua 已执行，WAITAOF 超时或确认数不足 | 已执行、持久未确认 | 返回“未决”：不报成功、不回滚；用同一请求重试 WAITAOF 只能在原连接上进行，换连接就要先回读状态再裁决。命令发布与 Complete 回复都不越过这个点 | 2、4 |
| 发布器在 PubAck 之后、Ack 之前崩溃 | outbox 仍在 | 租约过期后重新发布；JetStream 按 MsgID 去重，收件箱幂等 | 6 |
| Store Service 在 Mongo 提交后、更新 `proj` 之前崩溃 | Mongo 已有，`proj` 落后 | 从 `proj` 重放；Mongo 上按 `version` 判定：小于等于已存版本的条目跳过，相同版本时摘要必须一致 | 6 |
| Store Service 在 Mongo 提交前崩溃 | Mongo 没有 | 从 `proj` 重放 | 6 |
| Redis 主节点崩溃（选项 A，无切换） | AOF 已 fsync 的部分保留 | 重启、重放 AOF；未决的请求按查证处理 | 2 |
| Redis 切主，丢了已确认的尾部（选项 B 的双故障） | Redis 落后于“已确认”；Mongo 投影可能**领先**于 Redis | 检测：Store Service 发现 Mongo 版本大于 Redis 条目，或 epoch 不连续 → 该桶停写、报警；用 Mongo 中已投影的部分重新装入 Redis；未投影的已确认尾部按人工核对处理（这就是 RPO > 0 的具体后果） | 由 §7 的选择决定 |
| 驱逐与迟到成功并发 | — | 驱逐只删 `version` 等于已投影版本的记录（Lua CAS）；重新装入用“不存在才写”，冲突时重读 | 5、6 |
| 同一业务键的并发 Create，与驱逐交错 | — | `bk` 保留到重投窗口结束，Create 的 Lua 以它判定，不依赖“先查 Mongo” | 3 |

## 10. 方向判断

- **Saga 已知会反复出问题**：完成判定（已统一为 `judgeCompletion`）、认领争用（`9dd9806a`）、逐跳诊断（`0ff5014c`、`ee3aa6d1`）。这一轮的证据显示，延迟的主因是串行往返次数和部署距离，不是存储引擎。上一轮的 Redis 调度索引实验已经证明，只换调度不降延迟。
- **历史上有过 Redis 提交日志**（遗留 checkpoint 包里的 `redis_wal.go`，带 TTL，于 `66cfea12` 删除），之后改为本地文件 WAL + 投影。方案部分回到 Redis 日志，应该在协议里写明这次与那次的不同：持久确认、回收规则、单一提交链。
- **建议的方向调整**：先测（同地域）、再减往返（第 1.5 步）。只有在 Mongo 链路里结构上省不掉、而交易延迟目标确实需要的地方，才引入 Redis 权威。代价是第 2～4 步可能推迟或缩小；好处是不改变 RPO，不新增第二条提交链。

## 11. 尚未验证与限制

- 本篇所有往返计数来自读源码，没有运行。云库正式报告 `SAGA-CLOUD-MONGO-20261026.md` 收尾时还没写出，**待云库数据**。它需要回答：
  1. §6.1 的逐跳计数与实测往返数是否逐项一致；
  2. 投影事务的耗时拆分，以及投影器吞吐上限是否就是并发 16 起出现 Failed 的原因；
  3. 同地域部署下，残余延迟主要来自服务端写入（4～5ms × 次数），还是来自 JetStream 投递和协调等待；
  4. 同地域下 Redis `WAITAOF 1 0` / `1 1` 与 Mongo majority 提交的延迟对照；
  5. 云 Mongo 的 CPU、磁盘与复制指标；
  6. Failed 的分类：超时、写冲突，还是结果未知。
- §6.4 每项的收益都是 RTT 乘以次数的理论值，没有考虑服务端时间与并发排队；O3、O4、O6 都需要契约复核后才能实施。
- 本篇没有覆盖 Linux 上的行为，也没有跑任何压测或故障注入。
