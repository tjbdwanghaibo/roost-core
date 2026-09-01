# Data Engine 迁移与运行手册

本文描述从 legacy Checkpoint 写引擎迁移到统一 Data Engine 的开发契约、发布顺序、
回滚边界和生产门禁。当前阶段已经具备 WAL、Mongo projection、outbox、聚合加载、
schema migration、Saga native step 和 Remote Entity 的统一实现；legacy Checkpoint
代码只作为迁移期旧引擎保留，不能与 Data Engine 同时写。

## 1. 一致性边界

一次成功的持久化 Nest handler 只产生一个 `dataengine.CommitRecord`：

```text
Nest transaction
  ├─ Put / Patch / Delete mutations
  ├─ receipts（幂等结果）
  └─ effects（可靠消息意图）
          │
          ▼
local WAL ──► Mongo projection transaction ──► WAL ACK
                       │
                       └─► Mongo outbox ──► JetStream publisher
```

WAL admission 是本地提交点。Mongo projection 按文档版本 CAS，事务 receipt 和 effect
outbox 与业务文档在同一个 Mongo transaction 中落地；无 effect/receipt 的单文档写走
非 session 快路径。WAL ACK 只等待 Mongo projection，不等待 NATS。NATS 故障因此只会
增加 outbox backlog，不会阻塞 Entity version 推进。

持久化变化只存在于当前 Nest/system transaction 的 `PersistChange` 中。DAO tracker
只保存已接受的持久化版本和 sync dirty，不存在“事务外持久化 dirty 聚合”。Entity
release 不编码 BSON、不写 Redis/Mongo，也不提交 journal。

## 2. 开发者契约

### 2.1 写入必须处于事务中

- 带持久化属性的生成 setter/map mutator 只能在 Nest handler 或显式 system
  transaction 中调用；事务外调用会失败并 panic，避免内存已改而没有 durable record。
- `durability=memory` 的 handler 不能修改 persistent DAO。Memory 只适合纯内存或仅
  sync 的状态；低隔离持久化操作仍应抽象成 async transaction。
- 外部 RPC、NATS publish 和不可回滚 I/O 不放在 Entity 锁内，使用 effect/outbox 或 Saga。

### 2.2 Mutation 选择

| 情形 | Mutation | 说明 |
| --- | --- | --- |
| 新文档、迁移替换、全字段替换、当前版本为 0 | `Put` | 携带完整 BSON 与 schema |
| 已存在文档的普通字段/map key 修改 | `Patch` | 只携带 `$set`/`$unset`；不附 full fallback |
| Entity/DAO 删除 | `Delete` | 写入带版本的 tombstone，禁止无版本物理覆盖 |

Map key 能形成安全 BSON path 时生成字段 patch；不安全 key 或嵌套修改退化为该字段的
完整 patch，但不退化成整 DAO `Put`。同一事务对同一 DAO 的修改会合并成一个 mutation。

### 2.3 Load 与 migration

`EntityRepository` 是 Data Engine 的冷加载入口。相同 Entity ID 并发 miss 由
single-flight 合并；聚合的所有 DAO 在 Mongo snapshot transaction 中读取，缺文档、
重复文档、ID 不匹配和 tombstone 都 fail closed。运行时先完成 WAL recovery barrier，
再安装 loader 并变为 ready。

旧 schema 不在 load 过程中直接覆写 Mongo。migration 生成 system transaction，提交
Put 并等待 projection ticket 完成后，才发布迁移后的 Entity。Remote-managed 文档还需
lease/version vector；无法取得 lease 时返回明确错误，不绕开 fencing。

### 2.4 Saga 边界

Nest 入口使用 `saga.EmitStart`，启动 effect 与 Entity mutation 共用 CommitRecord。
Native Entity step 使用 Data Engine inbox：handler 内先 `BindCommand`，完成业务修改后
写 completion payload 并 `EmitCompletion`。Entity mutation、`saga-step/CommandID`
receipt 和 `saga-completion:{CommandID}` effect 原子提交；handler 返回后不能直接 publish。

Raw Mongo step 继续使用 `MongoCommandInbox`/`SubscribeMongoStep`。Saga coordinator 的
state、lease、version、outbox 和 completion receipt 仍由其 Mongo Store 管理，不迁入
Entity WAL。两种 inbox 不能混用同一个业务事务边界。

### 2.5 Remote Entity

Remote commit 使用与普通 mutation 相同的 Nest 事务变更源，但继续保留 ownership
lease、marker epoch、lock fence、route epoch 和 aggregate version。生成代码在 Entity
锁内冻结完整 Remote document；只有权威远端提交确认后才推进 DAO tracker version。
旧 participant 的 dirty 回退只用于滚动升级兼容，新生成代码不再读取持久化 dirty。

## 3. 配置与装配

两个引擎严格互斥：

```yaml
persistence:
  engine: dataengine # 或 checkpoint；不能同时启用

dataengine:
  database: game
  wal:
    dir: data/wal/dataengine/1001
    writer_version: 1 # reader-first 阶段；完成读端升级后改为 2
    max_disk_bytes: 8589934592
    max_unacked_age: 5m
  projection:
    batch_records: 256
  outbox:
    workers: 2
    batch_size: 64
    max_pending: 1000000
    max_oldest_age: 30m
```

Data Engine Mod 自己拥有 WAL、projector、outbox 和 repository。不要再额外把 legacy
NestWAL/Checkpoint 作为第二写路径装配；迁移期可以让旧 Mod 留在列表中，但
`ResolvePersistenceEngine` 必须使其 inactive。每个写实例使用独占 WAL 目录/PVC。

WAL reader 永远同时接受 v1/v2。v1 writer 只适合旧 full-record 阶段；Patch、Delete、
receipt 和新 effect header 需要 v2，v1 writer 遇到这些记录会显式拒绝，不会降级丢字段。

## 4. Reader-first / writer-second 发布

1. **盘点和备份**：确认 Mongo 使用 replica set，JetStream 使用 file storage；记录旧
   Checkpoint pending、Redis snapshot WAL pending、legacy NestWAL unacked、Mongo 版本与
   outbox 基线。备份 Mongo 和每个实例 WAL。
2. **先升级 reader**：所有实例发布能读取 WAL v1/v2 的 core/kit，但保持
   `persistence.engine=checkpoint`、writer v1，暂不重新生成 DAO。
3. **排空旧路径**：停止入口流量，调用旧 runtime 的有界 `Flush`/shutdown；等待
   checkpoint pending、Redis WAL pending、legacy WAL unacked 全部为 0。不要用 Redis
   key 删除代替 drain。
4. **校验切换**：把上述观测填入 `dataengine.ValidateCutover("checkpoint",
   "dataengine", state)`；任一 pending 非 0 都拒绝切换。
5. **升级 writer**：在所有 reader 已兼容后，将目标池设为
   `dataengine.wal.writer_version=2`。同一 WAL 目录同一时刻只能有一个 writer。
6. **原子替换写池**：发布由当前 codegen 重新生成的 DAO/Entity，并将
   `persistence.engine=dataengine`。不能让同一分片同时运行 checkpoint writer 和
   dataengine writer，也不能让 v1 writer 接收 patch-only 生成代码。
7. **恢复流量前检查**：等待 startup recovery barrier 完成，确认 `dataengine` health
   为 OK，抽查 load/migration、Put/Patch/Delete、Saga native/raw Mongo 与 Remote 场景。
8. **观察期**：至少覆盖一次 WAL rotation、一次实例重启、预期最大 backlog 恢复和一轮
   Mongo/NATS 故障演练。观察期结束前不删除 legacy Checkpoint 代码。

跨仓库发布时先发布包含 Data Engine package 的 core/kit，再发布依赖它的 codegen。
在首个 core/kit tag 可解析前，codegen 的默认 project preset 有意保持 legacy；源码联调
可显式选择 `dataengine`，但不能把尚不可由 `GOWORK=off` 解析的生成项目作为发布产物。

## 5. 回滚边界

切换前可以回滚二进制。Data Engine 开始接受业务记录后，回滚必须同时满足：

- Data Engine WAL unacked 为 0；
- projector healthy；
- effect 已可靠 staged 到 Mongo outbox；
- 旧 Checkpoint 代码和业务 DAO 仍支持恢复这些文档形态。

用 `ValidateCutover("dataengine", "checkpoint", state)` 执行检查。一旦发布 patch-only
生成 DAO，Checkpoint 已无法重建其旧 dirty/snapshot 协议，`CheckpointRollbackSupported`
必须为 false；此后只允许向前修复或从一致备份恢复，不能配置回切制造双写。

## 6. 启停、健康与故障处理

启动顺序固定为：Mongo indexes/collections → JetStream stream → WAL open → projector
recovery/Flush → outbox worker → repository loader → ready。恢复完成前不得接收 Entity 流量。

停机先停止服务入口，再把 runtime 置为 not-ready、卸载 loader、Flush/Shutdown projector，
最后停止 outbox claim。已经 staged 的 effect 可在下次启动继续发布。

`dataengine` health message 至少包含：

- `wal_unacked`、`wal_oldest`、WAL disk bytes/segment；
- `projection_failures`、`fatal_projection_conflicts`；
- `outbox_pending`、`outbox_oldest`、`publish_failures`。

建议告警：WAL 磁盘 70% warning/85% critical，oldest unacked 超过两个正常恢复窗口，
outbox pending/oldest 接近配置硬上限，任何 fatal projection conflict，Mongo collection
容量或 JetStream stream bytes 超过 70%。`max_disk_bytes`、`max_unacked_age`、
`max_pending`、`max_oldest_age` 是 fail-closed 硬限制，不应作为日常告警阈值本身。

NATS 故障时 projection 应继续，outbox 增长；恢复后以 EffectID 去重清空。Mongo CAS
conflict、WAL fsync 不确定或硬 backlog 超限会 fence writer，需要先查明权威结果再重启，
不能盲目重试覆盖。

## 7. 性能与发布门禁

可重复的本地矩阵位于 `roost-kit/scripts/perf/dataengine.sh`，覆盖 WAL v2 record 编码、
async/strict/pipelined admission（1/8/32 writers）、Mongo adapter shape、Saga reservation
以及 NATS outage 后 backlog 恢复。2026-09-01 在 Apple M5、macOS 26.5.2、Go 1.27.0
的短样本中，1-field patch
record 为 147 bytes，1 KiB full record 为 1152 bytes，满足“至少缩小 50%”的编码门禁；
这些数字只说明编码方向正确，不是生产吞吐结论。

正式发布必须在与生产相同的 Mongo replica set、JetStream file storage、磁盘和网络上
保存 baseline/current 各五次结果，并验证：

- pipelined Entity lock-hold P99 回退不超过 10%；
- async aggregate throughput 回退不超过 10%；
- NATS 故障期间 projection throughput 不低于无 effect workload 的 80%；
- 100k backlog 恢复无 version gap、重复 receipt 或 Saga 重复推进；
- Mongo primary、NATS leader、writer 进程故障后均能从 durable prefix 恢复。

没有上述真实拓扑报告时，只能宣布“实现与本地门禁通过”，不能宣布生产性能达标。

## 8. Legacy Checkpoint 删除条件

物理删除 Checkpoint write runtime 是发布后的独立变更，不属于首次切换部署。必须同时满足：

- 所有服务完成 Data Engine 切换并经过 patch-only 观察期；
- 没有 checkpoint/Redis WAL/legacy WAL backlog；
- 不再需要向 Checkpoint 回滚；
- 真实拓扑性能、故障和 backlog 门禁通过；
- repository guard 确认生产代码不再生成 `Snapshot`/`RemoveSnapshot`、SnapshotWAL、
  Journal、release persistence hook 或 legacy Mutation composite fields。

满足后删除 SnapshotWAL、Journal、Flusher 和 release hook，只保留一版不形成写路径的
load/type compatibility alias。当前在达到这些外部条件前保留 legacy 包是有意的迁移门禁，
不是允许双写。
