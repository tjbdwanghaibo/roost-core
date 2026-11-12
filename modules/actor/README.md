# actor：实体移动底座（位置与朝向、走路与跟随、定身与沉默门控、位置存档）

## 能力

给 `Player` 提供一个“身体”（`actor.Body`）：它记录玩家在哪个空间的什么坐标、朝向哪里，能沿路径走、能跟随另一个实体、能被定身或沉默。它不知道地图长什么样：所在的空间（场景或房间）由上层代码以 `actor.Environment` 传进来。`monster` 模块的怪物复用同一个 `Body`。

- **客户端协议**（`protocol/def/`）
  - `ActorMove`（`actor_move.go`）：请求带目标坐标 `X`、`Y`，应答 `{Code, Reason, X, Y}` 带当前坐标。端点 `HandleActorMove`（`game/controllers/actor/actor_move.go`）。客户端只能指定目标，速度是服务端固定值 60（写在 `game/handler/actor_move.go`）。被拒时 `Code` 是下面“错误码”里的业务码，连接保留。
  - `ActorStop`（`actor_stop.go`）：停止移动，应答带当前坐标。端点 `HandleActorStop`。
  - 没有“进入空间”的协议：把玩家放进某个空间是上层（`scene` 模块或工程自己的房间代码）的事。
- **服务端入口**
  - 取身体：`target.ActorComp().Body()`（`game/entities/player/actor_component.go`），窄接口 `IActorEntity`。另有 `ActorComp().SavedPosition()` 读上次存档的空间号、坐标与朝向，`player.MapBasic()`、`player.MoveComponent()` 取底层移动组件。
  - `ActorComp().MoveRefusal(err)`：把 `Body.Walk` / `Follow` 的正常拒绝换成带业务码的错误（见“错误码”），在同一把实体锁内、紧接着失败的那次调用使用；别的错误原样返回。`handlerActorMove` 用的就是它，工程自己写的移动协议同样调它。
  - `Body` 的方法（`game/actor/body.go`，都必须在所属实体的锁内调用）：
    - `Enter(env Environment, at spatial.Point, facing movement.Facing) error`：放进一个空间。重复进入同一个空间保留现有站位；从别的空间切过来时先 `Leave`，同一笔事务。
    - `Leave() error`：离开当前空间并存档位置。
    - `Walk(path []spatial.Point, speed float64) error`、`Follow(target int64, speed float64, keep int64) error`、`Stop()`：走路径、跟随、停下。不能移动时返回 `actor.ErrCannotMove`，路径点超限返回包着 `actor.ErrPathTooLong` 的错误；`Body` 由 `Player`、怪物与 game-demo 共用，自己不带业务码。
    - `CanMove()`、`CanCast()`、`Position()`、`Environment()`：是否能移动（在空间里且没被定身）、是否能施法（在空间里且没被沉默）、当前坐标、当前空间接线。
    - `Tick(beat)`：移动拍，由正式心跳驱动；`MoveHeartbeat()` 返回心跳登记供诊断。
    - 字段 `States`（状态集合，状态号 `actor.Rooted`、`actor.Silenced`）、`Map`、`Move`、`Host`。
  - `actor.New(owner, Config)`：给别的实体（怪物）造一个 `Body`。`Config` 可给 `Fields`（位置存在哪）、`Save`（存档回调）、`Hooks`、`MaxPathPoints`、`Surface`。
  - `actor.Environment{ID, Space, Ground, Surface, Time}`：空间接线。`actor.Nowhere{}` 是一个什么都不做的 `Space`，用于没有 AOI 的房间。
  - `actor.Get(id)`：按实体号取已加载的实体（跟随目标用），由运行期入口 `actor.Start(registry)`（`game/actor/runtime.go`）接通。
  - Nest handler `handlerActorMove`、`handlerActorStop`（`game/handler/`，`durability=async`）。
- **登记点 / 钩子**
  - `OnActor(name string, factory func(*Player) actor.Hooks)`（`game/entities/player/actor_hooks.go`）：登记进入、离开、每拍之后三个回调（`actor.Hooks{Enter, Leave, AfterTick}`）。回调在当前实体锁和事务内执行，返回错误会撤销本次变更；回调里不能发 RPC、取别的实体锁或开 goroutine。只在 `init` 登记，多个按名字排序，重名 panic。
  - `OnActorSyncFields(name string, profile entity.SyncProfile, fields ...string)`（`game/entities/player/actor_sync.go`）：给一个非本人的同步视图登记明确的字段白名单（例如“附近玩家能看到哪些 `PlayerDao` 字段”）。不能登记本人视图，不能用 `*`。只在 `init` 登记。
  - `NewActorPlayerSyncPacker`：`Player` 唯一的同步打包工厂（清单的 `sync_packer`）。本人视图包含 `PlayerDao` 全部可同步字段；其他视图只含登记过的字段。
- **状态**
  - `PlayerDao.Actor`（`ActorState`：`SceneID`、`X`、`Y`、`Facing`，`db/def/actor.go`）：当前站位，`nopersist,sync`，只同步不落库。
  - `PlayerDao.ActorSaved`（`ActorSavedState`，同样四个字段）：存档位置，`persist,nosync`，落库不下发。
  - 定身、沉默状态集合是进程内的临时状态，不落库。
- **配置**：没有配置表与配置键。常量在 `game/actor/body.go`：`MovePeriod = 200ms`、`SavePeriod = 10s`、`MaxPathPoints = 256`、`MaxConfiguredPathPoints = 4096`。
- **错误码**（`internal/errors/actor.go`，都是玩家自己能触发的正常拒绝；端点把码填进应答，不记 Error 日志）
  - `actor_not_placed`：还没有被放进任何空间（没进场景或房间）。先进场再移动。
  - `actor_rooted`：在空间里但被定身。状态解除之后再移动。
  - `actor_path_too_long`：一次给的路径点超过上限（缺省 256）。模块自带的 `ActorMove` 每次只带 1 个点，触发不了；给服务端调用方与工程自己的多点移动协议用。
  - 别的失败（跟随自己、提交被拒等）不带码：端点答框架的内部码并记一条 Error。

## 能力边界

- **不做的事**：不管理地图、场景表与观察者（AOI），不做寻路（客户端或上层给路径点），不实现伤害与技能（`CanCast` 只是给 `combat` 用的门控）。不负责把玩家放进空间：单独装 actor 而没有任何代码调 `Body.Enter` 时，`ActorMove` 答 `actor_not_placed`。
- **上限**：一条路径缺省至多 256 个点，可信构造方可经 `Config.MaxPathPoints` 调到至多 4096。`ActorMove` 协议每次只带 1 个目标点。每拍推进的时间有上限，慢拍不追帧。
- **频率**：移动拍 200 毫秒一次。协议本身没有频率限制。
- **一致性**：位置在 DAO 里，路径、空间归属、心跳启停、当前环境都随 Nest undo 回滚；提交被拒后站位与存档位置恢复原状。空间索引（`Environment.Space`）只在提交后收到进出与位移。
- **存档**：每 10 秒以及离开空间时把位置写入 `ActorSaved`。崩溃时最多丢最近一次存档之后的位移；重新加载后当前站位（`Actor`）为空，要由业务按 `SavedPosition()` 重新放置。
- **定身与沉默不持久**：进程重启后状态集合清空，要由施加方（例如 `combat` 的 buff）恢复时重新派生。
- **跟随**：目标的位置经已发布的快照读取，不锁对方；不能跟随另一个空间里的目标，不能跟随自己。
- **前提**：需要 `nest` 与 `heartbeat` 两个 Mod（`heartbeat` 安装时代办）；运行期入口 `actor.Start` 由生成的 bootstrap 自动接线。实体必须叫 `Player`。工程已有自己的同步打包器时拒绝安装。只在单个 game 进程内有效，没有跨进程的移动或切图。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“场景模块的边界”“运行期接线的边界”两条。

## 复用方式

- **需求正好是它**：`roost module add actor`。不连带安装别的模块。一般不单独装，而是作为 `scene`、`combat`、`monster` 的依赖被带上。
- **需求比它多一点**
  - 改移动速度：改 `game/handler/actor_move.go` 里的 `60`。
  - 自己的房间或副本要放置玩家：在持有 `Player` 锁的 handler 里调 `target.ActorComp().Body().Enter(actor.Environment{ID: 房间号, Space: actor.Nowhere{}}, 坐标, 朝向)`；需要地形校验时填 `Ground` 或 `Surface`，需要独立时间流速时填 `Time`。
  - 进入、离开空间或每拍之后要做事：`init` 里调 `OnActor("<功能名>", 工厂)`。
  - 让别的玩家看到某个 `PlayerDao` 字段：`init` 里调 `OnActorSyncFields`（装了 `scene` 时用它提供的 `OnSceneFields`）。
  - `game/actor/body.go` 是 `Player` 与怪物共用的实现，尽量不改；要加行为优先用钩子。
- **别的模块 / 业务代码要用它**
  - 服务端驱动移动：`body.Walk(path, speed)` / `body.Follow(target, speed, keep)` / `body.Stop()`，持锁调用，错误返回给 Nest。结果要答给客户端时先过一遍 `target.ActorComp().MoveRefusal(err)`，否则这些拒绝到客户端是内部错误。
  - 施加定身、沉默：改 `body.States`（状态号 `actor.Rooted`、`actor.Silenced`）；`combat` 用 buff 绑定派生这两个状态。
  - 读位置：持锁时 `body.Position()`；空间号 `body.Map.SceneID()`。
  - 没有持锁时：`syncsender.NewActorMoveSender(client).Sync_ActorMove(ctx, 完整实体号, &pb.ActorMoveRequest{…})`。
- **只适合借鉴的情形**：帧同步、物理驱动或需要服务端寻路的移动不适合直接用；值得借鉴的是“位置权威在 DAO、空间索引只收提交后的事实、环境由上层注入、状态门控与移动分开”的边界划分。

## 联动

- 本模块没有 `with/` 目录，别的模块也没有 `with/actor`。
- 以 `requires` 依赖它的模块：`scene`（经 `OnActor` 登记区域回调，经 `OnActorSyncFields` 登记附近可见字段）、`monster`（用 `actor.New` 给怪物造 `Body`）、`combat`（读位置、读 `CanCast`、把 buff 绑定到 `States`，并在 `Body.Host` 上登记战斗心跳）。

## 验证范围

- `roost module verify actor`：14 个场景（`./game/handler`，`^TestActor`）。
- 场景是离线的：真实的 Nest 引擎、生成的 DAO、框架的 movement 与 heartbeat；提交器（`actorCommitter`，可注入拒绝）与空间是替身。
- 覆盖：没有 scene 也能工作、协议使用认证身份与服务端速度、定身暂停与恢复移动、停止取消移动、离开时存档位置与朝向、移动启动失败回滚路径、离开失败恢复归属与存档、提交被拒恢复存档与当前站位、使用所进入空间的地形、重复进入不瞬移、拒绝超长路径与跟随自己、心跳经 Nest 推进；经正式端点（手写入口 + 生成的转交）发 `ActorMove`：没进空间与被定身各答自己的业务码、不记 Error、不起路径，解除之后同一个入口成功；路径点超限换成 `actor_path_too_long`，别的错误不被改写。
- 没有验证：真实数据库、经 TCP 的客户端链路、吞吐压力。

## 实现说明

- `Body` 组合了框架的 `movement.MapBasic`、`movement.Move`、`state.Set` 与 `heartbeat.Host`；每拍位置走 DAO 与同步，不写持久日志。
- `Environment` 的地形与时间域必须支持无锁读取已提交状态；`Space` 的进出沿用提交后回调。跟随目标通过 `actor.Get` 取到的实体的已发布位置读取。
- 共享实现只保存在 `files/game/actor/body.go.tmpl`。game-demo 消费同一份模板，用自己的 `Player` 字段与分区场景接线适配；它共用的是移动机制，并没有完整安装 actor / scene 模块。所以这个文件不 import 工程的 `internal/errors`：拒绝是两个普通的哨兵错误（`ErrCannotMove`、`ErrPathTooLong`），换成业务码的一步在 `Player` 这一侧的 `MoveRefusal`。
