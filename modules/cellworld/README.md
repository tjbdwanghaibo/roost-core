# cellworld：固定分区大世界的进程装配

## 能力

`roost module add cellworld` 安装 `game/modules/cellworldhost` 与它的 manager（`game/managers/cellworld_host.go` 的 `NewCellWorldHost`，安装器登记进 `game/managers/managers.go`）。它复用 `gameplay/cellworld`，负责取得 Nest、NATS、Redis、App 单实例身份与租约守卫，建立共享授权和出生目录，启动 Node，管理出生槽巡检，并在停机时排空已接纳的工作。game-demo 的分区场景使用同一个模块。

实体一侧的通用适配也在模块里（D21 后半，2026-11-17 从 game-demo 移入）：

| 带来的 | 位置 | 说明 |
| --- | --- | --- |
| SceneActor 实体 | `game/entities/scene_actor`、`db/def/scene_actor.go` | 玩家在格上的那一份（自带实体，号由安装器分配）；`noPersist` / `ephemeral`，只由 Node 以 `CellMode` 建出，整份状态是 `SceneActorDao` 的 Cell* 字段，跨格交接整份带走 |
| 分区状态的嵌套类型 | `db/def/cellworld.go` | CellHit / CellHandoff / CellOwnership / CellRecovery / CellEffect / CellCooldown / CellDeath；工程让自己的实体走格时 DAO 用同样的类型 |
| Player 上的模块状态 | `PlayerDao.CellworldBase`（存储键 `cellworld_base`，持久、不同步） | 恢复快照（Buff / 冷却）与死亡回执，与 SettleDeath 的奖励同一笔 strict 事务 |
| 状态映射 | `cellworldhost.ReadActor` / `WriteActor`（`CellDao`）、`ReadBase` / `WriteBase`（`BaseDao`） | DAO 与 `cellworld.ActorState` / `BaseState` 之间唯一的一份映射：读出是副本，写经生成 setter（回滚即还原），快照位置只在回写时写 |
| Node 安装点与格心跳 | `Install` / `Uninstall` / `Node` / `SceneID` / `Heartbeat`、`CellMode`、`RegisterActorBeat` / `RegisterBaseBeat` | 实体在回调里找本进程的 Node；心跳名与周期取 Node 配置，Node 卸下后什么都不做 |
| 死亡回执 | `DeathReceipt` / `RecordDeathReceipt` | 表满时新目标被拒，Node 不确认这条死亡 |

玩法状态仍在 Entity 的 DAO 中：Player 留在 Base，SceneActor / 走格的怪物由其所在 Cell 执行。内部状态用 MessagePack 增量传输，客户端仍走 Base 的 Sync mirror 与 EntitySync。详见 [CellWorld 契约](../../gameplay/cellworld/README.md)。

## 边界

- 固定网格、一个进程一个 Cell；不提供动态拆格、跨多 Cell 原子战斗结算。
- 模块只带 SceneActor；不预设 Scene / Monster 类型，不替业务创建它们，也不替业务决定战斗、掉落与玩家复活规则。Player 的路由、快照位置与面板字段由工程在 PlayerDao 里声明（下表）：其中 HP 与面板常要借 Player 的同步编码给客户端看 SceneActor 的 mirror，同步形状归工程定。
- Cell 必须开启 App singleton。Redis 权威键须持久化、禁止淘汰，不允许在丢失授权记录后靠旧 Mongo 快照直接重建。
- 所有节点使用相同 world、网格与协议版本。内网 NATS 必须隔离环境并使用连接级认证。旧协议 / schema 不兼容，数据升级由离线迁移负责。
- `Runtime.Stop` 超时意味着 Node 仍未排空：不得先关 Nest / NATS / Redis 或释放单实例锁。App 不重试 `Service.Shutdown`，但 Node 归 manager 托管（D22）：Shutdown 把错误交给 App 之后，App 停 Mod 时 manager Mod 排在 Nest、NATS、Redis 与心跳之前停，`Host.Close` 用 manager 的停机期限再排空一次（`Node.Drain` 可以重试）；仍超期时 ManagerMod 返回期限错误，App 不再停那些依赖、不释放锁，进程随后退出，没排空的入场与交接由授权租约到期后的接管处理。

## 复用方式

1. 在服务的 Nest 注册阶段调用 `cellworld.NewHandlers().Register(engine)`，把同一个 Handlers 传入 NodeConfig。
2. 玩家的 Actor 用模块的 SceneActor：`NodeConfig.CreateActor` 对 `KindPlayer` 以 `EntityCreateParam{Param: cellworldhost.CellMode{}}` 建它。工程自己的怪物要走格时，DAO 声明与 `SceneActorDao` 同名同类型的 Cell* / PosX / PosY / HP 字段（`cellworldhost.CellDao`），`CellState` / `SetCellState` 调 `ReadActor` / `WriteActor`，心跳用 `RegisterActorBeat`。场景实现 `RegionHost`。
3. Player 实现 `BasePlayer`：PlayerDao 按下表声明字段（`cellworldhost.BaseDao`），`BaseState` / `SetBaseState` 调 `ReadBase` / `WriteBase(d, st, cellworldhost.SceneID())`，`CellDeathReceipt` / `RecordCellDeathReceipt` 调 `DeathReceipt` / `RecordDeathReceipt(dao.GetCellworldBase(), …)`，Base 心跳用 `RegisterBaseBeat`（不是分区进程时返回 nil）。位置 / HP / Buff / 冷却随回写恢复，回写周期不是硬 RPO。

   | 字段 | 类型 | 选项 |
   | --- | --- | --- |
   | `PosX` `PosY` | int64 | 实时位置，`nopersist` |
   | `SavedSceneID` `SavedPosX` `SavedPosY` | int64 | 快照位置，持久（只在回写时写） |
   | `CellHP` `CellMaxHP` `CellAttack` `CellPower` | int64 | 持久；要给客户端看 SceneActor 的 mirror 时加 `sync` |
   | `CellSnapEpoch` `CellSnapSeq` `CellPanelVer` `CellPanelAcked` `CellEntry` | uint64 | 持久、`nosync` |
   | `CellActor` `CellVX` `CellVY` `CellViewAt` `CellEnterAt` `CellLeaseAt` `CellLostAt` `CellPanelSentAt` `CellPanelSince` `CellGroupSentAt` `CellGroupSyncAt` `CellGroupViewID` | int64 | `nopersist,nosync` |
   | `CellEpoch` `CellIntentSeq` `CellAttackSeq` `CellLostEpoch` `CellGroupViewVer` | uint64 | `nopersist,nosync` |
   | `CellOwner` `CellHint` | int32 | `nopersist,nosync` |
   | `CellInWorld` `CellShown` `CellPanelUnknown` | bool | `nopersist,nosync` |

4. 调 `cellworldhost.New(registry, cfg)`。它只装配；`cellworldhost.Install(node, 场景 ID, 心跳驱动)`（SceneActor 从这个驱动取心跳宿主），先安装 Entity 心跳，再 `Runtime.Start(ctx)`；停机 `Runtime.Stop` 成功后 `Uninstall(node)`。Actor 心跳和 Scene 心跳按 50ms 驱动，普通业务仍走 Nest。**装配顺序**：manager 的 Start 在 Service.Init 之前，那时场景与心跳还没装，所以 manager 只发布 `cellworldhost.Host`、不启动 Node；Node 的启动点由服务给出——`Runtime.Start` 把 Runtime 交给 Host 再起 Node，Registry 里没有 Host（`Managers()` 里删掉了 `NewCellWorldHost()`）或 Host 已停（进程在停机）时拒绝、不起 Node；一个进程只能交一个 Node。
5. NPC 启动时用 `Births.Reserve` 固定名字与序号，用 `SpawnID` 构建稳定 ID（`Static | home<<34 | 1<<33 | seq`，seq ≤ 50000；第 33 位是出生号段，同一 sid 下业务自己按 `sid<<34` 编码的运行期号序号只能用第 0～32 位，game-demo 的 `runtimeid` 与 monster 模块已按此封顶），调用 `Node.ReconcileSpawn` / `Runtime.StartSpawns`。改名、删槽、改变已存在槽的规则被拒绝，须明确离线处理旧槽。
6. 有奖励时配置 `NodeConfig.SettleDeath`，在获奖 Player 的 strict 事务里调用钱包 / 背包组件；不得做外部 I/O、另开事务或在观察 Hook 中发奖。死亡回执和奖励同提交；重复投递不会重复奖励。
7. 停止业务准入后、拆掉 Node 用到的场景（Mirror / Interest）之前调用 `Runtime.Stop(ctx)`，成功后才卸载实体与底层 Mods。服务没停下的 Node（Stop 超时、Init 在 Start 之后失败）由 manager 停时 `Host.Close` 在期限内排空并收回协程；`Runtime.Stop` 可以重复调用。

完整且可生成运行的 Player / 怪物 / 服务适配在 [game-demo](../../demo/README.md) 的 `game/entities/player/cell_base.go`、`game/entities/monster/cell.go` 与 `internal/service/game/partition.go`（demo 先单独装本模块，再写用到模块类型的 MonsterDao）。

## 联动

依赖 `nest`、`redis`、`nats` 与 `entity` / `dao` 特性；没有 `with/` 文件。基础 `scene` / `monster` 模块的单进程运行时不是 CellWorld，不可对同一个 Actor 同时安装两套调度。可复用组件的数据与规则，但迁移、心跳及同步归属必须唯一。

## 验证范围

- `roost module verify cellworld`：17 条（`./game/modules/cellworldhost`、`./game/managers`，`^TestCellworld`）。实体适配 7 条（2026-11-17）：SceneActorDao 的映射完整、读出是副本、回到 Real 清掉交接与去重表；格内状态随失败事务经生成 setter 整份还原；Base 映射的快照位置只在回写时写、重新装载的玩家取记在本分区场景之下的保存位置；死亡回执表满时拒绝新目标；Player 的 CellworldBase 随失败事务还原、经正式持久编码再水合仍在；SceneActor 只在装了 Node 时以 CellMode 建出、心跳可启动与换档、`Uninstall` 只卸自己装的那一个、普通进程上 Base 心跳不登记。其余 10 条装配契约，覆盖出生号稳定性、越界拒绝、缺失 Registry 拒绝、`New` 的 Cell 分支（授权记录、出生目录、启动令牌与租约检查取自 App，缺一项或 sid 对不上即拒绝）、`Start` / `Stop`（就绪后返回、只能调一次、Stop 等 Node 退出），以及 `Start` 失败（Node 就绪前退出、ctx 先到、就绪后 `AddIngress` 因 App 已关输入而失败）后回到未启动（StartSpawns 拒绝、Stop 无事可做、再 Start 仍拒绝；`AddIngress` 失败那条还核对 Node.Run 已退出、订阅全退、Runtime 已从 Host 拿下，错误点名 `"cellworld"` 输入）；manager 托管（D22）：没有 Host 或 Host 已停时 Start 拒绝且不起 Node、一个进程第二个 Node 被拒，`Host.Close` 排空服务没停下的 Node（之后服务的 Stop 什么都不做）、服务停过的再 Close 什么都不做，manager 经 ManagerMod 起停时 Start 之前没有 Host、停了之后 Host 关闭。ManagerMod 排在 Nest / NATS / Redis / 心跳之后启动、之前停止由 core `wiring/manager` 的 `TestManagerModStartsAfterAndStopsBeforeTheInfrastructureItsManagersUse` 钉住。用替身 Redis / Nest / Transport，不连真实依赖（`AddIngress` 失败用 core 导出的 `app.CloseRegisteredIngress` 先关 Registry 的输入）。真实 NATS / Redis 下的启停见下面 core 与 game-demo 两条。
- core `go test -race ./gameplay/cellworld`：授权交错、丢包 / 乱序、死亡与奖励回滚、出生代次、出站背压、排空，以及本机真实 NATS / Redis / App 进程故障。
- 生成 game-demo 后测试 `./game/entities/player`（demo 的 PlayerDao 接上模块映射：快照位置、保存位置、面板回滚、死亡回执 + 钱包 + 恢复快照的真实文件 WAL 恢复）与 `./game/entities/monster`（MonsterDao 的整份状态与回滚）。`scripts/test-seamless-demo.sh`：私有真实依赖下的账号、玩家 TCP、跨 Cell 与进程重启。
- 容量入口及已测边界见 [CellWorld README](../../gameplay/cellworld/README.md)。Linux / 跨机 / 生产 Redis HA 的结果须在目标环境实跑，不能由本机样本替代。
