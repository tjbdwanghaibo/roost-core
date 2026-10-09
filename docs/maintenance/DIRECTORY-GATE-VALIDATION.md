# 目录与 Gate 实施验收

日期：2026-10-09。实施工作树：`codex/gate-design-plan`，起始提交 `ef640e6e`。先目录后 Gate，两阶段分别验证；稳定基线历史性能不作为新增 Gate 性能证据。

## 目录阶段

Framework、Infra、Gameplay、Service、Wiring 分类已实施。十个领域的模型、存储、RPC 传输和周期运行统一到 Service；Wiring 保留配置、依赖、构造和生命周期委托。Ops 运行归 Infra，StatsLog 运行归 Framework。旧 kit 入口和领域 alias 已删除，生成器与调用方同步修改。

| 检查 | 实际结果 |
| --- | --- |
| 改前 `go test ./...` | 通过，保留原始日志 |
| 改后 `go test ./...` | 通过；包括正式生成工程编译、运行和配置检查 |
| `go build ./...`、`go vet ./...` | 通过，macOS arm64 / Go 1.27.0 |
| 两次 `go generate ./...` | 通过；第二次所有 Go/JSON 文件哈希零变化，无新增文件 |
| 服务、Ops、StatsLog、跨服务接线 race | 通过；真实资源的 integration 标签测试未由本命令执行 |
| 根目录文档链接和运行时依赖方向 | 通过；文档锚点检查 37 份文件、零断链；最终根目录链接回归另重验 |
| Linux amd64 | Linux amd64 交叉构建通过；未执行 Linux 测试 |
| CBM | 当前工作树项目 `roost-core-gate-design-plan` 已刷新，generation `2026-10-09T07:19:51Z`；新路径 coverage 已核对，五处模板解析缺口仍以源码补证 |

已有短基准（Apple M5、100ms/op 采样预算）通过：Nest 单请求约 3756ns/op；Sync 64 会话 × 32 subject × 单 profile 约 0.477ms/flush，256 会话约 1.788ms/flush；投影 segment planner 约 11～19μs/op。它们仅证明现有基准可运行，不是业务 TPS、前后性能对比或 Gate 验收。

本地原始证据保留在 `/private/tmp/roost-directory-*.log` 与被忽略的 `artifacts/reorganization/`；不把备份 Go 源文件混入构建或发布。

## Gate 阶段

尚未实施。完整范围按 [Gate P1～P5](../framework/GATEWAY-IMPLEMENTATION.md)：MessagePack/TCP 提取、PB 绑定与广播、Sync/Lockstep、故障与排空、性能。未运行的阶段不得标记通过；跨机和 Linux 实机验证与本机 loopback 分开记录。
