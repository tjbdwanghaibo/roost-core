# 包目录与职责边界

目录分类已完成。全包入口见 [PACKAGES](PACKAGES.md)，逐模块契约见 [框架目录](README.md)。仓库与 Go module 仍叫 roost-core。

| 目录 | 职责 |
| --- | --- |
| framework | App、Entity、Nest、DataEngine、Sync、Remote、Saga 等服务器机制 |
| infra | 网络、存储驱动、基础数据结构与观测等基础设施 |
| gameplay | 可选玩法能力：心跳、移动、状态、技能、战斗、空间等 |
| service | account、match、session 等领域服务与正式运行实现 |
| wiring | 配置声明、依赖装配、生命周期接线和便捷接入；不拥有第二套业务状态机 |
| codegen | 工程与协议、实体、DAO、Sender 等生成工具 |
| demo | game-demo 的可读模板，复用正式模块，不另建框架实现 |
| client、robot | 客户端协议、SDK 与场景验证消费者 |

## Service 与 Wiring

`framework/app.Service` 是进程生命周期接口；`service/<领域>` 是领域功能，两者不重复。`service/session` 的 Run 表示副本、试炼等有期限运行，与网络 socket、Gate Binding、Sync Session 不同。进入、附着资源、结束与释放由领域契约负责，资源释放的业务实现必须幂等。

Wiring 负责把正式功能接到 App，包括配置读取、能力发布、启动与排空；算法、并发状态机与存储实现留在其所属模块。基建可以按接口与 driver 隔离，核心三大模块保持局部聚集，不为每层接口新建包。

## Gate

Gate 是独立接入服务，Game 是业务进程，均与 `gameplay` 目录名不同。Gate/Game 的运行实现归 `infra/network/gateway`，App 接线归 `wiring/gate`，协议分发生成由 codegen 负责。详见 [Gate](GATEWAY.md)。

## 迁移历史

旧 import 与包名的迁移已交付，不保留旧路径别名。升级记录见 [v1.25.0](../release/v1.25.0-NOTES.md)，旧目录映射与实施证据见[固定历史方案](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/framework/PACKAGE-REORGANIZATION.md)。该方案不再代表待实施工作。
