# P2-①：mongo/mongotest 与 nestwal 并入 core

批次：P2-① / 收敛方案 §4（对应统一方案 M-03 的前置）
状态：完成（core 侧）；kit 侧旧路径保留到 P3
目标与非目标：把 roost-kit 的 `mongo/mongotest`（严格 Mongo 替身）与 `nestwal`（Nest WAL 实现）带历史搬入 roost-core；不改行为、不改文件格式；kit 仍保留自己的拷贝并继续在旧路径上构建
负责人、工作树：本机，`roost-core` 分支 `consolidation`
前置批次：P0（分支、映射表）
仓库 HEAD：core `consolidation` 自 `cc360c1` 起；kit `consolidation` `caa3682`（未改动，只建了 `split/mongotest`、`split/nestwal` 两个历史分支）

## 文件清单

- 搬入：`git subtree split` 在 kit 上切出两目录的历史（nestwal 20 个提交），`git subtree add` 并入 core（两个合并提交，历史可 `git log --follow`）。
- 改动：`nestwal/effect_inbox_test.go` 与 `mongo/mongotest` 内的 import `roost-kit/mongo/mongotest` → `roost-core/mongo/mongotest`（唯一一处跨包引用）。
- **删除**：`nestwal/backlog_integration_test.go`（`integration` 标签）。它是 kit/dataengine + kit/mongo + nestwal 三者的真实 Mongo 积压回放集成测试，import 了 kit 的 dataengine 与 mongo 适配器，属于 dataengine 批次；留在 kit 原位，等 P2-④ dataengine 并入 core 后再重新落到 `core/dataengine`。此前 `go mod tidy` 曾因它把 roost-kit 拉进 core go.mod，已还原。

## 公共 API 变化

无。包名不变（`nestwal`、`mongotest`），导出符号不变；只是导入路径由 `roost-kit/...` 变为 `roost-core/...`。

## 不变量、持久化 / wire 格式

未触碰 WAL 记录格式、CRC、checkpoint、effect inbox 文档结构。

## 关联历史 U / B / T

U-0013（replayPass 串行化）、U-0027（接纳计数关闭）、U-0048 / U-0053（WAL 守卫）、U-0084（mongotest 契约）随文件搬入，在新路径上全部通过；不算新审计，矩阵格子不改。

## 命令与结果（本机 macOS，go 1.27.0，GOWORK=/Users/whb/roost/go.work）

| 命令 | 结果 |
| --- | --- |
| `go mod tidy` | go.mod 只变 1 行（bson 依赖已存在；无 roost-kit） |
| `go build ./...` | 0 |
| `go vet ./mongo/... ./nestwal/` | 0 |
| `go vet -tags integration ./nestwal/` | 0 |
| `go test -count=1 ./mongo/... ./nestwal/ .` | mongotest ok、nestwal ok、根模块（含 `TestCoreDependencyBoundary`）ok |

kit 在旧路径上未改动，仍绿（P0 CI）。

## 未验证项 / 风险

- 全仓 `go test ./...` 与 `-race` 由 `consolidation` 分支 CI 跑。
- `backlog_integration_test.go` 的再落位依赖 P2-④。
- kit 与 core 在 P2 期间同时持有 nestwal 拷贝：**只改 core 侧**，kit 侧任何修复都要同步到 core（P2 期间不预期发生）。

## 回退范围

`git revert` core 上的三个提交（两个 subtree add + 一个修正提交）；kit 无改动。

## 下一步唯一动作

P2-②：`nats` / `redis` / `mongo` / `etcd` 客户端实现并入 core 同名契约包（第一次触碰 15 个同名包合并与 Core 新增驱动依赖）。
