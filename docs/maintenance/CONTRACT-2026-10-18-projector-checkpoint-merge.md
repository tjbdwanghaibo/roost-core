# 契约变更：投影器对特殊记录合并 checkpoint（2026-10-18）

维护者 2026-10-18 在 [Saga 原生步骤性能优化](../performance/SAGA-OPT-20261018.md) §7 的三个第三档候选里选 **A**：带回执 / effect 的“特殊记录”投影后不再每条立即 checkpoint，改为按 `CheckpointInterval`（及条数阈值 `CheckpointRecords`）与普通批记录一起合并。**B（投影合批）与 C（WAL 与 Mongo 分盘）不做。** 基线 origin/main `e90dd4ad`。

范围只有 `framework/dataengine/engine` 的投影器确认时机。WAL 格式、checkpoint 文件格式、Mongo 文档格式、公开 API 与配置键都不变；`BatchProjectionStore` 的文档契约扩大一句（§5）。属于 DataEngine（三大块之一）的持久契约，先论证后实施。

## 1. 现状与问题

`ReplayPass`（`framework/dataengine/engine/projector_replay.go`）是单 goroutine 投影循环。修改前，“特殊记录”（`isLocalBatchRecord` 为 false：带 effect、回执——含 lease fence 回执——或 Remote 写，`projection_plan.go`）有两处强制确认：

- 投影**前**：`special` 时先 `checkpoint()`，把前面已投影的普通前缀确认掉；
- 投影**后**：`if special || !replaySafe || …` 立即 `checkpoint()`。

每次 checkpoint 是 `WAL.Ack`：刷活动段（strict 下多为空操作）、写临时文件 `Sync`、rename、目录 `Sync`（`framework/nestwal/checkpoint.go` `storeCheckpoint`），darwin 上 `File.Sync` 是 F_FULLFSYNC。saga 原生步骤每条记录都是特殊记录，负载下一次投影事务约 16 ms、一次 checkpoint 44～55 ms，单进程约 15 条/s，是当前第一瓶颈（SAGA-OPT §5、§10.2，KNOWN-LIMITS“Saga 原生步骤容量”）。

## 2. 当初为什么特殊记录要立即 checkpoint

查证范围：`git log -S'special := !isLocalBatchRecord'`、`git log -S'replaySafe'`、`git log -S'CheckpointInterval' --all`、`git log --follow` 与 `-L` 追溯 `projector_replay.go` 这两段；`docs/framework/impl/03-dataengine.md`、`guide/03-dataengine.md`、KNOWN-LIMITS、`docs/release/`、本机 `roost-bugfix/reference/lessons.md`。结论：

- 两段都来自 `646ce63f`（2026-09-25 “完成 Nest、Sync、DataEngine 和 Remote 优化”）。在它之前，`ReplayPass`（当时在 `dataengine/engine/projector.go`）**每个投影单元都立即 ack**，没有合并。
- `646ce63f` 引入 `CheckpointRecords` / `CheckpointInterval`，只对它当时能论证的单元放开合并：批次段与多 mutation 单条记录（`ProjectBatch` / 事务路径写持久事务标记）。注释只写了两条不合并的理由：**单 mutation 快路只保存文档末次事务 ID**（后续版本覆盖后旧记录无法判定幂等）、**未知 Store 保留旧契约**。特殊记录前后各确认一次是把旧的“逐单元确认”原样保留，代码、提交信息与文档里**没有**特殊记录专属的理由；review 记录（`381efc92`）提到 `CheckpointInterval` 只为 Windows 时钟粒度。
- 所以“特殊记录立即确认”是保守沿用，不是某个已登记不变量的强制点。下面逐条列出它可能在保护的东西，并论证合并后是否仍成立。

## 3. 不变量清单与论证

合并后的新状态只有一种：**若干条特殊记录已在 Mongo 提交、WAL checkpoint 仍停在它们之前**，随后进程崩溃（或 ack 失败）时从上一个 checkpoint 重放，被重放的特殊记录之后可能已经有**同文档 / 同实体 / 同操作文档的更新版本**投影过。修改前这个窗口最多一条特殊记录，且其后没有已投影的记录（ack 失败时投影器立即停下）。

| # | 不变量 | 修改后是否成立 | 依据（当前源码） |
| --- | --- | --- | --- |
| I1 | 重放同一特殊记录不重复应用业务 mutation | 成立 | `MongoStore.ProjectFenced`（`mongo_projection.go`）：非快路记录在 Mongo 事务里**先** `checkTransaction` 查 `_dataengine_transactions` 标记（`_id` = 事务 ID，digest 必须一致），命中即返回首次的结论，不执行任何写。标记与业务写、回执、outbox、Remote 提交在**同一** Mongo 事务插入，要么一起可见要么都不可见。判定只看标记，不看文档版本，所以后续版本覆盖同一文档不影响。批次段早已依赖同一机制合并确认（`TestRealLocalMultiBatchOrderIdentityAndLostCheckpoint`：当前版本 4 时重放版本 1..4）。 |
| I2 | 重放时不重新判定 lease fence，已生效的步骤不会被误判“被接替”，被跳过的步骤也不会在重放时生效 | 成立 | `checkTransaction` 在 `leaseFencesMatch` **之前**；跳过时写的是 `Skipped: true` 的标记（同一事务），重放返回 `skipped` 原值。首次投影之后协调器 / 消费者对操作状态文档的改动（`markCompleted` 置 completed、超时后新尝试 token+1、接管）都不会被重放读到。 |
| I3 | 回执恰好一份 | 成立 | 由 I1 的标记挡住；即使到达 `stageReceipt` 也是事务内 FindOne 先查、digest / payload 不同才报 `ErrReceiptIdentity`。saga 读者 `DataEngineStepInbox.readReceipt` 直接读 Mongo 回执，不读 checkpoint。 |
| I4 | effect 不重复暂存（outbox 已发布并删除的 effect 不会被重放插回） | 成立 | 由 I1 的标记挡住（重放不进入 `stageEffect`）；`stageEffect` 本身也是 FindOne 先查。outbox 已发布未删除的 effect 由 OutboxWorker 按 effect ID 至少一次发布，下游按 ID 去重——与 checkpoint 无关，未改。 |
| I5 | Remote 写的特殊记录重放不冲突、不倒退内存版本 | 成立 | 事务部分由 I1 挡住；之后 `ApplyRemoteCommits` 仍会再调一次：`MongoCommitter.CommitRemoteBatch` 先按事务 ID 读 `_remote_tx`（与 DataEngine 事务同提交），找到即返回原回执，不做版本 CAS；`CommitStatus` 为 Committed 时 `confirmRemoteCommit` → `acknowledgeRemoteCommit` 对常驻实体先判 `remoteReceiptObsolete`（更新版本已确认则无操作），`notifyRemoteVersion` 只取最大值。崩溃重启的重放发生在启动恢复、实体可加载之前，没有常驻实体。并行 Remote 窗口（`projectRemoteWindow`）早已对整窗合并确认，本次不改它。 |
| I6 | 没有读者依赖“checkpoint 已推进” | 成立 | `WAL.Ack` 只更新 checkpoint 槽（决定下次 `Replay` 起点）与清理已确认段（`nestwal/wal.go`）。投影完成的通知（`completeProjection`、票据、实体投影等待、屏障解除）在**投影成功时**发出，不等 ack；saga `waitReplay` 轮询 Mongo 回执；协调器 Complete 由 outbox 发出的 completion effect 驱动。`Flush` 循环到一轮没有记录为止，每轮 `ReplayPass` 退出前的 defer 都确认已成功前缀，所以 `Flush` / `Shutdown` 返回时 checkpoint 已到末尾。 |
| I7 | 被跳过的原生步骤只驱逐一次 | 成立 | `startStaleEviction`（`fenced_step.go`）只对本进程这次准入、仍在 `pendingTransactions` 里的记录登记驱逐；重放（启动恢复或 ack 失败后）时记录已不在其中，返回 false，注释已写明“ack 丢失后的重放”。 |
| I8 | 快路记录仍立即确认 | 成立 | `replaySafe` 改为 `batchCapable && (len(unit.records) > 1 \|\| !projectsOnFastPath(unit.records[0]))`；`projectsOnFastPath` = 单个本地 mutation、无 effect / 回执 / Remote，与 `ProjectFenced` 选择快路用同一个函数，两处不会分叉。快路记录的确认是累积的，顺带覆盖它前面尚未确认的特殊记录。 |
| I9 | checkpoint 只推进连续成功前缀；ack 失败后不再投影后续段 | 成立（未改） | `checkpoint()` 只确认 `fences[processed-1]`；任何 `checkpoint()` 失败都 `ackFailed` 并返回；投影失败返回前 defer 只确认已成功前缀。删掉的“投影前确认”只是提前做了一次同样的累积确认。 |
| I10 | 背压与关闭不变 | 成立 | `admitted` / `walUnacked` 在 ack 后才减（`acknowledge`），合并确认使已投影记录多占额度至多一个间隔（20 ms）或 `CheckpointRecords`（256）条，`MaxUnackedRecords` 语义不变。`Close` 等 `ReplayPass` 退出（含 defer 确认）后才关 WAL；`WAL.Ack` 不看 context，取消不会丢掉最后一次确认。 |
| I11 | 自定义 / 非批量 Store 不受影响 | 成立 | 只有实现 `BatchProjectionStore` 的 Store 才合并；它的文档契约扩大为“后续版本写入后仍能重放已成功的批次、多 mutation 事务与带 effect / 回执 / Remote 写的记录”。仓内唯一实现是 `MongoStore`（满足 I1～I5）；只实现 `ProjectionStore` 的 Store 照旧逐单元确认。 |
| I12 | 事务标记在仍可能重放时不过期 | 成立（未改） | 标记 TTL（缺省 30 天）由 `ValidateReceiptRetention` 要求大于 WAL 未确认年龄告警窗口；合并只把未确认窗口延长到毫秒级。 |
| I13 | 执行契约：不跳过冲突 WAL、不放宽版本比较、不伪造持久确认 | 成立 | 版本 CAS、冲突 fatal、身份校验路径都没有改；确认只会**更晚**，不会更早。 |

论证结论：没有不成立的点，不需要维护者另做决定。

## 4. 崩溃语义

- **重放上界**：一轮 `ReplayPass` 内，自上次确认起累计 `CheckpointRecords`（缺省 256）条，或距上次确认完成超过 `CheckpointInterval`（缺省 20 ms）后的第一个单元边界，就确认一次；一轮结束也确认。所以崩溃后会重放的已投影记录 ≤ 256 条，且只来自崩溃前约 20 ms 加一个单元耗时的窗口。saga 负载下一条特殊记录的投影事务约 16 ms，**合并系数约 2**（每两条确认一次），重放 ≤ 2～3 条。
- **重放代价**：每条已投影的特殊记录重放是一次 Mongo 事务里的一次标记读（命中即返回）；Remote 记录另加一次 `_remote_tx` 读与 `CommitStatus`。启动恢复在 Runtime 就绪前完成，最多多几十毫秒。
- **与 saga 的交互**：步骤消费者的 `waitReplay` / `markCompleted` 只看 Mongo 回执，与 checkpoint 无关；重放不改回执、不改操作状态文档（不重判 fence）。协调器 Complete 由 completion effect 驱动，重放不重复暂存 effect；崩溃时尚未发布的 effect 仍在 outbox，由重启后的 OutboxWorker 发布（未改）。所以崩溃后每个操作实例的业务值、回执、effect、终态都恰好一次。

## 5. 改动

- `projector_replay.go`：删掉特殊记录投影前的 `checkpoint()`；投影后的条件去掉 `special ||`，`replaySafe` 用 `projectsOnFastPath`。并行 Remote 窗口前后的确认不变。
- `projection_plan.go`：新增 `projectsOnFastPath`（唯一判定）。
- `mongo_projection.go`：`ProjectFenced` 的快路条件改用 `projectsOnFastPath`（逻辑等价：`isLocalBatchRecord` 要求有 mutation、无 effect / 回执 / Remote）。
- `projector.go`：`BatchProjectionStore` 文档契约补“带 effect / 回执 / Remote 写的记录”。
- 测试：`batch_limits_test.go` 新增特殊记录合并阈值、快路累积确认、checkpoint 丢失重放三条；`TestMultiBatchCapabilityAndSpecialBoundaries` 的期望改为合并后的序列；`TestProjectorStopsAfterSegmentAckFailure` 原靠“投影前确认”触发失败，改为用条数阈值在批次段后触发，承诺不变。真实 Mongo 崩溃注入见 §6。

不做：调整 `CheckpointInterval` 缺省值（合并系数受它限制，见 §7）；并行 Remote 窗口的确认；B / C。

## 6. 验证

- 先红后绿（单测）：新测试在 `e90dd4ad` 上必须失败，修后通过。结果、修前失败原文与性能对照见 [前后对照报告](../performance/SAGA-CHECKPOINT-20261018.md)。
- 真实 WAL 基准 `BenchmarkProjectorWALReplayAckMatrix` 修前 / 修后交替各 6 轮（checkpoint 次数与耗时）。
- 崩溃注入（真实 Mongo）：引擎级（真实 `DataEngineStepInbox` 构造的原生步骤记录：lease fence + saga 回执 + completion effect；首投后改操作状态文档使 fence 现在会失败 / 会通过、outbox 已发布删除、同文档后续版本已投影；丢 checkpoint 后重开 WAL、新投影器 Flush）与 saga 级（真实 Nest + DataEngine + saga，checkpoint 一律不落盘，跑完正向 / 业务拒绝补偿 / 投影停顿被跳过后停进程，用同一 WAL 目录与库重启）。
- 夹具 B 同机前后对照：持续档正向 5/s、全补偿 3/s 各 2 轮，上限档正向 7/8/10/s、全补偿 4/5/10/s。
- gofmt、`GOWORK=off go vet ./...`、相关包 `-race`、dataengine / saga integration、`scripts/test-dataengine-generated.sh`、`scripts/test-remote-generated.sh`、根包、全量 `go test ./...`。

## 7. 风险、边界与回退

- **合并系数受 `CheckpointInterval` 限制**：缺省 20 ms 是为批次段定的；特殊记录逐条投影约 16 ms，时间阈值在第二条后就触发。要更高的系数可调 `dataengine.projection.checkpoint_interval`（已有配置键，代价是崩溃后重放窗口与未确认额度占用相应变长）；是否改缺省值属于产品选择，本次不改。实测积压时每条特殊记录 0.56～0.75 次确认（[报告](../performance/SAGA-CHECKPOINT-20261018.md) §3.2）。
- 积压时一轮读到多条记录才会合并；低负载下一轮常常只有一条记录，轮末照常确认，延迟与修改前相同。
- 回退：还原实现提交即可；不涉及持久格式，新旧进程可互换 WAL 与 Mongo 数据。
