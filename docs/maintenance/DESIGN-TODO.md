# 设计事项：维护者的决定与待决定项

本篇登记**需要维护者做产品 / 设计选择**的事项：已决定的每项只留“问题一句话 + 维护者决定（含日期）+ 结果 + 链接”，仍待决定或后续要做的单列状态。实现细节、提交、强制点与红绿证据在各版本的实现篇（[v1.28.0](../release/v1.28.0-IMPLEMENTATION.md)、[v1.31.0](../release/v1.31.0-IMPLEMENTATION.md)）、`docs/framework/impl/*` 与方案 / 性能报告里；工程缺口与验证边界见 [KNOWN-LIMITS](KNOWN-LIMITS.md)。2026-10-22 收尾前本篇按期累积的过程记录（方案原文、各期变异核对红文本、中途数据）原文见[固定提交 `603ec537`](https://github.com/tjbdwanghaibo/roost-core/blob/603ec537068a672ba5806d058e154526dd38a3a4/docs/maintenance/DESIGN-TODO.md)。

决定后在对应条目写明“维护者 YYYY-MM-DD 决定 …”；不把本篇条目当作已承诺的功能。

## 状态总表

| # | 事项 | 状态 | 发布 |
| --- | --- | --- | --- |
| D1 | Lockstep 历史的保留与过期策略 | 10-15 决定 C（自动保留 + 越界明确失败），已实施；10-20 方案 B（全量快照恢复）立项并实施 | C：v1.28.0；B：v1.31.0 |
| D2 | SyncBus 有限投递的兜底 | 10-15 决定 A（可丢广播 + 业务对账模板），已实施；B 为候选 | v1.28.0 |
| D3 | Entity Sync `Drain` 遇到永久拒绝的出口 | 10-15 决定 A（诊断增强），已实施；B 为候选 | v1.28.0 |
| D4 | 业务 schema 离线迁移的模板 / 流程 | 10-15 决定 A，已实施；B 为候选 | v1.28.0 |
| D5 | 静态 Player 跨机迁移的参考流程 | 10-15 决定 A，已实施；B 上线后评估 | v1.28.0 |
| D6 | 玩家看自己状态（自身视图）的接入骨架 | 10-16 折中方案，已实施 | v1.28.0 |
| D7 | 实体不存在时的稳定错误码与首登步骤 | 10-16 决定 C，已实施 | v1.28.0 |
| D8 | 生成的服务包名大写 | 10-16 决定 C，已实施 | v1.28.0 |
| D9 | DAO 库名不随配置、`_id` 是编码后的 EntityID | 10-16 决定 B，已实施；10-20 修订版 A（逻辑名 + 部署映射），已实施 | B：v1.28.0；A：v1.31.0 |
| D10 | 删档后用同一 ID 重建 | 10-17 决定 B（只守 GetOrCreate），已实施 | v1.28.0 |
| D11 | 按实体的持久定时器接入 | 10-17 决定 B 与离线语义，已实施 | v1.28.0 |
| D12 | Saga 原生实体步骤与事务性 effect 消费者的生成支持 | 10-18 决定 A、B，已实施；容量按 A 接受 | v1.28.0 |
| D13 | DAO 持久键的来源与水合时的键集合校验 | 10-19 决定并实施 | v1.28.0 |
| D14 | AOI 观察者改为 playerId、视野按观察者配置 | 10-20 决定 A1 + B1，已实施 | v1.31.0 |
| D15 | 合服 | 10-20 决定别名合服，已实施 | v1.31.0 |
| D16 | 单个实体 / 玩家的运维入口 | 10-20 决定 C，已实施 | v1.31.0 |
| D17 | 全服邮件的内置投递策略 | 10-20 决定 A，已实施 | v1.31.0 |
| D18 | 业务时区与“业务日”边界 | 10-20 决定 A，已实施 | v1.31.0 |
| D19 | 视野（View）：逻辑 ID、空间 / 关系两类、多对多 | 10-20 决定实施，已实施；10-21 两种应用模式；10-21 起 Interest 单写者 | v1.31.0 |
| D20 | 移动由实体自己的心跳驱动 | 10-21 定方案，P1～P5、选项 C、生成器接入均已实施；10-22 心跳缺省批量投递 | v1.31.0 |
| D21 | 吸收 ssr 的优秀组件进 `gameplay` | 调研一（行为控制）10-21 决定 13 条按推荐，已实施；其余候选未调研 | v1.31.0 |
| D22 | 业务时间取自 ctx、支持变速 | 10-21 收窄为 D22-1 + 场景时间域，已实施；部署级变速取消 | v1.31.0 |

v1.31.0 尚未发版（待 2026-10-22 晚整体压测与发版验证）。已决定、不在本篇：Session 运行服务保持现状、不改为可选（2026-10-14）。

## 待维护者决定（不急）

下列各项都已按推荐先做（或先不做），维护者说不急；每项写现状与推荐。

1. **学习分支 `fix/core-game-server-review` 的深化 SLG 夹具是否合入**（`scripts/testdata/slg-alliance/**`、`scripts/test-slg-alliance*.sh`，约 4000 行测试与夹具，另有 SLG-INTEGRATION 约 470 行深化记录）。现状：深化内容的结论已整理进 main 的 [SLG-INTEGRATION §7.1 / §7.7](../framework/SLG-INTEGRATION.md)，夹具没有合入，文档写明 main 上没有对应入口。**推荐不合**：夹具要真实 Mongo / Redis / NATS、维护成本高，验证的都是业务接法而不是框架契约；需要时按章节单独拣。
2. **同一分支的几份小文档是否合入**：MMORPG-INTEGRATION 的“专题优先级：跨服、视野、活动”（约 31 行）、`scripts/README.md` 的深化入口说明（约 19 行）、`docs/framework/README.md` 一行改写。现状：未合入。**推荐不合**：它们指向深化夹具与 SLG 深化章节，main 上没有这些入口，合入会产生无效链接；MMORPG 的专题表需要时去掉对深化夹具的引用后单独补。
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

### 2. 只需改文档（学习分支 `learn/main-review-20261021` 第 11 条）

**已实施（维护者 2026-10-22，`8ead3dd1`，[O4](../release/v1.31.0-NOTES.md#o4)）**：guide/10-time §4B“逐拍移动适合谁”（SLG 行军、`Elapsed` 可变步长与 Lockstep）、§4C 控制等级 / 韧性 / 递减由业务在 Buff 层做；guide/04-sync 应用模式表的 MOBA 一行改为 ApplyAsync（或等“一个视野多个视点”），帧同步 MOBA 走 Lockstep，`policy.ApplyMode` 注释同步。下面是登记时的原建议。

- **SLG 行军**：现状 guide 没写边界，读者可能把行军做成逐拍移动。建议在 guide/10-time §4B 写明：行军按“起点 + 速度 + 出发时刻”按需算位置，到达用 entitytimer，不逐拍移动。
- **可变步长不适合确定性玩法**：`Beat.Elapsed` 是可变步长；MOBA / 帧同步走 Lockstep。建议在 §4A / §4B 写明。
- **控制等级 / 韧性 / 递减**：框架没有，建议在 guide 写明由业务在 Buff 层做（`BindStates` 只做派生状态）。
- **guide 里 MOBA 的 ApplySync 推荐要修正**：[guide/04-sync“两种应用模式”](../framework/guide/04-sync.md)的表让 MOBA 用 ApplySync，但阵营视野来自几十个友方单位，挂接数远超 9。建议改成“阵营视野做成一个关系视野 + 少量空间视野时 ApplySync，每个单位一个空间视野时 ApplyAsync”，或等“一个视野多个视点”（第 4 组）后再改。

### 3. 小改动（同一出处）

- **定身期间计划步骤的 Timeout 暂停计时**：现状 `PlanMission` 的步骤时限从发起时刻算、`FreezePause` 期间照样计时，定身长于 Timeout 就失败（护送 / 巡逻偏严）。建议冻结期间暂停时限（解冻时顺延）。**已实施（维护者 2026-10-22，`a0a71a4a`，[O2](../release/v1.31.0-NOTES.md#o2)）**：`Behavior` 按自己的时钟记各组冻结起止（可选接口 `actionflow.FrozenTimer`），`PlanMission` 从已用时长里扣掉冻结时间；`FreezeCancel` 照旧按取消走 `OnFail`。
- **限制每个 Interest 的空间视野总数，或给政策阶段加耗时预算**：现状 `MaxViewsPerPlayer` 只数挂接，已定义未挂接的空间视野（每个都是 AOI 观察者）不受限。ApplySync 已有政策阶段预算（10-22）；ApplyAsync 与空间视野总数仍没有上限。建议加 `MaxSpatialViews`（每个 Interest）。**已实施空间视野总数（维护者 2026-10-22，`da937be2`，[O3](../release/v1.31.0-NOTES.md#o3)）**：`InterestConfig.MaxSpatialViews`，缺省 8192（D19 实测：1000 玩家各 8 个空间视野时 ApplySync 的 Flush 约半个周期），只在登记侧计数，超出 `DefineSpatialView` 返回 `ErrSpatialViewLimit`；ApplyAsync 的政策阶段耗时预算没有做。

### 4. 功能 / 优化（同一出处）

- **一个视野多个视点**：阵营视野来自几十个单位时，现在要一个单位一个空间视野。建议空间视野支持多个视点（并集），挂接数不再随单位数增长。
- **按观察者过滤可见性**：隐身 / 反隐、相位、遮挡。现状可见性只看距离（与关系视野）。建议在 AOI 评估里加观察者谓词钩子。
- **`MaxVisible` 的优先级**：现状按距离选最近 N 个，密集主城里当前目标、BOSS 可能被挤掉。建议加“必须可见”的优先级（不占或优先占 N）。
- **状态在回滚后自动 Refresh**：现状 `state` 观察者的副作用不随回滚，回滚也不通知，要等下一次改动或业务 `Refresh`。建议回滚时自动 Refresh，缩短不一致窗口。

### 5. game-demo 场景的热点（2026-10-22 晚 loadtest，维护者 2026-10-22 晚决定 A：登记并按秒爬坡摸容量）

**现象与数据**（[压测清单 §3 结果](../performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)，发版候选 `3d727c2f`，单机私有环境，100 / 200 个机器人**同一秒起跑**，单 IP 上限 512）：connect 全部成功、`object limit exceeded` 0，但成功只有 24 / 100、30 / 200。三个热点：

- **`enter_game` 写全服 World 实体、上单场景实体**：200 人时同一批登录里 `Sync_RecordEnter`（全服一个 World 实体）106 次、`MultiSync_EnterScene`（一个场景实体）110 次在请求期限内没轮到（`nest: sync canceled`），复制会话因此没打开，110 个 `scene_ready` 失败；`player_tcp_dispatch_timeout_total` 122。同一实体在 strict 持久化下约 108 TPS（[GENERATED-TCP-STRICT](../performance/GENERATED-TCP-STRICT-20261020.md)），一秒内两百次登录必然排队。
- **gift saga 步骤排队**：100 人时 41 次 `saga: step command expired before it ran`（步骤超时 5s，命令排队到期才轮到），机器人 10 秒轮询窗口内 69 个 saga 未到终态；运行结束后 132 个 saga 全部到终态。gift 原生步骤围栏期间同一玩家的心跳提交被拒 9 次（`lease-fenced step awaiting projection`，下一拍照常）。
- **`found_guild` 慢派发**：Multi 同时持公会目录实体与玩家，100 人时慢派发 27 次（最长 2.96s）、失败 3 次，200 人时慢派发 36 次、失败 6 次。

**按秒爬坡结果（2026-10-23 凌晨，[清单 §3](../performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)）**：每秒起跑 2 个机器人时 100 / 200 人全部通过（场景 p95 9.2 / 11.2s）；每秒 4 个时 56 / 100、16 / 200；每秒 10～20 个时 0～20%。瓶颈依次是 gift saga（每个机器人两次原生步骤 saga，每秒 4 个机器人 ≈ 每秒 8 个 saga，正是原生步骤单进程容量）、等奖励邮件的 effect 链、`found_guild` 的公会目录实体（每秒 2 个时已慢派发上百次）；同秒起跑才出现的 World / 场景实体超时在爬坡下没有再出现。没有跨轮的状态复用（每轮新账号）。按现有证据没有框架缺陷。

**下一步（优化方向）**：gift saga 的吞吐（原生步骤每进程约 8/s；demo 场景可以少发或合并 saga、或拉长机器人的轮询窗口）；奖励邮件 effect 链在 saga 高峰下的延迟；World 计数改成异步 / 合批、登录上地图与计数拆出关键路径；公会目录分片。机器人阈值按冒烟规模定，压测规模另给阈值。

### 6. 心跳驱动下普通业务消息的服务端 P99 上升（维护者 2026-10-22 晚决定 A：专门调查）

与“待维护者决定”第 9 项（压测工具的缺省驱动）一起看。

**数据**（[HB-DRIVEN-LOAD §4A.3](../performance/HB-DRIVEN-LOAD-20261021.md#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)，nest-game V1290 重档参数、嵌入 TCP + 场景 Sync、各 15 分钟、单样本）：帧回调（ticker）→ 心跳驱动（缺省批量投递，组 4 / 200µs）时，普通业务消息**服务端** P50 / P95 / P99 0.08 / 0.39 / 0.87 → 0.51 / 1.58 / 2.13ms；同时进程 CPU 约 2.6 → 2.1 核、short 等待峰值 2614 → 139、Sync 提交→客户端 P99 9.42 → 6.07ms；业务端到端（混合口径）P99 11.25 → 11.44ms 基本不变。同方向的微基准：[NEST-BATCH-HEARTBEAT §3G](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3g-2026-10-22-晚参数补测结果v1310-发版前压测第-1-项) 业务 p99 单目标 0.095 → 批量 0.149ms（回调是几十纳秒的内存计算）；nest-game 的心跳回调带事务、更重，差距更大。

**要查**：批量作业一次占 worker 的时长分布（组大小 × 回调耗时，时间片在每组结束后才检查）；改为组 4 / 100µs（维护者 2026-10-22 晚已定，待实施）之后在 nest-game 上复测；业务消息与批量作业的公平性（是否需要按池分开或让业务优先）；单目标 `heartbeat.delivery: dispatch` 在同参数下的对照。另一个数据点（[清单 §7 结果](../performance/V1310-PRERELEASE-20261022.md#7-可选移动-200ms5hz口径缺省已改历史口径显式-100ms)，投递对比夹具 3 轮）：登记档位从 100ms 换成 200ms（拍数减半）时，批量投递的业务 p99 反而 0.152 → 0.312ms，单目标 0.092 → 0.077ms，原因未查。

## 后续候选（未排期）

- D2 方案 B：SyncBus 框架级对账（订阅端检测缺口后向发布端请求快照）。
- D3 方案 B：Drain 的明确放弃出口（要先梳理哪些拒绝是永久的）。
- D4 方案 B：`roost add migration` 骨架；上线后重新决定是否允许在线双读期。
- D5 方案 B：框架级“迁移 Player”运维命令（上线后有真实迁服需求再评估）。
- D11 方案 C：Remote 托管与 noPersist 实体的持久定时器。
- D1 后续：独立 Gate 写出回执（快照 / 追帧随链路放慢）、客户端上传快照、增量 / 压缩快照、检查点持久化与宿主崩溃续赛、Unity / IL2CPP 实测、game-demo 战斗接快照恢复。
- D21 其余 ssr 候选：`trigger_engine`、`eca_man`、`cooldown`、`timer`；空间（`rvo`、`dynamic_obstacle`、`hexagon`、`mist` 等）、战斗（`effect`、`damage`、`aggro`）、经济（`bag`、`drop`、`gacha`、`limit`）、社交（与 `service/*` 比对补缺）。原则：按 roost 模型重写、只收与玩法无关的通用部件，每个先写方案再实施。
- 嵌入 TCP / Game Ingress：Manager 在 `RetryLater` 时跳过被拒会话（预算持续耗尽时减少整轮重试的放大），需要时再做。

## D1 Lockstep 历史的保留与过期策略

- **问题**：宿主不裁剪则内存随对局长度线性增长，裁得激进则断线久的玩家追不上，且“追不上怎么办”没有框架约定。
- **决定**：维护者 2026-10-15 选 C（自动保留 3600 帧 + 越界明确失败）；2026-10-20 方案 B（全量快照恢复）立项，设计 §12 十三项全部按推荐；同日追加恢复中被 Detach 计 `failed{detached}`、MOBA `Room` 接入（已提交门），发送错误不停房间。
- **结果**：C 见 v1.28.0（`a6376934`）；B 见 [v1.31.0 L1](../release/v1.31.0-NOTES.md#l1)。设计与实施记录 [FEATURE-2026-10-20-lockstep-snapshot-recovery](FEATURE-2026-10-20-lockstep-snapshot-recovery.md)，现行说明 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)，演练 [LOCKSTEP-SNAPSHOT-DRILL-20261020](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)。后续见上文“后续候选”。

## D2 SyncBus 有限投递的兜底

- **问题**：投递耗尽或过期后订阅方与权威可能长期不一致，框架只计数。
- **决定**：维护者 2026-10-15 选 A：SyncBus 定位为默认可丢广播，需要最终一致的业务做“丢失检测 → 拉快照对账”；B（框架级对账）为候选，C 不采用。
- **结果**：`f385f8c5`，[guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)，模板 `syncbus.Example_reconcileAfterLoss`。

## D3 Entity Sync `Drain` 遇到永久拒绝的出口

- **问题**：停机时被永久拒绝的条目让 Drain 等到 ctx 超时，调用方看不出卡在哪。
- **决定**：维护者 2026-10-15 选 A（诊断增强，不放弃条目）。
- **结果**：`b86a1286`，`*entitysync.DrainStalledError`，[guide/04-sync §4](../framework/guide/04-sync.md#停机排空drain卡住时看什么)。

## D4 业务 schema 离线迁移的模板 / 流程

- **问题**：DAO 只接受当前 schema，业务第一次改 schema 时没有端到端流程。
- **决定**：维护者 2026-10-15 选 A（手册 + demo 示例）；迁移离线停写，上线后重新决定是否允许在线双读期。
- **结果**：`12aa1a03`，[SCHEMA-MIGRATION](../framework/SCHEMA-MIGRATION.md)，game-demo `cmd/schemamigrate`；2026-10-20 夜间重做演练（[NIGHT §12](../performance/NIGHT-20261020.md#12-第-12-项schema-迁移演练)）。

## D5 静态 Player 跨机迁移的参考流程

- **问题**：扩缩容 / 迁服时各业务自己设计停写、排空、切换，顺序错会丢写或双写。
- **决定**：维护者 2026-10-15 选 A（参考流程 + 检查工具），框架级命令上线后评估。
- **结果**：`ec96b74f`，[PLAYER-MIGRATION](../framework/PLAYER-MIGRATION.md)，game-demo `cmd/sidmove`；2026-10-20 夜间重做演练（[NIGHT §13](../performance/NIGHT-20261020.md#13-第-13-项player-跨机演练)）。

## D6 玩家看自己状态（Entity Sync 自身视图）的接入骨架

- **问题**：新用户想让客户端收到自己的金币与道具变化，没有可走的路。
- **决定**：维护者 2026-10-16 折中：并发生命周期放 core（`policy.SelfView` + `wiring/selfsync`），生成器只写可见接线（`access.player.self_sync`），C# 不做。2026-10-17 默认 packer。**2026-10-22：编码失败丢会话即断开该玩家，10-17 对编码失败定的“不关连接、靠滞留指标”作废**（见 D20 P3 后续）。
- **结果**：`fc0a8315`、`9aa7be92`；[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)、[impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)。

## D7 实体不存在时给客户端的稳定错误码与首登步骤

- **问题**：从未登录的玩家直接发业务请求得到 `code=1 server error` 并记 ERROR。
- **决定**：维护者 2026-10-16 选 C（框架码 + first-business 流程加 EnterGame）。
- **结果**：`e7e59298`，`errcode.CodeEntityNotFound = 2`；删过档由 D10 的 `entity_deleted = 3` 区分。

## D8 生成的服务包名大写（`package Game`）

- **问题**：生成的服务包名不合 Go 惯例，已有工程又不能只改生成器。
- **决定**：维护者 2026-10-16 选 C（新工程小写、已有工程按目录现有包名）。
- **结果**：`bac32cd2`。

## D9 DAO 库名不随配置、`_id` 是编码后的 EntityID

- **问题**：DAO 库名是编译期常量，同一 Mongo 上两个工程写进同一业务库；文档 `_id` 是编码后的 EntityID。
- **决定**：维护者 2026-10-16 选 B（骨架 `db=<工程名>` + `roost id entity`）；2026-10-20 修订版 A（代码写逻辑名、部署映射物理库），同日：回调与配置冲突拒绝启动（选 B）、没有映射回退逻辑名并告警（选 A）；2026-10-21 收紧：没有映射表时 DataEngine 与 Remote 两边都拒绝。
- **结果**：B `a068be37`（v1.28.0）；修订版 A 见 [v1.31.0 D1](../release/v1.31.0-NOTES.md#d1)，[guide/03 §3](../framework/guide/03-dataengine.md#3-投影与幂等)、[PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)。

## D10 删档后用同一 ID 重建

- **问题**：删档后同 ID 重建让投影版本冲突、进程 fail-stop、重启起不来。
- **决定**：维护者 2026-10-17 选 B：删档后该 ID 不可再用，范围只守 GetOrCreate；直接 Create 靠“必须用 id-gen 新 ID”的约束。
- **结果**：`572d87c2`；处置见 [KNOWN-LIMITS“删档后同 ID 重建”](KNOWN-LIMITS.md#删档后同-id-重建d10)。

## D11 按实体的持久定时器接入

- **问题**：Buff 到期、建造完成等按实体的到点事务没有框架接入。
- **决定**：维护者 2026-10-17 选 B（`roost add timer --entity`），离线语义：框架不追帧、加载后只补触发一次；2026-10-18 四项按推荐（卸载窗口接受、不做离线准点驱动、去掉条目不自动回收、多一个 Mod 的 3s 保底接受）；game-demo 保持手写参考实现。
- **结果**：`a2b4f4d8`、`5581deeb`；[guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器)、[impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动)。

## D12 Saga 原生实体步骤与事务性 effect 消费者的生成支持

- **问题**：Saga 步骤要改实体时只能走 Mongo 步骤，没有经 Nest + DataEngine 的原生步骤与生成支持。
- **决定**：维护者 2026-10-18 选 A（原生步骤）与 B（生成器）；原生步骤单进程容量按 A 接受并文档化（量大按 sid 分片）；checkpoint 缺省间隔保持 20ms（10-19）。
- **结果**：v1.28.0；容量口径 2026-10-21 按 15 分钟补测更新（[guide/06 §5](../framework/guide/06-saga.md#5-接入与运维)）。

## D13 DAO 持久键的来源与水合时的键集合校验

- **问题**：改 Go 字段名会静默丢老数据。
- **决定**：维护者 2026-10-19：定义里的 `bson` 名就是持久键（缺省 snake_case），改字段生成期 + 加载期 WARN、不拒绝，旧键不加载、整文档写回即删除（同日撤销方案 A 与清理入口）。
- **结果**：v1.28.0；业务约束见 [SCHEMA-MIGRATION §1](../framework/SCHEMA-MIGRATION.md)。

## D14 AOI 观察者改为 playerId、视野半径按观察者配置

- **问题**：observer 名义上是实体 ID、实际只有玩家；视野半径只有全局一个。
- **决定**：维护者 2026-10-20 选 A1 + B1：观察者是玩家（带自身实体）、入口直接改两个 ID、删除 `InterestConfig.Session`、客户端 LOD 请求带层级与视野大小（服务端夹取量化、限速、直接生效）；评审补“只看不被看”入口。
- **结果**：[v1.31.0 S1](../release/v1.31.0-NOTES.md#s1)，[guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)。

## D15 合服（把一个服并入另一个服）

- **问题**：迁服只搬同一个 sid，没有把一个服并入另一个服的能力（slot 键冲突、没有改绑接口、一个进程只认一个 sid）。
- **决定**：维护者 2026-10-20 选别名合服，设计 §10 八项按推荐；2026-10-21 实施后三项按推荐保留（启动自检 fail-closed、saga 终态检查整个存储、demo 合服角色带原服范围拉邮件）。
- **结果**：[v1.31.0 D3](../release/v1.31.0-NOTES.md#d3)，设计与实施 [FEATURE-2026-10-20-server-merge-alias](FEATURE-2026-10-20-server-merge-alias.md)，runbook [PLAYER-MIGRATION §6](../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)，边界 [KNOWN-LIMITS“合服”](KNOWN-LIMITS.md#合服d15)。按类型过滤 saga 检查见“待维护者决定”第 8 项。

## D16 单个实体 / 玩家的运维入口

- **问题**：没有踢下线、卸载等单实体运维入口；运行期直接改权威集合会被覆盖或 fence。
- **决定**：维护者 2026-10-20 选 C：框架只做 `entity.kick` / `entity.unload`，走现有 admin 鉴权与审计；改档 / 恢复按业务 GM handler 与离线流程。实施后四项按推荐（kick 不封禁、unload 缺省 5s、kind 用数值、在途事务等它结束）。
- **结果**：[v1.31.0 D2](../release/v1.31.0-NOTES.md#d2)，[guide/03 §6](../framework/guide/03-dataengine.md#单实体运维命令d16)。

## D17 全服邮件的内置投递策略

- **问题**：`AudienceBroadcast` 要业务自己维护广播索引与每玩家水位。
- **决定**：维护者 2026-10-20 选 A：全局邮件一个 ID、个人邮箱拉取合并，七条细则按推荐；实施后：合并副本随全局邮件过期，`RoleCreatedAtClaim` 放 account、撤回只在 Mail RPC。
- **结果**：[v1.31.0 D4](../release/v1.31.0-NOTES.md#d4)，[guide/09 §1B](../framework/guide/09-services.md#1b-mail-全服邮件)。

## D18 业务时区与“业务日”边界

- **问题**：没有业务时区配置与业务日工具，容器内 `time.Local` 是 UTC、开发机是本地时区。
- **决定**：维护者 2026-10-20 选 A，缺省 UTC。
- **结果**：[v1.31.0 T3](../release/v1.31.0-NOTES.md#t3)，[guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)。

## D19 视野（View）：逻辑 ID、空间 / 关系两类、玩家与视野多对多

- **问题**：一个玩家会有多个视野（召唤物、眼位、联盟），联盟同步要能归到同一模型，视野 ID 不能绑死实体。
- **决定**：维护者 2026-10-20：int64 逻辑视野 ID、空间 / 关系两类、多对多、同步层级取高或并集、`MaxVisible` 按视野、视野操作不进 Nest、删除 `RelationSource`；2026-10-21 三项小决定按推荐保留（跨政策仍按优先级取一个视图、新增 `MaxPlayerBlocks`、关系视野不排除自己）。
- **结果**：[v1.31.0 S2](../release/v1.31.0-NOTES.md#s2)，[impl/04-sync §2E](../framework/impl/04-sync.md#2e-interest-的视野d19在-d14-上扩展)。

### 排队事实异步应用（2026-10-21，维护者 2026-10-21 给出方向）

16 个额外空间视野时 Nest 模式失守（[NIGHT §14'.2](../performance/NIGHT-20261020.md#142-sync-aoi服务端--客户端两个进程)）：根因一是 AOI 批次的完整重评（改为只评估移动过的目标），根因二是排队事实一次性挤进 Flush。维护者给出方向：排队事实由独立应用线程分批应用、Flush 只结算。实施后剩下约一个同步周期的差距，维护者选 C（接受），并把它当作模式问题处理——即下一节的 `ApplyAsync`。数据见 [NIGHT §14'.2a](../performance/NIGHT-20261020.md#142a-根因与修复2026-10-21)。

### 两种应用模式（2026-10-21，维护者决定）

维护者 2026-10-21：每玩家额外视野 8 个以内与以上分两种模式，由业务按游戏类型选择；缺省 `ApplySync`（政策阶段同步应用，提交后下一次 Flush 可见，上限 9 含关系视野），`ApplyAsync` 为应用线程；“提交后立即 Flush 看不到、要用 Drain”只在 `ApplyAsync` 下。阈值 8 的实测依据（原场景 ApplySync Flush 4 / 8 / 16 视野 15 / 25 / 46ms）与选择指引见 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)，原始样本 [benchmarks/d19-multiview-20261021](../performance/benchmarks/d19-multiview-20261021/README.md)。

2026-10-21 起 Interest 改为单写者（[方案](REFACTOR-2026-10-21-interest-single-writer.md)）；2026-10-22 维护者两项决定 A：ApplySync 预算把结算也算进去（方案 §12）、一步只在日志的前缀边界停（纯前缀切开，方案 §11.2）。见 [v1.31.0 S4](../release/v1.31.0-NOTES.md#s4)。

## D20 移动由实体自己的心跳驱动：MapBasic / Move 组件 + actionflow 控制

- **问题**：压测里的移动由一个全局循环驱动、不像真实游戏；框架没有按实体的心跳。
- **决定（维护者 2026-10-21）**：组件与心跳框架放 `gameplay`，心跳就是在实体所在 Nest 池上执行的消息；不合并、跳拍打 Warn、ssr 式固定档位、全实体支持、HB 不持久、entitytimer 不改、房间 / 场景用自己的心跳；压测后：按档分环 10ms、1 个驱动；P1 后：`Elapsed` 上限 `max(1s, 档位)`、卡住兜底 10s 起翻倍、Remote 共享模式不允许挂载且回到独占不自动恢复；P2：移动由 action 驱动、所有控制实体行为的都走 action、runner 不随事务回滚由业务保证；P5：心跳只是技能 / Buff 的驱动方式之一；选项 C：先合入缺省关闭，**2026-10-22 决定 B：缺省批量投递、组 8 → 4，合并参数当晚补测**。
- **结果**：[v1.31.0 H1～H5、G1、M1、P1](../release/v1.31.0-NOTES.md#h1)；用法 [guide/10-time §4A～§4E](../framework/guide/10-time.md#4a-实体心跳周期推进)，契约 [impl/10-time §2B～§2G](../framework/impl/10-time.md#2b-实体心跳)；结构与投递方式的压测依据 [HB-STRUCTURES-20261021](../performance/HB-STRUCTURES-20261021.md)、[NEST-BATCH-HEARTBEAT-20261021](../performance/NEST-BATCH-HEARTBEAT-20261021.md)。下一阶段：移动缺省档位与无观察者降档（见[优化项](#优化项下一阶段)第 1 组）。

### P1 实施结果（2026-10-21）

`gameplay/heartbeat` + `wiring/heartbeat`；变异 M1～M11 原文在 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)。决定 ④（Remote 共享模式不允许挂载）随后实施（`96724b6a`、`d287031f`）。

### 真实 Remote 共享模式切换验证（2026-10-21）

经真实 `remoteentity.Manager`（进程内替身与真实 Redis + Mongo + NATS）验证独占 → 共享 → 独占、共享模式下迁移、卸载重载；修两处恢复入口（任务心跳、`SetStrategy` 同一策略）。见 [v1.31.0 H5](../release/v1.31.0-NOTES.md#h5)。`MapBasic.Sync` 维护者 2026-10-21 晚决定不加。

### P2 实施结果（2026-10-21）

`gameplay/movement` + `actionflow.BeatRunner`，按维护者决定改为 action 驱动（`382eb10c`）；变异 M1～M19 原文在 [impl/10-time §2C](../framework/impl/10-time.md#2c-移动与动作的心跳驱动)。

### P3 实施结果（2026-10-21）

game-demo 接入（[WALKTHROUGH](../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20)）；2026-10-22 冒烟（dev-smoke 2 / 2、loadtest 20 / 20）后续：主视野 `MaxVisible` 64、`scene_alive`、200 人里 72 个 connect 失败是单 IP 上限；**维护者 2026-10-22 决定编码失败就关闭连接**（[v1.31.0 S5](../release/v1.31.0-NOTES.md#s5)）。100 / 200 人 loadtest 在今晚压测清单里。**2026-10-22 晚结果**（[清单 §3](../performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)）：单 IP 上限 512 后 connect 全部成功、`object limit exceeded` 0（`MaxVisible` 64 的目的达成）；两轮 rc≠0（成功 24 / 100、30 / 200），失败来自同秒起跑打在 World / 场景实体、公会目录与 gift saga 上，尚未确认，下一步建议按秒爬坡复测。

### P4 实施结果（2026-10-21）

压测工具的心跳驱动（sync-aoi `-move-driver`、nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER`、§7 微基准），只交付代码；对比与缺省的选择在今晚压测清单与“待维护者决定”第 9 项。见 [HB-DRIVEN-LOAD-20261021](../performance/HB-DRIVEN-LOAD-20261021.md)。2026-10-22 晚正式对比已做（§4A）：心跳模式零跳拍、投递 = 执行，nest-game 心跳路径 `verified=true`；缺省仍待第 9 项决定。

### P5 实施结果（2026-10-21）

技能与 Buff 由心跳驱动；四项按推荐（放 `combatcomponent`、Buff 固定粒度、超过追帧上限一次跳过、`TickBuffs` 保留导出）。

### 驱动接口实施结果（2026-10-21）

维护者 2026-10-21：心跳只是技能 / Buff 的驱动方式之一，skill 只用接口（`skill.Driver` / `Stepper`、`DriveBuffs`）。变异 28 项原文在 [impl/10-time §2G](../framework/impl/10-time.md#2g-技能与-buff-的推进与驱动)。是否给 `Driver` 加 `Close()` 见“待维护者决定”第 3 项。

### 生成器接入实施结果（2026-10-21）

`roost add heartbeat` / `behavior` / `timedomain`；维护者 2026-10-21 晚按推荐：heartbeat Mod 按需装配、`scripts/test-heartbeat-generated.sh` 不进 pretag。见 [v1.31.0 G1](../release/v1.31.0-NOTES.md#g1)、[impl/12-codegen §2E](../framework/impl/12-codegen.md#2e-roost-add-heartbeat--behavior--timedomain)。

### 维护者 2026-10-21 晚：各期待拍板项的决定

- Remote 托管实体理论上不应挂 Move 与心跳组件：先记录方向（“待维护者决定”第 6 项），`MapBasic.Sync` 不加。
- 技能 / Buff 的驱动方式：心跳只是之一，skill 只用接口（已实施）。
- 其余按推荐：P5 四项；选项 C 批大小 64、时间片 200µs、`DispatchTryBatch` 对业务导出（文档注明目前只有心跳使用）；P4 压测工具缺省仍是全局循环，发版前对比后再定；生成器按需装配、脚本不进 pretag；P3 场景时间经关系视野下发（扇出上限 65536）、玩家位置每 10 秒落库、不在卸载前补 `LeaveScene`。

## D21 吸收 ssr 的优秀组件进 `gameplay`

- **问题**：D20 参考了 ssr 的 heartbeat / action / move / map_basic，维护者希望把 ssr 里通用的组件也收进 `gameplay`。
- **原则**：按 roost 的模型重写、不复制 ssr 代码；只收与玩法无关的通用部件；控制实体行为走 actionflow、周期推进走心跳；每个组件先写方案、确认后实施。
- **调研一（行为控制）决定**：维护者 2026-10-21 13 条全部按推荐（`Behavior` 默认装配、`Replacer` + `Conflict`、Start 即完成、暂停 / 打断计数冻结、任务心跳与计划 Timeout / Retry、AI 事件 / 分阶段 / `FreezeFor` / 独立 `"ai"` 心跳、新增 `gameplay/state`（来源 + 派生、DAO 随事务回滚、不内建时长、Buff → state 派生）、按 D21-1 → D21-5 分期）；`Agent` 更名 `Behavior`；实施中 4 项小决定按推荐（`FreezeCancel` 保留排队、门控回滚不补发、取消的步骤不重试、动作指标缺省关闭）。
- **结果**：[v1.31.0 A1～A5](../release/v1.31.0-NOTES.md#a1)。其余 ssr 候选见“后续候选”。

#### 6. 调研一 · 实施

D21-1 `86c6dc1c`、D21-2 `849031fa`、D21-3 `575f216a`、D21-4 `79138636`、D21-5 `e95916f7`、更名 `08c734b1`；变异 D1～D12、S1～S12、B1～B5、H1～H12、E1～E8 原文在 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态)。调研全文（ssr 模型、能力对比、引入方案）见本篇开头的固定提交。

### D20 与 D21 审查修复（2026-10-21）

学习分支 `learn/main-review-20261021` 第 2、3、4、7 条与慢派发 trace 的数据竞争：`heartbeat.Reg.Close` 与上层 `Close(reason)`（换场景 Close + 重建，不做 `SetTimeDomain`：旧域里记下的时刻不能拿到新域里比较）、被抢占组的排队项在抢占方结束时推进（保留排队）、`StateDao` 在 rollback=state 下未快照时返回错误、驱动摘除已移除实例、`Behavior.OnDestroy`、慢派发 trace 不反射打印参数。第 1、5、6 条由 Interest 单写者解决，第 8～10 条另修。见 [v1.31.0 IMPLEMENTATION A](../release/v1.31.0-IMPLEMENTATION.md#a-行为控制)。

## D22 业务时间取自 ctx、支持变速

- **问题**：业务里现读时钟，同一事务里可能读到不同时刻；endpoint 的业务时间是收到请求的时刻；没有变速。
- **决定**：维护者 2026-10-21 范围按 ssr 收窄：全局不做变速（D22-2 取消，`time.logic_offset` 保持现状），只做 D22-1（业务时间唯一入口 `fctx.Now()`，每条消息 / 每一拍冻结一次；endpoint 的业务时间参数执行时填）与场景时间域（倍率 [0, 100]、允许 0；心跳调度按真实时间、`Beat.Now` / `Elapsed` 用域时间；entitytimer 不接时间域）。
- **结果**：[v1.31.0 T1、T2](../release/v1.31.0-NOTES.md#t1)，[guide/10-time §1A、§4D](../framework/guide/10-time.md#1a-业务时间一律取自-ctxd22-1)，[impl/10-time §2E、§2F](../framework/impl/10-time.md#2e-业务时间取自-ctxd22-1)。
