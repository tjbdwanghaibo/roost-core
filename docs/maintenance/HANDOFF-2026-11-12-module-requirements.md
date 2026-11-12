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

登录线的工作在分支 `tmp/mod-portal`（worktree `/Users/whb/roost/worktrees/mod-portal`，远端 `feat/business-modules-portal`），**还没有合进 `feat/business-modules`**（维护者：主线等最终 skill 也加完后再合并）。它改了这些公共文件，接手 agent 在主线上改同一处时只加自己的行，合并时取并集：`scripts/test-modules-generated.sh`（`all_modules` / `all_counts` 加了 portal，login 的场景数 19 → 22）、`modules/README.md`、`docs/framework/CAPABILITIES.md`、`docs/maintenance/KNOWN-LIMITS.md`、`CHANGELOG.md`、`codegen/internal/roost/module_install.go`（`moduleCanAddMod` 认 web）、`module_spec.go`（清单新字段 `next`：安装后提示）。第二轮（同日，按维护者的三条定型）又动了：**模块 enter 本身**（新建请求 `EnterCreate`、推送 `EnterStatus`、三个业务码、`PlayerDao.Enter.Fresh`、新的登记点；原有 14 条场景与 `EnterGame` 的缺省行为不变，场景数 14 → 29，`needs.features` 多了 errcode，多一组联动 `enter/with/profile`——接手 agent 改 enter 或它的场景数时以登录线的为准）、`scripts/test-modules-generated.sh`（enter=29、login=32、portal=12、login+enter=6、enter+profile=3，portal 那一步断言 web 加在 account 上）、`codegen/internal/roost` 的 `module_test.go` / `module_actor_sync_test.go` / `module_portal_test.go`（场景数）、`module_runtime.go` / `module_install.go` / `module.go` / `render.go` / `port_blocks.go`（清单新字段 `runtime.framework`、web 开发端口按端口块）、`render_player_tcp.go`（认证器的可选接口 `SessionObserver`）、`docs/framework/guide/09-services.md`、`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`，以及 `service/account`（`Accounts` 多六个方法）与新包 `wiring/modulehost`。接手 agent 的模块需要“安装后提示”或 web Mod 代办时，等登录线合入后再用，或先写进模块 README。

## 2. 验证节奏

维护者 2026-11-12：“测试最终再跑，我还会继续开发”。每一步只跑与改动直接相关的检查（模块自己的 `roost module verify`、`go test -run 'TestModuleCatalog|TestModuleWith' ./codegen/internal/roost`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`）。完整测试（`scripts/test-modules-generated.sh`、`TestDemo|TestModule`、重新生成 game-demo 跑全量、全仓 `go test ./...`）留到最后统一跑一次；没跑的在提交说明与报告里写明。

## 3. 第一优先：让拼出来的工程“能运营”

### R1. gm（运维命令）

- **要解决的**：拼出来的工程没有办法发道具、加经验、发补偿邮件、查玩家；这些现在只在 game-demo 的 `internal/service/game/gm.go` 里手写（`gm.player.add_item` / `add_exp`、`gm.mail.send`、`gm.player.rehome`、`gm.world.stats`、`gm.saga.get|list` 等）。
- **形态（按推荐项做，待维护者确认）**：一个 `gm` 模块提供登记点与 ops `/admin` 的接线（命令名、参数解析、鉴权沿用 ops 的 admin token 规则）；各功能模块用联动文件（`with/gm/`）登记自己的命令——装了 bag 才有加道具，装了 mailbox 才有发补偿邮件。不做“一个模块里写死全部命令”。
- **需求**：命令经 Nest 的正式 handler 落到实体上（不绕过事务）；每条命令带操作者备注并记日志；命令的拒绝有带码错误；开发 token 在生产配置下被拒（沿用 ops 的规则，不另起一套）。
- **首批命令**：wallet 加减币、bag 加道具、profile 加经验、mailbox 发补偿邮件（单人）、rehome 发起换服（已有 `gm.player.rehome` 的写法）。
- **验收**：只装 gm 时零命令也能启动；每组联动各有场景（命令成功、参数非法零写入、目标不存在、提交失败回滚）；game-demo 的 gm.go 里对应命令改为由模块提供。

### R2. purchase（充值发货）

- **要解决的**：付费订单“恰好一次发货”。现在在 game-demo 的 `game/purchase/` 与 `internal/service/game/purchase_drain.go`，订单账本挂在 demo 的 `claim_ledgers.go`。
- **需求**：平台回调（经 platform 服务）到账 → 订单号去重 → 发货（wallet 加币或 bag 加道具，经登记点扩展）在同一笔事务里；重复回调零写入并答同一个结果；玩家不在线时到账也要发（下次进入时补发或直接冷加载，取 demo 现有做法并写明）；订单账本有界且到界后仍正确（不能“满了就拒绝”）。
- **依赖**：wallet；bag（联动）；platform 托管服务（验签仍归工程，缺省全拒，照 login 的 `login_channels.go` 写法留一个业务文件）。
- **验收**：重复回调、乱序回调、金额与商品不符、发货提交失败后重放、账本到界之后的行为各有场景。

### R3. 升级奖励邮件（mailbox + profile 联动）

- profile 已有按名字登记的升级钩子（attribute 在用）。做成 `mailbox` 的 `with/profile/` 联动：升级那一刻在同一条 WAL 记录里产生 effect，消费者发邮件（game-demo 的 `level_up_mail.go` 是现成写法）。奖励内容写在一个归工程所有的业务文件里，缺省不发（零配置不能凭空发奖）。
- **验收**：升级一次恰好一封；连升多级的行为写死并有场景；消费者重放不重发。

### R4. 登记点补齐

每个登记点都要有真实使用者（R5 的 task 就是）：

- wallet 的“消费成功”：要能区分 `Apply` 命中去重回执与首次生效，只有首次生效才触发。
- shop 的“购买成功”、stamina 的“消耗”、leaderboard 的“提交”（提交时机由结算规则决定，登记点只报“提交了”）。
- 写法照 checkin 的 `OnCheckinClaimed`：提供方留登记函数，接入方带一个自己的联动文件在 `init` 里登记；事务内的钩子不能做 I/O。

### R5. task 扩成核心系统

维护者：“task 是一个比较核心的系统，很多业务需求可以加到 task 中去。”方案见[业务模块方案 §18](FEATURE-2026-11-10-business-modules.md) 末尾，按这个先后做：

1. **按事件推进**：入口 `TaskComp().Report(事件, 次数, nowUnixMilli)`；别的功能只上报“发生了什么”，不认识任务号；现有 `Advance(任务号, …)` 保留。
2. **周期与种类**：每日之外加每周、永久（成就）、有前置的任务链。
3. **奖励种类**：改成（种类、号、数量），与 mailbox 的附件同一种表达；装了 bag 就能发道具。
4. **任务表做成配置表**（照 bag 的道具表）。
5. **联动**：R4 的各登记点上报事件（task + wallet / shop / stamina / profile / dungeon）。

- **约束**：零配置安装时现有的每日任务行为不变（18 条场景继续通过）；进度与领奖记录有界；跨天、跨周的惰性清零与 servertime 下发的业务日口径一致（同一个 `clock.BusinessDay`）。

### R6. 规则数据

- shop 的商品目录改成配置表（照 bag 的道具表）；模块带缺省表，零配置可用。
- 规则导出给客户端：stamina 的上限与周期、shop 的限购规则——定一个统一做法（协议里带，或进入时推一次），不要每个模块各做各的。当前业务日已由 servertime 下发，不要重复做。

## 4. 第二优先：多人与社交

### R7. team + matchmaking + matchroom（一起设计）

- **team**：队伍实体（game-demo 的 `game/entities/team`）；建队、入队、离队、队长；入队与写玩家档在同一笔 `[Team, Player]` 事务里（rehome 的排空依赖这一点，不能改成两笔）。
- **matchmaking**：排队、成局，经托管的 match 服务（demo 的 `game/matchmaking`、`internal/service/game/matchmaker.go`）。
- **matchroom**：成局后移交到 room（`room.Open`），可信匹配方提供唯一号。
- **联动**：rehome（在队伍里 / 排队中时拒绝换服，现在只有 demo 的 `rehome_checks.go` 做了）；turn-pvp 配方现在“没有客户端开房 / 匹配入口”，做完后把配方接上。
- **验收**：单人排队成局进房、组队排队、排队中取消、成局后一方掉线、重复成局通知各有场景。

### R8. gift（玩家间送礼）

- 扣 → 送的 saga，失败补偿；扣款步骤按 sid 确切路由（v1.44.0 的 `roost add saga -routed`）。game-demo 的 `saga/gift_item/` 与 `game/gift/` 是现成实现。
- 属主核对用 `rehome.Binding`（demo 的 `admitByBinding`）；没装 rehome 时退化为“本服即属主”，两种都要有场景。
- **安装器**：saga Mod 与 `roost add saga` 现在只检查、不代办；需要代办时在安装器里补（照 heartbeat / directory 的代办写法）并配测试。
- **只有离线场景时写明**：真实双 sid 的恰好一次要等真实依赖演练。

### R9. activity（活动）

- 活动榜、定时结算、发奖（demo 的 `game/activity`、`internal/service/game/activity.go`）；用模块的定时器类型分配；发奖走 mailbox。
- 活动定义做成配置表；零配置时没有任何活动在跑。

### R10. guild（公会）

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
| guild 能否作为模块装出一个新服务 | 先出方案 |
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
