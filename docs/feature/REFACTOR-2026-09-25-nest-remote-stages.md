# Nest Remote 慢操作隔离与容量上限验收

后续演进：四池已由 [快慢双池方案](REFACTOR-2026-09-25-nest-fast-slow.md) 收敛，本文保留当时容量与故障证据，不代表当前路由配置。

状态：代码、容量、race/vet 和 21/21 故障矩阵验收已完成；未部署。沿用严格持久化和原请求等待时间。

## 当前问题与目标

Remote 请求当前进入 cost worker，从获取分布式锁、加载、业务执行直到持久化确认及 Close 都占用同一个 worker。同一 hash 槽上的无关业务因此排队。前次 60 TPS 长稳通过证明可用容量下限，不能当作 TPS 上限。

保持 `nest/` 单包，新增 `remote_dispatch.go` 放置慢操作与逻辑阶段交接；`nest_dispatch.go` 保留请求生命周期与业务路由，`dispatcher.go` 管理池的准入和排空，`msg.go` 保留消息所有权。无包迁移、无存储格式或网络协议变化。

## 实施契约

1. Remote 写或声明 RemoteAccess 的请求进入独立、有限队列的 remote worker，默认并发等于业务 worker，可用启动 option 单独设置。按原 Key 哈希保持同 key Remote 请求整条生命周期的顺序；多实体交叉冲突仍由有序写门和 Entity Guard 仲裁，不承诺跨 key 的全局 FIFO。
2. 慢 worker 完成快照读取和写批次准备后，向已有 cost 逻辑池投递一个独立信封。原 Msg 只由慢 worker 持有引用，逻辑阶段借用；逻辑信封释放后才通知慢 worker 继续，不并发修改 Msg 的 RefCount，不传递已持有的 Entity 锁。
3. 逻辑 worker 建立自己的 fctx、Guard、事务上下文，执行 handler、FinalizeLocked、WAL 持久准入并释放 Entity 锁。框架上下文通过 snapshot 交接；回滚/标记未知仍在锁内完成。这些纯内存步骤不移出锁，严格 WAL 等待仍保留原契约。
4. 慢 worker 随后执行 Commit、Close、确认后 Sync 回调以及一次性回复。它自己等待逻辑阶段后继续释放，不把释放任务再排到可能全部阻塞在获取上的前置队列。准入满返回背压，不在逻辑 worker 退回同步 Remote I/O。
5. 停机先停止外部准入和延迟泵，排空 remote 池，再停止逻辑池，保证已准入的前置工作仍能完成投递。Fence/取消在逻辑开始前再次检查；发生错误仍关闭已获取的批次。停机调用者超时不取消后台排空。
6. 只读 RemoteAccess 的准备也走慢池。普通本地消息保持原路由。现有直接调用 NestDispatch 的内部测试/单次执行继续支持同步路径。

## 验证与容量方法

- 用事件屏障阻塞 prepare / commit / close，证明同 cost 槽本地业务仍能完成；验证快照上下文、同 key 顺序、错误与 panic、取消、过载、Fence 和停机排空，运行 race。
- 正式生成业务链路：1000 会话、10000 Entity、每事务 2 Entity × 2 DAO、strict、5 秒请求等待、真实 Mongo/Redis/NATS，默认 8 个 Remote 投影 worker。
- 逐级提高固定输入速率，保存成功 TPS、错误/丢弃、p95/p99/max、WAL 与队列变化及完整数据校验。找到失败上界后缩小区间，在最高通过档延长验证。容量以零错误/丢弃且积压可排空的持续完成速率衡量，不能用失败请求之外的直方图掩盖超时。
- 实测结果仅适用于明确记录的硬件、并发、数据和存储拓扑，不宣称无限精确的通用最大 TPS。保留失败档原始证据；24 小时稳定性另行复跑。

代码回退：撤销 NewEngine 的 Remote 慢池装配可恢复原同步执行；回退不涉及持久化迁移。默认隔离属于调度行为变化，队列满、取消和停机均需回归验证。

## 初始容量阶梯（默认慢池 64）

Apple M5、Go 1.27.0、GOMAXPROCS=4；Nest 逻辑 64、Remote 慢池 64、投影 8；1000 会话、10000 Entity、每事务 2 Entity × 2 DAO。每档 120 秒固定输入，计时 TPS 包含最后在途请求等待，不含 5000 笔冷初始化预热。成功延迟分位数不包含错误请求。

| 输入 TPS | 成功 / 错误 / 丢弃 | 成功回复 TPS | p95 / p99 / max ms | 判定 |
| ---: | --- | ---: | --- | --- |
| 80 | 9600 / 0 / 0 | 79.733 | 733 / 1106 / 1801 | 全量 Mongo/NATS/rollback/outbox 校验通过 |
| 90 | 10800 / 0 / 0 | 89.716 | 1139 / 1729 / 3083 | 短测及全量校验通过；仍需延长验证 |
| 100 | 11986 / 14 / 0 | 98.681 | 2082 / 3238 / 4915 | sync timeout，不通过 |
| 120 | 1754 / 12646 / 0 | 14.032 | 4665 / 4943 / 5000 | 持续过载，不通过 |

100 TPS 的 Remote 慢池 queue 平均 431.89ms / 最大 4890.56ms，logic_queue 平均 0.0056ms / 最大 3.85ms，remote_confirm 平均 373.47ms / 最大 595.90ms。最终采样 WAL 已清零，但 14 个错误仍使这一档失败，不能以排空抵消超时。90 TPS 的对应均值为 163.40ms / 0.0049ms / 284.26ms。

120 TPS 稳态饱和区每 10 秒约投影 850～890 笔，最终计时采样 WAL 积压 2847。请求超时仍可能落库，所以 14.032 是成功回复 TPS，不是数据库吞吐。失败档按夹具契约跳过最终全量值校验，没有 `.verified`，保留全部失败证据。

原始目录：`artifacts/perf/remote/staged-capacity-20260925-{80,120}`、`staged-refine-20260925-{90,100}`，每档包含 JSON/JSONL、压缩日志、环境/源码摘要、退出码和 `analysis.json`。后续长测和慢池并发对照是决定最终容量的依据。

## 延长验证与慢池并发对照

默认慢池 64 的 90 TPS 延长至 10 分钟：53994 成功、6 次 sync timeout、0 丢弃，成功回复 89.827 TPS；p95/p99/max=2014/3059/4931ms。最大采样 WAL=51，结束=0，logic queue 最大采样=0。慢池 queue 最大阶段耗时 4854.26ms。**延长验收失败，取代短测零错误对容量的乐观判断**；无最终全量校验标记。

仅把慢池提高到 128，逻辑仍为 64、投影仍为 8，100 TPS × 120 秒：11883 成功、117 次超时、0 丢弃，成功回复 97.512 TPS，p95/p99/max=3438/4517/4995ms，最大采样 WAL=97。这一档也失败，不能推荐简单增加慢池并发。不同测试存在运行时波动，该对照证明这一配置未通过，不单凭一次测试归因于并发数导致性能变差。

对应原始目录 `staged-sustain-10m-20260925-90`、`staged-io128-20260925-100`。最终持续验收回到默认 64 慢 worker，以 80 TPS 运行 10 分钟。

## 最终容量验收：80 TPS × 10 分钟

`staged-final-10m-20260925-80` 保持逻辑 64 / 慢池 64 / 投影 8，正式输入 600 秒。48000 成功、0 错误、0 丢弃，实际成功完成 **79.967 TPS**。p50/p95/p99/max 分别为 **315/681/997/2444ms**。最大采样 WAL 积压 36，计时结束已清零；Remote 最大采样排队 22，cost 逻辑队列最大采样为 0。阶段统计的逻辑排队平均 0.0065ms、慢池排队平均 78.72ms、Remote 确认平均 234.38ms。

输入结束后的 Flush、20000 个 Mongo DAO 文档、20000 个真实 NATS 快照、业务拒绝/panic 回滚及 outbox 校验全部通过；原始 Go PASS、`.verified`、脚本退出 0 齐全。没有用成功请求的延迟直方图掩盖错误，错误计数本轮为 0。

**当前机器/模型/默认并发下，已验证的 10 分钟零超时能力为 80 TPS；90 TPS 延长测试不通过，因此持续容量边界可定位在 80～90 TPS 这一档位区间。** 这不是精确数学最大值，也不是其他硬件、持久级别或业务形状的通用上限。90 TPS 的 120 秒通过不能替代 10 分钟失败。128 慢 worker 的 100 TPS 也未通过，保留默认值，未用调大并发、延长 5 秒超时或放宽 strict 来宣称达标。常态容量规划应在已验证档位以下留余量。

本轮 7 档运行记录中的 59 个非测试源码文件摘要完全一致（`staged-verification-20260925/source-comparison.json`）；只有并发、输入速率和时长按记录变更。24 小时未执行；10 分钟容量结果不能替代 24 小时长稳或所有生产负载形状。

## 功能与索引验收

最终六包 `go test -race`、相同范围 integration-tag `go vet`、脚本语法与 `git diff --check` 已通过；新加的 Remote 阶段测试连续执行 3 次通过，负对照恢复旧 cost 路由后三个阻塞场景均失败。日志在 `artifacts/perf/remote/staged-verification-20260925/`。

图谱更新到 `2026-09-25T08:12:52Z`，18239 nodes / 145219 edges；本轮 10 个核心实现/测试路径覆盖元数据新鲜且无记录缺口。3 个历史模板解析缺口与本轮无关；文档、脚本和生成夹具继续按排除规则直接读取，图谱覆盖结果不代表完备性证明。

故障矩阵 `staged-matrix-20260925` **21/21 一次性通过**，无跳过、无补跑，脚本退出 0；包括 12 个 Mongo/NATS × 持久化模式组合、进程终止/分区、Redis 集群切换与未复制 fence、持久权限、ownership、Mongo/WAL 恢复、broker 选主/网络故障和最终健康检查。

## 复跑入口

```bash
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off ROOST_IT_TOXIPROXY=1
ROOST_REMOTE_CAPACITY_LABEL="capacity-$(date +%Y%m%d-%H%M%S)" \
  ROOST_REMOTE_RATES='80 90 100' ROOST_REMOTE_DURATION=120s \
  ROOST_REMOTE_POLICY=strict ROOST_REMOTE_WORKERS=64 ROOST_REMOTE_IO_WORKERS=64 \
  ROOST_REMOTE_PROJECTION_WORKERS=8 bash scripts/perf/remote-capacity.sh
```

脚本在第一档失败后停止并保留退出码。持续验收用 `ROOST_REMOTE_RATES=80 ROOST_REMOTE_DURATION=10m`；24 小时复跑用 `ROOST_REMOTE_DURATION=24h`，每次采用新标签。不要与故障矩阵同时操作同一个隔离环境。
