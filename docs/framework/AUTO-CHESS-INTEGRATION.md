# 自走棋框架学习与验收

本手册把“八人一局、共享卡池、准备 / 战斗阶段轮转、按排名领奖”的自走棋业务路径映射到 core 的正式接口，分两部分：**第一部分“学习”**讲框架怎么用、为什么这样设计，给出阅读顺序与可编译的调用示例；**第二部分“验收”**说明脚本证明什么、不证明什么。棋子、羁绊、经济与战斗规则由游戏定义。下面的业务夹具（三种棋子各 32 张的共享卡池、三格商店、三格战场加六格备用席、一 / 三 / 九张对应一 / 二 / 三星、准备 10 秒 / 战斗 1 秒、至多 8 回合、最多 32 步的确定性战斗）只是学习和验收样例：框架与生成器不包含任何自走棋规则，生成器也不生成这些业务文件；完整的参考应用仍是 [game-demo](../../demo/README.md)。其他游戏类型的手册见 [SLG](SLG-INTEGRATION.md)、[MMORPG](MMORPG-INTEGRATION.md)、[MOBA](MOBA-INTEGRATION.md)、[卡牌 RPG](CARD-RPG-INTEGRATION.md)、[合成休闲](MERGE-CASUAL-INTEGRATION.md)、[放置模拟](IDLE-SIM-INTEGRATION.md)。

## 第一部分：学习

### 1. 适合谁、先读什么

适合准备用 core 做自走棋、多人回合竞技一类游戏后端的工程师。先读[入门](../GETTING-STARTED.md)、[Nest / Entity](guide/02-nest-entity.md)、[DataEngine](guide/03-dataengine.md)；第一次学建议先读完 [SLG 手册](SLG-INTEGRATION.md)的第一部分（Entity / DAO / Nest / 实体定时器的基本接法讲得最细），再读本篇；多实体领奖的写法与 [卡牌 RPG](CARD-RPG-INTEGRATION.md) 的开局 / 领奖相同。

本类型选择**一局一个权威实体**：八名玩家的商店、经济、阵容和共享卡池都放在同一个 `ChessMatch` 实体里，所有玩家的操作由它的 Nest 锁串行裁决；战斗由服务器在阶段切换时用冻结的阵容算出结果。不需要给每张卡建 Remote 实体，也不需要 Lockstep 的实时输入广播。

### 2. 模块分工：自走棋里的状态归谁

| 业务状态 | 拥有者 | 框架接法 | 业务边界 |
| --- | --- | --- | --- |
| 共享卡池、八席的商店 / 经济 / 等级 / 阵容 / 连接代际、阶段与回合、战斗快照、排名、每席命令回执 | `ChessMatch` 实体的 DAO `State` | `roost add entity ChessMatch` + `add dao`；实体放在 World 锁档 | 整局是一段业务 JSON，每席账本至多 128 条 |
| 阶段期限（准备超时、战斗结算） | ChessMatch 的实体定时器 + `WakeTimer` | `roost add timer --entity ChessMatch`；节点存 DAO，驱动到期投递 Tick | 每次到期只推进一个阶段；加载后到期节点只触发一次，框架不追帧 |
| 永久金币与建档标记 | Player DAO | 生成的 Player DAO 字段；建档经一次正式事务 `Sync_ChessPlayerInit` | 只承接领奖；跨 sid 发奖另接 Remote / Saga |
| 领奖 | Player + ChessMatch | 多实体 Sender `MultiSync_ChessClaim`：Match 的 `Claimed` 与 Player 金币同一 strict 事务 | 排名由服务器终态决定，客户端不能传胜者或奖励额 |
| 配置（棋子张数 / 价格 / 属性、阶段时长、回合上限） | configdata Store / Snapshot | 注册对象定义 → Load 时校验 → 成局时捕获并整份写进对局 | 已开始的对局沿用成局那一代 |
| 成局名单与随机种子 | 匹配 / 房间服务 | `Sync_ChessInit` 首次固定八个完整 Player ID、服务器种子与配置 | 同一 Match ID 改名单被拒、不重掷种子 |
| 客户端看到的对局 | 业务视图 `View` | 锁内读取，按观察者裁剪 | 只有本人能看自己的经济、商店与备用席；不含种子、卡池余量和对手账本 |
| 登录、接管票与账号 | account / Player 静态 sid | 复用正式生命周期与登录鉴权 | 夹具直接用合成的已鉴权身份与接管票；账号链见 [玩家接入](PLAYER-ACCESS.md) 与 game-demo |

```mermaid
flowchart LR
    A[已鉴权请求] --> B[生成 Sender]
    B --> C[Nest 锁 ChessMatch]
    C --> D[核对席位 / Epoch / Seq / 回合 / 期限]
    D --> E[改商店 / 经济 / 阵容，八人都准备则冻结战斗]
    E --> F[替换阶段定时器]
    F --> G[strict WAL 提交]
    G --> H[回复回执]
    G --> I[提交后报告新期限给驱动]
    I --> J[到期投递 Tick：推进一个阶段]
```

`strict` 保证回复前 WAL fsync，不保证回复时 Mongo 已投影（[DataEngine](guide/03-dataengine.md)）。

### 3. 共享卡池、阶段与领奖：如何使用

#### 3.1 生成工程从哪来

在仓库根目录使用 go.mod 声明的工具链运行：

```bash
ROOST_AUTO_CHESS_KEEP=1 bash scripts/test-auto-chess-generated.sh   # 保留生成工程以便阅读；不加 KEEP 时成功即删除
```

脚本打印临时目录，生成工程在其中的 `project/`。它做的事就是一个新工程的接入步骤：

1. `roost project new auto-chess-practice -module example.com/auto-chess-practice -template game`；
2. `roost add dao Player --entity Player`、`roost add entity ChessMatch`、`roost add dao ChessMatch --entity ChessMatch`、`roost add timer --entity ChessMatch`；
3. 业务只改这几处：在 `db/def/player.go` 加 ``Gold int64 `bson:"gold" dao:"persist"` `` 与 ``Initialized bool `bson:"initialized" dao:"persist"` ``，在 `db/def/chess_match.go` 加 ``State string `bson:"state" dao:"persist"` `` 与 ``WakeTimer int64 `bson:"wake_timer" dao:"persist"` ``；把 `game/entities/chess_match/entity.go` 注解里的 `category=entity.EntityCategoryOther` 改为 `EntityCategoryWorld`（Match 与 Player 一起锁，放在更低的锁档，锁序固定）；放入业务包 `game/autochess/`、定时回调 `game/entities/chess_match/timer_handlers.go` 与 handler `game/handler/auto_chess.go`；
4. `roost generate` 生成组件、访问器和 Sender，不手改生成文件。加字段时 generate 会打印 `WARN: … new stored key …`：这是 DAO 持久键变化的提示（加字段不需要迁移，老文档读成零值），见第 7 节。

生成的 `internal/bootstrap/generated.go` 已用 `wiring/entitytimer` 的 Mod 装配 ChessMatch 的定时器驱动（绑定 Nest、接业务时钟、启动与停止投递循环），`internal/registry/nest_gen.go` 已登记 handler；业务不手写这两处接线。测试里看到的 `TimerDriver.SetClock` / `Bind` / `DeliverDue` / `Start` 是用例自己搭 Nest、控制投递时机的替身接线，不是生产装配要抄的步骤。

#### 3.2 阅读顺序

进入 `project/` 后按顺序读（括号里是仓库内的源文件）：

| 顺序 | 文件 | 先回答的问题 |
| --- | --- | --- |
| 1 | `game/autochess/rules.go`（[rules.go.txt](../../scripts/testdata/auto-chess/rules.go.txt)） | 配置怎样经校验发布？对局为什么在成局时冻结一代？ |
| 2 | `game/autochess/state.go`（[state.go.txt](../../scripts/testdata/auto-chess/state.go.txt)） | 一局里有什么？卡池张数守恒怎样校验？视图怎样按观察者裁剪？商店抽卡怎样由种子 + 序号决定？ |
| 3 | `game/autochess/match.go`（[match.go.txt](../../scripts/testdata/auto-chess/match.go.txt)） | 命令怎样核对席位 / 代际 / 序号 / 回合 / 期限？八人准备后怎样冻结战斗？战斗、淘汰、收入与终局怎样结算？ |
| 4 | `game/handler/auto_chess.go`（[handlers.go.txt](../../scripts/testdata/auto-chess/handlers.go.txt)） | 成局、命令、接管、查回执、领奖分别在哪个锁与提交边界里生效？阶段定时器在哪里替换？ |
| 5 | `game/entities/chess_match/timer_handlers.go`（[timer_handlers.go.txt](../../scripts/testdata/auto-chess/timer_handlers.go.txt)） | 到期回调怎样核对身份？离线很久为什么只推进一个阶段？ |
| 6 | `game/handler/syncsender/` | 生成的 Sender 怎样把 Match ID、Player ID 与业务参数交给 Nest；业务调用它，不手改 |
| 7 | [gameplay_test.go.txt](../../scripts/testdata/auto-chess/gameplay_test.go.txt)、[rules_test.go.txt](../../scripts/testdata/auto-chess/rules_test.go.txt) | 并发、拒绝、回滚、阶段推进与隐私怎样实际验证 |
| 8 | [persistence/recovery_test.go](../../scripts/testdata/auto-chess/persistence/recovery_test.go) | 已确认但未投影的记录在 SIGKILL 后怎样恢复？冷加载后驱动怎样续完整局？ |

身份要分开理解：Match 与 Player 用框架完整 EntityID；每席命令的 `Seq` 是该席在本局内永久递增的序号，`Epoch` 是该席的连接代际（接管票），`Round` 是命令所属回合；商店抽卡用对局的 `Draw` 序号；定时器节点 ID 又是 Match 内的独立身份。配置内容 hash、业务 `State` 里的 `Schema`、DAO 的 `_schema` 与 core 版本是四个不同的概念。

#### 3.3 用 Sender 调用

下面的文件放进保留的生成工程（例如 `internal/chessflow/flow.go`）可直接编译。`client` 是当前服务的 Nest 客户端，`catalog` 是进程启动时 `autochess.OpenCatalog` 打开的配置，`now` 是服务器业务时钟（毫秒），EntityID 都是完整 ID：

```go
package chessflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"example.com/auto-chess-practice/game/autochess"
	syncsender "example.com/auto-chess-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// startMatch 是匹配服务成局后的调用：名单、种子、配置与时间都来自服务器。
// 同一 matchID 重试返回原对局（不重掷种子）；改名单被拒。Player 要先经 Sync_ChessPlayerInit 建档才能领奖。
func startMatch(ctx context.Context, client nest.Client, catalog *configdata.Store,
	matchID int64, seats [autochess.Seats]int64, now func() int64) error {
	rules, err := autochess.CaptureRules(catalog.Current())
	if err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	_, err = syncsender.NewAutoChessSender(client).Sync_ChessInit(ctx, matchID, seats, rules, hex.EncodeToString(raw), now())
	return err
}

// buyAndReady 是已鉴权入口的调用顺序：读本人视图、买商店第一格、宣布准备。
// 每条命令带视图里的 Epoch / 下一个 Seq / 当前回合；Player ID 取自会话。
func buyAndReady(ctx context.Context, client nest.Client, matchID, playerEntityID int64, now func() int64) error {
	s := syncsender.NewAutoChessSender(client)
	for _, kind := range []string{"buy", "ready"} {
		v, err := s.Sync_ChessView(ctx, matchID, playerEntityID)
		if err != nil {
			return err
		}
		command := autochess.Command{Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Round: v.Round, Kind: kind}
		if _, err := s.Sync_ChessCommand(ctx, matchID, playerEntityID, command, now()); err != nil {
			return err
		}
	}
	return nil
}

// claim 在终局后领奖：奖励由服务器排名推导，重试返回同一额度、不重复发放。
func claim(ctx context.Context, client nest.Client, matchID, playerEntityID int64) (int64, error) {
	return syncsender.NewAutoChessSender(client).MultiSync_ChessClaim(ctx, playerEntityID, matchID)
}
```

`Kind` 支持 `buy`（`A` 是商店格）、`sell`（`A` 是 0～8 的单位格）、`move`（`A` / `B` 交换两格，0～2 是战场）、`refresh`、`xp`、`lock`（切换锁店）与 `ready`。公网入口只接收命令本身；Match ID 由房间路由给出，Player ID、配置、种子、接管票、服务器时间、胜者与奖励都由服务器填。`Sync_ChessConnect(ctx, match, player, ticket, connected)` 由可信的连接层在接管 / 断线时调用：接管票递增，旧连接的断线事件踢不掉新连接。每次调用的错误都要按业务逐次处理，示例只演示调用方式；接网络协议时把 `autochess.ErrRule` 映射成业务错误码，存储 / 内部故障按[回复错误判别](guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)处理。

`matchID` 要用 id-gen 发的新号并先创建实体：直接 `Create` 不读存储，不能复用删过档或存了没加载的 ID（[删档后该 ID 不可再用，D10](guide/03-dataengine.md#4-冷加载与-schema)）。对局归档要与领奖防重一起设计，不能删掉 Match 再用同一 ID 重建；夹具在测试里直接建实体，没有演示 id-gen 接线。

#### 3.4 十二个场景各学什么

| 场景（测试名前缀 `TestAutoChess`） | 业务选择 | 学到的框架契约 |
| --- | --- | --- |
| `MergeSellAndPoolConservation` | 九张同种连锁合成三星、出售三星 | 合星与出售都按真实张数守恒，九张全部回池 |
| `CombatReplayOddByeAndElimination` | 七人存活（一人轮空）、三星对战 | 战斗是纯函数，能从冻结阵容重算出同一结果；奇数席一人轮空、淘汰定名次 |
| `StrictDecodeAndBoundedRecovery` | 状态被改、凭空多出卡、伪造战斗伤害、离线一小时 | 严格解码拒绝不守恒与伪造结果；一次推进只走一个阶段，不快进整局 |
| `CatalogReloadDoesNotRepriceFrozenMatch` | 合法与非法热更 | 合法热更不改已开始对局的配置；非法热更整次被拒、当前快照不变 |
| `SharedPoolConcurrentPurchaseAndRetry` | 八席同时买、每条命令四个并发 | 一局一个锁 + 每席序号：每席只买到一次、一份回执，卡池守恒 |
| `ReadyRaceFreezesOneBattle` | 八席各三个并发准备 | 只冻结一场战斗、只挂一个定时器，准备后不能再改；下一阶段只结算一次 |
| `DeadlineRejectsLateInputAndDisconnectCannotStall` | 掉线、期限后到达的准备 | 掉线席位不能操作也不阻塞阶段；期限到了的命令被拒，驱动照常推进 |
| `ReconnectFencesAndPrivateView` | 新连接接管、旧连接断线事件、局外人查看 | 接管后旧代际的命令与断线事件被拒，新连接可按原序号查旧回执；局外人被拒，视图不含种子、不与存档共享内存 |
| `FormationLevelShopLockAndConfigFrozen` | 上阵数受等级限制、锁店、升级、成局重试带新配置 | 进入战斗时阵容被冻结；锁店跨回合保留；同 Match 重复成局不改配置、不重掷 |
| `UndoRestoresPoolRandomReceiptAndTimer` | 刷新商店后 handler 失败 | 卡池、费用、抽卡序号、回执与定时器节点一起回滚，重试照常生效 |
| `TerminalRanksAndConcurrentRewards` | 打到终局、同分按席序、领奖中途失败、八人各四个并发领奖 | 多实体领奖事务：失败时两份 DAO 一起回滚；每人只得一份按排名的奖励；终局后不再挂定时器 |
| `LedgerFullKeepsOriginalReceipt` | 一席账本填满 128 条 | 满了拒绝新命令而不淘汰旧回执，旧命令重放仍得原回执 |

基础用例由用例调用驱动的 `DeliverDue` 推进阶段（实体一直在内存，不启动投递循环）。上述数值是样例，不是公共框架规则。这些场景实际证明到哪一步见第 10 节。

### 4. 配置与随机为什么在成局时冻结

`OpenCatalog` 用 `configdata.MustRegisterObject` 登记 `chess_rules`（`chess.json`），`Validate` 失败整次加载 / 重载被拒，旧快照继续服务（[业务表发布](guide/07-config.md#3-业务表发布)）。成局时捕获 `catalog.Current()` 再 `CaptureRules`，整份 `Rules` 写进对局：八名玩家在一局里看到同一代价格与属性，热更只影响新开的对局。商店抽卡由服务器种子与对局的 `Draw` 序号派生，所以 handler 失败回滚后重试仍是同一结果；种子由服务器安全随机源生成（测试固定种子便于复现），不进视图。公网请求**不携带** Rules 或种子。

### 5. 阶段、期限与断线

每次写对局时 handler 的 `saveChess` 替换阶段定时器（`RemoveTimer` 旧节点、`NewTimerAt` 新期限、ID 记进 `WakeTimer`），终局后不再挂。准备阶段八名存活玩家都准备时，命令事务里直接冻结四场战斗；否则准备期限到了由定时器冻结，用玩家当时的阵容托管。战斗期限到了结算伤害、淘汰、收入与刷新，进入下一回合；达到回合上限或只剩一人时排名收口。

```mermaid
sequenceDiagram
    participant C as 已鉴权入口
    participant N as Nest
    participant M as ChessMatch DAO
    participant T as 实体定时器
    C->>N: Match ID + Player ID + 命令
    N->>M: 核对席位、代际、序号、回合、期限
    N->>M: 改商店 / 经济 / 阵容；八人都准备则冻结战斗
    N->>T: 删除旧节点，登记新期限
    N->>N: strict WAL 提交
    N-->>C: 回执
    T->>N: 到期投递 Tick（当前节点 ID / 期限）
    N->>M: 推进一个阶段，登记下一期限
```

- 回调核对节点 ID 与原期限，旧计时事件不会推进新阶段。**框架不追帧**：实体不在内存期间到期的节点，加载后只触发一次（[按实体的持久定时器](guide/10-time.md#4-按实体的持久定时器)）；本例据此只推进一个阶段，下一期限从实际触发时间起算，不会因为离线一小时在一次加载里打完整局。
- 掉线（`Sync_ChessConnect(..., false)`）的席位不能再发命令，但不阻塞阶段：期限到了照常推进。期限之后到达的命令被拒。
- 接管（`Sync_ChessConnect(..., ticket+1, true)`）提高该席代际：旧代际的命令和断线事件被拒，新连接可以用当前代际按原序号查旧回执（`Sync_ChessReceipt`）。接管票由可信入口持久分配，不是客户端传一个数字就算接管。
- 结果未知时保留原 Match ID、Player ID、Epoch、Seq 查询或原样重发，不换序号再买一次。

### 6. 领奖与幂等身份

`MultiSync_ChessClaim(player, match)` 同时锁 Player 与 Match：回查终态与该席排名，奖励 `(9 - 名次) × 10` 金币，Player 金币与 Match 的 `Claimed` 在同一 strict 事务里提交；并发重试和冷加载后的重试都只得一份。领奖要求 Player 已经 `Sync_ChessPlayerInit` 正式建档——只 `Create` 出内存实例不代表记录已持久化。这是同一原服内的多实体原子提交；Player 在别的 sid 时应经 Remote / Saga 回原服发奖，本地 MultiSync 不提供跨 sid 原子性（Saga 原生步骤的单进程容量有限，见 [Saga 指南 §5](guide/06-saga.md#5-接入与运维)）。

每席账本是对局里的持久业务事实，与资产同事务；它不是服务的短期去重环。每席至多 128 条，满了拒绝该席的新命令而不淘汰旧回执；定时器仍会推进，终局后仍能领奖。

### 7. 重连视图与存储格式

重连时调用锁内的 `Sync_ChessView(ctx, match, player)` 取当前视图：公开部分是各席的血量、名次、准备 / 连接状态与战场三格；本人部分加上经济、等级、商店、备用席与下一个序号；不含种子、卡池余量、对手的商店 / 备用席 / 账本。不要给 `State` 加 Sync 标记或直接下发整个实体；需要推送时按 [self_sync](PLAYER-ACCESS.md#7-玩家看自己的状态self_sync) 写业务视图。夹具没有联调公网协议、观战或客户端引擎。

存储格式要分两层看：

- **DAO 持久键**：`State` / `WakeTimer` / `Gold` / `Initialized` 的 `bson` 名就是 Mongo 里的键（DESIGN-TODO D13），`roost add timer` 加的节点与种子字段同理。以后改 Go 字段名要保留原 `bson` 名；改了 `bson` 名或删字段时，库里的旧键不再加载、下一次整文档写回即丢失，`roost generate` 与加载时只打 WARN。要改就先离线迁移（[schema 变更手册 §1](SCHEMA-MIGRATION.md#1-现行规则运行时只认当前-schema)）。
- **业务 JSON**：`State` 里的 `Schema=1` 是业务格式版本，加载时拒绝未知字段、未知版本、卡池张数不守恒与重算不一致的战斗结果。改业务格式同样要离线迁移并配套代码，只改 DAO 字段的 `_schema` 不等于迁移了 JSON 内容。

### 8. 接入时容易踩的问题

- **每张卡一个实体**：共享卡池的一致性靠“一局一个锁”最简单；拆成许多实体后买卡要跨实体事务，锁序和死锁都要自己处理。
- **定时器回调只改本实体**：回调里只经组件 API 操作本 Match 的期限，不另建调度器、不直接改节点 map；需要 Player 一起变化的事交给多实体 handler（[SLG §6](SLG-INTEGRATION.md#6-定时器回调能做什么)）。
- **handler 里不发同步 Nest 请求**：已知参与者一次传给多实体 Sender；handler 内的同步请求被框架拒绝（`ErrSyncInHandler`）。
- **锁目标要能被识别**：不写 `target` / `targets` 时，Nest 生成器把类型名含 `Entity` 的参数推断为锁目标（本例 `IChessMatchEntity` / `IChessPlayerEntity`），找不到时 `roost generate` 报错并点名 handler（见 [Nest 生成器](../../codegen/docs/NEST_RUNTIME.zh-CN.md)）。

### 9. 练习

加“羁绊：同种三个上阵时攻击加成”或“选秀回合”。先写清拥有者、幂等身份（哪个序号域）、事务参与者、配置代际和阶段推进的恢复语义，再选 Nest / 定时器 / Saga，不另造一套与框架平行的持久或并发状态机。

## 第二部分：验收

### 10. 业务纵切证明什么

```bash
bash scripts/test-auto-chess-generated.sh
```

脚本按 3.1 的步骤生成工程，replace 指回当前仓库，必须实际通过 12 条顶层 race 场景（3.4）且无 skip，随后执行消费者 build / vet、`roost id check` 与 `roost generate --check`；成功删除临时工程，失败或 `ROOST_AUTO_CHESS_KEEP=1` 时保留。

- **证明**：公共 CLI 生成的 Player / ChessMatch DAO、实体定时器、多实体 Sender 与 Nest 在 12 个业务组合下的事务、undo 回滚、锁顺序、业务幂等和期限核对；配置原子发布与对局冻结；视图按观察者裁剪。
- **不证明**：提交器是替身，不含文件 WAL fsync 与 Mongo；定时器由用例调用驱动的 `DeliverDue`，实体一直在内存，没有经过投递循环与加载后补触发；八个“玩家”是同进程里的并发调用，不是八个网络客户端；单进程，无 Redis / NATS；鉴权与接管票是合成的。夹具里的数值是样例。

### 11. 真实 WAL / Mongo 崩溃恢复

先按[隔离环境说明](../../wiring/scripts/integration/README.md)准备独立 Mongo 副本集并设置地址变量（不要指向生产库或共享业务库；共享隔离环境下遵守那里的持锁与命名规则），再运行：

```bash
ROOST_AUTO_CHESS_REAL=1 bash scripts/test-auto-chess-generated.sh
```

它在第 10 节之后追加 `TestAutoChessRealCrashRecovery`（已登记根包 `integration_coverage_test.go`），用本次唯一的 DAO 库与 WAL 目录：

1. 子进程经 strict 文件 WAL 提交八名玩家建档、成局，以及每人买卡、上阵、准备（第八人准备时冻结四场战斗）；后台投影关闭，断言 WAL 恰有 33 条已确认未投影的记录后，父进程 SIGKILL 自己启动的子进程。
2. 父进程用同一 WAL 与 Mongo 冷加载（业务时间推进一小时），核对整局状态（卡池、冻结的战斗、回执）与定时器 ID，重放全部 24 条玩家命令得到原回执；核对冷加载已把存量期限报告给驱动索引（`DueOf`）。
3. 启动驱动自己的投递循环（`Driver.Start`，用例不调用 `DeliverDue`、不手发 Tick）：循环对离线一小时的战斗阶段只补一次，进入第 2 回合准备、期限从当前业务时间起算（entitytimer `delivered_total` 增量 1）；之后用例每次把业务时间推到新期限，循环按时投递一次、推进一个阶段（计数逐次加 1），直到终局、不再挂定时器。终局时核对总数 = 1 + 2×(终局回合 − 1)（样例打满 8 回合，计数 15；第 1 回合的准备→战斗由写进程里第八人准备触发，不经定时器），全程 `redelivered_total` 增量 0。计数只有一个来源：ChessMatch 驱动只投递阶段节点，重放的玩家命令命中原回执、不重排定时器。停循环后八人各领奖三次，金币按名次各一份。
4. 正常关闭后再起第三个进程冷加载：核对整局状态与八人金币，再重放全部命令与领奖，结果不变。成功后删除本次库；失败保留生成工程、WAL 与库名供诊断。

用例确实依赖投递循环：把 `Start` 调用换掉再跑，用例在第 3 步等补触发处超时失败（`bounded wait expired`）。

- **证明**：strict 已确认的提交在 SIGKILL 后可从未投影 WAL 恢复（共享卡池、随机进度、冻结战斗、回执与阶段定时器），不会重新初始化整局；冷加载把存量期限报告进驱动索引；驱动投递循环在加载后只补一个阶段（投递一次）、之后每个期限投递一次并推进一个阶段到终局、无重投；跨进程重试不重复购买或发奖。
- **不证明**：提交在途 / 结果未知时的崩溃、磁盘断电、WAL 损坏或丢盘；循环在系统时钟下的长时间计时（用例冻结业务时钟、由循环按 MaxWait 重读）、准入失败重试与 Redeliver（由 entitytimer 包测试覆盖）；八个网络客户端、多进程 / 跨机、Redis / NATS；跨 sid 发奖；容量与长稳。

### 12. 结论怎样给

棋子、羁绊、经济与战斗缺少默认实现，不自动构成框架缺陷；合法接口调用下出现丢期限、卡池不守恒、重复扣费 / 发奖、状态越权或生成物编译失败，则应在公共实现中修复。本夹具接入时没有触发框架或生成器缺陷。当前存储是教学规模：每席账本 128 条、至多 8 回合、整局状态每次整体编码提交并在加载时重算战斗结果，没有生产吞吐或延迟承诺。

本机功能、跨机故障和长稳分别给结论；缺少实际证据的继续保留为未验收项。当前执行结果在 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)“自走棋接入验收的范围”维护，不为每一轮另建报告。
