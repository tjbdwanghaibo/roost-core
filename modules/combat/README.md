# combat：固定双人 PVP 战斗（双方同意、技能运行时、控制 Buff、冷却与延迟伤害）

## 能力

两名玩家各自表示同意后，可以互相施放技能。技能由框架的技能运行时（`gameplay/skill` 的 Runtime）执行，技能内容是工程里的 JSON 文件。它是“两个人、一场会话”的战斗，不是开放世界混战。

- **客户端协议**（`protocol/def/`，四个应答形状相同：`Sequence`、`CastID`、`Health`、`PeerHealth`、`Alive`、`PeerAlive`、`Pending`、`Ended`）
  - `CombatReady`（`combat_ready.go`）：请求带 `Sequence`、`Ready`。表示同意或取消同意参加双人战斗。
  - `CombatCast`（`combat_cast.go`）：请求带 `Sequence`、`PeerID`（对手的原始玩家号）、`SkillID`。对对手施放一个技能；双方第一次成功施法时绑定为一场会话。
  - `CombatStep`（`combat_step.go`）：请求带 `Sequence`、`PeerID`。显式把这场战斗推进到服务端当前时刻（结算延迟伤害、让 Buff 到期）。
  - `CombatLeave`（`combat_leave.go`）：请求带 `Sequence`、`PeerID`。投降并结束会话。
  - 端点在 `game/controllers/combat/`：`HandleCombatReady`、`HandleCombatCast`、`HandleCombatStep`、`HandleCombatLeave`。操作者取自认证会话，时刻由服务端提供。
- **服务端入口**
  - 组件方法（`game/components/combat/combat.go`，要求已持有双方的锁）：`CombatComp().Ready(seq, ready)`、`Cast(peer, seq, skillID, now)`、`Step(peer, seq, now)`、`Leave(peer, seq)`、`Tick(peer, epoch, now)`；前四个返回 `CombatResult`，`Tick` 只返回错误。`Core()` 返回框架的战斗组件供持锁的可信业务只读；`Input()` 返回下一拍的外部输入（场景号、坐标、最大生命、攻击）。窄接口 `ICombatEntity`。
  - Nest handler（`game/handler/`，都是 `durability=strict`）：`handlerCombatReady`（单实体）、`handlerCombatCast` / `handlerCombatStep` / `handlerCombatLeave`（双实体，生成的 Sender 是 `MultiSync_CombatCast` 等）、`handlerCombatTick`（双实体，仅供本服的后台驱动调用，没有协议）。
  - 运行期入口 `combat.Start(registry)`（`game/combat/runtime.go`），以及 `combat.Queue(owner, peer, epoch)`、`combat.Program(id)`、`combat.Resolver()`、`combat.Environment()`、`combat.FailedDeliveries()`。
  - `combat.NewHost(...)`、`Host`、`Participant`、`Input`（`game/combat/host.go`）：把技能运行时限制在本次事务已锁住的两名参与者之内的宿主。
- **登记点 / 钩子**：本模块不提供登记函数。它自己接在别的模块上：在 `actor` 的 `Body.Host` 上登记心跳 `combat.duel`，把 Buff 标签绑定到 `actor.Rooted` / `actor.Silenced`。
- **状态**
  - `PlayerDao.Combat`（`CombatState`，`db/def/combat.go`，`persist,sync`）：公开投影与同意绑定——`Initialized`、`Ready`、`Opponent`、`Health`、`MaxHealth`、`Shield`、`Alive`、`Rooted`、`Silenced`。下发给本人。
  - `PlayerDao.CombatPrivate`（`CombatPrivateState`，`persist`，不下发）：最近一次请求的回执（`Sequence`、`Kind`、`Skill`、`Target`、`Reply`）与会话代际 `Epoch`；两人中实体号较小的一方另存整场的 `Runtime` 与 `Host` 检查点。
  - 附加 DAO `CombatDAO`（`db/combat.go`，清单的 `daos`，字段 `combatState`）：生命、护盾、Buff 的唯一权威，与 `PlayerDao` 一起加载和提交。
- **配置**
  - 四个技能文件 `game/skills/combat_strike.json`、`combat_bolt.json`、`combat_stun.json`、`combat_silence.json`，技能号是 `skill.<工程名>.combat.strike` 等。`strike` 立即造成伤害；`bolt` 延迟 4 拍后造成伤害；`stun`、`silence` 给对手加持续 20 拍的状态；四个技能冷却都是 20 拍。
  - 常量在 `game/components/combat/combat.go`：`CombatRange = 5000`（施法准入距离）、`CombatClock`（1 拍 = 50 毫秒）、`CombatRuntimeMaxBytes`（运行时检查点上限 512 KiB）；`game/combat/host.go` 的 `MaxHostBytes`（宿主检查点上限 256 KiB）。
- **错误码**（`internal/errors/combat.go`）
  - `combat_invalid`：请求非法（对手是自己、没有属性等）。
  - `combat_sequence`：序号不是“上一个加 1”也不是“最近一次的原样重试”。
  - `combat_conflict`：序号与最近一次相同但参数不同。
  - `combat_boundary`：检查点与实际战斗数据对不上（例如有代码绕过会话改了生命）。
  - `combat_busy`：参与者属于另一场会话，或这两人并没有绑定。
  - `combat_scene`：双方不在同一个有效空间。
  - `combat_range`：超出施法距离。
  - `combat_consent`：有一方没有同意。
  - `combat_dead`：有参与者已经死亡。
  - `combat_silenced`：施法者处于沉默状态（眩晕也会派生沉默），不能施法。
  - `combat_skill`：技能被拒绝（技能号不存在、冷却中、时钟回拨、同一拍内输入变化等）。

**日志**：准备 / 离开确认记 Info；combat_boundary 与未知内部失败记 Error，后台推进同样记录异常；施法、伤害和每拍成功推进不记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/combat/runtime.go`, `game/controllers/combat/combat_cast.go`, `game/controllers/combat/combat_leave.go`, `game/controllers/combat/combat_ready.go`, `game/controllers/combat/combat_step.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

结束规则：死亡状态保留到显式 `CombatLeave` 或场景结束；结束时双方恢复到各自已确认的最大生命、护盾清零，清除控制、冷却与施法状态。双方须重新 Ready 才能再战。恢复和会话清理在同一事务中，提交拒绝时全部回滚；重复 Ready 不能用于会话内回血。新增死亡后结束、拒交后重试两条场景。

## 能力边界

- **不做的事**
  - 不是多人战斗：会话固定两人，绑定期间第三人的施法答 `combat_busy`。不支持怪物参战。
  - 不判定胜负、不发奖励、不记战绩。一方死亡后会话并不自动结束：之后的施法答 `combat_dead`，要有人发 `CombatLeave` 或双方不在同一空间才结束。

  - 技能宿主只支持“对已声明的两名参与者读属性、读位置、造成战斗效果与加状态”。范围选择、召唤物、投射物、持久技能变量都明确返回不支持（`ErrUnsupported`），不伪造命中。
  - 不跟随 `scene` 的时间倍速：战斗用服务端墙钟，1 拍 50 毫秒。
- **数量与大小**：施法距离 5000；一场会话同时至多 4 个进行中的施法、32 个排队任务；运行时检查点至多 512 KiB、宿主检查点至多 256 KiB，超过答 `combat_boundary`。后台推进队列容量 128，每场会话至多排 1 项，队满时等下一拍重试。
- **请求序号**：每名玩家的 `Sequence` 从 1 开始、跨会话单调递增。只缓存最近一次的结果：同序号同参数的重试返回原回执（不重复造成伤害），更旧的序号一律拒绝。
- **一致性**：一次操作只在本次 Nest 事务里解析并锁住两名参与者；运行时检查点、宿主状态、双方的 `CombatDAO` 与公开投影一起提交或一起撤销。提交被拒时不消耗冷却、不漏掉延迟伤害、不留下控制状态。
- **外部输入按拍冻结**：移动与换装带来的位置、属性变化在下一拍才进入战斗世界；同一拍内输入变了的新施法被拒（`combat_skill`），要等下一拍。已经按“施法开始”取过快照的属性不会被之后的换装改写。
- **离开空间即结束**：推进时发现双方不在同一个有效空间，就结束整场、清掉同意与控制、丢弃延迟任务。距离只在施法准入时检查，施法后在同一空间里走远仍会命中。
- **自动推进是尽力而为的驱动**：心跳每 200 毫秒在提交后把“推进这一场”投进有界队列，由一个后台 worker 发起双实体事务。失败或队满下一拍重试；客户端也可以用 `CombatStep` 显式推进。
- **前提**：依赖 `actor`、`attribute`、`skillbook` 三个模块；需要 `nest`、`heartbeat` 两个 Mod；运行期入口 `combat.Start` 由生成的 bootstrap 自动接线，要求 Nest 客户端支持 `RequestMultiSettled`。双方必须已经被放进同一个空间（`actor` 的 `Body.Enter`，通常由 `scene` 完成）。两名玩家要在同一个 game 进程里，没有跨服战斗。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“场景模块的边界”“运行期接线的边界”两条。

## 复用方式

- **需求正好是它**：`roost module add combat`，连带安装 `actor`、`attribute`、`skillbook`（`attribute` 又带上 `profile`）。要让玩家能站进同一个空间，一般还要装 `scene`。
- **需求比它多一点**
  - 改技能数值或加技能：改或新增 `game/skills/` 下的 JSON（技能由 `skillbook` 在服务启动时编译）。新技能用到的状态要在技能编译环境里登记，写法见本模块的 `with/skillbook` 联动文件。
  - 改施法距离、拍长：改 `game/components/combat/combat.go` 的 `CombatRange`、`CombatClock`。
  - 加胜负判定与奖励：在 `game/components/combat/combat.go` 的 `run` 之后（`Cast` / `Step` / `Tick` 拿到结果处）检查 `Alive`，调用 `end` 并在同一笔事务里写自己的结算状态。

  - `game/combat/host.go` 的检查点格式与哈希校验不要改。
- **别的模块 / 业务代码要用它**
  - 读战斗状态：持锁时读 `owner.Dao().GetCombat()`（是否在战斗 `GetOpponent() != 0`、生命、是否存活）。
  - 服务端代发操作：`syncsender.NewCombatCastSender(client).MultiSync_CombatCast(ctx, 施法者完整实体号, 对手完整实体号, &pb.CombatCastRequest{…})`；`Ready` 是单实体的 `Sync_CombatReady`。
- **只适合借鉴的情形**：多人团战、打怪、需要范围技能或召唤物的战斗不能直接用。值得借鉴的是“附加 DAO 持有战斗权威、运行时每次从完整检查点恢复、检查点与权威数据用哈希绑定、后台推进只传实体号与代际”的做法。

## 联动

- `combat` + `skillbook`（`with/skillbook/game/skillbook/combat_environment.go`）：经 `OnCompileEnvironment("combat", …)` 往技能编译环境里登记 `stun`、`silence` 两个状态；另有 1 条正式目录、摘要校验、眩晕与到期释放的联动场景。
- `combat` + `scene`（`with/scene/game/components/combat/combat_scene.go`）：经 `OnSceneFields("combat", "Combat")` 只把公开投影 `Combat` 开放给附近玩家，`CombatPrivate` 不公开。
- `combat` + `equipment`（`with/equipment/`，只有场景测试）：验证真实换装只在下一拍进入战斗世界、换装提交被拒时保持原权威。
- 别的模块没有 `with/combat`。

## 验证范围

- `roost module verify combat`：38 个场景（`./game/handler`，`^TestCombat`）；与 `scene` 的联动另有 2 个场景（`./game/controllers/scene`），与 `equipment` 的联动另有 2 个场景（`./game/handler`）。
- 场景是离线的：真实生成的 PB、controller、Nest 与技能运行时；提交器是替身（`combatCommitter`，可注入拒绝与阻塞）。
- 覆盖：双方同意、伤害经两个 `CombatDAO`、原样重试不重复伤害、序号冲突与跳号、冷却随检查点恢复并按服务端时间到期、延迟技能按拍结算、提交被拒后绑定生命冷却回滚、眩晕驱动 `actor` 门控、Buff 到期、沉默拒绝施法、跨空间与超距拒绝、第三人不能加入、时钟回拨、绕过会话改生命或 Buff 被拒、属性变化下一拍生效、离开清理、附加 DAO 的登记与往返、协议使用会话身份并返回业务码、并发重放共用一份回执、未知技能不绑定、检查点损坏时拒绝、心跳队列驱动双实体推进、离开或切换空间取消施法与控制、重新加载后继续延迟技能并恢复 Buff 投影、停止时等待队列中的事务完成。
- 没有验证：跨服、真实数据库恢复、经 TCP 的客户端链路与长时间运行都没有验证，不能当作生产验收。
