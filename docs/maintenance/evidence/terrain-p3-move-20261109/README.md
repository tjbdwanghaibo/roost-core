# 可编辑地形 P3 移动与寻路（单格）验证证据（2026-11-09）

实现与设计见 [方案 §22](../../FEATURE-2026-11-08-editable-terrain.md#22-p3-移动与寻路实施结果2026-11-09)；进度见 [TERRAIN-STATUS T06](../../TERRAIN-STATUS.md)。分支 `feat/terrain-p3-move`（本地），不是已发布状态。基准、变异与首轮测试在 main `23247c38` 基础上取得；变基到 `93c00538`、再合并 main `3fb089d9`（T01/T02 编辑身份）后，生成 demo 的 build / vet / 全部测试与新增端到端 race 重跑通过（`demo-test-merged.log`、`demo-race-merged.log`），仓库侧 build / vet / race / codegen / 根包 / 文档锚点也重跑通过（`core-merged.log`）；库代码在两次变动中没有改。

## 环境

Apple M5，macOS，Go 1.27；机器长期有后台负载，基准开始时交换占用约 14.2GB / 15GB（`bench.txt` 第一行）。基准是两个测试二进制交替跑 6 轮，取中位数；波动大的行以 `benchstat.txt` 的 ± 为准，只作同机前后对照，不作容量结论。真实依赖（Mongo / NATS / Redis）没有用到：demo 端到端的持久层是 `terrain_test.go` 的离线替身。

## 复跑

```bash
# 库（仓库根）
GOWORK=off go test -race -count=1 ./gameplay/terrain ./gameplay/movement
# 基准（6 轮交替，见 bench.txt）
GOWORK=off go test -c -o <tmp>/terrain.test ./gameplay/terrain
GOWORK=off go test -c -o <tmp>/movement.test ./gameplay/movement
<tmp>/terrain.test -test.run XXX -test.bench 'BenchmarkWalkerStep|BenchmarkWalkerFindPath|BenchmarkGridFindPath' -test.benchmem -test.count 1 -test.benchtime 1s
<tmp>/movement.test -test.run XXX -test.bench 'BenchmarkMoveAdvance' -test.benchmem -test.count 1 -test.benchtime 20000x
# demo（生成工程，go.mod 追加 replace 到本仓库）
go run ./codegen/cmd/roost project new planet -module example.com/planet -out <tmp>/planet -template game-demo --skip-deps
cd <tmp>/planet && go build ./... && go vet ./... && go test ./...
go test -race -count=5 -run 'TestTerrainWalkIsHeld|TestTerrainALake|TestTerrainEditsDuring' ./internal/service/game/
go test -race -count=1 -run 'TestTerrain|TestAPlayer|TestATwice|TestTheScenes|TestARooted' ./internal/service/game/
go test -race -count=1 ./game/scene/runtime/ ./game/actor/ ./game/entities/player/ ./game/handler/
```

## 结果

| 项 | 结果 | 文件 |
| --- | --- | --- |
| `gameplay/terrain`、`gameplay/movement` race | 通过 | `core-race.log` |
| 生成 game-demo：build / vet / 全部测试；`roost id check`、`roost generate --check` | 通过 | `demo-test.log` |
| demo 地形移动端到端 race（新增三组各 5 遍）、场景移动 / 地形同步 / 场景运行时 race | 通过 | `demo-race.log` |
| `go test ./codegen/internal/roost/`、根包 `go test .`、`check-doc-anchors.py` | 通过 | `core-checks.log` |
| 变异 11 项（M1～M5 `gameplay/terrain`，M6～M7 `gameplay/movement`，M8～M11 生成的 demo） | 全部变红 | `mutations.txt` |

## 基准（6 轮中位数，`benchstat.txt`）

| 项 | 现有（P3 前） | P3 | 说明 |
| --- | ---: | ---: | --- |
| 单步判定 `Walker.Step` | — | 9.9ns，0 分配 | 正交一步，起伏地形 |
| 一拍移动（5Hz、12 单位） | 233ns（二维格子） | 288ns（格子 + Walker） | 3 次分配 / 137B 两者相同；无格子无地形 114ns，只有 Walker 250ns |
| 寻路 50m（起伏） | 271µs（格子四方向 A*） | 73µs，展开 378 个 | 不加权（最优）422µs、展开 1795 个 |
| 寻路 200m（起伏） | 250µs（格子） | 170µs，展开 683 个 | 不加权时展开 14348 个，超出 4096 上限（`TestFindPathSearchWeightBoundsTheCostAndTheSearch`） |
| 寻路 200m（平地） | — | 62µs，展开 201 个 | |
| 扫满 4096 个节点（终点被围住） | 1.28ms（格子） | 1.21ms | 两者都在 `MoveTarget` 的 Nest handler 里同步执行 |

格子 A* 的对照是同样大小的无障碍 `GridTerrain`（1 格 = 1m 节点），不是同一张地形：格子 A* 没有高度，是“P3 之前 demo 寻路的成本”，不是同一问题的两个解法。
