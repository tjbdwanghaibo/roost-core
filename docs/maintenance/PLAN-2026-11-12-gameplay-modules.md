# gameplay 包与清单外模块的模块化方案（2026-11-12）

维护者 2026-11-11：“这个方案你来做部分……actor 的可以拆出来和剩下的部分，先整理出文档，我给另一个 agent”。本文是[待办交接](HANDOFF-2026-11-11-module-backlog.md)的补充：`gameplay/` 各包怎么进模块、交接清单之外还能加哪些模块、两个会话各做哪部分。规矩与验证节奏照交接文档 §3。

## 1. 判断标准

`gameplay/` 下的包是**机制库**（怎么算移动、怎么编译技能），模块是**接好线的业务单元**（状态 + 组件 + 协议 + 测试，装上就能用）。包不搬进模块；由模块把它们包一层接到实体上——game-demo 里这层接线现在靠手写（心跳 8 个文件、移动 7 个文件）。

## 2. gameplay 各包的处理

| 包 | 是什么 | 处理 | 归谁 |
| --- | --- | --- | --- |
| heartbeat | 实体的周期逻辑驱动（Mod） | 不做模块。安装器加一个小改进：模块 `needs.mods` 里要它而工程没有时代办 `roost add mod heartbeat`，不是只报错 | 场景会话 |
| movement | 位置、朝向、路径 | **`actor` 模块**：实体的心跳驱动、动作词表、状态目录与移动接线（game-demo 的 `game/actor`、Player 的 `map_component`）。从场景里**独立出来**，scene 依赖它 | 场景会话 |
| state | 状态位（定身、沉默……） | 并进 `actor`：能不能动、能不能放技能由它门控 | 场景会话 |
| timedomain | 场景内倍速、时停 | scene 的可选联动，不单独成模块 | 场景会话 |
| attribute | 属性分层与合成 | **`attribute` 模块**（交接清单 B 组已有），与 profile（等级成长）、equipment（装备加成）各一组联动 | 场景会话 |
| skill | 技能编译器与运行时 | `skillbook` 已有；下一步 **`combat` 模块**：实体上放技能、结算、buff、战斗同步。依赖 actor、attribute | 场景会话 |
| ai、actionflow | AI 决策边界、动作与任务流 | **`monster` 模块**：巡逻、感知、追击。依赖 actor、scene | 场景会话 |
| level | 关卡组件（多阶段、事件 / 条件 / 动作） | game-demo 没用到。让 **`dungeon` 模块**用它表达副本阶段，这个包才有真实使用者 | 场景会话 |
| physics | 固定步长模拟 | 暂不做，留给帧同步战斗房间 | — |
| terrain | 可编辑地形 | **`terrain` 模块**（地形块实体、编辑、建筑），排在 scene 之后 | 场景会话 |
| cellworld | 多进程分区场景 | 不做模块，仍是原型 | — |

### actor 独立出来的要点（最急：场景已开工）

- 范围：Player / Monster 共用的心跳驱动、动作词表、状态门控、移动（`movement.MapBasic` 的接线：位置、朝向、路径、落库的存档位置）。**不含**“在哪张图上、谁看得见谁”——那是 scene。
- 对外入口要稳定并写进模块说明：别的模块（combat、monster、rehome 的离开钩子）靠它们判断“能不能动”“现在在哪”“停下来”。
- 登记点：进入 / 离开地图、每拍之后（给 combat 的周期效果、monster 的决策用）。
- scene 以 `requires: [actor]` 依赖它；没有 scene 的工程（例如只要房间内站位）也能单独装 actor。
- 不要把移动与心跳的接线写死在 scene 模块里——拆不出来的话先停下来报告卡在哪。

## 3. 交接清单之外可加的模块

| 模块 | 做什么 | 为什么 | 归谁 |
| --- | --- | --- | --- |
| **login** | 用 account 服务的会话令牌做登录鉴权，替换生成的“一律拒绝”的鉴权桩（`replaces`）；渠道验签仍留给项目 | 最关键的缺口：现在装了 enter 真实客户端仍连不上 | 原会话 |
| **servertime** | 进入游戏时下发服务器时间、当前业务日、下次重置时刻 | checkin、task、stamina、shop 都按业务日清零，客户端各自推算容易错 | 原会话 |
| **naming** | 名字全服唯一（profile + directory 服务的联动） | profile 现在改名不查重 | 原会话 |
| **cdkey** | 兑换码：一码一次或一人一次，发奖走 mailbox | 运营常用 | 原会话 |
| **notice** | 公告、跑马灯（chat 的系统频道） | 运营常用 | 原会话 |
| **gacha** | 抽卡：概率表（配置表）、保底、十连 | 卡牌类必备，夹具里有做法 | 原会话 |
| **friends** | 好友、黑名单、申请 | 社交类必备 | 原会话 |
| **idle** | 离线收益：按时长惰性结算 | 放置类必备，夹具里有做法 | 原会话 |
| 各模块的机器人步骤 | 每个模块带自己的压测 / 验收步骤 | 现在所有模块验收都是离线的 | 待定 |

## 4. 分工与先后

**场景会话（另一个 agent）**：`actor`（先于或同时于 scene 定下来）→ `scene` → `monster` → `attribute` / `equipment` → `combat` → `dungeon`（用上 level）→ `terrain`；安装器对 heartbeat 的代办。交接清单里的其余项照旧。

**原会话**：`login` + `servertime`（先做，直接决定半成品能不能真的连上、客户端显示对不对）→ `naming`、`cdkey`、`notice` → `gacha`、`friends`、`idle`。另外收尾换服审查的 RV-20261111-BM-04 / 05（在 worktree `worktrees/fix-rehome-bm45`，分支 `tmp/fix-rehome-bm45`，本文也暂时提交在这个分支上，随修复一起合回主线）。

**两边都要注意**

- 原会话的模块只挂 Player、不碰场景与 actor；会动的公共文件是 `scripts/test-modules-generated.sh`、`modules/README.md`、`docs/framework/CAPABILITIES.md`、`CHANGELOG.md`——各自只加自己的行，合并时取并集。
- 原会话在自己的 worktree 里做、合回前先并入主线的最新提交；场景会话直接在 `feat/business-modules` 的检出里做。
- main 上有四项地形修复（`2ec5c577`，RV-20261110-01～04：`Replicator.Want` 返回错误、水深改 64 位、`Meta` 一律是副本）还没并进 `feat/business-modules`；做 `terrain` 模块或动 demo 的地形分区之前先把 main 并进来。
- 给 agent 用的 skill 最后由原会话统一重写，迁模块时只对 `compose.md` 做事实层面的跟进。
