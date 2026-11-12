# shop：用金币买道具的商店——扣钱、发货、记限购在一笔事务里

## 能力

- **客户端协议**
  - `ShopBuy`（`protocol/def/shop_buy.go`）：请求 `ShopBuyRequest{Key, GoodsID, Count}`，应答 `ShopBuyResponse{Code, Reason, Cost, Balance, ItemID, Gained, Held, Bought}`。用金币买 `Count` 份商品 `GoodsID`；`Key` 是客户端为这一次购买生成的请求键，用来防重发。只能给自己买（目标取认证会话的玩家）。应答是成交那一刻的结果。
  - `ShopRules`（`protocol/def/shop_rules.go`）：只有推送形状 `ShopRulesPush{Goods}`，每项 `ShopGoodsRule{ID, ItemID, ItemCount, Price, Limit, LimitCount}`；不单独发送，作为 rules 模块规则快照里的一项下发。
- **服务端入口**
  - 组件 `ShopComponent`（`game/entities/player/shop_component.go`），经 `IShopEntity` 的 `ShopComp()` 取得：`Buy(key string, goodsID, count, nowUnixMilli int64) (ShopResult, error)`。`ShopResult{Cost, Balance, ItemID, Gained, Held, Bought}` 就是回包与回执里存的内容。
  - Nest handler `handlerShopBuy`（`game/handler/shop_buy.go`）：协议对应的 handler；生成的 Sender 是 `sender.NewShopBuySender(client).Sync_ShopBuy(ctx, playerEntityID, &pb.ShopBuyRequest{…})`。
  - `shopGoodsByID(goodsID)`（`game/entities/player/shop_goods.go`）：读当前 Nest 请求钉住的配置快照里的一行商品。
- **登记点**（`game/entities/player/shop_hooks.go`）
  - `OnShopBought(name, hook)`，`hook` 是 `ShopBoughtHook func(owner *Player, goodsID, count int64, result ShopResult, nowUnixMilli int64) error`。购买第一次生效时执行一次；回执重放与被拒绝的购买不执行。在 Player 锁内、同一笔 Nest 事务里，只改同一个 Player，不做 I/O。只在 `init` 登记；空名、nil、重名 panic。
- **状态**（`db/def/shop.go`）
  - `PlayerDao.Shop`（`ShopState{Day, Daily, Lifetime}`）：限购计数，落库并同步，玩家本人可见。只给有限购的商品记数。
  - `PlayerDao.ShopLedger`（`ShopLedger{Receipts, NextSeq}`）：请求键的去重窗口，只落库、不下发。
- **配置**
  - 配置表 `shop_goods`：结构在 `configs/schema/shop_goods.go`（`ShopGoods{ID, ItemID, ItemCount, Price, Limit, LimitCount, Off}`），数据在 `configs/table/shop_goods.csv`。`Limit` 取 0 不限、1 每业务日、2 终身（常量 `ShopLimitNone` / `ShopLimitDaily` / `ShopLimitLifetime`）；`LimitCount` 是份数上限；`ItemCount` 是每份里的道具个数；`Off` 是临时下架。
  - 缺省三行：10 金币买 1 个道具 1001；30 金币买 5 个道具 1002、每业务日限 3 份；200 金币买 1 个道具 1003、终身限 1 份。
  - 常量 `ShopReceiptWindow = 64`、`ShopKeyMaxLen = 64`（`shop_component.go`）。
- **错误码**（`internal/errors/shop.go`）：`shop_key_invalid`、`shop_key_conflict`（同一个键换了商品或份数）、`shop_count_invalid`、`shop_goods_invalid`（不在目录里、已下架或目录行不合法）、`shop_overflow`（总价或道具总数超出 int64）、`shop_limit`（超过限购）。钱不够、背包放不下原样透出 `wallet_short` 与背包模块的 `bag_stack_full`、`bag_kinds_full`、`bag_item_invalid`。

**日志**：购买确认，记录请求键、商品、份数、花费、奖励和余额。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/shop/shop_buy.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：只有金币一种支付货币，只卖道具；没有全服库存、折扣、限时上架、刷新、购物车；没有付费到账（另装 purchase）；没有“期望价格”校验——客户端目录与服务端不一致时按服务端当时的价格成交；超过限购时不做“能买几份买几份”，整笔拒绝。
- **样例目录装上就在卖**：缺省三行商品是样例，上线前必须换成自己的。
- **限额**：请求键 1～64 字节，只收 ASCII 字母、数字与 `-` `_` `:`；去重窗口保留这名玩家最近 64 次成功购买；限购计数表的大小不超过目录里有限购的商品数。
- **幂等的范围**：同一个键在窗口内重发，在任何校验与写入之前就认出来，原样返回第一次的结果（连每日清零都不做）。一个键成功之后又成功买了 64 次，它被淘汰，再用它会再扣一次钱、再发一次货（仍受限购约束）。被拒绝的购买不留回执，重发时重新判断。
- **一致性**：扣钱（`WalletComp().Add`）、发货（`BagComp().Add`）、限购计数、回执在同一笔 Nest 事务里（`rollback=undo durability=strict`）。背包拒绝时钱已经在本笔事务里扣过，靠事务回滚退回——调用方不能吞掉 `Buy` 的错误。提交被拒时回执也没了，重发会重新买一次。
- **每日限购按业务日惰性清零**：跨天后要等下一次成功购买才清 `Daily`；客户端显示前拿 `ShopState.Day` 与当前业务日（servertime 下发的 `Day`）比对。时钟回拨时不清零（宁可少卖）。
- **目录变更的后果**：删行、或改变一行的限购类型，会在玩家下一次成功购买时清掉对应的旧计数；复用商品号会串用终身限购计数。要保留计数就用 `Off` 下架。商品号、商品含义与限购类型上线后不要复用或改变。
- **规则快照不冻结价格**：展示目录与购买是两次请求，中间配置可能热更；以购买应答为准。
- **前提**：依赖 wallet、bag、rules（rules 再依赖 enter）、nest Mod 与配置表功能；实体名 `Player`。只在持有这名 Player 的进程内有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“shop 的边界”一条；“wallet 的业务键”一条记录了同样的键规则的来历。

## 复用方式

- **需求正好是它**：`roost module add shop`，连带装上 wallet、bag、rules、enter。普通购买不需要 Saga。
- **需求多一点**：
  - 换商品、调价、加限购：只改 `configs/table/shop_goods.csv`（列的校验在 `configs/schema/shop_goods.go`），再运行 `roost generate`；运行期按 configdata 热更。新增商品不需要改组件。
  - 购买成功后顺带做事（任务、成就、首购标记）：在自己的文件里 `init` 登记 `OnShopBought`。
  - 不要改 `shop_component.go` 里校验与写入的先后（商店自己的校验都在第一次写入之前），不要改 `ShopState` / `ShopLedger` 的 store 键与错误码码值。
- **别的功能要用它的数据**：读限购计数直接读 `PlayerDao.Shop`；读商品行用 `shopGoodsByID`（同包）或生成的配置访问器。要在自己的事务里代玩家买一份，嵌入 `player.IShopEntity` 后调 `target.ShopComp().Buy(key, goodsID, count, nowUnixMilli)`，错误原样返回。
- **不适合直接用的情形**：多货币 / 以物易物、全服限量、随机刷新的神秘商店、需要永久防重的订单。值得借鉴的是：回执记在自己的账本里而不占货币的去重窗口（能认出“同一个键换了内容”，也存得下发货结果）；用除法与减法做溢出与限购比较；成功购买时顺手清理过期计数（`dropStaleCounts`）让状态有上界。

## 联动

- `shop/with/rules`：登记名 `shop`，把未下架的商品按商品号升序编码成 `ShopRulesPush` 放进规则快照；读的是当前请求的配置快照。2 条场景。
- `task/with/shop`：登记 `OnShopBought("task", …)`，购买第一次生效时给任务事件 `shop.bought` 记“购买份数”，与购买同一笔事务；回执重放不计。3 条场景。（同一笔购买的扣款还会经 wallet 的钩子另记一次 `wallet.spent`。）

## 验证范围

- `roost module verify shop`：20 条场景（`^TestShop`，包 `./game/handler` 17 条、`./game/entities/player` 3 条）。覆盖：成交扣钱发货记数、不限购不占计数、键 / 份数 / 商品的各种拒绝、钱不够与背包满（钱退回）、总价溢出、提交被拒整笔回滚、到限购后拒绝而别的商品照卖、每日限购跨业务日清零、终身限购不清、时钟回拨、同键重发只扣一次、窗口淘汰、过期计数清理、同步只带 `Shop` 不带账本；配置装载拒绝非正价格、未知限购类型、未知道具。
- rules 联动另有 2 条（`^TestWithShopRules`）。
- 场景用真实的 Nest 引擎、生成的 DAO / Sender 与配置装载，提交器是不落盘的替身。没有经过真实 TCP、Mongo、WAL 或多进程。
