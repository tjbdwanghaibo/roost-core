# 2026-09-26 核心优化复审运行记录（Nest / Sync / DataEngine+Remote）

基线 main `a22c5a5`（对照 `8ce21a5`）。范围：交接文档 §3 “最新”各批（N1～N3、S1～S3、D1～D3）及其前一批的正式记录与当前源码。
方法：三路只读审查，对照 roost-coding 执行契约；codebase-memory 图谱 generation `2026-09-25T11:41:37Z` 早于基线，结论全部来自源码、
`git show` 与临时探针（已删除）。没有重跑长压测、故障矩阵或 Mongo/Redis/NATS 集成环境。

> 后续状态：RR-03～06 和已确认的观测/准入问题已修复、验证，详见[核实与修复](REVIEW-2026-09-26-followup.md)。下文保留原始只读复审证据与当时状态。

## 登记为 RR（复审时未修复）

| 编号 | 等级 | 结论 |
| --- | --- | --- |
| [RR-20260926-03](../bug/RR-20260926-03.md) | P2 | Slow 快阶段继承慢阶段 local executor，RunLocal 在快池内自等，快池饥饿死锁、停机排不空 |
| [RR-20260926-04](../bug/RR-20260926-04.md) | P2 | 字节软预算下大对象只能作窗口首个准入，游标停在最后准入者，被持续插队而饿死 |
| [RR-20260926-05](../bug/RR-20260926-05.md) | P2 | 快照额度在 Push 前对全部会话预扣，RetryLater / 取消后窗口额度被未尝试会话占满（a22c5a5 回退） |
| [RR-20260926-06](../bug/RR-20260926-06.md) | P2 | 快池内框架阻塞等待入口没有 fail-fast，维护者要求直接 panic |

复现附录：[REPRO-2026-09-26](../bug/REPRO-2026-09-26.md)。同日 roost-coding “Nest 与 Entity”契约补充“快池内不得阻塞等待”。

## 疑点（未登记 RR，供后续判断）

- **Remote 并行窗口失败错误不带事务 ID**：`dataengine/engine/projector_remote.go:110-111` 只报 `remote window stopped after %d/%d records`，
  串行路径（`projector_replay.go:160,185`）带事务 ID。诊断退化，a22c5a5 引入。
- **`Projected` 在失败后缀重放时重复计数**：`projector_remote.go:84-87` 与 `projector_replay.go:165`。出现重试后 `committed == projected`
  不再是一致性证据；窗口上限扩到 `ReplayBatchRecords` 后更明显。
- **nestwal `Stats.Replayed` 漏计批次截止时保留的最后一条**：`projector_replay.go:45-46` 返回 `errProjectorBatchComplete`，
  `nestwal/wal.go:551-555` 只在回调返回 nil 时计数。探针：20 条全部投影、WAL 排空，`Replayed=10`。只影响观测。
- **快池 Broadcast 静默跳过冷目标**：`nest/nest_dispatch.go:429-432` 对 `Get` 错误直接 `continue` 不记日志；N1 后冷目标得到
  `ErrColdLoadInLogic` 即被丢弃（N1 前在快 worker 上加载）。与“动态缺失给出可行动错误”的描述不符。
- **快阶段约束只挂在 `msg.getter`**：handler 直接用 `ManagerAccess` / 生成的 lifecycle 仍可在快 worker 上冷加载（已并入 RR-20260926-03/06 的实施方向）。
- **N2 队列观测口径分叉**：续行占住快 worker 时 `QueueLen` / `PeakWaiting`（`dispatch_queue.go:310`）与 `WaitingForWorker`（`:295-299`）口径不一致；
  `OldestWaiting` / `PeakWaiting` 是前驱等待与 worker 等待合并的单值，文档写“分阶段”。
- **Sync 恢复分类只覆盖在已有会话上 Hold**：`subscriptions.go:97` 仅 `HoldSession` 标记 recovery；旧会话先 Close 再重连的订阅全部算新入场。
  需业务确认“重连恢复”的定义。
- **只配字节预算时 periodic 每轮重复打包全部冷请求**（`snapshot_budget.go:158-164,209`，8ce21a5 起）；会话因编码 / Push 失败关闭时已扣额度不退。
- **空 ID Remote 批次在停机时多归还一个写额度**：正式链路 `prepareRemoteWriteBatch` 遇空 ID 直接返回，走不到，不单列。

## 文档不一致

- `sync/README.md:53-55` 仍写“Stats 会遍历订阅”；S3 后 Stats 为增量计数，完整遍历在 AuditStats（`USER_GUIDE.md` 已正确）。
- 九项报告 D1 验收列写“取消 / 跨段 / 真实 WAL”，但实现与验证进度只列边界、首条超大与后缀；仓库无取消 / 跨段回归，80TPS 长测
  `SegmentFiles=1` 未换段（审查探针确认行为正确）。D2 缺少“观察到失败后停止补位”的严格断言（`projector_remote_test.go:101-103` 放宽）。
- `docs/bugfix/RR-20260925-05.md` 与 `REFACTOR-2026-09-25-remote-throughput.md` 仍描述“按 worker 数分批 / 等待全部 worker”，未标注已被 D2 滑动窗口取代；
  `projector_remote_test.go:139-141` 子用例名 `"workers"` 实际传 maxRecords。
- 窗口字节上限实际是 `ReplayBatchBytes`，文档写作 read_bytes 窗口（默认值相同，默认配置下无差异）。
- `docs/bugfix/RR-20260926-02.md` 的验证证据引用本机 `/tmp/roost-nine-logic-race{,-2}.txt`，违反“不依赖某台机器 /tmp”。
- 交接文档 §3 Nest 表 “N1～N4”（旧 NEST-COMPLETION）与 “最新 N1～N3”（九项）编号重名。
- 九项报告称覆盖“动态 Cast / GetMany 回归”，提交的 `TestFastLogicMarksGetterContextAndSlowPreparationCanLoad` 只测 mock getter；真实 ManagerAccess
  的 CastMulti / Multi 路径由本次探针补证。RR-20260926-02 的错误语义收紧（配置 loader 时不存在的实体返回 `ErrColdLoadInLogic` 而非
  `ErrEntityNotFound`；Multi 可选缺失目标从 nil 占位变为报错）未列为兼容项。

## 核对通过（摘要）

- D1 读取预算在 `replayGate` + WAL `replayMu` 下串行，首条必收保证进度；D2 滑动窗口无 goroutine 泄漏、结果按 index 写回、只 ack 连续成功前缀、
  特殊记录仍为屏障、多 DAO 单事务；Remote 写许可 Prepare→Close 全程持有、各退出路径各归还一次、过载 `errors.Is(ErrRemoteOverloaded)`。
- N1 在 Single / Multi / 动态 Cast / preparedGetter 回退上不可绕过；N2 计数全部在 `q.mu` 下成对回收；N3 metricKey 输出与旧实现逐字一致。
- Sync 待快照请求增量索引随订阅 / 会话 / subject 生命周期正确增删（300 种子 × 2 模式 × 4 预算随机探针 + 并发 race 探针）；Stats 与 AuditStats 口径一致；
  已交付前缀结算、revision / lifetime、remove-before-create、RR-12/13 边界保持；`SnapshotBudget.Interval` 无残留。
- 交接文档、九项报告、证据 JSON 与提交信息的数字一致；证据 JSON 中 211 个源码 SHA256 与 a22c5a5 一致。

执行过的定向测试：`GOWORK=off go test -race ./dataengine/engine -run 'Remote|Replay|ReadBudget|Oversized|Backpressure|Checkpoint'`、
`./kit/dataengine`、`-race ./remoteentity ./kit/remoteentity -run 'Budget|WriteSlot|Overload'`、`-race ./nest -run 'TestQueueStats|TestFastLogic|TestWorkerBudget|TestTwoPools|TestSlow' -count=3`、
`./entity -run TestLoadedOnly -count=3`、`-race ./sync/entitysync -count=1`，均通过。
