# room 补 `Room().Authorize`（2026-11-17，R9 §9.5）

提交：`1b38c2b8`（实现、场景、计数、文档与本目录）。

不是缺陷修复，是按维护者 Q1 B 给 room 模块补一个能力：同一 Room 上玩法命令的身份围栏“代际 + 连接 + 在线”一处实现，turn 与房间类玩法共用。

## 入口

`game/components/room/room.go`（与 `game/view/component_room.go` 的 `view.Room`）：

```go
func (component *Component) Authorize(playerID int64, epoch uint64, connection string) (*db.RoomSeat, error)
```

- 先 `Member(playerID, epoch)`：房间未开 `room_unopened`、已关 `room_closed`、不在座 `room_not_member`、代际为 0 或不是当前代际 `room_stale_epoch`。
- 再要求 `connection` 格式合法、是席位认下的那条、现在在线（`Connected`），否则 `room_stale_epoch`。
- 只读。签名与 `Member` 同一风格（返回席位指针），按 §9.5 的建议，没有调整；没有新增错误码。

## 改法

| 位置 | 改动 |
| --- | --- |
| room 组件 / `view.Room` | 新增 `Authorize` |
| turn 组件 | 删掉私有 `authorize`，`Start` / `End` / `CheckEnd` / `View` 改调 `owner.Room().Authorize`；去掉不再用的 room 组件包 import。行为不变（原来也是 Member + 连接合法 + 在线 + 连接相等，全部 `room_stale_epoch`） |
| 塔防夹具 `towerMember` | 分两支：新命令经 `Room().Authorize`；已有回执的重放（`Seq` 落在本席已有回执之内）与 `TowerReceipt` 查回执仍是塔防自己那一支（`Member` + 连接核对，不要求在线：重启后全员断线时原连接、原代际的重试仍得原回执，REAL 档断言这一条）。命令 handler 因此先解码状态再过围栏 |

塔防语义变化只有一处：断线席位的新命令原来由 `Apply` 里的 `!connected` 拒成规则错误，现在先被 room 拒成 `room_stale_epoch`；场景 `ReadyDisconnectAndOldConnection` 断言的是“被拒且状态、定时器不变”，照过。

## 场景数

- room：33 → 34，新增 `TestRoomAuthorizeChecksGenerationConnectionAndPresence`（`game/handler`，21 → 22）。覆盖房间未开、当前成员放行（返回席位代际与座位号）、不在座、零代际、别人的代际、别人的连接、空连接（被拒后 DAO 字节不变）、换连接之后旧连接带新代际 / 新连接带旧代际都拒、断线拒（`Member` 仍认）、从持久字节重新装载后原连接原代际拒、换连接重连后放行、关房后 `room_closed`。
- turn：25 不变（围栏行为不变，已有 `TestTurnOldConnectionCannotActOrReplay`、`TestTurnReloadKeepsSequenceAndFiresOverdueOnce`、`TestTurnEndpointRejectsMissingIdentityAndOldConnection` 经它走）。
- 塔防：11 条 + REAL 1 条不变。

改过的计数：`modules/room/module.yaml` `verify.pass`；`modules/room/README.md`（34 条、handler 22 条）；`modules/README.md` 总表；`codegen/internal/roost/module_entity_test.go`、`module_timer_test.go`（两处）、`module_matchroom_test.go`、`module_matchmaking_test.go`（两处）；`scripts/test-modules-generated.sh`（`all_counts` 与三处 `expect_verify`）；`docs/framework/TOWER-DEFENSE-INTEGRATION.md`（两处）；`docs/agent-skills/roost-genre/references/room-coop-pve.md`（三处；第 131 行是 11-17 那次运行的历史记录，未改）；`docs/maintenance/PLAN-2026-11-17-r9-genre-modules.md` §9.2 一处。

## 验证与证据

| 内容 | 结果 | 文件 |
| --- | --- | --- |
| 新工程 `roost module add turn`（连带 room），`roost module verify room` / `turn` / 两个一起 | room 34/34、turn 25/25、0 skip；build / vet / id check / generate --check 通过 | [verify-room-turn.txt](verify-room-turn.txt) |
| 变异 M1～M3（room 的判断行去掉在线 / 去掉连接 / 只剩 Member） | 三次都红：room 新场景 + turn 的 `ReloadKeepsSequence…` / `EndpointRejectsMissingIdentity…`；还原后绿 | [mutations.txt](mutations.txt) |
| 变异 T1（塔防重放也要求在线） | 缺省档抓不到；REAL 档红（`retry changed purchase or phase room_stale_epoch`） | [mutations.txt](mutations.txt) |
| 变异 T2（塔防新命令不经 Authorize） | 抓不到：`Apply` 自己的在线检查是第二道，旧代际 / 旧连接由另一支拒；如实记录 | [mutations.txt](mutations.txt) |
| `bash scripts/test-tower-defense-generated.sh` 缺省档与 `ROOST_TOWER_DEFENSE_REAL=1` | 都 exit 0：11 PASS、REAL 1 PASS、room 34 / wallet 17 / enter 29 | [tower-defense.txt](tower-defense.txt) |
| 改了计数的模块门禁单测（`module_{entity,timer,matchroom,matchmaking}_test.go` 里的 10 个 `TestModule…`） | ok（126 s） | [gate-unit-tests.txt](gate-unit-tests.txt) |
| 塔防工程再装 turn：定时器类型号 | 不冲突，`TurnDeadline = 2`（见下） | [timer-type-tower-plus-turn.txt](timer-type-tower-plus-turn.txt) |

## 定时器类型号

塔防的 `TimerTowerPhase int32 = 1` 是 `game/entities/room/timer_handlers.go` 里的手写常量，不在 `roost.yaml timers.room.types` 里。安装器已有分号机制（`codegen/internal/roost/module_timer.go` 的 `planModuleTimer` / `scanModuleTimerConstants`）：分号前用 go/types 求实体包里全部 int32 常量与 `Register` 的类型实参，再加上 `timers.<实体>.types` 与已安装模块记录，取最小空号；求不出值时拒绝分号。实测在塔防工程上 `roost module plan turn` 给 `TurnDeadline: 2`、`create=false`（复用塔防的 timer 组件），`module add turn` 后 `timers.room.types.TurnDeadline: 2`、`registerTurnTimers(h)` 追加进塔防的登记函数，room / turn / wallet / enter 的 module verify 全过。结论：不冲突，无需交批 1A。

顺带发现（未修，交批 1A）：同一工程、同一进程里先跑塔防场景再跑 turn 的定时器场景，turn 有 6 条失败；分开跑都过。原因是塔防夹具调了全局的 `room.TimerDriver.SetClock(塔防测试时钟)`，`SetClock(nil)` 是空操作、无法撤销，之后 turn 的定时器 Host 用 `driver.Now()` 读到的是塔防那只时钟。属于测试夹具的全局状态串扰，与类型号无关；`module verify` 与塔防脚本按 `-run` 分开跑，不受影响。批 1A 若把房间类玩法与 turn 的场景放在同一个包、同一次 `go test` 里，要先处理（例如夹具改用 `clock.SetSource` 并不设驱动时钟，或给 `SetClock` 一个可撤销的写法）。
