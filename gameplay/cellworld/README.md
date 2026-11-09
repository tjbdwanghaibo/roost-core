# cellworld：分区 Scene 原型（v2.5）

**原型，不是现行能力。** 只在分支 `feat/seamless-world-prototype` 上；设计、取舍与实测见 [无缝大世界方案 §7 / §8](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#7-v2-设计分区-scene2026-11-03分支-featseamless-world-prototype)（v1 见 §5 / §6，v2.1 的 NPC 降档见 §9，v2.2 的 game-demo 接入与多进程端到端见 §10，v2.3 的框架契约见 §11，v2.4 的战斗面板见 §12，v2.5 的跨 Base 队伍面板见 §13）。

沿 BigWorld 的 Cell + Ghost 流派，作为 **Scene 的一种模式**：声明了分区的 Scene 每个格由一个进程托管。Player 不迁移（Player 所在进程充当 BaseApp），角色在格里的部分是不持久的 Nest 实体 SceneActor，跟着交接走。

## 状态放在哪

| 状态 | 放在哪 | 谁推进 |
| --- | --- | --- |
| 格的区域表 `Region`：邻格 Ghost、交接 seen、墓碑、跨格伤害待决表、本格记录索引 | 该格的 **Scene 实体**（手写 DAO，写时登记逆操作） | Scene 的 `scene.cell` 心跳（`Node.RegionBeat`）与 Scene 上的 handler |
| 角色 / NPC 的格内状态 `ActorState`：位置、速度、HP、**战斗面板 `Panel`**、去重表、交接进度、租约、回写时间、入场序号 `Entry` | **SceneActor / Monster 实体**的 DAO（`Actor.SetCellState` 经 DAO setter 写） | 实体自己的 `cell.actor` 心跳（`Node.ActorBeat`）与 handler |
| 玩家的路由状态 `BaseState`：owner 格与 epoch、输入 / 攻击序号、快照（HP、位置、版本）、**待发面板与已确认序号**、看门狗、入场序号 `Entry` | **Player 实体**（快照与面板映射到持久字段） | Player 的 Base 心跳（`Node.BaseBeat`：看门狗 + 续租 + 面板重发） |
| 客户端看到的格内对象 | Base 进程 entitysync Manager 里的 **Sync mirror**（`policy.Mirrors[EntityState]`） | witness 到达时在 Player handler 里 Apply；别的 Base 发来的队员快照在传输回调里 Apply |
| 队伍成员表的缓存 `Group`（ID、版本、每名成员的 Player / SceneActor / Base sid） | **Player 实体**（game 映射到 DAO；权威在 game，demo 是 Remote 托管的 Team 实体） | game 改成员表的事务（`Node.SetGroup` / `NotifyGroup`）、成员表通知、重读 |

**入场与离场（RV-20261106-03，线上格式 v6）**：每次 `Enter` 由 Base 编一个入场序号 `BaseState.Entry`（Enter 时的系统时间 UnixNano，同进程内至少比上一次大 1；不持久，跨 Base 重启的唯一性依赖系统时钟不回拨）。Enter、Leave 与交接 Offer 都带它，SceneActor 存在 `ActorState.Entry`；Real 只接受 Entry 相同的 Leave，上一次入场重复 / 迟到的 Leave 碰到同 ID 重新入场的角色时丢弃（计 `StaleDrops`）。看门狗重进、交接放弃后补回是同一次入场，序号不变；目标格已有 Real 时幂等的 Enter 只把序号往大改（Base 重启后的新入场接管旧 Real）。仍是尽力而为：发给冻结中旧格的 Leave 不处理、离场后才到的旧 Enter 会重建一个无人续租的角色，两者都由租约（`LeaseTimeout`）兜底。

**扩展消息（线上格式 v7，`ext.go`）**：别的模块借本进程 Node 的通道收发自己的消息，不另起连接与主题。收件方在 `NodeConfig.Ext` 里按命名空间（字母、数字、`-`、`_`，至多 64 个字符）登记处理器；发件方调 `Node.SendExt(sid, space, body)`：只非阻塞地排进出站队列（队列满丢弃计 `OutboxDrops` 并返回 false），由出站 goroutine 编码发出，所以可以在 Nest 的提交后回调里调用（那里不能 Dispatch、不做 I/O）。收件在传输回调上、与 Node 自己的消息同一个准入门（`CloseIngress` 之后不再调用处理器），处理器必须快速返回；没有处理器的命名空间计 `ExtDrops`。语义与其余消息相同：至多一次，可能丢失、重复、乱序，可靠性由使用方的协议负责。计数 `ExtOut` / `ExtIn` / `ExtDrops`。v6 及更早的消息按坏消息丢弃（未上线，升级须先停旧进程）。第一个使用方是可编辑地形的边界块副本（`gameplay/terrain` 的 `Replicator`，命名空间 `terrain`；game-demo 接线见 `internal/service/<game>/terrain_partition.go`），在回环与真实 nats-server 上的用例是 `ext_test.go` 的 `TestTerrainReplicaOverNodes`。

`BaseState` / `ActorState` 的 MsgPack 字段各有独立标签（RV-20261106-04 前 `BaseState` 的 X/Y、VX/VY 共用标签，编码丢 X 与 VX），往返由 `TestStateMsgpackRoundTrip` 逐字段覆盖。

`Node` 是每进程一个的端点：传输回调只解码并把消息投成 Nest 请求（Scene、[Scene, 角色]、[Scene, 攻击者, 目标]、Player）；事务提交后的发送、给区域的报告、实体销毁、跟进投递排进一个有界出站队列，由一个 goroutine 执行（Nest 不允许提交后回调里再 Dispatch）。Node 自己没有锁、不持有格内状态。

## NPC 心跳分档（D1）

维护者 2026-11-03 决定 D1 = B：附近没有玩家的 NPC 心跳降到 `NodeConfig.IdlePeriod`（heartbeat 档位，如 200ms / 1s；零值不降档），有玩家靠近立即回到 `Period`。只降格内移动心跳的档位，不暂停 AI（与 `docs/maintenance/FEATURE-2026-10-22-ai-idle-without-observers.md` 是两件事，那份设计稿不在本轮）。代码在 `tier.go`，设计与实测见 [FEATURE §9](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#9-v21d1-降档与-demo-接入2026-11-03)。

| 项 | 规则 |
| --- | --- |
| 观察者 | 本格玩家 Real / Frozen + 邻格玩家 Ghost（`WakeRadius + H ≤ G` 保证 W 内的邻格玩家一定在本格有 Ghost） |
| 升档 | 某个观察者离 NPC 报告位置 ≤ W + drift（drift = (\|VX\| + \|VY\|) × 当前档位周期，补慢档报告的陈旧）→ 立即请求升档 |
| 降档 | 快档 NPC 连续 `IdleAfter`（缺省 2s）附近没人 → 请求降档；时间滞后，玩家在 W 边上来回只升一次 |
| 执行 | 区域心跳决定，请求经出站队列投给 NPC 自己的 `cellworld_tier` handler（只锁 NPC，幂等），换档 `Actor.SetCellPeriod` 在同一事务里、提交后生效 |
| 慢档越界 | 一拍按 `Period` 分步积分、每步判交接，越界即在那一步冻结：冻结点离本格至多 H + 一步，与快档相同 |
| 积分时长 | 快慢档都按这一拍实际经过的时间（`ActorBeat` 的 `elapsed` = `heartbeat.Beat.Elapsed`，心跳已按上限夹过）积分，不按回调次数：驱动变慢、跳拍时角色仍按设定速度走完这段时间（2026-11-09；此前每拍固定积分一个档位周期，心跳不补跑之后跳拍会让实际速度低于设定速度） |
| 交接 | 冻结中一律快档（重传、放弃计时不变）；`EntityState.Idle` 随 Offer 带走，新格以慢档装入；被拒恢复回到慢档 |
| Ghost | 慢档 NPC 的 Ghost 过期放宽为 `GhostTTL + IdlePeriod`（接收方自己的配置） |

## 接入正式运行时（v2.2 / v2.3，game-demo）

game-demo 的分区配置（`scene.partition`）接的就是这个包：SceneActor / Monster 是生成实体，格内状态在生成 DAO（D8），
Player 的 `BaseState` 在 PlayerDao；接法与端到端结果见 [FEATURE §10](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#10-v22demo-接入分区-scene2026-11-03)。
接入时发现的三处接缝，v2.3 都改成了框架契约（[FEATURE §11](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#11-v23框架契约-e1e3自己的状态e4与追加四项2026-11-04)），cellworld 侧的绕法已删：

| 项 | 规则 |
| --- | --- |
| 不在内存的角色 | 不持久的种类（SceneActor / Monster）内存未命中时 getter 答 nil、不问 DataEngine 加载器（框架契约，`docs/framework/impl/02-nest-entity.md` 第 6a 条，E1）。Node 把名单原样交给 Nest：多实体请求里 handler 拿到 nil、按实体 ID 取角色；只锁角色的单实体请求（输入、换档）由 Nest 以 not found 丢弃 |
| 自己的 mirror 重新出现 | owner 格崩溃、mirror 过期删除（Hide + 注销）后又回来时，Interest 自己恢复“玩家看自己”（self 跟着自身实体的 Hide / Show 走，框架契约，`docs/framework/guide/04-sync.md`，E2），Node 不再记 mirror 代号、不调 `Resubscribe` |
| handler 注册时机 | Node 在服务 Init 里才建（引擎已启动）：服务经 wiring/nest 的 `NestHandlerInstaller` 钩子在引擎启动前 `Register` 一份 `cellworld.NewHandlers()`，`NodeConfig.Handlers` 交给它，`NewNode` 接上（E3）；不再用包级注册表 + 派发时兜底 |

## 战斗面板（v2.4，第 3 项 = A）

维护者 2026-11-04 决定：养成输入（等级、经验、装备）留在 Base 的 Player 上；面板（最大 HP、攻击、战力，将来的 buff）与当前 HP 的权威在 Cell 的 Real。设计与实测见 [FEATURE §12.2](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#122-第-3-项实施结果)。

| 项 | 规则 |
| --- | --- |
| 状态 | `EntityState.Panel{MaxHP, Attack, Power, Ver}`：随 Ghost、Offer（交接带走）、witness、回写、Enter 传；零值字段 = 没有给出（NPC 目前没有面板） |
| 来源 | `BasePlayer.CellPanel()` 是玩家养成输入现在推出的数值；数值变了 Base 才编下一个序号 `Ver`（单调，game 持久化它，跨 Base 重启不回退） |
| 发出 | 在 Player 的 handler 里（升级、换装）调 `Node.UpdatePanel(p)`：同一笔事务写进 `BaseState.Panel`，提交后发 `kindPanel` 给当前 owner；不在事务里时用 `Node.RefreshPanel(ctx, player)`（一次 Player 请求）。不发同步请求、不等结论 |
| 应用 | Real 只接受 `Ver` 更大的（同序号、更旧的答重复）；收到的格不是 Real（冻结中、已交出）答“重试”，Base 不按提示改投 |
| 确认与重发 | 面板结论、owner 通知（`PanelVer`）、witness 里的自己、回写都带回 Real 上的序号（`BaseState.PanelAcked`）；没确认时 owner 变化立即补发、否则每 `LeasePeriod` 重发 |
| 结果未知 | `PanelDeadline`（缺省 2s）内没确认就经 `Hooks.PanelResult` 报一次 Unknown（可能已应用；Base 照旧以同一序号重发，直到确认） |
| 伤害 | 攻击者所在格（攻击者是那里的 Real）按攻击者面板的 `Attack` 定伤害值，没有面板时取 `Attack(…, amount)` 的缺省值；转发给目标格的是定好的数值；结论带实际伤害（`AttackResult.Damage`） |
| 上限 | 当前 HP 不超过 `MaxHP`（0 = 无上限）：进入时快照 HP 高于上限 → 夹；上限变小 → 夹；**上限变大不回血**（暂定，可改） |

## 队伍面板（v2.5，第 4 项 = A）

维护者 2026-11-04 决定：不同 Base 进程上、彼此不在视野里的队友互相看见位置与 HP；成员表的权威在 game（demo 是 Remote 托管的
Team 实体，组队协议是分区场景专用的邀请 / 接受 / 离队 / 解散，普通场景、匹配、锁步战斗不动）。设计与实测见
[FEATURE §13](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#13-v25跨-base-的队伍面板第-4-项--a2026-11-04)。代码在 `group.go`。

| 项 | 规则 |
| --- | --- |
| 成员表 | `Group{ID, Ver, Members}`，`Ver` 是权威每次改成员表时 +1；Base 在 Player 上缓存一份（`GroupPlayer.CellGroup / SetCellGroup`，经 DAO setter，回滚即还原） |
| 改成员表 | game 在改权威的那笔事务里 `Node.SetGroup(自己, 新表)`，并 `Node.NotifyGroup(新表, 离开的人)`：提交后发 `kindGroupNotice` 给变化前后全部成员所在的 Base（含本进程） |
| 采纳通知 | 只在缓存的是同一组、缓存版本非 0、通知的版本更新时采纳；玩家不在新表里就清空。迟到 / 重复的旧通知不改任何东西；版本 0（重启、重新加载）只认重读 |
| 重读 | 缓存版本为 0 时一个 `GroupEvery` 一次、平时每 `GroupResyncEvery`（缺省 10s）一次，经 `NodeConfig.GroupResync` 投一笔 game 的事务（出站 goroutine 上，只做准入），兜住丢失的通知 |
| 快照 | Base 心跳每 `GroupEvery`（缺省 1s）把玩家自己的 SceneActor 状态（本进程 mirror 里最新的 witness 自己，带 Real 的版本）发给其他队员的 Base（`kindGroupSnap`），不发给自己的 sid：同 Base 的队友本来就在本进程的 mirror 里 |
| 收件 | 传输回调里按版本喂同一个 Sync mirror，`MirrorItem.Hold = GroupHold`（缺省 3 × GroupEvery）：3 个周期没更新就过期删除。与 witness 同一个 subject（队友的 SceneActor ID），队友在视野里时两路按版本取新的 |
| 面板 | 每名玩家一个组关系视野（`NodeConfig.GroupView` 给 ID）：成员是其他队员的 SceneActor，玩家挂接它，不论距离都订阅队友；在空间视野里的队友与它合并成一份订阅，不重复推。Base 心跳按缓存的成员表对齐视野（`Interest.Hide` 会把过期的队友从关系视野里拿掉，快照重新到来时下一拍加回） |
| 换格 | 快照跟着队友自己 Base 的 witness 走：交接后的 epoch 更大，收件方按版本更新同一个对象，不删不重建 |
| 信任 | 收件方不核对快照的发送者是否真是本进程某个玩家的队友（原型：集群内互信）；不相干的快照只进 mirror、没有视野订阅，过期即删 |

## 地面钩子（可编辑地形 P3，跨格移动）

`NodeConfig.Surface func() Surface`（可选，2026-11-09，[地形方案 §24](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#24-p3-跨格移动按地形判定t06-剩余2026-11-09)）：角色每拍开始取一次地面，`integrate` 沿这一步（已按世界边缘夹过）的直线逐个整数点走，进入新的地面格问 `Step(上一格, 新格)`。`Surface` 与 `movement.Surface` 同形，`gameplay/terrain` 的 `Walker` 实现它；方法在角色的 Nest 锁内、short 池上调用，只读内存、不阻塞。

| 项 | 规则 |
| --- | --- |
| 被挡 | 停在上一个整数点；玩家速度清零（与停在世界边缘相同，等 Base 的下一个移动输入）；NPC 两个分量都反向、原路返回（与碰世界边缘反弹相同；地形的挡没有“哪条轴碰了边”）。同一拍剩下的子步照常积分（NPC 往回走）。计 `SurfaceBlocks` |
| 减速 | 速度千分比 s < 1000（游泳）时路程按 s / 1000 折算：预算是这一步未被世界边缘夹过的位移的 max(\|dx\|, \|dy\|) × 1000，每走一个点花 ⌈10⁶ / s⌉；路程用完停在那里，速度不变。只有走到终点，世界边缘的速度规则才生效 |
| 没有定义 | 站立的地面格没有定义（块没装入、地图外）时一步不走，按被挡处理；不猜平地 |
| 跨格 | 交接之前角色可能走出本格（至多 H + 一步），地面要覆盖本格外扩 GhostBand：分区进程的地形视图本来就装着这个范围，邻格写的块是 `terrain.Replicator` 推来的 Ghost 副本（已提交版本，丢失时至多一个续期周期补齐）。交接之后由目标格的 Node 按它自己的地面判定 |
| nil | `Surface` 为 nil 或返回 nil 时与没有钩子逐位相同（`TestANilSurfaceIntegratesExactlyLikeNoSurface`，随机位置 / 速度 / 拍长，含世界边缘与交接）。返回值不能是带类型的 nil |

成本（Apple M5，`BenchmarkIntegrateOnSurface`，一个 Period 的一拍）：平地 Walker 上全速玩家（2000 / 秒，一拍 100 个点）约 0.8～1.7µs、慢 NPC（200 / 秒）约 85～155ns，0 分配；没有地面约 15～30ns。每个点问一次 `Cell`（地面格对 cellworld 不透明，不按格边界解析求交）。

## 交接以事务为边界

旧格：角色心跳越界 → 同一笔事务里冻结（epoch+1）→ 提交后发 Offer；超时只删不恢复、告诉 Base。新格：Offer handler（锁 Scene + 该角色）在同一笔事务里经 `NodeConfig.CreateActor`（`EntityManager.Create`，handler 内即 `CreateInScope`，由事务捕获）建出实体、写成 Real、登记 seen，提交后才回“已接受”；回滚即撤销新建与区域表，不回复（旧格重传）。v1 的协议不变量（先冻结后 Offer、一个 epoch 一个目标、幂等回复、隐式确认）不变。

## 用法（框架侧接线）

```go
node, _ := cellworld.NewNode(cellworld.NodeConfig{
	World: "w1", SID: sid, Grid: grid, Transport: cellworld.NATSTransport(natsClient), Nest: nestMgr,
	Destroy: func(ctx context.Context, e entity.IThreadSafeEntity) error {
		return entityManager.Destroy(ctx, e, entity.DestroyReasonCommon, false)
	},
	// Cell 侧：本进程托管的格、持有 Region 的 Scene 实体、在 handler 内新建 SceneActor / Monster 的函数
	Cell: &cellID, Scene: sceneEntityID, ViewRadius: 2500,
	CreateActor: func(id int64, kind cellworld.EntityKind) (cellworld.Actor, error) { /* access.Create(...IsCreate...) */ },
	// Base 侧：角色 ID → Player 实体 ID；witness 喂给 Sync mirror
	PlayerEntity: playerOfActor, Mirrors: mirrors, Interest: interest, Session: sessionOfActor, MirrorKind: kindOf,
	// 队伍面板（可选）：每名玩家的组关系视野 ID；重读成员表的一笔 game 事务（只做准入）
	GroupView: teamViewOf, GroupResync: func(ctx context.Context, team, player int64) error { /* MultiSend_TeamSync */ },
})
_ = node.RegisterHandlers(nestMgr) // 引擎 Start 之前；Node 在引擎启动后才建时（正式进程：服务 Init 里），服务在引擎
// 启动前经 wiring/nest 的 NestHandlerInstaller 钩子 Register 一份 cellworld.NewHandlers()，NodeConfig.Handlers 交给它（E3）
go node.Run(ctx)

// 实体侧：Scene 的心跳调 node.RegionBeat(scene, time.Now())；SceneActor / Monster 的心跳调 node.ActorBeat(actor, time.Now())
// （登记时用 Config().Period，SetCellPeriod 转给 heartbeat.Reg.SetPeriod：D1 降档由 Node 在事务里调用）；
// Player 的 Base 心跳调 node.BaseBeat(player, time.Now())。
_ = node.Spawn(ctx, cellworld.EntityState{ID: monsterID, X: 100, Y: 100, HP: 100}) // 本格生成 NPC（一笔 Scene + NPC 事务）
_ = node.Enter(ctx, playerEntityID, sceneActorID, spatial.Point{X: 48000, Y: 1000}, 100)
_ = node.Move(ctx, playerEntityID, 500, 0)
hit, _ := node.Attack(ctx, playerEntityID, monsterID, 10) // 10 是缺省伤害：攻击者有面板时用面板的攻击
// 面板：Player 的 handler 里（升级、换装之后）node.UpdatePanel(player)，同一笔事务提交后发给 owner
// 队伍：改成员表的那笔 [Team, Player] 事务里 node.SetGroup(player, group) 与 node.NotifyGroup(group, 离开的成员)
```

Cell 进程是普通 roost App：`app.New(...).Singleton(kitredis.SingletonStore)`、`singleton.enabled=true`，同 sid 的第二个进程启动失败，失锁 fail-stop；cellworld 不自己做单实例判断。

## 并发与契约

- 区域表只在 Scene 的 Nest 锁内读写；角色状态只在角色的 Nest 锁内读写；锁序 Scene（World 档）在前、角色（Other 档）在后。角色心跳不锁 Scene：它的结果在提交后经出站队列批量报给 Scene 的 handler。
- 外发一律在提交之后：回滚的事务不外发。
- 消息至多一次：每个实体的版本是 (epoch, seq)，接收端只接受更新的版本；交接靠重传与幂等回复，伤害靠 HitID 去重（去重表在目标实体上，随交接带走）；mirror 按对象版本取新的。
- 同一 (实体, epoch) 至多一个格取得 Real；同一进程里同一 ID 至多一个实体（EntityManager 保证）。

## 测试

| 文件 | 内容 |
| --- | --- |
| `grid_test.go` | 分区映射、边界带、迟滞、版本序 |
| `world_test.go` | 进程内多“进程”（另有 strict getter 版：不在内存的实体答错误，同 DataEngine）（各自的 EntityManager、Nest 引擎、心跳驱动、Node）：直线、抖动、斜穿四格、跨界攻击、慢交接、丢包；witness 连续与客户端（Base 的 Manager 推出的帧）连续 |
| `handoff_test.go`、`combat_test.go` | v1 细粒度用例按新模型改写：丢失 / 重复 / 隐式确认 / 超时只删不恢复 / 被拒恢复 / 回复扣住时来回 / 交接链 / Enter 幂等与拒绝 / 墓碑；伤害只结算一次、距离与可见性、去重随交接、结果未知、去重表满；战斗面板（v2.4）：伤害按攻击者面板、上限夹血不回血、命令丢失重发、重复与乱序只应用一次、交接中改投新 owner、结果未知只报一次 |
| `tier_test.go` | D1：无人时降档、邻格玩家（Ghost）靠近升档、W 边上来回不抖动、慢档 Ghost 不过期、慢档一拍冲过边界仍在边界带内交接并以慢档装入、慢档 NPC 被跨界攻击只结算一次、`advance` 分步积分单元；地面钩子：nil 逐位相同、被挡停在上一个点（玩家停、NPC 折回）、对角只跨相邻格、减速与世界边缘、没有定义的格不走、挡在边界前不交接 |
| `group_test.go` | 队伍面板（v2.5）：两个 Base 上彼此不在视野里的队员互相看见位置 / HP、同 Base 队友不经快照、一个周期一份；对方 Base 停了 3 个周期后移除、重启后重读恢复；队员换格时面板不删不重建；离队 / 迟到的旧通知 / 再入队 / 解散；通知丢了由重读收敛；视野内的队友只有一个对象，走出视野不闪 |
| `lifecycle_test.go` | Cell 崩溃后按 Player 快照重建（含客户端重新持有自己、面板随 Enter 回到新 SceneActor）、Base 消失后租约离场、回写按版本幂等、交接事务回滚不移交 |
| `nats_test.go` | 同一组场景跑在真实 nats-server 上（测试自起私有端口） |
| `ext_test.go` | 扩展消息（v7）：按命名空间交给处理器、提交后回调里可发、`CloseIngress` 之后不再调用、旧版本与空内容按坏消息丢弃；地形边界块副本在回环与真实 nats-server 上推到邻格（`ROOST_TERRAIN_REPLICA_PERF=1` 记延迟与字节）；跨格移动按地形：副本缺块时停下、副本到了再走，真实 Node + 回环 + 两格各自的视图与 Replicator 上玩家在邻格块的墙前停住（交接之前）、墙挖平后交接、新格按自己的视图在坑前停住；`BenchmarkIntegrateOnSurface` |
| `process_test.go` | 每个 Cell 是一个带单实例锁的 App 子进程（私有 redis-server）：跨进程至多一个 Real 与伤害只结算一次、kill -9 后按快照恢复、同 sid 第二个进程被拒、Base 进程消失后 SceneActor 清理 |
| `measure_test.go` | 原型测量：`ROOST_CELLWORLD_PERF=1 GOWORK=off go test -run TestCellWorldMeasure -v ./gameplay/cellworld/`（档位 `ROOST_CELLWORLD_PERF_NPCS=1000,5000`，窗口 `ROOST_CELLWORLD_PERF_SECONDS=20`；D1：`ROOST_CELLWORLD_PERF_IDLE_MS=1000`，玩家分布 `ROOST_CELLWORLD_PERF_PLAYERS=spread|cell`） |

Sync mirror 的单测在 `framework/sync/entitysync/policy/mirror_test.go`。

game-demo 上的多进程端到端（两个 Cell 进程、独立的 Base 进程、kill -9 后重建、普通场景照常）：`scripts/test-seamless-demo.sh`（需要私有依赖，见脚本头）。
