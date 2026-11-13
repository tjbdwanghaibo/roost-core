# 可编辑地形大世界（定稿方案，2026-11-08）

当前完成、未完成与待验收清单统一见 [地形进度总表](TERRAIN-STATUS.md)。

**2026-11-09 更新**：编辑配额 270 点/秒与 terrainpack 默认高度基准 −100 米已落地；同步按类型调度的框架机制及 game-demo 生产接线已实现；客户端圆滑岸线已确认、尚未实现，见 [后续决策及实施状态](TERRAIN-DECISIONS-20261109.md)。P3 移动与寻路（坡度 / 台阶 / 下落 / 游泳、高度场 A*、demo 普通场景接入）在本地分支 `feat/terrain-p3-move` 实施，未合入 main，见[第 22 节](#22-p3-移动与寻路实施结果2026-11-09)。（2026-11-11 核对：这一段是 11-09 当时的状态；P3 移动与寻路此后已合入 main，圆滑岸线的 C# 算法层已实现（§31），均随 v1.43.0 发布，当前状态以进度总表为准。）下文 §11～19 的测量与暂停记录保留为历史证据，不能当作当前状态。

状态：**定稿方案；P1 已实施：框架部分（`gameplay/terrain`：坐标、原始地形包、地形视图与查询）见[第 11 节](#11-p1-框架部分实施结果2026-11-08)，实体接入（game-demo 的 `TerrainChunk` 实体、地形服务、改块重启后一致）见[第 12 节](#12-p1-实体接入实施结果2026-11-08)；P2（单格）已实施：C# 客户端部分（读包、查询、接 DAO 同步、mesh 钩子）见[第 13 节](#13-p2-客户端c实施结果2026-11-08)，服务端部分（笔刷、校验、幂等、多块事务、整字段守卫、地形同步，真实 TCP 实测）见[第 14 节](#14-p2-服务端单格实施结果2026-11-08)；材质改为列表按下标增量（D11 复议）见[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)；P3 开工后暂停（额度用尽），交接见[第 19 节](#19-暂停点2026-11-08额度用尽)；P3 Ghost 副本（T05）见[第 21 节](#21-p3-ghost-副本t05d192026-11-09)。** 维护者 2026-11-08 确认按本文推荐整理为最终方案（第 8 节）；材质层当日先定 `[]byte`，P2 实测后按客户端下行预算（< 50KB/s）复议为 `[]uint16`（D11）。目标是在 cellworld 无缝大世界上支持玩家改地形（挖、填、抬高、整平、改材质）、由此形成湖泊和高地，并在地形上放置建筑。

数字的来源分两种：同步与持久化的单块字节、编码耗时来自离线微基准（[实验记录](evidence/terrain-sync-20261108/results.md)）；规模、带宽与参数初值是按假设推算，P2 / P5 实测后修正。

## 1. 现状（main v1.42.2）

| 部分 | 现状 | 位置 |
| --- | --- | --- |
| 大世界坐标 | cellworld 的 `EntityState` 只有 `X, Y`（int64）与速度，没有高度 | `gameplay/cellworld/wire.go` |
| 地图 | `spatial.GridTerrain`：二维稀疏阻挡格，每格只有“挡 / 不挡”；边界建时固定，只在内存，不持久化；没有高度、水、材质 | `infra/base/spatial/terrain.go`，demo `game/scene/runtime/terrain.go.tmpl` |
| 移动 | 逐格推进，每进一格问 `InBounds` / `Blocked`。P3 起（第 22 节）：`MoveConfig.Surface` 可接高度场，另外每进一个 1m 地面节点按坡度 / 台阶 / 下落 / 水判定，不接时逐位不变 | `gameplay/movement/move.go`、`gameplay/terrain/walk.go` |
| 寻路 | 格子 A*，单次最多 4096 个节点；分区模式下每个格进程整张图加载，跨格寻路只算到本格边界。P3 起（第 22 节）：开地形的普通场景走高度场 1m 节点八方向 A*（同一套通行规则、距离 + 爬升代价、上限仍 4096）；分区场景未接 | demo `pathfind.go.tmpl`、`gameplay/terrain/walk.go`，[无缝大世界方案](FEATURE-2026-11-03-seamless-world.md) 管理器分类表 |
| 物理 | 只在独立物理房间（约 20 人），形状只有 Box / Capsule / Sphere，世界开房时静态建好；没有高度场、三角网格、运行中增删体、射线 | `infra/physics/physx/types.go`，[CHUNK-PHYSICS](../framework/CHUNK-PHYSICS.md) |
| 分格 | 缺省 `cell_size` 3000、`ghost_band` 500，格到进程静态映射 | demo `game/settings/partition.go.tmpl` |
| 同步 | entitysync 按 DAO 字段同步；`[]byte` 整字段替换，列表可按下标增量；会话可持有对象数缺省 100，超出关闭会话；没有传输压缩 | [Entity Sync 契约](../framework/ENTITY_SYNC.md)、[DAO 同步 PB](../framework/DAO-SYNC-PROTOBUF.md)、`framework/sync/frame/frame_limits.go` |

结论：服务端没有高度、水体和 mesh 的表示，地形需要新建；同步可直接用 entitysync。cellworld 本身的生产化缺口另见 `fix/core-game-server-review` 分支上的首发清单（`LAUNCH-BACKLOG-3000-20261108.md` 的 L04，尚未合入 main）。

## 2. 地形表示

- **2.5D 高度场**：水平面上按固定间隔采样，每个 (X, Y) 只有一个地面高度。能表达山、高地、坡、盆地、坑、河床、湖（盆地 + 水面）；不能表达洞穴、隧道、悬空与天然拱。桥、楼、地下室作为建筑物体放在地形上。
- **渲染 mesh 在客户端生成**：服务端只存高度场与各层数据，Unity 按块生成网格、做 LOD 与接缝；服务端不存三角网格。
- **块（chunk）64m × 64m，采样间隔 1m**，每块 64 × 64 = 4096 个点（相邻块共享边由右 / 上块持有，不重复存）。
- **每点的层**：
  - 高度 `uint16`，10cm 精度、最大 6553.5m。存相对地图基准高度的值；值小于 16384（相对基准 1638m 以内）时同步全量每点 2 字节，超过时每点 3 字节。
  - 材质 `uint8` 编号（草地、沙、岩石、泥土、湖床……最多 256 种）。
  - 建筑占位由建筑实体推出，不单独持久化（第 6 节）。
  - 水按规则计算，不逐点存（第 5 节）。

### 2.1 规模（1w × 1w，1m 采样）

| 块大小 | 每边块数 | 总块数 | 每块原始（高度 + 材质） |
| --- | --- | --- | --- |
| 32m | 313 | 约 9.8 万 | 约 3KB |
| **64m（采用）** | 157 | **24,649** | 约 12KB |
| 128m | 79 | 约 6,200 | 约 48KB |
| 256m | 40 | 1,600 | 约 192KB |

整图 1 亿个采样点，原始约 300MB（高度 200MB + 材质 100MB）。原始地形是静态资源，不进数据库；数据库只存被改过的块；服务端内存只装有玩家附近的块。格边长取 48 块（`cell_size` 307200：cellworld 的世界单位是厘米，原写 3072 是按米算的，P1 更正），1w 地图约 4 × 4 个格，每格约 2,300 块，全部装入约 27MB 原始数据。

## 3. 数据模型与持久化

**原始地形（base）**：按块切好的静态资源文件，带 `BaseRevision` 与每块 SHA-256，服务端与客户端用同一份（沿用物理房间关卡“版本 + SHA-256 加载”的做法）。服务端启动时只读索引，块按需加载。

**地形块实体 `TerrainChunk`**：Nest 实体，ID 由块坐标编码；只为被改过的块建持久记录。DAO 示意（store 键按仓库规则显式写数字短键）：

```go
//roost:dao coll=terrain_chunks db=game schema=2
type TerrainChunkDao struct {
	BaseRevision int64    `store:"1"` // 基于哪一版原始地形
	Version      int64    `store:"2"` // 每次提交加一
	Heights      []uint16 `store:"3"` // 4096 点，同步与持久化按下标增量
	Materials    []uint16 `store:"4"` // 4096 点（值 0～255），同步与持久化按下标增量（D11 复议）
	Applied      []uint64 `store:"5"` // 最近 N 个已应用的编辑 ID，有界，不下发客户端
}
```

| 字段 | 写法与代价（单块，实测） |
| --- | --- |
| `Heights` | 小笔刷逐点 setter：同步每个改动点约 8 字节，持久化按下标 `$set` 每点约 12 字节；单次改动超过 1000 点时业务改用 `SetHeightsAll` 整字段（同步约 8.2KB、持久化约 40KB）。BSON 中是 int32 数组，整文档约 44KB |
| `Materials` | 与 `Heights` 同一写法（D11 复议，[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)）：小笔刷逐点 setter，同步每点约 7.3 字节（编号 < 128）/ 8.3 字节（≥ 128）；单块改动超过 500 点时 `SetMaterialsAll` 整字段（同步约 4.1KB，编号 ≥ 128 的点多时最多约 8.2KB；阈值 500 是维护者 2026-11-08 定的，字节持平点约 560 点，[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)）。原始地形包与 `gameplay/terrain` 里仍是每点 1 字节，DAO 为按下标增量取 `uint16`、值域 0～255，越界的记录按损坏处理。BSON 中也是 int32 数组：整文档约 80KB（`[]byte` 时约 44KB） |
| `Applied` | 定长环（如 32 个），每次编辑整字段替换（约 256 字节）；用同步视图白名单排除，不下发 |

加载 = 原始块 + 持久记录覆盖；从没改过的块不建实体、不读数据库，只读原始资源。BSON 更新小不等于 Mongo 磁盘写放大等比例下降（[增量持久化](../framework/DATAENGINE-INCREMENTAL-PERSISTENCE.md#持久安全边界)）；编辑频率高时再考虑“编辑日志 + 定期压实”（P5 实测后决定）。

## 4. 编辑：权威、校验与事务

**权威**：块归包含它的 cellworld 格所在进程；格到进程静态映射，块不迁移。`cell_size` 是块边长的整数倍，一块不跨格。

**编辑操作**（笔刷类，整数运算，客户端可同算用于预测）：

| 操作 | 参数 |
| --- | --- |
| 抬高 / 降低 | 中心、半径、增量、衰减曲线 |
| 整平 | 中心、半径、目标高度 |
| 改材质 | 中心、半径、材质 |

每个操作带（玩家 ID, 编辑序号），按玩家的编辑账本判重：同一请求至多执行一次（D18，[第 20 节](#20-编辑请求身份与判重t01t02d182026-11-09)；原“块内 `Applied` 环”有 T01/T02 两个缺陷，已取代）。

**链路**：客户端 → 玩家所在的 Base 进程（鉴权、领地 / 权限、频率）→ 按落点转发给目标格的 Real 所在进程（与 cellworld 跨格攻击“转发给目标 Real 结算”同一模式）→ Nest 多实体请求锁住受影响的块 → 校验 → 写 DAO → 提交；提交并持久确认后由 entitysync 下发（第 7 节）。

**服务端校验**：

- 玩家笔刷半径上限 16m（单块约 800 点：改高度低于高度守卫 1000，改材质超过材质守卫 500 时整字段）；整平、地基等大范围操作走守卫；
- 单次最多影响 4 块；
- 单次最大高度增量、整平目标的范围；不能低于基岩高度、不能高于上限；
- 不能改有建筑占位的点；
- 每玩家改动点数配额：每人每秒 270 点（维护者 2026-11-08，取代“每秒 2 次”）。一次编辑的点数 = 会改变的高度点数 + 材质点数（共享边点只算一次，一个点两者都改算 2），至少 20；令牌桶容量 270，满桶时可以透支一次（单次超过 270 点的 r=16m 笔刷、整平 / 地基照样能做，做完等补满）；不够时整笔拒绝、零写入。重复请求与确定没改的拒绝只计 20 点。按区域限每秒改动点数的兜底不做，P5 视实测再定（规则与实测见[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)）；
- 领地 / 权限规则由业务提供钩子。

**整字段守卫**：一次编辑在某块内改动的高度点数超过 1000 时，对该块用 `SetHeightsAll` 写整字段，不逐点写。原因：逐点补丁每点约 8 字节，约 1020 点以上比整字段大，阈值附近最多是整字段的 4 倍（[实验 §1](evidence/terrain-sync-20261108/results.md)）。材质另有自己的守卫：改动的材质点数超过 500 时 `SetMaterialsAll`（材质整字段约 4.1KB、逐点每点约 7.3 字节，持平点约 560 点；维护者 2026-11-08）。

**跨格编辑**：首版拆分为各格独立提交（同一请求 ID 派生子 ID），边界接缝最终一致；跨格原子编辑不做。

**消耗资源**：玩家在 Base 进程、块在格进程，是跨进程。Base 先预扣，编辑结果按请求 ID 对账：确认失败才退还；结果未知时按请求 ID 查询编辑结果后决定，不能直接退。

## 5. 水：规则水位

**全局（或按区域配置的）水位线**：地面低于水位线的点就是水。挖到水位线以下自动成为池塘或湖，抬高到水位线以上就是陆地 / 高地。纯规则、可在任一端算出、不需要跨块计算，也不需要同步额外数据。

后续可选：盆地水位（编辑造出封闭盆地时有界洪水填充算出水面高度），P4 之后按需求做。不做流体模拟（流动、漫溢、决堤）。

## 6. 建筑

建筑是独立持久实体 `Building`：

- 类型、锚点、朝向、占地形状（格子集合）、底面高度、状态（建造中 / 完成 / 损坏）。
- 归锚点所在格的进程，首版禁止跨格建筑。
- 放置校验：占地内坡度上限；不在水下（或按类型允许）；不与其他建筑重叠；权限。
- 放置时可选同一事务整平地基（对块的编辑，受第 4 节约束，通常触发整字段守卫）。

块的“占位”不单独持久化：格进程加载时由本格建筑实体推出，建筑变化时在同一事务里更新内存占位。建筑本身也是 entitysync 的实体，与地形块分别计入会话对象数。

## 7. 同步

**载体：改过的块作为实体，用 entitysync 同步。** 生成 DAO 的变化由框架收集、按字段同步；不新增地形消息，不复用 chunksync（它是物理对象的按区域增量，不是数据补丁），也不做“下发编辑操作、客户端重放”。

- **订阅：独立的地形 Interest。** 地形单独建一个 `policy.Interest`（独立订阅来源），玩家主视野作为它的 AOI 观察者，只把改过的块登记为 subject。从没改过的块不建实体、不订阅，客户端用本地原始资源（按 `BaseRevision` 与 SHA-256 校验）。视野 200m 约覆盖 31 块的中心（π·200² / 64²；P2 更正，原写 50～60），加上进出视野的滞回（220m）约 37 块，所以缺省视野到不了 `MaxVisible` 64，64 是放大视野或以后加大半径时的上限。
- **对象上限。** `frame.Limits.MaxObjects` 缺省 100，同时是一个会话可持有的 subject 数；超出时不关会话（D17，2026-11-08 起超额降级，[第 18 节](#18-超额降级减少同步对象不断开d172026-11-08)）：放不下的新对象暂缓、不截断，地形 Interest 从最远的块开始收紧视野，有空位时逐步放宽。地形块单独计（D15，2026-11-08 起）：场景 Manager 的 `ManagerConfig.Namespaces` 列出 `terrain` 命名空间，上限 64（等于地形 Interest 的 `AOIConfig.MaxVisible`）；玩家、NPC、建筑仍按 `Limits.MaxObjects`（demo 100），不被放宽。会话的对象引用表合起来 164，每帧对象数仍按 100 切。地形块超过 64 只在地形一类降级，玩家、怪照常；玩家超过 100 同样只在玩家一类降级，地形照常。（P2 原做法是把整个 Manager 的 `MaxObjects` 调到“其他可见实体上限 + 64”，玩家的上限也跟着放宽，已按 D15 改掉。）
- **基线限流。** 改过的块进入视野时发全量（约 12.3KB：高度 8.2KB + 材质 4.1KB；材质编号 ≥ 128 的点多时最多约 16.4KB）。地形命名空间单独的快照预算（`NamespaceLimits.SnapshotBudget`），每会话每次 Flush 的地形全量块数初值 4（50ms 一轮时峰值约 1MB/s，31 块约 0.4s 发完）；只计地形，玩家与怪不占它、也不受它限（D15；P2 原先开的是 Manager 级 `SnapshotBudget`，对玩家、怪也生效）；增量与 remove 不受限。
- **增量。** 高度逐点补丁每点约 8 字节（r=5m 约 0.67KB），材质每点约 7.3～8.3 字节（r=5m 约 0.6～0.7KB）；大编辑走各自的整字段守卫（高度超过 1000 点 8.2KB、材质超过 500 点约 4.1KB）。变化树每 DAO 最多 4096 个节点、由两个列表共享：单笔提交在两个守卫之内最多约 1504 个节点，不会提前回退；同一块在一次确认之前累积超过预算时整树回退（[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)）。单块补丁最坏（两个字段各约 2000 个不同下标）约 32KB，低于单组件上限 `MaxComponentBytes` 64KB。
- **继承的交付契约。** 持久化门槛（按 `CommitLSN` 挡整个 subject，编辑落 WAL 前不下发）、版本顺序与断档恢复（EntitySync 外层负责）、断线重连 / Hold-Ready 的新 epoch 全量都直接继承。
- **客户端。** Go 用生成的 `ApplySync<Dao>`，C# / C++ 用同一份 `dao_sync.proto` 与生成 Apply；应用后按 `Version` 重建该块 mesh。
- **没有客户端缓存。** entitysync 不记客户端已有哪个版本：块离开视野再回来会重发全量，版本没变也一样（AOI 的进出半径不同，只防边界抖动）。首版接受，按第 7.1 节的量评估；超出预算再做“客户端已有此版本则跳过全量”（第 8 节 D12）。

**格边界**：邻格需要 Ghost 带（按 500m 配置即 `ghost_band` 50000；demo 缺省的 500 只有 5m）内的地形与建筑占位副本，用于视野、移动和交接判断。格进程之间像 Ghost 一样推送边界块的版本与变化，副本只读。

### 7.1 同步数据量（实测）

数字是真实 TCP 实测（[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)、[证据](evidence/terrain-materials-list-20261108/README.md)；材质守卫 500 与编辑配额的重测见[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)、[证据](evidence/terrain-quota-20261108/README.md)；方法同 P2）；全服出口一行仍是推算。客户端下行预算：**低于 50KB/s**（维护者 2026-11-08）。

单块下发量（DAO 组件 payload，不含帧头与 TCP/IP 头；框架没有传输压缩）：

| 内容 | 字节 |
| --- | ---: |
| 高度增量 r=5m（69～81 点） | 575～671 |
| 材质增量 r=5m（81 点） | 590（编号 < 128）～671（≥ 128） |
| 高度 + 材质同一拍改同一处 r=5m | 1,150～1,231 |
| 改材质 r=16m（玩家上限，约 800 点 > 材质守卫 500，整字段） | 4,151～4,948（第 16 节守卫 1000 时按下标 5,280～6,399） |
| 守卫整字段：高度 / 材质 | 8,213 / 4,117 |
| 改过的块进入视野（全量） | 12,319（材质编号 ≥ 128 的点多时最多约 16.4KB） |

| 场景（一个观察者） | 下行（线上，含帧头） | 对照 50KB/s |
| --- | --- | --- |
| 建造热点：附近 20 人各每秒 1 次 r=5m 编辑 | 只改高度 19.2KB/s；一半带材质 25.1KB/s；全部带材质 30.6KB/s（P2 材质整字段时 75.5 / 132.6） | 内 |
| 同上，笔刷改为 r=16m（玩家上限） | 不带材质 112.5KB/s；全部带材质 231.4KB/s（每人每秒约 1,500 点，D16 配额下到不了） | 超（不限配额时） |
| 建造热点，附近 20 人都按配额满速编辑（每人 300 点 / 秒，D16；稳态） | r=16m 抬高 47.9KB/s、抬高 + 改材质 48.8～49.4KB/s；r=5m 抬高 54.1KB/s、抬高 + 改材质 53.1～53.4KB/s；混合 52.3KB/s | r=16m 内；**r=5m 超 6～8%**（见第 17 节待决定） |
| 跑图 6m/s / 骑乘 12m/s | 沿途全改过 8.1 / 16.6KB/s；沿途改过 20% 1.6 / 4.1KB/s | 内 |
| 登录 / 传送 / 重连到全改过的区域 | 约 31 块 × 12.3KB ≈ 380KB，由快照预算分摊（每 Flush 4 个，约 0.4s 发完） | 一次性 |
| 全服出口（推算）：3000 人在线，10% 每秒编辑 1 次、平均 10 个观察者；一半在跑图，沿途 30% 改过 | 编辑增量约 2.6～5.0MB/s（每次编辑每个观察者只改高度约 0.9KB、同时改材质约 1.65KB；材质整字段时上限 10.6MB/s）+ 跑图加载约 3.9MB/s，合计约 6.5～8.9MB/s；按 16 个格进程平摊每个不到 1MB/s | — |

读法：材质改为按下标增量后，热点下行由“改材质的比例”变为“每秒改动的点数”决定：不限配额时玩家用最大半径 r=16m 连续编辑的热点超预算约 2～4.5 倍，所以 D16 改为按改动点数限每个玩家。配额满速的稳态实测：线上 ≈ 8.0 字节 × 改动点数 + 约 110 字节 × 块更新数，同样每秒 6,000 点，小笔刷摊到的块更新多，r=5m 档比 r=16m 档多约 5KB/s、超预算 6～8%（[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)）。跑图加载与材质无关，D12 维持不做。

## 8. 决策（定稿）

| 编号 | 决策 | 采用 | 备选与复议条件 |
| --- | --- | --- | --- |
| D1 | 地形表示 | 2.5D 高度场 | 体素（可做洞穴，数据与同步量大一个量级）；不做 |
| D2 | 采样与高度精度 | 1m 采样；高度 `uint16`、10cm 精度、相对基准存储 | 0.5m / 2m 采样；1cm 精度（655m 高差） |
| D3 | 水 | 全局 / 区域水位线 | 盆地水位（P4 之后按需）；流体模拟不做 |
| D4 | 块与格对齐 | 块 64m，格 48 块（`cell_size` 307200，世界单位厘米；P1 更正，原写 3072） | 块 128m；块跨格不做 |
| D5 | 跨格编辑 | 拆分为各格独立提交，接缝最终一致 | Remote / Saga 原子编辑（有玩法需要时再议） |
| D6 | 编辑扣资源 | Base 预扣 + 按请求 ID 对账 | Saga |
| D7 | 跨格建筑 | 首版禁止 | 锚点格拥有 + 邻格只读副本 |
| D8 | 高度字段形状 | `[]uint16` 列表按下标增量；单块改动超过 1000 点用 `SetHeightsAll` 整字段；玩家笔刷半径上限 16m。小笔刷每次 56～665 字节，是整字段 8.2KB 的 1/150～1/12（[实验](evidence/terrain-sync-20261108/results.md)） | `[]byte` 整字段（CPU 最低、文档 12KB）；生成器按字节量自动回退（要改生成器，守卫维护成本高时再议） |
| D9 | 寻路 | 高度场上的格子 A*（坡度代价、按块版本失效）。P3 实施（第 22 节）：节点 = 1m 采样点、八方向不切角，边与移动逐节点判定同一规则（`terrain.Walker`）；代价 = 水平距离 + 爬升 × 2（下坡不加）÷ 游泳速度；估价放大 1.5 倍（加权 A*，代价至多是最优的 1.5 倍；维护者 2026-11-09 确认，见第 22 节）；单次最多展开 4096 个节点（沿用 demo）；不缓存路线（一次请求算一次，移动逐节点按最新地形判定），路线带所经块的版本（`Route.Chunks` / `Fresh`）供持有方判断失效 | 动态 navmesh（重建代价高）；按块版本失效的有界路线缓存（同一路线被高频重复请求、实测寻路成为瓶颈时再做） |
| D10 | 地形物理碰撞 | 首版不做 | PhysX 高度场 + 每格物理世界（P6） |
| D11 | 材质字段形状 | `[]uint16` 列表按下标增量；单块改动超过 500 点用 `SetMaterialsAll` 整字段（守卫阈值维护者 2026-11-08 定为 500，原 1000，[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)；维护者 2026-11-08 复议：客户端下行预算 **< 50KB/s**；P2 实测热点全部带材质 133KB/s，材质整字段每次 4.1KB 超预算）。原始地形包与 `gameplay/terrain` 仍是每点 1 字节，转换在 demo 组件边界（值域 0～255，越界拒绝）。热点全部带材质实测 30.6KB/s（[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)） | `[]byte` 整字段（当日初定；CPU 与文档最省，改材质每次 4.1KB）；材质守卫 1000（与高度一致，第 16 节初版；560～1000 点的改材质逐点比整字段大，最多约 1.6 倍） |
| D12 | 客户端版本缓存 | 首版不做，离开再进入视野重发全量 | entitysync 支持“客户端已有此版本则跳过全量”；跑图加载流量超预算时做（框架功能） |
| D13 | 原始地形换版后的旧编辑 | 丢弃（维护者 2026-11-08）：格进程启动时，本格基于旧 `BaseRevision` 的记录经正式实体 / DataEngine 路径处理后改用新原始块，计数并记日志；处理失败时这块不装入、拒绝编辑，绝不用原始块覆盖旧记录；邻格 Ghost 带里的旧记录只由它的格处理（实现见第 14 节）。“丢弃”实现为把记录重置为 Version 0、不真删记录（确定性 ID 删档后不能再建），维护者 2026-11-08 接受 | 按差值重放到新原始块（要保留编辑日志或旧包）；保留旧记录覆盖新原始块（地形与新美术不衔接） |
| D14 | Ghost 带地形副本的更新 | P3 把邻格的编辑推送到 Ghost 副本（维护者 2026-11-08 定为后续工作；2026-11-09 实施，[第 21 节](#21-p3-ghost-副本t05d192026-11-09)）；P3 之前 Ghost 副本只是启动时的快照，依赖邻格地形的判断（例如跨格移动的高度）可能读到旧数据 | — |
| D15 | 地形的对象上限与快照预算 | 不影响玩家和怪（维护者 2026-11-08：“会话对象上限 164 和全量分摊这两个设置，不能影响玩家和怪的同步”）：entitysync 按 subject 命名空间单独计（`ManagerConfig.Namespaces`），场景里 `terrain` 上限 64、快照预算每会话每 Flush 4 个；玩家 / 怪沿用 100、不限快照，交付与不开地形时逐字节相同（实现见第 15 节） | P2 原做法：Manager 级 `MaxObjects` 164 + Manager 级 `SnapshotBudget`（玩家 / 怪也被限流、上限也被放宽）；按订阅来源分类（一个对象可经几个来源订阅，归类取决于订阅顺序） |
| D16 | 玩家编辑限流 | 每人每秒 270 个改动点、桶容量 270、每次至少 20 点、满桶允许一次透支；事务前按视图估算，配额不足整笔拒绝。维护者 2026-11-08 选定，2026-11-09 实施 | 300 档及 270 临时测量见 §17（历史证据）；本次未重跑带宽压测，区域兜底仍待 P5 |
| D17 | 超额降级：减少同步对象，不断开 | 会话持有对象超过上限（`Limits.MaxObjects` 或按命名空间的 `MaxObjects`）时不关会话、不断开（维护者 2026-11-08）：Manager 暂缓放不下的新对象（钉住在前、Profile 优先级、实体 ID 排序，不截断），通知订阅来源；Interest 收紧该玩家视野的可见上限、从最远的开始退订，最终留下最近的 N 个，拥挤散去后带滞回逐档放宽；自身永远优先、不暂缓；地形命名空间同样适用。`SessionLost` 不再有 `object_limit`。未超额时帧逐字节不变（实现见[第 18 节](#18-超额降级减少同步对象不断开d172026-11-08)） | 超出即关会话并断开玩家（D15 及以前的契约；重连回同一人堆会反复断开）；只拒该类新对象、不通知政策（订阅停在“要全量”却永远发不出，等于静默截断）；Manager 按 ID 挤掉已持有的对象（不知道距离、线上 remove / create 往返） |
| D18 | 编辑请求的身份与判重 | 身份 =（玩家 ID, 编辑序号），原样比较；序号由客户端按玩家单调递增地取。判重在玩家的编辑账本 `TerrainEditor`（每玩家一份：最大已应用序号 + 其下 64 位窗口），与改块同一笔事务；窗口内应用过的答重复、没应用过的照常执行，低于窗口的答过期（`terrain_seq`）、不执行。块上的 `Applied` 删除（`TerrainChunkDao` schema 3）。实施方 2026-11-09 选定，维护者 2026-11-09 确认按推荐：独立账本实体、窗口 64、超前上限 2^20、客户端续号用 Seq=0 查询（[第 20 节](#20-编辑请求身份与判重t01t02d182026-11-09)） | 扩大块上的 `Applied` 环（只推迟 T01）；账本放在块上按玩家记（随改过它的玩家数无界增长）；放在 Player 实体（P3 玩家与块分属两个进程时不能同事务）；带内容指纹判“序号被重用”（多存储，不解决同内容重用） |
| D19 | Ghost 副本的版本与代际（P3） | 代际 = 原始地形包的 `BaseRevision`，每条副本消息带它、两个方向不同即整条丢弃；同一 `BaseRevision` 内按 `Version` 只进不退；拥有者只推持久版本（改块 strict、提交后回调里登记推送）；进程间推整块，订阅每秒续期兼补齐（租约 3 秒）。实施方 2026-11-09 先定，维护者 2026-11-10 确认按推荐。 | 另设 epoch（改块不再 strict 时需要）；进程间推增量（省带宽，丢一条就要补全量） |

### 8.1 首版参数初值（P2 实测后修正）

| 参数 | 初值 |
| --- | --- |
| 块 / 采样 / 格 | 64m / 1m / `cell_size` 307200（48 块），`ghost_band` 50000（500m）。P1 更正：世界单位是厘米（cellworld 约定），原写 3072 / 500 是按米算的，块边长 6400 |
| 高度 | `uint16`，10cm；本游戏资源默认 HeightBase = −100 米（−10000 厘米），水位独立；通用库不固定基准 |
| 整字段守卫阈值 | 单块高度 1000 点、材质 500 点，各自计数（材质守卫是 D11 复议新增，500 由维护者 2026-11-08 定） |
| 玩家笔刷半径上限 / 单次最多块数 / 编辑配额 | 16m / 4 块 / 每人每秒 270 个改动点（D16，维护者 2026-11-08 取代 P2 的“每秒 2 次”）：令牌桶容量 270、满桶可透支一次、每次至少 20 点（`Policy.EditPointsPerSecond` / `MinEditPoints`）；参数与归属合法的请求才计 |
| 玩家单次最大高度增量 / 高度范围（P2 新增） | 强度 ≤ 50（5m）；基岩 0、上限 16383（1638.3m；原始值不超过 16383 时 varint 2 字节，增量每点仍约 8 字节） |
| 地形视野半径 / `MaxVisible` | 200m（滞回 20m，视点每移动 8m 更新一次）/ 64 块；200m 实际约覆盖 31 块（第 7 节 P2 更正） |
| 对象上限 | 地形单独 64（`Namespaces["terrain"].MaxObjects`）；玩家 / 怪 / 场景仍是 `Limits.MaxObjects` 100；会话引用表合起来 164（只在开地形的普通场景；D15）。超出任一类时超额降级、不断开（D17） |
| 地形快照预算：每会话每 Flush 全量块数 | 4（`Namespaces["terrain"].SnapshotBudget`，只计地形，D15；视野满 64 块时至少 16 次 Flush 发完，40 个其他对象第一次 Flush 全部到达） |
| 编辑账本窗口（原 `Applied` 环长 32，D18 取代） | 已应用的最大序号之下 64 个；序号最多超前 2^20（[第 20 节](#20-编辑请求身份与判重t01t02d182026-11-09)） |
| 行走规则（P3，`terrain.DefaultWalkRules`，第 22 节） | 坡度上限 45°（千分比 1000：相邻节点正交 100 厘米、对角 141 厘米）；台阶 50 厘米（只在调低坡度时起作用）；陡于坡度的下行落差 ≤ 3 米；水深 ≤ 50 厘米涉水、更深游泳（站立高度取水面）；游泳速度 60%；寻路上坡代价 2（爬 1 米折 2 米平路）、下坡 0、估价放大 1.5 倍。demo 不做成配置，与客户端预测同一组数 |
| 寻路节点上限 | 4096（沿用 demo 网格寻路）。实测：平地 200m 展开 201 个节点；起伏地面 50m / 200m 展开 378 / 683 个（不加权时 1795 / 约 1.4 万，200m 超限） |

## 9. 分期与验收

| 期 | 内容 | 验收 |
| --- | --- | --- |
| P1 地形底座 | 原始地形资源格式与加载；`TerrainChunk` 实体、持久化与“原始 + 覆盖”加载；`Height(x, y)` / `Material` / `IsWater` 查询（单进程） | 1w × 1w 原始资源加载与按需装块；改块重启后一致；查询与客户端同算法结果一致（实施结果：[§11](#11-p1-框架部分实施结果2026-11-08)、[§12](#12-p1-实体接入实施结果2026-11-08)；C# / C++ 客户端同算法留 P2） |
| P2 编辑与同步（单格） | 笔刷操作、校验、幂等、多块事务、整字段守卫；地形 Interest、`MaxObjects` / `MaxVisible`、`SnapshotBudget`；C# 客户端 Apply 与重建 mesh | 重复 / 乱序请求不重复生效；断线重连后客户端与服务端块版本、数据一致；视野内改过的块达到 `MaxVisible` 时会话不断开（P2 当时的口径是不因 `object_limit` 断开；D17 起超额也不断开）；真实 TCP 下 r=5m 单块增量约 0.7KB、材质约 4.1KB；第 7.1 节热点与跑图两档的下行实测，并据此复议 D11 / D12；单格编辑吞吐与 Nest 短池耗时实测（实施结果：C# 客户端 [§13](#13-p2-客户端c实施结果2026-11-08)、服务端 [§14](#14-p2-服务端单格实施结果2026-11-08)；C# 的笔刷同算与 C++ 客户端未做） |
| P3 接入 cellworld | 块与格对齐、边界块副本推送；移动加入高度查询与坡度 / 台阶上限、游泳；寻路改为高度场 A* | 跨格移动、交接与边界视野在改地形后正确；寻路随块版本失效；现有 cellworld 回归不退化 |
| P4 水与建筑 | 水位线规则；`Building` 实体、放置校验、地基整平、占位推导 | 挖坑成湖、填平成陆的规则可复现；建筑占位重启后正确；不在水下 / 坡上违规放置 |
| P5 跨格与规模 | 跨格编辑拆分与扣资源对账；按需编辑日志压实；按区域限每秒改动点数的兜底（D16 备选，视机器人实测的编辑分布再定）；1w 地图 + 目标在线的容量测试（并入首发清单 L06 / L07） | 跨格编辑无双写、接缝最终一致；内存、写放大、全服出口带宽在预算内（对照第 7.1 节） |
| P6 物理（可选） | PhysX 高度场形状、运行中更新地形、每格物理世界 | 投射物 / 载具与改过的地形碰撞正确；与 Unity 同 tick 对照 |

## 10. 不在范围

洞穴与隧道（体素）、流体模拟、地形的跨机物理世界迁移、客户端地形渲染与 LOD 的具体实现（只约定数据格式与版本）。cellworld 自身的生产化（崩溃恢复、交接验收、时钟前提）按首发清单 L04 另行推进。

## 11. P1 框架部分实施结果（2026-11-08）

分支 `feat/terrain-p1`，新包 `gameplay/terrain`（[包说明](../../gameplay/terrain/README.md)），纯逻辑加文件 I/O，不依赖 Nest / DataEngine / entitysync。P1 剩下的 `TerrainChunk` 实体、持久化与“改块重启后一致”由下一轮在 demo 里接。实测数字与复跑命令见[证据目录](evidence/terrain-p1-20261108/README.md)。

**坐标**：世界单位按 cellworld 的约定是厘米（`grid.go`：测试与测量里 1 单位 = 1 厘米），块边长 6400、采样间隔 100、高度步长 10（10cm）。这三个值与每块 64 × 64 采样都是常量（决定 DAO 形状和客户端算法），包头记录、读包核对。块键 `ChunkKey{X, Y}`，`ID()` 编码为高 32 位 X、低 32 位 Y；`ChunkOf` / `PointIndex` 向负无穷取整，远处坐标饱和不回绕。由此第 2.1、7、8 节的 `cell_size` / `ghost_band` 已改成厘米值。

**API 摘要**：

| 部分 | 入口 |
| --- | --- |
| 块数据 | `Chunk{Heights []uint16; Materials []byte}`（各 4096，行优先），与 DAO 同形；返回 `Chunk` 的函数都给新切片，接收的函数都复制 |
| 原始地形包 | `WritePack(io.WriterAt, Meta, ChunkSource)`、`WritePackFile`（临时文件 + fsync + 改名）；`OpenPack`、`Pack.ReadChunk`（可并发）、`Pack.ChunkSum`、`Close` |
| 数据来源 | `RAWImport{Heights, Materials, Width, Height, BigEndian, FlipY, MaxHeight}.Source(meta)`；确定性程序化生成 `Generated(meta, seed)` |
| 地形视图 | `NewView(pack)`；写入口 `LoadBase` / `SetOverlay(key, Overlay{BaseRevision, Version, Heights, Materials})` / `Unload`；`Version` / `Chunk` / `Len` |
| 查询 | `Height`（双线性插值、整数定点、四舍五入到厘米）、`Material`（所在 1m 格左下角采样点）、`IsWater`（低于全局水位线）、`Probe`（三者一次取） |
| 错误 | `ErrOutOfMap`、`ErrNotLoaded`（邻块没装，带块坐标，不猜）、`ErrCorruptChunk`（单块坏，不给默认值）、`ErrBadPack`、`ErrPackVersion`、`ErrStaleOverlay`、`ErrBaseRevision`、`ErrInvalidChunk` |

**包格式（版本 1）**：128 字节包头（magic、格式版本、`BaseRevision`、块数、几何常量、`HeightBase`、`WaterLevel`、codec、对包头与索引表的整体 SHA-256）+ 每块一项的索引表（偏移、长度、存储字节的 SHA-256）+ 每块独立压缩的数据。块编码是“平面预测差分 zigzag varint + raw DEFLATE”。打开只读包头与索引（1w 地图约 1MB），读块时校验该块的 SHA-256。

**实测（1w × 1w，157 × 157 块，Apple M5，程序化地形，页缓存热）**：

| 项 | 结果 |
| --- | --- |
| 包大小 | 66.8 MiB，平均每块 2.8KB |
| 生成 + 写包 | 约 2.4s |
| 打开（只读索引） | 约 0.5ms |
| 单块加载 P50 / P99 | 约 39µs / 约 163µs |
| 装满一个格（48 × 48 = 2304 块） | 单线程约 120ms，8 路并行约 29ms；堆约 27MB |
| 查询 | 单 goroutine 9ns（块在缓存里）～25ns（满格随机）；10 路并行无锁竞争 |

**与本文不一致之处**：

- `cell_size` / `ghost_band` 初值的单位（上面已述），第 2.1、7、8、8.1 节已改。
- 块编码：第 3 节没定编码，实验记录推荐的“行差分 varint + flate”约 4KB / 块；P1 实测平面预测（左 + 下 − 左下）约 2.7KB，采用后者（程序化地形，真实地形待测）。
- 每块 SHA-256 取的是存储（压缩后）字节：读块先校验再解压；客户端用同一份包，按 `BaseRevision` 与这个值核对。实测翻转块数据中间一个比特，DEFLATE 仍能“成功”解出数据，只有 SHA-256 能发现。
- 覆盖块的 `BaseRevision` 与包不同时 `SetOverlay` 拒绝（`ErrBaseRevision`）。原始地形换版后旧编辑怎么办（保留 / 重放 / 丢弃）本文没写，留给维护者与产品决定。

**客户端金样**：`client/spec/terrain-golden.pack`（3 × 2 块，含 0 / 65535 棋盘格与斜坡的边界情形块）与 `client/spec/terrain-queries.json`（块解码后的 SHA-256、246 个位置的期望高度 / 材质 / 水）。证据目录里一份只按文档写的独立实现（Python）全部一致；C# / C++ 留 P2。

## 12. P1 实体接入实施结果（2026-11-08）

分支 `feat/terrain-p1b`，全部在 game-demo 模板里（`demo/`，[demo README“可编辑地形”](../../demo/README.md#可编辑地形p1)），`gameplay/terrain` 未改。P1 验收“改块重启后一致”在离线替身与真实 Mongo（DataEngine 全链路）上各验证一次。

**接法**：

| 部分 | 位置（生成后路径） | 做法 |
| --- | --- | --- |
| DAO | `db/def/terrain_chunk.go` | 按 §3：`BaseRevision` / `Version` / `Heights []uint16` / `Materials []byte` / `Applied []uint64`，store 键 `1`～`5`，`coll=terrain_chunks db=main`。前四个 `persist,sync`，`Applied` 用 `nosync` 只持久：同步视图的字段资格在 DAO 上一次定下，P2 接 entitysync 时 `Applied` 自然不下发 |
| 实体 | `game/entities/terrain_chunk` | kind 11（不复用已删的 5 / 8），`EntityCategoryWorld`（与 Scene 同档），持久；实体唯一号 = `ChunkKey.ID() + 1`（unique 0 无效，块 (0,0) 的 `ID()` 正是 0；地图每边 ≤ 1024 块，`ID()` < 2^42，远小于 52 位 unique）。ID 确定、可能已有记录，所以只经 lifecycle 的 `GetOrCreate` 建，不直接 `Create` |
| 组件 | `SurfaceComponent`（type 2011） | `Apply(baseRevision, base, edits)`：Version 0 时用原始块填满 DAO 再改，之后要求记录的 `BaseRevision` 等于当前包；高度只写变了的下标（`SetHeightsAt`），材质有变化时整字段（`SetMaterialsAll`）；每次成功 Version + 1；全部校验在第一次写之前。`Overlay()` / `OverlayOf(dao)` 给视图的副本；`RecordApplied` / `WasApplied` 是 `Applied` 的辅助（满 32 个丢最旧，整字段替换） |
| 地形服务 | `game/terrainmap`（`Map`） | 持有 `*terrain.Pack`、`*terrain.View`、本进程范围 `Region{Own, Load}`；`Load` 装块、`Edit` 写入口、`ApplyEdit` 是 handler 的业务、`Height` / `Material` / `IsWater` / `Probe` 委托视图 |
| 改块事务 | handler `TerrainEdit`（`rollback=undo durability=strict`） | 一块一笔；`ApplyEdit` 改 DAO 后 `nest.AfterCommit` 登记 `SetOverlay`（新数据的副本），提交被接受才进视图，回滚 / 提交被拒时不执行。handler 是包级函数，经 `terrainmap.Active()` 拿到本进程的 Map（与 `partition.Node()` 同一做法） |
| 服务接线 | `internal/service/<game>/terrain.go` | `Init` 里（World、Scene 之后）`startTerrain`：读配置 → 打开包 → 算范围 → `Load` → `Install` → 发布能力 `<game>.terrain`；`Shutdown` 最后 `Close`（撤下发布、关包） |
| 配置 | `game/settings/terrain.go` | `scene.terrain.pack`（可选，空 = 没有地形）；分区场景开着时 `Scene.ValidateConfig` 要求 `cell_size` 是 6400 的整数倍（加载配置时拒绝，doctor / `--check-config` 同一份） |
| 原始地形包 | `cmd/terrainpack` | `terrain.Generated` 写确定性小地图，缺省 2 × 1 块（128m × 64m，`BaseRevision` 1、水位线 400m），覆盖普通场景（1000 × 1000）与两个 6400 格的分区场景（P1 当时；2026-11-10 起缺省是 16 × 16 块的平地、`BaseRevision` 2，起伏地形要 `-shape generated`，见[第 25.6 节](#256-demo-缺省地形改为平地维护者-2026-11-10)） |

**装入（启动阶段）**：`Region.Load` 里的块（不分区：整张图；分区且本 sid 托管格：本格外扩 `ghost_band`；只做 Base：空）。哪些块有持久记录：DataEngine 没有“按集合列 ID”的正式能力（`OwnedEntityLister` 只给 Remote 所有权记录），这里用 DataEngine 的 `Store.Load`（与实体冷加载同一个读入口：逻辑库到物理库映射、墓碑过滤都在里面）按 `_id ∈ 候选块的实体 ID` 分批（每批 256）查，候选集就是本区域的块（≤ 本格 + Ghost 带块数），走 `_id` 索引，不扫全表；读出的记录用生成 DAO 的 `RestorePersisted` 解码（只接受当前 schema）后 `SetOverlay`，**不装成实体**——本格的块第一次被改时再由 `GetOrCreate` 冷加载，Ghost 带里邻格的块从不装成实体（不能在本进程持有别的格的可写实体）。其余块 8 个 goroutine 并行 `LoadBase`。时机在 `Service.Init`（App 启动阶段，不在 Nest 业务池）：此时 DataEngine 的 `Runtime.Start` 已排空历史 WAL（恢复屏障），一个 sid 一个进程、块只归本格写，库里就是最新已提交状态。

**BaseRevision 不一致**（P1 的保守做法；维护者 2026-11-08 已决定丢弃旧编辑，见 D13 与[第 14 节](#14-p2-服务端单格实施结果2026-11-08)，下面这段只对 P1 当时成立）：记录基于另一版原始地形的块不装入、`LoadStats.StaleBase` 计数、记 Error 日志，库里的记录不动；这些块查询返回 `ErrNotLoaded`，`Edit` 返回 `terrainmap.ErrUnloadable`（同时满足 `errors.Is(err, terrain.ErrNotLoaded)`）。记录损坏（解码失败、形状不对）与原始块损坏同样不装入，计入 `Corrupt`。

**改块（P1 最小写入口）**：`Map.Edit(ctx, []SampleEdit{GX, GY, Height, Material})`，按块分组、块坐标升序，每块：检查归本进程写（整块在 `Region.Own` 里）与记录可用 → `GetOrCreate`（第一次改的块建出空实体，DataEngine 新建准入门补一笔 strict 事务落库）→ `Sync_TerrainEdit`。块之间不原子，中途失败返回已提交的部分与错误。会等 Mongo 与 WAL，只能在 Nest 业务 worker 之外调用（运维命令、测试、I/O 池）。`Version 0` 的记录（建了实体、第一笔改动失败）装入时按原始块看待。

**测试命令与结果**（2026-11-08，Apple M5，Go 1.27，生成工程 replace 到本分支；生成步骤见 [demo README](../../demo/README.md#改这里的东西之后)）：

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| 组件规则 | `go test -race ./game/entities/terrain_chunk/` | 4 条通过：第一次改从原始块开始、ID 往返、拒绝不写、回滚还原 DAO、`Applied` 有界 |
| 改块 / 回滚 / 重启（离线） | `go test -race -run Terrain ./internal/service/<game>/` | `TestTerrainEditsSurviveARestart`：跨四块交点的 64 点改动 + (0,0) 第二次改动后查询即新值；提交器拒绝的改动（已改块与未改块的第一笔各一次）视图与实体都不变；停下后新实体管理器、新视图从“库”装入（4 块覆盖、2 块原始，其中 1 块是 Version 0 的空记录），整张图每 37 单位一个点共 179,574 个点的高度 / 材质 / 水与 6 块版本与重启前逐点一致，重启后接着改 Version 从 2 到 3。`TestTerrainRecordOnAnotherBaseRevisionIsNotLoaded`：包换成 `BaseRevision` 2 后 1 块 StaleBase、查询 `ErrNotLoaded`、改块 `ErrUnloadable`、库里记录逐字节不变。`TestTerrainRegionFollowsThePartition`：格 (0,0) 装 4 块（本格 + 3 块 Ghost）、只写本格、邻格块可读不可写；只做 Base 的 sid 不装块；`cell_size` 3000 + 地形在加载配置时拒绝，不开地形不受影响 |
| 重启（真实 Mongo） | `ROOST_TERRAIN_IT_MONGO_URI=… go test -race -run '^TestTerrainRestartRealMongo$' ./internal/service/<game>/`（`scripts/test-demo-generated.sh` 已挂入） | 私有 `scripts/mirror-local.sh` 副本集，唯一库名、结束删库。真实 MongoStore + 文件 WAL + Projector + Runtime（恢复屏障、冷加载、新建准入门）+ Nest：改 5 块后正常停机，库里恰好 5 条记录；同一 WAL 目录重启后 5 块覆盖 + 1 块原始，179,574 个点与 6 块版本逐点一致；重启后冷加载 (0,0) 接着改到 Version 3，停机后库里文档 Version 3、改过的点在 |
| 测试能变红 | 在生成工程副本上改坏实现后跑上面两条 | 提交前就 `SetOverlay`：离线用例报 `a rolled-back edit changed the view`；装入时不用记录：离线与真实 Mongo 都报 `Overlays:0`；装入的覆盖块改一个点：报第 0 个点高度 72640 ≠ 72650 |

**与本文不一致之处**：

- §4“`cell_size` 是块边长的整数倍”在 demo 里是加载配置时的校验，不是缺省值：demo 缺省 `cell_size` 3000（现有分区 / cellworld 测试与 `scripts/test-seamless-demo.sh` 都按它写），不开地形不受影响；开地形的分区场景要把格改成 6400 的倍数（README 有 `cell_size: 6400` 的示例）。§8.1 的 307200 仍是 1w 地图的推荐值。
- §3 `Applied`“定长环”实现为有界列表（满 32 个丢最旧、每次整字段替换），没有写游标字段；体积与写法与 §3 表格一致。
- 新建实体不是“第一次改块时用原始数据建实体”一步完成：handler 只能锁已存在的实体，所以先 `GetOrCreate` 建空实体（Version 0，单独一笔 strict 事务），改块事务里再用原始块填满。代价是第一次改一块多一笔提交；中间失败会留下 Version 0 的空记录，装入时按原始块看待。

**待维护者决定**：

1. **（已决定，D13：丢弃，第 14 节实现）** 原始地形换版（`BaseRevision` 变了）后旧编辑怎么办：P1 只做“不装入、计数、报错日志、不动记录”。可选：A 丢弃旧记录（玩家编辑丢失）；B 按旧编辑的差值重放到新原始块上（需要保留编辑日志或旧原始包）；C 保留旧记录覆盖新原始块（地形与新美术资源不衔接）。产品决定前，换版要求运维先处理这些记录。
2. **（已决定，D14：P3 推送，之前是启动快照；已实施，[第 21 节](#21-p3-ghost-副本t05d192026-11-09)）** Ghost 带里邻格的块只在启动时读一次快照，之后邻格改了不更新（P3 的边界块推送）；P1 是否需要在 P3 之前就禁止依赖 Ghost 带块的查询结果（例如跨格移动的高度判断）。

**给 P2 的接口说明**：

- 多块事务：P2 的笔刷 handler 声明至多 4 个 `ISurfaceEntity` 目标（同为 World 档，按 ID 取锁），对每块调 `SurfaceComponent.Apply` 后逐块 `nest.AfterCommit` 交给 `Map`（`ApplyEdit` 里的提交后装视图要提成可复用的一步）。目标块第一次改时仍要先 `GetOrCreate`。
- 幂等：`WasApplied(editID)` 判重、`RecordApplied(editID)` 与改动同一事务写；子 ID 派生规则（§4 跨格拆分）P2 定。
- 整字段守卫：在 `Apply` 里按本块改动点数选 `SetHeightsAt` 或 `SetHeightsAll`（阈值 1000）。
- 同步：实体加 `sync=true` 与地形 Interest；DAO 字段资格已定（`Applied` 不同步）。
- 错误码：P1 的拒绝是包内哨兵（`terrainmap.ErrDisabled` / `ErrNotOwned` / `ErrUnloadable`、`terrain.ErrOutOfMap` / `ErrInvalidChunk` / `ErrBaseRevision` / `ErrNotLoaded`），客户端码 P2 接协议时定。

**未验证**：崩溃重启（WAL 里有未投影记录时 `Load` 读到的是排空之后的库：由 DataEngine 的恢复屏障保证，本轮没有单独的 SIGKILL 演练）；真实 game 进程实跑（配置 + `cmd/terrainpack` + `make dev-run`）；1w 地图规模下按格装入的耗时与内存（§11 的数字是 `gameplay/terrain` 单独测的，没有经过 `Store` 读记录这一步）；Linux。

## 13. P2 客户端（C#）实施结果（2026-11-08）

分支 `feat/terrain-p2-client`，只做 C# 客户端：读原始地形包、地形视图与查询、接 DAO 同步、mesh 重建钩子。服务端的笔刷、校验、多块事务与 entitysync 接入是 P2 的另一部分，本轮没有改 `gameplay/`、`demo/`、`codegen/`。用法见[客户端手册“地形”](../../client/README.md#地形可编辑地形客户端)。

**位置**：`client/dotnet/Roost.Client/Terrain/`，命名空间 `Roost.Client.Terrain`，随 `Roost.Client.dll`（`netstandard2.1`、C# 8）进 Unity 包，不新增程序集与依赖，只用 BCL 的 `DeflateStream`（raw DEFLATE）与 `SHA256`。测试是独立项目 `client/dotnet/Roost.Client.Terrain.Tests`（TerrainChunk 的生成物与 `Roost.Client.Tests` 里 codegen 金样的生成物同一 proto 包、同名静态类，不能编进同一个程序集），已挂进 `scripts/test-client-dotnet.sh`（pretag 的金样模式也跑）。

**API 摘要**：

| 部分 | 入口 | 与 Go 的对应 |
| --- | --- | --- |
| 几何 | `TerrainGeometry`（常量、`ChunkOf`、`PointIndex`）、`ChunkKey`（`Id` / `FromId`）、`TerrainMeta` | `coord.go` 同名常量与函数，负坐标向负无穷取整、远处饱和 |
| 原始地形包 | `TerrainPack.Open(ReadOnlyMemory<byte>)` / `Open(Stream, leaveOpen)`、`ReadChunk`、`ChunkSha256`、`Meta`、`Dispose` | `OpenPack` / `ReadChunk` / `ChunkSum`；包头字段的检查顺序与 `parseHeader` 相同，同一个坏包报同一类错误 |
| 地形视图 | `TerrainView`：`LoadBase`、`SetOverlay(key, baseRevision, version, heights, materials)`（`ushort` span 或 DAO 的 `IReadOnlyList<uint>`）、`Unload`、`TryGetVersion`、`GetChunk`（不可变、不复制）、`Count` | `View` 的写入口与拒绝规则（形状、`Version ≥ 1`、`BaseRevision`、版本单调、相同则替换） |
| 查询 | `Height` / `Material` / `IsWater` / `Probe`；`TryProbe` 返回 `Ok` / `OutOfMap` / `NotLoaded`，不抛异常 | `probe` 逐行移植：64 位整数、权重为 0 不读、边缘复制、缺块报第一个缺的块（读取次序相同） |
| mesh 钩子 | `ChunkChanged`（`Loaded` / `Updated` / `Unloaded`，前后版本，`MeshesToRebuild`）、`TryBuildHeightGrid`、纯函数 `TerrainMesh.TryBuildHeightGrid` / `AffectedChunks` | 顶点 = 采样点上的 `Height`；邻块规则同查询 |

**与 Go 的一致性证据**（2026-11-08，Apple M5，.NET SDK 10.0.302，测试程序 `net10.0`、运行库 `netstandard2.1`）：

| 用例 | 结果 |
| --- | --- |
| 金样 `client/spec/terrain-golden.pack` + `terrain-queries.json` | 整包 SHA-256、6 块存储字节与解码后高度 / 材质的 SHA-256 全部一致；246 个位置（含块边界、四块交点、地图边缘、地图外）的高度 / 材质 / 水全部一致；字节与 Stream 两种打开方式各跑一遍 |
| DAO 同步夹具（`Fixtures/terrain-chunk-sync.json`） | `regen.sh` 用正式生成器从 `demo/db/def/terrain_chunk.go.tmpl` 生成 Go DAO、`dao_sync.proto`、`dao_sync_apply.cs`，Go 侧在 Nest 隔离事务里改块并取 entitysync 会下发的字节：全量（12,318 字节）、块左边 r=5m 逐点 `SetHeightsAt`（list delta，390 字节）、`SetMaterialsAll`（4,116 字节）、`SetHeightsAll`（整字段，8,212 字节）。C# 生成 Apply 每步合并结果与 Go 的合并结果 `Equals`，交给 `TerrainView` 后 4 × 130 = 520 个位置与 Go `View` 的查询逐点一致，每步收到对应版本的 `ChunkChanged`、`MeshesToRebuild` 含左邻块 |
| 网格 | 6 块的 65 × 65 网格里 25,026 个地图内顶点逐个等于 `Height`；相邻块共享边逐个相同；编辑块 (1,0) 的第 0 列后按 `MeshesToRebuild` 重建，块 (0,0) 的右边跟着变、接缝仍连续 |
| 坏包 / 坏块 | magic、版本、几何常量、codec、索引位置、块数、`HeightBase` 溢出、保留字节与索引被改（整体校验和）、索引项越界（重新封口后）、6 种截断；块数据翻转一位（只坏这一块，其他块照常可读）、Stream 读不满、明文多 / 少一字节、高度越界、varint 越界或不完整、明文超长、非 DEFLATE、截断的 DEFLATE、空 DEFLATE |
| 覆盖块与并发 | 拒绝规则逐条、拒绝后视图不变、入参被复制、同版本同数据不通知、订阅者抛错交给 `Error`；4 个查询线程在约 2000 次版本切换中读 3 万次左右，没有读到混用两个版本的结果 |
| 测试能变红 | 逐个改坏实现后跑：去掉四舍五入报 `query (50, 50): got 60472, Go 60473`；去掉边缘复制或读权重为 0 的邻点报 `chunk not loaded`；不校验块 / 整体 SHA-256、接受多余材质字节、不查 `BaseRevision`、不查 uint32 上限各自报 `accepted …`；同版本也拒绝报 `stale overlay`；总是通知报 `repeated full`；网格不读右邻块报 `vertex (0,0) [64,0] equals Height`；上边不复制报 `top map edge replicated` |

**生成器 / SDK**：当前 C# 生成 Apply 对 `repeated uint32`（来自 `[]uint16`）的 list delta 合并正确，上面的夹具实际走过 `Delta = true`、`Updates` 按下标、`Length = 4096` 的路径，没有发现问题。代价是每个增量都克隆一次整份视图并逐个重建 4096 个元素的列表（生成 Apply 的通用做法），耗时没有测。夹具的 proto / C# Apply 取自 demo 的 DAO 定义，P2 服务端若改 `TerrainChunkDao` 或生成器，`regen.sh`（核对模式）会报漂移，需要 `-update` 后复跑 C# 测试。

**方案没覆盖、本轮先选的**：

1. 放进 `Roost.Client` 而不是新程序集：Unity 打包脚本只构建 `Roost.Client.dll`，不用改打包与 asmdef。
2. 实体 ID → 块坐标的映射不进 SDK（是游戏工程的约定，game-demo 是唯一号 = `ChunkKey.Id + 1`），SDK 只给 `ChunkKey.Id` / `FromId`。
3. `Version 0` 的记录（服务端建了实体、第一笔改动还没提交，数据为空）在接线示例里跳过、保持原始块，与服务端装入规则一致；`SetOverlay` 本身仍按 Go 拒绝 `Version < 1`。
4. 块离开地形 Interest（`Remove`）时示例保留已装的覆盖块，不改回原始块；按视距卸下后再装入时是原始块，直到再次收到全量（方案 D12“没有客户端缓存”的显示代价，客户端手册已写）。
5. `ChunkChanged` 只在数据或版本真的变了时发（同版本同数据的重复全量不发，免得重连 / 重新进入视野时整片重建 mesh）；在写入口的调用线程、锁外、新数据可查询之后触发；订阅者抛错交给 `Error`（与 `GameClient` 的做法相同）。
6. 网格输出世界高度（`int` 厘米），65 × 65，缺邻块返回 false 不猜；网格三角形在格内是平面、查询是双线性，格内可能差几厘米，判定类逻辑用查询。
7. 增加不抛异常的 `TryProbe` / `TryBuildHeightGrid`，给每帧热路径用；抛异常的入口与 Go 的错误一一对应。

**未验证**：Unity Editor、Mono / IL2CPP Player 与移动设备（`DeflateStream`、`SHA256` 在 IL2CPP 上的行为与耗时）；真实 entitysync 链路（服务端 `TerrainChunk` 还没接 entitysync，本轮用 Go 生成 DAO 写出的字节代替）；Linux 上的 .NET；1w 地图规模下 C# 的装块耗时、查询耗时与内存；C++ 客户端（不在本轮）。

## 14. P2 服务端（单格）实施结果（2026-11-08）

分支 `feat/terrain-p2-server`。纯计算在 `gameplay/terrain/brush.go`（[包说明“笔刷”](../../gameplay/terrain/README.md#笔刷p2brushgo客户端须逐位相同)），其余在 game-demo 模板里（[demo README“玩家编辑与同步”](../../demo/README.md#玩家编辑与同步p2)）。`gameplay/terrain` 已有的公开语义没有改，只新增。玩家进程 = 格进程（单格；跨格转发是 P3）。实测见[证据目录](evidence/terrain-p2-20261108/README.md)。

**接法**：

| 部分 | 位置（生成后路径） | 做法 |
| --- | --- | --- |
| 笔刷 | `gameplay/terrain/brush.go` | `Brush{Op, X, Y, Radius, Falloff, Strength, Target, Material}`，抬高 / 降低 / 整平 / 改材质，衰减常数 / 线性 / 平滑，全整数（权重刻度 65536、`ISqrt`）；`Chunks(meta)` 是覆盖到的块（按 (Y, X)），`Apply` 返回每块改了值的点（下标升序）；夹在 `[Bedrock, Ceiling]`，已在限外的点不往外推；共享边按 §2 归右 / 上的块，每个采样点只改一次。客户端金样 `client/spec/terrain-brush.json`（17 例） |
| 编辑入口 | `game/terrainmap`：`Map.Brush(ctx, playerID, requestID, brush)` | 顺序：参数与 `Policy`（半径 ≤ 1600、≤ 4 块、强度 ≤ 50、高度 `[0, 16383]`）→ 每玩家令牌桶（2/s；第 17 节起改为改动点数配额，放在权限钩子之前）→ `Rules.AllowEdit`（领地 / 权限钩子，缺省放行）→ 第一次改的块 `GetOrCreate` → 一笔多块事务。前三步拒绝时零写入；`ErrBrush` / `ErrRateLimited` / `ErrDenied` / `ErrNotOwned` / `ErrUnloadable` / `ErrOccupied` 各自可 `errors.Is` |
| 多块事务 | handler `TerrainBrush(targets []ISurfaceEntity, editID, brush)`（`rollback=undo durability=strict`） | 生成的 `MultiGroupSync_TerrainBrush`，Nest 排序取锁。`Map.ApplyBrush`：任一块已记过这个编辑 ID → 整笔按重复返回；核对目标正是 `brush.Chunks`、都归本进程写且可用；取编辑前数据（改过的块取 DAO，没改过的取原始块）→ `brush.Apply` → `Rules.Occupied`（建筑占位钩子，命中则整笔返回 `ErrOccupied`、回滚）→ 每块 `SurfaceComponent.Apply` → 每块 `RecordApplied`（锁住的每一块都记）。提交后装视图是可复用的一步 `Map.afterCommit`（P1 的 `ApplyEdit` 也改用它）：`nest.AfterCommit` 里 `SetOverlay` 并通知地形同步登记 subject |
| 幂等（D18 已取代，见第 20 节） | `terrainmap.EditID(playerID, requestID)` | splitmix64 混合成 63 位（DAO 的 `[]uint64` 存成 BSON int64，最高位置位的值在持久化准备阶段被拒，整笔失败）。重复返回 `Duplicate = true` 与各块当前版本（不是第一次的结果），不再改 |
| 整字段守卫 | `SurfaceComponent.Apply` | 本块改了值的高度点数 > `HeightsGuard`（1000）时 `SetHeightsAll`，否则对变了的下标 `SetHeightsAt`；材质有变化时整字段 `SetMaterialsAll`（第 16 节起材质也逐点写，另有材质守卫，第 17 节起阈值 500）；写完 `PublishSyncDirty` |
| 协议与错误码 | `protocol/def/terrain_edit.go`、`game/controllers/player/terrain_edit.go`、`internal/errors/terrain.go` | `TerrainEdit` msgid 10038；线上整数越出字段类型范围直接拒绝（不截断）。业务码 100052 `terrain_disabled`、100053 `terrain_brush`、100054 `terrain_not_owned`、100055 `terrain_unavailable`、100056 `terrain_rate`、100057 `terrain_denied`、100058 `terrain_occupied`；其余错误答 internal |
| 同步对象 | `game/entities/terrain_chunk`：`sync=true syncNamespace="terrain" subjectPacker=NewTerrainChunkSyncPacker` | packer 只有缺省视图，字段是 DAO 上标了 sync 的四个（`Applied` 不在里面）；全量 `MarshalSync`，增量 `MarshalSyncChanges`（变化树：按下标的高度增量、整字段替换），实现 `SubjectSyncChangePacker`。demo 的 Nest 没有 EntitySync 观察者，提交不自己标 subject，所以与 Player / Monster 一样由 `PublishSyncDirty` 标脏（取客户端专用掩码 `TakeEntitySyncChanges`，不动服务间同步掩码） |
| 地形 Interest | `internal/service/<game>/terrain_sync.go`、`scene.go` | 场景 Manager 上另注册一个 `policy.Interest`（独立订阅来源），玩家主视野是它的观察者：`Join` 时 `Watch`、移动提交后 `MoveView`（离上次报告 ≥ 8m 才报）、离开时 `Unwatch`；进入半径 200m、离开 220m、`MaxVisible` 64；只有改过的块是 subject（显示在块中心）。开地形时 `frame.Limits.MaxObjects` = 100 + 64，`SnapshotBudget.MaxSnapshotsPerSessionPerFlush` = 4。只在普通场景开（分区场景的地形同步随 P3） |
| 启动时的 subject | `terrainSync.loadEdited`（`startTerrainSync`，`Map.Load` 之后） | P1 的 `Load` 只把记录读进视图、不装实体；同步要实体的 `SubjectSyncState`，所以本格改过的块（`Map.EditedOwned`）经 `GetOrCreate` 冷加载（8 路并行）并登记。之后第一次改的块在提交后登记（`Map.SetCommitted`）。Ghost 带里邻格的块不登记 |
| 换版丢弃（D13） | `Map.Load` + handler `TerrainDiscard` + `SurfaceComponent.Discard` | 本格的块：记录 `BaseRevision` ≠ 包 → `GetOrCreate` + 一笔 strict 事务把 DAO 重置为 Version 0（`BaseRevision` 0、数据清空、`Applied` 保留）→ `LoadBase`，`LoadStats.Discarded` 计数、Warn 日志（块、旧 / 新版本、旧 Version）。事务失败：不装入、`markUnloadable`（查询 `ErrNotLoaded`、编辑 `ErrUnloadable`）、`StaleBase` 计数、Error 日志，记录不动。邻格 Ghost 带里的旧记录：不装入、不动，`StaleBase` 计数 |

**为什么丢弃是“重置为 Version 0”而不是删档**（维护者 2026-11-08 接受）：块的实体 ID 由坐标确定，DataEngine 删档会留下墓碑，删过档的 ID 不能再 `GetOrCreate`（答 entity_deleted），这块就永远不能再改。重置走的是同一条正式路径（Nest strict 事务 → WAL → 投影），P1 早已规定 Version 0 的记录按原始块看待，重启后也一样。代价：库里留一条约 300 字节的空记录（含 `Applied`）。

**Ghost 带里邻格的旧记录怎么显示**：选“不装入”（查询答 `ErrNotLoaded`）。约束是不双写、不越权删：本进程既不能改它，也不能替它的格丢弃；按旧记录显示是已作废的数据，按原始块显示可能与它的格在丢弃前仍在服务的数据不一致。它的格丢弃后，本进程要到下次启动才读到（Ghost 副本是启动快照，D14）。

**测试命令与结果**（2026-11-08，Apple M5，go1.27.3，生成工程 replace 到本分支）：

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| 笔刷算法与金样 | `go test -race ./gameplay/terrain/` | `ISqrt`、三种衰减的权重、四块交点每点只改一次（177 点）、夹到基岩 / 上限且不往外推、常数整平幂等、确定性且不改入参、参数拒绝、玩家半径至多 4 块、`TestClientBrushGolden` |
| 组件 | `go test -race ./game/entities/terrain_chunk/` | 新增：整字段守卫（1001 点整字段、两笔各 ≤ 1000 点逐点，两条路径数据相同，值没变的点不写）、`Discard` 只重置仍基于旧版本的记录 |
| 编辑、幂等、回滚 | `go test -race -run Terrain ./internal/service/<game>/` | `TestTerrainBrushRepeatsAndReordersApplyOnce`（四块交点的编辑：同一请求重放、与后一个请求乱序到达、另一个玩家的同一请求 ID，各只生效一次，重放不改任何块，每块都记了编辑 ID，重启后重放仍是重复，最终视图等于纯计算按生效顺序叠加的结果）；`TestTerrainBrushRefusalsWriteNothing`（请求 ID 为 0、参数、半径 / 强度 / 高度上限、地图外、没覆盖到采样点、不归本格、权限钩子、建筑占位钩子，每种拒绝后所有块的实体、`Applied` 与视图逐点不变、没有提交）；`TestTerrainBrushRateLimit`；`TestTerrainBrushRollbackLeavesEveryChunkAndTheView`（提交器拒绝的四块编辑后，已改与未改的块、视图都不变，同一请求再来一次生效）；`TestTerrainBrushOverTheGuardWritesTheWholeField` |
| 换版丢弃 | 同上 | `TestTerrainRecordOnAnotherBaseRevisionIsDiscarded`（替换 P1 的 `…IsNotLoaded`）：分区 3 × 2 格的格 (0,0)：本格旧记录丢弃（`Discarded` 1），邻格的不装入（`StaleBase` 1）、库里不动；注入事务拒绝时这块不装入、查询 `ErrNotLoaded`、编辑 `ErrUnloadable`、库里记录逐字节不变；重启后这块是新原始块、再改从 Version 1 开始，再重启是覆盖块 |
| 真实 TCP | 同上，`terrain_sync_test.go` | `TestTerrainChunksReachAClientOverRealTCPAndMatchAfterAReconnect`：视野内 3 块改过的块以全量到达、视野外的不到；r=5m 抬高是按下标的高度增量、改材质是材质整字段、守卫整平是高度整字段；断线后改两块、新改一块，重连后客户端持有恰好这 4 块，版本与 4096 点高度 / 材质逐点等于服务端，`SessionsLost` 0。`TestATerrainViewAtItsCapKeepsTheSession`：视野 500m 内改过 100 块、另有 40 个怪，客户端持有恰好 64 块 + 41 个其他对象，连接仍在、`SessionsLost` 0，28 次 Flush 发完（快照预算生效；D15 之后：41 个其他对象第一次 Flush 全部到达，17 次 Flush 发完，见第 15 节） |
| 真实 Mongo | `ROOST_TERRAIN_IT_MONGO_URI=… go test -race -run '^TestTerrainRestartRealMongo$' ./internal/service/<game>/` | 私有 `scripts/mirror-local.sh` 副本集：P1 的重启一致之外，换成 `BaseRevision` 2 的包重启：5 块丢弃，库里 (0,0) 是 Version 0、没有高度；再重启 `Discarded` 0、6 块全是原始块 |
| 端点 | `go test -race -run Terrain ./game/controllers/player/` | 没配地形答 `terrain_disabled`；越界的线上值拒绝；每种拒绝对应业务码，基础设施错误答 internal |
| 测试能变红 | 在生成工程副本上改坏实现后跑 | 不查 `WasApplied`：重放得到 `Duplicate:false`、各块 Version 2；去掉整字段守卫：组件用例报 `delta=true`，TCP 用例报守卫整平以 1793 点的增量（14,367 字节）发出；提交前就装视图：回滚用例报 `view version 1 → 2`；不丢弃：报 `Discarded:0 StaleBase:2`；不 `PublishSyncDirty`：增量不到达（`DirtyCaptured` 0）；`MaxObjects` 留在 100：会话以 `object_limit` 断开 |

**实测**（离线提交器、单观察者，方法与边界见[证据目录](evidence/terrain-p2-20261108/README.md)）：

| 项 | 结果 | 方案 |
| --- | --- | --- |
| r=5m 单块高度增量（69～81 点） | DAO payload 575～671 字节，线上 719～815 字节 | 约 0.7KB |
| 改材质 r=5m | payload 4,117 字节，线上 4,261 字节 | 约 4.1KB |
| 守卫整字段 / 进入视野全量 | 8,213 / 12,319 字节（payload） | 8.2KB / 12.3KB |
| 热点（20 人各 1 次/秒）：材质 0 / 50% / 100% | 19.2 / 75.5 / 132.6 KB/s（线上） | 约 17 / 70 / 124 KB/s |
| 跑图 6m/s：全改过 / 改过 20%；12m/s 全改过 | 8.1 / 1.6 KB/s；16.6 KB/s | 约 8.6 / 1.7 KB/s |
| `Map.Brush` 端到端（Nest short 池 4 worker，含排队与交接，不含 WAL） | r=5m 单块 P50 约 75µs、P99 约 0.2ms；r=16m 四块交点 P50 约 0.56ms；守卫整平 P50 约 0.18ms | — |
| 单格吞吐（8 人各改不同块） | 约 3.8～4.5 万次/秒（离线提交器） | — |
| 笔刷纯计算 | r=5m 2.6µs；r=16m 四块 15.7µs | — |

**与本文不一致之处**：

- §7“视野 200m 约覆盖 50～60 块”算错了：π·200² / 64² ≈ 31 块（已在 §7、§7.1、§8.1 更正）。缺省视野里改过的块到不了 `MaxVisible` 64，验收用例放大视野到 500m 才测到上限；登录到全改过区域约 380KB 而不是 600KB。
- §4“提交并持久确认后由 entitysync 下发”：demo 的 Nest 没有装 EntitySync 观察者，脏标记由业务 `PublishSyncDirty` 交给 subject（Player / Monster 同一做法）；持久化门槛仍由场景 Manager 的 `DurableLSN` 水位按 `CommitLSN` 挡（生产路径与 P1 相同），但本轮的测试场景没有接水位，持久门槛没有在地形上单独验证。事务回滚时 subject 仍是脏的，下一次 Flush 多发一个按当前数据的增量（安全方向）。
- §3 删档：换版丢弃不删档，重置为 Version 0（理由见上）。
- `SnapshotBudget` 是 Manager 级配置，开地形后对同一场景的玩家、怪的进入视野全量也生效（每会话每 Flush 4 个）；不开地形的场景不变。（已按 D15 改为地形单独计，见第 15 节。）
- 去重的回复是“重复 + 当前版本”，不是第一次的结果（第一次的结果没有存）。

**方案没覆盖、本轮先选的**：

1. `Policy` 的数值：强度上限 50（5m）、基岩 0、上限 16383（原始值 ≤ 16383 时 PB varint 2 字节）；整平目标必须在范围内。
2. 频率限制放在 `Map` 里（进程内令牌桶，容量 = 每秒次数，重启清零，重复请求也占频率；第 17 节起改为改动点数配额，位置不变）：玩家进程 = 格进程，这里是唯一入口；P3 拆成 Base / Real 后要挪到 Base。
3. 编辑 ID 的派生 `splitmix64(playerID ⊕ splitmix64(requestID))` 取 63 位；跨格子 ID 预留为对（玩家，请求 ID ⊕ 格号）同一函数（P5 定）。
4. 启动时把本格改过的块装成实体（每块多一份约 12KB 的 DAO 加变化树；1w 地图一个格全改过即 2304 块约 30MB、启动多 2304 次按 `_id` 读，耗时按 §11 的单块读推算为几百毫秒，**没有实测**）。备选是只给进入某玩家视野的块按需装实体，要改订阅侧，P5 规模测试时再看。
5. 第一次改的块在提交后才登记为 subject（提交前没有可同步的数据）；登记失败只记 Error 日志，玩家要到重启后才看得到这块（没有重试）。
6. 地形视野不跟玩家的 `maxVisible`（主视野的）走，固定 64；视点按 8m 节流。
7. 没有给机器人（loadtest）加编辑动作：地形缺省关闭，压测配置里也没有地形包。

**D11 / D12 复议建议**（不改决定；D11 已按预算复议为 `[]uint16`，见[第 16 节](#16-材质改为列表按下标增量实施结果2026-11-08)）：

- **D11（材质整字段）**：实测证实热点下行几乎全由改材质决定：全部带材质时每观察者约 133KB/s，是只改高度的 7 倍；按下标增量的材质（每点约 8 字节，r=5m 约 0.65KB）能把这一档降到约 35KB/s。建议：先定客户端下行预算与“挖填是否总伴随改材质”的玩法前提；如果挖填默认改材质（例如挖开露出泥土）或预算低于约 100KB/s，就换成 `[]uint16` 列表（未上线，换类型不需要兼容）。如果改材质是低频的单独操作（刷路、铺地），保持 `[]byte`。
- **D12（客户端版本缓存）**：跑图加载 6m/s 全改过 8KB/s、12m/s 17KB/s，登录 / 传送约 380KB 由快照预算在约 0.4s 内发完，都远小于热点里改材质的量。建议维持“不做”，到出现高速移动（> 20m/s）、频繁传送或加大视野半径时再做。

**待维护者决定**：

1. **（已决定：维护者 2026-11-08 接受）** 换版丢弃用“重置为 Version 0”代替删档（上文理由）。如果要真删，需要 DataEngine 允许确定性 ID 删档后重建（框架改动），或块实体 ID 带上 `BaseRevision`（换版即换 ID，旧记录另行清理）。
2. 开地形后会话对象上限变成 164：客户端的帧解码上限须 ≥ 164（C# `SyncFrame` 缺省 `maxObjects = 100`，按 2 倍容差可解 200 个对象的帧，现在能过；建议客户端显式传 164）；`SnapshotBudget` 对玩家、怪同时生效，是否接受。（已决定，D15：不接受；地形单独计上限与预算，帧仍按 100 切，客户端帧解码上限不用改，见第 15 节。）
3. **（已决定：低于 50KB/s，维护者 2026-11-08；D11 据此改为列表，第 16 节）** 客户端下行预算（D11 复议的前提）。

**给 P3 的接口说明**：

- 跨格：`Map.Brush` 现在要求覆盖到的块全归本进程（否则 `ErrNotOwned`）；P3 在 Base 侧按块的归属拆成每格一个子请求（子 ID 用 `EditID` 同一函数派生），各格独立走 `Brush` 的事务部分（`ApplyBrush`），频率与权限在 Base 侧做一次。（D18 起编辑身份改为（玩家, 序号）、`EditID` 已删除，跨格的账本安排见[第 20 节](#20-编辑请求身份与判重t01t02d182026-11-09)“剩余边界”。）
- Ghost 推送（D14）：本格提交后的通知入口是 `Map.SetCommitted`（目前只接了地形同步）；推送可以挂在同一处，邻格收到后 `View.SetOverlay`（版本单调）。邻格换版丢弃后的空记录（Version 0）需要一条“回到原始块”的推送（视图目前没有“撤下覆盖块”的写入口，要加）。
- 分区场景的地形同步：`terrainSync` 现在只挂在普通场景的 Manager 上；分区场景要在每个格的复制管理器上各挂一个，并接上 Ghost 带块的 subject。
- 移动 / 寻路：查询仍是 `Map.Height` / `Material` / `IsWater` / `Probe`（无锁、handler 里可调），P3 起改过的块按 `View.Chunk` 的版本失效寻路缓存。

**未验证**：真实 WAL / Mongo 下的单笔编辑延迟与吞吐，以及持久门槛对地形块的实际挡住（测试场景没接 `DurableLSN`）；多观察者同时在热点时的服务端 CPU 与总出口；真实 game 进程实跑（配置地形包 + 客户端发 `TerrainEdit`）；C# 客户端的笔刷同算（金样已备好）与真实 entitysync 链路上的 C# 接收；四块交点编辑比单块慢约 7 倍的原因（CPU 剖析里 Nest 只占约 10%，疑为多实体请求的分派交接，没有细查）；启动时装实体的耗时与内存（第 4 条只是推算）；Linux。

## 15. 地形单独计对象上限与快照预算（D15，2026-11-08）

维护者决定：“会话对象上限 164 和全量分摊这两个设置，不能影响玩家和怪的同步。”P2 为了地形把场景 Manager 的 `frame.Limits.MaxObjects` 调到 164、打开 Manager 级 `SnapshotBudget{PerSessionObjects: 4}`，两者都作用于整个 Manager。

**框架**（`framework/sync/entitysync`，契约见 [ENTITY_SYNC“按命名空间分开计”](../framework/ENTITY_SYNC.md#按命名空间分开计namespaces)）：`ManagerConfig.Namespaces map[string]NamespaceLimits{MaxObjects, SnapshotBudget}` 按 subject 的命名空间单独计会话持有上限与快照预算，没列出的命名空间沿用 `Limits.MaxObjects` 与 Manager 级 `SnapshotBudget`；不配置时逐字节不变。

- 分类键选命名空间而不是订阅来源：命名空间是对象自己的属性、随每个组件下发、是客户端分流键；一个对象可经几个来源订阅，按来源分会让“它算哪一类”取决于订阅顺序。
- 超出一类的上限：与既有契约相同，关闭整个会话（`object_limit`，错误点名命名空间），不只拒该类的新对象（那样订阅会停在“要全量”却永远发不出，等于静默截断）。持有量由地形 Interest 的 `MaxVisible`（64）保证。**D17 起改为超额降级、不关会话（[第 18 节](#18-超额降级减少同步对象不断开d172026-11-08)）**；这里保留 D15 实施时的口径。
- 帧：每帧对象数仍按 `Limits.MaxObjects`（100）切分，只有引用表上限是各类之和（164）。客户端（Go `frame.Receiver`、C# `SyncFrame.Decode`）的帧上限不用改；显式设对象表容量（C# `EntityViews` 的 `maxObjects`，缺省 10000）的不得小于 164。
- 预算：每个列出的命名空间一路（`snapshotLane`：额度、窗口内计数、会话 / 来源游标、待快照索引、字节阻挡各一份），窗口起点共用；会话里每路一份实体轮转序号。

**demo**：场景 Manager 的 `Limits` 与 `SnapshotBudget` 回到零值（100、不限），`Namespaces: terrainNamespaceLimits()`（`terrain`：64、每会话每 Flush 4 个）。

**验证**：entitysync 新增 8 个用例（`manager_test.go` 5 个：不相关命名空间逐字节不变、每类各自计与超限关会话、Hold/Ready / 关闭重开 / 退役后重登记、配置校验、并发 churn（-race）；`snapshot_budget_test.go` 3 个：两路互不占额度（periodic / on_change × 四种组合）、每路各自轮转与字节阻挡、RetryLater 下每路按实际尝试计费）；变异（把所有命名空间归到类别 0）后 8 个全红。生成工程新增 3 个用例并改 1 个：`TestTerrainLeavesPlayerAndMonsterReplicationByteIdentical`（开 / 不开地形时观察者的帧逐字节相同：第一次 Flush 15 个对象、remove、Hold/Ready 全量）、`TestTerrainDoesNotWidenThePlayerObjectLimit`（开地形时 101 人挤一起仍全部 `object_limit` 断开；D17 起改为玩家一类降级到 100、不断开）、`TestTerrainChunksOverTheirLimitCloseTheSession`（地形视野配成 80 > 64：第 65 块时关会话、断开连接；D17 起由 `TestTerrainChunksOverTheirLimitShrinkTheView` 取代）；`TestATerrainViewAtItsCapKeepsTheSession` 的预算断言按 D15 改为“41 个其他对象第一次 Flush 全部到达、地形至多 4 块，至少 16 次 Flush”（原断言“105 个对象至少 27 次 Flush”钉的是被否决的 Manager 级预算）。这 4 个用例在 P2 代码（b20f439d）上全红、在本实现上全绿，原文与基准对照见 [evidence/sync-namespace-limits-20261108](evidence/sync-namespace-limits-20261108/README.md)。

## 16. 材质改为列表按下标增量实施结果（2026-11-08）

分支 `feat/terrain-materials-list`（基于 b20f439d）。D11 复议：维护者 2026-11-08 定客户端下行预算 **低于 50KB/s**；P2 实测热点每个观察者只改高度 19KB/s、一半带材质 76KB/s、全部带材质 133KB/s，材质整字段（每次 4.1KB）超预算，所以材质也按下标增量。`gameplay/terrain`（`Chunk.Materials []byte`、原始地形包格式）不变，只改 game-demo 与 C# 客户端。实测见[证据目录](evidence/terrain-materials-list-20261108/README.md)。

**改动**：

| 部分 | 位置（生成后路径） | 做法 |
| --- | --- | --- |
| DAO | `db/def/terrain_chunk.go` | `Materials []uint16`，store 键仍是 `4`；`schema=2`。生成期 WARN（`stored key "4" changes type []byte -> []uint16 ... schema= was raised from 1 to 2`）按 [TAGS 手册](../../codegen/docs/TAGS.zh-CN.md#必填的-store-键与变更告警v1400-起) 与 [schema 变更手册](../framework/SCHEMA-MIGRATION.md) 处理：项目未上线，不写迁移，提高 schema 让旧形状的文档以 `ErrSchemaMismatch` 被拒绝（而不是解码失败或读一半）；升级须清空 `terrain_chunks`。没有删生成基线、没有重编号 |
| 组件边界 | `game/entities/terrain_chunk`：`SurfaceComponent.Apply` / `Data` / `Overlay`、`OverlayOf` | DAO ↔ terrain 的换算只在这里：写入时每点 1 字节放宽成 `uint16`；读出（`Data`、`Overlay`、`OverlayOf`）时换回 1 字节，有值 > 255 返回 `terrain.ErrInvalidChunk`，不截断。`Data` / `Overlay` / `OverlayOf` 多了 error 返回值；`Map.Load` 把这样的记录计入 `Corrupt`、不装入（查询 `ErrNotLoaded`、编辑 `ErrUnloadable`）；`Apply` 在写之前检查记录的材质值域 |
| 整字段守卫 | `SurfaceComponent.Apply` | 高度、材质各自计数：本块改了值的材质点数 > `MaterialsGuard`（1000；第 17 节起 500）时 `SetMaterialsAll`，否则对变了的下标 `SetMaterialsAt`，与高度守卫同一写法。`Apply` 改为只看改动涉及的点（去重、同一下标取最后一个），不再整块复制比较 |
| 同步 / 协议 | 生成的 `dao_sync.proto` | `SyncTerrainChunkDaoMaterialsField` 从 `bytes value` 变为与高度相同的 `repeated uint32 value` + `delta` / `updates` / `length`。packer、地形 Interest、Manager 配置都没改 |
| C# 客户端 | `Roost.Client.Terrain.TerrainView` | `SetOverlay(key, baseRevision, version, IReadOnlyList<uint> heights, IReadOnlyList<uint> materials)` 取代原来的 `(IReadOnlyList<uint>, ReadOnlySpan<byte>)` 重载（未上线，不保留旧重载）；高度 > 65535、材质 > 255 抛 `TerrainInvalidChunkException`，视图不变。`(ReadOnlySpan<ushort>, ReadOnlySpan<byte>)` 重载不变 |

**节点预算（两个列表共享）的结论**：守卫按字段各自计数，不按两者合计。理由：

- 变化树每 DAO 最多 4096 个节点，同步树跨事务累积到确认为止，持久化树每笔事务一棵。单笔提交在两个守卫之内最多约 2004 个节点（根、两个字段、各 1000 个下标、Version），不会因为共享预算提前整树回退。组件用例实测：一笔 1000 + 1000 两个字段都是增量；不确认时累计到第三笔（约 6000 个下标）才整树回退，两个字段都按整字段发出、数据正确。
- 字节上每个字段整字段与逐点的取舍只取决于本字段改了几个点；按合计会把“各改 600 点”推成两个整字段（12.3KB，逐点约 9.4KB）。
- 跨事务累积超过预算要一拍内同一块有 5 次以上最大半径、互不重叠的笔刷，结果是这块下一次增量等于全量，字节有界。

**测试命令与结果**（2026-11-08，Apple M5，go1.27.3 / .NET SDK 10.0.302，生成工程 replace 到本分支）：

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| 组件 | `go test -race ./game/entities/terrain_chunk/` | P1 / P2 的用例全部改为新类型后通过；新增 `TestTerrainChunkMaterialsGoOutPerPointUnderTheGuard`（1000 点分两笔逐点、同步增量按下标且不带高度，1001 点整字段，两条路径数据相同，只改高度时增量不带材质）、`TestTerrainChunkHeightsAndMaterialsShareTheChangeBudget`（上面的预算结论）、`TestTerrainChunkMaterialOutsideAByteIsRefused`（记录里写 256：`Overlay` / `OverlayOf` / `Data` 报 `ErrInvalidChunk`，`Apply` 拒绝、不写） |
| 编辑、幂等、回滚、重启、换版丢弃 | `go test -race -run Terrain ./internal/service/<game>/` | P1 / P2 用例全部通过；新增 `TestTerrainRecordWithAMaterialOutsideAByteIsNotLoaded`（库里一块的材质是 300：`Corrupt` 1、原因 `ErrInvalidChunk`、查询 `ErrNotLoaded`、编辑 `ErrUnloadable`，另一块照常装入） |
| 真实 TCP | 同上，`terrain_sync_test.go` | 重连后客户端 4 块逐点一致（材质按 `[]uint16` 比较）；r=3m 改材质是按下标的材质增量（不带高度、< 1KB），新增守卫改材质 r=24m 是材质整字段（4～5KB） |
| 真实 Mongo | `scripts/test-demo-generated.sh`（含 `TestTerrainRestartRealMongo`） | 见下文“验证” |
| C# 客户端 | `bash scripts/test-client-dotnet.sh`（完整模式） | 夹具用 `regen.sh -update` 重新生成（DAO 变了：proto 的材质字段、生成 Apply、`terrain-chunk-sync.json`）；夹具增加“材质逐点”一步，共五步：全量 12,318 字节、高度逐点 390 字节、材质逐点（197 点）1,401 字节、材质整字段（半块编号 200）6,164 字节、高度整字段 8,212 字节，C# 生成 Apply 每步与 Go 合并结果相等、650 次查询与 Go `View` 逐点一致；覆盖块规则新增 uint32 材质 > 255 拒绝；材质增量缺基线时生成 Apply 拒绝。全部通过，regen 核对模式 up to date |
| 测试能变红 | 在生成工程副本 / C# 源码上改坏后跑 | 材质改回总是整字段：组件用例报 `1000 changed materials went out as &{... Heights:<nil> Materials:0x…}, want one material update per point and no heights`，TCP 用例报 `an r=3m paint went out as {… payloadBytes:4117 heightsDelta:false heightUpdates:0 materials:true materialsDelta:false materialUpdates:0}, want per-index materials only`；去掉材质守卫：报 `1001 changed materials went out as … want the whole materials field and no heights` 与 `a guarded paint went out as {… payloadBytes:12574 … materialsDelta:true materialUpdates:1793}, want the whole materials field and no heights`；越界截断而不拒绝：报 `load with material 300 in chunk (0,0): {Chunks:6 Overlays:2 Base:4 Discarded:0 StaleBase:0 Corrupt:0}, want it corrupt`；C# 不查材质上限：报 `FAIL accepted uint32 material above 255` |

**实测**（离线提交器、单观察者，方法同 P2，[证据](evidence/terrain-materials-list-20261108/README.md)；合入 D15 后在合并结果上重跑，字节与下行逐字相同）：

| 项 | 结果 | P2（材质整字段） |
| --- | --- | --- |
| 改材质 r=5m 单块（81 点） | payload 590～671 字节，线上 734～815 字节 | 4,117 / 4,261 |
| 抬高 + 改材质同一处同一拍 r=5m | payload 1,150～1,231 字节，线上 1,294～1,375 字节 | 约 4,980（线上） |
| 改材质 r=16m（玩家上限，约 800 点） | payload 5,280～6,399 字节 | 4,117 |
| 守卫整字段：材质 / 高度；全量 | 4,117 / 8,213；12,319 字节 | 4,117 / 8,213；12,319 |
| 热点 r=5m（20 人各 1 次/秒）：材质 0 / 50% / 100% | **19.2 / 25.1 / 30.6 KB/s**（线上） | 19.2 / 75.5 / 132.6 |
| 热点 r=16m：材质 0 / 100%（加测） | **112.5 / 227.5 KB/s** | — |
| 跑图 6m/s：全改过 / 改过 20%；12m/s：全改过 / 20% | 8.1 / 1.6；16.6 / 4.1 KB/s | 相同 |
| 持久整文档 | 约 80KB | 约 44KB |
| `Map.Brush` r=5m 单块 P50（A/B 同机交替 4 轮） | 80～86µs | 74～80µs（+5～8%）；吞吐无可见差别 |

对照 50KB/s：方案假设（r=5m）的三档热点与跑图都在预算内；**玩家用最大半径 r=16m 连续编辑的热点超预算**（不带材质 2.2 倍、全部带材质 4.5 倍）。下行与每秒改动的点数成正比（每点约 7 字节，另有约 9.5KB/s 全量与帧头），50KB/s 约等于观察者附近每秒 5,800 个改动点。另一个边：每玩家频率上限是每秒 2 次，20 人都按上限做 r=5m 抬高 + 改材质时按拟合约 52KB/s，正好在预算线上。

**方案没覆盖、本轮先选的**：

1. 提高 `schema=2`、不写迁移（未上线规则：旧版本拒绝，CHANGELOG 写“升级需清空 `terrain_chunks`”）。
2. 材质值 > 255 的记录一律当作损坏：`Map.Load` 不装入、`Apply` 拒绝；提交后装视图时万一换算失败只记 Error 并计入 `CommitFailures`（`Apply` 已先查过，正常不会发生）。
3. C# 删掉 `(IReadOnlyList<uint>, ReadOnlySpan<byte>)` 重载，换成两个 `IReadOnlyList<uint>`；手写 `ushort` / `byte` span 的重载保留。
4. `Apply` 只看改动涉及的点（第一版整块复制比较三遍，r=5m 编辑 P50 慢约 28%，见证据“编辑耗时”）。行为不变：同一下标取最后一个、值没变的点不算改动。
5. 测量加了“热点 r=16m”两档（超出原定的三档）：原定档位都在预算内，但玩家上限半径下的结论不同，需要维护者知道。

**待维护者决定**：

1. **（已决定：B，降到 500，维护者 2026-11-08；实施见[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)）材质守卫阈值**。材质整字段约 4.1KB（编号都 < 128 时），逐点每点约 7.3 字节，字节持平点约 560 点；阈值 1000 时 560～1000 点的改材质（r≈13～16m）逐点补丁比整字段大，r=16m 时 5.3～6.4KB 对 4.1～4.9KB（最多约 1.6 倍）。A：保持 1000（本轮实现，与高度一致，持久化每次写得少：逐点约 12 字节 / 点，整字段约 40KB）。B（推荐）：降到约 550，r≥13m 的改材质每个观察者省约 1.2～2.3KB，代价是这类编辑持久化多写约 28～33KB（整字段约 40KB 对逐点 7～12KB）、守卫附近 CPU 不变（整字段编码约 0.1ms，比 550 点逐点记账还省）。
2. **（已决定：B，每人每秒 300 个改动点，C 按区域限流记为 P5 视实测再定，维护者 2026-11-08；实施见[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)）玩家最大半径的热点超预算**（r=16m：112.5 / 227.5 KB/s）。可选措施：A：玩家笔刷半径上限从 16m 降到约 6.5m（全部带材质时 20 人各 1 次/秒在预算内；挖大坑要更多次编辑）；B（推荐）：每玩家频率改为按改动点数计的令牌桶（例如每人每秒约 300 点，r=5m 抬高 + 改材质一次约 150 点，r=16m 一次约 1,550 点要攒约 5 秒），半径上限不变，附近 20 人时合计约每秒 6,000 点 ≈ 预算；C：按区域（如以块为单位的邻域）限每秒改动点数，超出排队或拒绝——不依赖“附近 20 人”的假设，但要新机制和公平性规则；D：接受，50KB/s 按平均值看，20 人同时连续用最大半径是极端情况，等 P5 用机器人测真实编辑分布再定；E：框架层更紧凑的列表增量编码（笔刷改的是连续下标，按下标段编码可把每点约 7 字节降到约 2～3 字节），要改生成器与 C# / C++ Apply。

## 17. 编辑配额与材质守卫 500 实施结果（2026-11-08）

分支 `feat/terrain-quota`（基于 b4699758）。维护者 2026-11-08 对第 16 节两项待决定的答复：材质整字段守卫 1000 → **500**（高度守卫仍 1000）；玩家编辑限流从“每人每秒 2 次”改为“**每人每秒改动点数配额**”，高度与材质各算一次，初值 300 点 / 秒；按区域限流的兜底这次不做，记为 P5 视实测再定（D16）。实测见[证据目录](evidence/terrain-quota-20261108/README.md)。

**改动**：

| 部分 | 位置（生成后路径） | 做法 |
| --- | --- | --- |
| 材质守卫 | `game/entities/terrain_chunk`：`MaterialsGuard` | 1000 → 500。玩家 r≈13～16m 的改材质（单块 500～800 点）发整字段约 4.1～4.9KB，不再是 3.7～6.4KB 的逐点补丁；持久化这类编辑写整字段（约 40KB，逐点约 12 字节 / 点）。单笔提交在两个守卫之内最多约 1504 个变化节点，仍到不了共享的 4096 |
| 计点 | `gameplay/terrain`：`View.BrushChanges(b, limits) (heights, materials, err)` | 新增（只新增，已有语义不变）：在视图当前数据上数这笔会改变值的高度点数与材质点数，算法同 `Brush.Apply`，每个采样点只算一次（共享边归右 / 上的块），一个点两者都变了算 2；不复制块数据（装入的块不可变）、只读、可并发；覆盖到没装的块报 `ErrNotLoaded` |
| 配额 | `game/terrainmap`：`Policy.EditPointsPerSecond`（300）/ `MinEditPoints`（20），取代 `EditsPerSecond` | `Map.Brush` 的顺序：参数与 `Policy`、归属 → **计点、扣配额** → `Rules.AllowEdit` → 第一次改的块 `GetOrCreate` → 一笔多块事务。规则见下文。`ErrRateLimited`（业务码 100056 `terrain_rate` 不变，文案改为 `terrain edit point quota used up`） |
| 设置 | — | `Policy` 仍是代码里的缺省值（`DefaultPolicy`，`Map.SetPolicy` 可换），不进 `scene.terrain` 配置：它与客户端的笔刷 UI 一起定（P2 的取舍不变）。demo settings 没有要同步的键 |

**配额规则**：

1. **口径**：参数、归属校验通过后，按 `View.BrushChanges` 算这次会改变的高度点数 + 材质点数；每个采样点只算一次（四块交点 r=7.5m 改材质 = 177 点）；一个点高度、材质都变算 2；值不变的点不算（已在上限再抬高、刷成同一种材质 = 0 点）。现有四种笔刷一次只改一个字段，“同一处抬高 + 改材质”是两次编辑、各计各的（r=5m 约 69 + 81 = 150 点）。
2. **下限**：每次至少计 `MinEditPoints` 20 点。没改到值的编辑与重复请求也要开一笔事务、记 `Applied`（持久化写一次）；不设下限时它们不占配额，一个玩家每秒可以发任意多次。20 约等于一次块更新的线上固定开销（约 110～144 字节 ÷ 每点约 7.5 字节），满配额时这类请求每人每秒至多 15 次。
3. **扣与透支**：令牌桶每秒补 300、容量 300。余额够这次的点数就扣；**桶满时放行一次**（扣成负数），之后要补回到满才能再透支、补到够才能做小编辑。所以单次超过 300 点的编辑——玩家上限 r=16m 抬高约 650～720 点、改材质约 750～800 点，运维 / 地基的整平 r=24m 约 1,800 点——照样能做，做完等：r=16m 一笔约等 2.2～2.7 秒。长期平均不超过每秒 300 点，一次最多多出一笔的量。没选“单次上限与桶上限对齐”：那要么拒绝 r=16m（与“玩家仍可以偶尔用 16m 笔刷”相反），要么把桶加到约 1,600、小笔刷的突发也跟着放大 5 倍。
4. **不够时**：整笔拒绝（`ErrRateLimited`），不部分执行；拒绝在建实体之前，没改过的块也不会留下空实体。
5. **退还**：确定什么都没改时退回到下限 20 点：重复请求（`WasApplied` 命中）、`Rules.AllowEdit` 拒绝、建实体失败、事务里的地形拒绝（`ErrOccupied` / `ErrBrush` / `ErrNotOwned` / `ErrUnloadable`，整笔回滚）。重复请求重新判断后的结论：P2“重复请求也占频率”的理由（重放也要开事务、锁块，不能免费刷）仍成立，所以计下限；但按点数全额计会让“结果未知后用同一请求 ID 重试”的一次大编辑被扣两遍、刚透支后还会被拒，所以只计下限。提交失败、结果未知（WAL 超时等）不退：可能已经生效。
6. **估算的边界**：点数是事务之前、锁外读视图算的。别人的编辑在估算与上锁之间提交了，实际改变的点数可能不同，按估算计、不事后对账（重复请求在估算时也按“会改变”计，结果是重复后才退）。刚做完一次透支的大编辑时，同一请求的重试要等补满才放行。
7. 拒绝发生在扣之前的（请求 ID、参数、半径 / 强度 / 块数、归属、配额本身）不计。配额只在进程内存（重启或换进程从满桶开始），与 P2 的频率限制同一位置；P3 拆成 Base / Real 后挪到 Base。

**测试命令与结果**（2026-11-08，Apple M5，go1.27.3，生成工程 `--skip-deps` + replace 到本分支）：

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| 计点 | `go test -race ./gameplay/terrain/` | 新增 `TestViewBrushChangesCountsWhatApplyWouldChange`：七种笔刷（四种操作、四块交点、半块已是目标材质、高度已在上限、已是整平目标）在覆盖块 + 原始块上，`BrushChanges` 与“在同一份数据上 Apply 后逐点比较”数出的高度 / 材质点数相同；交点改材质 0 / 177；在上限再抬高 0 / 0；覆盖到没装的块 `ErrNotLoaded`、参数不合法 `ErrInvalidBrush` |
| 配额 | `go test -race -run Terrain ./internal/service/<game>/` | 新增三条（替换 `TestTerrainBrushRateLimit`），时钟由测试拨，用“差 0.3 点被拒、补够即过”的边界逐点钉住扣了多少：`TestTerrainBrushQuotaCountsTheChangedPointsOfEachField`（满桶 300：改材质 81 + 抬高 81 + 刷同一种材质 0 点按 20 计 → 余 118；交点改材质 177 点整笔拒绝，实体、`Applied`、视图、提交数不变、交点上没改过的块不建实体；别的玩家不受影响）、`TestTerrainBrushQuotaLetsAFullBucketOverdrawOnce`（满桶时 r=16m 抬高 717 点放行；之后小编辑要等补到它的点数；不满的桶不许再透支、补满即过；整平 r=24m（超过高度守卫）满桶放行、之后小编辑被拒）、`TestTerrainBrushQuotaChargesRepeatsAndRefusalsTheMinimum`（重放、领地拒绝、建筑占位拒绝各只计 20：抬高 81 后余 199 → 179 → 159，交点 177 点补到才过） |
| 材质守卫 | `go test -race ./game/entities/terrain_chunk/` 与 `terrain_sync_test.go` | 组件用例按新阈值（500 点分两笔逐点、501 点整字段；共享预算用例改为每笔高度 1000 + 材质 500、约 1504 个节点仍是增量，不确认累计第三笔约 4505 个节点整树回退）；真实 TCP 用例新增两步：r=12m 改材质（约 450 点）按下标、r=16m 改材质（约 800 点）是材质整字段（4～5KB） |
| 全量 | 生成工程 `go build ./... && go vet ./... && go test ./...` | 通过 |
| 测试能变红 | 在生成工程副本 / 本仓源码上改坏后跑 | 不许透支、余额为正就许透支、重复 / 领地 / 占位不退回、没有下限、点数少算、先建实体后扣配额、材质守卫留在 1000 或降到 400、`BrushChanges` 多算，各自变红，原文见[证据](evidence/terrain-quota-20261108/README.md#测试能变红) |

**实测**（离线提交器、单观察者，方法同 P2，[证据](evidence/terrain-quota-20261108/README.md)）：

| 项 | 结果 |
| --- | --- |
| 配额满速的建造热点（20 人，每人稳态 300 点 / 秒，跳过开头的突发） | r=16m 抬高 47.9、抬高 + 改材质 48.8（60 秒 49.4）KB/s：**在预算内**；r=5m 抬高 54.1、抬高 + 改材质 53.4（60 秒 53.1）KB/s、混合 52.3KB/s：**超 5～8%** |
| 为什么 r=5m 超 | payload 每点约 7.8～8.0 字节（高度约 8.3、材质约 7.3），每次块更新线上另约 110 字节；方案的 7.5 字节 / 点没算后者。r=5m 每秒约 61～66 次块更新、r=16m 约 19～22 次 |
| 改材质 r=16m 单块 | payload 4,151～4,948 字节（整字段），守卫 1000 时按下标 5,280～6,399 |
| 不限配额的热点 r=16m 全部带材质（每人 1 次/秒） | 231.4KB/s（守卫 1000 时 227.5）：材质编号 ≥ 128 的点多时整字段每点 2 字节；配额下到不了这档 |
| 其余（r=5m 三档、跑图、全量、持久整文档） | 与第 16 节逐字相同 |
| `Map.Brush` 多出的计点 | r=5m P50 +2～6µs（+2～7%），r=16m 四块交点 +12～19µs（+2～3%）；同进程先关后开、没有交替多轮 |

**与本文不一致之处**：§7.1 / D16 依据里“每点约 7.5 字节、20 × 300 × 7.5 ≈ 45KB/s 正好落在预算内”在小笔刷下不成立（实测见上）；r=16m 档成立。

**方案没覆盖、本轮先选的**：

1. 每次至少计 20 点（`MinEditPoints`），理由见规则 2。
2. 桶容量取 300（等于每秒补充量，与 P2 “容量同值”一致），透支只在满桶时允许一次。
3. 重复请求与确定没改的拒绝退回到 20 点；提交失败 / 结果未知不退。
4. 计点放在锁外、按视图估算，不在事务里按实际改动对账（与“在校验阶段由笔刷结果算出”一致；在事务里算要把拒绝挪到 GetOrCreate 之后，被拒的请求会先建出空实体）。为此在 `gameplay/terrain` 新增 `View.BrushChanges`（不复制块数据），而不是复制 4 块再 `Apply`。
5. 配额顺序在权限钩子之前（与 P2 频率限制同一位置：钩子可能读别的服务，先挡住刷请求）；被钩子拒绝的退回到 20 点。
6. 守卫整字段的编辑仍按改动点数计（例如 r=24m 整平 1,800 点），不按整字段的字节折算；对守卫附近的编辑偏保守（材质整字段约等于 560 点的字节）。
7. 错误码沿用 100056 `terrain_rate`，只改文案与注释。
8. 测量加了“配额满速”的七档（五种笔刷组合，其中两种另跑 60 秒），每人在头 3 秒内随机开工、预热 5 秒后计 20 / 60 秒稳态：所有人同一拍开工时步调一致，同一拍的编辑合成一个增量，r=16m 档只量到约 28KB/s（预备运行，未入原始输出），不代表真实分布。

**需要维护者决定**：

1. **r=5m 档的配额满速热点超预算 6～8%**（53.1～54.1KB/s；r=16m 档 47.9～49.4KB/s 在内）。A：保持 300（本轮实现）；超出部分按“附近 20 人同时满速”这一极端假设接受，等 P5 机器人实测的编辑分布再定。B（推荐）：降到 270 点 / 秒——临时改参数重跑，五种组合 43.3～49.6KB/s 全部在内，仍是纯“改动点数”一条规则；代价是每人少 10%，r=5m 抬高这一档只剩约 0.4KB/s 余量。C：仍 300，另按覆盖到的块每块加计约 15 点（摊每次块更新约 110 字节的固定开销）——临时实现重跑 41.3～45.5KB/s，配额更贴近字节，小笔刷比现在少做约 25%；代价是多一条规则，客户端预测配额时也要知道。

**未验证**：真实 WAL / Mongo 下的配额与编辑延迟；多观察者、多编辑者时的服务端 CPU；开头满桶 + 透支的突发（20 人同一秒用 r=16m 开工约 2 万点、约 160KB 一次性到达，只是推算）；真实 game 进程实跑与 C# 客户端对 `terrain_rate` 的退避；Linux。

## 18. 超额降级：减少同步对象，不断开（D17，2026-11-08）

分支 `feat/sync-overcap-degrade`（基于 b4699758，合入 D16 之后的 main）。维护者 2026-11-08 决定：会话持有的对象超过上限时不关会话、不断开，减少同步给它的对象。契约见 [ENTITY_SYNC“超额降级”](../framework/ENTITY_SYNC.md#超额降级会话持有对象超过上限)，实现走读见 [impl/04-sync §2G](../framework/impl/04-sync.md#2g-超额降级会话持有对象超过上限d17)，证据见 [evidence/sync-overcap-degrade-20261108](evidence/sync-overcap-degrade-20261108/README.md)。

**设计**（实现之前先定，写在 `framework/sync/entitysync/over_limit.go` 文件开头）：

1. **谁决定留下谁**：Manager 不知道距离，只按（钉住、Profile 优先级、实体 ID）决定本轮新对象里谁先进；已持有的对象它不挤（挤掉会在线上产生 remove / create 往返，还会挤掉政策更想留的）。真正按距离的取舍交给 Interest：收到通知后收紧这名玩家空间视野的可见上限，AOI 从最远的开始离开，腾出的位置由 Manager 放回最近的暂缓对象。所以收敛后可见集与“`MaxVisible` 恰好等于会话剩余容量”时相同：最近的 N 个。
2. **不截断**：对象要么完整持有、要么完全不在客户端。暂缓的订阅保持“待创建”，不捕获、不占快照预算、不让 Drain 等；有空位时按同一排序放回。
3. **自身永远在前**：`SubscriptionSource.SubscribePinned`；SelfView 与 Interest 的 self 钉住。钉住的放不下时 Manager 挤掉同类里优先级最低的未钉住对象（同一帧先 remove 再 create），这是 Manager 唯一会挤已持有对象的情形。
4. **通知与收敛**：`SubscriptionSourceHooks.Deferred` 在下一次政策阶段交付；仍在暂缓里每秒再通知一次（政策退订的若是还没捕获的订阅——快照预算挡着时常见——只通知一次会停在“想要的比放得下的多”上，地形用例实测会停住）。
5. **放宽带滞回**：每次结算先在当前上限内补位；收紧后冷却 1 秒，到期时上限不起作用就撤掉，否则上调一档（`MaxVisible` 的 1/8，至少 1）；上调后冷却期内又被通知则冷却加倍（至多 32 秒），安然度过则减半。失败的试探只是 Manager 内部的一次暂缓，不上线。
6. **地形命名空间同样适用**：按类别计（D15 的 `Namespaces`），一类满了只降级这一类，另一类照常；指标的 `namespace` 标签区分。
7. **未超额零改动**：准入的快路径只数不分配，不超出时整批原样往下走；以 b4699758 的帧 SHA-256 钉住。

**契约变化**：

| 项 | 以前 | 现在 |
| --- | --- | --- |
| 持有对象超过上限 | 下一次 Flush 编码失败、关闭整个会话（`SessionLost`，`object_limit`），会话 ID 是玩家 ID 时断开玩家 | 不关会话、不断开；放不下的新对象暂缓，Interest 收紧视野留下最近的 |
| `SessionLossObjectLimit` / `object_limit` | 有 | 删除；编码时的持有上限错误只可能来自内部不一致，归 `encode_failed` |
| `player.sync.session_lost_disconnect.total{reason}` | 含 `object_limit` | 不再出现 `object_limit` |
| 新 API | — | `SubscriptionSourceHooks.Deferred`、`DeferredSubscription`、`SubscriptionSource.SubscribePinned`、`ManagerStats.OverLimitWaiting / OverLimitDeferred / OverLimitEvicted`、`ManagerCounters.OverLimitDeferred / OverLimitEvicted`、`AOI.SetObserverLimit` |
| 观测 | — | `entitysync_over_limit_deferred_total{namespace,reason}`、`entitysync_over_limit_waiting{namespace}`、`entitysync_interest_over_limit_total{action}`、`entitysync_interest_limited_views`、`CheckHealth` 在有暂缓时 degraded、限频 Warn；告警基线第 15 项 |
| 客户端、帧格式、存储 | — | 不变 |

**demo**：配置不变（玩家 / 怪 100、地形 64、主视野 64、地形视野 64）。用例改为断言不断开：`TestACrowdOverflowingASessionShrinksTheViewInsteadOfDisconnecting`（主视野不设上限的 101 人挤在一起：没有玩家被断开、每个会话收敛到 100 个对象（自身 + 最近的 99 个）、被丢的不比留下的近；人群散去后的放宽由 policy 包的 `TestInterestShrinksAFullSessionToTheNearest` 钉住）取代断开用例；`TestTerrainDoesNotWidenThePlayerObjectLimit` 改为玩家一类降级到 100、地形不受影响；`TestTerrainChunksOverTheirLimitShrinkTheView`（地形视野配成 80 > 64）取代 `TestTerrainChunksOverTheirLimitCloseTheSession`：稳定在 64 块、丢的是最远的、真实 TCP 客户端解出的对象表与服务端持有集合一致、连接不断。`demo/README.md` 地形同步段里过时的“会话对象上限 164 … 每会话每次 Flush 至多 4 个全量（对玩家、怪也生效）”一并改正。

**验证**：见[证据目录](evidence/sync-overcap-degrade-20261108/README.md)：修前红（被改写的用例在 b4699758 上的失败原文）、修后绿、未超额逐字节不变（SHA-256 对照）、同机前后基准对照（count 6，benchstat）：frames / packs / wire-B 与 allocs/op 全部相同；`SessionRecovery` B/op +1.3%（`sessionLifetime` 多一个指针、跨入 64 字节分配档）、`ExhaustedSnapshotWindow` +1.4%；每个订阅多 16 字节（96→112）；`AOIBatchRecompute` 4 worker 稀疏一档 +7～11%，原因尚未确认（KNOWN-LIMITS）。第一轮对照发现的两处（每次 Flush 无条件取时钟 +10%、订阅结构多 48 字节）已改掉。

**接受的代价与剩余边界**（KNOWN-LIMITS“超额降级的剩余边界”）：收敛要几轮 Flush、秒级（地形用例约 2 秒，没有证明上界）；放宽是试探式的；只有 Interest 收紧（Group / Direct / GroupView 等空位，`AOICluster` 没接）；共享空间视野被收紧时挂接它的其他玩家一起少看；`MaxVisible` 之和大于会话上限这类配置错误只降级、不在启动时拒绝。

## 19. 暂停点（2026-11-08，额度用尽）

维护者 2026-11-08 指示：额度用尽就先暂停。当日周额度已满，按此停下，未完成的工作记录如下，恢复时从这里接。

**已决定、未实施**

- **D16 配额改为 270 点 / 秒**（维护者 2026-11-08 选推荐项 B，取代 300）：只改 `terrainmap.Policy.EditPointsPerSecond` 缺省值与令牌桶容量，按[第 17 节](#17-编辑配额与材质守卫-500-实施结果2026-11-08)的方法重跑热点五种组合确认都 ≤ 50KB/s（临时重测为 43.3～49.6KB/s），再改 §7.1 / §8 D16 / §8.1 的数字。

**进行中、只在本地分支（未推送，勿当已完成）**

| 分支（本地 worktree） | 内容 | 状态 |
| --- | --- | --- |
| `feat/terrain-p3-ghost`（2026-11-09 已接续，见[第 21 节](#21-p3-ghost-副本t05d192026-11-09)） | P3 跨格：Ghost 带地形副本推送、乱序 / 丢失 / 重启补齐、D13 重置后邻格撤回原始块、分区场景接入地形同步、邻格玩家可见 | 中途停止，一笔 WIP 提交（cellworld 线上消息扩展、`gameplay/terrain` 只读副本），未验证；停止时正在修一个仅测试代码里的数据竞争 |
| `feat/terrain-p3-move` | P3 移动与寻路：坡度 / 台阶 / 下落上限、游泳、高度场 A*（坡度代价、按块版本失效），不开地形时行为不变 | 2026-11-09 接续完成单格部分（[第 22 节](#22-p3-移动与寻路实施结果2026-11-09)），仍在本地分支、未合入；跨格依赖 T05 |

两支的原始任务要求（范围、验收、测量、文档小节编号）以维护者重新下达为准；恢复时先 `git merge origin/main`，WIP 提交可以改写或丢弃。

**待维护者确认（不阻塞）**

- [第 18 节](#18-超额降级减少同步对象不断开d172026-11-08) 超额降级里实施方先定的四点：仍在暂缓的订阅每秒再通知一次（不可配置）；已持有对象 Manager 不挤（钉住的“看自己”除外）；只有 Interest 收紧，Group / Direct / GroupView 只暂缓；`MaxVisible` 之和大于会话上限时只在运行期降级、启动不拒绝。
- KNOWN-LIMITS：`AOIBatchRecompute` 4 worker 组合慢 7～11%（约 9µs），尚未确认。

**之后**：P3 收尾后是 P4（水位线规则、建筑实体与占位）、P5（跨格编辑转发与扣资源对账、按区域限流兜底、1w 地图容量测试）、P6（可选物理）。

## 20. 编辑请求身份与判重（T01/T02，D18，2026-11-09）

分支 `fix/terrain-edit-identity`（基于 `23247c38`，合入 `93c00538`）。修 [进度总表](TERRAIN-STATUS.md) 的 T01、T02。证据见 [evidence/terrain-edit-identity-20261109](evidence/terrain-edit-identity-20261109/README.md)。

**原缺陷**（P2 的做法：`EditID = splitmix64(玩家 ⊕ splitmix64(请求 ID))` 取 63 位，记进锁住的每一块的 `Applied`，每块最近 32 个）：

- T01：同一块之后又被改 32 次以上，旧编辑 ID 被挤出环，旧请求重放被当成新请求再执行一次（修前红：40 次后续编辑后重放序号 1，块 Version 41 → 42；重启后同样）。
- T02：两个不同请求的 64 位混合值只差最高位时截断后相同，后到的被当成重复、不执行（修前红：构造同一玩家两个请求 ID、以及两个合法玩家各自的请求 1，第二个都答 `Duplicate`）。

**身份**：一次编辑是（玩家 ID, 编辑序号 `Seq`），两者原样比较，不混合、不截断。玩家 ID 是 unique ID（1～2^52−1），`Seq` 取 1～2^63−1（协议是 `uint64`，超出的拒绝为 `terrain_brush`，不截断）。

**账本**：每个玩家一份 `TerrainEditor` 实体（kind 12、PlayerScoped，实体 unique = 玩家 unique，集合 `terrain_editors`，`db=main`，schema 1）：

| 字段 | store 键 | 含义 |
| --- | --- | --- |
| `LastSeq int64` | 1 | 已应用的最大序号，0 = 还没有 |
| `Window int64` | 2 | 位图：第 i 位 = 序号 `LastSeq−1−i` 已应用（按位使用，第 63 位为 1 时数值为负；BSON int64 原样保存全部 64 位） |

判定（`terrain_editor.SeqWindow.Check`，纯函数）：

| 序号 s | 结论 | 处理 |
| --- | --- | --- |
| `s > LastSeq` 且 `s − LastSeq ≤ 2^20` | 新 | 执行，`Record`：窗口左移，旧 `LastSeq` 落入位图 |
| `s > LastSeq + 2^20` | 超前 `ErrSeqAhead` | 不执行、不记（防止一个巨大的序号把 `LastSeq` 推到顶，此后再没有新号） |
| `LastSeq − 64 ≤ s ≤ LastSeq`，已应用 | 重复 | 不写，答 `Duplicate` 与各块当前版本 |
| 同上，未应用 | 新（乱序到达） | 执行，置位 |
| `s < LastSeq − 64` | 过期 `ErrStale` | 不执行、不写，答 `terrain_seq` 与 `LastSeq` |

**为什么是对的**：被记下的序号 s 此后只可能是 `s == LastSeq`、在窗口内且位为 1、或低于窗口；三种情况 `Check` 都不答“新”。所以同一（玩家, 序号）至多执行一次，不依赖窗口外的记录是否还在——窗口大小只决定“答重复”还是“答过期”，不决定会不会再执行。账本大小固定（两个 int64），不随时间、块数或改过同一块的玩家数增长。单元测试把 `SeqWindow` 与“记住全部已应用序号的无界模型”在 200 × 2000 步随机流（顺序、跳号、窗口内乱序、任意旧序号、越界）上逐步对照。

**一致性**：笔刷事务（handler `TerrainBrush`，`rollback=undo durability=strict`）的目标是覆盖到的块（World）+ 玩家账本（PlayerScoped），Nest 一起取锁。判重、校验、算笔刷、写块、`Record` 在同一笔事务里：拒绝（过期、超前、占位、参数）在第一次写之前，整笔回滚；提交被拒时块与账本都由 setter 逆操作还原，同一序号重试照常执行。跨块编辑（至多 4 块）只有一份账本，不存在“某块记了、某块没记”。

**重启**：账本是持久实体，`GetOrCreate` 冷加载，与块同一条 WAL → 投影路径；离线替身与真实 Mongo 各验证一次（见下）。

**客户端取号**：新编辑 = 上一次的序号 + 1（被拒绝的序号没有记下，可以跳号也可以复用）；结果未知（超时、断线）时用同一序号重发。客户端不知道上一次用到哪里时（会话开始、重装、换设备）先发 `Seq = 0` 的查询（`Map.LastSeq`：只读，计 `MinEditPoints` 20 点配额），从 `LastSeq + 1` 开始；答复里也总带 `LastSeq`。收到 `terrain_seq` 时：`Seq ≤ LastSeq` 是太旧（这一笔当时是否生效已无法确认——看同步流里的地形，不要换新号把同一笔再发一遍），`Seq > LastSeq` 是跳得太远（从 `LastSeq + 1` 续号再发）。

**配额**：规则不变（第 17 节），过期、超前与重复、占位一样属于“确定什么都没改”，退回到下限 20 点。

**存储与协议变化**（未上线，不兼容旧数据 / 旧客户端）：

- `TerrainChunkDao` schema 2 → 3：删掉 `Applied []uint64`（store 键 5 弃用、不复用）。旧记录按 `ErrSchemaMismatch` 拒绝，**升级须清空 `terrain_chunks`**（同时清空 `terrain_editors`，新集合本来为空）。
- 新集合 `terrain_editors`（`TerrainEditorDao` schema 1）、实体 kind 12、组件类型 2012。
- 协议 `TerrainEdit`（msgid 10038）：请求字段 1 由 `RequestID` 改名 `Seq`（类型仍 `uint64`，语义改为按玩家单调递增，`0` 为查询）；答复新增字段 5 `LastSeq int64`。新业务码 100059 `terrain_seq`。同步视图（`TerrainChunkDao` 的四个同步字段）不变，C# 同步夹具 `regen.sh` 核对无变化。
- `terrainmap`：删掉 `EditID`；`Open` 多一个账本 lifecycle 参数；新增 `Map.LastSeq`、`ErrStale`、`ErrSeqAhead`、`*SeqError`；`BrushResult` 多 `LastSeq`；`terrain_chunk` 删掉 `AppliedRing` / `WasApplied` / `RecordApplied`。

**方案没覆盖、本轮先选的**（待维护者确认）：

1. 账本放在单独的 `TerrainEditor` 实体，不放在块上（按块记会随玩家数无界增长，有界又回到 T01），也不放在 Player 实体（P3 玩家在 Base、块在 Real 时不能与块同一笔事务；单独实体也不让地形事务去锁玩家）。
2. 窗口 64（一个 int64 位图）；超前上限 2^20。
3. 续号查询复用 `TerrainEdit` 的 `Seq = 0`，不新增消息号；查询计 20 点配额；第一次查询会建出空账本（`GetOrCreate`）。备选：登录 / 进场答复带 `LastSeq`。
4. 过期与超前用同一个业务码 `terrain_seq`，客户端按 `Seq` 与 `LastSeq` 的大小区分。
5. 账本 `db=main`、全局作用域（与块相同），不按 sid 分库：P2 里一个玩家的编辑只在他所在、且写这些块的进程执行。

**剩余边界**：

- 窗口外的旧请求得到“过期”而不是原来的结果：结果未知的重试必须在之后的 64 个新序号之前发出，否则只能确认“这次没做”，确认不了“当时做没做”。答复不保存第一次的结果（重复请求答当前版本，与 P2 相同）。
- 客户端不先查询就从 1 重新计数时，窗口内的旧序号会被当成重复（新编辑不执行）——客户端须遵守“会话开始先查”。
- P3 跨格转发（玩家在 Base、块在 Real）时，账本要跟着写块的进程按（玩家, 格）分开，Base 侧再保留面向客户端的序号；ID 方案到时定。玩家迁服（sidmove）沿用玩家迁移的停写与排空，本轮没有单独验证账本在迁服中的投影滞后。

**验证**（2026-11-09，Apple M5，go1.27.3，生成工程 `--skip-deps` + replace 到本分支）：

| 用例 | 结果 |
| --- | --- |
| 修前红（基线 `23247c38` 的生成工程，临时用例） | T01：40 次后续编辑后重放序号 1 被再执行（Version 41 → 42），重启后同样；T02：同一玩家请求 `2830626994681676340` 与 `6709829656160145507`、玩家 `2676` 与 `1249291350150932` 的请求 1，编辑 ID 相同，第二个答 `Duplicate` 未执行 |
| `TestSeqWindowAgreesWithTheUnboundedModel`、`TestSeqWindowEdges`、`TestLedgerPersistsAllSixtyFourBitsAndRollsBack` | 通过（`-race`） |
| `TestTerrainBrushReplayBelowTheWindowIsStale` | 序号 1 之后又编辑 100 次：重放 1、36 答 `ErrStale`（`LastSeq` 101），37、100、101 答 `Duplicate`，块 / 视图 / 账本 / 提交数不变；重启后同样；新序号 102 生效 |
| `TestTerrainBrushRequestsThatSharedATruncatedIDBothApply` | 玩家 `2676` 与 `1249291350150932`（旧算法下请求 1 的编辑 ID 都是 `8307342279125212343`）编辑同一块，两次都执行（Version 1、2），各自重试只得 `Duplicate`、不写 |
| `TestTerrainBrushRollbackLeavesEveryChunkAndTheView` | 四块事务被提交器拒绝：块、视图与账本都不变，同一序号重试执行 |
| `TestTerrainBrushRefusalsWriteNothing` | 新增序号 0、超过 2^63−1、超前 2^20 三种拒绝；每种拒绝后块、视图、账本、提交数不变；拒绝过的序号之后照常可用 |
| `TestTerrainBrushRepeatsAndReordersApplyOnce`、`TestTerrainLastSeqTellsAClientWhereToContinue`、配额三条 | 通过；乱序 1、3、2 后账本 `LastSeq 3`、位图 `0b11`；超前拒绝只计 20 点 |
| 测试能变红 | 把过期当成新请求：`TestTerrainBrushReplayBelowTheWindowIsStale` 报重放 1 被执行（Version 102）；不 `Record`：重复与碰撞两条用例报重放未答 `Duplicate` |
| `TestTerrainEditLedgerRealMongo`（`scripts/test-demo-generated.sh`，私有 `scripts/mirror-local.sh` 副本集） | 通过（`-race`）：序号 1～70 经 strict 事务 → WAL → 投影，停机后库里恰一份账本 `{LastSeq 70, Window −1}`（64 位全 1）；重启冷加载后重放 1、5 答 `ErrStale`，6、70 答 `Duplicate`，块 Version 仍 70；新序号 71 生效，库里账本 `{71, −1}`。同一脚本里 `TestTerrainRestartRealMongo` 与其余真实依赖用例也通过（[原始输出](evidence/terrain-edit-identity-20261109/real-dependencies.txt)） |

## 21. P3 Ghost 副本（T05，D19，2026-11-09）

分支 `feat/terrain-p3-ghost`（接续第 19 节记录的 WIP 提交，WIP 已在本分支改写、不在历史里；合入 main `3fb089d9` 与 `ebd0ecc3`）。完成[进度总表](TERRAIN-STATUS.md)的 T05：邻格的编辑推到 Ghost 副本、丢失 / 乱序 / 重启补齐、D13 重置后邻格撤回原始块、分区场景接入地形同步、邻格玩家看到别的格改过的块。跨格编辑仍拒绝（P5 / T08）。证据见 [evidence/terrain-p3-ghost-20261109](evidence/terrain-p3-ghost-20261109/README.md)。

**WIP 审查结论**：

| WIP 内容 | 结论 | 原因 |
| --- | --- | --- |
| cellworld 扩展消息（`ext.go`、`NodeConfig.Ext`、`Node.SendExt`、线上格式 v7） | 保留 | 发件只排进出站队列（可在提交后回调里调），收件与 Node 自己的消息同一个准入门；用例齐全 |
| `gameplay/terrain` 的 `Replicator`：订阅续期兼补齐、推送、按版本取新 | 保留骨架，改写三处 | ① 代际只在 `Subscribe` 的块状态里带 `BaseRevision`，拥有者照样答别的包版本的订阅（变异实测：旧包的拥有者 2 秒内答了 47 份订阅、发出 252,813 字节）→ 改为每条消息头带 `BaseRevision`、两个方向都整条丢弃；② 本进程自己写的块也会订阅自己（变异实测红）→ 跳过；③ 包说明补“代际”“限制”与并发 |
| 分区场景接线 | WIP 没有，新写 | 见下 |

**传输**：每个分区进程的 cellworld Node 的扩展消息，命名空间 `terrain`。发件非阻塞（出站队列满丢弃、计数，由续期补齐），所以拥有者在改块事务的提交后回调里调 `Replicator.Changed` 不 Dispatch、不做 I/O。NATS 内网互信，不加按主题的鉴权。

**协议与语义**（`gameplay/terrain/replica.go`，[包说明“块副本”](../../gameplay/terrain/README.md#块副本p3replicago)）：

1. 订阅（副本 → 拥有者）：想要的块按拥有者分组，附本地持有的版本，每 1 秒重发（租约 3 秒）；拥有者只答版本不同的块（覆盖块整块、原始块发“原始块”）。所以推送丢了、副本重启、拥有者重启（订阅表丢了）都在至多一个续期周期（1 秒）内对齐。
2. 推送（拥有者 → 副本）：块提交之后推当前整块；发出前又提交几次只发最新一份。
3. 顺序：副本只认比本地新的版本，重复、迟到、倒序的消息不回退（计 `Stale`）；每条都是整块，丢中间版本无妨。
4. **版本与代际**：代际就是原始地形包的 `BaseRevision`，不另设 epoch。论证：(a) 同一 `BaseRevision` 内拥有者的 `Version` 单调，且一个版本只对应一份内容——前提是只推持久的版本：改块事务是 strict，`nest.AfterCommit` 在 WAL 落盘之后执行，`Changed` 在那里调用，所以崩溃重启不会出现“同一个版本号、另一份内容”；(b) `Version` 只在原始地形换版时由 D13 重置为 0，换版必然换包重启，重启后的进程带新的 `BaseRevision`；(c) 每条消息头带发件进程的 `BaseRevision`，不同的整条丢弃（两个方向：拥有者不答，副本不收）。所以换版前的推送到不了换版后的进程，D13 的重置不会被旧推送撤销；换版后拥有者从 0 重新单调递增，副本按新包的原始块起步。D13 的存储语义不变（不需要维护者另行决定）。
5. **D13 撤回**：邻格用新包启动时，旧包的记录装不进视图（持有 −1），订阅一到拥有者就答“原始块”，邻格装新的原始块（`ApplyReplica` 的 Version 0）；视图持有更新的覆盖块时“原始块”按旧消息拒绝（`ErrStaleOverlay`），不会抹掉新编辑。
6. 只认拥有者：块状态只接受 `OwnerOf` 发来的（`WrongOwner`），拥有者只答 `Owns` 的块；本进程写的块不订阅。

**两种副本，一路订阅**（`internal/service/<game>/terrain_partition.go`）：

- **Ghost 带**：`terrainmap.Map.ApplyReplica` 把邻格写的、本格 Ghost 带里的块装进地形视图（`Height` / `Probe` 等查询、移动、交接判断读到邻格最新的已提交数据）；不在 Ghost 带里的块拒绝（`ErrNotOwned`）；Version 0 撤回覆盖改用原始块；装不进来的块从视图删掉，查询答 not loaded。
- **邻格玩家可见**：玩家连在自己的 Base 进程上、可能站在任何一个格里。Base 进程按本进程玩家的地形视点（进场位置、之后是 cellworld witness 里玩家自己的位置）订阅附近别的进程写的块，改过的块是只读的 `terrain_chunk.Replica`，作为 subject 登记进场景同一个 entitysync Manager、同一个地形 Interest。与实体同一个 ID、命名空间、kind 与同步编码，TerrainChunk 的类型调度（periodic 1s）、`terrain` 命名空间上限 64 与快照预算照样适用；本进程自己写的块仍是实体。客户端看不出区别。
- 订阅范围：地形视野 200m + 滞回 20m + 视点上报步长 8m + 一块 64m = 292m（块在进入视野之前已订阅）；视点变化后至少隔 100ms 重算一次。Ghost 带与客户端想要的块合成一份订阅，同一块只订一次。
- `Replica` 不绑实体、没有 DAO（生成 DAO 的 setter 只能在 Nest 事务里调用）。增量按“每个点最后一次改变的序号”自己算：增量包含序号大于已确认序号的全部点、取打包时的最新值；确认只推进到这次捕获里第一次打包时的序号（一次捕获可能给老订阅者打增量、给新订阅者打全量，之间副本可能又变了）。所以邻格玩家收到的小编辑也是按下标的增量（r=3m 抬高：21 个高度点，载荷 195 字节）。

**线上格式与升级**：cellworld 线上格式 v6 → v7（新增扩展消息），v6 及更早的消息按坏消息丢弃；副本消息自身也带版本号。未上线，不兼容：**升级须先停旧进程再起新进程**。存储不变。

**方案没覆盖、本轮先选的**（D19，维护者 2026-11-10 确认按推荐）：

1. 代际用 `BaseRevision`，不另设 epoch（论证见上第 4 条）；前提是地形改块保持 strict。将来改块若改成非 strict，要加代际。
2. 进程间推整块（压缩后约 2.9～3.7KB / 块 / 订阅进程），不推进程间增量；客户端方向的增量由副本自己算。一个热点块被 k 个进程订阅时，拥有者每次提交发 k 份。
3. 续期 1 秒、租约 3 秒、订阅范围 292m、重算间隔 ≥ 100ms 都是常量，不进配置。
4. 一个进程只有一路副本（Ghost 带与客户端想要的块共用）。
5. 分区场景玩家的地形视点取 cellworld witness 里玩家自己的位置（本格玩家与跨格玩家同一来源）。
6. 各进程原始地形包版本不同（换版只换了一部分进程）时两边之间不复制，副本停在换包前的数据，邻格 Ghost 带里装不进来的块查询答 not loaded；换版要整组进程换包重启。
7. 拥有者数据倒退（从备份恢复）不在保证内，副本的更新版本不会被拉回（计 `Ahead`），要重启副本进程。

**限制与未覆盖**：

- 订阅重算按玩家数 × 视点周围块数（292m 半径约 100 块）算，T09 规模（单格数千玩家）未测；拥有者的订阅表、推送扇出没有上限测量。
- 一份订阅是一条消息，接收端解码至多 32768 块。（2026-11-11 补，审查 RV-20261110-01：本轮实现时发送侧不查这个数，一路对同一个拥有者想要更多时会发出接收端必然拒绝的订阅、每次续期都一样；现在 `Want` 按拥有者检查 `MaxSubscriptionChunks`，超过时整份拒绝并返回 `ErrSubscriptionTooLarge`，之前接受的订阅照常续期，见[包说明“块副本”](../../gameplay/terrain/README.md#块副本p3replicago)。没有做分片订阅。）
- 客户端第一次看到别的进程写的块是新订阅的即时全量（实测约 7～48ms），之后的更新按 TerrainChunk 的 1 秒周期合并；Ghost 带不受这个周期影响。
- 真实 Mongo / WAL 下的崩溃重启（“只推持久版本”的前提）只靠 strict 契约推出，本轮没有真实依赖的崩溃演练；`scripts/test-seamless-demo.sh` 的分区配置 `cell_size` 3000 不满足开地形的要求，演练没有覆盖地形。（2026-11-10 补：开地形的分区场景真实多进程演练，含 kill -9 格进程后重启、真实 Mongo / WAL，见 [§26](#26-开地形的分区场景真实多进程演练t05t062026-11-10)；“提交了但答复没到”的在途编辑仍没有稳定构造出来。）

**验证**（2026-11-09，Apple M5，go1.27.3，`GOWORK=off`；生成工程 `--skip-deps` + replace 到本分支）：

| 用例 | 结果 |
| --- | --- |
| `gameplay/terrain` `replica_test.go` 七条（推送、丢失 / 重复 / 乱序、任一边重启补齐、D13 换版撤回且旧包拥有者不答、只认拥有者、线上格式） | 通过（`-race`） |
| `gameplay/cellworld` `ext_test.go`（扩展消息）与 `TestTerrainReplicaOverNodes`（回环与真实 nats-server） | 通过；全包 `-race` 通过。合并 `ebd0ecc3` 后第一次全包 `-race`（与生成工程测试同时跑、机器负载高）失败一次，当时只留了末尾几行、没有用例名；随后同一提交连跑 4 次全部通过。原因尚未确认，与 KNOWN-LIMITS 里既有的 cellworld 间歇失败是否同一问题也未确认 |
| `TestTerrainPartitionGhostAndNeighbourClients`（三个进程：格 A、格 B、只做 Base 的 C；真实 TCP 客户端） | 边界编辑到邻格 Ghost 带约 2～12ms、到别的进程的客户端约 7～48ms；C 上站在格 B 的玩家与 B 上的玩家各自持有的块与写它们的格逐点一致；小编辑到邻格客户端是按下标增量；玩家走远后远处的块进入订阅；推送全丢由续期补齐（< 2s）；录下的旧消息倒序重放不回退；跨格编辑 Ghost 带的块答 `ErrNotOwned`、零写入 |
| `TestTerrainPartitionReplicasCatchUpAfterRestarts` | B 停机期间 A 继续改，B 重启后约 3～10ms 对齐；A 重启后 B、C 的续期重新订上，之后的改动照常推到 |
| `TestTerrainPartitionDiscardWithdrawsTheOverlay` | 新包启动的 A 按 D13 重置，B 撤回到新的原始块；旧包的推送重放不恢复覆盖；新包上的编辑照常推到 |
| `TestReplicaDeltasBringEverySubscriberToTheLatest`（`terrain_chunk`） | 同一次捕获里先增量、`Set`、再全量，老订阅者与新订阅者都到最新；超过守卫发整字段；旧版本 / 别的包版本拒绝 |
| 测试能变红（变异，均在本分支上临时改代码） | 拥有者不查 `BaseRevision`：旧包拥有者答 47 份订阅；不跳过自己的块：订阅了自己；不在提交后推送：Ghost 1.10s 才到（续期补齐）；不接 witness：远处的块等不到；副本不比版本：`Stale` 为 0；确认推进到当前序号：老订阅者 `heights[20]` 停在旧值；“原始块”压过更新的覆盖块：`ApplyReplica` 答 nil；装不进来的块不删：撤回超时 |
| 生成工程 `go build ./... && go vet ./... && go test ./...`、`roost id check`、`roost generate --check` | 通过 |
| `scripts/test-seamless-demo.sh`（私有 `scripts/mirror-local.sh`） | 14 个演练步骤全部通过；最后一步普通场景 sid 1003 的机器人压测阈值失败（本分支 p95 16.45s > 16s；同机同时段 main `3fb089d9` 同一步也失败：公会 saga 等待超时，error_rate 1），与本轮无关，见证据 |

测量（`ROOST_TERRAIN_REPLICA_PERF=1`，r=5m 抬高，300 次）：提交到邻格副本应用完，回环 p50 182µs / p99 834µs，真实 nats-server p50 227µs / p99 946µs；每块推送 3,674～3,677 字节（整块压缩）；进程内网 p50 381µs、2,908 字节。

## 22. P3 移动与寻路实施结果（2026-11-09）

已合入 main（分支 `feat/terrain-p3-move`，基于 `93c00538`，合并过 `3fb089d9`（T01/T02））。接续 §19 记录的 WIP 提交（已改写为本节的四笔提交，原 WIP 不保留）：审查后保留其行走规则与 `movement.Surface` 接入，改为 int64 高差、加负高度与加权寻路，补齐 demo 接入、端到端与纠偏验证。完成判据对照 [进度总表 T06](TERRAIN-STATUS.md)；证据与复跑命令见 [证据目录](evidence/terrain-p3-move-20261109/README.md)。

**范围**：单格（不分区的普通场景，或分区场景里一个格的进程内部）。分区场景的位置由 cellworld 按速度积分（`gameplay/cellworld/actor.go` `integrate`，没有地面钩子），本轮不改 cellworld，跨格移动与交接未按高度判定，依赖 T05（Ghost 实时副本）后再接。

### 22.1 行走规则（`gameplay/terrain/walk.go`）

- **节点粒度 1m**：节点 = 采样点，世界坐标归最近的采样点（四舍五入）；节点高度就是采样点高度（只读本块，不插值）。取舍：高度场本身 1m 采样，坡度在采样之间是线性的，按节点判定已是地形能表达的全部精度；按位置连续判定要选基线，基线随心跳步长变（5Hz、60 单位 / 秒一拍只走 12 厘米，厘米取整的高差会把缓坡算成陡坡），按节点则与心跳、速度无关，移动与寻路天然一致。代价：一个采样间距内的陡崖与 1m 宽的坡判定相同；没有角色体积。
- **相邻一步**（八邻域，水平距离正交 100、对角 141 厘米）：上行坡度 ≤ `MaxSlope` 或高差 ≤ `MaxStep`；下行坡度 ≤ `MaxSlope` 或落差 ≤ max(`MaxStep`, `MaxDrop`)；对角两条正交拐角路线也要走得通（不切角）。高差按 int64 计算，与 `HeightBase` 正负无关（基准 −100 米与 +500 米同一组用例结果相同）。
- **水**：水深 ≤ `WadeDepth` 涉水（速度不变）；更深是游泳节点，站立高度取水面、速度 `SwimSpeed`‰；水里节点之间没有高差，上岸按“水面 → 岸上地面”判定。`SwimSpeed` 0 时深水不可进入，站在里面的可以走出来。
- **不可进入**：地图外、`Bounds`（场景世界）外、块没装入（不猜）、建筑占位钩子 `Occupied` 为真（P4 之前缺省没有）。
- **参数初值**（§8.1）：坡度 45°、台阶 50 厘米（缺省坡度下不起作用，留给调低坡度的玩法）、陡降 3 米、涉水 50 厘米、游泳 60%、上坡代价 2、下坡 0、寻路估价 1.5 倍。按常见 MMO / 开放世界取值（Unreal 44.8° / 45 厘米、Unity 45° / 30 厘米），没有玩法实测。
- **高度不进位置**：服务端位置仍只有 X / Y（cellworld `EntityState`、DAO 都不加 Z，线上格式不变）；站立高度由 `Walker.StandZ(p)` 推出（游泳节点是水面，否则 `View.Height` 插值），客户端用同一算法与同一份地形算出。

### 22.2 权威移动（`gameplay/movement`）

`MoveConfig.Surface func() Surface` 可选：每拍开始取一次；返回 nil 时推进与不设时逐位相同（`TestANilSurfaceWalksExactlyLikeNoSurface`）。有地面时按 0.5 单位采样，每进一个新地面节点问 `Step(上一节点, 新节点)`，不放行就以 `Blocked` 停在上一个采样点；速度千分比折算路程预算（游泳一秒走 60%）；站立节点没有定义时一步不走。`Walker` 只读视图的原子槽位，不取锁、`Step` 不分配，满足 short 池只做无锁内存查询的要求。每拍读视图当前版本：编辑提交并装进视图后，下一步就按新地形判定，不等同步。

### 22.3 寻路

`Walker.FindPath`：1m 节点上的八方向 A*，边与 `Step` 同一规则；代价 = (水平距离 × 1000 + 爬升 × `UphillCost` + 下降 × `DownhillCost`) × 1000 ÷ 目标节点速度千分比；单次最多展开 4096 个节点（沿用 demo，`DefaultMaxVisited`）。每块在一次搜索里只读一次槽位，并发提交不会混用同一块的两个版本；同一视图状态结果确定。

- **加权估价（实施方先定）**：估价只算水平距离（事先不知道要爬多少），坡度代价越大、地面越起伏，最优 A* 为证明“没有更省的路”展开的节点越多。实测起伏地面（两层正弦，坡度大多 45° 以内）上，上坡代价 2 时最优搜索 50m 展开 1795 个、200m 展开 14348 个（超出 4096，会退回直线走）；估价放大 1.5 倍（`SearchWeight` 1500）后 378 / 683 个，200m 路线代价比最优多 2.9%（上界 50%）。平地两者相同（200m 展开 201 个）。
- **路线形状**：`Route.Points` = 起点、起点所在节点、途经节点、终点所在节点、终点；每段只跨一个节点边界，移动逐节点检查时判定的正是寻路用过的相邻节点（随机起伏地形上逐步走一遍验证）。代价：开头可能先折回起点节点中心（≤ 约 0.7m）。
- **失效与缓存**：`Route.Chunks` 记判定读过的块与版本，`Route.Fresh(view)` 判断是否仍有效（改块、卸块即失效；不判断出现更短的路）。不做路线缓存（roost-coding：不引入无证据的缓存）：每次请求算一次，移动逐节点按最新地形判定，所以“路线失效”的效果是行走在被改处停下（`Blocked`），下一个请求按新地形重算。改块后旧路线走不通、重算绕行由 `TestFindPathGoesAroundAWallAndADitchAfterTheChunksChange`（库）与 `TestTerrainWalkIsHeldBySlopesAndRoutesAroundEdits`（demo）覆盖。
- **无地形**：demo 的 `PathFind` 不装地形时仍是 `spatial.FindPath` 的四方向格子 A*，结果与之逐一相同（`TestWithoutASurfaceTheMapIsTheFlatGrid`，400 组随机起终点 + 随机障碍）。

### 22.4 demo 接法

- `scene.Terrain` 增加 `Surface()` / `SetSurface(*terrain.Walker)` / `Standable(p)`；`TerrainSystem` 用原子指针存 Walker（地形在场景建好之后才装入），`SetSurface` 把它夹到场景世界。
- 服务 Init 在地形同步就绪之后（`scene.terrain != nil`，即开地形且不分区）调 `installSurface(area.Terrain(), terrainMap)`；`terrainmap.Map.Walker()` 给出缺省规则与建筑占位钩子（`Rules.Occupied`）。
- `actor.Surface` 是玩家与怪的 `MoveConfig.Surface`（读场景发布的地面；没有时返回 nil，避免带类型的 nil）。
- `PathFind.Path` 有地形时走 `Walker.FindPath`；`Place` 有地形时按 1m 节点向外找能站的点；`MoveTarget` 终点、瞬移、刷怪用 `Standable`。场景格子的占位（`Occupy`，demo 没有生产用户）不进节点图，行走仍逐点问格子。
- 不开地形、分区场景：`Surface()` 为 nil，所有入口与之前相同。

### 22.5 1 秒同步窗口与纠偏

TerrainChunk 按 1 秒周期下发，玩家位置变化即发；服务端行走按最新地形。`TestTerrainEditsDuringTheSyncWindowAreCorrected`（真实 Nest、心跳、混合同步、TCP 客户端）：

1. **挖在前方**：玩家朝东走时别人在前方挖 5 米深的坑。服务端这一拍停在坑边（549）；客户端先收到停下的位置（编辑后 < 900ms），此时它持有的块还是旧版本，按旧地形预测能走到 560；以服务端位置为准；新块在编辑后 ≥ 900ms 到达，客户端按新地形从 549 再预测也被挡，与服务端一致。
2. **挖在脚下**（线性衰减 2 米）：服务端站立高度立刻变成坑底（XY 不变、不发位置），窗口内客户端仍是旧高度，块到达后相同；坑沿缓于 45°，可以走出。**抬地**（3 米）同理。
3. **陡壁深坑**（5 米直壁）挖在脚下：寻路答没有路，直线走一步不动；填平后照常走。

客户端预测与本地地形重建在测试里用同一份原始包加客户端经生成的 `ApplySyncTerrainChunkDao` 收到的块；Unity 侧的预测与纠偏表现未做（T03 / T04 与客户端验收）。

### 22.6 验证与测量

- `gameplay/terrain`：坡度 / 台阶 / 下落边界（正交 100 / 110、对角 140 / 150、落差 300 / 310）、跨块与未装入、地图边缘、水（涉水 / 游泳 / 上岸 / 不能游泳）、负高度与平移不变、对角不切角、占位与 `Bounds`、平地直线与确定性、随机起伏地形上路线逐步可走、改块后重算、坡度代价、预算 / 无路、并发改块中寻路、加权代价上界。`gameplay/movement`：Surface 停步、游泳减速、未定义节点、nil Surface 逐位相同、真实 Walker 上改块后下一拍按新数据。demo：上面三组端到端 + 场景运行时的有 / 无地形对照。
- 变异 11 项（规则边界、不切角、游泳站立高度、未装入块猜平地、最优搜索、移动忽略 Step / 游泳速度、玩家移动不接 Surface、寻路不走高度场、Standable 不看地形、无地形时寻路改道）都有用例变红，原文在证据目录。
- 同机基准（Apple M5，机器有后台负载与约 14GB 交换占用，波动大，取 6 轮中位数）：`Walker.Step` 约 10ns、0 分配；寻路 50m / 200m（起伏）约 73µs / 170µs，平地 200m 约 62µs，扫满 4096 节点约 1.2ms；对照现有格子 A*（四方向、单位代价）50m / 200m / 扫满约 271µs / 250µs / 1.28ms。一拍移动（5Hz、一拍 12 单位）格子 + Walker 约 288ns，只有格子约 233ns。最坏 1.2ms 的寻路与原来的格子 A* 同量级，都在 `MoveTarget` 的 Nest handler 里同步执行。

### 22.7 未完成与待维护者决定

维护者 2026-11-09 确认下列各项全部按推荐（★）处理：加权寻路保持 1.5 倍；直壁陡坑困人保持现状（玩家自行填平或挖台阶）；开地形的普通场景改取地形图大小（先测 AOI 索引在大世界上的成本；已实施，见 [§23](#23-普通场景取地形图大小实施结果2026-11-09)）；跨格移动在 T05 合入后给 cellworld 加地面钩子、积分时按 `Walker.Step` 截停。

| 项 | 现状 | 选项（推荐标★） |
| --- | --- | --- |
| 跨格移动 / 交接（T06 剩余） | 分区场景 cellworld 积分没有地面钩子；本轮按要求不改 cellworld（按★实施，见 [§24](#24-p3-跨格移动按地形判定t06-剩余2026-11-09)） | ★ T05（Ghost 实时副本）合入后给 cellworld `NodeConfig` 加地面判定钩子（积分时按 `Walker.Step` 截停），交接用目标格的 Ghost 副本；或分区场景首版不判高度（只做水平移动） |
| 加权寻路（实施方先定） | 估价 1.5 倍：代价至多比最优多 50%（实测 2.9%） | ★ 保持 1.5；或最优（1000，起伏地面 200m 超出 4096 退回直线走）；或降上坡代价 |
| 陡坑困住 | 直壁深坑挖在脚下时玩家走不出来，服务端不替玩家找出路 | ★ 保持（玩家可填平 / 挖台阶，编辑者需对后果负责）；或检测被困后 `Place` 到最近可站节点；或编辑校验禁止在有人站的节点下挖超过 `MaxDrop` |
| 普通场景大小 | **已实施（[§23](#23-普通场景取地形图大小实施结果2026-11-09)）**：开地形的普通场景取地形图大小，AOI 块边长不变；块表放不下（超过 16 × 16 块，1024m 见方）的地形图在启动装入前拒绝、改走分区场景。实施前：仍是 `DefaultConfig` 1000 × 1000（10m 见方），比地形图小 | ★ 开地形时场景世界取地形图大小（要先测 AOI 索引在大世界上的成本）；或保持，大地图走分区场景 |
| 路线开头折回节点中心 | ≤ 约 0.7m，换来“每段只跨一个节点边界” | ★ 保持；或去掉起点节点中心（要证明第一段只经过相邻节点） |

## 23. 普通场景取地形图大小实施结果（2026-11-09）

已合入 main（分支 `feat/terrain-scene-size`，基于 `618ee8c5`）。实施 §22.7 维护者确认的一项：开了地形的普通场景（不分区），场景世界取地形图大小；前提是先测 AOI 索引在大世界上的成本。证据与复跑命令见 [证据目录](evidence/terrain-scene-size-20261109/README.md)。

### 23.1 测量：世界大小对 AOI 的成本

场景同步的 Interest 里，只有 `spatial.BlockIndex` 的块表与世界大小有关：它按块数**急切分配**（每块一个指针 + 一个带读写锁的块，约 56 字节），上限 `spatial.MaxBlockCount` = 2^20 块，超出 `NewBlockIndex` 返回 `ErrTooManyBlocks`。AOI 的其余结构（观察者、可见集、扩展块）都是稀疏 map；观察者的块预算 `worstObserverBlocks` 只看 `MaxLeaveRadius / BlockSize`（世界只会把它截小），demo 视野上限 300、块边长 100 时每个观察者最坏 64 块，与世界大小无关，`MaxObserverBlocks` / `MaxPlayerBlocks` 都不会因世界变大被突破（`TestAOIWorldSizeBlockLimit`）。场景实体的地面 `GridTerrain` 只存障碍点（稀疏），寻路与 `Place` 有固定上限（4096 节点、16 圈），都与世界大小无关。

基准 `BenchmarkAOIWorldSize`（`framework/sync/entitysync/policy/benchmark_test.go`）：视野参数取 demo 普通场景，同一群人（512 个目标、128 个跟随目标的观察者，间距 25 / 10）放在各个世界的中央；6 轮交替的中位数（Apple M5，交换占用约 15.8GB，只作同一次运行里的对照）：

| 世界（世界单位 = 厘米） | 块边长 | 块数 | 构造 | 块表内存 | 一次完整 GC | 每批移动（间距 25 / 10） |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 现有 1000 × 1000（10m） | 100 | 100 | 1.1µs | 6.3KiB | 0.21ms | 254µs / 4.05ms |
| demo 缺省包 2 × 1 块（128m × 64m） | 100 | 8,192 | 80µs | 449KiB | 0.24ms | 251µs / 4.17ms |
| 16 × 16 块（1024m，块表上限） | 100 | 1,048,576 | 12ms | 56MiB | 3.2ms | 255µs / 4.53ms |
| 1w × 1w（157 × 157 块） | 100 | 约 1 亿 | 拒绝（`ErrTooManyBlocks`） | — | — | — |
| 1w × 1w | 1000 | 1,010,025 | 11ms | 54MiB | 3.0ms | 670µs / 4.88ms |
| 1w × 1w | 6400（同地形块） | 24,649 | 0.27ms | 1.3MiB | 0.33ms | 640µs / 4.91ms |

结论：

- **块边长不变时，移动与批量重评的成本与世界大小无关**（块定位是数组下标，候选只看订阅块），世界变大只多块表：构造时间、常驻内存、每次 GC 要标记的指针都随块数线性增长。demo 缺省的 2 × 1 包只多 0.4MiB、80µs。
- 块边长 100 下块表能容下的最大地图是 2^20 块：方图 1024m（16 × 16 块），代价 56MiB、构造 12ms、每次 GC 多约 3ms 标记。
- **1w 地图在普通场景里不可承受**：块边长 100 时被块表上限拒绝（若允许约 6GB）；放大块边长到 1000 仍要 54MiB 与每次 GC 约 3ms，且每个块 10m 见方、候选变多，稀疏人群移动成本 +160%（254 → 670µs）；放到 6400 内存小了，移动成本同样 +150%。

### 23.2 接法

- **直接用地形图 `Meta.Bounds()` 作场景边界，AOI 块边长不变（100）**：`gamescene.TerrainConfig(bounds)` = 缺省配置换成地形图的宽高（视野半径、块边长、出生点 (500, 500) 都不变——地形图至少一块 64m，总包含它）。场景实体的地面、场景同步的 AOI（`replicatedWorld`）、Walker 的 `Bounds`（`SetSurface` 夹到场景）三者同一边界。
- **上限**：块表放不下的地形图（`DivideCeil(宽, 100) × DivideCeil(高, 100)` > `spatial.MaxBlockCount`，即超过 1024m 见方的面积，如 17 × 16 块、1w 方图）在 `terrainRegion` 里拒绝——`terrainmap.Open` 读出包头后、装入任何块之前，进程启动失败，错误说明改用分区场景（`scene.partition`）。分区场景不受这个限制（1w 方图按格装入不变）。
- **接线**：服务 Init 改为先 `startTerrain` 再建场景实体：`sceneWorld(partition, terrainMap)`（没有地形或分区场景取 `lifecycle.WorldSceneConfig()`，即原来的缺省地图）交给 `lifecycle.EnsureWorldScene(ctx, registry, world)`；生成的 `SceneLifecycle.GetOrCreate` 不能带建造参数，场景单例改用手写的 `GetOrCreateOn`（同一 Get → Create → 已存在再 Get 的顺序，`EntityCreateParam.Param` 带 `gamescene.Config`，场景实体 `OnInitFinish` 本来就读它）；已存在的场景不在这张图上时报错，不静默用错地图。`NewScene` 用 `replicatedWorld`（普通场景 = `sceneWorld`，分区场景仍是网格），与场景实体同一来源。
- **不开地形时不变**：`sceneWorld` 返回 `lifecycle.WorldSceneConfig()`（= `DefaultConfig()`，`TestTerrainSceneWorldAndItsLimit` 逐字段比较），场景实体、场景同步 AOI 与之前相同；其余既有用例不改。

**实施方先定**（方案未覆盖；维护者 2026-11-10 确认按推荐）：

1. 上限取框架块表的上限（16 × 16 块、56MiB），没有另设更低的 demo 预算；
2. 出生点仍是 (500, 500)，没有改到地图中央；
3. 视野半径（进入 1.2m、离开 1.5m、最大 3m）不随地图变大——这是 demo 原有的尺度（移动 0.6m/s），本轮只改世界边界；
4. 超限在启动装入时拒绝（读包头之后），不在 `--check-config` 里拒绝：配置校验不读地形包文件。

### 23.3 验证

- 生成的 demo（`terrain_move_test.go`）：`TestTerrainSceneIsTheTerrainMap`（场景地面、Walker 与 AOI 都是地形图；原来 10m 之外进场景、两人互相看见、寻路走到地图角上、瞬移到远处；地图外的目标 / 瞬移被拒、位置不动；`Place` 把图外的点落到图里）；`TestTerrainSceneWorldAndItsLimit`（无地形 / 分区时是缺省地图、普通场景有地形时是地形图；16 × 16、32 × 8 块可以，17 × 16、16 × 17、157 × 157、1024 × 1 拒绝且指向分区场景；1w 方图在分区场景里照常）。`runtime_test.go` 的 `TestASceneOnTheTerrainMapCoversAllOfIt`：地形图上 `Standable` / 寻路 / `Place` / 刷怪（`Refresh.Due` 的点都可站）。
- 原有 P3 端到端用例里“横穿场景”的墙与湖原来只横穿 10 个节点（场景 10m）；场景变成 64m 后从第 10 个节点绕得过去，`TestTerrainWalkIsHeldBySlopesAndRoutesAroundEdits` 因此变红（“a goal behind a wall across the whole map was routed”），改为横穿 64 个节点（`terrainMoveNodes`），断言不变。
- 变异 5 项（`sceneWorld` 永远取缺省、`TerrainConfig` 不设上限、场景实体忽略建造参数、`terrainRegion` 不查上限、场景同步 AOI 仍取缺省）都有用例变红，原文在证据目录。

## 24. P3 跨格移动按地形判定（T06 剩余，2026-11-09）

已合入 main（分支 `feat/terrain-cross-cell`，基于 `58b9d9f9`，T05 已在其中）。按 §22.7 维护者确认的推荐（★）实施：cellworld 的 `NodeConfig` 加地面判定钩子，积分时按 `terrain.Walker.Step` 截停；交接之前读本格视图里邻格块的 Ghost 副本（T05 的 `Replicator` 推来的已提交版本）。完成判据对照[进度总表 T06](TERRAIN-STATUS.md)；证据、命令与红文本见 [evidence/terrain-p3-xcell-20261109](evidence/terrain-p3-xcell-20261109/README.md)。本节编号接在 §22 之后（§23 是同期另一轮“普通场景取地形图大小”）。

### 24.1 cellworld 的地面钩子

- **接口**：`cellworld.Surface`（`Cell` / `Step`，与 `movement.Surface` 同形；`terrain.Walker` 两者都实现）。`NodeConfig.Surface func() Surface`：角色每拍开始取一次（`advance`），各子步共用；为 nil 或返回 nil 时积分与没有钩子逐位相同。cellworld 不引用 `gameplay/terrain`（依赖方向不变）。
- **走法**（`actor.go` `integrate` / `walkOn`）：先按原规则算出这一子步的终点并夹到世界内，再沿直线逐个整数点走：第 i 点 = 起点 + 位移 × i / n（n = max(|dx|, |dy|)），相邻两点每个分量至多差 1，所以地面格每次至多变到相邻的一格，对角的不切角由 `Step` 判断。每点问一次 `Cell`，进入新格问一次 `Step`。与 movement 的 0.5 单位采样相比：cellworld 的位置是整数，按整数点走停下的点精确到 1 单位；地面格对 cellworld 不透明，不按格边界解析求交。
- **被挡**：停在上一个整数点。玩家速度清零——与停在世界边缘相同，等 Base 的下一个移动输入；NPC 两个分量都反向原路返回——与碰世界边缘反弹相同，地形的挡没有“哪条轴碰了边”，原路一定走得通（除非这期间又被改了）。同一拍剩下的子步照常积分。计 `NodeStats.SurfaceBlocks`。被挡的角色不会越过冻结线，也就不交接。
- **减速**（游泳 60%）：预算 = 这一步**未被世界边缘夹过**的位移的 max(|dx|, |dy|) × 1000，每点花 ⌈10⁶ / 速度千分比⌉；用完停下、速度不变。世界边缘的速度规则只在走到终点时生效（减速后没到边缘不反弹）。度量是 Chebyshev：不夹边时与按欧氏距离折算完全相同（同一条直线上的比例），夹边时是近似。（2026-11-10 起：积分按千分位累计零头，减速改为按速度千分比缩放位移、进入速度不同的格时缩放剩下的位移，不再按整点丢余数；交接 Offer 带零头，线上格式 v8。见 [cellworld README“位置零头”](../../gameplay/cellworld/README.md)。）
- **没有定义**：站立的地面格没有定义（块没装入、地图外）时一步不走，按被挡处理（§22.1“块没装入不猜”）。
- **跨格与交接**：分区进程的地形视图装着本格外扩 `ghost_band` 的块，角色交接之前至多走出本格 H + 一步，`ghost_band ≥ R + H` 保证这段在视图里。邻格写的块由副本推送更新（提交到副本约 0.2ms～12ms，推送丢了至多一个续期 1s 补齐），所以交接之前按邻格最新已提交的地形判定；副本装不进来（换版前后包不同、记录损坏）时那一块不在视图里，角色停在它前面。交接之后角色在目标格由目标格的 Node 积分，按目标格自己的视图判定——两个格的视图独立，不需要协议改动，线上格式不变（v7）。

### 24.2 game-demo 接法

- `terrain_partition.go` 新增 `partitionSurface(m, grid)`：`m.Walker()`（缺省规则 + 建筑占位钩子），`Bounds` 夹到分区世界；分区世界比地形图大时启动记 Warn（图外不可走）。
- `partition.go` 的 `startPartition`：开了地形（`terrainmap.Active() != nil`）且本进程托管格时装到 `NodeConfig.Surface`；只做 Base、不开地形的进程不装，行为与之前相同。
- 不改 SceneActor / Monster / Player 的 DAO 与 cellworld 的线上格式；服务端位置仍只有 X / Y（站立高度由 `Walker.StandZ` 推出，§22.1）。

### 24.3 验证

| 用例 | 内容 |
| --- | --- |
| `cellworld` `TestANilSurfaceIntegratesExactlyLikeNoSurface` | 2000 组随机位置 / 速度 / 拍长（含世界边缘反弹、越界冻结），NPC 与玩家，每组 4 拍：不设、返回 nil、处处放行三种逐位相同 |
| `TestSurfaceStopsTheWalkAtTheLastPointBeforeARefusedCell` | 玩家停在 1149（被挡格从 1150 起）、速度 0、下一拍不动；NPC 折回、下一拍走回 1049；对角走 Step 从没被问到不相邻的格 |
| `TestSurfaceSlowsTheWalkAndAnUndefinedCellHoldsIt` | 速度 500‰ 的格里一拍走一半；朝世界边缘游时没到边缘不反弹、到了才反弹；站在没有定义的格上一步不走 |
| `TestSurfaceBlockBeforeTheBoundaryKeepsTheActorReal` | 挡在边界前一格的 NPC 一直是本格 Real；不挡时同一拍冻结交出 |
| `TestCrossCellWalkDoesNotGuessAMissingGhostChunk` | 真实 `terrain.Walker`：邻格块没装入时玩家停在 12749（块从 12750 起）、NPC 折回、站在没装入的块上不动；块装入后走进去 |
| `TestCrossCellWalkFollowsTheNeighboursTerrain` | 真实 Node、回环传输、两个格进程各自的视图与 `Replicator`、Base 与玩家：邻格在它写的块上立墙、推到副本，玩家越过边界后在墙前 13249 停住、一直是旧格的 Real（冻结线 13800）；墙挖平后交接到新格；新格在只有它装着的块上挖坑，玩家停在 19449（旧格的视图里没有那块，若旧格在判定会停在 19149） |
| demo `TestTerrainPartitionWalkAcrossCellsFollowsTheEdits` | 两个格进程：正式 `terrainmap.Map`、改块 Nest 事务（`Brush`）、`partitionTerrain` 副本；各起一个真实 cellworld Node（替身 Scene / NPC 实体、自己的 Nest 与心跳），地面钩子就是 `partitionSurface`。B 紧挨分界线抬起 5 米的台、推到 A 的副本，A 的 NPC 越过边界后在台前折回、从没交给 B；台降回后 NPC 交到 B；B 挖 5 米深的坑，NPC 在坑前折回 |
| 变异（证据目录） | 钩子不生效（= main）：上面 cellworld 五条与 demo 用例全红（例：“player walking into the refused cell: at (1400, 5000) v (2000, 0) real, want stopped at (1149, 5000) with v 0”；demo NPC 走到 13786 交给 B）；只看终点、玩家不停、NPC 不折回、忽略速度、站立格没定义也走、没走到终点也套用边缘规则、预算按夹过的终点算——各有用例变红 |

成本见 cellworld README“地面钩子”：全速玩家一拍约 0.8～1.7µs、慢 NPC 约 85～155ns，0 分配（同机后台负载下 3 轮，波动大）。

### 24.4 未覆盖与限制

- demo 用例里的 NPC 是替身实体：demo 的 SceneActor / Monster 经进程级的 `partition.Node()` 找 Node，一个测试进程里装不下两个格；`startPartition` 里装钩子的三行只有真实进程演练（`scripts/test-seamless-demo.sh` 开地形的配置）覆盖，本轮没跑（真实依赖由维护者跑）。**2026-11-10 已补**：`ROOST_SEAMLESS_TERRAIN=1` 的演练覆盖这几行，去掉装钩子的那一行演练变红（[§26](#26-开地形的分区场景真实多进程演练t05t062026-11-10)）。现有演练配置的 `cell_size` 3000 不满足开地形的要求（§21），要另配。
- 玩家端没有新的 TCP 用例：玩家停下后客户端看到的是 witness 里的位置（与停在世界边缘相同的路径），cellworld 的 `TestCrossCellWalkFollowsTheNeighboursTerrain` 用 Base 收到的 witness 断言。
- 副本的新旧：判定用本进程当时持有的副本版本。拥有者刚提交、副本还没到的那几毫秒（推送丢了时至多 1 秒）里，旧格可能让角色走过邻格刚抬起的地方，或在刚挖平的地方停下；交接后新格按最新数据判定。T09 规模下副本延迟未测。
- 刷怪点与进场位置不按 `Standable` 检查：站在不可走节点上的角色一步不走（不猜），不会被挪到附近能站的点。
- 每拍按整数点问 `Cell`，成本与速度成正比；T09 规模（单格数千玩家）的 CPU 未测。

### 24.5 实施方先定的取舍（维护者 2026-11-10 确认按推荐）

| 项 | 现状 | 选项（推荐标★） |
| --- | --- | --- |
| NPC 被地形挡住 | 两个分量反向，原路返回 | ★ 原路返回（与世界边缘反弹同类，一定走得通）；或停住（速度清零，NPC 要靠 AI 另给速度，现在分区 NPC 没有 AI，会永远站着）；或只反向“碰到的”轴（地形没有这个概念，要按被挡的格相对方向猜） |
| 玩家被地形挡住 | 速度清零，等下一个移动输入 | ★ 清零（与停在世界边缘一致，被挡后不再每拍问 `Step`）；或保持速度一直“推着墙”（地形改开后自动继续走，每拍多一次 `Step`，客户端要区分“走不动”与“停下”） |
| 分区世界比地形图大 | 图外不可走（Walker 规则），启动记 Warn | ★ 不可走（不猜，与“块没装入”一致）；或图外不判高度（只按世界边缘走）；或启动拒绝（配置校验要求地形图覆盖分区世界） |
| 刷怪点 / 进场位置 | 不检查 `Standable` | ★ 先不查（demo 原型，站不住的角色只是不动）；或刷怪 / 进场时按 `Standable` 跳过或挪到最近能站的节点（同普通场景的 `Place`） |
| 每点问 `Cell` 的成本 | 全速玩家每拍约 1µs | ★ 先保持，T09 实测后再定；或 `Surface` 增加节点边长，按格边界解析求交（每拍只问跨过的边界，接口变化） |

## 25. P4 水与建筑实施结果（2026-11-10）

已合入 main（分支 `feat/terrain-p4`，基于 `960e485c`）。完成[进度总表](TERRAIN-STATUS.md) T07 的服务端部分：区域水位线（D3）与普通场景的建筑（放置校验、地基整平、拆除、占位、并发、重启与换版处置）。证据、命令与变异红文本见 [evidence/terrain-p4-20261110](evidence/terrain-p4-20261110/README.md)。§26 留给同期另一轮。

### 25.1 水位线（D3：全局 / 区域）

- **数据来源：原始地形包的元数据**（不是配置键）。`terrain.Meta.WaterRegions`：块对齐的半开矩形 `[MinX, MaxX) × [MinY, MaxY)`（块坐标）与区域水位 `Level`（世界高度，厘米；`math.MinInt32` 表示这片没有水）。区域在图内、非空、互不重叠，至多 1024 个（`MaxWaterRegions`），`Meta.Validate` 检查。一块只有一个水位：在区域里取区域的，否则取全局 `WaterLevel`（`Meta.WaterLevelOf`）。选包而不是配置：服务端、C# 客户端读同一个文件、同一份 SHA-256 校验，不会一端按配置、一端按包；换水位就是换包，走 D13。块对齐让一块一个水面，客户端岸线（T03）每块只有一个等高值。
- **包格式版本 2**：包头第 68 字节是区域数，区域表（每项 20 字节：四个 int32 块坐标 + 水位）紧跟块索引表之后，包头 / 索引的 SHA-256 覆盖它。版本 1 的包拒绝（未上线不兼容，升级须用新 `terrainpack` 重新生成）。
- **同一来源**：`View` 在 `NewView` 时按块算好水位表；`IsWater` / `Probe`（`Ground.WaterLevel`、`Depth()`）/ `WaterDepth`、`Walker` 的游泳判定与 `StandZ`、建筑的“不在水下”都按“地面高度 < 所在块的水位”判（等于不算水）。所以挖到水位以下就是水、填回水位以上就是陆地，同一份包与同一组编辑在任何一端得到同一结论（`TestDigALakeFillItBackByTheChunksWaterLevel`、`TestWalkerSwimsByTheSameWaterLevelAsTheQueries`）。区域边界两侧水位不同时，行走按两节点各自块的水位判（湖边 30m 落差两个方向都挡，449.5m 湖岸照常走）。
- **C#**：`TerrainMeta.WaterRegions`、`TerrainPack` 版本 2、`TerrainView` 每块水位与 `WaterDepth`，金样 `client/spec/terrain-golden.pack` 与 `terrain-queries.json` 加了两个区域（块 (1,1) 水位 +300m，块 (0,1) 无水）与逐查询的 `water_level`，Go 与 C# 逐点一致。
- **工具**：`terrainpack -water-region minX,minY,maxX,maxY,level`（可重复）。

### 25.2 建筑（普通场景）

实体与数据（全部按正式 codegen / demo 模板生成，`codegen/internal/roost/demo.go` 的步骤）：

| 名字 | 内容 |
| --- | --- |
| `Building`（kind 13，World 等级，集合 `buildings` schema 1） | 状态（0 空地 / 1 站着）、属主、类型、朝向、占地宽高、地基世界高度（没整平是 MinInt32）、放置时的 `BaseRevision`；只持久不同步。实体 ID 由锚点节点（占地左下角的 1m 采样点）决定，只经 `GetOrCreate`；拆除清空记录、不删档（确定性 ID 删档后不能再建，同一锚点还要能放） |
| `TerrainChunkDao` schema 4 | 加 `Lots []int64`（store 6，这块上的建筑占地，每项 int64 打包锚点与宽高）与 `LotRevision`（store 7，每次增删加一）；只持久不同步，不动 `Version` |
| `TerrainEditorDao` schema 2 | 加 `Buildings`（store 3，玩家站着的建筑数） |
| 协议 `Building`（10039） | `op` 1 放置（类型、锚点、朝向、是否整平）/ 2 拆除（锚点）；答复建筑状态、各块版本、`last_seq`；业务码 100060～100065（`building_disabled` / `building_bad` / `building_slope` / `building_underwater` / `building_limit` / `building_missing`），其余与地形编辑共用 |

规则（`game/terrainmap/building.go`）：

1. **身份**：与笔刷共用（玩家 ID, 编辑序号）与同一份账本（D18）：重发答 `duplicate`，窗口外答 `terrain_seq`。拆除的重发有一点不同：锚点上已经没有建筑时直接答 `building_missing`（不进事务、不建实体），对客户端的结论与 `duplicate` 相同。
2. **校验**（全部在第一次写之前，拒绝零写入）：类型 / 朝向 / 占地在图里；块归本进程且装着；锚点上没有站着的建筑；占地的每个节点不在块的 `Lots` 里、业务钩子 `Rules.Occupied` 不占；玩家建筑数 < `Policy.MaxBuildings`（缺省 16）；高差（占地各节点最高 − 最低）不超过类型的 `MaxRise`（不整平）或 `MaxCut`（整平）；每个节点（整平时按整平后的高度）不低于所在块的水位；每块至多 64 份占地。可选的业务权限钩子 `BuildRules.AllowBuild`（`Rules` 同时实现它时生效，事务之前）。
3. **整平**：把占地压到占地各节点高度原始值的平均（四舍五入），是同一笔事务里的 `SurfaceComponent.Apply`（整字段守卫照常、版本加一、提交后进视图并登记同步）。配额与笔刷同一个桶：每次至少 20 点，整平再加会改高度的节点数（事务前在视图上估算）。
4. **事务**：`handler BuildingPlace / BuildingRemove`（`rollback=undo durability=strict`），目标是占地覆盖的块（至多 2 × 2，组目标）、建筑实体与玩家账本；写建筑、每块 `AddLot`、账本记序号与建筑数。拆除只许属主（`terrain_denied`），地形不复原。
5. **串行化点：块的锁**。权威占地在块上（`Lots`），重叠检查在块的锁里读它；两份占地只要有一个公共节点，就有一块是两笔事务共同的目标，Nest 按块串行，后到的那笔看得见前一笔已提交的 `Lots`。笔刷同样锁块、改到 `Lots` 的节点答 `terrain_occupied`。所以并发放置不会重叠，也不会有笔刷改到正在放的建筑脚下。
6. **占位推导（行走 / 寻路）**：`Map.Walker()` 的 `Occupied` = 进程内占位表 OR `Rules.Occupied`。占位表每块一个原子槽位（占地 + 由它算出的 4096 位位图），`Load` 时按块记录建好，放置 / 拆除在 `nest.AfterCommit` 里按 `LotRevision` 换新（只接受更大的修订号，回调乱序也不回退）。它只给行走读（无锁，可以在 short 池里），不用于放置检查：提交与发布之间有一小段时间。寻路的路线不因占位变化失效（`Route.Fresh` 只看块版本）；路线上新出现的建筑由移动逐节点挡住，下一次请求重新寻路。
7. **重启**：建筑、块的占地、账本都随 DataEngine 落库；`Load` 按块记录发布占位（Version 0、只有占地的记录也算），启动时 `ReconcileBuildings` 对每份占地冷加载它的建筑、一笔事务（`handler BuildingRebase`）核对“建筑站着、占地相同、每块都记着它”。对不上（正常不会发生）计 `Mismatched`、记 Error、什么都不改。
8. **原始地形换版（D13 之后）**：块的地形改动照旧丢弃（`Discard` 不动 `Lots`），建筑与占地保留；建筑的 `BaseRevision` 与包不同时：整平过地基的，把占地重新整平到原来的地基**世界高度**（与 `HeightBase` 无关；超出新包的高度范围算不合规）；然后按放置规则查水位与高差，不合规的保留、计 `Unfit`、记 Warn（不自动拆玩家的资产）；记下新版本，再启动不再处置。重新整平是一次地形编辑，不计玩家配额。
9. **分区场景**：不接建筑（`startTerrain` 只在不分区时调用 `EnableBuildings`），放置 / 拆除答 `building_disabled`。邻格 Ghost 带还没有建筑占位的副本，跨格建筑是 P5。

**与第 6 节设计的差别**：第 6 节写的是“占位不单独持久化，加载时由本格建筑实体推出”。实施改成占位记在块上（持久），原因是串行化点：重叠检查要在一把放置事务一定会拿的锁里读到已提交的占位。块的锁满足（两份重叠的占地必有公共块）；由建筑实体推出的内存索引要么在事务外（AfterCommit 之后才更新，并发放置在这段时间里看不见彼此），要么是事务里要回滚的进程内状态（违反 A1：事务改的状态在 DAO 里）。代价：每块多一个至多 64 项的 int64 列表（约 0.5KB），放置多写一次块的记录（不改 `Version`、不同步）。

**同步与预算**：本轮建筑实体不同步（DAO 字段只持久）：放置者按答复显示，别的玩家暂时看不到建筑，见下面待确认。整平改的地形走地形块同步：一座 8 × 6 的 hall 至多改 48 个高度点，增量约 0.4KB，与一次 r≈2m 的笔刷相当，受同一配额约束，不改变 D15 的地形命名空间预算与 50KB/s 的估算。

### 25.3 验证

| 用例 | 内容 |
| --- | --- |
| `gameplay/terrain` `TestWaterRegionsRoundTripAndAreChecked` | 区域写读往返；越界、重叠、空区域、超过 1024 个、改了一个字节的水位都拒绝 |
| `TestDigALakeFillItBackByTheChunksWaterLevel` | 区域里挖到区域水位以下是水（深度、`WaterLevel` 正确），填回到水位是陆地；无水区域挖多深都不是水；区域外仍按全局水位 |
| `TestWalkerSwimsByTheSameWaterLevelAsTheQueries` | 每个节点的游泳判定与查询的“是水”一致，`StandZ` 是所在块的水位；区域边界两侧的落差与湖岸 |
| `TestClientTerrainGolden` / C# `PackTests` | 金样每个区域都被查询到，Go 与 C# 逐点相同；C# 坏包（版本 1、区域数超限、改一个区域字节、重叠）拒绝 |
| demo `TestBuildingPlacementChecks` | 合规放置；同一占地、重叠一个节点、锚点上换类型、区域水下、70cm 坡不整平、7m 坡整平、未知类型、坏朝向、出图、负锚点都拒绝且零写入；跨两块整平到 5001（50010cm），两块版本 1、占地外不动；上限 3 |
| `TestBuildingPlacementAppliesOncePerSeq` | 同一序号重发只建一座；笔刷用过的序号答 Duplicate；跳太远答 Ahead；都不写 |
| `TestBuildingRemoval` | 非属主 `terrain_denied`；空锚点 / 非锚点 `building_missing` 且不建实体；拆除后占位与账本回去、地基不复原；同一锚点再放 |
| `TestBrushUnderABuildingIsRefused` | 笔刷改到建筑脚下 `terrain_occupied` 零写入；旁边照常 |
| `TestWalkersGoAroundBuildings` | 放置前直线路线穿过占地；放置后 `FindPath` 绕开、`Step` 进不去；拆除后又穿过 |
| `TestBuildingConcurrentPlacementsNeverOverlap`（`-race`） | 16 个玩家各 12 次在 30m × 20m（跨两块）随机放 hut：约 20 座站着、约 170 次 `terrain_occupied`，两两不重叠，块记录 / 占位表 / 账本一致 |
| `TestBuildingsSurviveARestart` | 四座建筑（含跨两块整平、跨四块）停机落“库”后重启：对账 `Standing 4`，建筑、地形、占位、账本一致；上限照样生效、重放答 Duplicate、照常拆除 |
| `TestBuildingsAcrossABaseRevision` | 换版：两块地形改动丢弃；三座建筑全部 `Rebased`，整平的地基回到 50010cm（新地形 52000cm）、占地外是新地形；新版本里在水下的计 `Unfit` 但保留；再启动不再处置 |
| `TestBuildingReconcileReportsAMismatch` | 库里删掉建筑文档：对账 `Mismatched 1`，块的占地不动、照样挡路 |
| `TestBuildingsAreRefusedWithoutEnableBuildings` | 不接建筑时放置 / 拆除 `building_disabled`，零写入 |
| controller `TestBuildingRefusalsMapToTheirCodes` 等 | 建筑的拒绝先认，其余同地形编辑；`ErrLotMismatch` 与基础设施错误答 internal |
| `TestBuildingRestartRealMongo` | 真实 DataEngine 上的放置 / 拆除 / 重启 / 换版；门变量 `ROOST_TERRAIN_IT_MONGO_URI`，本轮**没有运行**（不连真实库），`scripts/test-demo-generated.sh` 已挂入 |
| 变异 | 水位：7 项 Go + 2 项 C# 全红。建筑 14 项里 13 项红（锁内不查重叠、水位用全局、行走 / 笔刷不看建筑、Load 不发布、换版不重新整平 / 不查不合规、拆除不查属主、账本不计数、坡度上限、丢弃清掉占地、不查上限、不记占地）；“重叠检查改读发布表”没变红，见 25.4 |

### 25.4 未覆盖与限制

- **“锁内读块的占地”没有专门的红测试**：把重叠检查改成读进程内发布表的变异，在 `-race` 与 5 轮下都没变红——测试用的提交器在内存里，提交与发布之间只有微秒，16 × 12 的并发放置碰不到。保证来自结构（检查与写在同一笔事务、同一组块锁里），“锁内不查重叠”的变异会让并发用例出现重叠（证据里有原文）。真实 WAL 下这段时间是 fsync 的量级，更容易撞上，所以不能改读发布表。
- 建筑不同步（见待确认）；建筑没有“建造中 / 损坏”等状态，只有站着 / 空地；类型表是 demo 的常量，不是配置。
- 真实 Mongo 用例没有运行；整进程（`startTerrain` 的新接线、分区场景不接建筑）只有编译与单元覆盖，没有真实进程演练。
- 换版处置只按“水位、高差”判不合规，不处理新地形与地基之间过大的挖填（重新整平不看 `MaxCut`）。
- 拆除地形不复原；玩家离线 / 删号后的建筑归属不处理。
- 每块的 `Occupancy` 在锁内按 `Lots` 现算（至多 64 份 × 占地节点），没有缓存；没有测成本。

### 25.5 实施方先定的取舍（维护者 2026-11-10 确认按推荐）

| 项 | 现状 | 选项（推荐标★） |
| --- | --- | --- |
| 水位数据来源 | 地形包元数据，块对齐区域 | ★ 包元数据（两端同一文件同一校验，换水位走 D13）；或服务端配置键（运营可改，但客户端要另行下发、与包可能不一致）；或节点粒度的水位图（更细，包更大、客户端岸线更复杂） |
| 占位的权威记录 | 块上的 `Lots`（持久） | ★ 块上持久（串行化点就是块锁，见 25.2）；或按第 6 节由建筑实体推出（需要在事务里维护可回滚的进程内索引，违反 A1，或接受并发放置的窗口）。**2026-11-10 起改由 [§27](#27-建筑实体化与同步2026-11-10) 处理**：`Lots` 降为服务端内部的串行化索引、不同步，客户端的占位由建筑实体 + 配置推出（§27.4） |
| 建筑的同步 | 不同步 | ★ 建筑自己的命名空间（`building`，按块的 Interest 一样订阅，单座约 40 字节，D15 单独计上限）；或把类型 / 朝向打包进块的 `Lots` 并让 `Lots` 随地形块同步（不多一个命名空间，但只有改过的块是同步对象，要把有建筑的块也登记）。**2026-11-10 起改由 [§27](#27-建筑实体化与同步2026-11-10) 处理**：维护者定为“建筑是普通实体、统一逻辑”，按第一个选项实施（§27.3） |
| 换版后的建筑 | 保留；整平的地基按原世界高度重新整平；不合规的保留并计 `Unfit` | ★ 保留并重新整平（不动玩家资产，地基高度玩家看得到）；或保留但不动地形（地基与新地形可能脱开）；或自动拆除不合规的（返还资源的规则另定） |
| 拆除的重发 | 锚点上没有建筑答 `building_missing` | ★ 照现在（不为找不到的建筑建实体、开事务）；或先查账本，已应用过的答 `duplicate`（多一次账本冷加载与事务） |
| 拆除时的地形 | 不复原 | ★ 不复原（与玩家挖的坑一样留着）；或按放置前的高度复原（要在建筑上存一份原地形） |
| 每人建筑上限 | `Policy.MaxBuildings` 16，demo 常量 | ★ 16（demo 原型）；正式值随玩法定 |

### 25.6 demo 缺省地形改为平地（维护者 2026-11-10）

背景：分区演练发现 `terrainpack` 的缺省包（`terrain.Generated`，seed 1）在 45° 行走规则下向东一步只有 45.5% 的边走得通（3700 / 8128，实测原文见[证据](evidence/terrain-p4-20261110/README.md)），缺省地图基本走不动。

- **平地源**：`gameplay/terrain` 新增 `Flat(meta, raw, material)`（每点同一高度与材质，图外的块报 `ErrOutOfMap`）。放在库里而不是 `terrainpack` 里：一行规则、两端（服务端与客户端工具）和别的测试都能用，且与 `Generated` 并列，`Generated` 本身逐位不变（测试、压测、客户端金样、压缩率仍用它）。
- **terrainpack**：`-shape flat`（缺省）/ `generated`；`-ground` 平地的世界高度（厘米，须落在 10cm 一档、在包能表示的范围里，不四舍五入）；`-seed` 只对 `generated`、`-ground` 只对 `flat`，给错报错。
- **尺寸**（维护者 2026-11-10 补充）：缺省 `-chunks-x 16 -chunks-y 16`，1024m × 1024m，正好是普通场景的上限 2^20 个 AOI 块（§23）。已知代价：开地形的普通场景 AOI 块表约 56MiB、构造约 12ms、每次 GC 多约 3ms 标记（§23.1 的实测）。包与视图本身很小：平地包 18,560 字节，写包约 110～160ms，打开并装入 256 块约 20～60ms、视图约 3MiB（同机 3 次，证据 `default-flat.txt`）。分区配置的示例改为 `cell_size: 51200`、`cols: 2`、`rows: 2`（四个 512m 的格正好盖住 16 × 16 块）。生成工程的测试都自己造图，不经缺省包，所以测试时间不变；`terrainpack` 自己的缺省用例在整张图上逐步检查东 / 东北两个方向（约 200 万次 `Step`），0.1 秒内结束（`-race` 约 4 秒）。没有在真实进程里用缺省包起普通场景量内存与启动时间（只有 §23 的基准）。
- **高度与水位**：缺省 `-ground` = `-water-level` + 1m（缺省水位 40000 → 地面 40100，原始值 5010；没有水时取 0）。水位缺省不变（40000，`HeightBase` 决策时维护者要求水位独立、保持原值，`-shape generated` 也因此与原缺省包同一份地形）。1m 的理由：按 25.1 的规则“地面 < 所在块水位就是水”，挖下去超过 1m 就成水域，超过 1.5m 要游泳（涉水上限 50cm），一笔笔刷（强度至多 5m）就能演示；填回 1m 以上又是陆地；平地本身离水面 1m，不会因为微小的笔刷误差变水。建筑的“不在水下”在平地上处处满足，坡度为 0。
- **BaseRevision**：D13 用“记录的 `BaseRevision` ≠ 包”判断原始地形换了，前提是同一个版本号只对应一份原始地形。缺省包的内容变了，所以平地缺省取 2；`-shape generated` 缺省仍是 1——与原缺省包同一份地形（`TestTerrainPackGeneratedShapeIsTheOldDefault` 逐块比对 `terrain.Generated`）。这样用旧缺省包改过的块，换上新缺省包时按 D13 丢弃，而不会被当成新平地上的改动叠上去。本轮还有包格式版本 2 与 chunk schema 4，升级本来就要清空数据重新生成包；版本号的改动是让 D13 语义自洽，不是迁移手段。
- **依赖缺省起伏的用例**：没有。demo 的服务测试都用 `terrain.Generated` 或自己的源造图（不经 `terrainpack`）；`terrainpack` 自己的用例改为断言平地缺省，原来覆盖“显式基准与水位”的用例加了 `-ground 0`（新规则下缺省地面 = 水位 + 1m 落在 `-height-base 0` 之下）。
- **验证**：`TestTerrainPackDefaultsAndHeightQueries`（缺省 16 × 16、1024m 见方、版本 2、每点 40100 与草、整张图每一步东 / 东北都走得通、挖 2m 是 1m 深的水、填回是陆地）、`TestTerrainPackGeneratedShapeIsTheOldDefault`（`-shape generated -seed 1 -chunks-x 2 -chunks-y 1` 就是原缺省包）、参数拒绝（未知形状、平地给 `-seed`、起伏给 `-ground`、不在 10cm 档上、低于基准、超出范围）、`gameplay/terrain` `TestFlatIsTheSameEverywhereAndWalkable`。新用例在改动前的 `terrainpack` 上的失败原文见证据。

## 26. 开地形的分区场景真实多进程演练（T05、T06，2026-11-10）

已合入 main（分支 `feat/terrain-partition-drill`，基于 `960e485c`）。补 §21 与 §24.4 留下的“开地形的无缝演练、真实 Mongo / WAL 崩溃演练、`startPartition` 装钩子的几行”。§25 留给同期 P4 那一轮。证据、命令与每步数字见 [evidence/terrain-partition-drill-20261110](evidence/terrain-partition-drill-20261110/README.md)。

**入口**：`scripts/test-seamless-demo.sh` 加 `ROOST_SEAMLESS_TERRAIN=1` 模式，复用依赖准备、生成工程、端口检查与清理；客户端步骤在 `scripts/testdata/seamless/terrain.go.txt`（与原 `drill.go.txt` 编成同一程序，`-terrain` 选步骤）。不设时原演练逐字不变（客户端只多认 `terrain` 命名空间，原演练不会收到）。另加 `ROOST_SEAMLESS_PROJECT_HOOK`（生成之后、编译之前在工程里执行一条命令）供变异验证。

**配置**：4 × 2 块平地包（演练程序写出），`cell_size` 12800（两块）、2 × 1 格（A = sid 1001、B = sid 1002），`ghost_band` 600 = `view_radius` 400 + `hysteresis` 200；1000、1004 只做 Base。演练用平地，是因为 demo 缺省程序化包上向东一步只有 45.5% 走得通，角色会被天然坡随机挡住。

**读视图**：生成 demo 新增只读 GM `gm.terrain.probe`（`internal/service/<game>/terrain.go` 的 `terrainProbe`）：每块报是否本进程写、是否装着、视图版本、副本订阅报的版本（`Map.ReplicaHeld`）与数据摘要（FNV-1a 64：高度小端 2 字节、再材质 1 字节），每点报世界高度；至多 64 块、256 点。客户端按同一规则算自己收到的块，所以“拥有者、Ghost 带、各进程客户端是同一份数据”可以逐块判定。用例 `TestTerrainProbeReportsTheView`。

**步骤与结论**（两次完整运行都通过）：

| 步骤 | 结论 |
| --- | --- |
| (a) A 改边界块与远处块 | B 的 Ghost 带在答复后一次轮询（约 3～4ms）内对齐；B、1004、A 上的客户端在答复之后 1ms 内拿到、摘要相同；远处块不进 B 的视图、只作为客户端副本；B 改 A 的块答 `terrain_not_owned` |
| (b) 真实 SceneActor | B 紧挨分界线抬 5m 的坎，坎到 A 的 Ghost 带 10～16ms；Base 在 1004 的玩家越过分界线、**停在 12949**（坎的第一个节点从 12950 起），2 秒内一直是 A 的 Real；挖平后再走 109～114ms 交给 B |
| (b) 真实 Monster | 怪困在 A 的墙与 B 的坎之间连续折回 4 次（东侧折回点 12933～12942 ≤ 12949，从没交给 B）；坎挖平后约 3.3s 交给 B |
| (c) kill -9 格 B 后重启 | 答复过的编辑重启后版本、摘要不变；A 的 Ghost 带与 B 对齐、B 的 Ghost 带补上停机期间 A 的编辑，都在 readyz 之前完成；连续编辑的账本 LastSeq = 最后答复的序号 = 块版本（在途的一笔没有生效），重发答 Duplicate；重新登录的客户端 52～88ms 内拿到 |
| 收尾 | 8 块在每个装着它的进程与三个客户端里都与拥有者一致 |
| 变异：去掉 `cfg.Surface = partitionSurface(m, grid)` | (b) 变红：玩家走过坎、交给 B、走到 x 20673 |
| 不开地形的原演练 | 14 步通过；最后的普通场景机器人压测同机一次通过（p95 13.54s）、一次失败（19.48s），与本项无关，登记 KNOWN-LIMITS“尚未确认” |

**没有发现框架或 demo 的缺陷**；演练程序自己的两处错误（没登记实体种类时 `BuildEntityID` 报错、业务码按文本比较）已改，记在证据里。

**未覆盖**：跨格寻路（没有入口）；“提交了但答复没到”的在途编辑；格 A、只做 Base 进程的崩溃与换版（D13）的真实进程演练；消息丢失 / 延迟注入、跨机、T09 规模；起伏地形上的真实进程移动。演练要真实依赖，不进 pretag。

**实施方先定的取舍**（维护者 2026-11-10 确认按推荐）：① `gm.terrain.probe` 留作 demo 的运维 / 诊断命令（★）还是只给演练用；② 演练用平地包（★），还是给 `cmd/terrainpack` 加平地 / 平缓选项、或改缓 `terrain.Generated`（包字节变化须换 BaseRevision）；③ 演练保持手动（★）还是进 pretag（多约 2 分钟、要求真实依赖）。

## 27. 建筑实体化与同步（2026-11-10）

维护者 2026-11-10：“建筑是个 entity，这样呢，是不是就统一逻辑了，不用走特殊逻辑，建筑是预制的或者生成一个整体的，数据是模型的配置 id 和模型的一些修改参数值。”本节把 §25 留下的“建筑的同步”“占位的权威记录”两项按这个方向实施。证据、命令与变异红文本见 [evidence/terrain-building-entity-20261110](evidence/terrain-building-entity-20261110/README.md)。

### 27.1 数据模型

一座建筑 = 一种预制的整体（配置 ID）+ 玩家给的修改参数值 + 放在哪、朝哪。`BuildingDao` schema 2（键 3 的 `Kind` 弃用、不得复用；未上线不迁移旧文档，升级须清空 `buildings` 与 `terrain_chunks`）：

| 字段 | store | 同步 | 说明 |
| --- | --- | --- | --- |
| `State` | 1 | 是 | 0 空地 / 1 站着（拆除不删档） |
| `Owner` | 2 | 是 | 属主；拆除、改参数只许属主 |
| `ConfigID` | 9 | 是 | 配置表 `building` 的 ID |
| `Params []int32` | 10 | 是 | 修改参数值，下标 = 配置表 `building_param` 的 `Slot`，至多 8 个 |
| `Facing` | 4 | 是 | 0～3，1、3 时配置的宽高互换 |
| `GX` / `GY` | 11 / 12 | 是 | 锚点节点（与实体 ID 推出的锚点相同，对账核对；给客户端直接摆放） |
| `W` / `H` | 5 / 6 | 否 | 放置时按配置与朝向算出的占地，服务端核对块上记录用的那一份 |
| `Foundation` | 7 | 是 | 整平地基的世界高度（MinInt32 = 没整平） |
| `BaseRevision` | 8 | 否 | 放置时原始地形包的版本（D13 换版处置） |

**占地只由配置的宽高与朝向决定，参数不改变占地**（写在 schema、DAO、`building.Spec` 的注释里）。将来参数要影响占地，须先在配置里声明（例如“某个槽是长度”），并同步改服务端的占地推导、块上的记录与客户端。

### 27.2 配置表

走正式 configdata 链路（`configs/schema` + `configs/table`，`roost generate` 转 JSON、生成 loader，configdata 每次加载 / 热更新按 tag 查行内规则）：

- `building`（`configs/schema/building.go`）：`ID`、`Name`、`Model`（客户端预制体 / 模型标识，服务端不读）、`W` / `H`（1m 节点，≥ 1）、`MaxRise` / `MaxCut`（厘米，原 `Catalog` 的字段）、`MustLevel`（只能整平放置）。demo 四行：1 hut 4×4、2 hall 8×6、3 tower 3×3（与原 `Catalog` 相同）、4 keep 12×12 必须整平。
- `building_param`（`configs/schema/building_param.go`）：`ID`、`Building`（`ref=building`）、`Slot`、`Name`、`Min`、`Max`、`Default`。demo：hut 1 槽、hall 2 槽、tower 3 槽、keep 无槽。用两张表而不是一列 JSON：每个值都是普通列，行内规则（必填、引用、最小值）照常由 tag 声明、configdata 执行；tablegen 的 JSON 列不认 schema 包里的结构体类型。
- **跨行规则只声明一次**（`game/entities/building` 的 `CatalogOf`）：槽从 0 连续、名字不重复、`Min ≤ Default ≤ Max`、至多 8 槽、宽高不超过一块的边长 64、参数行引用的建筑存在。game 服务启动时对当前快照执行（不合规启动失败，在地形对账之前），每次热更新在 Validate 阶段对新快照执行（`building_config.go`）。热更新另外不许删已有配置 ID、不许改已有配置的宽高（`CheckReload`；改占地 = 新配置 ID），可以加配置、加槽、改范围。
- **参数规则**（放置与改参数同一条，`Spec.Normalize`）：个数不能多于槽数；没给的（从末尾起）取槽的缺省值；每个值在 `[Min, Max]` 里。存量建筑在配置加了槽、删了末尾的槽、改了范围之后按 `Spec.ParamsOf` 读：缺的取缺省值、多出的忽略、值原样（不替玩家改）。客户端按同一规则读。
- **业务读配置**：`building.SpecOf` 按当前请求钉住的快照（`configdata.ActiveSnapshot`，Nest 事务里是提交时钉住的那份）查，同一快照只合成一次。放置在事务前查一次、事务里按钉住的快照再查一次并以它为准。

### 27.3 同步：与 Player / Monster 同一条链路

> **变更（维护者 2026-11-10，[§27.10](#2710-订阅改为场景主视野维护者-2026-11-10)）**：下面“订阅”“对象上限（D15）”两条与“预算”里的视野数字是首版（建筑自己的 Interest 200m / 128、命名空间 `building` 单独计 128、引用表 292），已改为与 Monster 完全相同地进场景主视野；`TypeSchedules` 不再有建筑条目。其余各条（packer、登记 / 撤出的时机与修订号、启动登记、继承的契约、客户端、字节数）仍然成立，撤出改为与怪相同的 `HideSubject`（退役 subject）。

- **实体**：`Building` 加 `sync=true syncNamespace="building" subjectPacker=NewBuildingSyncPacker`；packer 与 Player / Monster 同形（一个缺省视图、全量与增量都是按字段的 `MarshalSync`）。生成的同步字段、`dao_sync.proto`、Go / C# Apply 都由正式生成器出。
- **Manager 与节奏**：场景同一个 entitysync Manager、同一条玩家连接与推送编号；`TypeSchedules` 写明建筑按变化发（on_change），与玩家、怪相同（地形块仍是 1 秒一批）。
- **订阅**：建筑自己的 `policy.Interest`（独立订阅来源，与地形块同一做法），按位置订阅：subject 是站着的建筑、位置是占地中心；观察者是玩家，视点就是玩家的地形视点（`terrainSync` 的 watch / moved / unwatch 同时报给它）。视野进入半径 200m、离开 220m、块 64m，`MaxVisible` 128。
  - 为什么不放进场景的主视野（Monster 的 `ShowSubject` 那个 Interest）：主视野的 `MaxVisible` 64 是“离玩家最近的 64 个玩家 / 怪”，建筑放进去会按距离挤掉玩家；D17 的超额降级收紧的也是整个主视野。自己的 Interest 有自己的半径与上限，超额只在建筑一类降级。这不是新的同步通道：机制（Interest → Manager → 帧 → 同一条 TCP 推送）与玩家、怪完全相同，只是订阅来源分开，与地形块一样。
- **对象上限（D15）**：`ManagerConfig.Namespaces` 加 `building`：`MaxObjects` 128，不设快照预算。会话引用表合起来 100 + 64 + 128 = 292（`sceneTerrainMaxObjects`），每帧对象数仍按 100 切，客户端帧上限不用改；显式设 C# `EntityViews` 容量的不得小于 292。
- **登记 / 撤出**：放置事务提交后（`nest.AfterCommit`，WAL 落盘之后）`Map.SetBuildingCommitted` 通知建筑同步：`Register(b.Sync())` + `Interest.Show`；拆除提交后 `Interest.Hide`（看着它的客户端下一帧收到 remove），subject 留在 Manager 里，同一锚点再放时是同一个实体，重新 Show 即可。改参数是普通的 DAO 写，subject 已登记，提交观察者按 on_change 发只带 `Params` 的增量。提交后回调可能乱序：通知带锚点所在块的 `LotRevision`（同一座建筑的每次放置 / 拆除都写这一块，随提交次序严格递增），只认更大的修订号。
- **启动**：启动对账（`ReconcileBuildings`）核对通过的建筑记在 `Map.ReconciledBuildings`，场景启动之后、玩家能进来之前登记（`startTerrainSync` 的 `loadStanding`）。
- **继承的契约**：持久化门槛（CommitLSN）、版本顺序与断档恢复、断线重连 / Hold-Ready 的新 epoch 全量，都与玩家、怪相同，没有建筑特有的分支。
- **客户端**：收到命名空间 `building` 的全量 / 增量，用生成的 `ApplySyncBuildingDao` 合并，按 `ConfigID` 查配置表得到预制体与占地宽高，按 `Params` 设参数，按 `GX` / `GY` / `Facing` / `Foundation` 摆放。C# 由 `Roost.Client.Terrain.Tests` 的建筑夹具验证（不做 Unity 表现）。
- **预算（每观察者）**：实测 hall 的全量是 132 字节的同步组件（subject 头约 79 字节 + 载荷 53 字节），改参数的增量 94 字节（载荷 15 字节）。视野满 128 座约 17KB，一次进入视野发完；按 10m/s 走过每 100m² 一座建筑的街区，每秒约 40 座进入视野，约 5.3KB/s；改参数受编辑配额（每次至少 20 点）限制。与 D15 的地形估算相加仍低于 50KB/s。
- **观察到的通用行为（不是建筑特有）**：建筑在放置事务提交时已记下脏位，登记后第一次进入视野先只拿快照、保留脏位（on_change 的合并窗口），于是它的下一次改动以一份全量（重同步）发出，之后才是增量。多发一次约 130 字节；玩家、怪在同样的时序下也一样。记在这里，未改 entitysync。**更正（2026-11-10）**：原因不是 `preserveDirty`（它只用于周期类型的即时快照），而是 on_change 冻结在“已有冻结 / 交付在途”时一律升成全量——用例里判重的第二次 `SetParams` 落在第一次改动尚未被 Flush 取走或尚在推送的窗口里；同一原因也让第二次改动偶发以全量发出。已在 entitysync 修复，两次改动都是增量，见 [证据](evidence/entitysync-resync-flake-20261110/README.md)。

### 27.4 块上的 `Lots`：服务端的串行化索引，不同步

- **分工**：客户端看到的建筑全部来自建筑实体（同步）+ 配置（占地宽高）；块上的 `TerrainChunkDao.Lots` / `LotRevision` 只持久、不同步，是放置 / 笔刷在块锁内检查重叠用的索引，进程内占位表（行走 / 寻路）也由它发布。
- **为什么这一处不能完全“统一”**：两座锚点不同的建筑是两个实体、两把锁，只靠建筑实体看不见彼此的并发放置。要在事务里看见所有可能重叠的建筑，就得锁住所有可能与新占地重叠的锚点——任何重叠建筑的锚点都在 `[GX−63, GX+W) × [GY−63, GY+H)` 里（宽高至多 64），一次放置要锁约 70 × 70 个建筑实体并先把它们建出来，不可行；改成进程内的索引则要么在事务外（提交与发布之间看不见彼此，§25.4 记过这段时间在真实 WAL 下是 fsync 量级），要么是事务里要回滚的进程内状态（违反 A1）。块锁正好是“两份重叠的占地必有公共块”的那把锁，笔刷本来就锁块，所以索引放在块上。
- **评估过的备选：按区域的锁实体**（例如每块一个“地块”实体，只放 `Lots`）。它把占位从地形数据里拆出来，但放置与每一次笔刷都要多锁一组实体（笔刷改高度要知道脚下有没有建筑），事务目标翻倍、数据多一个集合；得到的仍是同一个“按块的串行化索引”，只是换了实体。代价大于收益，不实施，保留为备选（27.9 ③）。
- 一致性：建筑实体与块上的记录总在同一笔 Nest 事务里写；启动对账核对两者（`Mismatched`），并核对配置（`Misconfigured`：配置 ID 没了，或按配置 + 朝向推出的占地与存下的不同；保留、不改、记 Warn）。

### 27.5 改参数（新增 `op` 3）

模型的修改参数放置后通常还会调整，所以加了改参数：`Map.SetParams` → handler `BuildingParams`（`rollback=undo durability=strict`，目标只有建筑实体与玩家的编辑账本——参数不改变占地，不锁块）。判重与笔刷 / 放置共用（玩家, 序号）与同一份账本（D18，重发答 `duplicate`）；只许属主（`terrain_denied`）；按建筑的配置补齐与校验（`building_params`）；锚点上没有建筑答 `building_missing`（同拆除，不建实体）；配额 `MinEditPoints`。新业务码：100066 `building_config`（配置 ID 未知）、100067 `building_params`。协议 `BuildingRequest` 的键 3 改名 `ConfigID`、加 `Params`（键 8）；答复加 `ConfigID`、`Params`。

### 27.6 分区场景（只写设计，本轮不接）

（更新 2026-11-10：P5 已实施单格内的建筑、Ghost 带的占位与跨格占地的拒绝，见 [§30.4](#304-分区场景的建筑)；建筑副本给别的 Base 上的客户端仍未做。）

分区场景仍拒绝放置（`building_disabled`，P5）。同步链路要接时：建筑归锚点所在格的进程写（与块相同），客户端连在 Base 进程上。照地形块副本（§21、`terrain_chunk.Replica`）的做法，格进程把站着的建筑推给 Base 与邻格：Base 上是只读副本（同一实体 ID、命名空间、编码，没有 DAO 与事务），登记到 Base 场景 Manager；它怎样进玩家的视野按 §27.10 的原则走玩家与怪在分区场景里的那一套（格的 witness 与 Base 上的镜像），不单设建筑 Interest，具体接法属 P5（首版这里写的是“放进同一种建筑 Interest，视点来自格的 witness”，已作废）；邻格 Ghost 带里只要占位（`Lots` 的副本）给行走与跨格移动判定。跨格占地（一座建筑的块分属两格）需要两格的块锁在同一笔事务里，是跨进程事务（Remote）或“占地不跨格”的规则二选一，属 P5 决策。

### 27.7 验证

| 用例 | 内容 |
| --- | --- |
| `building_sync_test.go` `TestBuildingsReachAnotherClientOverRealTCP` | 生产接线（Nest 提交观察者、on_change）：另一个玩家的真实 TCP 客户端看到 hall 出现（全量，参数补齐为 `[3 0]`、朝向 1、锚点、地基）、视野外的 tower 不发；非属主改参数 `terrain_denied`、重复序号 `duplicate`；改参数到达，第二次改动是只带 `Params` 的增量；走远时 hall 消失、tower 出现，走回来 hall 以全量回来（带改过的参数）；拆除收到 remove；同一锚点再放 hut 又出现；`SessionsLost` 0；记录字节数 |
| `TestBuildingSyncAfterARestart` | 放置三座（其一必须整平）、改参数、拆一座，停机落“库”后重启：对账 `Standing 2`，登记 2 座，新登录的客户端收到的正是库里的两座 |
| `TestBuildingSyncTakesTheLatestRevision` | 迟到的“拆了”不撤出站着的建筑，迟到的“放了”不放回拆了的建筑 |
| `TestBuildingParamsChangeOncePerSeqByTheOwner` | 非属主、个数多于槽、超上限、低于下限、多于 8 个、空锚点、非锚点节点都零写入；放置 / 笔刷用过的序号答 `duplicate`；没给的槽取缺省；不改占地与地形；拆了之后不能改 |
| `TestBuildingConfigRulesAndReloads` | 工程配置合规；槽有空位、槽重复、缺省超上限、下限超上限、名字重复、宽度 65 都让启动失败；热更新改 hut 宽度、删 tower、槽有空位都被拒、旧快照留着；加配置、加槽、改范围生效，存量参数按新配置读 |
| `TestBuildingReconcileReportsAConfigDrift` | 重启时配置文件删了 tower、改了 hut 宽度：对账 `Misconfigured 2`，建筑保留、不改、仍登记同步 |
| `terrain_building_test.go` 原有用例 | 改为配置 ID；`TestBuildingPlacementChecks` 加未知配置、配置 0、参数个数 / 越界（三种）、必须整平却没整平 |
| `TestBuildingRestartRealMongo` | 加改参数，重启后对账通过的建筑带着改过的参数；门变量 `ROOST_TERRAIN_IT_MONGO_URI`，本轮**没有运行** |
| C# `Roost.Client.Terrain.Tests` | 建筑夹具（`Fixtures/building-sync.json`，`regen.sh` 从 `BuildingDao` 定义用正式生成器生成）：放置全量、只带参数的增量、换成无槽配置、拆除，生成的 C# Apply 与 Go 全量逐字节相等、摆放字段一致 |
| 变异 | 13 项里 12 项红（不登记、不按修订号、启动不登记、不查属主、不查范围、不查个数、热更新不查、不查槽连续、对账不查配置、拆除不撤视野、改参数不记账本、视野不跟视点、不查必须整平），C# 去掉 Params 合并变红；“改参数不 `PublishSyncDirty`”不变红（生产接线由提交观察者标脏，见证据） |

修前红：在改动前的代码（2709660f 生成的工程）上，“另一个客户端应收到 `building` 命名空间的对象”超时失败（证据原文）。

### 27.8 未覆盖与限制

- 真实 Mongo 用例没有运行；没有真实进程演练（整进程的 `installBuildingConfig` / `loadStanding` 接线只有编译与单元覆盖）；没有 Unity 表现。
- 第一次改动以全量发出（27.3 末条）是 entitysync 的通用行为，未改。（更正 2026-11-10：原因是 on_change 冻结的“忙时升全量”，已修复，见 27.3 末条的更正。）
- ~~建筑视野的半径与上限（200m / 128）是估算值，没有按真实玩法测。~~ 已随 §27.10 删除；现在是场景主视野的半径与上限，demo 尺度的影响见 §27.10。
- 配置热更新收窄了参数范围时，存量值不改（可能在新范围外），客户端按存量值显示。
- 建筑实体是 World 档、常驻；若以后允许卸载，要像玩家一样接 `ConfigureUnloadResync`。

### 27.9 实施方先定的取舍（维护者 2026-11-10 决定见表后）

| 项 | 现状 | 选项（推荐标★） |
| --- | --- | --- |
| ① 建筑的订阅来源 | 自己的 Interest（200m / 128），视点 = 地形视点 | ★ 自己的 Interest（不挤掉玩家，超额只在建筑一类降级）；或放进场景主视野（与怪的 `ShowSubject` 完全相同，但与玩家、怪共享“最近 64 个”与超额降级） |
| ② 参数个数规则 | 不多于槽数，没给的从末尾取缺省值（放置与改参数同一条） | ★ 现在的规则（客户端可以只发前几个）；或必须恰好等于槽数（改参数时更不容易误重置后面的槽） |
| ③ `Lots` 的位置 | 块上，服务端内部索引，不同步 | ★ 块上（块锁就是串行化点，笔刷本来锁块）；或按区域的锁实体（拆开数据，但放置与笔刷都多锁一组实体）；或进程内索引（有并发重叠窗口，否决） |
| ④ 热更新的配置规则 | 不许删配置 ID、不许改宽高；可加配置、加槽、改范围 | ★ 现在的规则；或更严（不许删槽、不许收窄范围，存量值永远合规）；或更松（允许改宽高，靠重启对账报 `Misconfigured`） |
| ⑤ 配置漂移的存量建筑 | 保留、不改、计 `Misconfigured`、仍同步 | ★ 保留（不动玩家资产，运维处理）；或启动时自动按新配置改写（改占地要重写块上的记录、可能与邻居重叠） |
| ⑥ 建筑视野半径 / 上限 | 200m / 128 | ★ 先用（与地形视野一致，预算内）；正式值随玩法定 |
| ⑦ 改参数的配额 | 每次 `MinEditPoints`（20 点） | ★ 与拆除相同；或单独的限频 |

**维护者 2026-11-10 决定**：① 建筑的订阅跟随玩家与怪（进场景主视野，与 Monster 的 `ShowSubject` 相同，不单设 Interest / 命名空间），实施中；其余各项按推荐。**原则**：可编辑地形里只有 TerrainChunk（地形块实体）是特殊的，其他实体（Building、TerrainEditor 等）都按普通实体处理——持久、同步、订阅走与 Player / Monster 相同的正式链路，不为它们另造通道；块上的 `Lots` 只作服务端内部串行化索引，不改变这一原则。

### 27.10 订阅改为场景主视野（维护者 2026-11-10）

维护者 2026-11-10：“建筑订阅跟随玩家和monster,后续只有terrainentity特使，别的entity都是普通的entity”。本节实施 §27.9 ① 的决定，并按“可编辑地形里只有 TerrainChunk 是特殊实体”的原则审视其他实体。证据、命令、修前红原文与变异见 [evidence/building-main-interest-20261110](evidence/building-main-interest-20261110/README.md)。

**接法：与 Monster 完全相同**

| 项 | 首版（§27.3） | 现在 |
| --- | --- | --- |
| 订阅来源 | 建筑自己的 `policy.Interest`（进入 200m / 离开 220m、块 64m、`MaxVisible` 128） | 场景主视野那个 Interest（与玩家、怪同一个：进入 1.2m / 离开 1.5m / 客户端最大 2.7m、`MaxVisible` 64、超额降级 D17） |
| 进 / 出 | `Manager.Register` + 自己的 `Interest.Show` / `Hide`（拆除不退役） | `Scene.ShowSubject`（登记 + 主视野 Show，位置是占地中心）/ `Scene.HideSubject`（主视野 Hide + 退役 subject），就是 spawner 给怪用的两个入口 |
| 视点 | 玩家的地形视点（`terrainSync` 的 watch / moved / unwatch 同时报给建筑） | 玩家自己的主视野（跟着玩家实体走），没有建筑专用的视点 |
| `ManagerConfig.Namespaces`（D15） | `terrain` 64 + `building` 128 | 只有 `terrain` 64；建筑与玩家、怪、场景同在 `Limits.MaxObjects`（100）那一类 |
| 会话引用表上限 | 100 + 64 + 128 = 292 | 100 + 64 = 164（`sceneTerrainMaxObjects`，与 §27 之前相同） |
| `TypeSchedules` | 地形块 periodic 1s + 建筑 on_change（显式条目） | 只有地形块（`sceneTypeSchedules`）；建筑与玩家、怪一样没有条目，按 Manager 的模式发（生产 on_change，结果与首版相同） |
| 命名空间 `building` | 客户端分流键 + Manager 的单独一类 | 只是客户端分流键（与 `monster` 相同），生成的同步字段、packer、C# Apply 不变 |

删除的建筑专用逻辑：`building_sync.go` 里的 Interest、视野常量（`buildingViewRadius` / `buildingViewHysteresis` / `buildingMaxVisible`）、`watch` / `move` / `unwatch` / `close`；`terrainSyncConfig.buildings` / `buildingMaxVisible` 与 `terrainSync.buildings`（地形视点不再转报）；`terrainNamespaceLimits` 里的 `building` 一类；`TypeSchedules` 的建筑条目。`building_sync.go` 只剩建筑与怪不同的那一点——**什么时候** Show / Hide：怪由 spawner 在一个 goroutine 上建与杀，建筑由放置 / 拆除的 Nest 事务决定，所以仍是提交后通知（`nest.AfterCommit` → `Map.SetBuildingCommitted`）、按锚点块 `LotRevision` 丢迟到通知、启动时 `loadStanding`；接线改为 `Scene.attachBuildings`（NewScene 在接建筑的普通场景调用）。

两处随之改动（都不是建筑专用的分支）：

- **`Scene.ShowSubject` 改用 `Manager.RegisterAfterRetirement`**（与 `Join` 相同）。拆除现在经 `HideSubject` 退役 subject；同一锚点再放是同一个实体 ID，若拆除欠订阅者的 remove 还没发出去，普通 `Register` 答 `ErrSubjectRetiring`，建筑就再也不显示（变异 M1 实测）。排在退役之后登记则先 remove 后 create；主视野的订阅在退役期间被拒、按 Interest 的重试表下一次结算再说。怪每次都是新 ID，对怪这就是 `Register`。
- **修订号判定与 Show / Hide 在同一把锁里做**。首版在锁外调用 Show / Hide：两个提交后回调并发时，修订号较小的那个可能在判定通过之后、生效之前被较大的那个超过，倒序生效。现在持锁调用（两者只取 Manager / Interest 的短锁、不等待）。这是结构性守卫：没有构造出并发倒序的红用例（变异 M9“放回锁外”不变红），保证只到“同一座建筑的通知按修订号次序生效”，不覆盖进程内别的地方对同一 subject 的调用。

**其他实体的审视**

| 实体 / 数据 | 结论 |
| --- | --- |
| TerrainChunk（与 `terrain_chunk.Replica`） | 唯一的特殊实体，不变：自己的 Interest（200m / 64）、`Namespaces` 的 `terrain` 一类（64、每会话每 Flush 4 个全量）、`TypeSchedules` 1 秒一批、分区场景的 Ghost 副本与只读副本 |
| Building | 本节：普通实体 |
| TerrainEditor（玩家编辑账本） | 本来就是普通实体：PlayerScoped、DataEngine 持久、经生成的 lifecycle `GetOrCreate`，与块 / 建筑在同一笔 Nest 事务里锁；不同步（`sync` 未开，客户端从答复的 `LastSeq` 知道进度），没有任何订阅或专用通道，无需改 |
| 块上的 `Lots` / `LotRevision` | 不是实体，是服务端内部的串行化索引（§27.4）：放置 / 笔刷在块锁内查重叠，只持久、不同步；进程内占位表（行走 / 寻路）由它发布。客户端看到的建筑全部来自 Building 实体 + 配置，`Lots` 不构成第二条建筑链路 |
| `Map.SetBuildingCommitted` 通知、`LotRevision` 判定、`loadStanding` | 进出场景的时机（相当于 spawner 的“建好了 / 杀掉了”），不是订阅或同步通道；建筑经事务产生，所以通知在提交之后、乱序要靠修订号 |
| Scene（时间域）、SceneActor、Monster、Player | 不属于可编辑地形；本轮不动 |

**主视野对建筑的影响（测量，demo 尺度）**

主视野是 §23 留下的 demo 尺度（世界单位厘米：进入 1.2m、离开 1.5m、客户端可要到进入 2.7m / 离开 3m；最近 64 个；会话 100）。建筑按**占地中心**进出。`building_sync_test.go` 的测量：

| 情形 | 结果 |
| --- | --- |
| 站在建筑旁边紧挨着的空节点上（离占地中心 (短边 + 1) / 2 米：hut 2.5m、tower 2m、hall 3.5m、keep 6.5m） | 缺省视野四种都看不见；客户端最大视野只看得见 hut 与 tower，hall 与 keep 看不见；keep 要站到中心 1.2m 以内（占地里面）才看得见（`TestBuildingsAtTheDemoMainViewScale`） |
| 主视野里最多几座建筑 | demo 最小的建筑是 3 × 3：两座占地中心至少差 3m（切比雪夫距离），缺省视野（直径 2.4m）至多 1 座，最大视野（直径 5.4m）至多 4 座 |
| 建筑挤占玩家 / 怪的名额 | 会：四座 tower（2.12m）+ 60 只怪（2.16～2.32m）占满 64 个，2.35m 的另一个玩家被挤出主视野（`TestBuildingsTakeMainViewPlacesLikeAnyoneElse`）。demo 尺度下建筑至多占 4 个名额，挤占只在视野本来就满时发生 |
| 空出名额之后 | AOI 的 `MaxVisible` 是近似语义（`policy.AOIConfig`：被忽略的目标在它下一次移动时重新参选）：拆掉一座 tower，被挤出的玩家要自己动一下才进来；反过来，先被 64 只更近的怪占满、后放的 4 座 tower 被忽略，走掉 10 只怪、看的人站着不动时仍是 0 座，动 1cm 后 4 座都到。**建筑从不移动**，所以只有看的人移动才会让它重新参选 |
| 会话超额（D17） | 主视野不设上限、97 只怪 + 4 座 tower + 自身 = 102 > 100：不断开，收敛到 100（2 座 tower 暂缓，`OverLimitDeferred` 2、`Evicted` 0）；拆掉留着的两座，暂缓的两座随即进来，不需要谁移动（`TestManyBuildingsDegradeWithTheCrowd`） |
| 字节数 | 不变：hall 全量 132 字节（载荷 53）、改参数增量 94 字节（载荷 15） |

结论：**demo 尺度明显不适合建筑**——主视野是按 0.6m/s 的人设计的 1～3m，而建筑边长 3～12m，按占地中心进视野时人站在建筑旁边看不见它（keep 从外面永远看不见）。反过来，若把主视野放大到建筑尺度，按 §27.3 的估算密度（每 100m² 一座）半径约 45m 内就有 64 座建筑，“最近 64 个”会被建筑占满、把更远的玩家 / 怪挤掉，会话也更容易超过 100。本轮**不改主视野参数**，列为待确认：

| 待确认 | 选项（推荐标★） | 代价 |
| --- | --- | --- |
| A 开地形场景的主视野尺度 | ★ 按游戏尺度调大（例如进入 30m / 离开 33m / 客户端最大 60m，`MaxVisible` 64 与块边长随之重定），玩家、怪、建筑一起用；或保持 1.2m（建筑在 demo 里只作接线演示，基本看不见） | 玩家 / 怪也在 30m 外可见，候选与下行变多；要重跑 §23 的 AOI 基准、D17 与场景同步用例，demo 的移动速度 / 巡逻范围可能也要跟着放大 |
| B 主视野名额是否按类型分 | ★ 先不分（A 定下之后按实测再看）；或按类型给名额 / 优先级（例如建筑至多 N 个、玩家优先） | 分类型要给 `policy.AOI` 的 `MaxVisible` 加按命名空间或 Profile 优先级的取舍——框架改动，“建筑与怪完全相同”就多了一条按类型的规则 |
| C 建筑按占地中心还是占地范围进视野 | ★ 先按中心（与怪一样是一个点）；或 AOI 支持目标带半径（离占地最近点进视野） | 带半径是框架改动（AOI 索引按扩展块登记，`MaxObservedBlocks` 已有），大建筑才需要 |
| D 静止目标被上限忽略后的重新参选 | ★ 接受近似（玩家通常在动；会话级超额降级 D17 的放回不受影响）；或在 AOI 里有空位时重评被忽略的候选 | 重评是 `policy.AOI` 的框架改动，要测批量重评成本（`BenchmarkAOIBatchRecompute`） |

**维护者 2026-11-10 决定**：A 按推荐（开地形的场景按游戏尺度放大主视野，实施中），B、C、D 按推荐；`TestBuildingsReachAnotherClientOverRealTCP` 的偶发（第二次改参数以全量发出）要查，实施中。

**A 的实施结果（[§28](#28-开地形场景的主视野尺度2026-11-10)）**：开地形的普通场景主视野改为进入 30m / 离开 33m / 客户端 15～60m，AOI 块边长随之 1m → 32m（1m 块在新视野下超过观察者块预算 1024，构造被拒）；“最近 64 个”与会话上限不变，不开地形的场景与分区场景保持上面的 demo 尺度。上表第一行在新尺度下：站在任一建筑旁边都看得见它（keep 从外面看）。挤占的实测：缺省视野下镇子密度（建筑每 100m² 一座）基本不挤（每人漏 0.004 个）；客户端开到 60m 时最近的 64 个约在 33m 以内、其中约 41 座是建筑，33m 外的玩家 / 怪被挤掉（§28.5）。B、C、D 未改，D 的“静止目标靠观察者移动重新参选”在新尺度下由 `TestBuildingsTakeMainViewPlacesLikeAnyoneElse` 照旧覆盖。

**验证**（生成的 game-demo，`building_sync_test.go`）

| 用例 | 内容 |
| --- | --- |
| `TestBuildingsReachAnotherClientOverRealTCP`（改） | 另一个玩家的真实 TCP 客户端站在 hall 上：出现（全量）、改参数（增量）、**走出主视野 3m 外消失**（首版的 200m 建筑视野下不会消失）、走到 tower 上看到 tower、走回来全量回来、拆除 remove、同一锚点再放、拆了马上再放 |
| `TestABuildingPlacedAgainBeforeItsRemoveWentOut`（新） | 复制循环不跑：拆除欠着 remove 时同一锚点再放，客户端先 remove 后收到新 tower 的全量 |
| `TestBuildingsHaveNoReplicationOfTheirOwn`（新） | `Namespaces` 只有 `terrain`、引用表 164、`TypeSchedules` 只有地形块、Manager on_change |
| `TestBuildingsTakeMainViewPlacesLikeAnyoneElse`（新） | 4 座 tower + 60 只怪挤掉 2.35m 的玩家；拆一座、玩家动一下进来；被忽略的 tower 在看的人动 1cm 后进来（站着不动的结果只记日志） |
| `TestManyBuildingsDegradeWithTheCrowd`（新） | 超额降级：102 个收敛到 100、不断开、建筑与怪同一类；拆掉留着的两座，暂缓的两座进来 |
| `TestBuildingsAtTheDemoMainViewScale`（新） | 上表第一行的测量 |
| `TestBuildingSyncAfterARestart`、`TestBuildingSyncTakesTheLatestRevision`（改） | 看的人站到每座建筑上核对；拆掉的那座在原位不出现（参照怪到了、建筑没到） |

修前红：新用例在改动前的代码（`21f631e1` 生成的工程，测试只加了编译垫片）上 6 个红（主视野走出 3m 不消失、`Namespaces` 有 `building`、主视野挤不掉邻居、超额等不到、hut 在缺省视野里看得见、重启后在一座建筑上同时持有两座）。变异 9 项里 8 项红；M9（Show / Hide 放回锁外）不变红，见上。C# 客户端、DAO、协议、存储都不变。

**未覆盖**：TCP 用例在重负载下偶发“第二次改参数以全量发出”（新代码 3 次，改动前的代码同样负载 0/400；内容正确，只是编码），尚未确认，见 KNOWN-LIMITS 与证据 §3。真实 Mongo、真实进程演练、Unity 表现仍未跑（同 §27.8）；分区场景仍不接建筑（§27.6 的设计已改为按本节原则走玩家与怪在分区场景里的可见性，不单设建筑 Interest，接法属 P5）。

## 28. 开地形场景的主视野尺度（2026-11-10）

维护者 2026-11-10 对 §27.10 的决定：A 按推荐——开了地形的场景把场景主视野按游戏尺度放大（进入 30m、离开 33m、客户端最大 60m）；B（名额不按类型分）、C（按占地中心进视野）、D（静止目标被挤后靠观察者移动重新参选）按推荐，不改框架的 `policy.AOI`。分支 `feat/terrain-main-view-scale`（基于 `7da1f62e`）。证据、命令、修前红与变异原文见 [evidence/terrain-main-view-scale-20261110](evidence/terrain-main-view-scale-20261110/README.md)。

### 28.1 参数

> **变更见 [§29](#29-游戏尺度参数视野-32m单进程-2048m同步-128-个2026-11-10)**：维护者 2026-11-10 给出游戏尺度的口径后，开地形的普通场景改为进入 32m / 离开 35.2m / 客户端 16～64m、“最近 128 个”、会话上限 164，普通场景的地图上限改为 2048m 见方。本节保留 §28 实施时的取值与理由。

主视野的尺度成了场景配置的一部分：`gamescene.Config.View`（`EnterRadius` / `LeaveRadius` / `MinRadius` / `MaxLeaveRadius` / `RadiusStep`），与块边长 `BlockSize` 一起由 `DefaultConfig` / `TerrainConfig` 给出；场景同步的主视野 Interest 直接读它（`scene.go` 原来的五个视野常量删除）。世界单位厘米：

| 项 | 不开地形的场景（demo 原尺度，逐字段不变） | 开地形的普通场景（`TerrainConfig`） |
| --- | --- | --- |
| 进入 / 离开半径 | 1.2m / 1.5m（`DefaultView`） | **30m / 33m**（`TerrainView`） |
| 客户端可要的视野（`scene_view`，进入半径） | 0.6m～2.7m（离开上限 3m），步长 0.3m | **15m～60m（离开上限 63m）**，步长 3m |
| AOI 块边长 | 1m | **32m**（`TerrainBlockSize` 3200，半个地形块） |
| 每个观察者最坏订阅块数（缺省视野 / 最大视野） | 25 / 64 | 16 / 25（预算 1024） |
| 主视野“最近 N 个”（`sceneMaxVisible`） | 64 | 64（不变） |
| 会话对象上限（D15） | 100 | 100 + 地形 64 = 164（不变） |

- “客户端最大 60m”按 §27.10 表里“客户端最大 2.7m”的同一口径读，即客户端能要到的**进入**半径 60m，离开上限 = 60m + 滞回 3m = 63m（见 28.8 ①）。最小 15m 与步长 3m 按原尺度的比例取（最小 = 缺省的一半，步长 = 滞回量），30m 与 60m 都在量化网格上。
- 客户端没有视野常量：`SceneViewResponse` 答服务端生效的视野，C#、协议都不用改。
- `Config.Normalize` 把零值的 `View` 补成 `DefaultView`（手写的 `Config` 与分区场景的 Base 世界）。分区场景的 Base 世界显式取 `DefaultView`：分区场景**不放大**（见 28.6）。
- 地图上限不变（**§29 起改为 2048m 见方**）：普通场景能放的地形图仍是 1024m 见方的面积（16 × 16 块，`TerrainConfig` 按面积拒绝，错误仍指向分区场景）。原来这个上限来自 1m 块的块表（2^20 块、56MiB）；块边长 32m 后同一张图只要 32 × 32 = 1024 块，块表不再是限制，上限保留为普通场景的地图预算（整张地形装在一个进程里），是否放宽见 28.8 ③。

### 28.2 块边长与预算

- **块边长 100 不能用**：缺省视野每个观察者 (⌈6601 / 100⌉ + 1)² = 68² = 4624 块，最大视野 128² = 16384 块，都超过 `policy.DefaultMaxObserverBlocks` 1024，场景构造被拒（`ErrInterestBudget`，`TestTerrainMainViewFitsTheBlockBudget`）。
- **调上限的做法**（块边长不变、把预算调到 16384、`MaxPlayerBlocks` 调到 9 × 16384 = 147456）：每次观察者移动要改两份 16384 项的块集合（自己的与反查表），块表仍是 1m 块的 56MiB；没有采用，也没有测（要改 demo 的预算才构造得出来）。
- **放大块边长**：10m、16m、32m、64m 四档同机测（28.3）。32m 与 16m 的每批耗时在噪声内相同、都比 10m 快，32m 每个观察者订阅的块最少（缺省视野平均 9.1 块、最大视野 23.6 块；16m 是 25.6 / 76.5）、常驻内存与每批分配最少，所以取 **32m**。64m 订阅更少（4.0 / 8.5 块）但同一轮里没有更快、候选更多，只在连续跑的参考数据里测。
- **预算核对**：观察者块预算按最大视野核 25 ≤ 1024；`MaxPlayerBlocks` 缺省 65536，ApplySync 至多 9 个视野，每个视野 7281 ≥ 25；块表 1024m 地图 1024 块（约 56KiB）。
- **“最近 64 个”与会话上限**：不调。64 + 3（自身、场景、队友）≤ 会话的 100（`newSceneOn` 启动时核对），地形另算 64（D15）；超额降级（D17）不变。28.5 是 64 在新尺度下的实测。

### 28.3 测量

`scene_view_scale_test.go` 的 `BenchmarkSceneMainViewScale`：生产的场景构造（`newSceneWith`：场景 Manager、主视野 Interest、1000 个会话），推送换成只计字节的替身。地图 1024m 见方（16 × 16 块）；中央 320m 见方的镇子每 10m 一座建筑（1024 座 = §27.3 的“每 100m² 一座”）和 500 个玩家，另 500 个玩家与 2000 只怪散在全图；一半玩家（6m/s）、四分之一的怪（3m/s）在走，移动心跳 200ms 一步，所以每次复制（50ms）约 250 个移动。一次 op = 一次复制（政策阶段的批量重评 + 帧）。建筑用静止的目标代替（AOI 不分类型）。

机器：Apple M5，负载很重（loadavg 10～18，交换占用约 20GB，同时在跑别的 agent 的测试与 Unity 构建）；时间只作同一轮里的对照。耗时取 6 轮交替（每轮六个变体各跑一次、顺序轮换，每次 300 op）的中位数：

| 视野 / 块边长 | 每次复制 | 每次分配 | 平均订阅块 / 观察者 | 场景常驻内存 |
| --- | ---: | ---: | ---: | ---: |
| 30m / 10m | 3.51ms ±72% | 1.31MiB | 56.8 | 72.7MiB |
| 30m / 16m | 3.25ms ±24% | 1.10MiB | 25.6 | 69.2MiB |
| **30m / 32m（选定）** | **3.20ms ±24%** | **1.05MiB** | **9.1** | **67.4MiB** |
| 60m（客户端都要最大）/ 10m | 13.7ms ±40% | 2.75MiB | 179.4 | 100.8MiB |
| 60m / 16m | 11.7ms ±54% | 2.16MiB | 76.5 | 94.2MiB |
| **60m / 32m（选定）** | **12.1ms ±33%** | **1.87MiB** | **23.6** | **90.3MiB** |

连续跑的参考（`bench-consecutive.txt`，每档 6 次连在一起，波动更大）：demo 原尺度（1.2m、1m 块）同一张图 1.17ms、91.5MiB（其中 1m 块的块表约 56MiB）；30m / 64m 6.06ms、66.7MiB；60m / 64m 13.0ms、89.3MiB。

每个玩家的主视野（全部 1000 人平均，与块边长无关）：缺省视野可见 30.3 个，其中建筑 15.4 座；最大视野可见 48.6 个，其中建筑 20.5 座。进出视野的下行（全量与 remove，不含位置增量）平均 389 B/s（30m）、552 B/s（60m），demo 原尺度 20 B/s。

同步字节（`TestMainViewMeasureSyncBytes`，帧 payload，含帧头）：玩家进入视野 205 字节，走一步单独一帧 140 字节、四人同一拍一帧 464 字节（边际约 108 字节 / 人）；怪三只一帧全量 443 字节，巡逻一步单独一帧 146 字节；建筑全量 132、改参数 94（§27.3）。

### 28.4 每观察者下行估算（对照 50KB/s）

移动的对象每 200ms 一步、一步一个增量，静止的建筑进视野后只在改参数时发。

| 情形 | 推算 | 下行 |
| --- | --- | ---: |
| 镇子里（缺省视野） | 可见约 52 个（全体平均 30.3，散在野外的人约 8 个，镇子里的人是两倍减去野外）：建筑约 31 座静止，移动的约 10 个（玩家 8、怪 1～2、野外的 1）× 5 步 / 秒 × 108～140 字节；自己的位置 5 × 140；进出视野约 0.8KB/s；6m/s 跑过镇子新进视野的建筑约 0.01 × 60m × 6m/s ≈ 3.6 座 / 秒 × 132 | 约 7～9KB/s |
| 上限（任何视野） | 主视野至多 64 个、全部在走：64 × 5 × 108～140 字节，加自己 0.7KB/s 与进出视野 | 约 35～46KB/s |

结论：常见情形远低于 50KB/s；上限由“最近 64 个”与移动心跳 200ms 决定、与视野半径无关，约 35～46KB/s，接近预算（移动心跳更快或 64 调大就会超）。**§29 把名额调到 128 后实测 128 个同时走约 73.5KB/s，超过预算；增量字节压缩后同一用例约 18.6KB/s，见 [§29.6](#296-每观察者下行增量字节压缩后-128-个同时动约-186kbs在-50kbs-以内)。**

### 28.5 §27.10 的挤占风险：实测结论

§27.10 的推算是“每 100m² 一座时，半径约 45m 内就有 64 座建筑”。按这里的密度（建筑每 100m² 一座 + 镇子玩家每 205m² 一人 + 怪）实测：

- **缺省视野（30m / 33m）基本不挤**：镇子里每个人 30m 内漏掉的对象平均 0.004 个（500 人里 2 个，是局部人多时 33m 内超过 64 个），建筑约 31 座、占不满 64。
- **客户端要到最大视野（60m）就挤**：镇子里每个人 60m 内平均有 108 个对象没进视野——64 个名额被最近的对象占满，在这个密度下最近的 64 个大约都在 33m 以内（64 / (0.0184 个 / m² × π) ≈ (33m)²），其中约 41 座是建筑；33m 外的玩家 / 怪都看不见。也就是说，在密集的镇子里把视野开到 33m 以上不增加可见的东西，只多订阅块与重评成本；在空旷的野外最大视野照常有用。
- 维护者按推荐不按类型分名额（B）；这个结论记入 28.8 ②。

### 28.6 分区场景（同理说明，本轮不改）

分区场景的主视野在 Base 进程上，客户端看到的只来自 Cell 的 witness：`settings.PartitionMaxBaseLeaveRadius`（= `gamescene.DefaultMaxLeaveRadius`，编译期断言）要求 `view_radius` ≥ Base 的最大离开半径，`view_radius + hysteresis` ≤ `ghost_band`。按同一尺度放大要一起改：Base 主视野取 `TerrainView`（离开上限 63m）、`view_radius` ≥ 6300、`ghost_band` ≥ 6300 + 滞回、`wake_radius` 随之放大，格边长与 Ghost 带的成本（每格镜像的对象数、witness 的大小）要重新测，`partitionBlockSize`（2m）也要随视野放大。属 P5 与分区场景的建筑接线一起做，本轮分区场景保持 demo 原尺度（`TestTerrainSceneWorldAndItsLimit` 断言）。

### 28.7 用例与变异

| 用例 | 内容 |
| --- | --- |
| `TestBuildingsAtTheTerrainMainViewScale`（新，替换 `TestBuildingsAtTheDemoMainViewScale`） | 站在每座建筑旁边紧挨着的节点上缺省视野看得见四种（hut 2.5m、tower 2m、hall 3.5m、keep 6.5m，keep 从外面看）；29m 的玩家 / 怪 / 建筑在视野里、31m 的怪不进来；退开 2m（31～32m）都留着，退到 34m 外都离开，走回来都回来；最大视野是 60m，看得见 59m 的怪。数字写成字面量 |
| `TestTerrainSceneWorldAndItsLimit`（改） | 开地形的普通场景是地形图 + `TerrainView` + 32m 块；不开地形的场景逐字段等于原来（含 1.2 / 1.5 / 0.6 / 3 / 0.3m 的视野）；分区场景的 Base 世界、手写 `Config` 都是原尺度；地图上限的接受 / 拒绝不变 |
| `TestTerrainMainViewFitsTheBlockBudget`（新） | 32m 块构造得出来（16 / 25 块）；30m 视野配 1m 块被拒（`ErrInterestBudget`，4624 / 16384 块）；不开地形的原尺度构造得出来 |
| `TestSceneViewRequestWidensClampsAndIsRateLimited`（改） | 不开地形的场景最大视野仍是 2.7m（字面量） |
| `TestBuildingsReachAnotherClientOverRealTCP`、`TestBuildingsTakeMainViewPlacesLikeAnyoneElse`、`TestManyBuildingsDegradeWithTheCrowd`、`TestBuildingSyncAfterARestart`（改几何） | 承诺不变，几何按新尺度放大：走出主视野 3m → 40m；四座 tower 离看的人 2.12m → 12.0m、怪 2.16～2.32m → 12.5～13.3m、被挤掉的玩家 2.35m → 14m，不再要 2.4m 的客户端视野；超额降级的怪 1.2～2m → 3～10m；重启用例三座建筑彼此相距 70m 以上（原来 hall 与 keep 约 33m，站在一座上会同时持有两座） |
| `TestMainViewMeasureSyncBytes`、`BenchmarkSceneMainViewScale`（新） | 28.3 的测量 |

修前红：新的 `TestBuildingsAtTheTerrainMainViewScale` 在改动前的代码（`7da1f62e` 生成的工程，只换了测试文件，不需要编译垫片）上失败：`timed out waiting for hut 4×4 seen from the node next to it`。变异 9 项全部变红（地形场景不放大视野、仍用 1m 块、Interest 不读场景的视野、去掉滞回、离开上限 60m、不开地形的也放大、`Normalize` 不补视野、去掉地图上限、进入半径 33m），原文见证据。

生成的 game-demo 全量 `go build` / `go vet` / `go test ./...` 通过；`internal/service/game`、`game/scene/runtime`、`game/settings` `-race` 通过，建筑与场景相关用例 `-race -count=5` 通过。没有真实依赖、真实进程演练与 Unity。

### 28.8 待维护者确认（维护者 2026-11-10 决定见本节末）

实施方按推荐先定、已实施的取舍：

| 项 | 现状（推荐标★） | 选项与代价 |
| --- | --- | --- |
| ① “客户端最大 60m”的口径 | ★ 客户端能要的进入半径 60m（离开 63m），与 §27.10 表里“客户端最大 2.7m”同一口径 | 或离开上限 60m（客户端最大进入 57m）：改 `TerrainView.MaxLeaveRadius` 一个数 |
| ② 密集处的最大视野 | ★ 不按类型分名额（B 已定），接受“镇子里 33m 外的玩家 / 怪被建筑挤掉、视野开大不多看见” | 或把客户端最大视野降到约 45m（订阅与重评成本少一半，镇子里看见的不变，野外看得近一些）；或按类型给名额（框架改动，B 的另一选项） |
| ③ 普通场景的地图上限 | ★ 保持 1024m 见方的面积（同一批地图接受 / 拒绝） | 或随块边长放宽到块表上限（32m 块时约 32km 见方，1w 方图可以进普通场景，但整张地形装在一个进程里，内存与启动装入没有测） |
| ④ 分区场景 | ★ 本轮保持原尺度，随 P5 一起放大（28.6） | 或现在就放大：witness / Ghost 带 / 格边长一起重定并重测 |
| ⑤ demo 的移动速度与巡逻范围 | ★ 不改（玩家 0.6m/s、怪 0.2m/s、巡逻 12cm 一角，是 demo 的游戏数字） | 或按 30m 视野放大到游戏尺度（例如 6m/s / 3m/s）：移动用例的期望要跟着改 |
| ⑥ 每次复制的成本 | ★ 接受：1000 人时缺省视野约 3.2ms / 50ms，人人开最大视野约 12ms | 或限制最大视野（同 ②） |
| ⑦ 下行上限 | ★ 接受：上限约 35～46KB/s，由 64 个名额与 200ms 一步决定 | 或给远处的对象降频（LOD 分档 / 远处 periodic），demo 的 packer 现在只有一档 |

**维护者 2026-11-10 决定**：七项按推荐；另给出游戏尺度的口径（**已实施，见 [§29](#29-游戏尺度参数视野-32m单进程-2048m同步-128-个2026-11-10)**；①～⑦ 里的 30m / 60m / 64 个 / 1024m 由 §29 的取值代替）：① 一般视野约 32m（主视野进入半径改为 32m，离开与客户端上限随之调整）；② 大地图一个进程至少要撑住 2048m × 2048m 级别（普通场景的地图上限从 1024m 见方提高到至少 2048m 见方，并实测整图装入的内存与装入时间）；③ 同步的实体数一般约 128 个（主视野“最近 N 个”名额从 64 提高到 128，会话上限随之调整，重估每观察者下行带宽与 50KB/s 预算）。entitysync 冻结契约的取舍（增量正确优先）同日确认按推荐。

## 29. 游戏尺度参数：视野 32m、单进程 2048m、同步 128 个（2026-11-10）

维护者 2026-11-10 对 §28.8 给出游戏尺度的口径：“一般视野是32m左右，而且大地图一般一个进程至少能撑住 2048*2048的级别，还有同步的entity 数量 一般是128左右吧”。本节实施这三条（只改开地形的普通场景；不开地形的场景与分区场景逐字段不变），取代 §28 的 30m / 64 个 / 1024m。分支 `feat/terrain-game-scale`（基于 `cf89e081`）。证据、复跑命令、修前红与变异原文见 [evidence/terrain-game-scale-20261110](evidence/terrain-game-scale-20261110/README.md)。

### 29.1 取值

世界单位厘米。主视野的名额从服务端常量 `sceneMaxVisible` 移进场景配置 `gamescene.View.MaxVisible`，与半径一起由 `DefaultView` / `TerrainView` 给出；会话上限由名额推出（`sceneSessionObjects` = 名额 + 自身 / 场景 / 队友 3 + 留给关系视野的 33），不再单独核对。

| 项 | 不开地形的场景、分区场景（`DefaultView`，不变） | §28（已取代） | 开地形的普通场景（`TerrainView`，§29） |
| --- | --- | --- | --- |
| 进入 / 离开半径 | 1.2m / 1.5m | 30m / 33m | **32m / 35.2m**（滞回 10%） |
| 客户端可要的视野（进入半径） | 0.6～2.7m，步长 0.3m | 15～60m（离开 63m），步长 3m | **16～64m（离开 67.2m），步长 3.2m** |
| “最近 N 个”（`View.MaxVisible`） | 64 | 64 | **128** |
| 会话上限（Manager `Limits.MaxObjects`，也是每帧对象数） | 100（`Limits` 仍是零值） | 100 | **164** |
| 会话引用表（加地形 64，D15） | 164（分区场景开地形时） | 164 | **228** |
| AOI 块边长 | 1m | 32m | 32m（不变，29.2） |
| 每个观察者最坏订阅块数（缺省 / 最大视野） | 25 / 64 | 16 / 25 | 16 / 36（预算 1024） |
| 普通场景的地图上限 | — | 1024m 见方的面积 | **2048m 见方的面积**（32 × 32 块；如 64 × 16、1024 × 1 块也可） |

- **进入 32m 正好是一个 AOI 块边长**。离开半径按 §28 的比例取滞回 10%：35.2m。维护者举例的 35m 配步长 3m 时，从最小 16m 起按 3m 取整得不到 32m 与 64m（客户端要缺省视野或最大视野会被取整到 31m / 61m）；取滞回 = 步长 = 3.2m，最小 = 缺省的一半（§28 的同一组比例），32m = 16 + 5 × 3.2、64m = 16 + 15 × 3.2 都在网格上（29.8 ①）。
- **客户端最大 64m**：“约缺省的 2 倍”，与 §28.8 ① 同一口径（客户端能要的进入半径），离开上限 64 + 3.2 = 67.2m。
- **会话上限 164**：沿用 demo 原尺度的结构（64 + 3 + 33 = 100），名额 128 时 128 + 3 + 33 = 164；名额与会话上限一起定，所以挤在一起时由名额截（最近的 128 个），会话不会进入超额降级（D17）；名额拿掉时才降级，收敛到 164。
- **客户端**：每帧对象数按会话上限切，开地形的普通场景一帧至多 164 个对象。Go `frame.Decode` 与 C# `SyncFrame.Decode` 都按 `maxObjects × 2` 拒绝，缺省 100 收得下（上限 200），C# 不用改；`EntityViews` 若显式设容量不得小于 228（[client/README](../../client/README.md)）。协议、存储、C# 代码都不变。

### 29.2 块边长核对

32m 块在新视野下每个观察者缺省最坏 16 块、最大视野 36 块（1m 块是 5184 / 18496 块，超预算被拒：`TestTerrainMainViewFitsTheBlockBudget`）。2048m 地图（4000 人，29.5）上 16m / 32m / 64m 三档同机交替：平均订阅块 28.9 / 10.1 / 4.3，每次复制 14.9ms ±36% / 19.9ms ±25% / 22.5ms ±18%（6 轮）；16m 与 32m 再单独交替 10 轮是 20.5ms ±27% / 19.4ms ±18%，即两者在噪声内相同，64m 不更快。**保持 32m**：耗时与 16m 相同，订阅的块少三分之二，常驻内存少 8MiB。

### 29.3 单进程 2048m：整图装入实测

`TestTerrainGameScaleLoadMeasure`（`ROOST_TERRAIN_SCALE_MEASURE=1`）：按 `startTerrain` 的顺序（`terrainRegion` → 打开包 → 接建筑 → 装入 → 建筑对账），再建场景同步（场景 Manager、主视野与地形 Interest 的块表）并启动地形同步（`loadEdited`）。每项 3 轮，取 GC 之后的 HeapAlloc 增量。持久层是离线替身（记录在内存里），真实 Mongo 读 1024 条记录的时间没有测。

| 地图 | 包 | 装入（开始到装完） | 视图 + 块记录 | 场景 + 地形同步的块表 | 合计常驻 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1024m 平地（16 × 16，对照） | 18KB | 2.7～5.5ms | 3.0MiB | 0.08MiB（24～134µs） | 3.1MiB |
| **2048m 平地（32 × 32）** | 0.1MiB | **7.5～9.3ms** | **12.1MiB** | 0.28MiB（53～135µs） | **12.4MiB** |
| **2048m `-shape generated`** | 2.8MiB | **11.4～16.6ms** | **12.1MiB** | 0.28MiB | **12.4MiB** |
| 2048m 平地、每块都改过 | 0.1MiB | 0.68～0.87s（其中替身把 1024 条记录建成实体 0.33～0.52s） | 12.1MiB + 实体 17.6MiB | 0.28MiB；`loadEdited` 1024 块 0.6～1.4ms（+0.6MiB） | 30.5MiB |
| 4096m 平地（64 × 64，超过上限，只作参考） | 0.3MiB | 24～27ms | 48.3MiB | 1.1MiB | 49.6MiB |

`terrainRegion`（面积检查）约 15ns。视图每块约 12KiB（高度 2 字节 + 材质 1 字节 × 4096 点），所以装入成本与块数成正比；地图大小对场景同步只多块表（2048m 是 64 × 64 = 4096 个 32m 块，加地形 Interest 的 1024 个 64m 块）。**结论：2048m 整张装进一个进程约 12MiB、10ms 级，改过的块全部常驻也只到约 30MiB；上限设为 2048m 见方的面积**（维护者的下限）。4096m 的装入（约 50MiB、30ms 内）也不构成负担，是否再放宽见 29.8 ②；真正随地图变大的成本是人（29.5）。

### 29.4 用例

| 用例 | 内容 |
| --- | --- |
| `TestAnOrdinarySceneTakesA2048mMap`（新） | 32 × 32、64 × 16 块被 `terrainRegion` 接受，33 × 32、32 × 33、64 × 17 拒绝且指向分区场景；32 × 32 平地包按启动顺序整张装入（1024 块都来自原始包）、场景世界 = 2048m 图 + 游戏尺度视野、场景与地形同步构造得出来 |
| `TestTheGameScaleMainViewHoldsTheNearest128`（新） | 开地形的普通场景，140 人挤在 19m 以内：每人恰好持有最近的 128 个、不暂缓不断开；名额拿掉、170 人：每个会话收敛到恰好 164、不断开、丢掉的不比留着的近（D17 在新上限下） |
| `TestBuildingsAtTheTerrainMainViewScale`（改） | 31m（玩家）/ 约 31.4m（怪、tower 的占地中心）进来，33.4m 的怪不进来；退开 2m（33～33.4m）都留着（离开 35.2m），退到 36m 外都离开、走回来都回来；客户端最大视野 64m、看得见 63m 的怪；站在四种建筑旁边都看得见 |
| `TestManyBuildingsDegradeWithTheCrowd`（改） | 161 只怪 + 4 座 tower + 自身 = 166 > 164：收敛到 164（2 座 tower 暂缓）、不断开，拆掉留着的两座、暂缓的两座进来 |
| `TestBuildingsTakeMainViewPlacesLikeAnyoneElse`（改） | 4 座 tower + 124 只怪占满 128 个，14m 的玩家被挤出 |
| `TestBuildingsHaveNoReplicationOfTheirOwn`（改） | 引用表：开地形的普通场景 164 + 64 = 228，分区场景 100 + 64 = 164；场景的会话上限 164 |
| `TestTerrainSceneWorldAndItsLimit`（改） | 开地形的普通场景是 `TerrainView` 的字面量；不开地形的场景逐字段等于原来（含名额 64）；分区场景的 Base 世界、手写 `Config` 是原尺度；给了半径没给名额的 `View` 补成 64；上限表改为 2048m（32 × 32、64 × 16、1024 × 1 可以，33 × 32、32 × 33、1025 × 1、157 × 157 拒绝） |
| `TestACrowdedSpawnPointKeepsEveryReplicationSessionOpen`（改） | 不开地形的场景名额仍是 64（字面量） |
| `TestTerrainMainViewFitsTheBlockBudget`（改） | 2048m 地图、32m 块构造得出来（16 / 36 块），1m 块被拒（5184 / 18496 块） |
| `TestMainViewMeasureMovingCrowdBytes`、`TestTerrainGameScaleLoadMeasure`、`BenchmarkSceneMainViewScale`（新 / 改） | 29.3、29.5、29.6 的测量 |

**修前红**：四个新 / 改用例在改动前的代码（`cf89e081` 生成的工程，只加这四个函数，编译垫片一处：2048m 用例里构造场景的名额参数）上都失败，落在承诺本身：`timed out waiting for the player, the monster and the tower 31m away`；`timed out waiting for the viewer's session settled at its limit`（164）；`player 34000 holds 64 other subjects …, want the nearest 128` 与 `player 35000 holds 100 objects …, want itself among exactly 164`；`32 × 32 chunks (2048m × 2048m) on an ordinary scene: … at most 1048576m² (about 1024m × 1024m)`。

**变异 11 项全部变红**：进入回到 30m（不在量化网格上，构造即拒绝）；进入 / 离开放大一档到 35.2m / 38.4m（33.4m 的怪进来）；去掉滞回；离开上限 63m（客户端最大 57.6m）；开地形的名额仍是 64；会话上限固定 100；Manager 不设 `Limits`；地图上限仍是 1024m；上限放到 2112m（33 × 32 被接受）；不开地形的名额也改成 128（非地形场景与分区场景的用例变红）；`Normalize` 不补名额。原文见证据。

### 29.5 2048m 地图上的场景同步

`BenchmarkSceneMainViewScale`：§28.3 的人群按面积放大 4 倍——2048m 地图分四个 1024m 象限，每个象限一个 320m 的镇子（1024 座建筑、500 个玩家）、另 500 个玩家与 2000 只怪散在象限里，合计 4000 个玩家（4000 个会话）、8000 只怪、4096 座建筑；一半玩家（6m/s）、四分之一的怪（3m/s）在走，每次复制（50ms）约 1000 个移动。另在 1024m 地图上用 §28 的参数与 §29 的参数对照，并用改动前的代码（`cf89e081` 生成的工程）跑 §28 的同名变体。6 轮交替（每轮七个变体各一次、顺序轮换，每次 300 op）的中位数；机器 Apple M5，loadavg 7～13（基准开始前几分钟曾到 480），交换占用约 21GB（`bench-env.txt`），时间只作同一轮里的对照：

| 变体 | 每次复制 | 每次分配 | 订阅块 / 观察者 | 场景常驻 | 可见 / 观察者（其中建筑） | 镇子里每人漏掉的 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 改动前的代码，1024m，30m / 64 | 3.02ms ±25% | 1.05MiB | 9.1 | 67.3MiB | 30.3（15.4） | 0.004 |
| 本分支，1024m，30m / 64（§28 参数） | 3.08ms ±13% | 1.05MiB | 9.1 | 67.2MiB | 30.3（15.4） | 0.004 |
| 本分支，1024m，**32m / 128** | 3.66ms ±272%（6 次里 4 次约 3.1ms） | 1.15MiB | 10.0 | 72.0MiB | 34.3（17.4） | 0 |
| **2048m（4000 人），32m / 128，32m 块** | **19.9ms ±25%** | 4.83MiB | 10.1 | **281.8MiB** | 34.2（17.2） | 0 |
| 2048m，人人要最大视野 64m | 42.7ms ±17% | 42.8MiB ±38% | 26.5 | 536.9MiB | 85.8（39.9） | 66.5 |

读法：同参数下改动前后在噪声内相同（代码的结构改动不增加成本）；同一张 1024m 图上 32m / 128 比 30m / 64 每人多看 4 个，镇子里不再漏（“最近 128 个”在缺省视野下从不满），常驻多 5MiB。2048m 的成本来自人数（4000 个会话、约 70KiB / 人），不是地图：每次复制约 20ms（50ms 窗口的 40%）、场景常驻约 280MiB；人人开到 64m 时约 43ms、540MiB，接近复制窗口（29.8 ⑤）。64m 视野下全体平均可见 85.8 个（其中 39.9 座建筑），镇子里每人平均有 66.5 个 64m 以内的对象因名额 128 没进来（名额被更近的对象占满），与 §28.5 的结论同理：密集处把视野开大不多看见。

### 29.6 每观察者下行：增量字节压缩后 128 个同时动约 18.6KB/s，在 50KB/s 以内

`TestMainViewMeasureMovingCrowdBytes`：开地形的普通场景，看的人站着，周围 10～29m 的 N 个玩家同时走（MoveTarget，移动心跳 200ms 一步，沿切线方向斜走，一步同时改 `pos_x` 与 `pos_y`），每一拍复制一次，记看的人收到的帧 payload（含帧头，不含线上消息头；KB = 1024 字节）。压缩前是本节初版的实测（帧协议 1 / 信封 v2），压缩后是帧协议 2 / 信封 v3、DAO 增量不带实体 ID（[sync-delta-bytes 证据](evidence/sync-delta-bytes-20261110/README.md)），同机同用例：

| 同时在动的可见对象 | 压缩前：每拍 / 每对象 / 下行 | 压缩后：每拍 / 每对象 / 下行 |
| --- | ---: | ---: |
| 64（§28 的名额） | 7.6KB / 119 字节 / **37.3KB/s** | 1.9KB / 30.4 字节 / **9.5KB/s** |
| 128（§29 的名额） | 15.1KB / 118 字节 / **73.5KB/s** | 3.8KB / 29.7 字节 / **18.6KB/s** |

- 压缩后每个斜走的对象一拍约 30 字节：对象头 4 + 组件头 4 + 信封 4～5 + DAO 补丁约 16（`sync_format` 3、`pos_x` 5、`pos_y` 5，个别步带 `facing`），帧头分摊后可忽略。128 个同时动约为预算的 37%；按每对象 30 字节，同时在动的可见对象到约 340 个才到 50KB/s（主视野名额是 128，会话上限 164）。
- 进入视野的全量也变小：64 个 2829 + 3327 + 866 = 7022 字节（压缩前 11447），128 个 13893 字节（压缩前 22583），其中玩家全量仍是缺省视图 `"*"` 的全部字段（KNOWN-LIMITS 记有改用 `near` 视图的待决定项）。
- 运行久了 Tick、Version 的 uvarint 会各长 1～2 字节（证据 §4.1），每对象稳态约 32 字节，结论不变。
- 本轮不设计、不实现降频 / LOD；压缩后不再需要为 128 个对象的预算向维护者提方向。

### 29.7 验证

生成的 game-demo 全量 `go build` / `go vet` / `go test ./...` 通过；`internal/service/game`、`game/scene/...`、`game/lifecycle`、`game/settings`、`game/terrainmap` `-race` 通过，场景 / 建筑 / 地形同步相关用例 `-race -count=3` 通过；`roost id check`、`roost generate --check` 通过。core 侧根包、`codegen/internal/roost`、文档锚点检查通过。没有真实依赖、真实进程演练与 Unity；C# 代码未改。

### 29.8 待维护者确认（维护者 2026-11-10 确认按推荐）

实施方按推荐先定、已实施的取舍（推荐标★）：

| 项 | 现状 | 选项与代价 |
| --- | --- | --- |
| ① 离开半径与步长 | ★ 离开 35.2m、步长 3.2m、最小 16m（滞回 10%、步长 = 滞回，32m 与 64m 在网格上） | 或离开 35m / 步长 3m：要么最小改成 14m（14 + 6 × 3 = 32、14 + 16 × 3 = 62，客户端最大 62m），要么步长与滞回脱钩（如步长 4m） |
| ② 普通场景的地图上限 | ★ 2048m 见方的面积（维护者的下限，已实测） | 或放宽到 4096m：装入约 50MiB、30ms 内（29.3），但同密度的人是 16 倍，单进程的复制成本（29.5）先到顶 |
| ③ demo 缺省地形包是否改成 32 × 32 | ★ 不改，仍 16 × 16（1024m）：改了只多约 9MiB 视图、几毫秒装入，没有实测的代价，但 README 的分区示例（四个 512m 的格正好覆盖缺省包）、客户端金样与各演练的地图都按 1024m 写 | 或改成 32 × 32（`terrainpack` 缺省 `-chunks-x/-y 32`，分区示例改为 4 个 1024m 的格） |
| ④ 会话上限的余量 | ★ 沿用 33 个关系视野余量：128 + 3 + 33 = 164 | 或收紧到 128 + 3 + 少量（如 140）：引用表与每帧对象数小一些，关系视野（公会、多人队伍）的余量变少 |
| ⑤ 2048m / 4000 人的复制成本 | ★ 接受：缺省视野每次复制约 20ms / 50ms、场景约 280MiB；人人开到 64m 约 43ms、540MiB | 或限制客户端最大视野（如 48m）：订阅与重评成本下降，空旷处看得近一些 |
| ⑥ 下行超过 50KB/s | 已重算（29.6）：增量字节压缩后 128 个同时动约 18.6KB/s（压缩前 73.5KB/s），在预算内，不再需要决定 | — |
| ⑦ 分区场景 | ★ 仍是 demo 原尺度（64 个、会话 100），随 P5 一起放大（§28.6）。**已随 P5 放大，见 [§30.5](#305-分区场景的主视野放大到游戏尺度)** | 或现在放大：witness / Ghost 带 / 格边长一起重定并重测 |

## 30. P5 跨格编辑、分区场景的建筑与游戏尺度的分区视野（T08，2026-11-10）

> **2026-11-10 更新（维护者）：跨格笔刷改为直接拒绝，拆分设计已撤回**，见 [§30.10](#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)。下面 §30.1～§30.3、§30.7 里关于“按格拆分 / 多份汇总 / 部分成功 / 多格配额退还”的原文保留，标“已撤回（维护者 11-10）”；单格转发、发号、格账本、逐块 `Outcome`、`terrain_pending`、重启对账照旧有效。

维护者 2026-11-10：未完成的代码先完成，验收后置。本节是 T08 的代码部分：跨格编辑（拆分、转发、各格一笔事务、配额、部分成功与结果未知的对账）、分区场景的建筑（单格可放、Ghost 带的占位、跨格占地）、分区场景的主视野放大到游戏尺度（§29.8 ⑦）。真实依赖与真实多进程演练由维护者跑（`ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh` 已按本节更新）。[证据](evidence/terrain-p5-20261110/README.md)。

### 30.1 拆分与转发

> **已撤回（维护者 11-10）**：下面第 1 步的“按写它的格分组”、第 4 步的“每个格一份、并行发出、等齐”与第 6 步的“汇总”只为跨格笔刷服务，已删除；现在覆盖块的格多于一个就在第 2 步之前拒绝（`terrain_cross_cell`），一次编辑只整份转发给一个格，见 [§30.10](#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)。第 3、5 步与进程间消息不变。

玩家连在自己的 Base 进程上；块只归包含它的格的进程写（§4）。一次编辑由玩家的 Base 拆成“每个格一份”（`terrainmap.Map.RelayBrush` / `RelayPlace` / `RelayRemove` / `RelayParams`，`game/terrainmap/relay.go`）：

1. 事务前校验（不看块归属）：参数、Policy（半径、增量、块数，`checkBrushParams` / `checkPlacementParams`）；覆盖到的块按写它的格分组（`Forwarder.OwnerOf` = 分区网格，没有格写的块答 `ErrNotOwned`）。
2. 配额（Base 扣，30.3）与权限钩子（`Rules.AllowEdit` / `BuildRules.AllowBuild`，在 Base 调，与单格相同）。
3. **发号**：玩家面向客户端的账本（`TerrainEditor`，全局库，只有玩家的 Base 写）一笔 strict 事务（handler `TerrainEditIssue`）：过期 / 超前答 `terrain_seq`（不转发）；新的记下；发过的（客户端的重试）答 `Again`、照样再转发。记下在转发之前落盘：`LastSeq` 查询（Seq = 0）答的数覆盖每一个可能已在某个格执行了的序号，客户端从 `LastSeq + 1` 续号不会撞上格里用过的序号。
4. **转发**：每个格一份 `PartRequest`（身份是客户端的（玩家, 序号），笔刷整笔转发，格自己按同一几何算出归它写的块：`Brush.ApplyIn` / `Footprint`，§30.6），并行发出、等齐。本进程托管的那一格直接执行，不经网络。
5. **格执行**（`Map.Exec`）：一笔 strict 事务锁住这一部分的块（建筑：占地的块 + 建筑实体）与玩家在本格的账本（`TerrainCellEditor`，kind 14，`dbscope=sid`：每个格进程写自己的库）；不扣配额、不调权限钩子。账本规则同 D18 的窗口，只是不判超前（Base 判过；玩家在别的格编辑很久之后第一次来本格，序号比本格的 `LastSeq` 大很多是正常的）。同一（玩家, 序号）在一个格至多执行一次。
6. **汇总**（`RelayResult.Err`）：各格 Applied / Duplicate 是成功；有 Unknown 答 `terrain_pending`（不论别的格）；否则答第一个拒绝的原因（按 sid 升序）。答复逐块带结论（`TerrainChunkVersion.Outcome`：1 执行了、2 以前执行过、3 拒绝、4 结果未知）。

进程间是 cellworld 扩展消息（命名空间 `terrain-edit`，`internal/service/<game>/terrain_relay.go`），至多一次：Base 按（Base 的 incarnation, 序号）每 250ms 重发、2s 期限，格对同一身份正在执行的忽略、执行完的确定结论缓存 4096 条（重发直接答缓存），结果未知的不缓存（重发再执行一次，账本答 Duplicate），同时执行至多 64 份、满了丢（Base 会重发）。线上格式首字节是版本 1。

### 30.2 责任方（完成判据）

| 情形 | 谁负责、怎么收敛 |
| --- | --- |
| ~~部分格拒绝（占位、权限之外的拒绝、块装不进来）~~ **已撤回（维护者 11-10）：一次编辑只有一个格，没有部分成功；格拒绝即整笔没写，客户端可用同一序号重发** | 不补、不回滚（D5）：已执行的格保留，答复逐块列出结论，汇总码是第一个拒绝的原因。客户端可以用同一序号重发：已执行的格答 Duplicate，拒绝过的格再判一次（与单格“拒绝不记序号”的语义相同） |
| 期限内没有结论（丢包、格忙、格的事务结果未知） | Base 在期限内按同一身份重发，格去重；期限到了这一份是 Unknown、汇总 `terrain_pending`，**由客户端用同一个序号重发**（与单格“结果未知”的规则相同）：Base 账本答 Again 再转发，执行过的格答 Duplicate，没执行的这次执行 |
| 重发、重复、迟到的消息 | 格：同一身份在途忽略、确定结论按缓存答；跨身份由持久的格账本答 Duplicate。Base：只认自己这次启动、正在等、发给那个 sid 的答复。不会双写 |
| 格进程重启 | 块与格账本是 strict 持久；重启后同一序号再来，账本答 Duplicate 或执行一次 |
| Base 进程重启 | 面向客户端的账本是 strict 持久；客户端重发同上（Again → 各格 Duplicate / 执行）。Base 不持久“没完成的编辑”：客户端不再重发时，结果未知的那一份停在它实际的状态（执行了或没有），接缝由各格的副本推送最终一致（T05） |
| 接缝 | 各格独立提交；邻格的 Ghost 带与客户端经副本跟上各格的提交（T05，§21），没有跨格原子性（D5） |

**编辑日志压实：不需要。** 判重是两份定长账本（每玩家 2 个 int64 + 建筑数，Base 一份、每个格一份），不随编辑次数增长；块上没有编辑记录（T02 起），副本按整块推最新已提交状态（§21），没有要回放的日志。

### 30.3 配额（D16，Base 扣）

> 多格的退还（按各格结论逐份相加、Unknown 那几份计各自的 Footprint）**已撤回（维护者 11-10）**：现在只有一个格，Applied 计实际点数、Unknown 计整笔的 Footprint、Refused / Duplicate 计 0，至少 `MinEditPoints`；跨格的拒绝在预扣之前，不扣（[§30.10](#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)）。

Base 没装这些块（只做 Base 的进程不装块），按 `Brush.Footprint`（每块覆盖的采样点数；一次笔刷只改高度或只改材质，所以它是实际改动点数的上限，见待确认 ③）预扣，至少 `MinEditPoints`；格答回实际改动的点数，Base 按结论多退：Applied 计实际点数、Unknown 计它的 Footprint（可能已生效，不退）、Refused / Duplicate 计 0，合计至少 `MinEditPoints`。转发之前的拒绝（权限、账本过期 / 超前）退到 `MinEditPoints`。建筑：放置预扣 `MinEditPoints` + 占地面积（整平时），Applied 计 `MinEditPoints` + 实际整平点数；拆除、改参数 `MinEditPoints`。配额只在 Base 的进程内存（重启从满桶开始，与单格相同）。

### 30.4 分区场景的建筑

- **单格内的建筑**：分区场景接建筑（`EnableBuildings` 不再只在普通场景）。放置 / 拆除 / 改参数由玩家的 Base 转发给写占地（拆除、改参数：锚点所在块）的格，那个格一笔事务（块、建筑实体、玩家在本格的账本）；建筑实体照旧归锚点所在格写（ID 由锚点决定，全局库，只有一个进程写）。
- **跨格占地：首版拒绝**（`building_cross_cell`，100069，在转发之前、什么都不写、不发号）。原因：一座建筑的放置要在一笔事务里锁住占地覆盖的全部块（§25 的串行化点），跨格就是跨进程事务；“占地不跨格”是 §27.6 列的二选一里成本低的一边。代价：格边上一条宽度 = 建筑边长的带里放不下跨边的建筑（hut 4m、keep 12m）。见待确认 ①。
- **Ghost 带里邻格的建筑对行走判定可见**：建筑是普通实体，只有地形块特殊，不另开通道——块上的占地（`Lots`，§25 的串行化索引）随地形副本一起推（`ReplicatorConfig.Lots`，副本线上格式 v2：订阅带“要占位”与持有的修订号，块状态带修订号 + 打包的占地；只有占位变了时不带块数据）。占地提交后（`publishLots` 之后的 `SetLotsCommitted`）立即推送；订阅续期按修订号补齐。邻格收到后进同一张占位表（`Map.ApplyReplicaLots`，修订号只进不退），`Walker` 不能走进去；启动时 Ghost 带的占地也按库里的记录装入（以前只装本格的，开发中发现：重启后要等第一次续期才看得见，修前红见证据）。
- **建筑数上限按格计**（每个格的账本数自己格里的建筑）：见待确认 ②。
- **客户端看见邻格 Base 上的建筑：未覆盖**。建筑实体在写它的格进程上进那个进程的场景主视野（与普通场景相同的 `ShowSubject`），连在别的 Base 上的玩家看不见（需要像地形块那样的只读副本，§27.6）。列入未覆盖。

### 30.5 分区场景的主视野放大到游戏尺度

开地形的分区场景，Base 的主视野改为与开地形的普通场景同一份 `TerrainView`（进入 32m、离开 35.2m、客户端 16～64m、离开上限 67.2m、最近 128 个）与 32m 的 AOI 块（`replicatedWorld`）；不开地形的分区场景不变（demo 尺度、2m 块）。客户端看到的只来自 witness，所以：

- `view_radius`（R）≥ 离开上限 6720（`settings.PartitionTerrainMaxBaseLeaveRadius`，加载配置时查；`replicatedWorld` 运行期再核对一次，测试断言它与 `TerrainView().MaxLeaveRadius` 相等）；
- `ghost_band`（G）≥ R + `hysteresis`（H），`wake_radius` + H ≤ G（原有检查）；`cell_size` > G（网格）且是 6400 的倍数。
- 推荐一组：R 6800、H 200、G 7000，格 ≥ 128m（演练用 12800 × 2；正式地图建议格远大于 G，例如 512m～1024m）。70m 的 Ghost 带比一块宽，一个格装入本格外两列块；演练的 4 × 2 块小图上每个格都装下整张图（演练里“远处的块不在 Ghost 带里”的那一步因此改为“也在 Ghost 带里”）。
- 成本没有实测：witness 半径从 4m 到 68m，每个 witness 里的对象多得多（cellworld 的 witness 是按半径全量列出）；Ghost 带从 5m 到 70m，Ghost 数与副本订阅的块数随之增加。列入未覆盖（T09）。

### 30.6 gameplay/terrain 的底座

`Brush.ApplyIn(meta, limits, chunks, part)`：只算 part 里的块（与整笔应用的结果逐点相同，各份拼起来就是整笔）；`Brush.Footprint`：每块覆盖的采样点数；`View.BrushChangesIn`。副本的占位记录（`ReplicaLotStore`、`ReplicatorConfig.Lots`，线上格式 v2）。见 `gameplay/terrain/README.md`。

### 30.7 验证

| 用例 | 内容 |
| --- | --- |
| ~~`terrain_relay_test.go` `TestTerrainRelaySplitsABrushAcrossCells`~~ 已撤回（维护者 11-10），改为 `TestTerrainRelayForwardsABrushToTheCellThatWritesIt` 与 `TestTerrainRelayRefusesABrushAcrossCells`（§30.10） | 两个格 + 只做 Base 的进程（进程内网，正式 Map / Nest / 生成代码）：跨格笔刷拆成两份、各一笔事务，两块与整图上应用一次逐点相同，版本 1，两个格账本与 Base 账本各记一次；玩家在格上时本格那一份不经网络 |
| ~~`TestTerrainRelayPartialRefusalKeepsTheOtherCell`~~ 已撤回（维护者 11-10），改为 `TestTerrainRelayRefusalWritesNothingAndRetries`（单格拒绝、同序号重发执行） | 一个格拒绝（占位钩子），另一个格提交、不回滚，汇总是拒绝原因、拒绝的格什么都没写；同一序号重发：Again，已执行的 Duplicate、拒绝过的执行 |
| `TestTerrainRelayUnknownResultsAreRetriedOnce` | 格的答复全丢：重发被缓存 / 在途去重吸收，期限后 Unknown、`ErrPending`，格只执行一次；网好后同一序号全是 Duplicate、版本不变；请求全丢：格没执行，重发后执行一次 |
| `TestTerrainRelayDuplicatedAndLateMessagesWriteOnce` | 每条消息投两次、事后全部重放：版本与格账本不变，迟到的答复被忽略 |
| `TestTerrainRelayReconcilesAfterRestarts` | 答复丢了的那一份在格上已执行；格重启后同一序号 Duplicate、不写第二次；Base 重启后 Again、各格 Duplicate，`LastSeq` 是 1，新序号照常 |
| `TestTerrainRelayChargesTheBaseByWhatWasApplied` | 冻结时钟：什么都不改的跨格笔刷只计 `MinEditPoints`（下一份在余额内的笔刷能过）；一个格结果未知时它的 Footprint 不退（补到差 1 点时最小的笔刷被拒、再补 0.1s 通过） |
| `TestTerrainRelayBuildingsStayInOneCell` | Base 转发放置到写占地的格（建筑在那个格、不在 Base；格的行走判定看得见），重发 Duplicate；改参数、拆除转发；跨格占地在转发前拒绝、不留实体、不发号；跨格笔刷改到格里建筑占着的点：那个格拒绝、另一个格执行 |
| `TestTerrainPartitionGhostBandSeesNeighbourBuildings` | 格 101 在交界块上放建筑：102 的 Ghost 带经副本收到占位（`LotsApplied`），102 的 `Walker` 走不进去；102 重启后启动即看得见；拆除后又能走 |
| `TestTerrainRelayWireFormat` | 四种请求、五种答复往返；截断、尾随字节拒绝；版本、kind、op 不对计 decode error；可跨进程的拒绝原因还原为同一哨兵错误，没有归类的是 `ErrCellFailed` |
| `terrain_cell_editor` 账本 | 超前的序号在格上是 Fresh；窗口与重复同玩家账本；DAO 往返；回滚撤销 |
| 控制器 | `terrain_pending` / `building_cross_cell` 的码；答复逐块的结论与汇总码 |
| 视野 | `TestTerrainSceneWorldAndItsLimit`：开地形的分区场景是 `TerrainView` + 32m 块，不开地形的不变，witness 半径不够时拒绝；`TestTerrainRegionFollowsThePartition`：推荐的一组数通过，`view_radius` 400 拒绝 |
| 变异 | 22 项：19 项红，3 项未杀死（各有兜底，见证据） |

修前红：跨格编辑与 Ghost 带占位在改动前没有入口（改动前的代码对跨格编辑答 `terrain_not_owned`，分区场景放置答 `building_disabled`，无缝演练 (a) 步也是按“拒绝”写的），这些用例在修前的代码上编译不过，不能当红；每条行为的红由“只撤掉守卫它的那一行”的变异给出。开发中真实变红一次：Ghost 带的占地启动时不装（`the restarted neighbour does not see the building`），已修。

### 30.8 实施方先定的取舍（维护者 2026-11-10 确认：全部按推荐★）

维护者 2026-11-10：① “跨格”即占地落在两个地图进程（两个分区格的拥有者）上，这种放置直接拒绝，不做跨进程事务；其余各项按推荐。~~注意 ① 只针对建筑占地；地形笔刷跨格仍按 §30 拆分转发给各格执行（P5 的本体）。~~ **笔刷同样拒绝**（维护者 2026-11-10 稍后：“跨格是指跨两个地图进程吗，如果是的话就直接拒绝”“玩家也不能刷地形，会在地形基础上设计这个限制”；`terrain_cross_cell`，[§30.10](#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)）。

| 项 | 现状 | 选项与代价 |
| --- | --- | --- |
| ① 跨格占地 | ★ 拒绝（`building_cross_cell`）；笔刷同样拒绝（`terrain_cross_cell`，§30.10） | 或实现：两格的块锁在一笔跨进程事务里（Remote / saga 两阶段），代价是放置延迟与故障面；或把建筑交给锚点所在格、邻格只读占位（会出现两格同时放的重叠，需要另一条串行化） |
| ② 建筑数上限 | ★ 按格计（每格的账本只数本格的建筑） | 或全局计：Base 的账本记数，放置结果未知时 Base 计数与实际不一致，需要对账 |
| ③ 配额口径 | ★ 预扣 Footprint（覆盖点数），格答回实际改动的点数（高度、材质各计一次，与单格同一口径）多退 | 现在的笔刷一次只改其一，实际 ≤ Footprint；以后有同时改两样的笔刷时预扣要计两倍 |
| ④ 期限与重发 | ★ 250ms 重发、2s 期限、格缓存 4096 条确定结论、并发 64 | 期限短则 `terrain_pending` 多、客户端重发多；长则玩家等得久 |
| ⑤ Base 不持久未完成的编辑 | ★ 不持久：客户端负责用同一序号重发（与单格相同） | 或 Base 持久一个出站表、重启后自己补齐：多一张表与恢复流程，客户端不重发时也最终完成 |
| ⑥ 格到进程的映射改动 | ★ 运维规则：改 `owners` 先停机、等客户端在途重试结束（格账本不跟着搬） | 或格账本按格（而不是 sid）分库：`dbscope` 要支持按格，生成器改动 |
| ⑦ 分区视野的推荐数 | ★ R 6800、H 200、G 7000（R 取离开上限 6720 之上的整数） | 或 R = 6720 正好、G 6920；或更大的滞回 |
| ⑧ 格账本所在的逻辑库 | ★ 仍在 `db=main`、`dbscope=sid`：main 因此成为混合库（合服清单把它列为按 sid 分库，`sidmerge migrate` 搬 `<物理名>_<sid>` 里的格账本，其余 DAO 照共享库要求同址；不开分区的工程这部分没有文档）。生成配置、映射不变；doctor `merge-readiness` 的摘要改为每个逻辑库都列进“两侧同址”（原先按 sid 的库只列 migrate，混合库的同址要求被漏掉） | 或单独的逻辑库（如 `terrain_cell`）：main 保持纯共享，但生成配置的 `dataengine.databases` 要为它写映射（现在的 Mod 示例只写 main，所有工程共用，没有地形的工程会因“没有 DAO 声明”被拒），不写映射则物理名直接用逻辑名、启动告警、同一 Mongo 上的两个工程会撞库。两种做法下合并两个分区格的 sid 都会在同一玩家的账本上冲突整体停止：分区格不是合服对象 |

### 30.9 未覆盖

- 真实依赖（Mongo / WAL / NATS）与真实多进程演练：演练配置与 (a) 步已更新（~~跨格笔刷由 B 拆给 A 与自己、两块都执行，再反向改回~~ 11-10 改为：跨格笔刷被拒绝、B 发起整份落在 A 格的笔刷转发执行再改回，§30.10）；合入前协调方已在私有真实依赖上跑通（[证据](evidence/terrain-p5-20261110/README.md#合入前的真实依赖验证协调方2026-11-10)）；`TestTerrainEditLedgerRealMongo` 等真实 Mongo 用例没有为格账本另写。
- 客户端看见别的 Base 上的建筑（建筑副本，§27.6）。
- 跨格占地的建筑、跨格的笔刷（都按决定拒绝，§30.10）；跨格寻路。
- 游戏尺度分区视野的成本（witness 列表、Ghost 数、副本订阅），T09 规模。
- Unity 端对 `Outcome` / `terrain_pending` 的处理（C# SDK 没有改：答复是生成的协议类型）。
- 玩家换 Base 进程之后旧进程里的 `TerrainEditor` 副本（尚未确认，[KNOWN-LIMITS](KNOWN-LIMITS.md)）；分区场景里建筑进 Base 场景主视野的接线只有编译与单元覆盖。
- 变异未杀死三项（立即推送占位、格的在途去重、答复的 incarnation 核对），原因见证据。

### 30.10 跨格笔刷直接拒绝（维护者 2026-11-10，撤回 §30.1 的拆分）

维护者 2026-11-10：“跨格是指跨两个地图进程吗，如果是的话就直接拒绝”；“玩家也不能刷地形，会在地形基础上设计这个限制”。地形笔刷与建筑占地同一规则：覆盖的块归两个及以上地图进程（分区格的拥有者）时直接拒绝，不拆分转发；玩法层靠地形设计避开这种情况。证据见 [evidence/terrain-brush-single-cell-20261110](evidence/terrain-brush-single-cell-20261110/README.md)。

**契约**

- `terrainmap.ErrBrushCrossCell` / 业务码 `terrain_cross_cell`（100070）。`Map.RelayBrush` 校验参数与 Policy（`checkBrushParams`）之后、**预扣配额与发号之前**拒绝：不扣配额、不占序号（客户端可以把同一个 `Seq` 用在下一笔上）、不转发、什么都不写。判定口径与建筑相同：覆盖的块按 `Forwarder.OwnerOf` 数出不同的格（`ownerOfAll`），多于一个即拒绝；有块没有格写仍答 `terrain_not_owned`。
- 覆盖块全归一个格时照旧：那个格是本进程就直接执行（不经网络），是远端就整份转发给它；结论、`terrain_pending`、逐块 `Outcome`、格账本判重、重发与重启的责任方同 §30.1～§30.2。
- 格一侧再查一次：转发来的笔刷覆盖的块必须都归本格写（`execBrush` 改用与普通场景相同的 `checkBrush`，事务里 `applyBrush` 以笔刷覆盖的全部块为目标组），否则答 `terrain_not_owned`（格到进程的映射在途中变了，§30.8 ⑥），不再执行其中一部分。

**删了什么（多格才用、单格路径上用不到）**

| 删除 / 收窄 | 原来做什么 | 单格路径上是否用到 |
| --- | --- | --- |
| `splitByOwner`（按 sid 分组排序）→ `ownerOfAll`（只数格、取那一个 sid） | 把覆盖块拆成每格一份 | 只用到“查每块有没有格写”，保留在 `ownerOfAll` 里 |
| `issueAndForward` 的并行转发、等齐（`sync.WaitGroup`） | 多份并行发出 | 否；改为发号后转发一次 |
| `RelayResult.Parts []RelayPart` → `RelayResult.Part RelayPart` | 多份结论 | 只用到一份 |
| `RelayResult.Err` 的多份汇总（Unknown 优先、按 sid 取第一个拒绝） | 部分成功的汇总码 | 否；改为直接按这一份的结论 |
| `RelayResult.Duplicate` 的“每一份都 Duplicate” | 多份判断 | 改为这一份是否 Duplicate |
| `RelayBrush` 按每份块的 Footprint 相加计 Unknown | 多格配额退还 | 否；Unknown 计整笔 Footprint |
| `Map.ownedPart`（覆盖块里归本进程的那几块） | 格只执行自己的那部分 | 否；单格时就是全部覆盖块，改用 `brush.Chunks` / `checkBrush` |
| 控制器 `relayChunks` 的多份循环；建筑控制器的 `len(Parts) == 1` 判断 | 多份逐块结论 | 只剩一份 |
| 用例 `TestTerrainRelaySplitsABrushAcrossCells`、`TestTerrainRelayPartialRefusalKeepsTheOtherCell`、辅助 `outcomes` / `wantOutcomes` | 断言拆分与部分成功 | 改为新契约的用例（下表） |

**保留了什么（单格也用）**：单格转发与本格直接执行（`Forwarder` / `terrainRelay`、线上格式 v1 不变）；Base 发号（`TerrainEditIssue`、`TerrainEditor`）；格账本 `TerrainCellEditor` 与判重；`terrain_pending` 与逐块 `Outcome`（协议字段不变）；格的确定结论缓存、在途去重、重发与 incarnation；Base 与格重启后的对账；配额的 Footprint 预扣与按结论多退。`PartRequest` / `PartAnswer` / `RelayPart` / handler `TerrainBrushPart` 的名字沿用（改名要动生成代码与线上格式的命名，没有行为收益）。`gameplay/terrain` 的 `Brush.ApplyIn` / `View.BrushChangesIn` 不再有调用方，作为通用能力保留（是否删除待确认，见下）；`Brush.Footprint` 仍用于 Base 预扣。

**用例**

| 用例 | 内容 |
| --- | --- |
| `TestTerrainRelayRefusesABrushAcrossCells`（新，修前红） | 只做 Base 的进程上跨两个格的笔刷：`ErrBrushCrossCell`，两格块版本与格账本为 0、没有消息发出；配额不变（冻结时钟，满桶才过得去的大笔刷照样过）；序号不前进（同一序号用在格内的笔刷上答新号、不是 Again，之后 `LastSeq` = 1）。玩家在格上时同样拒绝、本格那一部分不执行 |
| `TestTerrainRelayForwardsABrushToTheCellThatWritesIt`（原拆分用例改写） | 整份落在 102 的笔刷：转发给 102、与整图逐点相同、版本 1、102 的账本记下、101 什么都没收到；玩家在 101 上：落在邻格的一条消息，落在本格的不经网络 |
| `TestTerrainRelayRefusalWritesNothingAndRetries`（原部分拒绝用例改写） | 格拒绝（占位）：什么都没写、结论是格的原因；同一序号重发 Again、执行 |
| `TestTerrainRelayUnknownResultsAreRetriedOnce` / `DuplicatedAndLateMessagesWriteOnce` / `ReconcilesAfterRestarts` / `ChargesTheBaseByWhatWasApplied` | 原断言不变，笔刷改为整个落在一个格里（`cellBrush`）；重复与迟到改为两个格各三笔 |
| `TestTerrainRelayBuildingsStayInOneCell` | 末尾“跨格笔刷改到建筑占着的点：一格拒绝、另一格执行”改为“格内笔刷改到建筑占着的点：那个格拒绝” |
| 控制器 `TestTerrainRelayAnswerCarriesTheCellsOutcome`、`TestTerrainRefusalsMapToTheirCodes` | 一份结论的五种答复与码；`ErrBrushCrossCell` → `terrain_cross_cell` |
| 无缝演练 (a) 步 | B 上的跨格笔刷答 `terrain_cross_cell`、两块版本与摘要不变、`LastSeq` 不变；B 发起、整份落在 A 格块 (1,0)（B 的 Ghost 带里）的笔刷转发、Applied，再反向改回（记 `b_cross_cell_refused_rtt_ms` 与 `b_forward_to_a_rtt_ms`）。只编译与 vet，真实运行在合入时 |

**待确认**：`Brush.ApplyIn` / `View.BrushChangesIn`（`gameplay/terrain`，本轮未发版的新增）现在没有调用方：A（推荐）保留为通用能力，代价是多两个公开入口与一条测试；B 删除，回到 P5 之前的 `Apply` / `BrushChanges`，以后若恢复拆分再加。

## 31. 客户端岸线与重建调度（T03 / T04 算法层，2026-11-10）

分支 `feat/terrain-client-shore`，基于 main `7c5e6a90`。维护者 2026-11-10：未完成的代码先完成、验收后置。本节只做 C# 算法层（`client/dotnet/Roost.Client/Terrain/`，随 `Roost.Client.dll` 进 Unity 包，`netstandard2.1`、C# 8，不加依赖）：岸线与水面数据、块重建（顶点 / 坡度 / LOD / 岸线）、重建调度。**不做** Unity 表现（网格对象、材质、泡沫着色、线程切换）与 Unity 验收，二者后置。服务端行为不变；Go 侧只新增一份测试生成的金样。证据见 [evidence/terrain-client-shore-20261110](evidence/terrain-client-shore-20261110/README.md)，进度见 [TERRAIN-STATUS](TERRAIN-STATUS.md) T03 / T04。

### 31.1 权威水域就是一条等高线

服务端（`gameplay/terrain` `View.probe`）与 C#（`TerrainView.TryProbe`）对整数厘米点 p 的判定：地面高度 h = HeightBase + ⌊(num·10 + S²/2) / S²⌋（num 是四角原始高度的整数双线性），p 是水 ⟺ h < L，L 是 p 所在块（p 所在 1m 格的左下采样点所在块）的水位。记 Hc 为四角世界高度的连续双线性（不取整），整数点上 Hc = HeightBase + num / 1000，代入可得

> p 是水 ⟺ Hc(p) < L − 0.5。

这是恒等变形，没有误差：**权威水域 = 整数厘米点里连续双线性低于 L − 0.5 的点**。所以客户端提取的是 Hc = L − 0.5 这条等高线（`TerrainShoreBuilder`），而不是 Hc = L：

- 采样点高度是整数厘米、等高值是半整数，采样点永远不在等高线上。“端点等于水位”（等于按陆地，与服务端严格小于一致）、“整边等于水位”（没有交点）都不需要特判，交点公式不会除以零。
- 判定值 F = 2·(H − L) + 1 是奇数整数，F < 0 即水；格内用四角 F 的双线性，所有拓扑判定都是整数（鞍点用 `decimal`，防 64 位溢出），与平台浮点无关。
- 水面渲染在 L、等高线在 L − 0.5：竖直方向差 0.5cm，表现上可忽略。

### 31.2 Marching Squares 规则

| 情形 | 规则 |
| --- | --- |
| 交点 | 跨号的边上 t = F0 / (F0 − F1)，按边的**规范方向**（横边从左端、竖边从下端）在世界采样坐标里算（`TerrainShoreBuilder.Crossing`）。同一条边从相邻两格、相邻两块算出的值逐位相同，跨块的岸线端点用 `==` 对接 |
| 鞍点格（对角同号、相邻异号） | asymptotic decider：鞍点值 fs = (AD − BC) / (A + D − B − C)，分母与 A 同号，所以“鞍点是水 ⟺ (AD − BC)·sign(A) < 0”。是水：两个水角经格中心相连（岸线切掉两个陆角）；否则两个水角分开。只看四角值，与在哪一块、哪一格算无关，两端 / 两块不会得到不同拓扑 |
| 鞍点恰在等高线上（AD = BC） | 按“严格小于才是水”算鞍点不是水、水角分开；等高线是过鞍点的两条轴向直线，两段岸线都在鞍点 (us, vs) 折一次（精确，不细分）。与权威规则在鞍点上的判定一致，不随机 |
| 线性格（A − B − C + D = 0） | 等高线是直线，弦就是精确的 |
| 岸线走向 | 每段从“出水”交点到“入水”交点，**水在左手边**（俯视、X 右 Y 上）；块内按共边连成折线 `ShoreChain`，块内闭合的标 `Closed`，其余两端都在块边上 |
| 水位不同的块边 / 地图边 | 本块一侧是水的区间输出为 `ShoreSeam`（侧壁 / 瀑布 / 地图边收口用），端点与岸线端点逐位相同。区域边界线上的点（例如 x = 128m）按服务端规则属于右 / 上块；左侧格的岸线描述的是它的内部 |
| 水面 | 每格分类 `Cells`（Land / Water / Shore）；Shore 格给逆时针多边形 `Polygons`（至多两块，鞍点不连通时）；整格水不出多边形 |
| 泡沫 / 湿润带数据 | `AboveWater`：65 × 65 顶点的 H − L（厘米，负数是水深），与网格顶点一一对应、共享边逐个相同。泡沫（浅水）、湿润带（刚出水）、岸边材质渐变按它在三角形内插值；岸线走向给出外扩方向 |

### 31.3 有界细分与误差

格内精确等高线是双曲线 (u − us)(v − vs) = K 的一支。一段弦与这支曲线的最大距离在“切线平行于弦”的点取到，对这支双曲线就是 u* = us ± √((u0 − us)(u1 − us))（两端 u 的几何平均），所以每段误差是**精确算出的**、不是抽样估计。超过 `ShoreOptions.Tolerance`（缺省 1cm）就在 u* 处二分，至多 `MaxDepth`（缺省 4 层，每段至多 16 小段，上限 8）；到上限仍超差的段数记 `DepthLimited`，全块的实际最大误差记 `MaxDeviation`。细分点都在格内部，不落在格边 / 块边上，不会在公共边上制造 T 接缝。

| 误差项 | 大小与说明 |
| --- | --- |
| 岸线折线 ↔ 权威水域边界 | ≤ `MaxDeviation`（缺省参数下 ≤ 1cm）。视觉分类（`TerrainShore.ContainsWater`）与 `TerrainView.IsWater` 不同的整数点，全部在折线 `MaxDeviation` 以内（金样 9 块全图岸线格逐 1cm 核对） |
| 粗格 Marching Squares（不细分） | 圆形池塘实测 0.363cm；金样地图最大约 1cm 量级——1m 双线性格内曲率不大，细分主要用于鞍点附近与急弯 |
| 渲染网格 ↔ 双线性 | 网格每格两个平面三角形（`TerrainLod` 沿 (i, j)–(i+1, j+1) 切），格内与双线性最多差 \|A − B − C + D\| / 4（高度扭量）；水面平面与三角网的交线因此可能偏离岸线约 Δh / 坡度。用岸线数据画泡沫 / 岸边，不靠深度测试的交线当水陆边界；粗 LOD 下偏离更大 |
| 采样分辨率 | 1m 采样恢复不出未采到的细沟、细岸；视觉细分不增加权威细节，不用样条轮廓代替这条等高线 |

### 31.4 块重建（`TerrainChunkBuilder` / `TerrainChunkBuild`）

- 输出：65 × 65 顶点高度（与 `TryBuildHeightGrid` 逐个相同）、中心差分坡度 `GradientX/Y`（H(g+1) − H(g−1)，地图边按下标钳位，法线 = normalize(−gx, −gy, 200)）、LOD 三角形、岸线。输入是不可变快照（块数据只存引用），可以在任意线程 `Build`。
- 依赖：顶点读本块、右 / 上 / 右上；坡度另读左 / 下 / 左上 (63, 0) / 右下 (0, 63)；**左下块用不到**，内部块共 8 个依赖。每个依赖被读的采样矩形写在一张表里（`Dependencies` / `Dependents` 由同一张表推出，互逆）；“表外采样随便改，结果逐位不变”有测试。所以只改块内部（不碰边上两列 / 两行）时邻块不重建。
- LOD（`TerrainLod`）：级别 0～5（步长 1～32），块边上的顶点步长 = max(本块, 共边邻块)，邻块没有网格时用本块步长；两块对公共边算出同一组顶点，没有 T 接缝。内部是规则四边形，边框四个梯形用两条平行链交替连三角形。三角形俯视逆时针，Unity 按其约定换序。
- 接缝一致（`SeamMatches`）：公共边 / 公共角上的顶点高度与坡度逐个相同，公共边两侧 LOD 边步长相同，彼此记录的对方水位等于对方实际水位。按**输出**比较，而不是比较输入版本：只改块内部时旧邻居仍然接得上。

### 31.5 重建调度（`TerrainRebuildScheduler`）

| 输入 | 效果 |
| --- | --- |
| 视图 `ChunkChanged` | 按前后数据的变化矩形算出读到它的块（`Dependents`，含法线的一圈），只标这些块脏；`Unloaded` 同时撤销这块的确认 |
| `Want(key, lod)` / `Drop(key)` | 显示集合与 LOD；LOD 变化与增删让四个共边邻块重建接缝；`Drop` 在下一批 `Retired` 里撤下 |
| `Confirm(key)` | 这块在本会话里的覆盖块状态已确认。**基础包可读不等于覆盖块已确认**：缺省（`RequireConfirmation`）依赖块都确认后才派发、才发布；卸下即撤销 |
| `BeginSession()` | 断线重连 / 重新登录：会话代际加一，确认全部作废，全部块变脏 |
| `SwitchTerrain(view)` | 基础包换版（水位随包）：地形代际加一，改用新视图，其余同上 |

`Pump()`（Unity 里在主线程每帧调）依次：收回执行器完成的任务 → 校验 → 选出可以一起发布的结果 → 派发新任务；`Published` / `Error` 在锁外触发。发布前逐项校验，任一不过就丢弃并重新排队：

1. 结果来自这一块**最新派发**的任务（乱序完成的旧任务丢弃）；
2. 会话代际、地形代际与当前相同；块仍在显示集合里，本块与四邻的 LOD 与派发时相同；
3. 每个依赖块现在装着、（需要时）已确认，且它**被读到的那片采样**与快照相同（引用相同或内容相同）——只比较当前块 Version 不够，邻块的边界变了同样作废。

通过校验的结果先暂存，再取最大的一致集合：其中每块与 8 邻域里将要显示的结果（同批新结果，否则是正在显示的旧结果）`SeamMatches`；对不上的继续暂存，等邻居的新结果一起发布。期间渲染侧继续显示上一组一致的结果，`TryGetShown` 报告它已过期（`current = false`），不能当作当前权威数据。相邻块从没显示过时不构成约束。

有界：每块至多一个待重建标记（去重），待重建 ≤ 显示集合 ≤ `MaxChunks`（缺省 1024，`Want` 超出抛异常）；交给执行器、未收回的任务 ≤ `MaxInFlight`（缺省 4；被取代的旧任务收回前也占名额，变脏时标记取消，执行器可以跳过），所以执行器积压同样有界；暂存 ≤ 显示集合。执行器是接口 `ITerrainBuildExecutor`（`ThreadPoolTerrainBuildExecutor`；测试用手动推进的队列，用例确定、不靠 sleep）。

限制：某块的邻居长期不就绪（依赖一直没装 / 没确认）时，它与该邻居对不上的新结果会一直暂存——显示集合应比装入集合内收一圈（坡度需要 8 邻块），或 `Drop` 那个邻居。变化比一次重建还快地持续到来时，旧结果全部丢弃、要等变化停下才发布；地形块 1 秒合并下发（[按类型调度](TERRAIN-DECISIONS-20261109.md)）下一般不会出现，但不给硬延迟上界。不同格独立提交是最终一致，批量发布不制造跨格原子性。

### 31.6 验证

`dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec`（新增 `ShoreTests.cs`、`LodTests.cs`、`RebuildTests.cs`）：

| 用例 | 内容 |
| --- | --- |
| 判定规则 | 等于水位的采样是陆地、交点离它 1/20 格；整边 / 整块等于水位无交点；鞍点连通（一块多边形、一条闭合岸线）、不连通（两块、两条）、恰好相等（两块、两段都在鞍点折）且与位置无关；线性格误差 0；整块是水时地图边 / 不同水位邻块出侧壁、同水位邻块不出；无 NaN、多边形都逆时针 |
| 圆形池塘（任意整数厘米高度） | 一条闭合逆时针岸线；精确等高线上 26,934 个点到折线都 ≤ `MaxDeviation`；1cm 格点上视觉与权威不同的 150 个点都在误差带内；粗 / 细 / 层数受限三组参数如实报告误差 |
| Go 金样 `client/spec/terrain-shore.pack` + `terrain-shore.json`（`gameplay/terrain` `TestClientShoreGolden` 生成） | 3 × 3 块：圆心在四块交点附近的池塘、横跨两块公共边的池塘、跨区域水位边界（0 / +50cm）与跨无水区域边界的水坑、鞍点区、等于水位的平台。7 个窗口共 70,000 个点 C# 权威查询与 Go 逐点相同，视觉分类只在误差带内不同（70 个）；全图岸线格逐 1cm、其余每 13cm 共约 573 万点同样；开放岸线的 12 个端点在两块之间逐位相接、6 个落在侧壁端点上；四块交点池塘四块都有岸线 |
| LOD | 6 × 7 种级别组合都恰好铺满整块（面积 4096、三角形不退化）、公共边顶点集合相同、无 T 接缝 |
| 块重建 | 顶点等于高度网格；相邻块接缝一致；依赖 / 被依赖互逆；声明读取范围外的 20,155 个采样改掉后结果逐位不变；改公共边后旧邻居对不上、新邻居对得上；LOD 0 \| 2 缝合 |
| 调度 | 邻块晚到与覆盖块未确认（不派发、不把原始块当已确认数据）；公共边编辑四块一起重建，前三块完成时都不发布、旧网格报过期，第四块完成后一批发布；只改内部单块发布；乱序（先算完的旧结果、倒序完成的被取代任务）不发布；卸下后重装原始块，未确认不重建；重连前已算完的任务（数据未变）也不发布；LOD 变化带四邻重建；`Drop` 撤下；只换水位的新包（高度逐点相同）换版前已算完的结果不发布、全部按新水位重建；随机 4000 步（编辑 / LOD / 显示集合 / 卸载重装 / 重连 / 乱序执行）里执行器积压 ≤ 2、待重建 ≤ 显示集合，122 次发布每次都满足“与当前数据一致、属于当前代际、所有正在显示的相邻块接缝一致”，最后全部稳定为与直接重建相同的结果 |

变异（逐项改坏实现、跑同一组测试、还原）：鞍点总连通、相等算水、等于水位算水、侧壁交点不走规范函数、不细分、误差在参数中点量、忽略邻块水位、LOD 边用本块步长、读取表漏右下、发布不查接缝、新鲜度不比数据、会话 / 地形代际不查、不要求确认、卸下不撤销确认、不限在途、待重建不去重、变化矩形不用、LOD 变化不带邻块、地图边坡度改单侧差分都变红；失败原文见证据。**没变红的（防御重叠，如实记录）**：只去掉 Collect 里的会话（或地形）代际检查——发布前的新鲜度校验同样检查代际；去掉“最新派发”检查——被取代的任务要么已被取消（无结果），要么读的数据已过期被新鲜度拦下；数据回到原样时旧结果与当前数据一致，发布它不是“旧结果”。

### 31.7 实施方先定的取舍（维护者 2026-11-10 确认：全部按推荐★；③ 取 A，服务端给就绪标记 / 改过块清单，另行实施）

| 项 | 现状（推荐★） | 选项与代价 |
| --- | --- | --- |
| ① 等高值 | ★ L − 0.5（与权威水域逐点一致，§31.1） | 或 L：视觉边界整体向水里偏约 0.5cm 高度对应的距离，等于水位的采样要特判 |
| ② 鞍点恰在等高线上 | ★ 不连通（与严格小于一致） | 或连通：视觉上两片水相连，与“鞍点不是水”的权威判定相反 |
| ③ “覆盖块已确认”从哪来 | ★ SDK 提供 `Confirm`，缺省要求确认；接入层决定何时调用（维护者 11-10 定 A，已实施，见 [§31.9](#319-确认来源就绪标记317-③-a2026-11-10)） | **服务端目前没有“这块没改过”的信号**（只有改过的块是同步对象），客户端分不清“没改过”与“全量还没到”。A（推荐）服务端在地形 Interest 订阅时给每块一个就绪标记或改过块的清单（要改服务端，本轮未做）；B 客户端在订阅后首个全量帧应用后把视距内没有实体的块当作已确认（需要核实首帧是否包含视距内全部已改块，并处理移动中新进入的块）；C `RequireConfirmation = false`，先显示原始块、全量到达后跳变（D12 的显示代价） |
| ④ 发布一致性的判据 | ★ 比较公共边上的输出（高度、坡度、边步长、对方水位） | 或比较整组依赖版本：实现更简单，但只改块内部也要带邻块一起重建、一起发布 |
| ⑤ 坡度在地图边 | ★ 下标钳位、跨度仍记 2 格（边上坡度减半，两块一致） | 或单侧差分按 1 格：边上法线更准，公式要分情况 |
| ⑥ 细分缺省 | ★ 1cm / 4 层（金样最大误差约 1cm、无超限段） | 或更细 / 更粗：点数与误差的取舍，可按设备档位配置 |
| ⑦ 泡沫数据 | ★ 顶点的 H − L（`AboveWater`），只依赖本块顶点 | 或到岸线的水平距离场：泡沫宽度更均匀，但要读邻块的岸线，依赖再扩一圈 |

服务端水域规则与客户端需要的规则**没有发现不一致**：区域边界线上的点属于右 / 上块、等于水位不算水、无水区域 `MinInt32`，客户端都按 Go 的规则实现；不需要改服务端。

### 31.8 未覆盖（后置）

- Unity：网格对象与上传、材质 / 泡沫 / 湿润带着色、水面侧壁、主线程与 Job 的切换，以及 Editor、Mono / IL2CPP、移动设备上的耗时与内存。本机 .NET（Apple M5，`net10.0` 测试程序）以外没有运行过；没有性能基准（一次块重建的耗时只在 Debug 测试里粗看过，不作为结论）。
- 确认来源（§31.7 ③）已在 §31.9 实施：服务端信号走真实 entitysync 链路（Go TCP 客户端核对），C# 接入层用构造的说法核对时序，没有用真实帧驱动 C# 的 `EntityViews`；编辑者本地预测预览与已确认地形分层（本轮 SDK 没有预览层，预览由业务在 `TerrainView` 之外叠加）；远端 1 秒合并后的渐变表现。
- 粗 LOD 下岸线（全分辨率）与粗网格的视觉偏离；跨块岸线的连续 UV / 弧长（每块只有本块折线）；盆地水位 / 灌水（§5 后续可选）不在范围。
- 填土分裂池塘、挖通两片水：岸线按当前高度重新生成，拓扑自然正确（不保留旧水面），已由“任意高度场上逐点一致”覆盖，没有专门用例。

### 31.9 确认来源：就绪标记（§31.7 ③ A，2026-11-10）

分支 `feat/terrain-confirm-source`，基于 main `66aecd92`，合入 `70623709`（同步视图 / codec 校验）。维护者 2026-11-10 定 A：服务端在地形 Interest 订阅时给客户端一个可据以确认的信号，取最简单、与 entitysync 一致的形状。证据见 [evidence/terrain-confirm-source-20261110](evidence/terrain-confirm-source-20261110/README.md)。

**形状：地形视野里每一块都有一个 TerrainChunk 对象。** 改过的块照旧是实体（或分区场景里别的格写的块的副本 `Replica`）；没改过的块是**就绪标记** `terrain_chunk.Base`：同一个实体 ID、命名空间 `terrain`、kind、同步编码（`TerrainChunkDao` 的同步 PB），全量只有 `BaseRevision` 与 `Version 0`，不带高度与材质，从不变脏。客户端的规则因此只有一条：**这块的对象在本会话里、`BaseRevision` 等于本地原始地形包，就是已确认**；Version 0 用本地原始块，≥ 1 用对象里的覆盖块。没有新协议、新消息或新的实体类型；进出视野、视野上限、超额降级、快照预算、重连全量都由现有的地形 Interest 与 Manager 负责。

| 时序 | 服务端 | 客户端（`TerrainChunkSync`） |
| --- | --- | --- |
| 启动 | `startTerrainSync`：改过的块装成实体登记（`loadEdited`），本进程写的其余块（视图里 Version 0）各登记一个标记（`showBases`），都放在块中心 | — |
| 进场 | Interest 按块中心 ≤ 200m 订阅：普通场景约 29～37 个对象，每会话每 Flush 4 个全量（标记约 20 字节），8～10 次 Flush（实测 29 块 8 次，约 0.4s） | `Apply`：标记 → 装着原始块就 `Confirm`，没装就等业务 `LoadBase` 之后（`ChunkChanged` Loaded）再确认；实体 → `SetOverlay` 后 `Confirm` |
| 第一次改一块 | 提交后回调里关掉标记、以实体（副本）状态 `Register` 同一 ID：Manager 见旧状态已关闭，把订阅接到新状态上并强制全量（rebind）。**中间没有 remove** | 同一个对象收到实体全量，`Apply` 照常，确认不中断 |
| 移动中新进入视距 | 进入视野的块（标记或实体）发全量，离开离开半径（220m）的发 remove | `Apply` 确认；`Remove` → `Unconfirm`，视图数据保留 |
| 重连 / 重新登录 | 新会话从全量开始，视野里每一块重发（标记没变也重发） | `BeginSession`：说法全部作废、调度器会话代际加一；之后逐块 `Apply` 重新确认 |
| 换原始地形包 | 标记与实体都带新 `BaseRevision`（D13 由重启完成，客户端走重连） | 新包上的说法先到时 `Apply` 抛 `TerrainBaseRevisionException` 但留着说法；`SwitchTerrain(next)` 后用留着的说法装入并确认，旧包上的说法不确认 |
| 业务按视距卸载、重装 | — | Unload 撤销确认；`LoadBase` 之后按留着的说法补回覆盖块（Version ≥ 1）或重新确认（Version 0） |
| 分区场景 | 本进程写的块同上；别的格写的块：客户端想要、本进程还没有任何说法的块订阅时 `Held` 报 −1（Replicator 协议本来的“请发当前状态，含原始块”），拥有者答 Version 0 后登记标记、答覆盖块建副本，之后第一次改时标记转成副本（同样 rebind）；Ghost 带里的块按视图登记；离开全部玩家的视点范围撤下 | 与普通场景相同 |

**为什么不是改过块的清单。** 备选（都考虑过、没有采用）：

| 备选 | 不采用的原因 |
| --- | --- |
| 场景对象上一个字段带全图“改过的块”位图 / 清单（2048m 图 128 字节） | 客户端还要知道服务端视野的边界（块中心半径、离开滞回、视点每 8m 上报、`MaxVisible` 与超额降级）才能断定“清单里有、对象还没到”与“不在视野里”，等于在客户端重做一遍 AOI；分区场景的 Base 进程不知道全图哪些块改过，要另加一份跨进程协议；位图与块对象分属两条调度（on_change 与 periodic 1s），先后不定，客户端要处理“清单说改过、对象还没到”的空档 |
| 每个玩家一份“视野里改过的块”清单（新消息） | 同样要与 Interest 的进出、上限对齐，是第二个订阅来源；重连、换包都要单独处理 |
| 标记转实体时先 remove 再 create（`RegisterAfterRetirement`） | 客户端在两帧之间撤销确认，依赖它的块一次重建暂停；rebind 是 Manager 现成的“关闭的旧状态接到新状态并强制全量”路径，不用改框架 |
| 标记复用 `Replica`（Version 0 时不分配数据） | `Replica` 每块分配约 76KB 的数据与序号表；标记只要一个 `SubjectSyncState`，2048m 图 1024 块全部登记约几百 KB |

**代价与边界。**

- 每会话地形对象从“视野里改过的块”变为“视野里全部块”：普通场景约 29～37 个（上限 64 不变，视野 200m 时不会触发超额降级）；进场多 28～36 个约 20 字节的标记全量，按地形快照预算分 8～10 次 Flush。玩家 / 怪 / 建筑的名额与帧不受影响（地形单独计，D15）。
- 显示半径：一块就绪要 8 个邻块都已确认，服务端按块中心离视点 ≤ 200m 下发、视点每 8m 上报，所以 `Want` 的显示半径约为 200 − 8 − 90.5（一块的对角线）≈ 100m。要更远就放大地形视野（300m 约 69 块，超过 64 的上限，要一起改 `terrainMaxVisible` 与会话上限）——**待维护者确认**是否需要。
- 原始地形包里坏了的块（服务端装不进视图）不登记标记，客户端永远不确认它（服务端自己也查不了）；分区场景里拥有者装不进的块同样不答，订阅每秒续期都会再问一次（计 Replicator 的 `unloaded`）。
- 第一次改时以实体状态 `Register` 失败（Manager 已关闭、状态已归别的 Manager）：退役这个 ID，订阅者收到 remove、撤销确认，不留一个停在 Version 0 的标记把改过的块当成没改过；记 Error。
- 客户端：覆盖块只经 `TerrainChunkSync` 写进视图；与 `EntityViews` 接线时，地形块的视图键是缺省视图（`""`），不受 player / monster / building 的 `near` / `team` 视图注册影响。C# 只用构造的说法核对时序，没有用真实 entitysync 帧驱动；Unity 未跑。

**验证**（[证据](evidence/terrain-confirm-source-20261110/README.md)）：生成的 game-demo 里 `TestTerrainEveryChunkInViewReachesTheClient`（真实 TCP、生产启动步骤 `startTerrainSync`：进场 29 块齐、第一次改没有 remove 且是全量、移动后新进入的都到 / 220m 外的被 remove、重连后 29 块齐）与 `TestTerrainPartitionEveryChunkInViewReachesTheClient`（两个格进程 + 只做 Base 的进程：两个客户端都拿到视野里 11 块的标记，邻格第一次改后标记换成副本、没有 remove）修前都红；`TestChunkSyncConfirm`（C#）覆盖进场缺一块、标记转实体、离开 / 再进入不重建、移动中新进入（LoadBase 前后）、卸载重装、缓存的旧覆盖块被标记换回、重连、坏记录、换包。Go 5 项、C# 8 项变异全部变红。
