# 物理房间 service

本文描述当前物理房间服务。复用 [PhysX 与 Chunk](CHUNK-PHYSICS.md)，目标是单个 owner 进程承载多个独立物理房间。领域包 `service/physicsroom`，装配包 `wiring/physicsroom`。原验收宿主 `cmd/physics-viewer` 已于 2026-10-26 删除：与服务重合的场景由正式装配承担，本机入口是 `wiring/physicsroom/cmd/physicsroom-local`（见[本机三进程](#本机三进程正式装配2026-10-26)），原生与 Unity 的数值对照移到 `scripts/physics/comparison-host`。

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
4. `cfg := domain.DefaultConfig(commit)`，按部署显式调整容量；用 `physicsroom.NewMod(cfg, nestModName)` 注册能力，依赖名保证停房先于 Nest 停止。没有自己的房间 Entity 时改用 `physicsroom.NewEntityHostMod()`：按 `physicsroom.*` 配置（`entity_kind` / `entity_category` 必填，容量、TTL、AOI 有缺省）建下文的 EntityHost 与房间 Service，范围只在 `domain.New` 检查一次，Provide 时带 `physicsroom config:` 前缀报出；停止时先排空房间，成功后才关闭 Host。两者提供同一能力名，一个进程只装一个。`physicsroom.NewServer()` 实现 app.Service，可与现有 NATS Mod 装入独立进程。进程入口与 SID 由业务部署仓决定。
5. Game 侧用 `domain.NewOwnerClient(bus, ownerSID, timeout)`，或 `physicsroom.NewClientMod(ownerSID, timeout)`（它依赖 NATS Mod；2026-10-26 前写成能力名 `bus`，经 App 装配会在启动时报 unknown mod dependency，现由 `TestEveryModDependencyNamesAKitMod` 覆盖）；先 Describe 并保留进程代际到 Create；所有房间后续调用固定同一 SID。不要直接使用通用生成的 NewBusClient/NewDiscovered 路由。多个 owner 时，由业务维护 Room Ref → owner SID 的映射。
6. Game 完成 Join 后，每个 Member 仅运行一个 `physicsroom.Relay(ctx, client, gameIngress, binding, member, messageID)`。输入由认证处理器取服务端保存的 Member，调用 Submit；不信任客户端传来的成员令牌。这一步的正式接线是 `physicsroom.NewPlayerIngress(PlayerIngressConfig{InputMessageID, FrameMessageID, Seat})`：它同时是 Mod（依赖 GameIngressMod 与房间能力，Provide 时接上，停止时先于二者排空），把 `Dispatch` / `Ready` / `OnActive` / `OnClosed` 交给 `wiring/gate.NewGameIngressMod`。连接的首个输入调用业务的 `Seat(binding)` 选房选刚体并 Join，为该成员启动唯一 Relay；之后的输入 Submit；连接关闭取消 Relay（Relay 负责 Leave）；Relay 因出站或 RPC 错误结束时关闭连接。输入载荷是 Protobuf `RoomInput`（`client/physics-viewer/room_input.proto`），解码拒绝重复、未知字段与非 0/1 的 jump；其他消息号返回 ErrRequestInvalid，业务有自己的协议分发时把输入消息号转给它。

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
ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:14229 ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:14229 GOWORK=off go test -race -tags 'physx integration' -count=2 -run 'TestRealNATSRoomLifecycle|TestRealGatePhysicsRoomRelay|TestRealGateInputMovesRoomBodyAndStreamsChunks' ./service/physicsroom ./wiring/gate ./wiring/physicsroom
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

Unity `Room.unity` 经下文本机三进程的 Gate 连接这套 Host / Service，测试移动跳跃与 Chunk 显示；业务登录与开房 UI 留给使用方。具体操作与协议见 [客户端说明](../../client/physics-viewer/README.md)。

本轮 Room Entity / Unity 功能验证结果见 [验证记录](../performance/benchmarks/physics-room-input-20261025/README.md)。


## 本机三进程（正式装配，2026-10-26）

维护者 2026-10-26：“viewer 我希望是加一个 service，是不是已经有 service 了，如果逻辑已经重合了就不需要这个了。” 原 `cmd/physics-viewer` 逐项核对如下，重合部分删除，服务缺的接线补进 wiring。它还把 wiring/gate 与 NATS driver 引进按目录属于 core 层的 `cmd/`，使根包 `TestCoreDependencyBoundary` / `TestCoreContractsDoNotLinkDrivers` 变红（引入提交 ce49b3c3）；删除后两条门禁恢复，门禁规则未改。

| 原 viewer 场景 / 功能 | 正式服务是否覆盖 | 处理 |
| --- | --- | --- |
| `physics-service`：EntityHost + 房间 Service + 手建 NATS bus + 登记 RPC | 覆盖；Host 没有做成 Mod，bus 手建 | 补 `NewEntityHostMod`（配置声明一次）；进程用 NATS Mod + `NewServer` |
| `gate-room`：Gate 与 GameIngress 同进程、slot 票据、首输入 Join、Relay、Submit | Relay、ClientMod 有；首输入 Join、连接生命周期、输入解码没有 | 补 `PlayerIngress`；Gate / Game 改用 `wiring/gate` 的 GateMod、GameIngressMod |
| `room`：直连 TCP 的 PR01 适配 + 单房间 | 房间部分重合；PR01 只是验收协议 | 删除（Unity 走 Gate） |
| `crowd`：原始 TCP 控制连接、输入邮箱、AOI、NPC 施力、slot 0 旁观 | 多人控制、输入 TTL、AOI、Chunk 重合；NPC 与旁观不是服务能力 | 删除，连同 `measure-crowd.py`、`verify-crowd.sh` |
| `comparison`：原生与 Unity 同初态同输入的数值对照 | 不覆盖（要求首个订阅后才推进、整场景广播、固定输入带） | 保留为 `scripts/physics/comparison-host`，只依赖 core |
| `-log-level`、`-nats`、`-listen` 等调试参数 | App 的 `log.level`、`nats.url` 等配置 | 写进 `configs/` |

`wiring/physicsroom/cmd/physicsroom-local` 是一个二进制、三个 App 服务（子命令），配置在同目录 `configs/`：

| 子命令 | Mod | 说明 |
| --- | --- | --- |
| `physicsroom`（sid 7801） | NATS Mod、`NewEntityHostMod` | 房间 owner；`NewServer` 在 bus 上登记生成的 Rooms handler |
| `game`（sid 22） | NATS Mod、etcd Mod（只发现）、`NewClientMod(7801)`、`gate.NewGameIngressMod`、`PlayerIngress` | 启动时固定一个关卡：缺省 CrowdScene（slot N 控制刚体 50+N），`--level <json>` 时用校验过的 UGC 关卡（slot N 控制 `Players[N-1]`）；首个输入时按需建房（寿命 1h，停服时销毁）；`0x50485903` 内容握手返回关卡字节、SHA-256 与受控刚体 |
| `gate`（sid 11） | NATS Mod、etcd Mod（只发现）、`gate.NewGateMod` | TCP `127.0.0.1:19093`，经 etcd 找到 sid 22 的 game |

本机专用的只有 `physics-slot-N`（N=1..20）票据鉴权、Seat 与 Content（关卡），都在 main.go；正式部署由业务提供账号鉴权、开房、选座与内容发布，其余照搬。Gate 不读关卡，超出所选关卡槽数的票据由 game 的 Seat / Content 拒绝。依赖 NATS（4222）、Redis（6379，App 单实例锁，`wiring/gate` 要求启用）与 etcd（2379，Gate 发现 Game）。比原双进程多了 Redis 与 etcd：它们是 `wiring/gate` 正式装配的前提，不为本机另造一套。

```sh
scripts/physics/build-native.sh   # 一次
GOWORK=off go build -tags physx -o artifacts/physicsroom-local ./wiring/physicsroom/cmd/physicsroom-local
artifacts/physicsroom-local physicsroom --config wiring/physicsroom/cmd/physicsroom-local/configs/config.physicsroom.yaml
artifacts/physicsroom-local game        --config wiring/physicsroom/cmd/physicsroom-local/configs/config.game.yaml
# UGC 关卡：先导出可编辑示例，再让 game 用它开房（改关卡要重启 game 开新房）
artifacts/physicsroom-local export-level artifacts/obstacles.json
artifacts/physicsroom-local game --level artifacts/obstacles.json --config wiring/physicsroom/cmd/physicsroom-local/configs/config.game.yaml
artifacts/physicsroom-local gate        --config wiring/physicsroom/cmd/physicsroom-local/configs/config.gate.yaml
```

GoLand：`.run/` 下 Physics Service / Physics Game / Physics Gate 三个配置，Physics Stack 一次起三个（启动顺序无要求：房间在首个输入时创建，Gate 在登录时解析 Game）。三个配置 `log.level: debug`：连接、加入、离开逐次记录，输入每 20 个序号和每次跳跃记录，Chunk 每 20 帧记录。停止时先停 Gate、Game，再停 Physics Service；同一套配置不能起第二份（单实例锁让后起的等待后失败）。

链路：Unity → Gate（RS v2 握手、`physics-slot-N`）→ NATS → GameIngress → PlayerIngress → 固定 owner 的 Rooms RPC（MessagePack）→ 房间 PhysX 50Hz → RoomEntity 的 Nest Guard / StateDAO → AOI / Chunk → Relay → PushBound → Gate → Unity。

验证（2026-10-26，macOS ARM64，PhysX 4.1.2，私有 NATS / Redis / etcd）：

- `TestRealGateInputMovesRoomBodyAndStreamsChunks`（wiring/physicsroom，`integration && physx && cgo`）：EntityHostMod 经 Registry 提供、`NewServer` 登记 RPC，客户端经真实 Gate / GameIngress / PlayerIngress 发输入；断言受控刚体在已提交的 Entity 状态里沿 +X 移动超过 1m、客户端连续收到同 Epoch 的 Chunk 帧，断开后 PlayerIngress 在期限内排空。把输入加速度改成 0 时该用例在 8s 期限处失败（`moved=false`），断言落在输入生效上。`-race -count=3` 通过。没有 PhysX SDK 时这条用例因 tag 不满足而不编译：先 `scripts/physics/build-native.sh`，再按下文 `verify-service.sh` 运行。
- `TestPlayerIngressContentHandshake`（无 tag，2026-10-27 合入 UGC 时补）：内容消息号与回调必须成对且不与输入/帧消息号相同；空载荷请求按已认证绑定返回 RoomLevel，不 Join。`TestRoomInputProtobuf` 覆盖 `grab_target=5`，输入应答带成员代际 `RoomInputAck`。
- `TestPlayerIngressJoinsOnceAndReleasesWithTheConnection`、`TestPlayerIngressCloseRacesInput`（无 tag）：首输入只 Join 一次、断开后 Leave、Relay 出错关闭连接、Close 后拒绝输入且不再启动 Relay。
- 三进程实跑：按上面的配置（端口换成私有）启动三个子命令，Python 客户端以 `physics-slot-13` 登录，3s 内收到 39 个 Chunk 帧、40 个输入回执；game 日志记录按需建房、Join 刚体 63、跳跃输入。SIGTERM 依次停 gate、game、physicsroom，均正常退出。Unity 实机本轮未重跑。

边界：这是本机拓扑，SID、消息号与 Gate 地址是 main.go 常量（game 启动时核对自己的 sid）。不是多房间容量或长稳验证；没有多 owner 路由，Room Ref → owner SID 的映射仍由业务维护。

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
- Room 使用 RP2 私有帧头与 RC2 字段增量；完整量化对象 34 字节，输入确认只给当前接收成员。Comparison 对照（`scripts/physics/comparison-host`）仍使用 RC1、70 字节无损对象，不作为新 Room 带宽测量入口。

- Native ABI 升到 2，`rp_add_pose` / command 布局变化；必须先停旧服务、重建 native 库和 Go 程序、同步更新 Unity 的全部 viewer 源码及采集脚本，再启动。未部署阶段不兼容混跑旧版本；本功能没有持久房间数据迁移。

### 已验证与仍需验证

2026-10-26 在 Linux amd64、Go 1.27.2、PhysX 4.1.2、私有 NATS 2.12.3 上实际构建原生库并运行：升降平台托举及随台上升时起跳、转杆碰撞、弹簧抓取／释放、铰链马达、非法输入、抓取超时／静态目标拒绝、慢提交后的跳跃过期、重连输入代际以及已提交快照编码。Gate 验收使用真实 TCP 和 NATS，服务与 Gate 宿主在测试进程内启动（当时在 `cmd/physics-viewer`；2026-10-27 合入 main 时随 a5cae602 的收敛移植为 `wiring/physicsroom` 的 `TestRealGateUGCLevelAndInput`，经 PlayerIngress 的内容握手，移植后的版本尚未在装有原生库的机器上重跑），房间结果经真实 EntityHost/Nest 提交；验证关卡内容摘要、槽绑定、运动学对象同步和已采样输入确认。不是跨机器或多个独立 OS 进程的容量验收。

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


### 轮询间隔与 RPC 抖动

Gate Relay 从 **Poll 响应完成时** 安排下一次 50ms 轮询；等待与随后出站工作可重叠，但一次 Relay 不并发 Poll。服务端仍要求相邻新帧至少间隔 50ms，pending 重发与 ACK 规则不变。原固定 50ms ticker 在 RPC 准入交替延迟 15ms/0ms 时，会令相邻服务端请求只间隔约 35ms，导致一次空响应；读完三个有效帧的回归修前出现两次空轮询，修后为零。

该修复避免抖动触发双重限流，不是 ACK 流水线。长 RPC、慢出站或高 RTT 仍会降低有效频率；Relay ACK 表示 Game 可靠出站准入，不是 Unity 已收包。真实 Gate/NATS/TCP 回归入口为 `TestRealGatePhysicsRoomRelay`，含延迟注入、连续解码及取消后旧成员拒绝。


### 共享索引与多房间测量（2026-10-26）

Room 建立一次 `ReplicationScene`，保存形状包围半径与轨迹 ID；每个已提交帧建立一次 `ReplicationFrame` 状态索引，与 AOI 帧在同一把锁内发布。所有接收成员共用只读索引，私有输入确认也直接按 body ID 查询。`Frame` 复制输入；房间提交路径用 `AdoptFrame` 直接只读引用本 Tick `World.Step` 新建的状态切片（2026-10-29 起，`Commit` 返回后不得再修改 `Frame.States`，见下文长稳一节），输入无序时先复制再排序，从不修改调用方切片；ACK 后才提交的成员视图账本仍保持独立。独立调用 `PlanView` 的便捷接口保留，需要共享时使用 `NewReplicationScene(...).Frame(states).Plan(...)`。

Linux amd64、Go 1.27.2、GOMAXPROCS=4，CPU 型号 Xeon Platinum 8573C。`BenchmarkRecipientViewIndex` 在同一份 71 对象完整快照上比较每接收者重建与共享索引，三次测量：

| 规划方式 | ns/op 范围 | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| 每接收者重建索引 | 20,018～20,468 | 37,688 | 94 |
| 共享索引 | 11,518～11,716 | 19,064 | 85 |

单次规划分配量下降 **49.4%**，该微基准耗时中位数下降约 **43.4%**。共享路径的建房/每帧索引构建成本在规划循环外，不能据此声称整个服务器 CPU 或内存下降同样比例；完整链路仍有快照、视图、编码与载荷副本分配。

`TestMultiRoomReplicationLoad` 实际运行 Room + PhysX + EntityHost/Nest：每房间 20 个独立逻辑客户端，输入后 Poll，再从响应完成起等待 50ms；1 秒预热、5 秒采集，各档一次。普通场景 71 个非静态刚体；机关场景额外加入 8 个运动学转杆与 4 个动态铰链马达，共 83 个非静态刚体。20 个玩家持续提交移动输入，50 个 NPC 胶囊只参与物理碰撞，没有调用 CrowdCommands 自主巡逻。每世界一个原生 worker，Nest short pool 4 个 worker。GOMAXPROCS=4 不表示获得四个独占物理核。

| 场景 | 房间/逻辑客户端 | 平均物理 Hz/房间 | 交付 Hz/客户端 | 物理阶段 P99 ms | Poll P99 ms | Go 分配 MB/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 普通 | 1 / 20 | 49.99 | 19.80 | 0.703 | 0.351 | 8.69 |
| 普通 | 4 / 80 | 49.99 | 19.80 | 1.024 | 0.414 | 34.88 |
| 普通 | 8 / 160 | 49.32 | 19.48 | 1.690 | 0.562 | 68.29 |
| 机关 | 1 / 20 | 49.99 | 19.80 | 0.715 | 0.260 | 10.10 |
| 机关 | 4 / 80 | 50.05 | 19.80 | 1.379 | 0.622 | 40.76 |
| 机关 | 8 / 160 | 50.00 | 19.80 | 1.200 | 0.513 | 80.81 |

最终六档采样窗口均无空 Poll，所有房间持续提交连续 tick。物理阶段耗时来自 Frame.StepTime，包含前置机制/输入与原生 Step/读回；不含 Nest 提交和网络。机关 8 房间的 Nest Commit P99 为 0.367ms，提交间隔 P99 为 21.312ms，载荷约 5.96kB/s/逻辑客户端。普通 8 房间样本的实际均频有下降，不能把这份短测写成“稳定满 50Hz”的承诺。

**边界与下一步：**此负载直接调用 Service，不含 Gate/NATS/TCP、Unity 或 WAN；真实网络正确性另由专用回归验证。Go 分配/GC 覆盖测试进程与驱动，不包含 PhysX 原生堆，不是 RSS。机关 8 房间五秒中采集到 141 次 Go GC，累计 GC pause 约 28.3ms；整体分配的 profile 定位与削减见下一节（2026-10-29）。当前证据不足以宣布生产容量上限，也不能归因所有尾延迟；长稳、真实链路和更密集接触需独立测量。

复跑命令（已构建原生库，加载仓库 Go 环境后）：

```sh
go test -run '^$' -bench BenchmarkRecipientViewIndex -benchmem -count=3 ./gameplay/physics
ROOST_PHYSICS_LOAD=1 go test -tags physx -run '^TestMultiRoomReplicationLoad$' -count=1 -timeout=2m -v ./wiring/physicsroom
ROOST_PHYSICS_LOAD=1 go test -race -tags physx -run 'TestMultiRoomReplicationLoad/mechanisms/8$' -count=1 -timeout=1m ./wiring/physicsroom
```

负载测试默认不启用，SKIP 不算通过；race 档用于并发正确性，性能数字取不带 race 的六档输出。测试输出 JSON 含原始统计、实际频率、空 Poll、分配和 GC，方便同口径复测。此轮不改变 RP2/RC2、完整对象大小或 native ABI。

### Go 分配 profile 与削减（2026-10-29，macOS ARM64）

**提交号**：本节与下一节的测量跑在 rebase 进 main 之前的等价提交上，表中已换成 main 上的对应提交。被测的物理房间依赖代码逐字相同：本节的差异只在 saga / NATS 代码与文档，下一节只差 `bb42ef9e`（`infra/base/index` 的 NaN 守卫重构，不在 `service/physicsroom` / `wiring/physicsroom` 的依赖里）。证据目录里的 pprof 文件名保留 rebase 前的前缀。

**环境**：Apple M5（10 核）、macOS 26（Darwin 25.5）、Go 1.27.3、GOMAXPROCS=10；原生库按 [CHUNK-PHYSICS](CHUNK-PHYSICS.md#原生构建) 用同一份 PhysX 4.1.2 源码在私有构建目录重新编出 ABI 2 arm64 动态库。负载与上一节同口径（`TestMultiRoomReplicationLoad`，每房 20 个逻辑客户端，预热 1 秒、采集 5 秒），只跑 8 房间两档。与上一节 Linux amd64 / GOMAXPROCS=4 的数字不是同一台机器，只在本节内部比较。测试输出新增 `MallocsPerSecond`、`AllocKBPerFrame`、`AllocsPerFrame`：进程级分配按交付帧摊分，含每 Tick 与驱动本身的分配，只用于同口径前后对照。

**结果**（基线 = c8825078 的代码加上报告字段的测试提交 `48607d55`，之后 = `188e302e`；两个预编译测试二进制同机交替 3 轮，取中位数，方括号为 3 次样本范围）：

| 场景 8 房间 / 160 客户端 | Go 分配 MB/s | 分配次数 k/s | KB / 交付帧 | 分配次数 / 交付帧 | 5 秒 GC 次数 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 机关 基线 | 81.57 [81.56–81.73] | 520.0 | 25.75 | 164.2 | 164–167 |
| 机关 之后 | **31.15** [31.15–31.16] | **191.5** | 9.83 | 60.5 | 52–55 |
| 普通 基线 | 70.04 [69.75–70.04] | 450.7 | 22.11 | 142.3 | 177–193 |
| 普通 之后 | **27.38** [27.35–27.39] | **173.4** | 8.64 | 54.7 | 60–62 |

两档分配字节下降约 61～62%，分配次数下降约 62～63%。物理均频 50.00Hz、交付 19.80Hz/客户端前后相同；复制载荷不变（下述字节金样）。本机基线机关 8 房间 81.6MB/s 与上一节 Linux 的 80.81MB/s 接近，说明这部分分配主要来自 Go 侧逻辑而不是平台。

**基线分配归属**（机关 8 房间 alloc_space，总量含 1 秒预热；`go tool pprof -sample_index=alloc_space`）：

- 每接收者每次交付（`Service.Poll` 累计 57%）：`ReplicationFrame.Plan` 22%（视图账本 map 9%，快照对象表逐个追加扩容 8%，载荷副本 2%）；`Cursor.plan` 19%（每次新建基线 map 6%，`binary.Write` 装箱与 bytes.Buffer 扩容约 4%，对象字节副本 3%，分组 map 3%，逐对象字段编码 2%）；`SpatialFrame.Visible` 11%（对象表扩容 7%，可见集合 map 4%）；`RoomPayload` 与返回副本各约 1.5%；指标 key 拼接约 2%。
- 每房间每 Tick（约 42%）：`updateGrabs` 8.5%（每 Tick 复制全部位置的 poses map 与按 spec 重建的 dynamic map）；run 回调复制整帧状态 4%（只供 updateGrabs）；`World.Step` 7.6%（原生交换缓冲 + 返回状态）；`ReplicationScene.Frame` 6%；`spatialFrame` 4%；`QuantizedSnapshot` 3.4%（每对象 34 字节落在 48 字节规格）；`StateDAO.Accept` 5%；输入命令追加扩容 1.3%。
- CPU profile 中 Go 业务代码占比小，主要是调度唤醒与 cgo；GC 标记约 6～12%，`mallocgc` 约 5%。

**改动与逐项证据**（每项与前一状态交替 3 轮；括号内为机关 / 普通 8 房间 MB/s 中位数）：

| 提交 | 改动 | 机关 | 普通 |
| --- | --- | ---: | ---: |
| `2eafcaaf` | 抓取校验查已提交帧共享索引 `r.viewFrame`，可控集合建房时固定；删除只供它用的 `r.states` 每帧副本 | 81.30→71.10 | 69.85→59.64 |
| `eba63477` | `Plan` / `Visible` 对象表与可见集合按已知规模预留 | 71.14→63.54 | 59.96→53.31 |
| `bfbffdbc` | `Cursor.plan` 精确算帧长一次分配，`AppendUint*` 代替 `binary.Write`，单切片按 (Chunk, ID) 排序代替分组 map，RC2 字段直接写进帧，基线未变时沿用旧副本 | 63.43→59.85（487k→267k allocs/s） | 53.45→50.74 |
| `9a776601` | `Commit` 换下的基线 map 清空后留作下一次 plan 的 spare | 59.79→53.16 | 50.84→45.58 |
| `7e231810` | 成员视野 / 视图账本双缓冲：ACK 换下的旧 map 交给 `VisibleInto` / `PlanInto` | 52.87→42.75 | 45.24→37.70 |
| `af4b5092` | `World.Step` 的 input / raw / seen 交换缓冲按 World 复用 | 42.91→39.79 | 37.73→35.01 |
| `fb4bf820` | 量化快照对象载荷切自同一块数组 | 39.79→39.40（233k→200k allocs/s） | 35.01→34.77 |
| `2d70b207` | 每 Tick 输入命令按轨迹数 + 成员数一次预留（`AppendMotionCommands`） | 39.40→38.94 | 34.77→33.75 |
| `59156e2b` + `c59eb16e` | 游标临时变化表复用；房间级 Poll 临时对象表复用 | 38.78→33.62 | 33.77→28.83 |
| `188e302e` | 修正 `59156e2b`：`Commit` 的 `*c = p.next` 曾覆盖变化表，复用只在未提交重做时生效 | 33.81→31.15 | 28.96→27.38 |

没有改动因无收益被回退；`59156e2b` 单独时因上述覆盖几乎无效（变化表反而从一次预留变成逐次扩容），由 `188e302e` 修正后计入收益。

**复用缓冲的所有权**（都不离开所属锁、不交给网络或异步消费者）：

- `Cursor.spare`：`Commit` 换下的上一代基线 map。旧 Plan 的 `next` 只在 `Commit` 通过序号校验时被读取，持有这张 map 的 Plan 已提交过、序号不会再匹配；Plan 取走 spare 后归该计划，未提交则随之丢弃。`Cursor.changes`：只在一次 `plan` 调用内使用，返回前清零。两者都依赖游标“调用方串行使用、不按值复制”的既有契约（文档已注明）。`Plan.Data` 每次新分配，不与任何复用缓冲共享。
- 成员 `spareView` / `spareVisible`：ACK 时被替换的旧账本 / 旧视野，只由该成员在 `r.mu` 下持有；交给 `PlanInto` / `VisibleInto` 后成为 `next*`，规划失败时随结果丢弃。spare 永远不是 previous（previous 是刚确认的那份）。账本里的 `ViewEntry` 按值复制，载荷字节由 Plan 复制或沿用已确认条目。
- 房间 `visibleObjects` / `viewObjects`：只在 `Poll` 持 `r.mu` 期间使用；`Cursor.PlanFields` 返回后游标与视图账本都已复制对象值，二者清零后留给本房间下一次 `Poll`。交付载荷来自 `RoomPayload` 的新数组，`Delivery.Payload` 再复制一次，与这些缓冲无关。
- `World.input` / `raw` / `seen`：只在 `w.mu` 下使用、每次使用前清零；C 侧在调用返回前读写完毕、不保留指针；返回给调用方的 `[]State` 每次新建。
- 量化快照共享数组：每帧新建、生成后不再修改，各对象切片容量截断到本对象；需要改写的 `Plan` 先复制。

**一致性证据**：`TestPlanFramesStayByteIdentical`（chunksync，RC1/RC2 进入、离开、跨 Chunk、未确认重做、epoch 切换）与 `TestRoomReplicationPipelineStaysByteIdentical`（physics，量化快照、AOI、视图分档、字段增量、私有确认，并断言场景覆盖 near / middle / far / motion / 暂缓 / 移出 / 跨 Chunk）的金样取自优化前代码，之后每个提交都逐帧字节相同；后者对“新建”和“按 Poll 方式复用”两种方式都校验，把 spare 错设为 previous 会变红。`-race`（含 `-tags physx` 与机关 8 房间负载）通过。

**剩余**（`c59eb16e` 的机关 8 房间 alloc_space，约 211MB / 7 秒；`188e302e` 又去掉其中的变化表扩容）：每 Tick 部分占比升到约一半以上——`ReplicationScene.Frame` 每帧状态索引 map 15%、`spatialFrame` 位置 map 与 Block 切片 10%、`StateDAO.Accept` 的所有权副本与查重 map 12%、`World.Step` 返回状态 9%、`QuantizedSnapshot` 7%；每接收者部分剩 `PlanInto` 新载荷副本与分档计数 map、`RoomPayload` 拷贝与 `Delivery` 返回副本、帧缓冲、指标 key 拼接（`metrics.metricKey` 每次带标签调用拼一个字符串，约 4%）。这些要么是有意的所有权副本（DAO、Delivery），要么需要改变每帧索引结构（排序切片 / 按刚体槽位）才能再降，单项收益估计不超过 2MB/s，本轮未做（重新 profile 后其中状态索引与 AOI 索引两项实测约 5.4 与 1.9MB/s，已在同日晚削减；原生堆、RSS 与 macOS 长稳也已测，见下节）。未覆盖：Gate/NATS/TCP 链路。

复跑（证据、脚本与 pprof 在 [physicsroom-alloc-20261029](../performance/benchmarks/physicsroom-alloc-20261029/README.md)）：

```sh
GOWORK=off go test -tags physx -c -o /tmp/after.test ./wiring/physicsroom
docs/performance/benchmarks/physicsroom-alloc-20261029/measure.sh after=/tmp/after.test > runs.txt
python3 docs/performance/benchmarks/physicsroom-alloc-20261029/summ.py runs.txt
```

### 长稳、RSS 与原生内存（2026-10-29，macOS ARM64）

**只覆盖 macOS ARM64**：Apple M5（10 核）、macOS 26.5.2（25F84）、Go 1.27.3、GOMAXPROCS=10，PhysX 4.1.2 ABI 2 arm64 动态库（按 [CHUNK-PHYSICS](CHUNK-PHYSICS.md#原生构建) 用 `.physics-deps/PhysX-4.1` 源码在私有构建目录编出）。Linux 的复测、RSS 与长稳都没有做（本机 Docker 未运行）。

**入口**：`TestPhysicsRoomSoak`（`wiring/physicsroom`，`physx && cgo`，`ROOST_PHYSICS_SOAK_MINUTES` 启用，缺省跳过）与上一节同一夹具：机关场景 8 房间 × 20 逻辑客户端（每房 83 个非静态刚体），每个客户端按 Relay 节奏提交输入后 Poll；不含 Gate/NATS/TCP。每 15 秒一行：Go `runtime/metrics`（heap 对象字节与个数、累计分配、GC 次数、goroutine、Go 映射与已归还）、`ps` RSS、该窗口的物理 / 交付频率与分位数；每个窗口的原始记录整体换出，夹具自身内存不随时长增长。进程外 `sample.py` 每 30 秒跑 `footprint`（物理占用；untagged VM_ALLOCATE 即 Go 堆与栈映射，MALLOC_* 即 libc malloc），每 2 分钟跑 `vmmap --summary`（malloc 区已分配字节与个数）。

**原生内存的口径**：binding 用 PhysX 自带 `PxDefaultAllocator`，没有分配回调或计数器；它在 macOS 上走 libc `malloc`，Go 运行时不用 malloc，所以 malloc 区已分配字节就是 PhysX 加少量 libSystem 的原生堆（空 Go 程序约 12KB）。另给“估算原生 = footprint − Go 映射 dirty”，它还包含 malloc 元数据、页表与库的 dirty 数据段。`vmmap` 会挂起目标进程约 0.4～0.5 秒，含它的窗口物理均频约 48.7Hz，下面的频率判断排除这些窗口。

**结果**（20 分钟连续运行，`067d4606`，物理房间依赖的产品代码同 `ddc3d3d0`；预热 5 分钟后为稳定窗口，斜率为最小二乘、每分钟）：

| 指标 | 起点 | 5 分钟 | 终点 | 稳定窗口均值 | 斜率 / 分钟 |
| --- | ---: | ---: | ---: | ---: | ---: |
| footprint（MB） | 25.7（建房中） | 42.5 | 40.9 | 41.4 | −0.020 |
| Go 映射 dirty（MB） | 13.5 | 28.9 | 27.1 | 27.7 | −0.023 |
| 估算原生 = footprint − Go dirty（MB） | 12.2 | 13.67 | 13.72 | 13.70 | +0.003 |
| malloc 已分配（vmmap，MiB） | 9.15 | 9.64 | 9.66 | 9.64 | +0.002 |
| malloc 分配个数（vmmap） | 6365 | 6527 | 6526 | 6525 | −0.07 |
| RSS（ps，MB） | 49.9 | 56.2 | 56.7 | 56.5 | +0.035 |
| Go heap 对象字节（MB，4 分钟段内最低 / 最高） | 8.3 / 14.0 | 9.9 / 14.1 | 9.6 / 13.9 | 12.0 | 锯齿 |
| Go 映射 − 已归还（MB） | 25.9 | 31.4 | 30.5 | 31.2 | +0.003 |
| goroutine | 206 | 206 | 206 | 206 | 0 |
| 物理均频 / 房间（不含 vmmap 窗口最低） | — | — | — | 49.85（最低 49.93） | 0.000 |
| 交付频率 / 客户端（Hz） | 19.33 | 19.73 | 19.53 | 19.55 | −0.016 |
| Step P99 / Poll P99（ms） | 1.15 / 0.60 | 1.27 / 0.47 | 1.31 / 0.46 | 1.22 / 0.44 | ≈0 |

累计分配 30.57MB/s、GC 4.8 次/s，80 个窗口空 Poll 合计 0，每个房间每个窗口已提交 tick 连续。

**判断：没有发现无界增长。**footprint、Go 映射 dirty、Go 映射 − 已归还、goroutine 稳定窗口斜率均为 0 附近；Go heap 对象字节每 4 分钟段的采样最低值（第 4 分钟起）9.9 / 9.9 / 10.0 / 9.6MB，不随时间升高；PhysX 侧 malloc 已分配第 2～18 分钟 +0.04MB（10,087,424→10,126,336 字节，约 0.4%），分配个数 6513～6527 持平，没有逐 Tick 泄漏的形态（每秒 400 个 Tick，逐 Tick 泄漏哪怕 1 字节也会是每分钟 24KB 且个数随之增长）。RSS 稳定窗口 +0.035MB/分钟，而 footprint 持平、RSS − footprint 由 11MB 升到约 15.8MB：增长落在不计入 footprint 的干净页（代码段等文件映射逐步换入），**是推断，没有逐区域核对**。交付频率由前 8 分钟 19.66Hz 降到最后 8 分钟约 19.48Hz（−0.9%）：空 Poll 为 0，交付频率就是客户端循环频率（50ms 定时器 + 提交与 Poll），Poll P99 与物理频率没有同步变化，倾向本机定时器 / 后台负载，**原因尚未确认**。8 房间的 PhysX 原生堆约 9.6MiB（含 SDK / foundation 共用部分，平均每房间不超过约 1.2MiB），没有按房间分开测。

**优化后复跑**：下文两项削减之后（`c3946490`）同口径 4 分钟：估算原生 13.58MB 持平（+0.005MB/分钟）、malloc dirty 10.11MB 不变、footprint 41.7MB、goroutine 206、累计分配 23.66MB/s、GC 3.73 次/s。

**每 Tick 分配的继续削减**（HEAD 重新 profile 后只做两项，各与前一状态同机交替 3 轮，取中位数）：

| 提交 | 改动 | 机关 8 房间 MB/s | 普通 8 房间 MB/s |
| --- | --- | ---: | ---: |
| `e04c2efd` + `f9e01cfc` | `ReplicationFrame` 每帧状态索引由 `map[uint64]State`（83 个刚体约 12KB / 帧）改为按 ID 严格递增的切片、二分查找；房间提交路径 `AdoptFrame` 直接只读引用本 Tick `World.Step` 新建的切片，不再复制 | 30.95→25.56 | 27.29→21.78 |
| `c3946490` | `SpatialFrame` 的位置 map 与 Block map 改为与对象同下标的位置切片、按 (Block, 下标) 稳定排序的条目切片，查询对每个 Block 二分找起点 | 25.44→23.54（187k→168k allocs/s） | 21.76→19.89 |
| 合计（`067d4606` 对 `c3946490`） | | **30.90→23.46**（−24%，189.0k→167.5k allocs/s，GC 54→37～39 次 / 5 秒） | **27.29→19.87**（−27%，171.4k→151.8k） |

物理 50.00Hz、交付 19.60Hz/客户端前后相同；`TestPlanFramesStayByteIdentical` 与 `TestRoomReplicationPipelineStaysByteIdentical` 逐帧字节不变。第二项单独约 1.9MB/s，在“约 2MB/s”门槛边上，保留的理由是三轮范围不重叠、分配次数降 10%，输出次序另由 `TestSpatialVisibilityKeepsBlockOrder`（逐对象参考实现对照有序 / 乱序输入；旧实现通过，同 Block 次序反转即变红）锁住；若维护者认为不值得，可单独回退这一提交。

**所有权**：

- `ReplicationFrame.states` 建好后只读。`Frame` 复制输入；`AdoptFrame` 输入已按 ID 严格递增时直接引用、否则复制后排序（同 ID 取最后一个，与原 map 覆盖一致），从不修改调用方切片。房间提交回调里 `f.States` 是本 Tick `World.Step` 新建的切片（原生 `std::map` 按 ID 有序读出）：`Commit` 已返回、量化快照与 AOI 索引已复制所需值，之后没有人修改它。`service/physicsroom.Commit` 的注释写明返回后不得再修改 `Frame.States`，需要保留的实现自行复制；`EntityHost` 经 `StateDAO.Accept` 本来就复制。
- `SpatialFrame.positions` / `blocks` 每帧新建、建好后只读。重复 ID 时非观察者对象改用各自位置（原来是同 ID 最后一个的位置），只影响随后被 `chunksync` 以 duplicate object 拒绝的出错路径；观察者仍取最后一个。

**未做与原因**（`c3946490` 机关 8 房间 alloc_space 约 142MB / 7 秒）：

- `StateDAO.Accept` 的状态副本（约 14%，约 3MB/s）：这是 Nest undo 保护的权威事实与网络侧索引之间的所有权边界。去掉它要让 DAO 与本帧只读索引共用一块切片，经可插拔的 `Commit` 跨 service / wiring 两层传递“不得修改”的约定，一处违约就会静默改坏权威状态；收益约 3MB/s，判断为高风险，不做。查重 map（约 4%，约 0.8MB/s）低于门槛，不做。
- `World.Step` 返回的状态切片（约 13%）：现在它本身就是被 `AdoptFrame` 引用的本帧索引，按帧保留，不能复用。
- `QuantizedSnapshot`（约 10%）：每帧一块不可变载荷，供所有接收者共享，已是一帧一次分配。
- 每接收者部分（`PlanInto` 载荷副本、`Cursor.plan` 帧缓冲、`RoomPayload` 与 `Delivery` 返回副本）：都是交给网络或视图账本的所有权副本，上一轮已削到每次一次分配。

**仍未覆盖**：Linux（amd64 / arm64）的复测、RSS 与长稳；比 20 分钟更长的运行（小时级、房间反复创建与销毁）；Gate/NATS/TCP 真实链路下的内存；PhysX 原生堆按房间、按刚体数的拆分。

复跑（证据、脚本在 [physicsroom-soak-20261029](../performance/benchmarks/physicsroom-soak-20261029/README.md)）：

```sh
GOWORK=off go test -tags physx -c -o /tmp/soak.test ./wiring/physicsroom
docs/performance/benchmarks/physicsroom-soak-20261029/run.sh /tmp/soak.test /tmp/soak20 20   # 约 20 分钟
python3 docs/performance/benchmarks/physicsroom-soak-20261029/summ.py /tmp/soak20/go.tsv /tmp/soak20/proc.tsv 300
```
