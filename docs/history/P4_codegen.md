# P4：codegen 适配收敛布局与升级器

批次：P4（收敛方案 §4）
状态：代码完成（codegen `consolidation` 分支 `881c191`，全部单测通过）；`framework-compat` 过渡期只有 source-head lane 可绿（见 §4）
前置：P2、P3；core `v1.14.0-alpha.4`、kit `v1.13.0-alpha.1` 预发布 tag

## 1. 模板与版本（P4-a）

- 生成工程的 go.mod 只 require core 与 kit；`frameworkdeps` 保留 `roost-core/skill` 与 `roost-kit/service/servicemetrics`。
- 模板路径：replication 的 `nettransport` / `room` → core；saga 定义的 `kitsaga.*` → `saga.*`（core）；实体生命周期的持久化别名 `kitdataengine` → `engine`（`core/dataengine/engine`）；`roost add skill` 的 import → `roost-core/skill`；servicerpc 模板与 golden → `roost-core/servicerpc`；托管服务 `frameworkServiceModule` → `roost-kit/service`。Mod 目录（catalog.go）不变——Mod 仍在 kit。
- 版本下限：core v1.14.0、kit v1.13.0、codegen v1.15.0；`VersionSpec.Skill/Service` 保留为可读的废弃字段（`omitempty`），校验不再看它们，`Marshal` 不再写出。
- CLI：`project new` 的 `-roost-skill-version` / `-roost-service-version` 删除；`project upgrade -skill/-service` 保留旗标但给出明确错误并指向 `--consolidate`；Makefile 模板的 `project-upgrade` 目标去掉 `-skill latest`。
- 发布清单 schema 2：`framework: {core, kit}`；`framework verify` 只下载两个模块；schema 1 与列出 skill/service 的清单被拒绝。`ci/framework-release.yaml` → codegen v1.15.0 / core v1.14.0 / kit v1.13.0。release 工作流去掉 skill/service 输出。
- 文档模板与 help 文案：roost-skill → 技能系统（roost-core/skill），roost-service → roost-kit/service；ROOST_YAML 指南去掉 versions.skill 行。

## 2. 升级器（P4-b）：`roost project upgrade --consolidate`

`internal/roost/consolidate.go`，映射表 `internal/roost/migration/consolidation_imports.yaml` 以 `go:embed` 进二进制，是搬迁批次、升级器与账本"新位置"共用的唯一事实来源。

规则：
- 整包搬迁（move / skill / service）：改路径；新包名与文件原用标识符不同（`dataengine` → `engine`、`servicemods` → `mods`）时加别名保住原标识符。
- 拆分包（split）：按**符号**决定——`kit_keeps` 里的（Mod 胶水）留 kit import，其余改 core；一个文件两者都用时保留 kit import、为 core 加第二个 import（别名 `core<包名>`，与文件内已有标识符冲突则加数字）。
- `renames`：同步改符号（`syncstream.HealthOptions` → `PublisherHealthOptions` 等）；`to: core` 表示同名。
- 指向已删除模块（roost-skill / roost-service）但不在映射表的 import → 报错并列出，不留到 `go build`。
- go.mod：删 roost-skill / roost-service 的 require，core / kit 低于边界时抬到边界（预发布与伪版本按发布前缀比较）；`roost.yaml`：清 versions.skill / service。
- `--dry-run` 只报告不写；幂等（第二次运行无改动）。
- `roost project deps`（含 `project new` 后的解析）检测到 go.mod 仍 require 旧模块时**自动先改写**再解析；`project upgrade` 不带 `--consolidate` 遇到旧工程直接报错指路。

测试：混合文件（skill / 拆分 redis 两用 / dataengine 默认名 / syncstream 改名 / servicemods）、已在新布局的文件不动、dry-run 零写入、未映射旧路径报错、映射表边界 == 生成器下限。

## 3. 本地验证脚本

`scripts/source-head-check.sh [minimal|full] [core-dir] [kit-dir]`：本地复现 `framework-compat` 的 source-head lane——用本仓构建 roost → `project new`（bootstrap 解析 pin 到 alpha）→ 临时 go.work 挂 core / kit 工作树 → build / vet / test / glsvet → 断言 go.mod 无 replace。这是收敛方案 M-01 要的"source-head 通路本地可复现"。

## 4. CI 状态与过渡期说明

- codegen `ci` / `security` 绿。
- `framework-compat`：`minimum` lane pin 的 v1.14.0 / v1.13.0 尚未发布、`released` lane 解析到的 latest（core v1.13.0 / kit v1.12.6）是旧布局——**这两条 lane 在 P6 发版前必红**；`source-head` lane 过渡期 pin 到 alpha 做 bootstrap 解析，随后由 go.work 替换成源码，应绿（结果见下一次记录）。
- `upgrade-compat`：升级 v1.11 / v1.12 旧工程后 `go get core@latest` 拿到旧布局 → tidy 失败，同样等 P6。升级器本身在单测里覆盖。
- 版本策略现接受下限的预发布 pin（`v1.14.0-alpha.4` 视为 v1.14.0 的布局）。

## 5. 未验证项 / 风险

- `upgrade --consolidate` 对真实业务工程的端到端（改写后 `go build`）：等 P5 用 codegen 自带 golden 工程 + 一个旧布局示例跑。
- 生成的 `_impl` 文件名、`engine` 包别名策略对大型工程的可读性。
- 模板里 `roost-kit/checkpoint` 的历史引用（映射表 notes）尚未核实。

## 6. 下一步唯一动作

P5：总验收——source-head 本地脚本、故障矩阵重跑、性能对比、三仓文档路径更新、账本"新位置"列。
