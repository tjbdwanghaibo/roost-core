# PhysX 物理房间 + LevelComponent 联合玩法：方案

**状态**：只出方案，没有写实现；第 9 节的决定项定下后才动手（2026-11-19，基线 main `ba158535`）。来源：维护者 2026-11-19 对第三类第三批 3A 选 B“做一轮功能”（[证据 3A](evidence/category3-batch3-20261119/README.md#3a-physx-动作执行器与-levelcomponent-的真实联合玩法不是偶发是没有实现的集成)，KNOWN-LIMITS 第一部分“没有物理动作执行器，联合玩法未实现”）。实施完把结论写进现行文档（`docs/framework/PHYSICS-ROOM-SERVICE.md`、`docs/framework/LEVEL-COMPONENT.md`、`client/physics-viewer/README.md`、KNOWN-LIMITS），本文留作来源。

本文的代码事实按 HEAD `ba158535` 的源码核对（codebase-memory MCP 本会话连接失败，没有用图）。

## 1. 现状（2026-11-19 核对代码）

| 层 | 现有能力 | 缺口 |
| --- | --- | --- |
| native（`infra/physics/physx/native/bridge.h`、`bridge.cpp`） | `rp_create / rp_destroy / rp_add_pose / rp_step_control / rp_read / rp_joint_add / rp_joint_remove / rp_version / rp_abi_version`（ABI 2） | 没有直接设动态刚体位姿（传送 / 重生）；没有区域触发或任何事件回调 |
| Go 绑定（`infra/physics/physx/native.go`、`types.go`、`disabled.go`） | `World.Add / Step / AddJoint / RemoveJoint / Close`，`New` 要求 `rp_abi_version()==2` | 没有 `SetPose` |
| 关卡描述（`gameplay/physics/level.go`） | `Level` schema 1：`Bodies / Players / Motions / Joints`，`DecodeLevel` 拒绝未知字段，`Content()` 出 SHA-256 | 没有区域、出生点、流程定义 |
| 房间服务（`service/physicsroom/service.go`、`types.go`、`mechanisms.go`） | 每房一个执行 goroutine：`BeforeStep`（抓取）→ `Input` → `World.Step` → `Commit`（等 `RequestSettled`）→ 发布帧；提交失败停房 | `Commit` 只回 `error`，没有回流的位置；没有动作执行器 |
| 房间 Entity（`wiring/physicsroom/entity.go`、`nest.go`） | `RoomEntity` 只有 `gamephysics.StateDAO`；handler `physicsroom.entity.accept`（`RollbackUndo` + `DurabilityMemory`） | 没有挂 LevelComponent |
| 流程（`gameplay/level/component.go`） | `Initialize / Start / AdvanceTick / Handle / PendingActions / Complete / Close / Project`；能力名须是 identifier（不能带点），事件名可带点；回执重复无操作、取消后回执 `ErrStale` | 只产出动作请求，执行器由宿主提供；仓库里只有 dungeon 模块在用 |
| 同步（`gameplay/physics/quantized.go`、`replication.go`） | RP2 头 + RC2 字段增量；状态字节 bit0 休眠、bit1 展示档、bit2 轨迹档，解码拒绝 `d[33] > 7` | 没有“位置不连续”的表示，客户端会把传送当成高速移动插值 / 纠偏 |
| 客户端（`client/physics-viewer/*.cs`） | `PackedBodyCodec`、`ChunkReplica`、`InputPrediction.Reconcile`、`ReplicationPlayback`、`LevelViewer` | 不认识流程状态，没有 HUD |

`client/dotnet/Roost.Client`、`client/ts`、`client/cpp` 只搬运不透明消息，不含 RP / RC 编解码（`rg PackedBody client` 只命中 physics-viewer），本轮不改。codegen 不生成物理房间，生成器与 game-demo 不受影响。

## 2. 目标与范围

### 2.1 本轮要做

一句话：可信关卡流程（LevelComponent）在物理房间里能看到“谁进了 / 出了哪个区域、谁加入 / 离开”，能请求“传送、记检查点、重生到检查点”，物理房间在已提交之后执行并把回执交回流程；客户端看到不连续位置时不插值，并收到流程的公开状态。

| 类别 | 名称（流程里写的名字） | 参数 | 说明 |
| --- | --- | --- | --- |
| 事实（事件） | `region.enter` | `actor:int, region:string` | 刚体中心进入区域（OBB）；高速穿过薄区域时同一 tick 依次给 enter、leave |
| 事实（事件） | `region.leave` | `actor:int, region:string` | 离开时用 0.25m 滞回，避免边界抖动 |
| 事实（事件） | `member.join` / `member.leave` | `actor:int` | 成员加入 / 离开（含 MemberTTL 过期），供“全员到达终点”这类计数 |
| 动作 | `physics_teleport` | `actor:int, spawn:string` | 把一个动态刚体放到关卡里命名的出生点，速度清零；可用于玩家，也可用于复位机关箱子 |
| 动作 | `physics_checkpoint` | `actor:int, spawn:string` | 记下该玩家的检查点（只记，不移动） |
| 动作 | `physics_respawn` | `actor:int` | 传送到该玩家最近一次检查点；没有检查点时用关卡的缺省出生点 |
| 业务动作 | 业务注册的名字（如 `settle`） | 业务声明 | 不在物理执行器上跑：交给业务 `ActionHandler`，按同一回执契约回来（见 4.5） |
| 下行 | 流程公开视图 `RoomLevelView` | — | `Project()` 的结果，Revision 变化时下发一次；重连拿最新一份 |
| 下行 | 不连续标记 | — | 状态字节 bit3：本对象自接收方上次看到以来被传送过 |

`actor` 统一用房间内的刚体 ID（`< 2^63`，放得进 `int64`）。玩家 ID 到刚体的映射仍在业务的 `Seat`，结算时由业务换回玩家 ID。

### 2.2 明确不做

| 不做 | 原因 / 去向 |
| --- | --- |
| PhysX 原生 trigger / 接触事件、公开碰撞事件、Raycast API | 见 D2；区域在 Go 侧按已提交帧判定 |
| 流程直接写坐标传送、客户端请求传送 | 只允许关卡里命名的出生点；客户端只有输入，没有动作入口 |
| 运行期刚体增删、淘汰 / 观战、关卡在线编辑、网格碰撞 | 与本轮无关，保留在 KNOWN-LIMITS 的 UGC 边界里 |
| 参与者私有视图、排行榜、奖励到账链 | 奖励由业务 `ActionHandler` + Remote / Saga 做；本轮只给接口与契约测试（D5） |
| 持久关卡（`NewLevelDAO` + 正式持久事务） | 物理世界不可恢复，流程持久没有意义（D9）；只用 `NewTransientLevelDAO` |
| 物理跨机接管、多 owner 路由 | 不变 |
| Windows、PhysX 5 | 平台范围只 macOS / Linux；native 仍是 PhysX 4.1.2 |

## 3. native 侧

### 3.1 新增函数（ABI 2 → 3）

```c
// 只接受动态刚体：设全局位姿、线速度、角速度，清掉本步累积的力和力矩并唤醒。
// 静态、运动学（由 MotionTrack 驱动）或未知 id 返回 0。调用方保证不在 simulate 期间调用。
int rp_set_pose(rp_world *, uint64_t id, const float *position, const float *rotation,
                const float *velocity, const float *angular_velocity);
unsigned rp_abi_version(void); // 改为返回 3
```

| 项 | 内容 |
| --- | --- |
| 实现（`bridge.cpp`） | 在 `w->bodies` 找到 `PxRigidDynamic`，拒绝 `eKINEMATIC`；`setGlobalPose(pose, true)`、`setLinearVelocity`、`setAngularVelocity`、`clearForce` / `clearTorque`（`eFORCE` 与 `eACCELERATION` 两种都清）、`wakeUp()` |
| 数据布局 | 不新增结构体；`rp_command`、`rp_state`、`rp_joint` 布局不变，只多一个函数 |
| Go 绑定（`native.go`） | `World.SetPose(id uint64, p Vec3, q Quat, v, w Vec3) error`：与 `Step` 同一把 `w.mu`；校验有限值、单位四元数、`ids[id]==Dynamic`；`fctx.BlockingError` 守卫同其他方法。`New` 的版本检查改为 `!= 3`，旧库报 “native ABI mismatch; rebuild native library” |
| 无 tag（`disabled.go`） | 补同名桩，返回 `ErrUnavailable` |
| 关节 | Go 侧拒绝传送被**配置关节**（`Spec.Joints`）连着的刚体；抓取关节由房间先释放（4.4） |
| 重叠 | 不做原生空位查询；出生点给多个位置，按槽号取（D8） |

为什么不加区域事件：见 D2。只加这一个函数，native 改动约 30 行。

### 3.2 升级（先停旧再起新）

线上未部署，不兼容旧版本：ABI 3、Level schema 2、RP3 头、`Spec` / `Delivery` 字段都只认新版。CHANGELOG 写：**先停 Gate、Game、物理房间旧进程；`scripts/physics/build-native.sh` 重建 `libroost_physx`；重建 Go 程序（`-tags physx`）；同步更新 Unity 的全部 physics-viewer 源码；旧 schema 1 关卡 JSON 要补 `Version: 2` 与新字段后重新发布**。旧库被 `physx.New` 拒绝、旧客户端被 RP3 头拒绝、旧关卡被 `DecodeLevel` 拒绝，都是显式失败。

### 3.3 构建与测试

| 机器 | 做法 |
| --- | --- |
| 本机 macOS ARM64 | 沿用证据 3B：用 `/Users/whb/roost/.physics-deps/build` 里的 PhysX 4.1.2 静态库与新的 `bridge.cpp`，clang++ 链出 `infra/physics/physx/lib/libroost_physx.dylib`（gitignore，验收后删除）；或 `ROOST_PHYSICS_DEPS=/Users/whb/roost/.physics-deps scripts/physics/build-native.sh`。然后 `GOWORK=off CGO_ENABLED=1 go test -race -tags physx ./infra/physics/physx ./gameplay/physics ./service/physicsroom ./wiring/physicsroom` |
| 真实网络 | 私有 NATS（`scripts/mirror-local.sh` 起，或单个 brew `nats-server` 私有端口）：`ROOST_PHYSICS_TEST_NATS_URL=… scripts/physics/verify-service.sh`，脚本里补新用例名；根包 `integration_coverage_test.go` 第 878 行核对该脚本的包与用例名，要同步改 |
| Linux | 归 C-L（外部复测），不作本轮门槛；`build-native.sh` 的 Linux 路径不改 |

## 4. 服务端

### 4.1 流程放在哪：物理房间 owner 的 RoomEntity（D1）

LevelComponent 挂在 `wiring/physicsroom.RoomEntity` 上，和 `StateDAO` 同一个 Entity、同一个 Nest handler、同一笔 undo 事务。好处：区域事实和帧在同一笔提交里，没有跨进程回执、没有“结果未知”；房间提交失败停房的现有契约直接覆盖流程。Game 进程不碰流程状态，只在 `Create` 时把流程定义带过去。

### 4.2 数据与 API 变化（不新增包）

| 文件 | 改动 |
| --- | --- |
| `gameplay/physics/level.go` | `Level` schema 2：加 `Regions []Region`、`Spawns []Spawn`、`DefaultSpawn string`、`Flow json.RawMessage`（`gameplay/level.Definition` 的 JSON，`TimeDomain` 必须是 `tick`）。`Region{ID string; Center Vec3; HalfExtents Vec3; Rotation Quat; Watch []uint64}`（`Watch` 空 = 全部 `Players`）；`Spawn{ID string; Positions []Vec3; Rotation Quat}`。上限：区域 64、出生点 64、每个出生点 20 个位置、Flow 256KiB；坐标与尺寸沿用 1000m / 100m；ID 是 identifier。schema 1 拒绝 |
| `gameplay/physics/regions.go`（新文件） | `RegionTracker`：纯函数，输入上一已提交帧与本帧的状态、本 tick 被传送的刚体集合，输出有序事实与下一份“在区域内”集合。规则：进入用 OBB 判定；离开用扩 0.25m 的 OBB；两端都在外但线段穿过 OBB 时给 enter 再 leave；被传送的刚体只做终点判定，不做线段。次序：区域按关卡顺序、刚体按 ID |
| `infra/physics/physx/types.go` | `State` 加房间附加字段 `Teleports uint32`（同 `InputSequence`，不进 native、不上线） |
| `service/physicsroom/types.go` | `Spec` 加 `Players []uint64`、`Regions`、`Spawns`、`DefaultSpawn`、`Flow []byte`。`Commit` 改为 `func(ctx, Ref, Tick) (Settled, error)`：`Tick{Frame; Program *level.Program（只首帧非空）; Facts []level.Event; Receipts []level.Receipt}`，`Settled{Actions []level.Effect; View *level.View}`（Revision 未变时两者都为空）。`Config` 加 `Actions ActionHandler`（可空）。`Delivery` 加 `Level []byte`（编码后的 `RoomLevelView`，只在该成员未确认过这个 Revision 时带） |
| `service/physicsroom/actions.go`（新文件） | 物理能力注册表（`physics_teleport / physics_checkpoint / physics_respawn` 与 `region.* / member.*` 事件 schema）、执行器、检查点表、业务动作的异步派发 |
| `service/physicsroom/service.go` | `Create`：有 `Flow` 时用“物理注册表 + `cfg.Actions.Registry()`”`Compile`，失败答 `ErrRequestInvalid`；区域 / 出生点 / 流程里字面量出生点名逐个校验。`run`：顺序见 4.3 |
| `service/physicsroom/mechanisms.go` | `updateGrabs` 跳过本 tick 被传送、且 `viewFrame` 还早于传送 tick 的刚体（4.4） |
| `wiring/physicsroom/entity.go`、`nest.go` | `RoomEntity` 加 `level *level.LevelDAO`（transient）与 `flow *level.LevelComponent`；`FrameCommit` 改为携带 `Tick`；handler 顺序见 4.3；`EntityHost.Commit` 返回 `Settled`。`NestCommit` 同步改签名 |
| `wiring/physicsroom/players.go`、`relay.go` | `PlayerIngressConfig` 加 `LevelMessageID`（可选，非 0 且与其他消息号不同）；Relay 在推帧之前先推 `Delivery.Level` |
| `wiring/physicsroom/host_mod.go` | `NewEntityHostMod` 可选传入 `ActionHandler`；配置不加新键 |
| `wiring/physicsroom/cmd/physicsroom-local` | 示例关卡升 schema 2：加掉落区（kill）、两个检查点、终点与流程；新增消息号 `0x50485904`（流程视图） |
| `service/physicsroom/rooms_rpc_gen.go` | `go generate ./service/physicsroom` 重新生成，确认无手改 |

`Commit` 签名改动是 Go API 变化，写进 CHANGELOG；房间没有 `Flow` 时 `Tick` 只有 `Frame`，`Settled` 为空，行为与现在一样。

### 4.3 每 tick 的固定顺序

房间执行 goroutine（不是 Nest worker；物理不占 short / long / io 池）：

| 步 | 位置 | 做什么 |
| --- | --- | --- |
| 1 | `BeforeStep(N)` | 执行上一笔已提交返回的物理动作（按 Effect.Sequence 升序）：`SetPose`、记检查点、释放相关抓取、清该成员的 `jumpPending`，结果写入本 tick 的回执表；已执行过的 ID 只复用回执，不再动世界 |
| 2 | `BeforeStep(N)` | `updateGrabs`（跳过刚传送的刚体） |
| 3 | `Input(N)` | 运动学轨迹 + 成员输入（不变） |
| 4 | `World.Step` | 不变 |
| 5 | 执行 goroutine | `RegionTracker` 算事实；加入 / 离开事实从成员表的变更队列取；事实编号用房间内单调计数（同一 source `physics`，允许跳号） |
| 6 | Nest handler（`physicsroom.entity.accept`，undo + memory） | ① `StateDAO.Accept(frame)` ② 首帧：`Initialize(instance, epoch, 0)` + `Start(0)`，instance 用 `room-` + `ref.ID` 的 SHA-256 前 16 位十六进制（Room ID 可能不是 identifier），epoch 用 `Ref.Epoch` ③ 逐个 `Complete(receipt, tick)`，`ErrStale`（动作已被取消）记指标后忽略，不算失败 ④ 逐个 `Handle(fact, tick)` ⑤ `AdvanceTick(tick)` ⑥ Revision 变了才取 `PendingActions()` 副本与 `Project()` 返回 |
| 7 | 执行 goroutine | `RequestSettled` 成功后：采纳新动作表；新的业务动作交给 `ActionHandler`；`View` 编码成 `RoomLevelView` 存入房间；更新区域集合；发布帧 |

handler 返回的动作只在 `RequestSettled` 返回 nil 后使用，等价于 Level 手册第 4 步“settled 之后再取 PendingActions”，少一次查询。handler 内全是纯计算，不阻塞。

### 4.4 事务、回滚与失败

| 情况 | 处理 |
| --- | --- |
| handler 任一步出错（含流程预算 `ErrBudget`、冲突） | undo 同时撤回 `StateDAO` 与 `LevelDAO`；`Commit` 返回错误，房间按现有契约停止（D4） |
| 传送目标不合法（不是动态刚体、被配置关节连着、出生点不存在、`respawn` 的 actor 不是玩家） | 回执 `Success=false`（终态），流程收到 `action.done{success=false}` 自己决定；房间不停 |
| `SetPose` 返回失败（native 状态与 Go 账本不一致） | 当作 native 故障，停房，同 `Step` 失败 |
| 同一动作在回执提交前又出现在动作表里 | 执行器按 ID 去重，只复用已有回执，不二次传送 |
| 传送的刚体有抓取关节 | 先 `RemoveJoint` 释放它作为抓取者和被抓者的关节；在 `viewFrame.Tick` 追上传送 tick 之前不重新抓取（`updateGrabs` 按上一已提交帧量距离，否则会用传送前的位置） |
| 进程崩溃 | 房间与流程一起消失（瞬态）；已交给业务但未回执的动作由业务账本按动作 ID 去重（ID 含随机 Epoch，跨房间不重复） |

检查点表（`actor → spawn`）放在房间执行器内存（D3）：只由已提交的 `physics_checkpoint` 动作写，房间瞬态，不需要恢复。

### 4.5 业务动作（`ActionHandler`）

```go
type ActionHandler interface {
    Registry() level.Registry                       // 业务能力与业务事件的 schema，与物理注册表合并；重名拒绝
    Execute(ctx context.Context, room Ref, e level.Effect) (success bool, err error) // 必须按 e.ID 幂等
}
```

| 项 | 契约 |
| --- | --- |
| 在哪跑 | 每房一个有界派发（缺省同时在途 16 个），独立 goroutine，不在物理执行器、不在 Nest worker 上；`Execute` 可以阻塞（RPC 到 Game） |
| 超时 | 缺省 5s。超时或 `err != nil` 表示结果未知：**不伪造失败回执**，按同一 ID 退避重试，直到房间结束 |
| 回来 | 结果进下一 tick 的 `Tick.Receipts`；动作已被取消时 `Complete` 返回 `ErrStale`，忽略 |
| 停房 | 取消在途调用；已发出未回执的动作记指标与日志，由业务账本按 ID 了结 |
| 持久 | 物理房间自身只用 memory。业务在 Game 侧的到账 handler 用缺省 async；**如果 `Execute` 拿到回复就当完成、不再重投**（属于“回复后外部确认”），那个 handler 用 `durability=pipelined`，不用 strict |

### 4.6 确定性要求

- 流程只用 tick 时间域（`TimeDomain: "tick"`，`AdvanceTick(frame.Tick)`），不读墙钟。
- 给定同一串（帧状态、事实、回执），流程状态逐字节相同：事实次序固定（4.2 的 `RegionTracker` 次序 + 成员事实按刚体 ID）、动作按 Sequence 执行、出生点位置按 `actor 在 Players 里的下标 mod len(Positions)` 选。
- 不要求 PhysX 跨机器 / 跨运行逐位一致（单 owner 权威，与现状一致）。
- 用例用录下来的帧序列重放两遍，比对 `LevelComponent.Snapshot()` 的 JSON（5.1 的 T5）。

### 4.7 预算与指标

- 每 tick 事实上限 64，超出的留到下一 tick（按序），不丢。
- 没有事实、没有回执、计时器未到期时，handler 里流程只做一次 `NextDeadline` 比较，不写 DAO、不复制状态。
- `servicemetrics`：`Accepted/Refused("action", reason)`、`Dropped("receipt_stale")`、`Depth("actions_inflight")`、`Dropped("level_failed")`；不带 RoomID / PlayerID 标签。

## 5. 客户端

| 项 | 改动 | 文件 |
| --- | --- | --- |
| RP 头 | 版本 2 → 3（只有语义变化：状态字节可以带 bit3），旧客户端在帧头就显式拒绝，不等第一次传送 | `gameplay/physics/replication.go`、`PackedBodyCodec.cs`、`ChunkReplica.cs` |
| 不连续标记 | 状态字节 bit3 `ViewTeleported`。服务端视图账本 `ViewEntry` 记每个对象上次发给该接收方时的 `Teleports`；不一致时强制完整字段 + bit3（与“新可见 / 跨 Chunk”同一条刷新路径）。远档 5Hz 的接收方即使错过传送那一帧，下一次发送也带 bit3；pending 帧重发时冻结不变 | `gameplay/physics/replication.go`、`quantized.go`（解码上限 `> 7` 改为 `> 15`） |
| 远端表现 | bit3 → 直接放到新位置，丢弃插值缓冲 | `ReplicationPlayback.cs`、`CrowdViewer.cs` |
| 本地预测 | 自己的刚体带 bit3 → 以权威状态为基线，丢弃该帧 `InputSequence` 及以前的历史，只重放之后的输入；不做 250ms 外推 | `InputPrediction.cs`（`Reconcile` 加参数） |
| 流程视图 | 新 Protobuf `RoomLevelView{uint64 revision=1; string phase=2; string result=3; repeated Stage stages=4; repeated Variable variables=5}`，`Variable{string key=1; oneof value{int64 int=2; bool flag=3; string text=4}}`；解码拒绝未知字段与 revision 回退 | `client/physics-viewer/room_input.proto`、新增解码与简单 HUD（阶段、结果、公开变量） |
| 关卡显示 | `LevelViewer` 读 schema 2，画区域线框与出生点 | `LevelViewer.cs` |
| SDK | `Roost.Client`（C#）、`client/ts`、`client/cpp` 不改：它们只搬运消息 | — |

金样：Go 侧新增 RP3 / bit3 / `RoomLevelView` 金样，写进 `client/physics-viewer/tests/packed.json`（或新文件），C# 测试读同一份。

## 6. 验收

### 6.1 先红后绿

新增 API 在现有代码上没有可编译的红，所以红分两种：**行为红**（当前代码上可以写出、确实失败的用例）与**变异红**（绿之后逐条撤掉关键行，对应用例必须变红，记录变异与失败文本；做法同 [B-12 的 cellworld 变异](evidence/category3-batch3-20261119/README.md)）。每条用例写明是哪种。

| # | 用例（包，tag） | 承诺 | 红 |
| --- | --- | --- | --- |
| T1 | `infra/physics/physx`（physx）：`TestSetPoseMovesADynamicBodyAndClearsItsMotion` | 传送后下一步从新位姿出发、速度为零、休眠体被唤醒；静态 / 运动学 / 未知 ID / 已关闭拒绝；旧 ABI 被拒 | 变异：不清速度、不唤醒、不查运动学 |
| T2 | `gameplay/physics`：`TestLevelSchemaTwoRejectsBadRegionsSpawnsAndFlow` | schema 1、未知字段、越界、重名、非 tick 流程、未注册能力都拒绝 | 行为红：当前 `DecodeLevel` 对含 `Regions` 的 JSON 报未知字段（断言改为“schema 2 合法关卡被接受”时变红） |
| T3 | `gameplay/physics`：`TestRegionTrackerEntersLeavesAndCatchesFastCrossings` | 进入、滞回离开、高速穿过薄区域给 enter+leave、传送只判终点、次序稳定 | 变异：去掉线段判定、去掉滞回、传送仍做线段 |
| T4 | `gameplay/physics`：`TestTeleportMarkReachesEveryReceiverOnce` | 近 / 中 / 远三档接收方都在传送后的第一次发送里看到 bit3 + 完整字段，之后不再带；pending 重发冻结 | 变异：只在传送帧置位（远档接收方漏掉） |
| T5 | `service/physicsroom`（physx）：`TestLevelActionsRunOnlyAfterTheFrameIsSettled` | 提交失败时动作不执行；成功后下一 tick 执行，回执与传送后的帧同一笔提交 | 变异：在 handler 返回前执行动作；不检查 `RequestSettled` 错误 |
| T6 | 同上：`TestRepeatedPendingActionTeleportsOnce`、`TestTeleportReleasesGrabsAndWaitsForTheFrame`、`TestInvalidTeleportCompletesAsFailure`、`TestCancelledActionReceiptDoesNotStopTheRoom` | 去重、抓取释放与不按旧位置重抓、失败回执、取消后的回执被忽略 | 变异各一 |
| T7 | `service/physicsroom`：`TestBusinessActionsNeverBlockThePhysicsLoop`、`TestUnknownBusinessResultIsRetriedWithTheSameID` | 阻塞的 `Execute` 不降低物理频率；超时不给失败回执、按同 ID 重试；在途上限；停房取消 | 变异：在执行 goroutine 里同步调用；超时给 `Success=false` |
| T8 | `wiring/physicsroom`（physx）：`TestRoomEntityRollsBackFrameAndLevelTogether`、`TestLevelReplayIsDeterministic` | handler 出错时两份 DAO 一起撤回、房间停止；录制帧重放两遍状态一致 | 变异：LevelDAO 不在 undo 里（用无回滚 handler 时组件应拒绝） |
| T9 | `wiring/physicsroom`：`TestPlayerIngressPushesLevelViewOncePerRevision` | 每个 Revision 推一次、重连拿最新、消息号互不相同 | 变异：每帧都推 / 只推一次不随 Revision |
| T10 | `wiring/physicsroom`（integration、physx）：`TestRealGateLevelRespawnAndFinish` | 真实 Gate / NATS / TCP：客户端把刚体推进掉落区 → 收到 bit3 帧且位置在检查点；进入终点 → `RoomLevelView.phase=Finishing`、`result=clear` | 行为红在 T2～T9 绿之前不可写；变异：执行器不执行 respawn |
| T11 | C#：`client/physics-viewer/tests` | RP3 / bit3 / `RoomLevelView` 金样；`InputPrediction` 遇不连续丢弃旧历史 | 变异：C# 忽略 bit3 |

所有用例按功能主题放进包内现有文件（`native_test.go`、`level_test.go`、`replication_test.go`、`service_native_test.go`、`nest_native_test.go`、`players_test.go`、`players_real_test.go`），只有 `regions.go` 新增对应的 `regions_test.go`；文件名不带编号。

### 6.2 联合验收

| 项 | 做法 | 谁做 |
| --- | --- | --- |
| 真实三进程（Go 驱动） | `physicsroom-local` 的 physicsroom / game / gate 三个子命令（私有 NATS、etcd 端口），示例关卡 schema 2；Go 或 Python 客户端以 `physics-slot-N` 登录，脚本化：掉落 → 重生到检查点（记录从进入掉落区到收到 bit3 帧的 tick 数）、到终点 → 视图终局；SIGTERM 依次停三进程，日志无 WARN / ERROR | agent |
| Unity 编译 | 本机 Unity 6000.3.12f1 batchmode 编译 physics-viewer 源码 + EditMode 跑编解码金样 | agent |
| Unity 实机双客户端试玩（手感、HUD、弱网） | 写进 EXTERNAL-VERIFICATION，不作本轮门槛（D11） | 维护者 |
| Linux 原生 | 归 C-L 外部复测 | — |

### 6.3 性能口径

同机、同配置、流程开 / 关交替 3 轮，每轮约 10 分钟以内，长稳前看 `sysctl vm.swapusage`、`caffeinate -d -i`，证据放 `docs/performance/benchmarks/physx-level-actions-<日期>/`。

| 场景 | 量 | 判定 |
| --- | --- | --- |
| `TestMultiRoomReplicationLoad`（`ROOST_PHYSICS_LOAD=1`），机关关卡 1 / 4 / 8 房间 × 20 逻辑客户端，物理 50Hz、下发 ≤20Hz | 物理均频、提交 p50 / p99、Go 分配 MB/s、每客户端载荷字节 | 流程开（32 个区域、每客户端约每 5s 掉落重生一次）相对关：物理均频下降不超过 1%；分配与载荷如实报，不预设阈值 |
| `TestPhysicsRoomSoak` 8 房间 × 20，8 分钟，流程开 | RSS / footprint / goroutine / 原生堆斜率 | 与 3B 同口径对照，没有新的无界增长 |
| 流程关时 | 同上 | 与改前（`ba158535`）同机对照不回退：证明不用流程的房间不付成本 |

## 7. 分批计划

工作量按 agent 工作日估，含自测与文档；不含维护者决策等待。

| 批 | 产物 | 依赖 | 预计 |
| --- | --- | --- | --- |
| 1 native 与绑定 | `rp_set_pose`、ABI 3、`World.SetPose`、`disabled.go` 桩、T1；本机链出 dylib 跑通 | 决定项定稿 | 1 天 |
| 2 关卡与区域、同步标记 | Level schema 2、`regions.go`、`State.Teleports`、RP3 + bit3 + 视图账本、T2～T4、Go 金样 | 批 1 的类型（可并行写，合入在后） | 2 天 |
| 3 房间执行器与流程挂载 | `Commit` 新签名、`Spec` 字段、`actions.go`、`run` 顺序、抓取处理、`RoomEntity` 挂流程、`ActionHandler`、`Delivery.Level`、PlayerIngress 视图推送、RPC 重新生成、`physicsroom-local` 示例关卡、T5～T10、`verify-service.sh` 与根门禁用例名 | 批 1、2 | 3 天 |
| 4 客户端 | C# 编解码 / 回放 / 预测 / HUD / 关卡显示、T11、Unity batchmode 编译 | 批 2 金样 | 1.5 天 |
| 5 联合验收与性能、文档 | 三进程演练、性能对照、PHYSICS-ROOM-SERVICE / LEVEL-COMPONENT / viewer README / KNOWN-LIMITS / CHANGELOG / 功能版 NOTES | 批 3、4 | 2 天 |
| 合计 | | | 约 9.5 天 |

每批独立提交到 main（按维护者 10-16 规则），每批末本地 `GOWORK=off` build / vet / 受影响包 race；跨包改动（批 3 改 `Commit` 签名）push 前跑一次全量 `GOWORK=off go test ./...`；慢套件在批 5 统一跑一次。按功能版发（发版前写 `docs/release/vX.Y.0-NOTES.md`）。

## 8. 风险

| 风险 | 影响 | 应对 |
| --- | --- | --- |
| 每 tick 的 handler 多了流程调用，8 房间 × 50Hz 的提交路径变慢 | 物理均频下降 | 无事实时只做一次期限比较；Revision 不变不复制状态；6.3 对照，超标先查分配 profile |
| 中心点判定不等于形状重叠：大刚体擦边进入不算 | 关卡作者预期不符 | 写进手册：区域按刚体中心；需要形状重叠的另立项（D2 的 B） |
| 传送到有人的出生点 | PhysX 去穿透把人弹开 | 多位置按槽号分配（D8）；手册写明出生点要留空 |
| UGC 流程在运行期撞预算导致停房 | 该房间所有人掉线 | 发布期跑 `Compile` 与试玩；停房记 `level_failed` 指标（D4 选 B 可改为只冻结流程） |
| 客户端预测在传送附近仍有一帧错位 | 一帧闪烁 | bit3 只在权威帧到达时生效；本轮接受，Unity 实机再看 |
| `Commit` 签名变化波及测试替身 | 改动面 | 只有 `EntityHost.Commit`、`NestCommit` 与测试替身；批 3 一次改完，全量测试兜底 |
| 物理房间进程崩溃时业务动作结果未知 | 奖励重复或丢 | 业务账本按动作 ID 去重（Level 手册已有契约）；本轮只给接口，不做到账链 |

## 9. 需要维护者决定

| # | 问题 | A（推荐） | B / C | 推荐理由 |
| --- | --- | --- | --- | --- |
| D1 | LevelComponent 放哪 | A：物理房间 owner 的 RoomEntity，与帧同一笔 undo 事务 | B：Game 进程的业务实体，动作经 RPC 到房间 | A 没有跨进程回执与“结果未知”，失败语义直接复用“提交失败停房”；B 每个动作多一跳 RPC、要处理重试与房间重建 |
| D2 | 区域怎么判 | A：Go 侧按已提交帧判刚体中心（含线段穿越），纯函数，不加 native 事件 | B：PhysX trigger 形状 + `onTrigger` 回调，ABI 加事件缓冲 | A 确定、无 PhysX 也能测、改动小；B 判形状重叠更准，但回调在 `fetchResults` 内、次序与 CCD / 运动学组合要额外处理，native 改动大 |
| D3 | 每个玩家的检查点存哪 | A：房间执行器的表（`actor → spawn`），只由已提交动作写 | B：给 LevelComponent 加按参与者的变量作用域（流程定义升版本） | A 不动通用组件；B 改 `gameplay/level` 的定义与投影，影响 dungeon 等其他使用者 |
| D4 | 流程运行期出错 | A：整个房间停止（同提交失败） | B：只冻结流程（标记故障、下发视图），物理继续 | A 与现有契约一致、实现最少；B 体验好但多一个“流程故障”状态与客户端提示 |
| D5 | 业务动作（结算 / 奖励） | A：本轮给 `ActionHandler` 接口与契约测试，到账链留给业务 | B：只做物理动作，流程里不能写业务能力；C：本轮连 Saga 到账一起做 | A 让流程能表达“终点结算”且边界清楚；C 工作量约再加 3 天，且奖励规则属于业务 |
| D6 | 客户端怎么知道是传送 | A：RP3 + 状态字节 bit3 + 按接收方账本补发 | B：不改协议，客户端按误差阈值自己判断 | A 显式、不漏（远档接收方也收到）；B 阈值难定，误判会把快速移动当传送 |
| D7 | `Commit` 接口 | A：改签名为 `Tick → Settled`，一条路径 | B：保留旧签名，另加可选的流程提交钩子 | 未上线不需兼容；两条路径会让“帧与流程同一笔提交”变成可选，容易接错 |
| D8 | 出生点重叠 | A：每个出生点给多个位置，按槽号选 | B：native 加空位查询（`rp_overlap`），ABI 再扩 | A 零 native 成本、确定；B 更通用但要多一个原生函数和测试 |
| D9 | 流程持久化 | A：只用瞬态（`NewTransientLevelDAO`，memory） | B：支持持久模式 | 物理世界不可恢复，持久流程恢复后也对不上世界 |
| D10 | 成员事实 | A：本轮带 `member.join` / `member.leave` | B：不带 | “全员到终点”这类规则需要人数；成本是成员表变更队列，很小 |
| D11 | Unity 验收 | A：agent 做 batchmode 编译 + EditMode 金样，实机双客户端试玩写进 EXTERNAL-VERIFICATION 由维护者做 | B：本轮必须实机试玩通过才算完成 | 本机 agent 不能可靠地操作 Unity 实机与弱网；B 会让本轮卡在人工环节 |

维护者说“按推荐”时按全部 A 实施。
