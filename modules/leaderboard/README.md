# leaderboard：经托管 rank 服务的排行榜——读前 N 名与自己的名次，分数由服务端提交

排行榜的数据不在游戏服，而在框架托管的 rank 服务里；rank 只给数字排名，不关心数字代表什么。这个模块是游戏服这一侧的约定：有哪张榜、多少分算合法、客户端怎么读、服务端别的功能怎么把分数报上去。缺省是一张全服总分榜、只有一个周期。

## 能力

### 客户端协议（`protocol/def/leaderboard_top.go`、`leaderboard_me.go`）

- `LeaderboardTop`（请求 / 应答）：`LeaderboardTopRequest{Limit}`，返回 `Rows`（每行 `LeaderboardRow{Rank, PlayerID, Score}`，名次从 1 起）、`Total`（榜上总人数）以及调用者自己的位置 `MyOnBoard` / `MyRank` / `MyScore`。`Limit` 不填或为负按 `DefaultTop`（10），超过 `MaxTop`（50）按 50。
- `LeaderboardMe`（请求 / 应答）：请求体为空，返回 `OnBoard` / `Rank` / `Score`。给只刷新“我在哪”的界面用，少读一页。

两条协议都只读；没上榜不是错误（`OnBoard` 为 false，名次与分数为 0）。查的是谁取自已认证的会话；读哪张榜、哪个周期由服务端定，请求里没有榜名。**没有让客户端提交分数的协议。**

### 服务端入口（`game/leaderboard/leaderboard.go`）

- `Submit(ctx, registry, playerID, total) (Standing, error)`：把玩家**当前总分的快照**提交到当前周期的榜，返回提交之后他的位置 `Standing{OnBoard, Rank, Score}`。
- `SubmitSeason(ctx, registry, season, playerID, total) (Standing, error)`：同上，但周期由调用方指定。给总分按周期分开存的功能用。
- `Client(registry)`：取 rank 服务的客户端。
- 辅助：`CurrentBoard()`、`ValidateScore(total)`、`TopLimit(requested)`、`PlayerScore(playerID, total)`、`StandingOf(entry, found)`。
- 端点 `HandleLeaderboardTop` / `HandleLeaderboardMe` 在 `game/controllers/leaderboard`，直接调 rank 服务，不经过 Nest、不取任何实体锁。

### 登记点

`leaderboard.OnSubmitted(name, SubmittedHook)`（`game/leaderboard/leaderboard_hooks.go`）：每次 `Submit` / `SubmitSeason` 得到 rank 的成功答复之后执行，钩子签名 `func(context.Context, *app.Registry, Submission) error`，`Submission{Board, PlayerID, Total, Standing}`。它在事务之外执行，可以再发一条 Nest 消息，不能直接操作实体；失败只记一条 Warn。只在 `init` 登记，按名字顺序执行，空名、nil、重名 panic。

注意它的含义是“rank 答复了这次提交成功”，不是“分数增加了”：重复提交同一个快照也会触发。

### 状态

无。模块不往任何实体上加字段：玩家的总分由产生分数的功能保存在它自己的状态里，榜只是那个数的投影。

### 配置（`game/leaderboard/leaderboard_board.go`，归工程）

没有配置表与配置键，榜的身份与规则是这个业务文件里的常量与函数：

- `BoardID`（缺省 `"total"`）：榜在 rank 服务里的名字，是存储键的一部分。
- `Board(season)`：榜的范围，缺省全服一张（`svcrank.ScopeGlobal`）。
- `CurrentSeason()`：当前周期号，缺省返回 1。
- `MinScore` / `MaxScore`：合法分数范围，缺省 1 到一万亿，两端都含。
- `DefaultTop` / `MaxTop`：一屏榜单的缺省条数与上限，缺省 10 / 50。

业务服务要在 `roost.yaml` 里 `uses: [rank]`。

### 错误码

模块没有自己的错误码。参数被拒时返回 rank 服务的带码错误：`svcrank.ErrScoreInvalid`（总分越界）、`svcrank.ErrOwnerInvalid`（玩家号不为正）、`svcrank.ErrBoardInvalid`（周期不为正或榜不合法）。rank 服务读失败时，带码的原样给客户端，不带码的记日志、回内部错误。

## 能力边界

- **分数只升不降。** 提交用的是 rank 的 `UpdateMax`：新的总分不比已存的高就不写。所以会下降的分数（天梯分）不能用这个入口；一个错的大数提交上去之后压不下来——`MaxScore` 就是用来挡算错的值的。
- **提交的是总分快照，不是增量。** 没有“加 delta”的入口，也不带幂等键；快照加 `UpdateMax` 本身幂等，重复、乱序的旧快照都不会多加分。总分为 0 不能提交（下限是 1）。
- **提交与改总分的事务不是原子的。** 榜在另一个进程里。约定是：先在自己的事务里把总分改好并提交，再在事务之外调 `Submit`；不要在 Nest handler 里调。`Submit` 失败时不回滚、不让玩家的操作失败，记一条 Warn，靠重试或这名玩家下一次分数变动补上——这段时间榜上暂时落后。
- **只有一张榜可读。** 两条协议固定读 `CurrentBoard()`；`SubmitSeason` 只能选周期、不能选榜。多张榜、读历史周期的榜要自己写端点。
- **不换周期、不清榜、不发奖。** rank 服务不会自己换周期，也不在总线上开放清榜。缺省的换法是改 `CurrentSeason()` 的返回值后发版（也可以改成按业务时钟算）；周期号一变，读写都落到一张新的空榜上，旧榜原样留在 rank 里。周期结算奖励要工程自己在换周期之前读榜发放。
- **不存显示资料。** 榜上只有玩家号与分数，没有名字、头像；客户端按玩家号另取。
- **同分排序：** 先达到这个分数的在前（rank 记首次达到的时间），不是玩家号小的在前。
- **读的一致性：** 每一行的名次与分数来自同一次读取。`LeaderboardTop` 里调用者不在这一页时，“我的位置”是紧接着的第二次读取，与榜头可以相差一瞬；第二次读失败时整条请求按失败回答，不回半截数据。
- **上限：** 一次至多读 50 行（`MaxTop`；rank 服务自己的页上限是 200），没有翻页协议——只能读榜头。
- **`OnSubmitted` 是尽力通知：** 失败只记日志，重放会重复触发，不能用它发需要恰好一次的奖励。
- **前提：** 业务服务 `uses: [rank]`；取不到 rank 的客户端时 `FromRegistry` 返回错误，进程起不来。跨进程调用，受 rank 服务可用性影响。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“leaderboard 的边界”与“模块的规则大多还是 Go 写的业务文件”。

## 复用方式

- **需求正好是它**（一张全服总分榜）：`roost module add leaderboard`，没有依赖；确认业务服务的 `uses` 里有 `rank`。然后在产生分数的功能里提交：

  ```go
  // 自己的 Nest 事务提交之后、在端点里（事务之外）：
  if _, err := leaderboard.Submit(ctx, registry, playerID, total); err != nil {
      slog.Warn("score not on the board yet", "player_id", playerID, "total", total, "err", err)
  }
  ```

  总分为正时才提交；重放发现分已经加过时照样提交当前总分。
- **需求比它多一点**（都改 `game/leaderboard/leaderboard_board.go`，同时改 `leaderboard_board_test.go`，它把这些数钉住）：
  - 换成别的榜（通关次数榜）：改 `BoardID` 与分数范围。上线后改 `BoardID` 等于换一张空榜。
  - 分区榜：把 `Board` 里的范围改成 `svcrank.ScopeServer` 并填区号。
  - 赛季 / 周榜：改 `CurrentSeason()` 的函数体。这时持有总分的功能也要按周期分开存，并用 `SubmitSeason`：先取一次周期号，事务里按它记总分，事务外按同一个周期号提交。
  - 一屏更长：改 `MaxTop`（不超过 rank 的页上限 200）。
- **别的模块要在“有人上榜”时做事：** `leaderboard.OnSubmitted("名字", hook)`，只当作通知。
- **不要改的：** `leaderboard.go` 里 `SubmitSeason` 用 `UpdateMax`、请求号留空的写法（带请求号在重放风暴下会互相冲突，见文件注释）。
- **只适合借鉴的情形：** 分数会升会降的天梯、多张并存的榜、需要翻整张榜、榜上要带展示快照时，不适合直接用，要自己直接调 rank 服务的接口。值得借鉴的是“总分在自己的事务里、榜是事务外的快照投影”这套提交约定。

## 联动

本模块没有 `with/`。别的模块的 `with/leaderboard`：

- `dungeon`：领奖提交之后把赛季通关数用 `SubmitSeason` 投影到榜，失败回报 `BoardPending`，可以重试或用恢复入口补上。
- `task`：登记 `OnSubmitted`，排行成功提交时推进任务；尽力通知、重放会重复计数，不用于恰好一次的结算奖励。
- `activity`：按活动实例的身份隔离活动榜，结算前由业务冻结结果。

## 验证范围

`roost module verify` 跑 20 条场景（`^TestLeaderboard`：`game/leaderboard` 12 条、`game/controllers/leaderboard` 8 条）。覆盖缺省榜与规则的数值、分数范围两端、条数的收拢、`Standing` 的判断、提交用快照加 `UpdateMax` 且不带请求号、`SubmitSeason` 进指定周期、坏参数在调 rank 之前被拒、rank 失败返回给调用方、提交后读榜头与自己的名次、榜头带上调用者的位置、从不向 rank 要超过 `MaxTop` 行、没上榜是答案不是错误、重复与过期的快照不动榜、rank 失败作为码到达客户端、没有 rank 能力时 controller 拒绝启动。

场景是离线的，并且 rank 一侧是**手写的替身**，不是 rank 服务本身：`game/leaderboard` 的场景用只记录调用的替身（`leaderboardRecordingRank`），端点的场景用照 rank 接口文档写的内存替身（`leaderboardFakeRank`）。所以“只升不降”“同分先到在前”这些 rank 的行为在这里是按约定模拟的；真实的 Redis 脚本、跨进程调用、真实 TCP 都没有经过。game-demo 用的是这个模块，但模块自己没有在真实进程上的专项验证记录。
