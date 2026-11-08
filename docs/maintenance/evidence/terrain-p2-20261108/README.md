# 可编辑地形 P2 服务端（单格）的实测（2026-11-08）

服务于[可编辑地形方案](../../FEATURE-2026-11-08-editable-terrain.md#14-p2-服务端单格实施结果2026-11-08)第 14 节。测的是 game-demo 生成工程里的正式代码：笔刷（`gameplay/terrain`）、`terrainmap.Map.Brush`、生成的 Nest handler、`TerrainChunk` 的同步 packer、场景的地形同步（`terrain_sync.go`）、帧编码与玩家 TCP 接入；客户端是测试里的 Go 接收端（`frame.Receiver` + 生成的 `ApplySyncTerrainChunkDao`，与 C# 同一协议）。

## 边界

- 单机：Apple M5（10 核）、macOS 26（Darwin 25.5.0 arm64）、go1.27.3，见 [env.txt](env.txt)。机器有常驻后台负载（1 分钟负载约 3～4）。
- 存储是离线提交器（`terrain_test.go` 的 `memoryChunkStore`：收下记录、不写 WAL / Mongo）。**耗时数字不含 WAL fsync 与投影**；strict 持久化下真实单笔延迟会多出 WAL 落盘时间，本轮没有测。
- 同步走真实回环 TCP，但服务端与客户端在同一进程；字节是精确的，时延不是测量对象。
- 下行模拟不起复制循环：模拟时间按“拍”走，一拍 = 50ms = 一次 `Flush`，每拍之前做这一拍的编辑 / 移动（同一块在同一拍里的多次编辑合成一个增量，与真实 50ms 合批相同）。随机数种子固定，三轮的字节数逐字相同。
- 地形是 `terrain.Generated`（种子 20261108）的 16 × 16 块小图；字节数与地形内容无关（同步是未压缩的 DAO PB），与改动的点数有关。

## 复跑

```sh
# 生成工程（replace 到当前检出），见 demo/README.md“改这里的东西之后”
ROOST_TERRAIN_MEASURE=1 GOWORK=off go test -count=1 -run 'TestTerrainMeasure' -v ./internal/service/<game>/
# 笔刷纯计算
GOWORK=off go test -count=3 -run '^$' -bench BenchmarkBrushApply -benchmem ./gameplay/terrain
```

原始输出：[measure-run1.txt](measure-run1.txt)、[measure-run2.txt](measure-run2.txt)、[measure-run3.txt](measure-run3.txt)、[bench-brush.txt](bench-brush.txt)。

## 单块更新的字节（`TestTerrainMeasureUpdateBytes`）

口径：payload 是 DAO 同步 PB（方案 §7.1 的口径）；component 加上 EntitySync subject 头；frame 是整帧（一帧只有这一个对象）；wire 再加 16 字节包头（不含 TCP/IP 头）。

| 更新 | 改了的点 | payload | frame | wire | 形状 |
| --- | ---: | ---: | ---: | ---: | --- |
| 改过的块进入视野（全量） | — | 12,319 | 12,611 | 12,627 | 高度 + 材质整字段 |
| 抬高 r=5m 线性衰减 | 69 | 575 | 703 | 719 | 高度按下标增量 |
| 抬高 r=5m 常数衰减 | 81 | 671 | 799 | 815 | 高度按下标增量 |
| 抬高 r=16m 平滑衰减（玩家上限） | 717 | 5,759 | 5,887 | 5,903 | 高度按下标增量 |
| 改材质 r=5m | 81 | 4,117 | 4,245 | 4,261 | 材质整字段（D11） |
| 整平 r=24m（覆盖约 1,800 点 > 1000，整字段守卫） | — | 8,213 | 8,341 | 8,357 | 高度整字段 |

每点约 8.3 字节（下标 + 值 + PB 开销），与[离线实验](../terrain-sync-20261108/results.md)和 C# 夹具一致。方案 §9 的“r=5m 单块增量约 0.7KB、材质约 4.1KB”成立（wire 0.72KB / 4.26KB）。

## 一个观察者的下行（`TestTerrainMeasureDownlinkScenarios`）

| 场景 | 线上 KB/s | 其中 DAO payload KB/s | 更新数 | 方案 §7.1 推算 |
| --- | ---: | ---: | --- | --- |
| 建造热点：20 人各每秒 1 次 r=5m 编辑（±40m 内），不带材质 | 19.2 | 15.8 | 20s 内 9 全量 + 524 增量 | 约 17 |
| 同上，一半带材质（抬高 + 同处改材质） | 75.5 | 71.9 | 9 + 559 | 约 70 |
| 同上，全部带材质 | 132.6 | 128.9 | 9 + 565 | 约 124 |
| 跑图 6m/s，沿途全改过 | 8.1 | 8.0 | 30s 内 20 全量 | 约 8.6 |
| 跑图 6m/s，沿途改过 20% | 1.6 | 1.6 | 4 全量 | 约 1.7 |
| 骑乘 12m/s，沿途全改过 | 16.6 | 16.4 | 41 全量 | （推算按 15m/s 约 21，按速度折算约 17） |
| 骑乘 12m/s，沿途改过 20% | 4.1 | 4.0 | 10 全量 | — |

读法：实测与推算相差在 15% 以内（DAO payload 口径在 10% 以内）（线上多出的是每帧的帧头与 subject 头；热点里一次编辑平均产生约 1.3 个块更新）。热点的下行几乎完全取决于改材质的比例：每次改材质给每个观察者约 4.2KB × 覆盖块数，高度增量只占零头。跑图加载每秒约 0.6 块全量（6m/s）。

## 编辑耗时与吞吐（`TestTerrainMeasureBrushCost`、`BenchmarkBrushApplyPlayerRadius`）

`Map.Brush` 端到端：校验 → 限速 / 权限钩子 → 每块 `GetOrCreate`（块已存在）→ Nest 多实体请求（排序取锁）→ handler（取编辑前数据、笔刷计算、写 DAO、`RecordApplied`）→ 离线提交 → 提交后装视图 → 答复。Nest 4 个 short worker。

| 编辑 | n | 平均 | P50 | P99 | 最大 |
| --- | ---: | ---: | ---: | ---: | ---: |
| r=5m 线性，1 块 | 2000 | 76～84µs | 73～79µs | 203～253µs | 0.31～0.41ms |
| r=16m 平滑，落在四块交点（4 块，约 2,900 点） | 1000 | 574～588µs | 554～563µs | 1.24～1.34ms | 1.41～1.62ms |
| 整平 r=24m（整字段守卫） | 500 | 196～210µs | 179～188µs | 506～681µs | 0.74～0.81ms |

| 项 | 结果 |
| --- | --- |
| 单格吞吐：8 个编辑者各改自己的一列块（互不争锁），每人 500 次 r=5m | 37,800～44,800 次/秒（离线提交器） |
| 笔刷纯计算 `Brush.Apply`：r=5m 线性 1 块 | 2.6µs，2.2KB / 17 次分配 |
| 笔刷纯计算：r=16m 平滑 4 块 | 15.7µs，16.7KB / 49 次分配 |

读法：笔刷计算本身只占端到端的 3%；四块交点的编辑比单块慢约 7 倍，CPU 剖析显示 Nest 相关 CPU 只占采样的约 10%，时间主要在多实体请求的分派与交接（没有细查，记为未验证）。建造热点 20 次/秒的编辑在 short 池上不到一个 worker 的 2%。

## 未测

- 真实 WAL / Mongo 下的单笔编辑延迟与单格吞吐（strict 持久化每笔要等 WAL 落盘）。
- 多个观察者同时在热点时的服务端 CPU（每个观察者一份帧，编码按观察者分摊）与出口总量；本轮只测一个观察者收到的字节。
- 真实客户端（C# / Unity）在热点下的应用与 mesh 重建耗时。
- Linux。
