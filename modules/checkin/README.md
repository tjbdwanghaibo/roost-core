# checkin：每日签到（按业务日切分，连续天数加成，奖励发金币）

## 能力

每名玩家每个业务日可以签到一次，奖励是金币，连续签到的天数越多奖励越高。“业务日”指按部署配置的业务时区与每日重置时刻划分的一天（框架的 `clock.BusinessDay`，缺省 UTC 零点）。

- **客户端协议**
  - `CheckinClaim`（`protocol/def/checkin_claim.go`）：领今天的签到奖励。请求没有字段；应答带 `Streak`（连续天数）、`Reward`（本次金币）、`Balance`（发放后的余额）。端点是 `game/controllers/checkin/checkin_claim.go` 的 `HandleCheckinClaim`，目标玩家取自认证会话。
  - 没有“查询签到进度”的协议：进度随同步下发给本人（见“状态”）。
- **服务端入口**
  - `CheckinComp().Claim(nowUnixMilli int64) (CheckinResult, error)`（`game/components/checkin/checkin.go`）：要求调用方已持有这名 `Player` 的锁。`CheckinResult` 带 `Streak`、`Reward`、`Balance`。
  - `ICheckinEntity`：窄实体接口，别的 handler 的目标参数嵌入它即可调用组件。
  - Nest handler `handlerCheckinClaim`（`game/handler/checkin_claim.go`，`durability=strict`）：协议背后的事务；时间参数 `nowUnixMilli` 由 Nest 填这条消息冻结的业务时间，客户端不传时间。
- **登记点 / 钩子**
  - `OnCheckinClaimed(name string, hook CheckinClaimedHook)`（`game/components/checkin/hooks.go`）：签到成功后执行。钩子签名是 `func(owner view.PlayerEntity, result CheckinResult, nowUnixMilli int64) error`。
  - 钩子在 `Claim` 的同一笔事务里、`Player` 的实体锁内执行；只能改这名 `Player` 的状态，不能发消息或调别的服务。
  - 钩子返回错误会让这一次签到整笔回滚（奖励、进度、此前钩子写的内容都撤销），玩家之后可以再签。
  - 只在 `init` 里登记；多个钩子按名字排序执行；重名、空名、`nil` 直接 panic。
- **状态**
  - `PlayerDao.Checkin`（类型 `CheckinState`，`db/def/checkin.go`）：`LastDay`（最近签到的业务日，形如 `20261110`，0 表示从未签到）、`Streak`（连续天数）、`Total`（累计次数）。标记为 `persist,sync`：落库，并随同步下发给玩家本人。
- **配置**
  - 没有配置表。奖励规则是 `game/components/checkin/checkin.go` 里的两个常量：`CheckinBaseReward = 10`（第 1 天的金币数）与 `CheckinStreakCap = 7`（倍数封顶）。连续第 n 天发 `10 × min(n, 7)` 金币。
  - 业务时区与每日重置时刻是部署配置 `time.business_timezone` / `time.business_day_start`，不属于本模块。
- **错误码**（`internal/errors/checkin.go`）
  - `checkin_done`：这个业务日已经签过。

**日志**：签到确认，记录连续天数、奖励和余额。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/checkin/checkin_claim.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有补签、没有按月的签到日历、没有累计签到里程碑奖励、没有道具奖励（只发金币）、没有奖励配置表。断签后连续天数从 1 重新计，不保留历史最高值。
- **频率**：每业务日至多成功 1 次。判断依据是 `LastDay >= 今天`，所以业务时钟被往回拨时，在重新走过上次签到那一天之前都不能再签。
- **一致性与幂等**：进度（`LastDay`、`Streak`、`Total`）、金币（`WalletComp().Add`）与钩子的写入在同一笔 Nest 事务里，任何一步被拒绝都一起回滚。“领过没有”记在 `LastDay` 里，所以重发的请求第二次答 `checkin_done`、不写任何字段，不依赖货币模块的去重窗口。
- **“昨天”按日历算**：取今天业务日起点之前那一刻所在的业务日，不按固定 24 小时推（夏令时那天不是 24 小时）。
- **进度是惰性更新的**：只在签到那一次写入时更新。跨天之后、当天签到之前，客户端手里仍是上一次的记录，要自己比对当前业务日：`LastDay` 等于今天是已签；等于昨天是今天未签、连续天数还作数；更早则连续已断，下一次签到从 1 计。当前业务日由 `servertime` 模块下发，没装它时客户端要按业务时区自己推算。
- **前提**：依赖 `wallet` 模块（同一个 `Player` 实体上的 `WalletComp()`）；需要 `nest` Mod；实体必须叫 `Player`。只在持有这名 `Player` 的进程内有效。
- **已知限制**：见 `docs/maintenance/KNOWN-LIMITS.md` 的“惰性清零对客户端可见”“模块安装绑定 Player 与工程形状”“wallet 的去重窗口不是永久账本”三条。

## 复用方式

- **需求正好是它**：`roost module add checkin`，连带安装 `wallet`。
- **需求比它多一点**
  - 改奖励数额或封顶：改 `game/components/checkin/checkin.go` 的 `CheckinBaseReward`、`CheckinStreakCap`，或直接改 `Claim` 里算 `reward` 的那几行（常量可以改名、删掉，测试不引用它们）。**同时改 `game/handler/checkin_test.go` 里的两条用例**，把样例规则的数字换成自己的：`TestCheckinFirstClaimPaysTheBaseReward`（第 1 天的奖励与余额）与 `TestCheckinConsecutiveDaysGrowTheStreakAndReward`（逐天的奖励表，样例是 `10, 20, …, 70, 70, 70`）。其余 6 条只断言签到自己的事实（同日拒绝零写入、断签从第 1 天算、回滚、钩子），拿应答里的 `Reward` / `Balance` 比对，不用改；task 的签到联动（`TestWithTaskCheckin*`）同样不写死签到奖励。
  - 奖励改成道具或多种奖励：在 `Claim` 里把 `WalletComp().Add` 换成（或加上）同一个实体上别的组件的调用，保持在同一笔事务里。
  - 签到成功时顺带做别的事（任务进度、活动计数）：在自己的包的 `init` 里调 `OnCheckinClaimed("<功能名>", 钩子)`，不要改 `Claim`。不想让签到因为自己失败的功能，在钩子里把错误吞掉。
  - `game/components/checkin/hooks.go` 的登记与执行顺序逻辑不需要改。
- **别的模块 / 业务代码要用它**
  - 读进度：在持有 `Player` 锁的 handler 里读 `owner.Dao().GetCheckin()` 的 `GetLastDay()` / `GetStreak()` / `GetTotal()`。
  - 代玩家签到（例如运营补签当天）：已持有 `Player` 锁时在自己的 Nest handler 里调 `target.CheckinComp().Claim(nowUnixMilli)`；没有持锁时用协议 handler 的生成 Sender：`syncsender.NewCheckinClaimSender(client).Sync_CheckinClaim(ctx, 完整实体号, &pb.CheckinClaimRequest{})`。模块没有另外提供专给服务端用的 handler。
- **只适合借鉴的情形**：要做“每日一次”的其他功能（每日礼包、每日免费抽）时，值得照抄的是三点：用 `clock.BusinessDay` 切日、把“做过没有”记在自己的状态里而不是靠货币去重、用 `clock.DayStart(now).Add(-time.Nanosecond)` 求昨天。月历式签到、补签需要另写状态结构，不适合在它上面改。

## 联动

- `task` + `checkin`（在 `task/with/checkin`）：经 `OnCheckinClaimed("task", …)` 在签到成功时上报任务事件 `checkin.claimed` 一次，与签到同一笔事务。
- 本模块自己没有 `with/` 目录。

## 验证范围

- `roost module verify checkin`：8 个场景（`./game/handler`，`^TestCheckin`）。奖励规则的数字只写死在两条里（见“复用方式”），不引用 `game/components/checkin/checkin.go` 的常量：常量改错了，引用常量的断言照样通过。
- 场景是离线的：真实的 Nest 引擎、生成的 DAO 与 Sender、真实的 `wallet` 组件；提交器是替身（`checkinCommitter`，不落盘，可注入拒绝），业务时钟由用例拨动（缺省 UTC 零点切日）。
- 覆盖：首次签到发基础奖励、同日第二次零写入、连续天数与奖励增长、断签重置、提交被拒后进度与金币一起回滚、连续签到超过钱包回执窗口后仍然能发、钩子每次成功签到执行一次、钩子失败整笔回滚。
- 没有验证：真实数据库、经 TCP 的客户端链路、非 UTC 业务时区与夏令时当天的行为（代码按日历计算，但没有对应场景）。
