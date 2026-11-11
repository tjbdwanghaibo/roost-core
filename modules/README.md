# 业务模块

这里是 `roost module add` 安装的业务模块源码。模块是一个现成的业务单元：状态结构体、组件、Nest handler、协议、错误码和场景测试。安装时把源码写进工程，之后归工程所有，`project sync` / `upgrade` 不改写。

它不是生成器输出。生成器（`roost generate`、`roost add`）只产出框架骨架；模块是业务代码，只是提前写好、验收过。设计与取舍见[业务模块与配方方案](../docs/maintenance/FEATURE-2026-11-10-business-modules.md)。

## 现有模块

| 模块 | 做什么 | 依赖 | 状态字段 | 场景数 |
| --- | --- | --- | --- | --- |
| [wallet](wallet/module.yaml) | 玩家货币：余额；`Add`（调用方自己判重的发放与扣除）；`Apply`（带业务键、在最近 128 次的去重窗口内只生效一次）；消费协议 `WalletSpend`；服务端发放 handler `WalletGrant` | 无 | `PlayerDao.Wallet`（同步）、`PlayerDao.WalletLedger`（只落库） | 9 |
| [checkin](checkin/module.yaml) | 每日签到：按业务日（`clock.BusinessDay`）切分，连续天数加成，奖励经货币模块在同一笔事务里发放；签到成功后执行别的功能登记的钩子（`OnCheckinClaimed`，同一笔事务） | wallet | `PlayerDao.Checkin`（同步） | 8 |
| [chat](chat/module.yaml) | 聊天：世界频道与一对一私聊的发送（`ChatSend`）、向在线玩家的推送（`ChatMessage`）、按序号翻历史（`ChatHistory`）、给服务端用的系统公告（`AnnounceWorld`）；经托管的 chat 服务存储，替换它默认拒绝一切的 collaborators。game-demo 用的就是它 | 无（要求业务服务 `uses: [chat]` 与玩家 TCP 接入，后者安装时代办） | 无 | 8 |
| [enter](enter/module.yaml) | 进入游戏：`EnterGame` 端点加载或创建 Player，每次进入都跑一遍“首次初始化”（做过没有记在档里），新玩家的初始发放写在 `game/entities/player/enter_first.go`；之后执行别的功能登记的进入钩子（`game/enter` 的 `OnEntered`）；安装时打开玩家 TCP 接入与自身视图 | 无 | `PlayerDao.Enter`（只落库） | 6 |
| [profile](profile/module.yaml) | 玩家档案：名字、等级、经验；改名协议 `ProfileRename`（非空、合法 UTF-8、至多 16 个字符，不查重）；服务端发经验的 handler `ProfileAddExp`（每级 100 经验，可连升多级）；升级钩子写在 `game/entities/player/profile_levelup.go` | 无 | `PlayerDao.Profile`（同步） | 16 |
| [bag](bag/module.yaml) | 玩家背包：道具号到数量的堆叠背包（至多 200 种、单种至多 999999）；组件入口 `Add` / `Remove` / `Count` / `Has`；丢弃协议 `BagDiscard`；服务端发放 handler `BagGrant`；哪些道具号合法写在 `game/entities/player/bag_items.go`（缺省只要求为正） | 无 | `PlayerDao.Bag`（同步） | 12 |
| [leaderboard](leaderboard/module.yaml) | 排行榜：缺省一张全服总分榜；前 N 名连同自己的名次（`LeaderboardTop`，缺省 10、至多 50）、只查自己的名次（`LeaderboardMe`）；提交分数是给服务端别的功能调的函数 `leaderboard.Submit` / `SubmitSeason`（当前总分的快照，只升不降，不带幂等键），不对客户端开放；榜名、范围、周期、分数范围、一屏多长写在 `game/leaderboard/leaderboard_board.go`。game-demo 用的就是它 | 无（要求业务服务 `uses: [rank]`） | 无 | 20 |
| [mailbox](mailbox/module.yaml) | 邮件：分页列出自己的邮件（`MailboxList`）、领取附件（`MailboxClaim`）；经托管的 mail 服务按“预留 → 发放 → 确认”三步领取，Player 上的领取账本保证每封至多到账一次；附件是（种类、号、数量），种类是登记点：金币内置，别的种类由登记了校验（`game/mailbox` 的 `RegisterType`）与发放方式（`RegisterMailboxGrant`）的功能提供 | wallet（要求业务服务 `uses: [mail]`） | `PlayerDao.MailboxLedger`（只落库） | 28 |
| [quest](quest/module.yaml) | 每日任务：按业务日惰性清零的进度；服务端推进（组件入口 `Advance`、不对客户端开放的 handler `QuestAdvance`）；领奖协议 `QuestClaim`，金币经货币模块在同一笔事务里到账；任务表写在 `game/entities/player/quest_catalog.go` | wallet | `PlayerDao.Quest`（同步） | 18 |
| [stamina](stamina/module.yaml) | 体力：随业务时间惰性恢复到上限（不用定时器，客户端按同步下来的字段自己推算）；组件入口 `Spend` / `Grant`（可超上限到硬顶，超出截断）/ `Current`；花金币补满的协议 `StaminaRefill`（每业务日限次、逐次涨价）；服务端发放 handler `StaminaGrant`；规则写在 `game/entities/player/stamina_rules.go` | wallet | `PlayerDao.Stamina`（同步） | 25 |
| [shop](shop/module.yaml) | 商店：协议 `ShopBuy` 用金币买道具，扣钱、发货、记限购在一笔事务里；每业务日 / 终身限购；请求键在最近 64 次成功购买的窗口内防重发；商品目录写在 `game/entities/player/shop_goods.go` | wallet、bag | `PlayerDao.Shop`（同步）、`PlayerDao.ShopLedger`（只落库） | 17 |

两个模块都装着时还会多出**联动文件**（不论先装哪个，在第二个装上的那一次写）：

| 联动 | 做什么 | 场景数 |
| --- | --- | --- |
| chat + enter | 玩家进入游戏时加进世界频道的在线名单（`game/chat/chat_enter.go`） | 2 |
| mailbox + enter | 玩家进入游戏时把属于他的全服邮件合并进邮箱（`game/controllers/mailbox/mailbox_enter.go`，画像规则在这个文件里改） | 3 |
| mailbox + bag | 道具附件：登记 `item` 这个附件种类，领取时道具进背包，与领取账本同一笔事务；背包拒绝时邮件保持未领（`game/mailbox/mailbox_item.go`、`game/entities/player/mailbox_item.go`） | 9 |
| quest + checkin | 签到成功时把每日任务“签到一次”推进一次，与签到同一笔事务（`game/entities/player/quest_checkin.go`） | 3 |

以 `roost module list` 的输出为准。一个新工程装 `enter` 加需要的业务模块，就有了登录后的第一条请求、客户端可见的状态和业务本身。game-demo 目前用的是 `enter`、`chat` 与 `leaderboard`；demo 自己的背包、经验与邮件正在换成对应的模块。

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

1. **前置骨架**：实体还没有主 DAO 时执行 `roost add dao <Entity> --entity <Entity>`；模块有协议而工程还没有玩家接入时执行 `roost add access player --service <service>`。
2. **状态**：把模块的状态字段追加到实体主 DAO 的**末尾**。类型是模块自带的嵌套结构体（`db/def/<module>.go`）。
3. **组件**：把模块的组件接到实体上。
4. **文件**：把 `files/` 下的模板渲染后写进工程。落点已被业务文件占用时拒绝，不覆盖。
5. **记录**：在 `roost.yaml` 的 `modules.<name>` 记下这个模块拥有的文件、字段和分配到的号。
6. **生成**：运行全部生成器。

两个模块之间的**联动文件**在第 4 步之后写：这次装的模块与工程里已有的（或同一次装的）模块凑成一对、而这一对有联动时，把联动文件写进工程，归属记在拥有它的模块的 `with` 下。`plan` 与 `add` 的输出里是 `glue <模块> with <另一个模块>`。两个都早已装好的不补。

第 2～6 步任何一步失败，模块写过的业务文件与 `roost.yaml` 恢复原样。已安装的模块不再处理，重复执行没有变化。没有卸载命令：分配过的号与存储键不回收，要去掉一个模块时按 `roost.yaml` 里的归属清单手工删。

## 写一个模块

目录结构：

```
modules/<name>/
  module.yaml          清单
  files/<工程内路径>.tmpl   模板，按安装后的真实路径存放
```

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
entity: Player               # 状态与组件挂在哪个实体上
state:                       # 追加到主 DAO 末尾的顶层字段
  - field: Wallet            # 以模块名开头
    type: WalletState        # 模块在 db/def 里定义的嵌套结构体
    store: wallet            # 存储键，以模块名开头，发布后不要改
    dao: persist,sync
    comment: 写进 DAO 定义的注释
component: Wallet            # 挂在实体上的组件
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

`replaces` 用于生成器建一次、默认拒绝一切、留给项目实现的文件——目前认识托管服务的 `internal/service/<name>/collaborators.go`。安装时只有它仍是没改过的生成桩才替换；业务改过就拒绝安装。chat 模块用它接管 chat 服务的协作者。替换后的文件里不要再出现 `is not configured` 这句话：`project doctor` 靠它判断“还是拒绝桩”。

`with` 是模块之间的联动：`modules/<name>/with/<另一个模块>/<工程内路径>.tmpl`，两个模块都装着时才写进工程。用它把本模块接到对方留的**登记点**上——对方提供登记函数，联动文件在本模块自己的包里用 `init` 登记。登记点有两种：事务之外的，放在一个不依赖任何 controller 与实体的小包里（enter 的 `game/enter.OnEntered`：失败只记日志）；事务之内的，放在实体包里、拿到实体本身（checkin 的 `OnCheckinClaimed`：与对方的写入同一笔事务，返回错误会让对方那一次操作整笔回滚；mailbox 的 `RegisterMailboxGrant`：一种附件怎么发）。一个登记点分成“纯函数的一半”与“实体上的一半”时两处都要登记（mailbox 的附件种类：校验在 `game/mailbox`，发放在实体包）。规则：

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
| `{{ID:protocol:<name>}}`、`{{ID:errcode:<name>}}`、`{{ID:component:<name>}}` | 安装时从 `roost.yaml` 的 `ids` 段分配的号 |

### 必须遵守的约定

- **模块可以做全、做复杂，但简单用法必须照样能用**（维护者 2026-11-11）：game-demo 里有的能力（查配置表、分赛季、一次请求带回多样数据……）做进模块，不为了“简单”砍功能；全新的 `-template game` 工程不做任何配置直接 `roost module add <模块>`，缺省就要能工作、验收全绿，复杂能力在此之上填数据或打开。判断一个功能放不放进模块：放进去之后，不用它的简单工程会不会被迫多做事——不会，就放进模块。工程要改的规则与数据放进归工程所有的业务文件（或模块自带的配置表），逻辑文件不带规则。

- **状态自带**：只动自己的状态结构体，不往别的模块的结构体里加字段。挂在已有实体上的状态一律是嵌套结构体，不新建 DAO。
- **同步按顶层字段决定**：`dao: persist,sync` 下发给客户端，`dao: persist` 只落库。既有客户端可见状态又有服务端账本的模块写两个字段（wallet 就是）。嵌套类型内部的字段只写 `store` 键。
- **号不写死**：协议号、错误码、组件号一律用 `{{ID:...}}`。
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
