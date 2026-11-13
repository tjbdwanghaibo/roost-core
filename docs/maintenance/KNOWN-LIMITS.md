# 当前已知限制与验证边界

本文件分三部分（维护者 2026-11-13 要求把框架本身的限制与目前的遗留问题分开）：

- **[框架本身的限制](#一框架本身的限制)**：设计上的边界或取舍。要改需要新的框架能力，或者是维护者有意不做（每条尽量写明决定人与日期）。例如 Nest 不做延迟补偿、一个场景归一个进程、预加载只按主 DAO 枚举、sid 只在一个服务类型内唯一。
- **[遗留问题](#二遗留问题)**：实现、生成物、模块、demo 或验收没做到位的地方，可以按计划修。例如某个模块缺某个能力、生成器的缺省不对、没有自动清理、某项验收（Linux、跨机、真实 Mongo 崩溃、容量、长稳）没做，以及状态是“尚未确认”的怀疑。每条写现状，能看出下一步的写下一步。
- **[已修复的历史](#三已修复的历史)**：已经修掉、只留作证据或决策出处的条目，每条一行结论；现象、根因、红绿与复现原文在[已修复历史的证据](evidence/known-limits-history-20261113/README.md)、CHANGELOG 与对应 FEATURE / FIX 文档。

新增条目：先判断类别，再放进对应领域的小节（Nest / App 与实体定时器、DataEngine / Remote / Saga、Sync / 接入 / Lockstep、生成器配置与模块、框架服务与 game-demo、地形 / 无缝世界、玩法组件与物理房间、客户端、验收与运维）；不按日期新开“某日审查”小节。一件事既有设计边界又有没做完的部分时拆成两条、互相链接；拿不准类别的放进遗留问题并标“分类待确认”。拿不准是否仍有效、或证据不足的怀疑标“尚未确认”，不因整理而关闭。修掉之后把条目压成第三部分的一行，证据细节移到 `evidence/` 或对应 FEATURE 文档；被别处链接的旧锚点用 `<a id="旧锚点"></a>` 保留。

适用版本：最新发布版本（见 [CHANGELOG](../../CHANGELOG.md) 最上面的已归档段）与 main 上未发版的改动（CHANGELOG `[Unreleased]`）。审查发现的问题以 `RV-*` 编号写进对应领域（2026-10-22、10-28、10-30、10-31 四轮及 RV-20261101-01/02/03 已修复，对应发布状态以 CHANGELOG 为准；进度见 [COMMIT-REVIEW](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/COMMIT-REVIEW.md)）。历史压测留下、要等更合适环境的补测项汇总在[待补测](#待补测历史压测遗留)。

需要维护者做设计选择的事项与下一阶段优化项汇总在 [DESIGN-TODO](DESIGN-TODO.md)；[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据；[性能基线](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本文件是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零 bug 或 100% 行为覆盖；不确定的能力不写成已经支持。

## 一、框架本身的限制

### Nest、App 与实体定时器

#### 按实体的持久定时器（D11）

`roost add timer --entity <Entity>` 与 core 驱动 `framework/nest/entitytimer`（[DESIGN-TODO D11](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d11-按实体的持久定时器接入) 方案 B，`a2b4f4d8`、`5581deeb`）。离线语义：框架不追帧，离线期间到期的节点在加载后只触发一次，补算由业务按 `Fire` 参数自己做。用法见 [guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器)，契约见 [impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动)。

- **离线实体不准点**：驱动只索引已加载实例，未加载实体到期不唤醒，下次加载时补触发一次。维护者 2026-10-18 决定不做全局“离线也准点”驱动：必须准点的动作（如到点给离线玩家发邮件）由业务发 Nest 消息给目标实体或服务；不要求准点的在加载后的回调里自己追帧。
- **卸载与已入队 Tick 的窗口**：驱动投递前核对实例，但投递准入之后、执行之前实例被卸载时，Nest 按“发给未加载实体的消息”冷加载它并补触发一次（删档实体加载失败、不触发）。维护者 2026-10-18 决定接受，不给 Nest 加“只投给已加载实体”的发送选项。
- **构建了但没发布的实例**：实例在 `OnInitFinish`（发布之前）向驱动报告；发布失败且带期限时留下一个条目，到期时同 ID 已有别的实例则丢弃，没有实例则按加载中投递（Nest 冷加载并补触发）。条目只在到期前占内存，不重复投递。
- **Remote 托管与 noPersist 实体不支持**：add 时拒绝。迁移中的到期语义未定义，要支持需另行设计（D11 方案 C 方向）。
- **去掉 `timers` 条目后生成部分不自动删除**：需手工删 `*_component_gen.go`、节点类型、Tick handler 及 DAO / 实体里的两处字段；维护者 2026-10-18 决定维持现状。
- **装了定时器的服务多一个 Mod**：`wiring/entitytimer` Mod 让该服务的 `shutdown.total_timeout` 多 3s（每个 Mod 的保底）；维护者 2026-10-18 决定接受，不改 app 保底规则。game-demo 手写接线、不装这个 Mod。
- game-demo 手写驱动的启动顺序与端到端用例的范围见[按实体的持久定时器：遗留](#按实体的持久定时器遗留)。

#### 慢派发 trace 的参数样本

慢派发 trace（handler 执行超过 200ms，可能在 handler 还在跑时由计时 goroutine 打印）的 `param_samples` 只打印按值装箱的标量（布尔、数字、字符串）与实现了 `fmt.GoStringer` 的参数，其余参数只给类型名（`<*pkg.T>`）：不按反射读 handler 可能正在改写的对象（[DESIGN-TODO“D20 与 D21 审查修复”](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d20-与-d21-审查修复2026-10-21)）。要在 trace 里看到某个参数的内容，让它的类型实现 `GoString`，并保证只读构造后不变的字段（例如心跳的投递凭据 `heartbeat.beatTicket`、`*heartbeat.Reg`）；按值传的结构体（是副本、没有竞争）也只给类型名，这是为了不在 trace 里区分“副本里有没有引用”。

#### 实体心跳（D20 第 1 期）

`gameplay/heartbeat` + `wiring/heartbeat`（[DESIGN-TODO D20](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制)，维护者 2026-10-21 定方案）。用法见 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)，契约见 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)。

- **心跳缺省走 Nest 批量作业，普通业务消息 p99 变高**（维护者 2026-10-22 决定 B）：`heartbeat.delivery` 缺省 `batch`（驱动空值 `DefaultDelivery` 同此），64 拍一个 Nest 批量作业，每拍仍在实体自己的 Guard 作用域与事务里执行、Remote 托管实体仍单目标；`dispatch`（每拍一条单目标 `Dispatch`）保留为可选项。代价按 2026-10-22 补测（[NEST-BATCH-HEARTBEAT-20261021 §3～§3F](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3f-维护者-2026-10-22-决定-b-与参数测量)，5 万实体 100ms、100% 移动、5000 条/s 业务消息、回调是几十纳秒的内存计算）：同时段进程 CPU 比单目标少约 42%～73%，同一快池上普通业务消息 p99 约 0.08 → 0.19ms（组 4 / 200µs），p99.9 0.33 → 约 0.4ms；p99 由批量作业一次占住 worker 的时长（一组的目标数乘回调耗时，时间片在每组结束后检查）决定，回调越慢越高。缺省组 4 / 时间片 100µs：同时段补测（[§3G](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3g-2026-10-22-晚参数补测结果v1310-发版前压测第-1-项)）业务 p99 0.104ms（组 4 / 200µs 0.149ms，单目标 0.095ms），进程 CPU 1.52 核（单目标 3.00 核），心跳迟到 p99 8.23ms（200µs 7.14ms）。需要单目标方式的低尾延迟时配 `heartbeat.delivery: dispatch`；只实现 `nest.Client` 的客户端包装在缺省下 `Bind` 报错，要显式设 `DeliveryDispatch`。（参数的未测部分见[实体心跳：遗留](#实体心跳遗留)）
- **批量方式下的观测口径不同**：批内目标不逐条记 `nest.dispatch.total` / `cost`、慢派发 trace 与 `nest.handler.lock_hold` 直方图（只计超过慢阈值的），按批记 `nest.dispatch.try_batch.*`。业务时间与 Frozen 派生值仍按拍各冻结一次（与单目标方式相同）。
- **（批量方式）冲突转单目标时可能有一次 worker 等锁**：同 ID 冲突的拍转成单目标作业排在冲突消息之后，执行时锁一定空闲；但若实体锁被 Nest 作业顺序之外的持有者占着（别的 handler 的 `Cast` 等），转成的单目标作业与普通 `Dispatch` 一样在 worker 上等这把锁，与 X 相同、不比 X 多。（热点实体下的转换率见[实体心跳：遗留](#实体心跳遗留)）
- **卸载窗口**：单目标方式（`dispatch`）下，投递准入之后、执行之前被卸载的实体可能被 Nest 冷加载一次（与发给它的任何消息相同），这一拍在新实例上按过期丢弃；维护者决定 ⑨ 接受。批量方式只读已加载的实体，卸载后没有重载的按 `not_loaded` 丢弃、不冷加载；Remote 托管实体与冲突转成的单目标作业仍走普通 `Dispatch`，同上。
- **卡住兜底会多压消息**：在途 10s 未执行（同一登记连续卡住时 20s、40s、80s）就重投一次。拍没丢、只是 worker 排队超过阈值时，每个登记多出对数级份消息（同一代至多一份执行回调，其余按 `duplicate` 丢弃）。-race 下 5 万实体 1s 档冒烟（每秒 5 万拍超过 -race 吞吐）实测到这种放大，所以 -race 冒烟降到 2000 实体。
- **Remote 托管实体每拍一次 Remote 写准入**：消息经 I/O 阶段 `PrepareRemoteWriteBatch`，比普通实体贵；档位宜低。非拥有方（`Unknown` 状态、权威指向别人）的拍被准入拒绝后按卡住兜底重试（10s 起翻倍）。只有独占拥有（`LocalOwned`）才执行。共享模式下的所有权迁移已在真实 `remoteentity.Manager` 下验证没有双推进（进程内权威替身与真实 Redis + Mongo 两套）；2026-10-31 按现行 owner/shared 实现收敛：保留独占 owner 能力，不增加一律禁止挂载的开关；共享模式限制见下条。（独占模式的真实依赖故障注入见[实体心跳：遗留](#实体心跳遗留)）
- **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21）：共享是运行期状态、不是种类声明，登记时只拦得住“当时已在共享”的实例（`Register` / `Start` 返回 `ErrHeartbeatRemoteShared`）；登记后才进入共享的，由下一拍在实体锁内停掉，计 `heartbeat.stopped_total{reason="remote_shared"}` 并打限频 Warn。边界：① 这一拍要走一次共享模式写准入（分布式锁），回调不执行；② 共享窗口短于登记的周期、期间没有到期的拍时，这个登记不会被停掉（期间也没有执行）；全部登记停掉最多要一个最长档位的周期；③ 回到独占（`LeaveRemoteSharedMode`）后不自动恢复，业务需要时自己 `Start`。共享实体的周期逻辑由业务层驱动（例如负责它的服务按周期发 Nest 消息）。
- **快池准入满时的日志**：心跳本身按跳拍处理并限频 Warn，但 Nest 对每条被拒的异步投递另打一条 `async dispatch failed`（不限频，Nest 现有行为，本期不改 Nest）。
- **`Strict` 持久级别的心跳每拍都在实体 worker 上等落盘**：`WithDurability(nest.DurabilityStrict)` 的登记，每一拍的提交都按 strict 走，回调改了持久字段的拍要在实体 worker 上、持实体锁等提交持久化完成才返回（与任何 strict handler 相同），快池实体的高频心跳会把 worker 时间花在等 I/O 上；缺省 `Async`。用法见 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)。
- **组件漏调 `Host.OnDestroy` 时由驱动补上，但要等登记到期**：驱动在登记到期、发现实例已移除（`IsRemoved`）时撤销整个宿主，所以环与驱动对已移除实例的引用最多再保留一个最长档位的周期（60s 档最多 60s）；`dropped{reason="removed"}` 每个登记至多计一次。还没发布或被同 ID 另一实例顶替的不撤销（可能是暂时的），只丢这一拍。
- **`Reg.Close` 不随事务回滚**：与 `Register` 一样是生命周期操作，立即生效；回滚的 handler 里 Close 掉的登记不会回来，同一 handler 里同名新登记的 `Start` 随回滚撤销、登记本身保留（名字被占着，要再 Close 或复用）。换场景重建的调用放在 handler 最后一步（与 runner 的约束相同）。
- **装了心跳的服务多一个 Mod**：与 entitytimer 相同，`shutdown.total_timeout` 多 3s 保底。

#### 移动与动作（D20 第 2 期）

`gameplay/movement`（`MapBasic`、`Move`、`MoveTo` / `Follow`）与 `actionflow.BeatRunner`（[DESIGN-TODO D20 P2](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#p2-实施结果2026-10-21)）。用法见 [guide/10-time §4B](../framework/guide/10-time.md#4b-移动与动作心跳驱动)，契约见 [impl/10-time §2C](../framework/impl/10-time.md#2c-移动与动作的心跳驱动)。

- **框架没有巡逻、寻路组件**：寻路在发起 `MoveTo` 之前由业务做，路径作参数；巡逻是 AI 策略（game-demo 有参考接法）。（每拍分配未量，见[移动与动作、行为与技能：遗留](#移动与动作行为与技能遗留)）
- **runner 不随事务回滚：框架不处理，由业务保证**（维护者 2026-10-21）。`ActionRunner` / `MissionRunner` 是纯内存状态；handler 先做完全部可能失败的校验与修改，最后一步才调用 `Start` / `Enqueue` / `End` 等。违反时，回滚后 runner 里残留这次的变更：例如回滚的 handler 里启动的 MoveTo 仍是移动组的当前动作，而 `Move` 的设定与组的心跳都随回滚恢复——这个动作不被 tick，直到被取代、被结束，或业务调用 `BeatRunner.Sync` 后在下一拍以取消（`Replaced`）结束。心跳那一拍只在提交失败时回滚（拍回调不返回错误），此时动作对组件的修改回滚、runner 按 Tick 的结论的推进保留，同样不处理。
- **进出场景在提交后、实体锁外调用 `Space`**：`InterestSpace` 的 `Enter` / `Leave` 只在 Interest 登记侧登记一条命令（2026-10-21 单写者重构起不再取 Interest 的应用侧锁、不再 `Apply`，不等 AOI 批次），由政策阶段 / 应用线程按调用顺序执行；失败只经 `MapConfig.OnSpaceError` 报告（缺省 Warn），场景字段已提交。pipelined handler 的提交后回调在完成泵上执行，同实体的下一条消息可能早于 `Space.Enter` 登记，这期间 `QueueMove` 返回 `ErrInterestUnknown`（移动以 `Refused` 结束、`SetPos` 的调用方收到错误）。（事务内门控的进出场景见[移动与动作、行为与技能：遗留](#移动与动作行为与技能遗留)）
- **`Published` 晚于同一拍里的事件**：快照在提交后、实体锁释放之后才发布；在拍的事务里观察到的事件（动作结束、OnEnded）早于它。要读到这一拍提交后的位置，等该实体的下一条消息（同实体消息要等前驱作业连同提交后回调整个结束）。
- **Remote 共享模式停心跳后组件不感知**：实体运行中进入共享模式时框架停掉动作组、任务、AI 的心跳与 `"map.save"`，回到独占不自动恢复；动作与任务留在 runner 里不推进、AI 不决策、周期落库停。业务回到独占后恢复：动作组与任务心跳 `Behavior.Beat().Sync()`，AI `SetStrategy(c.Strategy())`；周期落库没有单独入口（`Enter` 在场景里返回 `ErrAlreadyInScene`，只能 `Leave` 再 `Enter`）；不新增 `MapBasic.Sync`，装配时必须考虑共享往返后这个恢复边界。共享期间启动需要周期 tick 的动作会以失败结束。
- **不持久“在场景里”与“正在移动”**：实例重新加载后由业务按自己的 DAO 状态重新 `Enter`、重新发起移动动作；只从内存卸载不调用 `Leave`（`MapBasic.OnDestroy` 只发布“已不在”，跟随者以 `TargetLost` 收尾）。
- **跟随只看本进程、只看提交后的位置**：`LocateIn` 经实体管理器的只读查找，目标不在本进程内存（另一个进程的 Remote 实体、已卸载）即 `TargetLost`；读的是目标最近一次提交的快照，最多落后目标一拍。
- **一个实体多个组各自一个心跳登记**：参考 ssr，动作组之间不合并投递（决定 ③）；移动之外再有一个需要周期 tick 的组（例如技能），每拍就是两条消息。
- **缺省 5Hz 的位置粒度**（2026-10-22，[O1](../release/v1.31.0-NOTES.md#o1)）：`DefaultMovePeriod` 200ms，同样的速度下每拍位移是 10Hz 时的两倍；观察者看到的位置每 200ms 更新一次（客户端插值），跟随读到的目标位置最多落后一拍（200ms）。近战贴身、动作游戏在动作参数里显式给 100ms。
- **地形判定按 0.5 格采样**：每进一个新格问一次 `Terrain`，斜穿两个被挡格的夹缝可能漏判；一拍的位移不设上限（速度 × `Elapsed`，`Elapsed` 上限 `max(1s, 档位)`），速度极大时采样数随距离增长。
- **`MemFields` 只给测试、示例与不持久的 NPC**：逆操作只在 `rollback=undo` 的 handler 里登记，`rollback=state` 不恢复；它不标同步脏（`OnChange` 由使用方接）。需要同步的实体用 DAO 字段实现 `Fields`。

#### 行为控制（D21）

`actionflow.Behavior`、`Replacer`、跨组 `Conflict`、`EndAction`、计数冻结与 Start 即完成（[D21 实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/3fddb51ea19dcd66451fd0ce8c80dcdf6b04be45/docs/maintenance/DESIGN-TODO.md#6-调研一--实施)）。用法见 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结)，契约见 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态)。

- **业务接入**：game-demo（D20 P3）：玩家的 `Behavior`、怪物的 `Behavior` + AI 心跳 + `state.Gate` + `BindStates`；生成器的 `roost add behavior [--ai] [--state]` 生成 `Behavior` + AI 心跳控制器 + 示例动作 / 策略 + `state` 的 Catalog 声明（边界见下文“生成器接入心跳、行为与时间域”）。仓库里实现 `Action` 的只有 `movement` 的两个动作、测试动作与生成的示例动作。
- **Behavior 与 runner 一样不随事务回滚**（D20 决定 3）：冻结计数、取代与抢占的结果、任务的推进都是内存状态，handler 回滚后保留，由业务把这些调用放在 handler 最后一步。
- **任务回调期间结束的动作，观察者可能先于任务收到**：动作结束经 `MissionRunner.OnActionEnd` 送任务，任务正在执行自己的回调时（例如任务 `Start` 里发起的动作启动即失败）这次送达延后到回调返回后，而观察者（AI）在当次就收到。“先任务后观察者”只在任务不处于自己的回调时成立。
- **动作的归属任务按“任务 runner 是否正在执行”判定**：任务回调同步引出的其他回调（例如任务回调里启动的动作启动即失败、`OnEnded` 送到观察者、观察者又发起动作）发起的动作也会记在当前任务名下，随任务结束被取消。
- **步骤时限的精度是任务心跳的周期**：到时最多晚一个 `MissionPlan.Period`（缺省 100ms，取档）才结束动作；动作所在组被冻结的时间不计入时限（2026-10-22，[O2](../release/v1.31.0-NOTES.md#o2)），冻结起止按 Behavior 的时钟在 `OnFreeze` 里记、不受任务心跳周期影响，自己实现 `ActionList` 而不实现 `actionflow.FrozenTimer` 的照常计时；超时的结束发生在任务自己的 Tick 回调里，所以观察者（AI）先收到这次动作结束、后收到可能随之而来的任务结束（上一条的例外）。没有配 `Beat` 的 Behavior 不检查时限（任务不被 tick）。
- **AI 心跳的错误只报告**：`"ai"` 登记挂不上（Remote 共享模式）时经 `OnError` 报告，AI 不再周期决策，回到独占后要业务 `SetStrategy(c.Strategy())`（同一策略不是替换、只重新对齐心跳）或 `Recover` 触发对齐；`SetStrategy` 不返回挂不上的错误（经 `OnError`）；动作 / 任务结束照常送达。
- **任务结束可能先于引起它的动作结束送达观察者**：动作结束先交给任务，任务因此结束（例如 `PlanMission` 按 `OnFail` 结束）时，观察者在这次处理里先收到任务结束、再收到这个动作结束；由 `movement/shared_test.go` 场景一固定。
- **AI 事件是“下一拍”的、会丢**：事件缓冲到策略的下一拍，冻结期间、切换中、没有策略时直接丢弃；两拍之间超过 `MaxEvents`（缺省 64）丢最早的，不报告。没被取走的事件在那一拍结束时丢弃。要可靠送达的业务事实放进 DAO / state，事件只用来唤起决策。
- **决策轨迹不留存**：`Trace` 只是钩子，框架不保存轨迹；打开 `TraceNodes` 后每个节点每拍多一次接口调用，适合按实体采样，不要全服常开（没有测量）。
- **动作指标缺省关闭**：`BehaviorConfig.Metrics` 打开后每个动作开始、结束各写一次注册表（拼标签有分配），全服量级的开销没有测量。
- **`FreezeCancel` 只打断当前动作**：排队项保留，解冻后接着推进；要连排队一起丢弃，业务另调 `ClearQueue` / `EndAction`。
- **被抢占组的排队项在抢占方结束时推进，照常过跨组冲突**：抢占时不推进，抢占方（那一个动作）结束时推进一次。抢占方被同组的另一个动作取代时，推进排在新动作启动之后，按跨组规则判定：新动作同样挡住它（`BlockIncoming`）时排队项以失败结束、接着试下一个，整队可能这样各收到一次失败；要“等这一类动作全部结束再走”的，由业务在抢占时自己 `ClearQueue` 或在结束回调里重新排队。
- **`Behavior.OnDestroy` 不在事务里**：卸载时结束在途动作与任务（`ErrOwnerDestroyed`，取消状态），动作的 `Cancel` 与观察者这时改的持久字段不落库；组件要按装配顺序调（AI 的 `Controller.Close` 在前，见 guide §4C）。漏调时在途动作没有结论、`actionflow.actions.running` 不减。直接经 `Runner()` 发起的动作不受 Close 后的拒绝约束，只是挂不上心跳（以 `ErrBeatRunnerClosed` 失败结束）。
- **冻结计数不随事务回滚**：直接调用的 `Freeze` 与 `Recover` 必须成对，handler 回滚后多出的一次 `Freeze` 要由业务撤销。经 `state.Gate` 的冻结每条规则至多持有一次，成功回滚后立即按状态对齐（见下条）。
- **状态观察者在成功回滚后重建内存状态**（`gameplay/state`）：Nest 全部逆操作成功后、实体锁释放前按差发送 `Change.Rollback=true`。该通知不能写 DAO、发奖励或网络消息。门控冻结立即恢复，但已打断的动作不复活，其他外部副作用不会自动补偿；逆操作失败时不发成功恢复通知。
- **Buff 到期才撤销派生状态**：`BindStates` 的状态随 buff 驱动（D20 P5；心跳驱动 `AttachBeat` 或别的驱动经 `DriveBuffs`）移除到期 buff 而失效，心跳驱动下最多晚一个心跳周期（`BeatConfig.Granularity` 取档，缺省 1 秒）加 tick 取整；没装驱动的实体只在业务自己调 `TickBuffs` 时失效。
- **派生状态的变化要数据拥有者调 `Changed`**：派生函数查询时现算，`Has` 总是正确，但观察者与同步脏位只在 `Changed`（或任何其他改动 / `Refresh`）时才更新；漏调时门控与客户端看到的是旧值。
- **持久状态改名等于丢弃旧数据**：持久文档的键是状态名，`RestorePersisted` 跳过不认识的名字（不报警），下一次整文档写回时消失。（`state` 还没接同步打包，见[移动与动作、行为与技能：遗留](#移动与动作行为与技能遗留)）

#### 技能与 Buff 的推进与驱动（D20 第 5 期）

用法见 [guide/10-time §4E](../framework/guide/10-time.md#4e-技能与-buff推进与驱动d20-第-5-期)，契约见 [impl/10-time §2G](../framework/impl/10-time.md#2g-技能与-buff-的推进与驱动)。

- **逐 tick 确定性只在追帧上限内成立**：落后超过 `MaxCatchUp`（`skill.Stepper` 缺省 1s，心跳驱动缺省 `max(1s, 周期)`，乘 max(倍率, 1)）时剩余部分一次 `Advance`，Host 的中间 tick 合并；`Stepper.Stats().Jumps` 计数并打限频 Warn，没有指标。回放要在同样的 tick 跳。
- **输入落在哪个 tick 取决于输入的时刻**（心跳驱动下是 handler 开始执行的时刻）：同一输入序列在不同驱动节奏、不同驱动方式下逐 tick 相同，前提是“每个输入落在同一 tick”；输入本身的 tick 随网络与排队变化，回放要记录输入的 tick。
- **Runtime 不随事务回滚、心跳启动随回滚作废**：handler 的 `Start` / `Input` 之后回滚，施法留在 Runtime 里、心跳没挂上，要等下一次输入或业务调 `Sync`（与 actionflow runner 同一规则，业务把输入放在最后一步）。拍里 `Advance` 的错误只经 `OnError` 报告，下一拍从 Runtime 实际到达的 tick 继续。
- **忙时每个 tick 推进一次 Host**：长时排程任务（例如 30 秒后的弹药回充）让 Runtime 保持“忙”，期间逐 tick 推进（50ms tick 下每秒 20 次空推进）；没有“跳到下一个到期点”的稀疏推进，那样 Host 的 tick 序列会随拍变化。
- **buff 到期精度是一个心跳周期**：缺省 1Hz，短控制（0.3 秒眩晕）最多多持续约 1 秒，要更准调小 `Granularity`；需要离线准点、结果要落档的长时 buff 用 entitytimer。
- **周期伤害、冷却、回复不在组件里**：`combatcomponent` 只持有 buff；业务的周期工作经 `Periodic` / `OnStep`（心跳驱动是 `BeatConfig.Periodic` / `OnBeat`）挂在同一个 buff 驱动上，答案变了要调 `SyncBuffDriver`。
- **Runtime 与持有它的实体**：`RuntimeBeat` 只在 Runtime 所属实体的锁内推进（`skill.Stepper` 只要求串行调用，不加锁）；Host 对其他实体（战斗单位）的效果按 Host 自己的规则进它们的事务（`StatusBridge.Committer` 等），驱动不替它加锁。game-demo 只有怪物接了 `AttachBeat`，没有技能 `RuntimeBeat`；生成器不生成战斗组件骨架。
- **不随事务的驱动在回滚后多醒一步**：`skill.Driver` 不是事务的一部分；心跳驱动的启停随事务，定步长循环这类驱动在施加限时 buff 的事务回滚后仍醒着，下一步没有工作才睡（`TestARolledBackBuffCostsANonTransactionalDriverOneStep`）。驱动要立即一致的话自己按事务提交后生效。

#### 业务时间取自 ctx 与场景时间域（D22）

业务时间一律取自 `fctx.Now()`（D22-1）与 `gameplay/timedomain`（场景时间域）。用法见 [guide/10-time §1A](../framework/guide/10-time.md#1a-业务时间一律取自-ctxd22-1)、[§4D](../framework/guide/10-time.md#4d-场景时间域变速)，契约见 [impl/10-time §2E](../framework/impl/10-time.md#2e-业务时间取自-ctxd22-1)、[§2F](../framework/impl/10-time.md#2f-场景时间域)。

- **“一律从 ctx 取”只有提示，没有强制**：glsvet 只看业务目录里 Nest handler 函数体内的 `app.BusinessClock` / `clock.Now` / `clock.UnixMilli`，不跟进 helper、包装函数或改名的变量；组件方法、业务库里现读业务钟不会被提示。约束靠 guide §1A 与生成文档。
- **冻结点在消息开始执行时，不在拿到实体锁时**：实体锁等待的时间算在冻结之后，热点实体上排队久的消息拿到的“现在”比实际执行早；同一条消息内一致，跨消息仍单调（按开始时刻）。
- **进程业务时钟与 Registry 业务时钟同源但不是同一个对象**：心跳与 entitytimer 驱动、`fctx.Now` 读进程业务时钟（App 启动时 `clock.SetOffset`），服务读 `app.BusinessClock(r)`。同一进程里起多个 App 且 `time.logic_offset` 不同时，进程级偏移是最后启动的那个（与 D22 之前 `fctx.Now` 的行为相同）。
- **时间域：成员看到改倍率比场景晚一个发布**：场景事务提交后（`AfterCommit`）才发布；之间开始的成员消息按旧倍率。域时间对读者单调（取已交出的最大值），代价是改倍率前后成员与客户端的域时间差“发布延迟 × 倍率”（微秒到毫秒级），时停期间一直差这一点。
- **时间域实例随场景实例**：场景卸载重载后是新的 `Domain`，已经挂在旧 `Domain` 上的成员心跳不会自动换；场景（副本）在有成员时不应卸载，或由业务在重载后让成员重新登记。`SetRate` 是否在所属实体锁内调用不校验（契约写在 API 注释）。
- **域要在构造时给，换场景靠重建**：`actionflow.BeatConfig.TimeDomain`、`ai.BeatConfig.TimeDomain` 在构造 `BeatRunner` / `Behavior` / `Controller` 时定下，运行中不能换域。成员换场景时先 `Controller.Close`、`Behavior.Close`（在途动作与任务以给定原因结束、心跳登记撤销、名字释放），再在同一心跳宿主上按新域重建（[guide/10-time §4D](../framework/guide/10-time.md#4d-场景时间域变速)）；不提供 `SetTimeDomain`：旧域里记下的时刻（上一拍的 `Elapsed` 基线、计划步骤时限的起点、`FreezeFor` 的期限）不能拿到新域里比较，而换场景本来就要结束场景里的动作。显式配了 `BehaviorConfig.Now` / `ControllerHooks.Now` 的以业务的为准，不取域时间。
- **技能 / Buff 的时间域在构造时给**：心跳驱动的 `combatcomponent.BeatConfig.TimeDomain`、`RuntimeBeatConfig.TimeDomain` 在 `AttachBeat` / `NewRuntimeBeat` 时定下，运行中不能换域，换场景靠 `CloseBuffDriver` / `RuntimeBeat.Close` 后按新域重建（不走心跳的驱动经 `StepperConfig.Now` / `Rate` 自己传域时间）。buff 的 `DueTick` 是按域时间算的绝对 tick：实体换到另一个域（或离开变速场景）时，已有 buff 的剩余时长按新域的 tick 算，框架不换算。
- **客户端外推只有 C# 的计算类**：game-demo 的场景已挂时间域（D20 P3，单场景、`nopersist,sync`、经场景视野下发给每个成员）；生成器的 `roost add timedomain` 只生成场景侧的字段与组件，成员怎样拿到所在场景的域由业务写；C# 客户端有 `Roost.Client.SceneTimeDomain`（同一公式、单调托底）；SDK 不解码业务载荷，三个字段由游戏自己的解码器读出交给它；“估计的服务端业务时间”由游戏提供（SDK 没有对时）。（Unity 适配层见[客户端：遗留](#客户端遗留)）

#### Skill Runtime

不参与 Nest 回滚；Host 副作用和确定性由业务契约保证。（待补的测试见[移动与动作、行为与技能：遗留](#移动与动作行为与技能遗留)）

#### App 停机关输入后的边界

- **App 停机关输入后的边界**（2026-11-04，[方案](FEATURE-2026-11-04-app-lifecycle.md) 第 6 节，契约见 [guide/01 §4.1](../framework/guide/01-app-lifecycle.md#41-停机第一步关外部输入)）：① **集群里退订兴趣的传播窗口**：退掉一个订阅之后，集群其他节点要过一小段时间才撤掉转发兴趣，这段时间经别的节点发来的请求到达本节点时本地订阅已经没了、被服务端丢掉，调用方等到超时（结果未知）。真实依赖 `TestRealWithdrawRpcAnswersEveryRequestDefinitely` 设 `ROOST_WITHDRAW_SPLIT_NODES=1`（服务端与调用方连不同节点）10 次里 5 次丢 1～4 个请求，同节点 10 次 0 次；game-demo 两个 alliance 实例运行中停，`4947c62a` 时 22 次里 1 次、关输入之后 15 次里 2 次出现超时（方案 §8，样本区分不了两者）。任何订阅退掉都有这个窗口（Bus 停止、进程退出同样），关输入没有新增它，只是把它挪到停机第一步；曾试过让实例主题留到 Bus 停止、期间答可重试，窗口移到 Bus 停止时的直接退订与连接关闭，冒烟里超时反而更多，已撤回。调用方照 `Classify` 把超时当结果未知、用同一 RequestID 查结果或重发。② **启动步骤进行中发生的 fail-stop**（失锁等 `RuntimeFailure`）要等这一步返回才被看到：`startupFailure` 只在步骤之间查。③ **按依赖序停的输入不在第一步**：玩家 TCP / 网关连接（方案 D3）由各自的 Mod 按依赖逆序停，它们排在依赖之上，在途答完才停依赖；`Service.Shutdown` 期间玩家仍连着。④ **不自动登记的输入**：业务在 `Service.Init` 里直接用 `IJetStream.Subscribe` 建的消费者（不经 `SubscribeEffects` / `Steps`，如 game-demo 的活动阶段消费者）、自己开的 HTTP / TCP 监听，不受这条保证，要实现 `app.IngressCloser` 或调 `app.AddIngress`，否则由业务的 `Shutdown` 负责。⑤ **排空和后续停机共用 `shutdown.total_timeout`**（方案 D1）：一条执行很久的运维命令或慢 handler 会吃掉后面 Mod 的停机时间；第一步没排空时 App 不再停 Mod、保留锁，进程按停机未完成退出。⑥ **`startup.timeout` 到期只关输入、不等那一步**（方案 D2）：卡住的步骤仍在跑，依赖与锁照旧保留到进程退出。⑦ 模块消息走核心 NATS、不持久：第一步之后到达的消息丢弃或进死信，与进程退出后到达的消息相同，靠业务原有的重投 / 登录补齐。

### DataEngine、Remote 与 Saga

#### 起服预加载与按服分库（v1.47.0）

设计见 [guide/02“起服预加载与按服分库”](../framework/guide/02-nest-entity.md#起服预加载与按服分库v1470-起)、实现见 [impl/12 §2H1](../framework/impl/12-codegen.md#2h1-本地实体起服预加载与-dao-缺省-dbscopesidv1470)。没做到位的部分见[起服预加载与按服分库：遗留](#起服预加载与按服分库遗留)。

- **预加载不等于常驻，常驻要显式声明**：只写 `preload=startup` 的实体被运维卸载、换服交出、业务空闲卸载之后按需加载，不会再预加载。要一直留在内存用 `lifetime=resident`（v1.47.0 起，隐含 `preload=startup`）：除换服交出、删档收尾与运维强制刷新三处框架许可外，所有移出内存的路径都返回 `entity.ErrEntityResident`。
- **常驻实体的内存没有上限**：占用随本服库里该类实体的文档数增长，框架不估算、不截断。
- **常驻实体加载失败不自动重试**：起服预加载时单个常驻实体失败、换服中止（Release）后载回失败，它就不在内存，直到被访问或运维 `entity.unload {..., force:true}` 载回；两处都记 ERROR 与 `dataengine.resident_reload_total{result=error}`。
- **换服后到首次进入之间不在目标服内存**：写权随交出到了目标服，第一次进入时载入后常驻；目标服重启后由它的预加载载入。
- **常驻许可靠约定**：`entity.WithResidentRelease` 是导出的，只给框架那三处用；业务代码不能用它绕过常驻。常驻实体不要写空闲卸载（会拿到 `ErrEntityResident`）。
- **只按主 DAO 枚举**：kind 的第一个 DAO（`DaoBuilders[0]`）所在集合里有文档的实体才会被列出（新建实体时框架给每个持久 DAO 都写整文档，正常不会只缺主文档）。
- **sid 只在一个服务类型内唯一**：不同服务类型的 sid 可能相同，同一个逻辑库在它们那里会落到同一个 `<库>_<sid>`。所以没写 `service=` 的本地预加载要求只有一个候选服务；两个服务类型共用同一个逻辑库与集合的部署不受支持。
- **一个实体的文档在同一个库范围里**：组件 DAO 跟随宿主实体的 dbscope（维护者 2026-11-13）。本地实体的组件 DAO 与主 DAO 范围不同时，生成工程的 `registry.RegisterAll`（`entity.ValidateEntityRegistry`）在起服时拒绝（`entity.ErrMixedDaoScope`）；托管实体照旧全部 global。
- **存储位置变了、不兼容旧数据**：Player / World 与模块本地实体的文档从共用库搬到 `<库>_<sid>`；线上尚未部署，按规则不做迁移，升级前清空数据（CHANGELOG）。

#### 删档后同 ID 重建（D10）

GetOrCreate 路径已守住（维护者 2026-10-17 决定 [D10](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，`572d87c2`）：冷加载对删过档返回 `entity.ErrAuthorityEntityDeleted`（不满足 not found），生成 GetOrCreate 不重建，发往该 ID 的 Nest 请求回复 `entity_deleted = 3`。

- **约束（直接 Create）**：handler 内或 lifecycle `Create` 不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID，也不要对存了但没加载的 ID 直接 Create；`EntityManager` 的 ID 生成器同样必须永不重复。违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来。直接 Create 的保护按维护者决定不做。
- **删档审计**：“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记。
- 回归：`TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`。

**删档后同 ID 重建：违反约束或修复前写下的冲突记录**。这类记录不会被跳过或放宽：启动投影恢复报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来；框架不自动删记录、不跳过，也不提供“跳过一条记录”的入口。人工处置（只在确认是这类记录时）：

1. 停进程，保留 WAL 目录与 Mongo 备份。
2. 按错误给出的事务 ID 与 `<库>/<集合>/<_id>`，在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份只读副本留证（不推进 checkpoint、不跳过记录）。
3. 线上尚未部署（维护者 2026-10-06）：停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本。上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码。见[维护手册 §4](README.md#4-故障处理)。

#### 新建实体落库与缺失 DAO 补空（R15，维护者 2026-10-30）

新建的持久实体由创建它的持久事务写齐每个 DAO（没改字段也写）；冷加载时部分 DAO 缺文档（没有墓碑）按初值补上、由这个实体的下一笔持久提交写出（规则与取舍见 [impl 03 §2 第 12 条](../framework/impl/03-dataengine.md#2-必须保持的契约)）。剩余限制：

- **补空会掩盖真实丢写 / 误删**：原先“只缺一部分就拒绝”是丢写、误删、投影缺口的唯一信号，现在改由 WARN 与 `dataengine.aggregate.dao_filled.total{kind,collection}` 承担，框架不区分“新增的 DAO”与“丢掉的文档”。新增 DAO 之外这条计数非零要人核对；没有按 DAO 声明“可后加”的白名单（维护者决定全局补空）。补上的 DAO 没有下一笔持久提交时一直没有文档，每次加载都会再补、再计数。
- **下游迁移**：从 cube 迁来的实体若只有部分 DAO 文档，框架会自动补空，迁移工具不再需要补齐缺失的 DAO 文档；但迁移出的文档仍要满足当前 schema（`_schema`）与持久键约定（D13），补空不处理 schema 不符。
- **handler 之外的 `RunDetachedTransaction` 里新建**：外层没有能把新实体锁持到提交的 Guard 作用域，新建由 DataEngine 的新建准入门单独起一笔 strict 事务提交；外层随后回滚不撤销已落库的新实体（handler 内的 detached / isolated 事务不受此限，回滚会撤销）。
- **Nest 之外新建要等 WAL**：生成 Lifecycle 的 `Create` / `GetOrCreate` 在 Nest 之外为持久实体补一笔 strict 事务，提交被接受后才返回；业务 worker 上调用返回 `fctx.ErrBlockingInBusinessWorker`，I/O worker 上开 Guard 作用域照旧 panic。每次新建是一条独立 WAL 记录，之后的修改是第二笔。没有 DataEngine 的进程（没注册准入门）事务外新建只在内存。
- **Remote 托管 kind 在 Nest 之外新建不经准入门**：Remote 持久写只能经消息声明的 Remote 批次，准入门的本地 strict 事务写不了；这类实体照旧在第一笔 Remote 批次提交时写齐每个 DAO，新建后没有 Remote 写就不落库。handler 内新建（带 Remote 批次的消息）验证到 nest 层认领（每个 DAO 整文档认领）与 Remote 事务记录掩码按位存取（`scripts/test-remote-generated.sh` 全部通过）。给 Remote 托管 kind 的事务外新建补一笔 Remote 写（需要所有权认领）按需求触发再做（维护者 2026-11-03），见 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。（Remote 端到端用例见[Remote：遗留](#remote遗留)）
- **失败的新建**（RV-20261031-01 已修复，见 [impl 03 §2 第 12 条](../framework/impl/03-dataengine.md#2-必须保持的契约)）：交给事务捕获之前失败的新建，构建期间以它自己 DAO 登记的修改从事务撤销；构建还写了别的持久内容（别的实体、`Emit`、`AddReceipt`、删除意图、构建中新建的其他实体）时整条回滚、业务吞不掉（`nest.ErrFailedCreateLeftEffects`）。边界：只覆盖 `CreateInScope` 在 Nest 事务捕获者下的新建（handler 内、handler 里的 isolated / detached 事务、准入门）；handler 内经 `Create(IsCreate=false)` 走自建短作用域的冷加载 builder 若登记持久修改后失败，不在撤销范围内（冷加载 builder 按设计不写库）；失败 builder 对其他实体非持久字段的内存修改只随事务回滚撤销，业务吞掉错误并提交时保留，与 handler 里任何部分完成的函数相同。
- **手写 DAO**：tracker 不实现 `entity.AggregateMember` 的 DAO（自己用 `AddMutation` 落库）不参与新建写齐与补空后的写出，由写它的代码保证文档齐全。

#### 运行期改档与单实体运维（D16）

框架只提供与业务字段无关的 `entity.kick`（踢下线）与 `entity.unload`（排空投影后卸载），**不提供改档与恢复**（维护者 2026-10-20 决定 C，[DESIGN-TODO D16](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d16-单个实体--玩家的运维入口)，用法见 [guide/03 §6](../framework/guide/03-dataengine.md#单实体运维命令d16)）。`entity.unload` 之后任何访问都会立刻重新加载，不能当作改库的窗口。运行期直接改权威集合的结果按设计是确定的：只改字段时下一次投影覆盖改动；改 `_version`（恢复旧备份）时投影 fatal、进程 fence、重启起不来，直到人工处理（[维护手册 §4](README.md#4-故障处理)）。在线改档写成业务 GM handler，整档恢复走停服排空后的离线写回（[guide/03 §3](../framework/guide/03-dataengine.md#3-投影与幂等)）。

#### 交出实体（HandOffEntity）的边界

`Runtime.HandOffEntity`（[guide/03 “交出实体”](../framework/guide/03-dataengine.md#交出实体handoffentity)）只保证本进程一侧：已准入的写入落库且不会再被重放、此后不写。以下不在它的保证里：

- **屏障不持久**。进程重启后屏障不存在，重启后还能不能写这个实体由调用方的权威在准入时回答（换服是 account 的绑定）；没过业务准入的写入口在重启后的旧服上仍能加载并写一个已经换走的实体（[换服方案 §8](FEATURE-2026-11-11-player-rehome.md#8-已知风险与没查清的)的“加载守卫”，二期评估）。
- **交出不拦加载**。交出期间实体可以被读回内存（仍有 Sync 订阅者时的卸载后重载、任何一次访问）；这样的实例只读，下一个写者写过之后落后于库，仍订阅着它的客户端看到的是旧内容。调用方要先断开会话、离开场景、退订再交出；`Release` 会把期间加载的实例卸载。要让已交出的实体在本进程根本加载不了，需要在冷加载入口加守卫（改冷加载路径的行为），没有做。
- **`RollbackNone` 的 handler**。被屏障拒绝的事务按回滚策略撤销内存修改；`RollbackNone` 的 handler 在期间加载回来的实例上留下的修改不会落库，只留在那个只读实例里，`Release` 或再次交出时随实例丢弃。
- **Remote 托管实体不支持**（`ErrEntityHandOffUnsupported`）。
- **等 checkpoint 用轮询**（2 ms），没有在 checkpoint 路径上加通知；交出因此最多多等一个间隔。集合取的是全部“已投影、未确认”的记录，不按实体区分：别的实体的投影或 checkpoint 卡住（例如并行窗口里另一条通道持续失败）时，交出会等到期限并以 `ErrEntityUnloadTimeout` 失败，即使本实体的记录其实已被确认。
- 验证范围见[Remote：遗留](#remote遗留)。

#### DAO 持久键（D13）

定义里字段必填的 `store:"x"` 就是持久键（v1.40.0 起；没有自动编号或 snake_case 回退，规则见 [tag 手册](../../codegen/docs/TAGS.zh-CN.md#必填的-store-键与变更告警v1400-起)），库里有、DAO 未声明的键不读入，下一次整文档写回即删除（维护者 2026-10-19，[DESIGN-TODO D13](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）。

- **业务约束**（[SCHEMA-MIGRATION §1](../framework/SCHEMA-MIGRATION.md)）：改名 / 删字段前要么保留原 `store` 键，要么先离线迁移再提升 schema，否则旧数据不再加载、写回即丢失。
- **剩余风险**：`roost generate` 与 `RestorePersisted` 的 WARN 不阻断，忽略告警时旧值丢失由业务约束承担；单字段补丁不碰旧键，它们留在库里但不再被读取，清理要一次性离线迁移；生成期基线只在工作树里，删掉生成物或换机器重新生成看不到上一次的形状，只剩加载期 WARN。

#### Remote

- **多写者本地副本**（2026-11-04 修复后的边界，契约见 [REMOTE_ENTITY“多写者：本地副本与权威”](../framework/REMOTE_ENTITY.md#多写者本地副本与权威)）：写准入发现本地实例落后于权威或停在 `Fenced` 时卸载重载，需要 loader 实现 `IRemoteEntityUnloader`（正式 `ManagerAccess` 实现了）；自定义 loader 不支持卸载时照旧报版本冲突 / fenced，直到业务自己重载。事务外独立删档持有实体锁，遇到落后实例按版本冲突拒绝，不在删档中途换实例；先有一次 Nest 写准入（或 `ManagerAccess.Unload`）换掉旧实例再删。回归用两进程共用的 Mongo 权威替身与 Redis 版本锁替身（`write_admission_test.go`），以及 `scripts/test-remote-generated.sh` / `test-remote-matrix.sh` 的现有真实依赖用例。（两个真实进程交替写的用例见[Remote：遗留](#remote遗留)）
- **路由 RPC 与 Redis 路由登记的边界**（联盟服务方案 1R，2026-11-04，契约见 [REMOTE_ENTITY“路由登记”](../framework/REMOTE_ENTITY.md#路由登记)、[guide/09“路由 RPC 与改投”](../framework/guide/09-services.md#2a-路由-rpc-与改投)）：① 登记表只增不减：Remote 实体没有空闲卸载，续期量是“已加载的登记 kind 实体数 / 10s”，`AllianceMember` 这类按玩家的实体长时间运行后会一直增长，阶段 5 实测：续期量就是已登记实体数——预加载 1000 / 10000 个联盟后 `route_renew_total{result=renewed}` 每 10s 1000 / 10000 次（每批不超过 1000 个键一次 pipeline），按需加载的守卫每个同样每 10s 一次，长时间运行后的守卫加载量没有测（[benchmarks/alliance-20261104](../performance/benchmarks/alliance-20261104/README.md)）。② Redis 不可用时每次缓存未命中的查找多一次 Mongo 点读：阶段 5 断开 toxiproxy 的 redis 代理实测，空路由缓存的调用方 Redis 正常时 0 次点读，断开时每次 1 次，调用方 p50 14ms → 125ms（多出的是 go-redis 拨号重试）；跨实例两步入会照常完成，恢复后约 5s 由续期“不存在”重新登记。**断开超过单实例锁扛得住的时长时，实例因单实例锁续不上而 fail-stop**（缺省配置下断开开始后 9～12s，取决于断开落在续期节拍的哪个相位，Redis 恢复后驱动还可能再拖约 1s；算法见 [guide/01“单实例锁在后端短暂不可用时”](../framework/guide/01-app-lifecycle.md#单实例锁在后端短暂不可用时)，原来写的“`ttl − guard` 约 10s”不对）（`singleton lock lost; fail-stop`，exit 1）：单实例锁连的是同一个 Redis，所以“只影响路由效率”只对短于这个期限的断开成立，更长的断开由进程管理按同 sid 拉起（App 级“失锁即停”的统一保证，没改）。③ 死掉的 owner 不拉起同 sid 时登记按 TTL 过期，调用方回退读到的仍是它，请求一直是 no responders（“可重试”），直到它被拉起；转不走它的实体（`TransferRemoteOwnership` 要求本地是 owner）。④ 缩容入口 `(*bus.Bus).LeaveRPCQueue` 只支持轻量 RPC：JetStream RPC 的服务级 consumer 是全部实例共享的 durable，按实例退出返回 `ErrRPCQueueLeaveUnsupported`；退出后不能重新加入（重启实例）。⑤ “预加载完成前不接请求”要求路由 RPC 的 owner Mod 把 `remote_entity.preload` 写进 `OptionalDependsOn`，由生成器（方案 2R）声明；1R 只验证了预加载 Mod 的 Start 跑完才返回与依赖声明，阶段 5 已在生成工程的真实进程上验证（`scripts/test-alliance-e2e.sh` 的 `TestAllianceE2EPreloadOrder`：2000 个联盟预加载 2.0s，期间 342 次路由请求全部 no responders，完成后约 30ms 第一次得到答复）。⑥ 预加载的起服时间与内存随“owner 是本 sid 的该 kind 实体数”线性增长：阶段 5 实测 1000 / 2000 / 10000 个演示联盟预加载 1.7 / 2.0 / 13.7s（约 1.4ms 一个，并发 16），预加载完的常驻内存 65 / 71 / 123 MiB；演示联盟只有几个成员，大联盟的文档与内存没有测。
- **独占实体的只读请求与所有权查询**（2026-11-06 修复后，修复经过见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-06remote-独占实体的只读请求不再取写许可)）：（1）扫描仍是 N 次本地 Nest 事务，所有权列表 `ListOwnedRemoteEntities` 按 `_id` 扫、在 FETCH 里按 owner / kind 过滤，键数约 N + 守卫数；按 DAO 字段只列带标记的联盟仍不可行（Remote 的 DAO 文档在 Mongo 里是不透明的 `data` 字节）。（2）只读请求与写请求同样排在同一实体的写门后、占 `max_concurrent_writes` 名额，写额度被在途写占满时读也答 overloaded。（3）前提（一个实体同一时刻只有一个 owner）由 App 单实例与 fail-stop 保证；被打破时稳定持有的进程在下一次写之前读到的是自己的内存，写在投影元数据 CAS 被拒（WAL 写使进程 fail-stop，memory 写隔离后重载），计 `remote_entity.commit_conflict_total` 并记 ERROR。
- **NATS 驱动建消费者的重发**（2026-11-04 修复后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-04联盟服务冒烟里的两条观察)）：重发最多 4 次、每次 2s，连续 4 次都没有回答仍按原样失败；`EnsureStream` 同时建同一个流没有观察到同样的问题，没有加重发。
- **空认领**（`9a1d9ee4` 修复后，契约见 [REMOTE_ENTITY“路由登记”](../framework/REMOTE_ENTITY.md#路由登记)“空认领”）：① **重启前后答复的码不同**：重启前内存里还有业务 `GetOrCreate` 新建、未提交的空实例，game-demo 的 `Info` 答业务码 100031；重启后答框架的 `entity_not_found`（errcode 2）。两者都是确定的拒绝。 ③ 未采用“回滚时撤销认领”（理由见契约）。（空认领不清除见[Remote：遗留](#remote遗留)）
- **实体服务的 RPC 按顺序键派发后剩下的边界**（修复经过见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-0406实体服务-rpc-按方法串行与跨实例写超时)，契约见 [guide/09“接收端并发与顺序”](../framework/guide/09-services.md)）：
  - **同一实体的请求仍逐个处理**：一个热点联盟的吞吐就是单个请求耗时的倒数（`Info` 约 100 次/秒、`Join` 约 10～12 次/秒），要更高只能缩短单个事务；这是“同实体按到达顺序”的直接代价。
  - **同键自调用等到超时**：同一实例上，一个键的 handler 同步调用发往本实例、同一个键的 RPC，会排在自己后面，等到调用方超时。仓内没有这样的调用（alliance 的 `Join` 调的是守卫键 `ReserveGuard`，`Found` 不带键）；业务不要在 handler 里对同一实体发同步 RPC 回到自己。
  - **同一联盟 / 守卫的记录按 WAL 顺序**：同一联盟 / 同一守卫的记录仍按 WAL 顺序（同实体有序是契约）；单联盟热点的上限由单条 `Join` 约 60～80 ms 决定（Remote 写准备约 30 ms——两个实体各一次 `GrantWrite`、WAL fsync 约 10 ms、发布写“已发布”约 15 ms），约 12～16 次/秒，不在本条范围。
  - **outbox 发布归属**（2026-11-06 维护者选 A“写者自己先发”之后，仍有的限制）：① 写者崩溃后它留下的提交晚一个宽限期（缺省 30 s）才由别的进程发布（同 sid 重启立即发布；接管者写同一实体时立即代发；还需要别的进程在崩溃后扫描过一次 outbox——扫描由各进程自己的写触发，没有周期轮询，别的进程都不写时等到同 sid 重启或下一次别处的写，2026-11-06 演练时读代码确认，未改），其间订阅方收不到推送（维护者 2026-11-06 确认保持缺省 30 s、作为下限，不与收尾期限一起调小）；② 宽限期按写者写入时刻与代发方本地时钟比较，依赖进程间时钟大致同步（**2026-11-06 已演练**，时钟偏差经 `outboxNow` 注入真实进程的发布归属判定，宽限期按 1/10 缩为 3 s：实际宽限期是“宽限期 − 代发方时钟快出的量”，崩溃接管时延实测与之相符（慢 6 / 3 / 1 s 时 9.0 / 6.0 / 4.0 s，快 1 / 2.5 s 时 2.0 / 0.54 s，快到宽限期以上时下一次扫描即代发）；代发方快到接近宽限期时活着的写者退化为修复前的现象（strict 写超时、写门持有到收尾期限），偏差小于宽限期减去写者发布耗时则没有影响；任何偏差下每笔都只由 Applied 改成 Committed 一次、写者 tracker 都在上界内结束，没有正确性问题。要求各进程 NTP 校时、偏差 1 s 以内并告警，不靠调小宽限期提速；写者之间的偏差还会让 outbox 排序与写入先后不一致，“同实体后继立即代发”认不出前序，前序照常等宽限期、晚于后继发布（被版本比较丢弃），见 [REMOTE_ENTITY“时钟同步要求”](../framework/REMOTE_ENTITY.md#outbox-发布归属)）；③ 写者活着但超过宽限期仍没发布、被别的进程代发时，它的 tracker 没人通知，靠延迟收尾的期限回源结束（最多再等一次 strict 等待的剩余时间或一次收尾重试间隔），不另外回源（维护者未选方案 B）（**2026-11-06 已演练，与上界相符**：两个真实进程，写者到 NATS 的发布确认经 toxiproxy 延迟 3.5 s，宽限期 = 收尾期限 = 3 s、strict 等待 1 s；接管方写入后 3.01～3.02 s 代发、每笔只发布一次，被抢先的 strict 写 1.0 s 答“结果未知”，写门在代发后 0.95 s 释放（上界 1 s），释放后同实体的写 46 ms 确认；宽限期 = 2 × 收尾期限时代发后 0.96 s 释放（上界一次重试间隔）；写者 `finalize_projection_timeout_total` 1 / 2、`remote_entity_write_gate_timeout_total` 1（演练里故意在持门期间写了一次），接管方 `outbox.takeover_total{reason="grace"}` 2。用例 `TestRealOutboxTakeoverOfALiveStalledWriter`，`ROOST_OUTBOX_DRILL=1` 才运行，已加入 `scripts/test-remote-matrix.sh`）；④ “同实体后继立即代发”只在一页（256 条）内判断，跨页时后面的提交可能先发布（维护者 2026-11-06 确认保持页内判断，与发布失败时的同实体顺序语义一致，不做跨页状态）。
  - **顺序只在一个接收实例上成立**：改投、缩容转移、调用方并发发出的请求不在承诺之内（与修前相同）；新旧进程混跑时旧接收端按旧规则串行、旧调用方不带键，不承诺顺序（按未上线规则整体换新）。
  - **RPC 并发上限**：在跑与排队的 RPC 合计受 `bus.Config.RPCMaxPending`（缺省 `nats.worker_num` × 1024）约束，没有单独的“同时在跑”上限；下游压力由 Nest 的准入与队列承担。模块消息（`Bus.Handle`）仍按目标模块串行，handler 阻塞会挡住同一 worker 上的其他模块（没有改，模块消息依赖到达顺序）。
- **单实例锁的约 1s 盲区**（维护者 2026-11-05 决定保持现状；`TestAllianceE2ERedisOutage` 的定位经过见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-05testalliancee2eredisoutage-偶发失败)）：③ 的约 1s 盲区还在——Redis 已恢复，续期仍可能拿到缓存的拨号错误而失锁；只会让失锁提前（安全一侧），不会让两个进程同时持锁。不换客户端、不调 `PoolSize`，缺省参数下对任意相位按“稳扛约 8s”理解。
- **Remote WAL级Rejected处理**（`38b710a2`）：Durability 1/2/3 的正式链路不产生 Remote Rejected，strict / pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`；tracker 的 Rejected 收尾只剩 Durability 0 来源。守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore / MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict / pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`）。

#### Saga

协调器权威是 JetStream 分区事件日志（saga 第 4 批，[SAGA.md](../framework/SAGA.md#协调器jetstream-分区事件日志)，实施记录 [设计 §13.3](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md#133-第-4-批实施记录)）。当前边界：

- **分区归属是静态的（P-A）**：`saga.coordinators` 按 sid 分区，没有租约与再平衡；协调器进程停了，它的分区就停（新启动等判定超时 `ErrStartUnconfirmed`，在途 saga 不推进），同 sid 进程重启后从快照 + 日志恢复。改 `coordinators` / `partitions` 要先让全部 saga 终态、停全部协调器、删流与桶。
- **同一个 sid 只能有一种服务类型装 saga Mod（部署规则，没有启动期检查）**：`coordinators` 只写 sid（维护者 2026-10-29“按推荐”保持），两种服务类型用同一个 sid 且都装 saga Mod 时会认领同样的分区（单实例锁按“服务类型 + sid”，挡不住），期望序号隔离会让其中一个 fail-stop（`saga.partition.fail_stop_total{reason="fenced"}`）。配置检查看不到别的服务类型的配置；可靠的跨类型检查要另建跨进程登记（带 TTL 即模块自己的租约，不带则迁移服务类型要人工清理），第 5 批没有加，规则写在 SAGA.md「分区归属」。
- **冷副本是可选的只读回落（第 6 批，缺省关闭）**：不开时 `Get` / `GetByBusinessKey` / `List` 只看协调器内存，终态在 `saga.terminal_retention`（缺省 1h）后剪掉；开 `saga.cold_copy.enabled` 后，内存里没有的记录从 Mongo 冷副本读回（只读、最终一致，kill -9 后最晚约一个 AckWait 30s 追平）。不论开不开，**业务键唯一性只在运行期间 + 终态保留期内成立**（冷副本不参与唯一性；维护者 2026-10-31 决定 A：业务键在终态保留期后可复用，前后两轮靠协调器生成的生命周期身份区分；不采用 B（禁止复用，需冷副本成为权威、Mongo 进判定路径）），`ManualRequired` 常驻。边界：
  - 冷副本 durable 是分区清理守卫：Mongo 不可写时它的 ack floor 停住、清理随之停住，流按 `stream_max_bytes` 满了会 fail-stop——开冷副本就要求 Mongo 可写；关掉冷副本后要由运维删掉每条流上的 `-cold-copy` durable，否则清理一直被挡（没有自动清理：不知道是“关了”还是“这台还没升级配置”）。
  - 冷副本文档不过期（框架不设 TTL），历史量由运维按 `updated_at` 清理；开启之前已经被清理掉的终态不进冷副本。
  - 版本门先比 `created_at`（一生）再比版本：同业务键两生之间至少隔一个 `terminal_retention`（≥ 1m），协调器时钟回拨超过它时旧一生可能盖住新一生（不在威胁模型内，没有单独防护）。
  - `List` 回落冷副本时一页被内存持有的 ID 占掉会再翻页，最多 4 页，之后结果是精确前缀但可能少于 `Limit` 条（按 `UpdatedBefore` 缩小范围再查）。
- **参与方回执只保护计划内的重试窗口**（RV-20261030-02 修复后剩下的边界，2026-10-31）：`Engine.Register` 与配置检查要求 `saga.receipt_retention ≥ 2 × 每步重试窗口`（超时 × 次数 + 退避上限 ×（次数 − 1）），窗口按计划时间算。协调器停机、分区积压、命令在流里滞留或参与方积压，使同一操作的某次尝试晚于“第一次生效 + 回执保留期”才执行时，回执可能已被剪掉（原生步骤由同实体别的操作 `Complete` 顺手剪，Mongo 步骤由 TTL），这次尝试会再执行一次业务。缺省 168h 下余量约 168h − 窗口；协调器侧只对 `Resume` / 补偿方向的人工 `Compensate` 有 RR-A 守卫（`ErrReceiptsExpired`），自动重试没有按“操作首次派发时刻”拒绝的守卫（要在记录里另存操作开启时刻，第一轮没有加）。另外，检查只在注册了定义的进程上做：只跑参与方、不注册定义的进程按自己的 `saga.receipt_retention` 剪回执，各进程须配同一个值（部署规则，没有跨进程检查）。处置：停机超过“保留期 − 窗口”后恢复前，先按业务数据对账再放行积压的步骤命令。
- **原生步骤的多进程分片**：准入与转交由业务写（框架提供 `WithAdmit`；`RunHandedOver` 已删除，转交由业务自己写；生成的 `RegisterSteps` 注释点名要补），见 [DESIGN-TODO D12](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持)。
- **发起方查结果**：生成的 `Lookup` / `saga.Engine.GetByBusinessKey` 按 `(Type, BusinessKey)` 查；终态通知没有默认实现，需要时由最后一步 / 补偿步骤自己发 effect，或用 `Engine.WaitTerminal`（SAGA.md）。
- **一笔 saga 只有一个路由地址**（2026-11-11 saga 步骤按 sid 确切路由之后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10无缝演练最后一步普通场景机器人压测阈值时过时不过尚未确认)）：一笔 saga 只有一个路由地址。（退回重派的待确认项见[Saga：遗留](#saga遗留)）
- **清理守卫的 floor**（2026-10-29 修复后剩下的边界，修复经过见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-10-29saga-清理守卫的-floor)）：
  - **依赖“回答的是当前流 leader”**：网络分区里被罢免、还没察觉的旧流 leader 可能短暂按旧状态回答（nats-server 的读没有 read-index / 租约），floor 可能越过它没见到的命令。不在威胁模型内（崩溃重启的短暂重叠），**没有防护**；后果同原疑点（步骤命令靠协调器超时重发；冷副本缺这条终态）。
  - **承载 R1 冷副本 durable 的节点停机或长时间不回答时，这条流的分区清理一直停住**（floor 0，超过 `saga.purge_stall_alarm` 点名告警）。服务端还登记着这个 durable，投影器不会重建；节点不回来就要由运维删掉它（投影器 5s 内在活着的节点上重建、从流里还在的事件重投）。修复前节点被服务端判为离线后，它以零状态出现在列表里、被当成追上了，清理会按流末进行、丢掉还没投影的终态。维护者 2026-10-29 选 A（“按推荐，选A”）：保持现状，不让投影器自动删除长时间不回答的冷副本 durable，节点回不来时由运维删除（处置见 SAGA.md“冷副本”运维条）；步骤 durable 跟随流 R3，只在选主的几秒内不回答。
  - 每次清理对每个 durable 的每个过滤多一次流 leader 的 `MSG.GET`（带消息正文，受 `max_payload_bytes` 约束）。
- **遗留 Mongo 事务（设计 T-A，已按推荐实施）**：被 kill -9 的参与方留下的事务持锁到 Mongo `transactionLifetimeLimitSeconds` 才回收，服务端每半个参数扫一次，**最晚 1.5 倍参数**。部署要求 20s（最晚 30s 回收）；参与方启动探针超过缺省步骤预算窗口时告警（`saga.step.mongo_txn_lifetime_exceeds_budget`），DataEngine 启动排空在 `dataengine.startup_timeout` 内重试。`startup_timeout` 缺省已按维护者 2026-10-29 决定调到 60s（比最坏回收时刻多 30s，第 5 批）；参数配得比 20s 大、或期限被调小时，超过期限的那次启动失败、由部署重启。（托管 Mongo 的 `getParameter` 见[Saga：遗留](#saga遗留)）

### Sync、玩家接入与 Lockstep

#### 自身视图（self_sync）与 Sync 慢消费者规则

`access.player.self_sync: true` 让玩家经 Entity Sync 收到自己的 Player（DESIGN-TODO D6，`policy.SelfView` + `wiring/selfsync`）；推送在活着的连接上被拒即关闭该玩家连接，进程驻留预算耗尽整轮重试（维护者批准“A + A'”，[impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)）。编号沿用 `67807950` / `cf6ad085` 之后的口径：

1. 一个 `SubjectSyncState` 只登记在一个 Manager，第二次登记返回 `entitysync.ErrSubjectRegisteredElsewhere`（2026-10-20 起）。game-demo 打开 self_sync 时启动在 `service game init` 失败，自建 Manager 在登记时被拒：该组合本身仍不支持。
2. 一帧上限取接入层上限（嵌入 TCP 读 `player_access.tcp.max_payload_bytes`；独立 Gate 由 `IngressMod.WithSelfSync` 交给 Game Ingress 的 `Config.MaxPayloadBytes`；`selfsync.Config.MaxFrameBytes` 只能再收紧）。**独立 Gate 下 Game 与 Gate 两个进程的 `MaxPayloadBytes` 相等靠部署保证**（Gate 侧 `GateMod.Init` 只检查本进程 TCP 与内部上限相同）。维护者 2026-10-20 决定暂不加跨进程检查：要检查就得在握手时互相报上限，是协议改动；继续由部署保证两边配置相同。
3. 玩家实体的帧在 Manager 内编码失败（单个实体包超过 `frame.Limits` 硬上限、或超过第 2 条的上限）时 Manager 不经传输丢会话，`SessionLost` 里 `DisconnectLostPlayer` 断开该玩家（`player.sync.session_lost_disconnect.total{reason="too_large"}`），客户端重连后从全量开始；实体一直超限时每次重连都会再被断开（反复重连，见“game-demo 的移动、怪物与场景时间”里编码失败一条）。game-demo 场景同一条路径。
4. Game 进程出站驻留预算（Game Ingress `outbound.resident_bytes`）持续耗尽时 Sync 每轮在第一个被拒会话处整轮重试：排在后面的会话本轮不推送，已准入的会话下一轮改收全量（`requireSnapshotsAfterRetry`），带宽放大、延迟上升；发不出去的绑定在 `outbound.max_age` 到期时被关闭、释放预算；业务提交不受影响。嵌入 TCP 的进程预算同一规则，但没有 `max_age`（见下节）。按发送能力配预算并看 `player.sync.push_refused.total{outcome="retried"}`；需要时可让 Manager 在 RetryLater 时跳过被拒会话。
5. Gate 侧自己的驻留预算耗尽仍关闭连接，没有 Game 侧的整轮重试例外；需要时 Gate 回 NotAdmitted、由 Game 侧按预算例外处理。

第 6、7 条（C# 解码样例、重档测量）见[自身视图、嵌入 TCP 与 AOI：未测部分](#自身视图嵌入-tcp-与-aoi未测部分)。

**self_sync 默认 packer 的代价**：开关打开且 marker 没写 `subjectPacker=` 时生成器生成 `player_sync_packer_gen.go`，标了 sync 的字段默认全部对玩家本人可见，需要裁剪的写自己的 packer；marker 写了生成名 `NewPlayerSyncPacker` 时按业务 packer 处理、不生成；默认 packer 只发一个 DAO，多 DAO 的 Player 仍要自己写（[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)）。

#### 嵌入 TCP 出站

每连接有界出站队列 + 唯一写协程：上限 1024 帧（维护者 2026-10-16 决定）与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计，不开放配置键；停读客户端被关闭、不拖住场景 Sync Flush / Lockstep Tick。2026-10-20 起有全进程驻留预算 `player_access.tcp.outbound.resident_bytes`（`cf6ad085`，与独立 Gate `outbound.resident_bytes` 同口径），放不下的一帧当场被拒（`gateway.ErrResidentBudgetExhausted`，计 `player_tcp_outbound_budget_exhausted_total`），不关闭连接、不等预算；Entity Sync 当整轮重试，PB 推送返回错误由业务处理。说明见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)。

维护者 2026-10-20 决定（均按推荐）：

- **Lockstep 实时帧被预算拒**：按数据报丢失处理，不断开连接，靠冗余帧或追帧补回（A）；追帧页被拒计一次追帧失败。
- **读循环回包与心跳 ACK**：计入预算但不拒绝（每连接至多一帧在等），驻留量最多可超出预算 `max_connections × (max_payload_bytes + 16)`（A）；读循环直写快路径的那一帧不计。
- **驻留字节 Gauge 与按帧龄提前关闭停读连接**：先不做，等重档压测看 `player_tcp_outbound_budget_exhausted_total` 再定。

剩余边界：

- 停读连接占着的预算要等它撞上每连接上限被关或 `write_timeout`（默认 5s）写超时才归还，期间健康连接的推送可能被拒（Sync 延迟上升，Lockstep 实时帧靠冗余补）；没有 Gate 那样的 `max_age`。
- 只有拒绝计数，没有驻留字节 Gauge。
- 推送入队多一次 payload 复制，每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。
- 停读客户端下的预算与证据范围见[自身视图、嵌入 TCP 与 AOI：未测部分](#自身视图嵌入-tcp-与-aoi未测部分)。

#### Lockstep

- **Lockstep历史**：默认自动保留 3600 帧（60Hz 60 秒 / 30Hz 120 秒，D1 方案 C，`a6376934`）。越界按 `MatchConfig.Recovery` 区分（D1 方案 B，维护者 2026-10-20 批准，P1～P5 已实施，[设计](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md)）：配了 `Snapshots.Provider`（零值即 auto）或显式 snapshot 时，越界改发业务快照、客户端恢复后续帧，只有快照路线最终失败（取不到、过大、钉住过期）才发 C9 v2 通知让客户端终止；catchup（没配快照来源时的缺省）仍是越界明确失败、通知客户端终止、不自动重同步。快照内容来自业务的服务器参考模拟，框架不提供默认模拟、不替业务持久化检查点。本地 20000 帧周期性快照恢复与默认窗口长局有界已验证。见 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)、[SYNC-OPTIMIZATION](../framework/SYNC-OPTIMIZATION.md)。（按业务参数核对窗口见[Lockstep：遗留](#lockstep遗留)）
- **Lockstep TCP 追帧**（`640e9fac`，维护者 2026-10-16 方向 A）：`lockstep.TCPSender` 实时帧 `SendDatagram` 只准入，追帧页 `SendReliable` 经 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出，追帧随链路放慢、积压只有在途一页，`CatchupSendTimeout` 生效。等写出只在 Match 的 `launchSend` goroutine 上；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。限速链路上：快照分块与快照之后的续帧页经同一等写出机制在 toxiproxy 256 KB/s、64 KB/s 下演练过（2026-10-20，main 代码，1 MiB 与 4 MiB 都恢复、连接不断、耗时与“大小 / 速率”对应，[演练](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）；纯追帧页 2026-10-20 夜间也在 toxiproxy 256 KB/s、64 KB/s 下演练过（[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §10）：客户端回到头部的时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断，64 KB/s 下一页超过 2s 发送时限后重发一次；服务端追帧结束早于客户端收完（socket 发送缓冲），同快照演练。见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)。
- **Lockstep 追帧链路背压（独立 Gate）**：维护者 2026-10-20 决定暂不做。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，nil 只表示 Game 本地准入：Game 侧队列满返回 `ErrReliableBackpressure`（每 Tick 计一次追帧失败，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时关闭连接；追帧与快照都不随链路放慢。快照按 `UnpacedBytesPerTick`（缺省 16 KiB/Tick，30Hz 约 492 KB/s）固定节流；2026-10-20 toxiproxy 实测（30Hz、缺省参数，[演练 §3](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md#3-独立-gate63-风险-1-实测成立)）：64 KB/s × 1 MiB、256 KB/s × 4 MiB 每次连接都在同一位置被 Gate 断开、重连后从头重传，三次都未恢复；256 KB/s × 1 MiB 恢复（低于预算的超出部分被缓冲吸收）。是否断开取决于快照大小、每 Tick 预算与链路速度三者；断开发生在 Gate 写 socket 的期限到期（`writev ... i/o timeout`，`gate.channel.failure_total{phase="socket"}`），不是 `max_age` 积压规则。2026-10-20 起恢复中被 Detach 计 `lockstep.snapshot.failed.total{reason="detached"}`，这个循环可见。配置建议见[设计 §6.3](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#63-独立-gate只准入时的行为与风险)。要做到随链路放慢，Gate 需要在写出 socket 后回执给 Game，这是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。生成 game-demo 在独立 Gate 部署（`accessplayertcp.NewIngressMod`）下拿不到 `TCPRuntime`，战斗的追帧页同样只准入（`battleLinks`）。（断开的期限来源、纯追帧页见[Lockstep：遗留](#lockstep遗留)）
- **Lockstep 快照恢复的剩余边界**（2026-10-20）：已提交门要求宿主提交落后切帧不超过 `RedundancyDepth - 1` 帧，否则恢复中的会话一直经可靠通道逐页跟随、不切回实时（Room 每 Tick 同步提交，不受影响）；③ 服务端“写出”只到本机 socket 发送缓冲（嵌入 TCP）或 Game 本地准入（独立 Gate）：服务端认为恢复结束时慢链路客户端可能还差 1 MB 以上，之后的实时帧排在后面，恢复后头几秒延迟等于排空时间（演练推断，未单独测量缓冲）；④ 快照不续传：断开重连后从头重传，独立 Gate 慢链路上会循环（上条）；⑤ Provider 不响应 ctx 时 `FetchTimeout` 后可能再发起一次，“每局至多一次在途”不成立；⑥ 宿主手动 `TrimHistory` 越过钉住点时快照作废、按失败计，但不计 `failed` 指标。（①②⑦⑧ 中未测、未做的部分见[Lockstep：遗留](#lockstep遗留)）
- **game-demo 战斗经 `lockstep.TCPSender`**（维护者 2026-10-20 决定）：生成 demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`）两条通道改经 `TCPSender`，嵌入 TCP（生成默认）下追帧页经 `TCPRuntime.PushLockstepSessionWritten` 等写出，与上面嵌入 TCP 追帧同一规则；回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（生成工程 `go test ./internal/service/game`）。线格式随之变化：10102 推送的是 Lockstep 包本身（RS v2 Lockstep 标志、不带 PB 包装与 battle id），按连接（接入层 SessionID）寻址；座位在它第一条 BattleInput 到达的连接上加入，没说过话的座位收不到帧，晚加入的座位靠追帧补齐。一条连接同时只打一场战斗（帧不带 battle id）。（演练见[Lockstep：遗留](#lockstep遗留)）

#### 会话 Hold / Ready

Hold 之后撤到一个订阅都不剩时，Ready 后补发一个不含对象的 Full（[ENTITY_SYNC §会话](../framework/ENTITY_SYNC.md#会话)）。剩余边界：

- **每次 Hold → Ready 都按全量重发仍在的对象**：没有“只停发布、保留基线与待发 Remove”的暂停原语（方案 B，维护者决定暂不做，[DESIGN-TODO“保持现状的决定”](DESIGN-TODO.md#待维护者决定不急)第 4 项）。按周期刷新权限、每轮都 Hold 的业务（SLG §7.5 / §7.7）每个周期每个对象重发一次全量；SLG 文档建议只在权限读失败与重连时 Hold。
- **仍有订阅但一时没有可发的快照时，新 epoch 的 Full 会推迟**：例如 Ready 时剩下的订阅指向已卸载、等待重载的实体，或被冷创建预算推到后面的窗口；Full 发出之前接收端还留着上一 epoch 的对象（包括 held 期间撤掉的）。只在一个订阅都不剩时才补空 Full（读代码推断，未写用例）。

#### 同一 SessionID 的重连

嵌入 TCP 的 `TCPServer.registerSession` 与独立 Gate 模式下 Game 的绑定表（同一 Gate 实例内）都按连接被接受的先后定同一 SessionID 的新旧（2026-11-04，回归 `TestALoginThatFinishesLateDoesNotDisplaceANewerConnectionOfTheSameSession`、`TestALateBindOfAnOlderConnectionDoesNotRetireTheNewerBindingOfTheSameSession`）：迟到完成的旧登录 / 旧 Bind 被拒，不关闭新连接。跨 Gate 实例按客户端在认证帧里带的连接代号定（2026-11-05，回归 `TestAcrossGatesALateBindWithAnOlderConnectionGenerationDoesNotRetireTheNewerBinding`、真实 NATS 的 `TestRealAcrossGatesALateOlderBindDoesNotDisplaceTheNewerConnection`）。契约见 [GATEWAY §3](../framework/GATEWAY.md#3-绑定与请求结果)。剩余边界：

- **跨 Gate 时没带连接代号的客户端仍按 Bind 完成先后替换**（已实现，2026-11-06）：C# 运行库（`client/dotnet` 的 `TcpSession`，`Connection` / `GameClient` 与 Unity 托管适配都经它）和 C++ 运行库（`roost_connect`，Unity `NativeGameClient` / `NativeReconnect` 与经 C ABI 的 Unreal 都经它）每次连接自动带连接代号，与 Go robot / loadtest 一致（[client/README](../../client/README.md#鉴权首包的连接代号)）。覆盖：三种语言用同一份 Go 金样 `auth_generation` 逐字节核对编码，C# 与 C++ 各有同一进程两次连接代号递增的用例，C++ 另经 Unity 托管绑定（`tests/managed`，dotnet 宿主）跑重连；端到端只有 C#：`TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection`，同进程两个 Gate + 一个 Game，用的是进程内 NATS 替身，不是真实 NATS，C# 是真实进程，事件先后用屏障控制，修前红、修后绿。剩余边界：① 同一会话的两次握手只要有一次没带代号（不是这三套运行库的自建客户端、没重新打包的旧 Unity 插件或旧 DLL），仍是后完成者胜出；③ 下面两条（Gate 发 Bind 之前就慢、时钟回拨）对三套运行库同样成立。（② 见[客户端：遗留](#客户端遗留)）
- **带了代号也识别不出的迟到 Bind**：Game 只拒“到达 Game 早于新绑定被确认”的旧 Bind。旧连接的 Gate 在发出 Bind 之前就慢了（Gate 本地鉴权、`ResolveGame`、身份核对、NATS 积压），旧 Bind 在新绑定确认之后才到达时按后完成者胜出处理，新连接被断开（客户端重连一次恢复）。这条条件是为了让代号锁不死会话（论证见 GATEWAY §3），没有测量这种情形的实际概率。
- **客户端重启前后时钟往回拨**：各运行库（Go / C# / C++）的代号都取 `max(上一个 + 1, 当前 Unix 纳秒)`，重启后若时钟被往回拨，新进程的代号可能小于旧进程仍在途的握手；两者在 Game 上重叠、且旧的先确认时，新连接的 Bind 被拒一次，重连即恢复（读代码推断，未写用例；绑定表的“确认之后到达照常替换”有 `TestBindingTableOrdersASessionAcrossGatesByConnectionGeneration`）。
- **“接受先后”只代表同一客户端的重连意图**：两台设备用同一 SessionID 交错登录时，留下的是接受得更晚的那条；SessionID 本来就标识一次登录，框架不另做裁决。
- **Robot 的 `connect` 发出认证帧不等 ack**：连接动作返回不代表握手完成，紧接着的 `Reconnect` 让两条连接的握手在服务端交错；修复后这种交错不再踢掉新连接（同一 Gate 内靠接受序号，跨 Gate 靠 Robot 自 2026-11-05 起在认证帧里带的连接代号），Robot 不等 ack 的行为本身未改。

#### 广播与 AOI

- **SyncBus投递**：有限次数、保留期限及容量，不保证绝不丢；定位为默认可丢广播（维护者 2026-10-15）。需要最终一致的业务自建快照 / 对账闭环，不能仅测重试成功；兜底写法见 [guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)；框架级对账是未排期候选（[DESIGN-TODO](DESIGN-TODO.md#未排期候选) D2）。
- **policy Drain**：等待 retry 和已承担事实；永久拒绝仍需修关系或 ctx 超时，Drain 不放弃条目，超时返回 `*DrainStalledError`（列出卡住条目、原因与等待时长）并计数（D3，维护者 2026-10-15 决定 A，`b86a1286`）。“放弃出口”（D3 方案 B）需先梳理永久拒绝的判定来源。
- **Interest 单写者（[方案与实施结果](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/REFACTOR-2026-10-21-interest-single-writer.md)）的边界**：① 全部写入口只登记，效果在下一次结算可见（ApplySync 下一次 Flush、ApplyAsync 应用线程走到之后的 Flush），登记之后立即读 `Visible` / `Seen` 会先执行日志（诊断读取应用侧锁，不在快池上调用）；`Apply()` 同样取应用侧锁，只给测试 / 工具 / 离线驱动。② 未确认的移动被同一目标之后的 `Hide` 越过时整条作废：跟随该目标的视野停在 Hide 前已应用的位置（随后的 `Show` 会把视野带到新位置）；已确认的移动不受影响。③ `Leave` / `Unwatch` / `Hide` 之后同一次结算里又被持有的 pair 先退订再订阅：`Hide` → `Show` 落在同一周期时客户端多收一次全量。④ 命令（`Enter`、视野操作等）不计入 `MaxQueuedFacts`，只有门控事实受限；Flush 长期停摆时命令日志无界增长。⑤ **ApplySync 预算**（维护者 2026-10-22 决定 A，[方案 §12](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/REFACTOR-2026-10-21-interest-single-writer.md#12-applysync-预算把结算也算进去2026-10-22维护者决定-a)）：一次政策阶段的执行 + 结算按同步周期的一半估计，预计超出就停、剩下的顺延，重负载下 AOI 的变化晚一两个周期可见；没有配置项。不是硬上界：启动后第一次（还没有估计）、一个单位本身超预算（至少前进一个单位）、切点要等已取用的提交结束（见 ⑥）、成本突变时可以超出；命令也计入单位，只有命令的一段日志同样按预算分步（维护者决定命令计入预算、保持纯前缀切点，[DESIGN-TODO“保持现状的决定”](DESIGN-TODO.md#待维护者决定不急)第 5 项）。积压看 `entitysync.interest.log_backlog` 与 `entitysync.interest.budget_stop.total`。⑥ **一步只在日志的前缀边界停**（维护者 2026-10-22 决定 A，[方案 §11.2](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/REFACTOR-2026-10-21-interest-single-writer.md#112-批次切点之后同一提交的后续事实越过排在它前面的命令2026-10-22产品缺陷本重构引入已修维护者-2026-10-22-决定-a纯前缀切开)）：(a) 到了上限（`BatchSize` 或预算单位）之后，切点要等已取用的提交全部结束，一步会多执行这些提交在 handler 期间别的线程交错登记的条目，上界是这一步取到的日志快照；`BatchSize` 因此只是软上限；没有对超出量做数值测量。(b) 一笔提交只要有一条事实还在等（它的某个实体前面还有没确认的提交），整笔不取用，同一笔里已就绪的实体与同一目标后续的提交连带等；前序提交永不确认时一直等，由 `MaxQueuedFacts` 背压与 Drain 诊断 `commit_pending` 暴露。(c) 应用失败（位置越界等）的事实之后同一 `(目标, 代际)` 的事实留到下一步，所属提交分两步应用（错误路径）。⑦ sync-aoi 的 direct 基线不再是“AOI 全部在压测线程”：政策阶段可能先于压测线程的 `Apply()` 执行一部分日志，与重构前的 direct 数字不能直接比。（⑧ 的未测部分见[自身视图、嵌入 TCP 与 AOI：未测部分](#自身视图嵌入-tcp-与-aoi未测部分)）
- **空间视野总数上限 `MaxSpatialViews`**（2026-10-22，[O3](../release/v1.31.0-NOTES.md#o3)）：① 只数 `DefineSpatialView` 定义的业务空间视野，玩家主视野不计（由在线玩家数约束），所以 AOI 观察者总数 = 主视野数 + 至多 `MaxSpatialViews`；② 缺省 8192 来自一台机器（M5）、一种密度（1000 玩家、每视野可见约 50）的 D19 实测，玩家更多、更密或移动比例更高时同样数量的视野政策阶段更长，没有测；③ 只在定义时拒绝，不按耗时自适应，ApplyAsync 没有政策阶段耗时预算。
- **AOICluster 容量抖动**：Cluster 的 `MaxVisible` 每次移动后严格等于候选内最近 N 个，容量层没有滞回，两个目标在第 N 名附近来回时每次移动都 Leave/Enter（`BenchmarkClusterFlushRandomWalkCapped` Enter +15.6%）。维护者 2026-10-17 决定不加滞回：抖动只在候选数超过 N 时出现，不设 `MaxVisible` 或把 N 设到常规候选数之上即可，容量只作人群聚集时的带宽保护阀。单区域 AOI 不受影响；Cluster 目前没有生产装配调用方。见 [INTEREST-BLOCK-AOI](../framework/INTEREST-BLOCK-AOI.md)。
- **AOI 视野（D14 / D19）**：① 视野变大时新进入范围的目标一次性订阅、各下发全量，峰值受 `MaxVisible`（按视野计）与 Manager 快照预算约束，没有按玩家单独的预算。② 改视野、视野操作都没有排队版（维护者决定），事务内决定的视野变化（入盟、放眼、组队、Buff 改视野）由业务在提交之后调用。③ 一个玩家挂多个视野时，它持有的目标数受 Manager `Limits.MaxObjects`（缺省 100，也是每帧对象上限）约束：`Subscribe` 不按它拒绝，超出时（2026-11-08 起，D17）不关会话：Manager 暂缓放不下的新对象，Interest 收紧视野、留下最近的（见上“超额降级的剩余边界”）；业务仍用各视野的 `MaxVisible` 把持有量压在 `MaxObjects` 以下，按玩法调大，否则玩家会在边界上少看几个。④ 共享视野的扇出：一次变化的成本 = 变化量 × 挂接人数，由 `MaxPlayersPerView`（缺省 4096）约束；`BenchmarkRelationViewMemberChange` 1024 人挂接、1024 成员时约 0.78ms；sync-aoi 共享关系视野 100 / 1000 人与单视野同量级（[NIGHT §14'](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md#14-aoi-三点对照d14--d19)）。⑤ **两种应用模式**（维护者 2026-10-21 由业务选，选择指引见 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)）：**ApplySync（缺省）** 每玩家视野不超过主视野 + 8（含关系视野），超过拒绝构造；AOI 在 Flush 的政策阶段，原场景（1000 玩家、可见每视野约 50）Flush 均值 4 / 8 个额外视野时 15 / 25～27 ms，玩家更多、更密或移动比例更高时同样 8 个视野 Flush 更长，没有测；**ApplyAsync** 不限视野数、Flush 只结算，比 direct 多约一个同步周期（维护者接受，选 C），改值集中在周期末尾到达的负载下尾部会更差，没有测。本夹具里 ApplySync 的测得延迟没有比 ApplyAsync 低，它的好处是“提交后下一次 Flush 必定可见、没有额外线程”。⑥ sync-aoi 夹具是内存提交（`DurabilityMemory`），strict / pipelined 的 WAL 等待与排队事实就绪的关系没有测；新订阅全量只计本 tick 改的值，跨 tick 才送到的全量不计入，全量 P99 偏乐观。⑦ 包含关系按 `ViewSets` 字段判断；不配 `ViewSets` 时不同层级一律取并集身份，手写 packer 若不经 `SyncViewSet.Lookup` 解析就要自己认 `SyncProfile.Layers()`。⑧ 层级并集只在一个 Interest 内部合并；同一 pair 同时被 Interest 与 Group / Direct / SelfView 持有时，Manager 仍按优先级取一个视图（维护者 2026-10-21 按推荐保留）。⑨ 关系视野不排除挂接玩家自己的实体（按包含取高或取并集，维护者 2026-10-21 按推荐保留）。⑩ D14 / D19 前后对照（2026-10-20 夜间）：`ClusterFlushRandomWalk` +3.5%（D14 引入），其余 AOI / Cluster 基准无差异；sync-aoi 单视野三点无差异。⑪ 可见性只看距离与关系视野，没有按观察者过滤（隐身、相位、遮挡），`MaxVisible` 按距离选、没有“必须可见”的优先级（[DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#优化项下一阶段)的“按观察者过滤可见性”“MaxVisible 优先级”两行）。见 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)、[impl/04-sync §2E](../framework/impl/04-sync.md#2e-interest-的视野d19在-d14-上扩展)。（大联盟未压测见[自身视图、嵌入 TCP 与 AOI：未测部分](#自身视图嵌入-tcp-与-aoi未测部分)）

#### 场景主视野的视图与 Codec

**实体同步增量字节（帧协议 2 / 信封 v3，2026-11-10）**：一步移动已从 140 字节降到 30 字节（[证据](evidence/sync-delta-bytes-20261110/README.md)）。

- **视图声明**（2026-11-10 主视野改用 `near` / `team` 之后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10场景主视野改用-near--team-视图)）：`ViewSets` 要求不会用到某层级的类型也声明它（怪、建筑的 `team`），框架没有“只校验会出现的类型”；按视图键注册解码器的客户端要为 `near` / `team` 各注册一次。
- **同一视图换 Codec**（见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10同一视图换-codec-时改发全量)）：代价：受影响的会话晚一个 Flush、多收一份全量。见 [ENTITY_SYNC 线格式](../framework/ENTITY_SYNC.md#线格式帧协议-2--信封-v3)、[证据](evidence/sync-views-codec-20261110/README.md)。
- **继续省字节的候选**（证据 §3，按收益；维护者 2026-11-10：先不做）：增量补丁不带 `sync_format`（3 字节）；标量字段包装改 proto3 `optional`（每字段 2 字节）；增量不写 Mask（1 字节）；“一个对象一个组件”的帧简写（约 3 字节 / 对象）；BaseVersion 只发低位（运行久了 1～3 字节，削弱逐对象版本核对，需决定）；全量的命名空间改编号（只省全量）。远处降频 / 距离 LOD 本轮按要求不做。
- **on_change 冻结修复后的代价**（见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10建筑进场景主视野后的-demo-尺度维护者-2026-11-10主视野已按推荐放大见地形方案-28偶发已定位修复见本条末段)）：交付在途时提交的变化，Flush 要多取一次 Entity 锁、多打包一次增量。
- **按实体类型的同步节奏（TypeSchedules，2026-11-09）**：TypeSchedules 支持同 Manager 内按 SubjectKind 配置周期脏增量；即时快照独立交付并保留待发变化。代价：脏周期内的即时快照使下一次周期更新回退 Full；仍按 tick 检查 pending 脏集合，提交观察者可能合并唤醒，不宣称消除全部调度成本。见 [类型调度契约](../framework/ENTITY_SYNC.md#按实体类型的同步节奏2026-11-09)。（demo 的性能验收见[同步：尚未确认的偶发与疑点](#同步尚未确认的偶发与疑点)）

#### game-demo 主视野名额、会话丢失与超额降级

- **主视野只留最近 64 个**（维护者 2026-10-22 按推荐）：所有玩家从同一出生点进场，名额 64（2026-11-10 起是场景配置 `gamescene.View.MaxVisible`；开地形的普通场景是游戏尺度的 128，地形方案 §29）、会话另持有自身、场景实体、队友共 3 个（`sceneReservedObjects`），会话上限由名额推出（`sceneSessionObjects` = 名额 + 3 + 33：100 / 164）；以后加的关系视野要在剩下的 33 个里。超出 `MaxObjects` 时不关会话、不断开（2026-11-08 起超额降级，D17）：放不下的新对象暂缓，Interest 收紧这名玩家的视野、留下最近的；见下“超额降级”。剩余边界：
  - **同步会话因编码失败被丢时断开该玩家，原因持续时会反复重连**（维护者 2026-10-22：编码失败就关闭连接）：Manager 交给 `SessionLost` 的原因带分类（`entitysync.SessionLoss` / `LossReason`：`push_refused` / `too_large` / `encode_failed`；`object_limit` 随超额降级删除），原因不是推送拒收时 `wiring/selfsync.DisconnectLostPlayer` 断开该玩家的全部连接（自身视图经 `EntitySyncSetup` 自动接上，game-demo 场景在 `sessionLost` 里调用），计 `player.sync.session_lost_disconnect.total{reason}`，打限频 Warn（每 10 秒至多一条）。客户端重连、`Join` 从新 epoch 的全量开始。剩余边界：
    - **原因一直在时反复踢、反复重连，框架不做防风暴**：例如自己的实体始终超过帧上限（超员的人堆自 2026-11-08 起走超额降级，不再断开）。一个循环的服务端成本是一次鉴权握手 + 一次 `Join` + 一次编码尝试，节奏由客户端的重连退避决定，上限是接入层已有的握手预算与单 IP 连接上限。不另加按玩家的冷却（维护者决定不做，[DESIGN-TODO“保持现状的决定”](DESIGN-TODO.md#待维护者决定不急)第 10 项）。发现靠 `player.sync.session_lost_disconnect.total{reason}` 持续增长与 Warn 里反复出现的同一个玩家 ID；根治在业务侧（实体大小）。
    - **断开与 `settle` 之间可能多 settle 一次**：嵌入 TCP 的 `CloseSessions` 关 socket 后连接注销可能晚于场景的 `settle`，那次 `settle` 数到连接、把玩家留下；随后的会话关闭事件（`watchSessions`）再 settle 一次、玩家离场。没有新增状态，与推送拒收路径相同。
    - **只断开“会话 ID 就是玩家 ID”的 Manager**：业务自建、会话 ID 是别的东西（连接 ID、机器人）的 Manager 要在自己的 `SessionLost` 里按 `LossReason` 处理，框架不替它判断连接归属。
  - **挤满时看见的是最近的 64 个**：近的进来挤掉最远的；没入选的目标要等它（或观察者）下一次移动才重新参评（AOI `MaxVisible` 的近似语义）。挤在出生点时最近的 64 个可能全是玩家，旁边的怪看不见，`scene_expect subjects: 2` 由自身 + 任一其他目标满足，不会因此失败。
  - **超额降级（D17，2026-11-08）的剩余边界**（契约见 [ENTITY_SYNC“超额降级”](../framework/ENTITY_SYNC.md#超额降级会话持有对象超过上限)）：
    - **收敛要几轮 Flush、秒级**：Manager 暂缓 → 通知 Interest → 收紧、退订最远的 → 下一次 Flush 放回最近的暂缓对象。快照预算挡着、政策退订的是还没捕获的订阅时，要等每秒一次的再通知（`overLimitRenotifyInterval`）；地形用例（视野 80 > 上限 64）825 次 Flush 后稳定在 64 块，用例耗时约 2 秒；没有证明收敛时间的上界。期间客户端持有的永远不超过上限，只是“哪 64 个”还没换成最近的。
    - **放宽是试探式的**：冷却 1 秒起，上调后又超额就加倍（至多 32 秒）。拥挤边界附近每次试探在 Manager 内部多一次暂缓，不上线；人群散去时上限不再起作用就直接撤掉。上限停在边界上时，新进入范围的更近目标要等成员离开或下一次试探才换入（AOI `MaxVisible` 的近似语义照旧）。
    - **只有 Interest 收紧**：Group / Direct / GroupView 没有距离，超额时订阅一直暂缓到有空位，按（Profile 优先级、实体 ID）放回；`AOICluster` 没有接这条回馈，超额时同样只暂缓。共享空间视野被收紧时，挂接它的其他玩家一起少看。
    - **Manager 只为钉住的订阅挤已持有的对象**：钉住的只有自身视图与 Interest 的 self；业务自建政策要“必须可见”的对象时用 `SubscriptionSource.SubscribePinned`。同类全是钉住的对象时钉住的也只能暂缓（`pinned_blocked`）。
    - 基准 `workers=4` 慢 7～11% 的原因见[同步：尚未确认的偶发与疑点](#同步尚未确认的偶发与疑点)。
    - **配置错误只降级、不报错**：`MaxVisible` 之和大于会话上限不会在启动时拒绝，运行期表现为持续的 `entitysync_over_limit_*` 与 `CheckHealth` degraded（告警基线第 15 项）。game-demo 启动期的 64 + 3 ≤ 100 核对仍在。
  - **单 IP 连接上限**：机器人全从 127.0.0.1 来，`player_access.tcp.max_connections_per_ip` 缺省 128，超出的连接在握手前被关（2026-10-22 200 人风暴里 72 个 `connect` 失败即此，`player_tcp_connection_rejected_total{reason="per_ip"}`）；压 128 个以上要调高该配置。

#### 组同步与 self_sync

- **房间内同步与 self_sync 同进程**（2026-10-31 修复后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-10-31房间内同步与-self_sync-同进程)）：要求 self_sync（会话归自身视图）；成员关系不持久，重启后靠玩家重连的 handler 再 `Join`；离线成员每个政策阶段复查一次，成本与有离线成员的组的人数成正比。容量与独立 Gate 下的用例见[游戏接入验收](#游戏接入验收)。

### 生成器、配置与模块

<a id="生成器配置与工具"></a>
#### 生成器、配置与工具

- **DAO 库名映射**（[D9](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A）：`db=` 是逻辑库名，物理库由 `dataengine.databases` / `WithDatabaseResolver` 在启动时冻结。限制：①改映射等于搬数据，框架只在 WAL 还有未投影记录时拒绝启动（`ErrDatabaseMappingChanged`），不复制数据；排空后改了映射却没迁移，新库是空的，冷加载得到“从未创建”，GetOrCreate 会新建玩家——步骤见 [PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)。②声明了却没有映射的逻辑库物理名就是逻辑名本身，只在启动日志告警（生成配置总写 `main` 的映射，手写配置删掉它会落到名为 `main` 的库）；这条回退只在有表时存在；根本没有表时 DataEngine 投影存储（`MongoStoreConfig.Databases` 为 nil）与 Remote 存储（没绑定）同一条规则：读写带库名的 DAO 文档返回 `dataengine.ErrDatabaseMapNotBound`，两边不再不对称（维护者 2026-10-21）；`SetRemoteProjection` 把 DataEngine 的表自动绑给 Remote 存储，自建装配只给 DataEngine 一张表。剩余边界：`NewMongoStore` 只在进程有声明的逻辑库时构造即拒绝，手写 DAO（没经生成器声明）缺表要到第一次投影才报错，那时记录已进 WAL，投影器反复重试这条记录（`ProjectionFailures` 增长、后面的记录不投影），补上表重启后照常投影（WAL 目录记下的上次映射为空，不触发 `ErrDatabaseMappingChanged`）。③不同进程（多个 sid、跨进程写 Remote 实体）的映射是否一致不校验。④二进制一个逻辑库都没声明（工程还没有 DAO）时，配置里的逻辑名无从做拼写检查，只告警。⑤工具经 `LoadDatabaseMapFile` 读同一份配置，但不会自动执行进程里注册的回调，用了回调的部署要给工具传同一个函数或用 `-db` 指定物理库。⑥根包守卫只看同一行里交给 `.Database(` 的参数，先赋给变量再传入的写法看不到，由各解析点的行为测试兜底。⑦回调与配置对同一逻辑库给出不同物理名时拒绝启动（同一逻辑库只能有一个来源，维护者 2026-10-20），但回调只在 Init 调用，`--check-config` / doctor 不执行回调，这类冲突要到进程启动才发现；工具传入回调时（⑤）按同一规则拒绝。
- **DAO 引用形状生成期拒绝**（2026-10-30 维护者决定 A，已实施）：生成器只给 `[]byte` 字段的 `Get<Field>All()` 和 map 的 `[]<基本类型>` 值（`Get` / `Range` / `Set`）收发副本；其余引用形状——map 值是 map（`map[K]map[K2]V`）、指向非嵌套类型的指针（`map[K]*int64`、`*int64` 字段、`[]*int64`）、元素仍是引用的切片（`map[K][][]byte`、`[][]byte`、`[]map[K]V`）——取得的值会与 DAO 内部共用内存，改它绕过 setter（无脏位、不进 WAL 与同步、回滚覆盖不到），所以在解析阶段报错，点名类型、字段与写法并提示改用 `[]byte` / 基本类型切片 / 基本类型 / 嵌套类型指针（规则见[命令与标记参考 §4](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#生成物与读写约定)）。代价：已有这类定义的工程重新生成时报错，需要改定义（线上未部署，不做兼容）。剩余限制：判定按语法，底层是 slice / map 的命名类型（`type Tags []string` 再写 `map[K]Tags`）认不出；非指针嵌套 struct 值（`T`、`[]T`、`map[K]T`）不在这条规则范围；`//roost:redisdao` 整值读写，不查。2026-10-30 核对：仓内全部 DAO 定义（codegen testdata、game-demo 模板、`roost add` 骨架与 saga 回执）与下游 ai-game-blackbox-server 的 `db/def/player.go` 都只用允许的形状。
- **doctor `config-sections`**：配置文件生成后归应用所有，`add` 命令只在让服务多出 Mod 的那一刻追加缺失段；在 add 命令之外手改 `roost.yaml` 增加 Mod 后 `project sync` 不补（维护者 2026-10-17 决定 B），由 doctor 只读检查 `config-sections:<service>` 报 WARN 并给出可追加的文本。限制：业务有意删掉的段同样报 WARN，没有豁免开关（可忽略或留空段如 `saga: {}`）；“有没有”只看行首 `<key>:`，段里缺键、值形态不对不报（归 `config-schema`）；Secret 示例结构认不出（没有恰好一个 `config.yaml: |` 块）时只报“未检查”。
- **配置叠加 `config.includes` 的工具边界**（2026-10-30，R5 方案 A，规则见[设计 §1.1](../framework/guide/07-config.md#11-多文件叠加configincludes)）：①doctor `config-sections:` 对叠加的入口不查（配置段散在多个文件里，“缺段 + 追加文本”按单文件成立），该项说明里写明未查，键由 `config-schema:` 按叠加结果检查。②生成器 / doctor 的写入动作（`roost add` 追加段、停机预算刷新）只改入口文件文本，内容在 include 里的段不会被改到。③k8s Secret 示例里内嵌的 `config.yaml` 不解析 includes（没有所在目录可解析相对路径）。④App 侧错误的来源文件按错误文本里的 `config: <键>` 匹配，键不在叠加结果里（例如 required 缺失）时只列叠加顺序、不点名文件。⑤叠加经 viper `AllSettings`（与 cube 相同），`~` / `{}` 不清前一层的值，没有“删除键”的写法。
- **`roost project next` 的文件名约定**：first-business 修复提示按工程现状填名字，判断“缺协议 / 缺 endpoint”沿用 doctor 的文件名约定（`game/handler/<snake>.go` ↔ `protocol/def/<snake>.go` ↔ `game/controllers/<域>/<snake>.go`），协议名与 handler 名不同的工程仍会落到第一个可用名字或示例名。
- **endpoint 服务器决定的参数**：只识别业务时间这一类约定名（`nowUnix` / `nowUnixMilli`，`int64`，由生成的 Nest 分发包装在 handler 执行时从 `fctx` 填，D22-1）；其他服务器决定的值（玩家状态、配置）在 handler 里读，没有 `--server-param` 一类的通用标记。约定名是全局的：任何 `//roost:nest` handler 的参数叫这两个名字就不再从消息里取，要调用方传一个时间时换个名字（例如 `resolvedAtUnix`）。
- **Saga / Remote / Skill 的 doctor 检查**：`remote-runtime` 在带 Nest 的服务部分缺 `remote_entity` 时只 WARN（实体由哪个服务写静态上看不出）；技能目录由生成的 `catalog_test.go` 与 doctor `skills:catalog` 检查，服务 Init 里调 `CompileAll` 归业务。
- **configdata 重载内容未变也递增版本**（维护者 2026-10-18 决定 B）：照常发布、递增版本，回答 / 日志 / 指标标明 `changed=false`（`configdata.reload.total{result="unchanged"}`）；理由是监听者依赖每次重载都执行（如开关表用重载恢复表值）。版本号不是内容版本，内容是否变化看 `changed` / `hash`。
- **文档提交号门禁**（`TestDocumentedCommitsAreReachableFromHEAD`）：浅克隆（CI 默认 depth=1）整条跳过，只在本地完整检出与 release 工作流（fetch-depth: 0）生效；纯数字短 SHA 与不带反引号的提交号不检查；只查“在 HEAD 历史里”，不查是不是文档所说的改动。分支上的文档引用本分支提交时 rebase 后会红：引用本轮改动优先写测试名 / CHANGELOG 条目，或合入后改成 main 上的提交号。
- 嵌套 nil map 的文档形状、开发配置的 WAL 目录、`-template game` 的 Player DAO 见[生成器与工具：遗留](#生成器与工具遗留)。

#### 生成器接入心跳、行为与时间域

`roost add heartbeat` / `roost add behavior [--ai] [--state]` / `roost add timedomain`（[D20“生成器接入”实施记录](https://github.com/tjbdwanghaibo/roost-core/blob/3fddb51ea19dcd66451fd0ce8c80dcdf6b04be45/docs/maintenance/DESIGN-TODO.md#生成器接入实施结果2026-10-21)，用法见 [CODEGEN_REFERENCE](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-heartbeat--behavior--timedomain实体心跳行为与时间域)）。

- **按需装配，不进缺省 Mod 集**（维护者 2026-10-21 晚按推荐）：`heartbeat` Mod 只在 add 命令（或 `roost add mod heartbeat`）之后才装在那个服务上。实体侧零开销，进程侧装上之后驱动空转也每 10ms 走一格、停机预算多 3s。
- **工程只有一个驱动变量**：`game/heartbeat.Driver`，依据是生成工程每个进程只跑一个服务；同一个进程里起两个装了 heartbeat Mod 的 App（测试）时它们共用一个驱动：后一次 `Bind` 覆盖前一次的 Nest 客户端（`Bind` 允许重复调用），拍都投到后接上的那个 Nest。驱动的 `Name`（日志 / 指标标签）是工程名，不区分服务。
- **示例 AI 的 `EndActions` 结束全部组的动作**：生成的 `ai.ControllerHooks.EndActions` 是 `Behavior.EndAllAction(true, reason)`（切换策略时清场），写在生成部分，业务要换成“只结束某些组”需要另写 Controller，生成器不提供钩子。
- **成员挂场景时间域由业务写**：生成的 `timeDomain()` 返回 nil；域在构造时定下、运行中不能换（D22 边界），实体换场景要 `Close` 旧的行为组件后重建（`Behavior.Close` / `Controller.Close`，见上文“域要在构造时给”），生成器没有生成重建入口；生成的 `BehaviorComponent.OnDestroy` 已调 `Controller.Close` 与 `Behavior.OnDestroy`。
- **时间域字段缺省 `nopersist,sync`**：随进程结束的副本场景；要持久的场景手工改成 `persist`（DAO 定义归业务）。三个字段名固定，已有同名字段只有一部分或类型不同时 add 拒绝。
- **去掉 `heartbeats` / `time_domains` 条目后生成部分不自动删除**：与 `timers` 相同，需要手工删 `*_component_gen.go` 与实体里的组件字段；服务 `mods` 里的 `heartbeat` 也不自动去掉。
- **`heartbeat.*` 配置段跟着 Mod 的声明走**：生成配置写 `heartbeat.delivery: batch`（选项 C 的开关，维护者 2026-10-22 起缺省批量作业；`dispatch` 是单目标可选项）；配置段、doctor 与 `--check-config` 用 `kitconfig_gen.go` 里的快照（`wiring/internal/configschemagen` 的 `heartbeat` 组），Mod 加新键后要重新 `go generate ./codegen/internal/roost`（`TestKitConfigSchemasMatchKitDeclarations` 守）。已有配置里改过的 `heartbeat:` 段不被改写。
- 端到端用例的范围见[生成器与工具：遗留](#生成器与工具遗留)。

#### 业务模块的既定设计

模块各自的边界与未完成项见[业务模块（roost module）](#业务模块roost-module)。

- **没有卸载与升级**：安装只加不减，分配过的号与存储键不回收；内置模块之后变了，已装的模块不会跟着变，安装器只报告“装的是另一版”（`installed_other_version`），不给差异、不合并。
- **模块登记点**：enter 的首次建档与进入、checkin、wallet 消费、shop 购买、stamina 消耗、profile 升级、dungeon 首次领奖、leaderboard 成功提交、mailbox 附件和 task 奖励都已有实际联动。事务内钩子只能改同一 Player；排行提交钩子在事务外。登记名空/nil/重名在启动时拒绝，模块库静态目录校验不推断动态 init 名称。
- **事务内事件**：checkin / wallet / shop / stamina / profile / dungeon 的任务联动与原业务一起提交或回滚。钱包 Apply 与商店购买的回执重放不触发；未知事件不写任务。回调错误必须交还 Nest，不能吞错后提交半笔业务。
- **前置骨架不回退**：安装会先执行缺失的 `roost add dao` / `roost add access player`；之后模块安装失败时这两步的结果保留（它们是完整的工程状态）。
- **game-demo 接入登录链路的既定取舍**（2026-11-12；验证范围见[业务模块（roost module）](#业务模块roost-module)）：
  - **维护者 2026-11-12 接受了下面六项取舍**（“做的 6 个决定都接受”），按现状保留，不是待办：连错服有两个码、不再拒绝退役的计数键、demo 渠道经入口对外、告知在驻留表之外装载 Player、每条会话多一帧推送、发版后单独提最低 core 版本。
  - **会话激活后的告知在驻留表之外装载 Player**：进服票据握手、已经有档的玩家，`EnterStatus` 读“建成了没有”时经 lifecycle 装载他的档，不经 `PlayerOwners.Serve`（模块的 `enter_status.go` 不认识 demo 的驻留表）。它只对通过了进入前检查的会话发生，随后的 `EnterGame` 照常 `Serve`；握手之后不进入就断开的玩家，这份内存副本没有驻留记录，闲置卸载扫不到，到进程重启才释放。有界于“握手了却不进入的已有玩家数”，没有修。要修得让 enter 模块的告知认一个“装载之前”的登记点。
  - **连错服有两个码**：`EnterGame` / `EnterCreate` 答 demo 的 `player_elsewhere`（带 `owner_sid`），会话激活后的告知推的是登录模块的 `login_elsewhere`，随后断开。保留 `player_elsewhere` 是为了不改会话令牌那条老路的应答；统一成一个码要改客户端约定，等维护者决定。
  - **每条会话都多一帧推送**：会话令牌握手的会话（机器人缺省方式、`scripts/testdata` 里的演练客户端）现在也收到 `EnterStatus{need_create=false}`。读过代码的三个演练客户端（alliance-e2e、seamless、mmorpg crosssid）按关联号或消息号取应答，会跳过它；没有实跑确认。连错服的会话在收到告知 300 毫秒后被断开，原来可以一直连着重发 `EnterGame`。
  - **demo 渠道经入口对外**：`demo:<open_id>` 不是身份校验，不看配置、哪份配置下都开着；装了 portal 之后够得着 account 进程 `web.addr` 的人都能以任何账号登录。入口缺省只听回环地址，生产配置要显式写 `web.allow_public_addr`；对外之前必须换掉 `login_channels.go`。是否让 demo 渠道在生产配置下自动关闭，等维护者决定。
  - **不再拒绝退役的计数键**：demo 原来的玩家号分配器在旧固定键 `roost:demo:player_id` 存在时拒绝分配、在 `account.key_prefix=roost:demo` 时拒绝绑定（RV-20261014-01）；改用登录模块的计数器之后这两条没有了。新生成的工程没有那个键；从旧 demo 升级、Redis 里还留着旧键的部署会从 100001 重新发号。
  - **生成依赖没发布的包**：portal 的接线用 `wiring/modulehost`，已发布的 core（v1.44.0）里没有，所以 `-template game-demo` 现在要 `--skip-deps` 加本地 `replace` 才生成得出来；demo 的模块那一步因此与配方一样不在安装时跑 `go mod tidy`。下一次发版之后要把最低 core 版本提到那一版（打 tag 之后单独提交）。
  - **accountctl 的两处缺省变了**：`upsert-server` 的 `-sid` 必填（原来缺省 1000）、`-region` 缺省为空（原来是 `demo`）。仓库里的脚本都带 `-sid`。

#### 业务 HTTP 路由与配置检查

**业务 HTTP 路由（`wiring/web`）**：

- 传输层错误不带 `code`：解码失败 400 `{"ok":false,"error":"decode request"}`、请求体超限 413、panic 500 `{"ok":false,"error":"internal server error"}` 与业务错误的 `{ok, code, reason}` 形状不同；客户端先看 HTTP 状态，再按有无 `code` 区分。
- 映射成 5xx 的业务错误原文日志（`webroute: route failed with a server error`）只有 `request_id`、没有路由模板（生成的路由调用 `WriteResult(w, …)` 拿不到请求）；路由模板在同一 `request_id` 的 `web: request failed` Warn 里。
- 指标按 handler 写出的状态码计：handler 超过 `web.write_timeout` 才写完时客户端收到空回复，仍计为 handler 写的类别（时长直方图能看出超时）。
- Mod 不做鉴权，生成的 JSON 路由拿不到请求头（CODEGEN_REFERENCE §11）；安全边界沿用 ops 的“缺省回环、生产绑公网要显式允许”。业务路由与 ops 分开监听是可由维护者改的取舍。

**`roost config check` 与跨服务比较**（`--all` 与 doctor `config-across-services`）：

- 工程编译不过时只做键检查，明确打印 “cross-mod rules NOT checked”，退出码 5，不报 valid。
- 跨服务比较只按生成器认识的框架声明（`kitconfig_gen.go` 快照与生成的接入层声明），业务服务自己声明的键即使跨服务共享也不比较；值按写法比较，`gd.effect` 与 `gd.effect.` 这类运行时等价写法会报不一致（改成同一写法即可）。
- 端口只比较开发配置集（假定一台机器）；生产示例按每服务一个容器只比较共享资源名，同机多进程的生产部署要自己保证端口。`etcd.advertise_addr` 按 `unique` 要求开发配置集里各服务不同（维护者 2026-10-19 决定）。
- 端口来自 `roost.yaml` 的 `services.<name>.port_block`，块号上限 99，超过 100 个服务的工程要改端口段规则（[PROJECT_GENERATOR §5](../../codegen/docs/PROJECT_GENERATOR.zh-CN.md#5-wiring-mod-自动装配)）。
- `shared` 只表达“全部服务相同”，有意让不同服务用不同效果流 / saga 库的拆法会被报不一致，没有豁免开关。

### 框架服务与 game-demo

- **匹配队列状态**（维护者 2026-10-19 方案 A）：一个分区的全部状态整体读写；结束满 `match.retention`（缺省 10 分钟）的票、比赛与排队请求记录在下一次写入或 Sweep 时移除，重放与读历史只在保留期内有效（超期 Commit 重放报 `ErrTicketMissing`，不会成第二局）。状态上限 = 等待票（≤ `MaxQueueLength`）+ 保留期内的成局量；大队列按 `Partition` 分键是推荐用法，跨分区成局用 `CommitAcross`（键带 hash tag，一个逻辑队列的全部分区在 Redis Cluster 的同一节点）。规模建议见 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界)。2026-10-20 夜间容量测量（一次性夹具，本机 loopback 私有 Redis，[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §11）：每个分区的状态按“每秒成局人数 × 已过时间”增长（每名已成局玩家约 510 B），单次操作耗时随状态线性上升；4 分区各每秒成局 10 人、保留期 10 分钟时第 8 分钟（每分区约 2.6 MB）CAS 冲突比例到 0.94、请求超时，同一 Redis 上无关命令的 PING P99 到 0.6s——这一档**不可持续**，持续上限约为每分区“每秒成局人数 × 保留期”≤ 约 4500 人（状态约 2.3 MB），超过时调小 `match.retention` 或加分区（2026-10-21 维护者批准写进 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界) 规模建议，注明单样本、非目标负载）；Redis 被拖慢会影响同实例的其他服务。`CommitAcross` 与相邻分区匹配器抢同一批票时冲突约 49%（夹具有意的高竞争形态）。私有 3 主 3 从 Cluster 上两次杀掉该逻辑队列槽位的主节点：只有约 1～2 秒的连接错误，多键脚本没有部分生效，590 场比赛核对全部一致。（Cluster、跨机与异步复制见[框架服务与 game-demo：未测部分](#框架服务与-game-demo未测部分)）
- **匹配的弹性人数成局与补位**（2026-10-31）：弹性人数队列（`Queue.MinSize` / `FillAfterSeconds`，[guide/09 §4](../framework/guide/09-services.md#4-关键业务边界)）的等待按 Unix 秒取整，判定最多提前不到 1 秒；存储按自己的时钟判定，匹配器与存储所在进程的业务时钟不一致时，策略认为能开的不满员局可能被 `ErrGroupNotReady` 拒绝，下一轮再试。弹性字段只有新版本服务端认识：旧服务端会忽略它们、把弹性队列当固定人数队列（尚未上线，不做兼容，先升级匹配服务）。已开局房间的中途补位按业务层写法（维护者 2026-10-31 选方案 B，[派对手册 §6A](../framework/PARTY-ROOM-INTEGRATION.md#6a-补位业务层写法)）：房间票 + 玩家票在补位队列里弹性成局，**不超员靠房间的进房 handler 按座位拒绝，不是存储保证**（超员的补位局在匹配服务里照常成功，被拒的人要重新排队）；补位与普通成局的优先级由业务匹配循环决定，存储不跨队列检查同一玩家；跨分区补位不支持；心跳推进的补位截止没有回调通知宿主，撤票靠宿主周期性对账，期间过期的房间票留在队列里（匹配循环不再给它配人）。存储原生补位（方案 A：`OpenSlots` / `Backfill` / `CloseSlots`，空位在同一 CAS 里扣）未提供（[FEATURE-2026-10-31 第 3 节](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-31-match-flexible-grouping.md#3-第二阶段中途补位backfill待维护者选择)）。（Redis 与多进程匹配器见[框架服务与 game-demo：未测部分](#框架服务与-game-demo未测部分)）
- **game-demo 公会名**（维护者 2026-10-17 决定：业务逻辑，框架不处理）：`alliancerule.ValidateName` 只拒绝空、全空白、超过 48 字节、控制字符与非法 UTF-8（端点与 alliance 服务各判一次）；敏感词与全服重名 demo 不实现，两个公会可以同名。需要时敏感词加在 `ValidateName`，全服唯一参考 `service/directory`（先 Reserve 再创建、成功 Commit、失败 Cancel），见 [Service 篇 §4](../framework/guide/09-services.md#4-关键业务边界)。
- **game-demo 副本排行**：排行准入就是奖励准入（领奖事务内的赛季通关数 + `UpdateMax`）。通关数按赛季计（维护者 2026-10-18），改 `ranking.CurrentSeason` 后新榜从 0 开始；`UpdateMax` 同分保留较早的 tie，补报的那一分在同分时按补报时间排序。
- **game-demo 活动贡献**（维护者 2026-10-18 决定 A：只在奖励首次付款时贡献）：**已接受的代价**——付款之后、贡献之前进程崩溃或协调器调用失败，这一分永久丢失，重放不补（端点打 Warn）。协调器只在同一窗口内去重，业务在别处直接调用 Contribute 须自己保证每局一次。
- **全服邮件 `mail.LazyBroadcast`**（[DESIGN-TODO D17](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d17-全服邮件的内置投递策略)，用法见 [Service 篇 §1B](../framework/guide/09-services.md#1b-mail-全服邮件)）：生成骨架缺省仍为 nil（拒绝广播），启用要改 `Broadcast()` 一行。剩余边界：② **画像以最近一次 `PullBroadcasts` 为准**：入会 / 退会后业务不再拉一次，`List` / `Summary` 会按旧范围合并、水位越过新公会已发出的邮件（之后再拉也补不回）；在两次拉取之间入会、而公会邮件发于入会之前的，会被合并（需要“入会后才收”时把入会时间放进 `Attributes` 由 `Eligible` 判断）。③ **条件只判一次**：被内置条件或 `Eligible` 判为不满足的全局邮件，水位越过后不再重新判断（玩家之后满足了也收不到）。④ 没调用过 `PullBroadcasts` 的玩家（没有画像）`List` 不合并；game-demo 只放本服范围、不放公会范围。⑤ 撤回只挡未合并的，已合并的副本没有“强制收回”入口；撤回记录保留到过期。⑥ 内置条件按秒比较、严格早于：发送同一秒注册的角色收不到。⑦ 合并只在邮箱放得下时进行（全是未读的满邮箱等玩家腾位置），停住期间过期的全局邮件不会再补。（① 见[框架服务与 game-demo：未测部分](#框架服务与-game-demo未测部分)）
- **业务时区与每日重置**（DESIGN-TODO D18 已实施，维护者 2026-10-20 决定 A）：业务日按 `time.business_timezone`（缺省 UTC）/ `time.business_day_start`（缺省 00:00）算，见 [guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)。剩余限制：① 时区数据库靠 `main.go` 导入 `time/tzdata`；`main.go` 归应用、sync 不改，旧工程或手写 main 在没有 zoneinfo 的镜像里，启动检查会拒绝非 UTC 时区（报错点名 `time/tzdata`，不会静默退回 UTC），要自己加一行导入。② “看不到 zoneinfo”的回归在 macOS 上用 `sandbox-exec` 模拟；自带 zoneinfo、没有这种隔离的 Linux 主机上 Skip（只设 `ZONEINFO` 为空目录模拟不了：Go 照样回落到系统目录）。③ macOS 文件系统不区分大小写，`asia/shanghai` 在开发机上能通过检查，在 Linux 镜像里加载失败（启动时拒绝）。④ 周重置、按周 / 月划分的活动窗口没有框架辅助，由业务按 `Day` 推。（distroless 实测见[框架服务与 game-demo：未测部分](#框架服务与-game-demo未测部分)）
- **Session 运行服务**：保持现状、不改为可选（维护者 2026-10-14）；后续事项见 [ACCESS-SYNC-FOLLOWUPS](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/ACCESS-SYNC-FOLLOWUPS.md)。
- **观测接线**：ExportMetrics / admin 元数据不是自动外部监控或审批系统；部署实际接线与鉴权审计归部署方。

#### 合服（D15）

别名合服（[DESIGN-TODO D15](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d15-合服把一个服并入另一个服) 已实施，设计与实施记录 [FEATURE-2026-10-20-server-merge-alias](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-20-server-merge-alias.md)，runbook [PLAYER-MIGRATION §6](../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)）。当前边界：

- **持久总线消息只列清单、不检查积压**：`wiring/sidmerge` 不连 NATS。按 sid 寻址的持久用法（`syncbus.transport: jetstream` 的每 sid durable、`nats.rpc.transport: jetstream` 的实例 durable、可靠总线的死信）由运维按清单人工确认（检查项 `bus` 为 `confirm`）。框架不把发往 A 的消息重投到 B。
- **saga 终态检查是整个 saga 分区日志**：检查离线重放 `saga.stream` 的全部分区（协调器要先停，`InspectPartitions`）；saga 记录不带“属于哪个 sid”，同一套流上其他仍在运行的服的非终态 saga 也会让检查不通过；只有全集群停服窗口里才自然满足。维护者决定保持整个存储严格终态检查，真实多服共用存储部署出现需求后再考虑过滤（[DESIGN-TODO“保持现状的决定”](DESIGN-TODO.md#待维护者决定不急)第 8 项）。
- **“同一 Mongo”按 URI 比较**：主机列表（小写排序）+ 副本集名；同一集群换主机名或 IP 写会被判为不同而拒绝合服（偏保守，不会把两套 Mongo 当一套）。
- **跨记录的 account 写不是原子的**：每条服务器记录按版本条件写；写入顺序保证中途停下不会出现两跳，结束复核，并发合服造成的两跳报 `ErrServerMergeCycle` 并给修复命令（unmerge 再 merge）。拍平之后“X 是经由 A 并入的”这一历史不保留：`UnmergeServer` 只恢复一条记录，经 A 改指的其他服仍指向承载服。`MergedFrom` 是辅助索引，可能多出并发留下的项，是否并入以对方的 `MergedInto` 为准。
- **启动自检的范围**：`ServingGuardMod` 只在生成器装配的、承载玩家且接了 account 的服务里；手写装配要自己加。它排在 DataEngine 之前，但不排在 `syncbus`、`remote_entity` 等 Mod 之前：误启动的 A 在被拒之前这些 Mod 已经 Start 又随即停止（2026-10-20 演练：拒绝发生在启动后约 60ms）。account 不可达时 30 秒后拒绝启动（fail-closed），代价是 account 故障期间 game 起不来。
- **按 sid 独占数据的迁移不拷贝索引**：B 启动时按 DAO 声明建；不拷贝 `_dataengine_*` 控制集合。Remote 托管（`remote=managed`）落在按 sid 分库里的集合只在报告里点名，所有权元数据按 Remote 规则另行处理。回退不动合服期间 B 新产生的按 sid 独占文档（只计数），归属由业务判断。
- **源库保留为只读归档**：框架不加只读保护，靠启动自检与 runbook；删除由运维确认后手动做。
- **消费者**：Gate 转交给 game 的绑定不带 Claims，game 侧需要原服标识时从自己的角色数据取；`OriginServerIDClaim` 只在认证器那一侧可见（嵌入 TCP 的 Principal 里两者都有）。CreateRole 被拒经总线时客户端看到的是 `ErrServerClosed` 的通用原因，“已并入 B”的详情只在 account 进程的错误里。 game-demo 拉全服邮件（D17）时，原服并入本服的角色另带原服范围 `ServerScope(A)`，合服前发给 A、还没拉到的全服邮件照样属于他——这是 demo 的业务选择，框架不规定。
- 验证边界见[框架服务与 game-demo：未测部分](#框架服务与-game-demo未测部分)。

#### 单个玩家换服（rehome）：既定设计

2026-11-11 一期（[方案](FEATURE-2026-11-11-player-rehome.md)）。验证范围与未完成项见[单个玩家换服（模块 rehome 与 game-demo 接线）](#单个玩家换服模块-rehome-与-game-demo-接线)。

- **结果未知时无限重发**：提交、中止、撤屏障在 account 或实体不给结论时按退避（封顶 30 秒）一直重发，没有上限也没有停机钩子（后台 goroutine 随进程退出，靠重启后的向前恢复）。account 长时间不可用时，这名玩家哪个服都进不去——有意如此。
- **saga 准入可能发一次 account 调用**：本进程没有这名玩家的驻留记录时，属主核对要问 account 当前绑定；account 慢的时候步骤消费者会被拖住，account 不可用时离线玩家的步骤只能重投。
- **迁出表只在玩家换回来时删行**，随换走的玩家数增长，进程重启清空；“已迁走”只直接回答 30 秒，之后向 account 核对（否则 A→B→A 时两个服各说“在对方那里”）。
- **运维用 `accountctl` 直接发起的换服**没有记意图：目标服不做到达（不起冷却、不跑到达钩子）。事务外的到达钩子至多执行一次。
- **“离开中即拒绝、先排空再检查”的边界**（RV-20261111-BM-05 的通用边界仍需工程关注；R10 后联盟允许换服，不再以“禁止联盟成员迁服”举例）：
  - 排空等的是“这名玩家一条会话都不剩”，依据是嵌入 TCP 的读循环串行分发、会话在读循环退出后才摘掉。**推送失败时被传输提前摘掉的连接**（它的 handler 可能还没返回，context 已取消）与**独立 Gate 部署下经 Game Ingress 转来的请求**不在这个保证里，后者没有核对过。
  - 被断开取消的请求，它发给别的服务的调用可能已经生效。模块只保证离开钩子在这之后才执行；钩子要向那个服务问**包含在途操作**的事实（需要禁止某种关系迁移时，工程须自行定义该事实）。**请求已发出、还没到达那个服务、比钩子的询问晚到**的情形拦不住。
  - **跨服务关系不能靠一次读检查冻结**：别的操作者仍可能在检查后改状态。R10 的联盟不再禁止成员换服，改为步骤按当前绑定准入、进入时对账，并以入盟批次与旧 SID 条件修正 HomeSID。HomeSID 是尽力通知提示，非线性一致的 account 复制；快速往返换服时可暂时过时。工程若要求跨域关系完全冻结，仍须另做业务协议。
  - 工程有自己的写闸门（`access.player.write_gate`）而又用模块缺省的迁出表时，模块不抢闸门，只记一条 Warn：那个闸门要自己调 `rehome.AdmitMessage`，否则离开中的玩家新的请求不会被拒。
  - 排空的预算是 30 秒（一条请求的分发期限缺省 3 秒）；等不到就中止换服。受理到断开之间有 500 毫秒宽限，这期间他别的连接上的请求会被闸门关掉连接。
- **结论按当前绑定判定**（RV-20261111-BM-04）：驱动器不再从 Commit / Abort 的错误码推断某个换服 ID 的历史。account 的绑定对不上时（承载服是本服而绑定代数恰好比发起时大 1）不下结论、按退避一直重问，屏障与迁出记录保持。运维用 `accountctl` 把玩家换走时源服若正卡在“发起结果未知”，源服会按“已迁走”落定并补交实体。
- **搬档（v1.47.0）的前提与窗口**：Player 等本地实体的 DAO 改为 `dbscope=sid` 之后，换服在交出之后把玩家实体的 sid 文档复制到目标服的库、提交后删源库（[模块 README“搬档”](../../modules/rehome/README.md)）。
  - 前提：两个服把同一个逻辑库映射到同一个 Mongo、同一个前缀；不满足时目标服读不到搬过去的档、新建空档。靠规则文件的可去列表把关，没有框架检查。
  - 只搬玩家实体自己的 sid DAO；他在别的独立实体里的数据不搬。
  - 搬档之后、中止删目标副本之前崩溃：目标库留一份旧副本；下一次换去那个服时被覆盖（提交前的复制用 overwrite）。
  - 运维在模块之外用 `accountctl` 提交、源服又没有续做时，目标服没有他的档（新建空档）；续做时才补搬。
  - 崩溃窗口没有自动清理、验证范围见[单个玩家换服（模块 rehome 与 game-demo 接线）](#单个玩家换服模块-rehome-与-game-demo-接线)。

#### 单个角色换服（account 一侧）

单个玩家换服（[方案](FEATURE-2026-11-11-player-rehome.md)，一期实施中）里 account 的部分已实现（未发版）：`RoleBinding` / `BeginRoleTransfer` / `CommitRoleTransfer` / `AbortRoleTransfer`、认证器的两个新 claim、`accountctl show-role / transfer-role`，契约与 runbook 见 [PLAYER-MIGRATION §7](../framework/PLAYER-MIGRATION.md#7-单个玩家换服account-一侧)。当前边界：

- **与方案的出入**（实施时按不变量调整）：① 中止的写入顺序是“先写角色、再删占位”（方案是反过来）：按方案的顺序，并发的提交会在占位被删之后把角色改绑过去，留下没有 slot 的角色（用例 `TestConcurrentTransferRequestsHaveOneWinner` 在那个顺序下变红：`ValidateSession: account: conflict: role creation is pending reconciliation`）。② slot 另记所属的绑定代数（`Slot.BindEpoch`）、`LastTransfer` 另记 `Epoch` / `Note` / `Settled`：提交的收尾按代数认旧 slot，迟到的收尾不会删掉角色换回来的 slot；`Settled` 让没做完的收尾能被下一次 `SelectRole` / `Begin` 发现并补完。③ 中止一个没发起成功的 ID 也记为已中止（挡住迟到的 Begin）。④ 人工提交带备注的入口 `Admin.CommitRoleTransferWithNote`（方案的 `CommitRoleTransfer` 没有备注参数）。⑤ 另设换服尝试序号 `Role.TransferSeq`（发起带 `ExpectSeq`）与“占位以角色记录为准”的规则，见下面两条（审查 RV-20261111-BM-01 / BM-02 之后加的）。
- **“涉及已合服的服”按“已并入另一服”解释**：角色的绑定服或目标服的 `MergedInto` 非 0 才拒绝；承载着别的服的服照常可作源或目标。`MergeServer` 不检查在途的换服（没有索引）：换服期间目标服被并走，提交被拒绝，由调用方中止。
- **过期的 Begin 由 `TransferSeq` 挡住，ID 只认两个**（RV-20261111-BM-02 已修）：每受理一次发起、每记下一次中止（含中止没发起过的 ID），`Role.TransferSeq` 加 1，发起必须带当前值，所以中止之前发出的 Begin 无论隔了多少次发起与中止都不能被重放复活。按 ID 只认得出在途的那一次与最近了结的那一次（`LastTransfer` 只有一条），因此仍有两条边界：① 调用方拿一个早已了结的 ID、带着**当前**的代数与序号重新发起，会被当作新的换服受理（之后那个 ID 迟到的 Commit 会提交它）——换服 ID 仍要不重复；② 当前没有换服在途时，重发一个已被后来的了结覆盖掉的 ID 的 Abort，会被当作“中止一个没发起过的 ID”再记一次并返回成功，**即使那个 ID 当年是提交的**；调用方要判断“是否已提交”，看返回的绑定（`ServerID` / `BindEpoch`），不要只看 Abort 成没成。读绑定与发 Begin 之间有别的发起或中止落地时，发起方会得到 `ErrTransferStale`，重读再发。
- **收尾是尽力而为的后半段**：结果已定、slot 没收尾时，另一头的名额仍按已占用回答（`ErrRoleLimit`），直到重发的 Commit / Abort、这个角色的下一次 `SelectRole` 或 `Begin` 补完；`SelectRole` 里补不完只记 Warn 与 `role_transfer.settle_failed`，不挡登录。
- **没有人认的占位靠入口撤，没有后台清扫**（RV-20261111-BM-01 已修）：Begin / Commit 停在占位写入这一步时换服被中止，占位可能在中止收尾之后才落地。Begin 占完位会再读一次角色并撤掉自己留下的；进程在那之前退出的，由之后的入口撤——这个 ID 重发的 Abort、这个角色的 `SelectRole`、下一次受理的 Begin、Commit 补占位时、账号在那个服的 `CreateRole`。没有人再碰那个名额时占位一直在（不挡任何人，但 `show-role` 看不到它，只能直接读 slot）；`Admin.ResolvePendingCreation` 对它仍答 `ErrNotResolvable`、不撤。被拒绝的 Begin（例如重发一个已中止的 ID）不撤。代价：最近一次了结是中止的角色，每次 `SelectRole` 多读一次目标服的 slot，直到它下一次换服。判定规则（slot 在角色的绑定服上、或角色在途的换服就是它才作数；先读 slot 再读角色）依赖“占位只在角色写下 `Transfer` 之后才建”与“`LastTransfer` 被覆盖之前上一次的收尾已经做完”这两条写入顺序；改这两处要一起看 `releaseStrayHold`。
- **建角服不再可得**：`OriginServerIDClaim` 在换服之后是当前绑定服；业务要按“最初的服”做显示或归属，得自己在玩家数据里记。
- **物理库不同的服**：account 不知道两个服是否把玩家数据映射到同一个 Mongo 与前缀（v1.47.0 起搬档由 rehome 模块做，前提见上一节）；独立 Gate 转交给 game 的绑定不带 claims（两个新 claim 只在认证器那一侧可见）。
- 端到端、待确认的决定、清扫与验证边界见[单个角色换服（account 一侧）：遗留](#单个角色换服account-一侧遗留)。

### 地形与无缝世界

#### 地形副本订阅与地图参数（RV-20261110-01～04 修复后）

修复经过见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-11地形审查-rv-20261110-0104已修复未发版留下的边界)。

- **一路地形副本对同一个拥有者至多订 32768 块（`terrain.MaxSubscriptionChunks`）**（RV-20261110-01）：订阅是一条消息、整份替换拥有者记下的那一份，没有做带批次身份的分片订阅。超过时 `Replicator.Want` 整份拒绝（`ErrSubscriptionTooLarge`，计 `WantsRejected`），之前接受的订阅照常续期。上限按拥有者分别算；缺省一个格 48 × 48 = 2304 块，格边长超过 181 块（约 11.6km）时一个格才可能超。game-demo：Ghost 带本身超限时拒绝启动；运行中玩家视野的订阅被拒时保留上一份、只在被拒与恢复时各记一条日志，新进视野的别的格的块等不到拥有者的状态，客户端那几块停在未确认，视点再变时重试。（demo 端到端用例见[地形 / 无缝世界：遗留](#地形--无缝世界遗留)）
- **`Want` 在调用方的 goroutine 上分组，`OwnerOf` 必须能并发调用**（RV-20261110-01 的修法带来的契约变化）：原来只在 `Run` 的 goroutine 上调用。game-demo 的 `OwnerOf` 读只读的 `cellworld.Grid`，满足；业务自己的实现要是只读的分区表。
- **地图参数向外是副本，有区域水位时每次 `Meta()` 分配一份区域表**（RV-20261110-03）：`Pack.Meta` / `View.Meta` / demo `Map.Meta`，至多 1024 片 × 20 字节；循环里先取一次。C# `TerrainMeta.WaterRegions` 是只读视图（`ReadOnlyCollection`），不能再转回数组。
- **水深的类型变了**（RV-20261110-04）：Go `Ground.Depth` / `View.WaterDepth` 返回 int64，C# `TerrainGround.Depth` / `TerrainView.WaterDepth` 返回 long；把它存进 int32 / int 的调用方要显式转换（编译期报错，不会静默截断）。没有新增地图参数的约束，已有的地形包不受影响。（Unity / IL2CPP 见[客户端：遗留](#客户端遗留)）

#### 跨格编辑直接拒绝与确认来源

[地形方案 §30](FEATURE-2026-11-08-editable-terrain.md#30-p5-跨格编辑分区场景的建筑与游戏尺度的分区视野t082026-11-10)、[证据](evidence/terrain-p5-20261110/README.md)。

- **跨格笔刷与跨格占地直接拒绝（维护者 2026-11-10，[方案 §30.10](FEATURE-2026-11-08-editable-terrain.md#3010-跨格笔刷直接拒绝维护者-2026-11-10撤回-301-的拆分)）**：覆盖块归两个及以上地图进程的笔刷答 `terrain_cross_cell`（100070）、占地答 `building_cross_cell`（100069），都在预扣配额与发号之前，不做拆分、跨进程事务或部分成功。代价：格边上半径以内（笔刷至多 16m）不能一次刷过分界线，玩法层靠地形设计避开；客户端要能处理这个码（挪进一个格里再发，可用同一个 `Seq`）。（演练与 `Brush.ApplyIn` 的去留见[地形 / 无缝世界：遗留](#地形--无缝世界遗留)）

**地形确认来源（就绪标记，2026-11-10）**：可编辑地形 §31.7 ③ 按维护者定的 A 实施（[方案 §31.9](FEATURE-2026-11-08-editable-terrain.md#319-确认来源就绪标记317-③-a2026-11-10)；已合入 main，随 v1.43.0 发布）：地形视野里每一块都下发 TerrainChunk 对象，没改过的块是 Version 0 的就绪标记，C# `TerrainChunkSync` 据此 `Confirm` / `Unconfirm`。留下的边界：① 可确认的显示半径约 100m（一块就绪要 8 个邻块都已确认，地形视野 200m、视点每 8m 上报）；要更远需放大地形视野，300m 约 69 块，超过地形 64 的上限，要连同 `terrainMaxVisible` 与会话上限一起改——**待维护者确认**是否需要；② 进场时视野里 29～37 块按地形快照预算每 Flush 4 个，约 8～10 次 Flush（约 0.4～0.5s）后才全部可确认，期间显示原有网格或空洞。③④⑤ 见[地形 / 无缝世界：遗留](#地形--无缝世界遗留)。

#### 无缝大世界原型的部署要求

- **无缝大世界原型（2026-11-04 合入 main，[方案](FEATURE-2026-11-03-seamless-world.md) §14）**：`gameplay/cellworld` 与 game-demo 的分区场景（`scene.partition`，缺省关闭）是原型，不是承诺的正式能力。Cell 主题没有按主题的鉴权（连上 NATS 的进程都能往 Cell 发 Enter / 攻击）、结算方不复核攻击距离（信任攻击者所在格已按视野检查）：两者都只防内部进程出错或被冒充，按内网互信降为部署要求（维护者 2026-11-05：内网互信，不做按主题的 ACL）：NATS 只在内网可达，开连接级账号（`nats.username` / `password`），防止别的环境误连（方案 §6.3 第 8 条）。（原型的其余待办与待维护者决定的 Team 归属见[地形 / 无缝世界：遗留](#地形--无缝世界遗留)）

#### cellworld 的位置零头与入场序号

- **位置零头**（2026-11-10 按千分位累计零头之后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10cellworld-按经过时间积分的子步截断已证实维护者-2026-11-10-选定按千分位整数累计零头已修复未发版)）：每个子步仍在千分位上截断，丢不到 1 千分位（400/s、50ms 档平均约十万分之二点五）；游泳进出速度不同的地面格时缩放剩下的位移，再截断一次千分位。各情形的零头规则见 [cellworld README“位置零头”](../../gameplay/cellworld/README.md)，修复记录见 [证据](evidence/cellworld-subunit-20261110/README.md)、CHANGELOG `[Unreleased]`。
- **入场序号**（RV-20261106-03 修复后，原有、未变的边界，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-06-续审cellworld-生命周期与编码)）：发给冻结中旧格的 Leave 不处理、离场后才到的旧 Enter 会重建无人续租的角色，均由租约兜底；入场序号取 Enter 时系统时间，跨 Base 重启唯一性依赖时钟不回拨。

### 玩法组件与物理房间

#### 通用 LevelComponent 首版边界

`gameplay/level` 已验证通用阶段／规则、事务回滚、DAO 编解码与正式生成消费者；[Level 手册](../framework/LEVEL-COMPONENT.md)记录接线和准确语义。checkpoint／spawn_wave／settle 示例是能力替身，不提供物理重生、完整多人闯关或永久奖励；Registry 只是宿主能力类型表。私有参与者状态、网络增量与 Unity UI 由宿主接入。（未验证的部分见[LevelComponent 与 UGC 物理闯关：遗留](#levelcomponent-与-ugc-物理闯关遗留)）

#### UGC 物理闯关

UGC 数据是有限基础几何与轨迹描述，不含网格烘焙、脚本、编辑器、发布审核、CDN、运行期刚体增删、公开碰撞事件、检查点/通关/奖励或物理跨机接管。抓取按中心距离，无视线检查。近似客户端预测不保证移动平台摩擦或跨版本 PhysX 一致；输入邮箱不是逐命令帧同步。Room 为 RP2 私有确认 + RC2 字段增量（完整量化对象 34 字节）；Comparison/Crowd 对照仍为 RC1/70 字节。native ABI 2，先停旧再同步更新服务与客户端。同帧编码样本载荷降低约 51.7%，位置单轴量化误差约 5mm；不是 WAN 或容量验收。

物理 Room 默认启用接收者分频/字段裁剪，远处规则障碍可按固定关卡轨迹显示；完整对象仍为 34 字节，RP2 明确标记省略速度，RP1 客户端须同步更新。近处/抓取关联与快速接近对象保留高频；包围球用于分频保护，AOI 仍为中心距离，不是几何遮挡或全形状相交查询。降频引入样本年龄和插值延迟，不能用量化的 5mm 误差宣称实时显示误差。指标与同帧对照见 [Room 手册](../framework/PHYSICS-ROOM-SERVICE.md#按接收者分频字段裁剪与轨迹显示)。（未验收的部分见[LevelComponent 与 UGC 物理闯关：遗留](#levelcomponent-与-ugc-物理闯关遗留)）

### 客户端

#### Native 客户端（2026-11-01）

当前 Core TCP 协议的 PB、EntitySync、Lockstep 与时间域已提供 native 实现；通知/取消/重连与引擎接入见 [native 手册](../../client/cpp/README.md)。网络 ABI 2 不兼容旧插件，重新编译库和绑定。DNS 和账号取票由平台/业务异步准备，不在网络线程阻塞解析。（未测的平台与未实现的传输见[客户端：遗留](#客户端遗留)）

- **原生C++接入**：无完整 C++ / Unreal / GDExtension SDK 承诺，需独立设计和协议一致性验收。

### 验收与运维

- **Windows**：非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'` 和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异（打开中的文件不能改名 / 删除：nestwal 损坏记录检查、日志轮转、statslog；checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真；run 38500440362 的 6 个失败）。本机结果不改变 macOS / Linux 支持范围，跨平台也成立的缺陷不归入此条。
- **schema升级**：DAO 自动迁移已撤销，运行时只接受当前 schema；离线迁移按 [schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo 有执行器与 `cmd/schemamigrate`。在线双读与 `roost add migration` 骨架未实施，上线后重新决定；业务需明确离线转换和保留数据范围。2026-10-20 夜间在 v1.30 候选生成的 game-demo 上重做真实进程演练（[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §12），与 10-15 逐步一致。
- **Player跨机**：以已落库数据为准，不要求本地 WAL 热迁移；搬的是 sid，按 [参考流程](../framework/PLAYER-MIGRATION.md) 停写→排空 WAL→释放单实例锁→切路由→新进程空 WAL 接管，game-demo 有检查工具 `cmd/sidmove`。（换服、跨机演练见[平台与外部验证](#平台与外部验证)）

#### 压测工具的心跳驱动（D20 第 4 期）

sync-aoi `-move-driver=heartbeat`、nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat` 与 `gameplay/heartbeat/internal/bench` 的 §7 微基准（[HB-DRIVEN-LOAD-20261021](../performance/HB-DRIVEN-LOAD-20261021.md)，[DESIGN-TODO D20 P4](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#p4-实施结果2026-10-21)）。

- **缺省仍是全局循环**：两种工具不加开关时是原负载，已有报告的复跑命令不变；维护者决定保留 loop / ticker 历史负载口径为缺省，heartbeat 是明确选择的对照，报告必须写驱动（[DESIGN-TODO“保持现状的决定”](DESIGN-TODO.md#待维护者决定不急)第 9 项）。
- **心跳模式没有计划时刻**：sync-aoi 的 `Planned` 取这一拍消息冻结的业务时间、nest-game 的 `Completion` 从回调开始算，都不含驱动与快池的迟到（看 `beat_interval_ms` / `Beats.Interval` 与跳拍计数），与全局循环的“计划 → 客户端”不是同一口径。
- **sync-aoi 心跳模式的核对弱于全局循环**：位置取决于心跳时机，客户端只核对在实体的巡逻带上（全局循环逐项核对位置）；同一实体在一个 tick 里改两次时第二次不计时（照常核对）。
- **负载形状不同**：心跳模式的移动者是固定集合、持续巡逻（改值速率按与全局循环相同折算），全局循环每 tick 换一批实体；两者 AOI 进出的空间分布不同。
- **sync-aoi 端到端用例不判零跳拍**（2026-11-04）：`TestHeartbeatPatrolReachesClientsAndTheAOI` 曾在一次多 agent 并发的全量里以 `inflight 4` 失败。宿主把进程挂起超过一个 50ms 档位时，驱动恢复后逐格补走，每个登记的上一拍还在途，到期的拍按设计跳过（每个登记至多一拍在途、迟到不补投）；用 SIGSTOP / SIGCONT 把测试进程冻结 200ms 可确定复现（修前 10/10 失败，`inflight` 23～25、投递 = 执行），这是调度迟到，不是心跳或 AOI 缺陷。用例改为核对与调度无关的计数（投递的每一拍都执行，或在执行侧按停止 / 重复丢弃；没有准入失败、卡住与动作失败），测量窗口按“每个移动者在窗口里写过两次位置”结束（至多 200 个 tick）；冻结 200ms 时修后 10/10 通过。零跳拍仍是正式压测（`main` 的 `verdict`）的门槛；用例因此不再能发现“负载下跳拍变多”这类性能退化，那由正式压测负责。
- 正式测量的范围与没有透传的开关见[压测工具的心跳驱动：遗留](#压测工具的心跳驱动遗留)。

## 二、遗留问题

### Nest、App 与实体定时器（遗留）

#### 按实体的持久定时器：遗留

- **game-demo 手写驱动在 `Service.Init` 才接上业务时钟**：生成工程的 Mod 在 Provide 阶段绑时钟；demo 的 `startWorldTimers` 在 `Service.Init`、`EnsureWorld` 之前调用，“World 第一次加载在驱动接上之后”靠 demo 自己的启动顺序（写闸门 `require_write_gate: true` 在 `EnsureWorld` 之后才发布），不是框架保证；更早加载的 World 按系统时钟记加载时刻（`time.logic_offset` 为 0 时两者相同）。（分类待确认：这是 demo 自己的启动顺序，不是框架保证）
- **端到端用例用的是内存权威**：`TestEntityTimerGeneratedProjectEndToEnd` 以不落库 committer + 内存 loader 代替 DataEngine + Mongo；循环的其余计时分支（MaxWait 重读时钟、准入失败重试、Redeliver）由它与 entitytimer 包测试覆盖。真实 WAL / Mongo 下的定时器往返由 7 个游戏夹具的 SIGKILL 崩溃恢复与 MMORPG 的正常停机夹具覆盖，全部经驱动投递循环（`Driver.Start`）、不手发 Tick，见 [游戏接入验收](#游戏接入验收)。

#### 实体心跳：遗留

- **性能测量只覆盖驱动与压测夹具**：心跳的投递与驱动（[NEST-BATCH-HEARTBEAT §3G](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3g-2026-10-22-晚参数补测结果v1310-发版前压测第-1-项)、[HB-DRIVEN-LOAD §4A](../performance/HB-DRIVEN-LOAD-20261021.md#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)，2026-10-22 晚）有正式对比；移动、行为、技能 / Buff 本身只有功能用例与冒烟（3000 实体移动、5 万实体投递）。心跳数据除 sync-aoi 的一组 200ms 外都按 100ms 档测，而移动缺省档位已改为 200ms（[O1](../release/v1.31.0-NOTES.md#o1)，见下文“缺省 5Hz 的位置粒度”）；“无观察者时暂停 AI”是按需求触发的候选（[DESIGN-TODO](DESIGN-TODO.md#优化项下一阶段)）。
- **批量作业参数的未测部分**（[实体心跳（D20 第 1 期）](#实体心跳d20-第-1-期)“心跳缺省走 Nest 批量作业”的边界）：① 组 4 / 100µs 是同时段测过的组合（组 4 / 8 × 100 / 200µs）里 p99 最低的，更小的组或时间片没有测；② 全部数据来自 macOS / arm64，Linux 未复测，宿主在测量中途换过一次“时段”（原因尚未确认）；③ 真实移动回调（5～20µs、带事务）与热点实体下的业务延迟与冲突转单目标率没有测。
- **热点实体下的冲突转单目标率**：热点实体（同一实体高频业务消息）下的转换率没有测。
- **独占模式 Remote 心跳的真实依赖故障注入**：独占模式下不经共享的直接迁移只有模拟 Remote 写准入的用例（`heartbeat/remote_test.go`），真实依赖下的故障注入（Redis / Mongo 断开时切换）没有做。

#### 移动与动作、行为与技能：遗留

- **移动的每拍分配**：每拍的分配（`Move` 与 `MapBasic` 的回滚闭包、提交后记录、`MemFields` 的逆操作）未量。
- **事务内门控的进出场景**：事务内门控的进出场景（`QueueEnter` / `QueueLeave`）未做（[重构方案 §8 第 6 项](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/REFACTOR-2026-10-21-interest-single-writer.md#8-待拍板均先按推荐实施)）。
- **`state` 的同步与接线**：同步打包还没有接 `state`（game-demo 只用不持久的派生状态，经观察者镜像进 DAO 下发）；生成器只生成 Catalog 声明（`--state`），`Set` / `Gate` 与 `StateDao` 的接线由业务写。
- **`Pending` 的“活动施法”一项没有强制点证据**：测试用的程序在施法活动期间总有排程任务，去掉这一项的变异没有变红；保留是保守的。
- **没有迁移现有的 Lockstep 房间与 `scripts/testdata` 夹具**：维护者 2026-10-21 只要求示例与测试（`skill.ExampleStepper_fixedStepLoop`、`TestStepperOnAFixedStepLoopMatchesTheJitteredDriver`、`TestBuffsOnAFixedStepLoop`）；`framework/sync` 的 Lockstep 房间与塔防 / 自走棋夹具的 `Advance(now)` 仍按自己的规则推进，没有用 `Stepper` / `DriveBuffs`。
- **Skill Runtime**：Host 能力、重入、阻塞、恢复和回放测试仍待补。
- **MOBA 路由档在 D22-1 之后没有实跑**：`ROOST_MOBA_ROUTING=1`（要隔离 Mongo 与 NATS）用 `clock.SetSource` 控制时间，按设计仍应成立，没有在真实依赖上跑（2026-11-02 Linux 续报也未启用）。`ROOST_SLG_REAL=1` 2026-11-02 在 Linux 上被过时的 WAL 计数断言挡住，夹具修复后在 macOS 私有环境通过（[核心执行边界审查 RV-20261102-05](CORE-REVIEW.md#rv-20261102-05--p2slg-恢复夹具遗漏创建提交)），修后未在 Linux 复跑。其余真实模式 2026-10-31 在共享隔离 Mongo 上实跑通过：七个玩法（auto-chess、card-rpg、idle-sim、merge-casual、mmorpg、survival-extraction、tower-defense，业务时间改为从 fctx 取之后，未投影记录数同时跟上 R15“新建实体本身就落库”）与 MOBA 的三条 Mongo 用例。

#### 压测工具的心跳驱动：遗留

- **正式测量（2026-10-22 晚，[HB-DRIVEN-LOAD §4A](../performance/HB-DRIVEN-LOAD-20261021.md#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)）只覆盖 macOS 单机、每格 2～3 次（nest-game 每档 1 次）**：心跳模式零跳拍、投递 = 执行；心跳驱动的移动者是固定集合、持续巡逻，与全局循环“每 tick 换一批”的空间分布不同，多视野下的差距（16 视野 P99 70～84 对 116～137ms）不能全归给投递方式；nest-game 心跳驱动下普通业务消息服务端 P99 0.87 → 2.13ms（批量作业占 worker）。ticker 档 21:40:57 一次约 0.2 秒的整体停顿原因尚未确认（同时刻宿主 GUI 活动）。
- **心跳模式的投递方式跟驱动缺省走、没有开关**：工具构造驱动时不设 `Delivery`，维护者 2026-10-22 决定 B 之后即批量作业（之前是单目标 `Dispatch`）；`heartbeat.delivery` 仍没有透传（[DESIGN-TODO D20 P4](https://github.com/tjbdwanghaibo/roost-core/blob/3fddb51ea19dcd66451fd0ce8c80dcdf6b04be45/docs/maintenance/DESIGN-TODO.md#p4-实施结果2026-10-21) 第 5 条），要对比两种方式需在工具里加开关。

### DataEngine、Remote 与 Saga（遗留）

#### 起服预加载与按服分库：遗留

设计上的边界见[起服预加载与按服分库（v1.47.0）](#起服预加载与按服分库v1470)；`-template game` 的 Player DAO 见[生成器与工具：遗留](#生成器与工具遗留)，game-demo 地形 DAO 见[地形 / 无缝世界：遗留](#地形--无缝世界遗留)。

- **预加载没有数量上限与时间预算**：每页 1000 条、每批 16 个并发，用进程的基础 context；Mongo 卡住时起服一直等到进程被停，实体很多时起服时间与内存随之增长。要加预算与超时。
- **换服崩溃窗口没有自动清理**：换服提交后、删原服的库之前崩溃，原服重启会把旧档预加载进内存；续做时目标服补搬冲突，旧档留给运维删（见 [rehome 模块 README](../../modules/rehome/README.md)）。与 rehome 的“搬档”是同一个窗口，见[单个玩家换服（模块 rehome 与 game-demo 接线）](#单个玩家换服模块-rehome-与-game-demo-接线)。
- **夹具与玩法脚本没有重跑**：`add dao --entity` 的骨架缺省改成 `dbscope=sid`，各玩法脚本（`scripts/test-{slg,mmorpg,card-rpg,auto-chess,idle-sim}-generated.sh`、`scripts/test-genres-generated.py`）与 genre 参考文档里经它生成的 DAO 随之变化；mmorpg 跨服演练改为按玩家的 sid 读 `<db>_<sid>`，各玩法真实依赖用例（`ROOST_*_REAL`）的收尾多删一个 `<库>_0`。合并后的全套与真实依赖用例跑过之前，是否还有别处直接读共用库**尚未确认**。
- **game-demo 的空闲卸载不认识常驻**：`PlayerOwners` 的清扫如果遇到标了 `lifetime=resident` 的 Player，每轮记 WARN，且卸载前已让玩家离开场景，结果是实体留在内存、人已不在场景。demo 的 Player 缺省不是常驻，未改；要常驻 Player 时清扫要先跳过常驻实体。
- **常驻的刷新与载回只有替身存储的用例**：强制刷新、换服中止后的载回用 mongotest 验证，没有真实 Mongo 副本集用例；真实 DataEngine 下 rehome 走到常驻实体的交出与载回未演练。

#### Remote：遗留

- **Remote 托管 kind 的“新建不改字段”**：没有“新建不改字段”的 Remote 端到端用例。
- **交出实体的验证范围**：正式 Nest + 文件 WAL + Mongo 替身的用例（含 `-race`、并发写入、停机、订阅者重载），真实副本集上的现状刻画与整条流程各一条（本机隔离环境）。没有跑两个真实进程的换服演练（每个阶段后 kill -9），那是换服二期的内容。
- **多写者本地副本**：两个真实进程交替写同一共享实体的真实依赖用例没有加。
- **路由登记的长时运行与大联盟**：[Remote](#remote) 路由登记条的 ① “长时间运行后的守卫加载量没有测”、⑥ “大联盟的文档与内存没有测”。
- **空认领不清除**（[Remote](#remote) 空认领条的 ②）：每个占一条 `_remote_entity_meta`，按 owner 枚举会列出它、`alliance.transfer` 会把它转走（转过去仍是空认领）；对它的请求每次多一次所有权点读与一次聚合冷加载（不登记路由，调用方回退读所有权记录）。被拒的建盟多时这些会累积，要清理需要新的“删空认领”入口（带所有权代际守护），未做。
- **单实例 128 / 256 并发写偶发“全体同时约 3 s 超时”**（发布侧两处框架缺陷已修复，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-0406实体服务-rpc-按方法串行与跨实例写超时)；剩下的尚未处理，不是“非问题”）：修后 8 轮仍有 20 个全体超时簇（修前 8 轮 19 个；本机当时 load 11～57、swap 几乎用满，噪声大于差异），剩下的停顿全部在 I/O——投影器的 Mongo 事务与 outbox 的 Mongo 标记等多数派写关注（三个副本集节点同机，从节点应用 oplog 卡 1～2 s 时主节点的写一起等）、WAL `fsyncSegment`、投影器主循环里同步做的 checkpoint 落盘（`nestwal.storeCheckpoint` 的 rename / fsync 在 `Projector.ReplayPass` 里执行，磁盘一卡投影全停）。strict 写要等持久与发布，这类停顿超过请求期限时全部在途写仍会一起超时；是否把 checkpoint 落盘移出投影器主循环、在独立机器 / 安静机器上这类停顿多大，都未评估。下次复现时抓：探针的停顿栈（看 `Projector).run` 栈顶、`fsyncSegment`、`MarkRemoteCommitPublished`、`ApplyRemoteCommitsInTransaction` 各多少）、`remote_entity_outbox_detached_total` 与 `nestwal_fsync_duration_max`、`remote_entity_outbox_mark_published_max` 的差值、mongod 慢日志（主节点带写关注的慢操作、从节点的慢 `Applied op`）、`uptime` 与 `sysctl vm.swapusage`。
- **单实例分散 `Join` 的目标 1000 次/秒（维护者 2026-11-05）：本机约 340～400 次/秒封顶在 Mongo，未达到**（[benchmarks/alliance-join-1000-20261105](../performance/benchmarks/alliance-join-1000-20261105/README.md)）。并发逐级加到 16 / 64 / 128 / 256：16 并发约 80～90 次/秒是压测并发不够；64 并发起依次被三个框架缺省值封顶——Nest I/O 池 40 个 worker（strict Remote 写从准备到确认都占一个，128 并发起 I/O 队列满、八成请求被拒且答成 `CodeInternal`）、投影 8 通道（单条 Mongo 事务约 50 ms）、Remote 发布 8 个并发（约 330 次/秒时忙 86%）。已改：I/O 池缺省 max(256, 4×短池)、`dataengine.projection.workers` 缺省 32、`remote_entity.outbox_publish_workers` 缺省 32、Nest 派发队列满答“可重试”、DataEngine effect outbox 一批认领 / 确认常数次往返（见 CHANGELOG `[Unreleased]`）。同机对照：64 并发约 150～180 → 236～264 次/秒；128 并发从八成失败到 0 失败、约 340～377；256 并发（调用方对“可重试”退避重试）194（11% 最终失败）→ 398（0 失败）。再放开写额度、I/O 池都不再上升：每次 `Join` 约 22～25 条 Mongo 命令、约 1.15 个提交的事务，直接压同一副本集的对照（单次 majority 写约 8 ms，事务 128 并发以上下降）与之吻合。剩余：
  - **本机到不了 1000/s**（差约 2.5～3 倍，Mongo 三成员同盘、缓存 0.5 GB、机器有其他负载）；生产硬件上需要主节点约 2.2～2.5 万条命令/秒、约 1200 个事务/秒，**尚未在目标硬件上实测**。
  - **effect 消费者在一个进程里逐条处理，约 50 条/秒：已改为进程内按实体并发**（维护者 2026-11-05 决定 A，契约与仓内消费者核对见 [impl/03 effect 消费者进程内并发](../framework/impl/03-dataengine.md#effect-消费者进程内并发)）。`SubscribeEffects` 缺省 16 个 handler 并发（`dataengine.effect_consumer.workers` / `EffectConsumer.Workers`），同一信封 Key 按到达顺序、不同 Key 并发，在途不超过 `MaxAckPending`（满了背压）。同机前后对照（128 / 256 并发重试探针，[基准页“effect 消费者并发”](../performance/benchmarks/alliance-join-1000-20261105/README.md#effect-消费者并发2026-11-05-晚)）：压测期间消费 36～43 → 227～300 条/秒，跟上暂存（JetStream 积压峰值 2.1～2.9 万条 → 不到 160 条，结束时 0）；修前无新写入时也只有约 102～108 条/秒。剩余：`Join` 吞吐在本机负载噪声（load average 10～72）内看不出变化；修后 effect 链路与 RPC 同时占 Mongo（修前推迟成积压），256 并发“重试 50 次仍可重试”的最终失败修前 28～278、修后 103～440 次，“修后代码 + 修前缺省”与修前背靠背两对在噪声内，**对 RPC 的影响要在安静的机器上再比（未确认）**；消费者自身的上限这一轮没有测到（写入量先到顶）。
  - **每次 `Join` 的 Mongo 命令数：联盟去掉可靠 effect 与守卫快照后少约 7 条**（维护者 2026-11-05，[方案 §2.5A](FEATURE-2026-11-04-alliance-service.md#25a-守卫是名单的缓存惰性自愈2026-11-05)；同机修前 `86658aa3` / 修后交替 3 轮，profiler，[基准页](../performance/benchmarks/alliance-join-1000-20261105/README.md#effect-持久性与守卫自愈2026-11-05-晚)）：不计 outbox 后台轮询，同实例新玩家 `Join` 27.2～27.3 → 20.2、`Leave` 23.0～23.1 → 16.2～16.3、守卫已存在 `Join` 23.0～23.1 → 16.2～16.3，跨实例 `Join` 50.8～51.8 → 40.1～40.2、`Leave` 32.8～35.3 → 26.9～27.9；少掉的是 effect outbox、消费方 inbox 与守卫快照。重试版吞吐 128 并发新玩家 `Join` 229～270 → 272～353、256 并发 197～246 → 259～299 次/秒（区间部分交叠，本机负载噪声内方向确定、幅度不精确）。仍到不了 1000/s。
  - effect outbox `dataengine.outbox.workers` 缺省已由 2 改为 8（维护者 2026-11-05 决定 B，2026-11-06 定为正式缺省 8，不再是试用值）。修后代码配 2 个 worker 时压测结束 outbox 积压 1.5～1.9 千条、配 8 个时不到 100 条，`Join` 吞吐在本机噪声内看不出差别。
  - 写额度 `remote_entity.max_concurrent_writes` 128 在本机 256 并发时答“可重试”，放到 512 不提高吞吐；改到 512 后 256 并发仍有大量“可重试”答复，来源**尚未确认**。

#### Saga：遗留

- **路由步骤退回的重派**（待确认）：补偿方向与没有截止的 saga 上，退回会一直重派（待确认，见方案 §7 第 2 条）。
- **上线前缺口**（[上线前 Saga 审查](evidence/known-limits-history-20261113/README.md#上线前-saga-审查2026-10-28--10-30已修复)）：该审查列的上线前缺口中，目标 Linux 拓扑的资产守恒、准入限流下的持续负载与追平仍未验证（见 [Saga](#saga) 性能条），上线后的兼容政策未定（见[维护手册 §2](README.md#2-升级与回退)）；当前状态逐条见提交审查 2026-10-30 节。
- **正式实现的性能只在 macOS 单机测过（Mongo 在本机或云上）**（2026-10-29，[验收报告](../performance/SAGA-JETSTREAM-AUTHORITY-20261029.md)，设计 §12 判据全部达标）：生产推荐配置（原生步骤 pipelined + Nest 异步完成、short 池 4）原生正向 c128 约 1050 saga/s、c1 P50 23.6ms，正确性门槛全 0；单样本为主，同配置跨时段差约 25%。**Linux、跨机的正式实现压测都没有跑**（原型的云 Mongo 数字不代表正式实现）。参与方是框架层正式路径（Nest + WAL + DataEngine 投影 + 实体内回执 + `SubscribeNativeStep`），账户 DAO 是同形手写，不是 `roost add` 生成物，`wiring/saga` 的 `Steps` 胶水不在压测路径上。旧 Redis / 旧原生路径的容量结论不再适用。
  - 云 Mongo 只有 2026-10-29 的三档单样本（[复现报告](../performance/SAGA-CLOUD-C128-REPRO-20261029.md)，RTT 约 9ms）：原生正向 c128 2084 saga/s（@strict64 1904）、c64 1292 saga/s，正确性全 0。
  - 参与方投影器约 1.5～1.8k 条/s，低于 c128 的准入速率（约 4k 条/s），停止加压后追平 c128 45.9s、@strict64 39.8s，**超过设计 §12 的 30s 门槛**（c64 14.6s）。原生正向档不开准入，持续高于投影速率的负载要靠准入限流。
  - 原型第三轮云 c128 的余额 false 是否与“卡住的笔不进预期”同源，**尚未确认**（逐账户证据缺失，原型代码已删除）。积压一处已解释。
- **托管 Mongo 的 `getParameter`**：托管 Mongo 是否允许 `getParameter` **尚未确认**（手头没有托管实例，拒绝时探针只记“未探测”）。
- **Saga 专项只在 macOS 上验证**：2026-11-02 Linux 续报只覆盖生成 Saga 用例（原生步骤 + effect 消费者）与真实依赖总入口里的功能用例（[报告](LINUX-FUNCTIONAL-20261102.md)），`kill -9` 恢复与性能是否在 Linux 跑过尚未逐项核对。

### Sync、玩家接入与 Lockstep（遗留）

#### 同步：尚未确认的偶发与疑点

- **2026-11-13：game-demo 主视野超限用例在重负载下失败一次（尚未确认）**：`TestTheGameScaleMainViewHoldsTheNearest128/over_the_session_limit_of_164`（game-demo 的 `internal/service/game/scene_view_scale_test.go`）在全仓 `go test ./...` 的 `TestClientSDKAgainstGeneratedPlayerTCP`（生成 game-demo 并跑它的测试）里失败一次，原文 `scene_view_scale_test.go:138: over the limit: 0 sessions lost, 0 waiting, 0 ever deferred (want 0, 0 and the crowd overflowing)`；当时同机并行跑着模块门禁与 17 个玩法纵切脚本。之后单独重跑 `TestClientSDKAgainstGeneratedPlayerTCP` 通过、生成的 game-demo 里该用例 `-count=5` 通过、独立生成的 game-demo `go test ./...` 通过。发生在“生成工程只带用到的托管服务”那一轮（2026-11-13），但那一轮的 game-demo 生成物除 `docs/ROOST_YAML.zh-CN.md` 一段说明外与改动前逐字相同，判断与该改动无关；“超限从未延后”在什么时序下出现没有定位，用例断言没有放宽。
- **建筑进主视野后偶发频率变高的原因**（根因已修复，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-10建筑进场景主视野后的-demo-尺度维护者-2026-11-10主视野已按推荐放大见地形方案-28偶发已定位修复见本条末段)）：建筑进主视野之后频率变高的具体原因尚未确认（只影响时序窗口的宽窄，不影响根因结论）。
- **类型调度的 demo 性能验收**：game-demo 生产 Scene 已改成 on_change/地形 1 秒；历史功能/测量夹具仍保留原周期构造器，本批未跑新性能验收。
- **`BenchmarkAOIBatchRecompute/dense=false/coverage=false/workers=4` 慢 7～11%，原因尚未确认**：三次同机交替对照（count 6）都显著，约 9µs 一档；同一路径 workers=1 与其余七个组合无差异，AOI 观察者结构已收回原大小，改动在这条路径上只多两次分支判断。可能是 4 worker 小批量下的调度或代码布局噪声，没有定位到代码原因（[证据](evidence/sync-overcap-degrade-20261108/README.md#基准对照同机交替)）。

#### 自身视图、嵌入 TCP 与 AOI：未测部分

以下是[自身视图](#自身视图self_sync与-sync-慢消费者规则)、[嵌入 TCP 出站](#嵌入-tcp-出站)与[广播与 AOI](#广播与-aoi)里没测、没做的部分。

6. 不提供 C# 的 subject 载荷解码样例。
7. 嵌入 TCP + self_sync 重档 15 分钟（2026-10-20 夜间，[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §2）通过：1000 玩家每秒约 1000 帧自身视图，`push_refused` retried / closed 0、滞留玩家 0，提交→客户端 P99 1.43ms。第 4 条的整轮重试放大在该负载下没有被触发，**没有测到不等于不存在**（要测需把预算压到触发点）；独立 Gate + self_sync 的重档夹具没有。

- 缺省 256 MiB 是按 Gate 缺省 128 MiB / 4096 连接折算到 10000 连接取整。2026-10-20 夜间嵌入 TCP 重档 15 分钟（1000 连接、场景 Sync 4198 万条 delta、每秒 1 万条回包，[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §2）`player_tcp_outbound_budget_exhausted_total` 增量 0、溢出 / 推送关连接 / 写错误全 0，正常负载下不触发；停读客户端或更高推送量下是否够用仍未测（没有驻留 Gauge，峰值取不到）。上面“驻留字节 Gauge / 按帧龄关闭”的决定所等的数据就是这一份：本负载下拒绝为 0，不构成要做的证据。
- 证据只有包内回归（含关连接与写协程并发退出时恰好归还一次）、自身视图与 Lockstep 的真实回环 TCP 集成用例与生成 demo 场景用例。
- **Interest 单写者 ⑧**：⑧ 性能：重构前后对比见 [INTEREST-SINGLE-WRITER-20261022](../performance/INTEREST-SINGLE-WRITER-20261022.md)（ApplySync 0～8 视野 P50 / P99 降低，16 视野诊断构建仍超周期）；预算计入结算与纯前缀切开之后的对照 2026-10-22 晚已测（[§7.1](../performance/INTEREST-SINGLE-WRITER-20261022.md#71-结果2026-10-22-晚v1310-发版前压测第-2-项)）：16 视野诊断构建政策阶段每 tick P95 约 27～28ms（预算 25ms），但单次最大仍 51～57ms，Flush 均值仍约 43ms（打包不在预算内），帧数与 Flush 次数满额；0 / 8 视野不退化。`log_backlog` 只在运行末读过（为 0），运行中的积压没有采样。
- **AOI 视野的大联盟**：真正的大联盟（上千人互相可见、百万级 pair）没有压测。

#### Lockstep：遗留

- **Lockstep 深缓冲链路子例（2026-11-07 本机实验与夹具修正，Linux 未复跑）**：Linux 上 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive/deep-buffered_link/written_lane_follows_the_link` 偶发 `closed=true`（约 67 秒，五轮一败）。本机实验（[记录](evidence/lockstep-deep-link-20261107/README.md)）：链路均匀变慢到 128 KiB/s 仍通过，只有中转那一跳完全停住 10 秒以上才失败（测试里 `write_timeout` / `CatchupSendTimeout` 都是 10 秒，按设计放弃）。Linux 回环上，夹具中转那一跳 16 KiB 的接收窗口小于回环 MSS，持续定时器退避会造成这种长停顿，真实网络不会；同文件直连路径早已因同一原因用 256 KiB。已把这一跳改成与直连一致（`deepLinkReadBuffer = slowReadBuffer`），只准入负对照照旧溢出；用例失败时打出 Gate 关连接计数增量与链路各跳进度。**判定为夹具问题，不是框架缺陷；修正后尚未在 Linux 复跑，复跑通过前不关闭。**
- **最新 Linux 复验（2026-11-03，v1.40.1）**：NATS 关闭表与 SLG 恢复夹具原问题已在 Linux 复验通过；etcd 停止屏障相关两用例各五轮通过。Lockstep 慢快照的深缓冲链路仍会断开，完整包及独立重复运行均有失败，不能关闭或据定向回归宣布上线验收完成。详见 [复验报告](FOLLOWUP-REVIEW-20261102.md)；下方历史结论不覆盖本节。
- **TCP 快照用例的平台假设**（2026-10-31 修夹具）：`TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive` 在 Linux 稳定失败的原因在夹具：Linux 回环发送缓冲在连接建立时就放大到 MiB 级，只准入对照的 4 MiB 快照被内核吞下、不溢出；服务器“已写出”领先客户端“已收齐”几 MiB，而 16 KiB 接收缓冲小于回环 MSS，交付退化到每秒百 KiB 级，5 秒收齐预算不够。没有发现框架丢快照。现在快照大小由链路上界推出（发送缓冲上限：Linux 读 `tcp_wmem` 第三项，其余平台按 macOS `autosndbufmax` 缺省 4 MiB 计；加沿途接收缓冲与中转深度），停读结束时直接断言追帧仍未完成；服务器写完与客户端收齐分开观察，后者按客户端是否仍在推进等待（10 秒无新字节判卡住，总上限 2 分钟）；另加“深缓冲链路”子用例（测试中转，3 MiB 深、4 MiB/s），在任何平台复现 Linux 的条件。剩余假设：非 Linux 平台的发送缓冲上限按 4 MiB 计，机器上调大时用例以“规模不足以区分”失败、不会误通过；修后夹具 2026-11-02 在 Linux 全仓运行中又在深缓冲子场景约 90 秒处失败（两个会话 `session not found`），原因是夹具只收下行、没有维持入站活性，撞上 90 秒空闲期限；夹具已加客户端心跳并收紧期限（[RV-20261102-06](CORE-REVIEW.md#rv-20261102-06--p2快照夹具没有维持入站活性)），修后只在 macOS 验证，尚未在 Linux 复跑，Linux 全仓为什么把该场景拖到 90 秒尚未确认。
- **历史窗口的取值**：按业务帧率、断线时长与快照大小核对窗口和预算未做。
- **独立 Gate 慢链路**：具体是 5s `write_timeout` 还是单帧发送预算先到期**尚未确认**（按代码缺省配置下单帧预算 ≤ `outbound.send_timeout` 250ms 更早，日志未区分）。纯追帧页在独立 Gate 慢链路上的影响未实测。
- **快照恢复未测、未做的部分**：① MOBA 持久对局 `Room` 已接快照来源（2026-10-20，`RecoverRoomWithSnapshots` + 已提交门 `MatchConfig.CommittedFrame`，[MOBA §5.7](../framework/MOBA-INTEGRATION.md#57-重连宿主代际与交接)），替身权威下有 KCP 与 Nest 两条回归，真实 strict WAL / Mongo 下有 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复的演练（`TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL`，下行是记录器、不经 KCP）；真实 WAL 的提交延迟分布、宿主交接与快照恢复同时发生、宿主与权威分属不同进程时的快照恢复未测； ② C# 的 `SHA256.Create` 与 restore 路径只在本机 .NET 10 测试过，Unity 编辑器 / IL2CPP 未实测；⑦ 真实移动网络、跨机与目标负载下的快照恢复、快照大小分布与窗口取值未测；⑧ 客户端上传快照（多数一致）、增量 / 压缩快照、检查点持久化与宿主崩溃续赛未做（设计 §13）；game-demo 战斗未接（§12 第 13 条）。
- **高负载下两个偶发失败，尚未确认**（2026-11-06，客户端连接代号那一轮在 load average 40～55 时观察到，与该改动无关）：① `robot/session` 的 `TestCallReturnsWhenTheWriteBlocksPastItsContext` 失败 1 次，robot 代码未改，单独重跑 10 次全过；② C++ `native_weak` 的 `send_stall` 模式在修后构建约 10 次里报过 1 次 `send failed`，旧代码构建约 10 次未见，机器空闲时两边交替各 6 次都通过；该模式的连接在服务端读完首包后才进入发送阶段，而改动只改了首包内容，推断无关。两者都没有失败现场（goroutine / 套接字状态），下次复现时先保留现场再判断是夹具时序还是实现问题。
- **game-demo 战斗**：限速链路与真实进程下的 demo 战斗未演练。

### 生成器、配置与模块（遗留）

#### 生成器与工具：遗留

- **`-template game` 的 Player 缺省没有 DAO**：要预加载 Player 先 `add dao Player --entity Player`（装任何往 Player 加状态的模块时安装器会建）。生成器应缺省带上 Player 的 DAO。
- **开发配置里两个带 DataEngine 的服务同 sid、同工作目录会争同一个 WAL 目录**（2026-11-04 发现，game-demo 加 alliance 服务时）：开发配置不写 `dataengine.wal.dir` 时取 `data/wal/dataengine/<sid>`，不含服务名；`deploy/dev/run.sh` 用同一个 `SID` 起全部服务，第二个进程会因 WAL 目录锁退出。game-demo 的 alliance 开发配置显式写了 `data/wal/alliance` 绕开（一个工作目录只能起一个 alliance 进程）；生成器的缺省目录是否要带服务名尚未决定。
- **嵌套类型里的 nil map 往返后文档形状变化**（2026-10-30，尚未确认是否需要处理）：修 DAO map 切片值副本（`cdf5ae07`）时观察到，嵌套 struct 的 map 字段为 nil 时持久化存成 `null`，读回后再存一次变成 `{}`（嵌套 `set<Field>RawMap(nil)` 总会新建空 map），把夹具放在 `GemInfo` 上时 `TestThePersistDocumentHasOneShapeAndRoundTrips` 因此报 “restore changed the document”；夹具改放 `Envelope` 避开，代码未改。语义上两者都是空 map，读取结果相同；影响限于文档形状不稳定（一次往返多一次无实际变化的写、按 `null` 查询的外部脚本查不到）。DAO 顶层 map 是否同样如此、是否有别的读者依赖 `null`，尚未核对。
- **端到端用的是内存权威**：`scripts/test-heartbeat-generated.sh` 以不落库 committer 代替 DataEngine，只覆盖新建实体（不覆盖卸载重载后重新登记、Remote 托管实体、真实 WAL / Mongo）；脚本不在 `go test ./...` 里；2026-10-31 起由 `scripts/pretag.sh` 每次发版跑（维护者决定纳入），平时改心跳 / 行为 / 时间域生成器时手工运行。

#### 业务模块（roost module）

模块是业务模板，下面多数条目同时写了既定设计与没做完的部分，整体归遗留；维护者已明确确认、不再当待办的设计见[业务模块的既定设计](#业务模块的既定设计)。

2026-11-10，[方案](FEATURE-2026-11-10-business-modules.md)第 2 期，现行说明见 [modules/README](../../modules/README.md) 与[命令参考“业务模块”](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module)。

- **gm 联动边界**：直接加减币 / 道具 / 经验不自动重试，超时不等于未生效；可重试的补偿使用稳定 `request_id` 的邮件。操作者字段是 token 持有人自报的审计信息，不是独立账号认证。只读 Nest 验收收件人后才调 mail，发件 RPC 不在实体锁内；换服返回 accepted 表示已受理。模块与 Nest 的离线验收不替代真实 ops HTTP / 跨进程 mail / 真实 DataEngine 交出实体，统一验收仍待本轮结束。
- **业务模块目录**（包括 gm、login、portal、servertime、purchase、rules、team，边界见下面各自的条目）：现行清单、固定场景数与联动以 [modules/README](../../modules/README.md) 和各 `module.yaml` 为准。新增场景线与六个补充模块面向新工程，不兼容旧生成工程/存储形状。game-demo 的属性/装备已换模块，移动共用 actor Body；cellworld、LOD/队伍视图、SceneReady、玩法刷怪、副本与地形适配仍在 demo，不能宣称全面迁移。13 类玩法尚未改成模块组合；casual / turn-pvp 仍需业务提供平台身份验证、角色号分配或可信匹配/胜负规则。
- **场景模块的边界**：scene / monster / terrain 是单进程所有权，没有跨进程切图或分布式容量。Scene / Terrain 的控制器构造不再要求 Start 后发布的 runtime；2026-11-13 已完成单机真实依赖的生成工程启动与 TCP 验收，未改框架生命周期。combat 仅固定双人会话，墙钟 50ms 拍，不跟随 scene 倍速；退出 / 会话结束恢复生命，死亡本身不自动结束会话。friends 仅拒绝申请 / 取消拉黑支持对方离线，双边动作仍需双方本进程在线以证明写归属。维护者已于 2026-11-13 确认该边界，归属不明时拒绝双边修改。notice 的 GM 命令仅装 gm 时登记；本身不默认调度。naming / cdkey 的共享占名依赖 Directory。详见[本轮修复验收](evidence/module-fixes-20261112/README.md)。
- **chat 模块的边界**：在线名单是每进程的内存集合，多个 game 进程不互相扇出世界频道的推送（要么各推各的在线玩家，要么改为订阅 chat 的流）；连接关闭的订阅在第一次取推送面时才挂上、进程退出时不单独退订；系统消息入口无条件授予（前提是 chat 只经内部总线可达，见 collaborators 的注释）；没有群组频道。基础 10 条场景覆盖频道规则、消息体校验、在线名单和本地频道登记；scene 联动增加 5 条附近聊天场景，Nest 锁内只取收件人快照，实际发送在锁外，没有附近历史或离线投递。game-demo 使用同一 chat 模块。
- **模块安装绑定 Player 与工程形状**：模块的 `entity:` 是名为 `Player` 的实体，按 `-template game` 的目录约定写文件（`game/entities/player/`、`game/handler/`、`game/controllers/<module>/`）；实体叫别的名字的工程装不了。2026-11-13 起 bag、wallet、attribute 的组件逻辑放在共用包 `game/components/<x>/`，经宿主接口（`bag.Owner`、`wallet.Owner`、`attribute.Owner`）拿状态，可以由工程手写一个接线文件挂到别的实体上（各模块 README 的“复用方式”，模块场景 `Test{Bag,Wallet,Attribute}SecondHost*` 在测试里的第二个实体上验证）；但安装器只接 Player，协议、自带 handler、钩子与联动不随组件到别的实体，第二个实体的状态字段、组件号与入口由工程自己加。activity 早已是同样的写法；其余模块（例如 checkin）的组件仍直接写死 `*Player`。
- **room 的边界**：1～64 席，单个 owner，公开房间的成员准入；没有私房邀请码、跨房唯一成员约束、自动广播或匹配移交。客户端保存 RoomID / Epoch，重连显式 CAS，连接 nonce 只从服务端 Session 取得。业务接入层仍需将该连接成功取得的代际交给 `room.Disconnect`；断线保留/踢出/重启后的状态重建不自动代办。持久 `Connected` 是最后处理的连接事件事实，不是跨重启在线权威。代理连接身份与配方的双人 Room/Turn 端点已覆盖真实 TCP；Account、跨服务客户端和持久提交仍为替身，Mongo/WAL 联合验收未完成。见 [room 接入](../../modules/room/README.md)。
- **运行期接线的边界**：`runtime` 声明的入口在框架 Mod 启动后、业务 Service.Init 前按依赖运行，业务先停、模块逆序停；不能依赖业务 Init 才发布的能力。自动接线只在生成的 bootstrap 生效，手动构造服务要使用 `NewWithModules()`。启动失败清理、停机失败保留依赖已验证；生命周期只支持一次 Init、串行 Shutdown，不支持重启。skillbook、actor、scene、monster、combat、terrain 已接入，其他后台消费者按模块逐项迁移；模块自己的 ingress 登记、超时与排空契约仍要实现。
- **team 的边界**：Remote 共享权威，Player 存队伍引用与有界邀请收件记录。七个请求加邀请推送；查询按 Team 当前权威过滤失效记录，续期用当前到期时间。邀请提交后仅对本进程在线玩家写收件记录并尽力推送，离线 / 跨 game 玩家没有自动投递；记录写入或推送失败不回滚权威邀请，查询也无法发现从未送达的记录。解散后的引用仍惰性清理，rehome 双向检查保留；真实跨进程共享队伍、弱网与容量未验收。
- **turn 与定时器边界**：固定开局名单、每位玩家成功命令连续编号、最后一条可重放；期限迟到只推进一次，离线实体下次加载补触发。成员减少/房间关闭在下一次推进结束，无自动广播、胜负/伤害规则或跨机接管；25 个模块场景使用替身提交器；另有配方双人 TCP → PB → Controller → Nest 链路验证，仍未作 Gate/NATS/真实存储联合验收。见 [turn](../../modules/turn/README.md)。模块仍不带 Robot 压测步骤。
- **enter 模块的边界**：它建档并跑首次初始化，安装时代办 `roost add transport tcp` 与 `access.player.self_sync: true`；但不做鉴权（`auth.go` 仍缺省拒绝，`roost config enable player-tcp` 也没有执行），所以真实客户端仍连不上。独立安装仍需业务接入鉴权与开启监听；配方使用 login 模块的进服票据/session 验证并开启开发监听；覆盖先新建、进入、重连和进入前拒绝，休闲配方另测发放与标记一起回滚。enter 自带 29 条场景（含进入前检查与 3 条事务内钩子登记检查）。 “先新建、再进入”那条路（`EnterCreate` / `EnterStatus` / `enter_need_create`，2026-11-12）只对被登记为“先新建”的会话生效（login 的联动登记的是用进服票据握手的会话），别的会话与没装 login 的工程照旧由 `EnterGame` 加载或创建。新建与进入的事务在模块场景里用真实的 Nest（替身提交器）验证，端点里“没有档就答 `enter_need_create`、不创建”与会话激活后读档已有两个配方的 TCP 场景和单机真实进程登录验证；不等同多实例故障恢复验证。新建先建一份空档再提交：名字被 naming 拒绝、提交失败时空档留着（没建成，下一次新建接着用），不合规的名字在建档之前被拒。已经建成的玩家再发 `EnterCreate` 答 `enter_created`、什么都不改。装了 naming 时名字先于“建成了”落定（占了名而新建事务没成的玩家，下一次换名字由 naming 释放旧名）；这条组合只用替身命名入口验证过，没有与真实的 naming 一起跑。在 `EnterGame` 里直接建成的玩家没有名字，照旧用改名协议。`PlayerDao.Enter` 多了一个只落库的字段 `Fresh`（store 键 3）。
- **game-demo 只用了 enter 模块的协议、域与首次初始化**：进入流程本身仍是 demo 自己的（`game/controllers/player/enter_game.go`），demo 覆盖了模块的 `protocol/def/enter_game.go` 与 `game/controllers/enter/` 两个文件。enter 域的 controller 自己建一个 player controller，一个进程里因此有两个 player controller 实例；它们共享的东西（在线名单、归属、场景）都经 Registry 或进程级对象，不存在各自一份的状态，但往 player controller 上加每实例状态时要记得这一点。demo 的 `enter_first.go` 是模块的缺省版本（不发任何东西）：demo 没有把自己的新玩家初始化搬进这个钩子。
- **联动文件的边界**：只在一对模块里第二个装上的那一次写；两个都早已装好（用旧版本的 roost 装的）不补。联动文件引用双方已装的源码：已装的模块被业务改过、或装的是另一版时，联动文件可能编译不过，`roost module add` 不编译工程，要靠随后的 `roost module verify` 发现。联动文件不能申请新号、不能加状态字段。
- **stamina 的边界**：同步只在写入时发生，体力自然恢复不产生推送，客户端要按上限、恢复周期与服务器业务时间自己推算（规则由 rules 在进入时推送或 RulesGet 补取，不在每条状态变化中重复带）；`Grant` 超过硬顶静默截断，调用方不看返回的 `Granted` 就是少发；调高上限等于给所有已满的玩家立即补到新上限；补满协议不带请求键，买完立刻用掉再重发会买两次；业务时钟被往回拨时，恢复中的玩家要等业务时间重新走过上次结算时刻才继续恢复；免费的一次补满（价格为 0）没有场景覆盖。
- **shop 的边界**：样例目录装上就在卖（10 金币买道具 1001），上线前必须换掉；请求键只在该玩家最近 64 次成功购买内去重，更早的键重发会再扣一次钱、再发一次货（受限购约束）；没有“期望价格”校验，客户端目录与服务端不一致时按服务端价格成交；背包放不下时靠事务回滚把已扣的钱退回，调用方不能吞掉 `Buy` 的错误；目录删行或复用商品号会清掉或串用终身限购计数（要保留计数用下架）；目录使用 shop_goods 配置表，统一 rules 快照不承诺跨请求冻结成交价格。
- **task 的边界**：支持配置表、每日/每周/永久与前置链。业务目录接受 ID 1～4096，ID/周期/前置发布后固定，退役保留永久领奖事实；无效行不可推进。Report/Advance 不自带跨消息去重，事件提供方在首次事实提交时调用；排行榜提交是跨服务后的尽力通知，重复提交也计数，不能拿来发恰好一次奖励。奖励按领取时配置读取。跨日/周未推进时同步记录仍是旧周期，客户端按周期戳判断。见[任务模块](../../modules/task/README.md)。
- **进入钩子不是事务的一部分**：钩子在首次初始化提交之后、事务之外执行，失败只记 Warn，下一次进入再调；必须成功的事（初始发放）仍写在 `enter_first.go` 的事务里。mailbox 的钩子缺省把“首次进入游戏的时间”当注册时间、范围只有“所有玩家”；会话 claims 里有 account 的 `role_created_at` / `server_id` / `origin_server_id` 时自动采用（角色创建时间、本服与合服前的原服范围），公会范围仍要工程自己加。`role_created_at` 缺失时退回首次进入的时间照拉（只会少收，不会多收）。`Entered.Claims` 与会话共用同一个 map，约定只读。工程改写进入端点（game-demo 就改写了）时要自己在同一位置调 `gamelogin.RunEntered`，否则钩子不执行。
- **模块的规则大多还是 Go 写的业务文件**：模块可以带配置表（bag 的道具表就是），但清单不能声明配置项。profile 的每级经验、stamina 的规则、leaderboard 的榜与周期（`leaderboard_board.go`）、mailbox 的账本宽限（600 秒）都是代码里的常量或业务文件，要调只能改已装的文件；task 与 shop 已使用配置表；rules 统一进入推送和补取，尚无热更广播或大目录分页。
- **mailbox 的边界**：只在进程内验证过（真实的 mail `Service` 加内存存储），经 NATS 的 BusClient、Redis 存储、超时与重投没有跑过；确认丢失没有后台补偿（钱已到账，邮件仍显示可领，玩家再点一次才补上确认）；领取账本存在 Player 文档里、每次首次发放扫一遍；账本的遗忘以“邮件过期后再等 600 秒”为准，game 进程之间业务时钟差超过它时，已遗忘的邮件可能被晚到的重放再发一次；Nest 调用因等结果超时返回错误、事务其实已提交的那条路径没有用例（重试会在账本上认出来，不会多发）。发件只有 `SendTo`（单个收件人、一份附件），没有已读与删除协议。附件：一封邮件只有一份（“100 金币加 3 个道具”发不了）；校验与发放是两处登记，只登记了校验的种类解得出、列表里看得见，但不可领；发件方的 `Encode` 只跑纯函数校验，道具号在不在表里、数量超不超背包上限要到领取时才知道——发了超过单种上限的道具，这封邮件每次领取都被背包拒绝直到过期；满背包的道具邮件在列表里一直显示可领（列表不取实体锁）；新旧进程混跑时旧进程把新种类当未知附件拒绝并归还预留，玩家会看到同一封邮件时而可领、时而不可领；Go 的 JSON 解码对键名大小写不敏感，手拼附件而不经 `Encode` 的发件方要注意。
- **leaderboard 的边界**：提交与改总分的事务不是原子的，提交失败时榜上暂时落后，靠重试或下一次分数变动补上（提交的是快照加 UpdateMax，本身幂等，不带请求键）；只升不降，一个错的大数提交上去之后压不下来（所以有分数上限的检查）；缺省是改 `leaderboard_board.go` 里 `CurrentSeason()` 的返回值后发版来换周期，也可以按业务时钟算，没有清榜协议；只有一张榜——读榜的两条协议固定读 `CurrentBoard()`，写入侧 `SubmitSeason` 只能选周期不能选榜，多张榜、读历史周期要手写。
- **bag 的边界**：表名 `item` 是固定的，工程已有 `configs/schema/item.go` 或 `configs/table/item.csv` 时装不了（落点被占用），要先把自己的表挪开、装完再合并列；进程里没装载配置时背包拒绝一切放入（有意如此：不退回“正数就行”），所以装了 bag 之后，往背包里放道具的测试都要先装道具表，否则一律 `bag_item_invalid`；每次变化同步的是整份背包（至多 200 种），不是按键增量；道具从表里删掉后能数、能丢、不能再放，单种上限调小到持有量以下时只拒放入、不扣已有；没有格子、装备与过期。
- **game-demo 的背包同步有一处冗余**：`Player.PublishSyncDirty` 与提交观察者各消费一份脏掩码，模块的写入只被观察者那份消费，所以背包变化后这名玩家下一次 `PublishSyncDirty`（走一步、加经验）会把整份背包再给本人发一次，内容相同。把 `Player.PublishSyncDirty` 改成用 `TakeEntitySyncChanges()`（TerrainChunk / Building 已经这样）应能消除，要动 Player 的整条同步路径，没有改。
- **profile 的边界**：名字不查重、不查敏感词；没有等级上限。在 game-demo 里，模块的 `ProfileAddExp` 只动 Player、不记 World 的全服累计（demo 自己的 AddExp 才记），demo 的入口都不走它；新开放的 `ProfileRename` 没有压测机器人覆盖。
- **模块装进 game-demo 之后靠提交观察者同步**：模块的 handler 不调 demo 的 `PublishSyncDirty`，靠 Nest 的提交观察者复制（bag、profile 各有一条按生产接线的用例钉住）；没装观察者的夹具里模块的写入不会被推送。
- **game-demo 删掉写死的号之后，模块的号会重排**：模块的协议号、错误码按“最小空号”分配；demo 每换用一块、删掉自己写死的号，空出来的号就被列表里靠前的模块占走（例如邮件换用后 `ChatSend` 10016 → 10008）。“新模块加在末尾、前面的号不变”只对加模块成立。未上线，不做兼容；要让号稳定需要安装器支持保留号，没有做。
- **game-demo 里 wallet 的金币没有玩法来源**：只有 GM 的 `gm.mail.send -coins` 邮件产出，demo 的玩法（副本、活动）发的仍是道具。
- **`roost add handler` 不能指向稍后才由模块装进来的组件**：game-demo 里建在 Bag / Profile 组件上的 handler 因此没有骨架步骤，文件直接写出；全部模块在同一步里安装，覆盖模块文件的 `write` 排在它之后（`TestDemoStepsLeaveModuleComponentsToTheModules` 钉住）。
- **`roost module verify` 把子测试数进通过数**：模块的场景测试因此不用 `t.Run`。
- **客户端访问按模块清单声明**：equipment、idle、gacha 独立安装会开启 `access.player.transport: tcp` 与 `self_sync: true`，已新建工程检查 manifest、生成文件与 build；dungeon 经 profile → enter 获得同样接线。仅声明协议不等于开启 TCP，其他模块须按自己清单与依赖核对。真正监听仍要认证器与 `roost config enable player-tcp`，独立安装验收不代表真实客户端鉴权已配置。
- **wallet 的去重窗口不是永久账本**：`Apply(key, delta)` 只在最近 128 次带业务键的加减内去重，更早的键被淘汰后再用会重新执行。长期“只发一次”由调用方在自己的状态里记事实并调 `Add`（checkin 这样做）。首版曾是“回执满了拒绝、不淘汰”，连续签到到第 129 天起签到与消费全部被拒，由只凭 skill 的实测发现并修掉（回归 `TestCheckinKeepsPayingPastTheWalletReceiptWindow`、`TestWalletOldReceiptsLeaveTheWindow`）。
- **惰性清零对客户端可见**：签到进度（以及照它写的每日功能）在当天第一次写入时才更新；跨天后没有新写入时，客户端经自身视图看到的仍是上一次的记录，要自己按业务日字段比对。当前业务日由 servertime 模块下发（`ServertimeNow` / `ServertimeSync` 的 Day、PrevDay、NextDay）；没装它的工程仍要客户端自己按业务时区推算。
- **login 的边界**：模块自己的场景是离线的（真实 account 内存存储 + 认证器；与 enter 的联动里游戏服的档是内存替身）。握手认两种票据：进服票据 `enter:<票据>`（account 的 `EnterServer`，新号的第一次握手在 account 里建出账号 ↔ 玩家号的映射——握手之后不在游戏服新建就离开的账号，映射与玩家号已经占了）与角色会话令牌。握手成功与否只由 account 决定，不等游戏服；游戏服的结论（本服该不该服务他、要不要先新建）在会话激活之后推 `EnterStatus`，**只是告知**：后台执行、同时至多 256 条会话、超过的丢弃、进程退出不等，推完带码的拒绝 300 毫秒后按玩家断开（传输没有只断一条会话的入口，慢连接可能没收到那一帧），权威判断在 `EnterGame` / `EnterCreate`。**独立 Gate 部署只做过一次单机真实进程演练**（2026-11-12，account + 走 Game Ingress 的 game + Gate 三个进程；接法与差异见 `modules/login/README.md` 的“边界”）：生成器不生成 Gate 进程，Gate 服务要工程自己装；每次握手 account 的 `EnterServer` 执行两次（Gate 与 Game Ingress 各认证一次）；别的服的票据在 Gate 上握手直接被断开、客户端收不到 `login_elsewhere`；告知要的 claims 由 gateway 的激活通知带到（`GameDependencies.OnActivated`，维护者 2026-11-12 定“改”；原先按玩家号记 30 秒的旁路缓存已删）；多个 Gate、重启后的重连、跨机没有跑。独立装模块仍要在开发配置里写 `login.dev_token`、`roost config enable player-tcp`，区服用模块带的 `cmd/accountctl upsert-server` 登记；game-demo 与两个配方已代办开发配置。生成的 `deploy/dev/run.sh` 在全部服务起来后自动登记本机的服，2026-11-12 在新生成的 game-demo 与两个配方工程上都实跑过。工程已有自己的 `cmd/accountctl` 时装不了 login。票据都不能单张吊销；封号对进服票据与账号票据立即生效（account 每次校验都再读账号），对已签发的会话令牌不生效（至多一个 `session_ttl`）。开发令牌可以冒充任何账号，生产拒绝靠环境键加 `dev-` 前缀——没写 `env: prod` 的预发环境带着它就是全员可冒充。doctor 的 `collaborators:account` 变 OK 不代表渠道验签已接。玩家号计数器在 Redis 里，被清或换前缀没搬数据会重号。没装 rehome 时换服在途的角色不被源服拒绝；login 与 rehome 的进入前检查先后靠名字排序。已经自己实现了 account collaborators 的工程装不了 login（`replaces` 全有或全无）。game-demo 与两个配方均已换用 login：进服票据握手的会话先新建再进入，会话令牌握手的仍是加载或创建；配方另有一条进入前检查拒绝换服在途的角色。
- **portal 的边界**：入口挂在**托管 account 服务的进程**上（清单 `runtime.framework: account`；自己手写 bootstrap 的工程要照着加 `kitmodulehost.NewMod`），三个端点：登录 → 凭票清单与推荐 → 换进服票据；不持有任何密钥（票据全部由 account 签发与校验），不建角不选角。模块自己的场景是离线的（真实 account 内存存储 + 真实路由引擎 + login 的认证器）。**真实进程只做过一次单机演练**（2026-11-12，最终形态：一个 account 进程加一个嵌入 TCP 的 game 进程，登录 → 清单与推荐 → 进服票据 → 握手 → `EnterStatus` → `EnterCreate` → `EnterGame`，以及各种拒绝，见 CHANGELOG）；game-demo 与两个配方另各做过 `make dev-run` 自动登记、curl 三步与真实 TCP 的登录 / 新建 / 重连（见各自的条目）；account 多副本、动态注册表、合服与换服中的角色、naming 组合、生产配置都没有跑；独立 Gate 另做过一次单机演练（见 login 的边界）。进服票据是哪个服的，映射就建在哪个服：别的服的票据拿到本服握手，account 照样在那个服上建映射，本服答 `login_elsewhere`。没有限流、没有防撞库，`/portal/login` 每次都去问渠道。凭据与票据在请求体里明文传，web Mod 不终止 TLS：对外要放在终止 TLS 的代理后面并显式写 `web.allow_public_addr`。账号票据与进服票据不落库、不能单张吊销，有效期内可重复使用（封号立即生效）。清单里“有哪些服”来自 account 的 `ListServers`（至多 256 个；有索引之前登记的服要重新 `UpsertServer` 一次才出现），“连哪里”来自区服目录：缺省是静态配置 `portal.servers`（写在 account 服务的配置里，启动时由模块自己读，不在 App 的配置声明里，`--check-config` 查不到写错的值），零配置时清单与进服结果里地址为空；动态目录只给了进程内带过期的注册表这个机制，**上报从哪里来（Gate / game 经总线、服务发现、运维登记）没有接**，而且注册表每个副本一张。清单的开销随区服数增长（account 读 1 + N 条记录，再每个服一次角色查询，顺序执行、没有缓存）；任何一个服的记录对不上（560113）或合服别名两跳，整个请求失败。清单里的 `role` 说的是 account 的映射：握手过而没有在游戏服新建的账号已经算“老号”。新号被送去非 open 的服时拿得到进服票据，握手才被拒、客户端拿不到原因。account 的 `ListServers`、`RoleOnServer`、`IssueAccountTicket`、`ValidateAccountTicket`、`IssueEnterTicket`、`EnterServer` 是新加的总线接口：manifest 拆出去之后比 account 进程新时请求会失败，要先升 account。
- **配方登录验收边界**（2026-11-12）：casual / turn-pvp 使用 login、portal、servertime，开发配置自带本机令牌与业务服地址，生产与 Secret 示例不带。定向生成验收分别 7 / 6 条配方场景、零 SKIP，包含真实回环 TCP、名字与重复新建断言，模块 verify、race、build / vet 和生成漂移；持久提交仍是替身。另将两份生成工程各自 `make dev-run`，在真实 Mongo 副本集 / Redis / NATS JetStream / etcd 上经 curl 三步、TCP 完成创建与重连。它验证登录通路，不涵盖完整购买/对局、self_sync 内容解析、独立 Gate、多实例、重启恢复、生产渠道或性能；全量模块/玩法矩阵留给统一收口。证据与复跑见[配方登录验收](evidence/recipes-login-20261112/README.md)。
- **game-demo 接入登录链路的边界**（2026-11-12，随业务模块分支 `feat/business-modules`，v1.45.0 起在 main）：demo 装 login / portal / servertime，握手用登录模块的认证器，`EnterGame` 对进服票据握手的会话遵守“先新建”，说明见 [demo 的登录一节](../../demo/WALKTHROUGH.md#登录入口--握手--先新建--进入)。真实进程只做过一次单机演练（`make dev-run` 的 11 个服务、curl 三步、机器人的 `-login portal` 与缺省的总线登录，见 CHANGELOG）；独立 Gate 下的 demo、多个 game 进程、换服与合服中的角色、现有的三个演练脚本（alliance-e2e、seamless、mmorpg crosssid）没有在新认证器下跑过。
  - **验证范围**：仓库里的用例是离线的——整条链路的场景（真实 account 内存存储、登录模块的认证器与告知、demo 的 `EnterGame` / `EnterCreate`、真实 Nest）里持久层、驻留表与传输是替身；开发配置上的三步登录是对着入口的 `Start` 与处理器跑的；机器人握手之后的 `EnterStatus` / `EnterCreate` / `EnterGame` 没有离线场景。真实进程上手工演练过一次（2026-11-12：`make dev-run`、curl 三步、`cmd/loadtest -login portal` 新号与老号各一轮、缺省的总线登录一轮，全部通过），没有做成可重复的脚本；现有演练脚本在新认证器下的整轮没有实跑。
- **servertime 的边界**：只有离线场景，推送经真实 TCP 到达、与 EnterGame 应答的先后、独立 Gate 部署都没有验收；独立 Gate 下 Game 重启后客户端连接是否保留尚未确认，保留时测试环境改 `time.logic_offset` 要等客户端下一次定期同步才发现；客户端的业务时间是估计（网络时延加走速差），重置前后几秒以服务器应答为准；重置之后在线客户端会集中来问一次，服务端不限流；与 checkin 等模块的一致性按表达式核对（同一个 `clock.BusinessDay`），没有调它们的 handler 对比。
- **模块验收的外部依赖边界**：`roost module verify` 的默认场景主要用正式 Nest + 生成 DAO / Sender 与替身提交器，不能代表线上部署。本轮 alliance 新入口已补单机多服务 + 真实 Mongo / Redis / NATS / etcd + Portal / TCP 演练；Scene / Terrain 也已补新工程真实启动、Scene 业务拒绝后同 TCP 连接继续切图 / 交互拒绝 / 退场，以及 Terrain 默认块查询；不等同于跨机、容量或地形编辑恢复验收。完整范围见[修复记录](evidence/module-fixes-20261112/README.md)。
- **嵌套字段的 `persist,nosync` 不生效（尚未确认是缺陷还是既定形状）**：在嵌套类型的字段上写 `dao:"persist,nosync"`，生成器接受，但这个字段照样出现在同步补丁的 PB 里（2026-11-10 在临时生成工程上实测）。`nopersist,nosync` 的嵌套字段另有既有用例（`codegen/internal/dao/testdata/remoteviews`），两者的语义没有逐项核对。现行规则：同步与否按 DAO 顶层字段决定，要让一部分状态不同步就放进另一个顶层字段的嵌套类型里。下一步：读 `codegen/internal/dao/sync_proto.go` 与 `template_nested.go` 里嵌套字段对 `Tag.Sync` / `Tag.Persist` 的处理，确认是该拒绝还是该生效。

- **模块自带场景读工程配置表（尚未确认）**：2026-11-13 组合 skill 试用发现，task 的自带场景读工程 `configs/data`，工程按 README 换表后 24 条变红；task 与它的联动已改为装载自带样例行，checkin 的测试不再引用被测常量。同类读工程配置的还有 shop（`shop_config_test`）、activity、activity_alliance 的场景，以及 purchase+bag、bag+gm 的联动；它们在工程换表后会不会红没有逐个确认。确认办法：新生成工程装上该模块，按它 README 的“复用方式”换表，跑 `roost module verify`。

##### 2026-11-12：活动模块的驱动与验收边界

[activity](../../modules/activity/README.md) 共用同一个 activity.Component 与 ActivityBook，分别挂在 Player、ActivityWorld、Alliance 上；实体包只有类型别名及宿主适配。个人活动复用 entitytimer；全服/联盟保留 Remote 不挂本地持久定时器的限制，权威服务每秒最多扫描一页 128 个宿主、8 个并发调用。扫描积压会延后推进，业务操作仍按冻结期限拒绝过期请求，不把扫描周期当准点保证。个人离线默认加载/进入时惰性恢复，需要离线准点结算时由业务提供可靠后台驱动。

每宿主单次最多 32 次阶段/流程动作；128 个定义号、128 条共享参与资格，满后明确拒绝，不驱逐未完成责任。排行榜/邮件/coordinator 只提供可组合适配，业务仍需冻结奖单并幂等执行。生成工程 33 条场景、9 个变异与两轮 race 已通过；Remote 替身、BSON 恢复和进程内路由验证不等同真实 Mongo/WAL/NATS、跨进程迁移或长稳。全模块矩阵与真实依赖按交接 §5/§6 最终验收。[证据](evidence/module-activity-20261112/README.md)。

##### 2026-11-12：purchase 模块的恢复与验收边界

[purchase](../../modules/purchase/README.md) 的 Player 账本是常量大小游标，Redis 未履行订单和 platform 支付历史仍会增长；每次补领最多 32 笔，较早的异常订单会暂缓该玩家后续订单。渠道缺省拒绝，真实支付验签/查单由工程接入。队列依赖部署提供的持久 Redis，游标、待发和 Player 必须协调恢复，不支持单独重置其中一侧。新工程离线场景、变异/race 与独立 Redis Lua 已验证；真实 NATS/TCP/Mongo/WAL、跨 sid 和故障恢复仍在新任务 §6，不宣称已验收。demo schema 4 不自动迁移旧订单账本。

##### R7 匹配模块的当前边界

[matchmaking](../../modules/matchmaking/README.md) 已提供本服单排/整队与匹配房移交。match 历史默认保留 10 分钟；已经绑定票但历史消失时不自动解除意图，返回 unknown 等待核对。默认每池候选窗口 64、两支同人数队伍成局，不提供跨服房间路由。匹配房的持久映射不自动清理。离线 + turn-pvp TCP 接线验收不能替代 §6 真实 Redis/Mongo/NATS 联合验收。

room 自动断线订阅、保留策略、重启在线状态重建、广播继续保留为 §5 后续；本轮实现名单准入和已有重连代际的组合。demo 的旧锁步匹配仍单独服务 battle，不在 R7 改造成回合房；R11 延后。

#### 联盟（alliance）

业务模块 R10 联盟的验收范围：alliance 模块从 demo 迁入，49 基础场景与两组各 2 条联动、6 个变异和两轮 race 通过。独立服务与三个 Saga 可由安装器直接装配；demo 复用相同源码。成员通知不可靠投递，离线/漏通知靠进入对账；副本与 HomeSID 不承担权威写权限。模块用例的提交器、总线和跨实例路由仍是替身，真实 Mongo / Redis / NATS / etcd、双 game sid 的迁服 kill -9 和全模块矩阵留在最终联合验收。历史联盟压测属于当时的 demo 版本，不自动当作本轮模块迁移验证。见[模块说明](../../modules/alliance/README.md)。

- **game-demo 公会迁到 alliance 服务后的边界**（联盟服务方案阶段 4，2026-11-04；原“game-demo 的公会跨进程写会被拒”一条由此解决：公会 kind 归 alliance 服务（`service=alliance`），game 进程的 Remote 运行时对它们拒绝认领与写（`ForeignKinds`；无缝大世界原型 2026-11-04 合入后 game 为自己的 Team 保留 `remote_entity`，见 [无缝大世界方案 §14](FEATURE-2026-11-03-seamless-world.md#14-合入-main2026-11-04)），公会只由 alliance 服务写，game 经路由 RPC / Saga 改、经 Mirror 读，见 [WALKTHROUGH](../../demo/WALKTHROUGH.md#跨服务公会--alliance-服务写走-rpc-或-saga读走-mirror)）：① **建盟 / 捐献的迟到成功不撤销**：公会一侧的 Mongo 步骤在协调器已放弃（超过 `guild.Deadline` 10 分钟或步骤次数用尽）之后才成功时，扣费已退回，补偿只记 Warn（`guild saga: the found/donate step succeeded after the saga gave up`），联盟与捐献保留——玩家可能白得一个联盟或一笔贡献。demo 不做解散 / 扣回，业务需要时在补偿里调服务的撤销方法。领奖的迟到成功由 `ReleaseReward` 撤销，没有这个问题。② **成员变化通知尽力投递**（2026-11-05 晚起不经 effect：alliance 写答复成功后由服务层直接按成员的原服 sid 发一次，失败只记日志与 `alliance.member_notice_lost_total`，[联盟服务方案 §2.5B](FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）：只给本进程在线的玩家改副本（`PlayerDao.GuildID`，2026-11-05 起持久）与推送；同一玩家的通知乱序时，离开只清“正显示这个公会”的，不会盖掉新公会，但“加入”晚于更新的“离开”到达时副本会停在旧公会，直到下一条通知、玩家自己的公会操作或下次登录的核对。**离线时被批准入会：已由登录查询补上**（维护者 2026-11-05 选 A）：登录时副本为 0 就经只读路由 RPC `Membership` 问一次守卫（守卫被占先自愈再答；不新建守卫——守卫不存在答拒绝），答出公会就在玩家事务里写回；查询在登录的 RPC 阶段、超时 1 秒，失败只记日志、副本保持 0、不影响登录。代价：副本为 0 的玩家每次登录多一次 RPC，守卫冷加载时多一次 Mongo 读；在线玩家的“加入”通知丢了仍要等下一条通知、一次公会操作或下次登录。覆盖：`TestLoginLooksUpAGuildJoinedWhileOffline`（生成工程单元）、`TestAllianceE2EGamePlayers` 的“离线入会后再登录”（真实进程）、机器人场景 `guild_offline_join`（2026-11-06：离线由运维命令 `gm.player.join_guild` 保证——game 仍有该玩家会话时不入会、机器人重试，不靠等待；`TestAllianceE2ERobotOfflineJoin` 在真实进程上跑 4 个机器人，`make dev-smoke` 跑第二轮；去掉登录查询时 4 轮全红）。剩下的不确定：入会后成员通知若在玩家重新登录之后才被 game 处理，副本由通知写上，那一轮没有检验登录查询（断言仍成立）；4 轮变异全红，没观察到。③ **金币与奖励**：建盟费与捐献扣道具 9001（`configs/table/item.csv`，最多叠 9999），`PlayerDao.Gold` 仍不用；领奖的道具与数量是 alliance 的原型缺省（`alliance.reward_item_id` 1002、`reward_count` 5、`reward_period` 24h），按周期号（Unix 秒 / 周期 + 1）判断，改周期长度会让周期号跳变。④ **读用 Monotonic**：Cached 未命中时不回源，第一次读一个没见过的公会会答“不存在”，所以展示也用 Monotonic，代价是每个未命中一次回源；game 进程的生成配置 `cached_max_staleness` 为 5s（`fe4a1153`，维护者 2026-11-04 同意阶段 5 的推荐；alliance 进程不读 Mirror，仍是 30s）。阶段 5 量了回源：推送正常时回源量正好是“在读的不同 key 数 / 陈旧上限”，与读频率无关（21 个 key、60s：30s 时 42 次 Redis HGET，5s 时 252 次）；5s 的代价是 10 万在线约 2 万次/秒 HGET（[benchmarks/alliance-20261104](../performance/benchmarks/alliance-20261104/README.md#mirror-回源testalliancee2emirrorrevalidation)）。⑤ 阶段 5 已用真实进程验证（`scripts/test-alliance-e2e.sh` 的 `TestAllianceE2EGamePlayers`）：两个 game 进程上的玩家加入同一公会并带写版本互相读到、成员通知推到另一个 game 进程上在线的盟主；公会一侧冻结后 kill -9 时在途捐献 / 建盟 Saga 收敛、Player 扣款步骤之后 kill -9 game，资金与道具 9001 各只变一次；建盟与入会同时发生时服务拒绝建盟、补偿退费。仍未覆盖：机器人加入别的玩家的公会只有 `guild_offline_join` 一条（同一个机器人的第二个身份、同一个 game 进程、经运维入会）；两个 game 进程上的玩家互相入会仍只由 Go 客户端驱动的 `TestAllianceE2EGamePlayers` 覆盖。
- **宣战扫描接手**（RV-20261107-01 修复后，见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-07新功能审查收尾)）：迁入后到新 owner 补问最多一个扫描间隔（1 分钟）再加一次读；扫描每分钟一次所有权列表查询。回归与红绿见 [联盟方案 §2.5B“宣战”](FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)。真实进程端到端没有构造“检查与 fence 之间宣战”的时序（夹具用 ownership 钩子构造），只验证了空认领扫描与既有迁移 / 接管用例。
- **`alliance.transfer` 的分页**（见[已修复的历史](evidence/known-limits-history-20261113/README.md#2026-11-04alliancetransfer-count-在实体多时超过运维命令期限)）：单个实体的转移本身约 19～25ms，一次命令在缺省 `ops.admin_timeout` 10s 内约转 400 个，上万个要反复调用（缩容脚本照“直到 moved 为空”循环即可）；有未决预约的守卫每次都会被列出并报 pending，`count` 小于这类守卫数时一次调用可能一个都转不走。

### 框架服务与 game-demo（遗留）

#### game-demo 的移动、怪物与场景时间（D20 第 3 期）

game-demo 手写接入心跳、移动、行为、状态与场景时间域（[DESIGN-TODO D20 P3](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#p3-实施结果2026-10-21)，[WALKTHROUGH](../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20)）。

- **投递方式跟驱动空值走，不读配置**：demo 手写接线、不装 `wiring/heartbeat` Mod，所以不读 `heartbeat.delivery`；`game/actor` 的 `heartbeat.Config` 没设 `Delivery`，即缺省的批量作业。要回到单目标，在那里设 `Delivery: heartbeat.DeliveryDispatch`（只能在驱动第一次 `Start` 之前）。
- **一个进程一张地图**：心跳驱动、`actor.WorldTime`（场景时间域）与 `actor.Ground`（地形）都是进程级的；成员在构造时绑定域（`BeatConfig.TimeDomain` 运行中不能换）。多场景的游戏要在成员进入场景时构造它的行为组件，或不挂域。
- **玩家不经 `LeaveScene` 离开**：断线只走复制桥的 `settle`（不进 Nest 事务）、卸载不调 `Leave`，所以 `SaveOnLeave` 在 demo 里不会触发，存档位置只靠 10 秒一次的周期落库（`actor.SavePeriod`）：崩溃或卸载最多丢 10 秒的走动；仍在内存里的玩家重连时留在原处。
- **寻路预算用完走直线**：`PathFind.Path` 最多访问 4096 个节点，开阔地上远处的斜向目标就会用完；用完时改走直线，`Move` 每进一格问一次地形，被挡停在最后一个可走的格（动作以 `Blocked` 失败），不绕路。证明不可达的目标直接拒绝。
- **`Join` 之前的移动 AOI 不跟**：`actor.InterestMoves` 对兴趣政策还不认识的实体（进场景事务已提交、`Scene.Join` 还没做或失败）忽略 `ErrInterestUnknown`，位置照写、AOI 不更新；`Join` 用进场景事务返回的位置，期间走的几步要等下一步才进 AOI。
- **只有怪物的 Buff 挂了心跳，没有技能 Runtime**：怪物的 `CombatComponent` 用 P5 的 `AttachBeat`（1Hz、场景时间），限时定身到点由它自己的心跳移除；demo 的实体上没有技能 `Runtime`（`game/skills` 只编译目录），所以没有用 `NewRuntimeBeat`。到期最多晚一个心跳周期（1 秒）加 tick 取整。怪物的 `CombatDao` / `StateDao` 不在 DaoManager 里（怪物不持久），只有 `rollback=undo` 的 handler 能回滚它们（demo 的 handler 都是 undo）。
- **怪物只巡逻**：没有追击、回巢、占地与战斗伤害；巡逻走直线（出生点附近边长 12 的正方形），角按地形裁剪。
- **刷怪一次一批**：上一批还在建时交来的批次丢弃，等下一拍；建一只要一次 `lifecycle.Create`、一次 `ShowSubject` 与一条场景 + 怪物的两实体 Nest 消息（场景实体是这一步的串行点，玩家的 `move_target` / `move` 也锁场景）。
- **场景视野的扇出上限 65536**：每个成员都挂在场景视野上（`MaxPlayersPerView` 由缺省 4096 调到 `sceneMaxMembers`）；超过时 `AttachView` 失败、只打 Warn，那个玩家收不到场景时间。
- **机器人负载：100 / 200 人同秒起跑不通过**（2026-10-22 晚，[压测清单 §3 结果](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)）：`make dev-smoke`（2 / 2）与 20 人 loadtest 通过；100 / 200 人（单 IP 上限调到 512）connect 全部成功、`object limit exceeded` 0，但成功 24 / 100、30 / 200：gift saga 步骤排队超时、公会目录实体慢派发，200 人时 `enter_game` 写全服 World 实体与上单场景实体在请求期限内没轮到（110 个复制会话没打开）。按现有证据是 demo 场景的共享热点与同秒突发，不判为框架缺陷，**尚未确认**（没有按秒爬坡的对照）。

#### 框架服务与 game-demo：未测部分

- **匹配队列**：异步复制丢最近写、跨机分区与 Cluster 下的容量未测。夹具的后台 Sweep 因参数错误（limit 1000 超过上限 200）未生效，单样本。
- **匹配补位**：业务层补位只在派对夹具的内存存储与单进程匹配循环上验证，Redis 存储与多进程匹配器未跑。派对夹具只在内存存储上用弹性队列，真实 Redis 上的弹性语义由 `wiring/integration` 的 `TestMatchFlexibleGroupingOnRedis` 单独覆盖（单机 Redis，Cluster 未跑）。
- **game-demo 活动结算榜**：`ZADD GT` 原子保留较大累计值，需要 Redis 6.2 及以上；只修复成功回复乱序导致的回退。Redis 写失败只记日志，协调器与结算索引不是一个事务，没有自动对账；Redis Cluster、跨机延迟与长稳未验证。业务将来允许扣分时需改用版本化快照或其他明确的顺序方案。
- **全服邮件**：① **账本规模没有压测**：账本是一个文档（缺省至多 1000 条未过期记录，每条约 150 字节 JSON），每次发布 / 撤回整体读写、在 Redis Cluster 上落在一个节点；读路径按版本在进程内缓存，账本变化后每个 mail 进程各重读一次。上界附近的发布延迟、十万级在线同时登录时版本键的读压力、公会邮件频繁发送时的上界是否够用 v1.29 未验证。
- **业务时区**：distroless 镜像本身是否带 zoneinfo 仍未在容器内实测，内嵌后不再依赖它。
- **合服（D15）的验证边界**：真实进程两服演练（私有环境，sid 1000 / 1001，[证据](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/benchmarks/d15-server-merge-20261020/README.md)）覆盖嵌入 TCP；独立 Gate 只有单元级（正式 account 服务 + `FixedGameResolver`）。演练里的 `dbscope=sid` 文档是直接写 Mongo 的（game-demo 没有用它的实体），B 的进程没有冷加载它们；玩家数据（共享库 main）经真实进程读写。

#### 单个玩家换服（模块 rehome 与 game-demo 接线）

2026-11-11，[方案](FEATURE-2026-11-11-player-rehome.md)一期。既定设计见[单个玩家换服（rehome）：既定设计](#单个玩家换服rehome既定设计)。

- **只有离线验证**：模块的场景用替身提交器，交出实体是替身；game-demo 的用例用真实的 `account.Service`（内存存储）与两张真实的驻留表。真实的 `HandOffEntity` 路径（模块的缺省实现与 demo 的离开流程）、两个 game 进程加 account 的演练、每个阶段后 kill -9、checkpoint 等待实际要多久，都没有做。
- **没装 enter 的工程要自己接两处**：加载 Player 之前调 `Driver.Admit`、进入之后调 `Driver.Arrive`，否则换走的玩家仍能进旧服、目标服不做到达。工程改写了进入端点（game-demo 就改写了）时要自己调 `gamelogin.RunEntering`。
- **向前恢复只由登录触发**：源服受理后重启、玩家又不再来登录源服的，换服停在在途；account 没有“列出在途换服”的索引，要用 `accountctl transfer-role commit|abort` 人工了结。`rehome.Binding`（saga 准入用）不触发恢复。
- **受理之后中止也会断开会话**——没有推送协议，客户端在被受理之后唯一的信号是断线后重新选角。它与 `Serve` 不直接收 claims（claims 只在模块的 `Admit` 一处解释）、enter 模块的应答没有“目标服”字段，是实现时的取舍，等维护者定。（离开钩子的先后已改回设计的顺序：断开会话并排空 → 复核 → 离开钩子 → 交出，RV-20261111-BM-05。）
- **game-demo 换服前的业务边界**：队伍通过双向事务内准入阻止成员换服，本服开着分区场景时仍拒绝。联盟成员可以换服，R10 已按入盟批次与旧 SID 条件刷新 HomeSID，进入时对账；它是尽力通知地址，不是 account 的线性一致副本。匹配票据、进行中的副本与对战、名下建筑与地形账本仍未被完整检查；全服邮件的服范围未随换服自动迁移。
- **搬档（v1.47.0）的崩溃窗口与验证**：
  - 提交之后、删源库之前源服崩溃：源库留一份旧档，源服重启时被预加载进内存（接不到他的请求，但实体自己的定时逻辑仍可能写这份旧档）；他再来登录源服续做时补搬不覆盖目标服已写过的版本，源库这份记 Error 留给运维删。没有后台清扫。
  - 只有离线场景与框架侧（单测 + 真实副本集集成）验证，模块经真实 DataEngine 走到搬档、两个真实 game 进程的演练都没有做。
- 费用没有实现（模块不依赖货币，规则文件里写了做法）。

#### 单个角色换服（account 一侧）：遗留

- **绕开 rehome 模块直接改绑不安全**：游戏侧的停写、断开、交出实体、到达处理由 `rehome` 模块完成（v1.45.0 起；game-demo 的 `PlayerOwners` 与 saga 准入用它的绑定与“换服中”状态）。account 自己不检查源服是否已经停写：直接调这四个接口（或 `accountctl transfer-role commit`）改绑一个在线玩家，会出现两个进程写同一个玩家。
- **待维护者确认的决定**（方案 §6 第 1～4 项，按推荐项先实施）：接口上总线；`Role.ServerID` 改为“当前绑定服”、不另存建角服；目标服必须 open、未并入、账号在目标服没有角色；发起之后中途停下一律向前恢复。
- **期限只记录**：account 不自动中止过期的换服，也不拒绝迟到的提交；没有在途换服的列举与清扫（方案三期）。
- **验证边界**：单元用例在 Memory 与本机隔离的单机 Redis 上都跑（含注入失败的存储、并发的提交 / 中止、用 channel 固定的“占位写入之前 / 之后 × 中止 / 提交”交错与占位应答丢失），`wiring/integration` 有一条真实 Redis 用例；Redis Cluster 没有跑；没有真实进程（两个 game sid + account）的演练，那是游戏侧接上之后的事。

### 地形与无缝世界（遗留）

#### 地形 / 无缝世界：遗留

- **game-demo 自己的地形 DAO 仍是 global**：`terrain_chunks`、`buildings`、`terrain_editors`（维护者本轮只点名 Player / World）。terrain 模块的 `terrain_building` / `terrain_chunk` 已改 sid；分区场景（同一张地图跨多个 game 服）下地形按服分库是否合适**尚未确认**，要定下来并让 demo 与模块一致。
- **地形副本订阅超限的 demo 端到端用例**：被拒的路径在 demo 里没有端到端用例（要一张对一个格超过 32768 块的图），由 `gameplay/terrain` 的 `TestReplicaSubscriptionLimitPerOwner` 覆盖。
- **跨格笔刷的演练与旧接口**：真实多进程演练的 (a) 步已按此改写，改写时只编译、未运行；[v1.43.0 发版说明](../release/v1.43.0-NOTES.md)记录发版前在 main 上跑过开地形的分区多进程演练 6 步（该步在其中）。`gameplay/terrain` 的 `Brush.ApplyIn` / `View.BrushChangesIn` 因此没有调用方，是否删除待确认。
- **转发编辑用例在重负载下的一次 `-race` 失败（尚未确认）**：`TestTerrainRelayForwardsABrushToTheCellThatWritesIt` 在与仓库其余测试并发运行时失败一次，原文没有保存，之后单独、并发与 `-count=40` 的压力运行都没有复现；疑似进程内网转发超过用例配的 300ms 期限（结论 Unknown）。见[证据](evidence/terrain-brush-single-cell-20261110/README.md#尚未确认一次--race-失败没有复现)。
- **玩家换 Base 进程之后旧进程里的 `TerrainEditor`（尚未确认）**：分区场景的“发号”账本是全局库里的普通实体，只由玩家当前的 Base 写；demo 没有在玩家离开时卸载它的机制（`EntityCategoryPlayerScoped` 只决定取锁次序）。玩家从 Base 1 换到 Base 2 编辑、再回到 Base 1 时，Base 1 内存里若还是旧副本，会按旧的 `LastSeq` 判号并在下一次提交时把库里较新的账本写回旧值（`LastSeq` 回退）。各格的账本是持久的、不受影响，所以块不会被写两次；受影响的是答给客户端的 `LastSeq` 与“过期 / 超前”判断。普通场景单进程时没有这个问题。没有构造用例；需要确认 demo 的玩家实体在换进程时的统一处理（App 级的单写者保证）后再定。
- **分区场景里建筑进本进程场景主视野的接线只有编译与单元覆盖**：`EnableBuildings` 现在也在分区场景调用，`attachBuildings` / `loadStanding` 随之在分区的 Base 场景上执行；连在别的 Base 上的玩家看不见建筑（需要建筑副本，§27.6）。真实多进程演练没有放建筑的步骤。
- **游戏尺度分区视野（`view_radius` 68m、`ghost_band` 70m）的成本没有实测**：witness 列表、Ghost 数、副本订阅的块数都随之增加（T09）。
- **变异未杀死的三项**（如实记录）：去掉“占地提交后立即推送”（订阅每秒续期按修订号补齐，5s 的等待里照样到）、去掉格的在途去重（账本去重兜底）、去掉答复的 incarnation 核对（没有构造“Base 重启后序号撞上旧答复”的用例）。
- **地形确认来源 ③④⑤**：③ 原始地形包里坏了的块（服务端装不进视图）、分区场景里拥有者装不进的块没有标记，客户端永远不确认（后者每次订阅续期都会再问一次）；④ C# 接入层只用构造的说法核对时序，没有用真实 entitysync 帧驱动 `EntityViews` → `TerrainChunkSync`，Unity 未跑；⑤ 没有在真实依赖（mirror-local）与无缝演练上跑过（合入时跑）。证据见 [evidence/terrain-confirm-source-20261110](evidence/terrain-confirm-source-20261110/README.md)。
- **2026-11-09：地形首批参数实施边界**：默认编辑配额与桶容量已降至 270，terrainpack 默认 HeightBase 已设为 −100 米（−10000 厘米）；不重写既有资源。按类型同步现已完成框架与 demo 接线。本条写下时“圆滑岸线只完成决策，原地形去重两项问题及 P3～P6 未完成”，此后的状态（2026-11-11 按 CHANGELOG v1.43.0 段核对）：去重两项 T01 / T02 已修复；P3（Ghost 副本、移动与寻路、跨格移动）、P4（区域水位、建筑）、P5（转发编辑、分区场景的建筑）的代码与岸线 / 重建调度的 C# 算法层都已合入 main 并随 v1.43.0 发布；仍未完成的是 P5 的规模与降级验收（T09）、P6 可选物理（T10）、Unity 表现，以及真实 Mongo / WAL 崩溃演练、跨机与长稳验收。当前清单见 [地形进度总表](TERRAIN-STATUS.md)。功能测试、修正记录与预期跳过见 [实施状态](TERRAIN-DECISIONS-20261109.md)；未新增压测，历史 270 档临时数据不代替容量验收。
- **无缝大世界原型**：上线前必须解决的其余项见方案 §6.3；其余原型简化与待产品规则见方案 §6.3、§8.4、§9.6、§10.4、§11.10、§12.2、§13.6（刷怪账本 E5、面板变化不回血、邀请不推送、队伍面板只在分区场景里等）。待维护者决定：Team 归 game 服务（`service=game`、共享模式，game 因此保留 `remote_entity`）还是像联盟一样归一个实体服务独占（方案 §14.3、§14.6）。
- **cellworld 的 Linux 原始失败样本**：审查时的 Linux 原始失败样本未在本机复现，若收尾真的卡住会超时并带诊断失败，不会被掩盖。
- **cellworld 真实时钟用例**：仍按墙钟、负载更高时可能误报的只剩协议自己的墙钟期限（HandoffAbort 3s、租约 1s、GhostTTL），各处注释写明。证据见 [cellworld 真实时钟用例整治](evidence/cellworld-clock-cleanup-20261110/README.md)。

### 玩法组件与物理房间（遗留）

#### LevelComponent 与 UGC 物理闯关：遗留

- **LevelComponent**：该组件尚未验证真实 Mongo/WAL 多进程崩溃恢复，不恢复原生物理世界。状态复制与整状态 mutation 有预算但没有容量承诺，动作账本满额拒绝，长期归档另设计。
- **UGC 物理闯关**：可选 PhysX 房间当前支持运动学转轮/平台、动态 Box、弹簧抓取和铰链马达、固定版本关卡加载、Unity 预测/纠偏接法；见 [物理房间契约与验证](../framework/PHYSICS-ROOM-SERVICE.md#unity-ugc-多人闯关运动抓取关卡与预测)。Linux 原生功能、真实 TCP/Gate/NATS/Nest 联合链及 C# 预测核心已验证，新增 Unity 交互未在编辑器运行。历史单房间性能数据不覆盖新载荷/约束或多房间容量。
- **UGC 分频显示**：Unity 编辑器、WAN 和多房间容量未验。
- **UGC 多房间与长稳**：物理房间已补 Linux 的 1/4/8 房间短测（每房 20 个逻辑客户端，普通/机关两类）。这是直接 Service + PhysX + Nest、预热 1 秒/采集 5 秒的证据，不含生产网络、原生内存与长稳，不能作为容量上限；普通 8 房间样本实际物理均频 49.32Hz。Go 分配已于 2026-10-29 profile 定位并削减：macOS ARM64 同机对照，机关 8 房间 81.57→31.15MB/s、普通 70.04→27.38MB/s，载荷字节不变；同日晚每帧状态索引与 AOI 索引由 map 改为切片（房间提交路径不再复制本帧状态），再降到机关 23.46、普通 19.87MB/s（同机交替 3 轮，载荷字节不变）；剩余主要是 `StateDAO.Accept` 的所有权副本（约 3MB/s，去掉要让 Nest undo 保护的权威状态与网络侧索引共用切片，判断为高风险，不做）与交给网络 / 视图账本的副本。**macOS ARM64 长稳已测**（2026-10-29，机关 8 房间 × 20 客户端连续 20 分钟）：footprint、Go 映射、goroutine、PhysX 原生堆（malloc 已分配约 9.6MiB / 8 房间，个数持平）在稳定窗口斜率都在 0 附近，没有发现无界增长；物理均频 49.85Hz（不含 vmmap 挂起窗口最低 49.93Hz）。RSS 每分钟 +0.035MB 推断为干净页换入（footprint 持平），未逐区域核对；交付频率后 8 分钟比前 8 分钟低 0.9%，原因尚未确认（倾向客户端定时器）。**仍未测**：Linux 的复测、RSS 与长稳（本机 Docker 未运行），小时级与房间反复创建销毁，真实 Gate/NATS/TCP 链路下的内存，原生堆按房间拆分。见 [测量口径和复跑](../framework/PHYSICS-ROOM-SERVICE.md#共享索引与多房间测量2026-10-26)、[分配 profile](../framework/PHYSICS-ROOM-SERVICE.md#go-分配-profile-与削减2026-10-29macos-arm64) 与 [长稳与原生内存](../framework/PHYSICS-ROOM-SERVICE.md#长稳rss-与原生内存2026-10-29macos-arm64)。闯关流程由通用 `gameplay/level` 承担（见上文“通用 LevelComponent 首版边界”），不另建专用检查点 / 排名 / 结算状态机；物理动作执行器与 LevelComponent 的真实联合玩法尚未验收。

### 客户端（遗留）

#### 客户端：遗留

- **Native 客户端**：macOS ARM64 的 Editor、Mono/IL2CPP Player、50～10000 可见集解码和 TCP 延迟/分片/故障已实跑，见 [客户端报告](../performance/CLIENT-20261101.md)；不替代 Windows/Linux/Android/iOS 或 UE 验收，移动前后台/真实 WAN 和设备容量仍未测。TLS/WS/UDP/KCP 适配未实现。
- **地形 C#**：C# 只在 .NET 10 的测试工程上跑过，Unity / IL2CPP 未跑。
- **场景时间域的 Unity 适配**：Unity 适配层没有接。
- **同一 SessionID 跨 Gate 的客户端覆盖**：② C++ 没有跨 Gate 端到端，Unity Editor / Player、IL2CPP 与 Unreal 也没有实跑，只验到 native 库、托管绑定与共享 C# 运行库这一层；
- **未运行**：C++ 的 protobuf 用例（`ROOST_PROTOBUF_TESTS=ON`，`client/cpp/tests/views.py` 已按新 API 改写）本机没有 protoc，没跑；C++ 不带 protobuf 的 ctest（含 sync 解码）已跑通过。
- **客户端引擎**：有 Go / C# 及 Unity 适配源码与测试，未声明真实 Unity / Godot / Unreal 全矩阵；各引擎实机、线程、断线与跨平台确定性未验收。

### 验收与运维（遗留）

#### 平台与外部验证

- **跨机HA与Linux物理故障**：本机集群 / loopback 不证明跨机器分区、掉电、真实网卡；具名清单见 [EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) E01～E28。
- **独立 Gate**：已实施 Gate / Game、广播与 Sync / Lockstep（[GATEWAY](../framework/GATEWAY.md)）；不用本机多实例代替多进程 / 跨机矩阵。容量结论见能力表。
- **单个玩家改绑 sid 与 Player 跨机演练**：account 一侧与游戏侧（`rehome` 模块，v1.45.0 起；v1.47.0 起换服搬 sid 库）都已实现；跨机部署下的 Player 换服仍未演练（见[单个角色换服（account 一侧）](#单个角色换服account-一侧)）；合服用别名合服（D15 已实施，边界见下文[合服（D15）](#合服d15)）；真实跨机网络、卷迁移、客户端重连路径未演练；框架级迁移命令上线后评估。2026-10-20 夜间在 v1.30 候选上重做同机两进程演练（[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md) §13：正常迁移、kill -9 后原 WAL 重启、Mongo 停顿时 kill -9 的未排空，302 次确认逐键 +1、最终 Mongo 等于最后确认），与 10-15 一致。

#### Linux 功能与新功能复核（2026-11-02）

[Linux 验收续报](LINUX-FUNCTIONAL-20261102.md)保留 Lockstep、etcd、NATS 与 SLG 的原始失败和环境限制；[v1.38～v1.40 功能复核](FEATURE-REVIEW-20261102.md)补充 DAO / Sync / Remote View 的 Linux 功能证据。两份报告不代表生产容量或完整上线验收。后续定位与修复见 [核心执行边界审查](CORE-REVIEW.md)。旧提交审查记录保留于[固定审查分支历史](https://github.com/tjbdwanghaibo/roost-core/blob/ed3b36473ff116e8010d590ae98fb8b77798e012/docs/maintenance/COMMIT-REVIEW.md)，不恢复已经归档的维护流水账。

#### Robot 推广验收（2026-11-01）

13 类网络玩法已扩至现有规则的对局/经营闭环；公共 Robot 增加有界 Full/Delta/Remove 应用延迟观测，见 [Robot](../framework/ROBOT.md)。这些默认玩法仍使用替身鉴权/提交器。通用 game-demo 的真实账号/Gate/PB/strict WAL/Mongo 热点诊断与每个玩法的生产组合不是同一项验收。

本机单 Player + 共享 World 的 100/s 投影积压已定位为验收记录器隐藏 MongoStore 批量接口，导致逐笔投影/checkpoint；修复后同配置两轮 30s 的负载末 WALUnacked 为 6/4，排空后均 0，一致性通过。仍有 19/15 个输入丢拍，短测不代表无丢拍持续容量；strict WAL 与 Mongo 持久级别不变，原失败证据保留。20/s 对照、1000 玩家/10000 Entity 与少量 Saga 混合短测见 [当前性能报告](../performance/ROBOT-GATE-20261101.md)。Linux、跨机、完整进程故障组合和移动设备仍未验收。

#### 10-23 候选诊断边界

[诊断续报](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/PRERELEASE-DIAGNOSTICS-20261023.md)：WAL 刷盘指标漏记已修复，生成工程 async 复现持锁 6.272ms，其中提交阶段 6.234ms，主要等 WAL writer（后台 fsync 均值 5.494ms）；不精确回填旧样本；Redis MOVED / ASK 已加入既有有界安全重试，持续重定向仍可耗尽预算，另建六节点计划切换验证 7383 次写无错，历史八次实际路由未恢复。Saga 修复原子认领并显式启用两个消费 worker 后，正向 30/s × 10 分钟 18,000 笔全部成功、ManualRequired=0、P99 576ms，默认仍串行。全局停顿与 200ms 心跳档抖动仍未确定根因，不能把这五项标为全部收尾。 后续[混合负载诊断](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/benchmarks/jetstream-isolation-20261023/README.md)观察到Mongo journal同步与NATS文件写入相互影响；维护者决定保留事务、majority+j:true及生产默认配置，暂不继续压测，实际Mongo服务容量留待目标环境验证。

#### sync-business 夹具的范围

64 人、10% 脏、40 tick 冒烟通过并出数字（脚本墙钟 50 秒），正式档位由维护者统一跑，本条不提供容量结论。剩下的边界：夹具的移动只登记 AOI 索引、不改 DAO 位置，`sync_bytes` 不含移动的下行（移动看 `TestMainViewMeasureMovingCrowdBytes`）。见[证据“夹具重写”](evidence/perf-terrain-acceptance-20261110/README.md#夹具重写2026-11-10-追加已修复)。

#### 能力与验证边界

口径：v1.28.0 与 v1.29.0 发版时都没有压测；2026-10-19 发版后补测 v1.29.0 同参数重档 30 分钟与 Saga 两档（[v1.29.0 发版后复测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/V1290-HEAVY-30M-20261019.md)，附同日同机 v1.27.0 对照）。2026-10-20 夜间在 main `69fb668f`（v1.30.0 候选）上按维护者批准的清单补测，每项 15 分钟（Saga Mongo 步骤夹具上限 10 分钟），服务端 / 压测端分开统计，宿主负载正常、全部作为基线（[夜间补测](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md)）。下表“当前结论”取最新一次。

| 项 | 当前结论 | 后续入口 |
| --- | --- | --- |
| 重档总体 | 30 分钟通过：吞吐、计数、DAO 终值、AOI 可见集合、P99 门槛通过，消息 / HB 拒绝 0，堆不增长；goroutine 恒定比 v1.27.0 多 1000（每连接写协程）。业务 P99 13.09ms、HB 11.01ms（混合口径）；服务端口径业务 P99 4.37ms、HB 2.68ms（第一份未污染的 30 分钟服务端样本）。2026-10-20 夜间 A-B-A-B（v1.27.0 / HEAD 各 2 次 × 15 分钟）：HEAD 的 Sync 提交→客户端 P99 稳定高 0.6～1.1ms（13.99 / 13.92 对 13.35 / 12.80），两对方向一致，判断为代码带来的小幅增加，具体来源未做单变量对照、尚未确认；业务 / HB 混合 P99 高 0.1～0.2ms、Gate 往返无差异；服务端业务 P99 3.37 / 3.38ms | 单变量对照（例如 B3 写协程交接）；独占服务器 / 跨机 |
| Sync偶发长尾 | on_change 按 P99≤50ms 验收：Sync 提交→客户端 P99 15.43ms、最大 156.262ms、>50ms 0.0260%。>50ms 总量变多同日对照同样出现，按宿主条件处理；10-19 的“Sync >50ms 为同日对照 1.8 倍”在 2026-10-20 夜间 A-B-A-B 里不复现（四次服务端段 >50ms 都是 0），判断为宿主条件、不是代码。`TAIL=metrics` 15 分钟只出现 1 个长尾簇（284 个混合口径事件、最大 52.6ms），落在整分钟后第 1 秒、同秒调度延迟 21ms，相位与 10-15 一致但只有一簇，不足以确认；非整分钟的簇本次没有出现 | 维护者 2026-10-15 决定先接受现状，有独占服务器 / Linux 实机时再复测（方案 A） |
| 独立 Gate | 重档 30 分钟通过（short 8 / io 8，GOMAXPROCS=10，Gate 与 Sync 开）：Gate 往返 P99 6.78ms、最大 236.691ms。2026-10-20 夜间 A-B-A-B：HEAD 5.30 / 5.26ms 对 v1.27.0 5.40 / 5.20ms，>50ms 四次都是 0，10-19 的“>50ms 为对照 2.6 倍”不复现 | 多进程 / 跨机矩阵；[Gate/AOI 性能报告](../performance/GATE-AOI.md) |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行 observer。2026-10-20 夜间 4 并行度重档 15 分钟通过（GOMAXPROCS 4、short 4）：服务端业务 P99 4.83ms、Gate 往返 P99 7.74ms、Sync 提交→客户端 P99 17.98ms，>50ms（服务端）0；不压额外 observedBlocks 扇出 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验（未做） |
| 嵌入 TCP 出站 | 2026-10-20 夜间嵌入 TCP + 场景 Sync 重档 15 分钟通过：`budget_exhausted` 与全部出站拒绝 / 关闭计数 0，写协程 1000 → 1000；服务端业务 P99 1.57ms、往返 P99 1.98ms、Sync 提交→客户端 P99 10.14ms，>50ms 全 0；堆 208MB。每连接队列峰值与驻留字节取不到（没有 Gauge） | 停读客户端 / 更高推送量下的预算；驻留 Gauge 与按帧龄关闭有实际停读或驻留内存问题再做（DESIGN-TODO“保持现状的决定”第 11 项） |
| 自身视图（self_sync） | 2026-10-20 夜间嵌入 TCP + self_sync 重档 15 分钟通过：约每秒 1000 帧，`push_refused` 0、滞留 0，提交→客户端 P99 1.43ms，服务端业务 P99 2.07ms；整轮重试放大未被触发（未测到） | 独立 Gate + self_sync 夹具；预算压到触发点的放大测量 |
| Lockstep 追帧 / 快照恢复 | 嵌入 TCP 追帧随链路放慢由包内与 game-demo 生成工程的真实回环 TCP 回归覆盖；快照恢复（2026-10-20）由包内、MOBA 十客户端 KCP、生成工程 C# 真实 TCP 回归覆盖，toxiproxy 演练嵌入 TCP 256 / 64 KB/s 下 1～4 MiB 快照恢复、独立 Gate 慢链路上大快照被断开循环（只准入，需 Gate 写出回执的协议改动，维护者 2026-10-20 决定暂不做）；纯追帧页 2026-10-20 夜间 toxiproxy 256 / 64 KB/s 演练：回到头部时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断 | 独立 Gate 慢链路上的纯追帧页；真实移动网络 |
| Remote容量 | 2026-10-20 夜间 80TPS × 15 分钟通过（共享隔离环境）：72000 / 72000，零错误零丢弃，全量核验通过，服务端 p50 / p95 / p99 164 / 249 / 749ms。2026-10-21 同机对照（v1.27.0 `a147877c` / HEAD `af904c76` 各 1 次 × 15 分钟，同参数背靠背，[§9.1](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md#91-同机-a-b-a-b2026-10-21)）：两边都通过，混合 p99 同为 263ms（HEAD 服务端口径 262ms），p50 / p95 158 / 229 对 160 / 231ms，WAL 未确认 / 在途事务峰值 17 / 16 对 19 / 19；夜间 p99 偏高与峰值约 2 倍**倾向环境条件（同时段共享 Mongo 主节点慢操作 640 条对今天 79～126 条），不是代码退化**，单样本、Mongo 当时变慢的原因尚未确认；非跨机上限 | 跨机 RTT / 故障 / 24h；要排除 HEAD max / ≥500ms 次数偏多（1280ms、122 条对 605ms、26 条，单样本）需每边 ≥2 次交替 |
| Saga补偿容量 | **旧 Redis / 旧原生路径的数字，第 4 批之后不再适用（正式实现的数字见上文 [Saga](#saga) 的“正式实现的性能”条）**。Mongo 步骤路径 v1.29 各 10 分钟：正向 20/s P99 0.62s、全补偿 10/s P99 0.61s（服务端口径 0.59 / 0.58s），十个分钟段无增长趋势，核对通过。2026-10-20 夜间更高档位（10 分钟）：正向 30/s 完成率 28.4/s、P99 141.7s、17302 笔以补偿 / 失败结束，全补偿 15/s 完成率 14.0/s、P50 单调增长到 49.5s、866 笔 failed——两档都饱和（outbox / lease 排空、存储与发布零失败）。单进程上限在正向 20～30/s、全补偿 10～15/s 之间 | 中间档位（25、12）；真实步骤、混合负载与生产存储另测 |
| Saga 原生步骤容量（单进程） | **旧 Redis / 旧原生路径的数字，第 4 批之后不再适用（正式实现的数字见上文 [Saga](#saga) 的“正式实现的性能”条）**。2026-10-20 夜间 15 分钟：正向 8/s（两次）与全补偿 5/s 都 0 笔终态不对、核对通过；服务端 P50 559 / 549ms、713ms（P50 < 1s 成立），**P99 0.92～3.9s**，有分钟级尖峰（各段 Mongo 命令最大值同时到 750～900ms，接近投影器容量时积压放大）；两次正向 8/s 的 P99 差 4 倍。原先 guide/06 §5、生成的 `sagaCapacitySummary` / `steps.go` 注释与 `roost help saga` 里的“P99 0.8 / 1.2 s”来自 2 分钟单样本，15 分钟样本上不成立；2026-10-21 维护者批准，这几处与 CODEGEN_REFERENCE / PROJECT_GENERATOR / BEGINNER_WORKBOOK 已改为 15 分钟口径（P50 < 1s、P99 0.9～3.9s、WAL 放快盘、Mongo 步骤上限区间）；限制与维护者决定见上文 [Saga](#saga) | [SAGA-CHECKPOINT](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/SAGA-CHECKPOINT-20261018.md) 复跑入口；P99 尖峰的 Mongo 侧原因 |
| 投影 checkpoint 与 outbox 唤醒同盘 | v1.29 未验证：2026-10-18 观测到投影后唤醒 outbox（`de8b6e97`）使 checkpoint 变慢，机制为推断（darwin 上 `WAL.Ack` 的 F_FULLFSYNC 与被唤醒 outbox 的 Mongo majority 写同时落盘），无逐次 fsync 证据，Linux 未测。唤醒的净收益为正，不回退；WAL 与 Mongo 分盘（第三档 C）不做。2026-10-20 夜间 RAM disk 对照（正向 8/s × 15 分钟，WAL 放 RAM disk）：checkpoint P50 56～58 → 1ms、WAL 追加 18 → 1ms、投影等待 62 → 0ms、服务端 P50 549～559 → 334ms，空载 checkpoint 约 8.8ms，说明负载下变慢主要来自同盘竞争；RAM disk 同时去掉设备 fsync 与同盘竞争，两者没有分开；P99 由 Mongo 侧停顿决定、两种盘都有。2026-10-21 维护者批准把“磁盘同步是主要成本，部署时 WAL 放快盘”写进 guide/06 §5 与生成的容量说明 | Linux 复测；逐次 fsync 计时 |
| 匹配队列容量 | 2026-10-20 夜间（本机私有 Redis，一次性夹具）：每分区“每秒成局人数 × 保留期”持续上限约 4500 人（状态约 2.3 MB），4 分区各 10 人/s、保留期 10 分钟第 8 分钟崩溃（CAS 冲突 0.94、超时、同 Redis PING P99 0.6s）；CommitAcross 竞争冲突约 49%；Cluster 主节点 SIGKILL 切换两次无部分生效。见上文“匹配队列状态” | Cluster 下的容量；跨机 Redis（guide/09 §4 已补保留期的规模建议，2026-10-21） |
| 生成工程完整链路持久写入（嵌入 TCP + strict） | 2026-10-20 单样本、非独占宿主（[报告](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/GENERATED-TCP-STRICT-20261020.md)）：CLI 生成工程、strict handler 金币 +1、不经 Saga、无 self_sync，50 连接各自一个玩家**压测端** 1253 TPS、P50 38ms、P99 75ms，WAL 每批约 9.9 条；同一实体 108 TPS。上限约为 short 池 worker 数（缺省 GOMAXPROCS）/ 每笔持锁时间（strict 在锁内等所在批 fsync，本机约 8ms）。async 不比 strict 快（同机 1083 对 1257），原因尚未确认 | 2026-10-22 发版候选复跑（[NIGHT-20261022 §13](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md#13-第-14-项生成工程完整链路持久写入嵌入-tcp--strict)）：strict 1241～1314 TPS、同实体 113 TPS，pipelined / async 对照已做；self_sync 对照与独占宿主仍待补测 |
| schema 升级 / Player 跨机演练 | 2026-10-20 夜间在 v1.30 候选生成的 game-demo 上真实进程演练通过（schema：拒绝旧 schema → plan / apply / verify → rollback 只恢复未被写过的一份；Player：正常迁移、kill -9 后原 WAL 重启、未排空被 `wal-drained` 拒绝，302 次确认逐键 +1），与 10-15 一致 | 真实跨机网络、卷迁移、客户端重连路径 |

#### 游戏接入验收

既有十份接入手册的“第二部分 验收”是各自验收范围的完整说明（场景清单、证明与不证明、崩溃点、计数核对）。**发版门禁**（2026-10-31 起）：当前十三类玩法脚本的缺省模式（离线、替身提交器；新增三类见 SESSION-GENRES）由 `scripts/pretag.sh` 每次发版并行跑一遍（并发数 `ROOST_PRETAG_GENRE_JOBS`，缺省 4；本机 macOS 10 核实测：空闲时 45～60 秒，pretag 全程里、机器另有负载（load 7～8）时 125 秒），缺省模式同时把真实模式的恢复夹具复制进生成工程并 `go vet`（只编译不运行）；根包 `TestGeneratedScriptsAreInPretag` 要求每个 `scripts/test-*-generated.sh` 都在 pretag 里或登记了豁免原因（此前九个脚本不在任何门禁里，v1.33.0 上 card-rpg / mmorpg / merge-casual 三个夹具因 D22-1 改了 Tick 签名编译失败而无人发现）。**不在门禁里的**：真实模式（`ROOST_<玩法>_REAL=1` 需要隔离 Mongo 等，仍在改 `game` 模板、`add dao` / `add entity` / `add timer` / `add attribute`、多实体 Sender、Nest 生成或 Lockstep 时手动跑；入口登记在根包 `integration_coverage_test.go`）；守卫只查 pretag 调用了脚本，场景是否真的执行由各脚本自己的通过数断言负责。夹具里的数值都是业务样例，玩法规则缺少默认实现不算框架缺陷。

| 游戏 | 入口脚本 / 真实模式变量 | 证明什么 | 不证明什么 | 手册 |
| --- | --- | --- | --- | --- |
| SLG | `scripts/test-slg-generated.sh`，`ROOST_SLG_REAL=1` | 6 个建造、行军、抢地组合的事务与期限行为 + 1 条业务 Robot（脚本断言 7 条）；联盟活动联合链路 3 条：协调器子进程重启后贡献只计一次、阶段屏障、原服奖励义务丢 ACK 后重投，两个原服 Player 在“已到账未标记”处 SIGKILL 后冷加载补完，联盟权威经 Remote owner 交接后旧 owner 被拒 | 缺省模式无 WAL / Mongo、无真实重载与驱动加载后投递（离线定时器只做 DAO 水合）；联合链路的联盟 owner 与 sid 客户端同在一个父进程、固定服务发现、协调器只正常重启、视野传输为内存捕获、无空间迷雾与跨服撤权、不含实体定时器 | [第二部分](../framework/SLG-INTEGRATION.md#第二部分验收) |
| MMORPG | `scripts/test-mmorpg-generated.sh`，`ROOST_MMORPG_REAL=1`；`test-mmorpg-scenes.sh`；`test-mmorpg-cross-sid.sh` | 13 个多实体组合 + 1 条业务 Robot（脚本断言 14 条）；正常停机与 SIGKILL 后交易 / 托管 / 定时器经投递循环恢复不丢不重；多地图 TCP；双 sid 赠礼 Saga | 提交在途崩溃、断电、跨机、动态 Player 跨 sid、真实客户端与目标密度 | [第二部分](../framework/MMORPG-INTEGRATION.md#第二部分验收) |
| MOBA | `scripts/test-moba-generated.sh`，`ROOST_MOBA_REAL=1`，再加 `ROOST_MOBA_ROUTING=1` | 28 条结算 / Lockstep / 匹配 / KCP 追帧场景（含公共 `robot.LockstepBot` 的 KCP 整局与客户端结算结果；含断线超过历史窗口后经服务器参考检查点快照恢复：直接驱动 Match 一条、经持久对局 Room 与已提交门一条；Room 提交卡住 / 结果未知时不发未提交帧）；结算两进程恢复；活动对局 SIGKILL 后续赛与 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复（真实夹具的宿主都用 `RecoverRoomWithSnapshots`）；五进程 NATS 宿主交接与奖励重投 | 对客户端广播与真实鉴权的端到端链路、权威 HA、tick 预算、5v5 目标负载与跨机 | [第二部分](../framework/MOBA-INTEGRATION.md#第二部分验收) |
| 卡牌 RPG | `scripts/test-card-rpg-generated.sh`，`ROOST_CARD_RPG_REAL=1` | 16 个抽卡 / 养成 / 回合组合 + 1 条业务 Robot（脚本断言 17 条）；SIGKILL 后从未投影 WAL 恢复，投递循环补一回合再按期投递 | 提交在途崩溃、断电、系统时钟长时计时、Redeliver、多进程与容量 | [第二部分](../framework/CARD-RPG-INTEGRATION.md#第二部分验收) |
| 合作塔防 | `scripts/test-tower-defense-generated.sh`，`ROOST_TOWER_DEFENSE_REAL=1` | 10 个共享建造 / 波次组合 + 1 条业务 Robot（脚本断言 11 条）；第一波进行中 SIGKILL 后逐字节恢复，循环补一步再按期打完三波 | 提交在途崩溃、断电、四台真实客户端、多进程与容量 | [第二部分](../framework/TOWER-DEFENSE-INTEGRATION.md#第二部分验收) |
| 合作生存撤离 | `scripts/test-survival-extraction-generated.sh`，`ROOST_SURVIVAL_EXTRACTION_REAL=1` | 12 个入场 / 采集 / 制作 / 撤离组合 + 1 条业务 Robot（脚本断言 13 条）；制作与撤离在途 SIGKILL 后恢复，循环补触发一次且不追帧 | 恢复路径上的探险结束处理、提交在途崩溃、真实客户端、多进程与容量 | [第二部分](../framework/SURVIVAL-EXTRACTION-INTEGRATION.md#第二部分验收) |
| 合成休闲 | `scripts/test-merge-casual-generated.sh`，`ROOST_MERGE_CASUAL_REAL=1` | 15 个合成 / 离线批次 / 广告组合 + 1 条业务 Robot（脚本断言 16 条）；SIGKILL 后恢复，过期挑战只判负一次、下一期限按时投递 | 提交在途崩溃、真实广告平台与登录票分配、多进程与容量 | [第二部分](../framework/MERGE-CASUAL-INTEGRATION.md#第二部分验收) |
| 放置模拟 | `scripts/test-idle-sim-generated.sh`，`ROOST_IDLE_SIM_REAL=1` | 12 个生产队列 / 离线收益组合 + 1 条业务 Robot（脚本断言 13 条）；SIGKILL 后恢复，补触发只积分到原窗口、下一秒刷新按时投递 | 提交在途崩溃、真实客户端 / 支付、跨服市场、多进程与容量 | [第二部分](../framework/IDLE-SIM-INTEGRATION.md#第二部分验收) |
| 自走棋 | `scripts/test-auto-chess-generated.sh`，`ROOST_AUTO_CHESS_REAL=1` | 12 个卡池 / 阶段 / 领奖组合 + 1 条业务 Robot（脚本断言 13 条）；冻结战斗后 SIGKILL 恢复，循环只补一个阶段并按期推进到终局 | 提交在途崩溃、八个网络客户端、跨 sid 发奖、多进程与容量 | [第二部分](../framework/AUTO-CHESS-INTEGRATION.md#第二部分验收) |
| 实时派对房间 | `scripts/test-party-room-generated.sh`，`ROOST_PARTY_ROOM_REAL=1` | 18 个成局 / 进房 / 心跳回合 / 组同步 / 去重 / 重连 / 结算组合 + 1 条业务 Robot（脚本断言 19 条）（心跳跑真实驱动循环，房间同步组与自身视图经正式 Nest Mod 装在同一个 Manager 上，正式 Receiver 按命名空间分流解码）；局内 SIGKILL 后冷加载、心跳按持久阶段重启并结算、发奖一次、删档 | 提交在途崩溃、Entity Sync 经真实接入层与慢消费者、match / session 的 Redis 存储与 RPC、房间跨机接管、房间数与推送带宽容量 | [第二部分](../framework/PARTY-ROOM-INTEGRATION.md#第二部分验收) |

- **组同步的容量与独立 Gate 用例**（设计边界见[组同步与 self_sync](#组同步与-self_sync)）：没有房间数 / 成员数的容量数据；独立 Gate 下的组同步、慢消费者断开只经自身视图的既有用例间接覆盖，没有组同步自己的用例；派对房间真实依赖模式（`ROOST_PARTY_ROOM_REAL=1`）改用新入口后 2026-11-02 在 Linux（v1.37.1）上通过（[Linux 验收续报](LINUX-FUNCTIONAL-20261102.md)）。

#### 待补测（历史压测遗留）

<a id="v129-待补测"></a><a id="待补测v131-发版前"></a>v1.31 发版前的整体压测（命令、判据与原报告）汇总在 [V1310-PRERELEASE-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/V1310-PRERELEASE-20261022.md)：选项 C 参数、Interest ApplySync 预算、game-demo 100 / 200 人 loadtest、2026-10-20 夜间补测 13 项与第 14 项在发版候选上重跑、AOI 三点与两种应用模式、心跳负载；Linux 复测可选。2026-10-22 晚已执行（逐项状态见清单 §0 末尾）：选项 C 参数、ApplySync 预算、demo loadtest、AOI 三点、心跳负载已完成，结果写在上面对应条目；夜间补测在发版候选上的重跑已完成（2026-10-22～23，[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)：14 项判据全部成立，没有发现产品缺陷），下面各条按重跑结果更新。夜间补测（2026-10-20，[报告](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261020.md)）留下的、要等更合适环境的项：

1. **Sync 长尾的归因**：`TAIL=metrics` 15 分钟只出现 1 个长尾簇（整分钟后第 1 秒），不足以确认或排除与宿主整分钟周期任务同秒；按方案 A 等独占服务器 / Linux 实机复测。2026-10-22 重跑 15 分钟没有任何 >50ms 事件（[NIGHT-20261022 §4](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md#4-第-4-项sync-长尾归因roost_nest_game_tailmetrics)），仍无法分析相位。
2. **Sync 提交→客户端 P99 +0.6～1.1ms 的来源**：A-B-A-B 判断为代码带来，但没有单变量对照，具体提交尚未确认。2026-10-22 发版候选对 v1.29.0 的 A-B-A-B（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：发版候选反而低 2.4～2.7ms（两对一致），所以至少相对 v1.29.0 没有这类增加；相对 v1.27.0 的那段增加的来源仍未确认。
3. **Remote 80TPS 夜间 p99 偏高**：2026-10-21 同机对照（各 1 次）v1.27.0 / HEAD p99 同为 263ms，倾向环境条件；夜间共享 Mongo 主节点变慢的原因尚未确认。2026-10-22 发版候选复测（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)） p99 287ms、共享 Mongo 慢操作 0，与“环境条件”一致。
4. **投影 checkpoint 同盘**：RAM disk 对照已做（2026-10-22 重跑结论相同：服务端 P50 −49%、投影不积压（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）），Linux 上复测与逐次 fsync 计时没做。
5. **Saga Mongo 步骤中间档位（历史，随旧协调器关闭）**：以下数字测的是已删除的旧协调器与旧压测夹具 `TestRealSagaPerformance`（saga JetStream 切换第 4 批 `298e4396` 删除），对正式实现不再适用，也不能用当前代码复跑；正式实现（含 Mongo 步骤）的容量由 `scripts/perf/saga-js-scan.sh` 测，见上文 [Saga](#saga) 的“正式实现的性能”条。原记录：20 / 10 稳定、30 / 15 饱和，25 / 12 未测；2026-10-22 重跑（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：正向 30/s 仍饱和（另有 3 笔 ManualRequired，夜间 0，尚未逐个核对），全补偿 15/s 这次稳定（14.96/s、0 笔终态不对、P99 3.0s），15/s 是边界档、两晚环境不同，需要多样本；旧夹具 `TestRealSagaPerformance` 当时限制单次 ≤10 分钟。10-23 修复原子认领 + 显式两个消费 worker 后，正向 30/s × 10 分钟全成功；旧串行容量不覆盖新配置，全补偿的新配置仍待验证。
6. **匹配队列**：Cluster 下的容量、跨机 Redis、异步复制丢写没有测。夹具的 Sweep 参数 2026-10-22 已在一次性夹具里改为 200 并重跑（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：Sweep 生效后饱和点不变（每分区状态约 2.6 MB，第 8 分钟）；Cluster 主节点切换窗口里有 8 次 `MOVED` 直接返回调用方（夜间没有），受控测试确认驱动跟随重定向；现已将一次 Cluster 预算耗尽后的 MOVED / ASK 纳入既有有界安全重试，历史八次具体路由仍未确认。
7. **生成工程完整链路持久写入**（原第 14 项）：2026-10-22 已在发版候选上复跑（[NIGHT-20261022](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md)）：strict 50 连接 1241～1314 TPS（4 次）、同一实体 113 TPS，与 1253 TPS 单样本一致；pipelined 持锁 0.07ms 但闭环吞吐不变；async 1378～1451 TPS、锁内仍约 6.8ms（旧指标漏记后台 fsync，已修正；不能再据此断言没有 fsync，等待占比需剖析）。**仍待补测**：self_sync 对照、pipelined 的开环 / 同实体并发写对照、独占宿主。
8. **demo 按秒爬坡**（维护者 2026-10-22 晚决定 A，已做：[清单 §3](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/V1310-PRERELEASE-20261022.md#3-game-demo-loadtest-100--200-人)）：每秒起跑 2 个时 100 / 200 人全部通过；更高速率的失败来自 gift saga（旧协调器原生步骤每进程约 8/s，已随 JetStream 切换删除，不再作为当前容量）、奖励邮件 effect 链与公会目录；按当前实现复测登记在 [DESIGN-TODO“先测量再优化”](DESIGN-TODO.md#先测量再优化)第 2 项。

#### 测试夹具与模板的一致性

Nest 的 `TestLifecycleCreateInHandler*` 仍需保留：它们覆盖 handler 内新建的生命周期契约。测试手写的 `generatedShapeLifecycle` 与模板没有自动同步守卫；不能因生成器另有测试就删掉。原审计证据见[测试审计快照](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/test-audit-2026-10-29/README.md)。

#### 新增离散玩法纵切

回合 PvP、合作 Roguelike、异步社交经营分别提供 9、10、9 个场景，进入 pretag，包含公共 Robot 的实际 TCP/PB 接线；合作包含双客户端三关、领奖、重连和授权视图。玩法纵切仍用替身提交器，未逐玩法验证真实持久恢复、跨服或容量；回合超时尚无自动 timer 装配，Roguelike 仅离散闯关。范围与命令见[手册](../framework/SESSION-GENRES.md)。

## 三、已修复的历史

只留一行结论；现象、根因、红绿与复现原文在[已修复历史的证据](evidence/known-limits-history-20261113/README.md)（2026-11-13 从本文件原样移出），发布状态以 CHANGELOG 为准。修复后仍有效的边界在前两部分对应的条目里。

- **2026-11-13：saga 准入的“换服中”钩子**：原来 demo 的 `admitBound` 只留钩子 `playerMoving`（缺省不在换服中）；rehome 模块合入后（v1.45.0）准入改用 `PlayerOwners` 取玩家当前绑定与是否换服中，钩子已删除（`kitsaga.WithAdmit(steps.admitBound)`，`demo/internal/service/game/gift_saga.go.tmpl`）。
- **2026-11-13：combat 与 dungeon 的组件 DAO 原来是 global**：`CombatDao` / `LevelDAO` 原来没有 `DbScope()`，挂在 sid 实体上时一个实体的文档分在两个库里；2026-11-13 改为跟随宿主主 DAO 的范围，注册校验拒绝范围不一致的组合（未发版）。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-13本地实体起服预加载与本地实体-dao-缺省-dbscopesidcombat-与-dungeon-的组件-dao)；CHANGELOG `[Unreleased]`“组件 DAO 跟随宿主实体的 dbscope”。
- **2026-11-11：地形审查 RV-20261110-01～04**：四项已在 main 修复，留下的限制见[地形副本订阅与地图参数](#地形副本订阅与地图参数rv-20261110-0104-修复后)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-11地形审查-rv-20261110-0104已修复未发版留下的边界)；[地形进度总表 §2](TERRAIN-STATUS.md#2-缺陷修复均已关闭)。
- **2026-11-11：wallet 的业务键**：`Apply` 的键只收 1～64 字节的 ASCII 字母、数字与 `- _ :`，别的答 `wallet_invalid`（shop 模块的作者发现）。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-11wallet-的业务键)
- **2026-11-10：开地形场景同步 128 个对象时每观察者的下行**：增量字节压缩后 128 个同时动约 18.6KB/s，在 50KB/s 预算以内，不再需要维护者决定；降频 / LOD 不做。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10开地形场景同步-128-个对象时每观察者的下行已重算增量字节压缩后约-186kbs在-50kbs-以内)
- **2026-11-10：场景主视野改用 near / team 视图**：主视野改用 packer 的 `near` 视图、队伍关系视野用 `team`（业务层实施，未改框架），留下的边界见[场景主视野的视图与 Codec](#场景主视野的视图与-codec)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10场景主视野改用-near--team-视图)
- **2026-11-10：同一视图换 Codec 时改发全量**：确认会静默错用后，Manager 按（会话, subject）记基线 Codec，不符就转回待快照、下一次 Flush 收全量（Reason = Resync），代价见[场景主视野的视图与 Codec](#场景主视野的视图与-codec)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10同一视图换-codec-时改发全量)
- **2026-11-10：建筑进场景主视野后的 demo 尺度与偶发全量**：主视野尺度先按 §28 放大、再由 §29 按游戏尺度重定；偶发的“第二次改参数以全量发出”已证实为 on_change 冻结的缺陷并修复（随 v1.43.0 发布）。剩下的代价与尚未确认的频率原因分别见[场景主视野的视图与 Codec](#场景主视野的视图与-codec)与[同步：尚未确认的偶发与疑点](#同步尚未确认的偶发与疑点)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10建筑进场景主视野后的-demo-尺度维护者-2026-11-10主视野已按推荐放大见地形方案-28偶发已定位修复见本条末段)；[证据](evidence/entitysync-resync-flake-20261110/README.md)。
- **2026-11-10：cellworld 按经过时间积分的子步截断**：按 A 实施：零头按千分位存在 `ActorState.FracX` / `FracY`（未发版），剩下的边界见[cellworld 的位置零头与入场序号](#cellworld-的位置零头与入场序号)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10cellworld-按经过时间积分的子步截断已证实维护者-2026-11-10-选定按千分位整数累计零头已修复未发版)；CHANGELOG `[Unreleased]`。
<a id="2026-11-10无缝演练最后一步普通场景机器人压测阈值时过时不过尚未确认"></a>
- **2026-11-10：无缝演练最后一步（普通场景机器人压测阈值）时过时不过**：已定位为 saga 步骤落到非属主进程后按 NAK 退避重投；先放宽演练阈值（方案 A），2026-11-11 改为 saga 步骤按 sid 确切路由（方案 ③）并恢复缺省阈值 16s。剩下的边界见 [Saga](#saga) 与 [Saga：遗留](#saga遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10无缝演练最后一步普通场景机器人压测阈值时过时不过尚未确认)；[FEATURE-2026-11-11-saga-step-sid-route](FEATURE-2026-11-11-saga-step-sid-route.md)。
- **2026-11-10：`scripts/perf/sync-business.sh` 夹具与当前 core 不兼容**：原因 ③ 更正为空载荷增量，夹具按当前契约重写（未发版），没有放宽框架校验；冒烟与剩下的边界见[sync-business 夹具](#sync-business-夹具的范围)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-10scriptsperfsync-businesssh-夹具与当前-core-不兼容已修复未发版)
- **2026-11-09：地形原两项去重问题（T01 / T02）**：原两项去重问题（T01 / T02）已修复、客户端岸线 C# 算法层已实现，随 v1.43.0 发布。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-09类型调度框架与-demo-生产接线已实现原两项去重问题)
- **2026-11-07：v1.42.1 修复复验**：RV-20261106-01/03/04 与 RV-20261107-02 复验关闭；RV-20261107-03 已修复（未发版）；Saga 偶发 fenced 见下一条。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-07v1421-修复复验)；[修复复验与续审](FIX-REVIEW-20261107.md)。
- **2026-11-07：Saga 协调器偶发误判“被别的写者隔离”**：错误码 10164 是服务端同一期望序号过滤还有提案在处理，不是被隔离；修复后原样重发并计 `Stats.FenceBusy`（未发版），Saga 整轮 15 档通过。[经过与证据](evidence/known-limits-history-20261113/README.md#saga-协调器偶发误判被别的写者隔离2026-11-07-发版前压测已定位并修复未发版)；[证据](evidence/saga-fence-busy-20261107/probe-results.txt)。
- **2026-11-07：新功能审查收尾**：RV-20261107-01（联盟迁移后宣战补发）与 RV-20261107-02（best_effort Stop）已修复（未发版）；联盟扫描剩下的边界见[联盟（alliance）](#联盟alliance)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-07新功能审查收尾)；[收尾报告](FEATURE-REVIEW-20261107.md)。
- **2026-11-06：cellworld 生命周期与编码（续审）**：RV-20261106-03（迟到 Leave 删新角色）与 RV-20261106-04（BaseState 重复标签）已修复（未发版），原有的剩余边界见[cellworld 的位置零头与入场序号](#cellworld-的位置零头与入场序号)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-06-续审cellworld-生命周期与编码)；[续审报告](FEATURE-REVIEW-20261106.md#续审跨区生命周期与状态编码)。
- **2026-11-06：新功能审查（v1.42.0）**：RV-20261106-01（best_effort 失败创建发布）与 RV-20261106-02（cellworld 收尾等待）已修复；2026-11-10 真实时钟用例统一整治，冻结报告重建 frozen 记录的实现缺陷已修复（未发版）。剩下的见[无缝世界：遗留](#地形--无缝世界遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#最新新功能审查2026-11-06v1420)；[审查报告](FEATURE-REVIEW-20261106.md)。
- **2026-11-06：Remote 独占实体的只读请求不再取写许可**：独占实体稳定持有期间读写都不碰 Mongo，`GetRemoteOwnership` 在稳定独占时答缓存的租约；仍有效的边界见 [Remote](#remote)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-06remote-独占实体的只读请求不再取写许可)
- **2026-11-05：`TestAllianceE2ERedisOutage` 偶发失败**：根因是用例的“短断开”没有上限、单实例锁实际扛得住的断开比文档写的短，用例已修，产品语义没改；仍有的限制见 [Remote](#remote)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-05testalliancee2eredisoutage-偶发失败)
- **2026-11-04：联盟服务冒烟里的两条观察**：停机窗口里路由 RPC 答成结果未知（改为 App 停机第一步统一关输入）与双实例起服建 effect 消费者超时（驱动同配置重发）均已修复；剩下的边界见 [App 停机关输入后的边界](#app-停机关输入后的边界)与 [Remote](#remote)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-04联盟服务冒烟里的两条观察)；CHANGELOG `[Unreleased]`。
- **2026-11-04：空认领（建盟被拒留下只有所有权记录的联盟号）**：已修（`9a1d9ee4`）：框架把它当作本实例拥有、业务上还不存在的实体；剩余边界见 [Remote](#remote) 与 [Remote：遗留](#remote遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-04空认领建盟被拒留下只有所有权记录的联盟号)
- **2026-11-04～06：实体服务 RPC 按方法串行与跨实例写超时**：按顺序键派发、实体锁错位、投影器 effect 顺序改按实体、outbox 写者自己先发、outbox 队头阻塞与 publish 分片均已修复；剩下的限制见 [Remote](#remote) 与 [Remote：遗留](#remote遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-0406实体服务-rpc-按方法串行与跨实例写超时)；CHANGELOG `[Unreleased]`。
- **2026-11-04：`alliance.transfer {count}` 在实体多时超过运维命令期限**：已修（`117e45ef`）：按 owner 分页枚举、期限不足时答 `more: true`；剩余边界见[联盟（alliance）](#联盟alliance)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-04alliancetransfer-count-在实体多时超过运维命令期限)
- **2026-11-04：mmorpg 生成工程的 `TestMMORPGRobotBusiness` 偶发失败**：根因在嵌入 TCP 接入（同一 SessionID 迟到完成的旧登录关掉新连接），已修复；见[同一 SessionID 的重连](#同一-sessionid-的重连)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-11-04mmorpg-生成工程的-testmmorpgrobotbusiness-偶发失败)
- **2026-10-31：实体创建与 TCP 快照审查**：RV-20261031-01 已修复（`aad320d2`，v1.33.1）；TCP 快照门禁失败是夹具假设，夹具已修正。剩下的边界见[新建实体落库与缺失 DAO 补空](#新建实体落库与缺失-dao-补空r15维护者-2026-10-30)与 [Lockstep：遗留](#lockstep遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#实体创建与-tcp-快照审查2026-10-31已修复)
- **2026-10-31：房间内同步与 self_sync 同进程**：覆盖冲突已修（`Provide` 返回 `ErrEntitySyncObserverConflict`），组同步由 `policy.GroupView` + `wiring/selfsync.Groups` 取代；剩余边界见[组同步与 self_sync](#组同步与-self_sync) 与[游戏接入验收](#游戏接入验收)。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-10-31房间内同步与-self_sync-同进程)
- **2026-10-29：Saga 清理守卫的 floor**：原登记的疑点已确认并修复（随 v1.31.1 发布）；剩下的边界见 [Saga](#saga) 的“清理守卫的 floor”。[经过与证据](evidence/known-limits-history-20261113/README.md#2026-10-29saga-清理守卫的-floor)
- **2026-10-28 / 10-30：上线前 Saga 审查**：RV-20261030-01 / 02 已修复（`f7d09022`，v1.33.1）；RV-20261028-01/02/03 在已删除的原型上修复。剩下的边界见 [Saga](#saga) 与 [Saga：遗留](#saga遗留)。[经过与证据](evidence/known-limits-history-20261113/README.md#上线前-saga-审查2026-10-28--10-30已修复)
- **2026-10-22：提交审查**：三项均已修复（v1.31.0），本节没有仍未修复的项。[经过与证据](evidence/known-limits-history-20261113/README.md#提交审查2026-10-22已修复)
