# dungeon：持久关卡与恰好一次领奖

`roost module add dungeon` 自动安装 profile，建立服务端 Dungeon 实体、两个正式持久 DAO、Player 奖励账本、生成 Nest Sender 与客户端 PB/controller。新建 game 工程不需要补配置。`roost module add leaderboard` 另行启用提交后排行榜投影。

可信服务器从 `game/controllers/dungeon.FromRegistry(registry)` 取得入口：

```go
run, err := controller.Open(ctx, freshUniqueID, playerID, season)
// 后续只接受服务器确认的战斗事实。每个固定 source 的 sequence 单调递增。
run, err = controller.Defeated(ctx, freshUniqueID, run.Instance, run.Epoch, "battle", 1, "minion")
run, err = controller.Defeated(ctx, freshUniqueID, run.Instance, run.Epoch, "battle", 2, "minion")
run, err = controller.Defeated(ctx, freshUniqueID, run.Instance, run.Epoch, "battle", 3, "boss")
```

实际业务必须检查每次错误。Open 的 ID 必须由项目全局 ID 分配器提供，永不复用；同 ID、owner、season 重试只读，换 owner 或 season 拒绝。已结束的同 ID 不会重新开局。路由到持有该 Dungeon 的 game 实例是项目的部署职责；本模块不提供跨服战斗仲裁。

`game/dungeon.Program` 用 `gameplay/level` 的事件、条件、动作与计时器执行：waves 累计两只 minion → boss → boss 击败才产生 success。并行 deadline 在 30 分钟后失败，由查询或后续事件执行 Advance，无周期任务。事件携带 Instance、Epoch、Source、Sequence；同序号同摘要重试零写入，内容冲突、旧序号、串局与超过 4 个来源拒绝。仅保留每个来源的最近序号/摘要，来源集合固定且有界。项目战斗服务负责证明击杀真实发生；客户端没有提交事件或 success 的协议。

客户端仅 `DungeonView(RunID)` 与 `DungeonClaim(RunID)`，owner 来自认证上下文。Dungeon 元数据 DAO 与官方 LevelDAO 均由实体生成器挂入 DaoBuilders/DaoManager；别名适配保持 Level 内部事务参与者与聚合持久参与者为同一指针。冷加载先恢复所有 DAO，再执行实体一致性检查；缺少一半事实或身份不符不能继续领奖。规则 hash 不匹配会拒绝恢复；修改已运行定义时需保留旧定义或显式迁移。

成功后 4 小时内可领（恰好第 4 小时仍可领），时间以首次可信成功时保存的 ResolvedAtUnix 为准。固定奖励 100 经验，由 profile 同事务发放；关卡与 Player 同时锁定。Player 同笔记账、发奖、加赛季通关数与总通关数；提交失败整体回滚，重试不重复奖励。重复领取本地零写入；超过窗口的重复请求拒绝。

领奖事实最多 128 条，只有领奖窗口关闭再过 10 分钟才能清理；满额拒绝新领取，不淘汰仍有效的记录。赛季计数最多 8 季，超过后拒绝第 9 季首次领奖，需项目明确制定归档迁移规则。Dungeon 实体本身由项目在奖励窗口与审计保留期结束后归档/销毁，本模块不擅自清除关卡或重用 ID。

安装 leaderboard 后，已提交的赛季总数经 `leaderboard.SubmitSeason` 以 UpdateMax 投影；外部调用在 Nest 锁与业务 worker 外执行。失败保留本地奖励，返回 BoardPending 并记录带玩家/赛季/总数的告警。相同领取可重试投影；窗口之外用可信 `RefreshLeaderboard(ctx, playerID, season)` 从 Player 持久计数恢复，无需旧 Dungeon 仍在内存。无自动后台重试，运营或业务需调用恢复入口。排行榜是 leaderboard 的默认 Board(season)，项目同时给该榜接其他积分源时需统一榜规则。

验收：基础 19 个独立场景、leaderboard 联动 3 个，零跳过；包括真实生成 Sender/Nest/controller、双 DAO 持久形态与冷恢复、128 条账本、8 季上限、并发领奖、提交拒绝、两个可编译行为变异。提交器和 rank 传输使用内存替身；不宣称已经验证真实 Mongo/WAL、Redis/NATS、跨进程部署或崩溃耐久性。
