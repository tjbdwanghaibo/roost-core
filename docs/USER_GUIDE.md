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
| 状态同步 | player 接入层或 nettransport 作为 `entitysync.Transport`；业务装 `entitysync.Manager`，`policy.Interest / Group / Direct` 作为订阅政策 |
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
- `pipelined`：高吞吐 group commit；外部可见结果仍受 durable watermark 约束。

结果不确定时框架 fence 实例，不进行猜测性回滚。业务必须把“服务暂不可用”和“业务失败”分成不同错误码。

## 5. Commit、Load 与主动 Flush

Data Engine 是唯一保存入口。字段变化先进入当前 Nest transaction，再作为版本化 Put/Patch/Delete 写入 WAL；Mongo version CAS 防止旧写覆盖新状态。WAL/Projector backlog 有硬容量和年龄上限，超过门禁触发 runtime failure，而不是无限堆内存。

主动刷盘使用 Registry 中的 Data Engine 能力调用 `Flush(ctx)`。典型时机：停机、迁服、运维检查和版本升级。不要为每个普通请求 Flush，否则会破坏 group commit/批 projection 吞吐；需要强确认的业务选择 Nest strict durability。

Load 只接受完整聚合快照。迁移函数必须幂等、可测试并携带 schema version；加载失败不允许生成“空玩家”覆盖旧数据。

## 6. Remote Entity

Read 模式返回不可变 snapshot：L1 是进程内有界原子缓存，L2 是共享 snapshot store。`Cached` 不回源，`Monotonic` 在版本不足时 singleflight 回源，`Linearizable` 每次读权威存储。高频展示、排行榜引用和 AOI 属性优先 Cached/Monotonic；结算前校验使用 Linearizable 或转成 owner 命令。

Write 模式使用 Mongo 持久所有权；共享写先竞争 Redis 协调锁，再取得 Mongo majority 写许可，owner-routed 写也取得持久许可，然后权威加载 → Nest 事务修改 → 条件提交。存储条件同时包含 StateVersion、MarkerEpoch、LockFence、RouteEpoch。分布式锁只避免同时进入临界区，四维 fence 才能拒绝暂停后恢复的旧 owner 或旧路由写入。

正式 Assembly 要求持久权威能力；MongoCommitter 创建即强制校验许可，WriteAuthority() 只返回能力，没有弱校验开关。当前未部署，不提供旧协议迁移入口；不支持的元数据拒绝启动，见 [提交契约](bugfix/RR-20260925-02.md)。

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

Kit 对应 `nest.fast.workers`、`nest.fast.queue_capacity`、`nest.slow.workers`、`nest.slow.queue_capacity`。容量是整池等待总数，包括等待 ID 前驱的消息，排除执行中的请求和已预留执行额度的就绪请求；内部快延续单独计数。默认快并发 GOMAXPROCS、容量 10000；慢并发 max(32, 快并发×4)、容量 64。旧 `worker_num/queue_capacity/remote_workers` 仍作为未设置新值时的来源，`heartbeat_worker_num` 不再创建单独池，旧 `SendOptionIsCost()` 等同 `SendOptionSlow()`。冷目标不再需要手工加 Slow：Nest 在统一准入时对 Single/Multi/MultiGroup 的声明目标做一次只读内存判定（Getter 的 LoadedEntitiesOnly 契约），有未加载且可由 loader 加载的目标就走慢阶段预加载，业务代码和生成 sender 不变；Broadcast 仍按已加载目标尽力扇出，冷目标逐个报告。显式 Slow 仍有效，用于强制慢准备。handler 内部阻塞 RPC 必须显式拆为前置 I/O，无法自动迁移。

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
未声明的动态 Cast 目标保持只读已加载实体（返回 `ErrColdLoadInLogic`）。自定义 Getter 需遵守 `entity.LoadedEntitiesOnly(ctx)`：
准入判定就是用这条只读路径，违反契约的 Getter 会在调用方 goroutine 上做 I/O。
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
- 关闭超时不代表 WAL 目录已释放；等待在途调用退出后再次 Close。`OpenRuntime` 始终关闭自己创建的 WAL。
- `Projector.Close` 之后 `Flush` / `ReplayPass` 返回 `ErrRuntimeStopped`，不能再用“先 Close 停后台循环再手动回放”的写法；需要逐步驱动回放的外部夹具用 `ProjectorOptions.ManualReplay`（不启动后台循环，生产装配不设置），见 [RR-20260926-29](bugfix/RR-20260926-29.md)。
- `ProjectorOptions.OnFatal` 在首次确定性投影冲突后**异步**调用一次；调用前 fatal 已对准入、Flush 和实体等待方可见，回调里可以同步 Close/Shutdown。依赖“Flush 返回前回调已执行完”的代码需改为等待回调，见 [RR-20260926-17 补修](bugfix/RR-20260926-17.md)。

细节与验证边界见 [RR-10～24 修复汇总](review/REVIEW-2026-09-26-release-fixes.md)。
