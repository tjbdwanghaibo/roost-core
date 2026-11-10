# 可编辑地形 T03 客户端岸线与 T04 重建调度（算法层）证据

分支 `feat/terrain-client-shore`，基于 main `7c5e6a90`。设计、规则、误差与待确认见[方案 §31](../../FEATURE-2026-11-08-editable-terrain.md#31-客户端岸线与重建调度t03--t04-算法层2026-11-10)，进度见 [TERRAIN-STATUS](../../TERRAIN-STATUS.md) T03 / T04。本机 macOS 26.5、Apple M5、.NET SDK 10.0.302（测试程序 `net10.0`，运行库 `netstandard2.1`）、Go 1.27.3。没有连接任何数据库或真实依赖；没有在 Unity Editor / IL2CPP / 设备上运行（验收后置）。

## 1. 先红后绿

新功能没有“修前”版本，红证据来自变异：把实现里的一条规则去掉或改错，跑同一组 C# 测试，记录第一条失败原文，然后还原（脚本只改工作区文件、跑完逐字节还原）。原文全部在 [mutations.txt](mutations.txt)（三轮，最后一次按最终测试整体重跑）。

| 变异 | 结果（失败原文摘要） |
| --- | --- |
| M1 鞍点总按连通 | 红：`separated saddle: two polygons` |
| M2 鞍点恰在等高线上算水 | 红：`tied saddle: water corners separated` |
| M3 等于水位的采样算水（F = 2(H − L) − 1） | 红：`crossing next to a sample at the water level is 1/20 of a cell away from it` |
| M4 侧壁交点从另一端算（数学等价） | 没变红：这组数据上两种算法逐位相同。改为 M4b |
| M4b 侧壁交点偏 1e-9cm（不走规范函数） | 红：`chain end (3856.4285714285716, 12800) of (0,1) closes on a seam (Top)` |
| M5 / M7 / M10 第一版写法 | 没编译过（不可达代码告警按错误），改为 M5b / M7b / M10b |
| M5b 不细分 | 红：`circle: tolerance met within depth (max 0.3627710595056452)` |
| M6 误差在参数中点量（不是精确最大点） | 红：`exact contour point in cell (28,34) is 0.2425038480795984cm from the shoreline, bound 0.24249857858857277` |
| M7b 忽略邻块水位（不出侧壁） | 红：`all-water chunk: map-edge and level seams, none towards a same-level neighbour` |
| M8 LOD 块边用本块步长 | 红：`lod 0|1: shared edge uses the same vertices` |
| M9 读取表漏右下邻块 | 红：`InvalidOperationException: build (0,1): sample (64, 63) needs chunk offset (1, -1), which was not captured` |
| M10b 发布不查接缝 | 红：`edge edit: nothing published with 3 of 4 rebuilds outstanding, got (0,1)` |
| M11 新鲜度不比被读采样 | 红：`edge edit: the old mesh stays shown and is reported expired` |
| M12 只去掉收回时的会话代际检查 | 没变红：发布前的新鲜度校验同样查会话代际（防御重叠） |
| M12b / M12c 两处都去掉 | 红：`reconnect: the job from the previous session is not published, though its data did not change since`（第一版测试把旧任务放在重连之后执行，被“变脏即取消”拦下，M12b 红在过期报告上；改测试为重连前算完，M12c 红在发布上） |
| M13 只去掉收回时的地形代际检查 | 没变红：同 M12 |
| M13b 两处都去掉 | 第一、二版测试没变红：换版用例的新包高度不同（数据校验拦下）、旧任务在换版后才执行（被取消）。改为“只换水位、高度逐点相同，旧任务换版前算完”后红：`switch: results of the old terrain are not published, though the heights are identical` |
| M14 只在派发处不要求确认 | 红：`scheduler did not settle`（派发与发布规则不一致，空转）；M14b 两处都去掉：`late neighbours: base data of an unconfirmed neighbour is not used` |
| M15 卸下不撤销确认 | 红：`reload: the base chunk is not used before it is confirmed` |
| M16 不限在途 | 红：`bounded: executor backlog 6, in flight 6` |
| M17 待重建不去重 | 红：`late neighbours: published once the overlay is confirmed` |
| M18 去掉“最新派发”检查 | 没变红：被取代的任务在变脏时已标记取消（没有结果），或读的数据已过期被新鲜度拦下；数据回到原样时旧结果与当前数据一致，发布它不是“旧结果”。保留作防御，不计为有红证据的保证 |
| M19 变化矩形不用（整块都算变了） | 红：`edge edit: nothing published with 2 of 4 rebuilds outstanding, got (2,0)`（只改块内部时邻块不重建的性质） |
| M20 LOD 变化不带四邻 | 红：`lod change: the chunk and its three in-map edge neighbours rebuild (1)` |
| M21 坡度改成单侧差分 | 红：`central difference` |

第一次写调度器测试时发现并修了一处实现问题：调度器接上视图之前就装着的块没有“已看到”的数据，第一次编辑按整块变化处理，多重建了两个邻块（`edge edit … got (2,0)`）。改为派发时以读到的数据为基准（`TerrainRebuildScheduler.Dispatch`）。另有一条测试期望写错（连通鞍点期望两条闭合岸线，实际是一片水一条岸线），改了测试。

## 2. 验证命令与结果

仓库根（worktree）：

| 命令 | 结果 |
| --- | --- |
| `dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec` | 通过，输出见 [verify.txt](verify.txt)（第一次与变异脚本并行跑撞上了变异中的源码，已在变异结束后重跑） |
| `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` | 通过 |
| `sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh` | **漂移，基线上就有**：`dao_sync_apply.cs`、`terrain-chunk-sync.json` differs from a fresh generation。在 main `7c5e6a90` 的干净检出上同样漂移；差异是 `578fa31f`（DAO 增量不带实体 ID）后生成 Apply 的身份检查改为“增量可以不带实体 ID”、夹具没有重新生成。属于同步编解码那一轮，本轮没有改 DAO 夹具、没有 `-update`；现有提交的夹具照常通过 C# 测试 |
| `GOWORK=off go test -count=1 ./gameplay/terrain`、`go vet` | 通过（含新的 `TestClientShoreGolden`：从提交的包重新算 JSON 逐字节相同） |
| `GOWORK=off go test -count=1 .` | 通过 |
| `python3 scripts/check-doc-anchors.py` | 通过 |

岸线金样由 `GOWORK=off go test ./gameplay/terrain -run TestClientShoreGolden -update` 生成（包 1,725 字节、JSON 28,486 字节）；平时不带 `-update` 只核对。

## 3. 关键数字（C# 测试输出）

| 项 | 数字 |
| --- | --- |
| 圆形池塘（半径 7.3m） | 缺省外的细参数 0.25cm / 8 层：49 个点、最大误差 0.2425cm；不细分 46 点、0.363cm；精确等高线上 26,934 个点都在误差内；1cm 格点上视觉与权威不同 150 个，全部在误差带内 |
| Go 金样窗口 | 7 个窗口共 70,000 点 C# `IsWater` 与 Go 逐点相同；视觉分类不同 70 个，全部在误差带内 |
| 全图扫描 | 9 块、岸线格逐 1cm、其余每 13cm，共 5,727,600 点；视觉与权威不同 6,079 个，全部在 `MaxDeviation` 以内；全图最大误差 0.9628cm（缺省 1cm / 4 层，无超限段） |
| 跨块 | 开放岸线端点 12 个在两块之间逐位相接，6 个落在侧壁端点上（区域水位边界两侧、无水区域边界） |
| LOD | 6 × 7 种级别组合铺满整块、公共边顶点相同、无 T 接缝 |
| 读取表 | 右上 / 左 / 上 / 左上 / 右邻块声明读取范围外的 20,155 个采样改掉后，重建结果逐位不变 |
| 调度随机 4000 步 | 在途上限 2：派发 579、发布 116 块 / 122 批、丢弃 460；执行器积压从未超过 2；每批都满足“与当前数据一致、当前代际、显示中的相邻块接缝一致” |

## 4. 未覆盖

见方案 §31.8：Unity 表现与验收、IL2CPP / 设备、性能基准、真实 entitysync 链路上的确认来源（§31.7 ③ 待维护者确认）、编辑预测预览分层、粗 LOD 下的视觉偏离、跨块岸线 UV。
