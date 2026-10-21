# 时间与定时器：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/10-time.md) · [模块总目录](../README.md)

## 先理解这一块

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

选择时钟时先问：这是游戏规则中的期限，还是网络、租约、停机的等待时间？

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 双时钟

业务时间表达玩法活动、冷却和可测试的游戏期限；系统时间表达 lease、网络期限、重试、WAL、日志和停机。业务 Now 与 SystemNow 分别注入；默认业务 clock.Now，系统 time.Now。不能把加速玩法时间同时作用到 Redis 租约或写超时。

## 2. 业务高水位

App 管理业务时间与持久高水位，运行中不允许时间倒退。初始 offset 可以为负，但生产必须为0，重启仍受持久高水位约束；“只允许前进”不是说所有测试初始 offset 都非负。offset 保存完整 Duration，不截断成整数秒。

## 2A. 业务时区与业务日

DESIGN-TODO D18（维护者 2026-10-20 决定 A，缺省 UTC）。存储与协议里的时间一律是绝对时刻（Unix 时间戳），“哪一天”只在**业务时区**里算。业务时区与每日重置时刻是部署配置，App 启动时读一次，不取机器的 `TZ` / `time.Local`——容器里 `time.Local` 是 UTC、开发机是本地时区，同一份代码跨天时刻会不同。

| 键 | 缺省 | 含义 |
| --- | --- | --- |
| `time.business_timezone` | `UTC` | IANA 时区名（`Asia/Shanghai`、`America/New_York`）。`Local`、空名、加载不了的名字在加载时拒绝 |
| `time.business_day_start` | `00:00` | 每日重置时刻，业务时区的墙上时间，两位 `HH:MM`。业务日从这一刻开始，之前的时间属前一天 |

两个键在 App 的配置声明里（`--check-config`、doctor `config-check`、`roost config check` 同一处检查），标了 `shared`：同一工程各服务必须相同（`roost config check --all`、doctor `config-across-services`）。生成器把它们写进每个服务的开发配置与生产示例；启动日志 `starting server` 一行打印实际的 `business_timezone` 与 `business_day_start`。

接口（`infra/base/clock` 的 `Calendar` 与 `Day`）：

- `app.BusinessCalendar(registry)` 取日历（挂在 `app.BusinessClock(registry)` 上，`clock.CalendarOf(clock)` 同一份）；没有 Registry 的代码用进程级的 `clock.BusinessDay(t)` / `clock.DayStart(t)` / `clock.NextDayStart(t)`。
- `BusinessDay(t)` 返回 `clock.Day`：业务日的日期，十进制 `YYYYMMDD`（`20261020`），可直接存进 DAO、按大小比较；零值表示“还没有过”。
- `DayStart(t)` / `NextDayStart(t)`：`t` 所在业务日的起点（不晚于 `t`）与下一个起点（晚于 `t`），是业务时区里的时刻。重置时刻按墙上时间：夏令时切换那天是 23 或 25 小时，不按固定 24 小时推；重置时刻落在被跳过或重复的那一小时，那一天仍只有一个起点。
- 时刻本身仍读业务时钟（带 `time.logic_offset`）：`cal.BusinessDay(app.BusinessClock(r).Now())`。

**每日重置的推荐写法**：DAO 里存上次重置的业务日；登录或当天第一次操作时 `BusinessDay(now)` 与存的不同就在同一个事务里惰性重置，不需要全服零点扫描。需要准点动作（零点推送、活动开门）时用按实体的持久定时器（§4），期限取 `NextDayStart(now)`，回调里再登记下一天。周重置由业务按 `Day` 推（例如取业务日起点的 `Weekday()`）。glsvet 对业务包（`game` 目录）里的 `time.Local`、`time.LoadLocation` 打印提示。

**时区数据库**：加载时区需要 zoneinfo。生成工程的 `main.go` 导入 `time/tzdata`（约 450 KB），distroless 运行镜像没有 zoneinfo 也能加载；手写或旧工程的 `main.go` 要自己加，否则启动检查报 `is not a time zone this process can load` 并拒绝启动（不会静默退回 UTC）。macOS 的文件系统不区分大小写，`asia/shanghai` 在开发机上能从系统 zoneinfo 加载、在 Linux 镜像里不能：照 IANA 原样写。

## 3. Scheduler

通用 Scheduler 保留纳秒期限和同期限确定顺序，宿主决定驱动 Tick 与持久化。若宿主只持久毫秒，入队前应把期限对齐到同一精度，避免恢复后排序变化；不能声称任意宿主都有纳秒持久保证。

`NewTimer` / `NewClosureTimer` 以调用时注入时钟的 `now + delay` 固定期限，typed timer 的 payload 同时复制，调用返回后可复用输入缓冲。Tick 回调内创建只把入堆及持久化通知推迟到最外层 Tick 结束；不会因后续回调耗时重新计算期限，新节点也不会在这次 Tick 中触发。若入堆时期限已到，由下次 Tick 处理。同一个回调里新建再取消时，钩子仍先收到一次 upsert 再收到删除，宿主可以靠 upsert 推进持久的 ID 种子。

加载时识别非法/过时记录并告警，NeedsCleanup 由正常事务 Tick 执行清理，不在水合阶段写 DAO。回调取消、重新安排、同期限顺序与再次触发应有业务测试。

回调返回正间隔表示按本次 `Tick(now)` 的时间自动重排，返回非正间隔默认结束任务。回调内显式 `ChangeTimer(ctx.Node.ID, base, delay)` 覆盖这个返回值，包括返回 0；`base` 为零时读取注入业务钟，指定 `ctx.Now` 时从本次触发时刻计算。只需从触发时刻延后自身时，直接返回间隔即可。

被显式改期的回调节点保持移出，最外层 Tick 收尾时才按最后一次改期放回，重入 Tick 也不会提前触发。`RemoveTimer` 取消正在执行或等待放回的回调节点后，返回间隔和后续改期都不会复活它；需要新任务应重新 `NewTimer`。这些规则对 typed 和闭包节点相同，但只有 typed 节点通过 `ChangeFunc` 持久化。对已经不存在的 ID（本轮已触发结束、已取消、从未登记）调用 `ChangeTimer` / `RemoveTimer` 返回 false、无效果，Tick 内外一致（2026-10-17 起；此前 Tick 内返回 true）。

持久化钩子在 Tick 期间就会收到变化：到期出堆时的删除、按返回值重排的 upsert、取消堆中节点的删除立即回调；等待放回的节点在最外层 Tick 收尾时各回调一次 upsert（多次改期只发最后一次）。钩子 panic 时调度器内存状态已经一致、不会在下次 Tick 重放，但剩余变化不再回调，宿主应把这次 Tick 当作失败：回滚事务并重建 Scheduler。

钩子里可以调用调度器（取消、改期、新建、重入 Tick）。收尾先把等待节点全部入堆再依次 upsert，所以钩子重入 Tick 时可能触发还没收到 upsert 的节点：调度器按钩子视图发变化——从未通知过的 ID 先补一次 upsert 再删除（按 upsert 推进 ID 种子的宿主不会在重建后复用这个 ID），钩子不持有的不再发删除。在到期出堆的通知（删除或补发的 upsert）里取消或改期该节点，本次回调不执行，与回调执行前取消 / 改期一致。

SLG 行军或建造可以用 typed 节点保存任务身份与期限；宿主须把到期删除、改期写回与业务状态放在同一锁和事务中。Scheduler 不提供独立事务，回调 panic 后的 DAO 撤销仍由宿主负责；按实体的接入不用自己写宿主，见 §4。

## 4. 按实体的持久定时器

到点要做的事（Buff 到期、建造完成、行军到达）属于某个实体时，用 `roost add timer --entity <Entity>`（DESIGN-TODO D11，维护者 2026-10-17 决定方案 B）。它把上面这套 Scheduler 接到实体上，分三处：

- **期限在实体自己的 DAO 里**：节点 map 与 ID 种子两个持久字段，生成的组件每次调用从 DAO 现建一个 Scheduler、钩子写回 DAO、用完丢弃。增删改都在调用所在的 Nest 事务里，回滚就是 DAO 的回滚；持久走 DataEngine 正式链路，没有第二份存储。种子随 upsert 推进，重启后不复用 ID。
- **触发在实体的 Nest 锁内**：生成的 Tick handler（`rollback=undo durability=strict`）调用组件的 `Tick(now)`，回调改的东西与节点的删除 / 续期是同一条 WAL 记录。回调只改 DAO、记 effect，不做 I/O、不阻塞。
- **驱动在 core**（`framework/nest/entitytimer`，`wiring/entitytimer` 装配）：进程内按“已加载实例的最早期限”排的小堆。实例加载时报告存量的最早期限，事务提交后（`nest.AfterCommit`）报告新的最早期限，回滚的事务不改；卸载、删档、撤销新建时实例离开索引。只有最早期限到了才经 Nest 正式准入给那一个实体投递一次 Tick，不按在线人数轮询；投递前核对实例，已卸载 / 已删档 / 被同 ID 新实例取代的丢弃。时间取 App 业务时钟，按毫秒。

**离线语义（维护者 2026-10-17 决定）**：框架不追帧。实体不在内存期间（离线、停服）到期的节点，在实体加载起来之后只触发一次，不论错过多少个周期。回调参数 `entitytimer.Fire` 带足业务自己补算的信息：

| 字段 | 含义 |
| --- | --- |
| `Deadline` | 原定期限：最近一次登记 / 改期 / 续期算出的到期时刻 |
| `Now` | 实际触发时刻（这次 Tick 的业务时间） |
| `Overdue` | 原定期限不晚于本实例加载时刻：加载后的补触发 |
| `LoadedAt` | 本实例加载时刻（本进程新建的为零值） |
| `Interval` | 最近一次登记 / 续期用的间隔（周期定时器的周期） |

回调返回值与 Scheduler 一致：正值是下次间隔，从 `Now` 起算；0 或负值结束。错过几个周期由业务算（例如 `(Now - Deadline) / Interval`），框架不计算、不循环补触发。驱动不唤醒不在内存的实体：离线实体到期只在下次加载时处理，需要“离线也准点”的到期动作要另外的全局扫描机制（不在本机制范围）。

回调内可以通过同一组件登记下一阶段、取消或改期其他节点，以及显式改期自己。它们共享当前 Tick 的调度视图，当前回调结束后按 Scheduler 的组合规则落定，最外层 Tick 才报告新的最早期限；不会从中间 DAO 状态重建另一份堆。这个视图只属于当前调用栈，退出或 panic 即清掉，事务回滚仍只撤销 DAO。回调里的 `Timer` / `Timers` 读的也是这份视图：刚登记的、刚改期的立即可见（新期限），刚取消的立即不可见，正在触发的节点自己可见（原定期限，显式改期后为新期限），与 `ChangeTimer` / `RemoveTimer` 能否成功一致，所以“没有就建一个”不会在一次 Tick 内重复登记。回调之外读 DAO。

不支持 Remote 托管实体（`remote=managed`）与 `noPersist` 实体，`roost add timer` 拒绝并说明原因：前者所有权在进程间迁移，驱动只索引本进程加载的实例；后者没有可补触发的存储。生成物、归属与命令细则见 [CODEGEN_REFERENCE“roost add timer”](../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-timer按实体的持久定时器)，实现契约见 [impl/10-time §2A](../impl/10-time.md#2a-按实体的持久定时器驱动)。game-demo 的 World 不用这个生成器：demo 是手写的参考实现，不依赖生成器产物（维护者 2026-10-18），它手写组件、Store 适配、Tick handler 与驱动接线，直接调用 `framework/nest/entitytimer`（`timer_component.go`、`tick_world_timers.go`、game 服务 `activity.go` 的 `startWorldTimers`），是不用生成器时接入同一套驱动的写法。

## 4A. 实体心跳（周期推进）

所有跑在 Nest 业务池上的实体周期逻辑——移动、actionflow / AI 的 tick、周期性伤害与回复、冷却推进、房间或场景实体自己的逻辑帧——都以 `gameplay/heartbeat` 为底层（DESIGN-TODO D20，维护者 2026-10-21 定方案，第 1 期已实施）。不要再写“服务 goroutine 每 N 秒给实体发一条消息”的循环。

**它是什么**：每个到期的登记由驱动投一条单目标 Nest 消息，框架注册的心跳 handler 在**实体自己的 Nest worker 上、实体锁与事务内**调用回调，与该实体的其他消息串行；快池实体在快池、慢池实体在慢池（Nest 按实体种类分流）。所以回调里能做的事与普通 handler 相同：改 DAO、记 effect、`QueueMove`，Sync 冻结与提交门照常成立。回调只做内存计算和入队，**不阻塞、不做 I/O、不 `Dispatch` / `Request`**（快池规则）；返回错误时本拍事务回滚。

**用法**：一个 Nest 引擎一个驱动，写成包级变量；组件取宿主、登记、按自己的状态启停；进程装配用 `wiring/heartbeat`。

```go
// 业务包：一个驱动（与 Nest 引擎一一对应）。
var Heartbeat = heartbeat.NewDriver(heartbeat.Config{Name: "game"})

func (c *MoveComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error {
	c.hb = Heartbeat.NewHost(c.owner)
	reg, err := c.hb.Register("move", 100*time.Millisecond, c.onBeat)
	if err != nil {
		return err
	}
	c.move = reg
	if c.dao.Moving() { // 心跳不持久：加载后按自己的持久状态决定是否启动
		reg.Start()
	}
	return nil
}

func (c *MoveComponent) OnDestroy(entity.EntityDestroyReason) { c.hb.OnDestroy() }

func (c *MoveComponent) onBeat(b heartbeat.Beat) error {
	arrived := c.advance(b.Elapsed) // 按经过时间推进，不按“第几拍”
	if arrived {
		c.move.Stop()
	}
	return nil
}

// 进程装配（依赖 Nest Mod，先于 Nest 停止）：wiringheartbeat.NewMod(entityAccess, Heartbeat)
```

| API | 说明 |
| --- | --- |
| `Driver.NewHost(entity)` | 每个实体实例一个宿主（Remote 托管、慢池实体都可以） |
| `Host.Register(name, period, fn, opts...)` | 登记，返回停止状态的 `*Reg`；name 在实例内唯一。一个实体可以有多个登记，**各自投递、不合并** |
| `Reg.Start` / `Stop` / `SetPeriod` | 启停、换档。在事务里调用时**提交后生效、回滚作废**；不在事务里（加载发布前）立即生效。回调里调用的随本拍事务 |
| `Host.OnDestroy()` | 卸载、删档、撤销新建时由组件调用：实例永久离开驱动，之后的启停被忽略 |
| `WithDurability(policy)` | 本登记心跳 handler 的持久级别，缺省 `async`。心跳本身什么都不持久；回调改了持久字段时按这里提交（`memory` 下改持久字段在提交点被拒） |

**档位**：只有 10 个固定档位，不支持任意周期（维护者决定 ⑥）：50ms、100ms、200ms、500ms、1s、2s、5s、10s、30s、60s。建议：移动 100ms、AI 200ms、Buff / 回复 / 冷却 1s，空闲时 `Stop`（停止的登记不占调度）。驱动 10ms 一格，**相位 = hash(实体 ID)**：同档的实体均匀落在周期内的各格，没有整点尖峰；同一实体的同档登记落在同一格（仍是各自一条消息）。首拍在启动生效后的 (0, 周期] 内。

**`Beat` 与跳拍语义**：

| 字段 | 含义 |
| --- | --- |
| `Now` | 本拍业务时间（App 业务时钟） |
| `Elapsed` | 距本登记上次执行（首拍：距启动生效）的业务时间，夹到 `[0, max(1s, 档位)]`：掉拍、停顿、业务时钟被调整时不让一拍走出几公里 |
| `Period` | 本登记当前档位 |
| `Skipped` | 上次执行以来跳过的拍数，只供观测 |
| `First` | 本次启动后的第一拍（重载后重新启动、Stop 后再 Start 都算新的一次） |

- **不追帧**：每个登记至多一拍在途。上一拍还没执行完（还在排队或执行中）时到期的拍**跳过**，不累积、不补发；快池准入满（`ErrQueueFull`）同样按跳拍处理。跳拍计 `heartbeat.skipped_total{reason}` 并打限频 Warn（缺省 10 秒一条，汇总这段时间的计数）。回调按 `Elapsed` 算一次：移动 = 速度 × Elapsed。需要逐 tick 确定性的逻辑（技能 Runtime）在回调里把自己推进到 `Now` 对应的 tick，那是业务自己的推进，不是框架追帧。
- **卸载窗口**：驱动投递前核对实例（已卸载、已被同 ID 新实例取代、还没发布的不投，不唤醒不在内存的实体）；执行时再核对实例与令牌，卸载后同 ID 重载的新实例收到旧实例的拍时按过期丢弃，不会在新实例上跑旧登记的回调（维护者决定 ⑨）。投递之后、执行之前被卸载的实体可能因这条消息被 Nest 冷加载一次，与发给它的任何消息相同。
- **Remote 托管实体**：只在当前拥有方跳。本进程实例处于独占拥有（`LocalOwned`）时投递并执行；首次接触（`Unknown`）交给 Nest 的 Remote 写准入判定；转移中、已隔离、共享模式不投。所有权迁移时旧拥有方在离开 `LocalOwned` 后不再执行任何一拍，新拥有方加载实例（组件重新登记）或实例回到 `LocalOwned` 后开始跳，两边不会同时跳。每拍要经过一次 Remote 写准入（I/O 阶段），比普通实体贵，档位选低一些。共享模式下 P1 不跳（见 [KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)）。

**与按实体持久定时器（§4）的分工**：持久、准点、离线也要算的事走 entitytimer（Buff 到期写档、建造完成、行军到达、活动阶段、离线产出）；在线周期推进走心跳（移动、AI、周期效果、冷却、逻辑帧）。同一件事可以两者都用：行军到达是 entitytimer 期限，行军途中给 AOI 的位置是 1～2Hz 心跳。心跳不持久，加载后由组件重新启动；entitytimer 的驱动不改（维护者决定 ⑫）。房间、场景本身是实体，它们的逻辑帧就是自己的心跳登记，不提供“非实体心跳”（决定 ⑬）。

实现契约见 [impl/10-time §2B](../impl/10-time.md#2b-实体心跳)，指标见 [OBSERVABILITY“实体心跳”](../OBSERVABILITY.md#实体心跳gameplayheartbeat)。

## 5. 运维

区分业务时间推进失败与机器系统时钟漂移；日志与故障记录使用系统时间。glsvet 的时钟检查提供静态提示，不能识别任意封装后的错误时钟。开发测试同时注入两个时钟，验证推进业务钟不会导致基础设施超时。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
