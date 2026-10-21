# 次核心：Codegen 与工程工具：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[设计与使用](../guide/12-codegen.md) · 用户文档：[入口](../../../codegen/README.md) · [命令与标记参考](../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md) · [项目生成器](../../../codegen/docs/PROJECT_GENERATOR.zh-CN.md)

## 如何阅读

Codegen根据业务定义生成重复的连接代码，让你少写DAO、消息调用和协议注册等样板。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`codegen`、`demo`、`robot`、`cmd/glsvet`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 生成物依赖最低 core v1.26.0（`minimumVersions.Core`），发布 manifest 与 tag 一致。
2. 全阶段成功后发布目标，标记不等于覆盖权限。
3. 重复生成无漂移，真实 tag 消费不使用本地 replace。

## 2A. 正式工程生成链路

[manifest.go](../../../codegen/internal/roost/manifest.go)定义Manifest、默认策略和minimumVersions；运行时最低Core固定v1.26.0；VersionSpec 只有 Core，kit/skill/service/codegen 键在解码前按名拒绝。renderProject在[render.go](../../../codegen/internal/roost/render.go)构建plannedFile集合，renderBootstrap/服务Mod构造/配置段/Makefile等由同一清单导出。render生成候选字节不是正式发布到用户目标目录，写入权限和全阶段发布由项目事务流程控制。

DAO/Entity/Nest/Protocol/ServiceRPC各有独立解析和golden测试，模板中出现Go函数文本不表示它是生成器运行时函数。修改生成形状应验证生成输出的行为，而非只断言某段字符串出现。

[pretag.sh](../../../scripts/pretag.sh)先确认版本、远端tag状态、无replace与干净树；go generate后再次检查，再检查release manifest，最后build/vet/tidy/test。远端查询失败不能解释成tag不存在。最低版本与发布版本分别更新，文档补丁不随意抬高minimum。

## 2B. 生成器共同实现

- **语法级 AST 解析，不做类型检查**：解析器基于 `go/ast`（`parser.ParseComments`），不引入 `go/types`，目标项目不必能编译、不必下载依赖。代价是拿不到跨包类型信息：嵌套 struct 只在同一 def 目录内按名字递归发现（`internal/dao/parse.go` 的第三遍扫描），类型分类靠语法形状（`classifyField`）。模板无法生成正确代码的形状（固定长度数组、`chan`、`*map[...]...`）在生成期拒绝，不能留给下游编译错误——`format.Source` 只保证语法合法。
- **内容哈希幂等**：所有生成器写文件都走 `internal/genutil.WriteIfChanged`，内容一致就不写、mtime 不动。
- **隔离提交**：`roost generate` / `project sync` / `deps` 把项目复制到同级临时目录（跳过 `.git` / `bin` / `dist` / `log` 与生成器临时目录），完成全部步骤与 `GOWORK=off go mod tidy` 后一次提交；提交计划去重并保存旧内容用于回滚，提交窗口内业务输入或目标文件被另一进程修改时拒绝覆盖。提交只认领生成器所有的文件（`planStagedProjectCommit`，project.go）：带 `Code generated` 头的文件、按目录归属的无头输出（`isGeneratedData`，generate.go：`configs/data/*.json`、`configs/table_template/*.csv`、`docs/generated/`）、协议生成物与首次出现的 Controller 骨架；新增一种不带头的生成输出时必须同时登记到 `isGeneratedData`，否则暂存树里写出、日志里打印了路径，提交时却被丢掉（`configs/table_template` 曾如此）。
- **`--check`**：同一隔离机制下跑全部生成器，对含 `Code generated` 头的文件以及 `configs/data/*.json`、`configs/table_template/*.csv`、`docs/generated/` 做 SHA-256 快照比对，有差异报 `generated files are stale: ...`，工作区不被修改；比较前只把 CRLF 规范为 LF。
- **`-force`**：单生成器的 `-force` 跳过内容比较、无条件重写；`roost generate --force` 转发给每个支持它的生成器。tablegen 例外：它的输出是"已存在即拒绝覆盖"，编排层对 tablegen 总是传 `-force`；`--check` 内部不传 force。
- **golden 测试**：`internal/genutil.AssertGolden` 加各生成器的 `testdata/golden/` 把完整生成物锁成字节级基线，片段断言看不见的模板漂移 golden diff 一定看得见；有意修改模板后用 `-update` 刷新（如 `go test ./codegen/internal/dao -run TestGoldenGeneratedOutput -update`）。
- **配置声明快照**：生成器不导入运行时，Wiring Mod 的配置声明由 `wiring/internal/configschemagen` 快照成 [kitconfig_gen.go](../../../codegen/internal/roost/kitconfig_gen.go)。修改 Wiring 的配置声明后运行 `go generate ./codegen/internal/roost`，再跑 `TestKitConfigSchemasMatchKitDeclarations` 与 `TestDoctorReadsBusinessDeclarationsFromTheProcess`；遗漏快照会让配置检查与运行时声明不一致，doctor 还会把新增框架字段误计为业务字段。不能只测 Wiring 包或改业务字段数量断言来掩盖漂移。
- **跨服务配置规则写在声明上**（维护者 2026-10-19 批准）：[config_across_services.go](../../../codegen/internal/roost/config_across_services.go) 的 `checkConfigAcrossServices` 按 `configschema.Key` 的 `Shared` / `Follows` / `Listen`（声明 tag `shared` / `follows` / `listen`，随快照进入 `kitconfig_gen.go`）比较各服务配置，`roost config check --all`（[config_check.go](../../../codegen/internal/roost/config_check.go)，退出码 0～5 与 `--json` 报告也在这里）与 doctor 的 `config-across-services` 共用；不在生成器里另列键名。
- **服务配置段一份规则**：服务实际注册的 Mod 只由 [render.go](../../../codegen/internal/roost/render.go) 的 `serviceConfigMods`（共享 Mod 加 `effectiveServiceMods`，按依赖解析）给出，它写进配置的段只由 `serviceConfigSections` 给出，文件里“有没有这一段”只由 `missingConfigBlocks` 判断（看行首 `<key>:`）。`renderServiceConfig`（新工程）、`appendModConfigSections`（add 让服务多出 Mod）与 doctor 的只读检查 `config-sections:<service>`（[config_schema_doctor.go](../../../codegen/internal/roost/config_schema_doctor.go) `checkConfigSections`，维护者 2026-10-17 决定 B）共用这三处；Secret 示例内嵌配置的定位只在 `kubernetesSecretEmbeddedConfig`。新增“生成器给某类服务写某段”的规则改这里，不在 doctor 里另写一份。回归 `TestDoctorNamesTheConfigSectionsAManifestAddedModLacks`、`TestFreshlyGeneratedConfigsHaveEveryModSection`。
- **服务包名跟随目录**（D8，维护者 2026-10-16 决定 C）：`internal/service/<name>/` 里业务文件（service.go、managers.go、collaborators.go）与生成文件（framework_clients_gen.go）必须同一个包名。[service_package.go](../../../codegen/internal/roost/service_package.go) 的 `withServicePackages(root)` 在每次 render（sync / diff / generate 的提交计划）前读出目录现有包名，优先取不带生成头的业务文件；目录还不存在时用 `defaultServicePackage`：去掉 `_` / `-`、全小写，Go 关键字加后缀 `service`。新工程因此是 `package game`，老工程的 `package Game` 保持不变、不被改写，也不会出现双包名。回归 `TestServicePackageNamesAreLowercaseAndFollowAnExistingProject`。
- **DAO 逻辑库名与 EntityID 换算**（D9：2026-10-16 方案 B，2026-10-20 修订版 A）：`roost add dao` 骨架写逻辑库 `db=main`（[add_entity.go](../../../codegen/internal/roost/add_entity.go) 的 `defaultDAODatabase`），game-demo 的 Player / Guild / World 同样写 `db=main`；物理库名是部署配置，生成配置的 `dataengine.databases: {main: <project_db>}` 由 Mod 声明的 example 经 `projectNameVars` 展开，`<project_db>` 仍是 `daoDatabaseName(project.name)`（系统库加 `_data`，52 字节截断）。DAO 生成器（[codegen/internal/dao/databases.go](../../../codegen/internal/dao/databases.go)）解析时校验 `db=`（规则与运行期 `dataengine.ValidLogicalDatabaseName` 相同；生成器不得 import 运行时，所以写了一份，不一致时生成物 `init` 里的 `DeclareDatabases` 会 panic），去重后生成 `gen_databases.go`（每个逻辑库一个 `Database<Name>` 常量、`init` 里 `DeclareDatabases`），`<Type>DBName` 引用该常量；没有持久 DAO 时按孤儿删除。`roost id entity`（[id_entity.go](../../../codegen/internal/roost/id_entity.go)）持有 `framework/entity/idgen.go` 位布局的副本，`TestRoostIDEntityLayoutMatchesTheFramework` 读那份源码的常量比对；category 只存低两位，解码给出这两位与工程标记里声明的分类。回归 `TestRoostIDEntityConvertsBothWays`、`TestDAOSkeletonDatabaseFollowsTheProjectName`（骨架 `db=main` 与生成配置的映射）、dao golden `gen_databases.go`。
- **旧写法不迁移**：项目未上线，生成器对已退役写法（清单见[项目生成器 §4](../../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#4-roostyaml)）只点名拒绝并给出手工改法，不读取、不自动迁移、不静默忽略。

## 2C. roost add timer

DESIGN-TODO D11（维护者 2026-10-17 决定方案 B）。[add_timer.go](../../../codegen/internal/roost/add_timer.go) 一处实现 add、清单校验与渲染；运行期在 core 的 `framework/nest/entitytimer`（[impl/10-time §2A](10-time.md#2a-按实体的持久定时器驱动)）。

- **清单是唯一声明**：`timers.<entity>`（`TimerSpec`：component、id、service、dao）。`renderProject` 调 `renderTimerFiles` 按它渲染生成部分（节点类型、组件、Tick handler 及其测试；带生成头，sync 重写）与业务回调文件（`<component>_handlers.go`，`Owned=false`，只在缺失时创建）；`serviceModConstructors` 给 `spec.Service` 追加 `kitentitytimer.NewMod(EntityAccess, <entity>.<Component>Driver...)`，实体包 import 与 self_sync 同一别名（实体 snake 名）。所有名字只由 `newTimerNames` 从清单推出，渲染与 add 共用。
- **两处业务文件只在 add 时改一次**：DAO 定义加节点 map 与种子（文本插在结构体右括号前，保留业务注释；同名同类型已存在时不改、类型不同拒绝）；`entity.go` 加组件字段（已有同一组件字段时不改）。Go 结构体不能跨文件，所以 DAO 字段不能放进 `_gen.go`；节点类型可以，放在 `db/def/<entity>_<component>_node_gen.go`。先改这两处、再写清单并 sync（`commitManifestSync`），sync 失败时两处一并还原（`restoreFilesIfCurrent`）。
- **拒绝在改文件之前**：实体事实由 `entity.InspectTimerHost`（codegen/internal/entity）读：`remote=managed`、`noPersist=true` 拒绝并说明原因；DAO 只有一个，或几个里取字段 `dao`；组件名被别的组件占用（包里已声明 `CompType<Component>` 常量，或字段类型不是 `*<Component>Component`）拒绝；服务取 `--service` 或唯一装了 nest 的业务服务。重复 add 按清单拒绝。
- **handler 名对齐**：驱动投递的名字是常量 `<Component>TickHandler = "handlerTick<Entity><Component>"`，Nest 生成器按函数名得出同一个名字；生成的 `tick_<entity>_<component>_gen_test.go` 在生成工程里比较两者（`handlerName…` 由 Nest 生成器写出），任何一边改名都会让工程测试变红。`roost add handler --component <timer 组件>` 认 `<component>_component_gen.go`。
- **game-demo 不用这个生成器**（维护者 2026-10-18：game-demo 是手写的参考实现，不迁到生成器产物上；生成器只生成框架内容，不生成 demo）。`5581deeb` 曾把 World 迁到 `add timer`，现已恢复手写：demo 步骤写 `game/entities/world/timer_component.go`（组件、Store 适配、`TimerDriver`、回调）与 `game/handler/tick_world_timers.go`（`add handler TickWorldTimers` 后覆盖），驱动由 game 服务 `activity.go` 的 `startWorldTimers` 在 `Service.Init` 加载 World 之前手工接上业务时钟与 Nest 并启动、`Shutdown` 里停止，不经 `wiring/entitytimer` Mod、清单里没有 `timers.world`。handler 名对齐由 demo 的 `tick_world_timers_test.go` 守。game 服务少一个 Mod。当前缺省 Mod 集 26 个、`total_timeout` 120s；`shutdown_test.go` 的夹具 `-mods configdata,mongo,nats,dataengine,nest` 25 个、117s（D15 起承载玩家且接了 account 的服务多一个合服启动自检 `svcaccount.NewServingGuardMod()`）。
- **已知缺口**：去掉清单条目后生成部分不自动删除（与其他按清单渲染的文件相同，见 KNOWN-LIMITS）。

回归：[add_timer_test.go](../../../codegen/internal/roost/add_timer_test.go)（`TestAddTimerGeneratesOwnedPartsAndBusinessCallbacks`、`TestAddTimerComponentNameAndExplicitID`、`TestAddTimerRefusesWhatItCannotSupport`、`TestManifestRejectsAnInconsistentTimerEntry`、`TestEntityTimerGeneratedProjectEndToEnd`）。修前（`a56d5965`）`roost add timer --entity Player` 报 `invalid timer name ""`。

## 2D. roost add saga -native 与 roost add effect

DESIGN-TODO D12 方向 B（维护者 2026-10-18 批准）。运行期是方向 A 的 `wiring/saga.Steps`、`framework/saga.NativeStep` 与 `wiring/dataengine.SubscribeEffects`（[guide/06 §2.1](../guide/06-saga.md#21-业务怎样写步骤步骤运行时design-todo-d12-方向-a)），生成器只写业务文件、不加清单字段。

- **[add_saga.go](../../../codegen/internal/roost/add_saga.go)**：`parseSagaSteps` 保序解析 `-steps` 与 `-native`（重复、`-native` 不在 `-steps` 里都报错）；`planSaga` 在写盘之前算出全部文件内容并校验（原生步骤的锁目标走 `resolveNestHandlerTarget`——与 `add handler` 同一函数；跑步骤的服务按 `resolveMods(effectiveServiceMods)` 要有 nest；`rejectExistingHandlers` 按 `func handler<Name>(` 扫描 `game/handler`；任何目标文件已存在都拒绝），`addSaga` 再 `commitSyncChanges` 写入、把 saga Mod 接到服务并 `commitManifestSync`（同步里的 generate 为新 handler 生成 Sender），同步失败用 `restoreFilesIfCurrent` 撤回。生成物：`definition.go`（不再有 `Subscribe*`）、`steps.go`（`renderSagaSteps`：`StepsGroup`、`StepDeps`、`State` 编解码、步骤体、`RegisterSteps`、`StartSteps`，容量说明写在文件注释）、每个原生步骤两个 handler（`renderSagaNativeHandler`）。骨架默认拒绝。
- **[add_effect.go](../../../codegen/internal/roost/add_effect.go)**：`parseEffectTopic`（点分、`saga.*` 保留）、消费服务按 `resolveMods` 要有 dataengine；effect 文件已存在且声明了 `Topic<Topic>` 与载荷类型时只建消费者。
- **归属**：都是业务文件、只建一次；服务 Init / Shutdown 不改写，`printSagaNext` / `printEffectNext` 给两行。
- **doctor**：[saga_effect_doctor.go](../../../codegen/internal/roost/saga_effect_doctor.go) 的 `checkStepAndEffectWiring`——只读、文本判据（源码先 `stripGoComments`，骨架注释里的用法不算调用）：`saga-steps:<name>`（`steps.go` 有 `StartSteps` / `RegisterSteps` 时，找导入该 saga 包并调用 `.StartSteps(` / `.RegisterSteps(` 的非测试文件，再看它所在包有没有带参数的 `.Drain(`）与 `effect-consumer:<service>/<func>`（`start<Topic>EffectConsumer` 在服务包里的出现次数多于声明）。只说明写了，不证明运行时走到；game-demo 的 `steps.go` 没有这两个函数，不报。
- **game-demo**：送礼 saga 保留手写接线，demo 模板的 `saga/gift_item/steps.go` 覆盖生成的骨架、只留说明（demo 步骤表里紧跟 `add saga GiftItem` 的 `write`）。

回归：[add_saga_test.go](../../../codegen/internal/roost/add_saga_test.go)（生成物与登记顺序、错误输入工程逐字节不变、服务无 nest、无 `-native` 的 Mongo 登记、doctor 两项、生成工程 build / vet / doctor，以及真实依赖 `TestGeneratedSagaNativeStepsAndEffectConsumer`：`testdata/saga_native` 覆盖最小业务，生成工程里的 `TestSagaNativeStepsAndEffectRealDependencies` 在真实 Mongo / NATS / DataEngine / Nest 上核对一笔正向、一笔补偿、一条 effect 恰好一次，入口 `scripts/test-demo-generated.sh`）、[add_effect_test.go](../../../codegen/internal/roost/add_effect_test.go)。修前（`5afd19fc`）`add saga -native` 报 `flag provided but not defined: -native`，`add effect` 报 `flag provided but not defined: -consumer`。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### cmd/glsvet

5 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [clockhints.go](../../../cmd/glsvet/clockhints.go) | 函数/方法或内部实现；见源码 |
| [componentfields.go](../../../cmd/glsvet/componentfields.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../cmd/glsvet/main.go) | 函数/方法或内部实现；见源码 |
| [stophints.go](../../../cmd/glsvet/stophints.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/attribute

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/attribute/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/cfggen

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/cfggen/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/dao

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/dao/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/entity

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/entity/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/errcode

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/errcode/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/nest

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/nest/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/project

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/project/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/protocol

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/protocol/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/roost

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/roost/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/servicerpc

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/servicerpc/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/tablegen

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/tablegen/main.go) | 函数/方法或内部实现；见源码 |

### codegen/cmd/webroute

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/webroute/main.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/attribute

5 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/attribute/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/attribute/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/attribute/parse.go) | 函数/方法或内部实现；见源码 |
| [types.go](../../../codegen/internal/attribute/types.go) | `ProfileDef`、`AttributeField`、`FormulaDef` |
| [util.go](../../../codegen/internal/attribute/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/cfggen

1 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/cfggen/main.go) | `Meta`、`GroupsMeta`、`GroupList`、`BeanMeta`、`TableMeta`、`FieldMeta` |

### codegen/internal/dao

8 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/dao/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/dao/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/dao/parse.go) | `Definitions`、`DaoDef`、`RedisDaoDef`、`NestedDef`、`FieldDef`、`FieldKind`、`DaoTag` |
| [template_dao.go](../../../codegen/internal/dao/template_dao.go) | 函数/方法或内部实现；见源码 |
| [template_helpers.go](../../../codegen/internal/dao/template_helpers.go) | 函数/方法或内部实现；见源码 |
| [template_nested.go](../../../codegen/internal/dao/template_nested.go) | 函数/方法或内部实现；见源码 |
| [template_redis.go](../../../codegen/internal/dao/template_redis.go) | 函数/方法或内部实现；见源码 |
| [util.go](../../../codegen/internal/dao/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/entity

8 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/entity/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/entity/main.go) | 函数/方法或内部实现；见源码 |
| [mirror.go](../../../codegen/internal/entity/mirror.go) | `MirrorDef` |
| [nocoll_dao.go](../../../codegen/internal/entity/nocoll_dao.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/entity/parse.go) | `EntityDef`、`ImportDef`、`ComponentField`、`DaoField` |
| [remote_dao_scope.go](../../../codegen/internal/entity/remote_dao_scope.go) | 函数/方法或内部实现；见源码 |
| [run.go](../../../codegen/internal/entity/run.go) | 函数/方法或内部实现；见源码 |
| [self_sync_packer.go](../../../codegen/internal/entity/self_sync_packer.go) | `SelfSyncPlan`、`InspectSelfSync`（`-self-sync`：access.player.self_sync 的标记缺省与默认 packer `<snake>_sync_packer_gen.go` 的生成 / 删除） |
| [util.go](../../../codegen/internal/entity/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/errcode

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/errcode/main.go) | `Definition` |

### codegen/internal/genutil

2 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [golden.go](../../../codegen/internal/genutil/golden.go) | 函数/方法或内部实现；见源码 |
| [write.go](../../../codegen/internal/genutil/write.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/marker

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [marker.go](../../../codegen/internal/marker/marker.go) | 函数/方法或内部实现；见源码 |
| [options.go](../../../codegen/internal/marker/options.go) | `Spec` |

### codegen/internal/nest

5 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bootstrap.go](../../../codegen/internal/nest/bootstrap.go) | 函数/方法或内部实现；见源码 |
| [gen.go](../../../codegen/internal/nest/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/nest/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/nest/parse.go) | `FuncInfo`、`RemoteAccessInfo`、`EntityParam`、`NonEntityParam`、`RetParam`、`ErrorRetParam`、`ParseResult`、`ImportInfo` |
| [run.go](../../../codegen/internal/nest/run.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/project

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [module.go](../../../codegen/internal/project/module.go) | `Info` |

### codegen/internal/protocol

10 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [classify.go](../../../codegen/internal/protocol/classify.go) | 函数/方法或内部实现；见源码 |
| [csharp.go](../../../codegen/internal/protocol/csharp.go) | 函数/方法或内部实现；见源码 |
| [gen.go](../../../codegen/internal/protocol/gen.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/protocol/parse.go) | 函数/方法或内部实现；见源码 |
| [retire.go](../../../codegen/internal/protocol/retire.go) | 函数/方法或内部实现；见源码 |
| [reverse.go](../../../codegen/internal/protocol/reverse.go) | 函数/方法或内部实现；见源码 |
| [run.go](../../../codegen/internal/protocol/run.go) | 函数/方法或内部实现；见源码 |
| [types.go](../../../codegen/internal/protocol/types.go) | `Definitions`、`EnumDef`、`EnumValueDef`、`StructDef`、`MsgDef`、`PushDef`、`FieldDef`、`FieldKind`、`ScalarKind` |
| [util.go](../../../codegen/internal/protocol/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/registry

3 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/registry/gen.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/registry/parse.go) | `Registration` |
| [run.go](../../../codegen/internal/registry/run.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/roost

47 个实现文件、31 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [activity_groups.go](../../../codegen/internal/roost/activity_groups.go) | 函数/方法或内部实现；见源码 |
| [add.go](../../../codegen/internal/roost/add.go) | `AddOptions` |
| [add_effect.go](../../../codegen/internal/roost/add_effect.go) | 函数/方法或内部实现；见源码（`roost add effect`，§2D） |
| [add_entity.go](../../../codegen/internal/roost/add_entity.go) | `I` |
| [add_rpc.go](../../../codegen/internal/roost/add_rpc.go) | `Service`、`Mod` |
| [add_saga.go](../../../codegen/internal/roost/add_saga.go) | 函数/方法或内部实现；见源码（`roost add saga`、`-native`，§2D） |
| [add_skill.go](../../../codegen/internal/roost/add_skill.go) | `Catalog` |
| [add_workflow.go](../../../codegen/internal/roost/add_workflow.go) | `Controller` |
| [attribute_runtime.go](../../../codegen/internal/roost/attribute_runtime.go) | 函数/方法或内部实现；见源码 |
| [catalog.go](../../../codegen/internal/roost/catalog.go) | 函数/方法或内部实现；见源码 |
| [cli.go](../../../codegen/internal/roost/cli.go) | 函数/方法或内部实现；见源码 |
| [command_tree.go](../../../codegen/internal/roost/command_tree.go) | 函数/方法或内部实现；见源码 |
| [command_tree_other.go](../../../codegen/internal/roost/command_tree_other.go) | 函数/方法或内部实现；见源码 |
| [command_tree_unix.go](../../../codegen/internal/roost/command_tree_unix.go) | 函数/方法或内部实现；见源码 |
| [command_tree_windows.go](../../../codegen/internal/roost/command_tree_windows.go) | 函数/方法或内部实现；见源码 |
| [config_schema_doctor.go](../../../codegen/internal/roost/config_schema_doctor.go) | 函数/方法或内部实现；见源码 |
| [demo.go](../../../codegen/internal/roost/demo.go) | 函数/方法或内部实现；见源码 |
| [dependencies.go](../../../codegen/internal/roost/dependencies.go) | 函数/方法或内部实现；见源码 |
| [doctor.go](../../../codegen/internal/roost/doctor.go) | `CheckStatus`、`CheckItem`、`DoctorReport`、`DoctorOptions` |
| [format.go](../../../codegen/internal/roost/format.go) | 函数/方法或内部实现；见源码 |
| [framework_release.go](../../../codegen/internal/roost/framework_release.go) | `FrameworkReleaseManifest`、`FrameworkModuleLock`、`FrameworkReleaseLock` |
| [framework_services.go](../../../codegen/internal/roost/framework_services.go) | `Service` |
| [generate.go](../../../codegen/internal/roost/generate.go) | `GenerateOptions` |
| [generated_header.go](../../../codegen/internal/roost/generated_header.go) | 函数/方法或内部实现；见源码 |
| [help.go](../../../codegen/internal/roost/help.go) | `PlayerDao`、`Player`、`GameProtocol`、`Monster`、`PlayerProfile` |
| [id.go](../../../codegen/internal/roost/id.go) | `IDUse` |
| [interrupt.go](../../../codegen/internal/roost/interrupt.go) | 函数/方法或内部实现；见源码 |
| [kitconfig_gen.go](../../../codegen/internal/roost/kitconfig_gen.go) | 函数/方法或内部实现；见源码 |
| [logic_offset_doctor.go](../../../codegen/internal/roost/logic_offset_doctor.go) | 函数/方法或内部实现；见源码 |
| [manifest.go](../../../codegen/internal/roost/manifest.go) | `Manifest`、`ProjectSpec`、`VersionSpec`、`CICDSpec`、`ServiceSpec`、`AccessSpec`、`IDSpace`、`IDRange` |
| [next_optional.go](../../../codegen/internal/roost/next_optional.go) | 函数/方法或内部实现；见源码 |
| [player_tcp_config.go](../../../codegen/internal/roost/player_tcp_config.go) | 函数/方法或内部实现；见源码 |
| [project.go](../../../codegen/internal/roost/project.go) | `NewOptions`、`SyncResult` |
| [render.go](../../../codegen/internal/roost/render.go) | `SkillProgram`、`SkillProgram`、`ServiceMetrics`、`Service` |
| [render_access.go](../../../codegen/internal/roost/render_access.go) | `Context`、`Response`、`Decoder`、`Encoder`、`HandlerFunc`、`Middleware`、`ProtocolDef`、`ProtocolRegistry`、`WriteGate`、`Runtime`、`ProtocolRegistrar`、`Mod` |
| [render_cicd.go](../../../codegen/internal/roost/render_cicd.go) | 函数/方法或内部实现；见源码 |
| [render_deploy.go](../../../codegen/internal/roost/render_deploy.go) | 函数/方法或内部实现；见源码 |
| [render_dev_run.go](../../../codegen/internal/roost/render_dev_run.go) | 函数/方法或内部实现；见源码 |
| [render_docs.go](../../../codegen/internal/roost/render_docs.go) | 函数/方法或内部实现；见源码 |
| [render_player_tcp.go](../../../codegen/internal/roost/render_player_tcp.go) | `Config`、`Authenticator`、`RegistryBound`、`AuthenticatorFunc`、`Mod`、`Runtime`、`SessionClosed`、`Server` |
| [render_workflow_docs.go](../../../codegen/internal/roost/render_workflow_docs.go) | 函数/方法或内部实现；见源码 |
| [saga_effect_doctor.go](../../../codegen/internal/roost/saga_effect_doctor.go) | 函数/方法或内部实现；见源码（doctor 的 `saga-steps:` / `effect-consumer:`，§2D） |
| [self_sync.go](../../../codegen/internal/roost/self_sync.go) | 函数/方法或内部实现；见源码（`access.player.self_sync` 的接线渲染与 generate / doctor 前置检查；默认 packer 由 entity 生成器 `-self-sync Player` 写出） |
| [shutdown_budget.go](../../../codegen/internal/roost/shutdown_budget.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/roost/cmd/attributeruntime

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/roost/cmd/attributeruntime/main.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/servicerpc

6 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/servicerpc/gen.go) | `File`、`Half`、`Options` |
| [parse.go](../../../codegen/internal/servicerpc/parse.go) | `Service`、`Method`、`Field` |
| [run.go](../../../codegen/internal/servicerpc/run.go) | 函数/方法或内部实现；见源码 |
| [template.go](../../../codegen/internal/servicerpc/template.go) | `BusClient`、`Server`、`ClientMod` |
| [validate.go](../../../codegen/internal/servicerpc/validate.go) | 函数/方法或内部实现；见源码 |
| [wiresafe.go](../../../codegen/internal/servicerpc/wiresafe.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/tablegen

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/tablegen/main.go) | `TableKind`、`Meta`、`Field` |

### codegen/internal/webroute

3 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/webroute/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/webroute/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/webroute/parse.go) | `Route` |

### demo

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [embed.go](../../../demo/embed.go) | 函数/方法或内部实现；见源码 |

### robot/action

4 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [action.go](../../../robot/action/action.go) | `Action`、`Func`、`Registry` |
| [builtin.go](../../../robot/action/builtin.go) | 函数/方法或内部实现；见源码 |
| [call.go](../../../robot/action/call.go) | `CallOption` |
| [params.go](../../../robot/action/params.go) | `Params` |

### robot

4 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [blackboard.go](../../../robot/blackboard.go) | `Blackboard` |
| [dialers.go](../../../robot/dialers.go) | `KCPDialerConfig`、`QUICDialerConfig` |
| [lockstep.go](../../../robot/lockstep.go) | `LockstepSink`、`LockstepBotConfig`、`LockstepBotStats`、`LockstepBot`、`FrameHasher` |
| [robot.go](../../../robot/robot.go) | `ActionRunner`、`AuthProvider`、`Config`、`Context`、`TypedKey`、`Coalescer`、`BoundedQueue` |

### robot/loadtest

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../robot/loadtest/admin.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../robot/loadtest/manager.go) | `RunState`、`StopReason`、`Runner`、`RunnerFactory`、`Threshold`、`Profile`、`Config`、`StartRequest`、`StopRequest`、`StatusRequest`、`HistoryRequest`、`ReportRequest`、`ProfilesResult`、`ProfileSummary`、`StatusResult`、`HistoryResult`、`StopResult`、`ThresholdResult`、`RunSnapshot`、`StatsSnapshot`、`Manager` |

### robot/protocol

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [registry.go](../../../robot/protocol/registry.go) | `Encoder`、`Decoder`、`Codec`、`CodecFuncs`、`JSONCodec`、`Registry` |

### robot/runner

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [runner.go](../../../robot/runner/runner.go) | `Executor`、`Ramp`、`Stage`、`IdentityProvider`、`Config`、`Stats`、`Runner`、`Option` |

### robot/scenario

2 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [scenario.go](../../../robot/scenario/scenario.go) | `Scenario`、`Node`、`NodeFunc`、`Registry`、`WeightedNode` |
| [spec.go](../../../robot/scenario/spec.go) | `Spec`、`ScenarioSpec`、`NodeSpec`、`LoopSpec`、`RetrySpec`、`TimeoutSpec`、`WeightedSpec` |

### robot/session

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [session.go](../../../robot/session/session.go) | `Message`、`PushFilter`、`PushHandler`、`Session` |

### robot/transport

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [transport.go](../../../robot/transport/transport.go) | `Packet`、`Conn`、`Dialer`、`Config`、`TCPConn`、`WebSocketConn` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Generate`、`Test.*Protocol`、`Test.*Project`、`Test.*Minimum`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### cmd/glsvet

- [clockhints_test.go](../../../cmd/glsvet/clockhints_test.go)：`TestClockHintsFlagDirectReadsInABusinessPackage`、`TestClockHintsHonourTheSystemClockDirective`、`TestClockHintsLeaveOtherPackagesAlone`、`TestClockHintsLookOnlyBelowTheModuleRoot`、`TestClockHintsAreNotFindings`
- [componentfields_test.go](../../../cmd/glsvet/componentfields_test.go)：`TestComponentFieldWritesOutsideTheDaoAreHinted`、`TestSkillPackagesGetNoComponentFieldHint`、`TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix`
- [inputs_test.go](../../../cmd/glsvet/inputs_test.go)：`TestMain`、`TestMissingDirectoryArgumentFails`、`TestUnparsableFileFailsInsteadOfSkippingTheDirectory`、`TestDirectoryWithoutGoFilesStillPasses`
- [main_test.go](../../../cmd/glsvet/main_test.go)：`TestHandlerRawGoroutineIsRejected`、`TestHandlerNamedGoroutineWrapperIsRejected`、`TestHandlerErrgroupGoIsRejected`、`TestHandlerCoreWorkerPoolGoIsAllowed`、`TestHandlerCoreWorkerPoolFieldGoIsAllowed`；其余 13 项见文件
- [stophints_test.go](../../../cmd/glsvet/stophints_test.go)：`TestStopHintsFlagABareReceiveInAStopWithContext`、`TestStopHintsFollowOneCallIntoAHelperThatCannotSeeTheContext`、`TestStopHintsAcceptBoundedWaits`、`TestStopHintsDoNotCountAsFindings`

### codegen/internal/attribute

- [retirement_test.go](../../../codegen/internal/attribute/retirement_test.go)：`TestRunRetiresOnlyOwnedProfileAfterLastMarkerIsRemoved`
- [run_test.go](../../../codegen/internal/attribute/run_test.go)：`TestRunGeneratesProfile`
- [validation_test.go](../../../codegen/internal/attribute/validation_test.go)：`TestParseDirRejectsEachProfileViolation`、`TestParseDirRejectsAttributesTheWireCannotCarry`、`TestParsePositiveIntRefusesNonPositiveValues`

### codegen/internal/cfggen

- [globals_test.go](../../../codegen/internal/cfggen/globals_test.go)：`TestCfggenGlobalRulesBecomeObjectDefRules`、`TestCfggenGlobalWithoutRulesKeepsItsRegistration`、`TestCfggenGlobalRulesStillRejectWhatAnObjectCannotMean`
- [groups_test.go](../../../codegen/internal/cfggen/groups_test.go)：`TestCfggenExportsOnlyTheTargetGroups`、`TestCfggenGroupsFlagSelectsAnotherTarget`、`TestCfggenWithoutGroupsExportsEverything`、`TestCfggenRefusesEachInvalidGroupUse`、`TestExportRefusesABeanWithEveryFieldExcluded`
- [main_test.go](../../../codegen/internal/cfggen/main_test.go)：`TestCfggenGeneratesStructsRegistrationAndAccessors`、`TestCfggenRejectsTheRetiredObjectsAlias`、`TestCfggenRejectsBrokenMetaForTheStatedReason`、`TestCfggenIndexFalseDisablesIndex`、`TestCfggenKeywordFieldNamesGenerateCompilableParams`；其余 9 项见文件

### codegen/internal/dao

- [failfast_test.go](../../../codegen/internal/dao/failfast_test.go)：`TestParseRejectsOrphanMarker`、`TestParseRejectsMarkerBindingMultipleStructs`、`TestRunSweepsOrphansWhenAllDefinitionsRemoved`、`TestParseAcceptsMarkerAboveDocComment`、`TestParseRejectsMarkerWithoutParameters`；其余 8 项见文件
- [fields_test.go](../../../codegen/internal/dao/fields_test.go)：`TestAFieldWhoseLowercaseNameIsAKeywordGenerates`、`TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates`、`TestNestedStructsPersistTheirFieldsNotTheDirtyHook`、`TestTheDaoDocumentCarriesNestedWireFormsNotMarshalers`、`TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync`
- [golden_test.go](../../../codegen/internal/dao/golden_test.go)：`TestGoldenGeneratedOutput`
- [nocollection_test.go](../../../codegen/internal/dao/nocollection_test.go)：`TestANoCollectionDaoKeepsSyncAndDropsEveryStoragePath`、`TestANoCollectionDaoWithPersistentFieldsIsRefusedByName`、`TestTheNoCollectionMarkerRefusesContradictions`、`TestACollectionDaoIsUnchangedByTheNoCollectionFlag`、`TestSwitchingADaoToNoCollectionRetiresItsStoragePaths`
- [parse_test.go](../../../codegen/internal/dao/parse_test.go)：`TestParseDefDir`、`TestGenerateDao`、`TestGenerateNested`、`TestGenerateNestedPointerGetterDoesNotAddPointerLevel`、`TestFieldVarNamePreservesInitialisms`；其余 4 项见文件
- [schema_version_test.go](../../../codegen/internal/dao/schema_version_test.go)：`TestDaoSchemaVersionComesFromTheMarker`、`TestDaoSchemaVersionDefaultsToOne`、`TestDaoSchemaVersionRefusesNonsense`

### codegen/internal/entity

- [category_contract_test.go](../../../codegen/internal/entity/category_contract_test.go)：`TestGeneratedWiringImportsEveryPackageItQualifies`、`TestEntityMarkerCategoryIsGeneratedDirectly`、`TestEntityMarkerWithoutCategoryKeepsTheRuntimeLookup`、`TestEntityMarkerRefusesAMalformedCategory`
- [marker_test.go](../../../codegen/internal/entity/marker_test.go)：`TestParseDirRejectsMarkerNotAttachedToStruct`、`TestParseDirRejectsMarkerAboveNonStruct`、`TestParseDirRejectsUnknownMarkerParameters`、`TestParseDirRejectsInvalidMarkerValues`、`TestParseDirAcceptsEveryDocumentedMarkerForm`；其余 3 项见文件
- [mirror_test.go](../../../codegen/internal/entity/mirror_test.go)：`TestRemoteMirrorEntityMarkerIsAMigrationError`、`TestMirrorDTOGeneratesReadOnlyView`、`TestMirrorDTOOnlyPackageIsDiscoveredAndRetired`、`TestMirrorMarkerValidation`
- [nocollection_test.go](../../../codegen/internal/entity/nocollection_test.go)：`TestANoPersistEntityWiresANoCollectionDaoByItsRegistryKey`、`TestACollectionDaoIsStillWiredByItsCollection`、`TestAStoredEntityCannotUseANoCollectionDao`
- [parse_test.go](../../../codegen/internal/entity/parse_test.go)：`TestParseDir`、`TestGenerate`、`TestGenerateRemoteManagedV2Participant`、`TestGeneratePreservesManualSyncMethods`、`TestToSnake`
- [registration_test.go](../../../codegen/internal/entity/registration_test.go)：`TestGenerateGivesEachEntityInAPackageDistinctRegistrationSymbols`、`TestGeneratedBusinessPoolRegistration`
- [remote_contract_test.go](../../../codegen/internal/entity/remote_contract_test.go)：`TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement`、`TestRemoteManagedEntityRejectsServerScopedDao`、`TestGenerateEmitsAndReconcilesRemoteGuardTests`
- [retirement_test.go](../../../codegen/internal/entity/retirement_test.go)：`TestRunRetiresEntityWireAfterLastMarkerRemoved`、`TestRunMovesRegistrationWhenFirstSiblingIsRetired`
- [run_test.go](../../../codegen/internal/entity/run_test.go)：`TestRunReturnsFlagErrors`
- [sync_contract_test.go](../../../codegen/internal/entity/sync_contract_test.go)：`TestSyncNamespaceRejectsABareIdentifier`、`TestSyncNamespaceAcceptsLiteralsAndQualifiedConstants`、`TestGeneratedSyncBlockOnlyNamesFieldsCoreHas`、`TestRetiredPackerMarkerIsRefused`、`TestPackerWithoutSyncIsRefused`、`TestSelfSyncDefaultPackerFollowsTheMarker`（开关开 / 关、写 / 不写 subjectPacker、业务同名文件、拒绝项）

### codegen/internal/errcode

- [export_test.go](../../../codegen/internal/errcode/export_test.go)：`TestExtractDefinitionsAndWriteCSV`
- [scan_test.go](../../../codegen/internal/errcode/scan_test.go)：`TestExtractDefinitionsIgnoresCommentAndStringLiterals`、`TestExtractDefinitionsRefusesWhatItCannotRead`

### codegen/internal/marker

- [marker_test.go](../../../codegen/internal/marker/marker_test.go)：`TestOnlyTheRoostPrefixIsRead`、`TestRegexpIsAnchoredAndKindIsLiteral`、`TestFindRetiredPrefixReportsProductionSourcesOnly`
- [typo_guard_test.go](../../../codegen/internal/marker/typo_guard_test.go)：`TestEveryMarkerRefusesAMisspeltKey`、`TestEveryMarkerKindHasASpecAndAGuardCase`

### codegen/internal/nest

- [generated_signatures_test.go](../../../codegen/internal/nest/generated_signatures_test.go)：`TestABlankParameterNameGeneratesUsableCode`、`TestSenderRefusesATypeDeclaredInTheHandlerPackage`、`TestSenderRefusesARemoteRequestStructWithoutSuggestingAMove`、`TestHandlerSideGenerationDoesNotImportReturnOnlyPackages`
- [handler_contract_test.go](../../../codegen/internal/nest/handler_contract_test.go)：`TestParseFileRefusesValueReceiversAndBadTargetDeclarations`、`TestParseFileRejectsDuplicateRemoteAliasesWithinAndAcrossFiles`、`TestModuleDiscoveryRejectsBrokenOrMissingGoMod`、`TestGeneratePipelinedHandlerMeta`、`TestParseFileRejectsRemainingRemoteTagViolations`；其余 1 项见文件
- [nest_test.go](../../../codegen/internal/nest/nest_test.go)：`TestParseFile`、`TestValidateTransactionOptionsRejectsLegacyDirty`、`TestParseFileReadsRemoteRequestTagsFromPackageFiles`、`TestGenerate`、`TestGenerateMultipleNonErrorReturns`；其余 7 项见文件
- [retirement_test.go](../../../codegen/internal/nest/retirement_test.go)：`TestRunRetiresNestOutputsAfterLastMarkerRemoved`、`TestRunWithoutSenderRetiresOnlySenderOutputs`
- [run_test.go](../../../codegen/internal/nest/run_test.go)：`TestRunReturnsFlagErrors`
- [sender_guard_test.go](../../../codegen/internal/nest/sender_guard_test.go)：`TestGenerateSenderGuardTest`

### codegen/internal/project

- [module_test.go](../../../codegen/internal/project/module_test.go)：`TestDiscoverDerivesImportAndPackage`、`TestDiscoverRejectsDirectoryOutsideModule`、`TestPackageNameFallsBackToGeneratedFiles`、`TestPackageNameRefusesADirectoryWithTwoPackages`

### codegen/internal/protocol

- [controllers_test.go](../../../codegen/internal/protocol/controllers_test.go)：`TestControllerScaffoldsPreserveBusinessAndFollowMethods`、`TestControllerScaffoldRefusesFilenameConflict`
- [csharp_test.go](../../../codegen/internal/protocol/csharp_test.go)：`TestCSharpCLIIDsAndRetirement`
- [golden_test.go](../../../codegen/internal/protocol/golden_test.go)：`TestGoldenProtocolOutput`
- [protocol_test.go](../../../codegen/internal/protocol/protocol_test.go)：`TestParseAndGenerateProtocol`、`TestRunDiscoversTargetModule`、`TestDuplicateMessageIDRejected`
- [retirement_test.go](../../../codegen/internal/protocol/retirement_test.go)：`TestRunRetiresOutputsWhenLastDefinitionIsRemoved`、`TestRunRetiresHandlersButKeepsEmptyBootstrap`、`TestRunPreservesUnrecognizedFilesWhenDefinitionsAreEmpty`、`TestRunRetiresOnlyRemovedHandlerDomain`、`TestRunDoesNotRetireOutputsBeforeEmptyBootstrapIsValidated`
- [validation_test.go](../../../codegen/internal/protocol/validation_test.go)：`TestParseRejectsEachStructuralViolation`、`TestParseRejectsEachValueAndReferenceViolation`、`TestParseRejectsEachMarkerViolation`、`TestValidateDefinitionsEnforcesIDAndEnumInvariants`、`TestBootstrapRequiresHandlerImportBaseWhenControllersExist`

### codegen/internal/registry

- [registry_test.go](../../../codegen/internal/registry/registry_test.go)：`TestScanOrdersByPhaseThenOrderThenPath`、`TestScanIsDeterministicAcrossRuns`、`TestScanRejectsUnusableMarkers`、`TestScanIgnoresTestdataVendorAndTestFiles`、`TestGeneratedAggregateParsesAndImportsWhatItUses`；其余 12 项见文件

### codegen/internal/roost

- [add_effect_test.go](../../../codegen/internal/roost/add_effect_test.go)：`TestAddEffectGeneratesPayloadEmitAndConsumer`、`TestAddEffectRejectsBadInputWithoutTouchingTheProject`
- [add_saga_test.go](../../../codegen/internal/roost/add_saga_test.go)：`TestAddSagaNativeGeneratesStepsHandlersAndSenders`、`TestAddSagaWithoutNativeRegistersMongoSteps`、`TestAddSagaNativeRejectsBadInputWithoutTouchingTheProject`、`TestAddSagaNativeNeedsNestInTheStepService`、`TestDoctorWarnsWhileStepsOrEffectConsumersAreNotStarted`；其余 2 项见文件（含真实依赖 `TestGeneratedSagaNativeStepsAndEffectConsumer`）
- [add_test.go](../../../codegen/internal/roost/add_test.go)：`TestAddEntityScaffoldsTheOtherCategoryInsteadOfMintingOne`、`TestAddModAppendsItsConfigSectionToExistingServiceConfigs`、`TestAddRPCScaffoldsOwnerAndWiresCaller`、`TestAddRefusesEachInvalidKindParameter`、`TestAddHandlerRequiresNestFeature`；其余 2 项见文件
- [attribute_runtime_test.go](../../../codegen/internal/roost/attribute_runtime_test.go)：`TestAttributeRuntimeFileIsMarkedGenerated`、`TestGeneratedAttributeProjectPassesTheTemplateCheck`
- [config_contract_test.go](../../../codegen/internal/roost/config_contract_test.go)：`TestKitConfigSchemasMatchKitDeclarations`、`TestGeneratedConfigsMatchDeclarations`、`TestGeneratedConfigCheckCatchesDrift`、`TestGeneratorConstantsMatchTheDeclaredExamples`、`TestPlayerTCPDeclarationAgreesWithKit`；其余 8 项见文件
- [demo_test.go](../../../codegen/internal/roost/demo_test.go)：`TestDemoEmbedCoversEveryFile`、`TestDemoTemplateStepsAndShippedFilesAgree`、`TestDemoTemplateKeepsTheGameTemplateAndItsFeatures`、`TestDemoTemplateGeneratesABuildableWritePath`、`TestDemoTemplateFollowsTheGameServiceName`；其余 6 项见文件
- [dependencies_test.go](../../../codegen/internal/roost/dependencies_test.go)：`TestFrameworkDependencyUpdateStagesAndCommitsOnlyModuleFiles`、`TestUpdateFrameworkDependenciesResolvesAllDirectModulesTogether`、`TestUpdateFrameworkDependenciesUsesExplicitPolicies`、`TestUpdateFrameworkDependenciesRollsBackModuleFiles`、`TestTidyProjectDependenciesDoesNotUpgradeFramework`；其余 5 项见文件
- [deploy_compose_test.go](../../../codegen/internal/roost/deploy_compose_test.go)：`TestProductionComposeMountsTheConfigAsAnExplicitBind`、`TestGeneratedComposeInvocationsUseAnAbsoluteConfigRoot`、`TestDeployScriptsCarryNoKnownShellcheckFindings`、`TestFrameworkCompatMinimumSetMatchesTheGeneratorFloor`、`TestUpgradeCompatHistoryStartsAtTheCurrentPackageLayout`；其余 8 项见文件
- [deploy_image_test.go](../../../codegen/internal/roost/deploy_image_test.go)：`TestProductionImageCarriesConfigDataWhereConfigDataDirResolves`、`TestGeneratedDeploymentsGiveStatsLogAWritableDirectory`
- [dev_run_test.go](../../../codegen/internal/roost/dev_run_test.go)：`TestDevRunRegistersTheGameServerInTheAccountServicesRedisDatabase`、`TestDevRunKeepsAccountctlDefaultsWhenTheRedisKeysAreAbsent`、`TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster`
- [framework_services_test.go](../../../codegen/internal/roost/framework_services_test.go)：`TestGameTemplateHostsEveryFrameworkServiceAndWiresTheGame`、`TestFrameworkServiceManifestValidation`、`TestGameTemplateRendersHostingAndClientWiring`、`TestAProjectWithoutFrameworkServicesDoesNotDependOnRoostService`、`TestGameTemplateScaffoldsWorldAndPlayer`；其余 9 项见文件
- [generate_test.go](../../../codegen/internal/roost/generate_test.go)：`TestTemplateFailureDoesNotPublishPartialProject`、`TestOrdinaryMentionOfGeneratedTextRemainsBusinessInput`、`TestProductionConfigAllowsLoopbackOpsAddress`、`TestForeignGeneratedFileIsNotOwnedByRoost`、`TestGenerateAndSyncIgnoreTheProjectsRuntimeOutput`、`TestGenerateWritesTheTableTemplateItReports`；其余 8 项见文件
- [generated_contract_test.go](../../../codegen/internal/roost/generated_contract_test.go)：`TestA4GeneratedConfigsPassValidation`、`TestGeneratedConfigsPassStrictAndProductionValidation`、`TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout`
- [go_command_tree_test.go](../../../codegen/internal/roost/go_command_tree_test.go)：`TestDoctorGoCommandTimeoutReturnsAndLeavesNoGrandchild`、`TestDoctorGoCommandTimeoutIsItsOwnDeadline`、`TestDependencyCommandCancelWithBufferedOutputReturnsAndLeavesNoGrandchild`、`TestDependencyCommandCancelWithFileOutputLeavesNoGrandchild`、`TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`
- [help_test.go](../../../codegen/internal/roost/help_test.go)：`TestHelpCatalogIsCompleteAndUnique`、`TestHelpOverviewListsCapabilities`、`TestEveryCapabilityHelpContainsConfigurationAndExample`、`TestHelpAliasesAndContextCommands`、`TestUnknownHelpCapabilityFailsWithDiscoveryHint`；其余 3 项见文件
- [id_tools_test.go](../../../codegen/internal/roost/id_tools_test.go)：`TestIDToolsSeeErrcodeDefinitionsTheWayTheGeneratorDoes`
- [k8s_contract_test.go](../../../codegen/internal/roost/k8s_contract_test.go)：`TestGameDemoKubernetesSecretExamplesMirrorTheProductionExamples`、`TestAddedConfigSectionsReachTheKubernetesSecretExample`、`TestCRLFKubernetesSecretExampleGetsTheSameEditsAsLF`、`TestUnrecognizedKubernetesSecretExampleIsAVisibleWarningForEveryCommand`
- [literal_coupling_test.go](../../../codegen/internal/roost/literal_coupling_test.go)：`TestOrchestratorDoesNotRepeatGeneratorDefaultPaths`
- [manifest_test.go](../../../codegen/internal/roost/manifest_test.go)：`TestValidateRejectsUnknownAndRetiredModAndFeatureNames`、`TestValidateRejectsManagerModInSharedMods`、`TestDefaultManifestIncludesManagerModAndValidates`、`TestBootstrapWiresManagerModPerService`、`TestServiceManagersHookMatchesServicePackageClause`
- [next_test.go](../../../codegen/internal/roost/next_test.go)：`TestOptionalHintsNameWhatTheProjectHasNotUsedYet`、`TestGenerateCheckToleratesCRLFCheckouts`
- [player_client_test.go](../../../codegen/internal/roost/player_client_test.go)：`TestGeneratedPlayerTCPTestsWaitForTheSessionsTheyUse`、`TestGeneratedSceneConnectionTestsWaitForEveryDialedPlayer`、`TestA3GeneratedServerStopContract`、`TestA3GeneratedModStopContract`、`TestGeneratedPlayerTCPStopContract`；其余 4 项见文件
- [project_fixture_test.go](../../../codegen/internal/roost/project_fixture_test.go)：`TestMain`、`TestProjectFixtureCopiesMatchAFreshNewProject`
- [release_test.go](../../../codegen/internal/roost/release_test.go)：`TestFrameworkReleaseManifestStrictValidation`、`TestPublishedFrameworkGoModRejectsLocalOrPseudoDependencies`、`TestFrameworkGitHubOutputAndBuildVersion`、`TestCorePinScriptPrintsTheGeneratorMinimum`、`TestGeneratorsDoNotMoveTheProcessIntoTheTreeTheyGenerate`；其余 3 项见文件
- [retirement_test.go](../../../codegen/internal/roost/retirement_test.go)：`TestStagedProjectCommitSeesEntityRetirement`、`TestStagedProjectCommitAndCheckSeeTableJSONRetirement`、`TestSyncCommitsProtocolRetirement`、`TestGenerateCheckDetectsMarkerlessProtocolDrift`
- [roost_test.go](../../../codegen/internal/roost/roost_test.go)：`TestResolveModsAddsRequiredDependencies`、`TestResolveModsDefaultsNestAndSagaToDataEngine`、`TestResolveModsRejectsRemovedPersistenceMods`、`TestDefaultManifestUsesOnlyDataEnginePersistence`、`TestNewProjectSyncPreservesBusinessFiles`、`TestEndpointScaffoldTakesTheHandlerResultAndAnswersBusinessErrors`；其余 48 项见文件
- [shell_contract_test.go](../../../codegen/internal/roost/shell_contract_test.go)：`TestShellInstallPutsConfigDataWhereConfigDataDirResolves`、`TestShellReleaseSwitchStopsTheRunningProcessUnderItsOwnUnit`、`TestShellReleaseSwitchRestoresCurrentAndUnitWhenTheUnitCannotBeInstalled`、`TestShellRollbackRejectsDotVersions`、`TestShellScriptsRefuseAReleaseWithoutARecordedUnit`
- [self_sync_test.go](../../../codegen/internal/roost/self_sync_test.go)：`TestSelfSyncManifestValidation`、`TestSelfSyncOffKeepsTheFormerWiring`、`TestSelfSyncRendersVisibleWiring`、`TestSelfSyncCheckNamesTheFix`、`TestSelfSyncGeneratedProjectOverRealTCP`（按新手流程搭工程、只打开开关并标 sync 字段、不写 packer，在工程里经生成接线跑真实 TCP：全量→增量、重连全量；再换业务 packer 跑一遍、切回默认 packer）
- [shutdown_test.go](../../../codegen/internal/roost/shutdown_test.go)：`TestGeneratedShutdownTimeoutFollowsEachServicesMods`、`TestGeneratedShutdownTimeoutIsSmallerForAServiceWithFewMods`、`TestSyncMovesAnUneditedShutdownBlockWithTheServicesMods`、`TestSyncNeverLowersTheGracePeriodBelowTheConfiguredTotal`、`TestDoctorChecksEachServicesShutdownWindow`；其余 20 项见文件
- [singleton_test.go](../../../codegen/internal/roost/singleton_test.go)：`TestBootstrapInstallsTheSingletonStoreWhenAConfigCanTurnItOn`、`TestServiceConfigsTurnTheSingletonOnOnlyForDataEngineServices`、`TestAddingTheDataEngineLaterTurnsTheGeneratedSingletonOn`、`TestSingletonReleaseIsPartOfTheGeneratedShutdownBlock`、`TestDoctorCountsTheSingletonReleaseForAConfigThatTurnsItOn`；其余 1 项见文件
- [staging_unix_test.go](../../../codegen/internal/roost/staging_unix_test.go)：`TestInterruptedCommandRemovesItsStagingTree`、`TestInterruptInEveryStagePhaseRemovesTheStageAndDiesOfTheSignal`、`TestStagedGeneratorErrorsNameTheProjectPath`、`TestDependencyFailureAfterUpgradeOrSyncPointsAtProjectDeps`

### codegen/internal/servicerpc

- [golden_test.go](../../../codegen/internal/servicerpc/golden_test.go)：`TestGoldenTransport`、`TestGenerationIsDeterministic`、`TestGeneratedSourceIsValidGo`、`TestWireTypesAreUnexported`、`TestAnAffinityMarkerReachesTheGeneratedClient`；其余 8 项见文件
- [halves_test.go](../../../codegen/internal/servicerpc/halves_test.go)：`TestEitherHalfCanBeEmittedAloneIntoAnotherDirectory`、`TestDirAcceptsAnImportPathResolvedInTheOutModule`、`TestTransportHalfImportsCoreOnlyAndAssemblyHalfOwnsTheMods`
- [parse_test.go](../../../codegen/internal/servicerpc/parse_test.go)：`TestParsesAnAnnotatedInterface`、`TestAnUnmarkedInterfaceIsIgnored`、`TestRefusals`、`TestParsingIsDeterministic`、`TestAnUnsafeTypeInsideAPassedStructIsRefused`；其余 21 项见文件
- [retirement_test.go](../../../codegen/internal/servicerpc/retirement_test.go)：`TestRunRetiresTransportWhenInterfaceMarkerIsRemoved`、`TestRunRetiresOnlyItsAssemblyHalf`

### codegen/internal/tablegen

- [main_test.go](../../../codegen/internal/tablegen/main_test.go)：`TestParseMetaRootUsesTargetProjectModulePath`、`TestUnlabelledFieldsRoundTripThroughGeneratedJSON`、`TestUnknownTableKeyIsRejected`、`TestObjectCSVRejectsMultipleDataRows`、`TestGeneratedLoaderAndObjectConverterMatchSchema`
- [retirement_test.go](../../../codegen/internal/tablegen/retirement_test.go)：`TestConvertCSVToJSONRetiresOwnedDataAndPreservesManualData`、`TestConvertCSVToJSONRefusesEditedOrLegacyUnownedRetirement`、`TestTableManifestOtherThanV2IsRejected`
- [rules_test.go](../../../codegen/internal/tablegen/rules_test.go)：`TestReadCSVRecordsEnforcesDeclaredRules`、`TestReadCSVRecordsSkipsTitleTypeAndRuleRows`、`TestWriteGeneratedRefusesToOverwriteWithoutForce`、`TestCheckJSONEnforcesTheDeclaredRules`、`TestRefDeclarationsAreResolvedAtGeneration`；其余 3 项见文件
- [run_test.go](../../../codegen/internal/tablegen/run_test.go)：`TestRunReturnsFlagErrors`

### codegen/internal/webroute

- [retirement_test.go](../../../codegen/internal/webroute/retirement_test.go)：`TestGenerateDirRetiresLastRouteWithoutRemovingManualFile`
- [webroute_test.go](../../../codegen/internal/webroute/webroute_test.go)：`TestParseJSONRoute`、`TestParseRejectsInvalidHandler`、`TestGenerateRoutes`、`TestGenerateRejectsDuplicateRoute`、`TestGenerateRejectsMalformedPatternsWithoutChangingOutput`；其余 5 项见文件

### robot/action

- [call_test.go](../../../robot/action/call_test.go)：`TestRegisterCallConventions`、`TestRegisterCallMapFieldAndValidation`、`TestParamsChain`、`TestRegisterCallGuards`
- [registry_test.go](../../../robot/action/registry_test.go)：`TestActionRegistryRefusesNamelessAndDuplicateActions`、`TestActionRegistryNilAndLookupGuards`、`TestWaitPushRequiresMsgParamAndSession`

### robot

- [context_test.go](../../../robot/context_test.go)：`TestContextRefusesMissingRunnerSessionAndCaptureArguments`
- [lockstep_test.go](../../../robot/lockstep_test.go)：`TestLockstepBotTransientSinkFailureKeepsTheBatchTail`、`TestLockstepBotSimulateFailureIsTerminal`、`TestLockstepBotPendingApplyIsBounded`、`TestLockstepBotRetainedFramesDoNotAliasCallerPayloads`、`TestLockstepBotRequestsCatchupOnAnyUnhealedGap`、`TestLockstepBotRestoresFromASnapshotAndContinues`（快照恢复：另有纯线协议续链、失败终止、取代 pending 帧、传输停住后重试）；其余 7 项见文件
- [robot_impl_test.go](../../../robot/robot_impl_test.go)：`TestKCPDialerEndToEnd`、`TestQUICDialerEndToEnd`、`TestQUICDialerRequiresALPN`、`TestLockstepBotsSurviveThirtyPercentLoss`、`TestLockstepBotRequestsCatchupOncePerGap`；其余 1 项见文件
- [robot_test.go](../../../robot/robot_test.go)：`TestPacketCodecRoundTrip`、`TestSessionCallEchoAndPush`、`TestSessionCloseFansOutToPendingAndWaiters`、`TestTypedKeyAndBlackboard`、`TestCoalescerDedupsAndFlushes`；其余 5 项见文件

### robot/loadtest

- [loadtest_test.go](../../../robot/loadtest/loadtest_test.go)：`TestManagerLifecycleAndSingleActiveRun`、`TestAdminCommandsAndReport`、`TestAdminStartGate`、`TestManagerEntryPointsRefuseMissingPartsAndUnknownRuns`、`TestARunCutByItsDurationStopsWithReasonDuration`；其余 2 项见文件
- [threshold_test.go](../../../robot/loadtest/threshold_test.go)：`TestThresholdViolationFailsRun`、`TestThresholdsWithoutSamplesFailTheRun`、`TestQuantileThresholdFailsWhenTheHistogramSeriesWasDropped`、`TestQuantileThresholdJudgesTheObservedCostsNotABucketBound`、`TestThresholdFailureNamesTheThresholdAndTheActualValue`

### robot/runner

- [runner_test.go](../../../robot/runner/runner_test.go)：`TestPoolExecutorRunsEveryRobotOnce`、`TestLoopingExecutorStopsAndCountsCanceled`、`TestStagedRampUpAndDown`、`TestArrivalRateExecutorLaunchesFreshRobots`、`TestRunnerUnknownScenarioFails`；其余 1 项见文件

### robot/scenario

- [scenario_test.go](../../../robot/scenario/scenario_test.go)：`TestCombinators`、`TestRandomIsSeededPerRobot`、`TestSpecInterpreterRunsEquivalentTree`、`TestScenarioRegistryRefusesNamelessAndDuplicateScenarios`、`TestSpecRefusesEachBrokenDocumentByMessage`；其余 1 项见文件

### robot/session

- [session_test.go](../../../robot/session/session_test.go)：`TestCallReturnsWhenTheWriteBlocksPastItsContext`、`TestALateResponseIsNotDeliveredAsAPush`

### robot/transport

- [dial_test.go](../../../robot/transport/dial_test.go)：`TestWebSocketDialHonorsDialTimeoutAndContext`、`TestWebSocketDialStillReachesARealEndpoint`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)；改生成模板要重新生成 game-demo 跑 build / vet / test，并保证 `go generate ./...` 后工作区无漂移。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
