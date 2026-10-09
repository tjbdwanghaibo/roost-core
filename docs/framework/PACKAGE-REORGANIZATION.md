# 包目录分类、Wiring 与 Service 收敛方案

状态：**目录、Service/Wiring 拆分已实施；全仓回归与 CBM 刷新已完成；Gate 已落码，性能验收暂停**。日期：2026-10-09；起始基准：`ef640e6e`。当前源码采用 `framework / infra / gameplay / service / wiring`，原 Kit 接线迁为 Wiring，领域和周期运行归 Service，Ops 与 StatsLog 运行归所属模块。下文第 2 节及迁移表记录改前位置，当前源码见 [全包索引](PACKAGES.md)。验证结果见 [实施验收](../maintenance/DIRECTORY-GATE-VALIDATION.md)。

## 1. 结论与范围

五个主要分区分别表达框架运行能力、基础设施、玩法能力、领域服务和便捷接线。分类目录本身不放 Go 源码，不创建总入口包；现有叶子包尽量保留包名、职责和内部布局。仍使用一个 Go module，Wiring、生成器和客户端随仓库一起维护。

Wiring 延续 Kit 的设计初衷：Game 或其他服务通过少量 Mod、配置和 option 即可使用正式能力。它负责把依赖接好，而不是要求业务自行管理数据库连接、恢复顺序、投影器和关闭顺序。删除旧入口与 alias 时，同步修改生成器和消费者，不能以分层为由把接入复杂度转嫁给业务。

这轮分阶段提交与验证：目录和 import 迁移、Service/运行逻辑与 Wiring 分离、独立 Gate 实施。目录搬迁不应改变行为；拆分接线需验证构造与生命周期；MessagePack、TCP 提取、NATS 转发与广播属于 Gate 新行为，单独验收。本轮目录完成不等于 Gate 开发完成。

`framework` 表示服务器框架；`gameplay` 表示可选玩法能力。Gate 设计中的 Game 仍指游戏服务进程，与 gameplay 目录不同。仓库名 roost-core 和当前 module 名不因分类名称而改变。

三大模块继续局部聚集，不增加统一的 controller/usecase/repository 层级，不按接口拆包。基础设施允许接口与 driver 的必要隔离。

## 2. 当前结构与实际问题

当前单模块根目录混放 ECS、调度、持久化、网络驱动、集合、日志、玩法和工具。两处 service 是领域迁移尚未完成；Kit 也并非全部属于接线。

| 当前位置 | 当前职责 | 收敛方向 |
| --- | --- | --- |
| `app/service.go` | 进程 Service 的 Init / Serve / Shutdown 接口 | 归 framework/app，继续拥有进程生命周期机制 |
| `service/mail`、match、session | 领域、存储、接口与 RPC 传输 | 保留为正式领域入口 |
| `kit/service/account`、chat、directory、global、global/activity、platform、rank | 领域实现与 Mod/RPC Server 混放 | 分到 service/<领域> 与 wiring/<领域> |
| `kit/service/mail`、match、session | Mod、Server/run、ClientMod、领域 alias | 运行实现归领域；接线归 wiring；删除 alias |
| `kit/ops` | 运维 HTTP handler、鉴权、监听、停止，以及配置接线 | 运行实现归 infra/observe/ops；接线归 wiring/ops |
| `kit/statslog` | Nest/Entity 采集、窗口、周期采样、文件写入，以及配置接线 | 运行实现归 framework/statslog；接线归 wiring/statslog |
| `servicemetrics` 与 `kit/service/servicemetrics` | 实现与转发入口 | 只保留 infra/observe/servicemetrics |
| `gateway` 与生成 TCP 逻辑 | 边界工具在 gateway；监听、收发在生成器 | 同轮搬 gateway，并在 Gate 阶段提取唯一 TCP runtime |

App 引用 Nest；Cache 引用 SyncBus/Mirror；StatsLog 采集 Nest/Entity 并维护采样窗口，因此这些能力归 Framework。Spatial 为 AOI/玩法共同使用的空间原语，归 Infra。归属按实际依赖判断，不能仅凭名字把运行代码都塞进基础设施。

## 3. 目标目录

```text
roost-core/
├── framework/
│   ├── app/                 # Registry、配置、进程生命周期、singleton
│   ├── nest/                # 正式调度与快慢池
│   ├── entity/              # ECS、Guard、变更
│   ├── dataengine/          # 持久化契约与 engine
│   ├── sync/                # Entity Sync、Lockstep、SyncBus、SyncStream
│   ├── remoteentity/
│   ├── nestwal/
│   ├── saga/
│   ├── cache/
│   ├── configdata/
│   ├── hotcode/
│   ├── manager/
│   ├── event/
│   └── statslog/            # 框架统计采集、窗口与周期/文件运行
├── infra/
│   ├── base/                # 时间、集合、并发、安全、空间原语
│   ├── network/
│   │   ├── gateway/         # 已有边界工具 + TCP/Gate/Game Ingress 运行实现
│   │   ├── nats/            # NATS 契约与 driver
│   │   ├── bus/
│   │   ├── servicerpc/
│   │   └── …
│   ├── storage/             # Mongo、Redis、版本存储、显式迁移工具
│   └── observe/
│       ├── ops/             # 运维 HTTP、探针、管理接口的运行实现
│       ├── metrics/
│       ├── health/
│       ├── log/
│       └── …
├── gameplay/
│   ├── actionflow/
│   ├── ai/
│   ├── attribute/
│   └── skill/
├── service/                 # 领域、存储、RPC 传输与服务运行逻辑
│   ├── account/
│   ├── platform/
│   ├── directory/
│   ├── global/
│   ├── activity/
│   ├── mail/
│   ├── chat/
│   ├── rank/
│   ├── match/
│   └── session/             # 副本/试炼等运行会话，不是网络连接会话
├── wiring/                  # 延续 Kit 的便捷接入能力，只有接线和启停委托
│   ├── account/、mail/…     # 本地 Mod / 远端 ClientMod / 进程接线
│   ├── gate/                # Gate 与 Game Ingress 的两端接线
│   ├── nest/
│   ├── dataengine/
│   ├── remoteentity/
│   ├── mongo/、redis/…
│   ├── ops/、statslog/
│   ├── mods/
│   └── internal/
├── internal/                # 全模块共享的非公开契约
├── client/
├── robot/
├── codegen/
├── cmd/
├── demo/                    # 工程生成模板
├── examples/
├── scripts/
├── docs/
└── artifacts/               # 保留验收证据及当前管理规则
```

Framework/Infra/Gameplay 是导航分类，不增加聚合门面。正常 import 指向具体包，例如 `…/framework/nest`、`…/gameplay/skill`、`…/wiring/nest`。Infra 内四个分类目录本身也不创建 Go 包。

## 4. 完整迁移映射

除显式拆分外，整个子树随父目录搬迁，叶子 package 名保持。工具、客户端、模板与示例引用同时更新。

| 当前目录 | 目标目录 |
| --- | --- |
| app、nest、entity、dataengine、remoteentity、nestwal、saga | 分别为 framework/<原名> |
| sync | framework/sync，保留 entitysync、policy、frame、lockstep、nettransport、syncbus、driver、mirror |
| syncstream | framework/sync/syncstream，不并入 entitysync |
| cache、configdata、hotcode、manager、event | 分别为 framework/<原名> |
| actionflow、ai、attribute、skill | 分别为 gameplay/<原名> |
| clock、timer、container、index、safemap、misc、fctx、goroutine、worker、lock、lifecycle、errcode、featureflag、security、spatial | 分别为 infra/base/<原名> |
| bus、servicerpc、ownerroute、etcd、nats、gateway、httpclient、httpserver、webroute | 分别为 infra/network/<原名>，driver 随子树保留 |
| mongo、redis、versionstore、migration | 分别为 infra/storage/<原名>，driver/mongotest 随子树保留 |
| admin、log、metrics、health、failurelog、servicemetrics | 分别为 infra/observe/<原名> |
| service/mail、match、session | 保持位置；补入所属运行逻辑，成为唯一领域入口 |
| kit/service/account、chat、directory、global、platform、rank | 领域及服务运行实现迁入 service/<原名>；接线迁入 wiring/<原名> |
| kit/service/global/activity | 领域及服务运行实现迁入 service/activity；接线迁入 wiring/activity |
| kit/service/mail、match、session | Server 的运行实现归 service/<领域>；Mod/ClientMod/Server 接线归 wiring/<领域>；删除领域 alias |
| kit/service/servicemetrics | 删除转发包；直接使用 infra/observe/servicemetrics |
| kit/ops | handler、监听、鉴权、排空归 infra/observe/ops；配置/Registry/启停委托归 wiring/ops |
| kit/statslog | 采集、窗口、文件及周期运行归 framework/statslog；配置与接线归 wiring/statslog |
| 其他 kit/* | 接线迁入 wiring/<原名>；发现运行状态/循环时放回对应 Framework/Infra 能力，不机械整体改名 |
| kit/service/examples、kit/service/integration | 接线示例/跨服务测试归 wiring/examples、wiring/integration；领域测试随领域迁移 |
| 根 internal/*、codegen/internal/* | 保持所属可见范围，不能统一移入 infra/internal |
| kit/internal/* | 随 Kit 接线迁入 wiring/internal，配置生成器消费者同步改路径 |
| client、robot、codegen、cmd、demo、examples、scripts、docs | 保持入口位置，同步相关路径、链接与模板 |

迁移后不保留 kit 源码入口。通用 migration 仍是显式工具，不恢复 DAO 自动迁移；废弃 cube 仓库不在范围内。

## 5. Service 内容与 Session 含义

| 领域包 | 主要实现 |
| --- | --- |
| account | 身份验证、会话令牌、建角、选角 |
| platform | 平台身份及管理能力 |
| directory | 全局名字预留、提交、释放 |
| global | 静态 Player/区服路由 |
| activity | 活动组、窗口、协调、结果投递 |
| mail | 信封、邮箱、发送与领取状态 |
| chat | 频道、消息保留、读取 |
| rank | 排行更新、查询、幂等 |
| match | 排队、分组、匹配、取消 |
| session | 副本/试炼等运行记录与外部资源释放 |

这些领域可以嵌入同进程或独立部署；目录不强制一包一进程。Directory 当前是嵌入能力。

Session 的领域对象叫 Run，记录 OwnerID、Kind、RequestID、期限、终态与 Resources。其接口 Enter/Attach/Finish/Leave/Get/Current 用于开始运行、登记外部资源、结束或退出并查询状态；业务必须提供 Releaser，框架不解释资源 Kind 的玩法含义，也不负责模拟副本场景。

例如某玩家进入试炼：Enter 建立 Run；业务分配场景后 Attach 记录资源引用；完成、退出或到期时走正式释放流程。SweepPending 处理待准入记录与可扫描 owner 的过期运行，释放/存储失败不能伪造成功。它不是 Gate socket 会话、Account 登录令牌或 Sync SessionID；四者保持独立身份与生命周期。本轮保留 service/session 名称，文档和关键注释明确“运行会话”。

领域模型、错误、幂等、存储实现、RPC wire/handler/BusClient，以及服务自己的 sweep/retry/运行循环放在 service/<领域>。同包按职责分文件，不新增 service/<领域>/biz/data/server 的机械分层。

## 6. Wiring 的边界与便捷接入

### 6.1 可以放什么

- ConfigSchema、配置读取/校验，以及转换为所属模块的普通 Config/Options。
- DependsOn、Registry Lookup/Register、协作者绑定和消费者 public/local capability 接线。
- 本地 Mod、远端 ClientMod、实例构造与初始化失败清理的委托。
- 给 App 接上运行对象的 Start/Serve/StopWithContext；只委托模块的正式生命周期方法。
- 生成器和业务需要的便捷入口，如 wiring/nest.NewMod、wiring/account.NewMod/NewClientMod/NewServer、wiring/gate 的两端接线。

Wiring 可以持配置、依赖引用和接线结果。单纯出现 Start/Stop 方法并不意味着代码不属于 Wiring；判断标准是其中是否自己实现了队列、重试、业务状态、循环或资源协议。像 DataEngine Mod 的 Start 委托已有 engine Assembly.Start 是合理接线，应保留该便利。

### 6.2 哪些实现迁出去

| 具体实现 | 所属位置 | Wiring 保留什么 |
| --- | --- | --- |
| Entity/事务/恢复/WAL/投影/Remote 状态与实际关闭协议 | framework 对应模块 | 配置、依赖与生命周期委托 |
| Session Sweep、Activity 投递重试、Chat 清扫等领域周期工作 | service/<领域> | 传入运行参数，并接上领域 Run/Server |
| RPC handler 执行、领域 Server 的运行与排空 | service/<领域> | 取得本地实例与 Bus，构造并登记 Server；远端 ClientMod 接线 |
| 运维 HTTP 路由、鉴权、监听、发送与排空 | infra/observe/ops | 配置、观测依赖和启动/停止委托 |
| Nest/Entity 统计采集、窗口推进、周期/文件运行 | framework/statslog | 身份、配置、提供者注册和生命周期委托 |
| Gate TCP、绑定状态、NATS 收发、续期、队列、广播与排空 | infra/network/gateway | 认证/发现/进程身份/dispatch/Sync/Lockstep 依赖接线 |

Ops runtime 通过注入的观测接口/函数读取 readiness、stats、命令等，不反向 import Framework/Registry。StatsLog 本身与 Nest/Entity 关联，放 Framework，避免为了“全搬到 Infra”再制造一套快照和适配层。

普通 runtime Config 放所属模块；Viper/YAML 配置声明留 Wiring。业务仍选择 Mod/ClientMod 和少量 option；依赖排序、资源借用/拥有、启动失败清理和真实排空由现有 App 与模块契约协作完成，调用方不手写这些流程。

### 6.3 RPC 生成物拆分

当前 Mail 的 assembly 生成物依赖 alias.go，并包含 Server/run 接入。本轮生成器须同步调整：领域的传输/Server 运行实现以显式依赖构造，留在领域包；接线生成物显式导入领域类型，只处理 Registry、OwnerCapabilities、ClientMod 与 App 适配。

wiring/<领域>.NewServer 可以作为便利入口，返回实现 app.Service 的薄接线对象：在 App 正确初始化阶段取得依赖、构造领域 runtime；Serve/Shutdown 委托该对象，不在接线对象中写 ticker 或执行 sweep。保留 Mod Init/Provide/Start → Service Init/Serve 的现有时序；不能提前读取尚未发布的能力，也不能让 ClientMod 发布本地 Server。

生成器现有 transport/assembly 输出模式可继续表达这两类输出，assembly 指生成接线，不是新目录名称。修改生成器输入与模板后重生成，不手改生成物；旧 alias/转发包全部删除。Account 的 RegistryBound 检查留在 Wiring，必填业务协作者与绑定时机保持。

```go
import (
    "github.com/tjbdwanghaibo/roost-core/service/account"
    accountwiring "github.com/tjbdwanghaibo/roost-core/wiring/account"
)
// 业务类型/接口来自 account；便捷装配入口来自 accountwiring。
```

import 别名只解决使用处的同名，不复制领域类型。领域测试随运行代码迁移；配置/构造/Registry/生成 Server 接线测试留 Wiring；真实生命周期与 split 集成跨边界验证，不为测试方便公开全部内部状态。

## 7. Gate 同轮纳入

本轮同时覆盖现有 Gateway 搬迁、生成 TCP 提取与已确认的独立 Gate 实施，具体契约见 [Gate 方案](GATEWAY-IMPLEMENTATION.md)。

| 内容 | 目标位置 |
| --- | --- |
| 已有 Principal/Session/Endpoint/中间件 | infra/network/gateway，同步所有消费者 |
| TCP、RS 包头、连接、writer、绑定、预算、续期、广播、NATS adapter、Game Ingress runtime | infra/network/gateway，同包分文件，embedded 与独立 Gate 共用 |
| Account 鉴权、固定路由发现、incarnation 校验、App 配置及两个部署端的初始化接线 | wiring/gate，通过明确依赖注入 runtime |
| 业务 ProtocolRegistry、Nest Sender、PB 编解码、业务/Room 拥有者操作 | codegen 生成接线与业务 Game；仍进入正式调度 |
| Sync 交付/生命周期与 Lockstep Room/恢复 | framework/sync；接入统一 Gate 出站，保留原语义 |

Gate 是接入运行能力，本轮不新建 service/gate 领域包，也不把连接表、控制协议或 NATS callback 放进 Wiring。infra/network/gateway 不 import Wiring、Nest、Entity 或业务生成物；鉴权、发现、绑定身份核对和业务 dispatch 通过显式注入，适配函数只连接已有能力。

目录阶段先搬已有 gateway。Gate 阶段再把通用 TCP 从生成器提取进目标包，接 Core NATS 和 MessagePack，并实现 Bind/Activate/Lease、PB 广播、Sync/Lockstep、故障/排空与性能验收。两者同轮跟踪，分阶段提交；不能把移动 gateway 文件记成独立 Gate 已交付。

## 8. 依赖与必须保留的契约

主要方向：Wiring → Service / Gameplay / Framework / Infra；Service、Gameplay → Framework / Infra；Framework → Infra。Infra 按需引用其他 infra 叶子包；共享客户端 wire 原语仍可被框架和接入使用。实施时以实际 Imports/TestImports 检查，不能把分类图当成已证明的严格拓扑。

Framework、Infra、领域 Service 禁止反向 import Wiring；Infra 不引入 Entity/Nest 业务实现。各核心叶子包仍须无循环 import。根 internal 保持全模块可见范围；codegen/internal 保持工具内部范围，wiring/internal 只属于接线，不通过公开化或复制文件绕过可见性。

| 契约 | 验收要求 |
| --- | --- |
| Nest | 同 ID FIFO、快慢池、准入/拒绝保持；慢池不拿 Entity local 锁 |
| Entity/Sync | setter 标脏；handler 全部变化在 Guard 释放前冻结，解锁后按水位唤醒；on_change 与 20Hz 兜底保持 |
| DataEngine/WAL | strict、未知、投影、恢复、关闭语义保持，不增加旧格式兼容 |
| Remote/Saga | 固定拥有者、代际、持久 fence、未知与补偿保持 |
| Service | RPC 标识、业务错误、幂等、public/local 区别和必填协作者保持 |
| 生命周期 | 真实排空再关依赖，超时不等于完成，借用资源不重复关闭 |

核对包路径对 schema/类型标识、反射、插件和注册表的影响，验证 DAO descriptor、WAL、RS/Sync/Lockstep golden 与 RPC 标识。编译成功不证明格式未变；隐式身份变化须查明并形成稳定契约，不能自动迁移或清空业务数据。

## 9. 实施与验收

| 阶段 | 工作 | 完成标准 |
| --- | --- | --- |
| R0 基线 | 包列表、实际依赖、生成/测试状态与协议身份 | Mac/Linux 证据，失败/跳过分别记录 |
| R1 目录 | Framework/Infra/Gameplay/已有 Gateway 搬迁；接线入口迁为 Wiring | 全部 import/生成字符串/脚本/链接同步，可构建；不混算法改动 |
| R2 运行与接线 | RPC 生成物分离；Session 等运行、Ops、StatsLog 迁出接线；先完成已拆领域 | 删除 alias 后可重生成/编译，生命周期和真实 split 接线通过 |
| R3 领域 | 其余七个领域及运行实现统一到 Service；删除剩余 Kit/转发入口 | 领域回归、配置、构造、Server、本地/远端与停机验证通过 |
| R4 全仓 | 更新索引/维护/使用文档、工具和真实消费者；刷新 CBM | 全仓构建/测试/生成及消费工程验证，路径和 generation 对齐 |
| G1～G5 Gate | 按已确认 Gate P1～P5，在新目录提取 TCP、接编码/转发/广播/Sync/Lockstep，验证故障与负载 | 本轮 Gate 交付另有真实链路与性能证据；不以 R4 完成替代 |

阶段结束保持可构建；生成器与对应生成物同阶段提交。检查包括：

1. 实际 Go 包/测试导入、Mac/Linux 编译与适用测试；交叉编译不是执行测试。
2. build/vet/全仓测试、受影响并发包 race、领域幂等/未知/恢复和生命周期组合，真实资源用独立命名空间。
3. 第一轮生成核对预期变化；第二轮生成必须零漂移；重新生成并运行真实消费工程，保留少量 option/Mod 的便利。
4. 搜索旧 import、模板、命令、文档与门禁路径；跟随真实模块位置，不删除检查或添加无效代码强过门禁。
5. 正式 Nest → Entity/DAO → WAL/投影 → Sync 回归；核对协议身份，复跑相关短基准，行为变化再追加相同负载验证。
6. CBM 刷新、新路径 coverage 和旧路径清理；保留原始压测证据，不重写历史结论。
7. Gate 按其完整资源/身份/故障/性能矩阵验收，保留 P99≤50ms 与偶发长尾追踪要求。

## 10. 升级与证据

导入路径与类型声明位置改变，属于消费者源码不兼容重构，应单独发布。建议按不兼容主版本处理；发布方案需统一版本号、Go module 主版本路径、生成器最低版本与消费者升级说明，不能只改 tag。上表相对于 module 根，示意 import 暂沿用当前 module 前缀。

不保留旧 import 门面、alias 或双版本接线；消费者使用匹配工具重新生成。目录重构不应要求持久格式迁移；额外行为确需跨格式时，仍由匹配旧程序完成所需数据落库，不能清空数据。

本次使用 CBM Verify（Tier 2），项目 Users-whb-roost-roost-core，generation `2026-10-08T11:31:15Z`。除前轮 Account/Mail/App 证据外，本轮核对 Session 的接口、Run/Resource/Config、SweepPending、原 Server.run，以及 Ops、StatsLog、DataEngine Mod。精确路径 coverage 无记录缺口且 metadata_match；图谱共同名字存在启发式连边，实际源码作为职责依据。

关键新增证据：`service/session/{types.go,service.go,session_rpc.go,sweep_source.go}`、`kit/service/session/{server_run.go,session_rpc_assembly_gen.go}`、`kit/ops/ops_mod.go`、`kit/statslog/statslog.go`、`kit/dataengine/mod.go`、`app/{mod.go,service.go}`。前轮根 coverage 已完整分页，排除的源码/模板以直接读取补证；不宣称全调用图或所有 build tag 已审计。

R0～R3 已完成。R4 的全仓测试、真实生成消费者、build/vet、受影响包 race 和两次生成已通过；CBM 已刷新到当前工作树，Linux amd64 交叉构建通过；未执行 Linux 测试。Gate 阶段尚未实施，不能以目录回归代替 Gate 或整服性能验收。
