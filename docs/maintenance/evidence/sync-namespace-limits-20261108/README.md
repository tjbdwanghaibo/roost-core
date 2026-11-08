# 按命名空间单独计上限与快照预算：证据（2026-11-08）

对应 [方案 §15 / D15](../../FEATURE-2026-11-08-editable-terrain.md#15-地形单独计对象上限与快照预算d152026-11-08) 与 [ENTITY_SYNC“按命名空间分开计”](../../../framework/ENTITY_SYNC.md#按命名空间分开计namespaces)。

## 基准对照（同机交替）

- 基线：b20f439d（P2 服务端）；新：本分支实现。两边各编译一个测试二进制，`base` / `new` 交替各跑 6 轮（`-test.count 1`，`BenchmarkManagerFlush$|BenchmarkByteBudgetRecoveryDrain|BenchmarkSessionRecovery|BenchmarkExhaustedSnapshotWindow`），再单独交替 8 轮 `BenchmarkManagerFlush$` 复核。Apple M5，机器有后台负载、swap 已用约 8GB（耗时方差大，见 ± 列）。
- 原始数据：[base.txt](base.txt) / [new.txt](new.txt) → [benchstat.txt](benchstat.txt)；复核 [base-flush.txt](base-flush.txt) / [new-flush.txt](new-flush.txt) → [benchstat-flush.txt](benchstat-flush.txt)。

| 指标 | 结论 |
| --- | --- |
| sec/op | 全部无显著差异（6 轮里 `sessions=256/profiles=1` 一项 p=0.041、+2.1%，但新侧方差 ±145%；8 轮复核 p=0.72、几何平均 −1.6%） |
| allocs/op | 全部相同（`ByteBudgetRecoveryDrain` +0.17%） |
| B/op | 稳定 tick（`ManagerFlush`）相同；`SessionRecovery` +0.45%；`ByteBudgetRecoveryDrain` +1.3%（快照候选多一个路号字段、每轮多一个按路的小切片） |
| frames / packs / wire-B | 全部相同（逐字节交付不变） |

第一版实现 session 结构多了 48 字节（两个切片头），`SessionRecovery` 的 B/op +7.9%；改成两个指针并把 `held` / `referencesOwned` 并到 `next` 所在字长后 session 是 112 字节，与原来的 104 字节同属 112 字节分配档，回到 +0.45%。

配置了命名空间时的稳定 tick：[ns.txt](ns.txt)（`BenchmarkManagerFlushWithNamespaces`，32 个默认命名空间 + 16 个 terrain subject、terrain 每会话每 Flush 4 个）。与它比较的是 subject 数为 2/3 的 `BenchmarkManagerFlush`，不能直接相减；用于以后改动的自身对照。

## 先红后绿（生成工程）

把本分支的 `terrain_sync_test.go.tmpl` 放到 b20f439d 上生成 game-demo（`--skip-deps`、replace 指回该检出）跑同样四个用例：[demo-red-b20f439d.txt](demo-red-b20f439d.txt)，四个全红——

- `TestATerrainViewAtItsCapKeepsTheSession`：第一次 Flush 后只有 4 个其他对象（Manager 级预算限流了怪）；
- `TestTerrainLeavesPlayerAndMonsterReplicationByteIdentical`：不开地形 8 帧（含 Flush 分界），开地形 10 帧；
- `TestTerrainDoesNotWidenThePlayerObjectLimit`：101 人里 0 个会话因超限关闭（上限被放宽到 164）；
- `TestTerrainChunksOverTheirLimitCloseTheSession`：地形视野 80 块时会话不关（总上限 164 装得下），10 秒超时。

本分支：[demo-green.txt](demo-green.txt)，四个全绿。

entitysync 包内 8 个新用例用的是新 API，不能在旧代码上编译；以变异代替：`classOf` 恒返回类别 0（所有命名空间归一类）后 8 个全红，恢复后全绿。
