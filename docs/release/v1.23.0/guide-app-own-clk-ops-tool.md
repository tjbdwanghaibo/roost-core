# v1.23.0 说明 · APP / OWN / CLK / OPS / TOOL 部分

> 范围：`git log v1.19.2..02c8a10d` 里属于 App 生命周期（APP）、玩家所有权与活动组（OWN）、业务时钟（CLK）、运维可观测（OPS）、发版工具与门禁（TOOL）的全部改动。
> 读者：维护者（先读每条的“一句话”和“兼容与迁移”）与 review agent（每条末尾链到[实现文档](impl-app-own-clk-ops-tool.md)的同编号条目）。
> 源码基准：提交 `02c8a10d`（本版发版前 main）。历史记录与源码不一致处以源码为准，并在条目里注明。
> 其他主题（SAGA / DRV / DAO / REM / CFG / SKILL / NONCORE）由另外两份分册说明；本分册提到它们时只写主题名，不加链接。

## 目录

- [条目总表](#条目总表)
- [本部分的版本时间线](#本部分的版本时间线)
- [需要业务或运维改动的清单](#需要业务或运维改动的清单)
- [APP：App 生命周期](#app)
- [OWN：玩家所有权与活动组](#own)
- [CLK：业务时钟](#clk)
- [OPS：运维与可观测](#ops)
- [TOOL：发版工具与门禁](#tool)
- [外部验证清单（本部分相关）](#外部验证清单本部分相关)
- [仍待决定的事项与 WANTED](#仍待决定的事项与-wanted)

## 条目总表

“行为变化”指升级后同样的输入会得到不同结果；“兼容破坏”指以前能编译 / 启动 / 调通的东西现在不行；“需业务改动”指业务代码、配置、部署脚本或看板要跟着改（只影响手工装配或可选能力的也算“是”，并在条目里写明范围）。

| 编号 | 一句话 | 首发版本 | 行为变化 | 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- | --- |
| [APP-1](#app-1) | 同一服务类型 + sid 只跑一个进程：App 在任何 Mod Init 之前取 Redis 单键锁，失锁 fail-stop | v1.20.0 | 是 | 否 | 是（手工装配要装 opener 并写 `singleton` 段） |
| [APP-2](#app-2) | 生成器默认给带 dataengine 的服务装上单实例锁，停机预算 +3s、部署启动等待加长；演练修了 etcd 两处 | v1.20.0 | 是 | 否 | 是（已生成工程可选手工补；`etcd.service_prefix` 补 `/`） |
| [APP-3](#app-3) | “按进程 / sid 唯一”统一归 App：删 global 租约 API，停发 `redis.lock` / `etcd.election` capability | v1.20.0 | 是 | **是** | 是（仓外调用方迁移；运维删旧 `:lease:*` 键） |
| [APP-4](#app-4) | `RuntimeFailure.OnFail`：任何 fail-stop 先围栏 Nest；启动阶段与停机期间的 fail-stop 都如实报告、非零退出 | v1.20.0（停机期非零退出 v1.21.0） | 是 | 否 | 否 |
| [APP-5](#app-5) | 只读活性查询 `SingletonLiveness.Live`；停机中的进程到 Release 为止仍算活（C5） | v1.20.0（C5 契约 v1.21.0） | 否 | 否 | 否 |
| [APP-6](#app-6) | 停机共用 `operation.Lifetime` 与契约测试骨架，glsvet 对停止入口给提示（A3） | v1.20.2 | 极小 | 否 | 否 |
| [APP-7](#app-7) | 停止入口串行器 `operation.Serial`；kit Mod 停止收敛（重复 Stop / Close 返回 nil） | v1.23.0（本版）；NC-233 / 234 v1.21.0 | 是 | 小 | 否 |
| [APP-8](#app-8) | 停机阶段的 lifecycle hook 在 `shutdown.total_timeout` 内等，超时按“停机不完整”保留依赖 | v1.21.0 | 是 | 否 | 否（自写 hook 应配合 ctx） |
| [APP-9](#app-9) | 启动失败先调 `Service.Shutdown` 收回已启动部分，收不回就保留 Mod 与锁 | v1.20.2 | 是 | **是**（契约收紧） | 是（自写 `Shutdown` 要容忍部分初始化） |
| [APP-10](#app-10) | `/readyz`：Degraded 算就绪，只有 Fail 返回 503（D1） | v1.21.0 | 是 | **是**（`health.Snapshot.OK` 含义变） | 视情况（依赖 readiness 卸载流量的部署） |
| [APP-11](#app-11) | `/readyz` 每个 checker 最多等 1.5s，卡住的记 Fail；同一 checker 不叠加调用 | v1.23.0（本版） | 是 | 否 | 视情况（慢 checker） |
| [APP-12](#app-12) | `run` 出错时在关闭文件日志之前写 `app run failed`，启动阶段的退出原因进日志文件 | v1.23.0（本版） | 是（多一行日志） | 否 | 否 |
| [APP-13](#app-13) | App 向 Mod 提供 `singleton` / `singleton_incarnation` / `clock.business` 三个能力，依赖方取不到即启动失败 | v1.20.0 / v1.21.0 / v1.23.0（本版） | 是 | 否 | 是（用 activity 的 game 服务须开 singleton） |
| [OWN-1](#own-1) | 玩家租约状态机期的三处修复（RR-20261004-10 / 11 / 14），同版本即被 OWN-2 取代 | v1.20.0 | 否（代码已删） | 否 | 否 |
| [OWN-2](#own-2) | game-demo 玩家所有权改为静态绑定：本地驻留表 + 闲置卸载，登录按 `server_id` 判定 | v1.20.0 | 是 | **是**（模板层） | 是（客户端处理 `player_elsewhere`；已生成工程不迁移） |
| [OWN-3](#own-3) | 赠礼 debit / refund 按发送方绑定的 sid 准入与转交，转交核对 phase / topic；退款预算覆盖一次崩溃重启 | v1.20.0（预算改由配置 v1.20.1） | 是 | **是**（`start_gift` 多参数，旧载荷不执行） | 是（已生成工程手工合并） |
| [OWN-4](#own-4) | activity 不再持有 global 租约，expected 集合改用 App 的 `Live`；启动时拒绝开不出窗口的候选 | v1.20.0（候选校验 v1.20.1） | 是 | 否 | 是（game 服务须开 singleton） |
| [OWN-5](#own-5) | 活动组由 `configs/activity_groups.yaml` 定义，每组 ≤ 64 个 game，违例启动即报错（C4） | v1.20.2 | 是 | **是**（game 键 `activity.game_sids` 删除） | 是（新工程维护组文件） |
| [OWN-6](#own-6) | global `Bind` 结果未知后同参数重试按幂等成功 | v1.23.0（本版） | 是（放宽） | 否 | 否 |
| [CLK-1](#clk-1) | 业务时钟（真实时间 + `time.logic_offset`）与系统时钟分开，生产偏移必须为 0（D-L3） | v1.21.0 | 是 | 是（生产配了偏移拒绝启动） | 是（业务代码读 `app.BusinessClock`） |
| [CLK-2](#clk-2) | match / chat 展示 / account 业务时间走业务钟，`roost project doctor` 检查偏移一致（D-L3 第八轮） | v1.21.0 | 是 | 否 | 是（客户端展示改读 `SentAtUnix`；配置偏移一致） |
| [CLK-3](#clk-3) | 业务时间只许前进：App 在协调存储里记高水位，非生产偏移回调超过 1 分钟拒绝启动 | v1.22.0 | 是 | **是**（测试环境） | 是（测试环境回到过去只能清库重建） |
| [CLK-4](#clk-4) | activity 派发退避 / 进度凭证、mail 领取租约回到业务钟，删掉只为“偏移回调”存在的拆分 | v1.22.0（拆分在 v1.21.0 引入） | 偏移为 0 时无 | **是**（删 3 个字段、改 1 个名字） | 是（手工装配删对应行） |
| [CLK-5](#clk-5) | 高水位推进失败计 `app.business_time.advance_failed.total` | v1.23.0（本版） | 新指标 | 否 | 否 |
| [CLK-6](#clk-6) | `timer` 同期限按 priority、再按登记顺序触发；未注册类型的节点删除时告警计数（D-L1 / D-L2） | v1.21.0 | 是 | 否 | 否（模板需 `project sync` 才得到新行为） |
| [OPS-1](#ops-1) | 服务指标默认写进 metrics 注册表，六个固定名，名字里不再带 ID（C6） | v1.20.2 | 是 | **是**（指标名与 `Recorder` 事件名变） | 是（看板 / 告警） |
| [OPS-2](#ops-2) | `metrics.DeleteSeries` 按标签删序列；loadtest 运行挤出历史时删它的 `run` 序列 | v1.23.0（本版） | 是 | 否 | 否 |
| [OPS-3](#ops-3) | Ops 端口在 Start 里同步 bind，占用即启动失败；`/admin/execute` 受 `ops.admin_timeout`（10s）约束、到期 504 | v1.21.0 | 是 | **是** | 是（同机多实例各配 `ops.addr`；长命令调大期限） |
| [OPS-4](#ops-4) | Ops 的 `Authorization` 只认 `Bearer <token>` | v1.23.0（本版） | 是 | **是** | 是（运维脚本） |
| [OPS-5](#ops-5) | CAS 冲突在 versionstore 统一计数，chat / rank 不再自报 `conflict:*` | v1.23.0（本版） | 是 | **是**（指标口径） | 是（看板 / 告警） |
| [OPS-6](#ops-6) | game-demo 仪表盘补“配置撤回”“场景复制会话”两个面板 | v1.23.0（本版） | 否 | 否 | 否（已生成工程不迁移） |
| [OPS-7](#ops-7) | robot Stage 先缩后扩不再复用刚停掉的机器人的序号与 PlayerID | v1.23.0（本版） | 是 | 否 | 视情况（自定义 `IdentityProvider`） |
| [TOOL-1](#tool-1) | pretag：测试失败时打出失败的包与用例；origin 不可达时失败 | v1.20.0 / v1.20.2 | 是 | 否 | 否 |
| [TOOL-2](#tool-2) | full 场景 add 序列只定义在 `full-scenario-adds.sh` 一处，本地 `source-head-check.sh` 不再吞失败 | v1.23.0（本版） | 是 | 否 | 否 |
| [TOOL-3](#tool-3) | 根包门禁：跟踪文件不得带合并冲突标记 | v1.21.0 | 新门禁 | 否 | 否 |
| [TOOL-4](#tool-4) | 根包门禁：`examples/` 下全部 `main` 包要能编译并运行到退出码 0 | v1.23.0（本版） | 新门禁 | 否 | 否 |
| [TOOL-5](#tool-5) | 根包门禁：跟踪的 Markdown 相对链接必须指向跟踪的文件 | v1.23.0（本版） | 新门禁 | 否 | 否 |
| [TOOL-6](#tool-6) | `scripts/mirror-local.sh`：本机私有依赖进程上的 Mirror 故障与性能；本版加 `test-core` 等 | v1.22.0（扩展 v1.23.0） | 新工具 | 否 | 否 |
| [TOOL-7](#tool-7) | 故障矩阵：没跑用例的格记 FAIL，全局运维命令持验收锁；本版预跑 21 格全 PASS | v1.20.2（预跑记录 v1.23.0） | 是 | 否 | 否 |

共 39 条：APP 13、OWN 6、CLK 6、OPS 7、TOOL 7。

## 本部分的版本时间线

| 版本 | tag 提交 | 本部分落地的条目 |
| --- | --- | --- |
| v1.20.0（2026-10-05） | `999dc672` | APP-1～5、APP-13（`singleton`）、OWN-1～4、TOOL-1（失败打印） |
| v1.20.1（2026-10-05） | `be7407ab` | OWN-4 的候选校验（RR-20261005-01）；OWN-3 的退款预算改由配置提供（随 U-0280） |
| v1.20.2（2026-10-06） | `c85d4565` | APP-6、APP-9、OWN-5、OPS-1、TOOL-1（origin 不可达）、TOOL-7（无用例记 FAIL、持锁） |
| v1.21.0（2026-10-06） | `4881f2b7` | APP-4（停机期非零退出）、APP-5（C5）、APP-7（NC-233 / 234）、APP-8、APP-10、APP-13（`clock.business`）、CLK-1、CLK-2、CLK-4 的拆分、CLK-6、OPS-3、TOOL-3 |
| v1.22.0（2026-10-06） | `9bf690fb` | CLK-3、CLK-4（合并回业务钟）、TOOL-6 |
| v1.23.0（本版） | 待打 | APP-7（`Serial`）、APP-11、APP-12、APP-13（`singleton_incarnation`）、OWN-6、CLK-5、OPS-2、OPS-4～7、TOOL-2、TOOL-4、TOOL-5、TOOL-6 扩展、TOOL-7 预跑 |

## 需要业务或运维改动的清单

按“升级到 v1.23.0 时要做什么”排列；从 v1.19.2 直接升级的部署全部适用，从中间版本升级的按条目首发版本取舍。

| # | 谁 | 要做什么 | 条目 |
| --- | --- | --- | --- |
| 1 | 仓外调用 `global` 租约 API、`mods.ModRedisLock`、`mods.ModEtcdElection` 的应用 | 改用 App 单实例锁（`singleton.enabled: true` + bootstrap `App.Singleton(kitredis.SingletonStore)`，查询方 `app.Lookup[app.SingletonLiveness](r, app.ModSingleton)`）；键级锁 / 选主自己用 `redisdriver.Assemble(cfg).Locks`、`etcddriver.Assemble(cfg).Election` 装配 | APP-3 |
| 2 | 运维 | 从 v1.20.0 之前升级后手工删除旧 `<global.key_prefix>:lease:*` 键（无 TTL，不再读写），见 [DEPLOYMENT §7.1](../../DEPLOYMENT.md#71-升级后的手工清理) | APP-3 |
| 3 | 手工装配 / 已生成工程想用单实例锁 | 写 `singleton.{enabled,key_prefix,ttl,renew_interval,guard,startup_wait}`；bootstrap 装 opener；`shutdown.total_timeout` 与部署宽限期加 3s；k8s `startupProbe` 覆盖 `startup_wait + 30s` | APP-1、APP-2 |
| 4 | 已生成工程的 etcd 配置 | `etcd.service_prefix` 补结尾 `/`（同一部署的进程一起改） | APP-2 |
| 5 | 自写 `app.Service` | `Shutdown` 必须容忍部分初始化：`Init` 返回错误时 App 也会调用它 | APP-9 |
| 6 | 用 readiness 在 80% 容量时卸载流量的部署；直接读 `health.Snapshot.OK` 的代码 | 前者改看响应体 `degraded` 或各来源指标；后者需要旧语义时用 `OK && !Degraded` | APP-10 |
| 7 | 自写 health checker | 1.5s 内返回（或 `Registry.SetCheckTimeout`）；超过记 Fail | APP-11 |
| 8 | game-demo 客户端 | `player_elsewhere`（100015）改连 `owner_sid` 对应服；`owner_sid=0` 时重新 `SelectRole` | OWN-2 |
| 9 | 已生成的 game-demo 工程 | 不迁移（维护者决定）；需要新语义按新模板重新生成或手工移植 `playerowner.go`、`gift_saga.go`、`activity.go`、组文件 | OWN-2～5 |
| 10 | 托管 activity 的新工程 | 维护 `configs/activity_groups.yaml`（每组 ≤ 64、sid 唯一）；某环境要不同分组时挂载覆盖该路径或改 `activity.groups_file` | OWN-5 |
| 11 | 业务代码 | 业务时间读 `app.BusinessClock(registry)` 或注入的 `Config.Now`；系统时间（租约、超时、TTL、日志）继续用 `time` | CLK-1 |
| 12 | 生产配置 | `time.logic_offset` 必须为 0（否则拒绝启动）；同一套配置里所有服务的偏移一致（doctor 检查） | CLK-1、CLK-2 |
| 13 | chat 客户端 | 展示时间改读 `Message.SentAtUnix`（旧消息读出时已用 `StoredAtUnix` 兜底） | CLK-2 |
| 14 | 测试环境 | 偏移只能前拨或保持；要“回到过去”只能清库（连同 `<singleton.key_prefix>:business_time`）重建；配非 0 偏移的进程必须能连上协调存储 | CLK-3 |
| 15 | 手工装配 activity / mail 的代码 | 删除 `activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace` 的赋值；`mail.DefaultEnvelopeStorageGrace` 改名 `EnvelopeStorageGrace` | CLK-4 |
| 16 | 看板 / 告警 | 服务指标改查 `service_*` 六个固定名（`key` 标签）；session 清扫改 `dropped` 计数；CAS 冲突改查 `versionstore_conflict_total{store}` / `versionstore_cas_total` | OPS-1、OPS-5 |
| 17 | 同机多实例部署 | 每个实例配不同的 `ops.addr`（端口占用现在启动失败） | OPS-3 |
| 18 | 运维脚本 | `/admin/*` 用 `Authorization: Bearer <token>` 或 `X-Admin-Token`；超过 10s 的配合 ctx 的命令调大 `ops.admin_timeout` | OPS-3、OPS-4 |
| 19 | 自定义 robot `IdentityProvider` | 按“`Count` + 每次扩回的数量”准备身份，序号不再复用 | OPS-7 |
| 20 | 新增示例的贡献者 | 登记进根包 `exampleRuns`；示例模块依赖变化时在该模块 `GOWORK=off go mod tidy` | TOOL-4 |

---

<a id="app"></a>
## APP：App 生命周期

本主题的共同前提是维护者 2026-10-05 的两条决定：只处理“同一 sid 崩溃重启时短暂并存两个进程”；这个保证由 App 本身提供，模块不感知锁、不各自检查。之后“所有这种都需要 app 接管”扩展到存活查询、失锁自停与停机收尾。原话与决定表见 [APP-SINGLETON-LOCK §0、§10、§12](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)。

<a id="app-1"></a>
### APP-1 App 单实例锁：同一服务类型 + sid 只跑一个进程

**一句话**：`singleton.enabled=true` 时，App 在第一个 Mod `Init` 之前用 Redis 单键 CAS 取 `<singleton.key_prefix>:<server_type>:<sid>`，持有期间按固定节拍续期，失锁即 fail-stop，全部 Mod 停完才释放。

**背景**：game-demo 的 `PlayerOwners` 用按玩家的 Redis 租约保证“同一玩家至多一个写者”，2026-10-04 一天修了 7 条交错缺陷（RR-20260930-23、RR-20261001-07、RR-20260921-03 / 04、RR-20261004-10 / 11 / 14），每修一条都暴露更窄的交错（[租约状态机方案](../../feature/PLAYEROWNER-LEASE-STATE-MACHINE-2026-10-04.md) §0）。维护者确认前提是“玩家静态绑定到 sid、一个 sid 只有一个进程”，于是问题收缩为“同一 sid 崩溃重启时的短暂并存”。没有 App 锁时，新进程 P2 在 DataEngine 打开 WAL（`flock` 失败）之前已经启动了全部前置 Mod（bus 订阅、Redis 写、RemoteEntity 恢复）；旧进程 P1 恢复（SIGCONT）后不知道自己已被替换，继续服务。

**维护者决定**（[方案 §0 / §10](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)）：只考虑同一 sid 崩溃重启时短暂出现两个进程；保证由 App 提供，DataEngine / activity / PlayerOwners 不感知；不做 DataEngine 层 per-record fencing；D-A“窗口耗尽即 fail-stop”、D-B“只对带 dataengine 的服务默认启用”按推荐。没有采用：放在 kit Mod（Mod 按阶段整体推进，任何 Mod 都无法先于其他 Mod 的副作用）、放在 `Service.Init` 第一步（那时 Mod 都已启动）、窗口耗尽先停准入再宽限（要在每个入口加检查，与“模块不感知锁”冲突）。

**现在的行为**：

| 项 | 行为 |
| --- | --- |
| 获取 | 键被别人持有就每 `renew_interval` 重试，最多 `startup_wait`；到上限返回 `app.ErrSingletonHeld`（最后一次是报错则 `app.ErrSingletonStoreUnavailable`），不抢锁；丢回复后读到键是自己的值则认领 |
| 续期 | 固定节拍；窗口从请求发出时刻（`asked`）起算；迟到的 Applied 不作数、立即再续；续期答“不是我的”，或续期失败已到 `validUntil − guard`，即 `RuntimeFailure.Fail(app.ErrSingletonLost …)` |
| 释放 | 只在全部 Mod 停完且未失锁时 `CompareAndDelete`；`Service.Shutdown` 超时、任一层 Mod 停机不完整、失锁三条路径不释放，键在 TTL 内过期 |
| 停机预算 | 启用时 Mod 停机截止时间提前 `min(3s, total_timeout/2)` 留给 Release；进入停机时已失锁则不预留 |
| 健康 | `/readyz` 多一项 `singleton`：持有 OK，续期结果未知 Degraded（D1 起算就绪），失锁 / 未持有 Fail |
| 配置 | `singleton.enabled`（缺省 false）、`key_prefix`（启用时必填、无空白）、`ttl` 15s、`renew_interval` 3s、`guard` 5s、`startup_wait` 缺省 2×ttl；`ValidateServiceConfig` 钉住 `renew_interval ≤ guard`、`2×renew_interval ≤ ttl − guard`、`startup_wait ≥ ttl + 2×renew_interval` |
| 后端 | `kitredis.SingletonStore`：同一份 `redis.*` 建两个独立小客户端（CAS 一个、Live 一个，各 PoolSize 2，关闭驱动自动重试）；缺 `redis.addr` / `redis.cluster_addrs` 报错，不用 localhost 兜底 |
| 日志 | `singleton: acquiring` / `waiting … holder=…` / `acquired` / `singleton lock lost; fail-stop` / `released` / `lock was lost; not releasing` / `shutdown incomplete; leaving the key to expire` |

**兼容与迁移**：不开就与以前完全相同。开启后：崩溃重启的新进程最多等约 `ttl`（实测 15.0s）才启动 Mod；Redis 连续不可用约 10s 以上进程会 fail-stop 退出（需要更宽容时调大 `ttl`）；`singleton.enabled=true` 而 bootstrap 没装 opener 启动失败（`app.ErrSingletonOpenerMissing`）。v1.20.2 起 `singleton.enabled: on` 之类的非布尔写法启动校验报错（NC-190，属 CFG 主题）。v1.23.0 起 store 的 `Close` 出错后再调返回 nil（APP-7）。

**已知限制 / 外部验证**：跨主机、换卷、网络分区、Redis failover 丢键不在范围内（维护者决定）。P1 恢复到收到 NotHeld 之间（一次 Redis 往返）可能多发生一次非 DataEngine 副作用，列为接受的边界。真实 Redis Cluster 下两客户端的 integration 与多机切主未验证（外部验证 E08 / E10 / E13）。

**链接**：[实现 APP-1](impl-app-own-clk-ops-tool.md#app-1) · [方案与实施记录](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [USER_GUIDE 单实例锁](../../USER_GUIDE.md#单实例锁singleton)

<a id="app-2"></a>
### APP-2 生成器装配单实例锁与部署启动等待；演练发现的 etcd 两处修复

**一句话**：项目里有 `redis` Mod 或带 `dataengine` 的服务时，bootstrap 生成 `a.Singleton(kitredis.SingletonStore)`；带 dataengine 的服务配置默认 `singleton.enabled: true`，其他服务写 `enabled: false`；停机预算与部署启动等待随之调整。

**背景**：方案第 2 笔（[§6.3](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)）。是否启用写在归应用所有的配置里，bootstrap 是生成文件、用户不能改，所以 opener 按“项目有 redis 或有 dataengine 服务”总是安装（dataengine 服务不一定带 redis Mod，不装会让默认打开的 singleton 启动即 fail-closed）。第 5 笔真实进程演练发现两处代码缺陷：生成的 `etcd.service_prefix` 没有结尾 `/`（game 1300 注册成 `/roost/servicesgame/1300`）；进程暂停超过 `etcd.lease_ttl` 后停机注销报 `requested lease not found`，正常 SIGTERM 退出码非零。

**维护者决定**：D-B 只对带 dataengine 的服务默认启用（其他服务可以手工打开）。

**现在的行为**：

| 项 | 新生成工程 |
| --- | --- |
| 配置 | dataengine 服务 `singleton.enabled: true`、`key_prefix: roost:<project>:singleton`、15s / 3s / 5s / 30s；缺 `redis:` 段时只补配置段；`add mod dataengine` 后把未改过的 `enabled: false` 段翻成 true，改过的段保持并 WARN |
| 停机预算 | 启用的服务 `shutdown.total_timeout` 计入 Release 的 3s（game-demo game 服务 114s / 119s → 117s / 122s）；`roost project doctor` 按配置里的 `singleton.enabled` 计入 |
| 部署 | k8s `startupProbe` 按 `startup_wait + 30s`（缺省仍 60s）；shell `HEALTH_ATTEMPTS` 按服务（启用 60、其他 30）；compose `start_period` 启用的服务 60s |
| etcd | 新工程 `service_prefix: /roost/services/`；Discovery 对“租约已不存在”视为注销已达成（记 Info），停机期间才完成的重注册不再留 keepalive |

**兼容与迁移**：已生成工程不自动加 `singleton` 段；未改过的 `shutdown:` 段由 `roost project sync` 随新公式刷新。`etcd.service_prefix` 需要手工补 `/`，同一部署的所有进程一起改（注册与查询用同一前缀，混跑期间互相看不见）。

**已知限制 / 外部验证**：kubeconform / `docker compose config` 对新模板的渲染、真实 systemd / k8s 部署（E21 / E22）未在本机执行。

**链接**：[实现 APP-2](impl-app-own-clk-ops-tool.md#app-2) · [方案 §13 第 2 笔、第 5 笔](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)

<a id="app-3"></a>
### APP-3 “按进程 / sid 唯一”统一归 App：删除 global 租约 API，停发选主与锁 capability

**一句话**：kit `service/global` 删除游戏服租约 API（`AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames` 及相关类型、配置与错误码）；kit 不再发布 `redis.lock`、`etcd.election` capability；etcd Discovery 只做地址发现。

**背景**：方案 §12 全仓盘点了“按进程 / 按 sid 的单实例、存活登记、持有者租约、失锁自停”这一类机制。activity 改用 App 的 `Live` 之后（OWN-4），global 租约 API 唯一的调用方消失；两个 capability 在仓内没有使用者。

**维护者决定**：“所有这种都需要 app 接管”（[§12](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)），取代方案 §7.2 末条“保留、不标弃用”的推荐。路由 `Bind` / `Resolve` / 迁移属于分组归属（epoch CAS），不属于这一类，保留。core 原语 `redis.IDistLock`、`etcd.IElection` 保留给键级用途。

**现在的行为**：`global.Routing` 与 `Service` 只剩路由五个方法；错误码 570105～570108（租约）与 570109（只由 `LiveGames` 产生）退役、不复用；`global.lease_ttl` 不再读取（旧配置留着不报错）；`kit/mods` 删除 `ModRedisLock`、`ModEtcdElection` 常量；etcd 注册不再承担存活语义，`App.Live` 是唯一的存活权威。

**兼容与迁移**：**破坏性**。进程存活改用 App 单实例锁：被查的服务 `singleton.enabled: true` 并装 opener，查询方 `app.Lookup[app.SingletonLiveness](registry, app.ModSingleton)` 后 `Live(ctx, serverType, sids)`；`Live` 不按 global 组与路由世代过滤、不带负载快照。键级锁 / 选主自己装配 `redisdriver.Assemble(cfg).Locks`、`etcddriver.Assemble(cfg).Election`。旧部署 Redis 里 `<global.key_prefix>:lease:*` 键不再被读写，可以删除（v1.23.0 在 DEPLOYMENT §7.1 写明）。

**已知限制**：仓外调用方无法核对，按破坏性变更登记。

**链接**：[实现 APP-3](impl-app-own-clk-ops-tool.md#app-3) · [方案 §12、§13 第 2b / 3b 笔](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [DEPLOYMENT §7.1](../../DEPLOYMENT.md#71-升级后的手工清理)

<a id="app-4"></a>
### APP-4 `RuntimeFailure.OnFail` 与统一 fail-stop

**一句话**：任何 fail-stop（失锁、DataEngine fatal、Remote fatal）都先同步执行登记的回调（kit Nest Mod 登记了 `NestMgr.Fence`），再唤醒停机；启动阶段的 fail-stop 在阶段边界停下；停机开始之后才发生的 fail-stop 并入 `run` 的返回值、进程非零退出。

**背景**：之前 DataEngine 的 `onFatal` 自己找 Nest 围栏，Remote fatal 只调 `Fail`、不围栏 Nest，每加一个来源就要再写一遍。`run` 只在进入 Serve 的 select 之后才看失败，启动期间（DataEngine 重放很长）发生的失锁会让后面的 Mod 继续启动。信号之后才发生的 RuntimeFailure 只写 Error 日志、进程以 0 退出（RR-20261005-NC-232，来源 N01/S4 O1）。

**维护者决定**：fail-stop 由 App 统一触发（方案 §5）；“App 级统一保证”（单实例、存活、fail-stop 在 core app 做一次，模块继承）。

**现在的行为**：`OnFail(hook)`——首次失败时恰好调用一次、按登记顺序、在调用 `Fail` 的 goroutine 上同步执行，全部执行完才投递 `Done`；失败后登记立即调用；回调 panic 被 recover 并入 `Err`；回调里再调 `Fail` 不死锁。**行为变化**：Remote Entity fatal 现在也会围栏 Nest（`nest.ErrNestFenced`）；停机期间的 fail-stop 让 `run` 返回 `app: runtime failure after shutdown began: …`。安全动作（围栏、失锁不 Release）不变。

**兼容与迁移**：依据退出码判断“正常停机”的部署脚本，在停机期间发生 fail-stop 时会看到非零退出——这是有意的。

**已知限制**：`NestMgr.Fence` 不打日志，演练中可见的只有 `runtime infrastructure failure`（方案 §13 第 5 笔偏差 2）。

**链接**：[实现 APP-4](impl-app-own-clk-ops-tool.md#app-4) · [方案 §5](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [NC-232 修复](../../bugfix/RR-20261005-NC-232.md)

<a id="app-5"></a>
### APP-5 只读活性查询 `Live` 与“停机中仍算活”契约（C5）

**一句话**：`app.SingletonLiveness.Live(ctx, serverType, sids)` 按 `<key_prefix>:<serverType>:<sid>` 逐键读、值非空即活，按入参顺序返回；一次最多 200 个 sid；停机中的进程到 Release 删键为止都算活。

**背景**：activity 原来用自己的 global 租约算“协调器该等哪些 game 服”。维护者追加决定“走 App 级别，不需要各个模块单独处理”，活性改由 App 锁提供。C5 来自方案 §7.2 差异 3：停机中的进程若恰在开窗时被算进 expected，该窗口要等到宽限期。

**维护者决定**：C5（第四轮）“停机中的进程仍算‘活着’：保持现状，写进 `Live` 契约”。没有采用推荐的“停机开始时把键值改成‘停机中’”——活性只有锁这一个事实来源。

**现在的行为**：能力名 `app.ModSingleton`（kit 别名 `mods.ModSingleton`），`singleton.enabled=false` 时不登记；Redis 实现逐键 GET（pipeline），Cluster 下跨槽不报 `CROSSSLOT`；空 `serverType` 报错、空 `sids` 直接返回。“活”从拿锁（任何 Mod Init 之前）到全部 Mod 停完、Release 为止；崩溃进程最多再算 `ttl`；停机不完整时算到键过期。

**兼容与迁移**：C5 没有行为变化。用 `Live` 决定“该等谁”的调用方要接受“恰在停机的服会被算进去”。

**已知限制**：`Live` 只能看见开了 singleton 的服务类型；项目给 game 关掉 singleton 时 activity 退化为只等自己（OWN-4 启动即失败）。

**链接**：[实现 APP-5](impl-app-own-clk-ops-tool.md#app-5) · [方案 §3.6、§7.2](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [B9 / C5 方案](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)

<a id="app-6"></a>
### APP-6 停机共用 `operation.Lifetime`、契约测试骨架与 glsvet 停止提示（A3）

**一句话**：在途工作的准入 / 计数 / 等待统一用 `internal/operation.Lifetime`（新增 `Wait(ctx)`），停止入口的回归统一用 `internal/stopcontract.Check`，`glsvet` 对带 ctx 的停止函数里不受 ctx 约束的通道接收打印 `hint:`。

**背景**：同一不变量“停止返回 = 回调已静止、资源可释放；重试收敛”被打破 11 次（RR-20261004-07 / 08、NC-04 / 09 / 83 / 90、NC-170～174），“准入 + 在途计数 + idle 通道”在 bus、syncbus、mirror 各写一份。

**维护者决定**：DECISIONS-PENDING A3“按推荐：① 共用小类型 + 停机契约测试骨架，③ glsvet 只提示”；② 排空下沉到 `ISyncBus` 带 ctx 的退订留到下个大版本。此前 roost-coding 里“不为此抽公共框架类型”的说法作废。

**现在的行为**：bus JetStream RPC、syncbus JetStream、mirror Replicator 三份手写门改用 `Lifetime`，错误值与停止顺序不变；唯一可观察差异是“已排空且 ctx 已结束”时不再随机返回 ctx 错误（如实返回 nil）。骨架套到 manager、kit/nest、syncbus、etcd、mirror、remoteentity、bus、生成 TCP、Ops；骨架在 etcd `Assembly.Close` 上发现“已停完再调用返回 `context canceled`”，作为 NC-173 复核补修。`glsvet -stophints`（缺省开）只提示、不计入违例、不改退出码；不提示 `Mutex.Lock`（实测误报约 100%）。

**兼容与迁移**：公开 API、wire、持久格式、生成形状都不变。新的停机对象按 [roost-coding 生命周期一节](../../agent-skills/roost-coding/SKILL.md)用共用类型并套骨架。

**已知限制**：`worker.Pool` 自带等价机制，未改用 `Lifetime`；A3 ② 未做。

**链接**：[实现 APP-6](impl-app-own-clk-ops-tool.md#app-6) · [方案与实施](../../feature/REFACTOR-2026-10-05-shared-stop-contract.md) · [stopshape 方向判断](../../bugfix/evidence/noncore-bugfix-20261005-stopshape/README.md)

<a id="app-7"></a>
### APP-7 停止入口串行器 `operation.Serial` 与 kit Mod 停止收敛

**一句话**：新增零值可用的 `internal/operation.Serial`，并发的停止调用串行执行、后到者的等待受它自己的 ctx 约束；kit Redis / Mongo / Nats Mod 的停止入口串行化，重复 Stop / Close 返回 nil，第一次的错误只报一次。

**背景**：RR-20261006-10（来源 W-2026-10-06-02）发现驱动与 Mod 的重复 Close 口径不一致（单机 redis 第二次 Close 返回 `ErrClosed`、Cluster 返回 nil；etcd Client 与单实例锁 store 粘滞返回第一次的错误），kit Redis / Mongo / Nats Mod 的停止入口并发调用有数据竞争。更早的 NC-233（v1.21.0）：Redis Mod 第一次 Close 出错后每次重试都得到 `client is closed`，停机永远不收敛；NC-234（v1.21.0）：remote_entity Mod 停止失败也记 `stopped`。驱动侧的完整口径属于 DRV 主题。

**维护者决定**：第十二轮“驱动 Close 契约：写进 A2 驱动契约表”；实测发现不一致后转 RR 统一口径。用 `sync.Mutex` 被否：后到者的等待不受自己的 ctx 约束，第一个调用者用 Background 等一个不结束的排空时会跟着永远等。

**现在的行为**：

| 对象 | 现在 |
| --- | --- |
| kit 单实例锁 store（`kitredis.SingletonStore`） | 第一次 Close 的错误只报一次，之后 nil（v1.20.0 是 `sync.Once` 返回同一个结果） |
| kit Redis Mod | 第一次 Stop 先交出连接池再 Close，错误包成 `redis mod: close (the connection pool is closed regardless): …` 只报一次（v1.21.0）；`mu` 保护 `asm`，Stop 与健康检查不再竞争（v1.23.0） |
| kit Mongo / Nats Mod | Stop 用 `operation.Serial` 串行，后到者在自己的 ctx 内等；`mu` 保护健康检查读的字段 |
| remote_entity Mod | 停止失败记 Warn `remote_entity mod: stop incomplete`，停完才记 `stopped`（v1.21.0） |

**兼容与迁移**：依赖“第二次 Close 报错”判断已关闭的调用方改用命令错误 `errors.Is(err, goredis.ErrClosed)`；App 停机路径每个 Mod 只串行调一次，不受影响。

**已知限制**：kit Redis Mod 用的是普通 `sync.Mutex`（go-redis Close 不等在途命令、持锁很短），没有用 `Serial`；见实现文档检查点。

**链接**：[实现 APP-7](impl-app-own-clk-ops-tool.md#app-7) · [RR-20261006-10 修复](../../bugfix/RR-20261006-10.md) · [NC-233](../../bugfix/RR-20261005-NC-233.md) · [NC-234](../../bugfix/RR-20261005-NC-234.md)

<a id="app-8"></a>
### APP-8 停机阶段的 lifecycle hook 受停机总预算约束

**一句话**：`service.stopping` / `service.stopped` 的 hook 在 `shutdown.total_timeout` 内等；`stopping` 卡住时与 `Service.Shutdown` 不完整相同——不调 Shutdown、不停 Mod、不释放单实例锁，`run` 按预算返回。

**背景**：RR-20261005-NC-231（来源 N01 留项）：`shutdownCtx` 只传给了 hook，没有约束 `run` 自己的等待；一个忽略 ctx 的 hook 让停机永远不返回。

**维护者决定**：N01 留项按 roost-coding 三步停机修（“维护者第四轮：补齐单元内留项”）。没有采用“hook 超时后继续 Shutdown 与停 Mod”——hook 可能正用着 Service / Mod 的能力，在它底下拆依赖违反三步停机③。

**现在的行为**：hook 超时返回 `app: lifecycle <phase> hooks did not return within the shutdown budget: context deadline exceeded`。`stopped` 阶段卡住只让 `run` 按预算返回，剩余时间不够时 Release 跳过（键在 ttl 内过期），释放规则不变。启动阶段的 hook 仍同步派发（启动由 k8s startupProbe 兜底）。

**兼容与迁移**：按时返回的 hook 行为不变。自写停机 hook 应配合 ctx。

**链接**：[实现 APP-8](impl-app-own-clk-ops-tool.md#app-8) · [NC-231 修复](../../bugfix/RR-20261005-NC-231.md)

<a id="app-9"></a>
### APP-9 启动失败先收回 Service 已启动的部分（NC-193）

**一句话**：`Service.Init` 返回错误、或 Init 之后的启动失败，App 都先调用 `Service.Shutdown`（限时 5s）再停 Mod、释放锁；Shutdown 没在时限内结束（含不配合 ctx、panic）时不停 Mod、不释放锁。

**背景**：RR-20261005-NC-193（N14）：Init 失败时直接拆依赖，game-demo Init 里已启动的场景、spawner、activity 循环被留在运行中；Init 之后的收尾不配合 ctx 会让进程挂住；收尾超时后仍停 Mod、释放锁，与正常停机“Shutdown 不完整就保留依赖”的规则不一致。

**维护者决定**：N14 收口按推荐修。没有采用“只改 game-demo 的 Init 自己收尾”（框架契约仍不覆盖其他 Service）与“收尾预算改用 `shutdown.total_timeout`”（启动失败不在部署的停机宽限期里）。

**现在的行为**：错误文本在超时时由 `cleanup after startup failure: context deadline exceeded` 变为 `cleanup after startup failure incomplete: …`；Shutdown 返回普通错误视为已结束、错误并入启动错误。

**兼容与迁移**：**契约收紧**：`app.Service.Shutdown` 必须容忍部分初始化（Init 返回错误时也会被调用）。仓内实现都已满足（生成的 servicerpc、game 模板、examples 返回 nil 或逐字段判 nil）；外部 Service 若在 Shutdown 里解引用 Init 才设置的字段会 panic，被 recover 成“收尾不完整”，进程以启动错误退出、不释放锁（安全方向）。

**已知限制**：真实 game-demo 进程里制造 Init 中途失败未验证（需要整套依赖与故障注入）。

**链接**：[实现 APP-9](impl-app-own-clk-ops-tool.md#app-9) · [NC-193 修复](../../bugfix/RR-20261005-NC-193.md)

<a id="app-10"></a>
### APP-10 `/readyz`：Degraded 算就绪（D1）

**一句话**：只有 checker 为 Fail（或就绪位为假）时 `/readyz` 返回 503；有 Degraded 时返回 200、`ok: true`，响应体新增 `degraded` 与 `degraded_dependencies`。

**背景**：`/readyz` = 就绪位 ∧ `health.Snapshot.OK`，以前任何非 OK 都让 `OK=false`。Degraded 的四个来源（单实例锁续期结果未知、entitysync ≥ 80% 容量、remoteentity 写许可用满、DataEngine 积压告警）都是“还能服务、需要关注”；生成的服务都是单副本，503 让 k8s 摘掉唯一的 endpoint，entitysync 在 80% 边界上没有滞回会来回翻转。

**维护者决定**：第五轮 D1“按推荐：Degraded 算就绪（`/readyz` 返回 200 并在响应体注明降级），只有 Fail 返回 503”。没有采用 (c)“每个 checker 自己声明是否影响就绪”——四个来源语义一致，多一个开关只增加配置面。

**现在的行为**：聚合规则只在 `health.Registry.Snapshot` 一处：`OK` 表示“没有 Fail”，新增 `Snapshot.Degraded` / `DegradedResults()`；`/healthz` 不变（无条件 200）。

**兼容与迁移**：`health.Snapshot.OK` 的含义从“全部 OK”变为“没有 Fail”，需要旧语义的调用方用 `OK && !Degraded`。生成的探针都只看状态码，模板不改。依赖 readiness 在 80% 容量时卸载流量的部署失去这个效果（仓内没有）。

**链接**：[实现 APP-10](impl-app-own-clk-ops-tool.md#app-10) · [D1 方案](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md) · [OBSERVABILITY“健康与就绪”](../../../OBSERVABILITY.md)

<a id="app-11"></a>
### APP-11 `/readyz` 每个 checker 的期限

**一句话**：`health.Registry.Snapshot` 并发调用全部 checker，每个最多等 `health.DefaultCheckTimeout`（1.5s，`SetCheckTimeout` 可改），到期未返回记 Fail 并写明期限与已跑时长；同一 checker 同一时刻只有一次调用。

**背景**：N01b 观察 O-H1：`/readyz` 用请求 ctx 串行跑全部 checker，一个不返回、不看 ctx 的 checker 让 `/readyz` 一直挂着，每次探针多留一个卡住的 handler，Ops 停机还要等它们。

**维护者决定**：第十二轮“readyz checker 期限：每个 checker 短期限，卡住报 Fail”（第十二轮总原话：“B 类的都按照推荐即可，mongo 的延迟可以分析下”）。取 1.5s 的理由：k8s 探针 `timeoutSeconds: 2`，并发之后整个 `/readyz` 在探针断开之前答完。

**现在的行为**：到期记 Fail：`message: check timed out`，`error: health check did not return within its deadline (per-check limit 1.5s; in flight for …)`；请求 ctx 先结束记 `check abandoned`。checker 拿到的 ctx 脱离请求的取消、只带期限。后来的探针等同一次调用（各自的期限内），卡住的 checker 杀不掉，但不会每次探针多一个 goroutine。Redis / Mongo / etcd checker 自带的 2s ping 超时被截到 1.5s。

**兼容与迁移**：以前慢但最终返回的 checker（> 1.5s）现在记 Fail、让 `/readyz` 503；必要时 `SetCheckTimeout`。

**链接**：[实现 APP-11](impl-app-own-clk-ops-tool.md#app-11) · [第十二轮 kit 批 §3](../../feature/DECISIONS-R12-KIT-2026-10-06.md#3-readyz-每个-checker-的期限)

<a id="app-12"></a>
### APP-12 退出原因写进文件日志

**一句话**：`run` 出错时，在关闭文件日志之前写一行 `slog.Error("app run failed", "err", …)`，带上停机期间并入的 RuntimeFailure 与单实例锁收尾结果。

**背景**：RR-20261006-07（收尾 A13）：生成的 main 在 `run` 返回、文件日志已关之后才打印 `server exit`，那一行只到 stderr；启动阶段失败（Mod Init / Provide / Start、单实例锁、业务时间检查）直接返回，日志文件里没有退出原因。相关的更早修复 RR-20261005-NC-165（v1.20.2，NONCORE 主题）：`log.Close` 之后默认 logger 改写控制台或 stderr，`server exit` 在 `log.stdout: false` 时也有去处。

**维护者决定**：第十一轮“收尾：盘点全部未完成问题，处理完后统一发一个版本”，收尾第 4 批 A13。

**现在的行为**：只在出错时写；与此前 `service exited with error` 等行可能各出现一次（有意：这一行是“进程以什么结论退出”）。生成的 main 里 `server exit` 不动。

**兼容与迁移**：返回值与退出码不变；按日志告警的部署会多看到一条 Error。

**链接**：[实现 APP-12](impl-app-own-clk-ops-tool.md#app-12) · [RR-20261006-07 修复](../../bugfix/RR-20261006-07.md) · [收尾第 4 批](../../bugfix/CLOSING-BATCH-4-2026-10-06.md)

<a id="app-13"></a>
### APP-13 App 向 Mod 提供的能力与 Mod 依赖

**一句话**：App 在任何 Mod 之前登记三个能力——`singleton`（`SingletonLiveness`，v1.20.0）、`clock.business`（`clock.Business`，v1.21.0）、`singleton_incarnation`（`SingletonIncarnation`，v1.23.0）；依赖它们的 Mod / 服务取不到时启动失败（fail-closed），不退化。

**背景**：“App 级统一保证”让模块从 Registry 取 App 已经做好的事：activity 取 `Live`（OWN-4），业务服务取业务时钟（CLK-1），`RemoteEntityMod` 取本次启动的锁身份以便同 sid 重启时立即接管上一代进程留下的 Remote 实体锁（O-M6-6，维护者第十一轮“按推荐”，完整行为属于 REM 主题）。

**现在的行为**：

| 能力 | 登记条件 | 依赖方 | 取不到时 |
| --- | --- | --- | --- |
| `app.ModSingleton` / `mods.ModSingleton` | `singleton.enabled=true` | game-demo activity（`startActivity`） | 启动失败，错误点名能力并提示 `singleton.enabled=true` |
| `app.ModBusinessClock` | 总是（`NewRegistry` 按 `time.logic_offset` 登记） | kit activity / mail / rank / session / match / chat / account Mod、game-demo | `app.BusinessClock` 退回进程级业务时钟 `clock.Process()` |
| `app.ModSingletonIncarnation` / `mods.ModSingletonIncarnation` | `singleton.enabled=true` | `RemoteEntityMod`（sid 相同时才交给 Remote） | 不接管，照旧按 `remote_entity.lock_ttl` 等 |

**兼容与迁移**：用 game-demo activity 的 game 服务必须开 singleton（生成默认已开）。

**链接**：[实现 APP-13](impl-app-own-clk-ops-tool.md#app-13) · [Mirror 第 6 步观察 §7（O-M6-6）](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)

---

<a id="own"></a>
## OWN：玩家所有权与活动组

<a id="own-1"></a>
### OWN-1 玩家租约状态机期的三处修复（已被 OWN-2 取代）

**一句话**：RR-20261004-10（刷新回合重新认领加时间预算、续租确认先于等待）、RR-20261004-11（撤离进行中 `Admit` 拒绝）、RR-20261004-14（租约窗口从设键那次请求起算、认领丢回复后下一轮 Held 改为重新认领）在 v1.20.0 开发期间先修了，随后整个按玩家租约机制在同一版本里被静态绑定删除。

**背景**：这三条加上 RR-20260930-23 等共 7 条是“一天内修了 7 条交错缺陷”的后半段。维护者据此要求先写[状态机方案](../../feature/PLAYEROWNER-LEASE-STATE-MACHINE-2026-10-04.md)，随后确认“玩家可以在进程间动态迁移”的前提不成立，改为静态绑定（roost-coding“反复出问题要上报方向判断”的先例）。

**现在的行为**：v1.20.0 发布的代码里不存在按玩家租约；三条修复只体现在历史提交与回归的“转写去向”里（RR-20261004-14 的两条承诺由 App 锁回归 `TestSingletonWindowStartsWhenTheRenewalWasAsked`、`TestSingletonClaimsItsOwnValueAfterALostAcquireReply` 承担；RR-20261004-11 的承诺由驻留表的 `TestAnUnloadWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds` 承担）。

**兼容与迁移**：无。v1.19.2 生成的工程若手工合并过这三笔 `playerowner.go`，升级时以 OWN-2 的模板为准。

**链接**：[实现 OWN-1](impl-app-own-clk-ops-tool.md#own-1) · [RR-20261004-10](../../bugfix/RR-20261004-10.md) · [RR-20261004-11](../../bugfix/RR-20261004-11.md) · [RR-20261004-14](../../bugfix/RR-20261004-14.md)

<a id="own-2"></a>
### OWN-2 game-demo 玩家所有权改为静态绑定

**一句话**：玩家建角时由 account 绑定到一个 game sid（`Role.ServerID`）、之后不迁移；`PlayerOwners` 不再维护按玩家的 Redis 租约，只剩本地驻留表与闲置卸载；登录按会话 Claims 里的 `server_id` 判定。

**背景**：见 OWN-1。静态绑定后“同一玩家至多一个写者”等价于“只在玩家绑定的 sid 上为他服务”，同 sid 的短暂并存由 APP-1 处理。

**维护者决定**（[静态绑定方案](../../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)开头的决定块）：已生成工程不考虑（不写迁移、不兼容旧 payload）；D1～D3 按推荐；D5 改为 App 层单实例锁；activity 不再持有自己的全局租约（“走 App 级别，不需要各个模块单独处理”）。

**现在的行为**：

| 入口 | 行为 |
| --- | --- |
| 登录 `Serve(ctx, playerID, boundSID)` | `boundSID` 不是本服（含 0）→ `player_elsewhere`（`owner_sid=boundSID`），不建记录；副本正在卸载且在 `evictBudget`（5s）或 ctx 内没结束 → `login_timeout`；否则建立 / 刷新驻留记录 |
| 认证 | 认证器把 `role.ServerID` 写进 `Principal.Claims[account.ServerIDClaim]`（`"server_id"`）；Claims 里没有 → fail-closed 拒绝（`owner_sid=0`，记 Error） |
| 写准入 `Admit` / WriteGate | 只看驻留记录与卸载状态；拒绝错误改名 `ErrNotServedHere`（原 `ErrLeaseNotHeld`） |
| 后台准入 `AdmitBound(playerID, boundSID)` | 不是本服 → `local=false`（调用方转交）；本服但卸载中 → 拒绝、不转交；否则建记录 |
| 闲置卸载 | 无连接且 `IdleUnload`（5 分钟）未被使用、每 30s 扫一次；只是本地内存管理，卸载中拒绝准入、登录等它结束（单航班） |
| 停机 | `Service.Shutdown` 第一步断开本进程服务中的全部玩家（fail-stop 时客户端重连到接替进程） |
| 错误码 | `player_elsewhere`（100015）含义收窄为“玩家绑定在另一个服，不要在本服重试”，message `player is bound to another server; reconnect to that server` |

删除：`game/playerroute` 整包、按玩家的租约 / 续租 / 重新认领 / 跨间断撤离 / 归还与投影等待、公开 `Claim` / `Owns` / `OwnedHere` / `OwnerSID` / `Routes` / `Release`、game 配置的 `game_route` 段。matchmaker 改用 `Resident`。

**兼容与迁移**：**破坏性（模板层）**：已生成工程不迁移（维护者决定）。客户端收到 `player_elsewhere` 改连 `owner_sid` 对应服的网关，`owner_sid=0` 时重新 `SelectRole` 并按返回的 `Session.ServerID` 连接。

**已知限制**（方案 §13 观察，未改）：冷加载超过 `IdleUnload` 时实体可能留在内存却没有驻留记录（只占内存）；`EntityManager.Destroy` 永不返回时卸载 goroutine 泄漏；优雅停机先断会话、listener 仍开，立即重连的客户端可能在本进程多登录一次（不形成两个写者）。

**链接**：[实现 OWN-2](impl-app-own-clk-ops-tool.md#own-2) · [静态绑定方案](../../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md) · [App 锁方案 §7、§13 第 3 笔](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [GAME_DEMO_TEMPLATE §9.15](../../feature/GAME_DEMO_TEMPLATE.md)

<a id="own-3"></a>
### OWN-3 赠礼按发送方绑定的 sid 准入与转交

**一句话**：`gift.State` 带上发送方 sid（`FromSID`，json `from_sid`），debit / refund 只在发送方绑定的 sid 上执行（离线发送方也能执行）；不是本服就经 bus 转交给 `FromSID`；转交接收方核对信封与载荷一致、命令 topic 与 phase 匹配；debit 步骤的重试预算覆盖发送方 sid 的一次崩溃重启。

**背景**：静态绑定第 3 笔的过渡形态只准入本进程驻留的发送方，离线发送方的 debit / refund 要等他重新登录或到 saga 截止。第 4 笔审查发现接收方原样信任信封的 Phase：debit 信封包着 refund 命令时会用 refund 的预留执行一次 debit（`c493a791`）。审查还发现发送方 sid 崩溃重启期间退款约 26s 后就用尽、转为 `manual_required`（`ac5acfbe`）。

**维护者决定**：静态绑定方案 §3.3（按 `server_id` 路由），旧 payload 兜底按“已生成工程不考虑”不做。

**现在的行为**：`start_gift` handler 多一个参数 `fromSID int32`，`send_gift` 传本进程 sid，`gift.Encode` 拒绝 `FromSID <= 0`；`FromSID == 0` 的步骤拒绝并记 Error、不兜底；本服但副本卸载中则拒绝、不转交。debit 步骤 `max_attempts` 15（Timeout 5s、退避 100ms..5s），重试窗口约 98～121s，覆盖 `startup_wait 30s + ttl 15s + 45s` = 90s；正反方向共用（扣款正向也 15 次）。

**兼容与迁移**：**破坏性（模板层）**：已生成工程须手工合并 `gift_saga.go`、`gift.go`、`start_gift.go`、`send_gift.go`；进行中的旧赠礼（载荷无 `from_sid`）不会被执行。

**源码与记录不一致（以源码为准）**：v1.20.0 CHANGELOG 与 App 锁方案 §13 obs34 写的是“生成的 `saga/gift_item/definition.go` 里 debit `MaxAttempts` 5 → 15”；v1.20.1 起步骤预算改由配置提供（U-0280，`054fdd66`），现在由 codegen 把 `saga.steps.gift_item.debit.max_attempts: 15` 写进 game 服务的三份配置（`codegen/internal/roost/demo.go` `demoGiftRefundBudget`），`definition.go` 不再写预算。

**已知限制**：两个 sid 同时跑时 demo 场景成本 p95 落进 16.384s 的桶（赠礼步骤弹一两次才转交到位），超过 loadtest 缺省 `-max-p95 16`。

**链接**：[实现 OWN-3](impl-app-own-clk-ops-tool.md#own-3) · [静态绑定方案 §3.3](../../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md) · [App 锁方案 §13 第 4 笔与 obs34](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)

<a id="own-4"></a>
### OWN-4 activity 改用 App 的 `Live`；启动时拒绝开不出窗口的候选

**一句话**：game-demo activity 删除自己的 global 租约（`incarnation`、`AcquireLease` / `RenewLease` / `ReleaseLease`、standby / retake），协调器 expected 集合改为 `Live(ctx, <本进程 server_type>, 候选)` 返回的活 sid；取不到 `app.ModSingleton` 时 activity 启动失败。v1.20.1 起启动时拒绝注定开不出窗口的候选列表。

**背景**：activity 租约 TTL 30s 长于 App 锁 15s，崩溃重启时新进程 `AcquireLease` 撞上旧进程还活着的租约会让 Init 失败。RR-20261005-01：`activity.game_sids` 里重复 sid、超出 int32、本服加配置超过 200 个时，进程照常启动、此后每个窗口都开不出来、只有 Warn。

**维护者决定**：“走 App 级别，不需要各个模块单独处理”（方案 §7.2）。

**现在的行为**：Live 为空时 expected 只有自己；Live 报错时本拍不开窗；`routing.Bind` 的组绑定保留。RR-20261005-01 修复后 `startActivity` 在任何远端调用之前按键名报错、`Service.Init` 失败。v1.20.2 起候选来源换成组文件（OWN-5），`activity.game_sids` 不再存在。

**兼容与迁移**：game 服务必须 `singleton.enabled: true`（生成默认）。崩溃的进程最多算 15s（原来 30s）。

**已知限制**：开窗是先写者赢——日志里的 `expected_game_sids` 是本进程算的，不一定是协调器记录的那份（方案 §13 第 5 笔观察，已写进 GAME_DEMO_TEMPLATE §9.15.3）。

**链接**：[实现 OWN-4](impl-app-own-clk-ops-tool.md#own-4) · [方案 §7.2](../../feature/APP-SINGLETON-LOCK-2026-10-05.md) · [RR-20261005-01 修复](../../bugfix/RR-20261005-01.md)

<a id="own-5"></a>
### OWN-5 活动组由一个配置文件定义，每组至多 64 个 game（C4）

**一句话**：托管 activity 协调器的工程生成 `configs/activity_groups.yaml`（`groups: [{id, game_sids}]`），协调器与 game-demo 的 game 都经 `activity.groups_file` 读它，校验只有一份 `activity.LoadGroupsFile`。

**背景**：N01/S4 O4：协调器单窗口 expected 上限 `MaxExpectedGames = 64`，候选 65～200 个能启动，同时活着超过 64 个时每个窗口被拒；组 id 写在 game 代码常量里、sweep 组写在协调器配置里，两处可以不一致；生成的 `sweep_groups: []` 让 demo 的宽限窗口没有进程兜底。

**维护者决定**：第三轮 C4 原话“game 组应该是一个配置文件，上限暂定是 64 个”（[C4 方案](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)）；不迁移已生成的工程。

**现在的行为**：组内重复、一个 sid 属于两个组、非正数或超出 int32、未知字段、组 id 空或含 `/`、成员超过 64、game 的 sid 不在任何组里，都在启动时点名文件、组、sid 报错。协调器的 `activity.groups_file` 可选：设置后启动校验，`sweep_groups` 为空时 sweep 扫文件里全部组（显式 `sweep_groups` 仍优先）。game-demo：组 id 来自文件（`gameactivity.Key` 多一个组参数），贡献榜键加组（`<prefix>:board:<组>:<窗口>`）；Dockerfile 与 shell `install.sh` 把文件拷进镜像 / release；`second-game.sh` 启动前检查 sid 在组文件里。文件默认组 id 为工程名（与原常量相同，Redis 里已有的窗口键不变）。

**兼容与迁移**：**破坏性（模板层）**：game 配置键 `activity.game_sids` 删除；旧工程的 game 代码仍读旧键、不受影响。新生成的 game-demo 需要 core ≥ v1.20.2。某环境要不同分组时挂载覆盖 `/app/configs/activity_groups.yaml` 或改 `activity.groups_file`。

**已知限制**：协调器 `OpenActivity` 不核对 expected 集合是否属于 Key 的组；没有在真实依赖上起进程演练。

**链接**：[实现 OWN-5](impl-app-own-clk-ops-tool.md#own-5) · [C4 方案](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)

<a id="own-6"></a>
### OWN-6 global `Bind` 重试幂等

**一句话**：`Bind` 的 insert-only `Create` 没建成时读回已存绑定，group 与 globalSID 都与请求一致就返回它（计 `replayed:bind`），不一致才报 `ErrConflict`，错误里带已存的 group / sid。

**背景**：RR-20261006-05（收尾 A7，来源是 A2 实施时留作观察的“global Bind 的误报”）：A2 之后 Redis 写命令不经驱动重放，写已落库、回复丢失时调用方重试同一个 `Bind`，第二次 `Create` 输给的正是自己第一次的写，被告知 `already bound`。

**维护者决定**：第十一轮收尾第 4 批。只比较 group 与 globalSID；没有用“请求 ID 记进值里”（要改持久格式，A2 ③ 已决定暂不做）。

**现在的行为**：同参数重复 `Bind` 由 `ErrConflict` 改为成功；迁移中而 globalSID 仍是请求值时也返回成功（原 `Bind` 确实成功过）；已迁移走（globalSID 变了）的迟到重试仍报冲突；`Get` 出错原样返回（仍是结果未知）。

**兼容与迁移**：行为放宽，RPC wire 不变。

**链接**：[实现 OWN-6](impl-app-own-clk-ops-tool.md#own-6) · [RR-20261006-05 修复](../../bugfix/RR-20261006-05.md)

---

<a id="clk"></a>
## CLK：业务时钟

维护者的边界（DECISIONS-PENDING 第六轮 D-L3 修订）：“两个时钟、边界写死”。**业务时钟**（带 `logic_offset`，App 统一提供）管活动窗口与协调器、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗游戏时间；**系统时钟**（真实时间）管 server 帧率、租约 / 锁、超时、重试退避、存储 TTL、消息 Ack、日志 / 指标 / WAL 时间戳。Nest / DataEngine / Sync 全部是系统时钟。v1.22.0 起补一条：业务服务内部、只与业务时间比较的退避和租约读业务钟（CLK-4）。

<a id="clk-1"></a>
### CLK-1 业务时钟与系统时钟分开（D-L3）

**一句话**：新增 `clock.Business` / `clock.NewBusiness` / `clock.Process` 与 `app.BusinessClock(registry)`（能力 `app.ModBusinessClock`）；kit 的 activity 协调器、mail、rank、session Mod 与 game-demo 业务时间改读业务钟；`env: production` 时 `time.logic_offset` 非 0 拒绝启动；glsvet 对 `game` 目录直接读 `time.Now` / `Since` / `Until` 打印提示。

**背景**：N11 O9：World 定时器与活动窗口跟不跟 `time.logic_offset` 没有定义。原推荐是“现在 (a) 保持墙钟；要用偏移测活动时再 (b)”。

**维护者决定**：第六轮 D-L3 修订版（上面的边界），约束“偏移单一配置源、只在启动时生效或只许前拨、生产启动校验强制为 0、业务过期不靠存储 TTL 判定（TTL 只兜底且更长）、glsvet 提示业务包直接 `time.Now()`”。运行期调整偏移没有采用（要协调所有进程同时前拨、成批到期要分批限流）。

**现在的行为**：业务时钟 = 真实时间 + `time.logic_offset`，偏移为 0 时 `Now()` 直接返回 `time.Now()`。`app.BusinessClock(nil)` 或没登记时退回进程级业务时钟；`timer.Scheduler`、`ai`、`actionflow` 未注入时缺省读进程级业务时钟。game-demo 的活动窗口、World 定时器每一拍、GM 关窗、怪物重生与各 handler 的 `nowUnix` 改读业务钟。glsvet `-clockhints`（缺省开）、`-businessdirs`（缺省 `game`），豁免 `//glsvet:system-clock <理由>`，只提示、不计入失败。

**兼容与迁移**：偏移为 0 时除 mail 信封 TTL（v1.21.0 多了 24h 宽限，见 CLK-4）外行为不变。生产配了非 0 偏移拒绝启动。已生成工程不迁移（`roost project sync` 取新模板）。

**链接**：[实现 CLK-1](impl-app-own-clk-ops-tool.md#clk-1) · [D-L3 方案 §1～§7](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md) · [roost-coding 时钟一段](../../agent-skills/roost-coding/SKILL.md)

<a id="clk-2"></a>
### CLK-2 match / chat 展示 / account 业务时间走业务钟，doctor 检查偏移一致（D-L3 第八轮）

**一句话**：match 票据时间、chat 展示给玩家的 `SentAtUnix`、account 账号 / 角色的创建与登录登出时间读业务钟；chat 保留期、account 会话 token 与运维记录留系统钟；`roost project doctor` 新增 `time:logic_offset`，要求同一套配置里所有服务的偏移一致。

**背景**：D-L3 第六轮把 match / chat / account 的归属列为“拿不准、交维护者”。

**维护者决定**：第八轮——match“匹配是业务逻辑：票据超时、等待放宽改走业务时钟；match 服务与 game `matchmaking.Pools` 一起换”；chat“展示给玩家的消息时间走业务时钟；保留期清理（空间回收）留系统时钟，拆成两个字段”；account“创建时间等业务用途走业务时钟”；saga 截止保留系统时钟；“`roost doctor` 检查所有服务配置的 `time.logic_offset` 一致”。

**现在的行为**：chat `Message` 新增 `SentAtUnix`（`json:"sent_at_unix,omitempty"`），旧消息读出时用 `StoredAtUnix` 兜底、不改写存量；chat / account 的 `Config.SystemNow` 由 Mod 注入 `time.Now`（nil 时沿用 `Now`）。doctor 对 dev / prod example / k8s secret example 三套配置各自比较（不写 = 0s），不一致或格式错时 FAIL 并点名服务、文件与值；三套之间不比（测试环境前拨、生产为 0 是正常的）。

**兼容与迁移**：偏移为 0 时不变。客户端展示改读 `SentAtUnix`（game-demo 的聊天推送不带时间）。

**已知限制**：仓库外的真实生产配置 doctor 看不到（外部验证 E24）。

**链接**：[实现 CLK-2](impl-app-own-clk-ops-tool.md#clk-2) · [D-L3 方案 §8](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md#8-第八轮留项实施2026-10-06)

<a id="clk-3"></a>
### CLK-3 业务时间只许前进（高水位）

**一句话**：App 在单实例锁之后、任何 Mod Init 之前读协调存储里的部署级高水位（`<singleton.key_prefix>:business_time`，不过期），按新偏移算出的业务时间低于“高水位 − 1 分钟”就拒绝启动（`app.ErrBusinessTimeMovedBack`）；运行中每 10s 推进；只在非生产检查。

**背景**：D-L3 允许偏移跨重启往回调，于是 activity / mail 为“往回调”拆了系统钟（CLK-4）。

**维护者决定**（[下一轮规划 §1](../../review/NEXT-ROUND-PLAN-2026-10-06.md) 原话）：“acivity 需要走业务时间吧，不能走系统时间。之前很多问题的时间可以往回调，现在加上强限制时间不能往回调，这样 mail 的问题也不存在了，之前一些特殊写的逻辑也不需要了，可以查一查。”高水位放在单实例锁已有的协调存储（共享 Redis），没有采用 global 服务（启动顺序依赖）与 Mongo（App 层没有连接）。

**现在的行为**：

| 情形 | 行为 |
| --- | --- |
| 生产（`env` / `app.env` / `environment` 为 prod / production） | 不检查（偏移强制为 0，生产行为一字不变） |
| 非生产、开了单实例锁 | 用锁的存储检查与推进 |
| 非生产、偏移非 0、没开锁 | 只为高水位打开一个连接；没有 opener 或没写 `singleton.key_prefix` → `app.ErrBusinessTimeGuardMissing` 拒绝启动 |
| 非生产、偏移 0、没开锁 | 不检查 |
| 读写高水位失败 | 拒绝启动（fail-closed） |
| 运行中推进失败 | 只 Warn（v1.23.0 起另计数，CLK-5），不 fail-stop |

拒绝信息点名偏移、本进程业务时间、高水位、写入者（`<偏移>|<server_type>:<sid>`）、键，并给出“至少用多大偏移、或清库重建”。容差 1 分钟（不做成配置）。

**兼容与迁移**：**测试环境破坏性**：偏移改小超过“距上次运行的真实时间 + 1 分钟”拒绝启动；要回到过去只能清库重建（连同这个键）。只删键保留数据等于跳过守卫，不要这样做。开了单实例锁的进程多一个不过期的 Redis 键、每 10s 一次 CAS。

**已知限制**：没开单实例锁、偏移为 0 的进程不检查（部署里只有这类进程时检查不到）；进程崩溃时高水位最多落后 10s（容差吸收）；只在单机 Redis 上验证。

**链接**：[实现 CLK-3](impl-app-own-clk-ops-tool.md#clk-3) · [方案与实施](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md) · [D-L3 方案 §10](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md#10-业务时间只许前进2026-10-06下一轮规划第-1-项)

<a id="clk-4"></a>
### CLK-4 activity / mail 回到业务钟

**一句话**：v1.21.0 为“偏移往回调”把 activity 派发退避 / 进度凭证有效期、mail 领取租约拆到系统钟，并给 mail 信封 TTL 加了可配置宽限；v1.22.0 业务时间只许前进之后全部合并回业务钟，删除对应字段。

**背景与时间线**：

| 版本 | 提交 | 变化 |
| --- | --- | --- |
| v1.21.0 | `b9fc5342` | mail 领取租约改系统钟（`mail.Config.SystemNow`），信封 Redis TTL = 业务剩余时长 + `mail.RedisConfig.StorageGrace`（缺省 24h） |
| v1.21.0 | `5a3c4a60` | 发版前审查认为 activity 派发 `NextAttemptAtUnix` 与凭证 `ExpiresAtUnix` 违反“退避属系统钟”，新增 `activity.Config.SystemNow`（测试环境往回拨 D，欠下的派发要多挂 D） |
| v1.22.0 | `3e77beb9` | 回调被 CLK-3 在启动时拒绝，以上拆分只为回调，合并回业务钟；`StorageGrace` 字段删除，宽限固定 `mail.EnvelopeStorageGrace = 24h` |

**维护者决定**：同 CLK-3 原话（“这样 mail 的问题也不存在了，之前一些特殊写的逻辑也不需要了”）。保留系统钟的：chat `StoredAtUnix` 与 `Prune` 保留期（空间回收按真实年龄）、account 会话 token 与运维记录（安全有效期与审计）。“存储 TTL 比业务过期长”是独立原则（过期后领取报 `ErrExpired` 而不是 `ErrMailMissing`），所以宽限保留、只是不可配。

**兼容与迁移**：**破坏性（只影响手工装配）**：删除 `activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`，`mail.DefaultEnvelopeStorageGrace` 改名 `EnvelopeStorageGrace`（仍是 24h）；删掉那一行即可。持久格式不变；偏移为 0 时没有可观察差异。偏移非 0 的测试环境里旧代码按系统钟写的时间戳只会更早到期（重试提前）。`ReopenDispatch` 写业务钟当前时间（仍“立即可取”）。

**链接**：[实现 CLK-4](impl-app-own-clk-ops-tool.md#clk-4) · [业务时间只许前进 §4](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md) · [发版前审查收尾 §1](../../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)

<a id="clk-5"></a>
### CLK-5 高水位推进失败计数

**一句话**：`advanceLoop` 每次推进失败计 `app.business_time.advance_failed.total`（导出名 `app_business_time_advance_failed_total`，本 App 的指标注册表），仍只 Warn、不 fail-stop。

**背景**：业务时间只许前进方案的“未完成 / 风险”：运行中推进失败只记 Warn，没有指标。

**维护者决定**：第十二轮“低优先：`:lease:*` 旧键写迁移说明；业务时间高水位推进失败加计数；其余保持”。

**兼容与迁移**：新指标，无破坏。

**链接**：[实现 CLK-5](impl-app-own-clk-ops-tool.md#clk-5) · [第十二轮 kit 批 §8](../../feature/DECISIONS-R12-KIT-2026-10-06.md#8-业务时间高水位推进失败计数)

<a id="clk-6"></a>
### CLK-6 timer 同期限顺序与 priority、未注册类型的节点（D-L1 / D-L2）

**一句话**：`timer` 堆按 (End, `Node.Priority`, ID) 排序，priority 数值小的先触发、缺省 0、同 priority 按最初登记顺序；到期节点的类型没有 handler 时照旧删除，另打 Warn 并计 `timer.unhandled_dropped_total{kind}`；新增 `Scheduler.ReportUnhandledTypes()` 供加载时每种类型告警一次。

**背景**：N11 O6：同期限顺序由堆形状决定（登记 1..6 按 1、6、5、4、3、2 触发），World `TimerComponent` 每次从 DAO 的 map 重建调度器（A1），顺序每次都可能不同。N11 O7：下线一种定时器类型后，存量节点到期即无声消失。

**维护者决定**：第六轮 D-L1“同期限定时器缺省按登记顺序（ID 作第二键）；新增可选 `priority` 字段用于排序：先比期限，再比 priority，同 priority 按登记顺序”；D-L2“按推荐：未注册类型的到期节点删除时 Warn + 计数，加载时对无 handler 的存量类型告警一次”。没有采用“保留不删、跳过触发”（留在堆顶挡住之后的节点）。

**现在的行为**：新增 `Scheduler.NewTimerWithPriority`；闭包定时器不加 priority。game-demo 模板 `TimerNode` 加 `priority`（旧节点按 0 读回，不迁移），World `OnInitFinish` 调 `ReportUnhandledTypes`。宿主事务回滚后重试同一次 Tick 会再计一次。

**兼容与迁移**：同期限顺序从“未定义”变为确定（不可能有人依赖旧顺序）。已生成工程 `roost project sync` 或手工同步三个模板后生效；不同步也能编译。

**链接**：[实现 CLK-6](impl-app-own-clk-ops-tool.md#clk-6) · [D-L1 / D-L2 方案](../../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)

---

<a id="ops"></a>
## OPS：运维与可观测

<a id="ops-1"></a>
### OPS-1 服务指标默认落到 metrics 注册表（C6）

**一句话**：新增 `servicemetrics.NewMetricsReporter(service)`，把服务的 Accepted / Refused / Replayed / Dropped / Conflict / Depth 写成六个固定名指标（`service.{accepted,refused,replayed,dropped,conflict}.total` 与 `service.depth`），经 ops `/metrics` 导出；生成工程的 `Metrics()` 默认返回它。

**背景**：N06 观察 1 / N12 O1：kit 各服务都接受 `servicemetrics.Reporter`，生成工程一律返回 nil，事件在生产里无处落地；match / rank 的 Depth 名字里嵌了队列 / 看板标识（每个对象一个新指标名）；session 把一次清扫的条数当 gauge 报。

**维护者决定**：第二轮 C6“默认 metrics adapter：做”，“提供默认 Prometheus adapter（名字带 ID 的指标要拆成固定名 + 标签）”。

**现在的行为**：服务、操作、原因、对象都在标签里；队列 key / 看板 ID 改为 `key` 标签（可选接口 `servicemetrics.KeyedReporter` / `Sink.DepthOf`，项目自写的 Reporter 不实现它仍收到旧形状 `queue.<key>`）；session 清扫数改为计数 `Dropped("run.swept", n)`。每次上报解析当时的默认注册表（先建 Reporter、后起 App 也落在 App 导出的那个）。关闭：`service_metrics.enabled: false`（严格布尔）或 `Metrics()` 返回 nil。

**兼容与迁移**：**破坏性**：`Recorder` 事件名 `depth:queue.<key>` → `depth:queue{<key>}`、`depth:board{<id>}`，`depth:session.swept` → `dropped:run.swept`；依赖旧名字的看板、告警、项目测试要调整。新生成工程需要 core ≥ v1.20.2；已生成工程不迁移（collaborators 只创建一次）。

**已知限制**：`key` 由调用方传入，框架不能保证有界——队列分区与看板 ID 必须是有限枚举（写进 OBSERVABILITY 与 USER_GUIDE）。

**链接**：[实现 OPS-1](impl-app-own-clk-ops-tool.md#ops-1) · [C6 方案](../../feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)

<a id="ops-2"></a>
### OPS-2 metrics 按标签删除；loadtest 运行序列随运行记录删除

**一句话**：新增 `metrics.Registry.DeleteSeries(name, labels)` / 包级 `metrics.DeleteSeries`（按标签删除并归还每指标名额，空标签不删任何东西）与 `SeriesCount()`；loadtest Manager 在运行被挤出历史（`HistoryLimit`，缺省 20）时删掉带它 `run` 标签的全部序列。

**背景**：N12 O2 / O3：loadtest 给 `robot.runner.*` 加 `run` 标签，每次运行一组新序列、永不删除，控制面约一千次运行后触到每指标序列上限（2048），新运行的耗时直方图被丢弃、分位数阈值无从判定。C6 当时把它列为“未做”。

**维护者决定**：第十二轮“metrics 按标签删除：Registry 加按标签删除，对象拥有者销毁时删”。

**现在的行为**：运行结束时不立即删（还在历史里的运行保持可抓取）；同名 RunID 仍在历史里或正在跑时不删；删除后同名同标签再写入是从零开始的新序列。

**兼容与迁移**：新 API。长期运行的控制面序列数不再随运行次数增长。

**已知限制**：O3 的 `nest.dispatch.*{dispatcher}` 与 `bus_rpc_pending{method}` 未改（前者属核心线，后者有 2048 上限）。

**链接**：[实现 OPS-2](impl-app-own-clk-ops-tool.md#ops-2) · [第十二轮 kit 批 §2](../../feature/DECISIONS-R12-KIT-2026-10-06.md#2-metrics-按标签删除)

<a id="ops-3"></a>
### OPS-3 Ops 端点：同步 bind 与 admin 命令期限

**一句话**：`OpsMod.Start` 在 Start 里同步 `net.Listen`，端口被占用返回 `ops: listen on ops.addr …` 按启动失败收尾（NC-230）；`/admin/execute` 交给命令的 ctx 带 `ops.admin_timeout`（缺省 10s）期限，到期回 504 并写明结果未知，HTTP 写超时 = max(15s, admin_timeout + 5s)。

**背景**：NC-230：bind 在后台 goroutine 里失败只记日志，进程在没有 `/healthz`、`/readyz` 的情况下继续跑，同机部署的健康检查可能探到占着端口的另一个进程。N02 O1：admin 命令没有期限、写超时固定 15s，超过 15s 的命令执行完了回复写不出去，客户端只看到 `EOF`，重试可能重复执行。

**维护者决定**：第四轮“补齐单元内留项”（N01 含 N02 O1、N14 O3 / O4），NC-230～234 与 `ops.admin_timeout` 一并实施。不配合 ctx 的命令不放进另一个 goroutine（那样 Ops 的停止等不到它，违反三步停机）。

**现在的行为**：日志打印实际监听地址（`ops.addr` 写端口 0 时是系统分配的端口）。`ops.admin_timeout` 写了就必须为正，登记进严格读取的时长键。不配合 ctx 的命令仍可能以传输错误结束（同样是结果未知）。

**兼容与迁移**：**行为收紧**：同机多实例必须各配 `ops.addr`（k8s 每 Pod 独立网络命名空间不受影响）；原来超过 10s 的配合 ctx 的命令（大批量 DLQ requeue 等）需要调大 `ops.admin_timeout`。APP-1 的 P2 场景里，P1 卡住仍占着 ops 端口时，P2 现在在 ops Start（DataEngine 之前）就失败退出。

**已知限制**：真实 shell / systemd 部署下端口冲突的完整进程链未验证。

**链接**：[实现 OPS-3](impl-app-own-clk-ops-tool.md#ops-3) · [NC-230 修复](../../bugfix/RR-20261005-NC-230.md) · [Ops admin 期限方案](../../feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)

<a id="ops-4"></a>
### OPS-4 Ops 的 `Authorization` 必须带 `Bearer `

**一句话**：`/admin/*` 的 `Authorization` 头只认 `Bearer <token>`（scheme 大小写不敏感，RFC 7235），不带 scheme 或别的 scheme 一律 401；`X-Admin-Token: <token>` 不变。

**背景**：N01b 观察 O-P1：`bearerToken` 没有前缀时原样返回整个头，`Authorization: <token>` 也能通过。不构成绕过（仍要知道 token），但别的 scheme（Basic 等）不能被当成 admin token。

**维护者决定**：第十二轮“Ops Bearer：收紧为必须带 `Bearer `”。

**兼容与迁移**：**行为收紧**：用裸 token 调 admin 的脚本改成 `Authorization: Bearer <token>` 或 `X-Admin-Token`。仓内脚本、生成工程与文档没有裸 token 的用法。

**链接**：[实现 OPS-4](impl-app-own-clk-ops-tool.md#ops-4) · [第十二轮 kit 批 §4](../../feature/DECISIONS-R12-KIT-2026-10-06.md#4-ops-必须带-bearer)

<a id="ops-5"></a>
### OPS-5 CAS 冲突率在 versionstore 统一计数

**一句话**：`versionstore.RedisStore.Update` 每次 compare-and-set 计 `versionstore.cas.total{store,result=applied|lost}`，预算用尽计 `versionstore.conflict.total{store}`（`store` 是键前缀）；rank 自己的 CAS 循环经 `CountCompareAndSet` / `CountConflict` 计到同一处；chat / rank 不再自报。

**背景**：N06 观察 2：只有 chat 对 `versionstore.ErrConflict` 计数，其他服务只在 insert-only 碰撞处报 `Conflict`，account / activity 一处都没有。

**维护者决定**：第十二轮“CAS 冲突率口径：versionstore 层统一计数”。

**现在的行为**：冲突率 = lost / (applied + lost)。`servicemetrics.Conflict` 只留给业务冲突（session `enter`、mail `send` 撞号、global `bind` 已绑到别处、directory `release` 身份变化、platform `deliver` 竞态），`kit/service/README.md` 写明分工。account / activity 等直接用 `RedisStore` 的服务自动获得计数。

**兼容与迁移**：**指标口径变化**：chat 的 `conflict:append` / `conflict:prune`、rank 的 `conflict:submit` 不再上报；按 `service_conflict_total{op=…}` 做的看板 / 告警改查 `versionstore_conflict_total{store=…}` 与 `versionstore_cas_total`。

**链接**：[实现 OPS-5](impl-app-own-clk-ops-tool.md#ops-5) · [第十二轮 kit 批 §5](../../feature/DECISIONS-R12-KIT-2026-10-06.md#5-cas-冲突率统一计数)

<a id="ops-6"></a>
### OPS-6 game-demo 仪表盘补两个面板

**一句话**：“事件链与配置”行新增“配置撤回（按触发方式，5 分钟内次数）”面板（`configdata_rollback_total{trigger}`）；新增“场景复制会话”行与“复制会话重开放弃（按原因，5 分钟内次数）”面板（`scene_session_reopen_failed_total{reason}`）。

**背景**：两个指标都已在 game-demo 可观测性 README 里列出，仪表盘没有查询它们（收尾第 2 批 A17；A17 的顺带观察引出配置撤回面板）。

**维护者决定**：第十一轮收尾第 2 批 A17；第十二轮实施要求“game-demo 仪表盘补 `configdata_rollback_total{trigger}` 面板”。

**兼容与迁移**：只影响新生成工程的仪表盘模板；生成器的 Core 下限不变。

**链接**：[实现 OPS-6](impl-app-own-clk-ops-tool.md#ops-6) · [收尾第 2 批 A17](../../bugfix/CLOSING-BATCH-2-2026-10-06.md) · [第十二轮 kit 批 §9](../../feature/DECISIONS-R12-KIT-2026-10-06.md#9-configdata_rollback_total-面板)

<a id="ops-7"></a>
### OPS-7 robot Stage 序号只增不回收

**一句话**：staged 运行先缩后扩时，新机器人的序号接着已发出的最大序号往后排，不再复用刚停掉的机器人的序号与 PlayerID。

**背景**：RR-20261006-09（N12 O9）：`launchUpTo` 从 `launched + 1` 编号、`shrinkTo` 把 `launched` 减回去，被停的机器人还在收尾（关会话、登出）时新机器人已以同一序号、同一 PlayerID 登录，压测结果失真。

**维护者决定**：第十二轮“robot Stage 序号：只增不回收”。没有采用“缩容时等收尾结束再把序号放回池里”（要在调度循环里阻塞或另建回收队列，换来的只是序号连续）。

**兼容与迁移**：有过缩扩的 staged 运行会用到超过 `Count` 的序号（`Count` 加每次扩回的数量）；自定义的、按 `Count` 预分配身份表的 `IdentityProvider` 要扩大。仓内 game-demo `cmd/loadtest` 没有上限，不受影响；pool / arrival-rate 执行器与没有 Stages 的 looping 不受影响。

**链接**：[实现 OPS-7](impl-app-own-clk-ops-tool.md#ops-7) · [RR-20261006-09 修复](../../bugfix/RR-20261006-09.md)

---

<a id="tool"></a>
## TOOL：发版工具与门禁

<a id="tool-1"></a>
### TOOL-1 pretag：失败可读、origin 不可达即失败

**一句话**：`scripts/pretag.sh` 的 `go test ./...` 失败时把 `--- FAIL` / `FAIL` / `panic:` 行打到 stderr 并留下完整输出路径（v1.20.0）；检查远端同名 tag 时区分“远端没有”（退出码 2，继续）与“无法核对远端”（其他，失败）（v1.20.2，NC-205）。

**背景**：v1.20.0 发版时一次偶发失败，`go test` 输出整段丢进 `/dev/null`，无从判断是哪条。NC-205（N15）：origin 不可达时 pretag 跳过远端同名 tag 检查、报 `ready to tag`。

**维护者决定**：失败打印是 v1.20.0 发版时的直接跟进（`999dc672` 提交说明）；NC-205 随 N15 收口按推荐修（[NC-200 记录](../../bugfix/RR-20261005-NC-200.md)同批）。pretag 是发版前唯一的本地全量测试关口（roost-coding“不等待或轮询 GitHub CI”）。

**兼容与迁移**：origin 指向错误或网络不通时 pretag 失败（`cannot check origin for an existing <version> tag (git ls-remote exit <n>); fix the remote or the network and rerun`）。

**链接**：[实现 TOOL-1](impl-app-own-clk-ops-tool.md#tool-1) · [NC-205 修复](../../bugfix/RR-20261005-NC-205.md)

<a id="tool-2"></a>
### TOOL-2 full 场景 add 序列只定义一处，本地检查不再吞失败

**一句话**：framework-compat full 场景的 add 序列收拢到 `codegen/scripts/full-scenario-adds.sh`（`set -euo pipefail`），CI workflow 与本地 `codegen/scripts/source-head-check.sh` 各调用一次；根包门禁 `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed` 钉住。

**背景**：收尾第 2 批 A11：`source-head-check.sh` 用 `(...) || true` 包住 add，任何一步失败都被吞掉；它只 add 了 access / transport / skill / saga 四步，而 CI 的 source-head lane 还有 component、dao、handler、protocol、endpoint、rpc 与 `project sync`，本地“full OK”不代表 CI 那条路能过。

**维护者决定**：第十一轮收尾第 2 批。

**现在的行为**：`uses_rpcs` 的清单编辑由 GNU 专用的 `sed -i` 改为 awk + mv（本地 macOS 可用），改完 grep 确认，找不到 `    gate:` 条目就失败。门禁要求两个调用方都不自己 `roost add`、恰好调用共享脚本一次且不带 `||`、没有 `) || true`；共享脚本带 `set -euo pipefail` 且没有 `|| true`。

**链接**：[实现 TOOL-2](impl-app-own-clk-ops-tool.md#tool-2) · [收尾第 2 批 A11](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)

<a id="tool-3"></a>
### TOOL-3 根包门禁：跟踪文件不得带合并冲突标记

**一句话**：`TestNoMergeConflictMarkersInTrackedFiles` 用 `git grep` 扫跟踪文件里的 `<<<<<<<` / `>>>>>>>` 行（单独的 `=======` 是 Markdown setext 标题线，只在同文件已有前两种时列出），并自检能抓到 `0aa2e1b9` 那次进 main 的冲突。

**背景**：`80902948` rebase 成 `0aa2e1b9` 时把一段未解决的冲突带进了 main 的 `REMAINING-REVIEW-HANDOFF`，build / vet / 测试全绿；`2c1c7be7` 顺手删掉（revleft 小防护 A）。

**维护者决定**：第四轮“补齐单元内留项”中的 revleft 小防护。

**兼容与迁移**：`artifacts/` 排除；非 git 工作区（模块 zip）跳过。

**链接**：[实现 TOOL-3](impl-app-own-clk-ops-tool.md#tool-3) · [revleft 记录](../../review/REVIEW-2026-10-06-revleft.md)

<a id="tool-4"></a>
### TOOL-4 根包门禁：示例要实跑，不能只编译

**一句话**：`TestExamplesRun` 穷尽发现所有 `examples` 路径段下的 `main` 包，以 `GOWORK=off` 在各自模块里编译（5 分钟上限）并运行（1 分钟上限），要求退出码 0；新示例必须登记进 `exampleRuns`，只有需要外部依赖的才允许写明理由跳过。

**背景**：A1 之后 `skill/examples/statusbridge` 一运行就 panic（战斗组件改动在事务外），`examples/` 模块的 go.sum 也早已缺 robot / nettransport 新依赖的条目、`GOWORK=off go run ./robotdemo` 编译不过；两个示例目录都是独立模块，根模块的 `go build ./...` 不包含它们，CI 也没有任何一步构建它们。

**维护者决定**：v1.23.0 发版前补充验证；规则写进 [roost-coding“验证与性能纪律”](../../agent-skills/roost-coding/SKILL.md)“示例要实跑，不能只编译”。

**现在的行为**：现有 6 个示例（`examples/{configgen,lubanreal,robotdemo}`、`skill/examples/{combat,fireball,statusbridge}`）全部必须运行，没有跳过项；`kit/service/examples/split` 是库包，不在其列。缓存热时约 2.5～4s。离线且模块缓存缺失时在编译阶段失败（有意，不静默放行）。

**兼容与迁移**：改了示例用到的 API（skill、robot、configdata、事务 / DAO 形状等）或新增示例时根包测试必须通过；示例模块依赖变化时在该模块 `GOWORK=off go mod tidy`。

**链接**：[实现 TOOL-4](impl-app-own-clk-ops-tool.md#tool-4) · [发版前补充验证 §2](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)

<a id="tool-5"></a>
### TOOL-5 根包门禁：文档相对链接必须能解析

**一句话**：`TestTrackedMarkdownRelativeLinksResolve` 扫全部跟踪的（以及未忽略、即将提交的）`*.md`，每个相对链接（去掉锚点与查询）必须指向跟踪的文件或目录；跳过外部 URL、纯锚点、代码块与行内代码；不检查锚点；显式豁免目标在 `artifacts/` 下与源文件在 `docs/history/` 下。

**背景**：RR-20261006-10 同批：合仓后 `skill/README.md` 52 个相对链接仍按旧仓布局写、全部落空；扩大范围后首跑另有 6 处旧路径。根包之前只有冲突标记门禁，不读链接。

**维护者决定**：随 RR-20261006-10 实施；58 处全部修正，没有为它们加豁免。

**兼容与迁移**：写文档时链接到不存在的文件会让根包测试失败。

**链接**：[实现 TOOL-5](impl-app-own-clk-ops-tool.md#tool-5) · [RR-20261006-10 修复末节](../../bugfix/RR-20261006-10.md)

<a id="tool-6"></a>
### TOOL-6 `scripts/mirror-local.sh`：本机私有依赖进程上的故障与性能

**一句话**：自起私有依赖进程（NATS 三节点 JetStream、Mongo 三节点副本集、Redis 单机 + 副本与 3 主 3 从 Cluster、toxiproxy），跑生成工程两进程故障场景（`test`）、remoteentity 私有环境集成用例（`test-core`，本版）与 v1.20.2 对照基准（`bench`），结束清理；默认不在 CI 跑。

**背景**：维护者原话（[下一轮规划 §3](../../review/NEXT-ROUND-PLAN-2026-10-06.md)）：“mirror 的第 6 项看下能否在本机用别的方式替代。”

**现在的行为**：根目录缺省 `$TMPDIR/roost-mirror-local`，端口偏移缺省 20000；拒绝 `~/.roost-it` 与偏移 0 / 1000（共享环境与历史默认环境）；启动前清掉继承的 `ROOST_DATAENGINE_IT_*`，不读共享 env.sh；私有根不取 A5 验收锁。本版扩展：`test-core`（`ROOST_MIRROR_LOCAL_CORE_RUN` 覆盖缺省 `^TestMirrorLocal`）、`fault redis-cluster-stop-replica` / `redis-cluster-cont`、生成工程用例 `ROOST_MIRROR_LOCAL_ONLY` 支持逗号分隔（如 `S1,S7`）、Cluster 就绪判定加“每个主节点有 online 副本”。

**已知限制**：替代不了 Linux 内核网络、跨主机真实分区、长时间容量（外部验证 E01 / E02 / E15）。

**链接**：[实现 TOOL-6](impl-app-own-clk-ops-tool.md#tool-6) · [Mirror 第 6 步本机替代](../../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)

<a id="tool-7"></a>
### TOOL-7 故障矩阵与共享隔离环境的验收锁

**一句话**：`scripts/test-remote-matrix.sh` 对退出 0 但日志含 `no tests to run` / `[no test files]` 的格记 `FAIL(no tests ran)`（NC-207）；隔离环境的全局运维命令（up / down / heal / reset / fault、故障矩阵）运行期间持有 `remote-acceptance.lock`（A5）；v1.23.0 发版前在 `d6a677e0` 上预跑 21 格全部 PASS。

**背景**：NC-207（N15）：`-run` 正则一个用例都没匹配到时矩阵格照样 PASS。A5（N03 1s 事故、NC-201～203、NC-207 / 208）：隔离环境是独占还是共享。

**维护者决定**：A5“按推荐：② 共享，全局运维命令保留锁”——并行会话各跑 `-run` 选定的用例，故障一律自建代理 / 进程，heal 不全局 reset。规则源 [kit/scripts/integration/README.md](../../../kit/scripts/integration/README.md)。

**现在的行为**：矩阵与 `scripts/perf/remote.sh` 取得锁后导出 `ROOST_REMOTE_ACCEPTANCE_LOCK_HELD`，自己调起的 heal / remote-fault 照常执行；锁被别人持有时 `dataengine-env.sh` 的改环境命令与 `scripts/remote-fault.sh` 以 2 拒绝。预跑结果目录在主检出（被忽略）的 `artifacts/perf/remote/matrix-relprep-20261006/`。

**已知限制**：预跑在 `d6a677e0` 上，不是最终发版提交；“发版仍按惯例在最终 HEAD 再跑一次”（记录原文）。

**链接**：[实现 TOOL-7](impl-app-own-clk-ops-tool.md#tool-7) · [NC-207 修复](../../bugfix/RR-20261005-NC-207.md) · [NC-203 修复与 A5 补修](../../bugfix/RR-20261005-NC-203.md) · [发版前补充验证 §5](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)

---

## 外部验证清单（本部分相关）

全集见 [EXTERNAL-VERIFICATION-2026-10-06](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)，这些项不阻塞发版。与本部分直接相关的：

| # | 项 | 涉及条目 |
| --- | --- | --- |
| E08 | 驱动、单实例锁、Lua 布局在多机 Redis Cluster 切主下 | APP-1、APP-5、CLK-3 |
| E10 | 异步复制丢写与切主：锁 | APP-1 |
| E13 | 多主机强杀：双实例、旧回调 | APP-1、APP-4、OWN-2 |
| E21 | 真实 systemd 的 shell 部署与回滚（`HEALTH_ATTEMPTS`） | APP-2 |
| E22 | k8s 滚动停机与部署物（`startupProbe`、停机预算） | APP-2、APP-8 |
| E24 | 仓库外生产配置的 doctor 检查（含 `time:logic_offset`） | CLK-1、CLK-2 |
| E01 / E02 / E15 | Linux 内核网络、跨主机分区、长时间容量 | TOOL-6 |

另外两项本机未做、记录里明确写了的：Redis Cluster 下单实例锁的真实进程演练（App 锁方案 §13 第 5 笔“未验证”）；本版最终 HEAD 上的故障矩阵（TOOL-7）。

## 仍待决定的事项与 WANTED

- **维护者决定项**：本部分没有未决项（DECISIONS-PENDING 文首“未决事项：零”）。留到下个大版本的、与本部分相关的只有 A3 ②（排空下沉到 `ISyncBus` 带 ctx 的退订，APP-6）。
- **WANTED**：W-2026-10-06-02（驱动重复 Close 口径）已转 RR-20261006-10 并修复（APP-7）；W-2026-10-06-01（nest 无 Guard 作用域分支）不属于本部分。
- **记录里的观察，维持现状**（不是待决项）：单实例锁启动获取丢回复后下次多等一个 ttl（App 锁方案 §13 观察 6）；`PhaseServiceStopped` 钩子慢会吃掉 Release 预算（观察 7）；OWN-2 的三条驻留表观察；OWN-5 协调器不核对 expected 属于组。
