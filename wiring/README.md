# Wiring

Wiring 让 Game 或其他服务通过 Mod、配置和少量 option 使用框架能力。它负责依赖查询与注册、实例构造、配置声明、生命周期委托及必要的适配；运行状态由所属模块持有。

- 三大核心的便捷入口：[Nest](nest)、[DataEngine](dataengine)、[Remote](remoteentity)、[SyncBus](syncbus)。
- 跨服务副作用的两条正路（DESIGN-TODO D12 方向 A）：[Saga](saga) 的 `Steps`（业务按名字登记原生 / Mongo 步骤体，收件箱、durable、停止与指标由框架组装）与 [DataEngine](dataengine) 的 `SubscribeEffects`（事务性 outbox 的消费端，业务只给处理函数）；写法见 [Saga 使用](../docs/framework/guide/06-saga.md#21-业务怎样写步骤步骤运行时design-todo-d12-方向-a) 与 [DataEngine 使用 §3](../docs/framework/guide/03-dataengine.md#3-投影与幂等)。
- 外部资源接线：[Mongo](mongo)、[Redis](redis)、[NATS](nats)、[etcd](etcd)。
- [领域服务接入](SERVICES.md)：领域接口和运行实现位于 `service/<领域>`，Wiring 提供本地 Mod、远端 ClientMod 和进程接线。
- [Ops](ops) 接入 `infra/observe/ops`；[StatsLog](statslog) 接入 `framework/statslog`。
- [EntityTimer](entitytimer) 把按实体的持久定时器驱动（`framework/nest/entitytimer`，`roost add timer`）接到 Nest 与 App 业务时钟。

App 排序依赖并管理启停。创建资源的一方负责关闭，借用接口不重复关闭；StopWithContext 超时不表示任务已退出，不释放仍在使用的依赖。配置只经 ConfigSchema/LoadConfig 声明读取。

旧 kit 入口和领域类型 alias 已删除。生成器消费领域传输与 Wiring 接线两半，业务重新生成后使用当前路径。

[使用说明](../docs/framework/guide/13-wiring.md) · [实现边界](../docs/framework/impl/13-wiring.md) · [全包索引](../docs/framework/PACKAGES.md) · [目录方案](../docs/framework/PACKAGE-REORGANIZATION.md)
