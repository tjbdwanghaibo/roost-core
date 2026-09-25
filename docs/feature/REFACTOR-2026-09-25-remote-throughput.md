# Remote 超时定位与 50 TPS 优化

## 目标与证据

用户要求查明 30 分钟负载中的 `nest: sync timeout`，并达到至少 50 TPS。沿用 1000 worker、10000 Entity、每事务 2 Entity × 2 DAO、strict 和真实 Mongo/Redis/NATS。不得以延长 5 秒请求等待、降低持久级别或减少数据校验代替优化。

修前证据：`receipt-fastpath-strict-20-30m-20260925` 跑满 1800 秒，35996 成功、4 错误，p99=1506ms；首错来自 Nest waitResult 的 5 秒 timer。堆栈包含 Projector → MongoStore.Project → Mongo CommitTransaction 等待。原始记录未包含每个失败请求的阶段，不能仅凭堆栈把四笔超时都归因于同一个数据库调用。

当前结构：ReplayPass 按 WAL 读入，Remote 每笔成为独立 segment，随后串行执行 Project：Mongo 原子提交 → 回执读取 → 逐实体快照发布 → 标记发布 → checkpoint。不同 Entity 也共享这个串行服务队列，延迟波动会传递到后续请求。先增加负载错误样本（实体、请求耗时、投影状态）和可选 Nest 阶段统计，采集 50 TPS 修前对照。

## 实施方案

目录保持 `dataengine/engine`、`kit/dataengine`、现有生成夹具，不新增包。独立投影调度实现放在同包 `projector_remote.go`，原 ReplayPass 保留本地批量、特殊事务与 checkpoint 主线。

1. Store 显式声明支持 Remote 并行投影及跨重放幂等。MongoStore 提供该能力；未知 Store 维持串行。
2. Projector 提供 RemoteProjectionWorkers 配置，默认 8，1 为串行对照；Kit 映射正式配置项。并行窗口同时受记录数、字节数和 worker 数限制。
3. 仅纯 Remote、无 effect/receipt/迁移、经过契约验证且各记录 EntityID 不重叠的相邻事务可并行。相同 Entity、相同事务身份和未知/混合内容作为窗口边界；不跨越 held WAL 记录。
4. 每笔业务仍使用自己的 Mongo 原子事务与持久回执；成功后才完成其投影 ticket。一个窗口全部返回后，checkpoint 只推进连续成功前缀。失败后不运行后续窗口；已成功的后缀保留在 WAL，由持久事务身份安全重放。
5. 不改变 WAL/DAO/Remote 协议，不引入跨业务合并原子性，不放宽写许可、版本、ownership 校验或 strict 完成条件。

## 验证与完成条件

- 控制事件顺序验证独立实体可同时进入存储、重叠实体及特殊记录保持顺序、失败后缀不被错误确认、重放/取消/配置上限；适用包 race 与 vet。
- 同一 fixture 50 TPS 修前/修后对照，保留错误与阶段统计；正式多实体/多 DAO 生成工程检查 Mongo/NATS/outbox。
- 通过短测后扩大持续时间，并在最终报告明确实测 TPS、错误、延迟、积压和一致性结果；长期零错误不能由单轮短测代替。

状态：已实施；相关 race/vet、21/21 故障矩阵和 30 分钟 60 TPS 全量验收通过。详见下面的最终实测；未部署。

## 第一轮实测与阶段定位

Apple M5 / Go 1.27.0 / GOMAXPROCS=4，同一 1000 worker、10000 Entity、2 Entity × 2 DAO 模型，两轮均启用 Nest 阶段 metrics，使用真实 Mongo/Redis/NATS。

| 版本 | 输入速率 / 持续时间 | 成功 / 错误 / 丢弃 | 成功完成 TPS | p50/p95/p99 ms | 全量校验 |
| --- | --- | --- | ---: | --- | --- |
| 修前串行 | 50 TPS / 60 秒 | 143 / 2857 / 0 | 2.200 | 2431/4664/4892 | 未执行，负载失败 |
| 并行投影默认 8 | 60 TPS / 120 秒 | 7200 / 0 / 0 | 59.916 | 174/314/432 | 全部通过 |

修后最大成功延迟 681ms，最大采样 WAL 积压 15；后续 Flush、20000 个 Mongo 文档、20000 个 NATS 快照、拒绝/panic 回滚和 outbox 校验通过，`.verified`、Go PASS、脚本退出 0 齐全。

定位依据：修前 remote_update 平均 queue=3587.28ms、remote_confirm=1268.71ms、durable_commit=14.79ms；修后较高输入下分别为 15.26ms、119.58ms、16.84ms。业务 handler 很短，优化没有降低 WAL 持久级别，而是消除独立 Entity 之间的投影串行等待。阶段样本数量不同，不将阶段均值相加当作单笔耗时。

修前成功 TPS 只统计超时前收到成功回复的请求，后台仍可能继续落库，因此不能宣称数据库仅能处理 2.2 TPS。两轮原始失败和通过证据都保留：`timeout-baseline-50-20260925`、`parallel-remote-strict-60-20260925`（均位于 `artifacts/perf/remote/`），汇总见各自 `analysis.json`。

已确认问题登记 [RR-20260925-05](../bug/RR-20260925-05.md)，修复和负对照见[修复记录](../bugfix/RR-20260925-05.md)。短测之后继续完成以下长稳和故障验收。

## 最终验收：30 分钟 60 TPS

`parallel-remote-strict-60-30m-20260925` 正式输入 1800 秒，包含请求排空 1800.081 秒；完整 Go 测试含初始化、5000 笔预热和全量校验 1874.07 秒。预热不计入吞吐。记录的非测试源码 hash 与修后短测一致，期间没有修改代码/脚本，也没有注入故障。Nest 请求等待仍为默认 5 秒，Remote 并行 worker 使用正式默认值 8。

| 指标 | 实测 |
| --- | ---: |
| 成功 / 错误 / 丢弃 | **108000 / 0 / 0** |
| 成功完成 TPS | **59.997** |
| p50 / p95 / p99 | **124 / 262 / 845 ms** |
| 最大成功请求延迟 | 2518 ms |
| 最大采样 / 末次采样 WAL 积压 | 47 / **0** |
| 投影失败 / 永久冲突 | 0 / 0 |
| 采样堆内存最小 / 最大 / 结束 | 84.7 / 272.0 / 192.3 MiB |
| 活跃负载采样 goroutine 范围 | 1194～1282 |
| worker 退出后 goroutine / 活跃事务 | 190 / 0 |

三个 10 分钟窗口分别完成 35993、35998、36009 笔，成功 TPS 约 59.988、59.997、60.008，错误均为 0。最后窗口包含此前在途请求排空，可以略高于输入速率。后半程出现短时积压，但未持续增长或触发超时。阶段计量的平均 queue=13.69ms、remote_confirm=87.31ms、durable_commit=16.20ms。

`runtime.Flush`、20000 个 Mongo DAO 文档的版本/值、20000 个真实 NATS 快照、业务拒绝/panic 回滚、outbox 全量检查全部通过；`.verified`、Go PASS、外层脚本退出 0 齐全。末次计时样本本身就是 WAL 积压 0，未用后续 Flush 的结果改写采样。

最终功能验收：五包 race、同范围 integration-tag vet、脚本语法、`git diff --check` 均通过；`parallel-remote-matrix-20260925` **21/21 一次性通过**，含 Mongo/NATS 故障、Redis 故障接管、进程分区、持久权限和 WAL 恢复，无跳过/补跑。负对照与日志保存在 `parallel-remote-verification-20260925`。

本场景已满足至少 50 TPS 的目标；60 TPS 是实际验证过的持续输入，不是最大容量。延迟包含 Remote strict 持久事务确认，不是客户端 Sync 的 50ms 同步指标。24 小时仍未执行；本轮 WAL 未换段（最大 Segment=1），不能用这轮结果替代跨段恢复专项。

索引更新至 `2026-09-25T05:42:06Z`，新增与改动代码的覆盖元数据新鲜且无记录缺口；3 个历史模板解析缺口与本轮无关。脚本、生成夹具和文档仍按排除规则直接读取，覆盖结果不等于完整性证明。

## 复跑

```bash
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off ROOST_IT_TOXIPROXY=1
ROOST_REMOTE_LABEL="remote-60-$(date +%Y%m%d-%H%M%S)" ROOST_REMOTE_POLICY=strict \
  ROOST_REMOTE_DURATION=30m ROOST_REMOTE_RATE=60 ROOST_REMOTE_STAGE_METRICS=1 \
  bash scripts/perf/remote.sh
```

24 小时将 duration 改为 `24h`，使用新 label。串行对照可设置 `ROOST_REMOTE_PROJECTION_WORKERS=1`。不得与故障矩阵同时操作同一隔离环境。原始 JSON、10 秒采样、压缩日志、环境和源码摘要、汇总 `analysis.json`、实际退出码均在各自 `artifacts/perf/remote/<label>/` 中保留。
