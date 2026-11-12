# mailbox：玩家邮箱——列出邮件、领取附件，每封附件至多到账一次

邮件本身存在框架托管的 mail 服务里（另一个进程）。这个模块是游戏服这一侧：客户端读邮箱与领附件的两条协议、把附件发到玩家身上的那笔事务、附件的格式与种类，以及给服务端别的功能发件用的入口。金币附件内置（经 wallet 到账），别的种类由登记了它的功能提供。

## 能力

### 客户端协议（`protocol/def/mailbox_list.go`、`mailbox_claim.go`）

- `MailboxList`（请求 / 应答）：`MailboxListRequest{Cursor, Limit}`，新的在前。返回 `Mails`（每封 `MailboxEntry{MailID, Subject, Body, Status, HasAttachment, Claimable, ExpiresAtUnix, AttachmentType, AttachmentID, Amount}`）、`NextCursor`（为空表示到底）、`Unread`（整个邮箱的未读数）。页大小的缺省与上限由 mail 服务决定。`Claimable` 的意思是“现在领会成功吗”：mail 服务说可以预留，并且附件的种类登记了发放方式。
- `MailboxClaim`（请求 / 应答）：`MailboxClaimRequest{MailID}`，返回 `AttachmentType`、`AttachmentID`、`Amount`（这封发了什么）与 `Held`（到账后玩家持有多少：金币是余额，道具是这个道具号的数量）。没收到回包就用同一个 `MailID` 重发。

读谁的邮箱、领谁的邮件都取自已认证的会话。

### 服务端入口

- **发件** `mailbox.SendTo(ctx, mailer, Letter)`（`game/mailbox/mailbox_send.go`）：给一名玩家发一封邮件，返回 mail 服务产生的信封。`Letter{To, Subject, Body, Attachment, ExpiresInSeconds, RequestID}`；`RequestID` 是幂等键，必填；`ExpiresInSeconds` 为 0 用 `DefaultExpiresInSeconds`（7 天）。`mailer` 是 mail 服务的客户端（`app.Lookup[svcmail.Mail](registry, svcmail.CapabilityName)`）。
- **附件**（`game/mailbox/mailbox.go`）：`Attachment{Type, ID, Amount}`，线上格式是 JSON，例如 `{"type":"coins","amount":100}`。`Coins(amount)` 构造金币附件；`Encode` / `Decode` / `Validate` 是发件、列表、领取共用的同一个判断。
- **发放事务**：Nest handler `handlerMailboxClaimReward`（`game/handler/mailbox_claim_reward.go`，`rollback=undo durability=strict`，不对客户端开放）调组件 `MailboxComp().Claim(mailID, attachment, expiresAtUnix, nowUnix)`（`game/entities/player/mailbox_component.go`）。组件另有只读的 `Claimed(mailID)` 与 `LedgerSize()`。
- **端点** `HandleMailboxList` / `HandleMailboxClaim` 在 `game/controllers/mailbox`。
- **运行时** `mailbox.Start(registry)`（`game/mailbox/mailbox_runtime.go`，清单 `runtime.start`）：启动联动登记的消费者；只装 mailbox 时没有消费者。

### 领取的三步（`game/controllers/mailbox/mailbox_claim.go`）

| 步 | 调用 | 做什么 |
| --- | --- | --- |
| 1 | mail 服务 `ReserveClaim` | 交出附件与 token；token 对（玩家，邮件）恒定 |
| 2 | Nest 事务 `Sync_MailboxClaimReward` | 锁住 Player：附件到账、邮件 id 记进领取账本，同一次提交 |
| 3 | mail 服务 `CommitClaim` | 把邮件标成已领；同一个 token 幂等 |

第 2 步失败或附件这一版发不了时，端点用 `CancelClaim` 归还预留，邮件保持未领。

### 登记点

- `mailbox.RegisterType(kind, Validator)`（`game/mailbox/mailbox.go`）：登记一种附件的名字与校验。校验必须是纯函数，不能读配置表。
- `player.RegisterMailboxGrant(kind, MailboxGrant{Grant, Held})`（`game/entities/player/mailbox_grant.go`）：登记这种附件怎么发到玩家身上。`Grant` 在领取的那笔事务、Player 锁内执行，只能经同一个 Player 的 DAO 或组件改状态，不能做事务外的事；`Held` 只读，返回当前持有量。
- 两处都只在 `init` 里登记，空名、nil、重名 panic；给没有登记过校验的种类登记发放方式也 panic。只登记了校验的种类解得出来、列表里看得见，但不可领。

### 状态

`PlayerDao.MailboxLedger`（类型 `MailboxLedger`，`db/def/mailbox.go`）：`Claims` 是已到账的邮件 id → 这封邮件自己的过期时刻（Unix 秒）。只落库、不下发。邮件正文、已读未读、是否已领都在 mail 服务里，不在 Player 上。

### 配置

没有配置表与配置键。业务服务要在 `roost.yaml` 里 `uses: [mail]`。代码里的常量：`DefaultExpiresInSeconds`（7 天）、`LedgerPruneGraceSeconds`（600 秒，账本在邮件过期后还记多久）。

### 错误码（`internal/errors/mailbox.go`）

- `mailbox_invalid`：发放入口收到空的邮件 id、非正的数量或没有过期时刻。
- `mailbox_attachment`：附件这一版发不了（解不出、带不认识的字段、种类没登记、没通过校验、没有登记发放方式）。邮件保持未领。

邮件不存在、已过期、已领取、正被占用由 mail 服务答它自己的码；余额溢出、背包放不下原样透出 wallet、bag 的码。

## 能力边界

- **发件只有一种：一名玩家、一封、至多一份附件。** `SendTo` 不发全服邮件、不群发；“100 金币加 3 个道具”要发两封。
- **没有已读、删除、一键领取的协议。**
- **不在事务里发邮件。** `SendTo` 是跨进程调用，不该在 Nest handler 里调。要让“状态变化”与“发邮件”一起成立，在事务里记一条 effect，由消费者拿 `EffectID` 当幂等键调 `SendTo`（与 profile 的联动就是这样做的）。
- **“发得出去”不等于“领得到”。** `Encode` 只跑纯函数校验；道具号在不在道具表里、数量超不超背包上限要到领取时才知道。发了超过单种上限的道具，这封邮件每次领取都被背包拒绝，直到过期。
- **一致性与幂等：**
  - 附件到账与账本记录在一笔 `durability=strict` 的事务里。发放成功而 `CommitClaim` 丢失时，重试会再预留、再到发放事务一次，在账本上认出这封，返回 `Claimed=false` 与当前持有量，什么都不写，然后把确认补上。所以每封至多到账一次。
  - 没有后台补偿：`CommitClaim` 丢失之后，邮件在列表里仍显示可领，要等玩家再点一次才补上确认。
  - 预留成功后、发放前进程崩溃：邮件被占到 mail 服务的租约到期（缺省 30 秒），之后可以再领，什么都没丢。
  - 上一次已经完整成功、只是回包丢了的重发，会收到 mail 服务的“已领取”码，客户端当作已领。
  - `SendTo` 的幂等靠 `RequestID`：同一个键只产生一封，重发返回第一次的信封。键要取“这件事”自己的身份，不能用时间、随机数、自增计数。
- **账本有界但没有独立的清扫：** 每次首次发放时顺手清掉“邮件过期后再过 600 秒”的记录。已过期且不在账本里的邮件一律拒绝首次发放（按发放执行时 Nest 冻结的业务时间判断）。game 进程之间业务时钟差超过 600 秒时，已遗忘的邮件可能被晚到的重放再发一次。
- **列表不取实体锁：** 满背包时道具邮件仍显示可领。
- **新旧进程混跑：** 旧进程把新种类当未知附件拒绝并归还预留，玩家可能看到同一封邮件时而可领、时而不可领。
- **前提：** 依赖 wallet；需要 nest 与 dataengine 两个 Mod，业务服务 `uses: [mail]`。取不到 mail 的客户端时 `FromRegistry` 返回错误。领取跨两个进程（game 与 mail）。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“mailbox 的边界”（只在进程内验证过；经 NATS 的客户端、Redis 存储、超时与重投没有跑过）、“进入钩子不是事务的一部分”（全服邮件的画像规则）、“模块的规则大多还是 Go 写的业务文件”。

## 复用方式

- **需求正好是它：** `roost module add mailbox`，连带装上 wallet；确认业务服务的 `uses` 里有 `mail`。要道具附件再装 bag，要进入时拉全服邮件再装 enter，要 GM 补偿邮件再装 gm。
- **别的功能要给玩家发奖励邮件：**

  ```go
  mailer, _ := app.Lookup[svcmail.Mail](registry, svcmail.CapabilityName)
  _, err := mailbox.SendTo(ctx, mailer, mailbox.Letter{
      To: playerID, Subject: "活动奖励", Attachment: mailbox.Coins(100),
      RequestID: "activity:" + activityID + ":" + strconv.FormatInt(playerID, 10),
  })
  ```

  `errors.Is(err, mailbox.ErrLetterInvalid)` 表示这封信本身发不出去，不要重试；mail 服务的带码拒绝（邮箱已满、标题过长）重试也没用；其余错误用同一个 `RequestID` 重试是安全的。
- **加一种附件**（例如体力、英雄碎片）：在 `game/mailbox` 下新文件里 `RegisterType("stamina", validate)` 并给一个构造函数；在 `game/entities/player` 下新文件里 `RegisterMailboxGrant("stamina", MailboxGrant{Grant: …, Held: …})`。照 `with/bag` 的两个文件写。附件只有（种类，号，数量）三个字段，不要往 `Attachment` 里加字段。
- **手拼附件字节的发件方**（不经 `Encode`）要注意：解码不认识的字段会被拒绝；Go 的 JSON 解码对键名大小写不敏感。
- **不要改的：** `MailboxComponent.Claim` 里判断的顺序（请求合法 → 附件发得了 → 已在账本 → 已过期 → 发放并记账），以及 `ClaimWindowClosed` 与 `LedgerMayForget` 成对的定义——去掉其中一步会放出重复发放。调小 `LedgerPruneGraceSeconds` 到小于进程间时钟差同样会。
- **只适合借鉴的情形：** 邮件要存在游戏服自己的库里、要一封多附件、要玩家之间互发时，不适合直接用。值得借鉴的是“跨进程三步领取，中间那一步在游戏服的事务里记账本，账本的遗忘以邮件自己的过期时刻为界”。

## 联动

本模块的 `with/`：

- `with/bag`：登记 `item` 这种附件（`mailbox.Item(itemID, amount)`，线上格式 `{"type":"item","id":1001,"amount":3}`），领取时经 `BagComp().Add` 进背包，与账本同一笔事务；背包拒绝时邮件保持未领。
- `with/enter`：玩家进入游戏时把属于他的全服邮件合并进邮箱（mail 服务的 `PullBroadcasts`）。画像规则在工程的 `BroadcastProfileOnEnter`：注册时间缺省取首次进入的时间，会话带着 account 的角色创建时间与承载服 / 原服时改用它们。它是进入钩子，失败只记日志，下一次进入再做。
- `with/gm`：命令 `gm.mail.send`，单人补偿邮件，要求稳定的 `request_id`。
- `with/profile`：升级奖励邮件，见下一节。

别的模块：

- `activity/with/mailbox`：活动结算发邮件，以活动期次和收件人组成稳定的幂等键。
- `cdkey` 依赖本模块（`requires: [mailbox]`）。

## 验证范围

`roost module verify` 跑 32 条场景（`^TestMailbox`：`game/handler` 14 条、`game/mailbox` 10 条、`game/controllers/mailbox` 8 条）。联动另计：bag 9 条、enter 5 条、gm 4 条、profile 10 条。

场景是离线的、在一个进程内：发放事务用真实的 Nest 加替身提交器；端点的场景用真实的 mail `Service` 加内存存储。没有跑过的：经 NATS 的 mail 客户端、Redis 存储、超时与重投、Nest 调用因等结果超时返回错误而事务其实已提交的那条路径（重试会在账本上认出来，但没有用例）。

## 与 profile 的升级奖励联动

`roost module add mailbox profile` 安装升级钩子（`OnProfileLevelUp("mailbox", …)`）、可靠 effect 的生产与消费、模块启动接线，不用手写服务初始化。奖励规则归工程：`game/entities/player/mailbox_profile_rules.go` 中的 `MailboxProfileReward(from, to)`，缺省返回零值——不发奖，也不发 effect。这个函数变量只在启动前设置；热更时在函数内部读线程安全的规则源，不并发改写变量。

一次经验发放从 `from` 升到 `to`，**只调用规则一次、生成一封邮件**，奖励覆盖 `from+1…to`。例如每升一级送 10 币，返回 `mailbox.Coins(int64(to-from)*10)`；装了 bag 也可以返回 `mailbox.Item(id, count)`。不要逐级发邮件，不要在规则里做 I/O。非法附件会把错误返回给 Nest，升级、属性成长与奖励 effect 一起回滚。

`ProfileReward`（`game/mailbox/mailbox_profile.go`）在升级事务中冻结玩家号、起止等级与附件，经 `nest.Emit` 与 DAO 的变化进同一条 WAL。消费者不再查奖励表，所以热更只影响之后的升级。topic 是 `mailbox.profile_reward`（`TopicProfileReward`），消费者的 durable 名是 `<业务服务>-profile-reward-mail`；升级邮件保留 7 天。

消费走框架的 `SubscribeEffects`，邮件用 `RequestID = EffectID` 去重：邮件已发而消费记录或 ACK 没完成时，重试返回原信封，不会用新奖励覆盖第一次的邮件。坏载荷或永久非法的附件标记为毒消息并告警，传输错误保留重试。订阅登记为 App 的输入，App 先排空它、再关 Nest 与存储。

game-demo 也用这组联动，只覆盖了奖励规则：默认一次升级操作送一瓶法力药水，连升多级也只送一瓶。

这组联动的 10 条场景：默认无奖励、一级一封、多级合并、提交回滚、非法奖励、消费重放、坏载荷、空身份、传输重试、保留首次奖励。生产者的场景用正式的 Nest 与生成的 DAO 加替身提交器；消费重放调正式的 mail `Service` 加内存存储。它们证明事务内容与幂等调用的约定，不代表真实 WAL / JetStream / Mongo / Redis 故障恢复已经验收。仓库一侧另有专项测试 `TestModuleProfileMailScenariosAndMutations`，它不属于 `roost module verify`。
