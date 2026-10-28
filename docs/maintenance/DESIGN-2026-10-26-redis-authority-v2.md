# Redis 做 Saga / Remote 权威：设计 v2（2026-10-26）

> **路线已放弃（维护者 2026-10-28）**：放弃 Redis 权威路线，只保留 JetStream 版本（[原型稿 §17 第 12 条](PROPOSAL-2026-10-27-saga-jetstream-log.md)）。
> 本文第 2 步的 2a（`6709eb90`）、2b（`a3edef04`）在 main 上，待 JetStream 协调器实施时与 Store Service、`saga.redis.*` 配置同一批删除，删除前继续可用；
> 2c 没有合入，分支已删除，提交在本地 tag `archive/redis-authority-2c` 归档。第 3、4 步不再做。替换设计见
> [DESIGN-2026-10-28-saga-jetstream-authority](DESIGN-2026-10-28-saga-jetstream-authority.md)。本文保留为设计记录。

**状态：第 2 步 2a / 2b 已实施（分支 `feat/redis-authority-2ab`，未合入 main、未发版），2c 与第 3、4 步未实施。维护者 2026-10-26：“直接从 V2 开始吧，之后跑一次 saga 的压测”——按本文直接实施，不再单独评审一轮。** 本文中标“已采纳”的做法就是实施依据；真正还要维护者做产品选择的只有 §11 的三项（实施时按推荐先做，见 §13）。2a / 2b 的现行契约已迁入 `docs/framework/SAGA.md`「协调器存储：Redis 权威」与 `docs/framework/impl/06-saga.md`；实施结果、与本文的偏差、红绿证据与压测见 §13。本文保留为设计记录。

本文自成一体，接手人不需要先读原草稿。依据：

- **原草稿**：主检出未提交的 `docs/framework/REDIS-COMMIT-AND-STORE-SERVICE.md`（另一会话所写，不在仓库提交里）。本文吸收了它的方向、不变量、普通 Entity 状态机与验收矩阵（下文“草稿 §N”）。
- **审查**：[PROPOSAL-2026-10-26-redis-authority-review.md](PROPOSAL-2026-10-26-redis-authority-review.md)（`5b6b846d`，下文“审查 §N”）：R1～R18 问题清单、§6 往返审计、§8 分期门槛、§9 第 2 步记录结构草案。
- **第 0 步测量**：[SAGA-CLOUD-MONGO-20261026.md](../performance/SAGA-CLOUD-MONGO-20261026.md)（`30243c9b`，下文“报告 §N”）：本机 / 云 Mongo 闭环并发扫描、阶段拆分、往返实测、领取查询 explain、本机 Redis AOF 提交代价。
- **源码基线**：main `30243c9b`。它与审查基线 `5b6b846d` 之间只改了 Saga 的测试夹具与 `scripts/perf/*`，产品源码没有变化；本文行号按 `30243c9b` 读出。codebase-memory 图谱的根是主检出，结论以 worktree 源码为准。

## 0. 要点

1. **目标是减少每笔交易的存储操作次数，并把吞吐上限从“串行池 × 往返”里放出来。** 维护者判断：TPS 低不能只归因于云库延迟，Mongo 操作次数本身过多也是问题。报告 §0 证实两边的吞吐上限都在框架的串行池，不在 Mongo 服务端（本机 Saga 压满时 Mongo 每秒只提交约 350 个事务，单文档事务天花板 6888/s）。所以 v2 同时改两件事：**存储换成“一笔事务一个 Lua”**，以及**协调器、发布器、结果消费者、Store Service 的并发模型**。只换存储不改并发，积压只会从一个池转到另一个池（§3.3）。
2. **拓扑按单主处理，不考虑切主（维护者 2026-10-26 决定）。** 持久确认在主节点上：`appendonly yes` + `appendfsync always`，每个写脚本与 `WAITAOF 1 0 <timeout>` 在同一连接流水线发出。副本只做灾备，不参与确认、不自动切换。主机或磁盘损毁属于灾备，由运维的备份 / 恢复流程处理，不在框架承诺内（§2、§4）。审查 R1（切主时 fence 回退）与“在 Mongo / etcd 锚定 epoch”不在首版范围。
3. **第 2 步（Saga 协调器 + Store Service）写到可以直接开工**（§7）：键、Lua 脚本的输入输出与校验顺序、Go 落点、状态转换表、崩溃恢复表、投影协议、回收、并发模型、指标、配置、红测试清单、验收命令。第 2 步分三段交付：2a 存储替换（引擎不动），2b Store Service，2c 引擎减往返（完成即派发、派发后直接发布、批量派发 / 确认）。
4. **单主下不需要 hash tag 和分桶，saga ID 也不用改格式。** 原子性对单主天然成立；只保留 P 个“投影分区”（缺省 16）给 Store Service 并行投影与回收用（§7.3）。审查 §7.2 第 6 项（框架铸造带桶号的 saga ID）因此撤销。
5. **Redis 只保存“热”数据，Mongo 保存“冷”数据。** 非终态 saga 与刚结束的 saga 常驻 Redis；终态并投影完、过了保留窗的 saga 从 Redis 驱逐，之后 Mongo 副本就是它的权威。冷记录要写时先装回 Redis（带版本的原子交接）。回执与墓碑随 saga 一起驻留、一起驱逐，Mongo 保留 720h（R5）。
6. **Store Service 必须分区并行、批量写，不能逐条事务。** 报告 §4.2：本机原生步骤 83% 的时间在等逐条投影的 DataEngine 投影器。Store Service 若照搬逐条事务，持续吞吐上限不变。v2 的投影是“每分区每批 ≤ 3 次 bulkWrite + 1 次进度脚本”，版本单调、幂等、无 Mongo 事务；本机目标 ≥ 5000 条日志/s（§7.9）。
7. **第 2 步对原生步骤吞吐没有帮助，这一点要预先说清。** 原生步骤的上限是步骤侧的 DataEngine 投影器（本机约 10 TPS），第 2 步不动步骤侧（R11）。第 2 步之后的 Saga 压测，原生场景只验“不退化 + 协调段 Mongo 操作归零 + 延迟下降”，Mongo 步骤场景验吞吐（§6、§8）。

## 1. 目标与范围

### 1.1 目标

- **主指标**：每笔交易的存储操作次数（Mongo 往返、Redis 脚本往返分开计），见 §3。
- **次指标**：本机同机同配置的成功吞吐（闭环饱和点）与 P50 / P99，见 §6、§8。
- **不变的承诺**：已确认交易在进程崩溃 / 重启下不丢（RPO 0）；未知不等于失败；同一操作至多一次生效；先恢复再服务；先保存后回收（草稿 §3 的 7 条不变量全部保留）。

### 1.2 范围

| 对象 | v2 的权威位置 | 分期 |
| --- | --- | --- |
| Saga 协调器（记录、领取、outbox、completion 回执、操作墓碑、迟到告警标记） | Redis；Mongo 为冷副本 | 第 2 步 |
| Saga 步骤收件箱（原生步骤 `_dataengine_step_operations`，Mongo 步骤 `<收件箱>_operations`） | 原生：第 4 步前留在 Mongo；Mongo 步骤：永远留在 Mongo（R11） | 第 4 步（原生） |
| Remote Entity（数据、版本、ownership / grant / fence、回执、outbox） | Redis；Mongo 为异步副本 | 第 3 步 |
| 普通 Entity | 本地 WAL + DataEngine（不变）；只有进入 Saga 提交阶段的实体走 RedisActive | 第 4 步 |
| 用户自己的 Mongo 业务事务步骤 | 不迁移 | — |

不在范围：Redis Sentinel / Cluster、自动切主、跨地域部署、同地域对照测试（维护者 2026-10-26：云库不是线上部署，也不能和 Mongo 放同一网段，同地域对照不做）。

## 2. 拓扑假设与边界（维护者 2026-10-26 决定）

- **单主**：一个 Redis 主节点承担全部权威写与确认。线上不会出现主从切换。
- **副本只用于灾备**：`replicaof` 异步复制，自己开 AOF；不进入 `WAITAOF` 的副本计数（`numreplicas = 0`），不配 Sentinel，`min-replicas-to-write 0`。
- **框架承诺**：Redis 进程崩溃、重启、`kill -9`、Game / 协调器 / Store Service 崩溃时，已确认（脚本回复 + `WAITAOF` 本地确认 = 1）的交易不丢、不重复生效。
- **框架不承诺**：Redis 主机或磁盘损毁。此时人工提升副本或从备份恢复，丢失自上次复制 / 备份以来已确认的提交（RPO = 复制滞后，无界）。这是运维灾备流程，按 §7.8 末行的手册执行；框架只提供“空库不自动当权威”的启动守卫（§7.6.6）。
- **假设被打破时的表现**：如果有人在线上配了自动切主，fence 与版本可能回退（审查 R1 已复现计数器回退：`framework/remoteentity/cluster_fault_integration_test.go:317-470`），框架不能发现。所以启动探针拒绝 Cluster 与 `role:slave`，并在文档与 `doctor` 里写明“不支持 Sentinel 自动切换”。

## 3. 每笔存储操作次数：基线与目标

### 3.1 基线（源码审计 = 实测）

口径同审查 §6.1：一次往返 = 客户端发出到收到回复；Mongo 事务里每条命令一次、`commitTransaction` 一次；缺省配置（`CoordinatorBatch=3`、`PublisherBatch=1`），实体 1 个 DAO（k=1），不计重试 / 冲突。报告 §4.3 用云上 60ms RTT 逐跳实测核对，**逐项一致**：领取约 2、派发事务约 4、步骤预留约 4、投影事务约 9、DE outbox 认领约 2、完成推进 Get 1 + Apply 约 7。

| 事务类型 | 现状 Mongo（关键路径 / 后台） | Redis | NATS | 来源 |
| --- | --- | --- | --- | --- |
| 原生步骤（每步，含协调器推进） | 31～32 / 等回执轮询 ≥2 | 0 | PubAck 2 + 投递 2 跳 | 审查 §6.1；报告 §4.3 |
| 　其中协调器部分 | 15～16（领取 1～2、派发 4、outbox 领取 + 确认 2、Get 1 + Apply 7） | 0 | 1 + 2 跳 | 同上 |
| 　其中步骤侧 | 16（预留 4、投影 9、DE outbox 3） | 0 | 1 | 同上 |
| Mongo 步骤（每步） | 23～24（协调器 15～16 + 步骤侧 7+k=8） | 0 | 1 + 2 跳 | 审查 §6.1 |
| 启动 saga | 1（Create 插入） | 0 | — | `framework/saga/mongo_store.go:104-113` |
| Remote strict 写一次 | 9+c（c = 涉及的集合数） | 4 | 1 | 审查 §6.1 |
| 普通 Entity 写一次（Local，不在 Saga 中） | 关键路径 0（本地 WAL fsync）；后台投影：单 mutation 快路 1，带标记事务 k+3 | 0 | 0 | `framework/dataengine/engine/mongo_projection.go:41-45,82-140` |

两步原生交易约 63～65 次 Mongo 往返；报告 §4.3 按约 58ms 计约 3.7s，与云上 c1 服务端 P50 3.34s 吻合（交接有交叠）。

### 3.2 v2 目标（验收指标）

“关键路径”= 交易终态之前必须串行完成的操作；“后台”= Store Service 投影，按批摊薄，不在交易延迟里。

| 事务类型 | 第 2 步 2a/2b 后 | 第 2 步 2c 后 | 第 3 步后 | 第 4 步后 |
| --- | --- | --- | --- | --- |
| 原生步骤：协调器部分 | Mongo 0；Redis 脚本 5（领取 1、派发 1、outbox 领取 1、Get 1、Apply 1）+ 后台确认 1 | Mongo 0；Redis 2（Get 1、接收并派发下一步 Apply 1）+ 后台确认 1（批量） | 同 2c | 同 2c |
| 原生步骤：步骤侧 | 不变：Mongo 16 | 不变：Mongo 16 | 不变 | Mongo 0；Redis 2（预留 1、提交 1）+ completion 发布后确认 1（批量）；首次进入另加段建立 1 + 一次本地 checkpoint 等待 |
| 原生步骤合计 | Mongo 16 + Redis 5～6 | Mongo 16 + Redis 2～3 | 同 2c | **Mongo 0 + Redis 4～6**（现状 Mongo 31～32） |
| Mongo 步骤合计 | Mongo 8 + Redis 5～6 | Mongo 8 + Redis 2～3 | 同 2c | 同 2c（步骤状态永远在 Mongo，R11） |
| 启动 saga | Mongo 读 1 + Redis 1 | 同 2a | 同 2a | 同 2a |
| Remote strict 写一次 | 不变 | 不变 | Mongo 0；Redis 2（准备：锁 + 许可 + fence 1；提交：数据 + 版本 + 回执 + outbox + 解锁 1）；NATS 1 | 同第 3 步 |
| 普通 Entity RedisActive 写一次 | — | — | — | Mongo 0；Redis 1（提交脚本 + 同连接 WAITAOF）。**比 Local 多 1 次 Redis 往返**，只作用于进入 Saga 提交阶段的实体 |
| 后台投影（每条 Redis 日志） | 批量为 1 时 ≤ 3 次 bulkWrite；满批（256 条）时均摊 ≤ 0.02 次 | 同 2a | 同 2a | 同 2a |

计数规则：每个写脚本与它的 `WAITAOF` 在同一连接流水线发出，算 1 次往返（§4）；Get 是只读 `HMGET`，算 1 次往返。验收按 §8.4 的测量方法核对，与本表差 >10% 的项要逐项解释。

### 3.3 为什么必须同时改并发模型

报告 §2、§4 的结论：每个池的吞吐 ≈ worker 数 ÷（每项串行往返数 × RTT + 服务端耗时）。

- 原生步骤：上限是 DataEngine 投影器（逐条处理带回执 / effect 的记录，本机约 100ms/笔 → 约 10 TPS）。
- Mongo 步骤：本机 worker 2 卡在步骤消费者，worker 16 卡在发布器（4 个 worker 串行“领取 → PubAck → 删除”）；云上 worker 2 卡在结果消费者，worker 16 卡在协调器。worker 2 → 16 吞吐涨 3.2 倍（报告 §3.2）。

所以 v2 对每个池写明并发模型（§7.10）：

| 池 | 现状 | v2 |
| --- | --- | --- |
| 协调器 | 4 worker，每个 worker 领 3 条后逐条串行派发（每条一次 Mongo 事务） | 2a：领取一个脚本取一批（O(log N)）；2c：一批的派发合成一次 `EvalBatchDurable`（一次往返、一次 fsync） |
| 发布器 | 4 worker，每条串行领取 → PubAck → 删除 | 2a：领取一个脚本取一批；2c：派发后在进程内直接交给发布器（不领取），PubAck 并发等待，确认批量提交 |
| 结果消费者（saga 结果流、原生 completion 效果流） | 串行（wiring 没传 `Workers`；`NestCompletionConsumerConfig` 没有 `Workers` 字段） | 2a：可配置并发，缺省 8；`Engine.Complete` 本来就按版本 CAS 重试，可以并发 |
| 启动消费者 | 串行 | 2a：可配置并发，缺省 4 |
| Store Service | （新） | 每个投影分区一个循环，P 个分区并行；每批 ≤ 256 条 |
| Redis 连接 | 缺省池 10 | 权威 Redis 连接池缺省 64，Init 校验 ≥ 各池并发之和（§7.10） |

Redis 服务端的 `appendfsync always` 会把同一轮事件循环里所有连接的写合成一次 fsync（报告 §6.1：c1 每秒 242 次、c64 每秒 7424 次）。只要客户端有足够的并发连接，服务端就自然组提交；2c 的批量只是再省客户端往返。

## 4. 持久确认参数（单主）

### 4.1 推荐配置（已采纳）

| 项 | 值 | 理由 |
| --- | --- | --- |
| Redis 版本 | ≥ 7.2（本机 8.10.1） | `WAITAOF` 7.2 引入；脚本 shebang 标志位 7.0 引入 |
| `appendonly` | `yes` | 没有 AOF 就没有持久确认 |
| `appendfsync` | `always` | 见 §4.2 |
| `no-appendfsync-on-rewrite` | `no` | `yes` 会在后台重写期间跳过 fsync，形成持久空洞 |
| `aof-use-rdb-preamble` | `yes`（缺省） | 只影响重写后的加载速度 |
| `aof-load-truncated` | `yes`（缺省） | 崩溃时最后一条不完整的写被截掉；脚本效果以 MULTI/EXEC 写入 AOF，加载时不完整的事务整段回退，所以一个脚本的写在崩溃后要么全在、要么全不在（§7.14 的 `kill -9` 用例验证这一点） |
| `maxmemory` | 显式设定，留 30% 余量 | 容量预算见 §7.3.4 |
| `maxmemory-policy` | `noeviction` | 权威数据不能被淘汰；OOM 时写脚本在执行前被拒（脚本带 `#!lua` 头、不声明 `allow-oom`），属于“确定没执行” |
| `save` | 可关（`""`） | 持久靠 AOF；全量同步副本时 Redis 仍会临时生成 RDB |
| 副本 | 0～1 个异步副本，开 AOF；不配 Sentinel | 仅灾备 |
| 写脚本后的确认 | 同一物理连接流水线 `WAITAOF 1 0 <waitaof_timeout>`，缺省 timeout 1s | 见 §4.3 |

### 4.2 为什么是 `always` 而不是 `everysec`

- **`always` 下回复晚于 fsync**：Redis 在 `beforeSleep` 里先写 AOF 并 fsync，再向客户端写回复。所以任何客户端读到的数据都已经落盘——**没有读者能看到未落盘的状态**。这条性质让“发布器读到 outbox 就发布”“协调器 Get 到的版本”都不需要额外等确认。
- **代价已实测**：本机 AOF always 单脚本（HSET + XADD）c1 P50 4.0ms / P99 7.8ms，c16 每秒 1916 次，c64 每秒 7424 次（报告 §6.1）；同盘 Mongo 单文档事务 c1 10.7ms（报告 §2）。一次脚本提交比一次 Mongo 事务快约 2.7 倍，并且一次脚本替代的是 4～8 次 Mongo 往返。
- **`everysec` 不采用**：回复先于 fsync，其他客户端会读到未落盘的状态；`WAITAOF 1 0` 要等后台下一次 fsync（周期约 1s），延迟由后台周期决定。即使只在写入方等 `WAITAOF`，也挡不住读者先看到未落盘数据。

### 4.3 `WAITAOF` 的作用与结果分类

`always` 下脚本回复时本地 AOF 已经 fsync，同连接的 `WAITAOF 1 0` 会立即返回 `local = 1`，不增加往返（流水线）。保留它是为了**在每次提交上断言部署满足持久契约**：

| `WAITAOF` 结果 | 含义 | 处理 |
| --- | --- | --- |
| `local = 1` | 已确认 | 正常 |
| `local = 0`（超时） | 部署违反契约：AOF 被关、`appendfsync` 被改、`no-appendfsync-on-rewrite yes` 遇上重写 | 返回 `ErrDurabilityUnconfirmed`（结果“已执行、持久未确认”，不回滚、不报成功）；计 `saga.redis.waitaof_unconfirmed_total`；向 `app.RuntimeFailure.Fail` 报告，进程 fail-stop |
| `WAITAOF` 报错 `appendonly is disabled` | 同上 | 同上；启动探针会先拦住（§7.6.6） |
| 连接错误 / 读超时 | 脚本结果未知 | 按驱动契约“结果未知”处理：同一请求重试，靠 `la` 摘要查证（§7.5.3） |

`WAITAOF` 必须与脚本在同一物理连接上：现有 `EvalBatchDurable` 满足（`infra/storage/redis/driver/client.go:382-422`，用 `client.Conn()` 独占连接流水线发出），它拒绝 Cluster 客户端（`:388-391`）。`EvalReplicated`（WAIT）不用于权威提交（R12）。

### 4.4 持久级别映射（R8，已采纳；第 4 步用）

| Nest 持久级别 | Local 实体（不变） | RedisActive 实体 |
| --- | --- | --- |
| strict | 锁内等本地 WAL fsync | io 阶段提交脚本 + 同连接 WAITAOF，写闸门挡住该实体直到确认 |
| pipelined | 锁内入队、锁外等票据 | **按 strict 处理**：跨两条链没有“单日志前缀落盘”性质（R3），不能锁外等 |
| async | 只等写入、不等 fsync | **按 strict 处理**：Lua 的校验结果至少要 1 RTT 才拿到，否则没有拒绝点；`always` 下拿到回复即已 fsync，没有额外代价 |
| memory | 带 effect 升级为 strict；写持久字段拒绝 | 不变 |

## 5. 审查 R1～R18 的处理

| 编号 | v2 的处理 | 落在 |
| --- | --- | --- |
| R1 切主时 fence 回退 | **不在范围**（维护者 2026-10-26：线上不会出现主从切换）。边界与假设被打破时的表现见 §2；启动探针拒绝 Cluster 与非主节点 | §2、§7.6.6 |
| R2 往返次数 | **已采纳**：一笔事务对应一个 Lua，数据、版本、回执、outbox、completion 意图、日志一次原子写入；提交前不额外读（版本 / 租约 / 回执校验都在脚本里）；脚本与 WAITAOF 同连接流水线，算 1 次往返。目标次数见 §3.2 | §3.2、§7.5 |
| R3 两条链的 pipelined 前缀性质 | **已采纳**：涉及 RedisActive 实体的事务首版不走 pipelined（按 strict）；写闸门挡住所有**锁定**该实体的事务（含只读参与者），直到它的 Redis 提交确认，所以任何事务都读不到未确认状态 | 第 4 步，§9.2 |
| R4 首次进入的崩溃窗口 | **已采纳**：Entering 等到“该实体最后一条本地记录已被 checkpoint 覆盖”（给每个实体登记最后 LSN，等全局 ack 水位越过它），不是只等投影完成；交接点（base version、本地 LSN）持久写进 Redis 段记录 | 第 4 步，§9.2 |
| R5 回执保留期 | **已采纳**：回执分层。Redis 里的回执 / 墓碑作为 saga 记录的字段，随记录驻留、随记录驱逐（驱逐前提是已投影）；Mongo 保留 720h（`completion_receipt_ttl` 不变）；查询先 Redis、未命中再 Mongo | §7.3、§7.9.5 |
| R6 提交在哪里等网络 | **已采纳**：Nest 侧沿用 Remote 的形状——io 阶段准备、业务池执行、io 阶段提交，写闸门跨网络持有；快池不等网络。被 Lua 拒绝的事务按 Remote 持久拒绝处理（仅内存卸载、换代重载）。协调器 / Store Service 不在 Nest 上，不受此约束 | 第 3、4 步 |
| R7 多实体准备与传染 | **已采纳**：只针对显式声明的目标在 io 阶段准备；只有原生步骤主动触发 Entering；普通 handler 同时碰到 RedisActive 实体与写 Local 实体时，在 WAL 准入前返回可重试拒绝，不传染 | 第 4 步 |
| R8 持久级别映射 | **已采纳**：§4.4 | §4.4 |
| R9 冷加载与恢复顺序 | **已采纳**：有 Redis 段的实体冷加载 = Mongo 底版 + 按序回放段尾，不要求 Store Service 在线；Closing 之后的本地 WAL 记录带“接续版本”，Game 启动回放它们之前先等对应段投影到接续版本；RedisActive 实体拒绝 `entity.unload`（`ErrEntityUnloadBusy`）。Saga 协调器的冷读 / 装回见 §7.6.4 | §7.6.4、第 4 步 |
| R10 心跳写 RedisActive 实体 | **已采纳审查推荐的第一种**：批量心跳遇到 RedisActive 实体时转成单目标作业，按 R6 走 io 阶段提交。不采用“禁止心跳写持久字段”（那是对业务的新限制）。热点实体因此可能长期留在 RedisActive，这是草稿 §4.3 允许的 | 第 4 步 |
| R11 第 2 步只迁协调器 | **已采纳**：第 2 步只迁 `saga.Store`；原生步骤收件箱第 4 步之前留在 Mongo；Mongo 步骤的状态文档与业务写同一个 Mongo 事务，永远留在 Mongo | §7.1 |
| R12 驱动与分区 | **已采纳**：单主，不用 Cluster 提交原语；写脚本走同连接 WAITAOF；不用 `EvalReplicated`。单主下不需要 hash tag，saga 键用配置前缀；分区只用于投影（§7.3.1）。驱动补齐 EVALSHA 批量与流读取命令（§7.2） | §4.3、§7.2、§7.3 |
| R13 合服 | **已采纳**：sidmerge 前置检查增加——权威 Redis 里没有非终态 saga 记录、所有投影分区积压为 0（第 3、4 步再加 Remote 投影积压为 0、Redis 段全部关闭）。`listNonTerminalSagas` 改读 Redis（`wiring/sidmerge/migrate.go:396-409`）。合服是离线操作，要求先停协调器 | §7.13.3 |
| R14 墓碑 | **第 2 步**：saga 操作墓碑随记录驻留、投影到 Mongo，closure 只升不降（abandoned → result）。**第 3、4 步**：删除与 Remote“删过档”元数据先落 Redis，冷加载先查 Redis 再查 Mongo；Lua 写入前检查墓碑，拒绝“版本 0 的整文档 Put”；Store Service 按软删墓碑投影，冲突暂停不跳过 | §7.9.3、第 3、4 步 |
| R15 补偿 / 迟到成功与段关闭 | **已采纳**：每个原生步骤尝试（正向、补偿、迟到成功触发的补偿）都经过 Entering；Closing 条件加上“没有已预留、未提交、租约未到期的步骤尝试”；撞上已关闭段的尝试按可重试处理并交还租约 | 第 4 步 |
| R16 Remote 共享模式 | **已采纳**：EnterShared / LeaveShared / Transfer 与多写者仲裁改成等价 Lua；单主 + `always` 下 fence 在进程崩溃 / 重启时不回退；灾备恢复后由运维执行“fence 代际加一”（§9.1） | 第 3 步 |
| R17 普通 effect | **已采纳**：RedisActive 实体上的全部 effect（`EmitStart`、邮件等）进 Redis outbox，与 completion 同一套发布器 | 第 4 步 |
| R18 与已删除的 Redis WAL 的区别 | 见 §10.1 | §10.1 |

## 6. 分期与验收门槛

性能门槛一律是**本机、同机、同配置、与当前 main 交替对照**（§8），以“每笔操作次数”和“本机吞吐”为主。门槛不达标时记录并分析，不放宽门槛过关。

| 步骤 | 交付物 | 测试矩阵 | 门槛（全部满足才进下一步） |
| --- | --- | --- | --- |
| 0 测量 | 已完成：报告 `30243c9b` | — | — |
| 1 协议 | 本文 | — | 本文提交即完成（维护者：按 v2 直接实施） |
| 1.5 Mongo 链路减往返（可选，见 §6.1） | O3 / O4 / O5 / O6 | 每项先红后绿，同机前后对照 | 每项修前 / 修后对照；`framework/saga`、`framework/dataengine` 全绿 |
| **2a 存储替换** | 权威 Redis Mod；`RedisStore`（实现 `Store`、`CompletionHistoryStore`、`LateSuccessAlarmStore`）；Lua 脚本；结果 / 启动消费者并发；启动探针；`MongoStore` 收缩为冷副本 | §7.14 的 2a 用例；现有 `framework/saga` 契约用例改跑 Redis 实现 | 单元 + 集成（真实 Redis AOF + Mongo）全绿含 `-race`；`kill -9` Redis 用例通过；§8 压测：协调段 Mongo 操作 = 0；Redis 脚本数与 §3.2 一致（±10%）；Mongo 步骤 w16 饱和吞吐 ≥ 1.5 × 基线；Mongo 步骤 c1 P50 ≤ 0.85 × 基线；原生正向 / 补偿饱和吞吐 ≥ 0.95 × 基线；非过载档终态与账户核对全部通过 |
| **2b Store Service** | `framework/storeservice` 投影运行器；saga 投影（Mongo 冷副本）；驱逐与冷读 / 装回；`wiring/storeservice` Mod | §7.14 的 2b 用例；冷热一致性核对 | 投影吞吐本机 ≥ 5000 条/s（P=16、批 256）；压测期间积压不超过 2 × 在途（按笔计：未投影日志涉及的 saga 笔数 ≤ 2 × 在途笔数；没有按笔指标时用“日志条数 ÷ 每笔日志条数”折算，维护者 2026-10-27 确认），停止加压后 30s 内归零；冷副本核对 0 差异；迟到成功在驱逐后能装回并补偿 |
| **2c 引擎减往返** | O1 完成即派发；O2 派发后直接发布；批量派发；批量确认 | §7.14 的 2c 红测试 | 协调器关键路径 Redis 脚本 ≤ 2 / 步（首步除外）；Mongo 步骤 w16 饱和 ≥ 2 × 基线；c1 P50 ≤ 0.7 × 基线；Saga completion 统一规则用例全绿 |
| 3 Remote | Remote 提交、ownership、共享模式迁 Redis；Store Service 加 Remote 投影 | 草稿 §10 的 Remote 行；`framework/remoteentity` 全部适配；`kill -9` Redis 下 fence 不复用 | Remote strict 写关键路径 Mongo 0、Redis 2；`scripts/perf/remote.sh` 吞吐 ≥ 基线、P99 不退化 |
| 4 普通 Entity RedisActive | 状态机、路由、Entering / Closing、心跳、冷加载、重启顺序；步骤收件箱迁 Redis | 草稿 §10 全部 + R3 / R4 / R9 / R10 / R15 专项 | 原生步骤关键路径 Mongo 0、Redis 4～6；原生正向饱和 ≥ 3 × 基线（投影器移出关键路径）；Entering 等待分布单独报告 |

### 6.1 第 1.5 步与 Redis 方案的关系

审查 §6.4 的 O1～O7 在 v2 下的去向：

| 编号 | 改动 | v2 下 |
| --- | --- | --- |
| O1 完成即派发 | 引擎层，与存储无关 | **并入 2c**（在 Redis 上做），不在 Mongo 上单独做 |
| O2 派发后直接发布 | 引擎层 | **并入 2c** |
| O3 Reserve 快路 | 步骤侧 Mongo | 原生步骤到第 4 步前仍有用；**Mongo 步骤永远有用**。可选 |
| O4 投影事务合并写 | DataEngine 投影 | 原生步骤到第 4 步前是**唯一能抬高原生吞吐上限的做法**；Local 普通实体的投影永远受益。可选，见 §11 第 2 项 |
| O5 effect 进程内交接 | DataEngine outbox | 原生 completion 到第 4 步前有用；Local 实体的普通 effect 永远有用。可选 |
| O6 回执等待改进程内通知 | 步骤侧 | 第 4 步前降低 Mongo 负载；可选 |
| O7 同地域部署 | 部署 | 不做（维护者 2026-10-26） |
| 领取查询加索引（报告 §5） | Mongo `ClaimDue` / `ClaimOutbox` | 2a 落地后 Mongo 不再承担领取，索引修复只对过渡期有意义，见 §11 第 3 项 |

## 7. 第 2 步详细设计：Saga 协调器 + Store Service

### 7.1 范围与不变量

**迁移**：`saga.Store` 的全部状态——记录、领取租约、命令 outbox、completion 回执、操作墓碑、迟到告警标记。**不迁移**：原生步骤收件箱、Mongo 步骤收件箱、DataEngine 回执 / effect / outbox（R11）。

第 2 步要保持的不变量（编号对应草稿 §3）：

1. 单一提交位置：一个 saga 在任一时刻要么驻留 Redis（Redis 权威），要么已驱逐（Mongo 权威）；驻留 ↔ 驱逐的切换是带版本的原子交接（§7.6.4、§7.9.5）。
2. 单一生效点：`apply.lua` 原子接受 + WAITAOF 确认即生效；Mongo 投影只复制，不裁决。
3. 完整事务：一次 `Apply` 的 After、Outbox、Receipt、CloseOperation 与日志条目在一个脚本里写完。
4. 未知不等于失败：同一请求重试靠 `la`（最近一次 Apply 的请求摘要）查证（§7.5.3）。
5. 先恢复再服务：启动探针与元数据守卫通过前不开始领取（§7.6.6）。
6. 先保存后回收：日志条目只在投影进度越过后裁剪；记录只在“终态 + 已投影 + 过保留窗”后驱逐。

### 7.2 包与文件落点

| 位置 | 内容 | 改动 |
| --- | --- | --- |
| `infra/storage/redis/client.go` | `IRedis` 与可选接口 | 新增 `StreamReader`（`XRangeAfter(ctx, key, afterID string, count int64)`、`XReadAfter(ctx, key, afterID string, count int64, block time.Duration)`、`XLen`）、`ScriptLoader`（`ScriptLoad`）、`ServerInspector`（`Info(section)`、`ConfigGet(param)`）；新增 `DurableScriptBatcher`：`DurableScriptBatch(ctx, calls []ScriptCall, numLocal, numReplicas int, timeout time.Duration) (results []ScriptResult, local, replicas int64, err error)`，`ScriptCall{SHA, Source string; Keys []string; Args []any}`，`ScriptResult{Value any; Err error}` 按调用逐条给出 |
| `infra/storage/redis/driver/client.go` | 驱动实现 | 实现上述接口；`DurableScriptBatch` 同一独占连接流水线发出 `EVALSHA × n + WAITAOF`，允许一批里混合不同脚本；某条回 `NOSCRIPT`（确定没执行）时只把这些调用用 `EVAL` 在同一连接重发并补一条 `WAITAOF`；读命令按 README §1 的读重试规则；Cluster 客户端一律拒绝 |
| `infra/storage/redis/driver/README.md` | 驱动契约 | §1 表加 `DurableScriptBatch` 与流读取命令两行；§2 写明 `NOSCRIPT` 属“确定没执行” |
| `wiring/mods/name.go` | Mod 名 | `ModRedisAuthority app.ModName = "redis.authority"`、`ModStoreService app.ModName = "store_service"` |
| `wiring/redis/authority.go`（新） | 权威 Redis Mod | `AuthorityMod`：配置 `redis_authority.*`，发布 `mods.ModRedisAuthority` 能力（值为 `fredis.IRedis`，同时实现上述可选接口）；`Start` 跑启动探针（§7.6.6 第 1～4 条） |
| `framework/saga/redis_store.go`（新） | `RedisStore` | 实现 `Store`、`CompletionHistoryStore`、`LateSuccessAlarmStore`、（2c）`BatchApplier`；`EnsureInfrastructure`、`Ping`、冷读与装回 |
| `framework/saga/redis_codec.go`（新） | 编码 | 记录 ↔ 哈希字段、日志条目编解码（Store 与投影共用） |
| `framework/saga/redis_scripts.go` + `framework/saga/redis_lua/*.lua`（新） | Lua | `//go:embed redis_lua/*.lua`；启动时 `SCRIPT LOAD` 并缓存 SHA |
| `framework/saga/redis_projection.go`（新） | saga 投影 | 实现 `storeservice.Projection`：把日志批写进 Mongo 冷副本、驱逐 |
| `framework/saga/mongo_store.go` | 冷副本 | **删除**写路径（`Create`、`ClaimDue`、`Apply`、`ClaimOutbox`、`AckOutbox`、`NackOutbox`、`MarkLateSuccessAlarm`）与 outbox 集合；保留 `Get`、`GetByBusinessKey`、`List`、`CompletionHistory` 的只读实现与索引；新增未导出的 `loadForReload`、`projectBatch`、元数据读写。不再实现 `Store` |
| `framework/saga/errors.go` | 错误 | `ErrDurabilityUnconfirmed`、`ErrBackpressure`（可重试）、`ErrRedisAuthorityMismatch`（启动守卫） |
| `framework/saga/assembly.go` | 装配 | `Assemble(mongo fmongo.IMongo, authority fredis.IRedis, jetStream fnats.IJetStream, cfg AssemblyConfig, definitions ...Definition)`；`AssemblyConfig.Redis RedisStoreOptions`；`Assembly.Store *RedisStore`、`Assembly.Copy *MongoStore`；`Start` 调 `Store.EnsureInfrastructure` |
| `framework/saga/nest_completion_consumer.go`、`nest_start_consumer.go` | 消费者并发 | 两个配置加 `Workers`，复用 `withConsumerWorkers`（`framework/saga/jetstream.go:195`） |
| `framework/saga/engine.go`、`step_transition.go` | 2c | §7.11 |
| `framework/storeservice/`（新包） | 通用投影运行器 | `projection.go`（接口与 `Entry`）、`runner.go`（分区租约、读日志、批、进度、暂停、指标）、`lua/*.lua`（`plock.lua`、`advance.lua`） |
| `wiring/saga/config.go`、`mod.go` | saga Mod | 新配置（§7.13.2）；`DependsOn` 加 `mods.ModRedisAuthority`；`Store()` 返回 `*coresaga.RedisStore`；`Locations` 加 Redis 字段；健康检查加 Redis Ping 与积压 |
| `wiring/storeservice/`（新） | Store Service Mod | `Mod`：读 `store_service.*` 与 `saga` 的共享键，构造 `coresaga.NewRedisProjection` 并交给 `storeservice.Runner`；依赖 `mongo`、`redis.authority` |
| `wiring/sidmerge/migrate.go`、`checklist.go` | 合服 | §7.13.3 |
| codegen 模板、`demo/`、`scripts/mirror-local.sh` | 生成工程与本地依赖 | 生成配置写 `redis_authority.*`、`saga.redis.key_prefix`，与 saga Mod 同进程注册 `store_service` Mod；demo 按参考工程手工同步（demo 不迁到生成物）；本地依赖脚本把权威 Redis 起成 `--appendonly yes --appendfsync always --maxmemory-policy noeviction` |
| `scripts/it-redis-aof.sh`（新） | 测试用私有 Redis | `start|stop|env`：私有目录、端口偏移（缺省 26391），AOF always、noeviction；不碰共享隔离环境的 17379 |

新增两个包（`framework/storeservice`、`wiring/storeservice`）的理由：第 3、4 步的 Remote 与 RedisActive 投影复用同一个运行器（分区租约、批、进度、暂停、指标），它不属于 saga；领域投影（文档形状）仍各自留在 `framework/saga`、`framework/remoteentity`、`framework/dataengine`，符合“三块各自局部聚集”。

### 7.3 键设计

#### 7.3.1 单主下不需要 hash tag，也不需要按 saga 分桶

- **原子性**：单主下一个 Lua 可以访问任意键，不存在跨槽。hash tag 只为 Cluster 服务，而 v2 明确拒绝 Cluster（§2）。
- **领取**：全局一个 `due` ZSet、一个 `outbox` ZSet。`ZRANGEBYSCORE ... LIMIT 0 n` 是 O(log N + n)，积压 1 万、10 万条时取到期项的代价基本不变（报告 §5 的 Mongo 阻塞 SORT 问题在这里不存在）。Redis 单线程，全局 ZSet 没有锁竞争问题。
- **仍需要分区的只有“投影”**：Store Service 要按分区并行、每个分区一条有序日志、一个进度。分区 `p = fnv32a(sagaID) mod P`，P = `saga.redis.partitions`（缺省 16）。一个 saga 的全部日志条目落在同一分区，所以同一 saga 的条目在分区内有序。
- **saga ID 不变**：分区由 ID 算出，不需要 ID 里带桶号。调用方自带 ID（`StartRequest.ID`）照常接受。
- **脚本里拼键**：`claim_due.lua` 等脚本按前缀在脚本内拼出 `rec:<id>` 键。这在单主下可用，在 Cluster 下不可用；因为配置阶段拒绝 Cluster，这是有意的选择，脚本头注释写明。

#### 7.3.2 键表

前缀 `K = saga.redis.key_prefix`（必填，如 `roost:{project}:saga`；不同部署共用一个 Redis 时必须不同）。

| 键 | 类型 | 内容 | 生命周期 |
| --- | --- | --- | --- |
| `K:meta` | Hash | `instance`（首次初始化生成的随机 ID）、`partitions`、`format`（=1） | 永久 |
| `K:rec:<sagaID>` | Hash | 标量字段（脚本要读）：`v` 版本、`st` 状态、`nr` next_run 毫秒、`lo`/`lt`/`lu` 租约 owner / token / until 毫秒、`la` 最近一次 Apply 的请求摘要、`ls` 最近一次写入的日志条目 ID、`ta` 进入终态的毫秒（非终态为 0）、`bk` 业务键的键名、`p` 分区；`doc`：记录主体（§7.4）；回执字段 `c:<commandID>` = 摘要十六进制；墓碑字段 `o:<operationKey>` = `<closure>`（`abandoned` / `result`）+ `|<created_ms>`；告警字段 `a:<operationKey>:<incarnation>` = 毫秒 | 驻留期间；驱逐时整键删除 |
| `K:bk:<len(type)>:<type>:<businessKey>` | String | sagaID | 驻留期间永久；驱逐时设 `PEXPIRE business_key_guard` |
| `K:due` | ZSet | member = sagaID，score = `max(nr, lu)`；只含非终态记录 | Create / Apply / 领取维护；终态移除 |
| `K:evict:<p>` | ZSet | member = sagaID，score = `ta + terminal_retention`；只含终态记录 | 进入终态时加入；驱逐或重开时移除 |
| `K:ob:<commandID>` | Hash | `cmd`（命令编码）、`na` next_attempt 毫秒、`att` attempt、`lo`/`lt`/`lu` 租约、`ca` created 毫秒、`op` operationKey、`le` last_error | 派发时写；确认 / 关闭操作 / 替换时删 |
| `K:obk:<operationKey>` | String | 该操作当前排队的 commandID（“同一操作只留最新尝试”） | 同上 |
| `K:outbox` | ZSet | member = commandID，score = `max(na, lu)` | 同上 |
| `K:log:<p>` | Stream | 每次 Create / Apply / 装回之外的状态变化追加一条（§7.4.2） | 投影进度越过后裁剪 |
| `K:proj:<p>` | String | 已提交到 Mongo 的最大日志条目 ID | Store Service 推进 |
| `K:plock:<p>` | Hash | 分区租约 `owner`、`token`、`until` | Store Service |
| `K:paused:<p>` | String | 暂停原因（存在即暂停） | Store Service 写；运维确认后删除 |

#### 7.3.3 业务键守卫（Create 的 Mongo 读）

Mongo 的 `uniq_type_business` 唯一索引是永久的（`framework/saga/mongo_store.go:82-89`，saga 集合没有 TTL）。Redis 的 `bk` 键在驱逐后不能永久保留（容量），所以 Create 必须查一次 Mongo，才能保持“同一业务键永久唯一”的现有语义：

1. Go 先读 Mongo：`FindOne({$or: [{_id: id}, {type, business_key}]})`。命中 → 返回 `ErrAlreadyExists`（引擎随后按现有逻辑 `GetByBusinessKey` 比较意图，`framework/saga/engine.go:258-272`）。
2. 再执行 `create.lua`，带上第 1 步开始时的客户端时间 `read_ms`。脚本检查 `rec`、`bk` 是否存在，并检查 `TIME − read_ms < business_key_guard / 2`，超过则返回 `STALE_GUARD`，Go 回到第 1 步重做。
3. **为什么安全**：驱逐时 `bk` 再保留 `business_key_guard`（缺省 10m）。Mongo 读看不到的旧 saga，要么还驻留 Redis（`bk` 在），要么驱逐不足 guard 时长（`bk` 在），要么驱逐超过 guard——而驱逐只在投影之后，此时第 1 步的 Mongo 读开始时它已在 Mongo（因为第 2 步检查保证第 1 步到脚本执行不超过 guard/2）。时钟偏差预算是 guard/2 = 5 分钟。
4. 代价：每次启动多 1 次 Mongo 读（索引命中），不在每步路径上。

#### 7.3.4 容量预算

- 驻留记录数 ≈ 在途 saga 数 + 终态速率 × `terminal_retention`。每条约 1～2KB（`doc` 约 300B + 业务 Data，加回执 / 墓碑各约 100B、ZSet 与 `bk` 约 200B）。100 TPS × 15m ≈ 9 万条 ≈ 135MB。
- 日志积压：每条约等于一份 `doc`。`max_projection_backlog`（每分区缺省 20000 条）× 16 × 1KB ≈ 320MB 上限。
- `bk` 守卫期：100 TPS × 10m = 6 万个 ≈ 6MB。
- 合计按 `maxmemory` ≥ 1GB 起步，实际按 §7.12 的 `saga.redis.resident_records`、积压指标调整。

### 7.4 记录编码

#### 7.4.1 `doc`

`doc` = 记录主体的 JSON，字段名与 `recordDoc` 的 bson 名相同（`framework/saga/mongo_store.go:435-464`），**不含租约字段**（租约只在标量字段里；`record()` 解码时合并）。脚本不解析 `doc`，只把它整体写入哈希和日志；Store Service 把它解码后写成 Mongo `recordDoc`。

#### 7.4.2 日志条目

`XADD K:log:<p> * f <fields...>`，字段：

| 字段 | 含义 | 何时有 |
| --- | --- | --- |
| `id` | sagaID | 总有 |
| `v` | 写入后的记录版本 | 总有 |
| `doc` | 写入后的记录主体 | 总有 |
| `c`、`cd`、`ct` | 回执：commandID、摘要、created 毫秒 | Apply 带 Receipt |
| `o`、`oc`、`ot` | 墓碑：operationKey、closure、created 毫秒 | Apply 带 CloseOperation |
| `a`、`ai`、`at` | 迟到告警：operationKey、incarnation、毫秒 | `mark_alarm.lua` |

领取（只改租约）不写日志：Mongo 冷副本不需要租约。

### 7.5 Lua 脚本清单

通用规则：

- 每个脚本以 `#!lua` 开头，不声明 `allow-oom`、`no-writes`；OOM 时在执行前被拒。
- **先全部校验再写**：版本、租约、回执摘要、墓碑、容量、**键类型**（`TYPE` 不是预期类型时返回 `CORRUPT`）都在第一条写之前完成。校验之后的写只会因内部错误失败，这类错误按“部分写入”处理：Go 侧计 `saga.redis.script_partial_total`、记 ERROR、向 `RuntimeFailure` 报告。
- 业务结果用返回值表达（第一个元素是结果码字符串），**不用 `redis.error_reply`**：批量里一条报错会让 `DurableScriptBatch` 的整批结果失效。
- 时间：脚本内用 `redis.call('TIME')` 的服务端时间做守卫判断（只用于 guard 与驱逐）；业务时间（`next_run_at` 等）由 Go 传入，与现在一致。
- 全部写脚本走 `DurableScriptBatch(..., numLocal=1, numReplicas=0, waitaof_timeout)`。

#### 7.5.1 `create.lua`

| | 内容 |
| --- | --- |
| KEYS | `rec`、`bk`、`due`、`log:<p>` |
| ARGV | sagaID、`v`(=1)、`st`、`nr`、`doc`、`bk` 键名、`p`、`read_ms`、`guard_ms`、`max_backlog` |
| 校验顺序 | ① 键类型；② `rec` 存在 → `EXISTS_ID`；③ `bk` 存在 → `EXISTS_BK`, 已有 sagaID；④ `TIME − read_ms ≥ guard_ms/2` → `STALE_GUARD`；⑤ `XLEN log:<p> ≥ max_backlog` → `BACKPRESSURE` |
| 写 | `HSET rec`（标量 + `doc`，`la` 置空，`ta`=0）；`SET bk`；`ZADD due nr`；`XADD log`；`HSET rec ls <entryID>` |
| 返回 | `OK`, entryID |
| Go 映射 | `EXISTS_*` → `ErrAlreadyExists`；`STALE_GUARD` → 重做 Mongo 读（最多 3 次，之后 `ErrConflict`）；`BACKPRESSURE` → `ErrBackpressure`（启动消费者 Nak 退避） |

#### 7.5.2 `claim_due.lua` / `claim_outbox.lua`

| | `claim_due.lua` | `claim_outbox.lua` |
| --- | --- | --- |
| KEYS | `due` | `outbox` |
| ARGV | `now_ms`、owner、`lease_ms`、limit、前缀 | 同左 |
| 逐个候选 | `ZRANGEBYSCORE due -inf now LIMIT 0 limit`；`rec` 不存在或已终态 → `ZREM` 跳过（自愈）；否则 `lt+1`、`lo`、`lu=now+lease`，`ZADD due lu` | `ZRANGEBYSCORE outbox -inf now LIMIT 0 limit`；`ob` 不存在 → `ZREM` 跳过；否则 `lt+1`、`lo`、`lu`，`ZADD outbox lu` |
| 返回 | 每条：标量字段 + `doc` | 每条：`cmd`、`att`、`na`、租约、`ca` |
| 复杂度 | O(log N + limit) | 同左 |

score = `max(next, lease_until)`，所以 score ≤ now 的成员都可领取，不需要在脚本里再过滤，取到的就是到期项。校验不过的记录（`doc` 解码失败）在 Go 侧照现有规则只跳过那一条并告警（`saga.store.corrupt_record_total`，`framework/saga/mongo_store.go:241-259`）。

#### 7.5.3 `apply.lua`（核心）

| | 内容 |
| --- | --- |
| KEYS | `rec`、`due`、`evict:<p>`、`log:<p>`、`outbox` |
| ARGV | `exp_v`；期望租约 `exp_lo`、`exp_lt`（不带租约时为空）；After 标量（`v`、`st`、`nr`、`lo`、`lt`、`lu`、`ta`）与 `doc`；`digest`（整个 ApplyRequest 的 SHA-256）；回执（`c`、`cd`、`ct`，可空）；关闭操作（`o`、`oc`、`ot`，可空）；新 outbox（`cmdID`、`op`、`cmd`、`na`、`att`、`ca`，以及 2c 的直接发布租约 `olo`、`olt`、`olu`，可空）；`retention_ms`；前缀 |
| 校验顺序 | ① 键类型；② `rec` 不存在 → `NEED_RELOAD`；③ `v == exp_v + 1` 且 `la == digest` → `APPLIED_BEFORE`（同一请求在回复丢失后的重试，按已应用返回；放在回执检查之前，否则带回执的重试会被报成重复）；④ 带回执且 `c:<cmdID>` 存在：摘要相同 → `DUPLICATE`，不同 → `IDENTITY_CONFLICT`（回执检查先于版本检查，与 Mongo 实现相同，`framework/saga/mongo_store.go:295-310`）；⑤ `v ≠ exp_v` → `CONFLICT`；⑥ 带期望租约且 (`lo`,`lt`) ≠ (`exp_lo`,`exp_lt`) → `CONFLICT`（与 `applyFilter` 相同，`framework/saga/mongo_store.go:575-582`）；⑦ 关闭操作：`o` 必须以 `<sagaID>:` 开头（Go 已校验，脚本再核），读出已有 closure；⑧ outbox：`obk:<op>` 指向的旧命令键类型 |
| 写 | `HSET rec` 标量 + `doc` + `la=digest`；回执 `HSET c:<cmdID>`；墓碑：不存在则写 `o:<op>`，已存在且新 closure 为 `result`、旧为 `abandoned` 时升级，否则不动；新 outbox：删除 `obk:<op>` 指向的旧 `ob`（`DEL` + `ZREM outbox`），写 `ob:<cmdID>`、`SET obk:<op>`、`ZADD outbox max(na, olu)`；关闭操作：删除 `obk:<o>` 指向的 `ob`（`DEL` + `ZREM`）与 `obk:<o>`；状态：非终态 `ZADD due max(nr,lu)` 并 `ZREM evict:<p>`，终态 `ZREM due` 并 `ZADD evict:<p> ta+retention`；`XADD log`；`HSET rec ls <entryID>` |
| 返回 | `APPLIED` / `APPLIED_BEFORE` / `DUPLICATE`, entryID 或空 |
| Go 映射 | `APPLIED`、`APPLIED_BEFORE` → `ApplyApplied`；`DUPLICATE` → `ApplyDuplicate`；`CONFLICT` → `ErrConflict`；`IDENTITY_CONFLICT` → `ErrIdentityConflict`；`NEED_RELOAD` → 装回后重试一次（§7.6.4）；`CORRUPT` → `ErrInvalidRecord` + 告警 |

`la` 只记最近一次，够用：同一个协调器对同一记录的重试必然带同一个 `exp_v`；若中间别人又写了一次（`v` 变成 `exp_v+2`），重试得到 `CONFLICT`，引擎按现有逻辑重读（`Complete` 最多 8 次，`framework/saga/engine.go:448`），这与 Mongo 下“驱动重跑事务后撞版本”的结果相同。

#### 7.5.4 `ack_outbox.lua` / `nack_outbox.lua`

| | `ack_outbox.lua` | `nack_outbox.lua` |
| --- | --- | --- |
| KEYS | `ob:<cmdID>`、`outbox` | `ob:<cmdID>`、`outbox` |
| ARGV | cmdID、owner、token、前缀 | cmdID、owner、token、`next_ms`、`last_error` |
| 校验 | `ob` 不存在 → `GONE`；租约不符 → `CONFLICT` | 同左 |
| 写 | `DEL ob`、`ZREM outbox`；`obk:<op>` 仍指向本命令时 `DEL` | `na=next`、`le`、`att+1`、清租约、`ZADD outbox next` |
| Go 映射 | `GONE` → `nil`（命令已被“关闭操作”删掉，等价于已确认；不计冲突）；`CONFLICT` → `ErrConflict` | `GONE` → `nil`；`CONFLICT` → `ErrConflict` |

`GONE → nil` 是相对 Mongo 实现（`DeleteOne` 计数 0 即 `ErrConflict`，`framework/saga/mongo_store.go:400-409`）的有意改变：2c 之后完成常常先于确认到达，按冲突计数会误报。

#### 7.5.5 `mark_alarm.lua`

KEYS `rec`、`log:<p>`；ARGV opKey、incarnation、`now_ms`。校验：`rec` 不存在 → `NEED_RELOAD`；`o:<op>` 不存在 → 返回 `0`（没有墓碑，与 Mongo 相同）。写：`HSETNX a:<op>:<inc>`，得到 1 才 `XADD log`（`a`/`ai`/`at`）。返回 `1`（首次）或 `0`。

#### 7.5.6 `reload.lua`

KEYS `rec`、`bk`、`due`、`evict:<p>`；ARGV：Mongo 读出的记录标量 + `doc`、该 saga 的墓碑与告警（从 Mongo `operations` 按 `_id` 前缀 `^<sagaID>:` 读出）、`retention_ms`、`now_ms`。校验：`rec` 已存在 → `EXISTS`, 当前 `v`（并发装回或已在驻留）。写：`HSET rec`（`ls = "0-0"`，表示 Mongo 已有）、`SET bk`（`PERSIST`）、非终态 `ZADD due`，终态 `ZADD evict:<p> now+retention`。**不写日志**（数据来自 Mongo）。回执不装回：历史查询未命中 Redis 时回查 Mongo（§7.6.3）。

#### 7.5.7 `evict.lua`（Store Service 调用，saga 投影提供）

KEYS `evict:<p>`、`proj:<p>`；ARGV `now_ms`、limit、`guard_ms`、`delay_ms`、前缀。对 `ZRANGEBYSCORE evict:<p> -inf now LIMIT 0 limit` 的每个 ID：`rec` 不存在或非终态（已重开）→ `ZREM`；`ls > proj`（还没投影到）→ `ZADD evict:<p> now+delay`；否则 `DEL rec`、`PEXPIRE bk guard`、`ZREM`。返回驱逐数。日志条目 ID 比较在脚本里按 `毫秒-序号` 两段数值比较。

#### 7.5.8 `plock.lua` / `advance.lua`（`framework/storeservice`）

- `plock.lua`：KEYS `plock:<p>`；ARGV owner、`lease_ms`、`now_ms`。租约过期或 owner 相同 → `token+1`（owner 相同时不加）、写 `until`，返回 token；否则返回 `0`。
- `advance.lua`：KEYS `plock:<p>`、`proj:<p>`、`log:<p>`；ARGV owner、token、`upto`（本批最大条目 ID）。校验：租约 owner / token 相符且未过期，否则 `LOST`；`upto ≤ proj` → `NOOP`。写：`SET proj upto`；`XTRIM log MINID <upto 的下一个 ID>`（删除 ≤ upto 的条目）。

### 7.6 Go 侧流程（`RedisStore`）

```go
type RedisStoreOptions struct {
	KeyPrefix            string        // saga.redis.key_prefix
	Partitions           int           // saga.redis.partitions，缺省 16
	WaitAOFTimeout       time.Duration // saga.redis.waitaof_timeout，缺省 1s
	TerminalRetention    time.Duration // saga.redis.terminal_retention，缺省 15m
	BusinessKeyGuard     time.Duration // saga.redis.business_key_guard，缺省 10m
	MaxProjectionBacklog int64         // saga.redis.max_projection_backlog，缺省 20000（每分区）
}

func NewRedisStore(authority fredis.IRedis, copy *MongoStore, failure *app.RuntimeFailure, options RedisStoreOptions) (*RedisStore, error)
```

#### 7.6.1 读

- `Get(id)`：`HMGET rec` 标量 + `doc`（不读回执 / 墓碑字段）→ 解码。未命中 → `copy.Get`（冷读，计 `saga.redis.cold_read_total{op="get"}`）。
- `GetByBusinessKey(type, bk)`：`GET bk` → `Get(id)`；未命中 → `copy.GetByBusinessKey`。
- `List(query)`：只含非终态状态 → 从 `due` 全量取 ID（运维接口，`Limit ≤ 1000`）后批量 `HMGET` 过滤；含终态状态 → 终态部分读 Mongo 冷副本（最终一致，落后于投影积压），驻留的终态记录从 `evict:*` 补齐。文档写明“终态记录的 List 是最终一致的”。
- `CompletionHistory(c)` / `CompletionRecorded(c)`：`HMGET rec c:<cmdID> o:<op>`；记录驻留且两者都没有 → 回查 Mongo（回执可能已随驱逐装回而不在 Redis）；记录未驻留 → `copy.CompletionHistory`。身份检查（`o` 前缀、摘要）与 Mongo 实现相同。

#### 7.6.2 写

`Create`（§7.3.3）、`ClaimDue`、`Apply`、`ClaimOutbox`、`AckOutbox`、`NackOutbox`、`MarkLateSuccessAlarm` 各对应一个脚本；全部经 `DurableScriptBatch`；结果按 §4.3 / §7.5 映射。

#### 7.6.3 结果未知

驱动返回“结果未知”（EOF、读超时、ctx 到期）时，`RedisStore` 不自己重试，原样返回（与 Mongo 下 `ErrCommitResultUnknown` 的处理一致），由引擎的既有路径处理：`Complete` 由 JetStream 重投后重跑（`APPLIED_BEFORE` / `DUPLICATE` 查证）；协调循环中的派发靠租约过期后重新领取、读到新版本；发布由 outbox 租约过期后重发（JetStream 按 MsgID 去重，收件箱幂等）。

#### 7.6.4 冷记录装回

规则：**冷记录要写，先装回**。`Apply` / `MarkLateSuccessAlarm` 收到 `NEED_RELOAD` 时：

1. `copy.loadForReload(id)`：读 `sagas` 文档（1 次）与 `operations` 前缀查询（1 次）。
2. `reload.lua`；返回 `EXISTS` 也算成功（别人先装回了）。
3. 重跑原脚本一次；再 `NEED_RELOAD` 返回 `ErrConflict`。

安全性：驱逐只在 `ls ≤ proj`（Mongo 已有最新版本）时发生，驱逐后不会再有写（写必须先装回），所以 Mongo 副本就是驱逐时的最新版本。装回与驱逐、两个装回之间的竞争都由“不存在才写”与版本 CAS 收口（§7.8）。

#### 7.6.5 2c 的批量

`ApplyBatch(ctx, []ApplyRequest) ([]ApplyOutcome, []error)`：一批 `apply.lua` 调用一次 `DurableScriptBatch`；某条 `NEED_RELOAD` 的单独装回后逐条重跑。

#### 7.6.6 启动（`EnsureInfrastructure`）

按顺序，任何一条不过都拒绝启动：

1. `INFO server`：`redis_version ≥ 7.2`；`INFO cluster`：`cluster_enabled:0`；`INFO replication`：`role:master`。
2. `CONFIG GET appendonly / appendfsync / no-appendfsync-on-rewrite / maxmemory-policy`：分别必须是 `yes / always / no / noeviction`。托管 Redis 禁用 `CONFIG` 时记 WARN 跳过这一条，靠第 3 条兜底。
3. 探针写：`K:meta` 上的空操作脚本走 `DurableScriptBatch(numLocal=1)`，必须得到 `local = 1`。
4. `SCRIPT LOAD` 全部脚本。
5. 元数据守卫：读 Mongo `saga_meta` 文档（`_id: "redis"`）与 `K:meta`。
   - 都没有 → 首次初始化：先确认 Mongo `sagas` 集合里没有非终态记录（否则拒绝：“旧版本的非终态 saga 要先用旧版本跑完”，§7.13.4），再生成 `instance`，Mongo 插入（不存在才插）后读回，Redis `HSETNX`。
   - Mongo 有、Redis 没有 → 拒绝：“权威 Redis 为空或换了库，按灾备手册处理”。**空 Redis 不自动当权威。**
   - 都有但 `instance` 不同 → 拒绝（`ErrRedisAuthorityMismatch`）。
6. 分区守卫：`K:meta.partitions ≠ 配置` 时，所有 `log:<p>` 必须为空（积压已投影、已裁剪）才改写为新值；否则拒绝。
7. `copy.EnsureInfrastructure`（冷副本索引；删除 outbox 集合的索引登记）。

### 7.7 状态转换表（Redis 侧）

| 当前 | 事件 | 下一状态 | 原子写（同一脚本） |
| --- | --- | --- | --- |
| 不存在 | Create（Mongo 无、`bk` 无、guard 内、未超积压） | 驻留·活动 | `rec`、`bk`、`due`、`log` |
| 驻留·活动 | 领取（到期且租约过期） | 驻留·活动（已领取） | `rec` 租约字段、`due` score |
| 驻留·活动 | Apply 派发 | 驻留·活动（Waiting） | `rec`、`due`、`ob`/`obk`/`outbox`（替换同一操作的旧尝试）、`log` |
| 驻留·活动 | Apply 接收结果（2c：可连带派发下一步） | 驻留·活动或驻留·终态 | `rec`（含回执、墓碑字段）、清除关闭操作的 outbox、（连带派发）新 outbox；终态时 `due → evict:<p>`；`log` |
| 驻留·活动 | Apply 超时 / 截止 / 定义缺失 / 人工 | 驻留·活动或驻留·终态 | 同上 |
| 驻留·终态 | 迟到成功、Resume、人工 Compensate | 驻留·活动 | `evict:<p> → due`、`rec`、`log` |
| 驻留·终态 | 驱逐（`ta+retention ≤ now` 且 `ls ≤ proj`） | 已驱逐 | 删 `rec`；`bk` 设 guard 过期；`ZREM evict` |
| 已驱逐 | 读（Get / 历史） | 已驱逐 | 不写；读 Mongo |
| 已驱逐 | 写（Apply / 告警标记） | 驻留（装回）→ 再走对应转换 | `reload.lua`：`rec`、`bk`、`due` 或 `evict` |
| 任意 | 元数据守卫不过 | 拒绝启动 | 不写 |

### 7.8 崩溃恢复表（单主，无切主行）

| 崩溃 / 故障点 | 持久状态 | 恢复动作 | 不变量 |
| --- | --- | --- | --- |
| 协调器在脚本发出后、收到回复前崩溃 | 未知 | 租约过期后新领取者读到最新版本继续；原请求如被重试，`la` 查证返回 `APPLIED_BEFORE` | 4 |
| `Complete` 回复丢失 | 未知 | JetStream 重投 → 重跑 `Complete`：已应用则回执命中 `DUPLICATE`，或 `judgeCompletion` 走历史路径 | 4 |
| 脚本已执行，`WAITAOF local = 0` | 已执行、持久未确认（只在部署违约时出现） | `ErrDurabilityUnconfirmed`，fail-stop；运维修正配置后重启，重启后按 AOF 实际内容继续 | 2、4 |
| Redis `kill -9` / 宕机重启 | 已回复的写都在 AOF；未回复的脚本整段在或整段不在 | 重放 AOF；脚本缓存清空 → `NOSCRIPT`（确定没执行）→ 驱动重新加载后同连接重发；在途请求按“结果未知”查证 | 2、4 |
| 发布器在 PubAck 之后、确认之前崩溃 | outbox 仍在 | 租约过期后重新发布；JetStream 按 MsgID 去重，收件箱幂等 | 6 |
| Store Service 在 Mongo 写之前崩溃 | Mongo 没有 | 新租约持有者从 `proj` 重放 | 6 |
| Store Service 在 Mongo 写之后、`advance` 之前崩溃 | Mongo 已有，`proj` 落后 | 从 `proj` 重放；Mongo 写按版本单调，重放不倒退 | 6 |
| 两个 Store Service 实例同时处理一个分区（租约过期时慢节点仍在写） | — | 慢节点的 Mongo 写版本不高于快节点，单调条件让它不生效；它的 `advance` 因 token 不符返回 `LOST`，随即停止该分区 | 6 |
| 驱逐与 Apply 并发 | — | 脚本串行：驱逐先 → Apply 得 `NEED_RELOAD` 装回；Apply 先 → 记录可能重开（离开 `evict`）或 `ls` 变大（驱逐推迟） | 1、6 |
| 两个装回并发 | — | `reload.lua` 不存在才写，后到者得 `EXISTS` | 1 |
| 同一业务键并发 Create，与驱逐交错 | — | §7.3.3 的 guard | 3 |
| Mongo 不可用 | Redis 正常 | 协调器继续推进驻留 saga；Create 的 Mongo 读失败 → 启动消费者 Nak 退避；投影积压增长，超过 `max_projection_backlog` 时 Create 返回 `ErrBackpressure`；Mongo 恢复后投影追平。注意 Mongo 步骤与原生步骤侧本身依赖 Mongo，这期间步骤不会完成 | 5、6 |
| Redis 不可用 | — | 协调器、发布器、消费者的存储调用失败：消费者 Nak 重投，循环退避；不产生任何“部分推进” | 4 |
| Redis OOM | 写脚本在执行前被拒 | 确定没执行；Create 返回 `ErrBackpressure`，其余调用退避重试；告警 | 4 |
| 投影遇到 Mongo 非暂时错误（唯一索引冲突、校验失败） | Redis 已确认 | 该分区暂停（写 `paused:<p>`、计 `store_service.paused_partitions`、健康 Fail、ERROR 日志），**不跳过**；每 30s 重试；运维核对后删除暂停键 | 2、6 |
| 灾备：主机 / 磁盘损毁，从副本或备份恢复 | 丢失复制 / 备份之后的已确认提交 | **不在框架承诺内**。手册：停所有协调器与 Store Service → 恢复 Redis → 对每个分区比较 Mongo 冷副本与 Redis 的版本（Mongo 版本更高的记录以 Mongo 为准装回，其后的 Redis 未投影尾部人工核对）→ 核对 JetStream 上未完成的命令 / completion → 再启动。`instance` 不变，启动守卫不会拦（恢复的是同一个库）；空库则会被守卫拦下 | — |

### 7.9 Store Service：投影协议、进度与回收

#### 7.9.1 运行器（`framework/storeservice`）

```go
type Entry struct {
	ID     string            // 流条目 ID
	Fields map[string]string
}

type Projection interface {
	Name() string        // "saga"；第 3、4 步加 "remote"、"entity"
	KeyPrefix() string   // 运行器由它派生 log / proj / plock / paused 键
	Partitions() int
	// Apply 把一批条目写进 Mongo。必须幂等、按版本单调：重放、乱序重放都不能让副本倒退。
	// 返回 Permanent(err) 表示非暂时错误，运行器暂停该分区；其他错误按暂时错误退避重试。
	Apply(ctx context.Context, partition int, entries []Entry) error
	// AfterAdvance 在进度推进后调用（saga 用它驱逐）；出错只记录，下一轮再做。
	AfterAdvance(ctx context.Context, partition int, upto string) error
}

type Options struct {
	Owner         string        // store_service.owner，不写取 store-<sid>-<随机>
	BatchSize     int           // store_service.batch_size，缺省 256
	LeaseDuration time.Duration // store_service.lease_duration，缺省 10s
	Block         time.Duration // store_service.block，缺省 100ms
	MaxPartitions int           // store_service.max_partitions，本进程最多同时处理的分区数，0 = 不限
}

func NewRunner(authority fredis.IRedis, options Options, projections ...Projection) (*Runner, error)
func (r *Runner) Run(ctx context.Context) error
func (r *Runner) Stop(ctx context.Context) error
```

每个（投影，分区）一个循环：

1. `plock.lua` 取租约（取不到就每 `LeaseDuration/3` 重试；取到后每 `LeaseDuration/3` 续约，续约失败立即停止该分区）。
2. 游标 = `GET proj:<p>`（不存在为 `0-0`）。
3. `XRANGE log:<p> (游标 + COUNT BatchSize`；为空时 `XREAD BLOCK Block COUNT BatchSize STREAMS log:<p> 游标`。
4. `projection.Apply(entries)`；暂时错误退避（50ms 起，封顶 5s）后重试同一批；`Permanent` 错误暂停分区。
5. `advance.lua(upto = 最后一条 ID)`；`LOST` 时放弃该分区回到第 1 步。
6. `AfterAdvance`。
7. 游标 = upto，回到第 3 步。

多个 Store Service 实例靠分区租约分摊；**正确性不依赖租约排他**（Mongo 写单调、进度 CAS），租约只是避免重复劳动。所以这里不需要 app 级单实例保证。

#### 7.9.2 saga 投影的 Mongo 写（`RedisProjection.Apply`）

一批条目先在内存里合并：同一 saga 只保留最高版本的 `doc`；回执、墓碑、告警全部保留。然后最多三次 `BulkWrite`（无序，`ordered:false`，写关注沿用客户端缺省 majority + j:true），**不用 Mongo 事务**：

| 集合 | 写模型 | 单调 / 幂等条件 |
| --- | --- | --- |
| `sagas` | `UpdateOne({_id}, [{$replaceWith: {$cond: [{$lt: [{$ifNull: ["$version", 0]}, v]}, <新文档>, "$$ROOT"]}}], upsert)` | 版本只升不降 |
| `completions` | `UpdateOne({_id: cmdID}, {$setOnInsert: {digest, created_at}}, upsert)` | 已存在不改 |
| `operations` | `UpdateOne({_id: opKey}, [{$set: {saga_id: ifNull, created_at: ifNull, closure: 已是 "result" 则保持否则取新值}}, 告警 {$set: {"late_alarms.r<inc>": ifNull}}], upsert)` | closure 只升不降；告警首次写入 |

为什么不需要 Mongo 事务：冷副本不是权威，读它的场景（冷读、装回、历史回查、List）都发生在“该记录已投影”之后（驱逐的前提）或者明确是最终一致的（List）；一批里部分写成功、随后崩溃，重放会补齐，单调条件保证不倒退。

`created_at` 取条目里的值，Mongo 的 TTL 索引（`completions` / `operations` 按 `created_at`，`completion_receipt_ttl` 缺省 720h）语义不变。

#### 7.9.3 每批代价与目标吞吐

- 每批：`XRANGE` 1 + `BulkWrite` ≤ 3 + `advance.lua` 1 + 驱逐 0～1。
- 本机 Mongo 一次 bulkWrite（majority + j）约 10～20ms（单文档事务 10.7ms，报告 §2），一批 256 条约 30～60ms → **每分区约 4000～8000 条/s，16 分区远超所需**。
- 门槛：本机 ≥ 5000 条/s（P=16、批 256），用 §7.14 的 `BenchmarkSagaProjection` 测。参照：100 TPS 的两步 saga 每秒产生约 500 条日志（启动 1 + 每步派发 1 + 完成 1）。
- 云上 60ms RTT 时，每批约 4 次往返 ≈ 250ms，每分区约 1000 条/s，16 分区约 16000 条/s：投影吞吐由批量和分区数决定，不再由逐条往返决定。

#### 7.9.4 进度与积压

- 进度 = `proj:<p>`，只在 Mongo 写全部确认之后推进；日志只裁剪到进度。
- 积压 = `XLEN log:<p>`；延迟 = 当前时间 − 游标后第一条条目 ID 的毫秒部分。
- 背压只作用于 Create（§7.5.1）：已存在的 saga 必须能推进到终态，否则积压永远消不掉。

#### 7.9.5 回收规则

| 数据 | 可回收前提 | 谁回收 |
| --- | --- | --- |
| 日志条目 | 进度越过它（Mongo 已确认） | `advance.lua` 的 `XTRIM MINID` |
| 驻留的终态记录（连同回执、墓碑、告警字段） | 终态 + `ta + terminal_retention ≤ now` + `ls ≤ proj` | `evict.lua`（`AfterAdvance`） |
| `bk` | 记录驱逐后再过 `business_key_guard` | Redis 过期（这是唯一用 TTL 的键，它保护的是“Mongo 已有”的数据） |
| outbox 命令 | 发布后确认，或所属操作关闭 | `ack_outbox.lua` / `apply.lua` |
| Mongo 回执 / 墓碑 | 720h TTL（不变） | Mongo TTL 索引 |
| Mongo saga 文档 | 不回收（与现在相同） | — |

不对未投影的数据设 TTL，不用 `MAXLEN` 截断，淘汰策略 `noeviction`（草稿 §7）。

### 7.10 并发模型与连接

| 组件 | 并发 | 配置 | 说明 |
| --- | --- | --- | --- |
| 协调器 | `coordinator_workers`（缺省 4）个循环 | 2c 起 `coordinator_claim_batch` 缺省 3 → 16 | 2a 每条派发一个脚本；2c 一批合成一次 `DurableScriptBatch` |
| 发布器 | `publisher_workers`（缺省 4） | 2c 起 `publisher_claim_batch` 缺省 1 → 32 | 2c：直接发布通道 + 批内 PubAck 并发等待 + 确认批量（≤ 32 条或 2ms 一批） |
| saga 结果消费者 | `saga.result_workers`（新，缺省 8） | ≤ `result_max_ack_pending` | `Engine.Complete` 按版本 CAS，可并发 |
| 原生 completion 消费者 | `saga.result_effect_workers`（新，缺省 8） | 同上 | `NestCompletionConsumerConfig.Workers`（新字段） |
| 启动消费者 | `saga.start_effect_workers`（新，缺省 4） | 同上 | `NestStartConsumerConfig.Workers`（新字段） |
| Mongo 步骤消费者 | 不变（`MongoWorkers`，夹具 2 / 16） | — | 步骤侧不在第 2 步范围 |
| Store Service | 每分区一个循环 | `store_service.max_partitions` | 每个循环一个阻塞读连接 |
| 权威 Redis 连接池 | `redis_authority.pool_size` 缺省 64 | — | saga Mod 与 Store Service 的 Init 各自校验：池大小 ≥ 本进程两者并发之和（协调 + 发布 + 三类消费者 + 分区循环），不满足报错（配置规则只声明一次，在加载时强制） |

### 7.11 第 2c 段：引擎减往返

全部改在 `framework/saga/engine.go`、`step_transition.go`，`stepTransition` 仍是唯一写入口（批量版本 `stepTransitions` 用同一套关闭 / 代际 / 租约规则构造请求）。

1. **O1 完成即派发**：`Complete` 里 `applyCompletion` 得到的 `after` 若是“立即可派发”（Status 为 Pending 或 Compensating、`NextRunAt ≤ now`、定义存在、步骤有效），就用从 `processClaimed` 抽出的 `e.dispatchState(after, definition, now) (Record, *OutboxRecord)` 原地改成 Waiting + 新命令，**版本只加 1**（`applyCompletion` 已加过），一次 `stepTransition(cause: causeResult, receipt, outbox)` 写入。重试退避（`NextRunAt` 在未来）、终态、ManualRequired 不合并。`e.dispatched` 计数、hoptrace 标记照旧。
2. **O2 派发后直接发布**：派发写入的 outbox 带本进程租约（`Owner = opts.Owner`、`Token = 1`、`Until = now + LeaseDuration`），`Apply` 成功后把命令放进进程内有界通道，发布器优先消费通道；通道满就不放（靠领取兜底）。进程崩溃时租约过期，`ClaimOutbox` 接手。
3. **批量派发**：`coordinatorLoop` 领到一批后，先算出每条的目标状态，再一次 `ApplyBatch`。
4. **批量确认**：发布器把确认攒成批（≤ 32 条或 2ms），一次 `DurableScriptBatch`；确认不在交易关键路径上。

### 7.12 指标（登记进 `docs/framework/OBSERVABILITY.md`，根包门禁核对指标名）

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `saga.redis.script_total{script,result}` | Counter | 每个脚本的调用与结果码；§8 用它算每笔脚本数 |
| `saga.redis.script_seconds{script}` | Histogram | 脚本（含 WAITAOF）往返耗时 |
| `saga.redis.waitaof_unconfirmed_total` | Counter | `local = 0`；非零即部署违约 |
| `saga.redis.script_partial_total` | Counter | 校验后写失败；非零要人核对 |
| `saga.redis.cold_read_total{op}` | Counter | 回查 Mongo 冷副本 |
| `saga.redis.reload_total{result}` | Counter | 装回 |
| `saga.redis.backpressure_total` | Counter | Create 因积压被拒 |
| `saga.redis.resident_records{state}` | Gauge | `ZCARD due` / 各 `evict:<p>` 之和 |
| `store_service.backlog_entries{projection,partition}` | Gauge | `XLEN log:<p>` |
| `store_service.lag_seconds{projection}` | Gauge | 最老未投影条目的年龄（取各分区最大） |
| `store_service.batch_entries{projection}` | Histogram | 每批条数 |
| `store_service.batch_seconds{projection}` | Histogram | 每批 Mongo 写耗时 |
| `store_service.mongo_ops_total{projection,collection}` | Counter | bulkWrite 次数 |
| `store_service.paused_partitions{projection}` | Gauge | 暂停的分区数；非零健康 Fail |
| `store_service.evicted_total{projection}` | Counter | 驱逐数 |
| `store_service.lease_lost_total{projection}` | Counter | 分区租约丢失 |

### 7.13 对外 API、配置与升级（项目未上线，不做兼容）

#### 7.13.1 API

| 变化 | 位置 |
| --- | --- |
| `coresaga.Assemble` 加 `authority fredis.IRedis` 参数 | `framework/saga/assembly.go:93` |
| `AssemblyConfig.Redis RedisStoreOptions`；`Assembly.Store` 改为 `*RedisStore`，新增 `Assembly.Copy *MongoStore` | 同上 |
| `MongoStore` 不再实现 `Store`（删除写路径），只作冷副本 | `framework/saga/mongo_store.go` |
| 新错误 `ErrDurabilityUnconfirmed`、`ErrBackpressure`、`ErrRedisAuthorityMismatch` | `framework/saga/errors.go` |
| `NestCompletionConsumerConfig.Workers`、`NestStartConsumerConfig.Workers` | `framework/saga/nest_completion_consumer.go`、`nest_start_consumer.go` |
| `Store.AckOutbox` / `NackOutbox`：命令已被关闭操作删除时返回 `nil`（原为 `ErrConflict`） | `framework/saga/store.go` 注释同步 |
| 可选接口 `BatchApplier`（2c） | `framework/saga/store.go` |
| `wiring/saga.Mod.Store()` 返回 `*coresaga.RedisStore`；`Locations` 加 `Redis RedisStoreOptions` | `wiring/saga/mod.go:264`、`:61-80` |
| 新 Mod：`redis.authority`、`store_service` | `wiring/redis/authority.go`、`wiring/storeservice/` |
| `fredis` 新可选接口（§7.2） | `infra/storage/redis/client.go` |

#### 7.13.2 配置

| 键 | 缺省 | 说明 |
| --- | --- | --- |
| `redis_authority.addr` | 生产必填 | 权威 Redis 单主地址；可以与 `redis.addr` 相同，但该实例必须满足 §4.1（启动探针强制） |
| `redis_authority.password` / `db` | 空 / 0 | |
| `redis_authority.pool_size` / `min_idle_conns` | 64 / 8 | Init 校验 ≥ 并发之和（§7.10） |
| `saga.redis.key_prefix` | 必填，无缺省 | `shared:"saga.redis.key_prefix"`，Store Service 读同一个键 |
| `saga.redis.partitions` | 16 | 1～1024；改值见 §7.6.6 第 6 条 |
| `saga.redis.waitaof_timeout` | 1s | |
| `saga.redis.terminal_retention` | 15m | |
| `saga.redis.business_key_guard` | 10m | 必须 ≥ 10 × `saga.store_timeout`（ValidateConfig） |
| `saga.redis.max_projection_backlog` | 20000 | 每分区 |
| `saga.result_workers` / `saga.result_effect_workers` / `saga.start_effect_workers` | 8 / 8 / 4 | ≤ 对应 `max_ack_pending` |
| `saga.coordinator_claim_batch` / `saga.publisher_claim_batch` | 2c 起 16 / 32 | 原 3 / 1 |
| `saga.collections.outbox` | **删除** | outbox 不再在 Mongo |
| `store_service.batch_size` / `lease_duration` / `block` / `max_partitions` / `owner` | 256 / 10s / 100ms / 0 / 空 | |

Cluster：`redis_authority` 没有 `cluster_addrs` 键；启动探针也会拒绝 Cluster 节点。

#### 7.13.3 合服（R13）

`wiring/sidmerge`：`listNonTerminalSagas`（`migrate.go:396-409`）改为经 `wiring/saga.LoadLocations` 拿到 Redis 设置，读 `K:due`（非终态全部在里面）；检查清单加“所有 `log:<p>` 积压为 0”。合服要求先停协调器与 Store Service。

#### 7.13.4 升级（写进 CHANGELOG `[Unreleased]`）

- 升级前必须用旧版本把所有 saga 跑到终态（新版本启动守卫会拒绝“Mongo 有非终态、Redis 是空的”）。Mongo 里的终态 saga 文档保留为冷副本，新版本可以冷读。
- 旧 `saga_outbox` 集合废弃，确认为空后可删。
- 部署权威 Redis（§4.1）；配置 `redis_authority.*`、`saga.redis.key_prefix`；与协调器同进程或单独进程注册 `store_service`。
- 先停旧再起新。

### 7.14 测试清单与验收命令

测试按功能主题放进包内文件（新组件用新文件，文件名不带编号）：`framework/saga/redis_store_test.go`（单元，用 `scripts/it-redis-aof.sh` 起的私有 Redis；没有 `ROOST_SAGA_IT_REDIS_ADDR` 时跳过）、`framework/saga/redis_store_real_integration_test.go`（`integration` 标签，私有 Redis + 隔离环境 Mongo）、`framework/storeservice/runner_test.go`、`wiring/saga/saga_mod_test.go`。

**先写、先红的用例**（新组件的用例先写，确认它们在桩实现上失败、断言落在承诺本身；2c 的用例在当前代码上必须先红）：

| # | 用例 | 承诺 | 先红的方式 |
| --- | --- | --- | --- |
| 1 | `TestStoreContract`（把现有 Mongo 契约用例抽成 `runStoreContract(t, newStore)`，先在当前 `MongoStore` 上跑绿，证明它测的是承诺；再接 `RedisStore`） | Apply 的 fence、Outbox 替换、回执去重 / 身份冲突、墓碑 closure、领取的到期与租约、Ack / Nack 租约 | 接 `RedisStore` 桩时红 |
| 2 | `TestRedisApplyRetryAfterLostReplyIsApplied` | 回复丢失后同一请求重试 = 已应用，不报冲突 | 用一个“执行后返回 EOF”的连接包装注入；去掉 `la` 检查时红 |
| 3 | `TestRedisScriptValidatesEverythingBeforeWriting` | 校验失败零写入 | 把 `obk` 指向的键改成错误类型；比较前后全部键 |
| 4 | `TestRedisCreateKeepsBusinessKeyUniqueAfterEviction` | 驱逐后同业务键再启动：同意图返回原记录，不同意图 `ErrIdentityConflict` | 去掉 Create 的 Mongo 读时红 |
| 5 | `TestRedisCreateGuardRejectsStaleMongoRead` | Mongo 读到脚本超过 guard/2 时重做 | 注入延迟 |
| 6 | `TestRedisEvictsOnlyAfterProjection` | `ls > proj` 不驱逐 | 投影停住时检查 |
| 7 | `TestRedisLateSuccessAfterEvictionReloadsAndCompensates` | 驱逐后的迟到成功装回、补偿那一步、告警只记一次 | 端到端（内存 JetStream 桩 + 私有 Redis + Mongo） |
| 8 | `TestRedisWaitAOFUnconfirmedFailsStop` | AOF 关闭时启动被拒；运行中 `CONFIG SET appendonly no` 后提交返回 `ErrDurabilityUnconfirmed` 并触发 `RuntimeFailure` | 私有 Redis 改配置 |
| 9 | `TestRedisAuthorityGuardRefusesEmptyRedis` | Mongo 有元数据、Redis 被清空时拒绝启动 | `FLUSHDB` |
| 10 | `TestRedisKill9KeepsAcknowledgedApplies`（integration） | `kill -9` Redis 后，所有回复成功的 Apply 都在；每条记录 `v` 与 `doc`、日志一致，没有半个脚本 | 并发提交中途杀进程、重启后核对 |
| 11 | `BenchmarkRedisClaimDue`（积压 1k / 10k / 100k） | 领取 O(log N)：100k 积压的 P50 ≤ 2 × 1k 积压 | — |
| 12 | `TestProjectionReplayIsMonotonic` | 重放较旧条目不让 Mongo 版本倒退；closure 不降级 | 用 `ReplaceOne` 无条件覆盖的实现时红 |
| 13 | `TestProjectionLostLeaseCannotAdvance` | 旧租约持有者的 `advance` 返回 `LOST` | — |
| 14 | `TestProjectionPausesOnPermanentError` | 唯一索引冲突暂停分区、不跳过、健康 Fail | 预置冲突文档 |
| 15 | `BenchmarkSagaProjection`（integration） | 本机 ≥ 5000 条/s（P=16、批 256） | — |
| 16 | `TestCompleteDispatchesNextStepInSameApply`（2c） | 接收成功后同一次 Apply 已派发下一步：Status=Waiting、Attempt=1、outbox 有新命令、版本只加 1、没有经过 `ClaimDue` | **当前代码红**（记录停在 Pending） |
| 17 | `TestCompleteDoesNotFoldRetryBackoff`（2c） | 可重试失败的退避不合并派发 | 当前代码绿，2c 后保持绿 |
| 18 | `TestDispatchPublishesWithoutClaim`（2c） | 派发后的命令不经 `ClaimOutbox` 发布；崩溃后租约过期由领取接手 | **当前代码红**（`memoryStore` 计数 `ClaimOutbox` 调用 > 0） |
| 19 | `TestAckAfterCloseIsNotAConflict`（2c） | 完成先于确认时，确认返回 nil、不计冲突 | **当前代码红**（`ErrConflict`） |
| 20 | `TestSagaModRejectsSmallAuthorityPool` | 池大小 < 并发之和时 Init 报错 | — |

另外：现有 `framework/saga` 的全部 Mongo 集成用例（`*_real_mongo_integration_test.go`）中把 `MongoStore` 当协调器存储的，改用测试辅助 `newTestRedisStore(t)`；原生 / Mongo 步骤收件箱的用例不动。Saga completion 统一规则（`judgeCompletion`）、迟到成功、Resume、ManualRequired 的用例在 Redis 实现上必须全绿。

**验收命令**（干净 worktree；主检出 `artifacts/` 下有 `.go` 备份会干扰 `go build ./...`）：

```sh
eval "$(scripts/it-redis-aof.sh start)"     # 导出 ROOST_SAGA_IT_REDIS_ADDR；私有目录与端口
source ~/.roost-it/roost-dataengine-it/env.sh  # Mongo / NATS；只 source，不打印
GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go vet -tags integration ./framework/saga/... ./wiring/saga/...
GOWORK=off go test -race -count=1 ./infra/storage/redis/... ./framework/saga/... ./framework/storeservice/... ./wiring/redis/... ./wiring/saga/... ./wiring/storeservice/... ./wiring/sidmerge/...
GOWORK=off ROOST_DATAENGINE_IT=1 go test -tags integration -count=1 -run 'Redis|Projection|StoreContract' ./framework/saga ./framework/storeservice
GOWORK=off go test ./...            # 跨包行为改动，push 前必须全量
GOWORK=off go test -count=1 .       # 根包门禁（含指标名核对）
# 生成模板改动：按维护手册重新生成 game-demo，build / vet / test；go generate ./... 后 porcelain 干净
scripts/it-redis-aof.sh stop
```

## 8. 实施后的 Saga 压测

维护者 2026-10-26 要求第 2 步之后跑一次 Saga 压测。

### 8.1 环境

- **Mongo / NATS**：`~/.roost-it/roost-dataengine-it` 隔离环境（Mongo 副本集 3 节点、JetStream 三节点）。脚本持 `remote-acceptance.lock`；不并行跑其他容量测试。
- **Redis**：私有单主 `redis-server`（`scripts/it-redis-aof.sh`，端口 26391 起、私有目录），`--appendonly yes --appendfsync always --no-appendfsync-on-rewrite no --maxmemory 2gb --maxmemory-policy noeviction --save ""`，无副本。不用共享环境的 17379。
- **机器**：本机 macOS，GOMAXPROCS=4（Saga 历史口径），开跑前 loadavg 门控（`saga-scan.sh` 已有）；跑前看 `sysctl vm.swapusage`。

### 8.2 场景与档位（沿用 `scripts/perf/saga-scan.sh`）

- 场景：`native-forward`、`native-compensate`、`mongo-forward`。
- 档位：`ROOST_SCAN_LEVELS="1 4 16 32 64 128"`，每档 60s（稳态窗口 `[10s, 60s)`）。
- Mongo 步骤 worker：`ROOST_SCAN_MONGO_WORKERS=2` 与 `16` 各一轮。
- 每档至少 2 个样本；对照与新版本**交替**跑（A、B、A、B），不连续跑同一边。

### 8.3 对照

- **基线**：当前 main（`30243c9b`）的干净 worktree，只 cherry-pick “压测脚本改动”这一个提交（§8.5），不带任何产品代码。
- **新版本**：第 2 步 2a+2b 完成后一轮，2c 完成后再一轮。

### 8.4 指标与测量方法

| 指标 | 方法 |
| --- | --- |
| 每笔 Mongo 往返 | 每档前后取 `serverStatus().metrics.commands` 各命令 `total`（insert、update、delete、find、findAndModify、aggregate、bulkWrite、commitTransaction、abortTransaction）的增量之和 ÷ 稳态终态笔数；同时报 `opcounters`（文档级）与 `transactions.totalCommitted`。Store Service 的后台写单列（按 `store_service.mongo_ops_total` 增量） |
| 每笔 Redis 脚本 | 每档前后取 `INFO commandstats` 的 `cmdstat_evalsha` + `cmdstat_eval` 的 `calls` 增量 ÷ 终态笔数；按脚本拆分用 `saga.redis.script_total{script}` |
| 吞吐 | 稳态成功 TPS（`SteadyTPS`，只计期望终态） |
| 延迟 | 服务端 P50 / P99（夹具现有口径）；c1 另跑逐跳时间线（`ROOST_SCAN_HOPS=1`） |
| 终态核对 | 夹具的账户 / 业务核对；ManualRequired、Failed 计数；终态复读（报告 §7 提到夹具只记首次终态，这一项要补：结束时按 ID 复读最终状态） |
| 冷热一致 | 每档结束、停止加压后等 Store Service 积压归零（记录归零耗时），再核对：每个驻留记录的 Mongo 版本 = Redis 版本；Mongo 中终态数 = 夹具终态数 |
| Store Service | 积压峰值、`lag_seconds` 峰值、批大小分布 |
| 资源 | 进程 CPU、Redis `used_memory` 峰值、AOF 大小增量 |
| 残留 | 结束后按前缀核对 Mongo 库、JetStream 流、Redis 键为 0 |

### 8.5 需要补的脚本与夹具（单独一个提交，便于基线 cherry-pick）

- `scripts/perf/saga-scan.sh`：`ROOST_SCAN_STORE=redis` 时用 `scripts/it-redis-aof.sh` 起私有 Redis，导出 `ROOST_SAGA_PERF_REDIS_ADDR` 与唯一键前缀；每档前后各取一次 `INFO commandstats`、`INFO memory`；结束后按前缀删键并核对残留。
- `scripts/perf/mongo-ceiling -snapshot`：加 `metrics.commands` 与 `opcounters`。
- `scripts/perf/saga-scan-summary.py`：输出每笔 Mongo 往返、每笔 Redis 脚本两列。
- 夹具（`framework/saga/performance_real_integration_test.go`、`wiring/saga/steps_performance_real_integration_test.go`）：有 Redis 地址时装配 `RedisStore` 与进程内 Store Service；结束时终态复读与冷热核对。

### 8.6 预期（用于判断结果是否合理，不是承诺）

| 场景 | 预期 | 依据 |
| --- | --- | --- |
| mongo-forward c1 | P50 约 198ms → 约 130ms（2a）→ 约 105ms（2c） | 报告 §4.1：协调事务 64ms + 等待认领 25ms + outbox 排队 23ms；Redis 脚本约 4ms × 每步 5 次（2a）/ 2 次（2c） |
| mongo-forward w16 饱和 | 约 43 TPS → ≥ 65（2a 门槛）→ ≥ 86（2c 门槛） | 上限从发布器（每条 3 次 Mongo 串行）移到步骤消费者（每步约 33ms × 16 worker） |
| mongo-forward w2 饱和 | 基本不变 | 上限在步骤消费者 |
| native-forward / compensate 饱和 | 基本不变（约 10 / 6 TPS） | 上限在步骤侧 DataEngine 投影器（报告 §4.2），第 2 步不动 |
| native c1 P50 | 约 277ms → 约 200ms | 协调事务 64ms + 唤醒认领 51ms 中大部分被替换 |
| 每笔 Mongo 往返 | 原生两步约 65 → 约 33（步骤侧 32 + 启动读 1）；Mongo 步骤两步约 49 → 约 17 | §3.2 |

## 9. 第 3、4 步设计要点

第 3、4 步在第 2 步验收后各自出实施细则（像 §7 一样写到函数级），这里只固定方向与已采纳的规则。

### 9.1 第 3 步：Remote

- **提交**：准备脚本（取锁 + 许可 + fence 一次）、提交脚本（权威数据 / 版本 / 元数据 / 回执 / outbox / 解锁一次）；Mongo `GrantWrite` 与 `_grant_fence` CAS 从在线路径移除（`framework/remoteentity/versioned_lock.go:150-170`、`mongo_committer.go:470-490`），fence 在 Redis 元数据里单调递增。
- **一致快照**：多实体事务的快照与回执来自同一个提交脚本（草稿 §6），Store Service 投影时不读“最新值”。
- **共享模式**（R16）：EnterShared / LeaveShared / Transfer 与多写者仲裁写成等价 Lua，原子语义与 `framework/remoteentity/mongo_authority.go:142-180` 一致。
- **fence 与灾备**：单主 + `always` 下进程崩溃 / 重启不回退；灾备恢复（可能回退）后，运维执行“fence 代际加一”（`K:meta.fence_epoch` + 1，所有 fence 比较按 (代际, 计数)），一个命令完成，写进灾备手册。
- `cluster_fault_integration_test.go:317-470` 测的是 Sentinel 切主下的计数器回退，属于不支持的拓扑：改写为“`kill -9` Redis 后 fence 不复用”的用例，原用例删除并在提交说明里写明原因。
- 持久拒绝、Sync 冻结与交付、快照发布与业务回复的先后，保持 `docs/framework/REMOTE_ENTITY.md` 的现行契约。

### 9.2 第 4 步：普通 Entity RedisActive

采用草稿 §4 的 Local → Entering → RedisActive → Closing 状态机，并加上：

- 路由唯一入口在 `Projector.reserve`（与屏障、卸载屏障同一临界区，`framework/dataengine/engine/projector.go:578-610`）：RedisActive 实体的记录一律拒绝进本地 WAL。
- R3 / R6 / R8：io 阶段提交、写闸门挡住所有锁定该实体的事务、不走 pipelined。
- R4：Entering 等 checkpoint 覆盖；交接点持久写进段记录。
- R7：只有原生步骤触发 Entering；其他事务碰到 RedisActive 又写 Local 时准入前可重试拒绝。
- R9：冷加载 = Mongo 底版 + 段尾回放；Closing 之后的本地记录等段投影到接续版本再回放；RedisActive 拒绝卸载。
- R10：批量心跳遇到 RedisActive 实体转单目标 io 阶段提交。
- R15：Closing 条件含“无已预留未提交的步骤尝试”。
- R17：全部 effect 进 Redis outbox。
- 步骤收件箱迁 Redis：执行权（租约 fence）与实体提交在同一个脚本里裁决，“准入即裁决、不再跳过”。此后“原生步骤不能改 Remote”的限制理论上可以放开，但需要独立验收，首版继续拒绝。

## 10. 方向判断

### 10.1 与已删除的 Redis WAL（`66cfea12`）的区别（R18）

`66cfea12`（2026-09-01）删除的 `checkpoint/redis_wal.go` 是 cube-core 时代的“Redis 快照 WAL”：整实体快照、异步队列写入（队列 4096、4 个 worker）、键带 `PEXPIRE` TTL、没有 fsync 确认，Redis 与 Mongo 之间没有单一提交位置和交接。v2 的不同：

| 方面 | 旧 Redis WAL | v2 |
| --- | --- | --- |
| 确认 | 异步入队即返回 | 脚本回复 + 同连接 WAITAOF，`appendfsync always` |
| 数据 | 整实体快照 | Saga：记录 after-image；Remote / Entity：事务记录（复用 CommitRecord 语义） |
| 回收 | TTL 过期 | 只在 Mongo 确认后裁剪 / 驱逐；`noeviction`；未投影数据不设 TTL |
| 权威 | 不明确（Redis 与 Mongo 都可能被当成最新） | 驻留即 Redis 权威，驱逐即 Mongo 权威，交接带版本 |
| 拓扑 | 未限定 | 单主，启动探针强制 |

### 10.2 Saga 反复出问题的模块

已知反复：完成判定（已统一为 `judgeCompletion`）、认领争用（`9dd9806a`）、逐跳诊断。这一轮的证据（报告）把问题收敛到“串行池 × 往返次数”，不是完成判定的细节问题，所以方向是结构调整（存储一笔一脚本 + 并发模型），不是再修细节。风险控制：

- `stepTransition` 仍是唯一写入口，Store 只换实现；`judgeCompletion` 不动。
- 2a 只换存储、不改引擎，单独测一轮；2c 的引擎改动单独提交、单独红绿、单独测一轮。出问题能定位到段。
- 第 2 步不碰步骤侧，原生步骤的正确性风险集中在第 4 步。

## 11. 需要维护者拍板的点

只列推荐不能代替的产品选择：

1. **权威 Redis 在生产上是否独立部署。** 推荐：独立实例（AOF always 让每次写都 fsync，会拖慢同实例上的缓存类服务，`noeviction` 也不适合缓存）。配置上允许 `redis_authority.addr` 与 `redis.addr` 相同（开发环境方便），只要该实例满足 §4.1。
2. **原生步骤吞吐是否要在第 4 步之前抬高。** 第 2、3 步都不改变原生步骤约 10 TPS（本机）的上限，它在步骤侧 DataEngine 投影器。推荐：第 2 步压测之后，若原生步骤吞吐是业务目标，先做 O4（投影事务合并写，审查 §6.4，投影吞吐约翻倍、Local 实体也受益），再决定是否推进第 4 步；否则按 3 → 4 顺序推进。
3. **Mongo 领取查询的阻塞 SORT 修复（报告 §5）是否还做。** 2a 落地后 Mongo 不再承担领取，这个修复只对过渡期有效。推荐：2a 能在近期合入就不单独修；若 2a 推迟，按报告推荐 A 加两个索引。

## 12. 未验证与限制

- 本文所有次数、吞吐与延迟预期来自源码与报告的实测段占比推算，没有运行新代码；第 2 步压测（§8）给出实测。
- `appendfsync always` 下“回复晚于 fsync、读者看不到未落盘数据”与“AOF 中不完整的脚本事务加载时整段回退”是对 Redis 实现的理解，由 §7.14 第 10 条用例在本机 Redis 8.10.1 上验证；托管 Redis 若行为不同，以启动探针和该用例为准。
- 只覆盖 macOS / Linux；本机只有 macOS，Linux 未测。
- 云上数据只有报告中 60ms RTT 一个时段；v2 不做云上与同地域测试。

## 13. 实施结果（第 2 步 2a / 2b）

实施在分支 `feat/redis-authority-2ab`（基于 main `c74376c7`），代码提交按顺序：压测测量（`891d93c0`，可单独 cherry-pick 到 main 作基线）→ 2a（`6709eb90`）→ 2b（`a3edef04`）→ 压测夹具连接池取生产缺省（`2aad947c`）→ 协调器往返计数用例（`871a6c77`）。§11 的三项按推荐先做：权威 Redis 推荐独立实例，配置允许与 `redis.addr` 相同、启动探针强制部署契约；原生步骤吞吐不在第 4 步前处理；Mongo 领取查询的 SORT 索引不单独修（2a 之后 Mongo 不再承担领取）。

### 13.1 2a：存储替换（引擎不动）

| 设计项 | 落点 |
| --- | --- |
| 驱动可选接口（§7.2） | `infra/storage/redis/client.go`：`DurableScriptBatcher`、`StreamReader`、`HashFieldsReader`、`ScriptLoader`、`ServerInspector`、`ErrWaitAOFFailed`；实现 `infra/storage/redis/driver/durable_script.go`、`stream.go`；契约 `driver/README.md` §1 / §2 |
| 启动探针（§7.6.6 第 1～3 条） | `infra/storage/redis/authority.go` 的 `CheckAuthority`（单主、四项持久配置、探针写 WAITAOF local=1），`wiring/redis.AuthorityMod.Start` 与 `RedisStore.EnsureInfrastructure` 共用 |
| `RedisStore` 与 8 个脚本（§7.5） | `framework/saga/redis_store.go`、`redis_codec.go`、`redis_scripts.go`、`redis_startup.go`、`redis_lua/{create,claim_due,claim_outbox,apply,ack_outbox,nack_outbox,mark_alarm,meta}.lua` |
| `MongoStore` 收缩为冷副本 | `framework/saga/mongo_store.go`：删 `Create`、`ClaimDue`、`Apply`、`ClaimOutbox`、`Ack/NackOutbox`、`MarkLateSuccessAlarm` 与 outbox 集合；只读 + `recordExists`（业务键守卫） |
| 装配与消费者并发 | `Assemble(mongo, authority, jetStream, cfg, …)`；`NestCompletionConsumerConfig.Workers` / `NestStartConsumerConfig.Workers`（`withConsumerWorkers`）；`wiring/saga` 新键 `saga.redis.*`、`saga.result_workers` / `result_effect_workers` / `start_effect_workers`（8 / 8 / 4） |
| 权威 Redis Mod | `wiring/redis/authority.go`（`redis_authority.*`，能力 `mods.ModRedisAuthority`）；saga Mod `DependsOn` 加它；连接池不小于并发之和由 `CheckConfigAcrossMods` 强制 |
| 合服（§7.13.3） | `wiring/sidmerge`：非终态 saga 读权威 Redis（`RedisStore.List`），另要求投影积压为 0 |
| 生成器 / 本地依赖 | catalog 加 `redis_authority`（saga 依赖）；开发 compose 的 Redis 在项目有它时按 `appendfsync always` + `noeviction` 起；`kitconfig_gen.go` 重新生成；`scripts/mirror-local.sh` 起权威 Redis 并导出 `ROOST_SAGA_IT_REDIS_ADDR`；`scripts/it-redis-aof.sh` |

### 13.2 2b：Store Service

| 设计项 | 落点 |
| --- | --- |
| 投影运行器（§7.9.1） | `framework/storeservice`（`projection.go`、`runner.go`、`lua/plock.lua`、`lua/advance.lua`） |
| saga 投影与驱逐（§7.9.2、§7.5.7） | `framework/saga/redis_projection.go`、`redis_lua/evict.lua`：每批 ≤ 3 次 BulkWrite（`sagas` 用 `$replaceWith` + `$cond` 版本单调，`completions` `$setOnInsert`，`operations` closure 只升不降、告警只写第一次） |
| 冷记录装回（§7.6.4、§7.5.6） | `RedisStore.reload` + `redis_lua/reload.lua`；`MongoStore.loadForReload`；`Apply` / `MarkLateSuccessAlarm` 遇到 `NEED_RELOAD` 装回后重跑一次 |
| Store Service Mod | `wiring/storeservice`（`store_service.*`，依赖 `mongo`、`redis.authority`；暂停分区时健康 Fail）；生成器 catalog 加 `store_service`（saga 依赖它，与协调器同进程注册） |
| 公共 Lua 前言 | 两个包都要，移到驱动：`infra/storage/redis/lua/prelude.lua` + `lua_script.go`（`NewLuaScript`、`PreloadScripts`、`ReplyCode`） |

### 13.3 与本文设计的偏差

1. **`apply.lua` 的回执检查放在请求摘要（`la`）之前**（§7.5.3 原写“`APPLIED_BEFORE` 放在回执检查之前”）。原顺序下，两次送达可能算出逐字节相同的请求（`Complete` 用 `time.Now()` 作 `CompletedAt`，macOS 上同一微秒），第二次也得到 `APPLIED_BEFORE`、按“已应用”报告，迟到成功被计两次、重开被报两次。实测 `TestRealMongoLateSuccessReopensACompensatedSagaOnce` 红：`round 1: 2 of 8 concurrent deliveries brought the saga back to compensation, want exactly 1`；确定性回归（契约用例）在旧顺序上红：`store_contract_test.go:212: the identical receipt-bearing request delivered again = 1, <nil>; want ApplyDuplicate`，新顺序绿。带回执的重试现在报“重复”（与 Mongo 实现一致，引擎按重复重读）；不带回执的请求在回复丢失后原样重试仍得到“已应用”（`TestRedisApplyRetryAfterLostReplyIsApplied`）。
2. **启动探针的写**不再是 saga 自己的 `probe.lua`，而是驱动的 `CheckAuthority`（`HINCRBY <前缀>:meta probes 1`），权威 Redis Mod 与 `RedisStore` 共用。
3. **2a 的冷记录告警**：2a 没有驱逐，`MarkLateSuccessAlarm` 遇到“记录不在 Redis”时只按冷副本有没有 tombstone 回答（有就告警，宁可重复）；2b 改为装回后原子记一次（§7.6.4）。
4. **指标**：`store_service.batch_entries` 直方图改为两个计数器 `store_service.projected_entries_total` / `store_service.batches_total`（指标库的直方图只收时长）；`store_service.lag_seconds` 是整秒。`saga.redis.resident_records{state}` 由 saga 投影在每次 `AfterAdvance` 更新（只有跑 Store Service 的进程上报）。
5. **身份检查更严**：`CompletionHistory` 遇到操作键不以 `<sagaID>:` 开头的 completion 返回 `ErrIdentityConflict`（Mongo 实现只在另一笔 saga 的 tombstone 存在时才冲突）；`MarkLateSuccessAlarm` 对这样的 completion 返回 false。
6. **元数据守卫**多拒一种：Redis 有实例 ID、Mongo 没有 saga 元数据（冷副本被清空或换了库）。
7. **`saga.redis.key_prefix` 必填**：凡读 saga 配置的进程（包括只跑步骤的进程，`StreamSettings`）都要写；生成器按示例 `roost:{project}:saga` 写进每个服务的配置。
8. **压测夹具拆成两步提交**：§8.5 说“单独一个提交、便于基线 cherry-pick”，但“有 Redis 时装配 RedisStore 与 Store Service、冷热核对”只能在 2a / 2b 之后编译，所以测量提交（`891d93c0`）只带 main 能编译的部分（命令计数、终态复读、脚本），Redis 装配在 2a、Store Service 与冷热核对在 2b。Mongo 步骤夹具沿用基线的结果消费者并发（`ROOST_SAGA_PERF_WORKERS`），没有换成新缺省 8，两边同配置。
9. **单元用例的 Redis**：`redistest.AOFAddr` 优先用 `ROOST_SAGA_IT_REDIS_ADDR`，没有时为每个用例起一个私有 `redis-server`（AOF always），本机没有 `redis-server` 时跳过。CI 的主测试 job 没有 `redis-server`，这些用例在那里跳过（登记 KNOWN-LIMITS）。
10. **压测夹具的连接池**：Mongo 步骤压测夹具的权威 Redis 连接池原为 32（跨进程辅助函数的值），比生产缺省 64 小，改成 64 / 最小空闲 8（`2aad947c`）；原生步骤夹具走 wiring 装配，本来就是 64。32 下的一轮样本与 64 下没有可分辨差异，保留但不计入对照。

### 13.4 先红后绿与变异核对

**§7.14 中“当前代码上应先红”的用例，在 main（`c74376c7`）上实测**（当时的文件名 `round_trip_red_test.go`，提交时改名 `engine_round_trip_test.go`）：

```text
--- FAIL: TestCompleteDispatchesNextStepInSameApply (0.00s)
    round_trip_red_test.go:60: after the first step's success: status=pending step=1 attempt=0 version=3 (before 2); want Waiting on step 1, attempt 1, version +1
--- FAIL: TestDispatchPublishesWithoutClaim (0.00s)
    round_trip_red_test.go:93: the dispatched command went through ClaimOutbox 1 time(s); want it handed to the publisher directly
--- FAIL: TestAckAfterCloseIsNotAConflict (0.00s)
    round_trip_red_test.go:108: ack after the completion closed the operation = saga: optimistic concurrency conflict (ErrConflict=true); want nil
```

第 16、18 条属于 2c，提交时带 `t.Skip`（`framework/saga/engine_round_trip_test.go`，2c 实施时去掉）；第 19 条随 2a 的 `ack_outbox.lua`（`GONE → nil`）在 `RedisStore` 上变绿（`TestAckAfterCloseIsNotAConflict` 改跑 RedisStore，`memoryStore` 同步改口径）。第 17 条（`TestCompleteDoesNotFoldRetryBackoff`）在当前代码上绿，2a / 2b 后保持绿。

**第 1 条 `runStoreContract`** 先在 main 的 `MongoStore`（mongotest）上跑：除有意改变的“确认已删除的命令返回 nil”外全绿——

```text
--- FAIL: TestStoreContractOnMongoStore/ack_after_the_operation_closed_is_not_a_conflict (0.00s)
    store_contract_test.go:328: ack of a command whose operation closed = saga: optimistic concurrency conflict, want nil
```

再接 `RedisStore` 全绿（`TestStoreContract`）。

**Lua 校验项变异**（每次删掉一个 `-- check:<名> … -- end` 块，跑相关用例；脚本 `artifacts` 外，结果全文见 `docs/performance/benchmarks/saga-redis-authority-2ab-20261027/mutation.tsv`）：41 个校验项全部变红。

| 脚本 | 校验项 → 变红的用例 |
| --- | --- |
| `create.lua` | types → `TestRedisScriptValidatesEverythingBeforeWriting/create`；exists_id、exists_bk → `TestStoreContract/create,_read_and_identity`；guard → `TestRedisCreateGuardRejectsStaleMongoRead`；backpressure → `TestRedisCreateBackpressureRefusesNewSagas` |
| `claim_due.lua` / `claim_outbox.lua` | type → `…ValidatesEverythingBeforeWriting/claim_due`、`/claim_outbox` |
| `apply.lua` | types、need_reload、close_owner、close_types、close_command_type、outbox_types、outbox_old_type → `…ValidatesEverythingBeforeWriting/apply:*`；receipt → `TestStoreContract/receipt_dedup_and_identity_conflict`；applied_before → `TestRedisApplyRetryAfterLostReplyIsApplied`；version、lease → `TestStoreContract/apply_fences_version_and_lease` |
| `ack_outbox.lua` / `nack_outbox.lua` | types、index_type → `…ValidatesEverythingBeforeWriting/ack*`、`/nack*`；gone → `TestStoreContract/ack_after_the_operation_closed_is_not_a_conflict`；lease → `TestStoreContract/outbox_claim,_nack_backoff_and_ack_lease` |
| `mark_alarm.lua` | types → `…/alarm:_wrong-type_log`；need_reload → `TestRedisLateAlarmReloadsAnEvictedRecord`（integration）；tombstone → `TestStoreContract/tombstone_*`、`TestRedisStoreMarksALateSuccessAlarmOncePerLife` |
| `meta.lua` | type、instance、format、partitions → `TestRedisAuthorityGuardChecksInstanceFormatAndPartitions/*` |
| `evict.lua` | types → `TestRedisReloadAndEvictValidateBeforeWriting`；projected → `TestRedisEvictsOnlyAfterProjection` |
| `reload.lua` | types、exists → `TestRedisReloadAndEvictValidateBeforeWriting` |
| `plock.lua` / `advance.lua` | type、held、types、lease、noop → `TestProjectionLostLeaseCannotAdvance` |

两处 Go 侧守卫也做了变异：去掉 `Create` 的 Mongo 读 → `TestRedisCreateKeepsBusinessKeyUniqueAfterEviction` 红（`same intent after eviction = {… Status:pending …}; want the original record`）；去掉 `CompletionHistory` 的摘要比较 → `TestStoreContract` 红（`history of a forged receipt = <nil>, want ErrIdentityConflict`）。

新组件的其余用例（§7.14 第 2～15、20 条）与实现同轮写成，没有单独在桩实现上跑红；它们断言落在承诺本身，由上面的变异逐项证明。

### 13.5 验证

（2a、2b 各一遍；命令的退出码直接读。）

| 项 | 2a（`6709eb90`） | 2b（`a3edef04`） |
| --- | --- | --- |
| gofmt、`go vet ./...`、`vet -tags integration`（saga / storeservice / wiring / redis） | 通过 | 通过 |
| `go run ./cmd/glsvet ./framework/saga/... ./framework/nest/...` | 通过（退出码 0） | 通过 |
| `-race`：`framework/saga/...`、`framework/storeservice/...`、`wiring/saga/...`、`wiring/storeservice/...`、`wiring/redis/...`、`wiring/sidmerge/...`、`infra/storage/redis/...` | 通过 | 通过 |
| integration（私有 Redis + 隔离环境 Mongo / NATS）：`-run 'Real\|Redis\|StoreContract\|Projection'` 的 framework/saga、framework/storeservice、wiring/saga、wiring/sidmerge | 通过（含 `kill -9`、跨进程 SIGKILL 恢复） | 通过（含投影、驱逐、装回） |
| `TestRedisKill9KeepsAcknowledgedApplies`（私有 redis-server，SIGKILL 后重放 AOF） | 5/5 通过；每次约 270～370 个已确认 Apply、20～30 个结果未知，重启后无丢失、无半个脚本，NOSCRIPT 重发成功 | — |
| `BenchmarkSagaProjection`（P=16、批 256、本机隔离 Mongo） | — | 20528 / 21850 / 27393 条/s（GOMAXPROCS 10），22724 / 22943 条/s（GOMAXPROCS 4）；门槛 ≥ 5000 |
| `TestRedisRoundTripsPerTwoStepSaga`（`framework/saga/redis_store_test.go`，20 笔两步正向，按脚本计数） | — | 每笔正好 11 个脚本 + 2 次 HMGET（§3.2 的 11），空领取另计；`871a6c77` |
| `BenchmarkRedisClaimDue`（积压 1000 / 1 万 / 10 万） | — | 每次领取 4.10 / 4.08 / 4.11ms，不随积压变化（单次即一次空闲盘 F_FULLFSYNC） |
| 根包 `go test -count=1 .` | 通过 | 通过 |
| `scripts/test-sync-modes-generated.sh` | 通过 | 通过 |
| `scripts/test-demo-generated.sh`（持锁；含生成工程 saga 原生步骤真实依赖用例 `TestGeneratedSagaNativeStepsAndEffectConsumer`） | 通过 | 通过（生成工程带 Store Service） |
| game-demo 生成工程 build / vet / test | 通过（26 个包 ok） | 通过 |
| `go generate ./...` 后无改动 | 是 | 是 |
| 全量 `GOWORK=off go test -count=1 ./...` | 通过（156 个包 ok，在 2a 提交的干净 worktree 上跑） | 通过（158 个包 ok） |

### 13.6 压测与门槛判定（§8）

报告：[SAGA-REDIS-AUTHORITY-2AB-20261027](../performance/SAGA-REDIS-AUTHORITY-2AB-20261027.md)（样本在 `docs/performance/benchmarks/saga-redis-authority-2ab-20261027/`）。基线 = `c74376c7` + 测量提交 `891d93c0`；新版本 = 2a + 2b + `2aad947c`。按 §8.1 的环境（权威 Redis 的 AOF 与 Mongo、NATS 同一块盘），A / B / A / B 各两轮；另把权威 Redis 的 AOF 放内存盘跑一轮作诊断（不是部署口径，不用于判定）。

| 门槛 | 正式口径（共享盘） | 内存盘诊断 | 判定 |
| --- | --- | --- | --- |
| 2a：单元 + 集成全绿含 `-race`、`kill -9` | §13.5 | — | 达标 |
| 2a：协调段 Mongo 操作 = 0 | Mongo 步骤前台每笔 50 → 19.0（步骤侧 + 启动守卫读 1），`commitTransaction` 8 → 4 | 同 | 达标 |
| 2a：Redis 脚本数与 §3.2 一致 | 单元计数每笔 11 + 2 次 HMGET | — | 达标 |
| 2a：Mongo 步骤 w16 饱和 ≥ 1.5 × 基线 | 约 22 vs 45 TPS（0.47～0.52 倍） | 143～148 TPS（3.1～3.3 倍） | **不达标** |
| 2a：Mongo 步骤 c1 P50 ≤ 0.85 × 基线 | w2 195 / 192ms（1.02 倍）、w16 196 / 179ms（1.09 倍） | 0.61 / 0.64 倍 | **不达标** |
| 2a：原生正向 / 补偿饱和 ≥ 0.95 × 基线 | 0.78～0.83 / 0.84～0.87 | 1.00～1.35 | **不达标** |
| 2a：非过载档终态与账户核对 | c1～c64 全部通过 | 同 | 达标 |
| 2b：投影 ≥ 5000 条/s | 2.05 万～2.74 万条/s | — | 达标 |
| 2b：积压 ≤ 2 × 在途、停止后 30s 内归零 | 峰值 2～28 条，c16 起 ≤ 1.6 倍并发数；c1 / c4 的 Mongo 步骤档 3 / 1、10 / 4；归零 3～8ms | — | **达标**（维护者 2026-10-27 确认按笔计：积压条数 ÷ 每笔约 5 条日志 ≤ 2 × 在途笔数；全部档位 ≤ 0.6 倍，c1 0.6、c4 0.5；原先按“条数 ≤ 2 × 笔数”的字面口径 c1 / c4 为 3 / 1、10 / 4） |
| 2b：冷副本 0 差异 | 全部档位 0 | 同 | 达标 |
| 2b：驱逐后迟到成功装回并补偿 | 集成用例通过 | — | 达标 |

**不达标的原因**：Redis 在 macOS 上 `appendfsync always` 用 `fcntl(F_FULLFSYNC)`（Linux 用 `fdatasync`），且在主线程上同步落盘、期间不处理任何命令。本机这块盘空闲时一次约 4ms，压测中与 Mongo 三节点 journal、NATS 同盘，Redis 的 `aof-fsync-always` 典型 16～18ms、最大 103ms。每个脚本往返因此是 50～165ms（main 上同一位置的 Mongo 往返 17～35ms），worker 2 时结果消费者、worker 16 时发布器先饱和。AOF 放内存盘后同一套代码各跳降到 1.5～2.3ms、三项门槛都过，说明往返收益成立，差距落在落盘代价这一部署条件上。

**未验证**：Linux（`fdatasync`）与权威 Redis 独占一块盘的条件都没有测；不能拿内存盘数字当 Linux 预期。

**待维护者决定**（§6 规定 2a 门槛全部满足才进下一步）：A（推荐）2a / 2b 合入 main，2a 性能门槛改在 Linux、权威 Redis 独占盘的条件下复测判定，同时开始 2c（每步脚本 5 → 2，正对落盘次数）；B 不合入，在分支上继续 2c 后一起测；C 放宽持久性（违背 AOF always + WAITAOF 的决定，不推荐）。代价见报告 §6。
