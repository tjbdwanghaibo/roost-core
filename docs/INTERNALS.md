# Roost 实现原理与不变量

本文是第三级文档，描述当前唯一生产实现。代码是最终事实；专题文档给出状态机和恢复细节。

## 1. 分层与所有权

| 层 | 拥有什么 | 不拥有什么 |
| --- | --- | --- |
| core | 生命周期、Registry、Entity/Nest/checkpoint 契约与算法、同步协议语义、Saga engine、观测和安全原语 | Mongo/NATS/QUIC 等具体连接装配 |
| kit | 中间件 Mod、WAL/store、网络 transport、AI/taskflow/spatial 执行器 | 业务 Entity、协议认证、玩法规则 |
| skill | 技能 Program/Runtime、确定性数值与宿主契约 | Entity 锁、网络会话、数据库事务 |
| codegen | 项目声明、DAO/Entity/Nest/协议/配置/部署生成 | 生产 Secret、业务容量决策 |

跨层依赖只能向下。core 接口不 import kit；业务通过 Registry capability 或生成接口依赖抽象。

## 2. 生命周期图

App 先收集 shared Mods 与 Service Mods，分别按硬依赖和“存在时排序”的可选依赖构建 DAG。缺失硬依赖、重复名称和依赖环在初始化前失败。生命周期按拓扑顺序 Init/Provide/Start，停机逆序执行；实现 `StopWithContext` 的 Mod 接收 App 的统一 shutdown deadline。

Service 收到取消后先停止接流量并完成 `Shutdown(ctx)`。如果 Serve 不配合退出，App 不提前销毁依赖，防止仍运行的业务访问已关闭连接。RuntimeFailure 只记录第一个根因并唤醒同一停机流程。

重要边界：`Stop()` 是兼容 `app.Mod` 的无参入口，生产 App 总是优先调用 `StopWithContext`。任何新 Mod 有 flush/drain 行为时都必须实现后者。

## 3. Entity 发布与锁

EntityManager 维护 ID→Entity 的实例索引和删除 tombstone。创建顺序为构造聚合、绑定组件/DAO、完成 load/migration、获取并初始化锁相关状态、最后原子发布。删除在锁内生成更高版本 tombstone，durable admission 成功后才从索引移除。

Nest 对一个请求将所有 touched Entity ID 去重并按全局稳定顺序加锁，从而避免多实体请求 AB/BA 死锁。业务 handler 运行时已经持锁，生成的 guard 限定引用生命周期。Entity 内部 mutex 是数据 race 的最后保护；worker 哈希串行是调度优化，不能替代 mutex，因为迁移、管理命令或多 key 请求可能跨 worker。

## 4. Nest 事务状态机

```text
admit request
  -> resolve/touch entities
  -> sort + lock
  -> capture state or open undo log
  -> invoke handler
  -> prepare DAO patches + remote participants + effects
  -> WAL admission / group commit
  -> Mongo conditional transaction
  -> publish result/outbox/sync visibility
  -> unlock
```

错误发生在 commit point 前时执行 state/undo 回滚并恢复 dirty snapshot。commit 结果确定失败时可返回错误；结果不确定时不能回滚内存后继续服务，因为 WAL/Mongo 可能已经成功。此时事务进入 indeterminate，实例 fence 并退出，由 replay 使用 transaction ID 和 receipt 决定历史。

Pipelined commit 把 fsync 摊销到一批事务，但每个事务仍有 admission 序号和 durable watermark。业务结果、checkpoint 外化和同步可见性不得越过 watermark。

## 5. WAL 与恢复

kit/nestwal 使用分段 append-only WAL。记录有长度、版本和校验，尾部 torn write 在启动时截断；ack fence 表示已完成外部提交的连续前缀。段轮换、目录 fsync、容量和 replay 并发都有界。

恢复流程先打开并验证 WAL，重放未确认事务到幂等 Mongo transaction/outbox，再启动 Nest 接流量。WAL 目录只能由一个 writer 使用。文件系统、PVC 或挂载无法提供 write ordering/fsync 语义时，框架不能宣称 strict durability。

## 6. Checkpoint 与 Save/Load

DirtyTracker 用字段位掩码区分 persist 与 sync dirty。Journal 接收不可变 patch，Flusher 按 key/version 合并批次。Redis 通过 Lua CAS 拒绝旧版本，并在同连接执行 WAITAOF 作为 durable admission；Mongo 用 version/epoch/fence 条件更新权威文档。

pending admission、batch、journal、并发 flush 都有硬上限。主动 `Flush(ctx)` 与后台 flush 走同一条路径，因此不会产生“主动刷盘绕开版本检查”的第二套语义。Stop 在 worker 关闭前 flush；超时向上返回。

Load 必须恢复 Entity 聚合、DirtyTracker version、删除状态和 Remote marker/route 版本。Schema migration 在发布 Entity 前运行。配置或数据不兼容时 fail closed，不能用零值覆盖。

## 7. Remote Entity 四维 fencing

四个维度解决不同竞争：

- StateVersion：拒绝旧状态覆盖新状态。
- MarkerEpoch：拒绝旧 ownership incarnation。
- LockFence：即使旧持锁者暂停后恢复，也不能提交。
- RouteEpoch：拒绝拓扑切换前的路由命令。

分布式锁只保证租约存活期间的互斥，不能单独处理 GC pause、网络隔离和锁过期后的旧进程恢复；因此存储条件必须保留四维。Write wrapper 把 Remote participant 合并进 Nest transaction description，允许同一 Mongo database scope 内原子提交。跨 database 使用 Saga。

读路径把可变权威对象转换为不可变 snapshot。L1 用 atomic publication 和容量/TTL 淘汰；L2 通过 `(marker, route, version)` CAS 分发。Monotonic 回源按 key+最低版本 singleflight，并限制回源并发、等待者和 deadline，避免热点 miss 放大。

## 8. Saga 一致性模型

Saga store 原子保存实例状态与 outbox。Coordinator 通过 lease 获取带 fence 的执行权，只处理匹配 incarnation/fence 的结果。步骤消息有稳定 ID，consumer inbox/receipt 先去重；状态推进与下一条 outbox 同事务提交。崩溃后其他 coordinator 重新获得 lease 并重放未确认 outbox。

补偿按已成功步骤的逆业务顺序执行，但不等价于数据库回滚。不可补偿外部动作必须采用预留/确认、幂等 API 或人工仲裁状态。Resume 创建新 incarnation，旧结果不能推进新执行。

## 9. 状态同步链路

Entity mutation 先产生 sync dirty，再由 entitysync 管理 observer/interest。prepare 阶段构建将要发布的不可变 payload，commit 后才推进版本和 baseline，发送失败不会把未送达数据错误标为已确认。

syncstream 提供有序版本流、分片重组、checksum、ACK 和 resync；replication 提供房间 tick、全局 snapshot、delta、LOD、baseline 和传输分类。可靠通道承载基线、关键事件和重同步；latest-only datagram 丢弃过时帧。ControlPlane 校验 room/epoch/tick/sequence/checksum，旧 epoch 与回退序号直接拒绝。

RoomManager 对房间数、subject、subscriber、session 队列和空闲时间施加上限。共享 worker 只做有界工作，慢消费者按 session 驱逐，不能阻塞整个房间或全局 transport。

## 10. Lockstep 链路

Sequencer 收集玩家输入，在 tick 截止时锁定帧并广播最近 N 帧冗余包。历史按容量保存，重连客户端通过可靠通道分页追帧。关键帧 hash 由 quorum 裁决，仅在离群集合变化时触发回调。框架不解释输入 payload，也不运行模拟；确定性、反作弊输入验证和战斗规则属于客户端/业务共享逻辑。

## 11. 技能运行时

Skill compiler 严格解析配置，验证引用和预算，输出不可变 Program。Runtime 使用确定性数值和显式 RNG/state，执行结果分为 StateMutation、PresentationEvent、checkpoint。HostAdapter 是进入 Entity 世界的窄接口：调用时必须已持实体锁，提交仍服从 Nest commit point。技能 VM 不能直接获取数据库、网络或全局可变单例。

## 12. 观测与失败语义

健康状态分 liveness、readiness 和 dependency health。Liveness 不因为下游短暂失败重启进程；readiness 在 fence、队列饱和、恢复未完成或关键依赖失败时撤流。指标至少覆盖请求延迟/错误、队列、水位、WAL fsync/replay、checkpoint pending、Remote cache/source、Saga backlog、同步丢帧/重同步和连接数。

日志携带 service、sid、entity、transaction/request ID、route/marker/fence/version。任何后台 goroutine panic 必须被容器化为错误、健康失败或 RuntimeFailure，不能静默退出。

## 13. 实现索引

| 主题 | 入口 |
| --- | --- |
| 生命周期/依赖图 | `app/app.go`、`app/mod.go` |
| 实体与锁 | `entity/`、`lock/`、`worker/` |
| 事务/WAL 契约 | `nest/`、[Nest WAL](../NEST_TRANSACTION_WAL.md) |
| 保存 | `checkpoint/` |
| Remote | `entity/remote*`、`ownerroute/`、`replica/`、[Remote 文档](../REMOTE_ENTITY.md) |
| Saga | `saga/`、[Saga 文档](../SAGA.md) |
| 同步 | `entitysync/`、`sync/`、`syncstream/`、`replication/`、`lockstep/` |
| 中间件实现 | cube-kit 对应同名目录 |
| 项目与代码生成 | roost-codegen `internal/roost` 与各 generator |
