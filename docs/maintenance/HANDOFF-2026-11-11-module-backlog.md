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
- demo 换用一个模块的做法照已有的六块（enter、chat、leaderboard、bag、profile、mailbox）：`codegen/internal/roost/demo.go` 的 `demoModuleSteps()`，全部模块在同一步里安装、新模块接在列表末尾，覆盖模块文件的 `write` 排在它之后，建在模块组件上的 handler 不要 `roost add handler` 骨架步骤（`TestDemoStepsLeaveModuleComponentsToTheModules` 钉住）。
- 每个模块：场景测试（不用子测试、数字写死、只断言自己的事实）、变异自检、`module.yaml` 的 `verify.pass`；场景数写进 `scripts/test-modules-generated.sh` 与 modules/README。
- **验证节奏**（维护者 2026-11-11）：每一步只跑与改动直接相关的检查（模块自己的 `roost module verify`、`go test -run 'TestModuleCatalog|TestModuleWith' ./codegen/internal/roost`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`）；完整测试——`scripts/test-modules-generated.sh`、`go test -run 'TestDemo|TestModule' ./codegen/internal/roost`、重新生成 game-demo 跑全量、全仓 `go test ./...`——**最后统一跑一次**。没跑的在提交说明与报告里写明。
- DataEngine / Nest / Sync 三块基础不为这些工作改动；确需改动先报告维护者。全程 `GOWORK=off`；不等 GitHub CI；额度紧张时把未完成的写进文档、推送、停下。

## 3. 待办（按建议的先后）

### A. 安装器的两个缺口（先做，解开一多半剩余业务）

| 项 | 要什么 | 谁在等它 |
| --- | --- | --- |
| A1 服务级接线点 | 模块能声明“服务 `Init` 时启动、`Shutdown` 时停”的东西（步骤消费者、effect 订阅、后台循环、心跳、同步接线），安装器把它接进业务服务的启动与停机顺序；归属记进 `roost.yaml` | gift、升级奖励邮件消费者、活动、GM、匹配组队、副本、scene、rehome 的后台驱动 |
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
| skillbook | 技能目录：启动时编译、客户端可查 | — |
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

- 用一个只凭 skill（[roost-genre](../agent-skills/roost-genre/SKILL.md) 的“按需求组合”）的 agent，按一段不与样例重合的需求实测：第二、三批模块、联动、demo 换用之后的模块都没有被这样测过。
- 真实依赖：game-demo 的 `make dev-run` / `dev-smoke` 与压测机器人（邮件、排行榜、背包、档案的协议都换过）、`scripts/test-demo-generated.sh`、`scripts/test-mmorpg-cross-sid.sh`（客户端已改写、只编译过）、mailbox 经 NATS / Redis 的路径、模块端点经 TCP 的整条链路。
- game-demo 的背包同步有一处冗余（KNOWN-LIMITS）：`Player.PublishSyncDirty` 改用 `TakeEntitySyncChanges()` 可消除。

### 不建议做成模块的

登录鉴权（`auth.go`）、运维工具（搬服、合服、数据迁移、压测机器人）、部署与监控配置：留在 demo 作参考。

## 4. 等维护者定的

- task 的扩展排在 A / E 之前还是之后。
- B 组里 worldstats 是否单独成模块。
- A4（保留号）做不做。
