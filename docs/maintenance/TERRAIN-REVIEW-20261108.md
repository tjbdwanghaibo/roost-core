# 可编辑地形最终方案与 P1/P2 审查

日期：2026-11-08（Asia/Shanghai）。基线：main [f0ff1727](https://github.com/tjbdwanghaibo/roost-core/commit/f0ff1727)，相对上一轮 6fba8a8b。提交到固定审查分支，不修改运行时，不创建 issue/PR。

## 结论与范围

2.5D 高度场、不可变基础包加编辑覆盖、厘米单位、DAO 列表增量、独立地形同步命名空间及超额延迟发送的方向合理。本轮确认两项 P2 去重问题，不能据此宣布整个方案已完成。第一项是方案已明确采用的有界去重窗口尚缺过期处理；第二项是去重键实现缺陷。

重点检查 terrain 坐标/采样/基础包/编辑、生成的 TerrainChunk 与 Brush 事务、C# 查询与覆盖、EntitySync D15/D17 配额和超额处理，以及方案阶段状态。新增范围包含大量测试与文档，不宣称逐行穷尽全部 169 个变更文件。图谱工具不可用，使用 diff、精确源码和执行探针补证。

## RV-20261108-01：旧请求在 32 条记录淘汰后重新执行（P2，设计边界）

位置：[SurfaceComponent.WasApplied / RecordApplied](https://github.com/tjbdwanghaibo/roost-core/blob/f0ff1727/demo/game/entities/terrain_chunk/surface_component.go.tmpl#L312)、[Brush / ApplyBrush](https://github.com/tjbdwanghaibo/roost-core/blob/f0ff1727/demo/game/terrainmap/terrainmap.go.tmpl#L702)。

Applied 只保存块内最近 32 个编辑 ID。请求 1 成功后，对同一块执行请求 2～33，再重试请求 1：返回 Duplicate=false，采样高度从 82330 再变为 82340 厘米。连续三轮复现；只执行 1～32 再重试 1 的控制组三轮通过。

代码和方案已经说明窗口只有最近 32 条，因此这不是隐藏的无限去重承诺实现错误。不过，窗口外请求仍被当作全新请求接纳，没有过期拒绝或独立操作收据，无法支持可靠的超时重试／未知结果确认。共享块的其他玩家编辑也会淘汰记录；未来跨格编辑与资源扣减需要更明确的操作身份和恢复契约。

建议：定义可接受的请求生命周期，使用持久操作收据或可验证的请求代际/序列窗口，窗口外请求明确拒绝，不能再次执行；支持乱序时不能简单只存最大序号。块内短环可保留作快速路径，单纯扩大环不解决问题。跨格子操作及业务资源账本需要引用相同父操作身份。

探针利用现有 brushProcess 禁用限速以隔离语义；限速不能建立永久去重边界，真实流程可间隔发送相同序列。本轮没有模拟真实 Mongo 崩溃恢复。

## RV-20261108-02：63 位 EditID 可确定性碰撞，合法请求被跳过（P2，实现）

位置：[EditID](https://github.com/tjbdwanghaibo/roost-core/blob/f0ff1727/demo/game/terrainmap/terrainmap.go.tmpl#L657)。

实现为 splitmix64(playerID XOR splitmix64(requestID)) 清掉最高位。splitmix64 在 64 位域可逆，清掉最高位后，可直接构造同一玩家下另一个合法非零 requestID，得到完全相同的 EditID；并非只能依赖极低概率的随机碰撞。

正式生成工程的同一玩家，请求 1 与 1959905738347866660（均在有符号 64 位正数范围内）得到相同 EditID。第一个编辑成功，第二个被返回 Duplicate=true，合法的第二次编辑没有执行。连续三轮复现。探针包含逆变换与构造前置断言。

建议：持久化并比较完整的 (playerID, requestID) 身份，可用复合结构或无歧义字符串。摘要可以做索引，不能只凭截断摘要判定操作相同；如需防止同身份换参数，增加 payload 身份校验。跨格子 ID 也应使用明确的父操作与子目标身份。

## 尚未实施的内容与方案建议

- 最终暂停说明明确：配额 300 改为 270 已选定但未落地；P3 两个本地 WIP 分支尚未合入。这些是已登记待办，不作为新增实现缺陷。
- P3 地形变化后角色推离、移动/寻路/水域，以及后续建筑、跨格编辑和规模验证，不能按当前 P1/P2 测试通过认定完成。
- 270 点/秒的带宽估算依赖附近约 20 人等模型，不是 3000 人集中编辑或首次全量同步的硬带宽保证。当前配额依据锁外视图估算，且允许桶透支；文档应区分稳态目标、突发上限和实际改动点数，后续验证热点与公平性。本轮未运行压测。
- demo 的权限扩展点、基础版本升级时覆盖数据的处理政策应由实际游戏接入明确；不把已有选择误报成通用框架 bug。
- 本轮地形验证不关闭 [首发 L01–L15 清单](LAUNCH-BACKLOG-3000-20261108.md) 中真实部署、恢复、全能力联合运行和目标容量等缺口。

## Linux 验证与复现

证据目录：[evidence/terrain-review-20261108](evidence/terrain-review-20261108/)。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| terrain、entitysync 及 policy、selfsync，race 一轮 | 4 包通过 | [core.log](evidence/terrain-review-20261108/core.log) |
| 正式生成工程 game、TerrainChunk、player controller 的 Terrain/Scene 测试，race 一轮 | 3 包通过 | [demo.log](evidence/terrain-review-20261108/demo.log) |
| C# 地形 golden、查询、包校验、并发覆盖、网格、DAO 增量 | 通过 | [dotnet.log](evidence/terrain-review-20261108/dotnet.log) |
| 正式生成工程全包只编译 | 通过，不等于全包测试 | [compile.log](evidence/terrain-review-20261108/compile.log) |
| 两个缺陷探针及窗口内控制组，各三轮 | 两项各失败三轮，控制组通过三轮 | [replay-controls.log](evidence/terrain-review-20261108/replay-controls.log) |

生成工程测试在加入红色探针之前执行；非 verbose 的包通过不声明所有外部依赖门控用例均执行。本轮不包含真实 Unity Player、真实 Mongo 崩溃恢复、全仓完整回归或压力测试。

在基线 main 工作树运行（需 Go/.NET 环境），生成到一个不存在的临时目录：

```bash
go test -race ./gameplay/terrain ./framework/sync/entitysync/... ./wiring/selfsync -count=1
dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec
go run ./codegen/cmd/roost project new planet -module example.com/planet -out /tmp/terrain-review-planet -template game-demo --skip-deps
```

进入生成目录，通过 go mod edit 将 github.com/tjbdwanghaibo/roost-core replace 到上述基线工作树，然后运行：

```bash
go test -mod=mod -race ./internal/service/game ./game/entities/terrain_chunk ./game/controllers/player -run 'Terrain|Scene' -count=1
go test -mod=mod ./... -run '^$'
```

最后把 [探针源码](evidence/terrain-review-20261108/review_terrain_replay_test.go.txt) 复制为生成工程的 internal/service/game/review_terrain_replay_test.go，运行：

```bash
go test -mod=mod ./internal/service/game -run '^TestReviewTerrain' -count=3 -v
```

基线应出现上述两个失败及一个通过；这是记录缺陷的红色探针，不把它加入生产测试树。
