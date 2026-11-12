# Purchase 充值发货

从空的 `-template game` 工程执行 `roost module add purchase`，安装 wallet、platform 接线、发货组件和 `PurchaseCollect` 协议；`roost module add bag enter` 再安装道具发货与入场补发联动。基础 21 条场景，bag 联动 4 条、enter 联动 2 条。渠道尚未接入时拒绝回调，不能把零配置运行当作已接入支付渠道。

## 发货与清理

1. platform 对原文校验 HMAC，再调用工程 `internal/service/platform/purchase_channels.go` 的渠道校验。缺省全部拒绝。外部渠道 HTTPS 入口、渠道签名/查单和内部 JSON 转换由工程负责，不能相信客户端自报付款成功。
2. platform 记订单、调用发货协作者。`game/purchase/products.go` 是工程自己的商品与价格规则；第一次投递校验币种、最小货币单位整数金额、商品，再把不可变奖励快照写入 Redis。重复投递取原序号/快照，商品改价或下架不改变既有订单。
3. 每个玩家一个 hash：`next` / `acked` 游标，`o:<订单号>` 到序号、`g:<序号>` 到快照。序号和快照在同一条 HSET 分配，避免先计数后写入的空号；不假设 Redis Lua 会回滚已执行的命令。
4. 入场或 `PurchaseCollect` 每次最多取 32 笔。**发货之前**在实体锁外查询 platform：只有 `delivered` 或 `settled` 且 `PendingAttempts` 为空才进入 Nest。未终结或结果未知的订单保留，暂缓它和后续订单。
5. `PurchaseGrant` 严格事务把奖励与 Player 的 `Purchase.Applied` 连续游标一起提交。`delivered` 发奖；`settled` 表示退款/人工结清，只推进游标。更高序号不能越过前一笔；重放原序号不写 DAO，返回相同的订单、序号与奖励回执。
6. Nest 确认后才 Ack：推进 Redis 清理游标、删除已确认快照。其依据是 platform 不再发起终态订单，管理入口也不能重新打开它，且在途投递已经清空。预先取出的旧消息仍带旧序号，Player 不会再发一次。Ack 回答丢失可以原样重试。

平台的“delivered”在这里表示已写入待发队列，不等于 Player 奖励已经到账。金额不符、不存在的商品、背包满、提交失败、Redis 或平台不可用都会保留责任；不自动绕过投递次数上限。平台耗尽后的重新投递/人工结清仍走其受控管理流程，清理未知投递必须先确认外部尝试确实终止。

## 工程使用

客户端只调用无奖励参数的 `PurchaseCollect`，身份从可信会话取得。响应 `Receipts` 是本次确认的批次；同一批处理部分成功后失败时，已确认回执仍返回，并带业务错误码。收满 32 笔可继续调用，空批次表示当前待发已取完。回执不返回易变的余额，客户端余额/背包由自己的状态查询或同步链路读取。退款结清回执的 `Settled=true`，不得显示成发奖成功。

服务端锁外也可用 `game/controllers/purchase.FromRegistry(registry).Collect(ctx, playerID)`。装 enter 后每次入场补一批，不阻断登录；超过一批由客户端继续补领。离线期间的付费订单不设失效时间，不用“账本满了就拒绝”丢弃。奖励扩展用 `player.RegisterPurchaseReward`，只在 init 登记，校验/发放在事务内，不得 I/O。bag 联动就是一个完整例子。

`StoragePrefix`、Redis 部署必须在 game 与 platform 一致；隔离世界由工程配置不同前缀。持久 Redis 由部署保证，本模块不自动调整 AOF/副本策略。这里限制的是 **Player 常驻账本与单次拉取规模**：platform 支付历史与尚未履行的真实债务仍占空间。Redis 游标、待发数据与 Player 持久事实必须协调恢复，不能单独清空序号、回退 Player 备份或把旧订单重新手工放回队列。没有旧生成工程/旧订单格式自动迁移。

## 验证

`roost module verify` 覆盖默认金币发货、重复回执、乱序、价格不符、目录变化、提交回滚、513 笔之后重放旧单、结清和补领错误。根仓 `TestModulePurchaseScenariosAndMutations` 验证新工程、联动、重复安装、13 个变异和 race。

真实 Lua 的入口在生成工程：

```sh
ROOST_PURCHASE_REDIS_ADDR=127.0.0.1:6379 GOWORK=off go test -tags=integration -race -count=2 -run '^TestPurchaseRedis' ./game/purchase
```

显式 integration 缺少地址就失败；只清理测试唯一前缀。本批在独立临时 Redis 上执行过三条测试，不涉及共享服务，也不代表 Redis 持久性、真实 NATS/TCP/Mongo/WAL 联合验收已经完成。
