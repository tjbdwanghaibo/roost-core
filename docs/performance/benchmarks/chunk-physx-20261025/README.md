# Chunk / PhysX 偏差与服务器资源验证（2026-10-25）

工作树：`roost-core-review-v127`，基线 `ee3aa6d1` + 本轮未提交实现。macOS 26.5.2 arm64、10 个逻辑 CPU，Go 1.27.3、原生 PhysX 4.1.2、Unity 6000.3.12f1。本轮没有强设 GOMAXPROCS。不是 Linux 或生产 Gate 验收。

## 偏差不是运动随机数造成的

初始圆周位置固定，四阶段施力由 ID 和 tick 唯一决定，没有 Random 调用；epoch 的 crypto/rand 只标识流代际。同一原生构建新建世界重复 600 拍，逐刚体完整状态严格相等，`TestComparisonSceneRepeatable` 在 race/count=2 下通过。因此当前无需增加一个不参与计算的“固定种子”。未来增加随机规则时，两端必须消费同一份固定种子输入带，而不是假设 Go/C# 的 Random 同种子会产生相同序列。

对照先对齐创建顺序（静态物体 → 球 → 胶囊）及刚体 maxAngularVelocity=50、maxDepenetrationVelocity=10、sleepThreshold=.005。仅做这一步并没有消除偏差；599 拍平均位置误差 6.798321m。没有把调参当作修复成功。

### 确认的问题与修复

- 首次接地（tick 7）法向速度两端约 -0.70384m/s，但切向速度原生 -0.864197m/s、Unity -0.718265m/s。原生胶囊只锁角速度，仍用有限转动惯量参加接触求解，切向冲量被分配给随后丢弃的转动。设置无限转动惯量后原生为 -0.718262m/s。`TestLockedCapsuleContactFriction` 检查滑动摩擦冲量关系，旧实现失败、修复后通过。
- scene/body 打开 CCD 不够，默认 filter shader 没有为接触对开启 CCD。补全非 trigger 的 eDETECT_CCD_CONTACT。`TestCCDStopsFastBallAtThinWall` 旧实现球从 x=-2 穿到 x=+2，修后被墙阻挡。此缺陷独立于低速场景，修 CCD 单独复测的 599 拍结果不变。
- 修前/修后单元证据：[regression-before.txt](regression-before.txt)、[regression-after.txt](regression-after.txt)。负对照仅临时撤回两个修复点，随后恢复并重建最终库。
- 真实 Unity 诊断：[首次接触修前](before-inertia-contact.txt)、[首次接触修后](after-contact.txt)。两份 `before-inertia-*.csv` 是修正惯量前的重复运行；结果一致。`unity-after-50hz.csv` 是修正惯量后的同 tick 对照。

599 拍（11.98s 模拟时间）修后平均位置误差 **0.535059m**，最大 **3.668985m**，球 **0.190816m**。相对直接修前 6.798321m，平均误差下降约 92%；相对上一日最初版本 5.609412m 下降约 90%。这只是该时间点的误差，不能说整段或长稳误差有相同比例下降。

修后前 88 拍最大误差不超过 0.00001m；tick 89 为 0.000752m，tick 90 为 0.001262m。剩余差异在多刚体接触后累积，尚未逐项隔离 Unity 集成与原生 SDK 的接触/浮点实现差异。不能断言已完全定位，也不能保证两端独立模拟长期一致。NVIDIA 的[确定性条件](https://nvidiagameworks.github.io/PhysX/4.1/documentation/physxguide/Manual/RigidBodyDynamics.html)要求同版本、同平台、相同场景插入顺序和时间步；固定种子本身不足以保证跨宿主一致。

## 最终服务器结果

| 发送上限 | CPU（单核100%） | RSS峰值 | 实收下行 | 实收带宽 | 固定1s桶最大字节 | 物理步平均 / 最大 | 完成模拟拍数 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 20Hz | 4.18% | 19.20 MiB | 27.07 kB/s | 0.217 Mbps | 29.88 kB | 337.23 µs / 2.724 ms | 2996 |
| 50Hz | 4.55% | 19.70 MiB | 68.26 kB/s | 0.546 Mbps | 72.57 kB | 310.07 µs / 3.592 ms | 2997 |

两档均约 60 秒；20Hz 接收 1199 包、50Hz 接收 2997 包，全部包序连续且始终保有 21 个对象。物理总拍数略少于名义 3000，包含等首基线后再启动物理及调度影响；不以模拟时间替代墙钟。50Hz 的下行约是 20Hz 的 2.52 倍，CPU 增加约 0.37 个单核百分点。平均物理步的轻微差异属于本机单次样本变化，不能据此说提高发包率会让物理更快。

## 资源测量方法

50×50m、20 个胶囊 + 1 个动态球、5 个静态碰撞体，50Hz 固定物理，25m Chunk，1 个 TCP 接收端观看整个场地。原生 dispatcher 1 worker；Nest short/long/io 为 2/1/1。分别以 20/50Hz 发送上限连续运行 60 秒，顺序执行。最终测量窗口内没有启动 Unity、编译或测试；客户端只收包并核验完整 21 个刚体与流序号。

`measure-server.py` 用 wait4 获得**服务器子进程**所有线程的 user+system CPU 时间，除以该进程墙钟时间；100% 表示一个核心，不是整机百分比。包括 Go/Nest、native PhysX、编码和 socket 系统调用，排除 Python 接收端与 Unity；包括短暂启动/关闭，未扣空载成本。RSS 是该进程峰值。字节按实际收到的 RSv2 包计数，除以首次至末次接收的墙钟时间，包含首个基线，不含 TCP/IP、ACK、链路层开销。单秒峰值为从首次接收起的固定 1s 桶，不是任意滑动窗口或瞬时链路峰值。

`server-before-20hz` 为修正惯量/CCD 前的诊断样本，其间存在本轮编译/测试活动，不用于最终性能对比。最终结果用 `server-20hz` 和 `server-50hz`，每个目录保留 `result.json` 和 `host.log`。物理耗时取宿主日志，只含输入生成、原生 Step 与读回；不能替代全进程 CPU。

复跑（先构建原生库和宿主，编译不进入测量窗口）：

```sh
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
python3 scripts/physics/measure-server.py --server artifacts/physics-viewer --seconds 60 --hz 20 --output artifacts/physics-cpu-20
python3 scripts/physics/measure-server.py --server artifacts/physics-viewer --seconds 60 --hz 50 --output artifacts/physics-cpu-50
```

接收脚本使用 macOS/Linux 的 wait4；不支持 Windows。默认独占 loopback 19092，失败不停止其他进程，只清理自己启动的服务器。当前只是 21 个动态刚体的轻载数据，不可据此线性外推万刚体或千客户端容量，也没有 CPU 峰值分位、真实公网链路流量或能耗结论。

CBM 本轮索引仍因共享后台协调状态启动失败，coverage 返回未索引；相关新模块以直接源码核对和实际回归为依据。

## 最终复核

20Hz Unity 复测 240 个对照样本，50Hz 599 个样本，均到 compared_tick=599；240 个共同 tick 的位置、旋转、速度误差及服务器球位置全部相同（CSV 记录精度），确认切换发送频率不会改变此次独立物理结果。记录见 [20Hz](unity-after-20hz.csv) 与 [50Hz](unity-after-50hz.csv)。最终 C# 编译与播放通过，相关四包 race/count=2、go vet 通过；Python 资源脚本实际运行两档并通过语法编译。Unity 日志仍有先前已记录的编辑器 QuickSearch 内部异常，不把自动流程 PASS 等同于编辑器全日志无异常。
