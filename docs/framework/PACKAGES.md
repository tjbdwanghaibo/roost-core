# 全包源码索引

范围：当前工作树的 Go 文件，排除 docs、testdata、artifacts；包含示例、工具和平台文件，不是 go list 的构建包数量。测试列是文件数，不表示已运行的测试数。

目录已按 [分类方案](PACKAGE-REORGANIZATION.md)迁到 Framework、Infra、Gameplay、Service 与 Wiring。领域实现统一在 service；wiring 负责配置与启动接线，不保留旧 kit 或领域 alias。Gate 的实现进度见 [Gate 方案](GATEWAY-IMPLEMENTATION.md)。

<details>
<summary>展开171个包目录的职责映射</summary>

| 包路径 | 主说明 | 实现文件 | 测试文件 |
| --- | --- | ---: | ---: |
| [.](../..) | [公共基础设施](impl/14-foundation.md) | 0 | 11 |
| [client/wire](../../client/wire) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 1 | 2 |
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
| [demo](../../demo) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [examples/configgen](../../examples/configgen) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/configgen/cfg](../../examples/configgen/cfg) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/lubanreal](../../examples/lubanreal) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [examples/lubanreal/gen](../../examples/lubanreal/gen) | [其他包与公共基础设施](impl/14-foundation.md) | 3 | 0 |
| [examples/robotdemo](../../examples/robotdemo) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [framework/app](../../framework/app) | [App 与生命周期](impl/01-app-lifecycle.md) | 14 | 26 |
| [framework/app/buildinfo](../../framework/app/buildinfo) | [App 与生命周期](impl/01-app-lifecycle.md) | 1 | 1 |
| [framework/cache](../../framework/cache) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 12 | 24 |
| [framework/configdata](../../framework/configdata) | [配置、数据表与热更](impl/07-config.md) | 6 | 9 |
| [framework/configdata/rules](../../framework/configdata/rules) | [配置、数据表与热更](impl/07-config.md) | 1 | 2 |
| [framework/dataengine](../../framework/dataengine) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 10 | 10 |
| [framework/dataengine/engine](../../framework/dataengine/engine) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 18 | 41 |
| [framework/entity](../../framework/entity) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 28 | 46 |
| [framework/event](../../framework/event) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 4 | 1 |
| [framework/hotcode](../../framework/hotcode) | [配置、数据表与热更](impl/07-config.md) | 4 | 8 |
| [framework/hotcode/plugintest](../../framework/hotcode/plugintest) | [公共基础设施](impl/14-foundation.md) | 0 | 3 |
| [framework/manager](../../framework/manager) | [App 与生命周期](impl/01-app-lifecycle.md) | 2 | 9 |
| [framework/nest](../../framework/nest) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 22 | 87 |
| [framework/nestwal](../../framework/nestwal) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 11 | 22 |
| [framework/remoteentity](../../framework/remoteentity) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 26 | 108 |
| [framework/saga](../../framework/saga) | [Saga 长事务](impl/06-saga.md) | 14 | 62 |
| [framework/statslog](../../framework/statslog) | [观测、安全与运维](impl/11-observability.md) | 1 | 6 |
| [framework/sync/entitysync](../../framework/sync/entitysync) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 16 | 36 |
| [framework/sync/entitysync/policy](../../framework/sync/entitysync/policy) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 18 |
| [framework/sync/frame](../../framework/sync/frame) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 3 | 4 |
| [framework/sync/lockstep](../../framework/sync/lockstep) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 12 |
| [framework/sync/nettransport](../../framework/sync/nettransport) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 9 | 6 |
| [framework/sync/syncbus](../../framework/sync/syncbus) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 4 | 5 |
| [framework/sync/syncbus/driver](../../framework/sync/syncbus/driver) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 2 | 13 |
| [framework/sync/syncbus/mirror](../../framework/sync/syncbus/mirror) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 1 | 5 |
| [framework/sync/syncstream](../../framework/sync/syncstream) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 8 | 13 |
| [gameplay/actionflow](../../gameplay/actionflow) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 6 | 12 |
| [gameplay/ai](../../gameplay/ai) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 8 |
| [gameplay/attribute](../../gameplay/attribute) | [游戏技能、战斗与空间](impl/08-skill.md) | 2 | 2 |
| [gameplay/skill](../../gameplay/skill) | [游戏技能、战斗与空间](impl/08-skill.md) | 155 | 110 |
| [gameplay/skill/combat](../../gameplay/skill/combat) | [游戏技能、战斗与空间](impl/08-skill.md) | 5 | 2 |
| [gameplay/skill/combatcomponent](../../gameplay/skill/combatcomponent) | [游戏技能、战斗与空间](impl/08-skill.md) | 3 | 12 |
| [gameplay/skill/examples/combat](../../gameplay/skill/examples/combat) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [gameplay/skill/examples/fireball](../../gameplay/skill/examples/fireball) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [gameplay/skill/examples/statusbridge](../../gameplay/skill/examples/statusbridge) | [游戏技能、战斗与空间](impl/08-skill.md) | 1 | 0 |
| [gameplay/skill/integration/sync-e2e](../../gameplay/skill/integration/sync-e2e) | [公共基础设施](impl/14-foundation.md) | 0 | 2 |
| [gameplay/skill/skillcompose](../../gameplay/skill/skillcompose) | [游戏技能、战斗与空间](impl/08-skill.md) | 15 | 7 |
| [gameplay/skill/skillsync](../../gameplay/skill/skillsync) | [游戏技能、战斗与空间](impl/08-skill.md) | 10 | 19 |
| [infra/base/clock](../../infra/base/clock) | [时间与定时器](impl/10-time.md) | 1 | 2 |
| [infra/base/container](../../infra/base/container) | [其他包与公共基础设施](impl/14-foundation.md) | 4 | 5 |
| [infra/base/errcode](../../infra/base/errcode) | [观测、安全与运维](impl/11-observability.md) | 1 | 1 |
| [infra/base/fctx](../../infra/base/fctx) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 3 | 2 |
| [infra/base/featureflag](../../infra/base/featureflag) | [配置、数据表与热更](impl/07-config.md) | 1 | 1 |
| [infra/base/goroutine](../../infra/base/goroutine) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 6 | 8 |
| [infra/base/index](../../infra/base/index) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 2 |
| [infra/base/lifecycle](../../infra/base/lifecycle) | [App 与生命周期](impl/01-app-lifecycle.md) | 2 | 3 |
| [infra/base/lock](../../infra/base/lock) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 3 | 5 |
| [infra/base/misc](../../infra/base/misc) | [其他包与公共基础设施](impl/14-foundation.md) | 2 | 1 |
| [infra/base/safemap](../../infra/base/safemap) | [其他包与公共基础设施](impl/14-foundation.md) | 5 | 5 |
| [infra/base/security](../../infra/base/security) | [观测、安全与运维](impl/11-observability.md) | 3 | 5 |
| [infra/base/spatial](../../infra/base/spatial) | [游戏技能、战斗与空间](impl/08-skill.md) | 4 | 3 |
| [infra/base/timer](../../infra/base/timer) | [时间与定时器](impl/10-time.md) | 1 | 4 |
| [infra/base/worker](../../infra/base/worker) | [核心：Nest 调度与实体](impl/02-nest-entity.md) | 2 | 4 |
| [infra/network/bus](../../infra/network/bus) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 8 | 16 |
| [infra/network/etcd](../../infra/network/etcd) | [其他包与公共基础设施](impl/14-foundation.md) | 8 | 3 |
| [infra/network/etcd/driver](../../infra/network/etcd/driver) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 19 |
| [infra/network/gateway](../../infra/network/gateway) | [核心：Sync、Lockstep 与客户端](impl/04-sync.md) | 2 | 4 |
| [infra/network/httpclient](../../infra/network/httpclient) | [观测、安全与运维](impl/11-observability.md) | 1 | 3 |
| [infra/network/httpserver](../../infra/network/httpserver) | [观测、安全与运维](impl/11-observability.md) | 1 | 4 |
| [infra/network/nats](../../infra/network/nats) | [其他包与公共基础设施](impl/14-foundation.md) | 7 | 3 |
| [infra/network/nats/driver](../../infra/network/nats/driver) | [其他包与公共基础设施](impl/14-foundation.md) | 6 | 15 |
| [infra/network/ownerroute](../../infra/network/ownerroute) | [Remote Entity 与 Mirror](impl/05-remote-mirror.md) | 2 | 3 |
| [infra/network/servicerpc](../../infra/network/servicerpc) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 3 |
| [infra/network/webroute](../../infra/network/webroute) | [观测、安全与运维](impl/11-observability.md) | 1 | 5 |
| [infra/observe/admin](../../infra/observe/admin) | [观测、安全与运维](impl/11-observability.md) | 1 | 5 |
| [infra/observe/failurelog](../../infra/observe/failurelog) | [观测、安全与运维](impl/11-observability.md) | 1 | 6 |
| [infra/observe/health](../../infra/observe/health) | [观测、安全与运维](impl/11-observability.md) | 1 | 2 |
| [infra/observe/log](../../infra/observe/log) | [观测、安全与运维](impl/11-observability.md) | 4 | 5 |
| [infra/observe/metrics](../../infra/observe/metrics) | [观测、安全与运维](impl/11-observability.md) | 2 | 4 |
| [infra/observe/ops](../../infra/observe/ops) | [观测、安全与运维](impl/11-observability.md) | 1 | 5 |
| [infra/observe/servicemetrics](../../infra/observe/servicemetrics) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 2 |
| [infra/storage/migration](../../infra/storage/migration) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 3 |
| [infra/storage/mongo](../../infra/storage/mongo) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 8 | 0 |
| [infra/storage/mongo/driver](../../infra/storage/mongo/driver) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 4 | 9 |
| [infra/storage/mongo/mongotest](../../infra/storage/mongo/mongotest) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 2 | 14 |
| [infra/storage/redis](../../infra/storage/redis) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 9 | 4 |
| [infra/storage/redis/driver](../../infra/storage/redis/driver) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 8 | 19 |
| [infra/storage/versionstore](../../infra/storage/versionstore) | [核心：DataEngine 持久化](impl/03-dataengine.md) | 5 | 14 |
| [internal/configschema](../../internal/configschema) | [配置、数据表与热更](impl/07-config.md) | 5 | 2 |
| [internal/operation](../../internal/operation) | [其他包与公共基础设施](impl/14-foundation.md) | 2 | 2 |
| [internal/rangecontract](../../internal/rangecontract) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 0 |
| [internal/stopcontract](../../internal/stopcontract) | [App 与生命周期](impl/01-app-lifecycle.md) | 1 | 1 |
| [robot](../../robot) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 4 | 7 |
| [robot/action](../../robot/action) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 4 | 3 |
| [robot/loadtest](../../robot/loadtest) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 6 |
| [robot/protocol](../../robot/protocol) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 0 |
| [robot/runner](../../robot/runner) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 2 |
| [robot/scenario](../../robot/scenario) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 2 | 3 |
| [robot/session](../../robot/session) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 1 |
| [robot/transport](../../robot/transport) | [次核心：Codegen 与工程工具](impl/12-codegen.md) | 1 | 1 |
| [scripts/perf/nest-msg](../../scripts/perf/nest-msg) | [其他包与公共基础设施](impl/14-foundation.md) | 1 | 1 |
| [scripts/perf/sync-aoi](../../scripts/perf/sync-aoi) | [其他包与公共基础设施](impl/14-foundation.md) | 5 | 1 |
| [service/account](../../service/account) | [Service 领域能力](impl/09-services.md) | 11 | 17 |
| [service/activity](../../service/activity) | [Service 领域能力](impl/09-services.md) | 10 | 24 |
| [service/chat](../../service/chat) | [Service 领域能力](impl/09-services.md) | 8 | 12 |
| [service/directory](../../service/directory) | [Service 领域能力](impl/09-services.md) | 3 | 6 |
| [service/global](../../service/global) | [Service 领域能力](impl/09-services.md) | 7 | 7 |
| [service/mail](../../service/mail) | [次核心：Service 领域能力](impl/09-services.md) | 10 | 25 |
| [service/match](../../service/match) | [次核心：Service 领域能力](impl/09-services.md) | 9 | 13 |
| [service/platform](../../service/platform) | [Service 领域能力](impl/09-services.md) | 9 | 15 |
| [service/rank](../../service/rank) | [Service 领域能力](impl/09-services.md) | 8 | 12 |
| [service/session](../../service/session) | [次核心：Service 领域能力](impl/09-services.md) | 9 | 12 |
| [wiring](../../wiring) | [公共基础设施](impl/14-foundation.md) | 0 | 6 |
| [wiring/account](../../wiring/account) | [次核心：Service 领域能力](impl/09-services.md) | 4 | 4 |
| [wiring/activity](../../wiring/activity) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 6 |
| [wiring/chat](../../wiring/chat) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 3 |
| [wiring/configdata](../../wiring/configdata) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 3 |
| [wiring/dataengine](../../wiring/dataengine) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 13 |
| [wiring/directory](../../wiring/directory) | [次核心：Service 领域能力](impl/09-services.md) | 1 | 1 |
| [wiring/etcd](../../wiring/etcd) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 0 |
| [wiring/examples/split](../../wiring/examples/split) | [次核心：Service 领域能力](impl/09-services.md) | 4 | 4 |
| [wiring/global](../../wiring/global) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 1 |
| [wiring/integration](../../wiring/integration) | [次核心：Service 领域能力](impl/09-services.md) | 1 | 4 |
| [wiring/internal/configschemagen](../../wiring/internal/configschemagen) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 0 |
| [wiring/lock](../../wiring/lock) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 1 |
| [wiring/mail](../../wiring/mail) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 7 |
| [wiring/manager](../../wiring/manager) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 1 |
| [wiring/match](../../wiring/match) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 2 |
| [wiring/mods](../../wiring/mods) | [次核心：Wiring 装配](impl/13-wiring.md) | 6 | 5 |
| [wiring/mongo](../../wiring/mongo) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 4 |
| [wiring/nats](../../wiring/nats) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 12 |
| [wiring/nest](../../wiring/nest) | [次核心：Wiring 装配](impl/13-wiring.md) | 2 | 10 |
| [wiring/ops](../../wiring/ops) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 7 |
| [wiring/platform](../../wiring/platform) | [次核心：Service 领域能力](impl/09-services.md) | 4 | 6 |
| [wiring/rank](../../wiring/rank) | [次核心：Service 领域能力](impl/09-services.md) | 3 | 2 |
| [wiring/redis](../../wiring/redis) | [次核心：Wiring 装配](impl/13-wiring.md) | 2 | 6 |
| [wiring/remoteentity](../../wiring/remoteentity) | [次核心：Wiring 装配](impl/13-wiring.md) | 3 | 11 |
| [wiring/saga](../../wiring/saga) | [次核心：Wiring 装配](impl/13-wiring.md) | 3 | 7 |
| [wiring/session](../../wiring/session) | [次核心：Service 领域能力](impl/09-services.md) | 4 | 4 |
| [wiring/statslog](../../wiring/statslog) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 1 |
| [wiring/syncbus](../../wiring/syncbus) | [次核心：Wiring 装配](impl/13-wiring.md) | 1 | 7 |

</details>
