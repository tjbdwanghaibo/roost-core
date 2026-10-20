# 核心：Sync、Lockstep 与客户端：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/04-sync.md) · [模块总目录](../README.md)

## 先理解这一块

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

背包和位置通常看状态同步，按输入推进同一场战斗看Lockstep。两种方式解决不同问题，不能直接互换。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 子系统分工

当前主线将同步流、对局和空间区域分别命名为 `StreamID`、`lockstep.Match`、`AreaID`。这是随 v1.26.0 发布的源码 API 调整（不留旧别名），升级映射与验证见[维护记录](../../maintenance/ACCESS-SYNC-FOLLOWUPS.md)。流标识只在连接/同步上下文内有意义，Entity Sync 当前填入 1；不要把它当地图或副本 ID。

entitysync 维护 subject、session、subscription、profile 与每会话帧；policy 提供 Direct/Group/Interest/AOI 的可见关系。frame 定义帧格式，nettransport 负责发送能力与背压。syncbus/mirror 是服务间复制，syncstream 是带基线/增量与 journal 的流。lockstep 传输输入、帧与校验，不替业务保证跨引擎确定性。

工作分支已实现 [Block AOI 与手写 SpatialComponent](../INTEREST-BLOCK-AOI.md)：事实入队使用独立短锁，批次更新空间后并行计算observer；同一实体可在多个块被观察，实际位置仍唯一。内容发布必须同时满足提交门和政策事实已处理。接入不改Guard回调或生成器；完整重档性能尚待复验。

## 2. 状态同步流程

注册 subject → OpenSession（可 held）→ Subscribe → ReadySession → Flush/运行循环。held 会话可以建立订阅但不出帧；ready 后从新 epoch 全量开始。多个来源订阅同一 subject，各自持有引用，最后一个离开才真正取消。subject 保存订阅真相，session 保存其对象生命周期关系。

PrepareViews 汇总所需 profile；优先级配置先参与选择，再按确定顺序选择视图。视图必须限制字段可见范围，不能因空视图或缓存复用扩大权限。快照预算限制尚未持有对象的冷创建；增量、remove 和已持有对象的全量不能被当成同一种冷创建计费。

### 玩家只看自己：自身视图

最常见的需求是“客户端收到自己的金币、道具变化”。生成工程在 `roost.yaml` 的 `access.player` 上写 `self_sync: true`（默认关），生成器把 Nest Mod 换成 `NewModWithEntitySync` 并装上 `wiring/selfsync`；Player 的标记缺省按 `sync=true`，没写 `subjectPacker=` 时生成器写默认 packer（视图 `self` 包含 DAO 全部 sync 字段），业务只给要看的字段标 `sync`；要裁剪字段或别的视图时自己写 packer 并在标记写 `subjectPacker=`。步骤、生成物与客户端解码见 [PLAYER-ACCESS §7](../PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)。

行为：鉴权 ACK 之后登记连接，下一个 tick 开会话、绑定 player→自己（`policy.Direct`，视图 `self`），第一帧全量，之后提交的变化是增量；实体还不在内存时等它出现；断线重连、同一玩家的新连接都从全量开始，全部连接断开后关会话、注销 subject。生命周期全部在 core 的 [`policy.SelfView`](../../../framework/sync/entitysync/policy/self_view.go)：回调只记事实，Flush 的政策阶段一次对齐，所以登录、断线、顶号与 Flush 的交错不需要业务处理。多人视图（附近的人、队友）仍用下面的 Interest。

慢消费者：**跟不上就断开**。一帧被活着的连接拒收时，传输（`wiring/selfsync.PushPlayerFrame`）关闭这名玩家的全部连接、Manager 关会话，客户端重连从全量开始；SelfView 不在原连接上重开。整个进程的出站驻留预算耗尽是例外：不是谁慢，整轮重试、保留脏状态，连接与会话都不动。自建 Manager、会话 ID 就是玩家 ID 的场景（如 game-demo 的场景）用同一个函数做 Transport。规则与理由见 [impl/04-sync §2C](../impl/04-sync.md#2c-自身视图selfview的生命周期)、[GATEWAY §4](../GATEWAY.md#慢消费者跟不上就断开)。

两条由框架强制、业务不用对齐的规则：一帧的上限取接入层上限（嵌入 TCP 的 `player_access.tcp.max_payload_bytes`、独立 Gate 下 Game Ingress 的载荷上限），在 Manager 创建前定下，所以超过接入层上限的帧不会推出去；一个 `SubjectSyncState` 只能登记在一个 Manager，第二个 Manager 登记时得到 `entitysync.ErrSubjectRegisteredElsewhere`（点名已登记它的 Manager），所以工程里另有自建 Manager 登记 Player 时不能同时打开 self_sync。

### 谁看见谁：视野（View，D19）

`policy.Interest` 把玩家的视野归并成订阅。编号规则：**玩家 ID** 就是 Manager 的会话 ID；被看的目标是**实体 ID**；**视野 ID**（`policy.ViewID`，int64）由业务分配与解释——联盟号、副本阵营、眼位、单位都行，框架不解析，也不要求它是实体 ID。`0` 保留给玩家主视野（`policy.MainView`），业务视野不能用 0。

视野分两类，玩家与视野多对多：

| 视野 | 是什么 | 给的同步层级 | 入口 |
| --- | --- | --- | --- |
| 空间视野 | 一个视点 + `ObserverView{LOD, Radius}` + 可选跟随实体 | 按距离档：`InterestConfig.BandLayer(band)`（缺省 0 档 default、n 档 `bandN`）；额外块覆盖 `BlockLayer` | `DefineSpatialView(id, at, sight, follow)`、`MoveSpatialView`、`SetSpatialView`（服务端改视野，不限速）、`SetViewFollow` |
| 关系视野 | 一个显式的实体集合（联盟、队伍、好友、阵营全体单位） | 定义时给的一个层级 | `DefineRelationView(id, layer)`、`SetViewMembers` / `AddViewMembers` / `RemoveViewMembers` |

`AttachView(playerID, id)` 把视野挂给玩家，`DetachView` 解除，`RemoveView(id)` 移除视野并自动从所有玩家解除（之后同一 ID 可重新定义）。一个联盟一份关系视野挂给全体成员，成员加入、退出只改这一份集合和挂接；副本阵营的每个单位一个跟随它的空间视野，挂给阵营里每个玩家——任何一个单位看得见，全阵营都看得见；RTS、召唤物是一个玩家挂多个空间视野。读：`ViewPlayers`、`ViewMembers`、`ViewVisible`、`Seen(playerID)`（玩家经任一视野持有的全部目标，诊断用）。

**合并**：`(玩家, 目标)` 只要有任一挂接的视野持有就保持订阅，最后一个释放才退订，几个视野之间不会互相退订（主视野走开、眼位还看着就不退）。

**同步层级**：层级就是一个 `SyncProfile`，字段沿用现有的视图白名单——用 `dataengine.NewSyncViewSet` 按 DAO 字段名列出：

```go
var playerViews, _ = dataengine.NewSyncViewSet(db.PlayerDaoSyncFields(),
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "near"}, Fields: []string{"Name", "Level", "PosX", "PosY", "HP"}},
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "far", LOD: 1}, Fields: []string{"Name", "PosX", "PosY"}, Priority: 1},
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "alliance"}, Fields: []string{"Name", "AllianceRank", "Power"}},
)
```

同一目标被几个视野看见时：层级之间是包含关系（一个的字段是另一个的子集）就只订阅数据多的那个，身份就是它本身；不是包含关系就**各层级的数据都同步**——订阅它们的并集 `entity.UnionSyncProfiles(...)`（`Key` 为各层级身份排序后用 `+` 连接，例如 `alliance+far@1`），`SyncViewSet.Lookup` 把并集解析成字段的并集，`NewMaskedSyncPacker` / 按 `Lookup` 打包的 packer 不用改。包含关系按 `InterestConfig.ViewSets` 里每个实体类型的字段判断；不配 `ViewSets` 时不知道字段，不同层级一律取并集身份。业务视图 Key 不能含 `+`、`@`。

**主视野**：玩家 `Enter` / `Watch` 时自带一个空间视野（镜头），只挂给自己，计入挂接数；D14 的入口都作用于它：

| 登记 | 看 | 被看 | 移动 | 退出 |
| --- | --- | --- | --- | --- |
| `Enter(playerID, entityID, at, view)` 玩家 | 主视野跟随 `entityID`；经 self 看见自己 | 是 | `Move(entityID, to)`（跟随它的视野一起移动）；只动视点用 `MoveView(playerID, to)` | `Leave(playerID, entityID)` |
| `Watch(playerID, at, view)` 观战 / GM | 主视野不跟随实体、没有 self | 否 | `MoveView(playerID, to)` | `Unwatch(playerID)` |
| `Show(entityID, at)` 怪物、掉落 | 否 | 是 | `Move(entityID, to)` | `Hide(entityID)` |

已用 `Enter` 登记的玩家不能直接改成 `Watch`（`ErrInterestConfig`，先 `Leave`）；同一玩家再次 `Enter`（重连）按这次的位置与视野重新登记，要保留原视野就传 `ObserverView(playerID)` 的结果（demo 就这样做）。`Leave` 解除玩家的全部挂接、撤掉主视野与 self，并把实体从所有关系视野的成员里去掉；业务视野本身保留，玩家重新进场后由业务重新挂接。`Hide` 也把实体从关系视野成员里去掉。

**跟随**：空间视野跟随一个实体时视点取实体位置、随实体移动（`Move`、事务内的 `QueueMove` / `QueueSpatial`），并且看不见它自己跟随的实体；`SetViewFollow(id, 0)` 不再跟随，视点停在当前位置；换成跟随另一个实体时跳到那个实体的位置。实体 ID 数值恰好等于某个玩家 ID 的单位照常可见。

**视野操作不进 Nest、不排队**（维护者 2026-10-20 决定）：定义、修改、成员变更、挂接都是直接调用，只取视野登记表自己的锁，参数当场校验并写进登记表，效果由下一次 `Apply`（或排队模式下 Manager 的政策阶段）按调用顺序应用，不等待空间计算，可以在业务线程里调用。它们不走提交门：事务里决定的视野变化（入盟、放眼、组队）由业务在**提交之后**调用。位置不同：它是 DAO 驱动的事实，`QueueMove(e, at)` / `QueueSpatial(e, at, blocks)` 仍走提交门，提交确认后移动目标并带上跟随它的视野，回滚不移动。

**上限**：`MaxVisible` 与 `MaxObserverBlocks` 按每个空间视野计；`MaxViewsPerPlayer`（缺省 64，含主视野）与 `MaxPlayersPerView`（缺省 4096）超出时 `AttachView` / `Enter` / `Watch` 返回 `ErrViewLimit`，计 `entitysync_interest_view_limit_rejected_total{reason="views_per_player"|"players_per_view"}`；启动期按 `worstObserverBlocks × MaxViewsPerPlayer` 核算一个玩家的块预算，超过 `MaxPlayerBlocks`（缺省 `1024 × 64`）拒绝构造（`ErrInterestBudget`）。一个会话持有的目标还受 Manager 的 `Limits.MaxObjects`（缺省 100）约束，一个玩家挂很多视野时一并调大。

**已测规模与边界**（2026-10-20 夜间 sync-aoi：1000 玩家、1 万实体、20 Hz、`-mode=periodic`，本机两进程 loopback，每格 3 个样本，[夜间补测](../../performance/NIGHT-20261020.md) §14'.2；periodic 的周期等待本身最多 50 ms，单视野 Nest 也在 64 ms 左右，比较看差异）：

- **每玩家额外空间视野 4 个以内已测**：可见约 244，Nest 模式改值→客户端 P99 78 ms（direct 49.5 ms），帧数全满。
- **16 个额外空间视野时 Nest 模式失守**：1000 玩家 × 17 个空间视野（1.7 万个空间观察者，可见约 827），改值→客户端 P99 350～376 ms，服务端 tick 工作 P95 86～90 ms、超过 50 ms 的 tick 间隔，帧数只有应发的 38%（tick 被跳过、变化合并）；同负载 direct 模式正常（P99 58～62 ms、帧数全满）。
- **共享关系视野正常**：联盟 1000 人、每人可见约 1042，Nest 66 ms、direct 50 ms，与单视野同量级。
- 所以瓶颈是空间视野的数量在 Nest 路径上的评估，不是下发的实体数；具体原因尚未确认，维护者决定发版后再分析（[KNOWN-LIMITS“广播与 AOI”](../../maintenance/KNOWN-LIMITS.md#广播与-aoi)）。一个玩家要挂十几个空间视野的玩法（RTS、大量眼位 / 召唤物）先按自己的密度压测。

**视野参数**（D14 B1）。每个空间视野有自己的 `ObserverView{LOD, Radius}`，零值是缺省视野：

- `AOIConfig.LODs` 是 LOD 层级表，下标即层级号，每层一组距离分档（规则同 `Bands`）；不配时只有层级 0、分档取 `Bands`，两者不能同时配。
- 视野大小（`Radius`，即进入半径）允许范围 `[MinViewRadius, MaxLeaveRadius − 滞回量]`，按 `ViewRadiusStep` 从 `MinViewRadius` 起向下取整；离开半径 = 视野大小 + 滞回量，滞回量取缺省视野的 `LeaveRadius − EnterRadius`。零值：最小取 `EnterRadius`、最大取 `LeaveRadius`、步长 1，即只允许缺省视野。`EnterRadius` 必须落在量化网格上。
- 启动期检查：单个视野的块预算（`MaxObserverBlocks`）按 `MaxLeaveRadius` 算，超出拒绝（`ErrInterestBudget`）；每层分档递增、为正、不超过 `MaxLeaveRadius`。运行期的视野一律先归一，不会越过启动期检查过的范围。

客户端的 AOI / LOD 请求只改玩家主视野：接入层收到 `(lodLevel, viewRadius)` 后调用 `Interest.SetObserverView(playerID, ObserverView{LOD, Radius})`，与移动请求一样直接进政策、随后 `Apply`：

- 层级必须是配置里的，否则 `ErrObserverView`；视野大小由服务端夹取并量化，防止改过的客户端拉大视野多看单位、放大订阅与带宽。
- 量化后与当前相同的请求什么也不做、不占限速；同一玩家距上次生效不足 `InterestConfig.ViewChangeInterval`（缺省 1 秒，负数拒绝构造）的请求返回 `ErrObserverViewRate`。两种拒绝都计 `entitysync_interest_view_rejected_total{reason="rate"|"lod"}`。
- 生效后完整重评：视野变大，新进入的目标订阅、下发全量；变小，超出离开半径的退订；只换层级，按新分档重发视图。返回值是此后生效的视野（拒绝时是当前视野），接入层回给客户端。

game-demo 的接法：`scene_view` 请求（消息 10025，`SceneViewRequest{LodLevel, ViewRadius}`）→ `HandleSceneView` → `Scene.SetView` → `SetObserverView` + `Apply`；未配置的层级、超频、不在场景分别回 `scene_view_lod`（100022）、`scene_view_rate`（100023）、`scene_position`（100013）。队伍是每队一个关系视野（`Scene.SetTeam`）。SLG 夹具（`scripts/testdata/slg/alliance`）的联盟是一个共享关系视野（成员是联盟共有的据点），侦察是玩家私有的关系视野，两者各自持有同一个据点，最后一个释放才退订。

## 3. periodic 与 on_change

periodic 按 Interval 合并检查；on_change 在正式 handler 提交边界汇总：setter 只标脏，持 Guard 冻结需要的视图，解锁且满足持久确认后唤醒。唤醒可合并，pending 才是事实来源，不能依赖每次通知恰好被接收。两种模式共用订阅、版本和线协议。

配置 DurableWatermark 时，冻结状态的 CommitLSN 超出水位则暂不发送。不要把 MarkDirty 的时刻当成客户端可见时刻，也不要把 on_change 描述成每个 setter 都立即发包。

### 可配置合批窗口（v1.26.0 新增）

希望将短时间内多次已提交更新集中处理，可以设置：

```yaml
sync:
  entity:
    mode: on_change
    interval: 50ms
    on_change_batch_window: 5ms
```

代码配置对应 `entitysync.ManagerConfig.OnChangeBatchWindow`。缺省为 0，保持原来的立即处理唤醒；正值从循环收到第一次唤醒时开始计时，后续通知不延长这次窗口。周期检查可提前触发，因此持续写入不会一直等下去。5ms 是示例取舍，不是端到端延迟保证：Flush 执行、队列、提交确认、网络和重试仍需要时间。

只有 on_change 能设置非零窗口；负值或 periodic 搭配非零值会拒绝启动。配置文件显式写 0 可覆盖代码中的非零窗口，`EntitySyncSetup.Configure` 最后执行，仍有最终覆盖权。启动日志会打印实际窗口。

手动 `Flush`、`Drain` 和 `Stop` 的最后一次 Flush 不等窗口；周期 Interval 继续作为兜底与原有快照预算窗口，不能把合批配置当成提高冷创建额度。该选项只改变发送循环时机，不减少 handler 的捕获次数，也不改变提交水位或可靠增量规则；冻结数据可能保留更久，应按实际内容测量峰值内存和延迟。单独执行 Flush 不会取消循环中已经安排的计时器，之后可能多一次空 Flush。

## 4. 背压与恢复

状态全量、增量、remove 走可靠有序通道。Push 返回 nil 仅表示传输接纳；ErrRetryLater 保持脏状态等待重试。两种玩家接入都是有界异步出站：嵌入 TCP 的 `PushSync*` / `PushLockstep*` 只进入该连接的出站队列，独立 Gate 走 Game 有序出站口；读得太慢的客户端被关闭，不会拖住同一次 Flush / Tick 里排在它后面的会话；整个进程的出站驻留预算（独立 Gate 下 Game Ingress 与嵌入 TCP 各一个）耗尽时推送被拒、不关连接，Sync 整轮重试（[GATEWAY §4 嵌入 TCP 的出站](../GATEWAY.md#嵌入-tcp-的出站)）。基线缺口、epoch 切换或未知对象必须请求/等待全量恢复，客户端不继续在错误基线上应用 delta。会话替换与旧异步回调必须按代际核对。

### 停机排空（Drain）卡住时看什么

`Manager.Drain(ctx)` 循环 Flush 直到没有待处理条目：预算 / 水位暂缓等下个周期，传输失败直接返回错误。被永久暂缓或拒绝的条目（持久水位不再前进、提交确认一直不到、政策事实一直不能应用）会让 Drain 等到 ctx 结束；Drain 不会放弃它们（DESIGN-TODO D3，维护者 2026-10-15 决定只做诊断增强，行为不变）。ctx 结束时返回 `*entitysync.DrainStalledError`：

- `errors.Is(err, context.DeadlineExceeded / context.Canceled)` 照常成立；
- `Stalls` 最多列 `DrainStallLimit`（8）项、`Total` 是全部条目数。每项有来源（`subject` 待捕获、`snapshot` 等冷创建预算、`policy` / `interest` 政策阶段）、subject 与会话、最后一次暂缓或拒绝的原因（`commit_pending`、`durability_wait`、`prepare_failed`、`snapshot_budget`、`subscribe_refused`、`policy_pending` 等）、补充说明、已连续等待的时长和最后一次错误；
- 同时计 `entitysync_drain_timeout_total`，并把剩余条目数写入 `entitysync_drain_remaining_entries`（成功排空写 0）。

自定义政策用 `Manager.RegisterPolicy(entitysync.PolicyHooks{Apply, Pending, Stalls})` 登记；`Stalls` 可选，不提供时超时错误只报告“该政策仍有待处理事实”。Interest 的被拒订阅连续 10 次后不再阻挡 Drain（`entitysync_interest_retry_stalled_total`），所以出现在列表里的 `subscribe_refused` 都还在这个上界之内。

## 5. 服务间总线与流

SyncBusMod 的主题前缀缺省 `roost.sync`，与驱动缺省、生成配置一致；JetStream 流名显式 `syncbus.stream` 优先，否则由前缀推出（`roost.sync` → `ROOST_SYNC`，`zz.sync` → `ZZ_SYNC`），共用一个 NATS 的不同前缀各有各的流。没有按旧前缀的特例映射：要沿用已有的流与 durable 游标只能显式写 `syncbus.stream`；新流与已有流的 subjects 重叠时启动明确失败，不静默换流。

JetStream SyncBus 默认有限投递次数 5，不能宣称无限重试或绝不丢。topic 创建/退役期间拒绝冲突订阅，旧 consumer 和在途回调排空后才能重订。无 handler 准入不 ACK；业务错误与坏信封按各自规则结算。流容量、保留期限与次数上限均可能终止投递，恢复由快照或业务对账承担。

### SyncBus 的定位与丢失兜底

**定位（维护者 2026-10-15 决定）**：SyncBus 默认当作**可丢广播**。普通 NATS 驱动最多一次；JetStream 驱动在 MaxDeliver、MaxAge、MaxBytes 之内补投，超出就终止，终止只计数（`nats.jetstream.terminal.total{reason="max_deliver"}`，见[指标清单](../OBSERVABILITY.md#syncbus-有限重投与恢复rr-25)）并记日志，框架不自动修复订阅方。只用于“丢了也无妨或下一条会覆盖”的数据（在线提示、可重算的缓存失效通知）可以直接用；**需要最终一致的状态必须由业务做“丢失检测 → 拉快照对账”闭环**，不能只测重试成功。框架内的用法也遵守这一点：Remote 快照推送在拿不到可确认订阅时退化为按需读取，`PatchSyncer` 只承载瞬时补丁、不保存状态。

推荐写法（可编译、可运行的完整示例见 [syncbus/example_test.go](../../../framework/sync/syncbus/example_test.go) 的 `Example_reconcileAfterLoss`，随 `go test ./framework/sync/syncbus` 执行并核对输出）：

1. **版本是每个 key 的连续序号**：权威方每改一次该 key，`SyncMsg.Version` 加 1，增量与快照共用这一序列。`MessageID` 是投递身份，不能拿来判断丢失。
2. **序号缺口**：订阅方记录每个 key 已整体正确的版本 `applied`。`Version <= applied` 是旧消息或重投，丢弃；`Version == applied+1` 才应用增量；`Version > applied+1` 是缺口，停止应用这个 key 的增量，转去拉快照。
3. **版本落后 / 投递耗尽**：缺口只在后面还有消息时才看得见。末尾一条丢失（或订阅方停机期间超出保留期）必须靠**周期对账**发现：定时、启动或重订之后、收到投递终止告警之后，向权威方取各 key 的版本摘要，本地落后的逐个拉快照。
4. **拉快照对账**：快照来自权威方（RPC、权威存储、Remote 快照读等，由业务决定），按快照整体替换该 key 并把 `applied` 设为快照版本；快照版本可能已超过触发它的消息，之后到达的旧增量自然按第 2 步丢弃。拉取失败保持旧版本、等下一次触发，不能把“没拉到”当成已一致。

```go
// 订阅回调的核心判断（完整代码见 example_test.go）。
switch current := r.applied[msg.Key]; {
case msg.Version <= current:
    return nil // 旧消息或重投
case msg.Version > current+1:
    return r.resync(ctx, msg.Key) // 序号缺口：拉快照整体替换
}
r.values[msg.Key] += delta.Add
r.applied[msg.Key] = msg.Version
```

消息本身携带整体状态（而不是增量）时，缺口不会算错，只需“版本更大才覆盖”加第 3 步的周期对账。对账周期、摘要的分页与限流、权威方的快照负载都由业务按数据量决定。

框架级对账（订阅端检测到缺口或投递耗尽时自动向发布端请求快照，复用 Remote 快照 / Entity Sync 全量机制，并提供指标与告警）是**下一阶段候选**，尚未实施；需要先确认哪些业务在 SyncBus 上承载需要最终一致的状态，再定义快照请求协议、限流与发布端负载上限（[DESIGN-TODO D2](../../maintenance/DESIGN-TODO.md#d2-syncbus-有限投递的兜底)）。

syncstream 碎片 TTL 在新多片输入时清扫，不是独立定时器。FileJournal 直接并发 Append 可合批，但单一 History.Record 的写锁串行化不能据此承诺每业务记录减少 fsync。订阅停止、回调排空、journal 生命周期须一起关闭。

## 6. Lockstep

客户端 Command 提供 Input/Hash/Catchup；Match 根据当前 session 映射席位，不信任客户端自报 seat。Match 汇集输入、广播 C7 帧和历史追帧；历史与 hash 报告默认按保留窗口自动滚动裁剪（下节）。落后的会话按 `MatchConfig.Recovery` 追帧或收全量快照（2026-10-20 起，见[落后的会话怎样回来](#落后的会话怎样回来追帧快照或自动)）。输入校验、hash 判定、重连恢复与游戏模拟分开。两人不形成多数时 NoMajority 不等于已经识别作弊者。

### 长局如何控制内存：历史保留窗口

`MatchConfig.HistoryRetainFrames` 决定保留多少帧（含最新帧），维护者 2026-10-15 决定默认开启自动裁剪（DESIGN-TODO D1 方案 C）：

| 配置 | 行为 |
| --- | --- |
| 0（默认） | `DefaultHistoryRetainFrames` = 3600 帧：60Hz 下 60 秒、30Hz 下 120 秒。以 10 席、每帧每席 8 字节估算约 1.5MB / 局，与对局长度无关 |
| 正数 | 自定义窗口，按“允许断线秒数 × 逻辑帧率”换算；必须不小于 `MinHistoryRetainFrames`（= `ReplayHorizon` = 64，落后这么多帧的客户端在 Sequencer 看来仍在局内，它的追帧必须可服务；也覆盖一整页追帧与最大冗余深度），否则 `NewMatch` 返回 `ErrMatchConfigInvalid` |
| `lockstep.RetainFullHistory`（-1） | 关闭自动裁剪：历史与 hash 报告随对局长度增长，宿主自己调用 `TrimHistory` / `TrimHashReports`（需要整局回放且不另行持久化时才这样配） |

裁剪在每次 `Tick` 末尾执行：先发本次追帧页（页面在发出前已编码成独立字节），再裁掉窗口之前的帧；hash 报告按 64 帧步长跟随同一窗口裁剪，所以报告表最多比窗口多一个步长。宿主仍可调用 `TrimHistory` / `TrimHashReports` 裁得更多（例如 hash 判定窗口比断线窗口短），但裁掉的帧找不回来。`History.ReadRange` 和 `Replay` 返回只读视图，仅在下一次 Append/裁剪前有效；需要长期保存回放时自行复制并持久化。

### 落后的会话怎样回来：追帧、快照或自动

客户端发现缺帧（冗余补不上）后只发一种请求 `OpCatchup(from)`；走追帧还是走全量快照由服务端按 `MatchConfig.Recovery` 判定，客户端从收到的第一条下行消息知道结果（C7 历史页、CA 快照分块或 C9 失败通知）。快照恢复是 2026-10-20 实施的 DESIGN-TODO D1 方案 B，设计、各期偏差与证据见[功能设计](../../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md)。

| `Recovery` | 行为 |
| --- | --- |
| 零值 `RecoveryUnset` | 没配 `Snapshots.Provider` 时等于 `RecoveryCatchup`（现有工程逐字节不变）；配了时等于 `RecoveryAuto` |
| `RecoveryCatchup` | 只追帧；起点早于保留窗口即失败、客户端终止（[越界的处理](#越界的处理按恢复方式区分)） |
| `RecoveryAuto` | 缺的帧都还在窗口内、且缺口不超过 `Snapshots.AutoCatchupMaxFrames` 时追帧；越界或超阈值发快照 |
| `RecoverySnapshot` | 每个恢复请求都先发快照，再从快照帧之后追帧 |

请求时的判定（`start` 是仍保留的最早帧，缺口 = 最新帧 + 1 − `from`，`from` 为 0 按 1）：

| 模式 | 窗口内，阈值为 0 或缺口不超过阈值 | 窗口内，缺口超过阈值 | 起点已被裁掉 |
| --- | --- | --- | --- |
| catchup | 追帧 | 追帧（catchup 不看阈值） | 失败通知 |
| auto | 追帧 | 快照；取不到快照时回退追帧，不通知客户端 | 快照 |
| snapshot | 快照 | 快照 | 快照 |

- 追帧途中游标被窗口越过：catchup 发失败通知；auto / snapshot 转入取快照，不通知客户端。
- 恢复中的会话再发 `OpCatchup`：起点不早于游标是客户端重试，照原样继续（snapshot 模式也不为重试重发快照）；快照在取或在传时任何请求都是空操作；更早的起点才重新判定。
- 宿主可调 `Match.StartSnapshot(player)` / `SpectatorSnapshot(session)` 不论模式都走快照（例如 `OnDesync` 判出离群后用服务器参考状态把它拉回；是否这样处置离群者是业务的选择），没配 Provider 返回 `ErrSnapshotNotConfigured`。客户端没有“请求快照”的上行命令：让客户端强制快照等于给恶意客户端一个廉价的放大手段。
- 旁观者与座位用同一套判定。

**配置**（`MatchConfig.Snapshots`，`NewMatch` 时校验一次，违反返回 `ErrMatchConfigInvalid`；snapshot / auto 还要求配置了 `Reliable`）：

| 字段 | 缺省 | 说明 |
| --- | --- | --- |
| `Provider` | — | 业务快照来源；nil 时其余字段不生效 |
| `AutoCatchupMaxFrames` | 0 | auto 的缺口阈值；0 只按窗口判断，窗口内与 catchup 行为相同。模拟重的游戏按“客户端愿意花在重算上的秒数 × 逻辑帧率”配（60Hz、最多重算 10 秒 → 600） |
| `MaxBytes` | 4 MiB（`DefaultMaxSnapshotBytes`） | 快照上限，不得超过硬上限 64 MiB（`MaxSnapshotBytes`）；超过是确定失败，不重试 |
| `ChunkBytes` | 16 KiB | 一个分块的数据字节；加上分块头（`MaxSnapshotChunkHeaderBytes`）不得超过可靠通道声明的单条上限（`nettransport.ReliablePayloadLimiter`，TCPSender、KCP、QUIC、`gate.Transport` 都实现） |
| `BytesPerTick` | 64 KiB | 发送随链路放慢的可靠通道（实现 `nettransport.ReliablePacer` 且返回 true：`TCPSender`、KCP、QUIC）上每会话每 Tick 最多发起的快照字节，是上限，真实速度由链路决定 |
| `UnpacedBytesPerTick` | 16 KiB | 只准入的可靠通道（独立 Gate 的 `gate.Transport`）上每会话每 Tick 的快照字节，就是发送速率，见下文独立 Gate |
| `FetchTimeout` | 2s | 限一次 Provider 调用 |
| `PinFrames` | = `HistoryRetainFrames` | 恢复期间历史最多比窗口多留的帧数（最坏两倍窗口） |

每批分块至少发一块，所以每 Tick 实际发起的字节是预算向上取整到整块（缺省两者都是 16 KiB 的整数倍）；把预算调到小于 `ChunkBytes` 时要同时调小 `ChunkBytes`，否则速率不会低于“一块 / Tick”。

**快照来源怎么写**：

- 接口 `SnapshotProvider.Snapshot(ctx, SnapshotRequest) (Snapshot, error)`，函数可用 `SnapshotProviderFunc` 适配。返回的 `Snapshot.Frame` 必须落在 `[MinFrame, MaxFrame]`：`MinFrame` = 发起时仍保留的最早帧 − 1，`MaxFrame` = 已切出的最新帧（配了已提交门时不超过已提交帧，见下）；`Frame` 的含义是“依次应用帧 1..Frame 之后的状态”，0 表示开局初始状态。`Data` 非空、交出后不再修改（多个恢复中的会话共享同一份，框架不复制）。`StateHash` / `HasStateHash` 是 `Frame` 处的业务状态 hash（与关键帧 `ReportHash` 同一算法），推荐必填：客户端恢复后核对它，是“这份快照在客户端上能用”的唯一端到端证据；用默认输入链 hash 的客户端（`robot.FrameHasher`、C# `LockstepInputHasher`）要求它就是 `Frame` 处的输入链值。此刻没有合适的快照时返回 `ErrSnapshotUnavailable`。
- 调用线程：Match 在自己起的 goroutine 上调用，不是调用 `Tick` 的协程，也不是 Nest worker；`Tick` 只非阻塞地收结果、从不等 Provider。实现可以阻塞，但必须响应 ctx（`FetchTimeout`）；不响应时 Match 到期按超时计失败、不再等它，下一 Tick 可能再发起一次（“同一局至多一次在途”只对遵守 ctx 的实现成立）。实现必须并发安全，不得直接读房间 owner 独占的模拟状态；panic 被 recover、按取快照失败处理并记 ERROR 日志。
- 检查点写法（推荐）：房间 owner 每切一帧照常推进服务器参考模拟，每 K 帧把参考状态编码成不可变的 `Snapshot`、经 `atomic.Pointer` 发布；Provider 只读最新发布的一份，不在请求范围内就返回 `ErrSnapshotUnavailable`。完整可运行的样例是 [example_snapshot_test.go](../../../framework/sync/lockstep/example_snapshot_test.go) 的 `Example_snapshotRecovery`（随 `go test ./...` 运行并核对输出）；检查点缓存是样例里的业务代码，框架不导出。另一种合法写法是 Provider 把请求投进 owner 的邮箱、在 ctx 内等回复（owner 的 `Tick` 从不等 Provider，不会互等）。业务形态的样例见 MOBA 夹具的 `ArenaCheckpoints`（[MOBA 手册 §5.7](../MOBA-INTEGRATION.md#57-重连宿主代际与交接)）。
- 窗口下限：配了快照后，窗口不再决定“断线多久还能回来”，但仍须覆盖 `max(AutoCatchupMaxFrames, K + 取快照延迟对应的帧数 + 余量)`；窗口短于检查点间隔时，最新检查点可能已早于 `MinFrame`，取快照一直是 `ErrSnapshotUnavailable`。`NewMatch` 不检查这一项（K 是业务的）。缺省窗口仍是 3600 帧，没有随快照缩短。
- 已提交门（`MatchConfig.CommittedFrame func() FrameID`，2026-10-20）：给“先切帧、后提交、提交后才广播”的宿主用——实时广播由宿主暂存、权威提交成功才发，而追帧页与快照分块由 Match 的后台 goroutine 直接经可靠通道发出，`Tick` 刚切出的帧在提交前就已在历史里，没有这道门就会被发出去。配置后追帧页只覆盖 `<= min(最新帧, CommittedFrame())` 的帧，游标已越过它的会话本 Tick 不发页、保留恢复状态；取快照的 `MaxFrame` 取同一个值；空闲的恢复会话一旦落进本 Tick 冗余包的覆盖范围就切回实时（之后收的是宿主提交后才发的广播）。nil（缺省）表示切出即提交，行为不变。回调在调用 `Tick` 的协程上调用、只在有恢复中的会话时调用，不得阻塞。宿主的提交落后切帧超过 `RedundancyDepth - 1` 帧时，恢复中的会话会一直经可靠通道逐页跟随、不切回实时。检查点也只能在提交成功后发布。样例是 MOBA 夹具的 `RecoverRoomWithSnapshots`（[MOBA 手册 §5.7](../MOBA-INTEGRATION.md#57-重连宿主代际与交接)，设计见 [FEATURE §15](../../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#15-room-接入已提交门2026-10-20)）。

**下发、续帧与钉住**：

- 一局同一时刻至多一次取快照在途，同时等快照的会话共用这一次的结果；每 Tick 至多发起一次；没有会话再等时取消在途的取快照。
- 快照以 `CA 01` 分块经可靠通道、与 C7 / C9 同一路由下发，首块带帧号、总长、可选状态 hash 与 SHA-256 摘要。每会话同一时刻一批在途，一批发到本 Tick 的字节预算为止，整批受 `CatchupSendTimeout` 约束；游标只前进到已确认写出处，失败计一次、下一 Tick 从已确认偏移重发（客户端丢弃重复的分块）。传完后从快照帧 +1 起发普通 C7 历史页，追到头再接回实时广播；恢复期间会话不收实时广播，实时广播在 `Tick` 里先于恢复发出，一个慢会话只拖慢它自己的恢复。
- 恢复期间历史按会话钉住：等快照时钉住进入快照阶段时的窗口起点，拿到快照后钉快照帧 +1，续帧时钉游标，回到实时即释放；最多比窗口多留 `PinFrames` 帧，超过的会话放弃恢复、收到 `recovery_expired`。所以一次快照恢复（取快照 + 传输 + 续帧）要在约 `PinFrames / 逻辑帧率` 秒内完成（缺省 3600 帧，30Hz 下约 120 秒）。宿主手动 `TrimHistory` 不受钉住约束。
- 失败计数与追帧共用 `CatchupMaxFailures`（缺省 8）：取快照失败、分块发送失败、续帧页发送失败都计入，有进展即清零。

**失败与客户端表现**（失败通知 C9 v2：`C9 02 reason from start`）：

| 情况 | 服务端 | 客户端 |
| --- | --- | --- |
| catchup 模式越界 | 请求时返回 `*CatchupUnservableError`，通知 reason 1 `history_trimmed`（[越界的处理](#越界的处理按恢复方式区分)） | 终止本场 |
| 取快照连续失败达到 `CatchupMaxFailures`（`ErrSnapshotUnavailable`、其他错误、超时、panic、帧号越界或空数据 `ErrSnapshotInvalid`） | `Tick` 错误，通知 reason 2 `snapshot_unavailable` | 终止本场 |
| 快照超过 `MaxBytes` | 确定失败、不重试：`Tick` 错误带 `ErrSnapshotTooLarge`，立即通知 reason 3 `snapshot_too_large` | 终止本场 |
| 钉住超过 `PinFrames` | `Tick` 错误，通知 reason 4 `recovery_expired` | 终止本场 |
| 分块或续帧页连续发送失败达到 `CatchupMaxFailures` | 放弃这次恢复、不发通知（链路本来就坏了），会话回到实时广播 | 缺口仍在，按重试规则再请求；服务端重新判定，快照换新的传输编号从头发 |
| 会话被 Detach（断线、换连接） | 恢复状态随会话清掉，计 `failed{detached}`（每会话一次） | 重连后再请求，快照从头重传（不续传） |

所有失败原因 `errors.Is(err, lockstep.ErrCatchupUnservable)` 都成立（`*CatchupUnservableError.Reason` 区分原因），客户端“终止、交业务处理”的判断点不变。

**客户端怎样恢复**：

- Go：`FrameAssembler.Receive(packet)` 是支持快照的主入口，处理 C7 / C9 / CA，返回 `Delivery{Frames, Snapshot}`。拿到 `Snapshot`（已重组、SHA-256 已核对，`Data` 是调用方独占的副本）后，业务恢复状态并核对状态 hash，再调 `ResumeAfterSnapshot(snapshot.Frame)`：下一帧期待 `Frame + 1`，乱序缓冲整个清空（里面都是恢复前的实时帧，服务端之后会重发）；在此之前再投包返回 `ErrSnapshotNotApplied`。传输进行中收到的 C7 帧丢弃（`SnapshotInProgress()` 为真）。`Ingest` 只服务只会追帧的调用方，遇到快照分块返回 `ErrSnapshotUnsupported`。客户端愿意接受的快照上限用 `NewFrameAssemblerLimits(maxBuffer, maxSnapshotBytes)` 设（缺省 4 MiB），首块声明的总长超过它即坏包、不分配内存。
- `robot.LockstepBot`：`LockstepBotConfig.Restore` 用快照替换模拟状态、返回恢复后的状态 hash（与 `Hash` 同一算法），bot 核对后从快照帧 +1 续帧、以它重置输入链；`MaxSnapshotBytes` 是 bot 的快照上限；`ReportSnapshotMismatch`（缺省 false）为 true 时，状态 hash 不符的 bot 在终止前发一次 `ReportHash(快照帧, 算出的 hash)` 留证；`Stats().SnapshotsRestored` 计成功恢复次数。没设 `Simulate` / `Hash` 的纯线协议 bot 不设 `Restore` 也能以快照的 `StateHash` 续输入链；其余 bot 没设 `Restore` 时收到快照即终止。
- C#：`LockstepClient(..., restore, maxSnapshotBytes)`，`restore` 在调用方（引擎主线程）同步执行，见 [client README 快照恢复](../../../client/README.md#快照恢复)。
- 校验失败一律终止、不自动重试：坏分块（Go `ErrFrameCorrupt` / C# `InvalidDataException`）、摘要不符（`ErrSnapshotCorrupt` / `LockstepSnapshotCorruptException`）、恢复回调出错、状态 hash 不符（`*SnapshotMismatchError`，`errors.Is ErrSnapshotMismatch` / `LockstepSnapshotMismatchException`）、没有恢复手段（`ErrSnapshotUnsupported` / `LockstepSnapshotUnsupportedException`）。同一份快照重来结果相同；hash 不符说明客户端版本 / 规则 / 确定性与服务器参考不一致，由业务提示更新或踢出。
- 重连即新实例：C# `LockstepClient` 绑定一个 `TcpSession`，重新登录后要新建，组装器从第 1 帧开始（Go 新建的 `FrameAssembler` 同样）。缺的帧仍在窗口内（auto 缺省阈值 0）时服务端从第 1 帧追帧，业务须先把模拟状态重置到开局；越界才走快照。

**观测**：`lockstep.snapshot.total{reason}`、`lockstep.snapshot.bytes.total`、`lockstep.snapshot.failed.total{reason}`，口径见 [OBSERVABILITY](../OBSERVABILITY.md)。恢复进行中（等快照、传快照、快照之后续帧）的会话被 Detach 计 `failed{reason="detached"}`（维护者 2026-10-20 决定）；独立 Gate 下“断开—重连—从头重传”的循环表现为 `failed{detached}` 与 `snapshot.total{out_of_window}` 同步增长（都不分会话）。

### 越界的处理：按恢复方式区分

起点早于保留窗口的追帧不可能靠追帧补齐。**auto / snapshot 模式**（配了快照来源）下它改走快照，`StartCatchup` / `SpectatorCatchup` / `HandleCommand(OpCatchup)` 返回 nil，只有快照路线最终失败（上表 reason 2～4）才通知客户端终止。**catchup 模式**（没配快照来源时的缺省）下框架给出可判别的失败，客户端表现是“恢复失败、交由业务决定踢出或重开”，不做全量重同步：

- 服务端：`StartCatchup` / `SpectatorCatchup` / `HandleCommand(OpCatchup)` 立即返回 `*CatchupUnservableError`（`errors.Is(err, lockstep.ErrCatchupUnservable)`，`Reason` 为 `RecoveryHistoryTrimmed`，`From` 是客户端缺的第一帧、`Start` 是仍保留的最早帧）；起点 0 按帧 1 处理，同样核对。已在追帧的会话若页面一直发不出去、游标被窗口越过，下一次 `Tick` 的错误里带同一错误。两种情况都计 `lockstep.catchup.unservable.total`（只在 catchup 模式计）。
- 通知：下一次 `Tick` 经可靠通道、与追帧页同一路由给该会话发恢复失败通知（C9 v2，reason 1：`C9 02 01 from start`，`EncodeCatchupUnservable`；其他原因用 `EncodeRecoveryFailure`）。经 TCP Notify 发出的 Catchup 命令没有应答，客户端靠这条通知得知结果；通知发送失败只记进 `Tick` 的错误。
- Go 客户端：`FrameAssembler.Receive` / `Ingest` 收到通知返回 `*CatchupUnservableError`（带 `Reason`）；`robot.LockstepBot` 随即终止（`ErrLockstepBotTerminal`，链上带 `ErrCatchupUnservable`），不再请求追帧。sink 直接把服务端的该错误交回时同样终止。
- C# SDK：`LockstepAssembler` 抛 `LockstepCatchupUnservableException`（`Reason`、`RequestedFrom` / `HistoryStart`），`LockstepClient.HandlePushAsync` 把它记为 `Failure` 后重新抛出，之后的推送一律拒绝。业务据此踢出、回大厅或开新局；不能跳过缺帧继续模拟。

catchup 模式下保留窗口越短越省内存，也越容易让慢追帧失效：断线时长的分布与 `lockstep.catchup.unservable.total` 一起决定是否要调大窗口，或者配快照来源改用 auto。本机已验证一小时逻辑帧、默认窗口下的长局有界和滚动追帧，以及 20000 帧长局里每 1000 帧一次快照恢复时历史峰值有界（`TestMatchLongRunPeriodicSnapshotRecoveryKeepsHistoryBounded`）；这不是一小时真实网络运行或跨引擎确定性的证明，测量见[优化说明](../SYNC-OPTIMIZATION.md)。

### 经嵌入 TCP 追帧：随链路放慢

追帧页由 Match 在后台协程逐页发送（`launchSend` 每页起一个 goroutine），每会话同时最多一页在途；`CatchupSendTimeout`（默认 2s）经 ctx 限制一页的发送时长，超时与其他发送失败一起计入 `CatchupMaxFailures`（默认连续 8 次放弃）。这套限速依赖可靠通道在链路慢时让发送变慢——KCP / QUIC 的窗口满了发送就阻塞。

`lockstep.TCPSender` 的两条通道走同一条连接的出站队列，2026-10-16 起按是否等写出分开（维护者决定方向 A；此前 `1251e7f3` 把嵌入 TCP 推送改为只准入后，两条通道都不等写出，追帧失去链路背压）：

- 实时通道 `SendDatagram`（Tick 给每个接收者的当前帧）接 `TCPRuntime.PushLockstepSession`，只做出站准入、几乎立即返回。Tick 逐接收者串行发送，等写出会让一个停读连接拖住同一 Tick 的所有人；停读连接只会因为自己的出站上限被关闭（[GATEWAY §4](../GATEWAY.md#嵌入-tcp-的出站)）。
- 可靠通道 `SendReliable`（追帧页与追帧失败通知）接 `TCPRuntime.PushLockstepSessionWritten`，入队并等写出。慢链路上一页要等写协程把它写进 socket 才返回，下一页随后才发，追帧速度降到链路速度；该连接里属于追帧的积压只有在途那一页（加上至多一页已被写协程取走、等待超时后仍在写的页），不会因追帧超过出站上限被断开。
- 嵌入 TCP 的全进程出站驻留预算（`player_access.tcp.outbound.resident_bytes`，2026-10-20 起）耗尽时，两条通道的这一帧都当场被拒（`gateway.ErrResidentBudgetExhausted`），连接不关闭、不等预算：实时帧对这个接收者按数据报丢失处理（`Tick` 返回带这个错误的结果，下一个广播包的冗余帧或客户端追帧补上），追帧页计一次追帧失败（`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted`）。
- 一页在 `CatchupSendTimeout` 内没写出：还在排队就撤回、不再写出，已在写的照常写完；计一次失败，下一 Tick 从同一游标重发（客户端按帧 ID 去重）。连续 `CatchupMaxFailures` 次后放弃这次追帧，`Tick` 返回 “abandoned after N failures”（链上带 `context.DeadlineExceeded`），会话恢复实时广播；之后它的实时帧仍只入队，链路依旧跟不上时由出站上限或 `write_timeout` 关闭连接，客户端重连后再追帧。
- 快照分块与恢复失败通知走同一条可靠通道，规则相同：每块入队并等写出，一批（`BytesPerTick`，缺省 64 KiB）在途，快照速度降到链路速度（`TCPSender` 实现 `nettransport.ReliablePacer`）。toxiproxy 限速演练（[2026-10-20](../../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）：256 KB/s、64 KB/s 下 1 MiB 与 4 MiB 快照都恢复、连接不断，客户端传输耗时与“大小 / 速率”之比 1.00～1.04；64 KB/s 下个别批次超过 `CatchupSendTimeout` 后从已确认偏移重发，不影响结果。
- “写出”是写进本机 socket 发送缓冲：服务端认为恢复结束时，慢链路上的客户端可能还差 1 MB 以上没收到（本机发送缓冲自动增长上限 4 MiB，加上沿途缓冲；演练 4 MiB / 64 KB/s 档服务端 39.8s 结束、客户端 69.0s 才回到头部）。之后的实时帧排在这些字节后面，慢链路玩家恢复后头几秒的实时延迟等于缓冲排空时间；没有断连、没有丢帧（由计时与缓冲上限推断，没有单独测量缓冲占用）。

阻塞边界：

- 等写出只发生在 `launchSend` 起的 goroutine 上，不在调用 `Tick` 的协程，也不在 Nest 快池 / 业务 worker 上；`TCPSender` 发送前的 `fctx.BlockingError` 守卫保留，比赛驱动放进业务 worker 会在任何副作用前被拒绝。
- 每页各自一个 goroutine、各自一条连接的写协程，一个慢连接的等待不占其他会话的追帧。`Tick` 对本次发起的所有页最多共等 `CatchupSendWait`（默认 5ms，共用一个计时器），没写完的页交给后续 `Tick` 收取、不再等；所以一个停读的追帧会话最多让发起它那一页的 `Tick` 晚 `CatchupSendWait`。实时广播在 `Tick` 里先于追帧发出，不受影响。
- 回归（[tcp_test.go](../../../framework/sync/lockstep/tcp_test.go)，真实回环 TCP）：`TestACatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（约 8 MiB 历史，客户端先停读再慢读，追帧完成且连接不断）、`TestACatchUpPageTheLinkCannotTakeTimesOutWithoutHoldingOtherSessions`（停读连接的页按 `CatchupSendTimeout` 超时并放弃，同时另一连接的追帧与实时帧照常，`Tick` 不被拖住）、`TestALiveBroadcastOverEmbeddedTCPNeverWaitsForAStalledConnection`（停读连接上实时广播只入队）。

接线：`lockstep.NewTCPSender(messageID, maxPayload, resolve, push, pushWritten)` 的最后两个参数分别是实时与追帧的发送函数，生产接 `runtime.PushLockstepSession` 与 `runtime.PushLockstepSessionWritten`（生成工程用 `Runtime.TCPRuntime`）；接反了，要么实时广播被停读连接拖住，要么追帧又失去链路背压。

生成 game-demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`，2026-10-20 维护者决定）也经 `TCPSender`：`battleLinks` 在嵌入 TCP 下把追帧接到 `Runtime.TCPRuntime.PushLockstepSessionWritten`，实时帧经 `battleLiveLane` 只准入、单个接收者发送失败只记 Debug（冗余帧或追帧补上）。10102 推送的是 Lockstep 包本身（flags=5，不带 PB 包装与 battle id），按接入层 SessionID 寻址：座位在第一条 `BattleInput` 到达的连接上加入（换连接则换一个新的 Lockstep 会话 ID），没说过话的座位不收帧，晚加入的座位靠追帧补齐。回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt` 在生成工程 `internal/service/game` 里，与上面包内用例同一场景。

仍只准入的路径（[KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)“Lockstep 追帧链路背压（独立 Gate）”，维护者 2026-10-20 决定暂不做）：

- 独立 Gate（`wiring/gate.Transport`）：两条通道都经 `GameIngress.PushBound` 进 Game 侧有界队列，再经 NATS 交给 Gate 侧队列与 `WritePacket`；`SendReliable` 返回 nil 只表示 Game 本地准入，追帧同样不随链路放慢。慢链路上 Game 侧队列满时准入返回 `ErrReliableBackpressure`（计一次追帧失败，每 Tick 一次），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时 Gate 关闭连接。要等写出需要 Gate 在 socket 写出后再回执，是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。game-demo 换成独立 Gate 部署（`accessplayertcp.NewIngressMod`）时没有 `TCPRuntime`，`battleLinks` 让追帧页也只准入。
- 独立 Gate 上的快照：`gate.Transport` 不实现 `ReliablePacer`，快照按 `UnpacedBytesPerTick`（缺省 16 KiB，30Hz 下约 492 KB/s）固定节流，这个预算就是发送速率。toxiproxy 演练（[2026-10-20 §3](../../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md#3-独立-gate63-风险-1-实测成立)，30Hz、缺省参数）：1 MiB / 256 KB/s 恢复；1 MiB / 64 KB/s 与 4 MiB / 256 KB/s 每次连接都在同一位置被 Gate 断开、重连后从头再传，三次都没恢复。
  - 是否断开取决于快照大小、每 Tick 预算（乘帧率即发送速率）与链路速度三者，不只看速率：发送快于链路的部分先堆在 Gate 侧 socket 缓冲与沿途缓冲里，堆得下就传完（1 MiB / 256 KB/s 超出约 0.5 MB），堆不下就断开（4 MiB / 256 KB/s 超出约 2 MB）。缓冲能吸收多少随环境变化，这是由两档对比得出的推断，没有单独测量。
  - 断开发生在 Gate 写 socket 时写期限到期（`player tcp: connection cannot take the frame: writev ...: i/o timeout`，`gate.channel.failure_total{phase="socket",role="gate"}` 每次 +1），不是 `outbound.max_age` 250ms 的积压规则。写期限取 `write_timeout`（缺省 5s）与这一帧剩余发送预算中较早者；实测是哪一个先到期尚未确认。
  - 每次重连从头重传：Gate 断开后 Game 侧 `DetachSession` 清掉恢复状态，新连接是新会话、新传输编号，快照不续传；演练中三次的分块数逐字节相同，每次多发 0.8～1.6 MB。演练时（计数实施前）`lockstep.snapshot.failed.total` 不动，只有 `lockstep.snapshot.total{reason="out_of_window"}` 每次重连 +1；2026-10-20 起每次断开另计 `failed{reason="detached"}`，这个循环在失败指标上可见。
  - 配置建议（按实测）：先估最大快照大小 S 与要支持的最低下行带宽 B（扣掉同一连接上 Sync / PB 的占用）；让 `UnpacedBytesPerTick × 逻辑帧率` 不高于 B（再留余量，例如取一半），预算小于 16 KiB 时把 `ChunkBytes` 一起调小（每批至少一块）；传输时长约 `S / (UnpacedBytesPerTick × 帧率)`，要短于 `PinFrames / 帧率`，期间这名玩家不收实时帧。1 MiB 级快照在链路略低于预算时实测能靠缓冲传完，大快照不能指望缓冲。调大 Gate 的 `write_timeout` / 出站预算没有测过，不作为建议。做不到时用嵌入 TCP / KCP / QUIC（随链路放慢），或等 Gate 写出回执。

## 7. 公共客户端协议

RS v2 的 16 字节头保留 MsgID、Seq、PayloadSize；Flags bit0 是 push，bits1..2 区分 PB=0、Sync=1、Lockstep=2，值3及保留位拒绝。Lockstep 上行 flags=4，push=5。MsgID 0 的心跳不能携带业务类型标记。

C# 客户端提供 TCP、Sync receiver、Lockstep codec/assembler，Unity 提供主线程适配源码。Go wire 与生成 TCP 共享头验证；业务载荷类型须与注册路由一致。正式 Go/C# 及 Unity 源码测试不代表 Unity/Godot/Unreal 实机已验收；原生 C++ 客户端不在本版交付范围。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
