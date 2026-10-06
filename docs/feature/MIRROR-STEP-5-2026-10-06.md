# RemotePolicy Mirror 第 5 步：kit 只读装配、codegen 只读产物、公会摘要样例（2026-10-06）

依据：维护者授权实施 Mirror 第 5 步（方案六步表第 5 行）。
前置：[第 1～3 步](MIRROR-STEPS-1-3-2026-10-06.md)、[第 4 步与 O4](MIRROR-STEP-4-AND-O4-2026-10-06.md)（§6.8 是本步入口）、发版前审查的 `207163f9`（allow_stale 的 Cached 访问交给 `Accepts` 判定，本步不碰）。
范围：`kit/remoteentity`、`remoteentity`（只加只读 Mongo loader）、`app/config_validation.go`（登记新键）、`kit/mods`（能力名）、`codegen/internal/entity`、生成工程样例 `codegen/internal/entity/testdata/remoteflow`、文档。Remote 权限（K3）、entity 读出口不动。

## 1. 目标

维护者原则：使用方手写的代码尽量少。只读消费者接入 = **声明一个 DTO + 一行装配**：

```go
//roost:mirror entityKind=guild.EntityKindGuild coll=guild
type GuildSummary struct {
	Name      string `bson:"name"`
	FounderID int64  `bson:"founder_id"`
}
```

`roost generate`（或 `go run ./codegen/cmd/entity`）生成 `GuildSummaryMirrorSpec`、`DecodeGuildSummary`、`NewGuildSummaryReader(source)`；服务里装一个 `kitremote.NewRemoteMirrorMod(sid)`，读取时 `NewGuildSummaryReader(kitremote.MirrorSource(registry))`。

## 2. 设计

| 部分 | 做法 |
| --- | --- |
| 只读 Mongo loader | `remoteentity.NewMongoSnapshotLoader(mongo, database)`：只做 `_remote_entity_snapshots` 的 `FindOne`（与 `MongoCommitter.LoadRemoteSnapshot` 共用同一个函数），不建索引、不写、不需要事务 / 所有权集合。不声明线性化（Linearizable 读返回 `ErrRemoteReadUnsupported`）。 |
| kit `RemoteMirrorMod` | `kit/remoteentity` 同包。Init：与 `RemoteEntityMod` 共用一个快照段读取函数（A4 严格读取），外加新键 `remote_entity.mirror.shutdown_timeout`（停机预算，缺省 5s，登记进 `frameworkDurationKeys`）与 `remote_entity.mongo.database`。Provide：`NewSnapshotClient(cfg, {L2: NewSnapshotL2StoreWithKeyPrefix(redis, …), Loader, LinearizableLoader, ConsumerSID: sid})`，能力 `mods.ModRemoteMirror`（类型 `entity.RemoteSnapshotReadOnly`）与健康项 `remote_mirror`（`snapshot_push`、`interest_refused`）。Start：总线取自 `kit/syncbus`，JetStream 才有推送。Stop：`StopWithContext` = 客户端三步停机；`StopBudget()` 声明停机预算。缺省 loader 是只读 Mongo loader（依赖 `mongo` Mod）；`WithMirrorLoader(loader, linearizable)` 换成别的权威（同进程 Managed 用 `Backend.LoadRemoteSnapshot`）。不要求 Mongo 原子 backend、锁或 finalizer。 |
| 同进程 owner | `RemoteEntityMod` 也把 `Manager.SnapshotClient()` 登记为同一个能力 `mods.ModRemoteMirror`：使用方读取代码在 owner 进程与只读服务里一样；两个 Mod 装在同一进程时能力冲突，启动即失败（同一身份不开第二个客户端）。 |
| codegen | `//roost:entity remote=mirror`（及 `lifetime=mirror_cache`）报迁移诊断，指向 `//roost:mirror`。新标记 `//roost:mirror entityKind=<owner kind> coll=<owner DAO 集合>` 放在普通 struct 上，生成 `<dto>_gen_wire.go`：spec（`Kind` / `Scope=RemoteSnapshotScope(coll)` / `Schema=RemoteSnapshotSchema(kind, scope)` / `Codec=1`，与 Managed 生成的提交快照逐字同一规则）、BSON 解码函数、reader 构造函数。不生成 kind / builder / 解码器注册、DAO、提交参与能力；DTO 带 `dao:` / `comp:` 标签或嵌入字段直接拒绝。 |
| 公会摘要样例 | 放在生成工程 `testdata/remoteflow`（`scripts/test-remote-generated.sh` 用正式 DAO / Entity 生成器生成、以真实消费者编译运行）：owner = 测试进程里的 Managed `Guild`（Nest → WAL → Remote 提交 → 发布），只读方 = 子进程（同一测试二进制再执行），只装 kit `SyncBusMod` + `RemoteMirrorMod`，经生成的 `NewGuildSummaryReader` 读。JetStream 一次（推送）、普通 NATS 一次（按需）。 |

比 game-demo 模板简单：demo 要新增服务、清单、部署与配置，验收还要起整套服务；remoteflow 已有隔离库 / 前缀 / 清理与正式提交链路，两进程只差一个子进程。

## 3. 兼容

- 公开 API 只增：`remoteentity.NewMongoSnapshotLoader`、`kitremote.RemoteMirrorMod` 一组、`kitremote.MirrorSource`、`mods.ModRemoteMirror`；新配置键 `remote_entity.mirror.shutdown_timeout`。
- **生成器行为变化**：`remote=mirror` / `lifetime=mirror_cache` 从“生成一个名不副实的可写 Entity”改为报错（仓内与 demo 没有使用方）。core 的 `entity.RemotePolicyMirror` 常量保留（不删 API）。
- `RemoteEntityMod` 多登记一个能力名；不装 `RemoteMirrorMod` 的工程无变化。
- 生成形状：Managed / 普通 Entity 的生成物不变；新增的只有 `//roost:mirror` 的产物。生成产物用到 v1.20.2 没有的 `entity.RemoteMirrorReader` / `RemoteSnapshotReadOnly`（Mirror 第 1～3 步新增），生成工程的 core 下限需在发版准备时统一上调（`minimumVersions.Core`），本步不改。
- 手写 Mod 的 `StopBudget` 不计入生成器的 `shutdown.total_timeout`（RR-20260926-66）：装 `RemoteMirrorMod` 的服务需要手工给 total 与部署宽限期留出这段预算。

## 4. 验证计划

- kit：配置严格读取与新键；只读（能力不带写接口、注册表里没有写能力、两个 Mod 同进程冲突）；停机契约（`stopcontract.Check`）、停止取消在途读取、重复关闭 nil；健康信息。负对照：去掉停止取消（loader 不随 Stop 取消）→ 红；登记 Manager 而不是客户端 → 只读断言红。
- codegen：迁移诊断；mirror 产物（spec / 解码 / reader、无写能力）；目录发现与孤儿清理；`go test ./codegen/...`；`go generate ./...` 后 porcelain 干净。
- 样例：`scripts/test-remote-generated.sh`（`ROOST_REMOTE_RUN` 只跑本步用例）：真实 JetStream + Redis + Mongo 上 owner 提交、只读子进程经推送读到新版本；普通 NATS 上退化为按需；跨租户 / profile 读不到 owner 的数据、身份不符的结果被拒；只读子进程没有写能力；停止后读返回 `ErrSnapshotClientStopped`、重复关闭 nil。负对照：子进程注册 builder / 拿到写能力 → 红。
- 改动包 `go test -race -count=3`、`go vet`、根包、glsvet（未改 nest / entity / dataengine / sync 则不必）；game-demo 生成与 build / vet / test（生成器改动）。

## 5. 实施记录（基线 `c99b59f6`，分支 `mirror5`）

图谱 `Users-whb-roost-roost-core` 索引的是主 checkout、`codegen/internal/entity/testdata` 按设计不索引；本轮涉及的文件全部按当前源码读取。

### 5.1 实际实现

| 位置 | 变化 |
| --- | --- |
| `remoteentity/mongo_committer.go` | `NewMongoSnapshotLoader(mongo, database)`：与 `MongoCommitter.LoadRemoteSnapshot` 共用 `loadMongoRemoteSnapshot`（同一个 `FindOne`），只读、不建索引 |
| `kit/remoteentity/remote_mirror_mod.go`（新） | `RemoteMirrorMod`、`NewRemoteMirrorMod(sid, opts…)`、`WithMirrorLoader`、`MirrorSource(registry)`；Init / Provide / Start / Stop / StopWithContext / StopBudget / 健康 `remote_mirror` |
| `kit/remoteentity/remote_entity_mod.go` | 快照段读取提成 `readSnapshotConfig`（两个 Mod 共用，逐字搬移、行为不变）；Provide 多登记 `mods.ModRemoteMirror` = `Manager.SnapshotClient()` |
| `kit/mods/name.go` | `ModRemoteMirror = "remote_entity.mirror"` |
| `app/config_validation.go` | `remote_entity.mirror.shutdown_timeout` 进 `frameworkDurationKeys` |
| `codegen/internal/entity/mirror.go`（新）、`parse.go`、`gen.go`、`main.go` | `//roost:mirror` 解析与校验、生成模板；`parsePackage`（实体 + DTO，`parseDir` 保持旧签名）；`remote=mirror` / `lifetime=mirror_cache` 迁移诊断（解析与生成两处）；目录发现、`-output` 单文件模式、同名输出拒绝、孤儿清理、实体改 DTO 时删旧守卫测试 |
| `codegen/internal/entity/testdata/remote/` | 新 DTO fixture `guild_summary.go` 与生成物；`guild_gen_wire.go` 按当前生成器重生成（原 fixture 早于 RR-20260909-04 的注册拆分，生成器之前就已变化），补伴生守卫测试 `guild_gen_wire_test.go` |
| `codegen/internal/entity/testdata/remoteflow/` | 样例：`def/state.go` 加 `GuildDao`（`remote_guilds`，含摘要不读的 `Motto`）、`guild.go`（Managed，kind 238）、`guild_summary.go`（DTO）、`mirror_test.go`（两进程） |
| `demo/game/handler/guild_info.go.tmpl` | 说明改为指向 Mirror DTO 读法（注释，不用 `//roost:mirror` 字面，免得被目录扫描当成标记目录） |

### 5.2 先红后绿

这是新能力，没有修前红；用负对照证明用例抓得到问题（改完即恢复）：

```text
N1 去掉停止取消（gatedLoader 不再随 Stop 取消在途加载）
--- FAIL: TestRemoteMirrorModStopCancelsInFlightReads (2.00s)
    remote_mirror_mod_promises_test.go:273: Stop with a cancellable read in flight = context deadline exceeded; the in-flight load must be cancelled by the stop
N2 去掉只读限制（Mod 登记写 Manager 而不是只读客户端）
--- FAIL: TestRemoteMirrorModRegistersOnlyReadCapability
    the mirror capability *remoteentity.Manager is a remote entity manager
    the mirror capability is the write Manager
    Linearizable read through an undeclared loader = <nil>, want ErrRemoteReadUnsupported
N3 去掉 remote=mirror 迁移诊断（等于基线：旧形式被接受、生成可写 Entity）
--- FAIL: TestRemoteMirrorEntityMarkerIsAMigrationError
    parseDir accepted the source and produced 1 entities; want an error mentioning ["remote=mirror no longer generates an entity" "//roost:mirror entityKind="]
N4 生成物多出全局解码器注册
--- FAIL: TestMirrorDTOGeneratesReadOnlyView
    mirror view carries write / registration capability "MustRegisterRemoteSnapshotDecoder"
N5 样例：只读子进程调用 RegisterEntity（拿到 Guild 的 builder，能加载 / 写公会）
    mirror_test.go:493: the read-only process registered the Guild entity builder: it could load and write guilds
--- FAIL: TestGeneratedRemoteMirrorGuildSummary/jetstream
N6 样例：只读方在 JetStream 上不开推送
    mirror_test.go:336: read-only process push mode [push=false], want push=true
--- FAIL: TestGeneratedRemoteMirrorGuildSummary/jetstream
```

基线事实：`TestParseDirAcceptsEveryDocumentedMarkerForm` 原把 `remote=mirror lifetime=mirror-cache` 当作合法形式，`TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement` 原断言 `remote=mirror` 仍被接受；两处随新契约改为拒绝（语义变化，不是放宽）。

### 5.3 新用例（全部通过）

- `kit/remoteentity`：`TestRemoteMirrorModRegistersOnlyReadCapability`（注册表无写能力、能力不是 publisher / manager、换 loader 时不依赖 mongo、未声明线性化拒绝 Linearizable）、`TestRemoteMirrorModRefusesASecondClientBesideTheOwner`、`TestRemoteMirrorModConfiguration`（快照段、新键、错类型 / 非正 / 越界 / hash tag 前缀、sid、`ValidateServiceConfig`；Cluster 下无 hash tag 的 lock_key 不影响只读装配）、`TestRemoteMirrorModStopContract`（`stopcontract.Check` + `CallerReleases`）、`TestRemoteMirrorModStopCancelsInFlightReads`（停止取消在途读、之后读返回 `ErrSnapshotClientStopped`、重复关闭 nil、健康 Fail）、`TestRemoteMirrorModHealthReportsPushMode`。
- `codegen/internal/entity`：`TestRemoteMirrorEntityMarkerIsAMigrationError`、`TestMirrorDTOGeneratesReadOnlyView`（与 fixture 逐字一致、无写 / 注册能力、owner 提交仍用同一身份规则）、`TestMirrorDTOOnlyPackageIsDiscoveredAndRetired`、`TestMirrorMarkerValidation`。
- 生成工程：`TestGeneratedRemoteMirrorGuildSummary/{jetstream,nats}` + 子进程 `TestGeneratedRemoteMirrorReaderProcess`。

### 5.4 真实环境（隔离环境 `~/.roost-it/roost-dataengine-it`，`remote-acceptance.lock` 不存在）

`ROOST_REMOTE_RUN='^TestGeneratedRemoteMirror' scripts/test-remote-generated.sh`（正式 DAO / Entity 生成器生成，真实消费者编译）：

```text
jetstream: MIRROR READY push=true / FIRST 1 alpha 1 / ISOLATED / SECOND 2 beta 2 35ms / STOPPED   PASS (1.67s)
nats:      MIRROR READY push=false / FIRST 1 alpha 1 / ISOLATED / SECOND 2 beta 2 3.007s / STOPPED PASS (4.14s)
```

- JetStream：owner 提交 v1 → 只读子进程首读经 L2 / Mongo 读到 v1、续租兴趣 → owner 收到兴趣后提交 v2 → 子进程 Cached 读 35ms 读到 v2（陈旧上限 60s，只能来自推送）。DTO 只解码 `name` / `members`，owner DAO 的 `Motto` 被忽略。
- 普通 NATS：推送关闭（健康 `snapshot_push=false`），v2 在首读 3.007s 之后读到（陈旧上限 3s），按需读取。
- 跨租户 / profile：同一公会 ID 用 tenant 7 或 policy（profile）3 的 spec 读，`found=false`、不交出数据；源交回 tenant 0 / profile 0 的快照时 reader 报错拒绝。
- 只读：子进程注册表没有 `remote_entity` / `remote_entity.atomic_store` / `redis.versioned_lock`，能力不是 publisher / manager，Guild 没有 builder。
- 停止：`StopWithContext` 返回 nil，第二次 nil，之后读返回 `ErrSnapshotClientStopped`。停止取消在途读取由 kit 用例覆盖（真实 Mongo 读无法稳定卡在途中）。
- 全量 `scripts/test-remote-generated.sh`：原 12 条 `TestGeneratedRemote*` 与新用例全部 PASS（27.6s）。
- 资源：库 `roost_remote_generated_<pid>_<ts>`（用例结束 drop）、Redis 键前缀 `<库>:mirror:<mode>:`（scoped 记录后删除）、JetStream 流 `<库>.mirror.jetstream` 派生名（内存存储，用例结束删除）；事后检查 Redis 无 `roost_remote_generated_*mirror*` 键、Mongo 无残留库。

### 5.5 验证（`GOWORK=off`）

- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity ./kit/remoteentity`。
- `go test -race -count=3 ./kit/remoteentity ./remoteentity ./codegen/internal/entity ./kit/mods` 全过；`go test -count=1 . ./app ./kit` 全过。
- `go test -count=1 ./codegen/...` 全过；`go generate ./...` 之后 porcelain 只有本步改动。
- game-demo：`roost project new planet -template game-demo -mods configdata,mongo,nats,dataengine,nest -skip-deps`，replace 到 worktree，`go mod tidy`、`roost generate` 后无差异、`go build ./...`、`go vet ./...`、`go test ./...` 全过。
- 未改 nest / entity / dataengine / sync，未跑 glsvet。

### 5.6 兼容影响

见 §3。补充：`RemoteEntityMod` 的能力表多一项；生成器对 `remote=mirror` 由生成改为报错（仓内、kit、demo 都没有使用方）；`codegen/internal/entity` 的 `remote=bogus` 报错文本由 `none|managed|mirror` 改为 `none|managed (a read-only mirror is a //roost:mirror DTO)`。

### 5.7 未完成 / 边界

- 版本下限：生成的只读产物需要 v1.20.2 之后的 core（`entity.RemoteMirrorReader` / `RemoteSnapshotReadOnly` / `RemoteMirrorSpec`、kit `RemoteMirrorMod`），发版准备时统一上调 `minimumVersions.Core`，本步未改。
- `RemoteMirrorMod` 是手写装配的 Mod，`StopBudget` 不计入生成器的 `shutdown.total_timeout`（RR-20260926-66 / C31 的既有边界）；game-demo 没有加只读服务，所以生成器清单没有它。
- owner 的 Mongo 删除仍是删文档（没有带版本的墓碑），只读 loader 读到删除是 `found=false`；防复活靠共享 L2 墓碑（B2）。不在本步范围。
- 读结果仍没有“确认时刻 / 年龄”字段（第 3 步留项）。

## 6. 第 6 步（Linux 真实环境的故障与性能）需要的外部条件

- **Linux 主机**（方案要求 Linux 报告；本机是 macOS）：同机、同配置的前后对照，CPU / 内核 / Go 版本固定，不与索引重建或别的压测并跑。
- **真实依赖可被独占地注入故障**：NATS JetStream 集群（≥3 节点，能强杀 / 重启单节点、制造静默断线与分区）、Redis（单机与 Cluster，能停主 / 切主）、Mongo 副本集（能停主、制造选举）；需要持有 `remote-acceptance.lock` 的全局窗口，或一套独立于共享隔离环境的专用环境（自建 toxiproxy / 进程，不 reset 共享 toxiproxy）。
- **两进程以上的部署形态**：owner 与只读服务分属不同进程（最好不同主机），可强杀重启任一方；owner 切换（两个 owner 进程轮流取得所有权）与 outbox 补发需要正式 kit 装配的 owner 服务，第 5 步的 `testdata/remoteflow` 两进程样例可作为起点。
- **负载与口径**：方案“性能与完成标准”节的对照（owner 直读、Mirror L1 命中、冷加载；payload 大小、key 数、热点比例、订阅扇出、更新频率；吞吐、p95 / p99、allocs/op、RSS、队列深度、合并加载比例、回填放大；慢消费者与断线恢复）。需要维护者给出目标负载（key 数、扇出、更新频率）或接受按样例实测选取。
- **时长与额度**：强杀 / 重投 / 静默断线 / owner 切换各场景都要多轮重复，长稳至少数十分钟；按维护者节省额度的要求需单独批准。
