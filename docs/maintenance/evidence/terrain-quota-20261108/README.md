# 地形编辑配额与材质守卫 500 的重测（2026-11-08）

服务于[可编辑地形方案](../../FEATURE-2026-11-08-editable-terrain.md#17-编辑配额与材质守卫-500-实施结果2026-11-08)第 17 节。维护者 2026-11-08 决定：材质整字段守卫 1000 → 500；每玩家编辑限流从“每秒 2 次”改为“每人每秒 300 个改动点”（高度、材质各算）。测量方法、脚本与边界沿用 [P2 证据](../terrain-p2-20261108/README.md) 与[材质改列表证据](../terrain-materials-list-20261108/README.md)：game-demo 生成工程里的正式代码（笔刷、`Map.Brush` 与配额、生成 handler、`TerrainChunk` 同步 packer、场景地形同步、帧编码与玩家 TCP 接入），客户端是同进程的 Go 接收端。

## 边界

- 单机：Apple M5（10 核）、macOS 26（Darwin 25.5.0 arm64）、go1.27.3，见 [env.txt](env.txt)。机器有常驻后台负载（1 分钟负载约 4.3）。
- 存储是离线提交器（不写 WAL / Mongo）；同步走真实回环 TCP，字节精确。
- 下行模拟一拍 = 50ms = 一次 `Flush`，同一块在同一拍里的多次编辑合成一个增量。配额用模拟时钟（拍号 × 50ms），与真实时间无关；随机数种子固定，三轮的字节、点数与次数逐字相同（只有耗时行不同）。
- “20 人在附近”是方案的假设，不是实测的玩家分布；真实分布要等 P5 的机器人测。

## 复跑

```sh
# 生成工程（--skip-deps + replace 到当前检出），见 demo/README.md“改这里的东西之后”
ROOST_TERRAIN_MEASURE=1 GOWORK=off go test -count=1 -run 'TestTerrainMeasure' -v ./internal/service/<game>/
```

原始输出（只留 MEASURE 行）：[measure-run1.txt](measure-run1.txt)、[measure-run2.txt](measure-run2.txt)、[measure-run3.txt](measure-run3.txt)。

## 配额满速的建造热点（`TestTerrainMeasureDownlinkScenarios/quota_*`）

做法：观察者站在块 (8,8) 中心；附近 20 人每拍都尝试自己的下一笔，配额允许就做、同一拍里接着做下一笔，被拒（`ErrRateLimited`）就等下一拍。一轮是 ±40m 内随机一处的一笔抬高（或抬高 + 同处改材质），做完换一处；笔刷线性衰减、强度 5（与原热点档相同），材质编号 = 拍号（< 128 与 ≥ 128 都有）。热点里的 9 块先改过一次、全量在预热里发完；每人在头 3 秒内随机一拍开工，再预热 2 秒，跳过开头每人一满桶加一次透支的突发——测的是**稳态**。

| 笔刷组合 | 窗口 | 线上 KB/s | DAO payload KB/s | 块更新 / 秒 | 计入配额的点 / 秒（每人） | 编辑 / 秒 | 对照 50KB/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| r=5m 抬高 | 20s | **54.1** | 47.0 | 65.9 | 5,996（300） | 94.3 | **超 8%** |
| r=5m 抬高 + 改材质 | 20s | **53.4** | 46.8 | 60.8 | 6,019（301） | 86.0 | **超 7%** |
| 同上 | 60s | **53.1** | 46.4 | 61.6 | 6,001（300） | 86.2 | **超 6%** |
| r=16m 抬高 | 20s | 47.9 | 45.8 | 18.7 | 5,991（300） | 9.2 | 内 |
| r=16m 抬高 + 改材质 | 20s | 48.8 | 46.4 | 21.8 | 5,929（296） | 8.3 | 内 |
| 同上 | 60s | 49.4 | 47.2 | 19.3 | 5,965（298） | 8.3 | 内 |
| 混合（四种人各 5 个：r=5m 抬高 + 改材质、r=16m 抬高、r=16m 抬高 + 改材质、r=5m 抬高） | 20s | **52.3** | 47.0 | 47.2 | 6,018（301） | 49.2 | **超 5%** |

读法：

- **配额生效**：每人稳态正好约 300 点 / 秒，与笔刷大小无关；r=16m 的编辑每人约每 2.2～2.7 秒一笔（满桶透支后等补满）。
- **字节并不只随点数走**：payload 每点约 7.8～8.0 字节（高度每点约 8.3、材质约 7.3，r=16m 改材质常走整字段），另外每次块更新在线上约多 110～115 字节（帧头、组件头、包头）。小笔刷每个点摊到的块更新多，所以 r=5m 档线上比 r=16m 档多约 5KB/s。拟合：线上 ≈ 8.0 × 点数 + 110 × 块更新数（字节 / 秒）。
- 方案按每点 7.5 字节推算的 20 × 300 × 7.5 ≈ 45KB/s 只覆盖 payload 的大头；**r=16m 档在预算内（47.9～49.4KB/s），r=5m 档超约 6～8%（53.1～54.1KB/s）**。要让所有档都在 50KB/s 内的做法见方案第 17 节“需要维护者决定”。下面两组是为此临时改生成工程重跑的对照（没有提交，未入三轮原始输出）：
  - 配额降到 270 点 / 秒：r=5m 抬高 49.6、r=5m 抬高 + 改材质 48.3（60s 48.2）、r=16m 抬高 43.3、r=16m 抬高 + 改材质 43.5（60s 43.9）、混合 47.7 KB/s。
  - 配额仍 300，但每次编辑按覆盖到的块每块另计 15 点（摊每次块更新约 110 字节的固定开销）：41.3 / 41.5（60s 41.3）/ 45.5 / 44.0（60s 45.4）/ 43.9 KB/s。
- **突发没有算进稳态**：每人开头有一满桶 300 点、满桶还能透支一笔（玩家上限 r=16m 抬高约 650～720 点、改材质约 750～800 点）。20 人同一秒开工、都用 r=16m 抬高 + 改材质时，第一秒约 20 ×（300 + 一笔）≈ 2 万点，约 160KB 一次性到达（推算，没有单独测）；之后回到稳态。

## 不限配额的原档（与第 16 节对照）

| 场景 | 本次 | 第 16 节（守卫 1000） |
| --- | ---: | ---: |
| 热点 r=5m（20 人各 1 次/秒）：材质 0 / 50% / 100% | 19.2 / 25.1 / 30.6 KB/s | 相同 |
| 热点 r=16m 各 1 次/秒：材质 0 / 100% | 112.5 / 231.4 KB/s | 112.5 / 227.5 |
| 跑图 6m/s 全改过 / 20%；12m/s 全改过 / 20% | 8.1 / 1.6；16.6 / 4.1 KB/s | 相同 |

“热点 r=16m、全部带材质”多了 3.9KB/s：r=16m 改材质现在走整字段，而热点里材质编号 = 拍号，块里编号 ≥ 128 的点越来越多，整字段每个这样的点 2 字节（全部 ≥ 128 时约 8.2KB），比逐点补丁省得少、个别时候更大。这一档每人每秒约 1,500 点，配额下到不了。

## 单块更新（`TestTerrainMeasureUpdateBytes`，材质守卫 500 的影响）

| 更新 | 改了的材质点 | 本次 payload | 第 16 节（守卫 1000） |
| --- | ---: | ---: | ---: |
| 改材质 r=10m | 317 | 2,242（按下标） | 2,242 |
| 改材质 r=16m（玩家上限），编号 21 | 751 | **4,151（整字段）** | 5,280（按下标） |
| 改材质 r=16m，编号 200 | 797 | **4,948（整字段）** | 6,399（按下标） |
| 改材质 r=24m（守卫） | 约 1,800 | 4,117（整字段） | 4,117 |

其余行（全量 12,319、抬高 r=5m 575、抬高 + 改材质 1,150～1,231、整平守卫 8,213、持久整文档 79,951 字节）与第 16 节逐字相同。持久化：r≈13～16m 的改材质由逐点 `$set`（约 12 字节 / 点，6～10KB）变为整字段写（约 40KB，按 P2 实验的同形状推算，没有单独测）。

## 编辑耗时（`TestTerrainMeasureBrushCost`）

配额开着时 `Map.Brush` 在事务之前多做一步：在视图上数这笔会改变的点（`View.BrushChanges`，不复制块数据、只读）。同一进程先测关、后测开（没有交替多轮，量级参考）：

| 编辑 | 配额关 P50（三轮） | 配额开 P50（三轮） | 差 |
| --- | ---: | ---: | --- |
| r=5m 线性，1 块 | 80.8～82.4µs | 84.0～86.8µs | 约 +2～6µs（+2～7%） |
| r=16m 平滑，四块交点 | 584～607µs | 603～619µs | 约 +12～19µs（+2～3%） |

## 测试能变红

在生成工程副本上改坏实现后跑 `go test -run 'TestTerrainBrushQuota|TestTerrainChunksReachAClient' ./internal/service/<game>/` 与 `go test -run TestViewBrushChanges ./gameplay/terrain/`，每一处都变红（失败原文摘录）：

| 改坏 | 结果 |
| --- | --- |
| 不许透支（余额必须够） | `an r=16m raise (717 points) on a full bucket: terrain: edit point quota used up: player 4242: the edit changes 717 points` |
| 余额为正就许透支（不要求满桶） | `a 177-point edit with 118 left: <nil>, want ErrRateLimited`；`a small edit after the overdraft (0.3 points short): <nil>, want ErrRateLimited` |
| 重复请求不退回 | `the corner paint once the quota is back: {Duplicate:false Chunks:[]}, terrain: edit point quota used up: player 4242: the edit changes 177 points` |
| 领地钩子拒绝不退回 / 建筑占位拒绝不退回 | 同上一行 |
| 没有每次至少 20 点 | `the corner paint (0.3 points short): <nil>, want ErrRateLimited` |
| 点数少算（按一半计） | `a 177-point edit with 118 left: <nil>, want ErrRateLimited` |
| 先建实体、后扣配额 | `a 177-point edit with 118 left: an edit refused for its quota created chunk entity 4398046512174` |
| 材质守卫留在 1000 | `an r=16m paint went out as {… payloadBytes:5602 … materialsDelta:true materialUpdates:797}, want the whole materials field (over the 500-point guard)` |
| 材质守卫降到 400 | `an r=12m paint went out as {… payloadBytes:4117 … materialsDelta:false materialUpdates:0}, want per-index materials under the guard` |
| `BrushChanges` 把抬高覆盖到的点都算成改了 | `raise r=1600: BrushChanges 797 heights / 0 materials (<nil>), Apply changed 717 / 0` |
