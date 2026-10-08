# 02 Nest 调度与实体：说明

**2026-10-08更新：** EntityManager.Destroy在锁等待和删除准入前响应ctx取消，已准入或结果未知后仍完成销毁收尾。 历史条目与验收边界见[本轮收口](../../feature/REFACTOR-2026-10-08-historical-closure.md)。下方旧版本行号保留原时点。


> 配套实现文档：[impl/02-nest-entity.md](../impl/02-nest-entity.md)。
> 源码基准：tag `v1.23.0`（`28912cd6`），文中 `path:line` 都按这个 tag。codebase-memory 图谱停在 2026-09-23 的 generation，比 tag 旧，只用来定位；全部结论按 tag 源码核对。
> 读者：写业务 handler 的人、做运维的人。要看控制流、不变量与 review 清单，读实现文档。

## 速览

- **是什么**：Nest 是修改 Entity 的唯一入口。一条消息先在统一准入处按“声明的目标实体 ID”排队，再由**快池**执行 handler——取实体锁、跑事务、提交、放锁、回复；**慢池**只做冷加载、Remote 准备这类等待。
- **最重要的保证**：同一 ID 的消息按准入顺序执行；handler 运行时它声明的全部目标都已按全局锁序加锁；handler 返回错误、panic 或提交被明确拒绝时，DAO 里的修改整笔回滚；已越过提交点的消息绝不自动重排。
- **最容易踩的坑**：① 在 handler 里等待——再发 `Request` / `Dispatch`、自己开 goroutine、冷加载实体，都会被拒绝或 fail-fast；② 把事务会改的状态放在组件的普通字段里——只有 DAO 会回滚（维护者决定 A1）；③ 看到 `ErrLockTimeout` 就当“未执行”重试——先按 [§6.4 判别表](#64-回复错误怎么判断) 看有没有“可能已提交”的哨兵；④ `durability=memory` 加上 `rollback=state|undo` 时持久字段的修改不会落库（见 §7.2）。
- **glsvet**：handler 里开 goroutine、准入结果被丢弃等是**违例**（退出码 1）；A1 组件 undo / 字段写、停机等待、业务时钟是**提示**（`hint:`，不影响退出码）。

## 本篇覆盖的包

| 包 | 本篇覆盖的职责 | 不在本篇的部分 |
| --- | --- | --- |
| `nest` | 引擎 `NestMgr`、`Client`、派发队列（快慢双池）、事务与回滚、Cast、锁组与组迁移、Tick、派发器指标 | Remote 准备与收尾细节归 05；WAL / pipelined 持久侧归 03 |
| `entity`（Guard、锁序、Create / Destroy、Getter 契约） | `EntityGuard` / `GuardScope`、`GetEntityGroup` 锁序、`CreateInScope` 进入事务、`LoadedEntitiesOnly` / `LoadedChecker`、`RunLocal` | Remote 实体与 Mirror（05）、`SubjectSyncState` 与 `SyncMutation` 内容（04）、ID 编码与 kind 注册（12 生成侧） |
| `lock` | 实体可重入锁 `ReentrantMutex`、`LockManager` | — |
| `fctx` | 每 goroutine 的请求上下文、`ContextSnapshot`、快 worker 标记与 `BlockingError` | — |
| `goroutine` | `GoID`、`SafeFunc`、`TaskPool`、`MPSCQueue`、`ParallelMap` | — |
| `worker` | `Pool`（handler 里唯一允许的异步出口、pipelined 完成池的底座） | — |
| `actionflow` | `ActionRunner` / `MissionRunner`（回调里的修改走延后队列）、`PlanMission` | `ai` 包如何驱动它们（只在 §4.12 提一句） |
| `cmd/glsvet` | 全部规则：handler 并发边界、准入结果、goroutine 绑定调用、A1 两种提示、停机提示、业务时钟提示、`roost:nest` 标注 | 业务时钟的语义归 10 |
| `kit/nest` | 把引擎装配成 `app.Mod`、读 `nest.*` 配置、接 Fence 与健康检查 | Mod 生命周期通用规则归 01 |

跨分区的内容只给出入口：DataEngine 与 DAO 本身见 [03 dataengine 说明](03-dataengine.md)；Sync 见 [04 sync 说明](04-sync.md)；Remote 见 [05 remote / Mirror 说明](05-remote-mirror.md)；App 装配与停机契约见 [01 app 说明](01-app-lifecycle.md)（[实现](../impl/01-app-lifecycle.md)）；生成器见 [12 codegen 说明](12-codegen.md)。

---

## 1. 定位与边界

一句话：**Nest 把“谁在什么时候、拿着哪些锁、在哪个事务里改哪个实体”这件事收归框架，业务只写 handler。**

| Nest 负责 | Nest 不负责 |
| --- | --- |
| 按声明目标建立同 ID 顺序、背压（队列满即拒绝） | 网络接入、认证、限流（gateway / 接入层） |
| 快慢两种执行资源：快池跑 handler，慢池跑等待 | 在 handler 里替业务做 I/O：任意阻塞 RPC 不会被自动隔离 |
| 加载、按全局锁序加锁、引用计数（Touch） | 跨服务的一致性（用 Saga，见 [06 saga 说明](06-saga.md)） |
| 事务：回滚、准入、提交、提交后回调、回复 | DAO 的生成与持久化格式、WAL 与投影（03） |
| 结果未知时 fence 引擎，不猜测性回滚 | 恢复已 fence 的进程（由 App fail-stop + WAL 重放完成） |
| 静态检查（glsvet）发现 handler 里的并发越界 | 证明业务代码正确：glsvet 只按语法判断 |

`entity` 在本篇里是 Nest 的“锁与生命周期”一侧：Guard 怎么记账、锁按什么顺序取、handler 里新建 / 销毁的实体怎么进事务。

## 2. 核心概念与术语

| 术语 | 含义 | 源码入口 |
| --- | --- | --- |
| 引擎 `NestMgr` | 一个实例化的 Nest，`NewEngine` 构造，`Start` / `Shutdown` 一次性使用，不能重启 | `nest/nest.go:404`、`:488`、`:515` |
| `Client` | 生成的 Sender 依赖的接口：`Dispatch` / `Request` / `...Multi` / `...MultiGroup` / `DispatchBroadcast` | `nest/client.go:18` |
| handler | `BaseHandler`：参数是已加锁的实体切片与业务参数；由 `//roost:nest` 生成包装并注册 | `nest/handler.go:181` |
| 消息类型 | Single、Multi、MultiGroup、Broadcast、GroupTransition（内部） | `nest/msg.go:314` |
| 声明目标 | 消息上的 `Tid` / `Tids` / `GroupTIds`，即 handler 的实体参数。同 ID 顺序与冷热判断都只看它们 | `nest/dispatch_queue.go:102` |
| 快池 / 慢池 | 两条执行通道（lane 0 / 1）。快池执行全部 handler 与本地锁、回滚；慢池只做准备性等待 | `nest/dispatch_queue.go:56` |
| 快续行 | 慢阶段把“需要本地锁的步骤”交回快池同步执行的通道，不排在任何同 ID 链上 | `nest/dispatch_queue.go:174` |
| 准入 | `TrySendMsg` 把消息放进派发队列的那一步；成功后消息归派发器所有 | `nest/dispatcher.go:239` |
| 冷目标改道 | 快池首跑发现声明目标未加载 / 被驱逐，原位把同一作业交给慢池准备，不丢失同 ID 位置 | `nest/dispatch_queue.go:296` |
| Guard / Guard 作用域 | `EntityGuard` 是一个 goroutine 持有的锁账本；`GuardScope` 是它的唯一所有者，作用域结束时一次性放锁、跑解锁后回调、归还池 | `entity/entity_guard.go:76`、`:110` |
| 锁序 | 实体锁按 `GetEntityGroup`（kind 所属 category 的值）从小到大取，同 category 内按 ID 升序；Remote category 固定最先 | `entity/entity_guard.go:26`、`nest/nest_dispatch.go:604` |
| 锁组 / 组迁移 | 一组实体共用一把组锁串行化；加入、离开、换组走专门的内部消息 | `nest/group_lock.go`、`nest/group_transition.go` |
| Cast | handler 内按锁序动态取得未声明实体的锁，并纳入当前事务 | `nest/cast.go:99` |
| `RollbackTx` | 一次事务：回滚函数、提交参与者、持久化变更、effect、回执 | `nest/rollback.go:63` |
| 回滚策略 | `none` / `state` / `undo` | `nest/rollback.go:16` |
| 持久策略 | `memory` / `async` / `strict` / `pipelined` | `nest/transaction.go:19` |
| 提交点 | `committer.Commit` 返回成功（async 是 WAL 准入，strict 是持久）、pipelined 的记录被 WAL `Enqueue` 接纳、memory 快路径准入的那一刻；之后不再回滚、不再重排 | `nest/msg.go:269`、`nest/execution.go:312`、`:342` |
| fence | 结果未知或 App 报 `RuntimeFailure` 时，引擎立刻拒绝新消息和排队消息，幂等、只记第一个原因 | `nest/nest.go:227` |
| `RunLocal` | 框架后台 / 慢 worker 把需要实体锁的步骤交给快池执行并等它结束 | `nest/remote_dispatch.go:77` |
| 独立事务 | `RunIsolatedTransaction` / `RunDetachedTransaction`：基础设施用的、不认领当前消息的事务 | `nest/execution.go:59`、`:89` |
| fctx | 每 goroutine 一个请求上下文（Meta、Trace、Base ctx、KV），按 GoID 存 | `fctx/context.go:14`、`fctx/runtime_context.go:28` |
| 快 worker 标记 | 执行位置标记，只由派发队列在快 lane 建立，嵌套 Context 继承，快照不携带 | `fctx/worker.go:12` |
| `LoadedEntitiesOnly` | Getter 契约：快 worker 或带此标记的 ctx 只读内存，冷目标返回 `entity.ErrColdLoadInLogic` | `entity/local_executor.go:48` |
| actionflow | 实体内的动作 / 任务状态机：`ActionRunner`、`MissionRunner`；本身无锁，由持实体锁的一方串行调用 | `actionflow/action_runner.go:151`、`actionflow/mission_runner.go:96` |
| 延后队列 | runner 回调期间对 runner 的修改不立即执行，最外层调用返回前按序执行 | `actionflow/mission_runner.go:231` |
| finding / hint | glsvet 的违例（计数、退出码 1）与提示（`hint:`，不计数） | `cmd/glsvet/main.go:96` |

## 3. 设计原因

### 3.1 为什么是快慢双池，快池为什么不能等

- 快池 worker 数接近 CPU 数（默认 `GOMAXPROCS`，`nest/dispatcher.go:94`），它们持有实体锁执行业务。一个快 worker 去等 I/O，它持有的锁和它占的 worker 都被冻住；更糟的是“投递到快池再同步等它”——N 个快 worker 各做一次，整池饥饿死锁，Guard 永不释放。所以执行契约写死：**快池内不得阻塞等待**（[roost-coding 执行契约 Nest 与 Entity](../../agent-skills/roost-coding/SKILL.md)，RR-20260926-06）。
- 慢池（默认 `max(32, 快池×4)` 个 worker，`nest/dispatcher.go:133`）不持实体锁，专门承担冷加载、Remote 分布式锁 / 加载 / 确认 / 释放这类等待；需要本地锁的步骤用快续行交回快池（`nest/remote_dispatch.go:122`）。
- 明确豁免（设计内、不会饥饿）：Guard / 实体本地锁的有序等待；锁内 WAL 准入及 strict 的锁内 fsync；pipelined 阶段一在锁外等 group commit（`<-ticket.Done()`）。新的等待入口要评审（同上，执行契约原文）。

### 3.2 同 ID 顺序在统一准入处建立

派发队列在准入时按消息全部声明目标登记“前驱”，后到的同 ID 消息只占准入预算、不占 worker（`nest/dispatch_queue.go:127`）。所以：

- 同 ID 顺序与走哪个池无关——慢池准备完成后回到快池，仍在原来的位置；
- 等前驱的消息不会把 worker 占住，1024 个 worker 配 16 个等待位也能先吃掉 1024 个突发（`nest/dispatch_queue.go:147` 注释，`TestWorkerBudget1024WorkersDrainWithSmallQueue`）；
- 动态 Cast 的目标不在准入时登记：仍受锁保护，但**不承诺调度 FIFO**。

### 3.3 冷目标走慢池，而不是在 handler 里加载

业务不必知道目标冷热：Getter 实现了 `entity.LoadedChecker`（正式装配的 `entity.ManagerAccess` 实现了）时，准入只做一次内存查找判断冷热，有冷目标就直接走慢阶段（RR-20260926-25 / 47，`nest/dispatcher.go:270`）。准入之后、handler 取锁之前目标又被驱逐时，作业原位转慢池，保留同 ID 位置（`nest/nest_dispatch.go:55`）。**不允许**把执行了一半的 handler 搬去慢池或自动重试：handler 开始后快阶段 Getter 只读内存，冷目标就是 `ErrColdLoadInLogic`，业务可以据此降级（RR-20260926-26）。

### 3.4 Guard 只归作用域所有（RR-20261006-12）

派发取锁只用当前 goroutine 的 Guard 作用域里的 Guard，没有作用域直接报错、不取任何锁（`nest/group_lock.go:247`）。原因：Guard 上挂的不只是声明目标的锁，还有 handler 新建实体的锁、被同 ID 新实例取代的旧实例、解锁后回调、撤销新建的收尾，只有完整的作用域释放能收尾；之前“无作用域时从池里取独立 Guard”的分支会在组迁移重试时把同一个 Guard 两次放回池，两个快 worker 共用它、互相解对方的锁（[修复记录](../../bugfix/RR-20261006-12.md)）。

### 3.5 锁序由 category 的值决定，Remote 固定最先

`entity.GetEntityGroup` 把 ID 里的 kind 映射到它注册的 category，category 的**值**就是锁序（`entity/category_order.go:22`、`:37`）。Remote category 固定为 1：Remote 实体的分布式所有权锁在派发开头取，若在持有本地实体锁时再去取，会让本地锁跨一次网络往返（`entity/category_order.go:10` 注释）。推荐的顺序是 Remote → World → PlayerScoped → Player → Other；本进程不认识的 kind 排最后（255）。

### 3.6 回滚统一走 DAO（维护者决定 A1）

事务里会改、回滚时要恢复的状态一律放在 DAO（不落库用 `dao:"nopersist,sync"` 或 `dao:"nopersist,nosync"`），由 Nest 的 DAO 回滚统一兜住；组件不维护需要回滚的内存状态，不在组件里调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`。同一缺口此前出过四次（NC-61 / 65 / 140、N09 O1），每次都是组件自己补一份快照、漏一处就是资产错（[方案](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）。派生值同样是 DAO 字段，由组件里唯一的 derive 在加载与改源字段的事务里写；**回滚不是重算触发点**。明确例外：`skill.Runtime` 的冷却、ammo 等不进事务（维护者决定 B4，见 [08 skill 说明](08-skill.md)）。

### 3.7 越过提交点不重排；不能回滚的 handler 开始后也不重排

锁超时 / 组迁移这类暂时性错误会让框架自动重新准入（最多 400 次）。但：

- 消息自己的事务已越过提交点、handler 内嵌套的独立事务已提交或结果未知，重排会让已提交内容再执行一次——不重排（RR-20260926-49 / 65，`nest/msg.go:269`）；
- `rollback=none` 的 handler 一旦开始执行，失败前的内存修改不撤销，重排会让它们重复生效——不重排，回复补 `ErrNonRollbackNotRequeued`（RR-20260926-73）。

### 3.8 结果未知时 fence，不回滚

committer 报 `ErrCommitIndeterminate`（字节可能已到盘）时，回滚内存会制造第二条冲突历史。所以事务被 abandon（不跑回滚、不跑提交后回调），引擎立即 fence（`nest/execution.go:45`、`nest/rollback.go:510`）。Nest 自己只 fence、不让进程退出；正式装配里 kit DataEngine 遇到致命存储结果时同时 fence 引擎并上报 `RuntimeFailure`（`kit/dataengine/mod.go:460`），App fail-stop 后由 WAL 重放决定历史（01 / 03）。

### 3.9 actionflow 回调里的修改延后（维护者决定 B7、第五轮）

动作 / 任务回调运行期间对 runner 的修改（Start、End、SetMission……）进延后队列，最外层调用返回前按发起顺序执行；任一回调运行期间当前动作 / 任务都不会被换掉。之前靠回调点之后“事后比对 + `ErrReentrantMutation`”，分支多、漏比一处就出错；延后队列把判定收进 `submit` 一处（`actionflow/action_runner.go:362`、`actionflow/mission_runner.go:231`，[方案](../../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md)）。

### 3.10 glsvet：并发越界是违例，其余只提示

handler 开 goroutine、准入结果被丢弃会直接破坏正确性，算违例。A1、停机等待（A3 ③）、业务时钟（D-L3）的规则没有类型信息就分不清对错（例如停机函数里的 `Mutex.Lock` 实测几乎全是短临界区，`cmd/glsvet/stophints.go:15` 注释），只打印 `hint:` 给复审看，不改退出码。

### 3.11 派发器指标随派发器撤销（RR-20261006-18）

带 `dispatcher` 标签的 `nest.dispatch.*` 序列按名字计数持有：最后一个同名派发器排空停机后删除序列；停机超时（worker 仍在跑）不删，重试排空后再删（`nest/dispatcher_series.go:9` 注释）。

[→ 实现文档对应章节：3 主流程、9 历史](../impl/02-nest-entity.md#3-主流程)

---

## 4. 怎么用

### 4.1 最小可运行示例

仓库里最小的、能直接跑的例子是 `nest/nest_test.go:1556` 的 `TestInstanceScopedHandlersDoNotCollideAcrossEngines`：构造引擎、实例级注册 handler、`Start`、`Request`、`Shutdown`。骨架（摘要）：

```go
engine := nest.NewEngine(
    nest.NestOptionWithGetter(getter),               // 正式装配是 *entity.ManagerAccess
    nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}),
)
engine.MustRegisterHandlerWithMeta(name, handler, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
_ = engine.Start()                                    // Start 之后不能再实例注册
ret, err := engine.Request(ctx, name, entityID, nil)  // 同步：等 handler 结束（含提交）
_ = engine.Shutdown(ctx)                              // 停准入、排空、停 ticker
```

生产工程不手写这些：

1. 业务写带 `//roost:nest` 的 handler，例：`demo/game/handler/add_exp.go.tmpl`（Player 与 World 两实体一笔 strict 事务）、`demo/game/handler/join_guild.go.tmpl`（含 Remote 实体）。
2. `roost generate` 生成 `*_nest_gen.go`（invoke 包装 + `MustRegisterHandlerWithMeta`，`codegen/internal/nest/gen.go:820`）和注入式 Sender（`Send_*` 用 `client.Dispatch`、`Sync_*` 用 `client.Request`，`codegen/internal/nest/gen.go:640`、`:660`）。
3. kit 的 `nest.Mod` 在 Provide 时构造引擎，把 `*nest.NestMgr` 以 `mods.ModNest` 登记进 Registry（`kit/nest/nest_mod.go:206`、`:208`）；接入层取出 `nest.Client` 注入 Sender。

### 4.2 写 handler：`//roost:nest`

```go
//roost:nest rollback=undo durability=strict
func handlerAddExp(target player.IProfileEntity, stats world.IStatsEntity, amount int64) (int32, error) {
    gained, err := target.ProfileComp().AddExp(amount)   // 组件只读写 DAO
    if err != nil { return 0, err }                      // 返回错误 => 整笔回滚
    stats.StatsComp().RecordExp(amount)
    return gained, nil
}
```

| 标注选项 | 取值 | 生成的 `HandlerMeta` | 说明 |
| --- | --- | --- | --- |
| （缺省） | — | `{RollbackState, DurabilityAsync}` | `codegen/internal/nest/gen.go:202` |
| `rollback=` | `state` / `undo` | `RollbackState` / `RollbackUndo` | `none` 不能写，只有 `durability=memory` 且不写 rollback 时得到 `RollbackNone` |
| `durability=` | `memory` / `async` / `strict` | 对应策略 | 生成器**不接受** `pipelined`（`codegen/internal/nest/parse.go:177`），pipelined 只能手写 `RegisterHandlerWithMeta` 注册 |
| `target=` / `targets=` | 逗号分隔、不含空格 | — | 前 N 个参数是实体 capability；见 `codegen/docs/NEST_RUNTIME.zh-CN.md` |
| `sync` | 无值 | — | 没有返回值的 handler 也生成同步 Sender |

规则：

- 会修改的每个实体都必须出现在实体参数里；实体参数在普通参数之前；切片参数生成 MultiGroup。
- `durability` 非 memory 时 rollback 不能是 none：注册即失败（`nest/handler.go` `validateHandlerMeta`）。
- 手写 `HandlerMeta` 时 `Rollback` 不为 none 就必须显式写 `Durability`（`DurabilityMemory` / `DurabilityAsync` / `DurabilityStrict` / `DurabilityPipelined`），没写的注册即失败，错误满足 `errors.Is(err, nest.ErrDurabilityUnset)` 并点名 handler（RR-20261006-60，v1.23.1 起；之前零值即 memory，要到运行期第一次改持久字段才被拒）。`HandlerMeta{}` 仍是 rollback=none + memory；生成的 meta 总是写明 durability，不受影响。
- 包级函数注册进全局表；引擎构造时快照全局表，查找先实例、后全局（`nest/handler.go:109`）。测试和多引擎进程优先用实例级 `mgr.RegisterHandlerWithMeta`（只能在 `Start` 之前，`nest/handler.go:139`）。
- 注册的 handler 经 hotcode 解析（`nest/handler.go:98`），热更替换的是函数，不是 `HandlerMeta`。

### 4.3 发送：`Client` 的七个方法

| 方法 | 消息类型 | 等结果 | 在 handler 内调用 |
| --- | --- | --- | --- |
| `Dispatch` | Single | 否：准入成功即返回 | `ErrAsyncInHandler` |
| `Request` | Single | 是 | `ErrSyncInHandler` |
| `DispatchMulti` / `RequestMulti` | Multi（一笔事务、多个目标） | 否 / 是 | 同上 |
| `DispatchMultiGroup` / `RequestMultiGroup` | MultiGroup（分组的多目标） | 否 / 是 | 同上 |
| `DispatchBroadcast` | Broadcast（每个目标一笔独立事务） | 否 | `ErrAsyncInHandler` |

（`nest/client.go:31`～`:170`，拒绝在 `nest/client.go:191`。）

- **异步准入成功之后**，参数归 Nest 所有，调用方不能再改；异步消息只带框架信封（配置代、请求身份、trace），不带调用方 ctx 的值、KV 与事务状态，业务数据一律放 Params（`nest/client.go:211`、`nest/nest.go:686`）。
- **同步请求**带完整快照，handler 看到的 `Base` 就是调用方 ctx：ctx 已取消的消息在 handler 前被拒（`nest/nest_dispatch.go:153`）。等待上限是请求的 `SyncWait`，否则引擎的 `SyncTimeout`（kit 的 `nest.request_timeout`，缺省 5s，`nest/client.go:255`）。**等待先结束只说明没等到回复，不说明结果**（见 §6.4 第 14 行）。
- `DispatchBroadcast` 按目标分批独立准入：返回错误时之前已准入的批次仍会执行；需要全有或全无用 `DispatchMulti`（`nest/client.go:28`）。带 Remote 目标的广播被拒绝（`ErrRemoteBroadcastUnsupported`）。
- 发送选项：`SendOptionWithDelay(d)` 进延迟堆（上限 `nest.max_delay`，缺省 24h，超出 `ErrDelayTooLong`）；`SendOptionSlow()` 强制慢准备。
- 准入失败的错误：`ErrQueueFull`（预算满）、`ErrNestStopped`（未启动 / 已停）、`ErrNestFenced`（已 fence，带原因），都在返回前完成，消息不会执行。

### 4.4 handler 里能做什么、不能做什么

| 场景 | 做法 | 依据 |
| --- | --- | --- |
| 读写声明目标 | 直接调组件，组件只读写 DAO | A1 |
| 需要另一个实体 | `nest.CastOne` / `CastTwo` / `CastThree` / `CastMulti`；只能取锁序更高的实体 | §4.5 |
| 新建实体 | 在 handler 内调用 `Create` / 生成的 `Lifecycle.Create`、`GetOrCreate` | §4.6 |
| 对外发消息、起 Saga | `nest.Emit(effect)` / `saga.EmitStart`：随事务进 outbox，提交后投递 | 03 / 06 |
| 提交后做点事 | `nest.AfterCommit(fn)`：提交成功后执行（解锁后，或 Remote 确认后） | `nest/rollback.go:806` |
| 再发 Nest 消息 | **不行**：`Request` 返回 `ErrSyncInHandler`，`Dispatch` 返回 `ErrAsyncInHandler`。用 effect、或在接入层编排 | `nest/client.go:191` |
| 开 goroutine | **不行**：glsvet 违例。确需异步用 `roost-core/worker` 的 `Pool.Go/TryGo`，把业务数据拷进任务参数，不捕获外层变量 | §4.14 |
| 阻塞 I/O、跨服务 RPC | **不行**：放到锁外（Saga、outbox、接入层） | §3.1 |
| 冷加载实体 | **不行**：快阶段 Getter 只读内存，冷目标返回 `ErrColdLoadInLogic`；需要时把它声明成目标、走慢准备 | §4.7 |
| 自己登记 undo | **不行**（组件里）：DAO 方法自己登记是允许的，生成 setter 就是这样 | §4.9 |
| 把实体、DAO、可变 map/slice 带出锁 | **不行** | [USER_GUIDE §3](../../USER_GUIDE.md#3-entitycomponent-与-dao) |

### 4.5 多实体、锁序与 Cast

- Multi / MultiGroup 的全部目标在 handler 前排好序再加锁（`nest.SortEntity`，`nest/nest_dispatch.go:594`）：先按锁序（category 值）、再按 ID。业务不需要、也不应该自己加锁。
- `CastMulti` 规则（`nest/cast.go:99`）：
  - 必须在 Nest 派发的 handler 里（否则 `ErrCastNoContext`）；
  - 每个目标要么已被本 Guard 持有，要么 category 严格高于本 Guard 已持有的全部 category，否则 `ErrCastDeadlockRisk`——**同 category 的实体不能 Cast**，要声明成目标；
  - 未声明的 Remote 托管实体一律拒绝（`entity.ErrRemoteWriteCapabilityDisabled`，“must be declared before dispatch”），Remote 目标必须在派发前声明；
  - 等锁期间目标被 `Destroy` 或仅内存卸载，返回满足 `ErrEntityNotFound` 的错误（不是锁超时，RR-20260926-73）；
  - Cast 到的实体自动进当前事务（回滚、持久化参与）与本次 Sync 提交屏障；事务捕获失败时即使业务吞掉错误，整笔也回滚（RR-20260927-31）。
- `ReleaseCast(e)` 只在无事务、无 Sync 屏障的上下文里提前放锁；事务内是空操作，锁随作用域释放（`nest/cast.go:204`）。
- 一个 handler 不跨 EntityManager 持有同 ID 的实体（契约，REMAINING §3 N27，`entity/entity_guard.go:84` 注释）。

### 4.6 handler 内新建、销毁实体

- handler 内的 `EntityManager.Create`（`IsCreate`）自动走当前作用域的 `CreateInScope`，新实体**进入当前事务**：纳入回滚 / 持久化参与者与 Sync 提交屏障，提交确认前不外发；handler 失败、panic 或提交被明确拒绝时撤销发布（`entity/manager_factory.go:20`、`:73`，RR-20260926-35）。
- 新实体的锁持有到 handler 结束，取锁遵循 Cast 的锁序：锁序允许时等待，否则只尝试加锁（`entity/entity_guard.go:386`）。被别人占用时：
  - 可回滚事务得到同时满足 `ErrCreatedEntityLockConflict` 与 `ErrLockTimeout` 的错误，handler 结束时**即使业务吞掉了它**也整笔回滚并自动重排（RR-20260926-48）；
  - 不能回滚的 handler 得到只有 `ErrCreatedEntityLockConflict` 的错误，消息不重排（RR-20260926-64）。
- 两条消息交叉新建（一条先建 X 再建 Y，另一条反过来）会互相冲突、一起回滚；重排带随机抖动打破对称（U-0279，T-220）。从源头避免：多个新建按 ID 升序。
- 同 ID 的上一个实例正在撤销 / 销毁收尾时，handler 内新建按锁冲突处理（RR-20260926-81）；但若是**本 handler 自己**较早撤销的同 ID，在同一 handler 里再建是确定失败（`entity.ErrEntityRemoved`，不重排，RR-20260927-21）。
- 销毁用 `ManagerAccess.Destroy`：持实体锁做删除准入，确定失败时实体保持存活，结果未知时防御性移除（`entity/entity_manager.go:192` 注释）。`Destroy` 本身不是事务操作，事务回滚不会让被销毁的旧实例复活。
- 实体实现必须是指针（RR-20260930-15）：值类型在 `BuildEntity` / `TryAdd` 处被拒绝（`entity/entity_guard.go:325`）。

### 4.7 冷目标与 Slow

| Getter | 准入时 | 准入后被驱逐 | handler 内访问冷实体 |
| --- | --- | --- | --- |
| 实现了 `entity.LoadedChecker`（正式的 `ManagerAccess`） | 有冷声明目标即走慢阶段（自动） | 原位转慢池，保留同 ID 位置，指标 `nest.dispatch.slow_reroute.total` | `ErrColdLoadInLogic`（快 worker 上另包 `fctx.ErrBlockingInFastWorker`） |
| 没实现 | 不判冷 | 冷目标按原错误返回 | 同上；需要预加载时显式 `SendOptionSlow()` |

- 广播不批量冷加载，冷目标逐个记录日志并继续（`nest/slow_load.go:118` 注释）。
- 自定义 Getter 必须遵守 `LoadedEntitiesOnly`：快 worker 上或 ctx 带标记时只读内存、冷目标返回 `ErrColdLoadInLogic`，不能做 I/O、不能等在途加载、不能 panic（`entity/entity.go:47` 注释）。
- 多个请求同时冷加载同一实体时共用一次加载；领头请求自己的截止只让它离开，不取消加载，加载受 `nest.entity_load_timeout` 约束（RR-20260926-54，`entity/manager_access.go:265` 注释）。

### 4.8 锁组与组迁移

锁组让一组实体（例如一个场景里的单位）共用一把组锁串行化，handler 里可以经 `nest.CurrentEntityLockGroup()` 的 `Get` / `Range` / `GroupEntityAs` 访问同组实体（`nest/group_lock.go:33`～`:85`）。

- 派发时声明目标属于同一组：先 try 组锁，再 try 各实体锁；任一被占用返回 `ErrLockTimeout`，消息自动重排（`nest/group_lock.go:289`）。目标跨两个组：`ErrEntityLockGroupMix`，不重排。
- 加入 / 离开 / 换组：`mgr.RequestJoinEntityLockGroup` / `RequestLeaveEntityLockGroup` / `RequestMoveEntityLockGroup`。实体先被标记为“迁移待定”，普通派发遇到它返回 `ErrEntityGroupTransitionPending` 并重排；迁移消息 try 涉及的组锁与实体锁，忙则 5ms 后重试，最多 400 次或 2s，超时清除待定标记（`nest/group_transition.go:17`～`:24`、`:201`）。
- 在同步请求的 handler 里发起迁移并带 `GroupTransitionWithContinuation(handler, params)`：迁移完成后以续接 handler 回复原请求；发起的 handler 返回 `ErrEntityGroupTransitionScheduled`（被当成“已交接”，不回复、不重排）。用例见 `nest/group_transition_test.go:129`。
- 取锁期间组成员关系或组纪元变了，放掉已取的锁最多重试 4 次（`nest/group_lock.go:12`、`:258`）。
- 仓库内（kit、demo 模板）目前没有组迁移的生产调用方，只有 nest 的测试。

### 4.9 事务：回滚策略、持久策略与 A1 写法

**回滚策略**

| 策略 | 怎么回滚 | 适用 |
| --- | --- | --- |
| `undo` | 生成 DAO 的 setter 在事务里登记字段级逆操作（每字段 / 每 map key 每笔事务一次），回滚时逆序执行，并恢复 DAO 的 dirty tracker | 改动少的高频请求，热路径推荐 |
| `state` | 事务开始（含 Cast、新建实体被纳入时）对每个 DAO 拍快照（`RollbackSnapshotter`，生成 DAO 都实现），回滚时整体恢复 | 修改范围复杂 |
| `none` | 不回滚；只允许和 `memory` 搭配（memory 快路径） | 可重建的临时状态 |

（`nest/rollback.go:980` `captureDao`；`state` 下 DAO 既不实现 `RollbackSnapshotter` 也不实现 `RollbackParticipant` 时事务直接失败 `ErrRollbackUnsupported`。）

**持久策略**

| 策略 | 提交点 | 回复时机 | 说明 |
| --- | --- | --- | --- |
| `memory` | 内存提交 | handler 结束 | 没有 effect 时不产生提交记录；emit 了 effect 自动升为 strict（`nest/rollback.go:355`）；删除意图也升为 strict（`nest/rollback.go:247`） |
| `async`（缺省） | committer 接纳记录（正式 WAL：只等写入，由组提交周期刷盘，`nestwal/wal.go:317`） | 接纳后 | 后台批量投影到权威存储；async / strict 在 WAL 侧的确切含义见 03 |
| `strict` | committer 持久接纳 | 持久后 | 支付、跨服资产、唯一奖励 |
| `pipelined` | 锁内 Enqueue 被 WAL 接纳 | ticket 持久后 | 锁在 fsync 前释放；必须在 `nest.pipelined.allowlist` 里（为空时开发环境全放行）；broadcast 与带 Remote 批次的消息按 strict 在锁内提交；详见 [NEST_PIPELINED_COMMIT.md](../../../NEST_PIPELINED_COMMIT.md) |

**A1：事务会改的状态放 DAO**

- 生成的 DAO 字段标注：`persist,sync`（落库且同步）、`nopersist,sync`（只同步，派生值常用）、`nopersist,nosync`（只参与事务回滚）。`nopersist,nosync` 字段同样有 setter，`undo` 下登记逆操作、`state` 下在快照里（[A1 方案 §2](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）。
- 例：`demo/db/def/player.go.tmpl` 的 `AttrFinal`（`nopersist,sync`）与 `AttrGear`（`nopersist,nosync`）；组件 `demo/game/entities/player/attribute_component.go.tmpl` 每次从 DAO 构造属性容器，不持有状态。
- 手写 DAO 的方法可以自己登记逆操作（与生成 setter 同形），例：`skill/combatcomponent` 的 `CombatDao.beginChange`。
- 确属缓存、可从 DAO 重建、允许不随事务回滚的组件字段：在声明上一行或行尾写 `//roost:cache`（glsvet 据此不提示）。读它的代码要能容忍它与 DAO 不一致。
- 持久字段的 setter 只能在事务里调用：事务外调用 `MarkPersist` 返回 `ErrTransactionClosed`，生成 setter 据此 panic（fail fast，`nest/persist_change.go:56`）。memory 快路径（`rollback=none`）没有事务，所以 memory handler 写持久字段会 panic；但 `durability=memory` 搭配 `rollback=state|undo` 时有事务、setter 不报错、修改却不进任何提交记录（§7.2）。（v1.23.1 起运行期强制，见 [RR-20261006-41](../../bug/RR-20261006-41.md)：memory 事务改了持久字段时整笔失败回滚，错误 `ErrMemoryTransactionPersistentWrite` 点名实体与字段。）

**其他事务 API**：`nest.Emit(effect)`、`nest.AfterCommit(fn)`、`tx.AfterAdmission(fn)`（准入后、解锁前的生命周期变更，外部副作用放 AfterCommit）、`nest.AddReceipt` / `SetReceiptPayload`（幂等回执）、`nest.CurrentRollbackTx()`（框架与 DAO 用）。

### 4.10 基础设施入口：RunLocal 与独立事务

| 入口 | 用途 | 规则 |
| --- | --- | --- |
| `mgr.RunLocal(ctx, fn)` | 框架后台 / 慢 worker 把需要实体锁的步骤交给快池执行并等待（DataEngine 驱逐、Remote finalizer、共享冷加载的发布） | 在快 worker 上调用返回 `fctx.ErrBlockingInFastWorker`；引擎未启动 / 停机 / fence 时 fn 不执行；投递成功后一定等 fn 结束，不因 ctx 提前返回（`nest/remote_dispatch.go:69` 注释）。`NewEngine` 自动绑定给 committer、Remote 管理器与 Getter（`nest/nest.go:471`～`:482`） |
| `entity.RunLocal(ctx, fn)` | 实体 / Remote 代码里“需要本地锁的步骤” | 已在快 worker 上就地执行；ctx 带慢阶段注入的执行器时交给它；否则就地执行（`entity/local_executor.go:24`） |
| `nest.RunIsolatedTransaction(ctx, committer, name, fn)` | 基础设施生命周期操作，提交点不能随外层业务回滚 | 总是新建一笔 strict 事务，**从不认领当前消息**；要写外层可回滚事务已快照的实体时被拒（`ErrNestedTransactionRollbackConflict`）；所在消息带 Remote 批次时被拒（`ErrNestedTransactionInRemoteMessage`）；提交后外层消息不再重排，回复带 `ErrNestedTransactionCommitted`；结果未知时先 fence 引擎（`nest/execution.go:74` 注释） |
| `nest.RunDetachedTransaction(...)` | 不在实体锁里的适配器需要事务边界 | 已有 `RollbackTx` 时直接复用；否则同上新建，**不取实体锁**（调用方负责） |

**不要吞掉独立事务的错误**：结果未知被吞掉、外层又没有要持久的记录时，回复是成功，结果未知只能从之后请求得到的 `ErrNestFenced` 知道（REMAINING §3 N22，维护者决定不加哨兵）。

### 4.11 Tick 回调

`nest.RegisterTickCallback(name, fn)` 注册的回调在引擎的 ticker goroutine 上按注册顺序执行，每个回调各自 recover（`nest/ticker.go:168`）。**它不在快池、没有 Guard、没有事务**：回调里要改实体，经 `Client.Dispatch` 发消息。帧号 `TickMsg.FrameNumber` 写进 fctx 的 `Frame`（`nest/nest_dispatch.go:240`）。间隔由 `nest.tick_duration` 配置，缺省 100ms。

### 4.12 actionflow：ActionRunner 与 MissionRunner

- 用途：实体内的动作（移动、施法……按组互斥、可排队）与任务（多步计划，`PlanMission`）。`ai` 包的行为树经 `actionflow.ActionList` 驱动它们。
- **runner 本身无锁**，必须由持实体锁的一方串行调用：在 handler 里，或在经 Nest 派发的 tick 消息里（`actionflow/action_runner.go:132`）。
- **回调里的修改延后执行**：动作的 `Start` / `Tick` / `Cancel`、任务的 `Start` / `Tick` / `OnActionEnd` / `End` / `CanReplaceBy`、构建器与全部钩子都在“执行中”；这期间对本 runner 的修改：
  - `ActionRunner.Start` / `Enqueue` 立即构建并返回已分配的 ID 和 nil，动作在回调返回后才启动；拿到 ID 的动作一定有结论（延后执行时已无法启动也发一次失败 / 取消的 `OnEnded`）；
  - `MissionRunner.StartMission` / `CancelMission` 返回 nil，执行时的错误经 `OnError` 报告（`taskflow: deferred start mission: ...`）；
  - 回调里读到的是尚未执行延后命令的状态（`Deferring()` 为真表示正被延后）；
  - 延后命令按“执行时的当前状态”解释：回调里先 StartMission 再 EndCurMission，结束的是新任务。
- **有界**：一次最外层调用里待执行的延后命令最多 `MaxDeferredCommands`（缺省 64，超出 `ErrDeferredQueueFull`），总共执行最多 `MaxDeferredSteps`（缺省 1024，超出 `ErrDeferredRunaway`，剩余丢弃；已交出 ID 的动作各收到一次取消）（`actionflow/action_runner.go:29`）。
- 全部回调 panic 都恢复成错误；`Update(fn)` 的 fn panic 同样恢复（RR-20261005-NC-242）。
- **清场顺序**：`EndAll` 期间回调发起的动作会在它返回后启动，它不是清场。要什么都不再运行：先 `EndCurMission`，再 `EndAllAction`（`actionflow/action_types.go:10` 包注释，`TestEndCurMissionBeforeEndAllLeavesNothingRunning`）。
- 时间：`ActionContext.Now` / `MissionContext.Now` 缺省读 `clock.Now()`（业务时钟，D-L3，`actionflow/action_runner.go:698`）。
- `Registry` 是实例级的，启动后可 `Seal()` 禁止再注册（`actionflow/registry.go:22`）。

### 4.13 fctx 与 goroutine

- 每个 goroutine 一个 `fctx.Context`，按 GoID 分 64 片存（`fctx/runtime_context.go:8`）。Nest 在派发时为消息建立上下文（Meta.Source=`nest`、Handler、Frame），handler 内 `fctx.CurrentContext()` 可读请求身份与 trace。
- **goroutine 绑定**：`fctx.CurrentContext`、`entity.CurrentGuardScope`、`entity.GetEntityGuard`、`nest.CurrentRollbackTx`、`RecordUndo` 都按当前 GoID 查找；在你自己开的 goroutine 里它们返回 nil / false，修改会逃出回滚（glsvet 对 `go func(){...}` 里调用它们报违例，§4.14）。
- 快照：`fctx.CaptureSnapshot()` 只用于框架受控的同步交接，不是业务的异步 API；快 worker 标记不进快照（`fctx/context.go:27`、`fctx/worker.go:10`）。
- `goroutine.SafeFunc` / `SafeFuncWithRet`：recover 并记日志；`goroutine.TaskPool`：按任务 ID 哈希的固定 worker 池，一次性使用；`goroutine.ParallelMap` / `ParallelSlice`：有界并行；`goroutine.MPSCQueue`：有界无锁多生产者单消费者队列。框架的 goroutine 都经这些或 `worker` 产生并有上界（T-07）。
- handler 里需要异步时只能用 `roost-core/worker` 的 `Pool.Go` / `Pool.TryGo`（`worker/pool.go:171`、`:179`），且闭包不能捕获 handler 的外层变量（glsvet 违例），业务数据拷进任务参数。

### 4.14 glsvet：跑法与全部规则

跑法：`go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...`（生成工程 `make glsvet`，也在 `make ci` 里；core 的 CI 见 `.github/workflows/ci.yml:33`）。开关：`-tests` 包含 `_test.go`；`-stophints=false` 关停机提示；`-clockhints=false` 关时钟提示；`-businessdirs` 指定业务目录名（缺省 `game`）。退出码：0 干净；1 有违例；2 有参数没能检查（目录不存在、文件解析失败，RR-20261005-NC-204，T-254）。`./...` 跳过以 `.` 开头的目录、`testdata`、`vendor`。

| 规则 | 级别 | 触发 | 怎么改 |
| --- | --- | --- | --- |
| handler 内裸 goroutine | 违例 | Nest handler（见下行）里，或它调用的**同文件**包级函数里有 `go` 语句 | 用 `nest.Emit(effect)` 或 `worker.Pool.Go` |
| handler 内异步包装 | 违例 | 上述范围内 `x.Go(...)` / `x.TryGo(...)`，而 x 不是 `roost-core/worker` 的 Pool 变量 / 字段（errgroup 等） | 换成 `worker.Pool` |
| worker 闭包捕获外层 | 违例 | 允许的 `Pool.Go` 闭包引用了 handler 的参数 / 局部变量 | 把数据放进任务，用回调参数 |
| 什么算 Nest handler | — | 包级函数（无接收者），名字以 `handler` 开头，**或**文档注释里含 `roost:nest`（`cmd/glsvet/main.go:549`）。方法 handler（`func (h *X) handlerY`）不检查（§7.2） | — |
| 准入结果被丢弃 | 违例 | `Dispatch` / `TryDispatch` / `Publish` / `PublishRequest` / `Submit` / `TrySubmit` / `TryGo` 作为语句调用、结果被忽略，或只赋给 `_`（接收者是框架类型、或同包同名方法有返回值时） | 显式处理成功 / 失败 |
| goroutine 绑定调用 | 违例 | 任意 `go func(){...}()` 字面量里调用 `RecordUndo` / `RecordUndoToken` / `CurrentRollbackTx` / `CurrentContext` / `CurrentGuardScope` / `GetEntityGuard` | 不要在新 goroutine 里碰这些 |
| A1：组件自登记 undo | 提示 | 组件方法里直接调用 `RecordUndo` / `RecordUndoToken` / `DeferRollback`，或调用同包里直接登记 undo 的包级 helper（跟一层，RR-20261006-13） | 状态移进 DAO |
| A1：组件字段写 | 提示 | 组件方法（`OnInitFinish` / `OnDestroy` 除外）给组件自身字段赋值、`op=`、`++/--`、`for ... = range`、改字段里的 map / slice 元素、`delete` / `clear`；或经同包 helper 写（跟一层）。字段是 DAO 句柄（类型名以 `Dao` / `DAO` 结尾）、函数类型、或标了 `//roost:cache` 时不提示 | 状态移进 DAO，或确属缓存时标 `//roost:cache` |
| 什么算组件 | — | 本包里名字以 `Component` 结尾的类型，或匿名嵌入 `ComponentBase` 的结构体 | — |
| 停机等待（A3） | 提示 | 名字以 stop / close / shutdown / deregister / drain 开头、带 `context.Context` 参数的函数里，不在“有 `<-ctx.Done()` 或 default 的 select”里的通道接收；或调用同包里不带 ctx、自己做裸接收的函数（跟一层，`release*` 除外） | 确认通道在预算内关闭，或 select ctx / `operation.Lifetime.Wait` |
| 业务时钟（D-L3） | 提示 | 业务目录（路径里有 `game` 段）下直接读 `time.Now` / `Since` / `Until` | 用 `app.BusinessClock`；确是系统时间写 `//glsvet:system-clock <理由>`（同一行、上一行或函数文档） |

只按语法判断，没有类型信息：先取到局部变量再改字段（`m := c.items; m[k] = v`）、经方法调用改（`c.items.Add(x)`）、嵌入类型提升的字段、跨两层以上的 helper 都看不见（[A1 字段写提示记录 §2](../../feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)）。提示是复审线索，不是证明。

[→ 实现文档对应章节：2 关键类型、3 主流程](../impl/02-nest-entity.md#2-关键类型与数据结构)

---

## 5. 配置

`kit/nest` 的 `nest.*` 键（`kit/nest/nest_mod.go:95`，A4 声明式读取，负数在加载时被拒）。写 0 或不写取框架缺省。

| 键 | 缺省（不写 / 0） | 作用 |
| --- | --- | --- |
| `nest.fast.workers` | `GOMAXPROCS` | 快池 worker 数（`nest/nest.go:450`、`nest/dispatcher.go:94`） |
| `nest.fast.queue_capacity` | 10000 | 快池等待预算（整池，不按 worker 倍增）；同时作为延迟堆容量的缺省（`nest/nest.go:453`） |
| `nest.slow.workers` | `max(32, 快池×4)` | 慢池 worker 数（`nest/dispatcher.go:129`） |
| `nest.slow.queue_capacity` | 64 | 慢池等待预算 |
| `nest.heartbeat_worker_num` | — | **无效**：会被读取并传进引擎，但派发器不再创建心跳池（`nest/dispatcher.go:87` 不使用这个参数，§7.2） |
| `nest.delayed_capacity` | 等于快池等待预算 | 延迟消息（`SendOptionWithDelay` 与暂时性错误重排）的容量，满了 `ErrQueueFull` |
| `nest.max_delay` | 24h | 单条延迟上限，超出 `ErrDelayTooLong` |
| `nest.tick_duration` | 100ms | ticker 间隔 |
| `nest.request_timeout` | 5s | 同步请求的缺省等待上限 |
| `nest.entity_load_timeout` | entity 缺省 | 一次共享冷加载的框架上限；调用方自己的截止不结束它 |
| `nest.unload_resync.*` | Workers 4、Attempts 5、QueueCapacity 4096 | 仅内存卸载后仍有订阅者时的重载（Sync 语义见 04） |
| `sync.entity.*` | — | 只在 `NewModWithEntitySync` 装配时读（04） |

`nest.pipelined.*` 由 `kit/dataengine` 读并转成引擎选项（`kit/dataengine/mod.go:151`、`:382`）：

| 键 | 缺省 | 作用 |
| --- | --- | --- |
| `nest.pipelined.allowlist` | 空 = 全部放行 | 允许 pipelined 的 handler 名单；不在名单的 pipelined handler 返回 `ErrPipelinedNotAllowed`。**生产应固定名单** |
| `nest.pipelined.async` | false | 阶段二：完成（AfterCommit 与回复）交给完成池，worker 不等 ticket |
| `nest.pipelined.async_workers` / `async_queue_capacity` | 4 / 8192 | 完成池大小；队列满时单笔降级为 worker 内等待 |

只能用代码选项（`kitnest.NewMod(getter, opts...)` 透传）设置的：`NestOptionWithStageMetrics(true)`（按 handler / 阶段的耗时直方图，缺省关）、`NestOptionWithSlowLockThreshold(d)`（锁持有告警阈值，缺省 100ms，0 关告警但照样记指标）。

调参要点（[roost-coding 执行契约](../../agent-skills/roost-coding/SKILL.md)）：worker 数、等待容量与 Remote / 数据库写预算分开配；**不能推断慢 worker 越多、队列越短就越快**。

[→ 实现文档：5 并发（预算与容量）](../impl/02-nest-entity.md#5-并发)

---

## 6. 运行与运维

### 6.1 指标

| 指标 | 类型 / 标签 | 含义 |
| --- | --- | --- |
| `nest.dispatch.total` / `nest.dispatch.cost` | 计数 / 时长；`handler,type,result` | 每条消息一次（含失败） |
| `nest.dispatch.remote.total` | 计数 | 带 Remote 目标的消息 |
| `nest.dispatch.queue_len` / `nest.dispatch.worker_num` | gauge；`dispatcher,pool=fast\|slow` | 等待数（不含已预留执行额度的就绪消息）、worker 数；每 1024 次准入采样一次 |
| `nest.dispatch.fast_continuations` / `nest.dispatch.delayed_messages` | gauge；`dispatcher` | 排队的快续行、延迟堆里的消息 |
| `nest.dispatch.slow_reroute.total` | 计数；`dispatcher` | 冷目标原位改道 |
| `nest.dispatch.requeue.total` | 计数；`reason=lock_timeout\|group_changed\|group_transition_pending,type` | 暂时性错误自动重排 |
| `nest.entity_group.transition.total` | 计数；`state,result` | 组迁移的请求 / 重试 / 成功 / 超时 |
| `nest.handler.lock_hold` / `nest.handler.lock_hold.slow.total` | 时长 / 计数；`handler` | 实体锁持有时长；超阈值计数并打 Warn |
| `nest.stage.duration` | 时长；`handler,stage` | 仅开启阶段指标时：queue、load、lock、handler、capture、prepare、admission、durable_commit、enqueue、durable_wait、rollback、release、cleanup、remote_prepare、remote_confirm、logic_queue、commit_queue、completion_* |
| `nest.pipelined.durable_wait` / `nest.pipelined.async_total` | 时长 / 计数；`result=ok\|degraded\|indeterminate\|completion_failed` | pipelined 等持久、完成池结果 |
| `nest.remote.post_commit_without_outcome_total` / `nest.remote.deferred_after_commit_error_total` | 计数；`handler` | Remote 结论未知时的提交后回调交接 |
| `nest.trace.events.total` / `nest.trace.cost` / `nest.dispatch.slow_trace_suppressed` | — | 启用 trace 的消息事件、慢派发堆栈采样被限流 |

`dispatcher` 标签的序列在派发器排空停机后删除（§3.11）。

### 6.2 日志

- `slow dispatch`（≥200ms，`nest/trace.go:16`）：handler、类型、key、耗时、是否慢池 / Remote；超阈值的还会限流采样堆栈。
- `nest handler held entity locks beyond threshold`：锁持有超过 `SlowLockThreshold`。
- `async dispatch failed`：异步消息准入失败（调用方已不在等）。
- `nest dispatch panic` / `nest after-commit callback panic` / `entity guard post-release callback panic`：各自 recover 边界里的 panic。
- `nest pipelined completion failed`：完成池里结果未知或收尾失败而没有调用方等待。

### 6.3 健康与 fence

- kit 注册 `nest` 健康检查：引擎未运行（未启动、已停、已 fence）为 fail，否则 ok 并带 `queue=… delayed=…`（`kit/nest/nest_mod.go:349`）。
- App 的 `RuntimeFailure`（单实例锁丢失、DataEngine / Remote fatal）在唤醒停机之前同步调用 `NestMgr.Fence`（`kit/nest/nest_mod.go:220`）。fence 本身不打日志；之后的派发返回 `nest: admission fenced`（T-213）。

### 6.4 回复错误怎么判断

**规则：按下表从上到下取第一个命中的行，全部用 `errors.Is`。前 5 行任何一个命中都不得重试，即使链上同时有 `ErrLockTimeout`。** 完整说明与边界在 [USER_GUIDE §4 判别表](../../USER_GUIDE.md#回复错误判别是否可能已提交能否重试)（RR-20260926-77），这里是压缩版：

| # | 命中 | 是否可能已提交 | 能否重试 |
| --- | --- | --- | --- |
| 1 | `nest.ErrCommitIndeterminate` | 可能（引擎已 fence） | 不得；恢复后按幂等键核对 |
| 2 | `entity.ErrRemotePersistenceIndeterminate`（或 `ErrRemoteCommitTimeout`） | 可能（本地已提交、Remote 未知） | 不得 |
| 3 | `nest.ErrAfterCommitFailed` | 已提交，收尾失败 | 不得 |
| 4 | `nest.ErrRemotePartRejected` | 本地已提交、Remote 被拒 | 不得整笔重试 |
| 5 | `nest.ErrNestedTransactionCommitted` | 嵌套独立事务已提交 / 未知 | 不得整笔重试 |
| 6 | `nest.ErrNonRollbackNotRequeued` | 未持久提交，但失败前的内存修改已生效 | 框架不重试；业务确认幂等后再试 |
| 7 | `nest.ErrCreatedEntityLockConflict` 且不带 `ErrLockTimeout` | 同上 | 同上 |
| 8 | `ErrCreatedEntityLockConflict` 与 `ErrLockTimeout` 并存 | 否（已回滚、重排到上限） | 可 |
| 9 | `ErrLockTimeout` / `ErrEntityLockGroupChanged` / `ErrEntityGroupTransitionPending` | 否 | 可 |
| 10 | `nest.ErrNestedTransactionRollbackConflict` | 否（嵌套事务） | 原样会再被拒，改写法 |
| 11 | `nest.ErrNestedTransactionInRemoteMessage` | 否（嵌套事务） | 同上 |
| 12 | `nest.ErrCommitRejected` | 否（rollback=none 的内存修改不撤销） | 看原因；`dataengine.ErrFencedEntityPending` 可重试 |
| 13 | `entity.ErrEntityRemoved`（handler 内新建本 handler 刚撤销的 ID） | 否 | 改流程 |
| 14 | `nest.ErrNestCanceled` / `nest.ErrNestTimeout`（`Request*` 的等待出口） | **结果未知** | 持久 handler 按“可能已提交”处理 |
| 15 | 都不命中 | 否（rollback=none 的业务错误按第 6 行语义） | 按业务错误处理 |

另外几个常见错误：`ErrHandlerNotFound`、`ErrEntityNotFound`（Single 目标或 Multi 的第一个目标不存在；Multi 的其余目标缺失时传 nil 给 handler）、`ErrEntityTypeMismatch` / `ErrParamMismatch`（生成包装校验参数）、`ErrCastDeadlockRisk`、`ErrCastNoContext`、`ErrPipelinedNotAllowed`、`ErrPipelinedCommitterRequired`、`ErrDurableRemoteWriteUnsupported`、`ErrRollbackUnsupported`。

### 6.5 常见故障

| 现象 | 先看 | 处理 |
| --- | --- | --- |
| 交叉新建实体偶发 `nest: lock timeout: nest: created entity is locked by another holder` | `nest.dispatch.requeue.total{reason="lock_timeout"}`；失败消息执行约 401 次 | [T-220](../../TROUBLESHOOTING.md)：确认版本含 U-0279 抖动；按 ID 升序新建 |
| `ErrQueueFull` 增多 | `nest.dispatch.queue_len{pool}`、`DispatchLaneStats` 的 `BlockedOnPredecessor`（同 ID 积压）与 `WaitingForWorker`（worker 不够） | 区分热点 ID 与容量不足；不要只调大队列 |
| handler 回 `ErrColdLoadInLogic` / 日志带 `blocking operation in fast worker` | Getter 是否实现 `LoadedChecker`；是否在 handler 里取了未声明的冷实体 | 声明目标或 `SendOptionSlow()`；自定义 Getter 实现 `LoadedChecker` |
| 停机日志 `mod nest stop: ... context deadline exceeded` | 哪个 handler / 外部依赖不返回 | [T-251](../../TROUBLESHOOTING.md)：Nest 停机等真实排空，超时如实报错并保留对象；用新 ctx 重试 |
| 失锁后派发全部 `nest: admission fenced` | `singleton lock lost` 日志 | [T-213](../../TROUBLESHOOTING.md)：设计内 fail-stop |
| 对不存在的实体请求得到 internal 错误 / 广播只跑了前几个 | 版本 | [T-149](../../TROUBLESHOOTING.md)：现版本返回 `ErrEntityNotFound`、广播跳过缺失继续 |
| 回滚后属性 / 定时器与 DAO 不一致 | 组件是否有可变字段、`captureRollback` | [T-227](../../TROUBLESHOOTING.md)、[T-245](../../TROUBLESHOOTING.md)：A1，状态移进 DAO |
| goroutine 数持续增长、nest 队列平稳 | goroutine profile | [T-07](../../TROUBLESHOOTING.md)：业务侧泄漏 |
| glsvet 退出 2 | 输出点名的目录 / 文件 | [T-254](../../TROUBLESHOOTING.md) |

[→ 实现文档：6 失败与不确定结果处理](../impl/02-nest-entity.md#6-失败与不确定结果处理)

---

## 7. 保证与不保证

### 7.1 保证

| 保证 | 条件 / 范围 |
| --- | --- |
| 同一声明目标 ID 的消息按准入顺序执行，同一时刻至多一条在执行 | 只对声明目标；Cast 目标只受锁保护。暂时性错误自动重排的消息重新准入，排到当时已准入的同 ID 后继之后 |
| handler 运行时全部声明目标已按全局锁序加锁；多实体加锁无 AB/BA 死锁 | 锁序 = category 值 + ID；Cast / 新建只能“向上”等待 |
| handler 返回错误、panic、提交被明确拒绝：可回滚事务的 DAO 修改、tracker、新建实体全部撤销 | 只覆盖 DAO（A1）；组件普通字段、`skill.Runtime` 不在内 |
| 越过提交点后不回滚、不自动重排；收尾失败带 `ErrAfterCommitFailed` | — |
| 结果未知时不回滚，引擎立即 fence | 收尾阶段独立事务的结果未知只能从之后的 `ErrNestFenced` 看到 |
| 准入失败在调用返回前就确定，消息不会执行 | 异步消息准入后的失败只记日志 |
| 停机排空全部已准入的派发作业与 pipelined 完成；延迟堆里尚未到期的消息不执行，同步请求得到 `ErrNestStopped`；超时如实报错、不释放 | `Shutdown` 的后台排空不受调用方 ctx 约束 |
| 快池内的框架等待入口 fail-fast（见实现文档 §5.3 清单） | 定向保护，不是全局 I/O 拦截：业务 handler 里任意阻塞调用不会被识别 |

### 7.2 不保证 / 已知限制

- **业务里的阻塞**：handler 里调一个慢 RPC，框架不会隔离它；它占着快 worker 和实体锁。
- **Cast 目标不保证 FIFO**。
- **`durability=memory` + `rollback=state|undo`**：事务存在、持久字段 setter 不报错，但 memory 且无 effect 时 `durableCommit` 直接返回（`nest/rollback.go:589`），这些持久字段的修改不进任何提交记录；内存已变、库里不变：之后别的事务只改其他字段时生成的 Patch 不带它们，直到有事务再改同一字段或实体重载（重载后回到旧值）。USER_GUIDE 对 memory 的说法是“只承诺内存提交”，与此一致；但与生成器文档冲突，见下句。生成器文档说“memory handler 不能修改 persistent 字段”只在 `rollback=none` 时由 panic 强制。（已用临时单测验证 `durableCommit` 不调 `PrepareMutation`、不交 committer、不报错；未在生成工程端到端验证。）这一点已列入报告，待维护者判断是改契约还是改实现。（v1.23.1 起运行期强制，见 [RR-20261006-41](../../bug/RR-20261006-41.md)。）
- **方法 handler 不受 glsvet 并发检查**：`isNestHandler` 只看包级函数（`cmd/glsvet/main.go:550`），codegen 支持的指针方法 handler 里开 goroutine 不报违例。
- **glsvet 只跟同文件的函数调用**（handler 并发检查）、只跟一层同包 helper（A1、停机），没有类型信息。
- **`nest.heartbeat_worker_num` 无效**（见 §5）。
- 组迁移在仓库里没有生产调用方，只有 nest 单测覆盖。
- 需要外部验证：MMO 目标负载下 Nest 的端到端指标（v1.23.0 外部验证清单 E20）。

## 8. 相关文档

- 实现：[impl/02-nest-entity.md](../impl/02-nest-entity.md)
- 执行契约：[roost-coding SKILL](../../agent-skills/roost-coding/SKILL.md)（Nest 与 Entity 一节）
- 旧快速参考：[USER_GUIDE §3～§4](../../USER_GUIDE.md#4-nest-请求与事务)、[INTERNALS §3～§4](../../INTERNALS.md)、[RUNTIME_EXECUTION_MODEL](../../../RUNTIME_EXECUTION_MODEL.md)（部分内容已过时，见实现文档 §9）
- pipelined：[NEST_PIPELINED_COMMIT.md](../../../NEST_PIPELINED_COMMIT.md)；WAL：[NEST_TRANSACTION_WAL.md](../../../NEST_TRANSACTION_WAL.md)
- 生成器：`codegen/docs/NEST_RUNTIME.zh-CN.md`、`codegen/README.md`
- A1：[方案](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)、[字段写提示](../../feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)
- actionflow：[延后队列方案](../../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md)
- 故障：[TROUBLESHOOTING](../../TROUBLESHOOTING.md)
- 其他分区：[03 dataengine](03-dataengine.md)、[04 sync](04-sync.md)、[05 remote / Mirror](05-remote-mirror.md)、[01 app](01-app-lifecycle.md)、[10 时间](10-time.md)、[12 codegen](12-codegen.md)

## v1.23.1 当前口径补充（2026-10-08，未发布）

提交点统一称不可回滚边界：pipelined Enqueue成功后不回滚/重排；持久确认是fsync后票据完成，回复/AfterCommit/Sync仍等待它与解锁。生成标记已支持pipelined。 原正文保留v1.23.0证据。完整对应表见 [B8文档收口](../../review/B8-DOCUMENTATION-CLOSURE-2026-10-08.md)。
