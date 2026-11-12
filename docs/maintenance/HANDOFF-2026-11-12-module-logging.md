# 交接：给现有业务模块补业务层日志（2026-11-12）

维护者 2026-11-12：“player 相关的操作，业务层的日志要添加；高频的逻辑可以不加日志；日志使用框架的日志”，以及“日志我交给另一个 agent 去做”。本文是给接手 agent 的任务书。

## 0. 从哪里开始

- **基线**：远端 `feat/business-modules-portal`（登录线，已并入主线 `feat/business-modules` 的 `cb388ced` 与配方分支）。登录线合回主线之后，以 `feat/business-modules` 为准；动手前 `git fetch`，看哪个更新就从哪个拉。
- **分支**：自己开 `feat/business-modules-logging`，在自己的 worktree 里做，推到同名远端分支，**不要合进别的分支**，由维护者或原会话合。
- `GOWORK=off`。模块模板是编进 CLI 的：改模板后重新 `go build -o <scratch>/roost ./codegen/cmd/roost` 再生成工程验。

## 1. 规则

1. **玩家相关的操作要有业务层日志**：改了玩家状态、以后要能回答“这名玩家什么时候做了什么”的操作。
2. **高频逻辑可以不加**：移动、心跳、每帧 / 每次同步的推进、AOI、定时器的每次触发、只读查询、施法与伤害这类每秒多次的动作。要观测它们用指标，不用日志。
3. **只用框架的日志**：`log/slog` 的包级函数（`slog.Info / Warn / Error`，有 `ctx` 时用 `slog.InfoContext` 等）。框架在进程启动时（`infra/observe/log` 的 `Init`）把带上下文的 handler 装成 `slog` 的默认 logger，每行自动带 `goId`、`frame`、`server_time_ms`，在 Nest 或请求上下文里再带 `source`、`handler`、`player`、`msg`、`seq`。不用 `fmt.Print*`、标准库 `log`，不引入别的日志库。现有模块与 game-demo 都是这个写法，保持一致。

与组合 skill 的日志一节是同一套规则：`docs/agent-skills/roost-genre/references/compose.md` §4C。

## 2. 怎么打

- **在哪里**：记在事务提交之后——端点（`game/controllers/<module>/`）里 Nest 请求成功返回时，或服务端调用生成的 Sender 返回之后。**不要在 Nest handler 和组件方法里加成功日志**：事务回滚时那条日志也会留下。现有模块的日志都在端点 / controller 这一层，照这个位置加。
  - 由生成的转交代码直接接到 handler、没有手写端点的协议（例如 `WalletSpend`、`ShopBuy`、`CheckinClaim`）：先看该模块 controller 目录里哪些文件是工程所有、能不能在应答返回处加；做不到就在报告里列出来，不要为了加日志去改生成器或把 handler 改成别的形状。
  - 被别的模块在事务里调用的组件入口（`WalletComp().Add`、`BagComp().Add`）不在组件里打；由发起这笔事务的端点记（它知道原因）。
- **级别**：成功的玩家操作 `Info`；被业务规则正常拒绝的（余额不足、次数用完、名字不合规）**不打**，客户端已经拿到码；不该发生的（数据对不上、依赖给不出结论、结果未知、补偿失败）`Warn` / `Error`。
- **内容**：消息是一句固定的英文短语（现有风格，例如 `"rehome: transfer accepted"`），事实放键值对里：一律带 `player_id`，再带这件事的关键事实——数量、道具号、订单号、房间号、对方玩家号、变化后的值、业务键、原因。不把值拼进消息文字。
- **不能记**：票据、令牌、凭据、密钥、渠道返回的原始凭证；只记长度或是否存在。
- **顺手补的**：端点答“内部错误”而没有 `slog.Error` 的地方补上（带 `player_id`）；现有错误日志缺 `player_id` 的补上。

## 3. 各模块要补什么

下表是 2026-11-12 逐模块读代码的结果（“现状”是当时的；动手前以代码为准）。“高频不加”的已经剔掉。

| 模块 | 现状 | 要补的成功日志 | 顺手补的 |
| --- | --- | --- | --- |
| wallet | 只有生成转交的内部错误 | 余额变动：`WalletSpend`、服务端发放、GM 调整（delta、变化后余额、业务键） | — |
| bag | 没有 | 发放、丢弃（道具号、数量、变化后数量） | — |
| equipment | 没有 | 穿、卸（槽、道具、被换下的） | — |
| checkin | 没有 | 签到成功（连续天数、奖励） | — |
| task | 只有生成转交的内部错误 | 领奖成功（任务号、奖励）；`Report` 高频不加 | — |
| stamina | 只有生成转交的内部错误 | 付费补满（花费、当日第几次）、服务端发放（被上限截断时写明）；`Spend` 高频不加 | — |
| shop | 只有生成转交的内部错误 | 购买成功（商品、份数、花费、请求键） | — |
| gacha | 没有 | 抽卡成功（池、次数、花费、结果） | — |
| idle | 没有 | 领取成功（金额、是否封顶） | 时钟回拨被拒（`idle_clock_back`）记 `Warn` |
| cdkey | 没有 | 兑换受理、发件成功、确认完成 | 四步里任一步的基础设施失败；端点的内部错误 |
| purchase | 只有 `purchase collect incomplete` | **每笔发货成功**（订单号、序号、奖励、是否结清）；订单入队 | 因价格 / 商品不符被拒记 `Warn` |
| mailbox | 异常路径齐 | 领取成功（邮件、附件）；`SendTo` 发件成功；升级奖励发信成功 | — |
| profile | 没有 | 改名（旧名 → 新名）、升级（from → to）；不升级的发经验不加 | — |
| naming | 没有 | 改名成功（旧名 → 新名、请求键） | 结果未知留在途记 `Warn`；端点的内部错误 |
| friends | 没有 | 同意、删除、拉黑、取消拉黑（对方玩家号） | — |
| chat | 内部错误与推送 Debug | `AnnounceWorld`（低频）成功与失败；发言高频不加 | — |
| notice | 模块自己没有 | 服务端直接调 `Publish` 成功（kind、key） | — |
| leaderboard | 钩子失败、读榜内部错误 | `Submit` 按现有约定由调用方记，不改；读榜不加 | — |
| team | 内部错误（不带 `player_id`） | 建队、入队、离队、解散、队长变更 | 现有错误日志补 `player_id` |
| matchmaking | 内部错误（不带 `player_id`）、后台失败 | 成局（MatchID、RoomID、成员）；受理、取消、过期 | 现有错误日志补 `player_id`；`matchmaking_unknown` 记 `Warn` |
| matchroom | 没有 | 新建映射与开房成功（MatchID → RoomID、人数） | 冲突移交被拒记 `Warn` |
| room | 五个端点的内部错误 | 开房、入房、离开、房主交接、关闭；`room.Open` / `room.Disconnect` | — |
| turn | 三个端点的内部错误 | 开局、对局结束、超时跳过 | — |
| alliance | 异常与补偿路径齐 | 建盟、入盟、退盟、踢人、转让、捐赠（数量）、领奖、宣战；Saga 的扣费 / 退费 / 发奖成功 | — |
| activity、activity_world、activity_alliance | 只有 `driver.go` 两条 Error | 活动激活、阶段切换、`Complete`、`Reopen`、`JoinShared`（都低频） | — |
| dungeon | 只有排行榜投影失败 | 开局、通关或超时终局、领奖成功（局、经验、赛季） | — |
| combat | 没有 | 会话绑定与结束（低频）；施法与推进高频不加 | `combat_boundary` 记 `Error`；后台推进失败；四个端点的内部错误 |
| scene | 没有 | 进出与切换场景；开图、关图、排空、撤离、时间倍率变更 | — |
| monster | 只有重生推迟 | 出生、死亡、销毁、关图清怪；伤害高频不加 | `MonsterInspect` 的内部错误 |
| terrain | 4 处 `Error` | 建造、拆除（低频、持久）；开图、停机、`Refresh`；笔刷高频不加 | 端点的非业务错误 |
| actor | 没有 | 移动高频，不加；进出空间可选 | — |
| attribute | 没有 | 升级成长由 profile 侧记，不重复 | `attribute_overflow` 记 `Warn` |
| enter | 拒绝与失败路径齐 | 新玩家建成（首次初始化提交）、`EnterCreate` 建成（带名字）；每次进入成功一条 | — |
| login | 握手被拒 Debug、account 无结论 Warn | 握手成功（玩家号、承载服、握手方式、是否新建映射；**不记票据**） | — |
| portal | 启动与目录告警 | 登录成功、签发进服票据（账号、服号；**不记凭据与票据**）；登录被渠道拒绝记 `Info`（不带凭据） | — |
| gm | 执行前一条审计 | 执行结果（成功或拒绝的码），与执行前那条能按 trace 对上 | — |
| rehome | 很全 | 不用补 | 申请被规则、冷却或 account 拒绝时补一条 `Info` |
| rules、servertime、skillbook | 只读或启动期 | 不用补 | — |

登录线（enter、login、portal）的握手与进入是每名玩家每次上线一次，不算高频，要记。

## 4. 不要做的

- 不改行为：不改 handler 的事务内容、返回值、错误码，不改协议。本任务只加日志（以及第 2 节“顺手补的”那两类）。
- 不改场景数：加日志不应增减测试函数；`module.yaml` 的 `verify.pass` 不变。需要为日志加用例时，报告里说明并同步清单、门禁脚本与生成器测试里的数字。
- 不修读代码时发现的别的问题（下面 §6 列了一批已知的）；发现新的记进报告。
- 不动 `docs/agent-skills/roost-genre/references/compose.md` 的规则部分；做完之后可以把 §4C 里“现有模块的日志并不齐”那一条改成现状。
- game-demo 覆盖了模块文件的地方（`codegen/internal/roost/demo.go` 的 `demoModuleSteps` 里的 `write` 项，例如 enter 的端点）要同步加，否则 demo 里那几条操作仍没有日志。

## 5. 验证与交付

- 每改完一批模块：新建 `-template game` 工程装这批模块，`go build ./...`、`go vet ./...`、`roost module verify`（场景数不变）、工程自己的 `go test ./...`。
- 全部改完：门禁脚本 `scripts/test-modules-generated.sh`、`go test -run 'TestDemo|TestModule|TestRecipe' ./codegen/internal/roost`、新生成的 game-demo 的 `go test ./...`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`。这些跑一次即可，放在最后。
- 至少对三个模块（建议 wallet、purchase、enter）起真实进程看一眼日志确实打出来、字段齐、没有票据（`make dev-run`，照 `demo/WALKTHROUGH.md` 的登录一节走一遍）。
- 文档：每个改到的模块在它 `README.md` 的“能力”一节末尾加一小段“日志”（记了哪些操作、在哪个文件）；`CHANGELOG.md` 的 `[Unreleased]` 加一条。
- 报告：逐模块列出加了哪些日志（操作、级别、字段、文件）；按“高频”没加的；因为没有手写端点而没法加的；跑了哪些检查及结果。

## 6. 读代码时已经发现、不在本任务里修的问题

供参考，避免重复报告；是否修由维护者定。

- scene：场景已满、目标未开放等业务拒绝作为 Go error 返回，按代码链路会导致连接被关闭；模块没有声明业务错误码。
- terrain、monster、actor：没有业务错误码，玩家可触发的拒绝到客户端都是内部错误。
- activity 与 enter 的联动：进入回调把原始玩家号直接传给 Sender，别处都先转成实体号；未运行确认。
- alliance：申请 / 批准、踢人、转让、宣战只有服务侧 RPC，没有客户端协议；模板里残留 demo 的注释。
- friends：六种动作都要求对方在线。equipment：同一道具换等级再穿需要背包里另有一件。
- team：被邀请的人收不到邀请通知，也没有查询入口。combat：死亡之后没有复活或回血的路。
- notice：两条 `gm.notice.*` 命令不经 gm 模块的登记，gm 对操作者与备注的必填检查对它们不生效。

## 7. 接手记录

按维护者补充：基线固定为登录线 `17d35554`；玩家操作 Info，高频成功不记，其余有价值的低频诊断 Debug。本轮已在独立 `feat/business-modules-logging` 完成日志接入及所列验收；逐模块覆盖、未能在不改事务接口的条件下补齐的边界与验收见[日志报告](evidence/module-logging-20261112/README.md)。不合并其他分支、不发版。
