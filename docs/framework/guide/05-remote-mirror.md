# Remote Entity 与 Mirror：设计与使用

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/05-remote-mirror.md) · [模块总目录](../README.md)

## 先理解这一块

一个公会可能被多个游戏进程使用。Remote让共享实体的写入有明确负责人，Mirror让其他进程读取它的快照。

读到一份缓存不代表拿到了写权限。先分清权威数据、快照和拥有者，再看一致性选项。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 边界与角色

Remote Entity 是具有权威写入与版本的共享实体。写仍通过 Nest 事务与正式 DataEngine 持久链路；只读方通过 SnapshotClient/Mirror 获取快照，不因缓存存在就获得写许可。ownerroute 解决路由/拥有者定位，发现信息不等于持久 fence。

把实体声明成 Remote 托管要同时改三处：标记 `category=entity.EntityCategoryRemote remote=managed`、嵌入 `*entity.RemoteEntityBase`（生成器漏换时报错并点名要换的字段），以及写它的 Nest 服务装 `remote_entity` Mod（`wiring/remoteentity`）。没有 Remote 运行时的进程不会把这类写当本地写：Nest 派发与 DataEngine 删除一律返回 `entity.ErrNoRemoteRuntime`（包裹 `ErrRemoteWriteCapabilityDisabled`，文本给出加 Mod 的命令）。生成工程里 `roost generate` 与 doctor 的 `remote-runtime` 项在带 Nest 的服务全都没有 `remote_entity` 时拒绝并给出 `roost add mod remote_entity -service <service>`；只有部分服务缺时提示（实体由哪个服务写，静态上看不出）。

实体归某个服务写时（例如联盟服务独占全部 Alliance），别的服务的进程用 `wiring/remoteentity.WithForeignKinds(kinds...)` 声明这些 kind：写准入与 `ClaimRemoteOwnership` 等所有权操作在读写任何所有权记录之前返回 `entity.ErrRemoteKindNotOwned`，不会把新实体认领走；读快照不受影响，没声明的 kind 行为不变。见 [Remote Entity 契约“实体归属守卫”](../REMOTE_ENTITY.md#实体归属守卫)。

实体服务有多个实例时，归属服务的 `RemoteEntityMod` 用 `WithRouteKinds(kinds...)` 声明登记路由的 kind：owner 加载这些 kind 的独占实体后在 Redis 登记带 TTL 的“实体 ID → sid + 所有权代际”，批量续期，转移出去、失去所有权与正常停机时按值删除，kill -9 靠过期；调用方的路由 RPC 经 `RouteSource` 查 owner（Redis → 持久所有权记录），服务端前置检查 `RPCRouteCheck` 不让不存在的实体进入写准入。登记只是路由提示，写准入仍只看所有权记录与 fence。`WithPreloadKinds` 加 `NewRemotePreloadMod()` 是起服预加载：只加载所有权记录里 owner 是本 sid 的实体，完成前路由 RPC 不接请求。见 [Remote Entity 契约“路由登记”](../REMOTE_ENTITY.md#路由登记) 与 [guide/09“路由 RPC 与改投”](09-services.md#2a-路由-rpc-与改投)。

## 2. 写入生命周期

准备参与实体和写许可 → 业务池 handler 修改 DAO → 事务局部变更形成 Remote 批次 → 持久确认 → finalizer 收尾/释放。参与者必须判断本事务变化，不能用对象全局 dirty 推断。取消等待不撤销已经准入的持久写；结果未知保留 fence/恢复责任。

独占实体（所有权记录是单个 owner sid）不取写许可：一个实体同一时刻只有一个 owner（App 单实例与 fail-stop 保证），owner 只在加载、取得归属、从共享退回独占之后读一次权威对齐版本，稳定持有期间读写请求都不向 Mongo 确认归属；handler 不改持久字段的请求不产生任何 Mongo 命令。前提被打破时后到的写在投影时因版本 / 代际不符被拒。只有共享模式每次写取分布式锁与写许可。见 [Remote Entity 契约“独占写准入”](../REMOTE_ENTITY.md#独占写准入)。

持久拒绝（Rejected）只来自 memory（Durability 0）：直写权威被明确拒绝，或结果未知后由 finalizer 以同一事务 `_id` 写下持久拒绝。拒绝后实例回滚、隔离、释放写许可并仅内存卸载，下一次访问从权威重载，提交后工作得到 false。async / strict / pipelined 有 WAL，投影器写权威冲突是 fatal（fail-stop），不产生拒绝，所以它们的失败只会是“结果未知”，调用方不得把它当成 Remote 没写入。详见 [Remote Entity 契约](../REMOTE_ENTITY.md#持久拒绝rejected的来源与收尾)。

多个进程写同一实体（共享模式，或所有权转走又转回）时，每个进程内存里的实例只随本进程的写推进。写准入取得写许可（共享）或在对齐时刻读到权威版本（独占）后发现本地实例落后于权威、或停在 `Fenced`，先把它仅内存卸载、再从权威重载，然后在最新版本上执行 handler；卸载没完成时返回可重试的 `entity.ErrRemoteEntityReloading`。outbox 代发别的进程的提交时不推进本进程实例。需要 loader 支持卸载（正式 `ManagerAccess` 支持），细节与边界见 [Remote Entity 契约](../REMOTE_ENTITY.md#多写者本地副本与权威)。

删档（`Destroy(deletePersisted=true)`）物理删除 Remote 数据文档、在 Remote 元数据留墓碑（`_remote_entity_meta._deleted`）；之后该 ID 不能经 GetOrCreate 重建：DataEngine 冷加载据元数据墓碑返回删过档（`entity.ErrAuthorityEntityDeleted`）。直接 `Create` 不读存储，必须用 id-gen 新 ID（D10，见 [guide/03 §4](03-dataengine.md#4-冷加载与-schema)）。

Mongo 的持久条件和 incarnation 区分拥有者，旧进程、旧回调或重试不得在新拥有者下继续生效。批次成功、数据投影、快照发布是不同状态，不得以一次发送成功提前释放写额度或宣告提交。

## 3. 单一 outbox 发布者

自 v1.24.0 起只保留一个 outbox 发布协调者，投影与 finalizer 处理持久结论及收尾，不再各自竞争发布同一快照。协调者内部按事务涉及的全部 Entity 建立依赖，有界并行发布独立事务，同实体仍保序。失败页不能永久阻挡后续扫描进度。

发布确认失败重试保持稳定身份；进程崩溃后从持久 outbox 恢复。broker 接受不代表全部只读节点都应用，消费者仍需版本/身份去重及回源。

## 4. L1、L2 与数据权威

Mongo 是数据权威，L1 是进程缓存，Redis L2 是共享快照缓存及快照水位权威。L2 水位不能被本地旧 L1 反向覆盖；同版本冲突值也不能当作等价重复。缓存读取的一致性取决于所选策略与陈旧上限，不能把 Cached 描述成 Linearizable。

推送丢失、L1/L2 缺失、重启、撤销兴趣均要依靠版本/代际与权威回源恢复。缓存 TTL 只表达寿命，不证明内容已经追到最新提交。正常读、强一致读、只读副本使用场景应分别配置。

**没有读方的 kind 可以不发布快照**（维护者 2026-11-05）：`//roost:entity remote=managed snapshots=none` 的提交只写 DAO 文档——不写 `_remote_entity_snapshots`、不进 L2、不广播。快照只被 Mirror 读与 RemoteViewRef 解析读；owner 冷加载、接管（同 sid 拉起、所有权过期）、`TransferRemoteOwnership` 迁移与缩容枚举都读 DAO 集合与 `_remote_entity_meta`，不受影响。指向这种 kind 的 `//roost:mirror` 在生成期被拒，运行期读它报 `entity.ErrRemoteSnapshotsDisabled`。game-demo 的联盟守卫 `AllianceMember` 就是这样：game 进程只镜像联盟，玩家所在公会记在 Player 自己身上。

## 5. 兴趣协议

发布与接收要求非零 Generation、完整 Key、ConsumerSID；无 payload Delete 拒绝。兴趣撤销水位保护乱序/旧代消息不能复活订阅。两端必须一起升级，不支持旧发布端回退。Mirror 只复制声明的数据视图，隐私由业务投影与 profile 控制。

## 6. 普通 Entity 写入 + Remote View 读取

适用于玩家 handler 读取公会/联盟加成、展示资料等允许快照语义的业务。普通 Player 仍是唯一可写目标，Remote 引用是请求参数，不作为第二个锁目标；读取不取得 Remote 写许可。当前 main 新增独立请求生成，使用时工具与 core 检出保持一致，旧工具需升级并重新生成。

### 从 DAO 字段生成 Snapshot

在 Mongo DAO 定义的顶层字段写 `remoteview`，不需要再定义 Snapshot，也不需要额外的视图标记：

```go
// db/def/alliance.go
//roost:dao coll=alliances db=game dbscope=global
type AllianceDao struct {
    Name          string `remoteview:"public,member" store:"name"`
    Level         int32  `remoteview:"public,member" store:"level"`
    ResearchBonus int64  `remoteview:"member" store:"research_bonus"`
    Treasury      int64  `store:"treasury"`
}
```

`roost generate` 在 DAO 输出包（通常为 `db`）生成 `gen_remote_views.go`，不另建视图包：

- `db.AlliancePublicSnapshot`：Name、Level；`db.AllianceMemberSnapshot`：Name、Level、ResearchBonus。DAO 类型名的 `Dao` / `DAO` 后缀被去掉。未标记的 Treasury 不进入这两个投影。
- 两种 Snapshot 的 `RemoteScope()` / `RemoteSchema(kind)` 和 `DecodeAlliancePublicSnapshot` / `DecodeAllianceMemberSnapshot`。
- DAO 的 `PublicSnapshot()` / `MemberSnapshot()`：仅在拥有者 Entity 锁内使用，复制容器及嵌套数据，不带 DirtyHook、setter 或权威 DAO 引用。
- `RemoteDAOViewProvider`：按本次事务字段 mask 冻结投影，Remote Entity 注册、提交和删除代码自动接入。`RegisterEntity()` 注册解码器；只读进程如不注册权威 Entity，可在启动时对每个 kind 调用一次 `entity.RegisterRemoteDAOViews(db.NewAllianceDao(), allianceKind)`，这一步只登记解码器，不注册写能力。

声明范围是单 DAO 的多个视图，不支持跨 DAO 聚合和计算字段。视图名用小写 snake_case。只允许持久字段；`nocoll`、Redis DAO、`nopersist`、`dao:"-"`、未导出/匿名/同一行多名字段不允许声明 `remoteview`。嵌套字段被选中时整体包含其中的持久字段，嵌套内部不能再标 `remoteview`；不适合暴露整个嵌套结构时拆分顶层字段。

支持基本类型、切片、整数/字符串键 map，以及已声明的嵌套结构体、指针和这些类型的容器，沿用 DAO 自身支持的形状。map 中的切片、嵌套指针和 nil 都递归复制；未知外部类型、递归嵌套、非法视图名和生成类型名冲突明确报错，不静默漏字段。Snapshot 在 Go 中是普通值类型，使用者仍应按只读约定处理，修改它不写回权威数据。

字段投影与现有默认 DAO 快照分别使用 scope。新 scope 按 `remoteview:<collection>:<view>` 派生，schema 绑定 kind 及递归字段形状；载荷为完整 BSON（Remote codec 1），不改变客户端 EntitySync 的 PB。相关字段命中事务 mask 才生成该视图，无关字段变化不发布它；所有载荷冻结进同一 Remote commit，由既有持久确认/outbox 发布，不在 setter 中直接发送，失败回滚不发布。实体删除包含全部当前投影的失效键。

`RemoteViewRef.Version` 应使用该视图已知的最低版本，首读可为 0，不能随意填实体的最新全局版本：无关字段提交不会推进这个视图的水位。字段形状改变后旧 schema 不被当前解码器接受；本框架不迁移旧投影，发布时需按现有不兼容升级规则处理权威快照、缓存与重建，不能只删 L1 期待旧 Mongo 投影自动变成新 schema。删除视图声明会清理生成文件，不会自动删除存储中的历史快照；视图名称也不提供自动权限或历史撤权。

### 声明与生成

将请求类型放在 `game/requests` 等业务共用包，不能放在会调用 Sender 的 handler 包。`roost generate` 扫描 `game` 时，即使请求文件没有 handler 标记，也会在类型所在包生成 `<source>_remote_nest_gen.go`，提供 `RemoteAccess()`、`Alliance()` 和 `MustAlliance()`。多个 handler 共用请求只生成一份方法，Sender / SyncSender 直接引用同一 DTO，不复制数据结构、不反向导入 handler。

```go
// game/requests/alliance.go
package requests

import (
    "github.com/tjbdwanghaibo/roost-core/framework/entity"
    "example.com/mygame/db"
)

// Go 不会把 tag 中的类型当成 import 使用；保留这行类型引用。
var _ db.AllianceMemberSnapshot

type AllianceRequest struct {
    AllianceViewRef entity.RemoteViewRef `remote:"db.AllianceMemberSnapshot,required"`
}
```

`db.AllianceMemberSnapshot` 及其解码器由上面的 DAO 定义生成，Remote Entity 注册时自动登记。手工 DTO 仍需自行接入解码或 resolver，不能只声明同名 struct 期待默认 DAO 解码结果自动转换。请求源文件须导入 tag 引用的类型包；默认访问器由字段名去掉 `ViewRef` / `Ref` 后生成，也可用 `accessor=` 显式指定。租户、策略、一致性、最低版本与 TTL 声明沿用 Remote tag；表达式引用请求字段时接收者统一为 `req`。请求支持值和非 nil 指针，投递后不要修改请求内容。

```go
// game/handler/alliance.go；player、requests 为业务工程包。
//roost:nest rollback=undo durability=strict
func handlerApplyAllianceBonus(p player.IPlayerEntity, req requests.AllianceRequest) error {
    // 只访问已准备好的 DTO；此处不做缓存回源、RPC 或数据库读取。
    alliance := req.MustAlliance()
    // 先核对 p 的当前联盟关系，再按业务规则修改 Player DAO。
    return p.ApplyAllianceBonus(alliance)
}
```

上述 handler 是接入形状示意，`IPlayerEntity` 与 `ApplyAllianceBonus` 由业务定义。入口构造可信 `RemoteViewRef`，通过生成的异步 `Send_ApplyAllianceBonus` 或同步 `Sync_ApplyAllianceBonus` 投递；业务 handler 内不能同步调用 SyncSender（`ErrSyncInHandler`），另发命令使用异步 Sender。不要把客户端传来的联盟 ID、Tenant、Policy 直接当作授权。

### 调度与失败边界

1. Nest 识别参数的 `RemoteAccessProvider`，在 I/O 池读取并校验快照；默认经 Remote Manager，也可配置 `NestOptionWithRemoteSnapshotResolver` 接入只读快照来源。需要提前装配快照读取及对应类型的解码能力。只读进程（装 `wiring/remoteentity.NewRemoteMirrorMod`、不装 `remote_entity` 写能力）由 wiring 的 Nest Mod 自动接上：没有写 Manager 而注册表里有 `mods.ModRemoteMirror` 时，用 `nest.MirrorSnapshotResolver(MirrorSource)` 作快照来源（2026-11-04），读取规则与经 Manager 相同——`MinVersion` 即 After 的状态版本，Monotonic 在缓存不满足时回源一次，Cached 冷读在没有 L2 值时是未命中；Linearizable 只在只读 loader 声明了线性化读时可用。业务另给的 resolver 选项仍然优先。回归：`wiring/nest` 的 `TestReadOnlyProcessPrefetchesRemoteViewThroughMirror`。
2. I/O 完成后回到业务池，取得普通 Entity 的 Guard 执行 handler。访问器读取当前请求上下文，不再做 I/O；只在当前 handler 生命周期内使用，不保存到异步任务中。
3. `required` 的读取失败、身份不匹配或版本过旧使请求失败，handler 不执行。可选读取失败时访问器返回 `false`，业务明确降级；无效引用/重复别名属于声明错误，不靠可选模式忽略。多个请求参数的别名不能重复。
4. View 不自动写回 Remote，也不会使玩家和公会构成原子事务。读取策略只约束预取时刻，不保证排队后执行时公会仍未变化；玩家中途退会/换会须在锁内核对引用。扣联盟资源、审批成员、撤权等权威判断走 Remote 写入或拥有者命令，跨权威事务按 Saga 契约处理。
5. 框架不会锁住 Player 后自动推导 `Player.AllianceID` 再回源。引用需由可信入口/前置业务阶段提供；禁止为了自动推导而在持锁 handler 内读取 Mirror。

验证入口：`GOWORK=off go test ./codegen/internal/nest -run TestRemoteRequestGeneratedConsumer -count=1`。临时消费工程实际生成并以 race 执行异步/同步 Sender、跨 handler 共用 DTO、指针参数、必需失败/可选降级；阻塞一个 I/O worker 时，单业务 worker 仍能处理无关本地 ID。快照来源使用 resolver 替身，该检查不等于真实 Mongo/Redis/NATS 验收或性能结论。 2026-11-01 macOS 实跑：生成消费工程（race、count=2）、生成器与 Nest 包 race、SLG / MMORPG 缺省生成脚本均通过；两类玩法脚本包含现有 Robot TCP 业务验收，但不包含真实存储模式。

字段投影联合验证入口：`GOWORK=off go test ./codegen/internal/dao -run TestRemoteView -count=1`。消费工程运行正式 DAO → Entity → 请求/Sender 生成，验证多视图字段白名单、容器/嵌套隔离、冻结后修改、事务回滚、按字段更新、实体删除失效以及 Nest I/O 预取读取。事务提交器与快照来源使用替身，未以该用例声称真实 Mongo/Redis/NATS 或跨机验收。 2026-11-01 macOS 实跑通过：字段升级旧 schema 拒绝、DAO/Entity/Nest 生成消费工程（race、count=2）、六个受影响包 race、全仓 build、定向 vet、根包检查以及 SLG/MMORPG 缺省生成脚本。

## 7. 运维与性能范围

观察持久记录、outbox pending、发布错误、写额度、旧回调拒绝、缓存命中/回源，分别判断。稳定版 80 TPS 的一小时测量是两实体×两 DAO、1000会话/10000实体、单机真实资源场景，不是普通 Nest 消息吞吐上限，也不是物理跨机容量承诺。完整参数和失败样本见维护性能报告。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
