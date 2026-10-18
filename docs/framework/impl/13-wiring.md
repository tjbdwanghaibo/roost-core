# 次核心：Wiring 装配：实现与维护

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/13-wiring.md)

## 如何阅读

Wiring把数据库、调度器、同步总线等能力接进一个App。你通过配置和Mod组合这些能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`wiring/`（原 kit）。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 资源所有权随创建关系唯一，借用者不重复关闭。
2. 依赖写 Mod 名，Registry 查询 capability 接口。
3. StopBudget 与 StopWithContext 的真实完成条件一致。

## 2A. Mod实现核对方式

沿NewMod → ConfigSchema/Init → Provide → Start → StopWithContext逐段读。配置结构只负责本Mod读取的键；Provide发布接口，不应为了取一个接口再创建第二个后台实例。DependsOn指定生产者Mod名称；可选依赖仅在实际装配时进入排序。

验证所有启动失败分支：连接已创建但下一项校验失败、Provide失败、Start失败、Service启动失败。排空超时应保留仍在用的对象，成功Stop后才释放；同一个借用的Redis/NATS客户端不由每个服务分别关闭。

Service 与基础设施 Mod 共用这些生命周期规则。领域 Server/run 已归 service；Ops 的 HTTP 运行归 infra/observe/ops；StatsLog 的采集、窗口和周期运行归 framework/statslog。Wiring Server 在 App 初始化阶段取得依赖、构造 runtime 并委托 Serve/Shutdown。超时必须保留依赖并允许使用新的 context 等待真实排空。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 源码定位与回归

| 接线入口 | 唯一运行实现 | 主要接线责任 |
| --- | --- | --- |
| [领域服务](../../../wiring/SERVICES.md) | [service](../../../service) | 本地 Mod、远端 ClientMod、进程初始化和启停委托 |
| [nest](../../../wiring/nest) | [framework/nest](../../../framework/nest) | EntityAccess、执行器、配置与生命周期 |
| [selfsync](../../../wiring/selfsync) | [framework/sync/entitysync/policy.SelfView](../../../framework/sync/entitysync/policy/self_view.go) | 玩家看自己状态：PushSyncPlayer 包成 Transport、嵌入 TCP 握手钩子 / Game Ingress 回调转成连接激活与关闭、经 `EntitySyncSetup.Attach` 装到 Nest Mod 的 Manager |
| [dataengine](../../../wiring/dataengine) | [framework/dataengine](../../../framework/dataengine) | WAL、恢复、投影、持久依赖；业务 effect 消费者 `SubscribeEffects`（[effects.go](../../../wiring/dataengine/effects.go)，按 `dataengine.effects.*` / `dataengine.database` 组装 Mongo effect inbox 与 durable，毒消息 Term） |
| [saga](../../../wiring/saga) | [framework/saga](../../../framework/saga) | 协调器装配与跨 Mod 配置规则；业务步骤运行时 `Steps`（[steps.go](../../../wiring/saga/steps.go)，按名字登记原生 / Mongo 步骤体，组装收件箱、发布器、durable，Drain / Stop 与指标） |
| [remoteentity](../../../wiring/remoteentity) | [framework/remoteentity](../../../framework/remoteentity) | 权威存储、总线、Mirror |
| [ops](../../../wiring/ops) | [infra/observe/ops](../../../infra/observe/ops) | 管理命令、探针、统计查找与身份 |
| [statslog](../../../wiring/statslog) | [framework/statslog](../../../framework/statslog) | 配置、指标、Nest/Entity 的采样依赖 |
| [mods](../../../wiring/mods) | 各具体模块 | Mod/capability 名称与共享配置声明 |

Ops 的 HTTP handler、监听与 drain 由 runtime Server 持有；Wiring 使用同一个 Handler，不复制路由。StatsLog 的 provider、窗口、周期和文件由 runtime Logger 持有；采样时查询已装配的 Nest/Entity，避免 Provide 的先后次序让统计永久漏掉模块。

```sh
GOWORK=off go test ./wiring/... ./service/... ./infra/observe/ops ./framework/statslog
GOWORK=off go test -race ./wiring/ops ./wiring/statslog ./infra/observe/ops ./framework/statslog
GOWORK=off go test -race ./wiring/selfsync ./wiring/nest ./framework/sync/entitysync/policy
```

`wiring/saga.Steps` 与 `wiring/dataengine.SubscribeEffects`（D12 方向 A）不是 App Mod：步骤体与 effect handler 常要用服务 Init 里才发布的能力（玩家驻留表、mail 客户端），而 Mod 在服务 Init 之前启动；服务在 Init 里 Start / 订阅、在 Shutdown 里 Drain。两者都不增加配置键，只经各自 Mod 的声明读配置。回归：`wiring/saga/steps_test.go`、`wiring/dataengine/effects_test.go`，真实依赖 `TestRealStepsRunNativeAndMongoStepsThroughTheMods`、`TestRealStepsRunNativeStepsUnderTheRealNestScheduler`（真实 Nest 调度：重复投递、结果未知、业务拒绝、实体屏障交还租约、投影停到截止后跳过；公共装配在 `real_fixture_integration_test.go`）与负载 `TestRealNativeStepsPerformance`（显式入口，见 [Saga D12 前后对照](../../performance/SAGA-D12-20261018.md)）（`wiring/saga`）与 `TestRealSubscribeEffectsDeliversEachEffectOnce`（`wiring/dataengine`），`-tags integration` 且 `ROOST_DATAENGINE_IT=1`。

`wiring/nest.EntitySyncSetup` 的两个可选钩子：`Attach(manager) (detach, error)` 在 Init 里 Manager 创建之后调用一次（失败则关闭 Manager、Init 失败），detach 在 Mod 停止、Manager 关闭之后调用；`EntityLoaded` 在 DataEngine 发布从权威加载的实体之后、Mod 自己的 Rebind 之后调用（业务池上，不得阻塞）。`wiring/selfsync` 是唯一使用者：Attach 安装 `policy.SelfView`，EntityLoaded 把玩家实体的加载转成 `SelfView.EntityLoaded`。同一个 `SelfSync` 只能装到一个 Mod（第二次 Attach 报错）。回归：`wiring/selfsync/selfsync_test.go`（真实 gateway TCP：全量→增量、重连全量、首个全量排在 ACK 之后、第二条连接与旧连接关闭、Ingress 回调、只能装一次、未接推送能力时整轮重试）与 `example_client_test.go`（Go 客户端解码示例）。

增加或改变配置声明后执行 `go generate ./...`，再验证生成工程的配置检查和编译。全包列表见 [源码索引](../PACKAGES.md)。Gate 的新增接线及验收见 [实施方案](../GATEWAY-IMPLEMENTATION.md)。
