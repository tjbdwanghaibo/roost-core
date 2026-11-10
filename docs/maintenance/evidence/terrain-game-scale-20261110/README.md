# 游戏尺度参数（方案 §29）证据

分支 `feat/terrain-game-scale`，基于 main `cf89e081`。取值、实测结论与待确认项见[方案 §29](../../FEATURE-2026-11-08-editable-terrain.md#29-游戏尺度参数视野-32m单进程-2048m同步-128-个2026-11-10)。

机器：Apple M5，macOS，Go 1.27。**负载记录**：开工时 loadavg 480 / 314 / 161（别的进程的瞬时高峰），交换占用约 22GB / 23.5GB；基准期间 loadavg 7～13、交换占用约 21GB（`bench-env.txt`、`bench-block-env.txt` 每轮记一次）。时间只作同一轮里的对照，不作容量结论。

生成工程的做法（下文命令都在生成的工程根目录里跑，`<core>` 是本分支检出的绝对路径，`<base>` 是 `cf89e081` 的检出）：

```
GOWORK=off go run ./codegen/cmd/roost project new gd -module example.com/gd -out <dir>/gd -template game-demo --skip-deps
# go.mod 追加：replace github.com/tjbdwanghaibo/roost-core => <core>
GOWORK=off go mod tidy
```

## 1. 先红后绿

### 1.1 修前

在 `cf89e081` 生成的工程里删去 `building_sync_test.go` 的旧 `TestBuildingsAtTheTerrainMainViewScale` 与 `TestManyBuildingsDegradeWithTheCrowd`，加一个文件放本分支的四个用例（函数体与修后相同；唯一的编译垫片：`TestAnOrdinarySceneTakesA2048mMap` 构造场景时的名额参数，旧代码没有 `View.MaxVisible`，换成旧常量——这个用例在更早的 `terrainRegion` 处就失败，用不到它）：

```
GOWORK=off go test -count=1 -run '^<name>$' ./internal/service/game/
```

四个都失败，原文见 [red-before.txt](red-before.txt)，都落在承诺本身：

| 用例 | 修前失败 |
| --- | --- |
| `TestBuildingsAtTheTerrainMainViewScale` | `timed out waiting for the player, the monster and the tower 31m away`（旧进入半径 30m） |
| `TestManyBuildingsDegradeWithTheCrowd` | `timed out waiting for the viewer's session settled at its limit`（等 164，旧会话上限 100） |
| `TestTheGameScaleMainViewHoldsTheNearest128` | `player 34000 holds 64 other subjects (itself: true), want the nearest 128`；`player 35000 holds 100 objects (itself: true), want itself among exactly 164` |
| `TestAnOrdinarySceneTakesA2048mMap` | `32 × 32 chunks (2048m × 2048m) on an ordinary scene: … at most 1048576m² (about 1024m × 1024m) …` |

### 1.2 修后

[green.txt](green.txt)：上面四个与改动的用例（`TestTerrainSceneWorldAndItsLimit`、`TestBuildingsTakeMainViewPlacesLikeAnyoneElse`、`TestBuildingsHaveNoReplicationOfTheirOwn`、`TestTerrainMainViewFitsTheBlockBudget`、`TestACrowdedSpawnPointKeepsEveryReplicationSessionOpen`、`TestACrowdOverflowingASessionShrinksTheViewInsteadOfDisconnecting`、`TestTerrainDoesNotWidenThePlayerObjectLimit`）及字节测量通过。

| 检查 | 命令 | 结果 |
| --- | --- | --- |
| 生成工程全量 | `go build ./... && go vet ./... && go test -count=1 ./...` | 通过（[demo-test.log](demo-test.log)） |
| 相关包 race | `go test -race -count=1 ./internal/service/game/ ./game/scene/... ./game/lifecycle/ ./game/settings/ ./game/terrainmap/`；场景 / 建筑 / 地形同步相关用例 `-race -count=3` | 通过（[demo-race.log](demo-race.log)） |
| ID 与生成物漂移 | `roost id check`；`roost generate --check` | `IDs are valid`；`generated files are up to date` |
| core | `go build ./...`；根包 `go test -count=1 .`；`go test -count=1 -timeout 60m ./codegen/internal/roost/`；`python3 scripts/check-doc-anchors.py` | 通过（根包 16s；codegen 392s——第一次与基准并跑、撞上缺省 10 分钟超时，单独重跑通过；锚点 266 个文件 0 处断链） |

C# 代码没有改动（`SyncFrame.Decode` 缺省按 100 × 2 收得下每帧 164 个对象），没有跑 dotnet 用例。

## 2. 变异

[mutations.txt](mutations.txt)：在生成工程里逐项改实现、跑相关用例、再还原。11 项全部变红：

| 变异 | 变红的用例（首条） |
| --- | --- |
| M1 进入半径回到 30m | 构造被拒：`enter radius 3000 is not on the view radius grid (min 1600, step 320)` |
| M1b 进入 / 离开放大一档（35.2m / 38.4m，仍在网格上） | `a monster 33.4m away entered the main view: the enter radius is wider than 32m` |
| M2 去掉滞回（离开 32m） | `stepping back 2m (33～33.4m, inside the 35.2m leave radius): player false, monster false, tower false; want all kept` |
| M3 离开上限 63m | `the widest view a client can have: {LOD:0 Radius:5760}, <nil>; want 64m` |
| M4 开地形的名额仍是 64 | `player 34000 holds 64 other subjects …, want the nearest 128`；引用表 164 ≠ 228 |
| M5 会话上限固定 100 | `0 sessions lost, 4060 objects deferred: the cap of 128 should keep every session inside its limit` |
| M6 Manager 不设 `Limits` | 同上；`player 35000 holds 100 objects …, want itself among exactly 164` |
| M7 地图上限仍是 1024m | `32 × 32 chunks (2048m × 2048m) on an ordinary scene: … too large` |
| M8 地图上限放到 2112m | `33 × 32 chunks (2112m × 2048m) on an ordinary scene: <nil>` |
| M9 不开地形的名额也改成 128 | `player 20000 holds 102 other subjects, want the main view's cap 64`；`the map without terrain changed` |
| M10 `Normalize` 不补名额 | `a view without a cap normalizes to a cap of 0, want 64` |

## 3. 整图装入

`TestTerrainGameScaleLoadMeasure`（`scene_view_scale_test.go`），每项 3 轮，原文 [load-measure.txt](load-measure.txt)：

```
ROOST_TERRAIN_SCALE_MEASURE=1 GOWORK=off go test -count=1 -v -run TestTerrainGameScaleLoadMeasure ./internal/service/game/
```

| 地图 | 包 | 装入（开始到装完） | 视图 + 块记录 | 场景 + 地形同步的块表 | 合计常驻 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1024m 平地 | 18KB | 2.7～5.5ms | 3.0MiB | 0.08MiB | 3.1MiB |
| 2048m 平地 | 0.1MiB | 7.5～9.3ms | 12.1MiB | 0.28MiB | 12.4MiB |
| 2048m `-shape generated` | 2.8MiB | 11.4～16.6ms | 12.1MiB | 0.28MiB | 12.4MiB |
| 2048m 平地、每块都改过 | 0.1MiB | 0.68～0.87s（替身把 1024 条记录建成实体 0.33～0.52s） | 12.1MiB + 实体 17.6MiB | 0.28MiB；`loadEdited` 0.6～1.4ms | 30.5MiB |
| 4096m 平地（超过上限，参考） | 0.3MiB | 24～27ms | 48.3MiB | 1.1MiB | 49.6MiB |

持久层是离线替身，真实 Mongo 读记录的时间没有测。

## 4. 场景同步基准

`BenchmarkSceneMainViewScale`（人群见方案 §29.5）。复跑：

```
GOWORK=off go test -c -o <tmp>/scale-new.test ./internal/service/game/        # 本分支生成的工程
GOWORK=off go test -c -o <tmp>/scale-old.test ./internal/service/game/        # cf89e081 生成的工程
# 6 轮，每轮七个变体（old 的 view30m/block3200 与 new 的六个）各跑一次、顺序轮换，每次 300 op
<tmp>/scale-old.test -test.run '^$' -test.bench 'BenchmarkSceneMainViewScale/view30m/block3200$' -test.benchtime 300x -test.count 1 -test.timeout 0
<tmp>/scale-new.test -test.run '^$' -test.bench 'BenchmarkSceneMainViewScale/<variant>$' -test.benchtime 300x -test.count 1 -test.timeout 0
# 块边长复核：map2048/view32m-128/block1600 与 block3200 交替 10 轮（bench-block.txt）
benchstat bench-old.txt; benchstat bench-new.txt; benchstat bench-block.txt
```

中位数（[benchstat.txt](benchstat.txt)）：

| 变体 | sec/op | B/op | blocks/obs | heap-MiB | visible/obs | buildings/obs | missed/town-obs | enter-B/s/obs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| old：view30m/block3200（1024m） | 3.02ms ±25% | 1.05Mi | 9.13 | 67.3 | 30.29 | 15.36 | 0.004 | 389 |
| map1024/old-view30m-64/block3200 | 3.08ms ±13% | 1.05Mi | 9.13 | 67.2 | 30.29 | 15.36 | 0.004 | 389 |
| map1024/view32m-128/block3200 | 3.66ms ±272% | 1.15Mi | 10.01 | 72.0 | 34.32 | 17.43 | 0 | 409 |
| map2048/view32m-128/block1600 | 14.9ms ±36% | 5.15Mi | 28.86 | 290.2 | 34.16 | 17.22 | 0 | 408 |
| map2048/view32m-128/block3200 | 19.9ms ±25% | 4.83Mi | 10.13 | 281.8 | 34.16 | 17.22 | 0 | 408 |
| map2048/view32m-128/block6400 | 22.5ms ±18% | 5.31Mi | 4.33 | 279.2 | 34.16 | 17.22 | 0 | 408 |
| map2048/view64m-128/block3200 | 42.7ms ±17% | 42.8Mi ±38% | 26.50 | 536.9 | 85.81 | 39.90 | 66.51 | 344 |

块边长复核（`bench-block.txt`，10 轮）：block1600 20.5ms ±27%，block3200 19.4ms ±18%——在噪声内相同，保持 32m。`map1024/view32m-128` 的 ±272% 来自一次 13.6ms 的离群值，6 次里 4 次约 3.1ms。

## 5. 下行字节

`TestMainViewMeasureMovingCrowdBytes`（[green.txt](green.txt)，另一次运行的数字在方案 §29.6）：看的人周围 N 个玩家同时走，每拍（200ms）一次复制。64 个：每拍 7.6KB、每个对象约 119 字节、37.1KB/s；128 个：每拍 15.1KB、每个对象约 118 字节、73.7KB/s（帧 payload，不含线上消息头）。超过每观察者 50KB/s；增量字节压缩在另一轮进行，结论待其合入后按同一用例重算（方案 §29.6、KNOWN-LIMITS）。

## 6. 未覆盖

真实 Mongo / NATS、真实进程演练、Unity、跨机与长稳没有跑；分区场景没有放大（§28.6）；基准在有后台负载的机器上，时间的误差带宽。
