# 基建包边界补充（F00）

本页补齐原包地图“未展开”的入口，并纳入此前数量核对漏掉的 kit/manager。范围是职责、生命周期和使用限制，不表示重新完成了这些包的全面 bug 审计。CBM 代际仍为 2026-09-30，位置以当前文件为准。

| 包 | 主分区 | 使用与限制 | 源码入口 |
| --- | --- | --- | --- |
| ai | 02 | Blackboard/树/策略由实体业务驱动；不自建业务 worker，推进由宿主调度 | [controller](../../ai/controller.go) |
| container | 02 | 桶、对象池、拓扑排序；Range 遵守回调可重入容器的 C7 契约，池对象不可逃逸后被继续复用 | [bucket](../../container/bucket.go) |
| etcd | 09 | 发现、选举与 watcher 契约；发现不是实体写权限，Remote 所有权仍由自己的持久 fence 决定 | [discovery](../../etcd/discovery.go) |
| etcd/driver | 09 | etcd v3 具体适配，Mod 管理连接；ctx 决定请求期限 | [目录](../../etcd/driver) |
| event | 02 | 实体内同步事件，跨模块由 AsyncDispatcher 接 Nest；不要在事件回调自开并发修改 Entity | [handler](../../event/event_handler.go) |
| httpclient | 11 | 带签名 HTTP 客户端；保留为外部集成 API（C8），不代表项目默认使用 | [client](../../httpclient/client.go) |
| httpserver | 11 | HTTP 服务、限长读体与 shutdown；ops 使用，WebRoute 也可装配 | [server](../../httpserver/server.go) |
| index | 03 | 泛型内存索引，不是数据库索引；C8 保留公共 API，宿主管理其生命周期 | [index](../../index/index.go) |
| internal/rangecontract | 02 | 测试辅助：回调读写同容器、false 即停、稳定键只交一次；不承诺新增键是否被交出 | [contract](../../internal/rangecontract/rangecontract.go) |
| lock | 02 | 实体锁与可重入锁；锁序仍由 Entity/Nest 约束，不能用可重入规避跨实体排序 | [mutex](../../lock/reentrant_mutex.go) |
| migration | 03 | DAO 迁移步骤注册与执行；业务决定迁移顺序和持久策略，生成器引用 | [migration](../../migration/migration.go) |
| misc | 02 | 整数约束与哈希等叶子工具，无业务生命周期 | [integer](../../misc/integer.go) |
| safemap | 03 | 并发 map；Range 回调锁外执行，Read/Compute 一类锁内回调不适用 Range 的可重入承诺 | [sharded](../../safemap/sharded.go) |
| kit/etcd | 09 | 统一配置、创建/注册/关闭 etcd 能力与健康检查；不另建 App 单实例锁 | [Mod](../../kit/etcd/etcd_mod.go) |
| kit/lock | 02 | 注册进程本地 LockManager，无后台工作，StopWithContext 立即完成 | [Mod](../../kit/lock/lock_mod.go) |
| kit/mongo | 03 | 创建 Mongo 连接并注册能力和健康；停机按 App 依赖顺序释放 | [Mod](../../kit/mongo/mongo_mod.go) |
| kit/manager | 01 | 每 Service 一个 ManagerMod，委托 manager.Engine 依赖序启动/逆序停止，确实实现 StopWithContext | [Mod](../../kit/manager/manager_mod.go) |

跨分区的“主”表示职责归属，不要求搬包：cache 的持久辅助归03、复制归04，Entity 生命周期归02、Sync状态归04、Remote契约归05；nats/driver 归05、消费策略由调用者04/06各自说明。维持现有包结构，避免为了文档目录重新拆包。
