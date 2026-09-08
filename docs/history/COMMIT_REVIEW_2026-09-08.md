# 2026-09-08：五仓最近提交与实施状态复核

> **已合并**：可移植的事实已并入 [M-00_BASELINE.md](M-00_BASELINE.md)（合并版）；本页保留原文，其中的机器路径、目录名与 Windows 工具链故障不是仓库状态。ENV-01 系另一机器 Skill 检出过期所致，`fe23185` 作废。

关联：[统一实施方案](../CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)、[M-00a](M-00a_SOURCE_HEAD_ALIGNMENT.md)、[缺陷账本](ledger.md)。

本次读取本地五仓最近各 6 次提交、变更文件及相关源码，核对 M-00a 文档、外部 workspace 和已有日志。未 pull、未查询远端 CI、未推送或发布；本次交付仅为文档更新。以下 HEAD 为编辑前快照，五仓当时工作树均干净。

## 1. 当前源码集合及最近变更

| 仓库 | 本地目录 / HEAD | 最近提交核对与实施影响 |
| --- | --- | --- |
| Core | `cube-core` / `e4aeef8` | `19e6928` 提交统一方案与初轮基线；`4757b10` 提交直接 import 边界测试、M-00a 与导航；`e4aeef8` 提交 examples Go 1.27.0。更早的 `d67f654` 为 RateLimiter 改用 x/time/rate，`57d4514` 记录相关版本，`f99fe0d` 为历史交接。未迁移 Remote Entity。 |
| Kit | `cube-kit` / `c3426f2` | 与初轮 HEAD 一致。最近包括 U-0101 装载拒绝、U-0100 重入、U-0095 Room、U-0093 Remote Entity、U-0092 Saga 补测，以及 `c31f772` 采样脚本清理调整；没有本轮实现下沉。 |
| Service | `roost-service` / `2e18010` | 已定位，最近包括 U-0103 mail 生成装配补测、U-0098 account、U-0097 platform、U-0096 mail 竞态分支补测，以及 CHANGELOG/采样脚本调整。定位成功解除 ENV-02，不代表新做了一轮服务审计。 |
| Skill | `cube-skill` / `fe23185` | 本轮新增根与嵌套模块身份对齐、`sync` → Core `syncbus` 接线、移除嵌套 replace、身份检查及开发文档。前一 HEAD 为 `24ac8a9`（09-02），更早为持久化文档、事务跟踪及版本提交；不能仅因历史交接记载较新 U 单元，就认定当前 Skill 已含全部历史修复。 |
| Codegen | `cube-codegen` / `16e0c68` | 与初轮 HEAD 一致。`867d710` 增加 cfggen groups/group/-groups，`16e0c68` 更新发布清单；此前为 U-0091/U-0090/U-0089/U-0088 补测。未修改内部 `GOWORK=off`，TOOL-01 仍开放。 |

Codegen 当前清单为 core v1.13.0 / kit v1.12.6 / skill v1.10.3 / service v1.5.4 / codegen v1.14.0；这只是文件内容，不是本次远端 tag/CI 或发布兼容性验证。

## 2. 已落地能力及证据范围

- 五个主模块及三个嵌套模块的 go 指令均为 1.27.0。`D:/whb_s/.roost-refactor-baseline/go.work` 含五仓、Core examples、Skill examples、Skill integration/sync-e2e，共八个模块。
- Skill 身份检查解析仓库 Go import 和全部 go.mod，包含嵌套模块；当前源码已完成 Roost 身份对齐。
- Core 边界检查解析根模块全部 Go import，含测试与非当前 build tag 文件，并跳过嵌套 go.mod。它阻止直接依赖 Kit/Skill/Service/Codegen 和 cube-*；不是完整传递依赖分析，也不代替带标签编译及生命周期测试。
- Core examples 的 Go 版本变更已经提交，不再是用户未提交修改。

M-00a 记录的五仓及嵌套模块 build/vet/test、Skill race、Core/Skill 保护测试、Kit/Service integration vet 结果保留为历史证据。本机对应 `*-final.log`、`core-boundary.log`、`skill-identity-after-race.log` 等仍存在；检查 final 日志未发现 FAIL/panic，边界与 race 日志含成功结果。本次没有重跑这些全量命令，也没有据日志内容重新推定所有命令的退出码或 skip 数。

## 3. 本次定向复跑与环境限制

两条命令均在相应仓库根目录执行，并设置进程级 `GOWORK=D:/whb_s/.roost-refactor-baseline/go.work`：

| 目录 | 命令 | 退出码 | 结果 |
| --- | --- | --- | --- |
| cube-core | `go test -count=1 dependency_boundary_test.go` | 1 | setup failed，`package go/parser is not in std` |
| cube-skill | `go test -count=1 module_identity_test.go` | 1 | setup failed，`package go/parser is not in std` |

错误指向 `C:/Users/tjbdw/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.windows-amd64/src/go/parser`。该目录的 Test-Path 返回 True，故尚不能断言是工具链文件缺失；需要进一步检查当前执行身份下的工具链解析与访问环境。失败发生在标准库解析阶段，测试未运行，不记为行为回归、有效红或检查通过。未修改全局 Go 配置、未重新安装工具链。

## 4. 实施状态与后续顺序

| 项目 | 当前结论 | 下一动作 |
| --- | --- | --- |
| ENV-01 / M-00a | 依赖身份修改已提交；已有对齐基线通过记录 | 保留完成结论；环境恢复后复跑保护检查，不重复改名 |
| ENV-02 | Service 已定位并已有直接基线记录 | 继续纳入五仓及生成消费者验收 |
| M-00 | 直接基线已有通过记录，完整验收待补 | 查清本次工具链问题；补当前源码集合所需证据 |
| M-01 | Core 直接依赖保护已落地，其他部分未完成 | 追踪 Codegen 子进程、临时工程、依赖更新与 Makefile；补显式 dev source-head 生成、编译、运行通路及公共 API/测试支撑设计 |
| M-02～M-09 | 未开始 | M-00/M-01 完整门禁通过后才移动实现 |
| 真实集成与性能 | Mongo/NATS/Redis/etcd 故障矩阵、生成 app 真启动、性能基线仍未验收 | 准备环境并单独记录，不能用 sync-e2e 的总线替身代替 |

优先查清本次工具链环境，再继续 M-01。旧的“Service 未定位”“Skill 仍使用旧模块”“examples 改动未提交”不再作为下一步；早期失败日志留存，但不覆盖 M-00a 已记录的后续结果。

## 5. 前次文档审阅发现的处理边界

本次最近提交未修复 Saga 摘要丢错，也未修改采样器结果分类。B-14/B-18/B-19/B-25 保持开放，未新增 U/T 或变更覆盖矩阵。

- B-14 应按函数分别复核：当前 Saga Command 含 time.Time，Validate 仅要求非零，不能用“纯值字段”推出 Marshal 不会失败。尚需实际入口可达性及行为测试；本次不宣布已修复。
- gap map 的环境错误分类、invalid/HANG 明细及清理范围仍需落实统一方案 §7.2；当前脚本仍有 `git checkout -- .`，不能直接在带有修改的共享工作树运行。
- 自动规则只承担常规筛查，不能据规则上线取消高风险路径人工审计。历史交接正文保留，但后续执行应遵守统一方案的行为证据要求。

本次只更新状态、导航及证据边界；不将提交标题、历史发布清单或包测试成功升级为迁移验收完成。
