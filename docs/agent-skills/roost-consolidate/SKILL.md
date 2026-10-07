---
name: roost-consolidate
description: 把 roost-kit 与 roost-codegen 并入 roost-core，最终只剩一个仓、一个模块、一个 tag。分阶段搬迁与门禁，沿用 2026-09-08 五仓合三仓那一轮的方法论与机器。触发词：roost 合仓、三仓合一、把 kit 和 codegen 并进 core、$roost-consolidate。
---

> **历史 skill**：三仓合一已于 2026-09-26 完成（见 `docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md`），本文只作方法论参考，不要再按其中的三仓流程工作。

# Roost consolidate：三仓合一仓

维护者定的方向：**之后框架只有 roost-core 一个仓**。kit（装配层 + 12 个服务）与 codegen（生成器 + 模板 + demo）
并入 core，kit / codegen 发最后一个版本后归档。

工作目录 `/Users/whb/roost`，中文提交信息，末尾 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。
相关 skill：`roost-review`（只审不改）、`roost-bugfix`（修 RR）。相关记忆：`roost-local-env`、`release-no-ci-wait`、
`use-codebase-memory-mcp`。

## 0. 为什么（写下来，免得中途动摇）

跨仓版本是**每天**在付学费的地方，今天一天就有四笔：

| 事故 | 形态 |
| --- | --- |
| U-0263 | kit 新键空间没登记，本地漏跑 `-tags integration`，CI 才红 |
| U-0264 | codegen 声明的框架版本下限是假的，生成物在下限版本上编译不过 |
| U-0270 | 发布清单的版本号与 tag 漂移，受保护发布闸连红十次 |
| 日常 | 改 core → 发 core → 升 kit pin → 发 kit → 升 codegen pin → 发 codegen，中间还要等 goproxy 追上 |

合仓消掉的正是"同一件事写在多个仓里、靠版本号对齐"这一类。**它不消掉设计问题**——包与包的边界照旧存在，
只是不再用 git 仓库来表达。

## 1. 硬规则

- **只移动不优化**。搬迁批次里不修 Bug、不改公开 API、不重命名（除非合包撞名，见 §5）。
  搬的过程中看到缺陷 → 写进 `roost-core/docs/bug/WANTED.md`，交给 `roost-review` 分流，**不要顺手改**。
- **一批一个 M 编号**（重构，不占 U 单元），记录进 `docs/bugfix/` 的 M 表。修 Bug 才用 U。
- **分支 + 冻结**：三仓各建 `consolidation-v3` 分支；`main` 只收 Bug 修复，cherry-pick 进分支。
  上一轮（五仓合三仓）验证过：半迁移状态不能放在 main 上交接。
- **先改规则再搬**：依赖边界测试（`roost-core/dependency_boundary_test.go`）要先改成新规则，再动代码。
- **生成工程必须真编译真启动**：改模板之后重新 `roost project new` 跑全套，不能只跑 codegen 仓自己的测试。
- 搬迁期间 `roost-review` / `roost-bugfix` 照常跑，RR 修在 main 维护线，再 cherry-pick。

## 2. 已核对的事实（2026-09-20，动手前复核一次）

- **合仓不新增任何直接依赖**：codegen 的直接依赖只有 `gopkg.in/yaml.v3`；kit 的四个（go-redis、viper、
  mongo-driver、core）里 core 已经有前三个。这是"生成器进 core 会污染业务工程依赖树"这条反对意见的答案。
- **机器都是现成的**，别重写：
  - `roost-codegen/internal/roost/migration/consolidation_imports.yaml`——包路径映射的唯一事实来源，
    带 `boundary` 字段（跨过这个版本必须改写 import）。这一轮在它上面加第二段映射。
  - `roost project upgrade --consolidate`——AST 级 import 改写 + go.mod require + tidy，支持 `--check`。
  - `framework-compat` / `upgrade-compat` / `framework-release` 三个工作流，`scripts/source-head-check.sh`，
    `scripts/pretag.sh`。
  - `roost-core/dependency_boundary_test.go`。
- **上一轮的完整方案与批次记录**在 `roost-core/docs/ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md`
  与 `docs/history/P2-*.md` / `P3_kit.md` / `P4_codegen.md`。方法论逐条沿用，不要另起一套。
- 规模参考：core ≈67k LOC，kit ≈55k（14k 装配 + 41k service），codegen 生成器 + 模板另计。
  **core 的测试时长会再涨一截，CI 必须按包组拆 job**，上一轮已经拆过一次。

## 3. 目标布局（维护者 2026-09-20 定）

**一个仓库一个顶层目录**，不把 kit / codegen 的包打散进 core 现有的包树：

```
roost-core/
├── <core 包>     entity dataengine nest room redis nats mongo skill …
├── kit/          装配层：mods、app 生命周期与运维、service/ 12 个服务、integration
├── codegen/      生成器：原 internal/* 与 cmd/roost
└── demo/         game-demo 模板，与 codegen 平级
```

这个布局本身就回答了 D2（生成器一个目录）与 D4（kit 原样一个目录，不并进 `core/app`），
并定了 demo 的位置。它比"按包打散"好三件事：

1. **import 改写退化成前缀替换**：`roost-kit/X` → `roost-core/kit/X`、`roost-codegen/X` → `roost-core/codegen/X`。
   现有 `consolidation_imports.yaml` 的逐包映射仍然可用，但这一轮只需要两条前缀规则 + 少数例外。
2. **只移动不优化**这条硬规则自动成立：目录整体搬，包结构一行不动。
3. **依赖规则从模块边界降级为目录规则，但仍然可机器检查**：core 的包不得 import `core/kit`、`core/codegen`。
   `dependency_boundary_test.go` 要在**搬之前**改成按目录前缀判定（P1 门禁），否则这条规则在合仓当天就失去执行力。

另外两个决定同日拍板：

- **D1 版本**：模块路径不变，发一个破坏性 minor **core v1.16.0**，不走 `/v2`。业务工程靠升级器改一次 import。
- **D3 兼容承诺**：版本号照发（一个仓只有一个 tag），但**只有 core 包的改动进兼容承诺**；
  `demo/` 与 `codegen/` 的改动不构成框架行为变化，CHANGELOG 分节、README 写清楚。

落点细节：`cmd/roost` 放 `core/codegen/cmd/roost`，`go install …/roost-core/codegen/cmd/roost@latest` 可用。

四个决定与完整阶段表的权威版本在 `roost-core/docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md`，
本 skill 只是执行手册；两者冲突以那份文档为准。

## 4. 阶段与门禁

沿用"清单 → 行为测试先行 → 只移动不优化 → 更新消费方 → 门禁 → 记录"。

| 阶段 | 内容 | 门禁 |
| --- | --- | --- |
| **P0 决定与冻结** | D1～D4 拍板并写进方案文档；三仓建 `consolidation-v3` 分支；main 冻结公告（三仓 README / docs 首页） | 分支存在且 CI 对其生效；三仓入口都指向方案文档。**映射表升 schema 2 归 P4**——它要改 loader（新增前缀规则段）并配 golden，放在 P0 会是一笔没有门禁的改动 |
| **P1 core 骨架** | 边界测试改规则：core 不再禁 `roost-kit` / `roost-codegen` 路径，改为禁业务工程（`cube-*`）与旧模块路径；CI 按包组拆 job；tag `v1.16.0-alpha.1` | build/vet/test 全绿；boundary 绿；alpha 可 `go get` |
| **P2 kit 下沉** | `git subtree` 带历史搬 `service/` 12 包 → `core/service/`，再搬 `mods/` `app` 运维入口；每批修 import、包测试随代码走 | 每批：core 全绿；kit 在旧路径仍能编译（go.work 指向分支）；`-tags integration` 的 kit 套件在新路径重跑绿 |
| **P3 codegen 下沉** | 生成器 → `core/internal/codegen` + `core/cmd/roost`；模板与 demo 整体搬；**模板里的 import 字符串批量改成单模块路径**；golden / testdata 全部重生成 | codegen 仓清空前：生成 planet + game-demo 两种工程，编译 + `dev compose` 真实启动 |
| **P4 升级器** | `upgrade --consolidate` 加第二段映射（`roost-kit/*` → `roost-core/*`；codegen 作为工具不再被业务工程 import）；`deps-update` 跨边界自动改写 | `--check` 对 golden 工程零差异；对一个真实工程改写后编译通过 |
| **P5 验收与发布** | `source-head-check.sh`；故障矩阵；性能对比（同机三轮，>5% 退化要归因）；文档与 TROUBLESHOOTING 路径更新；发 core v1.16.0；kit / codegen 发最终版、README 置顶"已并入 core"、仓库 archive | 三仓 CI 绿；`roost new` 出来的工程**只依赖 core**；旧 tag 仍可 pin |

## 5. 这一轮特有的坑

- **模板里的路径是字符串**，编译器不管。改完必须重生成工程才知道对不对——`roost-codegen` 自己的
  `go test ./...` 全绿说明不了任何事（U-0264 就是这样漏的）。
- **`minimumVersions` 这个概念要重新定义**：合仓后生成器与框架同版本，"生成物要求的最低框架版本"
  要么删掉，要么改成"生成物要求的 core 版本"并保留 U-0264 的那条实测门禁。
- **发布清单的版本号**：合仓后只有一个 tag，但 `pretag.sh` 里 U-0270 加的那条"清单版本 == 要发的版本"
  检查仍然要留着（清单还在，只是少了两行）。
- **合包撞名**：契约名不动；实现名撞了就让它保持未导出，或让构造函数返回契约接口。每处改名进批次记录。
  上一轮的实例：`core/nats.Permanent` 留在契约包、`kit/syncstream.HealthOptions` → `PublisherHealthOptions`（T-45）。
- **goproxy 滞后**：发版当天 `go get @latest` 会拿不到新 tag，用
  `GOWORK=off GOPROXY=direct GOPRIVATE='github.com/tjbdwanghaibo/*' go get …@vX.Y.Z`。合仓后这件事只会遇到一次。
- **别在搬迁批次里回答"这个包该不该存在"**。ARCH-01/02/05 那些职责归属问题是另一条线，
  搬完再谈；把两件事混在一起，两件都做不完。按目标布局，kit 是整体搬进 `core/kit/` 的，
  ARCH-01/02/05 说的"下沉 core"在物理上已经完成，剩下的是"要不要打散进 core 的包树"——那是搬完之后的话题。
- **`demo/embed.go` 的注释会变成假话**。它现在写"`.tmpl` 后缀让这些文件不参与本模块构建：
  它们 import roost-core，而 codegen 故意不依赖它生成的那个运行时"。合仓后后半句不成立，
  而 `.tmpl` 仍然必要（模板里有 `{{MODULE}}` 占位符，本来就不是合法 Go）。搬的时候改掉这段。

## 6. 汇报格式

按阶段：这一批搬了什么（M 编号）、门禁跑了哪几条、撞名与改名清单、生成工程验证结果、下一批是什么。
最后一句说明分支状态与 main 是否还有未 cherry-pick 的修复。
