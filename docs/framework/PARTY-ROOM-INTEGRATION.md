# 实时派对房间框架学习与验收

本手册把“2～8 人短局实时小游戏”的主要业务路径映射到 core 的正式接口，分两部分：**第一部分“学习”**按成局、进房、房间逻辑帧、房间内同步、断线重连、结算与销毁的顺序讲框架怎么用、为什么这样设计；**第二部分“验收”**说明脚本证明什么、不证明什么。下面的业务夹具（“抢星”：每回合服务端在随机时刻放出一颗星，星星出现后第一个抢到的人得分，抢跑记犯规、犯规两次淘汰；三回合后唯一领先者胜，平分进入加时，开局 30 秒硬期限强制结算）只是学习和验收样例：框架与生成器不包含任何派对玩法规则，生成器也不生成这些业务文件；完整的参考应用仍是 [game-demo](../../demo/README.md)。其他游戏类型的手册见[模块目录](README.md#学习文档按游戏场景学框架)。

## 第一部分：学习

### 1. 适合谁、先读什么

适合准备用 core 做派对、聚会、抢答、躲避类短局房间玩法的后端工程师。先读[入门](../GETTING-STARTED.md)、[Nest / Entity](guide/02-nest-entity.md)、[Sync](guide/04-sync.md) 与[时间与定时器](guide/10-time.md)；第一次学建议先读[合作塔防手册](TOWER-DEFENSE-INTEGRATION.md)的第一部分（房间实体、命令序号、多实体领奖讲得最细），再读本篇。

本类型选择**服务器权威房间 + 实体心跳**：一个房间实体拥有座位、阶段、回合与计分，玩家输入都在这个实体的 Nest 锁内裁决；回合推进由房间自己的心跳（100ms 档）按业务时间完成，结果经 Entity Sync 的 `policy.Group` 推给房间全体成员。与塔防的区别是：塔防每一步都是持久定时器触发，本例在线推进靠心跳、持久定时器只做硬期限兜底。需要逐帧确定性、客户端预测或 >20Hz 权威模拟的玩法不适合这条路，看 [MOBA](MOBA-INTEGRATION.md) 的 Lockstep 或第 8 节。

### 2. 模块分工：派对房间里的状态归谁

| 业务状态 | 拥有者 | 框架接法 | 业务边界 |
| --- | --- | --- | --- |
| 排队与成局 | `service/match` 的 Store | `Enqueue`（同一 requestID 重放返回同一张票、一人一张有效票）→ `Candidates` → `Grouping` → `Commit`（同一票集合并发提交只成一局）；本例是弹性人数队列 `Queue{GroupSize: 8, MinSize: 2, FillAfterSeconds: 20}`：凑满 8 人立即开，不满时最早一张票等满 20 秒（业务时钟）后 2～7 人也开，存储按同一规则拒绝过早的不满员局（`ErrGroupNotReady`） | 已开局房间的中途补位没有（第 8 节） |
| 玩家在哪个房间 | `service/session` 的 Run | 每名成员一个 Run（`Enter`，请求 ID 必填且全局唯一）+ `Attach` 房间资源；重连用 `Current` 找回房间；`Finish` / `Leave` 触发业务 `Releaser` 归还座位 | 一名玩家同时只有一个有效 Run；Run 的期限按秒，`TTL` 要长于一局 |
| 房间：名单、规则快照、阶段、房主、座位（分数、犯规、出局、连接代、最后一条输入回执、领奖 / 归还标记）、回合时刻、种子 | PartyRoom 实体 DAO 的 `State`（persist） | `roost add entity PartyRoom` + `add dao`；锁档 World，领奖时与 Player 同一事务 | 整段业务 JSON，严格解码；种子与星星出现时刻只在这里 |
| 公开记分板 | PartyRoom DAO 的 `Board`（`nopersist,sync`） | 每次写 `State` 时由 `SaveRoom` 一起算出；业务 packer 只开放 `Board` 一个视图 | 不落库，冷加载后由下一拍重算 |
| 回合推进、星星出现、断线宽限 | PartyRoom 的实体心跳 | `roost add heartbeat --entity PartyRoom`，100ms 档，回调在实体锁与事务内按 `Beat.Now` 推进 | 心跳不持久；判定只看时间戳，不按“第几拍” |
| 开局硬期限 | PartyRoom 的持久实体定时器 | `roost add timer --entity PartyRoom`；开局登记、结算即删 | 心跳没在跑（冷加载、驱动停摆）时由它兜底结算 |
| 永久金币、完赛次数 | Player DAO | 多实体 `MultiSync_PartyClaim(player, room)`，strict | 局内分数与永久金币只在领奖事务里跨过去 |
| 房间内同步 | 业务的 `PartyRoomSync`：一个 `entitysync.Manager` + 每房间一个 `policy.Group` | Manager 作为 Nest 的 `EntitySync` 观察者（提交后才发布）；成员 `Join` / `Leave` 在事务提交后调用 | 生成器不生成这一层（第 8 节） |

```mermaid
flowchart LR
    M[match: Enqueue → Commit] --> O[Nest 锁 PartyRoom: 开房]
    O --> S[session: Enter + Attach 房间]
    C[已鉴权连接] --> K[Nest: Connect 记连接代] --> J[Group.Join]
    I[玩家输入] --> N[Nest 锁 PartyRoom: Advance(now) → Apply]
    H[心跳 100ms] --> B[Nest 锁 PartyRoom: Advance(now)]
    T[硬期限定时器] --> B
    N --> W[SaveRoom: State / Board / 心跳启停 / 期限]
    B --> W
    W --> F[提交后 Manager.Flush → 全体成员同一份 Board]
    W --> R[结算 → 多实体领奖 → session.Finish → Releaser 归还 → 关房 → 销毁]
```

### 3. 如何使用

#### 3.1 生成工程从哪来

在仓库根目录使用 go.mod 声明的工具链运行：

```bash
ROOST_PARTY_ROOM_KEEP=1 bash scripts/test-party-room-generated.sh   # 保留生成工程以便阅读；不加 KEEP 时成功即删除
```

脚本打印临时目录，生成工程在其中的 `project/`。它做的事就是一个新工程的接入步骤：

1. `roost project new party-room-practice -module example.com/party-room-practice -template game`；
2. `roost add dao Player --entity Player`、`roost add entity PartyRoom`、`roost add dao PartyRoom --entity PartyRoom`、`roost add heartbeat --entity PartyRoom`、`roost add timer --entity PartyRoom`；
3. 业务只改这几处：`db/def/player.go` 加 `Coins` / `Matches` / `Initialized`（persist）；`db/def/party_room.go` 加 ``State string `bson:"state" dao:"persist"` ``、``WakeTimer int64 `bson:"wake_timer" dao:"persist"` `` 与 ``Board string `bson:"board" dao:"nopersist,sync"` ``；把 `game/entities/party_room/entity.go` 注解的 `category=entity.EntityCategoryOther` 换成 `category=entity.EntityCategoryWorld sync=true syncNamespace=party_room subjectPacker=NewPartyRoomBoardPacker`；放入业务包 `game/partyroom/`、实体包里的 `room.go` / `heartbeat_beats.go` / `timer_handlers.go` / `board_packer.go`（后两个心跳、定时器文件是 `roost add` 只在缺失时创建的业务文件），handler `game/handler/party_room.go` 与同步接法 `game/handler/party_room_sync.go`；
4. `roost generate` 生成组件、访问器和 Sender，不手改生成文件。

生成的 `internal/bootstrap/generated.go` 已用 `wiring/heartbeat` 与 `wiring/entitytimer` 装配两个驱动（Nest 可用后启动、停机时先停），业务时间都是 `fctx.Now`。夹具的测试不起整个 App，所以在用例里手动 `Bind` / `Start`，业务时间用 `clock.SetSource` 拨（handler 的 `nowUnixMilli`、心跳的 `Beat.Now`、定时器的 `Fire.Now` 都读它，[时间 §1A](guide/10-time.md#1a-业务时间一律取自-ctxd22-1)）。

#### 3.2 阅读顺序

| 顺序 | 文件 | 先回答的问题 |
| --- | --- | --- |
| 1 | `game/partyroom/rules.go`（[rules.go.txt](../../scripts/testdata/party-room/rules.go.txt)） | 规则怎样校验、怎样按值冻结进房间？房间码是什么？ |
| 2 | `game/partyroom/state.go`（[state.go.txt](../../scripts/testdata/party-room/state.go.txt)） | 房间存了什么、记分板裁掉什么？`Advance` 怎样按事件时间顺序结算宽限到期、回合结束与硬期限？输入去重、连接代、房主转移、提前结算、领奖与归还的规则？ |
| 3 | `game/entities/party_room/room.go`（[room.go.txt](../../scripts/testdata/party-room/room.go.txt)） | 为什么房间只有一个写入口 `SaveRoom`？心跳与期限定时器怎样跟阶段保持一致？ |
| 4 | `heartbeat_beats.go` / `timer_handlers.go`（[heartbeat](../../scripts/testdata/party-room/heartbeat_beats.go.txt)、[timer](../../scripts/testdata/party-room/timer_handlers.go.txt)） | 冷加载后心跳怎样重新启动？期限回调怎样核对节点身份？ |
| 5 | `board_packer.go`（[board_packer.go.txt](../../scripts/testdata/party-room/board_packer.go.txt)） | 同步视图怎样只放出 `Board` 一个字段？ |
| 6 | `game/handler/party_room.go`（[handlers.go.txt](../../scripts/testdata/party-room/handlers.go.txt)） | 每个 handler 怎样先 `Advance(now)` 再裁决？领奖怎样与 Player 原子提交？ |
| 7 | `game/handler/party_room_sync.go`（[sync.go.txt](../../scripts/testdata/party-room/sync.go.txt)） | Manager、Group、会话三者的开关顺序 |
| 8 | [gameplay_test.go.txt](../../scripts/testdata/party-room/gameplay_test.go.txt)、[rules_test.go.txt](../../scripts/testdata/party-room/rules_test.go.txt) | 十二个场景怎样实际验证 |
| 9 | [persistence/recovery_test.go](../../scripts/testdata/party-room/persistence/recovery_test.go) | 局内 SIGKILL 后怎样冷加载、重启心跳、结算并删档 |

身份要分开理解：Player / PartyRoom 用框架完整 EntityID；匹配的票 ID 与 Match ID 是匹配服务的身份；session 的 Run ID 是一次“在房间里”的身份；Entity Sync 的会话 ID 在本例取玩家实体 ID；每个座位的输入序号与连接代（Epoch）是房间内的序号域；接入层票号（ticket）单调递增，用来区分新旧连接。

#### 3.3 用 Sender 调用

下面的文件放进保留的生成工程（例如 `internal/partyflow/flow.go`）可直接编译。`client` 是当前服务的 Nest 客户端；房间实体要先用 `EntityAccess.Create` 以 `PartyRoomIDForMatch(match.ID)` 建好（夹具在测试里直接建，没有演示 id-gen 与目录接线）：

```go
package partyflow

import (
	"context"
	"strconv"

	"example.com/party-room-practice/game/handler"
	syncsender "example.com/party-room-practice/game/handler/syncsender"
	party "example.com/party-room-practice/game/partyroom"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	svcmatch "github.com/tjbdwanghaibo/roost-core/service/match"
	"github.com/tjbdwanghaibo/roost-core/service/session"
)

// openMatchRoom 由成局方在 Commit 拿到 Match 后调用：名单、规则与时间都来自服务端。
// 同一 Match 重试返回原房间；每名成员一个 Run 并挂上房间，重连靠它找回房间。
func openMatchRoom(ctx context.Context, client nest.Client, sessions *session.Service, match svcmatch.Match, rules party.Rules) (int64, error) {
	roomID, err := handler.PartyRoomIDForMatch(match.ID)
	if err != nil {
		return 0, err
	}
	roster := make([]int64, len(match.Members))
	for i, member := range match.Members {
		roster[i] = member.ID
	}
	if _, err := syncsender.NewPartyRoomSender(client).Sync_PartyOpenMatch(ctx, roomID, match.ID, roster, rules); err != nil {
		return 0, err
	}
	resource := session.Resource{Kind: "party_room", ID: strconv.FormatInt(roomID, 10)}
	for _, id := range roster {
		// 请求 ID 在 session 里是全局身份：不同玩家不能共用同一个，所以带上玩家号。
		run, err := sessions.Enter(ctx, id, session.EnterRequest{Kind: "party-room", RequestID: match.ID + ":" + strconv.FormatInt(id, 10)})
		if err != nil {
			return 0, err
		}
		if _, err := sessions.Attach(ctx, id, run.ID, resource); err != nil {
			return 0, err
		}
	}
	return roomID, nil
}

// reconnect 是鉴权后的新连接：经 session 找回房间，Nest 记新连接代，提交后再加入同步组（从全量开始）。
func reconnect(ctx context.Context, client nest.Client, sessions *session.Service, hub *handler.PartyRoomSync, playerID int64, ticket uint64) (int64, uint64, error) {
	run, ok, err := sessions.Current(ctx, playerID)
	if err != nil || !ok {
		return 0, 0, err
	}
	attached, ok := run.Attached("party_room")
	if !ok {
		return 0, 0, party.ErrRule
	}
	roomID, err := strconv.ParseInt(attached.ID, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	epoch, err := syncsender.NewPartyRoomSender(client).Sync_PartyConnect(ctx, roomID, playerID, ticket, true)
	if err != nil {
		return 0, 0, err
	}
	return roomID, epoch, hub.Connect(roomID, playerID)
}

// grab 是一条玩家输入：超时重试时原样重发同一条（同一 Seq），不要分配新序号。
func grab(ctx context.Context, client nest.Client, roomID, playerID int64, epoch, seq uint64, round int) (party.Receipt, error) {
	return syncsender.NewPartyRoomSender(client).Sync_PartyInput(ctx, roomID, playerID, party.Input{Epoch: epoch, Seq: seq, Round: round, Kind: "grab"})
}
```

handler 的最后一个参数 `nowUnixMilli` 是约定名的业务时间参数：生成的分发包装在执行时从 `fctx` 填（这条消息冻结的业务时间），Sender 签名里没有它，调用方不传时间（[时间 §1A](guide/10-time.md#1a-业务时间一律取自-ctxd22-1)）。

#### 3.4 十二个场景各学什么

| 场景（测试名前缀 `TestPartyGame`） | 业务选择 | 学到的框架契约 |
| --- | --- | --- |
| `RulesValidateAndRoomCode` | 9 座、1 座、星星时刻超出回合、期限短于常规回合、输家奖励高于胜者都被拒；房间码往返 | 规则只在开房时校验一次，按值冻结 |
| `RulesVariableStepEquivalence` | 同一组输入按一步、100ms、50ms、不规则、7 秒步长推进到同一时刻，状态逐字节相同；第一次推进就越过硬期限时仍先结束第三回合判胜；平分进入加时直到期限按 timeout 结算，结算时刻是期限本身 | 心跳是可变步长（掉拍、卡顿后一拍算一次）；业务按事件时间顺序结算就与步长无关 |
| `MatchmakingOpensOneRoomPerMatch` | 同一请求重放不重复排队、第二张有效票被拒；2～8 人弹性队列里四人等待不满 20 秒时策略不出组、提交报 `ErrGroupNotReady`，业务时间推进 20 秒后四人成局、同一票集合并发两次 Commit 是同一局；同一 Match 并发开房三次是同一房间；换名单 / 换 Match 被拒且房间不变；每人一个 Run、重放 Enter 同一个 Run、第二个 Run 被拒；`Current` 带回房间 | service/match 与 service/session 的正式领域语义（内存存储）；开房 handler 以房间里记下的 Match 识别重放 |
| `CodeRoomFullAndLateJoinRejected` | 房主开房间码房间，7 人加入到 8 人满，第 9 人被拒且状态不变；重复加入无变化；开局后加入被拒 | 被拒的命令回滚：State、Board、心跳与期限都不变 |
| `HostLeaveTransfersHost` | lobby 里房主离开，房主转给下一个座位、座位让出；非房主开局被拒；局内房主离开同样转移，座位记为离开 | 房主是业务事实，存在房间状态里 |
| `HeartbeatDrivesRoundsAndGroupSync` | 没有输入时，心跳按业务时间放出星星、结束回合；每次 Flush 后四个客户端（正式 `frame.Receiver` 解码）与服务端 Board 逐字节相同，客户端文档里只有 `board`；业务时钟一次跳过两回合，一拍结算，结算后心跳与期限都停 | 心跳回调在实体锁与事务内；Board 的变化经 Nest 提交门进入下一次 Flush；Group 让全体成员订阅同一视图 |
| `FirstGrabWinsAndDuplicateInputDedup` | 四人并发抢同一颗星，只有一人得分、三人 late；重投三次回执不变、分数不变；旧序号、跳号、错类型、旧连接代都拒绝且不改状态 | 单实体 Nest 串行 + 业务序号去重 |
| `FoulEliminationAndUndoRollback` | 抢跑两次淘汰，之后输入被拒；输入写入后 handler 失败，State、Board、心跳、期限全部回滚，重试成功 | undo 覆盖 DAO、心跳启停与定时器增删 |
| `ReconnectWithinGraceReturnsToSameRoom` | 断线后其他成员看到离线；宽限内经 `session.Current` 找回同一房间，连接代加一；旧连接的断开晚到不踢新连接，旧连接代输入被拒；新连接从全量开始 | 连接代与票号由业务存在房间里；Entity Sync 会话重开后 `Group.Join` 重新提交订阅 |
| `GraceExpiryAndMidGameLeaveEndEarly` | 断线超过宽限由心跳判掉线，之后不能再连；房主与另一人先后离开，房主转移，局内只剩一人时提前结算 | 宽限到期也是按时间顺序处理的事件 |
| `SettlementRewardsOnceAndRoomDestroyed` | 中途离开者 `session.Leave` 后立即归还座位；结算前、领奖前归还被拒；领奖写入后失败回滚；四人各四次并发领奖每人只得一份（胜者 50、完赛 10、离开 0）；`session.Finish`（重放幂等）经 Releaser 归还，最后一个归还关闭房间；注销同步、卸载实体后房间不再应答、心跳环为空 | 多实体领奖：Player 金币与房间 Claimed 一起提交；Releaser 至少一次、handler 幂等 |
| `MatchDeadlineTimerForcesSettlement` | 平分进入加时（心跳照常推进），然后心跳驱动停摆；持久期限到期按 timeout 结算，没有胜者，完赛者拿参与奖，心跳登记与期限都清掉 | 持久定时器不依赖心跳；两条路径调用同一个 `Advance`，结果相同 |

上述数值是样例，不是公共框架规则。这些场景实际证明到哪一步见第 12 节。

### 4. 房间的两个驱动：心跳与持久期限

心跳（[时间 §4A](guide/10-time.md#4a-实体心跳周期推进)）是在线的周期推进：驱动按 10ms 一格、固定档位（50ms～60s 十档）把到期的登记交给 Nest，回调在实体自己的 worker 上、实体锁与事务内执行；每个登记至多一拍在途，上一拍没跑完就跳拍、不补发，`Beat.Elapsed` 给出距上次执行的业务时间。本例的回调只做一件事：`Advance(beat.Now)` 再 `SaveRoom`。判定只看时间戳，所以“星星是否已经出现”“回合是否已经结束”与心跳跑了几拍无关；玩家输入的 handler 同样先 `Advance(nowUnixMilli)`，输入与心跳谁先拿到锁结果都一样。

心跳不持久（[时间 §4A](guide/10-time.md#4a-实体心跳周期推进)）：进程重启、实体卸载后都要在 `onHeartbeatInit` 里按持久阶段重新启动；本例 `playing` 才启动。心跳的持久级别缺省 `async`：常规推进丢最后几拍可由下一次推进按时间重算；奖励走 strict 的领奖事务。

硬期限是持久实体定时器（[时间 §4](guide/10-time.md#4-按实体的持久定时器)）：开局登记在 `State.Deadline`，节点 ID 记进 `WakeTimer`，结算时删除。它不依赖心跳，实体不在内存时由驱动在加载后补触发一次；回调核对节点 ID 与期限后同样 `Advance(fire.Now)`。

`SaveRoom` 是房间唯一的写入口：写回 `State` 与 `Board`，并让心跳与期限跟阶段一致（`playing` 时心跳在跳、期限在登记，其余都停）。三个调用方（handler、心跳、定时器）不各自管理驱动，回滚时 DAO、心跳启停与定时器增删一起作废（[KNOWN-LIMITS 实体心跳](../maintenance/KNOWN-LIMITS.md#实体心跳d20-第-1-期) 里 `Reg.Close` 不随事务回滚，本例只用 `Start` / `Stop`）。

### 5. 房间内同步：Group

`policy.Group` 是“成员 × 实体全互见”（[ENTITY_SYNC](ENTITY_SYNC.md)）：本例每个房间一个 Group，房间实体是唯一的 subject，房间里的玩家是成员，全部订阅同一个视图 `RoomSyncProfile`。所以同一次 Flush 里，每名成员收到的记分板是同一份。

- **内容走提交门**：Manager 作为 Nest 的 `EntitySync` 观察者（`nest.NestOptionWithEntitySync`），Board 的修改在事务确认后才进入下一次 Flush；回滚的修改不会推出去（`FoulEliminationAndUndoRollback` 的 `refuse` 断言 Board 不变）。
- **成员关系不进 Nest**：`OpenRoom` / `Connect` / `Leave` / `CloseRoom` 在对应的业务事务**提交之后**调用。连接事件提交后 `OpenSession` + `Join`；断线只关会话、保留成员关系，宽限内重连时再 `Join`，新连接从全量开始；离开房间退出组；关房先 `Group.Close` 再 `Manager.Unregister`，然后销毁实体。
- **视图白名单**：packer 只放出 `Board`；`State`（种子、星星出现时刻、票号、回执）不标 sync，任何视图都拿不到。夹具的客户端核对收到的文档只有 `_id` 与 `board` 两个键。
- **慢消费者、帧上限**：沿用 Manager 与接入层的契约——一帧被活着的连接拒收时关会话（[guide/04 §2](guide/04-sync.md#玩家只看自己自身视图)）；一个会话持有的对象受 `Limits.MaxObjects`（缺省 100）约束（[guide/04 视野上限](guide/04-sync.md#谁看见谁视野viewd19)）。派对房间每个会话只有一个对象，不会碰到。
- **推送频率**：`Board` 带“本回合剩余毫秒”，心跳每拍都会改它，所以 `playing` 时每 100ms 每房间一个增量。要省带宽就去掉剩余时间、让客户端按回合结束时刻自己倒计时，Board 只在事件发生时变化。

### 6. 匹配、会话与重连

成局：`service/match` 的票、成局与幂等语义见 [MOBA §2](MOBA-INTEGRATION.md#2-模块分工对局里的状态归谁) 与 [guide/09 §4](guide/09-services.md#4-关键业务边界)。本例 `PartyRoomIDForMatch` 用散列把 Match ID 映射到房间 ID，开房 handler 用房间里记下的 Match 识别重放、拒绝碰撞；正式工程用目录实体或 directory 登记 Match → 房间（MOBA 的 BattleDirectory 是写法参考）。

房间码：本例的码就是房间唯一号的 36 进制编码，可被枚举；正式工程用 directory 一类的短码登记（带过期、可回收），不要把实体 ID 直接给玩家（[guide/09 §4 名字与 directory](guide/09-services.md#4-关键业务边界)）。

会话：`service/session` 的 Run 是“玩家正在这个房间里”的持久事实（[guide/09 §1A](guide/09-services.md#1a-session-运行会话)）。Gate 连接、账号 token、Entity Sync 会话、Run 是四个不同身份：断线不代表 Run 结束，Run 结束也不关闭玩家连接。`Releaser` 至少一次调用，房间侧的 `Release` 必须幂等，并且在奖励写进 Player 之前拒绝归还，所以房间不会在发奖之前关闭。

重连：鉴权后的新连接用更大的票号调 `Connect`，房间里这一座位的连接代加一；客户端输入必须带当前连接代，旧连接（旧代、旧票号的断开事件）一律拒绝。重投最后一条输入（同一序号、同一内容）总能拿回原回执，包括重连之后和结算之后。

### 7. 结算、归还与销毁

结算由服务端发起：局面变成 `settled` 后，对每个座位调用 `MultiSync_PartyClaim(player, room)`。它同时锁 Player 与房间，按终局推导奖励（胜者 50、其他完赛者含犯规淘汰 10、中途离开与掉线 0），Player 金币与座位 `Claimed` 一起提交，重复调用返回同一数额。这是同一原服内的多实体原子提交；Player 在别的 sid 时走 Saga / effect 原服协作（[Saga](guide/06-saga.md)），本地 MultiSync 不升级成分布式事务。

归还：中途离开的人 `session.Leave` 后立即归还座位；完赛者领奖后 `session.Finish` 归还。最后一个座位归还时房间变成 `closed`，心跳与期限已在结算时停下。之后业务调用 `PartyRoomSync.CloseRoom`，再销毁实体：真实进程用 `EntityAccess.Destroy(ctx, room, reason, true)` 删档（DataEngine 删除准入，删档后同一 ID 不可再用，[guide/03 §4](guide/03-dataengine.md#4-冷加载与-schema)），默认夹具没有 DataEngine，只做内存卸载；真实依赖用例走删档。

### 8. 框架没有什么

下面这些是派对房间常见需求、而 core 当前**没有**的部分；业务要自己补，或者说明这类玩法不适合这条路：

| 没有什么 | 现状与出处 | 业务怎么办 |
| --- | --- | --- |
| 高于 20Hz 的服务端权威模拟 | 心跳档位固定 50ms～60s 十档，最短 50ms（`gameplay/heartbeat/tier.go` 的 `Periods`；[时间 §4A](guide/10-time.md#4a-实体心跳周期推进)） | 抢答、回合制、计时类派对游戏够用；动作 / 物理对抗类用 Lockstep（[MOBA](MOBA-INTEGRATION.md)）或物理房间（[PHYSICS-ROOM-SERVICE](PHYSICS-ROOM-SERVICE.md)） |
| 固定步长、追帧 | 心跳是可变步长：上一拍没跑完就跳拍、不补发、不追帧，`Elapsed` 夹在 `[0, max(1s, 档位)]`（[时间 §4A](guide/10-time.md#4a-实体心跳周期推进)、[KNOWN-LIMITS 实体心跳](../maintenance/KNOWN-LIMITS.md#实体心跳d20-第-1-期)） | 规则按时间戳与事件顺序写（本例 `Advance`），不要按“第几拍”累加 |
| UDP / KCP 玩家接入 | 玩家接入层（嵌入 TCP、独立 Gate）只提供 TCP；`-features nettransport-kcp` 等只生成装配函数，监听、握手鉴权与 Session 绑定由宿主自己写（[MOBA §2](MOBA-INTEGRATION.md#2-模块分工对局里的状态归谁)、[GATEWAY](GATEWAY.md)） | 派对房间的输入频率低，TCP 足够；要 UDP 自己写接入 |
| 房间跨机接管 / HA | 房间是普通本地实体，属于开房的进程；进程崩溃后只能由同一份 WAL / Mongo 冷加载恢复（第 13 节），没有自动迁到别的机器继续跑。Player 静态绑定 sid、不做动态迁移（[guide/09 §4](guide/09-services.md#4-关键业务边界)） | 短局可接受“崩溃 → 冷加载 → 硬期限结算”；需要跨机接管要另做 Remote 托管或宿主交接（MOBA 的宿主交接是写法参考） |
| 房间内同步的生成器 / 装配 | `policy.Group` 只有包内测试与本夹具在用，生成器与 `wiring` 没有房间同步的入口；`access.player.self_sync` 打开后 Nest 只接受一个 `EntitySync` 观察者（`NestOptionWithEntitySync`），`EntitySyncSetup.Attach` 已被 `wiring/selfsync` 占用 | 本例自建 Manager；与 self_sync 同时用时怎样把 Group 挂到同一个 Manager 尚未确认（[KNOWN-LIMITS 游戏接入验收](../maintenance/KNOWN-LIMITS.md#游戏接入验收)） |
| 跨服务广播的可靠送达 | SyncBus 定位为可丢广播：普通 NATS 至多一次，JetStream 有限重投（[guide/04 §5](guide/04-sync.md#syncbus-的定位与丢失兜底)） | 房间内同步不走 SyncBus；跨服务的房间事件（大厅列表、好友在玩）要业务对账 |
| 已开局房间的中途补位 | 弹性人数成局**已提供**（`Queue.MinSize` / `FillAfterSeconds`，“够 M 人、最早一张票等满 T 就开”，[guide/09 §4](guide/09-services.md#4-关键业务边界)）；已开局房间声明空位、后来的票优先补进去的补位没有（方案与待选项见 [FEATURE-2026-10-31 第 3 节](../maintenance/FEATURE-2026-10-31-match-flexible-grouping.md#3-第二阶段中途补位backfill待维护者选择)） | 房间宿主把空座当成一张“房间票”排进单独的补位队列，匹配器取一张房间票 + 若干玩家票弹性成局，进房 handler 拒绝超员（方案 B）；或等框架补位 |
| 房间码、大厅列表 | 没有房间码服务与房间列表 | directory 做短码；大厅列表自己维护（可丢、可重建） |
| 掉线托管、AI 补位、观战 | 没有 | 业务规则；观战者可以只加入 Group 的成员、不占座位 |

### 9. 接入时容易踩的问题

- **测试名前缀撞上生成的测试**：`roost add timer --entity PartyRoom` 会生成 `TestPartyRoomTimerTickHandlerMatchesTheDriver`；场景用例若也以 `TestPartyRoom` 开头，`-run '^TestPartyRoom'` 会多跑这一条，通过数断言对不上。本例场景用 `TestPartyGame` 前缀。
- **锁目标没被识别**：锁接口名要含 `Entity`（本例 `IPartyRoomEntity` / `IPartyPlayerEntity`），否则 `roost generate` 报 `found no entity parameter`。
- **在测试 goroutine 里直接读 DAO**：心跳在并发写房间 DAO，测试直接读会被 `-race` 报出；经一个只读 Nest handler 读（夹具的 `partySnapshot`）。
- **session 的请求 ID 是全局身份**：两个玩家用同一个 `RequestID` 会冲突，本例用 `Match ID + 玩家号`。
- **业务时间**：handler 用约定名参数 `nowUnixMilli`，测试用 `clock.SetSource` 拨时钟并在 `t.Cleanup` 里恢复；不要让调用方传 `now`。
- **心跳里不做 I/O、不发同步 Nest 请求**：回调与 handler 一样在实体锁内；同步、session、匹配都在事务外调用。
- **心跳与 Board 推送**：Board 带剩余毫秒时每拍都会产生增量（第 5 节）。

### 10. 练习

加“抢答题”：每回合下发一道题（题目在 Board，答案只在 State），第一个答对的人得分。先写清拥有者、幂等身份、事务参与者与恢复语义；答题判定仍按时间戳写进 `Advance` / `Apply`，不要另起一个计时 goroutine。

## 第二部分：验收

### 11. 跑什么

```bash
bash scripts/test-party-room-generated.sh
```

脚本按 3.1 的步骤生成工程，`replace` 指回当前检出（在 `generate` 之前），必须实际通过 12 条顶层 race 场景（3.4：2 条规则 + 10 条 Nest / 心跳 / 同步）且无 skip，随后执行消费者 build / vet、`roost id check` 与 `roost generate --check`，再把真实依赖用例复制进工程只编译（`go vet`）；成功删除临时工程，失败或 `ROOST_PARTY_ROOM_KEEP=1` 时保留。缺省模式由 `scripts/pretag.sh` 每次发版跑（根包 `TestGeneratedScriptsAreInPretag` 防止漏掉）。2026-10-31 本机 macOS 实测：缺省模式约 18 秒（12 个场景合计约 2 秒，主要是心跳等待）。

### 12. 业务纵切证明什么

- **证明**：公共 CLI 生成的 Player / PartyRoom DAO、实体心跳（真实驱动循环、批量投递）、持久实体定时器、`nopersist,sync` 字段与业务 packer、多实体 Sender 与 Nest 在 12 个组合下的事务、undo 回滚（含心跳启停与定时器）、锁顺序、业务幂等与时间判定；`entitysync.Manager` + `policy.Group` 经 Nest 提交门把同一份记分板推给全体成员（正式 `frame.Receiver` 解码、视图白名单）；`service/match` 与 `service/session` 的正式领域实现（内存存储）在成局、进房、重连、归还里的语义。
- **不证明**：提交器是替身，不含文件 WAL fsync 与 Mongo；房间销毁只做内存卸载；match / session 用内存存储，不含 Redis 与 RPC；Entity Sync 的传输是内存捕获，没有经嵌入 TCP / 独立 Gate、慢消费者断开、真实客户端；单进程；心跳在 `-race` 下的少量房间运行，没有房间数量、心跳负载与推送带宽的容量数据；夹具里的数值是样例。

### 13. 真实 WAL / Mongo 崩溃恢复

先按[隔离环境说明](../../wiring/scripts/integration/README.md)准备独立 Mongo 副本集并设置地址变量（不要指向生产库或共享业务库），再运行：

```bash
ROOST_PARTY_ROOM_REAL=1 bash scripts/test-party-room-generated.sh
```

它在第 11 节之后追加 `TestPartyGameRealCrashRecovery`（已登记根包 `integration_coverage_test.go`），用本次唯一的 DAO 库与 WAL 目录：

1. 子进程经 strict 文件 WAL 提交三名玩家建档、成局开房、三人连接，把业务时间拨到星星出现、seat 0 抢到；后台投影关闭，断言 WAL 有已确认未投影的记录、房间处于 `playing`、心跳在业务视角启动、期限已登记，父进程 SIGKILL 自己启动的子进程。
2. 父进程用同一 WAL 与 Mongo 冷加载：房间 `State` 与期限节点 ID 和崩溃前逐字节一致；`onHeartbeatInit` 按持久阶段重新启动心跳（驱动环里恰好 1 个登记）；重投崩溃前的输入，回执不变。
3. 启动心跳驱动循环，业务时钟越过三个回合：重新启动的心跳一拍内按时间顺序结束回合，seat 0 唯一领先获胜，心跳与期限停下；三人各领奖两次（50 / 10 / 10）、归还座位，房间关闭；`EntityAccess.Destroy(..., true)` 经 DataEngine 删档，心跳环为空。
4. 正常关闭后起第三个进程冷加载：三人金币不变，房间不再加载，对它领奖失败、金币仍不变。成功后删除本次库；失败保留生成工程、WAL 与库名供诊断。

2026-10-31 本机实测（私有单节点 Mongo 副本集）：通过，崩溃时 12 条未投影记录，脚本全程约 19 秒。对照：把 `onHeartbeatInit` 里按阶段启动心跳的一行去掉后跑一次，用例在冷加载核对处失败（`cold load lost the room: beat=false`），所以“冷加载后心跳重新启动”确实由业务的 `onHeartbeatInit` 完成。

- **证明**：strict 已确认的输入与房间状态在 SIGKILL 后可从未投影 WAL 恢复；心跳不持久，冷加载后按持久阶段重新启动、离线期间错过的回合在第一拍里一次结算；跨进程重试不重复发奖；删档后房间不再加载。
- **不证明**：提交在途 / 结果未知时的崩溃、磁盘断电、WAL 损坏；期限定时器的冷加载补触发（由塔防等纵切与 entitytimer 包测试覆盖）；Entity Sync 在恢复后的重新登记；多进程 / 跨机、Redis / NATS；三个逻辑玩家不是三台客户端，容量与长稳未测。

### 14. 结论怎样给

派对玩法、计分、房间码与大厅缺少默认实现，不自动构成框架缺陷；合法接口调用下出现丢输入、重复发奖、记分板泄露 State 字段、回滚后仍推出去、心跳或定时器在结算后残留、生成物编译失败，则应在公共实现中修复。本夹具在当前 main 上没有触发框架或生成器缺陷；第 8 节的“房间同步没有生成器入口、与 self_sync 共用 Manager 尚未确认”是能力缺口，登记在 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#游戏接入验收)。100ms 档位与 30 秒期限是样例，不构成延迟或吞吐承诺。

尚未实施或验收：正式客户端与公网鉴权、接入层到 `Connect` 的票号签发、房间码短码服务、大厅列表、中途补位、掉线托管与 AI 补位、观战、跨 sid 发奖、房间跨机接管、房间数量与推送带宽的容量（均不因此算框架缺陷）。本部分是本游戏验收范围的完整说明；[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#游戏接入验收) 的游戏接入验收表只列一行摘要。
