# 业务模块

这里是 `roost module add` 安装的业务模块源码。模块是一个现成的业务单元：状态结构体、组件、Nest handler、协议、错误码和场景测试。安装时把源码写进工程，之后归工程所有，`project sync` / `upgrade` 不改写。

它不是生成器输出。生成器（`roost generate`、`roost add`）只产出框架骨架；模块是业务代码，只是提前写好、验收过。设计与取舍见[业务模块与配方方案](../docs/maintenance/FEATURE-2026-11-10-business-modules.md)。

普通商店购买、任务领奖在一笔 Player Nest 事务里完成，不使用 Saga；充值到账用 purchase 的订单与补发游标。只有多个独立提交方必须协调与补偿的复杂交易才用 Saga，本地多实体事务能覆盖的也不引入 Saga。

## 现有模块

| 模块 | 做什么 | 依赖 | 状态字段 | 场景数 |
| --- | --- | --- | --- | --- |
| [rules](rules/README.md) | 进入/重连推送完整规则快照，RulesGet 补取；同一 Player Nest 中读取各功能 PB | enter | 无 | 10 |
| [purchase](purchase/README.md) | 充值发货：platform 验签与订单去重、Redis 顺序待发、Player 常量大小游标；金币内置，道具与入场补发联动；渠道缺省拒绝 | wallet、托管 platform；bag / enter 联动 | `PlayerDao.Purchase`（只落库） | 21；bag 4、enter 2 条联动 |
| [gm](gm/README.md) | 运维命令登记、必填操作者与备注、带码拒绝；复用 ops `/admin`；功能模块按 `with/gm` 接入 | 无；game 底座的 ops | 无 | 9；wallet / bag / profile / mailbox / rehome 各 4 条联动 |
| [wallet](wallet/module.yaml) | 玩家货币：余额；`Add`（调用方自己判重的发放与扣除）；`Apply`（带业务键、在最近 128 次的去重窗口内只生效一次）；消费协议 `WalletSpend`；服务端发放 handler `WalletGrant` | 无 | `PlayerDao.Wallet`（同步）、`PlayerDao.WalletLedger`（只落库） | 14 |
| [checkin](checkin/module.yaml) | 每日签到：按业务日（`clock.BusinessDay`）切分，连续天数加成，奖励经货币模块在同一笔事务里发放；签到成功后执行别的功能登记的钩子（`OnCheckinClaimed`，同一笔事务） | wallet | `PlayerDao.Checkin`（同步） | 8 |
| [chat](chat/module.yaml) | 聊天：世界频道与一对一私聊的发送（`ChatSend`）、向在线玩家的推送（`ChatMessage`）、按序号翻历史（`ChatHistory`）、给服务端用的系统公告（`AnnounceWorld`）；经托管的 chat 服务存储，替换它默认拒绝一切的 collaborators。game-demo 用的就是它 | 无（要求业务服务 `uses: [chat]` 与玩家 TCP 接入，后者安装时代办） | 无 | 10 |
| [enter](enter/module.yaml) | 进入游戏：`EnterGame` 端点加载或创建 Player，每次进入都跑一遍“首次初始化”（做过没有记在档里），新玩家的初始发放可写在 `game/entities/player/enter_first.go`，模块联动可用 `player.OnFirstEnter` 登记同一事务内的发放；之后执行别的功能登记的进入钩子（`game/enter` 的 `OnEntered`；钩子拿到 `Entered{PlayerID, First, FirstAtUnixMilli, Claims}`，`Claims` 是会话的 claims，可为 nil）；安装时打开玩家 TCP 接入与自身视图；别的功能登记的进入前检查（`OnEntering`，能否决登录）在加载 Player 之前执行；进入的骨架是 `gameenter.Enter`（检查 → 加载与首次初始化 → 钩子） | 无 | `PlayerDao.Enter`（只落库） | 14 |
| [profile](profile/README.md) | 玩家档案（首次建档 1 级）：名字、等级、经验；改名协议 `ProfileRename`（非空、合法 UTF-8、至多 16 个字符，不查重）；服务端发经验的 handler `ProfileAddExp`（每级 100 经验，可连升多级）与只检查不执行的 `CheckAddExp`；给服务端按玩家号读的名片 handler `ProfileCard`（名字、等级）；工程升级钩子写在 `game/entities/player/profile_levelup.go`，模块扩展用 `OnProfileLevelUp` 按名字登记。game-demo 用的就是它 | enter | `PlayerDao.Profile`（同步，名字与等级，别的玩家也可以看）、`PlayerDao.ProfilePrivate`（同步，经验，只给本人） | 20 |
| [bag](bag/module.yaml) | 玩家背包：道具号到数量的堆叠背包（至多 200 种、单种硬上限 999999）；自带道具表 `configs/schema/item.go` 与 `configs/table/item.csv`（哪些道具存在、每种的单种上限；加道具只改 CSV 再 `roost generate`）；组件入口 `Add` / `Remove` / `Count` / `Has`；丢弃协议 `BagDiscard`；服务端发放 handler `BagGrant`；规则入口 `game/entities/player/bag_items.go`（缺省读道具表）。game-demo 用的就是它 | 无（要求 `config` 特性，`-template game` 缺省已开） | `PlayerDao.Bag`（同步） | 17 |
| [leaderboard](leaderboard/module.yaml) | 排行榜：缺省一张全服总分榜；前 N 名连同自己的名次（`LeaderboardTop`，缺省 10、至多 50）、只查自己的名次（`LeaderboardMe`）；提交分数是给服务端别的功能调的函数 `leaderboard.Submit` / `SubmitSeason`（当前总分的快照，只升不降，不带幂等键），不对客户端开放；榜名、范围、周期、分数范围、一屏多长写在 `game/leaderboard/leaderboard_board.go`。game-demo 用的就是它 | 无（要求业务服务 `uses: [rank]`） | 无 | 20 |
| [mailbox](mailbox/README.md) | 邮件：分页列出自己的邮件（`MailboxList`）、领取附件（`MailboxClaim`）；经托管的 mail 服务按“预留 → 发放 → 确认”三步领取，Player 上的领取账本保证每封至多到账一次；附件是（种类、号、数量），种类是登记点：金币内置，别的种类由登记了校验（`game/mailbox` 的 `RegisterType`）与发放方式（`RegisterMailboxGrant`）的功能提供；给服务端发件用的 `mailbox.SendTo`（一名玩家一封、附件先校验、幂等键必填）。与 profile 联动在升级时冻结奖励、通过 effect 发邮件（缺省不发奖），10 条场景；game-demo 用的就是它 | wallet（要求业务服务 `uses: [mail]`） | `PlayerDao.MailboxLedger`（只落库） | 32 |
| [task](task/README.md) | 事件任务：每日/每周/永久与前置链，配置表，金币/道具奖励；事务内事件联动 | wallet；bag 等联动 | `PlayerDao.Task`（同步） | 29；八组联动 25 条 |
| [stamina](stamina/module.yaml) | 体力：随业务时间惰性恢复到上限（不用定时器，客户端按同步下来的字段自己推算）；组件入口 `Spend` / `Grant`（可超上限到硬顶，超出截断）/ `Current`；花金币补满的协议 `StaminaRefill`（每业务日限次、逐次涨价）；服务端发放 handler `StaminaGrant`；规则写在 `game/entities/player/stamina_rules.go`，统一 rules 下发上限/周期 | wallet、rules | `PlayerDao.Stamina`（同步） | 25 |
| [shop](shop/README.md) | 商店：协议 `ShopBuy` 用金币买道具，扣钱、发货、记限购在一笔事务里；每业务日 / 终身限购；请求键在最近 64 次成功购买的窗口内防重发；商品目录为配置表 `shop_goods.csv`，统一 rules 下发 | wallet、bag、rules | `PlayerDao.Shop`（同步）、`PlayerDao.ShopLedger`（只落库） | 20 |
| [skillbook](skillbook/module.yaml) | 技能目录：服务启动时编译 `game/skills/*.json`，失败拒绝起服；`SkillCatalog` 查询同一份目录，返回排序后的技能 ID 与 warning 数；默认带一个可编译技能，工程修改 JSON 与 `CompileEnvironment` | 无 | 无（服务级只读目录） | 11 |
| [activity](activity/README.md) | 共用 activity.Component 五阶段活动状态机：一次性/日历循环、可信条件解锁、自定义流程、有界恢复；个人宿主 | 无；configdata / 个人持久定时器由安装器接入 | PlayerDao.Activity（只落库） | 21 |
| [activity_world](activity_world/README.md) | 全服/服务器组权威 Entity，独立服务、ownership 恢复扫描 | activity | ActivityWorldDao.ActivityWorld（只落库） | 4 |
| [activity_alliance](activity_alliance/README.md) | 既有联盟 Entity 的活动适配，扩展 alliance 配置/RPC | activity、alliance | AllianceDao.ActivityAlliance（只落库） | 4 |
| [alliance](alliance/README.md) | 独立联盟服务单写者、路由 RPC / 三个跨域 Saga、Mirror 展示与换服惰性校正 | bag；代办 saga / Mirror 与 alliance 服务 | AllianceDao、AllianceMemberDao、PlayerDao.Alliance / SagaReceipts | 49 |
| [team](team/README.md) | Remote 共享权威队伍，六个 PB 端点；成员与 Player 引用同事务，邀请有界 | remote_entity Mod（安装时代办） | TeamDao、PlayerDao.Team（只落库） | 27 |
| [matchmaking](matchmaking/README.md) | 单人/整队按服分池；持久意图、取消竞态、结果补查、组队冻结 | team、matchroom；托管 match | PlayerDao.Matchmaking | 16 |
| [matchroom](matchroom/README.md) | MatchID 幂等移交到 Room，固定准入名单、掉线保座 | room、Mongo | RoomDao.Matchroom、持久房号映射 | 12 |
| [room](room/README.md) | 自带 Room Entity、DAO 与 lifecycle：1～64 席、加入/退出、房主交接、重连代际、关闭；服务端开房，客户端只能操作自己的当前连接 | 无（安装时代办玩家 TCP 接入；鉴权与启用配置仍归工程） | `RoomDao`（只落库；成员读 PB 快照） | 22 |
| [turn](turn/README.md) | 固定名单的顺序回合：命令序号与重放、持久超时、离线不追帧；PB 查询；无默认伤害/胜负规则 | room | `RoomDao.Turn` 与实体定时器节点（只落库） | 25 |
| [login](login/README.md) | 登录鉴权：玩家 TCP 握手校验 account 签发的会话令牌（`session:<player_id>:<token>`），把承载服 / 原服 / 角色创建时间 / 绑定代数 / 换服标记写进会话；接管鉴权桩与 account 的 collaborators（开发令牌登录 `login.dev_token`、Redis 玩家号计数器；渠道入口 `login_channels.go` 缺省全拒） | 无（要求业务服务 `uses: [account]`；玩家 TCP 接入安装时代办，监听要自己 `roost config enable player-tcp`） | 无 | 19 |
| [servertime](servertime/module.yaml) | 服务器时间：客户端拿到服务器的业务时间、当前业务日、前后两个业务日、本业务日起点与下一次每日重置的时刻（拉取 `ServertimeSync`，原样带回 `ClientTick`；推送 `ServertimeNow`）；给别的功能调的纯函数 `servertime.At(nowUnixMilli)` / `Now()`，与 `clock.BusinessDay` 同一份日历；`PushNow` 按能力名取玩家 TCP 传输；对外展示的时区名写在 `game/servertime/servertime_zone.go` | 无（玩家 TCP 接入安装时代办） | 无 | 12 |
| [rehome](rehome/module.yaml) | 单个玩家换服（改绑 game sid）：申请协议 `RehomeApply` 与服务端入口 `Move`；account 的发起 / 提交 / 中止，结论按它带回的当前绑定判定；源服从“离开中”的标记起在写闸门拒绝这名玩家新的请求，受理之后断开会话、等在途的请求执行完、复核检查、跑离开钩子，再交出实体（之后拒写），目标服首次进入时到达；可去的服与冷却写在 `game/rehome/rehome_rules.go`；三个登记点（换服前检查、离开时、到达后）；给 saga 准入用的 `rehome.Binding`（这名玩家现在绑定在哪个服、是否正在换服）。game-demo 用的就是它 | 无（要求业务服务 `uses: [account]`） | `PlayerDao.Rehome`（只落库） | 40 |
| [actor](actor/README.md) | 共用 Body：移动、定身/沉默、心跳、存档位置与提交后位置通知；可独立安装 | 无（代办 heartbeat） | Player 的 Actor / ActorSaved | 12 |
| [scene](scene/README.md) | 多地图/分线、进出与排空、Block AOI、时间域、区域回调与幂等交互；自身和公开视图分离 | actor | SceneDao；Player 场景交互回执 | 26 |
| [monster](monster/README.md) | 正式实体生命周期、AI/actionflow 巡逻追击、死亡隐藏、同 ID 分代重生 | actor、scene | MonsterDao（运行时重建） | 25 |
| [attribute](attribute/README.md) | 生成的属性 profile 与 Container：基础/装备/最终三层、升级成长 | profile | AttributeBase / AttributeGear / AttributeFinal | 16 |
| [equipment](equipment/README.md) | 配置表槽位、穿卸、背包原子扣回；不包含物品实例和随机词条 | bag | Equipment | 16 |
| [combat](combat/README.md) | 双人同意战斗、正式技能 Runtime / CombatDAO、Buff 与冷却恢复 | actor、attribute、skillbook | CombatDAO；Player 的公开投影与私有 checkpoint | 36 |
| [dungeon](dungeon/README.md) | 正式 level 阶段推进、可信通关事件、领奖与赛季计数；客户端不能提交胜利 | profile | DungeonDao + LevelDAO；玩家领取账本 | 19 |
| [terrain](terrain/README.md) | 地形块、笔刷与建筑，玩家序号去重，持久事实重建 View/占位；单进程有界地图 | scene | TerrainChunk / TerrainBuilding；玩家编辑回执 | 23 |
| [naming](naming/README.md) | Directory 两阶段占名、未知结果恢复；接管 ProfileRename 的唯一名字流程 | profile（代办 directory Mod） | Naming | 15 |
| [cdkey](cdkey/README.md) | 一码一次或一人一次；配置表、持久兑换意图、以稳定键发邮件 | mailbox（代办 directory Mod） | Cdkey 的有限意图/回执 | 12 |
| [notice](notice/README.md) | 可信公告与跑马灯入口，经 chat 的 AnnounceWorld；不默认定时播放 | chat | 无 | 8 |
| [gacha](gacha/README.md) | 配置概率/保底、十连；扣钱、发卡和请求去重同事务 | wallet、bag | Gacha / GachaLedger | 24 |
| [friends](friends/README.md) | 好友、申请与黑名单；双方在线、同进程双 Player 事务 | 无 | Friends | 12 |
| [idle](idle/README.md) | 惰性离线收益、时长上限、回拨处理与序号重放 | wallet | Idle | 19 |

两个模块都装着时还会多出**联动文件**（不论先装哪个，在第二个装上的那一次写）：

| 联动 | 做什么 | 场景数 |
| --- | --- | --- |
| activity + enter | 持久创建锚点与进入对账，锁外恢复 pending | 1 |
| activity + profile | 升级与条件解锁在同一 Player 事务 | 1 |
| activity + leaderboard | 活动实例分榜身份 | 1 |
| activity + mailbox | 结算发件使用稳定期次/收件人键 | 1 |
| alliance + enter / rehome | 进入/重连对账与 HomeSID 条件修正 / 原生步骤按当前绑定准入 | 各 2 |
| matchmaking + rehome | 排队与换服双向互斥，未知结果继续保持屏障 | 2 |
| team + rehome | 在队伍中拒绝换服，换服意图未清除前拒绝建队/入队 | 2 |
| chat + enter | 玩家进入游戏时加进世界频道的在线名单（`game/chat/chat_enter.go`） | 2 |
| mailbox + enter | 玩家进入游戏时把属于他的全服邮件合并进邮箱（`game/controllers/mailbox/mailbox_enter.go`）；画像缺省取首次进入的时间，会话带着 account 的角色创建时间与区服时改用它们（读 `gameenter.Entered.Claims`） | 5 |
| mailbox + bag | 道具附件：登记 `item` 这个附件种类，领取时道具进背包，与领取账本同一笔事务；背包拒绝时邮件保持未领（`game/mailbox/mailbox_item.go`、`game/entities/player/mailbox_item.go`） | 9 |
| task + checkin | 签到成功时把每日任务“签到一次”推进一次，与签到同一笔事务（`game/entities/player/task_checkin.go`） | 3 |
| task + wallet / shop / stamina / profile / dungeon | 首次消费、购买份数、消耗点数、提升等级数、副本领奖在原事务中推进 | 6 / 3 / 3 / 2 / 2 |
| task + bag | 道具奖励与任务领奖在同一笔事务 | 3 |
| task + leaderboard | rank 成功答复后独立推进，尽力通知且重复提交计数 | 3 |
| shop + rules / stamina + rules | 统一导出商品目录 / 体力规则，保持快照独立 | 各 2 |
| profile + enter | 初次建档 1 级；不触发升级奖励，重进/冷加载不重置 | 4 |
| attribute + enter | 按初始等级建立基础/最终属性，同事务回滚 | 2 |
| login + enter | 进入前检查：会话记的承载服不是本服（或没有记）答 `login_elsewhere`，不加载也不创建 Player（`game/login/login_enter.go`） | 2 |
| servertime + enter | 玩家每次进入游戏时把服务器此刻的业务时间推给他（`game/controllers/servertime/servertime_enter.go`）；推送不成只记日志，不让登录失败 | 3 |
| rehome + enter | 进入前检查（正在换服答 `rehome_moving`，绑定在别的服答 `rehome_elsewhere` 并给出目标服；源服重启后在这里把停在半路的换服接着做完）；进入之后做到达与意图对账（`game/controllers/rehome/rehome_enter.go`） | 5 |
| attribute + equipment | 穿卸同事务重算装备层与最终属性，加载时重建 | 8 |
| dungeon + leaderboard | 提交持久通关计数快照；失败保留待补报标记 | 3 |
| scene + chat | 同地图范围内瞬时聊天，锁内取得收件人快照、锁外推送；不提供历史 | 5 |
| scene + profile / attribute / equipment | 各自显式登记公开字段，快照与增量均检查权限 | 各 2 |
| combat + scene / equipment | 公开战斗投影 / 真实换装对冻结输入的影响 | 各 2 |
| terrain + monster | 关图清本图怪物；失败保留资源与事实供重试 | 2 |

以 `roost module list` 的输出为准。一个新工程装 `enter` 加需要的业务模块，就有了登录后的第一条请求、客户端可见的状态和业务本身。game-demo 目前用的是 `enter`、`chat`、`leaderboard`、`bag`、`profile`、`mailbox`、`skillbook`、`rehome`、`attribute`、`equipment`、`team`、`alliance`（连带 `wallet`）。

## 使用

```sh
roost project new shop -module example.com/shop -template game
cd shop
roost module list                  # 看有什么
roost module plan checkin          # 只预览：依赖、追加的字段、分配的号、写入的文件
roost module add checkin           # 安装（连同依赖 wallet）并运行生成器
roost module verify                # 模块自带场景 + build / vet / id check / generate --check
```

agent 用 `--json`：`list`、`plan`、`add`、`verify` 都支持；`plan` 与 `add` 输出同一个结构，`applied` 区分是否已执行。

标了同步的状态要打开玩家自身视图客户端才收得到。装 `enter` 模块时安装器会代办（`roost add transport tcp`、`access.player.self_sync: true`、`roost project sync`）；不装它就自己做，步骤见 skill 的[按需求组合 §4A](../docs/agent-skills/roost-genre/references/compose.md#4a-让客户端看到状态)。

安装之后直接改工程里的文件（例如在 `game/entities/player/checkin_component.go` 里改奖励规则），改完再跑 `roost module verify`。模块库里没有的需求用 `roost add entity / component / dao / handler / protocol / endpoint` 手写，可以照着已装模块的文件写。

## 安装做了什么

1. **前置骨架**：缺少的 configdata / heartbeat / directory / remote_entity Mod 使用正式 `add mod` 代办；attribute feature 由模块启用并 `project sync`。实体还没有主 DAO 时执行 `roost add dao <Entity> --entity <Entity>`；模块有协议而工程还没有玩家接入时执行 `roost add access player --service <service>`。
2. **模块文件与自带实体**：先写模板；`entities` 声明的实体分配实体号并由模块提供 Entity / DAO，安装器补 lifecycle。已有实体、DAO 或 lifecycle 的标准落点被占用时拒绝，不接管业务文件。
3. **状态**：把模块的状态字段追加到实体主 DAO 的**末尾**。类型是模块自带的嵌套结构体（`db/def/<module>.go`）。
4. **组件与附加 DAO**：把模块组件接到实体上；`daos: [{field: combatState, type: CombatDAO, tag: combat_state}]` 追加独立 DAO 的聚合声明，复用同一份实例的加载、事务与恢复。字段/集合被占用时拒绝。`sync_packer` 只接明确的模块视图工厂，已有业务 packer 时拒绝覆盖。
5. **记录**：在 `roost.yaml` 的 `modules.<name>` 记下这个模块拥有的文件、字段、分配到的号、目标 `service`、`requires` 与可选 `runtime`。
6. **生成**：运行全部生成器。自带实体时同时刷新 owner / handler / shutdown 接线；有 `runtime` 时生成服务适配层。自身同步在 packer、附加 DAO 与模块文件全部就位后开启，并在同一暂存树补齐推送协议。全部生成物与需刷新配置一起暂存、提交，失败不会留下半套接线。

模块可用 `services` 声明独占的新业务服务；`existing: true` 只给已存在服务追加 Mod/RPC 及缺少的配置段，不能接管实体所有权或覆盖 `service.go`。新服务模板、开发/生产示例配置及 Kubernetes Secret 示例的修改都纳入安装失败回滚。

两个模块之间的**联动文件**在第 4 步之后写：这次装的模块与工程里已有的（或同一次装的）模块凑成一对、而这一对有联动时，把联动文件写进工程，归属记在拥有它的模块的 `with` 下。`plan` 与 `add` 的输出里是 `glue <模块> with <另一个模块>`。两个都早已装好的不补。

第 2～6 步任何一步失败，模块写过的业务文件与 `roost.yaml` 恢复原样。已安装的模块不再处理，重复执行没有变化。没有卸载命令：分配过的号与存储键不回收，要去掉一个模块时按 `roost.yaml` 里的归属清单手工删；先检查是否仍被其他模块依赖。服务不再包含任何 runtime 模块时，还需删除该服务的 `modules_gen.go` 并执行 `project sync`，让 bootstrap 恢复业务 `New()`；仍有 runtime 模块时由生成器重写适配层。

## 写一个模块

目录结构：

```
modules/<name>/
  module.yaml          清单
  files/<工程内路径>.tmpl   模板，按安装后的真实路径存放
```

### 自带实体

`entities: [Room]` 声明模块拥有的实体，`entity: Room` 仍表示组件/追加状态的挂靠目标，两者含义不同。每个自带实体必须有 `files/game/entities/<snake>/entity.go.tmpl` 与 `files/db/def/<snake>.go.tmpl`；实体号用 `{{ID:entity:Room}}`，所属服务用 `{{GAME_SERVICE}}`。安装器复用 `roost add lifecycle` 的实现生成 `game/lifecycle/<snake>.go`，模板不要再带第二份 lifecycle。

实体、DAO、lifecycle 都记入归属清单，并在模块安装的回滚边界内。它们不作为先执行、失败后保留的前置命令。实体号与其他号一样按最小空号分配，扫描已有代码与安装记录，已分配号不回收。依赖模块可以通过 `requires` 使用先安装的实体；需要追加状态时仍声明自己的 `entity` / `state` / `component`。

安装器在生成暂存区先同步服务归属，再生成全部接口；bootstrap、相关 shutdown 配置、模块源码和清单失败时一并回退。普通 `roost generate` 的行为不变，手工修改实体归属仍需 `project sync`。第一个使用者是 [room](room/README.md)；scene、monster、dungeon、terrain 同样复用，team 同样支持；alliance 尚未迁移。

### 服务级启动与停机

需要服务级资源的模块在 `module.yaml` 声明：

```yaml
runtime:
  package: game/skillbook
  start: Start
```

工程内入口为 `func Start(*app.Registry) (func(context.Context) error, error)`。返回的停止函数可为 nil；启动失败但已有部分资源时，也要返回对应停止函数。启动入口在全部框架 Mod 启动后、业务 `Service.Init` 前运行，可以发布 Registry 能力。按名称稳定遍历模块，并先启动 `requires` 列出的依赖；业务先停，再按反向顺序停模块。模块不能在启动时依赖业务 `Service.Init` 才发布的能力。

安装器根据 `roost.yaml` 生成 `internal/service/<service>/modules_gen.go`，bootstrap 用 `NewWithModules()` 构造适配层。它嵌入实际 `*Service`，保留配置声明、Nest 接线等可选接口，**不改业务 `service.go`**。手动绕过 bootstrap 使用 `New()` 不会启动模块；嵌入工程应使用 `NewWithModules()`。`project sync` 与 `generate` 重写适配层，模块业务文件仍归工程。

App 在 Init 失败后调用 Shutdown，适配层只清理已经启动（包括部分启动失败）且提供停止函数的模块；业务 Init 被调用后即纳入清理。停机函数必须等待真实退出并遵守 ctx，返回错误时停止继续关闭依赖，保留回调；再次 Shutdown 从未完成处继续，不重复关闭已成功退出的模块。模块停止共享业务服务的停机时间预算，不自动增加超时时间。失败不会自动重启；当前一个服务实例只允许一轮 Init → Shutdown，调用方不能并发调用生命周期。

模块的玩家侧只安装到一个业务服务，依赖属于同一服务；模块也可声明自己独占的新业务服务（如 alliance）。跨进程调用仍走框架 RPC，不是模块启动依赖。新增后台消费者还须按 App 的输入关闭契约登记 ingress，不能以取消 context 代替排空。`skillbook` 是首个使用者：只读目录不需要后台任务，客户端不再临时编译第二份目录。

### 清单

```yaml
name: wallet                 # 与目录名相同
summary: 一句话说明
requires: []                 # 先装的模块
conflicts: []                # 不能同装的模块
needs:
  mods: [nest]               # 业务服务必须有的 Mod
  features: [protocol, entity, dao, errcode]
  uses: []                   # 业务服务必须 uses 的托管框架服务（rank、mail……）
  uses_rpcs: []               # 模块新增的业务 RPC，安装器接到目标业务服务
entity: Player               # 状态与组件挂在哪个实体上
entities: []                 # 自带实体名字；与挂靠既有实体分开，见上文
state:                       # 追加到主 DAO 末尾的顶层字段
  - field: Wallet            # 以模块名开头
    type: WalletState        # 模块在 db/def 里定义的嵌套结构体
    store: wallet            # 存储键，以模块名开头，发布后不要改
    dao: persist,sync
    comment: 写进 DAO 定义的注释
component: Wallet            # 挂在实体上的组件
components: []               # 自带实体的其他组件名，安装时分配类型号
services: {}                 # 模块独占的新服务；mods/rpcs/entities，已有同名服务拒绝接管
sagas: []                    # name/steps/routed/receipts；定义与实体回执原子安装，业务由 runtime 提供
timer_types: []              # 实体内的持久定时器类型名，例如 turn 的 [TurnDeadline]
protocols: [WalletSpend]     # 模板里 {{ID:protocol:<name>}} 用到的名字
errcodes: [wallet_short]     # 模板里 {{ID:errcode:<name>}} 用到的名字
access:                      # 要求的玩家接入形态，由安装器代办（见下）
  transport: tcp             # 没有玩家 TCP 接入就先 roost add transport tcp
  self_sync: true            # 打开 access.player.self_sync：标了 sync 的字段推给玩家本人
  lifecycle: true            # Entity 没有 lifecycle 就先 roost add lifecycle <Entity>
replaces: []                 # 要接管的生成桩（见下），必须也在 files/ 里
with:                        # 与别的模块的联动（见下），文件在 with/<另一个模块>/ 下
  enter:
    summary: 一句话说明这组联动做什么
    verify:                  # 可以不写
      packages: [./game/chat]
      run: ^TestWithChatEnter      # 必须以 ^TestWith<本模块> 开头
      pass: 2
verify:
  packages: [./game/handler]
  run: ^TestWallet
  pass: 8                    # 恰好这么多条通过、零跳过
```

`replaces` 用于生成器建一次、默认拒绝一切、留给项目实现的文件——认识两种：托管服务的 `internal/service/<name>/collaborators.go` 与玩家 TCP 的鉴权桩 `internal/access/player/tcp/auth.go`（安装器的 `generatedStubKinds`；工程还没有 TCP 接入时，桩由前置命令先生成、随即被替换，plan 照样列出）。安装时只有它仍是没改过的生成桩才替换；业务改过就拒绝安装。chat 模块用它接管 chat 服务的协作者。替换后的文件里不要再出现 `is not configured` 这句话：`project doctor` 靠它判断“还是拒绝桩”。

`with` 是模块之间的联动：`modules/<name>/with/<另一个模块>/<工程内路径>.tmpl`，两个模块都装着时才写进工程。用它把本模块接到对方留的**登记点**上——对方提供登记函数，联动文件在本模块自己的包里用 `init` 登记。登记点有两种：事务之外的，放在一个不依赖任何 controller 与实体的小包里（enter 的 `game/enter.OnEntered`：失败只记日志；enter 的 `OnEntering`：返回错误就否决这次进入，所以要返回带码错误）；事务之内的，放在实体包里、拿到实体本身（checkin 的 `OnCheckinClaimed`：与对方的写入同一笔事务，返回错误会让对方那一次操作整笔回滚；mailbox 的 `RegisterMailboxGrant`：一种附件怎么发）。一个登记点分成“纯函数的一半”与“实体上的一半”时两处都要登记（mailbox 的附件种类：校验在 `game/mailbox`，发放在实体包）。规则：

- 联动文件归写它的模块所有，放在这个模块自己的包里，只 import 对方的登记点包；对方不认识它。
- 登记点只在 `init` 里登记、之后只读；这是“不用进程级全局变量”的唯一例外，与组件工厂的登记同理。登记点的包不 import controller 与实体包，谁都能 import 它而不成环。
- 联动文件只能用模块已声明的号（它可能在模块装好很久之后才写进工程），不能申请新号、不能加状态字段。
- 联动自带的场景以 `TestWith<本模块>` 开头，单独计数（`roost module verify` 里是 `<模块>+<另一个模块>`），两个模块各自的场景数不随对方装没装而变。
- 整个模块库里没有两处写同一个落点（目录校验会拒绝）。

`files` 不写在清单里，由 `files/` 目录读出。清单与模板必须对得上：模板用到的每个 `{{ID:...}}` 都要声明，声明的每个名字都要用到。

### 模板占位

| 占位 | 替换成 |
| --- | --- |
| `{{MODULE}}` | 工程的 Go module 路径 |
| `{{PROJECT}}` | 工程名 |
| `{{GAME_SERVICE}}`、`{{GAME_SERVICE_PKG}}` | 模块装进的业务服务名与它的 Go 包名 |
| `{{ID:entity:<name>}}`、`{{ID:protocol:<name>}}`、`{{ID:errcode:<name>}}`、`{{ID:component:<name>}}` | 安装时从 `roost.yaml` 的 `ids` 段分配的号 |
| `{{ID:timer:<name>}}` | 实体内持久定时器类型号，写入 `timers.<entity>.types` |
| `{{TIMER_COMPONENT}}` | 实际定时器组件的 PascalCase 名 |

### 必须遵守的约定

- **模块可以做全、做复杂，但简单用法必须照样能用**（维护者 2026-11-11）：game-demo 里有的能力（查配置表、分赛季、一次请求带回多样数据……）做进模块，不为了“简单”砍功能；全新的 `-template game` 工程不做任何配置直接 `roost module add <模块>`，缺省就要能工作、验收全绿，复杂能力在此之上填数据或打开。判断一个功能放不放进模块：放进去之后，不用它的简单工程会不会被迫多做事——不会，就放进模块。工程要改的规则与数据放进归工程所有的业务文件（或模块自带的配置表），逻辑文件不带规则。

- **状态自带**：只动自己的状态结构体，不往别的模块的结构体里加字段。挂在已有实体上的状态一律是嵌套结构体，不新建 DAO。
- **同步按顶层字段决定**：`dao: persist,sync` 下发给客户端，`dao: persist` 只落库。既有客户端可见状态又有服务端账本的模块写两个字段（wallet 就是）；有“别的玩家也能看”与“只给本人”两种状态的模块同样写两个字段（profile 就是：名字等级一个、经验一个），工程的同步视图按顶层字段挑。嵌套类型内部的字段只写 `store` 键。
- **号不写死**：协议号、错误码、组件号一律用 `{{ID:...}}`。
- **模块可以带配置表**：把 `configs/schema/<表>.go` 与 `configs/table/<表>.csv` 当普通模板放进 `files/`，清单 `needs.features` 加 `config`；表的登记、CSV 转换与 `generated.<表>ByID` 访问器由 `roost generate` 产出，不需要安装器另做什么。表名全库唯一；工程已有同名的表时落点被占用，装不了。读配置表的模块，场景测试带自己的表（只登记自己那张，写到临时目录），装载后把缺省 Store 与 `fctx.RuntimeConfig()` 一起换回（照 `modules/bag` 的测试）；依赖它的模块的测试同样要先装表。
- **文件名带模块名**：`db/def/<module>.go`、`internal/errors/<module>.go`、`game/handler/<module>_<handler>.go`、`protocol/def/<module>_<protocol>.go`、`game/controllers/<module>/`。协议与 Nest handler 一个一个文件（`roost generate` 按文件名找它们）。
- **自己的 controller 域**：`//roost:protocol ... handler=<module>`，不往别的模块的 controller 里加方法。
- **跨模块协作**：依赖的模块（`requires`）的组件在同一个实体上时，在自己的组件里直接调用它（同一把实体锁、同一笔事务，见 checkin 调 wallet）；不在同一个实体上时用多实体 Nest handler。不依赖对方、只是“对方也装着时顺带做一件事”，用联动文件接到对方的登记点上（见上）。不用进程级全局变量保存业务状态。
- **测试只断言自己的事实**：同一个 DAO 上还有别的模块的字段。例如断言“我的字段同步、我的账本不同步”，不要断言“同步字段恰好是这几个”。测试替身（getter、提交器）自己带一份，不借用别的模块的测试文件——那些文件归工程所有，可能被改。
- **长期“只做一次”记在自己的状态里**：每日、每局、每个订单这类周期性发放，先判自己的状态再调 `WalletComp().Add`；不要拿递增的键去占货币的去重窗口（它只留最近 128 次）。
- **日界用业务日**：`clock.BusinessDay(time.UnixMilli(nowUnixMilli))`，不自己除以一天的毫秒数。
- **场景测试不用子测试**：`roost module verify` 按 `--- PASS` 的行数核对 `verify.pass`，`t.Run` 的子测试也会被数进去。
- **回滚统一走 DAO**：组件不保存事务会改的状态，不登记 undo。
- **业务时间**：handler 的最后一个参数写 `nowUnixMilli int64`，由 Nest 填这条消息冻结的业务时间；测试用 `clock.SetSource` 拨。

### 写的顺序

最省事的做法是先在一个真实工程里把功能手写出来、跑通，再搬成模板：

1. `roost project new demo -module example.com/demo -template game --skip-deps`，`go mod edit -replace` 指向本仓库。
2. 用 `roost add component / protocol / errcode` 和手写把功能做完，`roost generate`，写场景测试并跑通。
3. 把业务文件拷到 `modules/<name>/files/`，加 `.tmpl` 后缀，把 module 路径换成 `{{MODULE}}`、把号换成 `{{ID:...}}`。
4. 写 `module.yaml`。
5. 在全新工程上 `roost module add <name>` 与 `roost module verify`；再和别的模块一起装一遍。

## 改这里的东西之后

```sh
GOWORK=off go test -count=1 -run 'TestModule|TestAppendDaoFields' ./codegen/internal/roost
bash scripts/test-modules-generated.sh
```

第一条包含在生成工程上真实安装并验收的用例（约一分钟）。第二条是发版门禁里的那一条：全新工程、自定义 module 名、逐个模块安装、组合安装、重复安装无变化。

## 从配方创建工程

```sh
roost project new mygame -module example.com/studio/mygame -recipe casual
roost project new arena -module example.com/studio/arena -recipe turn-pvp
```

配方以 `game` 为底，复用同一个模块安装器、自动联动、ID 分配和自身同步；可用 `-services arena` 指定业务服务名。不能与 `-template game-demo` 混用。所有文件在新工程的暂存树里完成，失败不留下半个目标工程。已装模块仍由 `module list / verify` 管理，配方不是第二种可重复安装的模块。

- `casual`：enter、wallet、checkin、task、stamina、shop、bag、mailbox；新玩家 100 金币写在 `recipe_first_enter.go`，由事务内 `OnFirstEnter` 发放。重复进入不再发；提交失败时余额与初始化标记一起回滚。
- `turn-pvp`：enter、profile、room、turn、leaderboard、team、matchroom、matchmaking；客户端 MatchEnqueue → MatchStatus → RoomJoin → TurnStart → TurnEnd。没有替业务假设胜负规则、排行榜结算或客户端开房授权。

两者都提供正式 Account session 认证器，拒绝错误承载服与在途迁移，开启开发 TCP。业务仍要接入 Account 的平台身份验证与持久角色号分配，不能把测试身份当作上线鉴权。生产 TCP 配置仍需显式开启。操作顺序和边界写入新工程的 `RECIPE.md`，规则文件归业务所有，后续 sync 不覆盖。

`OnFirstEnter(name, hook)` 只在 `init` 登记，按名字排序；先跑原来的 `firstEnter`，再跑登记钩子，最后写初始化标记。钩子在同一把 Player 锁内，只改该玩家 DAO，禁止 I/O/发消息，失败返回错误交给 Nest 整笔回滚。不要把它与事务外、失败只记录日志的 `game/enter.OnEntered` 混用。

配方验收入口：`GOWORK=off go test -count=1 -run '^TestRecipe' -timeout 15m ./codegen/internal/roost`。生成两个自定义 module/service 的工程，真实 TCP + Robot 传输 + PB + Nest 场景分别 9 / 7 条（含鉴权与回滚检查），再做模块 verify、build、vet 与漂移检查；Account/邮件/排行客户端、持久提交是替身，自身 Sync 的推送驱动和真实存储未在这里联合验收。

## 模块的持久定时器

清单写 `timer_types: [TurnDeadline]`，同时声明 `entity` / `component`；模板以 `{{ID:timer:TurnDeadline}}` 定义显式 int32 常量。安装器复用已有的实体定时器，缺失时补同一套组件、DAO 节点与 bootstrap 驱动。模板中的 `{{TIMER_COMPONENT}}` 是实际定时器组件的 PascalCase 名，支持项目自定义组件名。

模块在实际定时器组件上实现 `register<模块组件>Timers(*entitytimer.Handlers)`；安装器按语法树把调用追加到业务登记函数（只允许直接顺序登记，不在未知控制流后插代码）。构造阶段只绑定定时器组件的方法，真正触发时再找其他组件；不能依赖组件构造顺序。

类型号按实体分配，从 1 起避开已有 int32 常量、静态 Register 实参、`timers.<entity>.types` 和已安装记录；新组件的示例占 1。无法静态确定时拒绝。不要把登记表交给外部函数或缓存起来后再登记；本包显式接收 `*entitytimer.Handlers` 的登记方法可互相调用。已分配的号不回收，作用域写入 `modules.<module>.timer_entity`；归属与 types 不一致时清单校验失败。普通工程级 `roost id next` 不分配 timer 类型。

该步骤属于模块安装事务：类型号、DAO / Entity 改写、业务回调登记与生成接线一起回滚。`plan --json` 的 `timer` 列出作用域、复用/新建和最终定时器定义。首个真实使用者与完整边界见 [turn](turn/README.md)。
