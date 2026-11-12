# dungeon：服务端推进的单人持久副本，通关后恰好一次领奖

一局副本是一个服务端创建的 `Dungeon` 实体。它的进度只由可信的服务端代码推进（“谁被击败了”这类战斗事实），客户端只能查看进度和在通关后领奖。奖励是经验，经 profile 模块在同一笔事务里发放。

## 能力

### 客户端协议（`protocol/def/dungeon.go`）

- `DungeonView`（请求 / 应答）：`DungeonViewRequest{RunID}`，返回 `DungeonInfo{RunID, Instance, Epoch, Phase, Stage, Kills, Result, ResolvedAtUnix}`。只有这局的所有者能看；查看时会顺带处理超时（见下）。
- `DungeonClaim`（请求 / 应答）：`DungeonClaimRequest{RunID}`，返回 `Claimed`（这一次是否真的发了）、`ExpGranted`、`LevelsGained`、`Season`、`Clears`（本赛季通关数）、`TotalClears`、`BoardPending`（装了 leaderboard 时：奖励已到账但排行榜还没更新）。

两条协议的玩家身份都取自已认证的会话。**没有让客户端提交战斗事件或胜利的协议。**

### 服务端入口（`game/controllers/dungeon/controller.go`，经 `FromRegistry(registry)` 取得 `Controller`）

```go
run, err := controller.Open(ctx, uniqueID, playerID, season)            // 开一局；同 ID、同所有者、同赛季的重试只读
run, err = controller.Defeated(ctx, uniqueID, run.Instance, run.Epoch, "battle", 1, "minion")  // 上报一条战斗事实
result, err := controller.Claim(ctx, uniqueID, playerID)                 // 领奖（DungeonClaim 端点调的就是它）
```

- `Open` 的 `uniqueID` 必须由工程的全局 ID 分配器给出、永不复用；换所有者或赛季答 `dungeon_invalid`。
- `Defeated(ctx, uniqueID, instance, epoch, source, sequence, enemy)`：`enemy` 只认 `"minion"` 与 `"boss"`；每个 `source` 的 `sequence` 单调递增。
- Nest handler（`game/handler/dungeon.go`、`dungeon_counts.go`，都不直接对客户端开放）：`handlerDungeonOpen`、`handlerDungeonDefeated`、`handlerDungeonView`、`handlerDungeonClaim`（同时锁 `Dungeon` 与 `Player` 两个实体）、`handlerDungeonCounts`。
- 组件 `DungeonComp()`（`game/entities/player/dungeon_component.go`）：`Claim(runID, season, amount, resolvedAt, now)`、`Counts(season)`。

### 关卡规则（`game/dungeon/rules.go`，归工程）

`Program()` 用框架的 `gameplay/level`（事件、条件、动作、计时器）定义缺省关卡：`waves` 阶段累计击败两只 `minion` → 切到 `boss` 阶段 → `boss` 被击败才产生 `success`。并行的 `deadline` 阶段在开局 30 分钟后把结果定为 `failed`；没有周期任务，超时由下一次查看或下一条事件顺带处理。同文件的常量：`ClearExp`（100 经验）、`ClaimWindowSeconds`（4 小时）、`LedgerPruneGraceSeconds`（10 分钟）、`MaxClaims`（128）、`MaxSeasons`（8）。

### 登记点

`player.OnDungeonClaimed(name, DungeonClaimedHook)`（`game/entities/player/dungeon_hooks.go`）：副本**首次**领奖成功时执行，重复领取不执行。钩子签名 `func(owner *Player, runID, season, nowUnixMilli int64) error`，在 Player 锁内、领奖的同一笔 Nest 事务里，只能改同一个 Player、不能做 I/O；返回错误让奖励、账本、计数一起回滚。只在 `init` 登记，按名字顺序执行，空名、nil、重名 panic。

### 状态

- `Dungeon` 实体（`game/entities/dungeon/entity.go`）带两个持久 DAO：`DungeonDao`（`db/def/dungeon.go`：`OwnerID`、`Season`、`ExpReward`、`ResolvedAtUnix`）与 `DungeonLevelDAO`（`db/dungeon_level.go`，是 `level.LevelDAO` 的别名，保存关卡状态机）。都不下发。
- `PlayerDao.Dungeon`（类型 `PlayerDungeonState`）：`Claims`（已领的局 → 结算时刻）、`Clears`（赛季 → 通关数）、`TotalClears`。只落库、不下发；客户端从 `DungeonClaim` 的应答里看到计数。

### 配置

没有配置表与配置键。规则写在 `game/dungeon/rules.go`。

### 错误码（`internal/errors/dungeon.go`）

`dungeon_invalid`（参数无效或同 ID 内容冲突）、`dungeon_owner`（不是这局的所有者）、`dungeon_unfinished`（还没成功通关）、`dungeon_window`（领奖窗口已关闭）、`dungeon_full`（领奖账本或赛季计数到上限）。事件串局、旧序号、内容冲突由 `gameplay/level` 自己的错误拒绝；经验溢出透出 profile 的码。

**日志**：开局、领奖确认记 Info；可信战斗事件返回终局记 Debug（接口没有玩家号）；记录局、实例 / 代数、经验、赛季。端点内部错误补 Error。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/dungeon/controller.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不判定战斗是否真实发生。** `Defeated` 信任调用方；工程的战斗服务负责证明击杀。模块也不含战斗、怪物、掉落表、多人副本、扫荡、体力消耗。
- **不负责把请求路由到持有这局 `Dungeon` 的 game 进程**，不提供跨服战斗仲裁。
- **不销毁副本实体。** 奖励窗口与审计保留期结束后的归档 / 销毁由工程做；模块不清除关卡、不复用 ID。
- **奖励固定。** 每局 100 经验，开局时写进 `DungeonDao.ExpReward`；没有按关卡、按评分的奖励表。
- **数量上限：**
  - 每个事件来源只保留最近的序号与摘要，来源最多 4 个（`level.Limits{Sources: 4}`）。
  - 领奖账本最多 128 条；一条记录要等领奖窗口关闭再过 10 分钟才能清理；满了就拒绝新领取（`dungeon_full`），不淘汰仍有效的记录。
  - 赛季计数最多 8 季，第 9 个赛季的首次领奖被拒，需要工程自己定归档规则。
- **领奖窗口：** 成功后 4 小时内可领（恰好第 4 小时仍可领），以首次可信成功时保存的 `ResolvedAtUnix` 为准，不接受客户端时间。
- **一致性与幂等：**
  - `Open`、`Defeated`、`DungeonView`、领奖都是 `rollback=undo durability=strict` 的 Nest 事务。同序号同内容的事件重试零写入。
  - 领奖同时锁 `Dungeon` 与 `Player`：记账本、发经验、加赛季通关数与总通关数、执行 `OnDungeonClaimed` 的钩子在一笔事务里，提交失败整体回滚；重复领取返回当前计数、什么都不写。
  - 排行榜投影（装了 leaderboard 时）在事务之外，是尽力而为：失败只把 `BoardPending` 置 true 并记一条 Warn，没有后台自动重试。
- **规则变更：** 冷加载时关卡定义的 hash 对不上会拒绝恢复；修改已经有存档的 `Program()` 要保留旧定义或显式迁移。两份 DAO 缺一半、身份不符的存档不能继续领奖。
- **前提：** 依赖 profile 模块（发经验）与 nest Mod。两个实体必须由同一个 game 进程持有才能领奖。模块自己不声明玩家接入方式，玩家 TCP 接入由连带装上的 enter（profile 依赖它）代办；`PlayerDao.Dungeon` 不同步。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“业务模块目录”一条说明 game-demo 自己的副本仍在 demo 里、没有换成这个模块；“模块登记点”“事务内事件”两条说明首次领奖钩子的事务范围。

## 复用方式

- **需求正好是它：** `roost module add dungeon`，连带装上 profile（profile 又依赖 enter）。想要通关数排行榜再 `roost module add leaderboard`。
- **需求比它多一点：**
  - 换关卡流程、时限、奖励数额、窗口与上限：改 `game/dungeon/rules.go`。改了 `Program()` 的定义要考虑已有存档。
  - `enemy` 的取值写死在 `game/entities/dungeon/run.go` 的 `Defeated` 里，加新的敌人种类要同时改那里与 `Program()`。
  - 首次通关要顺带推进任务、成就：登记 `player.OnDungeonClaimed`，不要改 `dungeon_component.go` 的判断顺序。
  - 奖励想发别的东西：在 `DungeonComponent.Claim` 里把 `ProfileComp().AddExp` 换成同一实体上别的组件的入口，保持“先检查、后写、账本同事务”。
- **别的业务代码要开局 / 上报战斗结果：** `dungeon.FromRegistry(registry)` 取 `Controller`，调 `Open` 与 `Defeated`；每次都要检查错误。读某赛季通关数用 `DungeonCountsSender` 的 `Sync_DungeonCounts(ctx, playerEntityID, season)`。
- **只适合借鉴的情形：** 多人副本、实时战斗在副本进程内结算、客户端权威的单机关卡都不适合直接用。值得借鉴的是“关卡结果是持久事实，领奖事务只读它，不信请求里的结果”与“有界账本 + 过了窗口再宽限才清理”。

## 联动

本模块的 `with/`：

- `with/leaderboard`（`game/controllers/dungeon/dungeon_leaderboard.go`）：领奖提交之后，把这名玩家该赛季的通关数用 `leaderboard.SubmitSeason` 投影到排行榜（只升不降）。失败返回 `BoardPending`；同一次领取可以重试，窗口之外用 `controller.RefreshLeaderboard(ctx, playerID, season)` 从 Player 的持久计数恢复。排行榜用的是 leaderboard 缺省的那张榜，工程同时给它接别的积分来源时要自己统一规则。

别的模块的 `with/dungeon`：

- `task`：副本首次领奖推进任务，重复领取不重复计算。

## 验证范围

`roost module verify` 跑 19 条场景（`^TestDungeon`），与 leaderboard 的联动另 3 条（`^TestWithDungeonLeaderboard`），零跳过。场景是离线的：用真实生成的 Sender、Nest、controller 与两个 DAO 的持久形态，覆盖开局幂等、阶段条件、事件重放与冲突、4 个来源的上限、非所有者与未通关不能领、重复与并发领奖只发一次、提交失败回滚、窗口边界、账本清理与满额、8 季上限、冷恢复与残缺存档拒绝、协议端点。提交器与 rank 的传输是内存替身；没有验证真实 Mongo / WAL、Redis / NATS、跨进程部署与崩溃后的耐久性。
