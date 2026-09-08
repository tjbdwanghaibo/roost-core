# P2-②：nats / redis / mongo / etcd 客户端实现合入 core 同名契约包

批次：P2-②（收敛方案 §4；对应统一方案 M-08 的客户端部分，前移是因为 remoteentity / dataengine / saga 依赖它们）
状态：完成（core 侧）；kit 侧旧路径保留到 P3
目标与非目标：把四个 kit 客户端包的实现带历史合入 core 同名包（第一次执行 D2"契约与实现同包"）；Mod 胶水留 kit；不改行为
负责人、工作树：本机，`roost-core` 分支 `consolidation`
前置批次：P2-①（mongotest 已在 core）

## 做法（脚本化）

`scripts/consolidation/merge_kit_pkg.py <pkg> --alias <别名> --exclude <Mod 胶水> [--delete] [--replace]`：kit 上 `git subtree split` 切出目录历史 → core 上 `git subtree add` 到 `staging/<pkg>` → 逐文件 `git mv` 进目标包（文件名与 core 已有契约文件相同的加 `_impl` 后缀）→ 去掉对自身包的别名 import 并去前缀 → gofmt → `go mod tidy` → build / vet / vet -tags integration / 包测试 + 根模块边界测试。提交由人工检查后做。

| 包 | 别名 | 留在 kit 的文件 | 改名（clash → `_impl`） | 其他 |
| --- | --- | --- | --- | --- |
| nats | `fnats` | `nats_mod.go`、`nats_mod_test.go`、`jetstream_rpc_toxic_integration_test.go`（Mod 级 JetStream RPC 故障测试，依赖 kit/mods） | client、jetstream、rpc、rpc_test | 删 `permanent.go`（与 core `errors.go` 的 `Permanent` 重复，kit 版自称"滚动升级兼容形式"），`isPermanent(` → `IsPermanent(` |
| redis | `fredis`、`rediscore` | `redis_mod.go` | client、lock、pipeline、pubsub | — |
| mongo | `fmongo` | `mongo_mod.go` | client、collection、database、session | `mongotest` 子目录已在 P2-① 并入，此处丢弃重复 |
| etcd | `fetcd` | `etcd_mod.go` | client、discovery、election、local_mirror、watcher | — |

## 公共 API 变化

- 导入路径：`roost-kit/{nats,redis,mongo,etcd}` 的实现符号现在在 `roost-core/{nats,redis,mongo,etcd}`（`NewClient`、`Client`、`JetStream*`、锁、镜像、发现、选举等）。Mod（`NewNatsMod` 等）仍在 kit。
- `kit/nats.Permanent` 消失，统一用 `core/nats.Permanent`（结构相同，`errors.As` 标记接口一致，行为不变）。
- 无其他符号改名：四个包的导出与非导出顶层标识符与 core 同名包零冲突（迁移前核对）。

## 依赖变化（core go.mod）

新增直接依赖：nats.go、go-redis v9、etcd client/api v3（及其间接依赖）。这是收敛方案 §3.3 预告的代价。

## 不变量、持久化 / wire 格式

未触碰 Lua 脚本、JetStream 主题 / 消费者命名、etcd key 布局、Mongo 文档结构。

## 关联历史 U / B / T

U-0012 / U-0036 / U-0042 / U-0061 / U-0079 / U-0084 / U-0086 与 T-42 / T-43 涉及的测试随文件搬入并在新路径通过；不算新审计。

## 命令与结果（本机 macOS，go 1.27.0）

四包各自：`go build ./...` 0、`go vet ./<pkg>/` 0、`go vet -tags integration ./<pkg>/` 0、`go test -count=1 ./<pkg>/ .` 0（含 `TestCoreDependencyBoundary`）。
全仓：`go vet ./... && go run ./cmd/glsvet ./... && go test -count=1 ./...` 无 FAIL。

## 未验证项 / 风险

- `-race` 与 Windows 由分支 CI 跑。
- kit 在 P2 期间仍持有四个包的旧拷贝；修复只在 core 侧做。
- `_impl` 后缀文件名是本轮约定，P3 后可按需改回描述性名字（不影响 API）。

## 回退范围

`git revert` 本批的 8 个提交（4 个 subtree add + 4 个修正）。kit 无改动（只多了 `split/*` 本地分支）。

## 下一步唯一动作

P2-③：remoteentity（M-02 试点）。
