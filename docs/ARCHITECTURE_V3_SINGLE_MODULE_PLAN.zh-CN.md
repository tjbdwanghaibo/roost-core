# Roost 收敛第二步：三仓合一仓

状态（2026-09-20）：**P0–P4 完成，只剩 P5（验收与发布）**。上一步"五仓合三仓"（roost-skill / roost-service 并入）于 2026-09-08 完成，
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
| **P2 kit 搬入** ✅ | `git subtree` 把 roost-kit 带历史搬进 `core/kit/`；批量前缀改 import；kit 的测试随代码走 | 已达成：93 个 Go 文件前缀改写，go.mod 一行没动（kit 的四个直接依赖 core 全有）；build / vet / vet -tags integration / glsvet / test / test -race 全绿；kit 的 service-redis job 搬进 core 的 ci.yml |
| **P3 codegen 与 demo 搬入** ✅ | 同法搬进 `core/codegen/` 与 `core/demo/`；模板里的 import 字符串批量改成单模块路径；golden / testdata 重生成；改掉 `demo/embed.go` 里"codegen 故意不依赖它生成的运行时"那段（合仓后不成立） | 已达成：生成 game-demo 工程 → go.work 指向源码 → **编译通过、生成工程自己的测试全绿**。`dev compose` 真实启动留到 P5 与故障矩阵一起做 |
| **P4 升级器** ✅（CI 全绿） | `consolidation_imports.yaml` 升 schema 2：加第二段边界（core v1.16.0）与两条前缀规则；loader 支持前缀段；`upgrade --consolidate` 与 `deps-update` 跨界自动改写 | 已达成：每条前缀规则一条用例；顺序测试有变异验证（前缀提前跑就红）；用已发布的 codegen v1.15.31 生成老布局工程 → `--consolidate` → go.work 指向 checkout → build / vet 通过；`--dry-run` 与二次运行都证明幂等。framework-compat / upgrade-compat 已接回自动触发 |
| **P5 验收与发布** | `source-head-check.sh`；故障矩阵；性能对比（同机三轮，>5% 退化要归因）；文档与 TROUBLESHOOTING 路径更新；发 **core v1.16.0**；kit / codegen 发最终版、README 置顶"已并入 core"、仓库 archive | 三仓 CI 绿；`roost new` 出来的工程**只依赖 core**；旧 tag 仍可 pin |

## 3.1 P1 的两处发现（记下来，因为它们正是"护栏先行"的收益）

1. **`TestCoreContractsDoNotLinkDrivers` 会在 P2 当天炸**。它的注释早就写着"assembly happens in
   kit's Mods"，而 kit 搬进来之后这个 walker 会扫到每一个 Mod——每个 Mod 都会被一条**自己注释里
   已经豁免它们**的测试判红。现在按层豁免 kit。这条如果留到搬的那天才发现，很容易被误读成"搬错了"。
2. **CI 分片不能写包名清单**。手写清单是第二个要记住每个新包的地方，忘一次就静默漂移（C4，本仓
   U-0263 / U-0264 都是这个形状）。改成 `go list | awk 'NR % 4 == shard'`，无重复全覆盖由构造保证。

## 3.2 P2 / P3 的教训：盲改前缀会踩到"路径即数据"

三处，全部由测试当场挡下，逐条记在这里因为它们会在任何一次同类搬迁里重演：

1. **上一轮 5→3 的迁移映射表**（`consolidation_imports.yaml`）与它的固定装置里，旧路径**是数据本身**。
   把它们一起改掉，等于废掉老工程的升级路径——`roost upgrade --consolidate` 会把
   `roost-core/kit/X` 映射到 `roost-core/X`，两个方向都错。已完整还原。
2. **断言里的路径**：core 的 `TestForbiddenCoreImport` 里那条旧模块路径是断言本身（合仓后它是
   "某个文件漏改 import"的信号）；而 `framework_services_test.go` 里的期望值写的是不带域名的
   `roost-kit/service/...`，前缀规则反而没盖到，要手工跟上。
3. **前缀替换会造出死代码**：`roost-core/kit/` 是 `roost-core/` 的子集，glsvet 里那条判断从此
   永不触发。删掉而不是留着。

还有两处真实的行为改动，不是路径问题：生成的 go.mod 与 `roost project deps` 都只写一个 require，
因为 `roost-core/kit` 是**包路径不是模块路径**，向 `go get` 要它等于要一个不存在的模块。

## 3.4 P4 的第二个结论：边界迁移需要"只改写、不解析依赖"

`project new` / `project upgrade` / `project sync` 的最后一步是解析框架依赖。边界迁移有这么一段窗口：
**改写出来的 import 已经正确，而没有任何 proxy 能解析它们**——于是命令整体退出 1，尽管改写本身成功。
在 workflow 里补 go.work 没用，那一步在命令之后。

所以三条命令都接受 `--skip-deps`。这不是为 CI 开的后门：任何一次跨发布边界的迁移都会遇到同一段窗口。
两个 compat 门禁据此改成"生成 / 改写（`--skip-deps`）→ go.work 指向 checkout → 编译"。

代价是跳过了 `go mod tidy`，而 tidy 顺手做的一件事必须补上：etcd 的旧依赖会带进 pre-split 的
`google.golang.org/genproto`，与 core 用的 `googleapis/{api,rpc}` 提供同一个包，任何构建都会
"ambiguous import"。显式 `go get google.golang.org/genproto@latest` 补这一件，它不需要未发布的版本。

## 3.3 P4 的一个结论：合仓之后"用旧框架编译新生成器的产物"不再成立

`framework-compat` 原来有三格依赖集：minimum（钉生成器下限）、released（钉最新发布）、
source-head（指向源码）。合仓之后前两格**结构上不可能通过**：生成器产出的 import
（`roost-core/kit/...`）只存在于和它同一个版本的 core 里，而 minimum / released 装的是更老的发布。

这不是配置问题，是"生成器与框架同版本发布"这个决定的直接后果，也是它的代价之一：
**再也不能用一个旧框架去验证新生成器**。能验证的只剩两条，本轮都已接回自动触发：

- `framework-compat` 的 source-head：当前生成器 + 当前 checkout；
- `upgrade-compat`：**已发布**的老生成器造出的老工程，被当前升级器改写后能否编译。

minimum / released 两格在 P5 发布 v1.16.0 之后恢复，届时"最低支持版本"的含义变成
"这个生成器所属的那个 core 版本"。

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
