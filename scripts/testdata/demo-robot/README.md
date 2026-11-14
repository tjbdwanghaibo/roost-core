# game-demo Robot 单档入口

`scripts/perf/demo-robot.sh` 用当前 CLI 生成完整 game-demo，复用 `demo-drill/lib.sh` 启动 11 个服务，经正式 `bin/loadtest` 走总线登录、TCP、完整 `demo` 场景。它不修改 demo 的生产逻辑或场景。单 IP 连接上限设为 512；`GOMAXPROCS=0`，`GOWORK=off`，编译 `GOFLAGS=-p=2 -mod=mod`。

每次运行必须给一个**尚不存在的绝对输出目录**。本次私有 mirror HOME 和工程均在其中创建，拒绝复用上一档。清理只触及本次工程前缀与本次私有 mirror；不读共享环境、不重置共享服务。端口占用或各组端口重叠立即失败，其他任务并行时需协调偏移。

```sh
# 默认是 2 人功能冒烟，不是容量测量。
DEMO_ROBOT_OUT=/private/tmp/demo-robot-smoke-new bash scripts/perf/demo-robot.sh

# 在获准的空闲测量窗口逐档运行；下面一次只选一档。
DEMO_ROBOT_COUNT=100 DEMO_ROBOT_MODE=burst \
  DEMO_ROBOT_OUT=/private/tmp/demo-robot-100-burst-new bash scripts/perf/demo-robot.sh
```

六档是 count `100` / `200` × mode `burst` / `ramp20` / `ramp10`。`burst` 一批启动全部；后两档严格使用交接口径 `-ramp-step 20|10 -ramp-interval 1s`，第一批立即启动。不是每个请求的固定速率；`count` 是每人执行一次完整场景的人数，不能写作消息 TPS。命令固定 `-executor pool -duration 0 -seed 1 -scenario demo -login bus`，保留正式 CLI 的错误率 0、drop 0、整场景 p95 16s 门槛；失败仍保留报告并返回非零，不放宽门槛换绿。

可选端口参数：`DEMO_ROBOT_MIRROR_OFFSET`（默认 37000，最大 38135）、`DEMO_ROBOT_PORT_SHIFT`（默认 31000）、`DEMO_ROBOT_ETCD_PORT`（默认 35479）。每档全新目录；同一组端口仅能在前档清理完成后复用。依赖工具与 `mirror-local.sh` 相同，另需 etcd、jq、curl、Python 3。正式测量不要与编译、race、索引或其他负载并行；生成和编译完成后才采集 `host-before.txt` 并启动 Robot。

输出包含：

- `robot.json`、`robot.log`、`loadtest-args.json`：原始正式 runner 快照和进程输出，分消息请求结果也在 JSON 内；`test.log` 明确 `TestDemoRobotCapacity` 恰好 1 个 PASS、零 SKIP。
- `summary.json`、`diagnostic-matches.json`：来自正式 JSON 的成功/失败/取消人数、run 级最终错误及其分类、整场景分位数、原门槛结果，以及带源文件/行号的 Saga still/unknown/waiting 与 `nest: sync timeout` 日志匹配。正式 RunSnapshot 没有逐 Robot 错误明细；不能由日志出现次数补造事务失败数。
- `logs/`、`dependency-logs/`、`etcd.log`、`game-metrics-{before,after}.txt`：服务日志、私有依赖日志与游戏指标；`environment.txt`、`host-{before,after}.txt`：源码版本/工作树、Go、实际 CPU/内存/swap/load；`project-source.tgz`：本轮生成源码与配置。
- `mirror-up.log`、`generate.log`、`dev-run.log`、`services.txt`、`cleanup.log`：环境与启动/收尾证据。

日志计数有明确边界：场景 Retry 会吞掉中间失败，日志里的 `gift/alliance saga still` 主要是最终失败上下文，不能当成所有轮询次数、独立玩家数或剩余积压。WorldStats 与 RecordEnter 分别列明；AddExp 同时锁 Player/World，其超时不能单独归因于 World 锁。其余 Nest timeout 保留原日志，只有核对调用上下文后才能归为 World 热点。统计覆盖保留的启动、负载和停止日志，零匹配不证明整个运行没有等待。启动分区的 `sagas/messages/pending_inputs/last_event` 原值另列 `startup_saga_replay`，`pending_inputs:0` 不算等待。原报告缺失时 `state=not_reported`，不会伪造成零失败。

负载子进程 5 分钟超时，Go 夹具 6 分钟；只结束本次子进程。超时或门槛失败照常保存日志、停止应用并清理私有依赖。入口不进入 pretag 默认负载；已登记在根包真实依赖入口覆盖检查。
