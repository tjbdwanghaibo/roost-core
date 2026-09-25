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
