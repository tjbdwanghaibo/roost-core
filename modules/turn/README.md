# 回合模块（turn）

`roost module add turn` 自动安装 room，在 Room 上追加 Turn 组件与 `RoomDao.Turn`，并接入该实体唯一的持久定时器。新工程不需要手写登记或启动代码；已经有定时器时复用它，组件名不必叫 Timer。`roost module verify turn` 验收 25 个场景。

## 使用链路

1. 服务端通过 room 模块 `game/room.Open` 创建房间，客户端 `RoomJoin` 取得自己的 Epoch。接入要求沿用 [room](../room/README.md)，包括可信连接 nonce 与单 owner。
2. 房主发 `TurnStart{RoomID, Epoch, TurnMillis, MaxTurns}`。至少两名在座成员；固定开局名单，顺序按房间座位。回合时长 1000～3600000 毫秒，总回合数 1～10000。相同规则重试只读当前结果，不重新开局。
3. 当前行动者发 `TurnEnd{RoomID, Epoch, Round, Sequence}`。Sequence 是该玩家自己的成功命令号，从 1 连续递增；服务端从认证上下文取 PlayerID 与连接身份。主动结束移交下一行动者，并从本次执行时间重新计时。
4. `TurnView{RoomID, Epoch}` 返回当前状态、回合、行动者、服务器业务时间的绝对期限、固定名单、超时次数与请求者最后成功的命令号。客户端倒计时不能决定到期，也不能传入服务端时间。
5. 到期时生成的实体定时器驱动投递正式 Nest Tick。回调、下一回合与定时器续期在同一事务中提交；失败整体回滚，下次仍处理原回合。

状态值为 0 等待开局、1 进行中、2 已结束。MaxTurns 数的是各玩家依次行动的总回合，不是一圈人数；例如两人、MaxTurns=2，各行动一次后结束。结束后不在同一个房间 ID 重开一场。

## 游戏动作怎么接

模块的 TurnEnd 只是“结束本回合”。伤害、出牌、移动、奖励或胜负都由业务定义，不提供默认规则。业务 handler 在同一 Room 的 Nest 事务内按下面顺序组合：

```go
replayed, err := target.TurnComp().CheckEnd(playerID, epoch, connection, round, sequence)
if err != nil {
    return err
}
if replayed {
    return nil // 已成功的游戏动作不能再执行
}
// 校验并修改本场游戏的 DAO；失败直接返回，随事务回滚。
_, err = target.TurnComp().End(playerID, epoch, connection, round, sequence)
return err
```

增加游戏动作载荷后，业务回执还需要绑定动作内容或摘要；本模块仅为无额外载荷的 End 命令按 `(player, round, sequence)` 判重，不能直接当成任意操作的幂等账本。跨实体奖励需显式声明相应实体，不能在快 worker 内同步等待另一个 Nest 请求。

相同的最后成功命令重放返回**当前快照**，不重复推进；更早命令、跳号、相同序号配不同回合都拒绝。重放仍校验当前成员与连接身份。收到超时或未知结果时，可以先 View 看 Sequence，再决定重试，不把客户端超时当成服务端未执行。

## 定时器与成员边界

- `timer_types: [TurnDeadline]` 分配实体内类型号，记录到 `timers.room.types` 以及 `modules.turn.ids.timer`；`timer_entity: room` 表示作用域。已有显式 int32 常量、iota、直接 Register 的常量实参与分配记录都占号；删除源码不回收记录中的号。无法静态解析或把登记表交给外部代码时安装失败，要求先整理登记。
- 回调放在 `game/entities/room/turn_timers.go`；安装器给业务登记函数追加一次 `component.registerTurnTimers(handlers)`。组件构造期间只登记，不读取尚未构造的 Turn 组件。类型号和持久节点是数据契约，不能手工重编号。
- Tick 迟到或实体离线很久，只推进一回合；新期限从实际触发时刻起算，不追帧。已卸载实体不会被期限驱动主动加载，下次正常加载时补触发一次。
- 中途加入者可查看但不进入本场名单；离开的参与者被后续推进跳过。同一个玩家重新加入原房间仍属于开局名单，使用当前房间代际与连接；如玩法禁止重入，应在房间准入中加规则。
- 断线保留席位，轮到后可以超时跳过；自动断线订阅仍由接入层负责。关闭房间或参与者少于两人的情况在下一次推进时结束，最晚等当前期限到达；没有额外的 Room 生命周期钩子，也不提供断线踢出、重启在线状态重建或跨机房间接管。
- 默认通过请求/响应读取 PB 快照，没有自动广播、EntitySync 或 Robot 压测步骤。需要推送时从已提交事实接业务 effect，不在锁内做网络 I/O。

## 验证范围

22 个 Nest 场景覆盖规则与权限、序号/重放、精确期限、迟到不追帧、旧期限、参与名单、关闭/重连、提交拒绝回滚、BSON 重载与快照隔离；3 个 Controller 场景通过正式 ManagerAccess → lifecycle → Nest Sender，覆盖认证身份与业务 PB 编解码。定时器用生成的 Tick handler，测试时钟可控，场景不依赖 sleep。

这些场景使用替身提交器，不能视为真实 Mongo/WAL 落盘、TCP/Gate 联合验收、Linux 实跑或性能测试。重跑入口：`scripts/test-modules-generated.sh`，或在安装后的新工程运行 `GOWORK=off roost module verify turn`。
