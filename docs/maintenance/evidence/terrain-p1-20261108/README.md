# 可编辑地形 P1 框架部分的实测（2026-11-08）

服务于[可编辑地形方案](../../FEATURE-2026-11-08-editable-terrain.md#11-p1-框架部分实施结果2026-11-08)的 P1 框架部分（`gameplay/terrain`，[包说明](../../../../gameplay/terrain/README.md)）。

## 边界

- 单机：Apple M5（10 核）、macOS 26（Darwin 25.5.0 arm64）、go1.27.3，见 [env.txt](env.txt)。机器有常驻后台负载（1 分钟负载约 2～3.5）。
- 地形是包内的确定性程序化生成器（`Generated`，种子 20261108，含 ±20cm 逐点抖动），不是美术高度图；压缩率只作参考。
- 包刚写完就读，**读块耗时是页缓存热的**；冷盘读没有测（macOS 上清页缓存要 root）。
- 1w × 1w = 157 × 157 = 24,649 块；“一个格”取块 [48, 96) × [48, 96)，即 48 × 48 = 2304 块。

## 复跑

```sh
# 规模测量（写包约 70MB 到 ROOST_TERRAIN_SCALE_DIR 或临时目录，结束删除），连同编码对比
ROOST_TERRAIN_SCALE=1 GOWORK=off go test -count=1 -timeout 0 -run 'TestScale' -v ./gameplay/terrain
# 微基准（8 × 8 块的小地图）
GOWORK=off go test -count=3 -run '^$' -bench . -benchtime 2s ./gameplay/terrain
# 用独立实现核对客户端金样
python3 -I docs/maintenance/evidence/terrain-p1-20261108/check_golden.py client/spec
```

## 1w × 1w 整图（三轮，[scale-run1.txt](scale-run1.txt)、[scale-run2.txt](scale-run2.txt)、[scale-run3.txt](scale-run3.txt)）

| 项 | 结果 |
| --- | --- |
| 包文件大小 | 70,061,217 字节（66.8 MiB），含包头与索引平均每块 2,842 字节；索引表 1.08MB |
| 生成 + 写包（`WritePackFile`，10 路并行取块与压缩） | 2.34～2.40s |
| 打开（读包头 + 24,649 项索引并整体 SHA-256） | 中位数 477～524µs（每轮 7 次） |
| 单块加载 `ReadChunk`（ReadAt + SHA-256 + 解压 + 解码，5000 个随机块） | P50 38.5～39.9µs，P99 161～165µs，最大 199～222µs |
| 装满一个格（2304 块 `LoadBase`），单线程 | 119～120ms |
| 装满一个格，8 路并行 | 28～29ms |
| 装满一个格的堆增量 | 27.3 MiB（每块约 12,441 字节 = 12,288 字节数据 + 结构；另有 View 槽位数组 24,649 × 8 字节） |
| 满格上随机位置查询，单 goroutine（100 万次） | `Height` 22～25 ns，`Probe` 19～31 ns |

## 微基准（[bench.txt](bench.txt)，count=3）

| 基准 | 耗时 | 分配 |
| --- | ---: | ---: |
| `EncodeChunk`（平面预测 + DEFLATE best） | 228µs | 1.16MB / 21 次 |
| `ReadChunk`（页缓存热） | 40.9µs | 75.8KB / 35 次 |
| `Height`（单 goroutine，块在缓存里） | 9.2ns | 0 |
| `HeightParallel`（10 路并行，合计） | 1.57ns | 0 |

**锁的选择**：View 起初用“map + 读写锁”（[rwmutex/](rwmutex/)，那一版代码没有提交）：`Height` 单线程 24.8ns，10 路并行合计 109.8ns，即并行时总吞吐反而低于单线程（读锁计数的缓存行在核间来回）。现在的块表是按块序号排开的原子指针数组，查询不取锁，单线程 9.2ns、10 路并行合计 1.57ns。整图规模测量两版的装格耗时与包大小相同（同一份写包与读块代码）。

## 块编码对比（[codec-choice.txt](codec-choice.txt)）

1w 地图上 8 个块的平均字节（高度 + 材质）：原始小端 + DEFLATE 6,554；只用左邻差分 + DEFLATE best 4,078；只用下邻 4,010；**平面预测 + DEFLATE best 2,733（采用）**；平面预测 + DEFLATE default 2,817。上一轮实验（[terrain-sync-20261108](../terrain-sync-20261108/results.md#3-持久化大小)）的“行差分 varint + flate 约 4.1KB”（只算高度、另一种合成地形）与这里的左邻差分同量级；平面预测再小约三分之一。程序化材质成片分布，真实材质图的压缩率未测。

## 客户端金样核对

[check_golden.py](check_golden.py) 只按包说明与 `pack.go` 头注释独立实现了读包（包头、metaSHA256、每块 SHA-256、raw inflate、平面预测还原）与查询公式，对 `client/spec/terrain-golden.pack` + `terrain-queries.json`：6 块的解码 SHA-256 全部一致，246 个位置（含 5 个地图外）的高度 / 材质 / 水全部一致（`queries 246 mismatches 0`）。

## 未验证

- 冷盘读、Linux、其他 CPU。
- 美术高度图的压缩率与 RAW 导入耗时（RAW 导入只有单测的正确性覆盖）。
- C# / C++ 的读包与查询实现（P2）。
- 与 Nest / DataEngine / entitysync 的接入（下一轮在 demo 里做）。
