# wallet：玩家的一种货币（金币）——余额、发放、扣除与客户端消费协议

## 能力

- **客户端协议**（`protocol/def/wallet_spend.go`）
  - `WalletSpend`：请求 `WalletSpendRequest{Key, Amount}`，应答 `WalletSpendResponse{Code, Reason, Balance}`。从发请求的玩家自己的余额里扣 `Amount`（必须为正），`Key` 是客户端生成的请求键，同一个键重发不会扣第二次。目标玩家取自认证会话，请求里不带玩家号。
  - 没有推送协议。余额变化经玩家的自身视图同步下去（见“状态”）。
- **服务端入口**
  - 组件是共用包 `game/components/wallet` 的 `wallet.Component`（宿主接口 `wallet.Owner`，见“复用方式”；类型号 `wallet.CompTypeWallet` 与工厂登记也在那里），对外接口是 `game/view/component_wallet.go` 的 `view.Wallet`，在已经锁住 Player 的 Nest handler 里经 `Wallet()` 取得：
    - `Add(delta int64) (int64, error)`：余额加 `delta`（负数为扣），返回新余额。不留回执、不判重，给“自己记着这一次做过没有”的调用方用（签到、任务奖励、商店扣钱都走它）。
    - `Apply(key string, delta int64) (int64, error)`：带业务键的加减。同一个 `key` 在去重窗口内再来，不再改余额，返回第一次那一刻的余额；同一个 `key` 换了 `delta` 答 `wallet_conflict`。
    - `Balance() int64`：当前余额，只读。
  - 窄实体接口 `view.IWalletEntity`（生成）：别的功能的 Nest handler 把目标参数声明成它，就能在同一笔事务里调 `Wallet()`；同一笔里还要用别的组件时参数写实体总接口 `view.PlayerEntity`。
  - 宿主方法 `WalletState() *db.WalletState`、`WalletLedger() *db.WalletLedger`（`view.WalletHolder`）：Player 的由生成器写（`PlayerDao.Wallet`、`PlayerDao.WalletLedger` 是唯一对应的字段，纯 DAO 交接），实体包里没有手写文件。
  - Nest handler `handlerWalletGrant`（`game/handler/wallet_grant.go`）：服务端发放，不对客户端开放。不在这名玩家的事务里的调用方经生成的 Sender 调：`sender.NewWalletGrantSender(client).Sync_WalletGrant(ctx, playerEntityID, key, amount)`，`amount` 必须为正，内部走 `Apply`。
  - Nest handler `handlerWalletSpend`（`game/handler/wallet_spend.go`）：`WalletSpend` 协议对应的 handler，内部是 `Apply(request.Key, -request.Amount)`。
- **实体事件**（`game/event/wallet.go`）
  - `event.WalletSpent{Amount, NowUnixMilli}`：每一次成功的扣款（`Add` 的负数、`Apply` 第一次生效的负数）在宿主上发布一次，`Amount` 是扣掉的正数，`NowUnixMilli` 是这条 Nest 消息冻结的业务时间；`Apply` 命中回执的重放不发布，发放不发布。
  - 监听写成组件或实体方法上的 `//roost:listen [priority=N]`（签名 `func (c *Component) OnWalletSpent(ev event.WalletSpent) error`），由生成器登记到挂这个组件的实体上；在宿主锁内、与扣款同一笔 Nest 事务里执行，只能改同一个实体，不能做 I/O。
- **状态**（`db/def/wallet.go`）
  - `PlayerDao.Wallet`（`WalletState{Coins}`）：落库并同步，玩家本人可见。
  - `PlayerDao.WalletLedger`（`WalletLedger{Receipts, NextSeq}`）：`Apply` 的去重窗口，只落库、不下发。
- **配置**：没有配置表与配置键。两个常量写在共用组件里：`wallet.ReceiptWindow = 128`（去重窗口大小）、`wallet.KeyMaxLen = 64`（业务键最大字节数）。
- **错误码**（`internal/errors/wallet.go`）：`wallet_short`（余额不足）、`wallet_invalid`（键不合规或金额为 0 / 不为正）、`wallet_conflict`（同一个键换了金额）、`wallet_overflow`（余额超过 int64 上限）。

**日志**：客户端消费与 GM 调整确认，记录请求键 / delta、确认余额。服务端直接用 WalletGrant Sender 的调用方负责记原因。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/wallet.go`, `game/modules/gm/wallet.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：只有一种货币（`Coins`），没有多货币；不做充值到账（另装 purchase）；不做玩家之间转账；不记流水——`WalletLedger` 只是防重发的窗口，不能当对账账本查历史。
- **上限**：余额是非负的 int64；业务键 1～64 字节，只收 ASCII 字母、数字与 `-` `_` `:`，别的键在写入之前答 `wallet_invalid`；去重窗口保留最近 128 次带键的加减。
- **幂等的范围**：`Apply` 只在窗口内去重。一个键之后又发生了 128 次带键的加减，它的回执被淘汰，再用它会当成新的一次执行。要长期“只发一次”的发放（签到、任务奖励、订单到账），由调用方在自己的状态里记下事实并改用 `Add`。`Add` 完全不判重，调一次改一次。
- **一致性**：余额与回执都写在 Player 的 DAO 上，随所在的 Nest 事务一起提交或回滚；三个自带 handler 都是 `rollback=undo durability=strict`。余额不足、溢出、键不合规时组件自己不写任何字段。
- **监听失败的后果**：`Add` 先写余额、再发布 `event.WalletSpent`。监听返回错误时余额已经在本笔事务里改过，调用方必须把错误原样返回给 Nest 让整笔回滚，不能吞掉错误改走别的分支。
- **重放的应答是当时的余额**：`Apply` 命中回执时返回的是第一次生效那一刻的余额，不是现在的余额；当前余额以同步下来的 `Wallet` 为准。
- **宿主**：组件本身不认具体实体，任何在自己 DAO 里放 `WalletState` / `WalletLedger` 的实体都能挂（见“复用方式”）。模块装好时只接在 `Player` 上：协议、两个自带 handler 与各联动都是 Player 的；挂到别的实体时，`event.WalletSpent` 同样在那个实体上发布，但只有登记到那种实体上的监听（组件监听随组件挂过去，实体监听要自己写）会执行，入口由接线的人写。
- **跨实体**：组件的每个方法只改所在宿主自己的 DAO。一笔操作同时动两个宿主（例如玩家捐金币给公会金库）照多实体 handler（两边在同一个进程、同一笔 Nest 事务）、Remote 实体或 Saga（跨进程、带补偿）写，组件不替你协调。
- **前提**：工程是 `-template game` 的目录形状，模块装在名为 `Player` 的实体上，装有 nest Mod。所有入口只在持有宿主实体的进程内有效，没有跨进程的货币操作。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“wallet 的业务键”“wallet 的去重窗口不是永久账本”“模块安装绑定 Player 与工程形状”三条。

## 复用方式

- **需求正好是它**：`roost module add wallet`，没有依赖。checkin、task、stamina、shop、gacha、idle、mailbox、purchase 依赖它，装那些模块时会一起装上。
- **需求多一点**：模块没有单独留给工程改的规则文件，装上的源码归工程所有。要在扣款时顺带做事，在自己的组件（或 Player 实体）上写一个 `//roost:listen` 的 `OnWalletSpent(ev event.WalletSpent) error`。不要改的是 `WalletState` / `WalletLedger` 的 store 键与错误码的码值（发布后固定）。
- **别的功能要动金币**：
  - 已经在这名玩家的 handler 里：目标参数写 `view.IWalletEntity`（同一笔里还要用别的组件时写 `view.PlayerEntity`），然后 `balance, err := target.Wallet().Add(-cost)`；`err` 原样返回。
  - 客户端带请求键、自己没有别的判重手段：`target.Wallet().Apply(key, delta)`。
  - 不在这名玩家的事务里（别的实体的结算、后台任务）：`sender.NewWalletGrantSender(client).Sync_WalletGrant(ctx, playerEntityID, key, amount)`。
- **挂到别的实体上**（公会金库、活动奖池等；不新增模块，也不用写接线文件）：
  1. 在那个实体的 DAO（`db/def/<实体>.go`）里加两个字段，store 键自己起、发布后不改：`Wallet WalletState \`store:"wallet" dao:"persist,sync"\`` 与 `WalletLedger WalletLedger \`store:"wallet_ledger" dao:"nosync"\``（回执是服务端账本，不下发）。同步与否按那个实体给谁看决定。
  2. 实体结构体里加组件字段 `wallet *wallet.Component \`comp:"wallet.CompTypeWallet"\``（同一个组件同一个类型号），`roost generate` 生成 `Wallet()` 访问器和 `WalletState()` / `WalletLedger()`（DAO 里各只有一个对应字段时；有多个时生成器在编译期断言上方列出，在实体包里手写）。要在这个实体上扣款时做事，在实体上写 `//roost:listen` 的 `OnWalletSpent(ev event.WalletSpent) error`（同一笔事务，返回错误整笔回滚）。
  3. `view.IWalletEntity` 的 handler 对这个实体也适用；挂了钱包的实体分属不同服务时，handler 要写 `target=<实体>`。
  - 本模块的场景 `TestWalletSecondHost*` 就是这样把同一个组件挂到测试里的第二个实体上，验证加减、拒绝、回滚与两个宿主互不影响。
- **不适合直接用的情形**：多种货币、玩家间交易、需要永久幂等或可查询流水的账本。值得借鉴的是三点：组件不保存事务会改的状态（全在 DAO，回滚只有一处）；“键 → 结果”的有界回执窗口（按 `Seq` 淘汰最旧的）；扣款与溢出判断不用取负、不用先加后比的写法（`Add` 里的注释）。

## 联动

- `wallet/with/gm`：登记 GM 命令 `gm.player.coins`（`{player_id, delta, note}`），经 `handlerGMWalletAdjust` 调 `Add(delta)`；不自动重试。4 条场景。
- `task/with/wallet`：task 组件上 `//roost:listen` 的 `OnWalletSpent`（实体事件 `event.WalletSpent`），每次成功扣款给任务事件 `wallet.spent` 记 1 次，与扣款同一笔事务。6 条场景。
- checkin、task、stamina、shop、gacha、idle、mailbox、purchase 不是经 `with/` 联动，而是在各自的组件里经宿主直接调 `Wallet()`。

## 验证范围

- `roost module verify wallet`：17 条场景（`^TestWallet`，包 `./game/handler`）。覆盖发放、消费、余额不足零写入、同键只生效一次、同键换金额拒绝、窗口淘汰、不合规的键、`math.MinInt64` 扣款、最大值与溢出、提交被拒时余额与回执一起回滚、只有 `Wallet` 被同步；另有 3 条把同一个组件挂在测试里的第二个实体（不是 Player，自己一份状态）上：加减、去重与各种拒绝、提交被拒与扣款事件的监听失败时回滚、同一个业务键在玩家与第二个实体上各生效一次。
- GM 联动另有 4 条（`^TestWithWalletGM`，包 `./game/modules/gm`）。
- 这些场景用真实的 Nest 引擎与生成的 DAO / Sender，提交器是不落盘的替身（可注入拒绝）。没有经过真实 TCP、Mongo、WAL 或多进程；协议端点 `HandleWalletSpend` 的网络路径没有在本模块的场景里验证。
