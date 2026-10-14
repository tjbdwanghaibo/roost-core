# 玩家接入、运行服务与同步命名后续事项

记录日期：2026-10-11；源码基线：`8a42926f`。本篇记录当前职责和后续建议。Session 服务调整暂缓实施（维护者 2026-10-14 决定继续暂缓）；玩家接入目录已于 2026-10-14 由 `game/player_agent` 改名为 `game/playeraccess`（c163e977）；Sync 命名已按下文实施，随 v1.26.0 发布。玩家协议业务示例见 [接入教程](../framework/PLAYER-ACCESS.md)。

## 1. Session 运行服务：保留能力，后续评估可选化

`service/session` 的业务对象是 `Run`，表示一次副本、试炼等有期限的运行，不是玩家网络连接。它通过 Enter/Attach/Finish/Leave/Get/Current 管理状态、RequestID 幂等、owner 独占和外部资源引用。默认 Wiring 使用 Redis 保存相关记录，业务必须提供资源释放实现。

当前用途见 [Session 接口](../../service/session/session_rpc.go)、[运行实现](../../service/session/service.go) 与 [接线](../../wiring/session/session_mod.go)。资源释放是至少一次尝试，业务必须幂等；定期过期回收还需要运行周期和 owner 扫描接线，不能仅创建 Service 就认为资源会自动清理。

**后续方向：作为可选能力，不要求所有游戏独立部署 Session 服务。** 若一个副本的全部状态与资源都由单个 Game 内的 Room/Entity 持有，优先让该对象承担运行生命周期，避免两套权威状态。确有跨进程资源分配、Game 重启后继续清理的需求，再考虑独立服务。Go 中的 Service 类型不意味着必须单独进程部署。

后续实施需要逐项处理：

1. 盘点实际消费者，尤其 demo 的进入和结束副本流程，不直接删包。
2. 明确权威对象：运行状态、幂等、超时和资源释放各由谁负责；迁出服务的能力不能丢失。
3. 调整 demo/生成器默认接线，保留显式启用和远程部署能力；名称优先考虑 `run`，具体包名届时确定。
4. 检查结算链路：demo 目前先 Finish Run，再通过 Nest 提交奖励，两者不是同一事务。改变部署位置不会自动解决恢复与防重复。
5. 验证重复进入、重复结算、期限到达、异常退出、释放失败重试和重启后的遗留资源处理。

状态：**已登记，维护者 2026-10-14 决定继续暂缓（8a）。** 不把建议记录当作已完成重构。

## 2. Game 已有玩家接入层（目录改名已实施）

独立 Gate 的主要请求路径是：

```text
客户端 RS 协议包
  → Gate（连接、认证、拆包）
  → NATS → GameIngress（绑定、准入、回包通道）
  → playeraccess.ProtocolRegistry（类型校验、解码、中间件、请求上下文）
  → Controller.HandleXxx（接入业务编排）
  → 生成的 Nest Sender → Nest handler → Player/其他 Entity
```

嵌入式部署由 Game 本地 TCP 接入，之后共用生成的协议派发与 Controller。并非所有控制协议都必须进入 Entity handler：需要访问可变 Entity 状态时才通过 Nest，不能在 Controller 中直接取得 Entity 锁或并发修改 DAO。

当前有两个职责层：

| 层 | 当前实现 | 负责什么 |
| --- | --- | --- |
| 会话与传输接入 | `gateway.GameIngress`、`gateway.Session` | 玩家身份与连接绑定、准入、发送、激活与关闭通知 |
| 协议与应用接入 | 生成的 `game/playeraccess`、业务 Controller | 解码和编码、请求上下文、中间件、把协议请求转换成业务调用 |

生成的 `playeraccess.Context` 包含 BaseCtx、Session、PlayerID、MsgID、Seq。这里的 Session 是 `gateway.Session`，与上一节的运行服务无关。ProtocolRegistry 根据 Session 的可信 Principal 提取 PlayerID，检查 PayloadKind，再调用 Controller 并编码结果。

`playeraccess` 是生成工程的目录，不是 core 中现成同名目录；模板源在 [render_access.go](../../codegen/internal/roost/render_access.go)，协议绑定生成器在 [gen.go](../../codegen/internal/protocol/gen.go)，Game 接入实现在 [ingress.go](../../infra/network/gateway/ingress.go)。它也不意味着每个玩家都有一个 actor、一个 goroutine 或另一套实体执行锁。

后续建议：继续复用现有层，无需为处理玩家连接再建立一个名叫 session 的独立服务。

状态：**已实施（c163e977，维护者 2026-10-14 批准，8b）。** 生成目录由 `game/player_agent` 改为 `game/playeraccess`，包名、生成器常量 `protocol.PlayerAccessImportSuffix`、协议绑定、Controller 骨架、access 模板、demo 模板与文档同步；只改名，类型、方法与派发行为不变。已有工程按 CHANGELOG `[Unreleased]` 手工 `git mv game/player_agent game/playeraccess` 并改业务 import 后 `roost project sync`：sync 只写新目录、不删除旧目录，也不改写业务文件。

## 3. Sync 命名：已实施，随 v1.26.0 发布

本次以 c04b7629 为改动基线，保持原包目录和业务行为，按实际语义完成以下源码 API 迁移：

| 旧名称（升级查找用） | 实际用途 | 当前名称 |
| --- | --- | --- |
| `frame.SnapshotMeta.RoomID` | 区分同步流；Entity Sync 填入固定 `wireStream=1`，不是地图或副本 ID | `StreamID` |
| 客户端对应的 `RoomId` | 读取相同帧头字段 | `StreamId` |
| `lockstep.Room` / `RoomConfig` / `NewRoom` | 一场对局的帧序列、玩家席位、观战、历史追帧与哈希分歧检测 | `lockstep.Match` / `MatchConfig` / `NewMatch` |
| AOICluster 内部的 `rooms`、room 变量和注释 | 同一个世界坐标平面中的区域 | `areas`、`area`；保留已有 `AreaID`、`AddArea` |

证据：[Entity Sync 流说明](../../framework/sync/entitysync/wire.go)、[帧元数据](../../framework/sync/frame/frame_limits.go)、[Lockstep 对局协调](../../framework/sync/lockstep/match.go)、[AOI 区域组合](../../framework/sync/entitysync/policy/aoi_cluster.go)。

`StreamID` 只表示所在连接/同步上下文内的流身份；本建议不要求它全局唯一，也不把它改为 PlayerID 或 GameID。`Epoch` 表示流的代际，`Tick` 表示帧进度，三者职责继续分开。Lockstep 的 Match 表示对局执行对象，`service/match` 仍表示匹配服务，文档需要明确“匹配”和“对局”的区别。业务确实有大厅房间时，可以继续使用业务 Room，避免全仓机械替换。

迁移还包括 `ErrRoomConfigInvalid → ErrMatchConfigInvalid`、`ErrRoomClosed → ErrMatchClosed`；Lockstep 实现文件及两份测试文件从 `room*.go` 改为 `match*.go`。构造参数、方法行为、锁与生命周期均不变，指标名称保持不变。错误文本使用 match/area，业务判断应使用 errors.Is，不匹配完整字符串。AOICluster 的拓扑和边界镜像不变，仍是同一平面的区域，不代表跨进程分片已经实现。

这是源码 API 变更，不保留旧别名：Go 消费者更新字段、类型、构造函数和错误名，C# 更新 StreamId，再重新生成、编译业务工程。Demo 模板、生成器消费测试、客户端和现行文档已同步。历史 CHANGELOG、旧迁移诊断表保留旧名称，业务大厅/聊天等真正的 Room 不受影响。

帧头数值宽度、位置、端序及版本不变；Entity Sync 仍写入流常量 1。C7/C8 和 RS 格式不变，不修改协议金样以适配改名。升级不新增 WAL/schema/客户端自动迁移能力。若需回退，应成套恢复 core、生成工程与客户端源码，不能混用新旧源码 API。

状态：**已完成代码迁移，随 v1.26.0 发布；本地验证结果见下一节。**

## 4. 核对与验证范围

上一轮为静态职责核对，本轮新增命名实施和本地回归。使用 codebase-memory 的 roost-core 项目（coverage generation `2026-10-10T16:08:17Z`）；相关路径报告 metadata_changed，因此以当前源码及模板补证，不能用图谱未找到某调用证明没有调用方。本轮以隔离 worktree 的源码、全仓旧符号搜索及现有测试补证；图谱不覆盖新 worktree，不能据此声称索引已经更新。

2026-10-11 本机 Windows 验证，全部 Go 命令使用 `GOWORK=off`：

| 检查 | 结果与边界 |
| --- | --- |
| `go build ./...`、`go vet ./...` | 通过；build 曾打印模块 stat cache 写入被拒警告，但最终退出码为 0 |
| `go test ./framework/sync/... ./client/wire ./wiring/gate -count=1 -timeout=180s` | 正常本机权限下全部通过，包含 Lockstep 端到端和帧金样 |
| `go test ./codegen/internal/roost -run '^(TestDemoTemplateGeneratesABuildableWritePath\|TestClientSDKAgainstGeneratedPlayerTCP)$' -count=1 -v -timeout=10m` | 通过，232.832s；正式生成 game-demo，执行 build/vet、重生成无漂移以及 TCP/Game 的 race 测试 |
| 上述 SDK 用例内的 C# 验证 | .NET 构建与原金样消费通过；真实 TCP 鉴权、PB、并发序号、Sync、Lockstep input/hash/gap/catchup 通过；Unity 适配源码测试通过，不等于 Unity 引擎实机验收 |
| `go test . -run '^TestTrackedMarkdownRelativeLinksResolve$' -count=1 -timeout=45s`、`git diff --cached --check` | 通过 |

首次沙箱运行的生成工程测试被临时文件原子重命名权限拒绝；Sync 全量测试中的文件日志测试同样报 Access is denied，Lockstep 网络端到端在 120s 超时。相同代码在正常本机权限下复跑，全部 Sync 包用时约 0.6～5.9s，通过上述验证；没有删除或放宽断言。本次不宣称新性能收益、跨机故障验证或所有客户端引擎验收。
