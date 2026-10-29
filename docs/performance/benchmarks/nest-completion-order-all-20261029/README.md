# 同实体普通事务排在异步完成之后：红绿与开销对照（2026-10-29）

维护者 2026-10-29 决定 A：同一实体上每笔事务对外可见的效果（回复、AfterCommit）按提交序，包括跟在异步完成事务（saga 原生步骤）
后面的 strict / async / memory 事务。设计与不变量见 [NEST_PIPELINED_COMMIT §11](../../../framework/NEST_PIPELINED_COMMIT.md#11-phase-2-实现异步完成)。
基线 `13390a8c`。macOS / Apple M5 / Go 1.27.3，默认 GOMAXPROCS（10）；机器有常驻后台负载（loadavg 约 2.5～6）。

## 红绿

- `red.txt`：变异“普通事务不上完成链、不等待”（`runTransaction` 里跳过 `orderBehindAsyncCompletions`，等价于修改前的代码）与
  “批量作业不转单目标”时的失败原文。`TestOrdinaryCommitsWaitForEarlierAsyncCompletion` 的 async / strict / memory 三个子用例都在
  “T1 票据未持久时 T2 已回复”处变红；`TestAsyncCompletionLeavesNonPipelinedHandlersUnchanged` 的 strict / async 在第三种引擎上
  `repliedBeforeEarlierAsync:true`；`TestTryBatchTargetBehindAsyncCompletionIsConverted` 的 X 目标在 T1 完成前跑了 AfterCommit。
- `red-await-after-hooks.txt`：变异“等待登记在 AfterCommit 之后”（回复仍被挡住，但 AfterCommit 先于 T1）时的失败原文。
- 修后上述用例全绿：`GOWORK=off go test -race -count=3 ./framework/nest/...`。

## 快路径开销

修前、修后分别 `GOWORK=off go test -c -o <bin> ./framework/nest/` 编译（`BenchmarkNestHotPathCompletionPump` 只用已有公开选项，
修前二进制同样包含它），各跑一遍，`-count 6`：

```sh
<bin> -test.run '^$' -test.bench 'BenchmarkNestHotPathCompletionPump/' -test.count 6 -test.benchmem
<bin> -test.run '^$' -test.bench 'BenchmarkNestHotPath$/cpu=false/workers=(1|8)/independent=true/(single|multi4)/' -test.count 6 -test.benchmem
benchstat before.txt after.txt
```

`BenchmarkNestHotPathCompletionPump` 的引擎带完成泵（点名的是另一个 handler），被测 memory handler 每次提交都走“声明实体有没有
在途异步完成”的检查且都为 0（快路径）；`BenchmarkNestHotPath` 没有完成泵。结果（`benchstat.txt`）：allocs/op 与 B/op 全部不变；
sec/op 16 项在 −6.3%～+1.7% 之间、正负都有，geomean −0.36%，读作噪声范围内没有可测开销。不能据此宣称变快。

## saga 端到端短对照

`ROOST_SCAN_PLAN="native-forward:128 native-forward:128@strict4 native-forward:128" scripts/perf/saga-js-scan.sh`（私有三节点 NATS、
私有 mongod，GOMAXPROCS=4，每档 30s；`saga-js-scan-summary.tsv`、`saga-js-scan-env.txt`）：pipelined + 异步完成 1011.4 / 996.3 saga/s，
strict4 250.2 saga/s，约 4.0×；正确性门槛（非期望终态、余额不符、发布未知）全 0。上一轮同计划为 pipelined 1022–1053、strict4 254–262、
3.9–4.1×；这一轮开跑时 loadavg 5.6，差在噪声范围内。压测参与方的实体上只有原生步骤，没有普通事务，慢路径不在这条负载里出现。
