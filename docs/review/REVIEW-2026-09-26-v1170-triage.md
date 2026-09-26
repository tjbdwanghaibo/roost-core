# 2026-09-26 v1.17.0 未登记疑点核实

范围：此前各轮复审记录（[上线前复审](REVIEW-2026-09-26-release.md)、[修复复验](REVIEW-2026-09-26-fix-verification.md)）中“疑点”节的全部条目，
在 v1.17.0（`8811c02`）导出副本上逐条核实；涉及 Mongo 的在本地 roost-it 真实副本集验证。复现附录：[REPRO-2026-09-26-04](../bug/REPRO-2026-09-26-04.md)。

## 登记（均未修复；修复方向经维护者 2026-09-26 批准，写在各 RR“修复约束”）

| 编号 | 等级 | 结论 |
| --- | --- | --- |
| [RR-20260926-33](../bug/RR-20260926-33.md) | P2 | WAL fsync 失败后仍信任后续 fsync，checkpoint / DurableLSN 越过未落盘数据；票据错误未包 ErrCommitIndeterminate |
| [RR-20260926-34](../bug/RR-20260926-34.md) | P2 | 投影在 Mongo 事务内撞键后继续读，真实 Mongo 中止事务致投影卡死、身份冲突降级为非 fatal |
| [RR-20260926-35](../bug/RR-20260926-35.md) | P2 | handler 内 CreateInScope 不在提交边界内；kit 未自动接 DurableWatermark |
| [RR-20260926-36](../bug/RR-20260926-36.md) | P2 | 冷登录等待投影无超时，连接槽被累积占满 |
| [RR-20260926-37](../bug/RR-20260926-37.md) | P2 | Remote strict 确认超时后 postRemoteCommit 永不执行，Sync 冻结、AfterCommit 丢失 |
| [RR-20260926-38](../bug/RR-20260926-38.md) | P2 | Durability 1/2 下 finalizer 与投影器并发发布同一事务 |
| [RR-20260926-39](../bug/RR-20260926-39.md) | P2 | 持久拒绝隔离的 Remote 实体无重载入口 |
| [RR-20260926-40](../bug/RR-20260926-40.md) | P2 | demo 重连时旧连接推送失败移出新连接玩家 |
| [RR-20260926-41](../bug/RR-20260926-41.md) | P3 | 零填充尾部 ≥20 字节即拒绝打开 |
| [RR-20260926-42](../bug/RR-20260926-42.md) | P3 | dataengine.shutdown_timeout 在 App 停机路径不生效 |
| [RR-20260926-43](../bug/RR-20260926-43.md) | P3 | versionedLock 续期重启窗口 |
| [RR-20260926-44](../bug/RR-20260926-44.md) | P3 | Prepare 失败 Abort 多占快池续行 |
| [RR-20260926-45](../bug/RR-20260926-45.md) | P3 | Remote 实体允许 sid 作用域 DAO |
| [RR-20260926-46](../bug/RR-20260926-46.md) | P3 | 已提交但释放失败的回复无“已提交”哨兵 |
| [RR-20260926-47](../bug/RR-20260926-47.md) | P3 | 准入判定依赖自定义 Getter 遵守 LoadedEntitiesOnly |

## 核实后判为非问题

- RR-11 同 fence 窗口版本回退：正式装配每次写 `GrantWrite` 使 fence 严格递增，迟到重放被 `entity_remote.go:88` 拒绝后判 obsolete（探针实跑）。残留加固：同 fence 下拒绝 StateVersion 回退（仅非 authority 兼容装配）。
- Remote 停机按 NextVersion 解锁：Redis 版本仅缓存，下一次 TryLock 用 Mongo grant 覆盖；准入与提交 CAS 依据 Mongo。
- kit/nest 停机超时后 entitySync 不收尾：与“超时保留所有权”一致，再次 StopWithContext 完成收尾；kit/syncbus Register 失败时 bus 已纳入 stop，stream 为持久共享基础设施。
- 重启积压不计 `WALUnacked`：正式链路 `Runtime.Start` 先 Flush 排空再 Ready；仅注释需说明“只统计本进程准入”。
- WAL terminal 后 Shutdown 恒报错：fsync 失败后本不应进程内重开 WAL（fsyncgate），kit onFatal fence 并退出；可选改进见核实记录。
- `CommitSystem` 关闭窗口孤儿票据：RR-10/17 后不存在（200 轮并发 0 孤儿）。
- RR-08 已入队续行不计入准入：上界为慢 worker 数，交替取用不饿死外部任务（文档补说明）。
- pipelined 阶段一在快 worker 锁外等 group-commit：设计内，已写入 roost-coding 豁免清单。
- 无 Guard scope 时 `GetEntityGuard()` 不归还：池内对象已清理，行为正确，仅多一次分配。

## 性能（同机 Apple M5，Go 1.27.0；机器同时有其他负载，噪声约 ±25%）

- Nest：v1.17.0 对 aaada47 微基准与 `scripts/perf/nest-msg`（10000 实体、1000 producer、100 万消息）未测出退化（request 中位 452k 对 438k TPS）；RR-25 准入探测单目标 24.7ns/op、0 alloc。
- DataEngine async + Ack（RR-16 修复）：group-commit 2ms 时吞吐慢 4～9%（p=0.032），10ms 时无显著差异、fsync 次数 +12～25%；macOS `F_FULLFSYNC` 放大，Linux 未实测。
