# 次核心：Wiring 装配：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/13-wiring.md)

## 如何阅读

Wiring把数据库、调度器、同步总线等能力接进一个App。你通过配置和Mod组合这些能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`kit`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

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
| [dataengine](../../../wiring/dataengine) | [framework/dataengine](../../../framework/dataengine) | WAL、恢复、投影、持久依赖 |
| [remoteentity](../../../wiring/remoteentity) | [framework/remoteentity](../../../framework/remoteentity) | 权威存储、总线、Mirror |
| [ops](../../../wiring/ops) | [infra/observe/ops](../../../infra/observe/ops) | 管理命令、探针、统计查找与身份 |
| [statslog](../../../wiring/statslog) | [framework/statslog](../../../framework/statslog) | 配置、指标、Nest/Entity 的采样依赖 |
| [mods](../../../wiring/mods) | 各具体模块 | Mod/capability 名称与共享配置声明 |

Ops 的 HTTP handler、监听与 drain 由 runtime Server 持有；Wiring 使用同一个 Handler，不复制路由。StatsLog 的 provider、窗口、周期和文件由 runtime Logger 持有；采样时查询已装配的 Nest/Entity，避免 Provide 的先后次序让统计永久漏掉模块。

```sh
GOWORK=off go test ./wiring/... ./service/... ./infra/observe/ops ./framework/statslog
GOWORK=off go test -race ./wiring/ops ./wiring/statslog ./infra/observe/ops ./framework/statslog
```

增加或改变配置声明后执行 `go generate ./...`，再验证生成工程的配置检查和编译。全包列表见 [源码索引](../PACKAGES.md)。Gate 的新增接线及验收见 [实施方案](../GATEWAY-IMPLEMENTATION.md)。
