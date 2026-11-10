# 建筑实体化与同步（T07，方案 §27）证据

分支 `feat/terrain-building-entity`，基于 main `2709660f`，提交前合并到 main `f7c8e931`（快进，cellworld 千分位零头）。设计与规则见[方案 §27](../../FEATURE-2026-11-08-editable-terrain.md#27-建筑实体化与同步2026-11-10)，进度见 [TERRAIN-STATUS](../../TERRAIN-STATUS.md) T07。本机 macOS，Go 1.27；没有连接任何真实数据库，没有启动真实依赖；带门变量的真实 Mongo 用例跳过，不计为通过。

## 1. 先红后绿

### 1.1 修前

在 `2709660f` 生成的 game-demo 里加一条临时用例：一个玩家放建筑，另一个玩家（视野内、走真实 TCP）等着在自己的客户端上看到它。旧代码里建筑没有同步，原文见 [red-before.txt](red-before.txt)：

```
--- FAIL: TestRedAnotherClientSeesAPlacedBuilding (10.03s)
    zz_building_red_test.go:19: timed out waiting for the building on the watcher's client
```

这条临时用例用的是旧 API（`Placement{Kind}`），新代码里由 `TestBuildingsReachAnotherClientOverRealTCP` 取代（同一承诺：另一个客户端看到出现、改参数、拆除）。

### 1.2 变异

在生成工程里改生成的文件，跑 `go test -count=1 -run 'Building' ./internal/service/game/`，记录失败原文后还原。原文见 [mutations.txt](mutations.txt)。

| 变异 | 结果（失败原文摘一行） |
| --- | --- |
| M1 建筑同步不登记（`attach` 不接 `SetBuildingCommitted`） | 红：`timed out waiting for the hall on the viewer's client` |
| M2 不按修订号丢迟到通知 | 红：`a late removal notice hid a standing building` |
| M3 启动不登记站着的建筑 | 红：`0 buildings registered for replication after the restart, want 2` |
| M4 改参数不查属主 | 红：`another player: <nil>, want terrain: edit not allowed here` |
| M5 参数不查范围 | 红：`a value above its max: <nil>, want building: parameters refused` |
| M5b 参数不查个数 | 红：`four values for three slots: <nil>, want building: parameters refused` |
| M6 热更新不查删配置 / 改宽高 | 红：`a reload with the hut's width was accepted` |
| M7 不查参数槽连续 | 红：`start with a slot gap: <nil>, want ErrCatalog` |
| M8 对账不查配置 | 红：`{... Misconfigured:0}, want {... Misconfigured:2}` |
| M9 拆除不撤出视野 | 红：`timed out waiting for the removal on the viewer's client` |
| M10 改参数不记账本（不判重） | 红：`the same change again: {Duplicate:false ...}` |
| M11 建筑视野不跟着视点走 | 红：`timed out waiting for the view moving to the far corner` |
| M12 必须整平不查（事务前与事务里两处） | 红：`a keep that must be levelled, not levelled: <nil>` |
| M13 `SiteComponent.SetParams` 不调 `PublishSyncDirty` | **没变红**，不是有效变异：生产接线装了 Nest 的提交观察者，提交准入时按事务条目标脏；`PublishSyncDirty` 只在没有观察者的 periodic Manager 里起作用（与 `TerrainChunk` 同一做法） |
| C# 生成的 `dao_sync_apply.cs` 去掉 `Params` 的合并一行 | 红：`FAIL place_full: C# Apply equals the Go DAO` |

## 2. 实测字节

`TestBuildingsReachAnotherClientOverRealTCP` 记录（真实 TCP，生产的帧编码）：

```
building full: component 132 bytes (payload 53); parameter delta: component 94 bytes (payload 15)
```

用例断言全量不超过 160、增量不超过 112。每观察者预算的推算见方案 §27.3：视野满 128 座约 17KB、走过密集街区约 5.3KB/s，远低于 50KB/s。

注意：放置之后的**第一次**改参数可能以全量（原因 Resync）发出——放置事务的脏位在登记前已被捕获（entitysync 的 preserveDirty），之后的改动都是增量。用例只要求第二次改动是增量；见方案 §27.3 / §27.8。（更正 2026-11-10：原因是 on_change 冻结的“忙时升全量”，不是 preserveDirty；已修复，用例改为两次都要求增量，见 [entitysync 冻结升全量](../entitysync-resync-flake-20261110/README.md)。）

## 3. 验证命令与结果

仓库根（worktree），`GOWORK=off`：

| 命令 | 结果 |
| --- | --- |
| `gofmt -l codegen client/dotnet/Roost.Client.Terrain.Tests/Fixtures` | 无输出 |
| `go vet ./codegen/...` | 通过 |
| `go test -count=1 ./codegen/internal/roost/` | 通过（309.7s） |
| `go test -count=1 .`（根包） | 通过 |
| `python3 scripts/check-doc-anchors.py` | 通过 |
| `dotnet run --project dotnet/Roost.Client.Terrain.Tests -- spec`（在 `client/` 下） | 通过（含 `PASS building DAO sync`：放置全量、只带参数的增量、换成无槽配置、拆除四步） |
| `dotnet run --project dotnet/Roost.Client.Tests -- spec/packets.json`（在 `client/` 下） | 通过 |
| `sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh`（核对模式） | 地形与建筑夹具均 up to date |

生成工程：`go run ./codegen/cmd/roost project new gd -module example.com/gd -out <临时目录>/gd -template game-demo --skip-deps`，`go.mod` 追加 `replace github.com/tjbdwanghaibo/roost-core => <worktree>`（合并 `f7c8e931` 之后重新生成）：

| 命令 | 结果 |
| --- | --- |
| `go mod tidy && go build ./... && go vet ./... && go test -count=1 ./...` | 通过（35 个包 ok） |
| `gofmt -l .` | 无输出 |
| `go test -race -count=1 -run 'Building\|TerrainChunks\|TerrainMixed\|Walkers\|Brush' ./internal/service/game/ ./game/controllers/player/ ./game/entities/...` | 通过 |
| `roost id check` / `roost generate --check` | `IDs are valid` / `generated files are up to date` |
| `TestBuildingRestartRealMongo`（已加改参数与重启后参数核对）、`TestTerrainRestartRealMongo`、`TestTerrainEditLedgerRealMongo` | SKIP（`ROOST_TERRAIN_IT_MONGO_URI` 未设置；本轮不连真实库） |

## 4. 未覆盖

- 真实 Mongo 用例已写未运行；没有真实进程演练（Gate + game 进程 + 机器人）。
- Unity 表现：只验证了 C# 生成 Apply 与 Go 逐字节一致，没有接预制体实例化。
- 分区场景仍拒绝放置；§27.6 只写了接同步的设计。
- 视野半径 200m、上限 128 是估算值，没有目标容量下的压测。
- 热更新收窄参数范围时，已存的越界值保留（读时不夹、改参数时整组重新校验），见 §27.8；规则取舍是 §27.9 ④。
- World 类实体假定常驻（站着的建筑在场景存活期内一直是 subject）；建筑数量很大时的内存与登记开销未测。
