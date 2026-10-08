# 次核心：Codegen 与工程工具：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/12-codegen.md)

## 如何阅读

Codegen根据业务定义生成重复的连接代码，让你少写DAO、消息调用和协议注册等样板。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`codegen`、`demo`、`robot`、`cmd/glsvet`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 生成物依赖最低 core v1.24.0，发布 manifest 与 tag 一致。
2. 全阶段成功后发布目标，标记不等于覆盖权限。
3. 重复生成无漂移，真实 tag 消费不使用本地 replace。

## 2A. 正式工程生成链路

[manifest.go](../../../codegen/internal/roost/manifest.go)定义Manifest、默认策略和minimumVersions；运行时最低Core固定v1.24.0。renderProject在[render.go](../../../codegen/internal/roost/render.go)构建plannedFile集合，renderBootstrap/服务Mod构造/配置段/Makefile等由同一清单导出。render生成候选字节不是正式发布到用户目标目录，写入权限和全阶段发布由项目事务流程控制。

DAO/Entity/Nest/Protocol/ServiceRPC各有独立解析和golden测试，模板中出现Go函数文本不表示它是生成器运行时函数。修改生成形状应验证生成输出的行为，而非只断言某段字符串出现。

[pretag.sh](../../../scripts/pretag.sh)先确认版本、远端tag状态、无replace与干净树；go generate后再次检查，再检查release manifest，最后build/vet/tidy/test。远端查询失败不能解释成tag不存在。最低版本与发布版本分别更新，文档补丁不随意抬高minimum。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### cmd/glsvet

4 个实现文件、6 个测试文件。

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

### codegen/cmd/eventgen

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/cmd/eventgen/main.go) | 函数/方法或内部实现；见源码 |

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

5 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/attribute/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/attribute/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/attribute/parse.go) | 函数/方法或内部实现；见源码 |
| [types.go](../../../codegen/internal/attribute/types.go) | `ProfileDef`、`AttributeField`、`FormulaDef` |
| [util.go](../../../codegen/internal/attribute/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/cfggen

1 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/cfggen/main.go) | `Meta`、`GroupsMeta`、`GroupList`、`BeanMeta`、`TableMeta`、`FieldMeta` |

### codegen/internal/dao

8 个实现文件、10 个测试文件。

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

8 个实现文件、16 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/entity/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/entity/main.go) | 函数/方法或内部实现；见源码 |
| [mirror.go](../../../codegen/internal/entity/mirror.go) | `MirrorDef` |
| [nocoll_dao.go](../../../codegen/internal/entity/nocoll_dao.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/entity/parse.go) | `EntityDef`、`ImportDef`、`ComponentField`、`DaoField` |
| [remote_dao_scope.go](../../../codegen/internal/entity/remote_dao_scope.go) | 函数/方法或内部实现；见源码 |
| [run.go](../../../codegen/internal/entity/run.go) | 函数/方法或内部实现；见源码 |
| [util.go](../../../codegen/internal/entity/util.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/errcode

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/errcode/main.go) | `Definition` |

### codegen/internal/eventgen

6 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/eventgen/gen.go) | 函数/方法或内部实现；见源码 |
| [handler.go](../../../codegen/internal/eventgen/handler.go) | `HandlerInfo`、`HandlerEvent` |
| [handler_gen.go](../../../codegen/internal/eventgen/handler_gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/eventgen/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/eventgen/parse.go) | `EventDef`、`EventFieldDef` |
| [run.go](../../../codegen/internal/eventgen/run.go) | 函数/方法或内部实现；见源码 |

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

5 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bootstrap.go](../../../codegen/internal/nest/bootstrap.go) | 函数/方法或内部实现；见源码 |
| [gen.go](../../../codegen/internal/nest/gen.go) | 函数/方法或内部实现；见源码 |
| [main.go](../../../codegen/internal/nest/main.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/nest/parse.go) | `FuncInfo`、`RemoteAccessInfo`、`EntityParam`、`NonEntityParam`、`RetParam`、`ErrorRetParam`、`ParseResult`、`ImportInfo` |
| [run.go](../../../codegen/internal/nest/run.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/project

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [module.go](../../../codegen/internal/project/module.go) | `Info` |

### codegen/internal/protocol

9 个实现文件、7 个测试文件。

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

3 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/registry/gen.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../codegen/internal/registry/parse.go) | `Registration` |
| [run.go](../../../codegen/internal/registry/run.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/roost

41 个实现文件、75 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [activity_groups.go](../../../codegen/internal/roost/activity_groups.go) | 函数/方法或内部实现；见源码 |
| [add.go](../../../codegen/internal/roost/add.go) | `AddOptions` |
| [add_entity.go](../../../codegen/internal/roost/add_entity.go) | `I` |
| [add_rpc.go](../../../codegen/internal/roost/add_rpc.go) | `Service`、`Mod` |
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
| [consolidate.go](../../../codegen/internal/roost/consolidate.go) | `ConsolidateResult` |
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
| [shutdown_budget.go](../../../codegen/internal/roost/shutdown_budget.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/roost/cmd/attributeruntime

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/roost/cmd/attributeruntime/main.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/servicerpc

6 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gen.go](../../../codegen/internal/servicerpc/gen.go) | `File`、`Half`、`Options` |
| [parse.go](../../../codegen/internal/servicerpc/parse.go) | `Service`、`Method`、`Field` |
| [run.go](../../../codegen/internal/servicerpc/run.go) | 函数/方法或内部实现；见源码 |
| [template.go](../../../codegen/internal/servicerpc/template.go) | `BusClient`、`Server`、`ClientMod` |
| [validate.go](../../../codegen/internal/servicerpc/validate.go) | 函数/方法或内部实现；见源码 |
| [wiresafe.go](../../../codegen/internal/servicerpc/wiresafe.go) | 函数/方法或内部实现；见源码 |

### codegen/internal/tablegen

1 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../codegen/internal/tablegen/main.go) | `TableKind`、`Meta`、`Field` |

### codegen/internal/webroute

3 个实现文件、5 个测试文件。

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

4 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [action.go](../../../robot/action/action.go) | `Action`、`Func`、`Registry` |
| [builtin.go](../../../robot/action/builtin.go) | 函数/方法或内部实现；见源码 |
| [call.go](../../../robot/action/call.go) | `CallOption` |
| [params.go](../../../robot/action/params.go) | `Params` |

### robot

4 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [blackboard.go](../../../robot/blackboard.go) | `Blackboard` |
| [dialers.go](../../../robot/dialers.go) | `KCPDialerConfig`、`QUICDialerConfig` |
| [lockstep.go](../../../robot/lockstep.go) | `LockstepSink`、`LockstepBotConfig`、`LockstepBotStats`、`LockstepBot`、`FrameHasher` |
| [robot.go](../../../robot/robot.go) | `ActionRunner`、`AuthProvider`、`Config`、`Context`、`TypedKey`、`Coalescer`、`BoundedQueue` |

### robot/loadtest

2 个实现文件、6 个测试文件。

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

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [runner.go](../../../robot/runner/runner.go) | `Executor`、`Ramp`、`Stage`、`IdentityProvider`、`Config`、`Stats`、`Runner`、`Option` |

### robot/scenario

2 个实现文件、3 个测试文件。

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
- [componentfields_promises_test.go](../../../cmd/glsvet/componentfields_promises_test.go)：`TestComponentFieldWritesOutsideTheDaoAreHinted`、`TestSkillPackagesGetNoComponentFieldHint`、`TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix`
- [inputs_promises_test.go](../../../cmd/glsvet/inputs_promises_test.go)：`TestMain`、`TestMissingDirectoryArgumentFails`、`TestUnparsableFileFailsInsteadOfSkippingTheDirectory`、`TestDirectoryWithoutGoFilesStillPasses`
- [main_test.go](../../../cmd/glsvet/main_test.go)：`TestHandlerRawGoroutineIsRejected`、`TestHandlerNamedGoroutineWrapperIsRejected`、`TestHandlerErrgroupGoIsRejected`、`TestHandlerCoreWorkerPoolGoIsAllowed`、`TestHandlerCoreWorkerPoolFieldGoIsAllowed`；其余 11 项见文件
- [method_handler_promises_test.go](../../../cmd/glsvet/method_handler_promises_test.go)：`TestMethodHandlerConcurrencyChecks`
- [stophints_test.go](../../../cmd/glsvet/stophints_test.go)：`TestStopHintsFlagABareReceiveInAStopWithContext`、`TestStopHintsFollowOneCallIntoAHelperThatCannotSeeTheContext`、`TestStopHintsAcceptBoundedWaits`、`TestStopHintsDoNotCountAsFindings`

### codegen/internal/attribute

- [representable_promises_test.go](../../../codegen/internal/attribute/representable_promises_test.go)：`TestParseDirRejectsAttributesTheWireCannotCarry`
- [retirement_test.go](../../../codegen/internal/attribute/retirement_test.go)：`TestRunRetiresOnlyOwnedProfileAfterLastMarkerIsRemoved`
- [run_test.go](../../../codegen/internal/attribute/run_test.go)：`TestRunGeneratesProfile`
- [util_promises_test.go](../../../codegen/internal/attribute/util_promises_test.go)：`TestParsePositiveIntRefusesNonPositiveValues`
- [validation_test.go](../../../codegen/internal/attribute/validation_test.go)：`TestParseDirRejectsEachProfileViolation`

### codegen/internal/cfggen

- [consumer_promises_test.go](../../../codegen/internal/cfggen/consumer_promises_test.go)：`TestCfggenDisabledIndexesCompile`
- [global_rules_promises_test.go](../../../codegen/internal/cfggen/global_rules_promises_test.go)：`TestCfggenGlobalRulesBecomeObjectDefRules`、`TestCfggenGlobalWithoutRulesKeepsItsRegistration`、`TestCfggenGlobalRulesStillRejectWhatAnObjectCannotMean`
- [groups_exhausted_promises_test.go](../../../codegen/internal/cfggen/groups_exhausted_promises_test.go)：`TestExportRefusesABeanWithEveryFieldExcluded`
- [groups_test.go](../../../codegen/internal/cfggen/groups_test.go)：`TestCfggenExportsOnlyTheTargetGroups`、`TestCfggenGroupsFlagSelectsAnotherTarget`、`TestCfggenWithoutGroupsExportsEverything`、`TestCfggenRefusesEachInvalidGroupUse`
- [main_test.go](../../../codegen/internal/cfggen/main_test.go)：`TestCfggenGeneratesStructsRegistrationAndAccessors`、`TestCfggenAcceptsObjectsAliasForGlobals`、`TestCfggenRejectsBrokenMeta`、`TestCfggenIndexFalseDisablesIndex`、`TestCfggenKeywordFieldNamesGenerateCompilableParams`；其余 5 项见文件
- [meta_promises_test.go](../../../codegen/internal/cfggen/meta_promises_test.go)：`TestCfggenRejectsEachFieldRuleForTheStatedReason`
- [namespace_promises_test.go](../../../codegen/internal/cfggen/namespace_promises_test.go)：`TestCfggenRejectsReservedBeanNamesBeforeWriting`、`TestCfggenNonCollidingBeanNamesCompile`
- [validation_messages_test.go](../../../codegen/internal/cfggen/validation_messages_test.go)：`TestCfggenRejectsBrokenMetaForTheStatedReason`

### codegen/internal/dao

- [failfast_test.go](../../../codegen/internal/dao/failfast_test.go)：`TestParseRejectsOrphanMarker`、`TestParseRejectsMarkerBindingMultipleStructs`、`TestRunSweepsOrphansWhenAllDefinitionsRemoved`、`TestParseAcceptsMarkerAboveDocComment`、`TestParseRejectsMarkerWithoutParameters`；其余 5 项见文件
- [gen_promises_test.go](../../../codegen/internal/dao/gen_promises_test.go)：`TestGenerateRedisDaoRefusesEachIncompleteDefinition`、`TestGenerateDaoRefusesUnknownDatabaseScope`
- [golden_test.go](../../../codegen/internal/dao/golden_test.go)：`TestGoldenGeneratedOutput`
- [keyword_field_promises_test.go](../../../codegen/internal/dao/keyword_field_promises_test.go)：`TestAFieldWhoseLowercaseNameIsAKeywordGenerates`、`TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates`
- [nested_bson_promises_test.go](../../../codegen/internal/dao/nested_bson_promises_test.go)：`TestNestedStructsPersistTheirFieldsNotTheDirtyHook`、`TestTheDaoDocumentCarriesNestedWireFormsNotMarshalers`
- [nocoll_promises_test.go](../../../codegen/internal/dao/nocoll_promises_test.go)：`TestANoCollectionDaoKeepsSyncAndDropsEveryStoragePath`、`TestANoCollectionDaoWithPersistentFieldsIsRefusedByName`、`TestTheNoCollectionMarkerRefusesContradictions`、`TestACollectionDaoIsUnchangedByTheNoCollectionFlag`、`TestSwitchingADaoToNoCollectionRetiresItsStoragePaths`
- [parse_test.go](../../../codegen/internal/dao/parse_test.go)：`TestParseDefDir`、`TestGenerateDao`、`TestGenerateNested`、`TestGenerateNestedPointerGetterDoesNotAddPointerLevel`、`TestFieldVarNamePreservesInitialisms`；其余 4 项见文件
- [redis_marker_promises_test.go](../../../codegen/internal/dao/redis_marker_promises_test.go)：`TestParseRefusesEachMalformedRedisMarkerAndConflictingDaoTag`
- [schema_version_promises_test.go](../../../codegen/internal/dao/schema_version_promises_test.go)：`TestDaoSchemaVersionComesFromTheMarker`、`TestDaoSchemaVersionDefaultsToOne`、`TestDaoSchemaVersionRefusesNonsense`
- [transient_field_promises_test.go](../../../codegen/internal/dao/transient_field_promises_test.go)：`TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync`

### codegen/internal/entity

- [category_import_promises_test.go](../../../codegen/internal/entity/category_import_promises_test.go)：`TestGeneratedWiringImportsEveryPackageItQualifies`
- [category_marker_promises_test.go](../../../codegen/internal/entity/category_marker_promises_test.go)：`TestEntityMarkerCategoryIsGeneratedDirectly`、`TestEntityMarkerWithoutCategoryKeepsTheRuntimeLookup`、`TestEntityMarkerRefusesAMalformedCategory`
- [gen_promises_test.go](../../../codegen/internal/entity/gen_promises_test.go)：`TestGenerateRefusesManagedRemoteWithoutRemoteBase`、`TestParseDirRejectsMarkerParameterGivenTwice`
- [guard_tests_promises_test.go](../../../codegen/internal/entity/guard_tests_promises_test.go)：`TestGenerateEmitsAndReconcilesRemoteGuardTests`
- [marker_promises_test.go](../../../codegen/internal/entity/marker_promises_test.go)：`TestParseDirRejectsMarkerNotAttachedToStruct`、`TestParseDirRejectsMarkerAboveNonStruct`、`TestParseDirRejectsUnknownMarkerParameters`、`TestParseDirRejectsInvalidMarkerValues`、`TestParseDirAcceptsEveryDocumentedMarkerForm`
- [mirror_promises_test.go](../../../codegen/internal/entity/mirror_promises_test.go)：`TestRemoteMirrorEntityMarkerIsAMigrationError`、`TestMirrorDTOGeneratesReadOnlyView`、`TestMirrorDTOOnlyPackageIsDiscoveredAndRetired`、`TestMirrorMarkerValidation`
- [multi_entity_package_promises_test.go](../../../codegen/internal/entity/multi_entity_package_promises_test.go)：`TestGenerateGivesEachEntityInAPackageDistinctRegistrationSymbols`
- [nocoll_dao_promises_test.go](../../../codegen/internal/entity/nocoll_dao_promises_test.go)：`TestANoPersistEntityWiresANoCollectionDaoByItsRegistryKey`、`TestACollectionDaoIsStillWiredByItsCollection`、`TestAStoredEntityCannotUseANoCollectionDao`
- [output_multi_entity_promises_test.go](../../../codegen/internal/entity/output_multi_entity_promises_test.go)：`TestExplicitOutputRefusesMoreThanOneEntityInAPackage`
- [parse_test.go](../../../codegen/internal/entity/parse_test.go)：`TestParseDir`、`TestGenerate`、`TestGenerateRemoteManagedV2Participant`、`TestGeneratePreservesManualSyncMethods`、`TestToSnake`
- [remote_capable_removed_promises_test.go](../../../codegen/internal/entity/remote_capable_removed_promises_test.go)：`TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement`
- [remote_dao_scope_promises_test.go](../../../codegen/internal/entity/remote_dao_scope_promises_test.go)：`TestRemoteManagedEntityRejectsServerScopedDao`
- [retirement_test.go](../../../codegen/internal/entity/retirement_test.go)：`TestRunRetiresEntityWireAfterLastMarkerRemoved`、`TestRunMovesRegistrationWhenFirstSiblingIsRetired`
- [run_test.go](../../../codegen/internal/entity/run_test.go)：`TestRunReturnsFlagErrors`
- [sync_namespace_promises_test.go](../../../codegen/internal/entity/sync_namespace_promises_test.go)：`TestSyncNamespaceRejectsABareIdentifier`、`TestSyncNamespaceAcceptsLiteralsAndQualifiedConstants`
- [sync_packer_markers_promises_test.go](../../../codegen/internal/entity/sync_packer_markers_promises_test.go)：`TestGeneratedSyncBlockOnlyNamesFieldsCoreHas`、`TestRetiredPackerMarkerIsRefused`、`TestPackerWithoutSyncIsRefused`

### codegen/internal/errcode

- [ast_test.go](../../../codegen/internal/errcode/ast_test.go)：`TestExtractDefinitionsIgnoresCommentAndStringLiterals`
- [export_test.go](../../../codegen/internal/errcode/export_test.go)：`TestExtractDefinitionsAndWriteCSV`
- [promises_test.go](../../../codegen/internal/errcode/promises_test.go)：`TestExtractDefinitionsRejectsADuplicateCode`
- [scan_promises_test.go](../../../codegen/internal/errcode/scan_promises_test.go)：`TestExtractDefinitionsRefusesWhatItCannotRead`

### codegen/internal/eventgen

- [args_promises_test.go](../../../codegen/internal/eventgen/args_promises_test.go)：`TestRunNamesUnexpectedPositionalArguments`
- [golden_test.go](../../../codegen/internal/eventgen/golden_test.go)：`TestGoldenHandlerOutput`
- [handler_promises_test.go](../../../codegen/internal/eventgen/handler_promises_test.go)：`TestScanGameDirRefusesUnparseableSource`、`TestScanGameDirRejectsHandlerForUndeclaredEvent`、`TestScanGameDirAcceptsDeclaredHandlers`
- [handler_test.go](../../../codegen/internal/eventgen/handler_test.go)：`TestScanFile`、`TestScanGameDir`、`TestRecvVar`
- [parse_test.go](../../../codegen/internal/eventgen/parse_test.go)：`TestParseEventDir`、`TestConstName`、`TestGenerateDefs`、`TestGenerateTypes`、`TestGenerateTypeImpl`
- [retirement_test.go](../../../codegen/internal/eventgen/retirement_test.go)：`TestRunRetiresEventTypesAndHandlerAfterLastDefinition`、`TestScanGameDirRetiresOnlyRemovedReceiver`
- [run_test.go](../../../codegen/internal/eventgen/run_test.go)：`TestRunReturnsFlagErrors`

### codegen/internal/marker

- [marker_test.go](../../../codegen/internal/marker/marker_test.go)：`TestBothPrefixesParseIdentically`、`TestRegexpIsAnchoredAndKindIsLiteral`、`TestFindLegacyReportsProductionSourcesOnly`
- [typo_guard_test.go](../../../codegen/internal/marker/typo_guard_test.go)：`TestEveryMarkerRefusesAMisspeltKey`、`TestEveryMarkerKindHasASpecAndAGuardCase`

### codegen/internal/nest

- [blank_param_promises_test.go](../../../codegen/internal/nest/blank_param_promises_test.go)：`TestABlankParameterNameGeneratesUsableCode`
- [guards_promises_test.go](../../../codegen/internal/nest/guards_promises_test.go)：`TestParseFileRejectsDuplicateRemoteAliasesWithinAndAcrossFiles`、`TestModuleDiscoveryRejectsBrokenOrMissingGoMod`
- [handler_promises_test.go](../../../codegen/internal/nest/handler_promises_test.go)：`TestParseFileRefusesValueReceiversAndBadTargetDeclarations`
- [nest_test.go](../../../codegen/internal/nest/nest_test.go)：`TestParseFile`、`TestValidateTransactionOptionsRejectsLegacyDirty`、`TestParseFileReadsRemoteRequestTagsFromPackageFiles`、`TestGenerate`、`TestGenerateMultipleNonErrorReturns`；其余 7 项见文件
- [pipelined_marker_promises_test.go](../../../codegen/internal/nest/pipelined_marker_promises_test.go)：`TestGeneratePipelinedHandlerMeta`
- [promises_test.go](../../../codegen/internal/nest/promises_test.go)：`TestParseFileRejectsRemainingRemoteTagViolations`、`TestOneSourceFileCannotMixHandlerReceivers`
- [retirement_test.go](../../../codegen/internal/nest/retirement_test.go)：`TestRunRetiresNestOutputsAfterLastMarkerRemoved`、`TestRunWithoutSenderRetiresOnlySenderOutputs`
- [return_type_imports_promises_test.go](../../../codegen/internal/nest/return_type_imports_promises_test.go)：`TestHandlerSideGenerationDoesNotImportReturnOnlyPackages`
- [run_test.go](../../../codegen/internal/nest/run_test.go)：`TestRunReturnsFlagErrors`
- [sender_guard_test_promises_test.go](../../../codegen/internal/nest/sender_guard_test_promises_test.go)：`TestGenerateSenderGuardTest`

### codegen/internal/project

- [module_test.go](../../../codegen/internal/project/module_test.go)：`TestDiscoverDerivesImportAndPackage`、`TestDiscoverRejectsDirectoryOutsideModule`、`TestPackageNameFallsBackToGeneratedFiles`
- [package_promises_test.go](../../../codegen/internal/project/package_promises_test.go)：`TestPackageNameRefusesADirectoryWithTwoPackages`

### codegen/internal/protocol

- [csharp_test.go](../../../codegen/internal/protocol/csharp_test.go)：`TestCSharpCLIIDsAndRetirement`
- [definition_promises_test.go](../../../codegen/internal/protocol/definition_promises_test.go)：`TestParseRejectsEachValueAndReferenceViolation`
- [golden_test.go](../../../codegen/internal/protocol/golden_test.go)：`TestGoldenProtocolOutput`
- [guards_promises_test.go](../../../codegen/internal/protocol/guards_promises_test.go)：`TestParseRejectsEachMarkerViolation`、`TestValidateDefinitionsEnforcesIDAndEnumInvariants`、`TestBootstrapRequiresHandlerImportBaseWhenControllersExist`
- [protocol_test.go](../../../codegen/internal/protocol/protocol_test.go)：`TestParseAndGenerateProtocol`、`TestRunDiscoversTargetModule`、`TestDuplicateMessageIDRejected`
- [retirement_test.go](../../../codegen/internal/protocol/retirement_test.go)：`TestRunRetiresOutputsWhenLastDefinitionIsRemoved`、`TestRunRetiresHandlersButKeepsEmptyBootstrap`、`TestRunPreservesUnrecognizedFilesWhenDefinitionsAreEmpty`、`TestRunRetiresOnlyRemovedHandlerDomain`、`TestRunDoesNotRetireOutputsBeforeEmptyBootstrapIsValidated`
- [validation_test.go](../../../codegen/internal/protocol/validation_test.go)：`TestParseRejectsEachStructuralViolation`

### codegen/internal/registry

- [entity_alias_promises_test.go](../../../codegen/internal/registry/entity_alias_promises_test.go)：`TestAggregateReservesItsFixedImportNames`
- [promises_test.go](../../../codegen/internal/registry/promises_test.go)：`TestScanReportsAnUnparsableFileInsteadOfDroppingItsRegistrations`、`TestRenderChecksTheErrorOfErrorReturningRegistrations`
- [registry_test.go](../../../codegen/internal/registry/registry_test.go)：`TestScanOrdersByPhaseThenOrderThenPath`、`TestScanIsDeterministicAcrossRuns`、`TestScanRejectsUnusableMarkers`、`TestScanIgnoresTestdataVendorAndTestFiles`、`TestGeneratedAggregateParsesAndImportsWhatItUses`；其余 7 项见文件
- [validate_promises_test.go](../../../codegen/internal/registry/validate_promises_test.go)：`TestGeneratedAggregateValidatesTheEntityRegistry`、`TestGeneratedAggregateValidatesEvenWithNoRegistrations`

### codegen/internal/roost

- [activity_groups_promises_test.go](../../../codegen/internal/roost/activity_groups_promises_test.go)：`TestGameDemoActivityGroupsFileIsTheOneDefinition`、`TestNoActivityGroupsFileWithoutTheCoordinator`
- [add_entity_category_promises_test.go](../../../codegen/internal/roost/add_entity_category_promises_test.go)：`TestAddEntityScaffoldsTheOtherCategoryInsteadOfMintingOne`
- [add_mod_config_promises_test.go](../../../codegen/internal/roost/add_mod_config_promises_test.go)：`TestAddModAppendsItsConfigSectionToExistingServiceConfigs`
- [add_promises_test.go](../../../codegen/internal/roost/add_promises_test.go)：`TestAddRefusesEachInvalidKindParameter`、`TestAddHandlerRequiresNestFeature`
- [add_rpc_promises_test.go](../../../codegen/internal/roost/add_rpc_promises_test.go)：`TestAddRPCScaffoldsOwnerAndWiresCaller`
- [attribute_runtime_promises_test.go](../../../codegen/internal/roost/attribute_runtime_promises_test.go)：`TestAttributeRuntimeFileIsMarkedGenerated`、`TestGeneratedAttributeProjectPassesTheTemplateCheck`
- [cfggen_help_promises_test.go](../../../codegen/internal/roost/cfggen_help_promises_test.go)：`TestCfggenHelpUsageCoexistsWithTheProjectGenerators`
- [client_sdk_promises_test.go](../../../codegen/internal/roost/client_sdk_promises_test.go)：`TestClientSDKRealGeneratedTCP`、`TestClientSDKAgainstGeneratedPlayerTCP`、`TestClientSDKRegistrationHook`、`TestClientSDKRegistrationHook`
- [collaborators_imports_promises_test.go](../../../codegen/internal/roost/collaborators_imports_promises_test.go)：`TestFrameworkCollaboratorsImportOnlyWhatTheyUse`
- [compose_shape_check_promises_test.go](../../../codegen/internal/roost/compose_shape_check_promises_test.go)：`TestGeneratedProjectCarriesAComposeShapeCheckAndRunsItInCI`、`TestGeneratedComposeShapeCheckCatchesTheTmpfsShapeComposeUpRefuses`
- [compose_tmpfs_promises_test.go](../../../codegen/internal/roost/compose_tmpfs_promises_test.go)：`TestProductionComposeTmpfsIsOneAbsoluteMountPerService`
- [config_declarations_promises_test.go](../../../codegen/internal/roost/config_declarations_promises_test.go)：`TestKitConfigSchemasMatchKitDeclarations`、`TestGeneratedConfigsMatchDeclarations`、`TestGeneratedConfigCheckCatchesDrift`、`TestGeneratorConstantsMatchTheDeclaredExamples`、`TestPlayerTCPDeclarationAgreesWithKit`；其余 1 项见文件
- [config_reads_promises_test.go](../../../codegen/internal/roost/config_reads_promises_test.go)：`TestGeneratedProjectsReadConfigOnlyThroughDeclarations`、`TestGeneratedProjectConfigGuardCatchesDrift`、`TestDoctorReadsBusinessDeclarationsFromTheProcess`
- [config_section_line_endings_promises_test.go](../../../codegen/internal/roost/config_section_line_endings_promises_test.go)：`TestAddedConfigSectionsFollowTheFilesLineEndings`
- [consolidate_layout_promises_test.go](../../../codegen/internal/roost/consolidate_layout_promises_test.go)：`TestLayoutStageMovesTheSyncBlock`、`TestLayoutPathRules`
- [consolidate_single_import_promises_test.go](../../../codegen/internal/roost/consolidate_single_import_promises_test.go)：`TestConsolidateSplitsASingleLineImportIntoAValidSecondDeclaration`
- [consolidate_single_module_promises_test.go](../../../codegen/internal/roost/consolidate_single_module_promises_test.go)：`TestSingleModuleStageRunsAfterThePackageTable`、`TestSingleModulePathRules`
- [consolidate_test.go](../../../codegen/internal/roost/consolidate_test.go)：`TestConsolidateProjectRewritesImportsGoModAndManifest`、`TestConsolidateProjectDryRunWritesNothing`、`TestConsolidateProjectRefusesUnmappedRemovedImports`、`TestConsolidationMapMatchesTheGeneratorFloor`
- [core_pin_script_promises_test.go](../../../codegen/internal/roost/core_pin_script_promises_test.go)：`TestCorePinScriptPrintsTheGeneratorMinimum`
- [demo_prod_config_promises_test.go](../../../codegen/internal/roost/demo_prod_config_promises_test.go)：`TestGameDemoProductionConfigsPassTheGameInitChecks`
- [demo_test.go](../../../codegen/internal/roost/demo_test.go)：`TestDemoEmbedCoversEveryFile`、`TestDemoTemplateStepsAndShippedFilesAgree`、`TestDemoTemplateKeepsTheGameTemplateAndItsFeatures`、`TestDemoTemplateGeneratesABuildableWritePath`、`TestDemoTemplateFollowsTheGameServiceName`；其余 1 项见文件
- [dependencies_consolidation_promises_test.go](../../../codegen/internal/roost/dependencies_consolidation_promises_test.go)：`TestFrameworkDependencyConsolidationCommitsOnlyPlannedMigration`、`TestFrameworkDependencyConsolidationFailurePreservesInputs`、`TestFrameworkDependencyConsolidationRejectsConcurrentInputChanges`
- [dependencies_test.go](../../../codegen/internal/roost/dependencies_test.go)：`TestFrameworkDependencyUpdateStagesAndCommitsOnlyModuleFiles`、`TestUpdateFrameworkDependenciesResolvesAllDirectModulesTogether`、`TestUpdateFrameworkDependenciesUsesExplicitPolicies`、`TestUpdateFrameworkDependenciesRollsBackModuleFiles`、`TestTidyProjectDependenciesDoesNotUpgradeFramework`；其余 3 项见文件
- [deploy_hygiene_test.go](../../../codegen/internal/roost/deploy_hygiene_test.go)：`TestProductionComposeMountsTheConfigAsAnExplicitBind`、`TestGeneratedComposeInvocationsUseAnAbsoluteConfigRoot`、`TestDeployScriptsCarryNoKnownShellcheckFindings`、`TestFrameworkCompatMinimumSetMatchesTheGeneratorFloor`、`TestUpgradeCompatHistoryStartsAtTheRoostModulePaths`；其余 6 项见文件
- [dev_run_account_promises_test.go](../../../codegen/internal/roost/dev_run_account_promises_test.go)：`TestDevRunRegistersTheGameServerInTheAccountServicesRedisDatabase`、`TestDevRunKeepsAccountctlDefaultsWhenTheRedisKeysAreAbsent`、`TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster`
- [diff_preview_promises_test.go](../../../codegen/internal/roost/diff_preview_promises_test.go)：`TestProjectDiffListsEveryFileTheNextSyncRewrites`、`TestUpgradeDryRunListsEveryFileTheUpgradeRewrites`
- [entity_retirement_test.go](../../../codegen/internal/roost/entity_retirement_test.go)：`TestStagedProjectCommitSeesEntityRetirement`
- [etcd_service_prefix_test.go](../../../codegen/internal/roost/etcd_service_prefix_test.go)：`TestGeneratedEtcdServicePrefixSeparatesTheServerType`
- [framework_mod_chain_promises_test.go](../../../codegen/internal/roost/framework_mod_chain_promises_test.go)：`TestHostedServiceChainsItsOptionalCollaborators`、`TestPlatformConfigCarriesTheSecretsItsModRequires`、`TestDefaultCollaboratorsDefineEveryWiredName`、`TestAServiceNestedUnderAnotherIsHostable`、`TestActivityConfigCarriesTheTTLItsModRequires`
- [framework_release_test.go](../../../codegen/internal/roost/framework_release_test.go)：`TestFrameworkReleaseManifestStrictValidation`、`TestPublishedFrameworkGoModRejectsLocalOrPseudoDependencies`、`TestFrameworkGitHubOutputAndBuildVersion`
- [framework_services_test.go](../../../codegen/internal/roost/framework_services_test.go)：`TestGameTemplateHostsEveryFrameworkServiceAndWiresTheGame`、`TestFrameworkServiceManifestValidation`、`TestGameTemplateRendersHostingAndClientWiring`、`TestAProjectWithoutFrameworkServicesDoesNotDependOnRoostService`、`TestGameTemplateScaffoldsWorldAndPlayer`；其余 2 项见文件
- [generate_changed_promises_test.go](../../../codegen/internal/roost/generate_changed_promises_test.go)：`TestGenerateChangedSeesChangesOfAProjectInARepositorySubdirectory`、`TestGenerateChangedCountsBothSidesOfARename`
- [generated_config_validation_promises_test.go](../../../codegen/internal/roost/generated_config_validation_promises_test.go)：`TestA4GeneratedConfigsPassValidation`、`TestGeneratedConfigsPassStrictAndProductionValidation`
- [generated_project_compiles_promises_test.go](../../../codegen/internal/roost/generated_project_compiles_promises_test.go)：`TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout`
- [generator_cwd_promises_test.go](../../../codegen/internal/roost/generator_cwd_promises_test.go)：`TestGeneratorsDoNotMoveTheProcessIntoTheTreeTheyGenerate`、`TestSyncProjectStagesNeverBecomeAChildsWorkingDirectory`
- [generator_version_promises_test.go](../../../codegen/internal/roost/generator_version_promises_test.go)：`TestVersionsCodegenIsRefused`、`TestTheMakefileRunsTheGeneratorAtTheCoreVersion`
- [gitignore_runtime_promises_test.go](../../../codegen/internal/roost/gitignore_runtime_promises_test.go)：`TestGeneratedGitignoreIgnoresTheRuntimeOutputGenerationSkips`
- [go_command_tree_promises_test.go](../../../codegen/internal/roost/go_command_tree_promises_test.go)：`TestDoctorGoCommandTimeoutReturnsAndLeavesNoGrandchild`、`TestDoctorGoCommandTimeoutIsItsOwnDeadline`、`TestDependencyCommandCancelWithBufferedOutputReturnsAndLeavesNoGrandchild`、`TestDependencyCommandCancelWithFileOutputLeavesNoGrandchild`、`TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`
- [help_test.go](../../../codegen/internal/roost/help_test.go)：`TestHelpCatalogIsCompleteAndUnique`、`TestHelpOverviewListsCapabilities`、`TestEveryCapabilityHelpContainsConfigurationAndExample`、`TestHelpAliasesAndContextCommands`、`TestUnknownHelpCapabilityFailsWithDiscoveryHint`；其余 2 项见文件
- [id_errcode_scan_promises_test.go](../../../codegen/internal/roost/id_errcode_scan_promises_test.go)：`TestIDToolsSeeErrcodeDefinitionsTheWayTheGeneratorDoes`
- [image_configdata_promises_test.go](../../../codegen/internal/roost/image_configdata_promises_test.go)：`TestProductionImageCarriesConfigDataWhereConfigDataDirResolves`
- [interrupt_phases_promises_test.go](../../../codegen/internal/roost/interrupt_phases_promises_test.go)：`TestInterruptInEveryStagePhaseRemovesTheStageAndDiesOfTheSignal`
- [interrupt_stage_promises_test.go](../../../codegen/internal/roost/interrupt_stage_promises_test.go)：`TestInterruptedCommandRemovesItsStagingTree`
- [k8s_secret_config_promises_test.go](../../../codegen/internal/roost/k8s_secret_config_promises_test.go)：`TestGameDemoKubernetesSecretExamplesMirrorTheProductionExamples`、`TestAddedConfigSectionsReachTheKubernetesSecretExample`
- [k8s_secret_crlf_promises_test.go](../../../codegen/internal/roost/k8s_secret_crlf_promises_test.go)：`TestCRLFKubernetesSecretExampleGetsTheSameEditsAsLF`、`TestUnrecognizedKubernetesSecretExampleIsAVisibleWarningForEveryCommand`
- [literal_coupling_test.go](../../../codegen/internal/roost/literal_coupling_test.go)：`TestOrchestratorDoesNotRepeatGeneratorDefaultPaths`
- [logic_offset_doctor_promises_test.go](../../../codegen/internal/roost/logic_offset_doctor_promises_test.go)：`TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees`
- [manager_mod_test.go](../../../codegen/internal/roost/manager_mod_test.go)：`TestValidateRejectsManagerModInSharedMods`、`TestDefaultManifestIncludesManagerModAndValidates`、`TestBootstrapWiresManagerModPerService`、`TestServiceManagersHookMatchesServicePackageClause`
- [manifest_legacy_test.go](../../../codegen/internal/roost/manifest_legacy_test.go)：`TestValidateCanonicalizesLegacyModAndFeatureNames`、`TestValidateStillRejectsUnknownModAndFeature`
- [next_optional_promises_test.go](../../../codegen/internal/roost/next_optional_promises_test.go)：`TestOptionalHintsNameWhatTheProjectHasNotUsedYet`、`TestGenerateCheckToleratesCRLFCheckouts`
- [player_tcp_registration_wait_promises_test.go](../../../codegen/internal/roost/player_tcp_registration_wait_promises_test.go)：`TestGeneratedPlayerTCPTestsWaitForTheSessionsTheyUse`、`TestGeneratedSceneConnectionTestsWaitForEveryDialedPlayer`
- [player_tcp_stop_contract_promises_test.go](../../../codegen/internal/roost/player_tcp_stop_contract_promises_test.go)：`TestA3GeneratedServerStopContract`、`TestA3GeneratedModStopContract`、`TestGeneratedPlayerTCPStopContract`
- [project_boundary_promises_test.go](../../../codegen/internal/roost/project_boundary_promises_test.go)：`TestTemplateFailureDoesNotPublishPartialProject`、`TestOrdinaryMentionOfGeneratedTextRemainsBusinessInput`、`TestNewManifestDoesNotAdvertiseRetiredModules`、`TestProductionConfigAllowsLoopbackOpsAddress`、`TestForeignGeneratedFileIsNotOwnedByRoost`
- [project_fixture_test.go](../../../codegen/internal/roost/project_fixture_test.go)：`TestMain`、`TestProjectFixtureCopiesMatchAFreshNewProject`
- [protocol_retirement_test.go](../../../codegen/internal/roost/protocol_retirement_test.go)：`TestSyncCommitsProtocolRetirement`、`TestGenerateCheckDetectsMarkerlessProtocolDrift`
- [remote_entity_config_keys_promises_test.go](../../../codegen/internal/roost/remote_entity_config_keys_promises_test.go)：`TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys`
- [rollback_inspect_promises_test.go](../../../codegen/internal/roost/rollback_inspect_promises_test.go)：`TestRollbackSyncReportsAFileItCannotInspect`
- [roost_test.go](../../../codegen/internal/roost/roost_test.go)：`TestResolveModsAddsRequiredDependencies`、`TestResolveModsDefaultsNestAndSagaToDataEngine`、`TestResolveModsRejectsRemovedPersistenceMods`、`TestDefaultManifestUsesOnlyDataEnginePersistence`、`TestNewProjectSyncPreservesBusinessFiles`；其余 49 项见文件
- [runtime_dirs_promises_test.go](../../../codegen/internal/roost/runtime_dirs_promises_test.go)：`TestGenerateAndSyncIgnoreTheProjectsRuntimeOutput`
- [service_metrics_promises_test.go](../../../codegen/internal/roost/service_metrics_promises_test.go)：`TestGeneratedServicesReportIntoTheMetricsRegistryByDefault`
- [shell_install_configdata_promises_test.go](../../../codegen/internal/roost/shell_install_configdata_promises_test.go)：`TestShellInstallPutsConfigDataWhereConfigDataDirResolves`
- [shell_rollback_stop_promises_test.go](../../../codegen/internal/roost/shell_rollback_stop_promises_test.go)：`TestShellReleaseSwitchStopsTheRunningProcessUnderItsOwnUnit`、`TestShellReleaseSwitchRestoresCurrentAndUnitWhenTheUnitCannotBeInstalled`、`TestShellRollbackRejectsDotVersions`
- [shell_rollback_unit_promises_test.go](../../../codegen/internal/roost/shell_rollback_unit_promises_test.go)：`TestShellRollbackRunsThePreviousReleaseUnderItsOwnUnit`
- [shutdown_budget_promises_test.go](../../../codegen/internal/roost/shutdown_budget_promises_test.go)：`TestDoctorWarnsForEachRepositoryConfigThatCannotCoverTheModFloors`、`TestOneSyncConvergesAfterAServiceLosesAMod`、`TestOneSyncConvergesAfterAServiceGainsAMod`、`TestDoctorShowsTheGracePeriodTheTemplatesOnDiskSet`、`TestSyncWritesNothingWhenTheShutdownRefreshCannotBeWritten`；其余 2 项见文件
- [shutdown_budget_test.go](../../../codegen/internal/roost/shutdown_budget_test.go)：`TestGeneratedShutdownTimeoutFollowsEachServicesMods`、`TestGeneratedShutdownTimeoutIsSmallerForAServiceWithFewMods`、`TestSyncMovesAnUneditedShutdownBlockWithTheServicesMods`、`TestSyncNeverLowersTheGracePeriodBelowTheConfiguredTotal`、`TestDoctorChecksEachServicesShutdownWindow`
- [shutdown_doctor_config_promises_test.go](../../../codegen/internal/roost/shutdown_doctor_config_promises_test.go)：`TestDoctorJudgesANonPositiveTotalAsTheAppsFallback`、`TestDoctorReportsTheAppsFallbackWhenItCannotCoverTheMods`、`TestDoctorJudgesANonPositiveDataEngineBudgetAsItsFallback`、`TestDoctorAdviceFollowsTheConfiguredDataEngineBudget`、`TestDoctorAdvicePerFileWhenTheConfigsDiffer`
- [shutdown_example_parse_promises_test.go](../../../codegen/internal/roost/shutdown_example_parse_promises_test.go)：`TestDoctorNamesTheFileAndKeyOfAnExampleItCannotParse`、`TestDoctorKeepsTheAdviceForARealShortfallNextToAParseFailure`、`TestDoctorStillFailsOnAnInvalidDurationInTheDevConfig`
- [shutdown_player_tcp_budget_promises_test.go](../../../codegen/internal/roost/shutdown_player_tcp_budget_promises_test.go)：`TestGeneratedShutdownCountsThePlayerTCPStopBudget`、`TestDoctorCountsTheConfiguredPlayerTCPStopBudget`、`TestDoctorRejectsANegativePlayerTCPShutdownTimeout`、`TestSyncMovesAnUneditedBlockWrittenBeforeThePlayerTCPBudget`
- [singleton_promises_test.go](../../../codegen/internal/roost/singleton_promises_test.go)：`TestBootstrapInstallsTheSingletonStoreWhenAConfigCanTurnItOn`、`TestServiceConfigsTurnTheSingletonOnOnlyForDataEngineServices`、`TestAddingTheDataEngineLaterTurnsTheGeneratedSingletonOn`、`TestSingletonReleaseIsPartOfTheGeneratedShutdownBlock`、`TestDoctorCountsTheSingletonReleaseForAConfigThatTurnsItOn`；其余 1 项见文件
- [stage_error_messages_promises_test.go](../../../codegen/internal/roost/stage_error_messages_promises_test.go)：`TestStagedGeneratorErrorsNameTheProjectPath`、`TestDependencyFailureAfterUpgradeOrSyncPointsAtProjectDeps`
- [statslog_writable_promises_test.go](../../../codegen/internal/roost/statslog_writable_promises_test.go)：`TestGeneratedDeploymentsGiveStatsLogAWritableDirectory`
- [syncbus_migration_test.go](../../../codegen/internal/roost/syncbus_migration_test.go)：`TestConsolidationMapSyncBusLegacyPaths`、`TestConsolidationKeepsTheFilesNameForARenamedKitPackage`、`TestConsolidationReportsRemovedSymbolsInsteadOfSucceeding`、`TestConsolidationRemovedSymbolsAreReallyGone`
- [table_retirement_test.go](../../../codegen/internal/roost/table_retirement_test.go)：`TestStagedProjectCommitAndCheckSeeTableJSONRetirement`
- [table_without_csv_promises_test.go](../../../codegen/internal/roost/table_without_csv_promises_test.go)：`TestGenerateSkipsConfigDataWhenSchemaHasNoCSVYet`
- [tcp_config_boundary_promises_test.go](../../../codegen/internal/roost/tcp_config_boundary_promises_test.go)：`TestExplicitZeroTCPShutdownBudgetIsRefused`、`TestGeneratedTCPBudgetKeepsInheritance`

### codegen/internal/servicerpc

- [golden_test.go](../../../codegen/internal/servicerpc/golden_test.go)：`TestGoldenTransport`、`TestGenerationIsDeterministic`、`TestGeneratedSourceIsValidGo`、`TestWireTypesAreUnexported`、`TestAnAffinityMarkerReachesTheGeneratedClient`；其余 8 项见文件
- [halves_promises_test.go](../../../codegen/internal/servicerpc/halves_promises_test.go)：`TestEitherHalfCanBeEmittedAloneIntoAnotherDirectory`、`TestDirAcceptsAnImportPathResolvedInTheOutModule`
- [method_rules_promises_test.go](../../../codegen/internal/servicerpc/method_rules_promises_test.go)：`TestBuildServiceRefusesDuplicateMethodsAndMalformedDerivedAffinity`
- [parse_test.go](../../../codegen/internal/servicerpc/parse_test.go)：`TestParsesAnAnnotatedInterface`、`TestAnUnmarkedInterfaceIsIgnored`、`TestRefusals`、`TestParsingIsDeterministic`、`TestAnUnsafeTypeInsideAPassedStructIsRefused`；其余 15 项见文件
- [remaining_promises_test.go](../../../codegen/internal/servicerpc/remaining_promises_test.go)：`TestUnsupportedWireShapesAreRejectedAtTheNamedField`、`TestReliableMarkerCannotSilentlyPromiseAProtocol`、`TestDerivedAffinityRejectsExpressionsInsteadOfMethodNames`、`TestTicketIDsWireNameKeepsPluralAcronymTogether`、`TestAssemblyCollisionIsCheckedWhereCodeWillBeWritten`
- [retirement_test.go](../../../codegen/internal/servicerpc/retirement_test.go)：`TestRunRetiresTransportWhenInterfaceMarkerIsRemoved`、`TestRunRetiresOnlyItsAssemblyHalf`
- [split_promises_test.go](../../../codegen/internal/servicerpc/split_promises_test.go)：`TestTransportHalfImportsCoreOnlyAndAssemblyHalfOwnsTheMods`

### codegen/internal/tablegen

- [args_promises_test.go](../../../codegen/internal/tablegen/args_promises_test.go)：`TestRunNamesUnexpectedPositionalArguments`
- [check_json_promises_test.go](../../../codegen/internal/tablegen/check_json_promises_test.go)：`TestCheckJSONEnforcesTheDeclaredRules`、`TestRefDeclarationsAreResolvedAtGeneration`
- [csv_rules_test.go](../../../codegen/internal/tablegen/csv_rules_test.go)：`TestReadCSVRecordsEnforcesDeclaredRules`、`TestReadCSVRecordsSkipsTitleTypeAndRuleRows`、`TestWriteGeneratedRefusesToOverwriteWithoutForce`
- [key_case_promises_test.go](../../../codegen/internal/tablegen/key_case_promises_test.go)：`TestCheckJSONRejectsMisspelledKeys`、`TestCSVHeaderIsCaseSensitive`
- [legacy_manifest_promises_test.go](../../../codegen/internal/tablegen/legacy_manifest_promises_test.go)：`TestLegacyManifestUntrackedJSONErrorTellsRecoverySteps`
- [main_test.go](../../../codegen/internal/tablegen/main_test.go)：`TestParseMetaRootUsesTargetProjectModulePath`
- [remaining_promises_test.go](../../../codegen/internal/tablegen/remaining_promises_test.go)：`TestUnlabelledFieldsRoundTripThroughGeneratedJSON`、`TestUnknownTableKeyIsRejected`、`TestObjectCSVRejectsMultipleDataRows`、`TestGeneratedLoaderAndObjectConverterMatchSchema`
- [retirement_test.go](../../../codegen/internal/tablegen/retirement_test.go)：`TestConvertCSVToJSONRetiresOwnedDataAndPreservesManualData`、`TestConvertCSVToJSONRefusesEditedOrLegacyUnownedRetirement`
- [rules_single_source_promises_test.go](../../../codegen/internal/tablegen/rules_single_source_promises_test.go)：`TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader`
- [run_test.go](../../../codegen/internal/tablegen/run_test.go)：`TestRunReturnsFlagErrors`

### codegen/internal/webroute

- [pattern_promises_test.go](../../../codegen/internal/webroute/pattern_promises_test.go)：`TestGenerateRejectsMalformedPatternsWithoutChangingOutput`、`TestParseAcceptsChiPatterns`
- [promises_test.go](../../../codegen/internal/webroute/promises_test.go)：`TestParseFileRefusesEachBadMarkerByMessage`、`TestParseFileRefusesRawRouteWithTypedRequest`、`TestParseFileRefusesEachSignatureDefectSeparately`、`TestGenerateDirRefusesMixedPackagesInOneDirectory`
- [retirement_test.go](../../../codegen/internal/webroute/retirement_test.go)：`TestGenerateDirRetiresLastRouteWithoutRemovingManualFile`
- [signature_promises_test.go](../../../codegen/internal/webroute/signature_promises_test.go)：`TestParseFileRefusesAHandlerWhoseSecondResultIsNotError`
- [webroute_test.go](../../../codegen/internal/webroute/webroute_test.go)：`TestParseJSONRoute`、`TestParseRejectsInvalidHandler`、`TestGenerateRoutes`、`TestGenerateRejectsDuplicateRoute`

### robot/action

- [call_test.go](../../../robot/action/call_test.go)：`TestRegisterCallConventions`、`TestRegisterCallMapFieldAndValidation`、`TestParamsChain`
- [guards_promises_test.go](../../../robot/action/guards_promises_test.go)：`TestActionRegistryNilAndLookupGuards`、`TestWaitPushRequiresMsgParamAndSession`、`TestRegisterCallGuards`
- [promises_test.go](../../../robot/action/promises_test.go)：`TestActionRegistryRefusesNamelessAndDuplicateActions`

### robot

- [capture_reconnect_promises_test.go](../../../robot/capture_reconnect_promises_test.go)：`TestPushCaptureIsReinstalledOnTheSessionAfterReconnect`
- [coalescer_close_promises_test.go](../../../robot/coalescer_close_promises_test.go)：`TestCoalescerCloseReturnsAfterTheFinalFlush`
- [context_guards_promises_test.go](../../../robot/context_guards_promises_test.go)：`TestContextRefusesMissingRunnerSessionAndCaptureArguments`
- [lockstep_apply_promises_test.go](../../../robot/lockstep_apply_promises_test.go)：`TestLockstepBotPromiseTransientSinkFailureKeepsTheBatchTail`、`TestLockstepBotPromiseSimulateFailureIsTerminal`、`TestLockstepBotPromisePendingApplyIsBounded`、`TestLockstepBotPromiseRetainedFramesDoNotAliasCallerPayloads`
- [lockstep_gap_promises_test.go](../../../robot/lockstep_gap_promises_test.go)：`TestLockstepBotPromiseRequestsCatchupOnAnyUnhealedGap`、`TestLockstepBotPromiseReRequestsAnAbandonedCatchup`
- [robot_impl_test.go](../../../robot/robot_impl_test.go)：`TestKCPDialerEndToEnd`、`TestQUICDialerEndToEnd`、`TestQUICDialerRequiresALPN`、`TestLockstepBotsSurviveThirtyPercentLoss`、`TestLockstepBotRequestsCatchupOncePerGap`；其余 1 项见文件
- [robot_test.go](../../../robot/robot_test.go)：`TestPacketCodecRoundTrip`、`TestSessionCallEchoAndPush`、`TestSessionCloseFansOutToPendingAndWaiters`、`TestTypedKeyAndBlackboard`、`TestCoalescerDedupsAndFlushes`；其余 3 项见文件

### robot/loadtest

- [duration_stop_reason_promises_test.go](../../../robot/loadtest/duration_stop_reason_promises_test.go)：`TestARunCutByItsDurationStopsWithReasonDuration`、`TestARunThatFinishesBeforeItsDurationIsCompleted`
- [guards_promises_test.go](../../../robot/loadtest/guards_promises_test.go)：`TestManagerEntryPointsRefuseMissingPartsAndUnknownRuns`
- [loadtest_test.go](../../../robot/loadtest/loadtest_test.go)：`TestManagerLifecycleAndSingleActiveRun`、`TestThresholdViolationFailsRun`、`TestAdminCommandsAndReport`、`TestAdminStartGate`
- [run_series_lifecycle_promises_test.go](../../../robot/loadtest/run_series_lifecycle_promises_test.go)：`TestRunSeriesLeaveTheRegistryWithTheRunRecord`
- [threshold_no_samples_promises_test.go](../../../robot/loadtest/threshold_no_samples_promises_test.go)：`TestThresholdsWithoutSamplesFailTheRun`、`TestQuantileThresholdFailsWhenTheHistogramSeriesWasDropped`
- [threshold_verdict_promises_test.go](../../../robot/loadtest/threshold_verdict_promises_test.go)：`TestQuantileThresholdJudgesTheObservedCostsNotABucketBound`、`TestThresholdFailureNamesTheThresholdAndTheActualValue`

### robot/runner

- [runner_test.go](../../../robot/runner/runner_test.go)：`TestPoolExecutorRunsEveryRobotOnce`、`TestLoopingExecutorStopsAndCountsCanceled`、`TestStagedRampUpAndDown`、`TestArrivalRateExecutorLaunchesFreshRobots`、`TestRunnerUnknownScenarioFails`
- [stage_ordinal_promises_test.go](../../../robot/runner/stage_ordinal_promises_test.go)：`TestStageRegrowDoesNotReuseOrdinals`

### robot/scenario

- [guards_promises_test.go](../../../robot/scenario/guards_promises_test.go)：`TestRegistryAndSpecRefuseNilNodes`
- [promises_test.go](../../../robot/scenario/promises_test.go)：`TestScenarioRegistryRefusesNamelessAndDuplicateScenarios`、`TestSpecRefusesEachBrokenDocumentByMessage`
- [scenario_test.go](../../../robot/scenario/scenario_test.go)：`TestCombinators`、`TestRandomIsSeededPerRobot`、`TestSpecInterpreterRunsEquivalentTree`、`TestSpecRejectsBrokenDocuments`

### robot/session

- [session_promises_test.go](../../../robot/session/session_promises_test.go)：`TestCallReturnsWhenTheWriteBlocksPastItsContext`、`TestALateResponseIsNotDeliveredAsAPush`

### robot/transport

- [dial_bounds_promises_test.go](../../../robot/transport/dial_bounds_promises_test.go)：`TestWebSocketDialHonorsDialTimeoutAndContext`、`TestWebSocketDialStillReachesARealEndpoint`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
