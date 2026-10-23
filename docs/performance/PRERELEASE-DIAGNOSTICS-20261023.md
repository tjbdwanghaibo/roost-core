# v1.31.0 候选问题诊断（2026-10-23）

基线 `435f431e`，承接 [10-22 预发布报告](V1310-PRERELEASE-20261022.md) 与 [夜间报告](NIGHT-20261022.md)。本轮修复观测漏洞、补充协议验证并保留失败现场；**不代表五项全部解决，不作为发版通过结论**。

## 首轮诊断结论（下方后续修复更新状态）

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

## 后续修复：Redis 安全重试与 Saga 容量

本节更新前述初次诊断，保留失败数据。Redis 的 MOVED / ASK 明确表示请求未执行，现加入既有有界重试分类；Cluster 自己的一轮重定向预算耗尽后仍可重寻址，执行后丢响应不重放。连续重定向最终仍会耗尽总预算，不能承诺调用方永远看不到 MOVED。确定性红绿及执行一次、结果未知、总次数回归见 [Redis 证据](benchmarks/redis-redirect-20261023/README.md)。历史八次路由未恢复，不将此修复当作对旧样本的逐条归因。

Saga 两个瓶颈分别处理：到期记录 / outbox 从“先查共同候选、再按 ID 竞争”改为有序原子认领，避免失败 worker 空领后等待下一轮；Mongo 步骤与普通完成结果允许配置有界消费并发，仍通过原订阅的延迟结算排空，默认 0/1 串行。单独加并发或单独改认领均未通过 30/s，失败试验也保留。

两项结合、两个消费 worker 的 10 分钟正常测量：18,000 笔全部正向成功，29.987 TPS，P99 576.28ms（服务端 548.00ms），ManualRequired=0、错误终态=0、发布和存储失败=0，业务与效果文档核验通过，outbox / 租约排空。没有增加时限、取消重试上限或修改成功门槛。完整试验表、微基准退化和参数见 [Saga 证据](benchmarks/saga-claims-20261023/README.md)。这是本机显式并发配置的容量证据，不能泛化为默认串行或跨机容量。

### async 等待与真实 Redis 切换补证

生成工程正式 Nest → DataEngine 链路（50 玩家、50 并发、async、15 秒）：持锁均值 6.272ms，其中 durable_commit 6.234ms、handler 0.0055ms；物理段 fsync 平均 5.494ms。block profile 确认主要在 WAL.Append 等 writer 回执，而不是业务计算。唯一 writer 同时处理后台 fsync；async 不要求本条 fsync，却仍可能排在后台刷盘之后。见 [原始指标、剖析与复跑](benchmarks/async-wal-wait-20261023/README.md)。已解释本次同量级现象，不能精确回填旧样本每一段耗时；没有为提升数字拆开持久化顺序。

另建 6 节点 Redis Cluster，负载中依次计划切换三个主节点：7383 次写脚本全部成功，无重复、无成功写入缺失、无调用错误，前后拓扑已保存；不覆盖进程崩溃 / 网络分区。

维护者补充本机当前为电池模式：本轮结果用于当前条件下的正确性与诊断，不用来对比接电时吞吐上限；功耗状态也可能改变调度 / P99，不能把长尾天然视为不受影响。

本次即时 `pmset` 同时显示供电源 AC Power、内部电池 discharging、lowpowermode=0，不能据此还原前序各次压测的功耗状态；保留用户提供的电池模式背景与这次读数，不把所有历史样本强行归为同一电源条件。

本轮代码验证：受影响 Saga / Mongo / Redis 包 `go test -race -count=1` 通过；根包门禁、`go vet ./...`、`go test ./...` 通过。真实资源验证与默认跳过的 integration 测试分开记录，未运行完整发版矩阵，也未发布 tag。

### 游戏长尾采集

[2 分钟正式重档诊断](benchmarks/game-tail-20261023/README.md)通过计数 / 实体 / Sync 数据核验，普通业务服务端 P99 6.11ms、Sync 提交→客户端 P99 12.49ms。计划→客户端最大102.93ms的捕获样本以接收侧 network wait 为主，未看到同量级 GC STW；不等于所有 goroutine 同时停顿，也不能据此唯一归因于 NATS 或功耗。保留原始 trace，旧两次停顿与200ms档波动仍不冒认已经修复。

## 本轮收口与维护者决定（2026-10-23）

已修复的代码缺陷：WAL物理刷盘指标漏记；Redis明确未执行的MOVED/ASK纳入有界安全重试；Saga有序原子认领与显式有界消费并发。对应红绿与真实负载证据保留在前文，不能回推历史丢失样本的逐条根因。

本轮追加完成 [batch与交付拆分](benchmarks/saga-batch-wire-20261023/README.md) 及 [JetStream独立/混合、资源与调用栈诊断](benchmarks/jetstream-isolation-20261023/README.md)：独立JetStream低负载约1.4ms、闭环短测约6.5万消息/s；同机Mongo持久事务足以复现交付退化，进一步观察到journal同步与NATS pwrite驻留。该结果不等于Mongo服务容量上限，也没有证明具体文件系统/设备机制。

维护者明确决定：**保留当前Mongo事务、majority + j:true、三副本及Saga生产默认配置，暂不继续压测**。batch=1仅有诊断对照，不改生产默认；不合并存储文档、不移除事务、不降低持久保障。后续具备实际Mongo服务时，再验证真实业务混合负载、网络往返与容量余量，本轮不实施部署调整。

仍保留：历史全局停顿和200ms心跳档抖动根因未确认；历史Redis八次路由与Saga三笔原记录无法完整复原；目标Mongo服务/跨机隔离存储对照尚未执行。新诊断中的空闲睡眠已单独确认、保留无效样本并加入时间缺口拒绝，不能用它解释此前所有长尾。

本次提交用于归档已完成工作和明确剩余边界，不代表完整发版矩阵、Linux复测或所有历史问题已清零。
