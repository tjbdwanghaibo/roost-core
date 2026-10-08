# 总览与端到端设计：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/00-overview.md)

## 如何阅读

你可以把Roost看作游戏服务器的基础设施。你编写玩法规则，它帮助安排执行、保存数据和把变化告诉客户端。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

本篇为全局索引；逐模块定位见其他分区。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 三大模块共享同一正式事务链路，不能用旁路直接写持久字段。
2. 错误、持久确认、业务回执和客户端可见性分别建模。
3. 跨层约束由根包依赖守卫检查；包数量不作为功能完成率。

## 2A. 三条端到端路径

~~~mermaid
flowchart LR
  TCP[认证连接与协议路由] --> Sender[生成 Sender]
  Sender --> Nest[Nest 准入/实体锁/handler]
  Nest --> DAO[DAO 事务变化]
  DAO --> WAL[WAL 接纳与持久确认]
  WAL --> Projector[Projector 与 Mongo]
  Nest --> Freeze[Guard 内冻结视图]
  Freeze --> Sync[确认后 Sync 交付]
  Projector --> Outbox[持久 Outbox]
  Outbox --> Remote[Remote/Mirror 消费]
~~~

图中的箭头表达依赖，不是所有节点顺序阻塞：普通strict成功只等WAL，Mongo投影异步；Sync依赖锁内冻结及相应持久确认；outbox发布与数据库提交分别确认。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`TestCoreDependencyBoundary`、`TestTrackedMarkdownRelativeLinksResolve`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
