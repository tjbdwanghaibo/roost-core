# 按需求组合：用 roost-core 定制自己的服务逻辑

适用：用户给的是一段需求描述（“做一个有货币、签到和每日任务的服务”），不是“照某类玩法出一个 demo”；或者要在已有的 roost 工程上继续加功能。照某类玩法出 demo 用各玩法的 reference。

Markdown 链接相对本文件所在目录；反引号里写的路径（`modules/<name>/README.md` 等）相对 roost-core 根目录。命令以目标检出为准：`roost module` 在 v1.43.0 之后的 main 上才有（v1.43.0 及更早的 tag 没有），更早的 tag 没有这条路，只能手写（第 4 步）。

## 0. 四条规则（总边界，每一步都受它们约束）

1. **遵循框架的核心编程思路**（第 0A 节）。业务怎么改状态、怎么落盘、怎么让客户端看到，框架已经定了做法；不另起一套。
2. **模块里有的逻辑，复用**。动手之前每条需求都要过一遍“有没有模块已经做了”（第 1 步）。有就装模块、调它的入口、接它的登记点，不重写一份。
3. **不能复用，就借鉴**。没有模块覆盖时，先指定一个最接近的模块当样板，照它的结构、登记点与测试写法来（第 4 步）；不发明另一种风格。
4. **需求与限制冲突，必须提出**。对照模块的能力边界、[能力清单“明确没有 / 不适合的”](../../../framework/CAPABILITIES.md#明确没有--不适合的)与[已知限制](../../../maintenance/KNOWN-LIMITS.md)查出来的冲突，停下来告诉用户并给出选项（第 1 步末尾）。不许悄悄跳过、悄悄降级、悄悄换一种做法绕开。

另有一条编码规则贯穿手写与扩展：**玩家相关的操作要有业务层日志，高频逻辑可以不打，日志只用框架的日志**（第 4C 节）。

**本 skill 对使用它的 agent 只读**：不修改 `docs/agent-skills/roost-genre/` 下的任何文件，也不改装到本机的副本；不修改 roost-core 检出里的模块模板与框架代码。发现 skill 写错、写漏、照做走不通时，告诉用户（哪份文档哪一节、原文、实际情况、你怎么绕过的），由 roost-core 的维护者改；绕过的办法可以记进工程的记忆（第 2A 步），不能记进 skill。

**记忆可以保留，放在工程里**：每个工程的需求、决定与改动记在工程自己的 `docs/DEMO.zh-CN.md`（当前状态）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更）里，下一次增改需求时先读它们（第 2A 步）。agent 自带的记忆功能可以另记开发者的偏好（常用的模式、习惯的边界划法），但工程里的这两份是准；两边不一致时以工程为准，并提醒用户。

能做到什么先说清楚：

- **有现成模块的需求**：一条命令装进工程，带验收过的场景测试。模块速查表在第 3 步；每个模块能做什么、不能做什么写在它自己的 `modules/<name>/README.md` 里。
- **这一档交付到哪**：业务逻辑写完、离线场景通过、有“进入游戏”端点（建档，第 4B 步）、客户端可见的状态接上自身视图（第 4A 步）、客户端够得着的登录链路（第 3A 步）。这是离线验收；真起进程见 [SKILL §4](../SKILL.md)。
- **没有现成模块的需求**：用 `roost add …` 建骨架、手写业务。框架给的是事务、持久化、同步、定时、跨进程这些底座，规则要写。
- **框架不支持的需求**：如实说明并给出处，不拼假 demo。见 [SKILL §6](../SKILL.md) 与能力清单“明确没有 / 不适合的”。

交付时这几类要分开报告（第 6 步）。

## 0B. 两种模式：业余（默认）与专业

**开始时只问一个问题**：“要用专业模式吗？专业模式会在拆完业务后请你逐项确认；不选就用业余模式，由我按样例值直接做完。”用户说不用、没回答、答得不明确，或者没有人可问（被别的 agent 调用、无人值守），都按**业余模式**。用户中途要求切换，从当前步骤起按新模式做，已经做完的不返工。在已有工程上增改需求时，沿用 `docs/DEMO-HISTORY.zh-CN.md` 最近一次记录的模式，不再重问；开头说一句“沿用上次的××模式”，用户可以换。

两种模式走同样的步骤、守同样的四条规则，**最后都要生成同样的交付物**：工程、第 5A 步的说明文档（`docs/DEMO.zh-CN.md`，哪种模式都不能省）、第 6 步的报告。区别只在第 1 步拆完之后由谁拍板：

| | 业余模式（默认） | 专业模式 |
| --- | --- | --- |
| 需求拆分 | skill 自己拆 | skill 拆好一版，提问请开发者确认或修改，确认之后才写代码 |
| 服务器 / 客户端的边界 | skill 按第 1 步“再分端”的缺省规则自己分 | **由开发者把控**：skill 逐个列出逻辑点、给建议与放到客户端的风险，开发者逐个拍板（见下文） |
| 缺的数值、周期、上限 | 用样例值继续，记成“待确认”写进文档与报告 | 列出来提问；用户不给的再用样例值，同样记成“待确认” |
| 复用结论（装 / 扩展 / 借鉴 / 手写） | skill 按第 1 步的表自己定 | 列出来提问，尤其是“装了再扩展”还是“借鉴后手写”这类两可的 |
| 状态归属与服务 | skill 按第 0A 节自己定 | 提问确认：状态挂在哪个实体、新服务还是现有服务、要不要跨服务 |
| 需求与限制的冲突（规则 4） | **照样必须提出**：所有冲突合成一个问题，每个冲突用一两句白话说明，并标出推荐做法 | 照第 1 步的四要素格式逐条提出 |
| 措辞 | 少用框架术语，必须用时顺手解释一句 | 可以直接用框架术语 |

**专业模式的提问**放在第 1 步末尾、写任何代码之前，一次问完，不要边做边零碎地问。问题按这几类列，每类给出 skill 的建议，让用户只改不同意的地方：

1. 需求条目：拆成了哪几条，有没有漏的、多的。
2. 边界：每条需求拆成逻辑点（校验、计算、随机、计时、判定、展示……），每个逻辑点给出建议放哪一端、理由，以及放到客户端的风险（可被改包作弊、多端结果不一致、断线后对不上）。重点列出两可的：移动与碰撞、战斗数值与命中、冷却与倒计时、寻路、客户端预测与回滚、本地缓存的配置。开发者逐个定。
3. 数值：每条缺的数值、周期、上限，以及建议的样例值。
4. 复用：每条的结论与模块或样板。
5. 归属：每条的状态挂在哪个实体、跑在哪个服务上。
6. 冲突：照第 1 步的四要素格式。

**边界以开发者的决定为准**。开发者把影响结果、资产或公平的逻辑放到客户端时，照做：服务器只做开发者要求的那部分校验（比如只校验上限、不重算），并在说明文档里把这一条标成“开发者决定”，写明风险。框架本身强制的不能挪：身份取自会话、业务时间取服务器、状态只经 Nest handler 改。开发者要挪这些时，按规则 4 作为冲突提出。

用户确认之后，确认的结果原样写进说明文档的“需求对照”与“服务器 / 客户端边界”两节。之后用户改需求，回到第 1 步重新拆，只问受影响的条目。

**业余模式不等于跳过检查**：限制照样逐条查，冲突照样停下来问；没有冲突的条目不问，直接做。最终报告的开头写明“本次用的是业余模式，以下几项是我替你定的”，并列出 skill 自己定下的拆分、样例值、复用结论与归属，方便用户事后改。

## 0A. 框架的核心编程思路

三块基础各回答一个问题：Nest 决定何时、在什么锁下修改实体；DataEngine 决定修改怎么落盘与恢复；Sync 决定哪些已提交的状态给哪些客户端看（[总览](../../../framework/guide/00-overview.md)）。写业务代码时落到下面这些做法上，装模块、扩展模块、手写都一样：

| 做法 | 含义 | 出处 |
| --- | --- | --- |
| 状态在实体的 DAO 里 | 挂在已有实体上的状态是 DAO 的嵌套结构体；不挂在玩家身上的（房间、公会、全服对象）用独立实体。不用进程级全局变量存业务状态 | [Nest 与实体](../../../framework/guide/02-nest-entity.md) |
| 改状态只在 Nest handler 里 | 一条请求是一笔事务。状态只经生成的 setter 改；组件不另存一份会被回滚的东西，回滚统一走 DAO | 同上 |
| 同一个实体上的协作直接调组件 | 依赖的模块的组件在同一个实体上时直接调用（同一把锁、同一笔事务）；不在同一个实体上用多实体 handler；跨进程用 Remote / RPC / Saga | [Remote](../../../framework/guide/05-remote-mirror.md)、[Saga](../../../framework/guide/06-saga.md) |
| handler 里不等待 | 不在 handler 里等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体 | [Nest 三池与 Await](../../../framework/NEST-AWAIT.md) |
| 落盘交给 DataEngine | 不自己写库。请求返回的错误要分清“明确被拒”与“结果未知”，结果未知不能当成没发生 | [DataEngine](../../../framework/guide/03-dataengine.md)、[回复错误判别](../../../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试) |
| “只做一次”记在自己的状态里 | 首次发放、每日领取、订单到账：先判自己状态里的事实，再发放；不靠“这次是不是新建的”，不靠客户端不重发 | 第 4 步 |
| 客户端看到的是已提交的状态 | 给客户端看的字段标 `sync`，提交之后由 Sync 下发；不在 handler 里手工推状态。协议推送只用于通知（告知、公告） | [Sync](../../../framework/guide/04-sync.md) |
| 时间用业务时间 | handler 最后一个参数 `nowUnixMilli`；“今天”用业务日；随时间恢复的数值惰性结算，不用定时器逐点加；到点要做的事用实体的持久定时器 | [时间](../../../framework/guide/10-time.md) |
| 身份取自会话 | 玩家是谁取已认证的连接，不取请求字段；客户端不传时间、不传玩家号 | [玩家接入](../../../framework/PLAYER-ACCESS.md) |
| 号与存储键稳定 | 协议号、错误码、组件号、`store` 键分配之后不改、不复用；不改带 `_gen` 的文件 | [tag 手册](../../../../codegen/docs/TAGS.zh-CN.md) |
| 业务拒绝用错误码 | 从 `internal/errors` 返回带码的错误，端点把它变成应答里的 `Code` / `Reason` | 第 4 步 |

拿不准某个做法时读表里的出处，不凭印象。现有模块都是按这些做法写的，所以“照最接近的模块写”通常就是对的。

## 1. 拆需求、定复用、查限制

**先拆**：把需求拆成条目，一条一件事（“每天能签到一次”“签到给金币”“连续签到有加成”）。数值、周期、上限写进条目里；用户没给的不要自己填，记成待确认。

**再分端**：每条拆成服务器的部分与客户端的部分，写进需求对照表。

| 归服务器 | 归客户端 |
| --- | --- |
| 决定结果的一切：校验、扣与发、随机、次数与上限、时间与日界、排名、谁能对谁做什么 | 界面、动画、输入；按已同步的状态展示（余额、进度、“今天领过没有”）；把玩家的意图发成请求 |
| 状态与持久化、跨玩家与跨服务的协作、给客户端看哪些字段 | 收推送后的提示；请求的重发（带同一个请求键）；按错误码给提示 |

缺省规则（业余模式照它分；专业模式由开发者把控，见第 0B 节）：**影响结果、资产或公平的判断只在服务器做**，客户端的判断只用于提前提示，服务器照样再判一次；客户端不传时间、不传玩家号、不传随机结果。一条需求只有客户端部分（纯展示、本地设置）时，标成“客户端，服务器不涉及”，不为它写服务器代码。

本 skill 只生成服务器：工程、模块、手写功能与场景测试。客户端的部分不生成代码，交付的是对接说明（第 5A 步的“客户端对接”一节）：调用顺序、请求与推送、要解码的同步字段、错误码与该怎么提示。客户端拿生成工程里的 `protocol/proto/protocol.proto` 与 `db/dao_sync.proto` 生成自己的协议代码。

**再逐条定复用**（规则 2、3）。每条先查第 3 步的模块速查表，再读候选模块的 `README.md`——“能力”一节看它做不做这件事，“能力边界”一节看它做到什么程度，“复用方式”一节看怎么用。然后给每条一个结论：

| 结论 | 条件 | 怎么办 |
| --- | --- | --- |
| 直接装 | 模块的能力覆盖这条需求，边界不冲突 | 第 3 步安装 |
| 装了再扩展 | 模块覆盖大部分，差的那点落在它留给工程的文件或登记点上 | 第 3 步安装，然后按它 README 的“复用方式”改工程文件、登记钩子；不改它别的文件 |
| 借鉴 | 没有模块覆盖，但有形状接近的模块（同样是“周期性发放”“带上限的资源”“两个玩家之间的关系”……） | 第 4 步手写；写明样板是哪个模块、借鉴的是哪一点 |
| 手写 | 没有接近的模块，框架有对应的能力 | 第 4 步手写；先读[能力清单](../../../framework/CAPABILITIES.md)“文档”列里的契约，再挑一个结构上最像的模块当样板 |
| 做不了 | 只有设计 / 原型 / 不支持 | 告诉用户，给出处；不做 |

能力清单末尾的“需求条目 → 能力”速查表、各分组表的“怎么接入”与“状态”列用来判断后两类；有玩法夹具或 game-demo 的参考时照参考的做法，夹具与 demo 不能直接装进别的工程。

不要因为“模块多带了点东西”就改成手写：模块自带的客户端协议用不上时告诉用户它存在，由用户决定去留。也不要因为“差一点”就复制模块的代码另改一份：差的那点应当落在它留给工程的文件或登记点上，落不上再考虑借鉴。

**最后查限制**（规则 4）。对每条需求连同它的结论，对照三处：候选模块 README 的“能力边界”、能力清单的“明确没有 / 不适合的”、[已知限制](../../../maintenance/KNOWN-LIMITS.md)里相关的条目。下面这些都算冲突：

- 需求要的量、频率、一致性超出模块或框架写明的范围（例如要“永久防重”，而货币的去重窗口只留最近 128 次）；
- 需求要的行为模块明确不做（例如要全服唯一的名字，而档案模块不查重）；
- 需求依赖的能力在能力清单“状态”列或已知限制里明确标成只有原型、只有设计、只有离线验证，或者写明“尚未确认”（例如换服）。模块场景都是离线跑的这件事不算冲突，统一写进第 6 步的“验收到哪一步”；
- 两条需求各自可行、放在一起互相矛盾。

有冲突就**停下来报告，等用户选**（两种模式都一样；业余模式把全部冲突合成一个问题，用白话写并标出推荐做法，见第 0B 节）。专业模式的报告里每个冲突写四样：

```text
需求条目：<原话>
冲突的限制：<哪份文档的哪一条，原文一句>
影响：<照做会怎样；不做会缺什么>
可选做法：A. 改需求为……  B. 换成……（代价是……）  C. 接受这个限制，按……交付
```

用户选定之前不要动这一条（别的条目可以先做；专业模式下先等第 0B 节那一轮确认）。用户选了“接受限制”的，在最终报告里原样列出。没有冲突也要在报告里写一句“已对照三处，未发现冲突”，说明查过。

## 2. 工程

```bash
set -euo pipefail
export GOWORK=off
CORE=/path/to/roost-core                  # roost-core 检出
WORK=/path/to/work
NAME=mygame; MOD=example.com/mygame
mkdir -p "$WORK"
(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost)   # CLI 与 CORE 同版本
ROOST="$WORK/roost"; DIR="$WORK/$NAME"
```

- **新工程**：

  ```bash
  "$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game --skip-deps
  cd "$DIR"
  go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
  ```

  `-recipe casual` 或 `-recipe turn-pvp` 可直接创建已组合工程（自动采用 game 底座），两者都已带登录链路（login、portal、servertime）与开发配置：casual 是首次发放、签到、任务、体力、商店、背包、邮件；turn-pvp 是档案、本服匹配（matchmaking → matchroom）、房间与回合，组队用 team。模块名单与验收见[配方](../../../../modules/README.md#从配方创建工程)。需求覆盖配方的全部模块时才从配方起步，再按第 1 步的结论往上加；模块不能卸载，配方多出来的模块装了就去不掉，这时改用 `-template game` 加 `module add`。

  `-template game` 带 Player 与 World 两个实体、九个托管框架服务。玩家模块挂在 `Player` 上，要求这个形状；room 自带 Room Entity、DAO 与 lifecycle；skillbook 是服务级只读目录；alliance 会新增一个独立的联盟服务与三个跨域 Saga。
- **已有工程**：`cd` 进去，先读 `roost.yaml`：`modules` 段是已装的模块（每个模块拥有的文件、DAO 字段、号），`services`、`features`、`ids` 是装配现状。再 `"$ROOST" module list`，带 `*` 的是已装的。不要重装已装的模块；已装模块的文件是业务代码，可能已经被改过，以工程里的为准。

## 2A. 后续增加与修改需求

工程生成之后，需求会不断增加和修改。每一轮都按这个顺序做，不从头重新生成工程：

1. **先读记忆**：`docs/DEMO.zh-CN.md`（当前有哪些需求、边界、模块、实体、服务、手写功能）、`docs/DEMO-HISTORY.zh-CN.md`（以前每轮的原话、决定、冲突的选择、待确认项）、`roost.yaml`。记忆与工程对不上时（有人手改过工程），以工程为准，先把差异告诉用户，再把文档改成与工程一致。
2. **分清这一轮是什么**：新增的条目、修改的条目（指出改的是文档里的哪一条）、删除的条目。只对这些条目走第 1 步（拆、分端、定复用、查限制）；专业模式只就这些条目和受它们影响的旧条目提问。
3. **查与旧需求的关系**：新条目与已做的条目互相矛盾、或者会改变已做条目的行为时，算规则 4 的冲突，提出来让用户选，不要悄悄改掉旧行为。以前用户“接受限制”的条目，这一轮的改动让限制不再成立或变得更严时，也要提。
4. **修改与删除有额外的限制**，动手前对照：
   - 号与存储键不改、不复用：改协议、错误码、组件或 DAO 字段时只能加新的；旧的协议号、错误码、`store` 键弃用但保留，记进文档。
   - 模块不能卸载：删掉一个由模块提供的功能，做法是去掉入口（端点、客户端协议的处理）并在文档里标“已停用”，状态与号留着；用户坚持要彻底删，按 `roost.yaml` 的 `modules.<name>` 清单手工删并说明风险。
   - 改持久化状态的含义（字段改名、单位改变、拆分合并）：先问工程是否已经上线、有没有真实数据。已上线的按[schema 变更手册](../../../framework/SCHEMA-MIGRATION.md)做离线迁移，这是冲突，提出来；未上线的直接改，并在历史里写明“未上线，不做迁移”。
   - 已装模块的文件是业务代码，可能被上一轮改过：改之前读工程里现在的版本，不按模块原样覆盖。
5. **改完照第 5 步验收，并重跑以前所有的场景**：已装模块的 `module verify`，加上以前每轮手写的场景（文档“手写的功能”一节有清单）。以前的场景因为这一轮的需求而变红时，先判断是旧需求被这一轮改了（照用户的决定改测试，写进历史），还是这一轮改坏了（修代码）；不能为了变绿去删旧场景。
6. **更新记忆**：把 `docs/DEMO.zh-CN.md` 改成改完之后的当前状态；在 `docs/DEMO-HISTORY.zh-CN.md` 末尾追加一节（格式见第 5A 步）。历史只追加，不改以前的记录；以前的记录写错了，追加一条更正。

第 6 步的报告只讲这一轮：新增、修改、删除了什么，影响到哪些旧条目，验收结果（新场景与回归场景分开报数）。

## 3. 装现成模块

**先查表，再读它的 README**。下表按需求找模块；“连带”是安装器会一并装上的依赖。表只是索引，能力与边界以 `modules/<name>/README.md` 与 `"$ROOST" module list --json` 为准。

| 需求 | 模块 | 连带 | 一句话 |
| --- | --- | --- | --- |
| 进入游戏、建档、首次发放 | `enter` | — | `EnterGame` 加载或创建 Player、首次初始化；“先新建再进入”的另一条路；进入钩子；打开自身视图 |
| 握手鉴权 | `login` | — | 握手票据交给 account 校验（进服票据、角色会话令牌），开发登录，区服登记工具 |
| 客户端登录入口、选服 | `portal` | login | 三个 HTTP 端点：登录、区服清单与推荐、换进服票据；挂在托管 account 的进程上 |
| 服务器时间 | `servertime` | — | 业务时间、当前业务日、下一次每日重置；进入时推送 |
| 客户端规则下发 | `rules` | enter | 各模块登记的规则快照，进入时统一推送、可补取 |
| 档案：名字、等级、经验 | `profile` | enter | 改名、发经验、升级钩子；新角色 1 级 |
| 全服唯一名字 | `naming` | profile | 两阶段占名、改名状态机、旧名释放 |
| 货币 | `wallet` | — | 余额、按业务键幂等的加减、消费协议 |
| 背包、道具 | `bag` | — | 道具表、堆叠上限、发放与丢弃 |
| 装备 | `equipment` | bag | 槽位、穿卸与背包原子扣回 |
| 属性 | `attribute` | profile | 基础 / 装备 / 最终属性分层，随等级成长 |
| 每日签到 | `checkin` | wallet | 业务日切分、连续天数加成 |
| 任务 | `task` | wallet | 事件推进；每日 / 每周 / 永久、前置链；配置表与奖励 |
| 体力 | `stamina` | wallet、rules | 惰性恢复、消耗、花金币补满 |
| 商店 | `shop` | wallet、bag、rules | 金币买道具、限购、请求键防重 |
| 抽卡 | `gacha` | wallet、bag | 权重与保底、单抽与十连；组件有不扣费的单抽 `DrawFree`（每日免费这类次数规则写在工程里）；保底不能关，不要保底按 README 配到上限 |
| 离线收益 | `idle` | wallet | 按间隔惰性结算、时间与金额双上限 |
| 兑换码 | `cdkey` | mailbox | 一码一次或一人一次 |
| 充值发货 | `purchase` | wallet | platform 记单、顺序发货、恰好一次 |
| 邮件 | `mailbox` | wallet | 列表、领取附件、发件入口、全服邮件 |
| 聊天 | `chat` | — | 世界频道、私聊、历史、系统公告 |
| 公告、跑马灯 | `notice` | chat | 可信服务端入口与 GM 命令 |
| 好友 | `friends` | — | 申请、同意、删除、黑名单（同服）；申请、同意、删除、拉黑要求双方都在本进程在线 |
| 排行榜 | `leaderboard` | — | 前 N 名与自己的名次、提交入口；分数只升不降，会下降的分数（余额、天梯分）不能用 |
| 队伍 | `team` | — | 建队、邀请、入队、离队、队长交接 |
| 匹配 | `matchmaking` | team、matchroom | 本服单人或整队匹配，成局移交房间 |
| 匹配房间 | `matchroom` | room | MatchID 到房间的幂等移交、固定名单 |
| 房间 | `room` | — | 有限座位、进出、房主交接、重连 |
| 回合 | `turn` | room | 行动者、命令序号与重放、超时推进 |
| 联盟 / 公会 | `alliance` | bag | 独立联盟服务、成员、捐赠领奖、三个 Saga |
| 活动 | `activity` | — | 五阶段活动、循环、个人状态与定时器 |
| 全服 / 联盟活动 | `activity_world`、`activity_alliance` | activity（alliance） | 活动的权威宿主与恢复 |
| 移动 | `actor` | — | 心跳、位置朝向、路径与跟随 |
| 场景与视野 | `scene` | actor | 多图分线、切换、AOI 与公开字段同步 |
| 怪物 | `monster` | actor、scene | 巡逻、追击、死亡与重生 |
| 技能目录 | `skillbook` | — | 启动时编译技能定义，客户端读同一份 |
| 战斗（双人 PVP） | `combat` | actor、attribute、skillbook | 正式技能运行时、Buff、冷却 |
| 副本 | `dungeon` | profile | 阶段事件、可信开局与推进、恰好一次奖励 |
| 可编辑地形 | `terrain` | scene | 地形块、笔刷、建筑占地 |
| 换服 | `rehome` | — | 把一名玩家改绑到另一个 game sid |
| 运维命令 | `gm` | — | 命令登记与审计，各模块按联动接入 |

```bash
"$ROOST" module list --json                 # 模块、依赖、状态字段、场景数
"$ROOST" module plan checkin --json         # 只预览：不改动工程
"$ROOST" module add checkin                 # 连同依赖（wallet）安装并运行生成器
"$ROOST" module verify                      # 每个模块的场景数 + build / vet / id check / generate --check
```

- `plan` 的 JSON：`modules[].status` 为 `install`（将安装）、`installed`（已装）、`installed_other_version`（已装的是另一版，安装器不动它）；`prerequisites` 是会先执行的 `roost add`；`ids`、`state`、`files` 是将分配的号、追加的 DAO 字段、写入的文件；可选 `runtime` 是自动接入服务启动与停机的入口，安装后连同目标 service、requires 记进 roost.yaml，不用改业务 Service；见 [服务级接线](../../../../modules/README.md#服务级启动与停机)。
- `add` 报错时按错误文字处理，不要绕过：
  - `already exists and is not this module's file`：落点被业务文件占着。读那个文件，告诉用户，由用户决定保留自己的实现还是让位。
  - `cannot be installed yet`：缺 Mod、feature 或托管服务，错误里给了要执行的命令。
- **联动文件**：输出里的 `glue <模块> with <另一个模块>` 是两个模块之间的接线（例如 chat 在玩家进入游戏时把他加进在线名单、签到成功推进每日任务），两个模块都装着时安装器自动写，不论先装哪个；`plan --json` 的 `glue` 数组列出会写的文件。它们归工程所有，规则直接在文件里改。`module verify` 把它们的场景单独列一行 `<模块>+<另一个模块>`。
- 模块自带的客户端协议（如 wallet 的 `WalletSpend`：客户端发起的消费）是样例入口。需求里没有对应的客户端操作时，告诉用户它存在，由用户决定保留还是删掉这条协议与端点。
- 装完之后模块文件就是业务代码：要改规则直接改（例如 `game/entities/player/checkin_component.go` 里的奖励），改完再 `module verify`。改了行为就同步改模块自带的测试，不要删测试。

- **扩展已装的模块**（第 1 步结论是“装了再扩展”的）：只动两类地方——模块留给工程的业务文件（README 的“复用方式”列出，例如商品目录、推荐规则、渠道入口、首次发放），和它的登记点（进入钩子、升级钩子、附件种类、规则登记……）。要在别的功能里用它的数据或动作，调它 README 写的入口（同一个实体上直接调组件方法，例如 `component.owner.WalletComp().Add(delta)`）。它别的文件不要改；确实要改规则时改完同步改它自带的测试，再 `module verify`。

## 3A. 登录与进入

客户端从“有一个渠道身份”到“进入游戏”是一条固定的链路，四个角色各管一段，都有现成模块，**不要手写另一套登录**：

| 角色 | 管什么 | 现成的 |
| --- | --- | --- |
| account（托管的框架服务，全局、无状态） | 渠道身份 → 账号；账号在每个服上的玩家号；签发与校验全部票据 | 框架自带；渠道验签在工程的 `internal/service/account/login_channels.go` 接 |
| 登录入口（HTTP，挂在 account 的进程上） | 登录、区服清单与推荐、换进服票据与该连的地址 | 模块 `portal` |
| Gate / 玩家 TCP | 握手时把票据交给 account 校验，没有映射就新建 | 模块 `login` |
| game | 告诉客户端要不要先新建；新建（角色名在这里起）；进入 | 模块 `enter`（装了 `profile` 时名字进档案） |

```bash
"$ROOST" module add portal enter            # 连带 login；要名字再加 profile，要服务器时间加 servertime
"$ROOST" config enable player-tcp
```

客户端的顺序：`POST /portal/login`（渠道、open_id、凭据 → 账号票据）→ `POST /portal/servers`（清单与推荐的服）→ `POST /portal/enter`（进服票据与地址）→ 用 `enter:<进服票据>` 握手 → 服务器推 `EnterStatus{need_create}` → 需要新建就发 `EnterCreate{name}` → `EnterGame`。推送只是告知，没收到就直接发 `EnterGame`，得到 `enter_need_create` 再发 `EnterCreate`。

- 开发时要在托管 account 的服务的开发配置里写 `login.dev_token`（必须以 `dev-` 开头，只写在开发配置里）与 `portal.servers`（服号 → 客户端连得到的地址：有独立 Gate 填 Gate 的地址；没有时填 game 的 `player_access.tcp.addr`，把 `0.0.0.0` 换成客户端连得到的主机）；`-recipe` 与 `-template game-demo` 的工程已经写好，`-template game` 加模块的要自己写（安装输出的 `todo:` 行逐条照做）。区服要登记：`make dev-run` 起完服务后自动登记本机的服，别的环境用模块带的 `cmd/accountctl upsert-server`。
- 新玩家的初始发放写在 enter 的首次初始化里（第 4B 步），与“建成了”的标记同一笔事务；不要写在登录入口或握手里。
- 不经登录入口、由自己的入口调 account 的 `CreateRole` / `SelectRole` 拿 `session:<player_id>:<token>` 握手的老路仍然可用：这样的会话 `EnterGame` 是“加载或创建”，不要求先新建。
- 上线前要做而模块不替你做的：渠道验签、入口前面的 TLS 与限流、生产配置里的地址。这些写进第 6 步报告的“没做的”。
- 细节与边界读 `modules/portal/README.md`、`modules/login/README.md`、`modules/enter/README.md`；独立 Gate 的接法在 login 的 README 里。

## 4. 手写模块库里没有的功能

**先定样板**（规则 3）。第 1 步结论是“借鉴”或“手写”的每个功能，动手前写下两行：样板是哪个模块、借鉴它的哪一点。然后读那个模块的 `README.md` 与它的文件（装了它的话文件清单在 `roost.yaml` 的 `modules.<name>.files`；没装就读 roost-core 的 `modules/<name>/files/`），照它的结构写。常见的对应：

| 要写的东西像什么 | 样板 | 借鉴的点 |
| --- | --- | --- |
| 一个数值的加减、要防重发 | `wallet` | 可见状态与服务端账本分两个字段；带业务键的幂等 |
| 每天 / 每周做一次的事 | `checkin`、`task` | 业务日切分；“做过没有”记在自己的状态里 |
| 随时间恢复、有上限的资源 | `stamina`、`idle` | 惰性结算，不用定时器；零头与回拨的处理 |
| 花一种东西换另一种 | `shop`、`gacha` | 扣与发在一笔事务里；请求键防重；配置表 |
| 一份可以领的东西（奖励、补偿） | `mailbox`、`purchase` | 领取账本；恰好一次；附件种类做成登记点 |
| 两个玩家之间的关系 | `friends` | 两个 Player 同一笔多实体事务 |
| 多人共用的对象 | `room`、`team` | 独立实体；成员引用与对象同事务；重连代际 |
| 跨进程、要补偿的流程 | `alliance` | 单写者服务 + Mirror + 路由 Saga |
| 到点要发生的事 | `turn`、`activity` | 实体的持久定时器；超时推进幂等 |
| 进入游戏时顺带做的事 | `enter` 的联动（看 `modules/*/with/enter`） | 进入钩子在事务外、可重试；必须成功的放事务内钩子 |
| 给运维用的操作 | `gm` 的联动（看 `modules/*/with/gm`） | 命令登记、审计，写入仍走正式事务 |

借鉴的是做法，不是复制粘贴它的代码再改名：状态、协议、错误码都是你自己的，文件名带你的功能名。样板模块里与你的需求无关的部分不要带过来。找不到形状接近的模块时，回到第 0A 节的做法逐条对照，并在报告里写明“没有可借鉴的模块”。

**一个模块都没装时**，先补上模块安装会替你做的两件事，否则后面的步骤没有 `PlayerDao`、`add endpoint` 与 `add transport tcp` 也会被拒绝：

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add access player --service game
```

一个挂在 Player 上、带一条客户端协议的功能 `<feature>`，按这个顺序：

1. **状态**：新建 `db/def/<feature>.go`，定义嵌套结构体，字段只写 `store` 键（嵌套类型内从 `"1"` 起编号）：

   ```go
   package dbdef

   // TaskState 是每日任务的进度。
   type TaskState struct {
   	Day      int64 `store:"1"`
   	Progress int32 `store:"2"`
   	Claimed  bool  `store:"3"`
   }
   ```

   在 `db/def/player.go` 的 `PlayerDao` **末尾**加一个顶层字段，字段名与存储键带功能名：

   ```go
   	// Task 是每日任务给玩家本人看的进度。
   	Task TaskState `store:"task" dao:"persist,sync"`
   ```

   - 同步与否按这个顶层字段决定：标了 `sync` 的字段才会下发，`persist` 只落库。既有可见状态又有服务端账本就写两个字段、两个结构体。不要在嵌套结构体的字段上写 `dao` 标签。标了 `sync` 还要打开自身视图客户端才收得到，见第 4A 步。
   - 只能加在末尾（同步的字段号按定义顺序排）；不要改已有字段的 `store` 键。
2. **组件**：`"$ROOST" add component <Feature> --entity Player`，在生成的 `game/entities/player/<feature>_component.go` 里写业务方法。状态只经生成的 setter 改（`component.owner.Dao().GetTask().SetProgress(…)`）；组件不保存事务会改的状态、不加锁。同一个实体上的别的组件（模块的或自己写的）都用 `component.owner.<Other>Comp()` 直接调，在同一把锁、同一笔事务里。要动货币就调同一个实体上的货币组件，同一笔事务。两个入口按“谁来保证只做一次”选：
   - **你的状态里记着这一次做过没有**（今天领过、这个订单到过账）：先判自己的状态，再 `component.owner.WalletComp().Add(delta)`，不留回执。周期性发放（每日、每局、每个订单）都用这个。
   - **没有别的事实可判**（客户端带请求号的消费）：`WalletComp().Apply(key, delta)`，同一个 key 在去重窗口内重复调用只生效一次。窗口只留最近 128 次带业务键的加减，是短期防重发，不是永久账本：不要拿按天、按局递增的键去用它。
3. **错误码**：`"$ROOST" add errcode <feature>_<reason>`（每个一条，一码一个文件），生成在 `internal/errors/`，把说明文字（生成时是 `TODO`）改掉。模块是一个功能一个文件，两种布局都可以。业务拒绝就从组件 / handler 里返回这些带码的错误，生成的端点会把它转成回包的 `Code` / `Reason`；不用在 controller 里自己拼响应。
4. **协议**：`"$ROOST" add protocol <Feature><Action> --group game --handler <feature>`，在 `protocol/def/<feature>_<action>.go` 里给 Request / Response 加字段（Response 保留 `Code`、`Reason`）。
5. **Nest handler**：`"$ROOST" add handler <Feature><Action> --entity Player --component <Feature>`，把 `game/handler/<feature>_<action>.go` 改成收 PB 请求、回 PB 响应：

   ```go
   //roost:nest rollback=undo durability=strict
   func handlerTaskClaim(target player.ITaskEntity, request *pb.TaskClaimRequest, nowUnixMilli int64) (*pb.TaskClaimResponse, error) {
   	result, err := target.TaskComp().Claim(nowUnixMilli)
   	if err != nil {
   		return nil, err
   	}
   	return &pb.TaskClaimResponse{Reward: result.Reward}, nil
   }
   ```

   - 一个 handler 一个文件，文件名就是 handler 名的蛇形写法（`roost generate` 与 `add endpoint` 按文件名找）。
   - 最后一个参数写 `nowUnixMilli int64` 就能拿到这条消息冻结的业务时间；客户端不传时间。
   - `pb` 是 `<module>/protocol/pb`，要先跑一次 `"$ROOST" generate` 才有。
   - 请求没有字段时也保留 `request` 参数，形状与带字段的一样。
   - 不对客户端开放的写入（登录时的初始化、别的功能触发的发放）也写成 Nest handler，参数用普通类型，不建协议和端点，由服务端代码经生成的 Sender 调用。
6. **端点**：`"$ROOST" generate`，再 `"$ROOST" add endpoint <Feature><Action> --handler <feature>`，再 `"$ROOST" generate`。得到 `game/controllers/<feature>/` 下的手写入口与生成的转交。玩家身份取认证会话，不取请求字段。
   - 第一次 `generate` 之后 `game/controllers/<feature>/` 里已经有一个返回 `not implemented` 的入口桩（文件头写着 application-owned）。它没被改过时 `add endpoint` 会把它换成转交入口，不用手写、也不会被拒绝。
7. **场景测试**：照 `game/handler/wallet_test.go` 写 `game/handler/<feature>_test.go`：替身提交器、`nest.NewEngine`、生成的 `syncsender`，覆盖成功、拒绝时零写入、重复请求、提交被拒后回滚。要拨时间用 `clock.SetSource`（`game/handler/checkin_test.go` 有例子）。测试名加功能前缀；只断言自己的事实（同一个 DAO 上有别的功能的字段）。
   - getter 与替身提交器不借用模块测试文件里的类型（那些文件可能被改）；自己写的几个功能之间可以共用一个夹具文件。
   - 拨时间的写法（`clock` 是 `infra/base/clock`）：

     ```go
     var now atomic.Int64
     now.Store(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).UnixMilli())
     clock.SetSource(func() time.Time { return time.UnixMilli(now.Load()) })
     t.Cleanup(func() { clock.SetSource(nil) })
     // 之后 now.Add(毫秒数) 就是拨快业务时间；handler 的 nowUnixMilli 跟着变。
     ```

     `clock.SetSource` 是进程级的：一个用例只建一个 fixture，后建的会顶掉前一个的时钟。
   - 需求里给了数的（上限 20、每 6 分钟、每天 2 次），测试里把数字写死，不要引用被测代码里的常量——否则常量改错了测试照样通过。
   - 没有天然幂等键的写入（“每完成一局上报一次”）重发会多记一次。需要防重就让请求带一个业务键（局号、请求号）并在自己的状态里判重；不需要就把这条假设写进报告。

**日界**：“今天”用 `int64(clock.BusinessDay(time.UnixMilli(nowUnixMilli)))`（`infra/base/clock`；返回类型是 `clock.Day`，存进 `int64` 字段要转换），得到 `YYYYMMDD` 的业务日，直接存进状态、按大小比较；业务时区与每日重置时刻是部署配置（缺省 UTC 零点）。不要用 `nowUnixMilli / 86400000` 自己算。每日清零按惰性做：当天第一次写入时发现存的业务日不是今天就清。跨天后没有新写入时，客户端经自身视图看到的仍是昨天的记录——让状态带上业务日字段，客户端比对它判断是否已是新的一天。

**随时间恢复的数值**（体力、冷却）：不要用定时器逐点加。状态里存“上次结算后的值”和“下一点恢复的时刻”（已满时为 0），每次写入前先按 `nowUnixMilli` 把该恢复的点数结算进去（零头保留、到上限就停）。两个字段都同步给客户端，由客户端按它们推算当前值与倒计时——存的值只在写入时更新，客户端直接显示它会看到“不恢复”，这条约定要写进报告。

不挂在玩家身上的状态（房间、公会、全服对象）用独立实体：`"$ROOST" add entity <Name>` + `add dao <Name> --entity <Name>`，多人争用的对象把实体标记里的 `category` 改成 `entity.EntityCategoryWorld`。多实体事务、定时器、跨进程等见能力清单对应行的文档；这些比单实体功能复杂，先读契约再写。

不要做的事：不改带 `_gen` 的文件；不在 handler 里等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体；不用进程级全局变量在功能之间传状态。

## 4A. 让客户端看到状态

**装了 `enter` 模块就不用做这一步**：安装器已经代办，`roost project doctor` 里有一行 `OK self-sync`。没装时：标了 `sync` 的字段要打开玩家自身视图才会推给客户端，别的模块不会替你打开：

```bash
"$ROOST" add transport tcp                 # 生成玩家 TCP 接入与 auth.go；roost.yaml 的 access.player.transports 多出 tcp
```

在 `roost.yaml` 的 `access.player` 下手工加一行 `self_sync: true`（与 `transports` 同级），然后：

```bash
"$ROOST" project sync
"$ROOST" project doctor || true            # 应出现一行 OK self-sync
```

之后 Player 主 DAO 里所有标了 `sync` 的顶层字段（模块的与手写的）都推给玩家本人，不用写 packer；说明见 [PLAYER-ACCESS §7](../../../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)。真实客户端要连上还差两件，属于登录鉴权，不在这一档：实现 `internal/access/player/tcp/auth.go`（缺省拒绝一切），再 `"$ROOST" config enable player-tcp`；`"$ROOST" project doctor --workflow player-tcp` 列出剩下的项。

## 4B. 进入游戏：建档与首次初始化

**装 `enter` 模块即可**（`"$ROOST" module add enter`）：它带 `EnterGame` 端点和首次初始化，新玩家的初始发放写进它的 `game/entities/player/enter_first.go`（例如 `component.owner.WalletComp().Add(100)`）。手写的功能要在玩家进入时顺带做事（上线登记、拉离线期间的数据），不要改进入端点：在自己的包里写

```go
func init() {
	gameenter.OnEntered("<功能名>", func(ctx context.Context, registry *app.Registry, entered gameenter.Entered) error {
		// entered.PlayerID、entered.First（这次是否首次进入）、entered.FirstAtUnixMilli（首次进入的时间）
		return nil
	})
}
```

（`gameenter` 是 `<module>/game/enter`。）钩子在事务之外执行，失败只记日志、下次进入再调；必须成功的初始发放写在 `enter_first.go`，或在实体包用 `player.OnFirstEnter` 登记事务内钩子（配方已使用）。

**enter 有两条路**。会话是用进服票据握手的（第 3A 步的链路，装了 `login` 时）：玩家没建成之前 `EnterGame` 答 `enter_need_create`、什么都不写，客户端先发 `EnterCreate{name}`；建档、名字、首次初始化、“建成了”的标记在同一笔事务里。别的会话：`EnterGame` 加载或创建并做首次初始化。两条路的首次初始化是同一段代码，初始发放只写一处。要在“新建”这一步加自己的检查或写入，用 enter 的登记点（`OnCreating`、`OnCreatePrepare`、实体包的 `OnCreate`，见它的 README），不要改它的端点。

下面是不装它时的手写做法，也是它内部的做法。

Nest handler 的目标必须是已存在的实体。新玩家没有 Player，所以客户端登录后的第一条请求必须是一个“进入游戏”端点，由它加载或创建 Player；没有它，场景测试照样全绿（测试里直接造了实体），真实进程里每条玩法请求都会答 `entity_not_found`。`-template game` 已带 `game/lifecycle/player.go`，只差端点：

```bash
"$ROOST" add protocol EnterGame --group game --handler player
"$ROOST" generate
"$ROOST" add endpoint EnterGame --handler player --lifecycle Player
"$ROOST" generate
```

生成的 `game/controllers/player/enter_game.go` 调 lifecycle 的 `GetOrCreate`，不经过 Nest，是业务文件。

**新玩家的初始发放**（送金币、满体力）不要按“这次是不是新建的”判断：建档后、发放前进程崩了，这个玩家就永远拿不到。做法是：发放写成一个不对客户端开放的 Nest handler，“发过没有”记在自己的状态里（一个只落库的布尔字段），每次进入游戏在 `GetOrCreate` 之后都经生成的 Sender 调一次它，已发过就什么都不做。入口里这段手写串接没有离线测法（场景测试只到 Nest handler），在报告里列为未测。

## 4C. 日志

规则：**玩家相关的操作要有业务层日志；高频逻辑可以不打；日志只用框架的日志。**

- **用什么**：标准库的 `log/slog`（框架在进程启动时装好了它的输出：级别、JSON、落盘与轮转由服务配置的 `log:` 段决定，每行自动带协程号、帧号与业务时间），或者框架的 `infra/observe/log`（`ELog`，按实体 / 玩家带上标识的写法）。不用 `fmt.Print*`、标准库的 `log`，不引入别的日志库，不自己开文件写。
- **记什么**：改了玩家状态、以后要能回答“这名玩家什么时候做了什么”的操作——建角与进入、货币与道具的增减（数量、来源）、购买与发货、领奖、改名、关系变化（好友、队伍、联盟）、换服、运维命令对玩家的写入。每条带 `player_id` 与这件事的关键事实（道具号、数量、订单号、原因），用键值对，不把内容拼进消息文字。
- **在哪里记**：记在请求返回之后——端点（controller）里 Nest 请求成功返回时，或服务端调用生成的 Sender 返回之后；这时事务已经提交。handler 里打的日志在事务回滚时也会留下，所以 handler 里只在需要记“为什么拒绝 / 出了什么错”时打，并写明是拒绝。
- **级别**：成功的玩家操作用 `Info`；被业务规则正常拒绝的（余额不足、次数用完）不用打，客户端已经拿到码；不该发生的（数据对不上、依赖给不出结论）用 `Warn` / `Error`。
- **不记的**：高频的逻辑——移动、心跳、每帧 / 每次同步的推进、AOI、定时器的每次触发、只读查询。这些要观测用指标（[可观测性](../../../framework/guide/11-observability.md)），不用日志。
- **不能记的**：票据、令牌、凭据、密钥，哪怕是 `Debug`；只记长度或是否存在。
- 现有模块的手写端点与服务编排已有提交后日志；直接 Sender、定时器内状态切换等边界见[日志覆盖报告](../../../maintenance/evidence/module-logging-20261112/README.md)。工程新增调用仍须在提交确认后记日志，不为日志修改 handler 或生成器；幂等确认日志不能当成新增资产流水。

## 5. 验收

```bash
"$ROOST" module verify                                            # 已装模块 + 工程级检查
go test -mod=mod -race -count=1 -v -run '^Test(<FeatureA>|<FeatureB>)' ./game/handler 2>&1 | tee "$WORK/feature.log"
grep -c '^--- PASS' "$WORK/feature.log" || true                   # 自己数：等于你写的场景数
grep -c '^--- SKIP' "$WORK/feature.log" || true                   # 必须为 0
go test -mod=mod ./...                                            # 其余生成代码自带的测试
"$ROOST" project doctor || true
```

- `module verify` 全部 `ok`、退出码 0。它已经包含 `go build`、`go vet`、`roost id check`、`roost generate --check`。
- 手写功能的场景数自己核对：`-run` 写错或用例被跳过时 `go test` 照样报 `ok`。
- `project doctor` 在 `-template game` 的新工程上必然退出 1：托管服务的 collaborators 是拒绝桩，上线前由项目实现。没装 login 时是 account / chat / platform / session 四项；装了 login 后 account 由它接管，剩 chat / platform / session 三项。除这些外不应有 FAIL。
- 这一档是离线验收：替身提交器，没有 Mongo / NATS / Redis，没有真实客户端。要真起进程见 [SKILL §4](../SKILL.md)。

## 5A. 工程说明文档

每个生成的工程都要带 `docs/DEMO.zh-CN.md` 与 `docs/DEMO-HISTORY.zh-CN.md`，业余与专业模式都要生成。前者写给接手这个工程的人（服务器与客户端），描述**当前状态**；后者是逐次变更的记录，也是下一次增改需求时 agent 的记忆（第 2A 步）。

`docs/DEMO.zh-CN.md` 的内容从工程里取，不凭记忆写；改了工程就同步改它。固定这几节：

1. **需求对照**：第 6 步那张表（每条需求：服务器部分、客户端部分、结论、模块或样板、状态），加上冲突与待确认的数值。开头写明本次用的模式。
2. **服务器 / 客户端边界**：逐个逻辑点写放在哪一端、谁定的（业余模式是“按缺省规则”，专业模式是“开发者决定”或“开发者确认了建议”）、放在客户端的风险与服务器还做了哪些校验。
3. **模块**：装了哪些（含连带装上的），每个一句话说它在本工程里管什么、通过的场景数；装上了哪些联动（`with`）；模块自带而需求没用到的客户端协议。来源：`roost.yaml` 的 `modules` 段、`"$ROOST" module verify` 的输出。
4. **实体**：每种实体（Player、World、模块自带的 Room / Alliance 等）一节：谁创建、挂在哪个服务上、主 DAO 的顶层字段（字段名、来自哪个模块或手写、`persist` / `sync`）、组件与组件号、持久定时器类型。来源：`db/def/*.go`、`game/entities/`、`roost.yaml` 的 `entities` 与 `ids`。
5. **服务**：`roost.yaml` 的 `services` 段逐个列：业务服务还是托管的框架服务（account、chat 等）、带了哪些 Mod（web、directory、heartbeat……）、对外的端口与入口（玩家 TCP、HTTP 路由）、哪些模块的逻辑跑在它上面、服务之间的 RPC / Saga。开发启动顺序（`make dev-run`）与依赖（Mongo / NATS / Redis / etcd）。
6. **协议与错误码**：客户端请求、服务器推送各一张表（名字、号、谁处理、要点），错误码表（名字、号、什么时候出现）。来源：`protocol/def/`、`internal/errors/`、`roost.yaml` 的 `ids`。
7. **客户端对接**：登录到进入的完整顺序（第 3A 步）；每个功能的调用顺序；要解码的自身视图字段及含义（哪些字段要配合服务器时间判断“今天”）；每个错误码的提示建议；请求键怎么生成、什么时候重发。
8. **手写的功能**：文件清单、样板模块与借鉴点、业务假设（数值、日界、上限）、日志打在哪。
9. **验收与上线前缺口**：第 5 步的实际结果、离线验收的范围，上线前要补的（渠道验签、TLS、限流、生产地址、托管服务的 collaborators）。

`docs/DEMO-HISTORY.zh-CN.md` 每一轮追加一节，第一次生成工程也算一轮：

- 日期、模式（业余 / 专业）、用户的原话（照抄）；
- 这一轮新增、修改、删除了哪些条目（按 DEMO 文档里的编号）；
- 定下的决定：专业模式下开发者的每个回答（拆分、边界、数值、复用、归属），业余模式下 skill 替用户定的项；
- 冲突与用户的选择；仍待确认的项；
- 改了哪些文件、新分配或弃用的号与存储键；
- 验收结果（新场景与回归场景的数量）；
- 照 skill 做时走不通、自己绕过的地方（只记在这里并告诉用户，不改 skill）。

## 6. 向用户报告

先给一张需求对照表，每条需求一行：服务器部分与客户端部分（第 1 步“再分端”）、结论（直接装 / 装了再扩展 / 借鉴 / 手写 / 做不了）、用到的模块或样板、借鉴的点、状态（已做 / 等用户决定 / 没做）。报告里给出第 5A 步说明文档的路径；报告与文档的内容要一致。

再给限制检查的结果：发现的每个冲突（需求原话、冲突的限制与出处、用户选了哪个做法）；没有冲突写“已对照模块能力边界、能力清单与已知限制，未发现冲突”。**被你发现而用户还没表态的冲突不能出现在“已做”里。**

然后逐条对照需求，分四栏：

1. **装了哪些模块**：名字、各自通过的场景数、追加的 DAO 字段、分配的协议号与错误码（`roost.yaml` 的 `modules` 段有）。
2. **手写了哪些功能**：文件清单、场景数、分配到的号（手写功能的号不在 `roost.yaml` 里：组件号看 `//roost:component`，错误码看 `errcode.Define`，协议号看 `//roost:msg`）、你做的业务假设（数值、日界、上限都是样例）。
3. **没做的**：框架不支持或只有设计的，给出处；需要用户决定的。
4. **验收到哪一步**：`module verify` 与手写场景的实际输出；明确写“离线、替身提交器，未连真实依赖、未经真实客户端、没有 Robot 与网络验收；端点层（错误转成回包）只有生成器自己的用例”。

另外写明：**日志**——哪些玩家操作打了日志、在哪个文件；哪些按“高频”没打。**登录链路**——用的是第 3A 步的现成链路还是别的，开发配置写了什么，上线前还差什么（渠道验签、TLS、限流）。

临时目录用完即删，检查没有遗留的测试进程。

## 7. 边界

- 模块清单以 `roost module list` 为准；实体和依赖归属见各自清单，chat / leaderboard / mailbox 分别要求业务服务 `uses: [chat]` / `[rank]` / `[mail]`（`-template game` 都已有）。模块之间只自动接上 `with` 声明的联动；其余要自己写：升级奖励（`OnProfileLevelUp`）、新玩家初始发放（`enter_first.go`）、自定义任务事件（调 `TaskComp().Report`，指定任务仍可 `Advance`）、何时上榜（调 `leaderboard.Submit`）、什么操作扣体力（调 `StaminaComp().Spend`）。装了 shop 要把样例商品目录换成自己的。13 类玩法与 game-demo 的其余业务还不是完整的模块组合，它们的做法可以参考，剩余玩法要手写进工程。
- 没有卸载；分配过的号与存储键不回收。已装模块不会随 roost-core 升级自动更新。
- 模块已支持 `runtime` 服务 Init / Shutdown 接线；按实体分配持久定时器类型已由 turn 使用；模块自带压测步骤仍未提供。
- 细节与出处：[modules/README](../../../../modules/README.md)、[命令参考“业务模块”](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module)、[KNOWN-LIMITS“业务模块”](../../../maintenance/KNOWN-LIMITS.md#业务模块roost-module)。
- 模块的 README 写的是模块自己的能力与边界；几个模块组合之后的行为（联动没有覆盖的那些）没有人替你验证过，要自己写场景。
- 独占新服务的模块（alliance）与带 Saga 的模块只在新生成或结构相同的工程上验证过；已有工程的服务名、Mod 组合不同时先 `module plan` 看它要动什么。
- 登录链路在单机真实进程上演练过（嵌入 TCP 与独立 Gate 各一次，见 CHANGELOG）；多个 Gate、account 多副本、跨机没有验证。
