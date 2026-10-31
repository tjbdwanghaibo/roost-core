# 后续设计与优化候选

本页只列尚待选择或排期的事项，不代表承诺交付。当前能力与验证边界见 [KNOWN-LIMITS](KNOWN-LIMITS.md)，现行设计见 [框架手册](../framework/README.md)。已完成的 D1–D23 决策与实施记录保存在[整理前快照](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md)。Redis 权威路线已放弃；Saga 当前采用 JetStream 协调器，见 [Saga](../framework/SAGA.md)。

## 待维护者决定（不急）

以下保留既有编号以便交叉引用；保持现状、延期和待实施分别说明，不把候选当成缺陷。

3. **`skill.Driver` 是否加公开的 `Close()`**。现状：接口只有 `Wake() error` / `Sleep()`；撤销由 `CombatComponent.CloseBuffDriver` / `RuntimeBeat.Close` 提供（心跳驱动撤销登记，其他驱动收到一次 `Sleep`）。**推荐不加**：驱动由谁创建就由谁停，`skill` 不必知道驱动的生命周期；加了之后每个实现都要处理“关闭后又被 Wake”。
4. **entitysync“暂停不重置”原语（Hold / Ready 方案 B）**：只停发布、保留基线与待发 Remove。现状：每次 Hold → Ready 都开新 epoch、按全量重发仍在的对象（2026-10-21 起一个订阅都不剩时补空 Full）。**推荐先不做**：SLG 文档已建议只在权限读失败与重连时 Hold；按周期每轮都 Hold 的业务代价是每周期一次全量（KNOWN-LIMITS“会话 Hold / Ready”）。
5. **Interest 一步的切点是否让命令也触发切开**。现状：触发——2026-10-22 纯前缀切开之后命令也计入单位上限，只有命令的一段日志同样按 ApplySync 预算分步。可选：不触发（命令不计单位，只有命令的长日志一次做完，政策阶段可能超出预算）。**推荐保持现状**（规则只有一条：预算 / `BatchSize` 只决定停在哪个前缀边界），维护者可以选不触发。
6. **Remote 托管实体禁止挂移动 / 心跳组件**：维护者 2026-10-21 晚已定方向（理论上不应挂，`MapBasic.Sync` 不加），现状是登记时只拦“当时已在共享模式”的实例、运行中进入共享的由下一拍停掉。要在装配或登记时一律拒绝需要 Remote 的种类级声明（Remote 的改动），**另行设计**，未排期。
7. **学习分支 `learn/main-review-20261021` 第 11 条的设计意见**：已分类登记在下文 [优化项（下一阶段）](#优化项下一阶段) 第 2～4 组；第 2、3 组与第 1 组方向一已实施（维护者 2026-10-22），第 4 组逐项待维护者排期。
8. **合服的 saga 终态检查是否支持按类型 / 业务键过滤**（D15）。现状：检查整个 saga 存储（维护者 2026-10-21 确认严格），同一存储里其他服仍在运行的 saga 也会让检查不通过。可选：按类型 / 业务键过滤。推荐保持严格，有真实多服共用 saga 存储的部署再定。
9. **压测工具的缺省驱动**（D20 P4）。现状：sync-aoi `-move-driver=loop`、nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker`（旧复跑命令得到原负载）。维护者 2026-10-21 晚：v1.31 发版前对比后再定；对比在今晚压测清单里。**2026-10-22 晚对比已做**（[HB-DRIVEN-LOAD §4A](../performance/HB-DRIVEN-LOAD-20261021.md#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)）：心跳驱动进程 CPU 约 −19%（nest-game）/ −11%（sync-aoi）、Sync 延迟更低、多视野不失守，但负载形状与历史报告不同（固定移动者），普通业务消息服务端 P99 0.87 → 2.13ms；选 A（缺省改心跳，旧报告复跑显式加 `loop` / `ticker`）还是 B（维持全局循环，心跳作为对照档）仍待维护者决定。心跳驱动下业务服务端 P99 上升维护者 2026-10-22 晚定为专门调查，见“优化项（下一阶段）”第 6 组。
10. **编码失败断开的防风暴**（S5）。现状：原因持续时客户端反复重连，靠 `player.sync.session_lost_disconnect.total` 与 Warn 发现。可选：按玩家冷却（冷却期间要么连着收不到帧，要么拒绝登录，都是新的产品行为）。**推荐不做**，根治在业务侧（`MaxVisible`、实体大小）。
11. **嵌入 TCP 驻留字节 Gauge 与按帧龄提前关闭停读连接**：维护者 2026-10-20 决定等重档数据再定；2026-10-20 夜间重档拒绝为 0，不构成要做的证据。
12. **独立 Gate 下的 Lockstep 追帧背压与 Game / Gate 帧上限跨进程检查**：维护者 2026-10-20 决定暂不做（都要协议改动：Gate 写出回执、握手互报上限），等有具体游戏使用“Lockstep + 独立 Gate”时再做。
13. **AI 无观察者暂停的 6 个待拍板项**（方案 [§11](FEATURE-2026-10-22-ai-idle-without-observers.md#11-待拍板附推荐)，实施时定；均推荐 A）：
    - Q1 恢复延迟（探测档缺省值）：**A** 2s 档，平均约 1s 恢复；B 1s 档；C 加“计数从 0 变成大于 0”的边沿通知（与只读快照原则冲突，不推荐）。
    - Q2 去抖：**A** 扫描 2s × 连续两轮；B 1s × 三轮；C 5s × 两轮。
    - Q3 暂停时在途的任务与动作：**A** 结束当前任务、清空排队动作、当前动作走完，可用 `OnPause` 替换；B 只停决策、不碰任务；C 连当前动作一起结束。
    - Q4 game-demo 是否开启作为示范：**A** demo 装 `wiring/aiidle`、配置打开、怪物 `EnableIdle`、场景开 `TrackObservers`（“没有玩家时验证巡逻”的用例改为测试配置里关闭或放一个观察者）；B 只接线、配置关闭。
    - Q5 计数口径：**A** 按 held、含关系视野与块覆盖、排除 self；B 只算空间视野。
    - Q6 新生实体的初始状态：**A** 从运行开始、两轮内由扫描者决定；B 出生即暂停、等探测拍恢复。

## 优化项（下一阶段）

登记于 2026-10-22。**维护者 2026-10-22：“如果这些优化项不复杂的话，可以直接做了”**：第 1 组方向一、第 2 组、第 3 组已实施（下文各条标“已实施”，提交见 [v1.31.0 实现篇 O1～O4](../release/v1.31.0-IMPLEMENTATION.md#o1)）；第 1 组方向二（AI 停止）另行实施；其余待排期。

### 1. 移动心跳的缺省档位与无观察者降档

**维护者 2026-10-22**：MMO / SLG 的移动频率一般是 2～5Hz；附近没有玩家观察时，降低心跳档位或停跳。

- **现状**：`gameplay/movement.DefaultMovePeriod` = 100ms（10Hz）；sync-aoi `-move-period` 缺省 100ms；D20 的规模估算与多数心跳压测（HB-STRUCTURES、NEST-BATCH-HEARTBEAT、HB-DRIVEN-LOAD）都按 100ms 档做。
- **方向一：缺省移动档位改到 200ms 或 500ms**（5Hz / 2Hz）。**已实施（维护者 2026-10-22，`fde5c7ab`，[O1](../release/v1.31.0-NOTES.md#o1)）**：`DefaultMovePeriod` 100ms → 200ms；game-demo 的玩家移动与怪物巡逻用缺省、随之 5Hz；生成器示例动作 `ExampleAction` 不是移动、仍 100ms 档，`actionflow.DefaultBeatPeriod` / `DefaultMissionPeriod` 不变；sync-aoi `-move-period` 缺省跟 `DefaultMovePeriod`，[HB-DRIVEN-LOAD §4](../performance/HB-DRIVEN-LOAD-20261021.md#4-待补测2026-10-21-因宿主资源占用推迟) 的复跑命令显式加 `-move-period=100ms` 与历史数据同口径。
- **方向二：附近没有玩家观察时 AI 停止。状态：方案已写，待实施**（[FEATURE-2026-10-22-ai-idle-without-observers](FEATURE-2026-10-22-ai-idle-without-observers.md)，留到优化阶段做）。
  - 维护者 2026-10-22 原话：“一般是没有 AOI 计数时候是 AI 停止”；“我之前的处理是单开一个 running 处理这个事，发现可以停的就使用 nest 的广播信息，通知暂时停止”；“恢复的话一般是 entity 自己心跳恢复”。
  - 维护者补充：这是 feature，使用者自己选择用不用，**缺省不启用**（生成器 `roost add behavior --ai` 缺省也不启用，只在注释里写怎么打开）。
  - 方案要点：触发条件是实体的 AOI 观察者计数降为 0；暂停由独立扫描者按周期读计数（`InterestConfig.TrackObservers` 打开时 Interest 维护计数，`ObserverCount` / `ObserverCounts` 只读快照，关闭时零开销），对可以停的实体发 Nest 广播，`ai.Controller` 记“无观察者暂停”原因位（与业务 `Freeze` 叠加），结束当前任务、清空排队动作、当前动作走完，`"ai"` 心跳换到低频探测档；恢复由实体自己的探测拍完成（计数大于 0 时恢复并重新决策）。扫描者 `ai.IdleScanner`，进程开关是新 Mod `wiring/aiidle`（`ai_idle.enabled` 缺省 false）；Remote 托管实体不支持（Nest 广播不接受 Remote 目标）。待拍板 6 项见上文“待维护者决定（不急）”第 13 项；工作量约 2.5～3.5 天。
- **次要选项：降档**（没有观察者时降低移动心跳的频率，而不是停 AI）：要定换档的事务语义（`Reg.SetPeriod` 随事务）与跟随这类依赖他人位置的动作是否例外。

### 2. 只需改文档

已完成；现行边界见 [时间与定时器](../framework/guide/10-time.md) 和 [Sync](../framework/guide/04-sync.md)。

### 3. 小改动

冻结期间任务超时暂停、空间视野总数限制已完成；ApplyAsync 政策阶段耗时预算仍未实施。

### 4. 功能 / 优化（同一出处）

- **一个视野多个视点**：阵营视野来自几十个单位时，现在要一个单位一个空间视野。建议空间视野支持多个视点（并集），挂接数不再随单位数增长。
- **按观察者过滤可见性**：隐身 / 反隐、相位、遮挡。现状可见性只看距离（与关系视野）。建议在 AOI 评估里加观察者谓词钩子。
- **`MaxVisible` 的优先级**：现状按距离选最近 N 个，密集主城里当前目标、BOSS 可能被挤掉。建议加“必须可见”的优先级（不占或优先占 N）。
- **状态在回滚后自动 Refresh**：现状 `state` 观察者的副作用不随回滚，回滚也不通知，要等下一次改动或业务 `Refresh`。建议回滚时自动 Refresh，缩短不一致窗口。

### 5. game-demo 场景的热点（历史负载，需按当前实现复核）

本组是 2026-10-22 的历史负载观察，不是当前版本容量结论。Saga 此后切换 JetStream，lease fence 已删除；不能再把下述围栏现象列为当前故障。后续增强按当前正式链路重新定位。

**现象与数据**（[压测清单 §3 结果](../performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)，发版候选 `3d727c2f`，单机私有环境，100 / 200 个机器人**同一秒起跑**，单 IP 上限 512）：connect 全部成功、`object limit exceeded` 0，但成功只有 24 / 100、30 / 200。三个热点：

- **`enter_game` 写全服 World 实体、上单场景实体**：200 人时同一批登录里 `Sync_RecordEnter`（全服一个 World 实体）106 次、`MultiSync_EnterScene`（一个场景实体）110 次在请求期限内没轮到（`nest: sync canceled`），复制会话因此没打开，110 个 `scene_ready` 失败；`player_tcp_dispatch_timeout_total` 122。同一实体在 strict 持久化下约 108 TPS（[GENERATED-TCP-STRICT](../performance/GENERATED-TCP-STRICT-20261020.md)），一秒内两百次登录必然排队。
- **gift saga 步骤排队**：100 人时 41 次 `saga: step command expired before it ran`（步骤超时 5s，命令排队到期才轮到），机器人 10 秒轮询窗口内 69 个 saga 未到终态；运行结束后 132 个 saga 全部到终态。gift 原生步骤围栏期间同一玩家的心跳提交被拒 9 次（`lease-fenced step awaiting projection`，下一拍照常）。
- **`found_guild` 慢派发**：Multi 同时持公会目录实体与玩家，100 人时慢派发 27 次（最长 2.96s）、失败 3 次，200 人时慢派发 36 次、失败 6 次。

**按秒爬坡结果（2026-10-23 凌晨，[清单 §3](../performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)）**：每秒起跑 2 个机器人时 100 / 200 人全部通过（场景 p95 9.2 / 11.2s）；每秒 4 个时 56 / 100、16 / 200；每秒 10～20 个时 0～20%。瓶颈依次是 gift saga（每个机器人两次原生步骤 saga，每秒 4 个机器人 ≈ 每秒 8 个 saga，正是原生步骤单进程容量）、等奖励邮件的 effect 链、`found_guild` 的公会目录实体（每秒 2 个时已慢派发上百次）；同秒起跑才出现的 World / 场景实体超时在爬坡下没有再出现。没有跨轮的状态复用（每轮新账号）。按现有证据没有框架缺陷。

**下一步（优化方向）**：gift saga 的吞吐（原生步骤每进程约 8/s；demo 场景可以少发或合并 saga、或拉长机器人的轮询窗口）；奖励邮件 effect 链在 saga 高峰下的延迟；World 计数改成异步 / 合批、登录上地图与计数拆出关键路径；公会目录分片。机器人阈值按冒烟规模定，压测规模另给阈值。

### 6. 心跳驱动下普通业务消息的服务端 P99 上升（维护者 2026-10-22 晚决定 A：专门调查）

与“待维护者决定”第 9 项（压测工具的缺省驱动）一起看。

**数据**（[HB-DRIVEN-LOAD §4A.3](../performance/HB-DRIVEN-LOAD-20261021.md#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)，nest-game V1290 重档参数、嵌入 TCP + 场景 Sync、各 15 分钟、单样本）：帧回调（ticker）→ 心跳驱动（缺省批量投递，组 4 / 200µs）时，普通业务消息**服务端** P50 / P95 / P99 0.08 / 0.39 / 0.87 → 0.51 / 1.58 / 2.13ms；同时进程 CPU 约 2.6 → 2.1 核、short 等待峰值 2614 → 139、Sync 提交→客户端 P99 9.42 → 6.07ms；业务端到端（混合口径）P99 11.25 → 11.44ms 基本不变。同方向的微基准：[NEST-BATCH-HEARTBEAT §3G](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3g-2026-10-22-晚参数补测结果v1310-发版前压测第-1-项) 业务 p99 单目标 0.095 → 批量 0.149ms（回调是几十纳秒的内存计算）；nest-game 的心跳回调带事务、更重，差距更大。

**要查**：批量作业一次占 worker 的时长分布（组大小 × 回调耗时，时间片在每组结束后才检查）；改为组 4 / 100µs（维护者 2026-10-22 晚已定，已实施 `ee01221e`）之后在 nest-game 上复测；业务消息与批量作业的公平性（是否需要按池分开或让业务优先）；单目标 `heartbeat.delivery: dispatch` 在同参数下的对照。另一个数据点（[清单 §7 结果](../performance/V1310-PRERELEASE-20261022.md#7-可选移动-200ms5hz口径缺省已改历史口径显式-100ms)，投递对比夹具 3 轮）：登记档位从 100ms 换成 200ms（拍数减半）时，批量投递的业务 p99 反而 0.152 → 0.312ms，单目标 0.092 → 0.077ms，原因未查。

## 后续候选（未排期）

- D2 方案 B：SyncBus 框架级对账（订阅端检测缺口后向发布端请求快照）。
- D3 方案 B：Drain 的明确放弃出口（要先梳理哪些拒绝是永久的）。
- D4 方案 B：`roost add migration` 骨架；上线后重新决定是否允许在线双读期。
- D5 方案 B：框架级“迁移 Player”运维命令（上线后有真实迁服需求再评估）。
- D11 方案 C：Remote 托管与 noPersist 实体的持久定时器。
- D1 后续：独立 Gate 写出回执（快照 / 追帧随链路放慢）、客户端上传快照、增量 / 压缩快照、检查点持久化与宿主崩溃续赛、Unity / IL2CPP 实测、game-demo 战斗接快照恢复。
- D21 其余 ssr 候选：`trigger_engine`、`eca_man`、`cooldown`、`timer`；空间（`rvo`、`dynamic_obstacle`、`hexagon`、`mist` 等）、战斗（`effect`、`damage`、`aggro`）、经济（`bag`、`drop`、`gacha`、`limit`）、社交（与 `service/*` 比对补缺）。原则：按 roost 模型重写、只收与玩法无关的通用部件，每个先写方案再实施。
- 嵌入 TCP / Game Ingress：Manager 在 `RetryLater` 时跳过被拒会话（预算持续耗尽时减少整轮重试的放大），需要时再做。
