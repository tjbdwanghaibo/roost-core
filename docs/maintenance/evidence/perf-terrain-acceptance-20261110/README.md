# 可编辑地形与同步改动这一大轮的本机性能验收（2026-11-10）

基线：main `bc883581`（只加本证据与文档，没有改产品代码）。方案见[地形方案 §28 / §29](../../FEATURE-2026-11-08-editable-terrain.md#29-游戏尺度参数视野-32m单进程-2048m同步-128-个2026-11-10)，进度见 [TERRAIN-STATUS](../../TERRAIN-STATUS.md)。

时间预算：维护者在开跑后把预算改为“整体约 30 分钟、每轮压测约 10 分钟”（原为每轮 ≤ 30 分钟）；第 2 轮按新预算只做无缝演练最后一步的定位，bisect 与开地形演练基线没有做。

## 宿主状态

Apple M5（10 核），macOS，Go 1.27.3，`GOWORK=off`。CorpLink 常驻（系统扩展合计约 2%～36% CPU，随时间波动）。fseventsd 在维护者重启后没有持续高占用，四次采样都 ≤ 0.6% CPU，**没有等待**。swap 已用约 6.6～7.0GB / 8GB，全程偏高。

| 时刻 | swap 已用 | loadavg（1/5/15 分钟） | fseventsd CPU | CorpLink 扩展合计 |
| --- | ---: | --- | ---: | ---: |
| 开工 | 6985M | 4.28 / 4.59 / 7.29 | 0.1% | 约 31% |
| 第 1 轮开始 | 6777M | 6.65 / 5.56 / 7.39 | 0.0% | 36% |
| 第 1 轮结束 | 6705M | 2.64 / 4.36 / 6.47 | 0.0% | 27%（脚本段结束时） |
| 第 2 轮开始 | 6705M | 1.31 / 3.59 / 6.00 | 0.1% | 8% |
| 第 2 轮演练后 | — | 3.41 / 3.37 / 5.36 | 0.3% | 2% |
| 第 2 轮清理后 | 6657M | 1.48 / 2.69 / 4.80 | 0.4% | — |

下面的延迟类数字都在这种状态下取得（后台负载、swap 偏高），只作同机参考。

## 墙钟

| 轮 | 开跑前估时 | 实际墙钟 | 内容 |
| --- | --- | --- | --- |
| 第 1 轮（CPU / 字节，无外部服务） | 约 8 分钟 | 约 3 分 50 秒 | sync-business（失败，见下）、`sync.sh` 选 5 组基准、地形 / cellworld 基准、生成工程里的字节测量用例 |
| 第 2 轮（真实依赖） | 约 8～10 分钟 | 约 5 分 40 秒 | 私有 mirror-local 起停、不开地形的无缝演练（14 步 + 最后的 loadtest），保留进程栈取证后手工停 |

## 第 1 轮：CPU / 字节

### 命令

```bash
# 业务负载（生成 game-demo + 夹具 scripts/perf/fixtures/sync_business_test.go.tmpl）
ROOST_PERF_COUNT=1 ROOST_SYNC_CASE=custom ROOST_SYNC_PLAYERS=64 ROOST_SYNC_DIRTY_PERCENT=100 \
  ROOST_SYNC_LAYOUT=hotspot ROOST_SYNC_TICKS=200 scripts/perf/sync-business.sh
# entitysync / frame 微基准
ROOST_PERF_COUNT=1 ROOST_PERF_BENCHTIME=300ms \
  ROOST_PERF_BENCH='ManagerFlush|FrameCodec|ClusterFlushRandomWalk|SharedSpatialView|ProfileDemand' scripts/perf/sync.sh
# 地形与 cellworld
go test ./gameplay/terrain/ -run '^$' -bench . -benchmem -count=1
go test ./gameplay/cellworld/ -run '^$' -bench . -benchmem -count=1
# 生成工程（同一个 game-demo）里的字节测量
go test -count=1 -run '^(TestMainViewMeasureSyncBytes|TestMainViewMeasureMovingCrowdBytes)$' -v ./internal/service/game
ROOST_TERRAIN_MEASURE=1 go test -count=1 -run '^(TestTerrainMeasureUpdateBytes|TestTerrainMeasureDownlinkScenarios)$' -v ./internal/service/game
```

### sync-business 没有测出数字：夹具与当前 core 不兼容（已定位，未修）

`scripts/perf/sync-business.sh` 在当前 main 上直接失败，三处不兼容依次出现：

1. `panic: nest: handler "sync_perf_move" registered after engine start`：夹具在 `newSceneNest` 已 `Start` 的引擎上调 `(*NestMgr).MustRegisterHandlerWithMeta`，实例表只允许 Start 之前注册（`framework/nest/handler.go` `RegisterHandlerWithMeta`）。
2. 只在本次的临时工程里把它改成包级 `nest.MustRegisterHandlerWithMeta` 之后：`unsupported DAO sync codec 0`。增量信封 v3 不带 Codec（接收端沿用全量的），夹具经生成的 `clientsync.DecodeFields` 按每条更新的 `Codec() != 2` 拒绝。
3. 再在临时工程里放过 Codec 0 之后：`unsupported DAO sync format`。增量载荷是增量字节压缩之后的格式，`DecodeFields` 这条只认字段替换的动态解码路径解不了；夹具需要改成按 subject 记全量、用生成的 Apply 接增量，不是两行补丁能修的。

结论：sync-business 的“每 tick 开销 / 分配 / 业务负载下的下行字节”这一轮没有数字；下行字节改由已有的测量用例在同一个生成工程上取（下一节）。夹具要随帧协议 2 / 信封 v3 更新（[KNOWN-LIMITS](../../KNOWN-LIMITS.md) 已记）。两处本地补丁只在临时工程里，没有提交。

### 同步字节：与方案估算对照

帧 payload（含帧头，不含 16 字节线上消息头），KB = 1024 字节，5 拍 / 秒（移动心跳 200ms）。

| 项 | 估算（方案 / 前一轮证据） | 本次实测 | 结论 |
| --- | --- | --- | --- |
| 玩家走一步（增量） | 30B | 30B（三次都是 30） | 相符 |
| 怪巡逻一步 | 30B | 30B（与别的怪同帧时 90B / 3 个） | 相符 |
| 一名玩家进入视野（全量，near 视图） | 85B | 85B | 相符 |
| 三只怪同帧进入视野 | 235B | 235B | 相符 |
| 128 名玩家进入视野合计 | 9787B（四帧） | 2704 + 3010 + 3160 + 913 = 9787B | 相符 |
| 64 名玩家进入视野合计 | 4964B（两帧） | 1804 + 2635 + 535 = 4974B（三帧） | 多 10B，分帧不同（多一帧的帧头，推断，尚未确认） |
| 64 个同时动，每观察者 | 9.5KB/s | 中位数 1934B / 拍 = 30.2B / 对象，**9.4KB/s** | 相符 |
| 128 个同时动，每观察者 | 18.6KB/s | 中位数 3807B / 拍 = 29.7B / 对象，**18.6KB/s** | 相符，预算 50KB/s 的 37% |
| 128 个中 10% 在动（常态） | — | 未测（sync-business 失败）；按每对象 29.7B 线性推算约 1.9KB/s | 只是推算 |

地形下行（`ROOST_TERRAIN_MEASURE=1`，与 [terrain-quota 证据](../terrain-quota-20261108/README.md) 同一用例）：块全量 12476B（线上）、r=5m 抬地增量 606B、r=16m 平滑 5790B；热点 20 人每秒一笔 r=5m 时 16.5～27.7KB/s，r=16m 时 107.9～226.5KB/s，配额生效后约 42～44KB/s；6m/s 跑过全改过的块 8.1KB/s、12m/s 16.5KB/s。

### 微基准（`-count=1`，单样本，只作量级参考）

entitysync / frame（`-cpu 1`，benchtime 300ms）：

| 基准 | ns/op | B/op | allocs/op | 线上字节 / op |
| --- | ---: | ---: | ---: | ---: |
| ManagerFlush sessions=64 subjects=32 profiles=1 | 520674 | 688545 | 638 | 553266 |
| ManagerFlush sessions=256 subjects=32 profiles=1 | 2030804 | 2653668 | 1796 | 2208644 |
| ManagerFlushWithNamespaces sessions=64 | 803170 | 968842 | 756 | — |
| ClusterFlushRandomWalk | 219064 | 49506 | 1399 | — |
| ClusterFlushRandomWalkCapped | 11938896 | 3388737 | 13795 | — |
| SharedSpatialView players=1024 | 45346 | 425 | 6 | — |
| FrameCodec encode objects=256 payload=256 | 12504 | 77864 | 4 | — |
| FrameCodec decode objects=256 payload=256 | 20359 | 87336 | 516 | — |

地形（GOMAXPROCS 10）：

| 基准 | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| BrushApplyPlayerRadius r5m_linear_1chunk | 2724 | 2216 | 17 |
| BrushApplyPlayerRadius r16m_smooth_4chunks | 16064 | 16720 | 49 |
| EncodeChunk | 237943 | 1162931 | 21 |
| ReadChunk | 42435 | 75781 | 35 |
| Height / HeightParallel | 10.16 / 1.789 | 0 | 0 |
| WalkerStep | 10.14 | 0 | 0 |
| WalkerFindPath 50m / 200m / exhausted4096 | 72932 / 166051 / 1154359 | 120272 / 198400 / 1127268 | 15 / 18 / 41 |
| GridFindPath 50m / 200m / exhausted4096 | 267274 / 240884 / 1194723 | 434313 / 383512 / 1667165 | 1450 / 1437 / 4472 |
| cellworld IntegrateOnSurface none / walker（speed2000） | 10.79 / 425.0 | 0 | 0 |
| cellworld IntegrateOnSurface walker（speed200） | 60.70 | 0 | 0 |

## 第 2 轮：无缝演练最后一步的 p95（已定位）

### 命令

```bash
export ROOST_MIRROR_LOCAL_HOME=<scratch>/mirror ROOST_MIRROR_LOCAL_OFFSET=24000
scripts/mirror-local.sh up
set -a; source <scratch>/mirror/roost-dataengine-it/env.sh; set +a   # 只 source，不打印
ROOST_SEAMLESS_KEEP=1 ROOST_SEAMLESS_OUT=<scratch>/seamless-plain scripts/test-seamless-demo.sh
# 保留进程栈：读 JetStream 上 saga 流的终态记录（nats stream get），之后按 .dev/*.pid 停进程、删库、停 etcd
scripts/mirror-local.sh clean
```

### 结果

14 个演练步骤全部通过；最后的 `cmd/loadtest`（sid 1003，`-count 2`）复现失败：`p95 = 27.85s > max 16s (2 samples)`，两个机器人都成功（error_rate 0）。同一形状与之前两次留下的报告一致（29.03s、30.41s）。

**每个请求都很快**：报告里所有消息 p50 ≤ 26ms、最慢 34ms（enter_game），没有超时。时间全在轮询等待上（每次轮询 = 等 250ms + 一次请求）：

| 轮询 | 本次（2 个机器人合计） | 每机器人 | 折合时间 / 机器人 |
| --- | ---: | ---: | ---: |
| `gift_status`（两笔礼物 saga：完成 + 补偿） | 94 次 | 47 | 约 11.8s |
| `guild_op_status`（建会、捐献、领奖三笔 saga） | 96 次 | 48 | 约 12.0s |
| `list_mail` / `poll_match` | 4 / 2 | 2 / 1 | < 1s（邮件第一次就到；配对推送都收到了） |

合计约 23.8s / 27.85s。之前两份报告同样：gift_status 84 / 94 次、guild_op_status 97 / 90 次。

**saga 本身每笔约 3.8 秒**（JetStream 上 saga 流的终态记录，`CreatedAt` → 终态）：礼物完成 3.80s、3.80s，礼物补偿 7.56s（两个原生步骤），联盟领奖 3.82s。

**3.8 秒就是 NAK 退避**：演练拓扑里同一个项目有 5 个 game 进程（sid 1000～1004）消费同一份原生步骤；礼物 saga 的步骤命令落到不是发送者 sid 的进程时，`gift_saga.go` 的 `admit` 先 `handOver` 给发送者的 sid，再拒绝这次投递（“step left to the sender's sid”），消息不确认，**结果要等重投落到发送者进程上**才回放回执并发布。日志里每个步骤都恰好被另外 4 个进程各拒一次，时间点是 t、+0.25s、+0.75s、+1.75s，第 5 次投递在 +3.75s 落到 sid 1003：正是 `nak_backoff_min` 250ms 起翻倍的退避（250 + 500 + 1000 + 2000ms）。6 个原生步骤 × 4 次拒绝 = 日志里的 24 条。

| 步骤（命令前缀） | 被拒次数 | 被拒时刻（相对第一次） |
| --- | ---: | --- |
| 礼物 1 扣除 | 4 | 0、0.256、0.758、1.760s |
| 礼物 2 扣除 | 4 | 0、0.254、0.756、1.759s |
| 补偿礼物 1 扣除 / 退还 | 4 / 4 | 0、0.255、0.756、1.758s / 0、0.251、0.752、1.752s |
| 补偿礼物 2 扣除 / 退还 | 4 / 4 | 0、0.256、0.759、1.760s / 0、0.251、0.756、1.758s |

### 判断

- **不是性能退化**：请求延迟是毫秒级；慢的是“非本进程的步骤靠 NAK 重投找到主人”这条路径，每个原生步骤加约 3.75s，是退避表与“5 个 game 进程共用一个步骤消费者”这个演练拓扑共同决定的，与地形、分区、本轮同步改动无关。
- **阈值与样本**：阈值 16s 的依据（参数说明）是“一到两个 game 进程时最慢机器人 7～10s”；无缝演练有 5 个 game 进程，6 个原生步骤每个约 +3.75s，约 +22s，16s 在这个拓扑上必然超。2 个样本时 p95 就是最慢的那个机器人。
- **为什么时过时不过（推断，尚未确认）**：拒绝次数取决于 sid 1003 在投递轮转里排第几——本次与之前两次都是第 5 个（4 次拒绝）；排得靠前就只差几百毫秒，11-10 那次 13.54s 的通过与 11-09 的 16.45s 可能就是轮转位置不同。本次没有改变进程启动顺序去验证。
- **公会 saga**：联盟领奖同样约 3.8s，与礼物同一量级；公会扣费步骤的拒绝没有日志，推断是同一机制，尚未确认。
- **本机环境**：有后台负载与 swap 偏高，但只影响毫秒级部分，解释不了每步 3.75s。

需要维护者决定的（产品选择，本轮不改）见 [KNOWN-LIMITS](../../KNOWN-LIMITS.md#2026-11-10无缝演练最后一步普通场景机器人压测阈值时过时不过尚未确认) 该条的追加结论。

## 结论分类

**已测**

- 同步字节与方案估算全部相符：走一步 30B、进视野 85B、128 人进视野合计 9787B、128 个同时动 18.6KB/s（64 个 9.4KB/s）；地形下行与 terrain-quota 证据同一量级。
- entitysync / frame / 地形 / cellworld 微基准各一个样本（量级参考，未与旧基线做同机交替对照）。
- 无缝演练最后一步 p95 超阈值的原因：每个原生 saga 步骤约 3.75s 的 NAK 退避（5 个 game 进程共用步骤消费者，结果等重投落到主人才发布），不是请求变慢。

**尚未确认**

- sync-business 业务负载（每 tick 开销、分配、10% 常态下行）：夹具与当前 core 不兼容，没有数字。
- 64 人进视野合计比上一轮多 10B（分帧不同）。
- 时过时不过取决于 sid 1003 在投递轮转中的位置；公会 saga 步骤走同一机制。
- bisect 与开地形无缝演练的延迟基线（按新预算没有做）。

**本机做不了**

真机与 Unity 表现、跨机部署、单格数千玩家、长稳；微基准的多轮同机交替对照不在本轮预算内。

## 原始输出

原始输出（演练日志、loadtest 报告、saga 终态记录、基准原文、本地补丁 diff）保存在仓库外的证据目录 `perf-terrain-acceptance-20261110/`，不提交。
