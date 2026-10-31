# Robot：功能验收与压测

框架使用者沿用 `robot/`，不复制 demo 的连接实现，也不把直接调用 Nest 的夹具当作客户端压测。分层保持 transport → session → action/scenario → runner → loadtest。`game-demo/cmd/loadtest` 是完整应用入口；按玩法生成的工程保留自己的业务步骤、PB 和断言。

以下公共 Robot 接线自 v1.35.0 提供，CLI、夹具与 runtime 必须使用同一版本；不能把新生成物搭配 v1.34.2 或更早运行库。

## 生成协议与接入

`roost generate` 在存在业务协议时同时生成 `loadtest/protocol/registry_gen.go`。在 `protocol.NewRegistry(nil)` 上调用生成的 `RegisterDefaults`，使用正式 PB、消息 ID 和逐消息编解码器；不再引用工程中不存在的 Registry，也不需要 JSON 兜底。`action.RegisterCall` 可复用这些显式 codec。

没有 PB 定义就没有业务 Robot 协议表。生成工程通过 build 不代表已经接好网络；不能为了“每类都有 Robot”发一个无业务效果的 ping。玩法定义、鉴权身份与服务器端点先具备，再补请求、响应和最终数据断言。

`transport.PlayerTCPType`（`player-tcp`）同时服务嵌入 TCP 和独立 Gate，使用公共 RS v2。握手、通知、请求共用递增线序列；推送按 FlagPush 区分，不能被相同序号的请求吞掉。完整写出后应答超时释放关联，迟到响应丢弃；发送阶段超时关闭连接，阻止半帧复用及阻塞写积累；序列耗尽或关联表达到 4096 时关闭连接，不无限增长。demo 的 playertcp 仅委托这个实现。

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

服务端 CPU/内存/磁盘/网络需由部署侧监控单独采集；目前内置资源采样仍只覆盖 Robot 进程。Sync Full/Delta/Remove 的端到端延迟与尾样本、服务端队列延迟、消息计划调度欠载仍需业务压测接线，不由请求 RT 替代。

## 13 类玩法验收

```sh
python3 scripts/test-genres-generated.py --out /tmp/roost-genres-acceptance
```

输出目录必须新建。逐类生成独立工程，执行现有离线玩法断言，再换自定义 module、重新生成、build/vet/ID/漂移检查，并重新执行原先通过的场景（race，零意外 skip）。工程、原始日志、JSON 汇总保留；失败继续下一类，最终退出非零。不隐式打开 REAL，不打印服务凭据。

回合 PvP、合作 Roguelike、异步社交经营额外生成 `cmd/robot`、业务 PB、`loadtest.Smoke` 和正式 TCP → Sender 接线，每类第 9 个用例验证首个动作/同连接重试/重连重试。鉴权器与存储仍为测试替身；Gate、EntitySync、完整对局、实际容量未由该用例证明，见 [三类玩法](SESSION-GENRES.md)。九类既有状态玩法新增 `protocol/def/robot.go`、`game/robotaccess.Dispatch`、`loadtest.Commands`、`cmd/robot` 和使用说明。每类新用例验证两个有效动作、同连接重试、TCP 重连重试、非法命令与不存在玩家拒绝，并检查最终 DAO：

| 玩法 | 实际业务步骤 |
| --- | --- |
| SLG | 建造与研究；余额与持久定时器 |
| MMORPG | 两笔转账；双方余额与操作序号 |
| 卡牌 RPG | 十连抽再单抽；宝石与抽卡回执 |
| 塔防 | 建造再升级；归属与等级 |
| 生存撤离 | 移动再采集；位置与背包 |
| 合成 | 移动再合成；格子与等级 |
| 放置 | 两个工坊入队；任务与回执 |
| 自走棋 | 购买再出售；商店与单位 |
| 派对房间 | 同轮两次抢星；第二次不重复得分 |

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

## 上线补齐顺序

1. 将其余业务 smoke 按合作 Roguelike 与 MOBA 样板扩到完整对局和结算；补 Sync、真实账号重登与权限组合。统一使用当前 Robot，不新增第二套工具。
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

## 当前验证结果（2026-10-31）

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
| turn-pvp | 9 |
| coop-roguelike | 10 |
| social-sim | 9 |

SLG 的 9 条含原 6 条、两条联盟断言与新增 Robot。合计 181 条自定义 module 场景；九类新增 TCP Robot，MOBA 原 KCP 用例扩到公共 LockstepBot 整局及客户端结算结果，回合 PvP/社交经营各 9 条，合作 10 条包含整局 TCP Robot。全部为上述明确范围，不代表完整生产端到端认证。

前一轮全仓 `go test ./...` 通过，之后新增发送超时关闭与调度尾段计数经过 `go test -race ./robot/...`；生成 demo 专项、根包门禁、Robot vet 通过。Linux amd64 `go build ./robot/...` 通过，仅为交叉编译，未在 Linux 运行。

证据目录为 `/tmp/roost-genres-acceptance-20261031` 与派对房间补跑 `/tmp/roost-genres-party-confirm-20261031`。前者保留派对房间脚本在执行期间被编辑而导致的 shell 命令截断失败（业务 18 条通过，随后 `/...: No such file or directory`）；固定脚本后单独完整补跑通过，未删除失败记录。更早矩阵自身曾误把无测试文件包的 JSON `skip` 当成测试 SKIP，已改为只检查带 `Test` 的事件，同时仍校验全部预期用例实际 PASS；没有放宽业务断言。原始日志失效时按上方命令重跑，不依赖这台机器的临时目录作长期证明。

本轮后续验证原始目录：`/tmp/roost-robot-final-20261101`。早期失败仍留在 `/tmp/roost-robot-five-20261101`（塔防升级请求漏传期望等级，修正测试 B=1）和 `/tmp/roost-robot-four-20261101`（MMORPG 的 -run 漏选 Robot，补全过滤）。两者均未修改业务规则或放宽通过数，修正后单独复测目录为 `/tmp/roost-robot-recheck-20261101`。后续复跑不依赖临时路径，用本篇生成矩阵命令即可。

完整样板与请求报告这轮：`go test -race ./robot/...`、`go vet ./robot/...`、`go test -count=1 .` 以及 13 类矩阵通过，完整生成证据在 `/tmp/roost-robot-lifecycle-final-20261101`。新增运行隔离/重连/socket 字节、拒绝/取消/超时和报告深拷贝回归。报告别名回归修前出现 `returned report mutated retained evidence`，修后通过；日志 `/tmp/roost-report-alias-before.log` / `after.log`。早期生成失败（handler 包内结果类型导致 Sender 循环依赖、缺少生成包显式 import 别名）保留在 `/tmp/roost-coop-full-20261101`、`/tmp/roost-full-recheck-20261101`，已将 View 放进业务规则包并修正引用。未放宽门禁或修改玩法规则。

2026-11-01 补充：生成 game-demo 的 C# / C++ 客户端已完成真实 account RPC/Redis → Gate/NATS → PB/Nest → WAL/Mongo → EntitySync 联合验收，并核对会话重建、事务重放与 DAO 冷水合。入口与限制见 [Gate](GATEWAY.md#真实账号gate-与存储联合验收)。这是通用生成链路的功能证据，不把其结论扩张到上述 13 类玩法的独立真实存储或 Robot 容量。
