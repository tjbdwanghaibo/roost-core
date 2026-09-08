# M-00：迁移前基线（合并版，2026-09-08）

本页把另一台机器上产出的三份记录（首轮基线、M-00a 消费方对齐续跑、09-08 提交复核）合成一份，只保留**可移植的事实**。三份原文保留在同目录（[M-00a](M-00a_SOURCE_HEAD_ALIGNMENT.md)、[提交复核](COMMIT_REVIEW_2026-09-08.md)），开头已标"已合并"；它们里的 `D:/` 路径、`cube-*` 本地目录名、Windows 入口 Go 1.24 与 `go/parser is not in std` 故障属于那台机器的环境，不是仓库状态。

执行入口：[收敛方案](../ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md)；方法论：[统一实施方案](../CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)。

## 1. 源码集合（origin/main，2026-09-08）

| 模块 | 模块路径 | HEAD | go 指令 | 嵌套模块 |
| --- | --- | --- | --- | --- |
| core | github.com/tjbdwanghaibo/roost-core | `03a5709` | 1.27.0 | `examples/`（go 1.27.0，`e4aeef8` 起） |
| kit | github.com/tjbdwanghaibo/roost-kit | `d674e37` | 1.27.0 | — |
| skill | github.com/tjbdwanghaibo/roost-skill | `a7dd376` | 1.27.0 | `examples/`、`integration/sync-e2e/`（均 replace 到自身工作树） |
| service | github.com/tjbdwanghaibo/roost-service | `9f2bb30` | 1.27.0 | `examples/split/` |
| codegen | github.com/tjbdwanghaibo/roost-codegen | `b11af80` | 1.27.0 | — |

最新 tag：core v1.13.0、kit v1.12.6、skill v1.10.3、service v1.5.4、codegen v1.14.0，tag CI 全绿。codegen 清单 `ci/framework-release.yaml` 与之一致。

模块身份：五仓 go.mod 与全部 Go 文件**没有** `cube-*` 引用。另一台机器记录的 ENV-01（"Skill 仍依赖 cube-core v1.8.0"）是其 Skill 检出停在 `24ac8a9`（09-02）造成的，不是仓库问题；其未推送提交 `fe23185` 作废。

## 2. 依赖事实

- core 直接依赖：chi、viper、mongo-driver v2、x/net、x/time、yaml。
- kit 直接依赖：core、nats.go、go-redis v9、etcd client/api v3、quic-go、kcp-go、viper、mongo-driver。
- skill 直接依赖：core、mongo-driver；对 kit 仅 2 处测试 import（syncstream）。
- service 直接依赖：core、kit（versionstore 54 处、mods 34、servicerpc 9、redis 5）、viper。
- kit 内部 import 图：几乎所有包只依赖 `mods`；`dataengine → nestwal`、`saga → nats, nestwal`、`room / lockstep / robot → nettransport`；测试依赖 `mongo/mongotest`。
- 15 个 core / kit 同名包（actionflow ai configdata dataengine etcd gateway lock lockstep mongo nats nest redis robot saga syncstream）合并到 core 同名包**不产生 import 环**（逐包按传递依赖核对）。
- codegen 运行时不 import 其他四仓；模板里以字符串引用 8 个 kit 包路径、`roost-skill/skill`、`roost-service/servicemetrics`。
- codegen 生成流程内部 `GOWORK=off` 是刻意的（生成工程必须可发布、不带 replace）。source-head 验证由 codegen 的 `framework-compat` 工作流承担：生成工程 → 临时 go.work 挂 core / kit / skill 源码 → 编译测试 → 校验无 replace → dev compose 真实启动；每日 03:17 与 push 触发，可指定三仓 ref。

## 3. 本机（macOS）与 CI 的验证结果

| 项 | 结果 |
| --- | --- |
| 五仓 main CI（Linux） | 上述 HEAD 全绿 |
| core `TestCoreDependencyBoundary`（`4757b10` 引入，AST 扫根模块全部 Go 文件含测试与非当前 build tag，禁 kit / skill / service / codegen / cube-*） | 本机 macOS 通过；Linux CI 通过 |
| 五仓 nightly-gapmap（09-07 03:30，max=20） | core 197/382、kit 191/358、service 116/204、skill 35/83、codegen 36/175 无覆盖/采样；09-08 起采样器跳过 `*_gen.go` |
| 真实 Mongo / NATS / Redis / etcd 故障矩阵 | 五个 toxiproxy 切片存在（kit `scripts/integration`，账本第 8 节），本轮未重跑 |
| 生成工程真实启动 | 由 `framework-compat` 每日跑，最近一次绿 |
| 性能基线 | 未采集；收敛方案 P5 要求迁移前后同机对比，迁移前那一轮在 P1 前采 |

另一台机器上 Windows 工具链 `go/parser is not in std` 的失败是环境问题，不作为任何模块的阻塞记录。

## 4. 在途改动的处置（与收敛方案 §6 一致）

- 统一实施方案：方法论沿用；架构边界按收敛方案改写；M 编号保留并映射到 P2 ①～⑨。
- `TestCoreDependencyBoundary`：保留，P1 改规则（不再禁 skill / service）。
- `fe23185`：不推送。
- TOOL-01：非问题。
- mongotest 归属：P2 ② 迁入 core。
- B-25：已完成（五仓采样器跳过 `*_gen.go`）。
- U / B / T 编号继续；矩阵不因搬迁改格。

## 5. 未验证项（进入 P5 前必须补）

- 迁移前性能基线（P1 前采）。
- 故障矩阵五切片在新路径上的重跑（P3 门禁）。
- `framework-compat` 对 `consolidation` 分支 / alpha 的运行（P4 门禁）。
- 生成工程 `roost upgrade --consolidate` 的端到端（P4 门禁）。
