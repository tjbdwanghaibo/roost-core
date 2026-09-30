# Service 故障与容量学习：从可重试状态到业务可证明的回收

对应[第十二轮实测](REVIEW-2026-09-30-services-12.md)、[覆写探针及结果](evidence/service-review-20260930-01/README.md)。这里记录机制与实施边界，避免把本机通过误读成生产环境的完整验收。

## Session：状态提交与外部效果不是一个事务

[`resolve`](../../service/session/service.go)先通过 `Runs.Update` 将 run 置终态，再进入 `releasePending`。后者先调用外部 `Releaser.Release`，成功后逐资源 `markReleased`，最后移除 claim。进程可在外部效果与 `markReleased` 之间退出；重启后终态 run 仍带 pending，`Finish` 或 Sweep 可继续回收。这个顺序保证失败可见和可重试，但不能单靠 Core 状态对外部回调提供 exactly-once。实测子进程在回调后 `os.Exit(73)`：回调调用两次，Redis `SETNX` 模拟资源效果仅发生一次，说明**调用方的持久幂等键是必要条件**。实际资源分配器须以稳定 `(runID, resource kind, resource ID, action)` 或其等价业务键记录释放意图/结果，并允许重复请求返回同一结果；效果未知时还须能查询或对账，不能把重复 Release 当成异常再泄漏。

[`Get`](../../service/session/service.go)计算过期读视图，但不写回 run。真正调用 `Finish` 时，超过 deadline 的 open run 会落成 `expired`，完成可完成的回收，并返回 `ErrRunExpired`。调用者应同时看返回的 run 与错误，不可由 `ErrRunExpired` 推断“资源没有释放”。本轮第一份 HA fixture 在 1 分钟 TTL 外验证，暴露的是测试断言对该契约的误读；用 5 分钟 TTL 重跑，保留对真正过期场景的正确判断。

## Cluster：副本接管与写入结果未知要分开

[`clusterRecoveryHook`](../../redis/driver/cluster_recovery.go)在网络错误后请求异步刷新槽位，不自动重放可能已执行的写命令。本轮 `WAIT 1=1` 确认一个测试写被副本看到，强杀拥有该 slot 的 master 后，在副本提升、槽位恢复时由**另一个 Go 进程**完成同一 run 的后续操作。这支持“此拓扑、此写入、此故障下可恢复”的窄结论。若写入方只收到断连/超时，它仍需用稳定 request ID 与存储查询判断是否提交；普通 `WAIT 1` 之外的写、落盘与复制竞态、双向分区和多个 owner 同时迁移均未证明安全。测试使用本机 AOF everysec，也没有真正跨机器故障域。

## OwnerSource 决定积压公平性

[`SweepPending`](../../service/session/sweep_source.go)每轮只从提供的 `OwnerSource` 取至多 `limit` 个 owner，再交给 `Sweep`。本轮轮转源按 100/100/50/100 个 owner 返回，首轮一处读错被保留供第四轮重试；实测处理数为 `99,100,50,1`。这个结果验证了 Session 对**已提供的轮转页**可持续推进和重试，并不说明框架自己会枚举所有 owner。实施方需持久维护 owner 游标、重启后继续轮转，并确保错误 owner 不永久占满页；度量 backlog 年龄、每 tick 处理量、失败重试次数与尾部延迟。外部真实 Redis read fault 仍需代理故障测试。

## Match：原子单 key 的增长代价

[`queueState`](../../service/match/queue_store.go)把 Waiting、Tickets、Matches、SubjectTickets、Requests 放在一个 versioned value，单次 CAS 让队列与 ticket 状态一起提交。`clone()` 每次复制历史 map；Redis versionstore 再读写整份 JSON。终态 Tickets 和 Request 映射保留，重放语义因此稳定，但历史量越大，热点队列的每次请求都会搬动越来越多的旧数据。本轮 16 对串行操作在 Redis 历史 64→16384 时耗时 `48.039→5234.860 ms`、Go 分配 `8.38→2742.88 MB`；8192 条旧记录上的 128 对样本 p99 为 334.625 ms（**测试进程本地值**）。增长方向和源码结构一致；这个有限样本不应被当成生产 p99 或“达到某个固定容量必然失效”的结论。

[已有 Match 归档交接](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)仍是后续实现入口：把活跃队列与终态查询/请求幂等结果分开，保留重放窗口和所有权判断；归档/清理要与当前 CAS 版本及旧请求迁移协调，不能先删 Requests 再寄望终态 ticket 可找回。落地时使用实际业务请求分布、热点并发、历史数据迁移和长稳 SLO 重测，记录 key 大小、GC、Redis CPU、重试冲突与尾延迟。

## 尚缺的物理网络故障层

[`redis/driver/lock_toxic_integration_test.go`](../../redis/driver/lock_toxic_integration_test.go)已有丢释放回复、丢获取回复和延迟三项测试。它们本轮全部 skip，尚未构成故障验证。补齐独占 Redis、Toxiproxy API 与代理地址后，必须确认它们真的执行 `pass`，再扩展到 Session/Match/外部资源的写后丢回复、读失败、分区恢复和跨进程重建。检查 test skip 数比单看 `go test` 退出码更可靠。
