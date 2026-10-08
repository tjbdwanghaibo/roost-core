# v1.24.0 全包源码索引

范围：Git 跟踪的 Go 文件，排除 docs、testdata；包含示例/工具/平台文件，不是 go list 的平台构建包数，也不是语义 review 覆盖率。测试列为文件数量，不是已运行测试数。每包实现文件、导出类型和回归名字在所属实现篇。

| 包路径 | 主说明 | 实现文件 | 测试文件 |
| --- | --- | ---: | ---: |
| [actionflow](../../actionflow) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 6 | 12 |
| [admin](../../admin) | [观测、安全与运维](impl/11-observability.md) | 1 | 5 |
| [ai](../../ai) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 8 |
| [app](../../app) | [App 与生命周期](impl/01-app-lifecycle.md) | 14 | 26 |
| [app/buildinfo](../../app/buildinfo) | [App 与生命周期](impl/01-app-lifecycle.md) | 1 | 1 |
| [attribute](../../attribute) | [游戏技能、战斗与空间](impl/08-skill.md) | 2 | 2 |
| [bus](../../bus) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 8 | 16 |
| [cache](../../cache) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 12 | 24 |
| [client/wire](../../client/wire) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 1 | 2 |
| [clock](../../clock) | [时间与定时器](impl/10-time.md) | 1 | 2 |
| [cmd/glsvet](../../cmd/glsvet) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 4 | 6 |
| [cmd/walinspect](../../cmd/walinspect) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [codegen/cmd/attribute](../../codegen/cmd/attribute) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/cfggen](../../codegen/cmd/cfggen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/dao](../../codegen/cmd/dao) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/entity](../../codegen/cmd/entity) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/errcode](../../codegen/cmd/errcode) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/eventgen](../../codegen/cmd/eventgen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/nest](../../codegen/cmd/nest) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/project](../../codegen/cmd/project) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/protocol](../../codegen/cmd/protocol) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/roost](../../codegen/cmd/roost) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/servicerpc](../../codegen/cmd/servicerpc) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/tablegen](../../codegen/cmd/tablegen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/cmd/webroute](../../codegen/cmd/webroute) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/internal/attribute](../../codegen/internal/attribute) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 5 | 5 |
| [codegen/internal/cfggen](../../codegen/internal/cfggen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 8 |
| [codegen/internal/dao](../../codegen/internal/dao) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 8 | 10 |
| [codegen/internal/entity](../../codegen/internal/entity) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 8 | 16 |
| [codegen/internal/errcode](../../codegen/internal/errcode) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 4 |
| [codegen/internal/eventgen](../../codegen/internal/eventgen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 6 | 7 |
| [codegen/internal/genutil](../../codegen/internal/genutil) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 0 |
| [codegen/internal/marker](../../codegen/internal/marker) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 2 |
| [codegen/internal/nest](../../codegen/internal/nest) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 5 | 10 |
| [codegen/internal/project](../../codegen/internal/project) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 2 |
| [codegen/internal/protocol](../../codegen/internal/protocol) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 9 | 7 |
| [codegen/internal/registry](../../codegen/internal/registry) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 3 | 4 |
| [codegen/internal/roost](../../codegen/internal/roost) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 41 | 75 |
| [codegen/internal/roost/cmd/attributeruntime](../../codegen/internal/roost/cmd/attributeruntime) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [codegen/internal/servicerpc](../../codegen/internal/servicerpc) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 6 | 7 |
| [codegen/internal/tablegen](../../codegen/internal/tablegen) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 10 |
| [codegen/internal/webroute](../../codegen/internal/webroute) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 3 | 5 |
| [configdata](../../configdata) | [配置、数据表与热更](impl/07-config.md) | 6 | 9 |
| [configdata/rules](../../configdata/rules) | [配置、数据表与热更](impl/07-config.md) | 1 | 2 |
| [container](../../container) | [其他包与公共基础设施](impl/14-foundation.md) | 4 | 5 |
| [dataengine](../../dataengine) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 10 | 10 |
| [dataengine/engine](../../dataengine/engine) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 18 | 41 |
| [demo](../../demo) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [entity](../../entity) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 28 | 46 |
| [errcode](../../errcode) | [观测、安全与运维](impl/11-observability.md) | 1 | 1 |
| [etcd](../../etcd) | [其他包与公共基础设施](impl/14-foundation.md) | 8 | 3 |
| [etcd/driver](../../etcd/driver) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 19 |
| [event](../../event) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 4 | 1 |
| [examples/configgen](../../examples/configgen) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/configgen/cfg](../../examples/configgen/cfg) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/lubanreal](../../examples/lubanreal) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/lubanreal/gen](../../examples/lubanreal/gen) | [其他包与公共基础设施](impl/14-foundation.md) | 3 | 0 |
| [examples/robotdemo](../../examples/robotdemo) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [failurelog](../../failurelog) | [观测、安全与运维](impl/11-observability.md) | 1 | 6 |
| [fctx](../../fctx) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 3 | 2 |
| [featureflag](../../featureflag) | [配置、数据表与热更](impl/07-config.md) | 1 | 1 |
| [gateway](../../gateway) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 2 | 4 |
| [goroutine](../../goroutine) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 6 | 8 |
| [health](../../health) | [观测、安全与运维](impl/11-observability.md) | 1 | 2 |
| [hotcode](../../hotcode) | [配置、数据表与热更](impl/07-config.md) | 4 | 8 |
| [httpclient](../../httpclient) | [观测、安全与运维](impl/11-observability.md) | 1 | 3 |
| [httpserver](../../httpserver) | [观测、安全与运维](impl/11-observability.md) | 1 | 4 |
| [index](../../index) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 2 |
| [internal/configschema](../../internal/configschema) | [配置、数据表与热更](impl/07-config.md) | 5 | 2 |
| [internal/operation](../../internal/operation) | [其他包与公共基础设施](impl/14-foundation.md) | 2 | 2 |
| [internal/rangecontract](../../internal/rangecontract) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [internal/stopcontract](../../internal/stopcontract) | [App 与生命周期](impl/01-app-lifecycle.md) | 1 | 1 |
| [kit/configdata](../../kit/configdata) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 3 |
| [kit/dataengine](../../kit/dataengine) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 13 |
| [kit/etcd](../../kit/etcd) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 0 |
| [kit/internal/configschemagen](../../kit/internal/configschemagen) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 0 |
| [kit/lock](../../kit/lock) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 1 |
| [kit/manager](../../kit/manager) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 1 |
| [kit/mods](../../kit/mods) | [次核心：Kit 装配](impl/13-kit.md) | 6 | 5 |
| [kit/mongo](../../kit/mongo) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 4 |
| [kit/nats](../../kit/nats) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 12 |
| [kit/nest](../../kit/nest) | [次核心：Kit 装配](impl/13-kit.md) | 2 | 10 |
| [kit/ops](../../kit/ops) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 11 |
| [kit/redis](../../kit/redis) | [次核心：Kit 装配](impl/13-kit.md) | 2 | 6 |
| [kit/remoteentity](../../kit/remoteentity) | [次核心：Kit 装配](impl/13-kit.md) | 3 | 11 |
| [kit/saga](../../kit/saga) | [次核心：Kit 装配](impl/13-kit.md) | 3 | 7 |
| [kit/service/account](../../kit/service/account) | [次核心：Service 领域能力](impl/09-kit-services.md) | 12 | 20 |
| [kit/service/chat](../../kit/service/chat) | [次核心：Service 领域能力](impl/09-kit-services.md) | 9 | 13 |
| [kit/service/directory](../../kit/service/directory) | [次核心：Service 领域能力](impl/09-kit-services.md) | 4 | 7 |
| [kit/service/examples/split](../../kit/service/examples/split) | [次核心：Service 领域能力](impl/09-kit-services.md) | 4 | 4 |
| [kit/service/global](../../kit/service/global) | [次核心：Service 领域能力](impl/09-kit-services.md) | 8 | 8 |
| [kit/service/global/activity](../../kit/service/global/activity) | [次核心：Service 领域能力](impl/09-kit-services.md) | 11 | 28 |
| [kit/service/integration](../../kit/service/integration) | [次核心：Service 领域能力](impl/09-kit-services.md) | 1 | 4 |
| [kit/service/mail](../../kit/service/mail) | [次核心：Service 领域能力](impl/09-kit-services.md) | 4 | 7 |
| [kit/service/match](../../kit/service/match) | [次核心：Service 领域能力](impl/09-kit-services.md) | 4 | 2 |
| [kit/service/platform](../../kit/service/platform) | [次核心：Service 领域能力](impl/09-kit-services.md) | 10 | 17 |
| [kit/service/rank](../../kit/service/rank) | [次核心：Service 领域能力](impl/09-kit-services.md) | 9 | 14 |
| [kit/service/servicemetrics](../../kit/service/servicemetrics) | [次核心：Service 领域能力](impl/09-kit-services.md) | 1 | 0 |
| [kit/service/session](../../kit/service/session) | [次核心：Service 领域能力](impl/09-kit-services.md) | 5 | 4 |
| [kit/statslog](../../kit/statslog) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 5 |
| [kit/syncbus](../../kit/syncbus) | [次核心：Kit 装配](impl/13-kit.md) | 1 | 7 |
| [lifecycle](../../lifecycle) | [App 与生命周期](impl/01-app-lifecycle.md) | 2 | 3 |
| [lock](../../lock) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 3 | 5 |
| [log](../../log) | [观测、安全与运维](impl/11-observability.md) | 4 | 5 |
| [manager](../../manager) | [App 与生命周期](impl/01-app-lifecycle.md) | 2 | 9 |
| [metrics](../../metrics) | [观测、安全与运维](impl/11-observability.md) | 2 | 4 |
| [migration](../../migration) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 3 |
| [misc](../../misc) | [其他包与公共基础设施](impl/14-foundation.md) | 2 | 1 |
| [mongo](../../mongo) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 8 | 0 |
| [mongo/driver](../../mongo/driver) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 4 | 9 |
| [mongo/mongotest](../../mongo/mongotest) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 2 | 14 |
| [nats](../../nats) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 3 |
| [nats/driver](../../nats/driver) | [其他包与公共基础设施](impl/14-foundation.md) | 6 | 15 |
| [nest](../../nest) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 22 | 87 |
| [nestwal](../../nestwal) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 11 | 22 |
| [ownerroute](../../ownerroute) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 2 | 3 |
| [redis](../../redis) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 9 | 4 |
| [redis/driver](../../redis/driver) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 8 | 19 |
| [remoteentity](../../remoteentity) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 26 | 108 |
| [robot](../../robot) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 4 | 7 |
| [robot/action](../../robot/action) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 4 | 3 |
| [robot/loadtest](../../robot/loadtest) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 6 |
| [robot/protocol](../../robot/protocol) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [robot/runner](../../robot/runner) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 2 |
| [robot/scenario](../../robot/scenario) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 3 |
| [robot/session](../../robot/session) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 1 |
| [robot/transport](../../robot/transport) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 1 |
| [safemap](../../safemap) | [其他包与公共基础设施](impl/14-foundation.md) | 5 | 5 |
| [saga](../../saga) | [Saga 长事务](impl/06-saga.md) | 14 | 62 |
| [scripts/perf/nest-msg](../../scripts/perf/nest-msg) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 1 |
| [scripts/perf/sync-aoi](../../scripts/perf/sync-aoi) | [其他包与公共基础设施](impl/14-foundation.md) | 5 | 1 |
| [security](../../security) | [观测、安全与运维](impl/11-observability.md) | 3 | 5 |
| [service/mail](../../service/mail) | [次核心：Service 领域能力](impl/09-kit-services.md) | 8 | 25 |
| [service/match](../../service/match) | [次核心：Service 领域能力](impl/09-kit-services.md) | 7 | 12 |
| [service/session](../../service/session) | [次核心：Service 领域能力](impl/09-kit-services.md) | 7 | 12 |
| [servicemetrics](../../servicemetrics) | [次核心：Service 领域能力](impl/09-kit-services.md) | 3 | 2 |
| [servicerpc](../../servicerpc) | [次核心：Service 领域能力](impl/09-kit-services.md) | 3 | 3 |
| [skill](../../skill) | [游戏技能、战斗与空间](impl/08-skill.md) | 155 | 110 |
| [skill/combat](../../skill/combat) | [游戏技能、战斗与空间](impl/08-skill.md) | 5 | 2 |
| [skill/combatcomponent](../../skill/combatcomponent) | [游戏技能、战斗与空间](impl/08-skill.md) | 3 | 12 |
| [skill/examples/combat](../../skill/examples/combat) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [skill/examples/fireball](../../skill/examples/fireball) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [skill/examples/statusbridge](../../skill/examples/statusbridge) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [skill/skillcompose](../../skill/skillcompose) | [游戏技能、战斗与空间](impl/08-skill.md) | 15 | 7 |
| [skill/skillsync](../../skill/skillsync) | [游戏技能、战斗与空间](impl/08-skill.md) | 10 | 19 |
| [spatial](../../spatial) | [游戏技能、战斗与空间](impl/08-skill.md) | 4 | 3 |
| [sync/entitysync](../../sync/entitysync) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 16 | 36 |
| [sync/entitysync/policy](../../sync/entitysync/policy) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 18 |
| [sync/frame](../../sync/frame) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 3 | 4 |
| [sync/lockstep](../../sync/lockstep) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 12 |
| [sync/nettransport](../../sync/nettransport) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 9 | 6 |
| [sync/syncbus](../../sync/syncbus) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 4 | 5 |
| [sync/syncbus/driver](../../sync/syncbus/driver) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 2 | 13 |
| [sync/syncbus/mirror](../../sync/syncbus/mirror) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 1 | 5 |
| [syncstream](../../syncstream) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 13 |
| [timer](../../timer) | [时间与定时器](impl/10-time.md) | 1 | 4 |
| [versionstore](../../versionstore) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 5 | 14 |
| [webroute](../../webroute) | [观测、安全与运维](impl/11-observability.md) | 1 | 5 |
| [worker](../../worker) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 2 | 4 |
