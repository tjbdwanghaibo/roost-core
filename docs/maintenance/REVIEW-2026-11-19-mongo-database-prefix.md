# 评审：Mongo 库名前缀与 WAL 回放、运维工具选库不一致

**状态：已处理（2026-11-19）。** B 已修（`a4ff59d9`：`DAODatabaseFile` 与合服按加了前缀的实际库名选库）；A 按维护者决定不修（前缀在项目初定下，正常不改），只在配置帮助、[13-wiring §2.2](../framework/guide/13-wiring.md#22-mongo-库名前缀mongodatabase_prefix) 与 KNOWN-LIMITS（DAO 库名映射 ⑧）留警告，见[证据](evidence/mongo-database-prefix-20261119/README.md#评审-ab-处理)。以下为评审时的原文。

基线：`main@b05af487`（2026-11-19，引入 `mongo.database_prefix`）。两个定向断言均已在独立分支复现；未连接真实 Mongo，未运行线上迁移。

**评审结论：游戏进程在打开数据库时才加前缀，但 WAL 的重启检查和部分裸驱动工具在更早一层就把库名当作最终值。** 两处影响不同：第一处可能把历史写入回放到另一套库；第二处即使没有崩溃、没有改配置，也会让工具选错库。

## A. 未排空 WAL，只改前缀，重启检查仍然放行

**触发条件同时满足：** 复用同一个 WAL 目录；里面还有未投影的记录；只改 `mongo.database_prefix`，逻辑库映射和 SID 不变。正常排空并迁移后换前缀，不是这个问题。

[DataEngine 装配](../../framework/dataengine/engine/assembly.go#L157-L159) 在启动回放前调用 `reconcileDatabaseMapping`。顺序是对的，错的是比较对象：[检查与写回](../../framework/dataengine/engine/databases.go#L25-L55) 都只看到 `main → game`。在 [Mongo 客户端](../../wiring/mongo/database_prefix.go#L27-L34) 看来，前缀由 `old` 改成 `new`、SID 为 `3`，实际写入目标却由 `old_game_3` 变成 `new_game_3`。

**复现与证据（真实本地 WAL，内存 Mongo 替身）：**

1. 以 `old` 前缀投影玩家 7；把创建玩家 8 的记录追加到 WAL，保留为未投影状态。
2. 重新打开同一 WAL，确认仍有未确认记录。切换客户端前缀为 `new`，逻辑映射仍是 `main → game`，执行正式映射检查，再运行 Projector 回放。
3. **期望：** 检查在回放前返回 `ErrDatabaseMappingChanged`。**实测：** 检查通过；玩家 7 仍在 `old_game_3`，玩家 8 出现在 `new_game_3`，没有出现在 `old_game_3`。这验证的是一笔创建被写错库，不是所有旧记录都能在新库成功回放。

现有对照 `TestReconcileDatabaseMappingRefusesAChangeWhileRecordsAreUnprojected` 通过：它改的是映射本身，而不是前缀。[`sidmerge.walCheck`](../../wiring/sidmerge/migrate.go#L277-L302) 也以未加前缀的映射比对停机记录；这是源码关联，未做端到端合服验证。

## B. `schemamigrate` 和 `sidmove players` 不传 `-db` 时选错库

**触发条件：** 前缀非空，工具使用默认的「从配置选库」路径。无需故障重启。显式 `-db` 指定最终物理库名时不走这条路径。

[`DAODatabaseFile`](../../wiring/dataengine/databases.go#L67-L95) 读取逻辑映射并在需要时拼接 SID，没有拼前缀。[`schemamigrate`](../../demo/cmd/schemamigrate/main.go.tmpl#L42-L72) 把返回值直接用于裸驱动的目标集合和备份集合；[`sidmove players`](../../demo/cmd/sidmove/main.go.tmpl#L157-L187) 用相同规则定位玩家快照。两者后面都没有游戏进程的客户端包装再补一次前缀。

**定向验证：** 同一份配置，`mongo.database_prefix: atlas`、映射库名 `game`、`sid: 1000`。按服 DAO 对应两个工具使用的玩家路径；共享 DAO 用于核对同一解析函数的另一分支。

| DAO | 游戏进程实际库名 | 工具解析函数实测返回 |
| --- | --- | --- |
| 按服 | `atlas_game_1000` | `game_1000` |
| 共享 | `atlas_game` | `game` |

现有无前缀对照 `TestToolsResolveTheDAODatabaseLikeTheProcess` 通过，不能覆盖上表。**已经确认的是选库错误。** 如果无前缀库为空，工具可能看到空集合；如果里面有另一套数据，`schemamigrate apply` 的迁移和备份都可能作用于错误的库。本次没有执行 `apply` 或 `rollback`，不据此断言实际数据已被修改。

## 待拍板：修复边界与验收

| 评审项 | 建议 | 验收条件 |
| --- | --- | --- |
| WAL 比较什么 | 记录能识别**实际投影库**的信息，而非仅记录逻辑映射；运行时仍只加一次前缀。合服检查使用同一口径。 | 未排空时只改前缀，回放前拒绝且原记录不被覆盖；前缀不变仍可恢复；排空并迁移后允许换库。 |
| 旧 `databases.json` 怎么办 | 旧格式没有前缀，尤其是本次基线已经可能在非空前缀下写出这种文件。不能把「字段缺失」解释为「原前缀一定为空」。 | 对有积压且旧目标无法证明的情况，明确拒绝条件与操作提示；先用原配置排空，不静默猜测目标。 |
| 工具由谁解析最终库名 | 裸驱动工具和游戏进程共用同一套命名规则，保留 `-db` 是完整物理名的合同，不再补前缀。 | 覆盖空/非空前缀、共享/按服 DAO、配置 SID/显式 SID，以及显式 `-db` 不被二次加工。 |

**修复前的操作建议：** 有未投影 WAL 时不要改前缀，先按原配置排空；使用上述两个运维工具时显式传最终物理库名。以上是临时规避，不代替框架检查。本次提交仅含本文档；两条临时断言用于评审复核，未作为常驻测试提交。
