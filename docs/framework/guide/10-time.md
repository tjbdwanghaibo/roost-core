# 10 时间（说明）

> 配套实现文档：[impl/10-time.md](../impl/10-time.md)（文件地图、主流程、不变量与守卫测试、并发、review 检查点、源码疑点）。
> 源码基准：tag `v1.23.0`（`28912cd6`），文中 `path:line` 都按这个 tag。codebase-memory 图谱的代际是 2026-09-30：`app/business_time.go`、`app/business_clock.go`、`app/config_schema.go`、`cmd/glsvet/clockhints.go` 不在图谱里（`not_tracked`），`clock/clock.go`、`timer/scheduler.go` 是 `metadata_changed`。本篇的结论全部按 tag 源码直接读取，图谱只用来定位。

## 速览

- **这一块是什么**：时间分成两个钟。**业务时钟** = 真实时间 + `time.logic_offset`，活动窗口、World 定时器、冷却、邮件过期、赛季这类玩法时间读它；**系统时钟** = 真实时间，就是 `time` 包，帧率、租约与锁、超时、重试、存储 TTL、日志、WAL 时间戳读它（维护者决定 D-L3）。另有一个实体定时器调度器 `timer.Scheduler`，World 的定时器建在它上面。
- **最重要的保证**：偏移只有 `time.logic_offset` 一个来源，只在启动时读一次，运行期改不了；生产环境（`env` 为 `prod` / `production`）偏移必须为 0，否则拒绝启动。同一套部署的业务时间**只许前进**：非生产环境里，App 在第一个 Mod `Init` 之前对照 Redis 里的高水位检查，业务时间比高水位早 1 分钟以上就拒绝启动。定时器的触发顺序是全序：期限 → priority（小的先）→ 节点 ID（登记顺序）。
- **最容易踩的坑**：① 业务代码里直接写 `time.Now()` 读的是系统时钟。偏移不为 0 的测试环境里，它和活动窗口、协调器差一个偏移。`glsvet` 只对 `game` 目录给提示，不判失败。② 手工装配的 kit 服务如果没设 `Config.Now`，缺省是 `time.Now`（系统时钟），不是业务时钟，只有经 kit Mod 装配才会注入业务时钟（[impl §11 T2](../impl/10-time.md#11-源码疑点与文档不一致)）。③ 宿主用自己的时间驱动 `Scheduler.Tick` 时，必须用 `SetClock` 注入同一个时间源，否则新定时器的期限会差出一截。④ 下线一种定时器类型时，存量节点到期**会被删除**，只留下 Warn 日志和 `timer_unhandled_dropped_total{kind}` 计数。⑤ 测试环境要让业务时间“回到过去”，只能清库重建。只删高水位键、保留数据，等于绕过守卫。

### 本篇覆盖的包

| 包 / 文件 | 职责 |
| --- | --- |
| `clock` | 业务时钟接口 `Business`、固定偏移实现 `NewBusiness`、进程级业务时钟 `Process` / `Now` / `UnixMilli` / `SetOffset` |
| `app/business_clock.go` | Registry 能力 `clock.business`（`ModBusinessClock`）与取钟函数 `app.BusinessClock(r)` |
| `app/business_time.go` | 业务时间高水位守卫：启动检查、运行中推进、推进失败计数 |
| `app/config_schema.go`（`time.*` 部分） | `time.logic_offset` 的声明与“生产必须为 0”的校验 |
| `timer` | 实体定时器调度器：最小堆、排序规则、typed / 闭包定时器、持久化钩子、未注册类型的处理 |
| `cmd/glsvet/clockhints.go` | 业务包直接读系统时钟时的提示（`-clockhints`、`-businessdirs`、`//glsvet:system-clock`） |
| `codegen/internal/roost/logic_offset_doctor.go` | `roost project doctor` 的 `time:logic_offset` 一行：检查同一套部署里各服务的偏移是否一致 |
| 生成工程模板 `demo/game/entities/world/timer_component.go.tmpl` | World 的 `TimerComponent`：把 `timer.Scheduler` 放进 DAO 和事务里使用的参考实现 |

跨分区说明：
- 业务时间守卫在启动序列里的位置，以及它和单实例锁共用的存储，见 [01 app 说明](01-app-lifecycle.md)。
- Nest 请求上下文 `fctx` 怎样钉住一次请求的时间，见 [02 nest 说明](02-nest-entity.md)。
- 定时器节点写进 DAO 以后随事务统一回滚（A1），见 [03 dataengine 说明](03-dataengine.md)。
- 每个 kit 服务各自的时间字段，见 [09 kit 服务](09-kit-services.md)。
- 指标导出名与仪表盘，见 [11 可观测](11-observability.md)。
- doctor 和生成工程模板的整体，见 [12 代码生成](12-codegen.md)。

---

## 1. 定位与边界

**负责**

- 定义两个钟的边界，以及“业务时间从哪里拿”。业务时钟有三个来源：`app.BusinessClock(registry)`、kit Mod 注入的 `Config.Now`、请求上下文的 `fctx.Now()`。
- `time.logic_offset` 的读取。它只在启动时读一次，生产环境必须为 0。
- 部署级业务时间单调。它只在非生产环境检查，靠协调存储（共享 Redis）里的一个高水位键。
- 实体定时器的调度规则：排序、改期与取消在 Tick 中途的语义、持久化钩子、没有 handler 的类型。
- 静态提示（glsvet）与配置一致性检查（doctor）。

**不负责**

- 帧率与 tick 驱动：nest 的 ticker 是系统时钟（`nest/ticker.go:155`），见 [02](02-nest-entity.md)。
- 各 kit 服务字段级的时间语义，例如 mail 的过期和领取租约、chat 的保留期，见 [09 kit 服务](09-kit-services.md)。本篇只给归属总表（§4.1）。
- 日 / 周重置和赛季切换：框架与 game-demo 都没有实现（[D-L3 方案 §3.1](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md) 末段）。业务实现时读业务时钟。
- skill / 战斗的“游戏时间”：运行时按帧推进，不读墙钟（同上）。见 [08 skill](08-skill.md)。
- 跨主机的时钟同步：生产靠 NTP。框架只用 1 分钟容差吸收主机之间的偏差。

→ [实现文档](../impl/10-time.md)对应：§1 包与文件地图。

## 2. 核心概念与术语

| 术语 | 含义 | 位置 |
| --- | --- | --- |
| 业务时钟 | 真实时间 + `time.logic_offset`。接口 `clock.Business{ Now() time.Time }` | `clock/clock.go:26` |
| 系统时钟 | 真实时间，就是 `time` 包，没有对应的类型 | `clock/clock.go:7`（包注释） |
| `time.logic_offset` | 业务时钟偏移，时长类型，缺省 0；生产必须为 0 | `app/config_schema.go:225`、`:242` |
| Registry 业务时钟 | `NewRegistry` 按 `time.logic_offset` 登记 `clock.NewBusiness(offset)`，能力名 `clock.business` | `app/registry.go:43`、`app/business_clock.go:9` |
| `app.BusinessClock(r)` | 取 Registry 里的业务时钟；`r` 为 nil 或没有登记时，退回进程级业务时钟 | `app/business_clock.go:21` |
| 进程级业务时钟 | `clock.Now()` / `clock.UnixMilli()` / `clock.Process()`。偏移由 App 启动时 `clock.SetOffset` 设一次；`fctx`、`timer`、`ai`、`actionflow` 在没有注入时用它做缺省 | `clock/clock.go:57`、`:81`；`app/app.go:188` |
| 高水位 | 这套部署到过的最大业务时间，存在 `<singleton.key_prefix>:business_time`，不过期 | `app/business_time.go:47`、`:54` |
| 容差 | 启动检查放过的回退量，1 分钟，不可配置 | `app/business_time.go:37` |
| typed 定时器 | 有类型号的节点。类型号在重启后用来找回 handler；它会持久化（经 `ChangeFunc`） | `timer/scheduler.go:40`、`:63` |
| 闭包定时器 | `NewClosureTimer`，纯内存，不持久化，不进快照 | `timer/scheduler.go:155` |
| priority | 期限相同时的触发顺序，数值小的先触发，缺省 0（D-L1） | `timer/scheduler.go:43-45` |
| 未注册类型 | 节点到期时它的类型没有 handler：节点照样删除，同时记 Warn 和计数（D-L2） | `timer/scheduler.go:300-305` |

→ [实现文档](../impl/10-time.md)对应：§2 关键类型与数据结构。

## 3. 设计原因

| 取舍 | 结论 | 出处 |
| --- | --- | --- |
| 一个钟还是两个钟 | 两个钟，边界写死。测试环境要能把活动、邮件过期“快进”，但租约、锁、超时、TTL 一旦跟着偏移走，就会提前失效或永不失效 | 维护者第六轮决定 D-L3，[方案 §1](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md) |
| 偏移能否在运行期改 | 不能，只在启动时生效。运行期前拨会让所有进程不同步（game 与协调器的窗口 id 错开），还要给成批到期的窗口、邮件、定时器做分批限流 | [方案 §5](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md) |
| 生产是否允许偏移 | 不允许，非 0 拒绝启动 | 方案 §1 约束；`app/config_schema.go:242` |
| 偏移能否往回调 | 不能让业务时间回到这套部署已经到过的时刻。活动、邮件、副本截止都假定业务时间单调。早先为“往回调”拆到系统钟的逻辑（activity 派发退避、mail 领取租约、可配置的信封宽限）全部删掉，由 App 统一兜底 | 维护者 2026-10-06 指示，[业务时间只许前进 §1、§4](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md) |
| 高水位存在哪 | 单实例锁已有的 `app.SingletonStore`（共享 Redis）。App 在任何 Mod 之前已经有它，core 不需要多加驱动依赖。不放 global 服务，因为那样会引入“先起 global”的启动顺序依赖；不放 Mongo，因为 App 层没有 Mongo 连接 | [单调方案 §2](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md) |
| 生产是否检查高水位 | 不检查。生产偏移为 0，靠 NTP，生产行为一字不变：不多一次 Redis 往返，也不会多出一种因主机偏差而拒绝启动的情形 | 单调方案 §3 |
| 运行中推进失败 | 不 fail-stop，只记 Warn 并计数。高水位只守“下一次启动”，运行中的业务不依赖它 | 单调方案 §3；计数是第十二轮加的（[R12 kit 批 §8](../../feature/DECISIONS-R12-KIT-2026-10-06.md)） |
| 容差为什么固定 1 分钟 | 要吸收主机之间的时钟偏差和 10s 的推进间隔。调大等于放宽约束，调小会让正常的主机偏差拒绝启动 | `app/business_time.go:33-37` |
| 同期限定时器的顺序 | 依次比较期限、priority、ID，构成全序，与堆的形状和存储的遍历顺序无关。之前只比期限，World 每次从 map 重建堆，顺序每次都可能不同 | D-L1，[方案 §1、§2](../../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md) |
| 没有 handler 的定时器类型 | 照样删除，但要让人看得见。保留节点会让它挡在堆顶，挡住之后所有节点 | D-L2，同上 |
| 业务包的 `time.Now()` | glsvet 只提示，不判失败。用途要靠人判断，豁免要写理由 | 方案 §1 约束末条 |

→ [实现文档](../impl/10-time.md)对应：§9 历史与重要修复。

## 4. 怎么用

### 4.1 哪些逻辑走哪个钟（归属总表）

判断方法：**这个时间是玩法规则的一部分，或者只和别的业务时间比较** → 业务时钟。**它依赖存储服务端的 TTL，要和另一个读系统钟的进程比较，或者是安全有效期、空间回收、审计、帧率、超时** → 系统时钟（[方案 §1 更正](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)）。

**业务时钟**

| 逻辑 | 怎么拿到业务时钟（tag 源码） |
| --- | --- |
| 活动窗口、协调器的截止 / 宽限 / 开关窗、派发退避 `NextAttemptAtUnix`、进度凭证 `ExpiresAtUnix` | `kit/service/global/activity/activity_mod.go:143` 注入 `app.BusinessClock(r).Now`。退避在 `kit/service/global/activity/service.go:1661` |
| World 定时器（活动关窗）、开窗、结算 | game-demo `demo/internal/service/game/activity.go.tmpl:154`（runner 的 `now`）、`:244`（用业务时间 tick World） |
| 邮件创建 / 过期 / 投递、领取租约 `ClaimDeadlineUnix` | `kit/service/mail/mail_mod.go:94`。租约在 `service/mail/service.go:863` |
| 副本 run 的开始 / 截止 / 结束 | `kit/service/session/session_mod.go:106` |
| 排行榜同分按“谁先达到”排序 | `kit/service/rank/rank_mod.go:94` |
| 匹配票据的创建 / 过期 / 结束、等待放宽 | `kit/service/match/match_mod.go:125`；game-demo `matchmaker.go.tmpl:82` |
| 聊天展示给玩家的时间 `SentAtUnix` | `kit/service/chat/chat_mod.go:169`（`Now`） |
| 账号与角色的创建、登录、登出时间 | `kit/service/account/account_mod.go:151`（`Now`） |
| Nest 请求里的“现在” | `fctx.Now()` / `fctx.NowMilli()`（`fctx/context.go:129`、`:144`、`:151`，读进程级业务时钟） |
| AI、行为流、`timer.Scheduler` 在没有注入时的缺省 | `ai/controller.go:242`、`actionflow/mission_runner.go:436`、`actionflow/action_runner.go:698`、`timer/scheduler.go:131` |
| ops `/readyz` 里的 `server_time_ms` | `kit/ops/ops_mod.go:305`（`clock.UnixMilli()`） |

**系统时钟**

| 逻辑 | 例子（tag 源码） |
| --- | --- |
| 核心三大块：nest 帧率、排队与持锁耗时、WAL 时间戳；dataengine 回执 TTL、outbox 租约与退避；sync 的编码耗时、发送截止 | 一律用 `time` 包，例如 `nest/ticker.go:155`。核心三块没有业务钟（方案 §3.2） |
| 单实例锁的租约与续期 | `app/singleton.go:194-196`（`realSingletonClock`） |
| 业务时间守卫自己的推进间隔 | `app/business_time.go:210`（`time.NewTicker`）。注意：写进键里的值是业务时间 |
| saga 的截止和 claim | `saga/engine.go:229` 等；game-demo `gift_saga.go.tmpl:369`、`start_gift.go.tmpl:48` 带豁免注释 |
| 会话 token 的签发和有效期、服务器行与运维记录时间 | `kit/service/account/service.go:292`（`SystemNow`）；`security/session_token.go:37` |
| 聊天保留期 `StoredAtUnix` 与 `Prune` | `kit/service/chat/store.go:445`、`:680`（`SystemNow`，Mod 注入 `time.Now`） |
| 存储 TTL 兜底 | mail 信封键的 TTL = 业务剩余时长 + 固定 24h（`service/mail/redis_store.go:59` `EnvelopeStorageGrace`）。业务过期由服务判断，不靠 TTL |
| 支付时间、玩家驻留与闲置卸载 | game-demo `purchase.go.tmpl:63`、`playerowner.go.tmpl:124` / `:130`（带豁免注释） |

### 4.2 在 Mod / 业务服务里拿业务时钟

业务代码只用两种拿法：Registry 里的钟，或者注入的函数。不要直接调用 `clock.Now()` 这类全局函数（`clock/clock.go:15-16`）。kit 的写法是在 `Provide` 里把业务时钟注入服务的 `Config.Now`，真实例子是 `kit/service/session/session_mod.go:106`：

```go
Now: app.BusinessClock(r).Now,   // r 是 Provide 拿到的 *app.Registry
```

业务服务内部只调用 `cfg.Now()`。需要同时用两个钟的服务加一个 `SystemNow` 字段，参照 chat 和 account（`kit/service/chat/chat_mod.go:169`）。**注意**：`service/mail`、`service/session`、`service/match`、`kit/service/rank`、`kit/service/account`、`kit/service/global/activity`、`kit/service/chat` 在 `Config.Now` 为 nil 时缺省都是 `time.Now`（例如 `service/session/service.go:149`）。手工装配时要自己传 `app.BusinessClock(registry).Now`，否则偏移不为 0 的测试环境里，这个服务读的是系统时钟。

### 4.3 在 Nest handler 里

handler 读 `fctx.Now()`。`fctx` 在建上下文时把 `clock.Now()` 钉进 `Context.Now`（`fctx/context.go:129`），同一次请求里读到的是同一个值。进程级偏移和 Registry 的钟读的是同一个配置键，详见 impl §3.1。

### 4.4 实体定时器（`timer.Scheduler`）

最小可运行示例是生成工程的 World `TimerComponent`（`demo/game/entities/world/timer_component.go.tmpl`）。它的做法：

1. **每次调用都从 DAO 建一个调度器，用完丢掉**（`:130-150`）。节点就是 DAO 的 `Timers` map，变更钩子 `persist` 把每次增、删、改写回 DAO（`:165-185`）。这样节点和 handler 的改动落在同一个事务里，失败时随 DAO 一起回滚（A1，[03](03-dataengine.md)）。
2. **用调用方的时间钉住时钟**：`scheduler.SetClock(func() time.Time { return now })`（`:148`）。`Tick(now)` 和新建定时器用同一个 now，这个 now 是业务时钟：`activity.go.tmpl:244` 用 `runner.clock().UnixMilli()` 发 `TickWorldTimers`，handler 再把它交给 `TimerComp().Tick`（`demo/game/handler/tick_world_timers.go.tmpl:22`）。
3. **加载后**调用 `ReportUnhandledTypes()`，对存量里没有 handler 的类型每种告警一次（`:115`）。
4. **注册 handler**：`scheduler.RegisterHandler(TimerTypeActivityPhase, component.onActivityPhase)`（`:147`）。handler 返回 0 表示“完成，删掉我”，返回一个正时长表示“这么久之后再触发我”（`:250-260`，失败时 30s 后重试）。

要点：

- **同一时刻的先后**用 `NewTimerWithPriority(delay, type, priority, …)`，数值小的先触发。不指定时就是 `NewTimer`，priority 为 0（`timer/scheduler.go:142`、`:148`）。priority 随节点持久化，改期和重排都会保留。
- `delay <= 0` 会被拒绝，返回 0（`timer/scheduler.go:149`）。要“立即到期”时用一个很小的正时长，World 用的是 1ms（`timer_component.go.tmpl:34`、`:202-209`，RR-20261005-NC-142）。
- 在 handler 里调用 `RemoveTimer` / `ChangeTimer` 是安全的：目标还在堆里时立即出堆，不会在本次 Tick 按旧期限触发；在 Tick 中途新建的定时器推迟到最外层 Tick 结束后才入堆（`timer/scheduler.go:191-257`、`:343-354`）。
- 闭包定时器只活在内存里，重启就丢失。要重启后还在的截止时间，必须用 typed 定时器，并把节点写进存储。
- 不用 `SetClock` 时，新定时器按进程级业务时钟打期限（`timer/scheduler.go:127-132`）。宿主用别的时间驱动 `Tick` 时，必须用 `SetClock` 注入同一个时间源（`:114-119`）。

### 4.5 测试里替换时钟

- 在 cfg 里写 `time.logic_offset`，再调用 `app.NewRegistry(cfg)`（`app/business_clock_promises_test.go:22` 就是这样写的）。Registry 里已经登记了 `clock.business`，再用 `Register` 换掉同名能力会报重复登记。
- 或者直接给服务的 `Config.Now` 注入 `clock.BusinessFunc(fake)` 或一个函数（`clock/clock.go:31`）。
- `clock.SetOffset` / `clock.Set` / `clock.Reset` 改的是进程全局状态。测试改了要在 `t.Cleanup` 里恢复（`app/business_clock_promises_test.go:43-44`）。
- 定时器测试用 `SetClock` 钉住时间，再传显式的 `Tick(now)`（`timer/scheduler_test.go:76`）。

### 4.6 glsvet 时钟提示

`go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...` 会对“业务包”里的 `time.Now` / `time.Since` / `time.Until` 打印 `hint:`。调用和把它当函数值传递都算，`import t "time"` 这种改名导入也能识别。业务包的判定：相对模块根（向上找到的 `go.mod`）的路径里有一段目录名在 `-businessdirs` 里，缺省是 `game`。提示不计入违例，不改变退出码。

确实是系统时间的地方，有三种豁免写法：
- 在同一行写 `//glsvet:system-clock <理由>`；
- 写在上一行；
- 写进函数的文档注释，豁免整个函数。

`-clockhints=false` 关闭提示（`cmd/glsvet/clockhints.go:27-32`）。生成工程里现有 5 处豁免，都是系统用途（§4.1 系统表的最后两行和 saga 行）。

→ [实现文档](../impl/10-time.md)对应：§3 主流程、§5 并发。

## 5. 配置

| 键 | 缺省 | 范围 / 校验 | 说明 |
| --- | --- | --- | --- |
| `time.logic_offset` | 0 | Go 时长（`24h`、`1h30m`）。生产（`env` / `app.env` / `environment` 为 `prod` / `production`）必须为 0，否则拒绝启动（`app/config_schema.go:242-244`）。没有 min，负值不会被拒绝 | 业务时钟偏移。启动时读一次，所有进程必须写同一个值（doctor 检查）。存储精度：进程级时钟按毫秒存（`clock/clock.go:122`） |
| `singleton.key_prefix` | 空 | 开启单实例锁时必填，且不能含空白（`app/singleton.go:151-155`） | 高水位键是 `<key_prefix>:business_time`。偏移不为 0 而单实例锁没开时，也要写这个键（`app/business_time.go:119-123`） |
| `singleton.enabled` | false | 见 [01](01-app-lifecycle.md) | 开启时在非生产环境里一定会检查高水位，偏移为 0 也检查 |

没有可调的项：容差 1 分钟、推进间隔 10s、单次读写超时 3s、CAS 最多 8 次，都是常量（`app/business_time.go:32-52`）。

**doctor**：`roost project doctor` 的 `time:logic_offset` 一行，在 dev（`configs/service/config.<服务>.yaml`）、prod example、k8s secret example 三套配置**各自内部**比较各服务的偏移。不写等于 0s；不一致时 FAIL，并列出 `service=value`；格式错（如 `1d`、不带单位的数字）也 FAIL。三套之间不互相比较（`codegen/internal/roost/logic_offset_doctor.go:24-37`、`:97`）。仓库外的真实生产配置 doctor 看不到。

→ [实现文档](../impl/10-time.md)对应：§3.2 启动检查、§7 持久化 / 协议格式。

## 6. 运行与运维

**指标**

| 指标（内部名 → 导出名） | 含义 | 基线 |
| --- | --- | --- |
| `app.business_time.advance_failed.total` → `app_business_time_advance_failed_total` | 运行中推进高水位失败的次数（`app/business_time.go:51`、`:219`） | 0。一直涨说明协调存储不可用，下一次启动的检查会失败 |
| `timer.unhandled_dropped_total{kind}` → `timer_unhandled_dropped_total` | 到期时因类型没有 handler 而被删除的节点数，`kind` 是类型号（`timer/scheduler.go:31`、`:305`） | 0。宿主事务回滚后重试同一次 Tick 会再计一次 |

**日志**（按出现顺序）

- 启动：`business time: high-water mark checked`，带 `key`、`offset`、`high_water`（`app/business_time.go:167`）。只在守卫运行时出现。
- 运行中：`business time: advancing the high-water mark failed; retrying on the next beat`（Warn，`:220`）。
- 定时器宿主加载：`timer: stored timers have no handler registered for their type; they will be dropped when due`（Warn，每种类型一条，`timer/scheduler.go:185`）。
- 定时器到期：`timer: dropped a due timer with no handler registered for its type`（Warn，每个节点一条，`:303`）。

**拒绝启动的错误**

| 错误 | 条件 | 处理 |
| --- | --- | --- |
| `config: production requires time.logic_offset = 0, got …` | 生产配置里偏移不为 0 | 删掉这个键或写 0 |
| `app: business time would move back: …`（`app.ErrBusinessTimeMovedBack`） | 真实时间 + 新偏移 < 高水位 − 1 分钟 | 用错误信息里给出的最小偏移，或者等真实时间追上；要回到过去只能清库重建（全部业务数据连同高水位键） |
| `… time.logic_offset needs the business time high-water mark`（`app.ErrBusinessTimeGuardMissing`） | 非生产、偏移不为 0，但 bootstrap 没装 `App.Singleton`，或者没写 `singleton.key_prefix` | 装上 `App.Singleton(kitredis.SingletonStore)` 并写 `singleton.key_prefix`（`singleton.enabled` 可以保持 false），或者去掉偏移 |
| `app: business time high-water mark <key>: …` | 读写高水位失败，包括 Redis 不通、值无法解析、CAS 8 次都被抢先（fail-closed） | 恢复 Redis，查看这个键的值 |

**常见故障 → TROUBLESHOOTING**

| 现象 | T 行 |
| --- | --- |
| 生产 / 测试环境因为偏移或高水位拒绝启动；测试环境里活动、邮件整体错开一段固定时间 | [T-270](../../TROUBLESHOOTING.md) |
| `app_business_time_advance_failed_total` 一直涨 | [T-284](../../TROUBLESHOOTING.md) |
| 定时器类型没有 handler，节点被删除 | [T-268](../../TROUBLESHOOTING.md) |

**运维动作**

- 改偏移：所有进程写同一个值，然后**全部重启**，运行期改不了。偏移可以改大；改小时，最多能改小“距上次运行的真实时间 + 1 分钟”。
- 下线一种定时器类型：先让旧版本把存量节点都触发完，再下线；或者确认这些节点可以丢弃。

→ [实现文档](../impl/10-time.md)对应：§6 失败与不确定结果处理、§8 测试与门禁。

## 7. 保证与不保证

**保证**

- 偏移只有 `time.logic_offset` 一个来源。Registry 的业务时钟和进程级业务时钟读同一份配置的同一个键，运行期没有修改入口：`clock.SetOffset` 的非测试调用点只有 `app/app.go:188`。
- 生产环境偏移为 0，业务时钟就是 `time.Now()`。生产不跑高水位守卫，偏移为 0 时 `NewBusiness` 直接返回 `time.Now()`（`clock/clock.go:44-46`）。
- 非生产环境里，开了单实例锁的进程和偏移不为 0 的进程，都会在任何 Mod `Init` 之前确认：本进程的业务时间不早于“高水位 − 1 分钟”。读不到高水位就拒绝启动。
- 定时器的触发顺序是 (End, Priority, ID) 全序。从存储重建时顺序也一样，与 map 的遍历顺序无关。
- 没有 handler 的定时器到期时一定有一条 Warn，并计一次数。

**不保证 / 已知限制**

- 偏移为 0、又没开单实例锁的进程**不检查**高水位。一套部署里如果全是这类进程，从 +24h 改回 0 不会被拒绝（单调方案 §8“未完成 / 风险”）。
- 被放过的回退最多 1 分钟（容差）。进程崩溃时，高水位最多落后一个推进间隔（10s），这一段由容差吸收。
- 运行中推进高水位失败不会 fail-stop。
- 各进程的偏移是否一致，运行期没有检查，只有 doctor 查仓库里的配置文件。
- glsvet 只覆盖 `game` 目录；它不提示 `clock.Now()` 这类全局函数，也不检查 kit 服务手工装配时漏注入 `Config.Now`。
- 定时器的 handler 不能做 I/O，这一点由宿主保证（World 用 EFFECT 走 outbox，见 `timer_component.go.tmpl:49-52`），`timer` 包本身不强制。`Scheduler` 不加锁，由宿主在自己的锁或事务里调用。
- 外部验证项：高水位只在单机 Redis 上测过（`TestBusinessTimeHighWaterMarkOnRealRedis`）；Redis Cluster 切主（E08）没有验证。

**本次核对发现、尚未登记的问题**（条件、`path:line` 和修复方向见[实现文档 §11](../impl/10-time.md#11-源码疑点与文档不一致)，编号一致）：

- T1：`timer.NewScheduler` 静默跳过非法的存量节点（ID ≤ 0、类型 0、期限为零），不删除、不告警、不计数。
- T2：kit 服务 `Config.Now` 为 nil 时缺省是系统时钟，框架库的缺省是业务时钟；chat / account 的 `SystemNow` 为 nil 时退回 `Now`。
- T3：进程级时钟按毫秒存偏移，Registry 时钟保留纳秒，非整毫秒的偏移会让两者相差不到 1ms。
- T4（推断）：宿主按毫秒存期限，同一次调用内的排序可能和重建后的排序不同。
- T5：glsvet 注释过时。T6：OBSERVABILITY 缺推进失败计数。T7：负偏移的说明缺失。

→ [实现文档](../impl/10-time.md)对应：§4 不变量清单、§10 review 检查点、§11 源码疑点与文档不一致。

## 8. 相关文档

- 实现：[impl/10-time.md](../impl/10-time.md)
- 方案：[D-L3 业务时钟与系统时钟](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)、[业务时间只许前进](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)、[D-L1 / D-L2 定时器顺序与未注册类型](../../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)、[R12 kit 批（推进失败计数）](../../feature/DECISIONS-R12-KIT-2026-10-06.md)
- 发版记录：[v1.23.0 app / clk 分册](../../release/v1.23.0/impl-app-own-clk-ops-tool.md)（CLK-1～CLK-6）
- 快速参考：[USER_GUIDE 业务时钟与系统时钟](../../USER_GUIDE.md#业务时钟与系统时钟)
- 执行契约：[roost-coding](../../agent-skills/roost-coding/SKILL.md)
- 其他分区：[01 app](01-app-lifecycle.md)、[02 nest](02-nest-entity.md)、[03 dataengine](03-dataengine.md)、[08 skill](08-skill.md)；[09 kit 服务](09-kit-services.md)、[11 可观测](11-observability.md)、[12 代码生成](12-codegen.md)

[↑ 速览](#速览) · [实现文档](../impl/10-time.md)

## v1.23.1 B4 更正（2026-10-08，未发布）

T1：RR-20261008-21，加载告警，NeedsCleanup 驱动正常事务 Tick 清理，加载阶段不改 DAO 持久字段。T2/T3：RR-20261008-20，业务缺省统一 clock.Now，SystemNow 缺省独立 time.Now；两处偏移均保存完整 Duration。

T4 明确宿主责任：通用 Scheduler 保留纳秒期限，不能替所有宿主截断。World 存储毫秒，当前活动入口按秒武装、Tick 固定毫秒；新宿主在入队前对齐其存储精度，避免重建后同期限优先级发生变化。本次未声称所有自定义宿主有纳秒持久化保证。

T5：glsvet 直接复用主检查带注释 AST，不再二次解析；已有时钟提示回归通过。T6：OBSERVABILITY.md 已补 app.business_time.advance_failed.total 与 timer.invalid_dropped_total。T7：初始偏移允许负值，运行中不能修改；重启仍受持久高水位限制，生产必须为 0。“前拨”不是非负配置约束。
