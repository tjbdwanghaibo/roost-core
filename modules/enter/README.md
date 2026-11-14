# enter：进入游戏——建档、首次初始化，以及“先新建、再进入”的另一条路

客户端握手成功之后要先“进入游戏”，游戏服才有这名玩家的 Player 实体可以收玩法请求。这个模块提供进入的端点、只做一次的首次初始化、别的功能接进来的登记点，以及会话激活后的告知。它不做鉴权：玩家是谁由握手的认证器决定（见 [login](../login/README.md)）。

## 能力

### 两条进入的路

一条会话走哪条路由它的 claims（认证器在握手时写进会话的键值）决定，判断只有一处：`game/login/enter_create.go` 的 `CreateFirst(claims)`。

| 路 | 什么时候走 | `EnterGame` 做什么 | 新玩家的档在哪里建成 |
| --- | --- | --- | --- |
| 加载或创建（缺省） | 没有人调过 `RequireCreateFirst`，或者登记的规则对这条会话答 false | 加载 Player，没有就创建，然后提交首次初始化 | 第一次 `EnterGame` 里；没有名字 |
| 先新建、再进入 | 登记的规则对这条会话答 true（login 的联动登记的是“用进服票据握手的会话”） | 只进入已经建成的玩家；没建成答 `enter_need_create`，不创建档、不写任何字段 | 客户端先发 `EnterCreate{name}`，一笔事务里起名、初始发放、建成 |

“建成”只有一个事实：档里的 `PlayerDao.Enter.Initialized`。有档而这个标记没置位（上次建到一半）与没有档是一回事。

### 客户端协议（`protocol/def/enter_game.go`、`enter_create.go`、`enter_status.go`）

- `EnterGame`（请求 / 应答）：登录后的第一条请求，请求体为空，玩家是谁取自已认证的连接。应答 `EnterGameResponse{Code, Reason, First}`，`First` 为 true 表示这是新玩家的第一次进入。
- `EnterCreate`（请求 / 应答）：新建请求 `EnterCreateRequest{Name}`。应答 `Code` 为 0 表示建成了；`enter_created` 表示早就建成了、什么都没改。两种情况客户端都接着发 `EnterGame`。哪条会话都可以发它。
- `EnterStatus`（推送）：`EnterStatusPush{Code, Reason, NeedCreate}`，会话激活之后服务器主动告诉客户端“本服能不能进、要不要先新建”。只有有人在会话激活时调了 `Announce` 才会推（login 与 enter 都装着时由 login 的联动接上）。

### 服务端入口

- `game/controllers/enter` 的 `HandleEnterGame`、`HandleEnterCreate`：两个协议的端点。
- `game/login/enter.go` 的 `Enter(ctx, registry, entering, load)`：一次进入的骨架，顺序是进入前检查 → `load` → 进入钩子。工程改写进入端点时把自己的加载与初始化包成 `load` 交给它。
- `game/login/enter_create.go` 的 `EnterSession(ctx, registry, entering, Loads{LoadOrCreate, Existing})`：端点实际调的入口，按 `CreateFirst` 选一种 `load` 再走 `Enter`。
- `game/login/enter_create.go` 的 `Create(ctx, registry, creating, CreateSteps{Ensure, Commit})`：一次新建的骨架，六步见下面“一致性”。
- `game/login/enter_create.go` 的 `Assess(ctx, registry, entering, built)`：算出该告诉客户端什么（`Status{Code, Reason, NeedCreate}`），不推送、不断开。
- `game/controllers/enter/enter_status.go` 的 `Announce(ctx, registry, entering, sessionID)`：调 `Assess`，把结果作为 `EnterStatus` 推给这条会话；进入前检查拒绝时推完等 `StatusCloseDelay`（300 毫秒）再断开这名玩家在本进程的全部会话。
- 不经 `Enter` 的工程可以自己调 `RunEntering`、`RunEntered`、`RunCreating`、`RunCreatePrepare`（都在 `game/login`）。
- Nest handler（`game/handler/enter_*.go`，都不对客户端开放）：`handlerEnterInit`（加载或创建那条路的首次初始化）、`handlerEnterExisting`（只进入已建成的）、`handlerEnterCreate`（新建事务）、`handlerEnterBuilt`（只读，问“建成了没有”）。前三个是 `rollback=undo durability=strict`。
- 组件 `EnterComp()`（逻辑在 `game/components/enter/enter.go`，Player 的薄接线在 `game/entities/player/enter_component.go`）：`Init(nowUnixMilli)`、`Existing()`、`Create(name, nowUnixMilli)`、`Built()`，只能在持有 Player 锁的 Nest 事务里调。

### 登记点

全部只在 `init` 里登记；空名、nil、重名直接 panic；执行按名字排序。

| 登记点 | 所在文件 | 什么时候被调 | 在不在事务里 | 失败的后果 |
| --- | --- | --- | --- | --- |
| `OnEntering(name, Check)` 进入前检查 | `game/login/enter.go` | 加载或创建 Player 之前；`EnterGame`、`EnterCreate`、`Announce` 用的是同一组 | 不在，只读 | 第一个拒绝就停下，端点把它的码答给客户端；不加载、不创建、不跑钩子。拒绝要返回带码错误 |
| `OnEntered(name, Hook)` 进入钩子 | `game/login/enter.go` | 首次初始化提交之后，每次进入都调 | 不在 | 只记一条 Warn，不让登录失败，下一次进入再调 |
| `gamelogin.OnFirstEnter(name, FirstEnterHook)` 首次初始化钩子 | `game/login/enter_hooks.go` | 新玩家建成的那一笔事务里（两条路都经过它） | 在，同一把 Player 锁 | 整笔回滚，下次再做；只能改当前玩家的 DAO，不能调服务、发消息 |
| `firstEnter` | `game/login/enter_first.go` | 同上，排在 `OnFirstEnter` 的钩子之前 | 在 | 同上。这个文件归工程，初始发放写在这里 |
| `RequireCreateFirst(name, rule)` | `game/login/enter_create.go` | 每次判断会话走哪条路 | 不在 | 全工程只能登记一条，第二次登记 panic |
| `OnCreating(name, CreateCheck)` 新建前检查 | `game/login/enter_create.go` | `EnterCreate` 里，进入前检查之后、建档之前 | 不在，只读 | 带码拒绝，什么都没写。名字的格式规则放这里 |
| `OnCreatePrepare(name, CreateStep)` 新建步骤 | `game/login/enter_create.go` | 空档建好之后、新建事务之前 | 不在 | 这一次新建到此为止，玩家仍是没建成；必须可以重复执行。全服唯一占名放这里 |
| `gamelogin.OnCreate(name, CreateHook)` 新建钩子 | `game/login/enter_hooks.go` | 新建事务里，排在首次初始化之前；只在 `EnterCreate` 那条路执行 | 在 | 整笔新建回滚。名字写进档案放这里 |

检查与钩子拿到的 `Entering.Claims` / `Entered.Claims` / `Creating.Claims` 是会话的 claims，可以为 nil，与会话共用同一个 map，约定只读。`Entered` 还带 `First` 与 `FirstAtUnixMilli`（首次初始化提交时的业务时间，之后每次进入都是同一个值，可以当注册时间用）。

### 状态

`PlayerDao.Enter`（类型 `EnterState`，`db/def/enter.go`），只落库、不下发给客户端：`Initialized`（首次初始化提交过）、`FirstAt`（那一刻的业务时间，Unix 毫秒）、`Fresh`（经 `EnterCreate` 建成之后还没进入过；下一次进入报 `First=true` 并清掉）。

### 配置

没有配置表，也没有配置键。留给工程改的业务文件是 `game/login/enter_first.go`（缺省什么都不发）。

### 错误码（`internal/errors/enter.go`）

- `enter_need_create`：“先新建”的会话发 `EnterGame`，而玩家在本服没建成。什么都没写。
- `enter_created`：已经建成的玩家再发 `EnterCreate`。什么都没改，名字也不改。
- `enter_name_long`：`EnterCreate` 的名字超过 `MaxCreateNameBytes`（256 字节），在一切检查之前被拒。

删过档的玩家在两条路上都得到框架的 `entity_deleted`；进入前检查与名字规则的拒绝原样透出登记方的码。

**日志**：新建提交、首次进入与每次进入确认，记录玩家、名字或 first；game-demo 的覆盖端点同步处理。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/enter/enter_create.go`, `game/controllers/enter/enter_game.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做鉴权。** 握手票据怎么验、玩家号从哪来不归它管。模块安装时代办 `roost add transport tcp` 与 `access.player.self_sync: true`，但生成的 `auth.go` 仍是一律拒绝的桩，监听也没有打开（`roost config enable player-tcp` 没有执行），所以单装 enter 时真实客户端连不上。
- **不管名字规则。** 没有任何功能登记 `OnCreating` / `OnCreate` 时，`EnterCreate` 的名字没人看，可以为空；唯一的限制是 256 字节的请求边界。在 `EnterGame` 里直接建成的玩家没有名字。
- **不决定谁“先新建”。** 没有人调 `RequireCreateFirst` 时永远是加载或创建。
- **进入钩子不是事务的一部分。** 它在首次初始化提交之后执行，失败只记日志，可能重复（重连、重发登录帧都会再调）。必须成立的事写进 `enter_first.go` 或 `OnFirstEnter`。
- **`EnterStatus` 只是告知，不是关口。** 推送可能丢、可能过时；判断不了时（检查问不到权威、读档出错）什么都不推。断开时断的是这名玩家在本进程的全部会话（传输没有只断一条会话的入口），慢的连接可能没收到那一帧就被断开。权威判断在 `EnterGame` / `EnterCreate`。`Announce` 要读档，调用方必须在后台执行并自己限量。
- **一致性与幂等：**
  - 加载或创建：`GetOrCreate` 与首次初始化是两步。建档之后、初始化之前崩溃，下一次进入补上；初始发放与 `Initialized` 在同一笔事务里，恰好生效一次；同一名新玩家并发登录最后是同一个实体。
  - 新建的六步（`Create`）：名字超长拒绝 → 进入前检查 → 新建前检查（到此什么都没写）→ `Ensure` 建一份空档 → 新建步骤（事务外）→ `Commit` 一笔事务（新建钩子、初始发放、`Initialized` / `FirstAt` / `Fresh` 一起提交或回滚）。任何一步失败玩家仍是“没建成”，可以重试；名字被拒或提交失败时空档留着，下一次新建接着用。
  - 同一名玩家两条连接同时新建：两边的事务在实体锁上排队，先到的建成，后到的得到 `enter_created`。
  - 经 `EnterCreate` 建成之后的第一次 `EnterGame` 报 `First=true`；这一次的应答丢了、客户端重发时 `First` 已经是 false。
  - 装了 naming 时名字先于“建成了”落定：占了名而新建事务没成的玩家，下一次换名字由 naming 释放旧名。
- **前提：** 需要 nest Mod，实体 `Player` 与它的 lifecycle；只在承载这名玩家的 game 进程内有效。进入前检查若要问别的进程，要用传进来的 `ctx`，占的是登录请求的时间预算。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md` 的“enter 模块的边界”“进入钩子不是事务的一部分”“game-demo 只用了 enter 模块的协议、域与首次初始化”“game-demo 接入登录链路的边界”）：
  - 握手之后不进入就断开的已有玩家，`EnterStatus` 读档时装载的那份内存副本不经工程自己的驻留表（game-demo 的情形），到进程重启才释放。
  - 工程改写进入端点时要自己在同样的位置调 `RunEntering` 与 `RunEntered`，否则检查与钩子不执行。
  - 与真实 naming 的建角、跨 sid 重名拒绝、改名后旧名释放已由 `scripts/test-demo-login.sh` 的 casual + naming 阶段验证；故障窗口逐步恢复仍以 naming 的离线状态机场景为准。

## 复用方式

- **需求正好是它：** `roost module add enter`。没有依赖；安装时一并打开玩家 TCP 接入与自身视图。要让真实客户端连上，再装 `login`（或自己实现 `auth.go`）并执行 `roost config enable player-tcp`。
- **新玩家要发初始资源：** 改 `game/login/enter_first.go` 的 `firstEnter`，直接调同一实体上别的组件，例如 `host.Wallet().Add(100)`。用不留回执的入口，“只发一次”由标记保证。模块之间的联动不改这个文件，在自己的文件里登记 `gamelogin.OnFirstEnter("名字", func(owner view.PlayerEntity, nowUnixMilli int64) error {...})`。
- **别的功能要在玩家进入后顺带做事**（上线登记、拉离线数据、推一帧）：在自己的包的 `init` 里调 `gamelogin.OnEntered("名字", func(ctx context.Context, registry *app.Registry, entered gamelogin.Entered) error {...})`。
- **要在进入之前挡住某些玩家**（封停、维护白名单、不归本服）：`gamelogin.OnEntering("名字", func(ctx context.Context, registry *app.Registry, entering gamelogin.Entering) error {...})`，拒绝时返回 `errcode.Define` 出来的带码错误。
- **要让某类会话先起名再进入：** `gamelogin.RequireCreateFirst("名字", func(claims map[string]string) bool {...})`（全工程一条；装了 login 时它已经登记，不能再登记）；名字的格式用 `gamelogin.OnCreating`，要先有档的事务外步骤用 `gamelogin.OnCreatePrepare`，写进档用 `gamelogin.OnCreate`。
- **用自己的认证器、又想要 `EnterStatus`：** 在自己的会话激活回调里后台调包 `game/controllers/enter` 的 `Announce(ctx, registry, gamelogin.Entering{PlayerID, Claims}, sessionID)`，并自己限制同时在跑的数量、给 `ctx` 带上期限。
- **不要改的文件：** `game/login/enter.go`、`game/login/enter_create.go`（顺序与“被拒绝的进入什么都没碰”靠它们保证）、`game/components/enter/enter.go` 里标记与发放同一笔事务的写法。
- **只适合借鉴的情形：** 进入流程要加自己的步骤（排队、选角、驻留表）时可以整个改写端点——game-demo 就改写了 `game/controllers/enter` 与 `protocol/def/enter_game.go`，只保留协议、域与首次初始化。值得借鉴的是“做过没有记在档里、每次进入都问一遍”的首次初始化，以及“检查 → load → 钩子”的骨架。

## 联动

本模块的 `with/`：

- `with/profile`：`EnterCreate` 的名字按档案模块的 `NormalizeProfileName` 校验（不合规在建档之前被拒）；没装 naming 时在新建事务里写进档案，装了 naming 时经它的唯一命名入口占名。

别的模块的 `with/enter`：

- `login`：进入前检查（承载服不是本服答 `login_elsewhere`）；进服票据握手的会话登记为“先新建”；会话激活后调 `Announce` 推 `EnterStatus`。
- `rehome`：进入前检查（正在换服答 `rehome_moving`，绑定在别的服答 `rehome_elsewhere`）；进入之后做到达与意图对账。
- `mailbox`：进入时把属于这名玩家的全服邮件合并进邮箱。
- `chat`：进入时把玩家加进世界频道的在线名单。
- `servertime`：每次进入把服务器的业务时间推给客户端。
- `purchase`：进入后有界拉取待发订单。
- `alliance`：进入和重连后对账成员展示与 HomeSID。
- `activity`：首次建档记相对锚点；每次进入惰性对账。
- `profile`：首次建档事务里设初始 1 级。
- `attribute`：首次建档事务里建立基础与最终属性。
- `portal`：只有测试，一条从 HTTP 登录到进入游戏的整链路场景。

`rules` 模块不经 `with/`，直接依赖本模块并登记了 `OnEntered("rules", …)`。

## 验证范围

`roost module verify` 跑 29 条场景（`^TestEnter`，四个包），与 profile 的联动另 3 条（`^TestWithEnterProfile`）。全部是离线的：

- 新建与进入的事务（`game/handler`）用真实的 Nest 加替身提交器（不落盘，可以拒绝提交来验证回滚），覆盖首次初始化、重复进入、提交被拒后补做、新建恰好一次、并发新建、名字钩子拒绝什么都不写。
- 骨架与登记点（`game/login`、`game/entities/player`）是纯函数级的场景。
- `EnterStatus`（`game/controllers/enter`）用玩家传输的替身，验证推送内容、拒绝后断开、判断不了时不推。
- 模块自己的场景使用离线持久层；`scripts/test-demo-login.sh` 另外生成 casual + naming，通过真实 HTTP / Robot TCP、Redis Directory、Mongo / WAL 验证新建与进入、两服唯一命名、双 Gate、game 重启后首次金币与购买回执不重复。范围与复跑命令见[登录线证据](../../docs/maintenance/evidence/v147-login-20261114.md)。
