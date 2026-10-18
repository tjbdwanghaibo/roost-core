# 实体定时器回调共享调度视图

基线 Host 为 `63b63fa8`，修后为固定审查分支的 Host.active 调度视图复用实现；BenchmarkHostPeriodicTick 与 memStore 夹具两侧一致。Linux/amd64，Go 1.27.2，GOMAXPROCS=4，同机分别编译测试二进制，旧 / 新交替 10 轮，均记录 ns/op、B/op、allocs/op。

命令形态：两侧 `go test -c ./framework/nest/entitytimer`，交替运行各自二进制的 `-test.run '^$' -test.bench '^BenchmarkHostPeriodicTick$' -test.benchmem -test.benchtime=400ms`，然后 benchstat。每侧相同的 1 / 128 个常驻周期节点，每次推进 1 秒再 Tick。

`stable/` 是减少其他编译验证干扰后的样本，作为本次比较依据：1 节点 2.586µs → 2.602µs（p=0.353）；128 节点 3.641ms → 3.658ms（p=0.912），均未观察到显著差异，分配不变。这里的 memStore 为每次写记录整份 map 的 undo 快照，128 节点场景约 8 MiB/op；它是固定夹具成本，不是实际生成 DAO 的分配数字，不能推导生产吞吐或容量。

目录顶层的 before / after / benchstat 是首批 100ms 样本，当时并发运行其他检查，区间很宽；原样保留，不用它声称性能改善。没有测跨机、长稳或 SLG 在线规模。

修前行为回归原文（TestHostCallbackOperationsShareTheCurrentTick）：

```text
next-stage timer persisted=1 but driver due=0001-01-01 00:00:00 +0000 UTC present=false
canceled stage still fired 1 times
```

修后保留下一阶段、取消、显式改期、panic 清理和事务回滚 / 重试的行为回归；性能比较不替代这些正确性判据。
