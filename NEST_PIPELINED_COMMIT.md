# Nest Pipelined Commit（DurabilityPipelined）设计与实施

## 1. 目标

`DurabilityStrict` 把 WAL fsync 放在实体锁内：commit point 之前锁不释放，"成功不可先于 commit point 被观察"由锁的持有直接保证。代价是热点实体的锁持有时长包含毫秒级 fsync，所有等待者被 I/O 拖住。

`DurabilityPipelined` 把"锁持有时长"与"fsync 时长"解耦：

- 锁内只做到 **日志已入队（拿到 LSN）**；
- fsync 在锁外由 group-commit 完成；
- 一切离开进程的东西（回包、AfterCommit 副作用、entitysync 分发、checkpoint 落库）gate 在
  `durableLSN >= tx.LSN` 上——这就是**外化闸门**。

四条运行时不变量全部保持，其中第 1 条（成功不可先于 commit point 被观察）的实现方式从"锁不放"改为"外化闸门"。

## 2. 正确性论证

1. **append 在实体锁内** ⇒ 同一实体上 LSN 顺序 = 变更顺序。
2. **WAL 单日志、按 LSN 顺序 fsync** ⇒ 前缀持久性：任一记录落盘则一切 LSN 更小的记录落盘。
3. 由 1+2：进程内的级联脏读（T2 读到 T1 未落盘的状态）**无需阻止**。若 T2 观察过 T1 的状态，
   则 T2 的 LSN 必大于 T1（T2 拿到实体锁晚于 T1 的 append），崩溃只截掉 LSN 后缀，重放出的
   历史不可能"有 T2 没 T1"。跨实体同理：T2 append 时持有其读过的全部实体的锁，其 LSN 分配
   晚于任何被观察记录的 LSN 分配。
4. 唯一必须堵住的是脏状态**离开进程**——外化闸门负责。transactional outbox 的 Effects 内嵌在
   `CommitRecord` 里、由 committer 在 fsync 后投递，天然满足闸门，无需改动。

## 3. 语义变更（与 Strict 的差异）

| 方面 | Strict | Pipelined |
| --- | --- | --- |
| 锁内 I/O | WAL append + fsync | 仅 WAL append（内存拷贝 + LSN 分配） |
| 拒绝点 | `Commit` 可拒绝，拒绝后内存回滚 | **`Enqueue` 是唯一拒绝点**（锁内同步）：校验、大小限制、缓冲背压全部前移；拒绝后内存回滚。**Enqueue 成功后不存在 CommitRejected** |
| fsync 失败 | `ErrCommitIndeterminate` → abandon + fence | 相同：ticket 以 `ErrCommitIndeterminate` resolve → abandon + fence，内存不回滚（后续事务可能已基于该状态入队，回滚会制造第二条历史） |
| AfterCommit 钩子 | 锁内执行 | **放锁且 durable 后执行**。钩子本就只允许外部副作用、不得触碰实体状态，此变更是放宽；迁移前需审计存量钩子 |
| 回包时机 | durable 后（锁内等待） | durable 后（**锁外等待**，Phase 1；异步回包见 Phase 2） |

## 4. 接口（roost-core/nest）

```go
const (
    DurabilityMemory DurabilityPolicy = iota
    DurabilityAsync
    DurabilityStrict
    DurabilityPipelined // 配置值 "pipelined"
)

// CommitTicket 在入队记录变为 durable 时 resolve。
type CommitTicket interface {
    LSN() uint64
    Done() <-chan struct{}
    Err() error // nil 或 ErrCommitIndeterminate，不允许其他错误
}

// PipelinedTransactionCommitter 是 committer 的可选能力。
// Enqueue 在持实体锁状态下被调用：同步完成全部可拒绝校验与缓冲准入、
// 分配 LSN 后立即返回；group-commit worker 负责 resolve ticket。
// DurableLSN 是单调水位线：LSN <= 水位线的记录全部 durable（前缀性质）。
type PipelinedTransactionCommitter interface {
    TransactionCommitter
    Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error)
    DurableLSN() uint64
}
```

约束：

- 标记为 pipelined 的 handler 要求 `Rollback != RollbackNone`（Enqueue 拒绝仍走内存回滚）。
- committer 未实现 `PipelinedTransactionCommitter` 时，派发返回
  `ErrPipelinedCommitterRequired`——不静默降级为 Strict，静默降级会掩盖运维配置错误。
- `Enqueue` 的背压策略是**同步拒绝而非排队等待**：调用方持着实体锁，等待会把背压转化为锁占用。

## 5. 提交序（invokeWithTransaction，Phase 1）

```text
1. handler 成功 → prepareCommitRecord()          # participant 物化 after-image，锁内，不变
2. ticket, err := committer.Enqueue(ctx, record)  # 锁内，唯一拒绝点
   err != nil → tx.Rollback() → 返回错误          # 与 Strict 的 CommitRejected 路径相同
3. 为每个捕获实体记录 lastCommitLSN = ticket.LSN() # 锁内（EntityBase 与 SubjectSyncState 各一份原子副本）
4. releaseLocks()                                 # 提前放锁 —— 核心收益点
5. <-ticket.Done()                                # 锁外等待 durable
   Err() == ErrCommitIndeterminate → tx.abandon() + 既有 fence 流程
6. tx.Commit()（AfterCommit 钩子）→ 结果返回 reply 路径
```

范围限定（Phase 1）：

- `RemoteWriteBatch != nil` 的跨服写事务**继续走 Strict 路径**。跨服写有独立的两阶段协议与
  fence 逻辑，混入会使复杂度翻倍；本地路径跑稳后再评估。
- `broadcastDispatch` 的每实体事务不做提前放锁（release 与 guard 清理耦合），pipelined 在该
  路径上表现等同 Strict（锁内等待 durable），语义仍正确。

## 6. 外化闸门

**实体元数据**：`EntityBase.lastCommitLSN`（原子，Enqueue 成功后锁内更新）是闸门共用的唯一
新状态；实体启用 subject sync 时 `SubjectSyncState` 同步持有一份副本（避免 FlushSubject 签名
变更与包依赖环）。

**entitysync**：`SubscriptionCoordinator.SetDurableWatermark(func() uint64)` 注入水位线。
`FlushSubject` 在 Prepare 前检查 `state.LastCommitLSN() <= watermark()`，不满足则本 tick 跳过、
dirty 保留、下 tick 重试。fsync 组提交是毫秒级、同步 tick 是几十毫秒级，闸门延迟在噪声水平。

**Data Engine**：Mongo projection 只消费已经进入统一 WAL 的记录，因此不存在独立 Entity
snapshot 抢先落地的第二条路径。`LastCommitLSN` 只用于约束同步等外化行为，不再驱动旧
Checkpoint Mod。

## 7. kit 侧（nestwal，独立交付）

- `Enqueue`：持 WAL 缓冲锁做帧编码、大小校验、缓冲背压检查（满则同步拒绝）、分配单调
  LSN、挂 ticket 入等待表后返回。
- group-commit worker fsync 一批后：原子推进 `durableLSN` 水位线，按序 resolve 该批 ticket。
- fsync 结果不确定 → 既有 `OnFatal` 熔断，同时以 `ErrCommitIndeterminate` resolve 全部未决
  ticket。
- 现有阻塞式 `Commit` 保留（Strict 档继续使用），内部可重构为 Enqueue + Wait。

## 8. 测试与验收

全部已实现（core `nest/pipelined_commit_test.go`、`nest/pipelined_bench_test.go`；kit
`nestwal/pipelined_test.go`、`nestwal/crash_test.go`、`dataengine/projector_test.go`）：

1. 单元（fake PipelinedCommitter，可控 resolve 时机/结果）：Enqueue 拒绝→回滚完好；
   indeterminate→abandon 不回滚；早放锁（探针在 ticket 未 resolve 时成功抢到实体锁）；
   AfterCommit 严格晚于 durable；committer 缺能力→`ErrPipelinedCommitterRequired`；
   白名单外 handler→`ErrPipelinedNotAllowed`。
2. 锁时长回归基准（`BenchmarkCommitLockHold`）：5ms 模拟 fsync 下 strict 锁不可用
   ≈5.0ms/次、pipelined ≈50ns/次，端到端回包延迟两者相同。
3. crash 注入 e2e（`TestNestWALCrashKeepsDurablePrefix`）：子进程持续 Enqueue 时被
   SIGKILL，重放必须是连续前缀且覆盖每个已 resolve 的 ticket——durability 承诺在真实
   进程死亡下验证。
4. 级联脏读专项（`TestPipelinedCascadedReadGatesBothRepliesInOrder`）：T2 跨实体读到
   T1 未落盘的状态并入队，LSN 序=观察序；只 resolve T1 时仅 T1 回包，全部 resolve 后
   两者按序回包。

## 9. 灰度与观测

- durability 是 per-handler 元数据，逐 handler 灰度；首选高频写、非跨服、AfterCommit 简单的
  handler 试点。
- prod 配置门禁初期要求 pipelined 显式白名单。
- 装配接线（kit >= 对应版本）：
  - Data Engine Mod 独占 WAL，并在 recovery barrier 完成后提供 committer；
  - entitysync 闸门一行接线：`coordinator.SetDurableWatermark(runtime.DurableWatermark())`；
  - 引擎选项使用 `dataEngineMod.NestOptions()`（committer + 配置驱动的
    `nest.pipelined.allowlist` / `nest.pipelined.async` /
    `nest.pipelined.async_workers` / `nest.pipelined.async_queue_capacity`），
    不配置时与旧行为完全一致。
- 已实现的指标：
  - `nest.pipelined.durable_wait`（按 handler 标签的时长分布）——worker 因等 ticket 的阻塞
    时长，**Phase 2 的决策输入**：若其占 worker 忙时比例持续偏高且加 worker 无效，才立项
    Phase 2；
  - `entitysync_flush_gate_deferred_total` —— 同步分发被水位线推迟的次数。

## 10. Phase 2（决策记录：2026-08-25 灰度试点数据）

异步回包：dispatch 返回 deferred 结果、reply 路径注册 ticket 回调、worker 立即空出。侵入每条
回包链路，预定立项标准："durable_wait 占 worker 忙时 > 30% 且加 worker 无法缓解"。

试点台架：kit 中已无独立的 pilot 台架，pipelined 路径由 `roost-kit/nestwal` 与 `roost-kit/dataengine` 的常规测试覆盖（需要 core >= v1.5.1 的
`nest.pipelined.durable_wait` 埋点）。真实 nestwal 磁盘 fsync、真实 Nest worker 池、32 个
闭环客户端、256 实体 + 25% 流量集中在一个热点实体。Apple M5 / APFS 实测（每场景 5s）：

| 场景 | 总 req/s | 热点 req/s | p50 | 热点 p50 | durable_wait 占比 | wait 均值 |
| --- | --- | --- | --- | --- | --- | --- |
| strict/w4 | 362 | 91 | 16.0ms | 204ms | — | — |
| pipelined/w4 | 361 | 91 | 16.0ms | 204ms | 99.0% | 6.73ms |
| strict/w16 | 549 | 137 | 8.1ms | 192ms | — | — |
| pipelined/w16 | 555 | 138 | 8.0ms | 189ms | 99.3% | 6.51ms |
| strict/w32 | 601 | 150 | 8.0ms | 183ms | — | — |
| pipelined/w32 | 589 | 146 | 8.0ms | 188ms | 99.3% | 6.44ms |

读数：

1. durable_wait 占 worker 忙时 ~99%（极简 handler 下的上界），远超 30% 阈值。
2. 加 worker 的收益在 w16→w32 塌缩到 +8%，热点链吞吐贴死在 ~150/s ——正是
   1/durable_wait（6.5ms）的单 worker 串行上限：热点实体按哈希固定落在一个 worker 上，
   Phase 1 下该 worker 每笔阻塞一个组提交周期，**加 worker 无法缓解**。两条立项标准均满足。
3. Phase 1 与 strict 的吞吐/延迟相同——符合设计（worker 阻塞相同，Phase 1 的收益在锁可用性，
   此闭环负载不消费锁）。

结论与判据：**Phase 2 立项条件成立**，适用场景为 durable handler 密集 + 单实体热点。可移植
判据：Phase 1 的单实体 durable 吞吐硬上限 = 1/durable_wait（本机 SSD ≈150/s；服务器 NVMe
fsync 0.5–2ms 对应 ≈500–2000/s）。任一热点实体的 durable 事务需求超过该值时，要么 Phase 2，
要么业务侧拆分该实体。偏置声明：台架 handler 为纳秒级，真实业务逻辑会稀释 wait 占比；25%
热点集中度是偏保守（偏高）的设定；闭环负载与线上开环流量的排队形态不同。

## 11. Phase 2 实现（异步完成）

由 `NestOptionWithPipelinedAsyncCompletion(workers, queueCap)` 显式开启（默认关闭 = Phase 1
行为原样）。机制（`nest/pipelined_completion.go`）：

- 派发 worker 不再停在 ticket 上：完成体（AfterCommit 钩子、release 通知、RetChan 回包）
  在**实体锁内**提交给 engine 级完成泵，worker 立即处理下一条消息。
- 泵只做"FIFO 等 ticket → 转发"，业务钩子在按实体哈希的完成 worker 池里执行；不同实体的
  慢钩子互不队头阻塞。
- **同实体完成严格按提交（LSN）序，且无条件成立**：每个事务在实体锁内领取该实体的完成链
  节点（链序 = LSN 序），三条执行路径——泵→池、池饱和时泵内联、泵满时派发 worker 降级
  ——都先等前驱完成再执行。降级只牺牲延迟（worker 被占住），不牺牲顺序与正确性。链等待
  不会死锁：前驱只等更早的前驱与自己的 ticket，后者由 WAL 独立推进。
- 完成体只捕获普通值（cap-1 缓冲的回包 channel、handler 返回值），不持有池化的 Msg；
  RetChan 缓冲保证客户端超时离开后回包发送也不阻塞。
- 泵队列满时该事务**降级为 Phase 1 的原地等待**（同步拒绝、绝不丢失）；engine Shutdown
  把未决完成视为已受理工作，排空后才结束。
- indeterminate 判定不变：完成体 abandon、不回滚、错误回包；fence 流程照旧。
- **契约变更**：AfterCommit 钩子在完成池 goroutine 上运行——没有请求上下文（goid 绑定的
  fctx/guard/CurrentRollbackTx 均不可见）、不持任何实体锁。启用前需审计存量钩子。
- 指标口径变更：async 模式下 `nest.pipelined.durable_wait` 度量的是"入队→完成"的提交管道
  延迟（并发采样），"占 worker 忙时比例"的读法只适用于 Phase 1；新增
  `nest.pipelined.async_total{result}` 计数。
- 范围：走 dispatcher 的 single/multi/multi-group 路径生效；直调、broadcast、remote batch
  维持原语义。

同台架同负载复测（Apple M5 / APFS，5s/场景）：

| 场景 | 总 req/s | 热点 req/s | p50 | p95 |
| --- | --- | --- | --- | --- |
| pipelined/w32（Phase 1 最优） | 597 | 149 | 7.9ms | 194ms |
| **async/w4（Phase 2）** | **5653** | **1416** | 4.2ms | 8.2ms |
| **async/w16（Phase 2）** | **5719** | **1435** | 4.2ms | 8.1ms |

热点链击穿 1/durable_wait 天花板 ~10 倍（149→1435/s，此时受限于 32 个闭环客户端而非框架），
总吞吐 ×9.6，p95 由 worker 排队主导的 ~200ms 塌缩到一个组提交批次的 ~8ms；w4 与 w16 结果
相同——worker 数不再是瓶颈，符合设计。验收测试：`nest/pipelined_async_test.go`（worker 解放、
同实体完成序、indeterminate、Shutdown 排空、泵满降级五项，`-race` 全绿）。

## 12. 灰度扩大到默认档的路线（规划，2026-08-26）

当前档位：`DurabilityPipelined` 按 handler 经 `NestOptionWithPipelinedAllowlist` 白名单启用，
Phase 2 异步完成经 `NestOptionWithPipelinedAsyncCompletion` 单独开关。把它推进为默认提交档，
分四步，每步有明确的量化门槛与回退开关：

1. **观测就绪（当前版本已具备）**。`nest.handler.lock_hold`（按 handler 的锁内耗时分布）与
   `nest.handler.lock_hold.slow.total` 是选择灰度对象的依据：锁内耗时被 fsync 主导的 handler
   （strict 下 hold ≈ 组提交延迟）是最先受益者。配套指标：`nest.pipelined.async_total`
   （ok/degraded/indeterminate 三分）、nestwal 的 durable lag 与批大小。
2. **白名单扩大**。按 lock_hold 排序逐批把写多读少、AfterCommit 无请求上下文依赖的 handler
   加入 allowlist；每批观察一个发布周期，门槛：`async_total{result="degraded"}` 占比 < 0.1%、
   indeterminate 为零（出现即触发 fence，属于事故而非灰度信号）、无 Data Engine WAL/projection 告警。
   Phase 2 泵参数（workers/queueCap）按 degraded 占比调整，而不是按吞吐调整。
3. **默认翻转，显式退出**。allowlist 语义反转：新增 `NestOptionWithStrictList`（规划）声明
   仍需 strict 的 handler（跨实体 remote write batch 自动豁免，广播天然无提前放锁），其余
   handler 默认 pipelined。翻转前置条件：≥ 两个发布周期内 slow.total 无新增、回放/对账
   （entitysync 的 LastCommitLSN 闸门）零分叉。
4. **收尾**。strict 档保留为逃生舱（配置可回退，无需发版）；文档把"默认即 pipelined"写入
   Host/handler 编写规范——AfterCommit 在完成池上运行、无实体锁、无请求上下文（§10 的契约
   从"选择启用者须知"升级为"默认行为"）。

不做的事：不打算移除 strict 档（remote write batch 与需要"返回即持久"语义的管理面接口
永久保留 strict）；不打算让 Phase 2 的降级路径消失（泵满回退到 in-worker 等待是保序的
安全网，见 §11）。
