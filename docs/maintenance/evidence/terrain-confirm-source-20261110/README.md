# 地形确认来源（就绪标记）证据（2026-11-10）

设计与结论见[可编辑地形方案 §31.9](../../FEATURE-2026-11-08-editable-terrain.md#319-确认来源就绪标记317-③-a2026-11-10)。分支 `feat/terrain-confirm-source`，基于 main `66aecd92`，已合入 `70623709`（同步视图 / codec 校验）。环境：Apple M5、macOS、go1.27.3、.NET 10；机器上同时有另外两个实施任务在跑测试，负载高。生成工程：`roost project new gd -module example.com/gd -template game-demo --skip-deps`，`go.mod` 追加 `replace github.com/tjbdwanghaibo/roost-core => <本工作树>`，`go mod tidy`；下文命令在生成工程根目录或 roost-core 根目录里跑，`GOWORK=off`。不连任何真实数据库 / NATS（持久层是 `terrain_test.go` 的离线替身，同步、编码、TCP 传输、解码是正式代码）。

## 修前红

修前工程 = 用 `66aecd92` 的模板生成，只把两份测试文件（`terrain_sync_test.go`、`terrain_partition_test.go`）换成本分支的版本。新用例只用修前就有的生产入口（`startTerrainSync`、`newPartitionTerrain`），能在修前代码上编译。

`go test -count=1 -run TestTerrainEveryChunkInViewReachesTheClient ./internal/service/game/`：

```
INFO terrain sync started edited_chunks=1 view_radius=20000 max_visible=64 max_objects=164 snapshots_per_flush=4 buildings=0 building_sync=false took=216.833µs
--- FAIL: TestTerrainEveryChunkInViewReachesTheClient (12.95s)
    terrain_sync_test.go:685: the client holds 1 terrain objects, want every one of the 29 chunks in view: no object for (4,7), so it cannot be confirmed
```

`go test -count=1 -run TestTerrainPartitionEveryChunkInViewReachesTheClient ./internal/service/game/`：

```
--- FAIL: TestTerrainPartitionEveryChunkInViewReachesTheClient (5.05s)
    terrain_partition_test.go:292: want a base marker for each of the 11 chunks in view: the base-only process's client holds 0 [], the cell's client 0 []
```

两条都红在承诺本身上：客户端对视野里没改过的块收不到任何对象，分不清“没改过”与“还没到”。

## 修后绿

同两条用例在本分支生成的工程上：

```
INFO terrain sync started edited_chunks=1 base_markers=63 view_radius=20000 max_visible=64 ...
    terrain_sync_test.go:695: entering: 29 terrain objects (1 edited, 28 base markers) in 8 flushes (snapshot budget 4 per flush)
--- PASS: TestTerrainEveryChunkInViewReachesTheClient (3.21s)
--- PASS: TestTerrainPartitionEveryChunkInViewReachesTheClient (0.32s)
```

- 普通场景（8 × 8 块、视野 200m、生产启动步骤）：进场 29 块齐（1 块实体 + 28 个标记，8 次 Flush）；第一次改 (5,4)：同一对象收到实体全量、整段没有 remove；视点移到 128m 外：新进入的 200m 内的块都到、220m 外的都被 remove；断线重连：新会话 29 块齐；`SessionsLost`、`OverLimitDeferred` 都是 0。
- 分区场景（两个格进程 + 只做 Base 的进程，进程内网）：两个客户端都拿到视野里 11 块的标记（Base 进程的全部来自拥有者的“原始块”答复，格进程的来自自己的启动登记、Ghost 带视图和邻格答复）；邻格第一次改 (2,0)：两个客户端都收到全量、没有 remove，与拥有者视图逐点相同。

C#：`dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec` 新增 `TestChunkSyncConfirm`，输出 `PASS terrain confirm source: …`。C# 部分是新代码（`TerrainChunkSync`、`Unconfirm`），没有修前可编译的版本，以下面的变异代替修前红。

## 变异（逐项改坏、只跑相关用例、还原）

Go（在生成工程里直接改生成后的源码）：

| 变异 | 结果 |
| --- | --- |
| S1 启动不登记标记（`showBases` 不调用） | 两条都红：普通 `the client holds 1 terrain objects, want every one of the 29 chunks in view: no object for (5,2)`；分区 `the cell's client 3 [(2,0) (3,0) (3,1)]` |
| S2 标记转实体改为先退役再登记（remove 再 create） | 两条都红：普通 `the first edit of (5,4) did not reach the client (holds version -1)`；分区 `timed out waiting for both clients hold the first edit` |
| S3 分区 `Held` 没有说法时报 0（修前行为） | 分区红：`the base-only process's client holds 0 []` |
| S4 拥有者答原始块时不登记标记 | 分区红：`the base-only process's client holds 0 []` |
| S5 Ghost 带里的原始块不登记标记 | 分区红：格进程的客户端缺 (3,0) (3,1) |

S2 的结果另说明：先退役再登记时，政策并不会把新登记的 subject 重新订阅给已在视野里的玩家（Interest 认为它一直在），对象就此丢失，所以 rebind 不只是省掉一帧的空档。

C#（`TerrainChunkSync.cs` / `TerrainRebuildScheduler.cs`）：

| 变异 | 失败原文 |
| --- | --- |
| M1 `Remove` 不撤销确认 | `FAIL leave: unconfirmed` |
| M2 业务重装后不补回覆盖块 | `FAIL reload: the overlay comes back from the kept record and is confirmed` |
| M3 换包不重装留着的说法 | `FAIL switch: the records on the new pack are confirmed, the old-pack record is not` |
| M4 重连不换调度器会话代际 | `FAIL reconnect: nothing confirmed` |
| M5 标记到达时不把缓存的旧覆盖块换回原始块 | `FAIL cache: the server's base marker puts the base chunk back` |
| M6 不查 BaseRevision | `FAIL accepted a marker on another base revision` |
| M7 视图装入通知里不确认 | `FAIL enter while moving: confirmed once the business loads the base chunk` |
| M8 `Unconfirm` 空操作 | `FAIL leave: unconfirmed` |

## 合并后的验证

见下表，全部在合入 `70623709` 之后的树上。

| 项 | 结果 |
| --- | --- |
| 生成工程 `go build ./...`、`go vet ./...` | 通过 |
| 生成工程 `go test -count=1 ./...` | 全部 ok（`internal/service/game` 75s） |
| 生成工程 `-race`（`Terrain|Partition|Building`，`internal/service/game`、`game/entities/terrain_chunk`） | 通过（`internal/service/game` 56s、`terrain_chunk` 6s，无 DATA RACE） |
| 生成工程 `go generate ./...` 后 porcelain | 干净 |
| roost-core `gofmt`、`go build ./...`、`go vet`（改动包） | 通过（gofmt 无输出；build、vet `gameplay/terrain`、`framework/sync/...`、`codegen/...` 无输出） |
| `go test ./gameplay/terrain` | ok |
| 根包 `go test -count=1 .` | ok（第一次跑时证据文件还没写，文档链接检查红；补上证据后 ok） |
| `go test ./codegen/internal/roost` | ok（412s） |
| C# `Roost.Client.Terrain.Tests -- client/spec` | 通过（`PASS terrain`） |
| C# `Roost.Client.Tests -- client/spec/packets.json` | 通过（34 项 PASS） |
| 建筑夹具 `Fixtures/regen.sh -update` 后核对模式 | 干净；旧 C# 用例在新夹具上红（`FAIL params_delta: identity`），改后通过 |
| 文档锚点 `scripts/check-doc-anchors.py` | 271 个文件、0 处失效 |

## 没有跑的

- 真实依赖（`scripts/mirror-local.sh`）与无缝演练（`ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`）：按安排在合入时跑。
- C# 没有用真实 entitysync 帧驱动 `EntityViews` → `TerrainChunkSync`（帧内容由 Go TCP 客户端核对），Unity 未跑。
- 没有性能基准：标记的 AOI 与内存开销只按结构估算（每块一个 `SubjectSyncState`，2048m 图 1024 块约几百 KB），没有测量。
