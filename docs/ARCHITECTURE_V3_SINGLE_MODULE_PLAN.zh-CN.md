# Roost 收敛第二步：三仓合一仓

状态（2026-09-20）：**P0、P1 完成，P2 未开始**。上一步"五仓合三仓"（roost-skill / roost-service 并入）于 2026-09-08 完成，
方案见 [ARCHITECTURE_V2_CONSOLIDATION_PLAN](ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md)，
方法论、门禁与机器这一轮**逐条沿用**。执行手册是个人 skill `roost-consolidate`。

目标：**框架只剩 roost-core 一个仓、一个 Go module、一个 tag**。roost-kit 与 roost-codegen
发最后一个版本后归档。

## 0. 已拍板的决定（2026-09-20，维护者）

| 编号 | 决定 | 理由 |
| --- | --- | --- |
| **D1 版本** | 模块路径不变（`github.com/tjbdwanghaibo/roost-core`），发一个破坏性 minor **core v1.16.0**；不走 `/v2` | 与上一轮一致。业务工程靠 `roost project upgrade --consolidate` 改写一次 import；旧 tag 仍可 pin，未升级的工程不受影响 |
| **D2 生成器落点** | `roost-core/codegen/`——一个顶层目录装下原 codegen 的 `internal/*` 与 `cmd/roost`（即 `core/codegen/cmd/roost`） | 维护者定的"一个仓一个顶层目录"。已核对：codegen 的直接依赖只有 `gopkg.in/yaml.v3`，core 已经有，**合仓不新增任何直接依赖** |
| **D3 兼容承诺** | 版本号照发（一个仓只有一个 tag，躲不掉），但**只有 core 包的改动进兼容承诺**；`demo/` 与 `codegen/` 的改动不构成框架行为变化，CHANGELOG 分节写明，README 写清楚 | 09-20 一天 codegen 发了 11 个版本，多数只动 demo。不分节的话版本号的信息量会归零 |
| **D4 kit 落点** | `roost-core/kit/`——装配层（mods、app 生命周期与运维）、`service/` 12 个服务、`integration` 原样整体搬入，**不打散进 core 的包树** | 同 D2。"要不要打散"是 ARCH-01/02/05 的话题，搬完再谈；混在一起两件都做不完 |

## 1. 目标布局

```
roost-core/                       一个 module：github.com/tjbdwanghaibo/roost-core
├── <core 包>                     entity dataengine nest room redis nats mongo etcd
│                                 saga remoteentity statesync entitysync spatial skill …
├── kit/                          装配层
│   ├── mods/  app/  ops/         capability 名表、生命周期、运维入口
│   ├── service/                  account chat directory global global/activity mail
│   │                             match platform rank session servicemetrics
│   └── integration/              服务集成测试
├── codegen/                      生成器
│   ├── internal/…                原 roost-codegen/internal/*
│   └── cmd/roost                 CLI，`go install …/roost-core/codegen/cmd/roost@latest`
└── demo/                         game-demo 模板，与 codegen 平级
```

**依赖规则**（P1 门禁前置）：core 的包**不得** import `core/kit`、`core/codegen`；
`kit` 可以 import core 的包；`codegen` 运行时不 import 前两者（模板里的路径是字符串）。
今天这条规则由模块边界免费保证，合仓后必须由 `dependency_boundary_test.go` 按**目录前缀**判定——
**这条测试要在任何代码搬动之前改好**，否则规则在合仓当天就失去执行力。

## 2. 这个布局带来的三个简化

1. **import 改写退化成前缀替换**：`roost-kit/X` → `roost-core/kit/X`、`roost-codegen/X` → `roost-core/codegen/X`。
   上一轮要逐包映射（`consolidation_imports.yaml` 的 `split` / `move` / `skill` / `service` 四段），
   这一轮两条前缀规则加少数例外即可。
2. **"只移动不优化"自动成立**：目录整体搬，包结构一行不动，`git subtree` 带历史。
3. **发版链塌缩成一次**：不再有"改 core → 发 core → 升 kit pin → 发 kit → 升 codegen pin → 发 codegen"，
   也不再有清单与 tag 漂移（U-0270）、生成器版本下限造假（U-0264）这两类跨仓账。

## 3. 阶段与门禁

| 阶段 | 内容 | 门禁 |
| --- | --- | --- |
| **P0 决定与冻结** ✅ | 本文 §0 定稿；三仓建 `consolidation-v3` 分支；main 冻结为维护线（只收 Bug 修复，cherry-pick 进分支） | 已达成：三仓分支已推送，八个工作流的 `push` 触发加上该分支；kit / codegen README 置顶冻结公告，core 文档首页指向本文 |
| **P1 骨架与护栏** ✅ | `dependency_boundary_test.go` 改成目录前缀规则并**先于搬迁**生效；CI 按包组拆 job；core 预加将要用到的依赖（已核对：无新增）；tag `v1.16.0-alpha.1` | 已达成：反向 import 探针确实变红；`TestCoreContractsDoNotLinkDrivers` 按层豁免 kit；CI 四分片由 `go list` 计算（本地核对 82 包 → 20/21/21/20 无重复全覆盖）；`v1.16.0-alpha.1` 已发，实测可 `go get` 并编译 |
| **P2 kit 搬入** | `git subtree` 把 roost-kit 带历史搬进 `core/kit/`；批量前缀改 import；kit 的测试随代码走 | core 全绿（含 `-tags integration`）；kit 仓在旧路径仍能编译（go.work 指向分支） |
| **P3 codegen 与 demo 搬入** | 同法搬进 `core/codegen/` 与 `core/demo/`；模板里的 import 字符串批量改成单模块路径；golden / testdata 重生成；改掉 `demo/embed.go` 里"codegen 故意不依赖它生成的运行时"那段（合仓后不成立） | 生成 planet 与 game-demo 两种工程：编译 + `dev compose` 真实启动 |
| **P4 升级器** | `consolidation_imports.yaml` 升 schema 2：加第二段边界（core v1.16.0）与两条前缀规则；loader 支持前缀段；`upgrade --consolidate` 与 `deps-update` 跨界自动改写 | `--check` 对 golden 工程零差异；对一个真实工程改写后编译通过；每条规则至少一个 golden 覆盖 |
| **P5 验收与发布** | `source-head-check.sh`；故障矩阵；性能对比（同机三轮，>5% 退化要归因）；文档与 TROUBLESHOOTING 路径更新；发 **core v1.16.0**；kit / codegen 发最终版、README 置顶"已并入 core"、仓库 archive | 三仓 CI 绿；`roost new` 出来的工程**只依赖 core**；旧 tag 仍可 pin |

## 3.1 P1 的两处发现（记下来，因为它们正是"护栏先行"的收益）

1. **`TestCoreContractsDoNotLinkDrivers` 会在 P2 当天炸**。它的注释早就写着"assembly happens in
   kit's Mods"，而 kit 搬进来之后这个 walker 会扫到每一个 Mod——每个 Mod 都会被一条**自己注释里
   已经豁免它们**的测试判红。现在按层豁免 kit。这条如果留到搬的那天才发现，很容易被误读成"搬错了"。
2. **CI 分片不能写包名清单**。手写清单是第二个要记住每个新包的地方，忘一次就静默漂移（C4，本仓
   U-0263 / U-0264 都是这个形状）。改成 `go list | awk 'NR % 4 == shard'`，无重复全覆盖由构造保证。

## 4. 与上一轮不同的地方

- 上一轮是**合包**（kit/nats 并进 core/nats，同名包合一），这一轮是**归档目录**（整仓进一个目录）。
  所以上一轮的合包撞名规则这次基本用不上，风险面小得多。
- 上一轮三个仓都在动，这一轮 core 只增不改、kit / codegen 只删不改。
- 上一轮之后仍有三条发版链，这一轮之后只有一条。

## 5. 风险

| 风险 | 缓解 |
| --- | --- |
| core 的测试时长再涨一截 | P1 就按包组拆 job；本地跑 `-run` 过滤，全量放 CI |
| 业务工程被迫改一次 import | 升级器 P4 先做好、golden 覆盖；旧 tag 永远可用，不升级不受影响 |
| 依赖规则失去模块边界的保护 | P1 的目录前缀 boundary 测试**先行**，且要有一条"故意反向 import 会红"的用例 |
| 搬迁期间发现缺陷手痒 | 硬规则：搬迁批次里不修 Bug，写进 `docs/bug/WANTED.md` 交给 review 线 |
| 一个 tag 之后版本号信息量下降 | D3：CHANGELOG 分节，只有 core 包的改动进兼容承诺 |

## 6. 维护线冻结（P0 起生效）

三仓 `main` 从 P0 起只收 Bug 修复（U 单元），每笔修复 cherry-pick 进 `consolidation-v3`。
新功能、重构、文档大改一律进分支。review 线照常跑，登记的 RR 按等级处理：
P1 修在 main 并 cherry-pick，P2 及以下攒着，搬完再修。
