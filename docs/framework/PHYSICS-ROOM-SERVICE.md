# 物理房间 service

2026-10-25，当前未发版。复用 [PhysX 与 Chunk](CHUNK-PHYSICS.md)，目标是单个 owner 进程承载多个独立物理房间。领域包 `service/physicsroom`，装配包 `wiring/physicsroom`；不将验收宿主 `cmd/physics-viewer` 改成生产服务器。

## 已实施的调用链

```
已认证的 Game 入口
  → 固定 owner SID 的 Rooms RPC（Core NATS / MessagePack）
  → 房间的输入邮箱 → 独立 World，物理 50Hz
  → NestCommit → 业务房间 Entity Guard → StateDAO.Accept
  → 已提交快照 → XYZ AOI → Chunk Plan
  → Game Relay（20Hz Poll / ack）→ PushBound → Gate → TCP 客户端
```

领域实现与生成传输保持一个包，按 types/service/client/server 分文件；接线单独放 wiring，不新建额外 manager 包。RPC 请求/响应与 handler 从 `Rooms` 接口生成。普通服务的生成装配使用随机实例客户端，因此此处装配显式固定 owner；不会手工修改生成的协议文件。

本阶段房间为瞬态状态，进程退出意味着房间结束。这是实时物理权威的正式内存状态，不是模拟持久存储的内存替身。没有经济事务、掉线恢复快照、跨机物理迁移或接管。基础 SDK 的 Foundation/PxPhysics 共享，各 Scene 独立；每房一个 native worker，上限由 MaxRooms 控制。

## API 与身份

- `Describe()`：取得 owner 进程代际，填入 Spec.Instance 并随同一 Create 请求保留。进程重启后旧请求明确拒绝，不把一次未知结果的重试变成新的建房。
- `Create(Spec)`：可信调用方提供全局唯一房间 ID、静态/动态刚体及寿命秒数。相同 ID 的存活房间仅在配置完全一致时返回原 Ref；配置不同拒绝。创建失败不占房间槽。返回 Ref 包含随机 Epoch。
- `Join(Ref, Binding, bodyID)`：Game 从认证连接取得完整 Binding，指定该玩家可控制的动态体。房内一个玩家一个成员，一个刚体一个控制者；相同绑定重试返回原令牌，不同绑定必须先结束旧成员。Ref 代际不符直接拒绝。
- `Submit(Member, Input)`：令牌与序号校验，有限加速度模长不超过 20；同序号同载荷为重试，不刷新控制有效期。只保留最新输入，默认 250ms 未更新即停止施加加速度，已有速度由物理继续推进。
- `Poll(Member, ack)`：最多 20Hz 产生新帧；返回完整 Chunk 包。ack 只确认此前发给该成员的 pending 帧，丢失 RPC 响应时重取同一帧。每人最多一个 pending，最多 MaxFrameBytes；不会在慢接收者后堆积历史快照。
- `Leave(Member)`：精确核对成员令牌，旧连接的迟到 Leave 不会删除新成员。
- `Destroy(Ref)`：停止准入、取消模拟、等在途提交退出后释放原生资源。超时只返回未知完成状态，后台继续排空，不提前 Close。

内部总线是可信服务边界，Binding.Validate 只做格式检查，不是远程认证。客户端不得直接填写 Binding/PlayerID 调这些 RPC。成员 Token 不可作为公开房间列表字段泄漏；房间 ID 与 Epoch 一起携带。房间销毁后的 ID 在 MaxLifetime 内保留拒绝重建记录，记录达到 MaxRooms×16 后暂拒新建；使用者必须始终生成新的房间 ID，不应在保留期后重试旧建房请求。进程重启不会恢复该记录，旧 Spec.Instance 被拒绝，客户端必须把旧 owner 的房间作废。

## 装配

1. 编译 native 库，再用 `-tags physx` 构建服务；未启用后端时 `domain.New` 明确失败，不发布一个不能创建世界的服务。
2. 创建业务 Nest 房间 Entity/Getters，登记处理 `wiring/physicsroom.FrameCommit` 的 handler。检查 Room Ref，调用 Entity 的 `gameplay/physics.StateDAO.Accept`；handler 用 `RollbackUndo`、`DurabilityMemory`。创建物理房间前，该目标必须已经加载。
3. `commit := physicsroom.NestCommit(engine, handlerName, resolveRoomEntityID)`。等待 RequestSettled 在房间执行器上，物理模拟不占 Nest short/long/io worker。提交失败停止该房间，不继续推进不可回滚的原生状态。
4. `cfg := domain.DefaultConfig(commit)`，按部署显式调整容量；用 `physicsroom.NewMod(cfg, nestModName)` 注册能力，依赖名保证停房先于 Nest 停止。`physicsroom.NewServer()` 实现 app.Service，可与现有 NATS Mod 装入独立进程。进程入口与 SID 由业务部署仓决定。
5. Game 侧用 `domain.NewOwnerClient(bus, ownerSID, timeout)`，或 `physicsroom.NewClientMod(ownerSID, timeout)`；先 Describe 并保留进程代际到 Create；所有房间后续调用固定同一 SID。不要直接使用通用生成的 NewBusClient/NewDiscovered 路由。多个 owner 时，由业务维护 Room Ref → owner SID 的映射。
6. Game 完成 Join 后，每个 Member 仅运行一个 `physicsroom.Relay(ctx, client, gameIngress, binding, member, messageID)`。输入由认证处理器取服务端保存的 Member，调用 Submit；不信任客户端传来的成员令牌。

Relay 在可靠出站准入成功后，于下一次 Poll 确认帧；此 ack 不表示终端已经收到。PushBound 失败、RPC 失败或取消会结束 Relay 并尝试 Leave；网络故障导致 Leave 不达时，MemberTTL 回收成员。重连建立新 Member/基线，不能继续旧 Cursor。Gate 仍负责出站有界队列与慢连接关闭。应用须在 OnClosed 时取消对应 Relay，并先完成旧 Leave 再 Join 新 Binding；默认不会让新连接抢占旧成员。

## 默认预算与停止

| 配置 | 默认 |
| --- | ---: |
| MaxRooms | 64 |
| 每房 MaxPlayers | 20 |
| 每房 MaxBodies（含静态体） | 256 |
| 每成员 MaxFrameBytes | 64KiB |
| InputTTL / MemberTTL | 250ms / 5s |
| MaxLifetime | 1h |
| AOI 进入 / 退出 | 12m / 14m |

默认值是准入限制，不是容量承诺。建房按最坏每非静态对象 57 字节加 56 字节头检查整帧预算，保证分散到不同 Chunk 也不越界；待确认载荷的预算基数为 MaxRooms×MaxPlayers×MaxFrameBytes；实际内存还包括编码计划与其载荷副本、已确认/待确认的视图账本、世界、快照和临时分配，不能把该乘积当进程内存上界。NATS bus 自身 WorkerNum/QueueCap、Gate 出站预算需要部署方配置。

Shutdown 先封闭服务准入，取消所有房间，然后等待各自 done。若业务 Commit 不返回，Shutdown 超时必须保留依赖和 native 资源；Mod 提供 StopWithContext，供 app 执行这个契约。指标通过 servicemetrics 报 create/input/join、create 重放/容量拒绝、成员过期、房间失败与房间深度，不使用 RoomID/PlayerID 标签。

## 验证与边界

定向测试覆盖容量竞争、相同 Create 重试、旧 Epoch/成员拒绝、pending 重发、非法 ack、重新加入令牌、帧预算、停机等待在途提交。Nest 实测覆盖 Guard 内 StateDAO；真实 NATS 测试覆盖生成的七个 RPC、MessagePack 和固定 SID；真实 Gate/NATS/TCP 测试覆盖认证 Binding、三帧连续解码与 Relay 取消清理。测试中的轻量 Commit 替身只用于隔离服务或网络契约，Nest 链路有单独的真实 Engine 验证。

这些是组合部件验证，不是跨三个独立 OS 进程的完整生产部署。尚未测多房间容量上限或长稳。此处为 10-25 阶段证据；10-26 Linux 功能验证见下文。客户端输入协议/PB 与开房权限属于业务入口，需要业务接线；当前未修改 Unity 展示端的登录/开房界面。Raycast、碰撞业务事件、运行期刚体增删不在本阶段接口内。

复跑（私有 NATS 地址，不使用故障注入）：

```sh
GOWORK=off go generate ./service/physicsroom
GOWORK=off go test -race -tags physx -count=2 ./service/physicsroom ./wiring/physicsroom ./infra/network/servicerpc
ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14229 ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:14229 GOWORK=off go test -race -tags 'physx integration' -count=2 -run 'TestRealNATSRoomLifecycle|TestRealGatePhysicsRoomRelay' ./service/physicsroom ./wiring/gate
```


Nest 适配使用新增的 `NestMgr.RequestSettled`：准入前遵守取消与 worker 限制，准入后等待真实完成回复，不能被普通同步请求超时提前结束。普通 Request 的行为保持不变。已用真实 Nest handler 阻塞第二拍的测试确认：Destroy 超时不会宣称已完成；释放 handler 后才能完成销毁。适配器不能用普通 Request 替代，否则调用取消时会误判提交已结束。

本轮实测环境为 macOS ARM64、PhysX 4.1.2、独立临时 NATS 2.14.5；未修改已有 NATS 服务。CBM 对当前 worktree 的索引刷新仍失败（worker exit=1），新代码与执行边界通过当前源码和测试核对，未宣称索引已更新。

专用复跑入口：先构建原生库并启动私有 NATS，再执行 `ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14229 scripts/physics/verify-service.sh`。脚本缺少 NATS 配置直接失败；根目录覆盖门禁将需要 physx 的集成套件与普通 DataEngine 环境分开，并核对专用脚本的包和用例名。


提交前验证结果：默认 `go build ./...` 通过；service/physicsroom、wiring/physicsroom、servicerpc、Nest 的 race 三轮通过；专用 verify-service.sh 实跑通过（含真实 NATS/Gate/TCP 三轮）；root、wiring、servicerpc 生成器验收通过；相关包 vet 通过；再次生成 RPC 无漂移。固定 owner 在带服务发现的调用路径也不会重新选址，另有针对性 race 回归。原生验证脚本拒绝 CGO_ENABLED=0，避免所有 native 用例被构建条件排除后误报成功。

## 房间 Entity 接入（2026-10-25）

`wiring/physicsroom.NewEntityHost(kind, category, maxRooms, nestOptions...)` 提供可复用的瞬态 RoomEntity 宿主，部署方先注册 kind/category。Host 检查注册分类一致，专用 Nest 引擎只负责 Guard 中的帧提交和快照读取；原生 Step 仍在房间执行器中。每个 Ref 首次提交创建一个 Entity，后续帧必须同 Epoch、连续 Tick；StateDAO 不落库，读写快照均拷贝 slice。

```go
host, err := roomwiring.NewEntityHost(roomKind, roomCategory, maxRooms, nestOptions...)
// 检查 err，且 roomKind/category 已在业务注册。
cfg := physicsroom.DefaultConfig(host.Commit)
cfg.MaxRooms = maxRooms
cfg.Closed = host.Forget
rooms, err := physicsroom.New(cfg)
```

`Closed` 在模拟与在途提交结束、native world 释放后调用，Host.Forget 移除对应 Entity；不要从其他业务路径提前 Forget。关闭顺序为 Service.Shutdown 成功后再 Host.Close；Shutdown 超时时保持 Host 存活。业务自行决定开房、玩家权限和结算，不在 Host 中实现这些规则。

Input 新增 Jump，Acceleration 只允许 X/Z（Y 必须为零）。同序号重放不重新触发跳跃，也不延长旧输入 TTL；新移动输入不会覆盖尚未消费的跳跃，过期跳跃不被后续输入复活。服务器用排除自身的向下射线检查支撑面与相对支撑点的竖直速度，确认落地后加 7m/s 速度增量；该判定不是通用 CharacterController API。native command 布局变化，必须重建 native 静态库及 Go 程序，旧库必须重建；当前 native ABI 由 rp_abi_version 校验。

Unity `Room.unity` 复用上述 Host / Service，通过既有 loopback 验证入口测试移动跳跃与 Chunk 显示；业务登录与开房 UI 留给使用方。具体操作与协议见 [客户端说明](../../client/physics-viewer/README.md)。

本轮 Room Entity / Unity 功能验证结果见 [验证记录](../performance/benchmarks/physics-room-input-20261025/README.md)。


## 本地双进程调试

GoLand 的 Physics Service 与 Physics Gate 配置分别启动 owner 与接入进程；后者承载最小 GameIngress，通过真实 NATS Gateway 通道和 MessagePack RPC 访问 owner，不直接调用内存中的房间 Service。Unity 默认走 Gate 握手及 Protobuf 控制输入；业务认证仍由部署方实现，本地 slot 票据只用于 loopback。配置、日志采样和实测记录见 [客户端说明](../../client/physics-viewer/README.md#gate--physics-service当前默认)。本地两个进程中的 SID / namespace 固定，不能同时再起第二套相同配置。


## Unity UGC 多人闯关：运动、抓取、关卡与预测

本节是当前扩展契约。业务入口仍负责认证、开房权限、关卡发布审核、检查点、终点、淘汰及永久奖励；物理只产生权威运动事实，不能把客户端宣称通关当成奖励依据。

| 玩法 | 框架接口及接法 | 权威边界 |
| --- | --- | --- |
| 转杆、升降台、往复平台 | `physx.Body.Motion=Kinematic`，`Spec.Motions` 填 `physics.MotionTrack`；每 20ms 由服务器 tick 求姿态，`setKinematicTarget` 让碰撞求解器处理接触 | 客户端不能指定目标位置；网络 AOI 隐藏对象也不会从物理世界移除 |
| 摆锤、转轴马达 | 动态 Box + `Spec.Joints` 中的 `physx.Hinge`；锚点局部 X 为旋转轴，B=0 表示世界锚点，`MotorSpeed` 为 rad/s，`MotorForce` 为驱动力上限 | 关节来自可信建房配置，不是玩家远程创建接口 |
| 按住抓取／松手 | `Input.GrabTarget`，0 松手；每控制者最多一条 Distance 弹簧，当前实现中心距离 ≤2.5m、绳长 1.2m、刚度 100、阻尼 10、断裂力 300 | 服务器排除自身、静态和运动学目标；依据上一已提交帧检查。输入过期、距离越界、Leave、成员过期释放；无视线遮挡检查，规则可在业务入口继续收紧 |
| 玩家自建关卡 | `physics.Level` → `DecodeLevel` / `Validate` → `Content` → SHA-256 → `Spec{Bodies,Motions,Joints}` | 关卡只允许已知字段与基础几何，不执行用户代码；房间创建后配置固定，修改版本需新开房 |
| Unity 本地预测／纠偏 | `InputPrediction.Record` 保存输入，`Reconcile` 用已提交快照中的实际采样序列重置基线并重放未确认输入；Unity 适配器做胶囊扫掠与地面检测 | RPC 接收响应只返回成员 `InputGeneration`。只有快照里相同代际的 `InputSequence` 才确认模拟；服务器不接收预测后的坐标 |

`MotionDefault` 保持原调用方语义：Box 静态，其余动态；新关卡可显式指定 Static/Dynamic/Kinematic。快照及预算均包含动态和运动学对象，控制成员只能绑定动态对象。通用 World 提供 `AddJoint` / `RemoveJoint`，与 Step 共用锁；Room 对它们的调用只发生在物理执行器上。最多 128 个配置运动轨迹和 128 个配置关节；抓取关节 ID 使用高位保留区，房间刚体及配置关节 ID 必须小于 2^63。

Level schema 1 要求非空 Name、非零 Revision、最多 256 个刚体、20 个控制槽、256KiB 精确发布字节。槽到 Body 的映射由认证后的 Game 使用 `Players` 决定；Unity 不再固定要求 71 个对象。发布验证还限制坐标绝对值 ≤1000m、形状尺寸 ≤100m、有效半径/Box 半厚度 ≥0.05m、非静态质量 0.01～10000；这些是拒绝异常配置的预算，不能替代关卡试玩和性能测试。几何仅 Box/Capsule/Sphere，未含网格碰撞烘焙、脚本沙盒、资源商店、CDN、关卡在线编辑或运行期刚体增删。

跳跃修复：基线 ce49b3c3 在慢 Commit 期间持续收到移动时，会用移动时间续期 `jumpPending`，使已过期跳跃迟到执行。现在 `jumpAt` 独立记录跳跃期限，Submit 和消费点都校验；相同序号重试不重新触发、不刷新期限。默认测试用虚拟时钟验证邮箱；native 测试实际阻塞提交后确认不发生迟到起跳。

### 输入确认、协议与升级

- 成员每次重新 Join 都分配新的 `InputGeneration`。旧连接的快照可能短暂仍在视图中，但客户端不拿旧代际的序列确认新输入。
- 每帧在输入采样后记下 Sequence，只有 PhysX Step 和 Nest 提交成功后才发布；`InputActive=false` 表示该帧没有继续施加该成员输入。输入邮箱仍只保留最新状态，不承诺逐条模拟每条收到的输入。
- Room 使用 RP2 私有帧头与 RC2 字段增量；完整量化对象 34 字节，输入确认只给当前接收成员。Comparison/Crowd 对照仍使用 RC1、70 字节无损对象，不作为新 Room 带宽测量入口。

- Native ABI 升到 2，`rp_add_pose` / command 布局变化；必须先停旧服务、重建 native 库和 Go 程序、同步更新 Unity 的全部 viewer 源码及采集脚本，再启动。未部署阶段不兼容混跑旧版本；本功能没有持久房间数据迁移。

### 已验证与仍需验证

2026-10-26 在 Linux amd64、Go 1.27.2、PhysX 4.1.2、私有 NATS 2.12.3 上实际构建原生库并运行：升降平台托举及随台上升时起跳、转杆碰撞、弹簧抓取／释放、铰链马达、非法输入、抓取超时／静态目标拒绝、慢提交后的跳跃过期、重连输入代际以及已提交快照编码。Gate 验收使用真实 TCP 和 NATS，服务与 Gate 宿主在测试进程内启动，房间结果经真实 EntityHost/Nest 提交；验证关卡内容摘要、槽绑定、运动学对象同步和已采样输入确认。不是跨机器或多个独立 OS 进程的容量验收。

全仓 `go build ./...`、`go vet ./...` 与根目录门禁通过。全仓测试的 Lockstep 慢链路用例 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive` 在当前 Linux 环境失败（停读连接未溢出、快照未完成）；同一用例在未修改的 main ce49b3c3 归档中也复现，未将它记为本轮修复或全仓通过。

预测核心由 .NET 10 SDK 的 C# 编译器编译并执行，覆盖确认后纠偏、未确认输入重放、一次跳跃、输入过期、快照停顿、32 条历史上限及恢复。当前云环境的 `dotnet run` 首次配置因 NuGet 命名互斥量失败，实际使用 SDK 内的 Roslyn 编译器及 dotnet runtime 直接执行，未把该 CLI 失败记成测试通过。**未运行 Unity 编辑器或 Unity 碰撞适配器**，本轮新增交互还须在 Unity 6000.3 中编译、双客户端试玩及网络延迟下验收。

预测采用有界近似模型，最长重放/外推 250ms，历史溢出或快照停顿回到权威位置。它不复制服务器整个 PhysX Scene，不回滚其他玩家/抓取关节，也不保证不同 PhysX 版本数值一致；移动平台摩擦、角色贴边和复杂接触可能产生纠偏。此前 20 人 60 秒 direct TCP 基准不覆盖新增关节、载荷、Gate/NATS 或多房间成本，不能直接作为这些功能的容量报告。

复跑原生及真实链路：`ROOST_PHYSICS_TEST_NATS_URL=<私有地址> scripts/physics/verify-service.sh`。预测核心：`dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj`。实际 Unity 操作入口和关卡导出见 [viewer 使用说明](../../client/physics-viewer/README.md#ugc-关卡与本地预测)。

### Room 同步压缩与复测（2026-10-26）

服务器保持完整精度模拟；网络先量化，再比较量化后的字段。`Cursor.PlanFields` / `Replica.ApplyFields` 提供 RC2，旧 `Plan` / `Apply` 保持 RC1。Room 使用 `NewQuantizedSpatialFrame`，接收端使用 `RoomReplica`；不要把 RP2 直接传给通用 RC1 解码器。AOI 仍按真实位置判断。

RP2 头为 20 字节：`RP` 占 2 字节，版本 2 与 InputActive 各占 1 字节，随后 little-endian Sequence / Generation 各 8 字节。确认来自成功提交的物理帧，只包含接收成员；同一 pending 帧重发时确认也被冻结。Generation 每帧保留一次，不放进每个广播对象。其后为 RC2，原有 Chunk 头 36 字节及对象 ID/长度保持不变。

| 字段（完整对象偏移） | 字节 | 网络精度/表示 |
| --- | ---: | --- |
| 位置（0） | 8 | Chunk 内 X/Z 为 int16 厘米，Y 为 int32 厘米 |
| 旋转（8） | 7 | 最大分量索引 1 字节，其余三分量 int16；q/-q 统一符号 |
| 线速度（15） | 12 | 三个 int32，0.01m/s，避免高速下落被 int16 饱和 |
| 角速度（27） | 6 | 三个 int16，0.01rad/s |
| 状态（33） | 1 | bit0 休眠，bit1 展示档（省略速度），bit2 轨迹档；bit2 必须同时设置 bit1 |

每个变化对象先发 1 字节字段掩码，只附变化字段。首次可见、跨 Chunk、离开后重入和重新连接均发送完整字段；无可信基线的部分补丁、非法位、截断及多余字节被拒绝，整帧通过后才替换状态。量化越界拒绝发布，不静默夹紧；角速度编码范围约 ±327.67rad/s，原生角速度上限为 50。准入最坏预算为 `56 + 非静态对象数 × 57`，含对象各占一个 Chunk 的情况。

同帧对照测试 `TestRoomBandwidthSamePhysicsFrames` 使用一份 PhysX 模拟结果：71 个动态对象，50Hz 求解、20Hz 采样、20 个观察者、20 秒模拟时间，共 400 帧，平均每观察者 9.68 个可见对象。每种编码使用完全相同的帧和 AOI。

| 编码阶段 | 总载荷字节 |
| --- | ---: |
| 原 70 字节对象 RC1 | 6,315,788 |
| 输入确认私有化，公共对象 53 字节 | 5,229,076 |
| 完整量化对象 34 字节 | 3,566,536 |
| RP2 + RC2 字段增量 | 3,048,113 |

该样本减少 **51.74%**，平均每客户端约 **15.79 → 7.62 kB/s**（十进制）；逐帧解码实测最大单轴位置误差约 **0.005m**。计入 Chunk 与私有头，不计外层 RS、TCP/IP、NATS；这是编码对照，不是网络吞吐、CPU 性能、WAN 或多房间容量报告。复跑：`go test -tags physx -run TestRoomBandwidthSamePhysicsFrames -count=1 -v ./gameplay/physics`。场景、可见人数、运动和频率变化都会影响结果。

升级需先停旧再起新，更新 Room 服务及 Unity 的 `PackedBodyCodec.cs`、`ChunkReplica.cs`、`CrowdViewer.cs`（推荐同步全部 viewer 源码），重新连接建立全量基线。此轮 native ABI 仍为 2。Go 字段补丁/量化回归与 C# 共享金样覆盖补丁、截断、基线缺失及不可变历史；Unity 编辑器、碰撞手感和弱网仍须另验。

本轮补丁实际通过：相关包 race、`verify-service.sh` 原生与真实网络各三轮、Go/C# 共享金样、全仓 build/vet 和根目录门禁。全仓 `go test ./...` 仍只有上述 Lockstep 慢链路用例失败；未将其标记为全仓通过。新增拒绝非法量化状态不推进基线/私人确认、两成员确认隔离与 pending 重发回归。


### 按接收者分频、字段裁剪与轨迹显示

`DefaultConfig` 默认启用 `Replication: gameplay/physics.DefaultReplicationPolicy()`。自定义配置零值禁用；显式设 `cfg.Replication.Enabled = false` 可退回所有可见对象全频字段增量，用于同场景对照。默认策略如下，频率受 Poll ACK 和最大 20Hz 新帧限制，并非硬实时发送保证。

| 对象档位 | 更新间隔 | 内容 |
| --- | --- | --- |
| 自己、抓取关联对象、近处对象 | 每个新帧，最高 20Hz | 完整可用物理字段 |
| 中距对象 | 至少 5 个物理 tick，约 10Hz | 位置、旋转和状态；速度清零并显式标记省略 |
| 远距对象 | 至少 10 tick，约 5Hz | 同上 |
| 远处有固定 MotionTrack 的运动学障碍 | 至少 50 tick，约 1Hz 校准 | 权威姿态与轨迹档标记；客户端使用固定关卡的轨迹 |

近/中分界默认 5m/9m。判定使用两对象包围球的表面间距，并预留相对线速度 × 0.2s；降档多留 1m 滞回，升档立即补当前字段。长转杆按包围球保护，不因中心远就降频；抓取关系两端都保护。新可见、跨 Chunk、升档立即刷新，退出视野立即删除。规划使用已 ACK 的视图基线，pending 帧及分频计划一并冻结；重连从全量开始。该策略不改变服务器物理、Nest 提交或原有中心距离 AOI，超大障碍是否应扩大 AOI 仍由场景接入设计。

RP2 保持 20 字节私有头、RC2 与 34 字节完整对象，但状态字节增加档位标记。`RoomReplica.Profiles` / Unity `BodyState.DisplayOnly` 明确指出速度被省略，**不能把此时的零速度当作物理事实**。升档清除标记并恢复最新速度。旧 RP1 解码器必须升级，不混跑；ABI 仍为 2。

轨迹参数已经包含在摘要校验后的 Level.Motions：正弦 Translation/PeriodSeconds、世界旋转 Axis/AngularSpeed，时间原点为房间 tick 0，步长为 float32 0.02 秒。Unity `ReplicationPlayback.cs` 使用最近服务器姿态及该对象采样 tick 校准，再按已接收帧的显示时间求姿态；不积分累积误差，不跟随墙钟无限外推，收包停顿后停止推进。动态马达锤、抓取物与不带 MotionTrack 的刚体不套用轨迹模式。没有对应固定关卡元数据却收到轨迹档时客户端拒绝继续显示。

Unity 还保留每个对象自己的采样 tick；普通对象按自身采样间隔（上限 200ms）插值，其他对象的新帧不重置它的插值。近身预测的支撑速度只使用完整档对象。显示延迟、物理接触与弱网手感须在 Unity 编辑器验收，纯 C# 测试不能代替该验收。

同一 20 人 Crowd 样本中，上一节字段增量为 3,048,113 字节；分频与裁剪（含档位标记）为 **2,252,525 字节**，再降 **26.10%**，平均每客户端 **7.62 → 5.63 kB/s**。此 Crowd 样本无规则运动轨迹。独立 native 轨迹场景（10 秒、20Hz、一名观察者与一个远处运动学平台）为 **21,136 → 11,668 字节**，平台校准样本最大年龄 48 tick。二者都是同帧编码对照，非真实网络/CPU/容量结论。量化样本的约 5mm 单轴误差不包含降频后持有旧状态与插值延迟。

新增回归覆盖频率、升档恢复、双向抓取关联、快速接近/大包围体、跨区/删除、ACK 才提交视图、重连全量、原生轨迹相位以及 Go/C# 轨迹金样和稀疏插值停顿。复跑 `go test -race -tags physx ./gameplay/physics ./service/physicsroom`，真实链路仍用 `scripts/physics/verify-service.sh`。统计见 [可观测性清单](OBSERVABILITY.md#物理房间同步)：真实 Room Poll 路径分别计新帧/重试字节、Full/Delta、字段次数、档位变化/暂缓和 ACK 等待；不把 Poll 返回字节当作 socket 写成功。

本轮最终 RP2 代码通过全仓 build/vet、根目录门禁、受影响 race、三轮原生与真实 Gate/NATS/TCP 回归，以及 C# 量化/档位/轨迹/稀疏插值核心验证。全仓测试仍只有前述 main 基线已复现的 Lockstep 慢链路用例失败；未运行 Unity 编辑器与 WAN 验收。
