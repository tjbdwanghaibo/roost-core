# DataEngine 本机基准（2026-09-24）

Apple M5 / darwin arm64，Go 1.27.0，GOMAXPROCS=4；基线 `967bc69` 加当前工作树。最终两组命令顺序执行，未并行跑其他测试。最初与集成测试、另一组 benchmark 重叠的探索结果不作为下面的数据。

这是一份框架/文件系统基线，不是 MMO 业务写入容量或线上 SLA，也没有“优化前/后”收益结论。

正式 Nest + 生成 DAO + 真实 Mongo 的单/多 DAO 压测与收尾评估见 [业务压力报告](DATAENGINE-PRESSURE-2026-09-24.md)，其吞吐不能与本文的内存 Store 微基准混用。

## 命令

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test ./dataengine/engine -run '^$' \
  -bench 'BenchmarkProjectionSegmentPlanner|BenchmarkProjectorWALReplayAckMatrix' \
  -benchmem -benchtime=3x -count=3 -cpu=4
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test ./dataengine/engine -run '^$' \
  -bench '^BenchmarkProjectorAdmissionMatrix$' -benchmem -benchtime=200x -count=3 -cpu=4
```

## WAL replay/ack

每 op 256 条真实 WAL 记录；存储投影是内存替身，包含真实 ack/checkpoint 写盘。三轮中位数，括号为最小～最大值。special 表示含 effect、需要单条投影的事务比例，与 Sync 的实体变化率无直接对应关系。

| 负载 | 每 256 条耗时 ms | 折算记录/s | ack/批 | 分配 KiB/批 |
| --- | ---: | ---: | ---: | ---: |
| ordinary_only | 10.55 (9.82～11.82) | 24269 | 1 | 338.3 |
| special_1_percent | 50.50 (45.74～54.35) | 5069 | 5 | 345.8 |
| special_10_percent | 453.61 (447.70～484.75) | 564 | 51 | 422.4 |
| all_special | 2264.71 (2253.90～2282.89) | 113 | 256 | 779.4 |

单批 ack 数分别为 1、5、51、256；即使投影存储是替身，special 比例上升也明显拉长本机耗时。因此后续应 profile checkpoint/fsync 与成功前缀的 ack 合并机会，不能先假设主要成本在 Mongo 或分配。改变 ack 批次需要验证投影成功但 checkpoint 丢失、部分事务失败和 held 边界，本批没有修改这项语义。

## 准入

每轮 200 笔、3 轮中位数。writers=1/8/32 独立于 -cpu=4；每个 writer 同时只等待一笔。async 观测到 WAL 接受/写入返回；strict 与 pipelined 等待 durable，pipelined 每笔等待 ticket，不代表异步完成队列最大吞吐。表中 p99 是每轮样本 p99 的中位数，并非合并样本的 p99。吞吐是并发墙钟耗时的折算值。

| 模式 | writers | 事务/s | p95 ms | p99 ms | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| async | 1 | 127 | 12.01 | 13.04 | 179443 | 70 |
| async | 8 | 1010 | 10.31 | 12.27 | 134444 | 56 |
| async | 32 | 4447 | 11.46 | 11.48 | 66998 | 41 |
| strict | 1 | 123 | 9.97 | 11.04 | 158872 | 67 |
| strict | 8 | 1048 | 8.34 | 8.99 | 134800 | 58 |
| strict | 32 | 3719 | 8.90 | 11.90 | 87961 | 41 |
| pipelined | 1 | 125 | 9.24 | 10.82 | 179750 | 71 |
| pipelined | 8 | 1042 | 9.06 | 10.51 | 176941 | 61 |
| pipelined | 32 | 3497 | 11.63 | 11.71 | 109145 | 44 |

准入运行时包含后台 Projector；B/op 同时计入背景 goroutine 的分配及等待期间的 replay 轮询开销，不是 DAO setter 单次分配。样本较短、批处理填充与本机 fsync 抖动明显，不能根据本表推断三种 durability 的优劣或承诺 50ms 硬上限。1000 玩家/10000 实体/1% 或 5% 变化率还需要明确哪些字段持久化、每消息 DAO 数与事务形态才能转换成业务负载。

## 原始输出

以下保留最终顺序执行的完整 benchmark 输出，方便复核；所有命令 exit 0。

```text
goos: darwin
goarch: arm64
pkg: github.com/tjbdwanghaibo/roost-core/dataengine/engine
cpu: Apple M5
BenchmarkProjectionSegmentPlanner/special_every_0-4         	       3	     29750 ns/op	      64 B/op	       1 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_0-4         	       3	    105389 ns/op	      64 B/op	       1 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_0-4         	       3	     30028 ns/op	      64 B/op	       1 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_100-4       	       3	     30222 ns/op	    3920 B/op	       6 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_100-4       	       3	     77264 ns/op	    3920 B/op	       6 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_100-4       	       3	     28431 ns/op	    3920 B/op	       6 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_10-4        	       3	     13861 ns/op	   32592 B/op	       9 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_10-4        	       3	     14792 ns/op	   32592 B/op	       9 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_10-4        	       3	     35153 ns/op	   32592 B/op	       9 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_1-4         	       3	     27694 ns/op	  122704 B/op	      11 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_1-4         	       3	     13708 ns/op	  122704 B/op	      11 allocs/op
BenchmarkProjectionSegmentPlanner/special_every_1-4         	       3	     13389 ns/op	  122704 B/op	      11 allocs/op
BenchmarkProjectorWALReplayAckMatrix/ordinary_only-4        	       3	  11821681 ns/op	         1.000 acks/op	         1.000 batch_calls/op	         0 project_calls/op	  346373 B/op	    9424 allocs/op
BenchmarkProjectorWALReplayAckMatrix/ordinary_only-4        	       3	  10548486 ns/op	         1.000 acks/op	         1.000 batch_calls/op	         0 project_calls/op	  346346 B/op	    9423 allocs/op
BenchmarkProjectorWALReplayAckMatrix/ordinary_only-4        	       3	   9815069 ns/op	         1.000 acks/op	         1.000 batch_calls/op	         0 project_calls/op	  346373 B/op	    9424 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_1_percent-4    	       3	  45739348 ns/op	         5.000 acks/op	         3.000 batch_calls/op	         2.000 project_calls/op	  354112 B/op	    9506 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_1_percent-4    	       3	  50501833 ns/op	         5.000 acks/op	         3.000 batch_calls/op	         2.000 project_calls/op	  354104 B/op	    9506 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_1_percent-4    	       3	  54351847 ns/op	         5.000 acks/op	         3.000 batch_calls/op	         2.000 project_calls/op	  354178 B/op	    9507 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_10_percent-4   	       3	 453612667 ns/op	        51.00 acks/op	        26.00 batch_calls/op	        25.00 project_calls/op	  432554 B/op	   10399 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_10_percent-4   	       3	 484748042 ns/op	        51.00 acks/op	        26.00 batch_calls/op	        25.00 project_calls/op	  432480 B/op	   10398 allocs/op
BenchmarkProjectorWALReplayAckMatrix/special_10_percent-4   	       3	 447696181 ns/op	        51.00 acks/op	        26.00 batch_calls/op	        25.00 project_calls/op	  432560 B/op	   10399 allocs/op
BenchmarkProjectorWALReplayAckMatrix/all_special-4          	       3	2253904098 ns/op	       256.0 acks/op	         0 batch_calls/op	       256.0 project_calls/op	  798178 B/op	   15735 allocs/op
BenchmarkProjectorWALReplayAckMatrix/all_special-4          	       3	2264714569 ns/op	       256.0 acks/op	         0 batch_calls/op	       256.0 project_calls/op	  798037 B/op	   15734 allocs/op
BenchmarkProjectorWALReplayAckMatrix/all_special-4          	       3	2282886583 ns/op	       256.0 acks/op	         0 batch_calls/op	       256.0 project_calls/op	  798104 B/op	   15734 allocs/op
PASS
ok  	github.com/tjbdwanghaibo/roost-core/dataengine/engine	85.916s
```

```text
goos: darwin
goarch: arm64
pkg: github.com/tjbdwanghaibo/roost-core/dataengine/engine
cpu: Apple M5
BenchmarkProjectorAdmissionMatrix/async/writers_1-4         	     200	   7945284 ns/op	  12014709 p95-admit-ns	  13315709 p99-admit-ns	  201173 B/op	      72 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_1-4         	     200	   7713508 ns/op	  12115667 p95-admit-ns	  13037416 p99-admit-ns	  159192 B/op	      70 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_1-4         	     200	   7862573 ns/op	  11718958 p95-admit-ns	  12799459 p99-admit-ns	  179443 B/op	      61 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_8-4         	     200	    990233 ns/op	   9446500 p95-admit-ns	  11413166 p99-admit-ns	  134444 B/op	      43 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_8-4         	     200	   1598466 ns/op	  19976166 p95-admit-ns	  24480458 p99-admit-ns	  114156 B/op	      61 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_8-4         	     200	    962934 ns/op	  10305125 p95-admit-ns	  12266792 p99-admit-ns	  134769 B/op	      56 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_32-4        	     200	    224863 ns/op	  11462833 p95-admit-ns	  11484375 p99-admit-ns	   66998 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_32-4        	     200	    249658 ns/op	  13300833 p95-admit-ns	  13349125 p99-admit-ns	   88022 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/async/writers_32-4        	     200	    200464 ns/op	   8561208 p95-admit-ns	   8580042 p99-admit-ns	   66777 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_1-4        	     200	   7894191 ns/op	   9780500 p95-admit-ns	  10787083 p99-admit-ns	  263827 B/op	      71 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_1-4        	     200	   8098049 ns/op	   9970500 p95-admit-ns	  11041958 p99-admit-ns	  138116 B/op	      67 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_1-4        	     200	   8409556 ns/op	  10296375 p95-admit-ns	  14426750 p99-admit-ns	  158872 B/op	      67 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_8-4        	     200	    969041 ns/op	   9183542 p95-admit-ns	   9850375 p99-admit-ns	  134800 B/op	      58 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_8-4        	     200	    909719 ns/op	   8339041 p95-admit-ns	   8986667 p99-admit-ns	  176991 B/op	      58 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_8-4        	     200	    954011 ns/op	   8242542 p95-admit-ns	   8912583 p99-admit-ns	  114072 B/op	      58 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_32-4       	     200	    253892 ns/op	   8119500 p95-admit-ns	  15905375 p99-admit-ns	   87958 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_32-4       	     200	    284529 ns/op	  11883583 p95-admit-ns	  11899625 p99-admit-ns	   87961 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/strict/writers_32-4       	     200	    268918 ns/op	   8898750 p95-admit-ns	   8970125 p99-admit-ns	  108927 B/op	      41 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_1-4     	     200	   7998951 ns/op	   9949084 p95-admit-ns	  10815042 p99-admit-ns	  180031 B/op	      71 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_1-4     	     200	   8003598 ns/op	   9236875 p95-admit-ns	  11929083 p99-admit-ns	  159566 B/op	      80 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_1-4     	     200	   8008407 ns/op	   9183542 p95-admit-ns	  10080042 p99-admit-ns	  179750 B/op	      70 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_8-4     	     200	    933406 ns/op	   8352958 p95-admit-ns	   8878959 p99-admit-ns	  176941 B/op	      61 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_8-4     	     200	    959382 ns/op	   9060291 p95-admit-ns	  10505208 p99-admit-ns	  135251 B/op	      61 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_8-4     	     200	   1263677 ns/op	  16460625 p95-admit-ns	  22321208 p99-admit-ns	  198162 B/op	      61 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_32-4    	     200	    285971 ns/op	  11634125 p95-admit-ns	  11712250 p99-admit-ns	  109145 B/op	      44 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_32-4    	     200	    263626 ns/op	   8423375 p95-admit-ns	   8487667 p99-admit-ns	   67041 B/op	      45 allocs/op
BenchmarkProjectorAdmissionMatrix/pipelined/writers_32-4    	     200	    292654 ns/op	  15892292 p95-admit-ns	  15902125 p99-admit-ns	  150825 B/op	      44 allocs/op
PASS
ok  	github.com/tjbdwanghaibo/roost-core/dataengine/engine	56.847s
```



## 重放无进展时的分配优化

同机 Go 1.27 / Apple M5，固定 GOMAXPROCS=4；真实临时文件 WAL，后台 Projector 已停止。idle 为空 WAL；held 为首条事务未释放，只读取并停止，不执行 Mongo 投影。每项 2,000 次、重复 3 轮，以下为各指标中位数：

| 场景 | 修改前 B/op | 修改后 B/op | 降幅 | allocs/op 前→后 | ns/op 前→后 |
| --- | ---: | ---: | ---: | ---: | ---: |
| idle | 51,290 | 2,121 | 95.9% | 22→20 | 29,357→29,029 |
| held | 52,178 | 3,009 | 94.2% | 53→51 | 32,579→32,755 |

修改前 alloc_space profile 中 ReplayPass 自身累计分配 545.55 MB，占采样总量的 87.50%。它在读取 WAL 前就申请 256 条 records/fences，即使没有可投影记录也会分配。现在先检查 held，只有读到可投影记录才申请；同时直接使用投影单元中的记录完成 ack 记账，去掉 processedIDs 切片。投影单元、ack 次数、失败立即停止与重放幂等语义不变。

修改后 held 有一轮 82,996 ns/op 的波动，因此不能据此宣称耗时或吞吐获得稳定提升；确定收益是上述微基准的内存分配下降。真实 WAL 解析仍有必要分配，没有声称零分配。

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test ./dataengine/engine \
  -run '^$' -bench '^BenchmarkProjectorReplayIdleOrHeld$' \
  -benchtime=2000x -count=3 -cpu=4 -benchmem \
  -memprofile=/tmp/replay.mem -o /tmp/replay.test
```

证据目录 `/tmp/roost-dataengine-next/`：`replay-before.txt`、`replay-after.txt`、`replay-before.mem`、`replay-after.mem`；`engine-before.test` / `engine-after.test` 可供 pprof 符号化。准入矩阵同口径复跑保存在 `admit-after.txt`（200 次×3，57.041s）。它包含后台投影/刷盘与调度开销，波动较大，不从这次短测推导业务吞吐改善。此前混合分段/ack 基线仍有效，本轮没有改变 ack 策略。


## 100k 恢复实测补充

普通单 DAO 记录、10k 实体各 10 次版本变化，真实文件 WAL 跨 20 段，真实 Mongo 恢复 100k 条耗时 11.283 秒，约 8,863 条/秒、391 次 checkpoint ack。非 race、单次最终验收，不与前面的内存 Store 微基准混为同一指标；完整输入、正确性断言和复跑方式见 [恢复验收](DATAENGINE-RECOVERY-2026-09-24.md)。
