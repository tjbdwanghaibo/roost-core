# Roost 能力清单

本页是 roost-core **当前 main** 的能力索引，给要用 core 做游戏服务端的 agent 或工程师查用：按需求条目查表 → 看“怎么接入” → 读“文档”列里的现行契约。每条只有一两句，不重述契约；[CHANGELOG](../../CHANGELOG.md) 的 `[Unreleased]` 是 main 上未发版的变化，也算在内。

限制、未验证范围与性能数字以 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md) 和 [性能报告](../performance/README.md) 为准，本页不抄数字；两处说法不一致时以它们和对应 guide 为准。第一次接触框架先读[入门手册](../GETTING-STARTED.md)与[模块目录](README.md)。

“状态”一列只用五个词：

- **正式**：代码在 main、有现行契约文档与回归入口。不等于已做完生产、跨机或全平台验收，这些看“主要边界”与 KNOWN-LIMITS。
- **原型**：代码在 main，但文档明确写着不是承诺的正式能力。
- **参考实现（demo）**：业务参考代码，在 game-demo 模板（[demo](../../demo/README.md)）或 `scripts/testdata/<玩法>/` 夹具里，不是框架 API；生成或拷贝后归业务，数值都是样例。
- **现成模块**：`roost module add` 安装的业务模块（[modules](../../modules/README.md)）：提前写好并验收过的业务代码，安装后归工程所有、可以直接改。
- **只有设计**：只有方案文档，没有实现。

“怎么接入”里的 `roost …` 是生成器 CLI（`roost help <能力>` 有完整说明），`roost.yaml` 键是生成工程的清单，包路径相对仓库根（import 前缀 `github.com/tjbdwanghaibo/roost-core/`）。

## 进程与装配

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| App 与 Mod 生命周期 | 一个二进制声明多个服务类型，按 Mod 依赖排序启动，三步停机（发起 → 等排空 → 释放） | `roost project new` 生成 bootstrap；`roost add service <name>`、`roost add mod <name> -service <svc>`（或 `roost.yaml` 的 `services.<svc>.mods`）；业务写 `internal/service/<svc>/service.go` 的 `Init` / `Shutdown`；包 `framework/app` | 正式。依赖写 Mod 名而不是能力名；停机预算是 `shutdown.total_timeout` 一个窗口 | [App 生命周期](guide/01-app-lifecycle.md)、[Wiring](guide/13-wiring.md) |
| 可用的 Wiring Mod | lock、ops、statslog、configdata、etcd、redis、mongo、nats、syncbus、remote_entity、remote_mirror、dataengine、manager、nest、heartbeat、saga、web | `roost add mod <name> -service <svc>`；加 nest 或 saga 时自动补齐 dataengine、mongo、nats；包 `wiring/<mod>` | 正式。remote_mirror 与 remote_entity 互斥 | [项目生成器 §5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)、[Wiring](guide/13-wiring.md) |
| 能力注册表 Registry | 进程内按名字取接口，本地实现与远端客户端满足同一接口 | `app.Lookup[T](registry, name)`；能力名在 `wiring/mods` | 正式。不用另一个 App 的全局对象代替本实例的 Registry | [总览 §4](guide/00-overview.md#4-分层) |
| 停机先关外部输入 | 任何外部输入在它依赖的 Mod 停止前已关准入并排空，之后到达的请求得到确定的“没执行、可重试” | 实现 `app.IngressCloser`，或在 `Service.Init` 里 `app.AddIngress(registry, name, closer)`；总线、ops 管理端点、effect 与 saga 步骤消费者已自动接上 | 正式。业务直接用 JetStream 订阅建的消费者不自动登记 | [App 生命周期 §4.1](guide/01-app-lifecycle.md#41-停机第一步关外部输入) |
| 单实例锁与 fail-stop | 一个“服务类型 + sid”同一时刻只有一个进程；失锁即停，不带病服务 | 配置段 `singleton`（带 DataEngine 的服务要求 `singleton.enabled: true`）；存活查询 `app.SingletonLiveness` | 正式。活性只回答是否持有进程锁，不证明业务健康；后端短断能扛多久见文档 | [App 生命周期 §3](guide/01-app-lifecycle.md#3-运行与失败) |
| 进程配置：声明、检查、叠加 | 键、类型、默认值声明一次，启动、`--check-config`、doctor、生成器共用；可拆成多文件叠加 | Mod / Service 的 ConfigSchema 结构体与 tag；`roost config check --service <name>` 或 `--all`；入口文件写 `config.includes` | 正式。未声明的键与非法值直接拒绝；include 只支持一层 | [配置](guide/07-config.md)、[命令参考 §14](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#14-配置格式和生产门禁) |
| 配置表与热更 | 强类型业务表，整份快照原子发布，规则在每次加载与重载时强制 | `roost add table <name>`（Go metadata + CSV）或 cfggen（YAML schema）；Mod `configdata`；ops 命令 `configdata.reload`、`configdata.rollback`、`configdata.status` | 正式。重载只读 JSON，改 CSV 后先 `roost generate`；回滚只保留一级；排队中的消息保持准入时那一代配置 | [配置 §5](guide/07-config.md#5-运维重载)、[cfggen meta](../../codegen/docs/CFGGEN_META.zh-CN.md) |
| 代码热补丁 | 以 Go plugin 加载补丁包替换登记过的补丁点，可回退 | 包 `framework/hotcode`；运维命令 `hotcode.*`（命令清单未逐项核对） | 正式。受目标平台与工具链约束，Linux 插件实机未验收 | [配置 §6](guide/07-config.md#6-维护) |
| 观测与运维面 | 健康 / 就绪探针、指标导出、管理命令入口、周期统计 | Mod `ops`（`/healthz`、`/readyz`、`/metrics`、`/admin/execute`）与 `statslog`；业务指标直接调 `infra/observe/metrics` 的包级函数 | 正式。指标 label 只放有界枚举；`/readyz` 的 Degraded 仍就绪 | [观测](guide/11-observability.md)、[指标清单](OBSERVABILITY.md) |
| GM / 运维命令 | 业务命令登记进 App 的 admin 命令表，经 ops 的 HTTP 端点执行，带 token 与审计日志 | 命令表 `app.ModAdmin`（包 `infra/observe/admin`）；配置 `ops.admin_enabled`；框架自带 `entity.kick`、`entity.unload`、`configdata.*` | 正式。不是人工审批系统；改档写成业务自己的 GM handler | [观测 §5](guide/11-observability.md#5-管理与安全)、[demo GM](../../demo/WALKTHROUGH.md#gm-运维面admin-命令不是另一个-http-服务) |
| 业务 HTTP 路由 | 类型化 HTTP 路由（JSON / raw、gzip、错误映射），给支付回调、运营后台用 | `roost add webroute <name>`，标记 `//roost:web`；Mod `web`；服务 `Init` 里 `kitweb.RegisterRoutes(registry, &web.Service{})` | 正式。web Mod 不做鉴权；绑非回环地址要显式 `web.allow_public_addr: true` | [命令参考 §11](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#11-webroute)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#业务-http-路由与配置检查) |
| 错误码 | 全工程唯一的错误码，生成期查重并导出 CSV | `roost add errcode <name>`；`errcode.Define(code, name, message)`；编号空间 `roost.yaml` 的 `ids.errcode` | 正式 | [命令参考 §12](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#12-errcode) |
| 部署生成物 | Shell / systemd、Docker、Kubernetes 三套部署基线与 CI 工作流 | `roost project sync` 生成；`make cicd-check`、`make deploy-shell`、`make deploy-docker`、`make deploy-k8s` | 正式（生成基线）。跨机 HA、断电未验收；带 WAL 的服务用 StatefulSet 与独占卷 | [项目生成器 §17](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#17-生产部署生成物)、[CI/CD](../../codegen/docs/CI_CD_IMPLEMENTATION.zh-CN.md) |
| 本地开发依赖 | 一条命令起 Mongo 副本集、NATS、Redis 并运行服务 | 生成工程的 `make dev-up`、`make run SERVICE=game` | 正式（开发用） | [项目生成器 §11](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#11-启动和本地依赖) |

## 状态与事务

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| Entity / Component / DAO | Entity 是锁与生命周期边界，Component 写玩法方法，DAO 是事务里可回滚的状态 | `roost add entity <name>`、`roost add component <name> --entity <owner>`、`roost add dao <name> --entity <owner>`；标记 `//roost:entity`、`//roost:dao` | 正式。持久字段只经生成的 setter 修改；每个生成字段必须写稳定的 `store` 键 | [Nest 与实体](guide/02-nest-entity.md)、[tag 手册](../../codegen/docs/TAGS.zh-CN.md) |
| DAO 字段意图 | 每个字段声明落不落库、同不同步，以及 map 的实现 | 字段 tag `dao:"persist,sync"`、`nopersist`、`nosync`、`map=fast`、`map=sharded`；只在内存的 DAO 写 `//roost:dao nocoll` | 正式。嵌套 struct / map / list 的子字段变化按路径同步与落库 | [命令参考 §4](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#4-dao-生成器)、[递归增量持久化](DATAENGINE-INCREMENTAL-PERSISTENCE.md) |
| Nest handler 事务 | 一条消息在声明的实体锁下执行，失败按声明回滚，成功按持久级别提交 | `roost add handler <name> --entity <E> --component <C>`；标记 `//roost:nest rollback=… durability=…`；调用方用生成的 Sender | 正式。业务 worker 内不得同步等网络、磁盘、数据库或另一个 Nest 请求，不得自开 goroutine 访问实体 | [Nest 与实体 §1](guide/02-nest-entity.md#1-执行模型)、[执行模型](RUNTIME_EXECUTION_MODEL.md) |
| 持久级别 | memory / async / strict / pipelined 四档，决定回复前等到哪一步 | `//roost:nest` 的 `durability=` | 正式。strict 等 WAL fsync，仍不等 Mongo 投影；memory 不得改持久字段 | [Nest 与实体 §4](guide/02-nest-entity.md#4-提交与回复)、[Pipelined 提交](NEST_PIPELINED_COMMIT.md) |
| 回滚 | 失败的事务把 DAO 恢复到事务前 | `//roost:nest` 的 `rollback=state` 或 `undo`；状态都放 DAO，组件不另存要回滚的状态 | 正式。Skill Runtime、actionflow runner 不在回滚域内 | [Nest 与实体 §3](guide/02-nest-entity.md#3-事务与回滚)、[事务与 WAL](NEST_TRANSACTION_WAL.md) |
| 回复错误判别 | 一张表判断一次失败“是否可能已提交、能否重试” | 对生成 Sender 的返回值按表用 `errors.Is` | 正式。结果未知不得盲目重试，按业务幂等键核对 | [Nest 与实体 §6](guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试) |
| 提交后动作 | 只在事务提交后执行的回调（推送、通知、组同步成员变化） | `if !nest.AfterCommit(fn) { fn() }` | 正式。回调里不能再派发 Nest 消息、不做 I/O | [Nest 与实体 §3](guide/02-nest-entity.md#3-事务与回滚) |
| 多实体事务与锁序 | 一条消息锁多个实体，一起提交或一起回滚（转账、交易、组队领奖） | handler 开头写多个 Entity 参数，或 `target=` / `targets=`；锁档用 marker 的 `category=`（`entity.EntityCategoryWorld`、`PlayerScoped`、`Player`、`Other`） | 正式。只能访问已声明的目标，按锁档与 ID 顺序取锁；handler 内同步 Nest 请求被拒 | [Nest 与实体 §2](guide/02-nest-entity.md#2-锁与实体)、[demo 锁档](../../demo/WALKTHROUGH.md#实体锁档与跨实体事务) |
| 查询后续行（Await） | handler 里发起外部查询，结果回来后在实体锁下继续；查询后才知道的新目标重新准入 | `framework/nest` 的 Await 与 `WithResumeTargets` | 正式 | [三池与 Await](NEST-AWAIT.md) |
| 三池调度 | short / long 两个业务池加 io 池，按实体 kind 分流长短业务 | entity marker 的 `businessPool=short` 或 `long`；容量键 `nest.short.queue_capacity` | 正式。short 池内不得阻塞等待 | [三池与 Await](NEST-AWAIT.md#三种执行资源) |
| 实体生命周期 | 登录加载、首次创建、销毁、删档 | `roost add lifecycle <Entity> --service <svc>`（生成 Get / Create / GetOrCreate / Destroy）；登录端点 `roost add endpoint <name> --handler <domain> --lifecycle <Entity>` | 正式。删档后同一 ID 不可重建；直接 Create 必须用新发的 ID | [Nest 与实体 §2](guide/02-nest-entity.md#2-锁与实体)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#删档后同-id-重建d10) |
| 冷加载 | 访问不在内存的实体时由 io 池从库加载，上线后新增的 DAO 按初值补空 | 自动；不在 getter 里做 I/O | 正式。旧 schema 与未来 schema 都拒绝加载 | [DataEngine §4](guide/03-dataengine.md#4-冷加载与-schema)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#新建实体落库与缺失-dao-补空r15维护者-2026-10-30) |
| 实体归属服务 | 声明某类实体只由一个服务写，别的服务经 RPC 写 | marker `//roost:entity service=<服务名>`，改后先 `roost project sync` 再 `roost generate` | 正式。未声明时任何带 Nest 的服务都能写 | [项目生成器 §6.w](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#6w-实体归属服务roostentity-service) |
| 单实体运维 | 踢下线、把一个实体从内存卸载后按库重载 | ops 命令 `entity.kick`、`entity.unload` | 正式。不是改档入口；运行期不得直接改权威集合 | [DataEngine 单实体运维](guide/03-dataengine.md#单实体运维命令d16) |

## 持久化

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| DataEngine 持久链路 | 唯一正式链路：DAO 变更 → 提交记录 → WAL → 投影 → Mongo → outbox | Mod `dataengine`（`wiring/dataengine`）；业务不直接写库 | 正式。MongoDB 要求副本集，最低版本与检查工具 `cmd/mongocheck` 见文档 | [DataEngine](guide/03-dataengine.md)、[最低要求](guide/03-dataengine.md#dataengine-对-mongodb-的最低要求) |
| WAL 与崩溃恢复 | 提交先进本机 WAL，重启后从未确认处重放，重放幂等 | 自动；目录键 `dataengine.wal.dir`；只读分析 `cmd/walinspect` | 正式。目录由一个持有者独占；不提供跨机 WAL 热迁移 | [DataEngine §2](guide/03-dataengine.md#2-wal-格式与恢复)、[事务与 WAL](NEST_TRANSACTION_WAL.md) |
| 投影 | WAL 记录按实体保序、并行写入 Mongo | 自动；配置段 `dataengine.projection` | 正式。要等落库用明确的 Flush / 投影等待，不能只等 strict 成功；投影冲突是 fail-stop | [DataEngine §3](guide/03-dataengine.md#3-投影与幂等) |
| 增量持久化 | 只写变化的字段路径，嵌套 struct / map / list 逐路径 `$set` / `$unset` | 自动（由生成的 setter 驱动） | 正式。完整替换的边界见文档 | [递归增量持久化](DATAENGINE-INCREMENTAL-PERSISTENCE.md) |
| effect（事务性 outbox） | 事务里发出的事件与实体修改同一条 WAL 记录，提交后可靠送到消费者 | `roost add effect <topic> -consumer <service>`；handler 里调生成的 `Emit<Topic>(key, event)`；消费端 `kitdataengine.SubscribeEffects` | 正式。reliable 至少一次、按 EffectID 至多一次生效；`-delivery best_effort` 至多一次；同 Key 有序 | [DataEngine §3](guide/03-dataengine.md#3-投影与幂等)、[demo 事件链](../../demo/WALKTHROUGH.md#事件链升级--奖励邮件事务性-outbox) |
| 逻辑库名与分库 | DAO 写逻辑库名，物理库由部署映射；可按 sid 分库 | `//roost:dao coll=<集合> db=<逻辑库> dbscope=sid` 或 `global`；配置 `dataengine.databases` 或 `kitdataengine.WithDatabaseResolver` | 正式。改映射等于搬数据，要先排空 WAL | [DataEngine §3](guide/03-dataengine.md#3-投影与幂等)、[改库名映射](PLAYER-MIGRATION.md#5-改-dao-库名映射) |
| store 键 | 字段在库里的稳定键，与 Go 字段名解耦 | 字段 tag `store:"<键>"`（推荐数字短键） | 正式。改键等于改数据格式，要提升 schema 并离线迁移 | [tag 手册 §3](../../codegen/docs/TAGS.zh-CN.md#3-dao持久客户端同步与服务端视图)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#dao-持久键d13) |
| schema 版本 | 运行时只认当前 schema，读到旧版或更新版都拒绝 | `//roost:dao … schema=<N>` | 正式。没有自动迁移，没有在线双读 | [schema 变更手册 §1](SCHEMA-MIGRATION.md#1-现行规则运行时只认当前-schema) |
| schema 离线迁移示例 | 停写 → 备份 → 转换 → 校验 → 切换 → 回退的执行器与命令行 | game-demo 的 `db/migrations/` 与 `cmd/schemamigrate`；公共件 `infra/storage/migration` | 参考实现（demo）。`roost add migration` 骨架未实施 | [schema 变更手册](SCHEMA-MIGRATION.md)、[demo 持久 schema](../../demo/WALKTHROUGH.md#持久-schema) |
| 按 sid 搬机 | 把一个 game sid 连同数据搬到另一台机器的参考流程与检查工具 | 流程文档；game-demo 的 `cmd/sidmove` | 参考实现（demo）。以已落库数据为准；单个玩家改绑 sid 没有框架能力 | [Player 迁移](PLAYER-MIGRATION.md) |
| 合服（别名合服） | 把服 A 并入服 B：account 记录别名，数据按清单离线迁移 | `wiring/sidmerge`；`service/account` 的 `MergeServer`；game-demo 的 `cmd/sidmerge`、`cmd/accountctl merge-server` | 正式（工具与流程）。持久总线消息只列清单不检查积压；saga 要全部终态 | [Player 迁移 §6](PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#合服d15) |

## 同步

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| 自身视图 self_sync | 玩家收到自己 Player 的状态变化（金币、道具），首帧全量、之后增量 | `roost.yaml` 的 `access.player.self_sync: true`（需要 tcp 传输）后 `roost project sync`；字段写 `dao:"persist,sync"`；包 `wiring/selfsync` | 正式。跟不上就断开，重连从全量开始；一个 Nest 只有一个 EntitySync 观察者 | [Sync 自身视图](guide/04-sync.md#玩家只看自己自身视图)、[PLAYER-ACCESS §7](PLAYER-ACCESS.md#7-玩家看自己的状态self_sync) |
| 组同步 sync_groups | 房间、队伍的状态推给成员，与自身视图共用一条连接 | `roost.yaml` 的 `access.player.sync_groups: [<实体包名>]`（需要 self_sync）；handler 里调 `SyncGroup.Join`、`Leave`、`Disband` | 正式。成员关系不持久，进程重启后在重连的 handler 里再 Join | [Sync 按组同步](guide/04-sync.md#房间队伍按组同步与自身视图同一条连接)、[Entity Sync Groups](ENTITY_SYNC.md#按组同步与-self_sync-共用连接groups) |
| Interest 视野 | 把“谁看见谁”归并成订阅：空间视野（视点 + 半径）与关系视野（联盟、队伍等显式集合），玩家与视野多对多 | `framework/sync/entitysync/policy` 的 `Interest`：`DefineSpatialView`、`DefineRelationView`、`AttachView`；手写接线，没有生成器命令 | 正式。缺省应用模式每玩家至多 9 个视野，更多改用异步应用；视野变化由业务在提交之后调用 | [Sync 视野](guide/04-sync.md#谁看见谁视野viewd19)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#广播与-aoi) |
| AOI / Block AOI | 按距离的进入、离开与分档；按块的多块可见、并行观察者计算 | `policy.AOI`；Block AOI 用手写的空间组件，不需要代码生成 | 正式。容量与性能验证范围见文档 | [Block AOI](INTEREST-BLOCK-AOI.md)、[性能报告](../performance/GATE-AOI.md) |
| 同步视图与 LOD | 同一实体对不同观察者下发不同字段集（本人 / 附近 / 队友 / 远处）；按距离档与拉远层换视图 | 实体的 packer 按 DAO 字段名声明视图（`dataengine.NewSyncViewSet`）；`InterestConfig` 的 `ViewSets`、`BandLayer`；`AOIConfig.LODs`；优先级 `entity.MergeSyncViewPriorities` | 正式。可见性由业务层声明，框架不认识字段；缺声明的视图在构造时拒绝 | [demo 同步视图](../../demo/README.md#同步视图谁看见哪些字段)、[Sync 视野](guide/04-sync.md#谁看见谁视野viewd19) |
| on_change 与 periodic | 提交后汇总变化立即发送，或按周期合并；可设合批窗口；可按实体类型混用两种节奏 | 配置 `sync.entity.mode`、`interval`、`on_change_batch_window`；`entitysync.ManagerConfig.TypeSchedules` | 正式。on_change 不是每个 setter 都发包；首次全量、恢复与 remove 走即时路径 | [Sync §3](guide/04-sync.md#3-periodic-与-on_change)、[按类型的节奏](ENTITY_SYNC.md#按实体类型的同步节奏2026-11-09) |
| DAO 的 PB 增量同步 | DAO 的同步字段编码成 Protobuf，增量只带变化本身；客户端按命名空间与视图键注册解码器 | 自动（生成器产出同步 PB 与 packer）；客户端 `EntityViews.RegisterDao`（C#）、`register_dao`（C++） | 正式。帧协议 2 / 信封 v3 在 `[Unreleased]`，服务端与客户端要一起升级 | [DAO 同步](DAO-SYNC-PROTOBUF.md)、[线格式](ENTITY_SYNC.md#线格式帧协议-2--信封-v3) |
| 按命名空间分开计 | 一类对象（如地形块）有自己的持有上限与快照预算，不挤占玩家、怪物的额度 | `entitysync.ManagerConfig.Namespaces` | 正式 | [Entity Sync Namespaces](ENTITY_SYNC.md#按命名空间分开计namespaces) |
| 超额降级 | 会话持有对象超过上限时暂缓多出的对象，不关会话、不断开 | 自动；上限 `Limits.MaxObjects` | 正式。不保证超额部分立即可见 | [Entity Sync 超额降级](ENTITY_SYNC.md#超额降级会话持有对象超过上限) |
| 背压与慢消费者 | 出站有界；一条连接收不下就断开，重连后全量或追帧 | 自动；嵌入 TCP 配置段 `player_access.tcp` | 正式。Push 返回成功只表示本地接纳 | [Sync §4](guide/04-sync.md#4-背压与恢复)、[Gate 慢消费者](GATEWAY.md#慢消费者跟不上就断开) |
| Lockstep 帧同步 | 服务器收集输入、按帧广播、保留历史；落后的会话追帧或收快照恢复；hash 分歧检测 | `framework/sync/lockstep` 的 `Match`（`MatchConfig.Recovery`）；经 player TCP 或独立 Gate 发送 | 正式。不替业务保证跨引擎确定性；独立 Gate 下大快照慢链路的边界见 KNOWN-LIMITS | [Sync §6](guide/04-sync.md#6-lockstep)、[MOBA 手册](MOBA-INTEGRATION.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#lockstep) |
| SyncBus 服务间广播 | 服务之间发布、订阅带版本的状态消息 | Mod `syncbus`（`wiring/syncbus`，依赖 nats）；配置段 `syncbus` | 正式。定位是可丢广播，要最终一致须业务自建版本缺口检测与快照对账 | [Sync §5](guide/04-sync.md#syncbus-的定位与丢失兜底) |
| 在线广播（PB push） | 向指定玩家、某个 Game 的在线连接或全部在线连接推一条 PB 消息 | `infra/network/gateway` 的 `BroadcastSender`；嵌入 TCP 的 `PushPlayer` / `PushSession` | 正式。Game 只能广播自己拥有的连接；统计按连接，不是精确在线人数 | [Gate §4](GATEWAY.md#4-synclockstep-与广播) |
| Chunk 同步 | 按区域聚合的版本化基线与增量（给物理对象等高频状态） | `framework/sync/chunksync` | 正式（随可选物理后端使用） | [Chunk 与物理](CHUNK-PHYSICS.md) |

## 跨进程

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| Remote 托管实体 | 多个进程共用的实体（公会、联盟、队伍）有唯一的写入负责人与版本 | marker `category=entity.EntityCategoryRemote remote=managed`，嵌入 `*entity.RemoteEntityBase`，服务装 Mod `remote_entity` | 正式。独占 owner 不取写许可；共享模式每次写取分布式锁；容量数字见 KNOWN-LIMITS | [Remote 与 Mirror](guide/05-remote-mirror.md)、[Remote 协议](REMOTE_ENTITY.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#remote) |
| Mirror 只读快照 | 非 owner 进程读共享实体的快照；Player handler 可带一个 Remote 引用只读 | 标记 `//roost:mirror`；只读进程装 Mod `remote_mirror`；handler 请求里带 `remote` tag 的 `RemoteViewRef` 字段 | 正式。是快照语义，不是线性一致读；`snapshots=none` 的 kind 不能被 Mirror 读 | [Remote 与 Mirror §6](guide/05-remote-mirror.md#6-普通-entity-写入--remote-view-读取) |
| 路由 RPC 与实体归属 | 对已有实体的 RPC 自动发往它的 owner 进程，owner 变了按改投重试 | 方法标 `//roost:rpc route`；归属服务 `wiring/remoteentity.WithRouteKinds(...)`，别的服务 `WithForeignKinds(...)`；骨架 `roost add rpc <name> -service <svc> -entity-service` | 正式。路由登记带过期，只增不减等边界见 KNOWN-LIMITS；同一实体的请求逐个处理 | [Service §2A](guide/09-services.md#2a-路由-rpc-与改投)、[路由登记](REMOTE_ENTITY.md#路由登记) |
| 普通 Bus RPC | 工程自己的跨进程服务：手写接口，生成传输与装配两半 | `roost add rpc <name> -service <svc>`；调用方在 `roost.yaml` 写 `services.<caller>.uses_rpcs: [<name>]` 后 `roost project sync` | 正式。调用方不传期限时接收端自己限时；回复按 `servicerpc.Classify` 分类 | [项目生成器 §6.x](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#6x-本工程自己的跨进程服务roost-add-rpc)、[命令参考 §13](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#13-servicerpc接口先行的跨进程服务) |
| Saga 长事务 | 跨实体、跨服务的多步流程，失败按倒序补偿；按业务键查结果 | `roost add saga <name> -service <svc> -steps a,b -native a,b -entity <E> -component <C>`；服务 `Init` 里 `StartSteps`；handler 里 `EmitStart` | 正式。不是跨服务 ACID，也不是瞬时原子交易；依赖 NATS JetStream；补偿可能需要人工介入；业务键只在保留期内唯一 | [Saga](guide/06-saga.md)、[Saga 契约](SAGA.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#saga) |
| effect 跨服务通知 | 一个服务事务里发事件，另一个服务可靠消费（升级 → 发邮件） | 见“持久化”的 effect 行；`-consumer` 选消费服务 | 正式 | [DataEngine §3](guide/03-dataengine.md#3-投影与幂等) |
| 服务发现 | 进程登记与查找，给 Gate / Game 与托管服务用 | Mod `etcd`（`wiring/etcd`） | 正式。发现只是候选，身份由单实例锁核对 | [Gate §2](GATEWAY.md#2-正式接入方式) |

## 时间

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| 业务时钟 | 业务时间与系统时间分开；一条消息里业务时间冻结一次；可整体偏移用于测试 | handler 约定参数 `nowUnix` / `nowUnixMilli`（由生成的分发包装填，Sender 不带时间）；Nest 之外用 `app.BusinessClock(registry)`；配置 `time.logic_offset` | 正式。生产偏移必须为 0；运行中不允许倒退 | [时间 §1A](guide/10-time.md#1a-业务时间一律取自-ctxd22-1) |
| 业务日与时区 | “哪一天”按部署配置的业务时区与每日重置时刻算 | `app.BusinessCalendar(registry)` 或 `clock.BusinessDay(t)`、`clock.NextDayStart(t)`；配置 `time.business_timezone`、`time.business_day_start` | 正式。两个键各服务必须相同；每日重置推荐惰性重置 | [时间 §2A](guide/10-time.md#2a-业务时区与业务日) |
| 实体持久定时器 | 到点在实体自己的 Nest 锁内触发，随实体落库，重启不丢 | `roost add timer --entity <Entity>`；类型与回调写在 `game/entities/<entity>/<component>_handlers.go` | 正式。离线实体不准点，加载后只补触发一次，补算由业务按 `Fire` 做；Remote 托管与 noPersist 实体不支持 | [时间 §4](guide/10-time.md#4-按实体的持久定时器)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#按实体的持久定时器d11) |
| 实体心跳 | 在线周期推进（回复、冷却、逻辑帧），跑在实体自己的锁内 | `roost add heartbeat --entity <Entity>`；登记写在 `heartbeat_beats.go` | 正式。只有 50ms～60s 十个档位；不持久、不追帧，回调按 `Beat.Elapsed` 推进 | [时间 §4A](guide/10-time.md#4a-实体心跳周期推进)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#实体心跳d20-第-1-期) |
| 行为与 actionflow | 控制实体行为的逻辑都经“动作”：取代、冲突、冻结 | `roost add behavior --entity <Entity>`（没有心跳宿主时一并加上） | 正式。runner 不随事务回滚，handler 最后一步才发起动作 | [时间 §4C](guide/10-time.md#4c-行为控制behavior取代冲突与冻结)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#行为控制d21) |
| AI | 策略驱动的 AI，挂在行为组件的心跳上 | `roost add behavior --entity <Entity> --ai` | 正式。无观察者暂停 AI 只有设计 | [时间 §4C](guide/10-time.md#4c-行为控制behavior取代冲突与冻结) |
| 移动 | 位置、朝向、路径或跟随，由心跳推进，接 AOI | 包 `gameplay/movement`（`MapBasic`、`Move`）；手写接线，参考 game-demo 的 `game/actor` | 正式 | [时间 §4B](guide/10-time.md#4b-移动与动作心跳驱动)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#移动与动作d20-第-2-期) |
| 场景时间域 | 一个场景里的倍速、慢放、时停 | `roost add timedomain --entity <Scene>` | 正式。只做场景级，全局业务时间不变速 | [时间 §4D](guide/10-time.md#4d-场景时间域变速) |
| 通用 Scheduler | 宿主驱动的定时调度器，同期限顺序确定 | 包 `infra/base/timer`；按实体使用时直接用上面的实体定时器 | 正式。不提供独立事务，持久化由宿主负责 | [时间 §3](guide/10-time.md#3-scheduler) |

## 接入与客户端

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| 玩家协议边界 | 已认证玩家的协议注册表与类型化绑定，与传输无关 | `roost add access player --service <svc>` | 正式。access 名只支持 player | [玩家协议教程](PLAYER-ACCESS.md) |
| 协议与消息号 | 从 Go 定义生成 Proto、PB、消息号、绑定与 Robot 协议表 | `roost add protocol <name> --group <g> --handler <domain>`；标记 `//roost:protocol`、`//roost:msg` | 正式。支持请求 / 响应、客户端 push、服务端通知 | [命令参考 §7](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#7-protocol-生成器) |
| 端点（协议 → Nest） | 把协议请求映射到生成的 Nest Sender，手写入口与自动转交分开 | `roost add endpoint <name> --handler <domain>`（可加 `--protocol`、`--nest-handler`、`--lifecycle`）；controller 域 `roost add module <name>` | 正式。玩家身份只取自已认证连接，业务时间不从请求取 | [玩家协议教程 §3](PLAYER-ACCESS.md#3-controller把协议转换成业务调用) |
| 嵌入 player TCP | Game 进程内的有界 TCP 接入：帧协议、连接上限、防重放、有界出站 | `roost add transport tcp`；实现 `internal/access/player/tcp/auth.go` 后 `roost config enable player-tcp` | 正式。`auth.go` 默认拒绝，生产鉴权要自己实现 | [player TCP](../../codegen/docs/PLAYER_ACCESS_TCP.zh-CN.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#嵌入-tcp-出站) |
| 独立 Gate | 客户端连 Gate，Gate 经 NATS 转发到 Game；PB、Sync、Lockstep 与广播都经它 | Gate 进程装 `wiring/gate.NewGateMod(options)`，Game 装生成的 `player/tcp.NewIngressMod(options)` | 正式。信任边界是内网加 Gate 的客户端鉴权；多进程 / 跨机矩阵未验收 | [Gate](GATEWAY.md)、[Gate 实现契约](GATEWAY-IMPLEMENTATION.md) |
| KCP / UDP / QUIC | 生成这三种传输的装配函数 | `roost.yaml` 的 `features` 加 `nettransport-kcp`、`nettransport-udp`、`nettransport-quic` 后 `roost project sync` | 正式（只到装配函数）。玩家接入层只提供 TCP；监听、握手鉴权、Session 绑定由宿主自写 | [项目生成器 §16](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#16-帧同步-transport-预设)、[MOBA 手册](MOBA-INTEGRATION.md) |
| C# 客户端 SDK 与 Unity 适配 | TCP、请求关联、EntitySync 接收、Lockstep、地形 SDK；Unity 主线程适配 | `client/dotnet/Roost.Client`、`client/unity` | 正式。真实 Unity / Godot / Unreal 全矩阵未验收 | [客户端](../../client/README.md)、[网络体验](NETWORK-EXPERIENCE.md) |
| C++ 客户端 | C++17 网络核心与 C ABI，覆盖 PB、EntitySync、Lockstep、场景时间域 | `client/cpp` | 正式。已实跑的平台与未验收的平台见手册；TLS / WS / UDP / KCP 适配未实现 | [native 手册](../../client/cpp/README.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#native-客户端2026-11-01) |
| 校时、插值与预测 | Gate 校时、客户端有界插值与输入预测、阶段诊断 | 客户端 `NetworkClock` 等，按业务显式启用 | 正式。验证范围见文档 | [网络体验](NETWORK-EXPERIENCE.md) |
| Robot | 走正式协议的机器人：功能验收、重连重试断言、负载 | 包 `robot`；`roost generate` 产出 `loadtest/protocol/registry_gen.go`；生成工程的 `cmd/robot` | 正式。没有业务 PB 就没有业务 Robot；默认玩法验收用替身鉴权与提交器 | [Robot](ROBOT.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#robot-推广验收2026-11-01) |

## 通用玩法包

都在 `gameplay/` 下，由拥有它的实体在自己的 Nest 锁内调用。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| skill 与 combat | 技能 JSON 定义编译成执行计划，Runtime 按 tick 推进，经 Host 接业务；`combat` 算战斗，`combatcomponent` 把战斗状态接到实体 | `roost add skill <name>` 生成 `game/skills/<name>.json` 与编译目录；包 `gameplay/skill`（含 `combat`、`combatcomponent`、`skillsync`、`skillcompose`） | 正式。Runtime 不在 Nest 回滚域内；跨平台确定性由业务保证 | [技能](guide/08-skill.md)、[技能专项手册](../skill/README.md)、[包 README](../../gameplay/skill/README.md) |
| attribute | 属性 ID、分层容器、派生公式与快照 | `roost add attribute <Profile>`，标记 `//roost:attribute`；包 `gameplay/attribute` | 正式。属性必须是整数 | [命令参考 §10](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#10-attribute)、[demo 属性](../../demo/WALKTHROUGH.md#属性层合成以及两种存储意图) |
| ai | 感知实体的 AI 策略边界、行为树与黑板 | `roost add behavior --entity <Entity> --ai`；包 `gameplay/ai` | 正式 | [时间 §4C](guide/10-time.md#4c-行为控制behavior取代冲突与冻结) |
| actionflow | 动作与任务的运行契约：动作组、取代、延后变更 | `roost add behavior --entity <Entity>`；包 `gameplay/actionflow` | 正式。由拥有它的实体串行调用，不引入第二个锁域 | [时间 §4C](guide/10-time.md#4c-行为控制behavior取代冲突与冻结) |
| movement | 实体在地图上的位置与移动 | 包 `gameplay/movement`；手写接线 | 正式 | [时间 §4B](guide/10-time.md#4b-移动与动作心跳驱动) |
| heartbeat | 按实体的心跳驱动，是移动、行为、周期效果的底层 | `roost add heartbeat --entity <Entity>`；包 `gameplay/heartbeat`；Mod `heartbeat` | 正式。Remote 托管实体只在拥有方跳，共享模式下不允许挂载 | [时间 §4A](guide/10-time.md#4a-实体心跳周期推进) |
| state | 实体的状态位（定身、缴械、无敌等），按来源计数，可作门控 | `roost add behavior --entity <Entity> --state` 生成 `states.go` 的目录声明；Set / Gate 接线由业务写；包 `gameplay/state` | 正式。不是状态机 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#行为控制d21)、[demo 怪物](../../demo/WALKTHROUGH.md#怪物巡逻定身与状态门控d21) |
| timedomain | 场景 / 房间级的时间域 | `roost add timedomain --entity <Scene>`；包 `gameplay/timedomain` | 正式 | [时间 §4D](guide/10-time.md#4d-场景时间域变速) |
| level | 配置驱动的关卡组件：多阶段、事件 / 条件 / 动作、变量作用域 | 包 `gameplay/level`；手写装配（没有 add 命令），可运行消费者见 `scripts/testdata/level` 与 `scripts/test-level-generated.sh` | 正式。有预算上限、没有容量承诺；示例动作是能力替身，不提供完整闯关或永久奖励 | [LevelComponent](LEVEL-COMPONENT.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#通用-levelcomponent-首版边界) |
| cellworld | 分区场景：静态方格分区，每格一个进程（Cell + Ghost），Player 不迁移 | 包 `gameplay/cellworld`；game-demo 配置 `scene.partition.enabled: true`（缺省关闭） | 原型。不是承诺的正式能力；Cell 主题没有按主题鉴权，按内网互信部署 | [包 README](../../gameplay/cellworld/README.md)、[无缝大世界方案](../maintenance/FEATURE-2026-11-03-seamless-world.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#框架服务与-game-demo) |
| terrain | 可编辑地形底座：2.5D 高度场的块坐标、地形包读写、视图与整数定点查询、笔刷、行走判定 | 包 `gameplay/terrain`（纯逻辑，不依赖 Nest / DataEngine）；块实体、编辑事务、同步与建筑在 game-demo | 正式（底座，在 `[Unreleased]`）。不支持洞穴与流体模拟；真实依赖崩溃演练、Unity、跨机与容量验收后置 | [包 README](../../gameplay/terrain/README.md)、[地形进度](../maintenance/TERRAIN-STATUS.md) |
| physics | 固定步长的原生物理模拟与已提交结果交接 | 包 `gameplay/physics`、`infra/physics/physx`；构建加 `-tags physx` | 正式（可选后端）。模拟器是物理状态的唯一权威，提交失败后停止而不回滚；平台与容量边界见文档 | [Chunk 与物理](CHUNK-PHYSICS.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#ugc-物理闯关) |

## 领域服务

都在 `service/` 下，接线在 `wiring/<领域>`。前九个是**托管框架服务**：在 `roost.yaml` 写 `services.<name>.framework: <服务>` 声明一个进程托管它，业务服务写 `services.<svc>.uses: [<服务>]`，`roost project sync` 后在 `internal/service/<svc>/framework_clients_gen.go` 得到类型化访问器；`roost project new -template game` 一次生成全部九个。协作者（身份校验、投递、频道策略等）在 `internal/service/<name>/collaborators.go`，默认全部拒绝，要项目自己实现。存储多数在 Redis，不走玩家 DAO 的 WAL。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| account | 渠道身份验证、会话令牌、选角与建角计划、别名合服 | `uses: [account]`；包 `service/account` | 正式。身份验证、玩家号分配、名字校验没有默认实现；一个账号一区服一角色 | [Service §4](guide/09-services.md#4-关键业务边界)、[demo account](../../demo/WALKTHROUGH.md#account-的-collaborators) |
| activity | 跨服活动协调：活动组、窗口、进度账本、带确认的结果投递 | `uses: [activity]`；包 `service/activity` | 正式。无法恢复的投递走人工管理流程收口 | [Service §4](guide/09-services.md#4-关键业务边界)、[SLG 手册](SLG-INTEGRATION.md) |
| chat | 频道消息的发布、历史与保留 | `uses: [chat]`；包 `service/chat` | 正式。频道策略是协作者；系统消息只能经服务端签发的令牌发 | [通用服务](../../wiring/SERVICES.md#包)、[demo 聊天](../../demo/WALKTHROUGH.md#跨服务game--chat-聊天) |
| directory | 全局名字目录：预留 → 提交 → 释放的两阶段占名 | 嵌入能力，不在九个托管服务里：`wiring/directory.NewMod(normalize, reporter)`，`app.Lookup[directory.Directory]` | 正式。同一 owner 重复预留幂等，所以不是互斥锁 | [Service §4](guide/09-services.md#4-关键业务边界) |
| global | 静态角色 / 区服路由绑定 | `uses: [global]`；包 `service/global` | 正式。Player 静态绑定 sid，不做动态迁移 | [Service §1](guide/09-services.md#1-实际目录归属) |
| mail | 邮件信封、邮箱、三段式领取；全服 / 单服 / 公会广播邮件按登录拉取合并 | `uses: [mail]`；广播用 `mail.NewLazyBroadcast`，登录端点调 `PullBroadcasts` | 正式。广播缺省拒绝，要配置投递策略；未过期的全局邮件有条数上界 | [Service §1B](guide/09-services.md#1b-mail-全服邮件)、[demo 邮件](../../demo/WALKTHROUGH.md#邮件列表与领附件客户端这一半) |
| match | 排队、分组、原子成局、取消；按分区分键；弹性人数成局 | `uses: [match]`；包 `service/match` | 正式。已结束记录只保留固定时长；没有存储原生补位；分区容量见 KNOWN-LIMITS | [Service §4](guide/09-services.md#4-关键业务边界)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#框架服务与-game-demo) |
| platform | 渠道边缘：凭证换会话、支付回调换恰好一次发货 | `uses: [platform]`；包 `service/platform` | 正式。验签由接入方提供，且必要而不充分；订单是仅插入的持久记录 | [通用服务](../../wiring/SERVICES.md#包) |
| rank | 榜单提交与分页查询，带幂等 | `uses: [rank]`；包 `service/rank` | 正式 | [通用服务](../../wiring/SERVICES.md#包)、[demo 排行榜](../../demo/WALKTHROUGH.md#排行榜rank-服务与领奖账本同一个准入) |
| session | 一次运行（副本、试炼）的记录：幂等进入、每 owner 一个活动运行、资源恰好释放一次 | `uses: [session]`；资源释放由业务的 `Releaser` 协作者实现 | 正式。与网络连接、登录令牌、Sync 会话是不同的身份；不模拟场景或战斗 | [Service §1A](guide/09-services.md#1a-session-运行会话)、[demo 副本](../../demo/WALKTHROUGH.md#跨服务game--session-副本) |
| physicsroom | 固定 owner 的瞬态物理房间：创建、加入、输入、帧提交到 Nest | `wiring/physicsroom.NewMod` 或 `NewEntityHostMod`，Game 侧 `NewClientMod`；构建加 `-tags physx` | 正式（可选）。不在九个托管服务里；没有物理跨机接管；容量不是上限结论 | [物理房间](PHYSICS-ROOM-SERVICE.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#ugc-物理闯关) |

## 生成器命令

CLI 入口 `codegen/cmd/roost`。骨架文件归业务、只建一次，之后 `project sync` / `upgrade` 不改写；带 `_gen` 的生成物由 `roost generate` 重写。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| `roost project new` | 创建完整工程 | `roost project new <name> -module <go-module>`，可加 `-services`、`-mods`、`-features`、`-template game` 或 `-template game-demo` | 正式。`-module` 必填；没有按玩法的模板 | [项目生成器 §3](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#3-创建项目) |
| `roost project sync` / `diff` | 按 `roost.yaml` 同步受控文件；预览差异 | `roost project sync`、`roost project diff` | 正式。失败回滚已写文件 | [命令参考 §1.2](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#12-同步预览和诊断) |
| `roost project doctor` | 诊断工程：配置键、跨服务一致性、漏接的消费者与协作者 | `roost project doctor`，可加 `--workflow first-business` 或 `--workflow player-tcp` | 正式 | [项目生成器 §13](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#13-doctordiff-和升级) |
| `roost project next` | 按工程真实文件算进度，只给当前一步 | `roost project next` | 正式。不替业务决定字段 | [新手练习册](../../codegen/docs/BEGINNER_WORKBOOK.zh-CN.md) |
| `roost project upgrade` / `deps` | 升级受控模板与 roost-core 依赖 | `roost project upgrade [--dry-run] [-core <version>]`、`roost project deps` | 正式 | [命令参考 §1.3](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#13-升级项目模板) |
| `roost generate` | 按依赖顺序跑全部生成器 | `roost generate`，可加 `--check`、`--changed`、`--dry-run`、`--force` | 正式。`--check` 用于漂移检查 | [命令参考 §2](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#2-统一生成流水线roost-generate) |
| `roost add service` | 新增一个业务服务 | `roost add service <name>` | 正式 | [命令参考 §3](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#3-业务骨架roost-add) |
| `roost add mod` | 给服务装一个 Wiring Mod，自动补依赖 | `roost add mod <name> -service <svc>` | 正式 | 同上 |
| `roost add access` | 玩家协议边界 | `roost add access player --service <svc>` | 正式 | 同上 |
| `roost add transport` | 嵌入 player TCP | `roost add transport tcp` | 正式。要求先有 access player | 同上 |
| `roost add module` | 一个 controller 域（`game/controllers/<name>/controller.go`） | `roost add module <name>` | 正式。这是 controller 域，不是业务模块；业务模块用 `roost module` | 同上 |
| `roost add protocol` | 协议定义骨架并分配消息号 | `roost add protocol <name> --group <g> --handler <domain>` | 正式 | 同上 |
| `roost add entity` | Entity 骨架并分配 kind | `roost add entity <name>` | 正式 | 同上 |
| `roost add component` | Component 骨架并接入 Entity | `roost add component <name> --entity <owner>` | 正式 | 同上 |
| `roost add handler` | Nest handler 骨架 | `roost add handler <name> --entity <E> --component <C>` | 正式 | 同上 |
| `roost add lifecycle` | 实体的加载、创建、销毁边界 | `roost add lifecycle <Entity> --service <svc>` | 正式 | 同上 |
| `roost add endpoint` | 协议到 Nest Sender 的映射 | `roost add endpoint <name> --handler <domain>` | 正式 | 同上 |
| `roost add skill` | 技能 JSON 骨架与编译目录 | `roost add skill <name>` | 正式 | 同上 |
| `roost add table` | 配置表 schema 骨架（`configs/schema/<name>.go`） | `roost add table <name>` | 正式 | 同上 |
| `roost add dao` | DAO 定义骨架并接入 Entity | `roost add dao <name> --entity <owner>` | 正式。已存在时报错，不复用 | 同上 |
| `roost add webroute` | HTTP 路由骨架并装 web Mod | `roost add webroute <name>` | 正式 | 同上 |
| `roost add errcode` | 错误码定义并分配编号 | `roost add errcode <name>` | 正式 | 同上 |
| `roost add saga` | Saga 定义、步骤运行时与原生步骤的 handler 骨架 | `roost add saga <name> -service <svc> -steps a,b` | 正式。骨架默认拒绝 | 同上 |
| `roost add effect` | effect 载荷、发射辅助与消费者骨架 | `roost add effect <topic> -consumer <service>` | 正式。骨架默认返回错误（重投、不丢） | 同上 |
| `roost add rpc` | 工程自己的跨进程服务 | `roost add rpc <name> -service <svc>`，实体服务加 `-entity-service` | 正式 | [项目生成器 §6.x](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#6x-本工程自己的跨进程服务roost-add-rpc) |
| `roost add timer` | 实体持久定时器 | `roost add timer --entity <Entity>` | 正式。一个实体只有一个定时器组件 | [命令参考](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-timer按实体的持久定时器) |
| `roost add attribute` | 属性 profile 骨架并开启 attribute feature | `roost add attribute <Profile>` | 正式 | [命令参考 §10](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#10-attribute) |
| `roost add heartbeat` | 实体心跳宿主 | `roost add heartbeat --entity <Entity>` | 正式 | [命令参考](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-heartbeat--behavior--timedomain实体心跳行为与时间域) |
| `roost add behavior` | 行为组件，可选 AI 与状态目录 | `roost add behavior --entity <Entity> [--ai] [--state]` | 正式 | 同上 |
| `roost add timedomain` | 场景时间域 | `roost add timedomain --entity <Scene>` | 正式。要求实体有 DAO | 同上 |
| `roost id` | 分配与检查协议、Entity、Component、错误码编号；EntityID 与玩家号互相换算 | `roost id next <kind>`、`roost id check`、`roost id entity <entity-id>` | 正式。编号范围在 `roost.yaml` 的 `ids` | [项目生成器 §7](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#7-id-管理) |
| `roost config` | 检查服务配置；安全启停 player TCP | `roost config check --service <name>` 或 `--all`（可加 `--production`、`--keys-only`、`--json`）；`roost config enable player-tcp`、`roost config disable player-tcp` | 正式。退出码区分键错误、跨 Mod、跨服务与没查全 | [命令参考 §14](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#14-配置格式和生产门禁) |
| `roost help` 等 | 内置帮助、版本、环境检查、格式检查、发布清单校验 | `roost help <能力>`、`roost version`、`roost env doctor`、`roost format check`、`roost framework verify` | 正式 | [命令参考 §0](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#0-内置帮助系统) |
| 独立生成器 | 单独运行某一个生成器（dao、entity、nest、protocol、cfggen、tablegen、attribute、webroute、errcode、servicerpc） | `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/<名字>@latest …` | 正式。日常用 `roost generate` | [命令参考 §15](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#15-独立生成器参数速查) |

### 业务模块命令

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| `roost module list` | 列出内置业务模块；在工程里执行时标出已安装的 | `roost module list [--json]` | 正式。目录以它的输出为准 | [modules](../../modules/README.md) |
| `roost module plan` | 只预览，不改动工程：依赖的安装顺序、会先执行的 `roost add`、追加的 DAO 字段、分配的号、写入的文件 | `roost module plan <name…> [--json] [--service <svc>]` | 正式。`--json` 与 `add` 同一个结构 | 同上 |
| `roost module add` | 安装模块（连同依赖）并运行生成器；已安装的不再处理 | `roost module add <name…> [--json] [--service <svc>]` | 正式。落点被业务文件占用时拒绝、不覆盖；失败时模块写过的文件与 `roost.yaml` 恢复；没有卸载，分配过的号与存储键不回收 | 同上 |
| `roost module verify` | 跑已装模块自带的场景（通过数等于清单里的数、零跳过），再跑 build、vet、ID 检查与生成物漂移检查 | `roost module verify [name…] [--json]` | 正式。场景用替身提交器，不代表真实依赖验收 | 同上 |

## 现成的业务参考

### 业务模块

`roost module add <name>` 安装进工程；状态是嵌套结构体，追加在所挂实体主 DAO 的末尾。模块库里没有的需求照着已装模块的文件手写。完整列表以 `roost module list` 为准。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| 货币 wallet | 余额、按业务键幂等的加减（重复请求返回第一次的结果）、客户端消费协议；服务端发放走 handler | `roost module add wallet`；别的业务在自己的组件里调 `WalletComp().Apply(key, delta)` | 现成模块。单一货币；回执账本有上限（满了拒绝新键，不淘汰旧回执） | [modules](../../modules/README.md) |
| 每日签到 checkin | 按业务时间的 UTC 日切分，连续天数加成，奖励经货币模块在同一笔事务里发放 | `roost module add checkin`（依赖 wallet，一起装） | 现成模块。日界固定为 UTC；奖励规则是样例，在组件里改 | 同上 |

### 13 类玩法纵切

每类一个脚本：用正式 CLI 生成骨架，拷入 `scripts/testdata/<玩法>/` 的业务夹具，跑场景断言、build、vet、ID 与生成漂移检查。缺省模式离线、用替身提交器；`ROOST_<玩法>_REAL=1` 才验证真实依赖与崩溃恢复。批量入口 `python3 scripts/test-genres-generated.py --out <新目录>`。场景数、证明与不证明什么见 [KNOWN-LIMITS“游戏接入验收”](../maintenance/KNOWN-LIMITS.md#游戏接入验收)；选型与逐步命令见 [roost-genre skill](../agent-skills/roost-genre/SKILL.md#2-玩法选型)。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| SLG | 建造 → 行军、取消退款、离线收益、两名玩家抢地；跨服联盟活动联合链路 | [test-slg-generated.sh](../../scripts/test-slg-generated.sh) | 参考实现（demo）。不证明跨机、容量与真实客户端 | [SLG 手册](SLG-INTEGRATION.md) |
| MMORPG | 组队、托管交易、战斗与 Buff、同进程多地图切换与重连、双 sid 跨服赠礼 | [test-mmorpg-generated.sh](../../scripts/test-mmorpg-generated.sh)、[test-mmorpg-scenes.sh](../../scripts/test-mmorpg-scenes.sh)、[test-mmorpg-cross-sid.sh](../../scripts/test-mmorpg-cross-sid.sh) | 参考实现（demo）。没有动态 Player 跨 sid | [MMORPG 手册](MMORPG-INTEGRATION.md) |
| MOBA（帧同步） | 匹配成局、Lockstep 输入与追帧、服务器参考模拟判胜负、多实体结算、持久对局 | [test-moba-generated.sh](../../scripts/test-moba-generated.sh) | 参考实现（demo）。跨引擎确定性由业务保证 | [MOBA 手册](MOBA-INTEGRATION.md) |
| 卡牌 RPG | 抽卡保底与养成的幂等序号账本、服务器权威回合、持久回合定时器、领奖 | [test-card-rpg-generated.sh](../../scripts/test-card-rpg-generated.sh) | 参考实现（demo） | [卡牌 RPG 手册](CARD-RPG-INTEGRATION.md) |
| 合作塔防 | 四人共享房间实体：共享金币建塔、阻路校验、按期开波、团队领奖 | [test-tower-defense-generated.sh](../../scripts/test-tower-defense-generated.sh) | 参考实现（demo）。每次触发只算一步，框架不追帧 | [塔防手册](TOWER-DEFENSE-INTEGRATION.md) |
| 合作生存撤离 | 入场扣费与撤离兑付两个多实体事务、共享采集、制作、死亡掉落、物资守恒 | [test-survival-extraction-generated.sh](../../scripts/test-survival-extraction-generated.sh) | 参考实现（demo） | [生存撤离手册](SURVIVAL-EXTRACTION-INTEGRATION.md) |
| 自走棋 | 一局一个权威实体管八席卡池、商店与经济，阶段定时器，按排名领奖，按观察者裁剪的视图 | [test-auto-chess-generated.sh](../../scripts/test-auto-chess-generated.sh) | 参考实现（demo） | [自走棋手册](AUTO-CHESS-INTEGRATION.md) |
| 放置模拟 | 多工坊生产队列、有界窗口的离线收益、唤醒定时器、订单与每日奖励回执 | [test-idle-sim-generated.sh](../../scripts/test-idle-sim-generated.sh) | 参考实现（demo） | [放置手册](IDLE-SIM-INTEGRATION.md) |
| 合成休闲 | 服务器重放离线命令批次、惰性恢复体力、限时挑战、模拟的签名广告回调与到账账本 | [test-merge-casual-generated.sh](../../scripts/test-merge-casual-generated.sh) | 参考实现（demo）。广告平台是模拟的 | [合成手册](MERGE-CASUAL-INTEGRATION.md) |
| 实时派对房间 | 2～8 人短局：成局、进房、心跳推进回合、组同步、重连、补位、结算归还 | [test-party-room-generated.sh](../../scripts/test-party-room-generated.sh) | 参考实现（demo）。框架没有的部分列在手册第 8 节 | [派对房间手册](PARTY-ROOM-INTEGRATION.md) |
| 回合 PvP | 双人轮流行动、回合超时、终局拒绝、重连继续 | [test-turn-pvp-generated.sh](../../scripts/test-turn-pvp-generated.sh) | 参考实现（demo）。回合超时由宿主触发，没有挂自动定时驱动 | [三类离散玩法](SESSION-GENRES.md) |
| 合作 Roguelike | 双人投票选路线、阶段屏障、固定种子、分别领奖 | [test-coop-roguelike-generated.sh](../../scripts/test-coop-roguelike-generated.sh) | 参考实现（demo）。离散房间流程，不含实时战斗 | [三类离散玩法](SESSION-GENRES.md) |
| 异步社交经营 | 主人种植、受邀访客帮助一次、成熟收获、离线收获去重 | [test-social-sim-generated.sh](../../scripts/test-social-sim-generated.sh) | 参考实现（demo）。只演示本进程两实体事务 | [三类离散玩法](SESSION-GENRES.md) |

### game-demo 的功能

`roost project new <name> -module <mod> -template game-demo` 生成；源码在 [demo](../../demo/README.md)，按生成后的路径原样存放。下表都是参考实现（demo）。

| 能力 | 一句话说明 | 怎么接入 | 状态与主要边界 | 文档 |
| --- | --- | --- | --- | --- |
| 登录与凭据 | 经 account 校验的会话凭据握手，登录端点加载或创建 Player | `internal/access/player/tcp/auth.go`、account 的 collaborators | 参考实现（demo）。demo 渠道凭据不能用于生产 | [auth.go](../../demo/WALKTHROUGH.md#authgo两种凭据)、[collaborators](../../demo/WALKTHROUGH.md#account-的-collaborators) |
| 背包、经验与升级 | 一条玩家写请求从协议到 Nest 到 DAO 的完整链路 | `game/handler/`、`game/controllers/player/` | 参考实现（demo） | [链路](../../demo/WALKTHROUGH.md#链路) |
| 升级发奖励邮件 | effect 与状态变更同一条 WAL 记录，消费者调 mail | `effects.EmitPlayerLevelUp` 与 game 的消费者 | 参考实现（demo） | [事件链](../../demo/WALKTHROUGH.md#事件链升级--奖励邮件事务性-outbox) |
| 邮件列表与领附件 | 客户端这一半：列表、领取到背包 | 端点 + mail 访问器 | 参考实现（demo） | [邮件](../../demo/WALKTHROUGH.md#邮件列表与领附件客户端这一半) |
| 匹配组队 | game 调 match 成局 | `game/matchmaking` | 参考实现（demo） | [组队](../../demo/WALKTHROUGH.md#跨服务game--match-组队) |
| 聊天 | game 调 chat 发布与读取 | `game/chatroom` | 参考实现（demo） | [聊天](../../demo/WALKTHROUGH.md#跨服务game--chat-聊天) |
| 副本 | session 的一次运行，进入、通关、领奖 | `game/dungeon` | 参考实现（demo） | [副本](../../demo/WALKTHROUGH.md#跨服务game--session-副本) |
| 送礼 saga | 扣 → 送，失败补偿；原生步骤带准入 | `saga/gift_item/` | 参考实现（demo） | [送礼 saga](../../demo/WALKTHROUGH.md#跨域事务送礼-sagadebit--deliver失败补偿) |
| 公会 = alliance 服务 | 独立进程独占联盟实体，写走路由 RPC 或 saga，game 读 Mirror | `internal/rpc/alliance/`、`internal/service/alliance/`、`game/guild/` | 参考实现（demo）。联盟服务方案实施中，不在 `service/` 下；边界见 KNOWN-LIMITS“Remote” | [公会](../../demo/WALKTHROUGH.md#跨服务公会--alliance-服务写走-rpc-或-saga读走-mirror)、[联盟服务方案](../maintenance/FEATURE-2026-11-04-alliance-service.md) |
| 地图 Scene | Scene 是一个实体，子系统各自带锁 | `game/scene`、`internal/service/game/scene.go` | 参考实现（demo） | [地图](../../demo/WALKTHROUGH.md#地图scene-是一个实体它的-system-各自带锁) |
| 移动 | 客户端发目标，服务端在实体心跳里走 | `game/actor` | 参考实现（demo） | [移动](../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20) |
| 怪物 | 巡逻、定身与状态门控 | Monster 实体 + 行为组件 | 参考实现（demo） | [怪物](../../demo/WALKTHROUGH.md#怪物巡逻定身与状态门控d21) |
| 场景时间 | 副本 2 倍速 | 场景时间域 | 参考实现（demo） | [场景时间](../../demo/WALKTHROUGH.md#场景时间副本-2-倍速d22) |
| 服务端权威状态同步 | Player 是复制主体，场景是政策 | 自建 Manager + Nest 提交观察者 | 参考实现（demo） | [状态同步](../../demo/WALKTHROUGH.md#服务端权威状态同步player-是复制主体entitysyncmanager-是调度器scene-是政策) |
| 同步视图 | 本人、附近的人、队友各拿各的字段 | `game/entities/<kind>/sync_packer.go` | 参考实现（demo）。在 `[Unreleased]` | [同步视图](../../demo/README.md#同步视图谁看见哪些字段) |
| 兴趣 | 距离视野与关系视野 | 场景的 Interest | 参考实现（demo） | [兴趣](../../demo/WALKTHROUGH.md#兴趣距离是一种视野关系是另一种) |
| 刷新 | 场景该有多少东西活着 | spawner | 参考实现（demo） | [刷新](../../demo/WALKTHROUGH.md#刷新场景该有多少东西活着) |
| 装备栏 | demo 唯一的嵌套字段 | `PlayerDao` 的嵌套结构体 | 参考实现（demo） | [装备栏](../../demo/WALKTHROUGH.md#装备栏demo-唯一的嵌套字段以及为什么它在这里) |
| 属性 | 分层、合成与两种存储意图 | `game/gameplay/attribute` | 参考实现（demo） | [属性](../../demo/WALKTHROUGH.md#属性层合成以及两种存储意图) |
| 排行榜 | rank 服务，与领奖账本同一个准入 | `game/ranking` | 参考实现（demo） | [排行榜](../../demo/WALKTHROUGH.md#排行榜rank-服务与领奖账本同一个准入) |
| 技能目录 | 启动时编译，客户端可查 | `game/skills` | 参考实现（demo） | [技能目录](../../demo/WALKTHROUGH.md#技能目录启动时编译客户端可查) |
| lockstep 战斗 | 匹配之后的帧同步对战 | `game/battle` | 参考实现（demo） | [实时战斗](../../demo/WALKTHROUGH.md#实时战斗lockstep-帧同步匹配之后的那一段) |
| GM 运维面 | 加道具、发补偿邮件、查 saga 等 admin 命令 | `internal/service/game/gm.go` | 参考实现（demo）。开发 token 生产被拒 | [GM](../../demo/WALKTHROUGH.md#gm-运维面admin-命令不是另一个-http-服务) |
| World | 进程内单例实体的职责 | `EnsureWorld` | 参考实现（demo） | [World](../../demo/WALKTHROUGH.md#world-的职责) |
| 机器人压测 | 走真实凭据的机器人，同时是回归测试 | `cmd/loadtest`、`make loadtest` | 参考实现（demo） | [压测](../../demo/WALKTHROUGH.md#机器人压测--回归测试) |
| 实体锁档与跨实体事务 | 锁档怎样定、多实体事务怎样写 | 实体 marker 的 category | 参考实现（demo） | [锁档](../../demo/WALKTHROUGH.md#实体锁档与跨实体事务) |
| schema 迁移与 sid 搬机 | 离线迁移示例与搬机检查工具 | `cmd/schemamigrate`、`cmd/sidmove` | 参考实现（demo） | [持久 schema](../../demo/WALKTHROUGH.md#持久-schema)、[搬 sid](../../demo/WALKTHROUGH.md#把一个-sid-搬到另一台机器) |
| 可编辑地形与建筑 | 地形包、玩家笔刷编辑与同步、高度场上的行走与寻路、水位、建筑是普通同步实体 | `game/terrainmap/`、`game/entities/terrain_chunk/`、`game/entities/building/`、`cmd/terrainpack`（缺省关闭） | 参考实现（demo）。在 `[Unreleased]`；真实依赖、Unity、跨机验收后置 | [可编辑地形](../../demo/README.md#可编辑地形p1)、[水与建筑](../../demo/README.md#水与建筑p4)、[地形进度](../maintenance/TERRAIN-STATUS.md) |
| 分区场景 | cellworld 接进场景，多进程分格 | 配置 `scene.partition`（缺省关闭）；演练 `scripts/test-seamless-demo.sh` | 原型 | [分区场景](../../demo/README.md#分区场景原型分支-featseamless-world-prototype) |

## 明确没有 / 不适合的

需求落在这张表里时，如实说明缺口与出处，不要用现有零件拼一个“能跑”的假 demo。

| 需求或类型 | 结论 | 是哪条限制 | 出处 |
| --- | --- | --- | --- |
| 按玩法的配方（`roost project new -recipe`）、把 13 类玩法与 game-demo 拆成模块 | 只有设计 | 业务模块安装器已实现（见“业务模块命令”），目前只有两个模块；配方与现有玩法、demo 的迁移是方案的后几期 | [业务模块方案](../maintenance/FEATURE-2026-11-10-business-modules.md) |
| 业务模块的卸载 | 不做 | 分配过的号与存储键不回收；要去掉一个模块时按 `roost.yaml` 的 `modules.<name>` 归属清单手工删 | [业务模块方案“决定一览”](../maintenance/FEATURE-2026-11-10-business-modules.md#决定一览) |
| FPS / 动作射击 | 不建议 | 心跳档位最短 50ms；没有延迟补偿与命中回溯，没有通用客户端预测；玩家接入只有 TCP | [skill §6](../agent-skills/roost-genre/SKILL.md#6-不适合--暂缓的类型) |
| 无缝开放世界（多进程分区） | 只有原型 | Player 静态 sid、Interest 按进程；cellworld 是原型不是正式能力；正式做法是同进程多地图加按 sid 分服 | [skill §6](../agent-skills/roost-genre/SKILL.md#6-不适合--暂缓的类型)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#框架服务与-game-demo) |
| 状态同步的 MOBA / RTS | 暂缓 | 阵营或单位视野挂接几十个，必须用异步应用并自行压测；没有纵切与容量数据 | [skill §6](../agent-skills/roost-genre/SKILL.md#6-不适合--暂缓的类型)、[Sync 视野](guide/04-sync.md#谁看见谁视野viewd19) |
| 动态 Player 跨 sid 迁移、单个玩家改绑 sid | 没有 | Player 静态绑定 game sid；跨服协作走 Remote、Saga、邮件 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#平台与外部验证)、[skill §5](../agent-skills/roost-genre/SKILL.md#5-公共限制所有玩法都适用) |
| 玩家接入的 UDP / KCP / QUIC 监听 | 没有 | 只生成装配函数；监听、握手鉴权、Session 绑定宿主自写 | [项目生成器 §16](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#16-帧同步-transport-预设)、[Gate §4](GATEWAY.md#4-synclockstep-与广播) |
| 高于 20Hz 的实体心跳、固定步长逻辑帧 | 没有 | 心跳只有 50ms～60s 十档，不追帧 | [时间 §4A](guide/10-time.md#4a-实体心跳周期推进)、[派对房间 §8](PARTY-ROOM-INTEGRATION.md#8-框架没有什么) |
| 离线实体定时器准点 | 没有 | 未加载的实体到期不唤醒，加载后补触发一次；必须准点的由业务发消息 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#按实体的持久定时器d11) |
| Remote 托管或 noPersist 实体的持久定时器 | 没有 | `roost add timer` 拒绝 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#按实体的持久定时器d11)、[DESIGN-TODO](../maintenance/DESIGN-TODO.md#未排期候选) |
| DAO 自动 schema 迁移、在线双读、迁移骨架命令 | 没有 | 运行时只认当前 schema；离线迁移按手册 | [schema 变更手册](SCHEMA-MIGRATION.md)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#平台与外部验证) |
| SyncBus 可靠投递与框架级对账 | 没有 | 定位是可丢广播；最终一致由业务自建对账 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#广播与-aoi) |
| 跨服瞬时原子交易 | 不支持 | Saga 是持久步骤状态机加补偿，不是跨服务 ACID | [Saga §1](guide/06-saga.md#1-模型) |
| 按观察者过滤可见性（隐身、相位、阵营保密） | 没有 | 按需求触发的候选，未实施 | [DESIGN-TODO](../maintenance/DESIGN-TODO.md#按需求触发的功能) |
| 无观察者暂停 AI | 只有设计 | 设计稿保留，未实施 | [AI 方案](../maintenance/FEATURE-2026-10-22-ai-idle-without-observers.md) |
| 匹配的存储原生补位、房间跨机接管 | 没有 | 补位只有业务层写法；房间没有跨机接管 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#框架服务与-game-demo)、[派对房间 §8](PARTY-ROOM-INTEGRATION.md#8-框架没有什么) |
| 运行期直接改 Mongo 权威文档、框架级改档与整档恢复 | 不支持 | 加载着的实体以内存为权威；在线改档写业务 GM handler，整档恢复走离线流程 | [DataEngine §3](guide/03-dataengine.md#3-投影与幂等)、[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#运行期改档与单实体运维d16) |
| 名字规则（长度、敏感词、全服唯一策略） | 需手写业务 | 属于业务规则；全服唯一可用 directory 的两阶段占名 | [Service §4](guide/09-services.md#4-关键业务边界) |
| 物理：网格烘焙、运行期刚体增删、公开碰撞事件、物理跨机接管 | 没有 | UGC 物理只有有限基础几何与轨迹描述 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#ugc-物理闯关) |
| 跨机 HA、断电、真实 Unity / Godot / Unreal 全矩阵 | 未验收 | 本机集群与 loopback 不证明这些 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#平台与外部验证)、[外部验证](../maintenance/EXTERNAL-VERIFICATION.md) |
| Windows 服务端 | 不保证 | 非框架保证平台，只保证编译与 CLI 兼容 | [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#平台与外部验证) |

## 需求条目 → 能力

“能力”指上面各表的行；“最接近的现成参考”是玩法夹具或 game-demo 的一块。写着“需手写业务”的，框架只给事务、持久与同步的底座，规则自己写。

| 需求说法 | 用到的能力 | 最接近的现成参考 |
| --- | --- | --- |
| 玩家货币 | 现成模块 wallet（`roost module add wallet`）：余额、幂等加减、消费协议 | 模块 wallet；多种货币或充值到账照着它扩 |
| 玩家背包 | Entity / Component / DAO + Nest handler（strict）+ 自身视图；状态放主 DAO 下自己的嵌套结构体；需手写业务 | game-demo“背包、经验与升级”；写法照模块 wallet |
| 幂等扣发（重试不重复扣费、不重复发奖） | 实体上的持久业务事实（序号账本、领奖事实）+ 回复错误判别 | 卡牌 RPG、SLG 夹具；三类离散玩法的命令序号 |
| 抽卡保底 | 配置表（按值捕获）+ 幂等序号账本，同一事务里改保底计数与发卡；规则需手写业务 | 卡牌 RPG 夹具 |
| 离线收益、建造 / 行军倒计时 | 实体持久定时器（离线只补一次）+ 按区间惰性结算 | 放置模拟夹具；SLG 夹具的建造、行军与离线收益 |
| 每日签到 | 现成模块 checkin（`roost module add checkin`，连同 wallet） | 模块 checkin |
| 每日重置、体力恢复 | 业务日与时区 + DAO 里存上次重置的业务日，惰性重置；体力按服务器时间惰性恢复；需手写业务 | 模块 checkin 的日界写法；放置模拟夹具的每日奖励；合成休闲夹具的体力 |
| 玩家间交易、转账 | 多实体事务与锁序 | MMORPG 夹具的托管交易 |
| 跨服送礼、跨服交易 | Saga（原生步骤 + 补偿） | game-demo“送礼 saga”；MMORPG 的双 sid 赠礼脚本 |
| 组队副本 | match（成局）+ session（一次运行）+ 多实体事务（领奖） | game-demo“匹配组队”“副本”；MMORPG 夹具的组队 |
| 匹配成局 | match（分区、弹性人数） | game-demo“匹配组队”；MOBA、派对房间夹具 |
| 房间内同步 | 组同步 sync_groups | 派对房间夹具 |
| 附近的人、视野 | Interest 视野 + AOI + 同步视图与 LOD | game-demo“兴趣”“同步视图” |
| 帧同步对战 | Lockstep | MOBA 夹具；game-demo“lockstep 战斗” |
| 回合制对战 | 房间实体 + 多实体事务 + 持久回合定时器 | 回合 PvP 夹具；卡牌 RPG 夹具的回合定时器 |
| 断线重连 | 自身视图重连全量、组同步保留成员关系、Lockstep 追帧或快照恢复；座位与连接代际接管需手写业务 | 派对房间、自走棋、回合 PvP 夹具；[KNOWN-LIMITS“同一 SessionID 的重连”](../maintenance/KNOWN-LIMITS.md#同一-sessionid-的重连) |
| 排行榜 | rank 服务 | game-demo“排行榜” |
| 全服邮件、补偿邮件 | mail 服务的广播邮件；GM 命令发单人补偿 | game-demo“邮件”“GM 运维面” |
| 聊天 | chat 服务；频道策略是协作者，需手写 | game-demo“聊天” |
| 公会、联盟跨进程 / 跨服 | Remote 托管实体 + 路由 RPC + Mirror + Saga | game-demo“公会 = alliance 服务”（联盟服务方案实施中）；SLG 夹具的联盟活动联合链路 |
| 定时活动、跨服活动 | activity 服务 + 实体持久定时器（准点动作）；活动规则需手写业务 | SLG 夹具的联盟活动 |
| 登录、账号、选角 | account 服务 + 嵌入 player TCP 的 `auth.go`；渠道验签需手写 | game-demo“登录与凭据” |
| 角色名、公会名全服唯一 | directory 服务的两阶段占名；敏感词需手写业务 | [Service §4](guide/09-services.md#4-关键业务边界) |
| 支付回调 | 业务 HTTP 路由 + platform 服务（回调换恰好一次发货）；渠道验签需手写 | 合成休闲夹具的签名广告回调（模拟） |
| GM 命令、热更配置 | GM / 运维命令（admin 命令表）；配置表与热更（`configdata.reload`） | game-demo“GM 运维面”；[配置 §5](guide/07-config.md#5-运维重载) |
| 技能、Buff 与怪物 AI | skill 与 combat + 属性；行为与 actionflow + AI + 移动 + 实体心跳 | game-demo“技能目录”“怪物”“移动”；MMORPG 夹具的战斗与 Buff |
| 关卡、波次 | level 组件，或房间实体 + 定时器推进 | 合作塔防夹具；`scripts/testdata/level` |
| 可编辑地形、建造 | terrain 底座 + 多实体事务 + 按命名空间分开计的同步 | game-demo“可编辑地形与建筑” |
| 合服、数据结构变更上线 | 合服（别名合服）；schema 版本 + 离线迁移 | [Player 迁移 §6](PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)；game-demo“schema 迁移与 sid 搬机” |
| 压测 | Robot；`scripts/perf/` 的同机对照脚本 | game-demo“机器人压测”；[Robot](ROBOT.md) |
| 好友、任务、成就、商城 | 需手写业务：用 Entity / DAO / Nest 事务与幂等账本，按需加 effect 或 mail | 无专门参考；SLG 手册列了商城等路线的源码入口 |
| 无缝大世界、FPS、状态同步的 MOBA / RTS | 不支持：无缝大世界只有 cellworld 原型，FPS 不建议，状态同步的 MOBA / RTS 暂缓 | 见“明确没有 / 不适合的”及其出处 |
