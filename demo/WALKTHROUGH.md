# game-demo 功能逐项说明

`roost project new … -template game-demo` 生成的工程里每块功能为什么这样写：做了什么、刻意没做什么、边界在哪。
怎么生成、运行与验证见同目录 [README](README.md)；框架层的契约以 [docs/framework](../docs/framework/README.md) 为准，本文只写 demo 自己的取舍。
文中路径都是生成后工程里的路径（本目录里多一个 `.tmpl` 后缀）。

## 链路

本节是这条写入链的逐层说明；六条路径（同步写、事件链、跨服务、saga、帧同步、配置）横向的对比——
每层守什么、失败在哪一层处理、重放靠什么幂等——见[数据流：六条路径](../codegen/docs/DATA_FLOW.zh-CN.md)；协议、Controller 与 Nest handler 的分工见[玩家协议处理教程](../docs/framework/PLAYER-ACCESS.md)。

```
TCP (player access)
  → game/controllers/player/add_item.go      端点（本目录）：错误边界，coded error → 响应里的 Code/Reason
  → game/handler/syncsender/…Sync_AddItem     Sender（从 handler 参数与返回值生成）
  → Nest 锁住 Player
  → game/handler/add_item.go                  handlerAddItem（本目录）：int32 的协议数量在这里转成模块的 int64
  → BagComponent.Add                          bag 模块：数量为正 → 道具规则 → 单种上限 → 种类数上限 → 只走生成的 mutator
      ↳ bag_items.go 的 bagItemRule            bag 模块留给工程的规则文件；缺省读道具表，demo 没有改它
      ↳ configs/generated.ItemByID             生成的表访问器，读的是钉在本次请求上的配置快照
      ↳ internal/errors.ErrBag*                bag 模块的错误码（internal/errors/bag.go），号在安装时从清单的 errcode 号段分配
  → PlayerDao.Bag 的 SetItemsKey / DelItemsKey 生成的嵌套 map mutator，带 dirty / undo / patch
  → dataengine 落库
```

**背包是 [bag 业务模块](../modules/README.md)，不是 demo 自己的代码**：`-template game-demo` 生成时由 `roost module add bag`
装进来（`codegen/internal/roost/demo.go` 的 `demoModuleSteps`），别的工程装同一个模块得到同一份实现。模块带来组件
`BagComponent`（`Add` / `Remove` / `Count` / `Has`）、状态 `PlayerDao.Bag`（嵌套结构体，存储键 `bag`）、道具表
（`configs/schema/item.go` 与 `configs/table/item.csv`）、五个错误码、丢弃协议 `BagDiscard` 与服务端发放 handler `BagGrant`，
以及它自己的 17 条场景（`roost module verify`）。demo 在它之上做的：

- **只换道具表的数据**：模块装好之后覆盖 `configs/schema/item.go` 与 `configs/table/item.csv`——多 `attack` / `hp` 两列
  （属性系统给穿着的装备算加成），行是 demo 的五种道具。背包规则文件 `game/entities/player/bag_items.go` 是模块的缺省实现：
  表里有这一行道具号就合法，`max_stack` 是单种上限（0 表示不单独限制，按硬上限 999999）。加道具、改上限只改 CSV。
- **建在背包组件上的事务是 demo 的**：`AddItem`、赠礼的 debit / refund、公会的扣费与发放，
  都在自己的 Nest handler 里调 `target.BagComp().Add / Remove / Count`（同一个实体、同一笔事务）。`roost add handler --component Bag`
  要求组件文件已经在盘上，而组件在最后的模块步骤才落盘，所以生成步骤里这些 handler 没有骨架步骤，文件直接写出、留在原处
  （骨架本来也会被立刻覆盖）；模块那一步跑生成器时全部 handler 都已经在盘上。邮件附件进背包不在其中：那是 mailbox 模块与
  bag 模块的联动（`game/entities/player/mailbox_item.go`，见下文[邮件](#邮件列表与领附件客户端这一半)）。
- **领取账本不是背包的职责**：付费发货来自 [purchase 模块](../modules/purchase/README.md)，Redis 按玩家保存待发快照，
  Player 的 `Purchase.Applied` 连续游标与奖励在同一笔事务提交；确认后清理待发，旧序号不能再发一次。demo 不保留不断增长的
  `PurchaseClaims`。商品表与模拟渠道是 demo 的规则，真实渠道仍需工程接入；模块与 enter 联动补发离线订单。
  邮件附件账本仍由 mailbox 模块维护。demo 购买响应里的 BagCount 是另一笔只读 Nest 查询的当前快照，不是发货幂等回执。
- **同步不用组件自己标脏**：模块的组件不调 `Player.PublishSyncDirty()`（模块不认识工程的同步）。demo 的 Nest 装着场景的
  提交观察者，事务准入时按实体收集生成的 setter 留下的同步标记，所以背包的变化照样作为增量到本人的客户端；被拒绝而回滚的
  发放什么都不发；别的玩家的视图里没有 `bag`。`internal/service/game/bag_sync_test.go` 在与服务相同的接线上断言这三件事。
  有一处冗余：`PublishSyncDirty()` 与提交观察者各读一份掩码，模块的写入只被观察者那一份消费，所以背包变化之后，
  这名玩家下一次 `PublishSyncDirty()`（走一步、加经验）会把整份背包再给本人发一次，内容与上一次相同；背包至多 200 种，量有界。

几个刻意保留的教学点：

- **业务代码只碰生成的 mutator**（`bag.SetItemsKey(...)`），不直接写私有字段——dirty 追踪、Nest undo 和持久化
  patch 全靠它。
- **道具数量先校验再写**：`BagComponent.Add` 先判数量为正、道具在道具表里，再用减法比较单种上限（`count > 上限 - 持有量`，
  加法在数量接近 int64 上限时会溢出成负数、绕过上限），全部通过后才调用 mutator。拒绝时不写任何字段：超过单种上限是
  `bag_stack_full`（不做“能放多少放多少”），不在道具表里是 `bag_item_invalid`，数量不为正是 `bag_count_invalid`，
  种类数到 200 之后的新种类是 `bag_kinds_full`。扣到 0 时删掉这个键，不留数量为 0 的条目。
- **handler 参数与协议字段必须对上**：`add endpoint` 会解析两边并拒绝对不上的组合，所以
  `game/handler/add_item.go` 的 `(itemID int64, count int32)` 与 `protocol/def/add_item.go` 的
  `ItemID` / `Count` 是同一份契约。handler 的第一个返回值（新数量）经 Sender 原样带回端点。
- **`rollback=undo durability=strict`**：handler 返回 error 时本次调用过的 mutator 全部回滚。
- **错误在端点换形状**：生成的 TCP server 把"端点返回 error"当作坏帧处理——断连。所以业务失败不能作为
  error 返回，而是 `errcode.ClientError(err)` 换成 `(code, reason)` 写进响应；没有 code 的错误（基础设施故障、
  bug）统一坍缩成 `CodeInternal` + 固定文案，内部信息不出网，原因在端点处打日志。
- **配置表走生成器**：`configs/schema/item.go` 的 `//roost:table` 是唯一手写的地方；`roost generate` 生成类型化
  loader、把 `configs/table/item.csv` 转成 `configs/data/item.json`（CSV 前四行是列名 / 标题 / 类型 / 规则），
  并注册进 `framework/configdata`。`required` / `unique` 在转换时校验，坏数据死在 `make generate`，
  不会死在玩家请求里。
- **错误码归号段**：`roost add errcode X -id N` 的 N 必须落在清单 `ids.errcode`（100000–199999）里，
  `roost id check` 负责查重；`docs/generated/errcode.csv` 是给客户端的对照表。

## 用于 SLG 等游戏时，哪些规则归业务

demo 演示接法，玩法不要求进入公共框架；也不要把 demo 自己违反接入契约的问题都推给使用者。

| 需求或疑点 | 处理层与现有接法 |
| --- | --- |
| 公会名字数、敏感词、全服重名 | 业务规则。按维护者决定，demo 只做基本名字校验，允许同名；要唯一可用 `service/directory` 预约与提交，不要求 Entity 内建唯一名称。 |
| 资源产消、行军目标与速度、赛季分组、奖励额度、逾期补偿 | 业务决定状态和数值，用 DAO、Nest、业务钟、Timer、mail 等正式接口实现；不因缺少某种 SLG 玩法就认定框架有 bug。 |
| 已领奖的局能否再次发奖、示例排行榜能否被正常重放多加分 | 规则与实现分开。窗口由业务定义，但既然 demo 声称窗口内只发一次，就必须正确持久记账；示例误用短期去重导致多计分是 demo 问题。 |
| 玩家身份、Remote 所有权、锁、回滚、WAL 提交门与同步可见性 | 框架已经声明的保证，合法调用下违约要提框架问题；业务仍须选择正确的身份来源、事务边界和结果未知处理。 |
| 活动累计分被较旧的 RPC 回复覆盖 | demo 并发接法问题；公共协调器的权威值可以正确而本地结算索引错误，需要在示例写索引的位置修复。 |
| 每个玩家的建造 / 行军到期都能自动执行 | 通用 Scheduler 可用，持久字段、加载重建和 Tick 接入仍由业务负责；按实体接入骨架已由 [D11](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d11-按实体的持久定时器接入) 实施，离线准点唤醒等维护者明确留给业务的部分仍不归框架负责。 |

本机 demo 跑通说明这些示例路径能组合运行，不证明所有部署模式、远端故障和整套 SLG 玩法都已验收。

## 档案：名字、等级、经验来自 profile 模块

名字、等级、经验用的是 [profile 业务模块](../modules/README.md)（生成时由 `roost module add profile` 装进工程）：`ProfileComponent`、
状态、改名协议 `ProfileRename`（非空、合法 UTF-8、至多 16 个字符）、发经验的规则（每级 100、可连升多级、int64 经验与 int32 等级的范围）、
只检查不执行的 `CheckAddExp`、服务端发经验的 handler `ProfileAddExp`、给服务端读的名片 handler `ProfileCard` 与它们的场景测试
（`game/handler/profile_test.go`）都是模块的，demo 不再带自己的档案组件。

- **状态是模块追加在 `PlayerDao` 末尾的两个顶层字段**：`Profile`（名字、等级——别的玩家也看得到）与 `ProfilePrivate`（等级内经验——只给本人）。
  同步的白名单按顶层字段列，所以“别人看不到经验”就是别人的视图里不列 `ProfilePrivate`（[同步视图](README.md#同步视图谁看见哪些字段)）。存储键是
  `profile` / `profile_private`，里面是数字短键（`profile.1` 名字、`profile.2` 等级、`profile_private.1` 经验）。
- **demo 只覆盖模块留给工程的一个文件**：升级钩子 `game/entities/player/profile_levelup.go`（见下一节）。
- **demo 自己挂在这个组件上的两样东西不是档案**，各在自己的文件里、状态也在 demo 自己的 `PlayerDao` 字段里：公会副本
  `guild_membership.go`（`GuildID` / `SetGuild`）与副本领奖账本 `dungeon_ledger.go`（`ClaimDungeonRun`、通关计数）。它们仍是
  `ProfileComponent` 的方法：相关 handler 一直经 `IProfileEntity` 这个窄接口拿玩家，另立组件只多一个组件号。
- **两实体的 `AddExp` 是 demo 的**（Player 加经验、World 记全服累计，见[实体锁档与跨实体事务](#实体锁档与跨实体事务)），它调模块的
  `CheckAddExp` 与 `AddExp`。模块的 `ProfileAddExp` 只动 Player，不记 World 的累计，demo 的入口（端点、GM、副本结算）都不走它。
- 生成步骤里锁 Profile 组件的 handler（`AddExp`、`ClaimDungeon`、`PlayerTeam`、公会的 `ShowGuild` / `PlayerGuild`）与背包的那些一样，
  没有 `roost add handler` 骨架步骤（组件在最后的模块步骤才落盘），直接写文件。全部模块在同一步里装，号按列表顺序分配
  （`enter`、`chat`、`leaderboard`、`bag`、`profile`、`mailbox`），新换用的模块加在末尾。

## 事件链：升级 → 奖励邮件（事务性 outbox）

经验发放在任何 DAO 写入之前先做只读校验（`handler.checkExpGrant`）：Player 这一半问 profile 模块的 `ProfileComponent.CheckAddExp`（数额不是正数是
`profile_exp_invalid`，int64 经验加法或 int32 等级放不下是 `profile_exp_overflow`），World 这一半问 `StatsComponent.CheckRecordExp`（int64 累计放不下是 demo 的
`exp_amount`，100004）。任何一方拒绝时 Player、属性、副本领奖账本、World 与升级效果都不写，也没有新的提交记录；`AddExp` / `RecordExp` 自己再查一次只是兜底，不靠写完再由 `rollback=undo` 撤回。`ClaimDungeon` 先判“已领取”（重放照旧返回 Claimed=false），再校验，最后才写账本。等级按平坦曲线用除余计算，不按奖励数量逐级循环（修前 `amount=MaxInt64` 会一直占住业务 worker）。GM `gm.player.add_exp`、`AddExp` 端点、副本结算 `FinishDungeon` 都经这两个 handler，同一道校验。生成工程回归在 `game/handler/add_exp_test.go`。

```
AddExp 端点 → Nest 锁 World、Player → handlerAddExp → ProfileComponent.AddExp（profile 模块）
  ├─ ProfilePrivate.SetExp / Profile.SetLevel   状态变更
  ├─ onLevelUp(from, to)                         demo 的 profile_levelup.go 发升级事件
  │     └─ effects.EmitPlayerLevelUp             nest.Emit：effect 与状态变更进同一条 WAL 记录
  └─ OnProfileLevelUp 的 attribute 登记钩子       属性 Base 层跟着等级涨，仍在同一事务
        ↓ commit 后由 dataengine 发到 JetStream（<subject_prefix>.player.level_up）
internal/service/game/level_up_mail.go     kitdataengine.SubscribeEffects：durable + Mongo inbox（每个 EffectID 只处理一次）
  └─ gamemailbox.SendTo(RequestID = EffectID)  mailbox 模块的发件入口 → mail.Send；mail 服务按 RequestID 去重：第二层幂等
```

- **升级"这件事"在升级钩子里发出**，与让它成立的状态变更同一笔事务；谁对它做出反应住在别处。“升级了要通知别的功能”是档案模块的
  职责（它在升级之后调一次钩子，连升多级也只调一次），“通知谁、怎么通知”是工程的：demo 的钩子发 `player.level_up`，payload 里是升级之后的
  等级。handler 回滚时 effect 一起消失，所以永远不会给一个没落库的升级发奖励；钩子返回错误时经验和等级也一起撤回。从哪条路进
  `AddExp`（demo 的两实体 AddExp、副本领奖、模块的 `ProfileAddExp`）都走同一个钩子，`TestLevelUpCarriesTheRewardEffectInTheSameCommit` 钉住这一条。
- **两层幂等**：inbox 收据与业务写入在一个 Mongo 事务里提交，进程重启后重投的 effect 会被认出来；邮件是总线调用
  不是 Mongo 写入，所以还要靠 `RequestID = EffectID` 让 mail 服务自己去重。
- **生产者与消费者读同一组配置键**（`dataengine.database` / `dataengine.effects.*`）、用同一组默认值，两边不会对"effect 在哪"
  产生分歧。
- 消费者在 `Service.Init` 里订阅、`Shutdown` 里 `Drain`：进程宕机期间提交的升级，回来时会补投。
- 订阅只写处理函数：流、主题前缀、回执库与重投退避由框架的 `kitdataengine.SubscribeEffects` 按 DataEngine Mod 的声明组装。
  载荷解不开、或奖励不是 mailbox 发得出去的附件（`gamemailbox.ErrLetterInvalid`，例如热补丁换上了一个没登记的附件种类）
  的 effect 标记为毒消息（`kitdataengine.PoisonEffect`）直接丢弃并记 ERROR，不会一直重投；mail 服务调用失败按退避重投。

## 邮件：列表与领附件（客户端这一半）

邮件用的是 [mailbox 业务模块](../modules/README.md)（生成时由 `roost module add mailbox` 装进工程，连同它依赖的 wallet 模块；
`codegen/internal/roost/demo.go` 的 `demoModuleSteps`）。列表与领取两条协议、领取账本、附件的编码与种类、发件入口和它们的场景测试
都是模块的，demo 不带自己的 `ListMail` / `ClaimMail` / `ClaimMailReward`，也不覆盖模块的任何文件。demo 自己写的只有“发什么”：
`game/rewards/`（升级值一瓶法力药水，挂在热补丁点后面）与四个发件方。

```
MailboxList 端点  ─ List(playerID, cursor, limit)   ─▶ mail 服务（按认证玩家作用域，服务端不需要端点再查归属）
MailboxClaim 端点 ─ ReserveClaim(mailID, "")        ─▶ mail 服务：交出附件 + 对 (玩家, 邮件) 恒定的 token
                  ─ Sync_MailboxClaimReward(...)    ─▶ Nest 锁 Player 的一笔事务：奖励 + **邮件 id 记进账本**，同一条 WAL 记录
                  ─ CommitClaim(token)              ─▶ mail 服务：标记已领；重试同 token 幂等
```

- **附件是（种类，号，数量）**：线上格式 `{"type":"item","id":1002,"amount":1}`，金币是 `{"type":"coins","amount":100}`
  （`game/mailbox/mailbox.go`）。种类是登记点：金币内置，经 wallet 模块到账；道具（`item`）由 mailbox 与 bag 的联动登记
  （`game/mailbox/mailbox_item.go` 登记校验，`game/entities/player/mailbox_item.go` 登记发放：`BagComp().Add`，与领取账本同一笔事务）。
  demo 发的奖励邮件带的都是道具。回包与列表用同样的字段说附件：`AttachmentType` / `AttachmentID` / `Amount`，领取回包另有
  `Held`（到账后这个道具在背包里的数量；金币是余额）。
- **发件方都走 `gamemailbox.SendTo`**（`game/mailbox/mailbox_send.go`）：给一名玩家发一封信，附件先按领取端的同一个判断校验
  （发不出去的不进邮箱，返回 `ErrLetterInvalid`，重试无用），幂等键必填。demo 的四个发件方只差标题、附件与幂等键：
  升级奖励（`level_up_mail.go`，键是 EffectID）、GM 补偿（`gm.go`，键是命令的 trace id）、活动结算（`activity.go`，
  键是“活动 id + 玩家 id”）、送礼的送达（`gift_saga.go`，键是 saga 步骤的幂等键）。
- **领取是恰好一次的**：中间那一步把邮件 id 和奖励放进同一条 WAL 记录（`MailboxComponent.Claim`，账本是 `PlayerDao.MailboxLedger`）。
  三次调用不可能合成一个事务——邮件在另一个进程——所以窗口是真的：发放成功、`CommitClaim` 丢了、预留到期、重试用同一个 token 再预留一次。
  邮件服务只能把它算作重复**尝试**（它无从知道游戏发没发），游戏这边知道：第二次发放在账本上撞到自己，什么也不做，回的还是同一组数字。
  没有账本时这里就是第二叠道具——模块的 `TestMailboxSameMailArrivingAgainPaysNothing`、`TestWithMailboxBagItemMailLandsInTheBagOnce`
  与端点一级的 `TestMailboxClaimEndpointRetryAfterLostCommitPaysOnce` 把这条钉住了（去掉账本判断即红）。
- **账本清理必须配合执行时准入**：预留结果携带的 `ExpiresAtUnix` 决定何时不再允许新预留，但过期前取得的预留仍可能晚到（Nest 排队、Player 冷加载、重试）。`MailboxClaimReward` handler 的 `nowUnixMilli` 是执行时这条消息冻结的业务时间（生成的 Nest 分发包装从 `fctx.Now` 填，调用方不传时间；DESIGN-TODO D22-1），按它判 `gamemailbox.ClaimWindowClosed`（与邮件服务同一边界 `now >= ExpiresAtUnix`）：已在账本里的只确认、不再发奖；不在账本里且已过期的拒绝，错误码与 `ReserveClaim` 拒绝过期邮件相同（`mail: mail has expired`），判断在任何 DAO 写入之前。账本在过期后再等 `gamemailbox.LedgerPruneGraceSeconds`（10 分钟）才清理（`gamemailbox.LedgerMayForget`）：宕机重启后 Player 可能换到业务时钟稍慢的进程（App 允许启动时比高水位慢 1 分钟），那里在途的旧请求还看得到窗口开着，只能靠账本还记得。没有到期时间的发放请求直接拒绝（`mailbox_invalid`；mail 服务要求每封邮件都有过期时刻，真实的预留不会走到这里），所以账本里不会有清不掉的记录。不能用固定保留期猜测其他服务的最长邮件寿命，论证写在 `game/mailbox`；也不能仅凭一次 `CommitClaim` 成功就删除去重记录。回归是模块的 `TestMailboxExpiredFirstGrantIsRefusedAtExecutionTime`、`TestMailboxLateReplayAfterItsRecordWasPrunedIsRefused`、`TestMailboxRecordOutlivesTheExpiryByTheGrace`（`game/handler/mailbox_test.go`）。
- **仍然开着的**：预留成功但发放前崩溃（邮件被占到租约到期，玩家等，什么都没丢）；`CommitClaim` 丢失（账本挡的是重复**发放**，不是重复尝试）。
- `Claimable` 是“现在领会成功吗”：mail 服务说可以预留，并且附件是这一版发得了的奖励（解得出来、种类登记了发放方式）。
  客户端不用从 Status 猜（Status 表达不了"被在途投递占着"）。
- 附件这一版发不了（解不出、种类没登记）时答 `mailbox_attachment`，发放被拒（背包放不下是 bag 模块的 `bag_stack_full` / `bag_kinds_full`）
  时原样答那个码；两种情况都 `CancelClaim` 归还预留，邮件保持未领、不会卡在"held"直到租约到期。
- **登录时拉全服邮件是进入钩子**：mailbox 与 enter 两个模块的联动（`game/controllers/mailbox/mailbox_enter.go`）在进入游戏时调
  `PullBroadcasts`，demo 的进入流程里不再手写。画像规则是模块的 `BroadcastProfileOnEnter`：demo 的登录端点把会话的 claims
  带给进入钩子（`gameenter.Entered.Claims`），规则从里面读认证器写下的三个键——注册时间取 account 的角色创建时间
  （`RoleCreatedAtClaim`），范围是承载服（`ServerIDClaim`），别名合服后原服不同的角色另带原服（`OriginServerIDClaim`）。
  `internal/access/player/tcp/auth_test.go` 用真实的认证器产出 Principal、交给这个函数读，钉住两边的键对得上。
- **金币**：装 mailbox 连带装进了 wallet 模块（`PlayerDao.Wallet` 是同步字段、`WalletLedger` 与 `MailboxLedger` 只落库、协议 `WalletSpend`）。
  同步视图里 `Wallet` 只给本人：普通场景的 default 视图（全部同步字段）与分区场景的 `self` 视图带它，
  别的玩家（near / team / far）的字段白名单里没有（`game/entities/player/sync_packer.go`）。
  demo 的玩法不产出金币；GM 可以发金币附件（`gm.mail.send` 的 `attachment: {"type":"coins","amount":100}`），领取后进 wallet 的余额，`WalletSpend` 能把它花掉。
- 机器人脚本：add_exp 之后 `retry × 20 { wait 250ms; mailbox_list }`（效果链是异步的：WAL → outbox → JetStream → consumer → mail.Send），
  拿到第一封可领邮件的 id 再 `mailbox_claim`，断言领到的是道具、背包里这个道具的数量 ≥ 领到的数量；`mailbox_claim_replay`
  再领同一封，断言数量不再增加。

## 跨服务：game → match 组队

match 服务是通用的：队列由 `Queue{Mode, GroupSize, Partition}` 定义、subject 的 kind 对它不透明，它负责的是队列本身与
`Commit` 的原子性——**谁和谁一组不是它决定的**。`Candidates` 交出等待中的票，`Commit` 一次 CAS 成组；决定权是游戏策略，
所以住在 game 进程：

```
JoinQueue 端点 ─ Enqueue(队列, subject, requestID=帧序号) ─▶ match 服务（typed servicerpc，经生成的 Match() 客户端）
internal/service/game/matchmaker.go  每 500ms：Candidates → Grouping.Group → Commit → Sync_RecordMatch(World)
PollMatch 端点 ─ Ticket / Match ─▶ match 服务（带 subject，服务端校验票的归属）
```

- `game/matchmaking/queue.go` 是游戏对 match 说的话：duel 队列两人一组按到达顺序（`FirstComeGrouping`），ranked 队列按等级配（`ScoreWindowGrouping`：
  等级差 5 以内立刻配，窗口随最老候选的等待每秒放宽 5，上限 50）；ticket 的 Score 在 JoinQueue 时经 profile 模块的 `ProfileCard` 读 handler 取（Player 锁内，名字与等级），
  PollMatch 带 `mode` 选队列。subject kind 是 `player`。matchmaker 每 500ms 扫两个 pool，各用自己的策略。
- 所有对 match 的调用都从端点或普通 goroutine 发出，**从不在实体锁里**——World 只在 Commit 成功之后经自己的 Nest handler 记一笔。
- `configs/service/config.match.yaml` 的 `sweep_queues` 列出 duel 队列：match 进程只负责扫过期票、移除超过 `match.retention` 的已结束记录，不负责成组。
- `Grouping` 是调用方的工具，不是 match 服务的配置：match 的 Wiring Mod 不接受 `Grouping` collaborator。demo 的 matchmaker 直接调 `FirstComeGrouping{}.Group`，这是那个接口唯一的用法。

## 跨服务：game → chat 聊天

聊天不是 demo 自己的代码：它是 [chat 业务模块](../modules/README.md)，`-template game-demo` 生成时由 `roost module add chat` 装进来（`codegen/internal/roost/demo.go` 的 `demoModuleSteps`），别的工程装同一个模块得到同一份实现。demo 在它之上只做一件事：登录时把玩家加进在线名单并发一条公告（`game/controllers/player/enter_game.go`）。

chat 服务是通用的：它按频道存带序号的消息、执行这个工程交给它的策略；哪些频道存在、一句话长什么样、谁在线能收到，
是游戏的事，住在 `game/chat/`（game 进程与 chat 进程都 import 它——这就是两边对 `text` 这个消息类型名达成一致的方式）：

```
ChatSend 端点 ─ Publish(sender=认证玩家, requestID=帧序号) ─▶ chat 服务（typed servicerpc）
              └─ deliver：world 频道推给本进程在线名单里的每个玩家（含发送者），private 推给双方 ── ChatMessage 推送
EnterGame 端点 ─ Chat().AnnounceWorld("player N entered") ─▶ chat 服务的特权入口 PublishSystem ── 同样 deliver
ChatHistory 端点 ─ History(viewer, after_seq, limit) ─▶ chat 服务（服务端校验读权限）—— 重连补读路径
```

端点在模块自己的 controller 域 `game/controllers/chat/`，协议号在安装时从 `roost.yaml` 的 `ids` 段分配（记在 `modules.chat`）。

- `internal/service/chat/collaborators.go`（模块替换了生成的拒绝桩）：策略写成决定——world（只有 world 1）与 private 对所有认证玩家开放，group 拒绝（没有队伍；
  公会群聊没接，接时成员来源是 alliance 服务的 Mirror 与成员变化通知），system 频道只读；`Bodies()` 注册唯一的 `text` 类型，校验函数与端点共用（端点先校验，免一次总线往返）；
  `System()` 无条件 `GrantSystem()`——这是对部署的陈述：chat 只经内网 NATS 可达，没有端点转发 PublishSystem。总线对不可信一方可达的部署要换成查调用者身份。
- **推送是捷径，序号是依据**：每条投递都带 `Seq`，错过推送的客户端用 `ChatHistory(after_seq)` 补读——机器人脚本就是这么写的
  （它自己那句的推送可能先于响应到达而被丢，所以 `wait_push` 外面套了 selector，再用 history 断言）。
- `Presence` 是**每进程**的在线集合：EnterGame 加入（用过聊天的玩家也算在线），最后一条连接关闭、或推送失败且没有活动会话时移除。
  连接关闭的订阅由 chat 的 controller 在第一次取推送面时挂上（`game/controllers/chat/chat_push.go`），服务的 `Init` 里没有聊天的接线。
  多个 game 进程的部署应改为订阅 chat 的流或走同步总线（syncbus），而不是从内存扇出。
- 机器人 transport（`loadtest/playertcp/conn.go`）靠帧头的 server-push 标志位识别推送——服务端给推送编的是自己的会话序号，与客户端序号同起点，
  只看序号会把一条世界频道推送当成正在等的响应。

## 跨服务：game → session 副本

session 服务是通用的"有界 run 原语"：每个 owner 同时只有一个活 run、Enter 按 RequestID 幂等、run 有截止时间、附着的资源恰好释放一次。
什么算一局、清了给什么，是游戏的事：

```
EnterDungeon 端点  ─ Enter(playerID, {Kind, RequestID=帧序号}) ─▶ session 服务（typed servicerpc，经生成的 Session() 客户端）
FinishDungeon 端点 ─ Finish(playerID, runID, succeeded|failed, outcome) ─▶ session 服务：状态机 + 释放（调这个工程的 Release()）
                   └─ 清了：MultiSync_AddExp(Player, World, 100) —— 与 AddExp 端点同一笔两实体事务，够升一级，于是又走一遍升级 → 奖励邮件
```

- `internal/service/session/collaborators.go`：`Release()` 是 session 服务对每个附着资源恰好调一次的钩子；demo 的副本不占外部资源，所以是一行日志 + nil。
  真实游戏在这里释放实例 / 座位，返回 error 会让服务保留待释放并在下次 Enter / sweep 重试。
- 第二局要等第一局结束：一个 owner 一个活 run 是服务的契约，重复 Enter 得到 `ErrAlreadyRunning` 的 coded 响应，不是第二个 run。
- **发奖是恰好一次的**：`ClaimDungeon` 事务把 run id 与经验、World 计数放进同一条 WAL 记录（`game/handler/claim_dungeon.go`），
  重放找到账本里已有的 run id 就什么也不做。判发奖的依据是 `session.Finish` 返回的 `run.State`，不是请求里的 `Success`——
  Finish 对终态 run 幂等返回，"调用成功"不等于"这次调用结算了它"。
- **账本有期限，期限与"还能不能领"是同一条规则**：账本记的是 run 的**结算时刻**（`run.FinishedAtUnix`，session 服务盖的章，
  不是 `time.Now()`），准入用 `dungeon.ClaimWindowClosed`，清理用由它推出的 `dungeon.LedgerMayForget`（晚一个宽限
  `LedgerPruneGraceSeconds`，理由同邮件账本），准入时间是事务执行时重新读的业务时钟（与调用方时间取较大值），不是端点排队前
  读到的那个。于是"记录被清掉"蕴含"这个 run 已过窗口、会被拒"，
  不依赖 session 服务保留多久——**succeeded 的 run 在 session 服务里没有存储 TTL，可以被 Finish 到天荒地老**
  （`run_ttl` 管的是 run 能开多久）。账本保留期不能按"反正重放不了"定短：另一笔领取顺手清掉旧记录之后，
  重放旧 run 就会再拿一份。
- **过窗口的领取是一条有名字的拒绝**（`dungeon_claim_window`，端点打 Warn 日志），不是静默的 0 ——
  真的有玩家一直没领而窗口过了，这件事必须让运营看得见，补发是运营的口径。把重复发奖改成静默漏奖只是换了个 bug。
- **Finish 与发奖仍不是一个事务**：进程死在两步之间，run 已终态、exp 没给；这是安全的方向——重试 FinishDungeon 会拿到同一个
  succeeded run，账本还没记，下一次尝试就补上了。生产要补的是"重试不靠客户端的善意"。
- session 进程默认不扫过期 run（`sweepOwners` 返回空并在日志里说明）：过期 run 由同一 owner 的下一次 Enter 懒解决。要及时释放资源的部署自己接 owner 列表。

## 跨域事务：送礼 saga（debit → deliver，失败补偿）

送礼把一个道具从 A 的背包挪进 B 的邮箱：背包是 A 的 Player 上的 Nest 事务，邮件是对 mail 服务的调用——两个事务域，
所以是 saga（`roost add saga GiftItem -service game -steps debit,deliver` 的产物，`saga/gift_item/definition.go`）。

- **开始**：`SendGift`（10013）→ `StartGift` Nest 事务：在 A 的锁内检查背包够不够，然后 `saga.EmitStart`——start 意图和这次事务同一条 WAL 记录，
  Data Engine outbox 把它送到协调器（saga Mod 在 game 进程里，`saga.start` 效果）。saga id = 发送方 + 完整会话的 SHA-256 摘要 + 帧序号；同一请求得到稳定 ID，重连后的同一帧号属于新请求。不能截断会话前缀：demo 会话以 `demo-玩家ID-时间戳` 命名，前缀可能完全不包含时间戳。
  这步不动背包：检查是建议性的，道具在 debit 之前被花掉，debit 会以同一个 coded 错误拒绝，saga 以 failed 结束、无事可补。
- **步骤**（`internal/service/game/gift_saga.go`，四个步骤，**两种形状**，登记在框架的步骤运行时 `kitsaga.Steps` 上：
  收件箱、durable、等待投影、停止与指标都是框架的，demo 只写步骤体与属主核对）：
  - debit（`GiftDebit`，`BagComponent.Remove`，不够是 `bag_short`）与它的补偿（`GiftRefund`，`BagComponent.Add`）走**原生路径**
    `Steps.Native`：步骤体把框架的 `saga.NativeStep` 交给 Nest handler，handler 在自己的事务里 `step.Complete(...)`，
    于是背包变更、命令回执、协调器等的完成结果是**同一条 WAL 记录**。重投会撞上回执、回放已存的完成结果而不是再扣一次；
    提交前崩溃则三样都没发生。消费者自己不发布任何东西——它等回执被投影出来再 ack。
    这两个 handler 声明 `durability=pipelined`（原生步骤的推荐写法）：WAL 记录入队后就放 Player 锁，结果仍在落盘之后才外发；
    框架把声明 pipelined 的原生步骤自动加进 Nest 的 pipelined 白名单并只为它们打开异步完成，等 WAL 不占 Nest short 池的 worker，
    配置里不用写 `nest.pipelined.*`（见 [Saga 契约](../docs/framework/SAGA.md)「原生步骤的提交方式」）。
  - deliver（查 Player 集合确认收件人进过游戏，再 `mail.Send` 带附件）的业务是一次 bus 调用，不是 Nest 事务，没有东西可以绑，
    收件人查的是 Player DAO 所在的物理库和集合：DAO 标记的 `db=main` 是逻辑库名，启动时经 `kitdataengine.DAOCollection` 按 `dataengine.databases` 换成物理库（生成配置为 `main: planet`），不是 `dataengine.database`（它只给原生步骤的 inbox 用），也不能把 `db.PlayerDaoDBName` 直接当库名。
    所以登记为 `Steps.Mongo`：Mongo inbox 与原生步骤同一套操作实例契约（已有结果的尝试回放、在途尝试等待、过期接替，
    见 [Saga 契约](../docs/framework/SAGA.md)），但它只管写进 handler 那笔 Mongo 事务的东西；邮件是 bus 调用，不在事务里，
    被 fence 中止的尝试可能已经发出邮件，所以 mail 服务自己的幂等仍是**必需**的（Send 按 RequestID 去重，RequestID 就是命令的
    IdempotencyKey）。deliver 的补偿什么也不做（邮件不撤回）。
  - 这条分界是规则不是权宜：原生路径给"业务本来就经 Nest 提交"的步骤用；拿它去包一次跨服务调用，等于把回执绑在一个
    并不包含那次副作用的事务上。
  - 业务拒绝也**提交**：不动数据，只写回执和一个失败的完成结果——协调器听不到的拒绝会让 saga 空等到 deadline。
    只有基础设施错误才返回 error（回滚，让投递退避重试）。
- **在哪个进程执行**：debit / refund 改的是发送方的 Player，只在发送方绑定的 sid 上执行。`StartGift` 把发起进程的 sid
  写进 saga 状态（`from_sid`；能发起赠礼的玩家一定在本服服务，也就一定绑定在这个 sid 上），同时把它作为 saga 的路由地址
  （`StartRequest.Route = saga.SidRoute(from_sid)`）。debit / refund 是**路由步骤**（定义里 `Routed: true`，消费者
  `kitsaga.WithSidRoute()`）：协调器只把它们投给 `from_sid` 那个进程的 durable（`game-gift-debit-<sid>`），别的进程收不到。
  属主收到后准入（`kitsaga.WithAdmit`）仍核对绑定：不在本服就退回协调器并指明发送方的 sid（`saga.BounceStep`，不执行、零写入），
  副本正在卸载就在本服稍后重试；发送方离线、内存里没有副本也照常执行。没有 `from_sid` 的载荷不能路由：启动被拒绝，万一到了步骤
  也被拒收（`saga.RejectStep`）并记 Error。
- **状态**：`GiftStatus`（10014）经 saga Engine 读记录。刚发完轮询会得到 `unknown`——start 意图还在 outbox → 协调器的路上；非本人的 saga 也是 `unknown`。
  终态：`completed` / `compensated` / `failed` / `manual_required`（补偿自己也拒绝了，例如退回时叠加已满——运维在 `gm.saga.list` 里看到并决定）。
- **机器人**：送给自己（背包 −1）→ 轮询到终态 → `expect_gift completed` → 领邮件（背包 +1）；再送给玩家 1（从未进游戏）→ deliver 拒绝 → debit 补偿 →
  `expect_gift compensated`。两条路都在 `loadtest -count 6` 里跑。
- **边界**：原生收件箱的租约必须长于消费者的 `AckWait`（框架取 2 分钟 vs 30 秒，`kitsaga.NativeLeaseDuration`），否则租约会在消息还没 ack 时过期、让第二个进程开始同一条命令；
  消费者会当场拒绝这种配置。载荷解不出来的命令没有事务可以承载拒绝，只能靠重投与 deadline 收场。
  deliver 那条仍然是"两次提交"：mail 服务的去重是第二层，不是同一条记录。

## 跨服务：公会 = alliance 服务（写走 RPC 或 saga，读走 Mirror）

公会由独立的 alliance 服务独占（[联盟服务方案](../docs/maintenance/FEATURE-2026-11-04-alliance-service.md)）：`Alliance`（联盟）与
`AllianceMember`（每个玩家的成员守卫）是 Remote 托管实体、`service=alliance`，只有 alliance 进程能认领、能写。game 进程装着
`remote_entity`，但只为它自己拥有的一种 Remote 实体——分区场景的组队 `Team`（`service=game`，无缝大世界原型，见
[方案 §14](../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#14-合入-main2026-11-04)）；联盟两种 kind 对它是“别的服务的”
（生成的 `ForeignKinds`），它的 Remote 运行时对它们拒绝认领与写（`entity.ErrRemoteKindNotOwned`），读经同一个 Mod 提供的 Mirror。
`uses_rpcs: [alliance]` 让它拿到生成的 `rpcalliance` ClientMod（路由查找经 `RouteSource`）。所以 game 改公会只有两条路：

- **只改公会**——`join_guild`（10022）、`leave_guild`（10027）：一次路由 RPC（`Join` / `Leave`，发往联盟的 owner 实例），
  请求号 = 玩家 + 会话 + 帧序号（`guild.RequestID`），同一帧重发由服务端回执回放；答复带联盟新状态、版本与处理实例
  `ServedBySID`。成功后端点在玩家自己的 Nest 事务里改显示字段（`ShowGuild`）。
- **公会与玩家背包一起改**——`found_guild`（10021，建盟费 `guild.found_cost`，金币是道具 9001）、`donate_guild`（10028）、
  `claim_guild_reward`（10029）：两个事务域，各一个 saga（`saga/alliance_found`、`alliance_donate`、`alliance_reward`）。端点
  `StartGuildOp` 在玩家锁内做建议性检查后 `saga.EmitStart`，立即答“已受理”与交易号（`SagaKey`），结果用 `guild_op_status`（10030）查。
  步骤在 `internal/service/<game>/alliance_saga.go`：Player 一侧是**原生步骤**（`GuildDebit` / `GuildCredit`，`durability=pipelined`，
  回执在 `PlayerDao.SagaReceipts`，按 sid 路由到玩家绑定的 sid、属主核对与送礼相同）；公会一侧是 **Mongo 步骤**，步骤体调 alliance RPC，
  请求号 = 命令的 `IdempotencyKey`。

  | saga | 正向 | 补偿 |
  | --- | --- | --- |
  | 建盟 | ① 扣建盟费（原生）② `Found`（Mongo → RPC） | ① 退费；② 迟到成功时保留联盟、记日志 |
  | 捐献 | ① 扣金币（原生）② `Donate` | ① 退费；② 迟到成功时保留捐献、记日志 |
  | 领奖 | ① `ClaimReward` 在守卫上记本周期已领（Mongo → RPC，奖励由服务答出、写进 payload）② 发奖（原生） | ② 背包满被拒 → 补偿 ①：`ReleaseReward` 只撤这一次领取；② 的补偿把奖励扣回 |

  公会步骤按 RPC 的四类答复回应：成功 → 下一步；业务拒绝（名字非法、已在联盟、不是成员、本周期已领）→ 不可重试的失败，
  协调器补偿已完成的步骤（建盟被拒即退费）；“没执行”“结果未知”→ 返回错误，步骤回滚重投、同一请求号再问一次，
  真执行过的由服务端回执答出，不会退了费又建成盟。
- **读**——`guild_info`（10023）：“我的公会”取玩家自己的副本 `PlayerDao.GuildID`（`PlayerGuild` 读 handler，玩家锁内读），
  再读 `AllianceMirror`（`//roost:mirror` 只读 DTO，形状与 `alliancerule.View` 相同），用 Monotonic：缓存未命中时会回源加载
  （Cached 不加载）；客户端带上写答复里的 `Version` 作 `MinVersion`，读到的不会比自己刚写的旧，镜像还没追上时答 `guild_busy`、
  稍后再查。副本与 Mirror 只用于展示与预检（例如“已在公会”直接提示），写判断一律在服务里按权威（名单）做。

守卫 `AllianceMember` 不同步给 game 进程（`snapshots=none`：它的提交只写守卫文档，没有 Remote 快照、L2 条目与广播）；game
只镜像 `Alliance`。“玩家在哪个公会”是 `PlayerDao.GuildID`（`persist,sync`，附近玩家头顶也显示）：玩家自己的入会 / 退会 /
建盟成功后在玩家事务里写（`ShowGuild`；建盟在 `guild_op_status` 看到 saga 完成时写），在线时收到成员变化通知更新，登录时
（`EnterGame` 的 `refreshGuild`）对照该公会的 Mirror：公会不存在或名单里已没有这名玩家（离线时被踢 / 退出的通知没收到）就清掉；
副本为 0 时向 alliance 问一次（只读 RPC `Membership`，按玩家守卫路由；守卫被占就先自愈再答），答出公会就在玩家事务里写回——
离线时被批准入会的玩家下次登录就能看到公会。这次查询在登录流程的 RPC 阶段做（不在快池里等），超时 1 秒；失败只记日志、副本保持 0，
不影响登录。
这条路径有一个机器人场景 `guild_offline_join`（`loadtest/scenarios/demo.yaml`，`make dev-smoke` 的第二轮）：机器人建开放公会，
第二个身份登录一次后断开，运维命令 `gm.player.join_guild {player_id, guild_id, request_id}`（game 进程，按玩家代发 `Join`；
本进程仍有该玩家会话时不入会、答 `joined=false`，机器人重试它，所以“已离线”是一个事实而不是等出来的）把它拉进公会，
再登录后“我的公会”必须就是这个公会。
通知是 alliance 写答复成功之后由**服务层直接**按成员的原服 sid 经 Bus 发的 `alliance.member_notice`（不经 effect；
`internal/service/<game>/alliance_notice.go`）：玩家在本进程在线才处理——改副本（离开只清“正显示这个公会”的，晚到的旧
退盟不会盖掉新公会）并推 `GuildNoticePush`（10104）；至多一次，丢了不重发、只记日志与 `alliance.member_notice_lost_total`。

alliance 服务不产生、不消费任何 effect（2026-11-05 晚起）：守卫是名单的缓存，只允许“过时地占着”、绝不“过时地空着”。跨实例两步写成功后服务直接调
`ResolveGuard` / `ClearGuard` 收尾（尽力而为）；丢了，下一个碰到守卫被占的请求（Join / Found 的预约、`ClaimReward`、登录查询）先向持有它的
联盟问权威结论（`Settle`：预约过了期限才定论为过期，否则只问不写），再用条件守卫事务落下结论后照常判断。预约的期限由持有守卫的进程在内存里
排一个到期回调（预约成功时排、收尾时撤），到点做同样的自愈；进程重启丢了它，就退回“下一个碰到守卫的请求自愈”。跨实例宣战：宣战方在
自己的事务里记下“等对方确认”，答复后服务直接调对方的 `AcknowledgeWar`（按请求号幂等），拿到最终答复才条件清掉标记；没拿到按退避重试，
进程重启时启动扫描补发（后台跑、不挡起服，每页 8 个联盟并发读：每个读都要向所有权记录取一次写许可，逐个读时 1 万个联盟约 3 分钟才扫完，[基准页](../docs/performance/benchmarks/alliance-warscan-20261106/README.md)）；
扫描之后每分钟再跑一次，只读本进程还没接手的联盟（运行中迁入的、新建的），所以迁移检查与 fencing 之间提交的宣战随联盟迁走后由新 owner 接着问（[联盟服务方案 §2.5B](../docs/maintenance/FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）。

多实例：`sh deploy/dev/second-alliance.sh start` 在同一台机器上起第二个 alliance（sid 1001、ops 9210）。建盟经队列组分到两个实例，
每个联盟归接到它的实例；其余调用经路由登记找到 owner。缩容一台：在它的 ops 上 ① `alliance.stop_founding`（退出 `Found` 的队列组，
新联盟只落到别的实例）② 反复 `alliance.transfer {"count": N, "to_sid": <另一个 sid>}` 直到它什么都不拥有（按 owner 枚举本实例持有的；一次调用在 `ops.admin_timeout` 前停下时答 `"more": true`，接着再调）③ 停进程。
公会的 GM 读写（原 `gm.guild.*`）改为 alliance 进程自己的 `alliance.info` / `alliance.guard`，按权威读。

## 地图：Scene 是一个实体，它的 system 各自带锁

`game/entities/scene` 是地图实体：有 id、有框架驱动的生命周期、GM 能寻址、将来能跨进程；`noPersist=true lifetime=runtime_rebuild`，
因为一张地图每次启动都是从配置重建出来的。它唯一的 DAO `SceneDao`（`//roost:dao nocoll`，全部 `nopersist,sync`）放场景时间域的三个参数（下文“场景时间”）。

```
Scene 实体 ─ OnInitFinish ─▶ sceneruntime.Runtime{ terrain, path_find, refresh }   按序 Start，逆序 Stop
   │                       ─▶ timedomain.Domain（SceneDao 的锚点与倍率）
   │                       ─▶ 心跳 "scene.spawn"（1Hz，场景自己的锁里决定刷怪）
   └─ Terrain() / PathFind() / Space() / TimeDomain()  →  接口，不是实现
```

- **system 自带锁，不借实体锁**。地形查询来自端点、来自成员的心跳、来自 AOI tick；让它们都去拿 Scene 的实体锁，
  等于让地图成为这个场景里所有事情的瓶颈。实体锁排的是**实体状态**的事务顺序，"这里能不能站"不是那个问题。
- **位置的权威在 DAO，别处不缓存**。读位置 → `MapComponent.Pos()` → DAO；写位置只经 `movement.MapBasic`（`MapComponent` 持有）→ DAO，
  同一次写在事务里把新位置排进 AOI（`Space.QueueMove`，走提交门：回滚的一步不进 AOI）。AOI 的 id→坐标索引是索引不是第二个真相，
  永远不被当作"X 在哪"的答案读出来。两个可写的真相就是重连之后玩家出现在别处的那种 bug。
- **位置不每拍落库**（D20 决定 ⑮）：`PosX/PosY/SceneID/Facing` 是 `nopersist,sync`；落库的是 `SavedSceneID/SavedPosX/SavedPosY/SavedFacing`，
  由 `MapBasic` 在离开场景时与在场景里每 10 秒（`actor.SavePeriod`，心跳 `"map.save"`）写一次、场景 / 位置 / 朝向都没变不写；进场景时
  以保存的位置为首选点、恢复保存的朝向。朝向和位置是同一份 MapBasic 数据（维护者 2026-10-22）：只转向也算变过，回滚随 DAO。
- **玩家不占地**（`Walkable` 而不是 `Occupy`）：两个玩家可以站在同一点，于是断线也没有残留占位要回收。占位留给真正会挡路的东西
  （地图里的墙、将来判定为实心的怪）。
- **落点用 `Place` 向外一圈圈找**：地图每次重建，玩家记住的位置可能不再可用；返回最近的可站点，而不是"某个"可站点。
  登录不会因为地面变了而失败。
- 拒绝只有一个码（`scene_position`）：越界、被占、不在地图上、到不了共用它——能精确知道哪些点被占的客户端，等于拿到了所有人的位置。

## 移动：客户端发目标，服务端在实体自己的心跳里走（D20）

```
move_target(x, y | follow) ─▶ handlerMoveTarget（Scene + Player 两实体事务）
        PathFind.Path 找路（只此一次）─▶ Behavior.CreateAction(MoveTo{path, 60 格/秒})   ← 最后一步
                                                │  BeatRunner 给移动组挂心跳 "actionflow/1"（200ms）
每一拍（玩家自己的 Nest worker、实体锁与事务内）：
        Move.Advance(Beat.Elapsed) ─▶ MapBasic.SetPos ─▶ DAO PosX/PosY/Facing + Space.QueueMove ─▶ 提交 ─▶ AOI ─▶ 帧
到达 / 被新目标取代 / 被瞬移结束 / 被挡 ─▶ 动作结束，移动组的心跳停
```

- **客户端只发意图**（`MoveTargetRequest{X, Y, Follow}`，msg 10026）：回复只说动作开始了、从哪里开始；路上的每一格都作为
  复制的 `pos_x / pos_y / facing` 到达客户端，客户端渲染复制下来的位置即可。`Follow` 跟随另一个玩家（玩家 id），走到 3 格以内结束。
- **不追帧**：每拍走“速度 × 本拍经过的时间”，掉拍后下一拍一次走完；空闲的玩家没有任何心跳登记，走路的玩家一个。
  一个进程里成千上万人走路时，心跳按实体 ID 的哈希打散在周期里，没有“全服每 200ms 一起走一步”的尖峰。移动频率取 `movement.DefaultMovePeriod`（200ms，5Hz；MMO / SLG 一般 2～5Hz），走完一段的时间只由速度决定。
- **寻路只做一次**：`PathFind.Path` 有 4096 节点的预算（开阔地上远处的斜向目标会用完它）；用完时改走直线，`Move` 每进一格
  问一次地形（`actor.Ground`），被挡就停在最后一个可走的格（动作以 `Blocked` 失败结束）。证明到不了的目标直接拒绝。
- **按点移动保留为瞬移**（`move`，msg 10017）：一次校验过的写位置，结束正在走的路；它是服务端驱动之前的写法，留作对照与闪现 / GM 用。
  AOI 同样由 `MapBasic.SetPos` 的 `QueueMove` 跟上，端点之后不再调 `Scene.Moved`（已删除）。
- **runner 不随事务回滚**：handler 先做完全部会失败的校验，最后一步才 `CreateAction` / `EndCurAction`（D20 P2 决定 3）。
- **心跳驱动手写接线**：`internal/service/<game>/heartbeat.go` 在 `Service.Init` 开头 `Bind`（Nest 客户端 + 只读实体查找）并 `Start`
  `actor.Heartbeat`，与 World 定时器驱动同一种写法；生成工程用 `wiring/heartbeat` Mod。`Shutdown` 一开始就停驱动，Nest 还在。
- **共用的东西在 `game/actor`**：驱动、动作种类与注册表、状态目录、`InterestMoves`（成员进场景用的 `movement.Space`：只报告移动，
  进出场景仍由复制桥的 `Join` / `ShowSubject` 负责）、`Ground`、`WorldTime`。
- 测试：`internal/service/<game>/scene_movement_test.go` 在真实 Nest + 运行中的心跳驱动 + 复制桥上，用手动推进的业务时钟
  （`clock.SetSource`）逐步核对“走了几格”：发目标 → 每拍推进 → 到达 → 观察者看到每一步；新目标取代旧的；瞬移结束走路；
  超出寻路预算走直线、被墙挡住；怪物巡逻、定身、刷怪补齐；2 倍速。断言提交后的状态先 `settle`（同实体一条空消息），
  组件状态在实体锁里读（`locked`），不 sleep。

## 怪物：巡逻、定身与状态门控（D21）

`game/entities/monster/actor_component.go` 把框架的组件按游戏会用的方式接起来，全部跑在怪物自己的锁与心跳上：

| 组件 | 做什么 |
| --- | --- |
| `ai.Controller`（`ai.NewBeatController`，心跳 `"ai"`，500ms） | `actor.Patrol`：移动组空闲且没被冻结时，走向下一个角（出生点为一角、边长 12 的正方形，按地形裁掉不可走的角） |
| `actionflow.Behavior`（心跳 `"actionflow/1"`） | 执行 MoveTo，每拍推进 |
| `movement.Move` / `MapBasic` | 走路，位置写 `MonsterDao`、排进 AOI；怪物不落库 |
| `state.Set` + `state.Gate` | `Rooted` 生效时冻结移动组（`FreezePause`：正在走的路停在原地，心跳停；解除后接着走） |
| `combatcomponent.CombatComponent` + `BindStates` + `AttachBeat`（心跳 `"combat"`） | 定身 buff（标签 `root`）是唯一的持有者，`Rooted` 从标签派生，不复制；限时 buff 由这个心跳到期 |

- **策略不问“我是不是被定身了”**：门控是这条规则唯一的位置；策略只看移动组是否空闲。
- **客户端看得见**：`Rooted` 经状态观察者镜像进 `MonsterDao.Rooted`（`nopersist,sync`），观察者在改 buff 的同一个事务里写，随它回滚。
- GM：`gm.scene.root {"monster_id", "rooted"}` 加 / 驱散定身 buff（handler `handlerRootMonster` 只写 buff，其余跟着来）。
  `"seconds"` 给了就是限时定身：buff 时长按技能 tick（`actor.SkillClock`，100ms）计，由怪物自己的 buff 心跳（`combatcomponent.AttachBeat`，`"combat"`，1Hz、场景时间，有限时 buff 时才挂）到点移除——快池上的周期逻辑一律走心跳，没有扫全部怪物的循环。
- **没做**：追击与回巢、怪物占地、战斗伤害（只有 GM 的“杀掉”）。

## 场景时间：副本 2 倍速（D22）

场景实体持有 `timedomain.Domain`，参数在 `SceneDao`（`nopersist,sync`：随进程结束的副本不落库，但下发给客户端）。

- **成员跟着域时间走**：玩家的 `Behavior`、怪物的 `Behavior` 与 AI 都用 `BeatConfig.TimeDomain: actor.WorldTime()` 构造，
  所以动作每拍的 `Elapsed`、AI 的决策周期判断都是域时间：2 倍速时走路、巡逻快一倍，0 时全场停住。
- **只在场景自己的锁里改**：GM `gm.scene.time_rate {"rate": 0..4}` → `handlerSetSceneTimeRate` → `Domain.SetRate`；新锚点是生效时刻的
  域时间（连续不跳），写 `SceneDao`（随事务回滚），提交后发布给成员。越界拒绝、什么都不改。
- **下发给每个成员**：场景自己是一个复制主体（`sync=true`），复制桥把它放进一个关系视野（`sceneView`），每个 `Join` 把玩家挂上去，
  站在哪里都收得到；C# 客户端 `Roost.Client.SceneTimeDomain` 用同一公式按估计的服务端业务时间外推。
- **一个进程一张地图**：`actor.WorldTime` / `actor.Ground` 是进程级的，成员在构造时绑定域；多场景的游戏要在进入场景时为成员
  构造行为组件（KNOWN-LIMITS）。

## 服务端权威状态同步：Player 是复制主体，entitysync.Manager 是调度器，scene 是政策

`sync=true` 的实体有一个 **sync 主体**（`Player.Sync()`）：版本、脏掩码、packer。`entitysync.Manager`（进程一个）拥有全部主体与会话，
每 tick 给每个会话一帧；谁订谁由 roost-core 的 `sync/entitysync/policy.Interest`（主视野的距离 + 队伍关系视野）决定；scene 只剩应用层的三件事：
把 TCP 推送接成 `entitysync.Transport`、会话生命周期（`enter_game` 以 held 开会话 → 客户端 `scene_ready` → 离场关会话）、地图尺寸与半径。设计与契约见 [Sync 设计与使用](../docs/framework/guide/04-sync.md) 与 [Entity Sync](../docs/framework/ENTITY_SYNC.md)。

```
Player 的 DAO setter ─ MarkSync(mask) ─▶ Player.PublishSyncDirty() ─▶ 主体标脏 ─▶ Manager 记 pending
                                                                             │  每 50ms 一 tick
主体 ─ PrepareViews(packer：delta 给在线者、快照给新订阅者) ─▶ 按会话聚合成一帧 ─▶ sceneLane.Push ─▶ TCP 推送 10103
客户端：scene_watch 装解码器 → scene_ready（服务端此前 held，不出帧）→ entitysync.Decoder（每连接一个，增量从基线继承身份）→ 合并进本地视图
```

- **写入侧不是自动的**。把 DAO 标脏和把**主体**标脏是两件事：`PublishSyncDirty()` 在一次变更的末尾调一次，
  于是一次事务产出**一条** delta 而不是每个 setter 一条。复制是"一次已提交的变更"的副作用，不是"有人调了 setter"的副作用。
  Nest 事务里还有第二条路：场景的提交观察者在事务准入时按实体收集 setter 留下的同步标记（`TakeEntitySyncChanges`），
  与 `PublishSyncDirty()` 的标记并入同一事务条目、不会双发。业务模块的组件（背包）不认识工程的同步、不调 `PublishSyncDirty()`，
  靠的就是这条路；不在 Nest 事务里的写入（心跳里的移动）没有准入这一步，仍然要自己调。
- **payload 是 DAO 自己的同步文档**（`MarshalSync(mask)`，与 `ApplySync` 成对）。掩码从生成的 setter 来、原样回到生成的
  marshaller，两端都不需要知道哪个 bit 是哪个字段——生成的字段掩码常量是 DAO 包私有的，别的包里的 packer 没法按字段裁剪
  （尚未解决）。要自己的客户端协议的项目在这里换成自己的消息。
- **持久化水位**：`wiring/dataengine` 的 `DurableLSN` 装进 `entitysync.ManagerConfig.DurableWatermark`。流水线提交的部署会在 WAL 落盘前
  就确认事务，把这种内容外发等于让客户端看到服务端还可能丢掉的状态；Manager 会压住整个主体直到水位追上。
- **谁订阅谁不在这里决定**：`policy.Interest` 决定（视野：空间视野按距离、关系视野按成员），并直接说给 Manager；scene 只在 `NewInterest` 里给出地图尺寸与半径。
  加一种关系（好友、同盟）= 定义一个关系视野（`DefineRelationView`，视野 ID 由业务分配，例如联盟号）、维护成员（`SetViewMembers` / `AddViewMembers`），再挂给该看的玩家（`AttachView`）。
- **跟不上就断开**：lane 用框架的 `selfsync.PushPlayerFrame`（自身视图同一条规则）。活着的连接收不下一帧时先关闭该玩家的全部连接，Manager 再关会话并回调 `SessionLost`；
  接入层整体不可用或进程出站驻留预算耗尽是 `ErrRetryLater`，tick 重来、不踢人；玩家没有连接时只关会话。
- **编不出帧也断开**（维护者 2026-10-22 决定 A）：Manager 因编码失败关掉会话（单个实体超过帧上限）时没有连接拒收任何东西，
  `sessionLost` 调框架的 `selfsync.DisconnectLostPlayer` 断开该玩家（原因是推送拒收时什么都不做，lane 已经断过），计 `player.sync.session_lost_disconnect.total{reason}`、打限频 Warn（带玩家 ID 与原因）；
  客户端重连、`Join` 从全量开始。原因一直在（例如一个实体始终超过帧上限）时会反复被踢、重连，靠这个计数与 Warn 发现。
  持有对象超过 `MaxObjects` 不在此列（2026-11-08 起超额降级，方案 D17）：会话不关，多出来的对象暂缓，主视野从最远处收紧。
  会话关闭事件（`OnSessionClosed`）是主路径，入场时按 `ActiveSessions` 的 sweep 是兜底。
- **断线按 Join 代际判定**：每次 Join（首次登录或重连）有新代际；`SessionLost` 与会话关闭事件只登记“当时的代际”，
  在 `transitions` 锁内复查“代际仍是当前且确实没有活跃连接”才离场，否则丢弃。嵌入 TCP 推送给玩家所有连接，重连窗口里**旧**连接写失败而新连接收下时
  接入层只关旧连接、推送算成功，玩家留在场景、会话不动；复制会话被丢之后（推送拒收由 lane、其他原因由 `sessionLost` 断开当时的全部连接）scene 不在原连接上重开，还连着的只会是之后的新登录，由它的 Join 重新打开。
  Join 打开会话时上一次的会话还在关闭（`SessionOpenRetryable`）会有界退避重试；用尽或被拒绝时玩家留在场景、自己的视图等下次登录，除 Warn 外计 `scene_session_open_failed_total{reason=exhausted|refused}`。
- **仅内存卸载后重载**：运维 `entity.unload`（DataEngine 排空投影后卸载）或 Remote 事务被持久拒绝时，Player 会被仅内存卸载
  （`ManagerAccess.Unload`）。scene 的 Manager 是自建的，wiring 的 Nest Mod 不替它接线，所以 `Scene.Start` 自己调
  `ConfigureUnloadResync`：仍有人看着这个玩家时框架在 Nest 业务池（short / long）之外从 Mongo 重载并 `Rebind`，观察者收到权威全量、之后照常收增量；重载不了收到 remove。
  `Scene.Close` 先停重载再关 Manager（服务先于 Nest Mod 停止）。一个实体运行时只能接一处。scene 的提交观察者通过 Service.NestSyncObserver 在 Nest 构造前绑定；工程若另用 `NewModWithEntitySync`/self_sync，Nest Provide 阶段会明确报告观察者冲突。
  卸载时没人看着（自己的复制会话已丢、附近没人）框架不重载；之后业务再访问它、DataEngine 从 Mongo 加载出来时，`Scene.Start` 挂在 DataEngine
  `OnEntityLoaded` 上的钩子把场景的 subject `Rebind` 到新实例，稍后进入视野的观察者照常收到它；`Scene.Close` 停重载后解钩。
- 机器人 `scene_watch` / `scene_expect` 是真客户端：解码、合并、断言。场景帧走 `PushSyncPlayer`：RS v2 Sync 标志、原始帧字节、
  不包 `EntitySyncPush` PB，robot session 对 Sync 包不做 PB 解码，`scene_watch` 直接读包体（没有 Sync 标志的 `MsgEntitySync` 推送
  让 watch 失败）；回归见生成的 `cmd/loadtest/scene_watch_test.go`。场景最后一步 `scene_alive` 自己 `add_exp 1`，等这次改动作为帧落到自己的
  subject 上：服务端因编码失败（例如单个实体超过帧上限）关掉的复制会话会断开连接（见上“编不出帧也断开”），`scene_alive` 是“帧还在来”的端到端判据。**PB 推送消息必须在 loadtest 注册解码器**，否则推送到了也解不出来、静默丢弃。

## 刷新：场景该有多少东西活着

`configs/table/spawn.csv` 一行是一组：模板、数量、血量、中心点、半径、重生秒数。

- **场景自己的 1Hz 心跳决定缺额**（D20 P5）：`"scene.spawn"` 在场景的锁里问 `Refresh.Due(beat.Now)`，事务**提交之后**
  （`nest.AfterCommit`）把这一批交给 spawner；原来服务 goroutine 每秒一次的 ticker 删掉了。
- **建实体在服务 goroutine，按四步、按这个顺序**（`internal/service/<game>/spawner.go`）：① 取 id、`lifecycle.Create`（组件建好、AI 还没有策略，什么都不跑）；
  ② 放身体（模板、血量、位置——任何人看见之前的唯一一次写，所以不会在 (0,0) 被复制）；③ `ShowSubject`（先登记复制主体、再进 AOI，否则观察者会订到 Manager 没有的主体）；
  ④ Nest 事务 `handlerSpawnMonster`（场景 + 怪物）：进场景的移动（`MapBasic.Enter`）并开始巡逻——提交后它的心跳才开始，第一步一定在它可见之后。
  全部成功才 `Spawned`；任何一步失败按逆序撤销（Hide、Destroy），下一拍的 `Due` 里重新出现。
- **一次只建一批**：上一批还在建时交来的批次丢弃。`Refresh` 数的是被告知存在的，还没 `Spawned` 的请求下一拍会再要一次，
  不丢弃就会建两只。
- **数的是"被告知存在的"而不是"被请求过的"**。一次失败的创建会在下一次 `Due` 里重新出现；若按请求扣减，
  这个进程余下的时间里都会少一只，而且没人会说。
- **怪的 DAO 每个字段都是 `nopersist,sync`**：不存，但复制。这是为了让"位置住在 DAO 里、经组件读写"对**所有**实体一致——
  否则 demo 里会出现两种位置权威，而第一段要同时处理玩家和怪的代码就会挑错一种。
  它用 `//roost:dao nocoll` 声明**没有集合**：不编造 Mongo 集合名，生成物里没有集合常量、`PrepareMutation`、`RestorePersisted`
  这条永远不会写东西的持久化路径，只有读写方法、回滚快照与 `MarshalSync`；DaoManager 里以 `db.MonsterDaoRegistryKey` 登记。
  生成器保证它全字段 `nopersist`、且只被 `noPersist=true` 的实体使用（[设计记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DAO-NO-COLLECTION-2026-10-04.md)）。
- **system 仍是纯状态机**：`Due(now)` 的时间是传进去的（心跳的 `Beat.Now`），所以它可测、可重放。
- GM：`gm.scene.population` 看当前几只，`gm.scene.kill` 杀一只——表里写 20 秒，20 秒（业务时间）后的那一拍回到满员且是一个**新 id**；
  `gm.scene.root` 定身。怪物 id 由 `game/runtimeid` 按 sid 编码，两个进程不会撞。

## 装备栏

装备使用 [equipment 模块](../modules/equipment/README.md)，demo 只提供 `configs/table/equipment_item.csv` 的武器槽位与加成规则。客户端走 `EquipmentEquip` / `EquipmentUnequip`，Robot 的 `equip` 动作也调用同一协议。

`PlayerDao.Equipment` 的 Slots 持有 GearPiece，字段仍有两层嵌套。组件在同一 Nest 事务内扣新物品、替换槽位、退旧物品；逐键 setter 负责绑定与解绑，原有的替换、回滚和失效旧引用场景保留。装了 attribute 后，同事务钩子重算属性，handler 不再重复扣包或重复刷新。

## 持久 schema

属性/装备模块化后，当前示例使用 schema 3。schema 2 的旧示例数据会明确拒绝；本轮不提供其格式兼容或自动转换。重新生成示例请用新数据目录/数据库，不能复用已有 WAL。下述 schema 1 教学迁移仍只演示业务显式登记、停写后操作的方式。

`//roost:dao coll=player db=main schema=4` 声明当前 schema。加载只接受相同版本；旧版本和未来版本都返回 `dataengine.ErrSchemaMismatch`，不自动转换或写回。正式生成的 `db/restore_test.go` 验证拒绝前数据及 tracker 不变、当前版本正常恢复。

改形状时靠离线迁移：`db/migrations` 登记 Player 从 1 到当前 3 的教学步骤（v1 的扁平 `weapon_id` 变成当前的 `equipment.1`（模块的稳定存储键））和写入前的校验（用生成 DAO 的 `RestorePersisted` 按当前 schema 解码），`offline.go` 是执行器，`cmd/schemamigrate plan|apply|verify|rollback` 是运维入口。只能在停写之后运行；apply 先完整 dry-run、全部通过才“写备份 → 带 `_version` 与 `_schema` 条件改写”，改写时 `_version` 加 1，没停干净的旧进程因此冲突而不是把旧 schema 写回去；rollback 只恢复仍是迁移写的那一版。流程与边界见 [schema 变更操作手册](../docs/framework/SCHEMA-MIGRATION.md)。

## 把一个 sid 搬到另一台机器

搬机搬的是 sid，玩家与 sid 的绑定不变（单个玩家换到另一个 sid 是下一节）：旧进程正常停机（断开玩家、WAL 全部投影、释放单实例锁），新机器上同一 sid 的进程用空 WAL 目录接管。`cmd/sidmove` 只做步骤之间的检查：`wal-drained`（旧 WAL 已全部确认，进程还在跑就拒绝）、`singleton`（锁现在被谁持有）、`players`（停旧进程后与新进程写入前的玩家文档是否一致）。步骤、所用能力与本机演练见[参考流程](../docs/framework/PLAYER-MIGRATION.md)。

把一个服并入另一个服（别名合服）用 `cmd/sidmerge` 与 `cmd/accountctl merge-server`：角色身份不变，account 记录“A 并入 B”后会话的承载服是 B，认证器按承载服写 `server_id`、原服写 `origin_server_id`；`sidmerge` 做合服前的检查（两侧 WAL、共享库同处、outbox、saga 终态、持久总线清单）并搬按 sid 分库的数据（game-demo 的 DAO 都在共享库，不用搬）；装配了 account 的 game 进程启动时自检，本服已被并入就拒绝启动。步骤与回退见[参考流程 §6](../docs/framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)。

## 把一名玩家换到另一个服（rehome 模块）

单个玩家换服是 `rehome` 模块（`roost module add rehome`，demo 的模块列表里最后一个）：协议 `RehomeApply`（玩家自己申请，受规则文件 `game/rehome/rehome_rules.go` 的可去列表与 7 天冷却约束）与服务端入口（GM 命令 `gm.player.rehome {player_id, to, note}`，不受规则约束）走同一个驱动器。顺序是：源服打“离开中”标记（从此他新的请求在写闸门被拒）→ 换服前检查 → Player 上记意图 → account `BeginRoleTransfer` →（答复客户端“已受理”）→ 断开会话并等标记之前准入的请求执行完 → 事务里把检查再跑一遍 → 离开钩子 → 离开场景、DataEngine 交出实体（屏障保持）→ account `CommitRoleTransfer` → 目标服首次进入时到达（清意图、冷却起算）。提交 / 中止的结论按 account 带回的当前绑定判定，不按错误码（应答丢失之后重发得到的 stale 不等于“没提交”）。换服不搬数据：Player 的档在共享库 `main`，只交接写权。每一步失败的去向见 `game/rehome/rehome_driver.go` 的注释；account 一侧的契约见[参考流程 §7](../docs/framework/PLAYER-MIGRATION.md)。客户端在被受理之后只认一个信号：连接断开，然后重新选角，角色所在的服变了就是换成了。

demo 在模块之上接的线：

- **驻留表就是模块的 Residency**（`internal/service/game/playerowner.go`）：`PlayerOwners` 带迁出表（玩家 → 去了哪、是否还在换服中），以 `gamerehome.ResidencyCapability` 再发布一次。迁出表与驻留表同一把锁：表里有他，`Serve` 与 `AdmitBound` 都不给他建驻留记录；交出实体成功时驻留记录被删，写闸门（`Admit`）从此拒绝他。迁出表不持久，进程重启后他归谁问 account。
- **登录**（`game/controllers/player/enter_game.go`）：在把玩家接入服务、装载实体之前跑 enter 模块的进入前检查（`gameenter.RunEntering`，rehome 与 enter 的联动登记在上面）——正在换服答 `rehome_moving`，已经绑定到别的服答 `rehome_elsewhere`（应答的 `OwnerSID` 是他现在的服）；握手在换服之前的旧会话（`server_id` 仍是本服）也在这里被拒。到达与意图对账是进入钩子（`gameenter.RunEntered`，首次初始化之后）。认证器（`auth.go`）写 `bind_epoch` 与在途换服的 `transfer` 两个 claim。源服在受理之后重启的，这名玩家再来登录时源服把那一次换服接着做完。
- **saga 步骤按当前绑定准入**（`gift_saga.go`、`alliance_saga.go` 的 `admitBound`）：载荷里的 sid 只是发起时的服、用于最初的路由；准入问 `PlayerOwners.AdmitBound`——有驻留记录就在本服执行，迁出表里的答“换服中”或他去的服，都没有就问 account（`gamerehome.Binding`）。换服中退回协调器不带路由（不耗尝试，稍后重投），绑定在别处退回并给出当前 sid，问不到 account 则在本服重投。
- **demo 自己的换服检查**（`internal/service/game/rehome_checks.go`）：在公会里、在队伍里的玩家被拒绝（先退出），开着分区场景的服一律拒绝——这些东西记着“玩家在哪个服”，换服时还没有人替它们改。申请时查一次不够（客户端还连着、成员通知还在路上），所以分三处：申请时看档里的 `GuildID` / `TeamID`（尽早拒绝）；驻留表从标记起在写闸门拒绝他新的请求（`PlayerOwners.Admit`），受理之后 `Quiesce` 断开会话、等在途的请求执行完，模块随即在事务里复核——队伍在这里是可靠的（入队与写档同一笔事务）；离开钩子再向联盟服务问权威的成员卡（`Membership`：成员、入会预留在途、问不到都中止），因为档里的 `GuildID` 只是展示副本。用例 `rehome_checks_test.go` 用 channel 固定了“检查之后才入会”“标记之前准入的入会请求还在执行”“通知迟到、联盟已记着成员关系”三种交错。匹配队列里的票据、进行中的副本与对战、名下的建筑这里看不到，没有拦；运营给这样的玩家换服前要先确认。

用例：`internal/service/game/gift_route_test.go` 用真实的 `account.Service`（内存存储）与两张 `PlayerOwners`（1000、1001）钉住“Begin 之后步骤在 1000 被退回、不带地址、不建驻留记录；Commit 之后 1000 退回并给出 1001，同一命令在 1001 被接入”，公会步骤同规则，以及换服前的旧 claims 在换服后登录被拒；模块自带的场景在 `game/rehome`、`game/handler/rehome_test.go`、`game/controllers/rehome`。这些都是离线的（替身提交器、交出实体是替身）：两个真实 game 进程加 account 的演练还没有做。

## 兴趣：距离是一种视野，关系是另一种

"谁该收到谁的状态"由 `framework/sync/entitysync/policy.Interest` 回答，scene（`internal/service/game/scene.go`）只给配置：
地图尺寸、AOI 格边长与进出圈半径；队伍在 `SetTeam` 里建成一个关系视野。设计与契约见 [Sync 设计与使用](../docs/framework/guide/04-sync.md) 与 [Block AOI](../docs/framework/INTEREST-BLOCK-AOI.md)。

- **距离与关系是不同视野**：玩家的主视野（跟随玩家实体）按距离看，队伍是一个关系视野（成员是队员的实体，挂给每个队员）；一对（玩家，主体）可能被多个视野同时持有
  （队友正好站在旁边），解除一个视野不会退掉另一个视野仍需要的订阅。
- **被看的用 entity id，看的人用 player id**。entity id 把 unique id、kind、category 打包进一个 int64，跨 kind 唯一；unique id
  只在 kind 内唯一，拿它索引目标会让 Player 42 和 Monster 42 相撞。观察者是玩家，就是 player id，也是 Manager 的会话 id；
  `Enter(playerID, entityID, …)` 两个都给，玩家实体到 player id 的换算只在 `playerOf` 一处。
- **主视野最多看 64 个（`gamescene.View.MaxVisible`；开地形的普通场景是游戏尺度的 128、会话上限 164，见可编辑地形方案 §29）**：范围内多于 64 个时只留最近的 64 个（AOI 的 `MaxVisible`，近的进来挤掉最远的）。
  所有玩家从同一出生点进场，不设上限时挤在出生点的人加上旁边的怪会让每个玩家的复制会话持有超过 Manager 的 `Limits.MaxObjects`
  （缺省 100，也是一个会话能持有的对象数）。2026-10-22 的登录风暴里 100 个机器人的会话全部因此被关；2026-11-08 起超额不再关会话
  （超额降级，方案 D17：暂缓放不下的对象、主视野从最远处收紧），但上限仍要设——在会话上限以内就从不暂缓，视野也不会在边界上反复试探。取值：会话除主视野外还持有自己、场景实体、对局队友（GroupSize 2，同时只在一个队里）
  共 3 个（`sceneReservedObjects`），64 + 3 离 100 还留 33 个（`sceneRelationHeadroom`），给以后加的关系视野（公会、多人队伍）；会话上限由名额推出（`sceneSessionObjects` = 名额 + 3 + 33），两者总放得下。
  回归：`TestACrowdedSpawnPointKeepsEveryReplicationSessionOpen`（100 个玩家 + 3 只怪挤在出生点，会话都不关、每人恰好看到 64 个）。
  反过来去掉上限（`TestACrowdOverflowingASessionShrinksTheViewInsteadOfDisconnecting`，101 个玩家）时没有会话被关、没有人被断开，
  每人收敛到恰好 100 个对象（自己 + 最近的 99 人），去掉的都不比留下的近。
- **滞回**：进圈 120、出圈 150。在边界上来回微动不产生 Enter/Leave 抖动——每一次抖动都会重发一次快照。
- **格边长约等于视野半径**：一个观察者订阅的格数是 `(⌈2·出圈半径/格边长⌉+1)²`，格子远小于视野不会让 AOI 更准
  （半径判定本来就是精确的），只会让观察者每动一步的簿记成倍增加。框架不强制这个比值，所以它写在 `scene.go` 的注释里。
- **没配档位**：档位要能裁字段才有意义，而 demo 的 packer 不按字段掩码裁剪（生成的字段掩码常量是 DAO 包私有的，尚未解决）。
- **视野由客户端请求**：`scene_view`（层级 + 视野大小）直接进政策、不走提交门；handler 里只登记（短锁、不做 AOI 计算），下一个
  tick 的政策阶段按调用顺序应用。层级只有 0，视野大小夹到 60～270 并按 30 取整，每秒至多改一次，应答带生效的视野。大视野让观察者订阅更多格子，上限在启动时按最大出圈半径核过预算。

## 属性：层、合成，以及两种存储意图

属性使用 [attribute 模块](../modules/attribute/README.md)：`game/gameplay/attribute` 声明属性与派生公式，组件按 DAO 内容构造只读副本；升级由 profile 的登记点同事务触发，demo 的升级钩子只发奖励邮件事件。

- **三层**：`Base`（玩家自己的值）、`Gear`（穿戴集的投影，按 equipment_item 表的 `attack` / `hp` 求和）、`Final`（合成视图）。
- **合成规则**：`Final = Base + Gear` 逐属性相加，**然后**才 `Update()` 重算派生属性。派生属性只算一次、且算在合成后的输入上——
  把各层自己的战力相加，等于把两个各用一半输入算出来的评分加起来，那不是战力的意思。
- **两种存储意图**：`AttributeBase` 是 `persist,sync`；`AttributeFinal` 是 `nopersist,sync`——能从已存的东西推出来的值不存
  （存了就是第二个真相，会和第一个打架），但它是客户端要画的，所以照样复制。Mongo 里只有 `attribute_base`，线上两个都有。
- 容器在 `OnInitFinish` 填充：生成的 builder 先挂 DAO、再初始化组件，所以那时存储值已经在手上了。
- **没做**：会过期的层。真正的 buff 层需要一个时钟和一次扫描，而它该待在被 tick 调用的组件方法里，不是某个 getter 里的惰性检查。

## 排行榜：rank 服务，与领奖账本同一个准入

排行榜用的是 [leaderboard 业务模块](../modules/README.md)（生成时由 `roost module add leaderboard` 装进工程）：读榜的两条协议、
提交入口 `gameleaderboard.SubmitSeason` 与它们的场景测试都是模块的，demo 不再带自己的 `game/ranking` 与 `RankTop`。demo 只覆盖
模块留给工程的业务文件 `game/leaderboard/leaderboard_board.go`（连同钉住它的 `leaderboard_board_test.go`）——哪张榜（`clears`，
副本通关次数，全服）、哪个赛季（`CurrentSeason()`）、分数范围、一屏十行；一分是什么、什么让提交可重试也写在这个文件头上。
通关计数本身（`PlayerDao.DungeonClears`、领奖账本）是副本玩法，留在 demo。

- 一分不是“+1”，是**通关数快照**：领奖事务（`handlerClaimDungeon`）在记账本、发经验的同一笔事务里给
  `PlayerDao.DungeonClears[赛季]` 加一，首次领取与重放都返回当前通关数（`ClaimResult.Clears`）；端点调模块的
  `SubmitSeason`（`UpdateMax`、不带 requestID）把榜抬到这个数。重放、重复、乱序的快照都不多加也不降分。
- 为什么不再用 `UpdateAdd` + `clear:<run id>`：rank 只记每个玩家最近 8 个请求 ID（去重环，不是账本），而端点在重放时
  也要提交；一局在领奖窗口内、被后来 8 局挤出去重环后重放，会再加一分（9 局记 10 分）。通关数随奖励一起准入
  （执行时间判窗口，窗口外的一局在计数前就被拒绝），不需要第三本有期限的账本。
- 提交**不**以"这次是否真的发了奖"为条件：重放发现奖励已发，恰恰是"分可能还没记上"的那种情况；下一局的快照也会把它带上。
- 提交在事务**外**（榜在另一个服务），两者不原子。崩在中间会留下"已发奖但没记分"——可恢复的方向，重试这个端点只会把榜抬到同一个数。
  提交失败只打 Warn，不让结算失败。通关数为 0 时不提交（在别的赛季领过奖的一局重放到本赛季）：没有通关的玩家不上榜。
- 通关数按赛季分开存：改 `leaderboard_board.go` 的 `CurrentSeason()` 后新榜从 0 开始，不把上一季的数带过去（维护者 2026-10-18 决定按
  赛季，不做终身累计）。端点只取一次赛季号，领奖事务按它计数、`SubmitSeason` 按它提交，所以第 N 季的通关数只会进第 N 季的榜。
- 回归（`game/controllers/player/finish_dungeon_test.go`）：`TestDungeonClearSubmitsTheSeasonCountAsASnapshot`（无外部依赖，看端点交给
  rank 的每一次提交：快照、`UpdateMax`、不带 requestID、重放照交、失败不影响结算、0 不交）与
  `TestDungeonClearsBoardCountsEachRunOnceRealRedis`（真实 Redis 上看榜上的数，要 `ROOST_REVIEW_REDIS`）。
- **活动贡献与排行相反：只在首次付款时贡献**（`result.Claimed`）。排行提交的是事务里通关数的快照，重放重交无害；活动贡献是往
  “调用时刻的时钟所在的窗口”（300 秒一个）里加分，协调器只按（窗口, 玩家, `clear:<run id>`）去重。同一局在 4 小时领奖窗口内
  跨过窗口边界后重放，落进新窗口、键不同，会再记一分（一局最多可进 48 个窗口，影响按名次发的活动邮件）。所以驱动贡献的是
  “这次真的付了款”这一事实，与通关数同源；重放不贡献，响应里也不带 `ActivityID`。**代价**（维护者 2026-10-18 按推荐决定）：
  付款之后、贡献之前崩溃或协调器调用失败，这一分永久丢失，重放不补——活动是尽力而为，补报若落到后来的窗口本来也记错了窗口；
  端点对这种失败打 Warn 日志。回归 `TestAReplayContributesNothingToTheActivity`（`game/controllers/player/finish_dungeon_test.go`，无外部依赖）。
- 读榜是模块的 `LeaderboardTop`（消息号安装时分配，见 `roost.yaml` 的 `modules.leaderboard`）：榜头 + 自己的名次
  （`MyOnBoard` / `MyRank` / `MyScore`）。一页是榜头，说不了不在这一页上的人的事——自己在这一页上时取那一行，不在时再读一次。
  只要自己的名次用 `LeaderboardMe`。榜 id 由服务端定，从线上收一个 board id 等于让任何人读到任何榜。

## 技能目录：启动时编译，客户端可查

技能目录来自 [skillbook 模块](../modules/README.md)，demo 只覆盖规则数据 `game/skills/fireball.json`。模块的 `game/skills/catalog.go` 把目录下的 JSON
嵌进二进制并用 `gameplay/skill` 严格 `Parse` + `Compile`。生成的服务适配层在业务 `Init` 前编译整份目录——**编不过就不起来**；
warning 数进日志，编译环境在 `game/skillbook/skillbook_rules.go`。`game/controllers/skillbook` 的 `SkillCatalog` 端点读取启动时发布的同一份目录，
返回排序后的 id 与 warning 数；消息号安装时分配，见 `roost.yaml` 的 `modules.skillbook.ids`，机器人断言至少一个且 0 warning。
技能**执行**（Host 读已锁 Entity、确定性 tick、checkpoint / replay）刻意没进 demo，见 `roost help skill`。

## 实时战斗：lockstep 帧同步（匹配之后的那一段）

Nest 是"服务器说了算"的状态：客户端请求、服务器改实体、结果落库。lockstep 是另一半——**确定性模拟**：
服务器只把所有人的输入排成编号的帧广播出去，每个客户端用同一份输入跑同一套模拟，得到同一份状态，服务器一个字节的状态都不发。
demo 把两者都给出来：匹配成功后，形成这场比赛的 game 进程开一个 `lockstep.Match`，两名玩家打满 45 帧（30 Hz，1.5 秒）。

- **框架给的**（`framework/sync/lockstep`）：把输入按提交窗口排进帧、每个座位的重放保护、每个广播包携带最近几帧（丢一个包由后续包自愈，不重传）、
  给掉线重连的会话分页补发历史、把关键帧的哈希报告按法定人数判成 desync 裁决。
- **工程写的**（`internal/service/<game>/battle.go`）：房间的生命周期、驱动它的那条串行 goroutine、以及线。
  `lockstep.Match` 是单所有者状态（内部没有锁），所以所有调用都在一条 goroutine 上；端点把消息投进 channel，不直接碰房间。
- **线是 demo 已有的 player TCP 连接**：广播是服务器推送 `BattleFrame`（10102），载荷就是 Lockstep 包本身（RS v2 Lockstep 标志，不带 PB 包装和 battle id）；
  输入回来是一条普通请求（`BattleInput`，10015）。lockstep 不要求数据报通道，它要求帧能到；包里的冗余是让有损通道能用的东西。
  换成 UDP / KCP 部署时把 `lockstep.TCPSender` 换成对应的 nettransport 发送器即可，房间一行不动。
- **两条发送通道经 `lockstep.TCPSender`**：实时帧只入出站队列、Tick 不等任何连接；追帧页在嵌入 TCP（生成默认）下经
  `TCPRuntime.PushLockstepSessionWritten` 等写出，追帧随链路放慢，慢连接不会因追帧积压超过出站上限被断开。等写出只发生在 Match 为追帧页起的
  goroutine 上，不在房间的 Tick 协程，也不在业务 worker。换成独立 Gate 部署时拿不到 `TCPRuntime`，追帧页只准入（Gate 写出后不回执，
  见 KNOWN-LIMITS“Lockstep 追帧链路背压（独立 Gate）”）。
- **帧按连接发**：座位在它第一条 `BattleInput` 到达的连接（接入层 SessionID）上加入，重连后下一条消息把座位带到新连接。还没说过话的座位收不到帧；
  晚加入的座位第一个包就看到缺口，客户端请求追帧补齐。帧不带 battle id，所以一条连接同时只打一场战斗。
- **一条消息三件事**：`BattleInput` 同时带本帧输入、关键帧的模拟哈希、以及"从第 N 帧开始补发"的请求——这三样都是每帧节奏的东西。
- **收尾窗口**：房间切完最后一帧不会立刻关。客户端只有应用了关键帧才能报出那一帧的哈希，所以最后一个关键帧的报告必然晚于模拟结束；
  切完就关的房间会正好拒掉 desync 裁决需要的那批报告。战绩结算、反作弊校验也在这个窗口里做。
- **两端共享帧预算**：`game/battle` 里的 `Frames` 双方都读，所以客户端到最后一帧就停，而不是给一个刚关闭的房间发输入然后被拒。
  按条件结束的战斗要显式下发结果——房间的最后一帧不自带"我是最后一帧"。
- **机器人是真客户端**：`cmd/loadtest` 的 `battle` 动作用 `robot` 包的 `LockstepBot` 跑完整条链——推送喂给 bot，bot 按序应用帧到
  `battle.State`，再把本座位下一帧的输入和关键帧哈希发回。推送处理器只负责把包转交：它跑在会话读循环上，而应用一帧要在同一条会话上发请求，
  在那里应用会把读循环堵死在自己的响应上。
- **座位**：`poll_match` 返回的 members 顺序就是座位顺序，服务端开房间和客户端算自己座位用的是同一份顺序。
- **desync 长什么样**：两个客户端的模拟不一致时，房间在关键帧判出 outlier，game 日志里是一条 `battle: desync verdict` 的 ERROR。
  真实游戏在这里踢人或强制重同步；demo 只记录——所有客户端都诚实时它不该出现。
- **边界**：房间是进程内状态，只有形成比赛的那个 game 进程持有它。多 game 进程部署要把战斗做成自己的服务、按 match id 寻址；
  进程重启会结束它托管的战斗（框架能给重连会话补发历史，但只在房间还活着时）。ranked 匹配也会开房间，但 demo 的机器人只打 duel 那一场；
  没人进的房间在 `battleStartGrace` 后照常切帧（没有座位加入，帧不发给任何人），切完帧预算、过完收尾窗口后退出，最迟 `battleMaxLifetime`。

## GM 运维面：admin 命令，不是另一个 HTTP 服务

`gm` 模块与功能联动注册通用命令，`internal/service/game/gm.go` 保留 demo 专属命令与双实体经验回调；它们共用 app 的 admin 命令表（`app.ModAdmin`），ops Mod 经 HTTP 端出来：
`GET /admin/commands` 列命令、`POST /admin/execute` 执行，鉴权 `X-Admin-Token` 或 Bearer；`ops.admin_enabled=false` 时两个端点 404。
demo 的**开发配置**开着 admin、token 是 `dev-gm-token`（`allow_dev_token: true`）；生产示例配置关着，而且 `config check --production` 拒绝 `dev-` token。

```bash
curl -s -H 'X-Admin-Token: dev-gm-token' http://127.0.0.1:9100/admin/commands
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.player.add_item","operator":"ops","trace_id":"t1","payload":{"player_id":100866,"item_id":2001,"count":1,"note":"客服补偿"}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.mail.send","operator":"ops","trace_id":"t2","payload":{"player_id":100866,"subject":"补偿","body":"抱歉","attachment":{"type":"item","id":1002,"amount":5},"request_id":"incident-item-1","note":"故障补偿"}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.mail.send","operator":"ops","trace_id":"t3","payload":{"player_id":100866,"subject":"补偿","body":"抱歉","attachment":{"type":"coins","amount":100},"request_id":"incident-coins-1","note":"故障补偿"}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.world.stats"}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.saga.list","payload":{"status":"manual_required","limit":20}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.saga.get","payload":{"id":"gift-100866-demo-100866-1789-15"}}'
```

- 配置表重载不是 GM 命令：configdata Mod 自带 `configdata.reload` / `configdata.rollback` / `configdata.status`（框架注册，与 `hotcode.*` 同一套 token 与审计）。
  `curl ... -d '{"name":"configdata.reload","payload":{"reason":"改价"}}'` 回答新版本号与哪些表变了；重载只读 `configs/data` 下的 JSON，
  只改了 `configs/table` 的 CSV 却没 `roost generate` 时回答 `changed=false`、message 写 `no table changed`。开关表（`feature_flag.json`）也经这条生效，并覆盖 `gm.flag.set` 的进程内改动。
- `gm.player.rehome {player_id, to, note}` 把一名玩家换到另一个服（rehome 模块的服务端入口，见“把一名玩家换到另一个服”）：要在他当前绑定的服的进程上执行，别的服答 `rehome_elsewhere`；不受玩家的冷却与可去列表约束，登记的换服前检查（公会 / 队伍成员、分区场景）与 account 的条件照常生效。回答 `accepted=true` 与换服 ID 表示已受理、后台在继续，结论用 `cmd/accountctl show-role` 看。
- `gm.saga.list` / `gm.saga.get` 读 saga 协调器的记录（状态、当前步、最后一次错误、状态数据）；`manual_required` 是需要人的那一类。

- `player_id` 两种形式都收：客户端看到的唯一 id（`EnterGameResponse.PlayerID`，也是邮件的收件人 id），或 Mongo 里 Player 文档的 `_id`（完整实体 id，多了 kind / category 位）。
  两者不同：把 `_id` 当唯一 id 再包一层会得到一个不存在的实体（`entity aggregate not found`）。响应里同时给出 `player_id`（唯一 id）与 `entity_id`。
  唯一 id 从 account 服务 Redis 里的计数器 `<account.key_prefix>:player_id`（生成配置下是 `roost:<项目名>:account:player_id`）分配，同一份 Redis 上反复起 demo 不会从 100001 重来——别猜，从 `EnterGame` 的响应或 Mongo 取。
  计数器跟账号、角色、服务器登记在同一个前缀下：同一个 Redis db 上前缀不同的两份部署各数各的，按前缀清理一份部署会连它一起清掉。
  改 `account.key_prefix` 而保留数据时，要把旧前缀下的全部键（计数器在内）一起迁过去。
  旧版本写死的计数键 `roost:demo:player_id` 已退役、不再读取：它仍存在时每次分配都明确拒绝（`retired counter key roost:demo:player_id is still present`），
  不会从它接着数，也不会从 0 重新数而重发旧 id。这个键是保留键：`account.key_prefix=roost:demo` 会让计数键恰好等于它，启动绑定时直接按配置错误拒绝、不碰 Redis，要求换前缀
  （已经用这个前缀跑着的部署先停写，把该前缀下的全部键连同计数器迁到新前缀）。处置分配时的拒绝前先确认这个键不是同一 Redis 上另一份仍用 `roost:demo` 前缀的部署的当前计数器；
  确认是旧计数器后停止分配，若 `<account.key_prefix>:player_id` 还不存在就把旧键的值迁过去，再移除旧键；否则给这份部署换一个没有该键的 Redis。
- 加道具受 Player 的背包规则约束（道具必须在道具表里、每种道具的叠加上限），超出时命令按业务错误码拒绝（`bag_stack_full`，道具表里没有的是 `bag_item_invalid`），不是 GM 越过规则。
- wallet / bag / profile / mailbox / rehome 命令由 `gm` 模块联动提供，必须带 `operator` 与 `payload.note`；命令名与完整参数见 [GM 模块](../modules/gm/README.md)。demo 只保留额外的场景、联盟、活动等命令，以及加经验同时计入 World 的双实体执行回调。
- 命令做的就是游戏做的事：加道具 / 加经验走与端点相同的 Nest Sender（GM 加的经验升级同样发奖励邮件），发邮件走 mailbox 模块的 `gamemailbox.SendTo`（同一个 mail 客户端、同一份 `attachment` 附件编码，一封只带一种；稳定 `request_id` 必填，同一键重试不另发），玩家经 `MailboxClaim` 领；发不出去的附件（数量不为正……）在命令这里就按参数错误拒绝。
- `trace_id` 是邮件的幂等键：同一 trace 重试不会发两封。
- 为什么不是 webroute：这套 admin 命令表就是框架给运维面的位置（Wiring 的 nats / bus 也把 DLQ 命令注册在这里），共用鉴权、开关和 `/admin/commands` 的自描述；`webroute` 留给面向玩家 / 第三方的 HTTP 接口。

## World 的职责

World 有了自己的 DAO（`PlayersEntered` / `MatchesFormed`）和 `Stats` 组件：`RecordEnter` 在 EnterGame 之后、
`RecordMatch` 在 Commit 之后各是一次独立的 Nest 调用（Player 与 World 是不同实体、不同锁档）；`WorldStats` 是一个带返回值的
读 handler，锁内读、值出锁，端点从不碰 World 本身。

World 还有一组持久定时器（`Timer` 组件，类型号 2006），目前只有一种：活动窗口的关窗阶段。它是**手写**的，不用 `roost add timer`
（维护者 2026-10-18：demo 是手写的参考实现，不迁到生成器产物上，生成器只生成框架内容）；手写的是适配，期限、触发与驱动都用框架的
`framework/nest/entitytimer`，所以这一节也是“不用生成器时怎么接这套驱动”的写法：

```
game/entities/world/timer_component.go   TimerComponent（持有 entitytimer.Host）+ worldTimerStore（WorldDao.Timers / TimerSeed ↔ entitytimer.Store）
                                         + TimerDriver（entitytimer.NewDriver，投给 TimerTickHandler）+ 回调 onActivityPhase
game/handler/tick_world_timers.go        handlerTickWorldTimers：World 锁内 TimerComp().Tick(业务时间)，rollback=undo durability=strict
internal/service/game/activity.go        startWorldTimers：SetClock(业务时钟) → Bind(Nest 客户端, 只读实体查找) → Start；Shutdown 里 Stop
```

- **期限只在 DAO 里**：每次调用从 `WorldDao.Timers` 现建调度器、钩子写回、用完丢弃；武装 / 触发与它所在的事务是同一条 WAL 记录，回滚就是
  DAO 的回滚。重启后从存储重建，停机期间到期的关窗节点在 World 加载后补触发一次（框架不追帧）。
- **不轮询**：驱动只记“已加载 World 的最早期限”，到了才经 Nest 正式准入投一次 Tick，索引在事务提交后才更新；D11 之前服务里每秒发一次
  `Sync_TickWorldTimers` 的循环已经不需要。
- **回调不做 I/O**：`onActivityPhase` 只记 `activity.phase_due` effect，提交后由服务里的消费者调协调器；记不上就返回 30s 重试，节点留着。
- **为什么在 `Service.Init` 里接、而且在 `EnsureWorld` 之前**：驱动按业务时钟（`time.logic_offset`）记加载时刻和判断“停机期间是否错过”，
  必须在 World 第一次加载之前接上。生成工程由 `wiring/entitytimer` Mod 在 Provide 阶段做这件事；demo 手写，放在 Init 开头，靠的是 World
  只在 `EnsureWorld` 加载、玩家请求在写闸门发布前全被拒绝。停止放在 `Shutdown`：App 先停 Service、再停 Mod，投递中的 Tick 仍有 Nest 可用。
- **每个进程都武装同一个窗口**：`ScheduleActivityPhase` 按活动 id 幂等，第二次返回已有节点，不会每次重启多一个节点。
- 测试：`timer_component_test.go`（重启后照样触发、回滚、过期立即补、同期限按优先级再按登记顺序、未知类型告警计数、非法节点只在正常事务里清理）、
  `tick_world_timers_test.go`（handler 名与驱动一致）、game 服务 `TestTheWorldTimersRunOnTheBusinessClock`（驱动投递带业务时间）。

## 机器人压测 = 回归测试

```bash
go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000   # 环境准备时一次：account 得知道这台服
make loadtest LOADTEST_COUNT=20                                           # 等价于下面这行
go run ./cmd/loadtest -endpoint 127.0.0.1:7000 -count 20 -metrics-addr 127.0.0.1:9300 -account-nats nats://127.0.0.1:4222
```

机器人走真实凭据：在压测进程里起一条 bus，用 account 的 typed 客户端依次 `Login`（demo 渠道，凭据 `demo:<open_id>`）→
`CreateRole`（在 `-server-id` 上）→ `SelectRole`，再以 `session:<player_id>:<token>` 握手；每次运行用新的 open id
（账号在一个服务器上只能有一个角色，且没有角色列表可查）。

`CreateRole` 会拒绝未知或未开放的服务器，而 **`UpsertServer` 刻意不在 account 的 RPC 接口上**：登记 / 开关服务器改变的是
所有玩家能登录什么，game 进程无权做，放在 Login 同一条总线上等于任何能到达 account 的进程都能关服。所以 demo 给了一个
操作员工具 `cmd/accountctl`：用 Redis 凭据直接打开 account 服务自己的 store 写入服务器记录——
`go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000`，环境准备时跑一次。account 配成 Redis Cluster
（`redis.cluster_addrs`）时改用 `-redis-cluster <种子,种子,…>`：单机客户端只写得进槽恰好在所连节点上的键，其余 `MOVED`。

每个机器人：`connect`（握手凭据是 account 签发的票据）→ `enter_game`（GetOrCreate Player）→ `add_item` → `add_exp`（升级，触发奖励邮件）
→ `join_queue` → `wait_push` 等服务端推送的 `MatchFound`（msg 10100，10s 超时后退回每 250ms `poll_match`，`selector` 节点）
→ `world_stats`（两个计数都得大于零）。
任何一步返回非零 code、或 `error_rate` / `p95` 超阈值，进程以非零码退出并打印 JSON 报告。**`-count` 要给偶数**：duel 两人一组。
退出前最后一行点名违反的阈值、实际值、上限与样本数（例如 `threshold violated: p95 = 17.2s > max 16s (10 samples)`）。
`p95` 由翻倍桶估计、收在最快与最慢机器人之间，10 个机器人及以下就是最慢的那个；缺省 `-max-p95 16` 约为健康运行
（单 sid 7～8s、两个 game 同机约 10s）的两倍。
**`-count` 不要超过 `player_access.tcp.max_connections_per_ip`（默认 128）**：机器人全从一个 IP 来，超出的连接在握手前就被
server 关掉，机器人报 `connect: auth send: robot session: closed`，server 侧计入 `player_tcp_connection_rejected_total{reason}`。
600 个机器人的实跑正好 128 成功、472 这样失败——这是上限在工作，不是缺陷；要压更大就在 game 配置里调高它。
2026-10-22 登录风暴 200 个机器人里 72 个这样失败，game 指标 `player_tcp_connection_rejected_total{reason="per_ip"} 72`，同一原因。

最后一步 `scene_alive` 判“服务端还在给这个客户端复制”：自己 `add_exp 1`，10 秒内这次改动没有作为帧回到自己的 subject 上就失败
（`scene_alive: … the server stopped replicating to it`），原因在 game 日志的 `scene: push to player failed, its replication session was closed`。

- **runner / 场景树 / 动作注册 / 阈值门**全是框架的 `robot` 包，`cmd/loadtest/main.go` 只做三件事：注册本工程的消息
  （`action.MustRegisterCall` + 一个把泛型 Marshal 路由到生成的 pb 函数的 codec）、加载 `loadtest/scenarios/*.yaml`、
  把 flag 变成一个 `loadtest.Profile`。
- **`loadtest/playertcp`** 是生成的 player TCP 帧的客户端半边：core 的 robot 传输层说的是 12 字节小端帧，生成的 server
  说的是 16 字节大端带 magic / 版本 / flags 的帧，且要求先握手、每帧序号严格递增非零。适配器自己计数 wire 序号，
  用一张表把响应映射回机器人等待的序号；这是唯一同时知道两边格式的地方，常量要与 `server_gen.go` 同步。
- **`enter_game` 为什么不是 Nest handler**：Nest 处理的是已存在的实体，第一次登录还没有；创建 Player 是生命周期操作，
  端点直接走 `PlayerLifecycle.GetOrCreate`，所以这条协议只有 `roost add protocol`，控制器方法手写。
- **`EnterGame` 的协议与 controller 域来自 enter 业务模块**：`-template game-demo` 生成时装 [enter 模块](../modules/README.md)
  （`demo.go` 的 `demoModuleSteps`），协议绑在 `enter` 域上。demo 的进入流程比“加载或创建 Player”多得多（绑定的服、登录预算、
  场景、已付订单、公会、公告），所以它覆盖模块的三个文件：`protocol/def/enter_game.go` 与 `game/controllers/enter/` 下的两个，
  后者只把请求交给 `game/controllers/player/enter_game.go` 的 `EnterGame`。模块的首次初始化仍然跑：`GetOrCreate` 之后发一次
  `EnterInit`，响应里的 `First` 说明这一次是不是首次；新玩家的初始发放写在 `game/entities/player/enter_first.go`。
  之后照样执行进入钩子（`gameenter.RunEntered`，带着会话的 claims）：聊天上线与拉全服邮件是 chat、mailbox 各自与 enter 的联动登记的，
  不写在 demo 的进入流程里。
- **登录有预算**：传输层给每个请求 `player_access.tcp.dispatch_timeout`（与 `nest.request_timeout` 对齐）；
  EnterGame 的归属 Claim + GetOrCreate 另用 `player_access.tcp.login_timeout`（默认 2s，不大于 dispatch）。冷加载要等该玩家已准入
  事务投影到库，库不可用时这个等待不会自己结束；超出预算回 `login_timeout`（100021），连接不被挂住，客户端重试即可——超时只停止等待，
  不撤销任何已准入的事务，共享的加载 flight 也不因一个等待方离开而取消。
- 生成的 pb 类型没有 `GetCode()`，`RegisterCall` 的自动 code 检查不会生效，每个 call 用 `OnResp` 自己查 `Code`。
- **成组后推送**：matchmaker 在 Commit 成功后经传输层 `Runtime.PushPlayer` 给每个成员推 `MatchFound`（协议里是一个没有请求的
  notify 方法，生成的 bind 注册它的编码器）。推送到达的是该玩家**所有**已认证会话；玩家已下线时推送失败只记 debug 日志——票据里
  仍有 match_id，`PollMatch` 是兜底，推送是捷径而不是事实来源。

## 实体锁档与跨实体事务

`entity.EntityCategory` 的值就是锁的获取顺序（低的先锁）：Remote(1) → World(2) → PlayerScoped(3) → Player(4) → Other(5)。
生成器默认把新实体放在 Other——"持有它之后什么都锁不了"，对还没决定顺序的实体是安全的。demo 把 Player 放到
`EntityCategoryPlayer`、World 放到 `EntityCategoryWorld`，因为 `AddExp` 是一个**两实体事务**：
`handlerAddExp(target player.IProfileEntity, stats world.IStatsEntity, amount int64)`——Player 加经验升级、World 累计
`ExpGranted`，两处变更进同一条 WAL 记录，要么都落库要么都不。Nest 按档位锁：World 先、Player 后。生成的 Sender 变成
`MultiSync_AddExp(ctx, target, stats, amount)`，一个实体一个 id；`roost add endpoint` 只接单实体 handler，所以这个端点手写。

公会名字的规则只写在 `alliancerule.ValidateName` 一处（空、全空白、超过 48 字节、含控制字符、非法 UTF-8 拒绝为
`alliance_name`）：`found_guild` 端点第一步调用它，在扣建盟费、启动 saga 之前拒绝；alliance 服务的 `Found` 再按同一规则判一次。

**敏感词、全服唯一是业务规则，框架不处理**（维护者 2026-10-17 决定），demo 也不实现，两个公会可以同名、靠联盟号区分。
敏感词加在 `ValidateName` 同一处。需要全服唯一的名字时用框架已有的占名原语 `service/directory`（`wiring/directory` 装配，用法见
[Service 篇 §4](../docs/framework/guide/09-services.md#4-关键业务边界)）：建盟前先 `Reserve` 占名，成功后 `Commit`，失败 `Cancel`；
预约带过期，进程中途崩溃时名字到期自动释放，不会被永久占死。

**档位编进实体 id**：改一个 kind 的 category 会改它所有实体的 id，已落库的文档全部失配。所以这是在第一条文档落库之前
决定一次的事；之后再改等于一次数据迁移。

## account 的 collaborators

`game` 模板给的 `Verifier` / `Allocator` 默认全拒绝（这是对的：没有校验的身份和会重复的 id 都不该有默认值）。
demo 换成能跑的版本：

- `Verifier` 只认 `demo` 渠道，凭据必须是 `demo:<open_id>`，其它渠道一律 fail-closed，返回的是它确认过的身份而不是提交
  上来的那份。**这不是身份校验**，只是把"该做的两件事"做了个样子，上线前换成对平台的真实调用。
- `Allocator` 用 account 服务自己 Redis 里的一个 `INCR` 计数器——持久、跨副本共享，满足 allocator 契约。它通过
  `account.RegistryBound`（`wiring/account`）在 `Provide` 里拿到 registry 再查 Redis 客户端：collaborator 是在 app 存在之前
  构造的，没有这个钩子就拿不到任何持久的东西。

## auth.go：两种凭据

`internal/access/player/tcp/auth.go` 认两种字符串：

- `session:<player_id>:<token>` —— **真实路径**。token 由 account 服务签发（Login → CreateRole → SelectRole），
  这里经 account 客户端 `ValidateSession` 校验，principal 用的是 account 返回的角色而不是 socket 声称的 id。
  客户端拿到 account 客户端靠生成的 TCP 传输层新增的 `RegistryBound` 钩子：authenticator 在 `Init` 里只有 viper 配置，
  Mod 在 `Provide` 里把 registry 交给它。
- `player:<id>` —— **不是认证**，直接信任 socket 给的 id，只为了能用一个裸 TCP 客户端把 demo 跑通。上线前删掉
  `demoTokenPrefix` 和读它的分支。

生成器默认给的是 fail-closed 骨架，demo 故意替换掉它——这也是 `roost config` 启用 TCP 时会检查的那个文件。
脚手架同时把 `player_access.tcp.enabled` 置为 true（等价于 `roost config enable player-tcp`），
所以 `roost project doctor -workflow player-tcp` 在刚生成的工程上全绿。
