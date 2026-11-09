# EntityTimer 到期预算验证

2026-11-09，macOS ARM64 / Apple M5，GOMAXPROCS 未覆盖（基准后缀 10）。基线 `f5736ea2`，修改版为本目录证据同批提交的工作树。

正式执行入口仍为 Nest Dispatch；没有另造 worker、事务执行器或公共转发层。持久定时器与 heartbeat 的差异和预算配置见 [时间手册](../../../framework/guide/10-time.md#与-heartbeat-的分工及共同执行链)。

## 正确性

- [修前](red.txt)：一次投了 300 条而非最多 128；队列满仍尝试 5 次；取消的调用仍预占并投递。
- [两套链路 race](green.txt)、[补充回归](green-final.txt)：128/128/44 分批排空、队列满后停止且恢复不丢任务、kick 不能绕过系统冷却、失效条目占预算、失败不覆盖新报告；包含既有事务与生命周期测试。
- [正式生成工程](generated.txt)：`TestEntityTimerGeneratedProjectEndToEnd` 通过。受影响包 vet、根包文档与格式门禁通过。

- 复核补充：队列满以外的准入失败（引擎停止中）修前一轮尝试 5 次（`an admission failure made 5 attempts in one round, want 1`），修后结束本轮；新增 `entitytimer.overdue_ms` 积压指标（`TestAdmissionFailureEndsTheRoundAndOverdueIsReported`）。

## 微基准

`BenchmarkDriverDueBatch` 每次处理 128 个到期实例，假客户端仅返回准入成功，不运行 Nest handler。每轮推进测试时间 10s，让它们再次到期；不包含真实等待或持久化。改前/改后交替顺序，共各 10 轮：

```sh
go test -run '^$' -bench '^BenchmarkDriverDueBatch$' -benchmem -benchtime=100ms -count=1 ./framework/nest/entitytimer
```

改前通过 Go overlay 读取基线 driver.go，额外加一个不被旧驱动读取的 `systemNow func() time.Time` 字段，使同一测试时钟夹具可编译；旧生产函数体不改。样本见 [改前](before.txt)、[改后](after.txt)、[benchstat](benchstat.txt)。

128 实例一轮的中位耗时 13.85µs → 13.01µs（-6.02%，p=0.000）；7680B → 175B，10 → 2 allocs。减少整批列表分配抵消了逐条短锁的成本。本结果不代表 Nest 端到端 TPS、生产容量、Linux 或长稳验证；默认 128/10ms 只是单 Driver 的保护性预算。大量不同 Driver 同时到期、单实体很多任务和长 handler 仍需业务容量验证。
