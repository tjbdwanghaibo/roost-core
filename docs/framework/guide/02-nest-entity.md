# 核心：Nest 调度与实体：设计与使用

适用运行时：当前工作树（v1.25.0 后的未发布更新）。[实现与维护入口](../impl/02-nest-entity.md) · [模块总目录](../README.md)

## 先理解这一块

同一个玩家可能同时收到购买、聊天、领取奖励等请求。Nest负责安排执行，并保护需要一起修改的数据。

你主要写handler和DAO操作；不要在这些业务函数里自行建立另一套线程、锁或保存流程。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 执行模型

Nest 接收带目标实体声明的消息，负责队列准入、同 ID 顺序、实体准备、Guard、handler 执行和事务收尾。短业务池与长业务池持本地实体锁、执行业务、冻结同步和回滚；I/O 池做冷加载、Remote 准备及 Await 查询。Entity Kind 初始化时注册 BusinessPool；任一目标属于长业务，整条消息进入长池。三池共享 ID tail，等待前驱不占业务 worker。详细配置和代码例子见 [三池与 Await](../NEST-AWAIT.md)。

长短业务 worker 内禁止同步等待网络、磁盘、数据库、另一个 Nest Request，禁止自开 goroutine 并发访问 Entity/Component/DAO。LoadedEntitiesOnly 表示只能取已加载实体；发现冷目标必须走调度器的冷加载流程，不能在 getter 里悄悄 I/O。

工作分支的快池默认等待容量为65536，整个池共享；配置 `nest.fast.queue_capacity` 或显式构造参数可以覆盖，0取默认。前驱关系由调度器按声明 ID 维护，worker 只领取前驱完成的任务，没有固定 ID→worker 队列。容量增加不代表吞吐提高，应同时观察最老等待时间与拒绝数；延时任务的独立缺省预算仍为10000。

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

明确拒绝可按策略回滚；ErrCommitIndeterminate 表示结果未知，进入 fence，不能自动当未执行重试。生成 //roost:nest 支持 pipelined，仍须满足运行配置准入条件。

## 5. 调用方与运行排查

业务使用生成 Sender/Client 和声明的 handler，基础设施才使用 RunLocal、隔离事务等低层入口。异步消息准入捕获当前配置快照，排队后固定一代；同步链保持当前请求代际。观察短业务、长业务及 I/O 队列、冷加载、handler 时间、持久等待分别定位，不能把慢 Remote 确认误认为普通 handler 吞吐不足。

actionflow 的 runner 回调期间产生修改先进入延后队列，在最外层返回前处理，避免迭代中改容器。事件回调继承实体执行边界；异步事件经正式 Nest 派发，不直接并发改对象。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
