# 可编辑地形 P4：水与建筑（T07 服务端）证据

分支 `feat/terrain-p4`，基于 main `960e485c`。设计与规则见[方案 §25](../../FEATURE-2026-11-08-editable-terrain.md#25-p4-水与建筑实施结果2026-11-10)，进度见 [TERRAIN-STATUS](../../TERRAIN-STATUS.md) T07。本机 macOS，Go 1.27；没有连接任何真实数据库，没有启动真实依赖；带门变量的真实 Mongo 用例跳过，不计为通过。

## 1. 先红后绿

新功能没有“修前”版本，红证据都来自变异：把实现里的一条规则去掉或改错，跑同一组测试，记录失败原文，然后还原。三份原文在本目录：

| 文件 | 内容 |
| --- | --- |
| [mutations-water.txt](mutations-water.txt) | `gameplay/terrain` 水位区域 7 项（W1 查询忽略区域、W2 行走忽略区域、W3 `StandZ` 用全局水位、W4 不查区域重叠、W5 校验和不覆盖区域表、W6 等于水位算水、W7 允许区域出图）。第一轮 W1 的写法没编译过、W5 的断言落在金样上，第二轮重写后重跑，两轮都在文件里 |
| [mutations-water-csharp.txt](mutations-water-csharp.txt) | C# 2 项（C1 查询忽略区域：`FAIL query (0, 6400): got 74105/4/True/74265, Go 74105/4/False/-2147483648`；C2 不读区域表：`FAIL golden water region count`） |
| [mutations-building.txt](mutations-building.txt) | 生成的 game-demo 里的建筑 14 项，见下 |

建筑变异（`go test -count=1 -run 'Building|Walkers|Brush' ./internal/service/game/`，生成工程里改生成的文件，跑完还原）：

| 变异 | 结果（失败原文摘一行） |
| --- | --- |
| B1 放置不在锁内查块的占地 | 红：`a lot overlapping one node: <nil>`；并发用例 `buildings [46,50)×[10,14) and [47,51)×[12,16) overlap` |
| B2 重叠检查改读进程内发布表 | **没变红**（5 轮、`-race` 3 轮）：内存提交器下提交与发布之间只有微秒。保证与限制见方案 §25.4 |
| B3 放置的水位用全局 | 红：`under the regional water level: <nil>`；换版用例 `Unfit:0`，want 1 |
| B4 `Walker` 不看建筑 | 红：`the route still crosses the building` |
| B5 笔刷不看建筑 | 红：`a brush under the hut: <nil>` |
| B6 `Load` 不发布块的占地 | 红：重启对账 `{Standing:0 ...}` |
| B7 换版不重新整平 | 红：`node (60, 20) at 52000 cm on the new base, want the old foundation 50010` |
| B8 拆除不查属主 | 红：`removing another player's building: <nil>` |
| B9 账本不计建筑数 | 红：`player 4242's ledger counts 0 buildings, 1 stand` |
| B10 坡度上限总用 `MaxCut` | 红：`a hall across a 70cm slope, not levelled: <nil>` |
| B11 换版不判不合规 | 红：`Unfit:0`，want 1 |
| B12 丢弃旧地形时清掉占地 | 红：`{Standing:3 Rebased:1 Unfit:1 Mismatched:2}` |
| B13 不查上限 | 红：`a fourth building with a limit of 3: <nil>`；重启后 `the limit after the restart: <nil>` |
| B14 放置不记块的占地 | 红：`chunk (0,0) does not record the lot of building [10,14)×[10,14)` |

B2 / B14 第一次的写法没编译过（变量未使用），重写后的结果在原文末尾。

## 2. 验证命令与结果

仓库根（worktree），`GOWORK=off`：

| 命令 | 结果 |
| --- | --- |
| `gofmt -l .` | 无输出 |
| `go build ./...`、`go vet ./...` | 通过 |
| `go test -race -count=1 ./gameplay/terrain ./gameplay/movement ./gameplay/cellworld` | 通过 |
| `go test -count=1 ./codegen/internal/roost/` | 通过 |
| `go test -count=1 .`（根包，含 `integration_coverage_test.go` 的真实依赖登记） | 通过 |
| `python3 scripts/check-doc-anchors.py` | 通过 |
| `dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec` | 通过（`PASS terrain`，含区域水位金样与坏包） |
| `sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh`（核对模式） | `terrain fixture: up to date`（块上新加的 `Lots` / `LotRevision` 只持久不同步，同步夹具不变） |

生成工程：`go run ./codegen/cmd/roost project new gd -module example.com/gd -out <临时目录>/gd -template game-demo --skip-deps`，`go.mod` 追加 `replace github.com/tjbdwanghaibo/roost-core => <worktree>`：

| 命令 | 结果 |
| --- | --- |
| `go mod tidy && go build ./... && go vet ./... && go test -count=1 ./...` | 通过（35 个包 ok） |
| `go test -race -count=1 -run 'Building\|Walkers\|Brush\|Terrain' ./internal/service/game/ ./game/controllers/player/ ./game/entities/... ./cmd/terrainpack` | 通过 |
| `go test -race -count=3 -run TestBuildingConcurrent ./internal/service/game/` | 通过：每轮 20～21 座站着、171～172 次 `terrain_occupied` |
| `roost id check` / `roost generate --check` | `IDs are valid` / `generated files are up to date` |
| `TestBuildingRestartRealMongo`、`TestTerrainRestartRealMongo`、`TestTerrainEditLedgerRealMongo` | SKIP（`ROOST_TERRAIN_IT_MONGO_URI` 未设置；本轮不连真实库） |

## 3. demo 缺省地形改为平地（方案 §25.6）

[default-flat.txt](default-flat.txt)：新的 `cmd/terrainpack` 用例跑在改动前的 `main.go` 上的失败原文（缺省 `BaseRevision:1`、没有 `-ground` / `-shape`、新的参数拒绝没拒绝），以及 2 × 1 块缺省地图上向东一步走得通的边：原缺省（`-shape generated -seed 1`）3700 / 8128 = 45.5%，新缺省平地 8128 / 8128。第 3 部分是维护者补充把缺省尺寸改成 16 × 16 块之后的包大小（18,560 字节）、写包与装入时间、视图内存。改动后 `go test -count=1 ./cmd/terrainpack`（生成工程）与 `go test -count=1 ./gameplay/terrain`（含 `TestFlatIsTheSameEverywhereAndWalkable`）通过。

## 4. 未覆盖

- 真实 Mongo / WAL 上的建筑重启与换版（用例已写、已挂进 `scripts/test-demo-generated.sh`，没有运行）。
- 真实进程演练（`startTerrain` 的新接线：普通场景接建筑、分区场景不接）。
- 建筑同步、分区场景与跨格建筑、客户端岸线与建筑表现。
- 性能：锁内按占地现算占位位图、占位表发布的成本没有测。
