# 投影器合并 checkpoint 前后对照（2026-10-18）

维护者 2026-10-18 选第三档 A：带回执 / effect 的特殊记录投影后不再逐条 checkpoint，按 `CheckpointInterval`（缺省 20 ms）/ `CheckpointRecords`（缺省 256）与普通记录合并。方案与不变量论证：[CONTRACT-2026-10-18-projector-checkpoint-merge](../maintenance/CONTRACT-2026-10-18-projector-checkpoint-merge.md)。夹具、口径同 [Saga 原生步骤性能优化](SAGA-OPT-20261018.md) §2（夹具 B，`ROOST_STEPS_PERF_HOPS=1`，GOMAXPROCS=4）。样本与脚本：[benchmarks/saga-checkpoint-20261018](benchmarks/saga-checkpoint-20261018/)。

## 1. 结论

- **吞吐上限提高约三成**：投影器每条特殊记录的确认次数在积压时降到 0.56～0.75 次（修前恒为 1）。正向 8/s 由崩溃（307 笔终态不对、P50 8.9 s）变为全部正确、P50 517 ms / P99 793 ms；正向 10/s 由崩溃（877 笔不对）变为全部正确，但积压到 28 条、P50 2.2 s；全补偿 5/s 由 P50 3.9 s 降到 688 ms（P99 1.2 s）。正向 12/s、全补偿 7/s 起修后仍崩溃。投影器容量由约 15 条/s 升到约 20 条/s。
- **持续档不变**：正向 5/s、全补偿 3/s 没有积压，一轮回放通常只有一条记录，轮末照常确认，checkpoint 次数仍是每条 1 次；Server P50 修前 344 / 315、375 / 357 ms，修后 302 / 281、347 / 349 ms（正向 / 全补偿各两轮），差异在轮间波动内。
- **合并系数受 20 ms 间隔限制**：投影事务约 16 ms，时间阈值在第二条后就触发，所以积压时约每两条确认一次，checkpoint 本身的耗时（负载下 P50 51～59 ms）不变。真实 WAL 微基准里（内存 Store，投影几乎不耗时）256 条全特殊记录由 256 次 checkpoint 降到 1 次，一轮耗时 2.27 s → 10.2 ms。
- **正确性**：所有不崩溃的档位 0 笔终态不对、逐账户恰好一次；崩溃档位与修前同样是重试放大（步骤体超截止），过载时仍恰好一次。真实 Mongo 崩溃注入回归通过（§4）。

## 2. 真实 WAL 微基准

`BenchmarkProjectorWALReplayAckMatrix`（真实文件 WAL 与 checkpoint fsync，内存 `BatchProjectionStore`，每次 256 条记录，单 mutation，特殊记录带一个 effect），修前 / 修后二进制交替各 6 轮，`-benchtime 3x`（`wal-benchstat.txt`）：

| 负载 | 修前 sec/op | 修后 sec/op | 修前 acks/op | 修后 acks/op |
| --- | --- | --- | --- | --- |
| 全普通 | 10.45 m | 10.38 m（~） | 1 | 1 |
| 特殊 1% | 44.52 m | 10.41 m（−76.6%） | 5 | 1 |
| 特殊 10% | 460.1 m | 10.04 m（−97.8%） | 51 | 1 |
| 全特殊 | 2267 m | 10.18 m（−99.6%） | 256 | 1 |

空载下一次 checkpoint 约 8.8 ms（2267 ms / 256）。修前第 6 轮的“修前”运行期间有一次本仓 `go vet` 编译，不影响量级结论。

## 3. 夹具 B 同机前后对照

运行方式：修前 = origin/main `e90dd4ad`，修后 = 本分支；两个测试二进制事先编好，编排脚本 `perf-run.sh.txt` 只执行二进制，持续档前后交替、上限档前后相邻；每个运行开始前检查没有别的 `go test` / `go build`（整轮没有等待记录）。开测与结束 load average 3.5～5.2，swap 已用 6.7 GB / 8 GB，与 SAGA-OPT 相同。同时每秒采一行 `iostat -d disk0`（`iostat.txt`）。

### 3.1 持续档（各 3 分钟 × 2 轮）

| 档位 | 版本 | 完成 | 终态不对 | P50 | P99 | Server P50 | Server P99 | checkpoint P50 | checkpoint 次/条 | 核对 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 正向 5/s | 修前 | 900 / 900 ×2 | 0 / 0 | 385 / 355 | 575 / 556 | 344 / 315 | 536 / 499 | 39.9 / 44.6 | 1.00 / 1.00 | 通过 |
| 正向 5/s | 修后 | 900 / 900 ×2 | 0 / 0 | 341 / 326 | 496 / 517 | 302 / 281 | 455 / 472 | 34.8 / 42.8 | 1.00 / 1.00 | 通过 |
| 全补偿 3/s | 修前 | 540 / 540 ×2 | 0 / 0 | 420 / 401 | 685 / 681 | 375 / 357 | 636 / 617 | 32.6 / 40.3 | 1.00 / 1.00 | 通过 |
| 全补偿 3/s | 修后 | 540 / 540 ×2 | 0 / 0 | 391 / 394 | 645 / 681 | 347 / 349 | 600 / 639 | 38.8 / 40.7 | 1.00 / 1.00 | 通过 |

逐跳（两轮平均，`hops2.py.txt`）：正向 5/s 关键路径合计 386 → 343 ms/笔，差异分散在各个 Mongo 跳（每跳 P50 少 1～3 ms），9a“WAL 准入→开始投影”P95 43 → 30 ms；全补偿 3/s 448 → 435 ms/笔。每笔 Mongo 命令次数相同（正向 commitTransaction 8.1、findAndModify 9.7 / 9.0；全补偿 12.2 / 12.8 / 12.7）。持续档的差异按宿主时段波动看待，不归因到本改动。

### 3.2 上限档（各 2 分钟，单样本）

| 样本 | 终态/s | 终态不对 | P50 | P99 | 核对 | 投递+排队 P50/P95 | 投影等待 P50/P95 | 投影事务 P50 | checkpoint P50/P95 | checkpoint 次/条 | 积压峰值 | 步骤体超截止 | 屏障挡回 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 修前 正向 7/s | 6.99 | 0 | 465 | 883 | 通过 | 10/21 | 38/180 | 16 | 52/77 | 1.00 | 5 | 0 | 0 |
| 修后 正向 7/s | 6.99 | 0 | 462 | 624 | 通过 | 12/24 | 22/71 | 16 | 56/83 | 0.89 | 3 | 0 | 0 |
| 修前 正向 8/s | 6.37 | **307** | 8890 | 34820 | 否 | 3492/4946 | 1952/4296 | 16 | 52/79 | 1.14 | 66 | 506 | 119 |
| 修后 正向 8/s | 7.99 | **0** | 517 | 793 | 通过 | 12/24 | 41/138 | 16 | 58/88 | 0.73 | 5 | 0 | 0 |
| 修前 正向 10/s | 7.16 | **877** | 30879 | 37539 | 否 | 4902/4950 | 349/3878 | 16 | 48/73 | 0.95 | 66 | 1163 | 125 |
| 修后 正向 10/s | 9.85 | **0** | 2200 | 3074 | 通过 | 12/26 | 891/1210 | 16 | 59/88 | 0.56 | 28 | 0 | 0 |
| 修后 正向 12/s | 9.58 | 592 | 12976 | 38143 | 否 | 3676/4942 | 1083/3053 | 16 | 54/84 | 0.60 | 68 | 738 | 368 |
| 修前 全补偿 4/s | 4.00 | 0 | 672 | 1267 | 通过 | 12/25 | 21/130 | 16 | 53/88 | 1.00 | 3 | 0 | 0 |
| 修后 全补偿 4/s | 3.99 | 0 | 567 | 863 | 通过 | 12/24 | 0/43 | 16 | 51/82 | 0.95 | 2 | 0 | 0 |
| 修前 全补偿 5/s | 4.91 | 0 | 3943 | 4544 | 通过 | 12/21 | 1100/1336 | 16 | 52/79 | 1.00 | 21 | 0 | 0 |
| 修后 全补偿 5/s | 4.99 | 0 | 688 | 1226 | 通过 | 12/24 | 25/126 | 16 | 54/89 | 0.75 | 4 | 0 | 0 |
| 修后 全补偿 7/s | 5.78 | 204 | 12972 | 32853 | 否 | 631/4933 | 1894/3374 | 16 | 59/92 | 0.64 | 86 | 340 | 1021 |
| 修前 全补偿 10/s | 7.80 | 991 | 32239 | 38399 | 否 | 4901/4948 | 585/5958 | 16 | 51/81 | 0.62 | 88 | 1164 | 729 |
| 修后 全补偿 10/s | 8.01 | 771 | 33034 | 37860 | 通过* | 4884/4941 | 164/2955 | 16 | 56/87 | 0.47 | 63 | 1080 | 402 |

“checkpoint 次/条”= 每笔 checkpoint 次数 / 每笔特殊记录数（正向 2、全补偿 3）；崩溃档位里重试会多投影记录，这一列只作参考。*全补偿 10/s 修后核对为 true 是因为核对窗口内的余额恰好一致，终态不对 771 笔，属崩溃。

读法：

- 修前在正向 8/s（16 条/s）已越过投影器容量：投影等待超过命令截止，记录被 lease fence 跳过、协调器按超时重试，放大到崩溃——与 SAGA-OPT §5 的机制相同。SAGA-OPT §10.2 复测里修前正向 8/s 两个样本是 0 笔不对（积压线性增长），本轮这一档崩溃；边界档位样本间起伏大，本节只作定性比较。
- 修后正向 8/s 的投影等待 P95 138 ms、积压 5，余量明显；正向 10/s（20 条/s）跟得上但积压到 28、延迟秒级，处在新容量的边界；正向 12/s（24 条/s）、全补偿 7/s（21 条/s）越过边界后崩溃方式与修前相同。
- checkpoint 本身没有变快（P50 仍 51～59 ms），收益全部来自次数：积压越深合并越多（正向 10/s 时每条 0.56 次）。合并系数的上限由 `CheckpointInterval` 决定（§5）。

## 4. 崩溃注入回归（真实 Mongo）

| 用例 | 场景 | 修前（`e90dd4ad` 实现） | 修后 |
| --- | --- | --- | --- |
| `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`（framework/dataengine/engine，integration） | 真实 `DataEngineStepInbox` 构造的原生步骤记录 A（生效）、B（首投时租约过期被跳过）与带 effect 的普通记录 C（A 之后同文档的新版本）在一次合并 checkpoint 前“崩溃”；之后结算 A 的操作文档（fence 现在会失败）、续上 B 的租约（fence 现在会通过）、删除已发布的 effect；重开 WAL、新 Store、新投影器 `Flush` | 失败于第一步：`first pass n=1 acks=1 … want all 3 special records projected before one merged checkpoint`（`red-crash-engine.txt`） | 通过：业务文档、操作文档、回执、事务标记逐字段不变，outbox 不重新暂存，重放结论 A applied / B skipped / C applied |
| 同上，变异：把 `checkTransaction` 挪到 `leaseFencesMatch` 之后（不提交） | 验证用例确实守住“重放不重判 fence” | — | 失败：B 的操作文档被确认写改动、A 的重放结论变成 skipped（`mutation-fence-first.txt`） |
| `TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce`（wiring/saga，integration） | 真实 Nest + DataEngine + saga 跑正向 3 笔、业务拒绝补偿 2 笔、投影停顿被跳过后重试 1 笔；停进程，把 checkpoint 槽恢复成开跑前内容（等价于之后的 checkpoint 都没落盘），同一 WAL 目录与库重启 | 通过（守卫：修前用同样的“丢 checkpoint”状态也成立，它守的是重放幂等这个前提） | 通过：启动恢复重放 16 条，0 冲突；余额与版本、回执 14 份、事务标记 15 个（含 1 个 skipped）、操作文档、saga 终态与重启前相同，效果流 14 条消息不变 |

saga 级用例对“重放时重判 fence”的变异不敏感：启动恢复时没有常驻实体，被误判为 skipped 的重放没有可观察的副作用；这一点由引擎级用例守。

## 5. 判断与建议

- A 在容量上带来约 1.3 倍（15 → 20 条/s），把“全部正确但延迟秒级”的区间推到正向 10/s、全补偿 5～6/s。单进程 P50 在 1 秒内的档位修后约为正向 8/s、全补偿 5/s（P99 0.8 / 1.2 s，各单样本）。
- 继续提高同一机制的收益要放宽时间阈值：积压时每条特殊记录约 16 ms，`dataengine.projection.checkpoint_interval` 设为 100 ms 时理论上约每 6 条确认一次（每条摊到约 9 ms），代价是崩溃后重放窗口与未确认额度占用相应变长（重放仍是幂等的标记读）。缺省值是否调整属于产品选择，本轮没有改、也没有测。
- 投影事务（每条约 16 ms、串行）成为下一个上限：即使 checkpoint 完全摊薄，单 goroutine 约 60 条/s。再往上是第三档 B（合批带回执 / effect 的记录），维护者已决定本轮不做。

## 6. 未验证与限制

- 单进程、单机、共享隔离环境；宿主长期后台负载。上限档每档只有 1 个样本、2 分钟。
- checkpoint 的 F_FULLFSYNC 与 outbox 唤醒同盘的问题（SAGA-OPT §10.3）没有改变，本轮只减少了次数。
- Linux（`fsync` 不是 F_FULLFSYNC）未测；多进程、跨机未测。
- 崩溃注入是“丢 checkpoint”式模拟（ack 失败 / 恢复 checkpoint 槽），没有在真实进程里 `kill -9`；持久状态与崩溃后一致，但没有覆盖 checkpoint 文件写到一半的情形（那由 nestwal 双槽轮换与校验和守，未改）。

## 7. 复跑

```bash
source ~/.roost-it/roost-dataengine-it/env.sh   # 持有 $ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock
# 真实 WAL 微基准（修前 / 修后各编一个 framework/dataengine/engine 测试二进制，交替运行，见 bench-wal.sh.txt）
GOWORK=off go test -run '^$' -bench BenchmarkProjectorWALReplayAckMatrix -benchtime 3x -count 6 ./framework/dataengine/engine/
# 夹具 B（编排见 perf-run.sh.txt）
ROOST_STEPS_PERF_HOPS=1 ROOST_STEPS_PERF_OUTPUT=b.json ROOST_STEPS_PERF_VARIANT=steps ROOST_STEPS_PERF_MODE=forward \
  ROOST_STEPS_PERF_RATE=8 ROOST_STEPS_PERF_DURATION=2m ROOST_DATAENGINE_IT=1 GOMAXPROCS=4 GOWORK=off \
  go test -tags integration -count=1 -timeout 0 -run '^TestRealNativeStepsPerformance$' ./wiring/saga/
# 逐跳表：python3 hops2.py.txt "修前=a.json,b.json" "修后=c.json,d.json"；过载表：python3 sat.py.txt <样本…>
# 崩溃注入
ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run 'TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint' ./framework/dataengine/engine/
ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run 'TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce' ./wiring/saga/
```
