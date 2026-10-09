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

前置运行实现与编码提交 `634ae6ce`；独立 Gate 实施中。完整范围按 [Gate P1～P5](../framework/GATEWAY-IMPLEMENTATION.md)。已完成部分如下，不能据此标记 P2～P5 已通过：

| 检查 | 实际结果 |
| --- | --- |
| 共用 TCP | 网络、完整帧、期限、连接失败和停止重试回归通过 race；Runtime 在监听前也只能归属一个 Server |
| 生成消费者 | 真实生成工程 build/vet、停止契约通过；C# 真实 TCP 的 PB、Sync、Lockstep 与错误类别拒绝通过 |
| MessagePack | 默认 Bus / RPC / 异步信封已迁为 RM v1，RPC 信封 v2；无 JSON 双读；自定义类型、int64、nil/empty、时间、尺寸/深度/尾值拒绝及并发测试通过 |
| 真实 NATS | 私有 loopback Core RPC 成功/业务错误/异步消息与 JetStream ClientMod/超过 AckWait 不重执通过；不用共享业务资源 |
| 原始 NATS | 单次传输、context 取消、实际 MaxPayload、条目/字节 pending 上限、慢消费者通知通过；Drain 超时与 Close 后仍等待真实回调通过 race |
| 全仓 | `go test ./...`、build、vet 通过；Linux amd64 交叉构建通过，未执行 Linux 测试 |
| 索引 | CBM 已刷新到 `2026-10-09T08:17:42Z`；Gate 材料路径 coverage 已核对 |
| 配置 | 原始能力以 nats.raw 发布；Inbox 前缀与 reconnect 缓冲可配置，声明与生成 schema 回归通过 |

TCP 提取发现并修复两项具体问题：Notify 的 typed nil 在返回 any 时必须转成 nil，避免错误关闭真实 Lockstep 连接（C# 消费用例先失败后通过）；恶意日志 writer 的测试清理同时恢复 slog 与标准 log，避免污染后续网络测试。TCP 运行逻辑已按 config/frame/runtime/session 分文件，仍在同一个 gateway 包；生成器仅保留配置、协议和启停接线。

编码短基准为 Apple M5、Go 1.27.0、macOS arm64，同一代表类型含 int64/enum/raw bytes/map/time/嵌套结构。MessagePack 完整内部包 105B，JSON 194B；三轮约 encode 443～446ns/op、decode 793～841ns/op，JSON encode 519～538ns/op、decode 997～1008ns/op。MessagePack 分配为 536B/9 次与 962B/16 次；JSON 为 504B/5 次与 624B/9 次。编码更小更快不代表分配更少，也不是 Gate 吞吐。补录字节数一轮时延有抖动，原始日志保留，不混成严格前后对比。

本机原始日志 `/private/tmp/roost-tcp-*.log`、`roost-msgpack-*.log`、`roost-gate-raw-*.log`。未运行的独立 Gate 绑定/广播/Sync/Lockstep 集群、故障、1h 性能、跨机和 Linux 实机阶段继续待验收。
