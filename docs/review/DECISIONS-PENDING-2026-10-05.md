# 待维护者决定的事项（2026-10-05 汇总）

来源：v1.20.0 / v1.20.1 整体验证、U-0280、全部 15 个非核心单元的第一轮 review（N01～N15）以及各轮“方向判断”。
每项给出：来由（编号）、选项、推荐。**已修的缺陷不在此列**；这里只列需要维护者拍板的方向、契约与语义。
排序按影响面：A 是跨模块的框架契约（反复出缺陷的根因），B 是单模块方向，C 是业务语义与去留。

## A. 框架契约（反复出缺陷的根因）

| # | 事项 | 来由 | 选项 | 推荐 |
| --- | --- | --- | --- | --- |
| A1 | 组件内存不随 Nest 回滚 / 加载重建 | NC-61、NC-65、NC-140、N09 O1 | ① 规范 + 模板写明“组件内存是 DAO 的缓存，改前登记 undo”，模板给骨架；属性容器加 `Checkpoint/Restore` 与统一 `rebuild()`（加载、热更、回滚都调用）② Nest 回滚后调用组件重建钩子（核心线改动） | 先做 ①，② 视后续是否再出问题 |
| A2 | 驱动默认重试 / 超时语义与“结果未知交给调用方”不一致 | NC-21、NC-52、NC-100、NC-101、NC-160 | ① 在 `redis/driver`、`mongo/driver` 写驱动行为契约表（草稿见 NC-101 复审报告）② RedisMod 默认不重放写命令，只对“确定未发出”的错误重试 ③ versionstore 写入带一次性令牌（改持久格式） | ① + ②；③ 暂不做 |
| A3 | 停机语义各写一遍，同一不变量被打破 11 次 | RR-20261004-07/08、NC-04/09/83/90、NC-170～174 | ① 抽共用“准入 + 在途计数 + idle”小类型（现有 3 份重复）+ 共用停机契约测试骨架 ② 排空下沉到传输层（`ISyncBus` 加带 ctx 的退订，有兼容期）③ glsvet 加复审提示 | ① 现在做；② 下个大版本；③ 只做提示不做门禁 |
| A4 | 配置没有 schema，宽松 getter + 事后清单校验 | RR-20260926-12、NC-190、NC-192、N02 O2 | ① 每个 Mod 声明配置键与类型，校验 / 生成器 / doctor 共用 ② 逐个 Mod 改用 `app.ConfigBool` / `ConfigDuration`（还剩约 80 处宽松读取） | 先 ②，① 作为后续重构 |
| A5 | 隔离测试环境：独占还是共享 | N03 1s 事故、NC-201～203、NC-207/208 | ① 承认独占：所有改环境的入口持锁，根包加门禁 ② 承认共享：故障用例一律自建代理 / 进程，heal 不全局 reset | ②（本轮测试已按 ② 改完），全局运维命令保留锁 |

## B. 单模块方向

| # | 模块 | 来由 | 选项 | 推荐 |
| --- | --- | --- | --- | --- |
| B1 | saga 最终性 | U-0280 复审 | 协调器接收 completion 时核对代际 / 尝试（改协调器状态机）；Mongo 步骤是否也由框架兑现“同一操作最多一次” | 代际过滤做；Mongo 步骤另写方案 |
| B2 | remoteentity 快照缓存水位 | RR-20260913-01 链、NC-35/36、NC-130 | (a) 共享 L2 为水位权威，L1 只是有界副本 (b) 订阅代际 + 首载缓冲（Mirror 方案，依赖 JetStream）；两者都要把 Cached 读最大陈旧时间写成配置与契约 | (a) |
| B3 | skill 编译器与 Runtime / Host 各自维护可执行集合 | NC-110～117、NC-150～154、NC-210～216 | ① lower 查找失败一律 fail-fast ② 事件派发表单一来源 ③ Host 取值约束做成随环境下发的能力表（改环境格式与 authority digest）④ NC-151 / NC-213 走方向 A（实现 phase 计时、recast、chain repeat、stack_policy）还是保持方向 B（编译期拒绝） | ①② 做；③ 下个大版本；④ 保持 B 直到有业务需要 |
| B4 | skill Runtime 状态不在 Nest 事务里；buff 不进伤害 | N09 O1、O2 | 文档约束 / Runtime 参与回滚；属性投影放组件还是交业务 | 先写文档约束；投影交业务（组件给入口） |
| B5 | bus / etcd 选举 | N03 方向判断 | bus：统一准入 + 单一停机状态机（并入 A3）；etcd 选举无生产调用方：弃用或移出 core | etcd 选举标弃用，下个大版本移除 |
| B6 | CLI 信号 | RR-20261004-12/13、cb11be90、NC-70 | CLI 入口统一接管信号 → ctx 取消 → 正常回滚后再重抛 | 做 |
| B7 | actionflow 回调重入 | NC-121/122、U-0100 | (a) 回调期间拒绝重入 (b) 回调里的变更进延后队列 (c) ai / actionflow 移出 core | (c)（仓内无生产使用方），或 (b) |
| B8 | 生成的 TCP 接入层 | 约 8 个 RR | 已把“三步停机”写进 roost-coding；是否再抽共用件 | 并入 A3 |
| B9 | activity 窗口条目校验第 4 次出问题；account 建角判定只在部分入口生效 | NC-51、NC-42 链；RR-19/06 链 | 读窗口条目统一入口 + 持有方专用修复入口；建角写成“名额状态 × 名字状态 → 动作”判定表 | 都做 |
| B10 | 配置规则在哪一层强制 / 运行时 required | NC-75、NC-64 | A：新增 configdata API（需新版 core）B：生成的校验器重读原始 JSON（有替换窗口）；tablegen 与 cfggen 规则统一到一层 | A |

## C. 业务语义与去留

| # | 事项 | 来由 | 推荐 |
| --- | --- | --- | --- |
| C1 | 生产校验要求 9 个无人读取的开关（`env: production`） | NC-192 | 方案 1：删掉无效要求并写明校验范围；之后接入限流 / 鉴权时再加回 |
| C2 | 配置热更 AfterApply 失败时新快照已对外可见；回滚前准入的请求读被回滚的代 | N07 C-O1、C-O9 | 保持现状并写进契约（无真实触发路径） |
| C3 | event 模块零接线 | N07 | 移除或标实验；不再深审 |
| C4 | 活动组 game 服上限（协调器超过 64 个会拒绝每个窗口） | N01/S4 O4 | 定 64 并在启动时校验候选数 |
| C5 | 停机中的进程算不算“活着”（`Live`） | §7.2 差异 3 | 停机开始时把键值改成“停机中”，`Live` 不计入 |
| C6 | 服务指标默认不落地（各服务 Reporter 都是 nil，仓内无生产 adapter） | N06 O1、N12 | 提供默认 Prometheus adapter（名字带 ID 的指标要拆成固定名 + 标签） |
| C7 | 遍历回调语义 | NC-180/181/182/185 | 定为仓库级契约：“Range 回调里可以读写同一容器，返回 false 立即停止” |
| C8 | 零调用方 API 去留（container / goroutine / safemap 的一批、`index` 包、ai / actionflow） | N11、N13 | 标 Deprecated，下个大版本移除 |
| C9 | 测试开关 `publishedDataEngineGeneratorDependencies = false` 已过时 | N08 O1 | 打开会让默认测试依赖网络；改成只在 CI 的网络 lane 打开 |
| C10 | N10 留下的临时 worktree `wt-revn10`（收尾被权限规则拦下） | N10 | 维护者确认后删除 |

## 发布状态

v1.20.1（tag → `be7407ab`）之后 main 上又有 N05、N09 第三 / 四批、N12、N13（含复审补修）、N14、N15、六处同形停机（NC-170～174）、NC-208 补修等修复，均未发版。

## 维护者决定（2026-10-05，第二轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| A1 | **不采用推荐**：维护者要求“回滚都使用 DAO 的实现方式，这样回滚都可以统一”——组件的可回滚状态一律进 DAO（必要时为非持久字段），由 Nest 的 DAO 回滚统一兜住，不再让组件各自登记 undo / 重建 | 已实施（5407f127，[方案与实施](../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）；skill Runtime 状态进 DAO 列为后续（方案 §4.4） |
| A2～A5 | 按推荐 | A2、A3 见下行；A4、A5 **已实施（`3e3350d5`）**，见下两行 |
| A4 | 按推荐：先 ②（逐个改严格读取），① schema 作为后续重构 | **已实施（`3e3350d5`）**：新增 `app.ConfigInt` / `ConfigInt64` / `ConfigReader`；kit 各 Mod 与 app 改严格读取；`ValidateServiceConfig` 按 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`（16 / 95 / 77 个键，含 syncbus 三段与 `<service>.call_timeout`）检查；守卫测试扫描源码。生成工程三份配置在严格校验下全部通过（game-demo 与 CI full 场景）。**未做**：~~kit/redis 三个整数读取（留给 A2 之后，启动校验已兜住）~~ 已改为严格读取并删掉守卫放行（`5df60765`，分支 `c4c6`）；生成的 player TCP / RPC 客户端代码改严格读取（等生成器 Core 下限升到 v1.20.2）；① schema。[方案](../feature/REFACTOR-2026-10-05-strict-config-reads.md) |
| A5 | 按推荐：② 共享，全局运维命令保留锁 | **已实施（`3e3350d5`）**：共享使用规则写进 `kit/scripts/integration/README.md`、roost-coding、roost-bugfix（SKILL 与 lessons）；核对发现 NC-203 之后全局命令只检查锁、不持有，failover 用例与带故障的生成工程验收整段不持锁，按先红后绿补修（NC-203 复核补修）。[记录](../bugfix/RR-20261005-NC-203.md#复核后的补修2026-10-05维护者决定-a5) |
| C1 | 随 A4 按方案 1 | **已实施（`3e3350d5`）**：生产校验删去九组无读取方的要求，USER_GUIDE §10 写明 `env: production` 校验范围；生成的生产示例 / Secret 示例打开生产模式可以启动。[记录](../bugfix/RR-20261005-NC-192.md) |
| A2 | 按推荐：① 驱动行为契约表 ② RedisMod 默认不重放写命令；③ 暂不做 | **已实施（`cf5721c9`）**：Redis 写命令、含写的 pipeline、EvalBatchDurable、DistLock 都不经驱动重放，只在 `driver.IsDefinitelyNotExecuted` 判为真时重发（脚本同样，NC-101 复审应改项 1）；Mongo 提交发出之后的失败包 `mongo.ErrCommitResultUnknown`（应改项 2）；cache 的 hash / 有序集合在写结果未知时仍补发 EXPIRE。契约表：[redis/driver](../../redis/driver/README.md)、[mongo/driver](../../mongo/driver/README.md)。调用方核对没有发现双写；bus 的 SETNX 去重、global Bind 的误报、L2 快照 DEL 被吞掉、Redis Cluster 实测，留作观察或交给归属方。[方案](../feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md) |
| A3 | 按推荐：① 共用小类型 + 停机契约测试骨架，③ glsvet 只提示 | **已实施（`50f2ac2a`）**：`internal/operation.Lifetime` 补 `Wait(ctx)`，bus / syncbus / mirror 三份迁移；`internal/stopcontract` 骨架套 manager、kit/nest、syncbus、etcd、mirror、remoteentity、bus、生成 TCP；glsvet `-stophints`（Mutex.Lock 误报约 100%，未加）；骨架发现 NC-173 残余并补修。② 排空下沉到 ISyncBus 退订留待下个大版本。[方案](../feature/REFACTOR-2026-10-05-shared-stop-contract.md) |
| B1 | 协调器接收 completion 时核对代际：做 | **已实施（`3fabe34d`）**：completion 代际从 `CommandID` 解析，旧一生的拒绝 / 失败不接收（`saga.completion.stale_incarnation_total`），旧一生的成功在记录停在该操作上时接收为结果；放弃后迟到的成功按（操作，代际）只告警一次（tombstone `late_alarms`）；补偿方向 `ManualRequired` 上的人工 `Compensate` 进入新一生（正确做法写明为 `Resume`，二者等价）。四个边角先红后绿，真实 Mongo 并发标记通过。Mongo 步骤跨尝试幂等仍待另写方案。[方案与实施](../feature/B1-SAGA-COMPLETION-INCARNATION-2026-10-06.md) |
| B2 | 维护者问“什么意思”，已解释，待决定 | 待决定 |
| B3 | lower 查找失败一律报错：做 | **已实施（`023eb276`）**：① lower 的名字查找经唯一入口 `resolveName`，查不到返回 `LOWER_UNRESOLVED` 编译错误、不交出 Program（回归对全部种子逐表删条目，修前 35 处静默兜底 / 23 处未解析引用 / 4 处 panic）；② phase 事件派发表单一来源 `skill/phase_events.go`（代价小，一并做）。③ 下个大版本，④ 保持 B。[方案](../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md) |
| B5 | etcd 选举：保留（不弃用） | — |
| B7 | ai / actionflow：保留在 core | 重入方向见第三轮（b），已实施 |
| C3 / C8 | event、index 与零调用方 API：保留 | — |
| C4 | 维护者问“什么是活动组 game 服”，已解释，待决定上限 | 已在第三轮决定，见下表 |
| C6 | 默认 metrics adapter：做 | **已实施（`491aaf3b`，分支 `c4c6`）**：`servicemetrics.NewMetricsReporter` 把服务事件写成 `service.{accepted,refused,replayed,dropped,conflict}.total` 与 `service.depth` 六个固定名，经 ops `/metrics` 导出；队列 key / 看板 ID 改为 `key` 标签（`KeyedReporter` / `Sink.DepthOf`，项目自写 Reporter 不受影响），`session.swept` 改为计数 `run.swept`；生成工程 `Metrics()` 默认返回它，`service_metrics.enabled: false` 或返回 nil 关闭；真实进程 `/metrics` 修前 0 行、修后有 `service_*`。**未做**：注册表按标签删除序列（N12 O2 / O3）。新生成工程需要 core ≥ v1.20.2，发版时升 `minimumVersions.Core` 与 framework-compat `minimum`。[方案](../feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md) |
| C7 | 遍历回调语义定为仓库级契约：是 | **已实施（`cd43a5ac`）**：契约写进 roost-coding / README §16 / safemap 包注释；共用辅助 `internal/rangecontract` 套 container、safemap、entity、生成 DAO 三种 map 的 `RangeX`；补 NC-181 残余（`RangeWithCursorCnt` 重走同一桶）与 NC-180 残余（`RangeGroupEntities` 交出已清零实体）；index / lock 无遍历回调。[方案](../feature/C7-RANGE-CALLBACK-CONTRACT-2026-10-06.md) |
| C10 | N10 临时 worktree：已删除（分支已并入 main） | 完成 |
| 发布 | v1.20.2 暂不发，上述实施完成后统一发版 | — |
| 额度 | 额度不足时，先把未完成项记进文档并推送，再停止 | 规则 |

未在本轮答复中出现、仍按上表推荐待定的：B4、B6、B8、B9、B10、C2、C5、C9（C1 已随 A4 按方案 1 实施）。

## 维护者决定（2026-10-05，第三轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| B2 | 按推荐：共享 L2 为快照水位权威，L1 只是有界副本；Cached 读最大陈旧时间写成配置与契约 | **已实施（`f376bba0`、`7d49e54d`）**：L1 写入一律先经 L2 版本 CAS / 带版本删除，L1 只缓存 L2 确认过的版本并带确认时刻；`remote_entity.cached_max_staleness`（缺省 = `snapshot_cache_ttl`）；复制消息带发布时刻，DeliverAll 重放的过老快照不再被接受（O5）；O4 容量、L2 落后于权威（写 L2 失败）、生成配置模板留作后续。[方案](../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md) |
| C4 | 活动组（参与同一全服活动的 game sid 集合）应由一个配置文件定义；每组上限暂定 64，启动 / 加载时校验 | **已实施（`277e1252`，分支 `c4c6`）**：`configs/activity_groups.yaml`（`groups: [{id, game_sids}]`）由生成器为托管 activity 协调器的工程创建，协调器与 game-demo 的 game 经 `activity.groups_file` 读同一文件、用同一个 `kit/service/global/activity.LoadGroupsFile` 校验（每组 ≤ `MaxExpectedGames` = 64、sid 唯一且只属于一个组、正的 int32、本服 sid 必须在组里）；协调器 `sweep_groups` 为空时扫文件里的全部组；game 配置 `activity.game_sids` 删除，已生成工程不迁移。kit activity 除导出上限外还改了 Mod 读取组文件（为了“协调器读同一份定义”，Service 逻辑不变）。新生成的 game-demo 需要 core ≥ v1.20.2。[方案](../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md) |
| B7 | actionflow 回调重入：按推荐（b）——回调里对 runner 的变更进延后命令队列，回调返回后按序执行，判定集中一处 | **已实施（`a9b7075b`）**：判定集中在 `ActionRunner.submit`，删掉 U-0100 / NC-122 的六处事后比对；回调里的 Start / Enqueue 返回已分配 ID（`Deferring()`），拿到 ID 必有 OnEnded；`MaxDeferredCommands` / `MaxDeferredSteps` 有界防失控；O-A2 / O-A3 消失并钉住，O-A1 定义为“EndAll 先结束全部、回调变更随后执行”；`MissionRunner` 未改。[方案](../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md) |

## 维护者决定（2026-10-06，第四轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| 留项 | 补齐 15 个非核心单元的单元内留项（见 REMAINING-REVIEW-HANDOFF 各行停点） | 待实施 |
| B4 | skill Runtime 状态**不进**事务：保持现状，文档写明约束（handler 失败回滚后 Runtime 状态不回退，业务按此设计） | 待实施（文档） |
| B6 | CLI 信号统一接管：CLI 入口接管信号 → ctx 取消 → 正常回滚后再重抛 | **已实施（`f556049c`）**：`roost.Main` 接管 SIGINT / SIGTERM / SIGHUP，复制 / 生成器 / go 命令 / 提交点前按 ctx 停下并回滚、删暂存树，已开始的提交做完，最后重抛（退出码不变）；`runCommandTree` 的信号重发、NC-70 登记表删除，`reraisedSignalGrace` 只留入口一处；cb11be90 / NC-70 回归断言不变。N08 O2 / O3 / O6 一并修复。Windows 不接管（与之前相同，只 vet）。[方案与实施](../feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md) |
| B9 | activity 窗口条目统一读入口 + 持有方修复入口；account 建角改为“名额状态 × 名字状态 → 动作”判定表 | **已实施（`bd6df5e5`，分支 `b9svc`）**：activity 的 `PendingActivities` / `DeliveringActivities` / `RetireDelivered` / sweep 全经 `readWindowEntries`，坏条目跳过、保留、按列表计数（新增 `sweep.delivering_key_malformed`）；持有方专用 `Admin.MalformedWindowEntries` / `RemoveMalformedWindowEntry`；N06 S3 留项补齐（Delivering 坏条目、`RetireDelivered` 只在本次移走时回报 true、`plan_released` 并发只计一次），见 NC-51 / NC-50 复核补修。account：`decideCreation`（名额 6 × 入口 4 × 名字 6，144 格表格测试），create_role 同名 / 换名、同名被拒后释放、Admin 都查它，行为不变。C4 的按组 key 与入口一致，无需改动。表里看出的一处不对称（未 admitted 计划、名字只被他人 reserved：同名请求释放、换名不释放）保持现状，列给维护者。[方案](../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) |
| B10 | 按推荐：configdata 新增能看到原始字段是否出现的接口，必填在加载 / 热更时检查（A）；规则统一由运行时加载层强制，生成期检查只作提前反馈。维护者附加原则：**config 使用要容易，手写整理代码尽量少，结构简单易懂** | **已实施（`b12216ed`）**：新叶子包 `configdata/rules` 是规则的唯一表示与检查（required / unique / min / enum 查原始 JSON，缺列与零值分得清），configdata `TableDef.Rules` / `ObjectDef.Rules` 在每次 Load / Reload 执行（ref 在全部表加载后查），违反整次拒绝并点名表 / 行 / 字段 / 规则；tablegen 的 CSV 转换、`-check` 与生成 loader 共用同一组规则（取代手写 ref 循环），cfggen 的 `cfg` 标签解析成同一组规则；game-demo spawn 读取改用生成的 `SpawnTable()` / `SpawnByID`（15 行胶水 → 3 行）。端到端 `gm.config.reload` 删 required 列被点名拒绝。新生成代码需要下一版 core：发版时上调 `minimumVersions.Core` 与 framework-compat minimum 行。两种标签方言合一、cfggen globals 规则列为后续。[方案](../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) |
| C2 | “需要可见”：按协调者理解——保持新快照即刻可见的语义并写进契约；同时让热更失败 / 回滚可见（日志 + 指标，N07 C-O5） | **已实施（`b12216ed`）**：契约写进 configdata 包注释与 USER_GUIDE §10；`Store.OnReloadOutcome` 每次 Load / Reload / Rollback 恰好报告一次并写 Info / Warn 日志（失败带 stage，撤回为 `stage=apply`）；kit 指标 `configdata.reload.total{result=ok\|failed}`、`configdata.rollback.total{trigger=apply_failed\|operator}`，去掉 `reason` 标签（C-O6），被撤回的 reload 不再同时记 ok；observability README 改正版本号说法（C-O7）。[方案](../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) |
| C5 | 停机中的进程仍算“活着”：保持现状，写进 `Live` 契约 | **已实施（`bd6df5e5`）**：`app/singleton.go` 的 `SingletonLiveness` 注释、USER_GUIDE §2、APP-SINGLETON-LOCK §3.6 / §7.2 写明契约与对 activity 的影响（窗口可能等到宽限期）；用例 `TestSingletonLiveCountsAStoppingProcessUntilRelease` 钉住 |
| C9 | 过时测试开关要测：打开 `publishedDataEngineGeneratorDependencies` 覆盖的用例，放进需要网络的 CI lane 跑 | **已实施（`f556049c`）**：常量换成 `ROOST_NETWORK_TESTS=1`，framework-compat 新 job `codegen-network` 打开（SKIP 即失败），根包 `TestNetworkCodegenTestsRunInSomeWorkflow` 钉住；两条用例本机联网实跑通过（v1.20.2 刚发布，需 `GOPROXY=direct`）。默认 `go test` 不联网。 |
| Mirror | 上述全部完成后，补齐 PLAN-REMOTE-POLICY-MIRROR 剩余实现（只读 DTO reader / 契约、共享 snapshot client、订阅代际与首载缓冲、kit 装配与 codegen 只读产物、真实环境故障与性能报告） | 排队 |
