# Sync 性能基准

用户目标负载（1000 玩家 / 10000 实体 / AOI 每人约 50 可见）的实测见 [AOI 负载验证](SYNC-AOI-1000-10000-2026-09-23.md)，入口为 `./scripts/perf/sync-aoi.sh`。

业务链路负载（生成 demo、真实 Nest 事务、AOI、BSON 和本机 TCP）另见 [Sync 业务负载验证](SYNC-BUSINESS-LOAD-2026-09-23.md)。这里保留独立微基准口径，两类结果不直接混比。

## 一条命令

在 roost-core 根目录运行：

```bash
./scripts/perf/sync.sh
```

默认 `-run '^$' -bench . -benchmem -benchtime=1s -count=5 -cpu=1`，20 分钟超时。结果写入 `artifacts/perf/sync/<时间>.txt`，旁边 `.env.txt` 记录 Go/平台、提交、工作树状态和参数。该目录已加入 .gitignore，保存本地生成物，不提交原始机器数据。每次 Go 标准基准会自动校准迭代数；新基准使用当前工具链支持的 `testing.B.Loop` 排除初始化和清理。

快速检查入口：

```bash
ROOST_PERF_COUNT=1 ROOST_PERF_BENCHTIME=200ms ROOST_PERF_LABEL=smoke ./scripts/perf/sync.sh
```

## 覆盖与指标

| 基准 | 测量内容 | 不包含 |
| --- | --- | --- |
| `BenchmarkManagerFlush` | 1/64/256 会话 × 32 个 dirty subject，1/4 profile，256 字节 payload；MarkDirty、捕获、共享组件编码、会话组帧与内存准入 | 建连、网络、真实业务 packer、实际客户端 |
| `BenchmarkFrameCodec` | 1/32/256 对象 × 256 字节组件的帧编码与解码；256 场景显式提高 MaxObjects | 业务内容捕获、传输 |
| `BenchmarkClusterFlushRandomWalk` | 四个区域、1000 subject、100 observer 的空间变化与事件输出 | Manager 交付 |
| `BenchmarkRedundantEncodePush` | lockstep 冗余输入帧编码 | 实际网络 |
| `BenchmarkRoomTickTenPlayers` | 十人房间 Tick 与内存传输 | 真实客户端确定性模拟 |

Go 输出的 `ns/op` 越小越好，`B/op` 是堆分配，`allocs/op` 是分配次数。Manager 另外报告 `frames/op`、`packs/op` 与 `wire-B/op`，确保优化前后工作量相同。一次 op 是一整个 tick；`packs/op` 是业务 packer 调用数，不是组件序列化次数。共享组件编码不会减少会话外层帧或网络字节，因此不能把内存分配下降解释为带宽下降。

## 同机对比

用相同 Go 工具链、CPU 参数、场景、bench 时间和机器环境，依次在两个版本运行；避免同时构建或压测其他工程。至少取 5～10 个样本；正式结论建议延长单样本时间。比较包含行为改动时，先确保帧数、pack 数和字节数等工作量指标一致。

```bash
ROOST_PERF_LABEL=before ROOST_PERF_COUNT=10 ROOST_PERF_BENCHTIME=2s ./scripts/perf/sync.sh
# 切换到待比较版本，保留相同基准源码和运行条件
ROOST_PERF_LABEL=after ROOST_PERF_COUNT=10 ROOST_PERF_BENCHTIME=2s ./scripts/perf/sync.sh
benchstat artifacts/perf/sync/before.txt artifacts/perf/sync/after.txt
```

`benchstat` 属于 `golang.org/x/perf/cmd/benchstat`，不是 Go 自带程序；未安装时可先保留原始样本，在已配置 benchstat 的环境分析。本轮不自动安装额外工具。简单中位数只能作为观察，不能替代显著性检验。

## CPU 与堆分配定位

先单独运行目标场景，不开 race，不同时跑其他基准。使用 `-o` 将测试二进制一起留在输出目录，保证之后的 pprof 符号定位对应同一次构建：

```bash
mkdir -p artifacts/perf/sync
go test ./sync/entitysync -run '^$' \
  -bench '^BenchmarkManagerFlush$/sessions=256/subjects=32/profiles=1/' \
  -benchtime=3s -count=1 -cpu=1 \
  -o artifacts/perf/sync/entitysync.test \
  -cpuprofile artifacts/perf/sync/cpu.out \
  -memprofile artifacts/perf/sync/mem.out
go tool pprof -top artifacts/perf/sync/entitysync.test artifacts/perf/sync/cpu.out
go tool pprof -top -alloc_space artifacts/perf/sync/entitysync.test artifacts/perf/sync/mem.out
```

堆 profile 默认包含该测试进程采样到的初始化分配；不要把其总量当作 `B/op`。要查看调用图，可把 `-top` 换成 `-http=127.0.0.1:0`。更改 `ROOST_PERF_CPU` 可比较不同 GOMAXPROCS，但当前 Manager 基准是串行 Flush，CPU 数不等于客户端并发数。

## 本轮实测

本轮先采集无缓存基线，再使用同一个基准采集共享编码后的结果。精确参数与结果追加于此；它是本地 CPU/内存微基准，不是上线容量、p99 延迟或真实网络吞吐结论。NATS/JetStream 的吞吐需要真实 broker、消息大小/扇出、ACK 与持久策略；网络传输基准需要真实连接与慢消费者场景，不能用内存 sink 替代。

环境：Apple M5、darwin/arm64、Go 1.27.0，`-cpu=1 -benchtime=300ms -count=5`；下表取五个样本中位数。基线已包含本轮正确性修复与多来源订阅，只对比组件编码复用这一步，不是对比 `967bc69` 或合仓前后性能。

| 会话 / profile | 基线 μs/op | 共享编码 μs/op | 时间变化 | B/op 变化 | allocs/op 变化 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 / 1 | 27.39 | 31.46 | +14.9% | -4.4% | +0.3% |
| 64 / 1 | 857.60 | 784.09 | -8.6% | -47.3% | -32.9% |
| 64 / 4 | 879.47 | 846.91 | -3.7% | -44.2% | -31.0% |
| 256 / 1 | 3581.20 | 3224.09 | -10.0% | -48.0% | -34.6% |
| 256 / 4 | 3734.14 | 3517.49 | -5.8% | -47.2% | -34.1% |

所有场景的 frames/op、packs/op、wire-B/op 前后一致。256 会话、单 profile 时，32 次 pack 服务全部接收者，仍生成 256 帧、2,834,432 wire-B/op；缓存只省组件编码和内部载体分配。

初版 map 缓存曾使单会话中位数耗时上升 31.2%，已替换为直接共享捕获结果；保留 `encoding-after-map.txt` 用于解释这次取舍。最终方案不在每个会话上重复查编码缓存 map，并把捕获缓存集中分配为 tick 缓冲，避免每个 subject 单独分配。五个短样本未做 benchstat 显著性检验，百分比是本机观察值，不能外推生产容量。

本地原始文件：`artifacts/perf/sync/encoding-before.txt`、`encoding-after.txt`、`encoding-after-map.txt`。

单会话场景在本次短采样中未获时间收益（约 +15%），不能宣称所有负载都更快；共享编码主要面向同一实体被多个会话观察的负载。正式部署前应按实际扇出、dirty 比例和 payload 重复长采样。

上述 pprof 命令已实际跑通。256 会话、单 profile 的 `alloc_space` 采样中，`frame.Encode` 自身分配约占 57.6%，`Manager.Flush` 约占 21.8%，`maps.clone` 约占 7.6%。这提示下一轮可优先检查外层帧分配、tick 临时容器和会话 map 复制；它是采样定位线索，不是 CPU 耗时占比，也不作为本轮扩大重构范围的依据。本地保留 `cpu.out`、`mem.out`、`cpu-top.txt`、`alloc-top.txt` 和对应的 `entitysync.test`。
