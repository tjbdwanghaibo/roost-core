# scene：本进程内的场景——多地图与分线、进出与切换、视野（AOI）与公开字段、区域与交互、场景时间域

一个“场景”是一个 Scene 实体，带自己的成员账本、视野索引和时间域。玩家（以及怪物、建筑等别的实体）进入场景之后，才有“附近的人能看到我”这回事。玩家的移动本身由依赖的 actor 模块负责（`ActorMove` / `ActorStop`），scene 给 actor 提供“在哪张图上走、谁能看见”的环境。

## 能力

- **客户端协议**
  - `SceneChange`（`protocol/def/scene_change.go`）：请求 `SceneChangeRequest{FromID, ToID}`，应答 `SceneChangeResponse{Code, Reason, SceneID, X, Y}`。进入、离开或切换场景。`FromID` 是上一次应答里的场景号（第一次进场填 0），`ToID` 填 0 表示离场；两者相同表示重连（保留站位、重新订阅视野）。场景号是服务端开图时给的编号，客户端只能进已经开放的场景。
  - `SceneInteract`（`protocol/def/scene_interact.go`）：请求 `SceneInteractRequest{SceneID, ObjectID, Sequence}`，应答 `SceneInteractResponse{Code, Reason, Sequence, Text}`。与场景里一个静态交互对象交互一次；`Sequence` 是这名玩家自己的连续序号，从 1 起每次加 1。
  - 没有专门的推送协议：附近玩家的公开字段经框架的实体同步（entitysync）按视野下发；场景的时间域放在全场关系视图里，下发给这张图的所有成员，不需要靠近某一点。
- **服务端入口**
  - 场景管理器 `Manager`（`game/scene/service/runtime.go`），用 `service.FromRegistry(registry)` 取得（能力名 `gameplay.scene`）：
    - `Open(uniqueID int64, config gamescene.Config) (*sceneentity.Scene, error)`：开一张图，只给可信服务端用。
    - `Close(ctx, uniqueID) error`：关一张**空**图；还有成员就拒绝。
    - `Drain(ctx, id) error`：先关准入，再通知登记过的模块清走自己的成员，撤离玩家，最后关图；只动指定的这一张。失败可重试。
    - `Stop(ctx) error`：停机入口（模块的 `runtime` 接线自动调用）。先禁止再开图，再逐图 `Drain`。
    - `IDs(from, to int64) ([]int64, int64, error)`：把场景号换成实体号，供要把 Scene 放进自己事务的调用方声明参与者。
    - `RegisterDrainer(kind, run) (func(), error)`：见“登记点”。
  - 场景配置 `Config`（`game/scene/scene.go`）：`Template`、`Line`（分线号）、`Bounds`、`Spawn`、`BlockSize`、`Sight`（视距）、`Capacity`、`Ground`、`Surface`、`Regions`、`Objects`；`DefaultConfig()` 是缺省的一份。
  - 空间 `Space`（同文件，经 `Scene.Space()` 取得）：
    - `Sense(at, radius) []SensedActor`：返回同场景、半径内**已提交位置**的玩家，半径不能超过视距。不读别的实体的 DAO、不取别的实体的锁。
    - `Nearby(playerID) []int64`：这名玩家当前视野里的实体号。
    - `Enter(value, at)` / `Leave(value)` / `QueueMove(value, at)`：把一个实体放进 / 移出 / 移动这张图的视野索引（怪物、地形块、建筑等非玩家实体用 `Enter` 显示）。
    - `Placement(preferred)`：先试给定点，再试出生点，返回一个能站的位置。
    - `RegionsAt(at)`、`Object(id)`（`game/scene/regions.go`）：某点所在的区域号、按号取交互对象。
  - Scene 实体（`game/entities/scene/entity.go`）：`Admit(id)` / `Depart(id)`（成员账本，必须与对应的 actor 同时持锁）、`Environment()`（交给 actor 的环境）、`TimeDomain()`、`Config()`。
  - Nest handler（`game/handler/`）：`handlerSceneChange`（`rollback=undo durability=async`，Player 与至多两张 Scene 同一笔事务）、`handlerSceneInteract`（`durability=strict`）、`handlerSceneTime`（改场景时间倍率，只给可信管理逻辑：`sender.NewSceneTimeSender(client).Sync_SceneTime(ctx, sceneEntityID, rate)`）、`handlerSceneClose`、`handlerSceneBeginClosing`、`handlerSceneEvacuate`（后三个由 `Manager` 内部使用）。
- **登记点**
  - `OnSceneFields(name string, fields ...string)`（`game/entities/player/scene_sync.go`）：登记“附近的人能看到 Player 的哪些顶层字段”。只在 `init` 登记；没登记的字段默认只有本人能看。模块自己登记了 `Actor`（位置与朝向）。
  - `OnRegion(name, handler func(*Player, gamescene.RegionEvent) error)`（`game/entities/player/scene_hooks.go`）：玩家进入 / 离开一个静态区域时调用。区域由服务端按玩家已验证的位置计算，在位置变化的**同一笔 Nest 事务**里触发；返回错误会撤销这一拍的位置与区域列表。
  - `OnSceneInteract(name, handler func(*Player, gamescene.Object) error)`（同文件）：`SceneInteract` 的场景、距离与序号都通过之后触发一次，同一笔事务。
  - 以上两类回调只能改已经持锁的实体，不能发 RPC、不能加载或锁别的实体；只在 `init` 登记，空名、nil、重名 panic；按名字排序执行。
  - `Manager.RegisterDrainer(kind entity.EntityKind, run func(context.Context, *sceneentity.Scene) error)`：别的模块登记“关这张图之前怎么把我这一类实体清走”。回调在 Nest 与管理器锁之外执行，必须按传入的 Scene 实例清理、失败时保留可重试的状态。每种实体 kind 只能登记一个，不能给 Player 登记。monster 模块启动时登记了自己。
- **状态**
  - Scene 实体的 `SceneDao`（`db/def/scene.go`）：`Members`、`Closed`、`Closing` 不落库不同步；`AnchorBusiness`、`AnchorDomain`、`Rate`（时间域）不落库但同步给全场。整个 DAO 不落库（`nocoll`）：场景从开图配置重建，成员与时间域不从数据库恢复。
  - Player 上两个字段（`db/def/scene_player.go`）：`PlayerDao.SceneRuntime`（当前所在的区域号列表，不落库不同步）、`PlayerDao.SceneInteraction`（交互序号与最后一次应答，落库不同步）。
  - 谁能看到什么：本人视图包含 Player 所有可同步字段；附近视图只包含 `OnSceneFields` 登记过的顶层字段，快照与增量用同一个掩码。
- **配置**：没有配置表与配置键。启动时自动打开 1 号场景，用 `DefaultConfig()`：模板 `plain`、分线 1、范围 [0,1000)×[0,1000)、出生点 (50,50)、块大小 32、视距 64、容量 1000。别的图由工程在服务端代码里调 `Open`。
- **错误码**：无。拒绝是不带业务码的普通错误（文本如 `scene: full`、`scene: stale membership`、`scene: closed`）。

**日志**：玩家切图、撤离确认记 Info；开图、关闭、排空准入记 Debug。时间倍率只提供 handler / Sender，日志由外部调用方记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/scene/controller.go`, `game/scene/service/runtime.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **只管本进程**：Scene 是进程内的实体，成员、视野、时间域都是进程内权威。不做跨 game 进程切图、全服同图成员、分布式容量；跨服迁移要另做所有权交接。进程重启后场景要用同样的配置重新 `Open`，在线成员与时间域不恢复。
- **数量上限**（都写在 `game/scene/scene.go` 与 `service/runtime.go`）：一个进程至多 128 张图（`MaxScenes`）；每张图容量 1～10000；每张图至多 64 个静态区域、256 个静态交互对象；交互对象的半径不超过视距、文本不超过 512 字节；每个观察者视野里至多 128 个实体（`MaxVisible: 128`）；一次 `SceneChange` 至多涉及两张图。
- **视野规则**：进入视野的半径是 `Sight`，离开视野的半径是 `Sight + BlockSize`（留一圈滞回）。不同分线是不同的 Scene 实体，不共享成员、视野或时间域，互相看不见。进出与位移只在事务**提交之后**才影响旁观者；回滚的移动不会让别人看到。
- **视野不是授权依据**：`Sense` / `Nearby` 的结果只是候选。要对目标做修改（伤害、交易）时，必须在包含目标的 Nest 事务里重新核对场景、距离与存活，不能拿视野结果当事实。成员归属以 Scene 的成员账本与 actor 的所在场景为准。
- **一致性**
  - `SceneChange`：Player 与新旧两张 Scene 在同一笔 undo 事务里——容量检查、成员变更、离开旧图的存档、进入新图一起成功或撤销。客户端给的 `FromID` 只用来声明要锁谁；handler 在锁内重查玩家实际所在的场景，不一致就拒绝。
  - `SceneInteract`：序号必须是上一次的加 1。重发**最后一次**的同一请求返回上次的应答、不再触发回调；跳号、旧号、同序号换了场景或对象都拒绝。只保留最后一份回执，所以它只防紧接着的重发。
  - 断线不改成员：仍加载但断线的角色继续占容量（方便重连）；已卸载的角色留下的成员在 `Admit` / `Close` / `Stop` 持锁核实后清理。
  - 关图：`Close` 不会踢人；`Drain` / `Stop` 失败（某个模块没清干净、撤离事务被拒）时保留场景，可重试；别的实体 kind 没登记 `RegisterDrainer` 却还在图里时，`Drain` 返回错误。
- **拒绝会断开连接**：`HandleSceneChange` / `HandleSceneInteract`（`game/controllers/scene/`）把所有拒绝作为 Go error 直接返回，应答里的 `Code` / `Reason` 不会被填。按接入层的行为（`infra/network/gateway/tcp.go` 的 `dispatchRequest`：端点返回 error 时结束这条连接），场景已满、目标未开放、`FromID` 过期、交互对象太远、序号跳号这些业务拒绝都会让这名玩家的连接被关掉。要让客户端收到可恢复的拒绝，工程需要自己在这两个端点里把错误转成应答。
- **不做的事**：不做移动（在 actor）；不做怪物、战斗、掉落；不做有状态的 NPC / 建筑（`Config` 里的区域与对象是静态只读的，有状态的要另建实体）；不做场景内广播协议；不做动态开图的客户端入口；时间域只有倍率（0～4），不影响全局业务时间。
- **前提**：依赖 actor 模块、nest 与 heartbeat 两个 Mod；安装时代办玩家 TCP 接入、`access.player.self_sync` 与实体 lifecycle 的接线；Player 的同步 packer 必须是 actor 提供的 `NewActorPlayerSyncPacker`——工程已有自定义 packer 时安装被拒绝。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“场景模块的边界”一条（scene / monster / terrain 是单进程所有权）、“运行期接线的边界”一条（`runtime` 入口在业务 Init 之前运行、只支持一次启动与串行停机）、“chat 模块的边界”一条里关于附近聊天的部分。

## 复用方式

- **需求正好是它**：`roost module add scene`，连带装上 actor。装完启动就有 1 号场景，客户端发 `SceneChange{FromID: 0, ToID: 1}` 进场，再用 actor 的移动协议走动。
- **需求多一点**
  - 多张图、分线：在服务端启动后的可信代码里取 `Manager` 调 `Open(编号, 配置)`；分线就是同一个 `Template`、不同 `Line`、不同编号的几张图。客户端选哪条线由工程自己的逻辑决定后再发 `SceneChange`。
  - 区域触发（进副本入口、安全区）：在 `Config.Regions` 里配静态矩形，`init` 里登记 `OnRegion`。
  - 可交互的静态物件：在 `Config.Objects` 里配位置、半径与文本，`init` 里登记 `OnSceneInteract` 做业务（发奖、推进任务都在同一笔事务里）。
  - 让附近的人看到 Player 的更多字段：`init` 里 `OnSceneFields("<名字>", "<PlayerDao 顶层字段>")`。登记的是整个顶层字段，登记前检查它嵌套的内容里有没有不该公开的；不要登记 `*`。
  - 有地形的图：`Config.Ground` / `Config.Surface` 由提供方实现（terrain 模块就是这样接的），实现必须支持并发读取已提交状态。
  - 不要改的文件：`game/scene/scene.go` 里 `Space` 的提交后发布逻辑、`handlerSceneChange` 的锁内复查、`SceneDao` 的字段。
- **别的模块 / 业务代码要用它**
  - 取附近的玩家（附近聊天、AI 索敌）：在已持有某个 actor 锁的 handler 里，`space, ok := body.Environment().Space.(*gamescene.Space)`，再 `space.Sense(body.Position(), space.Sight())`；拿到名单后离开 Nest 再做网络推送。
  - 把自己的实体显示在场景里：实体开启同步后调 `area.Space().Enter(value, at)`，消失时 `Leave(value)`；并向 `Manager.RegisterDrainer` 登记关图时的清理。
  - 需要把 Scene 放进自己的事务：用 `Manager.IDs(0, sceneID)` 取实体号，handler 的参数里声明 `sceneentity.ISceneEntity`。
  - 关掉自己开的图：`Manager.Drain(ctx, id)`。
- **不适合直接用的情形**：无缝大世界 / 跨进程分区（参考仓库里的 cellworld 原型，不是这个模块）；需要全服唯一的场景实例；帧同步房间（用 room / turn）。值得借鉴的是：成员账本与视野索引分开、视野只在提交后更新；“关准入 → 模块各自清理 → 撤离玩家 → 关图”的排空次序与按实例复核；公开字段白名单的登记方式。

## 联动

scene 自己的 `with/`：

- `with/chat`：给 chat 加一个本地频道 `nearby`（`ChatSend` 里 `Kind="nearby"`、`Target=0`）。经 Nest 取发送者所在场景与视距内已提交站位的玩家快照，离开 Nest 后只向本进程仍有连接的收件人推 `ChatMessage`（含发送者自己）。是即时消息：没有历史、去重、离线补发或跨进程广播，`ChatHistory` 拒绝 `nearby`；推送失败不回滚任何状态。5 条场景。
- `with/profile`：`OnSceneFields("profile", "Profile")`——附近可见名字与等级，经验等私有字段仍只有本人可见。2 条场景。
- `with/attribute`：`OnSceneFields("attribute", "AttributeFinal")`——附近可见最终属性，`AttributeBase`、`AttributeGear` 仍私有。2 条场景。
- `with/equipment`：`OnSceneFields("equipment", "Equipment")`——附近可见装备，背包仍私有。2 条场景。

别的模块对 scene 的联动与依赖：

- `combat/with/scene`：`OnSceneFields("combat", "Combat")`——旁观者只看到公开的战斗投影。2 条场景。
- monster（依赖 scene）：怪物用 `Space` 显示与感知，启动时 `RegisterDrainer`，关图时先清本图怪物。
- terrain（依赖 scene）：用 `Open` 打开带 `Ground` / `Surface` 的图，地形块与建筑经 `Space().Enter` 进视野，停机用 `Drain` 只排空自己的图。

## 验证范围

- `roost module verify scene`：26 条场景（`^TestScene`，包 `./game/controllers/scene`）。覆盖：缺省场景与 PB 入口、切换同时改两边成员、过期 `FromID` 拒绝、客户端不能开图、有人的图不能关、满员拒绝且不把人从原图踢掉、切换被拒时存档位置回滚、同一视野、不同分线互相不可见、移动在提交后才改视野、回滚不动视野、公开 packer 只含登记字段、时间域按图独立与非法倍率、重连复用成员、区域进出由位置推导与回调失败回滚、交互按连续序号只生效一次、不能够到别的场景或远处的对象、感知只看已提交位置、停机撤离与重试、卸载角色的成员清理、依赖成员保留以便重试、单图排空。
- 联动：`with/chat` 5 条，`with/profile` / `with/attribute` / `with/equipment` 各 2 条；combat 模块拥有的 `with/scene` 另有 2 条。各联动都实际解码快照与增量，检查公开值存在、私有值不泄露、本人仍能看到私有值。
- 这些是离线场景：使用实际的 Controller、生成的 Sender、Nest 多实体事务、DAO、movement、Sync AOI 与时间域；提交器与同步 / 网络传输是受控的内存替身。没有验证真实数据库、跨进程 RPC、TCP 链路或容量性能。
- 仓库侧的安装验收在 `codegen/internal/roost/module_scene_test.go`（安装、重复安装不漂移、拒绝覆盖已有 packer 等）。
