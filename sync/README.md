# sync/ —— roost 的同步块

roost core 的三块基础之一（另两块：nest 调度、dataengine）。这个目录只是收纳，**没有 Go 文件**，所以不存在叫 `sync` 的包，也不会与标准库冲突；import 的是它下面的包。

两条互不相干的轴：

| 轴 | 包 | 一句话 |
| --- | --- | --- |
| 服务 → 客户端（实体复制） | `entitysync` | 机制：进程一个 `Manager`，subject 自己持有订阅者，按会话组帧并逐帧准入，prepare/commit 两阶段，held/ready 会话，持久化门槛 |
| | `entitysync/policy` | 组织：谁订谁——`Interest`（AOI + 关系源）、`Group`（全互见）、`Direct`（显式绑定）；只调 `Subscribe / Unsubscribe` |
| | `frame` | 帧格式：`Frame` 的 `Encode / Decode`、对象 / 组件 delta、`Limits`。不知道会话和传输 |
| | `nettransport` | 传输：UDP / KCP / QUIC、AEAD、`AsyncTransport`（每会话有界 reliable 队列，给 entitysync；只有这一条 lane）、`SessionID`。不可靠 datagram 直接走协议传输的 `DatagramSender`（lockstep 这么用）。不知道帧里有什么 |
| | `lockstep` | 帧同步（输入帧）：与状态同步并列的另一种模型，共用 `nettransport` |
| 服务 ↔ 服务（总线） | `syncbus` | `ISyncBus` 契约、`DeliveryIDs`、`PatchSyncer` |
| | `syncbus/driver` | NATS（至多一次）与 JetStream（持久、确认）实现；kit 的 `SyncBusMod` 二选一装配 |
| | `syncbus/mirror` | 在总线上的副本复制器（`Envelope` upsert / delete）；`cache.ReplicaSyncer`、`remoteentity` 的快照发布用它 |

依赖箭头只有一个方向：

```
policy → entitysync → frame
                    → nettransport ← lockstep
syncbus/mirror → syncbus ← syncbus/driver
entity（内容层，在块外）← entitysync        spatial（基建）← policy
```

不在块内、但常被一起提起的：`entity/subject_sync.go`（内容层归实体）、`spatial`（几何基建）、`syncstream`（有序持久流，给 skill 用）、`remoteentity`（跨服实体协议，dataengine 块的消费者）。

文档：[ENTITY_SYNC.md](../ENTITY_SYNC.md)（怎么用）、[ARCH-10](../docs/bugfix/ARCH-10-sync-manager.md)（为什么是这个形状）、[ARCH-12](../docs/bugfix/ARCH-12-sync-package-layout.md)（为什么是这个目录）。

[2026-09-23 优化审查与重构方案](../docs/feature/REFACTOR-2026-09-23-sync-readability.md)：八包职责核对、三个已修缺陷，以及已实施的同包职责整理与 Go API 更新。

实体复制的源码阅读顺序：`entitysync/manager.go`（配置、注册与生命周期）→ `entitysync/subscriptions.go`（会话与订阅）→ `entitysync/flush.go`（捕获、准入、重试与提交）→ `entitysync/session.go`（引用分配与编码）。`subject.go` 保存订阅意图，`session.go` 保存客户端已交付状态，两者职责分开。

当前已授权优化与双模式接入已完成，进入维护；最新复核、回归和生产验收边界见 [Sync 最终收尾](../docs/feature/SYNC-COMPLETION-2026-09-23.md#2026-09-24-最终截止复核)。性能基准：在仓库根目录运行 `./scripts/perf/sync.sh`；参数、指标、同机对比和 pprof 见[基准说明](../docs/feature/SYNC-BENCHMARKS.md)。

[Profile 字段视图与优先级](../docs/feature/SYNC-PROFILES.md)：使用生成 DAO 字段名定义白名单，按来源选择视图；同次捕获共享打包，保留版本链。后续优化实施见[分批方案](../docs/feature/REFACTOR-2026-09-23-sync-next-steps.md)。

09-24：[完整实体包组帧、profile 装配校验、快照预算与队列治理](../docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。包结构和线协议保持；预算默认关闭，周期观测可改用 Counters。

[正式双模式接入](../docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)：`ModePeriodic` 默认，`ModeOnChange` 锁内冻结、解锁及提交确认后唤醒。Nest option、Kit 配置、生成 DAO 自动收集、Interest 事实队列共用现有交付流水线；`Drain(ctx)` 在生产者停止后排空已登记工作。

会话恢复按实际订阅处理：Hold / Ready / Close 使用 Manager 维护的生命周期反向索引，包含待全量与待 remove 的关系，不再逐会话扫描全服 Entity。编码引用表仍以成功交付为准；同 ID 重开不会继承旧 lifetime 的订阅。集中恢复验收见[资源预算与会话恢复](../docs/feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)。


### 快照等待与阶段诊断

on_change 下快照预算按 Interval 窗口共享；额度耗尽的快照需求单独等待下一窗口，
业务变化、退订 remove 和新订阅仍可即时触发。periodic 仍按每次 Flush 计算预算。
预算只限制客户端尚未持有对象的创建；现有对象的全量视图替换和补发不占恢复额度，
仍受帧硬上限、冻结内存和传输背压约束。Hold/Ready 清空引用后重新创建对象，仍需额度。
20Hz 恢复窗口不保证冷对象创建在 50ms 内到达客户端，排队与传输耗时需要单独计入。
`Stats().Pending` 包含预算等待中的 subject；`PendingSnapshots` 统计待基线订阅（含 held），
`WaitingSnapshotSubjects` 和 `OldestSnapshotWait` 用于检查预算等待。Stats 使用增量计数和等待链表，
完整遍历在显式 AuditStats；两者的精确比对应在生命周期静止时进行。

冷预算的计划预留不等于实际消费：开始 Push 的会话按本批冷创建计费（包括 RetryLater），
编码失败、提前取消、失效及未轮到的会话不扣费。成功帧前缀独立结算，不因后缀失败倒退。
字节预算挡住首个候选时停止后续小包插队；会话内按快照意图入队顺序，合法大对象可独占下一次额度。
on_change 同窗口停止重复冷捕获，已有对象更新不受影响；periodic 仅配置字节预算仍可能预捕获全部候选。
恢复类别目前指同一会话 lifetime 的 Hold/Ready；Close/Open 后是新入场。
具体边界和回归见[复审核实](../docs/review/REVIEW-2026-09-26-followup.md)。

可显式创建 `NewSyncTrace(capacity)`，传入 `ManagerConfig.Trace` 开启有界阶段记录；
默认 nil，不记录事件。定期调用 `Drain()` 获取独立副本及本次覆盖数，随后在锁外写文件。
事件关联 subject/version 与 session/lifetime/epoch/tick，零字段表示该阶段尚未赋值；
记录不包含 payload。传输装配方可用 `Record` 补充发送阶段，客户端接收时间仍需客户端记录。
这属于诊断功能，会影响性能；缺失或被覆盖的记录不能当成零等待。

冷基线诊断另有 `baseline_requested`、`baseline_cancelled` 和 `session_reset` 事件；
前者的 Snapshot 字段表示请求来自 held 会话的恢复集合。请求不等于收到，恢复集合里的
实体离开可见范围也不能算接收成功。AOI 工具的客户端记录和离线分析见 scripts 说明。
预算按固定 Interval 边界轮转，晚醒不推迟后续边界，空闲窗口不积攒额度；这是软预算，
不是对任意滑动窗口的严格带宽限制。

本批实现、微基准和 AOI 验收见[快照调度与长尾优化](../docs/feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md)。
