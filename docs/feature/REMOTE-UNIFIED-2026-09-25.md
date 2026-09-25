# Remote 代码收敛、性能定位与复测

后续已定位本文 30 分钟失败涉及的串行投影/确认排队链路，并完成 30 分钟 60 TPS 全量验收，见 [Remote 超时与吞吐报告](REFACTOR-2026-09-25-remote-throughput.md)。本文保留当轮源码的原始通过与失败证据。

2026-09-25，继续当前工作树。用户确认没有新部署，本轮直接修改 bug/代码、再测试；不执行部署或生产数据迁移。前一轮原始数据见 [持久权限报告](REMOTE-AUTHORITY-2026-09-25.md)。

## 最终实现

1. [RR-20260925-02](../bugfix/RR-20260925-02.md)：MongoCommitter 默认强制持久许可，删除旧 CAS/隐式创建 metadata、可变开关和未发布迁移入口。WriteAuthority() 是无副作用的能力 getter。不支持的 metadata 拒绝启动，不自动清数据/WAL。
2. 共享准入复用本次 durable grant 的 ownership，每实体减少一次独立 Mongo FindOne；锁关闭/失效后不能提供有效 grant。回归覆盖冷准入 1 次读取、缓存命中 0 次独立读取。
3. [RR-20260925-03](../bugfix/RR-20260925-03.md)：Nest 全进程慢堆栈每 5 秒至多采样一次，保留逐请求慢日志/耗时指标和采样抑制计数。中间全量样本日志文本从约 1.45 GB 降到约 19.9 MB。
4. [RR-20260925-04](../bugfix/RR-20260925-04.md)：DataEngine 已原子落库后的发布路径，使用 majority 读取+完整 digest 校验直接重放持久回执，省去重复只读 Mongo 事务。首次提交与并发首次提交仍执行事务内幂等检查和最新许可 CAS。
5. 性能/矩阵脚本保存 Nest 源码 hash；生成工程测试支持可选 CPU profile 和 Go trace。包目录和 WAL/DAO/快照格式不变。

## 最终验收状态

- 相关五包 `go test -race ./remoteentity ./nest ./dataengine/engine ./kit/dataengine ./kit/remoteentity`：通过。
- 同范围 `go vet -tags=integration`：通过；三个脚本语法检查及 `git diff --check`：通过。
- 回执快路负对照：只禁用事务外查询，`TestMongoCommitterCASIdempotencySnapshotAndOutbox` 稳定失败 `replay must not open another transaction`；实际实现通过，复用 TxID 改内容仍拒绝。
- 最新代码完整故障矩阵 `receipt-fastpath-matrix-20260925`：**21/21 一次性通过**，脚本退出 0，无跳过/补跑。
- 最新代码原规模实载 `receipt-fastpath-strict-20-20260925`：**通过**，1200 成功、0 错误/丢弃；全量 Mongo/NATS 校验、后续 Flush、Go PASS 和脚本退出 0 齐全。
- 同代码 30 分钟实载 `receipt-fastpath-strict-20-30m-20260925`：**未通过**，35996 成功、4 错误、0 丢弃，首错 `nest: sync timeout`；完整结果见下方追加记录。不能用上面的 60 秒通过替代长稳验收。

结果以本文后续实际追加和原始产物为准，不用中间版本通过记录代替最终代码验收。30 分钟已重跑并保留失败证据；24 小时尚未执行。

## 负载口径

Apple M5 / Go 1.27.0 / GOMAXPROCS=4；真实 Mongo 三副本、Redis、NATS。1000 业务 worker、10000 实体，每事务 2 Entity × 2 DAO；strict / 20 TPS / 60 秒。5000 笔预热不计入吞吐，统计完成 TPS 包含排空；锁租期沿用容量脚本默认配置，短租期故障另由矩阵覆盖。

每次完整通过必须取得 `result.json.verified`、Go PASS 和外层脚本退出 0：20000 个 Mongo DAO 文档和 20000 个 NATS 快照版本/值，以及拒绝/panic 回滚、outbox 全量检查。延迟为 **Remote strict 事务请求返回时间**，包含持久化确认，不是客户端 Sync 的 50ms 同步延迟。

## 最终原规模结果

计时负载加请求排空约 60.060 秒，完整测试含初始化/预热/全量校验约 318.80 秒。

| 指标 | 最终代码 |
| --- | ---: |
| 成功 / 错误 / 丢弃 | **1200 / 0 / 0** |
| 完成 TPS | **19.980** |
| p50 / p95 / p99 | **59 / 435 / 624 ms** |
| 最大延迟 | 988 ms |
| 最大采样 WAL 积压 | 6 |
| 计时结束采样 WAL 积压 | 1 |
| 后续 runtime.Flush / outbox 校验 | 通过 / 无待发布记录 |

计时样本在请求回复后立即保存，最后一笔 checkpoint 尚在推进，因此样本记录 1 笔。之后 fixture 显式 Flush，再校验全部 DAO 和快照；不要把这个计时末采样写成“最终采样为 0”。

相比本轮追加回执快路前两次相同代码样本，p50 从 1137–1151ms 降至 59ms，p99 从 3346–3561ms 降至 624ms，采样积压不再持续增长。与更早的单轮 p99=386ms 基线相比，当前 p99 仍更高；不能用单轮数字宣称所有分位数都优于历史。当前验证了本机 20 TPS 负载正确完成和本轮重复事务优化的实际改善，**不等于最大吞吐或长稳验收**。

最终日志（包含预热）14,492,112 B、50 次全堆栈，压缩后 570,587 B；相对前一轮约 1.45GB 文本减少约 99%。完整汇总见该目录 `analysis.json`。

## 追加：30 分钟实跑（未通过）

2026-09-25，按用户要求跑满 30 分钟，没有修改代码或部署。保持上述 1000 worker / 10000 实体 / 每事务 2 Entity × 2 DAO、strict、20 TPS、GOMAXPROCS=4；108 项记录源码 hash 与上面 60 秒样本完全相同。正式输入 1800 秒，加请求排空为 1800.048 秒；Go 测试含初始化与预热约 2061.77 秒。

| 指标 | 30 分钟实测 |
| --- | ---: |
| 输入 / 成功 / 错误 / 丢弃 | 36000 / **35996 / 4 / 0** |
| 成功完成 TPS | **19.997** |
| 错误率 | 0.0111% |
| 成功请求 p50 / p95 / p99 | **55 / 646 / 1506 ms** |
| 成功请求最大延迟 | 4903 ms |
| 10 秒窗口成功 TPS 范围 | 16.402～22.002 |
| 最大采样 / 末次采样 WAL 积压 | 25 / 1 |
| 投影失败 / 永久冲突计数 | 0 / 0 |
| 采样堆内存最小 / 最大 / 结束 | 72.6 / 235.3 / 118.2 MiB |
| 活跃负载采样 goroutine 范围 | 1190～1241 |
| Go 测试 / 外层脚本 | FAIL / 退出 1 |
| `.verified` / 最终全量一致性校验 | 无 / 未执行 |

10 秒窗口可以高于输入 20 TPS，因为之前未完成的请求随后完成。前两个 10 分钟窗口分别完成 11998、12000 笔且无错误；最后窗口完成 11998 笔并记录 4 个错误。约第 27 分 10 秒的采样首次出现错误，首个错误为 `nest: sync timeout`；当前结果只保存首错，不能据此断言其余三个错误都相同。

**本轮证明了平均成功吞吐接近 20 TPS，同时暴露长尾和超时，不能宣布长稳验收通过。** 成功请求 p99 从短测的 624ms 增至 1506ms；失败请求不包含在延迟分位数中。此值是 Remote strict 持久事务延迟，不是 Sync 的同步延迟；本轮固定 20 TPS 输入，也不代表最大吞吐能力。

负载检测到错误后，夹具在最终 Flush、拒绝/panic 回滚、全量 Mongo/NATS 和 outbox 校验前终止。超时不等于事务未提交：末次投影计数为 41000（包含 5000 笔预热），但计数不能替代逐实体值/版本校验。保留末次 WAL 积压 1 的原始事实，不能写成已排空，也不能推断丢数据或全部数据正确。

待定位的是这次超时的具体等待阶段；现有采样表明有短时积压，但不能直接归因于 Mongo、NATS、GC 或 Nest。后续应围绕错误时段补充阶段耗时和复现证据，再决定代码修复，不通过放宽超时或减少输入掩盖失败。本次仅实跑与记录结果，未宣称修复该问题。

原始产物：`artifacts/perf/remote/receipt-fastpath-strict-20-30m-20260925/`，包含 `env.txt`、`preflight.log`、`run.log.gz`、`result.json`、10 秒采样 `result.json.jsonl`、汇总 `analysis.json` 和实际外层退出码 `exit-code.txt`。保留失败日志，不补造 `.verified`。

复跑命令（使用新 label）：

```bash
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off ROOST_IT_TOXIPROXY=1
ROOST_REMOTE_LABEL="remote-30m-$(date +%Y%m%d-%H%M%S)" ROOST_REMOTE_POLICY=strict \
  ROOST_REMOTE_DURATION=30m ROOST_REMOTE_RATE=20 bash scripts/perf/remote.sh
```

## 中间样本：保留退化和失败证据

下表均尚未包含最终的回执快路，不能作为最终版本性能结论。p99 统计成功请求，含错误样本不能与健康样本直接比较。

| 样本目录（artifacts/perf/remote/ 下） | 完成 TPS | 成功/错误/丢弃 | p99 ms | 最大采样 WAL 积压 | 全量校验 |
| --- | ---: | ---: | ---: | ---: | --- |
| authority-bulkmeta-strict-20-20260925（前一轮） | 19.950 | 1200/0/0 | 386 | 4 | 通过 |
| unified-strict-20-20260925 | 19.461 | 1200/0/0 | 3561 | 27 | 通过 |
| unified-control-strict-20-20260925 | 19.866 | 1200/0/0 | 721 | 6 | 通过 |
| unified-repeat-strict-20-20260925 | 19.575 | 1200/0/0 | 3346 | 23 | 通过 |
| unified-readcheck-strict-20-20260925 | 11.090 | 721/479/0 | 4940 | 见原始采样 | 未通过，nest: sync timeout |

`unified-control` 使用构建 overlay 恢复旧 ownership 二次读取和每请求全堆栈；`unified-readcheck` 只恢复 ownership 读取，保留堆栈限频。两者始终保留持久权限和原子 CAS，未修改工作区源码。对照源码/overlay/manifest 随产物保存。首次和第二次统一版本的源码 hash 完全一致。

中间结果表明：只靠少读一次和减少堆栈不能证明容量改善，恢复一次 ownership 读取也未解决超时，因此继续采集运行跟踪，而非只选择最好样本。

## 跟踪发现与追加修复

`unified-diagnostic-20260925` 使用 1000 实体/1000 worker、20 TPS/30 秒采集 CPU 和 Go trace。这是较小规模诊断：548 成功、52 超时，没有 `.verified`，不作为验收通过。

约 64.86 秒运行的 CPU 样本累计 10.51 秒，没有支持 Go 计算占满四核的猜测。Go trace 进一步显示，在 1100 笔预热+正式事务中：

- `MongoStore.Project → Manager.ApplyRemoteCommits → MongoCommitter.CommitRemoteBatch` 的第二次事务网络等待累计约 **10.847 秒**。
- 首次 DataEngine 原子事务提交路径网络等待累计约 **10.191 秒**。
- grant、发布确认和 checkpoint 也有成本；本轮首先消除可确定避免的重复事务。

这些是跨 goroutine 累计等待，不能直接当作端到端比例。当前裁剪 Go 工具链缺少 `go tool trace` 命令，使用标准库 `internal/trace.Reader` 和临时构建 overlay 读取 trace；没有修改 GOROOT。辅助源码与 `waits.json`、`cpu-top.txt`、原始 CPU/trace 一并保存。

回执快路只读取已经 majority 持久化、digest 一致的成功 transaction 文档；未存在则保持原事务。没有降低持久级别，也没有跳过最新许可校验。首次独立 CommitRemoteBatch 会多一次存在性读取；正式 DataEngine 新写直接走调用方事务，发布阶段节省一次只读事务。

## 产物与复跑

- `artifacts/perf/remote/unified-verification-20260925/`：race/vet、回执快路负对照、中间样本汇总与旧日志体积证据。
- `artifacts/perf/remote/unified-matrix-20260925/`：追加回执快路前的 21/21 故障矩阵。
- `artifacts/perf/remote/receipt-fastpath-matrix-20260925/`：最终代码矩阵。
- `artifacts/perf/remote/receipt-fastpath-strict-20-20260925/`：最终代码原规模负载。

```bash
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off ROOST_IT_TOXIPROXY=1
ROOST_REMOTE_LABEL="remote-check-$(date +%Y%m%d-%H%M%S)" ROOST_REMOTE_POLICY=strict \
  ROOST_REMOTE_DURATION=60s ROOST_REMOTE_RATE=20 bash scripts/perf/remote.sh
```

长期运行把 duration 改为 `30m` 或 `24h`，使用新的 label。不能与故障矩阵同时操作同一隔离环境。需要诊断时可设置绝对路径 `ROOST_REMOTE_CPU_PROFILE` 和 `ROOST_REMOTE_TRACE`；诊断与正常性能结果分开解释。

索引已更新至 `2026-09-25T01:22:04Z`，本次修改的代码覆盖元数据新鲜、没有记录缺口；3 个历史模板解析缺口与本轮无关。docs、脚本和生成夹具按排除规则直接读取。干净覆盖不等于完整性证明。
