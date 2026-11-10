# cellworld 积分按千分位累计零头（2026-11-10）

基线 main `cd795417`；macOS（Apple M5，10 核），Go 1.27，`GOWORK=off`。同机长期有别的负载（本轮平均负载 9～50）。图谱工具不可用（连接断开），以当前源码为准。

维护者 2026-11-10 的决定：“截断按照现在的浮点数*1000作为整型数处理”。理解为：位移按千分之一单位（千分位）的整数计算，不足 1 单位的零头作为整数千分位保存在角色状态里，下一子步接着累加，全程整型；位置字段与线上的位置仍是整数单位，**不**把位置整体放大 1000 倍（那样会改 `EntityState` / `BaseState`、Ghost、witness、回写、Sync mirror、game-demo 的 `PosX` / `PosY`、地形 `Surface` 的坐标与客户端协议，影响面远大于这个问题本身；零头只在积分里有意义，放在角色状态里就够了）。

## 修了什么

- `gameplay/cellworld/actor.go` `integrate`：每个子步的位移 = `V × dt / 1ms` 千分位（向零截断，丢不到 1 千分位），加上 `ActorState.FracX` / `FracY`，整数部分加到位置、余数留作零头（`settleAxis`）。零头范围 (-1000, 1000)，与“零头 + 位移”同号；真实位置 = X + FracX / 1000。
- `walkOn`（地面钩子）：先按站立地面格的速度千分比缩放位移、按 `settle` 算终点再逐个整数点走；进入速度不同的地面格时，从那个点起把剩下的位移（没被世界边缘夹过的真实终点 − 这个点）按新旧速度之比缩放，重新算终点接着走。原来的“路程预算 reach × 1000、每点花 ⌈10⁶ / s⌉、用完丢余数”去掉，`walkSlowed` 不再存在。进入新格的那一步仍按旧格的速度算，与原规则一致。
- 线上格式 v7 → v8：`handoffOffer` 加 `FracX` / `FracY`（`fx` / `fy`，omitempty）；`onOffer` 装入时带上，不在 (-1000, 1000) 里的当作 0（`validFrac`）。
- game-demo：`SceneActorDao` / `MonsterDao` 加 `CellFracX` / `CellFracY`（int32，`nopersist,nosync`），`game/partition` 的 `CellDao` / `ReadActor` / `WriteActor` 映射，`partition_test` 的往返与回滚样本带非零零头。只改 `demo/db/def/*.go.tmpl` 与 `demo/game/partition/*.tmpl`，生成文件由 `roost project new` 生成。

## 零头在各情形下的规则

| 情形 | 规则 | 用例 |
| --- | --- | --- |
| 符号 | 与“零头 + 位移”同号（Go 整除、取余向零截断）；往负方向走时整数位置在真实位置的正侧，走满 1 单位才变 | `TestTheRemainderKeepsItsSignAndSurvivesASpeedChange` |
| 速度改变（新输入，含反向）、换档 | 不动，接着从真实位置累加（零头是位置的一部分） | 同上 |
| 世界边缘 | 真实位置出了 [Min, Max−1]（整数越界，或恰在边上而零头指向界外）：停在边上、这条轴零头清零，这条轴速度按原规则（玩家清零、NPC 反向）；另一条轴照旧。真实位置恰好落在边上不算越界 | `TestTheRemainderStopsAtTheWorldEdge` |
| 地面挡住、站立格没有定义 | 停在上一个合法整数点，两条轴零头清零（零头指向被挡的方向） | `TestASurfaceBlockClearsTheRemainder` |
| 游泳等减速 | 位移按速度千分比缩短、零头照常累计；进出速度不同的格时缩放剩下的位移 | `TestSwimmingAcrossASpeedChangeKeepsTheRemainder`、红用例的 swim 组 |
| 交接 | 随 Offer 带到目标格，目标格接着累加；冻结中不积分、零头不变；被拒恢复 Real 时不动；线上坏零头当作 0 | `TestHandoffCarriesTheSubUnitRemainder` |
| 进场（Enter，含交接放弃后 Base 按最后位置重进）、生成（Spawn） | 从整数位置开始，零头 0；本格已是 Real 的幂等 Enter 不动零头 | 同上 |
| 事务回滚 | 与位置一起在 DAO 里还原 | game-demo `TestCellStateRollsBackWithTheTransaction`（样本带零头）；cellworld 测试替身整份 `ActorState` 回滚 |
| 没有地面 / Surface 返回 nil / 处处速度 1000 的地面 | 三者逐位相同（含零头） | `TestANilSurfaceIntegratesExactlyLikeNoSurface` |

**交接带零头（推荐，已实施）的理由**：零头就是位置的一部分，带过去后“位置 + 零头 = 速度对积分时间的积分”这一条不分在哪个格积分都成立，不需要在交接处开例外；玩家在边界附近来回（迟滞带内反复交接）时不累计每次至多 1 单位的损失；代价只是 Offer 多两个 omitempty 的小整数，Offer 本来就很少。另一选择是交接时丢零头（不改线上格式），每次交接至多少走 1 单位——与冻结期间不积分造成的位移损失同一量级，也可以接受。

**`TestANilSurfaceIntegratesExactlyLikeNoSurface` 的断言**：比较对象从 `EntityState` + `Role` 扩到再加 `FracX` / `FracY`。它的承诺是“没有地面、Surface 返回 nil、处处放行速度 1000 的地面三者逐位相同”，不是“与旧实现逐位相同”；三者一起换成千分位累计之后彼此仍逐位相同，承诺不变。与修改前的差别只有不丢截断余数这一处，由红用例断言。原有的其余地面用例（被挡、对角、游泳 500‰ 与世界边缘、挡在边界前不交接、跨格地形）断言没有改，修后全部通过：游泳用例原来的数值恰好没有余数（1000/s × 50ms × 500‰ 整除），所以新旧规则答案相同。

## 先红

用例 `TestIntegrationKeepsTheSubUnitRemainder`（`tier_test.go`）：速度固定、40 拍不规则的整毫秒拍长（31～130ms，多数不是 50ms 的整数倍）或 200 拍正好 50ms；断言总位移与 速度 × 总时间（游泳再乘 500‰）相差不到 1 单位。整毫秒拍长下千分位位移没有截断，误差只剩零头本身。没有地面 / 平地 / 游泳 × 400/s、对角反向 (-400, 130)、10/s、10/s 走 10 秒、(-6, 4) × 玩家 / NPC，共 30 组。

修前（`cd795417` 上，命令 `GOWORK=off go test -count=1 -run TestIntegrationKeepsTheSubUnitRemainder ./gameplay/cellworld`），30 组全红，节选原文：

```
--- FAIL: TestIntegrationKeepsTheSubUnitRemainder (0.00s)
    tier_test.go:623: none / 400/s irregular beats / kind 1: v (400, 0) for 3.106s moved (1225, 0), want (1242.400, 0.000) within 1 unit (role real)
    tier_test.go:623: none / diagonal backwards irregular beats / kind 1: v (-400, 130) for 3.106s moved (-1225, 364), want (-1242.400, 403.780) within 1 unit (role real)
    tier_test.go:623: none / 10/s irregular beats / kind 1: v (10, 0) for 3.106s moved (0, 0), want (31.060, 0.000) within 1 unit (role real)
    tier_test.go:623: none / 10/s for 10s in period beats / kind 1: v (0, -10) for 10s moved (0, 0), want (0.000, -100.000) within 1 unit (role real)
    tier_test.go:623: none / slow diagonal backwards / kind 1: v (-6, 4) for 3.106s moved (0, 0), want (-18.636, 12.424) within 1 unit (role real)
    tier_test.go:623: open / 400/s irregular beats / kind 1: v (400, 0) for 3.106s moved (1225, 0), want (1242.400, 0.000) within 1 unit (role real)
    tier_test.go:623: swim / 400/s irregular beats / kind 1: v (400, 0) for 3.106s moved (604, 0), want (621.200, 0.000) within 1 unit (role real)
    tier_test.go:623: swim / 10/s for 10s in period beats / kind 1: v (0, -10) for 10s moved (0, 0), want (0.000, -50.000) within 1 unit (role real)
FAIL
```

400/s 少走 17.4 单位（1225 / 3.106s ≈ 394/s，约 1.4%）；10/s 与 (-6, 4) 一步不动；游泳同样丢余数。

## 绿与变异

修后上述 30 组全绿。新增的规则用例只在修后的状态上有意义（引用 `FracX` / `FracY`），用变异证明它们抓得住：

| 变异 | 变红的用例 |
| --- | --- |
| 世界边缘只看整数位置（去掉“恰在边上而零头指向界外”） | `TestTheRemainderStopsAtTheWorldEdge` |
| 进入速度不同的地面格时不缩放剩下的位移 | `TestSwimmingAcrossASpeedChangeKeepsTheRemainder` |
| 被地面挡住时保留零头 | `TestASurfaceBlockClearsTheRemainder` |
| Offer 不带零头 / 目标格装入时不用 Offer 的零头 / 不校验线上零头范围 | `TestHandoffCarriesTheSubUnitRemainder`（三种各自变红） |

`TestIntegrationLosesLessThanAThousandthPerSubStep`：拍长 47～53ms 带纳秒抖动、200 拍，位置 + 零头与 速度 × 时间 相差不到子步数个千分位，整数位置相差不到 1 单位加这一点。

## 基准

`BenchmarkIntegrateOnSurface`（一个 Period 的一拍、无交接；没有地面 / 平地 `terrain.Walker` × 全速 2000/s / 慢 200/s）。修前修后各编一个测试二进制，同机交替 6 轮 × `-test.count 3 -test.benchtime 0.5s`，`benchstat`（n = 18，开始与结束时平均负载 15 / 11）：

| 子项 | 修前 | 修后 | 变化 |
| --- | --- | --- | --- |
| none/speed2000 | 8.7ns ± 6% | 13.0ns ± 8% | +4.3ns（+49%，p = 0.000） |
| none/speed200 | 8.1ns ± 6% | 13.0ns ± 8% | +4.9ns（+61%，p = 0.000） |
| walker/speed2000 | 558ns ± 6% | 523ns ± 10% | 无显著差异（p = 0.18） |
| walker/speed200 | 73.4ns ± 10% | 76.2ns ± 15% | +3.9%（p = 0.034） |

都是 0 分配。代价在没有地面时：每个子步多两次按常数的除法 / 取余与两个边缘判断，约 4～5ns；按 1 万个角色、每秒 20 拍算约每秒 1ms CPU。平地 Walker 上每个点一次 `Cell` 的成本占主导，零头的计算看不出来。同一天负载更高（平均负载 35）时的第一组对照（`-test.count 4` × 3 轮）方差 ±48%～±320%，不作结论。

## 验证

| 检查 | 结果 |
| --- | --- |
| `gofmt -l gameplay` | 空 |
| `GOWORK=off go build ./...`、`go vet ./gameplay/cellworld` | 通过 |
| `GOWORK=off go test -count=1 ./gameplay/cellworld`（非 race，修后第一遍） | 通过 |
| `GOWORK=off go test -race -count=1 ./gameplay/cellworld` | 见下表 |
| `GOWORK=off go test -count=1 .` | 通过 |
| `python3 scripts/check-doc-anchors.py` | 0 broken |
| 生成 game-demo（`roost project new … -template game-demo --skip-deps`，`replace` 指向本工作树）：`go mod tidy`、`go build ./...`、`go vet ./...`、`go test -count=1 ./...` | 全部通过（35 个包 ok）；`go generate ./...` 后工作树干净；删掉 `WriteActor` 里 `SetCellFracX` 时 `TestCellStateRoundTripsThroughBothDAOs` 两个 DAO 都红 |
| `GOWORK=off go test -count=1 ./codegen/internal/roost/` | 通过 |

整包 `-race`：本轮机器平均负载 9～50（同机别的负载），整包里依赖真实时钟的视野间隔断言（`view gap … exceeds 200ms / 250ms`）与 `TestDamageAcrossBoundaryAppliedOnce` 在高负载时偶发失败。为区分是否本次改动引入，用修前（`cd795417`）与修后的 race 测试二进制同时跑（同样负载）：

| 运行 | 修前 | 修后 |
| --- | --- | --- |
| 整包，3 轮同时跑（负载 11～27） | 1 轮失败：`TestDamageAcrossBoundaryAppliedOnce`（`9 distinct hits applied, want 10`） | 1 轮失败：`TestDamageAcrossBoundaryAppliedOnce`（`want hp 80`）、`TestWorldDiagonalThroughFourCellCorner`（view gap 254ms） |
| 5 个视野连续性用例 × 6 次，2 轮同时跑（负载 20～33） | 60 次中 4 次失败，全是 view gap | 60 次中 5 次失败，全是 view gap |

失败的都是同一类时序断言，修前修后同负载下频率相当；本次改动只改积分算术，不涉及消息与心跳时序。

合并 main 之前（`8d3c3789`）整包 `-race`（`go test -race -count=1`）共跑 10 遍：第 3、5、6、8、10 遍通过；第 1、2、4、7、9 遍失败，失败全部是上述 view gap（第 1 遍另有 `TestNATSWorldJitterAtBoundary` 在 5s 内没看到换向点、最后视野停在 x=9941，与视野停发同一时段；`TestSecondCellProcessForSameSIDIsRefused` 子进程单实例锁丢失），出现在平均负载 10～50 时。没有 `DATA RACE`。

合并 main（`2709660f`：分区地形演练、P4 水与建筑、缺省平地地形；cellworld 积分代码未变）之后（`171b5b3d`）：

| 检查 | 结果 |
| --- | --- |
| `gofmt -l gameplay demo`、`go build ./...`、`go vet ./gameplay/cellworld`、`check-doc-anchors` | 通过（261 个文件 0 broken） |
| 整包 `-race` | 共 6 遍：第 1、2 遍通过，第 3 遍 `TestWorldOnALoaderBackedGetter` view gap 206ms > 200ms（负载 15），**第 4、5、6 遍连续通过**（负载 6～12） |
| `go test -count=1 .` | 通过 |
| 重新生成 game-demo（合并后的模板）：`go mod tidy`、`go build ./...`、`go vet ./...`、`go test -count=1 ./...` | 全部通过（35 个包 ok，含 `game/partition`、`game/entities/monster`）；`go generate ./...` 后工作树干净 |

## 待维护者确认

1. 零头随交接带走（线上格式 v8）而不是交接时丢掉（不改线上格式）。理由见上。
2. 世界边缘按真实位置判定：整数位置恰在边上而零头指向界外也算越界，停在边上、零头清零、改这条轴的速度。修前以整数位置判定，低速角色可以停在边上带着指向界外的速度一直不动（例如 10/s 贴着下边缘往外走：每子步 0 单位，永远不触发“玩家停 / NPC 反弹”）。
3. 游泳的路程规则由“整点预算、丢余数”改为“缩放位移、进新格时缩放剩下的位移”。进入新格的那一步按旧格速度算这一点与原规则相同；不同之处是余数不再丢，以及 ⌈10⁶ / s⌉ 的向上取整没有了（例如 300‰ 原来每点花 3334，100 单位的预算只走 29 点，现在走 30 点）。
4. 每个子步仍在千分位上截断（丢不到 1 千分位），没有再加更细的余量（例如纳秒级）。按决定原话只做到千分位。
