# P2-④：dataengine 实现并入 core/dataengine/engine

批次：P2-④（收敛方案 §4；统一方案 M-04）
状态：完成（core 侧）；kit 侧旧路径保留到 P3
目标与非目标：Data Engine 的运行时（projector、outbox store / worker、Mongo 存储与装载、实体仓库、删除准入、迁移执行器）带历史并入 core；Mod 留 kit；不改行为、不改文档结构
前置批次：P2-①（nestwal、mongotest）、P2-②（core/mongo、core/nats 客户端）

## 与方案的偏差：落在实现子包，不是同名包

D2 说"同名包契约与实现合并"，前提是不成环。dataengine 是**第一个成环的**：core/nest 与 core/nestwal 都 import core/dataengine（契约），而实现 import core/nest 与 core/nestwal。合入 `core/dataengine` 会得到 `nest → dataengine → nest`、`nestwal → dataengine → nestwal`。按方案 §0 D2 的备注（"已有 Core 同名包先检查循环依赖，必要时使用实现子包"），实现落到 **`core/dataengine/engine`（包名 `engine`）**，契约包不动。映射表已改；`roost upgrade --consolidate` 对 `roost-kit/dataengine` 的非 Mod 符号改写到 `roost-core/dataengine/engine`。

顺带修正了 P0 的成环检查脚本：zsh 里 `for c in $imps` 不分词导致首轮全部误报"无环"（账本已有同类坑）。重跑结果：dataengine 成环；**nest、lock 的 kit 包只有 Mod 胶水**（`nest_mod.go`、`lock_mod.go` 及其测试），没有可下沉的实现，映射表改为 keep；其余同名包无环。

## 文件清单

- 搬入：`merge_kit_pkg.py dataengine --target dataengine/engine --package engine`，文件的 `package dataengine` 改为 `package engine`，对契约包的 `coredata` 别名保留（现在是另一个包）；`roost-kit/nestwal` → `roost-core/nestwal`、`roost-kit/mongo/mongotest` → `roost-core/mongo/mongotest`。
- 新增 `engine/health.go`：`HealthMessage(...)`，从 kit `mod.go` 的 `dataEngineHealthMessage` 提出并导出——它只依赖引擎统计，U-0037 的测试需要它。P3 时 kit Mod 改为调用它。
- **留在 kit**：`mod.go`、`mod_test.go`、`fatal_fence_test.go`（用 mods）、`real_fixture_integration_test.go`（用 Mongo / NATS Mod 建真实夹具）以及依赖该夹具的 `real_integration_test.go`、`failover_integration_test.go`、`toxic_integration_test.go`。这四个真实环境集成测试是故障矩阵的一部分，P3 时要么用 core 客户端重写夹具搬回 core，要么留在 kit 作为 Mod 级集成——二选一，记入 P3 清单。
- `mod.go` 里还残留实现片段（`jetStreamOutboxPublisher`、健康摘要），P3 瘦身时提到 core。

## 公共 API 变化

- `roost-kit/dataengine` 的实现符号 → `roost-core/dataengine/engine`（`NewEntityRepository`、`Projector*`、`OutboxWorker*`、`MongoStore*`、`MigrationRunner`、`ErrEntityAggregate*`、`ErrMigration*` 等）。
- 新增导出 `engine.HealthMessage`。
- Mod（`NewMod`、`ModOption`、`WithEntityAccess`、`WithRemoteProjection`）仍在 kit。

## 不变量、持久化 / wire 格式

未触碰 Mongo 文档、outbox 记录、WAL 记录、JetStream 主题。

## 关联历史 U / B / T

U-0037（outbox 健康）、U-0078 / U-0101（装载校验）、U-0044 系列随文件搬入并通过；MigrationRunner 三次冲突耗尽的替身测试仍待做（账本第 4 节）。

## 命令与结果

`go build ./...` 0；`go vet ./dataengine/...` 0；`go vet -tags integration ./dataengine/engine/` 0；`go test -count=1 ./dataengine/... .` ok（contract、engine、根模块边界测试）。

## 未验证项 / 风险

- 真实 Mongo / NATS 集成与故障矩阵在 core 侧无覆盖（见文件清单），P3 决定归属。
- 包名 `engine` 与业务工程里可能存在的局部变量 / 包名冲突由升级器加别名处理（P4 golden 用例覆盖）。

## 回退范围

`git revert` 本批三个提交（脚本、subtree add、合入）。

## 下一步唯一动作

P2-⑤：saga（无环，合入 core/saga 同名包）。
