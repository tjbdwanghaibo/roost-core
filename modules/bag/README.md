# bag：玩家背包（道具号到数量的堆叠背包，自带道具表）

## 能力

背包只记“每种道具有几个”。组件在共用包 `game/components/bag`（`bag.Component`，宿主接口 `bag.Owner`），模块装好时挂在 `Player` 实体上，别的功能要发道具或扣道具都经过它；同一个组件也能挂到别的实体上（见“复用方式”）。

- **客户端协议**
  - `BagDiscard`（`protocol/def/bag_discard.go`）：玩家丢弃自己的道具。请求带 `ItemID`、`Count`，应答带 `ItemID` 与丢弃后的剩余数量 `Left`。端点是 `game/controllers/bag/bag_discard.go` 的 `HandleBagDiscard`，目标玩家取自认证会话，不取请求字段。
  - 没有“查询背包”的协议：背包随同步下发给本人（见“状态”）。
- **服务端入口**（逻辑在 `game/components/bag/component.go`，Player 的接线在 `game/entities/player/bag_component.go`；都要求调用方已经在 Nest 里持有这名 `Player` 的锁）
  - `BagComp().Add(itemID, count int64) (int64, error)`：放入，返回放入后的数量。
  - `BagComp().Remove(itemID, count int64) (int64, error)`：扣除，返回剩余数量；扣到 0 时删掉这个键。
  - `BagComp().Count(itemID int64) int64`：持有数量，没有时为 0。
  - `BagComp().Has(itemID, count int64) bool`：是否至少有 `count` 个；返回 `true` 时紧接着的 `Remove(itemID, count)` 一定成功。
  - `IBagEntity`：窄实体接口。别的功能的 Nest handler 把目标参数声明成嵌入它的接口，就能在同一笔事务里调上面四个方法。
  - Nest handler `handlerBagGrant`（`game/handler/bag_grant.go`）：服务端发放道具，没有协议与端点。不持有 `Player` 锁的服务端代码经生成的 Sender 调用：`syncsender.NewBagGrantSender(client).Sync_BagGrant(ctx, 完整实体号, itemID, count)`。
  - Nest handler `handlerBagDiscard`（`game/handler/bag_discard.go`）：丢弃协议背后的事务，`durability=strict`。
- **登记点 / 钩子**
  - 没有登记函数。规则入口是工程文件 `game/components/bag/items.go` 的 `ItemRule(itemID) (maxStack int64, ok bool)`：`Add` 在写入前调用它，决定“这个道具号能不能进背包、单种最多放多少”；`Remove` 不调用它。规则对挂这个组件的所有宿主一样生效。Player 包里另有同义的 `bagItemRule`，给同一实体上的别的组件（gacha 的容量预检）用。
- **状态**
  - `PlayerDao.Bag`（类型 `BagState`，`db/def/bag.go`），字段 `Items` 是“道具号 → 数量”的 map，只存数量为正的道具。标记为 `persist,sync`：落库，并随同步下发给玩家本人。
- **配置**
  - 道具表：列定义在 `configs/schema/item.go`（`Item{ID, Name, MaxStack}`），数据在 `configs/table/item.csv`，自带三行：`1001` 药水（单种上限 99）、`1002` 体力券（999）、`1003` 新手礼包（`max_stack` 为 0，表示不单独限制）。`roost generate` 生成访问器 `generated.ItemByID`。
  - 两个代码常量在 `game/components/bag/component.go`：`bag.MaxKinds = 200`、`bag.MaxStack = 999999`；Player 包里以 `BagMaxKinds`、`BagMaxStack` 的名字再导出一次。
- **错误码**（`internal/errors/bag.go`）
  - `bag_item_invalid`：道具号不在规则允许的范围内（缺省是“道具表里没有这一行”）。
  - `bag_count_invalid`：数量不为正。
  - `bag_short`：持有不足。
  - `bag_stack_full`：放入后会超过这种道具的单种上限。
  - `bag_kinds_full`：背包里没有这种道具，且种类数已到上限。

**日志**：丢弃与 GM 发放确认，记录道具、数量和剩余数量；事务内 Add/Remove 由发起业务记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/bag/bag_discard.go`, `game/gm/bag.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有格子与位置、没有道具实例（每个道具只有数量，不能带强化等级、词条、绑定状态）、没有过期、没有“使用道具”、没有整理与交易。丢弃的道具不去任何地方。装备穿戴在 `equipment` 模块。
- **上限**：至多 `BagMaxKinds`（200）种道具；单种数量硬上限 `BagMaxStack`（999999），道具表的 `max_stack` 填得更大也按硬上限算。到种类上限后只拒绝新种类，已有的种类照常加减。
- **拒绝就是整笔拒绝**：`Add` 不做“能放多少放多少”，`Remove` 不做“有多少扣多少”；被拒绝时不写任何字段。
- **不判重**：`Add`、`handlerBagGrant` 调一次发一次；`BagDiscard` 请求不带业务键，客户端超时后重发会再丢一次（数量不足时被拒）。“这一次发过没有”要由调用方记在自己的状态里，并在同一笔事务里调 `Add`。
- **一致性**：`Add` / `Remove` 只改 `PlayerDao.Bag`，与调用它的 handler 的其他写入在同一笔 Nest 事务里提交或回滚；组件自己不登记 undo。只在持有这名 `Player` 的进程内有效，没有跨进程的背包操作。
- **道具表变化**：从表里删掉一行后，这种道具不能再放入，已持有的仍能数、能扣、能丢。单种上限调小到持有量以下时只拒绝放入，不扣已有的。进程里没有装载配置时道具表为空，任何放入都答 `bag_item_invalid`。
- **宿主**：组件本身不认具体实体，任何在自己 DAO 里放一份 `BagState` 的实体都能挂（见“复用方式”）。模块装好时只接在 `Player` 上：丢弃协议、发放 handler 与各联动都是 Player 的；挂到别的实体时这些不跟过去，那个实体的入口由接线的人写。种类上限、单种上限对每个宿主各算各的。
- **跨实体**：组件的每个方法只改所在宿主自己的 DAO。一笔操作同时动两个宿主（例如玩家把道具存进公会仓库）照多实体 handler（两边在同一个进程、同一笔 Nest 事务）、Remote 实体或 Saga（跨进程、带补偿）写，组件不替你协调。
- **前提**：需要 `nest` Mod 与 `config` 特性（`-template game` 缺省已开）；模块装在名为 `Player` 的实体上，按 `-template game` 的目录约定写文件。不依赖别的模块。
- **已知限制**：见 `docs/maintenance/KNOWN-LIMITS.md` 的“bag 的边界”一条（表名 `item` 固定、工程已有同名表时装不了；每次变化同步的是整份背包而不是按键增量）与“不装 enter 时安装不打开客户端同步”一条。

## 复用方式

- **需求正好是它**：`roost module add bag`。不连带安装别的模块。客户端要看到背包，还需要玩家 TCP 接入与自身视图（装 `enter` 模块时会代办）。
- **需求比它多一点**
  - 加道具、改单种上限：只改 `configs/table/item.csv`，再 `roost generate`。
  - 给道具加属性（品质、售价等）：在 `configs/schema/item.go` 加列，CSV 加同名列，`roost generate` 后用 `generated.ItemByID(id)` 读。
  - 换准入规则（某类道具不进背包、统一上限）：改 `game/components/bag/items.go` 的 `ItemRule`。
  - 改种类上限或硬上限：改 `game/components/bag/component.go` 的两个常量。
  - 道具号发布后不要挪给别的道具用：玩家背包里记的是道具号。
- **别的模块 / 业务代码要用它**
  - 已经持有 `Player` 锁（在自己的 Nest handler 里）：目标接口嵌入 `player.IBagEntity`，调 `target.BagComp().Add(itemID, count)` 或 `Remove`，错误原样返回给 Nest。一次扣多种道具时先逐个 `Has`，全部满足再逐个 `Remove`。
  - 没有持锁（controller、后台任务、GM）：`syncsender.NewBagGrantSender(client).Sync_BagGrant(ctx, 完整实体号, itemID, count)`。它不判重，有“只发一次”要求时不要用它，改为写自己的 handler（先判自己的状态，再调 `BagComp().Add`）。
  - 测试里往背包放道具之前要先装一张道具表，写法见 `game/handler/bag_test.go` 的 `bagTestLoadItems`。
- **挂到别的实体上**（公会仓库、队伍共享箱等；不新增模块，在工程里写一个接线文件即可）：
  1. 在那个实体的 DAO（`db/def/<实体>.go`）里加一个字段，store 键自己起、发布后不改：`Bag BagState \`store:"bag" dao:"persist,sync"\``，同步与否按那个实体给谁看决定。
  2. 在实体包里写接线文件（照 `game/entities/player/bag_component.go`）：`//roost:component type=<号>` 标出自己的组件号常量，号用 `roost id next component`（或 `roost add component` 分配）在工程的组件号段里申请，不复用 Player 的号；`type XxxBagComponent = bag.Component`；`init` 里 `entity.RegisterComponentFactory(<号>, bag.NewComponent)`；实现 `BagState()`，返回自己 DAO 里的那份。
  3. 实体结构体里加组件字段（`bag *XxxBagComponent \`comp:"<常量名>"\``），`roost generate` 生成 `XxxBagComp()` 访问器；自己的 Nest handler 经它调 `Add` / `Remove` / `Count` / `Has`。
  - 本模块的场景 `TestBagSecondHost*` 就是这样把同一个组件挂到测试里的第二个实体上，验证放入、拿走、拒绝、回滚与两个宿主互不影响。
- **只适合借鉴的情形**：需要格子背包、道具实例或带过期时间的道具时，不要在它上面改；可以借鉴的是“规则入口单独一个工程文件、组件只改 DAO、拒绝时零写入、用减法比较防溢出”这几点。

## 联动

- `bag` + `gm`（`with/gm/game/gm/bag.go`）：登记运维命令 `gm.player.add_item`，参数 `player_id`、`item_id`、`count` 与审计字段，经 `Sync_BagGrant` 发放；不自动重试。
- `mailbox` + `bag`（在 `mailbox/with/bag`）：登记 `item` 附件种类，领取邮件时道具进背包，与领取账本同一笔事务。
- `purchase` + `bag`（在 `purchase/with/bag`）：付费道具在发货游标的同一笔事务里进背包。
- `task` + `bag`（在 `task/with/bag`）：任务的道具奖励与领奖在同一笔事务里提交。
- 以 `requires` 依赖它的模块：`alliance`、`equipment`、`gacha`、`shop`。

## 验证范围

- `roost module verify bag`：20 个场景（`./game/handler`，`^TestBag`）；与 `gm` 的联动另有 4 个场景（`./game/gm`，`^TestWithBagGM`）。
- 场景是离线的：真实的 Nest 引擎、生成的 DAO 与 Sender、真实装载的测试道具表；提交器是替身（`bagCommitter`，不落盘，可注入拒绝来验证回滚）。
- 覆盖：发放与堆叠、丢弃与扣到 0 删键、数量不足与非法输入零写入、单种上限与种类上限、上限调小、道具从表里删除、没装配置表、提交被拒后回滚、状态标记为同步；以及把同一个组件挂在测试里的第二个实体（不是 Player，自己一份状态）上的 3 个场景：放入 / 拿走 / 各种拒绝、提交被拒与 handler 失败时回滚、玩家背包满了不影响第二个实体。
- 没有验证：真实数据库与 WAL、经 TCP 的客户端链路、同步载荷实际到达客户端。
