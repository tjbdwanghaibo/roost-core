# v1.23.0 双文档汇总素材 · SAGA / DRV / DAO / REM

给汇总者用：拼主文档 `docs/release/v1.23.0-GUIDE.md` / `docs/release/v1.23.0-IMPLEMENTATION.md` 的总目录与总表时，从这里取本部分的行。
本部分正文：[说明](guide-saga-drv-dao-rem.md) · [实现](impl-saga-drv-dao-rem.md)。基准提交 `02c8a10d`。

- 条目数：35（SAGA 13、DRV 6、DAO 3、REM 13）。
- 首发分布：v1.20.0 无；v1.20.1 5 条；v1.20.2 5 条；v1.21.0 10 条；v1.22.0 2 条；v1.23.0（本版）13 条。

## 条目总表

| 编号 | 一句话 | 首发 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [SAGA-1](guide-saga-drv-dao-rem.md#saga-1) | 过期且无回执的原生步骤命令直接 ack，不再无限 nak 占满共享 durable（U-0281） | v1.20.1 | 是（nak → ack） | 否 |
| [SAGA-2](guide-saga-drv-dao-rem.md#saga-2) | 原生步骤操作实例收件箱：同一操作最多生效一次、租约封顶到命令截止、放弃后迟到成功告警，含两处复核修复（U-0280） | v1.20.1 | 是（跨尝试回放 / 等待 / 接替；投影积压时步骤停住；持久格式只增） | 否（运维按 T-226 处置告警） |
| [SAGA-3](guide-saga-drv-dao-rem.md#saga-3) | 步骤超时与重试预算由配置 `saga.step_defaults` / `saga.steps` 提供 | v1.20.1 | 是（零值预算由 `Register` 补齐；写错配置 `Init` 失败） | 否（可选配置） |
| [SAGA-4](guide-saga-drv-dao-rem.md#saga-4) | 步骤覆盖原样查不到时按小写回退（RR-20261005-NC-194） | v1.20.2 | 是（无定义时大小写混写的覆盖开始生效） | 否 |
| [SAGA-5](guide-saga-drv-dao-rem.md#saga-5) | 步骤预算拒绝只差大小写的类型 / 步骤名（RR-20261006-06，收尾 A12） | v1.23.0（本版） | 是，收紧（这类命名启动失败） | 仅有这类命名的工程要改名 |
| [SAGA-6](guide-saga-drv-dao-rem.md#saga-6) | 协调器接收 completion 时核对代际、迟到告警去重、补偿方向人工 Compensate 换代（B1） | v1.20.2 | 是，收紧 | 否（运维：补偿方向 `ManualRequired` 用 `Resume`） |
| [SAGA-7](guide-saga-drv-dao-rem.md#saga-7) | 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250） | v1.21.0 | 是（迟到成功由丢弃改为 ack + 告警） | 否 |
| [SAGA-8](guide-saga-drv-dao-rem.md#saga-8) | 离开当前步骤收成一个转移 `stepTransition` + 守卫测试与补漏（saga 方向 ①） | v1.21.0 | 否（唯一差异在正常不可达路径） | 否 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | Mongo 步骤纳入操作实例收件箱（两事务、`_claims` 集合），结果消费者终态分类统一（saga 方向 ②、O-S5-1、O-S5-3） | v1.21.0 | 是（Mongo 步骤延迟约翻倍；新集合；普通结果流 Term 4 种错误） | 业务：事务外副作用仍需按 `IdempotencyKey` 幂等；运维：建议 `transactionLifetimeLimitSeconds=20` |
| [SAGA-10](guide-saga-drv-dao-rem.md#saga-10) | `ErrDefinitionMissing` 改为可重试 nak；回放不交还 claim 列为观察 | v1.21.0 | 是（Term → nak 退避） | 否 |
| [SAGA-11](guide-saga-drv-dao-rem.md#saga-11) | Mongo 步骤延迟分析，维护者选 A（接受两次落盘提交） | v1.23.0（本版） | 否（无生产代码改动） | 否 |
| [SAGA-12](guide-saga-drv-dao-rem.md#saga-12) | saga Mod 启动时校验 `saga.completion_receipt_ttl > dataengine.effects.max_age`（O-S5-2） | v1.23.0（本版） | 是，收紧（不满足拒绝启动） | 仅调过这两个键的部署 |
| [SAGA-13](guide-saga-drv-dao-rem.md#saga-13) | 真实 NATS 上 nak 退避 / `MaxDeliver` 实测，`TestAssemblyConsumesNativeNestCompletionEffects` 偶发失败根因 | v1.23.0（本版） | 否（只改测试） | 否 |
| [DRV-1](guide-saga-drv-dao-rem.md#drv-1) | Redis 脚本（Eval / EvalSha / EvalBatchDurable）回复丢失不再被驱动重放，一次调用至多执行一次（RR-20261005-NC-100） | v1.20.1 | 是（收紧）：脚本回复丢失返回传输错误 | 否 |
| [DRV-2](guide-saga-drv-dao-rem.md#drv-2) | `mongo.transaction_timeout` 端到端约束事务含提交；EndSession 补发的 abort 有 5s 上限；退避中到期保留最后一次事务错误（RR-20261005-NC-101） | v1.20.1 | 是：分区时更早返回错误，该错误可能已提交 | 否 |
| [DRV-3](guide-saga-drv-dao-rem.md#drv-3) | A2：Redis 写 / 含写 pipeline / EvalBatchDurable / DistLock 不经驱动重放，只在 `IsDefinitelyNotExecuted` 时重发；Mongo 提交后失败包 `ErrCommitResultUnknown`；契约表进仓 | v1.20.2 | 是（收紧）：写命令回复丢失返回结果未知；Mongo 错误文本多前缀 | 否（新增调用点按契约表 §6 核对） |
| [DRV-4](guide-saga-drv-dao-rem.md#drv-4) | O-M6-3：L2 墓碑写入后同连接 WAIT 副本（驱动能力 `EvalReplicated`），只计数 / Warn 不回滚；新键 `snapshot_l2_tombstone_wait_replicas` / `_timeout` | v1.23.0（本版） | 是：删除 Remote 实体最多多等 50ms（缺省） | 否（运维可调键） |
| [DRV-5](guide-saga-drv-dao-rem.md#drv-5) | 驱动与 Mod 的 Close 统一口径：重复 Close 返回 nil、并发后到者等第一个、关闭后返回已关闭错误；`operation.Serial`（第十二轮 + RR-20261006-10） | v1.23.0（本版） | 是：单机 Redis 重复 Close 改为 nil；etcd 关闭后立即 `ErrClosed`；DistLock 遇 `ErrClosed` 不再记未知 | 依赖“第二次 Close 报错”的调用方需改判断 |
| [DRV-6](guide-saga-drv-dao-rem.md#drv-6) | O-M6-5：启动建索引遇 Mongo 换主错误码有界重试（每个索引 10 次 × 1s） | v1.23.0（本版） | 是：撞上选举时启动变慢而非失败 | 否 |
| [DAO-1](guide-saga-drv-dao-rem.md#dao-1) | A1：回滚统一走 DAO，组件不再持有可回滚状态、不再登记 undo；`nopersist,nosync` 字段有 mutator；glsvet A1 提示 | v1.20.2 | 是（规范）：组件写法改变；生成 DAO 只增方法；持久格式不变 | 新组件按规范；已生成工程不迁移 |
| [DAO-2](guide-saga-drv-dao-rem.md#dao-2) | B4：skill Runtime 状态不进事务，写成约束 + 守卫测试 | v1.21.0 | 否（代码行为不变） | 是（设计约束）：先校验后推进 Runtime，扣费交给 Runtime commit |
| [DAO-3](guide-saga-drv-dao-rem.md#dao-3) | combatcomponent 属性投影入口 `ProjectAttributes`：投影写 DAO vitals，随 DAO 回滚 | v1.23.0（本版） | 只新增 API；装了投影后被投影字段以投影为准 | 想让 buff 影响伤害的业务写投影函数 |
| [REM-1](guide-saga-drv-dao-rem.md#rem-1) | 共享 L2 为快照水位权威，L1 只缓存 L2 确认过的版本；`remote_entity.cached_max_staleness`；复制消息带 `published_at`，过老快照不再接受（O5） | v1.20.2 | 是（收紧：未确认 / 超上限条目先重新确认；L2 `DeleteAtVersion` 被拒返回 `ErrStaleWrite`） | 否 |
| [REM-2](guide-saga-drv-dao-rem.md#rem-2) | Mirror 第 1～3 步：`RemoteSnapshotReadOnly` / `RemoteObservation` / `RemoteMirrorReader`、唯一读出口 `Read` + `Covers`、共享 `SnapshotClient` | v1.21.0 | 是（收紧：Monotonic 只回源一次；Cached 最低版本不满足返回 `ErrRemoteSnapshotStale`；Linearizable 需声明；停止后 `ErrSnapshotClientStopped`） | 否（只增 API） |
| [REM-3](guide-saga-drv-dao-rem.md#rem-3) | nest `allow_stale` 的 Cached Remote 访问照旧接受低于 `min_version` 的快照 | v1.21.0 | 否（回到 Mirror 之前） | 否 |
| [REM-4](guide-saga-drv-dao-rem.md#rem-4) | Mirror 第 4 步：可确认订阅（JetStream DeliverNew，普通 NATS 退化按需）、首载缓冲（64，溢出再回源）、兴趣代际锁内分配与撤销水位 | v1.21.0 | 是（普通 NATS 不再推送；JetStream 换新 durable） | 运维：可删旧 DeliverAll durable |
| [REM-5](guide-saga-drv-dao-rem.md#rem-5) | O4 兴趣容量按 consumer 配额：`snapshot_interest_per_consumer`、`ErrInterestQuotaExceeded` / `ErrInterestRegistryFull`、指标 | v1.21.0 | 是（按 consumer 拒绝，可识别、计数） | 否 |
| [REM-6](guide-saga-drv-dao-rem.md#rem-6) | Mirror 第 5 步：kit `RemoteMirrorMod`、`remote_entity.mirror.shutdown_timeout`、codegen `//roost:mirror`、`remote=mirror` 迁移诊断、公会摘要两进程样例 | v1.21.0 | 是（生成器对 `remote=mirror` 报错；只读产物需 core ≥ v1.21.0） | 是：`remote=mirror` 改 `//roost:mirror`（仓内无使用方）；装只读 Mod 要调大停机总预算 |
| [REM-7](guide-saga-drv-dao-rem.md#rem-7) | RR-20261006-01：删除提交确认时实例已被清空，确认视为完成、照常发布墓碑 | v1.22.0 | 是（strict 删除从“结果未知”变成功并发布） | 否 |
| [REM-8](guide-saga-drv-dao-rem.md#rem-8) | Mirror 第 6 步本机替代：`scripts/mirror-local.sh` 私有依赖进程，两进程 7 类故障 0 违例；v1.20.2 对照 n=6 无显著差别 | v1.22.0 | 否（测试设施） | 否 |
| [REM-9](guide-saga-drv-dao-rem.md#rem-9) | O-M6-1：owner 启动在 `remote_entity_interest_refresh` 广播“请重新续租”，合并、间隔 ≥ 1s | v1.23.0（本版） | 是（改善；wire 只新增主题） | 否 |
| [REM-10](guide-saga-drv-dao-rem.md#rem-10) | O-M6-6：同 sid 重启按进程代际令牌立即接管上一代留下的 Remote 实体锁 | v1.23.0（本版） | 是（开单实例锁时 token 格式变长；只接管同持有者上一代） | 否（需 `singleton.enabled=true` 才受益） |
| [REM-11](guide-saga-drv-dao-rem.md#rem-11) | L2 落后权威的上界：`snapshot_l2_ttl + cached_max_staleness`（core 缺省约 5m30s），保持不加后台补写 | v1.23.0（本版，文档） | 否 | 否 |
| [REM-12](guide-saga-drv-dao-rem.md#rem-12) | Redis Cluster 迁槽 ASK / MOVED 下 L2 读写与墓碑 WAIT 实测；mirror-local Cluster 就绪判定补“每个主节点有 online 副本” | v1.23.0（本版） | 否 | 否 |
| [REM-13](guide-saga-drv-dao-rem.md#rem-13) | 生成配置写出 `remote_entity` 五个新键；生产化不再把墓碑 WAIT 副本数改成 3 | v1.23.0（本版） | 否（只影响新生成工程） | 否 |

## 行为变化 / 兼容破坏

- SAGA-1：运维无需改动；旧版本残留在 durable 里的过期旧尝试，升级后第一次投递即被 ack。
- SAGA-2：运维要认识新告警 `saga.completion.late_after_abandon_total{saga_type,phase}` 与 ERROR `saga: step succeeded after the coordinator abandoned it; the effect is not compensated`，按 TROUBLESHOOTING T-226 核对业务数据（Failed 且无完成步骤可 `Resume`，否则手工撤销）。投影积压超过步骤 `Timeout` 时步骤会停住而不是重复执行：调大 `Timeout` 或解决 Mongo 变慢，不要调大 `LeaseDuration`。契约要全部步骤进程与协调器升级后才成立（滚动升级期间按旧语义）。业务（已生成工程）无需改动。
- SAGA-3：生成工程可在服务配置里写 `saga.step_defaults` / `saga.steps.<type>.<step>`；`saga.steps` 写错类型 / 步骤 / 字段、取值越界或时长不带单位时启动失败。已生成工程 `definition.go` 里写死的预算照旧生效，配置覆盖优先。
- SAGA-4：无需改动；以前静默不生效的大小写混写覆盖现在生效（实际预算可能因此改变）。
- SAGA-5：注册了只差大小写的 saga 类型名或步骤名的工程，saga Mod 启动失败，需改名。
- SAGA-6：运维——补偿方向 `ManualRequired` 修复原因后用 `Resume`（`Compensate` 在这种状态下等价，进入新一生）；新指标 `saga.completion.stale_incarnation_total{saga_type,phase}` 是 Resume 之后的正常现象，不是故障。自定义 Store 可选实现 `LateSuccessAlarmStore`，否则每次送达都告警。
- SAGA-7：无需改动。
- SAGA-8：无需改动；在 saga 包里新增协调器写记录的出口必须经 `stepTransition`，否则守卫测试失败。
- SAGA-9：业务——Mongo 步骤 handler 的业务写必须经传入的事务 ctx，才享有“同一操作最多一次”；调用别的服务的步骤仍要按 `IdempotencyKey` 幂等。运维——新集合 `<收件箱集合>_claims`（启动时自动建索引）；Mongo 服务端建议 `transactionLifetimeLimitSeconds=20`；Mongo 步骤吞吐约为之前的一半，按业务量评估。直接调用 `MongoCommandInbox.Handle` 的代码会多看到 `ErrCommandExpired` 与可重试的在途错误。
- SAGA-10：无需改动；配置错误导致定义永不注册时，结果消息在步骤超时前按 nak 退避重投、占一个 `MaxAckPending` 位。
- SAGA-11：无需改动；对延迟敏感的流程可改用原生步骤或不走 saga。
- SAGA-12：运维——若把 `dataengine.effects.max_age` 调到不小于 `saga.completion_receipt_ttl`（缺省 168h / 720h），启动失败，需调大回执 TTL 或调小流保留期（T-281）。
- SAGA-13：无。
- DRV-1：业务调用 Redis 脚本时，回复丢失现在返回传输错误（结果未知）；依赖驱动“救回”的代码会看到错误。业务按请求 ID / 版本 CAS 处理，无需改代码。
- DRV-2：运维注意网络分区下事务在 `mongo.transaction_timeout`（缺省 30s）内返回错误（回调失败另加至多 5s）；5s 内没送达的 abort 留下的服务端事务持锁到 `transactionLifetimeLimitSeconds`（缺省 60s），期间同文档写入会 WriteConflict 重跑。
- DRV-3：业务 Redis 写命令（计数、入队、SETNX、带写 pipeline、Lua）在网络抖动时会返回 EOF / i/o timeout（TROUBLESHOOTING T-259）；新写的 Redis 调用点必须按 `redis/driver/README.md` §6 核对：写错误按结果未知处理，返回值只在 `err == nil` 时可信，多步写第一步未知时仍补后续保护（如 EXPIRE），只想在确定没执行时重试的用 `driver.IsDefinitelyNotExecuted`。Mongo 调用方见到 `fmongo.ErrCommitResultUnknown` 必须按持久回执裁决；判断是否可能已提交不要再看 `UnknownTransactionCommitResult` 标签或 `DeadlineExceeded`。业务不要经 `Client.Raw()` 发写命令。
- DRV-4：运维可调 `remote_entity.snapshot_l2_tombstone_wait_replicas`（0 关闭）与 `_timeout`（(0, 1s]）；`result=short|error` 增长时查副本延迟（T-277）；要彻底避免删除复活需部署约束 `min-replicas-to-write`。已有工程配置不回写，缺省值生效。
- DRV-5：工具 / 集成测试若靠“第二次 Close 返回 `ErrClosed`”判断已关闭，改用 `errors.Is(<命令错误>, goredis.ErrClosed)`；并发 Close 的后到者现在会等（最长到自己的 ctx）。新写的驱动 / Mod 停止入口按 roost-coding 用 `internal/operation.Serial`。
- DRV-6：无需改动；运维看到启动慢几秒且 `mongo_ensure_index_election_retries_total` 非零，是启动撞上了选举（T-282）。
- DAO-1：业务新写实体组件时，事务会改的状态放进 DAO（不该落库的用 `nopersist,sync` / `nopersist,nosync`），派生值由唯一 derive 在加载与改源字段的事务里写；不要在组件方法里调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`（glsvet 会打 `hint:`）。已生成工程的旧 `captureRollback` 仍可用；需要新模板时 `roost project sync`。
- DAO-2：在 nest handler 里推进 `skill.Runtime` 的业务：会失败的业务检查放在 Runtime 调用之前；不要在 Runtime 之外手工扣费；提交被拒 / 结果未知需要严格一致的玩法，提交确认后再推进 Runtime 或用 `Checkpoint` / `RestoreRuntime`。
- DAO-3：要让 buff / 属性修饰影响伤害的业务，写一个纯投影函数并在实体工厂里 `component.ProjectAttributes(fn)`；装了投影后 `InitCombatant` 里给的被投影字段会被覆盖；投影函数不要改 `Health` / `Shield` / `Alive`。
- REM-1：L2 断网时写入的快照不再直接服务 Cached 读，要回源权威；超过 `cached_max_staleness`（缺省 = `snapshot_cache_ttl`）的条目每个窗口多一次 HGET 或回源。L2 与权威都失败时读返回错误。业务无需改；运维可按需调 `cached_max_staleness`。滚动升级期间旧发布者不带 `published_at`。
- REM-2：依赖 `Cached + minVersion` 交出低版本的调用方会收到 `ErrRemoteSnapshotStale`；`Linearizable` 只在 loader 声明线性化时开放（Manager 照旧声明）；`Assembly.Stop` 后读返回 `ErrSnapshotClientStopped`。
- REM-4：普通 NATS（kit 缺省 transport）部署不再收快照推送，新版本最晚在陈旧上限后读到，启动多一条 Warn；JetStream 部署快照主题换新 DeliverNew durable，**运维**可删旧 DeliverAll durable。
- REM-5：`snapshot_interest_subs` 语义变为每节点内存上限；单 consumer 超配额的 key 无推送、按需读取。consumer 节点多时**运维**按“consumer 数 × 配额 ≤ subs”调值。
- REM-6：**业务**：`//roost:entity remote=mirror` / `lifetime=mirror_cache` 生成报错，改用普通 struct 上的 `//roost:mirror entityKind=… coll=…`；**运维**：装 `RemoteMirrorMod` 的服务手工调大 `shutdown.total_timeout` 与部署宽限期（StopBudget 不计入生成器总预算）。生成器 Core 下限 v1.21.0。
- REM-7：strict 删除 Managed Remote 实体返回成功并发布墓碑（之前“结果未知”）。
- REM-9：JetStream 上推送开着的节点多一个 DeliverNew durable（续租请求主题）。
- REM-10：开 App 单实例锁时 Remote 锁 token 变长（Redis `owner`、Mongo `_grant_token`）；混跑时新旧都退回 TTL。
- REM-13：新生成工程配置多五个键；已有工程不回写。

## 需业务或运维改动

| 编号 | 谁 | 要做什么 | 不做的后果 |
| --- | --- | --- | --- |
| [SAGA-2](guide-saga-drv-dao-rem.md#saga-2) | 运维 | 认识新告警 `saga.completion.late_after_abandon_total{saga_type,phase}` 与 ERROR“step succeeded after the coordinator abandoned it”，按 TROUBLESHOOTING T-226 核对业务数据；投影积压超过步骤 `Timeout` 时调大 `Timeout` 或处理 Mongo 变慢，不要调大 `LeaseDuration` | 放弃后迟到生效的副作用不会被补偿，只有告警 |
| [SAGA-3](guide-saga-drv-dao-rem.md#saga-3) | 业务（可选） | 需要按环境调步骤预算时写 `saga.step_defaults` / `saga.steps.<type>.<step>`；写错类型 / 步骤 / 字段、取值越界、时长不带单位会启动失败 | 无（不写就用 `definition.go` 里的预算） |
| [SAGA-5](guide-saga-drv-dao-rem.md#saga-5) | 业务 | 注册了只差大小写的 saga 类型名或步骤名（如 `gift_item` 与 `Gift_Item`）的工程改名 | saga Mod 启动失败 |
| [SAGA-6](guide-saga-drv-dao-rem.md#saga-6) | 运维 | 补偿方向停在 `ManualRequired` 的 saga 修复原因后用 `Resume`（`Compensate` 在该状态下等价，进入新一生）；`saga.completion.stale_incarnation_total` 在 Resume 之后增长是正常现象 | 无 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | 业务 | Mongo 步骤 handler 的业务写必须经传入的事务 ctx 才享有“同一操作最多生效一次”；调用别的服务的步骤仍按 `IdempotencyKey` 幂等 | 事务外的写可能跨尝试重复 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | 运维 | 新集合 `<收件箱集合>_claims`（启动自动建索引）；建议 Mongo 服务端 `transactionLifetimeLimitSeconds=20`（O-S5-3）；Mongo 步骤吞吐约减半，按业务量评估（维护者已接受，见 [SAGA-11](guide-saga-drv-dao-rem.md#saga-11)） | kill -9 遗留的事务持锁到服务端上限（缺省 60s） |
| [SAGA-12](guide-saga-drv-dao-rem.md#saga-12) | 运维 | 若调过 `dataengine.effects.max_age` / `saga.completion_receipt_ttl`，保证 `completion_receipt_ttl > effects.max_age`（缺省 720h > 168h） | 启动失败（T-281） |
| [DRV-3](guide-saga-drv-dao-rem.md#drv-3) | 业务 | 新写的 Redis 调用点按 `redis/driver/README.md` §6 核对：写错误按结果未知处理，只想在确定没执行时重试的用 `driver.IsDefinitelyNotExecuted`；Mongo 见到 `fmongo.ErrCommitResultUnknown` 按持久回执裁决；不要经 `Client.Raw()` 发写命令 | 网络抖动时重复执行或误判未提交 |
| [DRV-4](guide-saga-drv-dao-rem.md#drv-4) | 运维（可选） | 按需调 `remote_entity.snapshot_l2_tombstone_wait_replicas`（0 关闭）/ `_timeout`（(0, 1s]）；`result=short|error` 增长时查副本延迟（T-277）；要消除删除复活需部署约束 `min-replicas-to-write` | 无（缺省 1 / 50ms 生效） |
| [DRV-5](guide-saga-drv-dao-rem.md#drv-5) | 工具 / 测试作者 | 靠“第二次 Close 返回 `ErrClosed`”判断已关闭的代码改用命令错误 `errors.Is(err, goredis.ErrClosed)`；新写的驱动 / Mod 停止入口用 `internal/operation.Serial` | 判断失效（单机 Redis 重复 Close 现在返回 nil） |
| [DAO-1](guide-saga-drv-dao-rem.md#dao-1) | 业务 | 新写实体组件时，事务会改的状态放进 DAO（不落库的用 `nopersist,sync` / `nopersist,nosync`），派生值由唯一 derive 写；组件方法里不调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`（glsvet 打 `hint:`）。已生成工程不需迁移 | 回滚后组件内存与 DAO 不一致 |
| [DAO-2](guide-saga-drv-dao-rem.md#dao-2) | 业务 | 在 nest handler 里推进 `skill.Runtime` 时：会失败的检查放在 Runtime 调用之前；扣费交给 Runtime commit 路径；严格一致的玩法在提交确认后再推进 Runtime | handler 失败回滚后冷却 / ammo / cast 状态不回退 |
| [DAO-3](guide-saga-drv-dao-rem.md#dao-3) | 业务（可选） | 要让 buff / 属性修饰影响伤害，写纯投影函数并 `component.ProjectAttributes(fn)`；装了投影后 `InitCombatant` 里给的被投影字段会被覆盖 | buff 对伤害没有效果（与之前相同） |
| [REM-4](guide-saga-drv-dao-rem.md#rem-4) | 运维 | 普通 NATS 部署不再收快照推送（读按 `cached_max_staleness` 回源，启动一条 Warn）；JetStream 部署可删除快照主题旧的 DeliverAll durable | 普通 NATS 上读到的快照最长陈旧到上限 |
| [REM-5](guide-saga-drv-dao-rem.md#rem-5) | 运维 | consumer 节点多时按“consumer 数 × `snapshot_interest_per_consumer` ≤ `snapshot_interest_subs`”调值 | 超配额的 key 无推送、按需读取（有指标与限频 Warn） |
| [REM-6](guide-saga-drv-dao-rem.md#rem-6) | 业务 | `//roost:entity remote=mirror` / `lifetime=mirror_cache` 改为普通 struct 上的 `//roost:mirror entityKind=… coll=…`（仓内无使用方）；生成器 Core 下限 v1.21.0 | 生成报迁移错误 |
| [REM-6](guide-saga-drv-dao-rem.md#rem-6) | 运维 | 装 `RemoteMirrorMod` 的服务手工调大 `shutdown.total_timeout` 与部署宽限期（只读 Mod 的停机预算不计入生成器总预算） | 停机超预算 |
| [REM-10](guide-saga-drv-dao-rem.md#rem-10) | 运维 | 想让同 sid 重启立即接管旧锁，需 `singleton.enabled=true`；开启后 Remote 锁 token 变长，新旧混跑时退回按 TTL | 不开则照旧等 `remote_entity.lock_ttl` |


## 新增门禁 / 守卫测试

| 测试名 | 文件 | 守什么 | 条目 |
| --- | --- | --- | --- |
| `TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered` | `saga/step_expired_promises_test.go` | 过期命令不执行、无回执 ack、读错误重投 | SAGA-1 |
| `TestNativeStepTakesEffectAtMostOncePerOperation` | `saga/step_operation_promises_test.go` | 操作实例最多生效一次（a / b / c / c' / d） | SAGA-2 |
| `TestNativeStepOperationInterleavingsWithCoordinatorDecisions` | `saga/step_operation_promises_test.go` | 协调器决定 × 收件箱交错 | SAGA-2 |
| `TestNativeStepLeaseNeverOutlivesTheCommandDeadline` | `saga/step_operation_promises_test.go` | 租约封顶 | SAGA-2 |
| `TestNativeStepConsumerHandlesOperationOutcomes` | `saga/step_operation_promises_test.go` | 消费者分支 | SAGA-2 |
| `TestMongoStoreTombstoneTellsAbandonedFromResolved` | `saga/step_operation_promises_test.go` | tombstone `closure` | SAGA-2 |
| `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`、`TestMongoStoreTombstoneOfAFailureCloseIsAbandoned`、`TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` | `saga/step_operation_review_test.go` | 两处复核修复 | SAGA-2 |
| `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`、`TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize` | `saga/step_operation_real_mongo_integration_test.go`（integration） | 真实服务端守卫串行化、接替 vs 投影 | SAGA-2 |
| `TestStepBudgetsComeFromConfigWithPerStepOverrides`、`TestStepBudgetConfigRejectsTyposAndImpossibleValues` | `kit/saga/step_budgets_test.go` | 预算配置与校验 | SAGA-3 |
| `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions`、`TestExactOverrideWinsOverTheLowercaseFallback` | `kit/saga/step_override_case_promises_test.go` | 小写回退与原样优先 | SAGA-4 |
| `TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase` | `kit/saga/step_budgets_test.go` | 大小写歧义报错 | SAGA-5 |
| `TestCoordinatorChecksTheIncarnationOfACompletion`、`TestMongoStoreMarksALateSuccessAlarmOncePerLife`、`TestCommandIDIncarnationInvertsCommandID` | `saga/step_operation_incarnation_promises_test.go` | B1 四个边角、告警去重、代际解析 | SAGA-6 |
| `TestRealMongoLateSuccessAlarmIsMarkedOnce` | `saga/late_alarm_real_mongo_integration_test.go`（integration） | 并发告警标记只有一个 first | SAGA-6 |
| `TestDefinitionFenceDuringBackoffAbandonsTheOperation` | `saga/definition_fence_abandon_promises_test.go` | 定义缺失出口放弃关闭 | SAGA-7 |
| `TestCoordinatorLeaseTakeoverFencesTheLateApply`、`TestOutboxSupersedeAndUnknownAckOnMongoStore` | `saga/coordinator_takeover_review_test.go` | N06 S5 审查用例（租约接管晚 Apply、outbox 替换与未知 ack） | SAGA-7 |
| `TestEveryCoordinatorWriteGoesThroughStepTransition` | `saga/step_transition_guard_test.go` | 协调器写记录只经 `stepTransition`（源码 AST 守卫） | SAGA-8 |
| ~~`TestStepTransitionGuardSeesBypassesWithoutALiteral`~~ | 已删除（RR-20261006-14）；守卫改为类型检查，另加 `TestStepTransitionAloneDecidesTheIncarnation` | 原为守卫自己的负对照 | SAGA-8 |
| `TestMongoStepAttemptsOfOneOperationTakeEffectOnce` / `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce` | `saga/mongo_step_operation_promises_test.go` / `saga/mongo_step_operation_real_mongo_integration_test.go` | Mongo 步骤操作实例最多一次（mongotest 与真实副本集同一份用例） | SAGA-9 |
| `TestMongoStepConsumerFollowsTheOperationInbox` | `saga/mongo_step_consumer_promises_test.go` | Mongo 步骤消费者分支 | SAGA-9 |
| `TestCompletionConsumersTermTheSameTerminalErrors` | `saga/completion_consumer_terminal_promises_test.go` | 两条结果流同一终态分类（O-S5-1） | SAGA-9 |
| `TestRealSagaCrossProcessKillRecovers`（断言加强为每个操作恰好一次提交） | `saga/cross_process_real_integration_test.go`（integration） | 跨进程强杀 | SAGA-7 / SAGA-9 |
| `TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`、`TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence` | `saga/completion_definition_rollout_promises_test.go` | 定义缺失可重试、最终由 fence 收尾 | SAGA-10 |
| `BenchmarkRealMongoStepLatencyBreakdown`、`BenchmarkRealMongoStepThroughput`、`BenchmarkRealMongoCommitWriteConcern` | `saga/mongo_step_latency_real_mongo_integration_test.go`（integration，只在 `-bench` 下运行） | 可复跑的延迟分析基准 | SAGA-11 |
| `TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts`、`TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom` | `kit/saga/effect_retention_promises_test.go` | O-S5-2 跨 Mod 校验 | SAGA-12 |
| `TestRealNatsCompletionNakBackoffAndMaxDeliver` | `saga/consumer_nak_maxdeliver_real_integration_test.go`（integration） | 真实 JetStream 上 nak 退避与 `MaxDeliver` | SAGA-13 |
| `TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/script_no_retry_promises_test.go` | eval / evalsha 回复丢失只执行一次 | DRV-1 |
| `TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`（原名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry`） | 同上 | `noReplay` 克隆保留标记；普通命令可重试 | DRV-1 |
| `TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice` | `versionstore/lost_reply_integration_test.go` | 真实 Redis：Update 至多写一次（integration） | DRV-1 |
| `TestRealMongoCommitIsBoundedByTransactionTimeout` | `mongo/driver/transaction_deadline_integration_test.go` | 提交受窗口约束；提交被截断时带 `ErrCommitResultUnknown`（integration） | DRV-2 / DRV-3 |
| `TestRealMongoEndSessionAfterCommitTimeoutIsBounded` | 同上 | EndSession 补发 abort 有上限（integration） | DRV-2 |
| `TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff` | `mongo/driver/transaction_retry_chain_test.go` | 退避中到期保留最后一次错误链 | DRV-2 |
| `TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown` | 同上 | 确定未提交的错误不带哨兵 | DRV-3 |
| `TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/write_no_replay_promises_test.go` | 21 个写调用点回复丢失只执行一次 | DRV-3 |
| `TestNotExecutedErrorsAreStillResent` / `TestResendsStopAtTheConfiguredBudget` / `TestAPipelineWithAnExecutedCommandIsNotResent` / `TestReadsKeepTheDriverRetry` / `TestDurableBatchIsResentWhenNothingExecuted` / `TestIsDefinitelyNotExecuted` | 同上 | 确定没执行才重发、次数上限、pipeline 不部分重放、读保留重试、分类表 | DRV-3 |
| `TestRealRedisAWriteWhoseReplyIsLostRunsOnce` | `redis/driver/write_lost_reply_integration_test.go` | 真实 Redis：incr / rpush / pipeline / setnx / distlock（integration） | DRV-3 |
| `TestAWriteWhoseReplyIsLostStillGetsItsTTL` | `cache/redis_lost_write_ttl_promises_test.go` | HSET / ZADD 结果未知后仍有 TTL | DRV-3 |
| `TestEvalReplicatedWaitsOnTheScriptsConnection` / `TestEvalReplicatedScriptErrorsAndDisabledWait` | `redis/driver/replicated_promises_test.go` | WAIT 同连接、无副本不等、WAIT 错误只进 WaitErr | DRV-4 |
| `TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete` / `…DisabledAndUnsupported` / `…SettingsAreValidated` | `remoteentity/snapshot_l2_tombstone_wait_promises_test.go` | 计数、返回值只由脚本决定、设置校验 | DRV-4 |
| `TestTombstoneWaitConfiguration` | `kit/remoteentity/tombstone_wait_config_test.go` | 两 Mod 严格读取 | DRV-4 |
| `TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait` / `TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary` | `remoteentity/snapshot_l2_tombstone_wait_integration_test.go` | 私有环境复制滞后切主 / Cluster 只打主节点（integration） | DRV-4 |
| 各包 `close_contract_promises_test.go`（redis / mongo / etcd / nats 驱动，kit redis / mongo / nats） | 见 DRV-5 实现 §6 | Close 统一口径；Mod 并发 Stop 无数据竞争 | DRV-5 |
| `TestSerialLaterCallerWaitsWithinItsOwnContext` | `internal/operation/serial_test.go` | 串行器等待受后到者 ctx 约束 | DRV-5 |
| `TestIndexCreationRetriesThroughAnElectionWithinBounds` | `mongo/driver/election_retry_promises_test.go` | 选举重试四种情形 | DRV-6 |
| `TestMirrorLocalOwnerStorageInitSurvivesAMongoElection` | `remoteentity/owner_startup_election_integration_test.go` | 私有副本集 stepDown 期间 owner 初始化（integration） | DRV-6 |
| `TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync` | `codegen/internal/dao/transient_field_promises_test.go` | `nopersist,nosync` 有 mutator、不进存储 / 同步 | DAO-1 |
| `TestATransientFieldRollsBackWithTheTransaction` / `…NeverReachesTheCommitRecordOrSync` / `…IsInTheStateSnapshot` | `codegen/internal/dao/testdata/runtime/transient_test.go` | daoruntime 运行验证 | DAO-1 |
| `TestComponentRecordingItsOwnUndoIsHinted` | `cmd/glsvet/main_test.go` | 组件方法登记 undo 被提示（不计失败） | DAO-1 |
| `TestAttributeRollbackIsTheDaoRollback` / `TestNonPersistentAttributeLayersStayOutOfTheWAL` / `TestTimerRollbackIsTheDaoRollback` / `TestTimerBookkeepingStaysOutOfTheCommitRecord` | `demo/game/entities/{player,world}/*_component_test.go.tmpl` | 生成 game-demo 的组合回滚、WAL 不含非持久字段 | DAO-1 |
| `TestCombatRollbackIsTheDaoRollback` | `skill/combatcomponent/dao_rollback_promises_test.go` | 两策略 × 两失败路径字节一致 | DAO-1 |
| `TestSkillPackagesGetNoComponentUndoHint` | `cmd/glsvet/main_test.go` | skill 四个包零 A1 提示 | DAO-2 |
| `TestBuffAttributeModifierReachesDamage` / `TestAttributeProjectionRollsBackWithTheDao` / `TestAttributeProjectionReprojectsOnLoad` | `skill/combatcomponent/attribute_projection_promises_test.go` | buff 进伤害、投影随 DAO 回滚、加载重投影不标脏 | DAO-3 |
| `TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2` 等 `TestB2*` 6 条 | `remoteentity/snapshot_l2_watermark_promises_test.go` | 陈旧上限、未确认不交出、补写丢失的 L2 写 / 删除、O5、并发收敛 | REM-1 |
| `TestRealB2WatermarkMatrixStandalone` / `…Cluster` | `remoteentity/snapshot_l2_watermark_matrix_integration_test.go` | 真实 Redis 单机 / Cluster 各 20 格水位矩阵 | REM-1 |
| `TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect` | `remoteentity/snapshot_replay_jetstream_integration_test.go` | O5：DeliverAll 重放不复活 | REM-1 |
| `TestCachedMaxStalenessConfiguration` | `kit/remoteentity/cached_max_staleness_test.go` | 新键严格读取 | REM-1 |
| `TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition` | `entity/remote_mirror_promises_test.go` | token 排序、DTO 副本、无注册冲突、读侧身份、读出口后置条件 | REM-2 |
| `TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce` / `…BelowMinimumLoadsAuthorityOnce` | `remoteentity/snapshot_read_exit_promises_test.go` | 一次 Monotonic 只回源一次 | REM-2 |
| `TestSnapshotClientHasNoWriteCapability`、`…LinearizableNeedsADeclaredLoader`、`…StartFailureLeavesNoSubscription`、`…StopContract`、`…StopCancelsLoads`、`…ReadsWhatTheOwnerPublishesInTheSameProcess` | `remoteentity/snapshot_client_promises_test.go` | 只读、线性化门、启动回收、三步停机 | REM-2 |
| `TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot` | `nest/remote_cached_allow_stale_promises_test.go` | allow_stale 的 Cached 访问 | REM-3 |
| `TestSnapshotBootstrapBuffersDeltaDuringFirstLoad`、`TestSnapshotBootstrapReplayMatrix`、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`、`TestInterestRenewReleaseConvergesInEveryDeliveryOrder`、`TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 首载缓冲、溢出再回源、renew / release 乱序收敛、退化 | REM-4 |
| `TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable` | `sync/syncbus/driver/jetstream_live_promises_test.go` | DeliverNew durable 与 DeliverAll 分开 | REM-4 |
| `TestRealJetStreamLiveSubscriptionConfirmsAndResumes`、`TestRealJetStreamLiveSnapshotPushReachesTheReader` | `remoteentity/mirror_step4_jetstream_integration_test.go` | 真实 JetStream 确认订阅与推送 | REM-4 |
| `TestInterestCapacityIsPerConsumer`、`TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 按 consumer 配额、拒绝可见 | REM-5 |
| `TestInterestPerConsumerConfiguration` | `kit/remoteentity/interest_quota_config_test.go` | 新键严格读取与范围 | REM-5 |
| `TestRemoteMirrorModRegistersOnlyReadCapability`、`…RefusesASecondClientBesideTheOwner`、`…Configuration`、`…StopContract`、`…StopCancelsInFlightReads`、`…HealthReportsPushMode` | `kit/remoteentity/remote_mirror_mod_promises_test.go` | 只读装配、冲突、配置、停机、健康 | REM-6 |
| `TestRemoteMirrorEntityMarkerIsAMigrationError`、`TestMirrorDTOGeneratesReadOnlyView`、`TestMirrorDTOOnlyPackageIsDiscoveredAndRetired`、`TestMirrorMarkerValidation` | `codegen/internal/entity/mirror_promises_test.go` | 迁移诊断、生成物无写能力 | REM-6 |
| `TestGeneratedRemoteMirrorGuildSummary/{jetstream,nats}`、`TestGeneratedRemoteMirrorReaderProcess` | `codegen/internal/entity/testdata/remoteflow/mirror_test.go` | 两进程样例（`scripts/test-remote-generated.sh`） | REM-6 |
| `TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared` | `remoteentity/remote_delete_ack_promises_test.go` | 删除确认身份不符时照常发布 | REM-7 |
| `TestGeneratedRemoteMirrorLocal`（S1～S7） | `codegen/internal/entity/testdata/remoteflow/mirror_local_test.go` | 私有环境故障场景（`ROOST_MIRROR_LOCAL=1`） | REM-8 |
| `BenchmarkMirrorLocalRead` / `BenchmarkMirrorLocalPush` | `remoteentity/mirror_local_bench_integration_test.go` | v1.20.2 对照基准 | REM-8 |
| `TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal`、`TestInterestRefreshRenewsOnlyLiveInterests`、`TestInterestRefreshRequestsCoalesceAndAreValidated`、`TestInterestRefreshGapWaitEndsOnStop`、`TestInterestRefreshNeedsPushAndToleratesOldConsumers`、`TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription` | `remoteentity/interest_refresh_promises_test.go` | 续租请求恢复、合并、校验、停机 | REM-9 |
| `TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`、`TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`、`TestAssembleRejectsAMalformedIncarnation` | `remoteentity/lock_takeover_promises_test.go` | 接管范围与校验 | REM-10 |
| `TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock` | `remoteentity/lock_takeover_integration_test.go` | 真实 Redis Lua + Mongo | REM-10 |
| `TestSingletonIncarnationIsTheHeldLocksIdentity` | `app/singleton_incarnation_promises_test.go` | 单实例锁身份登记 | REM-10 |
| `TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks` | `kit/remoteentity/lock_incarnation_promises_test.go` | kit 传代际的条件 | REM-10 |
| `TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone` | `remoteentity/cluster_slot_migration_integration_test.go` | Cluster ASK / MOVED 下 L2 与墓碑 | REM-12 |
| `TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys` | `codegen/internal/roost/remote_entity_config_keys_promises_test.go` | 生成配置带五个键 | REM-13 |
| `TestGeneratedConfigsPassStrictAndProductionValidation` | `codegen/internal/roost/generated_config_validation_promises_test.go` | 生成配置取值等于缺省并通过严格校验 | REM-13 |

## 按包的条目索引

| 包（目录） | 条目 |
| --- | --- |
| `saga/`（`command_consumer.go`） | SAGA-1、SAGA-2、SAGA-9、SAGA-10 |
| `saga/`（`step_operation_inbox.go`、`dataengine_step_inbox.go`） | SAGA-2、SAGA-6、SAGA-9 |
| `saga/`（`engine.go`、`step_transition.go`） | SAGA-2、SAGA-6、SAGA-7、SAGA-8、SAGA-10 |
| `saga/`（`mongo_store.go`、`store.go`） | SAGA-2、SAGA-6、SAGA-7 |
| `saga/`（`record.go`） | SAGA-3、SAGA-4 |
| `saga/`（`nest_completion_consumer.go`、`jetstream.go`） | SAGA-9、SAGA-10 |
| `saga/`（仅测试） | SAGA-11、SAGA-13 |
| `kit/saga/` | SAGA-3、SAGA-4、SAGA-5、SAGA-12 |
| `kit/dataengine/`（`EffectStreamRetention`） | SAGA-12 |
| `dataengine/`（`lease_fence.go`，未改，契约依赖） | SAGA-2 |
| `codegen/internal/roost/`（`add.go`、`catalog.go`、`demo.go`）与 `demo/` 模板 | SAGA-2、SAGA-3、SAGA-9 |
| 文档：`SAGA.md`、`docs/USER_GUIDE.md`、`docs/TROUBLESHOOTING.md`（T-225 / T-226 / T-281） | SAGA-1、SAGA-2、SAGA-6、SAGA-9、SAGA-10、SAGA-12 |
| `redis/driver` | DRV-1、DRV-3、DRV-4、DRV-5 |
| `redis`（`fredis` 接口） | DRV-4 |
| `mongo`（`fmongo`） | DRV-3 |
| `mongo/driver` | DRV-2、DRV-3、DRV-5、DRV-6 |
| `etcd/driver`、`nats/driver` | DRV-5 |
| `internal/operation` | DRV-5 |
| `kit/redis`、`kit/mongo`、`kit/nats` | DRV-5（kit/redis 单实例锁的 `MaxRetries=-1` 与 DRV-3 相关） |
| `kit/remoteentity` | DRV-4 |
| `remoteentity` | DRV-4、DRV-6（调用方与集成用例） |
| `app`（`config_validation.go`） | DRV-4 |
| `versionstore` | DRV-1 |
| `cache` | DRV-3 |
| `codegen/internal/dao`、`codegen/internal/roost` | DAO-1 |
| `demo`（game-demo 模板） | DAO-1 |
| `cmd/glsvet` | DAO-1、DAO-2 |
| `skill/combatcomponent` | DAO-1、DAO-3 |
| `skill/examples/statusbridge` | DAO-3 |
| `docs/skill`、`docs/agent-skills/roost-coding` | DAO-1、DAO-2、DAO-3、DRV-5 |
| `entity/` | REM-1、REM-2、REM-4 |
| `remoteentity/` | REM-1、REM-2、REM-4、REM-5、REM-6（只读 Mongo loader）、REM-7、REM-8（基准）、REM-9、REM-10、REM-11、REM-12 |
| `sync/syncbus/`（`sync.go`、`driver/`、`mirror/`） | REM-4 |
| `kit/remoteentity/` | REM-1、REM-5、REM-6、REM-10 |
| `kit/mods/` | REM-6、REM-10 |
| `app/`（`config_validation.go`、`singleton.go`） | REM-1、REM-5、REM-6、REM-10 |
| `nest/` | REM-3 |
| `codegen/internal/entity/`（含 `testdata/remoteflow`） | REM-6、REM-8 |
| `codegen/internal/roost/` | REM-13 |
| `scripts/mirror-local.sh` | REM-8、REM-12 |

## 未验证 / 外部验证项

外部验证编号见 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)；本部分涉及 E01、E02、E06、E08、E10、E11、E12、E13、E14、E15、E16。

- SAGA-1：真实 NATS 上“durable 被占满后恢复”未复现；外部验证清单未单列，NATS 多节点 HA 属 E06。
- SAGA-2：时钟偏差对租约封顶的影响未注入实测（E02）；多主机强杀（E13）；新旧步骤进程 / 协调器混跑未实跑（外部验证清单列为“本机能做、不算外部”）；真实进程下投影积压超过 `Timeout` 未实跑。
- SAGA-3 / SAGA-4 / SAGA-5：两条按源码推断的行为（无定义时写错名字静默不生效；覆盖 `backoff_min` 大于有效 `backoff_max` 在 `Register` 才报错）未写用例验证。
- SAGA-6：新旧协调器混跑未实跑；真实 Mongo 上去掉 `$exists` 的负对照未做。
- SAGA-7：混跑未实跑；真实 Mongo 上未单独复跑 NC-250 触发。
- SAGA-9：混跑未实跑；Mongo 跨主机副本集与切主（E11）；生产形态延迟（E12）。
- SAGA-10：滚动发布只在替身上模拟（真实 NATS 部分已由 SAGA-13 补）。
- SAGA-11：64 个以上协程吞吐、选项 C 实测、Linux + NVMe 跨主机提交耗时（E12）。
- SAGA-13：多节点 JetStream（E06）。
- DRV-1 / DRV-3：Redis Cluster 下 `noReplay` 的 MOVED / ASK 跟随与换节点、切主期间写不重复执行只按源码核对（E08）。
- DRV-2 / DRV-3：mongos、主从切换中的提交、提交结果未知分类与重试（E11）。
- DRV-4：多机真实复制延迟下墓碑 WAIT 与 `min-replicas-to-write` 的效果（E10）；多机 Cluster（E08）。Cluster 槽位迁移本机实测见 [REM-12](guide-saga-drv-dao-rem.md#rem-12)。
- DRV-5：未用真实 Mongo / etcd / NATS 验证 Close（Close 路径不经网络，用不可达地址）；toxiproxy 与 Cluster 用例未跑。无对应外部验证编号。
- DRV-6：多机副本集与 mongos 上 owner 启动撞选举（E11）。
- DAO-1：未在真实三进程（WAL + Mongo 投影）链路上跑被拒提交；无外部验证编号。
- DAO-2 / DAO-3：无外部验证项。
- REM-1：跨主机时钟偏差对 `published_at` 窗口的影响（E02）；Redis 异步复制丢写对 L2 水位（E10）；多机 Cluster 下 L2 Lua（E08）。无 L2 装配里删除标记受 LRU 淘汰。
- REM-2：本批未压测；读结果无“确认时刻 / 年龄”字段。
- REM-4：JetStream 多节点 HA 下的推送续投（E06）、Linux 网络（E01）、长时间容量（E15）；同 key 多个并发加载时只有最后结束的加载重放缓冲（推断，未有用例）；表满时 release 不留撤销水位（推断）；没有并发 renew / release 竞态用例直接钉住“代际锁内分配”。
- REM-5：兴趣表满载容量的多主机验证（E13 / E15 / E16）；owner 拒绝时 `InterestReplicaStore.ApplyReplica` 返回错误对 JetStream 重投的影响未核对。
- REM-6：game-demo 未加只读服务；生成 spec 不带 Tenant / Policy（推断需手写）。
- REM-7：pipelined 带 Remote 批次未单独实测；Durability 0 与清空先后未核对。
- REM-8：E01、E02、E14（Remote outbox 精确注入，缺插桩）、E15、E16（待维护者给目标负载）。
- REM-9：跨主机时钟偏差对“过期请求”判定（E02）；多主机强杀（E13）。
- REM-10：多主机强杀（E13）、多机 Cluster 锁 Lua（E08）；旧进程卡住恢复后的反向接管残余边界。
- REM-11：Redis 复制丢写（E10）；生成模板 L2 TTL 10m 下的实际上界未实测。
- REM-12：“键还在源上、槽位 MIGRATING”未单列；多机 Cluster（E08 / E10）。
- REM-13：framework-compat full 场景未等 GitHub 结果。

## 文档与源码不一致（以 `02c8a10d` 源码为准）

正文各条已按源码写，并在相应位置注明；下列是写作时发现的既有文档 / 注释与源码不一致，未改动原文档（本轮只新增三份文件）。

- `CHANGELOG.md` `[v1.21.0]` Changed 的 O-S5-1 条：记录写普通结果流对 `ErrNotWaiting` / `ErrNotFound` / `ErrIdentityConflict` / `ErrDefinitionMissing` / `ErrInvalidRecord` Term；源码（02c8a10d）`saga/nest_completion_consumer.go:154` 只有 4 种，`ErrDefinitionMissing` 已由同版本 `5a3c4a60` 移出（同一版本段的 Fixed 条有更正，但 Changed 条未改，单看 Changed 条会误读）。
- `docs/feature/SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md`“O-S5-1”段第一句同样列 5 种错误，紧随的括号更正说明了移出；源码为 4 种（以源码为准）。
- 同一文档“① 目标”段：记录写 `saga/engine.go` 新增 `stepTransition(before, after Record, cause transitionCause, receipt *Completion, outbox *OutboxRecord) ApplyRequest`；源码为 `saga/step_transition.go:53` `stepTransition(before, after Record, t transition) ApplyRequest`（参数收进 `transition` 结构体，另有 `fenced`）。“实施状态”段写的文件名正确，只是“目标”段的签名与位置是方案稿。
- 同一文档“防止新出口绕过”段：记录写守卫断言“`Engine` 方法里对 `Incarnation` 的赋值 / 自增只出现在 `stepTransition` 里”；源码 `saga/step_transition_guard_test.go:59-102` 对包内所有函数（不只 `Engine` 方法）检查 `Incarnation` 与 `ApplyRequest` 字面量，只有 `store.Apply` 参数检查限于 `Engine` 方法（源码更宽，不构成缺陷）。
- `docs/bugfix/RR-20261005-NC-250.md`“修法 / 文件与行为”：记录写新增 `abandonedOperation` 并由三个出口调用；源码（02c8a10d）已无此函数，`a95cf4dc` 用 `saga/step_transition.go:73` `openOperation` + `stepTransition` 取代（记录描述的是当时实现）。
- `docs/bugfix/U-0280-saga-step-reexecuted-after-crash.md`“根因”与 `docs/feature/B1-SAGA-COMPLETION-INCARNATION-2026-10-06.md` 第 6 节：记录写 `reserveInTransaction` / `readReceipt` / `commandIncarnation` 在 `saga/dataengine_step_inbox.go`（含旧行号 `:282`、`:190`）；源码中 `reserveInTransaction`、`commandIncarnation` 已移到 `saga/step_operation_inbox.go:162`、`:413`（`9669d181`），`readReceipt` 在 `saga/dataengine_step_inbox.go:156`。
- 维护者原话转录差异：`docs/feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md` 写“按照A，目前真正走saga的实际业务场景不多，55tps足够了”，`docs/review/DECISIONS-PENDING-2026-10-05.md` 第十二轮写“目前真正走 saga 的实际业务场景不多，55tps 足够了”（少“按照A，”、多空格）。不涉及源码。
- 性能口径差异（不是错误，提醒拼装时别混用）：saga 方向 ② 方案与 `CHANGELOG.md` `[v1.21.0]` 用 `bench.txt`（500 次 × 6）的 9.0 → 17.4 ms/op；延迟分析用 benchstat n=6 × 1000x 的 8.965 → 18.321 ms/op。两者是不同轮次的测量。**已在两份 feature 文档里标明出处（fixs）**：前者 `BenchmarkRealMongoCommandInboxHandle` 6 轮均值、base `a5e7b070`；后者 `BenchmarkRealMongoStepLatencyBreakdown` benchstat 中位数、base `a95cf4dc` / after `78e26853`。
- `docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md` §3 结果表：记录写未执行 WAIT 的结果为 `redirected` / `unsupported`；源码（02c8a10d）`remoteentity/snapshot_l2.go:373-375` 指标标签统一为 `skipped`（含 disabled）。TROUBLESHOOTING T-277 与 `docs/bugfix/PRERELEASE-VERIFICATION-2026-10-06.md` 写的是 `skipped`，与源码一致。
- `docs/review/DECISIONS-PENDING-2026-10-05.md` 文首“当前总状态”（第 13 行）：仍把 W-2026-10-06-02 列为“两条 WANTED 待 review 判断”之一；第十二轮表“驱动 Close 契约”行（第 210 行）仍写“单机与 Cluster 重复 Close 不一致等登记 WANTED，代码未改”。源码（02c8a10d）已由 `d05a04a1`（RR-20261006-10）统一，`docs/bug/WANTED.md` 也已标“已转 RR，已修复”。
- `docs/TROUBLESHOOTING.md` T-259：写“A2（main，未发版）起”；A2（`cf5721c9`）已随 v1.20.2 发布。
- 提交 `81659082` 的提交说明写 “T-231/232”；`docs/TROUBLESHOOTING.md` 里 NC-100 是 T-238、NC-101 是 T-239，T-231 / T-232 分别是 NC-64 / NC-70。
- 维护者 A1 原话两处文字不同：`docs/review/DECISIONS-PENDING-2026-10-05.md` 第二轮 A1 行为“回滚都使用 DAO 的实现方式，这样回滚都可以统一”；`docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md` 文首为“回滚都使用 dao 的实现方式，这样回滚都可以统一了”。按 BRIEF 以 DECISIONS-PENDING 为准引用。
- `docs/feature/DECISIONS-R12-KIT-2026-10-06.md` §7 与 DECISIONS-PENDING 第十二轮写 `EnsureIndexes` “遇换主 10 次 × 1s”，CHANGELOG 写“最多 10 次、间隔 1s”：源码 `mongo/driver/collection.go:251-252` 是**每个索引**单独 10 次预算，一次含 N 个索引的 `EnsureIndexes` 最坏约 N × 10s（由调用方启动 ctx 兜底）。表述不精确，非行为错误。
- 测试名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry`（`redis/driver/script_no_retry_promises_test.go:166`）在 A2 之后名字已不准确（写命令同样带 `NoRetry`），注释已改、名字未改。**已改名（fixs）**：`TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`。
- 覆盖观察（非文档冲突）：`redis/driver/write_no_replay_promises_test.go` 的 `writeCalls` 名为 `set` 的条目实际调用 `SetNX`，`Client.Set` 与 pipeline 的 Set / Del / HSet / Expire / ZAdd / LPop 没有逐个进“回复丢失只执行一次”的表；A2 方案“先红后绿”列表里的 `set` 指的是 SetNX。`TestSkillPackagesGetNoComponentUndoHint` 只扫 4 个 skill 目录，不含 `skill/skillcompose` 与 `skill/examples/*`。
- `docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md` §2 第 1 条、`entity/remote_snapshot.go:131`～`:133` 类型注释、`remoteentity/snapshot_client.go:35` 注释、DECISIONS-PENDING 第九轮“所有缓存写入仍只经 `admitLocked`”：记录写一个 key 的全部 L1 写入（含 L2 回填、修复）都经 `admitLocked`。源码（02c8a10d）为：经 `admitLocked` 的是 `publishLocked`（`:924`）、`DeleteAtVersion`（`:1110`）与 `refresh` 的“L1 比 L2 新”修复分支（`:765`）；`refresh` 的 L2 回填 / 同值确认 / 删除标记确认直接 `setL1Locked`（`:744`、`:756`、`:779`、`:795`），`adoptSharedLocked` 直接 `setL1Locked`（`:1001`）与 `l1.Delete`（`:997`），`loadForRefresh`（`:820`）与无版本 `Delete`（`:1088`）直接 `l1.Delete`。全部在 `publishMu` 下、写入值来自 L2 或权威“不存在”，语义上不破坏“L1 只缓存 L2 确认过的版本”，但字面说法不成立。以源码为准。
- `entity/remote_mirror.go:78`（`RemoteSnapshotRead.After` 注释）：记录写“Cached 读不因它回源：不满足就是未找到”；源码 `RemoteSnapshotCache.Read` 对 Cached 不满足 After 返回 `ErrRemoteSnapshotStale`（`entity/remote_snapshot.go:673`～`:674`），与 MIRROR-STEPS-1-3 §2、CHANGELOG v1.21.0 一致。注释过时。
- `docs/USER_GUIDE.md:343`：写“快照推送依赖能确认订阅的同步总线（main 未发版，…）”；Mirror 第 4 步已随 v1.21.0 发布（`23e17d81`，`git tag --contains` 最早 v1.21.0）。“main 未发版”过时。
- `docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md:40` 与 `docs/USER_GUIDE.md:343`：写新 durable 名为 `sync_remote_entity_snapshot.live_<sid>_…`；源码 `durableSyncName`（`sync/syncbus/driver/jetstream.go:427`）的可读部分经 `sanitizeSyncName`（`:445`）把 `.` 换成 `_`，实际应为 `sync_remote_entity_snapshot_live_<sid>_<hash>`（按源码推断，未连服务端核对）。
- `docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md` §3 结果表：写墓碑 WAIT 结果有 `redirected` / `unsupported`；源码 `recordTombstoneWait` 的指标标签只有 `confirmed|short|no_replicas|error|skipped`（`remoteentity/snapshot_l2.go:357`～`:380`，重定向与不支持都记 `skipped`，统计字段 `Skipped`）。`docs/bugfix/PRERELEASE-VERIFICATION-2026-10-06.md` §4b 用 `skipped`，与源码一致。（属 DRV-4 主题，此处登记。）
- `docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md` §2“可观测”：列 `interest_refresh_requests_total{result=accepted|coalesced|historic|own|invalid}`；源码还有 `stopped`（`remoteentity/interest_refresh.go:125`），且另有 `remote_entity.remote.interest_refresh_sent_total{result=sent|error}`（`:73`）未在记录列出。
- `docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md` §7、`docs/USER_GUIDE.md:339`、DECISIONS-PENDING 第十二轮“缺省约 5m30s”：按 core `DefaultConfig`（`snapshot_l2_ttl` 5m，`remoteentity/config.go:86`）成立；生成配置模板写 `snapshot_l2_ttl: 10m`（`codegen/internal/roost/catalog.go:71`），按模板部署的上界约 10m30s。不是错误，但“缺省”指的是 core 缺省，建议注明。
- `docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md` §4：写配额“缺省总上限的 1/16，即 16384”；按 core `DefaultConfig`（262144）成立，生成模板 `snapshot_interest_subs: 100000` 时缺省配额为 6250。
- 源码内部的语义差异（非文档）：本机兴趣表满时 `RemoteEntityMod` 健康报 Fail（`kit/remoteentity/remote_entity_mod.go:230`～`:231`），`RemoteMirrorMod` 报 Degraded（`kit/remoteentity/remote_mirror_mod.go:241`～`:242`）。
