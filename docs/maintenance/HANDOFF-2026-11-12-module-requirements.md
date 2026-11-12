# 业务模块后续需求（登录线之外，2026-11-12）

维护者 2026-11-12：“除了 login，其他的整理成需求文档，我让另一个 agent 处理”。本文是给接手 agent 的需求清单：现有 32 个模块之外还要做什么、每一项要达到什么、怎样算做完。它接在[待办交接](HANDOFF-2026-11-11-module-backlog.md)之后——那份文档的规矩（§3）继续有效，待办（§4）以本文为准。

## 0. 目标与验收口径

最终目标不变（待办交接 §0）：从空的 `-template game` 工程装几个模块，就得到能 build、能跑、覆盖“登录 → 进入 → 选中的玩法”的半成品，几乎不用手写胶水。

每一项需求做完的口径：

- 全新 `-template game` 工程**不做任何配置**直接 `roost module add <模块>` 能用，`roost module verify` 场景数对上；与全部现有模块正序、反序同装通过；重复安装无变化。
- 场景测试不用子测试、数字写死、只断言自己的事实；每条规则做过变异自检并在报告里列出。
- 模块可以做得完整复杂，但简单用法必须照样能用（维护者：“模版可以复杂，能兼容简单就可以”）。demo 比模块多的能力搬进模块，demo 只留规则数据与确属它独有的玩法。
- 不为以前生成的工程做兼容（未上线）。DataEngine / Nest / Sync 三块基础不为这些需求改动；确需改动先报告维护者。
- 需要真实依赖才能证明的（跨进程、saga、定时器落库）写明“只有离线场景”，不把替身测试说成生产验收。

## 1. 分工

| 归谁 | 范围 |
| --- | --- |
| **原会话（不要动）** | 登录线：模块 login、portal、servertime，game-demo 与两个配方换用它们，客户端登录链路的真实演练；给 agent 用的拼装 skill 的最终重写 |
| **接手 agent（本文）** | 下面 §3～§6 的全部需求 |

登录线的工作在分支 `tmp/mod-portal`（worktree `/Users/whb/roost/worktrees/mod-portal`，远端 `feat/business-modules-portal`），**还没有合进 `feat/business-modules`**（维护者：主线等最终 skill 也加完后再合并）。它改了这些公共文件，接手 agent 在主线上改同一处时只加自己的行，合并时取并集：`scripts/test-modules-generated.sh`（`all_modules` / `all_counts` 加了 portal，login 的场景数 19 → 22）、`modules/README.md`、`docs/framework/CAPABILITIES.md`、`docs/maintenance/KNOWN-LIMITS.md`、`CHANGELOG.md`、`codegen/internal/roost/module_install.go`（`moduleCanAddMod` 认 web）、`module_spec.go`（清单新字段 `next`：安装后提示）。接手 agent 的模块需要“安装后提示”或 web Mod 代办时，等登录线合入后再用，或先写进模块 README。

## 2. 验证节奏

维护者 2026-11-12：“测试最终再跑，我还会继续开发”。每一步只跑与改动直接相关的检查（模块自己的 `roost module verify`、`go test -run 'TestModuleCatalog|TestModuleWith' ./codegen/internal/roost`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`）。完整测试（`scripts/test-modules-generated.sh`、`TestDemo|TestModule`、重新生成 game-demo 跑全量、全仓 `go test ./...`）留到最后统一跑一次；没跑的在提交说明与报告里写明。

## 3. 第一优先：让拼出来的工程“能运营”

### R1. gm（运维命令）

已完成（提交 `8a49fe25`）：实现与定向验收完成，GM 9 条、五组联动各 4 条，8 个变异与定向 race 通过。demo 通用命令迁入模块，双实体经验事务保留；[验收与边界](evidence/module-gm-20261112/README.md)。全模块正反序新装与 §6 真实依赖仍按 §2 留到最后。

- **要解决的**：拼出来的工程没有办法发道具、加经验、发补偿邮件、查玩家；这些现在只在 game-demo 的 `internal/service/game/gm.go` 里手写（`gm.player.add_item` / `add_exp`、`gm.mail.send`、`gm.player.rehome`、`gm.world.stats`、`gm.saga.get|list` 等）。
- **形态（按推荐项做，待维护者确认）**：一个 `gm` 模块提供登记点与 ops `/admin` 的接线（命令名、参数解析、鉴权沿用 ops 的 admin token 规则）；各功能模块用联动文件（`with/gm/`）登记自己的命令——装了 bag 才有加道具，装了 mailbox 才有发补偿邮件。不做“一个模块里写死全部命令”。
- **需求**：命令经 Nest 的正式 handler 落到实体上（不绕过事务）；每条命令带操作者备注并记日志；命令的拒绝有带码错误；开发 token 在生产配置下被拒（沿用 ops 的规则，不另起一套）。
- **首批命令**：wallet 加减币、bag 加道具、profile 加经验、mailbox 发补偿邮件（单人）、rehome 发起换服（已有 `gm.player.rehome` 的写法）。
- **验收**：只装 gm 时零命令也能启动；每组联动各有场景（命令成功、参数非法零写入、目标不存在、提交失败回滚）；game-demo 的 gm.go 里对应命令改为由模块提供。

### R2. purchase（充值发货）

维护者补充（2026-11-12）：购买不默认用 Saga。普通金币购买走单 Player Nest 事务；充值到账走平台订单与补发游标。只有涉及多个独立提交方、确需失败补偿的复杂交易才使用 Saga；本地多实体事务能覆盖的也不引入 Saga。

已完成（提交 `cc0d7855`）：实现与定向验收完成：基础 21 条、bag 4 条、enter 2 条；新工程 72 PASS / 0 SKIP，13 个变异、两轮 race 与独立 Redis Lua 通过。demo 改用模块、删除无界旧账本；[验收与恢复前提](evidence/module-purchase-20261112/README.md)。全模块正反序和 §6 联合验收仍留到最后。

- **要解决的**：付费订单“恰好一次发货”。现在在 game-demo 的 `game/purchase/` 与 `internal/service/game/purchase_drain.go`，订单账本挂在 demo 的 `claim_ledgers.go`。
- **需求**：平台回调（经 platform 服务）到账 → 订单号去重 → 发货（wallet 加币或 bag 加道具，经登记点扩展）在同一笔事务里；重复回调零写入并答同一个结果；玩家不在线时到账也要发（下次进入时补发或直接冷加载，取 demo 现有做法并写明）；订单账本有界且到界后仍正确（不能“满了就拒绝”）。
- **依赖**：wallet；bag（联动）；platform 托管服务（验签仍归工程，缺省全拒，照 login 的 `login_channels.go` 写法留一个业务文件）。
- **验收**：重复回调、乱序回调、金额与商品不符、发货提交失败后重放、账本到界之后的行为各有场景。

### R3. 升级奖励邮件（mailbox + profile 联动）

已完成（提交 `7f91c18f`）：实现与定向验收完成：联动 10 条，新工程 76 PASS / 0 SKIP，6 个变异、两轮 race 与 demo 经验事务定向通过。奖励在升级时冻结，连升一次一封，缺省不发；demo 已迁移。[证据与边界](evidence/module-profile-mail-20261112/README.md)。真实联合验收留在 §6。

- profile 已有按名字登记的升级钩子（attribute 在用）。做成 `mailbox` 的 `with/profile/` 联动：升级那一刻在同一条 WAL 记录里产生 effect，消费者发邮件（game-demo 的 `level_up_mail.go` 是现成写法）。奖励内容写在一个归工程所有的业务文件里，缺省不发（零配置不能凭空发奖）。
- **验收**：升级一次恰好一封；连升多级的行为写死并有场景；消费者重放不重发。

### R4. 登记点补齐

已完成（提交 `35153b75`，实现与定向验收）：消费、购买、体力、副本与排行登记点已有 task 实际使用者；实体内钩子与业务同事务，使用冻结业务时间。排行提交后的任务是单独的尽力投递，不承担恰好一次结算。[R4/R5 证据](evidence/module-task-events-20261112/README.md)。

每个登记点都要有真实使用者（R5 的 task 就是）：

- wallet 的“消费成功”：要能区分 `Apply` 命中去重回执与首次生效，只有首次生效才触发。
- shop 的“购买成功”、stamina 的“消耗”、leaderboard 的“提交”（提交时机由结算规则决定，登记点只报“提交了”）。
- 写法照 checkin 的 `OnCheckinClaimed`：提供方留登记函数，接入方带一个自己的联动文件在 `init` 里登记；事务内的钩子不能做 I/O。

### R5. task 扩成核心系统

已完成（提交 `35153b75`，实现与定向验收）：事件、每日/每周/永久、前置链、金币/道具奖励与配置表；基础 29 条（原 18 条保留），八组 task 联动 25 条，组合 215 PASS / 0 SKIP、15 个变异与两轮 race 通过。完整矩阵和真实依赖按 §2/§6 留到最终批次。维护者补充的新角色默认 1 级已通过 profile + enter 的正式首次建档事务实现，失败回滚、不触发升级钩子；attribute 同事务派生初始属性。[使用说明](../../modules/task/README.md)、[默认等级](../../modules/profile/README.md)。

维护者：“task 是一个比较核心的系统，很多业务需求可以加到 task 中去。”方案见[业务模块方案 §18](FEATURE-2026-11-10-business-modules.md) 末尾，按这个先后做：

1. **按事件推进**：入口 `TaskComp().Report(事件, 次数, nowUnixMilli)`；别的功能只上报“发生了什么”，不认识任务号；现有 `Advance(任务号, …)` 保留。
2. **周期与种类**：每日之外加每周、永久（成就）、有前置的任务链。
3. **奖励种类**：改成（种类、号、数量），与 mailbox 的附件同一种表达；装了 bag 就能发道具。
4. **任务表做成配置表**（照 bag 的道具表）。
5. **联动**：R4 的各登记点上报事件（task + wallet / shop / stamina / profile / dungeon）。

- **约束**：零配置安装时现有的每日任务行为不变（18 条场景继续通过）；进度与领奖记录有界；跨天、跨周的惰性清零与 servertime 下发的业务日口径一致（同一个 `clock.BusinessDay`）。

### R6. 规则数据

已完成（提交 `29fd4f25`，实现与定向验收）：shop 商品配置表与约束；rules 统一 Player Nest 只读快照，进入/重连推送、RulesGet 补取，stamina/shop 自动依赖。组合 104 PASS / 0 SKIP、6 个变异与两轮 race；任务影响复验 232 PASS / 0 SKIP、原 15 个变异与 race 通过。[验收与边界](evidence/module-client-rules-20261112/README.md)。

- shop 的商品目录改成配置表（照 bag 的道具表）；模块带缺省表，零配置可用。
- 规则导出给客户端：stamina 的上限与周期、shop 的限购规则——定一个统一做法（协议里带，或进入时推一次），不要每个模块各做各的。当前业务日已由 servertime 下发，不要重复做。

## 4. 第二优先：多人与社交

### R7. team + matchmaking + matchroom（一起设计）

部分完成（2026-11-12）：`team` 基础与 rehome 联动已实现并定向验收，25 + 2 条场景，组合原 rehome 40 条均通过，8 个变异与两轮 race 通过。[证据与边界](evidence/module-team-20261112/README.md)。`matchmaking`、`matchroom`、turn-pvp 接入与 demo 队伍迁移仍未完成。

- **team**：队伍实体（game-demo 的 `game/entities/team`）；建队、入队、离队、队长；入队与写玩家档在同一笔 `[Team, Player]` 事务里（rehome 的排空依赖这一点，不能改成两笔）。
- **matchmaking**：排队、成局，经托管的 match 服务（demo 的 `game/matchmaking`、`internal/service/game/matchmaker.go`）。
- **matchroom**：成局后移交到 room（`room.Open`），可信匹配方提供唯一号。
- **联动**：rehome（在队伍里 / 排队中时拒绝换服，现在只有 demo 的 `rehome_checks.go` 做了）；turn-pvp 配方现在“没有客户端开房 / 匹配入口”，做完后把配方接上。
- **验收**：单人排队成局进房、组队排队、排队中取消、成局后一方掉线、重复成局通知各有场景。

实施方案（2026-11-12，逐项落地，不代表已完成）：

1. `team` 先提供独立组队能力：保留 Remote managed 的共享 Team 权威，Player 仅持久保存队伍引用；建队 / 入队 / 离队必须同时声明 Team 与 Player。取消 demo 的分区场景前置条件，队伍视野作为后续场景联动；队长离队选最小玩家号接任。邀请有数量和时间界限，队伍号通过持久计数器分配、不复用。安装器代办已有 `remote_entity` Mod，不修改 Nest / DataEngine / Sync。
2. `matchmaking` 持久保存排队意图，再在锁外调用托管 match 服务。同一意图使用稳定 requestID，未知结果保留意图，取消得到确定终态后再解除排队限制。组队排队冻结权威成员版本，排队期间不可改队；默认按本服分队列（room 由本进程拥有），跨服社交队伍要先明确房间路由，不能直接当作本地房间成员。
3. `matchroom` 负责成局到 room 的可重试移交：可信分配器持久分配唯一房号，按 matchID 幂等映射；开房成功才返回房号。不能把一次推送当成移交事实，查询匹配结果也能恢复开房。匹配房保留成员名单并校验入房资格，掉线成员可重连，不让旁人占用其座位。
4. rehome 在同一 Player 事务里拒绝有队伍 / 排队意图的玩家；反向入口也拒绝已记下换服意图的入队 / 排队。turn-pvp 最后接入匹配入口。普通流程不引入 Saga。
5. 每个模块先跑生成工程定向场景、回滚与重复安装；三者组合后验证成局 / 取消竞态、重复通知、掉线重试和换服互斥。Remote、match、room 的真实存储与跨进程验收仍归 §6，不用内存替身宣称通过。

### R8. gift（玩家间送礼）

- 扣 → 送的 saga，失败补偿；扣款步骤按 sid 确切路由（v1.44.0 的 `roost add saga -routed`）。game-demo 的 `saga/gift_item/` 与 `game/gift/` 是现成实现。
- 属主核对用 `rehome.Binding`（demo 的 `admitByBinding`）；没装 rehome 时退化为“本服即属主”，两种都要有场景。
- **安装器**：saga Mod 与 `roost add saga` 现在只检查、不代办；需要代办时在安装器里补（照 heartbeat / directory 的代办写法）并配测试。
- **只有离线场景时写明**：真实双 sid 的恰好一次要等真实依赖演练。

### R9. activity（活动）

- **维护者明确（2026-11-12）**：activity 是通用的按时间控制阶段的框架，不以活动榜或发奖定义它。阶段名和业务动作由工程提供，可以用于预告、报名、进行、结算、展示等任意有时间边界的流程。
- 核心提供活动实例、配置中的有序阶段与时间边界、业务时钟驱动的阶段推进、当前阶段查询及阶段进入登记点；用模块的持久定时器类型分配。暂停 / 重启后按持久游标恢复，明确跨过多个阶段时的处理和重复触发的幂等边界。
- 活动榜、结算与 mailbox 发奖作为可选联动，复用 demo 的 `game/activity`、`internal/service/game/activity.go` 与托管 activity 服务，不让没有排行或奖励的活动依赖它们。
- 活动定义做成配置表；零配置时没有任何活动在跑。定向验收覆盖边界时刻、延迟触发、重启恢复、重复投递、提交失败回滚及无排行 / 邮件的纯阶段活动。

### R10. alliance（联盟）

- **维护者明确（2026-11-12）**：模块、服务、目录和新业务 API 统一使用 `alliance`，不新增 `guild` 别名。迁移 demo 时一起处理其旧字段与协议命名；不为未上线旧工程保留兼容层。
- 最重的一项：独立进程的联盟服务独占联盟实体，写走路由 RPC 或 saga（demo 的 `saga/alliance_*`、`internal/service/alliance/`），game 读 Mirror。
- 先出方案再动手（报告维护者）：模块能不能装出一个新的业务服务、三个 saga 怎样随模块安装、成员名单与 `HomeSID` 的惰性修正（与 rehome 二期相关）。不要为了塞进模块而简化掉“联盟实体只有一个写者”。

### R11. 其余

- lockstep 战斗房间（demo 的 `game/battle`）：排在 R7 之后，依赖 room；`gameplay/physics` 留给它。
- worldstats（World 上的全服累计）：是否单独成模块**待维护者定**，未定之前不做。

## 5. 现有模块的后续

- **room**：自动断线订阅、断线保留策略、重启后在线状态重建、广播。
- **rehome 的联动**：mailbox（离开前按旧范围拉一次全服邮件）、chat（离开时移出在线名单）、leaderboard（按服的榜离开时移除）、room / turn（在房间里时拒绝换服）。二期、三期见[换服方案](FEATURE-2026-11-11-player-rehome.md)，其中“真实依赖演练”上线前必须做。
- **game-demo 换用场景线模块**：scene、monster、dungeon、terrain 模块已有，demo 仍用自己的实现（cellworld 分区、LOD / 队伍视图、SceneReady、玩法刷怪等适配没迁）。逐块评估：通用的搬进模块，确属 demo 独有的留下并在 WALKTHROUGH 里写明。cellworld 仍是原型，不做模块。
- **配方**：13 类玩法夹具（`scripts/testdata/<玩法>/`）改成模块组合；现有配方只有 casual、turn-pvp。每迁一类，夹具里反复手写的业务先看能不能落进现有模块，再决定要不要新模块（例如 SLG 的建造 / 行军队列、合成休闲的广告回调到账账本）。
- **各模块的机器人步骤**：每个模块带自己的压测 / 验收步骤，接到 game-demo 的 `cmd/loadtest`；现在全部模块的验收都是离线的。

## 6. 验证欠账（登录线之外）

- 模块端点经真实 TCP 的整条链路（登录入口那一段原会话已做过单机演练，其余模块的协议没有）。
- mailbox 经 NATS / Redis 的路径；room 与 turn 的 Mongo / WAL / TCP 联合验收；场景线模块的真实存储验收。
- game-demo 的 `make dev-run` / `dev-smoke` 与压测机器人（协议换过，机器人只编译过）、`scripts/test-demo-generated.sh`、`scripts/test-mmorpg-cross-sid.sh`。
- 换服：真实的交出实体、两个 game sid + account + saga、每个阶段后 kill -9。

## 7. 等维护者定的（未定时按推荐项做，并在报告里标明）

| 事项 | 推荐 |
| --- | --- |
| gm 的形态 | 一个 gm 模块提供登记点，各模块用联动登记自己的命令（R1） |
| worldstats 是否单独成模块 | 先不做 |
| 保留号（demo 每删掉一个写死的号，模块的号按最小空号重排） | 先不做；出现客户端兼容需求时再让安装器支持保留号 |
| alliance 能否作为模块装出一个新服务 | 先出方案，仍保持单写者 |
| 规则导出给客户端的统一做法 | 进入时推一次（照 servertime 的联动），协议里不重复带 |

## 8. 不做

- `gameplay/physics`（留给帧同步战斗）、`gameplay/cellworld`（原型）不做模块。
- 运维工具（搬服、合服、数据迁移、压测机器人本体）、部署与监控配置留在 demo 作参考。
- 登录线（§1）。

## 9. 交付

- 直接在 `feat/business-modules` 上做；每个模块一个或几个提交，提交说明写清楚验证跑了什么、没跑什么。
- **本文现在只在登录线的分支上**（`tmp/mod-portal`，远端 `feat/business-modules-portal`，路径同本文）。接手 agent 开工时把它原样复制进 `feat/business-modules` 的同一路径并提交（连同 [DOCUMENTS](DOCUMENTS.md) 里的那一行），之后的进度标记写在主线那一份上；登录线合入时以主线那一份为准。
- 公共文件只加自己的行：`scripts/test-modules-generated.sh`（`all_modules` / `all_counts` / 联动名单）、`modules/README.md`、`docs/framework/CAPABILITIES.md`、`docs/maintenance/KNOWN-LIMITS.md`、`CHANGELOG.md`。`docs/agent-skills/roost-genre/references/compose.md` 只做事实层面的跟进，结构留给原会话最后统一重写。
- 每完成一项，在[待办交接](HANDOFF-2026-11-11-module-backlog.md)的附录追加一段记录（做了什么、场景数、变异、没验证的范围），并在本文对应条目后标“已完成（提交号）”。
- 不合入 main、不发版，除非维护者明确说。
