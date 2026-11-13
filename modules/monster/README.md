# monster：场景里的怪物（巡逻、追击、死亡隐藏、原地重生）

模块自带 `Monster` 实体：服务端按一份不可变的出生规则在某个场景里放怪；怪在四个角点之间巡逻，感知到玩家就追；被打死后从视野里消失，过一段时间在出生点以同一个 ID 重生。它只提供“怪的生命周期与移动”，战斗、掉落、仇恨都不在这里。

## 能力

**客户端协议**（`protocol/def/monster_inspect.go`）

- `MonsterInspect{MonsterID}` → `MonsterInspectResponse{Code, Reason, MonsterID, LifeSeq, HP, Template, X, Y}`：查看一只怪。`MonsterID` 是完整的 Entity ID（就是 AOI 同步里的 subject id）。只有查看者与怪在同一个场景时才答；被拒时应答只有 `Code` / `Reason`（见“错误码”），连接保留。客户端没有出生、伤害、杀怪的入口。
- 怪的位置与生命经场景视野同步下发：公开视图只有 `Actor`（场景、坐标、朝向）与 `Vitals`（`Template`、`HP`、`LifeSeq`）两个字段（`game/entities/monster/sync.go`）。

**服务端入口**（`game/monster/service/runtime.go`；`service.FromRegistry(registry)` 取 `*Manager`，能力名 `gameplay.monster`。都会等待 Nest，必须在 Nest 业务 worker 之外调用）

- `Spawn(ctx, sceneUniqueID, spec) (monsterEntityID int64, err error)`：在已开的场景里按 `spec` 放一只怪。失败会自己清理；清理也失败时同时返回 ID 与错误，调用方可以对这个 ID 重试 `Destroy`。
- `Damage(ctx, entityID, expectedLife, amount) (monster.Snapshot, error)`：扣血。`expectedLife` 必须等于怪当前的 `LifeSeq`，否则拒绝（上一条命的迟到伤害打不到新命）。血量扣到 0 即死亡。
- `Destroy(ctx, entityID) error`：让怪退出场景并从 EntityManager 移除，不再重生。可重试。
- `IDs() []int64`：本进程全部出生槽位的怪物 ID。
- `DrainScene(ctx, area)`：scene 关图时回调，清掉这个场景实例的全部怪（含死亡待重生的）。模块启动时已经登记到 scene。

**Nest handler**（`game/handler/monster_*.go`）：`MonsterSpawn`、`MonsterDamage`、`MonsterRemove`（都同时锁 Monster 与 Scene，`rollback=undo durability=async`）、`MonsterInspect`（同时锁查看的 Player 与 Monster，只读）。组件 `MonsterActorComp()` 提供 `Body()`、`Snapshot()`、`Damage(expectedLife, amount, now)`、`Decide(expectedLife)`、`Spawn`、`Retire`、`SpawnPoint()`，只能持锁调用。

**登记点**：模块不提供给别的功能的登记点。它自己登记了 scene 的关图清理（`RegisterDrainer`）与 App 的 ingress（停机时先拒绝新操作）。

**状态**：`MonsterDao`（`db/def/monster.go`）三个字段，**都不落库**：`Actor`（同步）、`Vitals`（同步）、`Brain`（不同步：出生点、当前目标、巡逻角点游标、重生截止时间、是否已退休）。实体声明为 `noPersist=true lifetime=runtime_rebuild`。

**配置**：没有配置表与配置键。出生规则是归工程所有的 `game/monster/rules.go`：

- `Spec{Template, HP, Speed, PatrolHalf, SenseRadius, RespawnDelay, At}`，`Spec.Validate()` 限定取值范围。
- `DefaultSpec()`：模板 1、100 HP、速度 6、巡逻半边长 12、感知半径 30、死亡 5 秒后重生、出生点 `(80,80)`。模块启动时用它在场景 1 放一只怪。
- `Nearest(at, radius, candidates)`：选追击目标的规则（最近的玩家，距离相同取 ID 小的）。

**错误码**（`internal/errors/monster.go`，都是 `MonsterInspect` 的正常拒绝；端点把码填进应答，不记 Error 日志）

- `monster_invalid`：请求里的 `MonsterID` 不是怪物的实体号。
- `monster_gone`：这只怪现在不在任何场景里——已经死亡（等待重生）、已被销毁，或者这个号从来没有怪。重生之后经场景视野重新同步下来，带着新的 `LifeSeq`。它不核对查看者在哪个场景（死亡的怪没有场景可核对），所以任何已进入游戏的玩家都能知道某个怪物号“现在不在场”。
- `monster_not_here`：怪活着，但查看者不在它所在的场景里（或查看者还没进任何场景）。
- 查看者自己还没进入游戏时仍是框架码 `entity_not_found`；不带码的失败答框架的内部码并记一条 Error。服务端入口（`Spawn` / `Damage` / `Destroy`）的拒绝仍是不带码的普通错误，它们不面向客户端。

**日志**：出生、死亡、销毁、关图清怪确认记 Debug；普通命中、AI 与重生扫描不记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/monster/service/runtime.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有战斗（伤害数值由调用方算好传进来）、没有仇恨表、不攻击玩家、不放技能；没有掉落、击杀奖励与击杀记录；没有寻路（巡逻角点的地形碰撞由 actor 的移动处理，绕不过去就停）；没有刷怪点配置表、没有种群计划。
- **不持久**：怪不写 WAL 与 Mongo。进程重启后只按启动规则重建（缺省就是场景 1 的那一只）；运行中临时 `Spawn` 的怪不会跨重启恢复。要长期种群，业务自己保存规则并在启动时重放。
- **上限**：每个进程至多 256 个出生槽位（`MaxMonsters`）；`sid` 必须在 1 到 65535；重生队列容量 64（满了由下一秒的心跳重试）；`Spec` 的范围是 HP 1 到 10 亿、速度大于 0 且不超过 100、`PatrolHalf` 1 到 1000、`SenseRadius` 0 到 10000 且不能大于场景的 `Sight`、`RespawnDelay` 1 秒到 24 小时。
- **`Damage` 不判重**：同一条命里同一次攻击发两遍就扣两次。攻击的幂等由调用方（战斗）自己的账本负责，不能把 `LifeSeq` 当请求键。
- **感知不是授权**：怪看到的是场景已提交的玩家位置快照。战斗要在自己的多实体事务里重新检查场景、距离与 `LifeSeq`。
- **一致性**：出生、伤害、退出都是 Monster 与 Scene 两个实体的一笔事务，失败时位置、生命、AI 状态与场景成员一起撤回。提交是 `async` 的——怪本来就不落库。
- **死亡期间仍占场景成员槽位**：尸体隐藏但算容量，所以等重生时不能关掉它所在的场景，除非走 scene 的正式关图（会先清怪）。
- **重生用业务时间**：等待时长不随场景倍速或时停改变；时钟回拨会推迟重生。
- **前提**：依赖 `actor` 与 `scene`；业务服务要有 `nest` 与 `heartbeat` 两个 Mod。单进程所有权：怪不跨进程迁移，同一个 `sid` 不能同时跑两个写者；模块启动时场景 1 必须已开（scene 模块缺省会开）。
- **已知限制**：[KNOWN-LIMITS“场景模块的边界”](../../docs/maintenance/KNOWN-LIMITS.md)（scene / monster / terrain 单进程所有权，没有跨进程切图）与“运行期接线的边界”。

## 复用方式

- **需求正好是它**：`roost module add monster`，连带装上 `actor` 与 `scene`，并接好心跳、AOI 与模块启动入口。
- **需求多一点**：
  - 改缺省怪或不要它：改 `game/monster/rules.go` 的 `DefaultSpec`，或改 `runtime.go` 的 `Start` 里那一次 `Spawn`。
  - 多种怪、多个刷怪点：在自己的启动代码或玩法逻辑里多次调 `Spawn`，每次给不同的 `Spec`。
  - 改选目标的规则：改 `rules.go` 的 `Nearest`。改巡逻路线：改 `game/components/monsteractor/monsteractor.go` 的 `Decide`。
  - 不要把跨拍的状态存进 `game/monster/brain.go` 里每拍新建的 AI / actionflow 对象：它们不支持 Nest 回滚，跨拍事实只放 DAO 与 actor 的 `Body`。
- **别的代码要用它**：
  - 打怪：`manager.Damage(ctx, monsterEntityID, lifeSeq, amount)`，`lifeSeq` 取自先前读到的快照（`MonsterInspect` 的应答或同步下来的 `Vitals.LifeSeq`）。
  - 在自己的事务里读怪：handler 的目标接口用 `monster.IMonsterEntity`，调 `target.MonsterActorComp().Snapshot()`。
  - 放怪与收怪：`manager.Spawn(ctx, sceneUniqueID, spec)`、`manager.Destroy(ctx, id)`。
- **只适合借鉴的情形**：需要持久的世界 Boss、需要跨进程的大世界怪、数量远超 256 的群怪。值得借鉴的是：用 `LifeSeq` 挡住上一条命的迟到命令；重生复用同一个 Entity ID；死亡与重生在“实体 + 场景”一笔事务里；以及 `monster.Target`（`game/entities/monster/target.go`）用本进程内的实例身份挡住“同 ID 被重建之后的迟到操作”。

## 联动

- 本模块没有 `with/` 联动文件。
- `terrain` + `monster`（`terrain/with/monster`）：只有场景测试，验证关图时先清本图的怪、失败时保留资源供重试；不加运行时代码。
- scene 的正式关图入口按实体种类调本模块的 `DrainScene`；terrain 先于 monster 停止时也走这条链。

## 验证范围

- `roost module verify monster`：26 条场景，全部在 `game/monster/service`（清单的包列表只有这一个包）。
- 离线，但用的是真实的 Nest、实体 Manager 与 lifecycle、heartbeat、AI / actionflow、movement、Sync / AOI；持久提交与网络传输是替身。
- 覆盖：正式出生与 AOI 可见、`MonsterInspect`、`MonsterInspect` 的每种拒绝各答自己的业务码（不是怪的号、不存在的怪物号、查看者离开场景、怪已死亡；查看者没进入游戏仍是框架码；拒绝之后同一个入口照常可用）、巡逻、定身、低速追击、感知边界、决策失败回滚、死亡隐藏与重生截止、旧命的伤害被拒、销毁、非法规则与 sid、停机准入、真实心跳驱动的巡逻与重生、出生失败清理、死亡失败回滚、退出回复丢失后续做，以及出生 / 异步重生 / 伤害 / 退出遇到同 ID 重建的 8 种交错。
- 没有验证：真实数据库、TCP 联网、跨进程故障、长期稳定性、大规模性能。

## AI 与回滚

移动、位置、朝向、跟随与定身门控共用 actor 的 `Body`。场景通过实例回调提供已提交的玩家位置；怪只在自己的锁里读这份值快照，不读别人的 DAO、不拿别人的锁。当前目标没变且跟随仍在进行时不重新发起，低速移动的小数进度不会每拍归零。目标离开或看不见时回到四角巡逻。

每拍实际创建一个 `ai.Controller` 与一个 `actionflow.Behavior`（`game/monster/brain.go` 的 `Decide`），AI 经一个瞬时动作提交一次巡逻或追击意图。真正持续的移动只留在支持回滚的 `Body` 里，目标、游标与生命留在 DAO。一次决策的后续步骤失败时，DAO 与 `Body` 同时撤销，不会留下后台动作。

actor 的心跳按场景时间域移动；死亡重生用单独的 1 秒心跳（`monster.respawn`），按业务时间。

## 死亡、重生与停机

死亡在 Monster + Scene 的同一笔事务里把 HP 置 0、让 `Body` 离场（从 AOI 隐藏）并启动重生心跳。截止到达后，心跳在提交之后只向队列投一个 ID、不等待；后台 worker 再发 Monster + Scene 的重生事务：恢复出生位置与 HP、`LifeSeq` 加一、清目标与游标、重启移动。重生失败（例如还没到截止）由下一拍重试。

模块启动时登记了 ingress：停机时先拒绝新操作、取消并等完已接受的操作与队列 worker，再逐只退出场景并销毁。超时或销毁失败返回错误、保留可重试的记录；scene 在 monster 停止成功之后才停。玩家的排空仍按 scene 的协议由业务负责。

## 退出恢复与实例身份

`Destroy` 分两步：`MonsterRemove` 的结果未知时保留记录、重做这一步（它是幂等的）；明确成功后记下已退休，之后只重试 lifecycle 的销毁。删除前核对 EntityManager 里的对象仍是记录的那个实例，同 ID 的新实例拒绝接管。

出生、异步重生、伤害与退出都带着出生槽位固定下来的 `monster.Target`：一个只含本进程内 Monster / Scene 指针的不可变值，handler 在拿到两个实体的锁之后首先核对它。外层检查与 handler 之间发生同 ID 重建也会被拒绝，旧的 `LifeSeq` 不能授权新实例。这个值不能持久化、编码或跨进程传送。
