# activity：活动生命周期引擎（五阶段状态机、一次性或循环排期、可信条件解锁；个人活动宿主）

`roost module add activity` 安装个人活动及共用引擎；`activity_world` 增加全服 / 服务器组宿主，`activity_alliance` 增加联盟宿主。三种宿主共用同一个 `activity.Component` Go 类型，各实体包只保留类型别名、组件注册和宿主适配。配置表默认为空，不会自动开启任何示例活动。

## 能力

本模块只管“一个活动什么时候开、什么时候关、现在处于哪个阶段”，并在阶段切换时调用业务登记的回调。活动的具体玩法、奖励都由业务写在回调里。

- **客户端协议**
  - `ActivityView`（`protocol/def/activity_view.go`）：查询自己的个人活动。请求没有字段；应答 `Activities` 是 `ActivityState` 列表，每项带 `Definition`、`Generation`、`Cycle`、`Phase`、`Flow`、`FlowVersion`、`Start`、`OpenAt`、`CloseAt`、`Pending`、`Version`。查询时顺带按当前配置对账并推进一次。端点 `HandleActivityView`（`game/controllers/activity/activity_view.go`）。
  - 没有“我已满足条件”之类的上报协议，也没有推送协议。
- **服务端入口**
  - 组件（`game/activity/component.go`，在宿主实体的 Nest 事务里调用）：
    - `ActivityComp().Reconcile(event string, createdAt int64) error`：按当前配置激活符合条件的定义、推进阶段，再更新唤醒期限。
    - `ActivityComp().Host()`：返回一次调用用的状态机入口 `activity.Host`。
    - `ActivityComp().Rearm() error`：直接用过 `Host` 改状态之后调用，重新计算唤醒期限。
  - `activity.Host` 的方法（`game/activity/engine.go`）：`Activate(d, event, factTime, now)`、`Reopen(d, event, factTime, now, nextGeneration)`、`Reconcile(definitions, event, createdAt, now)`、`Advance(now)`、`Complete(token, now)`、`CheckOpen(token, now)`、`MoveFlow(token, next, now)`、`JoinShared(shared, now)`、`Views()`、`NextDeadline(now)`。
  - Nest handler（`game/handler/`，都是 `durability=strict`，没有持有 `Player` 锁的服务端代码经生成的 Sender 调用）：
    - `handlerActivityEvent`（`Sync_ActivityEvent(ctx, 完整实体号, event, createdAt)`）：可信业务入口，上报一个事件并返回全部活动快照。
    - `handlerActivityComplete`（`Sync_ActivityComplete(ctx, 完整实体号, token)`）：确认某一期的异步初始化或结算已经完成。
    - `handlerActivityView`：协议背后的事务。
  - 包级函数（`game/activity/`）：`Definitions(scope)`（读配置表并校验）、`Validate(d)`、`ResumePending(ctx, registry, views, complete)`（锁外继续未完成的异步工作，`resume.go`）、`StartDriver(owned, kind, advance)`（给 Remote 宿主用的恢复扫描，`driver.go`）、`CoordinatorKey(token)`（映射到框架跨服汇聚服务的窗口键，`coordinator.go`）、`NewComponent`（三种宿主共用的组件工厂）。
  - 类型：`Token`（一期活动在某阶段某流程版本的完整身份）、`Snapshot`（一期活动的只读快照）、`Context`（回调参数，`Key(action)` 给出稳定的幂等键）、`Flow{Name, Due}`、`Phase` 及常量 `Initializing`、`Warmup`、`Open`、`Settling`、`Ended`。
- **登记点 / 钩子**
  - `activity.Register(name string, hooks activity.Hooks)`（`game/activity/types.go`）：把一段活动逻辑登记到配置表 `Logic` 列引用的名字上。只在进程启动时（`init`）登记，重名或空名 panic。`Hooks` 有四个可选回调：
    - `Eligible(owner, event) bool`：解锁条件。在宿主实体的事务里调用，读实体的真实状态。
    - `Enter(Context) (pending bool, err error)`：每进入一个阶段调用一次，在宿主实体的事务里。只能改本次已锁住的实体的 DAO 或登记可靠 effect。返回 `pending=true` 只允许在 `Initializing` 与 `Settling` 阶段，表示要等后台工作完成。返回错误则整笔事务回滚。
    - `Flow(Context) (Flow, error)`：自定义流程的期限到了时调用，在事务里，返回下一个流程。
    - `Resume(ctx, registry, snapshot) (done bool, err error)`：在事务之外的 I/O 段继续 `pending` 的工作；返回 `done=true` 后引擎再用原 `Token` 调 `Complete`。可能被重复调用，外部副作用必须幂等。
  - 宿主接口 `activity.Owner`（`ActivityBook()`、`ActivityScope()`）：别的实体想当活动宿主时实现它。
- **状态**
  - `PlayerDao.Activity`（类型 `ActivityBook`，`db/def/activity.go`）：`Records`（定义号 → `ActivityRecord`，含冻结的规则、代际、期次、阶段、流程、各时间点）、`Participation`（参加共享活动的资格）、`TimerID`、`NextDue`、`Cursor`。标记为 `persist,nosync`：只落库，客户端只能经 `ActivityView` 看到。
  - 一个 `Player` 共用一个持久定时器，类型 `ActivityDeadline`（`game/entities/player/activity_timers.go`）。
- **配置**
  - 配置表：列定义 `configs/schema/activity.go`（`Activity`），数据 `configs/table/activity.csv`（默认只有表头）。各列含义见后文“定义与时间”。
  - 常量 `MaxDefinitions = 128`、`AdvanceBudget = 32`（`game/activity/types.go`）。
- **错误码**：模块不定义错误码。引擎返回的是包级错误值 `ErrRule`（规则非法）、`ErrStale`（期次或流程已过时）、`ErrClosed`（不在开启阶段）、`ErrDenied`（解锁条件不满足）、`ErrCapacity`（参与资格已满）；经协议端点返回给客户端时是框架的内部错误码。

**日志**：个人活动异步 Complete 确认，记录活动号、代数、期次、阶段和流程版本；没有阶段变化输出的定时推进不记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/activity/resume.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不含任何具体活动（没有奖励、任务、排行、兑换逻辑）；不发推送，阶段变化要客户端自己查 `ActivityView` 或由业务另行通知；不做活动列表的展示配置（名称、图标、文案）；不替业务决定退盟后的贡献与奖励归属。
- **数量上限**：定义号只能是 1～128，不回收；每个宿主至多 128 条共享参与资格，满了返回 `ErrCapacity`，不驱逐旧资格。每个宿主每次推进至多 32 次阶段或流程动作，在各定义之间轮转，剩余工作至少延后 1 秒继续。预热与开启时长各不超过 366 天。
- **阶段名不是写权限**：业务在改玩法数据之前必须先调 `CheckOpen(token, now)`。它按冻结的期限判断，即使到期消息还在排队也会拒绝过期动作。
- **一致性**：阶段游标、流程、定时器节点与业务在回调里对本实体的修改在同一笔 Nest 事务里提交或回滚。`Enter` 失败不会被吞掉。`pending` 不会因为时间到了而自动确认，必须有人调 `Complete`。
- **规则冻结**：活动激活时把配置行、业务时区与日界一起冻结进记录；之后热更配置不会改已生效的时间线。改规则要提升 `Version`，已激活的序列继续用旧规则。`Disabled` 只阻止创建新实例。
- **时钟**：单调推进，业务时钟回拨不会重开已关闭的阶段。迟到的旧期、旧流程、旧代际请求因 `Token` 对不上而被拒（`ErrStale`）。
- **个人活动是惰性恢复的**：靠 `Player` 的持久定时器、进入游戏时的对账与 `ActivityView` 查询推进。玩家离线期间不保证准点推进或结算；需要离线准点时业务要自己提供可靠的后台驱动。
- **历史不保留**：每个定义只保留当前一期和已完成期次的水位；历史成绩、领奖凭据要业务自己存。
- **前提**：需要 `nest`、`configdata` 两个 Mod 与 `config` 特性；个人持久定时器由安装器接入。不依赖别的模块。实体必须叫 `Player`。个人活动只在持有这名 `Player` 的进程内推进，换服沿用同一份已持久化的数据。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“活动模块的驱动与验收边界”一节。

## 复用方式

- **需求正好是它**：`roost module add activity`。不连带安装别的模块。全服活动另装 `activity_world`，联盟活动另装 `activity_alliance`。
- **做一个具体活动（常见用法）**
  1. 在 `configs/table/activity.csv` 加一行定义（范围、触发方式、时长、循环），`Logic` 填一个名字，然后 `roost generate`。
  2. 在工程自己的文件里 `init` 调 `activity.Register("<Logic 名>", activity.Hooks{…})`，写解锁条件与各阶段要做的事。
  3. 玩法 handler 在同一笔 `Player` 事务里先用 `Views()` 找到这一期的 `Token`，调 `CheckOpen`，通过后再改自己的玩法数据。
  4. 有需要等后台完成的初始化或结算时，`Enter` 返回 `pending=true` 并写下要做的事；完成后调 `Sync_ActivityComplete`，或登记 `Resume`。
- **需求比它多一点**
  - 报名、分组、比赛等子流程：用 `MoveFlow` 与 `Hooks.Flow`，不要扩充阶段枚举。
  - 事件触发的解锁：定义用 `Trigger=event`，在产生事件的那笔 `Player` 事务里调 `ActivityComp().Reconcile("<事件名>", 0)`。
  - 运营手动重开一个已彻底结束的活动：调 `Host().Reopen`，传旧代际加 1。
  - `game/activity/engine.go`、`rules.go` 是三种宿主共用的状态机与时间计算，不建议改；玩法都放在登记的 `Hooks` 里。
- **别的模块 / 业务代码要用它**
  - 已持有 `Player` 锁：`target.ActivityComp().Reconcile(event, 0)` 或 `target.ActivityComp().Host().Views()`；用 `Host` 改过状态后调 `Rearm()`。
  - 没有持锁：`syncsender.NewActivityEventSender(client).Sync_ActivityEvent(ctx, 完整实体号, event, createdAt)`。
  - 外部副作用的幂等键：回调里用 `Context.Key("<动作名>")`。
- **只适合借鉴的情形**：只需要“某个时间段内开放一个入口”且没有结算的简单开关，用不着这个引擎，直接读配置表判断时间即可。值得借鉴的是“激活时冻结规则、`Token` 隔离迟到请求、`pending` 必须显式确认”这三点。

## 联动

- `activity` + `enter`（`with/enter/`）：首次建档时经 `OnFirstEnter("activity", …)` 在建档事务里以建档时间激活 `created` 触发的活动，并把宿主的创建时间接到 `PlayerDao.Enter` 的首次进入时间；每次进入经 `gamelogin.OnEntered("activity", …)` 在事务外对账一次并 `ResumePending`（`with/enter/game/controllers/activity/activity_enter.go`）：钩子拿到的是会话里的玩家号，先用 `entity.BuildEntityID(玩家号, player.EntityKindPlayer)` 换成完整实体号再发 `Sync_ActivityEvent` / `Sync_ActivityComplete`；钩子失败只记日志，下一次进入再来。
- `activity` + `profile`（`with/profile/`）：经 `OnProfileLevelUp("activity", …)` 在升级的同一笔事务里上报事件 `profile.level_up`。
- `activity` + `leaderboard`（`with/leaderboard/game/activity/leaderboard.go`）：`activity.Board(token)` 按“范围、宿主、定义、代际”给出独立的榜，期次作为赛季。
- `activity` + `mailbox`（`with/mailbox/game/activity/mailbox.go`）：`activity.SendReward(ctx, mailer, token, action, letter)` 以“实例身份 + 动作 + 收件人”生成稳定的发件键，只允许在 `Settling` 阶段的 `Token` 上用，且只能在事务之外调用。
- 以 `requires` 依赖它的模块：`activity_world`、`activity_alliance`。

## 验证范围

- `roost module verify activity`：21 个场景（`./game/activity` 与 `./game/handler`，`^TestActivity`）；与 `enter` 的联动 2 个场景（`./game/handler` 与 `./game/controllers/activity`），与 `profile`、`leaderboard`、`mailbox` 的联动各 1 个场景。
- 场景是离线的：真实的 Nest 引擎、生成的 DAO 与实体定时器；提交器是替身。覆盖五阶段与期限、异步初始化与结算、零预热、独立解锁与重复激活、初始化失败回滚、定时器回调出错时保留期限并回滚、循环的有界补期与规则冻结、自定义流程与迟到事件、时钟回拨与显式重开、参与资格不是个人实例、`pending` 经 BSON 往返恢复、并发激活只有一个锚点；规则校验、业务日历不按 24 小时、重叠周期拒绝、跳过空期的有界查找、`Resume` 重试、驱动停止时保留在途工作、汇聚键、相对日历等到业务日界。
- 与 `enter` 的联动：一个场景直接调建档初始化，验证重连不重置创建锚点；另一个经 `gamelogin.RunEntered` 真正执行登记的进入钩子（正式 Nest、生成的 Sender 与 handler），验证钩子按玩家号定位到 `Player`、对账激活、`Resume` 报告完成后用原 `Token` 确认 `pending`，重连再进入不重复 `Resume`、不改时间线。
- 没有验证：经 `EnterGame` 端点与真实会话的整条进入链路（场景从 `RunEntered` 起）；真实的 Mongo、WAL、NATS、跨进程迁移与长时间运行。验收记录见 [证据](../../docs/maintenance/evidence/module-activity-20261112/README.md)。

## 生命周期

标准阶段只有五个：**Initializing → Warmup → Open → Settling → Ended**。

| 阶段 | 进入后的含义 |
| --- | --- |
| Initializing | 冻结定义、时间锚点与期次，准备业务状态；允许等待可靠后台完成 |
| Warmup | 等待开启时间；零预热会在同一事务继续推进 |
| Open | 允许业务按规则操作；实际操作必须先调用 `CheckOpen` |
| Settling | 关闭新行为，完成本期业务；允许等待异步结算 |
| Ended | 本期完成，保存终态水位；循环在固定排期创建下一期 |

报名、分组、比赛是 `Flow`，例如 `Open + registration`，不扩充阶段枚举。`MoveFlow(token, Flow{Name, Due}, now)` 修改流程；`Due=0` 表示等可信事件，非零表示同阶段内的持久期限，到期调用 `Hooks.Flow`。回调返回下一流程及期限。阶段改变会清除旧流程；流程版本、阶段、期次、激活代际一起隔离迟到事件。流程名至多 64 字节，只能在 `Warmup` 与 `Open` 阶段修改。

`activity.Component` 通过 `activity.Owner` 取得宿主的 `ActivityBook()` 和 `ActivityScope()`，不判断具体实体类型。宿主可提供 `ActivityCreatedAt()` 返回持久创建事实；只有 `Player` 实现 `SetActivityDeadline()`，把统一计算的期限接到实体定时器；Remote 宿主把期限存在 `NextDue`，由权威服务扫描。

`定义 + 范围 + 宿主 + 代际 + 期次` 是一期的身份；`Token` 还包含阶段和流程版本。宿主号是逻辑实体号，不是当前进程的 SID。

## 定义与时间

工程中的 `configs/schema/activity.go`、`configs/table/activity.csv` 是规则源。主要字段：

| 字段 | 约束 |
| --- | --- |
| ID / Version | ID 为 1..128，不回收；规则变化提升 Version |
| Scope | `player`、`world`、`alliance` |
| Trigger | `fixed` 用 StartMilli；`created` 用可信的持久创建时间；`event` 用同事务业务事件及 Eligible |
| Event / Logic | `event` 触发必须同时填事件名和已登记的逻辑；逻辑没有 `Eligible` 回调时拒绝激活 |
| DelayMillis | 非 `fixed` 触发时，从事实时间起延后多久开期；`fixed` 必须为 0 |
| WarmupMillis / OpenMillis | 预热可为 0，开启必须大于 0；各不超过 366 天 |
| Repeat / Period | `none`；`interval` 的 Period 是毫秒，且不小于预热加开启；`daily` / `weekly` 的 Period 是日数 / 周数（1～366） |
| MaxCycles / UntilMilli | 0 表示不限制；非零限制期数 / 整体结束时间 |
| Catchup | `queue` 保留每期责任；`skip_empty` 仅允许 Logic 为空的纯展示活动 |
| Disabled | 不再创建新实例；已激活实例继续执行被冻结的规则 |

日历规则冻结业务时区和日界。固定日期的日历活动，StartMilli 必须落在业务日界；创建或解锁触发的日历活动从“事实时间 + DelayMillis”之后的首个业务日界开期，恰好位于日界时使用该时刻。每周活动从首期锚点按 N 周重复；需要固定周一时，配置周一的固定锚点。日历按日期计算，跨夏令时不等于固定 24 小时。每个相邻窗口检查重叠，非法配置保留原状态并报错，不悄悄跳期。

默认首次解锁持久生效，重复事件不重置时间。`Reopen` 只允许已彻底结束的序列，必须传旧代际加 1；重复同代际请求返回现有实例。无限循环的规则不通过热更自动结束。

## 业务接入示例

例如升级到 20 级后解锁个人活动，在工程自己的文件（例如 `game/activity/activity_growth.go`；宿主经 `game/view` 的小接口拿别的组件，不 import 实体包）里登记：

```go
func init() {
    activity.Register("growth20", activity.Hooks{
        Eligible: func(owner entity.IThreadSafeEntity, event string) bool {
            p, ok := owner.(view.ProfileHolder)
            return ok && event == "profile.level_up" && p.Profile().Level() >= 20
        },
        Enter: func(c activity.Context) (bool, error) {
            // 只改本次 Nest 已锁住实体的 DAO，或登记可靠 effect。
            // 返回 true 只用于 Initializing / Settling，表示等待后台业务完成。
            return false, nil
        },
    })
}
```

规则使用 `Scope=player, Trigger=event, Event=profile.level_up, Logic=growth20`。与 `profile` 同装时，升级事实、`Eligible` 和活动激活在同一笔 `Player` 事务里；拒绝提交会一起回滚。没有 `enter` 时，`created` 触发的活动要由业务提供真实的创建时间，不能拿每次登录时间冒充。

## 异步完成与可选组合

`Hooks.Enter` 可在初始化或结算时返回 `pending=true`，同时写入被冻结的任务、名单或可靠意图。未登记 `Resume` 时保留 `pending`，等待业务自己的可靠完成入口（`Sync_ActivityComplete`）。

`Resume` 可能因超时、进程退出、属主迁移或 `Complete` 失败而重入，外部副作用必须幂等：

- 同装 `leaderboard`：分数应来自成功提交的业务事实；结算前先冻结榜单与奖单，不能每次重试重新读取可变的排名再发奖。
- 同装 `mailbox`：只有所有持久奖单都确认送达后才能 `Complete`，单封成功不代表整期成功。
- `activity.CoordinatorKey(token)` 连接框架的 `service/activity` 跨服汇聚窗口。它不替代本模块的生命周期，也不会自动部署汇聚服务。

这些是可组合的接口，不是默认的“活动榜前十发金币”。权威服务要调用排行、邮件或汇聚服务时，需要给**实际调用它的服务**配置对应的 `uses` 并登记业务钩子；只在 game 装了客户端，不代表联盟或全服进程也有这项能力。

## 归属与参与

个人活动由 `Player` 保存，两个玩家在不同时间解锁有各自的锚点。联盟活动由联盟权威实体保存；入退盟不重置联盟活动。全服实例归 `ActivityWorld`（见 `activity_world`）。

个人参加共享活动时调用 `JoinShared`：只写 `Participation`，不创建个人定时器，也不重开共享活动。输入必须来自服务端授权的查询；贡献与领奖仍要向权威宿主核对期次和成员资格。

## 恢复细节

- 定时器回调没有 error 返回值，适配层把错误以 panic 交给 Nest，由 Nest 回滚阶段、业务修改和定时器删除；不能改成吞错后返回。
- 生成器禁止给 Remote 实体挂本地持久定时器，本模块保留这条限制。全服与联盟由权威服务扫描自己拥有的实体：每秒至多一页 128 个宿主、8 个并行调用、每次调用预算 5 秒。实体多或依赖慢时扫描延迟会增长，不承诺 1 秒内全部到期。
- `queue` 保留旧期未完成的结算，后续期次按固定排期，不因结算晚而整体漂移。
- 关闭驱动时先取消再等待在途调用；超时后可以继续等待。
