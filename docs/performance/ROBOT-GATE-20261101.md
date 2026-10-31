# Robot / Gate 当前验证

基线：v1.35.0（83d7a3b1）加本轮 Robot 与夹具改动（结果随本次提交保存），Apple M5（10 核）、macOS arm64、Go 1.27.3。仅本机短测；没有 Linux、跨机或设备容量结论。真实依赖使用任务私有 Mongo 8.0 副本集、Redis 与三节点 NATS，不使用云库或已有业务库。缺省 GOMAXPROCS=0。

## 真实账号与持久热点

`scripts/perf/gate-mixed.sh`：正式 account/Redis/NATS → TCP Gate → PB/Nest → strict WAL/Mongo → PB EntitySync；一个 Player 和共享 World，8 个 short worker、65536 等待容量，IO/long 沿用默认（32/8）。10Hz heartbeat，每 20 拍修改 World 一次。普通 AddExp 同时修改 Player/World，包含 WAL fsync；Mongo 后台投影不在普通请求回执等待内。on_change 汇总 handler 变化，此夹具 Interval=1h，专门证明主动唤醒，不用于推荐生产兜底配置。

| 档位 | 测量区间 | 普通请求完成 | 丢拍 | 测量结束未投影 | 结论 |
| --- | --- | --- | --- | --- | --- |
| 100 请求/s | 30s | 2981 | 19 | 1906 | 排空再等 30s 仍剩 314，失败 |
| 20 请求/s | 30.0004s | 600 | 0 | 0 | 一致性及排空通过 |

100/s 失败位于 Mongo 投影排空：测量结束 committed=2996、projected=1090、无冲突/投影错误；排空超时后 projected=2684、WALUnacked=314，记录一次 context deadline。它说明本机此共享实体持久热点跟不上持续输入，不证明框架独立实体容量只有 20/s，也不是 Gate/Sync 超时。没有改 Mongo 持久级别、放宽排空期限或删掉失败样本；20/s 是显式另一档，不替代原档结论。

20/s 全程包含 300 次心跳、15 次心跳修改，最后增加 1 次双实体排空事务（请求总数 601，TPS 只计测量的 600 次）。RPC 成功 601、失败/超时/拒绝 0，最终 WAL=0、投影冲突=0，重复投影和 DAO 冷水合一致。

| 客户端指标 | 样本数 | P99 | 最大值 |
| --- | --- | --- | --- |
| PB 请求回复 | 601 | 12.01ms | 14.51ms |
| Full 应用 | 2 | 2.77ms | 2.77ms |
| Delta 应用 | 601 | 13.09ms | 14.76ms |
| Remove 应用 | 1 | 0.52ms | 0.52ms |

Sync 从动作发起到成功解码应用，业务值核对通过才结算样本；同进程单调时钟，分位数为约 9% 桶宽估算。Full/Remove 样本很少，不能声称冷启动容量。无超 50ms 样本。TCP 读 101780 字节、写 11068 字节（含握手、回复、同步与重连）；客户端和服务端合进程 CPU 3.63 秒 / 30.07 秒（单核 12.08%），RSS 生命期高水位 105185280 字节，采样 goroutine 峰值 524。依赖进程资源不包含在内，不能用这些数排除 Mongo 磁盘瓶颈。

原始结果在 `artifacts/perf/gate/gate-mixed-20rps-20261101`；100/s 失败在 `gate-mixed-30s-20261101`、`gate-mixed-diagnostic-20261101`、`gate-mixed-request-diagnostic-20261101`、`gate-mixed-drain-diagnostic-20261101`。前几轮补充定位日志，最终轮明确在 drain；这些不是独立随机性能样本。新的入口会在排空前保存 `result.json.load-phase.json`（verified=false），即使后续失败也保留已测阶段。

复跑：由调用方按隔离环境说明设置 `ROOST_DATAENGINE_IT_*`，不把凭据写进报告。

```bash
ROOST_PERF_CPU=0 ROOST_GATE_LOAD_SECONDS=30 ROOST_GATE_LOAD_RATE=20 \
  ROOST_PERF_LABEL=gate-stable-new bash scripts/perf/gate-mixed.sh
# 明确重测较高输入；失败也必须保留。
ROOST_PERF_CPU=0 ROOST_GATE_LOAD_SECONDS=30 ROOST_GATE_LOAD_RATE=100 \
  ROOST_PERF_LABEL=gate-hotspot-new bash scripts/perf/gate-mixed.sh
```

## 1000 玩家 / 10000 Entity

两档均 30 秒，正式生成 Unit/DAO，1000 玩家每人 10 条消息/s，Gate/NATS 接入，10000 Entity 挂正式 heartbeat，AOI 每人最多 50 个（含自己），on_change + 50ms 周期兜底，short=8、io=8、等待容量=65536、GOMAXPROCS=0。此夹具是内存业务/事务提交，不能套用到上面的 strict WAL/Mongo 热点；普通消息与心跳分别校验计数和最终 DAO。

| 档位 | 普通消息/s | 心跳回调/s | Entity 修改/s | 消息 P99 | 服务端消息 P99 | 提交→客户端 P99 / max | Sync 超 50ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1Hz / 1% 变化 | 9998.8 | 9995.5 | 199.9 | 12.13ms | 3.70ms | 6.27 / 14.86ms | 0/146550 |
| 10Hz / 5% 变化 | 9998.3 | 99949.8 | 5497.3 | 11.70ms | 3.40ms | 8.20 / 29.44ms | 0/1353586 |

两档普通请求各 300000 次、零拒绝；最终计数/DAO/Sync 值与延迟门禁均通过。heartbeat 批量模式不逐拍测完整调度队列，不能用其回调 Completion P99=0.01ms 宣称计划拍到客户端只需 0.01ms；上表单列 Sync 提交到应用。真实可见数分别 26～50 / 28～50。输入发生器和客户端与服务器同机，非跨机网络。

复跑入口 `scripts/perf/nest-game.sh`，设置 `ROOST_PERF_CPU=0 ROOST_PERF_COUNT=1 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 ROOST_NEST_GAME_DURATION=30s`，再分别设 HZ=1/DIRTY=1 和 HZ=10/DIRTY=5（变量前缀同为 ROOST_NEST_GAME_）。隔离 NATS 由调用方提供，原始证据在 `artifacts/perf/nest/robot-mmo-{1,10}hz-20261101`。

## 重档与少量 Saga 同机混合

在上述 10Hz/5% 档上延长为 60 秒，同时运行正式 `TestRealSagaJSScanLevel`：native-forward、并发 1、预热 5 秒、16 分区、参与方 8 个 short worker，默认 pipelined 异步完成。GOMAXPROCS 环境为 0，运行时采用 10；不把 pipelined 写成 strict。两个测试进程共享宿主与本任务的 NATS 集群，Saga 另使用同一隔离 Mongo。这是资源干扰验证，不是同一个生成业务服务里调用 Saga 的端到端测试；无 Remote 分支。

- MMO：普通消息 9999.4/s；Sync 提交→应用 P99 10.37ms，最大 52.83ms，3/2649391 个样本超过 50ms。计数、数据和 P99 门禁通过；保留长尾，不因通过而删除样本。
- Saga：共 1611 笔，稳态 26.89/s，相当于普通消息约 0.27%；P50 36.79ms、P99 65.15ms。异常终态、未知发布、未解决事务均 0；Nest/Mongo 余额核对通过、积压峰值 3、负载结束及排空后均 0。Saga 延迟不套用客户端状态 Sync 的 50ms 指标。

原始 JSON/日志在 `artifacts/perf/mixed/robot-mmo-saga-20261101`。复跑时先完成编译再并行加压，不混入 race/编译/索引：`go test -tags integration -c -o <新目录>/saga.test ./framework/saga`；设置 `ROOST_NATS_CLUSTER_URL` 为任务私有三节点地址、`ROOST_SAGA_SCAN_OUTPUT` 为新 JSON 路径，`ROOST_SAGA_SCAN_SCENARIO=native-forward ROOST_SAGA_SCAN_CONCURRENCY=1 ROOST_SAGA_SCAN_SECONDS=60 ROOST_SAGA_SCAN_WARMUP=5 ROOST_SAGA_SCAN_NEST_WORKERS=8`，执行二进制的 `-test.run '^TestRealSagaJSScanLevel$'`。同时按上一节重档配置运行已编译的 nest-game.test，DURATION=60s、OUTPUT 为新 JSON；两边退出码与数据断言都必须检查。

## 验证边界

这里只验证一个真实账号的热点，并不是 1000 人场景；账号上游使用测试 HMAC，不代表第三方平台登录验收。WAL 重开和对象冷水合不代表进程 SIGKILL 恢复；持久热点没有混入 Remote/Saga；独立混合档另见上节，仍没有 Remote 或原生 C++ 解码容量结论。目标拓扑、多服资产守恒、完整服务恢复及移动引擎仍独立验收。
