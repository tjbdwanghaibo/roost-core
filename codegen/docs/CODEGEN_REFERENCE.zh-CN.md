# roost-codegen 命令与标记参考

本文是 `roost` 命令、全部生成器的标记 / tag / 参数与用例的参考。业务项目默认由 Makefile/CI 调用 `@latest` 的 `roost`，独立命令主要用于调试或只采用部分框架能力的仓库。

同组文档的分工：[README](../README.md) 是入口与快速上手；[项目生成器](PROJECT_GENERATOR.zh-CN.md) 讲工程生成、`roost.yaml`、Mod 装配、升级与部署生成物；[实现与维护](../../docs/framework/impl/12-codegen.md) 讲生成器的实现与必须保持的不变量。本文只写"怎么写、怎么调用、会被怎样拒绝"。

```bash
go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest
roost help
```

生成器共同原则：输入是唯一事实来源；生成文件带 `Code generated` 标识；内容未变化时不改 mtime；标记、ID、签名或 tag 不合法时 fail-fast；不要手改生成文件。框架更新策略默认是 `latest`，但 `go.mod/go.sum` 始终保存一次解析得到的具体版本，根 `go.mod` 不保留 `replace`。

## 0. 内置帮助系统

安装完成后运行：

```bash
roost help                  # 能力目录
roost help <capability>     # 指定能力的配置与例子
roost help all              # 全部专题
roost <command> --help      # 上下文帮助
```

可查询的 capability：`environment`、`beginner`、`project`、`versions`、`framework-release`、`generate`、`add`、`mods`、`access`、`transport`、`lifecycle`、`endpoint`、`skill`、`services`、`dao`、`entity`、`nest`、`protocol`、`cfggen`、`tablegen`、`attribute`、`webroute`、`errcode`、`saga`、`effect`、`replication`、`config`、`id`、`format`、`deploy`。专题输出统一包含用途、命令、配置/marker 和可复制的最小示例。

框架发布维护者使用 `roost framework verify` 校验 `ci/framework-release.yaml` 的精确版本集合并生成 `framework-lock.json`。该命令拒绝 `latest`、replace 和框架内部伪版本依赖；具体发布顺序与 GitHub Environment 配置见 `docs/CI_CD_IMPLEMENTATION.zh-CN.md`。

```bash
roost help dao
roost help replication
roost help transport
roost env doctor
roost project deps --help
roost add saga --help
```

生成项目的 Makefile 更新入口（`project-upgrade` / `deps-update` / `roost-up` / `codegen-up`）与版本策略见 [项目生成器 §2](PROJECT_GENERATOR.zh-CN.md#2-安装和运行)。

## 1. 项目生成器：roost project

### 1.1 创建项目

```bash
roost project new planet \
  -module example.com/planet \
  -out ./planet \
  -services game,gate \
  -mods configdata,etcd,redis,mongo,nats,syncbus,remote_entity,dataengine,nest \
  -features protocol,config,entity,nest,dao,errcode
```

Data Engine 是唯一持久化引擎；即使只指定 `nest` 或 `saga`，生成器也会自动补齐
`dataengine`、Mongo 与 NATS。工程会把 `WithEntityAccess`（Remote 开启时同时加
`WithRemoteProjection`）写入 bootstrap；`checkpoint` 和 standalone `nestwal` 已不是可选 Mod。

主要产物：`roost.yaml`、main/bootstrap/Service、配置、Makefile、CI、开发 compose，以及 Shell、Docker、Kubernetes 生产部署模板。Secret 示例和业务文件归应用所有，后续 sync 不覆盖；带生成标识的装配文件由生成器管理。

新项目随附的使用文档清单见 [项目生成器 §18](PROJECT_GENERATOR.zh-CN.md#18-生成项目里的文档)。

第一次只验证生成流程可使用较轻的项目：

```bash
roost project new hello_roost -module example.com/hello_roost \
  -mods configdata \
  -features protocol,config,entity,nest,dao,errcode
```

### 1.2 同步、预览和诊断

```bash
roost project diff                 # 预览 sync 将改写的模板与配置 shutdown: 块（不含生成器输出）
roost project sync                 # 按 roost.yaml 更新生成文件
roost project doctor               # 含生成物新鲜度检查
roost project doctor -strict=false # 只检查结构、版本和配置
roost project doctor -json         # CI/平台读取 JSON
roost project doctor --workflow first-business # 检查首条业务链完整性并给出修复命令
roost project next                 # 根据真实进度只显示一个当前动作
roost project next --workflow player-tcp
```

只更新依赖：

```bash
roost project deps
```

配置段：服务配置建工程时生成一次、之后归应用；`add mod` / `add saga` / `add access` / `add rpc` 让服务多出 Mod 时追加缺的顶层段，手改 `roost.yaml` 后 `project sync` 不追加。doctor 的 `config-sections:<service>` 只读检查三份配置（dev、prod 示例、k8s Secret 示例）是否缺服务实际注册的 Mod 的段（带 DataEngine 的服务另查 `redis:` / `singleton:` 与 `singleton.enabled: true`），缺了报 WARN 并给出可直接追加的文本；有意删掉的段同样会报，可以忽略，或留空段（如 `saga: {}`）消掉提示。检查项清单见 [项目生成器 §13](PROJECT_GENERATOR.zh-CN.md#13-doctordiff-和升级)。

sync / deps 的暂存与提交语义（同级临时目录、逐文件原子替换与回滚、并发修改拒绝覆盖、只解析 roost-core 一个模块）见 [项目生成器 §13](PROJECT_GENERATOR.zh-CN.md#13-doctordiff-和升级)。

### 1.3 升级项目模板

```bash
roost project upgrade --root . --dry-run -core latest   # 只预览受控文件差异
roost project upgrade --root . -core latest             # 更新 roost.yaml 版本策略、受控模板和框架依赖
roost project upgrade -core vX.Y.Z                      # 提高 Core 下限（不得低于生成器兼容下限）
```

升级可重复执行，业务文件不覆盖，没有生成标识的自定义 Makefile 直接报冲突。合仓前的旧工程（仍 require roost-skill / roost-service / roost-kit / roost-codegen 或 import 旧包路径）不能自动升级，按当前模板重新生成或人工迁移；`versions.kit / skill / service / codegen` 等已退役写法会在加载时被点名拒绝，需手工删除（清单见 [项目生成器 §4](PROJECT_GENERATOR.zh-CN.md#4-roostyaml)）。

## 2. 统一生成流水线：roost generate

默认顺序为 DAO → Errcode → Protocol → Entity → Nest → Attribute → Config（template → data → go）→ WebRoute → ServiceRPC，避免后一个生成器读取旧产物；每一步只在 `roost.yaml` 启用了对应 feature 时运行。
普通生成在同级临时项目内完成全部生成器与 `GOWORK=off go mod tidy`，补齐新生成 import 的依赖与
checksum；只有全部成功才以可回滚批次提交生成物和 `go.mod/go.sum`。后置生成器、tidy 或提交失败
不会留下半生成工程；它不执行 `go get -u`，不会借机升级框架。

```bash
roost generate                  # 全量增量生成
roost generate --changed        # 只运行受 git 变更影响的生成器
roost generate --dry-run        # 打印计划，不写文件
roost generate --check          # 在临时副本生成并检查仓库是否最新
roost generate --force          # 强制重写生成物
```

CI 推荐：

```bash
roost generate --check
roost config check --service game --production \
  --file configs/service/config.game.prod.yaml
roost id check
roost format check
go test ./...
```

## 3. 业务骨架：roost add

支持 `service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|table|dao|webroute|errcode|saga|effect|rpc|timer`。

```bash
roost add service world -mods etcd,redis,mongo,nats,dataengine,nest
roost add mod saga -service game
roost add access player --service game
roost add transport tcp
roost add module inventory
roost add protocol use_item -group game --handler inventory
roost add entity player
roost add component inventory --entity player
roost add mod nest -service game
roost add handler use_item --entity player --component inventory
roost add endpoint use_item --handler inventory
roost add lifecycle player --service game
roost add endpoint enter_game --handler player --lifecycle player
roost add skill fireball
roost add table monster
roost add dao player --entity player
roost add webroute gm_query
roost add errcode item_not_found
roost add saga cross_server_trade -service game -steps reserve,deduct,deliver
roost add saga gift -service game -steps debit,credit,audit -native debit,credit -entity player -component wallet
roost add effect player.level_up -consumer game
roost add rpc guild -service game
```

`add entity` 的骨架默认是本地实体；改成 Remote 托管要一起改三处（骨架注释写明）：`category=entity.EntityCategoryRemote`、标记加 `remote=managed`、嵌入改为 `*entity.RemoteEntityBase`，并给写它的 Nest 服务加 `remote_entity` Mod（`roost add mod remote_entity -service <service>`；`generate` / doctor 缺时拒绝并给出这条命令）。

`add saga` 的 `-steps` 按执行顺序书写：生成的 `Definition()` 原样保留这个顺序（Saga 按下标推进、按下标倒序补偿），重复的步骤名（含只差大小写、蛇形化后相同的名字）直接报错；`-mods`、`-features` 这类集合参数才会去重排序。

`add saga` 的生成物都是业务文件、只建一次（再次 add 同名 saga 报 already exists，任何校验失败都不写文件）：

| 文件 | 内容 |
| --- | --- |
| `saga/<name>/definition.go` | `Type` / `Version`、步骤主题常量 `Topic<Step>` / `Topic<Step>Compensation`、`Definition()` / `Definitions()`、`Register(engine)`、`EmitStart`、`Start`（不再生成 `Subscribe<Step>` 辅助：步骤消费者只有 `steps.go` 一种写法） |
| `saga/<name>/steps.go` | `StepsGroup = "<service>-<name>"`（durable 前缀，上线后不要改名）、`StepDeps`、`State` + `EncodeState` / `DecodeState`、每步正向与补偿的步骤体、`RegisterSteps(steps, deps)`（按 `-steps` 顺序与主题常量登记：`-native` 的 `Steps.Native`，其余 `Steps.Mongo`；`WithAdmit` 留空并注释多进程分片要补）、`StartSteps(ctx, registry, deps)`；注释写明单进程容量 |
| `game/handler/<name>_<step>.go`、`<name>_<step>_compensation.go` | 每个 `-native` 步骤两个 Nest handler：`func handler<Name><Step>(target <entity>.I<Component>Entity, step saga.NativeStep) error`，事务里 `step.Complete`，业务拒绝也提交 |

`-native` 保序解析，必须是 `-steps` 的子集、不能重复；锁目标 `-entity` / `-component` 与 `add handler` 相同（工程只有一个 Entity 时 `-entity` 可省），跑步骤的服务要有 nest Mod，与已有 Nest handler 同名时拒绝。骨架默认拒绝（`... is not implemented`），写完业务前 saga 不报告成功。服务的 `Init` / `Shutdown` 不改写：next 提示给出 `steps, err := <name>.StartSteps(context.Background(), registry, <name>.StepDeps{})` 与 `steps.Drain(ctx)` 两行，doctor 的 `saga-steps:<name>` 在没有服务调用 `StartSteps` / `RegisterSteps`（或所在包没有 `Steps.Drain(ctx)`）时 WARN。

容量（2026-10-18 实测，投影 checkpoint 缺省间隔 20 ms）：步骤消费者的回调只做预留与 Nest 请求、等投影回执在后台；DataEngine 投影器逐条处理带回执 / effect 的记录，约 20 条/s（正向每笔 2 条、全补偿每笔 3 条）。单进程两步原生步骤 saga 正向约 8/s、全补偿约 5/s 以内 P50 在 1 秒内（P99 0.8 / 1.2 s）；正向 10/s 仍 0 笔终态不对，但积压、延迟升到秒级（P50 2.2 s）；正向 12/s、全补偿 7/s 起步骤超时、重试放大而崩溃（上限档各单样本）。量大时按 sid 分片多进程并给原生步骤加 `kitsaga.WithAdmit` 准入。数据见 [投影器合并 checkpoint 前后对照](../../docs/performance/SAGA-CHECKPOINT-20261018.md) §3、§5（优化过程见 [Saga 性能优化](../../docs/performance/SAGA-OPT-20261018.md)）。

`add effect <topic> -consumer <service>`：`<topic>` 点分（`player.level_up`，`saga.*` 保留）。生成 `game/effects/<topic>.go`（载荷 struct、`Topic<Topic>`、`Emit<Topic>(key, event)`，只能在 Nest handler 里调用）与 `internal/service/<service>/<topic>_consumer.go`（`start<Topic>EffectConsumer(ctx, registry)`：`kitdataengine.SubscribeEffects`，durable `<service>-<topic>`，载荷解不开返回 `PoisonEffect`，骨架默认返回错误让 effect 重投）。载荷里的玩家号在组件里用 `Owner().UniqueID()` 取；`Owner().ID()` 是编码后的 EntityID（带实体类型位），不是玩家号，骨架注释写明了这一点。消费服务要有 dataengine Mod；effect 文件已存在时只建新消费服务的消费者。服务里加 `subscription, err := start<Topic>EffectConsumer(context.Background(), registry)` 与 `subscription.Drain()`；doctor 的 `effect-consumer:<service>/<func>` 在没人调用时 WARN。`project new -services` 同样保留书写顺序，`-template` 把第一个写出的服务接到托管框架服务上。

需要编号的类型默认从 `roost.yaml` 的 ID 空间分配；`-id` 可显式指定，`-group` 选择协议分组。Component 使用 `--entity` 后会在所属 Entity 包生成工厂、窄 Entity 接口，并自动添加 ComponentManager 与字段 tag；项目只有一个 Entity 时可自动推断。DAO 使用 `--entity` 后会自动添加 import、DaoManager 和 dao tag。Entity wire 统一生成 Component/DAO getter。业务只需补领域字段、Component 方法与 Saga/Nest handler。

新增的首层业务命令不是 preset，彼此可独立组合：

| 命令 | 必需参数 | 生成/修改 | 下一步 |
| --- | --- | --- | --- |
| `add access player` | 多 Service 时 `--service` | roost.yaml、playeraccess、access Mod、bootstrap；给 Service 的两份配置（及 k8s Secret 示例）补上 nest 带入的 nest / dataengine / mongo / nats 段，打开单实例锁并补 redis 段（与 add mod 同一规则，只加缺失的顶层段） | 添加协议；需要真实 TCP 时 add transport；启动前把这些段里的地址改成自己的环境 |
| `add transport tcp` | 已存在 access.player | TCP server/Session/主动发布/test、应用 auth.go、自动合并 disabled 配置、bootstrap | 实现鉴权、config enable、player-tcp doctor |
| `add lifecycle <entity>` | 多 Entity 时 `--entity` | `game/lifecycle/<entity>.go` | 登录/创角边界从 Registry 构造 |
| `add endpoint <name>` | `--handler <controller-domain>` | controller 与 endpoint | `roost generate` 生成 typed binding |
| `add endpoint <name> --lifecycle <entity>` | `--handler`、已有同名 protocol 与 `add lifecycle` | 登录 endpoint（调 lifecycle `GetOrCreate`，不走 Nest） | 客户端首条请求先发它 |
| `add skill <name>` | 无 | 中性 JSON；首次生成 CompileAll catalog 与 catalog_test.go（`go test ./game/skills/` 编译全部定义，doctor `skills:catalog` 运行它） | 配置 CompileEnvironment 与 Host；服务 Init 调 CompileAll |
| `add timer --entity <entity>` | 实体已有 DAO、工程有 nest；多 Nest 服务时 `--service` | roost.yaml `timers`、DAO 定义加两个字段、实体加组件字段；生成组件 / 节点类型 / Tick handler / bootstrap 驱动；业务回调文件 | 在 `<component>_handlers.go` 写类型与回调，在 handler 里 `NewTimer` |

endpoint 默认把同名 protocol 接到同名 Nest handler；名字不同时使用 `--protocol` 与
`--nest-handler`。`add protocol --handler <域>` 之后跑过 `roost generate`，协议生成器会先写一个“未实现”占位
（`Handle<Protocol>` 只返回 `fmt.Errorf("Handle<Protocol>: not implemented")`）；add endpoint 认出**仍未改动**的占位并在同一次
提交里替换它。占位被改过（多了语句、换了返回值、加了别的函数）或是别的文件时不覆盖，报错写明“删掉或改名（保留要留的代码）后重跑”。第一个 Nest 参数必须是 Entity 接口，其余命名参数按 snake_case 对应 Request
字段；找不到字段会在 add 阶段报错，不生成半成品。**业务时间是服务器决定的值**（新用户试用第四轮登记项 2）：参数名
`nowUnix` / `nowUnixMilli`（`int64`）不对应 Request 字段，骨架先取 `businessNow := app.BusinessClock(controller.Registry()).Now()`
（真实时间 + `time.logic_offset`，与 game-demo 的 `BusinessNow()` 同一来源）再传 `businessNow.Unix()` / `.UnixMilli()`；
请求里有同名字段也不读。报错文案写明服务器决定的值不放进请求——放进去等于让客户端延长限时效果、提前领冷却奖励。
其他服务器决定的值（玩家自己的状态、配置）在 handler 里读，不做参数。用到业务时间时 controller 需要 `Registry()`
（新生成的都带；早先的缺少时 add 报错说明要补的方法）。handler 在事务里仍应按执行时刻复核期限
（排队等锁期间时间在走，见 game-demo `executionNowUnix`）。handler 返回值时（如 `(player.BuyResult, error)`），
骨架用 `result, err := sender.Sync_Xxx(...)` 接住全部返回值，并留一行 `_ = result` 提示把结果填进响应。
骨架按 demo 的边界写法处理错误：接入层把 Controller 返回的 error 当坏帧断开连接，所以 handler 错误
经 `errcode.ClientError` 变成 `&pb.XxxResponse{Code: code, Reason: reason}, nil`，只有
`errcode.CodeInternal`（没有业务码的错误）记 `slog.Error`；只有缺 context / request 或构造 Entity ID
失败才返回 error。因此 Response 必须有 `Code int32` 与 `Reason string`（`roost add protocol` 默认带上），
缺少时 add 阶段报错。

`--lifecycle <entity>` 生成登录边界：endpoint 经 `lifecycle.<Entity>FromRegistry(controller.Registry())`
对 `context.PlayerID` 调 `GetOrCreate`，保证之后的玩法请求打到的 Entity 已存在。从未登录的玩家直接发玩法请求时，
Nest 冷加载在 Data Engine 里找不到 Entity，回复带稳定码 `errcode.CodeEntityNotFound = 2`（`entity_not_found`），
客户端据此先 EnterGame 再重试，服务端不按 ERROR 记录。删过档的玩家（DAO 只剩墓碑）该 ID 不可再用：玩法请求与
EnterGame 都回复 `errcode.CodeEntityDeleted = 3`（`entity_deleted`），`GetOrCreate` 不重建（DESIGN-TODO D10）。
生成 lifecycle 的 `Create` 不读存储：只给 id-gen 发的新 ID 用，复用删过档的 ID 会在投影时版本冲突、进程 fail-stop。controller 需要 `Registry()`：新生成的 controller（`add endpoint`
与 protocol 生成器两处骨架）都带；早先生成的 controller 归业务、不改写，缺少时 add 报错并说明要补的方法。
`roost project doctor --workflow first-business` 的 `workflow:login` 检查是否有 controller 导入 `game/lifecycle`
并调用 `GetOrCreate`。完整顺序与编辑点见生成项目的
`docs/FIRST_BUSINESS.zh-CN.md`，边界契约见 `docs/PROTOCOL_TO_NEST.zh-CN.md`，可运行 TCP 接入与帧协议见
`docs/PLAYER_ACCESS_TCP.zh-CN.md`。

**玩家看自己的状态（`access.player.self_sync: true`）**：没有 add 命令，直接在 roost.yaml 的 `access.player`
下写 `self_sync: true`，再 `roost project sync`、`roost generate`。生成器改写的只有生成器所有的文件——
`internal/bootstrap/generated.go`（`PlayerSelfSync` 变量、`kitnest.NewModWithEntitySync(EntityAccess,
PlayerSelfSync.EntitySyncSetup())`、`accessplayertcp.NewMod().WithSelfSync(PlayerSelfSync)`）与
`internal/access/player/tcp/server_gen.go`（`Mod.WithSelfSync` / `IngressMod.WithSelfSync`）；推送编号
`protocol/def/self_sync.go`（`SelfSync()`，取所在协议组上限的 id）是业务文件，只在缺失时创建。前置检查
（`project sync`、`generate`、`doctor` 的 `self-sync` 项）要求：access.player 有 tcp 传输、features 含 entity、
`game/entities/player/entity.go` 存在、推送编号文件存在。Player 的 `//roost:entity` 标记不用改：开关打开时 entity
生成器（`-self-sync Player`）按 `sync=true`、`syncNamespace="player"` 生成；没写 `subjectPacker=` 时写默认 packer
`game/entities/player/player_sync_packer_gen.go`（生成器所有，`New<Name>SyncPacker`，视图集只有 `policy.SelfProfile`，
包含 Player 唯一 DAO 的全部 sync 字段，字段表运行期从 DAO 取；未知视图返回 `entity.ErrSyncViewUnknown`），wire 的
`PackerFactory` 指向它。代价：标了 sync 的字段默认全部对玩家本人可见。要裁剪字段或别的视图（AOI 他人视图、按字段
权限）时自己写 packer 并在标记写 `subjectPacker=`：生成器不再生成、删掉以前生成的那份（同名业务文件没有生成头，
不动、也不覆盖）；去掉 `subjectPacker=` 后它回来。拒绝并写出改法：没有 DAO（`roost add dao Player --entity Player`）、
多个 DAO 且没写 `subjectPacker=`、显式 `sync=false`；DAO 没有 sync 字段时 doctor 给 warn、`roost generate` 打印
`warning:`（工程照常编译，客户端收不到任何字段）。字段只经生成 setter 在 Nest handler 里改，提交时由 Nest 自动
收集，不手动标脏。关闭开关后再 sync，生成文件恢复原样、默认 packer 被删除，不引用 `wiring/selfsync`。运行时契约与客户端解码见 [PLAYER-ACCESS §7](../../docs/framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)；
回归 `codegen/internal/roost/self_sync_test.go`（按新手流程搭工程、只打开开关并标 sync 字段、经生成接线跑真实 TCP；
再换业务 packer 跑一遍、切回默认）与 `codegen/internal/entity/sync_contract_test.go`
（`TestSelfSyncDefaultPackerFollowsTheMarker`）。

### roost add timer（按实体的持久定时器）

DESIGN-TODO D11，维护者 2026-10-17 决定方案 B。`roost add timer --entity <Entity> [--component Timer] [--service <service>] [--id <component-id>]`
（也可写 `roost add timer <Entity>`）给一个本地持久实体加持久定时器：到点要做的事（Buff 到期、建造完成、行军到达）的期限存在实体自己的
DAO 里，到点由驱动投递一次 Tick，在实体的 Nest 锁与事务内触发。运行期在 core：`framework/nest/entitytimer`（索引、投递、离线语义）与
`wiring/entitytimer`（App 装配）。

| 文件 | 归属 | 内容 |
| --- | --- | --- |
| `roost.yaml` `timers.<entity>` | 声明 | `component`（缺省 timer）、`id`（组件类型号，ids.component 段内分配）、`service`（运行该实体 Nest 的服务）、`dao`（节点所在 DAO） |
| `db/def/<entity>.go`（实体 DAO 定义） | 业务（add 时加字段） | `<Component>s map[int64]*<Entity><Component>Node`（persist,map=fast）与 `<Component>Seed int64`（persist）；同名同类型已存在时不改 |
| `game/entities/<entity>/entity.go` | 业务（add 时加字段） | 组件字段 `<component> *<Component>Component`（tag `comp:"CompType<Component>"`）；已有时不改 |
| `db/def/<entity>_<component>_node_gen.go` | 生成（sync 重写） | 节点类型（期限按毫秒） |
| `game/entities/<entity>/<component>_component_gen.go` | 生成（sync 重写） | `//roost:component`、`<Component>Component`（`New<Component>` / `New<Component>WithPriority` / `New<Component>At` / `Change<Component>` / `Remove<Component>` / `<Component>(id)` / `<Component>s()` / `Tick`）、`I<Component>Entity`、DAO 适配、`<Component>Driver` 与 `<Component>TickHandler` |
| `game/handler/tick_<entity>_<component>_gen.go`（+`_test.go`） | 生成（sync 重写） | `//roost:nest rollback=undo durability=strict` 的 `handlerTick<Entity><Component>(target, nowUnixMilli int64) (int32, error)`；测试钉住驱动投递的名字就是 Nest 生成器得出的名字 |
| `internal/bootstrap/generated.go` | 生成（sync 重写） | 该服务追加 `kitentitytimer.NewMod(EntityAccess, <entity>.<Component>Driver, ...)` |
| `game/entities/<entity>/<component>_handlers.go` | 业务（只在缺失时创建） | 定时器类型号常量与 `register<Component>Handlers`（`handlers.Register(type, callback)`），回调签名 `func(entitytimer.Fire) time.Duration` |

**驱动**：进程内按“已加载实例的最早期限”排的小堆。实例加载（组件 `OnInitFinish`）时报告存量里的最早期限，事务里的增删改在提交后
（`nest.AfterCommit`）报告新的最早期限，回滚的事务不改索引；同一实例的报告带单调序号，晚到的旧报告丢弃；卸载 / 删档 / 撤销新建
（`OnDestroy`）后实例永久离开索引。只有最早期限到了才经 `nest.Client.Dispatch` 给那一个实体投递 Tick（正式准入，不按在线人数轮询），
投递前核对实例：已移除或已被同 ID 的新实例取代就丢弃，不唤醒不在内存的实体。投递后没有收到报告（handler 报错 / 回滚 / 提交被拒）
按 `entitytimer.DefaultRedeliver`（10s）重投；准入失败 1s 后重试。时间取 App 业务时钟（`app.BusinessClock`，含 `time.logic_offset`），
按毫秒。指标 `entitytimer.*`（OBSERVABILITY“实体定时器”）。

**离线语义（维护者 2026-10-17 决定）**：框架不追帧。实体不在内存期间到期的节点，实体加载后只触发一次，不论错过多少个周期；
`entitytimer.Fire` 带 `Deadline`（原定期限）、`Now`（实际触发时刻）、`LoadedAt`（本实例加载时刻）、`Overdue`（原定期限不晚于加载时刻，
即加载后补触发）与 `Interval`（最近一次登记 / 续期的间隔）。补算错过的周期由业务在回调里做（例如
`(fire.Now - fire.Deadline) / fire.Interval`）。回调返回正值是下次间隔（从 `Now` 起算），0 或负值结束；与 `timer.Scheduler` 一致。

**持久与恢复**：节点只在 DAO 里，走 DataEngine 正式链路，不另建存储；种子随 upsert 推进并持久，重建时不复用 ID（Scheduler 保证
每个分配过的 ID 至少经过一次 upsert）。

**拒绝**：没有 DAO（`roost add dao <Entity> --entity <Entity>`）、多个 DAO 且没有主 DAO（字段 `dao`）、`remote=managed`（所有权在进程间迁移，
驱动索引只覆盖本进程加载的实例，迁移中的到期语义未定义）、`noPersist=true`（不落库就没有可补触发的期限）、实体已有定时器组件（一个组件
承载全部类型）、组件名已被别的组件占用、服务没有 nest。拒绝在改任何文件之前报出。

回归：`codegen/internal/roost/add_timer_test.go`（生成物形状与归属、改名与显式 ID、拒绝的输入、清单校验；`TestEntityTimerGeneratedProjectEndToEnd`
按新手流程生成工程，经正式 Nest 与 wiring/entitytimer 跑在线到期、周期续期、离线错过多个周期后加载只触发一次、卸载后不投递、重载后 ID 不复用、
多定时器最早期限、删档不触发，`-race`）；core 见 `framework/nest/entitytimer` 的测试。game-demo 的 World 不用这个生成器（demo 是手写参考实现，
不依赖生成器产物，维护者 2026-10-18）：它手写同一套接入——`game/entities/world/timer_component.go` 的组件与 Store 适配、
`game/handler/tick_world_timers.go`、game 服务 `activity.go` 的 `startWorldTimers`（绑业务时钟与 Nest、启动、停机时停止）。

## 4. DAO 生成器

命令（`roost generate` 的 dao 步骤等价于它；`roost add dao <name> --entity <entity>` 生成初始骨架）：

```bash
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/dao@latest \
  -def ./db/def -out ./db -pkg db
```

输出目录需能推断出包名（已有至少一个非生成的 `.go` 文件），否则用 `-pkg` 指定。内容未变时不写文件（输出 `unchanged: ...`），mtime 不动。

定义：

```go
package dbdef

//roost:dao coll=players db=game dbscope=sid
type PlayerDao struct {
    Name      string                              // 无 tag：persist + sync 全开
    Level     int32
    LastLogin int64             `dao:"persist"`        // 只持久化，不同步给客户端
    Session   string            `dao:"nopersist,sync"` // 只同步，不落库
    Runtime   int64             `dao:"-"`              // 完全排除
    Items     map[int64]int32   `dao:"persist,sync,map=fast"`
    Tags      map[string]string `dao:"persist,map=sharded"`
    Equips    map[int64]*EquipInfo                // 嵌套 struct 自动递归生成
}

type EquipInfo struct {
    Level int32
    Star  int32
}
```

### struct 级标记

    //roost:dao coll=<collection> db=<database> [dbscope=global|sid]
    //roost:dao nocoll                       # 无集合 DAO，见 §4.1
    //roost:redisdao key=<字段路径> prefix=<键前缀> [mode=ref-hmap|raw] [key_type=...] [version=...] [ttl=<Go duration>] [name=...]

**库名与 `_id`**（D9，维护者 2026-10-16 决定 B）：`db=` 是编译期常量（生成 `db.<Type>DBName`），不读运行配置；
`dataengine.database` 只决定 Data Engine 自己的集合（receipts / transactions / outbox），生成配置里它也按同一规则取工程名（与 effects / saga 的主题与流等一起按工程名派生，见 PROJECT_GENERATOR §5）。`roost add dao` 骨架按工程名写
`db=<project>`：取 `roost.yaml project.name`（已满足 `^[a-z][a-z0-9_]*$`），Mongo 系统库名 `admin` / `local` / `config`
加后缀 `_data`，超过 52 字节截断并去掉末尾 `_`（库名上限 63 字节，`dbscope=sid` 还要追加 `_<sid>`）。同一 Mongo 上的
两个工程因此默认不写同一个库；已有 DAO 的 `db=` 归业务，不改写。库名改由运行配置决定（方案 A）未实施，见 DESIGN-TODO D9。

文档 `_id` 是完整 EntityID，不是玩家 ID：位布局 `[remote(1)][unique(52)][kind(8)][category 低 2 位]`，例如 kind 1、
category Other(5) 的玩家 9001 存成 `9001<<10 | 1<<2 | 5&3 = 9217029`。用 `roost id entity` 换算：

    roost id entity 9217029                          # → unique_id: 9001、kind: 1 (Player)、category_bits: 1、remote
    roost id entity --unique 9001 --entity Player    # → entity_id: 9217029（kind / category / remote 取自 //roost:entity）
    roost id entity --unique 9001 --kind 1 --category 5 [--remote]


- `coll=`、`db=` 必填；`dbscope` 默认 `global`，`sid` 表示按服分库（`dataengine.DatabaseServer`）。
- 标记必须**紧邻 struct**（相邻 1–2 行）或位于该 struct 的 doc 注释组内；标记与 struct 之间可以有普通文档注释。
- Redis DAO 生成类型化 Get/Set/Delete 包装；`key=`、`prefix=` 必填，`mode` 默认 `ref-hmap`，`key_type` 缺省时按字段路径从 struct 定义推断。

### 字段级 `dao:"..."` tag

| 写法 | 持久化 | 客户端同步 | 说明 |
| --- | --- | --- | --- |
| 无 tag | 是 | 是 | 安全默认值 |
| `persist` / `sync` | 按列出的 | 按列出的 | 只开列出的能力，未列出的关闭 |
| `nopersist,sync` | 否 | 是 | 临时但需同步 |
| `nopersist,nosync` | 否 | 否 | 只参与事务：生成同形 mutator（undo 登记逆操作、state 快照覆盖），不落库、不进 WAL、不同步；组件里事务会改又不该存的状态（派生层、调度索引）放这里 |
| `-` | 否 | 否 | 完全排除 |
| `map=small\|fast\|sharded` | 按其它标记 | 按其它标记 | map 存储实现：`small`（默认，`fmap.SmallSafeMap`）、`fast`（`fmap.FastMap`）、`sharded`（`fmap.ShardedSafeMap`）；`fast` / `sharded` 只支持 string 或整数 key |

**tag 一旦出现就必须表明 persist/sync 意图**：`map=...` 是辅助选项，必须与 persist/sync 意图同写。支持的字段类型：基础类型、slice、map、嵌套 struct（值或指针）；固定长度数组、`chan`、`*map[...]...`、指向非本地命名类型的指针在生成期被拒绝（提示用 `dao:"-"` 排除）。map 容器来自 `github.com/tjbdwanghaibo/roost-core/infra/base/safemap`（生成代码里别名 `fmap`）。

### 生成物与读写约定

生成的存储字段全部私有，业务只能经生成的方法读写，绕不过 dirty、回滚与同步语义：

```go
dao := db.NewPlayerDao()
dao.SetName("alice")
name := dao.GetName()
dao.SetItems(10001, 3)
count, ok := dao.GetItems(10001)
dao.RangeItems(func(itemID int64, count int32) bool { return true })
dao.DelItems(10001)
```

标量与嵌套对象用 `Get<Field>()`；slice 用 `Get<Field>(idx)` / `Add<Field>` / `Set<Field>All` / `Range<Field>` / `<Field>Len`；map 用 `Get<Field>(key)` / `Set<Field>(key, val)` / `Del<Field>(key)` / `Range<Field>` / `<Field>Len`。

生成器同时产出字段 mask、undo、事务内 `PersistChange`、字段级 Patch、BSON marshal/unmarshal、`MutationParticipant`、`RestorePersisted`（只接受 `schema=` 声明的当前版本，不做迁移；改形状按[schema 变更操作手册](../../docs/framework/SCHEMA-MIGRATION.md)离线转换）和 `DirtyTracker()`。三件要点：

- **dirty**：每个字段一个掩码位。setter 先比较再标记；持久化变化经 `nest.MarkPersist*` 进入当前 Nest 事务，同步变化留在 DAO tracker（`DirtyTracker()` 返回 `*dataengine.Tracker`，只保存已接受的持久化版本和 sync dirty）。
- **undo**：`rollback=undo` 的事务里 mutator 先登记逆操作再改值，回滚时逐字段撤销；`rollback=state` 用快照。登记失败直接 panic——逃出回滚覆盖范围的修改会静默破坏事务保证。
- **patch**：map 的键级修改记录为 MongoDB 路径级 `$set` / `$unset`（键不能安全编码为路径时退回整字段）。`PrepareMutation(change)` 把新建 / replace / 全字段修改编码为 Put，普通修改编码为字段级 Patch，删除编码为带版本的 tombstone；Patch 不携带整文档 fallback。嵌套 struct 生成到 `gen_<name>_nested.go`，内嵌 `dataengine.DirtyHook`，深层修改照样进入事务 patch。

持久化 setter / map mutator 不能在事务外调用；`durability=memory` handler 也不能修改 persistent 字段——`rollback=none` 时 setter panic，`rollback=state|undo` 时事务在提交点以 `nest.ErrMemoryTransactionPersistentWrite` 整笔失败回滚并点名实体与字段（生成 DAO 为此带 `PersistFieldNames(mask)`）。`MarshalPersist` / `MarshalSync` 内部 `bson.Marshal` 失败同样 panic：接口没有错误位，返回 nil 等于静默丢数据。

**回滚统一走 DAO**：生成 DAO 的 undo 与快照是唯一的回滚机制。事务内会改的状态一律放进 DAO（不该落库的用 `nopersist`），组件不持有这类内存状态，也不自己调 `RecordUndo` / `DeferRollback`。`cmd/glsvet` 对组件方法里的这类调用、以及组件方法（`OnInitFinish` / `OnDestroy` 除外）给组件普通字段赋值或改其中 map / slice 元素打印 `hint:`（不计入失败）；DAO 句柄与函数类型字段不提示，确属缓存、允许与 DAO 暂时不一致的字段在声明上一行或行尾写 `//roost:cache`。设计记录见 [DAO 统一回滚方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md)。

### 解析错误（fail-fast）

生成器对"看起来能过、实际埋雷"的写法一律报错而不是静默跳过，每条都对应一个曾经存在的静默陷阱（回归在 `codegen/internal/dao/failfast_test.go`）：

| 写法 | 报错要点 |
| --- | --- |
| 只写辅助选项 `dao:"map=fast"` | `does not state persist/sync intent`；按原意图改为 `persist,sync,map=fast` 或 `nopersist,nosync,map=fast` |
| 选项拼写错误 `dao:"persits,sync"` | `dao tag has unknown option "persits"`，并列出合法选项 |
| 标记与 struct 之间隔了别的声明 | `//roost:dao marker is not attached to a struct` |
| 分组 `type (...)` 只放一个标记 | `marker binds to multiple structs`；每个 struct 上方各放一个标记 |
| 缺 `coll=` / `db=` | `//roost:dao on HeroDao requires coll= and db=` |

其余硬错误：`//roost:dao` 后无任何参数、`persist` 与 `nopersist` 同写、未知 `map=` 值、`map=fast|sharded` 配非 string / 整数 key、字段私有存储名与框架保留名（`id` / `tracker` / `persistPatch*`）冲突。

### 孤儿生成文件清理

定义被删除或改名后，下一次生成移除对应的旧生成文件（输出 `removed orphan: db/gen_hero_dao.go`），包括删光所有定义的情况。只有文件名匹配 `gen_*_dao.go` / `gen_*_nested.go` 且以 `// Code generated by tool/dao. DO NOT EDIT.` 开头的文件才是候选，手写文件不会被删。

### 4.1 无集合 DAO：`//roost:dao nocoll`

只给 `noPersist=true` 实体（例如刷出来的怪）用的内存 DAO：位置等状态仍住在 DAO 里、经组件读写、随事务回滚、经 `MarshalSync(mask)` 复制，但不存储，所以不编造集合名：

```go
//roost:dao nocoll
type MonsterDao struct {
    PosX int64 `bson:"pos_x" dao:"nopersist,sync"`
    PosY int64 `bson:"pos_y" dao:"nopersist,sync"`
    HP   int64 `bson:"hp" dao:"nopersist,sync"`
}
```

- `nocoll` 是裸标志：不能写成 `nocoll=true`，也不能与 `coll=`、`db=`、`dbscope=`、`schema=` 同时出现（生成期报错并点名冲突的键）。
- **全部字段**（`dao:"-"` 除外）必须 `nopersist`；没写 dao tag 的字段默认 `persist,sync`，同样算持久字段。违反时报错并点名每个持久字段。
- 生成物保留字段 mask、getter/mutator、undo、`CaptureRollbackState`/`RestoreRollbackState`、`<Dao>SyncFields`、`MarshalSync`/`ApplySync`；
  **不生成** `<Dao>DBName`/`<Dao>Collection`/`<Dao>SchemaVersion`、`DbScope`、`PrepareMutation`/`AcceptMutation`、`Marshal*`、`Unmarshal`、`RestorePersisted`。
  `entity.DaoInterface` 仍满足：`DbName()` 返回 `""`，`CollName()` 返回 `<Dao>RegistryKey`（值为类型名，只是实体 DaoManager 的登记键，不是集合名）。
- Entity 生成器读 DAO 包的生成源码认出它（`<Dao>RegistryKey` 常量），接线改用该常量；持久实体或 `remote=managed` 实体使用它时生成期报错。
  DAO 在实体所在模块之外时无法在生成期判定，接线仍引用 `<Dao>Collection`，编译期失败。
- 有集合改无集合：改 marker、确认字段全 `nopersist`、确认实体 `noPersist=true`，`roost generate` 原地重写 `gen_<name>_dao.go` 与实体接线；
  业务里对 `XxxDaoCollection` 等常量或持久化方法的引用会在编译期暴露。设计记录见 [无集合 DAO 方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DAO-NO-COLLECTION-2026-10-04.md)。

## 5. Entity 生成器

新项目优先使用高层命令，不要手工复制下面的底层接线：

```bash
roost add entity Player
roost add component Profile --entity Player
roost add dao Player --entity Player
roost add mod nest -service game
roost add handler RenamePlayer --entity Player --component Profile
roost generate
roost project doctor
```

生成项目的 `docs/ENTITY_COMPONENT.zh-CN.md` 说明业务代码位置、DAO 方法式读写和 Nest 锁边界。
handler 会同时校验 nest feature 和至少一个 Service 的 nest 运行时 Mod；缺失时错误会给出
`roost add mod nest -service <service>` 修复命令。

命令：

```bash
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/entity@latest \
  -dir ./game/entities/player
```

定义示例：

```go
package player

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const (
    EntityCategoryPlayer entity.EntityCategory = 1
    EntityKindPlayer     entity.EntityKind = 1
    CompTypeBag          entity.ComponentType = 2000
)

var _ = func() struct{} {
    entity.MustRegisterEntityKindCategory(EntityKindPlayer, EntityCategoryPlayer)
    return struct{}{}
}()

//roost:entity id=1 entityKind=EntityKindPlayer
type Player struct {
    *entity.EntityBase
    entity.ComponentManager
    entity.DaoManager

    bag *BagComponent `comp:"CompTypeBag"`
    dao *PlayerDao    `dao:"players"`
}
```

`businessPool=short|long` 为 Entity Kind 指定业务池，默认 short；生成的 Builder 注册此元数据。任何 long 目标使整条多实体消息进入长业务池。先调用生成的 RegisterEntity，再创建 Nest；策略不能在运行中修改。

产物 `<entity>_gen_wire.go` 包含 factory、Base/Component/DAO getter、组件/DAO 装配、事务化 PrepareDelete 和生命周期 hook；普通 Entity 不再生成 Snapshot/RemoveSnapshot 写路径。`remote=managed` 时还生成 Remote Entity commit participant：它从同一 Nest transaction 读取 DAO 变更和**显式 delete intent**，在 ownership marker/lock fence/route epoch 下冻结完整远端 commit，并只在权威提交确认后推进版本。删除意图不再由 `IsRemoved()` 反推，因此 rollback 不会提前污染内存生命周期。`sync=true` 还可声明 `syncNamespace` 与**一个** packer 工厂：只接受 `subjectPacker`，它对应 Core 的 `entity.EntitySyncBuilderParam.PackerFactory`（签名 `func(entity.IThreadSafeEntity) entity.SubjectSyncPacker`）。
旧 `syncPacker` 标记明确报错；没写 `sync=true` 却写了 packer 同样报错。生成物只写 Core 真有的三个字段
（`Enabled` / `Topic` / `PackerFactory`），并由 `scripts/entity-sync-runtime.sh` 对着钉住的 core 编译 + 构建实体验证。

DAO 必须实现 codegen 生成的标准方法，尤其是 `PrepareMutation(nest.PersistChange)`、`AcceptMutation(dataengine.Mutation)` 和 `DirtyTracker() *dataengine.Tracker`。Entity 不直接访问 `dao.tracker` 字段。不要为普通 Entity 手写 Snapshot、RemoveSnapshot 或在 release hook 中落库；历史数据迁移通过 Data Engine migration/import 工具完成。

## 6. Nest handler 生成器

```bash
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/nest@latest \
  -dir ./game -sender=true
```

```go
//roost:nest rollback=undo durability=strict
func handlerTransfer(from IPlayerEntity, to IPlayerEntity, itemID int64, count int32) error {
    // 进入前两个 Entity 已按全局顺序加锁
    return nil
}

//roost:nest sync rollback=state durability=async
func handlerQuery(player IPlayerEntity) (PlayerView, error) { return PlayerView{}, nil }
```

| 选项 | 值 | 含义 |
| --- | --- | --- |
| `target` | 实体名 | 锁目标；不写时按参数类型推断 |
| `rollback` | `state`、`undo` | 全状态快照或生成 setter 的逆操作；热路径推荐 `undo` |
| `durability` | `memory`、`async`、`strict` | 内存、WAL admission、严格提交 |
| `sync` | flag | 生成同步 Sender |

未声明时默认 `rollback=state durability=async`，业务修改先进入 Nest WAL；需要请求返回前确认 durable admission 时声明 `durability=strict`，只有明确不持久化的 handler 才显式写 `durability=memory`。

第一个或多个 Entity 接口参数是锁目标；`[]IEntity` 表示实体组。进入 handler 前目标已按全局顺序加锁，业务层不要重复加锁。生成 `<source>_nest_gen.go`、sender/syncsender 和 bootstrap 注册代码。生成的 `sender` / `syncsender` 包是可注入的 `*Sender` 类型：接入层用 `NewXxxSender(nestClient)` 构造，方法带 `context.Context`、可返回入队错误；不生成包级 Send/Sync，也不读取全局 Nest。sender / syncsender 是 handler 包的子包，签名里的参数与结果类型原样搬进去，所以这些类型不能声明在 handler 包本身（子包看不见它，又不能反向导入 handler 包——handler 会调用 sender，导入成环）：生成器遇到这种类型直接报错 `type X is declared in package handler ... move X to a package the handler imports`，把它移到实体包或其他 handler 已导入的包即可（demo 的结果类型都在实体包里）。执行池（short / long / io）、`businessPool` 与 `SendOptionIO` 见 [Nest 业务执行模型](NEST_RUNTIME.zh-CN.md) 与 [Nest 三池](../../docs/framework/NEST-AWAIT.md)。

Remote read 可放在请求 struct：

```go
type QueryRequest struct {
    Target entity.RemoteViewRef `remote:"view.PlayerViewMapSnapshot,consistency=monotonic,required"`
}
```

生成 `TargetPlayer()`/`MustTargetPlayer()` 访问器。可选一致性、scope、tenant、policy、allow-stale、min-version 和 cache TTL 均在生成期校验。

**当前限制**：请求 struct 必须声明在 handler 包里（remote tag 只扫描 handler 包，访问器也生成在这里），而 sender / syncsender 子包看不见 handler 包的类型；`roost generate` 总会生成 sender，所以带 Remote 请求 struct 的 handler 目前生成会失败，生成器给出专门报错（不建议移走类型，移走后访问器就生成不出来）。见 [KNOWN-LIMITS](../../docs/maintenance/KNOWN-LIMITS.md)。

## 7. Protocol 生成器

```bash
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/protocol@latest \
  -def ./protocol/def \
  -proto ./protocol/proto -pb ./protocol/pb \
  -msgid ./protocol/msgid -bind ./protocol/player_bind \
  -handlers ./game/protocol_handlers \
  -manifest ./protocol/protocol_manifest.json
```

定义：

```go
//go:build protocoldef

package protocoldef

//roost:proto package=planet.protocol go_package=example.com/planet/protocol/pb;pb

type PingRequest struct { ClientTime int64 `pb:"1"` }
type PingResponse struct { ServerTime int64 `pb:"1"` }
type Notice struct { Text string `pb:"1"` }

//roost:protocol group=game handler=player
type GameProtocol interface {
    //roost:msg id=10001 tags=local
    Ping(PingRequest) PingResponse       // request/response
    //roost:msg id=10002
    Upload(PingRequest)                  // client push
    //roost:msg id=10003
    Notify() Notice                      // server notify
}
```

产物包含 `.proto`、pb Go、msgid、player binding、handler、robot registry 和协议 manifest。ID 全局查重；字段 `pb:"N"` 不得复用。反向生成：

```bash
go run .../cmd/protocol@latest \
  -reverse-proto ./third_party/game.proto \
  -reverse-out ./protocol/def/imported -reverse-package imported
```

## 8. cfggen：YAML schema 先行配置

### 先选管线

配置有 tablegen（§9）与 cfggen 两条生成管线，维护者 2026-10-06 决定保持两条、不合成一套（理由见[配置规则设计记录 §2.4](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md#24-tablegen-与-cfggen-能否合成一套)）。两条管线的规则底层是同一套（`configdata/rules`），configdata 在每次加载与 reload 时用同一个检查器执行。

| | tablegen | cfggen |
| --- | --- | --- |
| 手写什么 | `configs/schema` 下的 Go 结构体，`//roost:table` / `//roost:object` 标记，字段带 `csv` / `json` / `title` 与规则标签 | 一个 YAML 描述 `configs/schema/cfg.yaml`（tables / globals / beans，写法类似简化版 Luban），不写 Go 结构体 |
| 数据 | 策划编辑的 CSV（`configs/table`），转换成 `configs/data/*.json`；同时按 schema 生成表头 CSV 模板 `configs/table_template/*.csv`（每次整份重写，归生成器所有，复制到 `configs/table` 再填数据） | 直接维护 JSON |
| 单例配置 | `//roost:object` | `globals` |
| 怎么跑 | `roost generate` 默认执行（Config 步骤，Go 访问代码写到 `configs/generated`） | 不在 `roost generate` 里，单独执行 `cmd/cfggen`；输出不能和 tablegen 共用 `configs/generated` |
| 适合 | 策划用表格填数、以 CSV 为交付物的流程 | 程序维护的配置、需要 bean（嵌套结构）或二级索引、不想手写 Go 结构体 |

接真正 Luban 的方式见 `examples/lubanreal`（官方 luban CLI 真实生成的端到端示例）。

### cfggen 用法

```bash
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/cfggen@latest \
  -meta ./configs/schema/cfg.yaml -out ./configs/cfg -pkg cfg
```

`configs/generated` 是 tablegen（`roost generate` 的 config 步骤）的输出目录，两者同目录会重复声明 `RegisterConfigData`，所以 cfggen 输出到独立的 `configs/cfg`。业务接线只剩一行 `MustRegisterGeneratedConfigData(reg)`；二级索引生成强类型访问器（参数类型来自字段，不传索引名和字符串值）；悬空引用校验、原子热更与回滚由 configdata 承担。

```yaml
package: cfg
beans:
  - name: DropItem
    fields:
      - {name: item_id, type: int32}
      - {name: count, type: int32}
tables:
  - name: monster
    key: id
    fields:
      - {name: id, type: int32}
      - {name: scene_id, type: int32, index: true}
      - {name: drop_id, type: int32, ref: drop, required: true}
globals:
  - name: world
    fields:
      - {name: width, type: int32}
```

```go
generated.MustRegisterGeneratedConfigData(configdata.DefaultRegistry())
snap := configdata.ActiveSnapshot()
table, _ := generated.MonsterTableFrom(snap)
monster, ok := table.Get(1001)
sameScene := generated.MonsterBySceneID(snap, 7)
world, _ := generated.WorldFrom(snap)
```

完整类型系统、bean、index/skipempty 与规则（required / unique / min / enum / ref）见 [CFGGEN_META.zh-CN.md](CFGGEN_META.zh-CN.md)。规则由 configdata 在每次加载 / reload 时检查，与 tablegen 的标签走同一个检查器（§9）。

## 9. tablegen：Go metadata + CSV

```go
//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct {
    ID      int32  `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
    Name    string `csv:"name" json:"name" title:"名称" required:"true"`
    SceneID int32  `csv:"scene_id" json:"scene_id" ref:"scene"`
}

//roost:object name=world file=world.csv json=world.json
type World struct { Width int32 `csv:"width" json:"width"` }
```

```bash
go run .../cmd/tablegen@latest \
  -meta ./configs/schema \
  -out ./configs/generated -pkg generated \
  -csv-template ./configs/template

go run .../cmd/tablegen@latest \
  -meta ./configs/schema -csv ./configs/csv -json ./configs/data

go run .../cmd/tablegen@latest \
  -meta ./configs/schema -json ./configs/data -check
```

**规则只有一份，由运行时加载层强制**（[设计记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)）。字段标签 `required:"true"`（键必须出现且不为 null）、`unique:"true"`（主键隐含）、`min:"<n>"`、`enum:"a|b|c"`、`ref:"<table>"`（非零值必须是目标表的主键；同时写 required 时零值也要是）被翻译成一组 `configdata.FieldRule`，写进生成的 loader（`TableDef.Rules` / `ObjectDef.Rules`），configdata 在每次加载与 reload 时对 JSON 原样检查——直接改 `configs/data` 再 reload 也绕不过；违反即整次拒绝、旧快照保持，错误形如 `configdata: table spawn row 1 (key 1) field template: required: missing or null`。CSV 转 JSON 与 `-check` 用同一组规则、同一个检查器（`configdata/rules`）提前反馈：改一个标签，`roost generate` 后两处一起变。ref 的目标必须是同一 schema 里的表、字段类型等于它的主键类型，否则生成失败；ref 的数据只在加载时查（要全部表）。

**键与表头大小写敏感**（[设计记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md)）：JSON 的键必须逐字是字段的 json 名、CSV 表头必须逐字是 csv 名。只差大小写的键由 configdata 在每次加载 / reload 拒绝（`field level: case: key "Level" must be spelled "level"`），`-check` 用同一条规则（`configdata/rules.MisspelledKey`）提前报；只差大小写的表头在 CSV 转换和生成的 `Convert<Type>CSV` 里报 `header "Level" must be spelled "level"`——以前它被当成未知列跳过、值静默丢成零值。未声明的键 / 列维持原样。

CSV 前四行依次为字段名、标题、类型和规则；转换会校验规则、数字格式等并生成 `_manifest.json`（空的 required 单元格按 null 报 required）。每张表另生成 `<Type>Table()`（当前请求钉住的快照里的表，未加载时为 nil、读它安全）与 `<Type>By<Key>(id)`。两条管线怎么选见 [§8 先选管线](#先选管线)。

JSON 键以显式 `json` 标签为准，省略标签时沿用 Go 字段名（与 encoding/json 和运行时规则一致）；CSV 默认列名为 snake_case。`key=` 必须指向实际导出字段。单例 object 只能有 0 或 1 行数据，多行拒绝。`_manifest.json` 的 `generated_at` 是保留字段、当前不填充，不能作为生成时间来源。

### 9.1 `_manifest.json` 与 JSON 退役

`-csv … -json …` 每轮在 JSON 目录写 `_manifest.json`（v2）：`{"version":2,"generated_at":…,"tables":{"<name>.json":"<sha256>"}}`，`tables` 记录本轮生成的文件名与内容哈希。下一轮不再有对应 meta 的登记文件，内容未改动才删除（退役）；改动过的报 `retired table JSON … was modified; resolve it explicitly`，不删也不改 manifest。目录里**未登记**的 JSON 视为手写数据，始终保留。

`roost generate` 的 config-data 步骤：`configs/table` 已有 CSV 就运行转换；`configs/table` 还没有 CSV 时，只有 `_manifest.json` 仍登记着文件（有东西可退役）才运行，否则跳过——写好 schema、还没来得及放 CSV 的工程可以照常 `generate` / `--check` / `project sync`。保留 schema 但删掉它的 CSV、而上一轮 JSON 仍登记在 manifest 里，转换会报 `open configs/table/<file>.csv: no such file`：要么补回 CSV，要么把 schema 一起删掉让 JSON 退役。

**只读 v2 manifest**：`_manifest.json` 存在时版本必须是 2。v1（v1.17.2 及更早生成的工程，`tables` 恒空、没有归属记录）不再读取，转换与 `roost generate` 的 config-data 判断都报 `table manifest … is version 1, which is no longer read`；其它版本报 `unsupported table manifest version N`。一次性清理：删掉 v1 的 `_manifest.json`，同时删掉目录里由旧生成器写出、meta 已不存在的 JSON，手写 JSON 保留，再运行一次生成——新 manifest 只登记本轮输出，留下的未登记文件按手写数据永久保留。

## 10. attribute

```bash
roost add attribute Combat     # 开启 feature（project sync 写 doc.go 与 runtime.go）、写 profile 骨架并立即生成
roost generate                 # 改字段 / 公式之后
```

`roost add attribute <Profile>` 写 `game/gameplay/attribute/<profile>.go`（业务文件，只建一次）：第一个 profile 带示例字段与 `_Power` 公式，包里已有 profile 时只留 `dirtyMask` 与 TODO，`index=` 取已有 profile 范围之后（生成的 `Attr<Field>` 常量按字段名命名，同包不同 profile 的字段不能同名）。同名 profile 再 add 拒绝。手改 `roost.yaml` 的 `features` 开启时先 `roost project sync`；没有 `runtime.go` 时 `roost generate` 报错指出这一步。

```go
//roost:attribute index=1 max=64
type Combat struct {
    HP     int64 `attr:"hp"`
    Attack int64 `attr:"attack"`
    Power  int64 `attr:"power"`

    // 必须自带：每个生成的 setter 往这里 OR 自己的位。漏了会在生成期报错。
    dirtyMask uint64
}

// 下划线方法表示派生属性公式；参数名对应输入字段。
func (p *Combat) _Power(Attack int64, HP int64) int64 {
    return Attack*2 + HP/10
}
```

```bash
# 单独运行生成器时，目录与 roost generate 读的一致：
go run .../cmd/attribute@latest -dir ./game/gameplay/attribute
```

生成属性 ID/mask、metadata、类型化 setter、dirty mask、派生属性 Update、clone 以及包级访问器
`<Name>Of(Snapshot)` / `<Name>In(*Container, Selector)` / `<Name>Live(*Container, Selector)`。

属性统一以 `AttrValue`（int64）进出框架，所以字段只能是整数类型；`float32/float64`、`bool`、`string` 在生成期报错（float 会被静默截断），小数用缩放整数（如万分比）。`max` 不超过 64（dirty mask 位数），`index+max-1` 不超过 65535（AttrID）。

**框架半在 `roost-core/gameplay/attribute`**：`AttrID`、`AttrValue`、`Meta`、`Profile` 接口、`Selector`、`Snapshot`、`Container`。
启用 `attribute` feature 的工程会拿到 codegen 受控的 `game/gameplay/attribute/runtime.go`，把这些以别名再导出成
生成物使用的名字（`AttributeMeta`、`AttributeProfile`、`Snapshot`、`Container`、`Selector`），所以声明一个 profile 之后直接就能编译。访问器是**包级函数**而不是 `Snapshot` / `Container` 的方法：
Go 不允许给外包类型定义方法，做成函数别名才成立。

约定：profile 结构体自带 `dirtyMask uint64`（漏了生成期报错）；派生属性只能由公式写，`SetDirectAttr` 拒绝它们；
派生公式出现环、未知字段或重复输出时生成失败。`scripts/attribute-runtime.sh` 把脚手架的 runtime、真实 profile 与生成物放进临时模块，
对着钉住的 core 编译并跑一遍（类型化 setter / dirty / 公式 / 元数据 / 导入导出 / 快照隔离）。

## 11. webroute

从生成到 curl 的最小链路：

```bash
roost add webroute ShopPrice          # 多个业务服务时加 -service <svc>
# 按 next 提示在服务 Init 里加一行：
#   if err := kitweb.RegisterRoutes(registry, &web.Service{}); err != nil { return err }
go run . game                          # 或 make run SERVICE=game
curl http://127.0.0.1:8080/shop_price  # web.addr，见 configs/service/config.game.yaml
```

`add webroute <name>` 写 `service/web/<name>.go`（业务文件，只建一次；`GET /<name>`、`body=raw`），开启 `webroute` feature
（`project sync` 写 `service/web/doc.go`：路由收到的 `type Service struct{}`，依赖放在这里、在服务 Init 里构造），给宿主服务装上
`web` Mod（`wiring/web`）并补 `web:` 配置段，立即生成 `webroute_gen.go`。宿主服务：`-service` 显式给出；否则沿用已装 `web` Mod
的服务；否则工程只有一个业务服务时用它；框架托管服务不能挂业务路由。服务 Init 归业务、不改写，doctor 的 `webroute:serve`
在没有服务调用 `RegisterRoutes`（或调用了但服务没装 `web` Mod）时 WARN。

`web` Mod：`web.enabled`（缺省 true）、`web.addr`（缺省 `127.0.0.1:8080`）、`web.allow_public_addr`（生产环境绑非回环地址必须写 true，
与 ops 相同）。Start 只 bind（端口被占用时启动失败）；路由只能在 Service.Init 里登记，`service.started` 时冻结路由表并开始 Serve（之后再登记报错），
`service.stopping` 时 `http.Server.Shutdown`，Service.Shutdown 之后不会再有业务请求。**不做鉴权**：JSON 路由拿不到请求头，raw 路由可以自己读
`RawRequest.Header`；对外暴露时放在鉴权代理之后。也可以不用 `add webroute`、直接 `roost add mod web -service <svc>`。

```go
//roost:web method=POST path=/gm/player body=json
func queryPlayer(ctx context.Context, svc *Service, req QueryRequest) (QueryResponse, error) {
    return QueryResponse{}, nil
}

//roost:web method=POST path=/webhook body=raw
func webhook(ctx context.Context, svc *Service, req webroute.RawRequest) (Ack, error) {
    return Ack{}, nil
}
```

```bash
go run .../cmd/webroute@latest -dir ./service/web
```

生成 `(*Service).RegisterRoutes` 与包级 `RegisterRoutes(reg, svc)`（`kitweb.RegisterRoutes` 按 `webroute.Module` 调用前者）。JSON 模式只接受一个完整 JSON 文档并统一返回 400 解码错误；raw 模式保留原始 body。是否拒绝未知 JSON 字段由 webroute 运行时包的 `DecodeJSON` 契约决定。重复 method/path、非法 body mode 或 handler 签名直接失败。

## 12. errcode

```go
var ErrItemNotFound = errcode.Define(100001, "item_not_found", "item not found")
```

```bash
go run .../cmd/errcode@latest -root . -out docs/generated/errcode.csv
```

扫描整个仓库并导出编号、名称和消息；重复 code/name 或非常量调用失败。

码段归属（登记在 `infra/base/errcode/errcode.go` 包注释）：`0` OK；`1–999` 框架段，只由 errcode 包分配（`1` CodeInternal，
`2` CodeEntityNotFound——目标实体在权威里不存在，客户端先登录 / 创角）；`100000–199999` 是 `roost.yaml ids.errcode`
的默认业务段，可改但下界必须大于 999（manifest 校验拒绝重叠）；内置 service 各占 `5x0101–5x0199`（directory 53、
rank 54、match 55、account 56、global 57、chat 58、mail 59、platform 60、session 61、activity 62）。ID 分配配合：

```bash
roost id next errcode
roost id next -group game protocol
roost id check
roost id entity 9217029                          # 完整 EntityID ↔ 唯一号 / kind / category，见 §4
```

## 13. servicerpc：接口先行的跨进程服务

```go
// mail/mail.go —— 唯一手写的跨进程契约
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .
//
//roost:rpc service_type=mail capability=service.mail
type Mail interface {
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
	//roost:rpc affinity=queue.Key()
	List(ctx context.Context, playerID int64, cursor string, limit int) (page Page, err error)
}
```

`//roost:rpc service_type=... capability=...` 打在手写的服务接口上，方法上可选 `//roost:rpc affinity=...`。产出两个文件：`<接口名小写>_rpc_gen.go`（线上类型、handler 注册、类型化 `BusClient`、capability 包装与名字，只依赖框架运行时包）与 `<接口名小写>_rpc_assembly_gen.go`（`Server`、`OwnerCapabilities`、`ClientMod`，依赖 `wiring/` 的装配）。`-emit transport|assembly|all` 与 `-out` 让两半分开生成；`-dir` 可以是 import path（接口住在 `service/<领域>`、装配住在 `wiring/<领域>` 时用）。调用方只查接口（`app.Lookup[mail.Mail]`），本地实现与远端客户端都满足它；传输层从接口本身生成，所以漂移在结构上不可能出现。

拥有者进程：`mods.RegisterAll(r, OwnerCapabilities(service)...)` 加一个手写的 `func (s *Server) run(ctx context.Context) error`；调用方进程：`mail.NewClientMod()`。两者发布同一个 capability 名，同一进程放不下两份——registry 拒绝第二个，这是想要的行为。工程内的跨进程服务用 `roost add rpc`（见 [项目生成器 §6](PROJECT_GENERATOR.zh-CN.md#6-日常业务脚手架)）。

`-check` 用于 CI：先跑全部拒绝规则，再生成并逐字节比对磁盘上的文件，任何差异非零退出，并区分"缺失"（没人跑过生成器）与"不一致"（文件被改过或由另一版本生成器产出）。它不写文件。

拒绝规则（每条都对应一个真实踩过的坑）：

| 拒绝 | 为什么 |
| --- | --- |
| 未命名的参数 / 返回值 | 线上类型没有字段名可用 |
| `time.Duration` / `time.Time` | 纳秒整数抓包读不懂，非 Go 调用方造不出；校验递归进包内结构体 |
| 未导出字段 | 任何 codec 都会静默丢掉它，对面拿到不完整的值而不是报错 |
| 未导出类型、`any`、chan、func、嵌套超过 8 层 | 同上，或根本无法编码 |
| 首参不是 `context.Context` | 不接 ctx 的方法不做 I/O，没有理由是一次往返 |
| 包里没有 `ErrRequestInvalid` | 生成的 handler 需要一个带码的错误回答"这一帧读不懂"；用 `CodeInternal` 会把调用方的畸形请求报成服务端故障 |
| `affinity=` 指向不存在的参数，或裸 key 不是 string | 亲和 key 在调用时从实参读；派生形式写 `affinity=x.Key()` |
| 生成名与包内已有声明撞名 | 生成的那边不能动（`pkg.Server` 在每个服务里读法一致）；撞名的若是函数或常量，会在编译失败前先改掉整个包的含义 |
| 一个包里两个被标记的接口 | 生成文件在包级声明十几个固定名字；`app.Service` 每进程一个，两个被标记的接口应是两个包 |

## 14. 配置、格式和生产门禁

```bash
roost config check --service game
roost config enable player-tcp --service game
roost config disable player-tcp --service game
roost config check --service game --production \
  --file configs/service/config.game.prod.yaml
roost format check
```

`add transport tcp` 会增量补齐开发与生产示例中的 player_access.tcp 默认项，并保持其他 YAML 文本；
`enable` 在鉴权仍是默认拒绝骨架时 fail-fast，只修改 enabled，`disable` 可随时安全停流。`check` 解析 YAML、
要求 sid，并与 doctor 的 config-schema 用同一份声明检查值与键：框架段里没有任何声明的键（多半拼错，例如
`mongo.urii`）报错；静态检查发现可疑键时编译一次进程读回业务服务自己的声明复核（`checkConfigFileDeclarations`），
键检查通过后再交给同一个编译出的进程跑 `--check-config`：跨键规则（`ValidateConfig`）与跨 Mod 规则（`app.ModConfigCrossChecker`，例如 `saga.start_effect_stream` 必须等于 `dataengine.effects.stream`），与 doctor 的 `config-check:<service>`、进程启动前的检查是同一处规则，三处结论相同；工程编译不过时打印 `cross-mod rules NOT checked` 并以 `config keys are valid (cross-mod rules not checked)` 结束，不报 `config is valid`。业务独有段的拼写只由 doctor 检查。`--file` 按 `config.<service>[.<变体>].yaml` 认出服务，认不出时加 `--service`。`roost.yaml` 读不出来时检查直接失败。
production 另外拒绝 CHANGE_ME、localhost/127.0.0.1 和 dev- 前缀值；它不替代基础设施拓扑探测和故障演练。

## 15. 独立生成器参数速查

| 命令 | 输入参数 | 输出参数 | 其它参数 |
| --- | --- | --- | --- |
| `dao` | `-def`（默认 `./db/def`） | `-out`（默认 `./db`）、`-pkg` | `-force` |
| `entity` | `-dir`（默认当前目录或 `GOFILE` 所在目录） | `-output`（默认每个 Entity 一个 wire 文件） | `-force` |
| `nest` | `-dir`（默认 `.`） | 按源文件生成 | `-sender`、`-force` |
| `protocol` | `-def` | `-proto`、`-pb`、`-msgid`、`-bind`、`-handlers`、`-robot-protocol`、`-manifest` | `-reverse-proto`、`-reverse-out`、`-reverse-package`、`-force` |
| `cfggen` | `-meta`（默认 `./configs/schema/cfg.yaml`） | `-out`（默认 `./cfg`）、`-pkg` | 无 |
| `tablegen` | `-meta` | `-out`、`-pkg`、`-csv-template`、`-json` | `-csv`、`-check`、`-force` |
| `attribute` | `-dir` | `-output` | `-force` |
| `webroute` | `-dir` | 按源目录生成 | `-force` |
| `errcode` | `-root` | `-out` | 无 |
| `servicerpc` | `-dir`（目录或 import path，默认 `.`） | `-out`（默认 `-dir`） | `-emit`、`-runtime`、`-check` |

任何独立命令均可加 `-h` 查看当前版本的权威默认值。生产工程优先把这些参数写进 `roost.yaml` 后使用 `roost generate`，避免开发机与 CI 命令漂移。

## 16. 常见问题

- **entity 生成代码访问 `dao.Tracker` 编译失败**：DAO 与 Entity 不是同一代生成器产出；两者一起重新生成（当前模板调用 `DirtyTracker()`）。
- **`go mod tidy` 删除 skill**：新项目的 `internal/frameworkdeps/generated.go` 用类型别名固定 skill module。
- **生成后 CI 有 diff**：先运行 `roost generate`，确认没有手改生成文件；CI 用 `--check`。
- **输出目录无法确定 package**：先放一个非生成 `.go` 文件，或使用 `-pkg`。
- **标记没有生效**：标记必须紧邻目标声明，孤儿标记会报错；不要放在生成文件中。
- **直接修改 DAO 字段不再编译**：这是预期保护，改用生成 getter/mutator；不得为了兼容重新导出存储字段。
- **跨平台 golden 误报**：比较前只规范化 CRLF/LF，其他模板内容完整比较。

## 17. 推荐的日常闭环

```bash
roost add entity player
roost add component profile --entity player
roost add dao player --entity player
roost add mod nest -service game
roost add handler rename_player --entity player --component profile
# 补全 DAO 字段、Component 方法和 Nest handler 的业务参数
roost generate
roost project doctor
make ci
git diff --check
```

代码生成只消除一致性敏感的样板，不代替领域建模。Entity 边界、事务 durability、Remote read level、Saga 补偿和协议兼容仍必须由业务显式决定并经过评审。
