# activity_world：全服 / 服务器组活动宿主（独立服务里的权威实体，共用 activity 的生命周期）

## 能力

`activity` 模块的引擎默认只挂在 `Player` 上。本模块增加一种新宿主：`ActivityWorld` 实体，代表一个逻辑区服或服务器组，由独立的 `activity_world` 服务进程单独写入。多个 game 进程查询同一个逻辑号，看到的是同一条活动时间线。活动的阶段、循环、异步完成规则全部复用 `activity`，见 [activity](../activity/README.md)。

- **客户端协议**：无。本模块不给玩家开任何协议。
- **服务端入口**
  - 服务间 RPC 接口 `WorldActivities`（`internal/rpc/activity_world/activity_world.go`，能力名 `rpc.activity_world`，按目标实体路由到拥有它的实例）：
    - `Pulse(ctx, target int64, event string, createdAt int64) (views []Snapshot, err error)`：对账并推进，返回全部活动快照。`event` 为空是查询与对账；非空时必须来自已验证的业务事实。
    - `Complete(ctx, target int64, token Token) (err error)`：确认某一期的异步初始化或结算已经完成。
    - `target` 是 `ActivityWorld` 的完整实体号（逻辑号加实体种类 `EntityKindActivityWorld`）。`Snapshot`、`Token` 是 `activity` 包同名类型的别名。
  - 服务实现 `Service`（`internal/rpc/activity_world/service.go`）：`CheckRoute`、`Pulse`、`Complete`。每次调用先核对目标种类与 Remote 所有权，再进 Nest。
  - 组件：`WorldActivityComp()`，类型 `WorldActivityComponent` 是 `activity.Component` 的别名（`game/entities/activity_world/activity_component.go`），窄接口 `IWorldActivityEntity`。在 `ActivityWorld` 的 Nest 事务里用法与 `ActivityComp()` 相同（`Reconcile`、`Host()`、`Rearm()`）。
  - Nest handler（`game/handler/`，都是 `durability=strict`）：`handlerWorldActivityInit`（记下创建时间并首次对账）、`handlerWorldActivityPulse`、`handlerWorldActivityComplete`。
- **登记点 / 钩子**：本模块不新增登记函数。活动逻辑仍用 `activity.Register` 登记，注意要登记在 **`activity_world` 进程**会加载的代码里；`Hooks` 回调拿到的 `Owner` 是 `*ActivityWorld`。
- **状态**
  - 新实体 `ActivityWorld`（`game/entities/activity_world/entity.go`）：Remote 托管实体，归 `activity_world` 服务，启动时预加载。它的唯一号是逻辑区服 / 服务器组号，不是进程 SID。
  - `ActivityWorldDao`（集合 `activity_world`，`db/def/activity_world.go`）：`CreatedAt`（持久的开服时间，毫秒）与 `ActivityWorld`（类型 `ActivityBook`，活动权威状态）。都是 `persist,nosync`，不下发给任何客户端。
- **配置**
  - `configs/schema/activity_scope.go` 与 `configs/table/activity_scope.csv`（表 `activity_scope`，默认为空）：每行 `ID`（逻辑号）、`BootstrapSID`（由哪个 `activity_world` 实例首次建档）、`CreatedAt`（真实开服时间）。
  - 活动定义仍在 `activity` 的 `configs/table/activity.csv` 里，用 `Scope=world`。
  - 新增业务服务 `activity_world`（`internal/service/activity_world/service.go`），带 `configdata`、`dataengine`、`etcd`、`mongo`、`nats`、`nest`、`redis`、`remote_entity` 这些 Mod。
- **错误码**：`activity_world_request_invalid`（`ErrRequestInvalid`，RPC 请求无法解码或不合法时由生成的 RPC 代码使用）。引擎自身的拒绝仍是 `activity` 的包级错误值。

**日志**：显式事件触发与 Complete 确认记 Debug，记录宿主、scope、活动号、代数、期次与阶段；空事件恢复扫描不记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `internal/rpc/activity_world/service.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不给玩家提供查看全服活动的协议，也不向 game 推送阶段变化——game 要自己写端点，调 `Pulse(target, "", 0)` 取快照再转给客户端。不校验调用方身份与玩家资格：RPC 只面向可信服务器，面向玩家的端点要先校验区服与资格，不能把客户端自报的事件直接转发。不含任何具体活动内容。
- **推进不是准点的**：Remote 实体不能挂本地持久定时器，期限存在 `ActivityBook` 里，由 `activity_world` 进程的恢复扫描推进：每秒至多一页 128 个实体、8 个并行调用、每次调用预算 5 秒。扫描只推进当前归本实例所有的实体。业务操作仍按冻结的期限拒绝过期请求（`CheckOpen`），不依赖扫描的及时性。
- **数量上限**：沿用 `activity`——每个宿主 128 个定义号、每次推进至多 32 个动作。
- **一致性**：所有推进都在 `ActivityWorld` 实体自己的 Nest 事务里；单写者由 Remote 所有权保证，所有权换了之后旧实例的调用被路由检查与 Remote 准入拒绝。`Pulse` 的应答丢失后重发是安全的（对账是幂等的）；带事件的 `Pulse` 是否重复触发业务由登记的 `Eligible` / `Enter` 决定。
- **建档规则**：服务启动时，只有 `BootstrapSID` 等于本实例 SID 的行会被建档并初始化。创建时间一经写入不能改成别的值（`handlerWorldActivityInit` 发现不一致时拒绝）；创建时间不能晚于当前时间。迁移到别的实例后要同步改 `BootstrapSID`，不能换一个新的逻辑号重新开服。
- **前提**：依赖 `activity` 模块；需要一个独立部署的 `activity_world` 服务进程（Mongo、Redis、NATS、etcd）；调用方服务要声明 `uses_rpcs: [activity_world]`。排行、邮件等外部能力要配在 `activity_world` 服务上，不是 game 上。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“活动模块的驱动与验收边界”一节。

## 复用方式

- **需求正好是它**：`roost module add activity_world`，连带安装 `activity`，并新建 `activity_world` 服务。
- **做一个全服活动**
  1. 在 `configs/table/activity_scope.csv` 加一行逻辑号、首次承载的 SID 与开服时间。
  2. 在 `configs/table/activity.csv` 加 `Scope=world` 的定义，`roost generate`。
  3. 在 `activity_world` 进程加载的包里 `activity.Register` 登记逻辑。
  4. game 侧写自己的端点：校验玩家资格后，用生成的 RPC 客户端调 `Pulse` 取视图；玩家参加时在 `Player` 事务里调 `ActivityComp().Host().JoinShared(快照, now)` 记参与资格。
- **需求比它多一点**
  - 全服活动要调排行或邮件：给 `activity_world` 服务配置对应的 `uses`，在 `Hooks.Resume` 里做外部调用。
  - 不要改 `internal/rpc/activity_world/mod.go` 里“先建档、再注册 RPC、最后开扫描”的顺序。
- **别的模块 / 业务代码要用它**：取得 `WorldActivities` 客户端后调 `Pulse(ctx, target, event, createdAt)` / `Complete(ctx, target, token)`；`target` 用 `entity.BuildEntityID(逻辑号, EntityKindActivityWorld)` 构造。
- **只适合借鉴的情形**：想给别的共享实体（例如城市、赛区）挂活动时，照本模块的四样东西写一个新宿主即可：实现 `activity.Owner` 的实体、组件类型别名、三个 Nest handler、带 `StartDriver` 的 RPC Mod。

## 联动

无。本模块没有 `with/` 目录，别的模块也没有 `with/activity_world`。

## 验证范围

- `roost module verify activity_world`：4 个场景（`./internal/rpc/activity_world`，`^TestWorldActivity`）。
- 场景是离线的：Nest、生成的 DAO 与组件、handler、Remote 准入检查是真实代码；Remote 所有权与提交是内存替身。
- 场景装载自带的空 `activity` / `activity_scope` 表，不读工程的 `configs/data`：按“复用方式”加了自己的全服活动与逻辑号之后 `roost module verify` 照样通过（2026-11-14 换表核对：读工程表时“配置为空”“业务失败不留激活”两条变红）。
- 覆盖：一期活动在权威实体上走完并使用稳定的范围身份、所有权变更后旧驱动被拒、业务失败时不留下激活、配置为空时什么都不做。
- 没有验证：启动建档（`bootstrap`）与恢复扫描在真实进程里的行为、真实的 Mongo / WAL / NATS、跨进程迁移与长时间运行。
