# Heartbeat 时间片交叉复测

基线 `435f431e`，两份二进制唯一差异为 `framework/nest/try_batch.go` 的 `tryBatchSlice`（200 / 100µs）；本轮未修改默认值。macOS / Apple M5 / Go 1.27.3。

50,000 实体，全部活跃，8 个业务 worker，batch=64，普通业务 5,000/s。`GOMAXPROCS=0`，程序实际读数为 10；每档预热 3s、测量 15s、三轮交替顺序。模式 c，心跳周期 100 / 200ms。独立运行，没有并跑本轮其他负载；桌面宿主机并非独占。

分别编译两份二进制 `GOWORK=off go test -c -o /tmp/hb.test ./gameplay/heartbeat/internal/bench`，各档运行：

```sh
GOMAXPROCS=0 HB_DELIVERY=1 HB_MODE=c HB_ACTIVE=1 HB_PERIOD=200ms HB_WORKERS=8 HB_WARM_S=3 HB_MEASURE_S=15 /tmp/hb.test -test.run='^TestDeliveryRun$' -test.count=1 -test.timeout=2m -test.v
```

第 1、3 轮按 `(200µs,100ms),(100µs,100ms),(200µs,200ms),(100µs,200ms)`；第 2 轮反序。完整输出为同目录 log，聚合为 samples.jsonl。该 fixture 使用自己的 heartbeat 驱动，回调为内存操作，经过正式 Nest，但不能替代真实游戏的 WAL / 网络 / heartbeat 组件端到端验收。

| 心跳周期 | 200µs 业务 P99（三轮，ms） | 100µs 业务 P99（三轮，ms） |
|---|---|---|
| 100ms | 0.236 / 0.247 / 0.238 | 0.125 / 0.205 / 0.109 |
| 200ms | 0.137 / 0.200 / 0.266 | 0.238 / 0.118 / 0.217 |

所有档业务失败、心跳跳过为 0。100ms 档方向一致，200ms 档不一致；仅三轮短跑，不作显著性或长稳结论，也不能把历史 0.31ms 的原因归于时间片。
