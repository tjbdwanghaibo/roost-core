# MMORPG 框架接入与验收

从 [SLG 接入](SLG-INTEGRATION.md) 已覆盖的登录、资产、持久化、Remote 和计时能力继续，MMORPG 重点增加同屏交互、场景切换、队伍与交易。业务玩法不放进公共框架；本篇列明可执行接法和尚未验证的边界，不代表完整 MMORPG 产品已完成。

## 正式生成消费者

在仓库根目录、使用 go.mod 声明的工具链运行：

```bash
bash scripts/test-mmorpg-generated.sh
```

脚本从 `game` 模板生成新工程，经 CLI 添加 Player / Scene DAO 和 Scene 实体，仅填业务定义与 handler，再生成组件、访问器和 Sender。成功删除临时工程；`ROOST_MMORPG_KEEP=1` 或失败时保留。必须实际通过 4 条 race 场景，随后检查 ID 与生成状态。业务夹具在 [scripts/testdata/mmorpg](../../scripts/testdata/mmorpg/)。

| 场景 | 业务建模 | 验证契约 |
| --- | --- | --- |
| 两人组队 | 两个 Player 同一 party ID；已有其他队伍时拒绝 | 多实体 Sender 正式锁定两个玩家，重放不改变成员事实 |
| 并发交易重放 | 单笔串行转账，发送方单调序号绑定收款人及金额 | 8 个并发同身份请求只转一次；同序号改金额拒绝 |
| 交易拒绝与重试 | 扣款、入账和序号写入后注入业务拒绝 | 两玩家资产与幂等事实一起回滚，随后同序号可重试 |
| 切图、返回与旧请求 | Player 归属 / 版本与两个 Scene 人数同事务；示例容量 100 | 失败无半次切图，成功重放不重复计人数，返回后拒绝旧版本切图 |

这些是业务演示规则，不是框架默认交易协议。提交器是替身，证明生成代码、Nest 锁与 DAO undo 的组合；文件 WAL / Mongo 原子提交与恢复另由 `scripts/test-dataengine-generated.sh` 验收。示例只保存最近一笔交易身份；旧序号拒绝，不承诺任意历史结果查询。授权、物品交换、交易确认窗口、断线取消、队伍上下线与解散需要业务定义。

## 场景、同步与玩家归属

同进程切图的持久状态与 AOI 订阅是两个阶段。先由 Nest 确认业务归属，再更新旧 / 新场景订阅；订阅失败时按持久归属重试或重建全量，不能在事务尚可回滚时向客户端宣告切图成功。上面的纵切只验证第一阶段，尚未实现双 Scene 与网络客户端的完整切换。

现有 game-demo 的 `internal/service/game/scene.go`（[模板](../../demo/internal/service/game/scene.go.tmpl)）通过 Join / Ready / Leave 接入 EntitySync 与 Interest，SetTeam 把队伍变成关系订阅；这不是完整队伍服务，也不是跨进程地图迁移。现有生成回归实际检查观察另一玩家变化、离场删除、停读连接被关闭后重连全量恢复、旧 session 断开不删除新成员，以及卸载重载后观察者恢复。入口：新生成 game-demo 中运行：

```bash
go test -race -count=1 ./internal/service/game ./game/scene/... ./cmd/loadtest
```

这些场景含真实 loopback TCP，但鉴权与部分依赖使用测试替身；不能据此声称真实渠道认证或客户端引擎验收。Block AOI 的其他装配与容量边界见 [Block AOI](INTEREST-BLOCK-AOI.md)，不能把同进程单场景通过推成跨地图密集扇出的容量保证。

Player 当前静态绑定 game sid，[EnterGame 模板](../../demo/game/controllers/player/enter_game.go.tmpl)在装载前核对 account 认证的 server_id；[搬服](PLAYER-MIGRATION.md)是按 sid 排空后换机器，绑定不变。玩家动态跨 sid 无缝迁移不是现有保证；需要先定义权威交接、旧连接准入、路由与恢复协议，不能只改 SceneID 或借 Remote Guild 的 lease 推断 Player 已支持。

跨服务交易可用 Saga 原生步骤与事务性 effect，持锁期间不做远端 I/O。main 已提供 `roost add saga -native` 与 `roost add effect`；真实生成入口是 `scripts/test-demo-generated.sh`，另有 integration 标签的 `TestRealStepsRunNativeStepsUnderTheRealNestScheduler`。Saga 是前进 / 补偿流程，不等同于跨服务瞬时原子交易；单进程容量的已接受边界见 [Saga D12 实测](../performance/SAGA-D12-20261018.md)。

## 后续验收边界

优先继续做双场景网络切换：切图提交后新场景全量、旧场景删除、订阅中断重试、重连按权威归属恢复，并隔离旧代际消息。之后补业务交易确认 / 取消与队伍解散的完整状态机。真实客户端、跨机器故障、目标玩家密度与长稳需要各自的实际证据；本轮不把它们标成完成，也不把业务尚未定义的玩法登记成框架 bug。
