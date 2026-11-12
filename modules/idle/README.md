# idle：按两次领取之间的时间惰性发金币的离线收益

玩家每次点“领取”，服务器按上一次结算到现在过了多久算出金币并发放。没有定时器，也不判断玩家是否在线：在线停留的时间同样累计。金币经 wallet 模块到账。

## 能力

### 客户端协议（`protocol/def/idle_claim.go`）

`IdleClaim`（请求 / 应答）：`IdleClaimRequest{Sequence}`，返回 `Reward`（这次发了多少）、`Balance`（到账后的余额）、`SettledAt`（已经结算到的服务器时刻，Unix 毫秒）、`Sequence`、`Capped`（是否触到时间或金额上限）。

- 请求里只有一个单调序号，不接受客户端的时间、离线时长或奖励数额。
- 第一次发 `Sequence=1`：只建立计时起点，`Reward` 为 0，不追溯建档之前的时间。
- 之后每次新的领取用上次成功的序号加 1。客户端从自身同步视图的 `Idle.Sequence` 取得当前序号；超时应先用相同的序号重试。
- 玩家身份取自已认证的会话。

### 服务端入口

- 组件 `IdleComp()`（`game/entities/player/idle_component.go`）：`Claim(sequence, nowUnixMilli int64) (IdleResult, error)`，只能在持有 Player 锁的 Nest 事务里调，错误必须交还 Nest。`IdleResult{Reward, Balance, SettledAt, Sequence, Capped}`。
- 结算函数 `idleAccrue(last, now, rules)`（同文件）：不逐秒循环，按整周期数一次算出。
- Nest handler `handlerIdleClaim`（`game/handler/idle_claim.go`，`rollback=undo durability=strict`），`nowUnixMilli` 由 Nest 填这条消息冻结的业务时间；端点 `HandleIdleClaim`（`game/controllers/idle/idle_claim.go`）直接分发到它。

### 登记点

无。模块不挂进入游戏的钩子，领取成功也没有钩子。

### 状态

- `PlayerDao.Idle`（类型 `IdleState`，`db/def/idle.go`）：`Initialized`、`SettledAt`（结算游标）、`Sequence`（最后成功的序号）。落库并同步给玩家自己。
- `PlayerDao.IdleReceipt`（类型 `IdleReceipt`）：最后一次成功领取的完整回包（`Reward`、`Balance`、`SettledAt`、`Capped`）。只落库、不下发。两者都是固定大小。

### 配置

没有配置表与配置键。规则是工程的业务文件 `game/entities/player/idle_rules.go`：`idleRules()` 返回 `IdleRules{PeriodMillis, CoinsPerPeriod, OfflineCapMillis, RewardCap}`，缺省每 60 秒 1 金币，最多累计 8 小时或 480 金币。

### 错误码（`internal/errors/idle.go`）

`idle_sequence`（序号不是下一个：更旧、跳号、非正数，或序号已耗尽）、`idle_clock_back`（业务时钟早于上次结算时刻）、`idle_rules`（规则或业务时间不合法）。金币余额溢出原样透出 wallet 的码。

**日志**：领取确认，记录序号、奖励、余额和封顶；时钟回拨拒绝记 Warn。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/idle/idle_claim.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不判断在线 / 离线。** 收益只看两次结算之间的时间，名字叫“离线收益”，实际是“挂机收益”。
- **只发 wallet 的金币，只有一条收益线。** 没有多货币、多条产线、生产队列、加速道具、按离线时段分段的规则、收益预览协议（客户端只能按规则自己估算展示）。
- **上限：** 时间上限 `OfflineCapMillis`（缺省 8 小时）与金额上限 `RewardCap`（缺省 480），任一触到就算封顶。
- **零头与封顶的处理：** 没封顶时，不满一个周期的零头留给下一次（游标只前移整周期数）；封顶时游标直接推到当前时刻，超出的时间与零头全部丢弃，领完不会马上又能领同一段。
- **规则变更：** 对还没结算的整个区间生效（包括变更之前已经过去的时间），已经领取的结果不重算。
- **一致性与幂等：**
  - 发金币、前移游标、记序号、写回执在一笔事务里；钱包溢出或提交被拒全部回滚，之后可以用同一个序号重试。
  - 当前序号的重发返回原回包、不再结算（即使隔了很久）；更旧的序号与跳号一律拒绝，所以旧请求永远不会消费新的时间段。防重放只靠“最后一条回执 + 单调序号”，没有增长的账本。
  - 业务时钟回拨到上次结算之前时拒绝、零写入，不会重复发。
- **前提：** 依赖 wallet 与 nest Mod。只操作一个 Player。客户端要靠同步视图里的 `Idle.Sequence` 才知道下一个序号（协议不回答“现在是第几号”，序号不对只答 `idle_sequence`），所以清单声明了 `access: {transport: tcp, self_sync: true}`：工程还没有玩家 TCP 接入与自身视图时（没装 enter），安装器代办 `roost add transport tcp` 并打开 `access.player.self_sync`；已经有的不重复做。真实客户端连上还要工程自己实现 `internal/access/player/tcp/auth.go`（缺省拒绝一切）、`roost config enable player-tcp`，并有建档的入口（enter 模块或自己的进入流程）。
- **已知限制：** KNOWN-LIMITS 里没有单独的 idle 模块条目。

## 复用方式

- **需求正好是它：** `roost module add idle`，连带装上 wallet。
- **需求比它多一点：**
  - 改速率与上限、按玩家等级给不同规则：改 `game/entities/player/idle_rules.go` 的 `idleRules()`（它是组件的方法，可以读同一个 Player 上别的组件）。规则必须满足周期、每周期金币、金额上限为正，时间上限不小于一个周期，否则每次领取都答 `idle_rules`。
  - 发别的资源：改 `idle_component.go` 的 `Claim` 里 `WalletComp().Add(reward)` 那一步，保持与游标、序号、回执在同一笔事务里。
  - 想在进入游戏时自动结算一次：自己写一个 Nest handler 调 `IdleComp().Claim`，注意序号由服务端决定时要与客户端的序号约定一致。
- **别的业务代码要用它：** 在自己的 Nest handler 里（目标实体实现 `player.IIdleEntity`）调 `target.IdleComp().Claim(sequence, nowUnixMilli)`，`nowUnixMilli` 用 Nest 填的冻结时间。只读当前游标与序号读 `PlayerDao.Idle`。
- **不要改的：** `Claim` 里“当前序号重放 → 序号检查 → 结算 → 同事务写游标与回执”的顺序，以及 `idleAccrue` 里先比较上限再做乘法的写法（防溢出）。
- **只适合借鉴的情形：** 多建筑各自产出、带队列与加速的放置游戏不适合直接用，要自己建模每条产线。值得借鉴的是“惰性结算 + 零头保留 + 封顶时重置起点”和“固定大小的单调序号防重放”。

## 联动

无。本模块没有 `with/`，也没有别的模块带 `with/idle`。

## 验证范围

`roost module verify` 跑 19 条场景（`^TestIdle`，`game/handler` 15 条、`game/entities/player` 4 条），零跳过。场景是离线的：用正式的协议、controller、Nest 加替身提交器，覆盖首次只建立起点、整周期发放、零头保留、时间上限与金额上限、恰好封顶时重置起点、当前序号重放返回原回包、旧序号与跳号拒绝、初始化与发放的提交被拒后回滚并可重试、时钟回拨不重复发、钱包溢出时保留未领的时间、并发重复只发一次、序号耗尽、极大的时间与速率不溢出、协议用服务器时间。

没有经过真实存储与真实 TCP 的验证。仓库一侧另有专项测试 `TestModuleGachaIdleScenariosAndMutations`（两种可编译变异），它不属于 `roost module verify`。
