# Chunk 同步与原生 PhysX

**当前 Crowd：20 客户端、20Hz 同步、71 个动态刚体、立体地形与 XYZ AOI。** 下方较早的 50 人与平地段落是演进记录，当前契约见文末“当前场景”。

状态：2026-10-24 本地可运行首版已实施，生产接线与扩展边界见下文。基线 ee3aa6d1。用户授权 core 直接接入原生 PhysX，本地 Unity 6000.3.12f1 新工程作左右对照；不运行 Unity Server。

## 范围与边界

- infra/physics/physx：可选 CPU 原生后端，薄 C ABI、批量命令与快照，资源由原生层拥有。非物理工程不需要 PhysX。
- framework/sync/chunksync：按区域聚合的版本化基线与增量。稳定对象 ID、跨区原子应用、显式帧序、可靠传输断开后重建基线，不按任意字节拆对象。
- gameplay/physics：单世界执行权、固定步长、输入与结果交接。不得在 Nest handler 持锁期间推进 PhysX，不把 CPU 模拟放入 io 池。
- 本地独立 Unity 工程：左侧只显示服务器状态，右侧独立 Unity PhysX；相同输入、场景参数与 Tick，记录同 Tick 误差，不能把画面延迟当成物理误差。

50×50 米、20 胶囊、一个半径 2 米球、围墙，25 米 Chunk。全部 Chunk 共用一个物理世界。物理 50Hz，发送 20Hz 或每拍；20Hz 合并只允许合并绝对物理状态，不丢弃可靠业务事件。

## 实施顺序

1. 钉住官方 SDK 版本、验证 macOS 原生构建与球体/胶囊碰撞；保留可复跑构建脚本，不提交依赖二进制。
2. 提供正式可选后端与 Chunk 协议，验证基线、迁移、删除、失序拒绝、断线重建。
3. 接入 Nest 与物理执行生命周期，增加正式展示宿主和 Unity 工程生成/导入入口。
4. 本机真实通信、Unity 编译/运行检查、物理回归与同 Tick 对比。Linux 构建/运行未执行时明确标注。

## 正确性要求

原生句柄不保留 Go 指针；关闭与 Step 串行；输入限额、非有限数拒绝、原生调用失败明确返回。物理世界有单一写入者，发布结果不可变。所有客户端使用独立帧游标，失败发送不能推进游标。重连必须新基线。网络包有明确上限。

坐标采用米、Y 向上；胶囊轴和旋转显式映射。场景参数版本必须匹配。原生 PhysX 与 Unity 集成不是同一实现，不承诺跨平台确定性。先验证完整精度，再比较量化。

当前动态物理演示不持久化逐拍位置，不提供 UGC 编辑存储、跨机物理世界迁移、动态刚体跨物理世界碰撞或经济事务。原生模拟是不可回滚外部状态；物理结果提交失败时停止该运行，不能继续模拟并伪称事务回滚成功。

## 验证记录

实际命令与结果见文末。CBM 工作树索引失败（worker exit=1），旧 main 图谱为 2026-10-14；相关入口已按当前工作树源码核实，不把旧图谱当成本轮覆盖。

## 当前实现（2026-10-24）

已落地上述三个 core 包、`cmd/physics-viewer` 验证宿主与可复制创建的 Unity 工程源码。现有 Nest / EntitySync 行为未改，Chunk 是独立的新协议，不给旧 EntitySync 载荷套假格式。

### 原生构建

采用官方 PhysX **4.1.2**，源码分支 4.1，固定 NVIDIA 上游提交 [a2c0428acab643e60618c681b501e86f7fd558cc](https://github.com/NVIDIAGameWorks/PhysX/commit/a2c0428acab643e60618c681b501e86f7fd558cc)（属于第三方仓库，并非 Roost 提交）。5.4.2 与 5.1.2 的公开源码构建包未包含 Mac 平台 CMake 文件；未将未验证的 5.x Linux 配置作为 Mac 构建使用。4.1 的 Mac 构建文件原为 x86_64，本轮脚本明确适配 arm64、deployment target 11.0，并把 `V4SplatElement` 模板改用已有的常量 lane 分支，解决新 clang 对未选中分支的 lane 参数检查。未改物理求解算法。

第三方旧代码的 `-Werror` / `-Weverything` 不沿用为现代编译器门禁，其他 warning 仍保留；这不是忽略业务测试。native 基础落球与推球行为有实际回归。依赖源码和编译产物不入 Git，构建输出包含 NVIDIA 许可证。当前上游版本较旧，本轮不是对其安全维护状态或跨平台一致性的背书。

需要 CMake、Ninja、C/C++ 编译器、Python 3、Git、Go 和 CGO。构建：

```sh
scripts/physics/build-native.sh
scripts/physics/run-viewer-server.sh -send-hz 20
```

本机隔离工具与依赖复用方式（不改系统 Python）：

```sh
PATH="/Users/whb/roost/.physics-deps/build-tools/bin:$PATH" \
ROOST_PHYSICS_DEPS=/Users/whb/roost/.physics-deps \
scripts/physics/build-native.sh
```

默认构建无 PhysX 依赖；尝试创建物理世界返回 `ErrUnavailable`。启用 `-tags physx` 且 CGO 可用才链接原生库。运行时须保留动态库或按部署方式配置动态库搜索路径。原生 Step 在 Nest short/long/io worker 上被拒绝；本轮执行器在独立 goroutine 上推进，等待 Nest 的提交结果也在执行器上。原生 worker 缺省验证为 1，不替业务开启 GOMAXPROCS 个线程。

### 世界与事务

本轮世界运行期静态创建，后端支持地面/墙、胶囊、球；动态创建删除、Raycast、碰撞业务事件、资源 cooking 不属于已完成 API。

`StateDAO.Accept` 在 undo/memory handler 内接受物理帧，检查 epoch、连续 Tick、ID 与有限数，复制固定值状态并登记回滚。成功回复后执行器向网络发布。Nest 拒绝会停止 Run，不能继续前进或悄悄跳拍。单位是米、Y 向上，胶囊沿 X 的原生 shape 显式旋转为 Y。通用 RunConfig 接受业务输入回调和固定步长，Snapshot 接受 Chunk 尺寸；20 胶囊输入仅由对照宿主选择。CPU 过载时执行器放慢模拟时钟，不无限追帧；因此不能仅凭 Tick×dt 声称真实墙钟吞吐达标。

### 同步契约

RC v1 基线清空对象表，增量包含删除和按 Chunk 聚合的绝对对象值。对象 ID 稳定，跨块更新覆盖唯一归属，整帧验证完成才替换 Replica。每连接单独 Cursor，发送整包成功才 Commit；失败断开，重连重新基线。单快照/包上限 1 MiB、对象数上限 16384，内存复制前检查预算，不任意切分。

本轮对象载荷固定 53 字节，包含局部 float32 位置、旋转、速度、角速度与休眠状态。相同对象值不重复发送，但本场景仍发送 Tick 元信息用于比较。尚未实现量化、字段 mask 或资源 CDN，不把这些设计项描述为已经节省的流量。

20Hz 合并的是物理结果的绝对状态；每个连接从自己已成功发送的对象表计算变化。不是丢弃已经生成的可靠增量。50Hz 选项同样由发送定时器取最新已提交快照，调度迟到时可合并，因此表示发送上限，不保证“一拍一包”。

宿主当前只允许 loopback，复用 RS v2 包头与独立消息 ID `0x50485901`。没有接入生产 Gate、鉴权或玩家 AOI：验收观察者观看整个 50×50 场地，订阅全部四个 Chunk。对象可跨块，四块仍共享一个原生物理 Scene。不能把本轮结果冒充 1000 玩家 AOI 验收。

### Unity 项目与复跑

本机项目 `/Users/whb/roost/roost-physics-viewer`，场景 `Assets/Scenes/PhysicsComparison.unity`，打开后 Play。创建新副本与自动验证：

```sh
scripts/physics/create-viewer.sh /绝对路径/新Unity工程
scripts/physics/verify-viewer.sh /绝对路径/Unity工程 20
scripts/physics/verify-viewer.sh /绝对路径/Unity工程 50
```

详细行为见 [客户端说明](../../client/physics-viewer/README.md)。源码在 core 内维护，新工程是显式复制的独立工程；未覆盖用户已有工程。

### 已执行验证

- macOS arm64 动态库实际构建；`file` 确认为 arm64。
- `go test -race -tags physx -count=2 ./infra/physics/physx ./framework/sync/chunksync ./gameplay/physics ./cmd/physics-viewer`：通过。包含落球接地、胶囊推球、多世界生命周期、业务 worker 禁止模拟、Chunk 迁移/删除/失序/截断、Nest 回滚与连续帧。
- `go vet -tags physx`（上述包）：通过。
- `go build ./...`：通过（普通无原生依赖构建）。
- `go test -count=1 .`：通过；首次沙箱下 robotdemo 因监听 EPERM 失败，放开本地测试所需权限后实际运行通过，未改测试绕过。
- Unity 6000.3.12f1：实际创建场景、编译、Play Mode 下接收 TCP、独立物理和渲染导图通过；20/50Hz 记录见[功能对比证据](../performance/benchmarks/chunk-physx-20261024/README.md)。仅编译或通信成功不代表数值一致。
- Linux 原生编译/运行、生产 Gate、多观察者、大数据 Chunk、长稳及容量上限未验证。
- CBM 最终刷新仍被共享后台协调状态阻止：`a pre-coordination or unverified CBM generation is active`。没有停止其他项目的服务，未宣称新代码已入索引。

### 偏差诊断与修复（2026-10-25）

本场景没有运动随机数：20 个出生位置是固定圆周，施力由 ID、tick 和四阶段输入带唯一决定。epoch 使用随机数仅用于区分连接代际，不参与模拟；不要为了“固定种子”把 epoch 固定，破坏流身份隔离。新增同一构建两次 600 拍逐状态比较，验证固定场景可复跑。未来加入随机业务输入时应生成一份带种子的公共输入带，避免 Go 与 Unity 各自使用不同 PRNG。

确认并修复：

1. **禁转胶囊接触摩擦不正确**。原先只设置 angular lock，接触求解仍使用有限转动惯量，摩擦冲量部分分配给随后被锁掉的角速度。首次接地（tick 7）两端竖直速度相同，但服务端水平速度 -0.864197m/s、Unity -0.718265m/s。原生胶囊设置三轴无限转动惯量（PhysX 用零值表示），保留锁标志；修后 -0.718262m/s。回归直接检查首次滑动接触的切向冲量等于 μ × 法向冲量，不把某次 Unity 输出硬编码成通过条件。
2. **CCD 开关不完整**。scene、body 开启 CCD，但默认 filter shader 只给离散接触对；新增 shader 为非 trigger 接触对启用 CCD。薄墙测试旧实现球从 x=-2 穿到 +2，修后被阻挡。它不是此次低速接地偏差的主因。
3. **对照条件对齐**。Unity 按原生顺序创建球和胶囊；两端显式使用最大角速度 50、最大脱离速度 10、休眠阈值 .005，避免依赖不同的默认设置。

修后并不承诺跨宿主逐位确定性。当前同 tick 对照前 88 拍误差不超过 0.01mm，约 tick 89 多刚体接触开始放大差异；接触生成、求解与编译浮点路径的具体剩余差异尚未逐项隔离。服务器权威状态同步仍是正式方向，客户端纯本地模拟用于展示对照，不可据此改成只同步输入。

问题记录、修前失败/修后通过、Unity CSV 与服务器 CPU/流量实测统一保存在[本轮证据](../performance/benchmarks/chunk-physx-20261025/README.md)。没有创建重复历史 bug 目录。

### 50 客户端控制场景（2026-10-25）

`physics-viewer -scenario crowd` 保留同一 50×50m 围场和 25m Chunk，创建 101 个动态刚体：ID 1..50 为自动施力的胶囊，51..100 是半径 .5m、质量 5 的小球，101 是半径 2m、质量 80 的自由大球。每个胶囊半径 .4m、总高度 2m、质量 10。小球从半径 18m 圆周出发，胶囊半径 10m；没有随机运动采样。

50 条独立 TCP 连接分别绑定槽号 1..50，控制 ID=50+槽号的小球。控制端每秒 20 条加速度输入，50Hz 物理执行器取最新合法输入，仍通过原有 Nest/StateDAO 成功提交后发布 Chunk 状态。输入邮箱不是可回滚的业务世界，连接只能提交期望加速度，不能提交位置；正式物理状态始终由执行器独占。连接断开清除输入，超过 250ms 未更新不再施力（保留真实惯性），不把失联当作瞬移停球。

当前为本地验收协议：crowd 连接首先发大端 uint32 槽号，0 是只读旁观者；控制报文是 seq:uint32、ax:float32、az:float32，均为大端，共 12 字节。序号必须递增、加速度有限且模长不超过 20m/s²；重复占槽/非法输入关闭连接。服务器最多 64 条连接；控制开始要等 50 个槽各自首个输入。握手/后续读取超时 2 秒，写超时 1 秒。此协议只用于现有 loopback 宿主，不是帐号鉴权或生产 Gate 协议，不提供输入确认/重传或端到端控制延迟承诺。comparison 模式仍使用原有无上行对照协议。

最初全场基准中，所有控制客户端完整接收 101 个对象；当前默认已启用下文 AOI，复跑全场需指定 `-aoi-radius 0`。控制是持续状态而非离散事件，因此输入邮箱可以仅保留最新值；不可照搬到开火/交易等不可丢事件。小球输入依据各客户端序号的固定四阶段规则，网络到达时刻可能影响应用在哪一拍，不宣称跨次网络测试完全确定。

Unity 新增 `Assets/Scenes/Crowd.unity`，只显示服务器权威状态：橙色胶囊、彩色小球、黄色大球。没有伪造第二套“同输入”本地物理对照；原 `PhysicsComparison.unity` 保留。创建或自动验证见 `scripts/physics/verify-crowd.sh`；先向已有 Unity 工程同步 `client/physics-viewer/*.cs` 及 `Editor/ViewerProject.cs`。新建工程由 create-viewer.sh 复制全部源文件。

资源复跑（无需 Unity）：

```sh
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
python3 scripts/physics/measure-crowd.py --seconds 60 --hz 20 --output artifacts/crowd-20
python3 scripts/physics/measure-crowd.py --seconds 60 --hz 50 --output artifacts/crowd-50
```

视觉验收：`scripts/physics/verify-crowd.sh /absolute/unity/project`。该轮同时运行 Unity，其资源数值不能混入上面的性能样本。数据与边界见 [50 客户端报告](../performance/benchmarks/chunk-physx-crowd-20261025/README.md)。

### Crowd AOI（2026-10-25，当前默认）

控制连接默认以自己小球的**当前已提交权威 XYZ 位置**为中心，12m 内进入、14m 外离开，自己始终可见。`-aoi-radius 12 -aoi-margin 2` 可配置，`-aoi-radius 0` 恢复全场基准。槽号 0 的只读 Unity 旁观仍看全场，不计入控制客户端性能测试；它只是本地诊断权限，不是生产帐号权限。

每次 Nest 成功提交后，`SpatialFrame` 同时冻结物理快照和 10m Block 索引，各连接共享只读视图。网络发送阶段查询邻近 Block，再按中心距离精确筛选；独立的 25m Chunk 仍只承担分组编码，两种尺寸不混用。索引按已提交完整物理帧构建，不改变既有 Entity Interest 的实现，也不另引入异步位置消息。原生物理仍模拟全部 71 个动态对象，不能因网络不可见就移除碰撞体。

连接以**上次成功发送的可见集合**判断滞留区，得到完整的当前可见快照后交给既有 Cursor。进入/重新进入发送完整对象，离开生成 delete；失败写出不提交 Cursor 或视野。观察者移动、对象移动及跨块同时发生时，全部使用同一帧坐标，避免混用两拍坐标。重连重建基线，不沿用旧连接的滞留集合。

按中心距离而非包围体表面判定，没有遮挡剔除或“最多 N 个对象”的硬截断；密集聚集仍可能看到全场。查询半径/缓冲与 Block 尺寸有有限值检查，Block 至少 1m、单次最多扫描 4096 格，异常配置明确失败。

`measure-crowd.py` 默认跟随以上配置，每包检查自己存在、ID 合法、对象距离不超过退出半径，并统计平均/最大可见数量和进出/重入。确定性回归把 Block 查询与全量距离扫描逐对象对照，另覆盖负坐标、观察者移动、跨 Chunk、滞留与重入后的 Replica 结果。没有放宽序号和对象状态校验来适配 AOI。

实测与复跑命令见 [AOI 报告](../performance/benchmarks/chunk-physx-aoi-20261025/README.md)。此前 101 对象全可见报告为历史基准；当前 71 对象全场对照可用 `--aoi-radius 0`，不等同于复现历史人数。


### 当前场景：20 人 / 20Hz / 立体地形（2026-10-25）

维护者指定后续以 20Hz 同步、20 个控制客户端继续。保留 50 个胶囊 NPC 与一个大球，所以动态对象为 71 个：1..50 胶囊、51..70 玩家球、71 大球。物理求解仍为 50Hz。此前 50 人/101 对象结果是历史基准，不再作为当前场景的启动配置；没有同时维护一条旧人数兼容路径。

地形唯一来源是 `gameplay/physics/terrain.json`，Go embed 后创建静态碰撞体，Unity 从相同 JSON 资源创建显示网格。包含地面/围墙、4m 低平台、6m 高平台、约 21.8° 连接坡道、12 级台阶、高架窄桥/护栏、桥下通道、低顶隧道、柱阵、台座与 15° 侧坡。均为解析 box 几何，不引入网格 cooking 依赖。桥下与平台下保留可通行空间，没有用实心大方块把整个空间填满。

`physx.Body.Rotation` 新增可选单位四元数，零值表示单位旋转；非有限数/非单位旋转明确拒绝。原生创建接口改为 `rp_add_pose`，静态碰撞体使用该姿态，避免只旋转显示网格。旧动态库与新 Go 宿主不得混用，需一并重建；未保留旧 ABI 适配。InitialFrame 同样保留动态刚体的初始旋转。

部分玩家从两个平台出发，其余分散于外圈；胶囊按所在地形顶面抬高初始位置。地形配置仅使用绕 Z 轴的坡度，出生点按 box 顶面和物体投影半径取高度，避免生成在平台或坡道内部；后续全部运动由原生碰撞决定，不逐拍贴地。加速度输入仍是水平控制，重力、斜坡和碰撞带来垂直位移，不伪造 y 方向动画。

AOI 继续用 XZ Block 作为候选粗筛，精筛改为 XYZ 欧氏距离；楼层差计入 12/14m 半径。它不做墙体遮挡或按楼层 ID 隔离，不能把“3D 距离”当成视线可见性。每帧仍只创建一次只读空间索引。

已增加旋转坡道落球、不同高度对象的 AOI 滞留、场景元数据与服务器人口一致性回归。当前实测与视觉证据见 [20 人 3D 报告](../performance/benchmarks/chunk-physx-3d-20261025/README.md)。默认仅验证 20Hz，不再为本轮追加 50Hz 档。

围墙随立体地形升至 12m，Unity 使用半透明材质以便观察内部。初轮 4m 围墙低于 6m 平台，出现越墙坠落，该轮数据已标记无效；回归逐拍检查完整世界，网络客户端也检查可见对象是否越界或坠落。已有 Unity 项目需补 `com.unity.modules.jsonserialize: 1.0.0`，并同步 Resources/PhysicsTerrain.json；新建脚本已包含该依赖。

### 阶段交付与平台边界（2026-10-25）

本阶段可以作为可选物理基础能力和独立 Chunk 同步协议提交。启用需要 `physx` build tag、CGO 和对应平台的原生库；默认 Go 构建不依赖 PhysX。macOS ARM64 已做真实物理、20 人 20Hz 网络压测和 Unity 显示验证；Linux 有链接配置但尚未完成原生构建及运行验收，Windows 不在本轮范围。

已具备球体/胶囊与静态旋转 box 碰撞、固定步长世界、Nest 提交瞬态结果、XYZ 距离 AOI、Chunk 全量/变化对象/删除与重入，以及 Unity 二进制解码展示。尚不包含生产 Gate 装配、UGC 编辑持久化、Raycast、碰撞业务事件或运行期刚体增删接口；不能将本地宿主验收等同于这些能力已完成。

提交前重新执行默认 `go build ./...`、四个相关包的默认测试、`-race -tags physx -count=2` 和带 tag 的 vet，均通过。Unity 与独立 60 秒性能结果沿用同一代码的已保存证据，没有重复压测或把编译测试阶段的资源占用混入性能数据。CBM 再次刷新仍为 worker exit=1，本轮源码检查不依赖旧索引。

根目录 `go test -count=1 .` 已通过，包含真实示例运行；首次沙箱禁止本地监听，获准后复跑。文档的 PhysX 第三方提交改为明确指向 NVIDIA 仓库的链接，未修改提交号门禁。

### 后续房间 service 方向与本轮补验（2026-10-25）

维护者确定后续将物理碰撞房间封装为独立 service。本轮没有提前增加 service 包、注册路由或生产 Gate 接线。复用现有 PhysX 世界与 Chunk 协议，房间级封装应明确：

- 一个房间一个 Scene/World、固定步长执行权、输入队列和已提交快照；body ID 可以在不同房间复用。进程级 Foundation/PxPhysics 共享，创建和释放必须串行保护其引用计数，各 Scene 模拟可并发。
- 创建房间后分配唯一代际；加入/退出由已认证会话控制，RoomID/代际必须参与输入与出站路由，不能沿用本地宿主的裸槽位授权。切房、重连重建 Chunk 基线。
- 停房顺序为停止准入、取消模拟、等待 Run 和已接纳的提交/发送退出，再 Close 世界；超时不能提前释放仍在使用的原生句柄。提交失败只停止所属房间，不能继续推进不可回滚的物理世界。
- service 对房间数、原生 worker 总数、每房人数/刚体/输入/出站预算设上限；不能无限地为每个房间增加线程。容量上限要用后续实际多房间服务负载另测。
- 生产 Gate 装配、Raycast、碰撞业务事件和运行期增删尚未实现，不能作为“已验证”交付。Linux 实跑按维护者要求暂缓。

补验发现并修复了并发开关房间时 PxPhysics 单例引用计数竞争。此前单房间验收没有覆盖此问题；这次以 8 个并发房间反复创建、模拟、销毁，重复使用 body ID 并校验不同房间轨迹，另验证房间提交失败隔离、取消退出，以及 Chunk 任意截断位置不产生半帧状态。证据见 [多房间补验](../performance/benchmarks/physics-rooms-20261025/README.md)。这属于底层多世界能力验证，并不意味着房间 service 已经实现。
