# R3：地形 DAO 按地图归属选库、换服时建筑与全服邮件的归属（2026-11-17）

维护者 2026-11-14 决定 D11 = Q17 A + N3 A（[decisions.md](../known-limits-remaining-20261114/decisions.md#d11-q17-地形-dao-的库范围--n3-换服后建筑与全服邮件归属一起定)）：

- **Q17 A**：地形 DAO 按“地图归属”选库范围——普通场景按 sid，分区场景（跨服共享地图）保持 global，demo 与 terrain 模块一致。
- **N3 A**：换服时建筑留在原地图（归地图所在的服，跟 Q17）；全服邮件离开前按旧范围拉一次，到新服按新范围拉。

这是功能实施：每项新行为有回归用例，修前状态与变异都跑红（见下）。

## 一、库范围在哪里声明、选哪种表达

查到的事实：

- 库范围是 DAO 标记 `//roost:dao ... dbscope=sid|global`（`codegen/internal/dao/parse.go`），生成成 DAO 的常量方法 `DbScope()`（`template_dao.go`）。DataEngine 不自己判断：提交记录里每条改动的文档键带 `Key.Scope = d.DbScope()`，WAL 记下它、投影按它选库（`<物理库>_<sid>` 或 `<物理库>`，`framework/dataengine/engine/mongo_store.go`）；冷加载、`ResolveDatabaseScope`、预加载 / 搬档 / Remote 的校验也都是在运行期调 `DbScope()`。
- terrain 模块的 `terrain_chunk` / `terrain_building` 已是 `dbscope=sid`。模块的图只在一个进程里（README：“不做跨进程 / 分区地图的编辑”），归本服——已经符合 Q17 A，代码不用改。
- game-demo 的地形是同一套实体与 DAO（`TerrainChunk` kind 11、`Building` kind 13、`TerrainEditor` kind 12），**同一个二进制按配置 `scene.partition.enabled` 跑普通场景或分区场景**。分区场景的块必须共用：格进程启动时按库里的记录装入邻格 Ghost 带的块，格到服的映射可以改。所以同一个 DAO 类型要在不同进程里取不同的范围。

选项与取舍：

1. **分区地图用单独的 DAO / 集合**：DAO 类型绑在实体字段上（`dao *db.TerrainChunkDao`），要另开三个实体 kind、三套生成的 handler / Sender / lifecycle，`terrainmap` 全部改成接口——量大、重复。不选。
2. **逻辑库映射（`WithDatabaseResolver`）**：把地形放进单独的逻辑库，声明 global，普通场景时把物理库映射成 `<库>_<sid>`。生成工程没有注入 resolver 的位置、回调拿不到配置；而且“声明 global、实际按服”让 sidmerge / 搬档 / 运维工具都看错。不选。
3. **同一个 DAO 类型按进程选范围（选这个）**：生成器加 `dbscope=boot`——`DbScope()` 读一个包级 `atomic.Bool`，缺省按服分库（与“本地实体就是 sid”的缺省一致），起服装配在本进程第一次读写之前调用生成的 `db.Set<Dao>DbScope(dataengine.DatabaseGlobal)` 改共用。DataEngine 一行不改（它本来就在运行期读 `DbScope()`，WAL 记下写入时的范围）；在 demo 里，范围由 `terrainmap.Open` 按新增的 `Region.Shared` 选，`Region` 由 `terrainRegion`（`startTerrain` 用的同一个函数）给出：不分区 → 本服的图 → sid；分区（含只做 Base 的服）→ 共享的图 → global。一个进程一张 Map，Open 在任何装入与编辑之前。

   代价：范围是进程级的全局状态，同一个测试二进制里先后开普通 / 分区两种图时每次 Open 重新选（用例就是这样跑的，见 `TestTerrainDocumentsFollowTheMapOwnership` 的三段）；进程运行中不能改（改了等于把数据留在另一个库里），同一份部署从普通改分区等于搬数据，与改逻辑库映射相同。

`terrain_editors` 也跟着地图：账本记的是“这个玩家在这张图上应用过哪些编辑序号、站着几座建筑”，建筑留在原地图（N3 A），账本留在原服才对得上。分区场景里玩家在格上的账本 `terrain_cell_editors` 不变（始终 sid）。分区场景的服本来就拒绝换服（`rehome_checks.go`），所以“换服”只发生在普通场景。

## 二、改法

生成器（`codegen/internal/dao`）：

- `parse.go` / `gen.go`：`dbscope` 接受 `boot`；`databaseScopeExpr` 对 boot 生成 `<dao>DbScope()`。
- `template_dao.go`：boot 的 DAO 另生成 `var <dao>DbScopeGlobal atomic.Bool`、`Set<Dao>DbScope(scope)`、`<dao>DbScope()`（条件 import `sync/atomic`）。
- `databases.go`：boot 的逻辑库按按服算库名长度（`ServerScoped`）。
- 夹具 `testdata/def/realm.go`（`RealmDao dbscope=boot`）、golden 更新、运行档 `testdata/runtime/boot_scope_test.go`；`parse_test.go` / `failfast_test.go` 跟上；文档 `codegen/docs/TAGS.zh-CN.md`、`help.go`。

game-demo（`demo/`）：

- `db/def/{terrain_chunk,building,terrain_editor}.go.tmpl`：`dbscope=boot`，注释写明归属。
- `game/terrainmap/terrainmap.go.tmpl`：`Region.Shared`、`DatabaseScope(shared)`、`Open` 里 `useDatabaseScope`。
- `internal/service/game/terrain.go.tmpl`：`terrainRegion` 分区时（含只做 Base）`Shared: true`；删 `terrainEditorOf`。
- `internal/service/game/{playerowner,service}.go.tmpl`：`DepartPlayer` 只交出 Player，删 `playerEvictor.playerScoped`（账本不再随玩家交出——两个服各写各的库，旧副本写回旧版本的那个冲突不存在了）。
- 用例：`terrain_test.go.tmpl`（`TestTerrainDocumentsFollowTheMapOwnership` 离线；`TestTerrainMapOwnershipRealMongo` 真实 Mongo，替换原来“账本随玩家走”的换进程子用例；真实 Mongo 进程带 sid，读 `<物理库>_<sid>`）、`terrain_building_test.go.tmpl`（`buildingProcessIn`）、`terrain_wal_crash_test.go.tmpl`（库名换算）。

mailbox 模块（N3 的全服邮件）：

- 新联动 `modules/mailbox/with/rehome/game/controllers/mailbox_rehome.go.tmpl`：`gamerehome.OnLeaving("mailbox", ...)` 调 mail 的 `Summary`——mail 服务按记着的画像（他在本服进入时给的，范围是本服）合并全服邮件、水位推进到此刻；到了新服由已有的 `with/enter` 进入钩子按会话里新的承载服重新拉（account 改绑后 `server_id` / `origin_server_id` 都是新服）。合并失败返回错误、中止换服（离开钩子的约定）；Registry 里没有 mail 能力时跳过（起来的 game 进程里 mailbox 的启动检查保证它在，没有它只会是只装换服那几样的测试 Registry——不跳过时 rehome 自己的模块场景全部中止）。
- 没有改 rehome 模块的代码（R2 的范围不动），只在 README 的联动一节加一行。

## 三、用例、修前与变异

在 scratchpad 生成 game-demo（`roost project new planet -module example.com/planet -template game-demo --skip-deps`，`go mod edit -replace` 指向本检出），在生成工程里改生成物做修前 / 变异，每次之后 `git checkout` 还原。全部输出见 [mutations.txt](mutations.txt)。

| 编号 | 改动 | 跑的用例 | 结果 |
| --- | --- | --- | --- |
| M0 修前 | 三种地形 DAO 的 `DbScope()` 改回 global（改动前的范围） | `TestTerrainDocumentsFollowTheMapOwnership`、`TestTerrainMapOwnershipRealMongo` | 红：普通场景 scope 0 want 1；真实 Mongo 上 sid 1 / sid 2 写同一份块文档，`fatal projection version conflict: main/terrain_chunks/1070 ... expected=0 next=1 stored=3`，sid 1 的账本读不到 |
| M1 | `terrainmap.Open` 不调 `useDatabaseScope` | 离线用例 | 红：分区段 scope 1 want 0 |
| M2 | `terrainRegion` 的分区格不标 `Shared` | 离线用例 | 红：分区段 scope 1 want 0 |
| M3 | `DatabaseScope` 恒为 global | 离线 + 真实 Mongo | 红（同 M0） |
| M4 | 离开钩子不调 `Summary` | `TestWithMailboxRehome*` | 红：离开前发给旧服的那封没收到、离开前发给新服的那封多出来；失败不再中止 |
| M5 | 离开钩子不登记 | `TestWithMailboxRehome*` | 红：重复登记没有 panic |
| M6 | 生成器把 boot 的 `DbScope` 写成常量 sid | `TestGoldenGeneratedOutput` | 红：`gen_realm_dao.go differs` |
| M7 | golden 里 `RealmDao.DbScope` 恒为 sid | `scripts/dao-golden-runtime.sh`（`ROOST_CORE_REPLACE=<checkout>`） | 红：`after SetRealmDaoDbScope(global): scope 1` |

新用例（还原后全绿）：

- `TestTerrainDocumentsFollowTheMapOwnership`（离线，普通 → 分区 → 普通三段）：三种 DAO 的 `DbScope`、装入时查库的范围、被接受的提交里块 / 账本 / 建筑每条改动的 `Key.Scope` 都是该段的范围；格账本始终 sid。
- `TestTerrainMapOwnershipRealMongo`（真实 Mongo 副本集）：sid 1 编辑 + 放大厅 → 交出 Player → sid 2 同坐标编辑 → 回 sid 1。sid 2 装入时看不到 sid 1 的改动、对账 0 座建筑；回到 sid 1 序号 3 仍是重复；库里 sid 1 账本 LastSeq 7 / 1 座、sid 2 账本 LastSeq 6 / 0 座，块 (0,0) 两个服各一份，大厅只在 sid 1，共用库里没有地形文档。分区一段：`Shared` 的图写进 `<物理库>`、不进 `<物理库>_3`。
- `TestWithMailboxRehomeMergesTheOldServersMailBeforeLeaving`（真实 mail `Service` + 内存存储）：A 进入拉过之后发给 A 的一封收到；离开前发给 B 的、离开后发给 A 的不收；离开后发给 B 的收到。
- `TestWithMailboxRehomeAbortsTheMoveWhenTheMergeFails`：mail 失败时钩子带原错误返回（中止换服）、钩子已登记。
- codegen 运行档 `TestBootScopedDaoFollowsTheProcessChoice`：缺省 sid、设 global 后 `DbScope` 与提交记录的范围都是 global、设回 sid 跟着回。

## 四、验收

game-demo 生成工程（最终模板重新生成一次）：

```
go build ./... && go vet ./...     # 通过
roost generate --check             # generated files are up to date
roost id check                     # IDs are valid
go test -count=1 -timeout 30m ./...  # 57 个包 ok，0 FAIL（internal/service/game 85s）
roost module verify mailbox rehome # mailbox 33、+bag 9、+enter 5、+gm 4、+profile 10、+rehome 2；rehome 50、+enter 5、+gm 4；build / vet / id / generate 四项 ok
ROOST_TERRAIN_IT_MONGO_URI=<~/.roost-it 副本集> go test -run 'RealMongo|TestTerrainDocumentsFollowTheMapOwnership' ./internal/service/game/
                                   # TestBuildingRestartRealMongo、TestTerrainRestartRealMongo、TestTerrainEditLedgerRealMongo、
                                   # TestTerrainMapOwnershipRealMongo（两段）、TestTerrainWALCrashRealMongo（brush / building）全过；结束后无残留库
```

第一次全量时 `game/controllers` 的 rehome 场景与 `game/modules/gm` 的 `TestWithRehomeGMSuccess` 红了 6 条：离开钩子在没有 mail 能力的测试 Registry 里返回“capability not found”、把换服中止了。改成“取不到能力就跳过”（理由见上）后重跑全绿。

仓库一侧：

```
go test -count=1 ./codegen/internal/dao              # ok
go test -run 'TestModuleTerrainInstallAndVerify$|TestModuleGMScenariosAndMutations$' ./codegen/internal/roost/   # 两条 PASS（gm 那条含 mailbox+rehome 2）
go test -run 'TestDemo(EmbedCoversEveryFile|TemplateStepsAndShippedFilesAgree|...)$|TestHelp|TestModuleManifest' ./codegen/internal/roost/  # ok
python3 scripts/check-doc-anchors.py                  # 0 断链（本 README 写完后）
```

场景数变化：mailbox 的联动 `mailbox+rehome` 新增 2 条（`modules/mailbox/module.yaml`）；门禁计数 `codegen/internal/roost/module_gm_test.go`（want 加 `"mailbox+rehome": 2`）、`scripts/test-modules-generated.sh`（`all_counts`、装 rehome 那一步的 glue 检查与 `expect_verify`、全量工程的 glue 对列表）。terrain、rehome 的场景数不变（28、50）。

没跑：`TestDemoTemplateGeneratesABuildableWritePath`（TestDemo 整体，加了一条“demo 里有 mailbox+rehome 联动”的断言；已在生成工程里手查该文件内容）、`scripts/test-modules-generated.sh` 全量、codegen 全量、pretag；`scripts/test-demo-rehome.sh`（真实进程换服演练，没开地形、没配 LazyBroadcast，行为上只多一次离开钩子里的 `Summary`）与地形 REAL 演练（无缝 / 分区多进程）没跑，留给主线最后统一跑。

## 五、留下的边界（写进 KNOWN-LIMITS“地形的库范围按地图归属”）

- 客户端换服后按会话开始的规则先查 `last_seq`；旧服上没确认的编辑拿到新服重发会在新服执行（新服的账本不知道它）。
- terrain 模块的账本在 Player 档上、随玩家搬（建筑数按玩家算、不分服）；demo 的账本归地图（建筑数按图算）。两者的取舍不同，都按 N3 A 把建筑留在原地图。
- 合服：两个服的普通场景若是同一张图，sidmerge migrate 把 `<库>_<A>` 拷进 B 时块与建筑 `_id` 撞车，按 sidmerge 规则整体停止，由业务决定 A 的图怎么处理；terrain 模块同样如此。合服演练没开地形。
