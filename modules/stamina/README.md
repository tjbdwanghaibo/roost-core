# stamina：玩家体力——随业务时间惰性恢复，可消耗、可发放、可花金币补满

“惰性恢复”指服务端不用定时器逐点加体力：状态里存的是上一次结算的结果，每次读写时按“距上次结算过了多久”现算。

## 能力

- **客户端协议**
  - `StaminaRefill`（`protocol/def/stamina_refill.go`）：请求 `StaminaRefillRequest{}` 没有字段，应答 `StaminaRefillResponse{Code, Reason, Value, Cost, Balance, RefillCount}`。花金币把体力补到上限；价格由服务端按当日第几次购买决定，客户端不传价格与时间。
  - `StaminaRules`（`protocol/def/stamina_rules.go`）：只有推送形状 `StaminaRulesPush{Max, HardCap, RegenMillis, RefillDailyLimit}`，不单独发送，作为 rules 模块规则快照里的一项下发。
  - 没有“消耗体力”的客户端协议：消耗由服务端别的功能在自己的事务里调。
- **服务端入口**（组件 `StaminaComponent`，`game/components/stamina/stamina.go`，经 `IStaminaEntity` 的 `StaminaComp()` 取得）
  - `Current(nowUnixMilli int64) int64`：这一刻的体力，只读，不写任何字段。
  - `Spend(n, nowUnixMilli int64) (int64, error)`：扣 `n` 点，返回扣完的体力。不够就拒绝，不做“有多少扣多少”。
  - `Grant(n, nowUnixMilli int64) (StaminaGrantResult, error)`：发 `n` 点，可以超过上限直到硬顶，超出硬顶的部分截掉；`StaminaGrantResult{Granted, Value}` 里的 `Granted` 是实际加上的点数。
  - `Refill(nowUnixMilli int64) (StaminaRefillResult, error)`：`StaminaRefill` 协议用的那一个，扣金币并补满。
  - `nowUnixMilli` 一律传所在 Nest handler 最后一个参数（这条消息冻结的业务时间），不传 `time.Now()`，也不传客户端给的时间。
  - Nest handler `handlerStaminaGrant`（`game/handler/stamina_grant.go`）：服务端发放，没有协议与端点；不在这名玩家事务里的调用方用 `sender.NewStaminaGrantSender(client).Sync_StaminaGrant(ctx, playerEntityID, amount)`。
  - Nest handler `handlerStaminaRefill`（`game/handler/stamina_refill.go`）：协议对应的 handler。
- **登记点**（`game/components/stamina/hooks.go`）
  - `OnStaminaSpent(name, hook)`，`hook` 是 `StaminaSpentHook func(owner view.PlayerEntity, amount, nowUnixMilli int64) error`。每次成功的 `Spend` 执行一次；在 Player 锁内、同一笔 Nest 事务里，只改同一个 Player，不做 I/O。只在 `init` 登记；空名、nil、重名 panic。`Grant` 与 `Refill` 不触发它。
- **状态**（`db/def/stamina.go`）：`PlayerDao.Stamina`（`StaminaState{Value, SettledAt, RefillDay, RefillCount}`），落库并同步，玩家本人可见。从没有过体力写入的玩家是零值，按公式就是满体力，不用初始化。
- **配置**：没有配置表。规则写在留给工程的 `game/components/stamina/rules.go`：`staminaRules()` 返回 `StaminaRules{Max, HardCap, RegenMillis, RefillDailyLimit}`，缺省上限 100、硬顶 999、每 5 分钟恢复 1 点、每业务日能买 3 次补满；`staminaRefillPrice(nth)` 返回当日第 `nth` 次补满的金币价格，缺省 50、100、150。
- **错误码**（`internal/errors/stamina.go`）：`stamina_invalid`（点数不为正）、`stamina_short`（体力不够）、`stamina_full`（已满，不能买补满）、`stamina_refill_limit`（今天的购买次数用完）。补满时金币不够透出货币模块的 `wallet_short`。

**日志**：付费补满确认，记录花费、次数、体力和余额；Spend 不记录。服务端 Grant 的实际截断数量仍由调用方记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/stamina/stamina_refill.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不提供消耗体力的客户端协议；不判重——`Spend` / `Grant` 调一次算一次，`handlerStaminaGrant` 重发会多发；没有多种体力（只有一条）；不发“体力满了”的通知。
- **同步只在写入时发生**：自然恢复不产生任何推送，客户端不能直接显示同步下来的 `Value`，要按 `StaminaState` 注释里的公式、用上限与恢复周期、加上服务器业务时间自己推算。上限与恢复周期由 rules 在进入时推送或 `RulesGet` 补取，不随每次状态变化重复下发。
- **数量与频率**：缺省每业务日至多 3 次补满（`stamina_rules.go` 的 `RefillDailyLimit`）；发放的硬顶缺省 999（`HardCap`）；`RefillDailyLimit` 不为正表示不开放购买；`RegenMillis` 不为正表示不自然恢复（这时新玩家的体力是 0 而不是满）。
- **截断是静默的**：`Grant` 超过硬顶的部分直接丢掉、不报错，调用方不看返回的 `Granted` 就是少发。
- **一致性**：体力在 Player 的 DAO 上，随所在 Nest 事务提交或回滚。`Refill` 的扣金币（`WalletComp().Add`，不留回执）与补体力在同一笔事务里。拒绝时组件自己不写任何字段。`Spend` 先写体力再执行钩子，钩子出错必须把错误交还 Nest。
- **补满不带请求键**：紧接着的重发不会重复扣钱（第一次成功后体力已满，第二次得到 `stamina_full`）；两次之间又消耗过体力的重发会被当成新的一次购买。
- **每日次数是惰性清零的**：跨业务日后没有新的购买时，`RefillCount` 仍是旧的一天的数，客户端要拿 `RefillDay` 与当前业务日（servertime 下发的 `Day`）比对。
- **时钟回拨**：业务时间往回拨时不恢复也不倒扣，要等业务时间重新走过 `SettledAt` 才继续恢复；业务日回拨时购买次数不清零。
- **改规则立刻对所有人生效**：规则在每次读写时取一次，不需要迁移存档。调高上限等于给所有已满的玩家立即补到新上限；调低上限时超出的体力不扣，只是不再恢复。
- **前提**：依赖 wallet、rules（rules 再依赖 enter）与 nest Mod；实体名 `Player`、`-template game` 的目录形状。只在持有这名 Player 的进程内有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“stamina 的边界”一条（同步时机、静默截断、调高上限、补满不带键、时钟回拨、价格为 0 的免费补满没有场景覆盖）；“模块的规则大多还是 Go 写的业务文件”一条。

## 复用方式

- **需求正好是它**：`roost module add stamina`，连带装上 wallet、rules、enter。
- **需求多一点**：
  - 改数值、接配置表、让上限随等级成长：只改 `game/components/stamina/rules.go` 里两个函数的函数体（文件头注释有接配置表与读 profile 等级的写法）。客户端要用同一份上限与恢复周期。
  - 消耗体力时顺带做事（任务计数等）：在自己的文件里 `init` 登记 `OnStaminaSpent`。
  - 不要改 `game/components/stamina/stamina.go` 里的 `staminaSettle` 与 `write`：客户端推算公式与它是同一份；也不要改 `StaminaState` 的 store 键与错误码码值。
- **别的功能要扣 / 发体力**：
  - 在这名玩家的 handler 里：目标接口嵌入 `player.IStaminaEntity`，先做完自己的检查，再 `_, err := target.StaminaComp().Spend(cost, nowUnixMilli)`，再写自己的状态；任何一步返回错误整笔回滚。
  - 发体力且有“发过没有”的事实要记（邮件附件、订单、任务奖励）：在自己的 handler 里先判自己的状态，再调 `target.StaminaComp().Grant(n, nowUnixMilli)`，并处理 `Granted` 小于 `n` 的情况。
  - 不在这名玩家的事务里、且不需要判重：`sender.NewStaminaGrantSender(client).Sync_StaminaGrant(ctx, playerEntityID, amount)`。
- **不适合直接用的情形**：多种体力 / 行动点各自恢复、需要“体力回满”定时提醒、需要客户端直接扣体力。值得借鉴的是惰性结算的写法：`staminaSettle` 只算不写，起点只前进整周期、零头留着，满了就把时钟停下；以及“先比较差值再相加”避免溢出的做法。

## 联动

- `stamina/with/rules`：登记名 `stamina`，在 Player 锁内把 `staminaRules()` 的结果编码成 `StaminaRulesPush` 放进规则快照；只读，不结算也不写状态。2 条场景。
- `task/with/stamina`：登记 `OnStaminaSpent("task", …)`，每次成功消耗给任务事件 `stamina.spent` 记“实际消耗的点数”，与消耗同一笔事务。3 条场景。
- 对 wallet 的使用（依赖，不是 `with/`）：`Refill` 的扣款走 `WalletComp().Add`，所以会触发 wallet 的扣款钩子——装了 task 时，一次付费补满会给 `wallet.spent` 记 1 次。
- 本模块没有别的 `with/`。模块库里目前没有别的模块调用 `StaminaComp().Spend`（副本等玩法扣体力要工程自己接）。

## 验证范围

- `roost module verify stamina`：26 条场景（`^TestStamina`，包 `./game/handler` 20 条、`./game/components/stamina` 6 条）。覆盖：新玩家满体力、消耗与时钟起点、周期边界恰好恢复 1 点、零头保留、到上限停表、拒绝零写入、时钟回拨、隔很多年不溢出、发放超上限与硬顶截断、补满扣费与逐次涨价、免费的一次补满（价格为 0：补满、记次数、不调货币组件）、每日次数与跨业务日清零、业务日回拨、提交被拒时体力与金币一起回滚、状态被同步。
- rules 联动另有 2 条（`^TestWithStaminaRules`）。
- 场景用真实的 Nest 引擎与生成的 DAO / Sender，提交器是不落盘的替身，时钟可控。没有经过真实 TCP、Mongo、WAL；价格为 0 的免费补满没有场景覆盖。
