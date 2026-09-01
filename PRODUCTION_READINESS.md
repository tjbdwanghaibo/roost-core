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

## 3. Data Engine：Commit/Load/Migrate/Flush

- `dataengine.Mod` 是 Registry 中唯一的 Entity 数据引擎，同时提供 lazy Nest committer、aggregate loader、migration runner 和主动 `Flush(ctx)`。
- 所有持久化字段修改必须位于 Nest transaction；低隔离入口使用 detached transaction，不能在 Entity release 时另走 after-image 保存。
- WAL admission 有界；磁盘容量、最长 unacked age 或 fsync 不确定错误会使进程 fence，禁止无限堆内存或猜测结果。
- Projector 将 Put/Patch/Delete、Remote commit、Saga receipt 与 effect staging 按需放入同一个 Mongo transaction；成功后才推进 WAL ack。
- Mongo projection 使用 expected/next version CAS。删除写版本化 tombstone，旧 Patch 和重复 replay 不能复活已删除 ID；显式重建必须提交更高 version。
- aggregate load 在一个 snapshot read transaction 中读取完整 DAO 集合，完成 schema migration 与 tracker version 恢复后才发布 Entity。
- `Stop(ctx)` 先停止接入并收敛 Projector，再停止 outbox claim；未发布 effect 已经在 Mongo 持久化，下次启动继续投递。
- Mongo 必须支持 session/transaction；写 concern 为 majority+journal。transaction/effect receipt TTL 必须长于对应 WAL/stream 的重放窗口。

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

当前已发布运行时基线为 `cube-core v1.8.0`、`cube-kit v1.8.0`、`roost-skill v1.7.0`。本轮生产部署和三级文档生成属于 `roost-codegen v1.7.0`，发布后新项目固定这一组合。

发布顺序：

1. 发布包含公共契约变更的 `cube-core`。
2. 关闭 `go.work`，只使用已发布 core 构建、验证并发布 `cube-kit`。
3. 使用已发布 core/kit 验证并发布 `roost-skill`（若本轮有变更）。
4. 最后发布 `roost-codegen`，并用 release defaults 生成一个全新项目，执行 `GOWORK=off go mod tidy/verify/test/vet` 与部署模板检查。

每次发布至少执行：

```text
go test ./...
go vet ./...
go test -race ./dataengine ./entity ./nest ./entitysync ./replication ./sync ./syncstream ./ownerroute ./etcd ./cache ./saga
go test -race ./dataengine ./remote_entity ./nestwal ./nest ./replication ./sync ./saga
git diff --check
```

生成项目还必须校验 `deploy/shell/*.sh` 可通过 `sh -n`、全部 Kubernetes YAML 可解析、镜像不包含配置、生产 Secret 示例不被 kustomization 自动应用。正式流程见 [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)。

生产压测必须覆盖 20 Hz、单房间 100 Entity、目标房间并发量下的 P95/P99、UDP 丢包/乱序、Redis 重启、Mongo primary 切换和 etcd compaction。CI 负责 race/vet/单元回归；依赖真实基础设施的故障演练必须在 staging release gate 执行，不能用 fake 测试替代。

从历史快照引擎升级前必须停止旧 writer、排空全部 backlog，并按 [docs/DATA_ENGINE_MIGRATION.md](docs/DATA_ENGINE_MIGRATION.md) 执行一次性数据审计。业务销毁统一使用带 context 和 error 的 `ManagerAccess.Destroy`。WAL 目录必须是单写持久卷，滚动升级时不同实例不得共享同一目录。`nestwal` ack checkpoint 与 `syncstream` 文件 checkpoint 只是各自日志的消费 watermark，不构成 Entity 第二写路径。

第二条 race 命令在 `cube-kit` 仓库执行，并使用包含本次 core/kit 的本地 `go.work`；正式发布验证应再关闭 `go.work`，只使用已发布 module 运行一次全量测试。
