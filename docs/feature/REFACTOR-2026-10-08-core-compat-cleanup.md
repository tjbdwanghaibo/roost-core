# 三大模块旧兼容路径收敛

基线 `eedc0a41`；维护者要求检查并删除三大模块类似旧 WAL 的兼容复杂逻辑。沿用未上线、不兼容旧 API/配置/格式的共同规范，已授权直接实施。目录保持 `nest/`、`dataengine/engine/`、`sync/`、`entity/` 及正式 kit/codegen 调用链，不新建包。

## 范围与步骤

1. Nest 只保留 FastPool/SlowPool 和 NestOptionWithWorkerPools；删除 WorkerNum/MsgCap/RemoteWorkers 的第二份配置及旧 option，kit 配置只使用 nest.fast / nest.slow。删除 Main/Heart/Cost/Remote 统计别名；旧 SendOptionIsCost 调用改用 SendOptionSlow。同步调用方、生成 sender、配置快照和统计日志。
2. Sync 内容捕获只保留 PrepareViews；空列表固定表示无人需要该类内容，默认视图由调用方显式传入。删除 Prepare/PrepareTick 及默认视图转换 helper。正式 Manager 已使用 PrepareViews；原单测保持原本要验证的内容，只显式声明默认视图。生成器只接受 subjectPacker，删除 syncPacker 旧标记转换。
3. DataEngine BatchProjectionStore 统一支持本地多 DAO 事务及持久事务身份，删除单 mutation 的历史能力分层。没有批量能力的 Store 继续逐笔，单 mutation Project 的末次事务身份限制仍要求及时 checkpoint，不移除恢复保护。
4. Remote 作为 Nest/DataEngine 必要提交链，将 HasRemoteCommitLocked 纳入 IRemoteCommitParticipant，删除旧生成实体的 DAO 全局 dirty 回退；更新正式生成器及测试替身。事务局部持久变更和 Sync 脏位必须继续分开。

DAO schema 自动迁移是显式通用能力，已单独询问维护者是否撤销；其决定单列，不混同于旧框架 API 兼容。Remote interest Generation=0、无 authority 协调器属于进一步 Remote 专项候选，本轮先不扩大到它的协议和租约模型。

## 不删的当前契约

Sync epoch/lifetime、增量基线与全量恢复、AOI 迁移是当前业务生命周期。DataEngine 本地文档与 Remote 信封是两种现行数据模型；Mongo 批量冲突逐笔判定、WAL 幂等与持久确认均保留。快慢池业务边界、同 ID 顺序和延迟队列预算保持。

## 验证与交接

按影响跑 entity/Nest/Sync/DataEngine/Remote/kit/codegen 测试及相关 race，全仓 build/vet/test、根包实际示例、正式生成 game-demo build/vet/test、go generate 一致性。性能测试继续暂停，不声明 TPS 收益。结果写 release 说明/实现及核心交接，提交合并推送后刷新 CBM。

这是破坏性 API/配置收敛，仓外消费者须按新入口重新生成和修改。回退使用整个原代码版本，不混搭旧生成物；本轮不自动迁移或删除任何数据库/WAL。
