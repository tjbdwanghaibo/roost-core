# 2026-11-17 批次：最终全量与演练

范围：v1.52.0 之后 2026-11-17 这一批在 main 上的全部提交，未发版。包括：
- RV-20261116-01 / 02 修复；
- demo 组件迁移；
- R3 地形库范围；
- room `Authorize`；
- R9 13 类玩法模块组合；
- module verify 的两项修正；
- SLG 永久回执；
- client_export；
- 收尾文档。

CellWorld 进模块这一波不做（维护者 2026-11-17）。

## 全量 pretag

用的是去掉“发版清单版本号核对”的副本（不发版，`codegen/ci/framework-release.yaml` 仍写 v1.52.0），参数 `v1.53.0`，其余与 `scripts/pretag.sh` 相同。

| 轮 | HEAD | 结果 |
|---|---|---|
| 第 1 轮 | `e4bf3e52` | `go test ./...` 只有根包两条失败，后续步骤没跑。摘要见 [pretag-run1-summary.txt](pretag-run1-summary.txt) |
| 修复 | `f156aecd` | R3 新的真实用例 `TestTerrainMapOwnershipRealMongo` 补进 `test-demo-generated.sh` 与 `generatedRealDependencyEntries`；自走棋恢复夹具 gofmt |
| 第 2 轮 | `f156aecd` | 通过：`go test ./...`、13 类玩法与各生成脚本（模块矩阵 1233s）、C# SDK，最后 `ready to tag`。摘要见 [pretag-summary.txt](pretag-summary.txt) |

`f156aecd` 之后的提交只改大厅 / relay 方案文档（`a8eeb3d2`、`aad41d84`、`f0569aa7`），每次都单独跑过 `Link|Anchor|Doc`。

## 真实依赖演练

环境：`~/.roost-it/roost-dataengine-it/env.sh`（Mongo 副本集、NATS、Redis、私有 etcd）。

| 演练 | 命令 | 结果 |
|---|---|---|
| demo 真实档（含 R3 的 `TestTerrainMapOwnershipRealMongo`） | `bash scripts/test-demo-generated.sh` | exit 0，25 条 PASS，地形 5 条全过。见 [demo-real.txt](demo-real.txt) |
| demo 换服六个 SIGKILL 点加 checkpoint（R3 在离开时多一次全服邮件合并） | `DEMO_REHOME_OUT=<目录> bash scripts/test-demo-rehome.sh` | accepted、quiesced、leaving、handed-off、committed、dropped、checkpoint 全部 PASS。committed 窗口的旧副本仍按 Q4 记录。见 [rehome.txt](rehome.txt) |
| Gate 自定义服务名（RV-20261116-02） | `DRILL_GATE_NAME=edge DRILL_PLAYER_SERVICE=world … gate-drill.sh` | PASS，两个进程都是 0 条 ERROR 日志。见 [gate-custom.txt](gate-custom.txt) |

## 没跑的

- CellWorld 与地形分区的多进程 REAL：R3 只改了分区地图的库范围（global 不变），`TestTerrainMapOwnershipRealMongo` 已经覆盖“分区地图的文档在共用库里”；CellWorld 进模块这一波不做。
- 性能测试：这一批没有动 Nest、DataEngine、Sync 的热路径。
- Unity Editor 与 Player 实跑：client_export 只在 .NET SDK 下按 netstandard2.1 编译过，见 [client-export 证据](../client-export-20261117/README.md)。
