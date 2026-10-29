# 测试有效性审计（2026-10-29）

维护者要求：“整体检查下现在的test有些是否已经失效有些是否可以删除了”。在 saga JetStream 切换（第 1～6 批，main `4e2ff44c`）之后，按区域由四个 agent 并行审计，基线同为 `4e2ff44c`。

| 区域 | 记录 | 删除 | 加强 | 待维护者决定 |
| --- | --- | --- | --- | --- |
| 根目录门禁、codegen、scripts、cmd、internal | [tooling.md](tooling.md) | 1（孤儿 testdata） | 0 | 2 |
| gameplay、robot、service | [gameplay.md](gameplay.md) | 2 | 0（2 条登记需加强） | 2 |
| framework（不含 saga、dataengine） | [framework.md](framework.md) | 2 | 2（变异证明先红后绿） | 4 |
| framework/saga、framework/dataengine、infra、wiring | [core.md](core.md) | 5（3 个无引用辅助、2 条重复用例） | 0（1 条登记需加强） | 6 |

规则：只删确定的项，每项写明覆盖它的保留用例或承诺已不存在的依据，改动包的覆盖率前后对比；涉及 Nest / DataEngine / Sync 契约、文档点名的守卫、只靠测试存活的生产代码，一律列为待维护者决定，不在本次删除。

合并后验证：`GOWORK=off go build ./... && go vet ./...`、根包门禁与全量 `go test ./...`（见提交说明）。待决定项的处理结论记在各区域记录末尾。
