# v1.23.0 双文档汇总素材 · SAGA / DRV / DAO / REM

给汇总者用：拼主文档 `docs/release/v1.23.0-GUIDE.md` / `docs/release/v1.23.0-IMPLEMENTATION.md` 的总目录与总表时，从这里取本部分的行。
本部分正文：[说明](guide-saga-drv-dao-rem.md) · [实现](impl-saga-drv-dao-rem.md)。源码基准：代码冻结点 `5e72ca4d`（2026-10-07，之后只允许文档改动；初稿 `02c8a10d`，曾按 `e6828e4f`、`37338490` 重核）。本轮以 `5e72ca4d` 重核全部 `path:line`、符号与测试名：`37338490` 之后改过的文件按 diff 逐处换算（126 处），落在改动块里的 51 处逐条对源码重写；另修正 1 处因按上下文推断文件而换算错的引用。

- 条目数：44（SAGA 17、DRV 8、DAO 4、REM 15）。按冻结点补写：SAGA-16（saga 方向 ③④，`a6a902cd`，含 4 条旧断言改写与维护者选 A 允许重开）、SAGA-17（重开可观测，`93efc3cc`）、DRV-7（A2 ③ versionstore 写令牌，`6b3a0eb9`，墓碑维护者选 A 不加）、DRV-8（RR-20261006-35）、REM-15（A3 ② 排空下沉到 `ISyncBus` 退订 + RR-20261006-36，`ebf679e1`）；nats/driver 自持关闭状态（`f0de827a`）与 RR-20261006-24 / -26（`0db819b0`）并入 DRV-5。上一轮（`37338490`）重核新增 SAGA-14（收件箱改为每个操作一份状态文档）、SAGA-15（RR-20261006-14）、DAO-4（RR-20261006-13 + 第十三轮 A1 盲区 `//roost:cache`）、REM-14（RR-20261006-11 + 溢出水位到期实测）；RR-20261006-10 并入 DRV-5，L1 写入点守卫并入 REM-1，interest handler 出错即 Ack 并入 REM-5，durable 名实测并入 REM-4，O-M6-5 按索引计的重试预算并入 DRV-6，真实 NATS 与 Cluster 迁槽实测分别在 SAGA-13 / REM-12。
- 首发分布：v1.20.0 无；v1.20.1 5 条；v1.20.2 5 条；v1.21.0 10 条；v1.22.0 2 条；v1.23.0（本版）22 条。
- 原“下个大版本”项中属于本部分的（A2 ③、A3 ②、saga 方向 ③④）都已在本版完成，分册里不再有“留到下个大版本”的说法（只在描述历史决定时出现）。
- **WANTED 未决数 = 0**；仍待维护者决定的事项 = 0；review 检查点里没有“已知风险待判断”，只有给 review 去查的问题；未验证项只剩外部环境类（见文末）。
- **仍未闭环 = 1**（不是 0，列出交汇总者，不在本册判定）：共享文档与源码的措辞不一致，不在本册文件范围——TROUBLESHOOTING T-226 现象列 (2) 仍写 ERROR `...; the effect is not compensated`，源码（`a6a902cd` 起）已改为正向 WARN `...; compensating it`、补偿方向 ERROR `...; the coordinator cannot account for it`（见文末“文档与源码不一致”表）。

## 条目总表

| 编号 | 一句话 | 首发 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [SAGA-1](guide-saga-drv-dao-rem.md#saga-1) | 过期且无回执的原生步骤命令直接 ack，不再无限 nak 占满共享 durable（U-0281） | v1.20.1 | 是（nak → ack） | 否 |
| [SAGA-2](guide-saga-drv-dao-rem.md#saga-2) | 原生步骤操作实例收件箱：同一操作最多生效一次、租约封顶到命令截止、放弃后迟到成功告警，含两处复核修复（U-0280） | v1.20.1 | 是（跨尝试回放 / 等待 / 接替；投影积压时步骤停住；收件箱存储本版改为每个操作一份状态文档，见 SAGA-14；放弃后迟到的正向成功本版改为由协调器补偿，见 SAGA-16） | 否（运维按 T-226 处置补偿方向的告警） |
| [SAGA-3](guide-saga-drv-dao-rem.md#saga-3) | 步骤超时与重试预算由配置 `saga.step_defaults` / `saga.steps` 提供 | v1.20.1 | 是（零值预算由 `Register` 补齐；写错配置 `Init` 失败） | 否（可选配置） |
| [SAGA-4](guide-saga-drv-dao-rem.md#saga-4) | 步骤覆盖原样查不到时按小写回退（RR-20261005-NC-194） | v1.20.2 | 是（无定义时大小写混写的覆盖开始生效） | 否 |
| [SAGA-5](guide-saga-drv-dao-rem.md#saga-5) | 步骤预算拒绝只差大小写的类型 / 步骤名（RR-20261006-06，收尾 A12） | v1.23.0（本版） | 是，收紧（这类命名启动失败） | 仅有这类命名的工程要改名 |
| [SAGA-6](guide-saga-drv-dao-rem.md#saga-6) | 协调器接收 completion 时核对代际、迟到告警去重、补偿方向人工 Compensate 换代（B1） | v1.20.2 | 是，收紧（同一生内的接收规则本版再由 SAGA-16 ③ 收紧） | 否（运维：补偿方向 `ManualRequired` 用 `Resume`） |
| [SAGA-7](guide-saga-drv-dao-rem.md#saga-7) | 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250） | v1.21.0 | 是（迟到成功由丢弃改为 ack + 告警；本版正向的迟到成功改为补偿，见 SAGA-16） | 否 |
| [SAGA-8](guide-saga-drv-dao-rem.md#saga-8) | 离开当前步骤收成一个转移 `stepTransition` + 守卫测试与补漏（saga 方向 ①） | v1.21.0 | 否（唯一差异在正常不可达路径） | 否 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | Mongo 步骤纳入操作实例收件箱（两事务；本版状态文档集合 `<收件箱集合>_operations`，v1.21.0～v1.22.0 是 `_claims`），结果消费者终态分类统一（saga 方向 ②、O-S5-1、O-S5-3） | v1.21.0 | 是（Mongo 步骤延迟约翻倍；新集合；普通结果流 Term 4 种错误） | 业务：事务外副作用仍需按 `IdempotencyKey` 幂等；运维：建议 `transactionLifetimeLimitSeconds=20` |
| [SAGA-10](guide-saga-drv-dao-rem.md#saga-10) | `ErrDefinitionMissing` 改为可重试 nak；回执撞键时回放不交还租约列为观察 | v1.21.0 | 是（Term → nak 退避） | 否 |
| [SAGA-11](guide-saga-drv-dao-rem.md#saga-11) | Mongo 步骤延迟分析，维护者选 A（接受两次落盘提交） | v1.23.0（本版） | 否（无生产代码改动） | 否 |
| [SAGA-12](guide-saga-drv-dao-rem.md#saga-12) | saga Mod 启动时校验 `saga.completion_receipt_ttl > dataengine.effects.max_age`（O-S5-2） | v1.23.0（本版） | 是，收紧（不满足拒绝启动） | 仅调过这两个键的部署 |
| [SAGA-13](guide-saga-drv-dao-rem.md#saga-13) | 真实 NATS 上 nak 退避 / `MaxDeliver` 实测，`TestAssemblyConsumesNativeNestCompletionEffects` 偶发失败根因 | v1.23.0（本版） | 否（只改测试） | 否 |
| [SAGA-14](guide-saga-drv-dao-rem.md#saga-14) | 步骤收件箱改为每个操作一份状态文档：判定只读这一份，尝试次数与 Resume 次数不再进入判定（维护者第十三轮决定；取代 RR-20261006-15 / -16 的修法） | v1.23.0（本版） | 是，**不兼容**：存储形状改变，不读旧 claims 集合，不支持与旧步骤进程混跑；契约与延迟不变 | 运维：步骤服务先停旧再起新（原生步骤进程先排空 WAL），丢弃旧 claims 集合；业务无需改 |
| [SAGA-15](guide-saga-drv-dao-rem.md#saga-15) | `stepTransition` 改为 Engine 方法、自己写 Store；守卫改为 `go/types` 全包检查（RR-20261006-14） | v1.23.0（本版） | 否（每个出口写入的请求与之前逐字段相同） | 否 |
| [SAGA-16](guide-saga-drv-dao-rem.md#saga-16) | saga 方向 ③④：协调器只接收正在等的那次尝试的可重试失败（③，O-S5-7）；放弃之后迟到生效的正向步骤由协调器补偿这一步，`Failed` / `Compensated` 可被重开（④，U-0280 方向 C，维护者第十三轮选 A） | v1.23.0（本版） | 是：较早尝试晚到的可重试失败不再推进记录；放弃后迟到的正向成功由“只告警”改为自动补偿，终态可被重开回 `Compensating`；`Record` 多 `LateStep` / `LateData`（Mongo `late_step` / `late_data`） | 业务：按终态做的动作按 saga id 幂等、读到终态记下 `Version`（T-292）；自定义 Store 要持久化两个新字段 |
| [SAGA-17](guide-saga-drv-dao-rem.md#saga-17) | 重开可观测：`saga.reopened_total{saga_type,from_status,reason}`、`Stats().Reopened`、每次重开一条 WARN、迟到那一步补完一条 INFO（维护者第十三轮“允许重开、补指标与日志”的配套） | v1.23.0（本版） | 否（只增指标、日志与健康消息字段 `reopened=`） | 运维：认识新指标与日志（T-292） |
| [DRV-1](guide-saga-drv-dao-rem.md#drv-1) | Redis 脚本（Eval / EvalSha / EvalBatchDurable）回复丢失不再被驱动重放，一次调用至多执行一次（RR-20261005-NC-100） | v1.20.1 | 是（收紧）：脚本回复丢失返回传输错误 | 否 |
| [DRV-2](guide-saga-drv-dao-rem.md#drv-2) | `mongo.transaction_timeout` 端到端约束事务含提交；EndSession 补发的 abort 有 5s 上限；退避中到期保留最后一次事务错误（RR-20261005-NC-101） | v1.20.1 | 是：分区时更早返回错误，该错误可能已提交 | 否 |
| [DRV-3](guide-saga-drv-dao-rem.md#drv-3) | A2：Redis 写 / 含写 pipeline / EvalBatchDurable / DistLock 不经驱动重放，只在 `IsDefinitelyNotExecuted` 时重发；Mongo 提交后失败包 `ErrCommitResultUnknown`；契约表进仓 | v1.20.2 | 是（收紧）：写命令回复丢失返回结果未知；Mongo 错误文本多前缀 | 否（新增调用点按契约表 §6 核对） |
| [DRV-4](guide-saga-drv-dao-rem.md#drv-4) | O-M6-3：L2 墓碑写入后同连接 WAIT 副本（驱动能力 `EvalReplicated`），只计数 / Warn 不回滚；新键 `snapshot_l2_tombstone_wait_replicas` / `_timeout` | v1.23.0（本版） | 是：删除 Remote 实体最多多等 50ms（缺省） | 否（运维可调键） |
| [DRV-5](guide-saga-drv-dao-rem.md#drv-5) | 驱动与 Mod 的 Close 统一口径：重复 Close 返回 nil、并发后到者等第一个、关闭后返回已关闭错误；`operation.Serial`；nats/driver 自持唯一的“已关闭”状态（第十二轮 + RR-20261006-10 / -24 / -26 + 第十三轮方向调整） | v1.23.0（本版） | 是：单机 Redis 重复 Close 改为 nil；etcd 关闭后立即 `ErrClosed`；DistLock 遇 `ErrClosed` 不再记未知；nats 关闭后每个导出方法都 `errors.Is(fnats.ErrClosed)`、`Connected()` 为 false | 依赖“第二次 Close 报错”或按 nats.go 原错误判断的调用方需改判断 |
| [DRV-6](guide-saga-drv-dao-rem.md#drv-6) | O-M6-5：启动建索引遇 Mongo 换主错误码有界重试（每个索引 10 次 × 1s） | v1.23.0（本版） | 是：撞上选举时启动变慢而非失败 | 否 |
| [DRV-7](guide-saga-drv-dao-rem.md#drv-7) | A2 ③：versionstore 每次写在信封里带一次性令牌，回复丢失时 store 读一次按令牌核对（已生效返回那次结果 / 没执行原样重发 / 被别人抢先当比输），证明不了返回 `ErrOutcomeUnknown`；`Resume` 续核；墓碑维护者选 A 不加 | v1.23.0（本版） | 是，**不兼容**：信封格式改变，升级需清空 versionstore 的键（T-291）；回复丢失多数变为确定结果，剩下的错误多一层 `ErrOutcomeUnknown` | 运维：先停旧进程、清空 versionstore 键再起新版本；业务无需改代码（可选用 `Resume`） |
| [DRV-8](guide-saga-drv-dao-rem.md#drv-8) | RR-20261006-35：带索引的 compare-and-set 在发出前拒绝 NaN 分数，不再“值写入、索引不动、返回错误” | v1.23.0（本版） | 是，收紧：NaN 分数返回 `ErrCASInvalidCommand`、什么都不写 | 否（仓内分数都由整数时间换算） |
| [DAO-1](guide-saga-drv-dao-rem.md#dao-1) | A1：回滚统一走 DAO，组件不再持有可回滚状态、不再登记 undo；`nopersist,nosync` 字段有 mutator；glsvet A1 提示 | v1.20.2 | 是（规范）：组件写法改变；生成 DAO 只增方法；持久格式不变 | 新组件按规范；已生成工程不迁移 |
| [DAO-2](guide-saga-drv-dao-rem.md#dao-2) | B4：skill Runtime 状态不进事务，写成约束 + 守卫测试 | v1.21.0 | 否（代码行为不变） | 是（设计约束）：先校验后推进 Runtime，扣费交给 Runtime commit |
| [DAO-3](guide-saga-drv-dao-rem.md#dao-3) | combatcomponent 属性投影入口 `ProjectAttributes`：投影写 DAO vitals，随 DAO 回滚 | v1.23.0（本版） | 只新增 API；装了投影后被投影字段以投影为准 | 想让 buff 影响伤害的业务写投影函数 |
| [DAO-4](guide-saga-drv-dao-rem.md#dao-4) | glsvet A1 提示跟进一层同包 helper（RR-20261006-13）；组件方法写非 DAO 字段给提示，缓存字段 `//roost:cache` 豁免（第十三轮 A1 盲区） | v1.23.0（本版） | 否（只多提示，退出码不变）；`roost:nest` 文档标注开始生效 | 已有工程组件若有可变字段会看到提示：移进 DAO 或标 `//roost:cache` |
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
| [REM-14](guide-saga-drv-dao-rem.md#rem-14) | 兴趣表满时 release 改记每个 consumer 一个的溢出水位，迟到的旧续租不再复活已撤销的租约（RR-20261006-11） | v1.23.0（本版） | 是，收紧（只在表满时：之前会复活的旧续租现在被忽略） | 否 |
| [REM-15](guide-saga-drv-dao-rem.md#rem-15) | A3 ②：排空下沉到同步总线——`Subscribe` / `SubscribeLive` 返回 `*syncbus.Subscription`，`Unsubscribe(ctx)` 本身是三步停机，handler 带投递 ctx；`mirror.Replicator` 删掉自带的准入门；修 RR-20261006-36（`PatchSyncer` / `ReplicaSyncer` / `syncstream` 停止不等在途回调） | v1.23.0（本版） | 是，**API 破坏**：`syncbus.Handler` 加 ctx、订阅返回 `*Subscription`、`PatchSyncer.Stop` / `ReplicaSyncer.Stop` 改为 `Stop(ctx) error`；停止在回调返回前不报完成 | 自己实现 `ISyncBus` 或直接用这些 API 的代码按新签名改（仓内已全部迁移）；handler 里退订自己必须传入它收到的投递 ctx |

## 行为变化 / 兼容破坏

- SAGA-1：运维无需改动；旧版本残留在 durable 里的过期旧尝试，升级后第一次投递即被 ack。
- SAGA-2：运维要认识告警 `saga.completion.late_after_abandon_total{saga_type,phase}`：v1.20.1～v1.22.0 是 ERROR `saga: step succeeded after the coordinator abandoned it; the effect is not compensated`，按 TROUBLESHOOTING T-226 核对业务数据（Failed 且无完成步骤可 `Resume`，否则手工撤销）；本版起正向的迟到成功由协调器补偿（WARN `...; compensating it`，见 SAGA-16），ERROR 只剩补偿方向（`...; the coordinator cannot account for it`），按 T-226 核对后 `Resume`。投影积压超过步骤 `Timeout` 时步骤会停住而不是重复执行：调大 `Timeout` 或解决 Mongo 变慢，不要调大 `LeaseDuration`。本版收件箱存储改为每个操作一份状态文档、不兼容旧格式，升级步骤见 SAGA-14。业务（已生成工程）无需改动。
- SAGA-3：生成工程可在服务配置里写 `saga.step_defaults` / `saga.steps.<type>.<step>`；`saga.steps` 写错类型 / 步骤 / 字段、取值越界或时长不带单位时启动失败（本版 A4 ① 起字段的类型与范围由 saga Mod 的配置声明检查，报错以 `config: <键>` 开头，见 CFG 部分）。已生成工程 `definition.go` 里写死的预算照旧生效，配置覆盖优先。
- SAGA-4：无需改动；以前静默不生效的大小写混写覆盖现在生效（实际预算可能因此改变）。
- SAGA-5：注册了只差大小写的 saga 类型名或步骤名的工程，saga Mod 启动失败，需改名。
- SAGA-6：运维——补偿方向 `ManualRequired` 修复原因后用 `Resume`（`Compensate` 在这种状态下等价，进入新一生）；新指标 `saga.completion.stale_incarnation_total{saga_type,phase}` 是 Resume 之后的正常现象，不是故障（本版另有同一生的 `saga.completion.stale_attempt_total`，见 SAGA-16）。自定义 Store 可选实现 `LateSuccessAlarmStore`，否则每次送达都告警。
- SAGA-7：无需改动；本版正向的迟到成功改为补偿（SAGA-16）。
- SAGA-8：无需改动；在 saga 包里新增协调器写记录的出口必须经 `stepTransition`，否则守卫测试失败。
- SAGA-9：业务——Mongo 步骤 handler 的业务写必须经传入的事务 ctx，才享有“同一操作最多一次”；调用别的服务的步骤仍要按 `IdempotencyKey` 幂等。运维——状态文档集合 `<收件箱集合>_operations`（本版；启动时自动建 TTL 索引；v1.21.0～v1.22.0 是 `<收件箱集合>_claims`）；Mongo 服务端建议 `transactionLifetimeLimitSeconds=20`；Mongo 步骤吞吐约为之前的一半，按业务量评估。直接调用 `MongoCommandInbox.Handle` 的代码会多看到 `ErrCommandExpired` 与可重试的在途错误。
- SAGA-10：无需改动；配置错误导致定义永不注册时，结果消息在步骤超时前按 nak 退避重投、占一个 `MaxAckPending` 位。
- SAGA-11：无需改动；对延迟敏感的流程可改用原生步骤或不走 saga。
- SAGA-12：运维——若把 `dataengine.effects.max_age` 调到不小于 `saga.completion_receipt_ttl`（缺省 168h / 720h），启动失败，需调大回执 TTL 或调小流保留期（T-281）。
- SAGA-13：无。
- SAGA-14：**不兼容**。收件箱存储形状改变：原生 `_dataengine_step_operations`、Mongo 步骤 `<收件箱集合>_operations`（只有 TTL 索引），旧 `_dataengine_inbox_claims` / `<收件箱集合>_claims` 不再读写；不支持与 v1.22.0 及以前的步骤进程混跑。运维——步骤服务先停旧再起新：停掉全部旧步骤进程（原生步骤进程先排空 WAL），丢弃旧 claims 集合（`db.<name>.drop()`），再起新进程；协调器的升级顺序不受影响。`dataengine.fence.skipped.total` 的 `resource` 标签变为 `_dataengine_step_operations`。契约、延迟与回执格式不变。
- SAGA-15：无（只影响 saga 包内部结构与守卫）。
- SAGA-16：**行为变化**。同一生里较早尝试晚到的可重试失败不再推进记录（计 `saga.completion.stale_attempt_total{saga_type,phase}` / `Stats().StaleAttempt`，WARN，正常现象）。放弃之后迟到生效的**正向**步骤由协调器在同一事务里记回执并补偿这一步：`Failed` / `Compensated` 会被重开回 `Compensating`、补完回到 `Compensated`；某个补偿在途时等它结束再补；`ManualRequired` 只记下、运维 `Resume` / `Compensate` 时先补。补偿方向的迟到成功仍只告警（ERROR）。`saga` 记录在有待补偿的迟到步骤时多 `late_step` / `late_data`（`omitempty`）；自定义 Store 要原样保存 `Record.LateStep` / `LateData`，没实现 `CompletionHistoryStore` 的 Store 迟到步骤不会被补偿。线上未部署，不做兼容。
- SAGA-17：只增：指标 `saga.reopened_total{saga_type,from_status,reason}`、`Stats().Reopened`、kit saga 健康消息末尾 `reopened=<n>`；每次重开一条 WARN、迟到那一步补完一条 INFO。`Stats()` 的终态计数按“到达终态的次数”计，重开的 saga 会再计一次。
- DRV-1：业务调用 Redis 脚本时，回复丢失现在返回传输错误（结果未知）；依赖驱动“救回”的代码会看到错误。业务按请求 ID / 版本 CAS 处理，无需改代码。
- DRV-2：运维注意网络分区下事务在 `mongo.transaction_timeout`（缺省 30s）内返回错误（回调失败另加至多 5s）；5s 内没送达的 abort 留下的服务端事务持锁到 `transactionLifetimeLimitSeconds`（缺省 60s），期间同文档写入会 WriteConflict 重跑。
- DRV-3：业务 Redis 写命令（计数、入队、SETNX、带写 pipeline、Lua）在网络抖动时会返回 EOF / i/o timeout（TROUBLESHOOTING T-259）；新写的 Redis 调用点必须按 `redis/driver/README.md` §6 核对：写错误按结果未知处理，返回值只在 `err == nil` 时可信，多步写第一步未知时仍补后续保护（如 EXPIRE），只想在确定没执行时重试的用 `driver.IsDefinitelyNotExecuted`。Mongo 调用方见到 `fmongo.ErrCommitResultUnknown` 必须按持久回执裁决；判断是否可能已提交不要再看 `UnknownTransactionCommitResult` 标签或 `DeadlineExceeded`。业务不要经 `Client.Raw()` 发写命令。
- DRV-4：运维可调 `remote_entity.snapshot_l2_tombstone_wait_replicas`（0 关闭）与 `_timeout`（(0, 1s]）；`result=short|error` 增长时查副本延迟（T-277）；要彻底避免删除复活需部署约束 `min-replicas-to-write`。已有工程配置不回写，缺省值生效。
- DRV-5：工具 / 集成测试若靠“第二次 Close 返回 `ErrClosed`”判断已关闭，改用 `errors.Is(<命令错误>, goredis.ErrClosed)`；并发 Close 的后到者现在会等（最长到自己的 ctx）。新写的驱动 / Mod 停止入口按 roost-coding 用 `internal/operation.Serial`。nats：Close 之后每个导出方法（含订阅、JetStream 三方法、RPC CallAsync、`Drain`、订阅句柄 `Unsubscribe`）都返回可 `errors.Is(fnats.ErrClosed)` 的错误，不再同时 `errors.Is` 到 nats.go 的 `ErrConnectionClosed`（文本不变）；`Connected()` 在 Close 之后一直为 false；RPC 停止时在途的 CallAsync 回 `errRPCStopped`（同时 `errors.Is` 到 `ErrCancelled` 与 `ErrClosed`）；先 `Client.Close` 再 `Assembly.Close` 返回 nil。
- DRV-6：无需改动；运维看到启动慢几秒且 `mongo_ensure_index_election_retries_total` 非零，是启动撞上了选举（T-282）。
- DRV-7：**不兼容**。versionstore 信封改为 `<version>|<令牌>…\n<payload>`，旧信封读出报 `ErrMalformedRecord`（`no write token (old envelope format; clear the store on upgrade)`）；升级先停掉全部旧进程，清空各服务 versionstore 前缀下的键与带索引 store 的索引有序集合（T-291），不支持新旧进程混跑。回复丢失的写多数在 store 内被核对成确定结果（同一次调用里多一次 `GET`、至多 `MaxAttempts` 次原样重发）；剩下的错误多一层 `ErrOutcomeUnknown`（原传输错误仍可 `errors.Is`），其中 `Create` / `Delete` 回复丢失且核对时键不存在的两种情况按维护者选 A 保持结果未知。新增 `Resume`、`ErrWriteTokenMismatch`、`RedisConfig.WriteTokenHistory`（缺省 8）、指标 `versionstore.unknown_outcome.total{store,result}`；`Store` 接口不变，调用方不改代码。
- DRV-8：收紧：带索引写的分数是 NaN 时返回 `ErrCASInvalidCommand`、什么都不写（修前值写入、索引不动、返回 `ERR value is not a valid float`）。仓内分数都由整数时间换算，不受影响。
- DAO-1：业务新写实体组件时，事务会改的状态放进 DAO（不该落库的用 `nopersist,sync` / `nopersist,nosync`），派生值由唯一 derive 在加载与改源字段的事务里写；不要在组件方法里调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`（glsvet 会打 `hint:`）。已生成工程的旧 `captureRollback` 仍可用；需要新模板时 `roost project sync`。
- DAO-2：在 nest handler 里推进 `skill.Runtime` 的业务：会失败的业务检查放在 Runtime 调用之前；不要在 Runtime 之外手工扣费；提交被拒 / 结果未知需要严格一致的玩法，提交确认后再推进 Runtime 或用 `Checkpoint` / `RestoreRuntime`。
- DAO-3：要让 buff / 属性修饰影响伤害的业务，写一个纯投影函数并在实体工厂里 `component.ProjectAttributes(fn)`；装了投影后 `InitCombatant` 里给的被投影字段会被覆盖；投影函数不要改 `Health` / `Shield` / `Alive`。
- DAO-4：只多 `hint:`，不计入失败、退出码不变。已有工程的组件若在方法里写非 DAO 字段会看到提示：把状态移进 DAO（不落库用 `nopersist`），或确属缓存的字段在声明上一行或行尾加 `//roost:cache`。glsvet 改为带注释解析，名字不以 `handler` 开头、只靠 `//roost:nest` 文档标注的 handler 从此会被检查（本仓与 game-demo 无新违例）。
- REM-1：L2 断网时写入的快照不再直接服务 Cached 读，要回源权威；超过 `cached_max_staleness`（缺省 = `snapshot_cache_ttl`）的条目每个窗口多一次 HGET 或回源。L2 与权威都失败时读返回错误。业务无需改；运维可按需调 `cached_max_staleness`。滚动升级期间旧发布者不带 `published_at`。
- REM-2：依赖 `Cached + minVersion` 交出低版本的调用方会收到 `ErrRemoteSnapshotStale`；`Linearizable` 只在 loader 声明线性化时开放（Manager 照旧声明）；`Assembly.Stop` 后读返回 `ErrSnapshotClientStopped`。
- REM-4：普通 NATS（kit 缺省 transport）部署不再收快照推送，新版本最晚在陈旧上限后读到，启动多一条 Warn；JetStream 部署快照主题换新 DeliverNew durable，**运维**可删旧 DeliverAll durable。
- REM-5：`snapshot_interest_subs` 语义变为每节点内存上限；单 consumer 超配额的 key 无推送、按需读取。consumer 节点多时**运维**按“consumer 数 × 配额 ≤ subs”调值。
- REM-6：**业务**：`//roost:entity remote=mirror` / `lifetime=mirror_cache` 生成报错，改用普通 struct 上的 `//roost:mirror entityKind=… coll=…`；**运维**：装 `RemoteMirrorMod` 的服务手工调大 `shutdown.total_timeout` 与部署宽限期（StopBudget 不计入生成器总预算）。生成器 Core 下限 v1.21.0。
- REM-7：strict 删除 Managed Remote 实体返回成功并发布墓碑（之前“结果未知”）。
- REM-9：JetStream 上推送开着的节点多一个 DeliverNew durable（续租请求主题）。
- REM-10：开 App 单实例锁时 Remote 锁 token 变长（Redis `owner`、Mongo `_grant_token`）；混跑时新旧都退回 TTL。
- REM-13：新生成工程配置多五个键；已有工程不回写。
- REM-14：只在兴趣表满时变化：撤销之前发出、之后才到的旧续租以前可能复活已撤销的租约，现在被忽略；同一窗口里同一 consumer 另一个 key 的迟到旧续租若指纹碰撞也会被忽略（该 key 暂无推送、按陈旧上限回源）。wire、配置、指标不变。
- REM-15：**API 破坏**（线上未部署，不留兼容期）。`syncbus.Handler` 改为 `func(ctx, msg) error`；`Subscribe` / `SubscribeLive` 返回 `*syncbus.Subscription`，`Unsubscribe(ctx)` 返回 nil 才表示没有在途回调、也不会再有新回调；`cache.ReplicaSyncer.Stop`、`syncbus.PatchSyncer.Stop` 改为 `Stop(ctx) error` 并等在途回调（RR-20261006-36）；`syncstream.Subscribe*` 返回 `*Subscription`。在 handler 里退订自己必须传入 handler 收到的投递 ctx，传 `context.Background()` 会一直等自己。自己实现 `ISyncBus` 的替身要用 `syncbus.NewSubscription` + `Deliver`。wire、持久格式、durable 名、生成形状不变。

## 需业务或运维改动

| 编号 | 谁 | 要做什么 | 不做的后果 |
| --- | --- | --- | --- |
| [SAGA-2](guide-saga-drv-dao-rem.md#saga-2) | 运维 | 认识告警 `saga.completion.late_after_abandon_total{saga_type,phase}`：本版 `phase="forward"` 已由协调器自动补偿（SAGA-16），ERROR“step succeeded after the coordinator abandoned it; the coordinator cannot account for it”只剩补偿方向，按 TROUBLESHOOTING T-226 核对后 `Resume`；投影积压超过步骤 `Timeout` 时调大 `Timeout` 或处理 Mongo 变慢，不要调大 `LeaseDuration` | 补偿方向放弃后迟到生效的副作用只有告警，要运维处理 |
| [SAGA-3](guide-saga-drv-dao-rem.md#saga-3) | 业务（可选） | 需要按环境调步骤预算时写 `saga.step_defaults` / `saga.steps.<type>.<step>`；写错类型 / 步骤 / 字段、取值越界、时长不带单位会启动失败 | 无（不写就用 `definition.go` 里的预算） |
| [SAGA-5](guide-saga-drv-dao-rem.md#saga-5) | 业务 | 注册了只差大小写的 saga 类型名或步骤名（如 `gift_item` 与 `Gift_Item`）的工程改名 | saga Mod 启动失败 |
| [SAGA-6](guide-saga-drv-dao-rem.md#saga-6) | 运维 | 补偿方向停在 `ManualRequired` 的 saga 修复原因后用 `Resume`（`Compensate` 在该状态下等价，进入新一生）；`saga.completion.stale_incarnation_total` 在 Resume 之后增长是正常现象 | 无 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | 业务 | Mongo 步骤 handler 的业务写必须经传入的事务 ctx 才享有“同一操作最多生效一次”；调用别的服务的步骤仍按 `IdempotencyKey` 幂等 | 事务外的写可能跨尝试重复 |
| [SAGA-9](guide-saga-drv-dao-rem.md#saga-9) | 运维 | 状态文档集合 `<收件箱集合>_operations`（启动自动建 TTL 索引）；建议 Mongo 服务端 `transactionLifetimeLimitSeconds=20`（O-S5-3）；Mongo 步骤吞吐约减半，按业务量评估（维护者已接受，见 [SAGA-11](guide-saga-drv-dao-rem.md#saga-11)） | kill -9 遗留的事务持锁到服务端上限（缺省 60s） |
| [SAGA-12](guide-saga-drv-dao-rem.md#saga-12) | 运维 | 若调过 `dataengine.effects.max_age` / `saga.completion_receipt_ttl`，保证 `completion_receipt_ttl > effects.max_age`（缺省 720h > 168h） | 启动失败（T-281） |
| [SAGA-14](guide-saga-drv-dao-rem.md#saga-14) | 运维 | 升级 v1.23.0：停掉全部旧步骤进程（原生步骤进程先排空 WAL），丢弃旧 `_dataengine_inbox_claims` / `<收件箱集合>_claims`，再起新进程；不要新旧步骤进程混跑 | 旧进程写的 claim 新进程不读；WAL 里旧记录的 fence 指向旧 claim 文档，投影时被跳过 |
| [SAGA-16](guide-saga-drv-dao-rem.md#saga-16) | 业务 / 运维 | 按终态做业务的一方（失败通知、释放预留、对账）：动作按 saga id 幂等，读到终态时记下 `Record.Version`，版本变大按新状态重做（SAGA.md「运维观察」、T-292）；自定义 Store 原样保存 `Record.LateStep` / `LateData` 并实现 `CompletionHistoryStore` | 把被重开的 saga 当成永远失败 / 已补偿；自定义 Store 下迟到生效的步骤不被补偿 |
| [SAGA-17](guide-saga-drv-dao-rem.md#saga-17) | 运维 | 认识 `saga.reopened_total{saga_type,from_status,reason}` 与 WARN `saga: reopened to compensate a step that took effect after the coordinator abandoned it`（T-292）；持续增长时查结果流积压与投影延迟，或调大该步 `saga.steps.<type>.<step>.timeout` | 看不出终态被改过 |
| [DRV-3](guide-saga-drv-dao-rem.md#drv-3) | 业务 | 新写的 Redis 调用点按 `redis/driver/README.md` §6 核对：写错误按结果未知处理，只想在确定没执行时重试的用 `driver.IsDefinitelyNotExecuted`；Mongo 见到 `fmongo.ErrCommitResultUnknown` 按持久回执裁决；不要经 `Client.Raw()` 发写命令 | 网络抖动时重复执行或误判未提交 |
| [DRV-4](guide-saga-drv-dao-rem.md#drv-4) | 运维（可选） | 按需调 `remote_entity.snapshot_l2_tombstone_wait_replicas`（0 关闭）/ `_timeout`（(0, 1s]）；`result=short|error` 增长时查副本延迟（T-277）；要消除删除复活需部署约束 `min-replicas-to-write` | 无（缺省 1 / 50ms 生效） |
| [DRV-5](guide-saga-drv-dao-rem.md#drv-5) | 工具 / 测试作者 | 靠“第二次 Close 返回 `ErrClosed`”判断已关闭的代码改用命令错误 `errors.Is(err, goredis.ErrClosed)`；nats 关闭后的错误按 `errors.Is(err, fnats.ErrClosed)` 判断，不要按 nats.go 的 `ErrConnectionClosed`；新写的驱动 / Mod 停止入口用 `internal/operation.Serial` | 判断失效（单机 Redis 重复 Close 现在返回 nil；nats 关闭后的错误不再 `errors.Is` 到 nats.go 原错误） |
| [DRV-7](guide-saga-drv-dao-rem.md#drv-7) | 运维 / 业务（可选） | 升级 v1.23.0：停旧进程、清空各服务 versionstore 前缀下的键与索引有序集合再起新版本（T-291）；在 store 之外再重试写的调用方可用 `versionstore.Resume(ctx, err)` 让下一次调用续核上一次的令牌 | 新版本读旧信封全部报 `ErrMalformedRecord`；不用 `Resume` 时重试照旧要靠值里的请求 ID 去重 |
| [DAO-1](guide-saga-drv-dao-rem.md#dao-1) | 业务 | 新写实体组件时，事务会改的状态放进 DAO（不落库的用 `nopersist,sync` / `nopersist,nosync`），派生值由唯一 derive 写；组件方法里不调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`（glsvet 打 `hint:`）。已生成工程不需迁移 | 回滚后组件内存与 DAO 不一致 |
| [DAO-2](guide-saga-drv-dao-rem.md#dao-2) | 业务 | 在 nest handler 里推进 `skill.Runtime` 时：会失败的检查放在 Runtime 调用之前；扣费交给 Runtime commit 路径；严格一致的玩法在提交确认后再推进 Runtime | handler 失败回滚后冷却 / ammo / cast 状态不回退 |
| [DAO-3](guide-saga-drv-dao-rem.md#dao-3) | 业务（可选） | 要让 buff / 属性修饰影响伤害，写纯投影函数并 `component.ProjectAttributes(fn)`；装了投影后 `InitCombatant` 里给的被投影字段会被覆盖 | buff 对伤害没有效果（与之前相同） |
| [DAO-4](guide-saga-drv-dao-rem.md#dao-4) | 业务（可选） | glsvet 提示组件方法写非 DAO 字段时：状态移进 DAO（不落库用 `nopersist`），或缓存字段标 `//roost:cache` | 只是提示；不处理的话 handler 失败或提交被拒后这些字段不回滚 |
| [REM-4](guide-saga-drv-dao-rem.md#rem-4) | 运维 | 普通 NATS 部署不再收快照推送（读按 `cached_max_staleness` 回源，启动一条 Warn）；JetStream 部署可删除快照主题旧的 DeliverAll durable | 普通 NATS 上读到的快照最长陈旧到上限 |
| [REM-5](guide-saga-drv-dao-rem.md#rem-5) | 运维 | consumer 节点多时按“consumer 数 × `snapshot_interest_per_consumer` ≤ `snapshot_interest_subs`”调值 | 超配额的 key 无推送、按需读取（有指标与限频 Warn） |
| [REM-6](guide-saga-drv-dao-rem.md#rem-6) | 业务 | `//roost:entity remote=mirror` / `lifetime=mirror_cache` 改为普通 struct 上的 `//roost:mirror entityKind=… coll=…`（仓内无使用方）；生成器 Core 下限 v1.21.0 | 生成报迁移错误 |
| [REM-6](guide-saga-drv-dao-rem.md#rem-6) | 运维 | 装 `RemoteMirrorMod` 的服务手工调大 `shutdown.total_timeout` 与部署宽限期（只读 Mod 的停机预算不计入生成器总预算） | 停机超预算 |
| [REM-10](guide-saga-drv-dao-rem.md#rem-10) | 运维 | 想让同 sid 重启立即接管旧锁，需 `singleton.enabled=true`；开启后 Remote 锁 token 变长，新旧混跑时退回按 TTL | 不开则照旧等 `remote_entity.lock_ttl` |
| [REM-15](guide-saga-drv-dao-rem.md#rem-15) | 业务（自己实现或直接调用这些 API 的） | 按新签名改：`syncbus.Handler` 带 ctx、订阅拿 `*syncbus.Subscription`、`PatchSyncer.Stop(ctx)` / `ReplicaSyncer.Stop(ctx)`；handler 里退订自己传投递 ctx；替身用 `syncbus.NewSubscription` + `Deliver`（仓内已全部迁移） | 编译失败；handler 里用无关 ctx 退订自己会一直等 |


## 新增门禁 / 守卫测试

| 测试名 | 文件 | 守什么 | 条目 |
| --- | --- | --- | --- |
| `TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered` | `saga/step_expired_promises_test.go` | 过期命令不执行、无回执 ack、读错误重投 | SAGA-1 |
| `TestNativeStepTakesEffectAtMostOncePerOperation` | `saga/step_operation_promises_test.go` | 操作实例最多生效一次（a / b / c / c' / d） | SAGA-2 |
| `TestNativeStepOperationInterleavingsWithCoordinatorDecisions` | `saga/step_operation_promises_test.go` | 协调器决定 × 收件箱交错 | SAGA-2 |
| `TestNativeStepLeaseNeverOutlivesTheCommandDeadline` | `saga/step_operation_promises_test.go` | 租约封顶 | SAGA-2 |
| `TestNativeStepConsumerHandlesOperationOutcomes` | `saga/step_operation_promises_test.go` | 消费者分支 | SAGA-2 |
| `TestMongoStoreTombstoneTellsAbandonedFromResolved` | `saga/step_operation_promises_test.go` | tombstone `closure` | SAGA-2 |
| `TestMongoStoreTombstoneOfAFailureCloseIsAbandoned`、`TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` | `saga/step_operation_review_test.go` | 两处复核修复（复核 1 原时序用例 `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed` 被方向 ③ 推翻，本版改写为 SAGA-16 的 ③ 用例） | SAGA-2 |
| `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`、`TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize`、`TestRealMongoTakeoverFencesTheEarlierAttemptsProjection` | `saga/step_operation_real_mongo_integration_test.go`（integration） | 真实服务端上同一操作的 Reserve 串行化（本版起由状态文档的写冲突承担）、接替 vs 投影两边各一 | SAGA-2 / SAGA-14 |
| `TestStepBudgetsComeFromConfigWithPerStepOverrides`、`TestStepBudgetConfigRejectsTyposAndImpossibleValues` | `kit/saga/step_budgets_test.go` | 预算配置与校验 | SAGA-3 |
| `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions`、`TestExactOverrideWinsOverTheLowercaseFallback` | `kit/saga/step_override_case_promises_test.go` | 小写回退与原样优先 | SAGA-4 |
| `TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase` | `kit/saga/step_budgets_test.go` | 大小写歧义报错 | SAGA-5 |
| `TestCoordinatorChecksTheIncarnationOfACompletion`、`TestMongoStoreMarksALateSuccessAlarmOncePerLife`、`TestCommandIDIncarnationInvertsCommandID` | `saga/step_operation_incarnation_promises_test.go` | B1 四个边角、告警去重、代际解析 | SAGA-6 |
| `TestRealMongoLateSuccessAlarmIsMarkedOnce` | `saga/late_alarm_real_mongo_integration_test.go`（integration） | 并发告警标记只有一个 first | SAGA-6 |
| `TestDefinitionFenceDuringBackoffAbandonsTheOperation` | `saga/definition_fence_abandon_promises_test.go` | 定义缺失出口放弃关闭 | SAGA-7 |
| `TestCoordinatorLeaseTakeoverFencesTheLateApply`、`TestOutboxSupersedeAndUnknownAckOnMongoStore` | `saga/coordinator_takeover_review_test.go` | N06 S5 审查用例（租约接管晚 Apply、outbox 替换与未知 ack） | SAGA-7 |
| `TestEveryCoordinatorWriteGoesThroughStepTransition` | `saga/step_transition_guard_test.go` | 协调器写记录只经 `stepTransition`：v1.21.0 起语法守卫，本版改为 `go/types` 全包检查（请求只能在 `stepTransition` 里产生、不能改写、`Store.Apply` 只在那里调用） | SAGA-8 / SAGA-15 |
| `TestStepTransitionAloneDecidesTheIncarnation` | 同上 | 代际只由 `stepTransition` 按 before 与原因决定（本版新增；原负对照 `TestStepTransitionGuardSeesBypassesWithoutALiteral` 与 `saga/testdata/stepguard` 随 RR-20261006-14 删除） | SAGA-15 |
| `TestMongoStepAttemptsOfOneOperationTakeEffectOnce` / `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce` | `saga/mongo_step_operation_promises_test.go` / `saga/mongo_step_operation_real_mongo_integration_test.go` | Mongo 步骤操作实例最多一次（mongotest 与真实副本集同一份用例） | SAGA-9 |
| `TestMongoStepConsumerFollowsTheOperationInbox` | `saga/mongo_step_consumer_promises_test.go` | Mongo 步骤消费者分支 | SAGA-9 |
| `TestCompletionConsumersTermTheSameTerminalErrors` | `saga/completion_consumer_terminal_promises_test.go` | 两条结果流同一终态分类（O-S5-1） | SAGA-9 |
| `TestRealSagaCrossProcessKillRecovers`（断言加强为每个操作恰好一次提交） | `saga/cross_process_real_integration_test.go`（integration） | 跨进程强杀 | SAGA-7 / SAGA-9 |
| `TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`、`TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence` | `saga/completion_definition_rollout_promises_test.go` | 定义缺失可重试、最终由 fence 收尾 | SAGA-10 |
| `BenchmarkRealMongoStepLatencyBreakdown`、`BenchmarkRealMongoStepThroughput`、`BenchmarkRealMongoCommitWriteConcern` | `saga/mongo_step_latency_real_mongo_integration_test.go`（integration，只在 `-bench` 下运行） | 可复跑的延迟分析基准 | SAGA-11 |
| `TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts`、`TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom` | `kit/saga/effect_retention_promises_test.go` | O-S5-2 跨 Mod 校验 | SAGA-12 |
| `TestRealNatsCompletionNakBackoffAndMaxDeliver` | `saga/consumer_nak_maxdeliver_real_integration_test.go`（integration） | 真实 JetStream 上 nak 退避与 `MaxDeliver` | SAGA-13 |
| `TestOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife` / `TestRealMongoOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife` | `saga/step_operation_attempt_cap_promises_test.go` / `saga/mongo_step_operation_real_mongo_integration_test.go`（integration） | 跨 Resume 累积的尝试不挡新一生、整个操作只有一份状态文档（RR-20261006-15 的承诺） | SAGA-14 |
| `TestOperationStateKeepsTheRefusalsOfTheTwoNewestLives`、`TestOperationStateRemembersTheLatestSupersededAttempts` | `saga/step_operation_state_promises_test.go` | 状态文档的两段有界历史：拒绝两生、被接替 16 条 | SAGA-14 |
| `TestDataEngineOperationStateSatisfiesProjectorFencePredicate` | `saga/dataengine_step_inbox_test.go` | 投影的 fence 谓词逐字段匹配状态文档 | SAGA-14 |
| `TestRealMongoStepProcessesFenceAttemptsInFlightAcrossProcesses`、`TestRealMongoStepProcessesConcurrentAttemptsTakeEffectOnce` | `saga/mongo_step_multiprocess_real_mongo_integration_test.go`（integration） | 两个步骤进程：被杀后接替、停住的提交被 fence、并发至多一次 | SAGA-14 |
| `BenchmarkRealMongoNativeReserveThroughput` | `saga/step_operation_benchmark_real_mongo_integration_test.go`（integration，只在 `-bench` 下运行） | 原生 Reserve 吞吐基准 | SAGA-14 |
| `TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt`、`TestNativeStepReplayedRefusalOfAnEarlierAttemptIsAccepted` | `saga/saga_direction_3_4_promises_test.go` | 同一生较早尝试的可重试失败不接收；回放的较早拒绝仍接收（方向 ③） | SAGA-16 |
| `TestNativeStepLateSuccessReopensAFailedSagaToCompensateTheStep`、`TestNativeStepLateSuccessAfterCompensatedCompensatesOnlyThatStep`、`TestNativeStepLateSuccessDuringAnInFlightCompensationIsCompensatedNext`、`TestNativeStepLateSuccessOnManualRequiredIsCompensatedOnResume`、`TestNativeStepLateCompensationSuccessIsOnlyAlarmed` | 同上 | 放弃后迟到的正向成功只补偿这一步、终态可重开、在途补偿不打断、`ManualRequired` 等 Resume；补偿方向只告警（方向 ④） | SAGA-16 |
| `TestMongoStoreIgnoresARetryableFailureOfAnEarlierAttempt`、`TestMongoStoreLateStepCompensationRoundTrip` | 同上 | MongoStore 上的 ③；`late_step` / `late_data` 往返与同一事务 | SAGA-16 |
| `TestRealMongoLateSuccessReopensACompensatedSagaOnce` | `saga/late_step_real_mongo_integration_test.go`（integration） | 同一迟到成功 8 路并发 × 10 轮，每轮恰好一次重开 | SAGA-16 |
| `TestReopeningAFailedSagaIsCountedAndLogged`、`TestReopeningACompensatedSagaIsCountedAndLogged`、`TestLateSuccessDuringCompensationIsNotAReopen`、`TestResumeCompensatingALateStepIsCountedAndLogged`、`TestPlainResumeIsNotAReopen`、`TestManualCompensateOfALateStepIsCountedAndLogged` | `saga/saga_reopen_observability_promises_test.go` | `saga.reopened_total` 三种 `reason` 各计一次、WARN 字段、非重开不计 | SAGA-17 |
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
| `TestClientSubscribeAndJetStreamAfterCloseReportErrClosed`、`TestRPCCallAsyncAfterAssemblyCloseReportsErrClosed` | `nats/driver/close_contract_promises_test.go` | RR-20261006-24：关闭后订阅 / JetStream / CallAsync 也 `errors.Is(fnats.ErrClosed)` | DRV-5 |
| `TestEveryExportedDriverMethodHasAClosedStateCheck`、`TestEveryExportedDriverMethodReportsErrClosedAfterClose`、`TestACallAdmittedBeforeCloseFinishingAfterItReportsErrClosed`、`TestInFlightCallAsyncExpiresAsErrClosedAfterClientClose`、`TestCallsRacingCloseEitherCompleteOrReportErrClosed` | `nats/driver/closed_state_guard_promises_test.go` | nats 驱动自持关闭状态：导出方法登记守卫、两种关闭后逐个方法、屏障、在途 CallAsync、并发 | DRV-5 |
| `TestRealNatsEveryExportedMethodAnswersFromTheDriverStateAfterAnUndrainedClose` | `nats/driver/closed_state_guard_real_promises_test.go`（integration） | 真实 NATS 硬关后 `DRAINING_PUBS` 窗口里每个方法按驱动状态回答 | DRV-5 |
| `TestRealNatsModCloseContract`、`TestRealNatsModUndrainedCloseIsReportedOnce` | `kit/nats/close_contract_real_promises_test.go`（integration） | RR-24 真实 NATS 11 个调用；RR-26 硬关后 `Connected()` 一直 false | DRV-5 |
| `TestIndexCreationRetriesThroughAnElectionWithinBounds` | `mongo/driver/election_retry_promises_test.go` | 选举重试四种情形 | DRV-6 |
| `TestMirrorLocalOwnerStorageInitSurvivesAMongoElection` | `remoteentity/owner_startup_election_integration_test.go` | 私有副本集 stepDown 期间 owner 初始化（integration） | DRV-6 |
| `TestAnUpdateWhoseReplyIsLostIsNotAppliedTwiceWhenTheCallerRetries`、`TestACreateWhoseReplyIsLostIsNotReportedAsTakenWhenTheCallerRetries`、`TestAnUpdateWhoseReplyIsLostStaysAppliedOnceAfterSomeoneElseWritesOnTop`、`TestAnUpdateThatNeverRanIsResentOnce` | `versionstore/write_token_promises_test.go` | 回复丢失按令牌核对：不叠加、不把自己的写判成冲突、没执行才重发 | DRV-7 |
| `TestResumeReturnsTheEarlierWriteInsteadOfWritingAgain`、`TestResumeAfterTheEarlierWriteProvablyLostPerformsTheWrite`、`TestResumeRefusesTheSameTokenForADifferentWrite` | 同上 | `Resume` 续核与防误用 | DRV-7 |
| `TestATokenPushedOutOfTheHistoryIsNotGuessed`、`TestAbsenceProvesNothing`、`TestTheEnvelopeKeepsABoundedTokenHistory` | 同上 | 证明不了就返回结果未知（含墓碑选 A 的两种情况）、令牌有界 | DRV-7 |
| `TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken`、`TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken` | `versionstore/write_token_integration_test.go`（integration） | 真实 Redis 单机 / 私有 Cluster 3 主 3 从 | DRV-7 |
| `TestAnIndexedWriteWithANaNScoreChangesNothing` / `TestRealRedisAnIndexedWriteWithANaNScoreChangesNothing` | `versionstore/index_score_promises_test.go` / `versionstore/write_token_integration_test.go`（integration） | 带索引写 NaN 分数什么都不写（RR-20261006-35） | DRV-8 |
| `TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync` | `codegen/internal/dao/transient_field_promises_test.go` | `nopersist,nosync` 有 mutator、不进存储 / 同步 | DAO-1 |
| `TestATransientFieldRollsBackWithTheTransaction` / `…NeverReachesTheCommitRecordOrSync` / `…IsInTheStateSnapshot` | `codegen/internal/dao/testdata/runtime/transient_test.go` | daoruntime 运行验证 | DAO-1 |
| `TestComponentRecordingItsOwnUndoIsHinted` | `cmd/glsvet/main_test.go` | 组件方法登记 undo 被提示（不计失败） | DAO-1 |
| `TestAttributeRollbackIsTheDaoRollback` / `TestNonPersistentAttributeLayersStayOutOfTheWAL` / `TestTimerRollbackIsTheDaoRollback` / `TestTimerBookkeepingStaysOutOfTheCommitRecord` | `demo/game/entities/{player,world}/*_component_test.go.tmpl` | 生成 game-demo 的组合回滚、WAL 不含非持久字段 | DAO-1 |
| `TestCombatRollbackIsTheDaoRollback` | `skill/combatcomponent/dao_rollback_promises_test.go` | 两策略 × 两失败路径字节一致 | DAO-1 |
| `TestSkillPackagesGetNoComponentUndoHint` | `cmd/glsvet/main_test.go` | skill 四个包零 A1 提示 | DAO-2 |
| `TestComponentRecordingUndoThroughHelperIsHinted` | `cmd/glsvet/main_test.go` | 经一层同包 helper 登记 undo 被提示；调 DAO setter 与两层链不提示（RR-20261006-13） | DAO-4 |
| `TestComponentFieldWritesOutsideTheDaoAreHinted`、`TestSkillPackagesGetNoComponentFieldHint`、`TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix` | `cmd/glsvet/componentfields_promises_test.go` | 组件字段写提示与豁免；skill 四个包零字段写提示；`roost:nest` 文档标注生效 | DAO-4 |
| `TestBuffAttributeModifierReachesDamage` / `TestAttributeProjectionRollsBackWithTheDao` / `TestAttributeProjectionReprojectsOnLoad` | `skill/combatcomponent/attribute_projection_promises_test.go` | buff 进伤害、投影随 DAO 回滚、加载重投影不标脏 | DAO-3 |
| `TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2` 等 `TestB2*` 6 条 | `remoteentity/snapshot_l2_watermark_promises_test.go` | 陈旧上限、未确认不交出、补写丢失的 L2 写 / 删除、O5、并发收敛 | REM-1 |
| `TestRealB2WatermarkMatrixStandalone` / `…Cluster` | `remoteentity/snapshot_l2_watermark_matrix_integration_test.go` | 真实 Redis 单机 / Cluster 各 20 格水位矩阵 | REM-1 |
| `TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect` | `remoteentity/snapshot_replay_jetstream_integration_test.go` | O5：DeliverAll 重放不复活 | REM-1 |
| `TestCachedMaxStalenessConfiguration` | `kit/remoteentity/cached_max_staleness_test.go` | 新键严格读取 | REM-1 |
| `TestRemoteSnapshotCacheWritesStayInTheListedFunctions` | `entity/remote_snapshot_write_guard_test.go` | 快照缓存直接写 L1 / L2 只在封闭表里的函数、分片锁 helper 只在持锁处调用（本版结构守卫） | REM-1 |
| `TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition` | `entity/remote_mirror_promises_test.go` | token 排序、DTO 副本、无注册冲突、读侧身份、读出口后置条件 | REM-2 |
| `TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce` / `…BelowMinimumLoadsAuthorityOnce` | `remoteentity/snapshot_read_exit_promises_test.go` | 一次 Monotonic 只回源一次 | REM-2 |
| `TestSnapshotClientHasNoWriteCapability`、`…LinearizableNeedsADeclaredLoader`、`…StartFailureLeavesNoSubscription`、`…StopContract`、`…StopCancelsLoads`、`…ReadsWhatTheOwnerPublishesInTheSameProcess` | `remoteentity/snapshot_client_promises_test.go` | 只读、线性化门、启动回收、三步停机 | REM-2 |
| `TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot` | `nest/remote_cached_allow_stale_promises_test.go` | allow_stale 的 Cached 访问 | REM-3 |
| `TestSnapshotBootstrapBuffersDeltaDuringFirstLoad`、`TestSnapshotBootstrapReplayMatrix`、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`、`TestInterestRenewReleaseConvergesInEveryDeliveryOrder`、`TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 首载缓冲、溢出再回源、renew / release 乱序收敛、退化 | REM-4 |
| `TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable` | `sync/syncbus/driver/jetstream_live_promises_test.go` | DeliverNew durable 与 DeliverAll 分开 | REM-4 |
| `TestRealJetStreamLiveSubscriptionConfirmsAndResumes`、`TestRealJetStreamLiveSnapshotPushReachesTheReader`、`TestRealJetStreamLiveDurableNameShape` | `remoteentity/mirror_step4_jetstream_integration_test.go` | 真实 JetStream 确认订阅与推送；服务端 durable 名的实际形状（本版） | REM-4 |
| `TestInterestCapacityIsPerConsumer`、`TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 按 consumer 配额、拒绝可见 | REM-5 |
| `TestInterestPerConsumerConfiguration` | `kit/remoteentity/interest_quota_config_test.go` | 新键严格读取与范围 | REM-5 |
| `TestRealJetStreamInterestHandlerErrorIsAcknowledged` | `remoteentity/interest_handler_error_jetstream_integration_test.go`（integration） | 兴趣 handler 出错时 JetStream 结算为 Ack、不重投（本版） | REM-5 |
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
| `TestInterestReleaseOnAFullRegistryStillFencesTheLateRenewal` | `remoteentity/interest_release_full_promises_test.go` | 表满时 release 记溢出水位，迟到的旧续租不复活 | REM-14 |
| `TestInterestOverflowFenceExpiresOneTTLAfterTheLastRelease`、`TestInterestOverflowFencesAreOnePerConsumerAndReclaimedOnExpiry` | `remoteentity/interest_overflow_expiry_promises_test.go` | 溢出水位到期时刻、每 consumer 一个、过期回收 | REM-14 |
| `TestSubscriptionUnsubscribeStopContract`、`TestSubscriptionRefusesDeliveriesAfterUnsubscribe`、`TestSubscriptionSelfUnsubscribeWaitsOnlyForOthers`、`TestSubscriptionSelfUnsubscribeInReentrantDelivery`、`TestSubscriptionSelfUnsubscribeWithForeignContextTimesOut`、`TestSubscriptionPanicReleasesAdmission` | `sync/syncbus/subscription_promises_test.go` | `Subscription.Unsubscribe(ctx)` 三步停机、回调里退订自己、无关 ctx 如实超时 | REM-15 |
| `TestUnsubscribeWaitsForInFlightHandler`、`TestUnsubscribeFromOwnHandlerDoesNotDeadlock`、`TestUnsubscribeStopContract` | `sync/syncbus/driver/unsubscribe_drain_promises_test.go` | JetStream 与普通 NATS 驱动的退订排空 | REM-15 |
| `TestRealSyncBusUnsubscribeDrainsTheSubscription`、`TestRealSyncBusUnsubscribeFromOwnHandler` | `kit/syncbus/unsubscribe_drain_integration_test.go`（integration） | 真实 nats-server 上同一组承诺 | REM-15 |
| `TestPatchSyncerStopWaitsForInFlightApply` / `TestReplicaSyncerStopWaitsForInFlightStoreWrite` | `sync/syncbus/patch_syncer_stop_promises_test.go` / `cache/replica_stop_promises_test.go` | RR-20261006-36：订阅方停止等在途回调 | REM-15 |

## 按包的条目索引

| 包（目录） | 条目 |
| --- | --- |
| `saga/`（`command_consumer.go`） | SAGA-1、SAGA-2、SAGA-9、SAGA-10、SAGA-14 |
| `saga/`（`step_operation_inbox.go`、`dataengine_step_inbox.go`） | SAGA-2、SAGA-6、SAGA-9、SAGA-14 |
| `saga/`（`engine.go`、`step_transition.go`） | SAGA-2、SAGA-6、SAGA-7、SAGA-8、SAGA-10、SAGA-15、SAGA-16、SAGA-17 |
| `saga/`（`mongo_store.go`、`store.go`） | SAGA-2、SAGA-6、SAGA-7、SAGA-16 |
| `saga/`（`record.go`） | SAGA-3、SAGA-4、SAGA-16（`LateStep` / `LateData`） |
| `saga/`（`nest_completion_consumer.go`、`jetstream.go`） | SAGA-9、SAGA-10 |
| `saga/`（仅测试） | SAGA-11、SAGA-13、SAGA-14（两进程用例、原生 Reserve 基准）、SAGA-15（类型守卫） |
| `kit/saga/` | SAGA-3、SAGA-4、SAGA-5、SAGA-12、SAGA-17（健康消息 `reopened=`） |
| `kit/dataengine/`（`EffectStreamRetention`） | SAGA-12 |
| `dataengine/`（`lease_fence.go`，未改，契约依赖；`engine/lease_fence_integration_test.go` 本版改为状态文档） | SAGA-2、SAGA-14 |
| `codegen/internal/roost/`（`add.go`、`catalog.go`、`demo.go`）与 `demo/` 模板 | SAGA-2、SAGA-3、SAGA-9 |
| 文档：`SAGA.md`（含「操作状态文档」「运维观察」）、`docs/USER_GUIDE.md`、`docs/TROUBLESHOOTING.md`（T-225 / T-226 / T-281 / T-292） | SAGA-1、SAGA-2、SAGA-6、SAGA-9、SAGA-10、SAGA-12、SAGA-14、SAGA-16、SAGA-17 |
| `redis/driver` | DRV-1、DRV-3、DRV-4、DRV-5 |
| `redis`（`fredis` 接口；`cas.go` 的 NaN 检查） | DRV-4、DRV-8 |
| `mongo`（`fmongo`） | DRV-3 |
| `mongo/driver` | DRV-2、DRV-3、DRV-5、DRV-6 |
| `etcd/driver`、`nats/driver`（nats 本版自持关闭状态） | DRV-5 |
| `internal/operation` | DRV-5 |
| `kit/redis`、`kit/mongo`、`kit/nats` | DRV-5（kit/redis 单实例锁的 `MaxRetries=-1` 与 DRV-3 相关；kit/nats 的真实 NATS Close 用例） |
| `kit/remoteentity` | DRV-4 |
| `remoteentity` | DRV-4、DRV-6（调用方与集成用例） |
| `kit/remoteentity`（`config.go`，A4 ① 之后的声明） | DRV-4、REM-1、REM-5、REM-6、REM-13 |
| `versionstore` | DRV-1、DRV-7（`write_token.go`、`redis_store.go`）、DRV-8（替身与用例） |
| `cache` | DRV-3 |
| `codegen/internal/dao`、`codegen/internal/roost` | DAO-1 |
| `demo`（game-demo 模板） | DAO-1 |
| `cmd/glsvet` | DAO-1、DAO-2、DAO-4 |
| `skill/combatcomponent` | DAO-1、DAO-3 |
| `skill/examples/statusbridge` | DAO-3 |
| `docs/skill`、`docs/agent-skills/roost-coding` | DAO-1、DAO-2、DAO-3、DAO-4、DRV-5、REM-15 |
| `entity/` | REM-1（含写入点守卫）、REM-2、REM-4 |
| `remoteentity/` | REM-1、REM-2、REM-4、REM-5、REM-6（只读 Mongo loader）、REM-7、REM-8（基准）、REM-9、REM-10、REM-11、REM-12、REM-14 |
| `sync/syncbus/`（`sync.go`、`subscription.go`、`patch_syncer.go`、`driver/`、`mirror/`） | REM-4、REM-15 |
| `cache/`（`mirror.go`）、`syncstream/` | REM-15（RR-20261006-36） |
| `kit/remoteentity/` | REM-1、REM-5、REM-6、REM-10 |
| `kit/mods/` | REM-6、REM-10 |
| `app/`（`singleton.go`；`config_validation.go` 的键清单已由 A4 ① 删除） | REM-10 |
| `nest/` | REM-3 |
| `codegen/internal/entity/`（含 `testdata/remoteflow`） | REM-6、REM-8 |
| `codegen/internal/roost/` | REM-13 |
| `scripts/mirror-local.sh` | REM-8、REM-12 |

## 未验证 / 外部验证项

本部分的未验证项**只有外部环境类**，统一登记在 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)，不阻塞发版：

| 编号 | 项 | 相关条目 |
| --- | --- | --- |
| E01 | Mirror 七类场景在 Linux 内核网络下 | REM-2、REM-4、REM-8 |
| E02 | 跨主机真实分区、MTU、时钟偏差 | SAGA-2、SAGA-9、SAGA-16、REM-1、REM-2、REM-8、REM-9 |
| E06 | NATS JetStream 多节点 HA | SAGA-1、SAGA-10、SAGA-13、REM-4、REM-15 |
| E08 | 多机 Redis Cluster：驱动（写不重放、MOVED / ASK）、单实例锁、Lua 布局 | DRV-1、DRV-3、DRV-4、DRV-7、REM-1、REM-10、REM-12 |
| E10 | Redis 异步复制丢写与切主：墓碑、`WAIT`、锁 | DRV-4、DRV-7、REM-1、REM-11、REM-12 |
| E11 | Mongo 跨主机副本集、mongos、切主中提交 | DRV-2、DRV-3、DRV-6、SAGA-9、SAGA-14 |
| E12 | saga Mongo 步骤延迟的生产形态 | SAGA-9、SAGA-11、SAGA-14 |
| E13 | 多主机强杀：owner / 锁迁移、双实例、旧回调 | SAGA-2、SAGA-16、REM-5、REM-9、REM-10、REM-14 |
| E14 | Remote outbox“Mongo 已提交、发布前崩溃”精确注入 | REM-8 |
| E15 / E16 | Mirror 长时间容量 / 大规模扇出 | REM-2、REM-4、REM-5、REM-8、REM-14 |

初稿里各条自记的本机可做项，本轮的去向（条目正文已改）：

| 初稿未验证项 | 去向 |
| --- | --- |
| 新旧步骤进程 / 协调器混跑（SAGA-2、SAGA-6、SAGA-7、SAGA-9） | 不在范围：维护者 2026-10-06“不考虑旧进程，完成按照新的处理，线上还没有旧的进程跑”，升级先停旧再起新（SAGA-14） |
| SAGA-1 真实 NATS 上“返回 nil 即 ack、释放 `MaxAckPending` 位” | 实测：`TestRealNatsCompletionNakBackoffAndMaxDeliver`（SAGA-13）、`TestRealJetStreamInterestHandlerErrorIsAcknowledged`（REM-5） |
| SAGA-2 投影积压超过 `Timeout`、“截止前投影、放弃后送达” | 确定性用例 `TestNativeStepTakesEffectAtMostOncePerOperation/d`、`/c'`（真实消费者 + 真实投影器） |
| SAGA-3 / 4 / 5 两条按源码推断的预算行为 | 源码核对后写成事实，要不要改进列为 review 检查点 |
| SAGA-6 去掉 `$exists` 的负对照 | 改为 review 检查点（用例强度问题）；告警只标记一次已由 `TestRealMongoLateSuccessAlarmIsMarkedOnce` 实测 |
| SAGA-7 真实 Mongo 上 NC-250 触发 | 关闭写走 `MongoStore.Apply` 既有 `CloseOperation` 分支，真实副本集上由跨进程强杀与协调器接管用例经过 |
| SAGA-10 滚动发布 | 错误分类由确定性用例模拟，真实 JetStream 退避与 `MaxDeliver` 由 SAGA-13 实测 |
| SAGA-11 64 个以上协程、选项 C | 维护者选 A，B / C 不实施也不实测；生产形态归 E12 |
| DRV-5 Close 未用真实 Mongo / etcd / NATS | Close 契约在包装层实现，用例驱动同一代码路径；断开动作属驱动库自身行为（条目写明）。NATS 部分已在真实 NATS（私有 JetStream 3 节点）上实测：真实进程演练第 4 项（RR-20261006-24 / -26，`0db819b0`）与自持关闭状态的窗口用例（`f0de827a`） |
| DAO-1 真实三进程被拒提交 | 与真实 Nest 引擎 + 文件 WAL 覆盖的是同一个 `rejectCommit` 路径 |
| REM-4 durable 名（推断） | 实测：`TestRealJetStreamLiveDurableNameShape`（`155b9f91`） |
| REM-4 表满时 release 不留撤销水位（推断） | 转 RR-20261006-11 修复（`155b9f91`），到期实测（`d5682dc4`），见 REM-14 |
| REM-5 owner 拒绝时 JetStream 是否重投 | 实测为 Ack（`d5682dc4`，MIRROR-STEP-4 §6.10） |
| REM-6 生成 spec 不带 Tenant / Policy；REM-7 pipelined / Durability 0；REM-11 同值 CAS 续期；REM-12 MIGRATING 时键在源上；REM-13 旧 core 读新生成配置 | 源码核对后写成事实（位置见实现条目第 8 节） |

## 文档与源码不一致（初稿登记项的处理结果）

初稿按 `02c8a10d` 登记的既有文档 / 注释与源码不一致，全部已处理（以源码为准）；“不改”的只有历史提交说明与原话转录两类：

| 不一致 | 处理 |
| --- | --- |
| `CHANGELOG.md` `[v1.21.0]` O-S5-1 写 5 种 Term 错误（实际 4 种，`ErrDefinitionMissing` 已由 `5a3c4a60` 移出） | 本轮在该条加更正注（`saga/nest_completion_consumer.go:150` `isTerminalCompletionError`） |
| SAGA-DIRECTION“O-S5-1”段同样列 5 种 | 本轮加括号更正 |
| SAGA-DIRECTION“① 目标”段 `stepTransition` 的方案稿签名 | 本轮加更正注：实施时的签名，以及 RR-20261006-14 之后的 `func (e *Engine) stepTransition(ctx, before, after Record, t transition) (Record, ApplyOutcome, error)`（`saga/step_transition.go:66`） |
| SAGA-DIRECTION“防止新出口绕过”段对守卫范围的描述窄于源码 | 不构成缺陷；守卫已由 RR-20261006-14 改为 `go/types` 全包检查（SAGA-15） |
| `docs/bugfix/RR-20261005-NC-250.md` 的 `abandonedOperation` | 本轮加更正段：已由 `openOperation`（`saga/step_transition.go:88`）与 `stepTransition`（`:66`）取代 |
| U-0280 记录“根因”与 B1 方案第 6 节的旧文件 / 行号 | 本轮加更正注：`reserveInTransaction` `saga/step_operation_inbox.go:178`、`commandIncarnation` `:492`、`readReceipt` `saga/dataengine_step_inbox.go:156`；改为状态文档之后 claim 不再存在 |
| B2 方案验证段的测试名 `TestRemoteSnapshotDeleteAtVersionPromiseClearedByNewerSnapshot`（源码没有这个名字） | 本轮更正为 `TestRemoteSnapshotDeleteAtVersionPromiseFencesOlderSnapshot`（`entity/snapshot_delete_version_promises_test.go:47`） |
| B2 §2、`entity/remote_snapshot.go` 类型注释、`remoteentity/snapshot_client.go:35` 注释、DECISIONS 第九轮“L1 写入只经 `admitLocked`” | `155b9f91` 按源码改准（新值经 `admitLocked`，回填与删除几处直接写 L1，都在分片锁下），并加守卫 `TestRemoteSnapshotCacheWritesStayInTheListedFunctions`（REM-1） |
| `entity/remote_mirror.go` `RemoteSnapshotRead.After` 注释过时 | `155b9f91` 已改为“不满足返回 `ErrRemoteSnapshotStale`”（`entity/remote_mirror.go:78-80`） |
| MIRROR-M6-OBSERVATIONS §3 墓碑 WAIT 结果写 `redirected` / `unsupported` | `155b9f91` 更正为五个标签值，跳过统一记 `skipped`（`remoteentity/snapshot_l2.go:357-381`） |
| MIRROR-M6-OBSERVATIONS §2 缺 `stopped` 与 `interest_refresh_sent_total` | `155b9f91` 补上（`remoteentity/interest_refresh.go:125`、`:73`） |
| MIRROR-STEP-4 与 USER_GUIDE 的 durable 名 `sync_remote_entity_snapshot.live_…` | `155b9f91` 按隔离 NATS 实测改为 `sync_remote_entity_snapshot_live_<sid>_<16 位十六进制>` |
| USER_GUIDE:343 Mirror 第 4 步“main 未发版” | 本轮改为“v1.21.0 起” |
| TROUBLESHOOTING T-259 A2“main，未发版” | 本轮改为 v1.20.2 |
| L2 落后上界与兴趣配额只写 core 缺省（约 5m30s、16384） | `155b9f91` 在 USER_GUIDE、B2 §7、MIRROR-STEP-4 §4、DECISIONS 第十二轮分开写明生成模板口径（约 10m30s、6250） |
| DECISIONS-PENDING 文首与第十二轮“驱动 Close 契约”行仍写 W-2026-10-06-02 待判断 / 代码未改 | 文首由 `b7471ae4` 注明已转 RR；第十二轮行本轮加更正（`d05a04a1`，RR-20261006-10） |
| O-M6-5 “10 次 × 1s”没写明按索引计 | CHANGELOG 与第十二轮 kit 批 §7 由 `155b9f91` 写清（每个索引各 10 次，最坏约 N × 10s，`mongo/driver/collection.go:251-252`）；DECISIONS 第十二轮行本轮加注 |
| 两套 Mongo 步骤延迟数字没注明来源轮次 | `5ca32611` 在两份 feature 文档里标明出处 |
| 测试名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry` 名不副实 | `5ca32611` 改名为 `TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`（`redis/driver/script_no_retry_promises_test.go:167`） |
| 提交 `81659082` 说明里的 T 号笔误（T-231/232 应为 T-238/239） | 不改历史提交；以 TROUBLESHOOTING 为准 |
| 维护者原话两处转录差异（第十二轮“55tps”、第二轮 A1） | 不改原文；发版文档以 DECISIONS-PENDING 为准引用 |
| 覆盖观察：`writeCalls` 的 `set` 实为 `SetNX`；`TestSkillPackagesGetNoComponentUndoHint` 不扫 `skill/skillcompose` 与 `skill/examples/*` | 不是文档冲突；列为 DRV-3 / DAO-2 的 review 检查点 |
| 源码内部语义差异：本机兴趣表满时 `RemoteEntityMod` 报 Fail、`RemoteMirrorMod` 报 Degraded | 不是文档冲突；列为 REM-6 的 review 检查点 |

本轮（`5e72ca4d`）新发现与处理：

| 不一致 | 处理 |
| --- | --- |
| `docs/TROUBLESHOOTING.md` T-226 现象列 (2) 写“修复后出现 ERROR `saga: step succeeded after the coordinator abandoned it; the effect is not compensated`” | **未处理（共享文档，不在本册文件范围，交汇总者）**：`a6a902cd` 起源码已没有这句——正向是 WARN `...; compensating it`（`saga/engine.go:639`），补偿方向与防御分支是 ERROR `...; the coordinator cannot account for it`（`:681`）；T-226 处置列末尾“saga 方向 ③④ 之后”一段已写对，只差现象列这一句。本册各条目已按源码写，并注明 v1.20.1～v1.22.0 的旧文本 |
| 本册上一版 DRV-5 源码表两行（`nats/driver/assembly.go` 的 `closed` 字段、`Client.Publish` 关闭后同时 `errors.Is` 到 gonats 原错误） | 本轮按 `f0de827a` 改写（natsstate 方案 §7 点名） |
| 本册上一版各处“留到下个大版本”（A2 ③、saga ③④）与 SAGA 里“③④ 暂不做” | 本轮改为“第十三轮本版完成”，指向 DRV-7 / SAGA-16 / REM-15 |
| 本册上一版引用的 `kit/*` 手写配置读取（`readStepBudget`、`readSnapshotConfig`、`read.Err()`）、`app/config_validation.go` 的键清单与 `codegen/internal/roost/catalog.go` 的模板字符串 | A4 ①（`d1226825`）之后都已删除；本轮改指向各 Mod 的配置声明（`kit/saga/config.go`、`kit/remoteentity/config.go`、`kit/dataengine/mod.go` 的 `EffectsConfig`）与生成快照 `codegen/internal/roost/kitconfig_gen.go` |
| 本册上一版 SAGA-2 列的复核 1 用例 `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed` | 已被方向 ③ 推翻并删除（`a6a902cd`），本轮各表改为 `TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt`（SAGA-16）与 `TestMongoStoreTombstoneOfAFailureCloseIsAbandoned` |
