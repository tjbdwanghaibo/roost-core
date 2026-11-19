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
