# 汇总用摘要 · APP / OWN / CLK / OPS / TOOL 分册

给写 `v1.23.0-GUIDE.md` / `v1.23.0-IMPLEMENTATION.md` 总目录与总表的汇总者用。分册正文：[说明](guide-app-own-clk-ops-tool.md) · [实现](impl-app-own-clk-ops-tool.md)。源码基准：最终代码冻结提交 `5e72ca4d`（2026-10-07；初稿按 `02c8a10d` 写，2026-10-06 按 `e6828e4f` 重核，2026-10-07 按 `5e72ca4d` 再重核，记录见[实现分册开头](impl-app-own-clk-ops-tool.md#recheck)）。条目锚点在两份分册里都是小写编号（`#app-1`、`#own-3`、`#tool-7` …）。

## 1. 条目总表（45 条）

| 编号 | 一句话 | 首发版本 | 行为变化 | 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- | --- |
| APP-1 | App 单实例锁：任何 Mod Init 之前取 Redis 单键锁，失锁 fail-stop | v1.20.0 | 是 | 否 | 是（手工装配） |
| APP-2 | 生成器装配单实例锁、停机预算 +3s、启动等待；etcd `service_prefix` 与租约过期注销 | v1.20.0 | 是 | 否 | 是（已生成工程可选） |
| APP-3 | 删 global 租约 API，停发 `redis.lock` / `etcd.election` capability | v1.20.0 | 是 | 是 | 是 |
| APP-4 | `RuntimeFailure.OnFail` 统一 fail-stop：先围栏 Nest；停机期 fail-stop 非零退出 | v1.20.0 / v1.21.0 | 是 | 否 | 否 |
| APP-5 | `SingletonLiveness.Live`；停机中仍算活（C5） | v1.20.0 / v1.21.0 | 否 | 否 | 否 |
| APP-6 | `operation.Lifetime`、停机契约骨架、glsvet stophints（A3） | v1.20.2 | 极小 | 否 | 否 |
| APP-7 | `operation.Serial`；kit Mod 停止收敛（NC-233 / 234） | v1.23.0；NC-233 / 234 v1.21.0 | 是 | 小 | 否 |
| APP-8 | 停机 lifecycle hook 受 `shutdown.total_timeout` 约束（NC-231）；超时错误点名卡住的 hook（RR-20261006-25） | v1.21.0（点名 v1.23.0） | 是 | 否（错误文本变化） | 否 |
| APP-9 | 启动失败先调 `Service.Shutdown` 收回（NC-193） | v1.20.2 | 是 | 是（契约收紧） | 是 |
| APP-10 | `/readyz` Degraded 算就绪（D1） | v1.21.0 | 是 | 是（`Snapshot.OK` 含义） | 视情况 |
| APP-11 | `/readyz` 每个 checker 1.5s 期限 | v1.23.0 | 是 | 否 | 视情况 |
| APP-12 | 退出原因写进文件日志（RR-20261006-07） | v1.23.0 | 是 | 否 | 否 |
| APP-13 | App 能力 `singleton` / `clock.business` / `singleton_incarnation` 与 Mod 依赖 | v1.20.0 / v1.21.0 / v1.23.0 | 是 | 否 | 是 |
| APP-14 | 生成工程整体切 Redis Cluster：生产检查认 `cluster_addrs`、accountctl `-redis-cluster`、dev `run.sh`（RR-20261006-28） | v1.23.0 | 是（放宽） | 否 | 视情况 |
| APP-15 | 真实进程演练（单机 + 同机 Cluster）全部实测，途中 RR-20261006-24～29 | v1.23.0 | 否（验证记录） | 否 | 否 |
| OWN-1 | 租约状态机期三处修复（RR-20261004-10 / 11 / 14），同版本被取代 | v1.20.0 | 否 | 否 | 否 |
| OWN-2 | game-demo 玩家所有权静态绑定 | v1.20.0 | 是 | 是（模板） | 是 |
| OWN-3 | 赠礼按 `FromSID` 准入与转交；退款预算 | v1.20.0（预算入配置 v1.20.1） | 是 | 是（模板） | 是 |
| OWN-4 | activity 改用 `Live`；候选校验（RR-20261005-01，C4 后由组文件兑现，回归去向见 NONCORE-20） | v1.20.0 / v1.20.1 | 是 | 否 | 是 |
| OWN-5 | 活动组文件，每组 ≤ 64（C4）；守卫组上限 ≤ 一次 `Live` 上限；协调器按组核对 expected（RR-20261006-17）、`groups_file` 必填、`activity.New` 要组 | v1.20.2（守卫、核对、必填 v1.23.0） | 是 | 是 | 是 |
| OWN-6 | global `Bind` 重试幂等（RR-20261006-05） | v1.23.0 | 是（放宽） | 否 | 否 |
| OWN-7 | 通关遇到还没开的活动窗口时自己开窗（RR-20261006-29） | v1.23.0 | 是 | 否 | 否（模板） |
| CLK-1 | 业务时钟 / 系统时钟分开，生产偏移为 0（D-L3） | v1.21.0 | 是 | 是（生产偏移） | 是 |
| CLK-2 | match / chat 展示 / account 换钟，doctor 偏移一致 | v1.21.0 | 是 | 否 | 是 |
| CLK-3 | 业务时间只许前进（高水位） | v1.22.0 | 是 | 是（测试环境） | 是 |
| CLK-4 | activity / mail 回业务钟，删 `SystemNow` / `StorageGrace` | v1.22.0（拆分 v1.21.0） | 偏移 0 时无 | 是 | 是（手工装配） |
| CLK-5 | `app.business_time.advance_failed.total` | v1.23.0 | 新指标 | 否 | 否 |
| CLK-6 | timer 同期限按 priority / 登记顺序；未注册类型告警计数（D-L1 / D-L2） | v1.21.0 | 是 | 否 | 否 |
| OPS-1 | 服务指标默认进 metrics 注册表（C6） | v1.20.2 | 是 | 是（指标名） | 是 |
| OPS-2 | `metrics.DeleteSeries`；loadtest 运行、nest 派发器的序列随拥有者删除（RR-20261006-18）；RPC `method` 标签有界（RR-20261006-19） | v1.23.0 | 是 | 否 | 视情况（按 `method` 的看板） |
| OPS-3 | Ops 同步 bind（NC-230）；`ops.admin_timeout` | v1.21.0 | 是 | 是 | 是 |
| OPS-4 | Ops `Authorization` 只认 `Bearer` | v1.23.0 | 是 | 是 | 是 |
| OPS-5 | CAS 冲突在 versionstore 统一计数 | v1.23.0 | 是 | 是（指标口径） | 是 |
| OPS-6 | game-demo 仪表盘补两个面板 | v1.23.0 | 否 | 否 | 否 |
| OPS-7 | robot Stage 序号只增不回收（RR-20261006-09） | v1.23.0 | 是 | 否 | 视情况 |
| OPS-8 | 直方图分位数收在观测范围；loadtest 阈值失败写明原因（RR-20261006-27） | v1.23.0 | 是 | 否 | 视情况 |
| TOOL-1 | pretag：失败打印；origin 不可达失败（NC-205） | v1.20.0 / v1.20.2 | 是 | 否 | 否 |
| TOOL-2 | `full-scenario-adds.sh` 一处定义，`source-head-check.sh` 不吞失败 | v1.23.0 | 是 | 否 | 否 |
| TOOL-3 | 根包冲突标记门禁 | v1.21.0 | 新门禁 | 否 | 否 |
| TOOL-4 | 根包示例实跑门禁 | v1.23.0 | 新门禁 | 否 | 否 |
| TOOL-5 | 根包文档相对链接门禁 | v1.23.0 | 新门禁 | 否 | 否 |
| TOOL-6 | `scripts/mirror-local.sh`（扩展 `test-core` 等） | v1.22.0（扩展 v1.23.0） | 新工具 | 否 | 否 |
| TOOL-7 | 故障矩阵无用例记 FAIL、全局命令持锁；本版预跑 21 格 PASS | v1.20.2（预跑 v1.23.0） | 是 | 否 | 否 |
| TOOL-8 | 平台支持：Windows 不保证正确，Windows 问题暂存 | v1.23.0 | 否（文档） | 否 | 视情况（不在 Windows 部署生产） |
| TOOL-9 | 交给 review 之前不留 WANTED（roost-bugfix §7） | v1.23.0 | 否（规范） | 否 | 否 |

按首发版本计：v1.20.0 有 APP-1～5、APP-13、OWN-1～4、TOOL-1（各含后续补修）；v1.20.1 无新条目（OWN-3 / OWN-4 的补充）；v1.20.2 有 APP-6、APP-9、OWN-5、OPS-1、TOOL-7；v1.21.0 有 APP-8、APP-10、CLK-1、CLK-2、CLK-6、OPS-3、TOOL-3；v1.22.0 有 CLK-3、CLK-4、TOOL-6；v1.23.0（本版）有 APP-7、APP-11、APP-12、APP-14、APP-15、OWN-6、OWN-7、CLK-5、OPS-2、OPS-4～8、TOOL-2、TOOL-4、TOOL-5、TOOL-8、TOOL-9（另有 APP-8 的点名、APP-13 的 `singleton_incarnation`、OWN-4 / OWN-5 的回归去向、守卫、协调器核对与必填、TOOL-6 扩展、TOOL-7 预跑）。

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
| OWN-5（v1.23.0） | 协调器缺 `activity.groups_file` 拒绝启动；`activity.New` 缺 `Config.Groups` 拒绝构造；组文件不一致的开窗返回 `ErrInvalid` | 协调器配置补键与 `configs/activity_groups.yaml`（T-260）；手工装配传 `Groups`；两边组文件一起改；新生成工程需要 core ≥ v1.23.0（打 tag 时生成器 Core 下限随之提高） |
| APP-7 | 单实例锁 store / kit Mod 重复 Close 返回 nil（原来报错或粘滞返回第一次的错误） | 依赖第二次 Close 报错的调用方改看命令错误 |

### 2.2 其他行为变化（不破坏，但可见）

APP-1（启用后崩溃重启最多等约 15s；Redis 连续不可用约 10s 进程 fail-stop）、APP-2（新工程 dataengine 服务默认启用单实例锁，`total_timeout` +3s）、APP-4（Remote Entity fatal 也围栏 Nest；停机期 fail-stop 非零退出）、APP-8（hook 超时 `run` 按预算返回、不停 Mod；错误文本 `hooks did not return` → `hook "<名字>" did not return`）、APP-11（>1.5s 的 checker 记 Fail）、APP-12（多一行 `app run failed`）、APP-14（生产配置只写 `cluster_addrs` 也能启动）、OWN-4（activity 需 singleton，崩溃进程最多算 15s）、OWN-6（同参数重试 `Bind` 成功）、OWN-7（窗口开头的通关计入当前窗口）、CLK-2（chat 新字段 `SentAtUnix`）、CLK-6（同期限顺序确定）、OPS-2（挤出历史的 loadtest 运行序列、已销毁派发器的序列从 `/metrics` 消失；未注册方法记 `_unregistered`、超过 256 个方法记 `_other`）、OPS-7（序号可能超过 `Count`）、OPS-8（分位数估计变小、更接近真实值；阈值结果多 `samples`、失败运行 `error` 非空）、TOOL-1（origin 不可达 pretag 失败）、TOOL-2（本地 full 场景检查不再吞失败、步骤与 CI 一致）、TOOL-8（文档口径：Windows 不保证正确）。

### 2.3 需业务或运维改动的清单

见说明分册“[需要业务或运维改动的清单](guide-app-own-clk-ops-tool.md#需要业务或运维改动的清单)”，共 26 项（第 21 项：不在 Windows 部署生产，TOOL-8；第 26 项是打 v1.23.0 tag 时的发版步骤：生成器 Core 下限 `codegen/internal/roost/manifest.go:83` 与 `.github/workflows/framework-compat.yml:50` 提到 v1.23.0）；最需要在总表突出的五项：APP-3 仓外调用方迁移与删旧键、OPS-4 运维脚本改 Bearer、OPS-1 / OPS-5 看板改指标、CLK-3 测试环境不能再回拨偏移、OWN-5 协调器配置必须有 `activity.groups_file`。

## 3. 新增的全局门禁 / 守卫测试

| 守卫 | 位置 | 首发 | 条目 |
| --- | --- | --- | --- |
| `TestNoMergeConflictMarkersInTrackedFiles` | 根包 `conflict_marker_gate_test.go` | v1.21.0 | TOOL-3 |
| `TestExamplesRun`（`exampleRuns` 登记表） | 根包 `examples_run_test.go` | v1.23.0 | TOOL-4 |
| `TestTrackedMarkdownRelativeLinksResolve` | 根包 `doc_links_gate_test.go` | v1.23.0 | TOOL-5 |
| `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed` | 根包 `ci_full_scenario_test.go` | v1.23.0 | TOOL-2 |
| `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock` | 根包 `acceptance_lock_promises_test.go` | v1.20.2 | TOOL-7 |
| `TestAGroupFitsOneLiveQuery` | `kit/service/global/activity/groups_live_limit_promises_test.go` | v1.23.0 | OWN-4、OWN-5 |
| `internal/stopcontract.Check` 停机契约骨架 | 各包 `stop_contract_test.go` | v1.20.2 | APP-6 |
| `glsvet -stophints`（只提示） | `cmd/glsvet/stophints.go` | v1.20.2 | APP-6 |
| `glsvet -clockhints` / `-businessdirs`（只提示） | `cmd/glsvet/clockhints.go` | v1.21.0 | CLK-1 |
| `roost project doctor` 的 `time:logic_offset` | `codegen/internal/roost/logic_offset_doctor.go` | v1.21.0 | CLK-2 |
| 单实例锁时间关系（启动时的配置声明检查，A4① 起） | `app/singleton.go` `singletonConfig.ValidateConfig` | v1.20.0 | APP-1 |
| 生产偏移为 0（启动时的配置声明检查，A4① 起） | `app/config_schema.go` `appConfig.ValidateConfig` | v1.21.0 | CLK-1 |
| 业务时间高水位（启动拒绝） | `app/business_time.go` | v1.22.0 | CLK-3 |
| 协调器按组核对、`groups_file` 必填、`New` 要组 | `kit/service/global/activity/open_expected_group_promises_test.go` | v1.23.0 | OWN-5 |
| 派发器序列随最后一个同名派发器删除 | `nest/dispatcher_series_lifecycle_promises_test.go` | v1.23.0 | OPS-2 |
| RPC `method` 标签有界 | `bus/rpc_method_label_bound_promises_test.go` | v1.23.0 | OPS-2 |
| 分位数在观测范围内；阈值失败说明 | `metrics/histogram_quantile_bounds_promises_test.go`、`robot/loadtest/threshold_verdict_promises_test.go` | v1.23.0 | OPS-8 |
| 生产 Redis：`addr` 或 `cluster_addrs` | `kit/redis/config_types_promises_test.go` `TestProductionRedisNeedsAnAddrOrClusterSeeds` | v1.23.0 | APP-14 |
| kit Mongo / NATS 真实 Close（`-tags integration`）；故障矩阵必须跑 `./kit/mongo` | `kit/mongo/close_contract_real_promises_test.go`、`kit/nats/close_contract_real_promises_test.go`；根包 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` | v1.23.0 | APP-7、APP-15 |

roost-coding 规范同期新增的条款（规则源，不是测试）：“新的停机对象优先用共用类型并套契约骨架”与 `operation.Serial` / Close 统一口径，及“临界区很短、关闭不等在途工作的可以用 `sync.Mutex`（kit `RedisMod`）”例外（APP-6 / APP-7，例外 `b7471ae4`）、“业务时钟与系统时钟”（CLK-1 / CLK-3）、“示例要实跑，不能只编译”（TOOL-4）、“反复出问题要上报方向判断”（OWN-1 → OWN-2 的先例）。roost-bugfix §7“交给 review 之前不留 WANTED”（TOOL-9，`87d8d91e`）。README / DEPLOYMENT“Windows 不保证正确”（TOOL-8，`7fec136e`）。

## 4. 按包的条目索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | APP-1、APP-4、APP-5、APP-8、APP-9、APP-12、APP-13、CLK-1、CLK-3、CLK-5 |
| `lifecycle` | APP-8 |
| `clock` | CLK-1 |
| `health` | APP-10、APP-11 |
| `internal/operation`、`internal/stopcontract` | APP-6、APP-7 |
| `cmd/glsvet` | APP-6、CLK-1 |
| `metrics` | OPS-2、OPS-8 |
| `nest`、`bus` | OPS-2 |
| `servicemetrics` | OPS-1 |
| `timer` | CLK-6 |
| `versionstore` | OPS-5 |
| `etcd/driver` | APP-2 |
| `robot/loadtest`、`robot/runner` | OPS-2、OPS-7、OPS-8 |
| `ai`、`actionflow` | CLK-1 |
| `kit/redis` | APP-1、APP-3、APP-5、APP-7、APP-14 |
| `kit/mongo`、`kit/nats` | APP-7、APP-15 |
| `kit/nest` | APP-4、APP-6 |
| `kit/ops` | APP-10、APP-11、OPS-3、OPS-4 |
| `kit/mods` | APP-1、APP-3、APP-13、OPS-1 |
| `kit/remoteentity` | APP-7、APP-13 |
| `kit/etcd` | APP-3 |
| `kit/service/global` | APP-3、OWN-6 |
| `kit/service/global/activity` | OWN-4、OWN-5、CLK-1、CLK-4 |
| `kit/service/{mail,rank,session}`、`service/{mail,session}` | CLK-1、CLK-4、OPS-1 |
| `kit/service/{match,chat,account}`、`service/match` | CLK-2、OPS-1、OPS-5、OWN-2 |
| `codegen/internal/roost` | APP-2、APP-3、APP-14、OWN-2～5、CLK-2、OPS-1、OPS-6 |
| `demo/`（game-demo 模板） | APP-14（`cmd/accountctl`）、OWN-1～5、OWN-7、CLK-1、CLK-6、OPS-6、OPS-8（`cmd/loadtest`） |
| `scripts`、`codegen/scripts`、`kit/scripts/integration` | TOOL-1、TOOL-2、TOOL-6、TOOL-7、APP-15（`dataengine-env.sh` 加 `./kit/mongo`） |
| 根包 `*_test.go` | TOOL-2～5、TOOL-7 |
| 文档与规范（`README.md`、`docs/DEPLOYMENT.md`、`docs/agent-skills/*`、`docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md` 与演练脚本） | APP-6、APP-7、APP-15、TOOL-4、TOOL-8、TOOL-9 |

## 5. 未验证 / 外部验证项

只列外部环境项，编号见 [EXTERNAL-VERIFICATION-2026-10-06](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)；都不阻塞发版。

| 项 | 条目 | 外部验证编号 |
| --- | --- | --- |
| 多机 Redis Cluster 切主下的单实例锁两客户端、Lua 布局、高水位（同机 3 主 3 从的套件与真实进程演练已做） | APP-1、APP-5、APP-7、APP-14、APP-15、CLK-3 | E08 |
| 多机 Cluster 与多进程下的业务服务组合（activity 组核对等） | APP-14、APP-15、OWN-5 | E09 |
| 异步复制丢写与切主：锁 | APP-1 | E10 |
| 多主机强杀 / 双实例 / 跨服赠礼 | APP-1、APP-4、APP-15、OWN-2、OWN-3 | E13 |
| 真实 systemd 部署（启动等待、停机宽限期、ops 端口冲突） | APP-2、APP-8、OPS-3 | E21 |
| k8s 部署物与滚动停机（`startupProbe`、停机预算） | APP-2、APP-8 | E22 |
| 仓库外生产配置的 doctor（`time:logic_offset`） | CLK-1、CLK-2 | E24 |
| Linux 内核网络、跨主机分区、长时间容量 | TOOL-6 | E01、E02、E15 |
| Windows：**暂存，不保证正确** | TOOL-8 | E25、E27（Windows 部分） |

不是外部项、但要说明的：本版最终发版提交上的故障矩阵是发版步骤（TOOL-7，“每版 pretag 重跑”）；`d6a677e0` 上的预跑已闭环 NC-207 修复记录里“整张矩阵没在真实隔离环境上跑”的未验证项。生成器 Core 下限随 v1.23.0 提高是打 tag 时的发版步骤（说明分册清单第 26 项）。仓外调用方（APP-3）无法核对，按破坏性变更登记，不是未验证项。

**仍未闭环 = 0。** 上一版本节列出的 6 项“本机可做、记录里写明没做”全部闭环：

| 上一版的项 | 条目 | 闭环 |
| --- | --- | --- |
| game-demo 真实进程演练没有在 `c493a791` 与 obs34 之后重跑；Redis Cluster 下的演练未验证 | APP-1、OWN-2、OWN-3 | 演练 ① ⑥（`0db819b0`、`3b06c66c`），全部符合，见 APP-15 |
| 真实 game-demo 进程里 Init 中途失败的收尾 | APP-9 | 演练 ②，符合 |
| 真实进程里停机 hook 卡住时 SIGTERM 时序 | APP-8 | 演练 ③，时序符合；发现并修复 RR-20261006-25 |
| kit Mongo / NATS Mod 的 Close 没接真实依赖 | APP-7 | 演练 ④ 的 integration 用例；发现并修复 RR-20261006-24 / -26（DRV-5） |
| 协调器 `OpenActivity` 不核对 expected 属于 Key 的组 | OWN-5 | RR-20261006-17（`055a15d6`），随后 `groups_file` 必填（`2c01e06d`）、`New` 要组（`061cb538`） |
| O3 的 `nest.dispatch.*{dispatcher}`、`bus_rpc_pending{method}` 不接 `DeleteSeries` | OPS-2 | RR-20261006-18（派发器排空后删）、RR-20261006-19（method 标签有界，无注销可挂；`055a15d6`、`2c01e06d`） |

另外演练里一直挂着的观察“两个 sid 时 loadtest `rc=1`”查明是分位数口径并修复（RR-20261006-27，OPS-8），OWN-3 原来的“已知限制”随之删除。

## 6. 与其他分册的交叉（避免重复或遗漏）

- **DRV**：RR-20261006-10 的驱动层 Close 口径（redis / mongo / etcd / nats driver）由 DRV 写；真实进程演练 ④ 发现的 RR-20261006-24 / -26 与随后的 nats/driver 自持关闭状态（`f0de827a`）由 DRV-5 写。本分册 APP-7 只写 `operation.Serial` 与 kit Mod 一侧，APP-15 只引用。A2③ versionstore 写令牌改变了 `RedisStore.Update` 的结算方式（`settle`），OPS-5 的计数点随之移到结算之后（`versionstore/redis_store.go:391`），写令牌本身属 DRV。
- **REM**：O-M6-6 同 sid 接管 Remote 实体锁的实现由 REM 写；本分册 APP-13 只写 `app.ModSingletonIncarnation` 能力。A3②（`ebf679e1`，`ISyncBus` 带 ctx 的排空退订，原“下个大版本”，本版完成）由 REM 写；本分册 APP-6 只引用（mirror 的手写门由传输层的 `syncbus.Subscription` 取代）。
- **CFG**：A4①（`d1226825`、`6e0619bb`）把 `app/config_validation.go` 的严格读取清单与生产规则交给各 Mod 的配置声明：`singleton.*`、`time.logic_offset`（生产为 0）、`ops.admin_timeout`、`service_metrics.enabled`、`activity.groups_file`（必填）、`redis.addr` / `cluster_addrs`（生产二选一）都由声明检查。本分册 APP-1、CLK-1、OPS-1、OPS-3、OWN-5、APP-14 按新位置引用，声明机制本身属 CFG。
- **NONCORE**：NC-170～174（停机三步）、NC-200～208（scripts）、NC-165（`log.Close`）、NC-190（`singleton.enabled` 严格布尔）、NC-230～234 在 NONCORE 主题里可能也会列出；本分册分别在 APP-6、TOOL-1 / TOOL-7、APP-12、APP-1、APP-7 / APP-8 / OPS-3 里写了与本主题相关的部分。演练记录把 ⑤ 关联到 NONCORE-43、⑥ 关联到 CFG-5，那两条由分册 3 写，本分册的 OPS-8、APP-14 是同一修复的主条目。
- **SAGA**：OWN-3 的退款预算由 `saga.steps.gift_item.debit.max_attempts` 配置提供（U-0280）；RR-20261006-06（步骤预算大小写）属 SAGA。
- **SKILL**：本分册不涉及 skill；术语按冻结口径（召唤物 = Summon，衍生物 = Spawn）由分册 3 统一。本分册里的 “spawner” 是 game-demo 的怪物刷新器，与 skill 衍生物无关。
- **C9 / B9 的归属**：C9（`TestNetworkCodegenTestsRunInSomeWorkflow`，codegen 联网用例门）由 NONCORE-31 覆盖，B9（activity 窗口条目统一入口、account 建角判定表，`bd6df5e5`）由 NONCORE-21 覆盖；本分册 APP-5 只引用 B9 / C5 方案里的 C5 部分。
- **与 NONCORE 去重**（以本分册为准）：NONCORE-1 ↔ APP-4 / APP-7 / APP-8 / OPS-3，NONCORE-24 ↔ OWN-6，NONCORE-40 ↔ CLK-6，NONCORE-45 ↔ OPS-7，NONCORE-50 ↔ APP-9，NONCORE-56 ↔ OWN-1。反方向：RR-20261005-01 的回归去向（C4 后的逐项对照）以 [NONCORE-20](guide-cfg-skill-noncore.md#noncore-20) 为准，OWN-4 / OWN-5 只留一句话与组上限守卫。
- **WANTED**：未决数 0。本分册相关的 W-2026-10-06-02 → RR-20261006-10（APP-7）；W-2026-10-06-01 → RR-20261006-12（`b7471ae4`，NONCORE）。

## 7. 本分册发现的文档与源码不一致（以源码为准）

1. **赠礼退款预算的位置**：v1.20.0 CHANGELOG 与 App 锁方案 §13 obs34 写“生成的 `saga/gift_item/definition.go` 里 debit `MaxAttempts` 5 → 15”；v1.20.1（`054fdd66`）起改由配置提供，源码 `codegen/internal/roost/demo.go:217` `demoGiftRefundBudget` 写 game 服务三份配置里的 `saga.steps.gift_item.debit.max_attempts: 15`。**已处理**：方案 §13 obs34 第 3 条加更正注；CHANGELOG 历史段不改。
2. **`Live` 的 sid 上限**：App 锁方案 §3.6 写“最多 `MaxPageSize` 个 sid（与 `LiveGames` 相同）”，源码是 `app.SingletonLiveMaxSIDs = 200`（`app/singleton.go:93`）。**已处理**：§3.6 改为 `SingletonLiveMaxSIDs` 并加更正。
3. **D-L3 方案 §3.2 的豁免描述**：表里写 `playerowner.go.tmpl`（玩家归属租约）；静态绑定后用途是驻留与闲置卸载（`playerowner.go.tmpl:124` / `:130`）。**已处理**：§3.2 改为“驻留与闲置卸载”并注明更正。
4. **源码注释错位**：`stopModsReverse` 的文档注释错放在 `startupCleanupTimeout` 常量上方。**已处理**：`b7471ae4` 挪回函数上方（现在 `app/app.go:635-637`，函数 `:638`；常量在 `:594-595`）。
5. **roost-coding 与 kit Redis Mod 的串行方式**：规范写“用 `operation.Serial`（不用 `sync.Mutex`）”，`kit/redis/redis_mod.go:24` 用 `sync.Mutex`。**已处理**：`b7471ae4` 在规范里写明例外。
6. **RR-20261006-28 的位置（本次新发现）**：修复记录与 `docs/bugfix/README.md` 写生产校验在 `app/config_validation.go`、解析收到 `app.RedisClusterAddrs`、`kit/mods.RedisClusterAddrs` 转调、守卫在 `app/production_redis_cluster_promises_test.go`；A4①（`d1226825`）之后检查在 `kit/redis/redis_mod.go:63-68`，两个 `RedisClusterAddrs` 与该测试文件都已删除，承诺由 `TestProductionRedisNeedsAnAddrOrClusterSeeds` 承担，错误文本不再带服务类型。**已处理**：APP-14 两份文档写明“源码与记录不一致”；RR-28 / RR-17 修复记录已由 `a3aadb9d` 追加更正（[RR-20261006-28](../../bugfix/RR-20261006-28.md) 末节“更正”）。
7. **RR-20261006-17 “后续”的错误文本（本次新发现）**：修复记录写 `activity mod: activity.groups_file is required: set it to …`；A4① 之后由声明的 `required` 报出 `config: activity.groups_file is required (for example configs/activity_groups.yaml)`（仍点名键名与默认路径，守卫断言的子串不变）。**已处理**：OWN-5 实现文档写明；RR-28 / RR-17 修复记录已由 `a3aadb9d` 追加更正（[RR-20261006-17](../../bugfix/RR-20261006-17.md) 末节“更正”）。

## 8. 重核结果（`e6828e4f` → `5e72ca4d`，2026-10-07）

- 引用：三份文件按冻结提交逐条重核（脚本按 `git diff -U0 e6828e4f 5e72ca4d` 的 hunk 把每个旧行号映射到新行号，落在改动 hunk 里的与上下文归属不明的逐条人工读源码；最后在 `5e72ca4d` 上用“测试名必须落在 `func TestX`、符号必须在引用行附近”再验一遍）。数目见[实现分册“重核记录”](impl-app-own-clk-ops-tool.md#recheck)。测试名全部存在（除写明“已删除 / 改名”的历史用例与前缀写法）。
- 新增条目：APP-14（RR-28）、APP-15（真实进程演练）、OWN-7（RR-29）、OPS-8（RR-27）。并入已有条目：APP-8（RR-25）、OWN-5（RR-17 + `groups_file` 必填 + `New` 要组）、OPS-2（RR-18 / RR-19）；APP-1 / APP-5 / APP-7 / APP-9 / APP-12、OWN-2 / OWN-3、TOOL-7 / TOOL-9 补演练结论；APP-6 改 A3② 已完成。
- 过时说法已改：OWN-5、OPS-2 的“未做 / 已知限制”；APP-1、APP-7、APP-8、APP-9 与 OWN-2 / OWN-3 的“未在真实进程演练 / 本机未做”；OWN-3 的 loadtest p95 已知限制；“留到下个大版本”（A3②）。
- review 检查点：只写给 review 去查的问题；未验证项只剩外部环境类（E 编号）；仍未闭环 = 0。
