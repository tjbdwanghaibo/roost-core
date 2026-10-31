# Saga 正式实现：云 Mongo c128 复现原型的两处“尚未确认”（2026-10-29）

对象是[原型压测报告](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md)第三轮 T0 第 5 条：云 Mongo、c128 第一次运行的两处异常。
（a）余额核对报 false，但冷 Mongo 账户与内存逐账户一致、复读一致、非期望终态 0。当时用 5s 读超时，没有区分“读超时”和“不符”。
（b）停止加压时的积压是 41414 条，低于负载期采样到的峰值 111302 条；而投影速率只有约 560 条/s。

原型代码已删除（可从 `c565c81b` 的 `scripts/perf/saga-jslog-proto` 取回，本轮只读、没有放回树里）。本轮做了两件事：
一是给正式压测夹具加上分项计数和逐秒序列，在同等条件下复现；二是用原型留下的样本做算术核对。

样本在 [benchmarks/saga-cloud-c128-repro-20261029](benchmarks/saga-cloud-c128-repro-20261029/)，已脱敏（不含 URI 和主机名）。

## 1. 结论

1. **（b）已解释：原型的“停止加压时积压”不是在加压结束时读的，而是在开始加压后约 150s 读的。**
   - 原型 worker 在 `runCtx`（`seconds + 120` = 150s）到期前一直等某笔的终态；有 worker 卡住时，`wg.Wait()` 要到 150s 才返回。
   - “停止加压时积压”和“负载期投影速率”都在 `wg.Wait()` 之后计算，所以都以 150s 为准。
   - 在这 120s 里，加压实际已停，投影器以约 580 条/s 把积压从 111302 降到 41414。
   - 这不是采样问题，不是 WAL 或投影器的记账问题，也不是特殊记录合并 checkpoint 造成的跳变。证据见 §3。
2. **（a）有指向明确的解释，但逐账户证据缺失，仍标“尚未确认”。**
   - 卡住的那笔（或几笔）不在样本集里，原型按样本集计算预期余额，所以预期里缺了它们。
   - 如果它们的步骤在参与方执行过，内存和 Mongo 都包含这笔转账，而预期不包含：余额就会报 false，同时 Mongo 与内存一致、复读一致（复读只核对样本集）、非期望终态为 0（也只看样本集）。这与观察完全吻合。
   - 原型这一档比别的档多执行了 20 个步骤、多发了 28 笔的消息（§3.3），说明样本集之外确有步骤执行过。
   - 但原型没有保存逐账户输出，也没有区分读错误；那 20 步是未观察到终态的笔被执行，还是重投回放，已无法区分。所以按规则仍标“尚未确认”，不当作已排除。
   - 原型客户端为什么 120s 内没看到终态，同样尚未确认（原型代码已删）。
3. **正式实现在同等条件下没有复现。** 云 Mongo（本轮 ping P50 9ms，与原型第三轮的 7.8ms 同档），三档全部通过正确性门槛：
   - 发起方没观察到终态的笔为 0，读错误 0，Nest 与 Mongo 不符都是 0，Mongo 已核对；
   - 实际加压时长 30.0～30.1s；
   - c128 的两档在加压期间积压单调不降，停止加压时的积压等于峰值。
4. **新的事实：云 Mongo 上正式实现 c128 的追平超过 30s。**
   - 投影器约 1.5～1.8k 条/s（原型第三轮是串行投影器，约 0.5k 条/s），低于 c128 的准入速率（约 3.8～4.1k 条/s）。
   - 追平用时：c128 45.9s，c128@strict64 39.8s，c64 14.6s。
   - 设计 §12 的门槛是“停止加压后 30s 内积压归零”。本轮把追平上限放到 240s（`ROOST_SAGA_SCAN_DRAIN_SECONDS`），只是为了能核对 Mongo 余额；按 30s 门槛，c128 两档不达标。原生正向档本来就不开准入，这是容量事实，不是正确性问题。已写进 KNOWN-LIMITS。

## 2. 正式实现的复现结果

- **条件**
  - 正式 `TestRealSagaJSScanLevel`（`scripts/perf/saga-js-scan.sh`），提交 `16b7e6a2` 加本轮夹具改动。
  - 私有三节点 NATS（端口 29722 起），参与方投影写云 Mongo（`ROOST_SCAN_MONGO_URI`，唯一库 `roost_sagajs_scan_<pid>_<ns>`，用后已删除，复查列表为空）。
  - `GOMAXPROCS=4`，P=16，每档 30s、预热 5s，单样本。
  - 加压前 loadavg 2.7，swap 已用约 1.7 GB。
  - 整轮 18:30～18:33，约 4 分钟。

| 档 | 提交方式 / short 池 | saga/s | P50 / P99 ms | 实际加压 s | 积压峰值（50ms 采样） | 停止加压时积压 | 加压期最大单秒降幅 | 准入 / 投影 条/s（加压期） | 追平 s（投影 条/s） | 读错误 / Nest 不符 / Mongo 不符 / 未观察终态 |
| --- | --- | ---: | --- | ---: | ---: | ---: | ---: | --- | --- | --- |
| native-forward c128 | pipelined + 异步完成 / 4 | 2083.8 | 59.4 / 107.0 | 30.0 | 70202 | 70202 | 0 | 4142 / 1805 | 45.9（1530） | 0 / 0 / 0 / 0 |
| native-forward c128@strict64 | strict / 64 | 1904.0 | 65.5 / 114.8 | 30.0 | 61888 | 61888 | 0 | 3787 / 1729 | 39.8（1557） | 0 / 0 / 0 / 0 |
| native-forward c64 | pipelined + 异步完成 / 4 | 1291.6 | 46.9 / 108.6 | 30.1 | 26072 | 26072 | 234（t=17s） | 2586 / 1751 | 14.6（1679） | 0 / 0 / 0 / 0 |

- 非期望终态、发布结果未知都是 0；三档 `mongo_checked=true`。逐秒序列在样本目录的 `*.series.tsv`。
- 与原型同档对照（只看量级）：原型 c128 1962～2690 saga/s、投影 0.5～0.6k 条/s；正式实现 c128 约 1900～2080 saga/s、投影约 1.7～1.8k 条/s。

## 3. 证据

### 3.1 原型的“加压”实际持续了 150s

原型 `runPerfLevel`（`c565c81b:scripts/perf/saga-jslog-proto/perf_test.go`）的做法：

- `runCtx` 的期限是 `seconds+120`。
- worker 在 `<-ch` 与 `<-runCtx.Done()` 之间等终态；等到超时就 `t.Errorf("saga %s not terminal")` 并退出，这笔不进 `samples`。
- `wg.Wait()` 之后才停积压采样，再读 `PartBacklogAtStop`；`ProjRateDuringLoad = ProjectedDuringLoad / loadElapsed`。

样本（[round3-cloud/cloud-part-forward-p4g4.jsonl](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/benchmarks/saga-jetstream-log-proto-20261027/round3-cloud/cloud-part-forward-p4g4.jsonl)）里，`projected_during_load / projection_rate_during_load` 就是 `loadElapsed`：

| 档 | 负载期投影 条 | 负载期速率 条/s | 推出的 loadElapsed |
| --- | ---: | ---: | ---: |
| c1 | 5155 | 171.79 | 30.0s |
| c16 | 16112 | 536.76 | 30.0s |
| c64 | 17647 | 587.34 | 30.0s |
| **c128 第一次** | **84210** | **561.39** | **150.0s** |
| c128 重跑 | 18684 | 621.78 | 30.0s |

墙钟也独立印证了这一点（[runs.log](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/benchmarks/saga-jetstream-log-proto-20261027/round3-cloud/runs.log)）：

- c128 第一次 19:33:50～19:37:48，共 238s ≈ 150s 加压 + 83.6s 追平 + 约 4.5s 其余开销。
- 重跑 19:38:52～19:44:18，共 326s ≈ 30 + 290.5 + 5.5。
- 如果第一次只加压 30s，总时长应约为 120s。

结论：c128 第一次至少有一个 worker 卡到 `runCtx` 到期，这一档实际上已经 `t.Errorf` 失败，原报告没有记录这一点。

峰值 111302 是在真正的加压期（约前 30s）采到的。此后约 120s 只剩投影：(111302 − 41414) / 120s ≈ 582 条/s，与原型投影器的 0.5～0.6k 条/s 一致；之后 41414 条用 83.6s 追平，即 495 条/s，也一致。
所以“加压时积压下降”并不存在：下降发生在加压已经停止之后，只是被算进了“负载期”。

### 3.2 积压的记账：不会因合并 checkpoint 跳降

- **代码**（`framework/dataengine/engine`）：
  - `Projector.reserve`（projector.go:561 起）对每个新事务 ID 做一次 `walUnacked+1`。
  - `ReplayPass` 只在 `checkpoint()` 成功、即 WAL `Ack` 存盘之后调用 `acknowledge(records[acked:processed])`（projector_replay.go:38～50），对每条已确认记录做一次 `walUnacked−1`（projector_replay.go:259～270）。
  - 因此积压 = 已准入 − 已投影且已 checkpoint，按条计数。
  - 特殊记录合并 checkpoint（`9b11e4a5`，契约 `702b866e`）只决定几条成功投影的记录一起确认：最多 `CheckpointRecords`（256）条或 20ms；并行窗口一次最多 `ReplayBatchRecords × ProjectionWorkers`（256 × 8 = 2048）条。
  - 一次确认最多让积压降一个窗口，降掉的都是已经投影成功的记录；做不到降 7 万条。
  - 另外，原型第三轮云上运行（19:29～19:44）早于并行投影器提交 `75520e47`（同日 21:23），当时投影器是串行的，一次确认的跨度更小。
- **正式实现的逐秒序列**：
  - 每一行都满足 `committed − projected − backlog` ≤ 2078，即“已投影、尚未 checkpoint”的部分不超过约一个窗口。
  - 积压每次按窗口（2048 条）整块扣减；checkpoint 次数在加压期约每秒 1 次。
  - 两档 c128 在加压期没有一秒积压下降。

### 3.3 （a）的旁证与缺口

- **旁证 1：步骤数。** 原型样本的 `steps = executed + replayed`，`terminal` 只数样本集里到达期望终态的笔。
  - c1、c16、c64 和 c128 重跑都是 `steps = 2 × terminal`。
  - c128 第一次是 125382 = 2 × 62681 + **20**。
- **旁证 2：消息数。** `js_msgs_per_step × steps` 在其他档都是 6 × terminal（例如重跑 487626 = 6 × 81271）。
  - c128 第一次是 376254 = 6 × 62709，多出 **28** 笔的量。
- **推论**：样本集之外有笔推进过、步骤执行过；按 §1 第 2 条的机制，余额会报 false，而 Mongo 与内存一致。
- **缺口**：
  - 没有逐账户输出，无法核对不符的账户是否正好是卡住那几个 worker 的账户对（原型每个 worker 固定一对账户）。
  - `replayed` 和 `executed` 没有分开保存，那 20 步也可能是重投回放。
  - 原型 c565c81b 版本的核对已改成 30s 读超时并分项计数，那是重跑时加的；第一次运行时的 5s 版本是否有读超时，已无法确认。
  - 核对是在追平 83.6s 之后做的，那时 Nest 空闲，读超时的可能性很低，但不能排除。

### 3.4 c64 那一次 234 条的单秒降幅

c64 在 t=16→17s：准入 1814 条（前后几秒约 2.6k）、投影 2048 条（一个窗口），积压降 234 条；同一秒到达终态的只有 913 笔（平时约 1300）。

- 这是真实效应：那一秒准入暂时低于投影，不是记账错误。
- 积压按窗口整块扣减，按秒采样时会出现 0 个或 2 个窗口落在同一秒的锯齿（c128 t=33→34 只投影了 256 条，下一秒投影 1792 条），单秒波动的上界约一个窗口。
- 只要准入持续高于投影，就不会出现持续或大幅的下降。

## 4. 夹具改动（本轮）

`framework/saga/jetstream_performance_integration_test.go` `TestRealSagaJSScanLevel`：

- **余额核对分项计数**
  - 读失败或超时记 `balance_read_errors`；Nest 读的超时从 10s 放宽到 30s，读失败不再 `t.Fatal`。
  - 不符分别记 `balance_nest_mismatches`、`balance_mongo_mismatches`，`balance_mismatches` 是两者之和；第一处问题记进 `balance_first_problem`。
  - 关闭时 WAL 没投影完的，记 `close_error` 和 `mongo_checked=false`，不算作不符。
  - 读失败、Mongo 未核对都进门槛。
- **发起方没观察到终态的笔**（`StartSaga` 非背压错误、`WaitTerminal` 出错）
  - 不再静默丢出预期：追平后按协调器内存补查（`Get` / `WaitTerminal` 30s）。
  - 补到的记入预期（`late_terminals`），查不到的记 `unobserved_not_started`，补不到的记 `unresolved_sagas`，后者进门槛。
  - 原型的“卡住的笔不进预期”在这里会直接显出来。
- **加压结束时刻与积压**
  - `load_seconds` 是到 worker 全部返回的实际时长。
  - 另记 `backlog_at_load_end`、`projected_during_load` / `projected_during_drain`、`backlog_peak_in_load`、`backlog_max_drop_in_load`。
- **逐秒序列 `series`**
  - 内容：WAL 准入 / 追加 / 持久 LSN / fsync / checkpoint 次数，投影器准入 / 投影 / 积压，协调器准入判定与它看到的积压，已观察终态数。
  - 脚本另写 `<档>.series.tsv`，结束时补一行终值。
- **追平上限可调**：`ROOST_SAGA_SCAN_DRAIN_SECONDS`，缺省 30，最大 900；关闭参与方时的投影上限同步放宽（`ptParticipant.closeWithin`）。
- `scripts/perf/saga-js-scan.sh`：在 `summary.tsv` 中加入读错误、`mongo_checked`、未观察终态、实际加压时长、停止加压时积压、加压期最大降幅几列，并写出 series TSV。

## 5. 仍未确认

- 原型（a）：余额 false 是“样本集外的笔被执行、却不进预期”，还是读超时，缺逐账户证据。按旁证倾向前者。
- 原型客户端为什么在 120s 内没看到某笔的终态（原型代码已删除）。正式实现三档的 `unobserved_sagas` 都是 0，没有复现。
- 正式实现在云 Mongo 上只有这三档单样本，没有补偿档、准入档和更长时长；跨时段波动没有测。
