# roost-core

Roost 是通用游戏服务器框架。业务采用 ECS：Entity 组合组件与 DAO，Nest 调度业务 handler，DataEngine 持久化变更，Sync 按会话与 Interest 同步 Entity 状态。

当前版本为 [v1.27.0](docs/release/v1.27.0-NOTES.md)：玩家接入目录改名与 Remote Rejected 精简（运行时链路、配置、wire 与存储格式不变）。此前的功能版本 [v1.26.0](docs/release/v1.26.0-NOTES.md) 带来 Nest short/long/io 三池与 Await、Sync 合批/Relation 增量/AOI 优化、旧兼容移除与全仓整理；升级按各版发布说明的升级清单执行，验收见对应 VALIDATION。

## 阅读入口

- [文档首页](docs/README.md)：现行文档目录与当前版本。
- [入门与术语](docs/GETTING-STARTED.md)：生成、配置和运行业务工程。
- [生成器](codegen/README.md)与 [game-demo 模板](demo/README.md)：建工程、生成代码、本地实跑。
- [模块设计与实现](docs/framework/README.md)：Nest、Entity、DataEngine、Sync、Remote 与周边能力。
- [全包源码索引](docs/framework/PACKAGES.md)：当前目录与职责。
- [维护手册](docs/maintenance/README.md)：修改、验证、升级、故障与发布。
- [已知限制](docs/maintenance/KNOWN-LIMITS.md)与 [性能证据](docs/performance/STABLE-v1.24.0.md)：区分已验证行为和待验收项。

## 目录

| 目录 | 职责 |
| --- | --- |
| `framework/` | App 生命周期、Nest、Entity、DataEngine、Sync、Remote、WAL、Saga 等运行能力 |
| `infra/` | base、network、storage、observe 基础设施；Gateway 运行实现归 network |
| `gameplay/` | ActionFlow、AI、Attribute、Skill 等可选玩法能力 |
| `service/` | Account、Mail、Chat、Rank、Match、Session 等领域接口、存储、RPC 传输及运行逻辑 |
| `wiring/` | 配置、依赖查询与注册、实例构造、生命周期委托；保留便捷 Mod/option 接入 |
| `codegen/`、`demo/` | 生成工具及工程模板；与运行时同一个 module |
| `client/`、`robot/` | 客户端协议、SDK 与负载工具 |

分类目录本身不创建聚合 Go 包。调用方导入具体能力，如 `framework/nest`、`service/account` 或 `wiring/account`。目录迁移后不保留旧 `kit` 入口和领域 alias；业务使用匹配工具重新生成并编译。

`service/session` 管理副本/试炼等运行记录，独立于 Gate 的网络连接会话。

## 核心执行边界

业务经正式 Nest Sender 执行。short / long 业务池处理 handler 和 Entity local 锁（Entity Kind 决定进哪个池）；io 池处理前置 I/O、Remote 获取/释放、冷加载和 Await 查询，不在 io 池拿 Entity local 锁。

setter 只标脏。handler 完成后，Guard 内汇总并冻结变更；释放全部锁、满足持久提交水位后唤醒 Sync。支持 `on_change` 与 20Hz 兜底，网络写等待在 Guard 和业务 worker 外执行。AOI/Interest 由业务决定订阅，Sync 复用 profile、完整 Entity 包和会话生命周期。

WAL durable、业务事务完成与数据库投影完成是不同结果。结果未知不能盲重试；停止超时必须保留仍被在途任务使用的依赖，并允许新的 context 继续等待真实排空。

## 开发与验证

当前 go 指令为 1.27.0，toolchain 为 go1.27.2（标准库安全修复，`GOTOOLCHAIN=auto` 时自动下载），支持保证范围是 macOS/Linux。

```sh
GOWORK=off go generate ./...
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

并发修改补受影响包的 race 回归；生成器修改编译并运行真实生成消费者。外部资源集成测试使用隔离资源，跳过或仅编译不表示行为已经通过。具体维护要求见 [AGENTS.md](AGENTS.md)。
