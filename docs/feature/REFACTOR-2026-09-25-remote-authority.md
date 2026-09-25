# Remote 持久权威与提交批量优化

> 当前状态：已按用户“没有新部署”澄清完成协议收敛；旧迁移入口/弱校验分支已删除。前半段保留原方案背景，当前实现以文末“未部署前提下的收敛”和 [最新验收](REMOTE-UNIFIED-2026-09-25.md) 为准。

2026-09-25，继续当前工作树。用户已授权先修复问题，再继续 Remote 优化；本方案直接实施，不改变持久确认级别。

## 不变量与线性化点

- Redis 只负责竞争协调；Redis 自身 fence 可以因复制丢写回退，不再作为正式 Remote 写权限的权威。
- Mongo `_remote_entity_meta` 保存已提交版本，以及独立的所有权、最新授予 token/fence。授予在 majority 确认后才返回；每次调用 token 唯一，未知结果只按本次 token 查询，不重复授予。
- 共享写先竞争 Redis，再在 Mongo 授予 fence；本地 owner 写也取得持久授予。所有权转移、模式变化和业务提交写同一元数据文档，因此事务冲突检测覆盖授予与提交的竞争。
- 业务提交必须同时匹配 BaseVersion、最新 fence 和所有权 epoch。不能只比较历史已提交的 fence，也不能只读取权威而不写入它。
- Redis 中的版本只作为缓存；持久授予返回的 Mongo 版本决定是否重载实体。新授予生效后，旧 writer 的新提交被拒绝；已经成功提交的同一 transaction ID 仍可幂等重放。
- 超时/未知结果不等于回滚，不放宽版本比较，不清理坏 WAL，不用时间戳模拟单调计数。

## 包与职责

保持一个 `remoteentity` 包，不拆新管理层。

| 位置 | 改动 |
| --- | --- |
| 新增 mongo_authority.go | Mongo 所有权 CAS、原子授予与未知结果查询、显式迁移 |
| versioned_lock.go | 可接入持久授予，保持 Redis token 代际保护 |
| batch.go | owner-routed 写的持久授予与权威版本重载 |
| assemble.go / backend.go | 正式装配接入持久权威；缺能力则拒绝启动 |
| mongo_committer.go | 最新授予参与原子提交 CAS，DAO/快照按集合批量写 |

不新增 Entity/WAL 字段，避免改变旧事务 digest。底层 Redis-only factory 保留兼容，但不能宣称具备跨异步复制故障的 fencing 保证；正式 Assembly 不回退到该模式。

## 升级与回退边界

已有 meta 缺少权威字段时拒绝直接认领，提供显式迁移方法；调用方必须停掉旧写节点并排空旧 WAL，读取可信旧 ownership 和 fence，将计数提升到既有记录之上。不能在旧节点继续写时自动导入 Redis；也不能回退到旧二进制后继续写已迁移数据。迁移执行记录、备份与停写窗口由部署方负责，本轮不修改任何生产数据。

## 实施与验收

1. 构造持久授予、旧 writer 拒绝、未知回复幂等、所有权转移和溢出回归。
2. 接通 Assembly/Nest/生成业务路径，保持迁移边界与旧 WAL digest。
3. 在真实 Mongo + Redis Cluster 复现未复制写丢失，要求新权威 fence 增长、旧业务提交拒绝、新提交成功；保留原 Redis-only 反例。
4. 合并同一 Mongo 事务内的 DAO/快照操作，验证多 Entity、多 DAO 的原子冲突与重放；再比较真实负载。
5. race、相关真实依赖与正式生成工程回归，问题/修复文档和索引同步。未执行项目不写通过。


## 实施状态

上述结构已实施，payload 批量计划独立在同包 `mongo_payload.go`。授权不再先读后写，采用一次 FindAndModify；所有权初始化夹具限制 16 并发且不计入持续吞吐。修复与迁移契约见 RR-20260924-26，压测与故障矩阵保留独立原始日志。

容量复测显示接近投影饱和，追加同事务 metadata CAS 批量：已迁移实体均已有元数据，多个带版本/许可条件的 UpdateOne 合入一次 BulkWrite；仅当 MatchedCount 等于实体数才允许提交。任何实体失效，事务整体回滚，保留低层旧协议初始化兼容路径。

## 未部署前提下的收敛（2026-09-25，已实施）

用户确认没有新部署，本轮直接统一当前代码，不承担旧协议在线迁移。目标目录保持不变：`remoteentity/{mongo_authority,mongo_committer,versioned_lock,batch}.go`；必要的 Nest 慢调用诊断仍放 `nest/trace.go`。

1. `MongoCommitter` 创建即强制持久许可，删除旧 CAS/隐式创建元数据分支和可变 `requireAuthority`。`EnableWriteAuthority` 改为无副作用的 `WriteAuthority` getter；同步 Backend、Assembly、真实进程夹具。删除未发布的离线迁移入口，损坏/不支持的元数据仍拒绝准入，绝不自动清数据。
2. 共享锁保留本次 Mongo grant 的 ownership；Manager 使用这一份经过持久确认的结果，避免再读一次 ownership，防止混用 grant 前后的两代数据。Redis-only 基础锁仍只用于协调/测试，其路径仍需要刷新 ownership，正式 Assembly 始终配置持久权威。
3. Nest 全进程堆栈采样加进程级频率限制；每个请求的耗时指标和慢日志仍保留，避免 Remote 依赖变慢时每个请求都触发全 goroutine 堆栈，反过来拖慢业务。
4. 旧手工构造提交的测试改用正式 claim/grant，不引入测试专用弱校验开关。保持事务重放 digest 与多实体原子性。

验收：未申请许可直接提交必须拒绝；共享准入不再重复读 ownership、失效后不能返回旧 grant；慢堆栈并发采样受限；相关 race/vet、正式生成工程、真实故障矩阵与同规模 20 TPS 负载。接口改名是未发布 API 调整，不提供多套兼容 API。历史压测证据保留，最终数字另记。

本轮代码、相关 race/vet 与最终代码 21/21 故障矩阵已完成；实际负载结果见 [复测报告](REMOTE-UNIFIED-2026-09-25.md)。以上早期迁移/兼容设计保留为历史，当前代码以未部署前提下的收敛为准。

## 运行跟踪后的追加优化：已提交回执快速重放

1000 实体诊断样本的 Go trace 显示，1100 笔事务在 DataEngine 原子提交后，`MongoStore.Project → Manager.ApplyRemoteCommits → MongoCommitter.CommitRemoteBatch` 又开启一次 Mongo 事务读取已存在回执；该路径累计网络等待约 10.8 秒，与首次原子提交约 10.2 秒相当（跨 goroutine 累加，不能当作端到端百分比）。

在同包 `mongo_committer.go` 中增加事务外 majority 读取已提交 transaction 文档的快路。必须先验证完整提交及 digest：已存在且摘要一致、状态成功才返回持久回执；摘要冲突仍拒绝。未存在时照常开启完整 Mongo 事务，事务内部再次检查 transaction，覆盖并发首次提交。CAS 冲突后的读取也复用同一状态判断。不开新的发布 API、不传递可伪造的“已提交”标志，不改变 WAL/DAO/快照格式或 checkpoint 语义。

验证：已提交内容重放不再开启新 Mongo session/transaction；改内容复用 TxID 拒绝；新提交并发和新 fence 下旧事务重放仍成立；发布失败与 ack 丢失真实恢复；最终规模实载复测。

追加回执快路已实施，负对照失败/实际实现通过；最终代码重新跑完 21/21 故障矩阵和 1000 worker/10000 实体的 60 秒 20 TPS 负载，1200 笔成功，p50/p95/p99=59/435/624ms。完整限制和失败样本见最新报告。
