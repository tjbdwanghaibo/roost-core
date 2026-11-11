# terrain：持久地形编辑与建筑占地

`roost module add terrain` 自动安装 scene → actor，生成 TerrainChunk、TerrainBuilding 与 Player 编辑账本，正式 PB/controller/Nest Sender、实体同步和启动/停机接线都随模块安装。新 game 工程不需要额外地形配置；基础托管服务仍使用工程现有部署配置。

默认启动生成一份确定性 flat Pack（revision 1，高度 10m，水位 0），通过 `gameplay/terrain.WritePack`、`OpenPack`、`View.LoadBase` 读取全部四块，再恢复持久编辑与建筑。包放在临时文件，停机关闭并删除；玩家数据只存 DAO。模块显式打开 scene 2，保留 scene 的 plain 1。客户端先通过 `SceneChange(ToID:2)` 进入。

可信服务器可从 `game/terrain/service.FromRegistry(registry)` 取得 Runtime，并 `Open(ctx, sceneID, packPath)` 打开自己的包。示例业务限制为每图最多 2×2 块（128m×128m），最多 4 张 terrain 图，每玩家最多 32 座站立建筑。限制写在工程规则中，不是协议位宽：坐标使用 int64、sceneID 使用 int64；持久 ID 预留 20 位 scene、10+10 位块坐标或 16+16 位建筑锚点。相同坐标在不同地图或分线拥有不同持久实体，分线编辑互不共享。编号 2 到 2²⁰−1 必须稳定代表同一编辑空间；关闭/重启后不能把这个编号配给另一张地图。

客户端协议：

- `TerrainBrush`：SceneID、Sequence、Op（raise/lower/paint）、X/Y、Radius、Strength、Material。半径最多 4m，单次升降最多 2m，材质 0–4，高度原始值限制在 0–200。最多涉及四块。
- `TerrainBuild` / `TerrainRemove`：SceneID、Sequence、锚点 X/Y。固定 2×2 个 1m 采样点，坐标必须整米；建筑不可重叠，放置需非水下且四点高差不超过 50cm。拆除只释放占地，保留当前地表。
- `TerrainQuery`：当前场景的一块坐标，返回地形版本、数据和认证玩家的 LastSequence。重连可据此续号；客户端拿不到权威占地表。

所有修改在锁内重新核对认证玩家的 Actor 场景与当前距离（16m），拆除还核对建筑属主。客户端不能选择 PlayerID、块实体 ID、owner 或直接上传地形数组。出生采样点受保护，不能在其上建造或刷地形，避免封死重新入场。其他玩家脚下的已建建筑由块 DAO 的 Lots 保护，笔刷不得改动其占地节点。

编辑账本使用每玩家全局连续 Sequence、完整命令摘要与最后一份小应答，不按地图创建无界历史。新序号必须为 last+1；同序号同内容重试本地零写入、返回 Duplicate，异内容、跳号、旧序号拒绝。更旧的成功请求不再返回历史应答，永远不会重做。失败不消耗序号。笔刷涉及的块与 Player、或建筑涉及的块+建筑+Player，均为同一笔 Nest undo/strict 事务；任何提交拒绝一起回滚。

`gameplay/terrain.Brush.Apply` 在锁内基于当前 DAO 数据计算修改；建筑重叠也只查这些锁内的 Lots。提交之后才发布 `View.SetOverlay`、行走占位与建筑 subject，原始包读取不进入 Nest 快/长业务 worker。`Walker` 直接作为正式 scene Ground/Surface 的行走依据。跨块提交在数据库事务中原子，但 View 逐块发布，跨块插值不承诺读取同一瞬间的快照。

地形块和建筑走 scene 的 entitysync/AOI；客户端用生成的 DAO 同步协议应用快照/字段增量。块的 Heights/Materials/Version 可见，Lots 与玩家账本不下发。基础场景的 AOI 可见数量限制仍生效，模块不另建同步传输。

aftercommit 的本地投影失败会记录 scene/实体/版本告警，并把应答 ProjectionPending 置为当前地图的故障状态。持久编辑不回滚。可信服务器调用 `Runtime.Refresh(ctx, sceneID)`，从 DAO 恢复 View、占位与建筑显示/隐藏，成功后清除对应故障代际；不产生新编辑序号。该入口不是后台自动重试，也不代表网络帧已送达，网络恢复仍由 entitysync 协议负责。重启时先从权威 Lots 定位并核对站立建筑，再开放地图。模块停机用 scene 的单图 Drain 排空所属地图，再关闭 Pack；不关闭 plain 或其他模块的地图。

已有编辑的 BaseRevision 与新包不符会失败并保留旧数据，必须显式迁移后再开放；同一个 BaseRevision 的内容须保持不变。没有自动丢弃覆盖块或建筑，没有原始包热换版迁移。建筑槽以锚点确定，拆除保留版本；活建筑计数有界，槽的总地址空间由地图大小限定。

固定验收 23 个独立场景、零跳过，覆盖四块+Player / 四块+建筑+Player、提交拒绝、重复与异内容序号、跨图权限、并发重叠、持久格式冷恢复、Pack 版本冲突、同步字段权限、投影故障恢复、单图排空、两个可编译行为变异。同时安装 monster 时，另外固定 2 个联动场景核对先停 terrain 的按图怪物排空、其他地图不受影响和中断后重试。提交器、同步传输使用内存替身；未执行真实 Mongo/WAL/NATS 部署或跨进程耐久性验收。

本模块没有迁移完整 demo 的高级地形能力：没有 cellworld/跨分区写、物理、建筑配置表/旋转/参数槽、任意形状地基整平、原始包换版对账、区域配额或大地图流式装卸。需要这些能力的工程应在这里清楚的事务与持久边界上继续扩展。
