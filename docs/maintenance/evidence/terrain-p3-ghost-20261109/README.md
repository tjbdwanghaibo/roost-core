# 可编辑地形 P3 Ghost 副本（T05）证据（2026-11-09）

分支 `feat/terrain-p3-ghost`（接续 WIP `c23cf766`，合入 main `3fb089d9`）；设计与结论见[方案 §21](../../FEATURE-2026-11-08-editable-terrain.md#21-p3-ghost-副本t05d192026-11-09)。
机器 Apple M5（Mac17,2），macOS，go1.27.3，`GOWORK=off`。生成工程用 `roost project new planet -template game-demo --skip-deps`，`replace` 到本分支。

| 文件 | 内容 |
| --- | --- |
| [measure-replica.log](measure-replica.log) | `ROOST_TERRAIN_REPLICA_PERF=1`：r=5m 抬高 300 次，提交到邻格副本应用完的延迟与每块推送字节（cellworld Node 回环、真实 nats-server；`gameplay/terrain` 进程内网） |
| [demo-partition.log](demo-partition.log) | 生成工程 `TestTerrainPartition*` 三轮：边界编辑到邻格 Ghost 带、到别的进程的客户端的时间，邻格客户端收到的小编辑字节，重启后补齐时间 |
| [mutations.log](mutations.log) | 变异（在本分支上临时改代码，跑完还原）均能变红：拥有者不查包版本、不跳过自己的块、提交后不推送、不接 witness、副本不比版本、确认推进到当前序号、“原始块”压过新覆盖、装不进来的块不删 |
| [cellworld-elapsed-red.log](cellworld-elapsed-red.log) / [cellworld-elapsed-green.log](cellworld-elapsed-green.log) | cellworld 按心跳实际经过时间积分：修前红（150ms 的一拍只走 50ms）、修后绿 |
| [demo-test-all.log](demo-test-all.log) / [demo-race.log](demo-race.log) | 生成工程 `go test ./...` 与地形相关包 `-race` |
| [seamless-drill.log](seamless-drill.log) | `scripts/test-seamless-demo.sh`（私有 `scripts/mirror-local.sh`）本分支与 main 对照的步骤摘要（路径已替换为占位） |

## 结论摘要

- 提交到邻格副本应用完（r=5m，300 次）：回环 p50 182µs / p90 221µs / p99 834µs / max 1.35ms；真实 nats-server p50 227µs / p90 371µs / p99 946µs / max 1.34ms。每块推送 3,674～3,677 字节（整块 raw DEFLATE，每个订阅进程一份）。进程内网 p50 381µs、2,908 字节。
- 生成工程（三个进程、真实 TCP 客户端）：边界编辑到邻格 Ghost 带约 12ms；到别的进程的客户端约 42～48ms（第一次出现是新订阅的即时全量，之后的更新按 TerrainChunk 的 1 秒周期）；r=3m 抬高到邻格客户端是 21 个按下标的高度更新、载荷 195 字节；重启后的邻格约 7～10ms 补齐。
- 无缝演练：本分支 14 个步骤全部通过；最后一步普通场景 sid 1003 的机器人压测阈值失败（p95 16.45s > 16s）。紧接着在同机对 main `3fb089d9` 跑同一脚本，同一步同样失败（公会 saga 等待，error_rate 1）。判断为既有问题 / 环境负载，与本轮改动无关，原因尚未确认。
- 演练脚本的分区配置 `cell_size` 3000 不满足开地形的要求（6400 的整数倍），演练没有覆盖地形；地形的多进程行为由生成工程用例覆盖。

## 未覆盖

- 真实 Mongo / WAL 下的崩溃重启（“只推持久版本”的前提由 strict 契约推出，未演练）。
- T09 规模：订阅重算按玩家数 × 视点周围约 100 块，单格数千玩家、1w 地图未测；推送扇出未测上限。
- Linux、跨机。
