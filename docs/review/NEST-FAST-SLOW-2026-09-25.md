# Nest 双池复核与慢并发预算（2026-09-25）

## 结论与本轮范围

基于 RR-07 的双池实现，复核外部准入、全显式 ID 前驱、快慢交接、内部快延续、公平调度、取消/停机与 Remote 本地失败回滚。已修复 [RR-08 小队列提前拒绝](../bugfix/RR-20260925-08.md) 和 [RR-09 回滚 panic 锁泄漏](../bugfix/RR-20260925-09.md)。未新增包、未修改默认并发、未部署。

继续遵守：慢 worker 只执行 I/O 与前后置等待；Nest handler、Guard、本地事务、Entity 生命周期和锁内 Sync 都回到快池。慢池共享 worker，不按 ID 分槽；统一准入仍需记录显式 ID 前驱，保证同 ID 顺序。ID 等待者不占 worker。自定义 Loader 必须遵守 RunLocal 契约，框架不能自动阻止任意用户代码获取本地锁。

## 1024 worker / 16 等待位

这个配置在调度层可用：独立目标先取得最多 1024 个执行许可，再使用 16 个整池等待位；不因 worker 尚未被 Go 调度而在第 17 条提前拒绝。单一热 ID 只能有 1 条就绪/执行、16 条等待，增加 worker 不能突破实体顺序。热 ID 的等待预算满后，独立 ID 仍可使用空闲执行额度。跨多个显式目标的请求需全部前驱结束才能就绪。

所有慢 worker 都是 Go goroutine，不是 1024 条专属系统线程；较大数量适合独立 I/O 等待。它控制的是最大在途数，不等于提高数据库、网络或投影的服务能力。最多 1040 个外部慢请求可能同时处于准备、借用快池、确认、释放或等待阶段；内部快延续还有独立有界预算。快池 Stats.QueueLen 排除逻辑上已预留外部执行许可的就绪消息，内部延续仍可能先执行，应结合 FastContinuations 和 logic_queue 时延观察。

配置入口不变：

```go
nest.NestOptionWithWorkerPools(
    nest.WorkerPoolConfig{Workers: 8, QueueCap: 10000},
    nest.WorkerPoolConfig{Workers: 1024, QueueCap: 16},
)
```

Kit 为 `nest.slow.workers=1024`、`nest.slow.queue_capacity=16`。示例表示支持的选择，不是本机 Remote 最佳性能配置。默认继续为 max(32, 快并发×4) / 64；既有实测使用慢并发 64 / 等待 64。没有数据证明直接扩大到 1024 会提高成功 TPS，所以本轮没有替用户将全局默认改成 1024。

## 复用的性能证据

按用户“已经做了就不用做了”的要求，不重复上一轮真实负载与微基准。以下来自 RR-07 双池版，早于本轮准入和异常释放修复，不是当前代码或 1024/16 的新容量认证。

Apple M5、Go 1.27.0、GOMAXPROCS=4；快池 8/4096，慢池 64/64，Remote 投影 8；1000 会话、10000 实体，每事务 2 Entity × 2 DAO，strict，预热不计入。

| 输入/时长 | 成功/错误 | 完成 TPS | p99 | 验收 |
| --- | --- | ---: | ---: | --- |
| 100 TPS / 120 秒 | 12000 / 0 | 99.638 | 860ms | 全量 Mongo/NATS/rollback/outbox 校验通过 |
| 120 TPS / 120 秒 | 12653 / 1747 | 104.596 | 1352ms | 过载，未执行最终全量校验 |

原始目录 `artifacts/perf/remote/two-pool-{100,120}-20260925`。120 档前 32 个错误样本均为队列满，不能推断全部错误类型；不是零错误容量。100 档 Remote 确认均值 589.84ms，内部快排队均值约 0.0052ms。主要等待在后置确认，增加快 worker 没有当前证据支持。粗略用吞吐×等待估算，100 TPS × 0.59 秒约需 59 个并发等待位置；这是解释 64 档压力的近似，不是 1024 档实验结论。

1024 并发可能提升等待较长、目标独立且下游还有余量的场景；也可能增加下游排队及请求超时。若要确定其最优值，需要未来对同一后端实测不同并发档位。当前不能声称 100 TPS 是精确上限，更不能用带错误的 104.596 TPS 作为稳定容量。旧长稳、故障矩阵和两分钟前后数据分别见[原方案](../feature/REFACTOR-2026-09-25-nest-fast-slow.md)，不混用版本。

## 本轮实际验证

环境 Go 1.27.0、Apple M5；所有命令使用 `GOCACHE=/tmp/roost-nest-go-cache GOWORK=off`。

- 七包 `go test -race ./nest ./entity ./remoteentity ./dataengine/engine ./kit/nest ./kit/statslog ./sync/entitysync -timeout=120s`：通过（未变更依赖允许 Go 缓存命中）。
- 同七包 `go vet`：通过。
- `go test -race ./nest ./remoteentity -run 'TestWorkerBudget|TestRemoteRollbackPanicReleases|Test(TwoPools|SlowPool|SlowLoad|RemoteStages)' -count=20 -timeout=120s`：通过。
- 新增真实 1024 慢 worker / 16 等待位的启动、准入、背压及排空验证：包含在上述回归；不测 TPS。
- 准入旧实现配新增测试在第 17 条失败；回滚仅恢复旧锁释放代码的 overlay 失败，当前均通过。回滚探测使用另一 goroutine，避开可重入锁同 goroutine 探测的假通过。

证据目录：`artifacts/perf/nest/two-pool-review-20260925/`。不改写旧性能结果或 source-check；旧源码哈希与本轮新修复已不同。没有重跑 30 分钟、24 小时、完整集群故障矩阵或 1024 并发真实存储容量验证。

图谱采用 Verify：原代际 2026-09-25T08:49:04Z，目标符号和调用链优先走图；admit 同名接收器符号未完整呈现时读源码补证。新增/修改路径 coverage 提示 not_tracked/metadata_changed，已逐一读源码；docs 和性能产物按规则排除，直接读取。覆盖状态不是代码无缺陷或审查穷尽的证明。

收尾已刷新索引至 `2026-09-25T09:05:04Z`（18280 节点、145785 边），17 个调度/Remote/本地执行源码与测试证据路径均 metadata_match，无记录缺口；3 个历史模板部分解析与本轮无关。`git diff --check` 通过。

后续已完成新的慢并发/队列实测，见[10轮对比与100 TPS验收](NEST-SLOW-WORKERS-2026-09-25.md)。上文“未重跑”指当次复核，不代表后续状态。
