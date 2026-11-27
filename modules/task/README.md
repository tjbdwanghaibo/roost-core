# task：事件驱动的任务系统——每日 / 每周 / 永久、前置链、配置表、服务端推进、事务内发奖

任务的进度只由服务端推进：别的功能在事实第一次生效时上报一个“事件”，task 把它记到与事实匹配、且已经解锁的任务上。普通事件按名字累计次数；商店事实还可按商品号累计购买份数、按道具号累计实际发货量。可选的持有任务不累计，在领奖时核对当前背包。客户端只能领奖。

## 能力

- **客户端协议**（`protocol/def/task_claim.go`）
  - `TaskClaim`：请求 `TaskClaimRequest{TaskID}`，应答 `TaskClaimResponse{Code, Reason, TaskID, Reward, Balance, RewardKind, RewardID}`。领取一个已完成任务的奖励：每日 / 每周任务在周期内一次，永久任务终身一次。`Reward` 是到账数量，`RewardKind` / `RewardID` 说明是哪种资产，`Balance` 是领完之后的金币余额（非金币奖励不改变它）。目标取认证会话的玩家。
  - 没有推进任务的客户端协议，也没有推送：进度经 `PlayerDao.Task` 的自身视图同步下去。
- **服务端入口**（组件 `task.Component`：逻辑、类型号 `task.CompTypeTask` 与工厂登记在 `game/components/task/task.go`，对外接口在 `game/view/component_task.go`；Nest handler 的参数写生成的 `view.ITaskEntity`，经 `Task()` 取得）
  - `Report(event string, n, nowUnixMilli int64) error`：按事件推进。所有不限定对象、`Event` 等于 `event`、并且当时已解锁的任务各加 `n`，封顶在目标；没有匹配的任务时什么都不写。**事务内联动用这个**。
  - `Advance(taskID, n, nowUnixMilli int64) (int64, error)`：直接按任务号推进，返回推进后的进度。任务未解锁，或任务限定了对象而调用中没有对象事实，答 `task_incomplete`，拒绝前不写。
  - `Claim(taskID, nowUnixMilli int64) (TaskClaimResult, error)`：`TaskClaim` 协议用的那一个，发奖并记下“领过了”。
  - `nowUnixMilli` 一律是所在 Nest 消息冻结的业务时间，不采信客户端时间。
  - Nest handler（都在 `game/handler/`，`rollback=undo`（持久级别取缺省 async））：
    - `handlerTaskClaim`（`task_claim.go`）：协议对应的 handler。
    - `handlerTaskReport`（`task_report.go`）：只给服务端用，不在这名玩家事务里的调用方发 `sender.NewTaskReportSender(client).Sync_TaskReport(ctx, playerEntityID, event, n)`。
    - `handlerTaskAdvance`（`task_advance.go`）：同上，按任务号：`sender.NewTaskAdvanceSender(client).Sync_TaskAdvance(ctx, playerEntityID, taskID, n)`。
- **登记点**
  - 奖励种类（`game/components/task/rewards.go`）：`RegisterTaskReward(kind string, handler TaskRewardHandler)`，`TaskRewardHandler{Validate, Grant}`。`Validate` 在建任务目录时校验一行奖励是否合法，`Grant` 在领奖的同一笔事务、Player 锁内发放，只改同一个 Player、不做 I/O。只在 `init` 登记；不完整或重名 panic。内置 `coins`（`RewardID` 必须为 0，经 `Wallet().Add` 发放）；装了 bag 时多一个 `item`（`RewardID` 是道具号）。
  - 匿名事件没有登记表：事件名就是任务表 `event` 列里的字符串，任何功能调 `Report("自己的事件名", n, now)` 即可。
- **状态**（`db/def/task.go`）：`PlayerDao.Task`（`TaskState{Day, Progress, Claimed, Weekly, Permanent}`，后两个是 `TaskPeriodState{Day, Progress, Claimed}`），落库并同步，玩家本人可见。`Day` 是每日记录所属的业务日（YYYYMMDD），`Weekly.Day` 是每周记录所属那一周的周一（YYYYMMDD），`Permanent.Day` 恒为 0。
- **配置**
  - 配置表 `task`：结构在 `configs/schema/task.go`（`Task{ID, Event, Period, Target, Previous, RewardKind, RewardID, RewardCount, Disabled, ObjectKind, ObjectID, Mode}`），数据在 `configs/table/task.csv`。`Period` 取 `daily` / `weekly` / `permanent`；`Target` 是目标次数；`Previous` 是前置任务号（0 表示没有）；`Disabled` 是停用。
  - `mode` 可缺列 / 留空，表示累计任务；唯一非空模式是 `held`（见下节）。`object_kind` / `object_id` 两列可省：空种类、0 号表示原来的匿名行。限定对象须指定桥支持的种类和正 ID；种类拼错、缺一半、或对应桥未安装的行不可推进 / 领取，不降级成匿名任务。
  - 缺省三行每日任务：1 号签到 1 次（`checkin.claimed`）奖 20 金币；2 号消费 3 次（`wallet.spent`）奖 30 金币；3 号副本领奖 10 次（`dungeon.claimed`）奖 100 金币。
  - 任务目录与校验在 `game/components/task/catalog.go`：常量 `TaskMaxID = 4096`，`TaskPeriodDaily` / `TaskPeriodWeekly` / `TaskPeriodPermanent`。
- **错误码**（`internal/errors/task.go`）：`task_unknown`（任务号不存在、停用或这一行不合法）、`task_incomplete`（本周期没做完、未解锁、或周期已过）、`task_claimed`（本周期已领过）、`task_count_invalid`（推进次数不为正）。发奖被拒时透出奖励方的码（如 `wallet_overflow`、背包满）。

### 购物任务：三种行各用一个口径

| `event` | `object_kind` | `object_id` 示例 | 一次真实购买对匹配行的作用 |
|---|---|---:|---|
| `shop.bought` | 空 | 0 | 无论商品，增加 `ShopBought.Count` 份（旧行为） |
| `shop.bought` | `goods` | 42 | 仅 `GoodsID == 42`，增加 `Count` 份 |
| `shop.bought` | `item` | 1001 | 仅 `ItemID == 1001`，增加 `Gained` 个；不同商品可发同一道具 |

例如一份含 4 个道具、一次买 3 份：匿名行和匹配商品行各加 3，匹配道具行加 12。一行只有一种匹配方式，不会既按匿名又按对象计数。对象号是计数筛选条件，不是回执键；对同一对象的两笔新购买分别计入。只接首次生效的 `ShopBought`；拒绝、回执重放不计，提交失败一起回滚。`item` 仅代表**从商店获得**，不是所有背包入账，也不按 `Held`（购买后库存快照）累加。

### 持有任务：领奖时仍须持有，不消耗道具

装了 `bag` 就能用，不要求 `shop`。例如配置 `mode=held, event=bag.held, object_kind=item, object_id=1001, target=3`：玩家领奖时，任务在同一个 Player 的 Nest 事务里读取 `Bag.Count(1001)`；当前至少有 3 个才发奖。购买、邮件或 GM 发放等任意来源的现存道具都算，不看购物回执。拥有 3 个后丢掉 1 个不能领，补回 1 个即可领，本周期不能再领。不记录曾达标，也不把 `ShopBought.Held` 写入任务。`event` 在这个模式中是配置标记，不是可上报的库存事件。

持有行**不进入事件索引、不写 `Progress`**；`Report`、`Advance` 和客户端数字都不能代替当前背包。缺 bag 桥、错误的 event/kind、非正道具号、`bag.ItemRule` 不认可的道具，或目标不在 `1..bag.MaxStack` 的行不可用。仅热更道具表也会重新判定：框架重载整个请求快照，道具下架时该行不可用，重新上架可重新判定。单个道具的入账上限若后来调低，不会抹掉已有合法库存，领奖仍按实际 `Count`。

**显示**：客户端有自身背包同步视图时，读对应 `PlayerDao.Bag.Items[object_id]`（无条目按 0），可显示 `min(当前量, target) / target`；“已领”仍读相应周期的 `Claimed`，解锁仍看前置的本周期领奖事实。当前库存不是周期计数；跨日不能把它显示成 0。没有背包视图时不要从任务进度猜库存，待收到权威状态或领奖应答再更新。没有新增查询协议或查询写路径。

**跨期**：首次领取不需要先有本周期的任务事件。库存足够且前置已解锁时可直接领本周期奖励（不是补领上期）；成功才更新周期并写 `Claimed`。不足、奖励被拒或提交失败不清旧账、不记已领，之后可以重试。时钟早于存档所属周期时不回开持有任务。

### 任务规则（属于“能力”，逐条对应代码）

- **三种周期**
  - `daily`：按 `clock.BusinessDay`（部署配置的业务时区与每日重置时刻）切分。
  - `weekly`：先取业务日，再算这一天所在周的周一；周一的业务日起点换周。不是“毫秒除以一周”，也不是 YYYYMMDD 除以 7。
  - `permanent`：永不清零。
- **清零是惰性的**：周期换了之后，要等这个周期里第一次**有效推进**或第一次**成功领取当前持有任务**，才把旧的进度与领奖记录清掉。领奖被拒不会顺便清零；过期没领的每日 / 每周任务不能补领（答 `task_incomplete`）。时钟回拨不清零，也不会让已领的任务重新可领。
- **前置链**：`Previous` 指向的任务必须在它自己的当前周期里**已经领奖**，本任务才解锁。前置只能是同周期的任务或永久任务。成环、前置不存在、前置停用或不合法时，这个任务整体不可用（`Advance` / `Claim` 答 `task_unknown`，`Report` 跳过它）。
- **事件不追溯、不沿链重复消费**：一次 `Report` 只推进当时已解锁的任务；解锁之前发生的事件不会补记到后续任务上，同一个事件也不会在同一次上报里既完成前置又推进后继。
- **奖励按领取时的配置读**：不在完成时冻结奖励。改目标会影响还没领的任务；已领的不会重开。
- **不合法的行不可用**：ID 不在 1～4096、目标不为正、事件名为空、周期不认识、奖励种类没登记（例如没装 bag 却配了 `item`）或奖励校验不过的行，既不能推进也不能领。
- **永久任务的领奖事实不丢**：永久任务停用或从表里移除，记录保留；同一个 ID 重新出现不会再发一次奖。

**日志**：领奖确认，记录任务、奖励种类 / 道具 / 数量和余额；Report 不记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/task.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：客户端不能推进任务；不自动发奖（要客户端发 `TaskClaim`）；没有任务接取 / 放弃、没有按等级或条件的可见性、没有多目标任务（一行一个事件一个目标）、没有积分制活跃度宝箱、没有任务完成的推送；不为事件做去重。
- **数量上限**：任务 ID 只接受 1～4096（`TaskMaxID`），所以每个周期的进度与领奖记录各自至多 4096 条，表反复增删也有上界。进度封顶在目标值。
- **幂等的范围**：`Report` / `Advance` 调一次算一次。事务内联动由事件提供方保证“只在事实第一次生效时上报”（例如钱包回执重放、商店回执重放不会再报）。经 Sender 发的独立消息（`Sync_TaskReport` / `Sync_TaskAdvance`）不带去重，消息重发会多记。`Claim` 靠领奖事实防重：同一周期第二次答 `task_claimed`。
- **一致性**：进度、领奖事实与奖励都在 Player 的 DAO 上。事务内联动与原业务在同一笔 Nest 事务里提交或回滚；`Claim` 的发奖与“领过了”在同一笔事务里，奖励方拒绝时什么都不改。排行榜联动是例外，见“联动”。
- **客户端要自己判过期**：跨日 / 跨周后没有新的推进时，同步下来的仍是旧周期的记录；客户端拿 `Day` / `Weekly.Day` 与当前业务日比对（装了 servertime 时用它下发的 `Day`），上期累计进度与领奖标记按过期处理；持有任务的库存量另读背包，见上节。
- **发布后固定的东西**：任务 ID、周期、前置关系和事件对象语义上线后不要改（换对象应新建任务号，否则会沿用旧进度），不要复用已发布的 ID；停用用 `Disabled`。
- **前提**：依赖 wallet、nest Mod 与配置表功能；实体名 `Player`、`-template game` 的目录形状。所有入口只在持有这名 Player 的进程内有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“task 的边界”“事务内事件”“模块登记点”“惰性清零对客户端可见”四条。

## 复用方式

- **需求正好是它**：`roost module add task`，连带装上 wallet 与“消费计数”联动。再装 checkin、shop、stamina、profile、dungeon、bag、leaderboard 中的任意一个，对应的联动文件自动写入。
- **需求多一点**
  - 加任务、改目标与奖励、整张换掉：只改 `configs/table/task.csv`，再运行 `roost generate`；运行期沿用 configdata 的请求快照与热更。模块自带的场景（含联动场景）用自己带的样例表（`game/handler/task_test.go` 的 `taskSampleRows`，与缺省的三行相同），写到临时目录装载，换表不用改它们；只有 `TestTaskCatalogEntriesAreUsable` 检查工程自己的表：每一行没停用的任务都必须可用（ID、事件、周期、奖励合法，前置存在且可用），停用行跳过。代码里没有指向表中某一行的常量。
  - 让自己的功能驱动任务：在自己的、已经锁住 Player 的 Nest handler 里，事实第一次生效之后调：

    ```go
    if err := target.Task().Report("battle.won", 1, nowUnixMilli); err != nil {
        return err
    }
    ```

    然后在任务表里加一行 `event` 为 `battle.won` 的任务。错误必须原样返回给 Nest，不能吞掉后提交半笔业务。
  - 自己的功能有“首次生效”的实体事件、又不想直接依赖 task：照 `with/shop` 的写法，在 task 的组件包里放一个单独的联动文件，组件上写 `//roost:listen` 的 `On<事件名>(ev event.<事件名>) error`，在里面调 `c.Report`。
  - 加一种奖励资产：`init` 里 `RegisterTaskReward("<种类>", TaskRewardHandler{Validate: …, Grant: …})`，照 `with/bag` 的 `task_bag.go`。
  - 不要改 `game/components/task/task.go` 里周期与解锁的判断、`TaskState` 的 store 键、错误码码值；不要给业务代码留“测试用替换任务表”的开关（场景经真实的配置装载换表）。
- **别的模块要用它的数据或动作**
  - 同一个 Player 的事务里：目标参数写 `view.ITaskEntity`（同一笔里还要用别的组件时写 `view.PlayerEntity`），调 `Task().Report(event, n, nowUnixMilli)`。
  - 不在这名玩家的事务里（房间结算给每名玩家记一场、GM）：`sender.NewTaskReportSender(client).Sync_TaskReport(ctx, playerEntityID, event, n)`；自己保证只发一次。
  - 读进度：读 `PlayerDao.Task`，并按上面的规则判周期。
- **不适合直接用的情形**：成就点数与多级成就、需要接取的支线、多条件组合目标、全服进度任务、要求恰好一次的跨服务事件。值得借鉴的是：周期记录用“所属周期戳 + 惰性清零”而不用定时器；任务目录随不可变配置表建一次索引（`taskDefinitions`）并在建索引时判环；“事件提供方负责首次上报，任务方不判重”的职责划分。

## 联动

task 自己的 `with/`（八组，都是“另一个模块也装着时才有”的文件）：

| 联动 | 做什么 | 事件与次数 | 重放 | 场景 |
| --- | --- | --- | --- | --- |
| `with/wallet` | 组件上 `//roost:listen` 的 `OnWalletSpent`（实体事件 `event.WalletSpent`） | `wallet.spent`，每笔成功扣款 1 次 | `Apply` 命中回执不计；发放不计 | 6 |
| `with/checkin` | 组件上 `//roost:listen` 的 `OnCheckinClaimed`（实体事件 `event.CheckinClaimed`） | `checkin.claimed`，每次成功签到 1 次 | 当日重复签到被拒，不计 | 3 |
| `with/shop` | 组件上 `//roost:listen` 的 `OnShopBought`（实体事件 `event.ShopBought`） | `shop.bought`：匿名或指定商品按份数；指定道具按实际发货量 | 购买回执重放不计；扣款另计一次 `wallet.spent` | 3 |
| `with/stamina` | 组件上 `//roost:listen` 的 `OnStaminaSpent`（实体事件 `event.StaminaSpent`） | `stamina.spent`，实际消耗点数 | 每次成功 `Spend` 都计 | 3 |
| `with/profile` | 组件上 `//roost:listen priority=10` 的 `OnProfileLevelUp`（实体事件 `event.ProfileLevelUp`） | `profile.level_up`，提升的等级数（`to-from`） | 连升多级按差值 | 2 |
| `with/dungeon` | 组件上 `//roost:listen` 的 `OnDungeonClaimed`（实体事件 `event.DungeonClaimed`） | `dungeon.claimed`，首次副本领奖 1 次 | 已领过的副本不再计 | 2 |
| `with/bag` | 登记 `item` 奖励及 `held` 的权威库存读取 | 不产生事件；库存判定、奖励与领奖同一笔事务 | 拒绝、跨期、回滚及奖励回调均有生成工程用例 | 3 |
| `with/leaderboard` | 登记 `leaderboard.OnSubmitted`（事务之外，不是实体事件，保留登记方式） | `leaderboard.submitted`，rank 成功答复 1 次 | **同分重放也计数** | 3 |

- 前七组都在同一笔 Player 事务里，提交失败一起回滚。除 profile 的升级事件有多个监听方（task 用 `priority=10` 排在活动、属性、升级邮件之后）外，其余事件 task 是唯一的模块监听方，用缺省优先级。
- **排行榜联动不在事务里**：排行提交发生在 Player 事务之外，钩子经独立的 `Sync_TaskReport` 尽力推进；任务推进失败不撤销排行提交，只由 leaderboard 的钩子执行器记一条 Warn。它只适合“提交过榜单”这类允许重复计数的目标，不能用来发恰好一次的结算奖励。任务表里没有未停用的 `leaderboard.submitted` 任务时，钩子直接返回，不发 Nest 请求（缺省表就没有）。
- 别的模块没有 `with/task` 目录。场景模块的实体事件 `event.SceneRegion`（进入 / 离开区域）注释里建议用它向 task 上报，但模块库里没有现成的监听。

## 验证范围

- `roost module verify task`：29 条场景（`^TestTask`，包 `./game/handler` 26 条、`./game/components/task` 3 条）。覆盖：推进累加与封顶、已完成 / 已领不再写、拒绝零写入、领奖入账、同日二次领取、未完成 / 未知任务、货币拒绝时不领、新一天先清再记、昨天完成今天不能领、提交被拒整体回滚、时钟回拨、业务日随配置、记录有界、同步；事件只推进匹配的任务、周任务只在业务周一换周、周回拨不重开、永久任务跨天与下架后仍记得领过、链要等前置领奖且不复用事件、坏链零写入、周期重置与奖励回滚、ID 上界经得起表反复增删、应答写明资产、未登记的奖励不产生进度；目录可用性与配置装载校验。
- 八组联动共 25 条（见上表）。
- 全部是离线场景：真实的 Nest 引擎、生成的 DAO / Sender、真实的配置装载；提交器是不落盘的替身，排行榜联动里的 Rank 是替身。没有经过真实 TCP、WAL、Mongo、NATS 或跨进程。
- 场景装载的是自带的样例任务表，不读工程的 `configs/table/task.csv`；`with/bag`、`with/shop` 的场景还自带用到的两件道具（1001、1002）与两件商品（1、1001 号，每份发货量不同），盖在工程表的同号行上，工程别的表原样带上（同时装着的别的模块的钩子照常读得到）。工程改任务表、道具表、商品表不影响这些场景；工程表本身由 `roost generate` 时的表校验、`TestTaskCatalogEntriesAreUsable`（任务）与 bag 的 `TestBagAcceptsTheItemsOfTheProjectItemTable`（道具）检查。
- 仓库侧的组合验收入口是 `codegen/internal/roost` 里的 `TestModuleTaskEventsPeriodsAndMutations`。
