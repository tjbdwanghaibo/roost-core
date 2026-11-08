# 可编辑地形大世界（定稿方案，2026-11-08）

状态：**定稿方案；P1 已实施：框架部分（`gameplay/terrain`：坐标、原始地形包、地形视图与查询）见[第 11 节](#11-p1-框架部分实施结果2026-11-08)，实体接入（game-demo 的 `TerrainChunk` 实体、地形服务、改块重启后一致）见[第 12 节](#12-p1-实体接入实施结果2026-11-08)；P2（单格）已实施：C# 客户端部分（读包、查询、接 DAO 同步、mesh 钩子）见[第 13 节](#13-p2-客户端c实施结果2026-11-08)，服务端部分（笔刷、校验、幂等、多块事务、整字段守卫、地形同步，真实 TCP 实测）见[第 14 节](#14-p2-服务端单格实施结果2026-11-08)；P3 起未实施。** 维护者 2026-11-08 确认：材质层用 `[]byte`，其余按本文推荐整理为最终方案（第 8 节）。目标是在 cellworld 无缝大世界上支持玩家改地形（挖、填、抬高、整平、改材质）、由此形成湖泊和高地，并在地形上放置建筑。

数字的来源分两种：同步与持久化的单块字节、编码耗时来自离线微基准（[实验记录](evidence/terrain-sync-20261108/results.md)）；规模、带宽与参数初值是按假设推算，P2 / P5 实测后修正。

## 1. 现状（main v1.42.2）

| 部分 | 现状 | 位置 |
| --- | --- | --- |
| 大世界坐标 | cellworld 的 `EntityState` 只有 `X, Y`（int64）与速度，没有高度 | `gameplay/cellworld/wire.go` |
| 地图 | `spatial.GridTerrain`：二维稀疏阻挡格，每格只有“挡 / 不挡”；边界建时固定，只在内存，不持久化；没有高度、水、材质 | `infra/base/spatial/terrain.go`，demo `game/scene/runtime/terrain.go.tmpl` |
| 移动 | 逐格推进，每进一格问 `InBounds` / `Blocked` | `gameplay/movement/move.go` |
| 寻路 | 格子 A*，单次最多 4096 个节点；分区模式下每个格进程整张图加载，跨格寻路只算到本格边界 | demo `pathfind.go.tmpl`，[无缝大世界方案](FEATURE-2026-11-03-seamless-world.md) 管理器分类表 |
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
//roost:dao coll=terrain_chunks db=game
type TerrainChunkDao struct {
	BaseRevision int64    `store:"1"` // 基于哪一版原始地形
	Version      int64    `store:"2"` // 每次提交加一
	Heights      []uint16 `store:"3"` // 4096 点，同步与持久化按下标增量
	Materials    []byte   `store:"4"` // 4096 点，整字段替换
	Applied      []uint64 `store:"5"` // 最近 N 个已应用的编辑 ID，有界，不下发客户端
}
```

| 字段 | 写法与代价（单块，实测） |
| --- | --- |
| `Heights` | 小笔刷逐点 setter：同步每个改动点约 8 字节，持久化按下标 `$set` 每点约 12 字节；单次改动超过 1000 点时业务改用 `SetHeightsAll` 整字段（同步约 8.2KB、持久化约 40KB）。BSON 中是 int32 数组，整文档约 44KB |
| `Materials` | 整字段替换：同步与持久化每次约 4.1KB，不论改几个点 |
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

每个操作带玩家请求 ID，块内 `Applied` 环判重（幂等）。

**链路**：客户端 → 玩家所在的 Base 进程（鉴权、领地 / 权限、频率）→ 按落点转发给目标格的 Real 所在进程（与 cellworld 跨格攻击“转发给目标 Real 结算”同一模式）→ Nest 多实体请求锁住受影响的块 → 校验 → 写 DAO → 提交；提交并持久确认后由 entitysync 下发（第 7 节）。

**服务端校验**：

- 玩家笔刷半径上限 16m（约 800 点，低于整字段守卫阈值）；整平、地基等大范围操作走守卫；
- 单次最多影响 4 块；
- 单次最大高度增量、整平目标的范围；不能低于基岩高度、不能高于上限；
- 不能改有建筑占位的点；
- 每玩家编辑频率（初值每秒 2 次）；
- 领地 / 权限规则由业务提供钩子。

**整字段守卫**：一次编辑在某块内改动的高度点数超过 1000 时，对该块用 `SetHeightsAll` 写整字段，不逐点写。原因：逐点补丁每点约 8 字节，约 1020 点以上比整字段大，阈值附近最多是整字段的 4 倍（[实验 §1](evidence/terrain-sync-20261108/results.md)）。

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
- **对象上限。** `frame.Limits.MaxObjects` 缺省 100，同时是一个会话可持有的 subject 数；超出时 Manager 关闭整个会话（`object_limit`），不截断。地形块与玩家、NPC、建筑共用这个上限：地形 Interest 的 `AOIConfig.MaxVisible` 设 64，`MaxObjects` 调到“其他可见实体上限 + 64”。
- **基线限流。** 改过的块进入视野时发全量（约 12.3KB：高度 8.2KB + 材质 4.1KB）。开启 `ManagerConfig.SnapshotBudget`，每会话每次 Flush 的全量对象数初值 4（50ms 一轮时峰值约 1MB/s，50 块约 0.65s 发完），P2 实测后调整；增量与 remove 不受限。
- **增量。** 高度逐点补丁每点约 8 字节（r=5m 约 0.67KB）；大编辑走整字段守卫（8.2KB）；材质每次整字段 4.1KB。变化树每 DAO 最多 4096 个节点，4096 点的块在约 4093～4094 个不同下标时自动整字段回退；单块补丁最坏约 32.6KB，低于单组件上限 `MaxComponentBytes` 64KB。
- **继承的交付契约。** 持久化门槛（按 `CommitLSN` 挡整个 subject，编辑落 WAL 前不下发）、版本顺序与断档恢复（EntitySync 外层负责）、断线重连 / Hold-Ready 的新 epoch 全量都直接继承。
- **客户端。** Go 用生成的 `ApplySync<Dao>`，C# / C++ 用同一份 `dao_sync.proto` 与生成 Apply；应用后按 `Version` 重建该块 mesh。
- **没有客户端缓存。** entitysync 不记客户端已有哪个版本：块离开视野再回来会重发全量，版本没变也一样（AOI 的进出半径不同，只防边界抖动）。首版接受，按第 7.1 节的量评估；超出预算再做“客户端已有此版本则跳过全量”（第 8 节 D12）。

**格边界**：邻格需要 Ghost 带（按 500m 配置即 `ghost_band` 50000；demo 缺省的 500 只有 5m）内的地形与建筑占位副本，用于视野、移动和交接判断。格进程之间像 Ghost 一样推送边界块的版本与变化，副本只读。

### 7.1 同步数据量（推算）

单块下发量（实测，DAO 组件 payload，不含帧头与 TCP/IP 头；框架没有传输压缩）：

| 内容 | 字节 |
| --- | ---: |
| 高度增量 r=5m（81 点） | 约 0.67KB |
| 材质改动（整字段） | 约 4.1KB |
| 改过的块进入视野（全量） | 约 12.3KB |

一次编辑（r=5m，平均影响 1.3 块）给每个观察者：只挖填约 0.9KB；挖填同时改材质约 6.2KB；只改材质约 5.3KB。

| 场景（假设） | 下行 |
| --- | --- |
| 建造热点的一个观察者：附近 20 人各每秒编辑 1 次 | 只改高度约 17KB/s；一半带材质约 70KB/s；全部带材质约 124KB/s |
| 跑图（6m/s，每约 10.7s 进入一排 7～8 块） | 沿途全改过约 8.6KB/s，骑乘 15m/s 约 21KB/s；沿途改过 20% 约 1.7KB/s |
| 登录 / 传送 / 重连到全改过的区域 | 约 31 块 × 12.3KB ≈ 380KB（P2 更正，原按 50 块算约 600KB），由快照预算分摊（每 Flush 4 个，约 0.4s 发完） |
| 全服出口：3000 人在线，10% 每秒编辑 1 次、平均 10 个观察者；一半在跑图，沿途 30% 改过 | 编辑增量 2.6～10.6MB/s + 跑图加载约 3.9MB/s，合计约 6.5～14.5MB/s；按 16 个格进程平摊每个不到 1MB/s，热点集中在少数格 |

读法：高度增量不是瓶颈；材质用 `[]byte` 时，热点下行取决于改材质的比例；改得多的区域，跑图加载比编辑本身更费流量。P2 用真实 TCP 实测热点与跑图两档，超过客户端下行预算时按 D11 / D12 复议。**P2 实测**（[证据](evidence/terrain-p2-20261108/README.md)，第 14 节）：热点 19.2 / 75.5 / 132.6 KB/s（线上字节，含帧头；DAO payload 15.8 / 71.9 / 128.9），跑图 6m/s 全改过 8.1KB/s、改过 20% 1.6KB/s，12m/s 全改过 16.6KB/s，与上表的推算相差在 15% 以内。

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
| D9 | 寻路 | 高度场上的格子 A*（坡度代价、按块版本失效） | 动态 navmesh（重建代价高） |
| D10 | 地形物理碰撞 | 首版不做 | PhysX 高度场 + 每格物理世界（P6） |
| D11 | 材质字段形状 | `[]byte` 整字段（维护者 2026-11-08 决定） | `[]uint16` 列表按下标增量（r=5m 约 0.6KB，推算）；P2 实测挖填多数带材质、热点下行超预算时换（未上线，换类型不需要兼容旧数据） |
| D12 | 客户端版本缓存 | 首版不做，离开再进入视野重发全量 | entitysync 支持“客户端已有此版本则跳过全量”；跑图加载流量超预算时做（框架功能） |
| D13 | 原始地形换版后的旧编辑 | 丢弃（维护者 2026-11-08）：格进程启动时，本格基于旧 `BaseRevision` 的记录经正式实体 / DataEngine 路径处理后改用新原始块，计数并记日志；处理失败时这块不装入、拒绝编辑，绝不用原始块覆盖旧记录；邻格 Ghost 带里的旧记录只由它的格处理（实现见第 14 节） | 按差值重放到新原始块（要保留编辑日志或旧包）；保留旧记录覆盖新原始块（地形与新美术不衔接） |
| D14 | Ghost 带地形副本的更新 | P3 把邻格的编辑推送到 Ghost 副本（维护者 2026-11-08 定为后续工作）；P3 之前 Ghost 副本只是启动时的快照，依赖邻格地形的判断（例如跨格移动的高度）可能读到旧数据 | — |

### 8.1 首版参数初值（P2 实测后修正）

| 参数 | 初值 |
| --- | --- |
| 块 / 采样 / 格 | 64m / 1m / `cell_size` 307200（48 块），`ghost_band` 50000（500m）。P1 更正：世界单位是厘米（cellworld 约定），原写 3072 / 500 是按米算的，块边长 6400 |
| 高度 | `uint16`，10cm，相对地图基准 |
| 整字段守卫阈值 | 单块 1000 点 |
| 玩家笔刷半径上限 / 单次最多块数 / 频率 | 16m / 4 块 / 每秒 2 次（P2：令牌桶，容量 2；参数合法的请求才占频率） |
| 玩家单次最大高度增量 / 高度范围（P2 新增） | 强度 ≤ 50（5m）；基岩 0、上限 16383（1638.3m；原始值不超过 16383 时 varint 2 字节，增量每点仍约 8 字节） |
| 地形视野半径 / `MaxVisible` | 200m（滞回 20m，视点每移动 8m 更新一次）/ 64 块；200m 实际约覆盖 31 块（第 7 节 P2 更正） |
| `MaxObjects` | 其他可见实体上限 + 64（demo：100 + 64 = 164，只在开地形的普通场景） |
| `SnapshotBudget` 每会话每 Flush 全量对象数 | 4（Manager 级，对玩家、怪也生效；视野满 64 块 + 40 个其他对象时 28 次 Flush 发完） |
| `Applied` 环长 | 32 |

## 9. 分期与验收

| 期 | 内容 | 验收 |
| --- | --- | --- |
| P1 地形底座 | 原始地形资源格式与加载；`TerrainChunk` 实体、持久化与“原始 + 覆盖”加载；`Height(x, y)` / `Material` / `IsWater` 查询（单进程） | 1w × 1w 原始资源加载与按需装块；改块重启后一致；查询与客户端同算法结果一致（实施结果：[§11](#11-p1-框架部分实施结果2026-11-08)、[§12](#12-p1-实体接入实施结果2026-11-08)；C# / C++ 客户端同算法留 P2） |
| P2 编辑与同步（单格） | 笔刷操作、校验、幂等、多块事务、整字段守卫；地形 Interest、`MaxObjects` / `MaxVisible`、`SnapshotBudget`；C# 客户端 Apply 与重建 mesh | 重复 / 乱序请求不重复生效；断线重连后客户端与服务端块版本、数据一致；视野内改过的块达到 `MaxVisible` 时会话不因 `object_limit` 断开；真实 TCP 下 r=5m 单块增量约 0.7KB、材质约 4.1KB；第 7.1 节热点与跑图两档的下行实测，并据此复议 D11 / D12；单格编辑吞吐与 Nest 短池耗时实测（实施结果：C# 客户端 [§13](#13-p2-客户端c实施结果2026-11-08)、服务端 [§14](#14-p2-服务端单格实施结果2026-11-08)；C# 的笔刷同算与 C++ 客户端未做） |
| P3 接入 cellworld | 块与格对齐、边界块副本推送；移动加入高度查询与坡度 / 台阶上限、游泳；寻路改为高度场 A* | 跨格移动、交接与边界视野在改地形后正确；寻路随块版本失效；现有 cellworld 回归不退化 |
| P4 水与建筑 | 水位线规则；`Building` 实体、放置校验、地基整平、占位推导 | 挖坑成湖、填平成陆的规则可复现；建筑占位重启后正确；不在水下 / 坡上违规放置 |
| P5 跨格与规模 | 跨格编辑拆分与扣资源对账；按需编辑日志压实；1w 地图 + 目标在线的容量测试（并入首发清单 L06 / L07） | 跨格编辑无双写、接缝最终一致；内存、写放大、全服出口带宽在预算内（对照第 7.1 节） |
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
2. **（已决定，D14：P3 推送，之前是启动快照）** Ghost 带里邻格的块只在启动时读一次快照，之后邻格改了不更新（P3 的边界块推送）；P1 是否需要在 P3 之前就禁止依赖 Ghost 带块的查询结果（例如跨格移动的高度判断）。

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
| 编辑入口 | `game/terrainmap`：`Map.Brush(ctx, playerID, requestID, brush)` | 顺序：参数与 `Policy`（半径 ≤ 1600、≤ 4 块、强度 ≤ 50、高度 `[0, 16383]`）→ 每玩家令牌桶（2/s）→ `Rules.AllowEdit`（领地 / 权限钩子，缺省放行）→ 第一次改的块 `GetOrCreate` → 一笔多块事务。前三步拒绝时零写入；`ErrBrush` / `ErrRateLimited` / `ErrDenied` / `ErrNotOwned` / `ErrUnloadable` / `ErrOccupied` 各自可 `errors.Is` |
| 多块事务 | handler `TerrainBrush(targets []ISurfaceEntity, editID, brush)`（`rollback=undo durability=strict`） | 生成的 `MultiGroupSync_TerrainBrush`，Nest 排序取锁。`Map.ApplyBrush`：任一块已记过这个编辑 ID → 整笔按重复返回；核对目标正是 `brush.Chunks`、都归本进程写且可用；取编辑前数据（改过的块取 DAO，没改过的取原始块）→ `brush.Apply` → `Rules.Occupied`（建筑占位钩子，命中则整笔返回 `ErrOccupied`、回滚）→ 每块 `SurfaceComponent.Apply` → 每块 `RecordApplied`（锁住的每一块都记）。提交后装视图是可复用的一步 `Map.afterCommit`（P1 的 `ApplyEdit` 也改用它）：`nest.AfterCommit` 里 `SetOverlay` 并通知地形同步登记 subject |
| 幂等 | `terrainmap.EditID(playerID, requestID)` | splitmix64 混合成 63 位（DAO 的 `[]uint64` 存成 BSON int64，最高位置位的值在持久化准备阶段被拒，整笔失败）。重复返回 `Duplicate = true` 与各块当前版本（不是第一次的结果），不再改 |
| 整字段守卫 | `SurfaceComponent.Apply` | 本块改了值的高度点数 > `HeightsGuard`（1000）时 `SetHeightsAll`，否则对变了的下标 `SetHeightsAt`；材质有变化时整字段 `SetMaterialsAll`；写完 `PublishSyncDirty` |
| 协议与错误码 | `protocol/def/terrain_edit.go`、`game/controllers/player/terrain_edit.go`、`internal/errors/terrain.go` | `TerrainEdit` msgid 10038；线上整数越出字段类型范围直接拒绝（不截断）。业务码 100052 `terrain_disabled`、100053 `terrain_brush`、100054 `terrain_not_owned`、100055 `terrain_unavailable`、100056 `terrain_rate`、100057 `terrain_denied`、100058 `terrain_occupied`；其余错误答 internal |
| 同步对象 | `game/entities/terrain_chunk`：`sync=true syncNamespace="terrain" subjectPacker=NewTerrainChunkSyncPacker` | packer 只有缺省视图，字段是 DAO 上标了 sync 的四个（`Applied` 不在里面）；全量 `MarshalSync`，增量 `MarshalSyncChanges`（变化树：按下标的高度增量、整字段替换），实现 `SubjectSyncChangePacker`。demo 的 Nest 没有 EntitySync 观察者，提交不自己标 subject，所以与 Player / Monster 一样由 `PublishSyncDirty` 标脏（取客户端专用掩码 `TakeEntitySyncChanges`，不动服务间同步掩码） |
| 地形 Interest | `internal/service/<game>/terrain_sync.go`、`scene.go` | 场景 Manager 上另注册一个 `policy.Interest`（独立订阅来源），玩家主视野是它的观察者：`Join` 时 `Watch`、移动提交后 `MoveView`（离上次报告 ≥ 8m 才报）、离开时 `Unwatch`；进入半径 200m、离开 220m、`MaxVisible` 64；只有改过的块是 subject（显示在块中心）。开地形时 `frame.Limits.MaxObjects` = 100 + 64，`SnapshotBudget.MaxSnapshotsPerSessionPerFlush` = 4。只在普通场景开（分区场景的地形同步随 P3） |
| 启动时的 subject | `terrainSync.loadEdited`（`startTerrainSync`，`Map.Load` 之后） | P1 的 `Load` 只把记录读进视图、不装实体；同步要实体的 `SubjectSyncState`，所以本格改过的块（`Map.EditedOwned`）经 `GetOrCreate` 冷加载（8 路并行）并登记。之后第一次改的块在提交后登记（`Map.SetCommitted`）。Ghost 带里邻格的块不登记 |
| 换版丢弃（D13） | `Map.Load` + handler `TerrainDiscard` + `SurfaceComponent.Discard` | 本格的块：记录 `BaseRevision` ≠ 包 → `GetOrCreate` + 一笔 strict 事务把 DAO 重置为 Version 0（`BaseRevision` 0、数据清空、`Applied` 保留）→ `LoadBase`，`LoadStats.Discarded` 计数、Warn 日志（块、旧 / 新版本、旧 Version）。事务失败：不装入、`markUnloadable`（查询 `ErrNotLoaded`、编辑 `ErrUnloadable`）、`StaleBase` 计数、Error 日志，记录不动。邻格 Ghost 带里的旧记录：不装入、不动，`StaleBase` 计数 |

**为什么丢弃是“重置为 Version 0”而不是删档**：块的实体 ID 由坐标确定，DataEngine 删档会留下墓碑，删过档的 ID 不能再 `GetOrCreate`（答 entity_deleted），这块就永远不能再改。重置走的是同一条正式路径（Nest strict 事务 → WAL → 投影），P1 早已规定 Version 0 的记录按原始块看待，重启后也一样。代价：库里留一条约 300 字节的空记录（含 `Applied`）。

**Ghost 带里邻格的旧记录怎么显示**：选“不装入”（查询答 `ErrNotLoaded`）。约束是不双写、不越权删：本进程既不能改它，也不能替它的格丢弃；按旧记录显示是已作废的数据，按原始块显示可能与它的格在丢弃前仍在服务的数据不一致。它的格丢弃后，本进程要到下次启动才读到（Ghost 副本是启动快照，D14）。

**测试命令与结果**（2026-11-08，Apple M5，go1.27.3，生成工程 replace 到本分支）：

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| 笔刷算法与金样 | `go test -race ./gameplay/terrain/` | `ISqrt`、三种衰减的权重、四块交点每点只改一次（177 点）、夹到基岩 / 上限且不往外推、常数整平幂等、确定性且不改入参、参数拒绝、玩家半径至多 4 块、`TestClientBrushGolden` |
| 组件 | `go test -race ./game/entities/terrain_chunk/` | 新增：整字段守卫（1001 点整字段、两笔各 ≤ 1000 点逐点，两条路径数据相同，值没变的点不写）、`Discard` 只重置仍基于旧版本的记录 |
| 编辑、幂等、回滚 | `go test -race -run Terrain ./internal/service/<game>/` | `TestTerrainBrushRepeatsAndReordersApplyOnce`（四块交点的编辑：同一请求重放、与后一个请求乱序到达、另一个玩家的同一请求 ID，各只生效一次，重放不改任何块，每块都记了编辑 ID，重启后重放仍是重复，最终视图等于纯计算按生效顺序叠加的结果）；`TestTerrainBrushRefusalsWriteNothing`（请求 ID 为 0、参数、半径 / 强度 / 高度上限、地图外、没覆盖到采样点、不归本格、权限钩子、建筑占位钩子，每种拒绝后所有块的实体、`Applied` 与视图逐点不变、没有提交）；`TestTerrainBrushRateLimit`；`TestTerrainBrushRollbackLeavesEveryChunkAndTheView`（提交器拒绝的四块编辑后，已改与未改的块、视图都不变，同一请求再来一次生效）；`TestTerrainBrushOverTheGuardWritesTheWholeField` |
| 换版丢弃 | 同上 | `TestTerrainRecordOnAnotherBaseRevisionIsDiscarded`（替换 P1 的 `…IsNotLoaded`）：分区 3 × 2 格的格 (0,0)：本格旧记录丢弃（`Discarded` 1），邻格的不装入（`StaleBase` 1）、库里不动；注入事务拒绝时这块不装入、查询 `ErrNotLoaded`、编辑 `ErrUnloadable`、库里记录逐字节不变；重启后这块是新原始块、再改从 Version 1 开始，再重启是覆盖块 |
| 真实 TCP | 同上，`terrain_sync_test.go` | `TestTerrainChunksReachAClientOverRealTCPAndMatchAfterAReconnect`：视野内 3 块改过的块以全量到达、视野外的不到；r=5m 抬高是按下标的高度增量、改材质是材质整字段、守卫整平是高度整字段；断线后改两块、新改一块，重连后客户端持有恰好这 4 块，版本与 4096 点高度 / 材质逐点等于服务端，`SessionsLost` 0。`TestATerrainViewAtItsCapKeepsTheSession`：视野 500m 内改过 100 块、另有 40 个怪，客户端持有恰好 64 块 + 41 个其他对象，连接仍在、`SessionsLost` 0，28 次 Flush 发完（快照预算生效） |
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
- `SnapshotBudget` 是 Manager 级配置，开地形后对同一场景的玩家、怪的进入视野全量也生效（每会话每 Flush 4 个）；不开地形的场景不变。
- 去重的回复是“重复 + 当前版本”，不是第一次的结果（第一次的结果没有存）。

**方案没覆盖、本轮先选的**：

1. `Policy` 的数值：强度上限 50（5m）、基岩 0、上限 16383（原始值 ≤ 16383 时 PB varint 2 字节）；整平目标必须在范围内。
2. 频率限制放在 `Map` 里（进程内令牌桶，容量 = 每秒次数，重启清零，重复请求也占频率）：玩家进程 = 格进程，这里是唯一入口；P3 拆成 Base / Real 后要挪到 Base。
3. 编辑 ID 的派生 `splitmix64(playerID ⊕ splitmix64(requestID))` 取 63 位；跨格子 ID 预留为对（玩家，请求 ID ⊕ 格号）同一函数（P5 定）。
4. 启动时把本格改过的块装成实体（每块多一份约 12KB 的 DAO 加变化树；1w 地图一个格全改过即 2304 块约 30MB、启动多 2304 次按 `_id` 读，耗时按 §11 的单块读推算为几百毫秒，**没有实测**）。备选是只给进入某玩家视野的块按需装实体，要改订阅侧，P5 规模测试时再看。
5. 第一次改的块在提交后才登记为 subject（提交前没有可同步的数据）；登记失败只记 Error 日志，玩家要到重启后才看得到这块（没有重试）。
6. 地形视野不跟玩家的 `maxVisible`（主视野的）走，固定 64；视点按 8m 节流。
7. 没有给机器人（loadtest）加编辑动作：地形缺省关闭，压测配置里也没有地形包。

**D11 / D12 复议建议**（不改决定）：

- **D11（材质整字段）**：实测证实热点下行几乎全由改材质决定：全部带材质时每观察者约 133KB/s，是只改高度的 7 倍；按下标增量的材质（每点约 8 字节，r=5m 约 0.65KB）能把这一档降到约 35KB/s。建议：先定客户端下行预算与“挖填是否总伴随改材质”的玩法前提；如果挖填默认改材质（例如挖开露出泥土）或预算低于约 100KB/s，就换成 `[]uint16` 列表（未上线，换类型不需要兼容）。如果改材质是低频的单独操作（刷路、铺地），保持 `[]byte`。
- **D12（客户端版本缓存）**：跑图加载 6m/s 全改过 8KB/s、12m/s 17KB/s，登录 / 传送约 380KB 由快照预算在约 0.4s 内发完，都远小于热点里改材质的量。建议维持“不做”，到出现高速移动（> 20m/s）、频繁传送或加大视野半径时再做。

**待维护者决定**：

1. 换版丢弃用“重置为 Version 0”代替删档（上文理由）。如果要真删，需要 DataEngine 允许确定性 ID 删档后重建（框架改动），或块实体 ID 带上 `BaseRevision`（换版即换 ID，旧记录另行清理）。
2. 开地形后会话对象上限变成 164：客户端的帧解码上限须 ≥ 164（C# `SyncFrame` 缺省 `maxObjects = 100`，按 2 倍容差可解 200 个对象的帧，现在能过；建议客户端显式传 164）；`SnapshotBudget` 对玩家、怪同时生效，是否接受。
3. 客户端下行预算（D11 复议的前提）。

**给 P3 的接口说明**：

- 跨格：`Map.Brush` 现在要求覆盖到的块全归本进程（否则 `ErrNotOwned`）；P3 在 Base 侧按块的归属拆成每格一个子请求（子 ID 用 `EditID` 同一函数派生），各格独立走 `Brush` 的事务部分（`ApplyBrush`），频率与权限在 Base 侧做一次。
- Ghost 推送（D14）：本格提交后的通知入口是 `Map.SetCommitted`（目前只接了地形同步）；推送可以挂在同一处，邻格收到后 `View.SetOverlay`（版本单调）。邻格换版丢弃后的空记录（Version 0）需要一条“回到原始块”的推送（视图目前没有“撤下覆盖块”的写入口，要加）。
- 分区场景的地形同步：`terrainSync` 现在只挂在普通场景的 Manager 上；分区场景要在每个格的复制管理器上各挂一个，并接上 Ghost 带块的 subject。
- 移动 / 寻路：查询仍是 `Map.Height` / `Material` / `IsWater` / `Probe`（无锁、handler 里可调），P3 起改过的块按 `View.Chunk` 的版本失效寻路缓存。

**未验证**：真实 WAL / Mongo 下的单笔编辑延迟与吞吐，以及持久门槛对地形块的实际挡住（测试场景没接 `DurableLSN`）；多观察者同时在热点时的服务端 CPU 与总出口；真实 game 进程实跑（配置地形包 + 客户端发 `TerrainEdit`）；C# 客户端的笔刷同算（金样已备好）与真实 entitysync 链路上的 C# 接收；四块交点编辑比单块慢约 7 倍的原因（CPU 剖析里 Nest 只占约 10%，疑为多实体请求的分派交接，没有细查）；启动时装实体的耗时与内存（第 4 条只是推算）；Linux。
