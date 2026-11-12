# turn：房间里的顺序回合——固定名单轮流行动、命令序号与重放、持久的回合超时

turn 挂在 room 模块的 Room 实体上：一场对局开局时把在座的人按座位排成固定名单，之后依次轮到每个人；轮到的人主动结束回合，或者到期由持久定时器替他跳过。它只管“现在轮到谁、这条命令算不算数”，不定义任何玩法动作。

## 能力

- **客户端协议**（`protocol/def/turn.go`，三条应答都带 `Info`，类型是 `TurnInfo{Status, Round, ActorID, Deadline, MaxTurns, TurnMillis, Timeouts, Sequence, Players}`）
  - `TurnStart`：请求 `TurnStartRequest{RoomID, Epoch, TurnMillis, MaxTurns}`。房主开局。相同规则的重试只读当前结果，不重新开局。
  - `TurnEnd`：请求 `TurnEndRequest{RoomID, Epoch, Round, Sequence}`。当前行动者结束自己的回合，移交给下一个人，并从这次执行的时刻重新计时。`Sequence` 是该玩家自己的成功命令号，从 1 起连续递增。
  - `TurnView`：请求 `TurnViewRequest{RoomID, Epoch}`。读当前状态。
  - `TurnInfo` 里：`Status` 取 0 等待开局、1 进行中、2 已结束；`ActorID` 是当前行动者；`Deadline` 是服务器业务时间的绝对期限（Unix 毫秒）；`Timeouts` 是累计超时次数；`Sequence` 是**请求者**最后成功的命令号；`Players` 是开局的固定名单。
  - 操作人与连接身份都取自认证上下文；`Epoch` 是玩家在 room 里拿到的房间代际。没有推送协议。
- **服务端入口**（组件 `TurnComponent`，`game/entities/room/turn_component.go`，经 `ITurnEntity` 的 `TurnComp()` 取得；所有方法只在 Nest 持有 Room 锁时调用）
  - `Start(playerID, epoch, connection, turnMillis, maxTurns) (TurnSnapshot, error)`
  - `CheckEnd(playerID, epoch, connection, round, sequence) (replay bool, err error)`：只检查、不推进。`replay` 为真表示这是最后一条成功命令的重发，**不能再执行游戏动作**。
  - `End(playerID, epoch, connection, round, sequence) (TurnSnapshot, error)`：内部先 `CheckEnd`，再记回执、换定时器、推进一回合。
  - `View(playerID, epoch, connection) (TurnSnapshot, error)`
  - Nest handler（`game/handler/`，`rollback=undo durability=strict`）：`handlerTurnStart`、`handlerTurnEnd`、`handlerTurnView`；例如 `syncsender.NewTurnEndSender(client).Sync_TurnEnd(ctx, roomEntityID, playerID, epoch, connection, round, sequence)`。
  - Controller（`game/controllers/turn/`）：`HandleTurnStart`、`HandleTurnEnd`、`HandleTurnView`。
- **登记点 / 钩子**
  - 模块没有“回合开始 / 结束 / 对局结束”的业务钩子。接游戏动作的方式是自己写 handler 组合 `CheckEnd` 与 `End`（见“复用方式”）。
  - 定时器回调（`game/entities/room/turn_timers.go`）：类型常量 `TurnDeadline`；`registerTurnTimers(handlers)` 由安装器追加到 Room 的定时器登记函数里，到期时在 Room 的 Nest 事务中调 `onDeadline`。超时计数、下一回合与定时器续期在同一笔事务里提交。
- **状态**（`db/def/turn.go`）：`RoomDao.Turn`（`TurnState{Status, Round, Active, MaxTurns, TurnMillis, Deadline, TimerID, Players, Timeouts}`，`Players` 的每项是 `TurnPlayer{PlayerID, Sequence, LastRound}`），只落库不同步；加上 Room 实体定时器里的一个持久节点。客户端只能经 `TurnView` 等应答看到。
- **配置**：没有配置表。规则范围写在 `turn_component.go` 的 `Start` 里：每回合 1000～3600000 毫秒（1 秒～1 小时），总回合数 1～10000；工程可以收窄。
- **错误码**（`internal/errors/turn.go`）：`turn_invalid`（规则越界、重试时换了规则、不足两人）、`turn_not_running`、`turn_not_actor`（不是当前行动者、回合号不对、不在名单里）、`turn_sequence`（跳号、旧号、同序号配了不同回合）、`turn_expired`（已到期限，等定时器推进）。成员与连接校验透出 room 的 `room_stale_epoch`、`room_not_host` 等。

## 能力边界

- **不做的事**：没有任何默认玩法——伤害、出牌、移动、奖励、胜负都不提供；没有自动广播（轮到谁要靠客户端查 `TurnView` 或工程自己推）；没有同时行动、抢答、多阶段回合；断线不踢人、不代打；一个房间只开一场，结束后不能在同一个房间号上重开。
- **`MaxTurns` 数的是总回合**：各玩家依次行动一次算一回合，不是“一圈”。两人、`MaxTurns=2` 时各行动一次就结束。
- **名单是固定的**：开局至少两名在座成员，名单按房间座位排序后不再变。中途加入的人可以 `TurnView`，但不进入本场名单；离开的参与者在后续推进时被跳过；同一个人重新加入仍在名单里。
- **重放的范围**：每名玩家只记最后一条成功命令的 `(Sequence, LastRound)`。完全相同的最后一条重发返回**当前**快照、不再推进；更早的命令、跳号、同序号配不同回合都拒绝。重放仍然校验当前的成员资格与连接身份，旧连接即使带着正确的序号也不行。模块只为“无额外载荷的结束回合”判重——加了动作内容之后，回执要由业务自己绑定动作内容或摘要。
- **超时不追帧**：定时器迟到或实体离线很久，只推进一回合，新期限从实际触发的时刻起算。已卸载的房间不会被期限主动加载，下次正常加载时补触发一次。恰好到期限那一刻的 `TurnEnd` 答 `turn_expired`。
- **结束的时机**：达到 `MaxTurns`、房间被关闭、或仍在座的参与者不足两人时结束；后两种在**下一次推进**时才发现，最晚等到当前期限到达。
- **一致性**：回合事实、命令回执与定时器节点都在 Room 上，一笔 Nest 事务提交；提交被拒时一起回滚，超时回调失败后下次仍处理原回合。
- **前提**：依赖 room 模块（沿用它的可信连接身份、单房主、`Epoch` 规则）与 nest Mod；需要 Room 实体上有持久定时器——没有时安装器补一套，已有时复用。房间在哪个进程，回合就在哪个进程，没有跨机接管。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“turn 与定时器边界”一条；“room 的边界”一条里的断线、重启在线状态等同样适用。

## 复用方式

- **需求正好是它**：`roost module add turn`，连带装上 room。流程：服务端经 room 的 `game/room.Open` 开房 → 客户端 `RoomJoin` 拿到自己的 `Epoch` → 房主 `TurnStart` → 当前行动者 `TurnEnd` → 任何成员 `TurnView`。配方 `turn-pvp` 就是这样组合的。
- **需求多一点——接上游戏动作**：自己加一条协议与一个 Room 的 Nest handler，在同一笔事务里按这个顺序写：

  ```go
  replayed, err := target.TurnComp().CheckEnd(playerID, epoch, connection, round, sequence)
  if err != nil {
      return err
  }
  if replayed {
      return nil // 已经成功过的游戏动作不能再执行
  }
  // 校验并修改本场游戏的 DAO；失败直接返回，随事务回滚。
  _, err = target.TurnComp().End(playerID, epoch, connection, round, sequence)
  return err
  ```

  玩法状态放在 Room 上自己的 DAO 字段里。跨实体的奖励要显式把对应实体声明进事务，不能在快池的 worker 里同步等另一个 Nest 请求。需要推送时从已提交的事实接提交后的动作，不在锁内做网络 I/O。
- **其它调整**
  - 收窄回合时长与回合数：改 `turn_component.go` 里 `Start` 的范围判断。
  - 超时要做别的事（扣分、托管）：改 `turn_timers.go` 的 `onDeadline`，仍在同一笔事务里。
  - 不要改：`TurnDeadline` 的类型号（它与持久节点是数据契约，由安装器分配并记在 `roost.yaml` 的 `timers.room.types`）、`TurnState` 的 store 键、`onDeadline` 里“节点号与期限都对得上才处理”的校验。
- **别的模块 / 业务代码要用它**：在 Room 的 handler 里嵌入 `room.ITurnEntity`，用 `TurnComp().View(…)` 读快照；事务外用 `syncsender.NewTurnViewSender(client).Sync_TurnView(ctx, roomEntityID, playerID, epoch, connection)`。
- **不适合直接用的情形**：实时或帧同步玩法、所有人同时提交的回合（如自走棋的准备阶段）、名单会变的回合制、需要观战推送的对局。值得借鉴的是：“每人一个连续命令号 + 最后一条可重放”的回执；用“节点号 + 期限”双重校验让旧的定时器投递不会消费新回合；先 `CheckEnd` 再改玩法、最后 `End` 的三段式。

## 联动

- turn 自己没有 `with/` 目录，别的模块也没有 `with/turn`。
- 对 room 的使用（依赖）：成员与连接校验经 `RoomComp()`，定时器用 Room 上的实体定时器组件。
- 配方 `turn-pvp`（`modules/recipes/`）把 turn 与 enter、profile、room、leaderboard、matchmaking、login、portal、servertime 组合成双人回合对战；配方没有替业务假设胜负规则。

## 验证范围

- `roost module verify turn`：25 条场景（`^TestTurn`，包 `./game/handler` 22 条、`./game/controllers/turn` 3 条）。22 条 Nest 场景覆盖：开局与重试只有一个期限、只有房主能开、规则越界与换规则、至少两人、结束只推进一次并从当下续期、重复结束不多消耗回合、跳号与冲突重放、错误的行动者与回合、恰好到期拒绝命令、期限不提前不重复、迟到不追帧、旧期限不消费新回合、到上限结束且最后一条可重放、迟到加入者能看不能动、跳过离开的人、关房后在下一个期限结束、旧连接不能行动或重放、开局 / 结束 / 超时被拒提交时回滚、重载后保留序号且过期只触发一次、快照不暴露 DAO 数组。3 条 Controller 场景经实体运行时 → lifecycle → Nest Sender，覆盖认证身份与 PB 编解码。定时器用生成的 Tick handler，测试时钟可控，不依赖 sleep。
- 这些场景用替身提交器，不等于真实 Mongo / WAL 落盘、TCP / Gate 联合验收或性能测试。配方 `turn-pvp` 另有双人 TCP → PB → Controller → Nest 的链路场景（在配方里，不在本模块的计数内）。
- 重跑入口：仓库里的 `scripts/test-modules-generated.sh`，或在装好模块的新工程里运行 `GOWORK=off roost module verify turn`。

## 定时器安装细节

- 清单里的 `timer_types: [TurnDeadline]` 让安装器在 Room 这个实体的作用域内分配一个类型号，写进 `roost.yaml` 的 `timers.room.types`，并在已安装记录里记下作用域。已有的显式 int32 常量、iota、直接登记的常量实参与分配记录都占号；删除源码不回收记录里的号。登记表无法静态解析、或被交给外部代码时安装失败，要求先整理登记。
- 模板里的定时器组件名是占位的，安装时换成工程实际的定时器组件名（不必叫 Timer）。组件构造期间只登记回调，不读取尚未构造的 Turn 组件。
