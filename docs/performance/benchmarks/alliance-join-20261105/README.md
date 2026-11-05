# 联盟 `Join` 分散到 16 个联盟时的吞吐定位（2026-11-05）

问题：总线改为按顺序键派发之后（[rpc-dispatch-20261104](../rpc-dispatch-20261104/README.md)），一个 alliance 实例上 16 个联盟各一个并发的 `Join` 只到 16～18 次/秒（KNOWN-LIMITS 同名条目）。本轮逐个候选量过，找到两处跨联盟的串行点：

1. **实体锁错位（缺陷，已修）**：`EntityManager.build` 在规范 ID 之前按 `param.Id` 向 `LockManager` 取锁。生成的 lifecycle `Create` / `GetOrCreate` 只给唯一号，`param.Id` 此时还是 0，于是**运行期新建的实体全部共用 ID 0 的一把锁**（`LockId` 校验两边都是 0，没拦住）。用例里的 16 个联盟和全部守卫都是运行期新建的，它们的 Nest 消息、`EnsureGuard` 的新建都在抢这一把锁。回归 `TestEntityManagerCreateByUniqueIDGetsItsOwnLock`（修前 `entity 103439 holds the lock of id 0`）。
2. **投影器的 effect 顺序类（契约，2026-11-05 维护者改为缺省按实体有序，见下文“维护者决定后的前后对照”）**：每条入会 / 退出记录都带“成员变化”effect，带 effect 的记录之间按 WAL 顺序投影（`projector_parallel.go` 的 `effectOrderKey`，维护者 2026-10-28 选 A），每条单独一个 Mongo 事务（约 16～18 ms），记录之间还有一次 checkpoint fsync（`checkpoint_interval` 缺省 20 ms 不长于单条投影时间，本机 macOS 约 10～12 ms）。strict Remote 写要等投影与发布完成才答复，所以 RPC 吞吐等于这条串行通道的吞吐。

只修 1 时吞吐不变（仍被 2 卡住）；1、2 都去掉（2 只在实验里去掉）时 16 个联盟的 `Join` 从 21～26 次/秒到 90～130 次/秒。

## 环境与方法

- macOS（Apple M5，10 核），Go 1.27.3；`scripts/mirror-local.sh` 私有依赖（偏移 34000：Mongo 三节点副本集、NATS 三节点 JetStream、Redis 单机）+ 自起 etcd；生成的 game-demo 二进制起**一个** alliance 实例（3000，`alliance.max_members` 500），调用方是生成的路由 `BusClient`（带路由缓存）。本机有后台负载，单样本，延迟只作量级。
- 探针用例（未提交，与 `TestAllianceE2ERPCConcurrency` 同一套 harness）：16 并发建 16 个联盟 → `Info` 预热 → 单并发新玩家 `Join` / `Leave` / 同一批玩家再 `Join`（守卫已存在）→ 16 并发、每个联盟一个 worker 各 16 次：新玩家 `Join`、`Leave`、守卫已存在的玩家 `Join`（入另一个联盟）。每段前后抓 `/metrics` 全文，16 并发段中抓 goroutine 聚合、CPU / block / mutex（8s）与执行 trace（3s）。生成工程里临时加了 `net/http/pprof`（环境变量开关，探针外不存在）。
- 检出：修前 = main `ea31ec7c`；“锁修复” = 同一基线加本轮 `framework/entity` 的修复；“实验：去 effect 顺序类” = 基线把 `projectionConflictKeys` 里的 `effectOrderKey` 去掉（只用于测量，未提交）；`ckpt500` = 配置 `dataengine.projection.checkpoint_interval: 500ms`。

## 结果（次/秒 · p50；全部 failed=0）

| 检出 | 新玩家 `Join` ×16 | `Leave` ×16 | 守卫已存在 `Join` ×16 | 新玩家 `Join` ×1 | 守卫已存在 `Join` ×1 |
| --- | --- | --- | --- | --- | --- |
| 修前 1 | 26.3 · 588 ms | 25.5 · 628 ms | 21.5 · 728 ms | 13.0 · 74 ms | 16.1 · 60 ms |
| 修前 2 | 22.1 · 702 ms | 20.8 · 717 ms | 24.1 · 653 ms | 12.3 · 77 ms | 17.1 · 57 ms |
| 修前 3 | 20.6 · 747 ms | 23.8 · 658 ms | 24.9 · 640 ms | 13.1 · 77 ms | 14.4 · 69 ms |
| 锁修复 1（负载高，单并发也慢） | 11.9 · 1.36 s | 13.3 · 1.19 s | 15.3 · 967 ms | 8.2 · 117 ms | 12.2 · 81 ms |
| 锁修复 2 | 22.0 · 701 ms | 23.2 · 675 ms | 21.3 · 743 ms | 10.8 · 90 ms | 14.1 · 70 ms |
| 实验：去 effect 顺序类 | 54.7 · 273 ms | 62.2 · 234 ms | 63.3 · 233 ms | 13.0 · 76 ms | 16.1 · 60 ms |
| 实验 + 锁修复 | **90.0** · 170 ms | **130.3** · 113 ms | **110.0** · 123 ms | 12.7 · 77 ms | 16.2 · 61 ms |
| 修前 + ckpt500 | 40.9 · 379 ms | 42.4 · 369 ms | 40.9 · 376 ms | 10.8 · 93 ms | 14.2 · 71 ms |
| 锁修复 + ckpt500 | 39.2 · 378 ms | 43.9 · 343 ms | 35.8 · 420 ms | 12.0 · 80 ms | 14.8 · 67 ms |
| 实验 + ckpt500 | 52.1 · 288 ms | 69.6 · 223 ms | 69.0 · 220 ms | 11.1 · 85 ms | 16.2 · 60 ms |

单并发不变（承诺本身：一条请求的延迟由它自己的往返决定）。

## 各阶段（`/metrics` 在 16 并发“守卫已存在 `Join`”一段的差值，256 次）

| | 修前 2 | 实验 | 实验 + 锁修复 |
| --- | --- | --- | --- |
| Nest 派发总耗时（`nest_dispatch_cost`，均值） | 646 ms | 225 ms | 136 ms |
| 其中等 strict 确认（`remote_entity_remote_confirm_wait`） | 590 ms | 85 ms | 83 ms |
| Remote 写准备（`remote_prepare_latency`，两次 `GrantWrite` 与加载） | 32 ms | 34 ms | 33 ms |
| handler 持锁（`nest_handler_lock_hold`） | 14.8 ms | 14.6 ms | 18.9 ms |
| WAL 批次 / 追加 | 256 / 256 | 256 / 256 | 64 / 256 |
| WAL fsync（均值） | 10.4 ms | 9.8 ms | 10.0 ms |
| Remote 发布单个事务 / 其中写“已发布” | 17.8 / 17.4 ms | 18.9 / 18.1 ms | 15.5 / 14.1 ms |
| 写门等待（`remote_write_gate_wait`，512 次） | 约 0 | 约 0 | 约 0 |

执行 trace（3s 窗口）里投影器 goroutine：修前在 Mongo 网络等待约 1.16s、checkpoint fsync（`nestwal.storeCheckpoint`）约 0.88s；ckpt500 时网络约 2.18s、fsync 约 0.25s（一轮结束时的确认仍在）。CPU 约 0.12 核（8.14s 采样 980ms），不是计算瓶颈。

## 各候选的结论

- **总线**：不是（上一轮已修；16 个 handler 同时在跑）。
- **Nest io / 业务池**：不是。四次 goroutine 快照里 43～48 个派发 worker 空闲，16 个入会 handler 里 14～16 个停在 `remoteWriteBatch.Commit → waitTrackedRemoteTransaction`（等 strict 确认），没有在池里排队。
- **Remote 写额度 / 写门**：不是。写门等待约 0，没有写准入拒绝；`GrantWrite` 是每个实体自己的 CAS（`_remote_entity_meta` 按 `_id`），不是全局额度。它让单条 `Join` 多约 30 ms（准备阶段两个实体各一次），是单联盟热点延迟的一部分，不跨联盟串行。
- **守卫新建与认领**：不是主因。16 并发时新玩家与守卫已存在的玩家吞吐相当（修前 20.6～26.3 对 21.5～24.9）；单并发新玩家多 10～17 ms（守卫冷加载未命中 + 新建 + 首写认领）。但新建守卫暴露了实体锁错位：修前快照里两个不同玩家（6107010、6109010）的 `EnsureGuard` 等在同一个 `ReentrantMutex` 上，实验检出里 16 个 handler 有 8 个停在这里。
- **strict Remote 写的 Mongo 往返**：是，但起作用的方式是经投影器串行：每条记录的 Mongo 事务在一条通道里逐个提交，strict 写等它（确认等待占派发时间的 91%）。
- **列表 / 全量查询**：没有。`Join` 期间的 Mongo `Find` 只有 Remote 提交 outbox 的待发布分页（每页 ≤256，按状态索引）、DataEngine effect outbox 的认领、新玩家守卫的按 ID 冷加载；没有按 kind 扫描、`ListOwnedRemoteEntities`、“全部联盟”之类的查询（CPU / 网络等待 profile 与 goroutine 快照都没有）。Redis 只有路由登记与续期。

## 维护者决定后的前后对照：effect 缺省按实体有序（2026-11-05）

维护者决定：effect 缺省**按实体有序**，全局 WAL 顺序改为缺省关闭的 `dataengine.projection.effect_order: wal`（契约与仓内消费者核对见 [impl/03 effect 顺序范围](../../../framework/impl/03-dataengine.md#effect-顺序范围)）。同机同配置、同一探针（上一节的用例，这一轮不开 pprof 采样），私有 `scripts/mirror-local.sh`（偏移 36000），修前 = main `a2762377`（已含实体锁修复），修后 = 同一基线加本改动（未提交的工作树，生成的 alliance 配置不写 `effect_order`，即缺省 `entity`）；修前 / 修后交替跑，本机负载高（修后第 1 轮开始时 load average 约 45）。

| 检出 | 新玩家 `Join` ×16 | `Leave` ×16 | 守卫已存在 `Join` ×16 | 新玩家 `Join` ×1 | 守卫已存在 `Join` ×1 |
| --- | --- | --- | --- | --- | --- |
| 修前 1 | 23.0 · 691 ms | 24.4 · 647 ms | 25.3 · 634 ms | 10.9 · 92 ms | 12.5 · 72 ms |
| 修前 2 | 25.0 · 630 ms | 25.9 · 614 ms | 26.0 · 598 ms | 13.0 · 77 ms | 12.0 · 66 ms |
| 修前 3 | 25.7 · 622 ms | 27.1 · 583 ms | 26.7 · 589 ms | 12.1 · 81 ms | 16.0 · 62 ms |
| 修后 1 | **108.4** · 134 ms | **125.4** · 101 ms | **137.7** · 100 ms | 14.1 · 71 ms | 16.6 · 59 ms |
| 修后 2 | **92.5** · 154 ms | **100.9** · 128 ms | **128.0** · 113 ms | 11.8 · 80 ms | 15.4 · 64 ms |
| 修后 3 | **94.0** · 156 ms | **103.8** · 134 ms | **118.5** · 125 ms | 12.3 · 80 ms | 16.3 · 59 ms |
| 修后 + `effect_order: wal` | 22.8 · 688 ms | 24.6 · 654 ms | 25.9 · 609 ms | 11.6 · 83 ms | 14.8 · 67 ms |

全部 failed=0，进程日志 0 条 ERROR。16 并发约 4～5 倍；单并发不变（一条请求的延迟由它自己的往返决定）；打开 `wal` 回到修前的数（配置项端到端生效，原行为保留）。修后 `Leave` 的 p99（约 310～410 ms）比 p50 高得多，未单独定位（本机负载高、每轮 256 次只是一份样本；并行窗口是静态通道分配，一个慢单元拖住它所在通道与本窗口确认，可能是来源之一，未验证）。

## 后续

同日下午把并发逐级加到 16 / 64 / 128 / 256、定位并调整了几个并发缺省值，见 [alliance-join-1000-20261105](../alliance-join-1000-20261105/README.md)。

## 复跑

探针脚本与用例在本轮的证据目录（`artifacts/joinperf/probe/`，未提交）；正式入口仍是 `TestAllianceE2ERPCConcurrency`：

```sh
ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<偏移> scripts/mirror-local.sh up
source <私有目录>/roost-dataengine-it/env.sh
ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$' scripts/test-alliance-e2e.sh
scripts/mirror-local.sh clean
```
