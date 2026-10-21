# 当前已知限制与验证边界

适用版本：v1.29.0 与 main 上未发版的改动（CHANGELOG `[Unreleased]`）。本文件只记**当前仍有效**的限制、维护者已接受的代价和验证边界；已修复问题的现象、根因、红绿证据与修法见 [CHANGELOG](../../CHANGELOG.md)、[docs/release](../release/) 与 Git 历史。审查发现的问题以 `RV-*` 登记在本文件开头（当前没有未解决的 RV 项，进度见 [COMMIT-REVIEW](COMMIT-REVIEW.md)）。拿不准是否仍有效的条目标“待核对”；v1.29 上没有证据的验证项写“v1.29 未验证”；2026-10-20 维护者批准的发版前补测已在 main `69fb668f`（v1.30.0 候选）上跑完（[夜间补测](../performance/NIGHT-20261020.md)），结论回填到各条与能力表，没跑或没结论的项留在文末 [v1.29 待补测](#v129-待补测)。

需要维护者做设计选择的事项汇总在 [DESIGN-TODO](DESIGN-TODO.md)；[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据；[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本文件是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零 bug 或 100% 行为覆盖；不确定的能力不写成已经支持。

## Nest 与实体定时器

### 按实体的持久定时器（D11）

`roost add timer --entity <Entity>` 与 core 驱动 `framework/nest/entitytimer`（[DESIGN-TODO D11](DESIGN-TODO.md#d11-按实体的持久定时器接入) 方案 B，`a2b4f4d8`、`5581deeb`）。离线语义：框架不追帧，离线期间到期的节点在加载后只触发一次，补算由业务按 `Fire` 参数自己做。用法见 [guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器)，契约见 [impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动)。

- **离线实体不准点**：驱动只索引已加载实例，未加载实体到期不唤醒，下次加载时补触发一次。维护者 2026-10-18 决定不做全局“离线也准点”驱动：必须准点的动作（如到点给离线玩家发邮件）由业务发 Nest 消息给目标实体或服务；不要求准点的在加载后的回调里自己追帧。
- **卸载与已入队 Tick 的窗口**：驱动投递前核对实例，但投递准入之后、执行之前实例被卸载时，Nest 按“发给未加载实体的消息”冷加载它并补触发一次（删档实体加载失败、不触发）。维护者 2026-10-18 决定接受，不给 Nest 加“只投给已加载实体”的发送选项。
- **构建了但没发布的实例**：实例在 `OnInitFinish`（发布之前）向驱动报告；发布失败且带期限时留下一个条目，到期时同 ID 已有别的实例则丢弃，没有实例则按加载中投递（Nest 冷加载并补触发）。条目只在到期前占内存，不重复投递。
- **Remote 托管与 noPersist 实体不支持**：add 时拒绝。迁移中的到期语义未定义，要支持需另行设计（D11 方案 C 方向）。
- **去掉 `timers` 条目后生成部分不自动删除**：需手工删 `*_component_gen.go`、节点类型、Tick handler 及 DAO / 实体里的两处字段；维护者 2026-10-18 决定维持现状。
- **装了定时器的服务多一个 Mod**：`wiring/entitytimer` Mod 让该服务的 `shutdown.total_timeout` 多 3s（每个 Mod 的保底）；维护者 2026-10-18 决定接受，不改 app 保底规则。game-demo 手写接线、不装这个 Mod。
- **game-demo 手写驱动在 `Service.Init` 才接上业务时钟**：生成工程的 Mod 在 Provide 阶段绑时钟；demo 的 `startWorldTimers` 在 `Service.Init`、`EnsureWorld` 之前调用，“World 第一次加载在驱动接上之后”靠 demo 自己的启动顺序（写闸门 `require_write_gate: true` 在 `EnsureWorld` 之后才发布），不是框架保证；更早加载的 World 按系统时钟记加载时刻（`time.logic_offset` 为 0 时两者相同）。
- **端到端用例用的是内存权威**：`TestEntityTimerGeneratedProjectEndToEnd` 以不落库 committer + 内存 loader 代替 DataEngine + Mongo；循环的其余计时分支（MaxWait 重读时钟、准入失败重试、Redeliver）由它与 entitytimer 包测试覆盖。真实 WAL / Mongo 下的定时器往返由 7 个游戏夹具的 SIGKILL 崩溃恢复与 MMORPG 的正常停机夹具覆盖，全部经驱动投递循环（`Driver.Start`）、不手发 Tick，见 [游戏接入验收](#游戏接入验收)。

### 慢派发 trace 的参数样本

慢派发 trace（handler 执行超过 200ms，可能在 handler 还在跑时由计时 goroutine 打印）的 `param_samples` 只打印按值装箱的标量（布尔、数字、字符串）与实现了 `fmt.GoStringer` 的参数，其余参数只给类型名（`<*pkg.T>`）：不按反射读 handler 可能正在改写的对象（[DESIGN-TODO“D20 与 D21 审查修复”](DESIGN-TODO.md#d20-与-d21-审查修复2026-10-21)）。要在 trace 里看到某个参数的内容，让它的类型实现 `GoString`，并保证只读构造后不变的字段（例如 `*heartbeat.Reg`）；按值传的结构体（是副本、没有竞争）也只给类型名，这是为了不在 trace 里区分“副本里有没有引用”。

### 实体心跳（D20 第 1 期）

`gameplay/heartbeat` + `wiring/heartbeat`（[DESIGN-TODO D20](DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制)，维护者 2026-10-21 定方案）。用法见 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)，契约见 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)。

- **业务接入**：生成器已接入（`roost add heartbeat` / `behavior` / `timedomain`，边界见下文“生成器接入心跳、行为与时间域”）；game-demo 已接入（D20 P3，手写接线、不装 Mod，缺省单目标投递，见[game-demo 的移动、怪物与场景时间](#game-demo-的移动怪物与场景时间d20-第-3-期)）。P2（MapBasic / Move 组件、actionflow 适配）已实施，见下一节；P4（压测改心跳驱动）代码已实施、缺省仍是全局循环且没有测量，见“压测工具的心跳驱动”一节；P5 的技能与 Buff 两项已实施，game-demo 刷怪已随 P3 做。
- **批量心跳投递未经正式压测，缺省关闭**（维护者 2026-10-21 决定：选项 C 先合入、缺省关闭）：缺省 `heartbeat.delivery: dispatch` 仍是每拍一条单目标 `Dispatch`，50 万次/秒约 4 核（[HB-STRUCTURES](../performance/HB-STRUCTURES-20261021.md)）。`batch`（D20 选项 C，[设计](FEATURE-2026-10-21-nest-batch-heartbeat.md)）64 拍一个 Nest 批量作业，每拍仍在实体自己的 Guard 作用域与事务里执行、Remote 托管实体仍单目标；2026-10-22 补测（[NEST-BATCH-HEARTBEAT-20261021 §3](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3-2026-10-22-补测正式对比3-轮中位数-最小最大)）：判据“业务 p99 不差于 X”不成立（同时段 X 0.078 / C 0.235ms，由批量作业占用 worker 的时间片决定），“进程 CPU 接近 Y”只在一半测量时段成立（C 比 X 省 42%～73%），所以缺省仍关闭，是否改缺省交维护者决定（报告 §3E）。宿主在测量中途换过一次“时段”（轻负载组 CPU 翻倍，原因尚未确认），Linux 上没有测。
- **批量方式下的观测口径不同**：批内目标不逐条记 `nest.dispatch.total` / `cost`、慢派发 trace 与 `nest.handler.lock_hold` 直方图（只计超过慢阈值的），按批记 `nest.dispatch.try_batch.*`。业务时间与 Frozen 派生值仍按拍各冻结一次（与单目标方式相同）。
- **（批量方式）冲突转单目标时可能有一次 worker 等锁**：同 ID 冲突的拍转成单目标作业排在冲突消息之后，执行时锁一定空闲；但若实体锁被 Nest 作业顺序之外的持有者占着（别的 handler 的 `Cast` 等），转成的单目标作业与普通 `Dispatch` 一样在 worker 上等这把锁，与 X 相同、不比 X 多。热点实体（同一实体高频业务消息）下的转换率没有测。
- **卸载窗口**：缺省单目标方式下，投递准入之后、执行之前被卸载的实体可能被 Nest 冷加载一次（与发给它的任何消息相同），这一拍在新实例上按过期丢弃；维护者决定 ⑨ 接受。批量方式只读已加载的实体，卸载后没有重载的按 `not_loaded` 丢弃、不冷加载；Remote 托管实体与冲突转成的单目标作业仍走普通 `Dispatch`，同上。
- **卡住兜底会多压消息**：在途 10s 未执行（同一登记连续卡住时 20s、40s、80s）就重投一次。拍没丢、只是 worker 排队超过阈值时，每个登记多出对数级份消息（只有一份执行回调，其余按 `duplicate` 丢弃）。-race 下 5 万实体 1s 档冒烟（每秒 5 万拍超过 -race 吞吐）实测到这种放大，所以 -race 冒烟降到 2000 实体。
- **Remote 托管实体每拍一次 Remote 写准入**：消息经 I/O 阶段 `PrepareRemoteWriteBatch`，比普通实体贵；档位宜低。非拥有方（`Unknown` 状态、权威指向别人）的拍被准入拒绝后按卡住兜底重试（10s 起翻倍）。只有独占拥有（`LocalOwned`）才执行。共享模式下的所有权迁移（生成工程的 Remote 流程）已在真实 `remoteentity.Manager` 下验证没有双推进（进程内权威替身与真实 Redis + Mongo 两套，见 [DESIGN-TODO“真实 Remote 共享模式切换验证”](DESIGN-TODO.md#真实-remote-共享模式切换验证2026-10-21)）；独占模式下不经共享的直接迁移仍只有模拟 Remote 写准入的用例（`heartbeat/remote_test.go`），真实依赖下的故障注入（Redis / Mongo 断开时切换）没有做。
- **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21）：共享模式下各写者实例都是 `Shared`，分不出谁该跳。共享是运行期状态、不是种类声明，所以登记时只拦得住“当时已在共享”的实例（`Register` / `Start` 返回 `ErrHeartbeatRemoteShared`）；登记后才进入共享的（拥有方 `EnterRemoteSharedMode`，或第二个写者加载时是 `Unknown`、第一拍准入才转 `Shared`），由下一拍在实体锁内停掉，计 `heartbeat.stopped_total{reason="remote_shared"}` 并打限频 Warn。Remote 没有所有权切换回调，检测靠心跳自己读实例状态（驱动到期时、handler 执行时）。边界：① 这一拍要走一次共享模式写准入（分布式锁），回调不执行；② 共享窗口短于登记的周期、期间没有到期的拍时，这个登记不会被停掉（期间也没有执行），回到独占后照常跳；全部登记停掉最多要一个最长档位的周期；③ 回到独占（`LeaveRemoteSharedMode`）后不自动恢复，业务需要时自己 `Start`（维护者 2026-10-21 决定）；④ 2026-10-21 已在真实 `remoteentity.Manager`（进程内权威替身）与真实 Redis 分布式锁 + Mongo 持久权威下验证停掉、计数与不自动恢复（`movement/shared_test.go`、`shared_integration_test.go`）。共享实体的周期逻辑由业务层驱动（例如负责它的服务按周期发 Nest 消息）。
- **快池准入满时的日志**：心跳本身按跳拍处理并限频 Warn，但 Nest 对每条被拒的异步投递另打一条 `async dispatch failed`（不限频，Nest 现有行为，本期不改 Nest）。
- **`Strict` 持久级别的心跳每拍都在实体 worker 上等落盘**：`WithDurability(nest.DurabilityStrict)` 的登记，每一拍的提交都按 strict 走，回调改了持久字段的拍要在实体 worker 上、持实体锁等提交持久化完成才返回（与任何 strict handler 相同），快池实体的高频心跳会把 worker 时间花在等 I/O 上；缺省 `Async`。用法见 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)。
- **组件漏调 `Host.OnDestroy` 时由驱动补上，但要等登记到期**：驱动在登记到期、发现实例已移除（`IsRemoved`）时撤销整个宿主，所以环与驱动对已移除实例的引用最多再保留一个最长档位的周期（60s 档最多 60s）；`dropped{reason="removed"}` 每个登记至多计一次。还没发布或被同 ID 另一实例顶替的不撤销（可能是暂时的），只丢这一拍。
- **`Reg.Close` 不随事务回滚**：与 `Register` 一样是生命周期操作，立即生效；回滚的 handler 里 Close 掉的登记不会回来，同一 handler 里同名新登记的 `Start` 随回滚撤销、登记本身保留（名字被占着，要再 Close 或复用）。换场景重建的调用放在 handler 最后一步（与 runner 的约束相同）。
- **装了心跳的服务多一个 Mod**：与 entitytimer 相同，`shutdown.total_timeout` 多 3s 保底。

### 移动与动作（D20 第 2 期）

`gameplay/movement`（`MapBasic`、`Move`、`MoveTo` / `Follow`）与 `actionflow.BeatRunner`（[DESIGN-TODO D20 P2](DESIGN-TODO.md#p2-实施结果2026-10-21)）。用法见 [guide/10-time §4B](../framework/guide/10-time.md#4b-移动与动作心跳驱动)，契约见 [impl/10-time §2C](../framework/impl/10-time.md#2c-移动与动作的心跳驱动)。

- **业务接入只有 game-demo**（P3，见[下文](#game-demo-的移动怪物与场景时间d20-第-3-期)）：压测的心跳驱动模式已有、但缺省仍是全局循环且未测（P4，见下一节），其余快池 tick 未迁移（P5）；框架没有巡逻、寻路组件（寻路在发起 `MoveTo` 之前由业务做，路径作参数；巡逻是 AI 策略）。只做了 3000 实体的端到端功能冒烟，**没有正式性能测量**；每拍的分配（`Move` 与 `MapBasic` 的回滚闭包、提交后记录、`MemFields` 的逆操作）未量。
- **runner 不随事务回滚：框架不处理，由业务保证**（维护者 2026-10-21）。`ActionRunner` / `MissionRunner` 是纯内存状态；handler 先做完全部可能失败的校验与修改，最后一步才调用 `Start` / `Enqueue` / `End` 等。违反时，回滚后 runner 里残留这次的变更：例如回滚的 handler 里启动的 MoveTo 仍是移动组的当前动作，而 `Move` 的设定与组的心跳都随回滚恢复——这个动作不被 tick，直到被取代、被结束，或业务调用 `BeatRunner.Sync` 后在下一拍以取消（`Replaced`）结束。心跳那一拍只在提交失败时回滚（拍回调不返回错误），此时动作对组件的修改回滚、runner 按 Tick 的结论的推进保留，同样不处理。
- **进出场景在提交后、实体锁外调用 `Space`**：`InterestSpace` 的 `Enter` / `Leave` 只在 Interest 登记侧登记一条命令（2026-10-21 单写者重构起不再取 Interest 的应用侧锁、不再 `Apply`，不等 AOI 批次），由政策阶段 / 应用线程按调用顺序执行；失败只经 `MapConfig.OnSpaceError` 报告（缺省 Warn），场景字段已提交。pipelined handler 的提交后回调在完成泵上执行，同实体的下一条消息可能早于 `Space.Enter` 登记，这期间 `QueueMove` 返回 `ErrInterestUnknown`（移动以 `Refused` 结束、`SetPos` 的调用方收到错误）；事务内门控的进出场景（`QueueEnter` / `QueueLeave`）未做（[重构方案 §8 第 6 项](REFACTOR-2026-10-21-interest-single-writer.md#8-待拍板均先按推荐实施)）。
- **`Published` 晚于同一拍里的事件**：快照在提交后、实体锁释放之后才发布；在拍的事务里观察到的事件（动作结束、OnEnded）早于它。要读到这一拍提交后的位置，等该实体的下一条消息（同实体消息要等前驱作业连同提交后回调整个结束）。
- **Remote 共享模式停心跳后组件不感知**：实体运行中进入共享模式时框架停掉动作组、任务、AI 的心跳与 `"map.save"`，回到独占不自动恢复；心跳没有“被停掉”的回调，动作与任务留在 runner 里不推进、AI 不决策、周期落库停。业务回到独占后恢复：动作组与任务心跳 `Behavior.Beat().Sync()`（2026-10-21 起含任务心跳，之前任务心跳没有恢复入口），AI 重新设置当前策略 `SetStrategy(c.Strategy())`，周期落库**没有单独入口**——`Enter` 在场景里返回 `ErrAlreadyInScene`，只能 `Leave` 再 `Enter`（AOI 提交后收到一次离开再进入）。**维护者 2026-10-21 决定先不处理**：Remote 托管实体理论上不应挂 Move 与心跳组件，`MapBasic.Sync` 不加，“Remote 实体禁止挂移动 / 心跳组件”作为后续方向记录（见 [DESIGN-TODO](DESIGN-TODO.md#维护者-2026-10-21-晚各期待拍板项的决定)）。共享期间启动需要周期 tick 的动作会以失败结束。已经真实 Remote 共享模式切换验证（独占 → 共享 → 独占、共享模式下迁移、卸载重载，进程内替身与真实 Redis + Mongo，`dispatch` / `batch` 两种投递方式，同上）。
- **不持久“在场景里”与“正在移动”**：实例重新加载后由业务按自己的 DAO 状态重新 `Enter`、重新发起移动动作；只从内存卸载不调用 `Leave`（`MapBasic.OnDestroy` 只发布“已不在”，跟随者以 `TargetLost` 收尾）。
- **跟随只看本进程、只看提交后的位置**：`LocateIn` 经实体管理器的只读查找，目标不在本进程内存（另一个进程的 Remote 实体、已卸载）即 `TargetLost`；读的是目标最近一次提交的快照，最多落后目标一拍。
- **一个实体多个组各自一个心跳登记**：参考 ssr，动作组之间不合并投递（决定 ③）；移动之外再有一个需要周期 tick 的组（例如技能），每拍就是两条消息。
- **地形判定按 0.5 格采样**：每进一个新格问一次 `Terrain`，斜穿两个被挡格的夹缝可能漏判；一拍的位移不设上限（速度 × `Elapsed`，`Elapsed` 上限 `max(1s, 档位)`），速度极大时采样数随距离增长。
- **`MemFields` 只给测试、示例与不持久的 NPC**：逆操作只在 `rollback=undo` 的 handler 里登记，`rollback=state` 不恢复；它不标同步脏（`OnChange` 由使用方接）。需要同步的实体用 DAO 字段实现 `Fields`。

### 压测工具的心跳驱动（D20 第 4 期）

sync-aoi `-move-driver=heartbeat`、nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat` 与 `gameplay/heartbeat/internal/bench` 的 §7 微基准（[HB-DRIVEN-LOAD-20261021](../performance/HB-DRIVEN-LOAD-20261021.md)，[DESIGN-TODO D20 P4](DESIGN-TODO.md#p4-实施结果2026-10-21)）。

- **没有任何测量**（2026-10-21 维护者要求暂停压测，同日宿主资源占用、冒烟也推迟）：心跳驱动与全局循环的对比、D19 多视野 Nest 模式在心跳驱动下的复测、驱动微基准与 10 / 25 / 50 万拍每秒的开环投递都待 v1.31 发版前压测（命令在报告 §4）。nest-game `TestNestGameLoad` 的心跳路径只编译、没有运行过；sync-aoi 只有 16 实体 0.6 秒的端到端用例。
- **缺省仍是全局循环**：两种工具不加开关时是原负载，已有报告的复跑命令不变；对比做完后由维护者定缺省。
- **心跳模式没有计划时刻**：sync-aoi 的 `Planned` 取这一拍消息冻结的业务时间、nest-game 的 `Completion` 从回调开始算，都不含驱动与快池的迟到（看 `beat_interval_ms` / `Beats.Interval` 与跳拍计数），与全局循环的“计划 → 客户端”不是同一口径。
- **sync-aoi 心跳模式的核对弱于全局循环**：位置取决于心跳时机，客户端只核对在实体的巡逻带上（全局循环逐项核对位置）；同一实体在一个 tick 里改两次时第二次不计时（照常核对）。
- **`TestBusyShortPoolSkipsBeatsAndResumes` 偶发失败已收口（2026-10-21）**：原用例按墙钟划窗口（占位消息自 Dispatch 起占住 250ms、放开后 100ms 内每个登记至少两拍），宿主负载下占位消息晚开始、窗口里只剩一拍（`-race -count=50`、烧满 CPU 时 2 次 `ran 1 beats after the pool was released, want it to resume`；驱动 Delivered = Executed、没有卡住）。判断为用例口径问题、不是产品缺陷；改为按事件推进后同负载 `-race -count=100` 全过（提交说明见 git log `test(heartbeat/bench, demo)`）。
- **负载形状不同**：心跳模式的移动者是固定集合、持续巡逻（改值速率按与全局循环相同折算），全局循环每 tick 换一批实体；两者 AOI 进出的空间分布不同。

### game-demo 的移动、怪物与场景时间（D20 第 3 期）

game-demo 手写接入心跳、移动、行为、状态与场景时间域（[DESIGN-TODO D20 P3](DESIGN-TODO.md#p3-实施结果2026-10-21)，[WALKTHROUGH](../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20)）。

- **投递方式固定为缺省的单目标 `Dispatch`**：demo 手写接线、不装 `wiring/heartbeat` Mod，所以不读 `heartbeat.delivery`；要试批量作业，在 `game/actor` 的 `heartbeat.Config` 里设 `Delivery: heartbeat.DeliveryBatch`（只能在驱动第一次 `Start` 之前）。
- **一个进程一张地图**：心跳驱动、`actor.WorldTime`（场景时间域）与 `actor.Ground`（地形）都是进程级的；成员在构造时绑定域（`BeatConfig.TimeDomain` 运行中不能换）。多场景的游戏要在成员进入场景时构造它的行为组件，或不挂域。
- **玩家不经 `LeaveScene` 离开**：断线只走复制桥的 `settle`（不进 Nest 事务）、卸载不调 `Leave`，所以 `SaveOnLeave` 在 demo 里不会触发，存档位置只靠 10 秒一次的周期落库（`actor.SavePeriod`）：崩溃或卸载最多丢 10 秒的走动；仍在内存里的玩家重连时留在原处。
- **寻路预算用完走直线**：`PathFind.Path` 最多访问 4096 个节点，开阔地上远处的斜向目标就会用完；用完时改走直线，`Move` 每进一格问一次地形，被挡停在最后一个可走的格（动作以 `Blocked` 失败），不绕路。证明不可达的目标直接拒绝。
- **`Join` 之前的移动 AOI 不跟**：`actor.InterestMoves` 对兴趣政策还不认识的实体（进场景事务已提交、`Scene.Join` 还没做或失败）忽略 `ErrInterestUnknown`，位置照写、AOI 不更新；`Join` 用进场景事务返回的位置，期间走的几步要等下一步才进 AOI。
- **只有怪物的 Buff 挂了心跳，没有技能 Runtime**：怪物的 `CombatComponent` 用 P5 的 `AttachBeat`（1Hz、场景时间），限时定身到点由它自己的心跳移除；demo 的实体上没有技能 `Runtime`（`game/skills` 只编译目录），所以没有用 `NewRuntimeBeat`。到期最多晚一个心跳周期（1 秒）加 tick 取整。怪物的 `CombatDao` / `StateDao` 不在 DaoManager 里（怪物不持久），只有 `rollback=undo` 的 handler 能回滚它们（demo 的 handler 都是 undo）。
- **怪物只巡逻**：没有追击、回巢、占地与战斗伤害；巡逻走直线（出生点附近边长 12 的正方形），角按地形裁剪。
- **刷怪一次一批**：上一批还在建时交来的批次丢弃，等下一拍；建一只要一次 `lifecycle.Create`、一次 `ShowSubject` 与一条场景 + 怪物的两实体 Nest 消息（场景实体是这一步的串行点，玩家的 `move_target` / `move` 也锁场景）。
- **场景视野的扇出上限 65536**：每个成员都挂在场景视野上（`MaxPlayersPerView` 由缺省 4096 调到 `sceneMaxMembers`）；超过时 `AttachView` 失败、只打 Warn，那个玩家收不到场景时间。
- **机器人冒烟没跑**：loadtest 场景已按新协议加 `move_target` / `move_arrive`，编译与单元测试通过；真实进程上的 `make dev-smoke` / `make loadtest` 因宿主资源占用推迟（2026-10-21），见 DESIGN-TODO P3。

### 行为控制（D21）

`actionflow.Behavior`、`Replacer`、跨组 `Conflict`、`EndAction`、计数冻结与 Start 即完成（[DESIGN-TODO D21](DESIGN-TODO.md#6-调研一--实施)）。用法见 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结)，契约见 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态)。

- **业务接入**：game-demo（D20 P3）：玩家的 `Behavior`、怪物的 `Behavior` + AI 心跳 + `state.Gate` + `BindStates`；生成器的 `roost add behavior [--ai] [--state]` 生成 `Behavior` + AI 心跳控制器 + 示例动作 / 策略 + `state` 的 Catalog 声明（边界见下文“生成器接入心跳、行为与时间域”）。仓库里实现 `Action` 的只有 `movement` 的两个动作、测试动作与生成的示例动作。
- **Behavior 与 runner 一样不随事务回滚**（D20 决定 3）：冻结计数、取代与抢占的结果、任务的推进都是内存状态，handler 回滚后保留，由业务把这些调用放在 handler 最后一步。
- **任务回调期间结束的动作，观察者可能先于任务收到**：动作结束经 `MissionRunner.OnActionEnd` 送任务，任务正在执行自己的回调时（例如任务 `Start` 里发起的动作启动即失败）这次送达延后到回调返回后，而观察者（AI）在当次就收到。“先任务后观察者”只在任务不处于自己的回调时成立。
- **动作的归属任务按“任务 runner 是否正在执行”判定**：任务回调同步引出的其他回调（例如任务回调里启动的动作启动即失败、`OnEnded` 送到观察者、观察者又发起动作）发起的动作也会记在当前任务名下，随任务结束被取消。
- **步骤时限的精度是任务心跳的周期**：到时最多晚一个 `MissionPlan.Period`（缺省 100ms，取档）才结束动作；超时的结束发生在任务自己的 Tick 回调里，所以观察者（AI）先收到这次动作结束、后收到可能随之而来的任务结束（上一条的例外）。没有配 `Beat` 的 Behavior 不检查时限（任务不被 tick）。
- **AI 心跳的错误只报告**：`"ai"` 登记挂不上（Remote 共享模式）时经 `OnError` 报告，AI 不再周期决策，回到独占后要业务重新设置当前策略（`SetStrategy(c.Strategy())`：2026-10-21 起同一策略不是替换、只重新对齐心跳；之前是静默的空操作，策略拒绝被替换时还返回 `ErrStrategyRejected`）或 `Recover` 触发对齐，`SetStrategy` 不返回挂不上的错误（经 `OnError`）；动作 / 任务结束照常送达。
- **任务结束可能先于引起它的动作结束送达观察者**：动作结束先交给任务，任务因此结束（例如 `PlanMission` 按 `OnFail` 结束）时，观察者在这次处理里先收到任务结束、再收到这个动作结束；由 `movement/shared_test.go` 场景一固定。
- **AI 事件是“下一拍”的、会丢**：事件缓冲到策略的下一拍，冻结期间、切换中、没有策略时直接丢弃；两拍之间超过 `MaxEvents`（缺省 64）丢最早的，不报告。没被取走的事件在那一拍结束时丢弃。要可靠送达的业务事实放进 DAO / state，事件只用来唤起决策。
- **决策轨迹不留存**：`Trace` 只是钩子，框架不保存轨迹；打开 `TraceNodes` 后每个节点每拍多一次接口调用，适合按实体采样，不要全服常开（没有测量）。
- **动作指标缺省关闭**：`BehaviorConfig.Metrics` 打开后每个动作开始、结束各写一次注册表（拼标签有分配），全服量级的开销没有测量。
- **`FreezeCancel` 只打断当前动作**：排队项保留，解冻后接着推进；要连排队一起丢弃，业务另调 `ClearQueue` / `EndAction`。
- **被抢占组的排队项在抢占方结束时推进，照常过跨组冲突**（[DESIGN-TODO D20 / D21 审查修复](DESIGN-TODO.md#d20-与-d21-审查修复2026-10-21)）：抢占时不推进（变异 D4 锁定），抢占方（那一个动作）结束时推进一次。抢占方被同组的另一个动作取代时，推进排在新动作启动之后，按跨组规则判定：新动作同样挡住它（`BlockIncoming`）时排队项以失败结束、接着试下一个，整队可能这样各收到一次失败；要“等这一类动作全部结束再走”的，由业务在抢占时自己 `ClearQueue` 或在结束回调里重新排队。
- **`Behavior.OnDestroy` 不在事务里**：卸载时结束在途动作与任务（`ErrOwnerDestroyed`，取消状态），动作的 `Cancel` 与观察者这时改的持久字段不落库；组件要按装配顺序调（AI 的 `Controller.Close` 在前，见 guide §4C）。漏调时在途动作没有结论、`actionflow.actions.running` 不减。直接经 `Runner()` 发起的动作不受 Close 后的拒绝约束，只是挂不上心跳（以 `ErrBeatRunnerClosed` 失败结束）。
- **冻结计数不随事务回滚**：直接调用的 `Freeze` 与 `Recover` 必须成对，handler 回滚后多出的一次 `Freeze` 要由业务撤销。经 `state.Gate` 的冻结每条规则至多持有一次，回滚后的不一致在下一次状态通知时纠正（见下条）。
- **状态观察者的副作用不随回滚，回滚也不通知**（`gameplay/state`）：handler 里改了状态、观察者已经做了事（例如门控冻结了组、打断了动作），随后回滚时状态撤销而副作用保留；观察者要到这个 `Set` 下一次任何改动或 `Refresh` 才按差收到纠正。被打断的动作不会恢复。业务约束同 runner：门控状态的改动放在 handler 最后一步，回滚后需要立即一致就调 `Refresh`。
- **Buff 到期才撤销派生状态**：`BindStates` 的状态随 buff 驱动（D20 P5；心跳驱动 `AttachBeat` 或别的驱动经 `DriveBuffs`）移除到期 buff 而失效，心跳驱动下最多晚一个心跳周期（`BeatConfig.Granularity` 取档，缺省 1 秒）加 tick 取整；没装驱动的实体只在业务自己调 `TickBuffs` 时失效。
- **派生状态的变化要数据拥有者调 `Changed`**：派生函数查询时现算，`Has` 总是正确，但观察者与同步脏位只在 `Changed`（或任何其他改动 / `Refresh`）时才更新；漏调时门控与客户端看到的是旧值。
- **持久状态改名等于丢弃旧数据**：持久文档的键是状态名，`RestorePersisted` 跳过不认识的名字（不报警），下一次整文档写回时消失；同步打包还没有接 `state`（game-demo 只用不持久的派生状态，经观察者镜像进 DAO 下发）；生成器只生成 Catalog 声明（`--state`），`Set` / `Gate` 与 `StateDao` 的接线由业务写。

### 技能与 Buff 的推进与驱动（D20 第 5 期）

用法见 [guide/10-time §4E](../framework/guide/10-time.md#4e-技能与-buff推进与驱动d20-第-5-期)，契约见 [impl/10-time §2G](../framework/impl/10-time.md#2g-技能与-buff-的推进与驱动)。

- **逐 tick 确定性只在追帧上限内成立**：落后超过 `MaxCatchUp`（`skill.Stepper` 缺省 1s，心跳驱动缺省 `max(1s, 周期)`，乘 max(倍率, 1)）时剩余部分一次 `Advance`，Host 的中间 tick 合并；`Stepper.Stats().Jumps` 计数并打限频 Warn，没有指标。回放要在同样的 tick 跳。
- **输入落在哪个 tick 取决于输入的时刻**（心跳驱动下是 handler 开始执行的时刻）：同一输入序列在不同驱动节奏、不同驱动方式下逐 tick 相同，前提是“每个输入落在同一 tick”；输入本身的 tick 随网络与排队变化，回放要记录输入的 tick。
- **Runtime 不随事务回滚、心跳启动随回滚作废**：handler 的 `Start` / `Input` 之后回滚，施法留在 Runtime 里、心跳没挂上，要等下一次输入或业务调 `Sync`（与 actionflow runner 同一规则，业务把输入放在最后一步）。拍里 `Advance` 的错误只经 `OnError` 报告，下一拍从 Runtime 实际到达的 tick 继续。
- **忙时每个 tick 推进一次 Host**：长时排程任务（例如 30 秒后的弹药回充）让 Runtime 保持“忙”，期间逐 tick 推进（50ms tick 下每秒 20 次空推进）；没有“跳到下一个到期点”的稀疏推进，那样 Host 的 tick 序列会随拍变化。
- **`Pending` 的“活动施法”一项没有强制点证据**：测试用的程序在施法活动期间总有排程任务，去掉这一项的变异没有变红；保留是保守的。
- **buff 到期精度是一个心跳周期**：缺省 1Hz，短控制（0.3 秒眩晕）最多多持续约 1 秒，要更准调小 `Granularity`；需要离线准点、结果要落档的长时 buff 用 entitytimer。
- **周期伤害、冷却、回复不在组件里**：`combatcomponent` 只持有 buff；业务的周期工作经 `Periodic` / `OnStep`（心跳驱动是 `BeatConfig.Periodic` / `OnBeat`）挂在同一个 buff 驱动上，答案变了要调 `SyncBuffDriver`。
- **Runtime 与持有它的实体**：`RuntimeBeat` 只在 Runtime 所属实体的锁内推进（`skill.Stepper` 只要求串行调用，不加锁）；Host 对其他实体（战斗单位）的效果按 Host 自己的规则进它们的事务（`StatusBridge.Committer` 等），驱动不替它加锁。game-demo 只有怪物接了 `AttachBeat`，没有技能 `RuntimeBeat`；生成器不生成战斗组件骨架。
- **不随事务的驱动在回滚后多醒一步**：`skill.Driver` 不是事务的一部分；心跳驱动的启停随事务，定步长循环这类驱动在施加限时 buff 的事务回滚后仍醒着，下一步没有工作才睡（`TestARolledBackBuffCostsANonTransactionalDriverOneStep`）。驱动要立即一致的话自己按事务提交后生效。
- **没有迁移现有的 Lockstep 房间与 `scripts/testdata` 夹具**：维护者 2026-10-21 只要求示例与测试（`skill.ExampleStepper_fixedStepLoop`、`TestStepperOnAFixedStepLoopMatchesTheJitteredDriver`、`TestBuffsOnAFixedStepLoop`）；`framework/sync` 的 Lockstep 房间与塔防 / 自走棋夹具的 `Advance(now)` 仍按自己的规则推进，没有用 `Stepper` / `DriveBuffs`。

### 业务时间取自 ctx 与场景时间域（D22）

业务时间一律取自 `fctx.Now()`（D22-1）与 `gameplay/timedomain`（场景时间域）。用法见 [guide/10-time §1A](../framework/guide/10-time.md#1a-业务时间一律取自-ctxd22-1)、[§4D](../framework/guide/10-time.md#4d-场景时间域变速)，契约见 [impl/10-time §2E](../framework/impl/10-time.md#2e-业务时间取自-ctxd22-1)、[§2F](../framework/impl/10-time.md#2f-场景时间域)。

- **“一律从 ctx 取”只有提示，没有强制**：glsvet 只看业务目录里 Nest handler 函数体内的 `app.BusinessClock` / `clock.Now` / `clock.UnixMilli`，不跟进 helper、包装函数或改名的变量；组件方法、业务库里现读业务钟不会被提示。约束靠 guide §1A 与生成文档。
- **冻结点在消息开始执行时，不在拿到实体锁时**：实体锁等待的时间算在冻结之后，热点实体上排队久的消息拿到的“现在”比实际执行早；同一条消息内一致，跨消息仍单调（按开始时刻）。
- **进程业务时钟与 Registry 业务时钟同源但不是同一个对象**：心跳与 entitytimer 驱动、`fctx.Now` 读进程业务时钟（App 启动时 `clock.SetOffset`），服务读 `app.BusinessClock(r)`。同一进程里起多个 App 且 `time.logic_offset` 不同时，进程级偏移是最后启动的那个（与 D22 之前 `fctx.Now` 的行为相同）。
- **时间域：成员看到改倍率比场景晚一个发布**：场景事务提交后（`AfterCommit`）才发布；之间开始的成员消息按旧倍率。域时间对读者单调（取已交出的最大值），代价是改倍率前后成员与客户端的域时间差“发布延迟 × 倍率”（微秒到毫秒级），时停期间一直差这一点。
- **时间域实例随场景实例**：场景卸载重载后是新的 `Domain`，已经挂在旧 `Domain` 上的成员心跳不会自动换；场景（副本）在有成员时不应卸载，或由业务在重载后让成员重新登记。`SetRate` 是否在所属实体锁内调用不校验（契约写在 API 注释）。
- **域要在构造时给，换场景靠重建**：`actionflow.BeatConfig.TimeDomain`、`ai.BeatConfig.TimeDomain` 在构造 `BeatRunner` / `Behavior` / `Controller` 时定下，运行中不能换域。成员换场景时先 `Controller.Close`、`Behavior.Close`（在途动作与任务以给定原因结束、心跳登记撤销、名字释放），再在同一心跳宿主上按新域重建（[guide/10-time §4D](../framework/guide/10-time.md#4d-场景时间域变速)）；不提供 `SetTimeDomain`：旧域里记下的时刻（上一拍的 `Elapsed` 基线、计划步骤时限的起点、`FreezeFor` 的期限）不能拿到新域里比较，而换场景本来就要结束场景里的动作。显式配了 `BehaviorConfig.Now` / `ControllerHooks.Now` 的以业务的为准，不取域时间。
- **技能 / Buff 的时间域在构造时给**（D20 P5 起已接域，原“没有自动接域”一条关闭）：心跳驱动的 `combatcomponent.BeatConfig.TimeDomain`、`RuntimeBeatConfig.TimeDomain` 在 `AttachBeat` / `NewRuntimeBeat` 时定下，运行中不能换域，换场景靠 `CloseBuffDriver` / `RuntimeBeat.Close` 后按新域重建（不走心跳的驱动经 `StepperConfig.Now` / `Rate` 自己传域时间）。buff 的 `DueTick` 是按域时间算的绝对 tick：实体换到另一个域（或离开变速场景）时，已有 buff 的剩余时长按新域的 tick 算，2 倍速场景里攒下的“领先”会让 buff 在常速域里多持续一段，框架不换算。
- **客户端外推只有 C# 的计算类**：game-demo 的场景已挂时间域（D20 P3，单场景、`nopersist,sync`、经场景视野下发给每个成员）；生成器的 `roost add timedomain` 只生成场景侧的字段与组件，成员怎样拿到所在场景的域由业务写；C# 客户端有 `Roost.Client.SceneTimeDomain`（同一公式、单调托底）；SDK 不解码业务载荷，三个字段由游戏自己的解码器读出交给它；“估计的服务端业务时间”由游戏提供（SDK 没有对时）；Unity 适配层没有接。
- **生成工程消费夹具未实跑**：`scripts/testdata/*` 的持久化恢复用例（需要真实 Mongo）用 `TimerDriver.SetClock` 控制时间，D22-1 后 Tick 消息不带参数、Tick 用驱动时钟，按设计仍应成立，本轮没有在真实依赖上跑。

### 生成器接入心跳、行为与时间域

`roost add heartbeat` / `roost add behavior [--ai] [--state]` / `roost add timedomain`（[DESIGN-TODO D20“生成器接入”](DESIGN-TODO.md#生成器接入实施结果2026-10-21)，用法见 [CODEGEN_REFERENCE](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-heartbeat--behavior--timedomain实体心跳行为与时间域)）。

- **按需装配，不进缺省 Mod 集**：`heartbeat` Mod 只在 add 命令（或 `roost add mod heartbeat`）之后才装在那个服务上。实体侧零开销（没有登记、登记都停着不占调度，生成的示例登记是停止状态、AI 不设策略不跳），进程侧装上之后驱动空转也每 10ms 走一格、停机预算多 3s。是否改成“装了 nest 的服务缺省装”待维护者决定。
- **工程只有一个驱动变量**：`game/heartbeat.Driver`，依据是生成工程每个进程只跑一个服务；同一个进程里起两个装了 heartbeat Mod 的 App（测试）时它们共用一个驱动：后一次 `Bind` 覆盖前一次的 Nest 客户端（`Bind` 允许重复调用），拍都投到后接上的那个 Nest。驱动的 `Name`（日志 / 指标标签）是工程名，不区分服务。
- **示例 AI 的 `EndActions` 结束全部组的动作**：生成的 `ai.ControllerHooks.EndActions` 是 `Behavior.EndAllAction(true, reason)`（切换策略时清场），写在生成部分，业务要换成“只结束某些组”需要另写 Controller，生成器不提供钩子。
- **成员挂场景时间域由业务写**：生成的 `timeDomain()` 返回 nil；域在构造时定下、运行中不能换（D22 边界），实体换场景要 `Close` 旧的行为组件后重建（`Behavior.Close` / `Controller.Close`，见上文“域要在构造时给”），生成器没有生成重建入口；生成的 `BehaviorComponent.OnDestroy` 已调 `Controller.Close` 与 `Behavior.OnDestroy`。
- **时间域字段缺省 `nopersist,sync`**：随进程结束的副本场景；要持久的场景手工改成 `persist`（DAO 定义归业务）。三个字段名固定，已有同名字段只有一部分或类型不同时 add 拒绝。
- **去掉 `heartbeats` / `time_domains` 条目后生成部分不自动删除**：与 `timers` 相同，需要手工删 `*_component_gen.go` 与实体里的组件字段；服务 `mods` 里的 `heartbeat` 也不自动去掉。
- **端到端用的是内存权威**：`scripts/test-heartbeat-generated.sh` 以不落库 committer 代替 DataEngine，只覆盖新建实体（不覆盖卸载重载后重新登记、Remote 托管实体、真实 WAL / Mongo）；脚本不在 pretag 与 `go test ./...` 里，要手工运行。
- **`heartbeat.*` 配置段跟着 Mod 的声明走**：生成配置写 `heartbeat.delivery: dispatch`（选项 C 的开关，缺省单目标 Dispatch）；配置段、doctor 与 `--check-config` 用 `kitconfig_gen.go` 里的快照（`wiring/internal/configschemagen` 的 `heartbeat` 组），Mod 加新键后要重新 `go generate ./codegen/internal/roost`（`TestKitConfigSchemasMatchKitDeclarations` 守）。已有配置里改过的 `heartbeat:` 段不被改写。

### Skill Runtime

不参与 Nest 回滚；Host 副作用和确定性由业务契约保证。Host 能力、重入、阻塞、恢复和回放测试仍待补。

## DataEngine、Remote 与 Saga

### 删档后同 ID 重建（D10）

GetOrCreate 路径已守住（维护者 2026-10-17 决定 [D10](DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，`572d87c2`）：冷加载对删过档返回 `entity.ErrAuthorityEntityDeleted`（不满足 not found），生成 GetOrCreate 不重建，发往该 ID 的 Nest 请求回复 `entity_deleted = 3`。

- **约束（直接 Create）**：handler 内或 lifecycle `Create` 不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID，也不要对存了但没加载的 ID 直接 Create；`EntityManager` 的 ID 生成器同样必须永不重复。违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来。直接 Create 的保护按维护者决定不做。
- **删档审计**：“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记。
- 回归：`TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`。

**删档后同 ID 重建：违反约束或修复前写下的冲突记录**。这类记录不会被跳过或放宽：启动投影恢复报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来；框架不自动删记录、不跳过，也不提供“跳过一条记录”的入口。人工处置（只在确认是这类记录时）：

1. 停进程，保留 WAL 目录与 Mongo 备份。
2. 按错误给出的事务 ID 与 `<库>/<集合>/<_id>`，在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份只读副本留证（不推进 checkpoint、不跳过记录）。
3. 线上尚未部署（维护者 2026-10-06）：停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本。上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码。见[维护手册 §4](README.md#4-故障处理)。

### 运行期改档与单实体运维（D16）

框架只提供与业务字段无关的 `entity.kick`（踢下线）与 `entity.unload`（排空投影后卸载），**不提供改档与恢复**（维护者 2026-10-20 决定 C，[DESIGN-TODO D16](DESIGN-TODO.md#d16-单个实体--玩家的运维入口)，用法见 [guide/03 §6](../framework/guide/03-dataengine.md#单实体运维命令d16)）。`entity.unload` 之后任何访问都会立刻重新加载，不能当作改库的窗口。运行期直接改权威集合的结果按设计是确定的：只改字段时下一次投影覆盖改动；改 `_version`（恢复旧备份）时投影 fatal、进程 fence、重启起不来，直到人工处理（[维护手册 §4](README.md#4-故障处理)）。在线改档写成业务 GM handler，整档恢复走停服排空后的离线写回（[guide/03 §3](../framework/guide/03-dataengine.md#3-投影与幂等)）。

### DAO 持久键（D13）

定义里字段的 `bson:"x"` 就是持久键（缺省 snake_case），库里有、DAO 未声明的键不读入，下一次整文档写回即删除（维护者 2026-10-19，[DESIGN-TODO D13](DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）。

- **业务约束**（[SCHEMA-MIGRATION §1](../framework/SCHEMA-MIGRATION.md)）：改名 / 删字段前要么保留原 `bson` 名，要么先离线迁移再提升 schema，否则旧数据不再加载、写回即丢失。
- **剩余风险**：`roost generate` 与 `RestorePersisted` 的 WARN 不阻断，忽略告警时旧值丢失由业务约束承担；单字段补丁不碰旧键，它们留在库里但不再被读取，清理要一次性离线迁移；生成期基线只在工作树里，删掉生成物或换机器重新生成看不到上一次的形状，只剩加载期 WARN。

### Remote

- **Remote WAL级Rejected处理**（`38b710a2`）：Durability 1/2/3 的正式链路不产生 Remote Rejected，strict / pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`；tracker 的 Rejected 收尾只剩 Durability 0 来源。守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore / MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict / pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`）。
- **Remote 请求 struct 与 Sender**：handler 参数用带 `remote:"..."` 字段的请求 struct 时，struct 必须留在 handler 包（访问器生成在这里），sender / syncsender 子包又看不见它，所以这种 handler 生成失败（生成器给专门报错、不建议移走类型）。维护者 2026-10-16 决定先写清限制、不改生成结构；需要时再设计让 sender 携带这种 struct，或允许单个 handler 不生成 sender。

### Saga

- **原生步骤的多进程分片**：准入与转交由业务写（框架提供 `WithAdmit` 与 `RunHandedOver`，生成的 `RegisterSteps` 注释点名要补），见 [DESIGN-TODO D12](DESIGN-TODO.md#d12-saga-原生实体步骤与事务性-effect-消费者的生成支持)。
- **原生步骤单进程容量**：第一瓶颈是 DataEngine 投影器——带回执 / effect 的记录（`projector_replay.go` `isLocalBatchRecord`）逐条一次投影事务，checkpoint 已与普通记录按 `CheckpointRecords` / `CheckpointInterval` 合并；正向每笔 2 条、全补偿 3 条。等回执在后台（`StepConsumerConfig.ReceiptWaiters`），代价是同 durable 上同一实体的前后命令可能撞实体屏障（交还租约、退避 nak）。维护者 2026-10-18 决定 A（接受并文档化，量大按 sid 分片多进程、原生步骤加 `WithAdmit`）；第三档 B（合批带回执 / effect 的记录）与 C（分盘）不做；checkpoint 缺省间隔保持 20 ms（维护者 2026-10-19 方案 A），更高吞吐可调 `dataengine.projection.checkpoint_interval`，代价是崩溃后重放窗口与未确认额度变长（重放仍幂等）。容量数字见下方能力表（2026-10-20 夜间 15 分钟复测）与 [guide/06 §5](../framework/guide/06-saga.md#5-接入与运维)。
- **发起方查结果**：生成的 `Lookup` / `saga.Engine.GetByBusinessKey` 按 `(Type, BusinessKey)` 查；终态通知没有默认实现，需要时由最后一步 / 补偿步骤自己发 effect（SAGA.md）。

## Sync、玩家接入与 Lockstep

### 自身视图（self_sync）与 Sync 慢消费者规则

`access.player.self_sync: true` 让玩家经 Entity Sync 收到自己的 Player（DESIGN-TODO D6，`policy.SelfView` + `wiring/selfsync`）；推送在活着的连接上被拒即关闭该玩家连接，进程驻留预算耗尽整轮重试（维护者批准“A + A'”，[impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)）。编号沿用 `67807950` / `cf6ad085` 之后的口径：

1. 一个 `SubjectSyncState` 只登记在一个 Manager，第二次登记返回 `entitysync.ErrSubjectRegisteredElsewhere`（2026-10-20 起）。game-demo 打开 self_sync 时启动在 `service game init` 失败，自建 Manager 在登记时被拒：该组合本身仍不支持。
2. 一帧上限取接入层上限（嵌入 TCP 读 `player_access.tcp.max_payload_bytes`；独立 Gate 由 `IngressMod.WithSelfSync` 交给 Game Ingress 的 `Config.MaxPayloadBytes`；`selfsync.Config.MaxFrameBytes` 只能再收紧）。**独立 Gate 下 Game 与 Gate 两个进程的 `MaxPayloadBytes` 相等靠部署保证**（Gate 侧 `GateMod.Init` 只检查本进程 TCP 与内部上限相同）。维护者 2026-10-20 决定暂不加跨进程检查：要检查就得在握手时互相报上限，是协议改动；继续由部署保证两边配置相同。
3. 玩家实体的帧在 Manager 内编码失败（单个实体包超过 `frame.Limits` 硬上限、或超过第 2 条的上限）时 Manager 不经传输丢会话，连接不关，玩家在线却没有自身视图直到重新登录；由 `entitysync.self_view.stranded_players` 暴露，game-demo 场景同样。维护者 2026-10-17 决定 B：不关连接，靠指标告警。
4. Game 进程出站驻留预算（Game Ingress `outbound.resident_bytes`）持续耗尽时 Sync 每轮在第一个被拒会话处整轮重试：排在后面的会话本轮不推送，已准入的会话下一轮改收全量（`requireSnapshotsAfterRetry`），带宽放大、延迟上升；发不出去的绑定在 `outbound.max_age` 到期时被关闭、释放预算；业务提交不受影响。嵌入 TCP 的进程预算同一规则，但没有 `max_age`（见下节）。按发送能力配预算并看 `player.sync.push_refused.total{outcome="retried"}`；需要时可让 Manager 在 RetryLater 时跳过被拒会话。
5. Gate 侧自己的驻留预算耗尽仍关闭连接，没有 Game 侧的整轮重试例外；需要时 Gate 回 NotAdmitted、由 Game 侧按预算例外处理。
6. 不提供 C# 的 subject 载荷解码样例。
7. 嵌入 TCP + self_sync 重档 15 分钟（2026-10-20 夜间，[夜间补测](../performance/NIGHT-20261020.md) §2）通过：1000 玩家每秒约 1000 帧自身视图，`push_refused` retried / closed 0、滞留玩家 0，提交→客户端 P99 1.43ms。第 4 条的整轮重试放大在该负载下没有被触发，**没有测到不等于不存在**（要测需把预算压到触发点）；独立 Gate + self_sync 的重档夹具没有。

**self_sync 默认 packer 的代价**：开关打开且 marker 没写 `subjectPacker=` 时生成器生成 `player_sync_packer_gen.go`，标了 sync 的字段默认全部对玩家本人可见，需要裁剪的写自己的 packer；marker 写了生成名 `NewPlayerSyncPacker` 时按业务 packer 处理、不生成；默认 packer 只发一个 DAO，多 DAO 的 Player 仍要自己写（[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)）。

### 嵌入 TCP 出站

每连接有界出站队列 + 唯一写协程：上限 1024 帧（维护者 2026-10-16 决定）与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计，不开放配置键；停读客户端被关闭、不拖住场景 Sync Flush / Lockstep Tick。2026-10-20 起有全进程驻留预算 `player_access.tcp.outbound.resident_bytes`（`cf6ad085`，与独立 Gate `outbound.resident_bytes` 同口径），放不下的一帧当场被拒（`gateway.ErrResidentBudgetExhausted`，计 `player_tcp_outbound_budget_exhausted_total`），不关闭连接、不等预算；Entity Sync 当整轮重试，PB 推送返回错误由业务处理。说明见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)。

维护者 2026-10-20 决定（均按推荐）：

- **Lockstep 实时帧被预算拒**：按数据报丢失处理，不断开连接，靠冗余帧或追帧补回（A）；追帧页被拒计一次追帧失败。
- **读循环回包与心跳 ACK**：计入预算但不拒绝（每连接至多一帧在等），驻留量最多可超出预算 `max_connections × (max_payload_bytes + 16)`（A）；读循环直写快路径的那一帧不计。
- **驻留字节 Gauge 与按帧龄提前关闭停读连接**：先不做，等重档压测看 `player_tcp_outbound_budget_exhausted_total` 再定。

剩余边界：

- 缺省 256 MiB 是按 Gate 缺省 128 MiB / 4096 连接折算到 10000 连接取整。2026-10-20 夜间嵌入 TCP 重档 15 分钟（1000 连接、场景 Sync 4198 万条 delta、每秒 1 万条回包，[夜间补测](../performance/NIGHT-20261020.md) §2）`player_tcp_outbound_budget_exhausted_total` 增量 0、溢出 / 推送关连接 / 写错误全 0，正常负载下不触发；停读客户端或更高推送量下是否够用仍未测（没有驻留 Gauge，峰值取不到）。上面“驻留字节 Gauge / 按帧龄关闭”的决定所等的数据就是这一份：本负载下拒绝为 0，不构成要做的证据。
- 停读连接占着的预算要等它撞上每连接上限被关或 `write_timeout`（默认 5s）写超时才归还，期间健康连接的推送可能被拒（Sync 延迟上升，Lockstep 实时帧靠冗余补）；没有 Gate 那样的 `max_age`。
- 只有拒绝计数，没有驻留字节 Gauge。
- 推送入队多一次 payload 复制，每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。
- 证据只有包内回归（含关连接与写协程并发退出时恰好归还一次）、自身视图与 Lockstep 的真实回环 TCP 集成用例与生成 demo 场景用例。
- **生成 demo `TestAStalledOldConnectionDoesNotResetTheLiveClient` 偶发 `outbound queue full`（2026-10-21 收口）**：`pushPlayerKind` 只有在没有任何连接接住这一帧时才返回错误（`infra/network/gateway/tcp.go` 的 `delivered > 0 && !refused`），所以这条失败说明活连接也溢出了——用例紧循环推 512KiB 填充帧，活连接的写协程 / 读端在负载下被晚调度时，它的出站队列（几帧）与停读连接在同一次推送里一起满。判断为用例推送节奏问题、不是产品缺陷；原日志被截断，本机负载 13～16 下 290 次未复现，按代码推断。改为每推一帧等活连接读到再推下一帧；变异“任一连接失败就整体报错”仍让用例变红。

### Lockstep

- **已修复（2026-10-20）：`TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted` 在全仓 `go test ./...` 下偶发失败**（`tcp_test.go:513`：`Tick after the budget was returned = … frame of 16455 bytes, 261344 of 262144 resident`）。负载下（`-race -count=200 -cpu=1,2,8`，同时循环跑 nest / network / dataengine 的 race 测试）复现 97/600。根因是产品缺陷：嵌入 TCP 的 `serveConnection` 先发布会话关闭事件（`unregisterSession` 的 defer 在 `closeConnectionSession` 之前执行），再关闭 socket、等写协程退出，而连接占的出站驻留在写协程退出时才归还（`releaseOutbound`），与 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)“等写协程退出，再……发布关闭事件”不符；用例收到 filler 的关闭事件就 Tick，正好落在这个窗口里。修法与红绿见 CHANGELOG `[Unreleased]`“嵌入 TCP 会话关闭事件改在写协程退出之后发布”。同时把该用例与 `wiring/selfsync` 预算用例的“停一会儿再探”换成确定的占满方式（一帧 16 MiB、预算恰好一帧）。修后同一负载 `-count=500` 0/1500。
- **Lockstep历史**：默认自动保留 3600 帧（60Hz 60 秒 / 30Hz 120 秒，D1 方案 C，`a6376934`）。越界按 `MatchConfig.Recovery` 区分（D1 方案 B，维护者 2026-10-20 批准，P1～P5 已实施，[设计](FEATURE-2026-10-20-lockstep-snapshot-recovery.md)）：配了 `Snapshots.Provider`（零值即 auto）或显式 snapshot 时，越界改发业务快照、客户端恢复后续帧，只有快照路线最终失败（取不到、过大、钉住过期）才发 C9 v2 通知让客户端终止；catchup（没配快照来源时的缺省）仍是越界明确失败、通知客户端终止、不自动重同步。快照内容来自业务的服务器参考模拟，框架不提供默认模拟、不替业务持久化检查点。本地 20000 帧周期性快照恢复与默认窗口长局有界已验证；按业务帧率、断线时长与快照大小核对窗口和预算未做。见 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)、[SYNC-OPTIMIZATION](../framework/SYNC-OPTIMIZATION.md)。
- **Lockstep TCP 追帧**（`640e9fac`，维护者 2026-10-16 方向 A）：`lockstep.TCPSender` 实时帧 `SendDatagram` 只准入，追帧页 `SendReliable` 经 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出，追帧随链路放慢、积压只有在途一页，`CatchupSendTimeout` 生效。等写出只在 Match 的 `launchSend` goroutine 上；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。限速链路上：快照分块与快照之后的续帧页经同一等写出机制在 toxiproxy 256 KB/s、64 KB/s 下演练过（2026-10-20，main 代码，1 MiB 与 4 MiB 都恢复、连接不断、耗时与“大小 / 速率”对应，[演练](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）；纯追帧页 2026-10-20 夜间也在 toxiproxy 256 KB/s、64 KB/s 下演练过（[夜间补测](../performance/NIGHT-20261020.md) §10）：客户端回到头部的时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断，64 KB/s 下一页超过 2s 发送时限后重发一次；服务端追帧结束早于客户端收完（socket 发送缓冲），同快照演练。见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)。
- **Lockstep 追帧链路背压（独立 Gate）**：维护者 2026-10-20 决定暂不做。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，nil 只表示 Game 本地准入：Game 侧队列满返回 `ErrReliableBackpressure`（每 Tick 计一次追帧失败，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时关闭连接；追帧与快照都不随链路放慢。快照按 `UnpacedBytesPerTick`（缺省 16 KiB/Tick，30Hz 约 492 KB/s）固定节流；2026-10-20 toxiproxy 实测（30Hz、缺省参数，[演练 §3](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md#3-独立-gate63-风险-1-实测成立)）：64 KB/s × 1 MiB、256 KB/s × 4 MiB 每次连接都在同一位置被 Gate 断开、重连后从头重传，三次都未恢复；256 KB/s × 1 MiB 恢复（低于预算的超出部分被缓冲吸收）。是否断开取决于快照大小、每 Tick 预算与链路速度三者；断开发生在 Gate 写 socket 的期限到期（`writev ... i/o timeout`，`gate.channel.failure_total{phase="socket"}`），不是 `max_age` 积压规则，具体是 5s `write_timeout` 还是单帧发送预算先到期**尚未确认**（按代码缺省配置下单帧预算 ≤ `outbound.send_timeout` 250ms 更早，日志未区分）。2026-10-20 起恢复中被 Detach 计 `lockstep.snapshot.failed.total{reason="detached"}`，这个循环可见。配置建议见[设计 §6.3](FEATURE-2026-10-20-lockstep-snapshot-recovery.md#63-独立-gate只准入时的行为与风险)。纯追帧页在独立 Gate 慢链路上的影响未实测。要做到随链路放慢，Gate 需要在写出 socket 后回执给 Game，这是协议改动，等有具体游戏使用“Lockstep + 独立 Gate”时再做。生成 game-demo 在独立 Gate 部署（`accessplayertcp.NewIngressMod`）下拿不到 `TCPRuntime`，战斗的追帧页同样只准入（`battleLinks`）。
- **Lockstep 快照恢复的剩余边界**（2026-10-20）：① MOBA 持久对局 `Room` 已接快照来源（2026-10-20，`RecoverRoomWithSnapshots` + 已提交门 `MatchConfig.CommittedFrame`，[MOBA §5.7](../framework/MOBA-INTEGRATION.md#57-重连宿主代际与交接)），替身权威下有 KCP 与 Nest 两条回归，真实 strict WAL / Mongo 下有 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复的演练（`TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL`，下行是记录器、不经 KCP）；真实 WAL 的提交延迟分布、宿主交接与快照恢复同时发生、宿主与权威分属不同进程时的快照恢复未测；已提交门要求宿主提交落后切帧不超过 `RedundancyDepth - 1` 帧，否则恢复中的会话一直经可靠通道逐页跟随、不切回实时（Room 每 Tick 同步提交，不受影响）；② C# 的 `SHA256.Create` 与 restore 路径只在本机 .NET 10 测试过，Unity 编辑器 / IL2CPP 未实测；③ 服务端“写出”只到本机 socket 发送缓冲（嵌入 TCP）或 Game 本地准入（独立 Gate）：服务端认为恢复结束时慢链路客户端可能还差 1 MB 以上，之后的实时帧排在后面，恢复后头几秒延迟等于排空时间（演练推断，未单独测量缓冲）；④ 快照不续传：断开重连后从头重传，独立 Gate 慢链路上会循环（上条）；⑤ Provider 不响应 ctx 时 `FetchTimeout` 后可能再发起一次，“每局至多一次在途”不成立；⑥ 宿主手动 `TrimHistory` 越过钉住点时快照作废、按失败计，但不计 `failed` 指标；⑦ 真实移动网络、跨机与目标负载下的快照恢复、快照大小分布与窗口取值未测；⑧ 客户端上传快照（多数一致）、增量 / 压缩快照、检查点持久化与宿主崩溃续赛未做（设计 §13）；game-demo 战斗未接（§12 第 13 条）。
- **game-demo 战斗经 `lockstep.TCPSender`**（维护者 2026-10-20 决定）：生成 demo 的战斗房间（`demo/internal/service/game/battle.go.tmpl`）两条通道改经 `TCPSender`，嵌入 TCP（生成默认）下追帧页经 `TCPRuntime.PushLockstepSessionWritten` 等写出，与上面嵌入 TCP 追帧同一规则；回归 `TestABattleCatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt`（生成工程 `go test ./internal/service/game`）。线格式随之变化：10102 推送的是 Lockstep 包本身（RS v2 Lockstep 标志、不带 PB 包装与 battle id），按连接（接入层 SessionID）寻址；座位在它第一条 BattleInput 到达的连接上加入，没说过话的座位收不到帧，晚加入的座位靠追帧补齐。一条连接同时只打一场战斗（帧不带 battle id）。限速链路与真实进程下的 demo 战斗未演练。

### 会话 Hold / Ready

2026-10-21 起 Hold 之后撤到一个订阅都不剩，Ready 后补发一个不含对象的 Full（学习分支 `learn/main-review-20261021` 第 8 条，[ENTITY_SYNC §会话](../framework/ENTITY_SYNC.md#会话)）。剩余边界：

- **每次 Hold → Ready 都按全量重发仍在的对象**：没有“只停发布、保留基线与待发 Remove”的暂停原语（第 8 条的方向 B，未做，待维护者决定）。按周期刷新权限、每轮都 Hold 的业务（SLG §7.5 / §7.7）每个周期每个对象重发一次全量；SLG 文档建议只在权限读失败与重连时 Hold。
- **仍有订阅但一时没有可发的快照时，新 epoch 的 Full 会推迟**：例如 Ready 时剩下的订阅指向已卸载、等待重载的实体，或被冷创建预算推到后面的窗口；Full 发出之前接收端还留着上一 epoch 的对象（包括 held 期间撤掉的）。只在一个订阅都不剩时才补空 Full（读代码推断，未写用例）。

### 广播与 AOI

- **SyncBus投递**：有限次数、保留期限及容量，不保证绝不丢；定位为默认可丢广播（维护者 2026-10-15）。需要最终一致的业务自建快照 / 对账闭环，不能仅测重试成功；兜底写法见 [guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)；框架级对账为下一阶段候选。
- **policy Drain**：等待 retry 和已承担事实；永久拒绝仍需修关系或 ctx 超时，Drain 不放弃条目，超时返回 `*DrainStalledError`（列出卡住条目、原因与等待时长）并计数（D3，维护者 2026-10-15 决定 A，`b86a1286`）。“放弃出口”（D3 方案 B）需先梳理永久拒绝的判定来源。
- **Interest 单写者（2026-10-21 重构，[方案与实施结果](REFACTOR-2026-10-21-interest-single-writer.md)）的边界**：① 全部写入口只登记，效果在下一次结算可见（同步模式下一次 Flush、异步模式应用线程走到之后的 Flush），登记之后立即读 `Visible` / `Seen` 会先执行日志（诊断读取应用侧锁，不在快池上调用）；`Apply()` 同样取应用侧锁，只给测试 / 工具 / 离线驱动。② 未确认的移动被同一目标之后的 `Hide` 越过时整条作废：跟随该目标的视野停在 Hide 前已应用的位置，而不是那条移动的目标（随后的 `Show` 会把视野带到新位置）；已确认的移动不受影响。③ `Leave` / `Unwatch` / `Hide` 之后同一次结算里又被持有的 pair 先退订再订阅：`Hide` → `Show` 落在同一周期时客户端多收一次全量（换来会话重开 / 目标重新登记后不丢订阅）。④ 命令（`Enter`、视野操作等）不计入 `MaxQueuedFacts`，只有门控事实受限；Flush 长期停摆时命令日志无界增长（与重构前视野登记表的变更记录相同）。⑤ ApplySync 一次政策阶段的耗时预算固定为同步周期的一半，没有配置项；积压超过预算时顺延。⑥ sync-aoi 的 direct 基线不再是“AOI 全部在压测线程”：政策阶段可能先于压测线程的 `Apply()` 执行一部分日志。⑦ **没有性能测量**（维护者 2026-10-21：修完再统一压测），对照方法见方案 §6。
- **AOICluster 容量抖动**：Cluster 的 `MaxVisible` 每次移动后严格等于候选内最近 N 个，容量层没有滞回，两个目标在第 N 名附近来回时每次移动都 Leave/Enter（`BenchmarkClusterFlushRandomWalkCapped` Enter +15.6%）。维护者 2026-10-17 决定不加滞回：抖动只在候选数超过 N 时出现，不设 `MaxVisible` 或把 N 设到常规候选数之上即可，容量只作人群聚集时的带宽保护阀。单区域 AOI 不受影响；Cluster 目前没有生产装配调用方。见 [INTEREST-BLOCK-AOI](../framework/INTEREST-BLOCK-AOI.md)。
- **AOI 视野（D14 → D19，2026-10-20 实施）**：① 视野变大时新进入范围的目标一次性订阅、各下发全量，峰值受 `MaxVisible`（按视野计）与 Manager 快照预算约束，没有按玩家单独的预算。② 改视野、视野操作都没有排队版（维护者决定），事务内决定的视野变化（入盟、放眼、组队、Buff 改视野）由业务在提交之后调用。③ 一个玩家挂多个视野时，它持有的目标数受 Manager `Limits.MaxObjects`（缺省 100，也是每帧对象上限）约束，超出由 Manager 拒绝（重试表与 Drain 诊断可见），按玩法调大；`scripts/perf/sync-aoi` 的多视野场景按 `visible × (1 + views) + alliance` 放宽。④ 共享视野的扇出：一次变化的成本 = 变化量 × 挂接人数，由 `MaxPlayersPerView`（缺省 4096）约束；`BenchmarkRelationViewMemberChange` 冒烟（50 次，未正式测量）1024 人挂接时换一个成员约 0.2 到 1 ms（只含登记表与扇出，不含 Manager）；2026-10-20 夜间（[夜间补测](../performance/NIGHT-20261020.md) §14'）：`BenchmarkRelationViewMemberChange` 正式 10 样本 1024 人挂接、1024 成员时约 0.78ms；sync-aoi 共享关系视野 100 / 1000 人（可见约 143 / 1042）Nest 与 direct 都与单视野同量级；每玩家 4 个额外空间视野（可见约 244）Nest 模式改值→客户端 P99 78ms（direct 49.5ms）；每玩家 16 个额外空间视野（1000 玩家共 1.7 万个空间观察者，可见约 827）时 Nest 模式失守（P99 350～376ms、帧数 38%），**2026-10-21 已定位并修复**（[夜间补测 §14'.2a](../performance/NIGHT-20261020.md#142a-根因与修复2026-10-21)）：根因一是 AOI 批次里每个目标移动都让旧、新块上的全部观察者完整重评（改为只评估移动过的目标，结果与完整重评逐项相同）；根因二是排队事实在 Flush 的政策阶段一次性应用，AOI 全部挤进 Flush（改为每个 Interest 一个应用线程分批消费，Flush 只结算；2026-10-21 起这是 ApplyAsync，缺省 ApplySync 在政策阶段同步应用并限制视野数）。修后同负载 Nest P99 97～141ms、帧数 98～100%、Flush 30ms（政策阶段 7ms），direct 57～70ms。**剩余边界（按两种应用模式，维护者 2026-10-21 决定由业务选，[DESIGN-TODO D19“两种应用模式”](DESIGN-TODO.md#两种应用模式2026-10-21维护者决定)）**：① **ApplySync（缺省）**每玩家视野不超过主视野 + 8（含关系视野），超过拒绝构造；AOI 在 Flush 的政策阶段，原场景（1000 玩家、可见每视野约 50）Flush 均值 4 / 8 个额外视野时 15 / 25～27 ms，8 视野约半个周期；玩家更多、更密或移动比例更高时同样 8 个视野 Flush 更长，没有测，超过周期会丢 tick（16 视野诊断构建 46～47 ms 已开始丢）。② **ApplyAsync** 不限视野数，Flush 只结算（16 视野 35 ms），比 direct 多约一个同步周期——Manager 按自己的周期结算，截止前几毫秒才应用完的事实要等下一周期；维护者决定接受（选 C：不加“Flush 前等应用线程”、不做周期对齐）。16 视野 ApplyAsync P99 130～138 ms，direct 69～90 ms。改值集中在周期末尾到达的负载在 ApplyAsync 下尾部会更差，没有测。③ 本夹具里 ApplySync 的测得延迟没有比 ApplyAsync 低（4 视野 P99 65 对 59 ms，8 视野交错），它的好处是“提交后下一次 Flush 必定可见、没有额外线程”。④ 新的小块小视野场景（4000×4000、块 20、视野 30、5 万实体，每 tick 500 次移动）+4 / +8 视野两种模式 P99 53～56 ms、direct 50 ms，Flush 不超过 7 ms。⑤ 新订阅全量 P99 的夹具口径已修（2026-10-21）：全量只计本 tick 改的值，修前会把全量里旧改值的年龄算进去（确定场景 2000 ms → 0）；代价是跨 tick 才送到的全量不计入，全量 P99 偏乐观。⑥ 夹具是内存提交（`DurabilityMemory`），strict / pipelined 的 WAL 等待与排队事实就绪的关系没有测。 已写进 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)“已测规模与边界”与 CHANGELOG。真正的大联盟（上千人互相可见、百万级 pair）与目标密度的多视野仍没有压测。⑤ 包含关系按 `ViewSets` 字段判断；不配 `ViewSets` 时不知道字段，不同层级一律取并集身份，手写 packer 若不经 `SyncViewSet.Lookup` 解析就要自己认 `SyncProfile.Layers()`。⑥ 层级并集只在一个 Interest 内部合并；同一 pair 同时被 Interest 与 Group / Direct / SelfView 等其他政策持有时，Manager 仍按优先级取一个视图（未改，见 [DESIGN-TODO D19](DESIGN-TODO.md#d19-视野view逻辑-id空间--关系两类玩家与视野多对多) 实施结果）。⑦ 关系视野不排除挂接玩家自己的实体（联盟成员里有自己时，自己经 self 与联盟两个层级看见自己，按包含取高或取并集）。⑧ D14 / D19 前后对照（2026-10-20 夜间，P0 `49897751` / P1 `08432d4e` / P2 `69fb668f` 同机交替 10 轮）：`ClusterFlushRandomWalk` +3.5%（D14 引入，D19 不变），其余 AOI / Cluster 基准与分配无差异，`Relation*` 旧基准在 P2 上已删除（不可比）；sync-aoi 单视野三点无差异。见 [guide/04-sync](../framework/guide/04-sync.md#谁看见谁视野viewd19)、[impl/04-sync §2E](../framework/impl/04-sync.md#2e-interest-的视野d19在-d14-上扩展)。

## 生成器、配置与工具

- **DAO 库名映射**（[D9](DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A）：`db=` 是逻辑库名，物理库由 `dataengine.databases` / `WithDatabaseResolver` 在启动时冻结。限制：①改映射等于搬数据，框架只在 WAL 还有未投影记录时拒绝启动（`ErrDatabaseMappingChanged`），不复制数据；排空后改了映射却没迁移，新库是空的，冷加载得到“从未创建”，GetOrCreate 会新建玩家——步骤见 [PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)。②声明了却没有映射的逻辑库物理名就是逻辑名本身，只在启动日志告警（生成配置总写 `main` 的映射，手写配置删掉它会落到名为 `main` 的库）；这条回退只在有表时存在；根本没有表时 DataEngine 投影存储（`MongoStoreConfig.Databases` 为 nil）与 Remote 存储（没绑定）同一条规则：读写带库名的 DAO 文档返回 `dataengine.ErrDatabaseMapNotBound`，两边不再不对称（维护者 2026-10-21）；`SetRemoteProjection` 把 DataEngine 的表自动绑给 Remote 存储，自建装配只给 DataEngine 一张表。剩余边界：`NewMongoStore` 只在进程有声明的逻辑库时构造即拒绝，手写 DAO（没经生成器声明）缺表要到第一次投影才报错，那时记录已进 WAL，投影器反复重试这条记录（`ProjectionFailures` 增长、后面的记录不投影），补上表重启后照常投影（WAL 目录记下的上次映射为空，不触发 `ErrDatabaseMappingChanged`）。③不同进程（多个 sid、跨进程写 Remote 实体）的映射是否一致不校验。④二进制一个逻辑库都没声明（工程还没有 DAO）时，配置里的逻辑名无从做拼写检查，只告警。⑤工具经 `LoadDatabaseMapFile` 读同一份配置，但不会自动执行进程里注册的回调，用了回调的部署要给工具传同一个函数或用 `-db` 指定物理库。⑥根包守卫只看同一行里交给 `.Database(` 的参数，先赋给变量再传入的写法看不到，由各解析点的行为测试兜底。⑦回调与配置对同一逻辑库给出不同物理名时拒绝启动（同一逻辑库只能有一个来源，维护者 2026-10-20），但回调只在 Init 调用，`--check-config` / doctor 不执行回调，这类冲突要到进程启动才发现；工具传入回调时（⑤）按同一规则拒绝。
- **doctor `config-sections`**：配置文件生成后归应用所有，`add` 命令只在让服务多出 Mod 的那一刻追加缺失段；在 add 命令之外手改 `roost.yaml` 增加 Mod 后 `project sync` 不补（维护者 2026-10-17 决定 B），由 doctor 只读检查 `config-sections:<service>` 报 WARN 并给出可追加的文本。限制：业务有意删掉的段同样报 WARN，没有豁免开关（可忽略或留空段如 `saga: {}`）；“有没有”只看行首 `<key>:`，段里缺键、值形态不对不报（归 `config-schema`）；Secret 示例结构认不出（没有恰好一个 `config.yaml: |` 块）时只报“未检查”。
- **`roost project next` 的文件名约定**：first-business 修复提示按工程现状填名字，判断“缺协议 / 缺 endpoint”沿用 doctor 的文件名约定（`game/handler/<snake>.go` ↔ `protocol/def/<snake>.go` ↔ `game/controllers/<域>/<snake>.go`），协议名与 handler 名不同的工程仍会落到第一个可用名字或示例名。
- **endpoint 服务器决定的参数**：只识别业务时间这一类约定名（`nowUnix` / `nowUnixMilli`，`int64`，由生成的 Nest 分发包装在 handler 执行时从 `fctx` 填，D22-1）；其他服务器决定的值（玩家状态、配置）在 handler 里读，没有 `--server-param` 一类的通用标记。约定名是全局的：任何 `//roost:nest` handler 的参数叫这两个名字就不再从消息里取，要调用方传一个时间时换个名字（例如 `resolvedAtUnix`）。
- **Saga / Remote / Skill 的 doctor 检查**：`remote-runtime` 在带 Nest 的服务部分缺 `remote_entity` 时只 WARN（实体由哪个服务写静态上看不出）；技能目录由生成的 `catalog_test.go` 与 doctor `skills:catalog` 检查，服务 Init 里调 `CompileAll` 归业务。
- **configdata 重载内容未变也递增版本**（维护者 2026-10-18 决定 B）：照常发布、递增版本，回答 / 日志 / 指标标明 `changed=false`（`configdata.reload.total{result="unchanged"}`）；理由是监听者依赖每次重载都执行（如开关表用重载恢复表值）。版本号不是内容版本，内容是否变化看 `changed` / `hash`。
- **文档提交号门禁**（`TestDocumentedCommitsAreReachableFromHEAD`）：浅克隆（CI 默认 depth=1）整条跳过，只在本地完整检出与 release 工作流（fetch-depth: 0）生效；纯数字短 SHA 与不带反引号的提交号不检查；只查“在 HEAD 历史里”，不查是不是文档所说的改动。分支上的文档引用本分支提交时 rebase 后会红：引用本轮改动优先写测试名 / CHANGELOG 条目，或合入后改成 main 上的提交号。

### 业务 HTTP 路由与配置检查

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

## 框架服务与 game-demo

- **匹配队列状态**（维护者 2026-10-19 方案 A）：一个分区的全部状态整体读写；结束满 `match.retention`（缺省 10 分钟）的票、比赛与排队请求记录在下一次写入或 Sweep 时移除，重放与读历史只在保留期内有效（超期 Commit 重放报 `ErrTicketMissing`，不会成第二局）。状态上限 = 等待票（≤ `MaxQueueLength`）+ 保留期内的成局量；大队列按 `Partition` 分键是推荐用法，跨分区成局用 `CommitAcross`（键带 hash tag，一个逻辑队列的全部分区在 Redis Cluster 的同一节点）。规模建议见 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界)。2026-10-20 夜间容量测量（一次性夹具，本机 loopback 私有 Redis，[夜间补测](../performance/NIGHT-20261020.md) §11）：每个分区的状态按“每秒成局人数 × 已过时间”增长（每名已成局玩家约 510 B），单次操作耗时随状态线性上升；4 分区各每秒成局 10 人、保留期 10 分钟时第 8 分钟（每分区约 2.6 MB）CAS 冲突比例到 0.94、请求超时，同一 Redis 上无关命令的 PING P99 到 0.6s——这一档**不可持续**，持续上限约为每分区“每秒成局人数 × 保留期”≤ 约 4500 人（状态约 2.3 MB），超过时调小 `match.retention` 或加分区（2026-10-21 维护者批准写进 [guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界) 规模建议，注明单样本、非目标负载）；Redis 被拖慢会影响同实例的其他服务。`CommitAcross` 与相邻分区匹配器抢同一批票时冲突约 49%（夹具有意的高竞争形态）。私有 3 主 3 从 Cluster 上两次杀掉该逻辑队列槽位的主节点：只有约 1～2 秒的连接错误，多键脚本没有部分生效，590 场比赛核对全部一致；异步复制丢最近写、跨机分区与 Cluster 下的容量未测。夹具的后台 Sweep 因参数错误（limit 1000 超过上限 200）未生效，单样本。
- **game-demo 公会名**（维护者 2026-10-17 决定：业务逻辑，框架不处理）：`guild.ValidateName` 只拒绝空、全空白、控制字符与非法 UTF-8；长度上限与全服重名 demo 不实现，名字只受协议帧上限约束、两个公会可以同名。需要时长度 / 敏感词加在 `ValidateName`，全服唯一参考 `service/directory`（先 Reserve 再创建、成功 Commit、失败 Cancel），见 [Service 篇 §4](../framework/guide/09-services.md#4-关键业务边界)。
- **game-demo 副本排行**：排行准入就是奖励准入（领奖事务内的赛季通关数 + `UpdateMax`）。通关数按赛季计（维护者 2026-10-18），改 `ranking.CurrentSeason` 后新榜从 0 开始；`UpdateMax` 同分保留较早的 tie，补报的那一分在同分时按补报时间排序。
- **game-demo 活动结算榜**：`ZADD GT` 原子保留较大累计值，需要 Redis 6.2 及以上；只修复成功回复乱序导致的回退。Redis 写失败只记日志，协调器与结算索引不是一个事务，没有自动对账；Redis Cluster、跨机延迟与长稳未验证。业务将来允许扣分时需改用版本化快照或其他明确的顺序方案。
- **game-demo 活动贡献**（维护者 2026-10-18 决定 A：只在奖励首次付款时贡献）：**已接受的代价**——付款之后、贡献之前进程崩溃或协调器调用失败，这一分永久丢失，重放不补（端点打 Warn）。协调器只在同一窗口内去重，业务在别处直接调用 Contribute 须自己保证每局一次。
- **全服邮件 `mail.LazyBroadcast`**（[DESIGN-TODO D17](DESIGN-TODO.md#d17-全服邮件的内置投递策略)，用法见 [Service 篇 §1B](../framework/guide/09-services.md#1b-mail-全服邮件)）：生成骨架缺省仍为 nil（拒绝广播），启用要改 `Broadcast()` 一行。剩余边界：① **账本规模没有压测**：账本是一个文档（缺省至多 1000 条未过期记录，每条约 150 字节 JSON），每次发布 / 撤回整体读写、在 Redis Cluster 上落在一个节点；读路径按版本在进程内缓存，账本变化后每个 mail 进程各重读一次。上界附近的发布延迟、十万级在线同时登录时版本键的读压力、公会邮件频繁发送时的上界是否够用 v1.29 未验证。② **画像以最近一次 `PullBroadcasts` 为准**：入会 / 退会后业务不再拉一次，`List` / `Summary` 会按旧范围合并、水位越过新公会已发出的邮件（之后再拉也补不回）；在两次拉取之间入会、而公会邮件发于入会之前的，会被合并（需要“入会后才收”时把入会时间放进 `Attributes` 由 `Eligible` 判断）。③ **条件只判一次**：被内置条件或 `Eligible` 判为不满足的全局邮件，水位越过后不再重新判断（玩家之后满足了也收不到）。④ 没调用过 `PullBroadcasts` 的玩家（没有画像）`List` 不合并；game-demo 只放本服范围、不放公会范围。⑤ 撤回只挡未合并的，已合并的副本没有“强制收回”入口；撤回记录保留到过期。⑥ 内置条件按秒比较、严格早于：发送同一秒注册的角色收不到。⑦ 合并只在邮箱放得下时进行（全是未读的满邮箱等玩家腾位置），停住期间过期的全局邮件不会再补。
- **业务时区与每日重置**（DESIGN-TODO D18 已实施，维护者 2026-10-20 决定 A）：业务日按 `time.business_timezone`（缺省 UTC）/ `time.business_day_start`（缺省 00:00）算，见 [guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)。剩余限制：① 时区数据库靠 `main.go` 导入 `time/tzdata`；`main.go` 归应用、sync 不改，旧工程或手写 main 在没有 zoneinfo 的镜像里，启动检查会拒绝非 UTC 时区（报错点名 `time/tzdata`，不会静默退回 UTC），要自己加一行导入。distroless 镜像本身是否带 zoneinfo 仍未在容器内实测，内嵌后不再依赖它。② “看不到 zoneinfo”的回归在 macOS 上用 `sandbox-exec` 模拟；自带 zoneinfo、没有这种隔离的 Linux 主机上 Skip（只设 `ZONEINFO` 为空目录模拟不了：Go 照样回落到系统目录）。③ macOS 文件系统不区分大小写，`asia/shanghai` 在开发机上能通过检查，在 Linux 镜像里加载失败（启动时拒绝）。④ 周重置、按周 / 月划分的活动窗口没有框架辅助，由业务按 `Day` 推。
- **Session 运行服务**：保持现状、不改为可选（维护者 2026-10-14）；后续事项见 [ACCESS-SYNC-FOLLOWUPS](ACCESS-SYNC-FOLLOWUPS.md)。
- **观测接线**：ExportMetrics / admin 元数据不是自动外部监控或审批系统；部署实际接线与鉴权审计归部署方。


### 合服（D15）

别名合服（[DESIGN-TODO D15](DESIGN-TODO.md#d15-合服把一个服并入另一个服) 已实施，设计与实施记录 [FEATURE-2026-10-20-server-merge-alias](FEATURE-2026-10-20-server-merge-alias.md)，runbook [PLAYER-MIGRATION §6](../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)）。当前边界：

- **持久总线消息只列清单、不检查积压**：`wiring/sidmerge` 不连 NATS。按 sid 寻址的持久用法（`syncbus.transport: jetstream` 的每 sid durable、`nats.rpc.transport: jetstream` 的实例 durable、可靠总线的死信）由运维按清单人工确认（检查项 `bus` 为 `confirm`）。框架不把发往 A 的消息重投到 B。
- **saga 终态检查是整个 saga 存储**：saga 记录不带“属于哪个 sid”，同一存储里其他仍在运行的服的非终态 saga 也会让检查不通过；只有全集群停服窗口里才自然满足。是否提供“按类型 / 按业务键过滤”的检查需要维护者决定（尚未决定）。
- **“同一 Mongo”按 URI 比较**：主机列表（小写排序）+ 副本集名；同一集群换主机名或 IP 写会被判为不同而拒绝合服（偏保守，不会把两套 Mongo 当一套）。
- **跨记录的 account 写不是原子的**：每条服务器记录按版本条件写；写入顺序保证中途停下不会出现两跳，结束复核，并发合服造成的两跳报 `ErrServerMergeCycle` 并给修复命令（unmerge 再 merge）。拍平之后“X 是经由 A 并入的”这一历史不保留：`UnmergeServer` 只恢复一条记录，经 A 改指的其他服仍指向承载服。`MergedFrom` 是辅助索引，可能多出并发留下的项，是否并入以对方的 `MergedInto` 为准。
- **启动自检的范围**：`ServingGuardMod` 只在生成器装配的、承载玩家且接了 account 的服务里；手写装配要自己加。它排在 DataEngine 之前，但不排在 `syncbus`、`remote_entity` 等 Mod 之前：误启动的 A 在被拒之前这些 Mod 已经 Start 又随即停止（2026-10-20 演练：拒绝发生在启动后约 60ms）。account 不可达时 30 秒后拒绝启动（fail-closed），代价是 account 故障期间 game 起不来。
- **按 sid 独占数据的迁移不拷贝索引**：B 启动时按 DAO 声明建；不拷贝 `_dataengine_*` 控制集合。Remote 托管（`remote=managed`）落在按 sid 分库里的集合只在报告里点名，所有权元数据按 Remote 规则另行处理。回退不动合服期间 B 新产生的按 sid 独占文档（只计数），归属由业务判断。
- **源库保留为只读归档**：框架不加只读保护，靠启动自检与 runbook；删除由运维确认后手动做。
- **消费者**：Gate 转交给 game 的绑定不带 Claims，game 侧需要原服标识时从自己的角色数据取；`OriginServerIDClaim` 只在认证器那一侧可见（嵌入 TCP 的 Principal 里两者都有）。CreateRole 被拒经总线时客户端看到的是 `ErrServerClosed` 的通用原因，“已并入 B”的详情只在 account 进程的错误里。 game-demo 拉全服邮件（D17）时，原服并入本服的角色另带原服范围 `ServerScope(A)`，合服前发给 A、还没拉到的全服邮件照样属于他——这是 demo 的业务选择，框架不规定。
- **验证边界**：真实进程两服演练（私有环境，sid 1000 / 1001，[证据](../performance/benchmarks/d15-server-merge-20261020/README.md)）覆盖嵌入 TCP；独立 Gate 只有单元级（正式 account 服务 + `FixedGameResolver`）。演练里的 `dbscope=sid` 文档是直接写 Mongo 的（game-demo 没有用它的实体），B 的进程没有冷加载它们；玩家数据（共享库 main）经真实进程读写。

## 平台与外部验证

- **跨机HA与Linux物理故障**：本机集群 / loopback 不证明跨机器分区、掉电、真实网卡；具名清单见 [EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) E01～E28。
- **客户端引擎**：有 Go / C# 及 Unity 适配源码与测试，未声明真实 Unity / Godot / Unreal 全矩阵；各引擎实机、线程、断线与跨平台确定性未验收。
- **原生C++接入**：无完整 C++ / Unreal / GDExtension SDK 承诺，需独立设计和协议一致性验收。
- **独立 Gate**：已实施 Gate / Game、广播与 Sync / Lockstep（[GATEWAY](../framework/GATEWAY.md)）；不用本机多实例代替多进程 / 跨机矩阵。容量结论见能力表。
- **schema升级**：DAO 自动迁移已撤销，运行时只接受当前 schema；离线迁移按 [schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo 有执行器与 `cmd/schemamigrate`。在线双读与 `roost add migration` 骨架未实施，上线后重新决定；业务需明确离线转换和保留数据范围。2026-10-20 夜间在 v1.30 候选生成的 game-demo 上重做真实进程演练（[夜间补测](../performance/NIGHT-20261020.md) §12），与 10-15 逐步一致。
- **Player跨机**：以已落库数据为准，不要求本地 WAL 热迁移；搬的是 sid，按 [参考流程](../framework/PLAYER-MIGRATION.md) 停写→排空 WAL→释放单实例锁→切路由→新进程空 WAL 接管，game-demo 有检查工具 `cmd/sidmove`。单个玩家改绑 sid 无框架能力；合服用别名合服（D15 已实施，边界见下文[合服（D15）](#合服d15)）；真实跨机网络、卷迁移、客户端重连路径未演练；框架级迁移命令上线后评估。2026-10-20 夜间在 v1.30 候选上重做同机两进程演练（[夜间补测](../performance/NIGHT-20261020.md) §13：正常迁移、kill -9 后原 WAL 重启、Mongo 停顿时 kill -9 的未排空，302 次确认逐键 +1、最终 Mongo 等于最后确认），与 10-15 一致。
- **Windows**：非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'` 和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异（打开中的文件不能改名 / 删除：nestwal 损坏记录检查、日志轮转、statslog；checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真；run 38500440362 的 6 个失败）。本机结果不改变 macOS / Linux 支持范围，跨平台也成立的缺陷不归入此条。

## 能力与验证边界

口径：v1.28.0 与 v1.29.0 发版时都没有压测；2026-10-19 发版后补测 v1.29.0 同参数重档 30 分钟与 Saga 两档（[v1.29.0 发版后复测](../performance/V1290-HEAVY-30M-20261019.md)，附同日同机 v1.27.0 对照）。2026-10-20 夜间在 main `69fb668f`（v1.30.0 候选）上按维护者批准的清单补测，每项 15 分钟（Saga Mongo 步骤夹具上限 10 分钟），服务端 / 压测端分开统计，宿主负载正常、全部作为基线（[夜间补测](../performance/NIGHT-20261020.md)）。下表“当前结论”取最新一次。

| 项 | 当前结论 | 后续入口 |
| --- | --- | --- |
| 重档总体 | 30 分钟通过：吞吐、计数、DAO 终值、AOI 可见集合、P99 门槛通过，消息 / HB 拒绝 0，堆不增长；goroutine 恒定比 v1.27.0 多 1000（每连接写协程）。业务 P99 13.09ms、HB 11.01ms（混合口径）；服务端口径业务 P99 4.37ms、HB 2.68ms（第一份未污染的 30 分钟服务端样本）。2026-10-20 夜间 A-B-A-B（v1.27.0 / HEAD 各 2 次 × 15 分钟）：HEAD 的 Sync 提交→客户端 P99 稳定高 0.6～1.1ms（13.99 / 13.92 对 13.35 / 12.80），两对方向一致，判断为代码带来的小幅增加，具体来源未做单变量对照、尚未确认；业务 / HB 混合 P99 高 0.1～0.2ms、Gate 往返无差异；服务端业务 P99 3.37 / 3.38ms | 单变量对照（例如 B3 写协程交接）；独占服务器 / 跨机 |
| Sync偶发长尾 | on_change 按 P99≤50ms 验收：Sync 提交→客户端 P99 15.43ms、最大 156.262ms、>50ms 0.0260%。>50ms 总量变多同日对照同样出现，按宿主条件处理；10-19 的“Sync >50ms 为同日对照 1.8 倍”在 2026-10-20 夜间 A-B-A-B 里不复现（四次服务端段 >50ms 都是 0），判断为宿主条件、不是代码。`TAIL=metrics` 15 分钟只出现 1 个长尾簇（284 个混合口径事件、最大 52.6ms），落在整分钟后第 1 秒、同秒调度延迟 21ms，相位与 10-15 一致但只有一簇，不足以确认；非整分钟的簇本次没有出现 | 维护者 2026-10-15 决定先接受现状，有独占服务器 / Linux 实机时再复测（方案 A） |
| 独立 Gate | 重档 30 分钟通过（short 8 / io 8，GOMAXPROCS=10，Gate 与 Sync 开）：Gate 往返 P99 6.78ms、最大 236.691ms。2026-10-20 夜间 A-B-A-B：HEAD 5.30 / 5.26ms 对 v1.27.0 5.40 / 5.20ms，>50ms 四次都是 0，10-19 的“>50ms 为对照 2.6 倍”不复现 | 多进程 / 跨机矩阵；[Gate/AOI 性能报告](../performance/GATE-AOI.md) |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行 observer。2026-10-20 夜间 4 并行度重档 15 分钟通过（GOMAXPROCS 4、short 4）：服务端业务 P99 4.83ms、Gate 往返 P99 7.74ms、Sync 提交→客户端 P99 17.98ms，>50ms（服务端）0；不压额外 observedBlocks 扇出 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验（未做） |
| 嵌入 TCP 出站 | 2026-10-20 夜间嵌入 TCP + 场景 Sync 重档 15 分钟通过：`budget_exhausted` 与全部出站拒绝 / 关闭计数 0，写协程 1000 → 1000；服务端业务 P99 1.57ms、往返 P99 1.98ms、Sync 提交→客户端 P99 10.14ms，>50ms 全 0；堆 208MB。每连接队列峰值与驻留字节取不到（没有 Gauge） | 停读客户端 / 更高推送量下的预算；驻留 Gauge 与按帧龄关闭由维护者决定 |
| 自身视图（self_sync） | 2026-10-20 夜间嵌入 TCP + self_sync 重档 15 分钟通过：约每秒 1000 帧，`push_refused` 0、滞留 0，提交→客户端 P99 1.43ms，服务端业务 P99 2.07ms；整轮重试放大未被触发（未测到） | 独立 Gate + self_sync 夹具；预算压到触发点的放大测量 |
| Lockstep 追帧 / 快照恢复 | 嵌入 TCP 追帧随链路放慢由包内与 game-demo 生成工程的真实回环 TCP 回归覆盖；快照恢复（2026-10-20，未发版）由包内、MOBA 十客户端 KCP、生成工程 C# 真实 TCP 回归覆盖，toxiproxy 演练嵌入 TCP 256 / 64 KB/s 下 1～4 MiB 快照恢复、独立 Gate 慢链路上大快照被断开循环（只准入，需 Gate 写出回执的协议改动，维护者 2026-10-20 决定暂不做）；纯追帧页 2026-10-20 夜间 toxiproxy 256 / 64 KB/s 演练：回到头部时间与“积压 /（链路 − 每秒新帧）”之比 1.02 / 1.03，连接不断 | 独立 Gate 慢链路上的纯追帧页；真实移动网络 |
| Remote容量 | 2026-10-20 夜间 80TPS × 15 分钟通过（共享隔离环境）：72000 / 72000，零错误零丢弃，全量核验通过，服务端 p50 / p95 / p99 164 / 249 / 749ms。2026-10-21 同机对照（v1.27.0 `a147877c` / HEAD `af904c76` 各 1 次 × 15 分钟，同参数背靠背，[§9.1](../performance/NIGHT-20261020.md#91-同机-a-b-a-b2026-10-21)）：两边都通过，混合 p99 同为 263ms（HEAD 服务端口径 262ms），p50 / p95 158 / 229 对 160 / 231ms，WAL 未确认 / 在途事务峰值 17 / 16 对 19 / 19；夜间 p99 偏高与峰值约 2 倍**倾向环境条件（同时段共享 Mongo 主节点慢操作 640 条对今天 79～126 条），不是代码退化**，单样本、Mongo 当时变慢的原因尚未确认；非跨机上限 | 跨机 RTT / 故障 / 24h；要排除 HEAD max / ≥500ms 次数偏多（1280ms、122 条对 605ms、26 条，单样本）需每边 ≥2 次交替 |
| Saga补偿容量 | Mongo 步骤路径 v1.29 各 10 分钟：正向 20/s P99 0.62s、全补偿 10/s P99 0.61s（服务端口径 0.59 / 0.58s），十个分钟段无增长趋势，核对通过。2026-10-20 夜间更高档位（10 分钟）：正向 30/s 完成率 28.4/s、P99 141.7s、17302 笔以补偿 / 失败结束，全补偿 15/s 完成率 14.0/s、P50 单调增长到 49.5s、866 笔 failed——两档都饱和（outbox / lease 排空、存储与发布零失败）。单进程上限在正向 20～30/s、全补偿 10～15/s 之间 | 中间档位（25、12）；真实步骤、混合负载与生产存储另测 |
| Saga 原生步骤容量（单进程） | 2026-10-20 夜间 15 分钟：正向 8/s（两次）与全补偿 5/s 都 0 笔终态不对、核对通过；服务端 P50 559 / 549ms、713ms（P50 < 1s 成立），**P99 0.92～3.9s**，有分钟级尖峰（各段 Mongo 命令最大值同时到 750～900ms，接近投影器容量时积压放大）；两次正向 8/s 的 P99 差 4 倍。原先 guide/06 §5、生成的 `sagaCapacitySummary` / `steps.go` 注释与 `roost help saga` 里的“P99 0.8 / 1.2 s”来自 2 分钟单样本，15 分钟样本上不成立；2026-10-21 维护者批准，这几处与 CODEGEN_REFERENCE / PROJECT_GENERATOR / BEGINNER_WORKBOOK 已改为 15 分钟口径（P50 < 1s、P99 0.9～3.9s、WAL 放快盘、Mongo 步骤上限区间）；限制与维护者决定见上文 [Saga](#saga) | [SAGA-CHECKPOINT](../performance/SAGA-CHECKPOINT-20261018.md) 复跑入口；P99 尖峰的 Mongo 侧原因 |
| 投影 checkpoint 与 outbox 唤醒同盘 | v1.29 未验证：2026-10-18 观测到投影后唤醒 outbox（`de8b6e97`）使 checkpoint 变慢，机制为推断（darwin 上 `WAL.Ack` 的 F_FULLFSYNC 与被唤醒 outbox 的 Mongo majority 写同时落盘），无逐次 fsync 证据，Linux 未测。唤醒的净收益为正，不回退；WAL 与 Mongo 分盘（第三档 C）不做。2026-10-20 夜间 RAM disk 对照（正向 8/s × 15 分钟，WAL 放 RAM disk）：checkpoint P50 56～58 → 1ms、WAL 追加 18 → 1ms、投影等待 62 → 0ms、服务端 P50 549～559 → 334ms，空载 checkpoint 约 8.8ms，说明负载下变慢主要来自同盘竞争；RAM disk 同时去掉设备 fsync 与同盘竞争，两者没有分开；P99 由 Mongo 侧停顿决定、两种盘都有。2026-10-21 维护者批准把“磁盘同步是主要成本，部署时 WAL 放快盘”写进 guide/06 §5 与生成的容量说明 | Linux 复测；逐次 fsync 计时 |
| 匹配队列容量 | 2026-10-20 夜间（本机私有 Redis，一次性夹具）：每分区“每秒成局人数 × 保留期”持续上限约 4500 人（状态约 2.3 MB），4 分区各 10 人/s、保留期 10 分钟第 8 分钟崩溃（CAS 冲突 0.94、超时、同 Redis PING P99 0.6s）；CommitAcross 竞争冲突约 49%；Cluster 主节点 SIGKILL 切换两次无部分生效。见上文“匹配队列状态” | Cluster 下的容量；跨机 Redis（guide/09 §4 已补保留期的规模建议，2026-10-21） |
| 生成工程完整链路持久写入（嵌入 TCP + strict） | 2026-10-20 单样本、非独占宿主（[报告](../performance/GENERATED-TCP-STRICT-20261020.md)）：CLI 生成工程、strict handler 金币 +1、不经 Saga、无 self_sync，50 连接各自一个玩家**压测端** 1253 TPS、P50 38ms、P99 75ms，WAL 每批约 9.9 条；同一实体 108 TPS。上限约为 short 池 worker 数（缺省 GOMAXPROCS）/ 每笔持锁时间（strict 在锁内等所在批 fsync，本机约 8ms）。async 不比 strict 快（同机 1083 对 1257），原因尚未确认 | 独占宿主复测与 self_sync / pipelined 对照（待补测第 14 项；维护者 2026-10-21 批准，随 v1.31.0 发版前压测一起跑） |
| schema 升级 / Player 跨机演练 | 2026-10-20 夜间在 v1.30 候选生成的 game-demo 上真实进程演练通过（schema：拒绝旧 schema → plan / apply / verify → rollback 只恢复未被写过的一份；Player：正常迁移、kill -9 后原 WAL 重启、未排空被 `wal-drained` 拒绝，302 次确认逐键 +1），与 10-15 一致 | 真实跨机网络、卷迁移、客户端重连路径 |

## 游戏接入验收

九份接入手册的“第二部分 验收”是各自验收范围的完整说明（场景清单、证明与不证明、崩溃点、计数核对）。脚本都不挂在 `dataengine-env.sh test`、不在根包门禁里，改 `game` 模板、`add dao` / `add entity` / `add timer` / `add attribute`、多实体 Sender、Nest 生成或 Lockstep 时手动跑；真实模式入口登记在根包 `integration_coverage_test.go`。夹具里的数值都是业务样例，玩法规则缺少默认实现不算框架缺陷。

| 游戏 | 入口脚本 / 真实模式变量 | 证明什么 | 不证明什么 | 手册 |
| --- | --- | --- | --- | --- |
| SLG | `scripts/test-slg-generated.sh`，`ROOST_SLG_REAL=1` | 6 个建造、行军、抢地组合的事务与期限行为；联盟活动联合链路 3 条：协调器子进程重启后贡献只计一次、阶段屏障、原服奖励义务丢 ACK 后重投，两个原服 Player 在“已到账未标记”处 SIGKILL 后冷加载补完，联盟权威经 Remote owner 交接后旧 owner 被拒 | 缺省模式无 WAL / Mongo、无真实重载与驱动加载后投递（离线定时器只做 DAO 水合）；联合链路的联盟 owner 与 sid 客户端同在一个父进程、固定服务发现、协调器只正常重启、视野传输为内存捕获、无空间迷雾与跨服撤权、不含实体定时器 | [第二部分](../framework/SLG-INTEGRATION.md#第二部分验收) |
| MMORPG | `scripts/test-mmorpg-generated.sh`，`ROOST_MMORPG_REAL=1`；`test-mmorpg-scenes.sh`；`test-mmorpg-cross-sid.sh` | 13 个多实体组合；正常停机与 SIGKILL 后交易 / 托管 / 定时器经投递循环恢复不丢不重；多地图 TCP；双 sid 赠礼 Saga | 提交在途崩溃、断电、跨机、动态 Player 跨 sid、真实客户端与目标密度 | [第二部分](../framework/MMORPG-INTEGRATION.md#第二部分验收) |
| MOBA | `scripts/test-moba-generated.sh`，`ROOST_MOBA_REAL=1`，再加 `ROOST_MOBA_ROUTING=1` | 28 条结算 / Lockstep / 匹配 / KCP 追帧场景（含断线超过历史窗口后经服务器参考检查点快照恢复：直接驱动 Match 一条、经持久对局 Room 与已提交门一条；Room 提交卡住 / 结果未知时不发未提交帧）；结算两进程恢复；活动对局 SIGKILL 后续赛与 SIGKILL 冷加载后窗口外客户端经已提交检查点快照恢复（真实夹具的宿主都用 `RecoverRoomWithSnapshots`）；五进程 NATS 宿主交接与奖励重投 | 对客户端广播与真实鉴权的端到端链路、权威 HA、tick 预算、5v5 目标负载与跨机 | [第二部分](../framework/MOBA-INTEGRATION.md#第二部分验收) |
| 卡牌 RPG | `scripts/test-card-rpg-generated.sh`，`ROOST_CARD_RPG_REAL=1` | 16 个抽卡 / 养成 / 回合组合；SIGKILL 后从未投影 WAL 恢复，投递循环补一回合再按期投递 | 提交在途崩溃、断电、系统时钟长时计时、Redeliver、多进程与容量 | [第二部分](../framework/CARD-RPG-INTEGRATION.md#第二部分验收) |
| 合作塔防 | `scripts/test-tower-defense-generated.sh`，`ROOST_TOWER_DEFENSE_REAL=1` | 10 个共享建造 / 波次组合；第一波进行中 SIGKILL 后逐字节恢复，循环补一步再按期打完三波 | 提交在途崩溃、断电、四台真实客户端、多进程与容量 | [第二部分](../framework/TOWER-DEFENSE-INTEGRATION.md#第二部分验收) |
| 合作生存撤离 | `scripts/test-survival-extraction-generated.sh`，`ROOST_SURVIVAL_EXTRACTION_REAL=1` | 12 个入场 / 采集 / 制作 / 撤离组合；制作与撤离在途 SIGKILL 后恢复，循环补触发一次且不追帧 | 恢复路径上的探险结束处理、提交在途崩溃、真实客户端、多进程与容量 | [第二部分](../framework/SURVIVAL-EXTRACTION-INTEGRATION.md#第二部分验收) |
| 合成休闲 | `scripts/test-merge-casual-generated.sh`，`ROOST_MERGE_CASUAL_REAL=1` | 15 个合成 / 离线批次 / 广告组合；SIGKILL 后恢复，过期挑战只判负一次、下一期限按时投递 | 提交在途崩溃、真实广告平台与登录票分配、多进程与容量 | [第二部分](../framework/MERGE-CASUAL-INTEGRATION.md#第二部分验收) |
| 放置模拟 | `scripts/test-idle-sim-generated.sh`，`ROOST_IDLE_SIM_REAL=1` | 12 个生产队列 / 离线收益组合；SIGKILL 后恢复，补触发只积分到原窗口、下一秒刷新按时投递 | 提交在途崩溃、真实客户端 / 支付、跨服市场、多进程与容量 | [第二部分](../framework/IDLE-SIM-INTEGRATION.md#第二部分验收) |
| 自走棋 | `scripts/test-auto-chess-generated.sh`，`ROOST_AUTO_CHESS_REAL=1` | 12 个卡池 / 阶段 / 领奖组合；冻结战斗后 SIGKILL 恢复，循环只补一个阶段并按期推进到终局 | 提交在途崩溃、八个网络客户端、跨 sid 发奖、多进程与容量 | [第二部分](../framework/AUTO-CHESS-INTEGRATION.md#第二部分验收) |

## v1.29 待补测

维护者 2026-10-20 批准的补测已于当晚在 main `69fb668f`（v1.30.0 候选）上跑完，第 1～13 项与 AOI 三点对照的结论已回填到上文各条与能力表，报告见 [夜间补测](../performance/NIGHT-20261020.md)。仍未完成或没有结论的：

1. **Sync 长尾的归因**（原第 4 项）：`TAIL=metrics` 15 分钟只出现 1 个长尾簇（整分钟后第 1 秒），不足以确认或排除与宿主整分钟周期任务同秒；按方案 A 等独占服务器 / Linux 实机复测。
2. **Sync 提交→客户端 P99 +0.6～1.1ms 的来源**（原第 3 项的后续）：A-B-A-B 判断为代码带来，但没有单变量对照，具体提交尚未确认。
3. **Remote 80TPS p99 偏高**（原第 9 项的后续）：2026-10-21 同机对照已做（各 1 次，[§9.1](../performance/NIGHT-20261020.md#91-同机-a-b-a-b2026-10-21)），v1.27.0 / HEAD p99 同为 263ms，倾向环境条件、不是代码退化；夜间共享 Mongo 主节点变慢的原因尚未确认，结论见能力表“Remote容量”。
4. **D19 每玩家 16 个额外空间视野时 Nest 模式失守**（AOI 三点对照新发现）：2026-10-21 已定位并修复（AOI 批次重评 + 排队事实异步应用），剩余边界见上文“AOI 视野”的“剩余边界”；Nest（ApplyAsync）与 direct 之间约一个同步周期的差距维护者 2026-10-21 决定接受，并改为由业务选应用模式（ApplySync 缺省，每玩家额外视野不超过 8）。
5. **投影 checkpoint 同盘**（原第 7 项）：RAM disk 对照已做，Linux 上复测与逐次 fsync 计时没做。
6. **Saga Mongo 步骤中间档位**（原第 8 项的后续）：20 / 10 稳定、30 / 15 饱和，25 / 12 未测；`TestRealSagaPerformance` 限制单次 ≤10 分钟，本项只能跑 10 分钟。
7. **匹配队列**（原第 11 项的后续）：Cluster 下的容量、跨机 Redis、异步复制丢写没有测；夹具的后台 Sweep 因参数错误未生效，要复测需改夹具。
8. **生成工程完整链路持久写入**（原第 14 项，2026-10-20 审查新增）：维护者 2026-10-21 批准，随 v1.31.0 发版前压测一起跑（v1.30.0 不再单独发版）。入口见[报告 §4](../performance/GENERATED-TCP-STRICT-20261020.md#4-复跑)。
