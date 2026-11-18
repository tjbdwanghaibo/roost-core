# 玩法夹具与 codegen 测试里的业务 handler 改用缺省 async（2026-11-18）

维护者 2026-11-18 决定：业务代码不用 `durability=strict`——写 handler 不写 durability（缺省 async），saga 步骤与补偿用
`pipelined`，只读的用 `memory`；`strict` 仍是框架能力，解析与运行时保留。随后再定：`scripts/testdata/` 与 codegen 测试里手写的
业务 handler 也改，game-demo（`demo/`）这次不改。基线 main `0a9976b8`。前两轮见
[模块](../modules-async-20261118/README.md)与[生成器骨架](../generator-component-package-20261118/README.md)。

本轮没有改 Nest / DataEngine / WAL 的行为，没有改 `modules/`、`demo/`、`docs/agent-skills/`、`claude-skill/`。

## 1. async 在这些夹具里意味着什么（按源码核对）

- `framework/nestwal/wal.go` 的 `Append`：async 记录 `requireSync` 为假，批写进 `*os.File`（操作系统页缓存）后回复；fsync 由刷盘
  goroutine 按组提交 ticker（缺省 10ms）或同批里 strict / pipelined 记录的敲门完成。
- 进程被 SIGKILL：页缓存里的 WAL 不丢；重启时 `scanFramesFrom` 读到完整帧为止，只截掉不完整的尾帧（`torn_frame` / `zero_fill`），
  所以已回复的 async 记录照常回放。玩法 REAL 档的崩溃演练都是“父进程 SIGKILL 自己的子进程”，这一点与改前的 strict 一样成立。
- 整机掉电、内核崩溃：最后一个组提交间隔里已回复的修改可能丢失。本机做不到，没有演练。
- Projector 的 `walUnacked` 在 `reserve` 时就加一，与 durability 无关，所以 REAL 档“未投影记录恰为 N 条”的断言不受影响。
- Nest 之外 `Create` 新建持久实体时补的独立事务（`framework/nest/execution.go` `RunIsolatedTransaction`）仍是框架写死的 strict；
  夹具里“N 条未投影记录”里实体新建那几条仍是 strict，其余业务命令是 async。
- `memory` 事务改了持久字段时提交点整笔拒绝（`refuseMemoryPersistentWrite`），所以改成 memory 的只读 handler 若偷偷写了持久字段，
  场景会以 `ErrMemoryTransactionPersistentWrite` 失败——这是本轮归类的自检。

## 2. scripts/testdata：`//roost:nest` 标记（80 处，19 个文件）

| 归类 | 数量 | handler |
| --- | --- | --- |
| 缺省 async（去掉 `durability=strict`，保留原 `rollback`） | 64 | 见下表 |
| `durability=memory`（只读） | 16 | idle `IdleView` `IdleReceipt`；survival `SurvivalView` `SurvivalReceipt`；merge `MergeView` `MergeReceipt`；tower `TowerView` `TowerReceipt`；chess `ChessView` `ChessReceipt`；coop `CoopView`；card `CardProfile` `CardBattle`；moba `MOBAProfile`；slg `AllianceWallet` `AllianceRemoteRead` |
| `pipelined` | 0 | 夹具里没有 saga 步骤或补偿：`grep -i saga` 命中的四处都是“跨 sid 另接 Saga”的注释；SLG / MOBA 的跨服奖励是业务自己的两步投递，不经 Saga 协调器（见 §5） |

async 的 64 处：

| 文件 | handler |
| --- | --- |
| `auto-chess/handlers.go.txt` | ChessInit、ChessCommand、ChessClaim |
| `card-rpg/handlers.go.txt` | CardDraw、CardManage、CardStart、CardTurn、CardEnemy、CardClaim |
| `coop-roguelike/handlers.go.txt` | ScenarioOpen、ScenarioCommand |
| `generated-tcp/add_gold.go.txt` | AddGold（性能夹具，见 §4） |
| `idle-sim/handlers.go.txt` | IdleInit、IdleCommand |
| `merge-casual/handlers.go.txt` | MergeLogin、MergeBatch、MergeAd |
| `mmorpg/gameplay.go.txt` | MMORPGDisband、Offer、ResolveTrade、Buff、Strike、Loot、Quest、CompleteInstance、Hurt、Revive |
| `mmorpg/tasks.go.txt` | MMORPGParty、Transfer、Trade |
| `moba/round_handlers.go.txt` | MOBARound、MOBAAward、MOBAPreparePlayer、MOBAReserveRound |
| `moba/settlement.go.txt` | MOBASettle |
| `party-room/handlers.go.txt` | PartyOpenMatch、OpenCode、Join、Start、Leave、Connect、Input、Claim、Release、BackfillOffer、Backfill |
| `slg/alliance_authority.go.txt` | AllianceRemoteCAS（`rollback=state`，Remote 托管实体） |
| `slg/alliance_rewards.go.txt` | AllianceHomeInit、AllianceCredit |
| `slg/tasks.go.txt` | SLGResearch、SLGClaimTile、SLGStart、SLGCancel |
| `social-sim/handlers.go.txt` | ScenarioOwn、ScenarioVisit |
| `survival-extraction/handlers.go.txt` | SurvivalInit、SurvivalCommand、SurvivalEnter、SurvivalClaim |
| `tower-defense/handlers.go.txt` | TowerInit、TowerCommand、TowerClaim |
| `turn-pvp/handlers.go.txt` | ScenarioOpen、ScenarioCommand |

只读的判定：函数体只调 `Decode` / `View` / `Balance` / `Count` / `Current`（stamina 的只读结算）/ room 的 `View`、`Member`，不调任何 setter、
不改组件状态。`BackfillOffer`（会 `SaveRoom`）、`PreparePlayer`、`ReserveRound`、`IdleInit`（首次写存档）虽然名字像查询，都有写，归 async。
`AllianceRemoteRead` 是 Remote 托管实体上的只读：与 alliance 模块的 `AllianceInfo` 一样用 memory，带 Remote 批次、没有修改时不写任何东西。

## 3. 手写注册的 `HandlerMeta{…Durability: nest.DurabilityStrict}`（34 处）

| 归类 | 数量 | 位置 |
| --- | --- | --- |
| 改 `DurabilityAsync` | 26 | 玩法 `*_test.go.txt` 里业务式的测试 handler：undo / 拒绝用例（idle、moba×2、party×3、survival×3、mmorpg×5、merge、card×3、slg×3、tower×2、chess×2，含 party 的 `addCoins`、mmorpg 的初始化与场景种子），以及 `mmorpg/persistence/persistence_test.go` 的种子 `mmoRealSeed` |
| 改 `DurabilityMemory` | 1 | `mmorpg/persistence/persistence_test.go` 的 `mmoRealView`（只读，返回 DAO 字节与字段） |
| **保留 strict** | 7 | `remoteflow/`（3）、`entity-review/`（4），理由见下 |

保留的 7 处不是业务 handler，是框架能力的测试：

- `remoteflow/create_unchanged_test.go`（2）、`remoteflow/shared_writers_test.go`（1）：Remote 托管实体写路径的框架用例（与
  `codegen/internal/entity/testdata/remoteflow` 同包、同一个 `newRejectRig` 按策略参数化）。用例在 `Request` 返回后立即
  `assertNoPending`、读 Mongo 信封，依赖 strict“等 Remote 结论再回复”；async 在 Remote 托管实体上返回推测回执、不等 Remote 结论
  （[Nest §6 第 4 行](../../../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)），改了会让断言测的不再是被测路径。
- `entity-review/*.go.txt`（4）：`package nest` 的审查复现（失败建实体不得持久），要复制进 `framework/nest` 才能跑，没有脚本引用；
  其中一条的现行版本是 `framework/nest/failed_create_review_test.go`，同样写 strict。保持与框架内副本一致。

## 4. codegen

| 位置 | 处理 |
| --- | --- |
| `codegen/internal/roost/testdata/saga_native/game/handler/level_up.go`、`start_gift.go` | 改缺省 async。`LevelUp` 只是升级并发 reliable effect（同一条 WAL 记录经 outbox 发出，不需要 strict）；`StartGift` 是 Saga 的发起方，照 alliance `StartAllianceOp` 的归类：启动意图随这条记录经 outbox 送出，丢了协调器从没收到，同一业务键重发即可。四个原生步骤 handler（`gift_debit` 等）本来就是 `pipelined`，未动 |
| `codegen/internal/roost/self_sync_test.go` 写进生成工程的 `handlerAddGold` | 改缺省 async |
| 10 处“故意写坏的 handler”（`module_timer_test.go`×2、`module_activity_test.go`、`module_alliance_test.go`、`module_dao_test.go`、`module_test.go`×2、`module_entity_test.go`、`module_runtime_test.go`、`roost_test.go:2467`） | 去掉 `durability=strict`。它们坏在没有实体参数 / 语法不完整，与 durability 无关，生成照样失败 |
| `codegen/internal/roost/testdata/gate/load_test.go.txt` 的心跳 | 去掉 `heartbeat.WithDurability(nest.DurabilityStrict)`，取心跳缺省 async（与 modules 的 combat 心跳同样处理） |
| `scripts/perf/generated-tcp.sh` + `scripts/testdata/generated-tcp/add_gold.go.txt` | 夹具改为不写 durability；脚本按 `DURABILITY` 在 `//roost:nest rollback=undo` 后补级别（async 时不补）。`DURABILITY` 缺省仍是 `strict`，这是性能对照的测量旋钮，见 §6 |

保留（按任务说明）：

- `codegen/internal/nest/`：`parse.go`（原生步骤要求 strict 或 pipelined 的报错文本）、`gen.go`、`testdata/handler.go`（`handlerTransferItem`
  专测 strict 取值解析，`nest_test.go:198` 断言生成 `nest.DurabilityStrict`）、`generated_signatures_test.go`、`handler_contract_test.go`、
  `nest_test.go`、`owner_test.go`（解析器与生成器的取值测试）。
- 生成器守卫里断言“不出现 strict”的字符串：`add_test.go:1053/1157/1162/1163`、`roost_test.go:524`（`DurabilityStrict`）、`roost_test.go:2234`。
- `demo_test.go:788/873`：对 demo 内容的断言（demo 本轮不改）。
- `codegen/internal/entity/testdata/{dataengine,remoteflow}`、`codegen/internal/dao/testdata/runtime`：DataEngine / Remote 框架路径的用例，
  直接构造 strict 提交或按策略参数化，不是业务 handler。
- `codegen/docs/NEST_RUNTIME.zh-CN.md`：取值列表。

## 5. 依赖“回复前已落盘”的用例与写法

逐个核对了断言 strict 或依赖 strict 才成立的地方：

- **没有用例断言“提交记录是 strict”。** 玩法夹具没有读 `record.Durability` 的断言。
- **REAL 档 SIGKILL 恢复**（idle / merge / card / chess / tower / survival / party / mmorpg / moba / slg / coop / social）：只杀进程，
  按 §1 async 已回复的修改留在页缓存里的 WAL，重启照常回放；“未投影记录恰为 N 条”按 §1 不受 durability 影响。注释、日志文本里
  “strict records / strict WAL”改成“WAL records”，MOBA 快照用例的样本字段 `StrictMicros` 改名 `CommitMicros`（同一文件里写、读）。
  `genre-common` 的屏障仍断言 `Syncs > 0`：实体新建是 strict，且组提交 ticker 10ms 一次，屏障前必然刷过盘。
- **两步投递**（下一步在本步回复之后、对外不可撤销）——进程崩溃不受影响，**整机掉电时有丢失窗口**，与模块证据里 mailbox / purchase
  同类，不为它保留 strict，交维护者决定（前两条维护者已定 B，改 `pipelined`，见 [§8](#8-两步投递改-pipelined2026-11-18)）：
  - MOBA `MOBAAward`：原服到账回复后 `ackSeat` 向权威确认这一席。掉电丢掉已回复的到账而确认已到权威时，这名玩家的奖励丢失，
    重投扫描只看未确认的席，不会再发。
  - SLG `AllianceCredit`：`deliverPending` 到账回复后在原服义务账本（Redis）标完成。掉电丢掉已回复的到账而标记已写时，奖励丢失。
  - MOBA 帧：权威 `MOBARound` 回复后 Room 广播这一帧。掉电丢掉最后一个组提交间隔里已广播的帧时，恢复从已持久的头部继续，
    客户端已经看到过的帧在权威侧不存在；恢复后 `resume` 换代，旧代际客户端走快照 / 重连，没有掉电演练覆盖，**尚未确认**会不会分叉。
  对应文档（MOBA §、SLG 到账一行）已写上这一代价。
- party / survival / tower / chess / mmorpg 的领奖、交易都是同进程多实体一笔事务，回复之后没有对外不可撤销的下一步。

## 6. 验证

全部 `GOWORK=off`，本检出（基线 `0a9976b8` 加本轮改动），本机 macOS（Darwin 25.5，arm64），1 分钟负载 15～17（其他会话在跑）。
REAL 档用私有 `scripts/mirror-local.sh`（`ROOST_MIRROR_LOCAL_OFFSET=23700`，根目录在会话 scratch，用完 `clean`），没有用共享隔离环境。

| 命令 | 结果 |
| --- | --- |
| 缺省档 14 个脚本：`test-{auto-chess,card-rpg,coop-roguelike,idle-sim,merge-casual,mmorpg,moba,party-room,slg,social-sim,survival-extraction,tower-defense,turn-pvp}-generated.sh`、`test-mmorpg-scenes.sh`（清掉 REAL 开关，4 路并行） | 全部 ok（52～155s），每个脚本自己核对场景数与 0 SKIP、`roost module verify`、build / vet / `roost id check` / `generate --check` |
| REAL 档 12 个：上面除 turn-pvp、mmorpg-scenes 外各加 `ROOST_<GENRE>_REAL=1`（MOBA 另加 `ROOST_MOBA_ROUTING=1`） | 全部 ok。逐条 PASS：MOBA 的 `TestMOBARealSettlementAcrossProcesses`、`ActiveRoundCrashRecovery`、`ActiveRoundSnapshotRecoveryAfterSIGKILL`（“125 unprojected WAL records (25 after setup + 100 frames)”）、`SnapshotDuringHostHandoff`、`CrossSIDHostRouting`；SLG 的 `TestSLGAllianceActivity`、`PlayerDelivery`、`RemoteDelivery`（Remote 托管权威上 async CAS + memory 读，交接后继续到账）、`RPCReceiverBudget`；其余玩法的 SIGKILL 恢复用例 |
| `ROOST_TURN_PVP_REAL=1 scripts/test-turn-pvp-generated.sh` | ok：Gate / TCP / NATS / Mongo / WAL、SIGKILL 恢复、离线超时一次与重放 |
| `go test -run '^(TestModuleActivityServiceRollback\|…\|TestGeneratedSagaNativeAndEffectSkeletonsCompile)$' ./codegen/internal/roost/`（改到的 11 个用例 + 生成器守卫 + `TestAddSagaNative*` 4 个 + 骨架编译） | 17 个全部 PASS，0 SKIP（`TestModuleAddDeclaresTheFrameworkServicesItUses` 在前两轮证据里于 `f229585e` 失败，这次通过） |
| `TestGeneratedSagaNativeStepsAndEffectConsumer`（私有 Mongo / NATS） | PASS：async 的发起赠礼 / 升级 + pipelined 原生步骤，一笔正向、一笔补偿与一条 effect 消费 |
| `TestGateGeneratedBusinessConsumer`（`ROOST_GATE_REAL=1 ROOST_GATE_LOAD_SECONDS=5`，私有 Mongo / Redis / NATS） | PASS，负载段跑完（心跳取缺省 async） |
| `scripts/perf/generated-tcp.sh`，`DURABILITY=async` 与 `strict` 各 5 秒 | 都 PASS；生成工程里 async 时标记是 `//roost:nest rollback=undo`，strict 时补成 `rollback=undo durability=strict` |
| `go vet ./codegen/internal/roost/`、`go test -count=1 .`（根包） | 通过 |

MOBA 快照用例的诊断样本（100 次逐帧 Sender，async）：p50 7.0ms、p99 27.1ms——负载 15 以上、每帧重写整段日志并重算，只是功能运行，不作性能结论。

顺带修的：`scripts/testdata/generated-tcp/wallet.go.txt` 还按旧写法给实体包里的 `WalletComponent` 加方法，生成器改成组件包之后
`scripts/perf/generated-tcp.sh` 在基线上就编译失败（`undefined: WalletComponent`）。改成给 `game/components/wallet` 的 `Component` 加
`AddGold`，脚本复制到 `game/components/wallet/add_gold.go`，与 durability 无关。

## 7. 未验证与待决定

- 没有整机掉电 / 内核崩溃演练（本机做不到）：§5 的两步投递丢失窗口按源码推断。
- ~~**待维护者决定**：MOBA `MOBAAward` 与 SLG `AllianceCredit` 的掉电窗口（§5）。~~ 维护者 2026-11-18 定 B：这两个改 `pipelined`，
  见 [§8](#8-两步投递改-pipelined2026-11-18)。
- **待维护者决定**：`scripts/perf/generated-tcp.sh` 的 `DURABILITY` 缺省仍是 `strict`，保留与历史基线可比；要改成 `async` 是一行。
- `scripts/testdata/operations-real/` 的注释仍写“strict grant before Ack”：那是 purchase 模块的发货，模块已改 async、另一会话在改
  purchase，本轮不动。
- `demo/` 仍有 `durability=strict`，按维护者决定以后另排。

## 8. 两步投递改 pipelined（2026-11-18）

维护者定 B：§5 的 MOBA `MOBAAward` 与 SLG `AllianceCredit` 改 `//roost:nest rollback=undo durability=pipelined`（原 `rollback=undo`
不变）。基线 main `3d703d7e`。没有改 Nest / DataEngine / WAL，没有碰 `client/ts` 与 codegen。

### 8.1 改了什么

| 文件 | 改动 |
| --- | --- |
| `scripts/testdata/moba/round_handlers.go.txt`、`scripts/testdata/slg/alliance_rewards.go.txt` | 两个标记加 `durability=pipelined`；注释写明原因：回复之后马上 ackSeat / 标完成，确认之后不再重投，所以回复要在 fsync 之后 |
| `scripts/testdata/moba/settlement_test.go.txt`、`scripts/testdata/slg/tasks_test.go.txt` | 缺省档的替身提交器 `mobaCommitter` / `slgCommitter` 补 `Enqueue` / `DurableLSN`（照 alliance 模块测试的 `pipelinedRecorder`：入队即持久），改为指针；夹具拆出 `newRoundFixtureWith` / `newSLGFixtureWith` 接收提交器 |
| `scripts/testdata/moba/round_handlers_test.go.txt`、`scripts/testdata/slg/alliance_rewards_test.go.txt` | 各加一条用例与一个按 WAL 语义建模的替身（`mobaFsyncWAL` / `slgFsyncWAL`），见 §8.3 |
| `scripts/test-moba-generated.sh`、`scripts/test-slg-generated.sh` | 断言条数 28 → 29、8 → 9 |
| `docs/framework/MOBA-INTEGRATION.md` §4.4 / §4.5 / §5.8 / §10、`docs/framework/SLG-INTEGRATION.md` §3.4 / §7.3 / §10、`scripts/README.md`、`docs/maintenance/KNOWN-LIMITS.md` 玩法表、`docs/agent-skills/roost-genre`（SKILL 与 moba / slg 参考） | 掉电窗口那句改成“已用 pipelined，确认在落盘之后”；场景条数与列表同步 |

### 8.2 pipelined 的运行时要求怎样满足

- **提交器**：声明 pipelined 而提交器没有 `Enqueue` / `DurableLSN` 时 Nest 返回 `ErrPipelinedCommitterRequired`，不降级为 strict
  （`framework/nest/execution.go` 的 pipelined 分支）。正式装配用 DataEngine 的 `Projector`（`framework/dataengine/engine/projector.go`
  实现 `Enqueue` / `DurableLSN`，`Runtime.NestOptions` 把它装成 committer）；两套 REAL 用例都经 `engine.NewRuntime(...).NestOptions()` 建 Nest，
  走的就是它。缺省档替身见 §8.1。
- **allowlist**：框架只自动把声明 pipelined 的 saga 原生步骤补进 `nest.pipelined.allowlist`（`wiring/dataengine/mod.go`
  `pipelinedSettings`，且只在名单非空时补）。两套夹具都不改配置，生成工程的 `configs/service/config.game.yaml`、
  `config.game.prod.example.yaml` 与 `deploy/k8s/base/secret.game.example.yaml` 都是 `allowlist: []`（空 = 全部允许，本轮在保留的生成工程里
  核对过）；REAL 用例传 `engine.PipelinedRuntimeConfig{}`，同样不设名单。工程自己写了非空名单时要把这两个 handler 列进去，文档与夹具注释已写明。
- **异步完成**：这两个 handler 没有声明 `completion=async`，在派发 worker 上等票据、回复在持久之后；同一 Player 上的下一条请求要等这条
  持久并回复后才开始。到账是低频写，不需要异步完成。

### 8.3 用例：确认在落盘之后

`TestMOBAAwardRepliesOnlyAfterItsRecordIsDurable`、`TestSLGAllianceCreditRepliesOnlyAfterItsRecordIsDurable`：

- 替身提交器按 WAL 语义建模：strict 记录在 `Commit` 里即持久；async 记录 `Commit` 时只写进“页缓存”，替身不跑组提交 ticker，用例里一直不持久；
  pipelined 记录 `Enqueue` 后挂起，用例调 `fsync` 才持久并完成票据；`fsync` 按前缀生效。夹具建局 / 建档时自动 fsync。
- 用例在 goroutine 里发同步到账请求，回复之后立即记下到账记录是否已持久——这就是 ackSeat / 标完成的位置。看到挂起的票据时，先确认 100ms 内
  没有回复（持久之前不得回复），再 fsync；没有票据而直接回复时（async / strict 的路径）直接核对。断言：回复时到账记录已持久。
- MOBA 之后照常向权威 ack 这一席并核对资产；SLG 核对余额 60。
- 不用睡眠判断顺序：pipelined 的回复依赖用例 fsync，async 的回复不依赖任何票据，两条路径都是确定的。

现有用例不能自然覆盖：缺省档的替身 `Commit` 立即返回、不区分持久与否；REAL 档用真实 WAL，但演练只 SIGKILL 进程，async 已回复的记录在页缓存里
照样回放，不区分“写进页缓存”与“fsync”；本机做不到整机掉电。

### 8.4 先红后绿与变异

| 步骤 | 命令 | 结果 |
| --- | --- | --- |
| 红（标记仍为 `rollback=undo`，即缺省 async；新用例与替身已就位） | `scripts/test-moba-generated.sh`、`scripts/test-slg-generated.sh` 缺省档 | 都失败：MOBA 28 PASS 1 FAIL，SLG 8 PASS 1 FAIL，失败的就是新用例：`award replied while its WAL record (durability=async) was not durable: …`、`credit replied while its WAL record (durability=async) was not durable: …`（[pipelined-red.txt](pipelined-red.txt)） |
| 绿（改为 pipelined） | 同上 | 都 ok：MOBA 29 条 TestMOBA、SLG 9 条 TestSLG + 联盟纯内存 2 条，0 SKIP；`module verify: wallet 17/17 skipped=0`；build / vet / `roost id check` / `generate --check` 通过。新用例 0.12s / 0.10s（含持久前那 100ms 等待，说明走的是挂起票据的分支） |
| 变异 async | 保留的生成工程里把标记改成 `durability=async`、`roost generate`、单跑新用例 | 两条都 FAIL，文本同红（[pipelined-mutations.txt](pipelined-mutations.txt)） |
| 对照 strict | 同上改成 `durability=strict` | 两条都 PASS：strict 回复前已 fsync，同样满足承诺（只是持锁等 fsync） |

### 8.5 REAL 档

私有 `scripts/mirror-local.sh`（`ROOST_MIRROR_LOCAL_OFFSET=24900`，根目录在会话 scratch，用完 `clean`），没有用共享隔离环境；本机 1 分钟负载 9～11。

| 命令 | 结果 |
| --- | --- |
| `ROOST_MOBA_REAL=1 ROOST_MOBA_ROUTING=1 scripts/test-moba-generated.sh` | ok，35 PASS 0 SKIP：`TestMOBARealSettlementAcrossProcesses`（write / recover）、`ActiveRoundCrashRecovery`、`ActiveRoundSnapshotRecoveryAfterSIGKILL`、`SnapshotDuringHostHandoff`、`CrossSIDHostRouting`（五进程 NATS，原服奖励经 Projector 的 pipelined 提交到账、崩溃后重投） |
| `ROOST_SLG_REAL=1 scripts/test-slg-generated.sh` | ok，15 PASS 0 SKIP：`TestSLGAllianceActivity`、`PlayerDelivery`（每个原服仍是“create / initialize / reward”三条未投影 WAL 记录，SIGKILL 后冷加载补完）、`RemoteDelivery`、`RPCReceiverBudget` |
| `go test -count=1 .`（根包）、`scripts/check-doc-anchors.py` | 根包 ok；锚点检查本轮新增的锚点都解析，余下 3 处是 `codegen/docs/PROJECT_GENERATOR.zh-CN.md` 既有的 `#6w-…` 断链，与本轮无关 |

### 8.6 未验证

- 仍没有整机掉电 / 内核崩溃演练：“确认在落盘之后”由替身用例与 Nest pipelined 契约（`guide/02-nest-entity.md` §4）保证，真实 fsync 时序没有掉电注入。
- MOBA 帧（`MOBARound` 回复后广播）仍是 async，§5 第三条“尚未确认”不变，本轮不动。
- 吞吐：到账每笔多等一次组提交 fsync（缺省最长约 10ms），没有测量；这两处是低频写。

