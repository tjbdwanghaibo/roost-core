# activity_alliance：联盟活动宿主（挂在已有联盟实体上，共用 activity 的生命周期）

## 能力

把 `activity` 模块的活动引擎挂到 `alliance` 模块已有的 `Alliance` 实体上，让一个联盟拥有自己的活动时间线。推进发生在联盟服务进程里、联盟实体自己的事务中。活动的阶段、循环、异步完成规则全部复用 `activity`，见 [activity](../activity/README.md)。

- **客户端协议**：无。
- **服务端入口**
  - 服务间 RPC 接口 `AllianceActivities`（`internal/rpc/activity_alliance/activity_alliance.go`，能力名 `rpc.activity_alliance`，按目标联盟路由到拥有它的联盟服务实例）：
    - `Pulse(ctx, target int64, event string, createdAt int64) (views []Snapshot, err error)`：对账并推进，返回这个联盟的全部活动快照。`event` 为空是查询与对账；非空时必须来自已验证的业务事实。
    - `Complete(ctx, target int64, token Token) (err error)`：确认某一期的异步初始化或结算已经完成。
    - `target` 是联盟的完整实体号。`Snapshot`、`Token` 是 `activity` 包同名类型的别名。
  - 服务实现 `Service`（`internal/rpc/activity_alliance/service.go`）：`CheckRoute`、`Pulse`、`Complete`，先核对目标种类与 Remote 所有权再进 Nest。
  - 组件：`AllianceActivityComp()`，类型 `AllianceActivityComponent` 是 `activity.Component` 的别名（`game/entities/alliance/activity_component.go`），窄接口 `IAllianceActivityEntity`。在联盟实体的 Nest 事务里调 `AllianceActivityComp().Reconcile(event, createdAt)`、`Host()`、`Rearm()`。
  - Nest handler（`game/handler/`，都是 `durability=strict`）：`handlerAllianceActivityPulse`、`handlerAllianceActivityComplete`。
- **登记点 / 钩子**：本模块不新增登记函数。活动逻辑仍用 `activity.Register` 登记，要登记在**联盟服务进程**会加载的代码里；`Hooks` 回调拿到的 `Owner` 是 `*Alliance`，`Eligible` 读的是权威联盟实体的真实状态。
- **状态**
  - `AllianceDao.ActivityAlliance`（类型 `ActivityBook`）：追加到联盟主 DAO 上的活动权威状态，`persist,nosync`。不进入 game 侧的联盟只读镜像。
- **配置**
  - 没有自己的配置表。活动定义在 `activity` 的 `configs/table/activity.csv` 里，用 `Scope=alliance`。
  - 安装时给已有的 `alliance` 服务追加 `configdata` Mod 与 `activity_alliance` RPC；只补缺少的配置段，不覆盖联盟服务已有的文件与配置。
- **错误码**：`activity_alliance_request_invalid`（`ErrRequestInvalid`，RPC 请求无法解码或不合法时由生成的 RPC 代码使用）。引擎自身的拒绝仍是 `activity` 的包级错误值。

## 能力边界

- **不做的事**：不给玩家开协议，不向 game 或联盟成员推送阶段变化。不校验成员资格与操作权限：RPC 只面向可信服务器，game 的端点必须自己验证“这名玩家是不是这个联盟的成员、有没有资格”，不能把客户端声称的事件直接转发。不决定贡献归属、退盟后的领奖范围——这些是业务规则。
- **联盟没有自动提供创建时间**：`Alliance` 实体没有实现 `ActivityCreatedAt()`。`Trigger=created` 的定义只有在调用方把创建时间显式传给 `Reconcile` 或 `Pulse` 时才会激活；恢复扫描传的是 0，只会自动激活 `fixed` 触发的定义。
- **推进不是准点的**：Remote 实体不能挂本地持久定时器，期限存在 `ActivityBook` 里，由联盟服务进程里的恢复扫描推进：每秒至多一页 128 个联盟、8 个并行调用、每次调用预算 5 秒。联盟很多时一轮扫完的时间随联盟数增长。业务操作仍按冻结的期限拒绝过期请求（`CheckOpen`）。
- **数量上限**：沿用 `activity`——每个联盟 128 个定义号、每次推进至多 32 个动作。
- **一致性**：所有推进都在联盟实体自己的 Nest 事务里，与联盟的其他写入共用同一个单写者；所有权迁移后旧实例的调用被拒。入盟、退盟、成员所在 game 变动都不会重开或重置联盟的活动时间线。game 侧的联盟镜像不参与推进。
- **前提**：依赖 `activity` 与 `alliance` 两个模块；运行在已有的 `alliance` 服务进程里；调用方服务要声明 `uses_rpcs: [activity_alliance]`。排行、邮件等外部能力要配在 `alliance` 服务上。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“活动模块的驱动与验收边界”一节。

## 复用方式

- **需求正好是它**：`roost module add activity_alliance`，连带安装 `activity` 与 `alliance`（`alliance` 又带上 `bag`）。
- **做一个联盟活动**
  1. 在 `configs/table/activity.csv` 加 `Scope=alliance` 的定义，`roost generate`。
  2. 在联盟服务进程加载的包里 `activity.Register` 登记逻辑。
  3. 事件触发的活动（联盟建成、升级等）：在产生这个事实的联盟 Nest 事务里调 `AllianceActivityComp().Reconcile("<事件名>", createdAt)`。
  4. game 侧写自己的端点：校验成员资格后，用生成的 RPC 客户端调 `Pulse` 取视图；玩家参加时在 `Player` 事务里调 `ActivityComp().Host().JoinShared(快照, now)` 记参与资格。
- **需求比它多一点**：让 `created` 触发自动生效，可以在 `game/entities/alliance/activity_component.go` 里给 `Alliance` 加一个 `ActivityCreatedAt()` 方法返回持久的建盟时间（毫秒）。
- **别的模块 / 业务代码要用它**：取得 `AllianceActivities` 客户端后调 `Pulse(ctx, target, event, createdAt)` / `Complete(ctx, target, token)`；`target` 是联盟的完整实体号（`alliance` 模块的 `AllianceTarget(联盟号)` 可以构造）。
- **只适合借鉴的情形**：见 `activity_world` 的同名小节；本模块展示的是“给一个已有的 Remote 实体追加活动宿主”需要的最少文件。

## 联动

无。本模块没有 `with/` 目录，别的模块也没有 `with/activity_alliance`。

## 验证范围

- `roost module verify activity_alliance`：4 个场景（`./internal/rpc/activity_alliance`，`^TestAllianceActivity`）。
- 场景是离线的：Nest、生成的 DAO 与组件、handler、Remote 准入检查是真实代码；Remote 所有权与提交是内存替身。
- 覆盖：一期活动在权威联盟实体上走完并使用稳定的范围身份、所有权变更后旧驱动被拒、业务失败时不留下激活、配置为空时什么都不做。
- 没有验证：恢复扫描在真实联盟服务进程里的行为、与真实联盟写入并发时的表现、真实的 Mongo / WAL / NATS、跨进程迁移与长时间运行。
