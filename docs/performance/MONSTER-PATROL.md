# 普通怪单次巡逻决定：直接调用与旧动作包装

`BenchmarkOrdinaryPatrolDecision` 在正式 `monster` 模块生成工程内运行。两组都用 Nest 和同一个普通怪夹具，每次发起同一种巡逻；起步后故意令事务失败，把移动路线和游标撤回，使下一次仍需做完整决定。差别仅是直接调用组件 `Decide`，或先经旧 `monster.Decide(owner, now, fn)` 创建控制器和瞬时动作。计时包括派发和回滚，不含数据库、网络、重生或真实时间推进。不是生产吞吐、节拍截止或整张地图 CPU 指标。

复跑：使用本检出的 CLI 生成 `-template game` 工程并 `roost module add monster` 后，在生成工程中执行：

```sh
go test -run '^$' -bench '^BenchmarkOrdinaryPatrolDecision$' -benchmem -benchtime=300ms -count=5 ./game/modules/monster/service
```

在已包含共享路点实现的生成工程、本机 darwin/arm64、Go 1.27.3，五轮结果：

| 路径 | ns/op（五轮） | 中位 B/op | 每次分配次数（各轮） |
|---|---|---:|---:|
| 组件直接决定 | 10215、14905、11794、12213、12177 | 6740 | 94 |
| 旧 AI / actionflow 包装 | 15146、14232、14621、14071、14552 | 10415 | 134 |

时间中位数分别为 12.18 µs 和 14.55 µs，同机还有生成、演练等活动，不将这次耗时比作为性能门禁。更稳定的差异是少了 40 次分配。`TestMonsterFailedDecisionRollsBackCursorAndMovement` 验证后续失败后路线、目标和游标都能回滚；`TestMonsterOldLifeDamageAndCommandsAreRejected` 验证上一生命的迟到命令不得推进新生命。兼容入口仍在，普通怪默认不使用它。
