# Roost 核心基建生产实施说明

本文描述当前唯一保留的生产路径。旧 `AsyncSync`、Nest `Send/Sync`、匿名回调以及 Remote Entity 旧同步链路均不再提供兼容入口。

## 1. 运行模型

- 每个应用实例持有独立的 `EntityManager`、`LockManager`、`ManagerAccess` 和 `NestMgr`。Entity release/remove hook、Remote Entity manager、frame ticker 与 group lock 都属于实例；进程级注册表只允许在启动前登记不可变的 handler/component 定义。
- Entity 由 `EntityManager.Create/CreateInScope` 创建。框架先构建 Entity、获取 Entity mutex，再发布到全局索引；业务 handler 被调用前，Nest 已完成 `Touch`、确定性排序和加锁。
- 冷实体通过 `ManagerAccess` 的 `AggregateLoader` 加载。加载器必须一次读取完整聚合、完成 schema migration 和版本向量恢复，再通过 `EntityManager` 原子发布。
- 删除期间，同一 Entity ID 处于 tombstone 状态；生命周期回调完成前禁止同 ID 重建，避免旧对象清理与新对象复用同一锁或存储身份。

## 2. Nest 与事务一致性

- 业务只使用实例化 `nest.Client`，生成的 sender 负责调用 `Dispatch/Request` 系列接口；handler 只操作已加锁的 Entity/component。
- codegen 生成的业务 handler 默认 `rollback=state durability=async`；只有显式声明 `durability=memory` 才能绕过 WAL，强一致请求使用 `durability=strict`。
- 一个 handler 的普通 Entity DAO mutation 与 Remote Entity mutation 组成同一个事务描述，由一个 Mongo transaction 原子提交。
- WAL 先持久化事务意图，再提交 Mongo，最后确认 WAL。Mongo 返回不确定提交结果时进程立即 fence，拒绝新请求并进入受控退出，恢复阶段按 transaction ID 幂等重放。
- 同步请求入队后不再读取池化 `Msg`；返回通道、请求 context 和复制到栈上的 timeout 是唯一等待状态。

生产环境要求 MongoDB 使用支持事务的 replica set/sharded cluster。所有需要原子提交的 collection 必须处于同一 Mongo database scope；跨 database 原子性不被宣称。

## 3. Save/Load 与主动 Flush

- `checkpoint.Mod` 是注册到应用 Registry 的唯一 checkpoint 能力。应用不得绕过 Mod 直接持有内部 checkpoint。
- admission 失败的 save 会进入有界 pending 集合，由后台 worker 重试；`Flush(ctx)` 同时排空 pending 与 checkpoint 队列。
- 持久删除在 Entity mutex 内先生成版本化 tombstone，并等待 Redis durable admission 成功后才从 EntityManager 移除。删除 admission 的失败结果可能不确定，因此必须保留内存 Entity、fence 进程并由 WAL replay 恢复，禁止退化为仅内存重试。
- `checkpoint.admission_pending_capacity` 是硬上限；达到上限会触发 RuntimeFailure 并 fence 进程，禁止用无限内存换可用性。
- `Stop(ctx)` 在关闭 worker 前执行最终 flush；超时或失败必须向上返回，不允许静默丢弃。
- Mongo 保存使用版本、marker epoch、lock fence 和 route epoch 做条件更新；加载按完整聚合 snapshot 恢复，不发布半初始化 Entity。
- 删除是携带严格递增 version 的持久 tombstone，不再物理删除。旧 save、旧 WAL ACK 和并发 flush 都不能复活已删除 ID；业务 ID 默认不可复用，确需重建必须提交严格更高 version。
- Mongo 启动固定 primary + majority+journal 写、transaction snapshot read，并拒绝不支持 session/transaction 的 standalone。`_nest_transactions` 由 TTL 索引限制幂等 receipt 生命周期。
- Checkpoint admission 使用同一物理 Redis 连接批量执行 Lua CAS，再以一次 `WAITAOF` 接纳整批数据，并校验 local/replica fsync 数量；生产要求 Redis 7.2+、AOF 开启和单主/Sentinel 接入。不能证明同连接同分片语义的 Redis Cluster 必须 fail-closed，不能把普通命令成功冒充持久化成功。

## 4. Remote Entity

- Read 模式只暴露不可变 snapshot，L1 使用有界进程内原子快照，L2 使用共享 snapshot store；业务读取不获取分布式锁。Cached 只读 L1/L2，Monotonic 在版本不足时对相同 key/version 单飞回源，Linearizable 始终读取权威存储；回源并发、等待者和超时均有默认硬上限。
- Write 模式先通过 Redis ownership marker CAS 获取 owner/marker epoch，再获取带 fence 的分布式锁，之后才加载和修改权威 Entity。
- 提交条件包含 `StateVersion + MarkerEpoch + LockFence + RouteEpoch`。任一维度落后都会被存储层拒绝。
- ownership 切换、shared mode 和 owner transfer 均为显式状态机；状态迁移错误必须返回，不能忽略。
- Remote wrapper 使用 `remote_entity.wrapper_capacity` 和 `remote_entity.wrapper_idle_ttl` 做有界空闲淘汰；分布式锁释放失败会进入健康检查并触发 RuntimeFailure。
- marker epoch 与 lock fence 是两个独立概念，API 中分别命名，禁止混用。

## 5. 帧同步与重同步

- replication 数据面支持 snapshot、delta、LOD/interest、分片、压缩和可靠重传；服务器可按房间以 20 Hz 驱动。
- 房间默认硬限制 100 subject/100 subscriber。`RoomManager` 同时限制房间总数、全局 subject/subscriber 预算并回收空闲房间；应用不得自行维护无上限的房间 map。
- 单个慢客户端只淘汰自己的 session；共享 transport sink 按房间分片，不让一个房间阻塞全部房间。框架生命周期回调使用固定 worker 和有界队列，退房、断线和房间销毁会释放 sequence/baseline/LOD 状态。
- UDP 控制面使用固定长度带校验的 ACK/Resync 报文，包含 room、epoch、tick 和单调 sequence。过期 epoch、回退 sequence、非法 checksum 均被拒绝。
- `cube-kit/replication.ControlPlane` 可直接接管 UDP 控制报文；业务层不解析协议。QUIC/KCP transport 只承担传输，不改变 replication 一致性语义。

## 6. Saga

跨独立事务域的流程使用 `saga` 包。生产部署必须使用具备原子状态/outbox、
lease fencing、幂等 completion receipt 的持久化 Store；完整语义见 [SAGA.md](SAGA.md)。

## 7. 发布顺序与门禁

发布顺序：

1. 发布包含 Saga contract/engine 的 `roost-core v1.4.0`。
2. 使用已发布 core 构建并发布 `roost-kit v1.4.0`。
3. 发布 `roost-codegen v1.4.0`，新项目默认引用上述两个版本。

每次发布至少执行：

```text
go test ./...
go vet ./...
go test -race ./entity ./nest ./checkpoint ./entitysync ./replication ./sync ./syncstream ./ownerroute ./etcd ./cache ./saga
go test -race ./checkpoint ./remote_entity ./nestwal ./nest ./replication ./sync ./saga
git diff --check
```

生产压测必须覆盖 20 Hz、单房间 100 Entity、目标房间并发量下的 P95/P99、UDP 丢包/乱序、Redis 重启、Mongo primary 切换和 etcd compaction。CI 负责 race/vet/单元回归；依赖真实基础设施的故障演练必须在 staging release gate 执行，不能用 fake 测试替代。

升级前必须先停止旧 writer 并排空旧 checkpoint 队列。`EntityManager.Remove/RemoveAfter` 与无返回值删除 hook 已移除，业务必须改用带 context 和 error 的 `ManagerAccess.Destroy`。旧的物理删除记录没有 version，不能混入新进程；升级后必须重新生成 Entity，使 `RemoveSnapshot` 在 Entity mutex 内递增 DAO tracker version。WAL 目录必须是单写持久卷，滚动升级时不同实例不得共享同一目录。文件型 sync history 的 WAL 与 checkpoint 会在发布提交点同步目录元数据；Windows 使用 write-through rename，Linux/Unix 使用 rename 后目录 fsync。

第二条 race 命令在 `cube-kit` 仓库执行，并使用包含本次 core/kit 的本地 `go.work`；正式发布验证应再关闭 `go.work`，只使用已发布 module 运行一次全量测试。
