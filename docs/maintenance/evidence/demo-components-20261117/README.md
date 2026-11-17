# game-demo 自有组件迁到组件包写法（2026-11-17）

维护者 2026-11-17 决定把 game-demo 自己的组件从实体包写法迁到组件包写法（同模块）。只做结构迁移，不新增事件，行为不变。

## 迁移结果

| 实体 | 组件（view 名） | 组件包 | 类型号（不变） |
| --- | --- | --- | --- |
| Player | Map | `game/components/mapview` | 2004 |
| Player | DungeonLedger | `game/components/dungeonledger` | 2103 |
| Monster | MonsterBody | `game/components/monsterbody` | 2100 |
| Monster | MonsterActor | `game/components/monsteractor` | 2101 |
| World | Stats | `game/components/stats` | 2002 |
| World | ActivityPhase | `game/components/activityphase` | 2102 |
| TerrainChunk | TerrainSurface | `game/components/terrainsurface` | 2011 |
| TerrainEditor | TerrainLedger | `game/components/terrainledger` | 2012 |
| TerrainCellEditor | TerrainCellLedger | `game/components/terraincellledger` | 2014 |
| Building | BuildingSite | `game/components/buildingsite` | 2013 |

不迁：World 的 `timer_component.go`（2006），实体定时器宿主，按“实体包只留接线”留在实体包。

`CompType<X>` 的 X 取 view 里已有的 Holder 名（`MonsterBody`、`TerrainSurface`……），生成器因此自己写出访问器 `<X>()`，实体包里原来手写的
`MonsterBody()` / `TerrainLedger()` 等访问器随组件文件一起删除；具体类型的取值方法随之改名（`BodyComp()` → `MonsterBodyComp()` 等，
`MapComp()` / `StatsComp()` / `ActivityPhaseComp()` / `DungeonLedgerComp()` 名字不变）。

## 验证（在 scratchpad 生成，`--skip-deps` + `go mod edit -replace` 指向本检出）

```
go run ./codegen/cmd/roost project new planet -module example.com/planet -out <scratch>/after -template game-demo --skip-deps
go mod edit -replace github.com/tjbdwanghaibo/roost-core=<roost-core>; go mod tidy
go build ./... && go vet ./...          # 通过
roost generate --check                  # generated files are up to date
roost id check                          # IDs are valid
go test -count=1 -timeout 30m ./...     # 57 个包 ok，0 FAIL（internal/service/game 86s，game/handler、各实体包、组件包都过）
```

迁移前后两个生成工程逐文件对比：除本次改动的文件外，差异只有 room 模块（同批另一组的修复，与本次无关）。生成的 Nest 分派只换了断言的接口类型
（例如 `player.IDungeonClaimEntity` → `view.PlayerEntity`、`terrain_chunk.ISurfaceEntity` → `view.ITerrainSurfaceEntity`），handler 名与
Sender 不变；Player / Monster 等的组件创建顺序与类型号不变；`view.MonsterEntity` 不再嵌入 `roost add component` 留下的空 `ActorHolder` /
`BodyHolder`，`view.IActorEntity` / `IBodyEntity` 随之消失（没有使用者）。

codegen 只跑 demo 的便宜用例：

```
go test -count=1 -run 'TestDemo(EmbedCoversEveryFile|TemplateStepsAndShippedFilesAgree|StepsLeaveModuleComponentsToTheModules|ViewInterfacesListTheComponentMethods|ViewNamesDoNotCollideWithItsModules|EntityPackagesHoldOnlyComponentWiring|LeavesMailToTheMailboxModule|ActorConsumesOneBodyAndPreservesMovementTransactions)$' ./codegen/internal/roost/
ok
```

守卫变异：把旧的 `demo/game/entities/world/stats_component.go.tmpl` 原样放回，`TestDemoEntityPackagesHoldOnlyComponentWiring` 变红：

```
demo_test.go:306: game/entities/world/stats_component.go.tmpl declares //roost:component type=2002 in the entity package; ...
demo_test.go:306: game/entities/world/stats_component.go.tmpl keeps the alias StatsComponent; ...
demo_test.go:306: game/entities/world/stats_component.go.tmpl declares IStatsEntity in the entity package; ...
FAIL
```

未跑：`TestDemoTemplateGeneratesABuildableWritePath`（TestDemo 整体，新增了几条断言）与 codegen 全量，留给主线最后统一跑。
