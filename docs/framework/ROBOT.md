# Robot：功能验收与压测

框架使用者沿用 `robot/`，不复制 demo 的连接实现，也不把直接调用 Nest 的夹具当作客户端压测。分层保持 transport → session → action/scenario → runner → loadtest。`game-demo/cmd/loadtest` 是完整应用入口；按玩法生成的工程保留自己的业务步骤、PB 和断言。

以下公共 Robot 接线自 v1.35.0 提供，CLI、夹具与 runtime 必须使用同一版本；不能把新生成物搭配 v1.34.2 或更早运行库。

## 生成协议与接入

`roost generate` 在存在业务协议时同时生成 `loadtest/protocol/registry_gen.go`。在 `protocol.NewRegistry(nil)` 上调用生成的 `RegisterDefaults`，使用正式 PB、消息 ID 和逐消息编解码器；不再引用工程中不存在的 Registry，也不需要 JSON 兜底。`action.RegisterCall` 可复用这些显式 codec。

没有 PB 定义就没有业务 Robot 协议表。生成工程通过 build 不代表已经接好网络；不能为了“每类都有 Robot”发一个无业务效果的 ping。玩法定义、鉴权身份与服务器端点先具备，再补请求、响应和最终数据断言。

`transport.PlayerTCPType`（`player-tcp`）同时服务嵌入 TCP 和独立 Gate，使用公共 RS v2。握手首包由 `transport.AuthPacket(token)` 构造，每次调用（每次连接、包括重连）带一个新的连接代号（`NextConnectionGeneration`，信封格式见 [client/README](../../client/README.md#鉴权首包的连接代号)），供 Game 在跨 Gate 时认出迟到完成的旧握手（[GATEWAY §3](GATEWAY.md#3-绑定与请求结果)），AuthProvider 不要缓存复用同一个包。握手、通知、请求共用递增线序列；推送按 FlagPush 区分，不能被相同序号的请求吞掉。完整写出后应答超时释放关联，迟到响应丢弃；发送阶段超时关闭连接，阻止半帧复用及阻塞写积累；序列耗尽或关联表达到 4096 时关闭连接，不无限增长。demo 的 playertcp 仅委托这个实现。

## 三种测试口径

| 用途 | 配置与断言 | 不能据此宣称 |
| --- | --- | --- |
| 功能 smoke | pool；固定人数与种子，完成登录、业务、Sync/Lockstep、重连与最终数据检查 | 没有数据断言的请求成功不等于业务正确 |
| 容量 | arrival-rate；固定场景到达率、显式 MaxConcurrent，逐档上升 | Rate 是新机器人/整段场景每秒，不是消息 TPS、Nest TPS 或 Remote TPS |
| 长稳 | looping 或分阶段负载；限时，记录资源、水位、错误与退出 | 短测不替代长稳；同一幂等回执的重复读取不是持续业务写入 |

Runner 的 looping 每次重建 Context/会话、重新执行整段场景；不是默认保持一次登录。要验证长连接稳定态，在单个场景中 connect 一次，再用 Loop 驱动业务步骤。停止会取消在途请求并等待机器人清理，取消单列，不把结果未知算成功。Runner 一次性使用，重新运行需创建新实例。

生成的 game-demo 可直接控制：

```sh
go run ./cmd/loadtest -count 20 -ramp-step 2 -ramp-interval 1s -report smoke.json
go run ./cmd/loadtest -executor arrival-rate -rate 10 -max-concurrent 100 -duration 1m -seed 42 -report capacity.json
```

已有账号/NATS/服务器参数保持不变。默认复合场景包含匹配等待、Saga 与战斗，不代表普通 Nest 消息负载；需要业务稳定态时用自己的 scenario。当前容量入口默认 `drop_rate=0`，压测端欠载不会被当成通过。

## 报告解释

- `scheduled`：应投递的机器人；`started`：实际启动；`dropped_capacity`：达到本机并发上限；`dropped_schedule`：调度错过的时间槽；`launch_failed`：身份分配失败。排空后 scheduled = started + 三项未启动计数。不会把过期时间槽集中补发成突发负载。
- `success/failure/canceled`：场景执行结果。looping 会重复场景，不能拿它们之和与机器人 started 比。`online/peak_online` 是机器人实例数。
- `error_rate` 分母为已完成场景；`drop_rate` 分母为 scheduled。取消、丢拍与失败分别查看。零样本的门禁失败。
- `quantiles_ms` 保留小数毫秒；门禁不再先截断到整数毫秒。场景耗时包含等待与多个请求。现有 `robot.session.call` 是逐消息统计，需要监控采集，不能把场景 p99 称为网络包或 Sync 延迟。
- `config/environment` 记录 executor、rate、并发、seed、ramp/stages、Go、GOMAXPROCS、构建版本；不包含票据和 transport headers。使用本地 replace 时另保留检出源码，不能只留一个版本标签。
- `robot_resources` 是 Robot 进程自身的 CPU 时间/单核百分比、进程生命期 RSS 高水位、每秒采样堆/goroutine 峰值。短于一秒的峰值可能漏采。macOS/Linux 有 CPU/RSS；其他平台标 unavailable。不是服务器监控，不能据此判断服务端磁盘或网卡是否饱和。

### 按运行的请求与流量

公共 Runner 自己持有 `session.Recorder` 和 TCP `Traffic`，同次运行的多个机器人及重连累计；新 Runner 不继承历史。`MessageReport()` / `TrafficReport()` 可直接读取；loadtest Manager 的 JSON/Markdown 报告自动带入，生成工程的 `cmd/robot` 也输出这些字段。自定义 Runner 没有实现报告接口时省略字段，不能用零值冒充测量结果。

- `requests.messages` 按请求消息 ID 统计 `started/sent/completed/ok/rejected/timeout/canceled/errors/inflight`。排空后 started = completed，completed = ok + rejected + timeout + canceled + errors。started 是实际进入 Call 的次数，不是计划发送数；sent 是发送阶段观察到完整写出成功的次数，不能推断服务端已经执行。
- `Call` 的 ok 只证明协议成功。业务应使用 `CallChecked`，校验器返回 `session.ErrRejected` 才计 rejected；其他响应校验错误计 errors。当前生成玩法已接此校验。拒绝计数只是业务回执分类，不能将未知提交结果当作确定未执行，也不能据此盲重试。
- 每个消息 ID 的 p50/p95/p99 包含所有完成结果，从进入 Call 到响应解码和业务校验结束；不是服务器排队耗时或 Sync 端到端延迟。固定指数桶给上界，邻桶约 9%，上限裁到真实 max；max 保留实际值。最多 256 个常规消息系列，更多 ID 汇总到 overflow；最多保留 32 条最慢样本，仅含消息 ID、结果和耗时，不存身份、票据或载荷。
- `request_ok_per_second` = ok 总数 / 整次运行秒数，包含登录、重连和其他场景等待；不是仅采样活跃阶段的峰值 TPS，也不是资产成功写入 TPS。
- `traffic.socket_read_bytes/socket_written_bytes` 在 TCP socket 实际读写返回时累计，包含 RS 包头、握手、迟到应答和部分写入；不含 TCP/IP 头及重传。`measured_connections` 表示接入计数的连接数。默认 TCP 和 player-tcp 已测量，WS、直接使用 KCP 或自定义传输未接此计数，不能把 0 当成没有流量。
- 返回的历史报告深拷贝可变字段；调用方整理表格不会改写保留证据。拒绝、取消和超时不混入成功吞吐。

服务端 CPU/内存/磁盘/网络需由部署侧监控单独采集；目前内置资源采样仍只覆盖 Robot 进程。`SyncProbe` 提供 Full/Delta/Remove 的端到端统计与最慢 32 个样本；服务端队列仍须由场景读取 Nest Stats，不由请求 RT 替代。

## 13 类玩法验收

```sh
python3 scripts/test-genres-generated.py --out /tmp/roost-genres-acceptance
```

输出目录必须新建。逐类生成独立工程，执行现有离线玩法断言，再换自定义 module、重新生成、build/vet/ID/漂移检查，并重新执行原先通过的场景（race，零意外 skip）。工程、原始日志、JSON 汇总保留；失败继续下一类，最终退出非零。不隐式打开 REAL，不打印服务凭据。

回合 PvP、合作 Roguelike、异步社交经营生成 `cmd/robot`、业务 PB 和正式 TCP → Sender 接线。当前 Robot 已从首动作扩到对局/经营闭环；九类既有状态玩法使用 `loadtest.Commands`。鉴权和提交器仍是测试替身；客户端动作经 TCP，可信定时推进和敌方行动由服务器测试驱动，不提供客户端直接结算接口。

| 玩法 | 实际业务步骤 |
| --- | --- |
| SLG | 建造、研究、服务器完成建造/行军、重试不重复扣费 |
| MMORPG | 双方转账、战斗击杀、拾取和任务奖励只结算一次 |
| 卡牌 RPG | 抽卡、开战、服务器敌方回合、胜利领奖及重连重试 |
| 塔防 | 四人建塔/准备、服务器推进三波、全员领奖及重试 |
| 生存撤离 | 移动采集、返回撤离、服务器封存清单、兑付及重试 |
| 合成 | 移动、合成、出售及重试；这是经济循环，不是整局胜负 |
| 放置 | 双工坊生产、服务器推进时间、收取、订单结算及重试 |
| 自走棋 | 买卖、服务器推进终局、八席按排名领奖及重试 |
| 派对房间 | 抢星、期限结算、双方领奖及重试 |
| 回合 PvP | 双人交替攻击至终态、终态/非成员拒绝、重连重试 |
| 异步社交经营 | 种植、好友帮助、提前/越权收获拒绝、到期收获及重试 |

网络回包不替代实体锁：测试读取 DAO 也通过 Nest 本地执行并取锁；业务时钟用原子值推进。测试数量保持不变，扩展已有场景的断言。

玩家身份仅取鉴权 Session，不接受客户端指定操作者；业务时间、卡池规则和奖励不由请求控制。响应 Code=0/1 只区分业务成功/拒绝，不代表详细错误分类或未知结果重试策略已经完整。TCP 重连测试沿用既有业务 epoch；真实登录引发 epoch 变化时，需要先恢复视图再构造命令。拒绝用例使用不存在玩家，不等于已穷尽所有成员越权组合。

九类的手工生成应在正式 `roost generate` 前调用 `scripts/testdata/robot-common/install.py <repo> <project> <genre>`；同检出的脚本已包含此步骤。`cmd/robot -steps <file> -endpoint <addr>` 必须传实际业务步骤，票据用 `ROOST_ROBOT_TICKET` 环境变量。配置文件是 JSON，线上业务载荷仍为生成 PB；具体字段见工程 `loadtest/README.md`、PB 定义与网络测试。宿主须装配 Dispatcher、真实鉴权与实体初始化；骨架并不会自动成为一个可玩的服务器。

MOBA 生成 `loadtest.NewBattleBot` / `BattleSink`，复用公共 `robot.LockstepBot`。真实 KCP 用例有两名客户端、连续丢弃六帧广播和可靠追帧。攻塔输入只读客户端自己的模拟状态；运行至基地摧毁终态，两个客户端与服务器参考模拟摘要一致。服务器据终态调用正式 Nest 幂等结算两次，再经可靠通道 RS/PB 向每个客户端发送自己的金币/胜负次数，客户端确认胜者只获得一次 10 金币及一胜，败者只增加一负。输入按外部节拍发送，追历史帧时不补发过期输入；固定会话与密钥仅供测试。Lockstep 是帧协议，不强塞业务 PB；服务端初始化、结算和奖励保持可信端权限。

## 完整业务样板

合作 Roguelike 的第 10 条用例使用两名 TCP Robot 完成三次双人投票、三关结束、各自领奖、相同请求重试、重新鉴权连接后重试、再用新请求重复领奖拒绝。客户端通过新增 `CoopView` PB 查询核对 Stage=3、Revision=8、双方各增加 10 金币。视图由正式 Nest 同时读取 Room/Player，先检查成员身份，只公开本人余额与下一命令序号，不泄漏随机种子或其他成员回执。另以**存在但不在名单**的玩家验证查询和领奖都被拒绝。

生成的合作工程默认 `cmd/robot` 跑整局：

```sh
# 两份票据来自业务登录，房间已由宿主分配，双方都在名单内。
# 环境变量：ROOST_ROBOT_TICKET、ROOST_ROBOT_PEER_TICKET
go run ./cmd/robot -endpoint 127.0.0.1:7000 -room <新房间ID> -timeout 30s
```

宿主装配 `scenarionet.CoopDispatch`，仍需自己的账号/房间路由。每次使用新的房间和对应身份；不要复用已结算存档刷吞吐。`loadtest.Coop` 的一个场景对应两个客户端，不能把场景数称为客户端人数。两端关闭连接后退出，未增加销毁房间协议。

MOBA 的完整链由 `TestMOBAKCPRobotMatchSettlementAndClientResult` 验证；生成业务有 `BattleResult` PB、可信 Nest 结算和本人资产视图。KCP 绑定仍是测试凭据，不是正式账号登录；没有新增客户端可自报胜负或奖励的 API。

这两个样板证明既有玩法规则的整局和客户端结果闭环，**不证明账号服务、动态匹配、完整业务重登、EntitySync 或真实 WAL/Mongo 已串成生产链**。合作使用 PB 查询视图，不冒充 EntitySync 推送验收；MOBA 不通过 session.Call 传帧，不能把 TCP 请求报告当作其 KCP 流量统计。

game-demo 的 `turn` 场景使用正式 account 登录与 matchmaking/matchroom 开房，两个机器人各控制一个真实连接，完成四回合、每步同序号重试、断线重登与 Room Epoch 接管、终态回执重放。`make dev-smoke` 将它放在 demo 与 alliance_offline_join 之后，各跑 2 人；`scripts/test-demo-turn.sh` 实跑这一目标并追加独立 Gate 的 turn。正式配方的 SIGKILL 恢复与 GameRoom 纵切的持久 timer 恢复分别见 [回合 PvP 真实档](SESSION-GENRES.md#回合-pvp-真实档)，不从这份 Robot 记录推出跨机接管或容量结论。

## 上线补齐顺序

1. 13 类玩法的网络业务闭环已补齐；各玩法的独立真实账号重登、Sync 与持久存储组合仍要分别验收，不能从通用 game-demo 推导。
2. Robot 会话统计按运行聚合：请求/字节/超时/拒绝、排队与端到端耗时、慢样本；明确 reconnect 与未知结果，容量和长稳都断言最终状态。
3. 用真实账号、Gate、Nest、WAL/Mongo、Remote/Saga 的生成工程执行恢复与混合负载，分别记录 Sync 全量/增量/删除与 Lockstep。少量 Saga 的业务占比不改变普通消息/heartbeat 的验证权重。
4. 在目标 Linux/部署规模验证磁盘耗尽、依赖断连/主从切换、停机排空、权限隔离、备份恢复，以及原生客户端各平台与引擎集成。当前本机功能结果不能作为这些项的放行证据。

不要求兼容旧协议、旧 DAO 或旧 WAL；仍须保护需要保留的数据，不自动清理用户数据库。

## 已确认的缺陷与回归

| 问题 | 修复与证据 |
| --- | --- |
| 第 2 个身份分配失败时，第 1 个机器人仍等待 context，Run 先 Wait、defer cancel 永远到不了 | 启动错误先取消再排空。`TestLaunchErrorCancelsAlreadyRunningRobots` 修前失败，修后通过 |
| 900µs 的 p99 截断为 0ms，通过 500µs 门禁 | 分位数报告保留小数。`TestSubMillisecondThresholdIsNotRoundedDown` 修前 actual=0 且 finished，修后 actual=0.0009 秒且 failed |
| 写阻塞超时返回后连接未关，可继续积累后台写 | 发送阶段取消关闭会话。`TestCallReturnsWhenTheWriteBlocksPastItsContext` 的关闭断言修前失败、修后通过；应答超时的迟到包另有回归 |
| Robot 协议表引用目标包中不存在的 Registry/私有 helper；已有显式 codec 仍要求兜底 codec | 生成表直接引用公共 Robot；EnsureEncoder/Decoder 先复用已注册方向。三类生成工程真实 PB TCP 用例与 `TestExplicitCodecsNeedNoFallback` 验证 |
| arrival-rate 不限制同时在途数量，Ticker 丢拍未记录；Stop 可能在调度器仍 Add 时直接 Wait | 启动前保留名额，按绝对时间记欠载；Stop 等 Run 完成信号。容量/计数/清理顺序有定向 race 回归 |

验证命令：`go test -race ./robot/...`、`go vet ./robot/...`、`go test ./...`、`go test ./codegen/internal/roost -run '^TestDemoTemplateGeneratesABuildableWritePath$'`，以及上面的 13 类生成矩阵。原始日志留在执行时指定的输出目录；仓内记录具体通过范围，不将编译、替身和网络验收混为一项。

## 当前验证结果（2026-11-01）

macOS arm64、Go 1.27.3，基线 `ffdc7b2d` 加本轮未提交工作树：13 类独立工程生成、build/vet、ID/漂移检查通过；改成 `example.com/acceptance/<genre>` 后实际执行下表全部场景，零测试用例 SKIP。

| 玩法 | 自定义 module 实际 PASS |
| --- | ---: |
| slg | 9 |
| moba | 28 |
| mmorpg | 14 |
| card-rpg | 17 |
| tower-defense | 11 |
| survival-extraction | 13 |
| merge-casual | 16 |
| idle-sim | 13 |
| auto-chess | 13 |
| party-room | 19 |
| turn-pvp | 10 |
| coop-roguelike | 10 |
| social-sim | 9 |

SLG 的 9 条含原 6 条、两条联盟断言与新增 Robot。合计 182 条自定义 module 场景；九类新增 TCP Robot，MOBA 原 KCP 用例扩到公共 LockstepBot 整局及客户端结算结果，回合 PvP 10 条（含房间持久定时器自动超时）、社交经营 9 条，合作 10 条包含整局 TCP Robot。全部为上述明确范围，不代表完整生产端到端认证。

最终代码通过 `go test ./...`、`go vet ./...`、`go test -race ./robot/...` 及文档/真实依赖入口门禁。13 类完整生成矩阵 `/tmp/roost-closed-loop-delivery-20261101` 全部通过：181 条自定义 module 场景、零测试 SKIP，生成漂移/ID/build/vet/race 检查通过。使用本文命令可重新生成，不依赖临时目录作长期证明。本轮没有 Linux 实跑；2026-11-02 的 [Linux 验收续报](../maintenance/LINUX-FUNCTIONAL-20261102.md)在 v1.37.1 上跑通同一 13 类矩阵（自定义 module 181 PASS、零 SKIP）。

本轮初始失败保留在 `/tmp/roost-closed-loop-20261101` 与 `/tmp/roost-closed-loop-final-20261101`：网络回包后测试直接读 DAO 没有锁，MMORPG 测试同时读写 time.Time。现在由公共快照 helper 在 Nest 本地取锁复制，解析与断言留在测试 goroutine；网络时钟原子推进。修后完整矩阵通过，没有改变业务规则或减少场景。此前版本的验证过程见 Git 历史与 [v1.35.0 说明](../release/v1.35.0-NOTES.md)，本文只保留当前结果。

2026-11-01 补充：生成 game-demo 的 C# / C++ 客户端已完成真实 account RPC/Redis → Gate/NATS → PB/Nest → WAL/Mongo → EntitySync 联合验收，并核对会话重建、事务重放与 DAO 冷水合。入口与限制见 [Gate](GATEWAY.md#真实账号gate-与存储联合验收)。这是通用生成链路的功能证据，不把其结论扩张到上述 13 类玩法的独立真实存储或 Robot 容量。

## Sync 观测和真实 Gate 混合负载

本节 SyncProbe、gate-mixed 及上文新增闭环自 v1.36.0 起提供（v1.35.0 没有 SyncProbe）；必须使用同一检出的工具、夹具和运行库。

每次运行创建独立 `robot.NewSyncProbe(maxPending)`，用 `runner.WithSyncProbe(probe)` 装配。业务从 `robot.Context.Config.SyncProbe` 获取同一实例（具体字段见代码）；直接驱动 Session 的夹具也可以独立使用。请求发起前 `Expect(SyncKey, time.Now(), timeout)`，成功应用并核对业务状态后 `Observe(key, appliedAt)`；断连时 `CancelConnection`。关联号由场景分配，Connection 在整个运行内唯一且重连换代，不能用每个机器人自己的 1、2 与别人碰撞。多个 setter 或 handler 可以合并成一帧，不能把版本差当作丢包。

起终点必须在同一时钟域。报告按 full/delta/remove 给出 expected、received、timed_out、canceled、pending、P50/P95/P99/max；分位数为固定直方图桶上界（约 9% 粒度，最高不超过实测 max），不是保留全部样本的精确排序。空样本不是通过。控制面 JSON/Markdown 保存独立副本；待确认容量有上限，超过计 capacity_rejected，重复/迟到计 unmatched。

`scripts/perf/gate-mixed.sh` 使用调用方已经启动的隔离 Mongo 副本集、Redis、NATS，默认 30 秒，最长 120 秒；不启动或停止共享服务。按 `ROOST_DATAENGINE_IT_*` 注入地址，`ROOST_PERF_CPU=0` 不限定 GOMAXPROCS。真实 account/Redis/NATS → TCP Gate → PB/Nest → strict WAL/Mongo → PB EntitySync，叠加正式 10Hz heartbeat、5% 心跳修改。当前只有一个账号和一个共享 World，`ROOST_GATE_LOAD_RATE` 默认每秒 100 次请求，可显式选择对照档，是持久化热点诊断，不代表 MMO 容量。记录丢拍而不隐去欠载；最终额外一笔排空事务不计测量 TPS。

报告保存 Sync 三类延迟、请求/字节、Nest 排队累计/最大值、WAL/投影排空与进程资源。客户端与服务端在同一进程，CPU/RSS 不能拆成独立服务器值；不包含 Mongo/NATS/Redis 的进程资源。关闭后重复投影及 DAO 冷水合仍不是进程 SIGKILL 恢复。`verified` 只表示数据一致性断言通过，不替代负载与 P99 门禁。

旧 `sync-business.sh` 基准修前在编译阶段失败：`businessTCP` 缺少 `CloseSessions`、调用已删除 `Scene.Moved`；同一源码还保留 BSON 解码，不能消费当前 PB DAO。修复为现行 pusher 契约、Nest Guard 内 `QueueMove` 和生成 `clientsync.DecodeFields`。2 tick solo 冒烟已通过（`artifacts/perf/sync/pb-decoder-fixed-20261101`），修前证据在 `pb-decoder-before-20261101`；短测只证明入口恢复可运行，不构成容量结论。

2026-11-01 真实恢复补测：`ROOST_MMORPG_REAL=1 ROOST_MMORPG_KEEP=1 bash scripts/test-mmorpg-generated.sh` 通过正式生成消费工程及三个进程的持久化/恢复；SIGKILL 时存在 16 条未投影 strict WAL，交易托管、战斗奖励、Buff/复活定时器冷加载后恢复，第三进程重试无重复奖励/退款。日志 `/tmp/roost-mmorpg-real-final-20261101.log`，生成工程保留在测试输出路径。它验证 MMORPG 的持久状态与驱动，不冒充 Gate/账号/游戏三服务联合崩溃恢复。
