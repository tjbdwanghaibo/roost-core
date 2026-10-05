# N10 第一批证据（2026-10-05，revn10）

基线 `197f7bb9`（origin/main）。macOS / Go 1.27.0，`GOWORK=off`，不依赖外部服务。[本轮](../../../review/REVIEW-2026-10-05-noncore-n10.md)。

| 文件 | 内容 |
| --- | --- |
| `red.txt` | 修前：只加新用例、不改生产代码，`./ai ./actionflow ./hotcode` 的失败原文；hotcode 对撞单独再跑 5 次普通 + 1 次 race |
| `green.txt` | 修后：gofmt / vet、四包 race×3、新用例 -v、全仓 build / vet、根包、`./nest` race、全新生成 game-demo build / vet 与相关包测试 |
| `probes.txt` | 临时探针（跑完删除，不提交）的输出：ActionRunner + MissionRunner 最小接线（M3 / M4 / A8）与行为树中断传播（T4 / T5） |

复跑：`GOWORK=off go test -count=1 -race ./ai ./actionflow ./hotcode`；修前红用 `git stash push -- ai/controller.go actionflow/action_runner.go hotcode/registry.go` 后同命令。
