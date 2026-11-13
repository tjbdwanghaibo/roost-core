# 可编辑地形 T08 / P5：跨格编辑、分区场景的建筑与分区视野（2026-11-10）

分支 `feat/terrain-p5-crosscell`（基于 main `7c5e6a90`，合入 main `66aecd92` 后做最后验证），只在本地提交。设计与取舍见[方案 §30](../../FEATURE-2026-11-08-editable-terrain.md#30-p5-跨格编辑分区场景的建筑与游戏尺度的分区视野t082026-11-10)。维护者 2026-11-10：未完成的代码先完成，验收后置——本证据只有离线验证（进程内网、离线持久层、真实 Nest 与生成代码）；真实依赖与真实多进程演练由维护者跑。

## 修前红

跨格编辑与 Ghost 带占位在改动前没有入口：改动前的代码对跨格编辑答 `terrain_not_owned`、分区场景放置答 `building_disabled`，新用例在修前的代码上编译不过，不能当红。每条行为的红由“只撤掉守卫它的那一行”的变异给出（下节）。开发中真实变红一次（Ghost 带的占地启动时不装，已修）：

```
--- FAIL: TestTerrainPartitionGhostBandSeesNeighbourBuildings (0.01s)
    terrain_relay_test.go:733: the restarted neighbour does not see the building
```

## 变异

在生成的 game-demo 上逐项只改一处、跑相关用例、还原（原始输出的第一条失败见 [mutations.txt](mutations.txt)）。22 项：19 项红，3 项未杀死；2026-11-14 为这 3 项各补一条用例，全部变红（[mutations-20261114.txt](mutations-20261114.txt)）。

| 变异 | 结果 | 红在哪条承诺上 |
| --- | --- | --- |
| 拆分全部给第一个块的格 | 红 | 跨格笔刷只剩一份 |
| 格不按归属取自己那一份 | 红 | 格拒绝（目标组与笔刷不符） |
| 汇总忽略拒绝 | 红 | 部分拒绝的汇总不是拒绝原因 |
| 期限到答 Applied | 红 | 答复丢失时汇总不是 pending |
| 格账本不去重 | 红 | 重发写第二次（窗口的 Record 自己的守卫先触发，表现为结果未知） |
| 格账本判超前 | 红 | 远超前的序号在格上被拒 |
| Base 发号不记下 | 红 | 重试不是 Again；Base 重启后不认得 |
| Unknown 的点数退回 | 红 | 结果未知那一份的配额回到桶里 |
| 不退配额 | 红 | 什么都没改的笔刷之后余额不对 |
| 不拒绝跨格占地 | 红 | 跨格放置没有在转发前拒绝 |
| 副本不带占位 | 红 | 邻格 Ghost 带等不到占位 |
| 收到的占位不进占位表 | 红 | 同上 |
| 启动时不装 Ghost 带的占地 | 红 | 重启后的邻格看不见建筑 |
| 线上格式不查版本 | 红 | 坏版本不计 decode error |
| 格不缓存确定结论 | 红 | 重发不经缓存（计数断言；块与账本仍只写一次，由账本兜底） |
| 本格那一份也走网络 | 红 | 本格的请求经过了网 |
| 开地形的分区场景仍用 demo 尺度视野 | 红 | 主视野不是 `TerrainView` / 32m 块 |
| 设置不查 `view_radius` | 红 | `view_radius` 400 被接受 |
| `ErrPending` 不映射业务码 | 红 | 答 internal |
| 不在占地提交后立即推送 | 红（2026-11-14 补） | 拥有者没有推送（`PushedOut` 不增），占位只由续期补齐（`TestTerrainPartitionGhostBandSeesNeighbourBuildings` 数推送） |
| 格不做在途去重 | 红（2026-11-14 补） | 执行中的三次重发各执行一次（`TestTerrainRelayRunsARequestOnceWhileItIsRunning`：4 次） |
| 不核对答复的 incarnation | 红（2026-11-14 补） | 旧 incarnation 同一序号的答复被当成新请求的结论（`TestTerrainRelayIgnoresAnAnswerFromAnEarlierIncarnation`） |

## 验证（合入 main `70623709` 之后）

见 [verify.txt](verify.txt)。

## 没有运行的

- ~~真实 Mongo / WAL / NATS；`ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`~~ 合入前协调方已在私有 mirror-local 依赖上跑过（2026-11-10，见文末“合入前的真实依赖验证”）。
- C# / dotnet：没有改 C# 代码与 `client/spec`，没有跑。
- 性能：游戏尺度分区视野的成本没有测。

## 合入前的真实依赖验证（协调方，2026-11-10）

在合入 main 之前的分支 HEAD 上，用私有 mirror-local 依赖（真实 Mongo / Redis / NATS，跑完即清理）执行：

- `scripts/test-demo-generated.sh`：10 项真实依赖用例全部 PASS（含 `TestTerrainRestartRealMongo`、`TestTerrainEditLedgerRealMongo`、`TestBuildingRestartRealMongo`）。
- `ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`：`terrain_start`、`terrain_ghost_edit`（B 发起的跨格笔刷，往返 82.9ms / 19.7ms）、`terrain_player_step`、`terrain_monster_step`、`terrain_cell_restart`、`terrain_consistent` 六步全部 ok。
- 普通（不开地形）无缝演练：前 14 步 ok，最后一步压测阈值仍不过（p95 30.4s > 16s，2 个样本），与合入前 main 上的已知现象相同，KNOWN-LIMITS 里仍记“尚未确认”。
- 同一轮还跑了：`framework/sync`、`framework/entity` 的 `-race`；根包、`gameplay/...`、`codegen/...`；生成 game-demo 全测（36 个包）及 `-race`；`test-sync-modes-generated.sh`；C# 两组测试。全部通过。
