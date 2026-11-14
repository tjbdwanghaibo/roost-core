# M-15 登录潮：Base 进入路径超时的根因与修复（2026-11-15）

[性能入口](../../../maintenance/evidence/v147-performance-preparation-20261114/README.md)的 `s11-cellworld-robot` 段：`scripts/test-seamless-demo.sh`，ROBOT_ONLY + CAPACITY，1000 人、60 秒、active 5%、预热 30 秒；机器人爬坡每 100ms 20 人（约 5 秒登完）。macOS arm64 本机（10 核，load 约 4～8），私有镜像环境偏移 18000。每轮约 4 分钟。原始输出（机器人报告、各进程日志、前后指标）在检出目录 `artifacts/perf/v147-remaining/lb-*/`，不入库；本目录只放摘要与脚本：

- [runs-summary.txt](runs-summary.txt)：每轮的机器人结论、失败分类、Base 每名玩家的进入时序（`enter-timing.py`）与 Base 指标；
- [burst-series-*.txt](burst-series-baseline.txt)：登录潮期间每 0.5 秒采一次的 Base 指标（`metrics-sampler.sh` + `burst-series.py`）：在线连接、World 上 `handlerRecordEnter` 的完成数与平均锁持有、WAL fsync 次数与平均耗时、请求超时数、Nest 各池等待数；
- [chat-cas.txt](chat-cas.txt)：每轮 chat 服务世界频道键的 compare-and-set 写成 / 抢输次数；
- [red-enter-sync.txt](red-enter-sync.txt)、[red-chat-nolock.txt](red-chat-nolock.txt)：两条回归在修前（变异）时的失败原文。

## 一、根因：唯一的 World 在登录路径上逐次等 fsync

EnterGame 在装载、首次初始化（EnterInit）之后同步调用 `Sync_RecordEnter`，在 World（全进程唯一）上记一次登录。这笔计数是 `durability=strict`：fsync 在 World 锁内；Nest 又让同一实体的下一条消息等上一条（含回复）结束，所以 World 每次 fsync 只记一人。

证据（基线 `lb-baseline`，与 feat/v148 上此前两次 M-15 同一特征）：

- `handlerRecordEnter` 平均锁持有 8～11ms（同时段 WAL fsync 平均约 5ms，等上一批 + 自己一批），每 0.6 秒只完成 35～70 笔，约 75～120 笔/秒；登录到达约 200 人/秒。Nest short 池等待数（同一实体排队的消息也计在内）从 93 涨到 639，随后请求超时开始（`burst-series-baseline.txt`）。
- 每名玩家：握手 → EnterInit 只要 10～400ms，EnterInit → 完成随到达顺序线性增长，到 3.0 秒封顶（单个请求的分发期限 `player_access.tcp.dispatch_timeout`，未设时随 `nest.request_timeout`，本演练为 3s）；失败玩家的“握手 → 第一个失败”全部落在 3.00～3.05 秒。首个失败步骤：356 名在“world did not record the login”（World 队列里等光预算），36 名在进场景，其余在进入钩子 / 公告——期限已过，之后每一步立即以 `context deadline exceeded` / `nest: sync canceled` 失败，所以日志里每名失败玩家都有一串钩子失败。
- 为什么是后来的玩家：队列以“到达 200/秒 − 处理约 110/秒”的速度增长，登录潮开始约 2.3～2.7 秒之后到达的玩家在 World 前等待超过 3 秒；按握手秒分桶，第 0、1 秒全部成功，第 2 秒起失败占多数（`runs-summary.txt`）。没进复制场景的玩家不会移动，于是在预热期“did not reach spread target”。

两条曾试过但无效的做法（保留为排除依据）：

- `durability=pipelined`：第一阶段 pipelined 在锁外等票据，但派发器按实体 ID 的准入链仍让同一实体的后继等这条作业（含回复）结束；回归在 pipelined 下同样变红。
- `durability=async`（`lb-fix1`）：M-15 上没有改善（311 名仍卡在 World，锁持有仍约 8.6ms）。nestwal 只有一个 writer，async 记录要等同批或上一批 strict 记录的 fsync；登录潮里 EnterInit、`cellworld_base_enter` 等 strict 提交源源不断，async 的锁持有同样约等于一次 fsync。

修复（demo 参考代码）：EnterGame 只投递计数（`Send_RecordEnter`），不等；handler 仍是 strict。登录本来不看计数的结果；投递之后计数在 World 上排队补记（异步消息不带请求期限）。回归 `TestEnterGameDoesNotWaitForTheWorldsLoginCount`（`game/controllers/player/enter_login_scenario_test.go`）：World 计数的提交停住时 EnterGame 照常答复，放行后计数为 1；改回 `Sync_RecordEnter` 时 3 秒不答复、变红（`red-enter-sync.txt`）。

## 二、第二个瓶颈：chat 世界频道的写在同一进程里互相抢 compare-and-set

World 不再挡路之后（`lb-fix2`），1000 条登录公告几乎同时写世界频道。频道状态是一个 versionstore 键，每次写是一次 compare-and-set；chat 进程里并发的写互相抢：281 条写成、6391 次抢输，699 名玩家的公告输满 8 次以 `chat: conflict` / NATS 请求超时失败（`chat-cas.txt`）。同一轮还有 30 个机器人在登录前一步（account CreateRole，经 NATS）超时；它们在 chat 修复后的两轮里消失，推测是抢写把共用的 Redis 拖慢所致，**尚未单独确认**。

修复（core `service/chat`）：`channelStore` 加按频道键的进程内写锁 `writers`，同一进程对同一频道的写排队再做 compare-and-set（等待看 ctx，空键不留表）；多进程之间仍只靠 compare-and-set。回归 `TestConcurrentPublishesToOneChannelQueueInsteadOfFightingOverTheWrite`（屏障让第一轮的读全部先于写）：去掉写锁时 8 条写抢输 12 次、有的以 conflict 失败；加上之后 0 次（`red-chat-nolock.txt`）。之后两轮世界频道 1000 条写成、0 次抢输。

## 三、修复后的 M-15（两轮，参数不变）

| 指标 | 基线（feat/v148） | 修复后 A（`lb-fix3`） | 修复后 B（`lb-fix4`） | 试验：去掉登录公告（不提交） |
| --- | --- | --- | --- | --- |
| Robot 失败 | 368 | 446 | 580 | 194 |
| 失败原因 | 预热期未到分散目标（后来的玩家） | 预热期未到分散目标（各 ID 段均匀） | 同左 | 同左 |
| Base 进入路径有失败的玩家 | 422 | 0 | 0 | 0 |
| 握手 → 进入完成 p99 / 最大 | 3.02s / 3.06s（封顶于期限） | 1.17s / 1.20s | 1.47s / 1.52s | 0.74s / 0.76s |
| Base 请求超时（`player_tcp_dispatch_timeout_total`） | 438 | 0 | 0 | 0 |
| 世界频道写成 / 抢输 | 587 / 77 | 1000 / 0 | 1000 / 0 | — |
| Base 兴趣日志积压（结束时） | 0 | 377,329 | 495,652 | 0 |
| Base 兴趣预算停下 / 重试搁置 | 44 / — | 1,895 / 22,140 | 1,782 / 18,783 | 561 / 11,103 |
| Base 同步会话丢失 | 14 | 222 | 444 | 96 |
| Base 推送总数 | 816,686 | 1,098,972 | 1,065,978 | 757,463 |

读法：

- 本轮要修的“登录潮里进入路径超时”已经消除：1000 人全部在 1.5 秒内完成进入（含进复制场景与全部进入钩子），没有一个请求超时。
- M-15 仍不达标，失败换了特征：不再集中在后来的玩家，而是各 ID 段均匀分布；Base 的 entitysync 兴趣（AOI）处理跟不上（日志积压数十万、预算停下上千次），同步会话丢失 222～444 个，`scene: subscribe … subject is not registered` 警告 4～6 万条。此前只有约 630 人进入场景，现在 1000 人都在，而且出生点相同，开局时人人互相可见。
- 登录公告是其中一部分负载：每条公告推给当时在线的所有人，千人登录潮是约 50 万次推送（推送总数多了约 30 万）。去掉公告的试验里失败降到 194、积压清零，但仍不达标——剩下的是 Base 在 1000 人同出生点时的兴趣 / 同步容量，属于 Witness / AOI 一侧，不在进入路径上。
