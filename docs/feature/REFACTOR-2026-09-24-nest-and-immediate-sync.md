# Nest 优化审查与即时状态同步方案

日期：2026-09-24。基线：`967bc69` 加既有 Sync 工作树。初次审查修复两个正确性缺陷；后续已实施双模式同步及 N1 同包整理，并修复 Cast 事务边界。最新进度见第 10 节与 [Nest 收尾验收](NEST-COMPLETION-2026-09-24.md)：既定 N1～N4 已完成，未提交、未发布。第 1～7 节保留初次审查时点的事实与方案。

后续方案：[Entity Sync 双模式正式接入](PLAN-2026-09-24-sync-modes.md) 替代本文 S1–S3 的实施安排。周期模式保持默认，变化触发模式通过配置选择；两者共用正式的 Nest、Entity、Sync、Kit 与生成器能力，不依赖示例业务接线。本文保留当时的审查证据。

用户目标：Nest 执行业务修改，Entity 锁释放前完成同步数据捕获，解锁后立即推动状态同步；20Hz 仅兜底。负载维持 1000 玩家、10000 全局实体、Interest/AOI 每人约 50 可见；每 50ms 窗口 1% / 5% 变化，分别为 2000 / 10000 次变化每秒。业务变化量必须独立于发送调度频率。

## 1. 审查范围与证据

采用 codebase-memory Verify：最近项目 `Users-whb-roost-roost-core`，初始代际 `2026-09-24T02:59:00Z`，查询 Nest 文件与方法、事务调用链、Entity release 和 Sync 捕获实现。相关候选路径 coverage 为 metadata_match/no_recorded_issue；这只是 best-effort 信号，不代表整仓完整审查。

重点读了 `nest` 的准入、路由、组锁、事务、异步完成、ticker，以及 `entity/entity_guard.go`、`entity/subject_sync.go`、`sync/entitysync/{manager,flush,subject}.go`、DataEngine 的实体删除适配和 demo Player/Scene 接线。源码补查了生成模板的字符串调用，不能把图谱未找出模板调用理解成零调用。docs/scripts 在 fast 索引之外，以当前文件为准。

修复后已刷新核心模块索引：代际 `2026-09-24T03:57:02Z`，17828 nodes / 139571 edges；两个运行文件与两个新增回归文件均为 metadata_match/no_recorded_issue。未改变的两处 demo 模板 parse_partial 仍保留，文档不在 fast 索引范围。

历史文档：[事务与 WAL](../../NEST_TRANSACTION_WAL.md)、[pipelined 提交](../../NEST_PIPELINED_COMMIT.md)、[生命周期与性能](../review/IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)、[业务负载](SYNC-BUSINESS-LOAD-2026-09-23.md)。历史性能和路径归属不直接视为本轮测量结果。未穷尽 remoteentity、Saga、跨服提交、所有生成业务及真实外部存储故障。

## 2. Nest 当前实际调度

```text
生成 Sender / nest.Client
  → Dispatch / Request：校验、复制消息信封、队列准入
  → Dispatcher：普通 / heartbeat / cost worker 池
  → NestDispatch：绑定请求上下文和 EntityGuard
  → single / multi / multiGroup：加载、Touch、排序、获取组锁与 Entity 锁
  → invokeHandlerTransaction / invokeWithTransaction
  → handler 修改组件与 DAO
  → 提交或回滚
  → release hooks、解锁、AfterCommit、回复或异步完成
```

- 业务消息到达即进入 worker 队列，不等待 Nest ticker。`nest.Ticker` 驱动注册的周期回调并提供 frame；`entitysync.Manager` 有自己的周期，两者不是同一个 20Hz 调度器。
- 同一路由 worker 会串行执行队列；跨普通/cost/heartbeat 池、多实体操作的一致性仍由 Entity 锁和组锁保证，不能把 worker 哈希等同于所有访问全局 FIFO。
- multi/multiGroup 保留业务参数顺序，另构建排序后的锁集合。重构时不能为了复用切片破坏 handler 的实体顺序或缺失实体语义。
- Request 的返回代表业务/提交结果，尚不代表客户端收到了 Sync 帧。异步 Dispatch 成功只表示准入。

### 提交模式与同步触发位置

| 模式 | 业务锁内 | 解锁后 | 即时同步约束 |
| --- | --- | --- | --- |
| memory | 修改、按策略回滚、提交内存结果 | post-release 回调 | 仅成功修改可发布；无事务 memory 快路径需要显式成功出口 |
| async / strict | 准备记录、committer.Commit、接受持久化版本、准入回调 | AfterCommit / 回复 | async 与 strict 持久化保证不同，不统一描述为 fsync 完成 |
| pipelined | Enqueue、接受版本、写 CommitLSN、准入回调 | 等 ticket；可能在完成池执行 AfterCommit / 回复 | 锁内可冻结，外发必须遵守配置的 durable watermark；准入失败可回滚，不确定结果必须 fencing |

广播和 remote batch 不使用普通 pipelined 提前放锁路径。本轮没有改变这些模式的默认配置。

## 3. 本轮确认并修复的问题

1. [RR-20260924-01](../bug/RR-20260924-01.md)：`AfterAdmission` 只在 `RollbackTx.Commit` 运行，pipelined 此时已经解锁，异步模式甚至运行在另一个 goroutine。真实消费者是 DataEngine 的实体删除 finalizer。本轮改为写入 LSN 后、交出锁和事务所有权前执行，后续 Commit 不重复调用；回调失败保留并在完成时报告，不回滚已准入状态。[修复](../bugfix/RR-20260924-01.md)
2. [RR-20260924-02](../bug/RR-20260924-02.md)：release hook panic 会跳过组锁与 scope 清理，正常释放及部分加锁失败都可触发。本轮用显式所有权交接与 defer 保证清理，panic 继续交给现有恢复边界。[修复](../bugfix/RR-20260924-02.md)

这两项属于原有契约修复，不表示即时同步已经接入。核心边界补充中文注释。

## 4. Nest 与 Sync 当前如何连接

```text
Nest handler
  → 业务组件 / 生成 DAO setter
      持久化变化：登记到当前 RollbackTx 的 PersistChange
      同步变化：DAO Tracker.MarkSync
  → demo Player.PublishSyncDirty
      TakeSyncDirty → EntityBase.MarkSyncDirty → SubjectSyncState.MarkDirty
  → Manager.Register 安装的 notifier
      markPending(subjectID)，只登记，不唤醒发送
  → Manager.run 等 Interval（demo 为 50ms）
  → Flush：读取订阅 → Entity 锁内 PrepareViews → watermark 检查
      → 会话组帧 → Transport.Push → 结算版本/订阅
```

证据入口：[demo Player](../../demo/game/entities/player/entity.go.tmpl)、[demo Scene](../../demo/internal/service/game/scene.go.tmpl)、[Entity 内容状态](../../entity/subject_sync.go)、[Manager](../../sync/entitysync/manager.go)、[Flush](../../sync/entitysync/flush.go)。

当前框架并不自动在 Nest 成功出口把所有 DAO 的 sync dirty 转成 Entity 同步任务；demo 组件显式调用 PublishSyncDirty。该方法可能发生在 handler 尚未提交时。现有周期路径会重新获取 Entity 锁读取当前值，所以回滚后多余的 subject dirty 至多引起冗余更新；不能把它直接替换成锁内立即外发，否则会外化后来回滚的中间状态。

EntityManager 的 release hook 确实在解锁前运行，但解锁也发生在回滚、异常和加锁失败路径，单凭 release 事件不能推断事务成功。`AfterCommit` 也不是锁内捕获点：正常 dispatch 下它通常交给 guard 的 post-release，pipelined async 下则在完成池运行。

### 不能直接在 release hook 调 Flush

1. 当前 Flush 先获取 Sync `subject.mu`，再经 PrepareViews 获取 Entity 锁。若业务持 Entity 锁再进入 Flush/订阅锁，会形成反向锁顺序。
2. Flush 还会扫描其他 pending subject、修改会话引用/帧时钟并调用 Transport，不适合塞进某个 Entity 的锁内。
3. `PreparedSubjectSync` 当前每个 subject 只允许一个在途捕获；这是交付协议状态，不是可无限追加的业务提交队列。连续提交需要独立的冻结内容生命周期。
4. pipelined 的 AfterCommit 只保证同主实体的完成顺序；共享次实体的两笔事务不能据此假定同步内容顺序。

## 5. 重构目标：保持少包，先让流程可读

当前相关结构（省略测试和不变文件）：

```text
nest/
  nest.go                 配置、错误、引擎生命周期
  client.go / dispatcher.go / msg.go
  nest_dispatch.go         注册、路由、诊断、加载/锁、事务调用混排（1103 行）
  rollback.go             undo、回调、记录准备、提交路径、DAO 捕获混排
  transaction.go / persist_change.go / pipelined_completion.go
  group_lock.go / group_transition.go / remote_access.go / cast.go
  ticker.go / stats.go
```

目标只在原包内整理：

```text
nest/
  nest.go / client.go / dispatcher.go / msg.go    保持公开入口和生命周期
  handler.go              从 nest_dispatch.go 移入注册、校验、查找
  trace.go                从 nest_dispatch.go 移入慢请求和 trace 诊断
  nest_dispatch.go         路由、加载、Touch、锁和业务执行主线
  execution.go            从 rollback.go 移入 invokeWithTransaction 及提交模式分流
  rollback.go             事务状态、undo、准入/完成回调、DAO 捕获
  transaction.go / persist_change.go / pipelined_completion.go
  group_lock.go / group_transition.go / remote_access.go / cast.go
  ticker.go / stats.go
```

不新增 `scheduler/manager/executor` 包，不移动公开符号，不增加兼容转发层。实体生命周期/锁继续由 entity 提供；Nest 不导入 sync；Sync 不依赖具体游戏组件，装配层连接二者。生成 Sender 与 `kit/nest` 不需要因文件移动修改 import。

### 按批推进

| 批次 | 具体改进 | 验收和回退 |
| --- | --- | --- |
| N0（本轮完成） | 两个正确性缺陷修复、回归、问题与修复文档 | 定向修前失败/修后通过，Nest/Entity/Sync/DataEngine race |
| N1 | 上述同包文件整理；执行主线按调用、准入、解锁、完成命名；清理把 release 等同于持久化的过时注释 | 公共 API/import 不变，相关包测试和全模块编译；整批文件移动可独立回退 |
| N2 | 准入/锁释放/完成阶段的内部结果和所有权显式化；统一 single/multi/multiGroup 收尾，保留广播与远端特例 | panic、回滚、准入拒绝、不确定提交、完成队列满、取消、停机；不能用单一 success bool 混合已提交但回调失败 |
| N3 | 补阶段观测：排队、等锁、handler、捕获、WAL 等待、释放 hook、完成队列延迟 | 当前 lock_hold 在 release 调用前记录，不能用它评估新增锁内捕获的全部成本；新增指标与旧指标口径分开 |
| N4 | 再以 profile 决定热路径整理 | 每请求的慢请求 timer/channel、单实体集合临时分配、周期回调列表复制是候选；未测前不承诺收益，不取消卡死诊断 |
| S1–S3 | 下一节的即时同步执行路径，属于行为变化 | 独立配置回退周期模式；不可把文件重构与发送时机变更混在同一验收里 |

Go 以当前 go.mod/toolchain 的 1.27.0 为准。适用处使用现有 `sync.OnceFunc`、`slices`、类型化 atomic 等 API；不为替换写法抬高最低版本，不机械抽象无复用步骤。

## 6. 即时同步的具体接入方案（待实施）

### S1：一次成功业务操作形成冻结结果

- 在 Nest 成功准入且仍持有全部目标锁的阶段收集实际变化实体，一次 handler 的多字段变化合成一次捕获。setter 只标脏，不调用网络。
- 框架提供可选的提交观察接口，由应用装配 Sync 适配器。接口只表达实体变化、提交/释放状态，不让 Nest 持有 Sync 会话或导入 Sync 包；不能只靠业务每个方法手写 AfterCommit。
- 接口必须覆盖无事务 memory 快路径、严格提交、pipelined、广播的逐实体操作及失败出口。remote batch 保留最终确认前不可外化的约束，先维持原路径，不能声称所有模式同时支持。
- pipelined 在 Enqueue/Accept 成功、LSN 写入后冻结；只有捕获到的 CommitLSN 满足 durable watermark 才允许对外发送。不确定提交不发成功通知，沿用 fencing/recovery。
- 冻结数据必须独立拥有 map/slice/bytes，不能把 DAO 指针交给发送 goroutine。Profile 需求用只读快照获取，不能持 Entity 锁反向取得订阅锁；订阅变化导致缺少视图时进入新快照流程，不能复用私有字段更多的旧视图。
- 现有自定义 packer 仍需要 Entity 锁来读取字段，其打包耗时必须算进持锁时间；后续若做“锁内只复制字段、锁外编码”，需要单独明确 packer 的冻结接口，不能假设原 packer 自动支持。

### S2：锁外立即唤醒，单一发送顺序

- 锁内完成数据冻结和有界本地任务登记；任务带不可变内容、实体/订阅代际及 CommitLSN。发送端必须等释放/确认条件满足，不让完成池抢先外发未释放的提交。
- 解锁后发合并唤醒信号；一个 Sync 执行者统一处理即时通知和周期任务，保持会话帧时钟、引用表、remove/create、版本链顺序。多个同时到达的通知可在本轮自然组帧，不额外等到下一次 50ms。
- 捕获序列与交付基线分开建模：当前一个在途 PreparedSubjectSync 不能直接当提交队列。连续版本不允许随意丢弃旧 delta；若任务达到上限，应保留 dirty/缺口状态，显式切换相关订阅到最新全量恢复，并通过完整基线替换重新接续版本。
- 先做小规模可验证原型：同一实体连续两次提交、第二次回滚、第一版发送在途时 Profile 缩小、部分帧失败、快照与 remove 竞争。证明所有权和锁顺序后再接千人负载。
- AOI/关系事件也走有序待处理流程：移动造成的进入/离开/换视图不能仍滞留到 20Hz；先结算本次可见关系，再决定对应完整实体包的接收者。

### S3：20Hz 成为兜底

- 每 50ms 检查真实 pending 状态、暂缓的持久化水位、快照预算、可重试的交付与会话恢复，不重复发送已完成内容，不做全部实体的无差别打包。
- 数据先登记、后通知，通知可合并；通知丢失不能等于数据丢失。持久化未就绪时不把 requeue 当作新的即时通知，防止空转；可由 ticket 确认唤醒，周期负责补漏。
- 非立即支持的远端场景、显式低优先级任务仍可走周期路径，但必须在配置与指标中区分。
- 上线回退只切换调度方式，继续保持完整 EntitySync 包分帧、可靠队列背压、Profile 权限、watermark 与失败恢复，不改变线格式。

这条路径的“立即”表示取消正常路径的人为周期等待，不承诺零排队、零编码或零网络延迟；WAL 未就绪也不能绕过一致性要求。

## 7. 验证方案与本轮实测

本轮完成：基线 `go test ./nest ./entity ./sync/entitysync/...` 通过；新增回归在修前复现两个问题，修后通过；`go test -race ./nest ./entity ./sync/entitysync/... ./dataengine/engine` 通过；相关 vet 和 `go build ./...` 通过。具体日志与命令见两份 bugfix 文档。本轮没有跑新的 MMO 性能压测，不把正确性修复说成已获得吞吐收益。

即时同步验收需新增实际链路：生成业务 Handler → Nest 锁/提交 → 冻结内容 → Interest → AsyncTransport → 独立客户端解码。

1. 不等待周期也能收到成功提交；回滚/拒绝不泄漏中间状态，持久化未确认不提前外发。
2. 证明捕获发生在锁内、网络准入/等待发生在锁外；多实体参数顺序与每会话版本链保持。
3. 暂停即时唤醒后，20Hz 能处理已登记的 pending；未登记 dirty 无法靠兜底凭空恢复，需要另外验证所有 setter 接线。
4. 固定 2000 / 10000 次业务变化每秒，对照纯周期与即时+兜底；不再让 `dirty-per-tick × hz` 改变总负载。
5. 保留每人约 50 可见与相同的变化时间线/实体分布。记录请求排队、持锁、提交至发送、变化至客户端 p50/p95/p99/max、frames/s、bytes/s、alloc/s、全量恢复/暂缓次数。
6. 覆盖聚集热点、连续同实体写、慢会话、订阅 churn、WAL 延迟和长时间运行。仍保留严格 50ms 门禁失败，不把 20Hz 本身视为达标证明。

后续实施顺序以[双模式正式接入方案](PLAN-2026-09-24-sync-modes.md)为准，N1/N2 的可读性与执行边界整理随正式接入推进。后续双模式现已实施，见[实施记录](IMPLEMENTATION-2026-09-24-sync-modes.md)；默认仍是周期 Flush。

## 8. 后续实施：N1 与 Cast 事务边界

2026-09-24 继续 Nest 优化，复用上述已形成的方案。没有新增包或更改公开注册/调度签名，没有将 setter 改成发送入口。

### N1 已实施

| 原位置 | 现位置 | 职责 |
| --- | --- | --- |
| nest_dispatch.go 注册相关声明 | [handler.go](../../nest/handler.go)（181 行） | 全局/实例注册、元数据校验、热更新查找、handler 类型与 option |
| nest_dispatch.go 慢请求、trace、指标 | [trace.go](../../nest/trace.go)（379 行） | 保留慢请求 timer、堆栈诊断与原指标口径 |
| rollback.go 执行入口及 nest_dispatch.go 事务入口 | [execution.go](../../nest/execution.go)（307 行） | memory / durable / pipelined / remote 分流，准入、释放与完成所有权 |
| nest_dispatch.go 其余部分 | [nest_dispatch.go](../../nest/nest_dispatch.go)（1103 → 518 行） | 请求上下文、实体加载/Touch/排序/锁、路由及最终回复 |
| rollback.go 其余部分 | [rollback.go](../../nest/rollback.go)（741 行） | 回滚状态、参与者、提交记录准备、回调、DAO 捕获 |

```text
nest/
  nest.go / client.go / dispatcher.go / msg.go
  handler.go
  nest_dispatch.go
  execution.go
  rollback.go / transaction.go / persist_change.go
  pipelined_completion.go
  trace.go
  group_lock.go / group_transition.go / remote_access.go / cast.go
  ticker.go / stats.go
```

仅改变同包文件归属，不新增 import 依赖，Kit、生成 Sender 与调用方不迁移 API。Nest 仍通过 Entity 提交契约接入 Sync，不直接导入同步实现。

采用当前 Go 1.27 支持的 `maps.Clone` 复制注册表，`slices.SortFunc` / `cmp.Compare` 表达实体锁顺序；保留先 group、再 ID 的排序规则。补充中文执行边界和 Cast 释放契约注释。

文件迁移先单独用 Go AST 比对：修复后、搬迁前的 116 个非 import 顶层声明，与搬迁后完全一致（忽略注释/排版）。随后才替换上述标准库写法，相关测试再次通过。不靠减少文件行数宣称运行性能提升。

### 本轮正确性修复

[RR-20260924-03](../bug/RR-20260924-03.md)：原实现仅在 SyncMutation 存在时为 Cast 新实体捕获事务状态，并延迟释放；未接 Sync 时业务失败或准入拒绝会留下动态实体修改，pipelined 还漏写动态实体 LSN、持锁等待 WAL。

现在这些都是 Nest 自身的事务职责，Sync 接线只增加同步内容处理。`ReleaseCast` 在事务内延迟到统一边界释放，属于明确的正确性行为变更，详见 [修复记录](../bugfix/RR-20260924-03.md)。无事务且无正式 Sync 作用域的提前释放行为保留。

### 验证与证据

- 新增两项测试、其中回滚测试含两个 Request 子用例：修前均复现，修后通过。
- `GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race ./nest ./entity ./dataengine ./dataengine/engine ./sync/entitysync/... ./kit/nest ./codegen/internal/nest ./codegen/internal/entity -timeout=120s`：九个包全部通过。
- `GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go vet ./nest ./entity ./kit/nest ./codegen/internal/nest ./codegen/internal/entity`、`GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go build ./...`：均退出 0；build 报告一次模块版本 stat cache 写权限提示，未导致编译失败。
- `GOCACHE=/tmp/roost-nest-go-cache bash scripts/test-sync-modes-generated.sh`：真实 DAO/Entity 生成最小工程 race 通过，`ok syncmodes 1.531s`，不依赖 demo。
- `git diff --check` 通过。未跑新的 MMO 性能压测、真实存储故障注入或跨机长稳，不覆盖这些生产结论。

图谱采用 Verify，初始代际 `2026-09-24T04:51:47Z`；调用链确认 `invokeHandlerTransaction`、独立/隔离事务共享执行入口。相关 Nest/Entity 文件覆盖无记录缺口；图中的同名 `call`、`Join` 存在不相关边，直接核对源码，不将其视为真实依赖。搬迁后的文件曾显示 not_tracked/metadata_changed，已以源码和编译补证并刷新索引，代际 `2026-09-24T05:52:48Z`（17931 nodes / 140922 edges）；本轮七个代码/回归文件均为 metadata_match/no_recorded_issue。两个未改动的 demo 模板仍有 parse_partial；docs/scripts/生成夹具按 fast 规则排除，本轮直接阅读并执行适用验证。

### 后续批次

N2 仍需统一 single/multi/multiGroup 的重复收尾和细化执行结果，保持主实体缺失、次实体 nil、分组参数顺序、广播逐实体、远端最终确认及异常清理契约。不能把本轮文件整理等同于 N2 完成。

N3 应补排队、等锁、准入捕获、release hook、WAL 和完成队列的分阶段指标，旧 `nest.handler.lock_hold` 不含完整释放 hook 成本，暂不更改其口径。N4 在这些指标和 profile 后再优化 timer、临时集合和 ticker 分配。本轮未修改 worker 数、队列策略、Sync 模式默认值或通知频率。

回退时，N1 的同包搬迁可独立回退；RR-03 是独立行为修复，不应为了回退文件布局而恢复事务中途解锁。既有工作树修改均保留，未提交或发布。

## 9. Nest 常规调度收尾（N2a）

2026-09-24，按用户新的方向继续 Nest 自身优化。现有支持范围的双模式 Sync 链路作为已接通的契约做回归，本轮不修改 Sync，也不围绕同步增加调度特例。

本批承接第 5 节 N2 的常规调度收尾部分。目录结构保持第 8 节不变，仅在 `nest/nest_dispatch.go` 内提取两段有实际复用的流程，不新增包或公开 API：

| 路径 | 调整后的职责 |
| --- | --- |
| singleDispatch | 查找 handler，Get 单实体，Touch/UnTouch，然后调用 dispatchLoadedEntities |
| multiDispatch | 查找 handler，调用 dispatchMany |
| multiGroupDispatch | 查找 handler，保留各组长度和展平顺序，调用 dispatchMany |
| dispatchMany | 统一 GetMany、次实体 nil 占位、主实体存在检查和引用配对 |
| dispatchLoadedEntities | 统一组迁移检查、锁顺序、锁获取、事务调用与幂等释放 |

执行路径为 `路由/加载与 Touch → 组检查和锁 → invokeHandlerTransaction → 解锁 → UnTouch`。本轮主调度文件 518 → 470 行。多实体的业务参数、锁排序集合和引用归还集合继续独立，避免为了省一份切片破坏参数顺序或资源所有权。

保持以下原有契约：主实体缺失拒绝执行；次实体缺失保留 nil；重复实体按参数次数 Touch/UnTouch，但锁仍去重；空分组长度保留；成功、业务错误和 panic 都清理；单实体继续使用 Get，不改成 GetMany。去掉的 `len(lockEs) == 0` 检查由“主实体非 nil 且 Touch 成功”保证，不是放宽准入。

广播保持独立的逐实体事务，不并入普通批量调用。本轮确认并修复 [RR-20260924-04](../bug/RR-20260924-04.md)：原 defer 的释放 hook panic 漏归还引用并跳过剩余广播；独立 defer 确保清理完整并留在逐实体恢复边界内。[修复记录](../bugfix/RR-20260924-04.md)。

### 验证

新增 [dispatch_lifecycle_test.go](../../nest/dispatch_lifecycle_test.go)：广播释放异常修前失败、修后通过；三个常规路由 × 三种业务结果九个子用例，重构前后均通过，并刻意打乱 ID 顺序、保留重复项和 nil、包含空组、在 handler 内清空参数切片，验证框架没有混用业务数组和资源所有权。

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test ./nest \
  -run 'Test(BroadcastReleasePanic|DispatchRoutesPreserve)' -count=1 -timeout=30s
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race \
  ./nest ./entity ./dataengine/engine ./sync/entitysync/... \
  ./kit/nest ./codegen/internal/nest -timeout=120s
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go vet ./nest
```

定向测试、Nest vet 与上述七个包的 race 均通过（Nest 2.674s，DataEngine/engine 4.329s，Kit/Nest 2.527s，其余为有效缓存）。索引刷新至 `2026-09-24T06:01:52Z`，17940 nodes / 141031 edges；两个改动代码文件均为 metadata_match/no_recorded_issue。初始图谱路径、调用链和覆盖先经 Verify 核对，再读源码补充资源释放顺序；无记录缺口不是完备性证明。未做新的吞吐、延迟或分配压测，不宣称性能数值改善。

后续仍以 Nest 的正常执行链为主：N2 剩余是事务内部结果与执行所有权整理；N3 补排队、等锁、业务、提交等待和清理阶段指标；N4 再用 profile 决定热路径优化。现有 Sync 仅保持兼容回归。回退时可独立回退两个常规路由 helper，保留 RR-04 的资源清理修复。

## 10. Nest N1～N4 整体验收

2026-09-24，用户授权一次完成 Nest，已完成剩余 N2 事务主线与交接所有权、N3 可选分阶段指标、N4 基于 CPU/heap profile 的 timer、回调快照和临时集合优化。第 8、9 节的“后续”描述是当时状态，由本节更新。

补充修复 [RR-05 异步完成早于解锁](../bugfix/RR-20260924-05.md) 和 [RR-06 Ticker 启停竞争](../bugfix/RR-20260924-06.md)。正式 Sync 链路保持既有契约：setter 标脏，锁内成功准入捕获，解锁和提交确认后允许发送；周期模式默认，变化触发模式可选。

完整职责、验证命令、性能前后数据、阶段指标开关与适用边界见 [NEST-COMPLETION-2026-09-24.md](NEST-COMPLETION-2026-09-24.md)。既定批次无待实施项；通用 metrics/fctx 基建热点和线上多机故障/长稳验证不冒充本次 Nest 微基准已覆盖的能力。

2026-09-24 截止复核：补充 RR-07/08 后按当前范围收口，具体门槛、验证和保留契约见 [收口结论](NEST-COMPLETION-2026-09-24.md#8-截止复核与收口结论)。
