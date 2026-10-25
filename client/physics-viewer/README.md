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
