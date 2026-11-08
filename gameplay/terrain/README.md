# terrain：可编辑地形的底座（P1 框架部分）

[可编辑地形方案](../../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md) P1 的框架部分：块坐标、原始地形包（base pack）的读写、内存中的地形视图与整数定点查询。纯逻辑加文件 I/O，不依赖 Nest / DataEngine / entitysync。被玩家改过的块（`TerrainChunk` 实体、持久化、同步、重启一致）由上层接入，本包只接收它们的数据作为覆盖块。实测数字见[证据目录](../../docs/maintenance/evidence/terrain-p1-20261108/README.md)。

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

## 原始地形包（格式版本 1）

单文件，整数全部小端。字段的逐字节布局在 [pack.go](pack.go) 头注释。

```
包头 128 字节：magic "RTERRAIN"、格式版本 1、BaseRevision、ChunksX / ChunksY、
              每块采样数 64、采样间隔 100、高度步长 10、HeightBase、WaterLevel、codec 1、
              索引偏移与项长、metaSHA256 = SHA-256(包头前 96 字节 ‖ 索引表)
索引表       ChunksX·ChunksY 项（行优先），每项 offset uint64、length uint32、SHA-256（块存储字节）
块数据       每块独立压缩，按索引顺序排列
```

- **打开**（`OpenPack`）只读包头与索引表（1w 地图约 1MB），整体校验 `metaSHA256`、每项偏移在数据区内；包头或索引坏了返回 `ErrBadPack`，版本或几何常量不符返回 `ErrPackVersion`。
- **读块**（`Pack.ReadChunk`）：一次 `ReadAt`、校验这块的 SHA-256、解码，返回新分配的 `Chunk`。坏块返回 `ErrCorruptChunk`（带块坐标），其他块照常可读；不返回默认数据。实测翻转块数据中间一个比特，DEFLATE 解码仍然“成功”，只有 SHA-256 能发现。可被任意多个 goroutine 并发调用。
- **写**（`WritePack(io.WriterAt, …)` / `WritePackFile(path, …)`）：`ChunkSource` 逐块给数据，按批（256 块）并行取块与压缩、按索引顺序写出，输出与并行度无关、逐字节确定。`WritePackFile` 写同目录临时文件、fsync 后改名，失败时原文件不变。
- **数据来源**：`RAWImport{…}.Source(meta)` 读美术导出的 RAW16 高度图 + RAW8 材质图（大小端、Y 翻转、按 `MaxHeight` 换算可选；图不必是块的整数倍，图外采样复制图边）；`Generated(meta, seed)` 是只用整数运算的确定性程序化地形，供测试、压测与金样。没有做命令行工具，游戏工具链直接调库函数。

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
| `Height` / `Material` / `IsWater` / `Probe` | 查询，见下 |

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
- `IsWater(x, y)` = `Height(x, y) < WaterLevel`（全局水位线，方案 §5；等于水位线不算水）。

**金样**：[client/spec/terrain-golden.pack](../../client/spec/terrain-golden.pack)（3 × 2 块：程序化地形加一块 0 / 65535 棋盘格与斜坡的边界情形块）与 [terrain-queries.json](../../client/spec/terrain-queries.json)（包与每块解码后的 SHA-256，246 个位置的期望高度 / 材质 / 水，含块边界、四块交点、地图边缘、地图外）。客户端实现读这个包、逐块核对、全部装入后逐点核对。`TestClientTerrainGolden` 检查提交的包能解出金样数据、从它重新算出的 JSON 与提交的逐字节相同（不要求包与重新压缩的逐字节相同，Go 版本升级不会让它变红）；改格式或算法时 `go test ./gameplay/terrain -run TestClientTerrainGolden -update`。证据目录里的 `check_golden.py` 是只按本文与 pack.go 写的独立实现，246 个位置全部一致。C# / C++ 实现留 P2。

## 给上层（TerrainChunk 实体接入）的接口说明

- DAO 形状与 `Chunk` / `Overlay` 相同：`Heights []uint16`（4096）、`Materials []byte`（4096），`BaseRevision`、`Version`。
- 第一次改一块时，用 `Pack.ReadChunk(key)`（或 `View.Chunk(key)`）取原始数据作初值建实体；这两个都返回调用方独占的新切片，可以直接交给 DAO。
- 每次提交后把 DAO 的当前值 `SetOverlay` 给 View（View 复制，不与 DAO 共享内存）；版本必须单调（View 拒绝回退）。
- 格进程启动：对本格（加 Ghost 带）`ChunksIn` 出的块，有持久记录的 `SetOverlay`，其余 `LoadBase`（I/O，放在 io / long 池或启动阶段）。实测 1w 地图装满一个格（2304 块）单线程约 120ms、8 路并行约 29ms，堆约 27MB。
- 原始地形换版（`BaseRevision` 变了）时旧覆盖块会被 `ErrBaseRevision` 拒绝：保留、重放还是丢弃旧编辑，是上层与产品的决定。

## 限制

- 只有 2.5D 高度场；洞穴、隧道、悬空不在范围（方案 D1）。
- 盆地水位、区域水位线没做，只有包头里的一条全局水位线。
- 包要整份放在本地文件里（`os.File.ReadAt`），没有从对象存储按范围读的实现。
- 规模测量里的读块耗时是页缓存热的；冷盘读、Linux 与其他 CPU 没有测。
- 压缩率来自程序化地形，真实高度图未测。
