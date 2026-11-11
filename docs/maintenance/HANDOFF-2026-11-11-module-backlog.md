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

## 2. 现在的状态（2026-11-11 晚）

| 项 | 状态 |
| --- | --- |
| 模块 | 15 个：wallet、checkin、task、stamina、shop、bag、profile、chat、enter、leaderboard、mailbox、skillbook、room、turn、rehome；场景数以各自的 `module.yaml` 与 `scripts/test-modules-generated.sh` 为准 |
| 模块间联动 | 5 组：chat + enter、mailbox + enter、mailbox + bag、task + checkin、rehome + enter |
| 登记点 | enter 的进入前检查（能否决登录）与进入钩子；checkin 的签到钩子；mailbox 的附件种类；rehome 的换服前检查 / 离开时 / 到达后 |
| 安装器 | 服务级接线点（`runtime`）、模块自带实体（`entities`）、定时器类型分配（`timer_types`）都已实现；模块可以带配置表（bag 的道具表） |
| game-demo 用的模块 | enter、chat、leaderboard、bag、profile、mailbox（连带 wallet）、skillbook、rehome，全部在同一步里安装 |
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
| dungeon | 进入、结算、领奖账本、通关计数 | 通关榜已用 leaderboard |

### B. 不依赖新能力、随时可迁的

equipment（穿戴、卸下、槽位由道具决定；依赖 bag）、attribute（基础值、装备加成、等级成长；依赖 profile，与 equipment 联动）、purchase（付费订单恰好一次发货、订单账本——现在挂在 demo 的 `claim_ledgers.go`；依赖 bag 与 platform 服务）、worldstats（World 上的全服累计；是否单独成模块待定）。

### C. 配方（直接服务最终目标，模块已经够拼第一个）

`roost project new -recipe <name>`：一组模块加参数，一条命令出半成品。先做一两个最小的（例如“休闲”：enter + wallet + checkin + task + stamina + shop + bag + mailbox；“回合对战”：enter + profile + room + turn + leaderboard），用它们反过来检验哪些接线还要手写。13 类玩法夹具（`scripts/testdata/<genre>/`）迁成模块组合排在其后。

### D. 还缺的登记点与联动（每个都要有真实使用者）

- wallet 的“消费成功”（区分 `Apply` 命中去重回执与首次生效；task 的“消费三次”在等它）。
- profile 的升级（现在是业务文件钩子，别的模块挂不上）；enter 的首次发放（事务内）。
- leaderboard 的提交时机；stamina 的消耗；shop 的购买成功。
- rehome 的其余联动：mailbox（离开前按旧范围拉一次全服邮件）、chat（离开时移出在线名单）、leaderboard（按服的榜离开时移除）、room / turn（在房间里时拒绝换服）。

### E. task 扩成核心系统

维护者 2026-11-11：“task 是一个比较核心的系统，很多业务需求可以加到 task 中去。”见方案 §18 末尾：按事件推进（`Report(事件, 次数)`，别的功能只上报发生了什么）、每周 / 成就 / 任务链、奖励种类（与 mailbox 的附件同一种表达）、任务表做成配置表、各模块上报事件的联动（依赖 D）。

### F. 自带实体之后能做的

matchroom（匹配成局后移交到 room）、team（队伍实体）、guild（联盟服务：独立进程、三个 saga、成员名单、game 读镜像）、lockstep 战斗房间。room 还没有的：自动断线订阅、断线保留策略、重启后在线状态重建、广播。

### G. 场景（维护者 2026-11-11 同意的顺序）

把 demo 的场景（Scene 实体、进出、移动与视野、刷怪、心跳与同步接线，约 2900 行）拆成模块 `scene` → 加功能：多场景管理（按需创建销毁、切换）→ 区域触发（可接 task 的事件）→ 附近聊天（chat + scene 联动）→ 分线 → 可交互对象。怪物、巡逻、分区场景、可编辑地形与建筑随它处理。

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
- task 的扩展（E）排在配方（C）之前还是之后。
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
