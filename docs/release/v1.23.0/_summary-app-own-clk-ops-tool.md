# 汇总用摘要 · APP / OWN / CLK / OPS / TOOL 分册

给写 `v1.23.0-GUIDE.md` / `v1.23.0-IMPLEMENTATION.md` 总目录与总表的汇总者用。分册正文：[说明](guide-app-own-clk-ops-tool.md) · [实现](impl-app-own-clk-ops-tool.md)。源码基准 `02c8a10d`。条目锚点在两份分册里都是小写编号（`#app-1`、`#own-3`、`#tool-7` …）。

## 1. 条目总表（39 条）

| 编号 | 一句话 | 首发版本 | 行为变化 | 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- | --- |
| APP-1 | App 单实例锁：任何 Mod Init 之前取 Redis 单键锁，失锁 fail-stop | v1.20.0 | 是 | 否 | 是（手工装配） |
| APP-2 | 生成器装配单实例锁、停机预算 +3s、启动等待；etcd `service_prefix` 与租约过期注销 | v1.20.0 | 是 | 否 | 是（已生成工程可选） |
| APP-3 | 删 global 租约 API，停发 `redis.lock` / `etcd.election` capability | v1.20.0 | 是 | 是 | 是 |
| APP-4 | `RuntimeFailure.OnFail` 统一 fail-stop：先围栏 Nest；停机期 fail-stop 非零退出 | v1.20.0 / v1.21.0 | 是 | 否 | 否 |
| APP-5 | `SingletonLiveness.Live`；停机中仍算活（C5） | v1.20.0 / v1.21.0 | 否 | 否 | 否 |
| APP-6 | `operation.Lifetime`、停机契约骨架、glsvet stophints（A3） | v1.20.2 | 极小 | 否 | 否 |
| APP-7 | `operation.Serial`；kit Mod 停止收敛（NC-233 / 234） | v1.23.0；NC-233 / 234 v1.21.0 | 是 | 小 | 否 |
| APP-8 | 停机 lifecycle hook 受 `shutdown.total_timeout` 约束（NC-231） | v1.21.0 | 是 | 否 | 否 |
| APP-9 | 启动失败先调 `Service.Shutdown` 收回（NC-193） | v1.20.2 | 是 | 是（契约） | 是 |
| APP-10 | `/readyz` Degraded 算就绪（D1） | v1.21.0 | 是 | 是（`Snapshot.OK` 含义） | 视情况 |
| APP-11 | `/readyz` 每个 checker 1.5s 期限 | v1.23.0 | 是 | 否 | 视情况 |
| APP-12 | 退出原因写进文件日志（RR-20261006-07） | v1.23.0 | 是 | 否 | 否 |
| APP-13 | App 能力 `singleton` / `clock.business` / `singleton_incarnation` 与 Mod 依赖 | v1.20.0 / v1.21.0 / v1.23.0 | 是 | 否 | 是 |
| OWN-1 | 租约状态机期三处修复（RR-20261004-10 / 11 / 14），同版本被取代 | v1.20.0 | 否 | 否 | 否 |
| OWN-2 | game-demo 玩家所有权静态绑定 | v1.20.0 | 是 | 是（模板） | 是 |
| OWN-3 | 赠礼按 `FromSID` 准入与转交；退款预算 | v1.20.0（预算入配置 v1.20.1） | 是 | 是（模板） | 是 |
| OWN-4 | activity 改用 `Live`；候选校验（RR-20261005-01） | v1.20.0 / v1.20.1 | 是 | 否 | 是 |
| OWN-5 | 活动组文件，每组 ≤ 64（C4） | v1.20.2 | 是 | 是 | 是 |
| OWN-6 | global `Bind` 重试幂等（RR-20261006-05） | v1.23.0 | 是（放宽） | 否 | 否 |
| CLK-1 | 业务时钟 / 系统时钟分开，生产偏移为 0（D-L3） | v1.21.0 | 是 | 是（生产偏移） | 是 |
| CLK-2 | match / chat 展示 / account 换钟，doctor 偏移一致 | v1.21.0 | 是 | 否 | 是 |
| CLK-3 | 业务时间只许前进（高水位） | v1.22.0 | 是 | 是（测试环境） | 是 |
| CLK-4 | activity / mail 回业务钟，删 `SystemNow` / `StorageGrace` | v1.22.0（拆分 v1.21.0） | 偏移 0 时无 | 是 | 是（手工装配） |
| CLK-5 | `app.business_time.advance_failed.total` | v1.23.0 | 新指标 | 否 | 否 |
| CLK-6 | timer 同期限按 priority / 登记顺序；未注册类型告警计数（D-L1 / D-L2） | v1.21.0 | 是 | 否 | 否 |
| OPS-1 | 服务指标默认进 metrics 注册表（C6） | v1.20.2 | 是 | 是（指标名） | 是 |
| OPS-2 | `metrics.DeleteSeries`；loadtest 运行序列随历史删除 | v1.23.0 | 是 | 否 | 否 |
| OPS-3 | Ops 同步 bind（NC-230）；`ops.admin_timeout` | v1.21.0 | 是 | 是 | 是 |
| OPS-4 | Ops `Authorization` 只认 `Bearer` | v1.23.0 | 是 | 是 | 是 |
| OPS-5 | CAS 冲突在 versionstore 统一计数 | v1.23.0 | 是 | 是（指标口径） | 是 |
| OPS-6 | game-demo 仪表盘补两个面板 | v1.23.0 | 否 | 否 | 否 |
| OPS-7 | robot Stage 序号只增不回收（RR-20261006-09） | v1.23.0 | 是 | 否 | 视情况 |
| TOOL-1 | pretag：失败打印；origin 不可达失败（NC-205） | v1.20.0 / v1.20.2 | 是 | 否 | 否 |
| TOOL-2 | `full-scenario-adds.sh` 一处定义，`source-head-check.sh` 不吞失败 | v1.23.0 | 是 | 否 | 否 |
| TOOL-3 | 根包冲突标记门禁 | v1.21.0 | 新门禁 | 否 | 否 |
| TOOL-4 | 根包示例实跑门禁 | v1.23.0 | 新门禁 | 否 | 否 |
| TOOL-5 | 根包文档相对链接门禁 | v1.23.0 | 新门禁 | 否 | 否 |
| TOOL-6 | `scripts/mirror-local.sh`（扩展 `test-core` 等） | v1.22.0（扩展 v1.23.0） | 新工具 | 否 | 否 |
| TOOL-7 | 故障矩阵无用例记 FAIL、全局命令持锁；本版预跑 21 格 PASS | v1.20.2（预跑 v1.23.0） | 是 | 否 | 否 |

按首发版本计：v1.20.0 有 APP-1～5、APP-13、OWN-1～4、TOOL-1（各含后续补修）；v1.20.1 无新条目（OWN-3 / OWN-4 的补充）；v1.20.2 有 APP-6、APP-9、OWN-5、OPS-1、TOOL-7；v1.21.0 有 APP-8、APP-10、CLK-1、CLK-2、CLK-6、OPS-3、TOOL-3；v1.22.0 有 CLK-3、CLK-4、TOOL-6；v1.23.0（本版）有 APP-7、APP-11、APP-12、OWN-6、CLK-5、OPS-2、OPS-4～7、TOOL-2、TOOL-4、TOOL-5。

## 2. 行为变化 / 兼容破坏 / 需业务或运维改动

### 2.1 兼容破坏（升级前必读）

| 条目 | 破坏内容 | 迁移 |
| --- | --- | --- |
| APP-3 | 删除 `global` 的 `AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames`、`GameLease` / `LeaseState`、`Config.Leases` / `LeaseTTL` / `NewIncarnation`、`DefaultLeaseTTL`、`MaxPageSize` / `MaxLoadEntries`、`RedisStores.Leases`；错误码 570105～570109 退役；删除 `mods.ModRedisLock` / `mods.ModEtcdElection` | 改用 App 单实例锁与 `Live`；键级锁 / 选主自己 `Assemble`；运维删 `<global.key_prefix>:lease:*` |
| APP-9 | `app.Service.Shutdown` 必须容忍部分初始化（Init 返回错误时也会被调用） | 自写 Service 检查 Shutdown |
| APP-10 | `health.Snapshot.OK` 含义从“全部 OK”变为“没有 Fail”；`/readyz` 降级时 200 | 需要旧语义用 `OK && !Degraded` |
| OWN-2 | game-demo 模板：`player_elsewhere` 含义收窄、`ErrLeaseNotHeld` → `ErrNotServedHere`、`PlayerOwners` 公开方法删除、`game_route` 配置删除、`game/playerroute` 删除 | 已生成工程不迁移；客户端按新含义处理 100015 |
| OWN-3 | `start_gift` 多参数 `fromSID`，`gift.Encode` 拒绝无 sid；旧赠礼载荷不执行 | 已生成工程手工合并 |
| OWN-5 | game 配置键 `activity.game_sids` 删除 | 新工程用 `configs/activity_groups.yaml` |
| CLK-1 | 生产 `time.logic_offset` 非 0 拒绝启动 | 生产偏移写 0 |
| CLK-3 | 非生产偏移回调超过“经过的真实时间 + 1 分钟”拒绝启动；非 0 偏移必须有协调存储 | 测试环境回到过去只能清库重建 |
| CLK-4 | 删 `activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`；`mail.DefaultEnvelopeStorageGrace` → `EnvelopeStorageGrace` | 手工装配删对应行 |
| OPS-1 | 服务指标名、`Recorder` 事件名（`depth:queue{…}`、`depth:board{…}`、`dropped:run.swept`）变化 | 看板 / 告警 / 项目测试调整 |
| OPS-3 | ops 端口占用启动失败；`/admin/execute` 10s 期限、504 | 同机多实例各配 `ops.addr`；长命令调大 `ops.admin_timeout` |
| OPS-4 | 裸 `Authorization: <token>` 401 | 改 `Bearer` 或 `X-Admin-Token` |
| OPS-5 | chat `conflict:append` / `conflict:prune`、rank `conflict:submit` 不再上报 | 改查 `versionstore_conflict_total{store}` / `versionstore_cas_total` |
| APP-7 | 单实例锁 store / kit Mod 重复 Close 返回 nil（原来报错或粘滞返回第一次的错误） | 依赖第二次 Close 报错的调用方改看命令错误 |

### 2.2 其他行为变化（不破坏，但可见）

APP-1（启用后崩溃重启最多等约 15s；Redis 连续不可用约 10s 进程 fail-stop）、APP-2（新工程 dataengine 服务默认启用单实例锁，`total_timeout` +3s）、APP-4（Remote Entity fatal 也围栏 Nest；停机期 fail-stop 非零退出）、APP-8（hook 超时 `run` 按预算返回、不停 Mod）、APP-11（>1.5s 的 checker 记 Fail）、APP-12（多一行 `app run failed`）、OWN-4（activity 需 singleton，崩溃进程最多算 15s）、OWN-6（同参数重试 `Bind` 成功）、CLK-2（chat 新字段 `SentAtUnix`）、CLK-6（同期限顺序确定）、OPS-2（挤出历史的 loadtest 运行序列从 `/metrics` 消失）、OPS-7（序号可能超过 `Count`）、TOOL-1（origin 不可达 pretag 失败）、TOOL-2（本地 full 场景检查不再吞失败、步骤与 CI 一致）。

### 2.3 需业务或运维改动的清单

见说明分册“[需要业务或运维改动的清单](guide-app-own-clk-ops-tool.md#需要业务或运维改动的清单)”，共 20 项；最需要在总表突出的四项：APP-3 仓外调用方迁移与删旧键、OPS-4 运维脚本改 Bearer、OPS-1 / OPS-5 看板改指标、CLK-3 测试环境不能再回拨偏移。

## 3. 新增的全局门禁 / 守卫测试

| 守卫 | 位置 | 首发 | 条目 |
| --- | --- | --- | --- |
| `TestNoMergeConflictMarkersInTrackedFiles` | 根包 `conflict_marker_gate_test.go` | v1.21.0 | TOOL-3 |
| `TestExamplesRun`（`exampleRuns` 登记表） | 根包 `examples_run_test.go` | v1.23.0 | TOOL-4 |
| `TestTrackedMarkdownRelativeLinksResolve` | 根包 `doc_links_gate_test.go` | v1.23.0 | TOOL-5 |
| `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed` | 根包 `ci_full_scenario_test.go` | v1.23.0 | TOOL-2 |
| `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock` | 根包 `acceptance_lock_promises_test.go` | v1.20.2 | TOOL-7 |
| `internal/stopcontract.Check` 停机契约骨架 | 各包 `stop_contract_test.go` | v1.20.2 | APP-6 |
| `glsvet -stophints`（只提示） | `cmd/glsvet/stophints.go` | v1.20.2 | APP-6 |
| `glsvet -clockhints` / `-businessdirs`（只提示） | `cmd/glsvet/clockhints.go` | v1.21.0 | CLK-1 |
| `roost project doctor` 的 `time:logic_offset` | `codegen/internal/roost/logic_offset_doctor.go` | v1.21.0 | CLK-2 |
| 单实例锁时间关系（`ValidateServiceConfig`） | `app/singleton.go` `validate` | v1.20.0 | APP-1 |
| 生产偏移为 0（`ValidateServiceConfig`） | `app/business_clock.go` `validateProductionLogicOffset` | v1.21.0 | CLK-1 |
| 业务时间高水位（启动拒绝） | `app/business_time.go` | v1.22.0 | CLK-3 |

roost-coding 规范同期新增的条款（规则源，不是测试）：“新的停机对象优先用共用类型并套契约骨架”与 `operation.Serial` / Close 统一口径（APP-6 / APP-7）、“业务时钟与系统时钟”（CLK-1 / CLK-3）、“示例要实跑，不能只编译”（TOOL-4）、“反复出问题要上报方向判断”（OWN-1 → OWN-2 的先例）。

## 4. 按包的条目索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | APP-1、APP-4、APP-5、APP-8、APP-9、APP-12、APP-13、CLK-1、CLK-3、CLK-5 |
| `clock` | CLK-1 |
| `health` | APP-10、APP-11 |
| `internal/operation`、`internal/stopcontract` | APP-6、APP-7 |
| `cmd/glsvet` | APP-6、CLK-1 |
| `metrics` | OPS-2 |
| `servicemetrics` | OPS-1 |
| `timer` | CLK-6 |
| `versionstore` | OPS-5 |
| `etcd/driver` | APP-2 |
| `robot/loadtest`、`robot/runner` | OPS-2、OPS-7 |
| `ai`、`actionflow` | CLK-1 |
| `kit/redis` | APP-1、APP-3、APP-5、APP-7 |
| `kit/mongo`、`kit/nats` | APP-7 |
| `kit/nest` | APP-4、APP-6 |
| `kit/ops` | APP-10、APP-11、OPS-3、OPS-4 |
| `kit/mods` | APP-1、APP-3、APP-13、OPS-1 |
| `kit/remoteentity` | APP-7、APP-13 |
| `kit/etcd` | APP-3 |
| `kit/service/global` | APP-3、OWN-6 |
| `kit/service/global/activity` | OWN-5、CLK-1、CLK-4 |
| `kit/service/{mail,rank,session}`、`service/{mail,session}` | CLK-1、CLK-4、OPS-1 |
| `kit/service/{match,chat,account}`、`service/match` | CLK-2、OPS-1、OPS-5、OWN-2 |
| `codegen/internal/roost` | APP-2、APP-3、OWN-2～5、CLK-2、OPS-1、OPS-6 |
| `demo/`（game-demo 模板） | OWN-1～5、CLK-1、CLK-6、OPS-6 |
| `scripts`、`codegen/scripts`、`kit/scripts/integration` | TOOL-1、TOOL-2、TOOL-6、TOOL-7 |
| 根包 `*_test.go` | TOOL-2～5、TOOL-7 |

## 5. 未验证 / 外部验证项

| 项 | 条目 | 外部验证编号 |
| --- | --- | --- |
| 单实例锁两客户端在真实 Redis Cluster 上的 integration；Cluster 下真实进程演练；多机切主 | APP-1、APP-5 | E08、E10 |
| 多主机强杀 / 双实例 | APP-1、APP-4、OWN-2 | E13 |
| `c493a791` 与 obs34 之后的代码没有重跑真实进程演练 | OWN-2、OWN-3 | — |
| kubeconform / `docker compose config`、真实 systemd / k8s 部署（启动等待、停机预算） | APP-2、APP-8 | E21、E22 |
| 真实 game-demo 进程里 Init 中途失败的收尾 | APP-9 | — |
| 真实进程里业务 hook 卡住时的 SIGTERM → 宽限期时序 | APP-8 | — |
| 真实 shell / systemd 部署下 ops 端口冲突的完整进程链 | OPS-3 | E21 |
| 仓库外生产配置的 doctor（`time:logic_offset`） | CLK-2 | E24 |
| 高水位只在单机 Redis 上验证 | CLK-3 | E08 |
| 活动组没有真实依赖进程演练；协调器不核对 expected 属于组 | OWN-5 | — |
| 本版最终 HEAD 上的故障矩阵（预跑在 `d6a677e0`） | TOOL-7 | — |
| Linux 内核网络、跨主机分区、长时间容量 | TOOL-6 | E01、E02、E15 |
| 仓外调用方（global 租约 API、capability） | APP-3 | — |

## 6. 与其他分册的交叉（避免重复或遗漏）

- **DRV**：RR-20261006-10 的驱动层 Close 口径（redis / mongo / etcd / nats driver）由 DRV 写；本分册 APP-7 只写 `operation.Serial` 与 kit Mod 一侧。
- **REM**：O-M6-6 同 sid 接管 Remote 实体锁的实现由 REM 写；本分册 APP-13 只写 `app.ModSingletonIncarnation` 能力。
- **NONCORE**：NC-170～174（停机三步）、NC-200～208（scripts）、NC-165（`log.Close`）、NC-190（`singleton.enabled` 严格布尔）、NC-230～234 在 NONCORE 主题里可能也会列出；本分册分别在 APP-6、TOOL-1 / TOOL-7、APP-12、APP-1、APP-7 / APP-8 / OPS-3 里写了与本主题相关的部分。
- **SAGA**：OWN-3 的退款预算由 `saga.steps.gift_item.debit.max_attempts` 配置提供（U-0280）；RR-20261006-06（步骤预算大小写）属 SAGA。
- **CFG**：A4 严格配置读取登记了 `ops.admin_timeout`、`service_metrics.enabled`、`time.logic_offset` 等键，属 CFG。
- **未归入任何分册的提示**：C9（`TestNetworkCodegenTestsRunInSomeWorkflow`，codegen 联网用例门）与 B9（activity 窗口条目统一入口、account 建角判定表，`bd6df5e5`）不在本分册范围，汇总时确认其他分册是否覆盖。

## 7. 本分册发现的文档与源码不一致（以源码为准）

1. **赠礼退款预算的位置**：v1.20.0 CHANGELOG 与 App 锁方案 §13 obs34 写“生成的 `saga/gift_item/definition.go` 里 debit `MaxAttempts` 5 → 15”；v1.20.1（`054fdd66`）起预算改由配置提供，当前源码 `codegen/internal/roost/demo.go:209` `demoGiftRefundBudget` 写的是 game 服务三份配置里的 `saga.steps.gift_item.debit.max_attempts: 15`。
2. **`Live` 的 sid 上限**：App 锁方案 §3.6 写“最多 `MaxPageSize` 个 sid（与 `LiveGames` 相同）”；`MaxPageSize` 已随 global 租约删除，源码是 `app.SingletonLiveMaxSIDs = 200`（`app/singleton.go:93`）。
3. **D-L3 方案 §3.2 的豁免描述过时**：表里把 `playerowner.go.tmpl` 的系统钟豁免写成“玩家归属租约”；静态绑定之后那里没有按玩家的租约，`playerowner.go.tmpl:119` / `:125` 的豁免用途是驻留与闲置卸载。
4. **源码注释错位（不是文档冲突）**：`app/app.go:550-552` 是 `stopModsReverse` 的文档注释，却放在 `startupCleanupTimeout` 常量（`:553-554`）上方，函数本身（`:594`）没有文档注释；行为无关。
5. **roost-coding 与 kit Redis Mod 的串行方式**：roost-coding 写“停止入口的并发串行用 `operation.Serial`（不用 `sync.Mutex`）”，`kit/redis/redis_mod.go:23` 用的是 `sync.Mutex`；RR-20261006-10 修复记录给了理由（go-redis Close 不等在途命令、持锁很短），属于规范的隐含例外，规范里没写。
