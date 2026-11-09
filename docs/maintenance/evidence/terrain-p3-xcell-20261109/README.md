# 可编辑地形 P3 跨格移动按地形判定（T06 剩余）验证证据（2026-11-09）

实现与决策见 [方案 §24](../../FEATURE-2026-11-08-editable-terrain.md#24-p3-跨格移动按地形判定t06-剩余2026-11-09)；进度见 [TERRAIN-STATUS T06](../../TERRAIN-STATUS.md)。本地分支 `feat/terrain-cross-cell`，基于 main `58b9d9f9`（T05 Ghost 副本已在其中），未推送、未合入，不是已发布状态。

## 环境

Apple M5，macOS，Go 1.27.3，`GOWORK=off`；机器有后台负载。没有用到真实依赖（Mongo / NATS / Redis）：cellworld 用例走进程内回环传输，demo 用例的持久层是 `terrain_test.go` 的离线替身、cellworld 消息走测试里的进程内传输。`scripts/test-seamless-demo.sh` 与 `scripts/mirror-local.sh` 本轮没有跑。

## 先红后绿

库代码先改（`049466db`），用例同时写；“修前”的红用变异 M0 取得：`advance` 不取钩子（`ground` 恒为 nil），与 main 上没有钩子的积分逐位相同（`TestANilSurfaceIntegratesExactlyLikeNoSurface` 证明 nil 路径与原代码同一结果）。demo 的修前红是生成工程里 `partitionSurface` 返回一个答 nil 的地面。每个变异都在本分支上临时改代码、跑完还原。原文见 [mutations.log](mutations.log)，摘要：

| 变异 | 变红的用例与原文（节选） |
| --- | --- |
| M0 钩子不生效（= main） | `player walking into the refused cell: at (1400, 5000) v (2000, 0) real, want stopped at (1149, 5000) with v 0`；`walking towards the missing ghost chunk: at x=12900 vx 2000 real, want stopped at 12749`；`player 61: timed out waiting for stopped at the wall; last view cell=102 epoch=2 self=(22396,3000)`（走穿墙、交给了邻格）；`blocked: role frozen at x=10100, want frozen false`；减速用例 `at x=3200 ... want 3100` |
| demo M0（`partitionSurface` 答 nil） | `timed out waiting for the npc turns back at the step on B's chunk on sid 101: real here true, last (13786, 3000) v (1000, 0), max x 13786`（走到冻结线交给 B） |
| M1 只看这一步的终点 | `at x=12700 vx 0 ... want stopped at 12749`；墙前停在 13196；`at (1100, 5000) ... want (1149, 5000)` |
| M2 玩家被挡不停 | `at x=12749 vx 2000 ... want stopped`；`standing on an undefined cell: at x=6000 vx 1000` |
| M3 NPC 被挡不折回 | `npc towards the missing ghost chunk: at x=12749 vx 2000, want turned back`；`blocked: at x=9949 vx 2000, want turned back before 9950` |
| M4 忽略速度千分比 | `swimming 200ms at 1000/s with speed 500‰: at x=3200 vx 1000, want 3100` |
| M5 站立格没有定义也走 | `standing on the missing ghost chunk: at x=13049 vx 0, want held` |
| M6 没走到终点也套用世界边缘的速度规则 | `npc swimming towards the world edge, beat 1: at y=40 vy 2000, want y=40 vy -2000` |
| M7 减速预算按夹过的终点算 | `npc swimming towards the world edge, beat 1: at y=45 vy -2000, want y=40 vy -2000` |

M6 第一次没有变红（用例没有“减速 + 世界边缘”的组合），补了游向世界边缘的三拍之后变红；补用例时发现预算按夹过的终点算会让减速的角色在边缘附近少走（M7），改为按未被夹过的位移算。

## 复跑

```bash
# 仓库根
GOWORK=off go test -count=1 -run 'CrossCell|Surface' ./gameplay/cellworld/
GOWORK=off go test -race -count=1 ./gameplay/cellworld ./gameplay/terrain ./gameplay/movement   # 跑两遍
GOWORK=off go test -run '^$' -bench IntegrateOnSurface -benchmem -count 3 ./gameplay/cellworld/
GOWORK=off go test -count=1 ./codegen/internal/roost/
GOWORK=off go test -count=1 .
python3 scripts/check-doc-anchors.py
# 生成工程（go.mod 追加 replace 到本仓库）
go run ./codegen/cmd/roost project new gd -module example.com/gd -out <tmp>/gd -template game-demo --skip-deps
cd <tmp>/gd && go mod tidy && go build ./... && go vet ./... && go test -count=1 ./...
go test -race -count=2 -run TestTerrainPartitionWalkAcrossCellsFollowsTheEdits ./internal/service/game/
```

## 结果

| 项 | 结果 |
| --- | --- |
| `gofmt -l`、`go build ./...`、`go vet`（cellworld / terrain / movement / codegen roost） | 空 / 通过 |
| cellworld 新增用例（6 条，`-run 'CrossCell\|Surface'`） | 通过；`TestCrossCellWalkFollowsTheNeighboursTerrain` 约 3.6s |
| `-race` cellworld / terrain / movement，两遍 | terrain、movement 两遍通过；cellworld 第二遍通过，第一遍 `TestHandoffHysteresisAbsorbsJitter` 失败一次（见下方“验证记录”） |
| `codegen/internal/roost` | 通过（290s） |
| 生成工程 build / vet / 全部测试 | 通过（35 个包 ok，[demo-test.log](demo-test.log)；`internal/service/game` 63s） |
| 生成工程 `TestTerrainPartitionWalkAcrossCellsFollowsTheEdits` `-race -count=2` | 通过 |
| 根包 `go test -count=1 .`、`check-doc-anchors.py` | 通过（17s）；255 个文件 0 处断链 |

基准（`BenchmarkIntegrateOnSurface`，一个 Period 的一拍，3 轮，后台负载下波动大）：

| 地面 / 速度 | ns/op | 分配 |
| --- | --- | --- |
| 没有地面 / 2000 每秒（一拍 100 个点） | 21.6～29.5 | 0 |
| 没有地面 / 200 每秒 | 12.0～14.6 | 0 |
| 平地 Walker / 2000 每秒 | 805～1746 | 0 |
| 平地 Walker / 200 每秒（一拍 10 个点） | 85～155 | 0 |

## 验证记录

cellworld 全包 `-race` 第一遍（与 terrain、movement 两个包同时跑）里 `TestHandoffHysteresisAbsorbsJitter` 失败：`handoff_test.go:81: 1 handoffs inside the hysteresis band`；同一提交第二遍全包 `-race` 通过，之后单独再跑一遍全包 `-race` 也通过（154s）。判断与本轮改动无关，原因尚未确认：

- 这个用例没有设地面钩子，走的是 nil 路径；nil 路径与改动前逐位相同由 `TestANilSurfaceIntegratesExactlyLikeNoSurface` 断言（`advance` 只多一次 nil 判断）。
- 用例让玩家以 400 / 秒在分界线 ±60 来回，看到 witness 里 X ≥ 10060 才发反向输入；冻结线在 10100，留给“witness 到达测试 → 输入投到格 → 下一拍生效”的只有 40 单位 = 100ms。race 下与别的包同时跑，这段延迟超过 100ms 就会越过冻结线交接一次。疑点是夹具的时间余量，没有在 main 上做对照复现，按“尚未确认”记录，不当作本轮缺陷，也不改这个用例。KNOWN-LIMITS 里“cellworld 间歇失败仍未关闭”的条目与它是否同一问题也未确认。

## 未覆盖

- 开地形的真实多进程演练（NATS、demo 的 SceneActor / Monster 实体、`startPartition` 里装钩子的分支）：demo 的格内实体经进程级的 `partition.Node()` 找 Node，测试进程里装不下两个格，demo 用例用替身 NPC；演练配置的 `cell_size` 需改为 6400 的倍数。
- 玩家的 TCP 客户端视角（停下后的位置推送）没有新用例；cellworld 用例用 Base 收到的 witness 断言。
- T09 规模下的积分成本与副本延迟。
