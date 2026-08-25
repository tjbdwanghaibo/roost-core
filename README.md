# cube-core

[English](#english) | [中文](#cube-core-中文说明)

`cube-core` 是 Cube 游戏服务端的通用运行时。它定义应用生命周期、能力注册、Entity / Component、Nest Actor 调度、事务与 WAL 持久化、状态同步、Saga 编排、缓存、可观测性和基础安全能力；它不包含具体玩法、玩家协议或某个中间件的连接实现。

## cube-core 中文说明

### 仓库关系

```text
cube (业务服务、玩法、协议和部署)
  ├── cube-core (通用运行时与抽象)
  └── cube-kit  (Redis、Mongo、NATS、etcd 等 Mod 实现)
             └── cube-core
```

业务代码优先依赖本仓库暴露的接口和稳定类型；具体 Redis、Mongo、NATS、etcd 客户端由 `cube-kit` 提供。这样业务服务可以替换基础设施实现，而不把连接细节扩散到玩法代码中。

### 设计哲学

整个仓库围绕四条不变量构建，所有模块的失败语义都从这里推导：

1. **成功不可先于 commit point 被观察。** 任何"已完成"的对外承诺（返回值、outbox、同步分发）都必须晚于对应的持久化 admission。
2. **结果不确定时 fence，而不是猜。** fsync / 网络写的结果不确定（`ErrCommitIndeterminate`）时不做内存回滚——那会制造与 WAL 已提交历史冲突的第二条历史——而是放弃事务、熔断进程，由新进程 WAL replay 判定权威结果。
3. **删除防复活。** 版本化 tombstone 语义在内存去重、Redis Lua 脚本、WAL 回放三层保持一致：同版本 delete 胜出，复活必须携带严格更高的版本。
4. **接纳即执行。** 有界队列一旦受理任务（返回 true），任务必然被执行或被显式释放，不存在"接受了但悄悄丢掉"的窗口。

### 能力边界

| 领域 | 包 | 责任 |
| --- | --- | --- |
| 应用装配 | `app` | `Mod` / `Service` 生命周期、CLI、配置校验、`Registry`、运行期故障 fail-stop |
| 实体运行时 | `entity`、`nest`、`replica`、`ownerroute` | Entity / Component、串行 Actor、可重入实体锁、锁序死锁预防、跨实体投递、副本与所有权路由 |
| 事务与持久化 | `nest`（rollback/WAL）、`checkpoint`、`migration` | 内存事务 + undo、WAL commit point、journal + 批量刷盘、版本化 tombstone、数据版本迁移 |
| 状态同步 | `entitysync`、`sync`、`syncstream`、`replication` | dirty 掩码同步、subject prepare/commit、有序状态流、delta + LOD 房间复制 |
| 分布式编排 | `saga`、`bus`、`event`、`taskflow` | Saga 状态机 + outbox、服务间消息 / RPC、事件分发、任务流契约 |
| 通用连接抽象 | `cache`、`mongo`、`redis`、`nats`、`etcd`、`httpclient`、`httpserver` | 面向业务的接口、类型和通用辅助逻辑；不负责生产连接装配 |
| 平台能力 | `health`、`obs`、`log`、`admin`、`lifecycle`、`security`、`failurelog`、`featureflag`、`hotcode` | 健康检查、指标、日志、管理命令、生命周期 hook、会话令牌、故障记录、灰度开关、热修补 |
| 接入契约 | `gateway`、`webroute`、`errcode`、`configdata` | 协议无关的请求边界、生成路由运行时、错误码、配置表快照 |
| 调度与工具 | `worker`、`timer`、`clock`、`lock`、`map`、`query`、`ctx`、`misc` | worker pool、时间任务、逻辑时间、锁原语、容器与索引、请求上下文、GoID |

包可以依赖标准库、稳定第三方库和其他 core 抽象；不得引入 `player`、`alliance`、`activity` 等玩法语义，也不得直接依赖 `cube` 的业务包。

### 安装

发布版本可用后，业务模块按需引用：

```bash
go get github.com/tjbdwanghaibo/cube-core@latest
```

开发 Cube 三仓库时，在共同父目录创建临时工作区，不要将此 `go.work` 提交到任一仓库：

```bash
cd /path/to/workspace
go work init ./cube ./cube-core ./cube-kit
```

### 应用生命周期

`app.App` 统一管理配置与启动顺序。`Mod` 是基础能力提供者，`Service` 是业务入口。

```text
Mod.Init -> Mod.Provide -> Mod.Start -> Service.Init -> Service.Serve
                                                     -> Service.Shutdown
Mod.Stop (按注册逆序)
```

- `Init` 只读取配置、创建对象，不启动后台任务。
- `Provide` 将能力注册到 `app.Registry`；同名 capability 会失败，避免静默覆盖。
- `Start` 建立外部连接、注册健康检查并启动后台任务。启动失败时已完成的 Mod 按逆序精确回滚。
- `Stop` 必须释放资源；服务退出时按逆序执行。shutdown 超时时故意不再继续停 Mod（写明理由），避免半停状态下的未定义行为。
- `Service.Serve` 阻塞直到上层 context 被取消，`Shutdown` 处理服务自身的优雅收敛。
- `app/runtime_failure` 提供基础设施不可安全写入时 first-error-wins 的统一 fail-stop；`app/config_validation` 在 `env=prod` 下强制生产门禁（非 dev 密钥、WAL durable 等）。

最小装配形态如下。中间件 Mod 位于 `cube-kit`，业务 `Service` 由应用自行实现。

```go
application := app.New("game", "v1.0.0").
    Mods(sharedMods...).
    RegisterServer("game", gameService, gameOnlyMods...)

if err := application.Execute(); err != nil {
    panic(err)
}
```

服务从 `Registry` 使用类型安全的 capability，而不是引用全局单例：

```go
client, ok := app.Lookup[MyClient](registry, "my_client")
if !ok {
    return errors.New("my_client capability is unavailable")
}
_ = client
```

Entity 生命周期同样使用服务拥有的实例。启动阶段为 manager 配置由 Redis/etcd
分配 block 的 `entity.IDGen.Generate`，随后把 `ManagerAccess` 同时注入 Nest、
Remote Entity 和业务 Service；创建、销毁、分组索引都不会经过全局 manager：

```go
manager := entity.NewEntityManager()
idGen, err := entity.NewIDGen(initialBlock, acquireNextBlock)
if err != nil { return err }
if err := manager.ConfigureIDGenerator(idGen.Generate); err != nil { return err }
entities := entity.NewManagerAccess(manager)
```

### 执行模型：Nest Actor 与锁

完整模型见 [Roost 业务执行模型](RUNTIME_EXECUTION_MODEL.md)。要点：

- **串行调度。** 消息按实体 ID 哈希到固定 worker（`worker.Pool`），同一实体的处理天然串行。`nest.NewEngine` / `nest.Client` 是唯一入口，不再新增业务 Runtime 门面。
- **实体锁是停车等待的可重入锁**（`lock.ReentrantMutex`）。等待者停在信号量 channel 上而非自旋，持有者做慢操作（例如 WAL 提交）时等待者不烧 CPU；channel 接收顺序提供近似 FIFO 公平性。可重入语义基于 goroutine ID。
- **死锁预防是体系化设计**：全局锁序（Remote → Player → Alliance → Other）+ 批内确定性排序加锁 + handler 内 cast 的加载前预检 + 已持锁时降级 TryLock + 锁冲突的有界延迟重排队（带指标）。API 层直接禁止 handler 内同步跨实体调用（panic + 明确错误）；跨实体写使用 cast。
- **goroutine ID 是框架上下文的载体。** guard 作用域、请求上下文（`ctx`）、锁可重入性都挂在当前 goroutine 上。因此 **handler 内严禁裸 `go func(){...}` 后再访问框架能力**——新 goroutine 会静默丢失事务上下文、guard 与锁可重入性。需要异步工作时使用 `worker.Pool.Go` 或把工作投递回 Nest。
- `lock.LockManager.ReleaseLock` 与 `GetLock` 有明确契约：锁实例可能被并发释放并重建，因此 **拿到锁后必须重验受保护状态**（entity 的 `IsRemoved`/`IsClear` 与索引成员资格），发现状态已消失就退让。释放方必须先在持锁状态下使状态不可达（摘索引、置 removed）再释放锁与锁实例。

### 事务、WAL 与 commit point

完整语义见 [Nest Transaction WAL](NEST_TRANSACTION_WAL.md)。要点：

- Handler 声明 `Rollback` 与 `Durability` 元数据；`RollbackTx` 捕获实体 after-image，handler 失败时按 undo 恢复内存。
- **commit point 在锁内**（`DurabilityStrict`）：`durableCommit` 通过 `TransactionCommitter`（WAL group-commit + fsync）完成 admission，成功后才解锁、才执行 `AfterCommit`。数据库、RPC、消息发布等外部副作用必须走 `nest.AfterCommit`、outbox 或独立 service 流程，不能直接写在 handler 里绕开提交点。
- **`DurabilityPipelined`**（见 [Nest Pipelined Commit](NEST_PIPELINED_COMMIT.md)）：锁内只做到"日志已入队（拿到 LSN）"，`Enqueue` 是唯一拒绝点；fsync 在锁外等待，回包与 `AfterCommit` 在 durable 后执行。级联脏读由 WAL 的前缀持久性保证安全；未落盘状态由外化闸门（entitysync 水位线、checkpoint 水位线）挡在进程内。要求 committer 实现 `PipelinedTransactionCommitter`，否则派发返回 `ErrPipelinedCommitterRequired`；跨服写批与 broadcast 路径继续走锁内提交。
- **`ErrCommitIndeterminate` 不回滚**：内存状态保持事务后形态、事务 abandon、进程 fence，由新进程 WAL replay 判定该事务是否成为历史。这是设计哲学第 2 条的直接体现。
- 持久删除在实体锁内先做 durable admission（tombstone），admission 成功才把实体从内存移除——删除的"成功"同样不可先于 commit point 被观察。

### checkpoint：journal 与批量刷盘

`checkpoint` 提供 after-image 持久化管道：`DirtyTracker`（原子 Swap/OR 的无锁脏位，天然免 ABA）→ 有界 `Journal`（背压）→ `Flusher`（按 (collection, id) 去重合并、版本化 CAS 写入后端）。

- **WAL 三档**：`SnapshotWALModeAsync`（尽力）、`SnapshotWALRequired`（WAL 拒绝则拒绝提交）、`SnapshotWALModeDurable`（fsync 确认后才受理）。durable 模式先 WAL 后 journal；journal 满时因数据已持久化而返回成功并回滚脏位，等待重试或回放。
- **启动即回放**：`Start` 强制先完成 WAL replay 再接受实时 flush，回放失败拒绝启动。
- **删除防复活**：删除先写 tombstone、后进 journal，后端 `BulkRemove` 成功后才 ACK tombstone；Redis WAL 的 Lua 脚本用十进制字符串比较规避 Lua number 53-bit 精度陷阱，ack 用期望 token 栅栏（`version:fence:sid`）CAS 删除，迟到的旧 ack 无法误删更新记录。
- **并发 Flush 安全**：刷盘进度通过条件变量广播，任意数量的并发 `Flush` 调用者都会观察到完成事件；`FlushWorkers: 0` 是受支持的模式（无后台 worker，journal 仅由显式 `Flush` 驱动）。
- **配置消毒**：非法的数值配置（如 `FlushInterval <= 0`）在 `New` 时被钳制为默认值并告警，不会在 worker goroutine 里 panic。
- Redis WAL 的 production adapter 必须实现 `redis.DurableEvaler` / `redis.DurableBatchEvaler`：Lua 写入和 `WAITAOF` 固定在同一物理连接并校验 fsync 确认数量；`RequireAOF` 打开时缺少该能力不会退化为普通 ACK。

### 状态同步

**Entity Sync（`entitysync` + `entity.SubjectSyncState`）**，完整契约见 [Entity Sync](ENTITY_SYNC.md)：

- 只保留 observer-free 链路：`EntityBase.Sync()` 保存内容状态，`SubscriptionCoordinator` 保存订阅关系，业务 packer 在 Entity mutex 内生成不可变 `FrozenSyncPayload`。
- Subject 同步采用 prepare/commit 两阶段：`Prepare` 捕获版本与 dirty 掩码，`ReliableEnvelopeSink` 原子接收完整 envelope batch，只有 admission 成功后才提交 version、清除 dirty；失败保留 dirty，可安全重试。
- **in-flight 逃生门**：被丢弃（未 Commit/Abort）的 prepare 在超过 stale 阈值后被下一次 `Prepare` 回收并记录错误日志；被回收的旧 prepare 迟到提交会因 token 校验失败得到 `ErrSubjectSyncStalePrepare`，不会与新 prepare 竞争。
- 定时同步和主动 flush 都调用 `SubscriptionCoordinator.FlushSubject`，业务层不接触 Entity mutex，也不持有 observer/session/history 状态。

**实时复制（`replication`）**：Quake3 风格 acked-baseline delta + LOD 的房间状态数据面。

- `LODProjector` 在不可变 Snapshot 上执行 per-session Object LOD，不读取或锁定业务 Entity；支持八个细节等级、`LODCulled` 裁剪、按组件 LOD mask / 优先级 / 最大更新频率裁剪，非采样帧沿用上次已发送组件值避免错误的 ComponentRemove。可见性 Projector 作为 `LODProjectorConfig.Upstream` 先消除无权数据。
- 发送状态采用 prepare/commit 事务：`SendLatest` 在完整 datagram batch 被 transport 接收后自动提交；自定义发送链路必须 `PrepareLatest` → 成功 `Commit()` / 失败 `Abort()`。generation 守卫（ForceFull / 换传输 / Resync 都会 bump）保证过期投影构建的帧不可能被提交为 delta 基线。`BuildLatest` 仅用于无副作用预览。
- 帧序列号与控制消息去重使用 serial number arithmetic（RFC 1982），长会话序列号回绕后仍正确接受新帧。
- 分片重组（`Reassembler`）除全局 inflight 上限外还有 **per-session 上限**（`Limits.MaxInflightFramesPerSession`），单个会话发送永不补齐的首分片无法占满共享表、饿死其他会话。网关共享一个 Reassembler 时必须使用 `PushFor`；每客户端独立 Reassembler 可用 `Push`。
- `ObjectRef{ID, Generation}` 解决对象 ID 复用的 ABA 问题；解码面对不可信输入有长度与容量的纵深防御。

**有序状态流（`syncstream`）**：领域无关的分片、压缩、确认发布流，可选 WAL 日志。

### Saga 编排

`saga` 提供存储无关的多域业务操作状态机（Store 与消息实现在 cube-kit），完整说明见 [SAGA](SAGA.md)：

- `NewEngine` 在构造期按 `(LeaseDuration − StoreTimeout) / Batch` 静态校验租约预算，拒绝任何"处理中租约过期 → 双主并发"的配置组合。
- 精确意图幂等：Create 撞 `ErrAlreadyExists` 时只有 ID/版本/Data/Deadline 全部相等才返回既有记录，否则 `ErrIdentityConflict`。
- Coordinator 是唯一时间权威：强制覆盖 `completion.CompletedAt`，远端时钟无法把状态推回过去或提前过期去重 receipt。
- **Resume 开启新 incarnation**：`Record.Incarnation` 在每次 Resume 时递增并折入 `CommandID`，恢复后的命令标识与故障前生命周期的 completion receipt 永不碰撞；incarnation 0 保持历史格式，升级前的在途记录不受影响。
- Nest 集成（`saga/nest.go`）的 effect ID 取规范化 payload 的 sha256，业务键复用不同 payload 的编程错误不会被消息去重掩盖。

### 服务间消息（bus）

`bus` 在 NATS 之上提供模块级消息、RPC（轻量 request/reply 与 JetStream 可靠两种）、可靠消费（inbox 去重 + 死信）：

- **Bus 不可重启。** `Stop` 会拆除包括 `HandleRpc` 注册的全部订阅，而 `Start` 只重建基础 subject，重启会静默丢失 RPC 订阅——因此 `Start` 在 Stop 之后直接返回错误；需要新实例就重新 `New`。Start 失败后的重试不受影响。
- pending RPC 调用的回收遵循单一所有权：谁 `LoadAndDelete` 成功谁独占该调用的 channel，响应路径与停机清扫不会同时触碰同一 channel。
- 可靠消费要求 completion receipt TTL 大于 JetStream max age，否则 receipt 先于消息过期会破坏幂等（kit 侧装配时校验）。

### Remote Entity 与所有权路由

跨服实体写的唯一生产协议、Mongo fenced commit、Nest WAL/outbox、L1/L2 snapshot 与恢复边界见 [Remote Entity](REMOTE_ENTITY.md)。core 侧提供 `ownerroute`（路由 epoch）、`replica`（副本 payload 编解码）与 entity ownership 元数据（fence、owner sid）；写提交需同时满足 ownership marker、lock fence、route epoch 等提交条件。

### 并发与一致性约束（速查）

- Entity 状态修改在 Nest handler 或明确的 entity guard 内完成，避免绕过 Actor 串行模型直接写指针。
- 不能在 Nest handler 内进行同步跨实体调用；跨实体写使用 cast，必须等待结果时把同步调用放到 handler 外层。
- Handler 内不要裸起 goroutine 再访问框架上下文（事务、guard、可重入锁都挂在 goroutine ID 上，会静默丢失）。
- 数据库、RPC、消息发布等外部副作用使用 `nest.AfterCommit`、outbox 或独立 service 流程，不能在锁内执行。
- 从 `LockManager` 拿到的锁在加锁后必须重验受保护状态（见执行模型一节的 ReleaseLock 契约）。
- `worker.Worker.TryCast` 返回 true 即保证任务被执行且 `OnRelease` 被调用（接纳即执行）；`Pool.Go` 在池运行期间启动的 goroutine 会被 `StopWithContext` 等待。
- 长期状态必须有可恢复的 source；内存 map 只能作为缓存或索引。

### 日志与可观测性

`log` 基于标准库 `slog`，支持 JSON、文件轮转、服务标识、逻辑帧和调用点输出，并自动注入 goId / 逻辑帧 / player 上下文。应用配置中启用调用点：

```yaml
log:
  level: info
  json: false
  caller: true
```

`health`、`obs` 和 `admin` 在 `app.Registry` 创建时即注册。具体 Mod 应在 `Start` 阶段把外部依赖的健康检查和指标接入这些能力。Nest 自带慢派发看门狗（超阈值触发全栈 trace）、按 trace 标记的逐事件指标、队列与延迟消息 gauge。

### etcd Watch 回调契约

`etcd.IWatcher` 保留 channel 形式，供需要自行控制背压的底层代码使用；`etcd.WatchCallback` 将一个 watcher 交给单个有序回调消费，并通过 `IWatchSubscription` 暴露 `Done/Err/CloseWithContext`。回调返回错误或 panic 时只终止该订阅，错误可由 `Err()` 观测。

类型化本地镜像通过 `ILocalMirrorSubscriber.Subscribe` 提供更适合业务的回调：默认先发送完整 Snapshot，随后发送 Put/Delete，断线重建时再次发送 Snapshot。实现必须保证订阅注册与初始快照原子化、回调在镜像锁外运行，并用有界队列隔离慢订阅者。

### 热修补（hotcode）

`hotcode` 提供预埋补丁点 + Go plugin 的运行期修补，配套 admin 命令（list/load/rollback）。Go plugin 不可卸载，补丁点必须预先埋设；`hotcode.load_plugin` 等价于代码执行入口，生产环境必须限制 admin 通道的访问面。

### 开发与验证

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

CI 在 Linux 与 Windows 上运行完整测试矩阵，核心包开启 `-race`。修改公开接口时，请同时检查：生命周期是否可停止、是否需要 health/metrics、是否泄漏业务语义、以及是否能在没有具体中间件的测试环境中被替换。修复并发/一致性缺陷时必须附带能复现原缺陷的回归测试（现有测试中有大量此类样例可参考）。

### 文档索引

| 文档 | 内容 |
| --- | --- |
| [RUNTIME_EXECUTION_MODEL.md](RUNTIME_EXECUTION_MODEL.md) | 业务执行模型、锁边界、兼容迁移 |
| [NEST_TRANSACTION_WAL.md](NEST_TRANSACTION_WAL.md) | 内存事务、WAL commit point、indeterminate 语义 |
| [NEST_PIPELINED_COMMIT.md](NEST_PIPELINED_COMMIT.md) | Pipelined 提交：锁外 fsync、外化闸门、灰度与验收 |
| [ENTITY_SYNC.md](ENTITY_SYNC.md) | Entity 同步契约、prepare/commit、订阅协调 |
| [REMOTE_ENTITY.md](REMOTE_ENTITY.md) | 跨服实体写协议、fenced commit、恢复边界 |
| [SAGA.md](SAGA.md) | Saga 状态机、outbox、幂等与补偿 |
| [PRODUCTION_READINESS.md](PRODUCTION_READINESS.md) | 生产部署门禁与检查清单 |

### 许可证

本仓库随 Cube 项目以 [MIT License](LICENSE) 发布。

## English

`cube-core` is the generic runtime for Cube game services. It provides application lifecycle management, typed capability registration, entity/Nest actor scheduling with ordered locking, in-memory transactions with a WAL commit point (indeterminate outcomes fence the process and defer to WAL replay), checkpoint persistence with versioned anti-resurrection tombstones, subject/state synchronization, delta+LOD room replication, saga orchestration with resume incarnations, and infrastructure abstractions. Gameplay and concrete middleware clients belong in [`cube`](https://github.com/tjbdwanghaibo/cube) and [`cube-kit`](https://github.com/tjbdwanghaibo/cube-kit).

Key invariants: success is never observable before its commit point; indeterminate durability fences instead of guessing; deletes cannot be resurrected by stale saves; bounded queues execute every task they admit. See the document index above for the full contracts.

```bash
go get github.com/tjbdwanghaibo/cube-core@latest
```
