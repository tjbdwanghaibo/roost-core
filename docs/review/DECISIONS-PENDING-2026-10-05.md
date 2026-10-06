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
| 留项 | 补齐 15 个非核心单元的单元内留项（见 REMAINING-REVIEW-HANDOFF 各行停点） | 进行中：**N11 / N12 / N13 留项与小防护 A / B 已实施（revleft，NC-260～270，`36220f34`）**，O6 / O7 / O9 / N13 O10 待 D-L1～D-L4；**N01（含 N02 O1、N14 O3 / O4）已实施（`2c1c7be7`）**——NC-230～234、`ops.admin_timeout`，[记录](REVIEW-2026-10-06-n01b.md)；Health Degraded 映射待 D1（见文末）；其余单元待实施 |
| 留项 · N10 | N10 第二批（ai 节点、actionflow 池化与 B7 留项、hotcode 真实 .so 与 O-H1 回滚） | **已实施（`44964553`，分支 `revn10b`）**：NC-240～247 修复（含 B7 留项 O-A4 → NC-243、O-H1～O-H3 → NC-245～247，真实 .so 发现 NC-244）；MissionRunner 延后语义、O-A1 清场无确认缺陷，选项与推荐（保持）见 [记录](REVIEW-2026-10-06-noncore-n10b.md#待维护者语义选择无确认缺陷) |
| 留项 · N09 | N09 第五批（编译器 random / snapshot / temporal / graph / effect_result / proc / quantity 逐分支、直接分支用例、性质测试种子） | **已实施（`5c04726f`，分支 `revn09e`）**：NC-220～224 修复（NC-223 为 B3 回归）；NC-224 方向 B（冻结施法输入）、“求值上下文 → 可用引用”单一表与 O27～O33 待维护者定，见 [记录](REVIEW-2026-10-06-n09-batch5.md) |
| B4 | skill Runtime 状态**不进**事务：保持现状，文档写明约束（handler 失败回滚后 Runtime 状态不回退，业务按此设计） | **已实施（`62cec54e`，分支 `revn09e`）**：[skill-casting-and-combat.md](../skill/skill-casting-and-combat.md)“Runtime 不在事务里（B4）”一节（回退 / 不回退对照表、先校验后推进、扣费交给 Runtime commit 路径、失败用 Runtime 终态、提交被拒由业务处理）；skill README 补说明；roost-coding A1 条写明例外；glsvet A1 提示不命中 skill，无需豁免（注释 + `TestSkillPackagesGetNoComponentUndoHint`） |
| B6 | CLI 信号统一接管：CLI 入口接管信号 → ctx 取消 → 正常回滚后再重抛 | **已实施（`f556049c`）**：`roost.Main` 接管 SIGINT / SIGTERM / SIGHUP，复制 / 生成器 / go 命令 / 提交点前按 ctx 停下并回滚、删暂存树，已开始的提交做完，最后重抛（退出码不变）；`runCommandTree` 的信号重发、NC-70 登记表删除，`reraisedSignalGrace` 只留入口一处；cb11be90 / NC-70 回归断言不变。N08 O2 / O3 / O6 一并修复。Windows 不接管（与之前相同，只 vet）。[方案与实施](../feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md) |
| B9 | activity 窗口条目统一读入口 + 持有方修复入口；account 建角改为“名额状态 × 名字状态 → 动作”判定表 | **已实施（`bd6df5e5`，分支 `b9svc`）**：activity 的 `PendingActivities` / `DeliveringActivities` / `RetireDelivered` / sweep 全经 `readWindowEntries`，坏条目跳过、保留、按列表计数（新增 `sweep.delivering_key_malformed`）；持有方专用 `Admin.MalformedWindowEntries` / `RemoveMalformedWindowEntry`；N06 S3 留项补齐（Delivering 坏条目、`RetireDelivered` 只在本次移走时回报 true、`plan_released` 并发只计一次），见 NC-51 / NC-50 复核补修。account：`decideCreation`（名额 6 × 入口 4 × 名字 6，144 格表格测试），create_role 同名 / 换名、同名被拒后释放、Admin 都查它，行为不变。C4 的按组 key 与入口一致，无需改动。表里看出的一处不对称（未 admitted 计划、名字只被他人 reserved：同名请求释放、换名不释放）保持现状，列给维护者。[方案](../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) |
| B10 | 按推荐：configdata 新增能看到原始字段是否出现的接口，必填在加载 / 热更时检查（A）；规则统一由运行时加载层强制，生成期检查只作提前反馈。维护者附加原则：**config 使用要容易，手写整理代码尽量少，结构简单易懂** | **已实施（`b12216ed`）**：新叶子包 `configdata/rules` 是规则的唯一表示与检查（required / unique / min / enum 查原始 JSON，缺列与零值分得清），configdata `TableDef.Rules` / `ObjectDef.Rules` 在每次 Load / Reload 执行（ref 在全部表加载后查），违反整次拒绝并点名表 / 行 / 字段 / 规则；tablegen 的 CSV 转换、`-check` 与生成 loader 共用同一组规则（取代手写 ref 循环），cfggen 的 `cfg` 标签解析成同一组规则；game-demo spawn 读取改用生成的 `SpawnTable()` / `SpawnByID`（15 行胶水 → 3 行）。端到端 `gm.config.reload` 删 required 列被点名拒绝。新生成代码需要下一版 core：发版时上调 `minimumVersions.Core` 与 framework-compat minimum 行。两种标签方言合一、cfggen globals 规则列为后续。[方案](../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) |
| C2 | “需要可见”：按协调者理解——保持新快照即刻可见的语义并写进契约；同时让热更失败 / 回滚可见（日志 + 指标，N07 C-O5） | **已实施（`b12216ed`）**：契约写进 configdata 包注释与 USER_GUIDE §10；`Store.OnReloadOutcome` 每次 Load / Reload / Rollback 恰好报告一次并写 Info / Warn 日志（失败带 stage，撤回为 `stage=apply`）；kit 指标 `configdata.reload.total{result=ok\|failed}`、`configdata.rollback.total{trigger=apply_failed\|operator}`，去掉 `reason` 标签（C-O6），被撤回的 reload 不再同时记 ok；observability README 改正版本号说法（C-O7）。[方案](../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) |
| C5 | 停机中的进程仍算“活着”：保持现状，写进 `Live` 契约 | **已实施（`bd6df5e5`）**：`app/singleton.go` 的 `SingletonLiveness` 注释、USER_GUIDE §2、APP-SINGLETON-LOCK §3.6 / §7.2 写明契约与对 activity 的影响（窗口可能等到宽限期）；用例 `TestSingletonLiveCountsAStoppingProcessUntilRelease` 钉住 |
| C9 | 过时测试开关要测：打开 `publishedDataEngineGeneratorDependencies` 覆盖的用例，放进需要网络的 CI lane 跑 | **已实施（`f556049c`）**：常量换成 `ROOST_NETWORK_TESTS=1`，framework-compat 新 job `codegen-network` 打开（SKIP 即失败），根包 `TestNetworkCodegenTestsRunInSomeWorkflow` 钉住；两条用例本机联网实跑通过（v1.20.2 刚发布，需 `GOPROXY=direct`）。默认 `go test` 不联网。 |
| Mirror | 上述全部完成后，补齐 PLAN-REMOTE-POLICY-MIRROR 剩余实现（只读 DTO reader / 契约、共享 snapshot client、订阅代际与首载缓冲、kit 装配与 codegen 只读产物、真实环境故障与性能报告） | 进行中：**第 1～3 步已实施（`8495c5c4`，分支 `mirror13`）**——只读契约 `entity.RemoteSnapshotReadOnly` / 观察 token / DTO reader、快照缓存唯一读出口、共享 `remoteentity.SnapshotClient`（Manager 委托，停机套 A3 骨架）；修前红：Monotonic 未命中回源两次、Cached 交出低于最低版本的值。**第 4 步已实施（`23e17d81`，分支 `mirror4`）**——可确认订阅（JetStream DeliverNew，普通 NATS 显式退化为按需读取）、首载缓冲、兴趣代际与撤销水位，见第九轮表。**第 5 步已实施（`a6985cf3`，分支 `mirror5`）**——kit `RemoteMirrorMod`（只读 `SnapshotClient`，不要求原子 backend，新键 `remote_entity.mirror.shutdown_timeout`）、codegen `//roost:mirror` DTO 与 `remote=mirror` 迁移诊断、公会摘要两进程样例（真实 JetStream 推送 / 普通 NATS 按需），新能力用 6 个负对照（T-274）；**第 6 步本机替代已实施（`4ca757aa` 修复、`b15e70c8` 用例与记录，分支 `mirror6`）**——私有依赖进程上两进程 7 类故障 0 违例、同机 n=6 对照 v1.20.2 无显著差别；修复 RR-20261006-01（删除 Remote 实体时确认报身份不符、删除不发布）；观察 O-M6-1 / O-M6-3 待维护者；Linux 内核网络、跨主机分区、长时间容量列入外部验证清单，见 [第 6 步本机替代记录](../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md) §5 |

## 新增待决定（2026-10-06，N01 留项 revn01b）

| # | 事项 | 来由 | 选项 | 推荐 |
| --- | --- | --- | --- | --- |
| D1 | `/readyz` 对 Degraded 的处理：现在 Degraded 与 Fail 一样回 503（README 写明“degraded 在聚合层等同失败”），k8s readiness 摘掉该 Pod 的 Service endpoint；生成的服务都是单副本 | N01/S4 O2；[revn01b §3](REVIEW-2026-10-06-n01b.md) | (a) 保持现状（Degraded = 摘流量）(b) Degraded 算就绪：`/readyz` 回 200、`ok` 仍为 true，`dependencies` 照样列出 degraded，只有 Fail 让它 503 (c) 每个 checker 自己声明 Degraded 是否影响就绪 | (b)：四个 Degraded 来源（续期结果未知 ≤ renew_interval、entitysync ≥ 80% 容量、remoteentity 写许可用满、DataEngine 积压告警）都是“还能服务、需要关注”；单副本摘 endpoint 没有可切的副本，entitysync 在 80% 边界上无滞回会来回翻转。代价：靠 readiness 做 80% 容量卸载的部署会失去这个效果（目前仓内没有依赖它的配置） |

## 维护者决定（2026-10-06，第五轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| skill 求值上下文表 | 做：一张表写明每种求值上下文（施法流程 / cast_start / phase_start / 进程启动 / 移交后每步）可用的引用，编译期与 Runtime 都查这一张（N09 第五批方向判断） | **已实施（`4ed038d9`，分支 `revn09f`）**：`skill/eval_contexts.go` 8 个上下文（另加 memory 默认值、状态默认值、进程回调）× 24 行；表外引用 `ErrReferenceOutOfContext`；收拢 NC-220 / NC-224 / owned entity 的引用检查；守卫逐格；做表时发现 NC-280～283 一并修复。O33 漂移格子保持现状、写明语义，是否收紧待定。[方案](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) · [第六批](REVIEW-2026-10-06-n09-batch6.md) |
| NC-224 方向 B | 不做：维持编译期拒绝（进程启动时不冻结施法输入） | 已落实：表的 process_step 列不含 `$input` / `$memory` / `$local`（编译期拒绝、Runtime 查表），未冻结输入 |
| account 换名释放 | 做：未 admitted 计划、名字仅被他人 reserved 时，换名请求也释放 slot（判定表 `unadmitted other` 行） | **已实施（`b18d5613`，分支 `revn09f`）**：只改 res-else 一格（free 列维持 limit，未定），[B9 方案 §6](../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) |
| D1 | 按推荐：Degraded 算就绪（`/readyz` 返回 200 并在响应体注明降级），只有 Fail 返回 503 | **已实施（`f6828f17`，分支 `d1mr`）**：聚合规则只在 `health.Snapshot`（OK = 没有 Fail，新增 `Degraded` / `DegradedResults()`）；`/readyz` 响应体加 `degraded` 与 `degraded_dependencies`（name / status / message / error）；生成的 k8s / compose / shell / dev-run 探针都只看状态码，模板不改；OBSERVABILITY 新节、README、kit/README、USER_GUIDE、singleton 注释、T-267。修前 Degraded → 503（红），修后 200 带降级项、Fail 仍 503。[方案](../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md) |
| MissionRunner | 改为延后队列（与 ActionRunner / B7 一致：回调里的变更回调返回后按序执行） | **已实施（`f6828f17`）**：StartMission / CancelMission / EndCurMission / Tick / OnActionEnd 经 `submit` 一处判定，回调里返回 nil、执行错误经 OnError（`deferred start mission`），`MaxDeferredCommands` / `MaxDeferredSteps` 有界，SetRuntime 补 recover、`executing` 在 defer 里复位；starting / ending 与 `ErrReentrantMutation` 分支删除。两条既有用例按延后语义改写（理由逐条）。ai 无代码改动（OnMissionEnd 里 SetMission 从 `ErrReentrantMutation` 变为 nil + 结束后启动）。[B7 方案 §7](../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md) |
| EndAll 清场 | 按推荐：保持现状，在接线说明写清“要清场先结束当前任务再 EndAll” | **已实施（`f6828f17`，文档 + 用例）**：actionflow 包注释、`EndAll` / `ActionList` 注释、ai `Controller.SetStrategy` 注释、kit/README、B7 方案 §8；`TestEndCurMissionBeforeEndAllLeavesNothingRunning` 钉住（现状即绿） |

## 新增待决定（2026-10-06，N11～N13 留项 revleft）

[记录 §5](REVIEW-2026-10-06-revleft.md)。编号带 L 前缀，避免与并行轮次的 D 编号相撞。

| # | 事项 | 来由 | 选项 | 推荐 |
| --- | --- | --- | --- | --- |
| D-L1 | timer 同一期限的触发顺序 | N11 O6 | (a) 保持，注释写明未定义 (b) `timerHeap.Less` 以 ID 作第二键 = 登记顺序 | (b)，代价一次比较；无需求时 (a) 亦可 |
| D-L2 | 存储节点的类型没有注册 handler 时到期 | N11 O7 | (a) 保持静默删除 (b) 删除 + Warn + 计数，加载时对无 handler 的存量类型告警一次 (c) 保留不删、跳过触发 | (b) |
| D-L3 | World 定时器 / 活动窗口跟不跟 `time.logic_offset` | N11 O9 | (a) 保持墙钟，文档写明 offset 的作用范围 (b) game 与协调器全链用 `clock.Now`，部署约束同组 offset 相同（doctor 检查） | 现在 (a)；要用偏移测活动时再 (b)（只改 game 一端会与协调器错开） |
| D-L4 | `goroutine.Parallel*` 回调 panic 吞成零值 | N13 O10 | (a) 保持 (b) 改返回签名带错误 (c) 删除零调用方 API | (a)，出现调用方再定 |


## 维护者决定（2026-10-06，第六轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| D-L1 | 同期限定时器缺省按登记顺序（ID 作第二键）；**新增可选 `priority` 字段**用于排序：先比期限，再比 priority，同 priority 按登记顺序 | 已实施（5abae51e，priority 数值小的先触发，[方案](../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)） |
| D-L2 | 按推荐：未注册类型的到期节点删除时 Warn + 计数，加载时对无 handler 的存量类型告警一次 | 已实施（5abae51e，指标 `timer.unhandled_dropped_total{kind}`，T-268） |
| D-L3 | **修订（维护者确认推荐边界）**：两个时钟、边界写死。**业务时钟**（带 `logic_offset`，App 统一提供）：活动窗口与协调器、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗游戏时间等全部业务时间。**系统时钟**（真实时间）：server 帧率、租约 / 锁、超时、重试退避、存储 TTL、消息 Ack、日志 / 指标 / WAL 时间戳。约束：偏移单一配置源、只在启动时生效或只许前拨、生产启动校验强制为 0、业务过期不靠存储 TTL 判定（TTL 只兜底且更长）、glsvet 提示业务包直接 `time.Now()` | 已实施（b9fc5342，`app.BusinessClock` / `clock.Business`；kit activity / mail / rank / session 与 game-demo 业务时间接业务时钟，mail 领取租约改系统钟、信封 TTL 加 24h 宽限；生产非 0 拒启；glsvet `game` 目录提示，T-270。match / chat / account / saga 的归属待定，见[方案](../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md) §3.3） |
| D-L4 | 按推荐：`Parallel*` 回调 panic 保持现状 | — |
| saga 方向 | 按推荐：① “离开当前步骤”收成一个转移（截止 / 人工 Compensate / 定义缺失 / 正常推进共用）② Mongo 步骤纳入与原生步骤同一套“同一操作最多生效一次”的收件箱契约；③④ 暂不做 | 已实施（① `a95cf4dc`，② 与 O-S5-1 / O-S5-3 `9669d181`；[方案](../feature/SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md)，未发版） |

## 维护者决定（2026-10-06，第七轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| O33 | 漂移格子改为编译期拒绝：进程字段 / 状态默认值里使用会漂移的施法引用（`$primary_target`、`$cast.*`、`$ability.self`、进程字段里的 cast_start / phase_start 读取等）直接报错，提示改用进程自己的引用 | **已实施（`f6043e44`，分支 `o33`）**：21 个 ~ 格子改为不可用（含 memory 默认值的 phase_start：只是 Activate 时的值、与 cast_start 相同，拒绝），诊断点名上下文、表项并给替代写法；快照点表补逐格守卫。[表方案 §8](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) |
| O34～O36 | 按推荐：保持现状并写进作者文档 | **已实施（`f6043e44`，文档）**：[施法语义 · 引用在哪里能读](../skill/skill-casting-and-combat.md#引用在哪里能读求值上下文)、AI prompt、表 semantics |
| O37 | 按推荐：account 判定表 `unadmitted other` 行的 free 格也释放名额并用新名字建角 | **已实施（`f6043e44`）**：free 格 limit → retry，[B9 方案 §6.1](../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) |

## 维护者决定（2026-10-06，第八轮：D-L3 留项）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| match | 匹配是业务逻辑：票据超时、等待放宽改走业务时钟；match 服务与 game `matchmaking.Pools` 一起换（偏移只在启动生效，等待时长不受影响） | 已实施（fa472ee7） |
| chat | 展示给玩家的消息时间走业务时钟；保留期清理（空间回收）留系统时钟，拆成两个字段 | 已实施（fa472ee7） |
| account | 创建时间等业务用途走业务时钟 | 已实施（fa472ee7） |
| saga 截止 | 保留系统时钟 | — |
| 偏移一致 | `roost doctor` 检查所有服务配置的 `time.logic_offset` 一致 | 已实施（fa472ee7） |

## 维护者决定（2026-10-06，第九轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| Mirror 第 4 步 | 按推荐：推送订阅依赖 JetStream，`sync/syncbus/driver` 按主题加 DeliverNew 消费，保证确认订阅之后的发布不被静默丢掉；没开 JetStream 时退化为按需读取（Cached 按 `cached_max_staleness` 回源），显式检测并记日志；订阅可确认、首载缓冲有上界、溢出有明确行为，覆盖重连、旧 fetch 回调、renew / release 全交错；所有缓存写入仍只经 `admitLocked` | **已实施（`23e17d81`，分支 `mirror4`）**：`fsyncbus.ILiveSubscriber` / JetStream `SubscribeLive` / `mirror.NewLive`；`SnapshotClient.Start` 只在可确认订阅上开推送，普通 NATS 记 Warn、`PushEnabled=false`（T-272）；快照缓存 `ApplyReplica` 首载缓冲（缺省 64，溢出丢弃并再回源一次）；兴趣代际锁内分配 + release 撤销水位。修前红：加载期间的增量让权威读两次并丢失、release 后迟到的旧续租复活租约。真实 JetStream + Redis、B2 组合矩阵、生成工程 12 条通过。[记录](../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)（§6.8 第 5 步入口） |
| O4 | 按推荐：兴趣容量按节点计数（每个 consumer 节点各有配额），满了明确拒绝、可识别错误、日志与指标，续期失败时消费方感知并退化为按需读取；配额作为配置项，A4 严格读取并登记 | **已实施（`23e17d81`）**：`remote_entity.snapshot_interest_per_consumer`（0 = `snapshot_interest_subs / 16`，不能超过它）；`ErrInterestQuotaExceeded` / `ErrInterestRegistryFull`；`interest_rejected_total{reason}` / `interest_renew_refused_total{reason}`、限频 Warn、健康信息 `interest_refused`（T-273）。修前红：一个 consumer 占满全表后另一个 consumer 的兴趣在 owner 处被拒、读路径静默吞掉 |

## 发版前审查跟进（2026-10-06）

| # | 事项 | 实施状态 |
| --- | --- | --- |
| 审查修复 | Cached + AllowStale 远程访问回归（`207163f9`）、saga stepTransition 守卫补漏（`42419890`） | 已实施 |
| 审查观察 | activity 派发退避 / 凭证改系统钟、saga `ErrDefinitionMissing` 改可重试、`configdata/rules` 大小写选择确定化、mail 存储宽限写文档；saga 回放不交还 claim 列为观察（[记录](../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)） | 已实施（`5a3c4a60`） |

## 维护者决定（2026-10-06，第十轮：Mirror 第 6 步观察）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| O-M6-1 | 按推荐：同 sid 重启的 owner 启动时广播“请重新续租兴趣”，只读方收到后立即续租，推送不等 15s |**已实施（`db67b8ee`，分支 `m6obs`，未发版）**：新主题 `remote_entity_interest_refresh`，推送开着的只读方经同一续租入口立即续租，合并、间隔不小于 1s；修前红：同 sid 重启后新提交不推送。私有环境 S1 推送恢复从陈旧上限回源（约 1.9～2.2s）变为先于回复到达（T-278）。新观察 O-M6-6（强杀后同 sid 第一笔写等旧锁 TTL）待维护者。[记录](../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md) |
| O-M6-3 | 按推荐：只对 L2 删除墓碑写入加 `WAIT`（等副本确认后返回），缩小切主时墓碑未复制导致的删除短暂复活窗口 |**已实施（`db67b8ee`，未发版）**：墓碑脚本后同连接 `WAIT`（新驱动能力 `EvalReplicated`，Cluster 只打该键主节点，没有副本不等），新键 `snapshot_l2_tombstone_wait_replicas` 缺省 1 / `_timeout` 缺省 50ms 上限 1s；结果只计数 / Warn，不回滚（T-277）。私有 Redis 复制滞后 + 立刻提升副本：修前新只读方读到已删实体，修后墓碑仍在 |

## 维护者决定（2026-10-06，第十一轮）

| # | 决定 | 实施状态 |
| --- | --- | --- |
| O-M6-6 | 按推荐：同 sid 新进程（已持 App 单实例锁）启动时立即接管上一代同 sid 进程留下的 Remote 实体锁（按锁记录的进程代际令牌判定，只接管“同 sid、旧代际”） | 待实施 |
| 收尾 | 盘点全部未完成问题，处理完后统一发一个版本 | 进行中 |
