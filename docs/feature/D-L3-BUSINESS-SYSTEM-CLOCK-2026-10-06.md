# D-L3：业务时钟与系统时钟（2026-10-06）

> **状态（2026-10-06 核对）**：§7 第六轮（`b9fc5342`）与 §8 第八轮（`fa472ee7`）已随 v1.21.0 发布；其中 activity / mail 的系统钟拆分（`SystemNow` / `StorageGrace`）已被 v1.22.0 的“业务时间只许前进”（`3e77beb9`，[方案](BUSINESS-TIME-MONOTONIC-2026-10-06.md)）删除。

维护者第六轮决定 D-L3（修订版，[DECISIONS-PENDING 第六轮](../review/DECISIONS-PENDING-2026-10-05.md)，选项来由 [revleft §5](../review/REVIEW-2026-10-06-revleft.md)）：时间分成两个钟，边界写死。本文是方案与实施记录；第八轮决定的留项（match / chat / account 换钟、doctor 偏移一致检查）见 §8；业务时间只许前进（偏移不得回调）与随之删掉的拆分见 §10。基线 `e320578c`；源码盘点以当前源码为准（codebase-memory 共享 generation 停在 09-30，本轮用 `rg` 逐行核对）。

## 1. 规则

| 钟 | 是什么 | 用在哪 | 怎么拿 |
| --- | --- | --- | --- |
| **业务时钟** | 真实时间 + `time.logic_offset` | 玩法与业务时间：活动窗口与活动协调器（global 一侧也算）、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗里的游戏时间、业务层计时规则 | `app.BusinessClock(registry)`（`clock.Business` 接口）；服务的 `Config.Now` 由 Mod 注入它；框架库的缺省是进程级业务时钟 `clock.Now()` |
| **系统时钟** | 真实时间 | server 帧率、租约与锁（单实例锁、saga claim、versioned lock…）、超时与 ctx 截止、重试与退避、存储 TTL（Redis、Mongo TTL 索引）、消息 Ack / AckWait、日志、指标、WAL 与审计时间戳 | 继续用 `time` 包 |

**更正（§10，业务时间只许前进）**：业务服务内部、只与业务时间比较的退避和租约（activity 派发退避、mail 领取租约）读业务时钟；“租约与锁、重试与退避属系统钟”指依赖存储服务端 TTL、与读系统钟的进程比较、或属于框架基础设施（单实例锁、saga、Nest / DataEngine / Sync）的那些。

约束：

- 偏移只有一个配置来源 `time.logic_offset`，所有进程读同一份（部署时写在公共配置里）。
- 偏移只在启动时生效，运行期没有修改入口（§5 比较了“运行期只许前拨”的做法，没有采用）。
- **业务时间只许前进**（§10）：跨重启也不能让业务时间回到这套部署已经到过的时刻；App 用协调存储里的高水位在启动时拒绝。测试环境要回到过去只能清库重建。
- `env` / `app.env` / `environment` 为 `prod` / `production` 时，启动校验要求偏移为 0，否则拒绝启动。
- 业务过期不靠存储 TTL 判断。存储 TTL 只兜底回收空间，而且比业务过期更长。
- `glsvet` 对业务包里直接调用 `time.Now()` / `time.Since()` / `time.Until()`（以及把 `time.Now` 当函数值传）打印 `hint:`，只提示、不计入违例；豁免写 `//glsvet:system-clock <理由>`。

## 2. 接口

```go
// package clock
type Business interface{ Now() time.Time }      // 业务时钟；系统时钟就是 time 包
type BusinessFunc func() time.Time               // 测试替换用
func NewBusiness(offset time.Duration) Business   // 真实时间 + 固定偏移，构造后不可改
func Process() Business                          // 进程级业务时钟：App 启动时 SetOffset 设下的偏移
// 既有 clock.Now / UnixMilli / SetOffset …保留；SetOffset 只由 App 启动时调用一次，Set / Reset 只给测试。

// package app
const ModBusinessClock ModName = "clock.business"
func BusinessClock(r *Registry) clock.Business   // Registry 里的业务时钟；r 为 nil 或没登记时退回 clock.Process()
```

- `app.NewRegistry(cfg)` 按同一个键 `time.logic_offset` 登记 `clock.NewBusiness(offset)`；`App.run` 在建 Registry 之前读同一个键做生产校验并 `clock.SetOffset`（`fctx.Now` 读它）。两处读的是同一份配置的同一个键。
- 业务服务继续用既有的注入点 `Config.Now func() time.Time`（测试已经大量注入），生产装配由 Mod 传 `app.BusinessClock(r).Now`。业务代码只拿 Registry 里的钟或注入的函数，不直接调全局函数。
- 测试替换：`registry.Register(app.ModBusinessClock, clock.BusinessFunc(fake))` 之前不能有同名登记，所以测试用 `app.NewRegistry` 时把偏移写进 cfg（`time.logic_offset`），或直接给服务的 `Config.Now` 注入。
- 命名：凡是业务时钟都带 `Business`（`clock.Business`、`app.BusinessClock`、`ModBusinessClock`），凡是系统时钟都直接写 `time.Now()`；需要在业务服务里显式区分时，字段叫 `SystemNow`。

## 3. 盘点

范围：非测试 `.go` 里的 `time.Now()` / `time.Since()` / `time.Until()`（82 个文件，含 `scripts/perf` 6 个）、把 `time.Now` 当缺省值注入的位置（`Config.Now` 缺省等，约 25 处）、`clock.*` 的 4 个调用点，以及 game-demo 模板 `demo/**/*.go.tmpl` 的非测试文件。

### 3.1 业务时钟（本轮迁移）

| 位置 | 现状 | 改为 | 理由 |
| --- | --- | --- | --- |
| `clock/clock.go` | 逻辑时钟实现 | 加 `Business` / `NewBusiness` / `Process` | 业务时钟本身 |
| `fctx/context.go:129/144/151` | `clock.Now()` | 不变 | 请求上下文的业务时间，原本就带偏移 |
| `kit/ops/ops_mod.go:267` `server_time_ms` | `clock.UnixMilli()` | 不变 | ops 展示的就是服务器逻辑时间 |
| `timer/scheduler.go:128` 未注入时的缺省 | `time.Now()` | `clock.Now()` | 游戏定时器；World 定时器由调用方钉住时间，缺省只影响没注入的调用方 |
| `ai/controller.go:236`、`actionflow/mission_runner.go:435`、`actionflow/action_runner.go:697` 未注入时的缺省 | `time.Now()` | `clock.Now()` | AI / 行为流的游戏时间 |
| `kit/service/global/activity` `Config.Now`（`activity_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟；~~派发重试排期与进度凭证有效期改读新增的 `Config.SystemNow`（§9）~~ **全部业务时钟，`SystemNow` 已删（§10）** | 维护者点名：活动窗口与协调器两端同钟，截止、宽限、开关窗是业务时间；维护者明确要求 activity 走业务时间。派发退避只由协调器自己比较，没有服务端 TTL；§9 拆到系统钟只为偏移往回调，业务时间不能回退之后不再需要 |
| `service/mail` `Config.Now`、`RedisConfig.Now`（`kit/service/mail/mail_mod.go:88` 装配） | 缺省 `time.Now` | Mod 注入业务时钟；~~领取租约改用新增的 `Config.SystemNow`~~ **领取租约也是业务时钟，`SystemNow` 已删（§10）** | 邮件创建 / 过期 / 投递 / 已读是业务时间；`ClaimDeadlineUnix` 是 30s 的领取租约，只在 mail 服务内部比较、所有实例同一偏移、没有服务端 TTL，拆到系统钟只为偏移往回调 |
| `service/mail/redis_store.go:165` 信封键 TTL | 等于业务剩余时长 | 业务剩余时长 + `StorageGrace`（缺省 24h）；**§10 起固定 `EnvelopeStorageGrace` = 24h，`StorageGrace` 字段已删** | 存储 TTL 只兜底且要比业务过期长；业务过期一直由 `Envelope.Expired` 判断。可配置只为覆盖更大的回拨，回拨被禁止后不再需要 |
| `kit/service/rank` `RedisConfig.Now`（`rank_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟 | 同分按“谁先达到”排序是排行规则 |
| `service/session` `Config.Now`（`kit/service/session/session_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟 | 副本 run 的截止时间发给客户端，是玩法计时；run / claim 没有存储 TTL |
| `demo/internal/service/game/activity.go.tmpl` `tickWorld` / `openCurrentWindow` / 结算 / `now` 字段 | `time.Now()` | `runner.clock()`，缺省 `app.BusinessClock(registry).Now` | World 定时器的时间来源（A1 之后定时器由调用方钉时间，见 `timer/scheduler.go` `SetClock` 注释与 `timer_component.go.tmpl` `scheduler(now)`），窗口 id、关窗截止、结算时间 |
| `demo/internal/service/game/gm.go.tmpl:354`（GM 提前关窗）、`:402`（GM 入会） | `time.Now()` | 业务时钟 | 必须与 runner 算出同一个窗口 id；入会时间是玩法时间 |
| `demo/internal/service/game/spawner.go.tmpl:70/143` | ticker 时间、`time.Now()` | 业务时钟（ticker 频率仍是系统） | 怪物重生是冷却 |
| `demo/game/controllers/player/claim_mail.go.tmpl:74`、`finish_dungeon.go.tmpl:88`、`guild.go.tmpl:104/133` | `time.Now().Unix()` | 业务时钟 | 与邮件业务过期 / session 结算时间 / 公会玩法时间比较 |
| `demo/internal/service/game/purchase_drain.go.tmpl:104` | `time.Now().Unix()` | 业务时钟 | handler 的 `nowUnix` 参数统一是业务事务时间（该 handler 现在不读它） |
| `service/match` `Config.Now`（`kit/service/match/match_mod.go` 装配）**第八轮** | 缺省 `time.Now` | Mod 注入业务时钟 | 匹配是业务逻辑：票据创建 / 过期 / 结束时间、Match 创建时间与超时判断 |
| `demo/internal/service/game/matchmaker.go.tmpl` `matchmaking.Pools` 的 `now` **第八轮** | `time.Now().Unix()` + 豁免 | `app.BusinessClock(registry)`，去掉 `//glsvet:system-clock` | 等待放宽从票据的 `CreatedAtUnix` 量起，必须与 match 服务同钟；偏移只在启动生效，等待时长仍是真实时长 |
| `kit/service/chat` `Config.Now`（`chat_mod.go` 装配）**第八轮** | 缺省 `time.Now`，一个时间戳兼管展示与保留期 | Mod 注入业务时钟，只打新字段 `Message.SentAtUnix`；`StoredAtUnix` 与 `Prune` 截止改读新增的 `Config.SystemNow`（Mod 注入 `time.Now`，为 nil 时沿用 `Now`） | 展示给玩家的时间是业务时间；保留期是空间回收，按真实年龄 |
| `kit/service/account` `Config.Now`（`account_mod.go` 装配）**第八轮** | 缺省 `time.Now`，一个钟兼管全部 | Mod 注入业务时钟：账号 `CreatedAtUnix` / `LastLoginAtUnix`、建角计划与角色 `CreatedAtUnix`、角色 `LastLoginAtUnix` / `LastLogoutAtUnix`；新增 `Config.SystemNow`（Mod 注入 `time.Now`，为 nil 时沿用 `Now`）管会话 token 签发 / 校验 / `Session.ExpiresAtUnix`、`GameServer.UpdatedAtUnix`、`Account.AdminActionAtUnix` | 创建与登录时间是业务用途（新手期、账号年龄、登录展示）；token 有效期是安全边界，运维记录是审计，都是真实时间。名字预约 TTL 由名字目录自己的钟管（Mod 不注入，`time.Now`），本轮不变 |

skill / 战斗：`skill/` 运行时与 `battle.go.tmpl` 都按帧推进，不读墙钟；“游戏时间”就是帧号，没有需要迁移的点。日 / 周重置、赛季切换：框架与 game-demo 都没有实现（`demo/game/ranking` 的赛季是常量，切换是运维操作），业务实现时读 `app.BusinessClock`，glsvet 会提示直接读 `time.Now()` 的写法。

### 3.2 系统时钟（保持 `time`）

核心三大块全部是系统时钟，**本轮不改**：

| 包 | 文件与用途 |
| --- | --- |
| nest | `ticker.go`（server 帧率）、`dispatcher.go`（延迟消息到期）、`dispatch_queue.go` / `execution.go` / `nest_dispatch.go` / `pipelined_completion.go` / `trace.go`（排队、持锁、阶段耗时指标）、`group_transition.go`（迁移锁超时）、`remote_access.go`（Remote 快照读时刻与陈旧判断）、`rollback.go` / `transaction.go`（WAL 记录时间戳、事务 id 前缀） |
| dataengine | `engine/entity_delete.go`（WAL 时间戳）、`mongo_store.go` / `projector.go` / `outbox_worker.go` / `migration_runner.go` 注入的 `now`（回执 TTL 索引、重试退避、outbox 租约） |
| sync | `entitysync/flush.go` / `manager.go` / `mode.go` / `snapshot_budget.go` / `trace.go`（编码耗时、预算窗口、等待时长）、`nettransport/*`（发送截止、读写 deadline）、`syncbus/mirror/envelope.go`（镜像更新时间戳） |
| entity / remoteentity / nestwal / syncstream | 订阅在途超时、Remote 快照发布时刻与 L1 过期、interest 租约、事务跟踪清理、Mongo 回执、写闸等待、WAL fsync 与积压年龄、effect inbox 回执、流活跃度 |
| 基础设施 | `bus/*`、`nats/driver/rpc.go`（消息时间戳、RPC 截止与延迟）、`redis/driver/lock.go`（锁续期）、`cache/*`（L1 过期）、`metrics`、`log`（含 `rotation.go`）、`health`、`httpserver`（请求 id）、`gateway/middleware.go`（ctx 截止）、`manager/engine.go`、`hotcode`、`configdata`（加载 / 生效时间与耗时）、`admin/admin.go`（命令审计）、`security/session_token.go`（令牌有效期）与 `ratelimit.go`、`kit/statslog`、`robot/*`（压测计时）、`scripts/perf/*`（压测工具） |
| app | `app.go:612/616/739`（停机预算）、`singleton.go`（单实例锁租约，自带 `realSingletonClock`） |
| skill | `presentation_asset_cache.go`（缓存 LRU）、`skillsync/*`（outbox 重试、积压年龄） |
| saga | `engine.go` / `command_consumer.go` / `mongo_store.go` / `nest.go` / `record.go` / `dataengine_step_inbox.go`：协调器截止、claim、回执 TTL、迟到告警；saga 在 `wt-sagadir` 改动中，本轮不碰 |
| codegen | `render_player_tcp.go`（生成 TCP 的读写 deadline、耗时）、`project.go`（工具重试截止）、`shutdown_budget.go`（注释） |
| kit 服务 | `kit/service/directory`（目录项 TTL 租约）、`kit/service/global` 路由（迁移审计时间）、`kit/service/platform`（订单、支付、投递退避）、`kit/service/chat`（`StoredAtUnix` 与保留期清理，第八轮起）、`kit/service/account`（会话 token、名字预约 TTL、服务器行与运维记录时间，第八轮起）；match 第八轮整体改为业务时钟（§3.1） |
| game-demo 模板 | `playerowner.go.tmpl`（玩家归属租约）、`gift_saga.go.tmpl:353` 与 `game/handler/start_gift.go.tmpl:48`（saga 截止）、`game/controllers/player/purchase.go.tmpl:63`（支付时间）、`internal/access/player/tcp/auth.go.tmpl`（会话 id）、`cmd/loadtest`；落在 `game` 目录下的几处标 `//glsvet:system-clock` |

### 3.3 拿不准、交维护者

| 项 | 本轮做法 | 要定什么 |
| --- | --- | --- |
| match 票据（5 分钟超时、等待越久窗口越宽） | 系统时钟 | **第八轮已定并实施**：业务时钟，match 服务与 `matchmaking.Pools` 一起换（§8） |
| chat 消息时间戳 | 系统时钟 | **第八轮已定并实施**：拆成业务展示时间 `SentAtUnix` + 系统保留时间 `StoredAtUnix`（§8） |
| account 创建时间、认领 TTL | 不改（其他 agent 在改 account） | **第八轮已定并实施**：创建 / 登录等业务时间走业务钟，token 与认领 TTL 留系统钟（§8） |
| platform 订单 | 系统时钟 | 支付与对账按真实时间。若有“限时礼包”这类业务过期，应由业务侧判断，不放在 platform |
| security 会话令牌有效期 | 系统时钟 | 安全有效期按真实时间 |
| activity 进度账本 `ExpiresAtUnix` | ~~业务钟打戳~~ ~~系统钟打戳（§9 更正）~~ **业务钟打戳（§10）**，去重靠 Redis 相对 TTL | 字段只做记录，没人比较；与 `CreatedAtUnix` 同钟，`ExpiresAtUnix − CreatedAtUnix` 就是 `ReservationTTL` |
| saga（`DeadlineAt`、迟到告警） | 系统时钟，不改 | 第八轮定：保留系统时钟 |

## 4. 迁移与兼容

**持久化时间戳属于哪个钟**（偏移为 0 时两者相同，存量数据不需要迁移）：

| 钟 | 字段 |
| --- | --- |
| 业务 | activity 协调器的 Activity / Window 时间、Dispatch 的 `CreatedAtUnix` / `NextAttemptAtUnix`（§10）/ `LastAttemptAtUnix` / `AckedAtUnix` / `ExhaustedAtUnix` / `AdminActionAtUnix`、ProgressReservation 的 `CreatedAtUnix` / `AppliedAtUnix` / `ExpiresAtUnix`（§10）；mail `ClaimDeadlineUnix`（§10）；World 的 `Timers[*].EndUnixMilli`、`timer_next_due`、活动结算时间；mail 信封 `CreatedAtUnix` / `ExpiresAtUnix`、mailbox 条目与 settled claim 的投递 / 更新 / 结算时间；session run 的 `StartedAtUnix` / `DeadlineUnix` / `FinishedAtUnix`；rank 缺省 tiebreak；game-demo 公会建立 / 加入时间、邮件领取与副本领奖的 `nowUnix`；**第八轮起**：match `Ticket.CreatedAtUnix` / `ExpiresAtUnix` / `ResolvedAtUnix` 与 `Match.CreatedAtUnix`，chat `Message.SentAtUnix`（新字段），account `Account.CreatedAtUnix` / `LastLoginAtUnix`、`RoleCreation.CreatedAtUnix`、`Role.CreatedAtUnix` / `LastLoginAtUnix` / `LastLogoutAtUnix` |
| 系统 | chat `Message.StoredAtUnix`（空间回收按真实年龄）；account `Session.ExpiresAtUnix`（与 token 内的签发时间）、`GameServer.UpdatedAtUnix`、`Account.AdminActionAtUnix`、名字目录预约的到期时间；WAL、回执、outbox、saga、订单、目录、路由、锁与租约的所有时间戳 |

- 偏移跨重启变化（只可能发生在非生产）：前拨后，业务时间戳整体“过去了”，到期的定时器、窗口、邮件在下一次检查时成批处理，业务钟上的退避与租约提前结束（只是早一点重试）。~~后拨后，已打戳的业务截止会晚到一个偏移量。系统时钟的租约不受影响（这正是把 mail 领取租约拆成系统钟的原因）。~~ **更正（§10）**：让业务时间回退的后拨被 App 在启动时拒绝；偏移只能在“距上次运行的真实时间 + 1 分钟容差”以内改小。
- **偏移为 0 时行为不变**：业务时钟 `Now()` = `time.Now()`（`offset == 0` 时直接返回，不做 `Add`）；所有服务在没注入时仍退回 `time.Now`。唯一的行为变化是 mail 信封的 Redis TTL 多了 `StorageGrace`（缺省 24h），只影响空间回收，业务过期判断不变；`TestTheKeyTTLComesFromTheInjectedClock` 改为断言“剩余时长 + grace”。
- **热路径**：nest / dataengine / sync / entity 一行不改，C01 不重跑。`fctx` 请求上下文仍是原来的 `clock.Now()`（一次原子读 + Add）。新增的接口调用只在 kit 服务的单次请求里出现，那里每次都有 Redis 往返（毫秒级），一次接口分派（纳秒级）可以忽略。
- 已生成工程不迁移；`roost project sync` 可取新模板。模板与 codegen 同步改。

## 5. 运行期调整：只许启动时（比较）

（这里比较的是**运行期**能不能改偏移。跨重启的方向限制见 §10：业务时间只许前进，偏移后拨到让业务时间回退时拒绝启动。）

| 做法 | 代价 |
| --- | --- |
| **启动时生效（采用）** | 改偏移要重启全部进程；没有运行期入口，就没有“半数进程已改”的窗口 |
| 运行期只许前拨 | 要一个运维入口，所有进程要协调地同时前拨（否则 game 与协调器窗口错开）；前拨瞬间 World 定时器一次 Tick 触发全部到期节点，跨过的活动窗口不会再开（id 由时钟算出），协调器 `AdvanceExpired` 成批完成过期聚合，邮件与 session run 成批过期——这些都要分批与限流，复杂度高 |

## 6. 测试

| 场景 | 用例 |
| --- | --- |
| 偏移 +1 天，业务时间前移 | `app`：Registry 的业务钟与 `fctx.Now` 都是真实时间 +24h，日期跨天（日重置读它）；`kit/service/global/activity`：经 Mod 装配后协调器的时钟 +24h，过期判定按偏移后的时间（**红绿**）；`kit/service/mail`：经 Mod 装配后邮件业务过期按偏移后的时间，领取租约按真实时间；game-demo：`openCurrentWindow` / `tickWorld` 用 runner 的业务钟开窗、钉 World 定时器（**红绿**） |
| 偏移 +1 天，系统时间不受影响 | `app`：单实例锁的时钟仍是真实时间、续期正常；`context.WithTimeout` 截止按真实时间；mail 信封 Redis TTL = 业务剩余时长 + grace，不含偏移 |
| 生产拒绝非零偏移 | `app`：`env: production` + `time.logic_offset: 24h` → `ValidateServiceConfig` 报错（**红绿**） |
| glsvet 提示 | `cmd/glsvet`：`game` 目录下的 `time.Now()` / `time.Since` / `time.Now` 函数值被提示；豁免注释、非业务目录、测试文件不提示；提示不改退出码 |

## 7. 实施状态

已实施（分支 `dl3clock`，基线 `e320578c`，未发版）。按 §2～§4 落地，没有偏离方案的地方；补充两点：

- glsvet 的提示自己带注释重新解析业务目录（主检查按不带注释的模式解析，改模式会让既有 handler 检查看到不同的 AST），只对业务目录多解析一次。“业务目录”按模块根（向上找 `go.mod`）之下的路径判断，仓库放在名叫 `game` 的目录下不会把每个包都算进去。
- game-demo 的 `tickWorld` 拆出 `tickWorldOnce`，玩家控制器加 `BusinessNow()`；`timer_component.go.tmpl` 不变（它本来就只认调用方钉的时间）。

### 先红后绿

修前（`e320578c` + 新用例）：

```text
$ GOWORK=off go test -count=1 -run TestProductionRefusesANonZeroLogicOffset ./app
--- FAIL: TestProductionRefusesANonZeroLogicOffset (0.00s)
    logic_offset_production_promises_test.go:18: production with time.logic_offset=24h: ValidateServiceConfig error = <nil>, want a refusal naming time.logic_offset
$ GOWORK=off go test -count=1 -run TestTheModWiresTheCoordinatorToTheBusinessClock ./kit/service/global/activity
--- FAIL: TestTheModWiresTheCoordinatorToTheBusinessClock (0.00s)
    business_clock_promises_test.go:40: the coordinator's clock reads 2026-10-06T08:48:45+08:00, 0s from the wall clock; want the business clock, 24h ahead
# 生成的 game-demo（replace 到 worktree）
$ GOWORK=off go test -count=1 -run TestTheWindowFollowsTheBusinessClock ./internal/service/game/
--- FAIL: TestTheWindowFollowsTheBusinessClock (0.00s)
    activity_test.go:400: opened [west/race-1791247800/close], want the window the business clock is in (west/race-1791334200/close), not the wall clock's (west/race-1791247800/close)
```

修后全部通过：

- `app`：`TestProductionRefusesANonZeroLogicOffset`；`TestBusinessClockFollowsTheConfiguredOffset`；`TestOffsetMovesBusinessTimeButNotTheSingletonLease`（偏移 +24h：业务钟与 `fctx.Now` 前移一天且跨日，1h 的 ctx 截止仍是真实 1h，单实例锁按系统钟持有并续期）。
- `kit/service/global/activity`：`TestTheModWiresTheCoordinatorToTheBusinessClock`。
- `service/mail`：`TestMailExpiryIsBusinessTimeAndTheClaimLeaseIsSystemTime`（过期按业务钟、租约按系统钟、只前拨业务钟即过期）；`TestTheKeyTTLComesFromTheInjectedClock`（TTL = 剩余 + grace，与墙钟无关）。
- 生成工程：`TestTheWindowFollowsTheBusinessClock`、`TestTheWorldTickCarriesBusinessTime`。
- `cmd/glsvet`：`TestClockHints*`（4 种读法命中、改名导入命中、三种豁免；非业务目录、同名局部变量、测试文件、关掉开关、模块根之上的 game 段都不命中；提示不计入违例）。在生成工程里临时放一个 `game/activity` 下的 `time.Now()`，`glsvet ./...` 打印 1 条 hint、退出码 0。

### 验证

全部 `GOWORK=off`：

- `gofmt -l` 为空；`go build ./... && go vet ./...`（另跑 `go vet -tags integration ./service/mail`）。
- `go test -race -count=3`：app、clock、service/mail、kit/service/mail、kit/service/global/activity、kit/service/rank、kit/service/session、service/session、timer、ai、actionflow、cmd/glsvet、fctx。
- `go test -count=1 ./kit/... ./service/... ./skill/... ./saga/...`；根包 `go test -count=1 .`；`go test -count=1 ./codegen/...`；`go generate ./...` 后 porcelain 不变。
- `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`：无违例、无提示（核心没改）。
- 生成 game-demo、replace 到 worktree：`go build ./... && go vet ./... && go test ./...` 全过，`gofmt -l` 为空，`glsvet ./...` 无违例、无提示。
- 没有跑真实依赖：mail 的 integration 用例只改了宽限参数，编译通过。

### 未完成 / 后续

- §3.3 各项维护者已在第八轮定下，实施见 §8；saga 截止保留系统时钟。
- “所有进程同一个偏移”的 doctor 检查已在第八轮加上（§8）。

## 8. 第八轮：留项实施（2026-10-06）

维护者第八轮决定（[DECISIONS-PENDING 第八轮](../review/DECISIONS-PENDING-2026-10-05.md)）：match、chat 展示时间、account 业务时间走业务时钟；saga 截止保留系统时钟；`roost doctor` 检查偏移一致。分支 `dl3b`，基线 `2a7d2a65`（含 o33 合入），未发版。源码盘点用 `rg` 逐行核对当前源码（codebase-memory 共享 generation 停在 09-30）。

### 8.1 改动

| 项 | 改法 | 兼容 |
| --- | --- | --- |
| match | `kit/service/match` Mod 给 `Config.Now` 注入 `app.BusinessClock(r).Now`；`service/match` 本身不变（它所有时间本来就读 `Config.Now`）。game-demo `matchmaker.go.tmpl` 的 `matchmaking.Pools` 改读 `app.BusinessClock(registry)`，去掉 `//glsvet:system-clock` | 偏移只在启动生效，票据 TTL、等待放宽量的仍是真实时长；票据上的绝对时间戳变成业务时间（§4）。偏移为 0 时不变 |
| chat | `Message` 加 `SentAtUnix`（`json:"sent_at_unix,omitempty"`），读 `Config.Now`（Mod 注入业务时钟）；`StoredAtUnix` 与 `Prune` 截止改读新增的 `Config.SystemNow`（Mod 注入 `time.Now`；为 nil 时沿用 `Now`，与 mail 的 `SystemNow` 同一约定） | 持久格式只增不改：旧消息没有 `sent_at_unix`，读出时（`forReader`：Publish 重放应答、History / Conversation / Scrollback）用 `StoredAtUnix` 兜底，存着的数据不改写；旧数据写入时偏移为 0（生产强制），两个钟一致。客户端展示改读 `SentAtUnix`；game-demo 的聊天推送不带时间，没有要改的 |
| account | `Config.Now` 定为业务时钟（Mod 注入）：账号 / 角色创建、登录、登出；新增 `Config.SystemNow`（Mod 注入 `time.Now`；nil 沿用 `Now`）：会话 token 签发与校验、`Session.ExpiresAtUnix`、`UpsertServer` 的 `UpdatedAtUnix`、`ResolvePendingCreation` 的 `AdminActionAtUnix` | `creation_table.go` 与 `create_role.go` 一行没改（建角计划的 `CreatedAtUnix` 本来就读 `Config.Now`，换钟发生在 Mod 注入处），判定表格子不动。`rg` 盘点：仓库里没有新手期 / 账号年龄的业务实现，game-demo 不读账号时间 |
| doctor | `codegen/internal/roost/logic_offset_doctor.go`：`roost project doctor` 加一行 `time:logic_offset`。dev（`configs/service/config.<service>.yaml`）、prod example、k8s secret example 三套各自比较每个服务的值（不写 = 0s），按 App 的读法解析（`app.ConfigDuration`：空 / `"0"` / 0 是 0，字符串按 Go 时长，非 0 的裸数字要单位）；不一致 FAIL 并列出 `service=value`，格式错 FAIL 并点名文件与原值 | 三套之间不比：测试环境前拨、生产为 0 是正常的；比跨套会让每个用偏移的测试环境都报错。仓库外的真实生产配置看不到 |
| 生成文档 | `render_docs.go` 的时间规则补上匹配、聊天展示、账号时间与 doctor 检查 | 生成工程的 AGENTS 说明随 `roost project sync` 更新 |

### 8.2 先红后绿

修前（`2a7d2a65` + 新用例；chat 的展示时间在修前只有 `StoredAtUnix`，红跑时临时用它代替 `SentAtUnix` 断言，其余断言原样）：

```text
$ REDIS_ADDR=<隔离 Redis> GOWORK=off go test -tags integration -count=1 -run TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions ./kit/service/integration/
--- FAIL: TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions (0.01s)
    business_clock_test.go:61: ticket.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:62: ticket.ExpiresAtUnix = 2026-10-06T09:43:12+08:00, 5m0s from the wall clock; want 24h5m0s
    business_clock_test.go:74: message.StoredAtUnix (shown to players, pre-fix) = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:97: account.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:98: account.LastLoginAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:106: role.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
$ GOWORK=off go test -count=1 -run TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees ./codegen/internal/roost/
--- FAIL: TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees (8.66s)
    logic_offset_doctor_promises_test.go:59: only the dev game config sets time.logic_offset 24h: doctor says {Name: Status: Detail:} (present=false), want FAIL
```

修后全部通过：

- `kit/service/integration`（真实 Redis，Mod 从配置 Init / Provide，偏移 +24h）：`TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions`——match 票据创建 / 过期、chat `SentAtUnix`、account 创建 / 最近登录、角色创建都是真实时间 +24h；chat `StoredAtUnix` 是真实时间，保留期 1h 的 `Prune` 不删刚存的消息；会话 `ExpiresAtUnix` = 真实时间 + 30min，token 校验通过。负对照：把 `Prune` 截止临时改回业务时钟，用例报 `Prune with a 1h retention dropped 1 message(s) stored a moment ago`。
- `kit/service/chat`：`TestDisplayTimeIsBusinessTimeAndRetentionIsSystemTime`（只前拨业务时钟一周不清理，系统时钟过保留期才清理）；`TestAMessageStoredBeforeSentAtUnixShowsItsStoredTime`（旧 JSON 没有 `sent_at_unix`，History 与重放应答用 `StoredAtUnix` 兜底，存着的消息不改写）。
- `kit/service/account`：`TestAccountTimesAreBusinessTimeAndSessionsAreSystemTime`（创建 / 登录 / 登出是业务时钟，服务器行与 token 是系统时钟；只前拨业务时钟 token 仍有效，系统时钟过 TTL 才失效）。
- `codegen/internal/roost`：`TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees`（dev 只有 game 配 24h → FAIL，列出 `game=24h`、`activity=0s`、`match=0s` 与文件模式，doctor 返回错误；全部未配置 → OK；dev 全部 24h、prod example 0 → OK；secret example 写 `1d` → FAIL 点名文件和 `"1d"`）。

### 8.3 验证

全部 `GOWORK=off`：`gofmt -l` 为空；`go build ./... && go vet ./...`，另跑 `go vet -tags integration ./kit/service/integration/`；`go test -race -count=3 ./service/match/ ./kit/service/match/ ./kit/service/chat/ ./kit/service/account/`；`go test -count=1 ./kit/... ./service/...`；根包 `go test -count=1 .`；`go test -count=1 ./codegen/...`；`go generate ./...` 后 porcelain 只有本轮改动；integration 只跑 `-run 'TestEvery|TestOffsetMoves'`（隔离 Redis，唯一前缀、用完删键）。生成 game-demo、replace 到 worktree：`go build ./... && go vet ./... && go test ./...` 全过，`gofmt -l` 为空，`glsvet ./...` 退出码 0、无违例、无提示（豁免剩 5 处：支付时间、saga 截止 2 处、玩家归属租约 2 处）。Nest / DataEngine / Sync / Entity 没改，核心 glsvet 与 C01 不重跑。

## 9. 更正：activity 协调器的重试排期与凭证有效期属系统钟（2026-10-06，发版前审查）

发版前审查指出 §3.1 把整个 activity 协调器划成了业务时间，违反 §1 的“重试与退避属系统钟”：派发的 `NextAttemptAtUnix` 与进度凭证的 `ExpiresAtUnix` 读业务钟，
测试环境两次运行之间把偏移往回拨 D，欠下的派发要多挂 D。改法仿照 mail 的领取租约：`Config.SystemNow`（nil 时沿用 `Now`，Mod 注入 `time.Now`），
派发排期（创建、退避、重开）、全部到期比较、owed 索引查询与凭证 `ExpiresAtUnix` 读它；活动窗口、宽限、开关窗与记录上的事件时间戳仍是业务钟。上面 §3.1、§3.3、§4 的对应行已改。
分支 `auditfu`，先红后绿与验证见[发版前审查观察收尾](../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md) §1；同一记录 §2 写明 mail 信封存储宽限（24h）只覆盖往回拨不超过 24h 的偏移。

**再更正（§10）**：本节的拆分只为“偏移往回调”。业务时间只许前进之后，activity 派发退避与进度凭证回到业务钟，`Config.SystemNow` 删除；“往回拨 D 多挂 D”的场景由 App 启动拒绝取代。

## 10. 业务时间只许前进（2026-10-06，下一轮规划第 1 项）

维护者指示（[下一轮规划 §1](../review/NEXT-ROUND-PLAN-2026-10-06.md)）：业务时间不能往回调，为回调写的特殊逻辑一并清理。方案与实施记录：[业务时间只许前进](BUSINESS-TIME-MONOTONIC-2026-10-06.md)。要点：

- **约束**：同一套部署的业务时间单调不减。App 把部署级高水位存在协调存储（单实例锁的 `SingletonStore`，共享 Redis，键 `<singleton.key_prefix>:business_time`，不过期）里；单实例锁之后、第一个 Mod Init 之前，`真实时间 + 新偏移 < 高水位 − 1 分钟` 就拒绝启动（`app.ErrBusinessTimeMovedBack`，点名偏移、高水位、写入者与键），否则推进高水位；运行中每 10s 推进一次。各模块不感知。
- **谁检查**：非生产里开了单实例锁的进程，以及配了非 0 偏移的进程（没开锁的只为高水位打开一个连接；装不了就拒绝启动，`app.ErrBusinessTimeGuardMissing`）。生产不检查：偏移强制为 0，生产行为不变。
- **容差 1 分钟**：吸收主机之间的时钟偏差与运行中推进的间隔；放过的回退与生产里多主机本来就有的偏差同一量级。
- **回到过去**：只能清库重建（连同这个键）。只删键保留数据等于跳过守卫，不要这样做。

**时钟归属的更正**（逐项理由见方案 §4）：

| 位置 | 之前 | 现在 | 理由 |
| --- | --- | --- | --- |
| activity 派发 `NextAttemptAtUnix`（创建 / 退避 / 重开）、到期比较、owed 查询 | 系统钟（§9） | 业务钟，`Config.SystemNow` 删除 | §9 的拆分只为偏移往回调；值只由协调器自己比较，没有服务端 TTL。维护者要求 activity 走业务时间 |
| activity 进度凭证 `ExpiresAtUnix` | 系统钟（§9） | 业务钟 | 只是记录；去重靠 Redis 相对 TTL |
| mail 领取租约 `ClaimDeadlineUnix` | 系统钟 | 业务钟，`Config.SystemNow` 删除 | 只在 mail 服务内部比较、所有实例同一偏移、存在没有 TTL 的 versionstore 里；前拨时提前结束，重试拿到同一个 token |
| mail 信封存储宽限 | `RedisConfig.StorageGrace`，缺省 24h | 固定 `EnvelopeStorageGrace` = 24h，字段删除 | “存储 TTL 比业务过期长”是 §1 的独立原则（过期后领取报 `ErrExpired` 而不是 `ErrMailMissing`，并吸收主机偏差），保留；可配置只为覆盖更大的回拨 |
| chat `StoredAtUnix` 与 `Prune` 保留期 | 系统钟 | **不变** | 空间回收按真实年龄；前拨一周时按业务钟会删掉刚存的消息 |
| account 会话 token 签发 / 校验、`Session.ExpiresAtUnix` | 系统钟 | **不变** | 安全有效期，`security.VerifySessionToken` 按真实时间校验 |
| account `GameServer.UpdatedAtUnix`、`AdminActionAtUnix` | 系统钟 | **不变** | 运维记录与审计 |
| match | 业务钟（§8） | 不变 | 没有拆分 |

偏移为 0 时两个钟相等，以上合并在生产里没有可观察差异；信封宽限仍是 24h。
