# Roost 开发者完整使用说明

本文面向熟悉 Go、网络服务和数据库的开发者。示例版本基线：core/kit v1.8.0、skill v1.7.0、codegen v1.7.0。

## 1. 先理解边界

框架是一个 module（`github.com/tjbdwanghaibo/roost-core`），分四层：**仓库根部**定义稳定契约、调度和一致性语义，并承载 MongoDB、Redis、NATS、etcd 客户端、WAL、传输、常用玩法执行器与确定性技能程序（`skill/`）；**`kit/`** 负责配置解析与 Mod 装配，并托管通用游戏服务（`kit/service/`）；**`codegen/`** 生成工程、DAO、Entity、Nest、协议、配置和部署文件；**`demo/`** 是可运行的模板。业务仓库负责协议认证、玩家会话、具体组件、handler、玩法规则和容量参数。

推荐依赖方向：

```text
接入层 -> 生成 Sender -> Nest handler -> Entity/Component/DAO
                                      -> Remote Entity/Saga（需要跨域时）
DAO mutation -> Nest transaction -> Data Engine WAL -> Mongo projection/outbox
Entity mutation -> entitysync.Manager（按会话组帧、逐帧准入）或 lockstep -> 客户端
```

## 2. Service 与 Mod

一个二进制可注册多个 Service，通过 `server_type --sid` 选择启动实例。SID 是进程身份，也是路由和 fencing 的一部分；同一运行环境不能出现两个活跃 writer 共用 SID。

Mod 生命周期为 `Init → Provide → Start → StopWithContext`。硬依赖使用 `DependsOn`，存在时才需要排序的集成使用 `OptionalDependsOn`；框架拓扑排序，不要求业务记忆书写顺序。业务从 `app.Registry` 通过 capability 名称和目标类型获取依赖，不保存 kit 私有对象。

常用组合：

| 场景 | 建议 Mod |
| --- | --- |
| 无状态网关 | configdata、etcd、nats、ops、gateway |
| 普通实体服 | mongo、nats、dataengine、nest、ops |
| 跨服实体 | 普通实体服 + sync、remote_entity |
| 长事务协调器 | mongo、nats、dataengine、saga、ops |
| 状态同步 | player 接入层或 nettransport 作为 `entitysync.Transport`；业务装 `entitysync.Manager`，`policy.Interest / Group / Direct` 作为订阅政策（Manager 因传输失败丢掉会话而观察者仍在时，重开会话后 `Interest.Resubscribe` 恢复其订阅，见 RR-20260926-40；实体卸载后重载不了被框架退回 remove、之后重新登记时，三种政策自动重新提交仍持有的订阅，见 RR-20260926-70） |
| 确定性帧同步 | lockstep + KCP/QUIC/UDP transport |

## 3. Entity、Component 与 DAO

Entity 是锁、生命周期和路由的边界；Component 是领域能力；DAO 是持久化与同步状态。聚合加载必须完成所有 DAO、schema migration、版本向量恢复后再一次性发布 Entity，不能暴露半初始化对象。

DAO 字段由 codegen 改为私有存储，读取和修改都走生成方法。写方法在当前 Nest 事务中登记 undo、标记 persist/sync dirty、生成字段级 patch。Map 使用框架生成的受控容器，避免业务获得内部引用后绕过 dirty tracking。

必须遵守：

- handler 进入前 Nest 已按全局顺序获取 Entity mutex；业务不再加同一把锁。
- 不把 Entity、DAO、可变 map/slice 指针带出锁作用域。
- 异步 goroutine 只接收不可变值或 snapshot，不能闭包捕获 Entity。
- ID 默认不复用；删除使用高版本 tombstone，旧 save/ACK 不能复活对象。

## 4. Nest 请求与事务

客户端只持有 codegen 生成的 Sender。同步请求等待返回值，异步请求只表示已进入受控队列；两者都必须经过 Nest，不能直接调用 handler。

回滚模式：

- `rollback=undo`：生成的 mutator 登记逆操作，适合改动字段少的高频请求。
- `rollback=state`：事务前保存状态，适合修改范围复杂或第三方组件无法生成 undo 的请求。

持久化模式：

- `memory`：只承诺内存提交，用于可重建临时状态。
- `async`：WAL durable admission 后可返回，后台批量落权威存储。
- `strict`：等待事务 WAL 和存储提交点，适合支付、跨服资产、唯一奖励。
- `pipelined`：高吞吐 group commit；外部可见结果仍受 durable watermark 约束。Kit 装配的 EntitySync
  （`kitnest.NewModWithEntitySync`）在业务未显式设置 `ManagerConfig.DurableWatermark` 时自动接 committer 的
  `DurableLSN`：committer 实现 `PipelinedTransactionCommitter` 时按水位暂缓未持久内容，否则不设门槛
  （RR-20260926-35）。显式配置优先；自建 `entitysync.Manager` 仍需自己接线，入口 `NestMgr.DurableWatermark()`。

handler 内新建的实体属于当前事务：用当前 Guard 作用域调用 `CreateInScope`（`entity.CurrentGuardScope()`），
以及在 handler 内调用 `EntityManager.Create` / `ManagerAccess.Create`（`IsCreate`）——生成 Lifecycle 的 `Create`、`GetOrCreate`
的创建分支都经过它——都进入同一边界，
与动态 Cast 相同：纳入回滚 / 持久化参与者与 Sync 提交屏障，提交确认（pipelined 为 ticket 持久）之前不外发；
handler 报错、panic 或提交被明确拒绝时撤销发布——从 EntityManager 摘除并标记 removed、不写入持久化记录、
Nest 自己的 EntitySync 注销该 subject（已持有对象的会话收到 ObjectRemove），Guard 释放后调用
`OnDestroy(entity.DestroyReasonCreateRevoked)` 并回收 ID（与业务 `Destroy` 传入的原因、仅内存卸载的 `DestroyReasonMemoryUnload` 都不同；
之前是 `DestroyReasonCommon`，[RR-20260927-12](bugfix/RR-20260927-12.md)）。登记在业务自建 Sync Manager 上的 subject 需在 `OnDestroy` 里自行注销。
在 Nest 之外（登录 / 创角端点、spawner、Service.Init、独立 `WithGuardScope`）调用这些入口仍是立即发布的原语义；
Repository 聚合加载（`IsCreate=false`）不受影响。
RollbackState 下新实体的 DAO 需要可快照（生成 DAO 已满足），remote-managed 实体在 durable 事务内创建会被拒绝，
这两条与 Cast 的约束一致。这类事务捕获失败时 `Create` 撤销发布并返回该错误；业务即使吞掉它、handler 返回成功，
事务也整条回滚，调用方收到该错误（可 `errors.Is`，不按锁超时重排），committer 不被调用（[RR-20260927-11](bugfix/RR-20260927-11.md)）。
handler 内新建实体的锁持有到 handler 结束（memory handler 也一样，并进入本次 Sync 提交屏障），取锁遵循与 Cast 相同的锁序：
新实体的锁组高于 handler 已持有的全部锁组时等待；否则（与声明目标同组或更低组，最常见的写法）只尝试加锁，被其他 handler 占用时
`Create` 返回满足 `errors.Is(err, nest.ErrCreatedEntityLockConflict)` 的错误。可回滚（state / undo）的事务里它同时满足
`errors.Is(err, nest.ErrLockTimeout)`，事务整条回滚后自动重新准入——即使业务吞掉了这个错误；重排后排到同 ID 后继之后，
多次仍冲突时调用方收到锁超时（[RR-20260926-48](bugfix/RR-20260926-48.md)）。例外：同一消息里嵌套独立事务已经提交时消息不再重排，
回复同样两者并存但另带 `ErrNestedTransactionCommitted`，表示“已回滚外层、未重排、嵌套部分已提交”，按下文判别表处理。
不能回滚的 handler（rollback=none，即 memory 快路径）冲突前的内存修改不会撤销，所以框架**不**自动重排这条消息：
`Create` 的错误不带 `ErrLockTimeout`，请直接返回它，调用方收到 `ErrCreatedEntityLockConflict`；业务改返回别的锁超时类错误时，
回复同样补上该哨兵、不重排。是否重试由业务按 handler 的幂等性决定（[RR-20260926-64](bugfix/RR-20260926-64.md)）。
同一规则推广到不能回滚的 handler（memory 快路径，或带 Remote 批次的 memory handler）开始执行后的**任何**锁超时 / 组迁移类错误
（`ErrLockTimeout`、`ErrEntityLockGroupChanged`、`ErrEntityGroupTransitionPending`）：框架不重排，回复满足
`errors.Is(err, nest.ErrNonRollbackNotRequeued)`，原因仍可 `errors.Is`——此时 `ErrLockTimeout` 不再表示“未执行”，失败前的修改已生效。
handler 开始执行之前的准入失败（声明目标取锁超时、组锁被占、组迁移待定）照常自动重新准入。
动态 Cast 在等锁期间目标被 `Destroy` 或仅内存卸载时返回满足 `errors.Is(err, nest.ErrEntityNotFound)` 的错误（此前是 `ErrLockTimeout`），
可回滚的事务照常整条回滚、不重排（[RR-20260926-73](bugfix/RR-20260926-73.md)）。
handler 内先 `Destroy` 某个实体、再新建同 ID 的实体时，新实例拿到新锁，按上面的锁序规则取锁（同组即尝试加锁），持锁到 handler 结束并进入本次提交边界；
事务回滚时新实例按上文撤销发布，旧实例的销毁不回滚（`Destroy` 本身不是事务操作）。同一 handler 里若 `Destroy` 之后别处重建了同 ID、再 `Cast` 它，
返回 `nest.ErrCastDeadlockRisk`，不会拿到未加锁的实例（[RR-20260926-67](bugfix/RR-20260926-67.md)）。
生成 Lifecycle 的 `GetOrCreate` 在同 ID 的上一个实例正在撤销 / 销毁收尾（`entity.ErrEntityRemoved`）时最多再试两次，已生成的工程重新运行生成器即可获得（[RR-20260926-57](bugfix/RR-20260926-57.md)）；在 Nest handler 内这个窗口按新建锁冲突返回 `nest.ErrCreatedEntityLockConflict`（[RR-20260926-81](bugfix/RR-20260926-81.md)）。
handler 内新建时同 ID 的上一个实例正在撤销 / 销毁收尾（锁已释放、收尾回调未结束），与上面的锁冲突同样处理：`Create` 返回 `ErrCreatedEntityLockConflict`
（可回滚事务同时带 `ErrLockTimeout`、整条回滚后重新准入；不能回滚的 handler 不带、不重排），不再返回 `entity.ErrEntityRemoved`；
Nest 之外仍返回 `entity.ErrEntityRemoved`（[RR-20260926-81](bugfix/RR-20260926-81.md)）。
例外：同 ID 的上一个实例是**本 handler 自己**撤销的（例如 handler 内 `RunIsolatedTransaction` 新建后回滚，撤销收尾挂在同一个 Guard 上，
要等整个 handler 释放才执行），在同一 handler 里再建它是确定失败：`Create` 返回满足 `errors.Is(err, entity.ErrEntityRemoved)` 的错误、
文案说明是本 handler 内已撤销的同 ID，不带 `ErrCreatedEntityLockConflict` / `ErrLockTimeout`，消息不重排（之前可回滚事务会每次重排都重现、
空转到重排上限）。handler 结束后同一 ID 可以正常新建（[RR-20260927-21](bugfix/RR-20260927-21.md)）。

结果不确定时框架 fence 实例，不进行猜测性回滚。业务必须把“服务暂不可用”和“业务失败”分成不同错误码。

### 回复错误判别：是否可能已提交、能否重试

`Nest.Request` 等返回的错误可能同时满足多个哨兵（原因链保留），**只看有没有 `ErrAfterCommitFailed` 不够**：嵌套独立事务已提交时
回复是 `ErrNestedTransactionCommitted` 包着外层原因（可能正是 `ErrLockTimeout`），并不带 `ErrAfterCommitFailed`。
按下表**从上到下取第一个命中的行**（全部用 `errors.Is`，不要匹配文本）。规则：**带任一“可能已提交”哨兵（前 4 行）即不得重试**，
即使链上同时有 `ErrLockTimeout`；第 5、6 行“框架没有重排、修改未回滚”也不得盲目重试（[RR-20260926-77](bugfix/RR-20260926-77.md)）。

| # | 回复满足 `errors.Is(err, …)` | 含义 | 是否可能已提交 | 能否重试 |
| --- | --- | --- | --- | --- |
| 1 | `nest.ErrCommitIndeterminate` | 消息自己的事务或 handler 内嵌套独立事务的提交结果未知；引擎已 fence（RR-76）。消息自己的事务结束后、收尾阶段（Guard post-release、解锁后回调）调用的独立事务结果未知时引擎同样 fence（RR-84），但业务在收尾阶段已无法把错误带进回复：**回复里看不到本哨兵**，只能从之后的请求得到 `ErrNestFenced` 得知 | **可能** | 不得重试；等实例从 WAL 恢复后按业务幂等键核对 |
| 2 | `entity.ErrRemotePersistenceIndeterminate`，或 `entity.ErrRemoteCommitTimeout` | 本地已持久提交，Remote 结果未知（RR-37）：Durability 0 远端回复丢失 / 未到达、strict 等待期间投影器报告未知时带前者；strict 等待 Remote 确认到截止时带后者（与 `context.DeadlineExceeded` 并存，不带前者） | **可能** | 不得重试；结论由 Remote 后台收尾给出 |
| 3 | `nest.ErrAfterCommitFailed` | 消息自己的事务已提交，收尾（release hook、AfterCommit、Close、引用释放）失败（RR-46 / 53）。收尾阶段调用的 `RunIsolatedTransaction` 不算消息自己的事务，它的提交不会让回复带本哨兵（RR-84） | **已提交** | 不得重试 |
| 4 | `nest.ErrNestedTransactionCommitted` | 消息自己的事务没提交，但 handler 内嵌套独立事务（或收尾阶段——Guard post-release、解锁后回调——调用的独立事务，RR-84）已提交或结果未知；消息未重排（RR-65） | **部分已提交** | 不得整笔重试 |
| 5 | `nest.ErrNonRollbackNotRequeued` | 不能回滚的 handler（memory）开始执行后遇锁超时 / 组迁移类错误；未准入，但失败前的内存修改已生效且不撤销，未重排（RR-73） | 未持久提交，修改未回滚 | 框架不重试；确认 handler 幂等（或读回状态）后业务自行重试 |
| 6 | `nest.ErrCreatedEntityLockConflict`，且不带 `ErrLockTimeout` | 不能回滚的 handler 内新建实体锁冲突，未重排，冲突前的修改未回滚（RR-64） | 同上 | 同上 |
| 7 | `nest.ErrCreatedEntityLockConflict` 与 `ErrLockTimeout` 并存 | 可回滚事务新建实体冲突：已整条回滚、自动重排仍冲突到上限（RR-48）。只在不命中第 1～4 行时成立——外层已有嵌套提交时同样两者并存，但那是“已回滚、未重排”，按第 4 行。同 ID 由本 handler 自己撤销的不在此列，见第 12 行 | 否 | 可重试 |
| 8 | `nest.ErrLockTimeout` / `ErrEntityLockGroupChanged` / `ErrEntityGroupTransitionPending` | 未提交：准入阶段失败，或可回滚事务已回滚；框架已自动重排到上限 | 否 | 可重试 |
| 9 | `nest.ErrNestedTransactionRollbackConflict` | `RunIsolatedTransaction` 要写外层可回滚事务已快照的实体，写持久记录前被拒绝并自身回滚（RR-74） | 否（嵌套事务） | 原样重试仍被拒；把写入并入外层事务 |
| 10 | `nest.ErrNestedTransactionInRemoteMessage` | 带 Remote 批次的消息里调用 `RunIsolatedTransaction`（handler 内，或消息自己的事务结束后、批次收尾前的收尾阶段），函数体未执行（RR-75 / 84） | 否（嵌套事务） | 原样重试仍被拒；把写入并入消息自己的事务 |
| 11 | `nest.ErrCommitRejected` | 提交被明确拒绝，事务已回滚 | 否 | 看原因：`dataengine.ErrFencedEntityPending` 可重试，其余按业务错误处理 |
| 12 | `entity.ErrEntityRemoved`（handler 内新建） | 同一 handler 里再建本 handler 自己较早撤销的同 ID（撤销收尾要等 handler 释放），确定失败、未重排（RR-20260927-21）；可回滚事务已回滚 | 否（消息自己的事务；嵌套事务已提交时按第 4 行） | 原样重试会重复同一流程、再次失败；改业务流程，不在同一 handler 内重建刚撤销的 ID |

第 2 行的两种形态由 `remoteentity/reply_sentinel_table_test.go` 在真实 Nest + 正式 Remote Manager 上钉住（OPEN-ITEMS B23）。
调用方自己的等待先到截止时，`Nest.Request` 返回 `nest.ErrNestCanceled`（与 ctx 错误并存）或 `nest.ErrNestTimeout`，这只说明没等到回复、不说明结果：
截止只停止等待、不撤销已准入的业务，持久 handler（尤其 strict Remote，请求截止与 Remote 确认截止常是同一个时刻）按“可能已提交”处理，不得据此重试。

第 9、10 行是 `RunIsolatedTransaction` 返回给业务的错误；业务原样回复时，消息自己的事务是否提交仍按其余行判断（这两种情况下嵌套事务
什么都没提交，不会触发第 4 行）。`ErrNestFenced` 表示实例已 fence、请求未执行，或 handler 已执行但消息自己的事务在交给 committer 之前被拒绝并已回滚（例如 handler 内嵌套独立事务结果未知、fence 之后，RR-20260927-06），等实例恢复后可重试；链上同时有前 4 行的哨兵时按那一行。
`RunIsolatedTransaction`（以及新建事务的 `RunDetachedTransaction`）在消息里从不认领消息：消息自己的事务结束之后（提交、回滚或失败），
在它的收尾阶段（Guard post-release 回调、解锁后回调）调用时，同样按嵌套独立事务处理——带 Remote 批次的消息返回第 10 行的错误；
纯本地消息照常执行，已提交时回复按第 4 行、结果未知时按第 1 行 fence，不会被当成消息自己的提交（[RR-20260926-84](bugfix/RR-20260926-84.md)）。

## 5. Commit、Load 与主动 Flush

Data Engine 是唯一保存入口。字段变化先进入当前 Nest transaction，再作为版本化 Put/Patch/Delete 写入 WAL；Mongo version CAS 防止旧写覆盖新状态。WAL/Projector backlog 有硬容量和年龄上限，超过门禁触发 runtime failure，而不是无限堆内存。

主动刷盘使用 Registry 中的 Data Engine 能力调用 `Flush(ctx)`。典型时机：停机、迁服、运维检查和版本升级。不要为每个普通请求 Flush，否则会破坏 group commit/批 projection 吞吐；需要强确认的业务选择 Nest strict durability。

Load 只接受完整聚合快照。迁移函数必须幂等、可测试并携带 schema version；加载失败不允许生成“空玩家”覆盖旧数据。

## 6. Remote Entity

Read 模式返回不可变 snapshot：L1 是进程内有界原子缓存，L2 是共享 snapshot store。`Cached` 不回源，`Monotonic` 在版本不足时 singleflight 回源，`Linearizable` 每次读权威存储。高频展示、排行榜引用和 AOI 属性优先 Cached/Monotonic；结算前校验使用 Linearizable 或转成 owner 命令。

L2 快照键默认是 `remote_entity:snapshot:<tenant>:<kind>:<id>:<scope>:<policy>`，不带部署前缀。多个部署共用一个 Redis db 时，给每个部署配置不同的
`remote_entity.snapshot_l2_key_prefix`（例如 `roost:<工程名>`，core 为 `Config.SnapshotL2KeyPrefix`），键变为 `<prefix>:remote_entity:snapshot:…`，
否则彼此读写同一份快照。缺省为空时键与旧版本逐字相同，不需要迁移。同一部署的所有节点必须配置同一个值：新设或修改前缀相当于换一套空的 L2（快照会从权威重新发布），
滚动修改期间新旧节点互相看不到对方写入的 L2，`Cached` 读可能在 `snapshot_l2_ttl` 内读到旧前缀下的旧快照，所以应整体重启。前缀不能含空白，
也不能含 Redis Cluster hash tag（`{…}`，L2 脚本只操作单键，带 tag 会把全部快照键钉在同一个槽），Init / Assemble 拒绝这类值（[RR-20260927-17](bugfix/RR-20260927-17.md)）。
这个前缀只作用于 L2 快照键；Remote 的锁键仍由 `remote_entity.lock_key` 决定，非 authority 兼容装配的 Redis 所有权标记键由 `NewRedisMarker` 的 key 参数决定（默认 `remote_entity:marks`）。

Write 模式使用 Mongo 持久所有权；共享写先竞争 Redis 协调锁，再取得 Mongo majority 写许可，owner-routed 写也取得持久许可，然后权威加载 → Nest 事务修改 → 条件提交。存储条件同时包含 StateVersion、MarkerEpoch、LockFence、RouteEpoch。分布式锁只避免同时进入临界区，四维 fence 才能拒绝暂停后恢复的旧 owner 或旧路由写入。

正式 Assembly 要求持久权威能力；MongoCommitter 创建即强制校验许可，WriteAuthority() 只返回能力，没有弱校验开关。当前未部署，不提供旧协议迁移入口；不支持的元数据拒绝启动，见 [提交契约](bugfix/RR-20260925-02.md)。

`remote=managed` 实体的 DAO 必须是 `dbscope=global`（缺省）：托管实体由任一进程提交、所有权可迁移，按服选库会让提交与加载落在不同的库。这条规则在四处以同一个可 `errors.Is(err, entity.ErrRemoteManagedServerScopedDAO)` 的错误拒绝（`remoteentity.ErrRemoteManagedServerScopedDAO` 是同一个值）：`roost generate` 生成期、生成 registry 末尾的 `entity.ValidateEntityRegistry`、Remote 装配（`remoteentity.Assemble` / `Start`），以及 Remote 事务进入 WAL 之前（手写实体注册的 DAO 工厂漏报 sid DAO 时由这里兜底，事务回滚、不产生提交）。已写入 WAL 的旧记录重放不受影响。见 [RR-20260926-45](bugfix/RR-20260926-45.md)、[RR-20260927-09](bugfix/RR-20260927-09.md)。

不要把 Remote Entity 当透明 RPC ORM。调用方必须选择读一致性，命令必须有 request/transaction ID，重试必须幂等。

## 7. 跨服务 Saga

Saga 用于无法放进同一 Mongo transaction 的多阶段流程，例如跨区交易、联盟转服、邮件发奖与外部支付。每一步由持久状态机、lease fencing、outbox 和幂等 receipt 驱动；失败执行显式补偿。Nest 事务可写入 start effect，使“本地提交”和“启动 Saga”共享一个 commit point。

Saga 不是分布式 ACID：补偿可能延迟，外部系统可能需要人工处理。步骤 handler 要区分可重试错误、永久错误和结果未知；补偿同样必须幂等。

## 8. 实时同步怎么选

状态同步适合 ARPG/MMO/大多数房间服：服务器权威模拟，按 20 Hz 产生全局 snapshot 或 delta，按 AOI/LOD 给不同客户端裁剪字段；可靠通道发基线和关键事件，datagram 发可丢弃最新状态。技能的权威结果进入状态 mutation，施法表现、音效和轨迹进入 presentation event，因此能覆盖技能游戏而不要求把所有表现塞进 Entity snapshot。

Lockstep 适合客户端确定性模拟的 MOBA/RTS：服务器排序输入帧、保存历史、冗余广播和校验 hash，不运行完整战斗模拟。追帧走可靠通道，实时输入走 datagram。客户端算法、定点数、随机种子、配置 hash 必须一致。

单房间默认上限 100 Entity/订阅者。20 Hz 是调度目标，不代表所有字段每帧发送；用 interest、LOD、dirty delta、量化和 baseline ACK 控制带宽。慢客户端只能影响自己的 session。

## 9. 技能系统

技能配置先编译为不可变 Program，再由定点数 runtime 执行。HostAdapter 只能在已持锁 Entity 作用域内提交权威 mutation。技能逻辑要明确区分：可恢复的 runtime state、需要同步的权威状态、只发给客户端的表现事件。

版本发布时固定技能 schema/程序 hash；热更只能在声明的安全边界切换。正在执行的旧技能是继续旧 Program 还是迁移到新 Program，必须由业务策略明确，不能隐式替换。

## 10. 配置、协议与错误码

开发与生产配置分离。生产配置必须经过 `roost project doctor`，不得含 `CHANGE_ME`、localhost、开发 token 或明文仓库 Secret。环境差异使用部署系统挂载完整配置；不要靠构建不同镜像改变配置。

协议定义保持单一来源，由 codegen 生成 pb、msgid、绑定和 manifest。变更遵循向后兼容：字段只新增、不复用编号；先发布兼容 reader，再发布 writer，最后清理旧字段。错误码 ID 空间由 `roost id` 检查，不在多个服务手工分配。

## 11. 测试策略

每次提交至少：

```bash
make generate
make ci
go test -race ./...
```

生产门禁额外包含：真实 Mongo replica set、Redis AOF/WAITAOF、NATS JetStream、etcd compaction；kill -9、磁盘满、网络分区、主从切换；WAL replay 与 tombstone 防复活；重复消息和乱序；20 Hz/100 Entity 的 p95/p99、CPU、分配、队列和带宽。

## 12. 常见错误

- `readyz` 失败：先看依赖 health 和 runtime failure，不要只重启掩盖 fence 原因。
- WAL 无法启动：检查目录是否被另一个 SID 使用、权限、磁盘空间和旧版本格式。
- NATS reliable 找不到 Redis：必须安装 Redis Mod；顺序由可选依赖图自动处理。
- Remote Entity 旧写被拒绝：这是 fence 生效，重新解析 owner/route 并从新 snapshot 发起命令。
- K8s 滚动更新卡住：单副本 PDB 会阻止自愿驱逐；按部署手册执行有状态维护窗口，不要强行双 writer。

## Remote 投影并发

正式 DataEngine 的 `dataengine.projection.remote_workers` 默认 8，范围 1～64；设置 1 可保持串行投影。只并行相邻、Entity 不重叠的纯 Remote 事务，相同 Entity 和带普通 DAO/effect/receipt 的混合事务保留顺序。独立使用 core 时设置 `engine.ProjectorOptions.RemoteProjectionWorkers`，0 取默认值。自定义 Remote 适配器未声明并发安全时保持串行。

每笔业务仍执行原有 Mongo 原子提交和完整快照发布，strict 请求确认条件不变；WAL checkpoint 只跨越连续成功前缀。该参数调整 I/O 并发，不改变 Nest 请求超时和客户端 Sync 频率。性能与超时定位见 [实施报告](feature/REFACTOR-2026-09-25-remote-throughput.md)。

### Nest Remote 慢操作并发

Nest 统一使用快慢两个业务执行池。所有 handler、Entity Guard、本地事务和释放锁前的 Sync 在快池；Remote 获取/确认/释放、声明目标中未加载实体的预加载（统一准入自动判定，RR-20260926-25）与显式 `SendOptionSlow()` 的目标加载在慢池。慢池为共享队列，不按 ID 分槽；所有显式目标在统一准入时登记顺序，等待前驱不占快 worker。加载初始化、提交失败本地回滚等需要本地执行的步骤通过 `entity.RunLocal(ctx, fn)` 回到快池，自定义 Loader 也必须遵守。

```go
nest.NestOptionWithWorkerPools(
    nest.WorkerPoolConfig{Workers: 8, QueueCap: 10000}, // 快池
    nest.WorkerPoolConfig{Workers: 64, QueueCap: 64},   // 慢池
)
```

Kit 对应 `nest.fast.workers`、`nest.fast.queue_capacity`、`nest.slow.workers`、`nest.slow.queue_capacity`。容量是整池等待总数，包括等待 ID 前驱的消息，排除执行中的请求和已预留执行额度的就绪请求；内部快延续单独计数。默认快并发 GOMAXPROCS、容量 10000；慢并发 max(32, 快并发×4)、容量 64。旧 `worker_num/queue_capacity/remote_workers` 仍作为未设置新值时的来源，`heartbeat_worker_num` 不再创建单独池，旧 `SendOptionIsCost()` 等同 `SendOptionSlow()`。冷目标不再需要手工加 Slow：Nest 在统一准入时对 Single/Multi/MultiGroup 的声明目标做一次只读内存判定（Getter 可选实现的 `entity.LoadedChecker.IsLoaded`，`ManagerAccess` 已实现，不调用 `Get`，RR-20260926-47），有未加载且可由 loader 加载的目标就走慢阶段预加载，业务代码和生成 sender 不变；Broadcast 仍按已加载目标尽力扇出，冷目标逐个报告。显式 Slow 仍有效，用于强制慢准备。handler 内部阻塞 RPC 必须显式拆为前置 I/O，无法自动迁移。

`NestMgr.Stats().Fast/Slow/FastContinuations` 和 statslog 的 `nest.fast/nest.slow` 输出两池及内部延续；旧 `Stats().Remote` 只是 Slow 的源码兼容别名。可选 stage metrics 的 `remote_prepare`、`logic_queue`、`remote_confirm` 分别定位获取、逻辑排队和后置确认。停机关闭外部准入，两池保持运行直到已接受工作和内部延续全部排空；strict 请求仍在确认完成后返回，WAL 持久准入保留锁内契约。

慢池可以设为 1024 worker / 16 等待位，独立 ID 使用执行额度、同 ID 仍按前驱顺序等待；它最多准入 1040 个外部慢请求，不能把 16 当作总在途上限。应按后端容量选择并发，当前默认未上调；[配置解释与验证边界](review/NEST-FAST-SLOW-2026-09-25.md)。

### 回放、快阶段访问与观测

`dataengine.projection.read_bytes` 对应 `ProjectorOptions.ReplayReadBytes`，默认 4MiB，
限制一次回放保留的逻辑记录字节；与 `projection.batch_bytes` 的投影分段预算独立。
首条超过软限额的记录独占一次读取以保持进度；WAL 解码可能临时读取下一条，故不是精确堆内存上限。
Remote 在已读取记录中，再按 ReplayBatchRecords / ReplayBatchBytes（projection.batch_bytes）限制并行窗口；
worker 数只限制同时在途量，遇到共享 Entity、相同事务或特殊事务即截止。
观察到错误后停止补位，等待已经开始的工作结束，仅确认连续成功前缀。

Nest 快阶段对 Getter 传入 `entity.WithLoadedEntitiesOnly(ctx)`。
快阶段经 ManagerAccess `Get/GetMany`（含动态 Cast）访问未加载目标时，不做 I/O、不等 singleflight，
返回可 `errors.Is` 判别的 `entity.ErrColdLoadInLogic`（快 worker 上同时包裹 `fctx.ErrBlockingInFastWorker`），
业务可以据此降级，例如“好友离线”（RR-20260926-26）；仅带 LoadedEntitiesOnly context 的池外访问同样返回该错误。
真正会等待的入口——EntityRepository 冷加载、投影等待、Remote 准备/确认——在快 worker 上于等待和副作用之前 panic，
由 Nest 转为请求错误。无 loader 时保留 nil/缺失语义。
需要冷目标的业务把目标列入消息的 ID 集合即可：统一准入发现声明目标未加载时自动走慢阶段预加载，
同 ID 顺序仍在准入处建立（RR-20260926-25）。准入判定之后、handler 取得 Guard 之前目标被驱逐（读取、引用、
取锁三个窗口）时，同一条已准入请求原位转到慢池准备，保留同 ID 顺序位置，不重新排队、不在快 worker 上冷加载；
Stats 的慢池 Started 会多计一次，指标 `nest.dispatch.slow_reroute.total` 记录次数。显式 `nest.SendOptionSlow()` 仍然有效。
同一实体的并发冷加载由 ManagerAccess 合并成一次（singleflight）。加载运行在与调用方解耦的 ctx 上（RR-20260926-54）：
保留第一个调用方 ctx 的值，不随任何调用方取消或截止；每个等待方按自己的 ctx 离开并得到自己的 `ctx.Err()`，最后一个
等待方离开也不取消在途加载，完成后实体照常进入 EntityManager，之后的访问直接命中。加载只受两个框架约束：
`entity.DefaultEntityLoadTimeout`（30s，`ManagerAccess.ConfigureLoadTimeout` 可改，kit 配置键 `nest.entity_load_timeout`，缺省或 0 不改、负值拒绝启动，
[RR-20260927-13](bugfix/RR-20260927-13.md)；超出时等待方得到满足
`errors.Is(err, context.DeadlineExceeded)` 与 `entity.ErrEntityLoadTimeout` 的错误）和 loader 注销（DataEngine Runtime
停机时取消在途加载，错误满足 `context.Canceled` 与 `entity.ErrEntityLoaderStopped`）。因此短预算的调用方（如 2s 登录预算）
先超时离开，不再连带同一 flight 里预算更长的等待方；它随后重试会加入仍在进行的那次加载，而不是重新发起。
领头调用方仍在等待时，加载里需要 Entity 锁的发布沿用它自己的本地执行器（Nest 慢阶段即本条消息的快续行）；
它离开后改走 Nest 绑定给 ManagerAccess 的 `NestMgr.RunLocal`（Nest 构造时经 `LocalExecutorBinder` 自动绑定），仍在快池执行。
Nest 只对传入的 Getter 本身做 `LocalExecutorBinder` 类型断言：**包装了 `ManagerAccess` 的自定义 Getter 必须实现并转发
`BindLocalExecutor(run)` 给内层 ManagerAccess**，否则领头方离开后的发布会在加载 goroutine 上就地执行（与没有 Nest 时相同，
不经快池）；正式 kit 装配直接传 ManagerAccess，不受影响（RR-20260926-54）。
未声明的动态 Cast 目标保持只读已加载实体（返回 `ErrColdLoadInLogic`）。自定义 Getter 仍需遵守 `entity.LoadedEntitiesOnly(ctx)`
（快阶段读取用它）。准入判定只调用可选的 `entity.LoadedChecker.IsLoaded(id)`：它在发送方 goroutine（含快 worker 与唯一的延迟派发
goroutine）上执行，不得 I/O、等待或 Touch；未实现它的自定义 Getter 保持 RR-25 之前的行为——准入不判冷、不自动转慢、准入后也不原位转慢，
冷目标返回 `ErrColdLoadInLogic`，需要预加载时用显式 `SendOptionSlow()`（RR-20260926-47）。
不能在持有 Guard 的动态 Cast 中临时加载，也不能在失败后自动重放已执行的 handler。

`NestMgr.Stats().Queue` 分别报告快/慢池 Running、Ready、BlockedOnPredecessor、WaitingForWorker，
以及拒绝、峰值、最老等待与前驱/worker 累计等待；内部续行有独立运行计数。
Ready 包含已预留执行额度但 goroutine 尚未取走的请求；准入与等待统计都计入正在执行的续行。
PeakWaiting / OldestWaiting 是前驱等待和 worker 等待的合并值，累计等待时长分阶段。
Projected 是成功投影尝试数，成功但未 ack 的后缀重放后会再次增加；不能据此推断唯一事务数。
`Sync.Manager.Stats()` 使用增量计数和等待链表，不再获取所有 subject 锁；低频核对用 `AuditStats()`。
两者都允许各阶段间并发推进，需要在静止状态比较精确计数。

九项实现、回归、容量梯度、混合业务与长稳结果及复跑参数见
[三大模块九项实施记录](feature/REFACTOR-2026-09-26-core-nine-items.md)。


## 2026-09-26 提交与生命周期兼容说明

- 正式配置段为 `syncbus:`，旧 `room:` / `sync:` 仍兼容，优先级依次降低。旧段被读取时启动日志告警弃用；被 `syncbus:` 遮住的旧键与不认识的键也会告警；`syncbus.transport` 写错（非 nats / jetstream）启动失败（RR-20260926-12）。
- `roost project upgrade --consolidate` 遇到 v1.16.x 起已删除或换包的框架符号（如 `spatial.InterestManager`、`statesync.Reassembler`、`room.DecodeRoomWireFrame`）时，先完成 import 改写，再逐条列出 `文件:行` 与迁移指引并以非零退出；按指引修改后重跑（RR-20260926-24）。
- `EntityRepository` 在正式 Runtime 中冷加载时等待该实体在途投影，冷目标须声明 Slow；自定义 RecoveryGate 需要转发 `WaitEntityProjection`。
- 多进程按租约交接实体所有权时，交出前（驱逐本地副本之后、释放租约之前）须在慢路径调用 `kit/dataengine` Mod 的 `WaitEntityProjection(ctx, 完整EntityID)`，等待失败则保留租约重试；接手方只读 Mongo，看不到本进程未投影的 WAL。game-demo 的闲置交还已按此实现（RR-20260926-31）。
- Remote 数据提交成功后 Close/hook 错误仍可能返回给请求，不能按“未提交”自动重复业务。未知结果由恢复流程处理。
- 同 SessionID 旧传输仍在退出时，新 Open 返回 `ErrSessionAlreadyExists`；应在退出后重试或使用新连接 ID。
  （2026-09-26 复核补修）现在返回可 `errors.Is` 的 `entitysync.ErrSessionClosing`（仍包装 `nettransport.ErrSessionAlreadyExists`）；同 ID 另一次 Open 正在等待传输确认时返回 `entitysync.ErrSessionOpening`。两者都表示本次没有创建会话、可稍后重试；OpenSession 不阻塞等待，重试应放在慢阶段或下一次调度。传输确认前会话对 Subscribe/Flush 不可见，不再出现假的 SessionLost。
- Remote 写与 lease-fence receipt 同事务暂不支持，DataEngine 在写 WAL 前返回 `ErrRemoteLeaseFenceUnsupported`，由 Nest 锁内回滚。历史 skipped WAL 的 Remote 拒绝结论会被持久记录。
- **原生 saga 步骤的实体屏障（RR-20260926-30，行为收紧）**：本地 mutation + lease fence 的记录准入后、投影结果确定前，写同一实体的其他事务（含系统删档、同一命令的重投）在 WAL 准入处被拒绝，错误可 `errors.Is(err, dataengine.ErrFencedEntityPending)`（同时满足 `nest.ErrCommitRejected`），Nest 已整体回滚。它是**可重试**错误：业务入口应回复“稍后重试”或延迟重投，不能当成业务拒绝；正常只持续一次投影（毫秒级），Mongo 变慢或中断时与投影积压同量级。只读 handler 不受影响；屏障只看写集合。
- 投影时租约已失效的原生步骤被跳过后，DataEngine 在 Nest 快池内驱逐受影响的常驻实体（`EntityManager.Destroy`，`DestroyReasonCommon`，不删库），实体的 `OnDestroy` 会被调用；下次访问从 Mongo 重载。装配了 `NewModWithEntitySync` 的 kit 工程会在重载后调用 `entitysync.Manager.Rebind`，原订阅者收到整份全量；自建 entitysync 的装配需要自己在 `EntityRepository.OnEntityLoaded`（或 kit/dataengine Mod 的同名方法）里调用 `Rebind`，否则驱逐后的 subject 一直停在关闭状态（不发送、不计失败），直到 `Unregister`。`Register` 遇到旧状态已关闭的同 ID subject 也会走同一重新绑定路径。
- **驱逐 / 卸载后订阅者的修正（RR-20260926-59，行为变化）**：订阅者在驱逐前可能已收到被跳过的效果（Remote Durability 1 也可能已收到被拒绝的内容）。实体被仅内存卸载（`ManagerAccess.Unload`，本条与下文 Remote 持久拒绝共用）时同步状态立即关闭；若该 subject 仍有订阅者，框架在快池之外经正式仓储从权威重载实体（等该实体投影/驱逐结束，发布回快池）并 `Rebind`，订阅者收到同一对象的权威全量；权威中没有该实体、重载有界重试（默认 5 次）后仍失败或重载队列（默认 4096）已满时，订阅者收到 `ObjectRemove`（remove 发完前实体又被加载时，`Rebind` / `Register` 按 RR-55 的机制排到退役完成后登记、返回 nil，不再遇到 `ErrSubjectRetiring`）；无订阅者不主动重载。重载就是一次普通的共享冷加载（RR-54：受框架加载上限与 loader 注销约束）。`NewModWithEntitySync` 的 kit 装配自动接线，停机时先停重载（取消在途、不发 remove）；自建装配调用 `entity.ManagerAccess.ConfigureUnloadResync(entitySyncManager, entity.UnloadResyncConfig{})`（发布经 Nest 绑定给 getter 的 `RunLocal`）并在停 Nest 前调用返回的 stop。自定义 loader 报“权威没有”时包 `entity.ErrAuthorityEntityNotFound`（DataEngine 的 `ErrEntityAggregateNotFound` 已满足）或返回 `(nil, nil)`。计数见 `ManagerAccess.UnloadResyncStats()`（[RR-20260926-59](bugfix/RR-20260926-59.md)）。
  kit 配置键（[RR-20260927-13](bugfix/RR-20260927-13.md)，缺省或 0 取框架默认，负值拒绝启动）：`nest.unload_resync.workers`（并发重载上限，默认 4）、
  `nest.unload_resync.attempts`（每实体尝试次数，默认 5）、`nest.unload_resync.queue_capacity`（等待重载的实体数上限，默认 4096，超出立即 remove）；
  单次重载的上限即 `nest.entity_load_timeout`。退避（100ms 起翻倍、上限 2s）不开放配置。
  **最坏延迟（[RR-20260927-14](bugfix/RR-20260927-14.md)）**：一个实体从登记到得出结论（重载成功或退回 remove）最多
  `T_entity = attempts × T_load + Σ_{k=1..attempts-1} min(100ms·2^(k-1), 2s)`，`T_load` 为 `nest.entity_load_timeout`；队列 FIFO，风暴中最后一个被接纳的实体
  最迟在 `ceil((queue_capacity + workers) / workers) × T_entity` 后得出结论，其间订阅者停在旧内容上。默认值：`T_entity = 5 × 30s + 1.5s = 151.5s`，
  上界 `1025 × 151.5s ≈ 43.1 小时`（权威一直不作答、每次加载都等满 30s 时）；加载快速失败时约 `1025 × 1.5s ≈ 26 分钟`。上界假设风暴之后没有新的卸载
  （处理中又被卸载的实体会再排一轮），发布回快池的排队等待另计。需要更短的上界时减小 `queue_capacity`（放不下的立即 remove）、`attempts`、
  `nest.entity_load_timeout` 或增大 `workers`，例如 `workers: 8, attempts: 3, queue_capacity: 256, entity_load_timeout: 5s` 时约
  `33 × 15.3s ≈ 8.4 分钟`。积压看 gauge `entity.unload_resync.backlog`（无标签，等待或正在重载的实体数，停止时归零）或 `UnloadResyncStats().Backlog`。
- 运维：`Projector.Stats()` 新增 `FencedEntities`（当前被屏障挡住写入的实体数）、`FencedAdmissionRejected`、`StaleEvictions`；指标 `dataengine.fence.skipped.total` / `dataengine.fence.evictions.started.total` / `dataengine.fence.evictions.failed.total`。`StaleEvictions` 持续增长说明原生步骤的 `LeaseDuration` 短于投影延迟；`FencedEntities` 长时间不为 0 同时 `evictions.failed` 增长说明驱逐失败（如 Nest 已停止或被 fence），实体写入会一直被可重试地拒绝，按 Nest/DataEngine 健康检查处理。
- `nest.NestMgr.RunLocal(ctx, fn)` 是框架后台 goroutine 把需要 Entity 锁的步骤交给快池的正式入口（快 worker 上调用返回 `fctx.ErrBlockingInFastWorker`）；实现 `nest.LocalExecutorBinder` 的 committer 在 `NewEngine` 时自动拿到它，DataEngine 的 Projector 与 kit Mod 已实现。
- 投影事务现在对 lease fence 指向的 claim 文档做条件写（`updated_at`），与 `DataEngineStepInbox` 的过期接管串行化；claim 文档的 `updated_at` 会随投影更新。
- 关闭超时不代表 WAL 目录已释放；等待在途调用退出后再次 Close。`OpenRuntime` 始终关闭自己创建的 WAL。
- `Projector.Close` 之后 `Flush` / `ReplayPass` 返回 `ErrRuntimeStopped`，不能再用“先 Close 停后台循环再手动回放”的写法；需要逐步驱动回放的外部夹具用 `ProjectorOptions.ManualReplay`（不启动后台循环，生产装配不设置），见 [RR-20260926-29](bugfix/RR-20260926-29.md)。
- `ProjectorOptions.OnFatal` 在首次确定性投影冲突后**异步**调用一次；调用前 fatal 已对准入、Flush 和实体等待方可见，回调里可以同步 Close/Shutdown。依赖“Flush 返回前回调已执行完”的代码需改为等待回调，见 [RR-20260926-17 补修](bugfix/RR-20260926-17.md)。

细节与验证边界见 [RR-10～24 修复汇总](review/REVIEW-2026-09-26-release-fixes.md)。

## 2026-09-26 Remote 收尾链路（RR-37/38/39/46）

- 已提交的 Remote 请求若收尾失败（Close 释放不完整、release hook 或 AfterCommit 回调异常），回复错误满足 `errors.Is(err, nest.ErrAfterCommitFailed)`，原因仍可 `errors.Is`；Abort、确认结果未知与拒绝的回复不带该哨兵。判断“是否已提交”请用 `errors.Is`，不要匹配错误文本（[RR-20260926-46](bugfix/RR-20260926-46.md)）；只看 `ErrAfterCommitFailed` 不够，完整顺序见 §4“回复错误判别”表（[RR-20260926-77](bugfix/RR-20260926-77.md)）。纯本地事务同样如此：strict / memory 提交成功、pipelined ticket 持久之后的 release hook、释放或回调失败都带该哨兵，结果未知不带（[RR-20260926-53](bugfix/RR-20260926-53.md)）。已越过提交点的事务，即使错误链里有 `nest.ErrLockTimeout` 也不会被框架自动重新执行（[RR-20260926-49](bugfix/RR-20260926-49.md)）；调用方也不要对带 `ErrAfterCommitFailed` 的回复重试业务。handler 内嵌套的独立事务（`nest.RunIsolatedTransaction`）一旦持久提交（或结果未知），这条消息同样按已越过提交点处理：外层随后失败不再自动重排，回复满足 `errors.Is(err, nest.ErrNestedTransactionCommitted)`（外层原因仍可 `errors.Is`），表示“外层未提交、独立事务已提交”，不要当作什么都没发生重试整笔业务（[RR-20260926-65](bugfix/RR-20260926-65.md)）。外层是可回滚事务（state / undo）时，独立事务不能持久写外层已登记回滚快照的实体（声明目标、外层 Cast / 新建的实体、外层 `MarkPersist` 过的 DAO）：外层失败回滚会把快照恢复到内存、覆盖已持久的结果。这种写在写任何持久记录之前被拒绝，`RunIsolatedTransaction` 返回满足 `errors.Is(err, nest.ErrNestedTransactionRollbackConflict)` 的错误，独立事务自身回滚、什么都没提交，原样重试仍会被拒绝——把写入并入外层事务，或只让独立事务写外层没有捕获的实体（例如在独立事务里 Cast 取得的实体）。外层是 memory handler 时不受此限（[RR-20260926-74](bugfix/RR-20260926-74.md)）。不经 DAO、用 `RollbackTx.AddMutation` 直接加入的原始 mutation 同样适用：按 `EntityID`（DocumentKey 形式按 `Key.ID`）命中外层已快照的实体时返回同一哨兵（[RR-20260927-07](bugfix/RR-20260927-07.md)）。带 Remote 批次（声明了 remote-managed 目标）的消息里不支持嵌套独立事务：`RunIsolatedTransaction` 直接返回满足 `errors.Is(err, nest.ErrNestedTransactionInRemoteMessage)` 的错误，函数体不执行、什么都没提交，Remote 批次只随消息自己的事务提交或撤销；把写入并入消息自己的事务（[RR-20260926-75](bugfix/RR-20260926-75.md)）。消息自己的事务结束后、批次收尾前的收尾阶段（Guard post-release、解锁后回调）里调用同样被拒绝；纯本地消息的收尾阶段里调用照常执行，但按嵌套独立事务处理，不认领消息（[RR-20260926-84](bugfix/RR-20260926-84.md)）。handler 内的独立事务（`RunIsolatedTransaction`，以及 memory handler 内新建事务的 `RunDetachedTransaction`）提交结果未知（`ErrCommitIndeterminate`）时，框架在它返回之前就 fence 所在的 Nest 引擎，与消息自己的事务结果未知相同：`NestMgr.FenceError()` 满足 `nest.ErrNestFenced` 与 `nest.ErrCommitIndeterminate`，此后新请求一律被拒，业务吞掉这个错误也一样（[RR-20260926-76](bugfix/RR-20260926-76.md)）。外层 handler 仍会执行到结束，但它自己的事务不再交给 committer：提交点返回满足 `errors.Is(err, nest.ErrNestFenced)` 的错误并回滚外层修改，回复同时带 `ErrNestedTransactionCommitted`（按判别表第 4 行）。旧实现在结果未知来自 DAO `AcceptMutation` 失败（WAL 已接受嵌套记录、并未进入 terminal）时，会把外层记录照常写进 WAL（[RR-20260927-06](bugfix/RR-20260927-06.md)）。
- Durability 1/2（async/strict）的 Remote 写由 WAL 投影器完成确认：投影期间后台收尾不再回源 Mongo、不再隔离实体，投影完成即释放写权限；投影器报告结果未知或超过 `remote_entity.finalize_projection_timeout`（默认 30s）后才回源。同一事务的快照只发布一次（[RR-20260926-38](bugfix/RR-20260926-38.md)）。
- Remote 写被持久拒绝后，框架在释放写权限后把持有被拒绝修改的实例从本进程内存卸载（不删持久数据，业务收到 `OnDestroy(entity.DestroyReasonMemoryUnload)`；DataEngine 驱逐被跳过的原生步骤留下的实体也改用同一原因），下一次访问从权威重新加载，无需重启。释放写权限到卸载完成之间、以及卸载过程中发起的重载，下一写者得到可 `errors.Is(err, entity.ErrRemoteEntityReloading)` 的**可重试**错误（它包裹 `ErrRemoteFenced`，既有 `errors.Is(err, entity.ErrRemoteFenced)` 判断不变；需要区分时先判断 `ErrRemoteEntityReloading`），稍后重试即得到从权威重载的新实例；仍是 `ErrRemoteFenced` 而不是该哨兵的，是真正的 fence / 隔离（例如结果未知、自定义 loader 不支持卸载），不要按短窗口重试处理（[RR-20260926-62](bugfix/RR-20260926-62.md)）。Sync 与 DataEngine 驱逐同一规则：订阅不注销、不发 remove，重载后原订阅者收到全量（kit 自动 Rebind）；不要在 `DestroyReasonMemoryUnload` 的 `OnDestroy` 里 Unregister。自定义 Remote loader 需实现 `entity.IRemoteEntityUnloader` 才有此行为（`ManagerAccess` 已实现）（[RR-20260926-39](bugfix/RR-20260926-39.md)）。〔更正：“不发 remove”只指卸载本身；仍有订阅者时框架随即重载并全量，重载不了才发 remove，见上条 RR-20260926-59。〕
- 本地已提交、而 strict 远端确认超时或 Durability 0 结果未知时，请求返回“结果未知”的错误：Durability 0 与 strict 等待中投影器报告未知时可 `errors.Is(entity.ErrRemotePersistenceIndeterminate)`，strict 等待到截止时是 `errors.Is(entity.ErrRemoteCommitTimeout)`（不带前者，见 §4 判别表第 2 行）；该事务的 Sync 放行与 `AfterCommit` 回调转交 Remote 后台收尾，拿到持久结论后执行一次：已提交则在 Nest 快池执行（时机不早于这次错误回复，可能与回复并发或在其后），被拒绝则不执行且不再冻结同实体后续提交的 Sync——只丢弃被拒绝 Remote 实体本提交的 Sync 内容与兴趣事实，同一事务里已持久提交的本地实体照常同步、其 AOI/关系事实照常生效（[RR-20260926-58](bugfix/RR-20260926-58.md)）；停机前仍无结论则不执行。延迟执行的 AfterCommit 看到原请求的上下文快照（trace、handler 元数据、请求内写入的 fctx KV），其 Base 不继承已结束请求的取消。**停机 / fence 期间提交后回调可能不执行**：持久结论到达时 Nest 已停机或已 fence，框架不会在后台 goroutine 上执行业务回调，该事务的 AfterCommit 不执行、Sync 门保持冻结（重启后实体从权威重载），计数 `remote_entity.deferred_outcome_not_run_total{outcome}` 并记告警日志；需要可靠副作用的业务应使用持久记录 / outbox，而不是依赖“结果未知”的 AfterCommit 最终执行。不要因“结果未知”在别处重复 AfterCommit 的副作用（[RR-20260926-37](bugfix/RR-20260926-37.md)、[RR-20260926-61](bugfix/RR-20260926-61.md)）。

## 2026-09-26 连接、复制会话与同步总线（RR-52/55/56）

- 生成的玩家 TCP 传输：一次推送里某条连接写失败，这条连接立即被关闭注销（客户端断线重连），其余连接收到则 `PushPlayer` 返回 nil；全部连接都写不进时返回错误且这些连接都已关闭。写之前就被拒绝（ctx 结束、payload 超限）的推送不关闭连接。依赖“任一连接失败即报错”的调用方改看 `ActiveSessions` 或会话关闭事件。已生成工程重新生成 `server_gen.go` 即可（[RR-20260926-52](bugfix/RR-20260926-52.md)）。
- entitysync 新增 `Manager.RegisterAfterRetirement(state, done)`：subject 仍在退役（Leave 之后观察者还欠 ObjectRemove）时把登记排到退役完成，不再返回 `ErrSubjectRetiring`；每个 subject 至多一个排队，再次 `Unregister`、被替换、状态关闭或 Manager 关闭时 done 收到取消。`entitysync.SessionOpenRetryable(err)` 判断 OpenSession 的“稍后重试”错误。game-demo 的 scene 用它们让同 tick 内的快速重连进入复制场景，会话打开遇“旧会话仍在关闭”按 25ms 起翻倍、上限 1s、至多 8 次重试（[RR-20260926-55](bugfix/RR-20260926-55.md)）。
- （2026-09-27）卸载后重载不了、框架退回 remove 的实体（RR-59）重新登记后，`policy.Interest` / `Group` / `Direct` 自动重新提交仍持有的订阅：观察者仍在 AOI / 组内 / 仍绑定时恢复可见（先 remove、再 create），缺席期间已离开或解绑的不恢复；自建政策用 `Manager.NewSubscriptionSourceWithResubmit` 接入，直接 `Manager.Subscribe` 的订阅不恢复（[RR-20260926-70](bugfix/RR-20260926-70.md)）。重新登记后、政策重新提交之前实体又被退回 remove 时，这些订阅在下一次重新登记后照样恢复（[RR-20260926-78](bugfix/RR-20260926-78.md)）。`policy.Direct` 的绑定在会话关闭（含传输失败）或业务 `Unregister` 该实体后随下一次 Flush 删除，不再随历史绑定数增长；会话关闭时实体恰好缺席的绑定不会在实体重新登记后落到同 ID 重开的新连接上（需要继续观看就重新 `Bind`）。自建政策可用 `Manager.NewSubscriptionSourceWithHooks` 的 `Released` 回调得到同样的通知（[RR-20260926-79](bugfix/RR-20260926-79.md)），并用 `SubscribeStamped` 返回的戳与 `ReleasedSubscription.Stamp` 比较、只删除更早的簿记：通知途中在同 ID 重开的会话或重新登记的实体上重新 `Bind`、随后又被退回 remove 的 `Direct` 绑定不再被旧通知删掉，实体重新登记后照常恢复（[RR-20260926-85](bugfix/RR-20260926-85.md)）。卸载退役中调用 `RegisterAfterRetirement` 现在返回 `queued=true` 并由 done 报告结果（[RR-20260926-72](bugfix/RR-20260926-72.md)）。同一状态在退役收尾窗口里重新登记不再丢失脏通知器（[RR-20260926-69](bugfix/RR-20260926-69.md)）。
- syncbus 的 JetStream 流名缺省由 `syncbus.prefix` 派生：`roost.sync`（生成配置）与未配置 prefix 时仍为 `ROOST_SYNC`，其他 prefix 各得其流（`zz3640.sync` → `ZZ3640_SYNC`），`syncbus.stream` 显式配置优先，启动日志 `syncbus mod: started` 输出 `prefix` 与 `stream`。**升级注意**：写了非默认 prefix 却没写 stream 的 JetStream 部署会换到派生的新流；旧 `ROOST_SYNC` 仍占着该 prefix 的 subjects 时启动报 subjects overlap——要沿用旧流与 durable 游标就写 `syncbus.stream: ROOST_SYNC`，要迁移就先确认旧流已消费完（[RR-20260926-56](bugfix/RR-20260926-56.md)）。
