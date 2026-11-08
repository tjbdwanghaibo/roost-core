// Package terrain 是可编辑地形的底座（方案 docs/maintenance/FEATURE-2026-11-08-editable-terrain.md 的 P1 框架部分）：
// 2.5D 高度场的坐标与块、原始地形包（base pack）的读写、内存中的地形视图与整数定点查询。
//
// 本包只有纯逻辑与文件 I/O，不依赖 Nest / DataEngine / entitysync。被玩家改过的块（TerrainChunk 实体、持久化、
// 同步）由上层接入：上层把块的 DAO 数据作为覆盖块交给 View，View 只负责“原始块 + 覆盖块”的内存表与查询。
//
// 几个固定约定（理由见包内 README.md）：
//
//   - 世界坐标是整数世界单位，1 单位 = 1 厘米，与 cellworld 的约定一致（gameplay/cellworld/grid.go）。
//   - 块 64m × 64m（ChunkSize = 6400 单位），采样间隔 1m（SampleSpacing = 100），每块 64 × 64 = 4096 个采样点；
//     相邻块的共享边由右 / 上块持有，每个世界采样点恰好属于一个块。
//   - 高度原始值 uint16，1 = 10cm（HeightStep），世界高度 = HeightBase + 原始值 × HeightStep（厘米）。
//   - 查询全部是整数运算，客户端按同一公式可逐位复现（client/spec/terrain-queries.json 是金样）。
package terrain
