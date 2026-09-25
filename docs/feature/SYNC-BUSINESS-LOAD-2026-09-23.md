# Sync 业务链路负载验证（2026-09-23）

后续用户已明确目标为 1000 玩家、10000 个全局实体、Interest/AOI 下每人约 50 个可见实体、50ms 延迟。对应的双进程模型与实测见 [1000/10000 AOI 验证](SYNC-AOI-1000-10000-2026-09-23.md)。本文保留为早期 demo 探索，集中场景结果不代表该目标容量。

本轮以当前工作树生成的 game-demo 为业务样本。尚未获得生产目标参数，因此人数、活跃比例与站位是探索档位，不代表线上流量或容量承诺。

## 实际执行的链路

生成的 Player / World → `AddExp` Nest 多实体事务 → 生成 DAO 的 dirty 发布 → Player BSON 增量 packer → Scene 的 AOI Interest → Manager.Flush → 每玩家独立的本机 TCP 连接 → Sync 帧、版本链与 BSON 字段校验。

采用 demo 的 50ms 同步周期、1000×1000 地图、120/150 进入/离开半径、默认对象容量。每个脏玩家每 tick 加 1 点经验，并更新其 AOI 坐标；空间索引变化用于施加 AOI 负载，**不是调用完整移动端点，也不改变 Player 的持久化位置字段**。玩家持续在线，每个采样先发首帧并预热 10 轮，再进入测量阶段。

明确的替身与边界：

- 使用 demo 现有测试的提交器，提交立即成功；未测 Mongo/WAL/JetStream，也未启用 durable watermark 等待。
- TCP 外层是测试专用的长度、计划时刻和 tick 信封；内部 Sync 字节不变。未测生产 player-TCP 包装、认证或登录端点。
- 由测试按绝对时间调用 Flush，未使用 Manager.Start 的后台定时器；业务操作在本轮 Flush 前顺序完成，未模拟其他请求同时争抢锁。
- 接收端检查会话帧时钟、主体版本基线以及 exp/level BSON 值；不构建完整游戏客户端，不模拟弱网、慢消费者、断线重连或数据库故障。
- 服务端、Nest、接收端和断言运行在同一进程；进程分配量包含客户端解码和测量工具本身，不能解释为服务端单独的分配量或 RSS。

## 档位

| 档位 | 玩家 | 每 tick 改变玩家 | 站位 |
| --- | ---: | ---: | --- |
| solo | 1 | 1 | 单人 |
| hotspot32_sparse | 32 | 4（10% 向上取整） | 集中，相互可见 |
| hotspot96_sparse | 96 | 10（10% 向上取整） | 集中，相互可见 |
| hotspot96_busy | 96 | 96 | 集中，相互可见 |
| spread128_busy | 128 | 128 | 分散，局部 AOI 可见 |

首轮 128 人集中站位试跑触发 `frame: object limit exceeded`，会话被 Manager 关闭。源码契约明确 `Limits.MaxObjects` 同时约束单会话持有的主体数，默认是 100；这属于超出 demo 的容量配置，不能称为 CPU 瓶颈。集中档位因此选 96，保留 128 人分散档位。没有悄悄抬高生产默认值或客户端解码限制。

## 可复跑命令

在 roost-core 根目录：

```bash
# 默认 5 档，每档 200 tick（约 10 秒），3 次独立进程采样，GOMAXPROCS=4。
./scripts/perf/sync-business.sh

# 先做短烟测。
ROOST_PERF_LABEL=business-smoke ROOST_PERF_COUNT=1 ROOST_SYNC_TICKS=4 ./scripts/perf/sync-business.sh

# 延长单档采样到约 60 秒。
ROOST_PERF_LABEL=business-long ROOST_SYNC_CASE=hotspot96_busy \
  ROOST_SYNC_TICKS=1200 ROOST_PERF_COUNT=5 ./scripts/perf/sync-business.sh

# 将业务目标参数代入；默认容量仍然生效。
ROOST_PERF_LABEL=business-custom ROOST_SYNC_CASE=custom \
  ROOST_SYNC_PLAYERS=64 ROOST_SYNC_DIRTY_PERCENT=25 ROOST_SYNC_LAYOUT=hotspot \
  ROOST_SYNC_HZ=20 ROOST_SYNC_TICKS=600 ./scripts/perf/sync-business.sh
```

脚本从当前源码编译生成器，在系统临时目录生成隔离 demo，以 `replace` 指向本工作树，再编译专用测试二进制后开始采样。需要本机 TCP、Go 缓存和已缓存或可下载的依赖，不需要 Docker。结果位于 `artifacts/perf/sync/<label>/`：环境和 git 状态、生成/依赖日志、每轮日志、每个档位 JSON。输出目录已存在时拒绝覆盖。临时 demo 和测试二进制保留，其路径写入 `workspace.txt`，便于复现和 pprof。

`ROOST_PERF_CPU` 控制 GOMAXPROCS；`ROOST_PERF_COUNT` 控制独立采样次数。`custom` 人数限制 1～1024、脏比例 1～100，站位为 hotspot 或 spread；频率改变的是测试调用周期，不改 demo 默认配置。任何版本/字段错误、会话丢失、收发不齐或 tick 超期都会使命令非零退出，JSON 保留已有观测。

## 指标解释

- `tick_work_ms`：业务事务、AOI 更新、期望值记录与 Flush 的总用时；`flush_ms` 为其中 Flush 部分。二者都受同机客户端抢占影响。
- `scheduled_to_client_ms`：从该 tick 的**计划执行时刻**到一个完整帧被客户端解码和字段校验完成。采样单位是帧，不是玩家请求；不含 tick 之前的输入等待。
- 使用绝对计划时刻，积压 tick 不丢弃；因此过载会表现为延迟增长。`deadline_misses` 是服务端本 tick 的工作结束超过下一计划时刻的次数。
- `sync_bytes` 只计 Sync 帧字节，不含测试信封、TCP/IP 和生产业务协议开销。`subscriptions_final` 给出最后的实际扇出规模。
- `start_lag_ms` 与 `overdue_ticks` 记录计划迟到及每个超期 tick 的工作/Flush 耗时（后续长采样新增）。`process_gc_pause_ms` 是整个测量期的累计 STW 暂停，不包含所有 GC assist 或调度影响。
- `process_alloc_bytes` / `process_gc_cycles` 是测量区间整个测试进程的累计分配和 GC 次数。预热不计入；末尾排空接收端计入。
- 分位数由原始样本排序计算。短测的 p99 易受调度抖动影响，不能将三个采样的中位数宣称为统计显著结论。

## 验证与实测记录

环境：Apple M5、darwin/arm64、Go 1.27.0、GOMAXPROCS=4。框架为 `967bc69` 加本轮 Sync 工作树，未改生产代码。各档预热 10 轮，正式 200 tick、20Hz，三次独立测试进程采样。

下表 p95 为“三轮各自 p95 的中位数”；最差 p99 为三轮中最大的一轮 p99，不能理解为合并原始样本后的总体分位数。超期为三轮合计 / 600 tick。

| 档位 | tick p95 ms | Flush p95 ms | 客户端 p95 ms | 最差一轮客户端 p99 ms | 超期 tick |
| --- | ---: | ---: | ---: | ---: | ---: |
| solo | 0.369 | 0.126 | 2.402 | 2.624 | 0 / 600 |
| hotspot32_sparse | 2.228 | 1.480 | 3.652 | 4.677 | 0 / 600 |
| hotspot96_sparse | 5.557 | 3.675 | 6.664 | 387.620 | 11 / 600 |
| hotspot96_busy | 13.659 | 7.404 | 16.514 | 183.221 | 12 / 600 |
| spread128_busy | 10.810 | 3.369 | 12.202 | 17.686 | 0 / 600 |

三轮数据解码、字段值和版本链校验通过，未丢会话，但整套命令按“零超期”门禁返回失败。不能只凭 p95 低于 50ms 宣称稳定 20Hz。96 人集中场景分别有 9216 个订阅，128 人分散场景最后约 1026 个订阅；可见范围比在线人数更能解释同步工作量。

96 人全活跃每 tick Sync 字节中位数约 1.174 MB，按目标 20Hz 约 23.5 MB/s，尚不含外层业务协议和 TCP/IP；测试进程每 tick 累计分配约 15.1 MB，包含真实事务、客户端 BSON 解码及断言，不是服务端独立分配量。

原始结果保留在 `artifacts/perf/sync/business-20260923/`。旧微基准的“耗时降低 10%”只比较共享编码，不能直接移植到本业务链路；本轮建立当前实现的业务基线，没有业务链路修改前后的 A/B 证据。

工具验证：五档短场景在 race 下以 2Hz 通过（避免把 race 开销当性能门禁）；最初 20Hz race 运行因耗时超预算失败，没有 race 报告，其时延不用于性能结论。

### 集中场景长采样

在相同 20Hz / GOMAXPROCS=4 下，仅对 `hotspot96_busy` 再做三轮各 600 tick（约 30 秒），新增开始迟到与超期明细。保留全部失败样本，不通过反复运行挑选绿色结果。

| 轮次 | tick p95 ms | 客户端 p95 ms | 客户端 p99 ms | 最大工作耗时 ms | 超期 / 600 | 累计 GC STW ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 14.367 | 15.486 | 21.541 | 33.639 | 1 | 68.965 |
| 2 | 18.773 | 27.198 | 147.439 | 311.103 | 20 | 76.569 |
| 3 | 19.671 | 27.973 | 156.918 | 213.437 | 19 | 90.524 |

三轮合计 **40 / 1800（约 2.22%）tick 超期**，严格门禁失败。第一轮最大工作耗时小于 50ms 仍有超期，因为开始迟到最高约 47ms；第二、三轮同时存在工作耗时尖峰和积压。累计 STW 数字不能解释单次尖峰的根因，不据此认定“GC 导致全部抖动”。原始 JSON 的 `overdue_ticks` 保留逐 tick 证据，位于 `artifacts/perf/sync/business-long-20260923/`。

### 独立 profile 与下一步

另对同档位采集 200 tick 的 CPU / alloc_space profile，放在 `artifacts/perf/sync/business-profile-20260923/`；该轮未超期，但含 profile 开销，仅用于定位，不纳入上面的性能统计。

- 客户端 `businessTCP.consume` 的累计分配占约 **56.6%**，包含 Sync 解码、BSON 反序列化、数据拷贝与校验。Manager.Flush 累计占约 27.5%；二者是不同调用树，不能把整个进程分配称作服务端分配。
- CPU 样本主要落在系统调用与运行时唤醒/调度，单凭 top 表不能锁定偶发尖峰来源。
- 因此下一轮先将客户端与服务端分进程/分机，服务端单独采 CPU、RSS、GC 和 tick histogram，再分析 Flush/AOI 分配；目前不据此修改生产逻辑。

可对保留的测试二进制再次采样：

```bash
perf_workspace=$(cat artifacts/perf/sync/business-long-20260923/workspace.txt)
perf_report_dir="$PWD/artifacts/perf/sync/business-profile-local"
mkdir -p "$perf_report_dir"
cd "$perf_workspace/project"
GOMAXPROCS=4 ROOST_SYNC_BUSINESS=1 ROOST_SYNC_CASE=hotspot96_busy \
  ROOST_SYNC_TICKS=200 ROOST_SYNC_REPORT_DIR="$perf_report_dir" \
  ../business.test -test.run '^TestSyncBusinessLoad$' -test.timeout=2m \
  -test.cpuprofile="$perf_report_dir/cpu.out" -test.memprofile="$perf_report_dir/mem.out"
# 若性能门禁失败，仍检查已写出的报告和 profile；不要隐藏退出状态。
go tool pprof -top ../business.test "$perf_report_dir/cpu.out"
go tool pprof -top -alloc_space ../business.test "$perf_report_dir/mem.out"
```

自定义 32 人、25% dirty 的短场景通过；自定义 128 人集中场景如预期以容量错误失败。生成包 `go vet`、脚本 `bash -n`、fixture 的 gofmt、`git diff --check` 及相关文档链接检查通过；本轮没有修改 Sync 生产代码。

## 后续真实部署验证所需输入

需要明确每场景玩家/怪物数量、平均与峰值可见实体数、同步频率、dirty 比例及字段大小、上下线速率、目标机器和服务部署、慢客户端比例，以及允许的 p95/p99 与 tick 超期比例。具备这些参数后，选择生产接入协议与真实存储链路进行分机压测。
