# 功能设计：业务模块与配方（2026-11-10）

**状态：第 2 期闭环已实施，业务模块持续拆分中；服务级接线 A1 与 skillbook 已落地，配方和玩法迁移未完成。** 已有的：`roost module list / plan / add / verify` 与十二个模块、服务级生命周期接线、模块间联动（清单与状态见第 18 节，实施记录见第 19 节），game-demo 的进入、聊天、排行榜、背包、档案、邮件、技能目录已改用模块，现行说明在 [modules/README](../../modules/README.md)、[命令参考“业务模块”](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module) 与[能力清单](../framework/CAPABILITIES.md)，限制在 [KNOWN-LIMITS“业务模块”](KNOWN-LIMITS.md#业务模块roost-module)。还没有的：配方、现有 13 类玩法与 game-demo 的迁移——它们仍是 [roost-genre skill](../agent-skills/roost-genre/SKILL.md) 加 `scripts/testdata/<genre>/` 夹具与 `-template game-demo`。实施结果与方案的出入见[第 17 节](#17-第-2-期实施记录2026-11-10)。

**文档分两部分**：第 0～9 节是设计（做什么、为什么）；第 10 节起是实施方案（改哪些文件、怎么改、怎么验收），供实施者按期执行。实施者先读[第 10 节](#10-实施者须知)。

登记在 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。基线 `cf89e081`（main）。codebase-memory 的 MCP 与 CLI 在本轮都被代际锁挡住，第 1 节的现状结论全部按源码与 `rg` 核对，引用写到文件与行；标“推断”的没有运行验证。本轮没有改代码，也没有跑测试。

## 0. 目标与维护者原话

维护者 2026-11-10：roost-core 是通用框架，“既有底层框架也有业务模版”，后续要两个能力：

1. 按游戏类型直接生成一个基础的 demo（现在已有）。
2. 按需求描述，用 roost-core 提供的各种能力自定义组合出一个 demo；这个能力也要能补全能力 1 的产物，“后续需求变化可以在 game demo 中继续补全”。

两个能力的使用者既可以是外部的大模型 agent（经描述好的 skill），也可以是人类工程师（经学习文档手动使用）。维护者同时问：业务模板是否应从 demo 中抽离，还是 demo 中的业务模型支持任意插拔。

## 决定一览

| # | 事项 | 决定 |
| --- | --- | --- |
| 1 | 是否新增业务模块层 | 接受。新增 `roost module` 安装器，安装写入的源码归业务、之后不被覆盖；`roost generate` / `roost add` 仍只出框架骨架。AGENTS 里“玩法参考来自 `scripts/testdata/<genre>/`”的口径在机制落地的那一期改写，落地前不改 |
| 2 | 模块的持久状态放哪 | 挂在玩家等已有实体上的模块状态，作为**嵌套结构体**放在该实体的主 DAO 下，不新增独立 DAO（维护者同日改定，原话：“定义dao子结构体，还是放在主dao下，也能用到增量同步，还简单”）。同步的状态与纯服务端的状态各用一个结构体。不挂在已有实体上的状态（房间、联盟、地形编辑账本）仍用独立实体 |
| 3 | game-demo 的去向 | 分期拆成配方，从易到难；每拆一个功能，demo 全量测试保持通过。没拆的部分继续作为整体模板 |
| 4 | 是否支持卸载 | 不做。只做安装，并能列出每个模块拥有的文件与已分配的 ID；已分配的存储键、协议号、错误码不回收 |
| 5 | 已生成的旧工程（2026-11-11 追加） | 维护者：“模块需要继续拆解”；“不用考虑旧代码，能直接生成新的就行”，并澄清：“不是新写的意思，是对之前旧项目生成的旧代码可以不用考虑兼容，chat 修改后能直接用于新的就行”。即：模块、模板与生成器的改动只要求**新生成的工程**能直接用，不为以前生成出来的工程保留兼容或迁移路径。决定 3 不变：继续把 game-demo 与玩法里的业务拆成模块（第 12～14 节仍是待办），拆的时候不必顾及旧工程 |

备选与未采用的理由见第 7 节。

## 1. 现状

### 1.1 已有

- **框架与玩法包**：Nest、DataEngine、Sync、Remote、Saga、实体定时器与心跳、配置热更（[模块目录](../framework/README.md)）；`gameplay/` 下的技能与战斗、AI 与行为、移动、关卡组件、分区场景、可编辑地形、物理；`service/` 下十个领域服务（[Service](../framework/guide/09-services.md)）。
- **原子生成命令**：`roost add service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|table|dao|webroute|errcode|saga|effect|rpc|timer|attribute`，另有 `heartbeat` / `behavior` / `timedomain`（`codegen/internal/roost/help.go`、`add.go`）。实体类型、组件、协议的 ID 由 `roost.yaml` 的 ID 空间分配。`project sync / doctor / next` 与 `generate --check` 提供同步、诊断与漂移检查。
- **能力 1**：13 类玩法纵切（`scripts/test-<genre>-generated.sh`）与 `-template game-demo`；发版门禁每次跑缺省模式（[KNOWN-LIMITS“游戏接入验收”](KNOWN-LIMITS.md#游戏接入验收)）；公共 Robot 与批量验收入口 `scripts/test-genres-generated.py`（[Robot](../framework/ROBOT.md)）。
- **demo 的步骤表**：`codegen/internal/roost/demo.go` 的 `demoScaffoldStep`（`add` / `write` / `run` 三选一，带 `why`）与 `demoScaffoldSteps`，配 `{{MODULE}}`、`{{GAME_SERVICE}}`、`{{GAME_SERVICE_PKG}}`、`{{PROJECT}}` 占位与按服务名重映射的目标路径。这是最接近“模块安装器”的现成机制，但只服务一个固定的 demo，只在 `project new` 时执行一次。

### 1.2 没有

| 缺口 | 证据 |
| --- | --- |
| 机器可读的玩法或模块描述 | 实体、DAO 字段、协议、依赖的服务写在各脚本的内联 python 与散文手册里；`test-genres-generated.py` 只有一个玩法名字列表，期望场景数写死在各 shell 里 |
| 可重复、可叠加的安装 | 夹具对 `type PlayerDao struct {` 做字符串插入，不判断字段是否已存在；`Gold`、`Coins`、`Initialized` 在多个玩法里重复。`add dao` 遇到已存在报 `already exists`（`add_entity.go`） |
| 统一的 ID 分配 | 9 个玩法的 `scripts/testdata/<genre>/robot/protocol.go.txt` 都写死 `//roost:msg id=15011`；公共会话夹具与个别玩法另用固定的 15001～15003。demo 的错误码 100001 起连续手写，只有前 11 个经 `add errcode` 进入 `roost.yaml`（`demo.go` 的步骤表） |
| 一个实体上多模块共存 | 一个实体只有一个定时器组件，再加报 `already has a timer component`（`add_timer.go`）；各玩法的定时器类型都从 1 开始并整文件覆盖 `timer_handlers.go`。`scripts/testdata/robot-common/install.py` 无条件覆盖 `cmd/robot/main.go`、`loadtest/scenario.go` 等固定路径 |
| 运行期的模块接线 | demo 的 `internal/service/game/service.go.tmpl` 里 `Init` / `Shutdown` 是手写的顺序调用，没有模块列表或钩子表；`game/controllers/player/controller.go.tmpl` 的 `FromRegistry` 对联盟等做硬性查找，缺一个整个 controller 起不来 |
| 按功能归属的持久状态 | demo 的 `PlayerDao`（`demo/db/def/player.go.tmpl`）是 57 个顶层字段的扁平结构，混着副本、邮件、购买、公会、saga、分区、组队的字段；组件与功能不一一对应 |
| 抽出来的业务原语 | `const LedgerLimit` 在 6 个夹具里各有一份，幂等序号账本的实现近乎逐行相同；带座位的房间、终态领奖、规则目录、唤醒定时器也各有 3 份以上近似拷贝 |
| 统一的模板占位 | 夹具并存三种写法：写死 `example.com/<genre>-practice`、字面量 `GENRE_MODULE`、`MMO_MODULE`；服务名 `game` 写死 |
| 组合验收 | 只有“新工程 + 单个玩法”的验收，没有两个模块叠加的用例 |

### 1.3 demo 为什么不适合原地插拔

demo 有 371 个模板文件、约 5.9 万行（含测试）。三个包汇集了全部功能：`game/handler`、`game/controllers/player`、`internal/service/game`；另有几处进程级全局单例（地形图、分区节点、场景地面）。生成侧 `demoScaffoldSteps` 是一张按顺序的扁平清单，只有联盟被部分拆成 `demoAllianceSteps`，且主清单里仍留着公会的错误码与 `PlayerDao.GuildID` 的引用；`TestDemoTemplateStepsAndShippedFilesAgree` 与生成后的存在性断言把整体锁在一起。地形与分区已经是配置开关，但关掉后实体类型、DAO、handler、协议照样编译注册，是运行期开关而不是装配开关。

## 2. 设计

### 2.1 分层

```
配方      slg / mmorpg / game-demo / 用户自定义        = 模块清单 + 参数
模块      wallet / bag / room / chat / alliance / dungeon / terrain …
原子命令  roost add entity|component|dao|handler|…     （已有）
框架      framework / gameplay / service / wiring       （已有）
```

- **模块**：一个业务单元，自带状态结构体、组件、handler、协议、错误码、配置表、测试和 Robot 步骤。
- **配方**：一份模块清单加参数。13 类玩法与 game-demo 各是一份配方。能力 1 是能力 2 的特例，所以按玩法生成的工程可以继续用同一套命令补全。
- **边界**：生成器不产出业务代码；模块由安装器写入，写入后归业务，`project sync / upgrade` 不改写。

### 2.2 模块的形态

每个模块一个目录：一份清单、一组模板文件、自带的测试与 Robot 步骤、一页说明。清单示意（字段名在第 2 期定稿）：

```yaml
name: wallet
summary: 玩家货币：余额、按业务键幂等的加减、消费协议
requires: []                        # 依赖的其他模块，安装时先装
conflicts: []                       # 不能同装的模块
needs:
  mods: [nest]
  features: [protocol, entity, dao, errcode]
entity: Player                      # 状态与组件挂在哪个实体上
state:                              # 追加到该实体主 DAO 末尾的字段
  - {field: Wallet,       type: WalletState,  store: wallet,        dao: "persist,sync"}
  - {field: WalletLedger, type: WalletLedger, store: wallet_ledger, dao: "nosync"}
component: Wallet                   # 挂在 entity 上的组件；组件号安装时分配
protocols: [WalletSpend]            # 模板里 {{ID:protocol:<name>}} 的名字；号安装时分配
errcodes: [wallet_short, wallet_invalid, wallet_conflict, wallet_overflow]
verify: {packages: [./game/handler], run: '^TestWallet', pass: 8}
```

写入的文件不列在清单里，由模块的 `files/` 目录读出。这是第 2 期落地的格式（`modules/wallet/module.yaml`）；模块源码在仓库 `modules/`，与 demo 一样嵌入 CLI。

### 2.3 命令

| 命令 | 作用 |
| --- | --- |
| `roost module list [--json]` | 列出模块与配方、依赖、提供的能力 |
| `roost module plan <名字…> [--json]` | 只预览：将创建的文件、追加的 DAO 字段、分配的 ID、依赖与冲突 |
| `roost module add <名字…>` | 安装并记入 `roost.yaml` 的 `modules:`；重复执行无变化 |
| `roost module verify [名字…]` | 跑模块自带场景并核对通过数与零跳过 |
| `roost project new … -recipe <配方>` | 按配方装一组模块 |

`plan` 的 JSON 输出是 agent 的主要读取面；人读同一份内容的文本形式。

### 2.4 状态归属（决定 2 的细则）

- 模块自带一个嵌套类型文件（`db/def/<module>.go`）。安装时只往实体主 DAO 的**末尾**追加一到两行字段，字段名带模块名，存储键显式声明。
- **为什么是嵌套结构体**：嵌套结构体的子字段变化已按路径打成同步补丁并按路径增量落库（[DAO 同步](../framework/DAO-SYNC-PROTOBUF.md)、[递归增量持久化](../framework/DATAENGINE-INCREMENTAL-PERSISTENCE.md)），不需要改框架；默认的自身视图打包照常工作；玩家仍是一份文档。
- **同步与否按顶层字段决定**：`dao:"nosync"` 只落库，`dao:"persist,sync"` 落库并下发。视图白名单（本人 / 附近 / 远处）也按顶层字段（`framework/entity/sync_view.go`）。所以既有客户端可见状态、又有服务端账本的模块，拆成两个结构体。嵌套类型内部的字段只声明存储键、没有单独的同步声明（推断：依据 demo 的 `Equipment` 写法与 `codegen/internal/dao/parse.go` 对嵌套类型的解析，未逐分支验证）。
- **追加在末尾**：同步的 PB 字段号按定义顺序排，插在中间会让后面的字段号移位。
- **字段数上限**：同步脏位是 64 位掩码，每个顶层字段占一位（`codegen/internal/dao/template_dao.go` 的 `1 << iota`），一个 DAO 最多 64 个顶层字段（推断，没有实际生成第 65 个验证）。每个模块只占一到两位。
- **共用的预算**：一个 DAO 的全部模块共用一份文档、一个 schema 版本和一份变化记录预算（每 DAO 4096 个变化节点，超出后收敛为整体替换）。

### 2.5 让模块能叠加的规则

1. **状态自带**：按 2.4，不往别的模块的结构体里加字段。
2. **ID 全走分配**：协议号、错误码、实体类型、组件、定时器类型都由 `roost.yaml` 分配，模板里不写死。
3. **分片注册**：一个实体上的定时器类型、心跳、Robot 步骤、GM 命令，每个模块一个注册文件，由生成器汇总，不整文件覆盖。
4. **统一接线点**：服务的 `Init` / `Shutdown` 只调用一次模块列表；模块实现一个小接口（初始化、启动、停止、可选的 GM 命令）。接口放在哪、是否复用 App 的 Mod 生命周期，在第 2 期读过 `framework/app` 后定。
5. **只经接口协作**：模块之间通过 Registry 能力接口、effect 或多实体事务的窄实体接口协作，不用进程级全局单例。可选依赖缺失时关掉对应端点，不让整个服务起不来。
6. **一种模板占位**：统一成 demo 的 `{{MODULE}}` / `{{GAME_SERVICE}}` / `{{PROJECT}}`。

### 2.6 验收

- 每个模块自带替身提交器场景与 Robot 步骤（自己的 PB 消息与分发注册），期望通过数写在清单里。
- 配方的验收等于其模块验收之和，再加配方自己的端到端场景；现有 13 类玩法迁移后场景数不得减少。
- 新增**组合验收**：在已装一个配方的工程上再装另一个模块（首条：把回合 PvP 的房间与规则模块装进 SLG 工程），build / vet / ID 检查 / 生成物漂移检查 / 场景数全部核对。
- 真实依赖与崩溃恢复仍是单独的 `REAL` 模式，口径不变：离线场景通过不等于生产验收。

## 3. 需要生成器先补的点

| 事项 | 说明 |
| --- | --- |
| 主 DAO 的结构化追加 | 按语法树在主 DAO 末尾追加字段，已存在且同型时视为已安装，冲突时报错；取代字符串插入 |
| 定时器类型分配与分片注册 | 定时器类型进 ID 空间；一个实体的定时器回调允许分在多个文件里注册 |
| 原子命令的复用模式 | `add dao` / `add entity` / `add timer` 遇到已存在时可选择复用，供安装器调用 |
| `roost.yaml` 的结构化合并 | `modules:` 记录、`uses` / `sagas` / `sync_groups` 的追加，取代字符串标记插入 |
| Robot 与协议的多模块共存 | 每个模块一条自己的消息与分发注册，`cmd/robot` 按已装模块组合 |

框架三大块（Nest / DataEngine / Sync）不需要为本方案改动。

## 4. 模块候选

- **先抽成 core 里的普通 Go 包**（不是模板；只抽夹具里已有 3 处以上重复的）：幂等序号账本、终态领奖回执、带座位的房间（座位、连接代际接管）、规则目录、唤醒定时器辅助、崩溃恢复测试骨架。
- **玩家域**：档案与经验、货币、背包、装备、属性、邮件领取、购买。
- **对局域**：匹配到房间的移交、回合引擎、帧同步战斗、副本运行。
- **社交与运营**：聊天、公会（联盟服务）、组队、排行、活动、GM。
- **世界域**：场景与移动、怪物与刷新、技能战斗、分区场景、地形与建筑。

具体拆哪些、粒度多大，按各期实际迁移的玩法决定，不预先开空壳。

## 5. 两类使用者

- **Agent**：`roost-genre` skill 扩成“需求 → 选模块 → `plan` → `add` → 模块库没有的部分用原子命令手写 → `verify` → 报告缺口”。模块目录从 `roost module list --json` 读取，不在 skill 里另抄一份。
- **人**：模块目录文档由清单生成；每个模块一页说明（做什么、不做什么、怎么改）。
- **边界照旧如实说明**：模块库没有的需求是手写业务代码；框架不支持的类型（见 skill 的“不适合 / 暂缓的类型”）不拼假 demo。

## 6. 分期

| 期 | 内容 | 验收 |
| --- | --- | --- |
| 1 | 本文；清单格式与目录约定 | 维护者确认 |
| 2 | 最小闭环：`roost module list / plan / add / verify`、`roost.yaml` 记录、第 3 节的生成器补点、统一接线点；用货币与聊天两个模块验证 | 新工程安装两个模块后 build / vet / ID 检查 / 生成物漂移检查通过；重复安装无变化；“主 DAO 下挂十来个模块结构体”的增量同步与按路径落库用例通过并记录开销 |
| 3 | 回合 PvP、合作 Roguelike、异步社交经营三个会话型玩法改成配方（它们已共用 `scripts/testdata/genre-common`，体量最小） | 三类场景数不减；第一条组合验收通过 |
| 4 | 抽原语包，迁移其余玩法夹具；发版门禁改为按清单驱动；改写 AGENTS 与 skill 的口径 | 13 类场景数不减；自定义 module 名在生成时生效，不再靠事后替换 |
| 5 | 拆 demo：聊天、送礼、排行、购买 → 副本、公会 → 场景、分区、地形 | 每拆一个，demo 的生成、build、vet、测试与冒烟保持通过 |
| 6 | 模块目录文档与每个模块的说明页 | 文档链接与锚点检查通过 |

第 5 期工作量最大，可以按需推进；每一期独立可停。各期的逐项改动见[第 11～15 节](#11-第-2-期最小闭环)。

## 7. 未采用的方案

| 方案 | 不采用的原因 |
| --- | --- |
| 在 demo 内部做任意插拔 | 见 1.3：要先重写三个汇集包与 `PlayerDao`，还要验收各种拔法的组合 |
| 业务模块并入 `roost add`，由生成器产出业务代码 | 框架骨架与业务代码的边界变模糊；与“生成器只出框架骨架”相反 |
| 每个模块一个独立 DAO | 实体有两个及以上 DAO 时，生成的 `TakeEntitySyncChanges` 把任一 DAO 的变化映射成整份重发，除非手写映射（`codegen/internal/entity/gen.go`、`framework/entity/sync_commit.go` 的 `MapDAOSyncChanges`）；默认自身视图打包只接受一个 DAO（`codegen/internal/entity/self_sync_packer.go`）；客户端按“一个命名空间一个 DAO”注册解析器（[DAO 同步“客户端接入”](../framework/DAO-SYNC-PROTOBUF.md#客户端接入)，只读了文档）。补齐要改生成器、Sync 契约与三端客户端 |
| 模块字段直接合并进 `PlayerDao` 顶层 | 字段名冲突、归属不清，且受 64 个顶层字段的上限约束 |
| 支持卸载 | 存储键、协议号、错误码分配后不能回收；要验收各种装拔顺序 |

## 8. 风险与未确认

- **嵌套结构体的实战覆盖少**：demo 里只有装备栏一处使用。第 2 期的专门用例是这条风险的收口点。
- **一份文档的大小与加载**：全部模块状态在一份玩家文档里，与现状相同；背包等大状态的增长没有测过。
- **统一接线点的形态未定**：是否复用 App 的 Mod 生命周期，要在第 2 期读过实现后决定。
- **模块升级**：已安装的模块源码归业务，上游模板变化后只提示差异、不自动合并；差异提示的做法在第 2 期定。
- 1.2、1.3 的数字（文件数、行数、字段数）来自 2026-11-10 对基线的一次统计，随 demo 变化。

## 9. 进度

| 期 | 状态 |
| --- | --- |
| 1 | 本文已写（设计与第 10～16 节的实施方案），四项决定已确认（2026-11-10） |
| 2 | 已实施（2026-11-10，分支 `feat/business-modules`）：安装器与四个命令、wallet 与 checkin 两个模块、自动化测试与发版门禁脚本、十个模块结构体的验证、能力清单。结果与推后的项见第 17 节 |
| 3～6 | 未开始 |

---

# 实施方案

以下各节供实施者按期执行。文件与函数名按基线核对过；标“建议”的是推荐做法，实施时读过代码发现更合适的做法可以换，但要在第 9 节“进度”里写明换了什么、为什么。

## 10. 实施者须知

### 10.1 先读什么

1. 本文第 0～8 节（设计与决定）。四项决定已定，不要重新讨论；实施中发现决定行不通，停下来写明证据，交维护者决定。
2. [AGENTS](../../AGENTS.md)、[维护手册 §3、§6、§7](README.md#3-日常修改与验证)。
3. 生成器：[codegen README](../../codegen/README.md)、[命令与标记参考 §3](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#3-业务骨架roost-add)、[项目生成器](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md)、[tag 手册](../../codegen/docs/TAGS.zh-CN.md)。
4. 现有“最接近的实现”：`codegen/internal/roost/demo.go`（步骤表与模板写入）、`add_timer.go`（往 DAO 定义里加字段、给实体加组件）、`add_entity.go`（组件与 DAO 接到实体）、`scripts/testdata/genre-common/run.sh`（最小的玩法装配）。

### 10.2 规则

- 结构发现先用 codebase-memory，不可用时如实写明并按源码与 `rg` 继续（AGENTS“Codebase Memory”）。
- 生成器不产出业务代码；模块模板写入后归业务，`project sync / upgrade` 不改写。demo 的业务逻辑在第 5 期之前不动。
- 不改 Nest / DataEngine / Sync 的运行时。发现必须改时停下来，单独写明原因交维护者决定。
- 项目未上线：存储与协议变化不做旧数据、旧进程兼容；拒绝旧写法时说清原因和手工改法。
- 每个工作项一个提交：先有能在改前失败的测试，再实现；提交信息写清行为变化。不把文档补丁和行为改动混在一个提交里（文档随它描述的行为一起提交除外）。
- 每期结束更新 CHANGELOG 的 `[Unreleased]`、对应现行文档和本文第 9 节。不发版，发版要维护者明确说“发 vX”。
- 不等 GitHub CI；本地 build / vet / 测试是标准。没跑的检查不能写成通过。
- 临时工程、worktree 用完删除，检查没有遗留的测试进程。

### 10.3 每个工作项的基本验证

```sh
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -count=1 .                                  # 仓库级门禁（gofmt、文档链接、提交号、pretag 守卫等）
GOWORK=off go test -count=1 -timeout 30m ./codegen/...         # 生成器；单包负载高时接近 10 分钟
GOWORK=off go generate ./... && git diff --exit-code           # 改了生成器或模板
python3 scripts/check-doc-anchors.py                           # 改了文档
```

每期结束再跑一次 `GOWORK=off go test -timeout 30m ./...` 和该期表里列的脚本。

## 11. 第 2 期：最小闭环

> 本节是实施前写的计划。已实施；实际做法与本节的出入、三项核对的结果、推后的工作项见[第 17 节](#17-第-2-期实施记录2026-11-10)，以第 17 节为准。

目标：在一个 `-template game` 的新工程上，用 `roost module add wallet chat` 装两个模块，重复执行无变化，工程通过全部检查。这一期不迁移任何现有玩法，也不动 demo。

工作项按顺序做，前面的是后面的前提。

### 11.1 先做三项核对（不写产品代码）

用一个临时生成工程验证本文两处“推断”和一处未知，结果写进第 9 节。任何一项与本文不符，先改本文第 2.4 节再继续。

| 核对 | 做法 | 本文的预期 |
| --- | --- | --- |
| 嵌套结构体的同步粒度 | 主 DAO 加 `A AState` 标 `persist,sync`、`B BLedger` 标 `nosync`；改 `A` 的一个子字段、改 `B` 的一个子字段，看生成的 `MarshalSyncChanges` 与默认自身视图打包的输出 | 只有 `A` 的那个子字段进同步补丁；`B` 的变化不下发；两者都按路径进持久变化 |
| 嵌套类型内部能否单独声明不同步 | 给嵌套类型的字段写 `dao:"nosync"` 一类标签，看生成器是否接受、是否生效 | 不支持（所以才要求拆成两个结构体）。如果支持，第 2.4 节改为“可以用一个结构体” |
| 顶层字段数上限 | 生成一个有 65 个顶层字段的 DAO | 生成或编译失败。把实际的报错文本记下来；报错不清楚时，在 `codegen/internal/dao` 的解析阶段加一条明确的拒绝（这是生成器改动，可以做） |

### 11.2 模块源码的位置与嵌入

- **建议**：仓库根新增 `modules/`，结构仿 `demo/`：

  ```
  modules/
    embed.go                      # package modules；//go:embed，仿 demo/embed.go
    README.md                     # 模块目录约定与“改这里的东西之后”
    wallet/module.yaml
    wallet/files/**/*.tmpl        # 按安装后的真实路径存放，多一个 .tmpl 后缀
    chat/module.yaml
    chat/files/**/*.tmpl
    recipes/<name>.yaml           # 第 3 期开始有内容
  ```

- 模板占位只有 `{{MODULE}}`、`{{GAME_SERVICE}}`、`{{GAME_SERVICE_PKG}}`、`{{PROJECT}}`（沿用 `demo.go` 的 `demoVars.apply`，把它提成两边共用的函数），再加一种 ID 占位 `{{ID:<kind>:<name>}}`（见 11.5）。
- 加一条与 `TestDemoEmbedCoversEveryFile` 同类的测试：磁盘上每个模板都被嵌入，每个 `module.yaml` 的 `files` 与磁盘上的模板一一对应。
- 新增顶层目录后跑根包测试：`dependency_boundary_test.go`、`workspace_module_graph_test.go`、`ci_paths_test.go` 可能要登记新目录。

### 11.3 清单与 `roost.yaml` 记录

- 新文件 `codegen/internal/roost/module_manifest.go`：`ModuleSpec`（字段见第 2.2 节的示意）、严格解码（未知键拒绝）、校验（名字合法、`files` 非空、`requires` / `conflicts` 指向存在的模块且无环、`state` 的字段名以模块名开头）。
- `manifest.go` 的 `Manifest` 加 `Modules map[string]InstalledModule`（`yaml:"modules,omitempty"`）。`InstalledModule` 记录：模块版本（清单内容的哈希即可）、安装时的参数、分配到的每个 ID（`protocol` / `errcode` / `component` / 定时器类型，名字 → 号）。这份记录是“归属清单”的数据来源，也是重复安装判等的依据。
- `validateManifest` 加校验：记录里的 ID 落在对应 ID 空间内、互不重复。
- 生成工程的 `docs/ROOST_YAML.zh-CN.md`（`render_docs.go`）加 `modules` 一节。

### 11.4 结构化改写的三个原语

都放在 `codegen/internal/roost/`，各带表驱动测试，要求**幂等**（再执行一次输出不变）与**冲突明确报错**。

| 原语 | 现有可参照的实现 | 要求 |
| --- | --- | --- |
| 往主 DAO 末尾追加字段 | `add_timer.go` 的 `addTimerDaoFields`（在结构体右括号前插入文本、保留业务注释）与 `findStruct` | 输入字段名、类型、`dao` 与 `store` 标签；同名同型同标签视为已存在；同名不同型或不同标签报错并点名文件。把 `addTimerDaoFields` 改成调用这个原语，行为不变（它的现有测试必须照过） |
| 找实体的主 DAO | `add_timer.go` 的 `entityMainDao` | 多 DAO 实体按现有规则取主 DAO；没有 DAO 时提示先 `roost add dao <Entity> --entity <Entity>` |
| `roost.yaml` 的结构化合并 | `commitManifestSync`、`saveManifest` | 追加 `modules.<name>`、`services.<svc>.uses`、`sagas`、`access.player.sync_groups` 等列表项，去重；不做字符串标记插入 |

存储键：模块的两个顶层字段的 `store` 键用模块名加后缀（例如 `wallet`、`wallet_ledger`）；嵌套类型内部从 `1` 起用数字短键（tag 手册允许嵌套类型独立编号）。

### 11.5 ID 分配

- 协议号、错误码、组件号：调用现有 `NextID(root, m, kind, group)`。模板里写 `{{ID:protocol:WalletSpend}}`、`{{ID:errcode:wallet_short}}`，安装时替换并把“名字 → 号”记入 `roost.yaml` 的 `modules.<name>`。同一次安装要分配多个号时，注意 `NextID` 每次都重新扫描磁盘：要么每分配一个就先把文件写到暂存区再扫，要么在内存里维护“本次已占用”的集合。
- 协议组：模块的协议放进 `game` 组（缺省 10000～19999）。不新开组。
- **定时器类型（A3 已实施）**：模块声明 `timer_types`，按实体避开本包 int32 常量、Register 静态实参、`TimerSpec.Types` 与已安装记录；不能静态确定的登记拒绝分号。模块记录保存 `timer_entity` 与 `ids.timer`，不回收号。
  - 复用现有组件，缺失时复用 add timer 的 DAO / 组件改写原语，在模块安装回滚边界内补齐；不另起定时器驱动。
  - 模板 `{{TIMER_COMPONENT}}` 支持自定义组件名。模块为定时器组件提供 `register<模块组件>Timers`，安装器给业务登记函数追加一次调用；回调真正执行时才访问业务组件，避免构造顺序依赖。
  - 首个使用者为 [turn](../../modules/turn/README.md)，25 条场景。实体与定时器生成接线在同一暂存树生成；新增与已有登记的失败回滚均有实际工程验证。

### 11.6 运行期接线点

已实现（A1）：模块清单可声明 `runtime: {package: game/skillbook, start: Start}`，入口是 `func(*app.Registry) (func(context.Context) error, error)`。安装记录保存目标 service、requires 与 runtime；生成的 `modules_gen.go` 嵌入实际 Service，bootstrap 调用 `NewWithModules()`，不修改业务 Service 文件。启动顺序为框架 Mod → 模块依赖顺序 → 业务 Init；停机为业务 → 模块反向顺序 → 框架 Mod。部分启动失败也返回清理函数；停机错误保留依赖与回调，不把取消当成退出。

首个使用者为 skillbook，demo 已迁移，详见 [modules 的服务级启动与停机](../../modules/README.md#服务级启动与停机)。不引入框架运行时包、不要求业务补两行调用，也不通过 doctor 提示代替自动接线。生命周期只支持单次启动，不支持在业务 Init 之后再启动一组模块。

### 11.7 安装器与四个命令

- 新文件 `codegen/internal/roost/module.go`（命令实现）与 `module_install.go`（安装步骤）；`cli.go` 加顶层 `module` 分支；`help.go` 加 `module` 主题；`command_tree.go` 同步。
- 安装步骤复用 `demoScaffoldStep` 的形状（`add` / `write` / `run`），把它从 `demo.go` 提成两边共用的类型。一个模块的安装序列由清单推出，不手写：
  1. 校验：`needs` 满足（缺 Mod / feature 时给出 `roost add mod …` 的提示，不自动改）；`requires` 先装；`conflicts` 不在。
  2. 确保实体与主 DAO 存在（`-template game` 已有 Player / World；主 DAO 不存在时调用现有 `addEntityDAO`）。
  3. `add component <Component> --entity <Entity>`（已存在则复用，见下）。
  4. 分配 ID，替换模板里的占位。
  5. 追加主 DAO 字段（11.4）。
  6. 写入 `files`：目标已存在且内容与模板渲染结果相同 → 跳过；存在且不同 → 这个文件是业务改过的，报告“已安装、已被修改”，不覆盖；是 `Add` 刚生成的骨架 → 覆盖（与 `writeDemoFile` 的做法相同）。
  7. `add protocol` / `add errcode` / `add table` / `add endpoint` 等需要登记的项，按清单调用现有 `Add`。
  8. 合并 `roost.yaml`（`modules.<name>` 与需要的 `uses`）。
  9. `GenerateTransactional`。
- **事务性**：整个安装在同级临时副本里完成，全部成功后才提交到工程（`GenerateTransactional` 与 `project sync` 已是这个模式，沿用其“临时副本 + 所有权预检 + 回滚”）。失败不留半成品。
- **复用模式**：`addEntityComponent`、`addEntityDAO`、`addTimer` 现在遇到已存在就报错。给 `AddOptions` 加一个只供安装器使用的内部选项（不暴露成 CLI 参数），已存在且形状相符时返回“已存在”而不是错误。CLI 的现有行为不变。
- `plan` 与 `add` 共用同一条计算路径，`plan` 只是不提交；`--json` 输出的字段：模块、依赖顺序、将创建的文件、将追加的 DAO 字段、将分配的 ID、需要用户手工做的事（例如在 `service.go` 里加两行）。JSON 结构加一条金样测试，因为 skill 会读它。
- `verify`：对每个已装模块按清单的 `packages` 与 `run` 执行 `go test -mod=mod -race -count=1 -v`，核对 `--- PASS` 数等于 `pass`、`--- SKIP` 为 0；再跑 build、vet、`roost id check`、`roost generate --check`。输出逐模块一行结论。
- `list`：读嵌入的清单；`--json` 给 skill 用。

### 11.8 Robot 的多模块共存

现状：`scripts/testdata/robot-common/install.py` 把一个玩法的 `PlayRequest`（消息号写死）写进固定路径，一个工程只能装一个。

- 每个模块自带 `loadtest/<module>_steps.go`：用模块自己的协议，向一个工程级注册表登记场景节点。
- 生成器写 `loadtest/modules_gen.go`（生成器所有），汇总已装模块的登记函数；`cmd/robot/main.go` 归业务，只在缺失时生成，内容是“连接、鉴权、按名字跑已登记的场景”。
- 现有的 `loadtest/protocol/registry_gen.go` 由协议生成器产出（`generate.go` 里的 `-robot-protocol`），不用改：模块的协议进了 `protocol/def`，它自然包含。
- 第 2 期只要求货币模块带一条 Robot 步骤并在真实 TCP 上跑通（参照 `codegen/internal/roost/self_sync_test.go` 的 `TestSelfSyncGeneratedProjectOverRealTCP`，存储用替身）。

### 11.9 两个验证模块

**wallet（货币）**

- 状态：`Wallet WalletState`（`persist,sync`：余额）、`WalletLedger WalletLedger`（`persist`：幂等回执）。
- 组件 `Wallet`：`Credit(key, amount)`、`Debit(key, amount)`，溢出检查、余额不足返回 `wallet_short`；同一个 `key` 重复调用返回第一次的结果，金额不同则拒绝。回执账本有上限，满了拒绝新写、不淘汰旧回执（与现有夹具的账本语义一致，见 `scripts/testdata/tower-defense/match.go.txt`）。
- 能力：`Init` 里向 Registry 发布 `wallet` 能力（接口放在 `game/wallet`），供别的模块在自己的多实体 handler 里调用组件方法。
- 协议：查询余额、消费各一条；handler `//roost:nest rollback=undo durability=strict`。
- 场景（6 条，名字以 `TestWallet` 开头）：加钱、扣钱、余额不足零写入、重复 key 不重复生效、同 key 不同金额拒绝、handler 失败后回滚。

**chat（聊天）**

- 不带持久状态：包一层 `service/chat` 的客户端（发消息、拉历史），自己的 controller 域。它验证“模块依赖托管服务（`needs.uses: [chat]`）”与“没有 `state` 的模块”两条路径。
- 参照 demo 的 `game/chatroom`、`game/controllers/player/send_chat.go.tmpl`、`chat_history.go.tmpl`；本期是新写一个最小版本，不从 demo 里搬（demo 第 5 期才动）。
- 托管服务的 collaborators 仍是拒绝桩时，doctor 的那一项照旧 FAIL，这是现状，不在本期处理。

### 11.10 十个模块结构体的压力用例

在 `codegen/internal/roost` 加一条生成工程测试（可参照 `self_sync_test.go` 的搭法）：主 DAO 下挂 10 个模块结构体（5 个同步、5 个只落库），每个含标量、map、嵌套对象；跑一组写入。

- 断言：每次写入的同步补丁只含被改的那个结构体的被改子字段；持久变化按路径；handler 失败后同步与持久都回滚；重连拿到全量后与服务端一致。
- 记录（写进第 9 节，不作为门槛）：单次写入的补丁字节数、一次全量的字节数、变化记录预算是否触发整体替换。

### 11.11 文档

- [命令与标记参考](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)加“业务模块：`roost module`”一节；[项目生成器](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md)的 `roost.yaml` 一节加 `modules`。
- [guide/12-codegen](../framework/guide/12-codegen.md)加一段边界说明：生成器、模块安装器、业务文件三者各管什么。
- `modules/README.md`：怎么写一个模块、怎么本地验证。
- CHANGELOG `[Unreleased]` 记“新增”。AGENTS 本期不改。

### 11.12 第 2 期验收

新增脚本 `scripts/test-modules-generated.sh`（并加入 `scripts/pretag.sh` 的列表，根包 `TestGeneratedScriptsAreInPretag` 会检查）：

1. `project new -template game`，自定义 module 名（不是 `-practice` 固定名）。
2. `roost module plan wallet chat --json`，核对与金样一致。
3. `roost module add wallet chat`；再执行一次，`git status` 无变化。
4. `roost module verify`：wallet 6 条 PASS、0 SKIP；build / vet / `id check` / `generate --check` 通过。
5. `project doctor`：除已知的托管服务 collaborators 项外没有 FAIL，`modules:wiring` 通过。
6. 在一个已手工加过别的组件和 DAO 字段的工程上重复第 3～4 步（证明不是只在全新工程上能装）。

## 12. 第 3 期：三个会话型玩法改成配方

对象：回合 PvP、合作 Roguelike、异步社交经营。现状是 `scripts/test-<genre>-generated.sh` 各一行，调用 `scripts/testdata/genre-common/run.sh <genre> <首条命令>`；三者共用 `GameRoom` 实体、`game/rules` 包与 `TestScenario*` 前缀，一个工程只能装其中一个。

### 12.1 拆成的模块

| 模块 | 来源 | 内容 |
| --- | --- | --- |
| `wallet` | 第 2 期 | 取代公共夹具往 `PlayerDao` 加的 `Coins` |
| `sessionroom` | `genre-common` 的 `handlers`、`protocol`、`network`、`robot`、各测试 | `GameRoom` 实体（World 锁档）及其 DAO、开局 / 命令 / 领奖三个 handler、一条命令协议（号改为分配）、分发与 Robot 步骤。规则通过接口注入：`sessionroom` 发布“规则注册点”，规则模块在 `Init` 里登记 |
| `turnpvp` / `cooprogue` / `socialsim` | 各玩法的 `rules.go.txt`、`scenarios_test.go.txt`、Robot 补充文件 | 各自的规则包（包名各不相同，不再共用 `game/rules`）、场景测试；`requires: [sessionroom, wallet]` |
| 合作 Roguelike 的授权视图 | `coop-roguelike/view_*` | 并入 `cooprogue`，协议号改为分配 |

三个规则模块如果确实不能同装（例如共用同一个房间类型的唯一规则），在清单里写 `conflicts` 并在本节写明原因；能同装更好。

### 12.2 配方

`modules/recipes/turn-pvp.yaml` 等三份：基础模板（`game`）、模块列表、期望的场景总数。`roost project new … -recipe turn-pvp` = `-template game` + `module add` 列表。

### 12.3 脚本与门禁

- 三个 `scripts/test-<genre>-generated.sh` 保留文件名（pretag 守卫与手册都引用它们），内容改成“按配方新建 → `roost module verify` → 核对场景数”。
- 场景数不得减少：9 / 10 / 9（[SESSION-GENRES](../framework/SESSION-GENRES.md)）。测试改名（不再都叫 `TestScenario*`）时，同步改脚本的 `-run` 与计数断言，以及 `scripts/test-genres-generated.py` 的归类规则。
- 删除 `genre-common/run.sh` 里已被取代的部分；`genre-common` 目录在三类都迁完后删除，并检查所有 Markdown 引用。

### 12.4 第一条组合验收

新增到 `scripts/test-modules-generated.sh`：先按现有脚本生成 SLG 工程（`ROOST_SLG_KEEP=1 bash scripts/test-slg-generated.sh`，此时 SLG 还是旧夹具），在它上面 `roost module add sessionroom turnpvp`。

- 预期会遇到的问题：SLG 夹具已经往 `PlayerDao` 加了顶层字段 `Gold`，与 `wallet` 的状态不冲突（名字不同），两套货币并存——这是迁移期的已知状态，在第 4 期 SLG 迁移后消失；验收里如实写明。
- 通过条件：SLG 原有 7 条 + 回合 PvP 9 条全部 PASS、0 SKIP；build / vet / ID 检查 / 生成物漂移检查通过；Robot 两边的步骤都能跑。

### 12.5 文档

[SESSION-GENRES](../framework/SESSION-GENRES.md) 与 skill 的 [session-genres reference](../agent-skills/roost-genre/references/session-genres.md) 改成配方命令；CHANGELOG 记“变更”。

## 13. 第 4 期：抽原语包，迁移其余玩法

### 13.1 原语包

只抽在夹具里已有 3 处以上近似拷贝的。放进 core 的普通包（建议集中在 `gameplay/` 下一到两个包里，不一个原语一个包），带包测试；模块与夹具改为调用它们。

| 原语 | 现有拷贝（`scripts/testdata/` 下） |
| --- | --- |
| 幂等序号账本（序号连续、重复序号返回旧回执、改内容拒绝、上限） | `tower-defense/match.go.txt`、`survival-extraction/match.go.txt`、`merge-casual/operations.go.txt`、`idle-sim/production.go.txt`、`card-rpg/catalog.go.txt`、`auto-chess`；会话型变体在各 `rules.go.txt` |
| 终态领奖（座位已领标记 + 玩家加货币，同一笔 strict 事务） | `tower-defense`、`auto-chess`、`survival-extraction` 的 `handlers.go.txt`，`genre-common/handlers.go.txt` |
| 带座位的房间（座位、连接代际、票号接管） | `tower-defense`、`survival-extraction`、`auto-chess`、`party-room` 的 `state.go.txt` 与 `match.go.txt` |
| 规则目录（严格 JSON、哈希、打开与按值捕获） | `tower-defense`、`survival-extraction`、`auto-chess`、`idle-sim`、`merge-casual` 的 `rules.go.txt`，`card-rpg/catalog.go.txt`，`party-room/rules.go.txt` |
| 唤醒定时器（DAO 存一个唤醒节点、重排、回调核对节点号） | `tower-defense`、`idle-sim`、`merge-casual` 的 `handlers.go.txt` 与各 `timer_handlers.go.txt` |
| 崩溃恢复测试骨架 | 各玩法 `persistence/recovery_test.go` |

抽之前先把各拷贝并排比对，差异写进包文档：哪些是参数、哪些是玩法自己的。有一处行为不同就不要硬合并。

### 13.2 迁移顺序

每迁一个玩法：拆成模块 + 一份配方 → 脚本改为按配方 → 场景数与真实依赖模式（`ROOST_<GENRE>_REAL=1`）的用例数都不减 → 更新该玩法的接入手册与 skill reference。

建议顺序（先结构最像的）：塔防 → 自走棋 → 生存撤离（三者同构）→ 放置 → 合成 → 卡牌 → 派对房间（涉及匹配、会话、组同步、心跳）→ SLG（涉及 Remote 托管的联盟权威）→ MMORPG → MOBA（Lockstep、KCP）。后四个最复杂，各自单独评估，迁不动的保留旧脚本并在第 9 节写明。

### 13.3 收口

- 自定义 module 名在生成时生效：删除 `scripts/test-genres-generated.py` 里“事后全局替换 `example.com/<genre>-practice`”的做法，改为新建工程时直接传自定义名。
- `scripts/pretag.sh` 的玩法列表与 `test-genres-generated.py` 的 `GENRES` 改为从配方清单读取；根包 `TestGeneratedScriptsAreInPretag` 相应调整，但“每个玩法每次发版都跑”的保证不能弱化。
- 改写 [AGENTS“游戏接入 skill”](../../AGENTS.md#游戏接入-skill) 的口径（玩法来自模块与配方）与 [roost-genre skill](../agent-skills/roost-genre/SKILL.md)（见第 15 节）。
- 生成物如果用到了新增的 core 包，发版时按 `scripts/release.sh --min-core` 提高 `minimumVersions.Core`，并在 `manifest.go` 的注释里写原因。

## 14. 第 5 期：拆 demo

前提：第 2～4 期的机制已稳定。demo 仍通过 `-template game-demo` 生成，对使用者的入口不变；变的是内部从“一张步骤表写全部文件”逐步改成“基础部分 + 一组模块”。

### 14.1 每拆一个功能的固定步骤

1. 列出该功能的全部文件与它在共享文件里的引用（本文 1.3 与下表是起点，以当时的代码为准）。
2. 把状态从 `PlayerDao` 的顶层字段移进该模块的嵌套结构体（未上线，不迁移旧数据；`PlayerDao` 的 schema 版本与 `db/migrations` 示例相应调整）。
3. 把 `service.go` 里的字段与 `Init` / `Shutdown` 片段移进模块的 `Init` / `Shutdown`；把 `controller.go` 的硬性查找改成模块自己的 controller 域。
4. GM 命令、错误码、协议、压测步骤移进模块，ID 改为分配并登记。
5. `demoScaffoldSteps` 里对应的步骤删掉，改为 game-demo 配方里的一项；`TestDemoTemplateStepsAndShippedFilesAgree` 等测试相应调整。
6. 验证：`GOWORK=off go test ./codegen/internal/roost/ -run TestDemo`；按 [demo README“改这里的东西之后”](../../demo/README.md#改这里的东西之后) 生成、build、vet、test；`scripts/test-demo-generated.sh`（需要真实依赖，用法见脚本头）；有本机隔离环境时跑 `make dev-run` 与 `make dev-smoke`。

### 14.2 顺序与各自的共享点

| 顺序 | 功能 | 主要共享点（拆的时候要处理） |
| --- | --- | --- |
| 1 | 聊天 | `service.go` 的在线状态监听；`controller.go` 对 chat 的硬性查找；`enter_game` 里的登录公告 |
| 2 | 排行 | 副本结算里的提交调用（先改成排行模块订阅结算事件，或保留为副本模块的可选依赖） |
| 3 | 购买 | `PlayerDao.PurchaseClaims`；`settings` 里必填的 `platform.*` 键；`purchase` 功能开关 |
| 4 | 送礼 saga | `PlayerDao.SagaReceipts`（与公会 saga 共用，先抽成一个“saga 回执”小模块）；依赖背包 |
| 5 | 邮件领取与升级奖励 | `PlayerDao.MailClaims`；升级 effect 的消费者 |
| 6 | 副本 | `PlayerDao.DungeonClaims` / `DungeonClears`（读写方法现在挂在 Profile 组件上）；一条请求里串了会话、领奖、排行、活动 |
| 7 | 公会 / 联盟 | 跨两个服务；`PlayerDao.GuildID`；主清单里残留的公会错误码；`enter_game` 的公会刷新；`demo_alliance.go` 里按注释文本改配置的四个步骤 |
| 8 | 活动 | `settings` 里必填的 `activity.*` 键；World 定时器 |
| 9 | 场景、怪物、技能、战斗 | `scene.go`（约 1350 行，混着场景、分区、地形、建筑、组队）；`game/actor` 的进程级地面 |
| 10 | 分区与组队 | `PlayerDao` 的 `Cell*` 与 `Team*` 字段；`scene.go` 里几十处分区引用；`InstallNestHandlers` |
| 11 | 地形与建筑 | 已自成文件，但在 `service.go`、`scene.go`、`partition.go`、`gm.go` 里共约二十几处钩子；进程级的 `terrainmap.Active()` |

第 9～11 项互相缠绕最深，可能需要先把 `scene.go` 按职责拆文件再拆模块；做之前单独写一小节评估，交维护者确认是否继续。demo 拆到哪一步都可以停，没拆的部分继续作为整体模板。

## 15. 第 6 期：skill 与人类文档

- [roost-genre skill](../agent-skills/roost-genre/SKILL.md)：
  - “玩法选型”表改为配方表，命令改为 `roost project new … -recipe <名字>`。
  - 新增“按需求组合”流程：把需求拆成条目 → `roost module list --json` 对照 → `roost module plan … --json` 看依赖与冲突 → `roost module add` → 模块库没有的条目用 `roost add …` 手写 → `roost module verify` → 向使用者报告：用了哪些模块、哪些是手写的、哪些需求框架不支持。
  - 新增“在已有工程上继续补全”：先读 `roost.yaml` 的 `modules` 得知已装什么，再走同一流程。
  - 保留并前置“不适合 / 暂缓的类型”与公共限制；模块目录不在 skill 里另抄，一律从 CLI 读。
- 人类文档：
  - 模块目录页由清单生成（`go generate` 的一步，进漂移检查），列出每个模块做什么、依赖、状态字段、协议、场景数。
  - 每个模块一页说明放在 `modules/<name>/README.md`：做什么、不做什么、怎么改、与哪些模块协作。
  - [入门手册](../GETTING-STARTED.md)与 [codegen README](../../codegen/README.md)加“从配方开始”与“往工程里加模块”两条路径。
- 验收：一名没有本轮上下文的 agent 只凭 skill，在新工程上完成“回合 PvP + 聊天 + 一个手写的签到功能”，`roost module verify` 与全部检查通过；过程中 skill 的不准确之处回写到 skill。

## 16. 不要做的事

- 不把模块做成 `roost add` 的新种类；不让 `roost generate` 写业务文件。
- 不给模块建独立 DAO；不为“多个 DAO 都参与同步”改生成器或 Sync（第 7 节）。
- 不做卸载；不回收已分配的 ID 与存储键。
- 不在第 5 期之前改 demo 的业务逻辑；不为了让模块机制好做而改三大块的运行时。
- 不为了通过验收降低场景数、放宽断言或把 SKIP 当通过。
- 不新建“完整 demo 的第二份拷贝”；参考工程只有 game-demo 一个。

## 17. 第 2 期实施记录（2026-11-10）

基线 `6e03e925`（main），分支 `feat/business-modules`。codebase-memory 本轮连不上，全部按源码与 `rg` 核对。

### 17.1 三项核对的结果（第 11.1 节）

| 核对 | 结果 |
| --- | --- |
| 嵌套结构体的同步粒度 | 符合预期。`dao:"persist,sync"` 的结构体进同步字段表，`dao:"nosync"` 的不进；改子字段时同步补丁是子字段补丁（不是整体替换），持久补丁按路径。回归 `TestTenModuleStructsUnderOneDAO`、模块 wallet 的 `TestWalletOnlyTheVisibleStateIsSynced` |
| 嵌套类型内部能否单独声明不同步 | 不能按 `persist,nosync` 用：写在嵌套字段上生成器接受，但这个字段照样出现在同步补丁的 PB 里（在临时工程上实测）。`nopersist,nosync` 的嵌套字段另有既有用例（`codegen/internal/dao/testdata/remoteviews`），语义没有逐项核对。所以维持第 2.4 节的规则：同步与否按顶层字段决定，要拆就拆成两个结构体。这条登记在 KNOWN-LIMITS“尚未确认” |
| 顶层字段数上限 | 属实，且报错不清楚：65 个字段时生成成功、生成物编译失败（`constant overflows uint64`）。已改为解析阶段拒绝并说明（`TestDaoWithMoreThan64FieldsIsRefused`） |

核对中另发现一处生成器缺陷并已修：**嵌套类型的识别取决于文件名顺序**。嵌套类型定义在排在 DAO 文件之前的文件里时识别不到，生成时报 `unsupported sync type`（模块的 `db/def/checkin.go` 排在 `player.go` 之前，正好撞上）。现在全部文件解析完再认一遍，生成物里嵌套类型的排列不变（`TestNestedTypesAreFoundWhateverFileDefinesThem`，修前红）。

### 17.2 落地的做法与第 11 节的出入

| 第 11 节的计划 | 实际 | 原因 |
| --- | --- | --- |
| 11.2 模块目录 `modules/`，嵌入 CLI | 按计划。嵌入用通配（`*/module.yaml`、`*/files`），不逐个列目录；`TestModuleCatalogEmbedsEveryTemplateAndValidates` 核对磁盘与嵌入一致、模板里没有具体的 module 路径 | — |
| 11.3 清单与 `roost.yaml` 记录 | 按计划。`files` 不写在清单里，由 `files/` 目录读出；`roost.yaml` 的 `modules.<name>` 记摘要、号、状态字段、文件 | 少一处要对齐的清单 |
| 11.4 三个结构化改写原语 | 做了“往主 DAO 末尾追加字段”（`daoStructSource.appendFields`，`roost add timer` 改用同一个定位函数）。`roost.yaml` 的合并只有写 `modules` 记录一处，没有做通用合并 | 两个模块不需要改 `uses` / `sagas` 等列表；等有模块需要时再做 |
| 11.5 ID 分配 | 协议号、错误码、组件号按计划（一次计划内不相撞，已装记录里的号不再发）。定时器类型已按实体分配（A3） | turn 使用，支持已有自定义定时器与安装回滚 |
| 11.6 运行期接线点 | **已实现 A1** | `runtime` + `modules_gen.go` 服务适配层，首个使用者 skillbook；按依赖启动、逆序停机，见 §11.6 |
| 11.7 安装器 | 事务性用“先记下会动到的文件、失败时原样恢复”，不是整工程的临时副本；生成这一步仍是 `GenerateTransactional`（暂存区里生成）。组件不经 `roost add component`，直接接到实体上（组件文件由模板提供、组件号是分配的占位）。没有给 `AddOptions` 加复用选项：前置骨架只在缺失时才调用现有的 `roost add` | 模块写的文件集合是确定的，记录并恢复比复制整个工程简单；回滚有变异验证（去掉恢复后用例失败） |
| 11.8 Robot 的多模块共存 | **没有做** | 两个模块的验收只到 Nest 场景；Robot 步骤与真实 TCP 留给第 3 期（会话型玩法本来就带 Robot） |
| 11.9 两个验证模块 wallet、chat | wallet 按计划；**chat 换成 checkin**（每日签到，依赖 wallet） | chat 托管服务的 collaborators 在新工程里是拒绝桩，模块要覆盖一个已存在的业务文件才能用；checkin 更能验证依赖顺序、两个模块共用一个 DAO、同一笔事务里跨模块调用、业务时间 |
| 11.10 十个模块结构体 | 按计划。全量 3536 字节、增量 43 字节；持久补丁 4 条路径 67 字节（本机一次运行，只说明量级） | — |
| 11.11 文档 | 命令参考、项目生成器、guide/12、`modules/README.md`、CHANGELOG、KNOWN-LIMITS；另加[能力清单](../framework/CAPABILITIES.md)（维护者 2026-11-10：“是不是把 core 的能力都记录下”）与 skill 的“按需求组合”流程。AGENTS 加了模块的指引，玩法的口径仍在第 4 期改 | — |
| 11.12 验收 | `scripts/test-modules-generated.sh`（进 pretag）：全新工程自定义 module 名、依赖安装、重复安装无变化、wallet 8 + checkin 6、增量安装。“已手工加过别的组件和字段的工程上安装”“落点被占用时拒绝”“生成失败时回滚”在 Go 用例里（`TestModulesInstallOntoAProjectWithBusinessCodeAndRollBackOnFailure`）。`project doctor` 没有新增检查项 | 没有接线点，就没有 `modules:wiring` 可查 |

### 17.3 组合时暴露的一条规则

第一次把两个模块装在一起时，wallet 的一条用例失败：它断言“同步字段恰好是 `[Wallet]`”，而 checkin 也往同一个 DAO 加了同步字段。**模块的测试只能断言自己的事实。** 已改为断言“我的字段同步、我的账本不同步”，并写进 `modules/README.md` 的约定。这类问题只有组合验收能发现，所以第 3 期起每个新模块都要和已有模块一起装一遍。

### 17.4 验证

```sh
GOWORK=off go test -count=1 -timeout 30m ./codegen/...     # 模块、DAO 生成器与其余用例通过；见下面的一条环境性失败
GOWORK=off go test -count=1 .                               # 仓库级门禁通过
bash scripts/test-modules-generated.sh                      # modules: ok（wallet=8、checkin=6）
```

平台：macOS。模块场景用替身提交器，没有连真实 Mongo / NATS / Redis，也没有经过真实 TCP。

合入 v1.43.0 之后又跑了一遍 `GOWORK=off go test -count=1 -timeout 40m ./...`（全仓），没有失败。中间有一次 `./codegen/...` 的运行正好撞上 v1.43.0 刚发布：`codegen/internal/roost` 里 5 条会联网解析 `roost-core@latest` 的生成工程用例失败，报错是校验库还查不到新版本（`verifying module … v1.43.0 … 404 Not Found … temporarily unavailable`）；约半小时后重跑这 5 条全部通过。它们不经过本轮改动的代码。

### 17.5 只凭 skill 的实测（2026-11-10）

为了回答“做完之后能不能凭一段提示词用 core 定制服务逻辑”，让一个没有本轮上下文的 agent 只读 `docs/agent-skills/roost-genre/SKILL.md` 起步，按一段需求做工程：玩家金币、每日签到、每日任务（每天最多记 3 次进度，记满领 50 金币，一天一次，跨天清零），客户端要能看到三者。

结果：约 10 分钟做完，没有读 roost-core 的源码。装了 wallet 与 checkin，手写了每日任务（状态、组件、两条协议、两个 handler、端点、11 条场景），`roost module verify` 与手写场景全部通过，doctor 只剩四项托管服务 collaborators。它自己做了 4 个变异确认断言有效。

暴露并已修的问题：

| 问题 | 处理 |
| --- | --- |
| **模块缺陷**：wallet 的回执账本满了就拒绝、不淘汰；checkin 每天占一条，手写的每日任务照文档也每天占一条，第 65 天起（只装 checkin 是第 129 天）签到、领奖、消费全部被拒 | wallet 改成两个入口：`Add`（调用方自己判重，不留回执）与 `Apply`（带业务键，最近 128 次的去重窗口，最旧的淘汰）；checkin 改用 `Add`。修前红：`day 129: wallet_ledger_full` |
| 文档没写“客户端怎么看到”：模块安装不打开自身视图，`module verify` 也不报 | compose.md 新增第 4A 步（`add transport tcp`、`self_sync: true`、`project sync`），KNOWN-LIMITS 登记；PLAYER-ACCESS §7 补一句 `transports` 由哪条命令写入 |
| 日界两种说法：模块用 `毫秒 / 86400000`（固定 UTC），框架的约定是业务日 | checkin 改用 `clock.BusinessDay`；compose.md 写明日界与惰性清零的做法，以及跨天后客户端看到的是旧记录 |
| compose.md 的含糊处：`add protocol` 后的入口桩会被 `add endpoint` 替换；handler 返回带码错误即可；测试替身自己写一份；`clock.SetSource` 是进程级的；没有天然幂等键的写入怎么办；手写功能的号不在 `roost.yaml` 里；验收没含 `go test ./...`；这一档是否包含网络验收 | 逐条补进 compose.md |
| checkin 的测试借用 wallet 测试文件里的替身类型 | 改为自带一份 |
| skill 入口太长，按需求组合只占开头一句 | SKILL.md 开头加一条：这类任务直接读 compose.md |

这一轮的局限：compose.md 里手写功能的示例恰好就是“每日任务”，与实测的需求重合，对“手写一个文档里没出现过的功能”的检验偏弱。修完后换了一个不重合的需求再测一轮，见 17.6。

没有做的（实测报告指出、属于现状）：端点层（错误转成回包的 `Code`）没有模块用例覆盖；没有 Robot 与网络验收；真实客户端连上还差 `auth.go` 与 `config enable player-tcp`。

### 17.6 第二轮实测：不与示例重合的需求（2026-11-10）

另一个同样没有上下文的 agent，在 17.5 的修复之后只凭 skill 起步。需求：闯关小游戏，金币与体力（上限 20、每 6 分钟恢复 1 点），开局扣 5 点，花 30 金币补满（每天 2 次、已满不能买），结算赢了给 20 金币且同一局重复上报不重复给，新玩家送 100 金币和满体力，客户端看得到金币与体力。

结果：约 13 分钟做完，没有打开 `framework/`、`infra/`、`codegen/` 的源码。装了 wallet，手写了体力、对局、进入游戏三个功能共 19 条场景，`roost module verify`、手写场景、`go test ./...` 全部通过，自身视图接上，doctor 只剩四项 collaborators。它做了 15 条变异全部被杀，并指出自己交付物的十条上线风险（开局重发多扣体力、胜负由客户端说了算、客户端要按“下一点时刻”推算体力等）。17.5 修掉的问题这一轮没有再出现：它没有用按天递增的键去占钱包的去重窗口，日界用的是业务日。

这一轮暴露并已补进 compose.md 的缺口：

| 缺口 | 处理 |
| --- | --- |
| 没写 Player 是怎么被创建的：没有“进入游戏”端点时离线场景全绿，真实进程里所有请求答 `entity_not_found` | 新增第 4B 步：`add protocol` + `add endpoint --lifecycle Player`，并写明不加的后果 |
| “新玩家初始发放”没有写法 | 第 4B 步：发放写成不对客户端开放的 Nest handler，“发过没有”记在自己的状态里，每次进入都调；不按“是否新建”判断 |
| 一个模块都不装时第 4 步走不通（没有 `PlayerDao`、没有玩家接入） | 第 4 步开头给出两条前置命令 |
| 拨时间的例子指向只在装了 checkin 时才有的文件 | 把夹具代码贴进文档 |
| 随时间恢复的数值没有写法 | 新增一段：存“结算值 + 下一点时刻”，写入时结算，客户端推算 |
| `clock.BusinessDay` 的返回类型、多功能的 `-run`、空请求的 handler 形状、同实体组件互调、错误码文件布局、测试里不要引用被测常量、手写功能之间可以共用夹具、模块自带的客户端协议要不要保留 | 逐条补进 compose.md |
| skill 的 description 只讲按玩法出 demo | frontmatter 的说明与触发词加上“按需求定制” |

仍然成立的局限（两轮都指出）：入口里的手写串接（进入游戏之后调初始发放）没有离线测法；端点层、自身视图首帧、真实进程下的建档都没有跑；这一档不含登录鉴权、Robot 与网络验收。也就是说，现在能做到的是“凭一段需求得到一个业务逻辑写完、离线场景通过的工程”，还不是“可以直接上线的服务”。

## 18. 模块清单与后续顺序（2026-11-11）

当前待实施项以[后续需求](HANDOFF-2026-11-12-module-requirements.md)为准。维护者补充：联盟模块统一命名 `alliance`，与既有联盟服务一致；`activity` 是通用时间阶段框架，配置阶段、推进与恢复是核心，排行、结算和邮件奖励通过可选联动接入。以下历史批次的状态以该交接中的最新验收记录更新，不把早期“未做”当成当前事实。

后续是继续拆解：把 game-demo、13 类玩法夹具与两轮实测里反复手写的业务拆成模块。清单按“新工程最先需要什么”排。按决定 5，拆出来的模块只要求新生成的工程能直接用，不为以前生成的工程做兼容；game-demo 已改用 chat 与 enter 两个模块（生成时安装，见 `demo.go` 的 `demoModuleSteps`）；profile、bag、leaderboard、mailbox 是照 demo 的做法拆出来的独立版本，demo 自己的那几块还没有换成它们（决定 3 的那一步对这四个仍待做）。

| 批次 | 模块 | 做什么 | 依赖 / 需要安装器先有的能力 | 状态 |
| --- | --- | --- | --- | --- |
| 1 | wallet | 货币：余额、`Add`、带去重窗口的 `Apply`、消费协议 | — | 已有（9 条） |
| 1 | checkin | 每日签到，奖励经 wallet | wallet | 已有（8 条，含签到钩子 `OnCheckinClaimed`） |
| 1 | chat | 世界与私聊的发送、翻历史 | 托管 chat 服务；`replaces`（已做） | 已有（8 条）；game-demo 在用 |
| 2 | enter | 进入游戏端点（建档）与首次初始化：做过没有记在档里，每次进入都问；新玩家的初始发放写在业务文件 `enter_first.go` 里（没有做模块间的自动登记）。安装时打开玩家 TCP 接入与自身视图 | 清单的 `access`（已做） | 已有（6 条，含进入钩子的登记点）；game-demo 用它的协议、域、首次初始化与进入钩子 |
| 2 | profile | 名字、等级、经验与升级；只检查不执行的入口；给服务端读的名片；状态分“别人可见”与“只给本人”两个字段 | — | 已有（19 条）；game-demo 的档案在用 |
| 2 | bag | 道具背包：增减、上限、按道具号堆叠；自带道具表（道具是否存在、每种的单种上限） | — | 已有（17 条）；game-demo 的背包在用 |
| 2 | stamina | 体力：随时间恢复、消耗、用货币补满（每日次数） | wallet | 已有（25 条） |
| 2 | task | 任务系统（原名 quest，维护者 2026-11-11 改名：“task 是一个比较核心的系统，很多业务需求可以加到 task 中去”）。这一版：每日任务的进度、领奖、按业务日清零 | wallet | 已有（18 条）；签到推进经 task+checkin 联动。**扩成核心系统是下一步**，见本节末尾 |
| 2 | leaderboard | 排行：提交分数（可分周期）、一次请求查前 N 名与自己的名次；榜的规则是业务文件 | 托管 rank 服务 | 已有（20 条）；game-demo 的通关榜在用 |
| 2 | mailbox | 邮件：列表、领附件、发件入口 `SendTo`；附件种类是登记点，金币内置，道具经 mailbox + bag 联动 | 托管 mail 服务；wallet | 已有（32 条；联动 mailbox+bag 9 条、mailbox+enter 5 条）；game-demo 的邮件在用 |
| 3 | shop | 用货币买道具，每日 / 终身限购 | wallet、bag；配置表（`tables`） | 已有（17 条）（目录是业务文件，没有用配置表） |
| 3 | room | 带座位的房间实体：进出、连接代际、房主 | 自带 Room Entity、DAO、lifecycle，安装时分配实体号 | 已有基础模块（22 条）；匹配移交、回合与自动断线订阅不包含 |
| 3 | matchroom | 匹配成局到房间的移交 | 托管 match 服务；room | 未做 |
| 3 | turn | 回合推进：命令序号、超时（持久定时器） | room；定时器类型分配 | 已实施（25 条），边界见 turn README |
| 3 | gift | 玩家间送礼：扣 → 送的 saga，失败补偿；扣款步骤按 sid 确切路由（v1.44.0） | bag；saga Mod 与 `roost add saga` 作为安装前置（安装器现在只检查、不代办）；服务级接线点（步骤消费者要在服务 `Init` 里启动、带属主核对） | 未做（2026-11-11 合入 v1.44.0 后评估：game-demo 的赠礼 saga 删掉交接代码后只剩约 700 行，适合拆；卡在安装器没有服务级接线点） |
| 3 | transfer | 单个玩家换服（改绑 sid）：源服停写、等落库、account 改绑、目标服接管；给 saga 的“换服中”钩子提供状态 | account 服务的改绑接口与换服状态（框架内）；服务级接线点 | 未做（维护者 2026-11-11：“以模块方式，底层的放在框架内”；设计中） |
| 3 | scene | 场景：Scene 实体、进出场景、移动与视野、刷怪、心跳驱动与同步接线（现在只在 game-demo 里，约 2900 行参考实现；core 里只有视野 / AOI、同步视图、移动、时间域、地形这些零件） | 安装器要先支持模块自带实体与服务级接线点 | 未做。**维护者 2026-11-11 同意的顺序**：换服做完 → 补安装器这两个缺口（同时解开 room / matchroom / turn / gift）→ 把 demo 的场景拆成模块 → 再加功能，先后是：多场景管理（按需创建销毁、切换）、区域触发（可接 task 的事件）、附近聊天（chat + scene 联动）、分线、可交互对象 |
| 4 | 配方 | `roost project new -recipe <name>`：一组模块加参数（休闲、回合对战……） | 模块够用之后 | 未做 |

安装器已支持自带实体与 Init / Shutdown 接线点（2026-11-11）；定时器类型分配已完成；还缺、按需要补的能力：配置表与模块配置、更多的登记点（通用机制已有：清单的 `with` 联动文件，见 [modules/README](../../modules/README.md)；已有的登记点是 enter 的进入钩子，chat 与 mailbox 接在上面。还缺：mailbox 的附件种类让 bag 登记道具附件、profile 的升级与 enter 的首次发放让别的模块挂奖励——现在都是改业务文件）。`module verify` 把子测试数进通过数、`roost add errcode` 一码一个文件与“一个模块一个错误文件”不一致，是做第二批时记下的两个小毛刺。每补一项都要有一个真实用到它的模块一起做，不先造空机制。

task 要扩成核心系统的方向（未实施，按先后）：

1. **按事件推进**：别的功能只上报“发生了什么”（签到、消费、升级、通关、购买……），不认识任务号；任务表里写“哪个事件推进哪条任务”。入口 `TaskComp().Report(事件, 次数, nowUnixMilli)`，现有的 `Advance(任务号, …)` 保留。加一个业务需求 = 任务表加一行 + 在那个功能里上报一次事件。
2. **周期与种类**：每日之外加每周、永久（成就）、有前置的任务链（主线）；清零规则按种类走。
3. **奖励种类**：现在只有金币；改成（种类、号、数量），与 mailbox 的附件用同一种表达，装了 bag 就能发道具。
4. **任务表做成配置表**（照 bag 的道具表）。
5. **联动**：各模块在自己成功的那一刻上报事件——wallet 消费、profile 升级、shop 购买、stamina 消耗；它们要先留出登记点（现在只有 checkin 有）。

每个新模块的验收不变：全新工程上单独安装、与已有模块一起安装、重复安装无变化、`roost module verify` 的场景数；写进 `scripts/test-modules-generated.sh`。每批做完用一个只凭 skill 的 agent 按一段不重合的需求实测一遍。

## 19. 第三批与登记点的实施记录（2026-11-11）

维护者 2026-11-11：“按照你的建议实施”（先补模块间登记点并继续拆下一批；game-demo 换用 profile / bag / leaderboard / mailbox 排在其后）。中途因 5 小时额度用满按维护者要求暂停过一次，恢复后三个被停掉的 agent 从各自的记录续做完成。

**做了的**

- 安装器：联动文件（清单 `with`、`with/<另一个模块>/`），见 [modules/README](../../modules/README.md)“清单”一节。
- 四处登记点与四组联动：enter 的进入钩子（chat + enter、mailbox + enter）；checkin 的签到钩子（task + checkin）；mailbox 的附件种类（mailbox + bag）。
- 第三批模块：task（18）、stamina（25）、shop（17）。
- wallet 业务键的长度与字符限制（shop 的作者对照时发现，先红后绿）。
- game-demo 的进入流程改走进入钩子。

**与原计划的出入**

- 第 18 节原写 mailbox“领附件进货币 / 背包”依赖 bag：实际 mailbox 只 `requires` wallet，道具附件是联动，不装 bag 的工程不受影响。
- shop 原写依赖配置表：安装器还不支持模块带配置表，商品目录是 Go 写的业务文件。
- 登记点没有做成“安装器往别的模块的文件里追加一行”（三个模块的作者都提过这个想法）：那要改写归工程所有的业务文件。做成了“提供方留登记函数、接入方带一个自己的文件在 init 里登记”，安装器只负责在两个模块都在时把这个文件写进去。

**没做的（按建议的先后）**——其中第 1 项已在下一节完成，其余仍然成立：

1. ~~game-demo 换用 profile / bag / leaderboard / mailbox~~（已完成，见 §20）。
2. 还缺的登记点：wallet 的“消费成功”（要区分 `Apply` 命中去重回执与首次生效）、profile 的升级（现在是业务文件钩子，别的模块挂不上去）、enter 的首次发放（事务内）、leaderboard 的提交时机。每个都要有一个真实用它的模块一起做。
3. room 基础模块、自带实体与服务级接线点已完成；turn 与定时器类型分配已完成；matchroom / scene / gift 继续推进。
4. shop 的商品目录、task 的任务表改成配置表（照 bag）；规则导出给客户端（stamina 的上限与周期、当前业务日）。
5. 配方（`-recipe`）与 13 类玩法夹具的迁移。
6. 用一个只凭 skill 的 agent 按一段不重合的需求再实测一遍：第二、三批模块、联动机制与 demo 换用之后的模块还没有被这样测过。
7. 真实依赖下的验证：模块端点经 TCP 的整条链路、mailbox 经 NATS / Redis 的路径、game-demo 的 `make dev-run` 与压测机器人。全部模块验收都是离线场景。

## 20. game-demo 换用四个模块、合入 v1.44.0、task 改名（2026-11-11）

维护者 2026-11-11：“做 demo 换用”；过程中补了两条方向——“demo 的功能多的话，可以拆到模版中去”“模版可以复杂，能兼容简单就可以”（模块可以做全做复杂，但全新工程不做任何配置直接安装必须能用；已写进 [modules/README](../../modules/README.md) 的约定）。

**做了的**

| 块 | 模块怎么变的 | demo 还留着什么 |
| --- | --- | --- |
| 排行榜 → leaderboard（15 → 20 条） | 榜的身份与规则拆成业务文件 `leaderboard_board.go`（周期是函数）；`LeaderboardTop` 带自己的名次；`Submit` 不收幂等键，新增 `SubmitSeason` | 只覆盖 `leaderboard_board.go`（通关榜）；副本通关计数是副本玩法 |
| 背包 → bag（12 → 17 条） | 自带道具表（道具是否存在、每种的单种上限）；规则入口 `bagItemRule` 缺省读表 | 只覆盖道具表两个数据文件（多 `attack` / `hp` 两列）；充值订单账本；`AddItem` 演示入口 |
| 档案 → profile（16 → 19 条） | 状态拆成“别人可见”与“只给本人”两个同步字段；`CheckAddExp`；名片 `ProfileCard` | 只覆盖升级钩子 `profile_levelup.go`；公会归属与副本账本；两实体的 AddExp（记 World 累计） |
| 邮件 → mailbox（28 → 32 条，连带 wallet） | 发件入口 `SendTo`；mailbox+enter 的画像认 account 的 claims（3 → 5 条）；enter 的进入钩子带会话 claims | 不覆盖任何模块文件；升级奖励的消费者（要服务级接线）、GM / 活动 / 送礼三个发件方 |

- demo 现在从模块得到七块：进入游戏、聊天、排行榜、背包、档案、邮件、技能目录（加上依赖带进来的 wallet）。demo 的步骤约定：全部模块在同一步里安装（`enter, chat, leaderboard, bag, profile, mailbox, skillbook`），覆盖模块文件的 `write` 排在它之后，建在模块组件上的 handler 没有 `roost add handler` 骨架步骤（`TestDemoStepsLeaveModuleComponentsToTheModules` 钉住）。
- 合入 main 的 v1.44.0（saga 步骤按 sid 确切路由）；能力清单记入路由步骤；可拆的 gift 与 transfer 记入 §18。
- quest 改名 task，扩成核心系统的方向记在 §18 末尾。
- 单个玩家换服开工：方案见 [FEATURE-2026-11-11-player-rehome](FEATURE-2026-11-11-player-rehome.md)；框架侧两块（account 的换服接口、DataEngine 的交出实体）已合入，模块 `rehome` 与 demo 接线未做。

**没做的**：见 §19 的清单（第 1 项已完成），加上换服方案里的一期后半与二、三期。
