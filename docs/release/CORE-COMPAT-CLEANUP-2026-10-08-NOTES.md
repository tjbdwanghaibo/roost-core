# 三大模块旧兼容清理说明（未发版）

维护者要求：“查看下三大模块中还有没有类似兼容旧数据的复杂逻辑，可以删除掉，让代码干净很多”。规则源已经明确未上线阶段不兼容旧 API/配置/格式。本次从 main `eedc0a41` 接续，[方案](../feature/REFACTOR-2026-10-08-core-compat-cleanup.md)，[实现与验证](CORE-COMPAT-CLEANUP-2026-10-08-IMPLEMENTATION.md)。

| 编号 | 原问题 | 最终入口 |
| --- | --- | --- |
| CC1 | Nest 同时保留旧 worker/remote 配置、覆盖优先级和四池统计字段 | NestOptionWithWorkerPools 与 nest.fast / nest.slow；Stats 只用 Fast/Slow；发送慢准备用 SendOptionSlow |
| CC2 | Sync Prepare/PrepareTick 把空 delta 列表解释为默认视图，PrepareViews 则为空 | 只保留 PrepareViews；需要默认视图时显式传入 []entity.SyncProfile{{}}；空列表不打包内容，但允许提交脏版本 |
| CC3 | Entity 生成器为旧 syncPacker 标记维护第二份字段和工厂选择函数 | 只接受 subjectPacker；旧标记明确报错，不能静默丢失业务 packer |
| CC4 | DataEngine 为旧单 mutation 批量 Store 保留额外能力接口及规划分支 | BatchProjectionStore 统一承诺本地多 DAO 批量与持久幂等身份；非批量 Store 逐笔投影/确认 |
| CC5 | Remote 为旧生成 Entity 用跨事务 DAO dirty 判定是否持久提交 | IRemoteCommitParticipant 必须实现 HasRemoteCommitLocked，统一按本次事务持久变更判断；删除意图仍独立处理 |

## 升级边界

这是源码 API/配置的破坏性清理，旧工程重新生成并编译。删除 NestOptionWithWorkerNumAndMsgCap、NestOptionWithRemoteWorkers、SendOptionIsCost、NestOpts 旧字段，以及 DispatcherStats/StatsLog 的旧别名。手写 Store 若实现 ProjectBatch，必须满足新的多 DAO 与持久身份契约；仅实现 Project 的 Store 不变。手写 Remote participant 需显式实现事务局部判断。未知仓外消费者未自动修改。

生成工程的池配置改为四个 fast/slow 键，示例值均为 0，表示框架默认：快池 GOMAXPROCS/10000，慢池 max(32, 快池×4)/64。旧生成配置中的 worker_num/queue_capacity/remote_workers 不再读取；原本显式配置的值应按上述映射填写新键，不能假设仍然生效。

本轮没有修改 WAL 7、Mongo 文档或 Sync wire 的格式编号，没有数据库/WAL 自动迁移、清理或混部兼容。前一轮旧 WAL 5/6 的升级限制继续适用。回退需整套源码与生成物回退，不能混用已移除的接口。

已知仓外消费者：本机 `cube/manager/nest_mgr/nest_mgr.go`、`cube/server/gate_robot_cube_business_protocol_test.go`、`cube/server/gate_robot_bigworld_protocol_test.go` 仍使用旧 Nest worker option；升级 Core 时需改用 NestOptionWithWorkerPools。本轮只记录，没有修改 cube，也没有验证它升级后的编译。chaos/ssr 未找到本轮退役符号，不代表未知外部仓库没有调用。

## 保留的当前运行能力

Sync 的旧帧/lifetime 隔离、增量基线、重连全量是当前运行时正确性；本地文档与 Remote 信封是当前两种存储模型。Mongo 批量结果不明确时的逐笔重放用于判定当前版本冲突与幂等，单 mutation 快路径仍及时 ack，不因统一接口而取消保护。

DAO schema 自动迁移（MigrationRunner + 业务 migrator + 写回重读）是通用能力，是否整体撤销单独由维护者选择。Remote interest 的 Generation=0 旧发布端分支已发现，尚未纳入本批三模块必要提交链；进一步 Remote 协议专项可继续清理，不能据本轮宣称全仓兼容层均已删除。

未压测，不宣称吞吐提升；未部署、未发新 tag。
