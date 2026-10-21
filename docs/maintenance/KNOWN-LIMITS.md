# 当前已知限制与验证边界

适用版本：v1.29.0 与 main 上未发版的改动（CHANGELOG `[Unreleased]`）。本文件只记**当前仍有效**的限制、维护者已接受的代价和验证边界；已修复问题的现象、根因、红绿证据与修法见 [CHANGELOG](../../CHANGELOG.md)、[docs/release](../release/) 与 Git 历史。审查发现的问题以 `RV-*` 登记在本文件开头（当前没有未解决的 RV 项，进度见 [COMMIT-REVIEW](COMMIT-REVIEW.md)）。拿不准是否仍有效的条目标“待核对”；v1.29 上没有证据的验证项写“v1.29 未验证”；2026-10-20 维护者批准的发版前补测已在 main `69fb668f`（v1.30.0 候选）上跑完（[夜间补测](../performance/NIGHT-20261020.md)），结论回填到各条与能力表，没跑或没结论的项留在文末 [v1.29 待补测](#v129-待补测)。

需要维护者做设计选择的事项汇总在 [DESIGN-TODO](DESIGN-TODO.md)；[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据；[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本文件是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零 bug 或 100% 行为覆盖；不确定的能力不写成已经支持。

## Nest 与实体定时器

### 按实体的持久定时器（D11）

`roost add timer --entity <Entity>` 与 core 驱动 `framework/nest/entitytimer`（[DESIGN-TODO D11](DESIGN-TODO.md#d11-按实体的持久定时器接入) 方案 B，`a2b4f4d8`、`5581deeb`）。离线语义：框架不追帧，离线期间到期的节点在加载后只触发一次，补算由业务按 `Fire` 参数自己做。用法见 [guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器)，契约见 [impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动)。

- **离线实体不准点**：驱动只索引已加载实例，未加载实体到期不唤醒，下次加载时补触发一次。维护者 2026-10-18 决定不做全局“离线也准点”驱动：必须准点的动作（如到点给离线玩家发邮件）由业务发 Nest 消息给目标实体或服务；不要求准点的在加载后的回调里自己追帧。
- **卸载与已入队 Tick 的窗口**：驱动投递前核对实例，但投递准入之后、执行之前实例被卸载时，Nest 按“发给未加载实体的消息”冷加载它并补触发一次（删档实体加载失败、不触发）。维护者 2026-10-18 决定接受，不给 Nest 加“只投给已加载实体”的发送选项。
- **构建了但没发布的实例**：实例在 `OnInitFinish`（发布之前）向驱动报告；发布失败且带期限时留下一个条目，到期时同 ID 已有别的实例则丢弃，没有实例则按加载中投递（Nest 冷加载并补触发）。条目只在到期前占内存，不重复投递。
- **Remote 托管与 noPersist 实体不支持**：add 时拒绝。迁移中的到期语义未定义，要支持需另行设计（D11 方案 C 方向）。
- **去掉 `timers` 条目后生成部分不自动删除**：需手工删 `*_component_gen.go`、节点类型、Tick handler 及 DAO / 实体里的两处字段；维护者 2026-10-18 决定维持现状。
- **装了定时器的服务多一个 Mod**：`wiring/entitytimer` Mod 让该服务的 `shutdown.total_timeout` 多 3s（每个 Mod 的保底）；维护者 2026-10-18 决定接受，不改 app 保底规则。game-demo 手写接线、不装这个 Mod。
- **game-demo 手写驱动在 `Service.Init` 才接上业务时钟**：生成工程的 Mod 在 Provide 阶段绑时钟；demo 的 `startWorldTimers` 在 `Service.Init`、`EnsureWorld` 之前调用，“World 第一次加载在驱动接上之后”靠 demo 自己的启动顺序（写闸门 `require_write_gate: true` 在 `EnsureWorld` 之后才发布），不是框架保证；更早加载的 World 按系统时钟记加载时刻（`time.logic_offset` 为 0 时两者相同）。
- **端到端用例用的是内存权威**：`TestEntityTimerGeneratedProjectEndToEnd` 以不落库 committer + 内存 loader 代替 DataEngine + Mongo；循环的其余计时分支（MaxWait 重读时钟、准入失败重试、Redeliver）由它与 entitytimer 包测试覆盖。真实 WAL / Mongo 下的定时器往返由 7 个游戏夹具的 SIGKILL 崩溃恢复与 MMORPG 的正常停机夹具覆盖，全部经驱动投递循环（`Driver.Start`）、不手发 Tick，见 [游戏接入验收](#游戏接入验收)。

### 实体心跳（D20 第 1 期）

`gameplay/heartbeat` + `wiring/heartbeat`（[DESIGN-TODO D20](DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制)，维护者 2026-10-21 定方案）。用法见 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)，契约见 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)。

- **还没有业务接入**：P1 只有框架与 Mod。生成器不装 `wiring/heartbeat`、没有 `roost add` 命令，game-demo、sync-aoi / nest-game 压测仍是全局循环；P2（MapBasic / Move 组件、actionflow 适配）、P3（game-demo）、P4（压测改心跳驱动）未做。
- **投递是单目标 Dispatch**：压测 50 万次/秒约 4.25 核（[HB-STRUCTURES](../performance/HB-STRUCTURES-20261021.md)），CPU 约六成是 worker 每条消息停放 / 唤醒。Nest 批量入队（方案 D）原型另行进行，达标后只换投递层（`deliverer`）。本轮只做 5 万实体 1s 档的功能冒烟（10 万拍全部执行、零跳拍），**没有正式性能测量**。
- **卸载窗口**：投递准入之后、执行之前被卸载的实体可能被 Nest 冷加载一次（与发给它的任何消息相同），这一拍在新实例上按过期丢弃；维护者决定 ⑨ 接受。
- **卡住兜底会多压消息**：在途 10s 未执行（同一登记连续卡住时 20s、40s、80s）就重投一次。拍没丢、只是 worker 排队超过阈值时，每个登记多出对数级份消息（只有一份执行回调，其余按 `duplicate` 丢弃）。-race 下 5 万实体 1s 档冒烟（每秒 5 万拍超过 -race 吞吐）实测到这种放大，所以 -race 冒烟降到 2000 实体。
- **Remote 托管实体每拍一次 Remote 写准入**：消息经 I/O 阶段 `PrepareRemoteWriteBatch`，比普通实体贵；档位宜低。非拥有方（`Unknown` 状态、权威指向别人）的拍被准入拒绝后按卡住兜底重试（10s 起翻倍）。只有独占拥有（`LocalOwned`）才执行。所有权迁移的测试用的是模拟 Remote 写准入的假 Manager（按 `remoteentity.Manager.beginWrite` 与 `TransferRemoteOwnership` 的写门 / 状态顺序），没有真实 Mongo / Redis 下的迁移演练。
- **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21）：共享模式下各写者实例都是 `Shared`，分不出谁该跳。共享是运行期状态、不是种类声明，所以登记时只拦得住“当时已在共享”的实例（`Register` / `Start` 返回 `ErrHeartbeatRemoteShared`）；登记后才进入共享的（拥有方 `EnterRemoteSharedMode`，或第二个写者加载时是 `Unknown`、第一拍准入才转 `Shared`），由下一拍在实体锁内停掉，计 `heartbeat.stopped_total{reason="remote_shared"}` 并打 Warn。边界：① 这一拍要走一次共享模式写准入（分布式锁），回调不执行；② 回到独占（`LeaveRemoteSharedMode`）后不自动重启，业务需要时自己 `Start`；③ 测试用的是模拟写准入的假 Manager，没有真实 Redis 分布式锁下的演练。共享实体的周期逻辑由业务层驱动（例如负责它的服务按周期发 Nest 消息）。
- **快池准入满时的日志**：心跳本身按跳拍处理并限频 Warn，但 Nest 对每条被拒的异步投递另打一条 `async dispatch failed`（不限频，Nest 现有行为，本期不改 Nest）。
- **`Elapsed` 上限**：取 `max(1s, 档位)`，不是一律 1 秒（2s 及以上的档每拍都会少算），待维护者确认。
- **装了心跳的服务多一个 Mod**：与 entitytimer 相同，`shutdown.total_timeout` 多 3s 保底。

### Skill Runtime

不参与 Nest 回滚；Host 副作用和确定性由业务契约保证。Host 能力、重入、阻塞、恢复和回放测试仍待补。

## DataEngine、Remote 与 Saga

### 删档后同 ID 重建（D10）

GetOrCreate 路径已守住（维护者 2026-10-17 决定 [D10](DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，`572d87c2`）：冷加载对删过档返回 `entity.ErrAuthorityEntityDeleted`（不满足 not found），生成 GetOrCreate 不重建，发往该 ID 的 Nest 请求回复 `entity_deleted = 3`。

- **约束（直接 Create）**：handler 内或 lifecycle `Create` 不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID，也不要对存了但没加载的 ID 直接 Create；`EntityManager` 的 ID 生成器同样必须永不重复。违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来。直接 Create 的保护按维护者决定不做。
- **删档审计**：“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记。
- 回归：`TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`。

**删档后同 ID 重建：违反约束或修复前写下的冲突记录**。这类记录不会被跳过或放宽：启动投影恢复报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来；框架不自动删记录、不跳过，也不提供“跳过一条记录”的入口。人工处置（只在确认是这类记录时）：

1. 停进程，保留 WAL 目录与 Mongo 备份。
2. 按错误给出的事务 ID 与 `<库>/<集合>/<_id>`，在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份只读副本留证（不推进 checkpoint、不跳过记录）。
3. 线上尚未部署（维护者 2026-10-06）：停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本。上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码。见[维护手册 §4](README.md#4-故障处理)。

### 运行期改档与单实体运维（D16）

框架只提供与业务字段无关的 `entity.kick`（踢下线）与 `entity.unload`（排空投影后卸载），**不提供改档与恢复**（维护者 2026-10-20 决定 C，[DESIGN-TODO D16](DESIGN-TODO.md#d16-单个实体--玩家的运维入口)，用法见 [guide/03 §6](../framework/guide/03-dataengine.md#单实体运维命令d16)）。`entity.unload` 之后任何访问都会立刻重新加载，不能当作改库的窗口。运行期直接改权威集合的结果按设计是确定的：只改字段时下一次投影覆盖改动；改 `_version`（恢复旧备份）时投影 fatal、进程 fence、重启起不来，直到人工处理（[维护手册 §4](README.md#4-故障处理)）。在线改档写成业务 GM handler，整档恢复走停服排空后的离线写回（[guide/03 §3](../framework/guide/03-dataengine.md#3-投影与幂等)）。

### DAO 持久键（D13）

定义里字段的 `bson:"x"` 就是持久键（缺省 snake_case），库里有、DAO 未声明的键不读入，下一次整文档写回即删除（维护者 2026-10-19，[DESIGN-TODO D13](DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）。

- **业务约束**（[SCHEMA-MIGRATION §1](../framework/SCHEMA-MIGRATION.md)）：改名 / 删字段前要么保留原 `bson` 名，要么先离线迁移再提升 schema，否则旧数据不再加载、写回即丢失。
- **剩余风险**：`roost generate` 与 `RestorePersisted` 的 WARN 不阻断，忽略告警时旧值丢失由业务约束承担；单字段补丁不碰旧键，它们留在库里但不再被读取，清理要一次性离线迁移；生成期基线只在工作树里，删掉生成物或换机器重新生成看不到上一次的形状，只剩加载期 WARN。

### Remote

- **Remote WAL级Rejected处理**（`38b710a2`）：Durability 1/2/3 的正式链路不产生 Remote Rejected，strict / pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`；tracker 的 Rejected 收尾只剩 Durability 0 来源。守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore / MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict / pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`）。
- **Remote 请求 struct 与 Sender**：handler 参数用带 `remote:"..."` 字段的请求 struct 时，struct 必须留在 handler 包（访问器生成在这里），sender / syncsender 子包又看不见它，所以这种 handler 生成失败（生成器给专门报错、不建议移走类型）。维护者 2026-10-16 决定先写清限制、不改生成结构；需要时再设计让 sender 携带这种 struct，或允许单个 handler 不生成 sender。

### Saga

- **原生步骤的多进程分片**：准入与转交由业务写（框架提供 `WithAdmit` 与 `RunHandedOver`，生成的 `RegisterSteps` 注释点名要补），见 [DESIGN-TODO D12](DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持)。
- **原生步骤单进程容量**：第一瓶颈是 DataEngine 投影器——带回执 / effect 的记录（`projector_replay.go` `isLocalBatchRecord`）逐条一次投影事务，checkpoint 已与普通记录按 `CheckpointRecords` / `CheckpointInterval` 合并；正向每笔 2 条、全补偿 3 条。等回执在后台（`StepConsumerConfig.ReceiptWaiters`），代价是同 durable 上同一实体的前后命令可能撞实体屏障（交还租约、退避 nak）。维护者 2026-10-18 决定 A（接受并文档化，量大按 sid 分片多进程、原生步骤加 `WithAdmit`）；第三档 B（合批带回执 / effect 的记录）与 C（分盘）不做；checkpoint 缺省间隔保持 20 ms（维护者 2026-10-19 方案 A），更高吞吐可调 `dataengine.projection.checkpoint_interval`，代价是崩溃后重放窗口与未确认额度变长（重放仍幂等）。容量数字见下方能力表（2026-10-20 夜间 15 分钟复测）与 [guide/06 §5](../framework/guide/06-saga.md#5-接入与运维)。
- **发起方查结果**：生成的 `Lookup` / `saga.Engine.GetByBusinessKey` 按 `(Type, BusinessKey)` 查；终态通知没有默认实现，需要时由最后一步 / 补偿步骤自己发 effect（SAGA.md）。

## Sync、玩家接入与 Lockstep

### 自身视图（self_sync）与 Sync 慢消费者规则

`access.player.self_sync: true` 让玩家经 Entity Sync 收到自己的 Player（DESIGN-TODO D6，`policy.SelfView` + `wiring/selfsync`）；推送在活着的连接上被拒即关闭该玩家连接，进程驻留预算耗尽整轮重试（维护者批准“A + A'”，[impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)）。编号沿用 `67807950` / `cf6ad085` 之后的口径：

1. 一个 `SubjectSyncState` 只登记在一个 Manager，第二次登记返回 `entitysync.ErrSubjectRegisteredElsewhere`（2026-10-20 起）。game-demo 打开 self_sync 时启动在 `service game init` 失败，自建 Manager 在登记时被拒：该组合本身仍不支持。
2. 一帧上限取接入层上限（嵌入 TCP 读 `player_access.tcp.max_payload_bytes`；独立 Gate 由 `IngressMod.WithSelfSync` 交给 Game Ingress 的 `Config.MaxPayloadBytes`；`selfsync.Config.MaxFrameBytes` 只能再收紧）。**独立 Gate 下 Game 与 Gate 两个进程的 `MaxPayloadBytes` 相等靠部署保证**（Gate 侧 `GateMod.Init` 只检查本进程 TCP 与内部上限相同）。维护者 2026-10-20 决定暂不加跨进程检查：要检查就得在握手时互相报上限，是协议改动；继续由部署保证两边配置相同。
3. 玩家实体的帧在 Manager 内编码失败（单个实体包超过 `frame.Limits` 硬上限、或超过第 2 条的上限）时 Manager 不经传输丢会话，连接不关，玩家在线却没有自身视图直到重新登录；由 `entitysync.self_view.stranded_players` 暴露，game-demo 场景同样。维护者 2026-10-17 决定 B：不关连接，靠指标告警。
4. Game 进程出站驻留预算（Game Ingress `outbound.resident_bytes`）持续耗尽时 Sync 每轮在第一个被拒会话处整轮重试：排在后面的会话本轮不推送，已准入的会话下一轮改收全量（`requireSnapshotsAfterRetry`），带宽放大、延迟上升；发不出去的绑定在 `outbound.max_age` 到期时被关闭、释放预算；业务提交不受影响。嵌入 TCP 的进程预算同一规则，但没有 `max_age`（见下节）。按发送能力配预算并看 `player.sync.push_refused.total{outcome="retried"}`；需要时可让 Manager 在 RetryLater 时跳过被拒会话。
5. Gate 侧自己的驻留预算耗尽仍关闭连接，没有 Game 侧的整轮重试例外；需要时 Gate 回 NotAdmitted、由 Game 侧按预算例外处理。
6. 不提供 C# 的 subject 载荷解码样例。
7. 嵌入 TCP + self_sync 重档 15 分钟（2026-10-20 夜间，[夜间补测](../performance/NIGHT-20261020.md) §2）通过：1000 玩家每秒约 1000 帧自身视图，`push_refused` retried / closed 0、滞留玩家 0，提交→客户端 P99 1.43ms。第 4 条的整轮重试放大在该负载下没有被触发，**没有测到不等于不存在**（要测需把预算压到触发点）；独立 Gate + self_sync 的重档夹具没有。

**self_sync 默认 packer 的代价**：开关打开且 marker 没写 `subjectPacker=` 时生成器生成 `player_sync_packer_gen.go`，标了 sync 的字段默认全部对玩家本人可见，需要裁剪的写自己的 packer；marker 写了生成名 `NewPlayerSyncPacker` 时按业务 packer 处理、不生成；默认 packer 只发一个 DAO，多 DAO 的 Player 仍要自己写（[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)）。

### 嵌入 TCP 出站

每连接有界出站队列 + 唯一写协程：上限 1024 帧（维护者 2026-10-16 决定）与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计，不开放配置键；停读客户端被关闭、不拖住场景 Sync Flush / Lockstep Tick。2026-10-20 起有全进程驻留预算 `player_access.tcp.outbound.resident_bytes`（`cf6ad085`，与独立 Gate `outbound.resident_bytes` 同口径），放不下的一帧当场被拒（`gateway.ErrResidentBudgetExhausted`，计 `player_tcp_outbound_budget_exhausted_total`），不关闭连接、不等预算；Entity Sync 当整轮重试，PB 推送返回错误由业务处理。说明见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)。

维护者 2026-10-20 决定（均按推荐）：

- **Lockstep 实时帧被预算拒**：按数据报丢失处理，不断开连接，靠冗余帧或追帧补回（A）；追帧页被拒计一次追帧失败。
- **读循环回包与心跳 ACK**：计入预算但不拒绝（每连接至多一帧在等），驻留量最多可超出预算 `max_connections × (max_payload_bytes + 16)`（A）；读循环直写快路径的那一帧不计。
- **驻留字节 Gauge 与按帧龄提前关闭停读连接**：先不做，等重档压测看 `player_tcp_outbound_budget_exhausted_total` 再定。

剩余边界：

- 缺省 256 MiB 是按 Gate 缺省 128 MiB / 4096 连接折算到 10000 连接取整。2026-10-20 夜间嵌入 TCP 重档 15 分钟（1000 连接、场景 Sync 4198 万条 delta、每秒 1 万条回包，[夜间补测](../performance/NIGHT-20261020.md) §2）`player_tcp_outbound_budget_exhausted_total` 增量 0、溢出 / 推送关连接 / 写错误全 0，正常负载下不触发；停读客户端或更高推送量下是否够用仍未测（没有驻留 Gauge，峰值取不到）。上面“驻留字节 Gauge / 按帧龄关闭”的决定所等的数据就是这一份：本负载下拒绝为 0，不构成要做的证据。
- 停读连接占着的预算要等它撞上每连接上限被关或 `write_timeout`（默认 5s）写超时才归还，期间健康连接的推送可能被拒（Sync 延迟上升，Lockstep 实时帧靠冗余补）；没有 Gate 那样的 `max_age`。
- 只有拒绝计数，没有驻留字节 Gauge。
- 推送入队多一次 payload 复制，每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。
- 证据只有包内回归（含关连接与写协程并发退出时恰好归还一次）、自身视图与 Lockstep 的真实回环 TCP 集成用例与生成 demo 场景用例。

### Lockstep

- **已修复（2026-10-20）：`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted` 在全仓 `go test ./...` 下偶发失败**（`tcp_test.go:513`：`Tick after the budget was returned = … frame of 16455 bytes, 261344 of 262144 resident`）。负载下（`-race -count=200 -cpu=1,2,8`，同时循环跑 nest / network / dataengine 的 race 测试）复现 97/600。根因是产品缺陷：嵌入 TCP 的 `serveConnection` 先发布会话关闭事件（`unregisterSession` 的 defer 在 `closeConnectionSession` 之前执行），再关闭 socket、等写协程退出，而连接占的出站驻留在写协程退出时才归还（`releaseOutbound`），与 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)“等写协程退出，再……发布关闭事件”不符；用例收到 filler 的关闭事件就 Tick，正好落在这个窗口里。修法与红绿见 CHANGELOG `[Unreleased]`“嵌入 TCP 会话关闭事件改在写协程退出之后发布”。同时把该用例与 `wiring/selfsync` 预算用例的“停一会儿再探”换成确定的占满方式（一帧 16 MiB、预算恰好一帧）。修后同一负载 `-count=500` 0/1500。
- **Lockstep历史**：默认自动保留 3600 帧（60Hz 60 秒 / 30Hz 120 秒，D1 方案 C，`a6376934`）。越界按 `MatchConfig.Recovery` 区分（D1 方案 B，维护者 2026-10-20 批准，P1～P5 已实施，[设计](FEATURE-2026-10-20-lockstep-snapshot-recovery.md)）：配了 `Snapshots.Provider`（零值即 auto）或显式 snapshot 时，越界改发业务快照、客户端恢复后续帧，只有快照路线最终失败（取不到、过大、钉住过期）才发 C9 v2 通知让客户端终止；catchup（没配快照来源时的缺省）仍是越界明确失败、通知客户端终止、不自动重同步。快照内容来自业务的服务器参考模拟，框架不提供默认模拟、不替业务持久化检查点。本地 20000 帧周期性快照恢复与默认窗口长局有界已验证；按业务帧率、断线时长与快照大小核对窗口和预算未做。见 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)、[SYNC-OPTIMIZATION](../framework/SYNC-OPTIMIZATION.md)。
- **Lockstep TCP 追帧**（`640e9fac`，维护者 2026-10-16 方向 A）：`lockstep.TCPSender` 实时帧 `SendDatagram` 只准入，追帧页 `SendReliable` 经 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出，追帧随链路放慢、积压只有在途一页，`CatchupSendTimeout` 生效。等写出只在 Match 的 `launchSend` goroutine 上；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。限速链路上：快照分块与快照之后的续帧页经同一等写出机制在 toxiproxy 256 KB/s、64 KB/s 下演练过（2026-10-20，main 代码，1 MiB 与 4 MiB 都恢复、连接不断、耗时与“大小 / 速率”对应，[演练](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）；纯追帧页 2026-10-20 夜间也在 toxiproxy 256 KB/s、64 KB/s 下演练过（[夜间补测](../performance/NIGHT-20261020.md) §10）：客户端回到头部的时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断，64 KB/s 下一页超过 2s 发送时限后重发一次；服务端追帧结束早于客户端收完（socket 发送缓冲），同快照演练。见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)。
- **Lockstep 追帧链路背压（独立 Gate）**：维护者 2026-10-20 决定暂不做。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，nil 只表示 Game 本地准入：Game 侧队列满返回 `ErrReliableBackpressure`（每 Tick 计一次追帧失败，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时关闭连接；追帧与快照都不随链路放慢。快照按 `UnpacedBytesPerTick`（缺省 16 KiB/Tick，30Hz 约 492 KB/s）固定节流；2026-10-20 toxiproxy 实测（30Hz、缺省参数，[演练 §3](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md#3-独立-gate63-风险-1-实测成立)）：64 KB/s × 1 MiB、256 KB/s × 4 MiB 每次连接都在同一位置被 Gate 断开、重连后从头重传，三次都未恢复；256 KB/s × 1 MiB 恢复（低于预算的超出部分被缓冲吸收）。是否断开取决于快照大小、每 Tick 预算与链路速度三者；断开发生在 Gate 写 socket 的期限到期（`writev ... i/o timeout`，`gate.channel.failure_total{phase="socket"}`），不是 `max_age` 积压规则，具体是 5s `write_timeout` 还是单帧发送预算先到期**尚未确认**（按代码缺省配置下单帧预算 ≤ `outbound.send_timeout` 250ms 更早，日志未区分）。2026-10-20 起恢复中被 Detach 计 `lockstep.snapshot.failed.total{reason="detached"}`，这个循环可见。配置建议见[设计 §6.3](FEATURE-2026-10-20-lockstep-snapshot-recovery.md#63-独立-gate只准入时的行为与风险)。纯追帧页在独立 Gate 慢链路上的影响未实测。要做到随链路放慢，Gate 需要在写出 socket 后回执给 Game，这是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。生成 game-demo 在独立 Gate 部署（`accessplayertcp.NewIngressMod`）下拿不到 `TCPRuntime`，战斗的追帧页同样只准入（`battleLinks`）。
- **Lockstep 快照恢复的剩余边界**（2026-10-20）：① MOBA 持久对局 `Room` 已接快照来源（2026-10-20，`RecoverRoomWithSnapshots` + 已提交门 `MatchConfig.CommittedFrame`，[MOBA §5.7](../framework/MOBA-INTEGRATION.md#57-重连宿主代际与交接)），替身权威下有 KCP 与 Nest 两条回归，真实 strict WAL / Mongo 下有 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复的演练（`TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL`，下行是记录器、不经 KCP）；真实 WAL 的提交延迟分布、宿主交接与快照恢复同时发生、宿主与权威分属不同进程时的快照恢复未测；已提交门要求宿主提交落后切帧不超过 `RedundancyDepth - 1` 帧，否则恢复中的会话一直经可靠通道逐页跟随、不切回实时（Room 每 Tick 同步提交，不受影响）；② C# 的 `SHA256.Create` 与 restore 路径只在本机 .NET 10 测试过，Unity 编辑器 / IL2CPP 未实测；③ 服务端“写出”只到本机 socket 发送缓冲（嵌入 TCP）或 Game 本地准入（独立 Gate）：服务端认为恢复结束时慢链路客户端可能还差 1 MB 以上，之后的实时帧排在后面，恢复后头几秒延迟等于排空时间（演练推断，未单独测量缓冲）；④ 快照不续传：断开重连后从头重传，独立 Gate 慢链路上会循环（上条）；⑤ Provider 不响应 ctx 时 `FetchTimeout` 后可能再发起一次，“每局至多一次在途”不成立；⑥ 宿主手动 `TrimHistory` 越过钉住点时快照作废、按失败计，但不计 `failed` 指标；⑦ 真实移动网络、跨机与目标负载下的快照恢复、快照大小分布与窗口取值未测；⑧ 客户端上传快照（多数一致）、增量 / 压缩快照、检查点持久化与宿主崩溃续赛未做（设计 §13）；game-demo 战斗未接（§12 第 13 条）。
- **game-demo 战斗经 `lockstep.TCPSender`**（维护者 2026-10-20 决定）：生成 demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`）两条通道改经 `TCPSender`，嵌入 TCP（生成默认）下追帧页经 `TCPRuntime.PushLockstepSessionWritten` 等写出，与上面嵌入 TCP 追帧同一规则；回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（生成工程 `go test ./internal/service/game`）。线格式随之变化：10102 推送的是 Lockstep 包本身（RS v2 Lockstep 标志、不带 PB 包装与 battle id），按连接（接入层 SessionID）寻址；座位在它第一条 BattleInput 到达的连接上加入，没说过话的座位收不到帧，晚加入的座位靠追帧补齐。一条连接同时只打一场战斗（帧不带 battle id）。限速链路与真实进程下的 demo 战斗未演练。

### 广播与 AOI

- **SyncBus投递**：有限次数、保留期限及容量，不保证绝不丢；定位为默认可丢广播（维护者 2026-10-15）。需要最终一致的业务自建快照 / 对账闭环，不能仅测重试成功；兜底写法见 [guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)；框架级对账为下一阶段候选。
- **policy Drain**：等待 retry 和已承担事实；永久拒绝仍需修关系或 ctx 超时，Drain 不放弃条目，超时返回 `*DrainStalledError`（列出卡住条目、原因与等待时长）并计数（D3，维护者 2026-10-15 决定 A，`b86a1286`）。“放弃出口”（D3 方案 B）需先梳理永久拒绝的判定来源。
- **AOICluster 容量抖动**：Cluster 的 `MaxVisible` 每次移动后严格等于候选内最近 N 个，容量层没有滞回，两个目标在第 N 名附近来回时每次移动都 Leave/Enter（`BenchmarkClusterFlushRandomWalkCapped` Enter +15.6%）。维护者 2026-10-17 决定不加滞回：抖动只在候选数超过 N 时出现，不设 `MaxVisible` 或把 N 设到常规候选数之上即可，容量只作人群聚集时的带宽保护阀。单区域 AOI 不受影响；Cluster 目前没有生产装配调用方。见 [INTEREST-BLOCK-AOI](../framework/INTEREST-BLOCK-AOI.md)。
- **AOI 视野（D14 → D19，2026-10-20 实施）**：① 视野变大时新进入范围的目标一次性订阅、各下发全量，峰值受 `MaxVisible`（按视野计）与 Manager 快照预算约束，没有按玩家单独的预算。② 改视野、视野操作都没有排队版（维护者决定），事务内决定的视野变化（入盟、放眼、组队、Buff 改视野）由业务在提交之后调用。③ 一个玩家挂多个视野时，它持有的目标数受 Manager `Limits.MaxObjects`（缺省 100，也是每帧对象上限）约束，超出由 Manager 拒绝（重试表与 Drain 诊断可见），按玩法调大；`scripts/perf/sync-aoi` 的多视野场景按 `visible × (1 + views) + alliance` 放宽。④ 共享视野的扇出：一次变化的成本 = 变化量 × 挂接人数，由 `MaxPlayersPerView`（缺省 4096）约束；`BenchmarkRelationViewMemberChange` 冒烟（50 次，未正式测量）1024 人挂接时换一个成员约 0.2 到 1 ms（只含登记表与扇出，不含 Manager）；2026-10-20 夜间（[夜间补测](../performance/NIGHT-20261020.md) §14'）：`BenchmarkRelationViewMemberChange` 正式 10 样本 1024 人挂接、1024 成员时约 0.78ms；sync-aoi 共享关系视野 100 / 1000 人（可见约 143 / 1042）Nest 与 direct 都与单视野同量级；每玩家 4 个额外空间视野（可见约 244）Nest 模式改值→客户端 P99 78ms（direct 49.5ms）；每玩家 16 个额外空间视野（1000 玩家共 1.7 万个空间观察者，可见约 827）时 Nest 模式失守（P99 350～376ms、帧数 38%），**2026-10-21 已定位并修复**（[夜间补测 §14'.2a](../performance/NIGHT-20261020.md#142a-根因与修复2026-10-21)）：根因一是 AOI 批次里每个目标移动都让旧、新块上的全部观察者完整重评（改为只评估移动过的目标，结果与完整重评逐项相同）；根因二是排队事实在 Flush 的政策阶段一次性应用，AOI 全部挤进 Flush（改为每个 Interest 一个应用线程分批消费，Flush 只结算；2026-10-21 起这是 ApplyAsync，缺省 ApplySync 在政策阶段同步应用并限制视野数）。修后同负载 Nest P99 97～141ms、帧数 98～100%、Flush 30ms（政策阶段 7ms），direct 57～70ms。**剩余边界（按两种应用模式，维护者 2026-10-21 决定由业务选，[DESIGN-TODO D19“两种应用模式”](DESIGN-TODO.md#两种应用模式2026-10-21维护者决定)）**：① **ApplySync（缺省）**每玩家视野不超过主视野 + 8（含关系视野），超过拒绝构造；AOI 在 Flush 的政策阶段，原场景（1000 玩家、可见每视野约 50）Flush 均值 4 / 8 个额外视野时 15 / 25～27 ms，8 视野约半个周期；玩家更多、更密或移动比例更高时同样 8 个视野 Flush 更长，没有测，超过周期会丢 tick（16 视野诊断构建 46～47 ms 已开始丢）。② **ApplyAsync** 不限视野数，Flush 只结算（16 视野 35 ms），比 direct 多约一个同步周期——Manager 按自己的周期结算，截止前几毫秒才应用完的事实要等下一周期；维护者决定接受（选 C：不加“Flush 前等应用线程”、不做周期对齐）。16 视野 ApplyAsync P99 130～138 ms，direct 69～90 ms。改值集中在周期末尾到达的负载在 ApplyAsync 下尾部会更差，没有测。③ 本夹具里 ApplySync 的测得延迟没有比 ApplyAsync 低（4 视野 P99 65 对 59 ms，8 视野交错），它的好处是“提交后下一次 Flush 必定可见、没有额外线程”。④ 新的小块小视野场景（4000×4000、块 20、视野 30、5 万实体，每 tick 500 次移动）+4 / +8 视野两种模式 P99 53～56 ms、direct 50 ms，Flush 不超过 7 ms。⑤ 新订阅全量 P99 的夹具口径已修（2026-10-21）：全量只计本 tick 改的值，修前会把全量里旧改值的年龄算进去（确定场景 2000 ms → 0）；代价是跨 tick 才送到的全量不计入，全量 P99 偏乐观。⑥ 夹具是内存提交（`DurabilityMemory`），strict / pipelined 的 WAL 等待与排队事实就绪的关系没有测。 已写进 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)“已测规模与边界”与 CHANGELOG。真正的大联盟（上千人互相可见、百万级 pair）与目标密度的多视野仍没有压测。⑤ 包含关系按 `ViewSets` 字段判断；不配 `ViewSets` 时不知道字段，不同层级一律取并集身份，手写 packer 若不经 `SyncViewSet.Lookup` 解析就要自己认 `SyncProfile.Layers()`。⑥ 层级并集只在一个 Interest 内部合并；同一 pair 同时被 Interest 与 Group / Direct / SelfView 等其他政策持有时，Manager 仍按优先级取一个视图（未改，见 [DESIGN-TODO D19](DESIGN-TODO.md#d19-视野view逻辑-id空间--关系两类玩家与视野多对多) 实施结果）。⑦ 关系视野不排除挂接玩家自己的实体（联盟成员里有自己时，自己经 self 与联盟两个层级看见自己，按包含取高或取并集）。⑧ D14 / D19 前后对照（2026-10-20 夜间，P0 `49897751` / P1 `08432d4e` / P2 `69fb668f` 同机交替 10 轮）：`ClusterFlushRandomWalk` +3.5%（D14 引入，D19 不变），其余 AOI / Cluster 基准与分配无差异，`Relation*` 旧基准在 P2 上已删除（不可比）；sync-aoi 单视野三点无差异。见 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)、[impl/04-sync §2E](../framework/impl/04-sync.md#2e-interest-的视野d19在-d14-上扩展)。

## 生成器、配置与工具

- **DAO 库名映射**（[D9](DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A）：`db=` 是逻辑库名，物理库由 `dataengine.databases` / `WithDatabaseResolver` 在启动时冻结。限制：①改映射等于搬数据，框架只在 WAL 还有未投影记录时拒绝启动（`ErrDatabaseMappingChanged`），不复制数据；排空后改了映射却没迁移，新库是空的，冷加载得到“从未创建”，GetOrCreate 会新建玩家——步骤见 [PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)。②声明了却没有映射的逻辑库物理名就是逻辑名本身，只在启动日志告警（生成配置总写 `main` 的映射，手写配置删掉它会落到名为 `main` 的库）；这条回退只在有表时存在；根本没有表时 DataEngine 投影存储（`MongoStoreConfig.Databases` 为 nil）与 Remote 存储（没绑定）同一条规则：读写带库名的 DAO 文档返回 `dataengine.ErrDatabaseMapNotBound`，两边不再不对称（维护者 2026-10-21）；`SetRemoteProjection` 把 DataEngine 的表自动绑给 Remote 存储，自建装配只给 DataEngine 一张表。剩余边界：`NewMongoStore` 只在进程有声明的逻辑库时构造即拒绝，手写 DAO（没经生成器声明）缺表要到第一次投影才报错，那时记录已进 WAL，投影器反复重试这条记录（`ProjectionFailures` 增长、后面的记录不投影），补上表重启后照常投影（WAL 目录记下的上次映射为空，不触发 `ErrDatabaseMappingChanged`）。③不同进程（多个 sid、跨进程写 Remote 实体）的映射是否一致不校验。④二进制一个逻辑库都没声明（工程还没有 DAO）时，配置里的逻辑名无从做拼写检查，只告警。⑤工具经 `LoadDatabaseMapFile` 读同一份配置，但不会自动执行进程里注册的回调，用了回调的部署要给工具传同一个函数或用 `-db` 指定物理库。⑥根包守卫只看同一行里交给 `.Database(` 的参数，先赋给变量再传入的写法看不到，由各解析点的行为测试兜底。⑦回调与配置对同一逻辑库给出不同物理名时拒绝启动（同一逻辑库只能有一个来源，维护者 2026-10-20），但回调只在 Init 调用，`--check-config` / doctor 不执行回调，这类冲突要到进程启动才发现；工具传入回调时（⑤）按同一规则拒绝。
- **doctor `config-sections`**：配置文件生成后归应用所有，`add` 命令只在让服务多出 Mod 的那一刻追加缺失段；在 add 命令之外手改 `roost.yaml` 增加 Mod 后 `project sync` 不补（维护者 2026-10-17 决定 B），由 doctor 只读检查 `config-sections:<service>` 报 WARN 并给出可追加的文本。限制：业务有意删掉的段同样报 WARN，没有豁免开关（可忽略或留空段如 `saga: {}`）；“有没有”只看行首 `<key>:`，段里缺键、值形态不对不报（归 `config-schema`）；Secret 示例结构认不出（没有恰好一个 `config.yaml: |` 块）时只报“未检查”。
- **`roost project next` 的文件名约定**：first-business 修复提示按工程现状填名字，判断“缺协议 / 缺 endpoint”沿用 doctor 的文件名约定（`game/handler/<snake>.go` ↔ `protocol/def/<snake>.go` ↔ `game/controllers/<域>/<snake>.go`），协议名与 handler 名不同的工程仍会落到第一个可用名字或示例名。
- **endpoint 服务器决定的参数**：只识别业务时间这一类约定名（`nowUnix` / `nowUnixMilli`，`int64`，由 `app.BusinessClock(controller.Registry()).Now()` 填入）；其他服务器决定的值（玩家状态、配置）在 handler 里读，没有 `--server-param` 一类的通用标记。
- **Saga / Remote / Skill 的 doctor 检查**：`remote-runtime` 在带 Nest 的服务部分缺 `remote_entity` 时只 WARN（实体由哪个服务写静态上看不出）；技能目录由生成的 `catalog_test.go` 与 doctor `skills:catalog` 检查，服务 Init 里调 `CompileAll` 归业务。
- **configdata 重载内容未变也递增版本**（维护者 2026-10-18 决定 B）：照常发布、递增版本，回答 / 日志 / 指标标明 `changed=false`（`configdata.reload.total{result="unchanged"}`）；理由是监听者依赖每次重载都执行（如开关表用重载恢复表值）。版本号不是内容版本，内容是否变化看 `changed` / `hash`。
- **文档提交号门禁**（`TestDocumentedCommitsAreReachableFromHEAD`）：浅克隆（CI 默认 depth=1）整条跳过，只在本地完整检出与 release 工作流（fetch-depth: 0）生效；纯数字短 SHA 与不带反引号的提交号不检查；只查“在 HEAD 历史里”，不查是不是文档所说的改动。分支上的文档引用本分支提交时 rebase 后会红：引用本轮改动优先写测试名 / CHANGELOG 条目，或合入后改成 main 上的提交号。

### 业务 HTTP 路由与配置检查

**业务 HTTP 路由（`wiring/web`）**：

- 传输层错误不带 `code`：解码失败 400 `{"ok":false,"error":"decode request"}`、请求体超限 413、panic 500 `{"ok":false,"error":"internal server error"}` 与业务错误的 `{ok, code, reason}` 形状不同；客户端先看 HTTP 状态，再按有无 `code` 区分。
- 映射成 5xx 的业务错误原文日志（`webroute: route failed with a server error`）只有 `request_id`、没有路由模板（生成的路由调用 `WriteResult(w, …)` 拿不到请求）；路由模板在同一 `request_id` 的 `web: request failed` Warn 里。
- 指标按 handler 写出的状态码计：handler 超过 `web.write_timeout` 才写完时客户端收到空回复，仍计为 handler 写的类别（时长直方图能看出超时）。
- Mod 不做鉴权，生成的 JSON 路由拿不到请求头（CODEGEN_REFERENCE §11）；安全边界沿用 ops 的“缺省回环、生产绑公网要显式允许”。业务路由与 ops 分开监听是可由维护者改的取舍。

**`roost config check` 与跨服务比较**（`--all` 与 doctor `config-across-services`）：

- 工程编译不过时只做键检查，明确打印 “cross-mod rules NOT checked”，退出码 5，不报 valid。
- 跨服务比较只按生成器认识的框架声明（`kitconfig_gen.go` 快照与生成的接入层声明），业务服务自己声明的键即使跨服务共享也不比较；值按写法比较，`gd.effect` 与 `gd.effect.` 这类运行时等价写法会报不一致（改成同一写法即可）。
- 端口只比较开发配置集（假定一台机器）；生产示例按每服务一个容器只比较共享资源名，同机多进程的生产部署要自己保证端口。`etcd.advertise_addr` 按 `unique` 要求开发配置集里各服务不同（维护者 2026-10-19 决定）。
- 端口来自 `roost.yaml` 的 `services.<name>.port_block`，块号上限 99，超过 100 个服务的工程要改端口段规则（[PROJECT_GENERATOR §5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)）。
- `shared` 只表达“全部服务相同”，有意让不同服务用不同效果流 / saga 库的拆法会被报不一致，没有豁免开关。

## 框架服务与 game-demo

- **匹配队列状态**（维护者 2026-10-19 方案 A）：一个分区的全部状态整体读写；结束满 `match.retention`（缺省 10 分钟）的票、比赛与排队请求记录在下一次写入或 Sweep 时移除，重放与读历史只在保留期内有效（超期 Commit 重放报 `ErrTicketMissing`，不会成第二局）。状态上限 = 等待票（≤ `MaxQueueLength`）+ 保留期内的成局量；大队列按 `Partition` 分键是推荐用法，跨分区成局用 `CommitAcross`（键带 hash tag，一个逻辑队列的全部分区在 Redis Cluster 的同一节点）。规模建议见 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界)。2026-10-20 夜间容量测量（一次性夹具，本机 loopback 私有 Redis，[夜间补测](../performance/NIGHT-20261020.md) §11）：每个分区的状态按“每秒成局人数 × 已过时间”增长（每名已成局玩家约 510 B），单次操作耗时随状态线性上升；4 分区各每秒成局 10 人、保留期 10 分钟时第 8 分钟（每分区约 2.6 MB）CAS 冲突比例到 0.94、请求超时，同一 Redis 上无关命令的 PING P99 到 0.6s——这一档**不可持续**，持续上限约为每分区“每秒成局人数 × 保留期”≤ 约 4500 人（状态约 2.3 MB），超过时调小 `match.retention` 或加分区（2026-10-21 维护者批准写进 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界) 规模建议，注明单样本、非目标负载）；Redis 被拖慢会影响同实例的其他服务。`CommitAcross` 与相邻分区匹配器抢同一批票时冲突约 49%（夹具有意的高竞争形态）。私有 3 主 3 从 Cluster 上两次杀掉该逻辑队列槽位的主节点：只有约 1～2 秒的连接错误，多键脚本没有部分生效，590 场比赛核对全部一致；异步复制丢最近写、跨机分区与 Cluster 下的容量未测。夹具的后台 Sweep 因参数错误（limit 1000 超过上限 200）未生效，单样本。
- **game-demo 公会名**（维护者 2026-10-17 决定：业务逻辑，框架不处理）：`guild.ValidateName` 只拒绝空、全空白、控制字符与非法 UTF-8；长度上限与全服重名 demo 不实现，名字只受协议帧上限约束、两个公会可以同名。需要时长度 / 敏感词加在 `ValidateName`，全服唯一参考 `service/directory`（先 Reserve 再创建、成功 Commit、失败 Cancel），见 [Service 篇 §4](../framework/guide/09-services.md#4-关键业务边界)。
- **game-demo 副本排行**：排行准入就是奖励准入（领奖事务内的赛季通关数 + `UpdateMax`）。通关数按赛季计（维护者 2026-10-18），改 `ranking.CurrentSeason` 后新榜从 0 开始；`UpdateMax` 同分保留较早的 tie，补报的那一分在同分时按补报时间排序。
- **game-demo 活动结算榜**：`ZADD GT` 原子保留较大累计值，需要 Redis 6.2 及以上；只修复成功回复乱序导致的回退。Redis 写失败只记日志，协调器与结算索引不是一个事务，没有自动对账；Redis Cluster、跨机延迟与长稳未验证。业务将来允许扣分时需改用版本化快照或其他明确的顺序方案。
- **game-demo 活动贡献**（维护者 2026-10-18 决定 A：只在奖励首次付款时贡献）：**已接受的代价**——付款之后、贡献之前进程崩溃或协调器调用失败，这一分永久丢失，重放不补（端点打 Warn）。协调器只在同一窗口内去重，业务在别处直接调用 Contribute 须自己保证每局一次。
- **全服邮件 `mail.LazyBroadcast`**（[DESIGN-TODO D17](DESIGN-TODO.md#d17-全服邮件的内置投递策略)，用法见 [Service 篇 §1B](../framework/guide/09-services.md#1b-mail-全服邮件)）：生成骨架缺省仍为 nil（拒绝广播），启用要改 `Broadcast()` 一行。剩余边界：① **账本规模没有压测**：账本是一个文档（缺省至多 1000 条未过期记录，每条约 150 字节 JSON），每次发布 / 撤回整体读写、在 Redis Cluster 上落在一个节点；读路径按版本在进程内缓存，账本变化后每个 mail 进程各重读一次。上界附近的发布延迟、十万级在线同时登录时版本键的读压力、公会邮件频繁发送时的上界是否够用 v1.29 未验证。② **画像以最近一次 `PullBroadcasts` 为准**：入会 / 退会后业务不再拉一次，`List` / `Summary` 会按旧范围合并、水位越过新公会已发出的邮件（之后再拉也补不回）；在两次拉取之间入会、而公会邮件发于入会之前的，会被合并（需要“入会后才收”时把入会时间放进 `Attributes` 由 `Eligible` 判断）。③ **条件只判一次**：被内置条件或 `Eligible` 判为不满足的全局邮件，水位越过后不再重新判断（玩家之后满足了也收不到）。④ 没调用过 `PullBroadcasts` 的玩家（没有画像）`List` 不合并；game-demo 只放本服范围、不放公会范围。⑤ 撤回只挡未合并的，已合并的副本没有“强制收回”入口；撤回记录保留到过期。⑥ 内置条件按秒比较、严格早于：发送同一秒注册的角色收不到。⑦ 合并只在邮箱放得下时进行（全是未读的满邮箱等玩家腾位置），停住期间过期的全局邮件不会再补。
- **业务时区与每日重置**（DESIGN-TODO D18 已实施，维护者 2026-10-20 决定 A）：业务日按 `time.business_timezone`（缺省 UTC）/ `time.business_day_start`（缺省 00:00）算，见 [guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)。剩余限制：① 时区数据库靠 `main.go` 导入 `time/tzdata`；`main.go` 归应用、sync 不改，旧工程或手写 main 在没有 zoneinfo 的镜像里，启动检查会拒绝非 UTC 时区（报错点名 `time/tzdata`，不会静默退回 UTC），要自己加一行导入。distroless 镜像本身是否带 zoneinfo 仍未在容器内实测，内嵌后不再依赖它。② “看不到 zoneinfo”的回归在 macOS 上用 `sandbox-exec` 模拟；自带 zoneinfo、没有这种隔离的 Linux 主机上 Skip（只设 `ZONEINFO` 为空目录模拟不了：Go 照样回落到系统目录）。③ macOS 文件系统不区分大小写，`asia/shanghai` 在开发机上能通过检查，在 Linux 镜像里加载失败（启动时拒绝）。④ 周重置、按周 / 月划分的活动窗口没有框架辅助，由业务按 `Day` 推。
- **Session 运行服务**：保持现状、不改为可选（维护者 2026-10-14）；后续事项见 [ACCESS-SYNC-FOLLOWUPS](ACCESS-SYNC-FOLLOWUPS.md)。
- **观测接线**：ExportMetrics / admin 元数据不是自动外部监控或审批系统；部署实际接线与鉴权审计归部署方。


### 合服（D15）

别名合服（[DESIGN-TODO D15](DESIGN-TODO.md#d15-合服把一个服并入另一个服) 已实施，设计与实施记录 [FEATURE-2026-10-20-server-merge-alias](FEATURE-2026-10-20-server-merge-alias.md)，runbook [PLAYER-MIGRATION §6](../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)）。当前边界：

- **持久总线消息只列清单、不检查积压**：`wiring/sidmerge` 不连 NATS。按 sid 寻址的持久用法（`syncbus.transport: jetstream` 的每 sid durable、`nats.rpc.transport: jetstream` 的实例 durable、可靠总线的死信）由运维按清单人工确认（检查项 `bus` 为 `confirm`）。框架不把发往 A 的消息重投到 B。
- **saga 终态检查是整个 saga 存储**：saga 记录不带“属于哪个 sid”，同一存储里其他仍在运行的服的非终态 saga 也会让检查不通过；只有全集群停服窗口里才自然满足。是否提供“按类型 / 按业务键过滤”的检查需要维护者决定（尚未决定）。
- **“同一 Mongo”按 URI 比较**：主机列表（小写排序）+ 副本集名；同一集群换主机名或 IP 写会被判为不同而拒绝合服（偏保守，不会把两套 Mongo 当一套）。
- **跨记录的 account 写不是原子的**：每条服务器记录按版本条件写；写入顺序保证中途停下不会出现两跳，结束复核，并发合服造成的两跳报 `ErrServerMergeCycle` 并给修复命令（unmerge 再 merge）。拍平之后“X 是经由 A 并入的”这一历史不保留：`UnmergeServer` 只恢复一条记录，经 A 改指的其他服仍指向承载服。`MergedFrom` 是辅助索引，可能多出并发留下的项，是否并入以对方的 `MergedInto` 为准。
- **启动自检的范围**：`ServingGuardMod` 只在生成器装配的、承载玩家且接了 account 的服务里；手写装配要自己加。它排在 DataEngine 之前，但不排在 `syncbus`、`remote_entity` 等 Mod 之前：误启动的 A 在被拒之前这些 Mod 已经 Start 又随即停止（2026-10-20 演练：拒绝发生在启动后约 60ms）。account 不可达时 30 秒后拒绝启动（fail-closed），代价是 account 故障期间 game 起不来。
- **按 sid 独占数据的迁移不拷贝索引**：B 启动时按 DAO 声明建；不拷贝 `_dataengine_*` 控制集合。Remote 托管（`remote=managed`）落在按 sid 分库里的集合只在报告里点名，所有权元数据按 Remote 规则另行处理。回退不动合服期间 B 新产生的按 sid 独占文档（只计数），归属由业务判断。
- **源库保留为只读归档**：框架不加只读保护，靠启动自检与 runbook；删除由运维确认后手动做。
- **消费者**：Gate 转交给 game 的绑定不带 Claims，game 侧需要原服标识时从自己的角色数据取；`OriginServerIDClaim` 只在认证器那一侧可见（嵌入 TCP 的 Principal 里两者都有）。CreateRole 被拒经总线时客户端看到的是 `ErrServerClosed` 的通用原因，“已并入 B”的详情只在 account 进程的错误里。 game-demo 拉全服邮件（D17）时，原服并入本服的角色另带原服范围 `ServerScope(A)`，合服前发给 A、还没拉到的全服邮件照样属于他——这是 demo 的业务选择，框架不规定。
- **验证边界**：真实进程两服演练（私有环境，sid 1000 / 1001，[证据](../performance/benchmarks/d15-server-merge-20261020/README.md)）覆盖嵌入 TCP；独立 Gate 只有单元级（正式 account 服务 + `FixedGameResolver`）。演练里的 `dbscope=sid` 文档是直接写 Mongo 的（game-demo 没有用它的实体），B 的进程没有冷加载它们；玩家数据（共享库 main）经真实进程读写。

## 平台与外部验证

- **跨机HA与Linux物理故障**：本机集群 / loopback 不证明跨机器分区、掉电、真实网卡；具名清单见 [EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) E01～E28。
- **客户端引擎**：有 Go / C# 及 Unity 适配源码与测试，未声明真实 Unity / Godot / Unreal 全矩阵；各引擎实机、线程、断线与跨平台确定性未验收。
- **原生C++接入**：无完整 C++ / Unreal / GDExtension SDK 承诺，需独立设计和协议一致性验收。
- **独立 Gate**：已实施 Gate / Game、广播与 Sync / Lockstep（[GATEWAY](../framework/GATEWAY.md)）；不用本机多实例代替多进程 / 跨机矩阵。容量结论见能力表。
- **schema升级**：DAO 自动迁移已撤销，运行时只接受当前 schema；离线迁移按 [schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo 有执行器与 `cmd/schemamigrate`。在线双读与 `roost add migration` 骨架未实施，上线后重新决定；业务需明确离线转换和保留数据范围。2026-10-20 夜间在 v1.30 候选生成的 game-demo 上重做真实进程演练（[夜间补测](../performance/NIGHT-20261020.md) §12），与 10-15 逐步一致。
- **Player跨机**：以已落库数据为准，不要求本地 WAL 热迁移；搬的是 sid，按 [参考流程](../framework/PLAYER-MIGRATION.md) 停写→排空 WAL→释放单实例锁→切路由→新进程空 WAL 接管，game-demo 有检查工具 `cmd/sidmove`。单个玩家改绑 sid 无框架能力；合服用别名合服（D15 已实施，边界见下文[合服（D15）](#合服d15)）；真实跨机网络、卷迁移、客户端重连路径未演练；框架级迁移命令上线后评估。2026-10-20 夜间在 v1.30 候选上重做同机两进程演练（[夜间补测](../performance/NIGHT-20261020.md) §13：正常迁移、kill -9 后原 WAL 重启、Mongo 停顿时 kill -9 的未排空，302 次确认逐键 +1、最终 Mongo 等于最后确认），与 10-15 一致。
- **Windows**：非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'` 和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异（打开中的文件不能改名 / 删除：nestwal 损坏记录检查、日志轮转、statslog；checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真；run 38500440362 的 6 个失败）。本机结果不改变 macOS / Linux 支持范围，跨平台也成立的缺陷不归入此条。

## 能力与验证边界

口径：v1.28.0 与 v1.29.0 发版时都没有压测；2026-10-19 发版后补测 v1.29.0 同参数重档 30 分钟与 Saga 两档（[v1.29.0 发版后复测](../performance/V1290-HEAVY-30M-20261019.md)，附同日同机 v1.27.0 对照）。2026-10-20 夜间在 main `69fb668f`（v1.30.0 候选）上按维护者批准的清单补测，每项 15 分钟（Saga Mongo 步骤夹具上限 10 分钟），服务端 / 压测端分开统计，宿主负载正常、全部作为基线（[夜间补测](../performance/NIGHT-20261020.md)）。下表“当前结论”取最新一次。

| 项 | 当前结论 | 后续入口 |
| --- | --- | --- |
| 重档总体 | 30 分钟通过：吞吐、计数、DAO 终值、AOI 可见集合、P99 门槛通过，消息 / HB 拒绝 0，堆不增长；goroutine 恒定比 v1.27.0 多 1000（每连接写协程）。业务 P99 13.09ms、HB 11.01ms（混合口径）；服务端口径业务 P99 4.37ms、HB 2.68ms（第一份未污染的 30 分钟服务端样本）。2026-10-20 夜间 A-B-A-B（v1.27.0 / HEAD 各 2 次 × 15 分钟）：HEAD 的 Sync 提交→客户端 P99 稳定高 0.6～1.1ms（13.99 / 13.92 对 13.35 / 12.80），两对方向一致，判断为代码带来的小幅增加，具体来源未做单变量对照、尚未确认；业务 / HB 混合 P99 高 0.1～0.2ms、Gate 往返无差异；服务端业务 P99 3.37 / 3.38ms | 单变量对照（例如 B3 写协程交接）；独占服务器 / 跨机 |
| Sync偶发长尾 | on_change 按 P99≤50ms 验收：Sync 提交→客户端 P99 15.43ms、最大 156.262ms、>50ms 0.0260%。>50ms 总量变多同日对照同样出现，按宿主条件处理；10-19 的“Sync >50ms 为同日对照 1.8 倍”在 2026-10-20 夜间 A-B-A-B 里不复现（四次服务端段 >50ms 都是 0），判断为宿主条件、不是代码。`TAIL=metrics` 15 分钟只出现 1 个长尾簇（284 个混合口径事件、最大 52.6ms），落在整分钟后第 1 秒、同秒调度延迟 21ms，相位与 10-15 一致但只有一簇，不足以确认；非整分钟的簇本次没有出现 | 维护者 2026-10-15 决定先接受现状，有独占服务器 / Linux 实机时再复测（方案 A） |
| 独立 Gate | 重档 30 分钟通过（short 8 / io 8，GOMAXPROCS=10，Gate 与 Sync 开）：Gate 往返 P99 6.78ms、最大 236.691ms。2026-10-20 夜间 A-B-A-B：HEAD 5.30 / 5.26ms 对 v1.27.0 5.40 / 5.20ms，>50ms 四次都是 0，10-19 的“>50ms 为对照 2.6 倍”不复现 | 多进程 / 跨机矩阵；[Gate/AOI 性能报告](../performance/GATE-AOI.md) |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行 observer。2026-10-20 夜间 4 并行度重档 15 分钟通过（GOMAXPROCS 4、short 4）：服务端业务 P99 4.83ms、Gate 往返 P99 7.74ms、Sync 提交→客户端 P99 17.98ms，>50ms（服务端）0；不压额外 observedBlocks 扇出 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验（未做） |
| 嵌入 TCP 出站 | 2026-10-20 夜间嵌入 TCP + 场景 Sync 重档 15 分钟通过：`budget_exhausted` 与全部出站拒绝 / 关闭计数 0，写协程 1000 → 1000；服务端业务 P99 1.57ms、往返 P99 1.98ms、Sync 提交→客户端 P99 10.14ms，>50ms 全 0；堆 208MB。每连接队列峰值与驻留字节取不到（没有 Gauge） | 停读客户端 / 更高推送量下的预算；驻留 Gauge 与按帧龄关闭由维护者决定 |
| 自身视图（self_sync） | 2026-10-20 夜间嵌入 TCP + self_sync 重档 15 分钟通过：约每秒 1000 帧，`push_refused` 0、滞留 0，提交→客户端 P99 1.43ms，服务端业务 P99 2.07ms；整轮重试放大未被触发（未测到） | 独立 Gate + self_sync 夹具；预算压到触发点的放大测量 |
| Lockstep 追帧 / 快照恢复 | 嵌入 TCP 追帧随链路放慢由包内与 game-demo 生成工程的真实回环 TCP 回归覆盖；快照恢复（2026-10-20，未发版）由包内、MOBA 十客户端 KCP、生成工程 C# 真实 TCP 回归覆盖，toxiproxy 演练嵌入 TCP 256 / 64 KB/s 下 1～4 MiB 快照恢复、独立 Gate 慢链路上大快照被断开循环（只准入，需 Gate 写出回执的协议改动，维护者 2026-10-20 决定暂不做）；纯追帧页 2026-10-20 夜间 toxiproxy 256 / 64 KB/s 演练：回到头部时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断 | 独立 Gate 慢链路上的纯追帧页；真实移动网络 |
| Remote容量 | 2026-10-20 夜间 80TPS × 15 分钟通过（共享隔离环境）：72000 / 72000，零错误零丢弃，全量核验通过，服务端 p50 / p95 / p99 164 / 249 / 749ms。2026-10-21 同机对照（v1.27.0 `a147877c` / HEAD `af904c76` 各 1 次 × 15 分钟，同参数背靠背，[§9.1](../performance/NIGHT-20261020.md#91-同机-a-b-a-b2026-10-21)）：两边都通过，混合 p99 同为 263ms（HEAD 服务端口径 262ms），p50 / p95 158 / 229 对 160 / 231ms，WAL 未确认 / 在途事务峰值 17 / 16 对 19 / 19；夜间 p99 偏高与峰值约 2 倍**倾向环境条件（同时段共享 Mongo 主节点慢操作 640 条对今天 79～126 条），不是代码退化**，单样本、Mongo 当时变慢的原因尚未确认；非跨机上限 | 跨机 RTT / 故障 / 24h；要排除 HEAD max / ≥500ms 次数偏多（1280ms、122 条对 605ms、26 条，单样本）需每边 ≥2 次交替 |
| Saga补偿容量 | Mongo 步骤路径 v1.29 各 10 分钟：正向 20/s P99 0.62s、全补偿 10/s P99 0.61s（服务端口径 0.59 / 0.58s），十个分钟段无增长趋势，核对通过。2026-10-20 夜间更高档位（10 分钟）：正向 30/s 完成率 28.4/s、P99 141.7s、17302 笔以补偿 / 失败结束，全补偿 15/s 完成率 14.0/s、P50 单调增长到 49.5s、866 笔 failed——两档都饱和（outbox / lease 排空、存储与发布零失败）。单进程上限在正向 20～30/s、全补偿 10～15/s 之间 | 中间档位（25、12）；真实步骤、混合负载与生产存储另测 |
| Saga 原生步骤容量（单进程） | 2026-10-20 夜间 15 分钟：正向 8/s（两次）与全补偿 5/s 都 0 笔终态不对、核对通过；服务端 P50 559 / 549ms、713ms（P50 < 1s 成立），**P99 0.92～3.9s**，有分钟级尖峰（各段 Mongo 命令最大值同时到 750～900ms，接近投影器容量时积压放大）；两次正向 8/s 的 P99 差 4 倍。原先 guide/06 §5、生成的 `sagaCapacitySummary` / `steps.go` 注释与 `roost help saga` 里的“P99 0.8 / 1.2 s”来自 2 分钟单样本，15 分钟样本上不成立；2026-10-21 维护者批准，这几处与 CODEGEN_REFERENCE / PROJECT_GENERATOR / BEGINNER_WORKBOOK 已改为 15 分钟口径（P50 < 1s、P99 0.9～3.9s、WAL 放快盘、Mongo 步骤上限区间）；限制与维护者决定见上文 [Saga](#saga) | [SAGA-CHECKPOINT](../performance/SAGA-CHECKPOINT-20261018.md) 复跑入口；P99 尖峰的 Mongo 侧原因 |
| 投影 checkpoint 与 outbox 唤醒同盘 | v1.29 未验证：2026-10-18 观测到投影后唤醒 outbox（`de8b6e97`）使 checkpoint 变慢，机制为推断（darwin 上 `WAL.Ack` 的 F_FULLFSYNC 与被唤醒 outbox 的 Mongo majority 写同时落盘），无逐次 fsync 证据，Linux 未测。唤醒的净收益为正，不回退；WAL 与 Mongo 分盘（第三档 C）不做。2026-10-20 夜间 RAM disk 对照（正向 8/s × 15 分钟，WAL 放 RAM disk）：checkpoint P50 56～58 → 1ms、WAL 追加 18 → 1ms、投影等待 62 → 0ms、服务端 P50 549～559 → 334ms，空载 checkpoint 约 8.8ms，说明负载下变慢主要来自同盘竞争；RAM disk 同时去掉设备 fsync 与同盘竞争，两者没有分开；P99 由 Mongo 侧停顿决定、两种盘都有。2026-10-21 维护者批准把“磁盘同步是主要成本，部署时 WAL 放快盘”写进 guide/06 §5 与生成的容量说明 | Linux 复测；逐次 fsync 计时 |
| 匹配队列容量 | 2026-10-20 夜间（本机私有 Redis，一次性夹具）：每分区“每秒成局人数 × 保留期”持续上限约 4500 人（状态约 2.3 MB），4 分区各 10 人/s、保留期 10 分钟第 8 分钟崩溃（CAS 冲突 0.94、超时、同 Redis PING P99 0.6s）；CommitAcross 竞争冲突约 49%；Cluster 主节点 SIGKILL 切换两次无部分生效。见上文“匹配队列状态” | Cluster 下的容量；跨机 Redis（guide/09 §4 已补保留期的规模建议，2026-10-21） |
| 生成工程完整链路持久写入（嵌入 TCP + strict） | 2026-10-20 单样本、非独占宿主（[报告](../performance/GENERATED-TCP-STRICT-20261020.md)）：CLI 生成工程、strict handler 金币 +1、不经 Saga、无 self_sync，50 连接各自一个玩家**压测端** 1253 TPS、P50 38ms、P99 75ms，WAL 每批约 9.9 条；同一实体 108 TPS。上限约为 short 池 worker 数（缺省 GOMAXPROCS）/ 每笔持锁时间（strict 在锁内等所在批 fsync，本机约 8ms）。async 不比 strict 快（同机 1083 对 1257），原因尚未确认 | 独占宿主复测与 self_sync / pipelined 对照（待补测第 14 项；维护者 2026-10-21 批准，随 v1.31.0 发版前压测一起跑） |
| schema 升级 / Player 跨机演练 | 2026-10-20 夜间在 v1.30 候选生成的 game-demo 上真实进程演练通过（schema：拒绝旧 schema → plan / apply / verify → rollback 只恢复未被写过的一份；Player：正常迁移、kill -9 后原 WAL 重启、未排空被 `wal-drained` 拒绝，302 次确认逐键 +1），与 10-15 一致 | 真实跨机网络、卷迁移、客户端重连路径 |

## 游戏接入验收

九份接入手册的“第二部分 验收”是各自验收范围的完整说明（场景清单、证明与不证明、崩溃点、计数核对）。脚本都不挂在 `dataengine-env.sh test`、不在根包门禁里，改 `game` 模板、`add dao` / `add entity` / `add timer` / `add attribute`、多实体 Sender、Nest 生成或 Lockstep 时手动跑；真实模式入口登记在根包 `integration_coverage_test.go`。夹具里的数值都是业务样例，玩法规则缺少默认实现不算框架缺陷。

| 游戏 | 入口脚本 / 真实模式变量 | 证明什么 | 不证明什么 | 手册 |
| --- | --- | --- | --- | --- |
| SLG | `scripts/test-slg-generated.sh`，`ROOST_SLG_REAL=1` | 6 个建造、行军、抢地组合的事务与期限行为；联盟活动联合链路 3 条：协调器子进程重启后贡献只计一次、阶段屏障、原服奖励义务丢 ACK 后重投，两个原服 Player 在“已到账未标记”处 SIGKILL 后冷加载补完，联盟权威经 Remote owner 交接后旧 owner 被拒 | 缺省模式无 WAL / Mongo、无真实重载与驱动加载后投递（离线定时器只做 DAO 水合）；联合链路的联盟 owner 与 sid 客户端同在一个父进程、固定服务发现、协调器只正常重启、视野传输为内存捕获、无空间迷雾与跨服撤权、不含实体定时器 | [第二部分](../framework/SLG-INTEGRATION.md#第二部分验收) |
| MMORPG | `scripts/test-mmorpg-generated.sh`，`ROOST_MMORPG_REAL=1`；`test-mmorpg-scenes.sh`；`test-mmorpg-cross-sid.sh` | 13 个多实体组合；正常停机与 SIGKILL 后交易 / 托管 / 定时器经投递循环恢复不丢不重；多地图 TCP；双 sid 赠礼 Saga | 提交在途崩溃、断电、跨机、动态 Player 跨 sid、真实客户端与目标密度 | [第二部分](../framework/MMORPG-INTEGRATION.md#第二部分验收) |
| MOBA | `scripts/test-moba-generated.sh`，`ROOST_MOBA_REAL=1`，再加 `ROOST_MOBA_ROUTING=1` | 28 条结算 / Lockstep / 匹配 / KCP 追帧场景（含断线超过历史窗口后经服务器参考检查点快照恢复：直接驱动 Match 一条、经持久对局 Room 与已提交门一条；Room 提交卡住 / 结果未知时不发未提交帧）；结算两进程恢复；活动对局 SIGKILL 后续赛与 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复（真实夹具的宿主都用 `RecoverRoomWithSnapshots`）；五进程 NATS 宿主交接与奖励重投 | 对客户端广播与真实鉴权的端到端链路、权威 HA、tick 预算、5v5 目标负载与跨机 | [第二部分](../framework/MOBA-INTEGRATION.md#第二部分验收) |
| 卡牌 RPG | `scripts/test-card-rpg-generated.sh`，`ROOST_CARD_RPG_REAL=1` | 16 个抽卡 / 养成 / 回合组合；SIGKILL 后从未投影 WAL 恢复，投递循环补一回合再按期投递 | 提交在途崩溃、断电、系统时钟长时计时、Redeliver、多进程与容量 | [第二部分](../framework/CARD-RPG-INTEGRATION.md#第二部分验收) |
| 合作塔防 | `scripts/test-tower-defense-generated.sh`，`ROOST_TOWER_DEFENSE_REAL=1` | 10 个共享建造 / 波次组合；第一波进行中 SIGKILL 后逐字节恢复，循环补一步再按期打完三波 | 提交在途崩溃、断电、四台真实客户端、多进程与容量 | [第二部分](../framework/TOWER-DEFENSE-INTEGRATION.md#第二部分验收) |
| 合作生存撤离 | `scripts/test-survival-extraction-generated.sh`，`ROOST_SURVIVAL_EXTRACTION_REAL=1` | 12 个入场 / 采集 / 制作 / 撤离组合；制作与撤离在途 SIGKILL 后恢复，循环补触发一次且不追帧 | 恢复路径上的探险结束处理、提交在途崩溃、真实客户端、多进程与容量 | [第二部分](../framework/SURVIVAL-EXTRACTION-INTEGRATION.md#第二部分验收) |
| 合成休闲 | `scripts/test-merge-casual-generated.sh`，`ROOST_MERGE_CASUAL_REAL=1` | 15 个合成 / 离线批次 / 广告组合；SIGKILL 后恢复，过期挑战只判负一次、下一期限按时投递 | 提交在途崩溃、真实广告平台与登录票分配、多进程与容量 | [第二部分](../framework/MERGE-CASUAL-INTEGRATION.md#第二部分验收) |
| 放置模拟 | `scripts/test-idle-sim-generated.sh`，`ROOST_IDLE_SIM_REAL=1` | 12 个生产队列 / 离线收益组合；SIGKILL 后恢复，补触发只积分到原窗口、下一秒刷新按时投递 | 提交在途崩溃、真实客户端 / 支付、跨服市场、多进程与容量 | [第二部分](../framework/IDLE-SIM-INTEGRATION.md#第二部分验收) |
| 自走棋 | `scripts/test-auto-chess-generated.sh`，`ROOST_AUTO_CHESS_REAL=1` | 12 个卡池 / 阶段 / 领奖组合；冻结战斗后 SIGKILL 恢复，循环只补一个阶段并按期推进到终局 | 提交在途崩溃、八个网络客户端、跨 sid 发奖、多进程与容量 | [第二部分](../framework/AUTO-CHESS-INTEGRATION.md#第二部分验收) |

## v1.29 待补测

维护者 2026-10-20 批准的补测已于当晚在 main `69fb668f`（v1.30.0 候选）上跑完，第 1～13 项与 AOI 三点对照的结论已回填到上文各条与能力表，报告见 [夜间补测](../performance/NIGHT-20261020.md)。仍未完成或没有结论的：

1. **Sync 长尾的归因**（原第 4 项）：`TAIL=metrics` 15 分钟只出现 1 个长尾簇（整分钟后第 1 秒），不足以确认或排除与宿主整分钟周期任务同秒；按方案 A 等独占服务器 / Linux 实机复测。
2. **Sync 提交→客户端 P99 +0.6～1.1ms 的来源**（原第 3 项的后续）：A-B-A-B 判断为代码带来，但没有单变量对照，具体提交尚未确认。
3. **Remote 80TPS p99 偏高**（原第 9 项的后续）：2026-10-21 同机对照已做（各 1 次，[§9.1](../performance/NIGHT-20261020.md#91-同机-a-b-a-b2026-10-21)），v1.27.0 / HEAD p99 同为 263ms，倾向环境条件、不是代码退化；夜间共享 Mongo 主节点变慢的原因尚未确认，结论见能力表“Remote容量”。
4. **D19 每玩家 16 个额外空间视野时 Nest 模式失守**（AOI 三点对照新发现）：2026-10-21 已定位并修复（AOI 批次重评 + 排队事实异步应用），剩余边界见上文“AOI 视野”的“剩余边界”；Nest（ApplyAsync）与 direct 之间约一个同步周期的差距维护者 2026-10-21 决定接受，并改为由业务选应用模式（ApplySync 缺省，每玩家额外视野不超过 8）。
5. **投影 checkpoint 同盘**（原第 7 项）：RAM disk 对照已做，Linux 上复测与逐次 fsync 计时没做。
6. **Saga Mongo 步骤中间档位**（原第 8 项的后续）：20 / 10 稳定、30 / 15 饱和，25 / 12 未测；`TestRealSagaPerformance` 限制单次 ≤10 分钟，本项只能跑 10 分钟。
7. **匹配队列**（原第 11 项的后续）：Cluster 下的容量、跨机 Redis、异步复制丢写没有测；夹具的后台 Sweep 因参数错误未生效，要复测需改夹具。
8. **生成工程完整链路持久写入**（原第 14 项，2026-10-20 审查新增）：维护者 2026-10-21 批准，随 v1.31.0 发版前压测一起跑（v1.30.0 不再单独发版）。入口见[报告 §4](../performance/GENERATED-TCP-STRICT-20261020.md#4-复跑)。
