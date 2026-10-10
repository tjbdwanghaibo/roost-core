# Roost 业务执行模型

本文定义 Roost 在不引入业务 Runtime 门面的前提下，承接玩家请求、Entity
互斥、跨 Entity 命令和基础设施依赖的生产级模型。

## 目标链路

```text
player transport
  -> endpoint / protocol binder
  -> generated typed sender
  -> nest.Client
  -> generated handler adapter
  -> narrow entity capability
  -> component
```

接入层只负责认证、会话、限流、协议转换和响应。Nest 是唯一可变 Entity
命令入口：它定位 Entity、按确定顺序加锁、执行 handler、回滚并记录指标。
业务 handler 只依赖其声明的 Entity capability 和显式注入的只读服务。

## 强制边界

1. Handler 会修改的每个 Entity 都必须出现在 handler 的 Entity 参数中。
2. Handler、Entity 和 Component 不得通过 EntityManager 临时取得另一个 Entity
   并修改它。
3. 跨服务操作在锁外使用 Saga、Outbox 或其他应用编排；外部 I/O 不得在
   Entity mutex 内执行。
4. 业务依赖通过构造函数注入。`app.Registry` 只在进程装配阶段使用，不得成为
   业务 Service Locator。
5. 普通请求处理不定义 `PlayerRuntime`、`BagRuntime` 等门面。Runtime 名称只保留
   给具有独立生命周期、调度循环和资源所有权的 Scene、Battle、Replication
   等引擎。

## Framework、Infra、Wiring 与 Codegen 的职责

- `framework/nest` 提供实例化 Client、调度、Entity锁、回滚、背压和观测。
- `infra/network/gateway` 提供接入与Gate/Game传输能力。
- `wiring/nest`、`wiring/gate` 装配配置、依赖和生命周期；领域运行实现归 `service/`。
- `codegen/` 生成handler adapter、强类型Sender和endpoint binder；业务不访问全局Nest实例。

## 唯一生产入口

生产代码只使用 `NewEngine`、`nest.Client`、`//roost:nest target=...` 和注入式
Sender。全局 `InitNest`/`nest.Nest` 与 codegen 包级 Send/Sync 已删除，不提供
V1/V2 双实现；异步入队错误必须回到接入层处理。

## 发布门槛

- 运行时、Wiring 和 Codegen 必须通过 `go test ./...`、`go test -race ./...` 与
  `go vet ./...`。
- 入队失败、Context 取消、停止期间调用、重复启动/停止和 Entity 类型错误必须有
  确定性测试。
- Codegen 必须有 golden/compile 测试覆盖单 Entity、多 Entity、分组 Entity、
  多返回值和当前 marker。
- Nest 热路径保留基准，变更不得引入无界 goroutine、无界队列或每请求反射。

## 单仓发布

运行时、wiring、codegen 已并入一个模块，由同一个 roost-core tag 交付。应用升级该版本并重新生成代码；本地联调的 replace 不进入发布清单。生成器与运行时最低版本由清单和兼容测试共同检查。
