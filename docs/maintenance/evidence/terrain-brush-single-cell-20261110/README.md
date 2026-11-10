# 跨格笔刷直接拒绝（可编辑地形 T08 / P5 收窄，2026-11-10）

分支 `feat/terrain-brush-single-cell`（基于 main `49de7c98`），只在本地提交。决定与契约见[方案 §30.10](../../FEATURE-2026-11-08-editable-terrain.md#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)。

维护者 2026-11-10：“跨格是指跨两个地图进程吗，如果是的话就直接拒绝”；“玩家也不能刷地形，会在地形基础上设计这个限制”。地形笔刷与建筑占地同一规则：覆盖的块归两个及以上的格（地图进程）时，Base 在预扣配额与发号之前答 `terrain_cross_cell`（100070），不拆分转发。

## 修前红

在 main `49de7c98` 上只加 `ErrBrushCrossCell` 的声明（没有任何行为），新用例 `TestTerrainRelayRefusesABrushAcrossCells` 红在第一条承诺上（跨格笔刷被执行、没有拒绝）；格一侧的承诺（转发来的笔刷也覆盖了邻格的块时不执行本格那一部分）用同一检出上的临时用例单独核对，修前格执行了自己那一块（`Outcome:applied`）。原文见 [red.txt](red.txt)：

```
--- FAIL: TestTerrainRelayRefusesABrushAcrossCells (0.02s)
    terrain_relay_test.go:390: a brush across two cells: <nil>, want ErrBrushCrossCell before anything is charged or issued
```

用例的其余承诺（零写入、配额不变、序号不前进）在修前被第一条挡住，它们各自的红由下面的变异给出。

## 变异

生成的 game-demo 上逐项只改一处、跑相关用例、还原（每项第一条失败原文见 [mutations.txt](mutations.txt)；行号是当时的版本）。9 项：8 项红，1 项未杀死。

| 变异 | 结果 | 红在哪条承诺上 |
| --- | --- | --- |
| 不拒绝跨格笔刷 | 红 | 跨格笔刷没有被拒绝 |
| 跨格拒绝放到预扣配额之后（拒绝时按其他拒绝的口径退到 `MinEditPoints`） | 红 | 配额：满桶才过得去的笔刷答 `terrain_rate` |
| 跨格拒绝放到发号之后（配额全退，只看序号） | 红 | 序号：同一序号再用答 Again |
| Unknown 的点数退回 | 红 | 结果未知那一笔的配额回到桶里 |
| 格不查“覆盖的块都归本格”（`execBrush` 改回 `checkBrushParams`） | **未杀死** | 事务里 `applyBrush` 对每块再查 `Owns`，同样答 `ErrNotOwned`（防御重叠）；两处都去掉时红（下一行） |
| 格前置检查与锁内归属检查都去掉 | 红 | 格执行了也覆盖邻格的笔刷 |
| `ErrBrushCrossCell` 不映射业务码 | 红 | 答 internal |
| 拒绝 / 未知时答复不列块 | 红 | 逐块 `Outcome` 缺失 |
| 结论把 Refused 当成功 | 红 | 格拒绝时结论不是格的原因 |

## 验证

见 [verify.txt](verify.txt)。全部 `GOWORK=off`，离线（进程内网、离线持久层）；没有连真实数据库、NATS、Redis。

## 尚未确认：一次 `-race` 失败没有复现

生成工程 `go test -race ./internal/service/game` 的第一次运行里 `TestTerrainRelayForwardsABrushToTheCellThatWritesIt` 失败一次；那次与 `go vet ./...`、`gameplay/...`、`codegen/internal/roost` 同时在本机运行，输出过滤只留下了 FAIL 行，失败原文没有保存。之后单独 3 次、3 份并发 3 次、`-race -count=40 -run TestTerrainRelay` 四份并发都通过。用例里与时间有关的只有转发的期限（用例配 300ms，期限到了结论是 Unknown，`result.Err()` 非空即失败）；块版本与账本在答复前已提交；视图在提交后回调里更新，P5 原用例同样在答复后立即读视图、一直通过，本轮没有单独核对回调与答复的先后。疑似重负载下超过 300ms 期限，证据不足，记为尚未确认（[KNOWN-LIMITS](../../KNOWN-LIMITS.md)）。P5 原用例同样在 300ms 期限内做两次远端转发，暴露面相同。

## 没有运行的

- `ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`：(a) 步已改写（跨格笔刷答 `terrain_cross_cell`、两块与 `LastSeq` 不变；B 发起、整份落在 A 格块 (1,0) 的笔刷转发执行，再改回；记 `b_cross_cell_refused_rtt_ms` 与 `b_forward_to_a_rtt_ms`），只编译与 vet，真实运行在合入时。
- C# / dotnet 测试：没有改 C# 代码；C# 地形夹具 `regen.sh` 核对模式一致。

## 合入前的真实依赖验证（协调方，2026-11-10）

在本分支 HEAD 上，用私有 mirror-local 依赖（真实 Mongo / Redis / NATS，跑完即清理）执行：

- `scripts/test-demo-generated.sh`：10 项真实依赖用例全部 PASS。
- `ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`：六步全部 ok。`terrain_ghost_edit` 的跨格笔刷在 Base 上被拒（往返 0.057ms，不经网络）；B 发起、整份落在 A 格的笔刷转发执行，往返 48.9ms / 15.8ms（升高 / 改回）。
- 生成 game-demo 的 `internal/service/game` 在 `-race -count=3` 下通过（252s，单独运行）；“尚未确认”的那次 flake 没有复现。
- 同轮：根包、`gameplay/...`、`codegen/...`，生成 game-demo 全测（36 个包），`test-sync-modes-generated.sh`，C# 地形测试，全部通过。
