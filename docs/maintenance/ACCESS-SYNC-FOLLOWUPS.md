# 玩家接入、运行服务与同步命名后续事项

记录日期：2026-10-11；源码基线：`8a42926f`。本篇记录当前职责和后续建议。Session 服务调整暂缓实施；接入层与 Sync 命名是建议，尚未改动代码、生成物、部署或协议。

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

状态：**已登记，按维护者要求后续处理。** 不把建议记录当作已完成重构。

## 2. Game 已有玩家接入层

独立 Gate 的主要请求路径是：

```text
客户端 RS 协议包
  → Gate（连接、认证、拆包）
  → NATS → GameIngress（绑定、准入、回包通道）
  → player_agent.ProtocolRegistry（类型校验、解码、中间件、请求上下文）
  → Controller.HandleXxx（接入业务编排）
  → 生成的 Nest Sender → Nest handler → Player/其他 Entity
```

嵌入式部署由 Game 本地 TCP 接入，之后共用生成的协议派发与 Controller。并非所有控制协议都必须进入 Entity handler：需要访问可变 Entity 状态时才通过 Nest，不能在 Controller 中直接取得 Entity 锁或并发修改 DAO。

当前有两个职责层：

| 层 | 当前实现 | 负责什么 |
| --- | --- | --- |
| 会话与传输接入 | `gateway.GameIngress`、`gateway.Session` | 玩家身份与连接绑定、准入、发送、激活与关闭通知 |
| 协议与应用接入 | 生成的 `game/player_agent`、业务 Controller | 解码和编码、请求上下文、中间件、把协议请求转换成业务调用 |

生成的 `player_agent.Context` 包含 BaseCtx、Session、PlayerID、MsgID、Seq。这里的 Session 是 `gateway.Session`，与上一节的运行服务无关。ProtocolRegistry 根据 Session 的可信 Principal 提取 PlayerID，检查 PayloadKind，再调用 Controller 并编码结果。

`player_agent` 是生成工程的目录，不是 core 中现成同名目录；模板源在 [render_access.go](../../codegen/internal/roost/render_access.go)，协议绑定生成器在 [gen.go](../../codegen/internal/protocol/gen.go)，Game 接入实现在 [ingress.go](../../infra/network/gateway/ingress.go)。它也不意味着每个玩家都有一个 actor、一个 goroutine 或另一套实体执行锁。

后续建议：继续复用现有层。如果需要更直观的命名，可将生成目录改为 `game/playeraccess`，表达“玩家接入”；这是可选命名提案，届时同步修改生成器导入、绑定、模板和消费工程。无需为处理玩家连接再建立一个名叫 session 的独立服务。

## 3. Sync 的 room 按实际语义分别命名

最优先处理的是帧头的 RoomID，它已经与实际用途不符：

| 当前名称 | 当前实际用途 | 建议名称 |
| --- | --- | --- |
| `frame.SnapshotMeta.RoomID` | 区分同步流；Entity Sync 填入固定 `wireStream=1`，不是地图或副本 ID | `StreamID` |
| 客户端对应的 `RoomId` | 读取相同帧头字段 | `StreamId`，随服务端一起核对与迁移 |
| `lockstep.Room` / `RoomConfig` / `NewRoom` | 一场对局的帧序列、玩家席位、观战、历史追帧与哈希分歧检测 | `lockstep.Match` / `MatchConfig` / `NewMatch` |
| AOICluster 内部的 `rooms`、room 变量和注释 | 同一个世界坐标平面中的区域 | `areas`、`area`；沿用已有 `AreaID`、`AddArea` |

证据：[Entity Sync 流说明](../../framework/sync/entitysync/wire.go)、[帧元数据](../../framework/sync/frame/frame_limits.go)、[Lockstep 对局协调](../../framework/sync/lockstep/room.go)、[AOI 区域组合](../../framework/sync/entitysync/policy/aoi_cluster.go)。

`StreamID` 只表示所在连接/同步上下文内的流身份；本建议不要求它全局唯一，也不把它改为 PlayerID 或 GameID。`Epoch` 表示流的代际，`Tick` 表示帧进度，三者职责继续分开。Lockstep 的 Match 表示对局执行对象，`service/match` 仍表示匹配服务，文档需要明确“匹配”和“对局”的区别。业务确实有大厅房间时，可以继续使用业务 Room，避免全仓机械替换。

实施顺序建议：先改最有歧义的 StreamID，再清理 AOI 内部术语，最后改 Lockstep 对外 API。改名时同步检查 Go/C# 客户端、生成器、模板、文档、测试、日志和指标。二进制帧目前按固定顺序编码数值；单纯字段重命名可以保持字节布局，但必须以实际编码和跨语言样本验证，不能直接宣称没有协议影响。指标名称是否跟着改需要单独列出迁移项。

状态：**命名建议待选择和实施；本次只留档。**

## 4. 核对与验证范围

本轮为静态职责核对和文档记录，未运行网络、性能或故障测试。使用 codebase-memory 的 roost-core 项目（coverage generation `2026-10-10T16:08:17Z`）；相关路径报告 metadata_changed，因此以当前源码及模板补证，不能用图谱未找到某调用证明没有调用方。文档检查相对链接与 Git 空白错误，不修改运行时。
