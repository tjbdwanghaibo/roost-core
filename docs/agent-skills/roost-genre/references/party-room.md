# 实时派对房间

手册：`docs/framework/PARTY-ROOM-INTEGRATION.md`（下称 PARTY §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求：本篇命令序列（18 条场景，含补位）v1.34.1 起可用（2026-10-31 发布；发版前在 main `24a9dbc3` 实测 18 条 PASS，v1.34.1 与它代码相同）。** 分档：
> - **v1.33.2**：最早有这条纵切（12 条场景，房间同步靠夹具自建 Manager，匹配固定人数）；
> - **v1.34.0**：弹性成局（`Queue.MinSize` / `FillAfterSeconds`）与组同步正式入口（`access.player.sync_groups`）起可用，生成物最低 core 升到 v1.34.0，纵切 13 条；没有补位；
> - **v1.34.1（或 main `24a9dbc3` 之后）**：业务层补位（PARTY §6A），纵切 18 条。
>
> 三档的夹具文件与命令不同（v1.34.0 起没有 `sync.go.txt`、要加 access player / TCP / self_sync / sync_groups；补位版多 `backfill.go` 与 `game/partymatch`），**按目标版本检出里的 `scripts/test-party-room-generated.sh` 走**，不要拿本篇命令套旧版本。目标早于 v1.33.2 时告诉用户升级并停下，不要从别的夹具拼一个。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 2～8 人短局、服务器权威房间：抢答、计时、回合制、轻反应类聚会游戏（夹具是“抢星”：随机时刻放星、先抢得分、抢跑犯规、三回合 + 加时 + 硬期限）；弹性人数匹配成局（2～8 人、等 20 秒）或房间码开房、房主转移、断线宽限重连、局内同一份记分板推给全体成员（与玩家自身视图同一条连接）、开局后有人离开时第 1 回合内补位、结算发奖与座位归还 | 依赖 >20Hz 实体心跳、逐帧确定性或客户端预测的动作 / 物理对抗（该心跳调度器最短 50ms，不是 on_change Sync 发送上限，PARTY §1、§8）：要逐帧确定性走 [moba-lockstep](moba-lockstep.md)，物理闯关看 `PHYSICS-ROOM-SERVICE.md`（需 PhysX native）；要 UDP 接入、房间跨机接管、存储保证不超员的补位、大厅列表 / 短码服务的，框架没有，见 §5 |

与 [room-coop-pve](room-coop-pve.md) 的区别：那三篇每一步都由持久定时器推进；本篇在线推进靠**实体心跳**（100ms 档），持久定时器只做开局硬期限兜底（PARTY §1、§4）。

## 2. 用到的框架模块（PARTY §2）

| 业务 | 模块 | 出处 |
| --- | --- | --- |
| 排队、成局 | `service/match`：`Enqueue`（同 requestID 重放同一张票）→ `Candidates` → `Grouping` → `Commit`（同一票集合并发只成一局）。**弹性人数队列**（v1.34.0 起）：`Queue{GroupSize: 8, MinSize: 2, FillAfterSeconds: 20}`，凑满 8 人立即开，不满时最早一张票等满 20 秒（业务时钟）后 2～7 人也开；存储按同一规则（`Queue.FormableSize`）拒绝过早的不满员局（`ErrGroupNotReady`，550110）；逻辑队列人数段写 `2-8@20s`（`match.sweep_queues` 条目如 `party:2-8@20s:eu`）；`FirstComeGrouping` 用于弹性队列时要给 `NowUnix`；预判“候选够不够一局”用 `queue.MinGroupSize()` | PARTY §2、§6；guide/09-services.md §4“match 的弹性人数成局” |
| 玩家在哪个房间 | `service/session` Run：`Enter`（RequestID 必填、全局唯一）+ `Attach` 房间资源；重连 `Current`；`Finish` / `Leave` 经业务 `Releaser` 归还座位 | PARTY §2、§6；guide/09-services.md §1A |
| 房间权威状态（名单 `Roster`、规则快照、阶段、房主、座位、种子、回执、补位事实 `Backfill`） | PartyRoom 实体 DAO `State`（persist）+ `WakeTimer`，World 锁档 | PARTY §2 |
| 公开记分板 | DAO `Board`（`nopersist,sync`）+ 业务 packer 只开放 `Board` 一个视图（`RoomSyncProfile`） | PARTY §2、§5 |
| 回合推进、出星、断线宽限 | `roost add heartbeat --entity PartyRoom`（100ms 档），回调在实体锁与事务内 `Advance(beat.Now)` | PARTY §4；guide/10-time.md §4A |
| 开局硬期限 | `roost add timer --entity PartyRoom`，开局登记、结算即删 | PARTY §4；guide/10-time.md §4 |
| 领奖 | Player + 房间多实体 `MultiSync_PartyClaim`，strict | PARTY §7 |
| 房间内同步（v1.34.0 起正式入口） | roost.yaml `access.player.self_sync: true` + `sync_groups: [party_room]`（需要 `add access player` + `add transport tcp`）；`project sync` 在实体包创建一次业务文件 `sync_group.go`（`SyncGroup`，`wiring/selfsync.Groups`，装在 self_sync 的同一个 Manager 上），handler 里 `SyncGroup.Join / Leave / Disband`，在 Nest 事务里调用时**提交后才生效**、回滚不发生。与玩家自身视图同一条连接、同一个推送编号（`msgid.MsgSelfSync`），客户端只用一个 `frame.Receiver`，按每个更新的 `Namespace`（`party_room` / `player`）+ `Profile.Key` 分流。**不要再自建 Manager 交给 Nest**：带 Entity Sync 的 Nest Mod 只有一个观察者位置，`Provide` 报 `ErrEntitySyncObserverConflict` | PARTY §5；ENTITY_SYNC.md“按组同步与 self_sync 共用连接（Groups）”；CHANGELOG v1.34.0 |
| 补位（v1.34.1 起，业务层写法） | 无框架 API。房间状态 `Backfill` + handler `PartyBackfillOffer` / `PartyBackfill` 在实体锁内裁决；宿主与匹配循环 `game/partymatch` 在事务外把空座挂成补位队列（`Queue{Mode: "party-backfill", GroupSize: 8, MinSize: 2}`，不等待）里的**房间票**（requestID `backfill:<房间 ID>:<轮次>`，Score = 空位数），一张房间票 + 若干玩家票弹性 `Commit`，成局交房间按座位逐个接受，多出的人被拒并按 `requeue:<Match ID>:<玩家>` 重排；截止 = 第 1 回合结束，过后匹配循环不再配人、宿主撤票、在途补位局进房全部拒绝 | PARTY §6A；guide/09-services.md §4 末段 |
| 业务时间 | handler 约定参数 `nowUnixMilli`（分发包装从 fctx 填，Sender 不带时间）；心跳 `Beat.Now`、定时器 `Fire.Now` | PARTY §3.3；guide/10-time.md §1A |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后（已 `cd "$DIR"`）。对应 `scripts/test-party-room-generated.sh`（main `24a9dbc3`）的缺省模式，步骤与顺序照抄脚本，只把脚本里的 python 片段换成 §3.2 的 helper 与 perl。

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add entity PartyRoom
"$ROOST" add dao PartyRoom --entity PartyRoom
"$ROOST" add heartbeat --entity PartyRoom
"$ROOST" add timer --entity PartyRoom
# Coins 标 sync：自身视图（self_sync 的默认 packer）把它推给本人，与房间组同步共用连接
dao_add Player db/def/player.go "Coins int64 coins persist,sync" "Matches int64 matches" "Initialized bool initialized"
dao_add PartyRoom db/def/party_room.go "State string state" "WakeTimer int64 wake_timer" "Board string board nopersist,sync"
# World 锁档 + 房间作为 Entity Sync subject，用业务 packer 只同步 Board
perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryWorld sync=true syncNamespace=party_room subjectPacker=NewPartyRoomBoardPacker/' \
  game/entities/party_room/entity.go
grep -q 'subjectPacker=NewPartyRoomBoardPacker' game/entities/party_room/entity.go
F="$CORE/scripts/testdata/party-room"
mkdir -p game/partyroom game/partymatch
for n in rules state backfill rules_test; do cp "$F/$n.go.txt" "game/partyroom/$n.go"; done
# 心跳登记、定时器回调是 roost add 只在缺失时创建的业务文件，用夹具覆盖
for n in room heartbeat_beats timer_handlers board_packer; do cp "$F/$n.go.txt" "game/entities/party_room/$n.go"; done
cp "$F/handlers.go.txt"      game/handler/party_room.go
cp "$F/gameplay_test.go.txt" game/handler/party_room_test.go
# 补位宿主与匹配循环：必须在 project sync 之前放进工程——project sync 跑 go mod tidy，handler 测试 import game/partymatch，
# 缺了它 tidy 会去网上找 <module>/game/partymatch 而失败（2026-10-31 实测）
cp "$F/backfill_match.go.txt" game/partymatch/backfill.go
fix_mod party-room-practice game/partyroom/*.go game/partymatch/*.go game/entities/party_room/*.go game/handler/party_room*.go
gofmt -w db/def/*.go game/partyroom/*.go game/partymatch/*.go game/entities/party_room/*.go game/handler/party_room*.go
# 房间内同步走正式入口：实体标记与 packer 就位之后再打开（project sync 的就绪检查要求 sync=true）
"$ROOST" add access player --service game
"$ROOST" add transport tcp
perl -0pi -e 's/^(    player:\n        service: game\n)/$1        self_sync: true\n        sync_groups:\n            - party_room\n/m or die "access.player missing in roost.yaml\n"' roost.yaml
"$ROOST" project sync
# project sync 创建一次的 sync_group.go 是业务文件：换成本例视图 RoomSyncProfile 与座位上限
cp "$F/sync_group.go.txt" game/entities/party_room/sync_group.go
fix_mod party-room-practice game/entities/party_room/sync_group.go
gofmt -w game/entities/party_room/sync_group.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestPartyGame' ./game/partyroom ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: TestPartyGame` 恰好 **18** 条（`game/partyroom` 3 条规则 + `game/handler` 15 条 Nest / 心跳 / 同步 / 补位）、0 skip；然后按 SKILL.md §3.4 收尾（`<前缀>` = `PartyGame`）。仓库脚本另把 `scripts/testdata/party-room/persistence/*_test.go` 复制为 `game/handler/party_room_*_test.go`、`fix_mod` / `gofmt` 后 `go vet ./game/handler`（真实模式夹具只编译）；一档 demo 可省。

- 场景前缀必须是 `TestPartyGame`：`add timer --entity PartyRoom` 会生成 `TestPartyRoomTimerTickHandlerMatchesTheDriver`，用 `^TestPartyRoom` 会多跑一条、计数对不上（PARTY §9）。
- 本篇 Player 字段是 `Coins`（`persist,sync`）/ `Matches` / `Initialized`（不是别篇的 `Gold`）；同一工程叠加别篇夹具时要合并 Player 字段，不能各拷各的。
- 顺序：`game/partymatch` 与全部夹具（含 `party_room_test.go`）在 `project sync` 之前就位并 `fix_mod`。放到 `project sync` 之后时它报 `project files synchronized but framework resolution failed ... go mod tidy: exit status 1`（`cannot find module providing package <module>/game/partymatch`）。实体标记（`sync=true` + packer）也要先改好，`project sync` 才过 `sync_groups` 的就绪检查。
- roost.yaml 的 perl 改写依赖 `add access player --service game` 写出的缩进（`    player:` 下一行 `        service: game`）；改不到时 `die`，不要静默继续。
- `sync_group.go` 是 `project sync` 只在缺失时创建的业务文件，生成的缺省版本视图键是 `party_room`，本例 packer 只认 `RoomSyncProfile`，所以必须用夹具覆盖；覆盖后 `fix_mod`（它 import `game/partyroom`）。
- 与 v1.33.2 版命令的差别：没有 `sync.go.txt`（自建 Manager 已删）；多 `backfill.go`、`game/partymatch/backfill.go`、`sync_group.go`；多 `add access player` / `add transport tcp` / roost.yaml / `project sync` 四步；`Coins` 改为 `persist,sync`。

## 4. demo 的最小玩法范围（PARTY §3.4，18 条）

| 场景（`TestPartyGame…`） | 学到的框架契约 |
| --- | --- |
| `RulesValidateAndRoomCode` / `RulesVariableStepEquivalence` | 规则开房时校验一次、按值冻结；按任意步长推进到同一时刻状态逐字节相同（心跳可变步长，按事件时间结算） |
| `MatchmakingOpensOneRoomPerMatch` | 2～8 人弹性队列：四人等不满 20 秒策略不出组、提交报 `ErrGroupNotReady`，业务时间推进 20 秒后成局；重放 / 并发 Commit 只成一局，同一 Match 并发开房只一个房间；session 一人一个 Run、`Current` 带回房间 |
| `CodeRoomFullAndLateJoinRejected` / `HostLeaveTransfersHost` | 被拒命令整体回滚（State、Board、心跳、期限都不变）；房主是房间里的业务事实 |
| `HeartbeatDrivesRoundsAndGroupSync` | 心跳回调在实体锁与事务内；Board 经提交门进入 Flush；四个客户端（正式 `frame.Receiver`）与服务端逐字节一致、只见 `board` |
| `FirstGrabWinsAndDuplicateInputDedup` / `FoulEliminationAndUndoRollback` | 单实体串行 + 业务序号去重；undo 覆盖 DAO、心跳启停、定时器增删 |
| `ReconnectWithinGraceReturnsToSameRoom` / `GraceExpiryAndMidGameLeaveEndEarly` | 连接代与票号存在房间里；断线后成员关系保留，会话重开的那个同步周期框架重新订阅（业务不再调用）；离开者提交后退组、收到 remove |
| `SettlementRewardsOnceAndRoomDestroyed` | 多实体领奖每人一次（4×4 并发）；`session.Finish` → `Releaser` 归还至少一次、handler 幂等；最后一个归还提交后 `Disband`、成员收到 remove，再卸载 |
| `SelfViewAndRoomShareOneConnection` | self_sync 与房间组同一个 Manager、同一个推送编号：首个全量同时带自己的 Player 与房间；改金币只推本人；重连一个全量带回两路；离开房间只撤房间 |
| `MatchDeadlineTimerForcesSettlement` | 心跳停摆时持久期限照样结算；两条路径调用同一个 `Advance` |
| `RulesBackfillSeats`（补位） | 补位纯规则：房间码房间 / 满员不挂票；离开后开第 1 轮（截止 = 第 1 回合结束）；别轮次成局全拒；超员与已在房间的人被拒；同一成局重投结果不变；回合结束后撤回、迟到成局全拒 |
| `BackfillAfterMidGameLeave`（补位） | 弹性 `Commit` 成补位局；房间票排队身份 = 房间 + 轮次（重复 SyncRoom 是同一张票）；房间按 Match ID 识别重投；补位的人有 Run、连上后能抢星 |
| `BackfillConcurrentMatchesAdmitOne`（补位） | 同一张房间票只成一局（另一个 `ErrConflict`、玩家票仍在等待）；超员的补位局在匹配服务里照常成功，由进房 handler 按座位“一人进、一人拒”，被拒的人 `requeue` |
| `BackfillDeadlineWithdrawsTicket`（补位） | 截止后匹配循环不给过期房间票配人、宿主撤票；票已被消耗时撤票得“已匹配”（不是错误），迟到的补位局进房全部被拒、玩家重排 |
| `BackfillPlayerJoinsGroupWithFullSync`（补位） | 补位的人连上前不是组成员；`PartyConnect` 提交后 Join，新连接收到房间 + 自己 Player 的全量，之后都是增量 |

最小可讲的玩法：弹性匹配成局（2～8 人）→ 开房 + 每人一个 Run → 心跳推进三回合抢星（记分板与自身视图同一条连接）→ 第 1 回合有人离开时补位 → 结算 → 每人领奖一次 → 归还、解散同步组、关房。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 一房一锁串行裁决、心跳与持久期限双驱动、undo 覆盖 DAO + 心跳启停 + 定时器、多实体领奖原子幂等、组同步经提交门推同一份视图（与自身视图共用连接）、match 弹性成局与 session 正式领域语义 | PARTY §12“证明” |
| 心跳 ≤ 20Hz | 档位固定 50ms～60s 十档，最短 50ms；本心跳调度器没有更高频档位，不代表 on_change Sync 有 20Hz 硬上限 | PARTY §8 第 1 行；`gameplay/heartbeat/tier.go` `Periods`；guide/10-time.md §4A |
| 可变步长 | 上一拍没跑完就跳拍、不补发、不追帧，`Elapsed` 夹在 `[0, max(1s, 档位)]`；规则按时间戳与事件顺序写，不按“第几拍”累加 | PARTY §8 第 2 行；KNOWN-LIMITS“实体心跳（D20 第 1 期）” |
| 心跳不持久 | 重启 / 卸载后要在 `onHeartbeatInit` 按持久阶段重新启动（REAL 用例去掉这行即失败）；心跳缺省 `async`，丢最后几拍由下一拍按时间重算 | PARTY §4、§13 |
| 只有 TCP | 玩家接入层（嵌入 TCP、独立 Gate）只提供 TCP；KCP / UDP 只生成装配函数，监听、握手鉴权、Session 绑定宿主自写 | PARTY §8 第 3 行；GATEWAY.md；SKILL.md §5 #10 |
| 房间无跨机接管 | 房间是开房进程的本地实体，崩溃后只能同一份 WAL / Mongo 冷加载恢复，没有自动迁机；Player 静态绑定 sid | PARTY §8 第 4 行；guide/09-services.md §4 |
| SyncBus 可丢 | 至多一次 / 有限重投；房间内同步不走 SyncBus，跨服务房间事件（大厅、好友在玩）要业务对账 | PARTY §8 第 6 行；guide/04-sync.md §5 |
| 房间同步：**v1.34.0 起已提供正式入口**，剩余边界 | `access.player.sync_groups` + `wiring/selfsync.Groups`（§2）。剩余：**要求 self_sync**（会话归自身视图；不开 self_sync 的进程仍用 `policy.Group` 并自己开关会话）；**成员关系不持久**，重启后靠玩家重连的 `PartyConnect` 再 `Join`；离线成员每个政策阶段复查一次，成本与有离线成员的组的人数成正比，无房间数 / 成员数容量数据；**独立 Gate 下的组同步、慢消费者断开没有组同步自己的用例**（真实 TCP 只有 `wiring/selfsync` 的 `TestGroupsShareTheSelfSyncConnection`）；掉线出局的人宽限期满不自动退组 | PARTY §5、§6A 局限、§8 第 5 行；ENTITY_SYNC.md“Groups”限制；KNOWN-LIMITS“游戏接入验收”表下房间内同步一条 |
| 匹配：弹性成局**已提供（v1.34.0）** | “凑满 N 立即开、最早一张等满 T 秒后 M..N-1 人也开”。边界：等待按 Unix 秒取整（最多提前不到 1 秒）；存储按自己的时钟判，匹配器与存储业务时钟不一致时策略认为能开的局可能被 `ErrGroupNotReady` 拒、下一轮再试；`FillAfterSeconds` 不得 ≥ `match.ticket_ttl`；弹性字段只有新服务端认识，**先升级匹配服务再用弹性队列** | guide/09-services.md §4；KNOWN-LIMITS“匹配的弹性人数成局与补位” |
| 补位：**存储原生补位未提供**，业务层补位（v1.34.1 起）的局限 | ①**不超员靠进房 handler 按座位拒绝，不是存储保证**：超员的补位局在匹配服务里照常成功，被拒的人要多排一次队（要框架保证时等方案 A，未提供）；②**不跨分区**：房间票与玩家票要在同一分区，`CommitAcross` 不用于补位；③**一人两队列都可能成局**：补位与普通成局的优先级由业务匹配循环决定，存储不跨队列检查同一玩家，一名玩家同时在两个队列各有一张票会被两边各成一局——业务要保证一人只排一个队列（本例只有选“快速加入”才进补位队列）；④**截止无回调、需对账**：截止常由心跳推进，心跳回调不做 I/O，宿主得不到通知，要在离开 / 补位进房后调 `SyncRoom` 并**周期性对账**撤票，期间过期房间票留在队列里（匹配循环不再给它配人）直到撤回或票期限到；⑤本例只补主动离开的座位（掉线出局的座位、房间码房间不补），座位记录上限 `MaxSeatRecords`（16）；⑥只在内存存储、单进程匹配循环上验证，Redis 存储上的补位、多进程匹配器、补位吞吐没跑 | PARTY §6A“局限”“截止与撤票”、§8 第 7 行；KNOWN-LIMITS“匹配的弹性人数成局与补位” |
| 推送带宽 | Board 带剩余毫秒时 `playing` 期间每 100ms 每房间一个增量；要省就让客户端按回合结束时刻自己倒计时 | PARTY §5 |
| 容量 / 验证边界 | 没有房间数、心跳负载、推送带宽数据；组同步在纵切里经替身接入层（不经嵌入 TCP / 独立 Gate、无慢消费者）；match / session 只测内存存储；提交在途崩溃未测；真实依赖模式（`ROOST_PARTY_ROOM_REAL=1`）在改用组同步入口后未重跑 | PARTY §12“不证明”、§13；KNOWN-LIMITS“游戏接入验收”派对房间行 |
| 业务要自己补 | 玩法规则、房间码短码服务（夹具码是房间号 36 进制、可枚举）、大厅列表、Match → 房间目录（夹具用散列）、接入层票号签发、掉线托管 / AI 补位、观战、跨 sid 发奖（走 Saga / effect；Saga 业务键保留期后可复用，见 SKILL.md §5 #13）、补位宿主的周期性对账 | PARTY §6、§6A、§7、§8 末两行、§14 |

## 6. 常见坑（PARTY §5、§6A、§9）

- 场景前缀撞生成测试名：用 `TestPartyRoom` 前缀会把 `TestPartyRoomTimerTickHandlerMatchesTheDriver` 一起跑进去（§3）。
- session 的 `RequestID` 是全局身份：多名玩家共用同一个会冲突，夹具用 `Match ID + ":" + 玩家号`（补位进房同一写法）。
- 锁接口名不含 `Entity`：`generate` 报 `found no entity parameter`（夹具 `IPartyRoomEntity` / `IPartyPlayerEntity`）。
- 测试 goroutine 直接读 DAO：心跳在并发写，`-race` 报出；经只读 Nest handler 读（夹具 `partySnapshot`）。
- 让调用方传 `now` / 测试里传旧时间：handler 用 `nowUnixMilli`，测试用 `clock.SetSource` 拨并在 `t.Cleanup` 恢复。
- 心跳回调里做 I/O 或同步 Nest 请求：与 handler 一样在实体锁内；同步、session、匹配、补位排队 / 撤票都在事务外调。
- 三个写入方（handler、心跳、定时器）各自管驱动：只经 `SaveRoom` 一个入口，让心跳 / 期限跟阶段一致；只用 `Start` / `Stop`，`Reg.Close` 不随事务回滚（PARTY §4）。
- 自建 `entitysync.Manager` 经 `NestOptionWithEntitySync` 交给带 self_sync 的 Nest：v1.34.0 前被**静默覆盖**（收不到提交与回滚），v1.34.0 起 `Provide` 报 `ErrEntitySyncObserverConflict`。房间同步用 `sync_groups`；直接把 `policy.Group` 挂到 self_sync 的 Manager 上会在登录前 `Join` 被拒、重连后房间静默断供（ENTITY_SYNC“Groups”）。
- `SyncGroup.Join / Leave / Disband` 在事务里调用会推迟到提交后（回滚不发生），不用业务自己挂提交后回调；事务外调用立即生效。
- 弹性队列的匹配器预判写死 `GroupSize`：不满员的局永远凑不出来，用 `queue.MinGroupSize()`；`FirstComeGrouping` 用于弹性队列必须给 `NowUnix`，不给时 `Group` 报 `ErrQueueInvalid`（`service/match/grouping.go`）。
- 补位：一名玩家同时排普通队列与补位队列会被两边各成一局；补位截止过后不撤票，过期房间票一直挂在队列里（不会被配人）——宿主要周期性 `SyncRoom` 对账。

## 7. 验证

- 一档：§3 的 18 条 PASS + SKILL.md §3.4。2026-10-31 在 main `24a9dbc3`（v1.34.0 之后、补位已合入、未发版）、自定 module 按 SKILL.md §3.2 + 本节命令 + §3.4 原样抽 bash 块连跑：18 PASS（`game/partyroom` 3 + `game/handler` 15）、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL；persistence 夹具 `fix_mod` 后 `go vet ./game/handler` 通过。同次把 `game/partymatch` 挪到 `project sync` 之后重跑一遍作对照，`project sync` 的 `go mod tidy` 失败（见 §3）。
- 二档：`ROOST_PARTY_ROOM_REAL=1 bash scripts/test-party-room-generated.sh`（隔离 Mongo 副本集，`ROOST_DATAENGINE_IT_MONGO_URI`）：局内 SIGKILL → 冷加载、心跳按持久阶段重启并结算、发奖、DataEngine 删档 → 第三进程核对（PARTY §13）。仓库记录改用组同步入口后真实依赖模式未重跑；本 skill 未跑。
- 缺省模式不证明：无 WAL / Mongo、房间只做内存卸载、组同步的接入层是替身（真实 TCP 只在 `wiring/selfsync` 包测试）、match / session 内存存储、补位的“两个匹配器”是同进程 goroutine、单进程（PARTY §12）。

## 8. 手册指针

- `docs/framework/PARTY-ROOM-INTEGRATION.md`：§2 状态归谁、§3 生成 / 阅读顺序 / Sender 示例、§4 心跳与持久期限、§5 SyncGroup、§6 匹配会话重连、§6A 补位（流程、为什么不超员、幂等身份、截止与撤票、局限）、§7 结算销毁、§8 框架没有什么、§9 接入坑、第二部分验收
- `docs/framework/ENTITY_SYNC.md`“按组同步与 self_sync 共用连接（Groups）”；`docs/framework/guide/04-sync.md`“房间、队伍：按组同步（与自身视图同一条连接）”
- `docs/framework/guide/09-services.md` §1A（session）、§4（match 弹性人数成局、分区、补位指针）
- `docs/framework/guide/10-time.md` §1A（业务时间）、§4（实体定时器）、§4A（心跳）
- `docs/framework/TOWER-DEFENSE-INTEGRATION.md` 第一部分（房间实体、命令序号、多实体领奖讲得最细，PARTY §1 建议先读）
- `docs/maintenance/KNOWN-LIMITS.md`“匹配的弹性人数成局与补位”、“游戏接入验收”派对房间行与表下房间内同步一条；`docs/maintenance/FEATURE-2026-10-31-match-flexible-grouping.md`（方案 A / B）、`FEATURE-2026-10-31-room-sync-groups.md`

## v1.35.0 起的公共 Robot 接线

手工生成 party-room 时，在最终 `roost generate` 前执行（`genre` 取本篇对应脚本名）：

```sh
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" "$genre"
```

该步骤安装业务 PB、已鉴权 TCP → Sender 接线、`loadtest.Commands`、`cmd/robot` 与网络验收；之后仍须正式 generate。自定义 module 由安装器读取 go.mod。最稳妥的完整顺序直接使用同检出的玩法脚本。新增测试覆盖两个动作、重试、TCP 重连、非法命令和不存在玩家；不是完整业务恢复或真实存储验收。

验收数量与限制统一见 [Robot 手册](../../../framework/ROBOT.md)，不要沿用历史版本的通过数。
