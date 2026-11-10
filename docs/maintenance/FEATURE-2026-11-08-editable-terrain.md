# 可编辑地形大世界（定稿方案，2026-11-08）

当前完成、未完成与待验收清单统一见 [地形进度总表](TERRAIN-STATUS.md)。

**2026-11-09 更新**：编辑配额 270 点/秒与 terrainpack 默认高度基准 −100 米已落地；同步按类型调度的框架机制及 game-demo 生产接线已实现；客户端圆滑岸线已确认、尚未实现，见 [后续决策及实施状态](TERRAIN-DECISIONS-20261109.md)。P3 移动与寻路（坡度 / 台阶 / 下落 / 游泳、高度场 A*、demo 普通场景接入）在本地分支 `feat/terrain-p3-move` 实施，未合入 main，见[第 22 节](#22-p3-移动与寻路实施结果2026-11-09)。下文 §11～19 的测量与暂停记录保留为历史证据，不能当作当前状态。

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
| DAO | `db/def/terrain_chunk.go` | 按 §3：`BaseRevision` / `Version` / `Heights []uint16` / `Materials []byte` / `Applied []uint64`，store 键 `1`～`5`，`coll=terrain_chunks db=main`。前四个 `persist,sync`，`Applied` 只 `persist`：同步视图的字段资格在 DAO 上一次定下，P2 接 entitysync 时 `Applied` 自然不下发 |
| 实体 | `game/entities/terrain_chunk` | kind 11（不复用已删的 5 / 8），`EntityCategoryWorld`（与 Scene 同档），持久；实体唯一号 = `ChunkKey.ID() + 1`（unique 0 无效，块 (0,0) 的 `ID()` 正是 0；地图每边 ≤ 1024 块，`ID()` < 2^42，远小于 52 位 unique）。ID 确定、可能已有记录，所以只经 lifecycle 的 `GetOrCreate` 建，不直接 `Create` |
| 组件 | `SurfaceComponent`（type 2011） | `Apply(baseRevision, base, edits)`：Version 0 时用原始块填满 DAO 再改，之后要求记录的 `BaseRevision` 等于当前包；高度只写变了的下标（`SetHeightsAt`），材质有变化时整字段（`SetMaterialsAll`）；每次成功 Version + 1；全部校验在第一次写之前。`Overlay()` / `OverlayOf(dao)` 给视图的副本；`RecordApplied` / `WasApplied` 是 `Applied` 的辅助（满 32 个丢最旧，整字段替换） |
| 地形服务 | `game/terrainmap`（`Map`） | 持有 `*terrain.Pack`、`*terrain.View`、本进程范围 `Region{Own, Load}`；`Load` 装块、`Edit` 写入口、`ApplyEdit` 是 handler 的业务、`Height` / `Material` / `IsWater` / `Probe` 委托视图 |
| 改块事务 | handler `TerrainEdit`（`rollback=undo durability=strict`） | 一块一笔；`ApplyEdit` 改 DAO 后 `nest.AfterCommit` 登记 `SetOverlay`（新数据的副本），提交被接受才进视图，回滚 / 提交被拒时不执行。handler 是包级函数，经 `terrainmap.Active()` 拿到本进程的 Map（与 `partition.Node()` 同一做法） |
| 服务接线 | `internal/service/<game>/terrain.go` | `Init` 里（World、Scene 之后）`startTerrain`：读配置 → 打开包 → 算范围 → `Load` → `Install` → 发布能力 `<game>.terrain`；`Shutdown` 最后 `Close`（撤下发布、关包） |
| 配置 | `game/settings/terrain.go` | `scene.terrain.pack`（可选，空 = 没有地形）；分区场景开着时 `Scene.ValidateConfig` 要求 `cell_size` 是 6400 的整数倍（加载配置时拒绝，doctor / `--check-config` 同一份） |
| 原始地形包 | `cmd/terrainpack` | `terrain.Generated` 写确定性小地图，缺省 2 × 1 块（128m × 64m，`BaseRevision` 1、水位线 400m），覆盖普通场景（1000 × 1000）与两个 6400 格的分区场景 |

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

本地分支 `feat/terrain-p3-move`（基于 main `93c00538`，已合并 main `3fb089d9`（T01/T02），未推送、未合入）。接续 §19 记录的 WIP 提交（已改写为本节的四笔提交，原 WIP 不保留）：审查后保留其行走规则与 `movement.Surface` 接入，改为 int64 高差、加负高度与加权寻路，补齐 demo 接入、端到端与纠偏验证。完成判据对照 [进度总表 T06](TERRAIN-STATUS.md)；证据与复跑命令见 [证据目录](evidence/terrain-p3-move-20261109/README.md)。

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
- **减速**（游泳 60%）：预算 = 这一步**未被世界边缘夹过**的位移的 max(|dx|, |dy|) × 1000，每点花 ⌈10⁶ / 速度千分比⌉；用完停下、速度不变。世界边缘的速度规则只在走到终点时生效（减速后没到边缘不反弹）。度量是 Chebyshev：不夹边时与按欧氏距离折算完全相同（同一条直线上的比例），夹边时是近似。
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

## 26. 开地形的分区场景真实多进程演练（T05、T06，2026-11-10）

本地分支 `feat/terrain-partition-drill`（基于 main `960e485c`，未推送）。补 §21 与 §24.4 留下的“开地形的无缝演练、真实 Mongo / WAL 崩溃演练、`startPartition` 装钩子的几行”。§25 留给同期 P4 那一轮。证据、命令与每步数字见 [evidence/terrain-partition-drill-20261110](evidence/terrain-partition-drill-20261110/README.md)。

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

**待维护者确认**（实施方先按推荐做了）：① `gm.terrain.probe` 留作 demo 的运维 / 诊断命令（★）还是只给演练用；② 演练用平地包（★），还是给 `cmd/terrainpack` 加平地 / 平缓选项、或改缓 `terrain.Generated`（包字节变化须换 BaseRevision）；③ 演练保持手动（★）还是进 pretag（多约 2 分钟、要求真实依赖）。
