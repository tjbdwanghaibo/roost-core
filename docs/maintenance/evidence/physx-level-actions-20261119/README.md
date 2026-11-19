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

`codegen/internal/roost` 是仓库里已知的慢套件：`scripts/pretag.sh` 为它写了 `-timeout 60m`，注释里记着它在本机单独跑要 300～800s、随磁盘负载波动，全量跑时出现过同样的“timed out after 10m0s 且没有失败用例”。这个包与 `codegen/` 下的模板都不引用 `service/physicsroom`、`wiring/physicsroom`、`gameplay/physics`。按 `-timeout 60m` 单独重跑：到本批提交时已经跑了 42 分钟还没有结束（本机负载 30～45，另一个会话同时在它自己的目录里跑同一个套件），**结果未确认**；跑完后的结果补记在批 4 一节的开头。

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
