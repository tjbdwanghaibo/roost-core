# M-00a：消费方对齐与五仓基线续跑

关联：[M-00 初轮](M-00_BASELINE.md)、[统一方案](../CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)。

> 本页保留 M-00a 执行时的结果。后续 Core `4757b10` 已提交边界保护及本记录，`e4aeef8` 已提交 examples 的 Go 版本修改；当前提交集合与本次复跑限制见 [09-08 提交复核](COMMIT_REVIEW_2026-09-08.md)。下文“未提交”仅指当时状态。

状态：消费方依赖对齐完成；M-00 的五仓直接构建基线已通过，真实环境/生成流程/性能证据仍待补。M-01 已开始依赖边界保护，尚未完成；M-02 未开始。

## 1. 提交与工作树

- Core `19e6928`：已提交先前的统一方案、历史入口和首轮基线。
- Skill `fe23185`：依赖身份对齐、嵌套模块接线、身份检查及开发文档。
- Service 已定位：`D:/whb_s/roost-service`，HEAD `2e180101c6f4d1188843914aed13a32245c7f852`，工作树干净。
- Kit/Codegen HEAD 与 M-00 初轮一致，无修改。
- Core 原有 `examples/go.mod` 改动仍保留且未提交。本轮只在外部 go.work 加入其模块，未修改该文件。
- 未推送、打 tag 或发布；本轮提交不代表 release 验收。

## 2. M-00a 修改内容

Skill 根模块、examples、integration/sync-e2e 的 Go import 与 go.mod 统一为 Roost Core/Kit，sync-e2e 使用 Core 的 syncbus 包；移除 Skill 嵌套模块的本地 replace，以共享 go.work 联调。工具链下限为 Go 1.27。

依赖图中的 Core v1.13.0、Kit v1.12.6、Skill v1.10.3 是 go.mod 解析基准，不是本轮正式兼容矩阵。三个模块的 `GOWORK=off go mod tidy` 均成功，用于维护依赖文件；行为验收使用本地工作区。

中间失败也记录：嵌套模块原有 Skill v1.4.0/v1.5.0 下限在移除 replace 后触发 v1.4.0.mod 的 proxy 404，不能算代码回归。对齐到上述解析基准后五仓及嵌套模块恢复通过。

这是结构/依赖前置子批，不注册为“一包一个缺陷类”的 U 单元，不修改 U/B/T 历史结论。未修改 Skill 的 JSON schema、wire、checkpoint 或算法。

## 3. 回归保护

### Skill：TestFrameworkModuleIdentity

- 解析仓库全部 Go import，检查全部 go.mod，包括根 ./... 不覆盖的嵌套模块。
- 拒绝引入另一套框架模块身份，防止只在旧依赖上测试而误认为 source-head 通过。
- 用 git archive 导出修改前 HEAD 到隔离副本，只加入新检查；`go test module_identity_test.go` 退出 1，明确报告实际旧 import 和 go.mod 引用。
- 当前源码相同检查退出 0，全量 race 退出 0。
- 没有在共享工作树临时回退文件。

### Core：TestCoreDependencyBoundary（M-01 首项）

- AST 扫描根模块全部 Go import，含 _test.go 和非当前 build tag 文件。
- 拒绝 Core 依赖 Kit/Skill/Service/Codegen 或另一套框架模块身份。
- 嵌套模块单独验收，不误判为 Core 自身实现。
- 加禁止/允许 import 分类用例；本轮检查与 Core vet 通过。
- 这是源码直接依赖保护，不等于全部第三方传递依赖或运行行为都已审核；实际 go list 结果也单独记录。

## 4. 验证结果

工作区：`D:/whb_s/.roost-refactor-baseline/go.work`，现在含五个主模块与 Core examples、Skill examples、Skill sync-e2e。通过进程级 GOWORK 指定，未修改全局配置。

| 模块 | build ./... | vet ./... | test -count=1 -timeout 90s ./... |
| --- | --- | --- | --- |
| Core | 0 | 0 | 0 |
| Kit | 0 | 0 | 0 |
| Skill | 0 | 0 | 0 |
| Codegen | 0 | 0 | 0 |
| Service | 0 | 0 | 0 |
| Core examples | 0 | 0 | 0 |
| Skill examples | 0 | 0 | 0 |
| Skill sync-e2e | 0 | 0 | 0 |

额外：

- Skill `go test -race -count=1 ./...`：0。
- Core 边界测试、Skill 身份检查新增后单独复跑及 vet：0。
- Kit/Service `go vet -tags integration ./...`：0，仅标签编译/静态证据。
- 五主模块及 Skill 两嵌套模块执行 `go list -deps -test`，实际包依赖未出现 cube-* 模块；Module.Dir 输出已保存。
- LSP：gopls 未在工具等待时间内返回诊断，不能宣称 LSP 无问题；上述编译、vet 与测试均已实际执行。

日志位置：共同父目录 `.roost-refactor-baseline/logs/`：`*-final.log`、`skill-identity-before.log`、`skill-identity-after-race.log`、`core-boundary.log`、`kit-integration-vet.log`、`service-integration-vet.log` 等。日志目前是本机制品。

注意：sync-e2e 使用总线替身及真实文件日志；不等于真实 NATS 集群测试。全量单测成功也不代表依赖门控的 skip 测试已执行。

## 5. 阻塞项更新

- ENV-01（Skill 依赖身份）：已解除，当前包依赖解析有验证。
- ENV-02（Service source-head）：已解除，五仓直接编译/单测通过。
- TOOL-01（Codegen 内部 GOWORK=off）：仍开放，未修改依赖解析实现。
- 真实 Mongo/NATS/Redis/etcd 故障矩阵、生成工程 app 真启动、性能基线：尚未执行。

## 6. 下一步

继续 M-01：完整追踪 Codegen 的临时工程、生成器子进程、依赖更新和 Makefile，设计显式 dev workspace 通路并加生成+编译+运行测试；保留发布模式隔离语义。然后完成 Remote Entity 的 Core 公共 API 和 Mongo 测试支撑拆分设计。

在 TOOL-01 和真实链路证据未补齐前，不把 M-00/M-01 宣布全部完成，不开始 Remote Entity 的实现搬迁。
