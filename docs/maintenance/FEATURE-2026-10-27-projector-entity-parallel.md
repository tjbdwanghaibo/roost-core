# 功能设计：DataEngine 投影器按实体分片并行投影（2026-10-27）

**状态：设计稿（实施记录见 §11）。** 维护者 2026-10-27 决定按方案 A：投影器分片并行单独评审并实施。范围是三大块里的 DataEngine 投影器（`framework/dataengine/engine`），
不改 WAL 格式、不改 Nest 准入、不改 Mongo 文档形状。

读码说明：codebase-memory 图（项目 `Users-whb-roost-roost-core`）索引的是主检出 `ce49b3c3`，早于本分支基线 `8c691215`；
本篇的 `path:line` 全部按本分支源码（基线 `8c691215`）核对。

## 1. 为什么要做

- saga JetStream 权威日志原型第三轮（分支 `proto/saga-jetstream-log`，报告 `docs/performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md` “第三轮” T2/T4）：
  关键路径不碰 Mongo 之后，参与方的 DataEngine 投影器成了独立的持续吞吐上限。云 Mongo（ping P50 7.8ms）上约 500～590 条/s；
  每次 Mongo 调用平均承载约 50 条记录，但每次约 100ms，单通道串行约 10 次/s；c16 起积压持续增长。
- 现行 main 上同样受限：`docs/performance/SAGA-CLOUD-MONGO-20261026.md` §0 第 2 条——原生步骤卡在投影器（逐条处理带回执 / effect 的记录）。
- 现状：`projector_replay.go:18` `ReplayPass` 只对“相邻、互不重叠的纯 Remote 事务”并行（`projector_remote.go:14` `remoteProjectionWindow`，
  `RemoteProjectionWorkers` 缺省 8）；普通实体记录、带回执 / effect 的记录全部串行，一次只有一个 Mongo 调用在途。

上限在“同一时刻只有一个 Mongo 调用在途”，不在每条记录的调用次数（已经合批）。所以要让互不相干的记录同时有多个调用在途。

## 2. 目标与不变的东西

目标：同一轮回放读到的记录里，**冲突键互不相交**的记录分到不同通道并发投影；**有交集的记录保持 WAL 顺序**；checkpoint 仍只确认连续成功前缀。

不变（逐项在 §6 说明怎么保持）：

1. 同一文档的版本沿 WAL 顺序前进（版本 CAS 不变，高版本不是幂等证明）。
2. checkpoint 不超过“已成功的最长连续前缀”；成功后缀崩溃后重放幂等。
3. 投影冲突（`ErrProjectionConflict` / `ErrTransactionIdentity` / `ErrReceiptIdentity` / 退役记录）仍 fail-stop：fatal、`OnFatal` 一次、唤醒全部等待方、不 ack、WAL 原样保留。
4. `Flush`、`ManualReplay`、`Close` / `Shutdown` 的等待与排空、原生步骤屏障（`fencedEntities`）、运维卸载屏障（`unloadingEntities`）、`MaxUnackedRecords` 准入、
   held 记录（仍在实体锁内的事务）不投影，全部不变。
5. effect 进 outbox 的暂存顺序：缺省（本设计实施的版本）**不变**——带 effect 的记录之间仍按 WAL 顺序暂存（§5）。是否放宽交维护者决定（§9 第 1 项）。

## 3. 冲突键

每条记录算出一组冲突键（`projectionConflictKeys`）。两条记录有任一相同的键就互相“冲突”，必须按 WAL 顺序投影；冲突是传递的
（A 与 B 冲突、B 与 C 冲突，则 A、B、C 一起按顺序）。

| 记录内容 | 冲突键 | 原因 |
| --- | --- | --- |
| 每个 mutation（Put / Patch / Delete，本地或 Remote） | `entity:<Key.ID>` | 文档身份是 (Database, Scope, Resource, ID)，`_id` 就是 `Key.ID`；按 ID 取键比按文档粗（同 ID 不同集合的文档也被串起来），只会多串行、不会漏。DAO 共用完整 Entity ID，多 DAO 实体、多实体 Multi 事务自然把涉及的实体全部串起来。删除同样是该文档的一个版本。 |
| Remote mutation | 同上（`ValidateMutation` 保证 `Remote.EntityID == Key.ID`，`RemoteCommit.Validate` 保证其 DAO、删除、快照都属于该实体） | 与现行 `remoteProjectionWindow` 相同 |
| Remote mutation，且 Store 没有声明 Remote 适配器并发安全（`SupportsRemoteParallelProjection` 为假） | 另加 `class:remote` | 所有 Remote 记录之间串行，等于现行“无能力时 Remote 串行” |
| 回执（含 saga 回执） | `receipt:<Namespace>/<ID>` | 回执身份唯一；同一身份的两条记录谁先暂存决定谁报 `ErrReceiptIdentity`，必须保持顺序 |
| lease fence 回执（原生步骤） | `receipt:<保留命名空间>/<db/resource/docID>` | fence 回执的 ID 就是协调文档身份，投影时读它并写 `updated_at`；同一协调文档的记录按顺序判定 |
| effect | `effect:<ID>`，另加 `class:effects`（缺省，§5） | effect ID 是 outbox 身份；`class:effects` 让带 effect 的记录之间保持 WAL 顺序 |
| 事务 ID | `tx:<ID>` | WAL 里同一事务 ID 出现两次（不该发生）时按顺序处理，由 Store 的事务标记判定 |
| 无法算键：任一 mutation `Key.ID == 0`；Remote 写与 lease fence 同在的退役记录 | 屏障（不进并行窗口） | 交给串行路径，由它报现有错误；屏障前后的窗口各自独立 |

推广而不是另起：现行“纯 Remote、互不共享 Entity、无 effect / 回执”的窗口是上表的一个特例（只有 `entity:` 与 `tx:` 键、遇到特殊记录截止）。本设计用同一套键覆盖普通记录、
特殊记录与 Remote 记录，删除 `remoteProjectionWindow` / `projectRemoteWindow`，Remote 并行走同一个调度器。

## 4. 调度：窗口、通道与批

1. **窗口**：`ReplayPass` 读到的一批记录（`readReplayBatch`，在第一条 held 记录前停，受 `ReplayBatchRecords` / `ReplayReadBytes` 限制）按屏障切成若干段；
   每段在投影前先确认之前的成功前缀。
2. **连通分量**：段内按冲突键做并查集，得到互不冲突的分量。分量数 < 2（整段互相冲突）时走现行串行路径，行为与改动前逐字节一致（包括单 mutation 快路与立即确认）。
3. **通道**：通道数 `min(ProjectionWorkers, 分量数)`。分量按首条记录的 WAL 位置依次分给当前记录数最少的通道（整分量不拆）；每个通道内按 WAL 顺序排列。
   同一分量的记录只在一个通道里，所以有交集的记录保持 WAL 顺序；一个通道可以有多个分量，它们的记录交错排列、可以进同一个批。
4. **通道内的批**：通道内用现行 `planProjectionSegments` 切段——连续普通本地记录合成一个 `ProjectBatch`（受 `ReplayBatchRecords` / `ReplayBatchBytes` 限制），
   特殊记录（effect / 回执 / Remote）单条 `ProjectFenced`。每通道批大小因此最多是本窗口分到它的普通记录数，上限同现行。
5. **单 mutation 快路**：快路不写事务标记，幂等只靠文档上的 `_last_tx`（`projection_plan.go:37`）。窗口内若同一文档后面还有记录，前一条走了快路、后一条也成功、
   而更早的别的记录失败，重放前一条时文档已是更高版本 → 误报冲突 → fatal。所以并行窗口内**只有本窗口里它的所有键的最后一条、且独占一个批的单 mutation 记录才走快路**，
   其余单 mutation 记录一律进 `ProjectBatch`（写事务标记）。批退回逐条（`ErrProjectionBatchNeedsPerRecord`，只在重放 / 冲突时出现）时，逐条路径若让一条“不是本窗口最后一条”
   的快路记录成功，该通道在它之后停止（后面的记录本轮不投影，下一轮从确认前缀重读），其他通道照常。
6. **失败**：任一通道某单元失败即置“停止补位”：所有通道在下一单元前停下，已发出的单元等它返回。每个失败单独交给 `isFatalProjection`（不用合并后的错误判断，
   避免一个 `ErrProjectionBatchNeedsPerRecord` 包装掩盖另一通道的冲突）；返回的错误带每笔失败事务 ID，`errors.Join` 保留 `errors.Is`。
7. **完成通知**：每个单元成功后，在通道 goroutine 里做与串行路径相同的收尾：被跳过的原生步骤交驱逐（`startStaleEviction`）、否则 `completeProjection`（票据 / 实体等待方）、
   `projected` 计数、有 effect 时 `OnEffectsStaged`（现在可能被多个通道并发调用，接的是非阻塞的 `OutboxWorker.Kick`）。

## 5. effect 进 outbox 的顺序

现状：串行投影下 effect 按 WAL 顺序暂存（`available_at` 取暂存时刻，outbox 按 `available_at, _id` 认领，`outbox_store.go:75`）。但发布顺序现在就不是全局有序：
`dataengine.outbox.workers` 缺省 2 个认领循环并发发布（`wiring/dataengine/mod.go:154`），发布失败 `Nack` 后继续发同批后面的（`outbox_worker.go:141-149`）；文档也没有承诺 effect 全局顺序
（`NEST_TRANSACTION_WAL.md` §6 只承诺至少一次与 `Effect.ID` 去重）。

本设计缺省（实施版本）：`class:effects` 把带 effect 的记录全部连成一个分量，**暂存顺序与现在完全相同**；不带 effect 的记录与它们并行（除非共享实体等键）。
代价：原生步骤记录（回执 + lease fence + completion effect）全部带 effect，彼此仍串行——现行 main 的原生步骤吞吐不会因本设计提高（§8 实测）。

放宽（选项 B，**需要维护者决定**，本分支不实施）：去掉 `class:effects`，只保留 `effect:<ID>` 与实体等键。效果：同一实体（及共享回执 / 协调文档的记录）的 effect 仍按 WAL 顺序暂存；
不同实体的 effect 在同一窗口内（≤ `ReplayBatchRecords` 条）可能以不同于 WAL 的顺序暂存。§8 给出 B 的实验数据（未提交的实验补丁，只用于测量）。

## 6. ack / checkpoint 与崩溃重放

- 窗口结束后算前缀：WAL 顺序上第一条“未成功”（失败、未开始、被通道停止）之前的全部记录；`checkpoint` 只 ack 到前缀末尾（`fences[prefix-1]`），
  `acknowledge` 只释放前缀的未确认额度。前缀之后成功的记录不 ack，`WALUnacked` 不减。
- 前缀之后已成功的记录在下一轮（或崩溃后的新进程）重放，幂等靠持久身份：
  - `ProjectBatch`：事务里先按事务 ID 查标记（`mongo_projection.go:216`），已应用的整条跳过，与文档当前版本无关；
  - 多 mutation / 带 effect / 回执 / Remote 的记录：`ProjectFenced` 事务里先查标记（`mongo_projection.go:84`），返回首投结论（applied / skipped），不重判 lease fence、不重复暂存；
  - 单 mutation 快路：只在 §4 第 5 点的条件下使用——本窗口里它之后没有同键记录，下一轮又从确认前缀重读，它的重放一定早于任何后续同文档写，`_last_tx` 判定成立。
- Store 能力：新接口 `ParallelProjectionStore`（`BatchProjectionStore` + `SupportsParallelProjection() bool`）承诺上述三点与“冲突键不相交的记录可并发调用”。
  `MongoStore` 实现；只实现 `ProjectionStore` / `BatchProjectionStore` 的 Store 不并行。Remote 记录另需 `SupportsRemoteParallelProjection()`（现行能力，含义不变），否则加 `class:remote`。

## 7. 配置

- 一个正式配置项 `dataengine.projection.workers`（`ProjectorOptions.ProjectionWorkers`），范围 1～64，0 / 不写取缺省 8；**1 = 现行串行**（整轮走串行路径，连 Remote 也串行）。
- **与 `RemoteProjectionWorkers` 合并**（删除 `dataengine.projection.remote_workers` 与 `RemoteProjectionWorkers`）：Remote 并行是本调度器的特例，两个旋钮会出现“实体 8、Remote 2 到底几个 Mongo 调用在途”的歧义。
  缺省沿用 Remote 的 8，Remote 行为不变；未上线不兼容旧键：`remote_workers` 不再被读取（`app.LoadConfig` 不拒绝未声明的键，写了也只是被忽略），CHANGELOG 写明改名。
- 缺省 8 的理由：云 Mongo 上单调用约 100ms（含多数派写与提交）、本机约 2～5ms，8 个在途调用在云上把上限提高到约 8 倍量级；再多受 Mongo 连接池（驱动缺省 100）与通道失衡限制，
  §8 的 1/2/4/8/16 档实测确认取值。生成器的配置快照 `codegen/internal/roost/kitconfig_gen.go` 经 `go generate ./...` 刷新。

## 8. 测量（结果见 [PROJECTOR-ENTITY-PARALLEL-20261027](../performance/PROJECTOR-ENTITY-PARALLEL-20261027.md)）

- 本机隔离 Mongo：投影器吞吐（条/s）`workers` 1 / 2 / 4 / 8 / 16，形状：普通单 mutation（多实体）、普通 + 热点实体、原生步骤形状（回执 + effect）；现行 saga 原生步骤 scan c16 / c64 前后对照。
- 云 Mongo：同样的投影器吞吐对照；原型 `cloud-part` 组在本分支代码上 c16 / c64 / c128，看积压是否不再持续增长。

## 9. 需要维护者决定

1. **effect 暂存顺序是否放宽为“按实体有序”（选项 B）**。推荐：先保持 A（本分支），看 §8 的 B 实验数据再定；B 的收益只在“带 effect 的记录多、且分布在很多实体上”的负载（原生步骤），
   代价是 outbox 暂存顺序不再等于 WAL 顺序（发布顺序现在本来就不保证）。

## 10. 考虑过没采用的方案

| 方案 | 不采用的原因 |
| --- | --- |
| 按记录的动态依赖图 + worker 池（每条记录在前驱完成后就绪，像现行 Remote 窗口逐条补位） | 失去跨分量合批：参与方负载里每个实体每轮只有 1～2 条，按分量投影就是逐条事务，调用次数涨几十倍；静态通道 + 通道内合批兼顾两者 |
| 按实体 ID 哈希固定 N 条常驻通道、各自独立的 WAL 游标与 checkpoint | 需要多游标 WAL 与多份 checkpoint，Multi 事务跨通道要额外同步；崩溃恢复从“一个连续前缀”变成 N 个，改动面大 |
| 只加大批 / 拉长 checkpoint 间隔 | 不改变“一次只有一个调用在途”；云上单调用 100ms 的上限不动 |
| 同一实体多次改动只写最新版本（合并写） | 破坏版本链与 `_last_tx` 幂等判定、effect / 回执逐笔暂存，改的是持久语义 |
| 参与方按 sid 拆进程、每进程一个投影器 | 部署层面的办法，与本设计正交，不改单进程上限 |
| 并行窗口内全部单 mutation 记录都走快路、只在失败时退回 | 快路无标记，§4 第 5 点的重放误报冲突无法事后识别，只能事前排除 |
| effect 记录全局串行之外再做“同 Topic 有序” | 现有文档没有这种承诺，发布侧（2 个认领循环、Nack 继续）本来也不保证；不在本设计里新造语义 |

## 11. 实施与验证记录

（实施后补：提交、红绿证据、变异核对、验证命令。）
