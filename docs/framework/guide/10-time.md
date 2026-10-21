# 时间与定时器：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/10-time.md) · [模块总目录](../README.md)

## 先理解这一块

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

选择时钟时先问：这是游戏规则中的期限，还是网络、租约、停机的等待时间？

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 双时钟

业务时间表达玩法活动、冷却和可测试的游戏期限；系统时间表达 lease、网络期限、重试、WAL、日志和停机。业务 Now 与 SystemNow 分别注入；Nest 消息里的业务时间读 `fctx.Now()`（§1A），服务的缺省是 `app.BusinessClock`，系统 time.Now。不能把加速玩法时间同时作用到 Redis 租约或写超时。

## 1A. 业务时间一律取自 ctx（D22-1）

DESIGN-TODO D22（维护者 2026-10-21 决定，照 ssr：业务里的时间一律从 ctx 取）。**Nest 消息里的业务时间唯一入口是 `fctx.Now()`**（`fctx.NowMilli()`）：每条消息、每一拍在执行开始时冻结一次（进程业务时钟 = 真实时间 + `time.logic_offset`），同一条消息里读多少次都是同一个值。所以 handler、心跳的 `Beat.Now`、entitytimer 的 `Fire.Now`、actionflow / AI 的缺省时间、生成的分发包装填的 `nowUnix` 参数是同一时刻，同一事务里不会“前一行判定没过期、后一行按另一个时刻记账”。

| 在哪里 | 业务时间从哪里来 |
| --- | --- |
| Nest handler、心跳回调、entitytimer 回调、actionflow / AI / 技能的回调 | `fctx.Now()`；不直接读 `app.BusinessClock` 或 `clock.Now()`（glsvet 对 Nest handler 里的这两种读法打 `hint:`，确属现读的写 `//glsvet:live-business-clock <理由>`） |
| handler 参数 `nowUnix` / `nowUnixMilli`（`int64`） | 生成的 Nest 分发包装在 handler **执行时**从 `fctx` 填（`fctx.Now().Unix()` / `fctx.NowMilli()`）；不在消息里传，生成的 Sender 签名里没有它，endpoint 也不填。排队、冷加载、重试的耽搁不算进“之前”。handler 仍可在单测里直接传值 |
| 心跳 `Beat.Now` / `Elapsed` | 这一拍消息冻结的 `fctx.Now()`（驱动缺省时钟就是 `fctx.Now`） |
| entitytimer 的 Tick | Tick 消息不带时间；`Host.Tick()` 按这条消息冻结的业务时间触发，`Fire.Now` 与回调里读的 `fctx.Now()` 相同。handler 里增删改定时器的起点也是它 |
| `actionflow` / `ai` / `timer.Scheduler` 没给时间时 | `fctx.Now()` |
| 不在 Nest 消息里：服务 goroutine（活动协调器、匹配、spawner 的决策循环）、admin / GM 命令、启动阶段、驱动自己的循环 | 照旧读 `app.BusinessClock(registry)`（服务经 `Config.Now` 注入）；`fctx.Now()` 在这些地方现读进程业务时钟 |

边界：

- **冻结点**：请求上下文建立时（`fctx.NewContext`）。Nest 消息是逻辑阶段开始执行时；I/O 准备之后换池续行重新冻结（交接快照 `ContextSnapshot` 不带它）。实体锁等待的时间算在冻结之后：冻结的是“这条消息开始执行”的时刻，不是拿到锁的时刻。
- **进程业务时钟与 Registry 业务时钟**：两者读同一份 `time.logic_offset`，App 启动时设进程级偏移（`clock.SetOffset`）。心跳、entitytimer 的进程装配因此不再 `SetClock`，驱动缺省读 `fctx.Now`；`SetClock` 只给测试。只建 Registry、不启动 App 的测试里两者可以不同，这时用 `clock.SetOffset` 或 `clock.SetSource`（测试用：换真实时间来源，可停住或单步）设进程业务时钟。
- **一条消息里的派生值**：`fctx.Frozen(key, compute)` 让一次执行里第一次算出的值被冻结（与 `Now` 同生命期、不进快照）；时间域（§4D）用它保证一条消息读到同一个域时间。

不兼容（项目未上线，不保留旧行为）：生成的 Sender 去掉业务时间参数；`roost add timer` 生成的 Tick handler 去掉 `nowUnixMilli` 参数、组件的 `Tick()` 无参数；`entitytimer.Host.Tick()` 无参数（要指定时刻用 `TickAt`）。game-demo 去掉了 `executionNowUnix`（取调用方时间与持锁后时钟的较大者）——现在 `nowUnix` 本身就是执行时刻。

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
- 时刻本身仍是业务时间（带 `time.logic_offset`）：Nest handler 里用本条消息冻结的 `cal.BusinessDay(fctx.Now())`（§1A），同一事务里判“今天领过没有”与记下的业务日是同一天；Nest 之外的服务用 `cal.BusinessDay(app.BusinessClock(r).Now())`。

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
- **触发在实体的 Nest 锁内**：生成的 Tick handler（`rollback=undo durability=strict`）调用组件的 `Tick()`，按这条消息冻结的业务时间（`fctx.Now`，§1A）触发；回调改的东西与节点的删除 / 续期是同一条 WAL 记录。回调只改 DAO、记 effect，不做 I/O、不阻塞。
- **驱动在 core**（`framework/nest/entitytimer`，`wiring/entitytimer` 装配）：进程内按“已加载实例的最早期限”排的小堆。实例加载时报告存量的最早期限，事务提交后（`nest.AfterCommit`）报告新的最早期限，回滚的事务不改；卸载、删档、撤销新建时实例离开索引。只有最早期限到了才经 Nest 正式准入给那一个实体投递一次 Tick，不按在线人数轮询；投递前核对实例，已卸载 / 已删档 / 被同 ID 新实例取代的丢弃。时间取 `fctx.Now`（Nest 消息里是这条消息冻结的业务时间，驱动自己的循环现读进程业务时钟），按毫秒；Tick 消息不带时间。

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

**它是什么**：每个到期的登记由驱动投一条单目标 Nest 消息（缺省；配置 `heartbeat.delivery: batch` 时改为按 64 个一批的 Nest 批量作业，同一实体有已准入、还没执行完的消息时这一拍排在它后面，Remote 托管实体仍每拍一条单目标消息；批量方式未经正式压测，缺省关闭），框架注册的心跳 handler 在**实体自己的 Nest worker 上、实体锁与事务内**调用回调，与该实体的其他消息串行；快池实体在快池、慢池实体在慢池（Nest 按实体种类分流）。所以回调里能做的事与普通 handler 相同：改 DAO、记 effect、`QueueMove`，Sync 冻结与提交门照常成立。回调只做内存计算和入队，**不阻塞、不做 I/O、不 `Dispatch` / `Request`**（快池规则）；返回错误时本拍事务回滚。

**用法**：一个 Nest 引擎一个驱动，写成包级变量；组件取宿主、登记、按自己的状态启停；进程装配用 `wiring/heartbeat`。

```go
// 业务包：一个驱动（与 Nest 引擎一一对应）。
var Heartbeat = heartbeat.NewDriver(heartbeat.Config{Name: "game"})

func (c *RegenComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error {
	c.hb = Heartbeat.NewHost(c.owner)
	reg, err := c.hb.Register("regen", time.Second, c.onBeat)
	if err != nil {
		return err
	}
	c.regen = reg
	if c.dao.Regenerating() { // 心跳不持久：加载后按自己的持久状态决定是否启动
		if err := reg.Start(); err != nil { // 只在 Remote 共享模式下返回 ErrHeartbeatRemoteShared
			return err
		}
	}
	return nil
}

func (c *RegenComponent) OnDestroy(entity.EntityDestroyReason) { c.hb.OnDestroy() }

func (c *RegenComponent) onBeat(b heartbeat.Beat) error {
	full := c.regen(b.Elapsed) // 按经过时间推进，不按“第几拍”
	if full {
		c.regen.Stop()
	}
	return nil
}

// 进程装配（依赖 Nest Mod，先于 Nest 停止）：wiringheartbeat.NewMod(entityAccess, Heartbeat)
```

控制实体行为的逻辑（移动、技能、AI 决策）不这样直接挂组件心跳，而是写成动作、由 `actionflow.BeatRunner` 挂心跳（维护者 2026-10-21，见 §4B）。

**生成工程的入口**：`roost add heartbeat --entity <Entity>` 生成上面这套接线——工程唯一的驱动 `game/heartbeat/driver_gen.go`（`Driver`）、实体的 `HeartbeatComponent`（构造时 `Driver.NewHost`，`OnDestroy` 时 `Host.OnDestroy`，`Register(name, period, fn)` / `Beat(name)`），并把 `heartbeat` Mod 加进实体所在服务、bootstrap 装 `wiring/heartbeat`（Nest 之后启动、之前停止，服务 Init / Shutdown 不用改）。业务只写 `heartbeat_beats.go` 的 `onHeartbeatInit`（加载 / 新建时登记，示例登记是停止状态）。Mod 按需装配，不进新工程的缺省 Mod 集：实体侧零开销，但驱动空转也每 10ms 走一格、多一个 Mod 停机预算多 3s。细则见 [CODEGEN_REFERENCE“roost add heartbeat / behavior / timedomain”](../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-heartbeat--behavior--timedomain实体心跳行为与时间域)。

| API | 说明 |
| --- | --- |
| `Driver.NewHost(entity)` | 每个实体实例一个宿主（Remote 托管、慢池实体都可以） |
| `Host.Register(name, period, fn, opts...)` | 登记，返回停止状态的 `*Reg`；name 在实例内唯一。一个实体可以有多个登记，**各自投递、不合并**。Remote 托管实体处于共享模式时返回 `ErrHeartbeatRemoteShared` |
| `Reg.Start` / `Stop` / `SetPeriod` | 启停、换档。在事务里调用时**提交后生效、回滚作废**；不在事务里（加载发布前）立即生效。回调里调用的随本拍事务。`Start` 返回 `error`：Remote 托管实体处于共享模式时返回 `ErrHeartbeatRemoteShared`、不启动，其余情况为 nil |
| `Reg.Close()` | 撤销一个登记：立即离开驱动、释放名字（同一实例上马上可以同名再 `Register`），之后 `Start` / `SetPeriod` 返回 `ErrRegClosed`。已经投出、还在队列里的拍到达时不执行回调（按过期丢弃）。与 `Register` 一样不随事务回滚。行为组件换场景重建时经 `Behavior.Close` / `Controller.Close` 调用（§4D） |
| `Host.OnDestroy()` | 卸载、删档、撤销新建时由组件调用：实例永久离开驱动，之后的启停被忽略。组件漏调时，驱动在登记到期、发现实例已移除时替它撤销（计 `dropped{reason="removed"}`），但要等到期，最多一个最长档位的周期 |
| `WithDurability(policy)` | 本登记心跳 handler 的持久级别，缺省 `async`。心跳本身什么都不持久；回调改了持久字段时按这里提交（`memory` 下改持久字段在提交点被拒）。**`strict` 每一拍都在实体 worker 上、持实体锁等提交落盘**（改了持久字段的拍要等一次 WAL 持久化才释放 worker），快池实体的高频心跳不要用；要“这一拍的结果一定落盘才算”的少数登记才用，并选低档位 |
| `WithTimeDomain(d)` | 本登记按场景的时间域计时（§4D）：调度仍按真实时间，`Beat.Now` / `Elapsed` 是域时间 |

**档位**：只有 10 个固定档位，不支持任意周期（维护者决定 ⑥）：50ms、100ms、200ms、500ms、1s、2s、5s、10s、30s、60s。建议：移动 100ms、AI 200ms、Buff / 回复 / 冷却 1s，空闲时 `Stop`（停止的登记不占调度）。驱动 10ms 一格，**相位 = hash(实体 ID)**：同档的实体均匀落在周期内的各格，没有整点尖峰；同一实体的同档登记落在同一格（仍是各自一条消息）。首拍在启动生效后的 (0, 周期] 内。

**`Beat` 与跳拍语义**：

| 字段 | 含义 |
| --- | --- |
| `Now` | 本拍业务时间：这一拍消息冻结的 `fctx.Now`（§1A），回调里再读 `fctx.Now` 是同一个值；挂了时间域（§4D）时是域时间 |
| `Elapsed` | 距本登记上次执行（首拍：距启动生效）的业务时间，夹到 `[0, max(1s, 档位)]`：掉拍、停顿、业务时钟被调整时不让一拍走出几公里。挂了时间域时是域时间的经过，上限再乘倍率，时停时为 0 |
| `Period` | 本登记当前档位 |
| `Skipped` | 上次执行以来跳过的拍数，只供观测 |
| `First` | 本次启动后的第一拍（重载后重新启动、Stop 后再 Start 都算新的一次） |

- **不追帧**：每个登记至多一拍在途。上一拍还没执行完（还在排队或执行中）时到期的拍**跳过**，不累积、不补发；快池准入满（`ErrQueueFull`）同样按跳拍处理。跳拍计 `heartbeat.skipped_total{reason}` 并打限频 Warn（缺省 10 秒一条，汇总这段时间的计数）。回调按 `Elapsed` 算一次：移动 = 速度 × Elapsed。需要逐 tick 确定性的逻辑（技能 Runtime）在回调里把自己推进到 `Now` 对应的 tick，那是组件自己的推进，不是框架追帧（`combatcomponent.RuntimeBeat`，§4E）。
- **卸载窗口**：驱动投递前核对实例（已卸载、已被同 ID 新实例取代、还没发布的不投，不唤醒不在内存的实体）；执行时再核对实例与令牌，卸载后同 ID 重载的新实例收到旧实例的拍时按过期丢弃，不会在新实例上跑旧登记的回调（维护者决定 ⑨）。投递之后、执行之前被卸载的实体可能因这条消息被 Nest 冷加载一次，与发给它的任何消息相同。
- **Remote 托管实体**：只在当前拥有方跳。本进程实例处于独占拥有（`LocalOwned`）时投递并执行；首次接触（`Unknown`）交给 Nest 的 Remote 写准入判定；转移中、已隔离不投。所有权迁移时旧拥有方在离开 `LocalOwned` 后不再执行任何一拍，新拥有方加载实例（组件重新登记）或实例回到 `LocalOwned` 后开始跳，两边不会同时跳。每拍要经过一次 Remote 写准入（I/O 阶段），比普通实体贵，档位选低一些。
- **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21）：共享模式（`EnterRemoteSharedMode` 之后）各写者进程的实例都是 `Shared`、都经分布式锁写，本地分不出谁该跳，心跳无法执行。共享与否是**运行期**的所有权状态（同一种类的实体可以独占、也可以共享，`LeaveRemoteSharedMode` 回到独占），不是种类声明，所以分两处拦：
  - 实例已在共享模式（`Sharing` / `Shared`）时，`Register` 与 `Start` 返回 `ErrHeartbeatRemoteShared`，不登记 / 不启动；
  - 登记时合法、之后才进入共享的（拥有方 `EnterRemoteSharedMode`；或第二个写者加载共享实体时实例是 `Unknown`，第一拍的写准入把它转到 `Shared`）：下一拍在实体锁内**停掉这个登记**（`Active()` 变为 false），回调不执行，计 `heartbeat.stopped_total{reason="remote_shared"}` 并打限频 Warn（与跳拍相同的 `WarnInterval`，缺省 10 秒一条，汇总个数）。实体上的每个已启动登记都在自己下一次到期时被停掉，所以全部停掉最多要一个最长档位的周期；这期间没有任何一拍会执行回调。**回到独占不自动恢复**（维护者 2026-10-21 决定）：`LeaveRemoteSharedMode` 之后被停掉的登记保持停止，业务调用 `Start` 之后才恢复，避免意外重启（共享期间业务状态可能已被其他写者改过，是否还要跳由业务判断）；
  - 共享实体的周期逻辑由业务层自己驱动，例如由负责它的服务按周期给它发 Nest 消息。

**与按实体持久定时器（§4）的分工**：持久、准点、离线也要算的事走 entitytimer（Buff 到期写档、建造完成、行军到达、活动阶段、离线产出）；在线周期推进走心跳（移动、AI、周期效果、冷却、逻辑帧）。同一件事可以两者都用：行军到达是 entitytimer 期限，行军途中给 AOI 的位置是 1～2Hz 心跳。心跳不持久，加载后由组件重新启动；entitytimer 的驱动不改（维护者决定 ⑫）。房间、场景本身是实体，它们的逻辑帧就是自己的心跳登记，不提供“非实体心跳”（决定 ⑬）。

实现契约见 [impl/10-time §2B](../impl/10-time.md#2b-实体心跳)，指标见 [OBSERVABILITY“实体心跳”](../OBSERVABILITY.md#实体心跳gameplayheartbeat)。

## 4B. 移动与动作：心跳驱动

移动与 actionflow 动作都跑在实体自己的心跳上（DESIGN-TODO D20 第 2 期）。两条维护者决定（2026-10-21）：

- **移动由动作驱动**（参考 ssr：action 挂心跳、在心跳里调 move）：`Move` 是被动组件，只提供设定与推进接口；`MoveTo` / `Follow` 动作由实体的 `ActionRunner` 驱动，runner 给动作所在的组挂心跳，每拍动作调 `Move.Advance`。
- **所有控制实体行为的逻辑都经动作**：移动、技能、AI 决策等统一经 `ActionRunner` 下发与收尾，不绕过动作直接驱动组件。

框架给的东西，完整接法见 [`gameplay/movement` 的 Example](../../../gameplay/movement/example_test.go)：

| 类型 | 包 | 做什么 |
| --- | --- | --- |
| `BeatRunner` | `gameplay/actionflow` | 让实体的 `ActionRunner` 由心跳 tick：每个组的当前动作挂这个组的心跳（`"actionflow/<组号>"`，周期取动作的 `TickInterval`），组空闲时停 |
| `MapBasic` | `gameplay/movement` | 所在场景、位置、朝向，以及与 AOI 的接线：进场景 `Enter`、位置变化 `QueueMove`、离开 `Leave`；落库由业务选 |
| `Move` | `gameplay/movement` | 被动：速度、路径或跟随目标、状态；`MoveTo` / `Follow` 设定、`Advance(token, elapsed)` 推进一次、`Stop`。自己不挂心跳 |
| `MoveTo` / `Follow` 动作 | `gameplay/movement` | `RegisterActions` 以业务选定的种类注册；Start 设定移动，每拍 Tick 调 `Advance(ctx.Elapsed)`，移动结束即动作结束 |

寻路这类重计算在发起 `MoveTo` 之前做，路径作为参数；框架没有巡逻组件，巡逻是 AI 策略按自己的状态发起 `MoveTo`。

**参考接法：game-demo**（D20 第 3 期，[WALKTHROUGH“移动”“怪物”“场景时间”](../../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20)）：玩家的客户端只发目标（`move_target`），handler 找一次路、最后一步 `CreateAction(MoveTo)`，每拍由玩家自己的心跳推进并 `QueueMove`，观察者经 Entity Sync 看到每一步；按点移动保留为瞬移。怪物挂 `Behavior` + AI 心跳巡逻，定身 buff 经 `BindStates` 派生 `Rooted`，`state.Gate` 冻结移动组；场景实体的 1Hz 心跳决定刷怪缺额、提交后交给进程侧建实体；场景挂时间域，成员的行为与 AI 跟随域时间。demo 手写接线（不装 `wiring/heartbeat` Mod），端到端用例用手动推进的业务时钟逐格核对。

**接法**：实体一个心跳宿主，组件共用；实体实现 `movement.MapOwner`（被跟随时读位置）与 `movement.MoveOwner`（动作找组件），`OnDestroy` 里先 `MapBasic.OnDestroy` 再 `Host.OnDestroy`。

```go
m.host = driver.NewHost(m)
m.mb, _ = movement.NewMapBasic(m, m.host, movement.MapConfig{Fields: daoFields, SaveOnLeave: true, Save: saveToDAO})
m.move, _ = movement.NewMove(m.mb, movement.MoveConfig{Locate: movement.LocateIn(entityManager.Get)})
m.flow, _ = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{Registry: registry, GroupForKind: groups}, actionflow.BeatConfig{Host: m.host})

// handler 里（实体锁与事务内）：先做完会失败的校验与修改，最后一步才调用动作接口。
if err := m.mb.Enter(sceneID, movement.InterestSpace{Interest: interest}, at); err != nil {
	return err
}
_, err := m.flow.Runner().Start(ActMoveTo, movement.MoveToParam{Path: path, Speed: 6}, 0, time.Time{})
return err
```

**事务与 runner（业务约束，维护者 2026-10-21：由业务层保证，框架不处理）**：`ActionRunner` / `MissionRunner` 是纯内存状态，**不随 Nest 事务回滚**。handler 先做完全部可能失败的校验和修改，**最后一步**才调用 `Start` / `Enqueue` / `End` 等；之后若还可能失败，回滚后 runner 里会残留这次的变更（例如一个已启动的 MoveTo），而心跳登记与 `Move` 的状态随回滚恢复——残留的 MoveTo 下一拍 `Advance` 得到 `Replaced`，以取消结束。心跳那一拍里动作 Tick 对组件（DAO、`Move`）的修改随这一拍的事务提交或回滚；拍的回调不返回错误，所以这一拍只在提交失败时回滚，那时 runner 已经按 Tick 的结论推进，框架不处理。

**BeatRunner**：

- 动作可选实现 `actionflow.TickIntervaler`：不实现按 `BeatConfig.Period`（缺省 100ms）；返回 0 表示不需要周期 tick（由别的动作或 handler 结束它），这个组不挂心跳；返回正数取不大于它的最长档。组的当前动作切换时按新动作的周期启停这个组的心跳；同一实体的多个组各自一个登记、各自投递（与 ssr 相同，决定 ③ 不合并）。移动中的实体只有移动组一个登记，每拍一条消息。
- 一拍只 tick 这个组；`ActionContext.Elapsed` 是本拍的 `Beat.Elapsed`（不追帧），按时间推进的动作用它。
- 组的心跳挂不上（Remote 托管实体处于共享模式，§4A）时，这个动作以失败结束（原因带 `ErrHeartbeatRemoteShared`）；运行中进入共享模式、框架停掉了心跳时，动作留在 runner 里不推进，回到独占后调用 `BeatRunner.Sync()` 重新挂上（共享期间它返回错误）；`Behavior` 的 `Beat().Sync()` 同时重新挂上任务心跳。
- 技能以后用同一个 runner：行为都是动作，由动作所在组的心跳 tick。AI 的**决策**有自己独立的心跳（`ai.NewBeatController`，§4C），它下发的动作仍由动作组的心跳推进。

**MapBasic**：

- 状态的权威是业务 DAO 的字段（`movement.Fields`：场景 ID、位置、朝向）；位置与朝向声明为 nopersist、参与同步，组件不另存一份。位置**不每拍落库**：`SaveOnLeave` 离开场景时、`SaveEvery`（心跳 `"map.save"`）周期性地调 `Save` 写业务的持久字段，位置没变过不写。共享模式下带 `SaveEvery` 的 `Enter` 返回 `ErrHeartbeatRemoteShared`、什么都不改；运行中进入共享模式被框架停掉的周期落库，回到独占后目前只能 `Leave` 再 `Enter` 恢复（见 KNOWN-LIMITS“移动与动作”）。
- 位置变化 `SetPos` 先 `Space.QueueMove`（事务内只记事实、走提交门，提交确认后进 AOI、回滚丢弃），再写字段；`QueueMove` 被拒时什么都不写、返回错误。
- 进出场景 `Enter` / `Leave` 在**事务提交、实体锁释放之后**调 `Space.Enter` / `Space.Leave`（回滚的进出不发生；同一事务里进场景后又移动，提交后的 Enter 直接用最新位置；先进后出互相抵消）。不在事务里（加载 / 新建的初始化阶段）立即调用。
- `Space` 是接口，`InterestSpace` 把 `policy.Interest` 适配过来：`Player` 认出的实体按玩家进出（`Interest.Enter` / `Leave`），其余只被看（`Show` / `Hide`），进出后 `Apply` 一次。同步 subject 的登记、玩家会话由业务在进场景之前做；业务场景也可以自己实现 `Space`。
- `Published()` 是最近一次**提交**的快照，任意 goroutine 无锁读取：跟随者读目标位置用它，不取目标的锁。它在提交后、实体锁释放之后才更新，所以在同一拍的事务里观察到的事件（例如动作结束）早于这次发布。
- 心跳不持久、组件也不持久“在场景里”：实体重新加载后由业务重新 `Enter`。只从内存卸载不等于离开场景（`OnDestroy` 不调 `Leave`）。

**Move**：

- `MoveTo(path, speed)` 沿路径走、`Follow(target, speed, keep, stopWhenReached)` 跟随，返回编号；新的取代进行中的（旧编号的 `Advance` 得到 `Replaced`），`Stop(token)` 停。
- `Advance(token, elapsed)` 走 `速度 × elapsed`：**不追帧**，掉拍后下一拍一次走完整段；不足一格的位移累积在亚格位置里，按格写 `MapBasic`，朝向取本拍位移方向。`Terrain` 非 nil 时每进一个新格问一次，被挡就停在最后一个可走的格（`Blocked`）。返回 `Result`：`Reason` 为 0 表示还在移动。
- `Follow` 每拍读目标的 `Published`：目标不在内存（卸载、删档）→ `TargetLost`，不在本场景 → `TargetLeft`；走到 `keep` 格以内停住，`stopWhenReached` 时以 `Arrived` 结束，否则继续跟随。
- 自己离开场景 → `LeftScene`；AOI 排队满（`policy.ErrInterestQueueFull`，背压）时整拍作废、下一拍接着走；其他写位置错误 → `Refused`。
- 推进状态（亚格位置、路径进度）随事务回滚恢复：回滚的一拍不算走过，回滚的 `MoveTo` 不生效。
- 动作结论：`Arrived` → 成功，`Replaced` → 取消，其余 → 失败（原因是 `StopReason`）；动作被取消（含被同组新动作取代）时停掉自己那次移动。

边界见 [KNOWN-LIMITS“移动与动作”](../../maintenance/KNOWN-LIMITS.md#移动与动作d20-第-2-期)，实现契约见 [impl/10-time §2C](../impl/10-time.md#2c-移动与动作的心跳驱动)。

## 4C. 行为控制：Behavior、取代、冲突与冻结

DESIGN-TODO D21（吸收 ssr 的 action / action_list / mission / ai / state，维护者 2026-10-21 决定调研一 13 条全部按推荐实施）。本节随分期补全；第 1 期是 actionflow 本身的扩展。

**`actionflow.Behavior`：框架的 ActionList 实现**（对照 ssr 的 action_list 模块）。一个实体一个，把 `ActionRunner`（配置 `Beat` 时由 `BeatRunner` 驱动）、`MissionRunner` 与上层观察者（通常是 `ai.Controller`）按固定顺序接好：

```go
m.behavior, _ = actionflow.NewBehavior(m, actionflow.BehaviorConfig{
	Registry: registry, GroupForKind: groups,
	Beat:     &actionflow.BeatConfig{Host: m.host}, // 不配则不挂心跳（测试 / 手动 tick）
	Observer: m.ai,                                 // *ai.Controller 实现 actionflow.Observer
	OnError:  logError,
})
// handler 里最后一步：m.behavior.CreateAction(ActMoveTo, param) / m.behavior.SetMission(MissionPatrol, plan)
```

- 动作结束先交给当前任务，再交给观察者；任务结束先取消属于它的动作，再交给观察者。所以一个任务在处理动作结束时自己结束了，观察者先收到任务结束、再收到这次动作结束。
- 任务回调运行期间（`Start` / `Tick` / `OnActionEnd`，含它们同步引出的回调）经 `CreateAction` / `EnqueueAction` 发起的动作**归属当前任务**，任务结束时一并取消；AI 或 handler 发起的不属于任何任务。
- `Clear(reason)` 清场：先结束任务、再强制结束全部动作，之后什么都不在运行。
- 和 runner 一样只在实体锁内调用、不随事务回滚（§4B 的业务约束照旧）。
- **卸载与撤销**：组件在实体的 `OnDestroy` 里先 `controller.Close(reason)`（AI 卸下策略、撤销 `"ai"` 心跳），再 `behavior.OnDestroy()`：在途动作与任务以 `actionflow.ErrOwnerDestroyed`（取消状态）各结束一次，按 ID 等结果的一方有结论，动作指标的 `running` 减回；最后心跳宿主 `Host.OnDestroy()`。这时不在事务里，动作的 `Cancel` 只做内存收尾。`Behavior.Close(reason)` 是同一件事、原因由调用方给，用于换场景重建（§4D）；之后经 Behavior 发起的动作与任务返回 `ErrBehaviorClosed`。生成的 `BehaviorComponent.OnDestroy` 已按这个顺序调用。
- **任务心跳**（第 4 期）：配了 `Beat` 时，当前任务实现 `actionflow.TickIntervaler` 且返回正数，Behavior 给它登记 `"<Beat.Name>/mission"`（周期取不大于它的最长档），每拍 `MissionRunner.Tick`；任务结束或不再需要时停。不实现的任务不被周期 tick（由动作结束推进）。挂不上（Remote 共享模式）时任务以失败结束；运行中被框架停掉的（进入共享模式），回到独占后由 `Beat().Sync()` 重新挂上，任务不被结束。

**计划步骤的时限与重试**（`MissionStep.Timeout` / `Retry`，`MissionPlan.Period`）：

```go
plan := actionflow.MissionPlan{Steps: []actionflow.MissionStep{
	{Action: ActChase, Param: chase, Timeout: 5 * time.Second, Retry: 2, OnFail: actionflow.NextMissionStep(2)},
	{Action: ActAttack, Param: attack},
	{Action: ActReturn, Param: home},
}}
```

- `Timeout` 是这一步**每次尝试**的时限：到时任务把这一步的动作以过期（`ActionStatusExpired`）结束，按失败处理。需要任务心跳（Behavior 配了 `Beat`），检查粒度是 `MissionPlan.Period`（缺省 100ms，取档）；`PlanMission` 只在当前步有时限时要心跳。
- `Retry` 是失败（`Failed` / `Expired`）后原地重试的次数，每次重试重新计时，用完才走 `OnFail`；**被取消的不重试**（被别的动作取代、被业务结束），直接走 `OnFail`。次数按步计，进入下一步清零。
- 更复杂的分支（按条件、按结果原因）用行为树，计划里不加表达式。

**AI 心跳与定时冻结**（`gameplay/ai`，第 4 期）：`ai.NewBeatController(hooks, ai.BeatConfig{Host: m.host})` 让 `Controller` 用一个独立的心跳登记 `"ai"` 决策（不并进动作组的心跳：决策周期与动作推进周期不同，维护者决定 7）。Remote 共享模式下被框架停掉的 `"ai"`，回到独占后由业务重新设置当前策略恢复：`c.SetStrategy(c.Strategy())` 不是替换，只重新对齐心跳。

- 周期由当前策略决定：策略可选实现 `TickInterval() time.Duration`（与 `actionflow.TickIntervaler` 同一个方法），不实现按 `BeatConfig.Period`（缺省 500ms），返回 0 表示不需要周期决策；每拍之后按策略重新取档，所以策略可以随自身状态改周期（战斗中 200ms、空闲 1s）。
- 没有策略、或 `Freeze()`（无定时）时停心跳；`FreezeFor(d, interrupt)` 冻结 `d`，期间心跳照常跳但不决策，到期后的第一拍自动恢复并决策；`interrupt` 为真时同时经 `EndActions` 钩子结束在途动作。`Freeze` / `Recover` 清掉定时。
- 动作 / 任务结束在冻结期间照常送达策略（原有规则）。

**AI 事件、分阶段与轨迹**（`gameplay/ai`，第 5 期）：

- **事件**：`controller.OnEvent(ai.Event{Kind: EvAttacked, Data: attacker})` 把一个游戏事件交给当前策略（实现 `ai.EventStrategy` 的），返回是否送达；冻结、切换中、没有策略时丢弃（动作 / 任务结束不受影响）。事件词表由业务定义（`Kind uint16`），框架不定义具体事件。`BehaviorStrategy` 把事件缓冲到下一拍（`MaxEvents` 缺省 64，超出丢最早的），树里用 `EventReceived{Kind, Take}` 叶子（JSON `{"node": "event", "kind": 1}`，`"take": true` 取走）或 `BehaviorContext.HasEvent` / `TakeEvent` / `Events` 读；本拍没被取走的在拍结束时丢弃。状态变化要让 AI 知道，就在 `state` 的观察者里 `OnEvent`。
- **分阶段**（boss 多阶段）：`ai.NewStagedStrategy(map[string]Node{...}, "calm", options)`——若干命名的树，同一时刻只跑当前阶段。树里的 `SetStage` 叶子（JSON `{"node": "set_stage", "stage": "enraged"}`）或 `BehaviorContext.SetStage` 在本拍求值结束后切换；策略外调用 `strategy.SetStage(name)` 立即切换。切换时 Reset 旧树（运行中的 `TaskflowAction` 经 `OnInterrupt` 收尾），`OnStage(from, to)` 报告。JSON 用 `ai.ParseStages`：`{"schema": "roost.ai/v1", "start": "calm", "stages": {"calm": …, "enraged": …}}`，起始阶段与每个 `set_stage` 的目标都要存在；`ParseTree` 不接受 `set_stage`。`NewBehaviorStrategy(root, …)` 是只有一个阶段的特例。
- **决策轨迹**：`BehaviorStrategyOptions.Trace func(path string, status Status)`；`Registry.TraceNodes()` 之后解析的树每个节点都带轨迹（路径是 JSON 路径，例如 `$.stages["calm"].children[1]`），Go 写的树用 `ai.Traced{Path, Child}` 包。框架不在内存里留轨迹（ssr AIEditor 的日志无上限地追加），由业务按实体采样、写日志或调试命令。

**动作指标**（可选，第 5 期）：`BehaviorConfig.Metrics: true` 时写 `actionflow.actions.started_total{kind}`、`ended_total{kind,status}` 与 `running`（[OBSERVABILITY](../OBSERVABILITY.md#动作gameplayactionflow可选)），缺省关闭。

**动作的 Start 可以“启动即完成”**（瞬时动作：传送、改朝向、已经在终点的移动）。`Action.Start(ctx) (done bool, result ActionResult, err error)`：`err` 非空是没能启动（以失败结束）；`done` 为真时 runner 不再 tick 它，立即以 `result` 结束（状态为空算成功），照常发 `OnEnded`、通知任务、推进本组队列，不挂心跳。`movement` 的 `MoveTo` / `Follow` 在 Start 时按 0 时长核对一次：已经在终点、目标已在 `Keep` 以内且 `StopWhenReached`、目标已消失或不在本场景，都在发起它的 handler 里结束。

**同组取代：`Replacer`**（对照 ssr 的 `CanCancelByNext`）。当前动作可选实现 `ReplaceBy(ctx, next ActionSnapshot) ReplaceDecision`：

| 答复 | 结果 |
| --- | --- |
| `ReplaceAllow`（不实现即此） | 新动作取代当前动作（当前动作以取消结束） |
| `ReplaceKeep` | 保留当前动作；新动作拿到 ID、不启动，以取消结束一次（原因 `kept current action`），`Start` 返回 nil。用于“同一个请求不必重来”，例如同目的地的移动 |
| `ReplaceReject` | 保留当前动作；`Start` 返回 `ErrReplaceRejected`（回调里延后的以失败结束）。用于“手动操作不被 AI 打断” |

强制结束（`End` / `EndAll` / `EndAction` / 冻结打断 / 跨组抢占）不问它。

**跨组冲突：`ActionRunnerConfig.Conflict`（`BehaviorConfig.Conflict`）**（对照 ssr 的硬编码矩阵，改为业务给的函数）。动作要启动（`Start`，或排队项成为当前动作）时，对其他每个有当前动作的组（组号升序）调用 `Conflict(running, incoming)`：任一组 `BlockIncoming` 即拒绝新动作（`ErrBlockedByGroup`，什么都不改）；否则先取消全部 `PreemptRunning` 的组的当前动作（取消状态），再按同组规则启动。缺省 nil：组之间互不影响。被抢占组的**排队项保留**：抢占的那一刻不推进（否则排队项会先启动、再被正在启动的抢占方挡住），**抢占方结束时**（完成、被结束、被取代、启动失败）推进一次，排队项照常过冻结与跨组规则成为当前动作，按 ID 等它的 `TaskflowAction` / `PlanMission` 不会挂住；这期间组被冻结的由 `Recover` 推进，业务已在这组启动了别的动作的不动。与 `FreezeCancel` 相同的口径：打断只停下正在做的事，排队的计划去留由业务决定（`ClearQueue` / `EndAction`）。

**按 ID 结束：`EndAction(id, force, reason)`**：结束某组的当前动作（并推进该组队列），或把排队中的动作移出队列（取消状态）；找不到返回 `ErrActionNotFound`（回调里延后执行时找不到不报告——动作可能已在这之前结束）。

**冻结是计数的，分两种模式**：`Freeze(group, mode)` / `Recover(group, now)`，多个来源（定身、缴械、剧情）可同时冻结同一组，每次 `Freeze` 对应一次 `Recover`，计数归零才解冻；多 `Recover` 一次返回 `ErrActionGroupNotFrozen`。冻结期间 `Start` 被拒（`ErrActionGroupFrozen`）、当前动作不被 tick、队列不推进；`BeatRunner` 停掉这个组的心跳，解冻后重新挂上（解冻后第一拍的 `Elapsed` 从重新启动算起，暂停的时间不计入）。

| 模式 | 当前动作 |
| --- | --- |
| `FreezePause` | 留在组里、暂停，解冻后接着跑 |
| `FreezeCancel` | 取消（取消状态、原因 `action group frozen`，`Err` 是 `ErrActionGroupFrozen`）；排队项保留，解冻后推进 |

计数立即生效（回调里也是），`FreezeCancel` 的打断有回调、在回调里调用时延后。

**状态位：`gameplay/state`**（对照 ssr 的 state 模块；第 2 期）。一组各自独立的“是否生效”事实（定身、缴械、无敌、手动操作中、保护期……），不是状态机。启动时声明一次：

```go
catalog, _ := state.NewCatalog(
	state.Def{ID: Rooted, Name: "rooted", Sync: true},       // 客户端可见
	state.Def{ID: Protected, Name: "protected", Persist: true}, // 落库的长期标记
	state.Def{ID: Manual, Name: "manual"},                   // 缺省：不同步、不持久
)
// 实体工厂里：
m.stateDao = state.NewStateDao(id, "game", catalog) // 有 Persist 状态时登记进实体的 DaoManager
m.states = state.NewSet(m.stateDao, state.SetConfig{OnError: logError})
m.gate, _ = state.Gate(m.states, m.behavior, logError,
	state.Rule{State: Rooted, Group: GroupMove, Mode: actionflow.FreezeCancel}, // 定身打断移动
	state.Rule{State: Disarmed, Group: GroupFight, Mode: actionflow.FreezePause},
)
```

- **两种写入**：按来源 `Add(id, Source{Kind, ID})` / `Remove` / `RemoveSource`——有任一来源即生效，同一来源重复加不叠加、撤掉不存在的来源无效（`Source.Kind` 由业务定义：Buff、动作、GM……）；派生 `Provide(id, func() bool)`——查询时现算、不存储，拥有数据的组件在数据变化后调 `Changed(id)`。没有一次性事件（生命周期事件走实体回调或 AI 事件），没有通用参数槽。
- **查询**：`Has`、`Any`、`Satisfies(Requirement{All, None})`（“能否做某个操作”）、`Sources`、`Active`、`Synced`（同步打包读它）。
- **随事务回滚**：按来源的状态存在 `StateDao`，handler 回滚时一起撤销（两种回滚策略都支持；`rollback=state` 靠 Nest 对实体 DaoManager 里的 DAO 拍快照，所以要求 `StateDao` 登记在那里——没登记时 `rollback=state` 的 handler 里改状态返回 `state.ErrRollbackNotCaptured`、什么都不改；`rollback=undo` 不依赖登记）。声明了 `Sync` 的变化标同步脏（`FieldStates`，业务的同步打包读 `Set.Synced()`），声明了 `Persist` 的进持久文档（键是状态名；改名等于丢弃旧数据），缺省都不。持久状态只能在 Nest 事务里改（事务外返回 `ErrNotInTransaction`）；不持久的可以在加载 / 新建的初始化阶段直接改。
- **观察者**：`Watch(id, fn)` / `Unwatch`，`NewWatcher()` 打包一次撤销。按“观察者上次看到的值”算差通知（总是交替的 true / false），顺序 = 注册顺序；观察者里改状态立即生效、引出的通知排到本轮之后。**回滚不通知**：观察者的副作用保留，下一次任何状态改动或 `Refresh()` 时按差补发。能用门控表达的不要写观察者。
- **门控 `Gate`**：规则 `{State, Group, Mode}`，状态生效时对动作组计数冻结（`FreezeCancel` 在生效那一刻取消在途动作），失效时撤销；装上时按当前状态对齐，`Close` 撤销它持有的全部冻结。冻结计数不随事务回滚，所以 handler 回滚撤掉了一个门控状态时，组保持冻结直到下一次通知（每条规则至多持有一次冻结，不会累积）；需要立即一致就在回滚之后调 `Refresh`。
- **不内建时长**：有时长的控制效果走 Buff（到期移除 → 状态跟着变），离线也要准点的长期标记（保护期）走 entitytimer（[§4](#4-按实体的持久定时器)）到期时 `Remove`，短时标记由设置它的动作或 Buff 负责撤销。
- **Buff → 状态（第 3 期）**：控制效果的唯一持有者是 Buff（时长、叠层、驱散、免疫都在 `combat.BuffContainer` 里），状态不复制它，而是派生：`combatComponent.BindStates(set, combatcomponent.StateBinding{State: Stunned, Tags: []combat.Tag{"stun", "freeze"}})`——任一生效中的 buff 带其中一个标签，状态即生效。组件在每个改 buff 集合的方法（`ApplyBuff`、`RemoveBuff`、`SetBuffStacks`、`AdoptBuff`、`DispelBuffs`、`TickBuffs`）末尾与 `OnInitFinish` 里调 `Changed`，门控与 AI 跟着变；buff 随事务回滚、随实体加载，`Has` 当场就对。Buff 到期由 buff 心跳推进（`CombatComponent.AttachBeat`，§4E），派生状态随之失效。技能的免疫标签判断 `StatusBridge.HasGameplayTag` 可以接到 `Set.Has` 或 `CombatComponent.HasBuffTag`。
- **和动作的关系**：动作可以在 Start 里 `Add(id, Source{Kind: 动作种类, ID: 动作 ID})`、在结束（Tick 报告结束 / Cancel）时 `Remove`（ssr 移动动作置 / 清 Manual 的做法）。

**生成工程的入口**：`roost add behavior --entity <Entity> [--ai] [--state]` 生成 `BehaviorComponent`：在 `OnInitFinish` 用实体的心跳宿主构造 `Behavior`（`--ai` 时先构造 `ai.NewBeatController` 作 `Observer`，`Context` 钩子给出 `ActionList`）。业务文件只建一次：`behavior_actions.go`（动作种类、组、注册表、跨组冲突、`timeDomain()`、`onBehaviorReady`、示例动作 `ExampleAction`）、`--ai` 的 `behavior_strategy.go`（示例策略）、`--state` 的 `states.go`（`state.NewCatalog` 声明；Set / Gate 接线由业务写）与 `game/handler/<entity>_behavior.go`（发起示例动作 / 设定示例策略的 Nest handler）。没有心跳宿主时一并加上（同 §4A）。AI 不设策略不跳，示例策略要在 `onBehaviorReady` 或 handler 里 `SetStrategy`。

边界见 [KNOWN-LIMITS“行为控制”](../../maintenance/KNOWN-LIMITS.md#行为控制d21)，实现契约见 [impl/10-time §2D](../impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态)。

## 4D. 场景时间域（变速）

副本 2 倍速、出牌 / 瞄准慢放、技能时停这类“一个场景里的时间流速”用 `gameplay/timedomain`（DESIGN-TODO D22，维护者 2026-10-21 决定：**全局不做变速，只做场景级**；照 ssr 的场景时间）。部署级的业务时钟仍然只有偏移（`time.logic_offset`），没有倍率。

**模型**：域时间是业务时间的分段线性映射 `D(B) = AnchorDomain + (B − AnchorBusiness) × Rate`，`B` 是本条消息冻结的业务时间（§1A）。`SetRate` 以生效时刻为新锚点（新 `AnchorDomain` 是旧映射在生效时刻的值），所以域时间连续、不跳变；倍率在 `[0, 100]`，0 是时停，所以单调不减。域可以在 `Config` 里收窄倍率范围。从未改过倍率时是恒等映射（域时间就是业务时间）。

**场景本身是实体**（D20 决定 ⑬），域挂在场景实体上：

```go
// 场景实体：三个 DAO 字段（标 sync 下发给客户端；随进程结束的副本用 nopersist,sync），适配成 timedomain.Fields。
type sceneTime struct{ dao *db.SceneDao }

func (f sceneTime) TimeDomain() timedomain.State {
	return timedomain.State{AnchorBusinessNano: f.dao.GetTimeAnchorBusiness(), AnchorDomainNano: f.dao.GetTimeAnchorDomain(), Rate: f.dao.GetTimeRate()}
}
func (f sceneTime) SetTimeDomain(s timedomain.State) {
	f.dao.SetTimeAnchorBusiness(s.AnchorBusinessNano)
	f.dao.SetTimeAnchorDomain(s.AnchorDomainNano)
	f.dao.SetTimeRate(s.Rate)
}

// 场景组件 OnInitFinish（加载 / 新建）：构造域（读出存下的参数并发布）。
scene.time, err = timedomain.New(sceneTime{dao}, timedomain.Config{MinRate: 0, MaxRate: 4})

// 场景自己的 Nest handler 里：玩家请求战斗加速。
if err := scene.time.SetRate(2); err != nil { // 越界返回 ErrRateOutOfRange，什么都不改
	return err
}

// 场景里的成员（玩家、怪物）：行为组件与 AI 都挂上场景的域。
behavior, err := actionflow.NewBehavior(member, actionflow.BehaviorConfig{
	Registry: registry, GroupForKind: groups,
	Beat: &actionflow.BeatConfig{Host: host, TimeDomain: scene.time},
})
brain, err := ai.NewBeatController(hooks, ai.BeatConfig{Host: host, TimeDomain: scene.time})
```

| API | 说明 |
| --- | --- |
| `timedomain.New(fields, config)` | 所属实体初始化阶段构造；读出 `Fields` 里的参数并发布，存下的倍率越界返回 `ErrRateOutOfRange` |
| `Domain.SetRate(rate)` | **只在所属实体（场景）的锁内**调用。从本条消息冻结的业务时间起生效；写 `Fields`（随事务回滚、随同步下发），事务提交后（`nest.AfterCommit`）才发布给成员，回滚的不发布；不在事务里立即发布 |
| `Domain.Now()` / `Rate()` / `Read()` | 任意实体、任意 goroutine：本条消息的域时间与倍率，第一次读取时冻结（`fctx.Frozen`），同一条消息里读到同一个值，即使期间场景发布了新倍率 |
| `Domain.State()` | 最近一次发布（已提交）的参数 |
| `heartbeat.WithTimeDomain(d)` | 心跳登记按域计时：调度仍按真实时间（档位不缩放，CPU 不随倍率涨）；`Beat.Now` 是域时间，`Elapsed` 是域时间的经过，时停时为 0；`Elapsed` 上限乘倍率（`max(1s, 档位) × 倍率`），否则 10 倍速时 100ms 档每拍都被夹住。代价：高倍率下每拍步长变大，移动 / 碰撞精度下降 |

- **成员读发布的快照，不读场景的 DAO**：成员在自己的锁里，不跨实体加锁。快照在场景事务提交后才发布，所以生效时刻之后、发布之前开始的成员消息仍按旧倍率算；为了让成员读到的域时间单调，域记下已经交出去的最大域时间，读数取两者较大者，改倍率时新锚点也不低于它。代价是改倍率前后成员与客户端可能差“发布延迟 × 倍率”（微秒到毫秒级），随后一致。
- **actionflow / ai**：`actionflow.BeatConfig.TimeDomain`（`BeatRunner`、`Behavior` 的动作组心跳与任务心跳）与 `ai.BeatConfig.TimeDomain`（AI 心跳）挂上域：动作每拍的 `ActionContext.Now` / `Elapsed` 是域时间，移动（`Move.Advance(ctx.Elapsed)`）随之加速、慢放、时停；心跳之外调用 runner（handler 里 `Start` / `End`）时 `ActionContext.Now` 也是本条消息的域时间（不论调用方传没传时间）；`Behavior` 没配 `Now` 时取域时间，任务上下文与计划步骤 `Timeout` 的起点、任务心跳的到期判断同一把尺子；`ai.Controller` 没配 `Now` 钩子时取域时间，`FreezeFor` 的期限与 AI 心跳的到期判断同一把尺子。只想让某个登记按域计时，也可以直接把 `heartbeat.WithTimeDomain` 放进 `Options`。
- **成员换场景（换时间域）靠重建**：域在构造时给，运行中不换。成员进出副本时，在换场景的 handler 最后一步撤掉旧的、在同一心跳宿主上按新域重建：

  ```go
  m.brain.Close("scene change")                              // AI 卸下策略、撤销 "ai" 心跳，名字释放
  m.behavior.Close(actionflow.NewActionReason("scene change")) // 任务与动作以这个原因结束，组心跳与任务心跳撤销
  m.brain, _ = ai.NewBeatController(hooks, ai.BeatConfig{Host: m.host, TimeDomain: next.time})
  m.behavior, _ = actionflow.NewBehavior(m, actionflow.BehaviorConfig{ /* 同上 */ Observer: m.brain,
  	Beat: &actionflow.BeatConfig{Host: m.host, TimeDomain: next.time}})
  ```

  `Close` 立即撤销心跳登记（`heartbeat.Reg.Close`：离开驱动、释放名字），所以同名登记马上可以重建；旧登记已经在队列里的拍到达时不执行回调。不提供 `SetTimeDomain`：旧域里记下的时刻（上一拍的 `Elapsed` 基线、计划步骤时限的起点、`FreezeFor` 的期限）拿到新域里比较没有意义，而换场景本来就要结束旧场景里的动作（MoveTo 的目标点属于旧场景）。`Close` 与构造一样不随事务回滚（见 §4B 的业务约束）。只用 `BeatRunner` 的组件用 `BeatRunner.Close(reason)`。
- **技能 / Buff**：心跳驱动的 `combatcomponent.RuntimeBeatConfig.TimeDomain` 与 `BeatConfig.TimeDomain`（§4E）挂上域：技能 tick 与 buff 的到期按域时间换算，时停时 tick 不前进、2 倍速时走两倍；handler 里的 `RuntimeBeat.Start` / `CurrentTick` 同样取域时间。不走心跳的驱动没有 `TimeDomain` 字段：域时间与倍率经 `StepperConfig.Now` / `Rate`、`BuffDriveConfig.Now` 与 `StepTo` 的参数由驱动自己传进来。换场景同样靠重建：`combat.CloseBuffDriver()` 后按新域 `AttachBeat`，`runtimeBeat.Close()` 后用同一个 Runtime 按新域 `NewRuntimeBeat`，再 `SyncBuffDriver` / `Sync`。
- **持久化的期限与 entitytimer 不接时间域**（维护者决定 ⑦）：持久定时器的离线补触发只对业务时间成立；域内的定时走心跳（按域时间判定到期）。存进 DAO 的业务期限（邮件过期、活动截止）仍是业务时间。
- **持久与否**：`Fields` 是否持久由业务选。随进程结束的副本用 `nopersist,sync`（推荐）；要持久的场景用持久字段，重载后域时间按存下的锚点与倍率接着走（停服期间同样按倍率前进，时停的保持时停）。`Domain` 实例随场景实例：场景卸载重载后是新的 `Domain`，成员要换成新的。
- **下发给客户端**：`Fields` 的三个字段标 `sync`，随 Entity Sync 下发；客户端按同一公式用自己估计的服务器业务时间外推域时间。

**生成工程的入口**：`roost add timedomain --entity <Scene>` 在场景的主 DAO 定义里加 `TimeAnchorBusiness` / `TimeAnchorDomain` / `TimeRate` 三个 `nopersist,sync` 字段，生成 `TimeDomainComponent`（加载时 `timedomain.New` 并发布，`Domain()`，上面那段 `Fields` 适配），倍率范围写在 `timedomain_config.go`（业务文件，缺省 `[0, 4]`）。成员的行为组件在 `behavior_actions.go` 的 `timeDomain()` 里返回场景的域（生成的是 `nil`：成员怎样找到所在场景的域由业务决定）。

实现契约见 [impl/10-time §2F](../impl/10-time.md#2f-场景时间域)。

## 4E. 技能与 Buff：推进与驱动（D20 第 5 期）

技能推进与 buff 到期的**规则**与**驱动方式**分开（维护者 2026-10-21：心跳只是驱动方式之一，skill 只用接口）。规则在与驱动无关的推进器里——技能 Runtime 是 `skill.Stepper`，buff 是 `CombatComponent.DriveBuffs`；驱动方式经 `skill.Driver{Wake() error; Sleep()}` 接进来：推进器在有工作时叫 `Wake`、空闲时叫 `Sleep`，驱动决定什么时候、按什么时刻推进一步。

- **快池上的实体：心跳驱动**（`gameplay/skill/combatcomponent`）：`NewRuntimeBeat` / `AttachBeat` 把 `Wake` / `Sleep` 换成实体心跳登记的启停，每拍推进一步。业务不写周期消息调 `Runtime.Advance` / `TickBuffs`。
- **房间自己的定步长循环**（Lockstep 房间、塔防 / 自走棋那种 `Advance(now)` 循环）：直接用 `skill.NewStepper` / `DriveBuffs`，每帧调 `StepToTick` / `StepBuffsToTick`；驱动可以只记“谁醒着”，循环只推进醒着的（示例 `skill.ExampleStepper_fixedStepLoop`）。
- **测试**：Driver 为 nil 或自己写一个记录通知的驱动，手动调 `StepTo` / `StepBuffs`。

两者共用一把尺子 `skill.TickClock`：

```go
var SkillClock = skill.TickClock{Length: 50 * time.Millisecond} // Origin 缺省 = Unix 纪元：tick 是绝对的，可以存进 DAO

// 心跳驱动的战斗单位：限时 buff / 业务周期工作在时挂 1Hz 心跳，清空后停。
err := combat.AttachBeat(combatcomponent.BeatConfig{Host: host, Clock: SkillClock, TimeDomain: scene.time,
	Periodic: regen.Active, OnBeat: regen.Beat}) // Periodic / OnBeat 可选：回复、周期伤害、冷却这类业务自己的周期工作
id, outcome := combat.ApplyBuff(spec, combat.CurrentTick(), source)         // handler 里：tick 取本条消息的 tick
bridge := &combatcomponent.StatusBridge{CurrentTick: func() skill.Tick { return skill.Tick(combat.CurrentTick()) }, ...}

// 心跳驱动的技能 Runtime（场景 / 房间实体上）：有在途施法或衍生物时挂心跳，空闲时停。
beat, err := combatcomponent.NewRuntimeBeat(runtime, combatcomponent.RuntimeBeatConfig{Host: host, Clock: SkillClock, TimeDomain: scene.time})
castID, err := beat.Start(program, skill.CastInput{Caster: caster, Target: target}) // handler 的最后一步
err = beat.Do(func(rt *skill.Runtime) error { return rt.Release(castID) })      // 其他改 Runtime 的输入

// 不走心跳：房间的定步长循环。member 实现 skill.Driver（Wake / Sleep 记进房间的“醒着”集合）。
stepper, err := skill.NewStepper(runtime, skill.StepperConfig{Clock: SkillClock, Now: room.FrameTime, Driver: member})
err = combat.DriveBuffs(combatcomponent.BuffDriveConfig{Clock: SkillClock, Now: room.FrameTime, Driver: buffMember})
// 每帧（buff 的一步要在事务里）：醒着的 stepper.StepToTick(frame, 1)、combat.StepBuffsToTick(frame)
```

| API | 说明 |
| --- | --- |
| `skill.TickClock{Length, Origin}` | `tick = floor((t − Origin) / Length)`，向下取整；`Length` 在 [1ms, 1s]；`Origin` 零值是 Unix 纪元。存进 DAO 的 tick（buff 的 `DueTick`、Runtime checkpoint）不能换 `Length` / `Origin`。`Ticks(d)` 把时长换成 tick 数（向上取整） |
| `skill.Driver{Wake() error; Sleep()}` | 驱动方式。通知是**电平式**的：每次推进与输入之后按有没有工作调一次，同一状态可能连续调多次，实现必须幂等、不阻塞。`Wake` 返回错误表示挂不上，工作留着，之后 `Sync` 重试 |
| `skill.NewStepper(runtime, StepperConfig{Clock, Now, Rate, Driver, MaxCatchUp, Name})` | 与驱动无关的 Runtime 推进器。`Now` / `Rate` 是输入侧的“现在”与倍率（心跳适配层给的是本条消息冻结的 `fctx.Now` 或域时间）；`MaxCatchUp` 缺省 1s |
| `Stepper.StepTo(now, rate)` / `StepToTick(tick, rate)` | 驱动的一步：忙时逐 tick 推进到目标，空闲时不推进，然后通知驱动 |
| `Stepper.Start` / `Input` / `Do` / `CatchUp` / `Sync` / `Tick` / `Stats` | 输入：先推进到输入的 tick，再执行，最后通知驱动；读冷却、弹药前用 `CatchUp`；驱动状态被外部改掉后调 `Sync` |
| `CombatComponent.DriveBuffs(BuffDriveConfig{Clock, Now, Driver, Periodic, OnStep, OnError, Name})` | 与驱动无关的 buff 推进，构造时装一次。`BuffsPending`（有限时 buff 或 `Periodic()` 为真）/ `NextBuffDue` 回答有没有工作、下一个到期 tick；`StepBuffs(now)` / `StepBuffsToTick(tick)` 是一步（`TickBuffs`、`OnStep`、通知驱动，在事务里调）；`SyncBuffDriver` 重新通知 |
| `CombatComponent.AttachBeat(BeatConfig)` | 心跳驱动的 `DriveBuffs`：登记一个停止状态的心跳 `"combat"`，有工作时启动，周期取不大于 `Granularity`（缺省 1s）的最长档；每拍一步再调 `OnBeat`（`BuffBeat` = 心跳的拍 + `BuffStep{Tick, Expired}`）；全部清空后停 |
| `CombatComponent.CurrentTick()` / `SyncBuffDriver()` | 输入侧的 tick（心跳驱动下是 fctx.Now 或域时间经 Clock 换算）；`Periodic` 的答案变了、Remote 回到独占之后调 `SyncBuffDriver` |
| `CombatComponent.CloseBuffDriver()` / `RuntimeBeat.Close()` | 撤销：心跳驱动撤销登记（`heartbeat.Reg.Close`：立即离开驱动、释放名字），其他驱动收到一次 `Sleep`。`CombatComponent.OnDestroy` 调 `CloseBuffDriver`（组件由别的组件持有时由持有者转调）；持有 `RuntimeBeat` 的组件在自己的 `OnDestroy` 里调 `Close`。换场景（换时间域）时撤掉旧的、在同一宿主上按同名重建，再 `SyncBuffDriver` / `Sync` |
| `NewRuntimeBeat(runtime, RuntimeBeatConfig)` | 心跳驱动的 `Stepper`（`RuntimeBeat` 嵌入 `*skill.Stepper`，上面的方法都能直接调）：登记一个停止状态的心跳 `"skill"`，周期缺省取不大于 tick 长度的最长档（50ms tick → 50ms）。`Runtime.Pending()`（在途施法、排程任务、衍生物、待重试的停止）时启动，空闲时停 |

- **事务**：心跳驱动下 buff 心跳的启停随事务（施加限时 buff 的 handler 回滚，心跳不启动）；拍在实体锁与事务内，过期、属性投影、派生状态通知随这一拍提交或回滚。不随事务的驱动（定步长循环）在回滚后多醒一步，下一步没有工作就自己睡。技能 Runtime 不在回滚域里（[08-skill §4](08-skill.md#4-事务与战斗边界)）：`Start` / `Input` 放在 handler 的最后一步；之后回滚了，Runtime 里的施法还在、心跳没挂上，调 `RuntimeBeat.Sync` 补上（或等下一次输入：通知是电平式的）。拍里推进的错误交给 `OnError`、不让本拍回滚。
- **加载**：`OnInitFinish` 按加载出的 buff 通知驱动（心跳驱动下启动心跳），停服期间已经到期的 buff 在驱动的**第一步**清掉（`OnInitFinish` 不在事务里，不能在那里改 DAO）；第一步之前读到的 `ActiveBuffs` 仍含这些记录。恢复出的 Runtime 还有工作时，构造推进器（`NewStepper` / `NewRuntimeBeat`）后调一次 `Sync`。
- **精度**：心跳驱动下 buff 最多晚一个心跳周期（缺省 1 秒）加 tick 取整过期；短控制多的游戏调小 `Granularity`。定步长循环每帧一步时精度是一帧。需要离线也准点、结果要落档的长时 buff 用 entitytimer（§4）。
- **确定性**：Runtime 忙时逐 tick 推进（Host 每个 tick 恰好推进一次），一步覆盖几个 tick 不改变结果；空闲点由 Runtime 决定，之后的输入一次跳到输入的 tick。所以同一输入序列（落在同样的 tick 上）在任何驱动节奏下、换一种驱动方式也逐 tick 相同。落后超过追帧上限（`MaxCatchUp`，心跳驱动缺省 `max(1s, 周期)`，乘 max(倍率, 1)）时剩下的一次跳过，`Stats().Jumps` 计数并打限频 Warn，这段不满足逐 tick 确定性。规则与回放的关系见 [impl/10-time §2G](../impl/10-time.md#2g-技能与-buff-的推进与驱动)。
- **Remote 共享模式**：与所有心跳相同，不允许挂载（`ErrHeartbeatRemoteShared` 经 `OnError` / 输入的返回值报告）；回到独占后调 `SyncBuffDriver` / `Sync`。
- **原语**：`TickBuffs(now)` 与 `Runtime.Advance(tick)` 仍是推进一步的原语；驱动一律经推进器（`StepBuffs*` / `Stepper`），不直接调原语，否则空闲点、追帧上限与驱动通知都不成立。

## 5. 运维

区分业务时间推进失败与机器系统时钟漂移；日志与故障记录使用系统时间。glsvet 的时钟检查提供静态提示，不能识别任意封装后的错误时钟。开发测试同时注入两个时钟，验证推进业务钟不会导致基础设施超时。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
