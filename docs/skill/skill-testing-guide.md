# Skill 阅读与测试指南

需要按实现细节学习时，先阅读
[Skill 当前实现学习手册](skill-implementation-guide.md)；本文件保留为
日常开发时的快速阅读和测试清单。

## 阅读路径

1. 从 `wire_*.go` 和 `parse*.go` 开始：它们定义严格的
   `roost.skill/v2` JSON 边界，拒绝重复键、未知字段和尾随数据。
2. 阅读 `compile_normalize.go`、`ir*.go`：这里把 Wire 数据转换为封闭、
   强类型的 IR；不要从 Runtime 反推 JSON 含义。
3. 阅读 `compile.go` 与各 `compile_*.go`：按固定 Pass 顺序理解 Catalog、
   Type、Graph、Memory、Lifetime、Motion、Budget 和 Visual 的静态证明。
4. 阅读 `lower.go`、`program_*.go`、`inspect.go`：Program 是运行时唯一
   消费的不可变执行计划。
5. 阅读 `runtime*.go`、`executor.go`、`scheduler.go`：理解 Cast Window、
   Flow Turn、事件队列和确定性调度。
6. 阅读 `process*.go` 与 `memory_host*.go`：最后查看 Process、Motion、
   Numeric、Area、State、Owned Entity 与 Temporal Snapshot 的世界交互。
7. 阅读 `skillcompose/`：它只使用 Program Inspector，绝不进入 Runtime。

## 日常测试流程

先运行被改动能力的聚焦测试，例如：

```powershell
go test ./skill -run 'Test(Parse|Prompt|Trace|Recording)' -count=1
go test ./skillcompose -count=1
```

随后运行完整 Skill 验收、静态检查和并发检查：

```powershell
go test ./skill ./skillcompose -count=1
go vet ./skill ./skillcompose
go test -race ./skill ./skillcompose -count=1
go test -run=^$ -fuzz=FuzzParseGeneratedNeverPanics -fuzztime=10s ./skill
go test ./combat ./combatcomponent ./skillsync -count=1
git diff --check
```

## Fixture 规则

每个 `skill/testdata/*.json` 都必须是独立、直接根的
`roost.skill/v2` 定义。验收测试会自动发现它们，并执行
Parse → Compile → Inspect → Activate → Advance → 终态检查。

新增 Fixture 时要同时：

- 为场景提供真实的、可观察的能力断言，而不是复制基础伤害样例；
- 在 `acceptance_test.go` 中声明输入、推进 Tick 和终态；
- 用对应的专门单元测试验证边界与失败路径；
- 对 Prompt 中新增的 JSON 示例保持 Parse → Compile 测试。
