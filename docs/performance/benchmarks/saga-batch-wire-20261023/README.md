# Saga 认领批量与 JetStream 交付拆分（2026-10-23）

本轮验证协调器 batch=1 能否减少低负载等待，以及交付延迟是否主要来自 NATS 客户端回调排队。只增加性能夹具配置和诊断脚本，未修改生产默认值或可靠性契约。

## 场景与证据

基于 `0ff5014c` 加本轮夹具改动，macOS 同机 Mongo 三副本 / JetStream 三副本，30 笔正向 Saga/s，每笔两步，每轮 1 分钟；每步 Mongo 消费 worker=2、完成消费 worker=2、协调/发布 worker 各4、publisher batch=1、GOMAXPROCS=4。Mongo majority+journal 保持不变。用户提示处于电池模式；pmset 报 AC Power 同时显示 discharging，不能视为稳定供电或独占服务器结果。

ABBA 四轮按顺序执行，无并行编译和其他压测。每轮1800/1800成功、业务核对通过、ManualRequired=0。各结果与时间线汇总在同目录 JSON；原始文件在两份 worktree 的 `artifacts/perf/saga/batch-20261023-{a3,b1,c1,d3}`。

| 顺序 | CoordinatorBatch | 服务端 P99 ms | 每笔协调等待/认领平均 ms |
| --- | ---: | ---: | ---: |
| A | 3 | 489.16 | 50.06 |
| B | 1 | 442.93 | 18.97 |
| C | 1 | 467.18 | 18.72 |
| D | 3 | 503.33 | 48.49 |

两组相邻对照均改善：P99 下降约7%～9%，协调等待减少约30ms/笔。支持“低负载先认领再逐个处理使首条等待后续认领”的解释；这不是高吞吐饱和测试，不据此改全局默认。

复跑：source 私有 env.sh 后设置 `ROOST_SAGA_PERF_MODE=forward ROOST_SAGA_PERF_RATE=30 ROOST_SAGA_PERF_DURATION=1m ROOST_SAGA_PERF_WORKERS=2 ROOST_SAGA_PERF_HOPS=1 ROOST_SAGA_PERF_COORDINATOR_BATCH=1 ROOST_PERF_LABEL=<唯一名称>`，运行 `bash scripts/perf/saga.sh`；batch=3 对照保持其余相同。

## JetStream 解析器拆分

另运行 batch=3 的1800笔诊断，每步共3600条命令与3600条结果，全部成功且埋点完整。临时复制 nats.go v1.53.1 依赖，以独立 modfile replace 和测试文件 overlay 插桩：在 `Conn.processMsg` 完成协议解析、进入订阅队列前记时间。未修改依赖缓存、生产驱动、协议或持久配置。

| 每条消息平均耗时 | 命令 ms | 结果 ms |
| --- | ---: | ---: |
| 发布开始→客户端解析完成 | 30.95 | 32.13 |
| 解析完成→业务订阅回调入口 | 1.43 | 1.10 |
| 发布开始→PubAck（嵌套口径，不相加） | 14.35 | 14.10 |

解析后回调排队只占平均交付时间约4%，不是本轮平均延迟的大头；但最慢1%事务中，结果回调排队平均累计44.28ms/笔，仍会贡献长尾。消费者 worker 槽位等待单独统计，未混进表中。

**不能把解析前30余ms称为 JetStream 本身的延迟或吞吐上限。** 它包含发布编码、服务端处理/持久化/复制、网络、接收进程调度和协议解析；本轮尚未拆开这些部分。30 Saga TPS 是施加的业务输入，每笔包含8次 Mongo事务和4条 JetStream业务消息，不是 NATS 容量测试。该轮服务端P99为598.9ms，高于ABBA轮次；可能有环境漂移和诊断开销，不归因给单一因素，也不与ABBA直接估算优化收益。

诊断生成器：`python3 scripts/perf/saga-wire-overlay.py --repo "$PWD" --nats-dir "$(go list -m -f '{{.Dir}}' github.com/nats-io/nats.go)" --output /tmp/roost-saga-wire-unique`。输出目录必须不存在。使用生成的 `diagnostic.mod` 和 `overlay.json`：

```sh
GOWORK=off go test -modfile=/tmp/roost-saga-wire-unique/diagnostic.mod \
  -overlay=/tmp/roost-saga-wire-unique/overlay.json -tags=integration \
  -c -o /tmp/roost-saga-wire-unique/saga.test ./framework/saga
```

source 私有环境后，像 saga.sh 一样独占 `remote-acceptance.lock`，设置相同场景变量及 `ROOST_SAGA_PERF_OUTPUT`，只运行 `-test.run '^TestRealSagaPerformance$'`。同进程一次测试，hook在连接创建前安装，连接存活期间不变。插桩会增加一次JSON解码及记录锁开销，适合定位，不用来报极限吞吐。

使用 `python3 scripts/perf/saga-timeline.py <forward.json> --output <summary.json>` 重建完整事务。验证了拆分前后总耗时不变、两段之和等于原交付段，以及缺失任一解析事件时报错。普通夹具和生成后的诊断夹具 `TestSagaTimelineRecorder` 均通过。原始诊断日志、时间线、源码哈希保留于 `artifacts/perf/saga/wire-20261023-batch3`。一次外层zsh收尾使用保留变量status报错，但实际测试日志为PASS、结果和时间线完整、后续检查发现空锁遗留，确认原进程退出后已清理；不将外层命令记为通过。

下一步若继续定位消息部分，应使用相同三副本、持久策略、包大小的独立JetStream发布/接收基线，分别测低速延迟与饱和吞吐，并与同机Mongo混合负载对照。当前证据不支持替换JetStream或降低持久保障。
