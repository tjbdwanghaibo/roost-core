# CellWorld Base 按来源 Cell 分订阅（D13 C，2026-11-14）

基线 `1f618e1f`（main），改动在分支 `tmp/v148-cellworld-c`（未合入）。macOS arm64 Apple M5（4P + 6E，10 核）/ 32GiB，Go 1.27。测量期间宿主机另有别的任务压 CPU（每轮开始时的 load average 12～23，见 `core-runs.log` 每轮第一行），swap 已用约 4.9GiB / 6GiB。结论见 [CELLWORLD.md §按来源分订阅](../../CELLWORLD.md#base-按来源-cell-分订阅d13-c2026-11-14)。

- `red-green.txt`：新用例在基线上的失败输出、修后通过，以及 6 个变异各自变红的输出。
- `base-receive-before.txt` / `base-receive-after.txt` / `base-receive-benchstat.txt`：`BenchmarkBaseReceiveSources`（预编码的全量 witness，16 观察者 × 64 对象一批，1 / 4 个来源格），基线与修后两个测试二进制同机交替 6 轮，每轮 2s。
- `core-runs.log`：`TestCellWorldMeasure`（1000 玩家 + 每格 2250 NPC，4 Cell，5% 运动，20s 窗口）基线与修后同机交替各 3 轮，只保留测量输出行。测量自带 witness P99 ≤ 50ms 的断言，宿主机过载时两边都会报红（修后 3 轮里 1 轮通过）。

完整日志与测试二进制没有入库，留在本次会话的临时目录。M-15（Robot 千人，A + D + C 之后）没有跑。
