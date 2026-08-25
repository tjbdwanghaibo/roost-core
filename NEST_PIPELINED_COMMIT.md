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

## 4. 接口（cube-core/nest）

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

**checkpoint**：不变量一句话——**checkpoint 永不领先 WAL**。提交 after-image 的路径（kit
checkpoint mod / entity repository）对 `entity.LastCommitLSN() <= committer.DurableLSN()` 做同样
检查，未 durable 的实体本轮不 take dirty。否则崩溃后会出现"存储里有、WAL 重放历史里没有"
的状态。未接入水位线的部署不受影响（watermark 未注入时闸门旁路）。

## 7. kit 侧（nestwal，独立交付）

- `Enqueue`：持 WAL 缓冲锁做帧编码、大小校验、缓冲背压检查（满则同步拒绝）、分配单调
  LSN、挂 ticket 入等待表后返回。
- group-commit worker fsync 一批后：原子推进 `durableLSN` 水位线，按序 resolve 该批 ticket。
- fsync 结果不确定 → 既有 `OnFatal` 熔断，同时以 `ErrCommitIndeterminate` resolve 全部未决
  ticket。
- 现有阻塞式 `Commit` 保留（Strict 档继续使用），内部可重构为 Enqueue + Wait。

## 8. 测试与验收

1. 单元（fake PipelinedCommitter，可控 resolve 时机/结果）：Enqueue 拒绝→回滚完好；
   indeterminate→abandon 不回滚；早放锁后同实体第二事务可入队且 LSN 递增；AfterCommit 严格
   晚于 durable；committer 缺能力→`ErrPipelinedCommitterRequired`。
2. 锁时长回归基准：人工 10ms fsync 延迟下对比 strict vs pipelined 的锁等待分布，量化收益。
3. crash 注入 e2e（kit，照 `fatal_fence_test` 模子）："Enqueue 后、fsync 前"kill，重放后断言：
   已回包请求的效果必在恢复状态中；未回包请求效果可无但不矛盾；checkpoint 后端不存在超出
   durable 前缀的 after-image。
4. 级联脏读专项：T1 入队未 durable、T2 读 T1 状态并入队；fsync 只到 T1 时 T2 不回包，全完成
   时两者都回包且重放等价。

## 9. 灰度与观测

- durability 是 per-handler 元数据，逐 handler 灰度；首选高频写、非跨服、AfterCommit 简单的
  handler 试点。
- prod 配置门禁初期要求 pipelined 显式白名单。
- 指标：enqueue→durable 延迟直方图、水位线滞后 gauge、闸门跳过计数（entitysync/checkpoint
  各一）、worker 因等 ticket 阻塞时长。

## 10. Phase 2（按需）

异步回包：dispatch 返回 deferred 结果、reply 路径注册 ticket 回调、worker 立即空出。侵入每条
回包链路，只有"worker 等 ticket 阻塞时长"指标证明成为瓶颈时才做。
