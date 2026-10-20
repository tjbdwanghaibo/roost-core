# scripts/

Remote 容量/长稳：`scripts/perf/remote.sh`（默认 30m，`ROOST_REMOTE_DURATION=24h` 可复跑 24 小时）；故障矩阵：`scripts/test-remote-matrix.sh`。两者独占隔离环境，原始日志和数据保存于 `artifacts/perf/remote/<label>/`；[负载、判据与结果](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md)。

Remote 正式业务验收：加载隔离环境的 `ROOST_DATAENGINE_IT_MONGO_URI`、`ROOST_DATAENGINE_IT_REDIS_ADDR`、`ROOST_DATAENGINE_IT_NATS_URL` 后运行 `./scripts/test-remote-generated.sh`。正式 DAO/Entity 生成器构造两实体各两 DAO，经 Nest 与三种持久化策略提交，校验 Mongo、业务拒绝/panic 回滚和真实 NATS 快照接收；不依赖 demo。同一次运行还跑持久拒绝与收尾用例（`reject_test.go` / `outcome_test.go`：正式 `ManagerAccess` 作 Remote loader + entitysync，Durability 0～3 的持久拒绝后卸载、重载全量、重载后可写，以及 strict 确认截止、结果未知后停机、嵌套独立事务、业务 worker 上删除）；`ROOST_REMOTE_LOAD=1` 或设置了 `ROOST_REMOTE_FAULT` 时只跑 `TestGeneratedRemoteNestFlow`。`ROOST_REMOTE_COUNT` 设 `-count`，`ROOST_REMOTE_RUN` 覆盖 `-run`。脚本只清理自身临时工程，测试仅清理专属数据库和 Redis key。[完整验收及独立多进程故障命令](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-24-remote.md#第三轮进程故障与正式业务链路)。

仓库级的工具，**每个只有一份**（合仓前各仓的副本已收敛到这里）。

| 路径 | 做什么 | 谁在用 |
| --- | --- | --- |
| `pretag.sh <version>` | 发版前的门禁：tag 与 module 主版本一致、tag 未存在、无 replace、工作树干净、`GOWORK=off` 下 build/vet/tidy/test 全过、发布清单声明的版本等于要打的 tag | 人工在打 tag 之前跑；`.github/workflows/release.yml` 在 tag 之后再校验一次清单 |
| `gapmap.sh [--max N] [--group G] [pkg...]` | 覆盖率采样：按包回退断言看测试是否真的会红，产出 `gapmap-report.md`；`--group framework\|infra\|codegen\|rest` 只取样一组（按目录前缀分，其余目录都归 rest） | `.github/workflows/nightly-gapmap.yml`（四组 matrix 并行） |
| `gapmap/` | 上面那个脚本的 Python 部件（`classscan.py`、`revertsample.py`；`merge.py` 把各组报告合成一份 `gapmap-report.md`，缺组时写明并以 1 退出） | `gapmap.sh`、nightly-gapmap 的 merge job |
| `perf/` | `nest-msg.sh` 跑 [Nest 消息吞吐](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/NEST-MSG-THROUGHPUT-2026-09-24.md)；`nest.sh` 跑 [Nest 调度与诊断微基准](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/NEST-COMPLETION-2026-09-24.md)；`sync.sh` 跑微基准；`sync-business.sh` 跑[隔离 demo](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-BUSINESS-LOAD-2026-09-23.md)；`sync-aoi.sh` 跑[1000/10000 AOI 双进程负载](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-AOI-1000-10000-2026-09-23.md) | 人工 |
| `check-doc-anchors.py [路径...]` | Markdown 相对链接 + `#锚点` 核对（GitHub slug 规则），缺省查 `docs/framework`；根包 `TestTrackedMarkdownRelativeLinksResolve` 只查目标文件存在、不查锚点 | 改 [框架文档](../docs/framework/README.md) 后人工跑，须零断链 |

另外两处 `scripts/` 是**层内**的，不重复：

- `wiring/scripts/integration/` — 框架服务与 Wiring 的集成环境（brew 起的 Redis / Mongo 副本集 / NATS，见 `dataengine-env.sh`；根目录与端口偏移用 `ROOST_IT_HOME` / `ROOST_IT_PORT_OFFSET` 配置，长期环境不要放 /tmp，见[该目录 README](../wiring/scripts/integration/README.md)）。
- `codegen/scripts/` — 生成器自己的运行时校验（`*-runtime.sh` 把生成物编译进真运行时跑一遍）、
  `source-head-check.sh`（本地版 framework-compat）、`install-windows.ps1`。

Sync AOI 工具新增全量预算和集中恢复参数，见[六项实施与复跑命令](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。

正式双模式：`./scripts/test-sync-modes-generated.sh` 验证 DAO/Entity 生成工程与 Nest→Sync；`./scripts/perf/sync-aoi.sh -mode=periodic` 或 `-mode=on_change` 默认走正式 Nest 内存提交和 Interest 事实队列，`-nest=false` 仅用于兼容原来的独立 Sync 基线。业务输入固定每 50ms 一批，详见[双模式实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。

SLG 业务接入纵切：`./scripts/test-slg-generated.sh` 用当前 CLI 从 `game` 模板生成隔离工程，`add dao` / `add timer` / `add entity` 后只拷入 `scripts/testdata/slg/` 的业务文件，跑 6 条 `-race` 场景（建造 → 行军、重放与取消退款、失败回滚、离线收益、地块竞争、研究与建造并行）和 `roost id check` / `generate --check`。不需要外部依赖（提交器替身 + DAO 序列化），不挂在 `dataengine-env.sh test`；改 `game` 模板、`add timer` / `add dao` / `add entity` 或 Nest Sender 生成时手动跑，发版验证清单同 `test-sync-modes-generated.sh`。手册见 [SLG-INTEGRATION](../docs/framework/SLG-INTEGRATION.md)。

放置／模拟经营业务接入纵切：`./scripts/test-idle-sim-generated.sh` 正式生成 Player DAO／Sender／timer，运行 12 条 race 场景，覆盖三工坊、预扣／取消、满缓冲／仓库、倍率积分与升级、固定离线窗口、订单／收益／每日奖励、并发重放与 undo。`ROOST_IDLE_SIM_REAL=1` 加隔离 Mongo URI，在五条 strict WAL 未投影时 SIGKILL，冷加载半成品／成品／倍率／窗口，驱动补算一次，第三进程重放所有命令不重复扣料或发奖。`ROOST_IDLE_SIM_KEEP=1` 保留成功工程，失败保留现场；入口已登记根包覆盖表。接法与边界见 [IDLE-SIM-INTEGRATION](../docs/framework/IDLE-SIM-INTEGRATION.md)。

合成／超休闲业务接入纵切：`./scripts/test-merge-casual-generated.sh` 从正式 game 模板生成 Player DAO、Sender 和持久 timer，运行 15 条 race 场景，覆盖棋盘／仓库／生产器、订单／出售撤销、离线批次原子性、版本冲突与设备代际、服务器限时挑战、广告签名／永久回执。随后消费者 build／vet、ID 和生成状态检查。`ROOST_MERGE_CASUAL_REAL=1` 与隔离 Mongo URI 追加五条 strict WAL 未投影时 SIGKILL、冷加载期限与棋盘、第三进程生产／广告／领奖重试。`ROOST_MERGE_CASUAL_KEEP=1` 保留成功工程；失败保留现场，成功仅删除本次唯一库。专门入口登记根包覆盖表；教学与外部平台边界见 [MERGE-CASUAL-INTEGRATION](../docs/framework/MERGE-CASUAL-INTEGRATION.md)。

卡牌 RPG 业务接入纵切：`./scripts/test-card-rpg-generated.sh` 同样从 `game` 模板生成隔离工程，`add dao` / `add entity` / `add timer` / `add attribute` 后只改 DAO 定义与实体锁档并拷入 `scripts/testdata/card-rpg/` 的业务文件，跑 16 条 `-race` 场景（配置原子热更与按值冻结、属性生成、视图裁剪与状态防篡改、抽卡并发重放 / 保底 / 回滚、养成与编队、开局并发准入、回合权限与重放、护盾 / 法力 / 毒伤 / 治疗、领奖 / 首通 / 扫荡 / 任务、撤退、旧期限与迟到 Tick、开局 / 出手 undo、账本上限）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_CARD_RPG_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestCardRPGRealCrashRecovery`（strict WAL 未投影时 SIGKILL 子进程、冷加载后由驱动补触发一回合、续战领奖、第三进程冷加载重试；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_CARD_RPG_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add entity` / `add timer` / `add attribute`、多实体 Sender 或 Nest 生成时手动跑。手册见 [CARD-RPG-INTEGRATION](../docs/framework/CARD-RPG-INTEGRATION.md)。

合作塔防业务接入纵切：`./scripts/test-tower-defense-generated.sh` 从 `game` 模板生成隔离工程，`add dao Player`、`add entity` / `add dao` / `add timer` 的 TowerRoom 后只改 DAO 定义与实体锁档并拷入 `scripts/testdata/tower-defense/` 的业务文件，跑 10 条 `-race` 场景（4 条规则：BFS 绕路与封堵、确定性推进与每次触发一步、严格解码与账本上限、配置热更；6 条 Nest：同格并发建造与重放、阻路 / 他人出售 / 旧等级拒绝与退款、准备 / 掉线 / 旧连接与期限准入、建造 undo、四人胜利与并发领奖、失败无奖与视图副本）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_TOWER_DEFENSE_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestTowerDefenseRealCrashRecovery`（第一波进行中 strict WAL 未投影时 SIGKILL 子进程、冷加载后由驱动投递循环补触发一步并打完三波、领奖，第三进程冷加载重试；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_TOWER_DEFENSE_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add entity` / `add timer`、多实体 Sender 或 Nest 生成时手动跑。手册见 [TOWER-DEFENSE-INTEGRATION](../docs/framework/TOWER-DEFENSE-INTEGRATION.md)。

合作生存撤离业务接入纵切：`./scripts/test-survival-extraction-generated.sh` 从 `game` 模板生成隔离工程，`add dao Player`、`add entity` / `add dao` / `add timer` 的 SurvivalRaid 后只改 DAO 定义与实体锁档并拷入 `scripts/testdata/survival-extraction/` 的业务文件，跑 12 条 `-race` 场景（4 条规则：死亡退料与物资守恒、一次触发只算一次伤害且结束时刻不变、严格解码 / 账本上限 / 未入场席位、配置热更；8 条 Nest：入场 undo 与并发入场只扣一次、共享资源点并发采集、制作预扣 / 容量 / 取消 / 治疗、死亡掉落与并发拾取、撤离打断 / 封存 / 兑付 undo 与并发兑付、重连与私有视图 / 期限准入、越过结束时刻收局与重复入场、制作 undo）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_SURVIVAL_EXTRACTION_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestSurvivalExtractionRealCrashRecovery`（制作与撤离都在途时 strict WAL 未投影 SIGKILL 子进程、冷加载后由驱动投递循环补触发一次并按期收局、兑付，第三进程冷加载重试；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_SURVIVAL_EXTRACTION_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add entity` / `add timer`、多实体 Sender 或 Nest 生成时手动跑。手册见 [SURVIVAL-EXTRACTION-INTEGRATION](../docs/framework/SURVIVAL-EXTRACTION-INTEGRATION.md)。

合成休闲业务接入纵切：`./scripts/test-merge-casual-generated.sh` 同样从 `game` 模板生成隔离工程，`add dao` / `add timer` 后只改 DAO 定义并拷入 `scripts/testdata/merge-casual/` 的业务文件，跑 15 条 `-race` 场景（配置热更与存档冻结、存档防篡改与视图裁剪、命令账本上限、棋盘 / 仓库 / 解锁、并发生产重放、出售撤销、订单、离线批次原子性与版本冲突、设备接管、服务器判胜与并发领奖、挑战期限与迟到 Tick、生产 / 开局 undo、体力余数与时钟回退、广告验签 / 并发回调 / 每日上限）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_MERGE_CASUAL_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestMergeCasualRealCrashRecovery`（strict WAL 未投影时 SIGKILL 子进程、冷加载后核对驱动索引里的期限、启动驱动投递循环补触发一次判负并按时投递下一局的期限、续玩领奖、第三进程冷加载重试；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_MERGE_CASUAL_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add timer` 或 Nest 生成时手动跑。手册见 [MERGE-CASUAL-INTEGRATION](../docs/framework/MERGE-CASUAL-INTEGRATION.md)。

放置模拟业务接入纵切：`./scripts/test-idle-sim-generated.sh` 同样从 `game` 模板生成隔离工程，`add dao` / `add timer` 后只改 DAO 定义并拷入 `scripts/testdata/idle-sim/` 的业务文件，跑 12 条 `-race` 场景（配置热更与冻结、分段与一次积分一致、存档防篡改与溢出、三工坊并行、取消退款、仓库 / 订单 / 出售、倍率与升级先结算、并发入队 / 领取 / 每日奖励、一年时间跳跃只投递一次且不延长窗口、UTC 日界、入队 / 倍率 undo、待领格满与账本上限）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_IDLE_SIM_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestIdleSimRealCrashRecovery`（strict WAL 未投影时 SIGKILL 子进程、冷加载后核对驱动索引里的期限、启动驱动投递循环补触发一次、只积分到原窗口并按时投递下一秒的刷新、领取结算、第三进程冷加载重放；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_IDLE_SIM_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add timer` 或 Nest 生成时手动跑。手册见 [IDLE-SIM-INTEGRATION](../docs/framework/IDLE-SIM-INTEGRATION.md)。

自走棋业务接入纵切：`./scripts/test-auto-chess-generated.sh` 同样从 `game` 模板生成隔离工程，`add dao` / `add entity` / `add timer` 后只改 DAO 定义与实体锁档并拷入 `scripts/testdata/auto-chess/` 的业务文件，跑 12 条 `-race` 场景（合星与卡池守恒、战斗重算 / 轮空 / 淘汰、状态防篡改与一次一阶段、配置热更与对局冻结、八席并发购买、并发准备只冻结一场、期限与掉线、接管与私有视图、编队 / 升级 / 锁店、刷新 undo、终局排名与多实体并发领奖、账本上限）和消费者 build / vet、`roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_AUTO_CHESS_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加 `TestAutoChessRealCrashRecovery`（八人冻结战斗后 strict WAL 未投影时 SIGKILL 子进程、冷加载后启动驱动投递循环只补一个阶段并按期推进到终局、核对投递计数与阶段逐次对应且无重投、八人领奖、第三进程冷加载重试；登记在根包 `integration_coverage_test.go`），成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_AUTO_CHESS_KEEP=1` 保留；不挂在 `dataengine-env.sh test`，改 `game` 模板、`add dao` / `add entity` / `add timer`、多实体 Sender 或 Nest 生成时手动跑。手册见 [AUTO-CHESS-INTEGRATION](../docs/framework/AUTO-CHESS-INTEGRATION.md)。

MMORPG 业务接入纵切：`./scripts/test-mmorpg-generated.sh` 同样从 `game` 模板生成隔离工程，`add dao` / `add entity` / `add timer` 后只改 DAO 定义并拷入 `scripts/testdata/mmorpg/` 的业务文件，跑 13 条 `-race` 场景（组队与解散、并发转账去重与拒绝回滚、托管交易确认 / 取消 / 到期 / 竞争、战斗冷却与掉落任务幂等、副本完成判定、Buff 刷新 / 回滚 / 离线到期、死亡与延迟复活、带版本的切图）和 `roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_MMORPG_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加两个真实文件 WAL / Mongo 用例（登记在根包 `integration_coverage_test.go`），定时器到期都由 Player 驱动的投递循环投递（不手发 Tick）：两个独立进程正常停机后冷加载恢复，循环补触发一次离线到期的 Buff（`TestMMORPGRealPersistenceAcrossProcesses`）；野外战斗在途（Buff 与复活计时、托管交易单方确认）、strict WAL 有 12 条未投影记录时 SIGKILL 子进程，冷加载后逐字节一致，循环给两个过期玩家各补一次、按时投递下一个 Buff 期限，到期交易退款一次，第三个进程冷加载重试不重复（`TestMMORPGRealCrashRecovery`）；两个用例都必须 PASS、无 SKIP，成功后删除本次唯一库，失败保留并打印库名。成功删除临时工程，失败或 `ROOST_MMORPG_KEEP=1` 保留；改 `game` 模板、`add dao` / `add entity` / `add timer` 或多实体 Sender 生成时手动跑。

MMORPG 多地图网络接入：`./scripts/test-mmorpg-scenes.sh` 生成 game-demo 后放入多地图接法（一个 Manager、每张地图一个 Interest），在 demo 玩家 TCP 与框架 `frame.Receiver` 上检查切图删除 / 全量、准入失败后 held 重试、旧 epoch、重连与字段隐私，再 build / vet；不需要外部依赖。`./scripts/test-mmorpg-cross-sid.sh` 对**已启动**的双 sid（1000 / 1001）game-demo 运行真实账号错服拒绝、双向赠礼 Saga、已领邮件重领不加物品与无效收件人补偿；资源地址、前缀、库名与玩家 TCP 端点从环境变量取得，不起停服务、不改 Player sid。两者都不挂在 `dataengine-env.sh test`、不在根包门禁里。手册与起栈步骤见 [MMORPG-INTEGRATION](../docs/framework/MMORPG-INTEGRATION.md)。

MOBA 业务接入纵切：`./scripts/test-moba-generated.sh` 从 `game` 模板生成隔离工程，`add dao Player`、`add entity` / `add dao` 的 MatchResult、BattleRound、BattleDirectory 后只改 DAO 定义并拷入 `scripts/testdata/moba/` 的局内模拟、5v5 房间与 handler，跑 25 条 `-race` 场景（双英雄 12 条：匹配排队 / 取消 / 并发成局幂等、会话输入身份与迟到窗口、冗余去重、断线追帧与旁观者、历史过期与 NoMajority、推塔胜负、技能 / 商店 / 检查点、真实 loopback KCP 丢包追帧收敛、结算并发重放 / 身份冲突 / 拒绝回滚 / 无胜者 / 完整服务器结果链；5v5 13 条：十人成局与 ban/pick、三路规则与日志重算、十客户端 KCP 追帧、提交后广播与结果未知停房间、宿主交接隔离、帧与快照 undo、目录并发只分配一局、原服奖励部分投递与账本上限、有界邮箱排空、超过 64 帧的持久日志补齐与延迟观战），再对消费者 build / vet 和 `roost id check` / `generate --check`。缺省不需要外部依赖（提交器替身）；`ROOST_MOBA_REAL=1` 并给隔离 Mongo 副本集 `ROOST_DATAENGINE_IT_MONGO_URI` 时，追加两个独立进程停机后冷加载的结算重放（`TestMOBARealSettlementAcrossProcesses`）与活动对局 SIGKILL 后从未投影 WAL 续赛（`TestMOBARealActiveRoundCrashRecovery`）；再加 `ROOST_MOBA_ROUTING=1` 与 `ROOST_DATAENGINE_IT_NATS_URL` 时追加同机五进程真实 NATS 的宿主交接与原服奖励崩溃重投（`TestMOBARealCrossSIDHostRouting`）。三条都登记在根包 `integration_coverage_test.go`，各阶段成功后删除本次唯一库。成功删除临时工程，失败或 `ROOST_MOBA_KEEP=1` 保留；改 `game` 模板、`add dao` / `add entity`、多实体 Sender 生成或 Lockstep 时手动跑。手册见 [MOBA-INTEGRATION](../docs/framework/MOBA-INTEGRATION.md)。

game-demo 生成工程的真实依赖用例：`./scripts/test-demo-generated.sh`（需要隔离环境三个地址变量）用当前生成器 `project new -template game-demo --skip-deps` 并 `replace` 指回本仓，跑模板里的 `TestGuildIDsRealMongo*`（`ROOST_GUILD_IT_MONGO_URI`）、`TestPurchaseDeliveryDurableWinner`（`ROOST_REVIEW_REDIS`；调用方给 `ROOST_REVIEW_CLUSTER` 时含集群子用例），再跑 `codegen/internal/roost` 的 `TestGateGeneratedBusinessConsumer`。`dataengine-env.sh test` 与 `mirror-local.sh test-core`（带私有集群）调用它；`test-dataengine-generated.sh`、`test-remote-generated.sh` 的功能模式也由 `dataengine-env.sh test` 调用。

DataEngine 正式持久化链路：配置隔离 `ROOST_DATAENGINE_IT_MONGO_URI` 后运行 `./scripts/test-dataengine-generated.sh`。脚本用正式 DAO/Entity 生成器构造最小测试工程，三次独立进程使用同一测试库和 WAL，覆盖三种持久化策略、共享实体的四文档交易、错误/panic 回滚、ack 丢失重放与末文档冲突原子回滚。只清理本次生成的 `roost_generated_it_*` 库；本入口不运行 Remote 场景。[恢复验收](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DATAENGINE-RECOVERY-2026-09-24.md)。

DataEngine 真实 Mongo 压测：`bash scripts/perf/dataengine.sh` 复用正式生成工程，默认非 race、GOMAXPROCS=4、32 并发、128 实体、每样本 1000 笔，四种负载 × 三种策略 × 三轮。结果存入 `artifacts/perf/dataengine/<label>/`；每轮逐文档验证并排空 WAL。可通过 `ROOST_PERF_SHAPES="single dual pair hot"`、`ROOST_PERF_POLICIES="async strict pipelined"`、`ROOST_PERF_REQUESTS`、`ROOST_PERF_WRITERS`、`ROOST_PERF_ENTITIES`、`ROOST_PERF_COUNT`、`ROOST_PERF_CPU`、`ROOST_PERF_LABEL` 调整。`ROOST_PERF_RATE=20` 按每秒 20 笔目标节奏发送，未设置时为闭环突发压力；超载时同时观察排空时间和 WAL 积压，不能只看 Request 返回吞吐。


DataEngine 多 DAO 批量优化后的验收与性能对比见
[实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DATAENGINE-BATCH-2026-09-24.md)。正式生成链路现覆盖三种策略下
满额准入拒绝、四 DAO 回滚、排空后再准入。大积压专项增加
`ROOST_DATAENGINE_IT_BACKLOG_MULTI=1`（同时设置 `ROOST_DATAENGINE_IT_BACKLOG=1`），
把 100k WAL 恢复扩展为 10k Entity × 双 DAO；默认仍保留原单 DAO 用例。

Remote 的持久权限、最新故障矩阵和容量复跑见 [2026-09-25 实施报告](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-AUTHORITY-2026-09-25.md)。旧部署必须先停写、排空并迁移；脚本记录当前 Remote 源码摘要，不能拿旧协议长稳结果替代新协议验收。


Remote 诊断可设置 `ROOST_REMOTE_CPU_PROFILE=/绝对路径/cpu.pprof` 和 `ROOST_REMOTE_TRACE=/绝对路径/trace.out`；`scripts/test-remote-generated.sh` 把它们传给 Go test。跟踪会覆盖初始化、预热和计时阶段，诊断结果应与不带 profile 的正式负载分开。长稳仍使用 `scripts/perf/remote.sh` 的 `ROOST_REMOTE_DURATION=30m` / `24h`；当时的实跑范围见 [Remote 统一实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-UNIFIED-2026-09-25.md)。

Remote 阶段定位可设置 `ROOST_REMOTE_STAGE_METRICS=1`，结果包含 Nest 阶段 metrics 和最多 32 个请求错误样本。`ROOST_REMOTE_PROJECTION_WORKERS=1` 提供串行对照，默认使用正式 Projector 的 8 路独立 Entity 投影；`ROOST_REMOTE_RATE=60` 可验证 50 TPS 以上目标。脚本源码摘要包含 DataEngine。最新容量/长稳结果见 [吞吐与超时报告](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-remote-throughput.md)。

Remote 容量阶梯入口：`ROOST_REMOTE_CAPACITY_LABEL=<唯一标签> ROOST_REMOTE_RATES='80 120 160' ROOST_REMOTE_DURATION=120s bash scripts/perf/remote-capacity.sh`。先加载隔离环境 env.sh，沿用 1000 会话、10000 Entity、2 Entity × 2 DAO、strict。每档单独初始化、预热并全量核验，首次失败停止，保留退出码；之后用更细档位缩小区间，最高通过档应延长验证，不能把短测单点当通用上限。

`ROOST_REMOTE_WORKERS` 控制 Nest short 池并发（负载默认 64）；`ROOST_REMOTE_IO_WORKERS` 单独控制 io 池并发（默认跟随前者）；`ROOST_REMOTE_PROJECTION_WORKERS` 控制 DataEngine 独立 Remote 投影并发（默认 8）。容量对比必须同时披露这三项、CPU 和持久化策略。

### Nest 三池（short / long / io）验收

启动 option：`NestOptionWithWorkerPools(short, long, io)`；每池 QueueCap 为整池等待容量（池命名与迁移见 [Nest 三池](../docs/framework/NEST-AWAIT.md)）。
Remote 负载里 `ROOST_REMOTE_WORKERS` 与 `ROOST_REMOTE_FAST_QUEUE`（默认 4096）配 short 池，`ROOST_REMOTE_IO_WORKERS` 与 `ROOST_REMOTE_SLOW_QUEUE`（默认 64）配 io 池，long 池不配置（变量名沿用旧池名，保持复跑命令不变）。Remote 模式仍走正式业务/存储链路。

调度混合基准：`ROOST_PERF_BENCH=BenchmarkMixedShortIO bash scripts/perf/nest.sh`（short 池加 io 池混合；2026-10-14 前名为 BenchmarkMixedFastSlow）。1%/5% 是走 I/O 准备的请求比例，模拟等待 500μs，不能当作 Entity 变化频率或真实数据库 TPS。

io 池并发/队列对比：加载隔离环境后运行 `ROOST_REMOTE_WORKER_LABEL=<唯一标签> bash scripts/perf/remote-workers.sh`。默认固定 short 池 8、投影 8、每档 120 秒，比较 io 池 64/128/256/1024 与等待位 16/64；`ROOST_REMOTE_WORKER_CASES='64:64:120 1024:16:120'` 可指定 io worker:等待位:输入TPS。过载结果保留并继续下一档，环境/预热无结果则停止；以零错误、零丢弃和全量校验通过判定通过，不能只看成功TPS或队列长度。

Remote 资源预算验证新增 `ROOST_REMOTE_WRITE_LIMIT`（正整数，未设使用正式默认 128）、`ROOST_REMOTE_WAL_LIMIT`（正整数，未设保持 WAL 不设记录数上限）。它们独立于 short / io worker 数；历史 4096 预算对照可显式设 `ROOST_REMOTE_WRITE_LIMIT=4096`，前提是 `AsyncFinalizeCapacity` 足够。最新[预算与集中恢复验收](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)披露拒绝和成功 TPS。

`scripts/perf/remote.sh` 的一致性与负载错误分开报告（2026-10-04）：负载有错误时仍做区间核验并写 `result.json.verified`，之后测试再以错误失败；退出码 0 = 核验通过且无错误，3 = 核验通过但负载有错误（输出 `consistency verified; load errors=…`），1 = `.verified` 缺失。`ROOST_REMOTE_REQUEST_TIMEOUT`（缺省 30s）只用于区间核验负对照。口径见 [REMOTE-ACCEPTANCE §负载有错误时的区间核验](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md#负载有错误时的区间核验2026-10-04)。


Sync 长尾诊断：AOI 压测增加 `-trace-capacity=262144`，输出固定内存环形记录的逐批
`trace.jsonl`；随后运行 `python3 scripts/perf/sync-trace.py <sample-N目录>`，关联
`client.json` 的最差超标样本并输出 `outlier-stages.json`。需同时检查覆盖数与客户端
省略样本数。诊断运行含额外编码/文件输出，不能和关闭诊断的延迟门禁混用。
默认无诊断的客户端仍保留原门禁及有界超标样本，分别报告 delta/full 变更延迟。
`recovery_admission_caught_up_ms` 是集中恢复后服务端首次采样到待快照订阅为零的时间，
不是客户端全部恢复时间；零值表示未观测到追平（或没有发起恢复），按 `config` 区分。
最新[实现与复跑记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md)。
`-snapshot-objects` / `-snapshot-bytes` / `-snapshot-per-session` 只限制冷对象创建，
已有对象的 Full 更新不占额度。最新四轮集中恢复中三轮严格门禁通过，一轮冷创建最大
54.283ms，失败样本仍计入原门禁；不能将恢复预算视为全部 Full 更新的带宽上限。

Sync 首次可见与客户端恢复：诊断运行加 `-trace-capacity=262144`，随后执行
`python3 scripts/perf/sync-visibility.py <sample-N目录>`，生成 `visibility-latency.json`。
客户端额外写 `baseline-receipts.json`，每连接最多保留 4096 条 Create/Remove、汇总文件最多输出 1048576 条，
省略数明确报告。离线分析区分新请求首次可见、恢复初始集合收到/取消/未完成，以及在途创建
晚于取消到达；不能把取消算成功或将残缺证据当完整恢复。
诊断还在服务端待快照归零后排空并向恢复客户端发送当前 Interest 集合检查点，客户端逐项
核验后报告 `recovery_client_verified_ms` 与会话数。这是实际恢复完成时间的上界，不是首次
达到一致的精确时刻；检查点暂停输入及诊断开销只存在于诊断运行。
支持本工具的单次 Hold/Ready，不将多次重置或同 session ID 重开混作一个恢复样本。
最终固定预算对比、未采纳的短窗口实验和验收结果以实施报告末节为准。

MMORPG 业务纵切：`bash scripts/test-mmorpg-generated.sh`，公共 CLI 生成 DAO / Entity / Sender 与持久定时器，执行队伍、托管交易、战斗掉落任务、Buff、死亡复活和持久切图的 race 场景；缺省提交器替身。`ROOST_MMORPG_REAL=1` 并提供隔离 `ROOST_DATAENGINE_IT_MONGO_URI` 时增加真实 WAL / Mongo 的两独立进程恢复；固定入口登记在 `integration_coverage_test.go`。成功删除临时工程，失败或 `ROOST_MMORPG_KEEP=1` 保留。

MMORPG 多地图网络接入：`bash scripts/test-mmorpg-scenes.sh`，生成 game-demo 后填业务接法，一个 Manager 配多个 Interest，在真实 TCP 和 Receiver 上校验切图删除 / 全量、held 重试、旧 epoch、重连与字段隐私。`scripts/test-mmorpg-cross-sid.sh` 对已启动的隔离双 sid game-demo 运行真实账号错服拒绝、双向赠礼、邮件重放与失败补偿；资源、端口和命名空间从显式环境变量取得，不启动 / 停止服务、不改 Player sid。[MMORPG 学习与使用手册](../docs/framework/MMORPG-INTEGRATION.md)列出全部参数、业务映射和边界；[SLG 学习与使用手册](../docs/framework/SLG-INTEGRATION.md)独立维护。

MOBA 正式业务消费者：`bash scripts/test-moba-generated.sh`，CLI 生成 Player / MatchResult DAO 与多实体 Sender，结合正式 Match、FrameAssembler、真实 loopback KCP 跑匹配、会话输入、单路模拟、追帧 / 观战 / 分歧及结算 race 场景。`ROOST_MOBA_REAL=1` 与隔离 Mongo URI 追加两个独立进程的文件 WAL / Mongo 结算恢复；活动对局恢复另有边界。`ROOST_MOBA_KEEP=1` 保留成功消费者，失败保留目录和真实模式的唯一库名。[MOBA 学习手册](../docs/framework/MOBA-INTEGRATION.md)列出业务 / 框架映射与使用方式。

SLG 联盟活动联合场景：`bash scripts/test-slg-alliance.sh`，需显式隔离 Redis / NATS。正式 Activity RPC 连独立协调器子进程，验证成员撤权与关系视野、贡献持久意图与重放、冻结阶段屏障、原服奖励义务回执和丢失 ACK 后重投；不含 Player 实际金币到账或 Remote 联盟权威。流程与边界见 [SLG 主线手册](../docs/framework/SLG-INTEGRATION.md#联盟活动主线从准入到原服接收奖励义务)。

SLG 联盟活动 Player 到账扩展：`bash scripts/test-slg-alliance-player.sh`，显式提供隔离 Redis / NATS / Mongo 副本集。CLI 生成 Player DAO/Sender，在同一活动流程 ACK 后读取原服义务，两个 sid 分别经 strict WAL 未投影 SIGKILL、恢复先核对资产再重投、第三进程复核永久回执；build / vet / ID / 生成检查。流程与边界见 [SLG Player 到账](../docs/framework/SLG-INTEGRATION.md#player-到账扩展义务已-ack金币仍能恢复补发)。

SLG 联盟 Remote 模式：`ROOST_SLG_REMOTE=1 bash scripts/test-slg-alliance-player.sh`，沿用 Redis/NATS/Mongo 参数。生成 Managed Remote 实体与 Sender，两个独立 Runtime 显式交接 owner、拒绝旧写者、冷加载后继续同一活动和两服 Player 恢复。所有联盟模式增加编码帧 Receiver 的撤权／held／重连检查，Player 模式增加原服待发义务有界扫描。范围见 [SLG 手册](../docs/framework/SLG-INTEGRATION.md)。
