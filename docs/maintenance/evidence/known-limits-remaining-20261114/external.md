# v147 外部环境验收清单（2026-11-14）

本轮只核对现行入口、参数与关闭证据，没有执行 C。当前暂无生产部署；36 项外部验收继续保留。下列命令是在对应隔离环境运行的计划，不是通过记录；宿主本地多进程不等于跨机。需要远端拓扑 / 设备 / 专用夹具的项目明确列出，不能只替换环境变量就宣布覆盖。

仓内反引号路径均相对仓库根目录。No. 与 C 编号来自 [remaining.md](remaining.md) 的 `28055f06` 原始分类；按标题定位，不按旧行号。入口核对至 `f62ceec7`，包括正式 RPC 参数修正 `e980cdb7` 和 P8 `4fc7f02b`；最终 E2 只有实际负载窗口未复现，不能关闭外部设备 / 弱网项。

## 1. 上游与读取边界

| 代号 | 当前路径 | 精确标题 / 定位 |
| --- | --- | --- |
| R | `docs/maintenance/evidence/known-limits-remaining-20261114/remaining.md` | `4. C：按环境分组`；原分类的 36 行，重复 No. 合并到同一证据 |
| K | `docs/maintenance/KNOWN-LIMITS.md` | `二、遗留问题` 各领域精确小节，下面逐行指定 |
| External | `docs/maintenance/EXTERNAL-VERIFICATION.md` | `外部验证操作清单` / `总表` / E01～E28；E14 已有本机精确窗口通过、E25 服务端 Windows 排除，不把二者写为未做 |
| HB | `docs/performance/HB-DRIVEN-LOAD-20261021.md` | `4. 待补测（2026-10-21 因宿主资源占用推迟）`、`4A. 2026-10-22 晚正式测量（v1.31.0 发版前压测第 6 项）` |
| CW | `docs/performance/CELLWORLD.md` | `CellWorld 容量与验收` / `验证口径`、`复跑入口`；不是 `docs/framework/CELLWORLD.md` |
| Phys | `docs/framework/PHYSICS-ROOM-SERVICE.md` | `物理房间 service` / `共享索引与多房间测量（2026-10-26）`、`长稳、RSS 与原生内存（2026-10-29，macOS ARM64）` |
| Migration | `docs/framework/PLAYER-MIGRATION.md` | `2. 步骤`中的按 sid 搬机步骤、`3. 检查工具`、`7. 单个玩家换服（account 一侧）` |
| Terrain | `docs/maintenance/TERRAIN-STATUS.md` | `可编辑地形与混合同步：完成情况及待办` / `4. 已执行验证与待补验收` |
| Native | `client/cpp/README.md`、`client/unity/README.md`、`client/README.md` | `构建`、`验证入口与状态`；`Unity 客户端接入` / `构建可导入包`、`完整可运行验收场景`；`RoostSceneTime`说明 |
| UGC | `client/physics-viewer/README.md` | `Room 场景：按钮控制房间中的球`、`UGC 关卡与本地预测` |
| RPC | `docs/performance/benchmarks/alliance-probes-20261114/README.md` | `RPC 并发入口补齐`；功能参数验证，不是 C-Q1/Q2 正式容量结果 |
| P8 | `docs/maintenance/evidence/operations-real-20261114/README.md` | `验收窗口与结果`、`范围与 KNOWN-LIMITS 回写文本`；活动真实恢复已通过，长稳仍未验 |

图谱 Tier 2：项目 `Users-whb-roost-worktrees-v147` ready，覆盖 generation `2026-11-14T02:55:37Z`。首批已对 42 个相关文档/脚本/源码精确路径检查覆盖，收尾补查 RPC 报告和 P8 报告/脚本/夹具 4 个新路径；新 alliance `perf_test.go.txt` 为 not_tracked，gate-mixed/sync-aoi/alliance 脚本 metadata_changed，初审其余路径无记录缺口、metadata_match；收尾新路径为 not_tracked，已按精确路径读现行文本。已直接读取相关范围，不重建索引。函数发现：Saga 真实用例/Lockstep 快照精确名 12 项完整；物理负载/Soak 精确名定位；PhysX 关闭图谱未恢复 native.go 方法，直接补读。Soak/run 的图谱出边包含跨包同名误配，只采信精确源码确认的链路。一次 Remote broad search 返回 39 首 30，未穷尽；本文不据此声称全仓不存在某个长稳入口。

共通证据要求：独立依赖与命名空间、实际拓扑/主机与版本、有效参数和 CPU/池配置、原始日志与失败样本、运行次数/时长、退出状态、真正执行的用例与非预期 SKIP、完整性/一致性与性能分别判定。没有既定数值门槛的项目先记录实测，不能临时发明阈值宣称关闭。性能数字不跨平台直接算退化百分比。

## 2. Linux 单机（C-L1～C-L12）

所有命令默认仓库根目录、`GOWORK=off GOFLAGS=-p=2`。需要依赖时由该 Linux 环境提供私有 Mongo 副本集/Redis/NATS/etcd/原生库；不能 source 开发者本机路径或 reset 共享环境。`ROOST_PERF_CPU=0`应明确记录有效 GOMAXPROCS；旧报告 4/32/4 核与当前要求 8/8/自动 CPU 不是同参数样本。

| C / No. / K 精确小节 | 现行入口及可执行性 | 什么证据才能收口 |
| --- | --- | --- |
| C-L1 / 1②、8 / `实体心跳：遗留`、`压测工具的心跳驱动：遗留` | HB §4 的四段仍有入口：`scripts/perf/sync-aoi.sh` 的 `-move-driver=loop\|heartbeat -move-period=100ms`；`scripts/perf/nest-game.sh` 的 `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker\|heartbeat`、`ROOST_NEST_GAME_HEARTBEAT_DELIVERY=batch\|dispatch`；`gameplay/heartbeat/internal/bench` 的 `TestDeliveryRun`、`TestDriverOpenLoop`、`BenchmarkDriverDispatch`。参数按 HB 与 R/M1 冻结，Linux 每格>=3 交替。16 额外视野 sync 模式超现行 8 上限，必须标明临时诊断构建，不能直接抄命令期待通过。 | Linux 分档原始 JSON/业务服务端 P99/跳拍与投递执行计数/CPU，重现或未重现 ticker 约 0.2s 停顿的条件与样本。不同移动负载形状不能全归因投递方式。只跑微基准不能关端到端档。 |
| C-L2 / 7 / `移动与动作、行为与技能：遗留` | `ROOST_SLG_REAL=1 ROOST_GENRE_OUTPUT=/absolute/new-evidence bash scripts/test-slg-generated.sh`；真实门变量 `ROOST_DATAENGINE_IT_MONGO_URI`、`_REDIS_ADDR`、`_NATS_URL`必须存在。脚本有 7 条缺省场景及独立真实联盟活动/Player/Remote/RPC 预算入口。 | Linux 真实模式各父场景实际执行，真实 WAL 恢复/权威交接/回执核对通过，非因依赖缺失跳过；保留完整日志。macOS 本轮通过不替代 Linux。 |
| C-L3 / 19 / `Saga：遗留` | 功能：提供**`ROOST_NATS_CLUSTER_URL`**后 `go test -tags integration -run '^TestRealJetStreamCoordinator' -count=1 -v ./framework/saga`，含`TestRealJetStreamCoordinatorKill9Replays`。直接运行 Child 按设计 SKIP 须单列，不能把父用例因缺 NATS 而 SKIP 算通过。性能：`ROOST_PERF_LABEL=linux-saga-unique bash scripts/perf/saga-js-scan.sh`。该脚本自起本机 NATS R3 与私有 Mongo，默认 15 档；允许外部 Mongo 但不支持直接分离参与方。 | 功能杀协调器后日志回放/隔离/补偿终态正确；性能 15 档实际执行，余额、非预期终态、未知发布、排空门槛分项为 0，记录每档配置/时长。脚本强制 GOMAXPROCS=4、生产缺省 short4；不是外部设 CPU0 就生效。云大负载若排空>30s 仍保留 Q6，不能通过放宽采样超时掩盖。 |
| C-L4 / 27～29 / `Lockstep：遗留` | `go test -run '^TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive$' -count=20 -v ./framework/sync/lockstep`；精确函数仍在`framework/sync/lockstep/tcp_test.go`。R 另要求同环境全仓慢套件一次观察是否拖到 90s：留给最终独立 Linux 窗口，不在本次运行。 | 20 轮快照跟链路节奏/其他会话仍活、耗时与负载条件；慢套件日志需区分真实 LinuxTCP 时序与测试期限。单次 PASS 不能归因历史长尾。 |
| C-L5 / 75 / `地形 / 无缝世界：遗留` | 现成同机 Linux 入口：`ROOST_SEAMLESS_OUT=/absolute/new-evidence ROOST_SEAMLESS_ROBOT_ONLY=1 ROOST_SEAMLESS_CAPACITY=1 ROOST_SEAMLESS_ROBOTS=1000 ROOST_SEAMLESS_ROBOT_SECONDS=60 ROOST_SEAMLESS_ROBOT_ACTIVE=5 bash scripts/test-seamless-demo.sh`。当前自起本机各业务进程/etcd，固定应用端口，容量模式不能叠`ROOST_SEAMLESS_TERRAIN=1`。分机需部署后使用下面§4 的 Robot 命令，脚本不是远程编排。 | 1000 全部入场/稳态、万 Actor、零复制缺口/重置风暴/slow consumer、延迟与 CPU/队列分解。CW 明确 ACK P99 及 Witness 不是客户端完整状态交付，50ms 目标须端到端时间戳证据；ACK 快不能判容量达标。CellWorld B/C/E 待维护者决定不自动关闭。 |
| C-L6 / 79 / `地形 / 无缝世界：遗留` | `go test -race -count=20 ./gameplay/cellworld`。这是 Linux 行为/race 复验，不是 1000 人性能入口；测量`TestCellWorldMeasure`另有显式开关。 | 20 轮相关生命周期/重复包/进退场回归实际通过、race 无报错，记录平台/内核；不能用这条关闭 C-L5 容量或跨机 HA。 |
| C-L7 / 82 / `LevelComponent 与 UGC 物理闯关：遗留` | Linux 须先用目标平台 PhysX 原生库；`ROOST_PHYSICS_LOAD=1 go test -tags physx -run '^TestMultiRoomReplicationLoad$' -count=1 -timeout=2m -v ./wiring/physicsroom`。现行测试两种场景×1/4/8 房，直接 Service/Nest/PhysX+逻辑 Poll。RSS 可用测试里的 ps 与 Linux 外部采样，`sample.py`的 footprint/vmmap 不能用于 Linux。 | 1/4/8 分档原始频率、Step/Commit/Poll P99、交付字节、Go 分配、RSS/原生堆。历史 5s 短测不是长稳/容量上限；50Hz 要看实际均频，不能只看配置。 |
| C-L8 / 92 / `Robot 推广验收（2026-11-01）` | 每轮唯一 label，`ROOST_PERF_LABEL=linux-gate-drop-<n> ROOST_PERF_CPU=0 ROOST_GATE_LOAD_RATE=100 ROOST_GATE_LOAD_SECONDS=30 bash scripts/perf/gate-mixed.sh`，5 轮；提供 Mongo/Redis/NATS 三个门变量。 | result.json verified、Sync 已应用/无 pending、`Scheduled=Completed-final_verification_requests+Dropped`；Dropped 为未发送的客户端节拍，其他拒绝/期限/心跳信号另列。同机测试进程中 Gate/Game/客户端，不是跨机或 MMO；即使客户端节拍落后也不能排除服务端慢回复。 |
| C-L9 / 93 / `10-23 候选诊断边界` | `ROOST_NEST_GAME_TAIL=metrics` + `scripts/perf/nest-game.sh`，30m×2；重档参数见下方公共命令。明确 CPU0、short8/io8/queue65536，不沿用省略默认值。 | 保存尾簇全时间序列、调度延迟、CPU/宿主事件、Mongo/NATS I/O 对应，区分全局停顿与 200ms 心跳抖动。没出现只能写该条件未复现，不能证明旧根因。majority+j:true 决定保留。 |
| C-L10 / 94、103 / `能力与验证边界` | nest-game 重档 30m；GeneratedTCP 用`DURABILITY=strict SELF_SYNC=0 CONNECTIONS=50 ENTITY_MODE=distinct OPEN_LOOP_RATE=0 ROOST_GENERATED_TCP_SECONDS=300 ROOST_GENERATED_TCP_OUTPUT=/absolute/new-evidence bash scripts/perf/generated-tcp.sh`。该脚本测试二进制 timeout 固定 10m，**不要改 SECONDS=1800 当 30m 档**。 | nest-game 计数/DAO 终值/AOI 集合/延迟/错误完整；GeneratedTCP 保存 confirmed 数/冷载 Gold 相等/WAL 排空/错误/late/drop。二者分报告；后者客户端与服务模块同测试进程，不是旧 10/20 service Main 多进程的直接同参数对照。 |
| C-L11 / 95、106 / `能力与验证边界`、`待补测（历史压测遗留）` | 同公共 nest-game 重档，`ROOST_NEST_GAME_TAIL=metrics ROOST_NEST_GAME_DURATION=15m ROOST_PERF_COUNT=2`（或>=2 独立 label）。 | 长尾簇与整分钟相位、每段事件计数/最大值、是否同秒调度延迟；无长尾不能算根因关闭。可与 C-L9 同一更长数据共享，但各自结论分列。 |
| C-L12 / 101、109 / 同上 checkpoint 同盘 | R 指向历史 NIGHT-20261022“正向 8/s×15m、普通盘/tmpfs”口径，当前仓内没有该历史报告文件；本次不能据一条旧引用给出现成同参数命令。现有`framework/nestwal`、engine 基准以及 Saga JS 逐秒序列可作诊断部件，**需先明确现行等价夹具与 WAL 目录/逐次 fsync 采样**，也可按 External/E18 另测但不是同一项。 | 同硬件普通盘/tmpfs 配对、逐次 checkpoint/fsync/Mongo 等待与吞吐尾部；不放宽持久语义。tmpfs 同时去掉设备 fsync 及同盘竞争，二者不能单独归因；M19 后台 checkpoint 实验不自动替代此 Linux 证据。 |

Linux 重档公共命令（每轮换 label，时长/重复次数依对应行设置；是当前明确 8/8/自动 CPU 参数，不声称与历史 4/32 完全相同）：

```sh
ROOST_PERF_LABEL=linux-heavy-unique ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 ROOST_NEST_GAME_TAIL=metrics \
ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 \
ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_HZ=1 ROOST_NEST_GAME_DIRTY=1 \
ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 \
ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_DURATION=30m \
GOWORK=off GOFLAGS=-p=2 bash scripts/perf/nest-game.sh
```

该夹具 Gate 模式需私有`ROOST_DATAENGINE_IT_NATS_URL`；网络前端仍在测试进程内，不能据此称 Gate/Game 已跨 OS 进程或跨主机。历史需要 embedded 口径时显式改成 EMBEDDED 而不是同时设两者；比较必须记录入口变化。

## 3. 安静独占机器 / 目标Mongo硬件（C-Q1～C-Q2）

| C / No. / K 定位 | 当前入口与缺口 | 关闭证据 |
| --- | --- | --- |
| C-Q1 / 14 / `Remote：遗留`“单实例 128/256 并发约 3s 全体超时” | `ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RPC_CONCURRENCY=128 ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$' ALLIANCE_E2E_LOGS=/absolute/new-evidence bash scripts/test-alliance-e2e.sh`；换 256 并发，按 R 各 8 轮、每轮独立输出。`e980cdb7`正式读取 1..256、默认 16、写额度 512；Found 初始化仍固定 1/16，只有 Info/Join 分散及热点档使用变量。分散目标固定 16 联盟，单热点新增最多 256、总数不超 500。background_checkpoint 实现/配置仍在实验。 | 目标 Mongo 三成员分机/分盘及明确宿主空闲证据；每轮实参/样本量/超时簇起止、RPC 结果分类、Projector/Mongo/WAL 停顿栈、fsync/outbox delta、主从慢日志与 CPU/交换。8 轮没有簇仅是该配置下未复现；有簇按证据继续定位。 |
| C-Q2 / 15.1 / 同节“单实例分散 Join 目标 1000/s” | 同一正式 RPC 入口设置 128/256，以`join-spread16`统计为本探针口径，单热点另列，不把 Found 或顺序基线写为可变并发。B2 `TestAllianceE2EJoinRetryableSources`已经有`ALLIANCE_E2E_RETRY_CONCURRENCY=128\|256`、`ALLIANCE_E2E_RETRY=1`、写额度 512、effect worker 开关，可作 Join 诊断，但逻辑吞吐与 RPC attempts 不同，不擅自替换原容量口径。 | 目标硬件上分散 Join 实达 1000 逻辑成功/s、最终成员/守卫一致、错误/unknown 与重试数量、采样窗/重复、Mongo 命令与事务/秒；严格持久与业务上限不放宽。真实联盟最多 500 成员，1k/5k/10k 合成形状不是大联盟容量。 |

alliance 脚本自起本机 etcd 和业务进程；Mongo/Redis/NATS 可从隔离环境变量提供，配合远端 Mongo 测目标存储可以，但这不等于业务服务本身跨机。需 Mongo、Redis、NATS、proxied Redis、toxiproxy 五个门变量。RPC 报告已记录修前设 4 实际 16 的参数核对红、修后 c4 实际脚本 1 PASS/0 SKIP 与每档 failed/slow_ge_3s 均 0；这只验证开关和人数边界，不是 128/256 或 1000/s 容量结论。`measure`对 RPC 错误/≥3s 样本只写日志，脚本 PASS 并不保证每档零错误：收口必须核对所有`PERF rpc`行及慢样本，并保留外部采样栈。

## 4. 跨机 / HA（C-X1～C-X11）

这些行没有一条能靠“本机脚本换个名字”全部关闭。可复用当前正式程序与本机故障步骤；尚缺远程编排的明确写出。需要实际部署拓扑、可达地址、SID/服务类型/存储域、时间同步、卷归属、负载机与故障对象等参数之后再准备命令。

| C / No. / K 定位 | 现有可复用入口；缺少什么 | 关闭证据 |
| --- | --- | --- |
| C-X1 / 16 / `Saga：遗留` 上线前缺口 | `scripts/perf/saga-js-scan.sh`、`framework/saga/jetstream_coordinator_real_integration_test.go`/性能夹具是基线；脚本自起本机 NATS R3，协调器/参与方测试装配不提供分机角色 CLI，**尚需目标拓扑编排**。 | Linux 上 Mongo/NATS R3 分主机、协调器/参与方目标部署，持续准入限流+积压追平；逐账户资产守恒、终态/回执恰好一次、unknown 可核对，保留 30s 追平是否达标与 Q6。 |
| C-X2 / 17 / `Saga：遗留` 正式性能范围 | 同脚本生产推荐档可本地/远端 Mongo 运行，但不能只设 URI 就称协调器/参与方分机；需部署其正式角色与等价负载客户端。 | 至少 3 同档样本，RTT/拓扑/配置完整、业务/投影分项成本、正确性全部过；手写同形 DAO 和 wiring Steps 未在该基准里的范围原样保留。 |
| C-X3 / 31⑦、99 / `Lockstep：遗留`、能力表追帧 | 当前本机参考：`ROOST_MOBA_REAL=1 ROOST_MOBA_ROUTING=1 bash scripts/test-moba-generated.sh`；独立 Gate 慢链路 `LS_DRILL_FRAMES=3000 LS_DRILL_RATES=256,64 go test -tags integration -run '^TestRealGateLockstepCatchupOverASlowLinkDrill$' -count=1 -timeout 20m -v ./wiring/gate`。需私有 toxiproxy/NATS。它们自起本地对端；MOBA 交接下游是 recorder。 | 尚需真实客户端/负载机、跨机或真实移动网络、快照/帧摘要与已提交头、断线重连、追帧与链路净速率、deadline 分类。`256,64`是 kB/s；原 262144,65536 不能作弱网。独立 Gate 快照循环断开的现行边界不得被纯追帧通过覆盖。 |
| C-X4 / 46 / `业务模块（roost module）` login | `scripts/test-demo-login.sh`和`ROOST_GATE_CPP=1 bash scripts/test-gate-client.sh`是本机回归参考；前者两 Gate 真实进程、本机两 account，后者 Gate/Game 同 Go 测试进程。**需要部署两个远端 Gate、Game 和 account 及真实客户端地址/票据提供者**。 | 同 SessionID 旧握手迟到不得挤掉新连接，game/Gate/account 故障后的旧连接退出与新入场、资产/命名/self_sync 一致；目标部署记录，不把 portal 静态清单写成自动发现。 |
| C-X5 / 58、59、102、111 / `框架服务与 game-demo：未测部分`、历史匹配队列 | `ROOST_REVIEW_CLUSTER=<隔离多机Cluster地址列表> bash wiring/scripts/integration/redis-cluster-suites.sh`能指向给定 Cluster，相关门是**ROOST_REVIEW_CLUSTER**，不是仅设置 ROOST_REMOTE_CLUSTER_IT。该脚本是功能套件，某些非 Cluster 门用例按设计 SKIP；不自动做跨机分区/持久丢写/容量，也不等于 ZADD GT 结算榜负载。 | 另需分区/切主编排、目标业务长负载、已确认与 unknown 写账本、匹配/墓碑/水位不回退；异步复制丢写属边界，记录不能凭空保证不丢。结算榜并发最终 max 正确、失败对账限制和 24h 量另验。 |
| C-X6 / 74 / `地形 / 无缝世界：遗留` | `ROOST_SEAMLESS_TERRAIN=1 ... bash scripts/test-seamless-demo.sh`只本机小地图功能；CW 核心成本与 Robot 入口可复用。**T09 的 1w×1w 地图/3000 单服/10w 人没有一条现成等价跨机命令，需实际地图、负载分片与业务部署参数**。 | 地形/建筑完整版本和确认时序、迁移与重连、端到端同步字节/带宽/CPU/内存、目标人数密度与持久恢复分别证据；B11 仅 WAL 恢复不能覆盖容量。T10 未实现不是测试可关闭项。 |
| C-X7 / 81 / `LevelComponent 与 UGC 物理闯关：遗留` UGC 分频 | `scripts/physics/verify-service.sh`及`wiring/physicsroom/cmd/physicsroom-local`三子命令是参考；后者本机 SID/端口常量，**需业务部署入口、房间→owner 路由及 WAN 负载端**。 | 目标房数/刚体/约束/20 逻辑客户端与真实客户端分开计；实际物理/交付频率、WAN 丢包重连、分频显示/预测误差、CPU/RSS 与原生堆；不能用直接 Poll 短测作网络容量。 |
| C-X8 / 89 / `平台与外部验证` | External/E01～E28 为上游分项操作表，详见下方子清单；其中大量是人工环境/拓扑步骤，不能统一一条脚本。 | 每个选定目标分项的环境、操作、结果与判据；没部署不伪造。E14 已本机关闭、E25 服务端 Windows 排除；客户端 Windows 仍在 C-U4。 |
| C-X9 / 90、96 / `平台与外部验证`、能力表独立 Gate | `scripts/test-gate-real.sh`、`scripts/test-gate-client.sh`、`scripts/perf/nest-game.sh`的 GATE=1 覆盖组合部件，**不是现成跨机 Gate/Game 编排**。P6 两 Gate 进程仍同宿主。 | 负载端、Gate、Game 分别部署；NATS 故障/分区/重连/会话代际、实际业务和 Sync/Lockstep 交付、错误归类与排空；给出进程/主机身份。 |
| C-X10 / 91、104 / `平台与外部验证`、schema/Player 迁移能力行 | 单玩家参考`scripts/test-demo-rehome.sh`（本机嵌入式玩家 TCP，不含独立 Gate）；静态同 SID 搬机用 Migration 步骤及生成工程`cmd/sidmove`。**仍需两台机器、持久卷与网络路由切换编排**。迁移检查工具只读，不搬 WAL 或自动切流。 | 六 kill 点 account/玩家入口/资产/去重与 WAL；异常退出必须原 WAL 恢复后排空，或持久卷原样单写者挂载；不能新机器空 WAL 接管。Q4 committed 后旧源档/定时器隔离尚未定，跨机通过也不自动关闭；同 SID 搬机与玩家换 SID 分别记录。 |
| C-X11 / 100 / `能力与验证边界` Remote 容量 | `ROOST_REMOTE_RATE=80 ROOST_REMOTE_DURATION=15m bash scripts/perf/remote.sh`只现有生成链基线；它先调用`dataengine-env.sh heal`，依赖本地隔离环境管理，**不能仅替换 URI 照跑云/多机集群**。远程业务端/负载端与故障脚本需独立编排。 | RTT、故障注入、每笔确定/未知/未生效分类、`.verified`全量核验并单独看 Errors/Dropped、尾分位与恢复；24h 另 C-S5，不能用 15m 替代。 |

将 CW Robot 放到另一机器的现有入口（在**已经部署同版本分区业务工程的生成目录**执行；这些 host/prefix 是必须填写的占位符）：

```sh
GOWORK=off go run ./cmd/loadtest -scenario cellworld -count 1000 \
  -endpoint host:7000 -account-nats nats://host:4222 -nats-prefix project \
  -cellworld-duration 15m -cellworld-active 5 -ramp-step 20 -ramp-interval 100ms \
  -max-p95 1020 -report cellworld.json
```

此原文入口的 max-p95 是业务验收参数，不代表状态同步 50ms；要关闭 50ms 目标仍需 CW 说明的端到端状态应用测量。

External/E01～E28 现行使用方式（不重写该文结构）：

- E01～E05：Mirror/Remote/Sync/代理和真实网络。可复用`mirror-local.sh`、`test-remote-matrix.sh`、生成 game-demo/Robot；netem/iptables、远端客户端与代理拓扑仍人工补。Mirror 要求 0 违例、版本不回退；Sync 记录恢复/应用延迟而非仅 ACK。历史`sync-aoi -mode=...`示例需按当前开关核实后用，不能推导脚本本身远程部署。
- E06～E11、E13：多机 NATS/etcd/Redis/Mongo/owner 故障。Cluster 套件可以接地址；本机 fault 命令不能操作任意远程机器。需明确每台故障对象、恢复路径与最终 DAO/receipt/outbox/checkpoint 核验。Mongo 专用微基准`BenchmarkRealMongoStepThroughput`、`BenchmarkRealMongoStepLatencyBreakdown`、`BenchmarkRealMongoCommitWriteConcern`仍存在于`framework/saga/mongo_step_latency_real_mongo_integration_test.go`（E12 可复用），旧 55TPS 接受值是特定步骤口径，不是正式整体 Saga 容量。
- E14：本机 Mongo 提交后发布前 SIGKILL 已有历史通过，只保留其范围；不要列作新的“未运行”任务。
- E15～E20：Mirror 目标负载/长稳、Remote24h、Linuxfsync、物理断电、MMO 目标负载。需独立硬件及负载；E16 负载目标须维护者给，E19 涉及物理卷/root 权限故障，本文不提供会重启主机的可执行命令。SIGKILL 不等于断电。
- E21～E24、E26～E28：systemd/k8s/distroless/生产 doctor/Linux 信号与磁盘/hotcode/准生产窗口频率。使用**当前生成工程**的部署物，不能把旧文 10 服务数或仓内目录当现行固定清单；先提供真实部署配置/工具链/流量才能验。生成/编译成功不代表部署完成。
- E25 服务端 Windows 为维护者排除项；C-U4 原生客户端 Windows 不受此排除影响。

## 5. >=1h 长稳（C-S1～C-S5）

| C / No. / K 定位 | 现有入口与缺口 | 关闭证据 |
| --- | --- | --- |
| C-S1 / 12① / `Remote：遗留`“路由登记的长时运行与大联盟” | R 仅给`ROOST_REMOTE_DURATION=1h`不足以选中联盟路由登记。当前`remote.sh`调用生成 remoteflow 链；alliance 的`TestAllianceE2EShortPerf`确实取 route_renew_total，但窗口固定 30s，MirrorRevalidation 固定 60s；本次没有取得可直接 1h 跑“登记+守卫加载量”的正式入口。**需把已定位路由场景延长并记录守卫加载/引用/登记量**，不能拿通用 Remote1h 结果代替。 | >=1h 同进程，route 登记/续期/失效/守卫加载与内存、goroutine、回收计数、失败分类随时间；缺省 TTL 和真实访问形状明确，不能以稳定吞吐推出无增长。 |
| C-S2 / 52 / `2026-11-12：活动模块的驱动与验收边界` | P8 已合入：`ACTIVITY_REAL_OUT=/absolute/new-evidence bash scripts/test-activity-real.sh`是真实迁移/SIGKILL/恢复扫描入口；本机一期 60s 活动通过，脚本测试期限固定 8m，没有小时级时长开关。不能用现成单次脚本直接关闭>=1h 全服/联盟宿主扫描。仍需宿主数/活动阶段/冻结期限/扫描页与并发/迟到条件及持续运行装配。 | 持续>=1h 的实际阶段/冻结/奖励义务/重试对账、扫描积压和最老年龄、CPU/内存/守卫数；128 一页/8 并发/每宿主 32 步是上界契约，不是准点或容量通过。 |
| C-S3 / 59 / `框架服务与 game-demo：未测部分` 活动结算榜 | 现有 Redis/ZADD GT 链是功能部件，**仍需 24h 结算榜业务负载、失败恢复和对账采样入口**；不把 Cluster 套件当 24h 运行。 | 24h 输入/最终 max 分数核对、Redis 错误与丢更新/业务重试策略、键数量/内存、恢复；“只升不降/无自动对账”是设计边界，不因观察期无错删除。 |
| C-S4 / 82 / `LevelComponent 与 UGC 物理闯关：遗留` | **可用同一测试二进制 `-test.count=12` 每次 5min**，命令与限制见下一小节。覆盖同一 OS 进程内反复整个 Service/EntityHost/8 房间建销>=约 60min；不改生产 TTL。旧 run.sh 不透传 count、timeout 只 min+5，不能直接拿它跑 12 轮。 | 同 PID12 次完整 PASS、每轮连续 tick/无客户端失败，完整原始-v 日志按 unix_s 统一时间；heap/RSS/GC/原生内存跨轮趋势。**同一长寿 Service 内反复房间建销、Gate/NATS/TCP、按房间拆分原生堆仍未验**。 |
| C-S5 / 100 / `能力与验证边界` Remote 容量 | 在脚本能安全管理的**专用隔离 Linux 依赖环境**，`ROOST_REMOTE_LABEL=linux-remote24h-unique ROOST_REMOTE_DURATION=24h ROOST_REMOTE_TIMEOUT=25h ROOST_REMOTE_RATE=80 ROOST_REMOTE_HEAP_PROFILE_MINUTES=10,30,60 bash scripts/perf/remote.sh`（后续小时 profile 列表自行明确）。脚本有 heal 操作，不能指生产或不受其管理的远端集群。 | `.verified`且 Errors/Dropped/ProjectionFailures/FatalProjectionConflicts 为 0；全量终值/版本/outbox 核验、每小时堆最低值无无界增长、pprof 归属/goroutine/tracker 有界、实际持续速率与尾分位。退出 3 仅一致性已核对但负载有错，不能记全过。 |

### C-S4 同一进程12×5min：源码核对结论与可运行入口

这是**准备好的命令，不是已实跑结果**。使用已经由合适平台编出的`wiring/physicsroom`测试二进制，工作目录设为仓库`wiring/physicsroom`。以下两个路径变量需预先指向本次私有新位置；不在本次创建二进制、不运行命令：

```sh
# PHYSICS_SOAK_BINARY=/absolute/already-built/physicsroom.test
# PHYSICS_SOAK_OUT=/absolute/new-evidence
# 目录由执行者提前准备；二进制必须是同一个文件，整个命令只启动它一次。
ROOST_PHYSICS_SOAK_MINUTES=5 ROOST_PHYSICS_SOAK_INTERVAL=15 \
ROOST_PHYSICS_SOAK_TSV= \
  "$PHYSICS_SOAK_BINARY" -test.run '^TestPhysicsRoomSoak$' \
  -test.count=12 -test.v -test.timeout=75m \
  > "$PHYSICS_SOAK_OUT/test.log" 2>&1
```

核对的具体调用链：

1. `wiring/physicsroom/load_native_test.go` 的`TestPhysicsRoomSoak`每次执行设置 5min，`startRoomLoad(..., lifetimeSeconds=duration/second+120)`给房间 7min 寿命，未突破 1h 生产 TTL。Go 测试的`-count=12`在同一测试进程重复调用测试；每轮会日志打印`SOAK pid=<pid> rooms=8 clients=160`，运行后必须检查 12 个 PID 相同。不是 shell 重启 12 个进程。
2. `startRoomLoad`注册`t.Cleanup`，后进先出：先取消 160 个逻辑客户端并`clients.Wait()`；再`rooms.Shutdown(context.Background())`；再`host.Close(context.Background())`。Service.Shutdown 封准入、cancel 每房、等每个`r.done`。
3. `service/physicsroom/service.go`的`run`延迟清理先`r.world.Close()`，然后 Closed 回调、删除房间/记 retired、最后 close(done)；`wiring/physicsroom/entity.go`的 EntityHost.Close 再等 Nest Shutdown。`infra/physics/physx/native.go`的 World.Close 在互斥锁内调用`rp_destroy`并置 nil；`native/bridge.cpp`释放 joints、actors、scene、material、dispatcher、Physics，最后一个 world 将共享 foundation 释放。因此下一轮开始前经过资源关闭路径。Cleanup 没有额外断言 Shutdown 返回值/销毁后资源计数，仍靠实际日志/趋势识别异常，不能只读源码就宣布无泄漏。
4. `ROOST_PHYSICS_SOAK_TSV`若设为同一路径，每轮`os.Create`会截断，最终只剩第 12 轮；故上方显式空值，只保留一次重定向的完整`-test.v`日志。每轮 SOAK 行有`unix_s`、本轮`elapsed_s`、RSS、heap、全进程累计分配/GC/goroutine、物理/交付统计。日志含重复表头，提取时需跳过表头、加 round 编号；按首次窗口的`unix_s-elapsed_s`确定全进程起点，再算所有窗口的全程 elapsed。
5. 旧`docs/performance/benchmarks/physicsroom-soak-20261029/summ.py`假定单轮连续 elapsed，**不能直接拼 12 轮重置为 0～300 的 elapsed 来拟合**。同 PIDruntime 指标与 unix_s 可关联全程；要准备日志提取/归一化分析，分别比较各轮相同相位的最低值与整体趋势，保留轮次启动/关闭间隙。每轮仅 5min，不应把默认 warmup=300 套在每轮后导致没有稳定样本。
6. macOS 可在该唯一 PID 生命周期内单独运行既有`sample.py <pid> proc.tsv ...`，它采 footprint/vmmap；目标 PID 退出才结束。Linux 没有这些工具，需 Linux 原生分配器/进程映射采样；夹具 ps RSS 本身可读 Linux。RSS/Go heap 不等于 PhysX 原生泄漏精确归因。
7. 范围仍窄于原完整 C-S4：每轮重建整个 Service，retiredRoom 表也被丢弃，不能证明**同一个长寿 Service**里持续建销的 retired 缓存/TTL 回收；没有真实 Relay 的 Gate/NATS/TCP 链，也没有按房间分割原生堆。若同进程 12 轮实跑通过，只关闭“同 OS 进程累计一小时、整个 Service 反复建销”的验证子项，其余保留。

## 6. 云 / 托管 Mongo（C-M1～C-M2）

| C / No. / K 定位 | 当前可执行入口 | 关闭证据 |
| --- | --- | --- |
| C-M1 / 17.1 / `Saga：遗留` 云 Mongo 三档单样本 | 凭据由执行环境安全注入`ROOST_SCAN_MONGO_URI`，不在命令、报告或日志展开。`ROOST_PERF_LABEL=cloud-saga-unique ROOST_SCAN_PLAN='native-forward:64 native-forward:128 native-forward:128@strict64' bash scripts/perf/saga-js-scan.sh`，各档至少 3 样本（不同 label 交替）；可显式设置采样秒数。脚本支持外部 Mongo，但仍本机 NATS/测试角色且强制 GOMAXPROCS4。 | 实际云主机/托管实例规格、RTT、majority+j:true/事务、3 档余额与错误/unknown/逐秒积压/排空时间。`ROOST_SAGA_SCAN_DRAIN_SECONDS`可为观察放宽但**不能把超过设计 30s 写成达标**；Q6 保留。不是 C-X2 跨机参与方完成。 |
| C-M2 / 18 / `Saga：遗留` getParameter | 安全预设`ROOST_MONGO_URI`后 `go run ./cmd/mongocheck`；支持`-uri-env <变量名>`、`-timeout 15s`。源码确认只发 hello/buildInfo/getParameter/serverStatus，不接受 URI 命令行参数。 | 托管实例上实测 getParameter 允许/拒绝及参数值或明确未探测、权限/服务规格、输出与退出码。退出 0 可以带 WARN，**不能凭退出 0 认定 getParameter 成功**；拒绝时说明探针降级与部署验证办法。没有托管实例本项继续未验证。 |

## 7. Unity / 原生设备（C-U1～C-U4）

| C / No. / K 定位 | 当前入口 / 尚需设备步骤 | 关闭证据 |
| --- | --- | --- |
| C-U1 / 31②、85 / `Lockstep：遗留`、`客户端：遗留` | `ROOST_REQUIRE_DOTNET=1 bash scripts/test-client-dotnet.sh`只作算法/托管回归基线。Unity 构建包`python3 scripts/client/build-unity.py --target <目标> --output <新目录> ...`及`unity-project.py`/README 的`UnityAcceptanceBuild.Run`现存；**还需把 SHA256/Create+restore 与 RoostSceneTime 实际场景装入目标 Editor/IL2CPP**，不编造一个已覆盖新 SceneTime 的 Unity 自动命令。 | 真实引擎/架构/运行时下 snapshot 校验/restore 后帧一致，场景倍率/时停/重连时钟单调，主线程回调/生命周期；保存 Player/Editor 日志或设备录屏与断言。现有.NET TestUnitySceneTime 不替代。 |
| C-U2 / 73④、74、84 / `地形 / 无缝世界：遗留`、`客户端：遗留` | 当前`.NET`项目`client/dotnet/Roost.Client.Terrain.Tests/Roost.Client.Terrain.Tests.csproj`及`Fixtures/sync_fixture_test.go`真实帧可作为金样；`test-client-dotnet.sh`已补，但**没有凭现成通用 Native Session 便可关闭地形 Unity/IL2CPP 的命令**。需引擎侧接 EntityViews→TerrainChunkSync 和地图表现。 | 真实 Full/Delta/Remove/版本确认顺序、块到达后再确认、岸线/重建与 mesh 表现、IL2CPP 裁剪/数值、断线重连。坏块 N4 策略未答，不能用测通正常块替代。 |
| C-U3 / 80、81 / `LevelComponent 与 UGC 物理闯关：遗留` | `scripts/physics/create-viewer.sh /absolute/new-project`建工程；`scripts/physics/build-native.sh`目标库；Phys 文档本机三进程+UGC 关卡；在 Unity 打开`Room.unity`。`run-viewer-server.sh`是独立数值对照宿主，不等于正式 Gate 房间入口。分频/机关/抓取/预测等需实际操作或专门引擎断言。 | 实际 Editor/Player 中新交互、动态 Box/平台/马达/抓取、稳定关卡版本、分频与预测/纠偏；注明轨迹偏差允许口径，连通不能当数值一致。WAN/容量仍 C-X7。 |
| C-U4 / 83、86、88 / `客户端：遗留` | C++：`bash scripts/test-client-cpp.sh`、`ROOST_PROTOBUF_TESTS=ON ROOST_PROTOBUF_PREFIX=<目标runtime> ...`、`ROOST_GATE_CPP=1 bash scripts/test-gate-client.sh`可作目标宿主功能；构建/打包按 client/cpp 及 unity README，Windows MSVC、Linux 目标库、Android NDK、iOS Xcode 各自环境。**UE/Godot 完整接入、移动前后台、WAN、跨 Gate Unity/IL2CPP/Unreal 均需真实业务引擎工程和设备验收**。 | 每平台编译+运行独立记录，PB/DAO/Sync/Lockstep/重连、线程/销毁、相同 Session 旧握手、前后台重新取票、设备内存/可见集容量、WAN 故障。macOS Editor/Mono/IL2CPP 历史已跑与新 B14 C++真实 NATS 都保留，不扩成全平台通过；TLS/WS/UDP/KCP 未实现是能力缺口，不是设备测试可关闭。 |

## 8. 执行与状态规则

全部 C 仍待目标环境验证。各组的命令、真实拓扑、关闭判据与不覆盖项必须一起记录；脚本退出 0 不能替代正确性 / 错误分类 / 容量判据。没有既定数值阈值的项目先记录实测，不临时发明通过线。

使用独立环境和唯一命名空间，up / env / 用例 / down 在同一受控生命周期里，只停自己的进程；不 reset 共享数据库，不清理保留数据与缓存。首次执行前重新检查脚本有效参数与本机端口，外部环境中的凭据不展开到日志。

本清单不决定 Q4、Q6、Q13、#154、CellWorld B / C / E 或 N1～N4；测试不能替代维护者选择。C-S4 的 count=12 入口即使通过，也只关闭同进程整套 Service 反复建销的子范围。
