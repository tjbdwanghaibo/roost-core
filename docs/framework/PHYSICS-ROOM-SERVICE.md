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

默认值是准入限制，不是容量承诺。建房按最坏每非静态对象 92 字节加 36 字节头检查整帧预算，保证分散到不同 Chunk 也不越界；总待确认帧内存上界约 MaxRooms×MaxPlayers×MaxFrameBytes，另计世界、快照和编码临时分配。NATS bus 自身 WorkerNum/QueueCap、Gate 出站预算需要部署方配置。

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
- 通用 Chunk 分组协议仍为 v1；物理对象载荷从 **53 改为 70 字节**：原 53 字节之后是 little-endian `InputSequence:u64`、`InputActive:u8`、`InputGeneration:u64`。旧客户端按长度拒绝。单对象最坏分组开销 22 字节，因此预算为 `36 + 非静态对象数 × 92`。
- Native ABI 升到 2，`rp_add_pose` / command 布局变化；必须先停旧服务、重建 native 库和 Go 程序、同步更新 Unity 的全部 viewer 源码及采集脚本，再启动。未部署阶段不兼容混跑旧版本；本功能没有持久房间数据迁移。

### 已验证与仍需验证

2026-10-26 在 Linux amd64、Go 1.27.2、PhysX 4.1.2、私有 NATS 2.12.3 上实际构建原生库并运行：升降平台托举及随台上升时起跳、转杆碰撞、弹簧抓取／释放、铰链马达、非法输入、抓取超时／静态目标拒绝、慢提交后的跳跃过期、重连输入代际以及已提交快照编码。Gate 验收使用真实 TCP 和 NATS，服务与 Gate 宿主在测试进程内启动，房间结果经真实 EntityHost/Nest 提交；验证关卡内容摘要、槽绑定、运动学对象同步和已采样输入确认。不是跨机器或多个独立 OS 进程的容量验收。

全仓 `go build ./...`、`go vet ./...` 与根目录门禁通过。全仓测试的 Lockstep 慢链路用例 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive` 在当前 Linux 环境失败（停读连接未溢出、快照未完成）；同一用例在未修改的 main ce49b3c3 归档中也复现，未将它记为本轮修复或全仓通过。

预测核心由 .NET 10 SDK 的 C# 编译器编译并执行，覆盖确认后纠偏、未确认输入重放、一次跳跃、输入过期、快照停顿、32 条历史上限及恢复。当前云环境的 `dotnet run` 首次配置因 NuGet 命名互斥量失败，实际使用 SDK 内的 Roslyn 编译器及 dotnet runtime 直接执行，未把该 CLI 失败记成测试通过。**未运行 Unity 编辑器或 Unity 碰撞适配器**，本轮新增交互还须在 Unity 6000.3 中编译、双客户端试玩及网络延迟下验收。

预测采用有界近似模型，最长重放/外推 250ms，历史溢出或快照停顿回到权威位置。它不复制服务器整个 PhysX Scene，不回滚其他玩家/抓取关节，也不保证不同 PhysX 版本数值一致；移动平台摩擦、角色贴边和复杂接触可能产生纠偏。此前 20 人 60 秒 direct TCP 基准不覆盖新增关节、载荷、Gate/NATS 或多房间成本，不能直接作为这些功能的容量报告。

复跑原生及真实链路：`ROOST_PHYSICS_TEST_NATS_URL=<私有地址> scripts/physics/verify-service.sh`。预测核心：`dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj`。实际 Unity 操作入口和关卡导出见 [viewer 使用说明](../../client/physics-viewer/README.md#ugc-关卡与本地预测)。
