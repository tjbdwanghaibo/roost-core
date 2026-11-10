# 开地形场景的主视野尺度（方案 §28）证据

分支 `feat/terrain-main-view-scale`，基于 main `7da1f62e`。参数、块边长的选择、下行估算、挤占结论与待确认项见[方案 §28](../../FEATURE-2026-11-08-editable-terrain.md#28-开地形场景的主视野尺度2026-11-10)。

机器：Apple M5，macOS，Go 1.27。基准期间负载很重：loadavg 10～18、交换占用约 20GB（`bench-env.txt`、`bench-consecutive-env.txt`），同时在跑别的 agent 的测试与 Unity 构建。耗时只作同一轮里的对照，不作容量结论。

生成工程的做法（下文命令都在生成的工程根目录里跑，`<core>` 是本分支检出的绝对路径）：

```
GOWORK=off go run ./codegen/cmd/roost project new gd -module example.com/gd -out <dir>/gd -template game-demo --skip-deps
# go.mod 追加：replace github.com/tjbdwanghaibo/roost-core => <core>
GOWORK=off go mod tidy
```

## 1. 先红后绿

### 1.1 修前

在改动前的代码（`7da1f62e`）生成的工程里只换上新的 `building_sync_test.go`（新用例只用旧代码已有的符号，不需要编译垫片）：

```
GOWORK=off go test -count=1 -run 'TestBuildingsAtTheTerrainMainViewScale' ./internal/service/game/
```

失败原文见 [red-before.txt](red-before.txt)：`timed out waiting for hut 4×4 seen from the node next to it`——落在“站在建筑旁边看得见它”这条承诺上（旧的主视野进入半径 1.2m，hut 中心离旁边的节点 2.5m）。

### 1.2 修后

同一组用例在改动后的生成工程里通过，日志见 [green.txt](green.txt)（四种建筑在旁边看得见、块预算、挤占与超额降级、字节）。

```
GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -count=1 ./...
GOWORK=off go test -race -count=1 ./internal/service/game/ ./game/scene/... ./game/lifecycle/ ./game/settings/
GOWORK=off go test -race -count=5 -run 'TestBuildings|TestManyBuildings|TestBuildingSync|TestABuilding|TestTerrainScene|TestTerrainMainView|TestMainViewMeasure|SetView' ./internal/service/game/
```

均通过。

## 2. 变异

`mutations.txt`：在生成工程里逐项改实现、跑相关用例、再还原。9 项全部变红：

| 变异 | 变红的用例（首条原文） |
| --- | --- |
| M1 `TerrainConfig` 不放大视野 | `TestBuildingsTakeMainViewPlacesLikeAnyoneElse`：`timed out waiting for the four towers and the monsters in the main view` |
| M2 `TerrainConfig` 仍用 1m 块 | 场景构造被拒：`max leave radius 6300 over block size 100 covers 16384 blocks, budget is 1024` |
| M3 Interest 不读场景的视野 | 场景构造被拒：`view radius range [1500, leave 6300] must contain the default view (enter 120, leave 3300)` |
| M4 去掉滞回（离开 30m） | `stepping back 2m (31～32m, inside the 33m leave radius): player false, monster false, tower false; want all kept` |
| M5 离开上限 60m | `the widest view a client can have: {LOD:0 Radius:5700}, <nil>; want 60m` |
| M6 不开地形的也放大 | `premise: 220 units away is visible with the default view`；`the map without terrain changed` |
| M7 `Normalize` 不补视野 | `a config without a view normalizes to {EnterRadius:0 …}`；地形同步夹具构造被拒 |
| M8 去掉地图面积上限 | `17 × 16 chunks on an ordinary scene: config <nil>, region <nil>; want both refused` |
| M9 进入半径 33m | `a monster 31m away entered the main view: the enter radius is wider than 30m` |

## 3. 基准

`scene_view_scale_test.go` 的 `BenchmarkSceneMainViewScale`（场景与人群见方案 §28.3）。复跑：

```
GOWORK=off go test -c -o <tmp>/scale.test ./internal/service/game/
# 交替：6 轮，每轮六个变体各跑一次、顺序轮换（bench.txt）
for round in 1 2 3 4 5 6; do for v in view30m/block1000 view30m/block1600 view30m/block3200 view60m/block1000 view60m/block1600 view60m/block3200; do
  <tmp>/scale.test -test.run '^$' -test.bench "BenchmarkSceneMainViewScale/$v\$" -test.benchtime 300x -test.count 1 -test.timeout 0
done; done
# 参考：全部九个变体（含 demo 原尺度与 64m 块）-test.count 6 连续跑（bench-consecutive.txt，同一变体连在一起，波动更大）
<tmp>/scale.test -test.run '^$' -test.bench BenchmarkSceneMainViewScale -test.benchtime 200x -test.count 6 -test.timeout 0
benchstat bench.txt
```

`bench.txt` 的 benchstat 中位数：

| 变体 | sec/op | B/op | blocks/obs | heap-MiB | visible/obs | buildings/obs | missed/town-obs | enter-B/s/obs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| view30m/block1000 | 3.51ms ±72% | 1.31Mi | 56.8 | 72.7 | 30.3 | 15.4 | 0.004 | 389 |
| view30m/block1600 | 3.25ms ±24% | 1.10Mi | 25.6 | 69.2 | 30.3 | 15.4 | 0.004 | 389 |
| view30m/block3200 | 3.20ms ±24% | 1.05Mi | 9.1 | 67.4 | 30.3 | 15.4 | 0.004 | 389 |
| view60m/block1000 | 13.7ms ±40% | 2.75Mi | 179.4 | 100.8 | 48.6 | 20.5 | 108.2 | 552 |
| view60m/block1600 | 11.7ms ±54% | 2.16Mi | 76.5 | 94.2 | 48.6 | 20.5 | 108.2 | 552 |
| view60m/block3200 | 12.1ms ±33% | 1.87Mi | 23.6 | 90.3 | 48.6 | 20.5 | 108.2 | 552 |

`bench-consecutive.txt` 里另有：demo-view/block100（原尺度、1m 块、同一张 1024m 图）1.17ms、91.5MiB（含 1m 块的块表约 56MiB）、可见 0.05 个；view30m/block6400 6.06ms、66.7MiB、4.0 块；view60m/block6400 13.0ms、89.3MiB、8.5 块。

读法：`visible/obs`、`buildings/obs` 是 1000 个玩家的平均（镇子里约是两倍减去野外）；`missed/town-obs` 是镇子里每个玩家进入半径以内、却不在主视野里的对象数（被“最近 64 个”挤掉的）；`enter-B/s/obs` 只含进出视野的全量与 remove（基准里移动不改内容，不产生位置增量）；`heap-MiB` 是整个场景（块表、1000 个会话、可见集）在放好人群后的常驻增量。

## 4. 字节

`TestMainViewMeasureSyncBytes`（`green.txt`）：玩家进入视野 205 字节，走一步单独一帧 140 字节、四人同一拍一帧 464 字节；怪三只一帧全量 443 字节、巡逻一步单独一帧 146 字节。都是帧 payload（含帧头），线上另加消息头。建筑全量 132、改参数增量 94（方案 §27.3）。

## 5. 未覆盖

真实 Mongo / NATS、真实进程演练、Unity、跨机与长稳没有跑；分区场景没有放大（方案 §28.6）；基准在重负载机器上，时间的误差带宽。
