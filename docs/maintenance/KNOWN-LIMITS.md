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

## 新用户试用第四轮（2026-10-17 登记；1、2 已修复，3 按 DESIGN-TODO D11 方案 B 实施）

基线 `300d728a`（origin/main）。先复核第三轮三项：依赖带入的 Mod 配置段、`project next` 按工程现状填名字、self_sync 默认 packer，用 main 构建的 CLI 新建工程实测均已解决（零手写 packer 时客户端照样收到全量 + 每次修改的增量）。随后按新手流程做“双倍金币卡”（限时 Buff，按业务时间到期），在共享隔离环境起真实进程联调时确认下面三项。2026-10-17 复核三项均属实：1、2 先红后绿修复（`c3892c11`、`b3c9159a`），3 需要产品方向，登记为 [DESIGN-TODO D11](DESIGN-TODO.md#d11-按实体的持久定时器接入)。

### 1. 生成配置里的总线 / Data Engine 命名是所有工程共用的固定值（已修复，`c3892c11`）

**处置**：决定“与谁共享”的键全部按工程名派生——`nats.prefix: <project>`、`syncbus.prefix: <project>.sync`、`dataengine.database` 与 DAO 骨架 `db=` 同一规则（`daoDatabaseName`）、effects / saga 的主题与流 `<project>.effect` / `<PROJECT>_EFFECTS`、`<project>.saga` / `<PROJECT>_SAGA`、`saga.database: <db>_saga`、`remote_entity.mongo.database: <db>_remote`、`etcd.service_prefix: /<project>/services/`。规则写在 Wiring Mod 声明的 `example` 占位符里，新建与后加 Mod 的段同一规则（[PROJECT_GENERATOR §5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)）；回归 `TestGeneratedSharedResourceNamesFollowTheProject`。与登记时建议的差异：Data Engine 库与 DAO 同库（`<project>`，不是 `<project>_engine`），沿用改前“两者同为 game”的布局。**剩余限制**：已有工程配置不改写，旧工程之后追加的段会写新名字、与已有段不一致，需要手工统一（CHANGELOG 升级说明）；doctor 不对“仍是框架固定值”给警告：维护者 2026-10-17 确认目前没有旧工程，不做。


**现象**：新工程的 `configs/service/config.game.yaml` 写 `nats.prefix: roost`（66 行）、`dataengine.database: game`（103 行）、`dataengine.effects.subject_prefix: roost.effect` 与 `stream: ROOST_EFFECTS`（134、135 行）；同一工程的 DAO 已按工程名生成 `//roost:dao coll=player db=r4`（D9 方案 B）。

**复现**：`roost project new r4 -module example.com/r4 -mods configdata` → `roost add access player --service game` → 看上面四行。

**影响**：两个工程共用一套开发用 Mongo / NATS 时，Data Engine 的 outbox / receipts / transactions 落进同一个 `game` 库、effects 走同一条流；[demo README 常见问题](../../demo/README.md#常见问题)记录了后果：上一个工程留下的 outbox 行没人 ack，30 分钟后新工程 `dataengine outbox: hard backlog limit exceeded` fail-stop；共用 `nats.prefix` 时 RPC 被别的流抢答（`unsupported rpc response version 0`）。新手只能在出事后从 FAQ 里找到这几个键。saga 的 `subject_prefix` / `stream` 同样不随工程（README 同一节）。

**建议修法**：与 D9 方案 B 一致，生成时按工程名填这几项（`nats.prefix: <project>`、`dataengine.database: <project>_engine` 一类、effects / saga 的 subject 与 stream 带工程名）；已有工程不改写已有值，doctor 可以对仍是框架缺省值的给警告。

### 2. `roost add endpoint` 只能从请求字段取参数，服务器决定的值（业务时间）无处可放（已修复，`b3c9159a`）

**处置**：handler 参数名 `nowUnix` / `nowUnixMilli`（`int64`）由 endpoint 骨架从 `app.BusinessClock(controller.Registry()).Now()` 填入，不读请求；其他请求里没有的参数照旧拒绝，报错写明服务器决定的值不放进请求（[CODEGEN_REFERENCE endpoint 段](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)）；回归 `TestEndpointFillsBusinessTimeFromTheServer`。只识别业务时间这一类约定名；其他服务器决定的值（玩家状态、配置）按约定在 handler 里读，不做参数，没有 `--server-param` 一类的通用标记。


**现象**：handler 需要业务时间时（`handlerUseDoubleCard(target, nowUnixMilli int64)`，与 game-demo 的 `nowUnix` 参数同一写法），`roost add endpoint UseDoubleCard --handler shop` 拒绝：`protocol UseDoubleCardRequest has no field matching Nest parameter "nowUnixMilli"`（`codegen/internal/roost/add_workflow.go:620`）。

**影响**：报错只告诉新手“请求里缺这个字段”，最省事的改法是在请求里加 `NowUnixMilli`，于是业务时间由客户端决定，客户端可以随意延长限时效果、提前领取冷却奖励。正确写法（endpoint 里取 `app.BusinessClock(controller.Registry()).Now()` 传给 Sender）只能整段手写，game-demo 的 controller 都是手写的。

**建议修法**：endpoint 生成器认识“服务器提供”的参数，例如约定名 `now` / `nowUnix` / `nowUnixMilli`（`time.Time` / `int64`）由业务钟填入，或 `--server-param nowUnixMilli=business-clock`；其他请求里没有的参数，报错写明“业务时间等服务器决定的值不要放进请求，写在 endpoint 里”，并给出手写示例。

### 3. 按实体的持久定时器接入（已实施，D11）

**处置**：维护者 2026-10-17 决定 [D11](DESIGN-TODO.md#d11-按实体的持久定时器接入) 方案 B 与离线语义（框架不追帧，离线期间到期的在加载后只触发一次，补算由业务按回调参数自己做），已实施（`a2b4f4d8`、`5581deeb`）：`roost add timer --entity <Entity>` 生成组件、节点类型、Tick handler 与驱动接线，驱动在 core（`framework/nest/entitytimer`）按已加载实体的最早期限只给到期实体投递 Tick；game-demo World 迁入同一生成器。用法见 [guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器)。

**剩余限制**：

- **离线实体不准点**：驱动只索引已加载实例，离线（未加载）实体到期不会被唤醒，只在下次加载时补触发一次。维护者 2026-10-18 决定不做“离线也准点”的全局驱动，由业务层处理：必须准点的动作（例如到点给离线玩家发邮件）在业务里发送 Nest 消息（发给目标实体或服务，由它在执行时处理），不要求准点的在加载后的回调里按 `Fire` 参数自己追帧。
- **卸载与已入队 Tick 的窗口**：驱动在投递前核对实例（已移除或被同 ID 新实例取代就丢弃），但投递准入之后、消息执行之前实例被卸载时，Nest 按“发给未加载实体的消息”冷加载它，于是加载后补触发一次（与发给该实体的任何消息相同；删档实体加载失败、不触发）。驱动自己不会再对已卸载实例发起投递。维护者 2026-10-18 决定接受这个窗口，不给 Nest 加“只投给已加载实体”的发送选项（会扩大 Nest 契约）。
- **构建了但没发布的实例**：实例在 `OnInitFinish`（发布之前）向驱动报告；发布失败（同 ID 已存在等，框架不调用其 `OnDestroy`）且带有期限的实例留下一个条目，到期时因同 ID 已有别的实例而丢弃；同 ID 没有实例时按“加载中”投递，Nest 照常冷加载该实体并补触发。条目只在到期前占内存，不会重复投递。
- **Remote 托管与 noPersist 实体不支持**：add 时拒绝。Remote 实体所有权在进程间迁移，迁移中的到期语义未定义；要支持需另行设计（与 DESIGN-TODO D11 方案 C 同一方向）。
- **去掉 `timers` 条目后生成部分不自动删除**：与其他按清单渲染的文件相同，需要手工删 `*_component_gen.go`、节点类型、Tick handler 及 DAO / 实体里的两处字段；维护者 2026-10-18 决定维持现状、不加回收逻辑。
- **game 服务多一个 Mod**：生成的停机总预算按 Mod 数计，game-demo 的 `shutdown.total_timeout` 由 114s 变为 117s（实体定时器 Mod 本身停止是即时的，这 3s 是每个 Mod 的保底）；维护者 2026-10-18 决定接受，不改 app 保底规则。
- **端到端用例用的是内存权威**：`TestEntityTimerGeneratedProjectEndToEnd` 以不落库 committer + 内存 loader（卸载时记下 DAO 持久字节、按 `RestorePersisted` 水合、经 EntityManager 发布）代替 DataEngine + Mongo，删档用测试里的删除准入门；节点是普通 DAO 字段，Mongo 往返由 DataEngine 自身的测试覆盖。真实 Mongo 下的定时器重启演练尚未做。

**原登记（2026-10-17）**：

**现象**：`infra/base/timer.Scheduler` 是宿主驱动的通用堆：期限持久化、加载后重建、谁来 Tick 都由业务决定（[guide/10-time §3](../framework/guide/10-time.md#3-scheduler)）。现成的接入只有 game-demo World 的 `TimerComponent`（模板 263 行，期限存 `WorldDao.Timers`），由服务进程的 ticker 定期发 `Sync_TickWorldTimers` 驱动；它与 World 的活动阶段写在一起，生成器没有对应命令。

**影响**：按玩家的到期动作（Buff 到期推送、建造完成、行军到达）要把 TimerComponent、TimerNode DAO 与 ticker 整套复制到 Player，并自己决定怎么驱动每个玩家的 Tick；`Delay_` 又不持久（NEST_RUNTIME“延时 Sender 不持久”）。新手大多退回“存到期时刻、读取时按业务时间判断”（本轮双倍金币卡就是这样写的），到期那一刻服务器不会做任何事。

**需要维护者决定**：是否提供通用的 DAO 期限组件（例如 `roost add timer --entity Player` 生成节点 DAO 与组件，框架负责重建与持久化钩子），以及按实体的 Tick 由谁驱动（进程 ticker 按最早期限只给到期实体发消息，而不是每秒给所有在线实体发）。

## game-demo 副本排行：超过去重环后重放会重复计分（已修复）

**类别与基线**：demo 接入问题，非 rank 公开契约的实现缺陷；在 `91606e48` 与 `cac62833` 主线生成的正式 game-demo 上确认。rank 的 `UpdateAdd` 只记同一玩家最近 8 个请求 ID（[types.go](../../service/rank/types.go) 的 `maxAppliedRequests`），是去重环、不是通关账本；[FinishDungeon](../../demo/game/controllers/player/finish_dungeon.go.tmpl) 原来每次成功领取或重放都提交 `+1`（为了补上“奖励已提交、排行没提交就崩溃”的那一分），而 [ranking](../../demo/game/ranking/ranking.go.tmpl) 承诺同一局只计一次。

**触发与结果（修前）**：同一玩家在奖励领取窗口内连续成功通关 9 局，再重放第一局：经验账本仍 9 条、World 发放经验仍 900，排行却为 10（第一局的 `clear:<runID>` 已被后来 8 个请求挤出去重环，奖励事务返回 `Claimed=false` 后端点仍调用 `UpdateAdd`）。审查附的探针（rank 自带 Redis 替身）修前原文：`nine clears then replaying the first gives 10 leaderboard points; want 9 (reward claims remain 9)`；新模板回归（真实 Redis 上的 rank RedisStore，10 局后重放第一局）修前原文：`ten clears then replaying the first gives 11 leaderboard points; want 10 (reward claims remain 10)`。

**根因**：与 `a2afea41` / `ce0d5fa5` 修过的两本领奖账本同一类——一个会遗忘的去重存储（这里按条数遗忘），遗忘点与重放的准入窗口（领奖窗口 4h + 宽限）没有对齐。

**修复**：不新增第三本有期限的账本，而是让排行的准入就是奖励的准入。领奖事务在记账本、发经验的同一笔事务里给 `PlayerDao.DungeonClears[赛季]` 加一（只在真正付款时），首次领取与重放都返回当前通关数 `ClaimResult.Clears`；端点用 `UpdateMax`、不带 requestID 把榜抬到这个数。窗口外的一局在计数之前就被执行时间的窗口判断拒绝；窗口内的重放只重复提交同一个快照；重复、并发、乱序快照不加分也不降分；排行提交失败后，重放或下一局的快照把漏掉的分补上。通关数不裁剪，按赛季分开（改 `ranking.CurrentSeason` 后新榜从 0 开始）。`handlerClaimDungeon` 多一个 `season` 参数（生成 Sender 随之变化）；PlayerDao 新增持久字段 `dungeon_clears`。

**回归**：模板 [finish_dungeon_test.go](../../demo/game/controllers/player/finish_dungeon_test.go.tmpl) 的 `TestDungeonClearsBoardCountsEachRunOnceRealRedis`（`ROOST_REVIEW_REDIS`，挂入 `scripts/test-demo-generated.sh`）：10 局后重放第一局、并发重放 4 局旧的、一局提交排行失败后重放补报，榜分别为 10、10、11；[claim_dungeon_test.go](../../demo/game/handler/claim_dungeon_test.go.tmpl) 的 `TestTheClearCountMovesOnlyWhenARunIsPaid`（不需外部依赖）：10 局计数、重放不加、窗口外拒绝不计数、不带赛季拒绝、换赛季从 1 起、持久化字节恢复后计数不变。审查的探针脚本已由这两条取代并删除。

**升级**：线上尚未部署，不做兼容：已用旧 demo 跑出的 `clears` 榜是 `UpdateAdd` 累加值，与新的通关数快照不同源，升级时清空该榜（rank 前缀下 `clears` 榜的键），玩家通关数从 0 开始。

**边界**：UpdateMax 遇到相同值保留较早的 tie，所以补报的那一分在同分时按补报时间而不是原通关时间排序。累计值按赛季而不是终身划分（见下“维护者决定”）。

**维护者决定（2026-10-18，按推荐）**：通关数按赛季计（已实施）。理由：与榜的 Season 身份一致，换季不带旧数；终身累计要一个字段并在换季时另做初始化，不采用。

## game-demo 活动结算榜：旧累计回复覆盖新分数（已修复）

**类别与基线**：demo 并发接入问题，`91606e48` 主线的 `ActivityRunner.Contribute` 用无条件 ZADD 写协调器返回的累计分。协调器允许并发调用，回复没有顺序保证；不是 activity 框架计分丢失，也不是业务奖励额度的选择。

**触发、证据与影响**：同一玩家的两个不同通关请求先后在正式协调器提交，延迟第一条回复；第二条回复先把真实 Redis 结算榜写为 2，再放行第一条，榜降回 1，而协调器 Participant.Score 仍为 2。修前回归原文：`coordinator score is 2 but settlement board fell from 2 to 1 after the delayed score-1 reply`（2026-10-18 在 `cac62833` 主线模板生成的 game-demo 上用同一用例复核，红文相同）。结算读取这份 Redis 榜，SLG 竞赛可能因此排错名次、少发或错发活动邮件；没有声称已复现实际邮件赔付。

**修复与回归**：Contribute 只加正分，因此用单键 Lua 调用 `ZADD GT` 原子保留较大累计值，首次写入仍可插入。[模板回归](../../demo/internal/service/game/activity_test.go.tmpl) `TestActivityBoardKeepsTheLatestCumulativeScore` 经正式 ActivityRunner、正式协调器（内存版本存储）及真实 Redis 固定上述回复顺序，修后应保持 2；无 Redis 时明确 skip。[生成工程脚本](../../scripts/test-demo-generated.sh) 已接入这条用例，可在有隔离 Mongo / Redis / NATS 的环境运行整套脚本；或在新生成并 replace 到当前 core 的 game-demo 里执行：

```bash
ROOST_REVIEW_REDIS=127.0.0.1:6379 go test -mod=mod -race -count=1 \
  -run '^TestActivityBoardKeepsTheLatestCumulativeScore$' ./internal/service/game
```

**边界**：`ZADD GT` 需要 Redis 6.2 及以上（共享隔离环境为 8.x）。只修复成功回复乱序导致的回退。Redis 写失败仍只记日志，协调器与结算索引不是一个事务，没有新增自动对账；Redis Cluster、跨机延迟与长稳未在这条回归中验证。临时接法也应使用原子单调写，不能在应用层先读再无条件写；业务将来允许扣分时，需要改用版本化快照或其他明确的顺序方案。

## game-demo 活动贡献：同一局在后来的窗口重放会再计一次（已修复：只在首次付款时贡献）

**类别与基线**：demo 接入问题，与上面副本排行同一类（去重范围没有覆盖重放的准入窗口），在本轮复核副本排行时发现，`cac62833` 主线模板。[FinishDungeon](../../demo/game/controllers/player/finish_dungeon.go.tmpl) 原来在领取与重放时都调用 `ActivityRunner.Contribute`；[Contribute](../../demo/internal/service/game/activity.go.tmpl) 按**调用时的业务时钟**选窗口（300 秒一个），协调器按（窗口, 玩家, `clear:<runID>`）去重。同一局在领奖窗口（4h）内、跨过窗口边界后重放，落进新窗口，键不同，再加一分；理论上一局最多可进 48 个窗口，并影响按名次发的活动邮件。

**证据**：登记时的探针（生成工程里正式协调器 + 正式 ActivityRunner，临时工程，未入库）输出 `first window race-1792288200 score=1; replay of the same run in window race-1792288500 score=1 err=<nil>`。修复时的模板回归（正式端点，见下）修前原文：`first window race-1792292100 score=1; replay of the same run in window race-1792292400 score=1 activity="race-1792292400"; want 0 and no window (a replay contributes nothing)`。

**维护者决定（2026-10-18，按推荐 A）**：只在奖励首次付款时贡献，与副本排行的做法一致——贡献由权威领奖事务里“真正付款”（`ClaimResult.Claimed`）这一事实驱动，重放（`Claimed=false`）不贡献。未采用的 B：按这局的结束时间选窗口、重放落回原窗口由协调器去重，并让 Contribute 只为当前窗口自动开窗、拒绝已关闭窗口的贡献（保留窗口内补报，但改动面更大，要另定协调器对已结束活动的拒绝语义与 30 分钟预留 TTL 的关系）。

**修复**：端点在 `!result.Claimed` 时于排行提交之后直接返回，不调用 Contribute，响应不带 `ActivityID`。排行仍在重放时提交（它提交的是事务内通关数的快照，`UpdateMax` 重交无害）。Contribute 的注释写明协调器去重只在同一窗口内成立、调用方须每局只贡献一次。

**代价（已接受，写在端点注释、WALKTHROUGH 与 CHANGELOG）**：付款之后、贡献之前进程崩溃或协调器调用失败，这一分永久丢失，重放不补；端点打 Warn 日志并写明不会重试。活动是尽力而为，补报若落到后来的窗口本来就记错了窗口。

**回归**：模板 [finish_dungeon_test.go](../../demo/game/controllers/player/finish_dungeon_test.go.tmpl) 的 `TestAReplayContributesNothingToTheActivity`（无外部依赖，生成工程 `go test ./...` 即运行）：正式 Controller、生成 Sender、Nest 与 Player / World handler，正式 `svcactivity.Service`（内存版本存储，时钟可拨）；测试内的贡献方与正式 `Contribute` 走同一条协调器路径（`gameactivity.ID` 选窗口、`gameactivity.Key`、`ProgressRequestID`、窗口不存在时开窗再记），正式 Runner 在导入本包的 internal/service 包里，不能直接构造。断言：边界前一秒首次付款在窗口 W 记 1 分；时钟前进 300 秒重放同一局，新窗口为 0 且不报窗口；新窗口里新一局照常记 1 分；付款时贡献失败后重放不补。

**同类排查（事务外、可能被重放重复提交的累计写入）**：副本排行（`UpdateMax` 通关数快照）与活动结算榜（`ZADD GT` 协调器累计快照）是单调快照，重交不加分；World `ExpGranted`、`DungeonClears` 在领奖事务内只随付款变化；邮件领取、购买发货（platform 回执 + Player 账本）、送礼 saga（回执）、升级邮件与活动结算邮件（请求 ID）都有账本或幂等键。World `PlayersEntered` 由每次成功的 EnterGame 加一（客户端丢回复后的重试也算），语义是登录次数而不是首次付款，不改；`MatchesFormed` 每次组队 Commit 后加一，没有重放路径。没有发现其他需要按“只在首次付款时”处理的写入。

**边界**：只覆盖 demo 端点；协调器本身仍按窗口去重，业务若在别处直接调用 Contribute，须自己保证每局一次。

## 新用户试用第五轮（2026-10-17 登记；三项均已修复）

基线 `91606e48`（origin/main）。先复核第四轮两项已修复：新工程的 `nats.prefix`、`dataengine.database`、effects 主题与流按工程名派生（与 DAO `db=` 一致）；`roost add endpoint` 对 `nowUnixMilli` 参数生成从 `app.BusinessClock(controller.Registry())` 取值的代码，不再要求请求字段。随后按新手流程学配置表热更（策划改价不停服），在共享隔离环境起真实进程、经 ops `/admin/execute` 实测。正面结论：违反表规则的数据被整次拒绝、报错精确到表 / 行 / 字段 / 规则、旧快照继续服务；改价后下一次购买即按新价。

### 1. 文档里“已修复（提交号）”指向 rebase 前的旧提交（已修复）

**现象**：第四轮两项写的已修复提交号（b717fd14、990af957）只存在于 rebase 前的历史，main 上对应 `c3892c11`、`b3c9159a`；CHANGELOG 定时器一条的 f8851f4a 在 main 上是 `09846b5f`。

**修复**：根包门禁 `TestDocumentedCommitsAreReachableFromHEAD`（[doc_commit_refs_gate_test.go](../../doc_commit_refs_gate_test.go)）：跟踪的 `*.md` 里反引号括起来的 7～40 位十六进制串（至少一个数字、一个 a～f）视为提交号，必须是 HEAD 的祖先；自检扫描 `cac62833` 必须报出 KNOWN-LIMITS 里的 b717fd14。修前现状扫描共 10 个解析不了（都是确有其事的提交，只是不在 main 历史里），门禁原样报红；逐个按补丁内容找到 main 上改动相同的提交并改正：KNOWN-LIMITS 的两处、CHANGELOG 的 f8851f4a 与 gapmap 分支实跑、REFACTOR-2026-10-17-timer-scheduler 的三处“原提交”、PERF-ROUND-20261015 的 1bc6dd46 / ca0cbddf、EXTERNAL-VERIFICATION 的 35ef33f9 / 5e60a03c（测时在分支上的，写明“分支上与 main 某提交改动相同的提交”）。豁免清单 `documentedCommitExemptions` 现为空；条目必须写明分支与理由，不再被引用或已可解析时门禁要求删掉。

**限制**：浅克隆（CI 默认 depth=1）整条跳过，只在本地完整检出与 release 工作流（fetch-depth: 0）生效；纯数字短 SHA 与不带反引号的提交号不检查；只查“在 main 历史里”，不查是不是文档所说的那个改动。分支上的文档引用本分支提交时，rebase 后同样会红：引用本轮改动时优先写测试名 / CHANGELOG 条目，或合入后改成 main 上的提交号。

### 2. 框架没有现成的配置表重载运维命令（已修复）

**现象**：`wiring/configdata` 的 Mod 只注册 `*configdata.Store`；只有 game-demo 在 gm.go 里手写了 `gm.config.reload`，`hotcode` / `bus` 都有 `RegisterAdminCommands`，configdata 没有。

**修复**：新增 `configdata.RegisterAdminCommands(reg, store)`（[admin.go](../../framework/configdata/admin.go)），注册 `configdata.reload {reason?, dry_run?}`、`configdata.rollback {reason?}`、`configdata.status {}`；configdata Mod 在 Provide 时自动挂到 App 的 admin 注册表（缺 admin 能力时 Provide 失败），所以所有带 configdata Mod 的生成工程都自带。game-demo 删除 `gm.config.reload`，改用框架命令。修前回归原文：`admin registry lists [], want configdata.reload registered by the configdata Mod`。

### 3. 重载结果不说明哪些表变了；内容没变也报成功（已修复）

**现象**：只改 `configs/table/shop_item.csv` 忘了 `roost generate` 就重载，结果 `{"ok":true,"data":{"version":4}}`，价格没变。

**修复**：Snapshot 在 finalize 时按成员记下与 `Hash` 同源的内容摘要和表行数，`configdata.Compare` 比较两代；`Store.ReloadWithChanges` / `RollbackWithChanges` 与 `ReloadOutcome.Changes` 带上逐成员变化。`configdata.reload` 回答 `version`、`previous`、`hash`、`changed` 与 `changes`（新增 / 删除 / 变化的表及行数）；内容完全没变时 `changed=false`，message 写明 no table changed 并提示改了 CSV 要先 `roost generate`，日志一条 Warn，指标 `configdata.reload.total{result="unchanged"}`。**版本号仍递增**（选择及理由见下）。guide/07-config §5 写明重载只读 `config_data.dir` 下的 JSON。

**真实进程实测（2026-10-18）**：`-mods configdata` 新工程加 `shop_item` 表（price `min:"1"`），打开 `ops.admin_enabled`，经 `/admin/execute`：`GET /admin/commands` 返回 `["configdata.reload","configdata.rollback","configdata.status"]`；只改 CSV 后 reload 返回 `"changed":false,"changes":{"unchanged":1}`、version 1→2 并带上述 message；`roost generate` 后 dry_run 报 `shop_item` 变化（2 行→2 行）且不发布，reload 发布 version 4；把一行 price 改 0 的 JSON 被整次拒绝（`table shop_item row 2 (key 2) field price: min: value 0 is below min=1`），旧快照继续服务；rollback 回到 version 2 并报 `shop_item` 变化；`/metrics` 为 `configdata_reload_total{result="ok"} 2`、`{result="unchanged"} 1`、`{result="failed"} 1`。

**维护者决定（2026-10-18，按推荐 B）**：内容没变的重载照常递增版本（已实施）：照常发布、递增版本，但回答、日志与指标明确标“没有变化”。理由：监听者依赖每次重载都执行——game-demo 的开关表监听者用重载把 `gm.flag.set` 的进程内改动恢复成表里的值（`gm.flag.set` 的说明就是这么写的），跳过发布会让它静默失效；版本号本来就不是内容版本（失败的构建也会占用一个号），内容是否变化看 `changed` / `hash`。未采用的 A：内容没变时不发布、不递增、监听者不跑，回答“没有变化”；代价是上述“重载恢复表值”的用法失效，需要另给 featureflag 一个“恢复表值”的入口。

## 新用户试用第六轮：待修问题（2026-10-18 登记；1、3～6 已修复，2 按 DESIGN-TODO D12 方向 A + B 已实施）

基线 `91606e48`（origin/main）。本轮按 guide 目录一次学剩下的模块：Saga（06）、框架服务（09）、Remote（05）、Skill（08）、Lockstep（04 后半）、自建 RPC 与可观测性（11 / 13），在 r6 工程里逐块用 CLI 生成、写最小业务、编译与 doctor，必要处在共享隔离环境起真实进程或进程内 Nest harness 探针。正面结论：托管框架服务用 `services.<name>.framework` / `uses` 接入后生成类型化访问器、`rank.Submit` 带 requestID 幂等；自建 RPC 把拥有者从 game 挪到独立进程只改 roost.yaml、业务代码不变（只验证到编译与 doctor，未起两个进程实测跨进程调用）；`roost id next errcode` 同时看手写与生成的码；Remote 托管实体在没有 Remote 运行时的 Nest 里写入被拒、不会当本地写。修复由维护者在 main 上进行。

### 1. `roost add saga -steps` 按字母重排并去重步骤（会改变执行与补偿顺序）

**现象**：`roost add saga gift_gold -service game -steps debit,credit` 生成的 `Definition()` 是 `{credit, debit}`。Saga 按 `Definition.Steps` 的下标推进（`framework/saga/engine.go` 里 `CompletedSteps = record.Step + 1`，补偿按下标倒序），所以顺序就是业务语义：生成物变成先给收礼方加钱、再扣送礼方，补偿顺序也随之反过来。

**根因**：`codegen/internal/roost/cli.go:312` 用 `splitList(*steps)` 解析步骤；`splitList`（`manifest.go:546`）返回 `uniqueSorted(out)`，为 Mod 列表这类无序集合设计。同名步骤也被静默去掉。

**影响**：`roost help saga` 的示例 `-steps reserve,deduct,deliver` 会生成 `deduct, deliver, reserve`。生成后编译、doctor 都通过，只有读生成代码才发现；照示例写的跨服交易会先扣款再预留。

**建议修法**：步骤用保序解析（拒绝重复名并报错），不经 `uniqueSorted`；回归覆盖 help 里的示例顺序。

**状态**：已修复（2026-10-18，见 CHANGELOG `[Unreleased]` 第六轮登记项 1）：步骤保序、重复名报错；同类的 `project new -services` 取第一个服务一并改为保序。

### 2. Saga 原生实体步骤与 effect 消费者没有生成支持

**现象**：`roost add saga` 只为每一步生成 Mongo 步骤的 `Subscribe*` 函数（`saga.SubscribeMongoStep`）；改实体 DAO 的步骤（扣 / 加金币这类最常见的步骤）应走 `saga.SubscribeDataEngineStep` 与 DataEngine step inbox，骨架注释点名了它但不生成，业务要自己取 JetStream 客户端、`JetStreamPublisher`、`DataEngineStepInbox`、stream / durable，再写派发 Nest handler 并在事务里绑定 receipt 的 StepHandler。game-demo 的送礼 saga 接线 `internal/service/game/gift_saga.go` 有 661 行。事务性 outbox（`nest.Emit` effect + Mongo inbox 消费者，demo 的升级发邮件）同样只有 demo 手写，生成器没有对应命令。

**影响**：跨服务副作用的两条正路都要照 demo 大段手写；新手更可能退回“事务后尽力调用 + 幂等 ID”，接受崩溃时少一笔（demo 的排行提交就这样写并在注释里说明了代价）。

**需要维护者决定**：是否为原生实体步骤生成 `SubscribeXxxNative` 与 StepHandler 骨架（Nest handler 签名带 step receipt），以及是否提供 `roost add effect <topic> --consumer <service>`。

**状态**：方向 A 已实施（维护者 2026-10-18 决定，见 [DESIGN-TODO D12](DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持) 与 CHANGELOG `[Unreleased]`“Saga 原生步骤与 effect 消费者的胶水进框架”）：原生步骤体 `saga.NativeStep`、步骤运行时 `kitsaga.Steps`、effect 消费者 `kitdataengine.SubscribeEffects` 进框架，手写量降到“步骤体 / handler + 一次登记”，game-demo 改用它们。**方向 B（生成器）同日维护者批准并已实施**：`roost add saga … -native a,b`（`steps.go` 的 `StartSteps` / `RegisterSteps` 与原生步骤的 Nest handler 骨架，去掉 `definition.go` 的 `Subscribe*` 辅助）与 `roost add effect <topic> -consumer <svc>`，doctor 的 `saga-steps:` / `effect-consumer:` 只读检查服务是否启动，真实依赖端到端 `TestGeneratedSagaNativeStepsAndEffectConsumer`（见 DESIGN-TODO D12“B 实施结果”）。**仍由业务写**：多进程分片时的准入 / 转交（框架提供 `WithAdmit` 与 `RunHandedOver` 扩展点，生成的 `RegisterSteps` 注释点名要补）。**A 的四个未验证项（2026-10-18 已验证，见 [DESIGN-TODO D12](DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持)“验证结果”）**：真实 Nest 调度下的原生步骤（正向、补偿、重复投递、结果未知、业务拒绝、`ErrFencedEntityPending` 交还租约、投影停到截止后跳过）由 `TestRealStepsRunNativeStepsUnderTheRealNestScheduler` 覆盖；两个 game 进程的跨 sid 赠礼（转交、正常停机 `Drain` 约 2.1s、kill -9 重启）恰好一次——演练发现并修复转交与消费者因命令时间的时区不同互相 `ErrIdentityConflict`、消息 nak 到 MaxDeliver 的缺陷；性能对照见 [Saga D12 前后对照](../performance/SAGA-D12-20261018.md)，原生步骤单进程容量登记在下文“能力与验证边界”；已存在 durable 的参数原地更新、不可变字段冲突现在点名流与 durable 并给处置命令。B 已评估，建议等容量决定后再做。

### 3. `saga.start_effect_stream` / `start_effect_prefix` 与 `dataengine.effects` 必须一致，却没有校验

**现象**：Saga 经 DataEngine 的 effects 流接收 Nest 发起的 start（`saga.start_effect_stream` 默认 `ROOST_EFFECTS`，`dataengine.effects.stream` 默认也是 `ROOST_EFFECTS`），两组键分别填写。两者不一致时进程启动失败：`mod saga start: saga: subscribe Nest starts: nats: API error: code=404 err_code=10059 description=stream not found`，不点名是哪两个键不一致。

**复现**：工程已有 dataengine 配置段后 `roost add mod saga`，再把 `dataengine.effects.stream` / `subject_prefix` 改成别的值（demo README 常见问题正是建议这样隔离环境）；`roost config check`、进程 `--check-config`、doctor 都通过（2026-10-18 在 r6 上把 effects 改成 `OTHER_EFFECTS` 实测），启动时报上面的错。

**建议修法**：同进程有 DataEngine 时 saga 的 start / result effect 流与前缀默认取 `dataengine.effects` 的值（显式写了才覆盖）；两者都显式写且不一致时 `--check-config` / doctor 报错并点名两个键。

**状态**：已修复（2026-10-18，见 CHANGELOG `[Unreleased]` 第六轮登记项 3）：saga 两个键不写时取 `dataengine.effects`、生成配置不再写；同进程有 DataEngine 时显式写了不同值，启动前 / `--check-config` / doctor `config-check:<service>` 拒绝并点名两个键。

### 4. 声明 `remote=managed` 的实体缺 Remote 运行时，生成与 doctor 都不提示

**现象**：`roost add entity Guild` 后把标记改成 `category=entity.EntityCategoryRemote remote=managed`、嵌入 `*entity.RemoteEntityBase`，不加 `remote_entity` Mod：`roost generate`、`go build`、`roost project doctor` 全部通过；写这个实体的 handler 在运行时返回 `remote entity: write capability disabled`（进程内 harness 实测，写入被拒、无提交），报错不说缺的是 `remote_entity` Mod。另两点：`add entity` 骨架注释只说“改成 remote=managed 时移到 EntityCategoryRemote”，没提要换嵌入 `*entity.RemoteEntityBase`；漏换时 `roost generate` 在这一个实体上中止整次生成，其他实体的 DAO 也没生成，随后的编译报错（`undefined: db.GuildDao`）把人引向无关的地方。

**建议修法**：工程里有 `remote=managed` 实体而所属服务没有 `remote_entity` Mod 时，generate / doctor 报错并给出 `roost add mod remote_entity -service <svc>`；写入拒绝的错误带上这条修法；骨架注释写明三件事（类别、remote=managed、嵌入 RemoteEntityBase）。

**状态**：已修复（2026-10-18，见 CHANGELOG `[Unreleased]` 第六轮登记项 4）：generate / doctor `remote-runtime` 在带 Nest 的服务全都缺 `remote_entity` 时拒绝并给出命令（部分缺为 WARN：实体由哪个服务写静态上看不出）；运行时错误 `entity.ErrNoRemoteRuntime` 点名修法；骨架注释写明三处；漏换嵌入的报错点名字段；无 DAO 的 Remote 实体 wire 能编译。

### 5. 技能目录在生成工程里没有任何地方编译，写坏的 JSON 通过 build / test / doctor

**现象**：`roost add skill Fireball` 生成 `game/skills/fireball.json` 与 `catalog.go`（`CompileAll`），但工程里没有调用 `CompileAll` 的地方，也没有测试。把 JSON 里的 `"flow": "finish"` 改成不存在的流程后，`go build`、`go test ./game/skills/`（无测试文件）、`roost project doctor` 全部通过；只有手动调用 `CompileAll` 才报 `parse Skill fireball.json: phases: [0].on: unsupported flow "explode_everything"`。`roost help skill` 只写“启动时：skills.CompileAll(...)”。

**影响**：坏技能要到业务自己接上编译（或上线后第一次施放）才暴露；与 tablegen 数据“生成 / 加载时一次检查”的做法不一致。

**建议修法**：首次生成 `catalog.go` 时同时生成 `catalog_test.go`（`CompileAll(skill.DefaultCompileEnvironment())` 必须成功），并在服务 Init 里调用一次、失败拒绝启动；doctor 加一项检查。

**状态**：已修复（2026-10-18，见 CHANGELOG `[Unreleased]` 第六轮登记项 5）：生成 `catalog_test.go`（缺时下一次 add skill 补上），doctor `skills:catalog` 运行它；服务 Init 归业务，没有改写，help 与 add 提示写明在 Init 调 `CompileAll`。

### 6. 文档小问题

- 生成工程的 `docs/ROOST_YAML.zh-CN.md`（`codegen/internal/roost/render_docs.go:398`）说托管框架服务是“account、mail、match、chat 之一”，生成器实际支持九个（另有 platform、global、activity、rank、session，见 `framework_services.go`）。`services.<name>.framework` / `uses` 的用法只在这份生成文档里，`roost help` 各主题里没有，新手查内置帮助找不到“怎样把排行接进已有工程”。
- GETTING-STARTED 把[技能火球](../../gameplay/skill/examples/fireball/main.go)列为不依赖外部服务器的示例，但它在独立模块 `gameplay/skill/examples` 里，在仓库根 `go run ./gameplay/skill/examples/fireball` 报 `main module does not contain package`，文档没写要先 `cd gameplay/skill/examples`。
- guide/11 只提醒“业务新增指标应控制标签基数”，没有一处示例说明业务代码用哪个 API 加计数器 / 时延（`OBSERVABILITY.md` 只列框架自己的指标）。

**状态**：已修复（2026-10-18，见 CHANGELOG `[Unreleased]` 第六轮登记项 6）：托管服务清单改为九个、新增 `roost help services`、火球运行方式写明先进入独立模块目录、guide/11 与 OBSERVABILITY 补业务指标写法。

## 实体定时器回调操作另一份调度视图（P2，已修复）

基线 `63b63fa8`（D11 合入后的 main），框架实现问题，SLG 接入实践发现。合法的实体定时器回调里经同一组件调用 `NewTimer` / `RemoveTimer` / `ChangeTimer` 时，`Host` 从中间 DAO 状态另建一份 Scheduler：回调登记的下一阶段（建造完成接行军）已写进 DAO，最外层 Tick 却按旧堆报告期限并覆盖内层报告，驱动索引丢掉它，直到重载或该实体的下一次定时器操作才恢复；回调成功取消 / 改期同轮已到期的另一节点，它仍按旧期限执行；回调显式改期自己返回 false。

修前 `TestHostCallbackOperationsShareTheCurrentTick` 原文：`next-stage timer persisted=1 but driver due=0001-01-01 00:00:00 +0000 UTC present=false`、`canceled stage still fired 1 times`、`current stage was not found`、`rescheduled stage fired 1 times at its old deadline`。

修复：`Host` 在当前 Tick 调用栈内共享调度器，内层操作按 `timer.Scheduler` 的 Tick 内规则生效、不报告，最外层落定后报告一次；返回 / panic 都清掉，不在事务间缓存。契约见 [impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动) 第 1、2 条。回调内 `Timer` / `Timers` 读不到自己刚写入的节点（这次修复留下的剩余项，2026-10-18 审查确认为回归）已修复：Tick 调用栈内改读当前调度视图，见 CHANGELOG `[Unreleased]`“实体定时器：回调里读得到自己刚写的节点”。

## 合法业务参数名导致 Sender 无法编译（P2，已修复）

基线 `63b63fa8`，Nest 生成器问题，SLG 接入实践发现。handler 参数名原样带进 Sender，与 Sender 自己的路由参数、局部变量、导入包、预声明标识符或包级 `handlerName…` 变量同名时生成物编不过：取消建造 `handlerSLGCancel(target, id int64)` 生成 `Sync_SLGCancel(ctx context.Context, id int64, id int64)`，报 `id redeclared in this block`；参数名 `nil` / `int64` / `handlerName<X>` 等遮蔽类冲突能通过语法解析、类型检查才失败。

修复：只在 Sender 的元数据副本里给冲突名加 `Arg` 后缀并继续避让已有的 `idArg` 等名字；handler 元数据、参数顺序、类型与线协议不变。回归 `TestSenderNamesDoNotCollideWithBusinessArguments`（对生成的 sender / syncsender 做类型检查），修前原文 `sender has colliding generated declarations: … id redeclared in this block (and 12 more errors)`。旧版本可在业务 handler 里把参数改名（如 `taskID`）暂避，不需要改协议。

## SLG 接入验收的范围（2026-10-18）

入口与业务清单见 [SLG-INTEGRATION](../framework/SLG-INTEGRATION.md)，脚本 `scripts/test-slg-generated.sh`。它证明的是：公共 CLI 生成的 DAO / 组件 / Sender / Nest / 实体定时器在 6 个业务组合（建造 → 行军、重放与取消退款、失败回滚、离线收益、地块竞争、研究与建造并行）下的事务与期限行为；上面两条已修复问题就是它发现的。它不证明：提交器是替身（不含 WAL fsync / Mongo）、离线恢复用 DAO 序列化而非真实重载、单进程、无 Redis / NATS；真实存储与恢复仍以 `test-demo-generated.sh`、`test-dataengine-generated.sh`、`test-remote-generated.sh` 各自的实际执行为准。脚本不挂在 `dataengine-env.sh test`，也不在根包门禁里，改相关生成器时需手动跑。夹具里的金币、时长、上限等数值是业务样例，不是框架规则；SLG 战斗、数值、赛季政策没有默认实现，不因此算框架缺陷。

## 能力与验证边界

架构后续事项：[Session 运行服务可选化、玩家接入层与 Sync 命名](ACCESS-SYNC-FOLLOWUPS.md)。Session 运行服务保持现状、不改为可选（维护者 2026-10-14 决定）；玩家接入目录已改名为 `game/playeraccess`（dd362116）；Sync 命名已随 v1.26.0 发布，源码 API 需要随升级迁移。

当前版本v1.27.0。v1.25.0 时维护者接受重档失败先发布；v1.26.0 已修复确认的放大因素（慢诊断全进程抓栈），v1.26.0 发布后同参数 15 分钟重档复测通过（业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、>50ms 0.0098%、最大 86.341ms），长尾收窄但未唯一归因；v1.27.0 发布后同参数延长到 30 分钟仍通过（业务 P99 12.49ms、Sync P99 14.71ms、>50ms 0.0038%，堆不随时间增长，业务最大 107.307ms），分别见 [v1.26.0 发布验收](../release/v1.26.0-VALIDATION.md#perf15)、[15 分钟复测](../performance/V1260-HEAVY-20261013.md)与 [30 分钟复测](../performance/V1270-HEAVY-30M-20261014.md)。2026-10-15 带采集的同参数重档定位长尾（[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1）：约 60% 的长尾事件集中在每个墙钟分钟第 2 秒，与宿主 MDM / CorpLink 周期任务同秒，表现为调度延迟而非进程内 STW / GC / 锁，未归因到 roost 代码；另有持续数秒、不在整分钟的簇尚未确认。文档没有重新证明全部历史缺陷已关闭。已合入修复的旧日期状态不再当当前待办；不确定的能力不写成已经支持。

2026-10-12 在 `6c36e0df` 上复验快8/慢8/GOMAXPROCS=0的相同重档，约6分钟出现 Gate 通道 `context deadline exceeded` 并关闭绑定，15分钟目标未通过。同参数诊断复跑约12分25秒失败，轨迹确认Nest慢请求自动全进程抓栈触发330.160ms STW，跨过Gate的250ms期限并放大故障；自动抓栈现已移除并通过Nest及race回归，同参数15分钟复验通过，Sync提交→客户端P99为18.66ms，零消息/HB拒绝、零outbound失败；仍有0.1813%的已订阅delta超过50ms、最大336.387ms，初始延迟未唯一归因。2026-10-10 的通过样本不能作为当前新增提交已稳定的证明。详见[本轮性能报告](../performance/REVIEW-20261012.md)。

| 项 | 当前结论 | 后续验收入口 |
| --- | --- | --- |
| Sync偶发长尾 | on_change按P99≤50ms验收；最大250.699ms样本根因未唯一确定。2026-10-15 采集：本机约60%长尾与宿主每分钟第2秒的周期任务同秒（调度延迟、进程内无额外工作），GC STW/标记辅助/锁/进程内定时器已排除；持续数秒的非整分钟簇尚未确认。宿主留 CPU 余量对照（`GOMAXPROCS=8`，方案 B）同日已做，长尾反而更多（>50ms 为基线 3～10 倍），无效 | 维护者 2026-10-15 决定先接受现状；以后有独占服务器 / Linux 实机时再复测（方案 A）；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1、§5 |
| Remote容量 | 已测80TPS完整1h（v1.24.0）；v1.27 代码 80TPS×30m 在共享隔离环境复测通过（零错误、核验通过、p99 278ms），非跨机上限 | 跨机RTT/故障/24h；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3 |
| Saga补偿容量 | 正向20/s、全补偿10/s已测；20/s全补偿饱和。v1.27 代码各 10 分钟复测通过，全补偿10/s 首测 P99 1.47s / max 2.01s 高于历史 1 分钟样本（0.6s）；夹具加分钟分段后同日复测 P99 0.64s、十个分钟段无增长趋势，首测高值未复现、原因尚未确认 | 真实步骤、混合负载与生产存储另测；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3、§5 |
| Saga 原生步骤容量（单进程） | 2026-10-18 实测（[Saga D12 前后对照](../performance/SAGA-D12-20261018.md) §2、§3）：两步原生 saga 经真实 Nest + DataEngine，单进程正向 5/s、全补偿 3/s 各 5 分钟通过；正向 10/s、全补偿 5/s 起饱和，步骤超时、重试放大，大部分 saga 以 failed / compensated 结束（过载时逐账户核对仍恰好一次）。D12 前的手写胶水与 `Steps` 同处饱和，不是 D12 引入。原因：① 一个 durable 在一个进程里逐条处理（nats.go `Consume` 回调串行），一条原生命令占住消费者 = Reserve + Nest strict 提交 + 等回执投影，单 durable 约 14～18 条/s；② 带回执 / effect 的记录在投影器里单条处理、前后各一次 checkpoint、不合批（`projector_replay.go` `isLocalBatchRecord`），全补偿每笔三条。另：投影停顿时在等回执的命令挡住同 durable 后续命令直到其截止。Mongo 步骤路径（正向 20/s 已测）不受此限 | **维护者 2026-10-18 决定 A：接受现状并文档化**，不做消费者并发（B）或投影合批（C）。已写进 [guide/06 §5](../framework/guide/06-saga.md#5-接入与运维) 与 §2.1、生成的 `saga/<name>/steps.go` 注释、`roost add saga` 的 next 提示与 `roost help saga`：单进程每个 durable 约 14～18 条/s，原生步骤 saga 正向约 10/s、全补偿约 5/s 开始饱和，超过后重试放大；量大时按 sid 分片多进程，原生步骤加 `WithAdmit` 准入。将来量级需要时再评估 B / C（B：`Steps` 加并发度选项，代价是实体屏障冲突与 `ErrFencedEntityPending` 交还增多、需新的真实依赖验收；C：改 DataEngine 投影契约，代价最高）|
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
| 新用户接入缺口 | 2026-10-16 新用户按 beginner 流程实测的四项均已由维护者同日决定并实施：玩家看自己状态（D6 折中方案：`access.player.self_sync`，剩余限制见下文“自身视图（self_sync）”）；未登录玩家得到 `CodeInternal`、first-business 不含首登 GetOrCreate（D7 方案 C：`errcode.CodeEntityNotFound` 与 `roost add endpoint --lifecycle`）；生成的服务包名大写（D8 方案 C：新工程小写、已有工程跟随目录）；DAO 库名是代码常量（D9 方案 B：骨架 `db=<project>` 与 `roost id entity`）。库名取运行配置（D9 方案 A）保留待评估。2026-10-17 第三轮试用登记三项：配置段与 next 提示两项已修复（配置段剩余部分按维护者决定 B 由 doctor `config-sections` 只读检查），packer 同日由维护者决定并实施（self_sync 默认 packer 由生成器生成），见上文“新用户试用第三轮”。第四轮登记三项：共享资源名按工程名派生、endpoint 业务时间参数已修复，按实体的持久定时器由维护者决定 D11 方案 B 并已实施（剩余限制见上文“新用户试用第四轮”第 3 项） | [DESIGN-TODO D6～D9](DESIGN-TODO.md#d6-玩家看自己状态entity-sync-自身视图的接入骨架) |
| 删档后同 ID 重建 | GetOrCreate 路径已修复（维护者 2026-10-17 决定 [D10](DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，范围只守 GetOrCreate，`572d87c2`）：冷加载对删过档返回独立哨兵 `entity.ErrAuthorityEntityDeleted`（不满足 not found，Remote 托管实体查元数据墓碑），生成 GetOrCreate 不重建、发往该 ID 的 Nest 请求回复 `entity_deleted = 3`，不进 WAL、不 fence。**剩余限制（约束）**：直接 Create（handler 内或 lifecycle `Create`）不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID（也不要对存了但没加载的 ID 直接 Create）；违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来，处置见下一行。`EntityManager` 的 ID 生成器同样必须永不重复。“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记业务审计（D10 审计结论） | 回归 `TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo，已去掉 KNOWN_RED 跳过）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`；直接 Create 的保护按维护者决定不做（范围收窄前的实现已撤回，见 D10）|
| 删档后同 ID 重建：违反约束或修复前写下的冲突记录 | 修复前已进入 WAL 的“删档后重建”记录，以及违反上一行约束（直接 Create 复用删过档 / 已存储的 ID）写下的记录（首个 `Put expected=0` 撞墓碑或已有文档），不会被跳过或放宽：启动投影恢复仍报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来。框架不自动删记录、不跳过 | 人工处置（只在确认是这类记录时）：①停进程，保留 WAL 目录与 Mongo 备份；②启动错误已给出事务 ID 与 `<库>/<集合>/<_id> expected=0 next=1`：在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算出玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份完整只读副本留证（它不推进 checkpoint、不跳过记录）；③线上尚未部署（维护者 2026-10-06）：直接停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本；上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码，框架不提供“跳过一条记录”的入口。见[维护手册 §4](README.md#4-故障处理) |
| Windows | 非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'`（编译全部测试不执行）和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异：打开中的文件不能改名/删除（nestwal 损坏记录检查、日志轮转、statslog），checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真（run 38500440362 的 6 个失败） | 本机测试结果不改变macOS/Linux支持范围；跨平台也成立的缺陷不归入此行 |

需要维护者做设计选择的事项（Lockstep 历史、SyncBus 投递、policy Drain、schema 迁移、Player 跨机）汇总在 [DESIGN-TODO](DESIGN-TODO.md)。[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据。[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本次核对是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零bug或100%行为覆盖。
