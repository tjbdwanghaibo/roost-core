# 当前已知限制与验证边界

适用版本：v1.29.0 与 main 上未发版的改动（CHANGELOG `[Unreleased]`）。本文件只记**当前仍有效**的限制、维护者已接受的代价和验证边界；已修复问题的现象、根因、红绿证据与修法见 [CHANGELOG](../../CHANGELOG.md)、[docs/release](../release/) 与 Git 历史。审查发现的问题以 `RV-*` 登记在本文件开头（当前没有未解决的 RV 项，进度见 [COMMIT-REVIEW](COMMIT-REVIEW.md)）。拿不准是否仍有效的条目标“待核对”；v1.29 上没有证据的验证项写“v1.29 未验证”，补测入口汇总在文末 [v1.29 待补测](#v129-待补测)。

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

框架没有针对单个实体 / 玩家的运维命令（admin 只有 `configdata.*`、`hotcode.*`、`bus.dlq.*`），方向待维护者决定（[DESIGN-TODO D16](DESIGN-TODO.md#d16-单个实体--玩家的运维入口)）。运行期直接改权威集合的结果按设计是确定的：只改字段时下一次投影覆盖改动；改 `_version`（恢复旧备份）时投影 fatal、进程 fence、重启起不来，直到人工处理（[维护手册 §4](README.md#4-故障处理)）。在线改档写成业务 GM handler，整档恢复走停服排空后的离线写回（[guide/03 §3](../framework/guide/03-dataengine.md#3-投影与幂等)）。

### DAO 持久键（D13）

定义里字段的 `bson:"x"` 就是持久键（缺省 snake_case），库里有、DAO 未声明的键不读入，下一次整文档写回即删除（维护者 2026-10-19，[DESIGN-TODO D13](DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）。

- **业务约束**（[SCHEMA-MIGRATION §1](../framework/SCHEMA-MIGRATION.md)）：改名 / 删字段前要么保留原 `bson` 名，要么先离线迁移再提升 schema，否则旧数据不再加载、写回即丢失。
- **剩余风险**：`roost generate` 与 `RestorePersisted` 的 WARN 不阻断，忽略告警时旧值丢失由业务约束承担；单字段补丁不碰旧键，它们留在库里但不再被读取，清理要一次性离线迁移；生成期基线只在工作树里，删掉生成物或换机器重新生成看不到上一次的形状，只剩加载期 WARN。

### Remote

- **Remote WAL级Rejected处理**（`38b710a2`）：Durability 1/2/3 的正式链路不产生 Remote Rejected，strict / pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`；tracker 的 Rejected 收尾只剩 Durability 0 来源。守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore / MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict / pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`）。
- **Remote 请求 struct 与 Sender**：handler 参数用带 `remote:"..."` 字段的请求 struct 时，struct 必须留在 handler 包（访问器生成在这里），sender / syncsender 子包又看不见它，所以这种 handler 生成失败（生成器给专门报错、不建议移走类型）。维护者 2026-10-16 决定先写清限制、不改生成结构；需要时再设计让 sender 携带这种 struct，或允许单个 handler 不生成 sender。

### Saga

- **原生步骤的多进程分片**：准入与转交由业务写（框架提供 `WithAdmit` 与 `RunHandedOver`，生成的 `RegisterSteps` 注释点名要补），见 [DESIGN-TODO D12](DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持)。
- **原生步骤单进程容量**：第一瓶颈是 DataEngine 投影器——带回执 / effect 的记录（`projector_replay.go` `isLocalBatchRecord`）逐条一次投影事务，checkpoint 已与普通记录按 `CheckpointRecords` / `CheckpointInterval` 合并；正向每笔 2 条、全补偿 3 条。等回执在后台（`StepConsumerConfig.ReceiptWaiters`），代价是同 durable 上同一实体的前后命令可能撞实体屏障（交还租约、退避 nak）。维护者 2026-10-18 决定 A（接受并文档化，量大按 sid 分片多进程、原生步骤加 `WithAdmit`）；第三档 B（合批带回执 / effect 的记录）与 C（分盘）不做；checkpoint 缺省间隔保持 20 ms（维护者 2026-10-19 方案 A），更高吞吐可调 `dataengine.projection.checkpoint_interval`，代价是崩溃后重放窗口与未确认额度变长（重放仍幂等）。容量数字见下方能力表（v1.29 未验证）与 [guide/06 §5](../framework/guide/06-saga.md#5-接入与运维)。
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
7. 只做了本机共享隔离环境的真实进程演练、真实 Gate + Game Ingress + NATS 的包内回归与生成工程回归，重档压测 v1.29 未验证（待补测）。

**self_sync 默认 packer 的代价**：开关打开且 marker 没写 `subjectPacker=` 时生成器生成 `player_sync_packer_gen.go`，标了 sync 的字段默认全部对玩家本人可见，需要裁剪的写自己的 packer；marker 写了生成名 `NewPlayerSyncPacker` 时按业务 packer 处理、不生成；默认 packer 只发一个 DAO，多 DAO 的 Player 仍要自己写（[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)）。

### 嵌入 TCP 出站

每连接有界出站队列 + 唯一写协程：上限 1024 帧（维护者 2026-10-16 决定）与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计，不开放配置键；停读客户端被关闭、不拖住场景 Sync Flush / Lockstep Tick。2026-10-20 起有全进程驻留预算 `player_access.tcp.outbound.resident_bytes`（`cf6ad085`，与独立 Gate `outbound.resident_bytes` 同口径），放不下的一帧当场被拒（`gateway.ErrResidentBudgetExhausted`，计 `player_tcp_outbound_budget_exhausted_total`），不关闭连接、不等预算；Entity Sync 当整轮重试，PB 推送返回错误由业务处理。说明见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)。

维护者 2026-10-20 决定（均按推荐）：

- **Lockstep 实时帧被预算拒**：按数据报丢失处理，不断开连接，靠冗余帧或追帧补回（A）；追帧页被拒计一次追帧失败。
- **读循环回包与心跳 ACK**：计入预算但不拒绝（每连接至多一帧在等），驻留量最多可超出预算 `max_connections × (max_payload_bytes + 16)`（A）；读循环直写快路径的那一帧不计。
- **驻留字节 Gauge 与按帧龄提前关闭停读连接**：先不做，等重档压测看 `player_tcp_outbound_budget_exhausted_total` 再定。

剩余边界：

- 缺省 256 MiB 是按 Gate 缺省 128 MiB / 4096 连接折算到 10000 连接取整，没有重档压测验证正常负载不触发（v1.29 未验证，待补测）。
- 停读连接占着的预算要等它撞上每连接上限被关或 `write_timeout`（默认 5s）写超时才归还，期间健康连接的推送可能被拒（Sync 延迟上升，Lockstep 实时帧靠冗余补）；没有 Gate 那样的 `max_age`。
- 只有拒绝计数，没有驻留字节 Gauge。
- 推送入队多一次 payload 复制，每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。
- 证据只有包内回归（含关连接与写协程并发退出时恰好归还一次）、自身视图与 Lockstep 的真实回环 TCP 集成用例与生成 demo 场景用例。

### Lockstep

- **Lockstep历史**：默认自动保留 3600 帧（60Hz 60 秒 / 30Hz 120 秒，D1 方案 C，`a6376934`）。越界按 `MatchConfig.Recovery` 区分（D1 方案 B，维护者 2026-10-20 批准，P1～P5 已实施，[设计](FEATURE-2026-10-20-lockstep-snapshot-recovery.md)）：配了 `Snapshots.Provider`（零值即 auto）或显式 snapshot 时，越界改发业务快照、客户端恢复后续帧，只有快照路线最终失败（取不到、过大、钉住过期）才发 C9 v2 通知让客户端终止；catchup（没配快照来源时的缺省）仍是越界明确失败、通知客户端终止、不自动重同步。快照内容来自业务的服务器参考模拟，框架不提供默认模拟、不替业务持久化检查点。本地 20000 帧周期性快照恢复与默认窗口长局有界已验证；按业务帧率、断线时长与快照大小核对窗口和预算未做。见 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)、[SYNC-OPTIMIZATION](../framework/SYNC-OPTIMIZATION.md)。
- **Lockstep TCP 追帧**（`640e9fac`，维护者 2026-10-16 方向 A）：`lockstep.TCPSender` 实时帧 `SendDatagram` 只准入，追帧页 `SendReliable` 经 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出，追帧随链路放慢、积压只有在途一页，`CatchupSendTimeout` 生效。等写出只在 Match 的 `launchSend` goroutine 上；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。限速链路上：快照分块与快照之后的续帧页经同一等写出机制在 toxiproxy 256 KB/s、64 KB/s 下演练过（2026-10-20，main 代码，1 MiB 与 4 MiB 都恢复、连接不断、耗时与“大小 / 速率”对应，[演练](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）；纯追帧页的限速演练与 v1.29 上的表现仍未做（待补测）。见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)。
- **Lockstep 追帧链路背压（独立 Gate）**：维护者 2026-10-20 决定暂不做。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，nil 只表示 Game 本地准入：Game 侧队列满返回 `ErrReliableBackpressure`（每 Tick 计一次追帧失败，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时关闭连接；追帧与快照都不随链路放慢。快照按 `UnpacedBytesPerTick`（缺省 16 KiB/Tick，30Hz 约 492 KB/s）固定节流；2026-10-20 toxiproxy 实测（30Hz、缺省参数，[演练 §3](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md#3-独立-gate63-风险-1-实测成立)）：64 KB/s × 1 MiB、256 KB/s × 4 MiB 每次连接都在同一位置被 Gate 断开、重连后从头重传，三次都未恢复；256 KB/s × 1 MiB 恢复（低于预算的超出部分被缓冲吸收）。是否断开取决于快照大小、每 Tick 预算与链路速度三者；断开发生在 Gate 写 socket 的期限到期（`writev ... i/o timeout`，`gate.channel.failure_total{phase="socket"}`），不是 `max_age` 积压规则，具体是 5s `write_timeout` 还是单帧发送预算先到期**尚未确认**（按代码缺省配置下单帧预算 ≤ `outbound.send_timeout` 250ms 更早，日志未区分）。2026-10-20 起恢复中被 Detach 计 `lockstep.snapshot.failed.total{reason="detached"}`，这个循环可见。配置建议见[设计 §6.3](FEATURE-2026-10-20-lockstep-snapshot-recovery.md#63-独立-gate只准入时的行为与风险)。纯追帧页在独立 Gate 慢链路上的影响未实测。要做到随链路放慢，Gate 需要在写出 socket 后回执给 Game，这是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。生成 game-demo 在独立 Gate 部署（`accessplayertcp.NewIngressMod`）下拿不到 `TCPRuntime`，战斗的追帧页同样只准入（`battleLinks`）。
- **Lockstep 快照恢复的剩余边界**（2026-10-20）：① MOBA 持久对局 `Room` 已接快照来源（2026-10-20，`RecoverRoomWithSnapshots` + 已提交门 `MatchConfig.CommittedFrame`，[MOBA §5.7](../framework/MOBA-INTEGRATION.md#57-重连宿主代际与交接)），替身权威下有 KCP 与 Nest 两条回归，真实 strict WAL / Mongo 下有 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复的演练（`TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL`，下行是记录器、不经 KCP）；真实 WAL 的提交延迟分布、宿主交接与快照恢复同时发生、宿主与权威分属不同进程时的快照恢复未测；已提交门要求宿主提交落后切帧不超过 `RedundancyDepth - 1` 帧，否则恢复中的会话一直经可靠通道逐页跟随、不切回实时（Room 每 Tick 同步提交，不受影响）；② C# 的 `SHA256.Create` 与 restore 路径只在本机 .NET 10 测试过，Unity 编辑器 / IL2CPP 未实测；③ 服务端“写出”只到本机 socket 发送缓冲（嵌入 TCP）或 Game 本地准入（独立 Gate）：服务端认为恢复结束时慢链路客户端可能还差 1 MB 以上，之后的实时帧排在后面，恢复后头几秒延迟等于排空时间（演练推断，未单独测量缓冲）；④ 快照不续传：断开重连后从头重传，独立 Gate 慢链路上会循环（上条）；⑤ Provider 不响应 ctx 时 `FetchTimeout` 后可能再发起一次，“每局至多一次在途”不成立；⑥ 宿主手动 `TrimHistory` 越过钉住点时快照作废、按失败计，但不计 `failed` 指标；⑦ 真实移动网络、跨机与目标负载下的快照恢复、快照大小分布与窗口取值未测；⑧ 客户端上传快照（多数一致）、增量 / 压缩快照、检查点持久化与宿主崩溃续赛未做（设计 §13）；game-demo 战斗未接（§12 第 13 条）。
- **game-demo 战斗经 `lockstep.TCPSender`**（维护者 2026-10-20 决定）：生成 demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`）两条通道改经 `TCPSender`，嵌入 TCP（生成默认）下追帧页经 `TCPRuntime.PushLockstepSessionWritten` 等写出，与上面嵌入 TCP 追帧同一规则；回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（生成工程 `go test ./internal/service/game`）。线格式随之变化：10102 推送的是 Lockstep 包本身（RS v2 Lockstep 标志、不带 PB 包装与 battle id），按连接（接入层 SessionID）寻址；座位在它第一条 BattleInput 到达的连接上加入，没说过话的座位收不到帧，晚加入的座位靠追帧补齐。一条连接同时只打一场战斗（帧不带 battle id）。限速链路与真实进程下的 demo 战斗未演练。

### 广播与 AOI

- **SyncBus投递**：有限次数、保留期限及容量，不保证绝不丢；定位为默认可丢广播（维护者 2026-10-15）。需要最终一致的业务自建快照 / 对账闭环，不能仅测重试成功；兜底写法见 [guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)；框架级对账为下一阶段候选。
- **policy Drain**：等待 retry 和已承担事实；永久拒绝仍需修关系或 ctx 超时，Drain 不放弃条目，超时返回 `*DrainStalledError`（列出卡住条目、原因与等待时长）并计数（D3，维护者 2026-10-15 决定 A，`b86a1286`）。“放弃出口”（D3 方案 B）需先梳理永久拒绝的判定来源。
- **AOICluster 容量抖动**：Cluster 的 `MaxVisible` 每次移动后严格等于候选内最近 N 个，容量层没有滞回，两个目标在第 N 名附近来回时每次移动都 Leave/Enter（`BenchmarkClusterFlushRandomWalkCapped` Enter +15.6%）。维护者 2026-10-17 决定不加滞回：抖动只在候选数超过 N 时出现，不设 `MaxVisible` 或把 N 设到常规候选数之上即可，容量只作人群聚集时的带宽保护阀。单区域 AOI 不受影响；Cluster 目前没有生产装配调用方。见 [INTEREST-BLOCK-AOI](../framework/INTEREST-BLOCK-AOI.md)。
- **AOI 观察者与视野（D14，2026-10-20 实施）**：① 两个实体经 `ObserverOf` 换到同一玩家、各自排队 `observer=true` 的移动时，排队事实按实体（`fact.id`）保序，两个实体之间的先后不保证，视点停在哪一个的位置取决于政策批次内的顺序；需要视点跟随某个实体时只让那一个实体带 `observer=true`。② 视野变大时新进入范围的目标一次性订阅、各下发全量，峰值受 `MaxVisible`（全局）与 Manager 快照预算约束，没有按观察者单独的预算；`MaxVisible` 仍是全局一个。③ 改视野没有排队版（维护者决定），事务内想改视野的玩法须在提交后再调用 `SetObserverView`。④ D14 前后的性能对照（`policy` 包 AOI 基准、`scripts/perf/sync-aoi`）尚未跑，排在 2026-10-20 夜间性能测试（待补测）。见 [guide/04-sync](../framework/guide/04-sync.md#附近的人观察者是玩家视野按观察者d14)、[impl/04-sync §2E](../framework/impl/04-sync.md#2e-interest-的观察者编号与视野d14)。

## 生成器、配置与工具

- **DAO 库名映射**（[D9](DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A）：`db=` 是逻辑库名，物理库由 `dataengine.databases` / `WithDatabaseResolver` 在启动时冻结。限制：①改映射等于搬数据，框架只在 WAL 还有未投影记录时拒绝启动（`ErrDatabaseMappingChanged`），不复制数据；排空后改了映射却没迁移，新库是空的，冷加载得到“从未创建”，GetOrCreate 会新建玩家——步骤见 [PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)。②声明了却没有映射的逻辑库物理名就是逻辑名本身，只在启动日志告警（生成配置总写 `main` 的映射，手写配置删掉它会落到名为 `main` 的库）。③不同进程（多个 sid、跨进程写 Remote 实体）的映射是否一致不校验。④二进制一个逻辑库都没声明（工程还没有 DAO）时，配置里的逻辑名无从做拼写检查，只告警。⑤工具经 `LoadDatabaseMapFile` 读同一份配置，但不会自动执行进程里注册的回调，用了回调的部署要给工具传同一个函数或用 `-db` 指定物理库。⑥根包守卫只看同一行里交给 `.Database(` 的参数，先赋给变量再传入的写法看不到，由各解析点的行为测试兜底。⑦回调与配置对同一逻辑库给出不同物理名时拒绝启动（同一逻辑库只能有一个来源，维护者 2026-10-20），但回调只在 Init 调用，`--check-config` / doctor 不执行回调，这类冲突要到进程启动才发现；工具传入回调时（⑤）按同一规则拒绝。
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

- **匹配队列状态**（维护者 2026-10-19 方案 A）：一个分区的全部状态整体读写；结束满 `match.retention`（缺省 10 分钟）的票、比赛与排队请求记录在下一次写入或 Sweep 时移除，重放与读历史只在保留期内有效（超期 Commit 重放报 `ErrTicketMissing`，不会成第二局）。状态上限 = 等待票（≤ `MaxQueueLength`）+ 保留期内的成局量；大队列按 `Partition` 分键是推荐用法，跨分区成局用 `CommitAcross`（键带 hash tag，一个逻辑队列的全部分区在 Redis Cluster 的同一节点）。规模建议见 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界)。目标负载下的吞吐与延迟、Redis Cluster 故障切换下的多键脚本 v1.29 未验证（待补测；Cluster 只在本机 3 主 0 从私有集群核对过跨分区成局与 `CLUSTER KEYSLOT`）。
- **game-demo 公会名**（维护者 2026-10-17 决定：业务逻辑，框架不处理）：`guild.ValidateName` 只拒绝空、全空白、控制字符与非法 UTF-8；长度上限与全服重名 demo 不实现，名字只受协议帧上限约束、两个公会可以同名。需要时长度 / 敏感词加在 `ValidateName`，全服唯一参考 `service/directory`（先 Reserve 再创建、成功 Commit、失败 Cancel），见 [Service 篇 §4](../framework/guide/09-services.md#4-关键业务边界)。
- **game-demo 副本排行**：排行准入就是奖励准入（领奖事务内的赛季通关数 + `UpdateMax`）。通关数按赛季计（维护者 2026-10-18），改 `ranking.CurrentSeason` 后新榜从 0 开始；`UpdateMax` 同分保留较早的 tie，补报的那一分在同分时按补报时间排序。
- **game-demo 活动结算榜**：`ZADD GT` 原子保留较大累计值，需要 Redis 6.2 及以上；只修复成功回复乱序导致的回退。Redis 写失败只记日志，协调器与结算索引不是一个事务，没有自动对账；Redis Cluster、跨机延迟与长稳未验证。业务将来允许扣分时需改用版本化快照或其他明确的顺序方案。
- **game-demo 活动贡献**（维护者 2026-10-18 决定 A：只在奖励首次付款时贡献）：**已接受的代价**——付款之后、贡献之前进程崩溃或协调器调用失败，这一分永久丢失，重放不补（端点打 Warn）。协调器只在同一窗口内去重，业务在别处直接调用 Contribute 须自己保证每局一次。
- **全服邮件投递**：`AudienceBroadcast` 只经业务提供的 `mail.Deliverer` 投递，框架没有内置策略，生成骨架缺省 nil（拒绝广播）；离线与后来登录的玩家也要收到，由业务的策略保证。是否内置“存一份 + 懒合并”待维护者决定（[DESIGN-TODO D17](DESIGN-TODO.md#d17-全服邮件的内置投递策略)）。
- **业务时区与每日重置**（DESIGN-TODO D18 已实施，维护者 2026-10-20 决定 A）：业务日按 `time.business_timezone`（缺省 UTC）/ `time.business_day_start`（缺省 00:00）算，见 [guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)。剩余限制：① 时区数据库靠 `main.go` 导入 `time/tzdata`；`main.go` 归应用、sync 不改，旧工程或手写 main 在没有 zoneinfo 的镜像里，启动检查会拒绝非 UTC 时区（报错点名 `time/tzdata`，不会静默退回 UTC），要自己加一行导入。distroless 镜像本身是否带 zoneinfo 仍未在容器内实测，内嵌后不再依赖它。② “看不到 zoneinfo”的回归在 macOS 上用 `sandbox-exec` 模拟；自带 zoneinfo、没有这种隔离的 Linux 主机上 Skip（只设 `ZONEINFO` 为空目录模拟不了：Go 照样回落到系统目录）。③ macOS 文件系统不区分大小写，`asia/shanghai` 在开发机上能通过检查，在 Linux 镜像里加载失败（启动时拒绝）。④ 周重置、按周 / 月划分的活动窗口没有框架辅助，由业务按 `Day` 推。
- **Session 运行服务**：保持现状、不改为可选（维护者 2026-10-14）；后续事项见 [ACCESS-SYNC-FOLLOWUPS](ACCESS-SYNC-FOLLOWUPS.md)。
- **观测接线**：ExportMetrics / admin 元数据不是自动外部监控或审批系统；部署实际接线与鉴权审计归部署方。

## 平台与外部验证

- **跨机HA与Linux物理故障**：本机集群 / loopback 不证明跨机器分区、掉电、真实网卡；具名清单见 [EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) E01～E28。
- **客户端引擎**：有 Go / C# 及 Unity 适配源码与测试，未声明真实 Unity / Godot / Unreal 全矩阵；各引擎实机、线程、断线与跨平台确定性未验收。
- **原生C++接入**：无完整 C++ / Unreal / GDExtension SDK 承诺，需独立设计和协议一致性验收。
- **独立 Gate**：已实施 Gate / Game、广播与 Sync / Lockstep（[GATEWAY](../framework/GATEWAY.md)）；不用本机多实例代替多进程 / 跨机矩阵。容量结论见能力表。
- **schema升级**：DAO 自动迁移已撤销，运行时只接受当前 schema；离线迁移按 [schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo 有执行器与 `cmd/schemamigrate`。在线双读与 `roost add migration` 骨架未实施，上线后重新决定；业务需明确离线转换和保留数据范围。v1.29 未演练（待补测）。
- **Player跨机**：以已落库数据为准，不要求本地 WAL 热迁移；搬的是 sid，按 [参考流程](../framework/PLAYER-MIGRATION.md) 停写→排空 WAL→释放单实例锁→切路由→新进程空 WAL 接管，game-demo 有检查工具 `cmd/sidmove`。单个玩家改绑 sid 无框架能力；合服（把一个服并入另一个服）无框架支持，别名合服提案待维护者评审（[DESIGN-TODO D15](DESIGN-TODO.md#d15-合服把一个服并入另一个服)）；真实跨机网络、卷迁移、客户端重连路径未演练；框架级迁移命令上线后评估。v1.29 未演练（待补测）。
- **Windows**：非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'` 和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异（打开中的文件不能改名 / 删除：nestwal 损坏记录检查、日志轮转、statslog；checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真；run 38500440362 的 6 个失败）。本机结果不改变 macOS / Linux 支持范围，跨平台也成立的缺陷不归入此条。

## 能力与验证边界

v1.29 口径：v1.28.0 与 v1.29.0 发版时都没有压测；2026-10-19 发版后补测 v1.29.0 同参数重档 30 分钟与 Saga 两档（[v1.29.0 发版后复测](../performance/V1290-HEAVY-30M-20261019.md)，附同日同机 v1.27.0 对照）。该重档走独立 Gate，不经嵌入 TCP `TCPRuntime`、self_sync、Lockstep 追帧、Remote、Saga 原生步骤与 DataEngine 投影器，这些路径的 v1.29 结论见各行。

| 项 | v1.29 结论 | 后续入口 |
| --- | --- | --- |
| 重档总体 | 30 分钟通过：吞吐、计数、DAO 终值、AOI 可见集合、P99 门槛通过，消息 / HB 拒绝 0，堆不增长；goroutine 恒定比 v1.27.0 多 1000（每连接写协程）。业务 P99 13.09ms、HB 11.01ms（混合口径）；服务端口径业务 P99 4.37ms、HB 2.68ms（第一份未污染的 30 分钟服务端样本）。P99 比同日 v1.27.0 对照高 0.2～0.7ms，尚未确认、未归因 | A-B-A-B 同机交替多轮（待补测） |
| Sync偶发长尾 | on_change 按 P99≤50ms 验收：Sync 提交→客户端 P99 15.43ms、最大 156.262ms、>50ms 0.0260%。>50ms 总量变多同日对照同样出现，按宿主条件处理；Sync >50ms 为同日对照 1.8 倍，单样本，尚未确认。长尾与宿主整分钟周期任务同秒的归因、非整分钟的簇在 v1.29 上未验证 | 维护者 2026-10-15 决定先接受现状，有独占服务器 / Linux 实机时再复测（方案 A）；待补测 |
| 独立 Gate | 重档 30 分钟通过（short 8 / io 8，GOMAXPROCS=10，Gate 与 Sync 开）：Gate 往返 P99 6.78ms、最大 236.691ms；>50ms 为同日对照 2.6 倍，单样本，尚未确认 | 多进程 / 跨机矩阵；[Gate/AOI 性能报告](../performance/GATE-AOI.md) |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行 observer；v1.29 未验证 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验（待补测） |
| 嵌入 TCP 出站 | v1.29 未验证：重档不经嵌入 TCP；`cf6ad085` 的全进程预算缺省 256 MiB 是推算值 | 待补测 |
| 自身视图（self_sync） | v1.29 未验证：只有本机真实进程演练与回归，未做重档 | 待补测 |
| Lockstep 追帧 / 快照恢复 | 嵌入 TCP 追帧随链路放慢由包内与 game-demo 生成工程的真实回环 TCP 回归覆盖；快照恢复（2026-10-20，未发版）由包内、MOBA 十客户端 KCP、生成工程 C# 真实 TCP 回归覆盖，toxiproxy 演练嵌入 TCP 256 / 64 KB/s 下 1～4 MiB 快照恢复、独立 Gate 慢链路上大快照被断开循环（只准入，需 Gate 写出回执的协议改动，维护者 2026-10-20 决定暂不做）；纯追帧页的限速表现 v1.29 未验证 | 待补测 |
| Remote容量 | v1.29 未验证（最近一次 80TPS 在 v1.27 代码上测）；非跨机上限 | 跨机 RTT / 故障 / 24h；待补测 |
| Saga补偿容量 | Mongo 步骤路径 v1.29 各 10 分钟：正向 20/s P99 0.62s、全补偿 10/s P99 0.61s（服务端口径 0.59 / 0.58s），十个分钟段无增长趋势，核对通过；不是上限测试，更高档位未测 | 真实步骤、混合负载与生产存储另测 |
| Saga 原生步骤容量（单进程） | v1.29 未验证：现有容量指引（guide/06 §5、生成的 `steps.go` 注释、`roost help saga`）来自 v1.28.0 发布前 2026-10-18 的实测；限制与维护者决定见上文 [Saga](#saga) | [SAGA-CHECKPOINT](../performance/SAGA-CHECKPOINT-20261018.md) 复跑入口；待补测 |
| 投影 checkpoint 与 outbox 唤醒同盘 | v1.29 未验证：2026-10-18 观测到投影后唤醒 outbox（`de8b6e97`）使 checkpoint 变慢，机制为推断（darwin 上 `WAL.Ack` 的 F_FULLFSYNC 与被唤醒 outbox 的 Mongo majority 写同时落盘），无逐次 fsync 证据，Linux 未测。唤醒的净收益为正，不回退；WAL 与 Mongo 分盘（第三档 C）不做 | 待补测 |
| 匹配队列容量 | v1.29 未验证 | 待补测 |
| 生成工程完整链路持久写入（嵌入 TCP + strict） | 2026-10-20 单样本、非独占宿主（[报告](../performance/GENERATED-TCP-STRICT-20261020.md)）：CLI 生成工程、strict handler 金币 +1、不经 Saga、无 self_sync，50 连接各自一个玩家**压测端** 1253 TPS、P50 38ms、P99 75ms，WAL 每批约 9.9 条；同一实体 108 TPS。上限约为 short 池 worker 数（缺省 GOMAXPROCS）/ 每笔持锁时间（strict 在锁内等所在批 fsync，本机约 8ms）。async 不比 strict 快（同机 1083 对 1257），原因尚未确认 | 独占宿主复测与 self_sync / pipelined 对照（待补测） |
| schema 升级 / Player 跨机演练 | v1.29 未演练 | 待补测 |

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

维护者 2026-10-20 已批准全部补测，结果出来后回填本文件与能力表。

1. **嵌入 TCP 出站重档**：v1.28 改为每连接有界队列 + 写协程，`cf6ad085` 加全进程预算（缺省 256 MiB 为推算值）。核对推送路径 CPU、内存与 `player_tcp_outbound_budget_exhausted_total` 是否为 0，并据此决定是否做驻留 Gauge / 按帧龄关闭。入口：`scripts/perf/nest-game.sh` 的嵌入模式 `ROOST_NEST_GAME_EMBEDDED=1`（2026-10-20 夹具，玩家直连 Game 内 `TCPServer` + `TCPRuntime`，场景 Sync 经 `selfsync.PushPlayerFrame`；命令、预计时长与输出字段见 [性能 README](../performance/README.md#嵌入-tcp-重档)）；只做过缩小规模的冒烟，重档尚未跑。每连接出站队列峰值与驻留字节夹具取不到（没有 Gauge），只有拒绝 / 关闭计数与写协程数。
2. **自身视图（self_sync）重档**：Game Ingress / 嵌入 TCP 预算耗尽时的整轮重试放大（上文 self_sync 第 4 条）。入口同第 1 项加 `ROOST_NEST_GAME_SELF_SYNC=1`（只在嵌入模式下；与场景 Sync 互斥，一个玩家实体只能登记在一个 Manager）；独立 Gate + self_sync 的夹具没有。
3. **重档 P99 小幅上升与 Sync / Gate >50ms 条数**：v1.29 对同日 v1.27.0 对照的差异是否来自代码，需同机 A-B-A-B 交替多轮（`scripts/perf/nest-game.sh`，参数见 [V1290 复测 §2](../performance/V1290-HEAVY-30M-20261019.md#2-命令)）。
4. **Sync 长尾的归因**：整分钟周期任务同秒的归因与不在整分钟的簇在 v1.29 上复核；入口 `ROOST_NEST_GAME_TAIL=metrics`（有开销，结果不与正式样本比较）与 `scripts/perf/tailtrace`；有独占服务器 / Linux 实机时按方案 A 复测。
5. **Block AOI 重档**：v1.29 未跑；入口见 [Block AOI](../framework/INTEREST-BLOCK-AOI.md) 与 [Gate/AOI 性能报告](../performance/GATE-AOI.md)。
6. **Saga 原生步骤容量与 checkpoint 合并**：测于 2026-10-18（v1.28.0 发布前）；入口 `TestRealNativeStepsPerformance`（`wiring/saga`，命令见 [SAGA-CHECKPOINT](../performance/SAGA-CHECKPOINT-20261018.md)），复核 guide/06 §5 与生成物里的容量指引。
7. **投影 checkpoint 与 outbox 唤醒同盘**：WAL 目录放另一块盘 / tmpfs 做同样对照，或在 checkpoint 两次 `Sync` 前后单独计时；Linux 上复测。
8. **Saga Mongo 步骤路径的上限档位**：v1.29 只测了正向 20/s、全补偿 10/s；更高档位用 `scripts/perf/saga.sh` 的 `ROOST_SAGA_PERF_RATE`。
9. **Remote 80TPS**：最近一次测于 v1.27 代码；入口 `scripts/perf/remote.sh`（参数见 [2026-10-15 性能四项 §3](../performance/PERF-ROUND-20261015.md)），更高档位 `scripts/perf/remote-capacity.sh`。
10. **Lockstep TCP 追帧在限速链路上**：用 toxiproxy 限速链路做一次纯追帧页的演练，核对追帧耗时与链路速度对应、连接不断。快照分块与续帧页已于 2026-10-20 演练（[LOCKSTEP-SNAPSHOT-DRILL-20261020](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)，夹具可改成只追帧复用）。
11. **匹配队列**：目标负载下的容量（每秒成局人数 × 保留期对应的状态大小与 Redis 往返延迟，含 `CommitAcross` 与单分区操作的竞争比例）；Redis Cluster 故障切换下的多键脚本。
12. **schema 迁移演练**：上次 2026-10-15（v1.27 代码）；入口 [SCHEMA-MIGRATION](../framework/SCHEMA-MIGRATION.md) 与 game-demo `cmd/schemamigrate`。
13. **Player 跨机演练**：上次 2026-10-15（v1.27 代码，同机两进程，含崩溃与未排空场景）；入口 [PLAYER-MIGRATION](../framework/PLAYER-MIGRATION.md) 与 game-demo `cmd/sidmove`。
14. **生成工程完整链路持久写入**（2026-10-20 审查新增，未经维护者批准）：2026-10-20 只有非独占宿主的单样本（上表）；在独占宿主上用同一夹具复测，并加 self_sync 开、pipelined 与 async 的对照，查明 async 不比 strict 快的原因。入口见[报告 §4](../performance/GENERATED-TCP-STRICT-20261020.md#4-复跑)。
