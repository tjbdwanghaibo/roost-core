# 资源预算与会话集中恢复

日期：2026-09-25。状态：已实施并完成本批验证；未部署，性能边界见验收记录。

## 当前问题与范围

Nest 已收敛为快、慢两池，业务与 Entity local 锁只在快池。最近压测证明，慢池扩大至 1024 后，排队时间很短，但大量 Remote 写等待 Mongo/WAL 确认，吞吐反而下降。慢任务并发能力与后端安全写并发必须分别配置。

Remote 现有 finalizeSlots 从 Prepare 持有到真实 Close（包括不确定事务的后台收尾），但容量仅由 AsyncFinalizeCapacity 控制，默认 4096。复用这条完整生命周期，不增加另一个等待队列。

Sync 的 HoldSession、ReadySession、CloseSession 每次复制并扫描全部 subject。1000 会话、10000 实体、每会话约 50 个订阅时，恢复开销与全服实体数相乘。

## 目标结构与调用路径

目录和包均保持不变，不迁移导出符号：

```
nest/                       同 ID FIFO、快慢调度不变
remoteentity/               config.go / transaction_manager.go：写事务预算
kit/remoteentity/           配置、健康状态
sync/entitysync/            session.go / subscriptions.go：生命周期订阅反向索引
                            manager.go / flush.go：统一删除订阅与结算
codegen/.../testdata/remoteflow/ 正式生成链路压测入口
```

资源路径：Nest 慢阶段 → Remote 非阻塞预留写额度 → 获取远端权限 → 快池业务 → DataEngine WAL 原子准入 → 投影确认 → 释放 Remote 额度。额度不足直接 ErrRemoteOverloaded，不能占住 worker 等 permit，也不能自动重排同 ID 消息。DataEngine 仍用已有 MaxUnackedRecords 原子限制全部事务的未确认记录；不以 Stats 的竞态采样替代准入，不从 Remote 引入 DataEngine 包依赖。

新增 MaxConcurrentWrites：DefaultConfig 为 128；显式零值沿用原 AsyncFinalizeCapacity 行为。有效额度取它与收尾容量的较小值，保证每个准入写事务总有收尾空间。用户可按存储能力覆盖，128 只是保护起点，并非通用最优值。kit 增加 remote_entity.max_concurrent_writes（允许显式 0，拒绝负值），Stats/健康状态公开在途、有效上限和拒绝数。超时且结果不确定的事务仍持有额度到收尾完成；慢 worker 数不再决定 Remote 最大压力。

Sync 在 sessionLifetime 保存实际订阅 subject 索引，由 Manager.mu 保护，Hold/编码 clone 共享同一 lifetime，Close/重开使用新 lifetime。它包含待全量、在途、待 remove 的订阅，不能用已交付 objects 替代。订阅新增/删除同时维护双向关系，锁序统一 subject.mu → Manager.mu；会话批量操作在 Manager.mu 下只复制目标集合，释放后才锁 subject。旧生命周期结算不得删除新会话索引。Ready 批量入 pending 后只唤醒一次。

保持 setter 仅标脏、guard 释放前捕获、提交确认后同步、实体包帧边界、版本/epoch 与 ACK 契约不变。

## 实施与验收

1. 先记录 Sync 恢复微基准基线（1000 会话，50 可见，1000/10000 实体），然后改反向索引。检查订阅来源、Hold/Ready、退订/退役、旧帧与 Close/重开、并发操作；race 验证。
2. Remote 独立预算、指标和 kit 配置。覆盖并发满额拒绝、Close/失败回收、不确定事务保留额度、配置兼容性；沿用 DataEngine 原子 WAL 限额回归。
3. 正式生成 Remote 链路支持独立写预算及 WAL 额度参数；针对 1024 慢 worker + 128 写额度运行 100 TPS 短验收。明确拒绝、超时、成功吞吐、最终积压和数据验证，不以限流后的低错误数冒充容量提升。
4. Sync 1000/10000/50、20Hz、1% 变化、1000 会话集中恢复正式 AOI 压测，核对最终版本/队列与恢复耗时。微基准不代表端到端延迟，短测不替代 30 分钟/生产故障验收。
5. 更新文档及索引。确认 bug 单独登记问题和修复；纯重构只记本方案与验收。

## 风险与回退

反向索引额外消耗 O(订阅边数) 内存，删除路径遗漏会泄漏或漏恢复，必须集中删除并覆盖生命周期。资源限制会改变过载时的拒绝时机，新默认需明确记录；如需旧容量可显式置 0。无需迁移持久化或客户端协议；回退只还原本批源码，不回退历史工作。

## 实施记录与已有验证

已完成独立写预算、kit 配置/健康检查、Stats/拒绝计数、Sync 生命周期反向索引和统一删除入口，未新增包。源码审查发现并修复 [RR-20260925-10](../bugfix/RR-20260925-10.md)：Flush 必须核对旧订阅的 lifetime，防止相同 ID 新连接接收旧订阅。

七包 race：`remoteentity`、`kit/remoteentity`、`sync/entitysync`、`sync/entitysync/policy`、`nest`、`dataengine/engine`、`kit/dataengine` 通过。关键新用例重复 race 10 次通过，覆盖实际远端获取之前拒绝、并发 64 批只准入 8 批、失败/重复 Close 归还、不确定结果保留额度、普通慢业务继续推进和同 ID 后继执行；Sync 覆盖各删除路径及并发生命周期。

### 会话恢复微基准

Go 1.27.0 / Apple M5 / CPU=4，同一 benchmark，`-benchtime=1x -count=3`。每轮 1000 会话 × 50 订阅，仅 Hold/Ready 调用，不含网络、编码及快照到齐时间。

| 全服实体 | 修前每轮耗时 | 修后每轮耗时 | 修前/后分配字节 |
| --- | --- | --- | --- |
| 1000 | 39.23–52.40ms | 1.80–1.82ms | 16.67MB → 1.176MB |
| 10000 | 229.99–235.45ms | 1.58–1.69ms | 164.13MB → 1.176MB |

10000 实体中位数约 230.38 → 1.615ms（约 143 倍）；原因是消除每会话全场扫描，不能将倍数外推到整个 Sync。分配次数 7000 → 8000，新 lifetime 索引增加小对象；表中分配量仅本轮临时分配，不是索引常驻内存。反向索引的常驻空间为 O(实际订阅边数)。原始日志位于 `artifacts/perf/sync/session-recovery-20260925/`。

### Remote 首轮 100 TPS 过载验证

120 秒固定输入 12000 笔，1024/16 慢池、8/4096 快池、128 写预算、512 WAL 限额、8 投影；1000 会话、10000 实体、2 Entity × 2 DAO，strict。10552 成功、1448 错误、0 dropped，成功吞吐 87.35 TPS，成功请求 p99 1562ms。WriteRejected=1448，前 32 条错误样本均为容量拒绝；测量结束 WAL 和写在途都为 0。存在错误，测试按约定未执行全量精确数据校验，退出 1，不算 100 TPS 验收通过。

该轮开始附近与本次新增回归/微基准有短暂重叠，故不用于宣称存储容量回退或精确上限；有效结论是写预算能够及时拒绝并阻止在途无界堆积。后续无拒绝档位与 Sync 正式性能测试按顺序独立执行。原始目录 `artifacts/perf/remote/resource-budget-20260925-w1024-limit128-rate100/`。

### Remote 80 TPS 独立验收通过

相同配置，仅输入改为 80 TPS，计时阶段未并行运行其他测试。120 秒计划输入，9600 成功 / 0 错误 / 0 dropped，含收尾 120.394 秒，实测 **79.74 TPS**。p50 685ms、p95 983ms、p99 1021ms、max 1068ms。全量 Mongo / NATS 快照、回滚与 outbox 校验完成，`.verified` 存在，脚本退出 0；结束时 WAL、Remote 活跃事务及写额度全部归零。

该结果证明 1024 通用慢 worker 配独立写预算在当前负载下可用；不是存储物理 TPS 上限、不是 Sync 客户端延迟，也不能据此把所有服务的慢池默认改成 1024。未重复 30 分钟/24 小时长稳及完整故障矩阵。

复跑（需本机隔离环境已建立，每次使用新 label）：

```bash
source /tmp/roost-dataengine-it/env.sh
GOCACHE=/tmp/roost-nest-go-cache \
ROOST_REMOTE_LABEL=budget-unique-label \
ROOST_REMOTE_DURATION=120s ROOST_REMOTE_TIMEOUT=10m ROOST_REMOTE_RATE=80 \
ROOST_REMOTE_WORKERS=8 ROOST_REMOTE_FAST_QUEUE=4096 \
ROOST_REMOTE_IO_WORKERS=1024 ROOST_REMOTE_SLOW_QUEUE=16 \
ROOST_REMOTE_WRITE_LIMIT=128 ROOST_REMOTE_WAL_LIMIT=512 \
ROOST_REMOTE_PROJECTION_WORKERS=8 bash scripts/perf/remote.sh
```

原始目录：`artifacts/perf/remote/resource-budget-20260925-w1024-limit128-rate80/`。每批预热 5000 笔不计 TPS；10000 Entity、1000 会话、2 Entity × 2 DAO、strict 保持不变。

### Sync 正式 AOI 集中恢复（1% 变化）

两轮顺序独立运行，1000 玩家、10000 Entity、AOI 平均可见 49.59、20Hz、200 tick、异步可靠队列与回环 TCP；第 50 tick 对 1000 个现有会话 Hold/Ready，快照预算每窗口全局 1000、每会话 10。不是 TCP 连接重建或鉴权压测。

| 模式 | 集中重置调用耗时 | 最大服务端 active work | 实际变化→客户端 p99 / max | 计划事件→客户端 p99 / max | 50ms 门禁 |
| --- | --- | --- | --- | --- | --- |
| periodic | 9.689ms | 10.877ms | 55.709ms / 65.752ms | 59.601ms / 68.936ms | 未通过，计划事件 15409/84161 超标 |
| on_change | 11.581ms | 13.122ms | 10.14ms / 48.639ms | 11.114ms / 49.782ms | 本轮通过，92599 样本零超标 |

on_change 结束时 pending=0、SessionsLost=0、FlushFailures=0，可靠队列、在途发送及待发送字节均为 0；快照/帧解码、可见实体与最终版本校验通过。periodic 亦完成数据/交付一致性检查，退出 1 的原因是 50ms 延迟门禁，不是功能错误。周期模式本身最多等待一个 50ms 周期再加处理/传输开销，不能承诺端到端所有样本小于 50ms。

本次优化的是恢复遍历与锁访问开销；快照预算仍决定全量到齐速度，不能把 11.581ms 当作 1000 客户端全部恢复完毕。单轮 1% 门禁通过不替代 5%/长稳/真实网络验收。

原始目录：`artifacts/perf/sync/session-recovery-20260925-periodic/`、`session-recovery-20260925-onchange/`。复跑：

```bash
GOCACHE=/tmp/roost-nest-go-cache ROOST_PERF_LABEL=recovery-unique-label \
ROOST_PERF_COUNT=1 ROOST_PERF_CPU=4 bash scripts/perf/sync-aoi.sh \
  -players=1000 -entities=10000 -visible=50 -hz=20 -dirty=1 -ticks=200 \
  -async -mode=on_change -snapshot-objects=1000 -snapshot-per-session=10 \
  -reconnect-tick=50 -reconnect-players=1000
```

### Sync 5% 变化补充与验收边界

同样 1000 会话集中恢复，仅变化比例提升到 5%。实际变化→客户端 p99 **12.933ms**，计划事件→客户端 p99 **15.444ms**；458667 个样本中分别有 **3 / 5** 个超过 50ms，最大分别 **915.231 / 916.438ms**。因此该档脚本退出 1，不能宣称严格 50ms 全量达标。

集中重置调用 6.880ms，服务端 active work 最大 38.391ms。最终 pending、可靠发送队列、在途字节均为 0；会话丢失、捕获失败、发送失败为 0；最终实体状态及版本校验完成。原始目录 `artifacts/perf/sync/session-recovery-20260925-onchange-dirty5/`。

已去掉恢复期间每会话扫描全服 Entity 的阻塞；剩余端到端长尾不能仅靠减少这段 CPU 时间消除。这里全量恢复仍被 1000 对象/50ms 的预算主动分摊，约 5 万条订阅需要多个窗口。当前日志没有逐实体超标追踪，不能把 915ms 精确归因给某一队列，也不能偷偷排除这些样本。快照预算、恢复完成时间与极端样本的进一步定位属于后续性能工作，协议/交付语义本轮未调整。

## 收尾状态

本批方案已实施，边界和未达标项如上；代码未部署。新增历史问题与修复文档均已归档，配置与观测说明更新至 kit/README、OBSERVABILITY、sync/README 和 scripts/README。七包 race、重点 race ×10、七包 vet 及 diff whitespace 检查通过；最终单独重跑 Nest 新回归通过。

图谱已更新到 `2026-09-25T11:41:37Z`（18298 nodes / 146100 edges），本批全部源码和测试路径 coverage 为 metadata_match、无记录缺口。仍有三个与本批无关的历史模板 parse_partial；脚本、生成夹具、docs 和性能产物按规则不进索引，已直接读源验证。干净 coverage 不是完备性证明。
