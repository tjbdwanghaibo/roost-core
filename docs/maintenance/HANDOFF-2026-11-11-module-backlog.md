# 业务模块待办交接（2026-11-11）

给接手的 agent。维护者 2026-11-11：“把想做的列出文档，我让另一个 agent 处理，rehome 你已经开头了，还是你来做”。本文列的是**换服（rehome）之外**还要做的事；分支 `feat/business-modules`（未合入 main，不要自行合并或发版）。

## 0. 先读

1. [业务模块与配方方案](FEATURE-2026-11-10-business-modules.md)：决定一览、§18（模块清单与 task 的扩展方向）、§19、§20（做到哪了）。
2. [modules/README](../../modules/README.md)：清单格式、联动文件（`with`）与登记点、**约定**（尤其“模块可以做全、做复杂，但简单用法必须照样能用”）。
3. [能力清单](../framework/CAPABILITIES.md)、[KNOWN-LIMITS“业务模块”](KNOWN-LIMITS.md#业务模块roost-module)。
4. 仓库规则：[AGENTS.md](../../AGENTS.md)、[维护手册](README.md)。

## 1. 不要碰的范围（另一个会话在做）

单个玩家换服：`modules/rehome/**`、`modules/enter/` 里的“进入前检查”登记点、game-demo 的 `demo/internal/service/game/playerowner.go.tmpl`、`gift_saga.go.tmpl` / `alliance_saga.go.tmpl` 的准入（`admitBound`、`playerMoving`）、`service/account` 的换服接口、DataEngine 的 `HandOffEntity`。方案见 [FEATURE-2026-11-11-player-rehome](FEATURE-2026-11-11-player-rehome.md)；半成品在 worktree `worktrees/rehome-module`（分支 `tmp/rehome-module`），不要动。

## 2. 做事的规矩

- **demo 比模块多的能力搬进模块**，demo 只留规则数据（覆盖模块留给工程的业务文件或配置表）与确属它独有的玩法；全新 `-template game` 工程不做任何配置直接 `roost module add <模块>` 必须能用、验收全绿。
- **不为已生成的工程做兼容**（未上线）：DAO 字段、存储键、协议与消息号、错误码都可以变；但 demo 自己要自洽（迁移示例、GM、压测机器人、README / WALKTHROUGH）。
- demo 换用一个模块的做法照已有的七块（enter、chat、leaderboard、bag、profile、mailbox、skillbook）：`codegen/internal/roost/demo.go` 的 `demoModuleSteps()`，全部模块在同一步里安装、新模块接在列表末尾，覆盖模块文件的 `write` 排在它之后，建在模块组件上的 handler 不要 `roost add handler` 骨架步骤（`TestDemoStepsLeaveModuleComponentsToTheModules` 钉住）。
- 每个模块：场景测试（不用子测试、数字写死、只断言自己的事实）、变异自检、`module.yaml` 的 `verify.pass`；场景数写进 `scripts/test-modules-generated.sh` 与 modules/README。
- **验证节奏**（维护者 2026-11-11）：每一步只跑与改动直接相关的检查（模块自己的 `roost module verify`、`go test -run 'TestModuleCatalog|TestModuleWith' ./codegen/internal/roost`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`）；完整测试——`scripts/test-modules-generated.sh`、`go test -run 'TestDemo|TestModule' ./codegen/internal/roost`、重新生成 game-demo 跑全量、全仓 `go test ./...`——**最后统一跑一次**。没跑的在提交说明与报告里写明。
- DataEngine / Nest / Sync 三块基础不为这些工作改动；确需改动先报告维护者。全程 `GOWORK=off`；不等 GitHub CI；额度紧张时把未完成的写进文档、推送、停下。

## 2.1 本批接续结果

- A1 + skillbook 已实施。清单声明 `runtime.package/start`；安装归属含 service、requires、runtime；生成 `modules_gen.go`，bootstrap 使用 `NewWithModules()`，业务 Service 文件不改写。依赖顺序启动、反向停止，启动失败清理、停机失败保留依赖与回调。契约见 [modules/README](../../modules/README.md#服务级启动与停机)。
- demo 的技能目录和查询统一使用模块，移除 controller 自己的目录缓存与重复编译。场景直接使用 Service 自己的同步观察器，不再从 Registry 断言具体 `*Service`；模块适配层保留 `NestSyncObserver` 等可选接口。
- 验收发现并修正：适配层生成标记必须复用 `generatedHeader`；重写 bootstrap 必须保留实体 owner / route / preload 和配置派生信息；doctor 需要目录编译测试入口，使用与启动相同的 CompileEnvironment。失败记录未按“通过”统计。
- 已完成的定向验证：安装回滚、重复安装不变、业务文件保留、依赖顺序/环/缺失、五种实际生成适配层的生命周期场景；skillbook 8 条、目录编译 1 条。三项变异（返回共享切片、停止后仍可读、吞掉编译错误）全部被对应测试抓住，恢复后通过。
- 独立 skill 验收：自定义 module 工程安装 10 模块 + 3 联动，177 PASS / 0 SKIP；全工程 217 PASS / 1 预期 Compose SKIP。初次 doctor 暴露的两项问题，用新 CLI、新工程重验已通过（skillbook 8/0、目录编译 1、skills:catalog 与 project-templates OK）。离线替身，不等于真实 TCP/存储验收。
- 最终完整验证：`GOWORK=off go test -timeout 30m ./...` 164 个有测试的包全部通过（含全部 Demo/Module 用例）；`scripts/test-modules-generated.sh` 通过，12 模块 + 4 联动共 206 PASS / 0 SKIP，增量安装、换顺序安装、重复安装及工程检查通过。重新生成 game-demo：47 个有测试的包、670 PASS / 14 预期 SKIP / 0 FAIL；跳过项为显式开启的真实存储、部署或性能检查；build / vet、模块验收 135/0、ID 与生成物漂移全部通过。根包检查通过，文档锚点 284 文件 / 0 断链。测试原始证据位于本机 `/tmp/roost-a1-*`，独立验收为 `/tmp/roost-skill-acceptance.SzbE5W/acceptance-summary.json`；临时路径仅用于本机复核，持久结论见本段与 CHANGELOG。
- CBM 已重建当前 worktree 索引（2026-11-11）；改动文件逐一核对覆盖。新增两处模板局部解析缺口是测试复合字面量与协议 ID 占位，已读取源码并通过生成工程验证，未把图谱当成完整解析证明。
- 下一批从 A2 + 真实实体模块继续；A3、equipment/attribute/purchase、C–I 以及 J 的真实依赖路径均未完成。rehome 分工范围未改，没有合并 main 或发版。

## 3. 待办（按建议的先后）

### A. 安装器的两个缺口（先做，解开一多半剩余业务）

| 项 | 要什么 | 谁在等它 |
| --- | --- | --- |
| A1 服务级接线点（已实现） | 模块能声明“服务 `Init` 时启动、`Shutdown` 时停”的东西（步骤消费者、effect 订阅、后台循环、心跳、同步接线），安装器把它接进业务服务的启动与停机顺序；归属记进 `roost.yaml` | gift、升级奖励邮件消费者、活动、GM、匹配组队、副本、scene、rehome 的后台驱动 |
| A2 模块自带实体 | 模块能带自己的 Entity（`roost add entity` 作为安装前置或由模块文件带来）、它的 DAO、lifecycle 与实体号分配 | 公会、队伍、战斗房间、room / matchroom / turn、scene |
| A3 定时器类型分配 | 模块能申请实体定时器类型号 | turn、活动 |
| A4（可选）保留号 | demo 删掉写死的号之后，模块的号按最小空号重排；要稳定需要“保留号” | game-demo |

每项都要有一个真实用到它的模块一起做，不先造空机制。

### B. 现在就能从 demo 迁的（不依赖 A）

| 模块 | 内容 | 依赖 |
| --- | --- | --- |
| equipment | 穿戴、卸下、槽位由道具决定；与背包同一笔事务 | bag |
| attribute | 基础值、装备加成、等级成长、合成最终值（`gameplay/attribute`） | profile；与 equipment 的联动 |
| purchase | 付费订单恰好一次发货、订单账本（现在挂在 demo 的 `BagComponent` 上：`claim_ledgers.go`） | bag、托管 platform 服务 |
| skillbook（已实现） | 技能目录：启动时编译、客户端可查；demo 只覆盖 JSON 规则数据 | A1 |
| worldstats | World 上的全服累计（进入人数、成局数、发放的经验） | 看是否值得单独成模块 |

### C. A1 之后能迁的

gift（送礼 saga，路由步骤，v1.44.0）、升级奖励邮件消费者（并进 mailbox + profile 的联动）、activity（活动榜、定时结算、发奖）、gm（运维命令）、matchmaking（排队、成局）、dungeon（进入、结算、领奖账本、通关计数；通关榜已经用 leaderboard）。

### D. A2 之后能迁的

guild（联盟服务：独立进程、建盟 / 捐献 / 领奖三个 saga、成员名单、game 读镜像）、team（队伍实体，与 matchmaking 一起）、lockstep 战斗房间；夹具里的 room（带座位的房间）、matchroom、turn（另要 A3）。

### E. 场景（维护者 2026-11-11 同意的顺序）

A1 + A2 → 把 demo 的场景（Scene 实体、进出、移动与视野、刷怪、心跳与同步接线，约 2900 行）拆成模块 `scene` → 加功能：多场景管理（按需创建销毁、切换）→ 区域触发（可接 task 的事件）→ 附近聊天（chat + scene 联动）→ 分线 → 可交互对象。怪物、巡逻、分区场景、可编辑地形与建筑随它处理。

### F. task 扩成核心系统

见方案 §18 末尾：按事件推进（`Report(事件, 次数)`）、每周 / 成就 / 任务链、奖励种类（与 mailbox 的附件同一种表达）、任务表做成配置表、各模块上报事件的联动。

### G. 还缺的登记点（各自要有真实使用者）

wallet 的“消费成功”（区分 `Apply` 命中去重回执与首次生效；task 的“消费三次”在等它）、profile 的升级（现在是业务文件钩子，别的模块挂不上）、enter 的首次发放（事务内）、leaderboard 的提交时机、stamina 的消耗、shop 的购买成功。

### H. 规则数据

shop 的商品目录、task 的任务表改成配置表（照 bag 的道具表）；规则导出给客户端（stamina 的上限与周期、当前业务日）。

### I. 配方与玩法夹具

`roost project new -recipe <name>`；13 类玩法夹具（`scripts/testdata/<genre>/`）迁成模块组合。

### J. 验证欠账

- 独立 skill 接入已补一轮（见 §2.1）：覆盖第二、三批中 bag、mailbox、leaderboard、task、stamina、shop，三组联动与 skillbook；profile、chat 与完整 game-demo 的 skill 独立实测仍待补。
- 真实依赖：game-demo 的 `make dev-run` / `dev-smoke` 与压测机器人（邮件、排行榜、背包、档案的协议都换过）、`scripts/test-demo-generated.sh`、`scripts/test-mmorpg-cross-sid.sh`（客户端已改写、只编译过）、mailbox 经 NATS / Redis 的路径、模块端点经 TCP 的整条链路。
- game-demo 的背包同步有一处冗余（KNOWN-LIMITS）：`Player.PublishSyncDirty` 改用 `TakeEntitySyncChanges()` 可消除。

### 不建议做成模块的

登录鉴权（`auth.go`）、运维工具（搬服、合服、数据迁移、压测机器人）、部署与监控配置：留在 demo 作参考。

## 4. 等维护者定的

- task 的扩展排在 A / E 之前还是之后。
- B 组里 worldstats 是否单独成模块。
- A4（保留号）做不做。
