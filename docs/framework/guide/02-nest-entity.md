# 核心：Nest 调度与实体：设计与使用

适用版本：v1.25.0 及当前主线（未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/02-nest-entity.md) · [模块总目录](../README.md)

## 先理解这一块

同一个玩家可能同时收到购买、聊天、领取奖励等请求。Nest负责安排执行，并保护需要一起修改的数据。

你主要写handler和DAO操作；不要在这些业务函数里自行建立另一套线程、锁或保存流程。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 执行模型

Nest 接收带目标实体声明的消息，负责队列准入、同 ID 顺序、实体准备、Guard、handler 执行和事务收尾。短业务池与长业务池持本地实体锁、执行业务、冻结同步和回滚；I/O 池做冷加载、Remote 准备及 Await 查询。Entity Kind 初始化时注册 BusinessPool；任一目标属于长业务，整条消息进入长池。三池共享 ID tail，等待前驱不占业务 worker。详细配置和代码例子见 [三池与 Await](../NEST-AWAIT.md)。

长短业务 worker 内禁止同步等待网络、磁盘、数据库、另一个 Nest Request，禁止自开 goroutine 并发访问 Entity/Component/DAO。LoadedEntitiesOnly 表示只能取已加载实体；发现冷目标必须走调度器的冷加载流程，不能在 getter 里悄悄 I/O。

工作分支的短业务池默认等待容量为65536，整个池共享；配置 `nest.short.queue_capacity` 或显式构造参数可以覆盖，0取默认。前驱关系由调度器按声明 ID 维护，worker 只领取前驱完成的任务，没有固定 ID→worker 队列。容量增加不代表吞吐提高，应同时观察最老等待时间与拒绝数；延时任务的独立缺省预算仍为10000。

## 2. 锁与实体

声明目标参与同 ID 保序及冷热判断；handler 内 Cast 只允许访问本段已声明的目标，并须遵守 category 与 ID 锁序。查询后才知道的新目标通过 Await 的 WithResumeTargets 重新准入，不允许持锁补目标。GuardScope 独占锁账本，作用域退出统一释放；共享锁组的加入、退出、迁移由正式组消息协调。可重入锁不能消除跨实体锁序要求。

实体加载完成不等于可以绕过 Nest 修改它。卸载/重载会改变对象实例，依附旧对象的 Sync 等协作者须重新绑定并恢复全量基线。删除一旦准入便承担收尾责任；取消等待不会撤销已接受删除。

## 3. 事务与回滚

事务会修改且失败应恢复的状态放在 DAO。生成 setter 登记事务本地 PersistChange，回滚由 DAO 快照/undo 统一完成；组件不维护独立 undo 体系。rollback=none 不承诺失败恢复。事务外写持久 setter 被拒绝；只读和临时状态应明确声明 nopersist/nocoll。

memory 且没有 effect 的事务不写 WAL：带事务的本地持久修改由 refuseMemoryPersistentWrite 拒绝；不能再使用旧文档的“静默忽略持久修改”描述。Emit 与实体删除会提升所需持久级别。Remote 批次有独立参与者归属，不能把远端 mutation 当普通本地漏写误判。

## 4. 提交与回复

| 策略 | 正式语义 |
| --- | --- |
| memory | 内存完成，不提供磁盘保证；本地持久写不合法 |
| async | WAL 写入完成，可先回复；不等每次 fsync，不等 Mongo 投影 |
| strict | 等 WAL fsync；仍不等 Mongo 投影 |
| pipelined | Enqueue 接纳后不再回滚/重排；回复、AfterCommit、Sync 等票据持久确认与解锁 |

明确拒绝可按策略回滚；ErrCommitIndeterminate 表示结果未知，进入 fence，不能自动当未执行重试。回复错误怎样判断“是否可能已提交、能否重试”见 [§6](#6-回复错误判别是否可能已提交能否重试)。生成 //roost:nest 支持 pipelined，仍须满足运行配置准入条件。

## 5. 调用方与运行排查

业务使用生成 Sender/Client 和声明的 handler，基础设施才使用 RunLocal、隔离事务等低层入口。异步消息准入捕获当前配置快照，排队后固定一代；同步链保持当前请求代际。观察短业务、长业务及 I/O 队列、冷加载、handler 时间、持久等待分别定位，不能把慢 Remote 确认误认为普通 handler 吞吐不足。

actionflow 的 runner 回调期间产生修改先进入延后队列，在最外层返回前处理，避免迭代中改容器。

## 6. 回复错误判别：是否可能已提交、能否重试

`Nest.Request*` 与生成 Sender 返回的错误可能同时满足多个哨兵（原因链保留），只看有没有 `ErrAfterCommitFailed` 不够：嵌套独立事务已提交时回复是 `ErrNestedTransactionCommitted` 包着外层原因（可能正是 `ErrLockTimeout`）。按下表**从上到下取第一个命中的行**，全部用 `errors.Is`，不要匹配文本。带前 5 行任一哨兵即不得重试，即使链上同时有 `ErrLockTimeout`；第 6、7 行"框架没有重排、修改未回滚"也不得盲目重试。下表的 `nest.` 哨兵定义在 [nest.go](../../../framework/nest/nest.go)（每个哨兵的注释写同一组结论），`entity.` 哨兵在 [remote_protocol.go](../../../framework/entity/remote_protocol.go) 与 [remote_manager.go](../../../framework/entity/remote_manager.go)。

| # | 回复满足 `errors.Is(err, …)` | 含义 | 是否可能已提交 | 能否重试 |
| --- | --- | --- | --- | --- |
| 1 | `nest.ErrCommitIndeterminate` | 消息自己的事务或 handler 内嵌套独立事务提交结果未知，引擎已 fence | **可能** | 不得重试；等实例从 WAL 恢复后按业务幂等键核对 |
| 2 | `entity.ErrRemotePersistenceIndeterminate`（或 `entity.ErrRemoteCommitTimeout`） | 本地已持久提交，Remote 结果未知：Durability 0 远端回复丢失 / 未到达、strict 等待期间投影器报告未知、strict 等 Remote 确认到截止（另带 `ErrRemoteCommitTimeout` 与 `context.DeadlineExceeded`）、等待中结论被容量淘汰后重新登记报 `entity.ErrRemoteOverloaded`。判断只看前者；列出后者是为了只返回它的自定义 Remote 实现 | **可能** | 不得重试；结论由 Remote 后台收尾给出 |
| 3 | `nest.ErrAfterCommitFailed` | 消息自己的事务已提交，收尾（release hook、AfterCommit、Close、引用释放）失败 | **已提交** | 不得重试 |
| 4 | `nest.ErrRemotePartRejected` | 带 Remote 批次的消息自己的本地事务已提交，Remote 部分被明确拒绝：只丢弃被拒绝 Remote 实体的修改（回滚、隔离并从权威重载），本地部分照常生效，`AfterCommit` 不执行；原因（`entity.ErrRemoteRejected`、`entity.ErrRemoteVersionConflict` 等）仍可 `errors.Is`。框架自带的 remoteentity 在 async 下返回推测回执、不等 Remote 结论，不会出现本行 | **部分已提交**（本地部分） | 不得整笔重试；按业务只补做 Remote 部分，或读回状态后再决定 |
| 5 | `nest.ErrNestedTransactionCommitted` | 消息自己的事务没提交，但 handler 内或收尾阶段（Guard post-release、解锁后回调）的独立事务已提交或结果未知；消息未重排 | **部分已提交** | 不得整笔重试 |
| 6 | `nest.ErrNonRollbackNotRequeued` | 不能回滚的 handler（memory / RollbackNone）开始执行后遇锁超时 / 组迁移类错误；未准入，但失败前的内存修改已生效且不撤销，未重排 | 未持久提交，修改未回滚 | 框架不重试；确认 handler 幂等（或读回状态）后业务自行重试 |
| 7 | `nest.ErrCreatedEntityLockConflict`，且不带 `ErrLockTimeout` | 不能回滚的 handler 内新建实体锁冲突，未重排，冲突前的修改未回滚 | 同上 | 同上 |
| 8 | `nest.ErrCreatedEntityLockConflict` 与 `ErrLockTimeout` 并存 | 可回滚事务新建实体冲突：已整条回滚、自动重排仍冲突到上限（只在不命中第 1～5 行时成立） | 否 | 可重试 |
| 9 | `nest.ErrLockTimeout` / `ErrEntityLockGroupChanged` / `ErrEntityGroupTransitionPending` | 未提交：准入阶段失败，或可回滚事务已回滚；框架已自动重排到上限 | 否 | 可重试 |
| 10 | `nest.ErrNestedTransactionRollbackConflict` | `RunIsolatedTransaction` 要写外层可回滚事务已快照的实体，写持久记录前被拒绝并自身回滚 | 否（嵌套事务） | 原样重试仍被拒；把写入并入外层事务 |
| 11 | `nest.ErrNestedTransactionInRemoteMessage` | 带 Remote 批次的消息里调用 `RunIsolatedTransaction`（批次挂上之后：I/O 准备、handler 内或收尾阶段），函数体未执行 | 否（嵌套事务） | 原样重试仍被拒；把写入并入消息自己的事务 |
| 12 | `nest.ErrCommitRejected` | 提交在写任何持久记录之前被明确拒绝：committer / Enqueue 拒绝、Remote 批次定稿拒绝、准备提交记录失败、fence 之后不再交给 committer（同带 `ErrNestFenced`）、memory 事务改了持久字段（同带 `ErrMemoryTransactionPersistentWrite`）。可回滚事务已回滚；不能回滚的 handler 内存修改不撤销 | 否 | 看原因：`dataengine.ErrFencedEntityPending` 可重试，其余按业务错误处理；不能回滚的 handler 同第 6 行 |
| 13 | `entity.ErrEntityRemoved`（handler 内新建） | 同一 handler 里再建本 handler 自己较早撤销的同 ID，确定失败、未重排；可回滚事务已回滚 | 否 | 原样重试会再次失败；不在同一 handler 内重建刚撤销的 ID |
| 14 | `nest.ErrNestCanceled` / `nest.ErrNestTimeout` | 调用方自己的等待先结束（ctx 取消 / 截止，或同步等待超时）：只说明没等到回复，不说明结果——请求可能未执行，也可能已准入并在回复之后提交 | **结果未知** | 持久 handler 按“可能已提交”处理，不得据此重试；等结论后按业务幂等键核对 |
| 15 | 不命中以上任何一行 | 未提交：准入 / 停机 / I/O 准备失败、handler 返回的业务错误（可回滚事务已回滚）等。不能回滚的 handler 返回业务错误时失败前的内存修改不撤销，按第 6 行处理 | 否 | 按业务错误处理 |

表外的约束：

- **Remote 明确拒绝与结果未知的界线**：本地事务提交后 Remote `Commit` 返回的错误，带 `entity.ErrRemotePersistenceIndeterminate` 的是结果未知（第 2 行），不带的按 entity 契约就是 Remote 没有写入（第 4 行）。自带 remoteentity 的 Durability 0 由 `Commit` 直接写权威，写前校验失败与权威返回的 fenced / 版本冲突 / 拒绝不带前者，写出后回复丢失、发布失败带前者；strict（及随 strict 路径提交的 pipelined）由 WAL 投影器写权威，只有投影器写权威被拒（满足 `errors.Is(err, entity.ErrRemoteRejected)`）不带前者，其余没等到结论的失败一律带前者。自定义 Remote 实现在结果未知或没等到结论时必须带前者，否则会被当成明确拒绝。第 2、4、12、14 行的各种形态在 [reply_sentinel_table_test.go](../../../framework/remoteentity/reply_sentinel_table_test.go) 用真实 Nest 与正式 Remote Manager 钉住。
- **`ErrNestFenced`**：实例已 fence、请求未执行，或 handler 已执行但消息自己的事务在交给 committer 之前被拒绝（回复同带 `ErrCommitRejected`，按第 12 行）；等实例恢复后可重试，链上同时有前 5 行的哨兵时按那一行。fence 之后，带 Remote 批次、没有 effect 的 memory handler 的 Durability 0 直写同样被拒、Remote 批次 Abort。fence 检查是交给 committer 之前的一次性检查，不是临界区；检查之后别的 goroutine 才 fence 的窗口由 WAL terminal 兜底。fence 原因以文本写进错误、不能 `errors.As` 取到，避免原因链上的 `ErrCommitIndeterminate` 让调用方误走结果未知分支。
- **不要吞掉嵌套事务的错误**：handler 内嵌套独立事务结果未知、业务吞掉这个错误时，外层有要持久的记录则在交给 committer 前被拒（回复 `ErrNestedTransactionCommitted` + `ErrNestFenced` + `ErrCommitRejected`，按第 5 行）；外层没有要持久的记录（含 memory handler）时回复是成功，结果未知只能从之后请求得到的 `ErrNestFenced` 得知。`RunIsolatedTransaction` / `RunDetachedTransaction` 的错误要原样带进回复，才能命中第 1 / 5 行。
- **收尾阶段的独立事务**：`RunIsolatedTransaction` 在消息里从不认领消息。消息自己的事务结束后、收尾阶段调用时按嵌套独立事务处理：带 Remote 批次的消息返回第 11 行的错误；纯本地消息照常执行，已提交时回复按第 5 行、结果未知时按第 1 行 fence。收尾阶段的结果未知业务已无法带进回复，只能从之后的 `ErrNestFenced` 得知。`PrepareRemoteWriteBatch` 执行期间批次还没挂上，这时开的独立事务照常提交、不返回第 11 行；若随后消息自己的本地事务提交而 Remote 结果未知或被拒，回复同时带第 2 / 4 行哨兵与 `ErrNestedTransactionCommitted`（外层文本 `nest: a nested isolated transaction also committed`），按先命中的第 2 / 4 行处理。
- **memory 事务改持久字段**：`rollback=none` 时持久 setter 直接 panic；`rollback=state|undo`，或带 Remote 批次的 memory handler 改了本地持久字段（含 `AddMutation` 加的本地 mutation、绑定的 receipt）时提交点整笔拒绝，回复带 `ErrMemoryTransactionPersistentWrite` 与 `ErrCommitRejected`，错误点名 `collection/id` 与字段。原样重试仍被拒：把 durability 改成 `async` / `strict`，或把字段声明为 nopersist。
- **手写 `HandlerMeta` 必须写明 durability**：`Rollback` 不为 none 而没写 `Durability` 的在注册时失败（`ErrDurabilityUnset`，点名 handler 并列出可选值）；`HandlerMeta{}` 仍是 rollback=none 的 memory 快路径，`//roost:nest` 生成的 meta 总是写明 durability。
- **Guard 锁账本按 ID**：一个 handler 不跨 EntityManager 持有同 ID 的实体。Guard 以 ID 记当前持有的实例，不区分 EntityManager；这不是支持的用法，框架不为它扩大记账。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。

调度池统一使用 `short / long / io`：short、long 都执行业务并遵守 Guard，io 只处理外部等待；业务 Kind 决定短/长池。配置、API 与监控迁移见 [Nest 三池](../NEST-AWAIT.md#池命名收敛2026-10-13)。
