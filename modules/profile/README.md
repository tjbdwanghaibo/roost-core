# profile：玩家档案（名字、等级、经验）

挂在 `Player` 上的最基础的一块：一个显示名、一个等级、等级内的经验。它提供改名协议、服务端发经验的入口（每级固定 100 经验，一次可以连升多级）、升级时的两种钩子，以及给服务端按玩家号读“名片”（名字与等级）的入口。

## 能力

**客户端协议**

- `ProfileRename{Name}` → `ProfileRenameResponse{Code, Reason, Name}`（`protocol/def/profile_rename.go`）：改自己的名字。应答的 `Name` 是存下的名字（去掉了首尾空白），客户端以它为准。装了 `naming` 时这个协议自动改走全服唯一的改名流程。
- 档案随玩家自身视图同步下发：`Profile`（名字、等级）与 `ProfilePrivate`（经验）。没有“读档案”的请求协议。

**服务端入口**

- 发经验：handler `ProfileAddExp(amount)`（`game/handler/profile_add_exp.go`，`rollback=undo durability=strict`），事务之外用 `syncsender.NewProfileAddExpSender(client).Sync_ProfileAddExp(ctx, playerEntityID, amount)`，返回这一次升了几级。没有协议与端点，不对客户端开放。
- 读名片：handler `ProfileCard`（`game/handler/profile_card.go`，只读），用 `syncsender.NewProfileCardSender(client)` 按玩家的 Entity ID 调，返回 `player.ProfileCard{Name, Level}`。它是值，离开实体锁之后可以继续用。
- 在自己的 Player handler 里直接用组件（目标接口嵌入 `player.IProfileEntity`）：
  - `ProfileComp().AddExp(amount) (gained int32, err error)`：发经验，够就升级。
  - `ProfileComp().CheckAddExp(amount) error`：只检查不执行，回答与 `AddExp` 相同的业务码，不写任何字段。多方写入的事务先用它把每一方问一遍。
  - `ProfileComp().Rename(name)`：事务内写名字的原语（做名字校验，不查重）。
  - `ProfileComp().Card()`、`DisplayName()`、`Level()`、`Exp()`：读当前值。
- `player.NormalizeProfileName(name) (string, error)`：只做名字校验并返回去掉首尾空白的名字，不写实体。别的功能要按同一套规则校验名字时用它。
- 事务之外的改名入口：`profile.Rename(ctx, registry, playerID, name) (name string, handled bool, err error)`（`game/profile/rename.go`）。没有人登记改名器时 `handled` 为 false、什么都没做；装了 `naming` 时转给它。

**登记点 / 钩子**

- 工程钩子 `onLevelUp(from, to)`（`game/components/profile/levelup.go`，**归工程所有**）：升级之后调一次，升级奖励写在这里。
- 模块钩子 `player.OnProfileLevelUp(name, func(component *ProfileComponent, from, to int32) error)`（`profile_hooks.go`）：别的模块按名字登记，在工程钩子成功之后按名字顺序执行。
- 两种钩子都在发经验的那笔 Player 事务里：只能改同一个 Player，不能做外部 I/O；任何一个返回错误，经验、等级与全部钩子的写入一起回滚。一次发放连升多级也只调一次，参数是起止等级。
- `profile.RegisterRenamer(fn)`（`game/profile/rename.go`）：登记事务之外的正式改名入口，全进程只能登记一个（`naming` 用它）。
- 改名 handler 会看 `Player` 有没有 `CheckProfileRenameRequest() error` 方法，有就先问它（`naming` 用它拒绝本地改名）。

**状态**（`db/def/profile.go`，都挂在 `PlayerDao` 上，落库并同步）

- `PlayerDao.Profile`（`ProfileState{Name, Level}`）：别的玩家也可以看的那一半。给别的玩家做视图时，字段白名单里列它。
- `PlayerDao.ProfilePrivate`（`ProfilePrivateState{Exp}`）：只给本人的那一半。`Exp` 是当前等级内的经验，不是累计经验。

**配置**：没有配置表与配置键。规则是代码常量（`game/entities/player/profile_component.go`）：`ProfileNameMaxRunes = 16`、`ProfileExpPerLevel = 100`。

**错误码**（`internal/errors/profile.go`）：`profile_name_empty`（去掉首尾空白后为空）、`profile_name_long`（超过 16 个字符）、`profile_name_invalid`（不是合法 UTF-8）、`profile_exp_invalid`（数额不是正数）、`profile_exp_overflow`（经验或等级超出存储范围，或存档里的值已坏）。

**日志**：默认改名确认（新名）；GM 发经验只在升级时记录 levels_gained，升级奖励邮件另记 from/to。接管改名的 naming 记录旧名、新名和请求键。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/profile/profile_rename.go`, `game/gm/profile.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：名字不查重（要全服唯一装 `naming`）、不查敏感词、改名不收费也不限次数；没有等级上限；经验曲线是平的，没有按等级查表；没有头像、签名、称号等别的档案字段；不记累计经验；不提供客户端查别人档案的协议。
- **`AddExp` 不判重**：同一次发放调两次就加两次。调用方的请求会重试时，先在自己的状态里判重，或把发经验写进自己的 handler，让“发过了”与经验在同一笔事务里。
- **名字长度按字符数**：一个汉字算一个，至多 16 个。
- **一致性**：改名、发经验各是 Player 上的一笔严格事务；被拒绝时不写任何字段。升级钩子与经验、等级同一笔事务，所以钩子里不能做事务之外的事——要在升级之后通知别的服务（发邮件、推送），在钩子里发 effect，提交之后才送出。
- **连升多级不逐级循环**：`AddExp` 用除余算升级数；钩子里也不要按 `to-from` 逐级做重活，一次大额发放可以连升很多级。
- **建档与等级**：`BuildEntity` 出来的档等级是 0，还不是可进入游戏的角色；新角色在 `enter` 的建档事务里经 `profile` + `enter` 的联动设为 1 级。初始等级不是一次升级，不触发升级钩子；建档之前已经被设成正等级的档保留原值；建档失败时初始等级与标记一起回滚，下次进入重试。构造实体的代码不能绕过 Nest 直接写持久字段。
- **没升级的发放只动 `ProfilePrivate`**：别的玩家的视图不会为它收到增量。
- **前提**：依赖 `enter`；业务服务要有 `nest` Mod。状态标了同步，客户端要打开玩家自身视图才收得到（装 `enter` 时安装器代办）。只在持有这名 Player 的 game 进程内有效。
- **已知限制**：[KNOWN-LIMITS“profile 的边界”](../../docs/maintenance/KNOWN-LIMITS.md)（不查重、不查敏感词、没有等级上限；在 game-demo 里模块的 `ProfileAddExp` 不记全服累计）与“模块的规则大多还是 Go 写的业务文件”（每级经验是代码常量）。

## 复用方式

- **需求正好是它**：`roost module add profile`，连带装上 `enter`。
- **需求多一点**：
  - 升级奖励：改 `game/components/profile/levelup.go` 的 `onLevelUp`（例如装了 wallet 时 `component.owner.WalletComp().Add(int64(to-from) * 50)`）。
  - 换经验曲线、加等级上限：改 `profile_component.go` 的 `planExp`，保持“不按升级数逐级循环”。
  - 改名字规则：改 `NormalizeProfileName`（`naming` 与新建角色的名字检查都经它）。
  - 加档案字段：别人能看的加在 `ProfileState`，只给本人的加在 `ProfilePrivateState`。
  - 别的模块要在升级时做事：在自己的文件里 `OnProfileLevelUp("<模块名>", …)`，不要改 `profile_hooks.go`。
- **别的代码要用它**：
  - 发经验（事务之外）：`syncsender.NewProfileAddExpSender(client).Sync_ProfileAddExp(ctx, playerEntityID, amount)`。
  - 发经验（在自己的 Player handler 里）：先 `target.ProfileComp().CheckAddExp(amount)`，都通过了再 `AddExp(amount)`。
  - 按玩家号读名字与等级：`syncsender.NewProfileCardSender(client)`。
  - 在同一个 Player 上读等级：`owner.ProfileComp().Level()`。
- **只适合借鉴的情形**：多角色、转职、多条经验线。值得借鉴的是把档案按“别人可见 / 只给本人”拆成两个顶层字段（同步按顶层字段决定给谁），以及 `CheckAddExp` 这种“先全部检查、再全部写”的写法。

## 联动

本模块的 `with/`：

- `profile` + `enter`（`with/enter/game/login/profile_enter.go`）：登记 enter 的首次建档钩子，把等级设为 1，与“已建档”标记同一笔事务；不调升级钩子，重进与冷加载不重置。
- `profile` + `gm`（`with/gm/game/gm/profile.go`）：运维命令 `gm.player.add_exp`，payload `{player_id, amount, note}`，经 `ProfileAddExp` 的 Sender 发经验；不判重、不自动重试。

别的模块的 `with/profile`：

- `enter` + `profile`：新建角色（`EnterCreate`）的名字按 `NormalizeProfileName` 校验；没装 `naming` 时在新建事务里写进档案，装了时经唯一改名入口占名。
- `task` + `profile`：升级时把任务事件 `profile.level_up` 推进升的级数，同一笔事务。
- `activity` + `profile`：升级时在同一笔事务里重算活动条件。
- `mailbox` + `profile`：升级时冻结奖励并发出 effect，消费者发一封奖励邮件；规则在归工程所有的 `MailboxProfileReward`，缺省不发。
- `scene` + `profile`：把 `Profile` 登记为场景里旁人可见的字段。

直接依赖本模块的模块：`naming`（接管改名）、`attribute`（按等级算基础属性，升级时重算）、`dungeon`（通关发经验，先 `CheckAddExp` 再 `AddExp`）。

## 验证范围

- `roost module verify profile`：20 条场景，都在 `game/handler`；与 `enter` 的联动 4 条，与 `gm` 的联动 4 条。
- 离线：真实的 Nest 引擎与生成的 Sender，提交器是替身（可以注入提交失败）。
- 覆盖：改名（去空白、16 个字符的边界、空名、超长、非法编码都不写）、发经验（不满一级、恰好一级、连升多级、非正数、溢出、存档已坏）、被拒绝时即使调用方吞掉错误也没有写入、提交被拒后名字 / 等级 / 经验一起回滚、升级钩子出错回滚、`CheckAddExp` 与 `AddExp` 回答一致、名片、两个字段都同步且经验不在别人可见的那一半、装了改名器时端点走正式入口。
- 没有验证：真实数据库与网络；`ProfileRename` 没有压测机器人覆盖。
