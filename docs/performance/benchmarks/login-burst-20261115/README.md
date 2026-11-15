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

## 四、第二轮：维护者 2026-11-15 的四项决定与复测

维护者看过上面三节后决定：登录公告合并成每秒至多一条；M-15 把机器人的进入点分散开（接受测试条件改变）；进场景前请求期限已过的 EnterGame 答 `login_timeout` 并让机器人退避重试；两条框架发现（pipelined 第一阶段仍按实体排队、async 记录排在 nestwal 单 writer 的 strict fsync 之后）作为已知限制记录，不改代码。Base 兴趣 / 同步容量的优化另开一批，本轮只把发现记在下面作为输入。

### 1. 登录公告：每秒至多一条（demo `game/announce`）

- 放在 demo，不放 chat 服务：“登录要不要公告、怎么说、多久合并一次”是 demo 的产品决定；chat 服务只负责发一条系统消息，对所有系统消息一视同仁，替调用方攒批会把产品规则塞进通用服务。
- `announce.Batcher`：EnterGame 只调 `Entered(playerID)`（加锁记一笔，不做 I/O）；每个 1 秒窗口结束时发一条“N players entered the world: 前 8 个玩家号 and K more”（同一窗口里重复进入的同一名玩家只算一次；8 个号保证一条不超过世界频道 256 字节）。没有人进入的窗口不发。幂等键是“sid + 进程起始时刻 + 批序号”。
- 尽力而为：一批发送失败只记一条 Warn（带人数），不重发——公告是告知，不是账。停机时 `Close` 用停机 ctx 发出还没发的那一批；之后才到的进入不公告，只在 Close 的日志里计数；进程崩溃时最后不足 1 秒的那一批丢失。
- 回归：`TestALoginBurstIsAnnouncedOncePerWindow`（50 人一窗 → 一条，计数与点名正确；空窗不发；下一窗另一条、另一个键）、`TestCloseAnnouncesThePendingWindowAndNothingAfterIt`（停机发出未发的一批，之后的不发；失败的一批不重发）。变异（改回逐次公告、Close 不发最后一批）都变红，原文在 [mutations-round2.txt](mutations-round2.txt)。
- M-15 里每次登录潮 5 秒共 6～7 条公告（世界频道写成 6～7 次、抢输 0 次，[chat-cas.txt](chat-cas.txt)），此前是 1000 条。

### 2. 进入点分散：服务端配置 `scene.partition.spawn_spread`，演练设 1400

- 做法：没有快照的玩家进入分区场景时，进入点由服务端决定——`settings.ScenePartition.PlayerSpawn` 按玩家号（splitmix64）在出生点周围半边长 `spawn_spread` 的正方形里取一个固定点，截到世界内；0（默认）= 都从出生点进入，与旧行为相同。`scripts/test-seamless-demo.sh` 的 CAPACITY 模式按 `ROOST_SEAMLESS_SPAWN_SPREAD` 写这个配置（锚点找不到时失败，不让它悄悄不生效），性能入口的 `s11-cellworld-robot` 默认 1400，设 0 回到旧条件。
- 为什么这样做：出生点本来就是服务端的决定，真实游戏里也会把新玩家散开；不改机器人的爬坡、阈值和行走逻辑（机器人仍然从进入点走到它的分散目标，再测 60 秒），也不绕过 Nest 或直接改服务端状态。
- **测试条件变了**：分散之后开局每人视野里约几十人，不再是 1000 人互相可见。带 spread 的轮次不能与上面三节（同一出生点）的轮次当成同一条件比较；下表把两种条件分开列。
- 回归：`TestPlayersWithoutASnapshotEnterSpreadAroundTheSpawn`（1000 人落点在正方形与世界之内、互不相同的足够多、同一玩家每次相同；0 时都在出生点）。变异（忽略 spawn_spread）变红。

### 3. 进场景前期限已过 → `login_timeout`；机器人退避重试

- EnterGame 在进复制场景（place）之前检查请求 ctx：期限已过就答 `login_timeout`（100021），不再答成功——此前这种情况会答成功，但玩家并不在场景里，客户端以为进了游戏却收不到同步。进入钩子都是可重复的（下次进入再跑一次），EnterGame 可以重试。
- 回归：`TestEnterGameAnswersLoginTimeoutWhenTheDeadlinePassedBeforeTheSceneJoin`（在 place 之前停住直到期限过去 → 答 100021）；机器人侧 `TestRobotEnterRetriesLoginTimeout`（直连与 portal 两条路径，前几次答 login_timeout 时按 200ms 起翻倍退避、加按机器人编号的 0～99ms 错开，至多再试 4 次，其他错误不重试）。两侧变异都变红。
- 限制：重试的那次 EnterGame 答复里 `First=false`（第一次已确认过首次进入）。M-15 的 5 轮里 `login_timeout` 一次都没出现（进入路径没有超时），这条路径只由回归覆盖，没有在 M-15 里被触发过。

### 4. 复测（阈值、爬坡、时长不变）

| 轮次 | 条件 | Robot 失败 | Base 进入路径失败 / 请求超时 / login_timeout | 握手 → 进入完成 p99 / 最大 | 公告条数 | Base 兴趣积压（结束时） | 同步会话丢失 | 预热期 Base 订阅失败开始 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| feat/v148 原测 | 同一出生点、逐次公告、Sync_RecordEnter | 310 | — | — | — | — | — | — |
| `lb-baseline` | 同上 | 368 | 422 / 438 / — | 3.02s / 3.06s | 587 写成 | 0 | 14 | — |
| `lb-fix3` / `lb-fix4` | 同一出生点、逐次公告、修复后 | 446 / 580 | 0 / 0 / — | 1.17s / 1.20s；1.47s / 1.52s | 1000 | 37.7 万 / 49.6 万 | 222 / 444 | 是 |
| `lb-r2-nospread` | 同一出生点、合并公告 | 962 | 0 / 0 / 0 | 1.03s / 1.03s | 7 | 40.3 万 | 755 | 是（第 10 秒起） |
| `lb-r2-nospread-b` | 同上 | **0（通过）** | 0 / 0 / 0 | 0.73s / 0.75s | 6 | 0 | 17 | 否 |
| `lb-r2-spread-a` | 分散 1400、合并公告 | 240 | 0 / 0 / 0 | 0.88s / 0.92s | 7 | 0 | 154 | 否 |
| `lb-r2-spread-b` | 同上 | 21 | 0 / 0 / 0 | 0.94s / 1.03s | 7 | 0 | 28 | 否 |
| `lb-r2-spread-c` | 同上 | 417 | 0 / 0 / 0 | 0.93s / 0.97s | 7 | 0 | 199 | 否 |

（“预热期 Base 订阅失败开始”：`scene: subscribe … session is not open / subject is not registered` 警告在 30 秒预热结束之前就成批出现。“否”的轮次里这些警告与会话丢失都发生在失败的机器人预热结束、断开之后，是失败的结果，不是原因。各轮细节在 [runs-summary.txt](runs-summary.txt) 末尾。）

读法：

- 进入路径：第一轮修复之后的 7 轮复测（`lb-fix3`、`lb-fix4` 与本轮 5 轮）全部 0 失败、0 请求超时，本轮 1000 人握手 → 进入完成的 p99 在 0.73～1.03 秒。公告从 1000 条降到每次登录潮 6～7 条。
- M-15 整体仍不稳定，轮间差异远大于条件之间的差异：同一条件下 0 与 962、21 与 417。3 轮分散的结果是 21 / 240 / 417（中位 240），2 轮不分散是 0 / 962。**用 2～3 轮无法给这两种条件排序**，更不能据此说某项改动让 M-15 变好或变坏。
- 两种条件下失败的形态不同：不分散时有一轮在预热期就出现 Base 兴趣处理崩溃（积压 40 万、755 个会话丢失、几乎全员失败）；分散的 3 轮里 Base 兴趣在预热期都健康（积压 0，订阅失败只在机器人放弃之后），失败的机器人是在同步正常的情况下没有在 30 秒内“停在”目标 ±20 之内。分散去掉了“开局人人互相可见 → Base 兴趣崩溃”这一失败模式，剩下的是另一种失败。

### 5. 留给下一批（Base 兴趣 / 同步容量与机器人行走）的输入，均**尚未确认**

- 同一出生点时 Base entitysync 兴趣的饱和：日志积压 37～50 万、预算停下 1,782～2,187 次、重试搁置 1.9～4.2 万次、同步会话丢失 222～755、`scene: subscribe` 警告 4.4～6.1 万条（其中 `subject is not registered` 338～1,533 条，其余是 `session is not open`），都从第一名玩家进入后约 10 秒开始（`lb-fix3`、`lb-fix4`、`lb-r2-nospread`）；发生与否在轮间不稳定（`lb-r2-nospread-b`、更早的 `lb-exp-noannounce` 都没有发生）。
- “未到分散目标”与目标位置相关：进入路径不再失败、且不是几乎全员失败的各轮（`lb-fix4`、`lb-exp-noannounce`、spread a/b/c）里，失败人数都随目标 y 增大而升高，y 2500～2700 段的折算人数是 y 300～500 段的 1.7 倍到数十倍（`runs-summary.txt` 末尾的分段表；基线的失败来自进入路径，与位置无关，不在此列）。世界、出生点与 NPC 布置在 y 方向上并无明显不对称，原因未查。
- 失败的机器人集中在编号小（先登录、在登录潮仍在进行时开始行走）的那部分。
- 机器人到达判定：每 100ms 按回传位置做比例控制（速度 = 2 × 距离，截到 300），“到达”只看预热结束那一刻是否在 ±20 之内；位置回传有延迟时可能在目标附近来回、在截止那一刻不在框内。这是否就是剩余失败的主要原因，未验证。
- cellworld 节点计数：Base 的 `InputsResent` 每轮约 45～53 万（与机器人发的意图数同阶），两个 Cell 各 `InputsDropped` 0.4～4.4 千；这些是否属于正常的 owner 切换重发，未查。
