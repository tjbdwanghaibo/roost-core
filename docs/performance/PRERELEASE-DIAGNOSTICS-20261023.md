# v1.31.0 候选问题诊断（2026-10-23）

基线 `435f431e`，承接 [10-22 预发布报告](V1310-PRERELEASE-20261022.md) 与 [夜间报告](NIGHT-20261022.md)。本轮修复观测漏洞、补充协议验证并保留失败现场；**不代表五项全部解决，不作为发版通过结论**。

## 五项结论

| 问题 | 本轮证据与状态 |
|---|---|
| 0.1～0.2s 全局停顿 | 原样本缺足够调度 / GC / 宿主机时间线，尚未确认；不归因于桌面活动。下一次真实游戏复跑启用既有 `ROOST_NEST_GAME_TAIL=trace` 入口并对齐时间线。 |
| Saga 30/s 的 ManualRequired | 本轮复现 2 笔，均为补偿 step 0 的 5 次结果等待耗尽；正确进入人工状态。30/s 档业务验收失败，不能把到终态当成功 TPS。旧三笔已删除，仍不能确认旧样本原因。 |
| Redis failover 返回 MOVED | 受控协议测试证明 NoRetry 下仍跟随 MOVED / ASK；连续重定向可耗尽默认预算并返回最后错误。未复跑真实切换并捕获逐次路由，历史 8 次具体路径尚未确认。 |
| async 锁内约 6.8ms 且没 fsync 指标 | **确认并修复指标漏记**。async 追加可等待后台刷盘释放 writer / stateMu，不能据旧指标排除 fsync；精确的 6.8ms 组成仍需真实负载剖析。 |
| 200ms 心跳档业务 P99 升高 | 100 / 200µs 各三轮交叉验证；200ms 周期结果方向不一致，尚不能确定原因或声称缩短时间片解决。本轮未改默认值。 |

## WAL 修复与证据

旧 `writeBatch` 仅在 requireSync 路径记录 `nestwal.fsync.duration`，后台 ticker / Ack 的物理刷段没有记录。将计时收敛到 `fsyncSegment`，不改变持久化、顺序或锁范围。指标现在统计物理段刷盘尝试（含失败），覆盖创建 / 轮转 / 关闭；不计未发生刷盘的空调用，也不含 checkpoint / 目录 fsync。因此新旧计数、均值不能直接混为同一口径。

`TestSegmentSyncMetricsIncludeAsyncFlushAndAck` 修前两个子例均失败：`physical segment syncs=1 recorded=0, want 1/1`；修后通过。`TestAsyncAppendWaitsForAnOngoingSegmentSync` 用屏障证明间接等待路径存在，不声称测出了原样本的等待占比。前后各十轮交替 benchmark 无显著差异，见 [WAL 原始数据和复跑命令](benchmarks/wal-sync-metrics-20261023/README.md)。

## Saga：保留失败证据与区分吞吐口径

fixture 在消费者 / 引擎停止后、数据库清理前导出七个集合，且新增 ExpectedTerminalTPS；不改变测试成功门槛。复测 18,000 笔、814 正向完成、7,769 补偿、9,415 失败、2 人工处理；总终态 26.35/s，预期终态仅 **1.19/s**，P99 约 115.48s。故 **30/s 不是本环境下已证明的可用容量**。

日志出现大量 command deadline passed、Mongo commit result unknown / context deadline；本轮确认的状态转换是补偿结果超时并耗尽次数，尚未分离底层 Mongo / JetStream 资源饱和的贡献。不得通过加长时限、取消上限或忽略人工状态让测试通过。需要降低输入做容量曲线、配合服务端资源与队列诊断后再确定支持档位；这里不替用户承诺新的 Saga SLA。完整计数、两笔终态和复跑入口见 [Saga 证据](benchmarks/saga-forward30-evidence-20261023/README.md)。

## Redis 与 heartbeat

新增两个 RESP 受控测试，生产 Redis 代码未改：单次 MOVED / ASK 换节点执行一次；连续四次 MOVED 在默认 MaxRedirects=3 的有界循环后返回最后错误。该测试排除了“配置 NoRetry 就禁用了重定向”的推断，不能证明历史故障一定是预算耗尽。保持未知写结果不重放的安全边界。

[Heartbeat 三轮数据](benchmarks/heartbeat-slice-check-20261023/README.md)：固定业务 worker=8、GOMAXPROCS=0（实际 10）、50,000 活跃实体、普通业务 5,000/s。100ms 周期下 100µs 时间片三轮 P99 均改善；200ms 周期下 100µs 的 P99 为 0.238 / 0.118 / 0.217ms，200µs 为 0.137 / 0.200 / 0.266ms。不能从三轮短跑得出长稳或统计显著性结论。

## 验证与限制

定向红绿测试、真实 Saga 复测和 heartbeat 交叉测试已执行，失败样本保留。`GOWORK=off go test -race -count=1 ./framework/nestwal ./infra/storage/redis/driver` 通过；两包 `go vet`、Saga 的 `go vet -tags integration` 与 integration 编译通过；`GOWORK=off go test -count=1 .` 根包门禁通过。首次沙箱运行因本机监听被拒失败，放行本机监听后重跑通过，未将沙箱失败算作产品缺陷。没有运行完整发版矩阵、Linux 复测或发布 tag。

CBM 旧图谱代际为 2026-10-14，本轮覆盖检查后按当前源码补证；索引刷新仍被共享后台服务协调状态阻塞，未停止共享服务。没有将过期索引当作最新证据。
