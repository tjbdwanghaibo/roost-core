# 当前已知限制与验证边界

## RV-20261014-01：Demo 计数器前缀与退役键冲突

**P2，已修复（`23c7a997`），随 v1.26.1 发布。** 引入提交 `1ffb644f`，审查基线 `4370e6b3`，影响 game-demo 账号 allocator 模板，默认生成前缀不受影响。修复后 BindRegistry 在任何 Redis 调用前把计数键等于退役键的前缀作为配置错误拒绝，分配时的退役键报错不再引导删除；拒绝旧兼容的决定不变。回归见模板测试 `TestPlayerIDCounterContinuesAcrossRestartAndASecondReplica` 与 `TestPlayerIDAllocatorRefusesAPrefixWhoseCounterIsTheRetiredKey`，修前在真实生成的 game-demo 上以下文首次失败原文变红。已经用 `roost:demo` 前缀运行的部署仍须先停写、把整个前缀（计数器在内）迁到新前缀。以下为发现时的原始描述。

`account.key_prefix=roost:demo` 可以通过当前配置与 BindRegistry 校验，但生成的计数键恰好是被拒绝的 `roost:demo:player_id`。空 Redis 上第一次 Allocate 看不到旧键，将 checked 置为 true，随后 INCR 创建这个键；同进程继续成功，重启或新副本重新绑定后却把自己的新计数器当旧数据拒绝。当前错误还建议删除同一个键，照做可能让后续分配从头开始，重发已有玩家 ID。

证据位置：[collaborators 模板](../../demo/internal/service/account/collaborators.go.tmpl) 的 refuseRetiredCounter / BindRegistry / Allocate；[配置读取](../../wiring/account/account_mod.go)与[前缀检查](../../wiring/mods/service_servicemods.go)没有保留键冲突校验。隔离探针当前失败，`1ffb644f^` 对应模板通过；[复跑步骤与审查清单](COMMIT-REVIEW.md)。首次失败原文：

```text
fresh deployment restart cannot allocate: demo player id allocator: retired counter key roost:demo:player_id is still present and no longer read: set roost:demo:player_id to its value if that key does not exist yet, then delete roost:demo:player_id (or clear this deployment's account data)
```

审查建议（已按此修复）：在 BindRegistry、任何写入前校验新计数键不能等于退役键，冲突时报告配置错误；保留拒绝旧兼容的既定决定。已有冲突部署先停写并迁移完整前缀与计数器，不能直接删除当前计数器。修复应同时更新模板、示例说明和重启/多副本回归。

## 新用户试用第三轮（2026-10-17 登记）：三项已修复 / 实施（配置段剩余部分按维护者决定 B 由 doctor 只读检查），demo 公会名两点已决定

基线 `e85addd9`（origin/main）。用 main 构建的 CLI 从空目录按 beginner 流程（`roost project next` 逐步）建工程、打开 `self_sync`、在共享隔离环境起真实进程并用 TCP 客户端联调时确认。修复与回归见 CHANGELOG `[Unreleased]`“生成器 / game-demo：新用户试用第三轮与 demo 两处缺陷”。

### 1. 依赖带入的 Mod 在服务配置里没有配置段：已修复

**原现象**：`roost add access player --service game` 给 game 补上 nest，进程实际启动 `mongo`、`nats`、`dataengine`，但 `configs/service/config.game.yaml` 与 prod 示例里三段（连同 `nest:`）一段都没有，进程按声明默认值连本机 27017；`singleton.enabled` 停在 `false`。`roost add rpc` 让拥有者挂上总线，同样不写 `nats:` 段。

**修复**：两条路径与 `add mod` / `add saga` 走同一个 `appendModConfigSections`（只追加缺失的顶层段，dev / prod 示例 / k8s Secret 示例；带 DataEngine 的服务打开单实例锁、补 `redis:` 段）。回归 `TestModsBroughtInByAddAccessAndAddRPCGetTheirConfigSections`。

**剩余（维护者 2026-10-17 决定 B，已实施）**：配置文件生成后归应用所有，追加只发生在“这条 add 命令让服务多出 Mod”的那一刻；修复前已经走过 `add access` / `add rpc` 的工程、在 add 命令之外手改 `roost.yaml` 增加 Mod 后 `project sync`，都不补（sync 行为不变）。`roost project doctor` 新增只读检查 `config-sections:<service>`：按服务实际注册的 Mod（`serviceConfigMods`，与生成器同一份计算）找 dev 配置、prod 示例、k8s Secret 示例里缺的顶层段（带 DataEngine 的服务另查 `redis:` / `singleton:` 与 `singleton.enabled: true`），WARN 里给出可直接追加的文本，不改文件。回归 `TestDoctorNamesTheConfigSectionsAManifestAddedModLacks`、`TestFreshlyGeneratedConfigsHaveEveryModSection`。**限制**：业务有意删掉的段同样报 WARN（不让 doctor 失败），没有豁免开关，可忽略或留空段（如 `saga: {}`）消掉；“有没有”只看行首 `<key>:`，段里缺了某个键、值写成别的形态不报（必填键与值的检查归 `config-schema`）；Secret 示例被改成认不出的结构（没有恰好一个 `config.yaml: |` 块）时只报“未检查”。

### 2. `roost project next` 的修复提示是固定示例名：已修复

**原现象**：first-business 的 `nest-handler` / `protocol` / `endpoint` / `login` 修复命令写死 `RenamePlayer` 与 `--handler player`；工程已有 `BuyItem`、`GrantGold` 时仍提示 `roost add protocol RenamePlayer --group game --handler player`，已有 `shop` 域时登录步骤仍提示 `--handler player`。

**修复**：`doctor.go` 的 `firstBusinessNames` 按工程现状填名字（已有 Entity / Component、第一个缺协议的 handler、第一个缺 endpoint 的协议及其域、已有 controller 域与 lifecycle、`ids.protocol.groups`）；只有工程里没有可用名字时给示例名并加 `# example name, use your own`。回归 `TestFirstBusinessFixesNameWhatTheProjectAlreadyHas`。限制：判断“缺协议 / 缺 endpoint”沿用 doctor 现有的文件名约定（`game/handler/<snake>.go` ↔ `protocol/def/<snake>.go` ↔ `game/controllers/<域>/<snake>.go`），协议名与 handler 名不同的工程仍会落到第一个可用名字或示例名。

### 3. self_sync 的 packer 要业务整段照抄：已实施（维护者 2026-10-17 决定 A）

**原现象**：打开 `self_sync` 后就绪检查要求 Player 标记写 `sync=true syncNamespace="player" subjectPacker=NewPlayerSyncPacker`，并照抄 [PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync) 约 40 行的 packer；抄错只在运行期 `ErrSyncViewUnknown` 暴露。

**实施**：开关打开时 entity 生成器带 `-self-sync Player`：标记缺省按 `sync=true`（`syncNamespace` 缺省 `"player"`），没写 `subjectPacker=` 时生成 `game/entities/player/player_sync_packer_gen.go`（视图集只有 `policy.SelfProfile`，包含 Player 唯一 DAO 的全部 sync 字段，字段表运行期从 DAO 取），wire 的 `PackerFactory` 指向它；写了 `subjectPacker=`、或关掉开关时不生成并删掉生成的那份（业务同名文件没有生成头，不删不覆盖）。就绪检查对没有 DAO、多个 DAO 且没写 `subjectPacker=`、显式 `sync=false` 拒绝并给改法；DAO 没有 sync 字段时 doctor warn、`roost generate` 打印 `warning:`。开关关闭的工程生成物不变。回归 `TestSelfSyncDefaultPackerFollowsTheMarker`、`TestSelfSyncCheckNamesTheFix`、`TestSelfSyncGeneratedProjectOverRealTCP`（不写 packer 跑真实 TCP；换业务 packer 再跑；切回默认）。

**代价 / 剩余**：标了 sync 的字段默认全部对玩家本人可见，需要裁剪的业务写自己的 packer。与登记时的方向 A 不同处：marker 写的正是生成名（`subjectPacker=NewPlayerSyncPacker`）时按“业务自己的 packer”处理、不生成——旧工程照抄的 packer 不会与生成物重名冲突；想改回默认就删掉 `subjectPacker=` 与自己的 packer。默认 packer 只发一个 DAO，多 DAO 的 Player 仍要自己写。

### game-demo 公会名：长度上限与全服重名（维护者 2026-10-17 决定：业务逻辑，框架不处理，参考 service/directory）

`guild.ValidateName`（[模板](../../demo/game/entities/guild/roster_component.go.tmpl)）拒绝空、全空白、含控制字符与非法 UTF-8 的名字，端点在保留 ID 之前调用。**长度上限**与**全服重名**：维护者 2026-10-17 决定是业务逻辑，框架不处理，demo 也不实现——名字只受协议帧上限约束，两个公会可以同名、靠 `guild_id` 区分。业务需要长度上限 / 敏感词时加在 `ValidateName` 同一处；需要全服唯一时参考框架已有的 `service/directory`（`wiring/directory` 装配；先 Reserve 再创建、成功 Commit、失败 Cancel，预约带过期），用法写在 [Service 篇 §4](../framework/guide/09-services.md#4-关键业务边界)、[WALKTHROUGH](../../demo/WALKTHROUGH.md) 公会创建一段与模板注释。

## 新用户试用第四轮（2026-10-17 登记；1、2 已修复，3 转 DESIGN-TODO D11）

基线 `300d728a`（origin/main）。先复核第三轮三项：依赖带入的 Mod 配置段、`project next` 按工程现状填名字、self_sync 默认 packer，用 main 构建的 CLI 新建工程实测均已解决（零手写 packer 时客户端照样收到全量 + 每次修改的增量）。随后按新手流程做“双倍金币卡”（限时 Buff，按业务时间到期），在共享隔离环境起真实进程联调时确认下面三项。2026-10-17 复核三项均属实：1、2 先红后绿修复（`b717fd14`、`990af957`），3 需要产品方向，登记为 [DESIGN-TODO D11](DESIGN-TODO.md#d11-按实体的持久定时器接入)。

### 1. 生成配置里的总线 / Data Engine 命名是所有工程共用的固定值（已修复，`b717fd14`）

**处置**：决定“与谁共享”的键全部按工程名派生——`nats.prefix: <project>`、`syncbus.prefix: <project>.sync`、`dataengine.database` 与 DAO 骨架 `db=` 同一规则（`daoDatabaseName`）、effects / saga 的主题与流 `<project>.effect` / `<PROJECT>_EFFECTS`、`<project>.saga` / `<PROJECT>_SAGA`、`saga.database: <db>_saga`、`remote_entity.mongo.database: <db>_remote`、`etcd.service_prefix: /<project>/services/`。规则写在 Wiring Mod 声明的 `example` 占位符里，新建与后加 Mod 的段同一规则（[PROJECT_GENERATOR §5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)）；回归 `TestGeneratedSharedResourceNamesFollowTheProject`。与登记时建议的差异：Data Engine 库与 DAO 同库（`<project>`，不是 `<project>_engine`），沿用改前“两者同为 game”的布局。**剩余限制**：已有工程配置不改写，旧工程之后追加的段会写新名字、与已有段不一致，需要手工统一（CHANGELOG 升级说明）；doctor 未对“仍是框架固定值”给警告（没做，是否需要待维护者定）。


**现象**：新工程的 `configs/service/config.game.yaml` 写 `nats.prefix: roost`（66 行）、`dataengine.database: game`（103 行）、`dataengine.effects.subject_prefix: roost.effect` 与 `stream: ROOST_EFFECTS`（134、135 行）；同一工程的 DAO 已按工程名生成 `//roost:dao coll=player db=r4`（D9 方案 B）。

**复现**：`roost project new r4 -module example.com/r4 -mods configdata` → `roost add access player --service game` → 看上面四行。

**影响**：两个工程共用一套开发用 Mongo / NATS 时，Data Engine 的 outbox / receipts / transactions 落进同一个 `game` 库、effects 走同一条流；[demo README 常见问题](../../demo/README.md#常见问题)记录了后果：上一个工程留下的 outbox 行没人 ack，30 分钟后新工程 `dataengine outbox: hard backlog limit exceeded` fail-stop；共用 `nats.prefix` 时 RPC 被别的流抢答（`unsupported rpc response version 0`）。新手只能在出事后从 FAQ 里找到这几个键。saga 的 `subject_prefix` / `stream` 同样不随工程（README 同一节）。

**建议修法**：与 D9 方案 B 一致，生成时按工程名填这几项（`nats.prefix: <project>`、`dataengine.database: <project>_engine` 一类、effects / saga 的 subject 与 stream 带工程名）；已有工程不改写已有值，doctor 可以对仍是框架缺省值的给警告。

### 2. `roost add endpoint` 只能从请求字段取参数，服务器决定的值（业务时间）无处可放（已修复，`990af957`）

**处置**：handler 参数名 `nowUnix` / `nowUnixMilli`（`int64`）由 endpoint 骨架从 `app.BusinessClock(controller.Registry()).Now()` 填入，不读请求；其他请求里没有的参数照旧拒绝，报错写明服务器决定的值不放进请求（[CODEGEN_REFERENCE endpoint 段](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)）；回归 `TestEndpointFillsBusinessTimeFromTheServer`。只识别业务时间这一类约定名；其他服务器决定的值（玩家状态、配置）按约定在 handler 里读，不做参数，没有 `--server-param` 一类的通用标记。


**现象**：handler 需要业务时间时（`handlerUseDoubleCard(target, nowUnixMilli int64)`，与 game-demo 的 `nowUnix` 参数同一写法），`roost add endpoint UseDoubleCard --handler shop` 拒绝：`protocol UseDoubleCardRequest has no field matching Nest parameter "nowUnixMilli"`（`codegen/internal/roost/add_workflow.go:620`）。

**影响**：报错只告诉新手“请求里缺这个字段”，最省事的改法是在请求里加 `NowUnixMilli`，于是业务时间由客户端决定，客户端可以随意延长限时效果、提前领取冷却奖励。正确写法（endpoint 里取 `app.BusinessClock(controller.Registry()).Now()` 传给 Sender）只能整段手写，game-demo 的 controller 都是手写的。

**建议修法**：endpoint 生成器认识“服务器提供”的参数，例如约定名 `now` / `nowUnix` / `nowUnixMilli`（`time.Time` / `int64`）由业务钟填入，或 `--server-param nowUnixMilli=business-clock`；其他请求里没有的参数，报错写明“业务时间等服务器决定的值不要放进请求，写在 endpoint 里”，并给出手写示例。

### 3. 没有可复用的持久定时器接入（按实体的到期动作）（待维护者决定，[D11](DESIGN-TODO.md#d11-按实体的持久定时器接入)）

**现象**：`infra/base/timer.Scheduler` 是宿主驱动的通用堆：期限持久化、加载后重建、谁来 Tick 都由业务决定（[guide/10-time §3](../framework/guide/10-time.md#3-scheduler)）。现成的接入只有 game-demo World 的 `TimerComponent`（模板 263 行，期限存 `WorldDao.Timers`），由服务进程的 ticker 定期发 `Sync_TickWorldTimers` 驱动；它与 World 的活动阶段写在一起，生成器没有对应命令。

**影响**：按玩家的到期动作（Buff 到期推送、建造完成、行军到达）要把 TimerComponent、TimerNode DAO 与 ticker 整套复制到 Player，并自己决定怎么驱动每个玩家的 Tick；`Delay_` 又不持久（NEST_RUNTIME“延时 Sender 不持久”）。新手大多退回“存到期时刻、读取时按业务时间判断”（本轮双倍金币卡就是这样写的），到期那一刻服务器不会做任何事。

**需要维护者决定**：是否提供通用的 DAO 期限组件（例如 `roost add timer --entity Player` 生成节点 DAO 与组件，框架负责重建与持久化钩子），以及按实体的 Tick 由谁驱动（进程 ticker 按最早期限只给到期实体发消息，而不是每秒给所有在线实体发）。

## 能力与验证边界

架构后续事项：[Session 运行服务可选化、玩家接入层与 Sync 命名](ACCESS-SYNC-FOLLOWUPS.md)。Session 运行服务保持现状、不改为可选（维护者 2026-10-14 决定）；玩家接入目录已改名为 `game/playeraccess`（dd362116）；Sync 命名已随 v1.26.0 发布，源码 API 需要随升级迁移。

当前版本v1.27.0。v1.25.0 时维护者接受重档失败先发布；v1.26.0 已修复确认的放大因素（慢诊断全进程抓栈），v1.26.0 发布后同参数 15 分钟重档复测通过（业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、>50ms 0.0098%、最大 86.341ms），长尾收窄但未唯一归因；v1.27.0 发布后同参数延长到 30 分钟仍通过（业务 P99 12.49ms、Sync P99 14.71ms、>50ms 0.0038%，堆不随时间增长，业务最大 107.307ms），分别见 [v1.26.0 发布验收](../release/v1.26.0-VALIDATION.md#perf15)、[15 分钟复测](../performance/V1260-HEAVY-20261013.md)与 [30 分钟复测](../performance/V1270-HEAVY-30M-20261014.md)。2026-10-15 带采集的同参数重档定位长尾（[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1）：约 60% 的长尾事件集中在每个墙钟分钟第 2 秒，与宿主 MDM / CorpLink 周期任务同秒，表现为调度延迟而非进程内 STW / GC / 锁，未归因到 roost 代码；另有持续数秒、不在整分钟的簇尚未确认。文档没有重新证明全部历史缺陷已关闭。已合入修复的旧日期状态不再当当前待办；不确定的能力不写成已经支持。

2026-10-12 在 `6c36e0df` 上复验快8/慢8/GOMAXPROCS=0的相同重档，约6分钟出现 Gate 通道 `context deadline exceeded` 并关闭绑定，15分钟目标未通过。同参数诊断复跑约12分25秒失败，轨迹确认Nest慢请求自动全进程抓栈触发330.160ms STW，跨过Gate的250ms期限并放大故障；自动抓栈现已移除并通过Nest及race回归，同参数15分钟复验通过，Sync提交→客户端P99为18.66ms，零消息/HB拒绝、零outbound失败；仍有0.1813%的已订阅delta超过50ms、最大336.387ms，初始延迟未唯一归因。2026-10-10 的通过样本不能作为当前新增提交已稳定的证明。详见[本轮性能报告](../performance/REVIEW-20261012.md)。

| 项 | 当前结论 | 后续验收入口 |
| --- | --- | --- |
| Sync偶发长尾 | on_change按P99≤50ms验收；最大250.699ms样本根因未唯一确定。2026-10-15 采集：本机约60%长尾与宿主每分钟第2秒的周期任务同秒（调度延迟、进程内无额外工作），GC STW/标记辅助/锁/进程内定时器已排除；持续数秒的非整分钟簇尚未确认。宿主留 CPU 余量对照（`GOMAXPROCS=8`，方案 B）同日已做，长尾反而更多（>50ms 为基线 3～10 倍），无效 | 维护者 2026-10-15 决定先接受现状；以后有独占服务器 / Linux 实机时再复测（方案 A）；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1、§5 |
| Remote容量 | 已测80TPS完整1h（v1.24.0）；v1.27 代码 80TPS×30m 在共享隔离环境复测通过（零错误、核验通过、p99 278ms），非跨机上限 | 跨机RTT/故障/24h；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3 |
| Saga补偿容量 | 正向20/s、全补偿10/s已测；20/s全补偿饱和。v1.27 代码各 10 分钟复测通过，全补偿10/s 首测 P99 1.47s / max 2.01s 高于历史 1 分钟样本（0.6s）；夹具加分钟分段后同日复测 P99 0.64s、十个分钟段无增长趋势，首测高值未复现、原因尚未确认 | 真实步骤、混合负载与生产存储另测；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3、§5 |
| 跨机HA与Linux物理故障 | 本机集群/loopback不证明跨机器分区、掉电、真实网卡 | EXTERNAL-VERIFICATION E01～E28具名清单 |
| 客户端引擎 | 有Go/C#及Unity适配源码与测试，未声明真实Unity/Godot/Unreal全矩阵 | 各引擎实机、线程、断线与跨平台确定性 |
| 原生C++接入 | 本版无完整C++/Unreal/GDExtension SDK承诺 | 独立设计和协议一致性验收 |
| 目录升级 | 分类与 Service/Wiring 拆分已实施；旧 kit 与领域 alias 不保留 | [方案与升级要求](../framework/PACKAGE-REORGANIZATION.md)，重新生成并编译消费者；随v1.25.0交付 |
| 嵌入 TCP 出站 | 2026-10-16 起每连接有界出站队列 + 唯一写协程，停读客户端被关闭、不再拖住场景 Sync Flush / Lockstep Tick 的其他会话（修前一个停读连接让每次 Flush 卡满 `write_timeout`，默认 5s）。上限固定为 1024 帧与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计（2026-10-16 修正：原先帧数只数排队、不含在途整批，停读连接到第 2049 帧才拒绝），不开放配置键；帧数上限维护者 2026-10-16 决定保持 1024，理由：场景 Sync / Lockstep 每次每连接只推几帧，上千帧突发不现实，需要更大突发时再评估；没有 Gate 那样的全进程驻留预算，最坏 `max_connections × 字节上限`（默认约 20 GiB，只有全部连接同时停读才会达到）；推送入队多一次 payload 复制；每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。只有包内基准（往返延迟、分配与修前无显著差异）与生成 demo 场景用例，未做重档压测 | 重档 Sync/AOI 压测核对推送路径 CPU 与内存；部署需要不同队列上限或全局驻留预算时再加 `player_access.tcp` 键；见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站) |
| 独立 Gate | 已实施 Gate/Game、广播与 Sync/Lockstep；本机功能通过，较重 AOI 负载失败；旧GOMAXPROCS4重档802.919秒先Gate准入耗尽、后Nest满；2026-10-10快8/慢8/默认并行度（实际10）重档15分钟通过；2026-10-15 v1.27 代码按旧4并行度配置重档30分钟通过（业务P99 18.64ms、>50ms 0.236%，拒绝0），多项改动叠加、单样本，旧失败根因未关闭 | [设计与分阶段验收](../framework/GATEWAY.md)；详见[Gate/AOI性能报告](../performance/GATE-AOI.md)；随v1.25.0发布但保留重档失败，不用本机多实例代替多进程/跨机矩阵 |
| schema升级 | DAO自动迁移已撤销，运行时只接受当前schema；离线迁移按[schema变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo有执行器与`cmd/schemamigrate`，2026-10-15真实Mongo演练一次 | 业务明确离线转换和保留数据范围；在线双读与生成器骨架（`roost add migration`）未实施，上线后重新决定 |
| Player跨机 | 以已落库数据为准，不要求本地WAL热迁移；搬的是sid，按[参考流程](../framework/PLAYER-MIGRATION.md)停写→排空WAL→释放单实例锁→切路由→新进程空WAL接管，game-demo有检查工具`cmd/sidmove`；2026-10-15同机两进程演练通过（含崩溃与未排空场景） | 真实跨机网络、卷迁移、客户端重连路径未演练；单个玩家改绑sid无框架能力；框架级迁移命令上线后评估 |
| Lockstep历史 | 默认自动保留3600帧（60Hz 60秒 / 30Hz 120秒），越界追帧明确失败并通知客户端终止、不自动重同步（D1，维护者2026-10-15决定C，`a6376934`）；本地108000帧逻辑长局与默认窗口长局有界已验证 | 真实网络断线超过窗口的端到端演练、按业务帧率与断线时长核对窗口；全量快照恢复（D1方案B）待具体游戏需求；见[Sync现状与接入安排](../framework/SYNC-OPTIMIZATION.md) |
| Lockstep TCP 追帧限速 | 已处理（`640e9fac`；维护者 2026-10-16 决定方向 A）：`lockstep.TCPSender` 两条通道分开，实时帧 `SendDatagram` 仍经 `PushLockstepSession` 只准入，追帧页 `SendReliable` 改经新的 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出——追帧随链路速度放慢，属于追帧的出站积压只有在途一页，`CatchupSendTimeout` 对 TCP 发送器重新生效（超时的页还在排队就撤回），超时计入 `CatchupMaxFailures`。修前（`b1a06152`）真实回环 TCP 上约 8 MiB 历史、客户端先停读再慢读，第一批页就把连接推过出站上限被关闭；停读连接的追帧以“session not found”放弃，从未超时。等写出只在 Match 的 `launchSend` goroutine 上，不在 Tick 协程或业务 worker（`fctx.BlockingError` 守卫保留）；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。回归见 [lockstep/tcp_test.go](../../framework/sync/lockstep/tcp_test.go)。触发阈值未在 toxiproxy 限速的真实链路上测 | 用 toxiproxy 限速链路做一次追帧演练（追帧耗时与链路速度对应、连接不断）；说明见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢) |
| Lockstep 追帧链路背压（独立 Gate / demo） | 未处理、影响未实测。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，返回 nil 只表示 Game 本地准入，追帧不随链路放慢：Game 侧队列满时返回 `ErrReliableBackpressure`（计一次追帧失败，每 Tick 一次，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时 Gate 关闭连接。生成 demo 的 `battleSender` 两条通道都走 `PushPlayer`（只准入），不经 `TCPSender`，同样没有追帧背压 | 独立 Gate 需要 Gate 在 socket 写出后回执（协议改动），需维护者决定是否做；demo 可改用 `TCPSender` 或给可靠通道接等写出版本，需先定 demo 是否演示嵌入 TCP 追帧 |
| 自身视图（self_sync）与 Sync 慢消费者规则 | 2026-10-16 起 `access.player.self_sync: true` 让玩家经 Entity Sync 收到自己的 Player（DESIGN-TODO D6，`policy.SelfView` + `wiring/selfsync`）；2026-10-17 起推送在活着的连接上被拒即关闭该玩家连接、进程驻留预算耗尽整轮重试，SelfView 不再在原连接上重开（维护者批准“A + A'”，[impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)）。限制：①同一个 `SubjectSyncState` 只能登记在一个 Manager（脏通知器只有一个），工程里另有自建 Manager 也登记 Player（game-demo 的场景）时与 self_sync 互相覆盖，当前没有守卫，文档要求不要同时开；②一帧上限默认取 gateway 默认的 `max_payload_bytes`（1 MiB），部署调小 `player_access.tcp.max_payload_bytes` 而不改 `selfsync.Config.MaxFrameBytes` 时，超限帧被接入层拒收、连接被关闭，客户端重连后的全量同样超限，表现为反复断线重连（`player.sync.push_refused.total{outcome="closed"}` 持续增长）；③玩家实体的帧在 Manager 内编码失败（单个实体包超过 `frame.Limits` 硬上限，如组件超过 64 KiB）时 Manager 不经传输丢会话，连接不被关闭，玩家留在线上却没有自身视图，直到重新登录；由 `entitysync.self_view.stranded_players` 暴露，game-demo 场景同样（成员留在场景、没有自己的视图）；④Game 进程出站驻留预算（Game Ingress `outbound.resident_bytes`）持续耗尽时 Sync 每轮在第一个被拒会话处整轮重试：排在它后面的会话本轮不推送，本轮已准入的会话下一轮改收全量（`requireSnapshotsAfterRetry`），带宽被放大、延迟上升；发不出去的绑定在 `outbound.max_age` 到期时被关闭、释放预算，业务提交不受影响（on_change 冻结内容受 `MaxFrozenBytes` 约束）；未做重档压测；⑤Gate 侧自己的驻留预算耗尽仍关闭连接（任何入队失败都关），没有 Game 侧那样的“整轮重试”例外；⑥不提供 C# 的 subject 载荷解码样例；⑦生成物需要包含 `wiring/selfsync` 的 core（当前主线之后的下一个发布），`minimumVersions.Core` 发版时再定；⑧只做了本机共享隔离环境的真实进程演练、真实 Gate + Game Ingress + NATS 的包内回归与生成工程回归，未做重档压测 | ①需要时给 `SubjectSyncState` 加“已被另一个 Manager 登记”的拒绝；②生成接线从 `player_access.tcp.max_payload_bytes` 取帧上限（需要接入层在 Nest Init 前读到配置）；③维护者 2026-10-17 决定 B：编码失败不关闭连接，靠 `entitysync.self_view.stranded_players` 告警（内容超限是数据 / schema 问题，断线重连不能修复，反复重连只增加负载）；④按发送能力配 `outbound.resident_bytes` 并看 `player.sync.push_refused.total{outcome="retried"}`；需要时让 Manager 在 RetryLater 时跳过被拒会话、继续推送其余会话；⑤需要时 Gate 对自身预算耗尽回 NotAdmitted、由 Game 侧按预算例外处理；见 [PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)、[GATEWAY §4](../framework/GATEWAY.md#慢消费者跟不上就断开) |
| SyncBus投递 | 有限次数、保留期限及容量；并非绝不丢。定位为默认可丢广播（维护者2026-10-15），兜底写法与可运行模板见[guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底) | 需要最终一致的业务自建快照/对账闭环，不能仅测重试成功；框架级对账为下一阶段候选 |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行observer；15分钟常规及快8/慢8/默认并行度重档通过；旧4并行度失败保留，2026-10-15 同配置30分钟复测通过，未作单变量收益归因 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)及[性能报告](../performance/GATE-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验 |
| AOICluster 容量抖动 | 2026-10-17 起 Cluster 的 `MaxVisible` 在每次移动后都严格等于候选内最近 N 个（同档移动也重选），容量层没有滞回：两个目标在第 N 名附近来回时每次移动都会 Leave/Enter，下游随之退订 / 冷创建。`BenchmarkClusterFlushRandomWalkCapped` 同负载 97 步 Enter 25012 → 28918（+15.6%）；每步 CPU 5.8ms → 10.7ms。单区域 AOI 的容量规则不同（只在未入选目标进入或移动时淘汰严格更远者），不受影响。Cluster 目前没有生产装配调用方 | 维护者 2026-10-17 决定不加滞回：抖动只在候选数超过 N 时出现；不设 `MaxVisible` 或把 N 设到常规候选数之上时不会重选、不会抖动（不设容量的基准无差异），容量只作为人群聚集时的带宽保护阀，那时的额外 Leave/Enter 可接受 |
| policy Drain | 等待retry和已承担事实；永久拒绝仍需修关系或ctx超时，Drain不放弃条目；超时返回`*DrainStalledError`列出卡住条目、原因与等待时长并计数（D3，维护者2026-10-15决定A，`b86a1286`） | 拒绝、关闭和排空组合；“放弃出口”（D3方案B）需先梳理永久拒绝的判定来源 |
| Skill Runtime | 不参与Nest回滚；Host副作用和确定性由业务契约保证 | Host能力、重入、阻塞、恢复和回放测试 |
| 观测接线 | ExportMetrics/admin元数据不是自动外部监控或审批系统 | 部署实际接线与鉴权审计 |
| Remote WAL级Rejected处理 | 已处理（`38b710a2`，随 v1.27.0 发布，维护者 2026-10-14 选“直接精简”）：Durability 1/2/3 的正式链路不产生 Remote Rejected（投影器写权威冲突是 `ErrProjectionConflict` fatal）；三个测试注入函数 `MongoCommitter`/`Backend.RejectRemoteCommitsInTransaction` 与 `Manager.RejectRemoteTransaction` 已删除，strict/pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`。tracker 的 Rejected 收尾（回滚、隔离、仅内存卸载、重载、交付 false）只剩 Durability 0 来源，保留 | 守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore/MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict/pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`） |
| Remote请求struct与Sender | handler 参数用带 `remote:"..."` 字段的请求 struct 时，struct 必须留在 handler 包（访问器生成在这里），sender / syncsender 子包又看不见 handler 包类型；`roost generate` 总生成 sender，所以这种 handler 生成失败（修前修后都不可用，2026-10-16 起改为专门报错、不再建议移走类型）。维护者 2026-10-16 决定先写清限制，不改生成结构 | 需要时再设计：让 sender 携带这种 struct（改生成结构），或允许单个 handler 不生成 sender |
| 新用户接入缺口 | 2026-10-16 新用户按 beginner 流程实测的四项均已由维护者同日决定并实施：玩家看自己状态（D6 折中方案：`access.player.self_sync`，剩余限制见下文“自身视图（self_sync）”）；未登录玩家得到 `CodeInternal`、first-business 不含首登 GetOrCreate（D7 方案 C：`errcode.CodeEntityNotFound` 与 `roost add endpoint --lifecycle`）；生成的服务包名大写（D8 方案 C：新工程小写、已有工程跟随目录）；DAO 库名是代码常量（D9 方案 B：骨架 `db=<project>` 与 `roost id entity`）。库名取运行配置（D9 方案 A）保留待评估。2026-10-17 第三轮试用登记三项：配置段与 next 提示两项已修复（配置段剩余部分按维护者决定 B 由 doctor `config-sections` 只读检查），packer 同日由维护者决定并实施（self_sync 默认 packer 由生成器生成），见上文“新用户试用第三轮”。第四轮登记三项：共享资源名按工程名派生、endpoint 业务时间参数已修复，按实体的持久定时器待维护者决定（D11），见上文“新用户试用第四轮” | [DESIGN-TODO D6～D9](DESIGN-TODO.md#d6-玩家看自己状态entity-sync-自身视图的接入骨架) |
| 删档后同 ID 重建 | GetOrCreate 路径已修复（维护者 2026-10-17 决定 [D10](DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，范围只守 GetOrCreate，`572d87c2`）：冷加载对删过档返回独立哨兵 `entity.ErrAuthorityEntityDeleted`（不满足 not found，Remote 托管实体查元数据墓碑），生成 GetOrCreate 不重建、发往该 ID 的 Nest 请求回复 `entity_deleted = 3`，不进 WAL、不 fence。**剩余限制（约束）**：直接 Create（handler 内或 lifecycle `Create`）不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID（也不要对存了但没加载的 ID 直接 Create）；违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来，处置见下一行。`EntityManager` 的 ID 生成器同样必须永不重复。“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记业务审计（D10 审计结论） | 回归 `TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo，已去掉 KNOWN_RED 跳过）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`；直接 Create 的保护按维护者决定不做（范围收窄前的实现已撤回，见 D10）|
| 删档后同 ID 重建：违反约束或修复前写下的冲突记录 | 修复前已进入 WAL 的“删档后重建”记录，以及违反上一行约束（直接 Create 复用删过档 / 已存储的 ID）写下的记录（首个 `Put expected=0` 撞墓碑或已有文档），不会被跳过或放宽：启动投影恢复仍报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来。框架不自动删记录、不跳过 | 人工处置（只在确认是这类记录时）：①停进程，保留 WAL 目录与 Mongo 备份；②启动错误已给出事务 ID 与 `<库>/<集合>/<_id> expected=0 next=1`：在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算出玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份完整只读副本留证（它不推进 checkpoint、不跳过记录）；③线上尚未部署（维护者 2026-10-06）：直接停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本；上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码，框架不提供“跳过一条记录”的入口。见[维护手册 §4](README.md#4-故障处理) |
| Windows | 非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'`（编译全部测试不执行）和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异：打开中的文件不能改名/删除（nestwal 损坏记录检查、日志轮转、statslog），checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真（run 38500440362 的 6 个失败） | 本机测试结果不改变macOS/Linux支持范围；跨平台也成立的缺陷不归入此行 |

需要维护者做设计选择的事项（Lockstep 历史、SyncBus 投递、policy Drain、schema 迁移、Player 跨机）汇总在 [DESIGN-TODO](DESIGN-TODO.md)。[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据。[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本次核对是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零bug或100%行为覆盖。
