# ARCH-12：sync 块的包结构整理——先划清范围，再局部聚集

- 状态：**已实施**（M-15～M-18，见 §6）；下文 §1～§5 保留实施前的盘点与方案，当前目录以 [sync/README](../../sync/README.md) 为准。后续可读性整理见 [09-23 方案](../feature/REFACTOR-2026-09-23-sync-readability.md)。
- 原则（维护者）：roost core 就是三块基础——nest 调度、dataengine、sync；其余是通用基建。review 最高优先级是这三块，且不只查 bug，还查包结构与代码构造。除通用基建外，三块的代码要各自**局部聚集**。sync 先整理清楚，再基于整理结果给方案。
- 前置：ARCH-10（M-13 / M-14）已落地；`room/` → `syncbus/driver`、`policy.Room` → `Group` 已在 `feature/arch-10-sync-policy`
- 后续：ARCH-11 的 M-15 / M-16 在本方案 S3 之后做，避免同一批文件搬两次

## 1. 盘点：core 里名字或职责带"sync"的 19 处，哪些属于 sync 块

判定标准只有一条：**它是不是"把状态从一个地方复制到另一个地方"这条链的一环**。是 → sync 块；只是被这条链用到的通用能力 → 基建；靠 sync 块做事的业务协议 → 消费者。

| 包 | 源码行 | 今天做什么 | 依赖（core 内） | 判定 |
|---|---|---|---|---|
| `entitysync` | 1525 | 机制：Manager、会话、subject 私有订阅、每会话一帧、prepare/commit | entity, statesync, nettransport, health, metrics | **sync 块 · 服务→客户端 · 机制** |
| `entitysync/policy` | 735 | 组织：Interest（AOI + 关系源）、Group、Direct | entitysync, entity, spatial | **sync 块 · 组织** |
| `entity/subject_sync.go` | — | 内容：SubjectSyncState、PrepareTick、Packer | — | 内容层，**留在 entity**（内容属于实体，不属于 sync） |
| `statesync` | 2896 | 帧编解码（codec / 帧类型 / Limits / control / 分片）**加** ARCH-10 之后已无调用者的老 Replicator 一族 | — | **sync 块 · 帧格式**；约 1900 行是死码（§2.2） |
| `nettransport` | 2373 | UDP / KCP / QUIC 传输、AsyncTransport 双 lane、AEAD | statesync（只为会话与传输契约） | **sync 块 · 传输** |
| `lockstep` | 1321 | 帧同步（输入帧）：Sequencer、冗余广播、追帧、desync | nettransport, statesync（SessionID） | **sync 块 · 帧同步**，与状态同步并列 |
| `syncbus` | 231 | 服务↔服务 ISyncBus 契约、DeliveryIDs、PatchSyncer | — | **sync 块 · 服务↔服务 · 契约** |
| `syncbus/driver` | 452 | NATS / JetStream 实现 | syncbus, nats, fctx | **sync 块 · 服务↔服务 · 实现** |
| `mirror` | 193 | 在 ISyncBus 上的副本复制器（Envelope upsert / delete） | syncbus, fctx | **sync 块 · 服务↔服务 · 消费模式**（只有 193 行，独占一个顶层包） |
| `kit/syncbus` | 178 | SyncBusMod 装配 | syncbus, syncbus/driver | kit 胶水，跟随 core 路径命名 |
| `spatial` | 1678 | 二维网格：BlockIndex、Terrain、寻路、**InterestManager** | — | **通用基建**（游戏世界几何）。只有 `interest*.go` 被 policy 用；demo 模板另有 15 处用它做地形 / 寻路 |
| `syncstream` | 2160 | 有序持久状态流：序号、回放历史、journal、分片 | syncbus | **通用基建**（领域无关的有序流）。core 内唯一消费者是 `skill/skillsync`；不在实体同步链上，也不是总线本身 |
| `cache` | 2446 | 本地 / Redis / 分层 store；`cache/mirror.go` 用 mirror 做副本同步 | mirror, syncbus, redis, metrics | 通用基建；`mirror.go` 是 syncbus 的消费者 |
| `remoteentity` | 4765 | 跨服实体写链路：ownership / fence / 事务 / Mongo 提交 **加** 快照与 interest 的 mirror 发布 | cache, entity, mirror, mongo, redis, syncbus, fctx, metrics | **消费者**：一半是 dataengine 的事（提交、锁、WAL），一半通过 mirror 走总线。归 dataengine 块 review 时处理，本方案不动 |
| `kit/remoteentity` | 268 | RemoteEntityMod | remoteentity, syncbus | kit 胶水 |
| `ownerroute` | 121 | 按 owner sid 路由命令，走 `bus` | bus | 不是 sync（是 RPC 路由） |
| `bus` / `event` | 2150 / 209 | NATS RPC 与可靠消费 / 进程内事件总线 | — | 通用基建（消息），不是 sync |
| `gateway` | 171 | 请求边界契约 | security | 不是 sync |
| `robot` | — | 客户端侧：dialers 用 nettransport，`robot/lockstep.go` 解 lockstep 帧 | nettransport, lockstep | 消费者（模拟客户端） |

**结论：sync 块 = 两条轴、七个包。**

- 服务 → 客户端（实体复制）：`entitysync`、`entitysync/policy`、`statesync`（帧）、`nettransport`（传输）、`lockstep`（帧同步）
- 服务 ↔ 服务（总线）：`syncbus`、`syncbus/driver`、`mirror`

`syncstream` 名字里有 sync，但它是给 skill 用的持久有序流，不是这两条轴的一环；本方案把它划为基建，只修它过时的包注释（"adapts … to roost-kit sync transports"）。

## 2. 结构问题（review 包结构与代码构造的结果）

### 2.1 横切分包：一条链拆成四个顶层包，按技术类别命名

`entitysync` / `statesync` / `nettransport` / `spatial` 四个顶层目录承载一条链。`statesync` 这个名字今天最误导：它曾是"状态同步"本体，ARCH-10 之后本体是 `entitysync.Manager`，`statesync` 只剩帧格式。三个连续迭代（statesync 09-02 → nettransport 09-08 → entitysync 重写 09-20）各留一个顶层包，就是维护者说的"多版本迭代把结构做散了"。

### 2.2 死码：`statesync` 约 1900 行没有调用者

core 内对 `statesync` 的全部引用只来自 `entitysync`、`nettransport`、`lockstep` 与 demo 模板，用到的符号只有两类：

- 帧：`EncodeFrame / DecodeFrame`、`DeltaFrame / ObjectDelta / ComponentDelta / ComponentSet / ObjectRef / SnapshotMeta`、`Frame{Full,Delta}`、`Object{Create,Update,Remove}`、`Limits / DefaultLimits`、`ErrObjectLimit / ErrComponentTooLarge`
- 传输契约：`SessionID / SessionInfo`、`Transport / SessionTransport / DatagramBatchTransport`、`DatagramHeader / InspectDatagram / DefaultMaxDatagram`、`IsControlPayload / ErrInvalidControl`

以下文件的导出符号在 `statesync` 之外**零引用**（含 kit、codegen、skill、robot、demo 模板）：`replicator.go`（617）、`session.go` 的 `SessionState`（342）、`lod.go`（285）、`delta.go`（236）、`shadow.go`（126）、`ring.go`（102）、`schema.go`（99）、`snapshot.go`（80），以及 `frame_limits.go` 里的 `ReplicationPolicy / Priority / Reliability / Lane / Visibility / ComponentState / ObjectState / Snapshot`（只被上述死文件和 `transport.go` 的 Projector 用）。这是 ARCH-10 替换掉的老 Replicator 一族，连同约 1800 行测试一起该删。`nettransport.ControlPlane`（56 行）同样零外部引用。

### 2.3 契约倒置：传输层依赖帧编解码包

`nettransport` 的 7 个源文件全部 import `statesync`，拿的是 `SessionID`、`Transport`、`DatagramBatchTransport`、分片头——这些是**传输**的契约，却放在帧格式包里。结果是"编解码 → 传输"的依赖箭头反了：想单独用传输就得带上帧格式。`lockstep` 只为 `SessionID` 也拖进整个 `statesync`。

### 2.4 过时命名与注释（迭代残留）

- `nettransport` 包注释："Package replication contains … for roost-core/replication"；`nettransport/sender.go`、`lockstep/room.go` 仍说 "replication package"
- codegen `render.go`：生成传输胶水的函数叫 `renderReplication`，import 别名 `kitnet`（kit 早已没有 nettransport）；`render_deploy.go:659` 生成的文档写着 "replication 生成 snapshot/delta/LOD"
- `syncstream` 包注释提到 roost-kit sync transports

### 2.5 内容层反向依赖基建

`entity/remote_snapshot.go` import `cache`。内容层（entity）不该知道缓存实现；这是 remoteentity 那半的事，记入 dataengine 块 review 的待办，本方案不动。

### 2.6 代码构造观察（不改，记下）

- `remoteentity` 单包 4765 行 17 个文件，两种职责（提交链路 / 快照发布）同居，是 dataengine 块 review 的首要对象
- `syncstream` 2160 行只服务一个消费者，是否下沉到 `skill/` 由 skill 块 review 决定
- `entitysync` 6 文件 1525 行、`policy` 4 文件 735 行，粒度合适；`nettransport/channel.go` 706 行承载 AsyncTransport 双 lane，可读但偏大

## 3. 目标布局：一个 `sync/` 根，两条轴在里面

```
sync/                      ← 目录，无 Go 文件（避免与标准库 sync 同名的包），只放 README
  entitysync/              ← entitysync（机制）
  entitysync/policy/       ← entitysync/policy（组织）
  frame/                   ← statesync 的活部分：codec、帧类型、Limits、control
  transport/               ← nettransport + 从 statesync 迁来的会话 / 传输契约与分片
  lockstep/                ← lockstep
  syncbus/                 ← syncbus（契约）
  syncbus/driver/          ← syncbus/driver
  syncbus/mirror/          ← mirror
```

依赖箭头整理后只有一个方向：

```
policy → entitysync → frame
                    → transport        lockstep → transport
syncbus/mirror → syncbus ← syncbus/driver
entity（内容）← entitysync            spatial（基建）← policy
```

`frame` 与 `transport` 互不依赖：帧格式不知道传输，传输也不知道帧里有什么（分片头是传输的事，帧内容是字节）。`entitysync.AsyncTransport` 适配器留在 `entitysync`，它是机制对传输的适配。

不进 `sync/` 的：`spatial`（基建）、`syncstream`（基建）、`cache`（基建）、`remoteentity`（dataengine 块的消费者）、`entity/subject_sync.go`（内容层归 entity）。kit 胶水 `kit/syncbus` 名字不变。

## 4. 分步实施（每步一个 M，每步独立可合）

### S1 · 删死码 + 修注释（无路径变化）

删 §2.2 列出的文件与符号及其测试，`statesync/frame_limits.go` 瘦身为帧类型 + Limits，`transport.go` 只留 `Transport / SessionTransport / DatagramBatchTransport`（Projector 一族随 Replicator 走）。删 `nettransport.ControlPlane`。修 §2.4 的注释与 codegen 生成文本。验收：`go build ./... && go vet ./...`；`statesync` 剩余导出符号全部有外部引用（脚本核对）；`docs/review/ENTITYSYNC-MAP` 的 §2.3 标记已清。预计 −3700 行。

### S2 · 契约归位（一次路径内移动）

`SessionID / SessionInfo`、`Transport / SessionTransport / DatagramBatchTransport`、`datagram.go` 的分片 / 重组、`control.go` 的 `IsControlPayload`、`DefaultMaxDatagram` 从 `statesync` 移到 `nettransport`。之后 `nettransport` 不再 import `statesync`；`lockstep` 只 import `nettransport`；`entitysync` 从 `nettransport` 取 `SessionID`。验收：`go list -f '{{.Imports}}' ./nettransport` 不含 statesync；`entitysync/session.go` 的 frame 编码字节与 S1 前逐字节相同（现有 promise test 的 wire 断言覆盖）。

### S3 · 搬进 `sync/`（一次 git mv，一次 import 重写）

`git mv` 八个目录；`gofmt -r` / sed 重写 core 内 import；demo 模板 `roost-core/{entitysync,statesync,nettransport,lockstep}` → `roost-core/sync/{entitysync,frame,transport,lockstep}`（模板里 statesync 只用 `DefaultLimits`、`SessionID`、`ObjectRemove`，前者去 frame，中者去 transport）；`codegen/internal/roost/render.go` 的 `renderReplication` → `renderSyncTransport`，别名 `kitnet` → `synctransport`；`migration/consolidation_imports.yaml` 加第三阶段路径表，让 `roost upgrade` 重写业务工程的 import（`statesync` 映射到 `sync/frame`，个别 `SessionID` 由编译器指出，沿用 T-45 的做法）。roost.yaml 的 feature 名 `nettransport-*` 是用户面的词，**不改**。验收：core 全量 `go test ./...`；用本地 codegen 渲染 demo 工程并跑其测试与 e2e（16 机器人首帧带 `pos_x`，dup=0，沿用 M-14 的验收）。

### S4 · 文档收口

`README.md` 包表把 `entitysync、syncbus、syncstream、statesync` 那一行拆成"sync 块（两条轴）"一行与 `syncstream` 基建一行；`ENTITY_SYNC.md` 四层表路径更新；`kit/README.md`；`docs/review/ENTITYSYNC-MAP` 目录节重写为 `sync/` 布局；CHANGELOG 记"破坏性：路径"。

### 顺序与 ARCH-11 的关系

S1 → S2 → S3 → S4，然后再做 ARCH-11 的 M-15（多 profile 优先级）与 M-16（tick 级编码缓存）：两者都改 `entitysync/manager.go` 与 `session.go`，放在搬家之后只动一次。

## 5. 同一原则对另外两块的预告（不在本方案范围）

- nest 调度块今天散在 `nest`（6115）、`nestwal`（3128）、`worker`（389）、`lock`（263）、`kit/nest`；`nestwal` 与 `worker` 是 nest 的两个部件，同样适合收进 `nest/` 根
- dataengine 块：`dataengine`（939）、`dataengine/engine`（3009）、`saga`（3322）、`remoteentity`（4765）、`versionstore`（795）、`kit/dataengine`、`kit/saga`；`remoteentity` 的两半职责与 `entity → cache` 反向依赖是它的首要结构问题

两块各出一份同样格式的盘点表后再定。

## 6. 实施记录（2026-09-23，`feature/arch-10-sync-policy`）

| 步 | 单元 | 结果 |
| --- | --- | --- |
| S1 | [M-15](M-15-statesync-dead-code.md) | 删 statesync 老 Replicator 一族与 `nettransport.ControlPlane`，约 −3.9k 行 |
| S2 | [M-16](M-16-transport-contracts-home.md) | 会话 / 传输契约与分片头归位 nettransport；`FragmentDatagrams` 与帧解耦 |
| S3 + S4 | [M-17](M-17-sync-layout.md) | 八个目录收进 `sync/`；`statesync` → `sync/frame`；InterestManager 从 `spatial` 搬进 `policy`；文档收口 |
| 追加 | [M-18](M-18-async-transport-reliable-only.md) | W-2026-09-23-01 拍板：`AsyncTransport` 删掉 datagram lane，只留 reliable；分片头与批准入契约随行退场 |

与 §3 的两处偏差：

- **传输包名保留 `nettransport`**（`sync/nettransport`），没有叫 `transport`。两个原因：`entitysync.Transport` 是机制对下游的契约，"网络传输"与它是两个概念，`nettransport` 这个名字把区别说出来了；其次 `transport` 作为局部变量在 entitysync、lockstep、demo 模板里出现几十处，同名包会被遮蔽。
- **`sync/frame` 的 API 去掉了重复的 Frame 前缀**：`EncodeFrame / DecodeFrame / DeltaFrame / FrameKind / FrameFull / FrameDelta` → `frame.Encode / Decode / Frame / Kind / Full / Delta`。线格式一个字节没变。

维护者追加的一条（S3 一并做）：`spatial` 只留纯空间原语，`InterestManager / InterestCluster`（AOI 观察者索引）搬进 `sync/entitysync/policy`，因为"谁在谁的视野里"是同步的组织问题，不是几何问题。
