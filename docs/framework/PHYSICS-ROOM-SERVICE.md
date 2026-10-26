# 物理房间 service

2026-10-25，当前未发版。复用 [PhysX 与 Chunk](CHUNK-PHYSICS.md)，目标是单个 owner 进程承载多个独立物理房间。领域包 `service/physicsroom`，装配包 `wiring/physicsroom`。原验收宿主 `cmd/physics-viewer` 已于 2026-10-26 删除：与服务重合的场景由正式装配承担，本机入口是 `wiring/physicsroom/cmd/physicsroom-local`（见[本机三进程](#本机三进程正式装配2026-10-26)），原生与 Unity 的数值对照移到 `scripts/physics/comparison-host`。

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

默认值是准入限制，不是容量承诺。建房按最坏每动态对象 75 字节加 36 字节头检查整帧预算，保证分散到不同 Chunk 也不越界；总待确认帧内存上界约 MaxRooms×MaxPlayers×MaxFrameBytes，另计世界、快照和编码临时分配。NATS bus 自身 WorkerNum/QueueCap、Gate 出站预算需要部署方配置。

Shutdown 先封闭服务准入，取消所有房间，然后等待各自 done。若业务 Commit 不返回，Shutdown 超时必须保留依赖和 native 资源；Mod 提供 StopWithContext，供 app 执行这个契约。指标通过 servicemetrics 报 create/input/join、create 重放/容量拒绝、成员过期、房间失败与房间深度，不使用 RoomID/PlayerID 标签。

## 验证与边界

定向测试覆盖容量竞争、相同 Create 重试、旧 Epoch/成员拒绝、pending 重发、非法 ack、重新加入令牌、帧预算、停机等待在途提交。Nest 实测覆盖 Guard 内 StateDAO；真实 NATS 测试覆盖生成的七个 RPC、MessagePack 和固定 SID；真实 Gate/NATS/TCP 测试覆盖认证 Binding、三帧连续解码与 Relay 取消清理。测试中的轻量 Commit 替身只用于隔离服务或网络契约，Nest 链路有单独的真实 Engine 验证。

这些是组合部件验证，不是跨三个独立 OS 进程的完整生产部署。尚未测多房间容量上限或长稳，Linux 按维护者要求暂缓。客户端输入协议/PB 与开房权限属于业务入口，需要业务接线；当前未修改 Unity 展示端的登录/开房界面。Raycast、碰撞业务事件、运行期刚体增删不在本阶段接口内。

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

Input 新增 Jump，Acceleration 只允许 X/Z（Y 必须为零）。同序号重放不重新触发跳跃，也不延长旧输入 TTL；新移动输入不会覆盖尚未消费的跳跃，过期跳跃不被后续输入复活。服务器用排除自身的向下射线检查支撑面与竖直速度，确认落地后加 7m/s 速度增量；该判定不是通用 CharacterController API。native command 布局变化，必须重建 native 静态库及 Go 程序，旧库会因新桥接符号缺失而链接失败。

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
| `game`（sid 22） | NATS Mod、etcd Mod（只发现）、`NewClientMod(7801)`、`gate.NewGameIngressMod`、`PlayerIngress` | 首个输入时按需建一个 CrowdScene 房间（寿命 1h，停服时销毁），slot N 控制刚体 50+N |
| `gate`（sid 11） | NATS Mod、etcd Mod（只发现）、`gate.NewGateMod` | TCP `127.0.0.1:19093`，经 etcd 找到 sid 22 的 game |

本机专用的只有 `physics-slot-N`（N=1..20）票据鉴权与 Seat，都在 main.go；正式部署由业务提供账号鉴权、开房与选座，其余照搬。依赖 NATS（4222）、Redis（6379，App 单实例锁，`wiring/gate` 要求启用）与 etcd（2379，Gate 发现 Game）。比原双进程多了 Redis 与 etcd：它们是 `wiring/gate` 正式装配的前提，不为本机另造一套。

```sh
scripts/physics/build-native.sh   # 一次
GOWORK=off go build -tags physx -o artifacts/physicsroom-local ./wiring/physicsroom/cmd/physicsroom-local
artifacts/physicsroom-local physicsroom --config wiring/physicsroom/cmd/physicsroom-local/configs/config.physicsroom.yaml
artifacts/physicsroom-local game        --config wiring/physicsroom/cmd/physicsroom-local/configs/config.game.yaml
artifacts/physicsroom-local gate        --config wiring/physicsroom/cmd/physicsroom-local/configs/config.gate.yaml
```

GoLand：`.run/` 下 Physics Service / Physics Game / Physics Gate 三个配置，Physics Stack 一次起三个（启动顺序无要求：房间在首个输入时创建，Gate 在登录时解析 Game）。三个配置 `log.level: debug`：连接、加入、离开逐次记录，输入每 20 个序号和每次跳跃记录，Chunk 每 20 帧记录。停止时先停 Gate、Game，再停 Physics Service；同一套配置不能起第二份（单实例锁让后起的等待后失败）。

链路：Unity → Gate（RS v2 握手、`physics-slot-N`）→ NATS → GameIngress → PlayerIngress → 固定 owner 的 Rooms RPC（MessagePack）→ 房间 PhysX 50Hz → RoomEntity 的 Nest Guard / StateDAO → AOI / Chunk → Relay → PushBound → Gate → Unity。

验证（2026-10-26，macOS ARM64，PhysX 4.1.2，私有 NATS / Redis / etcd）：

- `TestRealGateInputMovesRoomBodyAndStreamsChunks`（wiring/physicsroom，`integration && physx && cgo`）：EntityHostMod 经 Registry 提供、`NewServer` 登记 RPC，客户端经真实 Gate / GameIngress / PlayerIngress 发输入；断言受控刚体在已提交的 Entity 状态里沿 +X 移动超过 1m、客户端连续收到同 Epoch 的 Chunk 帧，断开后 PlayerIngress 在期限内排空。把输入加速度改成 0 时该用例在 8s 期限处失败（`moved=false`），断言落在输入生效上。`-race -count=3` 通过。没有 PhysX SDK 时这条用例因 tag 不满足而不编译：先 `scripts/physics/build-native.sh`，再按下文 `verify-service.sh` 运行。
- `TestPlayerIngressJoinsOnceAndReleasesWithTheConnection`、`TestPlayerIngressCloseRacesInput`（无 tag）：首输入只 Join 一次、断开后 Leave、Relay 出错关闭连接、Close 后拒绝输入且不再启动 Relay。
- 三进程实跑：按上面的配置（端口换成私有）启动三个子命令，Python 客户端以 `physics-slot-13` 登录，3s 内收到 39 个 Chunk 帧、40 个输入回执；game 日志记录按需建房、Join 刚体 63、跳跃输入。SIGTERM 依次停 gate、game、physicsroom，均正常退出。Unity 实机本轮未重跑。

边界：这是本机拓扑，SID、消息号与 Gate 地址是 main.go 常量（game 启动时核对自己的 sid）。不是多房间容量或长稳验证；没有多 owner 路由，Room Ref → owner SID 的映射仍由业务维护。
