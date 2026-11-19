# PhysX 物理房间 + LevelComponent 联合玩法：实施证据（2026-11-19 起）

方案：[FEATURE-2026-11-19-physx-level-actions](../../FEATURE-2026-11-19-physx-level-actions.md)（D1～D11 全部按推荐 A）。在分支 `feat/physx-level-actions` 上实施，分支基线 main `2d15a1e5`，每批一个提交。本文按批记录：做了什么、怎么验的、哪些没有验到。

环境（各批相同，除非另写）：Apple M5（10 核）、macOS 26.5.2（25F84）、Go 1.27.3、Apple clang 21.0.0。原生库用本机已编好的 PhysX 4.1.2 静态库（`/Users/whb/roost/.physics-deps/build`，PhysX 上游仓库的修订 a2c0428a，不是本仓库的提交）与分支里的 `bridge.cpp` 以 clang++ 链出 `infra/physics/physx/lib/libroost_physx.dylib`（gitignore，不入库）。**只在 macOS ARM64 上跑过；Linux 没有跑。**

变异验证的做法：绿之后逐条撤掉或改掉一处关键代码，跑对应用例，要求变红并记下失败文本，然后原样恢复。每批的原始记录在 `mutations-batchN.txt`。

## 批 1：native `rp_set_pose`、ABI 3、`World.SetPose`

### 改动

| 文件 | 内容 |
| --- | --- |
| `infra/physics/physx/native/bridge.h`、`bridge.cpp` | 新增 `rp_set_pose`：只接受动态刚体，写入位姿、线速度、角速度，清力与力矩的两组累加器，最后 `wakeUp()`；静态、运动学、未知 id 返回 0。`rp_abi_version()` 返回 3。结构体布局未变 |
| `infra/physics/physx/native.go` | `World.SetPose(id, position, rotation, velocity, angularVelocity)`：与 `Step` 同一把锁；校验有限值与单位四元数；目标不是动态刚体返回 `ErrPoseTarget`；`New` 只认 ABI 3 |
| `infra/physics/physx/types.go` | `ErrPoseTarget`、常量 `nativeABI = 3` |
| `infra/physics/physx/disabled.go` | 不带 `physx` 标签时的同名桩，返回 `ErrUnavailable` |
| `infra/physics/physx/native_test.go` | `TestSetPoseMovesADynamicBodyAndClearsItsMotion` |

与方案文字的两处出入：

- 方案写“`clearForce` / `clearTorque`（`eFORCE` 与 `eACCELERATION` 两种都清）”。PhysX 里这两种模式共用同一组累加器，另一组是冲量 / 速度变化；实现清的是这两组（`eFORCE` 与 `eVELOCITY_CHANGE`），覆盖全部四种模式。
- 各 `set…` 调用都不带自动唤醒，统一由最后的 `wakeUp()` 唤醒，这样“唤醒”只有一处、能被用例单独证明。

### 验证

```
GOWORK=off CGO_ENABLED=1 go test -count=3 -race -tags physx ./infra/physics/physx    # ok
GOWORK=off go build ./...                                                           # 不带标签，通过
GOWORK=off CGO_ENABLED=1 go vet -tags physx ./infra/physics/physx                    # 通过
```

用例断言的内容：

- 带着水平速度和滚动角速度的球被传送后，下一步的位置等于目标位姿加一步重力位移，水平速度与角速度为零，旋转等于给定的四元数。
- 传送时给定的线速度与角速度在下一步生效。
- 两个休眠的球，把其中一个沿地面传送到与另一个重叠、速度为零：50 步后两球被推开（球心间距不小于 0.9，两半径之和是 1）。
- 静态、运动学、未知 ID 返回 `ErrPoseTarget`，运动学平台留在原处。
- 零四元数、非单位四元数、NaN、越界速度被拒绝；业务 worker 上调用返回 `fctx.ErrBlockingInBusinessWorker`；关闭后返回 `ErrClosed`。

新 API 在旧代码上写不出能编译的红，所以批 1 的红全部是变异红。另有一条真实的修前现象：新的 Go 绑定配旧原生库（ABI 2，没有 `rp_set_pose`）在链接期就失败：

```
Undefined symbols for architecture arm64:
  "_rp_set_pose", referenced from:
      __cgo_cfcf21c5b342_Cfunc_rp_set_pose in 000001.o
ld: symbol(s) not found for architecture arm64
FAIL	github.com/tjbdwanghaibo/roost-core/infra/physics/physx [build failed]
```

### 变异（[mutations-batch1.txt](mutations-batch1.txt)）

8 条，6 条变红。

| 变异 | 结果 | 失败文本（节选） |
| --- | --- | --- |
| native 不写线速度 | 红 | `next step does not start from the new pose`（x 漂到 5.227，速度 11.37） |
| native 不写角速度 | 红 | `old angular velocity survived the teleport on axis 2` |
| native 不唤醒 | 红 | `sleeping body was not woken by the teleport: still overlapping, gap 0.200` |
| Go 绑定不查运动类型 | 红 | `body 100: want ErrPoseTarget, got physx: set pose failed`、`body 50: …` |
| Go 绑定与 native 都不查运动学 | 红 | `body 50: want ErrPoseTarget, got <nil>`、`rejected teleport moved the kinematic platform: [0 9 0]` |
| 原生库仍报 ABI 2 | 红 | `physx: native ABI mismatch; rebuild native library` |
| 只有 native 不查运动学（Go 绑定仍查） | **没有变红** | Go 绑定的检查在前，请求到不了 native；native 这一处检查由上面“两边都不查”那条证明有效 |
| native 不清力与力矩累加器 | **没有变红** | 见下 |

**没有被用例覆盖的一处**：`rp_set_pose` 里的四行 `clearForce` / `clearTorque`。现在力只在 `rp_step_control` 里累积、并在同一次 `simulate` 里消耗，两次调用之间累加器本来就是空的，所以这四行在现有调用次序下不改变任何结果，Go 侧也写不出能区分的用例。保留它们是因为方案的接口约定写了“清掉累积的力和力矩”；它们是否正确只靠读 PhysX 文档判断，没有运行证据。

唤醒用例的一点说明：最初写的是“把休眠体放到空中，看它会不会下落”，以及“原地给一个初速度”，两种写法在去掉 `wakeUp()` 后都仍然通过——刚体脱离支撑或被赋予非零速度时 PhysX 自己会唤醒它。改成“沿地面传送到另一个休眠体身上、速度为零”后变异才变红，这也正是方案 §8 里“传送到有人的出生点”的情形。

“旧 ABI 被拒”没有做成进程内用例（一个进程里装不了两份原生库），由上表“原生库仍报 ABI 2”这条变异和上面的链接失败记录代替。

## 批 2：Level schema 2、区域判定、RP3 与传送标记

### 改动

| 文件 | 内容 |
| --- | --- |
| `gameplay/physics/level.go` | `Level` 只认 `LevelVersion = 2`，新增 `Regions`、`Spawns`、`DefaultSpawn`、`Flow`；`Region`、`Spawn` 类型；`ValidateRegions`（供批 3 的直接 `Spec` 建房共用）。上限：区域 64、出生点 64、每个出生点 20 个位置、Flow 256KiB；坐标 1000m、半尺寸 0.05～100m；ID 是 1～64 位的字母、数字、`_`、`-` |
| `gameplay/physics/flow.go`（新） | 物理房间自带的事件与动作声明（`region.enter` / `region.leave` / `member.join` / `member.leave`，`physics_teleport` / `physics_checkpoint` / `physics_respawn`）；`ValidateFlow`（能解码、tick 时间域）；`CompileFlow` 与 `Level.Program`（合并业务能力表后编译，并核对流程里写死的出生点名与区域名） |
| `gameplay/physics/regions.go`（新） | `RegionTracker.Step`：纯函数，由上一已提交帧、本帧、本 tick 被传送的刚体集合，给出有序的进入 / 离开事实与新的占用集合 |
| `infra/physics/physx/types.go` | `State.Teleports`（房间附加字段，不进 native、不上线） |
| `gameplay/physics/replication.go` | `ViewTeleported`（状态字节 bit3）；`ViewEntry.Teleports` 记每个对象上次发给该接收方时的传送计数，不一致就在这一帧强制刷新并置位 |
| `gameplay/physics/quantized.go` | 私有头版本 2 → 3（`RP\x03`）；状态字节解码上限 7 → 15；`RoomReplica.Profiles` 带出传送标记 |
| `client/physics-viewer/tests/packed.json` | 新增 `RoomFull`、`RoomTeleport` 两帧完整的 RP3 房间帧金样（Go 侧已核对，C# 侧在批 4 读同一份） |
| `wiring/physicsroom/cmd/physicsroom-local/main.go` | 内置的 crowd 关卡改用 `LevelVersion`（只改版本号；示例流程在批 3） |

规则摘要：

- 区域是有向包围盒，按刚体**中心点**判定。进入看区域本身；离开要走出外扩 0.25m（`RegionExitMargin`）的范围；两帧都在外面但连线穿过区域时，同一帧给进入、离开各一条，占用集合不变。被传送的刚体只按终点判定。事实次序：区域按关卡里的顺序，刚体按 ID 升序。
- 有出生点就必须写 `DefaultSpawn`，没有出生点就不能写——“没有检查点时重生到哪里”始终有确定答案。
- 传送标记只在传送后发给该接收方的第一帧里出现，下一帧清掉。为了清标记，对象在下一帧会再刷新一次（远档、中档因此比原周期多发一次，每次传送每个接收方一次）。新进入视野的对象不带标记。

### 与方案文字的出入

| 方案 | 实际 | 原因 |
| --- | --- | --- |
| 物理能力注册表放在 `service/physicsroom/actions.go` | 声明（名字与参数类型）放在 `gameplay/physics/flow.go`；执行器仍按方案在批 3 放进 `service/physicsroom/actions.go` | T2 要求在 `gameplay/physics` 里就拒绝“未注册能力”；能力名是关卡格式的一部分，Game 进程加载关卡时不该为了校验去依赖房间服务 |
| “强制完整字段 + bit3” | 刷新时带的是该接收方当前档位应有的字段：近档有速度，中 / 远档（展示档）仍然不带速度 | 与“新可见 / 跨 Chunk 同一条刷新路径”一致；展示档本来就不发速度，传送后的速度也是零。**如果本意是传送帧连展示档也要带速度，这里需要改** |
| `DecodeLevel` 拒绝未注册能力 | `DecodeLevel` / `Validate` 只查流程能否解码与时间域；能力是否登记要调用 `Level.Program(业务能力表)` 才查 | 业务能力由宿主登记，解码时不知道。批 3 的 `Create` 与 `physicsroom-local` 加载关卡时都要调用 `Program` |

### 验证

```
GOWORK=off go test -count=1 ./gameplay/physics                                  # ok
GOWORK=off CGO_ENABLED=1 go test -count=1 -race -tags physx \
  ./infra/physics/physx ./gameplay/physics ./gameplay/level \
  ./service/physicsroom ./wiring/physicsroom/...                                 # 6 个包全部 ok
GOWORK=off go build ./...                                                       # 通过
GOWORK=off go vet ./gameplay/physics ./gameplay/level                           # 通过
GOWORK=off CGO_ENABLED=1 go vet -tags physx ./infra/physics/physx ./gameplay/physics ./service/physicsroom ./wiring/physicsroom/...   # 通过
GOWORK=off go test -count=1 .                                                   # 根目录门禁，见下
dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj        # 3 条 PASS（C# 未改，只确认金样文件加字段不影响它）
```

用例（按方案 6.1 的编号）：

| # | 用例 | 断言 |
| --- | --- | --- |
| T2 | `TestLevelSchemaTwoRejectsBadRegionsSpawnsAndFlow`（`level_test.go`） | 合法的 schema 2 关卡能发布、解码、再发布字节不变、流程能编译；区域 / 出生点 / 流程都计入内容摘要；schema 1、schema 3、三处未知字段拒绝；27 种非法的区域 / 出生点 / 流程拒绝；达到上限合法、超过才拒绝；未登记的事件与动作、缺参数、参数类型不对、写错的出生点名与区域名（写在比较的左边或右边）编译失败；业务能力登记后可用，与物理能力重名拒绝 |
| T3 | `TestRegionTrackerEntersLeavesAndCatchesFastCrossings`、`TestRegionTrackerRotationWatchListsAndOrder`（`regions_test.go`） | 14 步的行走序列逐步核对事实与占用（面上算进入、滞回内不离开、滞回内不算进入、一帧穿过给一对、传送只判终点）；5 条不穿过盒子的路径不算；斜穿算；斜放 45° 的区域按自身轴向判定；关注名单与事实次序在 20 次随机输入次序下不变；不修改传入的占用集合，无变化时原样返回；不保留调用方切片 |
| T4 | `TestTeleportMarkReachesEveryReceiverOnce`（`replication_test.go`） | 近 / 中 / 远三档接收方走完整的字段增量 + RP3 头 + 客户端副本解码：传送帧带标记和新位置，下一帧清掉，之后回到各自周期；未确认的计划不改账本、重新计划标记仍在；传送那一帧没收帧的接收方在下一帧看到一次；两次传送各看到一次（期间没收帧的只看到一次）；新可见不带标记；关闭分档时同样只一次 |
| — | `TestRoomProfileFlagsAndOldProtocolRejection`（已有，扩充） | 帧头版本是 3；版本 1、2、4 拒绝且不动基线；状态字节 256 个取值逐个核对合法性 |
| — | `TestRoomTeleportGoldenSharedWithCSharp`（新） | 服务端同一条路径生成的两帧与 `packed.json` 逐字节相同，解码结果即 C# 侧要断言的值 |

改了期望值的旧用例两处，都是协议变化的直接后果：

- `TestRoomInvalidBodyDoesNotPublishAckOrBaseline` 原来用状态字节 8 当“非法值”，8 现在是传送标记，改用 16。
- `TestRoomReplicationPipelineStaysByteIdentical` 的摘要换了新值。把 `roomMagic` 临时改回 `RP\x02` 时旧摘要原样通过，说明没有传送的房间除帧头版本字节外逐字节不变（对象字节、分档、账本都没变）。

行为红（T2）：在批 1 的提交上，同一份 schema 2 关卡被拒绝——`schema 2 level refused: json: unknown field "Regions"`。T3、T4 是新 API，没有可编译的行为红，用变异证明。

根目录门禁：批 2 完成时全部通过。**批 1 提交时没有跑根目录门禁，留下一处失败**：本文环境一段把 PhysX 上游修订号写成了反引号括起的十六进制串，被 `TestDocumentedCommitsAreReachableFromHEAD` 当成不在历史里的本仓库提交。批 2 改了写法；从本批起每批提交前都跑根目录门禁。

### 变异（[mutations-batch2.txt](mutations-batch2.txt)）

30 条，全部变红。每条变异后跑整个 `gameplay/physics` 包。

| 用例 | 条数 | 变异 |
| --- | --- | --- |
| T2 | 14 | 同时接受 schema 1；不查区域与出生点；不查区域最小尺寸；Watch 不要求动态刚体；区域可以谁都不关注；缺省出生点规则放宽；上限放宽 1；旋转不要求单位四元数；流程不要求 tick；业务动作 / 业务事件可以与物理能力重名（各一条）；不查写死的出生点名；不查写死的区域名；区域名只查写在右边的 |
| T3 | 8 | 去掉线段判定；去掉离开滞回；被传送的刚体仍做线段判定；Watch 不排序；忽略区域旋转；旋转方向反了；直接改传入的占用集合；进入也用外扩范围 |
| T4 | 8 | 传送不强制刷新（方案写的“远档漏掉”，实际中档先红）；带过标记的对象下一帧不清；账本不记传送计数；刷新但不置位；解码上限仍是 7；帧头仍是 RP2；客户端副本丢掉标记；新可见也带标记 |

过程中的两次修正，都是测试自己的问题：

- 第一轮 28 条里“业务动作可以与物理动作重名”没有变红：用例里重名的那条声明参数表是空的，编译因为参数个数不对而失败，重名检查本身没被测到。改成“同名且声明一模一样”后变红，并补了事件重名的一条。
- T3 第一次运行失败：期望的占用状态是按位置推算的，没考虑滞回（在滞回带里是否算“在里面”取决于之前在不在）。改成每一步显式写出期望。实现没有改。

### 没有验到的

- 只在 macOS ARM64 上跑过。
- `service/physicsroom` 还没有产生 `State.Teleports`（批 3），所以真实房间里还不会出现带标记的帧；T4 是在 `gameplay/physics` 里用构造的帧证明的。
- 批 2 之后服务端发 RP3，而仓库里的 C# 客户端仍只认 RP2，要到批 4 才更新：这两批之间分支上的服务端与 Unity 客户端不能互通。现有 C# 用例不解析帧头，所以仍然通过。
- `ViewEntry` 多了一个字段（每个接收方每个可见对象多 8 字节账本），以及清标记多出的一次刷新，对分配与载荷的影响留到批 5 的性能对照里测。
- 现行文档（`docs/framework/PHYSICS-ROOM-SERVICE.md`、`client/physics-viewer/README.md` 等）仍写着 RP2 与旧关卡格式，按方案在批 5 统一更新；`CHANGELOG.md` 也在批 5 写。

## 批 3：房间执行器、流程挂载、业务动作与视图推送

### 改动

| 文件 | 内容 |
| --- | --- |
| `service/physicsroom/types.go` | `Spec` 增加 `Players`、`Regions`、`Spawns`、`DefaultSpawn`、`Flow`；`Delivery.Level`；新类型 `Tick`（帧 + 事实 + 回执，首帧带 `Program`）、`Settled`（待执行动作、公开视图、作废回执数）；`Commit` 签名改为 `func(ctx, Ref, Tick) (Settled, error)`；`ActionHandler`；`Config.Actions` / `ActionTimeout` / `ActionConcurrency`；错误码 `ErrLevel`（640107） |
| `service/physicsroom/actions.go`（新） | 流程执行器：建房时校验并编译流程；每 tick 在 Step 之前执行上一笔提交交回的传送 / 检查点 / 重生；把成员进出与区域进出变成事实；提交成功后才采纳动作、区域占用与视图；业务动作在独立 goroutine 上按上限派发、按同一 ID 退避重试 |
| `service/physicsroom/level_view.go`（新） | `RoomLevelView` 的确定性编码与严格解码，上限 16KiB |
| `service/physicsroom/service.go` | 主循环接入上面三步；`State.Teleports` 写入帧；成员加入、主动离开、超时离开记入变更队列；`Poll` 把流程视图随帧冻结、重发、确认；流程出错记 `level_failed`；停房时取消并等待在途的业务动作 |
| `service/physicsroom/mechanisms.go` | `updateGrabs` 不把本 tick 被传送的刚体算作想抓的一对 |
| `gameplay/level/projection.go` | `LevelComponent.Revision()`、`Phase()` 两个只读访问器 |
| `gameplay/physics/level.go` | 内置示例关卡 `ObstacleLevel` 带上掉落区、两个检查点、终点、三个出生点与流程（公开变量 `falls`） |
| `wiring/physicsroom/nest.go` | `AcceptTick`：提交 handler 的全部工作（接受帧 → 首帧初始化并启动流程 → 回执 → 事实 → 按帧号推进定时器 → 状态变了才取动作与视图）；`NestCommit` 跟随新签名 |
| `wiring/physicsroom/entity.go` | `RoomEntity` 挂 `LevelComponent`（瞬态 DAO，首帧用 `Tick.Program` 建）；`EntityHost.Commit` 返回 `Settled`；新增 `EntityHost.Level` 读流程状态 |
| `wiring/physicsroom/host_mod.go` | `EntityHostMod.WithActions` 登记业务动作执行者 |
| `wiring/physicsroom/relay.go`、`players.go` | `PlayerIngressConfig.LevelMessageID`；`Relay` 把 `Delivery.Level` 用这个消息号、Protobuf 载荷推在同一次交付的帧之前 |
| `wiring/physicsroom/cmd/physicsroom-local/main.go` | 开房时带上关卡的玩家、区域、出生点与流程；流程视图消息号 `0x50485904` |
| `client/physics-viewer/room_input.proto`、`tests/packed.json` | `RoomLevelView` 消息定义；金样 `LevelView` |
| `scripts/physics/verify-service.sh` | `-run` 列表加 `TestRealGateLevelRespawnAndFinish`（根包 `TestPhysicsServiceRunnerCoversNativeIntegration` 要求脚本点名每个原生集成用例） |

`service/physicsroom/rooms_rpc_gen.go` 重新生成后没有差异：RPC 用 JSON 传 `Spec` 与 `Delivery`，新字段随结构体走；`Tick` / `Settled` 不过 RPC。跨 NATS 的往返由 T10 实测（建房的 `Spec` 带区域、出生点、流程；`Delivery.Level` 经 Rooms RPC 回到 Game 侧）。

### 与方案文字的出入

- **抓取释放只保留一处。** 方案 4.4 写“先 `RemoveJoint` 释放……；在已提交帧追上传送 tick 之前不重新抓取”。实现里传送本身不拆弹簧：紧接着的 `updateGrabs`（同一个 `BeforeStep`，Step 之前）不把本 tick 被传送的刚体算作想抓的一对，于是它作为抓取者或被抓者的弹簧在那里释放，也不会按传送前的位置重新抓上。两处都写的话，前一处删掉后没有任何用例能区分，所以只留后一处。可观察的结果与方案一致（Step 之前弹簧已不在、下一帧提交前不重抓），由 `TestTeleportReleasesGrabsAndWaitsForTheFrame` 逐 tick 核对抓取表。
- **阶段名是小写。** 方案 T10 写 `phase=Finishing`；`gameplay/level` 的取值是 `waiting / preparing / running / finishing / closing / closed`，视图里原样下发。手工编码金样时按大写写了一次，与 Go 编码器对不上才发现，金样已按实际取值改正。
- **公开变量的键带作用域前缀。** `level.View.Variables` 的键是 `global/<名字>`、`stage/<阶段>/<名字>`、`trigger/<阶段>/<触发器>/<名字>`，视图原样下发；批 4 的 HUD 要按这个键显示。
- **事实按流程阶段投递（方案没有写）。** `LevelComponent.Handle` 只在 running 阶段接受事实。执行器因此在准备阶段（`PrepareDelay`）把事实攒着、开始后按序补投；流程结束之后产生的事实直接丢弃。同一笔提交里前面的事实已经让流程结束时，后面的不再投递。
- **事实积压有上限（方案只写了每 tick 64 条）。** 攒到 4096 条仍交不出去时房间停止并记 `level_failed`：那说明事实产生得持续比流程处理得快。
- **`Settled.StaleReceipts`。** 方案说作废回执“记指标后忽略”；指标在 `service/physicsroom` 里，handler 在 `wiring` 里，所以 handler 把作废数随结果带回。
- **`Spec.Players` 在建房时校验**（存在、不重复、是动态刚体），方案没有单列。
- **业务事件没有投递入口。** `ActionHandler.Registry()` 里声明的业务事件能通过流程编译，但本轮没有把业务事件投给流程的接口，接口注释里写明了。
- **`EntityHost.Level`** 是方案之外新增的只读入口，用来在用例里读流程状态（回滚、重放的比对）。

### 验证

HEAD 为本批提交；命令都带 `GOWORK=off`。私有 NATS：本机 `nats-server -a 127.0.0.1 -p 14377`（只给这次验证用，结束后已停）。

| 命令 | 结果 |
| --- | --- |
| `go build ./...`、`go vet ./...` | 退出码 0（vet 16s） |
| `go vet -tags "integration physx"` 于 `service/physicsroom`、`wiring/physicsroom/...`、`wiring/gate`、`gameplay/physics`、`gameplay/level`、`infra/physics/physx` | 退出码 0 |
| `gofmt -l service wiring gameplay infra/physics` | 无输出 |
| `go generate ./service/physicsroom` | `up to date: rooms_rpc_gen.go` |
| `ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14377 scripts/physics/verify-service.sh`（`-race -count=3`：原生物理六个包，再加五个真实 NATS / Gate 集成用例） | 退出码 0，57s（当时本机 1 分钟负载 31，另有会话在跑别的测试） |
| `go test -count=10 -race -tags physx ./service/physicsroom` | ok，80.7s |
| `go test -count=6 -race -tags "integration physx" -run 'Level\|RoomEntity\|PlayerIngress\|EntityHost\|RealGate' ./wiring/physicsroom` | ok，52.3s |
| `go test -count=120 -tags physx -run TestBusinessActionsNeverBlockThePhysicsLoop ./service/physicsroom`（仪表次序修复之后） | ok，76.4s |
| `go test -count=1 ./...`（全仓，缺省标签、缺省 10 分钟超时） | 172 个包 ok，25 个没有测试文件，1 个失败：`codegen/internal/roost` 在 601.9s 被缺省超时终止（当时在跑 `TestModuleAllianceGenerated`，1m48s），没有任何用例报失败。见下 |
| `go test -count=1 .`（根包门禁，证据文档写入之后） | ok，28.1s |
| `dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj` | 3 组 PASS，退出码 0（C# 还没有改；只确认金样文件多一项不影响现有用例） |

`codegen/internal/roost` 是仓库里已知的慢套件：`scripts/pretag.sh` 为它写了 `-timeout 60m`，注释里记着它在本机单独跑要 300～800s、随磁盘负载波动，全量跑时出现过同样的“timed out after 10m0s 且没有失败用例”。这个包与 `codegen/` 下的模板都不引用 `service/physicsroom`、`wiring/physicsroom`、`gameplay/physics`。按 `-timeout 60m` 单独重跑：到本批提交时已经跑了 42 分钟还没有结束（本机负载 30～45，另一个会话同时在它自己的目录里跑同一个套件），**结果未确认**；跑完后的结果补记在批 4 一节的开头。（补记：3404s 后以 3 个用例失败结束，这 3 个用例在分支基线上同样失败，见批 4 一节开头。）

用例（T5～T10 用方案里的名字；其余是实施中补的）：

| 编号 | 用例 | 断言的内容 |
| --- | --- | --- |
| T5 | `TestLevelActionsRunOnlyAfterTheFrameIsSettled` | 提交交回动作后，下一帧才从出生点出发（位置、朝向、速度清零、`Teleports=1`），回执与那一帧在同一次提交里；首帧带 `Program`；提交失败或流程失败时房间停止、不再提交、动作不执行，两种失败各记各的指标 |
| T6 | `TestRepeatedPendingActionTeleportsOnce` | 同一动作在回执提交前再次出现在动作表里（含回执正随那一帧提交的那一次）只传送一次、只有一条回执 |
| T6 | `TestTeleportReleasesGrabsAndWaitsForTheFrame` | 被传送的是被抓者或抓取者两种情况：传送那一帧及之后两帧的抓取表为空，两端都没被拖动；传送前按下的跳跃不在出生点起跳 |
| T6 | `TestInvalidTeleportCompletesAsFailure` | 九种不合法目标各得一条失败回执、按动作序号排列、世界不变、房间继续；检查点只记不动；重生回最近的检查点，没有检查点的玩家回缺省出生点里自己的位置 |
| T6 | `TestCancelledActionReceiptDoesNotStopTheRoom` | 作废回执只记指标 |
| T7 | `TestBusinessActionsNeverBlockThePhysicsLoop` | `Execute` 卡住时 20 帧在 1s 内走完；同时在途恰好等于上限 2；在途期间再次交回不重复派发；每个动作一条回执、结果与返回值一致；停房时在途调用被取消且 `Destroy` 等它们返回，未了结数记指标 |
| T7 | `TestUnknownBusinessResultIsRetriedWithTheSameID` | 返回错误、panic、超时三种“结果未知”都不产生回执，带同一 ID 重试，第四次成功才有一条成功回执；`Execute` 的期限是配置值；重试之间参数表互不影响 |
| — | `TestLevelFactsReachTheFlowInOrder`、`TestLevelFactBacklogStopsTheRoom`、`TestExpiredMemberLeavesTheFlow` | 准备阶段不投递；开始后 102 条事实按 64 / 38 分两帧、编号连续、成员事实在前区域事实在后；结束后不投递也不积压；积压超限停房；超时离开也产生 `member.leave` |
| — | `TestPollCarriesLevelViewOncePerRevision` | 视图每个版本每个成员一次；未确认时帧与视图原样重发；后加入的成员第一帧带最新视图；视图超 16KiB 按流程失败处理 |
| — | `TestCreateRejectsInvalidLevelSpecs` | 八种不合法的玩家 / 出生点 / 流程 / 业务能力被拒绝（含正向对照）；没有流程的房间提交里不带流程数据 |
| — | `TestLevelViewGoldenSharedWithCSharp`、`TestLevelViewEncodingIsDeterministicAndBounded`、`TestDecodeLevelViewRejectsMalformedInput` | 金样由 Python 按 proto 独立手工编码，不经过 Go 编码器；编码与 map 次序无关；恰好 16KiB 可以、多 1 字节拒绝；25 种畸形输入拒绝 |
| T8 | `TestRoomEntityRollsBackFrameAndLevelTogether` | 一笔提交里第一条事实合法、第二条出错：流程状态 JSON 与提交前逐字节相同，物理帧仍是上一帧；同一帧重试成功；空闲帧不交回动作与视图；被取消的动作与从未请求过的动作的回执各记一次作废且状态不变；结算后同一笔里的事实不投递；`PrepareDelay` 按帧号到点才开始；没有流程的房间拒绝事实 |
| T8 | `TestLevelReplayIsDeterministic` | 真实 PhysX 房间跑完“路过检查点 → 掉落 → 回检查点 → 终点”，录下每笔提交，在两个新 Entity 上重放：每笔交回的结果与最终流程状态 JSON 都相同 |
| T9 | `TestPlayerIngressPushesLevelViewOncePerRevision` | 视图用自己的消息号与 Protobuf 载荷、紧挨在同一次交付的帧之前；每个版本一次；推送失败关连接、重连后先收到最新视图；没配消息号只推帧；消息号互不相同 |
| T10 | `TestRealGateLevelRespawnAndFinish`（integration、physx） | 真实 TCP → Gate → NATS → GameIngress → Rooms RPC → 房间 → Relay：客户端只发输入、只看收到的包。带 `ViewTeleported` 的帧恰好一帧且球在检查点；随后收到 `phase=finishing`、`result=clear`、`global/falls=1` 的视图 |
| — | `TestSeatCreatesTheRoomWithTheLevelFlow`（`physicsroom-local`） | 示例进程开房带上关卡的流程字段；缺省 CrowdScene 不带 |

T10 五次运行的实测（50Hz 模拟、20Hz 下发）：从客户端第一次看到球低于 y=-1 到收到带标记的帧是 15～16 tick（约 0.3s），其中球还要再落 2m 才进掉落区。

### 行为红

本批没有做到“每个用例先红后绿”：实现先写，用例后补。留下的红有两种：

- 在批 2 的提交 `14706f60` 上放入本批的测试文件：编译不过（`Tick`、`Settled`、`EncodeLevelView`、`Spec.Flow` 等都不存在），原始输出在 [mutations-batch3.txt](mutations-batch3.txt) 开头。这只说明测试依赖新接口，不说明行为。
- 行为上的红全部来自下面的变异。

### 变异（[mutations-batch3.txt](mutations-batch3.txt)）

共 65 条：服务层 45 条（T5～T7、事实、视图、建房校验），接线层 20 条（T8～T10、示例进程）。第一轮 60 条变红；3 条是变异自己编译不过（删掉一行后留下未使用的变量或导入），重写后变红；2 条没有被发现，补用例后变红。原始记录里保留了两轮的全部输出。

| 范围 | 条数 | 变异内容（节选） |
| --- | --- | --- |
| T5 提交与执行的先后 | 5 | 不检查 `Commit` 的错误；回执不随帧提交；不累计传送次数；传送后不清速度；忽略出生点朝向 |
| T6 去重、抓取、失败回执 | 12 | 不按 ID 去重；刚传送的刚体按旧位置判断抓取；不清跳跃意图；允许传送带配置关节的刚体；不检查动态刚体；检查点 / 重生对非玩家生效；重生忽略检查点；出生点位置不错开；失败回执成成功；检查点不校验出生点；作废回执不记指标 |
| T7 业务动作 | 7 | 在房间执行协程上同步执行；结果未知时给失败回执；不限在途数；不兜 panic；重试共用参数表；停房不等在途调用；没有期限 |
| 事实 | 9 | 准备阶段就投递；每帧不设上限；结束后仍积压；积压不设上限；主动离开 / 超时离开 / 加入不产生事实；不采纳新的区域占用；已交付的事实不出队 |
| 视图与建房 | 12 | 每帧都带视图；确认后不记版本；未确认的交付换用最新视图；编码不设上限、不排序；解码接受没有值的变量、未知字段；编译不带业务能力；不校验玩家刚体；首帧不带 `Program`；视图编码出错不算流程失败；两种失败共用指标 |
| T8 提交 handler | 9 | 流程出错被吞掉；handler 不用 undo；作废回执当成错误；结束后继续投递事实；状态没变也交回结果；不推进定时器；首帧不启动流程；不给房间建流程组件；没有流程的房间收下事实 |
| T9 视图推送 | 6 | 视图排在帧之后；推送失败被忽略；用 Sync 载荷；没配消息号也推；消息号可以重复；入口不把消息号交给中继 |
| T10 与示例进程 | 5 | 执行器不执行重生（方案指定）；不记检查点；示例进程开房不带流程 / 不带区域；回执整段不处理 |

两轮变异之后实现只再改过一处（下面第一条的仪表计数），它不在任何一条变异替换的位置上；65 条没有在那之后重跑。当时本机负载很高（另有会话在跑整套生成器测试），而服务层用例里有对时间敏感的断言，重跑容易出与变异无关的红，所以没有重跑。

变异过程中发现并修掉的问题：

- **实现缺陷：在途业务动作数的仪表可能停在过时的值上。** 第一轮里一条与业务动作无关的变异（视图解码）运行时，`TestBusinessActionsNeverBlockThePhysicsLoop` 也失败了一次：停房后 `depth:actions_inflight` 是 1 而不是 0。原因是计数用原子量减一、随后各自上报，两个同时结束的动作可能后减的先报。单独重复这个用例 40 次复现 1 次；改成计数与上报在同一把锁里之后重复 120 次全过。
- **用例缺口两处**（实现没有改）：视图解码“拒绝未知字段”只测了 varint 线类型，去掉字段号上限后长度前缀类型的未知字段会被悄悄接受而用例不红，补了两条；“没配消息号不推视图”只统计了特定消息号的推送，改成断言只出现帧。
- **测试自己的问题三处**：提交替身的 `once` 在通道被读走后会再触发一次（同一份结果交回两遍）；T10 起初一读到结算视图就断开，而同一次交付的那一帧还在途，服务端每次都记一条“出站未确认”的 WARN，改成把那一帧读完；批 2 的一条用例拿 `ObstacleLevel()` 当“没有流程的关卡”，示例关卡带上流程后前提不成立，改成显式清空。

### 没有验到的

- 只在 macOS ARM64 上跑过；Linux 没有跑。
- `scripts/physics/verify-service.sh` 整个脚本已在最终代码上跑过一遍并通过（见上表），但是在别的会话占着机器、负载 31 的情况下跑的，耗时不能当作基线。
- C# 客户端还不认 RP3、`ViewTeleported` 与 `RoomLevelView`（批 4）。金样 `LevelView` 已放进共享文件，C# 侧还没有读它。本批之后分支上的服务端与仓库里的 Unity 客户端仍不能互通。
- 三进程（`physicsroom-local` 的 physicsroom / game / gate 各自起进程，带 etcd）没有跑；T10 的 Gate、GameIngress、房间 owner 都在测试进程内，用的是正式部件与真实 NATS / TCP。三进程演练在批 5。
- 示例关卡 `ObstacleLevel` 的流程只验证了能校验、编译、导出再读回，以及示例进程开房时带上它；没有人在这张图上实际走过检查点与终点（T8、T10 用的是测试里的窄道关卡）。
- 业务动作只用替身执行者测过，没有接真实的 Game 侧结算 / 发奖 handler；“进程崩溃后由业务账本按动作 ID 了结”没有演练。
- 流程带来的每 tick 开销、`Teleports` 与视图对载荷的影响没有测量（批 5 的性能对照）。
- 现行文档与 `CHANGELOG.md` 没有更新（批 5）：`docs/framework/PHYSICS-ROOM-SERVICE.md` 里的 `Commit` 签名、`Spec` 字段、消息号表目前与代码不一致。

## 批 4：C# 客户端认 RP3、传送标记与流程视图

### 批 3 留下的一项：`codegen/internal/roost`

批 3 提交时这个慢套件按 `-timeout 60m` 的重跑还没有结束。它在 3404s 后以 3 个用例失败结束（不是超时）：`TestModulePortalInstallsOnALobbyProjectWithoutGame`、`TestModulePortalInstallsOnTheHostedAccountService`、`TestModuleAddDeclaresTheFrameworkServicesItUses`，失败文本都是“再次 `project sync` 之后工程变了”。

| 在哪个提交上单独跑这三条 | 结果 |
| --- | --- |
| `51974e4d`（批 3），冻结的临时检出 | 3 条都失败，退出码 1 |
| `2d15a1e5`（分支基线） | 3 条都失败，退出码 1，失败文本相同 |
| 当时的 main 顶端（不在本分支的历史里，提交号见原始输出的 `main=` 一行） | 通过，94s |

结论：这三条在分支基线上就失败，基线之后已经在 main 上修掉；本分支没有改 `codegen/`。本分支没有变基（基线是维护者定的 `2d15a1e5`），所以**在本分支上全仓 `go test ./...` 仍有这三条失败**，合入或变基到 main 之后才会消失；这一点没有在变基后的代码上验证过。原始输出在 [mutations-batch4.txt](mutations-batch4.txt) 末尾。

### 改动

| 文件 | 内容 |
| --- | --- |
| `client/physics-viewer/PackedBodyCodec.cs` | 状态字节接受 bit3，读成 `State.Teleported`；更高的位仍拒绝，轨迹位仍要求仅显示位 |
| `client/physics-viewer/ChunkReplica.cs` | 房间帧头只认 RP3（RP2 在帧头拒绝）；`BodyState.Teleported` |
| `client/physics-viewer/ReplicationPlayback.cs` | `SparsePosePlayback.Sample` 多一个参数 `discontinuity`：丢掉已有样本，从新位置重新开始 |
| `client/physics-viewer/InputPrediction.cs` | `Reconcile` 多一个参数 `discontinuity`：没有更新的输入可重放时不再拿已确认的输入外推；下一帧恢复 |
| `client/physics-viewer/RoomLevelView.cs`（新） | `RoomLevelView` 的严格解码（与 Go 的 `DecodeLevelView` 同样的拒绝规则，另外拒绝非法 UTF-8）；`Advance`：版本相同保留手上的，变小按协议错误 |
| `client/physics-viewer/LevelMarkers.cs`（新） | 从关卡 JSON 读区域与出生点。Unity 的 `JsonUtility` 读不了数组的数组（`Spawn.Positions`），所以带一个只够读关卡文档的 JSON 读取器 |
| `client/physics-viewer/CrowdViewer.cs` | 收 `0x50485904`（推送、Protobuf）；带标记的对象直接放置；自己的刚体带标记时按不连续纠偏；HUD 显示阶段、结果、各阶段及其代数、公开变量；`Teleports`、`Level` 两个只读属性 |
| `client/physics-viewer/LevelViewer.cs` | 关卡只认版本 2；区域画橙色线框，出生点画半透明小球（缺省出生点绿色），都没有碰撞体 |
| `client/physics-viewer/tests/` | `LevelTests.cs`（5 组新用例）；`UnityShim.cs`（`UnityEngine.Vector3` / `Quaternion` 的最小替身，让 dotnet 测试编译并运行真正的 `ChunkReplica.cs`）；`packed.json` 新增 `Level` |
| `gameplay/physics/level_test.go` | `TestLevelMarkersGoldenSharedWithCSharp`：钉住 `packed.json` 的 `Level` 就是 `Content()` 的字节，并断言 C# 要读出的值 |
| `scripts/physics/create-viewer.sh` | 多复制 `GateTransport.cs`。**这是基线上就有的问题**：`Packet.cs` 自提交 `d9ad1d3a` 起引用 `IGateTransport`，脚本没有跟着改，新建的工程编译不过 |
| `service/physicsroom/level_view.go` | 只改注释：客户端对相同 / 更小版本的处理 |

### 与方案文字的出入

- **“丢弃该帧 `InputSequence` 及以前的历史”本来就是 `Reconcile` 的行为**（`PredictionHistory.Acknowledge`）。传送帧新增的只有一件事：没有更新的输入可重放时不外推。实现与用例按这个写。
- **版本相同的视图不报错。** 方案写“拒绝 revision 回退”；实现里更小的拒绝、相同的保留手上的。同一条连接上服务端每个版本只推一次，相同版本不该出现，出现了也无害。
- **金样 `Level` 是 Go 导出的字节**，不像 `LevelView` 那样独立手工编码：这个用例要证明的就是“C# 读得了 Go 写出来的关卡”。
- **`UnityShim.cs`** 是方案没有提的测试手段。它只替身两个值类型；Unity 下的行为由批处理编译与批 5 的联机演练覆盖。

### 验证

| 命令 | 结果 |
| --- | --- |
| `dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj` | 8 组 PASS（原有 3 组 + 新 5 组），退出码 0；`dotnet build` 无警告 |
| `GOWORK=off CGO_ENABLED=1 go test -count=1 -race -tags physx ./gameplay/physics ./service/physicsroom` | ok（6.9s、10.0s） |
| `GOWORK=off go build ./...`、`go vet`（上面两个包）、`gofmt -l gameplay service` | 退出码 0，无输出 |
| `TMPDIR=/tmp/rpx scripts/physics/create-viewer.sh <新目录>`（Unity 6000.3.12f1，`-batchmode -nographics -quit`） | 退出码 0，11s：全部脚本编译通过，生成 `PhysicsComparison` / `Crowd` / `Room` 三个场景，日志有 `ROOST_VIEWER_SCENE_READY`。警告 2 条，都在 `GateTransport.cs`（可空注解），不在本批的文件里 |
| `GOWORK=off go test -count=1 .`（根包门禁，证据写入之后） | ok，15.8s |

`TMPDIR=/tmp/rpx` 只和我跑命令的环境有关：那个环境的临时目录路径很长，Unity 的构建工具在里面建 unix 套接字时超过了路径长度上限（`ArgumentOutOfRangeException: ... invalid length for use with domain socket`）。在普通终端里不需要。

新用例（T11）：

| 组 | 断言的内容 |
| --- | --- |
| room frame | `RoomFull`、`RoomTeleport` 两个金样经真正的 `ChunkReplica` 解码，位置、速度、帧号、输入确认与 Go 用例断言的值相同；传送帧里刚体 1 带标记，没有随帧更新的刚体 2 保持旧的采样帧号；RP2、RP4 帧头拒绝；状态字节带未知位的整帧拒绝；传送帧的每个截断都被拒绝且副本不变 |
| playback discontinuity | 带标记的样本立即显示在新位置并保持；之后的样本从新位置继续插值，不回到旧位置；旧帧号的样本即使带标记也忽略。对照：不带标记时同样的样本会被插值 |
| prediction discontinuity | 传送帧之后不外推；之后按下的输入从新位置重放；下一帧恢复外推；传送帧确认之前的历史（含跳跃）不重放。对照：不带标记时已确认的输入会被外推 |
| level view | 金样解码出的版本、阶段、结果、两个阶段、四个变量（负整数、false、中文文本、零）；版本次序；34 种坏包；截断只会以 `InvalidDataException` 拒绝；恰好 16KiB 接受、多 1 字节拒绝 |
| level markers | 金样里两个区域（零旋转读成单位旋转、带旋转的一个）、两个出生点（两个位置的一个、带朝向的一个）、缺省出生点；没有标记的关卡；24 种坏文档；金样的每个截断都拒绝 |

### 变异（[mutations-batch4.txt](mutations-batch4.txt)）

共 38 条，在定稿后的代码上完整跑一遍，全部变红：帧与状态字节 6 条（含方案指定的“C# 忽略 bit3”）、回放 2 条、预测 3 条、流程视图 15 条、关卡标记 12 条。

第一轮有 2 条没有被发现，处理如下：

- **C12 视图接受未知字段号**：我写的变异没有效果。字段号大于 5 在 `Tag` 里已经被归零，`Decode` 里又检查了一次，两处任意去掉一处行为都不变。把 `Tag` 里的那一处改成只防整数溢出，范围检查只留在调用方；变异改成“像普通 Protobuf 解码器那样跳过未知字段”，变红。
- **C36 嵌套深度不设上限**：用例写错了。用来测深度的文档顶层是数组，它是因为“顶层不是对象”被拒的。改成把深层嵌套放在对象的一个字段里，并加一条“限度以内接受”的对照，变红。

### 没有验到的

- 只在 macOS ARM64 上跑过。
- **`CrowdViewer.cs` 与 `LevelViewer.cs` 里的接线没有被任何自动用例执行过**：把传送标记交给回放与预测、收 `0x50485904`、HUD、区域线框与出生点小球。它们只经过了 Unity 的编译。实际在 Play Mode 里连上带流程的房间、看到重生与结算，留给批 5 的联机演练。
- Unity 的 `JsonUtility` 读 schema 2 关卡（多出 `Regions` / `Spawns` / `Flow` 等它不认识的字段）没有实际跑过，同上。
- dotnet 测试里 `ChunkReplica` 用的是替身类型，不是 Unity 的 `Vector3` / `Quaternion`。
- 没有跑 `scripts/physics/verify-viewer.sh`（PhysX 与 Unity 的对照验收）：本批没有改它覆盖的 RC1 路径。
- 现行文档（`client/physics-viewer/README.md` 等）没有更新（批 5）。


## 批 5a：流程的动作账本只留未完成的动作（方案之外，维护者 2026-11-19 选“只给物理房间加可选淘汰”）

### 起因

批 5 的长稳测量（`TestPhysicsRoomSoak`，8 房间 × 20 逻辑客户端，流程开：32 个区域、每客户端约每 5～6 秒重生一次）发现两件事：

- **提交耗时随时间线性上涨。** 流程开时提交 p50 两分钟里从 0.066ms 涨到 0.737ms；流程关时一直是 0.05～0.06ms。
- **房间在约 330 秒时停止**（`physicsroom: room missing or stale`）。到 315 秒时每房间累计重生约 940 次。停止前提交 p99 到过 137ms，日志里有 368 行 `nest handler held entity locks beyond threshold`（门槛 100ms）与 200～300ms 的 `slow dispatch`，物理均频掉到 35～49Hz。原始数据 [ledger-before-go.tsv](../../../performance/benchmarks/physx-level-actions-20261119/ledger-before-go.tsv)、[ledger-before-tail.txt](../../../performance/benchmarks/physx-level-actions-20261119/ledger-before-tail.txt)。

原因在 `gameplay/level`：动作账本（`State.Effects`）不淘汰，完成了的动作也留着并计入 `Limits.Effects`（缺省 1024，硬上限 4096）。这是 `LevelComponent` 成文的契约（[LEVEL-COMPONENT §预算与验证范围](../../../framework/LEVEL-COMPONENT.md#预算与验证范围)：“动作账本不淘汰以复用 ID，满额拒绝”），不是本轮引入的缺陷。但物理房间里每次重生就是一个动作：账本填满之后下一次 `request` 得到 `ErrBudget`，提交失败，房间按 D4 停止；每次写操作又要复制并校验整份状态（一笔提交里每条回执、每条事实各一次），所以成本跟着账本一起涨。20 人的房间每人每分钟掉一次，一小时就是 1200 次。

方案的风险表只写了“UGC 流程在运行期撞预算导致停房”，没有预见到这是按实例一生累计的上限。向维护者报告后给了三个选项（只给物理房间加可选淘汰 / 改 `LevelComponent` 的缺省 / 不改只记限制），维护者选第一个。

### 改动

| 文件 | 内容 |
| --- | --- |
| `gameplay/level/component.go` | `LevelComponent.ForgetSettledActions()`：打开后，每次变更在 `drain`（`action.done` 已分派）之后、校验之前，从草稿里删掉不是 pending 的动作。缺省不打开，行为不变 |
| `gameplay/level/state.go` | `Effect.Status` 的注释 |
| `wiring/physicsroom/entity.go` | 房间的流程组件建好后调用 `ForgetSettledActions()` |
| `gameplay/level/ledger_test.go`（新） | 三条用例，见下 |
| `wiring/physicsroom/entity_test.go` | `TestRoomFlowLedgerHoldsOnlyUnsettledActions` |
| `wiring/physicsroom/load_native_test.go` | 压测夹具加“流程开”场景（`flow`：机关场景 + 32 个区域 + 东西两侧最外一列的格子会重生），`ROOST_PHYSICS_SOAK_FLOW=1` 让长稳用它；报告加提交 p50、每秒事实数、每客户端每秒重生次数，长稳 TSV 末尾追加同样三列。发现与验证这个问题用的就是它 |
| `docs/framework/LEVEL-COMPONENT.md` | 预算一节写明缺省行为与 `ForgetSettledActions()` 的语义和代价 |

语义上的代价：打开之后，对已经结算的动作再给一次回执，不论结论是否相同都是 `ErrStale`（缺省是“同结论幂等成功、不同结论 `ErrConflict`”）。物理房间对每个动作只给一次回执（`service/physicsroom` 的 `flow.known` 在回执提交时才清），提交 handler 本来就把 `ErrStale` 记成作废回执，所以对它没有影响；失去的是“同一个动作先后给出两种结论”的检测。动作 ID 由 `NextEffect` 递增生成，淘汰不动它，ID 不复用。

### 验证

```
GOWORK=off go vet ./gameplay/level ./wiring/physicsroom                                        # 通过
GOWORK=off CGO_ENABLED=1 go test -count=1 -tags physx ./gameplay/... ./service/physicsroom/... ./wiring/physicsroom/...   # 全部 ok
```

| 用例 | 断言 |
| --- | --- |
| `TestSettledActionsStayInTheLedgerByDefault` | 缺省：同时最多 2 个动作的流程，两个都完成后账本仍有 2 项，第三个 `request` 是 `ErrBudget`；同结论的重复回执幂等 |
| `TestForgetSettledActionsKeepsOnlyPendingOnes` | 打开后连续 10 轮“请求 → 回执”：每轮账本回到空、`NextEffect` 递增、ID 互不相同、`action.done` 触发器每轮都执行；已淘汰动作的重复回执（两种结论）都是 `ErrStale` 且状态不变；两个 pending 之后第三个仍是 `ErrBudget`，完成一个腾出一个；阶段结束取消的动作当次离开账本，它晚到的回执是 `ErrStale` |
| `TestForgettingRollsBackWithTheTransaction` | 同一个 handler 里先完成动作、后面出错：账本回到提交之前（动作还是 pending，`action.done` 的效果也撤回），重试成功 |
| `TestRoomFlowLedgerHoldsOnlyUnsettledActions` | 房间 Entity + 真实提交 handler：`Limits.Effects = 2` 的流程重生 12 次都能提交，流程状态里没有已完成的动作；重复的回执记成 1 条作废，不算错 |

修后的长稳（同一夹具、同一负载，8 分钟，带进程外的 footprint / vmmap 采样）：

| 量 | 修前 | 修后 |
| --- | --- | --- |
| 结果 | 约 330s 房间停止 | 480s 通过，每房间累计重生约 1630 次（超过 1024） |
| 提交 p50 | 0.115ms → 2.6ms（线性上涨） | 0.056～0.093ms，没有随时间的趋势 |
| 提交 p99 最大窗口 | 137ms | 5.7ms |
| `nest handler held entity locks` WARN | 368 行 | 0 行 |
| goroutine | — | 全程 206 |
| 估算原生内存 / malloc 已分配 | — | 13.84 → 13.88MB / 9.62 → 9.65MB，斜率 +0.003MB/分钟、+0.007MB/分钟 |
| Go heap in-use 斜率 | — | −0.08MB/分钟 |

原始数据：[soak8-flow-go.tsv](../../../performance/benchmarks/physx-level-actions-20261119/soak8-flow-go.tsv)、[soak8-flow-proc.tsv](../../../performance/benchmarks/physx-level-actions-20261119/soak8-flow-proc.tsv)、[soak8-flow-summary.txt](../../../performance/benchmarks/physx-level-actions-20261119/soak8-flow-summary.txt)（预热取 120 秒）。

**这两次长稳期间机器同时被别的会话占用**（修后那次 1 分钟负载 13～32，开跑前 swap 已用 12.3GB / 13GB）：修后的物理均频稳定窗口平均 48.9Hz、最低窗口 44.4Hz，Step p99 平均 12ms，这些受干扰的数不能当作容量或频率结论；修前那次物理均频掉到 35Hz 则既有干扰也有账本的成本，两者没有分开。与账本有关的结论（是否停房、提交 p50 的趋势、WARN 行数、内存与 goroutine 斜率）不依赖机器是否空闲。

### 变异（[mutations-batch5a.txt](mutations-batch5a.txt)）

8 条，全部变红：不执行淘汰、连 pending 也淘汰、只淘汰 done 不淘汰 cancelled、缺省也淘汰、淘汰放在 `drain` 之前、淘汰直接改已提交的状态而不是草稿、物理房间不打开淘汰、淘汰时把 `NextEffect` 归零。

“缺省也淘汰”那一条除了本批的缺省用例，还让既有的 `TestTwoConfiguredConsumersUseTheSameComponent` 变红，说明既有用例确实依赖缺省契约。

### 没有验到的

- 只在 macOS ARM64 上跑过。
- 持久化的流程（`NewLevelDAO`）打开淘汰没有专门的用例：淘汰只是从要写入的状态里少了几项，走的是原有的整状态写入；物理房间用的是瞬态 DAO。
- 业务动作（`ActionHandler`，独立 goroutine、同 ID 重试）在打开淘汰后的晚到结果没有新增用例；批 3 的 `TestUnknownBusinessResultIsRetriedWithTheSameID` 等仍然通过，房间侧在回执提交前不会对同一个 ID 给第二个结论。
- 长稳只跑了 8 分钟、每客户端约每 6 秒重生一次这一种负载。

## 批 5：三进程联机验收、性能对照、现行文档

### 改动

| 文件 | 内容 |
| --- | --- |
| `scripts/physics/level-drill/main.go`（新） | 联机演练的 Go 客户端：以 `physics-slot-N` 经真实 Gate 登录，内容握手取关卡并核对摘要，只发输入、只看收到的包，在示例关卡上走“过检查点 A → 走出地面掉落 → 重生到检查点 A → 到终点结算”，逐项核对 |
| `scripts/physics/verify-level.sh`（新） | 起 `physicsroom-local` 的 physicsroom / game / gate 三个进程（game 用 `export-level` 导出的示例关卡），跑演练客户端，再依次 SIGTERM gate、game、physicsroom；要求三个进程退出码都是 0、日志没有 WARN / ERROR。给一个 Unity 工程路径时客户端换成 Unity 批处理 Play Mode。NATS 与 etcd 地址由环境变量给，键前缀与 subject 前缀带本次运行的进程号 |
| `client/physics-viewer/Editor/ViewerProject.cs` | `ViewerProject.VerifyLevel`：1 号槽、`UseGate`，自动走出地面，要求收到流程视图、自己被传送、`global/falls ≥ 1`、读出区域与出生点 |
| `client/physics-viewer/CrowdViewer.cs`、`LevelViewer.cs` | 只读属性 `LevelHud`、`RegionMarkers`、`SpawnMarkers`，给上面的验收读 |
| `docs/framework/PHYSICS-ROOM-SERVICE.md` | 新增“关卡流程”一节（调用链、关卡字段、事件与动作、业务动作、客户端、预算与停止、成本）；调用链、装配步骤、指标、UGC 一节里 schema / RP / ABI 的过时表述改正 |
| `docs/framework/LEVEL-COMPONENT.md` | 写明物理房间是已有的真实宿主 |
| `client/physics-viewer/README.md` | 新增“关卡流程：重生、检查点与 HUD”与批处理联机验收；RP2 → RP3；依赖里去掉 Redis |
| `docs/maintenance/KNOWN-LIMITS.md` | “没有物理动作执行器，联合玩法未实现”换成已实现后的边界一条；UGC 一节的协议版本；遗留里加外部验收 |
| `docs/maintenance/evidence/known-limits-remaining-20261114/external.md` | C-U3、C-L7 两行补关卡流程要验的内容与入口 |
| `docs/maintenance/FEATURE-2026-11-19-physx-level-actions.md` | 状态改为已在分支上实施完 |
| `CHANGELOG.md` | Unreleased 一条，含升级步骤 |
| `docs/performance/benchmarks/physx-level-actions-20261119/` | 交替测量与长稳的脚本、原始数据、汇总、说明 |

### 与方案文字的出入

- **“从进入掉落区到收到 bit3 帧的 tick 数”不能直接量。** 进入掉落区到重生只隔一两个物理步，比下发间隔（2.5 步）还短，客户端通常收不到“球在区域里”的那一帧。演练客户端改用重生前最后一帧按抛体推算球心到达区域顶面的时刻，要求带标记的帧不早于它、也不晚于它 10 步以上。实测 1.6～3.0 步（8 次）。
- **“Unity batchmode 编译 + EditMode 跑编解码金样”。** 编解码金样仍由 dotnet 跑（批 4 的 8 组）；Unity 工程没有装 Test Framework，没有 EditMode 用例。本批多做的是方案留给维护者的那一项里能自动化的部分：Unity 批处理 Play Mode 实际连上三进程（见下）。它不看画面。
- **“每客户端约每 5s 掉落重生一次”。** 夹具里逻辑客户端的输入是缓慢转向的固定加速度，重生频率靠摆放会重生的区域来调：5 秒短测里约每 3.8 秒一次，长稳里约每 5.9 秒一次。
- **“写进 EXTERNAL-VERIFICATION”。** 那个文件是服务端 E1～E28 的清单；Unity 与 Linux 物理的外部项在[外部清单](../known-limits-remaining-20261114/external.md)的 C-U3、C-L7，写在了那里，KNOWN-LIMITS 的遗留一节指过去。
- **改前的二进制。** 方案写与 `ba158535` 对照。那个提交的原生库是 ABI 2，本机没有现成的：用同一份 PhysX 静态库与该提交的 `bridge.cpp` 另链了一个。
- **批 5a** 整个是方案之外的，见上一节。

### 验证

三进程联机（私有 NATS `127.0.0.1:14377`、私有 etcd `127.0.0.1:14379`，Gate 用入口写死的 `127.0.0.1:19093`）：

```
ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14377 ROOST_PHYSICS_TEST_ETCD=127.0.0.1:14379 scripts/physics/verify-level.sh
LEVEL_DRILL_PASS slot=1 body=1 frames=204 views=7 checkpoint_tick=267 left_ground_tick=381 last_seen_falling_tick=411 last_seen_y=-3.93
  kill_entry_tick_estimate=411.4 respawn_frame_tick=414 ticks_from_entry_to_respawn_frame=2.6 finish_tick=516 phase=finishing result=clear falls=1 revision=13
gate exit=0
game exit=0
physicsroom exit=0
```

- 定稿后连续 5 次、2 号槽 1 次、批 5a 之后在最终代码上再 2 次，共 8 次全部通过。每次约 18 秒。
- 客户端核对的内容：鉴权应答；内容握手的 SHA-256；第一帧之前先收到流程视图，视图的版本号严格递增、是普通推送；帧是 Sync 推送；沿路线到达检查点 A；离开地面后 5 秒内恰好收到一帧带传送标记，这一帧里球在检查点 A 自己槽位的出生点 0.3m 以内、速度不超过 1.5 m/s，并且此时 `global/falls` 已经是 1；之后到终点，视图是 `finishing / clear`，结束时球在终点区域里，`falls` 仍是 1。
- 服务端核对的内容：三个进程收到 SIGTERM 后都以 0 退出；三份日志里没有 `level=WARN`、`level=ERROR`、`panic:`。

演练客户端自己有两处判定写错过，第一次运行时暴露，改的都是客户端的假设，服务端行为没有改：

1. 要求带标记的那一帧速度不超过 0.5 m/s。实际是 0.6～1.2 m/s：传送时速度清零，但这一帧是传送之后 1～3 个物理步才采样的，重力与仍然生效的输入已经作用了几步。门槛改成 1.5（下落 4m 以上的速度超过 9 m/s，足以区分）。
2. 等“球心低于 −4”再认为进入了掉落区。这一帧通常不存在（见上面的出入第一条），于是传送帧到的时候客户端还以为在走向边缘。改成离开地面就进入“下落”状态，并用抛体推算进入时刻。

Unity 客户端（Unity 6000.3.12f1，`-batchmode -nographics`，工程由 `create-viewer.sh` 用本批源码新建）：

```
scripts/physics/verify-level.sh <Unity 工程>
ROOST_VIEWER_TCP_PHYSICS_PASS frames=200 tick=525 bodies=5 bytes=43796 teleports=1 regions=4 spawns=6 travel=31.5 hud=[flow: running | stages: run#1 | revision 6 | global/falls=1]
gate exit=0 / game exit=0 / physicsroom exit=0
```

3 次通过（定稿 1 次、变异恢复后 1 次、最终代码 1 次），每次约 30 秒。这关掉了批 4 留下的两项：`CrowdViewer` / `LevelViewer` 里的接线（传送标记交给回放与预测、收 `0x50485904`、HUD 文字、区域与出生点标记）在 Play Mode 里实际执行了；Unity 的 `JsonUtility` 读 schema 2 关卡（多出它不认识的字段）实际跑过。Unity 日志里仍有编辑器自己的 QuickSearch 异常（`SearchDatabase.EnumerateAll`，与工程无关，`client/physics-viewer/README.md` 早有记录）。这条验收只到“球掉下去、被重生、HUD 文字里 `falls=1`”，没有走到终点。

性能对照与长稳：见[基准目录](../../../performance/benchmarks/physx-level-actions-20261119/README.md)。对照方案 6.3 的三条判定：

| 方案的判定 | 结果 |
| --- | --- |
| 流程开相对关，物理均频下降不超过 1% | 达到：1 / 4 / 8 房间交替 3 轮的中位数差 −0.4% / 0% / 0% |
| 流程开 8 分钟长稳，没有新的无界增长 | 批 5a 之前不成立（约 330 秒停房）；批 5a 之后达到：goroutine 不变、原生内存斜率每分钟不到 0.01MB、Go 堆与提交 p50 没有上升趋势，每房间重生约 1630 次 |
| 流程关与改前（`ba158535`）对照不回退 | 达到：分配 +0.1%～+0.5%、载荷持平，在轮次波动以内；长稳的内存与 goroutine 与改前相同 |

分配与载荷“如实报”：流程开时 Go 分配 +21%～+22%，每客户端下行载荷 +16%～+18.5%。**测量期间机器被别的会话占用**（1 分钟负载 7～32），Step / 提交 p99 与长稳里的物理均频带着噪声，不作结论；基准目录的说明逐条标了哪些结论受影响。

最终代码上的其余检查：

```
gofmt -l gameplay service wiring scripts infra client                               # 无输出
GOWORK=off go build ./...                                                           # 通过
GOWORK=off go vet（带与不带 -tags physx）./gameplay/... ./service/physicsroom/... ./wiring/physicsroom/... ./scripts/... ./infra/physics/...   # 通过
GOWORK=off CGO_ENABLED=1 go test -count=1 -race -tags physx ./gameplay/level ./wiring/physicsroom ./service/physicsroom   # ok（批 5a 提交前；之后这三个包没有再改）
ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14377 scripts/physics/verify-service.sh   # 通过，55s
dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj             # 8 组 PASS
GOWORK=off go test -count=1 .                                                       # 根包门禁 ok
GOWORK=off go test -count=1 <除 codegen/internal/roost 以外的全部包>                    # 171 个包 ok，1 个包失败一次，见下
```

全仓默认测试里失败的一个：`client/relay` 的 `TestTransferHandsTheHandoffToTheNewHost`（`unexpected game frames dropped during the migration`）。本分支没有改 `client/relay`（相对分支基线没有差异）；单独重跑 20 次、整包重跑 1 次都通过。当时机器在高负载下同时跑 170 多个包，判断是对调度敏感，**没有进一步定位**，不计为本分支的问题，也不算全仓通过。`codegen/internal/roost` 没有跑：它在分支基线上就有 3 个用例失败（批 4 一节开头）。

### 变异（[mutations-batch5.txt](mutations-batch5.txt)）

8 条，全部变红。

三进程 + Go 客户端 5 条：房间执行器不执行重生（5 秒内没有重生）；示例进程不配置流程视图的消息号（第一帧之前没有视图）；不记检查点（传送帧里球在起点而不是检查点 A）；示例进程开房不带流程（没有视图）；房间里多打一条 WARN（演练本身通过，日志检查失败——证明日志检查是生效的）。

三进程 + Unity 客户端 3 条：服务端不执行重生（`teleports=0`，60 秒超时）；Unity 工程里的 `ChunkReplica` 忽略 bit3（`teleports=0`）；观察器不收 `0x50485904`（`Unexpected crowd packet`）。后两条改的是 Unity 工程里的副本，证明验收执行的确实是 Unity 里编译的那份代码。恢复后再跑一次通过。

### 没有验到的

- **只在 macOS ARM64 上跑过。** Linux 上没有重建 ABI 3 的原生库，没有跑任何一条；归外部清单 C-L7。
- **Unity 的画面与手感没有人看过。** 批处理验收用 `-nographics`，没有截图；重生时画面是否干净、重生后立刻移动的手感、HUD 是否好读、抓着或被抓着时被传送的表现、双客户端互相看、弱网、移动设备，都没有验；归 C-U3。
- Unity 验收没有走到终点，结算时的 HUD 文字（`flow: finishing result=clear …`）没有在 Unity 里出现过；结算视图的协议内容只由 Go 客户端核对。
- 三进程演练只有一个客户端在房间里；两个真实客户端同房（互相的传送标记、同一出生点错开位置）只有批 3 的进程内用例覆盖。
- 性能只测了一种流程负载、一台被占用的机器；没有带 Gate / NATS / TCP 的流程成本，没有 20 分钟以上的长稳。
- `client/relay` 那一次失败没有定位（见上）。
- 没有重跑 `scripts/physics/verify-viewer.sh`（PhysX 与 Unity 的数值对照）：本轮没有改它覆盖的 RC1 路径与对照宿主。
