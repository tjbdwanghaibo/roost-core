# cellworld：多进程无缝大世界 v1 原型

**原型，不是现行能力。** 只在分支 `feat/seamless-world-prototype` 上；设计、取舍与实测见 [无缝大世界方案 §5 / §6](../../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#5-v1-原型设计2026-11-03分支-featseamless-world-prototype)。不依赖、也不改动 Nest / DataEngine / Sync 三块。

沿 BigWorld 的 Cell + Ghost 流派：Player 不迁移（Player 进程充当 BaseApp），迁移的只是非持久的空间代理。

## 角色

| 类型 | 运行在 | 职责 |
| --- | --- | --- |
| `Grid` | 所有进程（同一份配置） | 静态方格分区：格 → Cell 进程 sid；边界带 G、交接迟滞 H |
| `Cell` | 每个格一个进程（v1 一个进程一个格） | 本格 Real（唯一写权威）、邻格 Ghost（只读镜像）；按心跳积分位置、发 Ghost、交接、算玩家视野、结算伤害 |
| `Base` | Player 所在 game 进程 | 记住玩家当前 owner Cell（epoch 只前进），转发移动意图与攻击，接收视野；视野静默或交接放弃时重新进入 |
| `Transport` | — | 消息通道；`NATSTransport(fnats.IClient)` 用 NATS core pub/sub，每个进程一个收件主题 `roost.cellworld.<world>.<sid>` |

## 用法

```go
grid, _ := cellworld.NewGrid(cellworld.GridConfig{
	CellSize: 50000, Cols: 2, Rows: 2, GhostBand: 3000, Hysteresis: 200,
	Owners: map[cellworld.CellID]int32{{0, 0}: 101, {1, 0}: 102, {0, 1}: 103, {1, 1}: 104},
})

// Cell 进程（sid 101）
cell, _ := cellworld.NewCell(cellworld.CellConfig{
	World: "w1", Grid: grid, Cell: cellworld.CellID{Col: 0, Row: 0},
	Transport: cellworld.NATSTransport(natsClient), ViewRadius: 2500, // Period 缺省 50ms
})
go cell.Run(ctx)
<-cell.Ready()
_ = cell.SpawnNPC(ctx, cellworld.EntityState{ID: 9001, X: 100, Y: 100, HP: 100})

// BaseApp（Player 所在 game 进程，sid 900）
base, _ := cellworld.NewBase(cellworld.BaseConfig{
	World: "w1", SID: 900, Grid: grid, Transport: cellworld.NATSTransport(natsClient),
	OnView:         func(v cellworld.View) { /* v.Entities 含邻区 Ghost */ },
	OnAttackResult: func(r cellworld.AttackResult) { /* Applied / Rejected / Unknown */ },
})
go base.Run(ctx)
<-base.Ready()
_ = base.Enter(playerID, spatial.Point{X: 48000, Y: 1000}, 100)
_ = base.Move(playerID, 500, 0) // 速度，单位 / 秒
hit, _ := base.Attack(playerID, 9001, 10)
```

## 并发与契约

- `Cell` 的全部状态只由 `Run` 的事件循环 goroutine 读写；传输回调只解码后非阻塞投进有界收件箱（满了丢弃并计数）。`SpawnNPC` / `Inspect` 经控制通道进循环执行，不能在 `Hooks` 回调里调用。
- 消息至多一次：每个实体的版本是 (epoch, seq)，接收端只接受更新的版本；交接靠重传与幂等回复，伤害靠 HitID 去重（随交接带走）。
- 同一 (实体, epoch) 至多一个 Cell 取得 Real：旧格先冻结再发 Offer，得不到结论时只删除、不恢复。
- `Base` 用一把短锁保护玩家表；发布与 `OnView` / `OnAttackResult` 回调都在锁外。

## 测试

| 文件 | 内容 |
| --- | --- |
| `grid_test.go` | 分区映射、边界带、迟滞、版本序 |
| `handoff_test.go`、`combat_test.go` | 确定性夹具（手动投递，可丢 / 重复 / 扣住）：交接正常、丢失、重复、超时、拒绝、来回、斜穿四格链；跨格伤害只结算一次、迁移中重试、去重随交接、结果未知、去重表满 |
| `world_test.go` | 进程内多 Cell（替身传输，可注入丢失 / 重复 / 乱序）：直线、抖动、斜穿四格、跨界攻击、慢交接窗口、丢包、Cell 崩溃重进 |
| `nats_test.go` | 同一组场景跑在真实 nats-server 上（测试自起私有端口，PATH 里没有 nats-server 或 `-short` 时跳过） |
| `process_test.go` | 每个 Cell 一个 OS 进程（测试二进制自 exec），跨进程检查至多一个 Real 与伤害只结算一次 |
| `measure_test.go` | 原型测量：`ROOST_CELLWORLD_PERF=1 GOWORK=off go test -run TestCellWorldMeasure -v ./gameplay/cellworld/`（档位 `ROOST_CELLWORLD_PERF_NPCS=1000,5000`，窗口 `ROOST_CELLWORLD_PERF_SECONDS=20`） |
