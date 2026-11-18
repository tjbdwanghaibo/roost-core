# 按需求组合：用 roost-core 生成或扩展自己的服务端工程

适用：用户给的是一段需求（“做一个有货币、签到和每日任务的服务”），或者要在已有的 roost 工程上继续加功能、改需求。照某类玩法出 demo 用主 [SKILL](../SKILL.md) §2、§3。被别的 agent 调用时，先读主 SKILL 的[与其他 agent 对接](../SKILL.md#1-与其他-agent-对接)。

Markdown 链接相对本文件所在目录；反引号里的路径（`modules/<name>/README.md` 等）相对 roost-core 根目录，取发布版时就是模块缓存里的 `$CORE`。本文只写当前版本的事实；目标 roost-core 版本早于本 skill 所在版本时，读那个 tag 里的 skill。

## 0. 总览与规则

### 步骤总览

| 步 | 做什么 | 完成标准 | 验证命令 |
| --- | --- | --- | --- |
| 1 | 问清需求：开场五个问题、定模式 | 五个问题都有答案，或记成“按默认” | 无；答案写进 HISTORY |
| 2 | 判断要不要 roost | 结论三选一（需要 / 不需要 / 一部分需要） | 无 |
| 3 | 实时战斗分档 | 每条战斗需求都有档位 | 无 |
| 4 | 拆需求、分端、定复用、查限制 | 需求对照表每行有结论；冲突逐条列出 | `"$ROOST" module plan <模块…>` 不报错 |
| 5–6 | 定接入方式；开房间时走 lobby + relay；专业模式出决策单 | 接入结论写下；专业模式的决策单已确认 | 无 |
| 7 | 建工程 | 工程目录生成 | `"$ROOST" project doctor` 没有 FAIL 行 |
| 8 | 装模块与登录 | 模块装上、登录链路配好 | `"$ROOST" module verify` 每行 `ok`、退出码 0 |
| 9 | 手写模块库里没有的功能 | 场景测试写好，场景数先记进 DEMO 第 8 节 | `go test … -run` 的 PASS 数等于 DEMO 记的数 |
| 10 | 验收 | 全部通过 | `module verify`、`go test ./...`、`project doctor` |
| 11 | 起服冒烟 | 能登录、能握手；做不了时写明原因 | curl 输出含 `"ok":true`；playerprobe 输出 `authentication succeeded` |
| 12 | 文档 | DEMO、CLIENT、HISTORY（专业模式还有决策单）写好，`shared/README.md` 与 CLIENT 相同 | `cmp docs/CLIENT.zh-CN.md shared/README.md` |
| 13 | 报告 | 开头三行白话 + 交付摘要块 | 无 |

已有工程上增改需求时，按第 14 节走一轮，不从头重新生成。

### 四条规则（每一步都受它们约束）

1. **遵循框架的核心编程思路**（[附录 A1](#a1-框架的核心编程思路)）。怎么改状态、怎么落盘、怎么让客户端看到，框架已经定了做法，不另起一套。
2. **模块里有的逻辑，复用**。每条需求先过一遍“有没有模块已经做了”（第 4 步）。有就装模块、调它的入口、接它的登记点，不重写。
3. **不能复用，就借鉴**。没有模块覆盖时，指定一个最接近的模块当样板，照它的结构、登记点与测试写（第 9 步）。
4. **需求与限制冲突，必须提出**。查出来的冲突写清楚、给选项（第 4 步末尾），不许悄悄跳过、降级或绕开。

另有一条贯穿手写的规则：玩家相关的操作要有业务日志，高频逻辑可以不打，只用框架的日志（[附录 A5](#a5-日志)）。

### 只读与记忆

- **skill 对使用它的 agent 只读**：不改 `docs/agent-skills/roost-genre/` 与装到工程里的入口副本，不改 roost-core 的模块模板与框架代码。照做走不通时告诉用户：哪份文档、哪一节、原文、实际情况、你怎么绕过的；绕过的办法只记进工程的 `docs/DEMO-HISTORY.zh-CN.md`。
- **记忆放在工程里**：`docs/DEMO.zh-CN.md` 是当前状态，`docs/DEMO-HISTORY.zh-CN.md` 是逐次变更（只追加）。下一轮增改需求先读它们。agent 自带的记忆可以另记开发者的偏好，两边不一致时以工程为准。

### 命令约定

命令按发布版写，变量来自入口 skill 的第 0～2 步：

```bash
export GOWORK=off
V=<roost-core 版本，如 v1.56.0>
WORK=<放 CLI 与工程的目录>
NAME=mygame; MOD=example.com/mygame          # 工程名与自己的 Go module
GOBIN="$WORK/bin" go install "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@$V"
ROOST="$WORK/bin/roost"; DIR="$WORK/$NAME"
```

CLI、工程依赖与 skill 正文必须是同一个 `$V`。维护者用 roost-core 本地检出时的写法见[附录 C](#附录-c维护者用本地检出时)。

## 1. 问清需求

**开场一次问完**，用白话，不用框架术语：

1. 客户端用什么做（Unity、Unreal、Godot、网页、别的），在哪些平台发行？客户端工程放在哪个目录（还没有就说没有）？
2. 一个服大概多少人同时在线？一局或一个房间几个人？
3. 玩家离开游戏以后，有没有要保存、不能让玩家自己改的东西（货币、背包、养成、排行、充值）？
4. 有没有实时战斗？是什么样的（回合制、锁定目标打怪、射击或动作、两队对战）？
5. 你想怎么参与：A. 让 AI 先做完，事后再改（业余模式，默认）；B. 拆完需求后逐项确认再写代码（专业模式）；C. 只给我一份决策清单，我改完再做（只给决策）。

需求原文里已经回答了的不再问。用户不答、答得不明确，或者没有人可问（被别的 agent 调用、无人值守）时：全部按默认，模式是业余，平台与人数按需求原文推断，推断不出的写进“待确认”。已有工程上增改需求时，沿用 `docs/DEMO-HISTORY.zh-CN.md` 最近一次记录的模式，开头说一句“沿用上次的××模式”。

**三种模式**走同样的步骤、守同样的规则，交付物也一样（工程、第 12 步的文档、第 13 步的报告）。区别只在第 4～6 步的结论由谁拍板：

| | 业余模式（默认） | 专业模式 | 只给决策 |
| --- | --- | --- | --- |
| 拆分、边界、数值、复用、归属 | AI 按缺省规则定，事后列在报告里 | 写进决策单，开发者确认后才写代码 | 只写决策单，不建工程、不写代码 |
| 缺的数值 | 用样例值，记成“待确认” | 决策单里给建议值，开发者改 | 同专业模式 |
| 需求与限制的冲突 | **照样必须提出**：合成一个问题，每条一两句白话，标出推荐做法 | 进决策单，每条写四要素（第 4 步） | 同专业模式 |
| 措辞 | 少用术语，必须用时顺手解释 | 可以直接用术语 | 只列决策与出处，不解释 |

用户中途换模式，从当前步骤起按新模式做，做完的不返工。

**先对照[客户端表](#客户端能连什么按客户端类型)**：客户端引擎与发行平台决定它能连哪条路。网页、Unity WebGL、Unreal / C++、Godot 尤其要先查，表里写“不支持”的组合在第 4 步作为冲突提出，不要默认客户端能连上。

**客户端是 Unity 或别的 C# 工程时**，缺省打开导出目录（第 7 步写 `client_export`），协议和配置表都导出，客户端照它编译。导出 C# 协议要 **protoc 31.1**（与 C# SDK 锁定的 Google.Protobuf 3.31.1 同版本，版本对不上 `roost generate` 报错并给出下载地址）：

- 从 protobuf 官方 GitHub release `v31.1` 下载对应平台的压缩包（`protoc-31.1-osx-aarch_64.zip`、`protoc-31.1-osx-x86_64.zip`、`protoc-31.1-linux-x86_64.zip`、`protoc-31.1-linux-aarch_64.zip`），解压到工程外的工具目录。下载前告诉用户文件名与来源。Homebrew、apt 装的 `protobuf` 通常是更新的版本，用不了。
- 指过去：`export ROOST_PROTOC=<解压目录>/bin/protoc`，或在 `roost.yaml` 写 `client_export.protoc: <路径>`（顶层，C# 与 TS 共用；相对路径按工程根解析）。查找顺序是 `client_export.protoc`、`ROOST_PROTOC`、`PATH`。导出 TS（`languages` 里有 `ts`）时，服务端开发机还要 node 22 以上与 protoc-gen-es 2.17.0。
- 核对：`"$ROOST_PROTOC" --version` 输出 `libprotoc 31.1`。

**前置条件**：Go（版本按 roost-core 的 `go.mod`）、git、curl、python3、perl（按玩法出 demo 的辅助函数用）；起服冒烟（第 11 步）还要 docker（`make dev-up` 起 Redis、Mongo、NATS、etcd）。缺了哪样就在报告里写哪一步做不了。

**完成标准**：五个问题的答案（或“按默认”）与模式写进这一轮的 HISTORY 草稿。

## 2. 判断要不要 roost

roost-core 是**专用服务器**框架：服务器权威、持久化、跨进程、运维一整套。很多联机需求用不上它，硬套只会多出一堆要部署、要花钱的服务器。拆需求之前先判断，结论写进 DEMO 第 1 节与报告。

**倾向于用 roost 的信号**（有一条成立就值得认真考虑）：

- 有要长期保存、玩家不能自己改的状态：账号、货币、背包、养成、排行、付费到账；
- 要防作弊：结果必须由服务器判定；
- 玩家离开之后世界还在跑：全服活动、公会、SLG 地图、离线收益、定时结算；
- 人多：一个服几百上千人、跨房间的社交（聊天、好友、组队、匹配）；
- 运营需求：GM、邮件、公告、兑换码、充值、换服、合服；
- 跨平台，或不想依赖某个平台的账号体系。

**倾向于不用 roost 的信号**（实时战斗不算，战斗按第 3 步分档）：

- 一局一局的联机，局外没有要保存的东西，或平台自带的云存档、成就、统计就够；
- 一个房间几个人，由某个玩家的机器当主机（host）完全够用；
- 只在一个平台上发行，可以直接用平台的大厅、邀请与中继；
- 朋友之间的合作玩法，对作弊不敏感。

**小房间、房主做主机，但不想绑定平台**（多平台、浏览器、自己的账号）：roost 有现成的大厅 lobby 与转发 relay，不需要 game 服务，见第 6 步。它不替你防作弊、不判定局内结果。网页与 Unity WebGL 客户端连不连得上 game 先查[客户端表](#客户端能连什么按客户端类型)，连不上时这是它们联机的路。

**典型例子**：只在 Steam 上发行、要联机开房间、人数少——建议 Steam Lobby（建房、邀请）+ Netcode for GameObjects（房主当主机）+ Steam 传输层（P2P 与中继），不用 roost。别的引擎换成对应的联机层，思路相同。通用方案与交付的方案文档见 [steam-lobby-ngo.md](steam-lobby-ngo.md)。

| 结论 | 怎么做 |
| --- | --- |
| 需要 roost | 往下走 |
| 不需要 roost | 停下，说明理由（对照信号逐条说）与建议的方案，问用户是否仍要用 roost。用户坚持就照常做，文档写明“用户在已知替代方案的情况下选择 roost”。没人可问时按推荐做：不建工程，交 steam-lobby-ngo.md 第 9 节的方案文档，“是否仍要 roost”放进摘要的“待决定” |
| 只有一部分需要 | 例如局内用平台方案，局外的账号、货币、养成用 roost。说明分工与两边怎么对接（局内结果怎么交给服务器、服务器怎么校验），问用户是否这样拆 |

判断是建议，不是框架的限制，用户的选择优先。

**完成标准**：结论、理由与用户的选择（或“按推荐，待决定”）写进 HISTORY 草稿；专业模式记进决策单。

## 3. 实时战斗先分档

有战斗时按下表判断。**不要笼统地说“实时战斗不适合”，也不要因为模块表里有 `combat` 就当成多人战斗现成可用。**

先弄清：判定方式（锁定、读条、范围，还是非指向弹道、精确碰撞）；节奏（判定每秒几次才不影响手感）；规模（同场景多少玩家、怪物、在动的单位）；对抗（PvE 为主还是 PvP，对公平和延迟多敏感）；权威（必须服务器判定，还是可以客户端或房主算）。业余模式从需求推断，推断不出的按样例假设并写进“待确认”。

| 档位 | 典型 | 结论 | 怎么做 |
| --- | --- | --- | --- |
| 1. 回合 / 异步 | 回合制、卡牌、放置、自动战斗结算 | 支持 | 按对应玩法的 reference |
| 2. MMO 级服务器权威实时战斗 | MMORPG、ARPG 副本、刷怪、组队打 Boss、锁定或范围技能、Buff 与冷却 | 支持（[mmorpg.md](mmorpg.md)） | 见下文 |
| 3. 帧同步对战 | MOBA、格斗、少人数竞技 PvP（逻辑在客户端确定性执行） | 支持 | [moba-lockstep.md](moba-lockstep.md)；服务器只做输入转发、追帧与快照恢复 |
| 4. 动作 / 射击级 | FPS、精确命中、高于 20 次 / 秒的判定、命中回溯、客户端预测与回滚 | 不支持 | 作为冲突提出。可选：改成第 3 档；战斗放客户端或房主（[steam-lobby-ngo.md](steam-lobby-ngo.md) 一类），roost 只管局外；或换专门的动作服务器 |
| 5. 大规模同屏状态同步 | RTS、状态同步 MOBA、数百单位同屏团战 | 暂缓（没有纵切与容量数据） | 作为冲突提出，先问规模；降到第 2 档能承受的范围才按第 2 档做 |

一个需求跨档时分开判断，例如 MMO 主体第 2 档，里面要求公平的竞技场单独走第 3 档。

**第 2 档怎么做**（最容易判断错）：

- **框架给的**：Player 与场景实体、Nest 事务；实体心跳最短 50ms（每秒 20 次）；移动拍 200ms（`actor`）；技能运行时 `gameplay/skill`（[技能](../../../framework/guide/08-skill.md)）；行为与 AI（`roost add behavior`）、持久定时器（Buff 到期、复活）；每张地图一个 Interest 做视野（AOI）；组队、副本、掉落与任务。
- **现成模块覆盖到哪**：`actor`（移动、定身沉默）、`scene`（多图、切图、视野）、`monster`（巡逻、追击、死亡重生）、`skillbook`（技能目录）、`attribute` / `equipment`、`dungeon`、`team`。**没有现成的多人战斗**：`combat` 只是双人 PvP 会话（不支持怪物参战、第三人、范围选择、召唤物、投射物）；`monster` 自己不战斗（伤害由调用方算好传进来，没有仇恨表）；`dungeon` 信任调用方报告的击杀。
- **所以多人战斗、打怪、范围技能要手写**：结论是“借鉴”，样板是 MMORPG 参考（`scripts/testdata/mmorpg/` 与 game-demo 的战斗、怪物、Buff），事务写法借鉴 `combat`；技能效果走 `gameplay/skill`。伤害结算在多实体 Nest 事务里重新检查场景、距离与怪物的 `LifeSeq`。
- **设计上要配合的边界**（写进 DEMO 第 2 节）：没有延迟补偿与命中回溯，非指向技能在高延迟下会“看着躲开了仍被打中”，用前摇、预警圈、锁定或范围判定来吸收；客户端负责插值、动作与特效预表现，命中与数值以服务器为准；一个场景归一个进程，同屏规模按 [mmorpg.md](mmorpg.md) 的视野容量一行估算，超出时提出来。
- **验收**：手写的战斗按第 9 步写场景；同屏规模接近已有数据时，建议用户按自己的密度压测，写进报告的“没做的”。

**完成标准**：每条战斗需求的档位、依据与用户的选择写进 HISTORY 草稿；专业模式记进决策单。

## 4. 拆需求、分端、定复用、查限制

**先拆**：一条一件事（“每天能签到一次”“签到给金币”“连续签到有加成”）。数值、周期、上限写进条目；用户没给的记成待确认（业余模式用样例值）。

**再分端**：每条拆成服务器部分与客户端部分。

| 归服务器 | 归客户端 |
| --- | --- |
| 决定结果的一切：校验、扣与发、随机、次数与上限、时间与日界、排名、谁能对谁做什么 | 界面、动画、输入；按已同步的状态展示（余额、进度、“今天领过没有”）；把玩家的意图发成请求 |
| 状态与持久化、跨玩家与跨服务的协作、给客户端看哪些字段 | 收推送后的提示；请求的重发（带同一个请求键）；按错误码给提示 |

缺省规则：**影响结果、资产或公平的判断只在服务器做**，客户端的判断只用于提前提示；客户端不传时间、不传玩家号、不传随机结果。只有客户端部分的条目（纯展示、本地设置）标“客户端，服务器不涉及”。专业模式下边界由开发者逐条定：开发者把影响结果的逻辑放到客户端时照做，服务器只做开发者要求的校验，DEMO 里标“开发者决定”并写明风险。框架强制的不能挪：身份取自会话、业务时间取服务器、状态只经 Nest handler 改；要挪这些按冲突提出。

**数值放哪**：奖励、曲线、上限、倍率、掉落、价格、冷却默认写进配置表 `configs/table`，不写成 Go 常量（[附录 A4](#a4-配置表数值写进-configstable两端共用一份)）。同时列出哪些表两端共用、哪些字段只给服务端（掉落种子、权重、内部阈值）、哪些表只给客户端（文案、模型）。

本 skill 只生成服务器。客户端部分交付对接文档 `docs/CLIENT.zh-CN.md`（第 12 步）与导出目录 `shared/`。

**再逐条定复用**（规则 2、3）。查下面的模块速查表，再读候选模块的 `modules/<name>/README.md`：“能力”看它做不做这件事，“能力边界”看做到什么程度，“复用方式”看怎么用。表只是索引，以 `"$ROOST" module list` 与各 README 为准；“连带”是安装器会一起装的依赖。

| 需求 | 模块 | 连带 | 一句话 |
| --- | --- | --- | --- |
| 进入游戏、建档、首次发放 | `enter` | — | `EnterGame` 加载或创建 Player、首次初始化（实体事件 `event.EnterFirst`）、“先新建再进入”、事务外的进入登记点；打开玩家 TCP 与自身视图 |
| 握手鉴权 | `login` | account_auth | 握手票据交给 account 校验；区服登记工具 `cmd/accountctl`；开发登录与渠道入口在 account_auth |
| 客户端登录入口、选服 | `portal` | account_auth | 四个 HTTP 端点：登录（可顺带进服）、区服清单与推荐、换进服票据、换游戏账号 token；挂在托管 account 的进程上 |
| 服务器时间 | `servertime` | — | 业务时间、当前业务日、下一次每日重置；进入时推送 |
| 客户端规则下发 | `rules` | enter | 各模块登记的规则快照，进入时推送、可补取 |
| 档案：名字、等级、经验 | `profile` | enter | 改名、发经验、升级事件 `event.ProfileLevelUp` |
| 全服唯一名字 | `naming` | profile | 两阶段占名、改名状态机、旧名释放 |
| 货币 | `wallet` | — | 余额、按业务键幂等的加减、消费协议 |
| 背包、道具 | `bag` | — | 道具表、堆叠上限、发放与丢弃 |
| 装备 | `equipment` | bag | 槽位、穿卸与背包原子扣回 |
| 属性 | `attribute` | profile | 基础 / 装备 / 最终属性分层，随等级成长 |
| 每日签到 | `checkin` | wallet | 业务日切分、连续天数加成 |
| 任务 | `task` | wallet | 事件推进；每日 / 每周 / 永久、前置链 |
| 体力 | `stamina` | wallet、rules | 惰性恢复、消耗、花金币补满 |
| 商店 | `shop` | wallet、bag、rules | 金币买道具、限购、请求键防重 |
| 抽卡 | `gacha` | wallet、bag | 权重与保底、单抽与十连；组件有不扣费的 `DrawFree` |
| 离线收益 | `idle` | wallet | 按间隔惰性结算、时间与金额双上限 |
| 兑换码 | `cdkey` | mailbox | 一码一次或一人一次 |
| 充值发货 | `purchase` | wallet | platform 记单、顺序发货、恰好一次 |
| 邮件 | `mailbox` | wallet | 列表、领取附件、发件入口、全服邮件 |
| 聊天 | `chat` | — | 世界频道、私聊、历史、系统公告 |
| 公告、跑马灯 | `notice` | chat | 可信服务端入口与 GM 命令 |
| 好友 | `friends` | — | 申请、同意、删除、黑名单（同服，双边动作要求双方在本进程在线） |
| 排行榜 | `leaderboard` | — | 前 N 名与自己的名次；分数只升不降 |
| 队伍 | `team` | facebook | 建队、邀请、入队、离队、队长交接 |
| 匹配 | `matchmaking` | team、matchroom | 本服单人或整队匹配，成局移交房间 |
| 匹配房间 | `matchroom` | room | MatchID 到房间的幂等移交、固定名单 |
| 房间 | `room` | — | 有限座位、进出、房主交接、重连 |
| 回合 | `turn` | room | 行动者、命令序号与重放、超时推进 |
| 联盟 / 公会 | `alliance` | bag | 独立联盟服务、成员、捐赠领奖、三个 Saga |
| 活动 | `activity`、`activity_world`、`activity_alliance` | activity（alliance） | 五阶段活动、循环、个人状态与定时器；全服 / 联盟的权威宿主 |
| 移动 | `actor` | — | 心跳、位置朝向、路径与跟随 |
| 场景与视野 | `scene` | actor | 多图分线、切换、视野与公开字段同步 |
| 怪物 | `monster` | actor、scene | 巡逻、追击、死亡与重生 |
| 技能目录 | `skillbook` | — | 启动时编译技能定义，客户端读同一份 |
| 战斗（双人 PvP） | `combat` | actor、attribute、skillbook | 技能运行时、Buff、冷却；**只限两名玩家**（第 3 步） |
| 副本 | `dungeon` | profile | 阶段事件、可信开局与推进、恰好一次奖励 |
| 可编辑地形 | `terrain` | scene | 地形块、笔刷、建筑占地 |
| 固定分区大世界 | `cellworld` | — | 多个进程各管一块地图、玩家跨格移动与战斗；容量目标没有通过，见主 SKILL §6 |
| 换服 | `rehome` | — | 把一名玩家改绑到另一个 game 服 |
| 运维命令 | `gm` | — | 命令登记与审计 |


给每条一个结论：

| 结论 | 条件 | 怎么办 |
| --- | --- | --- |
| 直接装 | 模块的能力覆盖这条需求，边界不冲突 | 第 8 步安装 |
| 装了再扩展 | 模块覆盖大部分，差的那点落在它留给工程的文件、它发布的实体事件或登记点上 | 第 8 步安装，再按 README 的“复用方式”改、监听（`//roost:listen`）或登记；不改它别的文件 |
| 借鉴 | 没有模块覆盖，但有形状接近的模块 | 第 9 步手写，写明样板与借鉴点 |
| 手写 | 没有接近的模块，框架有对应能力 | 第 9 步手写，先读[能力清单](../../../framework/CAPABILITIES.md)“文档”列里的契约 |
| 做不了 | 只有设计 / 原型 / 不支持 | 告诉用户，给出处，不做 |

- 不要因为“模块多带了点东西”就改成手写：模块自带的客户端协议用不上时告诉用户，由用户决定去留。也不要复制模块代码另改一份。
- **要和已装模块联动时先找现成的事件**：看各模块的 `game/event/<模块>.go`（README 的“实体事件”一节），在自己的组件上写 `//roost:listen` 方法；不改发布方，不另写登记表。
- 13 类玩法本身不是可安装的模块（没有 `roost module add slg` 这种命令）。每类的夹具是“装几个现成模块 + 手写模块没覆盖的部分”，装了哪些写在 `scripts/test-<玩法>-generated.sh` 的 `genre_modules` 里，选模块时可以照着看；夹具与 game-demo 不能直接装进别的工程。

**最后查限制**（规则 4）。每条需求连同它的结论，对照三处：候选模块 README 的“能力边界”、[能力清单“明确没有 / 不适合的”](../../../framework/CAPABILITIES.md#明确没有--不适合的)、[已知限制](../../../maintenance/KNOWN-LIMITS.md)；再看[附录 B](#附录-b边界速查)。这些算冲突：

- 要的量、频率、一致性超出写明的范围（例如要“永久防重”，而货币的去重窗口只留最近 128 次）；
- 要的行为模块明确不做（例如要全服唯一的名字，而档案模块不查重）；
- 依赖的能力在能力清单或已知限制里标成只有原型、只有设计、只有离线验证，或写明“尚未确认”。模块场景用替身提交器这件事不算冲突，统一写进报告的“验收到哪一步”；
- 用户的客户端在[客户端表](#客户端能连什么按客户端类型)里对这条路写着“不支持”（例如网页客户端要连 game）；
- 两条需求各自可行、放在一起互相矛盾。

每个冲突写四样：

```text
需求条目：<原话>
冲突的限制：<哪份文档的哪一节，原文一句>
影响：<照做会怎样；不做会缺什么>
可选做法：A. 改需求为……  B. 换成……（代价是……）  C. 接受这个限制，按……交付（推荐：X）
```

有冲突就停下来等用户选（别的条目可以先做）。没人可问时按推荐做法做，把这条放进摘要的“待决定”。没有冲突也要在报告里写“已对照模块能力边界、能力清单与已知限制，未发现冲突”。

**完成标准**：需求对照表每行有服务器部分、客户端部分、结论、模块或样板；`"$ROOST" module plan <要装的模块…>` 不报错（在第 7 步建好工程后跑，或在临时工程里试）；冲突逐条列出。

## 5. 定接入方式：嵌入 TCP 还是独立 Gate

玩家协议是 RS v2 帧，走 TCP 或 WebSocket；HTTP 只用于登录入口（portal）、大厅（lobby）与管理面，不是玩家协议。这里的“短连接 / 长连接”指交互方式，不是传输协议。开房间、房主做 host 的走第 6 步。

| 需求 | 接入方式 | 做法 |
| --- | --- | --- |
| 按请求交互、很少推送、不需要长期在线；或开房间、一局打完就散（卡牌、回合、休闲、房间合作） | **嵌入 TCP**（缺省） | `project new -template game` 缺省就是这样，玩家 TCP 嵌在 game 里 |
| 长期在线、持续推送（同屏 / 大地图、实时同步、频繁世界广播：MMO、SLG 大地图） | **独立 Gate** | 装完登录链路（第 8 步）后 `"$ROOST" add gate`：生成 Gate 服务、game 一侧的接线与配置（关掉 game 的嵌入 TCP），Gate 接管原来给客户端的端口；之后 `"$ROOST" project doctor` 的 `gate:*` 检查这套接法。参数以 `"$ROOST" help gate` 与 [GATEWAY §2](../../../framework/GATEWAY.md#2-正式接入方式) 为准 |

- 业余模式按上表定；专业模式进决策单。
- 不要用 `project new -services game,gate` 造 Gate：那只会多一个名叫 gate 的普通业务服务。
- 独立 Gate 只做过单机真实进程演练，**没有跨机验证**，多机部署前要自己补验收。
- **服务端的 WebSocket 接入**：在选定的接入上再 `"$ROOST" add transport websocket`（要求已有 tcp）。命令不改配置：在 `player_access.tcp.websocket`（独立 Gate 是 `player_access.gate.websocket`）写 `addr: 0.0.0.0:7080` 与 `allowed_origins`（游戏页面的域名）才开，路径缺省 `/ws`；**白名单为空时所有浏览器页面都被拒绝**；wss 由前置负载均衡终结；七层负载均衡后面要写 `trusted_proxies`（负载均衡自己的地址，逗号分隔），否则全部连接按负载均衡一个 IP 计 `max_connections_per_ip`。契约见 [GATEWAY §6](../../../framework/GATEWAY.md#6-websocket-接入)。
- **服务端开了 WebSocket 不等于客户端能用**：先查下面的客户端表。表里这位客户端连 game 写着“不支持”时，作为冲突提出（第 4 步），可选做法：改成表里能连的客户端或平台；开房间、房主做主机的玩法改走第 6 步的 lobby + relay；客户端自己实现 RS v2 帧、鉴权首包、心跳与 Entity Sync 解码（工作量大，没有现成代码可抄）。
- portal 按客户端请求里的 `transport` 给地址：`tcp`（缺省）是玩家 TCP（或 Gate）的 `host:port`，`ws` 是完整的 `ws://` / `wss://` URL；清单每个服带 `transports`，推荐只在支持所请求方式的服里挑，指定的服没有该方式的地址答 `portal_transport_unavailable`（不回落到 TCP）。WebSocket 地址写在 account 配置的 `portal.websocket_servers`（服号 -> URL），或 game 配置的 `login.presence.ws_addr`；嵌入 TCP 开着 WebSocket 监听时可以不写，自动取 `ws://<login.presence.addr 的主机>:<监听端口>/ws`。前面有终结 TLS 的负载均衡或用独立 Gate 时，一定要自己写 `wss://` 地址，推导出来的不对。

### 客户端能连什么（按客户端类型）

本表是 skill 里客户端一侧能力的唯一说明，别处只引用它。DEMO 第 7 节、CLIENT 文档第 1 节与交付摘要只写用户的客户端对应的那一行（能用哪几条路、不支持的与替代做法），从本表取，不凭印象写。“连 game”指连嵌入 TCP 或独立 Gate（玩家协议、自身视图、Lockstep）；“开房间”指第 6 步的 lobby（HTTP）+ relay。

| 客户端 | 连 game | 开房间（lobby + relay） | 登录入口 portal 与大厅 lobby 的 HTTP | 协议与配置表 | 实跑到哪 |
| --- | --- | --- | --- | --- | --- |
| Unity 桌面，托管路线（缺省） | TCP（缺省）或 WebSocket：`Roost.Client` 的 `GameClient` + `RoostClientDriver`；走 WebSocket 时 `ConnectionOptions.WebSocketUrl` 填完整 `ws(s)://` 地址 | `Roost.Client.Relay`，TCP 或 WebSocket | 自己用 `UnityWebRequest` / `HttpClient` 调 portal；lobby 用 SDK 的 `LobbyClient` | 导出的 C# 直接编译 | 运行库在 .NET 上实跑；托管包在 macOS 的 Unity Editor 里编译导入过，Editor batchmode 下经 WebSocket（`ClientWebSocket` + `PumpDriven`）连玩家接入、握手并保持 100s；经 TCP 连服、桌面 Player 没跑。relay SDK 在 Unity Editor（Mono）上 TCP 与 WebSocket 都实跑过（WebSocket 偶发断线修复后没有复跑），桌面 Player 没跑 |
| Unity 原生路线（可选） | TCP：`RoostNativeDriver` + `client/cpp` 的网络内核 | 同上（relay 只有托管 SDK） | 同上 | 同上 | macOS ARM64 的 Editor、Mono / IL2CPP Player 实跑；Windows / Linux 没有编译实跑 |
| Unity WebGL | WebSocket：`Roost.Client` + `new GameClient(RoostPlatform.Configure(new ConnectionOptions{ WebSocketUrl = 地址 }))`、`driver.Attach(client)`；`RoostPlatform`（Unity 包 `client/unity`，`build-unity.py` 含 `--managed-only` 打的包里就有）在 WebGL 上设 `PumpDriven` 与浏览器 WebSocket 桥接，其他平台原样；地址由 portal 进服请求带 `"transport":"ws"` 返回。不 Pump 就不发心跳，超过 `idle_timeout`（缺省 90s）被断开、恢复后重连 | `Roost.Client.Relay` 经 WebSocket：`RoostPlatform.Configure(new RelayClientOptions{ … })`（WebGL 上设 `Transport = Ws`、`PumpDriven`、浏览器桥接）+ `RoostPlatform.CreateLobby(url, gt2)`（WebGL 上用 `UnityWebRequest`），每帧 `RoostPlatform.Poll(); relay.Pump();`（不 Pump 就不发心跳，房主卡住超过 2s 被断开）；建房 / 加入请求的 `Transport` 填 `options.Transport` | `UnityWebRequest`；页面与服务不同源时服务端配 `portal.cors_origins`、`lobby.cors_origins`，relay 配 `ws_allowed_origins` | 同上 | 连 game 与 relay 各在一个 Unity 版本（6000.7.0b7）的 WebGL 构建、Chromium 内置浏览器面板里用 Unity 包里的桥接冒烟通过（连 game：握手 + 只靠 Pump 的心跳保持 100s；relay：建房、收发帧、停过空闲超时、离开）；进入游戏后的自身视图同步在 IL2CPP 裁剪下没验证；Safari、Firefox、手机浏览器、真实切后台没测 |
| 手机（Unity Android / iOS） | TCP：托管路线与桌面同一份代码；原生路线有 Android ARM64、iOS ARM64 的打包入口 | `Roost.Client.Relay` | 同 Unity 桌面 | 同上 | 没有在设备上编译实跑；切后台要断开，回前台重新取票登录 |
| 纯 C# / .NET（工具、Godot .NET） | TCP 或 WebSocket：`Roost.Client`，在主循环里自己调 `Pump()`；Godot 没有现成的主线程适配 | `Roost.Client.Relay`，TCP 或 WebSocket | `HttpClient` | 同上 | .NET 上的自测与生成工程联合链路；Godot 没跑 |
| 网页（TS / JS） | WebSocket：`@roost/client/gate` 的 `GameClient`，`webSocketUrl` 填 portal 进服请求带 `"transport":"ws"` 返回的完整 `ws(s)://` 地址；协议用 `@roost/client/protobuf` + 导出的 `ts/`。**自身视图同步还没有**（第 2 轮），Sync 推送进 `unhandledPush`；后台页被冻结超过 `idle_timeout` 会被断开，回前台靠 `autoReconnect` | `@roost/client/relay`（`/web` 子路径含房主辅助 `WebHost`），只走 WebSocket | `fetch`；跨域配 `portal.cors_origins`、`lobby.cors_origins`；页面来源写进玩家接入的 `allowed_origins` | 协议导出：`client_export.languages` 加 `ts`（服务端开发机要 protoc 31.1、node 22、protoc-gen-es 2.17.0），游戏工程装 `@bufbuild/protobuf@2.17.0`；配置表 `data/*.json` 能直接读，读取与校验代码只有 C# | gate：node 自动化、对真实玩家接入的 Go 驱动用例、Chromium 内置浏览器面板冒烟（连接、握手、心跳保持 100s）；relay：node 自动化与内置面板演练；Safari、Firefox、手机浏览器、小游戏没测 |
| Go（机器人、压测、工具） | TCP：`robot` 包与生成工程的 `cmd/robot`；`robot/transport` 另有 `ws` 拨号器（没有对 game 的 WebSocket 接入跑过），KCP / QUIC 拨号器给帧同步用 | `client/relay`，只走 TCP | `net/http` | Go 代码就在工程里 | 各玩法的 Robot 场景 |
| C++ / Unreal | TCP：`client/cpp`（C++17 与 C ABI）；不支持 TLS、WebSocket、KCP，地址只收数值 IP | 没有 SDK | 自己写 | 导出目录的 `proto/` 副本自己用 protoc 生成 C++ | macOS ARM64 实跑；没有运行过 Unreal 工程；Linux、Windows、移动没有编译实跑 |

- 服务端玩家接入不终结 TLS：TCP 是明文，对公网要放在终结 TLS 的代理后面或只开在内网；WebSocket 的 `wss://` 由前置负载均衡终结，C# 的 `ClientWebSocket` 与浏览器都能连 `wss://`。
- 帧同步（Lockstep）：C# `LockstepClient` 与 C++ 走 TCP；KCP / UDP 在服务端只有装配函数，客户端只有 Go 机器人的拨号器。
- 开发端口：玩家 TCP 7000（独立 Gate 时是 Gate 的）、玩家 WebSocket 7080（写了 `addr` 才开）、portal 是 account 的 `web.addr`（8080 + account 的端口块）、lobby 8080 + 端口块、relay 7100 + 端口块（WebSocket 7200 + 端口块）。
- 细节：[client/README](../../../../client/README.md)、[client/unity/README](../../../../client/unity/README.md)、[Roost.Client.Relay](../../../../client/dotnet/Roost.Client.Relay/README.md)、[client/ts](../../../../client/ts/README.md)、[client/cpp](../../../../client/cpp/README.md)。

**完成标准**：接入方式、谁定的、理由写进 HISTORY 草稿与 DEMO 第 5 节；用户的客户端对应客户端表的哪一行、哪几条路能用写进 DEMO 第 7 节。

## 6. 开房间联机：lobby + relay，房主做 host

**什么时候用**：一局一局的小房间联机，局内逻辑交给房主的客户端算（合作、派对、朋友对战，对作弊不敏感），又不想绑定某个平台（多平台、浏览器、自己的账号）。roost 提供两个框架服务：**大厅 lobby**（HTTP：建房、列房、加入、离开、改状态）与**转发 relay**（客户端直连，服务器只转发、不解析载荷），不需要 game 服务。契约见 [LOBBY](../../../framework/LOBBY.md)、[RELAY](../../../framework/RELAY.md)。

| 需求 | 走哪条路 |
| --- | --- |
| 只在一个平台发行、人数少、局外没有要保存的东西 | 平台方案（第 2 步），不用 roost |
| 房主做 host、跨平台或浏览器、自己的账号 | **本节** |
| 局内结果必须服务器判定、要防作弊、局内状态要落盘、断线要托管 | 不用本节：服务器权威的房间（`room` / `turn` 模块，或 [room-coop-pve.md](room-coop-pve.md)、[party-room.md](party-room.md)），实时对战用帧同步（[moba-lockstep.md](moba-lockstep.md)） |
| 局内房主做 host，局外有账号、货币、养成 | 两者一起：game 工程（局外）+ `add lobby` / `add relay`（局内）；局内结果交给服务器的部分由服务器校验 |

需求要求“服务器判定局内结果、反作弊、relay 崩溃后房间不丢、跨实例接管、断线托管”时，作为冲突提出（KNOWN-LIMITS“大厅 lobby 与转发 relay”）。

**建工程**：

```bash
# 只要开房间、不要 game 服务（只托管 account、lobby、relay）：
"$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template lobby -roost-core-version "$V"
cd "$DIR" && "$ROOST" module add portal
# 已有 game 工程上加：
"$ROOST" add lobby           # 没有托管 account 时一并声明；补 account.services、account.games
"$ROOST" add relay
```

- 生成的配置可以直接 `make dev-run`：lobby 端口 8080+N、relay 7100+N、relay WebSocket 7200+N（N 是端口块）；`lobby.games` 里有一个以工程名命名的游戏（`max_members: 8`），`account.games` 同名；两边的 `token_key_env` 都是 `ROOST_RELAY_TOKEN_KEY`，开发时 `run.sh` 给开发密钥，生产由环境变量注入（至少 16 字节）。改游戏名、人数上限时两处一起改（doctor 的 `lobby-relay:account-games` 会查）。
- **登录入口**：客户端先拿游戏账号 token（`gt2.…`）。`-template lobby` 的工程只装 `portal`（连带 account_auth），不要装 `login`（它要 game 服务）；开发登录在 `configs/service/config.account.yaml` 末尾写 `login:` 下的 `dev_token: dev-…`。这种工程没有区服：`/portal/servers` 的清单为空，`/portal/enter` 答 `560104`。有 game 的工程按第 8 步装 `login portal enter`。
- 客户端用 `POST /portal/login {channel, open_id, credential, game}` 一步拿到 `game_token`，或拿账号票据后 `POST /portal/game-token {account_ticket, game}`。
- 生产：relay 每个实例要能被客户端直连（`advertise_addr` 不能是回环、每个实例不同）；浏览器要用时配 `relay.ws_listen`、`ws_advertise_addr`（`wss://…`）、`ws_allowed_origins`（空表示拒绝所有浏览器页面）；游戏页面与服务不同源时 lobby 配 `lobby.cors_origins`、portal 配 `portal.cors_origins`（写在 account 的配置里），不写就不允许跨域。上线前的渠道验签、TLS、限流归工程，写进报告的“没做的”。

**客户端流程**（写进 CLIENT 文档）：`POST /portal/login` 带 `game` 拿 gt2 → 房主 `POST /rooms`（`Authorization: Bearer gt2…`，`transport` 选 tcp / ws）得到 `{room, relay_addr, token}` → 连 relay、首帧发令牌、收 Welcome；成员 `GET /rooms?game=&mode=` → `POST /rooms/{id}/join` → 连 relay。房主 `POST /rooms/{id}/state` 改成已开始后新人进不来；开局后掉线的人席位保留、再 join 回原座。断线重连由 SDK 先经 lobby join 拿新令牌再连 relay。

**玩法逻辑跑在客户端**：relay 不解析载荷、服务器不托管房间逻辑，所以本 skill **不为局内玩法写服务器代码**；交付工程（lobby、relay、account）、CLIENT 文档与房主规则的写法建议。DEMO 第 2 节里局内的判定（命中、结算、随机、胜负）都标“房主客户端”，风险写明“房主可以改包作弊，成员之间结果以房主为准”。

**两种通道**（帧头区分，载荷格式由游戏定）：游戏通道（成员发的只到房主；房主按槽位或全体发）；房间通道（聊天、表情、语音这类不需要房主处理的消息，单独限额，房主可禁言，超限丢帧不掉线）。

**换 host 与快照**：房主主动转交（`Transfer`，交接数据随帧带上，最完整）；房主离开（立即换）；房主断线（约每 0.6s 要有一帧，SDK 自动补心跳；断线宽限 5s，过了按房主声明的候选顺序 `SetSuccessors`、兜底最早进房的人选新房主，新房主回 `HostReady` 才生效）。快照里只放别人没有的状态（随机种子、AI 内部状态、隐藏信息、最近还没广播的操作），压缩后不超过 256 KiB，两份之间至少 1s；断线换 host 最多丢最后一份快照之后的变化。同一房间的客户端必须用同一种压缩实现。

**SDK**（[client/README“开房间 SDK”](../../../../client/README.md#开房间-sdklobby--relay)；各平台能不能用、要配什么、实跑到哪，以[客户端表](#客户端能连什么按客户端类型)为准）：Unity / C# 用 `client/dotnet/Roost.Client.Relay`（netstandard2.1，TCP 或 WebSocket，回调在 `Pump` 的线程上；Unity WebGL 经 Unity 包的 `RoostPlatform` 开 `PumpDriven`、换浏览器 WebSocket 桥接与 `UnityWebRequest` 的 lobby HTTP，见客户端表）；网页用 `client/ts` 的 `@roost/client/relay`（只走 WebSocket，`@roost/client/web` 含 `WebHost`：房主规则写成纯 TS 模块，放进 Worker 跑，页面隐藏满 `hiddenHandoffMs` 转交房主，见 [client/ts/README](../../../../client/ts/README.md)）；Go 机器人与压测用 `client/relay`（只走 TCP）。一个房间里 TCP 与 WebSocket 的连接可以混在一起，三份 SDK 可以混用。客户端表“实跑到哪”一列里没跑过的平台写进 DEMO 第 9 节。

**完成标准**：`"$ROOST" project doctor` 没有 FAIL 行，`lobby-relay:*` 各行是 OK（`-template lobby` 工程装 portal 之前有一行 `collaborators:account` FAIL，装完通过）。局内玩法在客户端，没有服务器场景可写；报告写明“lobby + relay 的多进程只在单机演练过，跨机、容量未验”。

### 6A. 专业模式：决策单（写代码前一次问完）

专业模式与“只给决策”在第 2～6 步不逐条提问，把结论汇总进一份 `docs/DECISIONS.zh-CN.md`，第 6 步之后一次交给开发者。没有工程目录时先写在报告里，建工程后放进工程。

```markdown
# 决策单（<日期>，roost-core <版本>）

回复方式：“全部接受”；“全部接受，除了 3、7（7 改为……）”；或直接在“开发者改为”一栏填写后说“按文件做”。空着的一栏表示接受建议。

| 编号 | 类别 | 建议 | 理由 | 风险 | 开发者改为 |
| --- | --- | --- | --- | --- | --- |
| 1 | 要不要 roost | 需要 | 有要防作弊的货币与养成 | — | |
| 2 | 战斗分档 | 第 2 档：锁定技能、服务器判定 | 同屏 ≤ 50 人 | 高延迟下非指向技能手感差 | |
| 3 | 接入方式 | 嵌入 TCP | 按请求交互、很少推送 | 以后改成长期在线要加 Gate | |
| 4 | 需求条目 | 每日签到拆成 3 条：…… | | | |
| 5 | 边界 | 冷却倒计时放客户端显示、服务器再判 | | 客户端改包只能看到假倒计时 | |
| 6 | 数值 | 签到奖励 10×min(n,7) 金币（样例） | 需求没给 | 上线前要改 | |
| 7 | 复用 | 签到：直接装 checkin | | | |
| 8 | 归属 | 全服限量计数挂 World 组件（附录 A3） | 要和玩家修改一起回滚 | 热点写排在一把锁上 | |
| 9 | 配置表 | item 表两端共用，掉落权重只给服务端 | 客户端拿到权重就能算出掉落 | | |
| 10 | 冲突 | 永久防重：wallet 去重窗口只有 128 次 → 推荐 C：用自己的状态判重 | KNOWN-LIMITS“业务模块” | | |
| 11 | 持久化方式 | 普通写用缺省 async（写进 WAL 就回复）；回复后马上对外部确认的写与 saga 步骤用 pipelined；只读用 memory | async 最快，进程崩溃不丢已回复的修改，整机掉电可能丢最后约 10ms；pipelined 等 fsync 才回复、锁先放 | 不用 strict：它在锁内等 fsync，同一实体只有百级 TPS，pipelined 已经保证回复前落盘 | |
```

- 类别至少覆盖：要不要 roost、战斗分档、接入方式（含开房间）、需求条目、边界（逐个逻辑点：校验、计算、随机、计时、判定、展示，重点列移动与碰撞、战斗数值与命中、冷却、寻路、客户端预测、本地缓存的配置）、数值、复用、归属（实体、服务、单例实体还是挂 World）、配置表（逐表）、冲突（四要素）、持久化方式（缺省 async；开发者明确要求时才把指定的 handler 改成 `durability=strict`，代价是等落盘再回复、同一实体约百级 TPS，本机样本约 120 次 / 秒，见 KNOWN-LIMITS“热点单例实体的写吞吐”）。
- **“只给决策”**：表头换成 `编号 | 类别 | 决策 | 出处 | 开发者改为`，不写理由与风险；写完就停在这里，不建工程、不写代码，报告给出决策单路径。用户改好后说“按这份做”，再按专业模式从第 7 步继续。
- **用户事先给了决策单**（工程里已有 `docs/DECISIONS.zh-CN.md`，或用户贴来一份）：照它做，不再问；单里没有的新决策按建议做，并追加进单、在报告里列出。
- 确认后的决策单留在工程里，第 12 步把最终结论写进 DEMO 第 1、2 节。之后改需求时只追加受影响的行。

**完成标准**：开发者回复了确认（或“只给决策”模式已交出决策单）；无人可问时，按建议做并把整张单列进摘要的“待决定”。

## 7. 建工程

**新工程**：

```bash
"$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game -roost-core-version "$V"
cd "$DIR"
```

- `-template game` 带 Player 与 World 两个实体，只有 game 一个业务服务，缺省不托管任何框架服务：装的模块用到哪个托管服务（login 与 portal 的 account、mailbox 的 mail……），安装器就在那次安装里声明哪个。手写功能要直接调某个托管服务（没有对应模块）时加 `-framework rank,chat`（或 `-framework all`）。
- `-recipe casual` 或 `-recipe turn-pvp` 直接创建已组合的工程（自动采用 game 底座），都带登录链路与开发配置：casual 是首次发放、签到、任务、体力、商店、背包、邮件；turn-pvp 是档案、本服匹配、房间与回合。模块名单见[配方](../../../../modules/README.md#从配方创建工程)。**需求覆盖配方的全部模块时才用配方**：模块不能卸载，多出来的去不掉。
- 只做开房间联机时用 `-template lobby`（第 6 步）。

**客户端是 C# / Unity 时**，在 `roost.yaml` 末尾加导出目录（第 1 步已准备好 protoc 31.1）：

```yaml
client_export:
  dir: shared/protocol        # 协议：proto 副本、C# 协议与同步代码、消息号、错误码、manifest.json
  languages: [csharp]
  config:
    dir: shared/config        # 配置表：只含客户端字段的 JSON、C# 读取代码、manifest.json
```

之后每次 `roost generate` 都更新 `shared/`，这个目录要提交。

客户端不是 C# 时（C++ / Unreal）：`client_export` 不写 `languages`，只导出 `proto/` 副本与 `manifest.json`（不需要 protoc），客户端用与自己的 Protobuf 运行库同版本的 protoc 生成代码；`config` 照写，`data/*.json` 是给客户端的数据，读取代码只有 C#。网页客户端只走开房间时用不到协议导出。

**已有工程**：`cd` 进去，先读 `roost.yaml`（`modules` 段是已装的模块，`services`、`features`、`ids` 是装配现状），再 `"$ROOST" module list`，带 `*` 的是已装的。不要重装已装的模块；已装模块的文件是业务代码，以工程里的为准。然后按第 14 节走。

**完成标准**：`"$ROOST" project doctor` 的输出里没有 `FAIL` 行。

## 8. 装模块与登录

### 8A. 业余模式的最短骨架

不论需求是什么，有 game 服务的工程先装登录这条线：

```bash
"$ROOST" module add login portal enter servertime   # 连带 account_auth，并声明托管的 account 服务
```

- `login`：握手时把票据交给 account 校验；`portal`：客户端的 HTTP 登录入口；`enter`：建档、进入游戏、打开玩家 TCP 与自身视图；`servertime`：服务器时间与每日重置时刻。
- 装完 doctor 没有 FAIL。Player 上还没有给客户端看的字段时有一行 `WARN self-sync`，装了带同步字段的模块（wallet 等）或手写了 `sync` 字段之后消失。
- 需求正好覆盖某个配方时用配方（第 7 步），它已带这四个模块。
- 一个模块都不装、手工接玩家 TCP 的做法只在专业模式下由开发者要求时用，见[附录 A8](#a8-不装任何模块时的手工接入专业模式)。

**开发配置**（安装输出里的 `todo:` 行逐条照做，配方与 game-demo 工程已经写好）：

1. 在 `configs/service/config.account.yaml` 末尾加开发登录与区服地址：

   ```yaml
   login:
     dev_token: dev-local          # 必须以 dev- 开头，只写在开发配置里；渠道 dev 的凭据就是这个值
   portal:
     servers:
       "1000": 127.0.0.1:7000      # 服号 -> 客户端连得到的地址（嵌入 TCP 时是 game 的 player_access.tcp.addr）
   ```

   有独立 Gate 时地址填 Gate 的；也可以改在 game 的配置里写 `login.presence.addr`，由 game 定期上报。浏览器 / WebGL 客户端还要 WebSocket 地址：在同一处加 `websocket_servers: {"1000": ws://127.0.0.1:7080/ws}`，或在 game 配置写 `login.presence.ws_addr`（第 5 步）。
2. `"$ROOST" config enable player-tcp`：打开 game 的玩家 TCP 监听。
3. 区服登记：`make dev-run` 起完服务后自动登记本机的服（缺省 1000）；别的环境用 `go run ./cmd/accountctl upsert-server -sid <服号>`。

**客户端的登录顺序**：`POST /portal/login`（渠道、open_id、凭据；可带 `enter`；走 WebSocket 的客户端在 `enter` 里写 `"transport": "ws"`，拿返回的 URL 去连）→ 用 `enter:<进服票据>` 握手 → 服务器推 `EnterStatus{need_create}` → 需要新建就发 `EnterCreate{name}` → `EnterGame`。推送只是告知，没收到就直接发 `EnterGame`，得到 `enter_need_create` 再发 `EnterCreate`。登录时带 `"enter": {}` 进推荐的服、`{"server_id": N}` 进指定的服，答复里直接有进服票据与地址，省掉 `/portal/servers` 与 `/portal/enter` 两个来回；推荐规则一个服也没推荐时答复里没有 `enter`，客户端改走 `/portal/servers` 让玩家选（`modules/portal/README.md`“端点”）。

- 新玩家的初始发放写在 enter 的首次初始化里（[附录 A7](#a7-进入游戏建档与首次初始化)），不写在登录入口或握手里。
- 上线前要做而模块不替你做的：渠道验签（`services/account/login_channels.go`）、入口前面的 TLS 与限流、生产配置里的地址，写进报告的“没做的”。
- 细节读 `modules/portal/README.md`、`modules/login/README.md`、`modules/enter/README.md`。

### 8B. 装第 4 步定下的模块

```bash
"$ROOST" module list --json                 # 模块、依赖、状态字段、场景数
"$ROOST" module plan checkin --json         # 只预览，不改工程
"$ROOST" module add checkin                 # 连同依赖安装并运行生成器
"$ROOST" module verify                      # 每个模块的场景数 + build / vet / id check / generate --check
```

- `plan` 的 JSON：`modules[].status` 是 `install` / `installed` / `installed_other_version`；`prerequisites` 是会先执行的 `roost add`；`framework_services` 是这次新声明的托管服务；`ids`、`state`、`files` 是将分配的号、追加的 DAO 字段、写入的文件；`runtime` 是自动接入服务启动与停机的入口（[服务级接线](../../../../modules/README.md#服务级启动与停机)）；`managers` 是模块的后台 manager（[后台循环](../../../../modules/README.md#后台循环manager)）。
- `add` 报错时按错误文字处理，不要绕过：`already exists and is not this module's file`（落点被业务文件占着，告诉用户，由用户决定）；`cannot be installed yet`（缺 Mod 或 feature，错误里给了命令）；`is taken by a business service`（同名业务服务，这是冲突，不要改名绕过）。
- **联动文件**：输出里的 `glue <模块> with <另一个模块>` 是两个都装着时安装器自动写的接线，归工程所有；`module verify` 把它们的场景单列一行 `<模块>+<另一个模块>`。
- 模块自带的客户端协议（如 wallet 的 `WalletSpend`）是样例入口，需求里没有对应操作时告诉用户，由用户决定留不留。
- 装完之后模块文件就是业务代码：要改规则直接改（例如 `game/components/checkin/checkin.go` 里的奖励），改了行为就同步改它自带的测试，再 `module verify`；不要删测试。
- **扩展已装的模块**：只动它留给工程的业务文件（README 的“复用方式”列出）、它发布的实体事件（在自己的组件上 `//roost:listen`）与它的登记点。在别的功能里用它的数据或动作，调 README 写的入口（同一个实体上直接调组件，例如 `host.Wallet().Add(delta)`）。

**完成标准**：`"$ROOST" module verify` 每行以 `ok` 开头、退出码 0；`"$ROOST" project doctor` 没有 FAIL 行。

## 9. 手写模块库里没有的功能

**先定样板**（规则 3）。结论是“借鉴”或“手写”的每个功能，动手前写下两行：样板是哪个模块、借鉴它的哪一点。读那个模块的 README 与文件（装了的看 `roost.yaml` 的 `modules.<name>.files`，没装的看 `$CORE/modules/<name>/files/`）。

| 要写的东西像什么 | 样板 | 借鉴的点 |
| --- | --- | --- |
| 一个数值的加减、要防重发 | `wallet` | 可见状态与服务端账本分两个字段；带业务键的幂等 |
| 每天 / 每周做一次的事 | `checkin`、`task` | 业务日切分；“做过没有”记在自己的状态里 |
| 随时间恢复、有上限的资源 | `stamina`、`idle` | 惰性结算，不用定时器 |
| 花一种东西换另一种 | `shop`、`gacha` | 扣与发在一笔事务里；请求键防重；配置表 |
| 一份可以领的东西 | `mailbox`、`purchase` | 领取账本；恰好一次 |
| 两个玩家之间的关系 | `friends` | 两个 Player 同一笔多实体事务 |
| 多人共用的对象 | `room`、`team` | 独立实体；成员引用与对象同事务；重连代际 |
| 跨进程、要补偿的流程 | `alliance` | 单写者服务 + Mirror + 路由 Saga |
| 到点要发生的事 | `turn`、`activity` | 实体的持久定时器；超时推进幂等 |
| 进入游戏时顺带做的事 | `enter` 的联动（`modules/*/with/enter`） | 事务外的 `OnEntered` 可重试；必须成功的监听事务内的 `event.EnterFirst` |
| 给运维用的操作 | `gm` 的联动（`modules/*/with/gm`） | 命令登记、审计，写入仍走正式事务 |
| 别的功能做完一件事后跟着改 | `task` 的联动（`modules/task/with/*`） | 在自己的组件上 `//roost:listen`，同一笔事务 |

借鉴的是做法，不是复制粘贴：状态、协议、错误码都是自己的，文件名带自己的功能名。找不到接近的模块时按[附录 A1](#a1-框架的核心编程思路)逐条对照，报告里写“没有可借鉴的模块”。

一个挂在 Player 上、带一条客户端协议的功能 `<feature>`，按这个顺序：

1. **状态**：新建 `db/def/<feature>.go`，定义嵌套结构体，字段只写 `store` 键（嵌套类型内从 `"1"` 起编号）；在 `db/def/player.go` 的 `PlayerDao` **末尾**加一个顶层字段，字段名与存储键带功能名：

   ```go
   // TaskState 是每日任务的进度。
   type TaskState struct {
   	Day      int64 `store:"1"`
   	Progress int32 `store:"2"`
   	Claimed  bool  `store:"3"`
   }
   ```

   ```go
   	// Task 是每日任务给玩家本人看的进度。
   	Task TaskState `store:"task" dao:"persist,sync"`
   ```

   - 顶层字段默认既落库又同步给本人；账本、回执、种子这类只给服务端的字段写 `nosync`，只同步不落库写 `nopersist`；单写 `persist` 不会关掉同步。既有可见状态又有服务端账本就写两个字段。嵌套结构体的字段上不写 `dao` 标签。
   - 只能加在末尾；不改已有字段的 `store` 键。
2. **组件**：写在 `game/components/<feature>/`（[附录 A2](#a2-组件的写法组件包--gameview--实体事件)）：组件类型号与工厂登记在组件包里，对外的方法列进 `game/view/component_<feature>.go` 的接口，实体包里只留生成文件与手写的宿主方法。

   **生成组件骨架**：`"$ROOST" add component <Feature> --entity <Entity>`（工程只有一个实体时 `--entity` 可省），再 `"$ROOST" generate`。命令写出：
   - `game/components/<包>/<包>.go`：`//roost:component`、类型号、工厂登记、`Owner`、`Component`（业务文件，只建一次）；
   - `game/view/component_<包>.go`：接口 `<Feature>` 与 `<Feature>Holder`；
   - 实体结构体上挂 `<feature> *<包>.Component` 字段。
   
   generate 之后有访问器 `<Feature>()`、`view.<Entity>Entity`、`view.I<Feature>Entity`。接着：业务方法写在组件包的 `*Component` 上；要被别处调用的方法列进 `view.<Feature>`；用 `"$ROOST" add handler <Handler> --entity <Entity> --component <Feature>` 生成收 `view.I<Feature>Entity` 的 handler，里面只写 `target.<Feature>().<方法>(…)`；组件要实体提供的东西写成 `<Feature>Holder` 里的宿主方法，在 `game/entities/<entity>/<包>.go` 实现。同一个组件挂到第二个实体时再跑一次 `add component`，按提示把 `Owner` 改成最小接口。

   状态只经生成的 setter 改（`c.owner.Dao().GetTask().SetProgress(…)`）；组件不另存会被回滚的东西、不加锁。同一实体上的别的组件经 view 访问器直接调（`c.owner.Wallet()`），同一把锁、同一笔事务。“只做一次”怎么保证：
   - **自己的状态里记着这一次做过没有**（今天领过、这个订单到过账）：先判自己的状态，再 `c.owner.Wallet().Add(delta)`。周期性发放都用这个。
   - **别的模块做完某件事时跟着改**：在组件上写 `//roost:listen` 方法监听 `game/event/` 里的事件，同一笔事务。
   - **没有别的事实可判**（客户端带请求号的消费）：`c.owner.Wallet().Apply(key, delta)`，同一个 key 在去重窗口内只生效一次。窗口只留最近 128 次，是短期防重发，不是永久账本。
3. **错误码**：`"$ROOST" add errcode <feature>_<reason>`（一码一个），生成在 `internal/errors/`，把说明文字（生成时是 `TODO`）改掉。业务拒绝从组件或 handler 返回这些带码的错误，生成的端点把它转成回包的 `Code` / `Reason`。
4. **协议**：`"$ROOST" add protocol <Feature><Action> --group game --handler <feature>`，在 `protocol/def/<feature>_<action>.go` 里给 Request / Response 加字段（Response 保留 `Code`、`Reason`）。
5. **Nest handler**：`"$ROOST" add handler <Feature><Action> --entity Player --component <Feature>`，把 `game/handler/<feature>_<action>.go` 改成收 PB 请求、回 PB 响应：

   ```go
   //roost:nest
   func handlerTaskClaim(target view.ITaskEntity, request *pb.TaskClaimRequest, nowUnixMilli int64) (*pb.TaskClaimResponse, error) {
   	result, err := target.Task().Claim(nowUnixMilli)
   	if err != nil {
   		return nil, err
   	}
   	return &pb.TaskClaimResponse{Reward: result.Reward}, nil
   }
   ```

   - `//roost:nest` 不带参数就是缺省的 `rollback=state durability=async`：修改写进 WAL 文件后就回复，不等 fsync。进程崩溃（kill -9、panic、OOM）不丢已回复的修改；代价是整机掉电或内核崩溃时可能丢最后一个组提交间隔（`dataengine.wal.group_commit_interval`，缺省 10ms）里的修改，由业务幂等兜底。回复之后马上向外部系统确认的（通知别的服务“已领”、删外部队列里的待发记录、在账本上标完成），handler 写 `durability=pipelined`：等 fsync 才回复、实体锁先放，掉电也不会出现“外部已确认、本地丢了”。saga 原生步骤与补偿同样用 pipelined（`roost add saga -native` 生成的就是它）。只读 handler 写 `durability=memory`。不写 `durability=strict`：它在锁内等 fsync，pipelined 已经够严格；生成的骨架或借鉴的旧代码里带 strict 时删掉或换成 pipelined。
   - 一个 handler 一个文件，文件名是 handler 名的蛇形写法。
   - 目标参数是生成的窄接口 `view.I<Feature>Entity`，经访问器 `target.<Feature>()` 调组件；`Claim` 要先列进 `view.Task`。要整个玩家时收 `view.PlayerEntity`。
   - 最后一个参数写 `nowUnixMilli int64` 拿到这条消息冻结的业务时间；客户端不传时间。
   - `pb` 是 `<module>/protocol/pb`，要先 `"$ROOST" generate` 一次才有。请求没有字段时也保留 `request` 参数。
   - 不对客户端开放的写入（初始化、别的功能触发的发放）也写成 Nest handler，参数用普通类型，不建协议和端点，由服务端代码经生成的 Sender 调用。
6. **端点**：`"$ROOST" generate`，再 `"$ROOST" add endpoint <Feature><Action> --handler <feature>`，再 `"$ROOST" generate`。得到 `game/controllers/<feature>.go` 里的手写入口与生成的 `*_endpoint_gen.go`。`game/controllers` 是一个包，`controller.go` 归生成器，每个功能一个 `<feature>.go`。组件挂在几种实体上、这些实体归属不同服务时，handler 写 `//roost:nest target=<实体>`，endpoint 加 `--target <实体>`。PB 请求直接接入 Nest 的细节见[命令参考“PB 请求与响应直接接入 Nest”](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)。
7. **场景测试**：照 `game/handler/wallet_test.go` 写 `game/handler/<feature>_test.go`：替身提交器、`nest.NewEngine`、生成的 `syncsender`，覆盖成功、拒绝时零写入、重复请求、提交被拒后回滚。测试名加功能前缀；只断言自己的事实。
   - **场景数先记进 DEMO 第 8 节**（每个功能几条、测试名前缀），第 10 步验收时比对。
   - 拨时间（`clock` 是 `infra/base/clock`；进程级，一个用例只建一个 fixture）：

     ```go
     var now atomic.Int64
     now.Store(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).UnixMilli())
     clock.SetSource(func() time.Time { return time.UnixMilli(now.Load()) })
     t.Cleanup(func() { clock.SetSource(nil) })
     // 之后 now.Add(毫秒数) 就是拨快业务时间；handler 的 nowUnixMilli 跟着变。
     ```

   - 需求给了数的（上限 20、每 6 分钟），测试里把数字写死，不引用被测代码的常量。
   - getter 与替身提交器不借用模块测试文件里的类型（那些文件可能被改）。
   - 没有天然幂等键的写入（“每完成一局上报一次”）重发会多记。要防重就让请求带业务键并在自己的状态里判重；不要就把这条假设写进报告。

**日界**：“今天”用 `int64(clock.BusinessDay(time.UnixMilli(nowUnixMilli)))`，得到 `YYYYMMDD` 的业务日，存进状态、按大小比较；业务时区与重置时刻是部署配置（缺省 UTC 零点）。每日清零按惰性做：当天第一次写入时发现存的不是今天就清；状态带上业务日字段，客户端比对它判断是否已是新的一天。

**随时间恢复的数值**（体力、冷却）：不用定时器逐点加。状态里存“上次结算后的值”和“下一点恢复的时刻”，每次写入前按 `nowUnixMilli` 结算。两个字段都同步给客户端，由客户端推算当前值与倒计时，这条约定写进 CLIENT 文档。

**不挂在玩家身上的状态**（房间、公会、全服对象）用独立实体：`"$ROOST" add entity <Name>` + `add dao <Name> --entity <Name>`，多人争用的对象把实体标记里的 `category` 改成 `entity.EntityCategoryWorld`。全服一份的东西先看[附录 A3](#a3-进程内的全局逻辑manager还是挂在-world-上)。

不要做的事：不改带 `_gen` 的文件；不在 handler 里等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体；不用进程级全局变量传状态。

**完成标准**：`go test -mod=mod -race -count=1 -v -run '^Test<前缀>' ./game/handler` 的 `--- PASS` 行数等于 DEMO 第 8 节记的数，`--- SKIP` 为 0。

## 10. 验收

```bash
"$ROOST" module verify                                            # 已装模块 + build / vet / id check / generate --check
go test -mod=mod -race -count=1 -v -run '^Test(<FeatureA>|<FeatureB>)' ./game/handler 2>&1 | tee "$WORK/feature.log"
grep -c '^--- PASS' "$WORK/feature.log" || true                   # 等于 DEMO 第 8 节记的场景数
grep -c '^--- SKIP' "$WORK/feature.log" || true                   # 必须为 0
go test -mod=mod ./...                                            # 生成代码自带的测试
"$ROOST" project doctor || true
```

- `-run` 写错或用例被跳过时 `go test` 照样报 `ok`，所以场景数要自己数。
- `project doctor`：除下面两类外不应有 FAIL。`-framework` 显式带上、又没有模块接管的 account / chat / platform / session 会各报一项 `collaborators:<服务>` FAIL（生成的拒绝桩，上线前由工程实现）；`-template lobby` 工程装 portal 之前的 `collaborators:account`。
- 声明了 `client_export` 的，`client-export:fresh` 与 `config-export:fresh` 是 OK。
- 这一档是离线验收：替身提交器，没有 Mongo / NATS / Redis，没有真实客户端。

**完成标准**：`module verify` 退出码 0；手写场景数与 DEMO 一致、0 SKIP；`go test ./...` 退出码 0；doctor 没有上面两类以外的 FAIL。

## 11. 起服冒烟

**必做**。离线验收证明不了“客户端连得上”。做不了（没有 docker、docker 没在运行、端口被占）时如实报告“起服冒烟未做”与原因，不要跳过不提。

```bash
# 0. 开发配置已按第 8A 步写好：login.dev_token、portal.servers、player-tcp 已打开
make dev-up        # docker compose 起 Redis、Mongo、NATS、etcd（deploy/dev/docker-compose.yaml）
make dev-run       # 编译并起全部服务；起完自动登记区服 1000；日志在 .dev/
PORTAL=http://127.0.0.1:8081   # configs/service/config.account.yaml 的 web.addr（开发端口 8080 + account 的端口块）
LOGIN=$(curl -s -X POST "$PORTAL/portal/login" -H 'Content-Type: application/json' \
  -d '{"channel":"dev","open_id":"smoke1","credential":"dev-local","enter":{}}')
echo "$LOGIN" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["ok"], d.get("enter",{}).get("addr"))'
ROOST_PLAYER_TOKEN=$(echo "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["enter"]["ticket"])') \
  go run ./cmd/playerprobe -addr 127.0.0.1:7000      # game 的 player_access.tcp.addr；独立 Gate 时连 Gate
make dev-stop && make dev-down
```

**你应该看到**：

- `make dev-run` 末尾 `all services ready`；
- 第一条 python 打印 `True 127.0.0.1:7000`（登录成功，并拿到了进服票据与地址）。打印 `False` 时看答复里的 `code`：`560102` 是凭据不对（`dev_token` 没写或不一致），`560104` 是服没登记；`enter` 缺失是没有可推荐的服；
- playerprobe 打印 `player TCP authentication succeeded: 127.0.0.1:7000`（握手通过，account 认了这张票据）。

**握手之后发一条业务请求**（`EnterGame` 或自己的功能）没有通用命令行工具：playerprobe 只做握手。要么用客户端（第 12 步 CLIENT 文档的最小接线）跑一次，要么自写机器人（`-template game-demo` 带 `cmd/loadtest`，`make dev-smoke` 只在那种工程里有）。没做时报告里写“握手之后的业务请求未经真实连接验证”。

票据只放在变量里，不打印、不写进文档与日志。用完 `make dev-stop`、`make dev-down`，确认没有遗留进程。

**完成标准**：两行期望输出都出现，写进 DEMO 第 9 节与报告；或报告里写明“未做”与原因。

## 12. 文档：DEMO、CLIENT、HISTORY 与决策单

每个工程都带这些文件，三种模式都要生成（“只给决策”只有决策单）：

| 文件 | 给谁 | 内容 |
| --- | --- | --- |
| `docs/DEMO.zh-CN.md` | 接手工程的人（服务器与客户端） | 当前状态 |
| `docs/CLIENT.zh-CN.md`，复制一份到 `shared/README.md` | 客户端开发者与客户端 agent | 客户端对接的唯一入口 |
| `docs/DEMO-HISTORY.zh-CN.md` | 下一轮增改需求的 agent | 逐次变更，只追加 |
| `docs/DECISIONS.zh-CN.md` | 专业模式的开发者 | 第 6A 节的决策单 |
| `AGENTS.md`、`CLAUDE.md`、入口 skill 副本 | 之后在工程里工作的 agent | 本节末尾 |

内容从工程里取，不凭记忆写；改了工程就同步改。

### DEMO.zh-CN.md 固定九节

1. **需求对照**：每条需求一行（服务器部分、客户端部分、结论、模块或样板、状态），加上冲突与待确认的数值。开头写模式、第 2 步的判断与用户的选择。
2. **服务器 / 客户端边界**：逐个逻辑点写放在哪一端、谁定的（“按缺省规则”“开发者决定”“开发者确认了建议”）、放客户端的风险与服务器还做了哪些校验。另附配置表的分端（每张表两端共用 / 只给服务端 / 只给客户端，共用表里只给服务端的字段与理由）。开房间联机的局内判定标“房主客户端”。
3. **模块**：装了哪些（含连带的），每个一句话说它在本工程里管什么、通过的场景数；装上了哪些联动；模块自带而需求没用到的客户端协议。来源：`roost.yaml` 的 `modules`、`module verify` 的输出。
4. **实体**：每种实体一节：谁创建、挂在哪个服务上、主 DAO 的顶层字段（字段名、来自哪个模块或手写、`persist` / `sync`）、组件与组件号、持久定时器。来源：`db/def/*.go`、`game/components/`、`game/entities/`、`roost.yaml` 的 `entities` 与 `ids`。
5. **服务**：玩家接入方式（第 5 步结论）；`roost.yaml` 的 `services` 逐个列：业务服务还是托管服务、带了哪些 Mod、对外的端口与入口、哪些模块的逻辑跑在它上面、服务之间的 RPC / Saga；开发启动方式（`make dev-up`、`make dev-run`）与依赖。
6. **协议与错误码**：客户端请求、服务器推送各一张表（名字、号、谁处理、要点），错误码表（名字、号、什么时候出现）。写明本轮 `shared/protocol/manifest.json` 的 `sources.protocol_def`、`sources.dao_sync` 两个哈希与 `generator` 版本；导出了配置表的再写 `shared/config/manifest.json` 的 `data_version_short`。
7. **客户端对接**：三五行摘要（客户端引擎与平台、对应[客户端表](#客户端能连什么按客户端类型)的哪一行、能用与不支持的路、接入地址怎么配、登录方式），详见 `docs/CLIENT.zh-CN.md`。
8. **手写的功能**：文件清单、样板模块与借鉴点、业务假设（数值、日界、上限）、日志打在哪、**每个功能的场景数与测试名前缀**（第 9 步写测试时先记，第 10 步比对）。
9. **验收与上线前缺口**：第 10、11 步的实际结果；离线验收的范围；上线前要补的（渠道验签、TLS、限流、生产地址、托管服务的 collaborators）。

### CLIENT.zh-CN.md 固定结构

写完复制到 `shared/README.md`（两份内容相同，随导出目录一起提交），客户端仓库同步 `shared/` 时就能读到。每次改了协议、同步字段、错误码、配置表或地址，两份一起改。

```markdown
# <游戏名> 客户端对接（服务端 roost-core <版本>，导出 manifest <protocol_def 哈希前 12 位>）

## 1. 客户端第一步
本客户端（<引擎 / 平台>）能用：<连 game 走 TCP / 开房间走 relay 的 TCP 或 WebSocket / …>；不支持：<…，替代做法；没有写“无”>。
1. 读完本文件。
2. 同步导出目录：协议 C# 到 <客户端目录>，配置表代码与数据到 <目录>（第 10 节）。
3. 做一处能改服务器接入地址的地方（第 2 节），缺省值填第 3 节的开发地址。
4. 跑最小联调：登录 → 握手 → EnterGame（第 4、5、11 节），看到 <期望的现象>。

## 2. 服务器接入地址怎么配
## 3. 开发环境地址与端口
## 4. 开发登录的原样请求
## 5. 登录到进入的时序
## 6. 每个功能的请求、回复与推送
## 7. 自身视图字段
## 8. 错误码与提示建议
## 9. 配置表怎么读
## 10. 不装 Go 与 roost CLI 时怎么拿到协议与 SDK
## 11. 最小 C# 接线
```

各节写什么：

1. **客户端第一步**：开头一句照[客户端表](#客户端能连什么按客户端类型)里用户客户端那一行写：能用哪几条路、哪些不支持与替代做法（表里写着不支持的，不能写成“能联机”）。再照上面的四步，填上本工程的实际目录与“期望的现象”（例如“EnterGame 回包 `First=true`，自身视图收到 Wallet”）；连 game 不支持的客户端，第 4 步换成它能走的路（开房间时是登录 → 建房 → 连 relay → 收 Welcome）。
2. **服务器接入地址怎么配**（不论客户端是什么引擎都要写）：客户端里要有一处能改服务器接入地址，不写死在代码里。
   - **位置**：客户端 GM / 调试面板里的输入框（改完存到本地，如 Unity 的 `PlayerPrefs`，下次启动仍用它，面板上显示当前生效的地址），或一个打包后还能改的配置文件（Unity 用 `StreamingAssets/server.json` 这类位置）。至少一处；正式包里 GM 面板通常关掉，所以至少保留配置文件。两处都有时 GM 面板的值优先，其次配置文件，最后是代码里的缺省值。
   - **要填哪些地址**：装了 portal 时只填入口的 HTTP 地址（`http(s)://host:port`），区服的连接地址由 `/portal/login` 带 `enter`（或 `/portal/enter`）返回，不另填；没装 portal、直接连服的，填玩家接入的 `host:port`（独立 Gate 时是 Gate 的，嵌入 TCP 时是 game 的）；开房间联机填入口地址与大厅 lobby 的 HTTP 地址，relay 的地址由 lobby 按请求里的 `transport` 返回（`tcp` 是 `host:port`，`ws` 是 `ws(s)://…`）；连 game 走 WebSocket 的客户端另填 `ws(s)://主机:端口/ws`，portal 返回的区服地址只有玩家 TCP 的 `host:port`。
   - **缺省值**是第 3 节的开发地址（主机写 `127.0.0.1`），写明真机或别的机器上要改成服务器所在机器的地址。
   - 写明地址填在哪（面板入口、配置文件路径与格式示例，如 `{"portal": "http://127.0.0.1:8081"}`）、每个地址的含义与缺省值。
3. **开发环境地址与端口**：portal（account 的 `web.addr`）、玩家 TCP（game 的 `player_access.tcp.addr`，或 Gate）、WebSocket、lobby、relay；起服命令 `make dev-up && make dev-run`。
4. **开发登录的原样请求**：照第 11 步的 curl 写成可直接复制的请求（`channel` 是 `dev`，`credential` 是 `dev_token` 的值），附一份答复样例（票据用 `at2.…`、`enter:et1.…` 省略号代替）。
5. **登录到进入的时序**：第 8A 步的顺序，标明哪一步是 HTTP、哪一步是 TCP 帧；推荐用登录时带 `"enter": {}` 一步拿到进服票据与地址；答复里没有 `enter` 时改走 `/portal/servers` 让玩家选；票据过期（进服票据缺省 30 分钟）后重新登录。开房间联机写第 6 步的客户端流程与选用的 SDK。
6. **每个功能的请求、回复与推送**：每个功能一张表：消息名、`MessageIds` 常量、请求字段、回复字段、可能的错误码、推送、请求键怎么生成、什么时候重发（超时可能已经执行，按同一个请求键重发）。
7. **自身视图字段**：Player 上标了 `sync` 的字段逐个列：含义、单位、什么时候变；哪些要配合服务器时间判断“今天”；随时间恢复的数值怎么推算（第 9 步）。嵌套字段的增量要先 `Apply` 到上一份再读。
8. **错误码与提示建议**：每个码一行：名字、号、什么时候出现、建议给玩家的提示、客户端该做什么（重试 / 重新登录 / 提示后不动）。`ErrorCodes.Name(code)` / `ErrorCodes.Message(code)` 可查名字与说明。
9. **配置表怎么读**：导出了哪些表；`ConfigTables.Load(name => 读文件)` 读取（加载时核对哈希与规则）；数据版本 `ConfigTables.DataVersion` 只记录、登录时不核对；不再保留客户端自己的一份 CSV。
10. **不装 Go 与 roost CLI 时怎么拿到协议与 SDK**：
    - 协议与配置：`shared/` 已提交在服务端仓库里，直接拷 `shared/protocol/csharp/*.cs` 与 `shared/config/csharp/*.cs`、`shared/config/data/*.json` 进客户端工程；或把服务端仓库作为 git 子模块引进来，指向这几个目录。有 roost CLI 时用 `roost client sync --from <服务端工程> --to <C# 目录> --config-code-to <代码目录> --config-data-to <数据目录>`，它会核对哈希、报告消息号与错误码的变化。网页客户端用 `roost client sync --from <服务端工程> --lang ts --to <目录>` 拷 `ts/`（服务端两种语言都声明时必须写 `--lang`；配置表读取代码暂时只有 C#）。手工拷时自己核对 `manifest.json` 与上次是否一致。
    - SDK：都来自同一版本 roost-core 的 `client/`，按[客户端表](#客户端能连什么按客户端类型)用户客户端那一行写用哪几个。C# 连 game 是 `Roost.Client`、`Roost.Client.Codecs`（`client/dotnet/`，netstandard2.1）加 Google.Protobuf 3.31.1（游戏工程自己锁版本）；开房间是 `client/dotnet/Roost.Client.Relay`，网页是 `client/ts` 的 `@roost/client`（连 game 用 `gate` + `protobuf` 子路径，开房间用 `relay` / `web`；不在 npm 上：同版本 `$CORE/client/ts` 拷出后 `npm ci && npm run build && npm pack`，游戏工程引用 tgz 或 `file:`，另装 `@bufbuild/protobuf@2.17.0`）。Unity 的托管路线还要 `RoostClientDriver`（`client/unity/Runtime/`）；打成 Unity 包的入口是 [client/unity/README](../../../../client/unity/README.md)“构建可导入包”：托管路线用 `--managed-only`，只要 .NET SDK；原生路线才要 CMake、C++17 编译器与 protoc。
    - 不要手写或复制 PB 类型，不要自己再跑 protoc。
11. **最小 C# 接线**：客户端不是 C#、或连 game 不支持时，本节改写成它能走的那条路的最小接线（开房间照对应 SDK README 的示例），标题照改。C# 连 game 时对着导出的代码写，命名空间按 `client_export.csharp.namespace`（缺省协议在 `Roost.Protocol`、DAO 同步在 `Roost.Dao.Sync`、消息号与错误码在 `Roost.Generated`）。下面以只装了第 8A 步四个模块的工程为例；Player 还没有 `sync` 字段时没有 `SyncPlayerDao`，删掉第 4 步：

    ```csharp
    using System.Threading.Tasks;
    using Roost.Client;
    using Roost.Client.Codecs;
    using Roost.Dao.Sync;      // DaoSync.cs、DaoSyncApply.cs
    using Roost.Generated;     // MessageIds、ErrorCodes
    using Roost.Protocol;      // Protocol.cs

    public static class MinimalLogin
    {
        // host、port 来自 /portal/login 答复里 enter.addr；ticket 是 enter.ticket（"enter:et1.…"，原样使用）。
        public static async Task<GameClient> ConnectAndEnter(string host, int port, string ticket)
        {
            var client = new GameClient(new ConnectionOptions { Host = host, Port = port });
            client.Subscribe(MessageIds.PushEnterStatus, EnterStatusPush.Parser,
                push => UnityEngine.Debug.Log($"need_create={push.NeedCreate}"));
            client.Entities.RegisterDao("player", SyncPlayerDao.Parser,
                (patch, previous) => patch.Apply(previous));
            await client.Connection.ConnectAsync(ticket);
            client.Request(MessageIds.RequestEnterGame, MessageIds.ResponseEnterGame,
                new EnterGameRequest(), EnterGameResponse.Parser,
                reply => UnityEngine.Debug.Log(reply.Code == 0 ? $"entered first={reply.First}" : ErrorCodes.Name(reply.Code)),
                error => UnityEngine.Debug.LogError(error));
            return client;   // Unity：常驻 GameObject 上的 RoostClientDriver.Attach(client) 每帧 Pump
        }
    }
    ```

    - 回复与推送在 `client.Pump()` 里回调，Unity 交给 `RoostClientDriver`；不用 Unity 时在主循环里自己调 `Pump()`。
    - **走 WebSocket**（浏览器或客户端表里选了 WebSocket 的）：portal 进服请求带 `"transport":"ws"`，用答复里的完整 `ws(s)://` 地址，构造时把 `Host` / `Port` 换成 `WebSocketUrl = 地址`。Unity（含 WebGL）把选项包一层 `RoostPlatform.Configure(new ConnectionOptions { WebSocketUrl = 地址 })`：WebGL 上它设 `PumpDriven = true` 与 Unity 包里的浏览器 WebSocket 桥接，其他平台原样返回；照旧 `RoostClientDriver.Attach(client)`（它每帧先轮询桥接再 `Pump`），`ConnectAsync` 在主线程上调；其余代码不变。不要从 `unity-webgl-smoke` 拷桥接代码，Unity 工程重新执行 `scripts/client/build-unity.py` 就有。
    - `EnterGame` 答 `enter_need_create` 时先发 `EnterCreate{Name}` 再发一次 `EnterGame`。
    - **网页（TS）**：本节改写成 TS 最小接线，标题改“最小 TS 接线”，对着导出的 `ts/` 写；`entry` 是 portal 进服请求带 `"transport":"ws"` 的答复：

      ```ts
      const client = new GameClient({ webSocketUrl: entry.gate, autoReconnect: true });
      subscribe(client, MessageIds.PushScore, ScoreNoticeSchema, n => show(n.score));
      await client.connection.connect(async signal => (await portal.enter(signal)).ticket);
      request(client, MessageIds.RequestPing, MessageIds.ResponsePing, PingRequestSchema, { nonce: 1n }, PingResponseSchema, ok, fail);
      ```

      int64 字段是 `bigint`；自身视图同步还没有，Sync 推送进 `unhandledPush`；见 [client/ts/README](../../../../client/ts/README.md)。
    - 重连不自动重发业务请求；回到前台要重新取票登录；完整说明见 [client/README](../../../../client/README.md)、[client/unity/README](../../../../client/unity/README.md)。
    - 写进 CLIENT 文档前，把例子里的消息名换成本工程 `MessageIds.cs` 里实际有的常量。

### DEMO-HISTORY.zh-CN.md

每一轮追加一节，第一次生成也算一轮：日期、模式、用户原话（照抄）、开场五个问题的答案；本轮新增 / 修改 / 删除的条目（按 DEMO 编号）；定下的决定（专业模式的回答、业余模式替用户定的项）；冲突与用户的选择、待确认项；改了哪些文件、新分配或弃用的号与存储键；验收结果（新场景与回归场景分开报数）与起服冒烟结果；更新检查的结果（入口与 roost-core 有没有新版、用户怎么选）；照 skill 做时走不通、自己绕过的地方。历史只追加，写错了追加一条更正。

### 给之后接手的 agent

- `.claude/skills/roost-genre/SKILL.md`：入口 skill，从 GitHub 下载 `https://raw.githubusercontent.com/tjbdwanghaibo/roost-core/main/claude-skill/roost-genre/SKILL.md` 整份放入，不改内容。用 Codex 等读 `.agents/skills/` 的工具时同一份再放到 `.agents/skills/roost-genre/SKILL.md`。
- `CLAUDE.md`：只写一行 `@AGENTS.md`。
- `AGENTS.md`：按下面的骨架写，尖括号换成实际内容。已有 `AGENTS.md` 的不整份覆盖，只补“需求怎么做”一节。

```markdown
# 编码助手指引

本工程是用 roost-core <版本> 生成的游戏服务端（Go）。<一句话：做什么游戏、对接哪个客户端工程、客户端工程的路径>。

## 需求怎么做

- 新增或修改业务需求，一律先用 `roost-genre` skill（入口在 `.claude/skills/roost-genre/SKILL.md`）：它会检查更新、读本工程的记忆、拆需求与分端、查限制，再改工程。
- 记忆：`docs/DEMO.zh-CN.md` 是当前状态，`docs/DEMO-HISTORY.zh-CN.md` 是逐次变更（只追加）。动手前先读这两份。客户端对接看 `docs/CLIENT.zh-CN.md`。
- skill 只读：不改 `.claude/skills/`、`.agents/skills/` 下的文件，也不改 Go 模块缓存里的 roost-core。发现 skill 有问题告诉用户。
- roost-core 的版本以 `go.mod` 为准，未经用户同意不升级。

## 目录

- `game/components/<功能>/`：组件（`//roost:component` 类型号与工厂登记、业务方法、`//roost:listen` 监听）；模块之间的联动 `<功能>_<对方>.go` 也在对应功能的包里。
- `game/view/`：组件接口与 `<功能>Holder`（手写），实体总接口 `<实体>Entity` 与窄接口 `I<功能>Entity`（生成）。
- `game/event/<功能>.go`：实体事件定义，只放值类型。
- `game/entities/<实体>/`：实体本体、生成文件、手写的宿主方法 `<模块>.go`；不放组件，不声明组件类型别名。
- `game/modules/<包>/`：功能包（模块带来的，与手写的玩法规则、状态机）；`game/modules/login/` 是登录链与进入游戏的登记点。
- `game/managers/managers.go`：进程内 manager 的登记点；`game/handler/`：Nest handler；`game/controllers/`：协议入口，一个包，每个功能一个 `<功能>.go`。
- `services/<托管服务>/`：托管框架服务的手写代码（collaborators、登录渠道、`services/account/portal/` 登录入口）；`internal/` 是生成的框架接线。
- `shared/`：给客户端的导出目录（协议、配置表、`README.md` 即客户端对接文档），要提交。

## 工程约定

- 业务状态在实体的 DAO 里，改状态只在 Nest handler 里；不改带 `_gen` 的文件；协议号、错误码、组件号、`store` 键分配后不改、不复用。
- 已装模块（`roost.yaml` 的 `modules` 段）的文件是业务代码，改之前读工程里现在的版本。
- 玩家操作在提交之后用框架的日志（`log/slog`）打业务日志，高频逻辑可以不打。

## 验证

- `roost module verify`（含 build、vet、`roost id check`、`roost generate --check`），`go test ./...`，全程 `GOWORK=off`。
- 改了行为要有场景测试；回归时以前的场景一条不能少。没有跑的验证写明“未验证”。
```

**完成标准**：这些文件都在；`cmp docs/CLIENT.zh-CN.md shared/README.md` 没有输出；DEMO 第 6 节的哈希与 `shared/protocol/manifest.json` 一致。

## 13. 报告

**开头固定三行白话**，不用术语：

```text
现在能做什么：<例如：玩家能登录、建角，每天签到领金币，金币和签到记录存在服务器上。>
怎么亲眼看到：<例如：make dev-up && make dev-run，然后按 docs/CLIENT.zh-CN.md 第 4 节的 curl 登录；客户端接好后在游戏里签到。>
上线前还差什么：<例如：接渠道登录验签、加 HTTPS 与限流、改生产地址；签到奖励数值是样例。>
```

然后是**交付摘要块**（格式见主 SKILL 的[与其他 agent 对接](../SKILL.md#1-与其他-agent-对接)），被别的 agent 调用时它是给调用方的全部交接。

技术细节放在后面：

1. **需求对照表**：每条一行：服务器部分、客户端部分、结论（直接装 / 装了再扩展 / 借鉴 / 手写 / 做不了）、模块或样板、状态（已做 / 等用户决定 / 没做）。
2. **限制检查**：每个冲突（需求原话、限制与出处、用户的选择或“按推荐，待决定”）；没有冲突写“已对照模块能力边界、能力清单与已知限制，未发现冲突”。**用户还没表态的冲突不能出现在“已做”里。**
3. **装了哪些模块**：名字、各自通过的场景数、追加的 DAO 字段、分配的协议号与错误码。
4. **手写了哪些功能**：文件、场景数、分配到的号（组件号看 `//roost:component`，错误码看 `errcode.Define`，协议号看 `//roost:msg`）、业务假设（数值、日界、上限都是样例）。
5. **没做的**：框架不支持或只有设计的（给出处）；需要用户决定的。
6. **验收到哪一步**：`module verify` 与手写场景的实际输出；起服冒烟的结果或“未做”与原因；明确写“离线场景用替身提交器；握手之后的业务请求<是否>经真实连接验证”。
7. **日志与登录**：哪些玩家操作打了日志、在哪个文件；登录链路用的是哪套、开发配置写了什么、上线前还差什么。
8. 业余模式加一句“以下几项是我替你定的”，列出拆分、样例值、复用结论与归属。

**命令失败时**按三样报告：原命令（照抄）、一句白话原因、下一步（你打算怎么做，或需要用户做什么）。例如：“`make dev-up` 失败：这台机器的 docker 没在运行。下一步：请启动 docker 后告诉我，我再做起服冒烟；其余验收已完成。”

临时目录用完即删，确认没有遗留的测试进程与 `make dev-run` 起的服务。

## 14. 已有工程上增改需求

工程生成之后需求会不断增改。每一轮按这个顺序，不从头生成：

1. **先读记忆**：`AGENTS.md`、`docs/DEMO.zh-CN.md`、`docs/DEMO-HISTORY.zh-CN.md`、`docs/CLIENT.zh-CN.md`、`roost.yaml`（专业模式再读 `docs/DECISIONS.zh-CN.md`）。记忆与工程对不上时（有人手改过），以工程为准，先把差异告诉用户，再把文档改成与工程一致。
2. **分清这一轮是什么**：新增、修改（指出改的是 DEMO 的哪一条）、删除。只对这些条目走第 4 步；专业模式只就这些条目和受影响的旧条目追加决策单。
3. **查与旧需求的关系**：新条目与已做的互相矛盾、或会改变已做条目的行为时，按冲突提出，不悄悄改掉旧行为。以前“接受限制”的条目，限制因这一轮变得更严时也要提。
4. **修改与删除的额外限制**：
   - 号与存储键不改、不复用：只能加新的；旧的协议号、错误码、`store` 键弃用但保留，记进文档。
   - 模块不能卸载：停用模块提供的功能是去掉入口并在文档里标“已停用”，状态与号留着；用户坚持彻底删，按 `roost.yaml` 的 `modules.<name>` 清单手工删并说明风险。
   - 改持久化状态的含义（改名、单位、拆分合并）：先问工程是否已上线、有没有真实数据。已上线的按 [schema 变更手册](../../../framework/SCHEMA-MIGRATION.md)离线迁移，这是冲突，提出来；未上线的直接改，历史里写“未上线，不做迁移”。
   - 已装模块的文件可能被上一轮改过：改之前读工程里现在的版本。
5. **改完照第 10 步验收，并重跑以前所有的场景**（DEMO 第 8 节有清单）。以前的场景变红时，先判断是旧需求被这一轮改了（照用户的决定改测试，写进历史），还是改坏了（修代码）；不能为了变绿删旧场景。再做一次第 11 步起服冒烟。
6. **更新记忆**：DEMO 改成改完后的当前状态；HISTORY 末尾追加一节。
7. **通知客户端**：改了 `protocol/def`、DAO 的 `sync` 字段、错误码或导出的配置表时，先 `roost generate`，`project doctor` 的 `client-export:fresh` / `config-export:fresh` 是 OK；改 CLIENT 文档并同步复制到 `shared/README.md`；HISTORY 本轮写“客户端要重新同步”，列出新增和修改的消息号与错误码（可贴 `roost client sync --dry-run` 的输出）、新的 manifest 哈希、配置数据版本“旧 → 新”。只改只给服务端的列时数据版本不变，不用通知。

第 13 步的报告只讲这一轮，新场景与回归场景分开报数。

**完成标准**：第 10 步的命令全部通过，回归场景数不少于上一轮 HISTORY 记的数；DEMO、CLIENT、`shared/README.md`、HISTORY 已更新。

## 附录 A：写代码时查

### A1. 框架的核心编程思路

三块基础各回答一个问题：Nest 决定何时、在什么锁下修改实体；DataEngine 决定修改怎么落盘与恢复；Sync 决定哪些已提交的状态给哪些客户端看（[总览](../../../framework/guide/00-overview.md)）。

| 做法 | 含义 | 出处 |
| --- | --- | --- |
| 状态在实体的 DAO 里 | 挂在已有实体上的状态是 DAO 的嵌套结构体；不挂在玩家身上的用独立实体。不用进程级全局变量存业务状态 | [Nest 与实体](../../../framework/guide/02-nest-entity.md) |
| 改状态只在 Nest handler 里 | 一条请求是一笔事务；状态只经生成的 setter 改；回滚统一走 DAO | 同上 |
| 同一个实体上的协作直接调组件 | 依赖的组件在同一个实体上时直接调（同一把锁、同一笔事务）；不在同一个实体上用多实体 handler；跨进程用 Remote / RPC / Saga | [Remote](../../../framework/guide/05-remote-mirror.md)、[Saga](../../../framework/guide/06-saga.md) |
| handler 里不等待 | 不等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体 | [Nest 三池与 Await](../../../framework/NEST-AWAIT.md) |
| 落盘交给 DataEngine | 不自己写库。请求返回的错误分清“明确被拒”与“结果未知”，结果未知不能当成没发生 | [DataEngine](../../../framework/guide/03-dataengine.md)、[回复错误判别](../../../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试) |
| “只做一次”记在自己的状态里 | 首次发放、每日领取、订单到账：先判自己状态里的事实，再发放 | 第 9 步 |
| 客户端看到的是已提交的状态 | 给客户端看的字段标 `sync`，提交后由 Sync 下发；协议推送只用于通知 | [Sync](../../../framework/guide/04-sync.md) |
| 时间用业务时间 | handler 最后一个参数 `nowUnixMilli`；“今天”用业务日；随时间恢复的数值惰性结算；到点要做的事用实体的持久定时器 | [时间](../../../framework/guide/10-time.md) |
| 身份取自会话 | 玩家是谁取已认证的连接，不取请求字段 | [玩家接入](../../../framework/PLAYER-ACCESS.md) |
| 号与存储键稳定 | 协议号、错误码、组件号、`store` 键分配后不改、不复用；不改 `_gen` 文件 | [tag 手册](../../../../codegen/docs/TAGS.zh-CN.md) |
| 业务拒绝用错误码 | 从 `internal/errors` 返回带码的错误，端点转成应答里的 `Code` / `Reason` | 第 9 步 |

拿不准时读表里的出处。现有模块都按这些做法写，所以“照最接近的模块写”通常就是对的。

### A2. 组件的写法：组件包 + game/view + 实体事件

**每个实体的每个组件都写在自己的包 `game/components/<功能>/` 里，不写进实体包**（Player 与别的实体一样，模块自带的实体也一样：dungeon 的 Dungeon 是 `game/components/dungeonrun`，terrain 的是 `terrainchunkdata` / `terrainsite`）。组件的类型号与工厂登记也在组件包里；实体包只剩实体本体、`_gen` 文件、组件要实体手写提供的宿主方法（`<模块>.go`，每个模块一个小文件），以及只属于实体本身的东西（实体号与坐标换算、同步打包器、镜像、实体对框架接口的实现、生成的定时器组件与它的登记）。实体上的业务规则也写成组件；唯一的例外是只对这一种实体生效的事件监听可以写成实体方法。样板：只挂在 Player 上的看 `modules/profile`、`modules/task`；别的实体看 `modules/alliance`、`modules/room` / `turn` / `matchroom`、`modules/team`；多个实体共用的看 `modules/bag`、`modules/wallet`、`modules/attribute`、`modules/activity`。

生成组件包用 `"$ROOST" add component <Feature> --entity <Entity>`（写出的文件与之后要手写的部分见第 9 步第 2 条），不要手写组件包骨架。

**依赖方向**（谁 import 谁）：

```
game/event                ← 最底层：只放事件值类型；只依赖标准库与 db
game/view                 ← 只放接口；只依赖 framework、db、协议类型
game/components/<功能>    → game/view、game/event（以及它联动的别的组件包）
game/modules/login、联动  → game/view、game/event、组件包
game/entities/<实体>      → 组件包、game/view、game/event、game/modules/login（实体实现 view 的接口）
game/handler、controllers → game/view、组件包（要实体的具体类型时才 import 实体包）
```

组件包、`game/modules/login` 与联动永远不 import 实体包；要实体类型号这类常量时在组件包里按同一个号再声明一次。

1. **组件包** `game/components/<功能>/<功能>.go`：`//roost:component type=<号>` 与 `const CompType<功能> entity.ComponentType = <号>`（同一个组件挂在几种实体上只用一个号）、`init()` 里的 `entity.RegisterComponentFactory(CompType<功能>, NewComponent)`、`Component` 类型、宿主接口 `Owner`、工厂 `NewComponent(value any, *entity.EntityCreateParam)`（把 `value` 断言成 `Owner`，失败返回错误）。实体结构体直接挂 `bag *bag.Component \`comp:"bag.CompTypeBag"\``；不在实体包里声明 `type BagComponent = bag.Component` 这类别名（组件带 `//roost:listen` 时生成会失败）。`Owner` 只列组件用得到的东西：

   ```go
   // 多个实体共用：只要本实体 DAO 里的那份状态（宿主方法写在 view 的 Holder 里）
   type Owner interface {
       entity.IThreadSafeEntity
       view.BagHolder // Bag() 访问器 + 宿主方法 BagState() *db.BagState
   }
   // 只挂在 Player 上、要用钱包：
   type Owner interface {
       entity.IThreadSafeEntity
       Dao() *db.PlayerDao
       view.WalletHolder
   }
   // 组件要访问整个宿主时：
   type Owner = view.PlayerEntity
   ```

   `Owner` 里有 `entity.IThreadSafeEntity` 就带着 `Publish`，组件拿宿主就能发实体事件。宿主能力不同的地方用可选接口区分（只有 Player 有持久定时器，就定义 `deadlineOwner`，组件里类型断言），不在组件里按实体类型写分支。定时器组件（`roost add timer` 生成）是实体的基础设施，它的回调入口留在实体包、只有一句：把到期节点交给组件的导出方法（`turn` 的 `OnDeadline`）；建 / 删定时节点写成实体上的宿主方法交给 `Owner`。
2. **game/view**：每个组件一个 `game/view/component_<功能>.go`，三样东西——组件对外的接口 `<功能>`（`*Component` 的导出方法都列进来；框架回调与 `//roost:listen` 方法不列）；签名用到的参数 / 结果结构体（以功能名作前缀，只含内建类型、db 与别的 view 类型）；“带有这个组件”的接口 `<功能>Holder`（访问器 `<功能>() <功能>` 加组件要实体提供的宿主方法）。`roost generate` 生成实体接口 `<实体>Entity`、实体上的访问器、每个 Holder 的窄接口 `view.I<功能>Entity`（给 handler 当参数类型），以及只交出 DAO 某个字段的宿主方法（`<实体>_gen_wire.go`）；其余宿主方法手写在 `game/entities/<实体>/<模块>.go`，漏写时编译期断言报错并列出要写的方法。要取回具体组件时用组件包自己的 `Of(holder)`。
3. **跨组件调用写访问器**：`host.Wallet().Add(n)`、`c.owner.Bag().Remove(id, 1)`。组件、监听与联动只收用得到的最小接口：只要背包的收 `view.BagHolder`，要整个玩家的才收 `view.PlayerEntity`。
4. **实体事件，不写钩子登记表**：发出“发生了什么”的组件在 `game/event/<模块>.go` 定义事件（导出结构体，只放值），做完后 `c.owner.Publish(event.<X>{...})`，错误原样返回。监听写成监听方自己组件上的方法：

   ```go
   //roost:listen
   func (c *Component) OnCheckinClaimed(ev event.CheckinClaimed) error {
   	return c.Report("checkin.claimed", 1, ev.NowUnixMilli)
   }
   ```

   - 签名固定：一个 `game/event` 结构体值参数、返回 `error`，方法名 `On<事件名>`；`roost generate` 登记到挂这个组件的每种实体上。顺序有要求时写 `//roost:listen priority=<N>`（大的先）。
   - 同一实体、同一笔事务、立即执行：监听里只改同一个实体，不做 I/O；改别的实体走 Nest 发送，跨进程走 `nest.Emit`。
   - 监听里再 `Publish` 返回的错误必须原样返回（没有保存点）。拦截类事件（`TeamJoining`、`RoomJoining`、`ProfileRenaming` 等）的监听只检查、不改状态。
   - 事务之外的登记点（`gamelogin.OnEntering` / `OnEntered` / `OnCreating` / `OnCreatePrepare`、rehome 的 `OnApplying` 等）与扩展提供方不是实体事件，照旧在 `init` 里登记。
   - 契约见 [Nest 与实体 §3A](../../../framework/guide/02-nest-entity.md#3a-实体事件)。
5. **共用组件的状态类型共用，每个实体各存一份**：同一个嵌套结构体放进各自的 DAO，各自落盘、各自回滚。两个实体之间搬东西仍走多实体 handler、Remote 或 Saga。
6. **测试**：每个宿主至少一条场景；共用组件另加一条证明两个宿主的状态互不影响。组件包里的测试不能 import 实体包（会成环），要实体的测试放 `game/handler`。

已装的模块要挂到第二个实体时，先看它 README 的“复用方式”：写明支持的照做；只支持 Player 的按本节借鉴，不改模块原有的 Player 接线。

### A3. 进程内的全局逻辑：manager，还是挂在 World 上

按**这份状态要不要跟业务事务一起提交、回滚**来选，不要默认新建单例实体：

| 需求 | 放哪里 | 依据 |
| --- | --- | --- |
| 缓存、索引、路由表、注册表、丢了能重建的东西；进程内的后台循环 | **manager**：实现 `app.IManager`（Start / Stop），登记在 `game/managers/managers.go` 的 `Managers()` | 不进事务、自己加锁；按 `DependsOn()` 启动、反序停止 |
| 全服状态，要和玩家修改同一笔事务成败（全服限量、全服计数） | **World 上加组件**，状态放 World 的 DAO（组件写法见 A2） | 走 Nest 的锁与事务；World 每个进程唯一、`Service.Init` 里已确保存在 |
| 同上，但不需要落盘 | 同上，字段标 `dao:"nopersist,nosync"` | 只在内存 |
| 很热：几乎每个请求都要写 | **先不要放进单个实体** | 一个实体同一时刻只有一笔事务拿到锁。能按 key 拆就拆成多个实体，或 manager 里攒着定时批量提交，写进文档的限制 |

不另建“全服单例实体”：ID 怎么定、多进程共库会不会写成两份、创建顺序，World 已经解决了。确实需要时按冲突向用户说明理由。

- 全部 Mod 先启动（manager 在这一步 Start），之后才执行 `Service.Init`（World 在那里建出或载入）。所以 **manager 的 Start 里不能用 World**。
- 起服就载入本服已有的某类实体：实体注解写 `preload=startup`（可加 `preload_order=<N>`，小的先）；它只载入已存在的文档，不创建实体。要一直在内存里写 `lifetime=resident`（隐含预加载，框架拒绝只从内存卸载它），在报告里写明内存估算。规则见 [Nest 与实体“起服预加载与按服分库”](../../../framework/guide/02-nest-entity.md#起服预加载与按服分库v1470-起)。
- DAO 的 `dbscope`：归某一个服的本地实体用 `dbscope=sid`（`add dao --entity` 的缺省）；各服共享的（Remote 托管实体、跨服共享的配置性数据）才用 `dbscope=global`；预加载要求每个 DAO 都是 `sid`。拿不准按 sid，并写明换服 / 合服时怎么搬。

### A4. 配置表：数值写进 configs/table，两端共用一份

**规则**：数值（奖励、曲线、上限、倍率、掉落、价格、冷却、概率）默认写进 `configs/table`，不写成 Go 常量；只有协议约束（号、长度上限）写成常量。服务端工程是唯一源头：同一份 schema + CSV，`roost generate` 同时产出服务端的 JSON 与 Go 绑定、客户端的 JSON 与 C# 读取代码（[命令参考 §2.2](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#22-配置表导出给客户端client_exportconfig)）。样板：`bag` 的道具表（`configs/schema/item.go` + `configs/table/item.csv`，组件里 `generated.ItemByID(id)`）、`shop` 的商品、`task` 的任务目录。

- `"$ROOST" add table <Name>` 建 schema（`//roost:table`，单例 `//roost:object`），CSV 写在 `configs/table/<name>.csv`（第一行列名必须有，`#` 开头是注释）。业务代码读 `generated.<Type>By<Key>(id)`、`generated.<Type>Table()`，不自己读文件。
- 规则写在字段 tag 上：`required`、`unique`、`min`、`enum`、`ref`，服务端加载与客户端 `Load` 都查。
- **按端分组**：字段 `side:"s"` 只给服务端，`side:"c"` 只给客户端；整张表只给一端时标记里写 `side=c` / `side=s`。主键不能带 side，`ref` 必须指向同一端的表。
- 一组散的数值写成竖排键值表 `//roost:object name=<名> layout=kv`（CSV 三列 `key,value,note`）。
- 按非主键查一组行时字段写 `index:"true"`。字段类型用内建类型；具名类型、结构体、`time.Duration` 导不到客户端。
- 场景测试从测试数据读：照模块安装写的 `roost_config_data_test.go` 在测试 `init` 里装载 `configs/data`，断言把需求给的数写死。
- 导出给客户端见第 7 步的 `client_export.config`；客户端怎么读写进 CLIENT 文档第 9 节。

**已有工程的迁移**（数值原来是 Go 常量，或客户端另有一份 CSV）：建表搬值并分端 → 代码改读 `generated.<Type>…`、删常量 → 测试改从测试数据读、重跑以前的场景 → 打开 `client_export.config`、`roost generate` → 客户端同步、改读 `ConfigTables` 后删掉自己的 CSV。改已有表的含义按第 14 节“改持久化状态的含义”先问。

### A5. 日志

规则：**玩家相关的操作要有业务日志；高频逻辑可以不打；只用框架的日志。**

- **用什么**：标准库 `log/slog`（框架在启动时装好输出：级别、JSON、落盘与轮转由服务配置的 `log:` 段决定），或框架的 `infra/observe/log`。不用 `fmt.Print*`、标准库 `log`，不引入别的日志库。
- **记什么**：改了玩家状态、以后要能回答“这名玩家什么时候做了什么”的操作：建角与进入、货币与道具增减（数量、来源）、购买与发货、领奖、改名、关系变化、换服、运维命令写玩家。每条带 `player_id` 与关键事实，用键值对。
- **在哪里记**：请求返回之后——端点里 Nest 请求成功返回时，或服务端调生成的 Sender 返回之后。handler 里打的日志在回滚时也会留下，所以 handler 里只记拒绝或出错的原因。
- **级别**：成功用 `Info`；正常的业务拒绝不用打；不该发生的用 `Warn` / `Error`。
- **不记**：移动、心跳、每帧推进、视野、定时器每次触发、只读查询（用指标，[可观测性](../../../framework/guide/11-observability.md)）。
- **不能记**：票据、令牌、凭据、密钥，哪怕是 `Debug`。

### A6. 让客户端看到状态

**装了 `enter` 模块就不用做**：安装器已打开自身视图，doctor 里有 `OK self-sync`（Player 上还没有 `sync` 字段时是一行 `WARN self-sync`）。没装 enter 时，标了 `sync` 的字段要打开玩家自身视图才会推给客户端：

```bash
"$ROOST" add transport tcp                 # 玩家 TCP 接入与 auth.go；access.player.transports 多出 tcp
# 在 roost.yaml 的 access.player 下加一行 self_sync: true（与 transports 同级）
"$ROOST" project sync
"$ROOST" project doctor || true            # 应出现一行 OK self-sync
```

之后 Player 主 DAO 里标了 `sync` 的顶层字段都推给玩家本人，不用写打包器；说明见 [PLAYER-ACCESS §7](../../../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)。真实客户端要连上还要实现 `internal/access/player/tcp/auth.go`（缺省拒绝一切）并 `"$ROOST" config enable player-tcp`；`"$ROOST" project doctor --workflow player-tcp` 列出剩下的项。

### A7. 进入游戏：建档与首次初始化

**装 `enter` 模块即可**：它带 `EnterGame` 端点和首次初始化，新玩家的初始发放写进 `game/modules/login/enter_first.go`（例如 `host.Wallet().Add(100)`），或在自己的组件上监听事务内的实体事件 `event.EnterFirst`。玩家进入时顺带做的事（上线登记、拉离线数据）不改进入端点，在自己的包里登记：

```go
func init() {
	gamelogin.OnEntered("<功能名>", func(ctx context.Context, registry *app.Registry, entered gamelogin.Entered) error {
		// entered.PlayerID、entered.First（是否首次进入）、entered.FirstAtUnixMilli
		return nil
	})
}
```

（`gamelogin` 是 `<module>/game/modules/login`。）这个登记点在事务之外，失败只记日志、下次进入再调；必须成功的写在 `enter_first.go` 或监听 `event.EnterFirst`。

**两条路**：用进服票据握手的会话（装了 login），玩家没建成之前 `EnterGame` 答 `enter_need_create`、什么都不写，客户端先发 `EnterCreate{name}`，建档、名字、首次初始化、“建成了”的标记同一笔事务。别的会话 `EnterGame` 加载或创建并做首次初始化。两条路的首次初始化是同一段代码。要在新建这一步加检查或写入，用 enter 的登记点（`OnCreating`、`OnCreatePrepare`）或监听 `event.EnterCreate`，不改它的端点。

不装 enter 时的手写做法：Nest handler 的目标必须是已存在的实体，所以客户端登录后的第一条请求必须是一个“进入游戏”端点，由它加载或创建 Player（没有它，场景测试照样全绿，真实进程里每条请求都答 `entity_not_found`）：

```bash
"$ROOST" add protocol EnterGame --group game --handler player
"$ROOST" generate
"$ROOST" add endpoint EnterGame --handler player --lifecycle Player
"$ROOST" generate
```

新玩家的初始发放不按“这次是不是新建的”判断（建档后、发放前进程崩了就永远拿不到）：写成不对客户端开放的 Nest handler，“发过没有”记在自己的状态里，每次进入都经 Sender 调一次。

### A8. 不装任何模块时的手工接入（专业模式）

只在开发者明确不要登录模块时用。没有模块替你接玩家时，`add endpoint` 与 `add transport tcp` 会被拒绝，先：

```bash
"$ROOST" add access player --service game
"$ROOST" add transport tcp
```

然后按 A6 打开自身视图、按 A7 写进入端点，自己实现 `internal/access/player/tcp/auth.go` 的鉴权（缺省拒绝一切；`ROOST_PLAYER_TOKEN` 只供 `cmd/playerprobe` 做最小验收，不是生产鉴权），再 `"$ROOST" config enable player-tcp`。登录入口、区服、票据都要自己做，写进决策单与报告的“没做的”。

## 附录 B：边界速查

- 模块清单以 `roost module list` 为准。模块之间只自动接上 `with` 声明的联动；其余要自己写：升级奖励（监听 `event.ProfileLevelUp`）、新玩家初始发放（`enter_first.go` 或监听 `event.EnterFirst`）、自定义任务事件（调 `Task().Report`）、何时上榜（调 `leaderboard.Submit`）、什么操作扣体力（调 `Stamina().Spend`）。装了 shop 要把样例商品目录换成自己的。
- 没有卸载；分配过的号与存储键不回收。已装模块不会随 roost-core 升级自动更新。
- 模块组合之后的行为（联动没覆盖的那些）没有人替你验证过，要自己写场景。Player 主 DAO 的顶层字段有上限，完整 game-demo 已到上限，不能再叠装 activity（KNOWN-LIMITS“业务模块”）。
- 独占新服务的模块（alliance）与带 Saga 的模块只在新生成或结构相同的工程上验证过；已有工程先 `module plan` 看它要动什么。
- **玩家接入是 RS v2 帧，走 TCP 或 WebSocket**。HTTP 不是玩家协议：`roost add webroute` 的路由拿不到玩家身份、不做鉴权、缺省只绑回环地址，只适合管理面或鉴权代理之后；需求要求玩家走 REST 或 WebSocket 上别的消息格式时按冲突提出。portal 的 HTTP 端点只负责登录、选服、换票，lobby 的只负责建房与加入。服务端能接什么与客户端能连什么是两回事，客户端一侧以[客户端表](#客户端能连什么按客户端类型)为准。
- **一个账号在一个区服只有一个角色**（account 的建角约束，[服务篇 §4](../../../framework/guide/09-services.md#4-关键业务边界)）；需求要“同服多角色”时按冲突提出。同一个人在不同游戏（`account.games` 的不同条目）里是不同的账号号。
- **lobby + relay 的多进程演练只在单机做过**（account、lobby、两个 relay 与 Go SDK 玩家进程）：跨机、两个 lobby 实例、容量没有验证；relay 进程崩溃时房间丢失。
- **独立 Gate 没有跨机验证**。
- 登录链路的单机真实脚本是 `scripts/test-demo-login.sh`（game-demo 加 casual + naming 阶段：双 account、双 game 服、双 Gate、重启后重连）；跨机与别的配方不在范围内。
- 细节与出处：[modules/README](../../../../modules/README.md)、[命令参考“业务模块”](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module)、[KNOWN-LIMITS](../../../maintenance/KNOWN-LIMITS.md)“业务模块”。

## 附录 C：维护者用本地检出时

维护者在 roost-core 检出里验证 skill 或改模块时，CLI 与依赖都跟检出走（仓库脚本也是这样）：

```bash
CORE=<roost-core 检出路径>
(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost); ROOST="$WORK/roost"
"$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game --skip-deps
cd "$DIR" && go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
```

`--skip-deps` 之后先加 replace 再 add / generate，否则 `go mod tidy` 会去解析可能未发布的版本。刚建完、还没装模块时 `project doctor` 会报一串 `compile:*` FAIL（依赖还没整理），第一次 `module add` 或 `roost generate` 之后消失。
