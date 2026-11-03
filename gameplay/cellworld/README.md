# cellworld：分区 Scene 原型（v2.4）

**原型，不是现行能力。** 只在分支 `feat/seamless-world-prototype` 上；设计、取舍与实测见 [无缝大世界方案 §7 / §8](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#7-v2-设计分区-scene2026-11-03分支-featseamless-world-prototype)（v1 见 §5 / §6，v2.1 的 NPC 降档见 §9，v2.2 的 game-demo 接入与多进程端到端见 §10，v2.3 的框架契约见 §11，v2.4 的战斗面板见 §12）。

沿 BigWorld 的 Cell + Ghost 流派，作为 **Scene 的一种模式**：声明了分区的 Scene 每个格由一个进程托管。Player 不迁移（Player 所在进程充当 BaseApp），角色在格里的部分是不持久的 Nest 实体 SceneActor，跟着交接走。

## 状态放在哪

| 状态 | 放在哪 | 谁推进 |
| --- | --- | --- |
| 格的区域表 `Region`：邻格 Ghost、交接 seen、墓碑、跨格伤害待决表、本格记录索引 | 该格的 **Scene 实体**（手写 DAO，写时登记逆操作） | Scene 的 `scene.cell` 心跳（`Node.RegionBeat`）与 Scene 上的 handler |
| 角色 / NPC 的格内状态 `ActorState`：位置、速度、HP、**战斗面板 `Panel`**、去重表、交接进度、租约、回写时间 | **SceneActor / Monster 实体**的 DAO（`Actor.SetCellState` 经 DAO setter 写） | 实体自己的 `cell.actor` 心跳（`Node.ActorBeat`）与 handler |
| 玩家的路由状态 `BaseState`：owner 格与 epoch、输入 / 攻击序号、快照（HP、位置、版本）、**待发面板与已确认序号**、看门狗 | **Player 实体**（快照与面板映射到持久字段） | Player 的 Base 心跳（`Node.BaseBeat`：看门狗 + 续租 + 面板重发） |
| 客户端看到的格内对象 | Base 进程 entitysync Manager 里的 **Sync mirror**（`policy.Mirrors[EntityState]`） | witness 到达时在 Player handler 里 Apply |

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
| `tier_test.go` | D1：无人时降档、邻格玩家（Ghost）靠近升档、W 边上来回不抖动、慢档 Ghost 不过期、慢档一拍冲过边界仍在边界带内交接并以慢档装入、慢档 NPC 被跨界攻击只结算一次、`advance` 分步积分单元 |
| `lifecycle_test.go` | Cell 崩溃后按 Player 快照重建（含客户端重新持有自己、面板随 Enter 回到新 SceneActor）、Base 消失后租约离场、回写按版本幂等、交接事务回滚不移交 |
| `nats_test.go` | 同一组场景跑在真实 nats-server 上（测试自起私有端口） |
| `process_test.go` | 每个 Cell 是一个带单实例锁的 App 子进程（私有 redis-server）：跨进程至多一个 Real 与伤害只结算一次、kill -9 后按快照恢复、同 sid 第二个进程被拒、Base 进程消失后 SceneActor 清理 |
| `measure_test.go` | 原型测量：`ROOST_CELLWORLD_PERF=1 GOWORK=off go test -run TestCellWorldMeasure -v ./gameplay/cellworld/`（档位 `ROOST_CELLWORLD_PERF_NPCS=1000,5000`，窗口 `ROOST_CELLWORLD_PERF_SECONDS=20`；D1：`ROOST_CELLWORLD_PERF_IDLE_MS=1000`，玩家分布 `ROOST_CELLWORLD_PERF_PLAYERS=spread|cell`） |

Sync mirror 的单测在 `framework/sync/entitysync/policy/mirror_test.go`。

game-demo 上的多进程端到端（两个 Cell 进程、独立的 Base 进程、kill -9 后重建、普通场景照常）：`scripts/test-seamless-demo.sh`（需要私有依赖，见脚本头）。
