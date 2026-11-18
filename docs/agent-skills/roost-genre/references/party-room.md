# 实时派对房间

手册：`docs/framework/PARTY-ROOM-INTEGRATION.md`（下称 PARTY §N）。命令对应当前版本的 `scripts/test-party-room-generated.sh`，步骤与顺序照抄脚本；目标 roost-core 版本更早时读那个 tag 里的本文件与脚本（派对房间各版本的夹具与命令不同，不要拿本篇命令套旧版本）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 2～8 人短局、服务器权威房间：抢答、计时、回合制、轻反应类聚会游戏（夹具是“抢星”：随机时刻放星、先抢得分、抢跑犯规、三回合 + 加时 + 硬期限）；弹性人数匹配成局（2～8 人、等 20 秒）或房间码开房、房主转移、断线宽限重连、局内同一份记分板推给全体成员（与玩家自身视图同一条连接）、开局后第 1 回合内补位、结算发奖与座位归还 | 依赖高于 20 次 / 秒心跳、逐帧确定性或客户端预测的动作 / 物理对抗（心跳最短 50ms，不是 Sync 发送上限）：逐帧确定性走 [moba-lockstep](moba-lockstep.md)，物理闯关看 `PHYSICS-ROOM-SERVICE.md`；要 UDP 接入、房间跨机接管、存储保证不超员的补位、大厅列表 / 短码服务的，框架没有 |

与 [room-coop-pve](room-coop-pve.md) 的区别：那三篇每一步都由持久定时器推进；本篇在线推进靠**实体心跳**（100ms 档），持久定时器只做开局硬期限兜底（PARTY §1、§4）。

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后（已 `cd "$DIR"`）。模块与分工：

| 业务 | 归谁 |
| --- | --- |
| 建档、永久金币 | **enter**（领奖查 `Enter().Built()`）、**wallet**（`PlayerDao.Wallet`，经自身视图推给本人，领奖 `Wallet().Add`）；enter 的安装同时打开玩家 TCP 与 `self_sync`。Player 上只留玩法自己的 `Matches` |
| 排队、成局 | `service/match`：弹性人数队列 `Queue{GroupSize: 8, MinSize: 2, FillAfterSeconds: 20}`，凑满 8 人立即开，不满时最早一张票等满 20 秒后 2～7 人也开；预判“候选够不够一局”用 `queue.MinGroupSize()` |
| 玩家在哪个房间 | `service/session` Run：`Enter`（RequestID 全局唯一）+ `Attach` 房间；重连 `Current`；`Finish` / `Leave` 经业务 `Releaser` 归还座位 |
| 房间权威状态 | PartyRoom 实体（World 锁档，不用 room 模块：要心跳 + 组同步记分板 + 补位）：DAO `State`（只落库）+ `WakeTimer`；公开记分板 `Board`（`nopersist,sync`），打包器只开放这一个视图 |
| 回合推进、出星、断线宽限 | `roost add heartbeat --entity PartyRoom`（100ms 档），回调在实体锁与事务内 `Advance(beat.Now)` |
| 开局硬期限 | `roost add timer --entity PartyRoom` |
| 房间内同步 | `roost.yaml` 的 `access.player.sync_groups: [party_room]`；`project sync` 生成一次业务文件 `sync_group.go`；handler 里 `SyncGroup.Join / Leave / Disband`，在事务里调用时提交后才生效。与自身视图同一条连接、同一个推送编号，客户端按 `Namespace` + `Profile.Key` 分流 |
| 补位 | 没有框架 API，业务层写法：房间状态 `Backfill` + handler 在实体锁内裁决；`game/modules/partymatch` 在事务外把空座挂成补位队列里的房间票，成局交房间按座位逐个接受，多出的人被拒并重排；截止 = 第 1 回合结束（PARTY §6A） |
| 领奖 | Player + 房间多实体 `MultiSync_PartyClaim`；“领过没有”是座位上的 `Claimed` |

```bash
F="$CORE/scripts/testdata/party-room"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" wallet enter
"$ROOST" add entity PartyRoom
"$ROOST" add dao PartyRoom --entity PartyRoom
"$ROOST" add heartbeat --entity PartyRoom
"$ROOST" add timer --entity PartyRoom
dao_add Player db/def/player.go "Matches int64 matches"
dao_add PartyRoom db/def/party_room.go "State string state" "WakeTimer int64 wake_timer" "Board string board nopersist,sync"
# World 锁档 + 房间作为 Entity Sync subject，用业务打包器只同步 Board
perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryWorld sync=true syncNamespace=party_room subjectPacker=NewPartyRoomBoardPacker/' \
  game/entities/party_room/entity.go
grep -q 'subjectPacker=NewPartyRoomBoardPacker' game/entities/party_room/entity.go
mkdir -p game/modules/partyroom game/modules/partymatch
for n in rules state backfill rules_test; do cp "$F/$n.go.txt" "game/modules/partyroom/$n.go"; done
# 心跳登记、定时器回调是 roost add 只在缺失时创建的业务文件，用夹具覆盖
for n in room heartbeat_beats timer_handlers board_packer; do cp "$F/$n.go.txt" "game/entities/party_room/$n.go"; done
cp "$F/handlers.go.txt"      game/handler/party_room.go
cp "$F/gameplay_test.go.txt" game/handler/party_room_test.go
# 补位宿主与匹配循环必须在 project sync 之前放进工程：project sync 跑 go mod tidy，handler 测试 import game/modules/partymatch
cp "$F/backfill_match.go.txt" game/modules/partymatch/backfill.go
fix_mod party-room-practice game/modules/partyroom/*.go game/modules/partymatch/*.go game/entities/party_room/*.go game/handler/party_room*.go
gofmt -w db/def/*.go game/modules/partyroom/*.go game/modules/partymatch/*.go game/entities/party_room/*.go game/handler/party_room*.go
# self_sync 已由 enter 打开；实体标记与打包器就位之后只追加 sync_groups
perl -0pi -e 's/^(        self_sync: true\n)/$1        sync_groups:\n            - party_room\n/m or die "access.player.self_sync missing in roost.yaml\n"' roost.yaml
"$ROOST" project sync
# project sync 创建一次的 sync_group.go 是业务文件：换成本例视图 RoomSyncProfile 与座位上限
cp "$F/sync_group.go.txt" game/entities/party_room/sync_group.go
fix_mod party-room-practice game/entities/party_room/sync_group.go
gofmt -w game/entities/party_room/sync_group.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" party-room
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestPartyGame' ./game/modules/partyroom ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" wallet enter
```

然后按 SKILL.md §3.4 收尾（`<前缀>` = `PartyGame`）。顺序上的要点：

- 场景前缀必须是 `TestPartyGame`：`add timer --entity PartyRoom` 会生成 `TestPartyRoomTimerTickHandlerMatchesTheDriver`，用 `^TestPartyRoom` 会多跑一条。
- `game/modules/partymatch` 与全部夹具在 `project sync` 之前就位并 `fix_mod`，否则 `project sync` 报 `framework resolution failed ... go mod tidy: exit status 1`。实体标记（`sync=true` + 打包器）也要先改好，`project sync` 才过 `sync_groups` 的就绪检查。
- `roost.yaml` 的 perl 改写依赖 enter 写出的 `        self_sync: true`；改不到时 `die`，不要静默继续。
- `sync_group.go` 的缺省版本视图键是 `party_room`，本例打包器只认 `RoomSyncProfile`，所以必须用夹具覆盖。

## 3. 期望场景数

`--- PASS: TestPartyGame` 恰好 **19** 条（`game/modules/partyroom` 3 条规则 + `game/handler` 15 条 Nest / 心跳 / 同步 / 补位 + `TestPartyGameRobotBusiness`）、0 SKIP；`modules.sh verify` 打印 `enter 29/29`、`wallet 17/17`。

最小可讲的玩法：弹性匹配成局（2～8 人）→ 开房 + 每人一个 Run → 心跳推进三回合抢星（记分板与自身视图同一条连接）→ 第 1 回合有人离开时补位 → 结算 → 每人领奖一次 → 归还、解散同步组、关房。客户端读自身视图里的金币：`Wallet` 是嵌套状态，增量只带嵌套补丁，按生成的 `db.ApplySyncPlayerDao(patch, 基线)` 合并后读 `Wallet.Value.Coins`。

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 一房一锁串行裁决、心跳与持久期限双驱动、回滚覆盖 DAO + 心跳启停 + 定时器、多实体领奖原子幂等、组同步经提交门推同一份视图、match 弹性成局与 session | PARTY §12 |
| 心跳 ≤ 20 次 / 秒 | 档位固定 50ms～60s 十档；不代表 on_change Sync 有 20Hz 硬上限 | PARTY §8；guide/10-time.md §4A |
| 可变步长 | 上一拍没跑完就跳拍、不补发，`Elapsed` 夹在 `[0, max(1s, 档位)]`；规则按时间戳与事件顺序写，不按“第几拍”累加 | PARTY §8；KNOWN-LIMITS“实体心跳” |
| 心跳不持久 | 重启 / 卸载后要在 `onHeartbeatInit` 按持久阶段重新启动 | PARTY §4、§13 |
| 接入 | 玩家接入提供 TCP 与 WebSocket；KCP / UDP 只生成装配函数 | PARTY §8；docs/framework/GATEWAY.md |
| 房间无跨机接管 | 房间是开房进程的本地实体，崩溃后只能同一份 WAL / Mongo 冷加载恢复 | PARTY §8 |
| SyncBus 可丢 | 房间内同步不走 SyncBus；跨服务房间事件（大厅、好友在玩）要业务对账 | PARTY §8；guide/04-sync.md §5 |
| 房间同步 | 要求 self_sync；成员关系不持久，重启后靠玩家重连再 `Join`；没有房间数 / 成员数容量数据；掉线出局的人宽限期满不自动退组 | PARTY §5、§8；ENTITY_SYNC.md“Groups” |
| 弹性成局 | 等待按 Unix 秒取整；存储按自己的时钟判，时钟不一致时可能被 `ErrGroupNotReady` 拒、下一轮再试；`FillAfterSeconds` 不得 ≥ `match.ticket_ttl` | guide/09-services.md §4；KNOWN-LIMITS“匹配的弹性人数成局与补位” |
| 补位 | 存储原生补位未提供。业务层补位：不超员靠进房 handler 按座位拒绝（不是存储保证，被拒的人要多排一次队）；不跨分区；一名玩家同时排两个队列会被两边各成一局，业务要保证一人只排一个队列；截止没有回调，要周期性 `SyncRoom` 对账撤票；本例只补主动离开的座位；只在内存存储、单进程匹配循环上验证 | PARTY §6A、§8 |
| 推送带宽 | Board 带剩余毫秒时 `playing` 期间每 100ms 每房间一个增量；要省就让客户端按回合结束时刻自己倒计时 | PARTY §5 |
| 缺省模式不证明 | 无 WAL / Mongo；组同步经替身接入层；match / session 只测内存存储。真实依赖档 `ROOST_PARTY_ROOM_REAL=1`（隔离 Mongo）：局内 SIGKILL → 冷加载、心跳按持久阶段重启并结算、发奖；不验证恢复后的组同步重新登记与真实网络送达 | PARTY §12、§13 |
| 业务要自己补 | 玩法规则、房间码短码服务（夹具码可枚举）、大厅列表、Match → 房间目录、接入层票号签发、掉线托管 / AI 补位、观战、跨服发奖、补位宿主的周期性对账 | PARTY §6、§6A、§7、§14 |

**常见坑**（PARTY §9）：

- session 的 `RequestID` 是全局身份：多名玩家共用同一个会冲突，夹具用 `Match ID + ":" + 玩家号`。
- 锁接口名不含 `Entity`：`generate` 报 `found no entity parameter`。
- 测试 goroutine 直接读 DAO：心跳在并发写，`-race` 报出；经只读 Nest handler 读。
- 心跳回调里做 I/O 或同步 Nest 请求：同步、session、匹配、补位排队 / 撤票都在事务外调。
- 三个写入方（handler、心跳、定时器）各自管驱动：只经 `SaveRoom` 一个入口；只用 `Start` / `Stop`，`Reg.Close` 不随事务回滚。
- 自建 `entitysync.Manager` 交给带 self_sync 的 Nest：报 `ErrEntitySyncObserverConflict`；房间同步用 `sync_groups`。
- 弹性队列的匹配器预判写死 `GroupSize`：不满员的局永远凑不出来，用 `queue.MinGroupSize()`；`FirstComeGrouping` 用于弹性队列必须给 `NowUnix`。
