# 已知限制里已修复条目的原文（2026-11-13 移出）

2026-11-13 整理 [KNOWN-LIMITS](../../KNOWN-LIMITS.md) 时，按维护者要求把“框架本身的限制”和“遗留问题”分开，已修复、只留作证据或决策出处的段落原样移到这里（链接已按新位置改写，文字未改）。KNOWN-LIMITS 第三部分每条一行结论并链接到下面对应的小节；修复后仍有效的边界留在 KNOWN-LIMITS 前两部分。段落里的“本条 / 下一节 / 上一类”指的是移出前在 KNOWN-LIMITS 里的位置。

## 2026-11-13：本地实体起服预加载与本地实体 DAO 缺省 dbscope=sid：combat 与 dungeon 的组件 DAO

已修：**combat 与 dungeon 的组件 DAO 原来是 global**（`combatcomponent.CombatDao`、`level.LevelDAO` 没有 `DbScope()`），挂在 sid 实体上时一个实体的文档分在两个库里，不能预加载、换服不搬。2026-11-13 改为跟随宿主：`NewCombatDaoIn` / `NewLevelDAOIn` 带上宿主主 DAO 的 `DbScope()`，提交的写入键带同一个范围，模块模板按此构造；注册校验拒绝范围不一致的组合（上一类第 4 条）。修前 `TestValidateEntityRegistryRejectsALocalKindWhoseDaosMixScopes` 报 `ValidateEntityRegistry accepted local kind 145 whose component DAO is dbscope=global under a dbscope=sid primary DAO`；生成工程里把 combat / dungeon 的 DAO 改回旧构造，`registry.RegisterAll` 报 `kind=1 dao[1] *combatcomponent.CombatDao collection="combat_state" uses dbscope=global but the primary DAO dao[0] collection="player" uses dbscope=sid`（dungeon 同理）。

## 2026-11-11：地形审查 RV-20261110-01～04（已修复，未发版）留下的边界

[审查报告](https://github.com/tjbdwanghaibo/roost-core/blob/fix/core-game-server-review/docs/maintenance/TERRAIN-REVIEW-20261110.md)（固定审查分支）登记的四项已在 main 修复，现象、根因与修前的红见 CHANGELOG `[Unreleased]`，状态见 [地形进度总表 §2](../../TERRAIN-STATUS.md#2-缺陷修复均已关闭)。留下的限制：

## 2026-11-10：开地形场景同步 128 个对象时每观察者的下行（已重算：增量字节压缩后约 18.6KB/s，在 50KB/s 以内）

维护者 2026-11-10 口径“同步的 entity 数量一般是 128 左右”实施后（[地形方案 §29](../../FEATURE-2026-11-08-editable-terrain.md#29-游戏尺度参数视野-32m单进程-2048m同步-128-个2026-11-10)），开地形的普通场景主视野“最近 128 个”。`TestMainViewMeasureMovingCrowdBytes`（帧 payload、不含线上消息头，KB = 1024 字节，同机同用例）：

| 同时在动的可见对象 | 压缩前（帧协议 1 / 信封 v2） | 压缩后（帧协议 2 / 信封 v3） |
| --- | ---: | ---: |
| 64 | 119 字节 / 对象 / 拍，37.3KB/s | 30.4 字节，9.5KB/s |
| 128 | 118 字节，**73.5KB/s（超预算）** | 29.7 字节，**18.6KB/s** |

结论：增量字节压缩（[证据](../sync-delta-bytes-20261110/README.md)、[方案 §29.6](../../FEATURE-2026-11-08-editable-terrain.md#296-每观察者下行增量字节压缩后-128-个同时动约-186kbs在-50kbs-以内)）之后 128 个同时动约为预算的 37%，按每对象约 30 字节，同时在动的可见对象到约 340 个才到 50KB/s，超过主视野名额 128 与会话上限 164。本条不再需要维护者决定；降频 / LOD 不做。剩余的省字节候选与待决定项见下一条。

## 2026-11-10：场景主视野改用 near / team 视图

- **场景主视野用的是缺省视图 `"*"`（维护者 2026-11-10：这是同步视图的问题，由业务层处理 DAO 的视图；已实施，见本条末）**：game-demo 的 `policy.InterestConfig` 没配 Bands，主视野 `DefineRelationView(sceneView, SyncProfile{})`，附近玩家收到的是 Player 的全部同步字段，包括别人的 exp、gold、items、attr_base、attr_final（[证据 §6](../sync-delta-bytes-20261110/README.md#6-每个同步字段是否需要发给其他客户端demo-player--monster) 逐字段列了是否需要）。这既是多发字节（玩家全量 78 字节里约一半），也是私有字段对旁人可见。选项：A（推荐）主视野改用 packer 里已有的 `near` 视图（`Name, Level, PosX, PosY, SceneID, Facing, Equipment, GuildID`），必要时加上 `cell_hp` / `cell_max_hp`；代价是改 `scene.go.tmpl` 的主视野配置与相关 demo 用例的字段断言。B 保持现状，只在文档里说明 demo 不做字段权限。本轮没改：`scene.go.tmpl` 同时在做主视野尺度的改动。**已实施（2026-11-10，未发版）**：按 A，在业务层（game-demo 的 packer 视图与场景配置）解决，框架已有的 `BandLayer` / `ViewSets` / `ProfilePriorities` 够用、未改框架。主视野给 `near`（名字、等级、位置、场景、朝向、外观、公会、`cell_hp` / `cell_max_hp`），队伍关系视野给新的 `team`（字段同 near，优先级更高，队友进出主视野不换视图），本人经 self 仍是缺省视图；Monster / Building 声明同名视图（Interest 要求每类实体声明每个用到的层级）。玩家进入别人视野 117 → 85 字节，走一步不变。“字段 × 视图”表见 [demo/README“同步视图”](../../../../demo/README.md#同步视图谁看见哪些字段)，红绿与变异见[证据](../sync-views-codec-20261110/README.md)。

## 2026-11-10：同一视图换 Codec 时改发全量

- **“同一视图的 packer 始终用同一个 Codec”是前提，没有在服务端强制（维护者 2026-11-10：服务端加校验；已实施，见本条末）**：增量信封不带 Codec，接收端沿用全量的。仓库内的 packer（生成的 DAO packer、self_sync、测试 packer）都满足；自定义 packer 若在同一视图里换 Codec，客户端会按全量的 Codec 去解增量载荷，结果取决于两种格式能否互相解析（尚未确认会不会静默错用）。需要时可在 Manager 按（subject, 视图）记下全量的 Codec，增量不符就改发全量。**已实施（2026-11-10，未发版）**：确认会静默错用（修前用例原文 `session 1 read "delta@2" as codec 1`）。Manager 按（会话, subject）记下会话持有的基线 Codec（只随全量变，空载荷增量不改，与 `Decoder` 相同），非空增量的 Codec 不同就不发、这个会话转回待快照，下一次 Flush 收全量（Reason = Resync）；计 `entitysync_codec_resyncs_total{namespace}`、`ManagerStats.CodecResyncs`，限频 Warn。没有选“打包 / 注册时拒绝”：换 Codec 本身合法，拒绝会让 subject 的同步停住。

## 2026-11-10：建筑进场景主视野后的 demo 尺度（维护者 2026-11-10：主视野已按推荐放大，见地形方案 §28；偶发已定位修复，见本条末段）

建筑的订阅改为与怪相同的场景主视野之后（[方案 §27.10](../../FEATURE-2026-11-08-editable-terrain.md#2710-订阅改为场景主视野维护者-2026-11-10)），demo 主视野的尺度（进入 1.2m、离开 1.5m、客户端最大 2.7m，按占地中心）对 3～12m 的建筑明显太小：站在建筑旁边紧挨着的节点上，缺省视野四种建筑都看不见，最大视野只看得见 hut 与 tower，keep 要走进占地里才看得见。另外 `policy.AOI` 的 `MaxVisible` 是近似语义，被上限忽略的目标在它下一次移动时才重新参选——建筑从不移动，视野满时后放的建筑要等看的人移动才进来（会话级超额降级 D17 的放回不受影响）。主视野尺度、按类型名额、按占地范围、静止目标重新参选四项待维护者确认，本轮未改主视野参数；测量见[证据](../building-main-interest-20261110/README.md)。

**主视野尺度（2026-11-10 已实施，已合入 main、随 v1.43.0 发布；取值随后按游戏尺度口径由 §29 重定为 32m / 35.2m / 最大 64m、最近 128 个，下面的数字是 §28 当时的，[方案 §28](../../FEATURE-2026-11-08-editable-terrain.md#28-开地形场景的主视野尺度2026-11-10)、[证据](../terrain-main-view-scale-20261110/README.md)）**：开地形的普通场景主视野改为进入 30m / 离开 33m / 客户端最大 60m（离开 63m），AOI 块边长 32m；站在建筑旁边看得见四种建筑。不开地形的场景与分区场景仍是 demo 尺度。留下的边界：① 客户端开到 60m 时，密集处（建筑每 100m² 一座）最近 64 个约在 33m 以内、约 41 座是建筑，33m 外的玩家 / 怪被挤掉（B 按推荐不分类型）；② 下行上限由 64 个名额与 200ms 一步决定，约 35～46KB/s，接近 50KB/s 预算；③ 1000 人时每次复制缺省视野约 3.2ms、人人最大视野约 12ms（重负载机器、交替 6 轮）；④ 分区场景未放大（要连同 witness / Ghost 带一起改，P5）；⑤ demo 的移动速度、巡逻范围没有随视野放大；静止目标被上限忽略后要等看的人移动才重新参选（D）不变。七项取舍列在方案 §28.8，维护者 2026-11-10 给出游戏尺度口径后由 §29 取代（§29.8 确认按推荐）。

**尚未确认**：`TestBuildingsReachAnotherClientOverRealTCP` 在重负载下偶发“第二次改参数以全量（132 字节）发出、不是增量”——同一次里第一次改参数是增量（94 字节），即“放置时的脏位让登记后第一次改动走重同步全量”（§27.3 末条）挪到了第二次改动上；客户端内容与服务端一致，只是编码。观察：新代码 3 次（整包 `-race` 与生成器测试并跑时 1 次、`-race -count=25 -cpu 1,2` 1/50、与整包 `-race` 并跑的 400 次里 1 次），改动前的代码在同样负载下 0/400、单独跑 0/200；新代码单独跑 0/380。可能是主视野那个 Interest 的结算时机让快照与脏位的先后不同（entitysync 的 `preserveDirty`），没有定位到哪一次快照；用例的断言没有放宽。

**结论（2026-11-10 追加，已证实；已修复，随 v1.43.0 发布）**：不是主视野的结算时机，也不是 `preserveDirty`（它只用于周期类型）。根因在 on_change 冻结：`SubjectSyncState.FreezeSyncViews` 在冻结槽已有内容或同一 subject 的捕获在途时一律升成全量（Resync），在途捕获提交后这份冻结也不释放。用例里判重、什么都不写的第二次 `SetParams` 落在后台复制循环推送第一次改动、尚未提交版本的窗口里时，留下一份过时冻结，第二次改动因“已有冻结”升成全量；落在第一次改动的冻结尚未被取走时，第一次改动升成全量（§27.3 末条记的现象）。在 Flush 推送与提交之间加 30ms 诊断延时可稳定复现原文，诊断日志点名了“在途”与“已有冻结”两步。修法：冻结不再因忙升全量，过时的增量由已有的基线校验拒绝复用、在锁内按新基线重新捕获；同代际的过时冻结随提交释放。entitysync 层有确定性的红绿回归（Push 钩子，不靠计时），game-demo 用例收紧为两次改参数都必须是增量。复现、前后对照与变异见[证据](../entitysync-resync-flake-20261110/README.md)、CHANGELOG `[Unreleased]`。

**频率原因（2026-11-13 从 KNOWN-LIMITS“同步：尚未确认的偶发与疑点”移入）**：

- **建筑进主视野后偶发频率变高的原因**（根因已修复，见本节上一段）：建筑进主视野之后频率变高的具体原因尚未确认（只影响时序窗口的宽窄，不影响根因结论）。

## 2026-11-09：类型调度框架与 demo 生产接线已实现：原两项去重问题

当时待实施的原两项去重问题（T01 / T02）已修复，客户端岸线的 C# 算法层已实现（Unity 表现未做），均随 v1.43.0 发布，见[地形进度总表](../../TERRAIN-STATUS.md)。

## 2026-11-10：cellworld 按经过时间积分的子步截断（已证实；维护者 2026-11-10 选定按千分位整数累计零头；已修复，未发版）

`gameplay/cellworld` `integrate` 每个子步按 `VX*dt/1s` 向零截断。按 `Beat.Elapsed` 积分（`dcd0a0c4`）之后 dt 不再恒为 `Period`，每拍平均少走约 0.5 个单位：400/s 实测 389～391/s（约 2.5%），速度越低比例越大；`VX*Period` 本来就不是整数的低速（如 10/s、50ms 档每拍 0.5）在修改前后都会被截成 0、不动。只影响位置精度，不影响交接几何（冻结点仍在 H + 一步以内）。可选：A. 在角色状态里带一个不足一个单位的余量（DAO 字段，随事务回滚；推荐，精确但加一个字段）；B. 接受并在 cellworld README 写明“速度按整数单位每子步截断”。证据见 [cellworld 整包偶发失败](../cellworld-realclock-flakes-20261110/README.md)。

**已修复（未发版，2026-11-10）**：按 A 实施。每子步位移按千分位整数算，不足 1 单位的零头存在 `ActorState.FracX` / `FracY`（随 DAO 回滚、随交接 Offer 带走，线上格式 v8），位置仍是整数单位；400/s 与 10/s 在不规则拍长下与 速度 × 时间 相差不到 1 单位（`TestIntegrationKeepsTheSubUnitRemainder` 修前红、修后绿）。

## 2026-11-10：无缝演练最后一步（普通场景机器人压测阈值）时过时不过（尚未确认）

`scripts/test-seamless-demo.sh`（不开地形）的 14 个演练步骤稳定通过，最后在普通场景 sid 1003 上跑 2 个机器人（`cmd/loadtest`，阈值 p95 ≤ 16s）：2026-11-09 本分支与 main `3fb089d9` 都失败（16.45s；main 那次是公会 saga 等待超时、error_rate 1），2026-11-10 同机未改动的检出上失败（p95 19.48s）、同一天开地形演练分支上通过（13.54s）（[证据](../terrain-partition-drill-20261110/README.md#原演练不开地形不退化)）。只有 2 个样本、机器有后台负载，没有区分是阈值过紧、同机多进程争用还是 saga 等待的问题；与地形、分区无关。开地形的演练（`ROOST_SEAMLESS_TERRAIN=1`）不跑这一步。

**定位结论（2026-11-10 追加，已证实原因；未修，待维护者决定）**：不是性能退化。本机复现 p95 27.85s（2 个机器人都成功），所有请求 p50 ≤ 26ms；时间全在 saga 轮询上（每机器人 `gift_status` 47 次、`guild_op_status` 48 次，约 23.8s）。每笔 saga 约 3.8s（JetStream 上的终态记录：礼物完成 3.80s、补偿 7.56s、联盟领奖 3.82s）：演练有 5 个 game 进程共用同一份原生步骤，步骤落到非发送者 sid 的进程时 `gift_saga.go` `admit` 先 `handOver`、再拒绝这次投递，结果要等重投落到发送者进程才发布；日志里每个步骤被另外 4 个进程各拒一次（t、+0.25、+0.75、+1.75s），第 5 次投递在 +3.75s 到 sid 1003，正是 `nak_backoff_min` 250ms 起翻倍的退避。16s 阈值的依据是“一到两个 game 进程”，这个拓扑下 6 个原生步骤约 +22s，必然超；时过时不过推断取决于 sid 1003 在投递轮转中的位置（尚未确认），公会扣费步骤推断是同一机制（无日志，尚未确认）。可选：A. 演练脚本对这一步按拓扑放宽阈值或只起一个 game 进程跑它（只改测试，最小；推荐先做，不掩盖 B）；B. 产品侧让“不是我的步骤”更快到主人：路由拒绝用短的固定 NAK 延迟、或 `handOver` 后由主人直接发布结果、或按 sid 分步骤消费者（多 game 进程部署的 saga 延迟都会受益，需设计）。见[证据](../perf-terrain-acceptance-20261110/README.md#第-2-轮无缝演练最后一步的-p95已定位)。

**处置（2026-11-10，维护者定方案 A，已实施）**：只改演练脚本——这一步的 `cmd/loadtest` 传 `-max-p95 40`（缺省 16s 按一两个游戏进程定；5 进程拓扑下 saga 步骤被非属主进程各拒一次、按 250ms 倍增重投，约多出 22s；40s = 健康上沿 10s + 22.5s 再留约 25% 余量），理由写在 `scripts/test-seamless-demo.sh` 注释里。复跑（私有真实依赖）：14 个演练步骤 ok，最后一步 p95 29.05s，通过。产品侧的路由重投加速（方案 B）没做。

**处置（2026-11-11，维护者改定方案 ③ 并实施，未发版）**：saga 步骤按 sid 确切路由——只能在某个 sid 上执行的步骤声明为路由步骤（`Step.Routed`，`roost add saga -routed`），发起方把属主 sid 写进 `StartRequest.Route`，协调器把命令发到 `ev.to.<sid>.<topic>`，只有那个 sid 的 durable（`<group>-<step>-<sid>`）收到；非属主进程收不到，结果一次投递就发出，不再受 nak 退避控制。属主核对仍在：玩家不绑定本服（或换服中）时退回协调器（`saga.BounceStep`，不执行、零写入，可带新地址），没有 sid 的载荷拒收（`saga.RejectStep`）。demo 赠礼与公会玩家步骤改用它，旧的交接（`RunHandedOver`、`handOver` 等）删除。回归 `TestARoutedStepIsDeliveredOnlyToTheOwnerSidsConsumer`（修前红：`non-owner sids [1000 1001] received the step; the result came after 750ms of nak backoff over 3 deliveries`）。方案、未采用的 ② 与待确认项见 [FEATURE-2026-11-11-saga-step-sid-route](../../FEATURE-2026-11-11-saga-step-sid-route.md)，证据见 [saga-step-sid-route-20261111](../saga-step-sid-route-20261111/README.md)。演练阈值已恢复缺省 16s（撤掉 `-max-p95 40`）：本机私有真实依赖复跑 14 步 ok，最后一步 p95 5.00s（11-10 为 27.85s），阈值改回 16s 后再跑 5.45s，见证据“真实依赖演练”一节。退回不算重试（维护者 2026-11-11）：不消耗尝试次数，正向受 saga 截止约束、到期补偿（`TestABouncedStepKeepsItsAttemptsAndCompensatesAtTheDeadline`）。

## 2026-11-10：`scripts/perf/sync-business.sh` 夹具与当前 core 不兼容（已修复，未发版）

夹具 `scripts/perf/fixtures/sync_business_test.go.tmpl` 在 main `bc883581` 上直接失败：① 在已 `Start` 的 Nest 引擎上调实例级 `MustRegisterHandlerWithMeta`（只允许 Start 之前），panic；② 绕过 ① 之后，增量信封 v3 不带 Codec，经 `clientsync.DecodeFields` 按 `Codec() != 2` 拒绝；③ 再绕过 ② 之后，增量字节压缩后的载荷不是 `DecodeFields` 能解的字段替换（`unsupported DAO sync format`），夹具需要按 subject 记全量、用生成的 Apply 接增量。因此业务负载下的每 tick 开销、分配与 10% 常态下行没有当前数字；同步字节改由 `TestMainViewMeasure*` 用例测得，与估算相符（[证据](../perf-terrain-acceptance-20261110/README.md#sync-business夹具与当前-core-不兼容已修复夹具已重写)）。

**已修复（2026-11-10，未发版）**：原因 ③ 的判断更正为与 ② 相同：都是别人 `near` 视图上的**空载荷增量**（AddExp 只改 `exp`，`exp` 不在 `near` 里，增量只推进版本，`Decoder` 给出 Codec 0 的空载荷），不是字节压缩的格式。夹具按当前契约重写，没有放宽框架校验：移动处理器在 `init()` 里包级注册；接收端每条连接一个 `entitysync.Decoder`，按 subject 记生成的 `db.SyncPlayerDao` 全量，增量经 `db.ApplySyncPlayerDao` 合并，空载荷增量跳过，合并后的 `level` / `exp`（视图里有时）与本 tick 提交后的 DAO 核对；三处变异各自变红。

## 2026-11-07：v1.42.1 修复复验

RV-20261106-01/03/04 与 RV-20261107-02 原缺陷复验通过，关闭；cellworld 边界抖动夹具修正经 Linux 完整包及三轮重复验证通过。联盟 RV-20261107-01 只部分修复：新增确认 RV-20261107-03（P2），迁出前 drop 后的扫描可重新写入 picked，导致再次迁回时永久跳过待确认宣战；三轮复现、控制组三轮通过。Lockstep 未闭环，main 新登记的 Saga 偶发 fenced 仍根因待查。详见 [修复复验与续审](../../FIX-REVIEW-20261107.md)。以下旧结论按其历史基线阅读。

**后续（维护侧 2026-11-07，原结论保留）**：

- RV-20261107-03 已修复（未发版）：扫描的“已接手”改为按所有权任期（`MarkerEpoch` / `RouteEpoch`，每次迁移前进）记，迁出再迁回是新任期、一定重读，不再依赖迁移前后哪一次失效；迁移不再删条目，完整扫描结束只放掉不再拥有的。回归 `TestWarScanDuringATransferOutDoesNotSkipTheReturn`（由审查探针改写，修前红文本 `waits [{war-on-the-way-back 2}], enemy's wars []`；变异成“只看有没有、不看任期”时实验组与对照组都红），测试替身的所有权记录改为每次迁移推进 `RouteEpoch`。见 [联盟方案“运行中迁入”](../../FEATURE-2026-11-04-alliance-service.md)、CHANGELOG `[Unreleased]`。
- Saga 偶发 fenced：根因已定位并修复（未发版），见下一节。

## Saga 协调器偶发误判“被别的写者隔离”（2026-11-07 发版前压测；已定位并修复，未发版）

v1.42.1 发版前 Saga 整轮压测（`scripts/perf/saga-js-scan.sh`，main `4b6e5212`，本机私有三节点 NATS + 单节点副本集）15 档中 `mongo-forward` c128 一档失败：开压 11 秒后分区 1 fail-stop，`reason=fenced`，错误为 `partition 1 expected last event 11463: nats log: wrong last sequence (err_code=10164)`。该档 128 笔未观察到终态、余额读取 1 次失败；非期望终态、余额不符、发布结果未知均为 0，即按设计安全停下，没有错写。同一档单独重跑 5 轮全部通过（986～1053 笔/s）。

- 测试里只有一个协调器进程，按 [SAGA.md 发布结果](../../../framework/SAGA.md) 的判定，期望序号不符时读分区最后一条（leader 读，不走 direct get）不是自己写的才判 fenced，单写者下不应出现。可能的方向（未验证）：服务端按在途提议判期望序号、而 leader 读只看到已落盘状态。
- 与 v1.42.1 改动无关：`framework/saga` 与 `infra/network/nats/driver/jetstream_log.go` 自 v1.37.0 起未改，`mongo-forward` 场景没有 Nest 参与方。
- 影响：生产单写者下偶发整进程 fail-stop（App 级重启后恢复），不影响正确性。频率按本次约为 c128 满载每 20 档 × 30 s 出现 1 次，样本太少，不作频率结论。
- 证据在本机 `artifacts/perf/saga-js/v1421-saga/10-mongo-forward-128.log`（不随仓）；当时的 NATS 数据目录已随脚本清理，没有流状态可回查。下一步：加诊断（fenced 时记录 LastMsg 的序号与 MsgID、fence.LastSeq、本批是否有过结果未知）后长跑复现。

**根因（2026-11-07 追加，以上原结论保留）**：错误码 10164 不是“序号真的不符”（那是 10071），而是 nats-server 2.14.5 在入站检查（`jetstream_batching.go` 的 `expectedPerSubjectInProcess`）发现同一个期望序号过滤还有提案在处理。服务端 apply 时先存消息、回 PubAck，之后才清这个标记（`jetstream_cluster.go`），协调器收到上一轮 ack 立刻发下一轮就可能撞上；这次没有写入，过滤上最后一条仍是自己上一轮的末条。旧实现只看“最后一条是不是这一块的”，于是判成被隔离。真实三节点集群探针：单写者背靠背 80 万次发布出现 2 次（[证据](../saga-fence-busy-20261107/probe-results.txt)），与压测频率同量级。
**修复**：期望序号被拒后，最后一条序号仍等于期望序号 = 还没有任何写入，原样重发（同一个 fence，别的写者落库后照样被隔离），计 `Stats.FenceBusy`；已前进且不是自己的才 `ErrPartitionFenced`，错误里带上过滤当前序号与 MsgID。回归 `TestPartitionPublishRetriesAFenceCheckRejectedWithoutAnotherWriter`（修前红文本与压测原样一致；把判据改成“一律重发”的变异让两条真隔离用例变红），真实集群 saga 用例与 `nofence` 变异照旧。见 CHANGELOG `[Unreleased]`。 修复后 Saga 整轮 15 档全部通过，其中 native-compensate c128 一档 `fence_busy=1`——真实负载下撞上了这个窗口并重发成功，旧实现会在此 fail-stop（[整轮结果](../saga-fence-busy-20261107/scan-after-fix.txt)）。

## 2026-11-07：新功能审查收尾

新增 RV-20261107-01（P2）：联盟迁移检查后仍可提交宣战，迁移完成后补发责任没有自动接续；RV-20261107-02（P2）：best_effort 单 worker Stop 在 handler 返回前关闭 Closed。两项均连续三轮复现。**RV-20261107-02 已修复（未发版）**：`bestEffortSubscription` 每个回调在接纳锁下检查已停并记入在途（Workers ≤ 1 就地处理的也记），Stop 先关接纳再退订、等在途，Drain 排空后关接纳再等；回归 `TestBestEffortSubscriptionClosesAfterAcceptedHandlers`，见 CHANGELOG `[Unreleased]`。之前三项确认缺陷仍未修复，Lockstep／cellworld 间歇失败仍未关闭。（后续：RV-20261106-01 与 cellworld 的 RV-20261106-02／03／04 已于 2026-11-07 修复、未发版，见下两节；Lockstep 状态不变。）根模块与生成工程 race、客户端及联盟真实进程证据和验证边界见 [收尾报告](../../FEATURE-REVIEW-20261107.md)。

RV-20261107-01 已修复（未发版，见 CHANGELOG `[Unreleased]`）：原结论成立——迁移前对 `WarAcks` 的检查是一次读，挡不住检查之后、fence 之前提交的宣战，旧 owner 的补发在联盟迁走后退出，新 owner 只在起服扫描。修法是“谁拥有联盟，谁的扫描接手标记”：`ScanWars` 起服后每 1 分钟再跑，只读本进程还没接手的联盟（运行中迁入的、新建的），迁走前从“已接手”里去掉，不依赖检查与 fence 的先后；`Pending` 保留为让原 owner 先问完的顺序安排。顺带修掉扫描在空认领上整次失败、无限重试（真实进程复现）。

## 2026-11-06 续审：cellworld 生命周期与编码

新增 RV-20261106-03（P2）：旧 Leave 迟到副本删除同 ID 重新入场的新角色，三轮复现；RV-20261106-04（P3）：BaseState 重复 MsgPack 标签丢失 X/VX，影响直接编码该类型的接入方，未证明当前 demo 的字段映射受影响。源码、探针、控制组及范围见 [续审报告](../../FEATURE-REVIEW-20261106.md#续审跨区生命周期与状态编码)。前述 best_effort、边界抖动和 Lockstep 状态不变。

**已修复（未发版，2026-11-07）**：RV-20261106-03 —— Base 每次 Enter 编入场序号 `BaseState.Entry`，随 Enter、Leave、交接 Offer 传递并存在 `ActorState.Entry`，Real 只接受同一次入场的 Leave；线上格式 v6、game-demo 新增不持久 `CellEntry`。探针改写为 `TestLateLeaveDoesNotRemoveReenteredActor`、`TestLateLeaveAfterHandoffKeepsTheNewEntry`，修前红、修后绿。RV-20261106-04 —— `BaseState` 改为 `x` / `y` / `vx` / `vy` 独立标签，`TestStateMsgpackRoundTrip` 逐字段覆盖 `BaseState` 与 `ActorState`；旧格式数据不迁移（未上线）。见 [cellworld README](../../../../gameplay/cellworld/README.md)、CHANGELOG `[Unreleased]`。

## 最新新功能审查（2026-11-06，v1.42.0）

新增 best_effort 未纳入失败创建守卫，可发布不存在实体的创建事件（RV-20261106-01）。**RV-20261106-01 已修复（未发版）**：失败创建标记 `createdBuildMark` 同时记录并比对 `tx.bestEffort` 长度，构建里 Emit 了 best_effort 的失败创建同 reliable 一样记 `ErrFailedCreateLeftEffects` 整条回滚、不发布；回归 `TestAFailedCreateDoesNotPublishItsEffects`，见 CHANGELOG `[Unreleased]`。cellworld 跨区抖动完整包有失败，独立复测通过，尚未闭环。见 [审查报告](../../FEATURE-REVIEW-20261106.md)；不据定向通过宣布上线验收完成。

**RV-20261106-02 已修复（未发版，2026-11-07）**：读代码判断为夹具等待不足——停下时最后一次交接的旧格等 OfferReply / 隐式确认、区域表等角色报告，都是异步几跳，固定 sleep 3 拍不够。改为有期限的收尾条件（5s，超时打印各格记录、交接计数与出站队列）并断言没有交接靠放弃收尾，原断言不变；新增 `TestWorldJitterAtBoundarySlowHandoffConclusion` 让收尾消息晚 1s，旧等待下三轮复现审查的失败签名（owner sid=101 epoch=13、`(1,0)` frozen），新等待通过。本机（macOS）完整包 race 1 轮、非 race 5 轮通过；

**2026-11-10 续（真实时钟用例统一整治）**：整包 `-race` 加压时的收尾超时（旧格 `accepted=1`、区域表仍 frozen；上一轮 `TestWorldStraightCrossing`“frozen 4.4s”记为尚未确认）已定位为实现缺陷并修复（未发版）：冻结报告经出站队列晚于交接结论到达，`applyReports` 重建了一条永久的 frozen 记录；回归 `TestLateFreezeReportDoesNotResurrectTheRecord`，见 CHANGELOG `[Unreleased]`。其余间歇失败（`view gap` 墙钟阈值、攻击前 Ghost 未到、sleep 后立即断言等）是用例写法，已统一改为“等结论、有期限、超时打诊断”与按心跳拍数计间隔，变异证明承诺未放宽。同负载（8 个满载进程 + 4 份整包 -race 并行，load 中位 19～42）对照：改后 13/14 通过（唯一一次是回复全丢用例与 3s 放弃期限赛跑，已把该用例期限放到 10s），main 6/14；无额外负载 3/3。

## 实体创建与 TCP 快照审查（2026-10-31，已修复）

基线 v1.33.0（030a9f11），问题、失败证据与修复见 [提交审查 2026-10-31 节](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/COMMIT-REVIEW.md#2026-10-31继续审查持久实体创建与-tcp-恢复)。RV-20261031-01 / P1（初始化登记 DAO 修改后构建失败，业务吞掉错误时失败实体的 DAO 仍随事务提交）已修复（`aad320d2`，v1.33.1），剩下的边界见本文[新建实体落库与缺失 DAO 补空](../../KNOWN-LIMITS.md#新建实体落库与缺失-dao-补空r15维护者-2026-10-30)的“失败的新建”条。同节的 TCP 快照门禁失败是测试夹具的平台缓冲与等待时间假设，夹具已修正（`de46d022`，运行时代码不变），修后夹具尚未在 Linux 实机复跑，见 [Lockstep](../../KNOWN-LIMITS.md#lockstep) 的“TCP 快照用例的平台假设”条。

## 上线前 Saga 审查（2026-10-28 / 10-30，已修复）

问题、失败证据与修复见 [提交审查 2026-10-30 节](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/COMMIT-REVIEW.md#2026-10-30上线前-saga-与验证门禁审查)与 [2026-10-28 节](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/COMMIT-REVIEW.md#2026-10-28main-jetstream-saga-原型审查历史问题正式实现已处理)。RV-20261030-01 / P1（终态剪掉后同业务键再开的一笔复用上一笔的操作身份）与 RV-20261030-02 / P1（自定义回执保留期短于每步重试窗口时同一操作可再执行）已修复（`f7d09022`，v1.33.1，Saga 存储格式升级需清空）；02 修后剩下的超期停机 / 积压边界见 [Saga](../../KNOWN-LIMITS.md#saga) 的“参与方回执只保护计划内的重试窗口”条。RV-20261028-01/02/03 是已删除的 JetStream 日志原型上的问题，已在原型上修复（`e3d66eb0`），正式实现另有对应约束与测试。

## 提交审查（2026-10-22，已修复）

基线 `13bc939e`，复现及验收要求见 [提交审查](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/COMMIT-REVIEW.md)。三项均已修复：RV-20261022-01（`6edebb13`，见 [v1.31.0 实现篇 H2](../../../release/v1.31.0-IMPLEMENTATION.md#h2)）、RV-20261022-02（`47691000`，见 [v1.31.0 实现篇 R2](../../../release/v1.31.0-IMPLEMENTATION.md#r2)）、RV-20261022-03（`b7b52486`，见 [v1.31.0 实现篇 S4](../../../release/v1.31.0-IMPLEMENTATION.md#s4)）。本节没有仍未修复的项。

## 2026-11-06：Remote 独占实体的只读请求不再取写许可

- **只读 handler 也取写许可：已解决**（2026-11-06 测量、同日按维护者决定修改，[benchmarks/alliance-warscan-20261106](../../../performance/benchmarks/alliance-warscan-20261106/README.md#独占不取写许可2026-11-06)，契约见 [REMOTE_ENTITY“独占写准入”](../../../framework/REMOTE_ENTITY.md#独占写准入)）：原来 Remote 托管实体上每个 Nest 请求（handler 不写也一样）都 `GrantWrite`——`_remote_entity_meta` 一次多数派 `findAndModify`，联盟起服 `ScanWars` 与 alliance 的全部读 RPC（`Info`、`Result`、`Membership`、守卫回执、`WarAcks`）因此每次一次 Mongo 写。现在独占实体稳定持有期间读写都不碰 Mongo，只在加载、取得归属、从共享退回独占时读一次权威对齐；没有新增只读声明（维护者的选项 A 未采用：读仍过实体写门、占写额度，理由见契约）。同机前后对照：Info 16 并发 1 052～1 093 /s（p50 约 15 ms）→ 32 043～35 005 /s（p50 0.4 ms），每次 1 条 `findAndModify` → 0 条命令；已有玩家 Join / Leave 每次 Mongo 命令约 16 → 13；1 万联盟起服扫描 18.1～18.6 s → 0.46～0.84 s，扫描期间 `command` 约 3.2 万 → 400～600。

（4）新建实体首写认领后的对齐读已省掉（2026-11-06，认领带回权威状态，认领前也不再先读一次，见 REMOTE_ENTITY“独占写准入”“对齐时刻”第 2 条）。（5）同实例每次 `Join` / `Leave` 剩下的 1 条 `_remote_entity_meta` 点读（game-demo 联盟服务的 `claimable` / `owned` 经 `Manager.GetRemoteOwnership` 读守卫的所有权）**已解决**（2026-11-06，维护者选 A）：`GetRemoteOwnership` 在本进程对实体是稳定独占持有时（与独占写准入同一判据）答缓存的租约，其余情况照旧读权威；前提被打破、还没有写发现时它答本 sid，写在投影上被拒之后回到读权威（契约见 [REMOTE_ENTITY“独占写准入”](../../../framework/REMOTE_ENTITY.md#独占写准入)“所有权查询”，对照见[基准页](../../../performance/benchmarks/alliance-warscan-20261106/README.md#所有权查询答缓存2026-11-06)）。

## 2026-11-04：联盟服务冒烟里的两条观察

- **联盟服务冒烟里的两条观察**（2026-11-04 发现，同日查清并修复，见 CHANGELOG `[Unreleased]` 对应“修复”条）：
  - **停机窗口里路由 RPC 答成结果未知：已修复**。根因：生成的 owner Mod（`add_rpc_entity.go` / `add_rpc.go` 的 `Stop() {}`）不撤 handler，总线也没有按方法撤下的入口；App 按依赖逆序先停 Nest，handler 挂到 nats Mod 停止，进来的请求拿到 `nest.ErrNestStopped`，`RPCWriteAnswer` 的 default 分支原样返回、`errcode.ClientError` 折成 `CodeInternal`。第一版修法（`4947c62a`）是总线 `WithdrawRpc` + 生成的 owner Mod 在 `StopWithContext` 里撤；同日按维护者“同一个问题：App 的生命周期管理”改为 App 停机第一步统一关输入（[方案](../../FEATURE-2026-11-04-app-lifecycle.md)），owner Mod 的撤下代码已删。当时列出的四条边界：
    - **集群里退订兴趣的传播窗口：仍在**，见下面“App 停机关输入后的边界”①。
    - 等在途请求的时间算在 owner Mod 的停机份额里：**已修复**（`109d673c` 起不再由 owner Mod 等；排空在 App 第一步，用 `shutdown.total_timeout` 的同一个窗口）。
    - 启动中（`Service.Init` 还没返回）收到停机信号时 App 不停 Mod 直接退出、在途请求超时：**已修复**（`9c7c8c5f`：先关输入、等那一步返回、再按顺序停）。
    - 独立进程的生成 `Server` 与框架内置服务 Mod 停机不撤 handler：**已修复**（`4f2a0708`：`NatsMod.CloseIngress` 一次撤下总线上全部方法，不分是谁注册的）。
    - `refuseTask` 的 `errRPCNotAdmitted` 被折成 `CodeInternal`：**已修复**（`aa55e801`：答 `CodeRetryable`）。
  - **两个 alliance 进程同时起服，一个在 `Service.Init` 建 effect 消费者时 `context deadline exceeded` 退出：已定位并修复**（驱动 `JetStreamClient.createOrUpdateConsumer` 单次超时后同配置重发）。复现：生成的 game-demo 两实例冒烟修前 8 次里 2 次（`alliance-reservation-due`，与原观察同一条报错），驱动层贴近起服的双连接临时用例 70 轮里 1 次（没有提交：复现率太低，不适合当回归）（`alliance-member-changed`）；失败的一方等满 nats.go 缺省 5s，nats-server 日志里这个 consumer 在约 16ms 内两次选出 leader，另一个进程正常建好并消费，说明消费者已经建好、只是请求方没收到回答。服务端为什么只回答一个请求没有读 nats-server 源码核实（本机没有 2.14.5 源码），按“回答丢失”处理：`CreateOrUpdate` 同配置幂等，重发拿到已建好的消费者（修后冒烟多次日志可见重发后 Init 正常完成）。

## 2026-11-04：空认领（建盟被拒留下只有所有权记录的联盟号）

- **空认领（建盟被拒留下“只有所有权记录、没有数据”的联盟号）**（2026-11-04 联盟服务阶段 5 发现；**已修**，`9a1d9ee4`，维护者按推荐 A，契约见 [REMOTE_ENTITY“路由登记”](../../../framework/REMOTE_ENTITY.md#路由登记)“空认领”）：首写认领在写准入、提交之前，事务被拒绝回滚或提交前崩溃时认领不撤销。修前同 sid 重启后预加载对每个这样的号记 ERROR、对它的路由 RPC 一直答 `remote.4`（可重试）；现在框架把它当作“本实例拥有、业务上还不存在的实体”：预加载计 `empty`、前置检查交给业务（创建类请求照常首写，其余由 Nest 答 `entity_not_found`）。

## 2026-11-04～06：实体服务 RPC 按方法串行与跨实例写超时

- **实体服务的 RPC 在一个实例上按方法串行：已修复**（2026-11-04 联盟服务阶段 5 短测发现，同日按维护者同意的方向修复，见 CHANGELOG `[Unreleased]` 对应“修复”条，契约见 [guide/09“接收端并发与顺序”](../../../framework/guide/09-services.md)）。根因：`infra/network/bus` 的 `onRpcMessage` 按 `hashString(MsgName)` 选调度 worker，handler 同步等 Nest 完成，同一方法在一个实例上一次只处理一个；JetStream RPC 在 consumer 回调里同步跑 handler，每个 consumer 同样串行。现按顺序键派发（`rpc_dispatch.go`）：路由 RPC 同一实体、affinity 方法同一键按到达顺序逐个处理，不同键与不带键的请求并行。核对过没有调用方依赖“同一方法在一个实例上串行”（仓内服务同一方法本来就在多个实例上并发，状态在存储 CAS 或自己的锁里）。修前修后（一个 alliance 实例，各两轮，[benchmarks/rpc-dispatch-20261104](../../../performance/benchmarks/rpc-dispatch-20261104/README.md)）：16 个联盟各一个并发的 `Info` 约 100 → 685～1040 次/秒、p50 约 160 → 16～21ms；`Found` 16 并发约 9 → 22 个/秒；同一联盟的请求吞吐不变（承诺本身）。剩下的边界：（各条仍有效的部分已移到 KNOWN-LIMITS 的 Remote）

  - **`Join` 分散到 16 个联盟：已从约 23～27 次/秒提到约 90～140 次/秒**（2026-11-05，[benchmarks/alliance-join-20261105](../../../performance/benchmarks/alliance-join-20261105/README.md)）。定位出的两处跨联盟串行点都已处理：① **实体锁错位（缺陷，已修）**：`EntityManager.build` 在规范 ID 之前取锁，只给唯一号新建的实体（生成的 lifecycle `Create` / `GetOrCreate`）全部共用 ID 0 的一把锁（见 CHANGELOG `[Unreleased]` 对应“修复”条）；② **投影器的 effect 顺序类（契约，维护者 2026-11-05 改缺省）**：原先带 effect 的记录之间按 WAL 顺序逐条投影（`effectOrderKey`），入会 / 退出记录都带“成员变化”effect，一个进程的全部成员变化排成一条通道（每条一个 Mongo 事务约 16～18 ms 加一次 checkpoint fsync），strict Remote 写等投影才答复。现在缺省**按实体有序**（`dataengine.projection.effect_order: entity`），全局顺序是缺省关闭的 `wal`；仓内 effect 消费者与 outbox 路径逐个核对过，没有依赖跨实体顺序的（[impl/03 effect 顺序范围](../../../framework/impl/03-dataengine.md#effect-顺序范围)），生成配置都不打开 `wal`。同机前后对照（a2762377 / 本改动，各 3 轮）：16 联盟新玩家 `Join` 23.0～25.7 → 92.5～108.4 次/秒、`Leave` 24.4～27.1 → 100.9～125.4、守卫已存在 `Join` 25.3～26.7 → 118.5～137.7，全部 failed=0；本改动打开 `wal` 退回 22.8～25.9。剩余：同一联盟 / 同一守卫的记录仍按 WAL 顺序（同实体有序是契约）；单联盟热点的上限由单条 `Join` 约 60～80 ms 决定（Remote 写准备约 30 ms——两个实体各一次 `GrantWrite`、WAL fsync 约 10 ms、发布写“已发布”约 15 ms），约 12～16 次/秒，不在本条范围；跨实例两步入会原先被守卫所在实例唯一的 `AllianceShard`（预约期限的持久定时器宿主）串成一条线（约 10～11 次/秒，p50 约 1.5 s）：**2026-11-05 晚随“联盟不用 effect”去掉**（[方案 §2.5B](../../FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）：预约期限改为持有守卫的进程里的到期回调，`ReserveGuard` / `ResolveGuard` 只写守卫一个实体，回归 `TestReservationsOfDifferentPlayersDoNotWaitOnEachOther`（修前红：第二个玩家的预约等第一个的，2 s 内没完成）。同机交替 3 轮（[基准页“联盟不用 effect”](../../../performance/benchmarks/alliance-join-1000-20261105/README.md#联盟不用-effect2026-11-05-晚)）：64 并发跨实例 `Join` 成功 9.3～11.2 → 29.0～32.8 次/秒、p50 4.1～4.7 s → 0.34～0.38 s；16 并发 p50 1.49～1.61 s → 0.26～0.28 s，但吞吐被下一条的 30 s 卡顿拖住（8.5～42.6 次/秒，3 轮里 2 轮有 26 / 42 次 `server error`）。

  - **跨实例写偶发 3 s 超时、同一实体随后约 30 s 内的写全部超时：已修复（2026-11-06，维护者选 A“写者自己先发”）**（原“跨实例 `Leave` 偶发 3 s 超时，原因尚未确认”）。根因：每个进程的 Remote outbox 协调者扫描共享 outbox 里全部已持久未发布（Applied）的事务并代发，代发方只完成自己进程的 tracker；写者进程的 tracker 停在 Applied，strict 写等到 Nest 请求期限（3 s）超时、转延迟收尾，收尾持写门到 `remote_entity.finalize_projection_timeout`（30 s）才回源释放，同一实体其间的写在写门上超时（`remote_entity_write_gate_timeout_total`）。修复：事务记录带写者 sid，扫描只立即发布本进程写的提交，别的进程写的提交过了宽限期（新配置 `remote_entity.outbox_takeover_grace`，零值与下限取 `finalize_projection_timeout`）才代发，同一实体上已有要发布的后继时立即代发；契约与边界见 [REMOTE_ENTITY“outbox 发布归属”](../../../framework/REMOTE_ENTITY.md#outbox-发布归属)。回归 `TestOutboxWriterPublishesItsOwnCommitBeforeOtherCoordinators`（修前 `writer tracker=2`（Applied）、`commit timed out`、非写者标记 1 次）。前后对照（同机交替，修后先跑，[基准页](../../../performance/benchmarks/alliance-join-1000-20261105/README.md#outbox-写者自己先发2026-11-06)）：两实例 64 并发跨实例 `Join` 3 轮修前 18.3～41.3 次/秒、失败 48～246 / 1536、p99 3.0～5.5 s，修后 154.7～167.9 次/秒、失败 0、p99 0.58～0.67 s；写门超时修前每轮 23～270 次、延迟收尾回源（`finalize_status_read_total`）18～44 次，修后两者与新指标 `finalize_projection_timeout_total`、`outbox.takeover_total` 都是 0；同实例 128 并发新玩家 `Join` 无退化（修后 396～420、修前 365～438 次/秒）。

  - **单实例 128 / 256 并发写偶发“全体同时约 3 s 超时”：发布侧两处框架缺陷已修复（2026-11-06），剩下的 I/O 停顿尚未处理**（原基准页“单实例 t 轮修前两次 128 个 `handlerAllianceRemove` 同时 3 s 超时，原因未查”）。一起超时的是全部写额度（`max_concurrent_writes` 128）：一次停顿让所有等确认的 strict 写一起越过 Nest 请求期限，`Join` / `Leave` 重试后成功。定位（私有 mirror-local、单 alliance 实例、重试版探针，≥32 个尝试超过 2 s 时抓 goroutine 栈，修前 13 轮、修后 8 轮，[基准页](../../../performance/benchmarks/alliance-join-1000-20261105/README.md#单实例全体同时超时2026-11-06)）：修前 34 份停顿栈里 16 份是 outbox 队头阻塞，来自两处代码——① 后台发布循环一次发一页、等整页结束才读下一页（`publishOutboxPage`），一笔 Mongo 标记慢到 1～2.5 s 时 32 个发布 worker 里 15～31 个空闲，之后的提交全部没人发布；② `RemoteSnapshotCache` 的 publish 分片只取实体 ID 低 6 位，而低 10 位是 kind 与 category，同一 kind 全部挤在一把锁上、持锁做 L2 往返，13～30 个发布排在同一把锁上。修复：一页只剩慢项且 100 ms 无进展时转到后台（[REMOTE_ENTITY“outbox 发布并发”](../../../framework/REMOTE_ENTITY.md#outbox-发布并发)，新指标 `remote_entity.outbox.detached_total`）；publish 分片与快照客户端兴趣条带按整个 ID 打散（`entity.EntityIDStripe`）。回归 `TestOutboxSlowPublicationDoesNotHoldUpLaterCommits`（修前 strict 写 `commit timed out`、tracker 停在 Applied）、`TestRemoteSnapshotPublishShardSpreadsOneKind`（修前 64 个同 kind 实体落在 1 个分片），另有同实体顺序、停机排空、后台失败退避三条。修后 24 份停顿栈里 outbox 队头阻塞 0 份。

## 2026-11-05：`TestAllianceE2ERedisOutage` 偶发失败

- **`TestAllianceE2ERedisOutage` 在本机偶发失败：Redis 恢复后一个 alliance 实例丢单实例锁 fail-stop：已确认，用例已修**（2026-11-05 定位 `Join` 吞吐时观察到；同日查清）。**根因是用例的“短断开”没有上限，而单实例锁实际扛得住的断开比文档写的短，不是续期状态机的缺陷**：① 用例把 info 轮（4s）、断开期间的入会 / 退会 / 跨实例入会（每步带 1 分钟重试）与删键放在同一次断开里，注释写 6s，本机实测 8.0～9.4s，入会重试慢时更长；② 续期节拍是上一次 Applied 的 asked（T）+ k × `renew_interval`，回复不早于 T + `ttl − guard` 的失败判失锁，连接被拒（toxiproxy 关代理）时最后一次能挽回的是 T+12s（缺省配置），断开可能在 T 之后将近 3s 才开始，所以对任意相位都扛得住的断开只有 9s，不是原来写的“`ttl − guard` 约 10s”（契约改写见 [guide/01“单实例锁在后端短暂不可用时”](../../../framework/guide/01-app-lifecycle.md#单实例锁在后端短暂不可用时)）；③ 锁客户端的 go-redis 连接池（`PoolSize` 2）连续两次拨号失败后不再拨号、直接返回上次的错误，由后台每秒探测一次，Redis 恢复后续期最多还要失败约 1s（单独量：恢复后第一次成功 1.02s，`PoolSize` 16 时 9ms）。复现（私有环境修前 16 次里 3 次失败，保留进程日志对齐 toxiproxy 开关时刻）：失败的一次断开 8.86s，3000 断开前最后一次 Applied 在断开前 2.79s，最后一拍 11:02:03.554，代理 11:02:03.207 已恢复，这一拍仍返回缓存的 `connection refused` → 失锁，用例报 `4 of 8 missing`；另一次入会重试拖长了断开，两个实例都在断开 9.8s 时失锁（`cross-instance join with Redis down: no responders`）；通过的各次里最后一拍距恢复 +0.9～+3.6s。**修复**：用例把锁参数写进配置，短断开的上限按它们算（对任意相位扛得住的时长 − 驱动恢复 1s − 余量 2s），断开期间的步骤带截止时间、做完立即恢复并断言实际断开时长，恢复后等两个实例 `/readyz` 的 singleton 回到 ok。缺省参数下上限只剩 6s，而断开期间这几步本机要 8～10s（每次断开开头各进程的 go-redis 每个请求先拨号重试约 0.4s），拆成三次断开也各要 2.5～6s、仍放不进（试过，3/4 次超上限），所以这个用例把 `ttl` 调到 30s（上限 21s）；缺省参数下锁在哪一拍失锁由 framework/app 新增的 `TestSingletonSurvivesAStoreOutageOnlyUntilTheLastChanceBeat` 钉住（连接被拒 T+12s、请求挂到超时 T+9s）。产品语义没改。

## 2026-11-04：`alliance.transfer {count}` 在实体多时超过运维命令期限

- **`alliance.transfer {count}` 在实体多时超过运维命令期限**（2026-11-04 联盟服务阶段 5 短测发现；**已修**，`117e45ef`）：改为经 Remote Manager 的 `ListOwnedRemoteEntities` 按 owner 分页枚举（代价与本实例持有数成正比；真实 Mongo 上 4 万条所有权记录里列出本实例的 1 万条约 55～80ms），离命令期限不足 2s 时停下、答 `more: true`，再调一次接着转。实测约 2 万个实体（三个实例共 15439 个转给一个实例）用 5m10s、43 次调用，最慢一次 8.1s，全部在缺省期限内答复（修前同一场景要把期限放宽到 600s）。

## 2026-10-29：Saga 清理守卫的 floor

- **清理守卫的 floor**：原登记的疑点**已确认并修复**（2026-10-29，随 v1.31.1 发布，[设计 §16](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md#16-风险与未核实项) 第 3 批发现（2））：消费者信息由消费者 leader 按本机流副本回答，旧实现在副本追赶时把 floor 抬到流末（私有三节点 SIGSTOP 实测：修复前恢复后第一次回答就越过了未取的命令；修复后的用例里追赶期间消费者信息连续 17～83 次报“没有待投递”）；没回答的 durable 在 nats.go 的列表里会消失。现在 `DurableAckFloors` 只用消费者 Raft 的 AckFloor 与流 leader 的 `MSG.GET`，没回答的消费者记 0。

## 2026-11-04：mmorpg 生成工程的 `TestMMORPGRobotBusiness` 偶发失败

- **mmorpg 玩法生成工程的 `TestMMORPGRobotBusiness` 偶发失败（2026-11-04，已定位并修复）**：根因在嵌入 TCP 接入，不在夹具——同一 SessionID 的旧连接迟到完成的登录把已经在服务的新连接关掉（见 [CHANGELOG](../../../../CHANGELOG.md) `[Unreleased]` 与本文[同一 SessionID 的重连](../../KNOWN-LIMITS.md#同一-sessionid-的重连)）。失败的是第 4 次 robot，它的第 0 步就是 `Reconnect`：Robot 的 `connect` 发出认证帧不等 ack，紧接着重连并关闭旧连接，旧连接的握手在服务端照常走完、晚于新连接登记。按这个顺序直接打 `TCPServer` 的探针修前 3000 轮 31 次新连接被关、修后 0 次；生成工程同一用例（8 路并行、修前 / 修后二进制交替各 5 轮）修前 1600 次失败 15 次、全部是 `step 0: robot session: closed`，修后 1600 次 0 失败。同一次报告里的 `scripts/perf/sync-aoi` `TestHeartbeatPatrolReachesClientsAndTheAOI` 跳拍失败（inflight 4）是用例的时序假设，不是缺陷，见本文“压测工具的心跳驱动”一节。

## 2026-11-11：wallet 的业务键

- **wallet 的业务键**：`Apply` 的键只收 1～64 字节的 ASCII 字母、数字与 `- _ :`，别的答 `wallet_invalid`（2026-11-11 修：之前不限长度与字符，带空字节的键在提交时才被拒、客户端拿到的不是业务码，超长键能把账本撑大；shop 模块的作者发现）。

## 2026-10-31：房间内同步与 self_sync 同进程

- **房间内同步与 self_sync 同进程：已确认并提供正式入口**（2026-10-31，[方案](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-31-room-sync-groups.md)）。结论：①业务自建 Manager 经 `NestOptionWithEntitySync` 交给带 self_sync 的 Nest Mod 会被静默覆盖——已修，`Provide` 返回 `ErrEntitySyncObserverConflict`；②`policy.Group` 挂到 self_sync 的 Manager 上线格式能共存（同一会话、按命名空间分流），但会话归自身视图，登录前 `Join` 被拒、重连后组订阅静默缺失——由 `policy.GroupView` + `wiring/selfsync.Groups` + `access.player.sync_groups` 取代（[ENTITY_SYNC](../../../framework/ENTITY_SYNC.md#按组同步与-self_sync-共用连接groups)）。

## 2026-11-05～06：联盟 Join 的 Mongo 命令数与 effect outbox workers 缺省

2026-11-13 从 KNOWN-LIMITS“Remote：遗留”的“单实例分散 `Join` 的目标 1000 次/秒”条下移出的两个子条目（已完成的改动与已定的缺省；“到不了 1000/s”仍留在原条）：

- **每次 `Join` 的 Mongo 命令数：联盟去掉可靠 effect 与守卫快照后少约 7 条**（维护者 2026-11-05，[方案 §2.5A](../../FEATURE-2026-11-04-alliance-service.md#25a-守卫是名单的缓存惰性自愈2026-11-05)；同机修前 `86658aa3` / 修后交替 3 轮，profiler，[基准页](../../../performance/benchmarks/alliance-join-1000-20261105/README.md#effect-持久性与守卫自愈2026-11-05-晚)）：不计 outbox 后台轮询，同实例新玩家 `Join` 27.2～27.3 → 20.2、`Leave` 23.0～23.1 → 16.2～16.3、守卫已存在 `Join` 23.0～23.1 → 16.2～16.3，跨实例 `Join` 50.8～51.8 → 40.1～40.2、`Leave` 32.8～35.3 → 26.9～27.9；少掉的是 effect outbox、消费方 inbox 与守卫快照。重试版吞吐 128 并发新玩家 `Join` 229～270 → 272～353、256 并发 197～246 → 259～299 次/秒（区间部分交叠，本机负载噪声内方向确定、幅度不精确）。仍到不了 1000/s。
- effect outbox `dataengine.outbox.workers` 缺省已由 2 改为 8（维护者 2026-11-05 决定 B，2026-11-06 定为正式缺省 8，不再是试用值）。修后代码配 2 个 worker 时压测结束 outbox 积压 1.5～1.9 千条、配 8 个时不到 100 条，`Join` 吞吐在本机噪声内看不出差别。

## 2026-10-29：旧 Saga 路径的容量数字

2026-11-13 从 KNOWN-LIMITS“能力与验证边界”表与“待补测”移出：测的是已删除的旧协调器与旧夹具（`298e4396` 删除 `performance_real_integration_test.go` 等），对正式实现不再适用。

| 项 | 当前结论 | 后续入口 |
| --- | --- | --- |
| Saga补偿容量 | **旧 Redis / 旧原生路径的数字，第 4 批之后不再适用（正式实现的数字见上文 [Saga](../../KNOWN-LIMITS.md#saga) 的“正式实现的性能”条）**。Mongo 步骤路径 v1.29 各 10 分钟：正向 20/s P99 0.62s、全补偿 10/s P99 0.61s（服务端口径 0.59 / 0.58s），十个分钟段无增长趋势，核对通过。2026-10-20 夜间更高档位（10 分钟）：正向 30/s 完成率 28.4/s、P99 141.7s、17302 笔以补偿 / 失败结束，全补偿 15/s 完成率 14.0/s、P50 单调增长到 49.5s、866 笔 failed——两档都饱和（outbox / lease 排空、存储与发布零失败）。单进程上限在正向 20～30/s、全补偿 10～15/s 之间 | 中间档位（25、12）；真实步骤、混合负载与生产存储另测 |
| Saga 原生步骤容量（单进程） | **旧 Redis / 旧原生路径的数字，第 4 批之后不再适用（正式实现的数字见上文 [Saga](../../KNOWN-LIMITS.md#saga) 的“正式实现的性能”条）**。2026-10-20 夜间 15 分钟：正向 8/s（两次）与全补偿 5/s 都 0 笔终态不对、核对通过；服务端 P50 559 / 549ms、713ms（P50 < 1s 成立），**P99 0.92～3.9s**，有分钟级尖峰（各段 Mongo 命令最大值同时到 750～900ms，接近投影器容量时积压放大）；两次正向 8/s 的 P99 差 4 倍。原先 guide/06 §5、生成的 `sagaCapacitySummary` / `steps.go` 注释与 `roost help saga` 里的“P99 0.8 / 1.2 s”来自 2 分钟单样本，15 分钟样本上不成立；2026-10-21 维护者批准，这几处与 CODEGEN_REFERENCE / PROJECT_GENERATOR / BEGINNER_WORKBOOK 已改为 15 分钟口径（P50 < 1s、P99 0.9～3.9s、WAL 放快盘、Mongo 步骤上限区间）；限制与维护者决定见上文 [Saga](../../KNOWN-LIMITS.md#saga) | [SAGA-CHECKPOINT](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/SAGA-CHECKPOINT-20261018.md) 复跑入口；P99 尖峰的 Mongo 侧原因 |

5. **Saga Mongo 步骤中间档位（历史，随旧协调器关闭）**：以下数字测的是已删除的旧协调器与旧压测夹具 `TestRealSagaPerformance`（saga JetStream 切换第 4 批 `298e4396` 删除），对正式实现不再适用，也不能用当前代码复跑；正式实现（含 Mongo 步骤）的容量由 `scripts/perf/saga-js-scan.sh` 测，见上文 [Saga](../../KNOWN-LIMITS.md#saga) 的“正式实现的性能”条。原记录：20 / 10 稳定、30 / 15 饱和，25 / 12 未测；2026-10-22 重跑（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：正向 30/s 仍饱和（另有 3 笔 ManualRequired，夜间 0，尚未逐个核对），全补偿 15/s 这次稳定（14.96/s、0 笔终态不对、P99 3.0s），15/s 是边界档、两晚环境不同，需要多样本；旧夹具 `TestRealSagaPerformance` 当时限制单次 ≤10 分钟。10-23 修复原子认领 + 显式两个消费 worker 后，正向 30/s × 10 分钟全成功；旧串行容量不覆盖新配置，全补偿的新配置仍待验证。

## 2026-10-22：历史压测两项待补测（Sync P99 增量来源、Remote 80TPS 夜间 p99）

2026-11-13 从 KNOWN-LIMITS“待补测（历史压测遗留）”第 2、3 项移出；两项里仍未确认的部分记在 KNOWN-LIMITS“能力与验证边界”的“重档总体”“Remote容量”两行。

2. **Sync 提交→客户端 P99 +0.6～1.1ms 的来源**：A-B-A-B 判断为代码带来，但没有单变量对照，具体提交尚未确认。2026-10-22 发版候选对 v1.29.0 的 A-B-A-B（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：发版候选反而低 2.4～2.7ms（两对一致），所以至少相对 v1.29.0 没有这类增加；相对 v1.27.0 的那段增加的来源仍未确认。
3. **Remote 80TPS 夜间 p99 偏高**：2026-10-21 同机对照（各 1 次）v1.27.0 / HEAD p99 同为 263ms，倾向环境条件；夜间共享 Mongo 主节点变慢的原因尚未确认。2026-10-22 发版候选复测（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)） p99 287ms、共享 Mongo 慢操作 0，与“环境条件”一致。
