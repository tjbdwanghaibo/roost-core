# cellworld v2.2（game-demo 接入分区 Scene）证据，2026-11-03

分支 `feat/seamless-world-prototype`，macOS（Apple M5）同机。说明见 [无缝大世界方案 §10](../../../maintenance/FEATURE-2026-11-03-seamless-world.md#10-v22demo-接入分区-scene2026-11-03)。

| 文件 | 内容 |
| --- | --- |
| `strict-getter-red-before.txt` | `TestWorldWithAGetterThatRefusesAbsentEntities` 修前（Node 把每个 ID 放进加锁集合）红 |
| `self-mirror-red-before.txt` | `TestCellCrashPlayerReentersFromSnapshot` 的客户端断言修前红 |
| `e2e-drill.log` / `e2e-evidence.json` | 最终一轮 `scripts/test-seamless-demo.sh` 的测试客户端输出与各步计数（含崩溃期间每 500ms 的 Base 路由状态 `cell_crash_trace`、首末两次各进程 `gm.scene.partition`） |
| `e2e-partition-log-lines.txt` | 各 game 进程加入分区场景、刷怪的日志行 |
| `e2e-robots-sid1003.txt` | demo 机器人（`cmd/loadtest -count 2`）在普通场景（sid 1003）上的结果 |
| `regression-summary.txt` | 不回归全套的逐项结果与耗时 |

各进程完整日志在工作树的 `artifacts/perf/cellworld-v22/e2e/logs/`（未提交）。依赖：`scripts/mirror-local.sh` 私有实例（偏移 30000），etcd 私有端口 58379，用后清理。
