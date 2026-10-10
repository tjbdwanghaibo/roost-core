# 性能验证与复跑

性能报告按版本、场景和参数保留；通过仅对该次实际负载成立。业务功能验收归 [release](../release/v1.25.0-VALIDATION.md)，未关闭问题归 [已知限制](../maintenance/KNOWN-LIMITS.md)。

| 报告 | 范围与结论 |
| --- | --- |
| [v1.24.0 稳定基线](STABLE-v1.24.0.md) | Nest、Sync、DataEngine、Remote/Saga 的历史验收；不代替新增 Gate 验收 |
| [Gate / AOI 混合负载](GATE-AOI.md) | 1000玩家、10000实体、10000消息/s；2026-10-09常规通过、4并行度重档失败；2026-10-10快8/慢8/默认并行度重档15分钟通过 |
| [Nest 调度对照](NEST-DISPATCH-BENCHMARK.md) | ID hash / ID tail、10～500μs handler 与 job 布局；微基准不能直接换算真实服务器容量 |

## 证据存放

- [benchmarks](benchmarks/)：随仓的精简原始样本与结果摘要，保留环境、参数及失败边界。
- 根目录 `artifacts/perf/`：本地完整日志、profile、二进制、源码差异和高频采样，由 `.gitignore` 排除；全新克隆不含这些文件。
- 历史报告中的 `/private/tmp/` 路径是当时的本地证据位置，不承诺在其他机器可访问。

不删除失败来留下全绿结果，不把中断、仅编译、沙箱拒绝连接计为性能通过。不同参数、硬件或夹具版本不得直接归因比较。Sync按P99≤50ms验收，保留max与超过50ms数量；on_change指一个Nest handler完成后统一处理变化，20Hz为兜底。

## 复跑入口

混合负载使用 [nest-game.sh](../../scripts/perf/nest-game.sh)。最新重档参数如下；私有NATS连接、独立标签及其他准备步骤见[完整报告](GATE-AOI.md)。不要指向共享生产资源。

```sh
ROOST_PERF_CPU=0 ROOST_PERF_COUNT=1 \
ROOST_NEST_GAME_WORKERS=8 ROOST_NEST_GAME_SLOW_WORKERS=8 \
ROOST_NEST_GAME_QUEUE=65536 ROOST_NEST_GAME_PLAYERS=1000 \
ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 \
ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_HZ=10 ROOST_NEST_GAME_DIRTY=5 \
ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_PROFILE=0 \
bash scripts/perf/nest-game.sh
```

运行前另设唯一 `ROOST_PERF_LABEL` 和私有 `ROOST_DATAENGINE_IT_NATS_URL`。`ROOST_PERF_CPU=0`保留Go自动选择，不代表运行时只有零个逻辑处理器；记录实际GOMAXPROCS。race、profile与索引刷新不要并跑正式延迟测量。其他模块命令及微基准见各报告，本文档整理未重新执行压测。
