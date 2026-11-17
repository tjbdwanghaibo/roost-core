# v1.54.0 发版前的压缩性能测量（2026-11-18）

维护者 2026-11-17：“性能测量先压缩，跑完后就可以发版了”。这一轮只跑三项：

1. relay 快照缺省压缩 deflate 的一次测量（交接 C4“先量一次”）；
2. M-2 移动每拍分配（`BenchmarkMoveBeatAllocations` 六轮）；
3. game-demo 机器人 100 / 200 人同秒起跑（KNOWN-LIMITS“机器人负载：100 / 200 人同秒起跑不通过”）。

M-1、M-3～M-11 和机器人爬坡四档这轮都没跑。

## 环境

- 机器：macOS arm64，10 核，32 GiB。源码为 main `b0ce0b5a`，加上未提交的 `client/relay/compress_bench_test.go`（随本证据一起提交）。Go 1.27.3。
- **不是空闲机**：
  - 负载 3～6：同机有维护者别的工程，以及常驻的 `~/.roost-it` 隔离环境。
  - swap 已用约 7.6 GiB（共 9 GiB）。
  - 各项的 `host-*.txt` 记了开始和结束时的负载。

## 1. deflate 快照压缩

用例：`client/relay/compress_bench_test.go` 的 `BenchmarkSnapshotCompression`。三种载荷：

- **json**：房间状态 JSON，成员数组带号、名字、坐标、血量、背包；
- **packed**：同样的状态排成定长小端整数，坐标是随机游走；
- **random**：随机字节。

四档大小，到 relay 的快照上限 256KiB。原始数据：

- `deflate-bench.txt`：`-count 6`，取中位数；
- `deflate-ratio.txt`：压缩率。第一次跑时压缩率指标报在 `b.Loop` 之前，被计时器重置清掉了；改到循环之后另跑了 `-benchtime 20x`。

| 载荷 / 大小 | 压缩后 / 原始 | 压缩 | 解压 | 压缩每次分配 |
|---|---|---|---|---|
| json 1KiB | 0.280 | 57 µs | 5.6 µs | 1.08 MB / 16 次 |
| json 16KiB | 0.204 | 94 µs | 31 µs | 1.08 MB |
| json 64KiB | 0.199 | 0.21 ms | 0.11 ms | 1.11 MB |
| json 256KiB | 0.200 | 0.77 ms | 0.47 ms | 1.21 MB |
| packed 16KiB | 0.350 | 0.12 ms | 52 µs | 1.09 MB |
| packed 256KiB | 0.334 | 1.34 ms | 0.80 ms | 1.34 MB |
| random 256KiB | 1.000 | 0.10 ms | 51 µs | 1.59 MB |

结论：

- **压缩率**：JSON 状态压到约 1/5，二进制状态约 1/3，随机字节压不动；random 档只多出 deflate 自己的几个字节。
- **耗时**：256KiB 一次压缩约 0.8～1.3 ms。快照最小间隔 1s，一秒最多压一次，耗时不是问题。
- **分配**：每次压缩新建 `flate.Writer`，固定约 1 MB 分配，1KiB 的小快照也一样。真要省，可以复用 Writer（`sync.Pool` 加 `Reset`），这一版没改。
- **结论不变**：“缺省 deflate，SDK 不带 zstd / LZ4”这条照旧。zstd / LZ4 没测，由游戏按 README 的示例自带。
- **其他 SDK 没测**：C# `DeflateStream` 和 TS 的压缩实现没有单独测。

## 2. M-2 移动每拍分配

`go test ./gameplay/movement -run '^$' -bench MoveBeatAllocations -benchmem -count 6`，原始数据见 `m2-move-beat.txt`。六轮结果几乎不变：

| 子项 | ns/op | B/op | allocs/op |
|---|---|---|---|
| empty（同一个空事务的底数） | 352.6～359.8 | 945 | 5 |
| advance（事务里推进一拍移动） | 805.5～810.1 | 1885 | 17 |

**移动本身每拍：12 次分配、940 B、约 450 ns**（advance 减 empty）。分配来源没跑 `-memprofile`，没有拆。

## 3. 机器人 100 / 200 人同秒起跑

入口 `scripts/perf/demo-robot.sh`，每档用全新私有依赖和全新工程，`DEMO_ROBOT_MIRROR_OFFSET=20000`，单 IP 上限 512，门槛沿用正式 CLI：错误率 0、drop 0、整场景 p95 16s。原始数据在 `robot-100-burst/`、`robot-200-burst/`。

| 档 | 成功 / 总数 | 整场景 p50 / p95 | 门槛 | 日志 |
|---|---|---|---|---|
| 100 同秒 | **100 / 100**，失败 0 | 20.7s / 24.8s | p95 超 16s | saga 卡住 0，`nest: sync timeout` 0 |
| 200 同秒 | 80 / 200，失败 120 | 18.9s / 21.7s（只算成功的 80 人） | 错误率、p95 都超 | `nest: sync timeout` 108 次，都在 AddExp（Player + World）|

- **100 人档**：每类消息都全部成功、零超时，最慢的是 AddExp（10001），p95 2.3s、最大 2.4s；其余消息 p95 都在 0.31s 以内。整场景 20s 以上的耗时大部分不在单个请求上，而在场景里等 saga 和轮询。这一点是从“请求快、场景慢”推断的，没有逐步计时，**尚未确认**。
- **200 人档**：失败形态与 2026-11-14 那次（72 / 200，`add_exp` 在 World 热点上 sync timeout）相同，这次 80 / 200。AddExp 同时锁 Player 和 World，日志分不出卡在哪把锁。
- **对比历史**：
  - 2026-10-22：24 / 100、30 / 200。
  - 2026-11-14：96 / 100、72 / 200。
  - 这一轮：100 / 100、80 / 200。

  三次机器负载不同，不据此判断改善的幅度。
- **两次端口冲突作废**（原始目录没提交）：
  - 第一次用默认偏移 37000，端口 64119 落在 macOS 的临时端口段（49152 以上），正被常驻环境 mongod 的一条出站连接占用。
  - 200 人档第一次启动时，47118 还被刚收尾的 100 人档占着，换新目录重跑。

  两次都在起服之前退出，没有产生负载数据。默认偏移 37000 会把私有 Mongo 端口（27117～27405 加偏移）放进临时端口段，以后要换成不大于 21746 的偏移。这一条已写进 KNOWN-LIMITS。

## 没做的

- 机器人爬坡四档（ramp20 / ramp10 × 100 / 200）、空闲机器上的复跑、Linux / Colima 复现：维护者要求先压缩，这一轮没跑。
- M-1、M-3～M-11 照旧待测。
