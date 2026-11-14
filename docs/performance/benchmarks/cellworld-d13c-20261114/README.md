# CellWorld Base 按来源 Cell 分订阅（D13 C，2026-11-14）

基线 `1f618e1f`（main），改动在分支 `tmp/v148-cellworld-c`（未合入）。macOS arm64 Apple M5（4P + 6E，10 核）/ 32GiB，Go 1.27。测量期间宿主机另有别的任务压 CPU（每轮开始时的 load average 12～23，见 `core-runs.log` 每轮第一行），swap 已用约 4.9GiB / 6GiB。结论见 [CELLWORLD.md §按来源分订阅](../../CELLWORLD.md#base-按来源-cell-分订阅d13-c2026-11-14)。

- `red-green.txt`：新用例在基线上的失败输出、修后通过，以及 6 个变异各自变红的输出。
- `base-receive-before.txt` / `base-receive-after.txt` / `base-receive-benchstat.txt`：`BenchmarkBaseReceiveSources`（预编码的全量 witness，16 观察者 × 64 对象一批，1 / 4 个来源格），基线与修后两个测试二进制同机交替 6 轮，每轮 2s。
- `core-runs.log`：`TestCellWorldMeasure`（1000 玩家 + 每格 2250 NPC，4 Cell，5% 运动，20s 窗口）基线与修后同机交替各 3 轮，只保留测量输出行。测量自带 witness P99 ≤ 50ms 的断言，宿主机过载时两边都会报红（修后 3 轮里 1 轮通过）。

完整日志与测试二进制没有入库，留在本次会话的临时目录。M-15（Robot 千人，A + D + C 之后）没有跑。

## M-15：Robot 千人复跑（2026-11-15，A + D + C 之后，同机对照 main）

[性能入口](../../../maintenance/evidence/v147-performance-preparation-20261114/README.md)的 `s11-cellworld-robot` 段（`scripts/test-seamless-demo.sh`，ROBOT_ONLY + CAPACITY，1000 人、60 秒、active 5%、预热 30 秒），macOS 本机，load 约 3.5～4.1。先跑 feat/v148（含 D13 C），再在同一台机器上跑 main `603dd139`（v1.47.0 + 最低 core，无 D13 C）作对照。原始输出在各自检出的 `artifacts/perf/v147-remaining/s11-cellworld-robot-{v148,main603}/`（不入库）。两次都按阈值判失败（error_rate 门槛 0）。

| 指标 | main（无 C） | feat/v148（有 C） |
| --- | --- | --- |
| Robot 成功 / 失败 | 629 / 371 | 690 / 310 |
| 失败原因 | 全部“预热期未到分散目标” | 同左 |
| 移动请求完成 / 错误 / p99 | 564,906 / 0 / 21.95 ms | 619,376 / 0 / 18.48 ms |
| 每秒完成请求 | 5,795 | 6,348 |
| Base（game-1000）“未进入复制场景” | 398 | 333 |
| Base 进入钩子失败（rehome / alliance `sync canceled`、mail / chat NATS 请求超时等） | 2,079 | 1,756 |
| NATS slow consumer | 0 | 0 |

读法：

- 失败的机器人集中在后上线的那一批（ID 尾部），目标点在地图上均匀分布；它们没走到，是因为在 1000 人约 5 秒的登录潮里 Base 的进入流程超时（进入钩子与世界频道公告超时、没进入复制场景），进不了场景就不会移动。瓶颈在 Base 的登录 / 进入路径，不在 Witness 接收。
- C 在同机对照下有改善（失败 371 → 310，吞吐 +9.5%，移动 p99 22 → 18 ms），没有退步；但千人仍不达标，50ms 端到端也无从判定。
- 下一步该查 Base 登录潮下的进入路径（进入钩子的 Nest sync 与 NATS 请求在 5 秒爬坡里排队），而不是 D13 的 B / E。
