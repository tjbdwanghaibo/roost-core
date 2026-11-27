# quota_alliance：联盟服务的权威额度账本

`roost module add quota_alliance` 显式安装；自动带上 `quota` 与 `alliance`（后者依赖 `bag`）。也可先后安装 `quota`、`alliance`，顺序不限，最后再加 `quota_alliance`。**没有最后一步就只有玩家账本，没有联盟额度。** 本模块给已有 `alliance` 服务追加 `configdata`，读取同一套 `quota` 表；服务实例必须采用一致的业务时区、换日配置和规则数据。

不新增组件号、协议、RPC 或定时任务。生成器将 `game/components/quota.Component` 挂到权威 `Alliance`，由 `Alliance.QuotaBook()` 交接 `AllianceDao.QuotaAlliance`（`persist,nosync`）。同一联盟的两个成员即使来自不同 game 进程，也应把获准的玩法操作路由到**同一联盟的权威服务**，由它在该联盟的 `rollback=undo` Nest 事务内调用 `a.Quota().Consume(allianceRuleID, amount)`，并在同一事务内完成联盟本地业务。不能在玩家、AllianceMember 或只读镜像上代扣。不同联盟各记各的；入盟、退盟不会复制或迁移额度。

规则 `scope=alliance` 不能在 Player 使用；`scope=player` 也不能在联盟使用。权限、成员资格、Remote 路由/所有权校验及请求防重由具体玩法入口负责，本模块不提供万能扣费 RPC。不要跨服务先查后扣；需要跨服务修改多个账本时，另行设计玩法级持久流程。API、热更/回拨语义和错误见 [quota](../quota/README.md)。

## 验证

生成工程运行 `roost module verify`：`^TestQuotaPlayer` 6 项，`^TestQuotaAlliance` 2 项。联盟用真正的生成实体、DAO 和 Nest，替换 Remote 准入和持久化介质；覆盖多次共享消费、两个联盟以及 Player 之间隔离、业务失败回滚和失去写权时拒绝。不是实际数据库或多进程故障演练。

在 roost-core 源码树使用匹配本树的 `roost` CLI 和可用的 Go 模块缓存，可执行 `bash modules/quota_alliance/tests/install-matrix.sh /绝对路径/roost /空的工作目录`；它独立生成玩家独装、两种增量顺序和直接装联盟伴随模块的消费者，检查装配和测试。不会在本源码树生成消费者。
