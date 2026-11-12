# 业务模块待办交接（2026-11-11，同日晚重排）

给接手的 agent。分支 `feat/business-modules`（未合入 main，不要自行合并或发版）。本文只列**还没做的**；已完成的见 §2 的状态表与文末附录。

## 0. 最终目标（验收标准）

维护者 2026-11-11：“这波模块的整理，最终目的是别的可以依赖这些模块，能非常容易的自定义拼接出一个 game server 的半成品。”

据此判断每件事值不值得做、先做哪件：**从一个空的 `-template game` 工程出发，挑几个模块装上，就得到一个能编译、能跑、覆盖“登录 → 进入 → 所选玩法”的服务器半成品，几乎不用手写接线。** 三条推论：

1. 模块要能被依赖：对外入口（组件方法、登记点、联动）稳定、有文档。
2. 拼起来要省事：缺省即可用，模块之间该接的线由联动文件自动接上；让拼接更容易的工作（配方、登记点、联动）优先于把 game-demo 再拆细。
3. 真正的检验是一个只凭 skill 的 agent 按一段需求拼出服务器，不是 demo 的测试全绿。

**给 agent 用的那份 skill（[roost-genre](../agent-skills/roost-genre/SKILL.md) 的“按需求组合”）由原会话最后统一重写**（维护者 2026-11-11：“这个 agent 的 skill 最后由你来写，后续再写”）：迁模块时只对 `references/compose.md` 做事实层面的跟进（模块名、场景数、例外列表），不要重写结构。

## 1. 先读

1. [业务模块与配方方案](FEATURE-2026-11-10-business-modules.md)：决定一览、§18（模块清单与 task 的扩展方向）、§19、§20。
2. [modules/README](../../modules/README.md)：清单格式（含 `runtime`、`entities`、`timer_types`、`with`）、登记点、**约定**（尤其“模块可以做全、做复杂，但简单用法必须照样能用”）。
3. [能力清单](../framework/CAPABILITIES.md)、[KNOWN-LIMITS“业务模块”](KNOWN-LIMITS.md#业务模块roost-module)。
4. 换服：[方案](FEATURE-2026-11-11-player-rehome.md)、[PLAYER-MIGRATION §7](../framework/PLAYER-MIGRATION.md)。
5. 仓库规则：[AGENTS.md](../../AGENTS.md)、[维护手册](README.md)。

## 2. 现在的状态（2026-11-12）

| 项 | 状态 |
| --- | --- |
| 模块 | 29 个：原有 15 个，加 actor、scene、monster、attribute、equipment、combat、dungeon、terrain、naming、cdkey、notice、gacha、friends、idle；场景数以各自的 `module.yaml` 与 `scripts/test-modules-generated.sh` 为准 |
| 模块间联动 | 原有 5 组，加属性/装备、场景/聊天与公开字段、战斗/目录/场景/装备、副本/排行榜、地形/怪物；精确清单以 module.yaml 为准 |
| 配方 | casual、turn-pvp：`roost project new -recipe`，细节见 modules/README；平台身份验证、可信匹配和胜负仍是业务接入点 |
| 登记点 | enter 的事务内首次初始化、进入前后；checkin 签到；mailbox 附件；rehome 换服；profile 升级；actor 进出与每拍；scene 区域/交互/公开字段；skillbook 编译环境 |
| 安装器 | 服务级接线点、自带实体、定时器、配置表；新增 heartbeat/directory 代办、attribute feature、附加 DAO 聚合与明确的 Sync packer |
| game-demo 用的模块 | enter、chat、leaderboard、bag、profile、mailbox（连带 wallet）、skillbook、rehome、attribute、equipment，同一步安装；移动复用 actor Body，保留 demo 的 cellworld/地形适配 |
| 单个玩家换服 | 一期已实现并合入（account 接口、DataEngine 交出实体、模块 rehome、demo 接线），**只有离线验证**；二、三期见 §4 的 K |
| 审查 | `fix/core-game-server-review` 上的三项缺陷（RV-20261111-BM-01/02/03：换服占位、换服重放、wallet 最小整数扣款）已修；那个分支的 KNOWN-LIMITS 里“三项 P2 尚未修复”一段要在那边关闭 |

原来“不要碰的范围”（换服）**已解除**：一期做完并合入了，余下的并进本文的待办。

## 3. 做事的规矩

- **demo 比模块多的能力搬进模块**，demo 只留规则数据（覆盖模块留给工程的业务文件或配置表）与确属它独有的玩法；全新 `-template game` 工程不做任何配置直接 `roost module add <模块>` 必须能用、验收全绿。
- **不为已生成的工程做兼容**（未上线）：DAO 字段、存储键、协议与消息号、错误码都可以变；但 demo 自己要自洽（迁移示例、GM、压测机器人、README / WALKTHROUGH）。
- demo 换用一个模块的做法照已有的七块（enter、chat、leaderboard、bag、profile、mailbox、skillbook）：`codegen/internal/roost/demo.go` 的 `demoModuleSteps()`，全部模块在同一步里安装、新模块接在列表末尾，覆盖模块文件的 `write` 排在它之后，建在模块组件上的 handler 不要 `roost add handler` 骨架步骤（`TestDemoStepsLeaveModuleComponentsToTheModules` 钉住）。
- 每个模块：场景测试（不用子测试、数字写死、只断言自己的事实）、变异自检、`module.yaml` 的 `verify.pass`；场景数写进 `scripts/test-modules-generated.sh` 与 modules/README。
- **验证节奏**（维护者 2026-11-11）：每一步只跑与改动直接相关的检查（模块自己的 `roost module verify`、`go test -run 'TestModuleCatalog|TestModuleWith' ./codegen/internal/roost`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`）；完整测试——`scripts/test-modules-generated.sh`、`go test -run 'TestDemo|TestModule' ./codegen/internal/roost`、重新生成 game-demo 跑全量、全仓 `go test ./...`——**最后统一跑一次**。没跑的在提交说明与报告里写明。
- DataEngine / Nest / Sync 三块基础不为这些工作改动；确需改动先报告维护者。全程 `GOWORK=off`；不等 GitHub CI；额度紧张时把未完成的写进文档、推送、停下。

## 4. 待办（按“让拼接更容易”排先后）

### A. 现在已经解开、可以直接迁的（服务级接线点已有）

| 模块 | 内容 | 备注 |
| --- | --- | --- |
| gift | 玩家间送礼：扣 → 送的 saga，失败补偿；扣款步骤按 sid 确切路由（v1.44.0） | 依赖 bag；属主核对用 `rehome.Binding`（demo 的 `admitByBinding` 是现成写法）；要 saga Mod 与 `roost add saga` 作安装前置 |
| 升级奖励邮件 | 消费“升级”事件并发邮件 | 做成 mailbox + profile 的联动；profile 要先有别的模块挂得上的升级登记点（见 D） |
| activity | 活动榜、定时结算、发奖 | 可用定时器类型分配 |
| gm | 运维命令：加道具、加经验、发补偿邮件、换服 | 各模块各带自己的命令，还是一个 gm 模块汇总，先定形态 |
| matchmaking | 排队、成局 | 与 team、matchroom 一起看 |
| dungeon | 已实现模块：进入、真实 level 阶段、结算、领奖账本、通关计数 | 通关榜联动已用 leaderboard；demo 专有玩法尚未迁移 |

### B. 不依赖新能力、随时可迁的

equipment 与 attribute 模块及 game-demo 接入已实现；尚余 purchase（付费订单恰好一次发货、订单账本——现在挂在 demo 的 `claim_ledgers.go`；依赖 bag 与 platform 服务）、worldstats（World 上的全服累计；是否单独成模块待定）。

### C. 配方（直接服务最终目标，模块已经够拼第一个）

已完成最小配方 `casual`（enter + wallet + checkin + task + stamina + shop + bag + mailbox）与 `turn-pvp`（enter + profile + room + turn + leaderboard）；支持自定义 module/service，安装、ID、联动、自身同步和原子新建均复用现有逻辑。生成正式 Account session 认证器、开发 TCP 配置、业务规则和 `RECIPE.md`，休闲首次 100 金币由 OnFirstEnter 接入。

仍需继续：13 类玩法夹具（`scripts/testdata/<genre>/`）迁成模块组合；更丰富的配方参数。回合配方没有客户端开房/匹配入口或默认胜负，可信匹配方提供唯一号并调用 `room.Open`；排行榜何时提交由结算规则决定。平台 IdentityVerifier / PlayerIDAllocator 仍由业务实现，不能把替身测试描述为可直接公网登录。

### D. 还缺的登记点与联动（每个都要有真实使用者）

- wallet 的“消费成功”（区分 `Apply` 命中去重回执与首次生效；task 的“消费三次”在等它）。
- profile 升级已提供按名字登记的钩子，由 attribute 实际使用；升级奖励邮件联动仍待做。enter 的事务内首次发放登记点已经完成，由 casual 配方实际使用。
- leaderboard 的提交时机；stamina 的消耗；shop 的购买成功。
- rehome 的其余联动：mailbox（离开前按旧范围拉一次全服邮件）、chat（离开时移出在线名单）、leaderboard（按服的榜离开时移除）、room / turn（在房间里时拒绝换服）。

### E. task 扩成核心系统

维护者 2026-11-11：“task 是一个比较核心的系统，很多业务需求可以加到 task 中去。”见方案 §18 末尾：按事件推进（`Report(事件, 次数)`，别的功能只上报发生了什么）、每周 / 成就 / 任务链、奖励种类（与 mailbox 的附件同一种表达）、任务表做成配置表、各模块上报事件的联动（依赖 D）。

### F. 自带实体之后能做的

matchroom（匹配成局后移交到 room）、team（队伍实体）、guild（联盟服务：独立进程、三个 saga、成员名单、game 读镜像）、lockstep 战斗房间。room 还没有的：自动断线订阅、断线保留策略、重启后在线状态重建、广播。

### G. 场景（维护者 2026-11-11 同意的顺序）

场景线 actor / scene / monster / attribute / equipment / combat / dungeon / terrain 已实现模块，包含多场景/分线、区域与交互、附近聊天、巡逻追击与单进程地形/建筑；各模块边界见 modules/README。game-demo 已统一属性/装备与共享 Body；其 cellworld 分区、LOD/队伍视图、SceneReady 和玩法刷怪等适配仍独立，尚未整体安装 scene/monster/dungeon/terrain 模块。不能把模块可用描述为 demo 全面迁移完成。cellworld 按新分工继续留作原型。

### H. 规则数据

shop 的商品目录、task 的任务表改成配置表（照 bag 的道具表）；规则导出给客户端（stamina 的上限与周期、当前业务日）。

### J. 验证欠账

- **只凭 skill 的实测**：已补过一轮（见附录），profile、chat、room、turn、rehome 与完整 game-demo 还没有被这样测过。
- **真实依赖**：game-demo 的 `make dev-run` / `dev-smoke` 与压测机器人（邮件、排行榜、背包、档案的协议都换过，机器人只编译过）、`scripts/test-demo-generated.sh`、`scripts/test-mmorpg-cross-sid.sh`（客户端已改写、只编译过）、mailbox 经 NATS / Redis 的路径、模块端点经 TCP 的整条链路、room 与 turn 的 Mongo / WAL / TCP 联合验收。
- 全仓首轮回归里 `gameplay/cellworld` 的三条用例（`TestPanelChangeDuringHandoff`、`TestSecondCellProcessForSameSIDIsRefused`、`TestWorldJitterAtBoundarySlowHandoffConclusion`）失败过一次、单独复测通过，**原因尚未确认**（附录有记录）；之后两次全仓回归没有再出现。
- game-demo 的背包同步有一处冗余（KNOWN-LIMITS）：`Player.PublishSyncDirty` 改用 `TakeEntitySyncChanges()` 可消除。

### K. 单个玩家换服的后续（[方案](FEATURE-2026-11-11-player-rehome.md) §7、§8）

- **真实依赖演练（上线前必须）**：真实的交出实体、两个 game sid + account + saga、每个阶段后 kill -9；在途赠礼的扣款在新服恰好执行一次。
- 二期：game-demo 没拦住的状态——匹配队列里的票据、进行中的副本与对战、名下建筑与地形账本；联盟成员的 `HomeSID` 惰性修正；全服邮件的服范围；分区场景的回收。
- 三期：account 的“在途换服”索引、列举与过期清扫（有了它，rehome 才值得用服务级接线点在启动时续做、停机时收尾）；无人认的占位的后台清扫；物理库不同的服；合服交叉；独立 Gate（转给 game 的绑定不带 claims）。
- demo 接线没有做变异自检。

### 不建议做成模块的

登录鉴权（`auth.go`）、运维工具（搬服、合服、数据迁移、压测机器人）、部署与监控配置：留在 demo 作参考。

## 5. 等维护者定的

- **换服**：方案 §6 的六项决定（按推荐项实施的）；实现时的取舍——离开钩子排在断开会话之前、受理后中止也断开会话、`PlayerOwners.Serve` 不直接收 claims、enter 的应答不带“目标服”；saga 准入可能发一次 account 调用（account 慢时拖住步骤消费者）是否接受；DataEngine 交出实体的三点（`Release` 的形态、等 checkpoint 用轮询还是按实体记账、`ErrEntityHandedOff` 放哪）。
- **account 换服按 ID 的两处既有边界**（审查修复后仍在，KNOWN-LIMITS 有记）：早已了结的 ID 带当前代数与序号重新发起会被受理；无在途换服时重发一个已被覆盖的 ID 的中止会再记一次并返回成功。是否收紧。
- 已按维护者同意的顺序先完成 C 的两个最小配方与首次发放登记点；接下来做 D 的真实联动，再推进 E。
- worldstats 是否单独成模块；gm 是一个模块还是各模块各带命令。
- 保留号：demo 每删掉一个写死的号，模块的号按最小空号重排；要稳定需要安装器支持保留号，做不做。

## 附录：已完成批次的原始记录（另一会话，2026-11-11）

- A1 + skillbook 已实施。清单声明 `runtime.package/start`；安装归属含 service、requires、runtime；生成 `modules_gen.go`，bootstrap 使用 `NewWithModules()`，业务 Service 文件不改写。依赖顺序启动、反向停止，启动失败清理、停机失败保留依赖与回调。契约见 [modules/README](../../modules/README.md#服务级启动与停机)。
- demo 的技能目录和查询统一使用模块，移除 controller 自己的目录缓存与重复编译。场景直接使用 Service 自己的同步观察器，不再从 Registry 断言具体 `*Service`；模块适配层保留 `NestSyncObserver` 等可选接口。
- 验收发现并修正：适配层生成标记必须复用 `generatedHeader`；重写 bootstrap 必须保留实体 owner / route / preload 和配置派生信息；doctor 需要目录编译测试入口，使用与启动相同的 CompileEnvironment。失败记录未按“通过”统计。
- 已完成的定向验证：安装回滚、重复安装不变、业务文件保留、依赖顺序/环/缺失、五种实际生成适配层的生命周期场景；skillbook 8 条、目录编译 1 条。三项变异（返回共享切片、停止后仍可读、吞掉编译错误）全部被对应测试抓住，恢复后通过。
- 独立 skill 验收：自定义 module 工程安装 10 模块 + 3 联动，177 PASS / 0 SKIP；全工程 217 PASS / 1 预期 Compose SKIP。初次 doctor 暴露的两项问题，用新 CLI、新工程重验已通过（skillbook 8/0、目录编译 1、skills:catalog 与 project-templates OK）。离线替身，不等于真实 TCP/存储验收。
- 最终完整验证：`GOWORK=off go test -timeout 30m ./...` 164 个有测试的包全部通过（含全部 Demo/Module 用例）；`scripts/test-modules-generated.sh` 通过，12 模块 + 4 联动共 206 PASS / 0 SKIP，增量安装、换顺序安装、重复安装及工程检查通过。重新生成 game-demo：47 个有测试的包、670 PASS / 14 预期 SKIP / 0 FAIL；跳过项为显式开启的真实存储、部署或性能检查；build / vet、模块验收 135/0、ID 与生成物漂移全部通过。根包检查通过，文档锚点 284 文件 / 0 断链。测试原始证据位于本机 `/tmp/roost-a1-*`，独立验收为 `/tmp/roost-skill-acceptance.SzbE5W/acceptance-summary.json`；临时路径仅用于本机复核，持久结论见本段与 CHANGELOG。
- CBM 已重建当前 worktree 索引（2026-11-11）；改动文件逐一核对覆盖。新增两处模板局部解析缺口是测试复合字面量与协议 ID 占位，已读取源码并通过生成工程验证，未把图谱当成完整解析证明。
- A2 + 基础 room 已实施：清单 `entities` 为模块分配实体号，模板带 Entity / DAO，复用 lifecycle 脚手架；源码、lifecycle、owner bootstrap 与 shutdown 配置一起纳入安装回滚，重复安装不覆盖业务修改。实际 `room` 模块有 22 条离线场景，包含正式 ManagerAccess → lifecycle → Nest Sender → DAO 与 PB 编解码。
- room 用持久 Epoch + 可信连接 nonce 隔离旧 Join / View / Leave / Disconnect；Gate 代理 Session 补只读 ConnectionID。已通过安装器冲突/回滚/号避让/重复安装定向验证、真实 TCP→Gate→Game 连接身份验证；容量、连接隔离、持久代际三项变异都被业务断言抓住，恢复后场景通过。首次连接变异只造成编译失败，未计为成功，改为可编译错误后才通过变异验收。
- A2 最终统一回归已通过：全仓 `go test -count=1 -timeout 30m ./...` 为 164 个有测试包通过（含全部 Demo/Module 用例；显式跳过用例 99）；全部模块脚本为 13 模块 + 4 联动、228 PASS / 0 SKIP，含增量/换顺序/重复安装与 build、vet、ID、生成物漂移检查；重新生成 game-demo 为 47 测试包、670 PASS / 14 预期 SKIP / 0 FAIL，module verify 为 135/0，四项工程检查全部通过。Room 22 场景与新增 Gate 真实转发用例的 race 检查通过。文档 284 文件 / 0 断链，shell 语法与 diff 空白检查通过；生产存储联合验收仍未计入。本机原始记录在 `/tmp/roost-a2-*`、`/tmp/roost-module-a2-*`，永久验收结论留在本段。CBM 已重建（2026-11-11T10:22:41Z）；本批新增 5 个模板局部解析缺口，均为 ID 占位或测试复合字面量，已读相应源码并用实际生成工程补证。
- A3 + turn 已在下节接续；下一批从此前待办中的 B / C 按依赖继续；equipment/attribute/purchase、matchroom、C–I 与 J 的真实依赖路径仍未完成。room 不包含自动断线订阅、断线保留策略、重启在线状态重建、匹配移交、广播或回合超时，见 [room](../../modules/room/README.md)。`compose.md` 只更新 3 处事实，结构未改；rehome 范围未动，没有合并 main 或发版。

### A3 + turn 接续

- 已实现模块 `timer_types` 与 `TimerSpec.Types`：类型号在 Entity 内唯一，避开本包 int32 / iota 常量、静态 Register 实参与已分配记录，不回收。不能静态确定的类型或外部登记拒绝；模板支持自定义 `{{TIMER_COMPONENT}}`。已有业务登记函数按 AST 追加一次模块方法调用，只接受直接顺序登记；类型归属写入 `timer_entity` / `ids.timer` 并做清单一致性校验。
- 安装缺失的定时器复用现有 DAO / 组件改写原语，生成物仍由同一暂存树的 project sync 产生。主 DAO（包括模块没有 State 时）、Entity、回调登记、manifest 与生成接线都在安装回滚边界；新增与已有自定义组件的回滚、增量/重复安装、分号避让均有实际工程验证。未改 Nest / DataEngine / Sync 底层。
- 新增 [turn](../../modules/turn/README.md)，依赖 room：固定开局名单、按座位轮转、每玩家连续成功序号、最后命令重放、持久超时、离线不追帧、有限回合结束、可信连接隔离。玩法可在同一 Nest 事务内 `CheckEnd → 修改游戏 DAO → End`；额外动作载荷必须自行绑定回执。没有自动广播、跨机接管、默认胜负/伤害或 Robot；关闭房间/成员不足在下次推进结束。
- 定向验证：25 个模块场景（22 Nest + 3 Controller）及 race 通过；包含真实 ManagerAccess → lifecycle → Sender、PB 编解码、BSON 水合，但提交器是替身，不当作 Mongo/WAL/TCP 联合验收。三项可编译变异（不保存序号、放过到期命令、不保存续期期限）均被行为断言抓住，恢复后通过。
- 组合验收首次发现生成的 `TestRoomTimer…` 被 room 的 `^TestRoom` 多算，已把框架绑定检查改成独立 `TestEntityTimerBindingFor…` 前缀；room 仍是 22，turn 为 25，没有改大期望数绕过验收。安装器回归改为同时 verify room + turn。
- 全仓统一回归首轮：9701 PASS、99 条显式条件 SKIP、5 FAIL，162 个有测试包通过、2 包失败。两个 entity owner 用例因断网时 `goproxy.cn` DNS 失败，恢复后与 A3 / add timer / 原持久定时器端到端检查一起补跑通过。另三条 `cellworld`（`TestPanelChangeDuringHandoff`、`TestSecondCellProcessForSameSIDIsRefused`、`TestWorldJitterAtBoundarySlowHandoffConclusion`）单独复测 3/3 通过；本轮未修改该包，首轮异常原因尚未确认，不能称为已修复或删除失败样本。首轮记录 `/tmp/roost-a3-full-repo.jsonl`，定向补跑 `/tmp/roost-a3-final-targeted.log`、`/tmp/roost-a3-cellworld-recheck.jsonl`。
- 重新生成 game-demo：47 测试包、670 PASS / 14 预期 SKIP / 0 FAIL；module verify 135 PASS / 0 SKIP，build / vet / ID / 生成物漂移四项通过。原始证据 `/tmp/roost-a3-demo-*`。完整模块组合复验通过：14 模块 + 4 联动、253 PASS / 0 SKIP，增量/反序/重复安装与四项工程检查全部通过，日志 `/tmp/roost-a3-all-modules-final.log`。暂存路径仅供本机复核，复跑入口为 `scripts/test-modules-generated.sh` 与上面的测试名。
- CBM 已刷新（代际 2026-11-11T12:14:57Z，36337 节点、零 skipped）。新增 5 个模板局部解析缺口是 ID 占位和测试复合字面量，均已逐段读源码并用真实生成工程补证；文档 285 文件 / 0 断链，shell 语法与 diff 检查通过。
- 使用文档、能力清单、CLI 清单说明和 CHANGELOG 已同步；`compose.md` 只改模块列表、例外列表与定时器能力三处事实，结构未重写。rehome 范围未动；仅提交当前功能分支，不合 main、不发版。

### C 最小配方 + D 首次初始化接续

- 新增 `casual` / `turn-pvp`，只在新建工程时使用，配方本身不是模块；整个生成过程仍在 `NewProjectContext` 的同级暂存树中完成。自定义 module/service 已验证，现成模块、ID、with、自身同步与房间定时器全部复用。
- `OnFirstEnter` 在实体锁内、同一 Nest 事务执行，失败回滚初始化标记与发放；已有 `firstEnter` 扩展文件仍先执行。casual 是登记点的实际使用者。模块库共 15 模块 / 5 联动，enter 从 11 增为 14 场景（新增三个登记契约场景）。
- 配方提供 Account session 认证器及开发 TCP 配置，拒绝错服或迁移中的角色。Account 平台身份验证/角色号分配由业务提供，turn-pvp 的房间号由可信匹配方提供；未实现默认胜负、排行榜结算或客户端开房。
- 两个配方分别有 9 / 7 条接线与鉴权场景，零跳过，普通和 race 均通过；真实 TCP + Robot transport + 生成 PB + Controller + lifecycle + Nest 经过实跑。Account / 邮件 / 排行客户端、持久提交使用替身；没有启动自身 Sync 推送驱动，不把这些结果算作 Mongo/WAL/NATS/Gate 联合验收。
- 三项可编译变异（不跑首次发放、忽略已初始化标记、放过错服票据）均被预定行为断言抓住。收尾发现配方的内层安装提前执行 tidy，会违背 `--skip-deps`；先用禁止 Go 依赖命令的用例复现 exit 91，再把新配方的依赖解析留给外层 CLI，定向复验通过。普通 module add 仍执行 tidy。
- 全仓一次回归：9,736 PASS / 99 条条件 SKIP / 0 FAIL，164 个有测试包通过，包含全部 TestDemo / TestModule / TestRecipe。之后的依赖解析调整已跑完整配方定向复验（含 race / 三项变异 / skip-deps）；完整模块组合脚本通过：15 模块 / 5 联动，共 301 PASS / 0 SKIP，增量、反序、重复安装及四项工程检查通过。新生成 game-demo：724 PASS / 14 条预期 SKIP / 0 FAIL；module verify 183 PASS / 0 SKIP，build / vet / ID / 漂移全部通过。
- 本机原始日志 `/tmp/roost-recipes-full-repo.jsonl`、`/tmp/roost-recipes-final-targeted.log`、`/tmp/roost-recipes-modules-final.log`、`/tmp/roost-recipes-demo-*.log`；复跑入口为本批命令、`scripts/test-modules-generated.sh` 与新生成工程的 `go test ./...`。文档锚点检查 286 文件 / 0 断链。
- CBM 已核对本批候选路径；3 处新模板的局部解析缺口来自协议 ID 占位与 Go 复合字面量，均已逐段读源码并以生成工程补证，未把图谱当作完整解析证明。
- `compose.md` 只更新配方入口、模块数和首次发放登记点等事实；skill 结构仍留给原会话。下一批仍是 D 的消费/升级等真实联动与 E 的任务事件，13 类夹具迁配方、独立 skill-only 验收及真实存储联合验收未完成。


### 场景线与六个补充模块（2026-11-12）

本会话承担新方案中“另一个 agent”的全部模块范围。已新增 actor、scene、monster、attribute、equipment、combat、dungeon、terrain、naming、cdkey、notice、gacha、friends、idle，共 14 个；全库 29 个。各模块的正式入口、持久/同步事实、恢复、上限和验收边界见模块 README。

- 共用接线：缺少 heartbeat/directory 时沿用正式 add mod；attribute 使用现有生成器。附加 DAO 声明复用实体聚合/加载/事务；actor 提供唯一 PlayerDao 同步工厂，scene 只登记公开视图。自身同步在所有声明就位之后开启，协议、配置、业务和生成物一起回滚。没有放宽多 DAO 的默认同步守卫。
- 服务停止：Scene 先关准入，再在管理锁外调用按 kind 登记的退出者；Monster 按 Scene 实例退场，避免 Terrain 先停时资源无法排空。Combat 用单个有界后台执行器提交双人事务；Nest 增加与 RequestSettled 同义的 RequestMultiSettled，真实完成前不释放资源。Sync Manager 经 Registry 暴露现有实例，未创建第二套调度器。
- 生命周期复核：Monster 的初生/重生/伤害/退出都携带不可变本地实例身份，取得 Monster 与 Scene 锁后校验；避免按 ID 排队期间发生同 ID 重建而误改新实例。Remove 已提交但回复丢失时，Scene 排空仍调用已登记退出者，不因成员列表已空而遗忘销毁责任。25 个 Monster 场景与 11 个可编译变异通过，见[红绿证据](evidence/gameplay-modules-20261112/README.md)。
- Combat 真实 Runtime 验收发现并修复 combatcomponent 成功结果遗漏 FailureReason=none；红绿证据见 CORE-REVIEW 的 RV-20261112-01。Runtime 严格检查保留，未知结果不伪装成功。
- demo 属性/装备改成模块，删除重复实现，协议为 EquipmentEquip；Actor Body 从同一模板消费。Player schema 升至 3，旧 schema 2 拒绝；新工程使用新数据/新 WAL，教学离线迁移示例同步到当前 schema。cellworld、LOD/队伍视图等独有适配仍保留，不能宣称 demo 全部迁移结束。
- 本批未发布，验收以当前 source-head 的 go.mod replace 为准。新增模块依赖 ModEntitySync 与 RequestMultiSettled；发版后按 manifest.go 既有规则再提高最低 core 版本并验证真实下载，不在 tag 尚不存在时预设未来版本号。

最终验收已完成，原始失败、修复原因和补测都保留在[本批证据](evidence/gameplay-modules-20261112/README.md)：

- 完整模块脚本退出 0。正序/反序两套全新工程各 29 模块、15 组联动、43 项报告、598 PASS / 0 SKIP；build/vet、ID/生成漂移、重复安装、全包测试全部通过。增量安装阶段也完整通过。
- 全仓首轮 9,770 PASS / 99 条条件 SKIP / 2 FAIL；两个失败是生成文档遗漏 Directory 和测试误要求同步 AttributeGear，修复后原入口均补测通过。之后组合验收发现的 Nest 文件级导入问题与六条 DAO 夹具错误也已修复并通过完整组合；不把定向补跑记作第二次全仓通过。
- 新 game-demo 774 PASS / 14 条预期 SKIP / 0 FAIL；模块验收 229 PASS / 0 SKIP。另补 Robot 字段名回归并通过 loadtest 包 race；新生成器再次核对 demo 无生成漂移。Monster、Combat、Nest 完成/排空与其他相关 race/变异证据分别保留。
- CBM 已刷新，所有本批代码证据路径核对 freshness；局部模板解析范围直接读源码补证。`compose.md` 仅两处事实更新，结构未改。

本会话分工的 14 个模块已实现并通过上述验收；不代表整份 backlog 已清空。仍未做本批模块的真实存储/Gate 联合验收、Linux、跨机、性能与长稳；demo 剩余专有适配及原会话的任务仍按本文和补充分工推进。本批仅提交推送 `feat/business-modules`，没有合入 main 或发版。

## 附录：R1 GM 运维入口（2026-11-12）

新需求文档 R1 已实施，提交 `8a49fe25`。gm 单独提供登记、审计与 ops 接线；wallet / bag / profile / mailbox / rehome 通过各自 `with/gm` 文件接入。每条通用命令要求操作者与备注，参数拒绝带码，实体修改仍经正式 Sender。补偿邮件要求稳定 request_id，发件在实体锁外；demo 的通用命令迁入模块，经验仍同时更新 Player 与 World。

GM 基础 9 条、每组联动 4 条；新 game 合计 161 PASS / 0 SKIP，8 个变异全部检出，定向 race 两轮通过。新 demo 的 GM、经验事务场景和服务编译通过；清单/联动/安装步骤、根包门禁与文档检查通过。首次编译和夹具失败及修正保留在[验收说明](evidence/module-gm-20261112/README.md)。未跑全仓、完整 demo、全模块正反序新装、真实 HTTP/NATS/Redis/Mongo/TCP；这些仍按新需求 §2、§6 最后统一验证。CBM 已刷新到 2026-11-12T02:48:06Z，新增错误码模板的占位符局部解析缺口已逐行读源码补证。

后续继续新需求 R2～R11 与 §5、§6；登录线和最终拼装 skill 重写仍归原会话。guild 先出方案，worldstats 暂不做。

## 附录：R2 Purchase 付费发货（2026-11-12）

新需求 R2 实现与定向验收完成，提交 `cc0d7855`。purchase 模块通过平台 HMAC + 工程渠道校验，按玩家 Redis 顺序队列保存奖励快照；只在平台终态无在途投递时进入 Nest，Player 连续游标与奖励同一笔事务，确认后清理待发。默认币、bag/enter 联动，离线订单不设到期；平台投递次数上限不变。demo 删掉独立 drain、不断增长的 PurchaseClaims，schema 4，只保留商品/模拟渠道与展示查询。

基础 21、bag 4、enter 2 条；新工程合计 72 PASS / 0 SKIP，13 个变异与两轮 race 通过；真实独立 Redis 3 条 × 2 轮；demo 定向场景及服务/Robot 编译通过。根包、清单、文档检查通过。初次入口/链接失败与最终边界见[证据](evidence/module-purchase-20261112/README.md)。CBM 本批路径已刷新补证；全量组合及 §6 真实联合验收仍延后。后续继续 R3～R11、§5、§6；guild 先出方案，worldstats 不做。

## 附录：R3 升级奖励邮件联动（2026-11-12）

mailbox + profile 联动已实现：默认不发奖，升级事务冻结附件与起止等级到 effect；一次连升一封，消费者以 EffectID 发件，App 负责排空。demo 改用同一联动，只留奖励规则，双实体经验路径不变；奖励热更现在在升级时取快照，非法奖励回滚升级。

联动 10 条，新工程 76 PASS / 0 SKIP，6 个变异、两轮 race、demo 定向升级/经验场景与服务/Robot 编译通过；提交 `7f91c18f`。原始错误断言与修正、真实依赖边界见[证据](evidence/module-profile-mail-20261112/README.md)。全量与真实联合验收按最终批次；后续继续 R4～R11、§5、§6。

## 附录：R4/R5 事件与任务（2026-11-12）

实现 wallet/shop/stamina/dungeon/leaderboard 命名登记点，task 事件推进、配置表、每日/每周/永久、前置链、道具奖励与八组联动。实体钩子使用冻结时间、与业务一起回滚；排行通知是尽力推进。默认任务不变、原 18 条场景保留，进度与领取记录按固定 ID 空间有界。新角色在首次进入事务中默认 1 级、不触发升级奖励，attribute 同事务派生；普通购买不默认使用 Saga。

最小 49、组合 215 PASS / 0 SKIP，15 个变异与两轮定向 race 通过；profile 新 enter 依赖影响的邮件/GM/attribute/equipment 组合复验通过（邮件 99、GM 199）。根包、清单与文档检查通过。失败过程、CBM 补证与真实依赖边界见[报告](evidence/module-task-events-20261112/README.md)。全矩阵、全 demo、全仓与 §6 联合验收仍延后；后续 R6～R11、§5、§6，guild 先出方案、worldstats 不做。
