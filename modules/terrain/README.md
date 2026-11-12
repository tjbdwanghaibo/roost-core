# terrain：可编辑地形——玩家用笔刷改高度与材质、放置与拆除建筑，编辑持久化并同步给附近的人

地形是 2.5D 高度场：一张图切成 64m×64m 的“块”，每块 64×64 个采样点（间隔 1m），每个点有一个高度原始值（1 = 10cm）与一个材质号。坐标单位是厘米。底图来自一个只读的地形包（Pack），玩家的编辑存成每块一个持久实体，叠加在底图之上。算法（包读写、视图、笔刷、行走判定）在框架的 `gameplay/terrain`，本模块提供的是持久实体、编辑事务、与场景的接线。

## 能力

- **客户端协议**（`protocol/def/terrain.go`）
  - `TerrainBrush`：请求 `TerrainBrushRequest{SceneID, Sequence, Op, X, Y, Radius, Strength, Material}`，应答 `TerrainBrushResponse{Code, Reason, Result}`。`Op` 取 `raise`（抬高）、`lower`（压低）、`paint`（刷材质）。
  - `TerrainBuild` / `TerrainRemove`：请求 `TerrainBuildRequest` / `TerrainRemoveRequest`，字段都是 `{SceneID, Sequence, X, Y}`，`X` / `Y` 是建筑的锚点。放置或拆除一座占 2×2 个采样点的建筑。
  - `TerrainQuery`：请求 `TerrainQueryRequest{SceneID, X, Y}`（这里 `X` / `Y` 是块坐标），应答 `TerrainQueryResponse{LastSequence, Code, Reason, Chunk}`，`Chunk` 是 `TerrainChunkInfo{SceneID, X, Y, BaseRevision, Version, Heights, Materials}`。读一块地形的当前数据与自己最后一个成功的编辑序号，重连后据此续号。
  - 三条编辑协议的 `Result` 都是 `TerrainEditResult{ProjectionPending, Sequence, Duplicate, Changed, BuildingID, Standing}`：`Duplicate` 表示这是重发、没有再执行；`Changed` 是改动的采样点数；`ProjectionPending` 表示这张图有一次提交后的本地投影失败、等服务端恢复。
  - 客户端不能指定操作人、块实体号、属主，也不能直接上传地形数组。地形块与建筑的内容经场景的实体同步下发，不另设推送协议。
- **服务端入口**
  - 运行期 `Runtime`（`game/terrain/service/runtime.go`），用 `service.FromRegistry(registry)` 取得（能力名 `gameplay.terrain`）：
    - `Open(ctx, id int64, path string) (*logic.World, error)`：用一个地形包打开一张地形图（同时在 scene 里开出同号场景），只给可信服务端用。
    - `Execute(ctx, playerID, command) (logic.Result, error)`：执行一条编辑命令（协议端点就是调它）。
    - `Query(ctx, playerID, sceneID, key)`：读一块。
    - `Refresh(ctx, id) error`：投影失败后，从 DAO 重新恢复本进程的视图、占位与建筑显示；不产生新的编辑序号。
    - `World(id)`、`Stop(ctx)`；包级的 `Start(registry)`（模块的运行期入口）与 `DefaultPack()`。
  - 规则与类型（`game/terrain/rules.go`）：`Command{SceneID, Sequence, Op, X, Y, Radius, Strength, Material}`（含 `Validate`、`Digest`、`Footprint`）、`Result`、`ChunkState`、`ChunkUnique` / `SiteUnique`（由场景号与坐标算持久实体号）。
  - `World`（`game/terrain/world.go`）：一张图的只读底图与提交后的投影；`Blocked(at)`、`Occupied(key, index)`、`Failures()`，并作为场景的 `Ground`、它的 `Walker` 作为场景的 `Surface`。
  - 组件 `TerrainComponent`（`game/entities/player/terrain_component.go`，经 `ITerrainEntity` 的 `TerrainComp()`）：`Check(command, sceneID)`（锁内判重与授权）、`Record(command, result, delta)`（记账本）、`BuildingCount()`、`Sequence()`。
  - Nest handler（`game/handler/terrain.go`，都是 `rollback=undo durability=strict`）：`handlerTerrainBrush`（参与的块 + Player）、`handlerTerrainBuild`（参与的块 + 建筑 + Player，放置与拆除共用）、`handlerTerrainQuery`、`handlerTerrainLoad`（开图装载）、`handlerTerrainSite`。经同一个 `sender.NewTerrainSender(client)` 调用。
- **登记点 / 钩子**：无。模块没有给别的功能留“编辑之后”的钩子。
- **状态**
  - `TerrainChunkDao`（`db/def/terrain_chunk.go`，集合 `terrain_chunk`）：`SceneID`、`X`、`Y`、`BaseRevision`、`Version`、`Heights`、`Materials` 落库并同步给视野内的人；`Lots`（采样点 → 占着它的建筑号）落库不下发，是占地的权威。
  - `TerrainBuildingDao`（`db/def/terrain_building.go`，集合 `terrain_building`）：`SceneID`、`GX`、`GY`、`OwnerID`、`Standing`、`Version`，落库并同步。一个锚点对应一个固定的建筑槽，拆除后槽与版本保留。
  - `PlayerDao.TerrainLedger`（`db/def/terrain.go`，`TerrainLedgerState{Sequence, Digest, Reply, Buildings}`）：这名玩家的编辑序号、最后一条命令的摘要与应答、站立建筑数；只落库不下发。
- **配置**：没有配置表与配置键。启动时生成一份缺省地形包（2×2 块即 128m×128m、版本 1、高度 10m 的平地、水位 0）放在临时文件里，并用它打开 **2 号**场景；scene 自带的 1 号平面场景保留。业务规则是 `game/terrain/rules.go` 里的常量：`MaxMaps = 4`、`MaxBuildings = 32`、`EditReach = 1600`、`MaxRadius = 400`、`MaxSceneID`。
- **错误码**：无。拒绝是不带业务码的普通错误（`ErrInvalid`、`ErrDenied`、`ErrSequence`、`ErrOccupied`、`ErrBase`、`ErrFull`）。

## 能力边界

- **规模上限**（写在 `rules.go` 与 `world.go`）：一个进程至多 4 张地形图；每张图至多 2×2 块；每名玩家至多 32 座站立的建筑；场景号必须在 2 到 2²⁰−1 之间，并且一个编号必须始终代表同一个编辑空间——关图、重启后不能把这个编号配给另一张地图。
- **单次编辑的限制**：笔刷半径 1～400（4m）；抬高 / 压低的 `Strength` 1～20（最多 2m）且 `Material` 必须为 0；刷材质的 `Material` 0～4 且 `Strength` 必须为 0；高度原始值限制在 0～200；一次笔刷至多涉及 4 块。建筑固定占 2×2 个采样点，锚点必须是整米坐标；放置要求四个点不在水下、高差不超过 50cm、不与别的建筑重叠；没有旋转、类型、参数。
- **权限**：所有编辑在锁内重新核对玩家当前所在的场景与距离（操作点离玩家不超过 16m，`EditReach`）；拆除还核对建筑属主。出生点受保护：不能在出生采样点上建造，笔刷范围不能盖到出生点。已有建筑占着的采样点不能被笔刷改动。
- **幂等的范围**：每名玩家一个**全局**连续序号（不分地图）。新命令的 `Sequence` 必须是上一个成功序号加 1；同序号同内容的重发不写任何东西，返回上次的应答并标 `Duplicate`；同序号不同内容、跳号、旧号都拒绝。只保留最后一条命令的回执——更早的成功命令重发会被拒绝，不会重做，也拿不回当时的应答。失败的命令不消耗序号。
- **一致性**
  - 一次笔刷涉及的所有块与 Player，或一次建筑操作涉及的块、建筑与 Player，在同一笔 Nest 事务里；提交被拒时一起回滚。重叠只查锁内 DAO 里的 `Lots`，不查缓存。
  - 提交之后才更新本进程的行走视图、占位与建筑显示。跨块的提交在存储上是原子的，但视图逐块发布，跨块边界的读取不保证看到同一瞬间。
  - 提交后的本地投影失败时，持久编辑不回滚：记一条错误日志，应答的 `ProjectionPending` 置真，直到可信服务端调 `Refresh` 恢复。`Refresh` 不是后台自动重试。
- **拒绝对客户端不可区分**：模块没有业务错误码。越权、序号冲突、占地冲突、建筑数到顶等拒绝，在应答里都是框架的通用内部错误码与固定的 `Reason`；只有笔刷端点的入参预检会带上 `ErrInvalid` 的文本。客户端分不出“序号错了该重查”与“这里不能建”。
- **换底图要迁移**：已有编辑的 `BaseRevision` 与新地形包不符时开图失败、旧数据保留（`ErrBase`），必须先显式迁移；同一个 `BaseRevision` 的包内容必须保持不变。没有自动丢弃编辑，也没有包热换版。
- **不做的事**：不做跨进程 / 分区地图的编辑、物理、洞穴与流体、建筑配置表与多种建筑、任意形状的地基整平、区域配额、编辑频率限制、大地图的流式装卸；客户端拿不到权威占地表（`Lots` 不下发）。
- **前提**：依赖 scene（scene 再依赖 actor）与 nest Mod；地形块和建筑走 scene 的视野，场景“每个观察者至多看到 128 个实体”的限制同样作用于它们。只在本进程内有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“场景模块的边界”一条（scene / monster / terrain 是单进程所有权）、“运行期接线的边界”一条。文件顶部关于可编辑地形的各条（分区场景、订阅规模、Unity 侧等）说的是 game-demo 与框架底座，不是这个模块。

## 复用方式

- **需求正好是它**：`roost module add terrain`，连带装上 scene 与 actor。装完启动就有 2 号地形场景；客户端先 `SceneChange{ToID: 2}` 进场，再发编辑协议。新工程不需要额外的地形配置。
- **需求多一点**
  - 用自己的地形包：在可信的服务端代码里 `runtime.Open(ctx, sceneID, packPath)`（每图至多 2×2 块；要更大先改 `world.go` 里 `OpenWorld` 与 `Occupied` 的 2×2 假设）。
  - 调编辑距离、笔刷半径、建筑上限、地图数：改 `game/terrain/rules.go` 顶部的常量与 `Command.Validate`。
  - 给客户端可区分的拒绝原因：模块没有声明错误码，要工程自己定义带码的错误并替换 `rules.go` 里的几个 `Err…`。
  - 多种建筑、朝向、建造消耗：在 `TerrainBuildingDao` 加字段（新字段用新的 store 键），在 `handlerTerrainBuild` 的事务里扣资源（同一个 Player 上的 wallet / bag 可以直接调）。
  - 不要改的地方：`ChunkUnique` / `SiteUnique` 的编号方式（它们决定持久实体号）、两个 DAO 已有的 store 键与集合名、“先查锁内 `Lots` 再写、提交后才发布视图”的次序。
- **别的模块 / 业务代码要用它**
  - 判断一个点能不能站、有没有被建筑占：取 `world, err := runtime.World(sceneID)`，用 `world.Blocked(point)`；这是已提交的投影，只能当候选，修改时仍要在事务里查块的 `Lots`。
  - 代玩家执行一次编辑（GM、剧情）：`runtime.Execute(ctx, playerID, logic.Command{…})`，序号规则与客户端相同。
  - 读一块的当前数据：`runtime.Query(ctx, playerID, sceneID, key)`（要求这名玩家在这张图里）。
- **不适合直接用的情形**：大世界或分区场景的可编辑地形（game-demo 里有另一套带分区的实现，不是这个模块）、体素 / 可挖洞的地形、建造玩法很重（多类型、升级、连接规则）的游戏。值得借鉴的是：“每玩家一个连续序号 + 命令摘要 + 最后一份应答”的有界防重账本；占地权威放在块的 DAO 里、与地形同一把锁；提交后投影失败用“故障代际 + 显式 `Refresh`”恢复而不是回滚持久事实。

## 联动

- `terrain/with/monster`：只有验证场景，没有额外的代码文件。场景核对的是：terrain 停机时经 scene 的单图排空先清走本图的怪物（monster 自己登记的清理入口），别的地图不受影响；排空中断后保留事实与资源、可以重试。2 条场景。
- 对 scene 的使用（依赖，不是 `with/`）：用 `Manager.Open` 开带 `Ground` / `Surface` 的图，块与建筑经 `Space().Enter` / `Leave` 进出视野，停机用 `Manager.Drain` 只排空自己的图。
- 别的模块没有 `with/terrain` 目录。

## 验证范围

- `roost module verify terrain`：23 条场景（`^TestTerrain`，包 `./game/controllers/terrain`）。覆盖：缺省包接到场景与行走判定、笔刷同时提交四块与玩家账本、重发只读、异内容 / 旧号 / 跳号拒绝、跨场景权限在事务内检查、过远与畸形笔刷、不同地图的同坐标有独立的持久号、建筑锁住四块 + 建筑槽 + 账本、重叠与拆别人的建筑被拒、不能刷建筑脚下、拆除释放占地但不恢复旧高度、提交失败时全部目标与视图回滚、建筑提交失败不发布占位、并发重叠建造只有一个成功、冷恢复保留编辑 / 占地 / 建筑 / 序号、换了版本的包不覆盖编辑、建筑数上限、同步暴露地表而隐藏占地权威、投影失败保留事实且 `Refresh` 可修复、PB 端点用认证身份与生成的编解码、损坏的包与地图数预算被拒、两个可编译的行为变异被发现、停机只排空地形图。
- monster 联动另有 2 条（`^TestWithTerrainMonster`）。
- 这些是离线场景：真实的 Nest 多实体事务、生成的 DAO / Sender、真实的地形包读写；提交器与同步传输是内存替身。没有做真实 Mongo / WAL / NATS 部署、跨进程耐久性、真实客户端或容量性能的验收。
- 仓库侧的安装验收是 `codegen/internal/roost` 里的 `TestModuleTerrainInstallAndVerify`。
