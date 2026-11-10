# 建筑订阅改为场景主视野（T07，方案 §27.10）证据

分支 `feat/building-main-interest`，基于 main `21f631e1`。设计、其他实体的审视与待确认项见[方案 §27.10](../../FEATURE-2026-11-08-editable-terrain.md#2710-订阅改为场景主视野维护者-2026-11-10)。机器：Apple M5（macOS），本机有后台负载；本页的数字都是功能用例里的计数与字节数，不是性能结论。

生成工程的做法（下文所有命令都在生成的工程根目录里跑，`<core>` 是本分支检出的绝对路径）：

```
GOWORK=off go run ./codegen/cmd/roost project new gd -module example.com/gd -out <dir>/gd -template game-demo --skip-deps
# go.mod 追加：replace github.com/tjbdwanghaibo/roost-core => <core>
GOWORK=off go mod tidy
```

## 1. 先红后绿

### 1.1 修前

在改动前的代码（`21f631e1`）生成的工程里，把新的 `building_sync_test.go` 放进去，只加编译垫片（每处标 `SHIM`）：harness 用旧构造（`defaultTerrainSync(bounds, true)` + `terrain.attach`）、`buildingSync()` 取旧字段 `scene.terrain.buildings`、`terrainNamespaceLimits` 用旧签名、`sceneTypeSchedules` 逐字照抄旧代码里内联的那份调度（地形块 periodic 1s + 建筑 on_change）。命令：

```
GOWORK=off go test -count=1 -run 'Building' -v ./internal/service/game/
```

6 个用例红，原文见 [red-before.txt](red-before.txt)：

| 用例 | 修前失败原文 | 落在哪条承诺上 |
| --- | --- | --- |
| `TestBuildingsReachAnotherClientOverRealTCP` | `timed out waiting for the hall leaving the main view 3m away` | 建筑跟着主视野走：3m 外（主视野离开半径 1.5m）消失；旧代码的 200m 建筑视野不让它走 |
| `TestBuildingsHaveNoReplicationOfTheirOwn` | `the scene manager's namespace limits: map[building:{MaxObjects:128 …} terrain:{…}], want only "terrain"` | 没有建筑专用的命名空间一类 |
| `TestBuildingsTakeMainViewPlacesLikeAnyoneElse` | `the neighbour 2.35m away is still held: the main view holds 66 objects` | 建筑与玩家、怪共享“最近 64 个” |
| `TestManyBuildingsDegradeWithTheCrowd` | `timed out waiting for the viewer's session settled at its limit` | 建筑与玩家、怪同在会话的 100 一类，超额一起降级 |
| `TestBuildingsAtTheDemoMainViewScale` | `hut 4×4 next to it: default view sees it true (want false), widest view true (want true)` | 建筑由主视野（1.2m）决定，不是 200m 视野 |
| `TestBuildingSyncAfterARestart` | `timed out waiting for a restarted building on a client` | 看的人站在一座建筑上只持有这一座（旧代码同时持有两座） |

`TestABuildingPlacedAgainBeforeItsRemoveWentOut` 在旧代码上通过：旧代码拆除时不退役 subject，没有这个窗口；它守的是本次改动引入的新路径（见 1.3 的 M1）。

### 1.2 修后

同一组用例在本分支生成的工程上全绿（`go test -count=1 -run 'Building' -v ./internal/service/game/`）。测量日志（摘）：

```
building full: component 132 bytes (payload 53); parameter delta: component 94 bytes (payload 15)
main view full: 4 buildings, 60 monsters, neighbour held false (objects held 65: itself + 64 in the main view)
a tower removed, nobody moved: neighbour held false
ten monsters gone, the viewer standing still: 0 of 4 towers held (free places 10)
over the limit: deferred 2, evicted 0; held 100 objects, 2 buildings, 97 monsters
hut 4×4: centre 2.5m from the node next to it; default view (1.2m) sees it false, widest view (2.7m) sees it true
tower 3×3: centre 2.0m from the node next to it; default view (1.2m) sees it false, widest view (2.7m) sees it true
hall 8×6: centre 3.5m from the node next to it; default view (1.2m) sees it false, widest view (2.7m) sees it false
keep 12×12: centre 6.5m from the node next to it; default view (1.2m) sees it false, widest view (2.7m) sees it false
```

“站着不动时被忽略的建筑不进来”“拆一座 tower、没人移动时邻居不进来”两条只记日志、不作断言（AOI `MaxVisible` 的近似语义，见方案 §27.10 待确认 D）；用例断言的是移动之后进来。

### 1.3 变异

在本分支生成的工程里改生成的文件，跑 `go test -count=1 -run 'Building' ./internal/service/game/`，记录后还原。原文见 [mutations.txt](mutations.txt)。

| 变异 | 结果（失败原文摘一行） |
| --- | --- |
| M1 `ShowSubject` 用普通 `Register`（不排在退役之后） | 红：`TestABuildingPlacedAgainBeforeItsRemoveWentOut` `timed out waiting for the tower placed again on the client`（日志 `scene: register subject …: entitysync: subject is retiring`） |
| M2 放置提交后不 `ShowSubject` | 红：`timed out waiting for the hall on the viewer's client` 等 6 个用例 |
| M3 拆除提交后不 `HideSubject` | 红：`timed out waiting for the removal on the viewer's client` 等 4 个用例 |
| M4 不按修订号丢迟到通知 | 红：`TestBuildingSyncTakesTheLatestRevision` 与 TCP 用例 |
| M5 启动不登记站着的建筑 | 红：`0 buildings registered for replication after the restart, want 2` |
| M6 `Namespaces` 加回 `building` 128 | 红：配置用例（`want only "terrain"`）与超额降级用例（等不到会话超额） |
| M7 `TypeSchedules` 加回建筑条目 | 红：`the scene manager's type schedules: map[11:{…} 13:{Mode:on_change …}], want only the terrain chunk` |
| M8 主视野位置用锚点而不是占地中心 | 红：`timed out waiting for the hall on the viewer's client` 等 6 个用例 |
| M9 修订号判定之后在锁外 Show / Hide | **不变红**：这是结构性守卫（并发的两个提交后回调倒序生效），没有构造出确定的并发红用例；保证与限制见方案 §27.10 |

## 2. 验证命令与结果

| 检查 | 命令 | 结果 |
| --- | --- | --- |
| 根仓构建 / 根包 | `GOWORK=off go build ./...`；`GOWORK=off go test -count=1 .` | 通过 |
| 生成器 | `GOWORK=off go vet ./codegen/...`；`GOWORK=off go test -count=1 -timeout 40m ./codegen/internal/roost/` | 通过（292s）。此前一次与多组 `-race` 并跑时，内含的生成工程用例 `TestACrowdedSpawnPointKeepsEveryReplicationSessionOpen` 红过一次（`player 20087 does not hold its own entity`，只跑 3 次 Flush 的时序用例，走的是玩家 Join 路径，与本次改动无关）；改动前后的工程各单独 `-race -count=30 -cpu 1,4` 都不复现，单独重跑整包通过 |
| 文档锚点 | `python3 scripts/check-doc-anchors.py` | `checked 262 files, 0 broken` |
| 生成 game-demo 全量 | `go build ./... && go vet ./... && go test -count=1 ./...` | 通过 |
| 场景 / 建筑同步包 `-race` | `go test -race -count=1 ./internal/service/game/ ./game/terrainmap/ ./game/entities/building/` | 通过（最后一次）；更早一次整包 `-race` 遇到 §3 的偶发 |
| ID 与生成物漂移 | `roost id check`；`roost generate --check` | `IDs are valid`；`generated files are up to date` |
| C# | 不涉及：DAO、同步编码、协议与 C# 代码都不变（只改了 `client/README.md` 的引用表数字），没有跑 `Roost.Client.Terrain.Tests` / `Roost.Client.Tests` | — |
| 真实 Mongo | `TestBuildingRestartRealMongo` 仍按 `ROOST_TERRAIN_IT_MONGO_URI` 门控，本轮没有运行 | — |

## 3. 偶发：第二次改参数以全量发出（尚未确认）

`TestBuildingsReachAnotherClientOverRealTCP` 在重负载下偶发：

```
building_sync_test.go:345: the second parameter change went out as [{subject:6711000118 full:true componentBytes:132 payloadBytes:53 params:true}], want one delta carrying the parameters; first change [{subject:6711000118 full:false componentBytes:94 payloadBytes:15 params:true}]; full update {subject:6711000118 full:true componentBytes:132 payloadBytes:53 params:true}
```

（这一行带的“first change / full update”是临时加的诊断输出，没有提交。）第一次改参数是增量、第二次是重同步全量：§27.3 末条记的“放置时的脏位让登记后第一次改动走全量”挪到了第二次改动上。客户端内容与服务端一致（之前的 `buildingMatches` 都通过），只是编码多 38 字节。

| 代码 | 条件 | 失败 / 次数 |
| --- | --- | --- |
| 本分支 | 整包 `go test -race ./internal/service/game/`，同时在跑生成器测试 | 1 / 1 |
| 本分支 | `-race -count=25 -cpu 1,2 -run TestBuildingsReachAnotherClientOverRealTCP`，同时在跑生成器测试 | 1 / 50 |
| 本分支 | 编好的 `-race` 测试二进制 `-test.count=200 -test.cpu 1,2`，同时跑整包 `-race` | 1 / 400 |
| 本分支 | 单独跑（`-race` `-cpu 1`、`-cpu 1,2`、不带 `-race`） | 0 / 380 |
| 改动前（`21f631e1`，旧用例） | 同样的并跑条件 | 0 / 400 |
| 改动前 | 单独跑 | 0 / 200 |

没有定位到是哪一次快照让脏位保留（entitysync 的 `preserveDirty`：快照推进了版本、而老订阅者还没消费的变化只能用全量追上）；可能与主视野那个 Interest 的结算时机有关。断言没有放宽，登记在 KNOWN-LIMITS（尚未确认）。

**结论（2026-11-10 追加）**：已定位并修复，不是主视野的结算时机，也不是 `preserveDirty`：on_change 冻结在同一 subject 的捕获在途时一律升成全量，并在在途捕获提交后留下一份过时冻结，下一次改动因“已有冻结”再升成全量。见 [entitysync 冻结升全量](../entitysync-resync-flake-20261110/README.md)。

## 4. 未覆盖

- 真实 Mongo、真实进程演练、Unity 表现（同方案 §27.8）。
- M9 的并发倒序没有红用例。
- demo 尺度之外的主视野参数（方案 §27.10 待确认 A～D）没有测；本轮不改主视野参数。
