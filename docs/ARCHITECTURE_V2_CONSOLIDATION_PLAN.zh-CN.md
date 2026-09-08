# Roost 收敛方案：五仓合三仓，实现下沉 Core

状态（2026-09-08 晚）：P0～P4 完成，P5 进行中；记录见 `docs/history/P2-*.md`、`P3_kit.md`、`P4_codegen.md`。三仓 `consolidation` 分支：core `8689c7f`+、kit `b8e25c2`+、codegen `52690fe`+；预发布 core `v1.14.0-alpha.4`、kit `v1.13.0-alpha.1`。维护者决定：**不用 /v2 模块路径，直接使用最终路径**；**全部由本机完成**。文中"v2"仅指"收敛后的形态"，不是模块路径。关联：[统一实施方案](CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)（方法论沿用）、[09-08 复核](history/PLAN_REVIEW_2026-09-08.md)（事实修正）、[账本](history/ledger.md)、[交接](history/HANDOFF_2026-09-07.md)。

## 0. 决定

已由维护者拍板的方向：

- **Core 是真核心**：契约 + 基础实现 + 技能 / 战斗（roost-skill 并入）。
- **Kit 是接入层**：配置解析、依赖装配（Mod）、应用生命周期、运维入口 + 通用游戏服务（roost-service 并入）。
- **Codegen 是辅助工具**：生成器、脚手架、升级器。
- 之后框架只有三个仓：roost-core、roost-kit、roost-codegen；roost-skill、roost-service 归档。

本方案再做三项配套决定（有理由，可以推翻，但要在 P0 之前定）：

| 决定 | 选择 | 理由 |
| --- | --- | --- |
| D1 版本 | **模块路径不变**（`github.com/tjbdwanghaibo/roost-core`、`roost-kit`），包直接放最终位置；以**同日协调发版**的方式发破坏性 minor：core v1.14.0、kit v1.13.0、codegen v1.15.0（codegen 先发，因为业务工程要靠它的 `roost upgrade` 改 import） | 维护者决定。代价：新旧不能并存，业务工程在升级框架版本的同时必须跑一次 import 改写；旧 tag 仍可 pin，所以没升级的工程不受影响。`versions.core: latest` 的工程在 `deps-update` 时会跨过边界——升级器要在 deps-update 里检测到跨界并自动改写（P4） |
| D2 合包方式 | 15 个同名包（actionflow ai configdata dataengine etcd gateway lock lockstep mongo nats nest redis robot saga syncstream）**契约与实现合进同一个包**；不加 `impl` 子包 | 已验证：把任一 kit 同名包合入 core 同名包都**不会产生 import 环**（§3.2 表）。同一包里"接口 + 默认实现"是 Go 的常态；子包只会让每个消费者多记一个路径 |
| D3 迁移形式 | 三仓各建 `consolidation` 分支一次性完成骨架与全部搬迁，只发预发布 tag（`v1.14.0-alpha.N` 这种在 v1 路径上合法）供 `framework-compat` 试用；main 冻结只修 Bug；全部门禁通过后合并、同日发正式版 | 统一方案 §4.2 的原则"不把半迁移状态交接为已完成"在 main 上做不到；分支 + 预发布 tag 才能让 `framework-compat` 真实验证 |

## 1. 目标形态

```
roost-core (v1.14+)                roost-kit (v1.13+)               roost-codegen (v1.15+)
├── 契约 + 实现（同名包合并）        ├── mods/        capability 名表（并入 servicemods）
│   actionflow ai bus cache        ├── app 生命周期、健康、运维（ops manager）
│   configdata dataengine entity   ├── <能力>/      只剩 *_mod.go + 项目配置→Core 配置的转换
│   entitysync etcd gateway lock   │   redis nats mongo etcd nest dataengine saga remoteentity room …
│   lockstep mongo(+mongotest)     ├── service/     ← roost-service 的 12 个包
│   nats nest nestwal remoteentity │   account chat directory global global/activity mail match
│   redis robot room saga          │   platform rank session servicemetrics
│   security spatial statslog      └── integration/ 服务集成测试
│   syncbus syncstream versionstore
│   servicerpc nettransport …
├── skill/  ← roost-skill          codegen：模板指向新路径；`roost upgrade --consolidate` 改写 import；
│   skill combat combatcomponent           清单 schema 2（core / kit / codegen 三项）
│   skillcompose skillsync
└── 第三方驱动：mongo-driver、go-redis、nats.go、etcd client、quic-go、kcp-go、x/time
```

**依赖规则**（用可执行测试钉住，见 §5）：core 不 import kit、codegen；kit import core；codegen 运行时不 import 任一（模板里的路径是字符串）。业务工程 import core 用能力、import kit 装配。

规模（当前 LOC，不含 examples）：core 67k + kit 实现约 40k + skill 47k ≈ **154k**；kit 约 14k 装配 + service 41k ≈ **55k**。Core 的测试时长会翻倍，CI 要按包组拆 job（§7）。

## 2. 包映射表

这张表是整个迁移的**唯一事实来源**：搬迁按它做，`roost upgrade --consolidate` 的 import 改写按它做，账本的"新位置"按它查。P0 落成 codegen 里的机器可读文件 `internal/roost/migration/consolidation_imports.yaml`，本节是它的草稿。

### 2.1 roost-kit → roost-core（实现下沉）

| 原 kit 包 | 去向 | 说明 |
| --- | --- | --- |
| actionflow ai configdata dataengine etcd gateway lock lockstep mongo nats nest redis robot saga syncstream | `core/<同名>` | 合入同名契约包。`*_mod.go` **不搬**，留 kit。**B-26 修正**：mongo / nats / redis / etcd 的驱动实现最终落在 `core/<x>/driver`（package `driver`），契约包只保留接口、错误、选项，避免只引契约的二进制链接驱动；dataengine 实现落 `core/dataengine/engine`（避免与 nest / nestwal 成环） |
| nestwal | `core/nestwal` | dataengine、saga 的前置 |
| remoteentity | `core/remoteentity` | M-02 试点对象；`remote_entity_mod.go` 留 kit |
| room lockstep nettransport spatial | `core/room` `…/lockstep` `…/nettransport` `…/spatial` | room / lockstep 依赖 nettransport，同批 |
| versionstore servicerpc | `core/versionstore` `core/servicerpc` | service 的直接依赖（54 + 9 处），先于 service 并入 kit |
| statslog | `core/statslog` | 实现依赖 nest / entity / worker；`statslog` 的 Mod 若有则留 kit |
| mongo/mongotest | `core/mongo/mongotest` | 严格测试替身；Core 已依赖 mongo-driver。这一步顺带关闭"Core 测试反向依赖 Kit"的唯一缺口 |
| mods | **留 kit** `kit/mods` | capability 名表；并入 roost-service 的 servicemods 常量（`service.*` 前缀不变） |
| manager ops | **留 kit** | 应用生命周期 / 运维入口，本来就是接入层 |
| 每个能力包的 `*_mod.go`、项目 YAML → Core 配置的转换 | **留 kit** `kit/<能力>` | 搬完后 kit/redis 只剩 `redis_mod.go`（110 行）这类薄装配 |

### 2.2 roost-skill → roost-core

| 原 | 去向 |
| --- | --- |
| `roost-skill/skill` | `core/skill` |
| `roost-skill/combat` `combatcomponent` `skillcompose` `skillsync` | `core/skill/combat` `…/combatcomponent` `…/skillcompose` `…/skillsync` |
| `examples/`（嵌套模块） | `core/examples/skill/…`（并入 Core 已有的 examples 嵌套模块） |
| `integration/sync-e2e`（嵌套模块） | `core/integration/skill-sync-e2e`，改用工作树 core（原意图"工作树 skill 对已发布 core/kit"在同仓后不再成立，直接删 replace） |
| skill 唯一的 kit 依赖 `kit/syncstream`（2 处测试） | 改 `core/syncstream`（合包后即同仓） |

### 2.3 roost-service → roost-kit

| 原 | 去向 |
| --- | --- |
| `roost-service/{account chat directory global global/activity mail match platform rank session}` | `kit/service/<同名>` |
| `servicemetrics` | `kit/service/servicemetrics` |
| `servicemods` | 常量并入 `kit/mods`（`service.*` 名字不变），包删除 |
| `integration/`、`examples/split` | `kit/service/integration`、`kit/examples/service-split` |
| 服务对 `kit/versionstore` `kit/servicerpc` `kit/redis` 的 import | 改为 `core/…` |

### 2.4 codegen

- 模板里的 8 个 kit 包路径、`roost-skill/skill`、`roost-service/servicemetrics` 全部改新路径。
- `framework-release.yaml` schema 2：`framework: {core, kit}` + `codegen`；skill / service 两项删除。
- CLI：`-roost-skill-version` / `-roost-service-version` / `deps-update -service` 等旗标删除，给出明确错误"core v1.14 起技能在 core、服务在 kit"。
- 新增 `roost upgrade --consolidate`：按映射表改写业务工程 import 与 go.mod；`deps-update` 检测到 core 跨过 v1.14.0 边界时自动执行同一改写（详见 §4 P4）。

## 3. 前置事实（已核对）

### 3.1 依赖浅、无环

- kit 内部：几乎所有包只依赖 `mods`；`dataengine → nestwal`、`saga → nats, nestwal`、`room / lockstep / robot → nettransport`；测试依赖 `mongo/mongotest`。
- skill 只依赖 core（syncstream 15 处、nest 5、entity 2、dataengine 2、syncbus 1）和 mongo-driver；kit 依赖仅 2 处测试。
- service 依赖 core + kit（versionstore 54、mods 34、servicerpc 9、redis 5）。

### 3.2 15 个同名包合并无环

对每个 kit 同名包 X，取它 import 的 core 包集合 C，检查 C 中任一包的传递依赖是否含 core/X——**全部为否**。因此合包不需要重排依赖，也不需要 impl 子包。（脚本见 §5，迁移前后各跑一次。）

### 3.3 Core 将新增的第三方依赖

nats.go、go-redis v9、etcd client/api v3、quic-go、kcp-go（x/time 已加）。pin 在 v1.13.x 及以下的消费者不受影响；升到 v1.14.0 起的消费者会拉进这些驱动。

## 4. 分阶段与门禁

方法论沿用统一方案 §4.2 / §7（清单 → 行为测试先行 → 只移动不优化 → 更新消费方 → 门禁 → 记录）。批次编号沿用 M 系列，但按合仓重排：

| 阶段 | 内容 | 门禁（全部通过才进下一阶段） | 估时 |
| --- | --- | --- | --- |
| **P0 冻结与分支** | 三仓建 `consolidation` 分支；main 冻结为维护线（只收 U 单元 Bug 修复，cherry-pick 到分支）；§2 映射表定稿并落成 `consolidation_imports.yaml`；B-25 采样器跳过 `*_gen.go`；三份机器基线合成一份 | 映射表落文件；CI 对 `consolidation` 分支生效 | 1 天 |
| **P1 core 骨架** | `TestCoreDependencyBoundary` 改为新规则（禁 kit / codegen / cube-*，**不再禁 skill / service**——它们即将成为 Core 的一部分）；CI 按包组拆 job；Core go.mod 预先加入将要下沉的驱动依赖；tag `v1.14.0-alpha.1` | build / vet / test 全绿；boundary 测试绿；alpha tag 可被 `go get` | 半天 |
| **P2 实现下沉**（= 原 M-02～M-08，顺序按依赖） | 每批：`git subtree` 把 kit 对应目录带历史搬入 staging → 移到目标包 → 删 `*_mod.go`（留 kit）→ 修 import → 该包 U 测试随代码走 → 跑包测试 + boundary + 依赖图脚本。顺序：① nestwal ② nats / redis / mongo(+mongotest) / etcd 客户端 ③ remoteentity ④ dataengine ⑤ saga ⑥ nettransport → room / lockstep / spatial ⑦ actionflow / ai ⑧ versionstore / servicerpc / gateway / robot / statslog ⑨ skill 五包（subtree 整仓） | 每批：core 全绿 + kit 在**旧路径**仍全绿（kit 尚未切换，验证的是 Core 独立可构建；本地用 go.work 指向 core 分支）；批次记录按模板；③ 之后按统一方案 M-02 的新增场景补 U 单元 | 8～10 天 |
| **P3 kit** | 依赖 core alpha；删除已下沉实现，只留 `*_mod.go` + 配置转换 + manager / ops / mods；`git subtree` 并入 roost-service 到 `service/`；servicemods → mods；服务 import 改 core；kit 自己的 boundary 测试（禁 import codegen、禁 cube-*、禁旧 roost-skill / roost-service 路径）；tag `v1.13.0-alpha.1` | kit 全绿含 `-tags integration` vet；service 12 包测试全绿；kit `scripts/integration` 故障矩阵五切片在新路径重跑绿 | 3 天 |
| **P4 codegen** | 模板改新路径；清单 schema 2；删 skill / service 旗标；**`roost upgrade --consolidate`**：读 `consolidation_imports.yaml`，改写业务工程 import（AST 级精确替换）+ go.mod require 升到边界版本 + tidy，支持 `--check`；`deps-update` 跨过 core v1.14.0 边界时自动调用同一改写；`framework-compat` 工作流指向 `consolidation` 分支 / alpha；golden 测试更新；`framework-release` 工作流适配 schema 2 | codegen 全绿；`framework-compat`（source-head 与 alpha 两组）绿：生成 planet → 编译 → dev compose 真实启动；对一个现有示例工程跑 `upgrade --consolidate` 后编译通过 | 3 天 |
| **P5 总验收**（= 原 M-09） | 本地 `source-head-check.sh`；故障矩阵；性能对比（同机同配置：请求延迟 / 吞吐 / 分配 / goroutine，迁移前 main 与分支各跑三轮）；gap map 三仓 nightly 按新路径出报告；文档：README / INTERNALS / DEPLOYMENT / codegen 文档 / TROUBLESHOOTING 路径更新（账本正文不重写，加映射表链接） | 全部证据按统一方案 §9 模板归档；性能无未解释的退化（>5% 需归因） | 3 天 |
| **P6 发布与归档** | `consolidation` 合 main；**同日按序**发 codegen v1.15.0 → core v1.14.0 → kit v1.13.0（codegen 先发，业务工程才有升级器可用）；roost-skill / roost-service 打最终 tag、README 置顶"已并入 core / kit"、仓库设为 archived；main 维护线公告：只修 Bug 至发版后 8 周 | 三仓 tag CI 绿；`framework-compat` 绿；`roost new` 出来的工程只依赖 core + kit | 1 天 |

合计约 **20～22 个工作日**，两台机器并行可压到 **3 周**：A 机做 P1–P2（Core），B 机在映射表定稿后并行做 P3 前半（kit 骨架 + service 并入，暂指向本地 core 分支的 go.work）和 P4 前半（升级器 + 模板），P2 ⑨ 完成后再合流跑 P5。账本整合者固定为 A 机。

## 5. 护栏（可执行，进 CI）

1. **依赖边界测试**（三份）：core 禁 import `roost-kit` `roost-codegen` `cube-*` 及旧 `roost-skill` / `roost-service` 路径；kit 禁 `roost-codegen`、`cube-*`、旧 `roost-skill` / `roost-service`；codegen 运行时禁 import 三者任一（模板字符串除外）。现有 `dependency_boundary_test.go` 改规则即可。
2. **依赖图脚本** `scripts/depgraph.sh`：`go list -json ./...` 输出仓内 import 图与外部消费统计（本方案 §3 的两张表就是它的输出形态），每批前后各跑一次存进批次记录。
3. **合包冲突检查**：P2 每批合并前 `go vet` 编译即是检查；预计冲突点是同名类型（如 core/nats 的 `IClient` 与 kit/nats 的 `Client`）——规则：契约名不动，实现名有冲突时加前缀（`redisClient` → 保持未导出，或 `NewClient` 返回契约接口）。每处改名写进批次记录。
4. **`framework-compat`**：现有工作流已做"生成工程 + 临时 go.work 挂源码 + 编译 + compose 真实启动"，P4 只改指向；P5 前把它脚本化成 `roost-codegen/scripts/source-head-check.sh` 供本地跑。
5. **升级器自测**：`roost upgrade --consolidate --check` 对 codegen 自带的 golden 工程输出零差异；对旧路径示例工程改写后编译通过；映射表里每一行至少被一个 golden 用例覆盖。

## 6. 兼容之前未完成的改动

| 在途项 | 处置 |
| --- | --- |
| 统一方案（另一 Agent，09-08） | 方法论全部沿用；§3 架构边界按本方案 §1 改写（Core 含驱动与技能；Kit 含服务）；M-02～M-09 编号保留，内容映射到本方案 P2 ①～⑨ 与 P5 |
| M-00 / M-00a / 09-08 提交复核 | 合并成一份可移植基线（去 D:/ 路径、cube-* 目录名、Windows 工具链故障）；作为 P0 的"迁移前基线"归档 |
| `TestCoreDependencyBoundary`（core `4757b10`） | 保留，P1 改规则（见 §5.1）；kit / codegen 各加一份 |
| Skill `fe23185`（另一机器未推送，基于 09-02 过期基线） | **不推送**。其"身份对齐"在 origin/main 上已不存在问题；`TestFrameworkModuleIdentity` 的意图由三份 boundary 测试覆盖；"移除嵌套 replace"在并入 core 后自然发生。那台机器先 `git pull` 到 `375c10c` |
| TOOL-01（codegen `GOWORK=off`） | 非问题：生成工程必须可发布。source-head 验证走 `framework-compat` / `source-head-check.sh` |
| mongotest 归属 | P2 ② 迁入 `core/mongo/mongotest`，Core 测试反向依赖 Kit 的唯一缺口随之关闭 |
| U 单元与账本 | U 编号继续；main 上只做 Bug 修复单元并 cherry-pick 到分支；P2 各批把该包既有 `*_promises_test.go` 随代码搬走并在新路径跑绿——**搬迁不算新审计**，矩阵格子不改；账本第 3 节加一列"新位置"，内容从映射表生成，不重写历史行 |
| 开放后台项 | B-14：P2 ⑤ 迁 saga 时顺手把摘要函数改为返回错误（本轮允许 API 调整）；B-18 entitysync、B-19 redis：作为 P2 ②/⑦ 批次的行为保护 U 单元先补再搬；B-25：采样器跳过 `*_gen.go`（P0 前半小时做掉），生成物验证归 `framework-compat` |
| 87 个未审格、nightly 高位包 | 不阻塞迁移；按交接 §4 在空档做，位置按映射表 |
| 故障矩阵（kit `scripts/integration`） | 脚本随 kit 留在 kit；被测包路径改 core；P3 门禁重跑五切片；新增切片（Mongo 事务级、etcd 断连、JetStream 重投）放 P5 之后 |
| TROUBLESHOOTING T-01…T-43 | 编号继续；涉及包路径的行加"新路径"括注，不重写 |
| 已生成的业务工程 | 升 core ≥ v1.14.0 时必须跑 `roost upgrade --consolidate`（`deps-update` 跨界自动执行）；pin 在旧版本的工程不受影响，旧 tag 永久可用；main 维护线只修 Bug 至发版后 8 周 |
| codegen v1.14.0 的 cfggen 导出分组、core v1.13.0 的 RateLimiter | 在 `consolidation` 分支上原样保留（分支从当前 main 拉出） |

## 7. 风险与缓解

| 风险 | 缓解 |
| --- | --- |
| 历史丢失 | 用 `git subtree add --prefix` 或 `git filter-repo` 搬目录，保留提交历史；不用复制文件 |
| Core CI 时长翻倍 | CI 按包组拆 job（契约 / 存储 / 网络 / 技能 / 其他）；integration tag 单独 job |
| 合包时同名标识符冲突 | §5.3 规则；每批记录改名清单；改名只发生在实现侧 |
| 双线维护窗口过长 | main 只修 Bug、发版后 8 周硬窗口；P2 单批不超过 2 天，超期拆批 |
| 业务工程迁移失败 | 升级器 `--check` 先看差异；映射表逐行有 golden 用例；旧 tag 一直可 pin。**新增风险**：`versions.core: latest` 的工程在不知情时跨界——对策是 `deps-update` 跨界自动改写 + 发版公告 |
| 另一台机器的在途改动 | 本机全做；那台机器的 `fe23185` 不推送、其文档已在 main；之后它若参与只做 U 轨道并先 `git pull` |
| Core 依赖变重让"只想用契约"的消费者不满 | 这是 D1 的已知代价；对策是文档明确"core = 框架运行时"，轻依赖需求走 v1 线或后续按需拆 `core/x/driver` 子模块（不在本轮） |

## 8. 已确认的决定与 P0 起手

- D1：不用 /v2，模块路径不变，同日协调发破坏性 minor（codegen 先发）。
- 分工：全部本机；另一台机器的 `fe23185` 不推送。
- main 维护窗口：发版后 8 周只修 Bug（未另行指定，按方案默认）。

P0 起手顺序：三仓建 `consolidation` 分支 → 映射表落 `consolidation_imports.yaml` → B-25 采样器跳过 `*_gen.go` → 三份机器基线合成一份 → 进 P1。
