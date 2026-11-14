# Gate 输入丢拍的计数口径（E-3，2026-11-14）

`scripts/perf/gate-mixed.sh` 是一个真实账号、一个 Player 与共享 World 热点的诊断。客户端和服务端在同一 Go 测试进程，使用真实账号 / TCP Gate / NATS / WAL / Mongo；不是多玩家容量测试。

## 计数含义

客户端按目标间隔计算下一次发送时间，但每次 `send()` 都等待上一笔 PB 应答和对应 EntitySync 解码完成。醒来时超过一个间隔，就跳过已经错过的输入槽位，不追发。这是 `Dropped` 的唯一增加位置。

- `drop_reasons.client_pacing`：未发送的输入槽位，等于 `Dropped`。它不是 Gate 或网络丢包。
- `signals.request_rejected`：收到业务拒绝；不能全部解释为队列满。
- `signals.nest_admission_rejected`：负载开始后 Nest 三个通道的容量拒绝增量。
- `signals.request_deadline` / `request_other_error`：Robot 已发请求的超时与其他错误。
- `signals.heartbeat`：正式 Driver 的独立计数，包括上一拍在途、队列满、拒绝、重投与各种丢弃原因。停止阶段也保留原始字段，不把正常停止丢弃算成业务丢包。

这些是不同层的观测，可能相关，不能相加。**客户端落后仍可能由慢服务端应答或 Sync 引起**；标明计数来源不等于排除服务端瓶颈，需要同时读 PB / Sync 延迟、Nest 等待与投影诊断。当前夹具是一条请求在途的有目标频率循环，并不保证维持指定的实际输入速率。

最终额外提交一笔双实体事务作持久终值核对，因此 `final_verification_requests=1`，必须满足：

```text
Scheduled = Completed - final_verification_requests + Dropped
```

成功报告、排空前报告都保留原因；负载开始后失败还会留下 `.failure.json`，其中 `verified=false`，不能拿失败阶段的数据当作验收通过。

## 已跑功能验证

并行编译期间用独立 offset 18000 的完整 mirror 环境，`GOMAXPROCS=0`，1000/s 目标、3 秒；故意用高于单在途请求可维持的目标来制造漏发槽位。以下只验证计数与数据一致性，不作为容量或时延验收：

| 运行 | Scheduled | Completed（含核对） | 客户端未发 | Nest 拒绝 / 请求拒绝 / 超时 | Heartbeat 投递 / 执行 |
| --- | ---: | ---: | ---: | --- | --- |
| 首轮 | 2998 | 294 | 2705 | 0 / 0 / 0 | 30 / 30 |
| 还原轮 | 2993 | 317 | 2677 | 0 / 0 / 0 | 30 / 30 |

两轮实际脚本退出 0，Sync Full / Delta / Remove、重连、最终 Mongo / WAL 数据核对通过。变异删除漏发计数的累加后，实际脚本失败于 `pacing counters do not balance: scheduled=2998 completed=317 dropped=0`；源码恢复后通过。失败 JSON 正确保留。

原始日志和 JSON 在工作树 `artifacts/perf/gate/{pacing-functional,pacing-count-mutant,pacing-restored}/`；入口日志 `/tmp/roost-v147-gate-pacing-{functional,mutation,restored}.log`。私有环境已停止，数据目录保留。

## 待执行的 E-3 正式复跑

没有其他编译、race、索引任务时，记录 load 与 swap，在私有依赖上连跑五轮：

```sh
export GOWORK=off GOFLAGS=-p=2
for round in 1 2 3 4 5; do
  ROOST_PERF_CPU=0 ROOST_PERF_LABEL="gate-drop-$round" \
  ROOST_GATE_LOAD_RATE=100 ROOST_GATE_LOAD_SECONDS=30 \
    bash scripts/perf/gate-mixed.sh
done
```

完成后按原因与独立指标回写 KNOWN-LIMITS；当前只完成计数实现与功能验证，未把历史 19 / 15 个漏发样本的运行原因写成已确认。
