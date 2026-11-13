# purchase：充值到账（付款回调 → 待发队列 → 玩家发货游标）

处理“玩家在渠道付了钱，游戏里要把东西发给他”这一段：托管的 platform 服务验签、记订单并把一份不可变的奖励快照写进 Redis 的待发队列；玩家进入游戏或主动补领时，game 在一笔 Player 事务里发奖并推进一个连续的发货游标。同一笔订单不论回调、补领重放多少次都只发一次。

它不是“金币买东西”：那个用 `shop`，一笔 Player 事务完成。这里也不用 Saga。

## 能力

**客户端协议**（`protocol/def/purchase_collect.go`）

- `PurchaseCollect{}` → `PurchaseCollectResponse{Code, Reason, Receipts}`：补领自己的待发订单。请求没有任何字段，玩家是谁取自认证会话。每张 `PurchaseReceipt{OrderID, Sequence, Kind, ItemID, Count, Settled}` 是本次确认的一笔；`Settled=true` 表示这一笔是退款或人工结清，只推进了游标、没有发奖，客户端不能显示成发奖成功。一次至多处理 32 笔；一批中途失败时，已确认的回执仍然返回，同时带错误码。

**服务端入口**

- `purchase.FromRegistry(registry)`（`game/controllers/purchase/controller.go`）之后 `controller.Collect(ctx, playerID) ([]purchase.Receipt, error)`：服务端替一名玩家补领一批，在 Nest 事务之外调用。
- handler `PurchaseGrant(grant)`（`game/handler/purchase_grant.go`，`rollback=undo durability=strict`）：把一笔待发记录应用到 Player。参数只能来自待发队列。
- 待发队列 `purchase.Queue`（`game/purchase/purchase.go`）：`Record(ctx, order)`（platform 一侧，第一次投递时写快照）、`Pending(ctx, playerID, limit)`、`Ack(ctx, grant)`。实现是 `RedisQueue`（`game/purchase/queue.go`）。
- platform 一侧的协作者（`services/platform/collaborators.go`，安装时替换生成的桩）：`Deliver()` 把订单记进待发队列；`Verify()` 与 `Players()` 一律拒绝（充值不管登录）。

**登记点**

- `player.RegisterPurchaseReward(kind, PurchaseReward{Validate, Grant})`（`game/entities/player/purchase_component.go`）：登记一种奖励怎么发。`Validate` 与 `Grant` 都在发货的那笔 Player 事务里执行，不能做 I/O；返回错误则这一笔不发、游标不动。只在 `init` 里登记，种类重名直接 panic。内置 `coins`（经 wallet 的 `Add` 加金币）。

**留给工程的文件**

- `game/purchase/products.go`：`LookupProduct(id)`（商品、价格、币种与奖励；缺省只有 `coins_100`：99 美分换 100 金币）与 `StoragePrefix`（待发队列的 Redis 命名空间，缺省 `roost:<工程名>:purchase`）。
- `services/platform/purchase_channels.go`：`verifyPaymentCallback(ctx, raw, signature)`，渠道验签与查单。**缺省全部拒绝**。

**状态**

- `PlayerDao.Purchase`（`db/def/purchase.go` 的 `PurchaseState{Applied}`）：已发到第几号，只落库、不下发。大小是常量，不随订单数增长。
- Redis：每名玩家一个 hash（键 `<StoragePrefix>:{<玩家号>}`），字段 `next`（已分配到几号）、`acked`（已清理到几号）、`o:<订单号>`（→ 序号）、`g:<序号>`（→ 奖励快照）。

**配置**：没有配置表与配置键；规则在上面两个文件里。

**错误码**（`internal/errors/purchase.go`）：`purchase_invalid`（待发记录不合法或不是这名玩家的）、`purchase_sequence`（前一笔还没发，不能越过）、`purchase_reward`（奖励种类没有登记）。补领时还会原样带回 platform 的码：订单对不上（600106）、投递还在途或订单未终结（600107）。

**日志**：订单入队和每笔发货确认，记录玩家、订单、序号、奖励、结清状态；发货确认在队列 Ack 前，重试按订单号关联。身份 / 商品不符及入队失败记 Warn。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/purchase/controller.go`, `game/controllers/purchase/purchase_collect.go`, `services/platform/collaborators.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不接任何真实渠道（没有渠道的 HTTPS 入口、没有验签与查单实现，都归工程）；不下单、不出价签；不做退款流程（只认 platform 已经标成结清的订单并跳过发奖）；不推送到账通知；回执里不带余额；不相信客户端自报的“付款成功”。
- **渠道缺省拒绝**：零配置能启动，但所有付款回调都被拒。能启动不等于已接入支付。
- **批量与暂缓**：每次补领至多 32 笔（`BatchLimit`）。队列按序号连续处理：某一笔发不出去（背包满、奖励种类没登记、platform 那边订单还没终结或查不到），它和它后面的订单都留在队列里，等它解决。
- **“已投递”不等于到账**：platform 的 `delivered` 只表示写进了待发队列。Player 奖励到账以 `PurchaseGrant` 提交为准。
- **一致性与幂等**：
  - 奖励与游标 `Applied` 在同一笔 Player 严格事务里；提交失败两者都不变，可以重放。
  - 序号小于等于游标的重放不写任何字段，返回同样的回执；序号必须恰好是游标加一。
  - 序号与快照在 Redis 里用同一条 HSET 分配；同一订单重复投递拿到原序号与原快照，商品改价或下架不改变已有订单。价格、币种或商品对不上的订单不入队。
  - 发货之前在实体锁外向 platform 查订单：只有 `delivered` 或 `settled` 且没有在途投递的才进 Nest。
  - Nest 确认之后才 `Ack`（推进 `acked`、删快照）；`Ack` 失败可以原样重试，重试时 Player 不会再发一次。
- **不过期**：离线期间的订单没有失效时间，不会因为“账本满了”被丢弃。Redis 里未履行的订单与 platform 的支付历史会一直占空间。
- **三处状态必须一起恢复**：Redis 的游标与待发、Player 的 `Applied`、platform 的订单，不能单独重置、回退其中一侧，也不能把旧订单手工放回队列。`StoragePrefix` 在有在途发货时不能改。
- **序号上限**：2^53−1（Redis Lua 的数值精度）。
- **前提**：依赖 `wallet`；业务服务要有 `nest` 与 `redis` 两个 Mod 并 `uses: [platform]`；game 与 platform 必须用同一个 Redis 与同一个 `StoragePrefix`；Redis 的持久性由部署保证，模块不调整 AOF 或副本策略。安装时替换 `services/platform/collaborators.go`，那个文件已经被工程改过时安装被拒绝。
- **已知限制**：[KNOWN-LIMITS“purchase 模块的恢复与验收边界”](../../docs/maintenance/KNOWN-LIMITS.md)——真实 NATS / TCP / Mongo / WAL、跨 sid 与故障恢复没有验收；旧订单格式不自动迁移。

## 复用方式

- **需求正好是它**：`roost module add purchase`，连带装上 `wallet`，并接好 platform 的协作者、发货组件与 `PurchaseCollect`。要发道具、要进入游戏时自动补领，再 `roost module add bag enter`。
- **需求多一点**：
  - 接渠道：实现 `purchase_channels.go` 的 `verifyPaymentCallback`；渠道的 HTTPS 接入与“渠道凭据 → 内部支付 JSON”的转换另写。
  - 商品表：改 `products.go` 的 `LookupProduct`（金额用货币最小单位的整数）。
  - 新的奖励种类（月卡、礼包）：在自己的文件里 `RegisterPurchaseReward("<种类>", …)`，照着 `with/bag` 的 `purchase_item.go` 写。
  - 不要改 `PurchaseComponent.Apply` 的游标判断、`queue.go` 的四段 Lua，以及 `Collect` 里“先查 platform、Nest 确认后才 Ack”的顺序。
- **别的代码要用它**：补领 `controller.Collect(ctx, playerID)`；读某名玩家发到第几号，在 Player handler 里 `owner.Dao().GetPurchase().GetApplied()`。
- **只适合借鉴的情形**：别的“外部系统欠玩家东西”的场景（补偿、跨服奖励）。值得借鉴的是“按玩家连续编号的待发队列 + Player 上一个常量大小的游标”，它让重放安全而且不需要在玩家档里存无限增长的订单表。直接复用本模块的队列去发别的东西不合适：序号空间与 platform 的订单绑定。

## 联动

- `purchase` + `bag`（`with/bag/game/entities/player/purchase_item.go`）：登记奖励种类 `item`，道具在发货游标的同一笔事务里进背包；背包拒绝（未知道具、放不下）时游标不动。
- `purchase` + `enter`（`with/enter/game/controllers/purchase/purchase_enter.go`）：登记 enter 的进入之后钩子，每次进入游戏补领一批；失败只由 enter 记日志，不阻断登录，玩家可以再发 `PurchaseCollect`。
- 别的模块没有 `with/purchase`。

## 验证范围

- `roost module verify purchase`：21 条场景（`game/handler` 8、`game/purchase` 5、`game/controllers/purchase` 6、`services/platform` 2）；与 `bag` 的联动 4 条，与 `enter` 的联动 2 条。
- 离线：真实的 Nest 引擎加替身提交器，待发队列与 platform 是内存替身。
- 覆盖：金币发货与游标同提交、重放不写、乱序等待、提交失败后重放、超过 513 笔之后重放旧单仍安全、别人的订单被拒、结清只推进游标、价格不符不入队、目录变化不影响已有快照、队列回答损坏不当回执、补领时等待在途投递与未终结订单、`Ack` 失败后重试得到同一张回执、渠道缺省拒绝。
- 真实 Redis 上的 Lua 另有 3 条集成用例（构建标签 `integration`，不在 `verify` 的 21 条里），在生成的工程里跑：

  ```sh
  ROOST_PURCHASE_REDIS_ADDR=127.0.0.1:6379 GOWORK=off go test -tags=integration -race -count=2 -run '^TestPurchaseRedis' ./game/purchase
  ```

- 没有验证：Redis 的持久性，真实 NATS / TCP / Mongo / WAL 的联合验收，跨 sid，故障恢复。

## 发货过程

1. platform 对回调原文校验 HMAC，再调工程的 `verifyPaymentCallback`。HMAC 只证明请求来自持有内部回调密钥的接入方，不能代替渠道的支付凭据验证。
2. platform 记订单并调发货协作者。第一次投递时按 `LookupProduct` 校验币种、金额与商品，把奖励快照写进 Redis。
3. 进入游戏（装了 `enter`）或 `PurchaseCollect` 时取一批待发。逐笔先向 platform 查订单状态，再发 `PurchaseGrant`。
4. `PurchaseGrant` 提交之后 `Ack`。此时 platform 的这笔订单已经终结、不会再被重新打开，在途投递也已清空，所以删掉快照是安全的；预先取出的旧消息带的是旧序号，Player 不会再发一次。

平台投递次数耗尽之后的重新投递与人工结清走 platform 自己的受控管理流程；清理结果未知的投递之前要先确认外部的尝试确实终止了。
