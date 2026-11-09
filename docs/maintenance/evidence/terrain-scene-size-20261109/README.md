# 普通场景取地形图大小：AOI 成本测量与验证证据（2026-11-09）

实现与结论见 [方案 §23](../../FEATURE-2026-11-08-editable-terrain.md#23-普通场景取地形图大小实施结果2026-11-09)；进度见 [TERRAIN-STATUS T06](../../TERRAIN-STATUS.md)。分支 `feat/terrain-scene-size`（本地，基于 main `618ee8c5`），不是已发布状态。

## 环境

Apple M5，macOS，Go 1.27。机器长期有后台负载，基准期间交换占用约 15.8～16.9GB（`bench.txt` / `bench-consecutive.txt` 第一行）。基准只比较同一个测试二进制在同一次运行里的不同世界大小（代码相同、只换 `AOIConfig` 的边界与块边长），不是前后两版代码的对照，也不作容量结论。真实依赖（Mongo / NATS / Redis）没有用到。

## 复跑

```bash
# 仓库根
GOWORK=off go test -c -o <tmp>/policy.test ./framework/sync/entitysync/policy/
# 6 轮交替（每轮每档 1 次，bench.txt）；bench-consecutive.txt 是 -test.count 6 的连续跑法，波动更大，只作参考
for i in 1 2 3 4 5 6; do <tmp>/policy.test -test.run XXX -test.bench BenchmarkAOIWorldSize -test.benchmem -test.count 1; done
GOWORK=off go test -count=1 -run TestAOIWorldSizeBlockLimit ./framework/sync/entitysync/policy/
# demo（生成工程，go.mod 追加 replace 到本仓库）
go run ./codegen/cmd/roost project new planet -module example.com/planet -out <tmp>/planet -template game-demo --skip-deps
cd <tmp>/planet && go build ./... && go vet ./... && go test ./...
go test -race -count=3 -run 'TestTerrainScene|TestTerrainWalkIsHeld|TestTerrainALake|TestTerrainEditsDuring' ./internal/service/game/
go test -race -count=1 ./game/scene/runtime/
```

## 基准（`benchstat.txt`，6 轮交替的中位数）

| 世界 | 块边长 | 块数 | 构造 | 块表 B/op | 完整 GC | 移动（间距 25） | 移动（间距 10） |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1000 × 1000（现有） | 100 | 100 | 1.14µs | 6.3KiB | 207µs | 254µs | 4.05ms |
| 2 × 1 块（demo 缺省包） | 100 | 8,192 | 80µs | 449KiB | 245µs | 251µs | 4.17ms |
| 16 × 16 块（块表上限） | 100 | 1,048,576 | 12.0ms | 56.0MiB | 3.21ms | 255µs | 4.53ms |
| 1w 方图 | 100 | — | 拒绝（`TestAOIWorldSizeBlockLimit`） | — | — | — | — |
| 1w 方图 | 1000 | 1,010,025 | 11.5ms | 54.0MiB | 3.02ms | 670µs | 4.88ms |
| 1w 方图 | 6400 | 24,649 | 0.27ms | 1.3MiB | 0.33ms | 640µs | 4.91ms |

- 构造的 B/op 与 allocs/op（每块一次分配）就是块表的常驻内存，跨轮完全相同；时间的 ± 见 `benchstat.txt`，16 × 16 一档移动的 ±79% 来自后台负载，连续跑法（`bench-consecutive.txt`）里同一档是 257µs。
- 每个观察者最坏订阅块数（`observer-blocks`）：块边长 100 时 64，1000 / 6400 时 4，都远低于缺省预算 1024；与世界大小无关。
- 移动一档：每批 32 个目标与其中 8 个观察者移动 80 单位，批量重评 + 收集事件（单 worker）。

## 结果

| 项 | 结果 | 文件 |
| --- | --- | --- |
| `policy` 包 gofmt / vet / `-race`；`go build ./...`；`go test ./codegen/internal/roost/`；根包 `go test .` | 通过 | `core-checks.log` |
| 生成 game-demo：build / vet / 全部测试；`roost id check`、`roost generate --check` | 通过 | `demo-test.log` |
| demo 地形移动与场景用例 race（新增两组与原 P3 三组各 3 遍）、场景运行时 / actor / player / handler race | 通过 | `demo-race.log` |
| 变异 5 项（`sceneWorld` 永远取缺省、`TerrainConfig` 不设上限、场景实体忽略建造参数、`terrainRegion` 不查上限、场景同步 AOI 仍取缺省） | 全部变红 | `mutations.txt` |

原 P3 用例 `TestTerrainWalkIsHeldBySlopesAndRoutesAroundEdits` 在场景改成地形图大小后变红（只横穿 10 个节点的墙从第 10 个节点绕过去，失败文本 `a goal behind a wall across the whole map was routed`），用例改为横穿整张图（`terrainMoveNodes` = 64），断言不变；这是用例对旧场景大小的假设，不是行为退化。
