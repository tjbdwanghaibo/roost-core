# cellworld 真实时钟用例统一整治（2026-11-10）

基线 main `21f631e1`；分支 `test/cellworld-clock-cleanup`。macOS（Apple M5，10 核），Go 1.27，`GOWORK=off`；同机长期有别的负载。图谱工具不可用（连接断开），以当前源码为准。维护者 2026-11-10 的要求：只改测试、承诺不放宽；实现问题先红后绿单独提交。

上一轮见 [cellworld 整包偶发失败](../cellworld-realclock-flakes-20261110/README.md)（迟滞 / W 边缘余量、交接收尾）与 [千分位零头](../cellworld-subunit-20261110/README.md)（加压时约一半遍数 `view gap … exceeds 200ms`、`TestDamageAcrossBoundaryAppliedOnce`）。

## 结论

1. **实现缺陷一项（已证实、已修复，单独提交）**：冻结报告晚于交接结论到达区域表时，旧格重建一条永久的 frozen 记录。上一轮记为“尚未确认”的 `TestWorldStraightCrossing` “frozen 4.4s”、本轮加压探针里 `TestNATSWorldDiagonalThroughFourCellCorner` / `TestWorldJitterAtBoundary` / `TestWorldOnALoaderBackedGetter` 的收尾超时都是它。见下文“实现缺陷”。
2. **其余失败都是用例写法**：固定 sleep 后立即断言、前提（Ghost 到达）靠 sleep、墙钟阈值的间隔 / 频率断言。按下面的统一改法改完，并对每个改过的承诺做了变异，改后的用例仍红。
3. **加压对照**：见“稳定性”。

## 统一改法

集中在 `harness_test.go` 文件头（注释写明规则）：

| 情形 | 改法 |
| --- | --- |
| 等异步结论 | `eventually(t, within, what, check func() (ok, diag))`（`waitFor` 改为它的包装）；`waitSettled` / `assertSingleReal` / `waitView`（超时打出各格诊断 `cellDiag`）；新增 `waitKnown`（格的区域表里有某实体） |
| 期限 | `settleWithin = 5s`：长于缺省 `HandoffAbort` / `LeaseTimeout`（3s）与 `AttackDeadline` / `PanelDeadline`（2s），协议在这些期限内一定给出结论（成功或放弃），过了就是真卡住；`travelWithin = 10s`：玩家走一段（最长约 2600 单位）加交接 |
| “过几拍再看”的窗口 | 按格实际执行的拍数：`world.waitBeats(cell, n)`（区域心跳，TickDone）、`testProc.waitBaseBeats(n)`（cell.base 心跳）；或按协议证据（重发次数、`StaleDrops`、`GroupStale`、`GroupSnapsIn`） |
| 间隔 / 频率 | 以拍数为尺子：见下“witness 间隔”“升档拍数”“队伍快照” |
| 仍按墙钟的 | 负向窗口（看一件事在一段时间里没发生，负载下只会变弱、不会误报）；按墙钟周期的上限（慢档 NPC 拍数上限）；换手中只有消息的一段（`transitCap`，见下）；队员过期上限 |

### witness 间隔（`checkContinuity`）

承诺：每拍区域心跳给本格每个玩家 Real 发一份 witness；交接时旧格停发、新格装入后第一拍就发，客户端看到的视野只空过几拍。

原写法：相邻两份到达间隔 ≤ 3 拍 + 50ms（200ms；NATS 250ms；丢 Offer 500ms；丢回复 300ms；丢包 2s）。负载下心跳与消息一起被推迟，加压探针（4 份并行 + 8 个满载进程）里同一场景的间隔到 0.3～0.6s。

改为按 `gapRule` 分段：

| 段 | 尺子 | 上限 |
| --- | --- | --- |
| 同一格、同一 epoch 相邻两份 | 该格自己的拍号之差（`View.Tick`，准确） | 4 拍（原“3 拍 + 50ms”取整）；NATS 5 |
| 换手：旧格最后一份到冻结提交 | 旧格在此期间完成的区域心跳（RealChanged 失去 Real 的提交时刻 − 最后一份的 Tick） | 2 拍；NATS 3（不同连接不保序，旧格最后一份可能晚于新格第一份到 Base 而被丢） |
| 换手：新格装入提交到第一份 | 第一份的 Tick − 新格到装入提交为止完成的区域心跳数 | 同上 |
| 换手：冻结提交到装入提交 | 墙钟 | `transitCap` = 1s（= 心跳 `Beat.Elapsed` 的上限 max(1s, Period)）加上旧格区域心跳在这段时间里的最长停顿（`stallOf`）；丢 3 个 Offer 时加 3 × OfferRetry；丢包场景基数取原约定 2s |

多进程用例里子进程每拍区域心跳输出一行 `tick` 事件，父进程按 tick / real 事件分段（`mpWorld.beatsUpTo` / `realAt`）。

**需要维护者知道的取舍（按推荐实施；维护者 2026-11-10 确认按推荐）**：“冻结提交到装入提交”这一段只有消息（Offer 出站、传输、新格 Scene 的 Nest 队列），没有心跳，按拍数计会误报——加压探针里这一段到过 0.54s，同时两端各自只空过 1 拍；变异“装入时在锁 Scene 的事务里多等 1.2s”时新格的区域心跳被同一把锁挡住、拍数几乎不涨，也只有墙钟看得见。所以这一段只能按墙钟，基数取心跳的经过时间上限 1s，再加上旧格心跳在这段时间里的最长停顿（第一版只有 1s，加压对照里平均负载 60 时一次换手到 1.53s、整份 witness 只 10 份，误报；旧格停顿度量“环境慢了多少”，新格自己锁着 Scene 变慢不会让旧格停顿，见 M4）：比原来 200ms 的总间隔松，**消息段结构性变慢但不到 1s 的回归（例如多一次往返、在新格多排一拍）这组用例不再抓得住**。无负载时这一段实测 2～10ms。可选：A. 接受（推荐，本轮实施）；B. 另加一条只在“这段时间两端心跳都准点”时才生效的严格上限（实现复杂、判据本身受负载影响，需要单独做证据）。

### 升档拍数（`TestIdleTierWithoutPlayersAndWakeOnApproach`）

原写法：升档后墙钟 1s 内至少 15 拍。改为 A 的 20 拍区域心跳里 NPC 至少 15 拍（快档与区域心跳同周期，负载下一起推迟，比值不变）。慢档上限（2s 内 ≤ 4 拍）仍按墙钟：慢档按墙钟的 IdlePeriod 跳，负载只会让拍数更少。

### 队伍快照（`TestGroupPanelAcrossBases`、`TestGroupPanelExpiresWhenTheBaseStopsAndComesBack`）

- 一个周期一份：原写法墙钟 1s 里 3～7 份。改为窗口 = 玩家 2 的 Base 跑满 10 拍（名义 1s），下限按拍数（10 × 100ms / 200ms − 2 = 3），上限按窗口墙钟（墙钟 / 周期 + 2）：负载只会让窗口变长，上限随之放宽、下限不变。
- 过期：原写法以“停的时刻”为起点，下限 hold − 周期 − 150ms、上限 hold + 500ms。改为以最后一份快照发出的时刻为起点（`snapAt`）：下限 hold（mirror 按收到时刻保留 hold，收到不早于发出，严格成立），上限 hold + `transitCap`（过期靠定时清理，负载下推迟）。

## 盘点

行号是基线 `21f631e1` 的。“敏感”指负载下心跳 / 消息被推迟时会不会误报。轮询等待（10ms 一轮、有期限）与场景节奏（攻击间隔、面板两次刷新之间）不列。

| 位置 | 原写法 | 承诺 | 为什么敏感 | 改法 |
| --- | --- | --- | --- | --- |
| world_test.go:382-406 `checkContinuity` | 到达间隔 ≤ 200ms（NATS 250ms，各故障用例另定） | witness 每拍一份，交接只空过几拍 | 心跳与消息一起推迟时墙钟间隔变长；上一轮加压约一半遍数 `view gap` | 按拍数分段（见上） |
| world_test.go:420 `checkClient` | sleep 4 拍后查客户端持有 | 结束时附近的 NPC 与自己都在客户端 | 帧晚到即误报 | `eventually` 等到持有，再查整段 remove 历史 |
| world_test.go:542 `scenarioDiagonal` | sleep 3 拍后读 owner、查连续性 | 最终 D 是唯一 Real | 最后一次交接可能还没收尾 | `waitSettled` |
| world_test.go:561 `scenarioAttackAcrossAndDuringMigration` | sleep 3 拍后开始攻击 | 前提：A 看得见 B 的目标 | Ghost 未到时第一刀“看不见” | `waitKnown(A, target)` |
| world_test.go:609 同上 | sleep 3 拍后读 A 的 HP | HP = 100 − 2 × 结算次数 | 目标交接 / 最后几刀结算未完 | `eventually`（每次重算结算次数） |
| world_test.go:781 `TestWorldLossyNetworkCrossingAndBack` | sleep 10 拍后读 owner | 收敛到唯一 owner | 丢包下收尾更慢 | `waitSettled` |
| world_test.go `spawnAll`（新增前提） | 不等 | 场景从“边界带里的 NPC 两边都看得见”开始 | Ghost 未到时新格头几份 witness 缺 NPC（加压探针 `missing 2 views in a row`） | 等每个 Ghost 目标格知道它 |
| handoff_test.go:126 / 140 | 间隔 ≤ 500ms / 300ms | 同 `checkContinuity` | 同上 | `gapRule`（丢 Offer 时消息段加 3 × OfferRetry） |
| handoff_test.go:131-140 `TestHandoffAckLossResolvedByGhostUpdate` | 缺省 HandoffAbort 3s 下断言“没有放弃” | 回复全丢时靠新格 Ghost 隐式收尾、不放弃 | 隐式确认要走 Ghost → 区域事务 → 出站 → 角色事务几跳，平均负载约 45 时被 3s 追上一次（放弃是按配置的正确行为） | 本用例放弃期限取 2 × settleWithin；变异 M18 仍红 |
| handoff_test.go:153 `TestHandoffAbortDeletesWithoutResuming` | 前 1.2s 丢掉全部 Offer | 目标收不到 Offer 时按 HandoffAbort 放弃 | 冻结 + 放弃走完超过 1.2s 时 Offer 送达、等不到放弃 | 丢到旧格真的放弃为止 |
| handoff_test.go:171 / lifecycle_test.go:330 | sleep 3 拍后 `assertSingleReal` | 唯一 Real | `assertSingleReal` 本身已等 | 去掉 sleep |
| handoff_test.go:192 `TestHandoffRejectedResumesAndRetriesWithNextEpoch` | sleep 4 拍后读 A | A 以 epoch ≥ 3 恢复 Real | 停下时可能还有一次重试在冻结中 | `eventually` |
| handoff_test.go:225 `TestHandoffConcurrentCrossingBack` | sleep 1s 后读 owner | 唯一 Real | 回复扣住 400ms，收尾可能更晚 | `waitSettled` |
| handoff_test.go:242 `TestHandoffChainAcrossCorner` | sleep 3 拍后读 Base 的 owner | owner D、epoch 3 | Base 换 owner 靠 D 的 witness | `waitSettled` |
| handoff_test.go:256 / 266 `TestEnterIsIdempotentAndRefusesWhenRealElsewhere` | sleep 4 / 2 拍后断言（负向） | 重复 Enter 不造第二个实例；拒绝后 B 不装入 | 负载下窗口里协议没走完，只会漏报 | `waitBeats(A, 4)` / `waitBeats(B, 2)` |
| combat_test.go:46 / 74 / 108 / 136 / 230 | sleep 2～3 拍后攻击 | 前提：攻击者所在格看得见目标 | Ghost 未到即“看不见”：`TestDamageAcrossBoundaryAppliedOnce` 加压时 `9 distinct hits applied` / `want hp 80` | `waitKnown`；同格目标的去掉 |
| combat_test.go:51 | 结论到齐后 sleep 3 拍再数结算 | 每刀只结算一次（消息全部重复） | 重复副本可能还没处理（只会漏报） | `waitBeats(B, 3)` |
| combat_test.go:155 / 277 / lifecycle_test.go:40 / process_test.go:797 | sleep 3 拍后同格攻击 | — | 无前提可等 | 去掉 |
| combat_test.go:311 `TestPanelCommandLostIsResent` | sleep 1.2s（负向） | 命令全丢时 Real 不变、Base 未确认 | 窗口里 Base 可能还没重发（只会漏报） | 等 Base 首发加两次重发 |
| combat_test.go:352 / 445 | sleep 700 / 900ms（负向） | 晚到的旧版不覆盖新版；已确认的不报未知 | 只会漏报 | 保留，注释写明 |
| combat_test.go:451 `TestPanelUnknownWhenUnconfirmed` | sleep 1.5s 后要求恰好报一次 | 超过 PanelDeadline 报一次未知、只一次 | Base 心跳推迟时期限后那一拍还没跑 | `eventually` 等到报了，再过 9 拍 Base 心跳看不报第二次 |
| lifecycle_test.go:49 `TestCellCrashPlayerReentersFromSnapshot` | sleep 1.5s 再重启 B | 场景：第一次重进发给已停的 B | Base 看门狗推迟时重启早于重进（只改场景路径） | 等 `Reenters` 增加再重启 |
| lifecycle_test.go:87 `TestBaseGoneSceneActorExpires` / process_test.go:858 | sleep 1.5s（负向，长于 1s 租约） | Base 在续租时角色不离场 | 租约按墙钟；Base 饿死超过 0.5s 理论上会真的过期（实现按配置的正确行为） | 保留，注释写明；未观察到失败 |
| lifecycle_test.go:139-151 `requireStaysReal` | 重放旧 Leave 后看 300ms（负向） | 旧 Leave 不删新入场 | 处理晚于窗口时什么也没看到（只会漏报） | 等到格把它当过期丢掉（`StaleDrops` 增加）且至少 300ms |
| lifecycle_test.go:227 `TestWritebackIsIdempotentAndOrdered` | sleep 1s 后要求 ≥ 5 份 | 回写版本单调、至少几份 | 负载下 1s 不够 5 份 | 等到 5 份，再过 5 拍 Base 心跳让晚到的重复副本到齐 |
| lifecycle_test.go:439 `TestCloseIngress…` / ext_test.go:119 | sleep 200 / 50ms（负向） | 关入口后不再投递 | 只会漏报 | 保留，注释写明 |
| tier_test.go:62 | 墙钟 2s 内慢档 ≤ 4 拍 | 降档后按 IdlePeriod 跳 | 上限，负载只会更少 | 保留，注释写明 |
| tier_test.go:79 | 墙钟 1s 内 ≥ 15 拍 | 升回快档 | 心跳推迟、不补跑时拍数少 | 20 拍区域心跳里 ≥ 15 拍 |
| tier_test.go:145 | sleep 2 个慢档周期（负向） | 离开后不再换档 | 只会漏报 | `waitBeats(A, 40)` |
| tier_test.go:191 `TestIdleTierGhostSurvivesSlowBeats` | 3s 采样（负向） | 慢档 Ghost 不按 GhostTTL 过期 | Ghost 过期按墙钟；源格饿死超过 0.4s 理论上真的过期 | 保留；未观察到失败 |
| tier_test.go:222 `TestIdleTierFastCrossingHandsOffInsideBand` | 500ms 内 A 出现 Ghost | 接受交接的那笔事务里留下 Ghost | 期限用来区分“接受时留下”与“下一个慢拍补上”，负载下误报 | 等 A 离开冻结，离开那一刻必须是 owner = B 的 Ghost |
| tier_test.go:304 | sleep 3 拍后读 HP | HP 与结算次数一致 | 同攻击场景 | `eventually` |
| group_test.go:212 / 351 | sleep 3 个周期（负向） | 组队前看不见；迟到的旧通知不采纳 | 只会漏报 | 前者保留；后者等 `GroupStale` 增加 |
| group_test.go:245 | 墙钟 1s 内 3～7 份快照 | 一个周期一份 | Base 心跳推迟时少于 3 份 | 见上“队伍快照” |
| group_test.go:264 / 273 | 以停的时刻为起点的上下限 | hold 内不删、过了就删 | 最后一份可能更早发出；过期清理推迟 | 见上“队伍快照” |
| group_test.go:414 / 424 | sleep 3 个周期 | 前提：组视野接上；走出视野后一直由快照供着 | 快照未到时走出视野会删再建 | 等 `GroupSnapsIn` > 0；再等 3 份快照 |
| process_test.go:740 `TestCellsInSeparateProcesses` | sleep 3 拍后读 owner | owner A、epoch 3 | 收尾未完 | `eventually`（Base 的 owner 与最近 witness） |
| process_test.go:742 | 间隔 ≤ 250ms | 同 `checkContinuity` | 同上 | `gapRule`（子进程 tick 事件） |
| process_test.go 攻击前（新增前提） | 看到“回到 9600 以西”的 witness 就停、立即攻击 | 前提：攻击者在攻击距离内、看得见目标 | 控制延迟让玩家多走几百单位，加压时 10 刀全按 OutOfRange（reason 3）被拒 | 以 200/s 闭环走回 x ≥ 9650（到不了交接线），停稳且 A 的 witness 里有目标再攻击 |
| process_test.go:874 `TestBaseProcessGoneSceneActorCleanedUp` | 墙钟 ≤ 2.5s 离场 | 租约 1s 后离场 | 子进程心跳推迟时误报 | A 在 kill 之后的拍数 ≤ 20 + 4 |
| nats_test.go:80 | 250ms | 同 `checkContinuity` | 同上 | `natsGap` |

上一轮已改为闭环、本轮未动：`TestHandoffHysteresisAbsorbsJitter`、`TestIdleTierNoFlappingAtWakeEdge`、`assertSingleReal` / `waitSettled`（期限改为 `settleWithin`）。`measure_test.go` 是测量（默认跳过），不改。

## 变异证明

每个变异临时改实现、跑改后的用例（非 race，`-count=1`），之后 `git checkout` 还原；失败文本原样摘录（只截一行）。

| 变异（实现上真的出错） | 用例 | 改后的用例 |
| --- | --- | --- |
| M1 区域心跳每 5 拍才发一次 witness（真的漏发） | `TestWorldStraightCrossing` | 红：`view gap of 5 beats of cell 101 between view 0 (tick 10) and 1 (tick 15), wall 250.535208ms: exceeds 4 beats` |
| M2 新格装入后 4 拍内不给这个玩家发 witness | 同上 | 红：`… new cell idle 5 beats after the install (limit 2 each), freeze to install 88.334µs (limit 1s)` |
| M3 旧格在交接线前 150 单位就停发（冻结前 3 拍） | 同上 | 红：`old cell idle 4 beats before the freeze …`（另一次 `idle 3 beats`，墙钟间隔 171ms——**原来 200ms 的墙钟上限抓不住这个变异**） |
| M4 新格第一次装入时在锁 Scene 的事务里多等 1.2s | 同上 | 红：`freeze to install 1.202106125s (limit 1s + the old cell's stall 5.016041ms)` |
| M5 伤害去重失效（重复的伤害消息再结算一次） | `TestDamageAcrossBoundaryAppliedOnce` | 红：`hit {1873 8} applied 2 times`（10 刀都是 2 次） |
| M6 升档改了档位标记但心跳没换档（仍按慢档跳） | `TestIdleTierWithoutPlayersAndWakeOnApproach` | 红：`woken npc beat 1 times in 20 region beats, want >= 15 (period 50ms)` |
| M7 队伍快照每拍都发（不管 GroupEvery） | `TestGroupPanelAcrossBases` | 红：`player 2's base sent 10 snapshots in 10 beats (963.038917ms), want between 3 and 6 (one per 200ms)` |
| M8 队伍快照的保留时间只给一个周期 | `TestGroupPanelExpiresWhenTheBaseStopsAndComesBack` | 红：`player 2 left player 1's panel 236.402708ms after its last snapshot, want no earlier than the 600ms hold and within 1s more` |
| M9 保留时间放成 4 倍（过期慢一个量级） | 同上 | 红：`… 2.492269833s after its last snapshot …` |
| M10 冻结超时从不放弃 | `TestHandoffAbortDeletesWithoutResuming` | 红：`timed out after 5s waiting for abort` |
| M11 Leave 不核对入场序号 | `TestLateLeaveDoesNotRemoveReenteredActor`、`…AfterHandoffKeepsTheNewEntry` | 红：`old Leave deleted the re-entered actor (old epoch 1, new epoch 2): present=false`；`old Leave deleted the actor handed over to B: present=false` |
| M12 Base 不重发面板 | `TestPanelCommandLostIsResent` | 红：`timed out after 5s waiting for the first panel command and two resends: base sent 1 panel commands since the refresh` |
| M13 “结果未知”每拍都报 | `TestPanelUnknownWhenUnconfirmed` | 红：`ver 3 unconfirmed past the deadline: reported unknown 10 times, want once` |
| M14 租约按 3 倍才过期 | `TestBaseProcessGoneSceneActorCleanedUp`（多进程） | 红：`SceneActor left 52 beats (2.600151s) after the base died, want within the 1s lease (24 beats) plus a few` |
| M15 交接被接受时不在旧格留 Ghost | `TestHandoffMovesRealAndLeavesGhost` | 红：`A holds {…} (known false), want a ghost owned by B`。`TestIdleTierFastCrossingHandsOffInsideBand` 在这个变异下改前改后都绿（基线原样 `-count=3` 也绿）：慢档 NPC 的交接通常由新格的 Ghost 隐式确认收尾，那份 Ghost 先于结论到旧格，留不留 Ghost 看不出来；不是本轮放宽 |
| M16 Base 接受不更新的回写 | `TestWritebackIsIdempotentAndOrdered` | 红：`writeback 1 version {Epoch:1 Seq:4} not newer than {Epoch:1 Seq:4}` |
| M17 Base 静默看门狗不重进 | `TestCellCrashPlayerReentersFromSnapshot` | 红：`timed out after 5s waiting for the first re-enter to the stopped cell` |
| M18 新格 Ghost 不再隐式确认交接 | `TestHandoffAckLossResolvedByGhostUpdate`（放弃期限 10s） | 红：`entity 1873 is frozen in (0,0) (want Real only in (1,0))`、`A never concluded its handoff` |

纯前提类的改动（`waitKnown`、`spawnAll` 等 Ghost）不改承诺，只让场景从约定的状态开始；它们“不放宽”的证据是同一用例的承诺断言仍对 M5 等变异红。

## 实现缺陷：冻结报告晚到，旧格留下永久的 frozen

提交 `dc88efa9`（`fix(cellworld)`，与用例整治分开）。

**机制（读代码 + 确定性复现）**：角色心跳里冻结（`freeze`）：epoch + 1，提交后的回调里先把 Offer、再把冻结报告排进出站队列（`registerCommit`：sends 在 reports 之前）；出站 goroutine 先发出 Offer，再把报告攒批 `Dispatch(hReports)` 给 Scene。交接结论——OfferReply（`onOfferReply`）或新格 Ghost 的隐式确认（`hImplied`）——在锁 Scene + 角色的事务里直接 `applyReports` 一份 Removed 报告：删本地记录、按位置留 owner = 新格的 Ghost、角色实体销毁。负载下 Scene 的报告事务慢（日志里 `cellworld_reports` 持锁 145～230ms），新格的回复又很快，结论就可能先于那一批冻结报告进区域表。冻结报告到时 `applyReports` 找不到本地记录（`known == false`），按报告新建了一条 `RoleFrozen` 记录，还 `r.ghosts.del` 掉刚留下的 Ghost。之后：

- 角色实体已销毁，没有任何东西再删这条记录：旧格 `Inspect` 永远报 frozen，`waitSettled` / `assertSingleReal` 等满 5s 超时；
- 区域表把本地记录放进空间索引、优先于 Ghost（`beatView.get`），别的玩家的 witness 里它停在冻结点；
- 新 owner 每拍的 Ghost 更新都撞上这条 frozen，`applyGhostBatch` 每次排一个隐式确认给已销毁的角色（`StaleDrops` 每秒涨约 20，日志 `async handler failed … entity not found`）。

**现场**（本轮加压探针，修前二进制）：

```
handoff of player 1 did not settle within 5s:
base: cell=104 epoch=2 inWorld=true lostEpoch=0 enterHint=0; last view from 104 epoch 2
cell (0,0): role=frozen epoch=2 seq=24 x=10105 owner/target=(1,1)
cell (0,0) stats: started=1 accepted=1 rejected=0 aborted=0 offerRetransmits=0 offersReceived=0 staleDrops=116 outboxDrops=0 dispatchErrors=0 actorBeats=163 outbox=0
--- FAIL: TestNATSWorldDiagonalThroughFourCellCorner (7.40s)
```

`accepted=1 aborted=0`：旧格已经得到结论，区域表却仍是 frozen——上一轮“尚未确认”里两个假设（角色心跳停了 / 区域表没收到报告）都不是，是收到的顺序反了。同一探针里 `TestWorldJitterAtBoundary`（`(1,0)` frozen，`started=6 accepted=6`）、`TestWorldOnALoaderBackedGetter/straight`（`(0,0)` frozen，`accepted=1`）同一签名。

**先红**：`TestLateFreezeReportDoesNotResurrectTheRecord`（handoff_test.go）给旧格的 Nest 客户端套一层 `holdFreezeReport`：含该角色冻结报告的那一批 `hReports` 投递挡到旧格 `HandoffsAccepted > 0` 之后再放行，确定性地造出“结论先到、报告后到”。修前 `-race -count=3` 3/3 红：

```
entity 1873 is frozen in (0,0) (want Real only in (1,0))
cell (0,0): role=frozen epoch=2 seq=6 x=10149 owner/target=(1,0)
cell (0,0) stats: started=1 accepted=1 rejected=0 aborted=0 offerRetransmits=0 offersReceived=0 staleDrops=101 outboxDrops=0 dispatchErrors=0 actorBeats=5 outbox=0
A holds {State:{ID:1873 … Ver:{Epoch:2 Seq:6} …} Role:frozen Owner:(1,0)} after the handoff, want nothing or a ghost owned by B
```

**修法**（`region.go` `applyReports`，最小改动）：本地记录只由装入建立（`install`：Offer / Enter / Spawn，都在锁 Scene 的事务里直接写区域表；`tier.go` 只改已有记录）。经出站队列异步到达、找不到本地记录的非删除报告说明记录已被结论 / 离场直接删掉，丢弃并计 `StaleDrops`。同一处也挡住了“离场（直接删）之后迟到的移动报告重建一条 Real 记录”（会给已离场的角色继续发 witness 与 Ghost）——同一类顺序，读代码推断，未单独构造用例。修后 `-race -count=3` 通过。线上格式、存储不变。

**方向判断**：cellworld 区域表的维护分两条路——锁 Scene 的事务直接改（装入、结论、离场、被拒恢复）与角色事务提交后经出站队列异步报告（移动、冻结、放弃、租约离场）。`applyReports` 原来对“同一角色的两次提交后回调乱序”只按 epoch / 版本挡了已知记录的情况，没有覆盖“直接删在前、异步报告在后”。本次补的是这一类的总规则（记录只由装入建立），不是逐个情形打补丁；若以后再出现区域表与角色状态不一致，应考虑让结论 / 离场也走异步报告（单一路径、按版本排序），代价是交接收尾多一跳。

## 稳定性

整包 `GOWORK=off go test -race -count=1 ./gameplay/cellworld`，各版本先编成测试二进制（`go test -race -c`），在同一时刻并行跑，负载取每遍开始 / 结束时的 1 分钟 load average（10 核）。满载进程自己起、按记录的进程号结束。

| 批次 | 配置 | load（范围 / 中位） | 改后 | main `21f631e1` |
| --- | --- | --- | --- | --- |
| 基线摸底 | 8 个满载进程 + 2 份 main | 7～29 | — | 4/4 |
| 中间版（`789b9892` + `dc88efa9`） | 8 个满载进程 + 2 份中间版 + 2 份 main，4 遍 | 8～63 / 约 50 | 5/8 | 0/8 |
| 改后 `1eeac768` | 同上，4 遍 | 24～53 / 41.5 | 7/8 | 3/8 |
| 改后 `c6c9d9cf`（最终） | 同上，3 遍 | 17～31 / 19.4 | 6/6 | 3/6 |
| 改后 `c6c9d9cf`，无额外负载 | 单份，3 遍（每遍约 150s） | 7～24 | 3/3 | — |

高负载合计（后两批，同批同负载）：改后 13/14，main 6/14。

- **改后唯一一次失败**（`1eeac768` 第 2 遍）：`TestHandoffAckLossResolvedByGhostUpdate` 报 `1 handoffs aborted although ghosts confirm them`。这个用例丢掉全部 OfferReply，靠新格 Ghost 隐式确认收尾；缺省 `HandoffAbort` 3s 是协议自己的墙钟期限，负载下 Ghost 晚于 3s 到达，旧格先放弃——是用例让协议期限与负载赛跑，不是实现问题。`c6c9d9cf` 把这个用例的放弃期限设为 `2*settleWithin`（10s），承诺（“结论来自 Ghost 隐式确认、不放弃”）不变，M18 仍红。之后 6/6 + 3/3 未再出现。
- **main 的失败**（14 遍里 8 遍失败）：96 行 `view gap … exceeds …`（墙钟间隔阈值，`TestWorld*` / `TestNATSWorld*` / `TestWorldOnALoaderBackedGetter` / 丢包场景）、2 次 frozen 收尾超时（即本轮修掉的冻结报告晚到）、2 次 `TestLateLeaveDoesNotRemoveReenteredActor` 的 `timed out waiting for actor removed by the current leave`（固定 sleep 后断言），以及 `TestDamageAcrossBoundaryAppliedOnce`、`TestHandoffAckLossResolvedByGhostUpdate`、`TestCellsInSeparateProcesses` 各一类——全在本轮改过的写法范围内。
- **改后 witness 间隔的实测**（三批全部改后日志）：同格相邻最多差 1 拍、换手边缘 1 拍；换手消息段最长 653ms，其中旧格心跳停顿 324ms，墙钟最坏间隔 1.18s——全部落在拆解后的上限内，而墙钟间隔已远超原 200ms。

**`TestWorldStraightCrossing` 冻结 4.4s（任务第 5 项）**：已复现并定位，就是上文“实现缺陷”：旧格 `accepted=1`、区域表仍 frozen，冻结报告晚于结论到达。修后上述各批次改后版本均未再出现 frozen 收尾超时。

**仍可能误报的**：协议自己的墙钟期限——`HandoffAbort`（3s）、租约（`LeaseTimeout` / 多进程用例的 1s）、`GhostTTL`、`AttackDeadline` / `PanelDeadline`——在比本轮更重的饥饿下仍会先于结论触发；这些是被测协议的行为，用例只能把期限放宽到 `settleWithin` 以上（已对 AckLoss 这样做）或接受。各处注释写明。

## 验证

全部 `GOWORK=off`，在本分支最终提交上：

- `gofmt -l gameplay/cellworld` 空；`go vet ./gameplay/cellworld` 通过。
- 整包 `go test -race -count=1 ./gameplay/cellworld`：见“稳定性”（无负载 3/3，高负载 6/6）。
- 根包 `go test -count=1 .` 通过。
- `python3 scripts/check-doc-anchors.py` 0 断链。
- 变异：M1～M18 各自在改后用例上红（上表）；变异只在临时副本里打，提交里没有。


## 合入前复核（2026-11-10，合并 main db33ebeb 之后）

整包 `-race` 两遍通过；根包通过；生成 game-demo 全量测试通过（35 个包）；私有环境的开地形分区演练 6 步通过、不开地形的无缝演练 14 步通过，最后的普通场景机器人压测阈值失败（p95 27.0s > 16s，同机同时还有两轮 agent 在跑），即 KNOWN-LIMITS 里已登记的“时过时不过”，与本轮无关。
