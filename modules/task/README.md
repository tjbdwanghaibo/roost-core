# 任务模块

`roost module add task` 安装任务、wallet、配置表和默认消费联动。任务的进度与领奖事实保存在 Player DAO，奖励与领奖在同一笔 Nest 事务内提交；这里不需要 Saga。

## 最小用法

默认 `configs/table/task.csv` 有三条每日任务：签到 1 次给 20 金币、消费 3 次给 30 金币、副本领奖 10 次给 100 金币。装了对应模块，联动自动登记。改表后运行 `roost generate`，运行期沿用 configdata 的请求快照和热更。

在已经锁住 Player 的 Nest handler 中：

```go
if err := player.TaskComp().Report("battle.won", 1, nowUnixMilli); err != nil {
    return err
}
```

事件由服务端提供，不直接开放给客户端。`Advance(taskID, count, nowUnixMilli)` 仍可指定任务。独立调用用生成的 `TaskReportSender` / `TaskAdvanceSender`；它们不代替业务去重。事件提供方应在首次生效时上报，重复业务请求不再次上报。

## 周期、任务链与边界

- `period=daily`：按 `clock.BusinessDay` 惰性清零；`Task.Day` 是 YYYYMMDD。
- `period=weekly`：按同一业务日所在周的周一清零；`Task.Weekly.Day` 是周一的 YYYYMMDD。业务时区、每日重置时刻与 servertime 一致，不用毫秒除以固定一周，也不用 YYYYMMDD 除以 7。
- `period=permanent`：永不清零；记录在 `Task.Permanent`。
- `previous`：前置任务必须已经领奖。只允许同周期前置或永久前置；循环、缺失、停用前置导致该任务不可用。一次事件不会沿链重复消费，解锁前的事件不追溯。
- 时钟回拨不清零；过期未领奖的每日/每周任务不能补领。领取被拒不顺便清零；只有有效推进执行周期重置。
- 业务目录只接受 ID **1～4096**，进度封顶在目标。周期各自最多 4096 条进度/领奖记录，表热更反复增删也有上界。ID、周期、前置关系发布后固定；停用用 `disabled`，不可复用已发布 ID。永久任务停用或移除也保留领奖事实，重新出现不会再次发奖。
- 标量列的 `required/min/enum` 由配置装载器检查；ID 范围、奖励处理器和前置关系由任务目录检查，无效行不可推进/领奖。事件索引随不可变配置表建立，同一消息使用同一份表。
- 改目标会影响尚未领取的任务；已领奖不会重开。奖励在领取时按当前配置读取，不承诺完成时冻结奖励。

客户端读取三个周期的进度：旧 `Day` / `Weekly.Day` 不属于当前周期时显示为零，永久记录一直有效。`TaskClaimResponse` 返回 `RewardKind`、`RewardID`、数量 `Reward` 与金币余额 `Balance`。

## 奖励与事件登记

奖励表达为 `Kind / ID / Count`，与邮件附件含义一致，但不为这个值类型强制启动 mail 服务。`coins` 内置且 ID 必须为 0；装 bag 后支持 `item`。自定义资产通过 `RegisterTaskReward` 在 init 登记校验与发放函数；发放只改同一个 Player，错误交还 Nest，不能在钩子里调用外部服务。

| 联动 | 事件 | 次数含义 | 重放 |
| --- | --- | --- | --- |
| wallet | `wallet.spent` | 成功消费笔数 | `Apply` 去重命中不计；`Add` 成功扣款计一次 |
| checkin | `checkin.claimed` | 成功签到次数 | 当日重复拒绝不计 |
| shop | `shop.bought` | 购买份数 | 购买回执重放不计；扣款另计一次 wallet 事件 |
| stamina | `stamina.spent` | 实际消耗点数 | 每次成功 `Spend` 计数 |
| profile | `profile.level_up` | 提升的等级数 | 一次连升多级按差值；首次建档 1 级不计 |
| dungeon | `dungeon.claimed` | 首次副本领奖次数 | 已领奖不再计数 |
| leaderboard | `leaderboard.submitted` | rank 成功答复的提交次数 | 同分重放也计数；见下方边界 |

前六项在同一笔 Player 事务中推进，提交失败会一起回滚。各功能在 `init` 登记自己命名的钩子，提供方按名字稳定执行；空名、nil、重名在启动时拒绝。

排行提交发生在事务外，`OnSubmitted` 只报“rank 已成功答复”，不报“首次结算”或“分数增加”。任务联动经独立 Nest 消息尽力推进；失败记 Warn，不伪装成 rank 失败。它适用于“提交过榜单”等允许重复计数的目标，不用于恰好一次的通关/交易奖励；这些必须使用原结算事务的事件。默认表没有排行任务，未配置时不发额外 Nest 请求。

## 验证范围

本轮定向验收入口：`TestModuleTaskEventsPeriodsAndMutations`。包含默认每日场景、跨周/回拨、永久与前置链、奖励、表校验、联动去重与回滚；新工程验证同时包含 build / vet / ID / 生成物漂移检查。

这里仍是本地 Nest + 生成 DAO + 替身提交器/Rank 的离线验收。真实 TCP、WAL、Mongo、NATS 与跨进程验收列在[任务交接](../../docs/maintenance/HANDOFF-2026-11-12-module-requirements.md) §6，不能用离线场景替代。
