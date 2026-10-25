# Roost 原生 PhysX 对照客户端

用 `scripts/physics/create-viewer.sh /绝对路径/新项目` 创建 Unity 6000.3.12f1 工程。源文件由 core 保存；创建器拒绝覆盖已有项目。项目只依赖 Unity 内置 Physics、IMGUI 和仓库 RS v2 PacketCodec，不要求 Unity Server。

先在 core 运行 `scripts/physics/build-native.sh`，再运行 `scripts/physics/run-viewer-server.sh`。用本地 Unity 打开新项目的 `Assets/Scenes/PhysicsComparison.unity` 并 Play。

左侧为服务器原生 PhysX 经 Nest/Chunk 的状态，右侧为独立 Unity PhysicsScene。21 个动态刚体、四块 25×25 米区域。初始订阅完成后服务器才开始推进。相同输入带与固定 20ms 步长；客户端按本地时间独立模拟，保存 512 拍历史，对比时从历史取相同 Tick，不拿当前屏幕位置相减。

首次基线之后右侧不接受纠正。重连中途初始化不能恢复求解器内部接触缓存，因此不能当成从初态同跑的确定性试验。默认插值最多附加一个接收间隔；可在画面上关闭，不影响同 Tick 指标。右侧不代表含预测/回滚纠正的正式联机客户端。

每次运行在 `Application.persistentDataPath/PhysicsComparison` 输出 CSV，包含同 Tick 位置、旋转、速度误差、球位置、本地追赶耗时、累计接收字节。耗时为该渲染帧追赶的总耗时，不是单物理拍 p99。

## 批处理验收

先启动宿主（默认 127.0.0.1:19091），再调用：

```sh
"$UNITY_EDITOR" -batchmode -projectPath /绝对路径/项目 \
  -executeMethod ViewerProject.VerifyConnection -logFile /绝对路径/验证.log
```

不要加 `-quit`：验收进入 Play Mode 后由回调退出。至少完成 240 次对比、服务器推进至 Tick 599；超时、网络错误、协议缺口失败。启用图形设备时导出 `Artifacts/comparison.png`；`-nographics` 只验证物理与通信，不产生图片。

这是实际运行验证，不是两端轨迹一致性门禁。允许并记录物理偏差，不把连通成功称为数值一致。官方 Unity 6000.3.12f1 编辑器在本机批处理启动时出现过 QuickSearch 内部异常，原始日志保留，不能宣称编辑器日志零异常。

## 协议边界

宿主是仅监听 loopback 的验证应用，未接入生产 Gate/账号鉴权。复用 RS v2 公共包头，Sync 类型、消息 ID `0x50485901` 专用于本场景 v1 的 RC v1 Chunk 载荷。不要把它交给 EntitySync 的 `SyncReceiver`。新场景/物理参数需要双方一起升级场景消息 ID 或增加协商，不能静默混用。

创建脚本复制的源码是当前 core 版本的固定副本；修改后须显式同步 `Assets/Roost`，不复制 Library、Temp、UserSettings 或其他项目资产。

源码独立放在 `client/physics-viewer`，不放进现有 `client/unity` UPM 包，避免 Unity 6 的 Rigidbody / PhysicsMaterial API 改变原客户端包的最低 Unity 版本。

本场景的出生位置和施力是固定输入带，没有随机运动采样；连接 epoch 不参与物理。两端使用相同创建顺序和显式刚体参数。`TraceContacts` 可记录前 12 拍第一个胶囊的两端位置、速度，默认关闭。数值误差按同 tick 计算；修复禁转惯量后仍存在碰撞累积偏差，不能用固定随机种子承诺跨宿主确定性。诊断与服务器资源结果见 `docs/performance/benchmarks/chunk-physx-20261025/README.md`。

## Crowd 场景（当前 20 人 / 20Hz）

`Crowd.unity`：20 个彩色小球（各由独立 TCP 连接控制）、50 个橙色胶囊、一个黄色大球，以及平台/坡道/阶梯等立体地形。该场景只渲染服务器权威状态，原 `PhysicsComparison.unity` 双端对照保留。

自动验证并截图：

```sh
scripts/physics/verify-crowd.sh /Users/whb/roost/roost-physics-viewer
```

手动持续查看：先构建宿主，再运行以下命令（包含启动服务器和 20 个控制客户端），然后在 Unity 打开 `Assets/Scenes/Crowd.unity` 并 Play：

```sh
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
python3 scripts/physics/measure-crowd.py --seconds 3600 --port 19091 --hz 20 --output artifacts/crowd-live
```

上例一小时后退出，可以自行缩短 `--seconds`。CPU 性能数字应在 Unity 关闭时测；本地查看模式的 CPU 不用于独占基准对比。控制连接默认使用 12m 进入/14m 离开的 AOI，Unity 旁观仍显示全场；尚未接生产 Gate。增加 `--aoi-radius 0` 可复跑全场负载。20 个控制客户端共约 400 条输入/秒；网络输入会保持到下次更新，超过 250ms 停止施力，断开释放控制槽。


## 当前默认：20 人、20Hz、立体场景

当前 Crowd 已改为 20 个控制小球、50 个胶囊和 1 个大球（共 71 个动态刚体），同步默认 20Hz，物理仍为 50Hz。场景包括 4m / 6m 平台、坡道、12 级阶梯、高架桥及护栏、桥下通道、低顶隧道、柱阵和不同高度障碍。

静态几何的唯一配置为 `gameplay/physics/terrain.json`；创建工程时复制到 `Assets/Roost/Resources/PhysicsTerrain.json`。修改现有工程时，需同时同步该资源和 `client/physics-viewer/*.cs`，不能只修改 Unity 外观。服务器原生碰撞体已支持旋转；更新本轮代码后必须重建动态库和 Go 宿主。旧桥接 ABI 不保留。

当前控制连接使用 XYZ 球形 AOI（12m 进入、14m 离开），Unity 旁观仍显示整个场地。`Crowd.unity` 场景名保持不变，原来的平地双端 `PhysicsComparison.unity` 不变。本页的运行命令启动当前 20 人立体场景。

现有工程升级 3D 地形时，在 Packages/manifest.json 的 dependencies 中加入 `"com.unity.modules.jsonserialize": "1.0.0"`，并同步地形资源。新建脚本已包含。当前截图与验收见 [20 人 3D 报告](../../docs/performance/benchmarks/chunk-physx-3d-20261025/README.md)。

## Room 场景：按钮控制房间中的球

先构建 native 库，然后在 core 中运行：

```sh
GOWORK=off go run -tags physx ./cmd/physics-viewer -scenario room -listen 127.0.0.1:19093 -send-hz 20 -duration 1h
```

当前默认使用下文 Gate + Physics Service 模式。若使用上面的直连命令，需要在 Inspector 关闭 UseGate。打开本地项目 `Assets/Scenes/Room.unity` 并 Play。方向按钮按住移动，JUMP 点击跳跃；也支持 WASD、方向键和空格。默认控制第 13 个小球，摄像机跟随；可在 Inspector 修改 PlayerSlot（1～20），不同客户端不能占用同一槽。场景仍为 50×50 米复杂地形、20 球、50 胶囊和一个大球；本入口不运行自动 NPC 移动，后续由业务增加。松开方向键停止施加加速度，刚体仍按惯性和摩擦运动。

链路为 Unity 输入 → 本地 TCP 适配 → physicsroom.Submit → 原生 PhysX 50Hz → RoomEntity 的 Nest Guard / StateDAO → AOI / Chunk → Unity。同步上限 20Hz；客户端只插值服务器位置，不运行本地预测。移动限制为水平加速度，跳跃请求由服务器落地射线判定；一个请求只消费一次，空中不追加冲量。这是刚体控制验证，不是完整角色控制器（例如贴边起跳、台阶辅助）。

本地输入握手 `PR01 + slot:u32BE`；每次输入 `sequence:u64BE + ax:f32BE + az:f32BE + jump:u8` 共 17 字节。只开放 loopback，不替代生产 Gate 登录或开房鉴权。正式 service 的 Input 同样带 Jump 字段。

自动验证共用输入队列与发送路径，驱动水平移动和两次跳跃请求；它不是自动鼠标点击测试：

```sh
"$UNITY_EDITOR" -batchmode -projectPath /绝对路径/项目 \
  -executeMethod ViewerProject.VerifyRoom -logFile /绝对路径/room.log
```

不要加 `-quit` 或 `-nographics`。验证检查至少 200 帧、水平位移超过 2m、离地高度超过 0.8m，并截图到 `Artifacts/room.png`。手动场景不会启用自动输入。旧本地工程需同步 CrowdViewer.cs、Editor/ViewerProject.cs，无需额外输入包。批处理截图使用相机渲染，不含 IMGUI 按钮；按钮在交互 Play 模式显示。

### GoLand 直接 Debug

core 工程已提供 `.run/Physics Room.run.xml`。首次使用先构建 `infra/physics/physx/lib` 原生库（当前本机已就绪），然后在运行配置中选择 **Physics Room → Debug**。配置自动设置 `CGO_ENABLED=1`、`GOWORK=off` 和 `-tags=physx`，入口为 `cmd/physics-viewer` 的 room 场景，监听 `127.0.0.1:19093`，运行期限 1h。Unity 打开 `Room.unity` 后 Play 即可连接，无需另外启动 shell server。

这是 Go 调试器，可在房间输入、Nest 提交、Chunk 编码等 Go 代码打断点；C++ PhysX 的内部单步需要原生调试器。断点暂停整个进程较久时，Unity 的网络读超时可能断开，恢复服务后重新 Play 即可。


### Gate + Physics Service（当前默认）

本机 NATS 位于 `127.0.0.1:4222`。GoLand 先 Debug **Physics Service**，看到 `physics service ready` 后 Debug **Physics Gate**，看到 `physics gate ready` 后 Unity Play。旧 **Physics Room** 是直连配置，不能同时占用 19093。

- Physics Service：独立进程，SID 7801；房间执行、PhysX、RoomEntity / Nest。
- Physics Gate：独立进程，同时承载 Gate 与最小 GameIngress；Gateway 内部转发和 PhysicsRoom RPC 都实际经过 NATS。它不是独立 Game 服务部署。
- Unity：`Room.unity`，UseGate=true。RS v2 握手、`room_input.proto` 的 Protobuf 输入，内部 RPC 为 MessagePack，返回 RC v1 Chunk。
- `physics-slot-N`（N=1..20）仅用于 loopback 本地认证，不用于生产账号登录；每次 Gate 启动创建唯一临时房间，生存期 1h。退出时先停 Gate 再停 Physics Service。重启服务后需重开 Gate 和 Unity Play。

两个配置均设置 `-log-level debug`，日志打印到各自 GoLand Console。连接、加入、离开逐次记录；输入每 20 个序号记录一次，跳跃单独记录；提交每 50 拍记录，Chunk 每 20 帧记录（含首帧、字节数）。默认 info 时这些诊断日志关闭。

Room 场景没有 Rigidbody，也不在客户端调用 Physics.Simulate；Unity 会初始化自己的 PhysX 模块，但这不等于客户端在模拟这些物体。只有旧 PhysicsComparison 对照场景运行独立 Unity 物理。

2026-10-25 本机交互验证：两个 GoLand / Delve 进程实际运行，Unity 通过 Gate 连接；按 D 与空格后，Gate 日志记录 sequence=580 acceleration=[10 0 0]，sequence=600 jump=true，随后持续输出 Chunk。新协议解析测试与受影响包 race 通过。这是功能验证，不是 Debug 模式性能基准。批处理验收因本地项目已在 Unity 打开而拒绝启动，改用当前窗口交互验证，未关闭用户工程。
