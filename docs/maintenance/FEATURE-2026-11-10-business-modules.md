# 功能设计：业务模块与配方（2026-11-10）

**状态：方案已定、未实施（维护者 2026-11-10 确认下表四项决定）。** 本文只描述将要做什么；现行能力仍是 [roost-genre skill](../agent-skills/roost-genre/SKILL.md) 加 `scripts/testdata/<genre>/` 夹具与 `-template game-demo`，以 [AGENTS](../../AGENTS.md#游戏接入-skill) 和 [KNOWN-LIMITS“游戏接入验收”](KNOWN-LIMITS.md#游戏接入验收) 为准。实施落地后，现行契约写进对应 guide / impl，本文只保留盘点、决定与实施记录。

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
模块      wallet / bag / room / chat / guild / dungeon / terrain …
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
summary: 玩家货币：余额、加减、幂等回执
requires: []                        # 依赖的其他模块，安装时先装
conflicts: []                       # 不能同装的模块
needs: {mods: [nest], features: [protocol, entity, dao, errcode], uses: []}
entity: Player                      # 状态挂在哪个实体上；自带实体的模块写在 entities 里
state:                              # 追加到该实体主 DAO 末尾的字段
  - {field: Wallet,       type: WalletState,  dao: "persist,sync"}
  - {field: WalletLedger, type: WalletLedger, dao: "persist"}
component: Wallet                   # 挂在 entity 上的组件
controller: wallet                  # 自己的 controller 域，不共用别的模块的
protocols: [WalletBalance, WalletSpend]          # 名字；号由 roost.yaml 分配
errcodes: [wallet_short, wallet_overflow]
tables: []
timers: []                          # 需要的定时器类型名；类型号安装时分配
provides: [wallet]                  # 发布到 Registry 的能力名
files:                              # 模板 -> 工程内路径；归业务，只写一次
  - db/def/wallet.go
  - game/wallet/wallet.go
  - game/entities/player/wallet_component.go
  - game/handler/wallet.go
  - game/handler/wallet_test.go
  - game/controllers/wallet/wallet_spend.go
  - loadtest/wallet_steps.go
verify: {packages: [./game/handler], run: '^TestWallet', pass: 6}
```

模块源码在仓库里的位置、是否像 demo 一样嵌入 CLI，在第 2 期按“少分包”定，倾向沿用 `demo/embed.go` 的嵌入方式。

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
- **同步与否按顶层字段决定**：`dao:"persist"` 只落库，`dao:"persist,sync"` 落库并下发。视图白名单（本人 / 附近 / 远处）也按顶层字段（`framework/entity/sync_view.go`）。所以既有客户端可见状态、又有服务端账本的模块，拆成两个结构体。嵌套类型内部的字段只声明存储键、没有单独的同步声明（推断：依据 demo 的 `Equipment` 写法与 `codegen/internal/dao/parse.go` 对嵌套类型的解析，未逐分支验证）。
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
| 1 | 本文已写（设计与第 10～16 节的实施方案），四项决定已确认（2026-11-10）。清单格式的字段名在第 2 期实现时定稿 |
| 2～6 | 未开始 |

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

目标：在一个 `-template game` 的新工程上，用 `roost module add wallet chat` 装两个模块，重复执行无变化，工程通过全部检查。这一期不迁移任何现有玩法，也不动 demo。

工作项按顺序做，前面的是后面的前提。

### 11.1 先做三项核对（不写产品代码）

用一个临时生成工程验证本文两处“推断”和一处未知，结果写进第 9 节。任何一项与本文不符，先改本文第 2.4 节再继续。

| 核对 | 做法 | 本文的预期 |
| --- | --- | --- |
| 嵌套结构体的同步粒度 | 主 DAO 加 `A AState` 标 `persist,sync`、`B BLedger` 标 `persist`；改 `A` 的一个子字段、改 `B` 的一个子字段，看生成的 `MarshalSyncChanges` 与默认自身视图打包的输出 | 只有 `A` 的那个子字段进同步补丁；`B` 的变化不下发；两者都按路径进持久变化 |
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
- **定时器类型**：现状是每个实体一个定时器组件，类型常量写在业务文件 `<component>_handlers.go` 的 `const` 块里、从 1 开始（`add_timer.go` 的 `renderTimerHandlers`）。建议：
  - `TimerSpec` 加 `Types map[string]int32`（名字 → 类型号），安装器在这里分配并记录；
  - 分配前扫描该实体包里已声明的 `int32` 定时器类型常量，避开已用的号；扫描不到确定结果时拒绝并说明；
  - 模块的回调写在自己的文件里（`game/entities/<entity>/<module>_timers.go`），提供一个方法 `register<Module>Timers(handlers *entitytimer.Handlers)`；安装器往业务文件的 `register<Comp>Handlers` 函数体末尾追加一行调用（按语法树追加，已存在则不加）。
  - 实体还没有定时器组件时，安装器先走现有 `addTimer`。
- 第 2 期的两个模块都不需要定时器；定时器这一项可以只做到“原语 + 测试”，第 3 期第一次真用。

### 11.6 运行期接线点

- **建议**：不新增框架包。生成器为每个装了模块的业务服务写 `internal/service/<svc>/modules_gen.go`（生成器所有，`project sync` 重写），内容是：

  ```go
  // 由 roost.yaml 的 modules 生成，不要修改。
  type businessModule interface {
      Name() string
      Init(*app.Registry) error
      Shutdown(context.Context) error
  }
  func installedModules() []businessModule { return []businessModule{wallet.New(), chat.New()} }
  func initModules(r *app.Registry) error            // 按依赖顺序 Init，失败时逆序 Shutdown 已成功的
  func shutdownModules(ctx context.Context) error     // 逆序
  ```

  模块在自己的包里实现 `New()`；要发布能力就在 `Init` 里 `registry.Register(...)`，要用别的模块的能力就 `app.Lookup`。可选能力（例如 GM 命令）用可选接口，由生成的代码做类型断言后汇总。
- 业务的 `service.go` 归业务、生成器不改写。做法与 saga 步骤相同：
  - `-template game` 新建工程时，`renderGameTemplateService`（`framework_services.go`）直接写入 `initModules` / `shutdownModules` 两处调用；没装模块时 `modules_gen.go` 里是空列表。
  - 已有工程：`module add` 的输出里给出要加的两行；`project doctor` 新增 `modules:wiring` 项，服务包里没有调用时 WARN（参照 `saga_effect_doctor.go` 的 `saga-steps` 检查）。
- 顺序：`requires` 决定 Init 顺序；Shutdown 逆序。App 对 `Service.Shutdown` 的要求不变（容忍部分初始化）。

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
