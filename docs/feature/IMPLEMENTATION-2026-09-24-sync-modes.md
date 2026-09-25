# Entity Sync 双模式正式接入实施记录

日期：2026-09-24。基线：`967bc69` 加既有 Sync/Nest 工作树。对应[设计方案](PLAN-2026-09-24-sync-modes.md)。运行代码已实施，未提交、未发布；本记录区分功能验证与性能边界。

## 使用方式

两种模式共用 Entity 内容、Interest、Profile、版本、共享编码、完整实体包组帧与 Transport。默认 `ModePeriodic` / `periodic`，`Interval` 默认 50ms。`ModeOnChange` / `on_change` 在成功准入且仍持有 Entity 锁时冻结，全部锁释放且提交确认后唤醒同一个 Manager；50ms 负责积压与恢复兜底。

纯 Go 装配：

```go
syncManager, err := entitysync.NewManager(entitysync.ManagerConfig{
    Mode: entitysync.ModeOnChange, // 省略即周期模式
    Interval: 50 * time.Millisecond,
    Transport: transport,
    DurableWatermark: durableLSN,
})
if err != nil { return err }
engine := nest.NewEngine(
    nest.NestOptionWithGetter(entityAccess),
    nest.NestOptionWithTransactionCommitter(committer),
    nest.NestOptionWithEntitySync(syncManager),
)
// 在接流前完成 Entity 注册、Interest 规则、会话 held/ready 装配。
if err := syncManager.Start(ctx); err != nil { return err }
if err := engine.Start(); err != nil { return err }
```

正式 Kit 装配：`kit/nest.NewModWithEntitySync(getter, EntitySyncSetup{Config: managerConfig, Configure: override}, nestOptions...)`。`Init` 创建 Manager，`EntitySync()` 提供给业务安装 Interest、实体与会话；`Provide` 注入 Nest option；`Start` 先启动 Sync，再启动 Nest；停止时先排空 Nest，再 Stop/Drain/Close Sync；Drain 包含等待提交的兴趣事实与预算暂缓工作，超时明确返回错误。网络传输必须在该 Mod 之后停止，DataEngine 水位来源也必须继续存活到 Sync 排空结束。

```yaml
sync:
  entity:
    mode: periodic
    interval: 50ms
    max_frozen_bytes: 67108864
```

优先级：ManagerConfig 初值 → 配置文件 → `EntitySyncSetup.Configure`。启动日志显示最终模式与周期。未知模式、负周期/容量、无法解析的周期报错；on_change 未绑定正式提交生产者时 `Start` 报错。纯 Go 通过 Nest option 自动绑定；独立执行器可绑定后使用 `entity.BeginSyncMutation` 的相同协议，不能把 `BindSyncProducer` 当作代替提交边界的开关。

第一版按 Manager 配置，启动后固定；回退时停止接流并排空，以 periodic 重启。客户端线协议无需切换。

## 正式链路与所有权

```text
生成 DAO setter / 手写 Entity.MarkSyncDirty
  → 独立客户端变化记录 / 事务内暂存
  → Nest 成功准入（pipelined 先写 CommitLSN）
  → periodic：登记 dirty；on_change：冻结待交付内容
  → 全部 Entity 解锁 + 原提交协议确认
  → Manager 单一 Flush 所有权
  → 应用已提交的 Interest 事实
  → 取得冻结内容或重新捕获
  → 持久化水位、Profile、订阅 revision、快照预算
  → 完整实体包组帧 → Transport.Push → 原有逐帧结算
```

- 生成 `TakeEntitySyncChanges()`，Nest 成功准入时自动消费。业务无需增加末尾 Publish/Flush；同步但不持久化的字段同样有效。`Tracker` 增加独立客户端掩码与回滚快照，客户端不消费服务间 `TakeSyncDirty`；服务间重试不重新发布客户端标记。
- 单 DAO 保留字段 mask。多 DAO 可实现 `entity.SyncChangeMapper` 显式映射；未映射时采用 Full，避免两个 DAO 的 bit 0 被错误合并。手写实体可实现 `SyncChangeCollector`，手写同步字段继续使用 MarkSyncDirty/MarkSyncFullDirty。
- 普通准入拒绝和回滚丢弃当前作用域标记，保留旧 pending。原有 RollbackNone 不提供业务数据撤销能力；需要错误后恢复字段的 handler 应使用原有 State/Undo 回滚策略。
- 捕获错误不反悔已成功提交的业务，保留 dirty，并记录 SubjectSyncState.LastError。pipelined 和 remote batch 的确认继续沿原有完成/远端确认机制运行；结果不确定时屏障不放行，沿用 fencing/recovery。
- Cast 动态取得的实体也加入同一边界。启用正式同步接入后，ReleaseCast 在该业务作用域内不提前解锁，统一留到准入捕获后释放；这是为了防止已经释放的实体在捕获前被下一笔业务修改。pipelined 的 CommitLSN 包括这些实体。它可能延长原来依赖 ReleaseCast 提前释放的持锁时间。
- 单实体与多实体共用同一协议；跨实体仍没有新增客户端原子事务。广播按原有每实体操作执行。

## 内容与调度

`entity/sync_frozen.go` 保留每实体一个待处理内容槽，当前在途交付仍由原有 prepared token 管理。空闲时冻结 delta；已有槽位或交付在途时冻结最新 Full，合并未交付中间状态。delta 基线变化或缺少新 Profile 时重捕获整个 Subject 的一致版本，不修改旧 delta 的基线强发。

Manager 默认冻结字节容量 64MiB，包含冻结槽及已转交 prepared 的冻结内容，提交/中止/关闭后归还。超过容量则保留 dirty，进入正常捕获恢复；`Counters.FrozenBytes/FrozenPeakBytes/FrozenDeferred` 显示保留字节、保留峰值与冻结失败次数。该容量不包含 packer 的瞬时分配和普通捕获、编码工作区，原有帧硬上限与传输队列治理仍然生效。

Flush 在复制 Profile 需求与订阅 revision 后释放订阅锁，再取得 Entity 锁捕获，最后按 revision 核对。避免 Entity→订阅锁和订阅锁→Entity 的反向嵌套。捕获期间新增/变更的订阅留到后续轮次，已经准入的帧仍按原契约不可撤回。

即时与周期共用 flushGate。通知可以合并；未解锁/未确认的任务不会靠重复即时通知空转，周期仍检查 pending。on_change 的快照额度在同一个 Interval 窗口共享，不能每次即时 Flush 都重新领取；periodic 保持每次 Flush 的预算规则。

## Interest 正式接点

- `Interest.QueueMove(entity, position, observer)` 和 `QueueRelation(entity, source, subjects)` 在业务锁内复制事实并关联当次提交条件；handler 应返回队列/参数错误。
- Manager 捕获前自动调用已登记的政策处理器；成功提交且全部解锁后才应用事实。回滚事实丢弃，同实体不能越过尚未确认的前序事实。
- `QueueMove` 在入队时校验实体、观察者与坐标；`QueueRelation` 校验来源并复制成员。队列默认最多 65536 条，可用 `InterestConfig.MaxQueuedFacts` 调整；满队列明确返回错误。
- 订阅容量拒绝继续由 Interest.retry 保留，不阻止已排队的 remove 释放容量。旧的 Move/Apply 手工接口继续有效；启用正式事实队列后，其 Apply/重试由 Manager 调度。
- 进入/离开、held/ready、显式订阅、退订、换视图与退役继续通过既有正式接口，相关 pending 会唤醒 on_change。具体地图和授权规则仍由业务提供；不自动猜测哪个字段是位置。

## 文件落点

未新增包：`entity/sync_commit.go` 管提交条件与变化暂存，`sync_frozen.go` 管不可变内容槽；`nest` 的统一事务入口和 Cast 接线；`sync/entitysync/mode.go` 管模式、唤醒与容量，原 flush/snapshot_budget 复用；`policy_queue.go` 和 `policy/interest_queue.go` 管正式兴趣事实；`kit/nest/entity_sync.go` 管配置与生命周期；Entity 生成器输出变化收集。DAO 模板沿用现有 MarkSync 调用，通过 Tracker 的独立掩码获得新的客户端记录。

## 验证

已通过：

```sh
GOWORK=off go test -race ./entity ./nest ./dataengine ./dataengine/engine \
  ./sync/entitysync/... ./kit/nest ./codegen/internal/entity \
  ./codegen/internal/dao ./scripts/perf/sync-aoi -timeout=120s
./scripts/test-sync-modes-generated.sh
GOWORK=off go vet ./entity ./nest ./dataengine ./sync/entitysync/... \
  ./kit/nest ./codegen/internal/entity ./scripts/perf/sync-aoi
GOWORK=off go build ./...
```

新增验证覆盖：默认配置/漏接线、成功提交与全锁释放、真实 Nest 唤醒、WAL 确认前阻断、拒绝/回滚、Cast 动态实体、独立 DAO 消费与回滚、冻结槽连续覆盖/在途预算、容量降级、新 Profile 重捕获、快照窗口预算、兴趣事实确认/回滚、Kit 生命周期。生成器测试在临时最小工程生成 DAO 与 Entity，再实际执行两种 Nest→Sync 路径，字段 setter 不调用额外 Publish。

双进程冒烟：20 玩家 / 200 实体 / 每人 20 可见 / 5% / on_change / AsyncTransport，内容、版本和最终可见集通过；正式规模对照结果见下节。测试程序默认使用正式 Nest 内存提交，不生成或启动示例游戏。

## 业务负载对照

两轮四组共八次测试均完成，所有运行的内容、版本、最终可见集校验通过，无会话丢失。测试为两进程回环 TCP，各 GOMAXPROCS=4；1000 玩家、10000 实体、每人约 50 可见、200 个 50ms 输入窗口。1% / 5% 固定为每窗口 100 / 500 次业务变化（2000 / 10000 次每秒），与 Sync 发送频率分开。

| 模式 | 变化率 | 轮次 | 修改→客户端 p99 / max（ms） | 准入→客户端 p99（ms） | 修改延迟 >50ms / 样本 | 分配 MB/s |
| --- | --- | --- | --- | --- | --- | --- |
| periodic | 1% | [1](../../artifacts/perf/sync/modes-20260924/periodic-1/client.json) | 131.64 / 186.99 | 131.64 | 21271 / 96786 | 32.10 |
| periodic | 1% | [2](../../artifacts/perf/sync/modes-20260924/periodic-1-repeat/client.json) | 56.22 / 66.80 | 56.20 | 16606 / 96685 | 32.15 |
| periodic | 5% | [1](../../artifacts/perf/sync/modes-20260924/periodic-5/client.json) | 62.38 / 140.94 | 62.38 | 145218 / 477268 | 159.00 |
| periodic | 5% | [2](../../artifacts/perf/sync/modes-20260924/periodic-5-repeat/client.json) | 61.69 / 109.33 | 61.69 | 140194 / 477341 | 158.84 |
| on_change | 1% | [1](../../artifacts/perf/sync/modes-20260924/on_change-1/client.json) | 2.95 / 18.79 | 2.80 | 0 / 108695 | 34.95 |
| on_change | 1% | [2](../../artifacts/perf/sync/modes-20260924/on_change-1-repeat/client.json) | 2.40 / 13.95 | 2.28 | 0 / 108715 | 35.02 |
| on_change | 5% | [1](../../artifacts/perf/sync/modes-20260924/on_change-5/client.json) | 5.24 / 22.71 | 5.22 | 0 / 566077 | 194.48 |
| on_change | 5% | [2](../../artifacts/perf/sync/modes-20260924/on_change-5-repeat/client.json) | 10.40 / 64.24 | 10.40 | 530 / 565579 | 194.14 |

完整原始报告：[运行目录](../../artifacts/perf/sync/modes-20260924/)，包含每轮 server.json/client.json、退出码及实际命令。退出码 1 保留严格 50ms 门禁失败，不等于内容校验失败。

结论：on_change 明显缩短正常同步延迟，1% 两轮均通过严格门禁；5% 第一轮通过、第二轮存在 530 / 565579 个修改延迟超过 50ms 的样本，最大 64.24ms（计划输入延迟超过 50ms 为 1336 个）。因此不认定 5% 已稳定满足全部样本 50ms。periodic 两轮都未通过该严格门禁；1% 首轮长尾明显高于复测，原因未专项定位，原始结果未剔除。

代价也要保留：5% 下 on_change 每秒约 5.2 万帧，periodic 约 2.0 万帧；分配约 194MB/s 对 159MB/s。共享编码和帧格式未变，较少等待也减少了批量合并机会。第二轮 on_change 的冻结保留峰值约 17.4KiB（1%）/ 110.8KiB（5%），最终归零，冻结容量降级为 0。负载组件增加了提交时刻字段且经过正式 Nest，不与旧独立 Sync 工具的历史分配数字直接作优化百分比比较。

复跑：

```sh
ROOST_PERF_COUNT=2 ROOST_PERF_LABEL=sync-periodic-1 ./scripts/perf/sync-aoi.sh -mode=periodic -dirty=1 -async
ROOST_PERF_COUNT=2 ROOST_PERF_LABEL=sync-change-1 ./scripts/perf/sync-aoi.sh -mode=on_change -dirty=1 -async
# 将 dirty 改为 5，label 换成新值即可复跑 5%；结果目录不能覆盖已有报告。
```

延迟同时记录计划事件、实际业务修改、成功准入三个起点；传输准入至客户端另外记录。新建可见对象的旧快照不当作刚发生的状态变化；同一实体版本对应的业务 step 在每个客户端只记一次。所有收到的帧仍检查完整性、版本和最终可见集。

这些是本机内存提交验证；不包含真实 WAL 延迟、跨机网络、弱网、远端事务故障注入或长稳运行。本轮未改变这些部署保证，也不把本机测试作为线上所有 MMO 场景的容量承诺。

## 索引与证据范围

已刷新 codebase-memory：`Users-whb-roost-roost-core`，代际 `2026-09-24T04:51:47Z`，17921 nodes / 140816 edges。28 个本轮涉及的正式源码/测试路径 coverage 为 metadata_match/no_recorded_issue。四个性能程序源文件与三个生成最小工程源文件仍按仓库规则排除，采用实际源码、编译和运行结果验证；没有把图谱排除理解为没有代码。既有两处未改动 demo 模板 parse_partial 保留。覆盖元数据是 best-effort，不代表全仓审计。
