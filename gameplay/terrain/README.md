# terrain：可编辑地形的底座（P1 框架部分 + P2 笔刷 + P3 行走与寻路、块副本 + P4 区域水位）

[可编辑地形方案](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md) P1 的框架部分：块坐标、原始地形包（base pack）的读写、内存中的地形视图与整数定点查询。纯逻辑加文件 I/O，不依赖 Nest / DataEngine / entitysync。P2 加了笔刷（`brush.go`：抬高 / 降低 / 整平 / 改材质的纯整数计算，见下文“笔刷”）。P3 加了行走规则与寻路（`walk.go`：坡度 / 台阶 / 下落 / 游泳的判定与高度场 A*，见下文“行走与寻路”）。被玩家改过的块（`TerrainChunk` 实体、持久化、同步、重启一致）由上层接入，本包只接收它们的数据作为覆盖块。实测数字见证据目录：[P1](../../docs/maintenance/evidence/terrain-p1-20261108/README.md)、[P2](../../docs/maintenance/evidence/terrain-p2-20261108/README.md)。

## 坐标与块

| 项 | 约定 | 理由 |
| --- | --- | --- |
| 世界单位 | 整数，**1 单位 = 1 厘米** | 与 cellworld 一致（`gameplay/cellworld/grid.go`：测试与测量里 1 单位 = 1 厘米；NPC 300 单位 / 秒 = 3 m/s）。地形插值需要亚米精度，厘米正合适 |
| 块 | 64m × 64m（`ChunkSize` = 6400 单位），每块 64 × 64 = 4096 个采样点（`ChunkPoints`） | 方案 D4 |
| 采样 | 间隔 1m（`SampleSpacing` = 100 单位） | 方案 D2 |
| 共享边 | 世界采样点 (gx, gy) 只属于块 (⌊gx/64⌋, ⌊gy/64⌋)：块的右边与上边那一排采样点归右 / 上邻块，不重复存 | 方案 §2 |
| 块内下标 | 行优先，`下标 = j·64 + i`（i 沿 X、j 沿 Y），`Heights` / `Materials` 都按这个顺序 | 与实验 DAO 一致 |
| 高度 | `uint16` 原始值，1 = 10cm（`HeightStep`）；世界高度 = `HeightBase + 原始值 × 10`（厘米） | 方案 D2 |
| 地图范围 | 从世界原点开始：`[0, ChunksX·6400) × [0, ChunksY·6400)`，每边最多 1024 块 | 包头记录块数 |
| 块键 → ID | `ChunkKey.ID()`：高 32 位 X、低 32 位 Y（uint32 位模式），`KeyFromID` 还原；地图内的 ID < 2^63 | 下一轮拼实体 ID |

这些是常量而不是配置：它们决定 DAO 形状与客户端算法，服务端、客户端与包必须完全一致。包头仍记录它们，读包时逐项核对，不一致按 `ErrPackVersion` 拒绝；真要换值时随格式版本一起改。

换算函数：`ChunkOf(x, y)`（向负无穷取整，远在地图外的坐标饱和到 int32，不会回绕进地图）、`PointIndex(x, y)`（所在 1m 格左下角采样点的块与下标）、`ChunkKey.Origin / Bounds`、`ChunksIn(rect)`（与矩形相交的全部块，上层列出一个格要装的块用）。

**注意 cellworld 的格尺寸**：方案 §2.1 / §8.1 的 `cell_size` 3072、`ghost_band` 500 是按“1 单位 = 1 米”写的。按厘米，48 块的格是 `cell_size` 307200，500m 的 Ghost 带是 `ghost_band` 50000；格边长必须是 6400 的整数倍（块不跨格），网格原点也要落在 6400 的整数倍上。

## 原始地形包（格式版本 2）

单文件，整数全部小端。字段的逐字节布局在 [pack.go](pack.go) 头注释。

```
包头 128 字节：magic "RTERRAIN"、格式版本 2、BaseRevision、ChunksX / ChunksY、
              每块采样数 64、采样间隔 100、高度步长 10、HeightBase、WaterLevel（全局水位）、codec 1、
              索引偏移与项长、区域水位个数、metaSHA256 = SHA-256(包头前 96 字节 ‖ 索引表 ‖ 区域表)
索引表       ChunksX·ChunksY 项（行优先），每项 offset uint64、length uint32、SHA-256（块存储字节）
区域表       每项 minX、minY、maxX、maxY（块坐标，半开）、level（世界高度厘米；MinInt32 = 没有水）
块数据       每块独立压缩，按索引顺序排列
```

版本 2（2026-11-10，P4）在版本 1 上加了区域水位；版本 1 的包按 `ErrPackVersion` 拒绝，用新工具重新生成（不兼容旧包）。

- **打开**（`OpenPack`）只读包头与索引表（1w 地图约 1MB），整体校验 `metaSHA256`、每项偏移在数据区内；包头或索引坏了返回 `ErrBadPack`，版本或几何常量不符返回 `ErrPackVersion`。
- **读块**（`Pack.ReadChunk`）：一次 `ReadAt`、校验这块的 SHA-256、解码，返回新分配的 `Chunk`。坏块返回 `ErrCorruptChunk`（带块坐标），其他块照常可读；不返回默认数据。实测翻转块数据中间一个比特，DEFLATE 解码仍然“成功”，只有 SHA-256 能发现。可被任意多个 goroutine 并发调用。
- **写**（`WritePack(io.WriterAt, …)` / `WritePackFile(path, …)`）：`ChunkSource` 逐块给数据，按批（256 块）并行取块与压缩、按索引顺序写出，输出与并行度无关、逐字节确定。`WritePackFile` 写同目录临时文件、fsync 后改名，失败时原文件不变。
- **数据来源**：`RAWImport{…}.Source(meta)` 读美术导出的 RAW16 高度图 + RAW8 材质图（大小端、Y 翻转、按 `MaxHeight` 换算可选；图不必是块的整数倍，图外采样复制图边）；`Generated(meta, seed)` 是只用整数运算的确定性程序化地形，供测试、压测与金样。`Flat(meta, raw, material)` 是一张平地（每点同一高度与材质），game-demo 的 `terrainpack` 缺省用它（起伏的 `Generated` 在 45° 行走规则下大半的边走不通，不适合演示）。没有做命令行工具，游戏工具链直接调库函数。

### 块编码（codec 1）

明文 = 4096 个高度的 zigzag uvarint ‖ 4096 字节材质；存储 = raw DEFLATE（RFC 1951，无 zlib 头，BestCompression）。高度的预测值是平面预测（左邻 + 下邻 − 左下邻；第一行只用左邻、行首只用下邻、第一个点为 0），存的是与预测值之差。

同一批 8 个块（1w 程序化地形，含 ±20cm 逐点抖动；`TestScaleCodecChoice`）每块的平均字节：

| 编码 | 字节 / 块 |
| --- | ---: |
| 原始小端 + DEFLATE | 6554 |
| 只用左邻差分 + DEFLATE best | 4078 |
| 只用下邻差分 + DEFLATE best | 4010 |
| **平面预测 + DEFLATE best（采用）** | **2733** |
| 平面预测 + DEFLATE default | 2817 |

程序化地形比真实高度图平滑，真实地形的压缩率要用美术的图重测；编码只影响包大小和读块耗时，不影响查询结果。

## 地形视图 View

`NewView(pack)` 建空视图。每个块坐标至多一份数据：

| 入口 | 行为 |
| --- | --- |
| `LoadBase(key)` | 从包读原始块装入（文件 I/O，锁外读）；这块已装着（原始或覆盖）时什么都不做 |
| `SetOverlay(key, Overlay{BaseRevision, Version, Heights, Materials})` | 装入 / 更新覆盖块，替换原始块或更旧的覆盖块；复制入参。拒绝：地图外、形状不对、`Version < 1`、`BaseRevision` 与包头不同（`ErrBaseRevision`）、比已装覆盖块旧（`ErrStaleOverlay`），版本相同则替换 |
| `Unload(key)` | 卸下（原始或覆盖） |
| `Version(key)` / `Chunk(key)` / `Len()` | 查装载状态与版本（0 = 原始块）；`Chunk` 返回副本 |
| `Height` / `Material` / `IsWater` / `WaterDepth` / `Probe` | 查询，见下 |
| `Meta()` | 地图参数的副本（`Pack.Meta()` 同）：`WaterRegions` 不与视图 / 包共享，调用方改返回值不影响查询——区域水位只随原始地形包换版。有区域水位时每次调用分配一份区域表，循环里先取一次；自己持有 `Meta` 又要向外给时用 `Meta.Clone()` |

不做 LRU 之类的自动淘汰：装哪些、卸哪些由上层按格 / 视野决定。

**并发契约**：

- 块表是按块序号排开的原子指针数组（1w 地图约 200KB，地图上限时 8MB）。查询不取锁，只做原子读与内存计算，任意 goroutine 可并发调用，可以在 Nest short 池里调用。
- 写入口只有 `LoadBase` / `SetOverlay` / `Unload`，由一把互斥锁串行，各自原子地换掉一个槽位。块数据装入后不可变，一次查询对每个用到的块只读一次槽位：同一块内不会混用两个版本，`Probe` 的高度与材质出自本块的同一版本。
- 跨块的查询（边界插值）各块分别取读到时的版本；一次改多块的编辑由上层逐块 `SetOverlay`，在 View 里不是原子的。
- 同一块的装 / 卸次序由上层保证（例如格进程按块串行处理），View 只保证每次调用本身原子。
- `LoadBase` 做文件 I/O，不要在 short 池里调用；`Pack` 要比 View 活得久。
- 曾用“map + 读写锁”：10 个 goroutine 并发查询的总吞吐（109.8 ns/op）反而低于单线程（24.8 ns/op），读锁计数的缓存行在核间来回；换成原子数组后单线程 9.2 ns/op、10 路并行合计 1.6 ns/op（证据目录 `bench.txt` / `rwmutex/`）。

## 查询算法（客户端须逐位相同）

S = `SampleSpacing` = 100，gx = x / S、fx = x % S（y 同理）。四个采样点 h00 = (gx, gy)、h10 = (gx+1, gy)、h01 = (gx, gy+1)、h11 = (gx+1, gy+1) 的原始值：

```
num    = (h00·(S−fx) + h10·fx)·(S−fy) + (h01·(S−fx) + h11·fx)·fy        // 0 ≤ num ≤ 65535·S²，用 64 位整数
Height = HeightBase + (num·HeightStep + S²/2) / S²                        // 整数除法，四舍五入到厘米
```

- 权重为 0 的邻点不读（fx = 0 不取 gx+1 列，fy = 0 不取 gy+1 行）：采样点正上方只需要本块。
- gx+1 越出地图右边界时用 gx 列（边缘复制），上边界同理：地图边上的查询不需要地图外数据。
- 其余情况下邻点可能在右 / 上 / 右上邻块，**邻块没装入返回 `ErrNotLoaded`**（错误里带块坐标），不猜测。地图外返回 `ErrOutOfMap`。
- `Material(x, y)` = 采样点 (gx, gy) 的材质（所在 1m 格左下角，总在本块）。
- **水**（方案 §5、D3“全局 / 区域水位线”，P4）：一块恰好一条水位线 `Meta.WaterLevelOf(块)`——块在某片区域水位（`Meta.WaterRegions`，按块对齐、互不重叠、至多 1024 片）里取区域的 `Level`，否则取全局 `WaterLevel`；`View` 在 `NewView` 时按块算好表（`View.WaterLevelOf`，O(1)）。`IsWater(x, y)` = `Height(x, y) < ` 包含 (x, y) 的块的水位线（等于不算水）；`WaterDepth(x, y)` = 水位线 − 地面（不是水为 0，是水为正）；`Probe` 的 `Ground.WaterLevel` / `Depth()`。水深按 **int64** 给出（客户端 `long`）：水位线与地面高度都是 int32，但高度基准可以为负、水位线独立配置，`Meta.Validate` 不约束两者之差，合法参数下差值可以超出 int32（最大 2³² − 1）；`Walker` 判游泳用同一个 64 位差值。挖到水位线以下自动是水、填到水位线及以上自动是陆地，不存“是不是水”、不需要跨块计算，客户端同算。区域边界两侧水位不同时水面在块边上有台阶，美术把区域边界放在高于两侧水位的岸上。

**金样**：[client/spec/terrain-golden.pack](../../client/spec/terrain-golden.pack)（3 × 2 块：程序化地形加一块 0 / 65535 棋盘格与斜坡的边界情形块）与 [terrain-queries.json](../../client/spec/terrain-queries.json)（包与每块解码后的 SHA-256，246 个位置的期望高度 / 材质 / 水 / 所在块的水位线，含块边界、四块交点、地图边缘、地图外；包里有两片区域水位：块 (1,1) 高出全局 3m、块 (0,1) 没有水）。客户端实现读这个包、逐块核对、全部装入后逐点核对。`TestClientTerrainGolden` 检查提交的包能解出金样数据、从它重新算出的 JSON 与提交的逐字节相同（不要求包与重新压缩的逐字节相同，Go 版本升级不会让它变红）；改格式或算法时 `go test ./gameplay/terrain -run TestClientTerrainGolden -update`。P1 证据目录里的 `check_golden.py` 是只按本文与 pack.go 写的独立实现（当时是格式版本 1、没有区域水位，246 个位置全部一致；格式版本 2 之后未重跑）。C# 已在 `client/dotnet/Roost.Client/Terrain/`，C++ 未做。

## 笔刷（P2，`brush.go`，客户端须逐位相同）

`Brush{Op, X, Y, Radius, Falloff, Strength, Target, Material}`：中心是世界坐标（厘米），半径厘米（`[1, MaxBrushRadius]`，玩家上限由上层的 Policy 定，demo 是 1600）。只用整数，同一输入在任何平台得到同一输出，客户端可以同算做预测。

- **覆盖的点**：采样点 `(gx·100, gy·100)` 与中心的距离平方 `d² ≤ R²`。每个采样点只属于一块（共享边归右 / 上的块，见“坐标与块”）；`Chunks(meta)` 是覆盖点所在的块，按 (Y, X) 排序。
- **权重** `w ∈ [0, 65536]`（`WeightScale`）：`FalloffConstant` 在 `d ≤ R` 时为 65536；`FalloffLinear` 取 `d = ISqrt(d²)`（向下取整的整数平方根），`d < R` 时 `w = ⌊65536·(R − d)/R⌋`；`FalloffSmooth` 在 `d² < R²` 时 `t = ⌊65536·(R² − d²)/R²⌋`、`w = ⌊t²/65536⌋`；其余为 0。
- **抬高 / 降低**：`δ = ⌊(Strength·w + 32768)/65536⌋`；抬高 `max(cur, min(cur + δ, Ceiling))`、降低 `min(cur, max(cur − δ, Bedrock))`——已经在限外的点不往外推。
- **整平**：`diff = Target − cur`，`step = sign(diff)·⌊(|diff|·w + 32768)/65536⌋`，新高度 `cur + step`（不越过 `Target`；`Target` 必须在 `[Bedrock, Ceiling]` 内）。常数衰减即整块压到 `Target`，同一个常数整平做两次，第二次没有变化。
- **改材质**：覆盖的点材质改成 `Material`，不看衰减；不动高度。
- **输出**：`Apply` 返回每块改了值的点（`PointChange{Index, Height, Material}`，下标按升序），值没变的点不出现；`ChunkChange.HeightChanges` 是这块改了高度的点数（上层的整字段守卫按它判断）。
- **只数不改**：`View.BrushChanges(b, limits)` 返回把 `b` 用在视图当前数据上会改变值的高度点数与材质点数（算法同 `Apply`，每个采样点只算一次，一个点两者都变算 2）。不复制块数据（装入的块不可变）、只读、可并发；覆盖到没装的块报 `ErrNotLoaded`。结果是读到时的快照，上层拿它在事务之前估算改动量（game-demo 的每玩家改动点数配额，[方案 §17](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#17-编辑配额与材质守卫-500-实施结果2026-11-08)）。
- **拆成几部分（为 P5 跨格编辑而加）**：`ApplyIn(meta, limits, chunks, part)` / `View.BrushChangesIn(b, limits, part)` 只算 `part` 里的块（nil 是全部）。每个采样点的结果只取决于它自己编辑前的值与到中心的距离，所以各部分合起来与整笔 `Apply` 逐点相同（`TestABrushSplitIntoPartsAddsUpToTheWholeBrush`）；只有 part 里覆盖到的块需要数据。维护者 2026-11-10 撤回跨格笔刷（覆盖两个格的笔刷在 Base 上直接拒绝，[方案 §30.10](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)），game-demo 不再按部分调用，这两个入口保留为通用能力。`Footprint(meta)` 是每块被覆盖的采样点数，只看几何：一次笔刷只改一个字段，所以它是这块改动点数的上限，没装这些块的进程（分区场景转发编辑的 Base）按它预扣配额（仍在用）。

**金样**：[client/spec/terrain-brush.json](../../client/spec/terrain-brush.json)——`ISqrt` 表、三种衰减的权重表、17 个笔刷（四种操作 × 三种衰减、块边界、四块交点、夹到基岩 / 上限、整平幂等、地图边缘）在 `terrain-golden.pack` 原始块上的结果：每例覆盖的块、每块改了的点数与 SHA-256（逐点 `index u16 LE, height u16 LE, material u8` 按下标升序），点数不超过 40 时列出全部点。`TestClientBrushGolden` 核对；改算法时 `go test ./gameplay/terrain -run TestClientBrushGolden -update`（不重写包）。

## 行走与寻路（P3，`walk.go`，客户端预测须同算）

移动与寻路共用一套判定（[方案 §22](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#22-p3-移动与寻路实施结果2026-11-09)），全是整数、只读视图、不取锁、不分配（`Step`），可以在 Nest short 池里调用。

- **节点**：1m 采样点 `(gx, gy)`；世界坐标 `p` 属于最近的采样点（`Walker.Cell`，四舍五入；在地图 / `Bounds` 里时夹到可用节点范围）。节点的地面高度就是采样点高度（只读本块，没有插值）。
- **站立高度**：普通节点是地面；水深（节点所属块的水位线 − 地面，与 `View.WaterDepth` 同一来源）超过 `WadeDepth` 的是游泳节点，站立高度取水面（那块的水位线），速度 `SwimSpeed`‰（0 = 不能游泳：深水不可进入，站在里面的可以走出来）。
- **相邻一步** `a → b`（八邻域，`run` 正交 100、对角 141 厘米，`dh` = 站立高度差）：上行 `dh·1000 ≤ MaxSlope·run` 或 `dh ≤ MaxStep`；下行 `|dh|·1000 ≤ MaxSlope·run` 或 `|dh| ≤ max(MaxStep, MaxDrop)`；对角还要两条正交拐角路线都走得通（不切角）。地图外、`Bounds` 外、块没装入（不猜）、`Occupied` 钩子（建筑占位，P4）为真的节点不可进入。
- **缺省规则** `DefaultWalkRules`：坡度 45°（1000‰）、台阶 50 厘米、陡降 3 米、涉水 50 厘米、游泳 60%、上坡代价 2（爬 1 米折 2 米平路）、下坡代价 0、寻路估价放大 1.5 倍（`SearchWeight` 1500）。判定只看高度差，与 `HeightBase` 正负无关。
- **`Step(from, to)`**：移动逐节点问（实现 `movement.Surface`）；`from == to` 表示站着，只要求节点有定义。**`Standable(p)`**：能否放在这里（瞬移、出生点、寻路终点）。**`StandZ(p)`**：站立高度（游泳节点是水面，否则 `View.Height` 的插值高度）——服务端位置只有 X / Y，高度由它推出，客户端同算。**`Swimming(p)`**。
- **`FindPath(from, to, maxVisited)`**：节点图上八方向 A*，边与 `Step` 同一规则；代价 `(run·1000 + 上升·UphillCost + 下降·DownhillCost)·1000 / 目标节点速度‰`，估价八方向距离 × `SearchWeight`（加权 A*：1000 是最优；缺省 1500，路线代价至多是最优的 1.5 倍——估价不含爬升，起伏地面上最优搜索为证明最优要展开的节点随坡度代价暴涨，200m 路线约 1.4 万个，加权后约 700 个）；同代价按“估价小、先发现”展开，同一视图状态结果确定。每块只读一次槽位（并发提交不会让一次搜索读到同一块的两个版本）。上限缺省 `DefaultMaxVisited` 4096，超过答 `spatial.ErrPathBudgetExhausted`（可能有路）；走不到 / 终点不可站答 `spatial.ErrNoPath`；地图外 `ErrOutOfMap`；起点 / 终点块没装 `ErrNotLoaded`。
- **`Route`**：`Points` 是起点、起点所在节点、途经节点、终点所在节点、终点（相邻重复去掉）——每段只跨一个节点边界，移动逐节点检查时判定的正是寻路用过的相邻节点；代价是开头可能先折回起点节点的中心（≤ 约 0.7m）。`Chunks` 是判定读过的块与版本，`Fresh(view)` 报告这些块是否仍是同一版本（改块、卸块即失效）；不判断有没有出现更短的路。

## 给上层（TerrainChunk 实体接入）的接口说明

- DAO 与 `Chunk` / `Overlay` 一一对应：`Heights []uint16`（4096）、`Materials`（4096）、`BaseRevision`、`Version`。本包与原始地形包里材质是每点 1 字节（`[]byte`）；game-demo 的 DAO 为让材质也按下标增量同步与持久化，用 `[]uint16`（方案 D11），在实体组件边界换算（值域 0～255，越界的记录按损坏拒绝）。本包的格式与公开类型不因此改变。
- 第一次改一块时，用 `Pack.ReadChunk(key)`（或 `View.Chunk(key)`）取原始数据作初值建实体；这两个都返回调用方独占的新切片，可以直接交给 DAO。
- 每次提交后把 DAO 的当前值 `SetOverlay` 给 View（View 复制，不与 DAO 共享内存）；版本必须单调（View 拒绝回退）。
- 格进程启动：对本格（加 Ghost 带）`ChunksIn` 出的块，有持久记录的 `SetOverlay`，其余 `LoadBase`（I/O，放在 io / long 池或启动阶段）。实测 1w 地图装满一个格（2304 块）单线程约 120ms、8 路并行约 29ms，堆约 27MB。
- 原始地形换版（`BaseRevision` 变了）时旧覆盖块会被 `ErrBaseRevision` 拒绝。维护者 2026-11-08 决定丢弃旧编辑：game-demo 的地形服务在启动时把本格基于旧版本的记录经正式事务重置为空（Version 0）、改用原始块，邻格的不动（方案第 14 节）。
- 笔刷：上层在锁住 `Chunks(meta)` 的全部块之后，用块的当前数据（改过的块取 DAO，没改过的取原始块）调 `Apply`，把每块的 `PointChange` 写进 DAO；`Validate` 只检查笔刷本身，频率 / 配额、权限、建筑占位等由上层做（配额的点数可以先用 `View.BrushChanges` 在锁外估算）。

## 块副本（P3，`replica.go`）

块只归包含它的格的进程（拥有者）写；别的进程要看它（邻格的 Ghost 带、把玩家附近的块同步给本进程的客户端）时放一份只读副本，由 `Replicator` 维护（[方案 §21](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#21-p3-ghost-副本t05d192026-11-09)）。与传输无关：`ReplicatorConfig.Send` 是“非阻塞地把一段字节发给某个 sid”，至多一次、可能丢失 / 重复 / 乱序（game-demo 用 cellworld 的扩展消息，线上格式 v7）。

- **订阅兼补齐**：副本按拥有者（`OwnerOf`）分组，把想要的块（`Want`）连同本地持有的版本（`ReplicaStore.Held`：0 原始块、≥ 1 覆盖块版本、−1 显示不了）发给拥有者，每 `Renew`（缺省 1s）重发一次；拥有者记下订阅（租约 `Lease`，缺省 3 × Renew），只答版本不同的块（覆盖块发整块，原始块发“原始块”）。推送丢了、任一边重启（拥有者的订阅表丢了），至多一个 Renew 后对齐。本进程自己写的块不订阅。
- **订阅上限**：一份订阅是一条消息（整份替换拥有者记下的那一份），一路对同一个拥有者至多 `MaxSubscriptionChunks` = 32768 块（接收端的解码上限；一个格 48 × 48 块时 10 倍余量），按拥有者分别算，重复的、地图外的、没有拥有者的、本进程自己写的块不计。超过时 `Want` 返回 `ErrSubscriptionTooLarge`、这一次 `Want` 整份不生效（计 `WantsRejected`）：之前接受的订阅照常续期，不会发出接收端解不了的消息；调用方缩小范围后再调。没有分片订阅。
- **推送**：拥有者在块提交之后调 `Changed(key)`（可在 Nest 的提交后回调里调：只登记、叫醒），Run 的 goroutine 把这块的当前状态发给订阅了它的副本；发出之前又提交了几次只发最新一份。整块 raw DEFLATE，r=5m 一次编辑约 2.9～3.7KB / 块 / 订阅进程。
- **顺序**：副本只认比本地新的版本（`Apply` 对旧版本答 `ErrStaleOverlay`，计 `Stale`）；每条状态都是整块，丢中间版本也没关系。
- **代际**：每条消息带发件进程原始地形包的 `BaseRevision`，与本进程不同的整条丢弃（计 `BaseRevisionDrops`；拥有者不答，副本不收）。版本只在同一 `BaseRevision` 内比较：换版时拥有者把旧记录重置为 Version 0（D13），换版前的推送带旧 `BaseRevision`、到换版后的进程一律丢弃，不会把丢弃的覆盖块装回来。同一 `BaseRevision` 内“同一个版本只对应一份内容”的前提是拥有者只推送持久的版本（改块事务 strict，提交后回调在 WAL 落盘之后）。
- **只认拥有者**：一块的状态只接受 `OwnerOf(块)` 发来的（计 `WrongOwner`），拥有者只答自己 `Owns` 的块。
- **并发**：订阅表、想要的块只在 `Run` 的 goroutine 上读写；`Want` / `Changed` / `Deliver` 不等 `Run`、任何 goroutine 可调（`Changed` / `Deliver` 只是短锁 + 非阻塞信号；`Want` 先在调用方的 goroutine 上按拥有者分组、排序、查上限，所以 `OwnerOf` 要能并发调用——只读的分区表）；`ReplicaStore` 的三个方法都在 Run 的 goroutine 上调用（解压、读包在这里做）。
- **占位记录（P5，可选）**：拥有者可以给每块附一份不透明的占位记录（`ReplicatorConfig.Lots`：修订号 + 至多 1024 个 int64；game-demo 是块上的建筑占地，服务端索引、不同步给客户端）。存放处同时实现 `ReplicaLotStore`（`HeldLots` / `ApplyLots`）的那一路订阅时附上持有的占位修订号，拥有者的答复与推送带上当前记录；占位与块数据各有版本，只差占位时答复不带块数据。占位修订号同样只进不退（`ApplyLots` 对旧的答 `ErrStaleOverlay`，计 `LotsStale`）。不实现它的那一路与 v1 行为相同（不带占位记录）。邻格的 Ghost 带用它让行走判定看见邻格的建筑。
- **线上格式 v2**（P5）：订阅加“要占位记录”与持有的占位修订号，块状态每条加标志字节（带占位记录 / 不带块数据）与占位记录。v1 的消息按坏消息丢弃（未上线不兼容，升级须先停旧进程）。
- **限制**：拥有者数据倒退（从备份恢复数据库）不在保证内，副本持有的更新版本不会被拉回（计 `Ahead`），要重启副本进程；各进程原始地形包版本不同（只换了一部分进程）时两边之间不复制，换版要整组进程换包重启。
- **测试**：`replica_test.go`（进程内网络：推送、丢失 / 重复 / 乱序、任一边重启补齐、D13 换版后撤回且旧包的拥有者不答新包的订阅、只认拥有者、订阅上限（32768 / 32769 两侧、被拒后原订阅续期、按拥有者分别算）、占位记录的推送 / 续期补齐 / 倒序不回退 / 不要的一路不带、线上格式；`ROOST_TERRAIN_REPLICA_PERF=1` 记延迟与字节），经真实 cellworld Node（回环与 nats-server）的用例在 `gameplay/cellworld/ext_test.go`。

## 限制

- 只有 2.5D 高度场；洞穴、隧道、悬空不在范围（方案 D1）。
- 水位线是每块一条（全局水位或块所在的区域水位，P4）；按连通区域算的盆地水位、灌水与流体没做。
- 一路副本对同一个拥有者至多订 32768 块，没有分片订阅（见“块副本”）。
- 行走按 1m 节点判定：一个采样间距内的陡崖与 1m 宽的坡判定相同；没有角色体积（只看节点本身）、没有跳跃与攀爬；路线不缓存、不平滑（拐点在节点上）。
- 包要整份放在本地文件里（`os.File.ReadAt`），没有从对象存储按范围读的实现。
- 规模测量里的读块耗时是页缓存热的；冷盘读、Linux 与其他 CPU 没有测。
- 压缩率来自程序化地形，真实高度图未测。
