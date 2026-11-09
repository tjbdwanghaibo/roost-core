# 地形默认参数首批实施验证（2026-11-09）

Linux / Go 1.27.2。基于 main f0ff1727 实施；正式 CLI 生成自定义 module example.com/planet，replace 指向本次 main 工作树。未运行 benchmark 或压力测试。图谱工具不可用，按精确源码检查。

## 执行与结果

| 检查 | 结果 |
| --- | --- |
| `go test ./codegen/internal/roost -run TestDemo -count=1` | 通过，含模板嵌入/生成步骤一致性 |
| `go test -race ./gameplay/terrain -count=1` | 通过 |
| 生成工程 `go test -mod=mod -race ./cmd/terrainpack ./internal/service/game ./game/entities/terrain_chunk ./game/controllers/player -run 'Terrain|Scene' -count=1 -v` | game、terrain_chunk、controller 三包通过；CLI 新增测试误把现有允许的 BaseRevision=0 当非法而失败。已删掉该错误假设，改测非法数字参数，保留此轮原始输出 |
| 修正后 `go test -mod=mod -race ./cmd/terrainpack -count=1 -v` | 通过，见 terrainpack.log；没有修改 BaseRevision 的运行时契约 |
| 生成工程 `go build -mod=mod ./...` | 因临时目录 VCS 状态获取失败而退出，原输出保留 |
| 生成工程 `go build -mod=mod -buildvcs=false ./...` | 通过；仅关闭临时生成目录的 VCS stamping |
| 生成工程 `go vet -mod=mod ./...` | 通过 |
| 文档相对链接和文档引用提交检查 | 通过；最终提交前再次检查 |

配额三个真实生成 Nest 用例覆盖 270 桶、按字段计点、拒绝不建实体/不写入、满桶透支后补充边界、重复及权限拒绝最小计费。CLI 测试覆盖实际写包再读包、默认 −10000cm、显式覆盖、负高度查询、独立水位，以及收窄 int32 之前的范围拒绝。

预期 SKIP 共四项：三项 TerrainMeasure 测量门、TestTerrainRestartRealMongo 外部依赖门；本次没有跑真实 Mongo/WAL 恢复、Unity、跨机或目标容量验收。未把 C# 同算法一致性记为本轮执行通过；本批不改变查询算法或包格式。

## 复跑

在当前 core 检出运行：

```bash
go run ./codegen/cmd/roost project new planet -module example.com/planet -out /tmp/terrain-defaults-planet -template game-demo --skip-deps
```

进入生成目录，用 `go mod edit -replace github.com/tjbdwanghaibo/roost-core=/absolute/path/to/core` 指向本次检出，再执行表中生成工程命令。最终代码的 CLI 测试应全部通过；无需复造已修正的错误测试假设。
