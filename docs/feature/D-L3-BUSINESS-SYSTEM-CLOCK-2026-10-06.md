# D-L3：业务时钟与系统时钟（2026-10-06）

维护者第六轮决定 D-L3（修订版，[DECISIONS-PENDING 第六轮](../review/DECISIONS-PENDING-2026-10-05.md)，选项来由 [revleft §5](../review/REVIEW-2026-10-06-revleft.md)）：时间分成两个钟，边界写死。本文是方案与实施记录。基线 `e320578c`；源码盘点以当前源码为准（codebase-memory 共享 generation 停在 09-30，本轮用 `rg` 逐行核对）。

## 1. 规则

| 钟 | 是什么 | 用在哪 | 怎么拿 |
| --- | --- | --- | --- |
| **业务时钟** | 真实时间 + `time.logic_offset` | 玩法与业务时间：活动窗口与活动协调器（global 一侧也算）、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗里的游戏时间、业务层计时规则 | `app.BusinessClock(registry)`（`clock.Business` 接口）；服务的 `Config.Now` 由 Mod 注入它；框架库的缺省是进程级业务时钟 `clock.Now()` |
| **系统时钟** | 真实时间 | server 帧率、租约与锁（单实例锁、saga claim、versioned lock…）、超时与 ctx 截止、重试与退避、存储 TTL（Redis、Mongo TTL 索引）、消息 Ack / AckWait、日志、指标、WAL 与审计时间戳 | 继续用 `time` 包 |

约束：

- 偏移只有一个配置来源 `time.logic_offset`，所有进程读同一份（部署时写在公共配置里）。
- 偏移只在启动时生效，运行期没有修改入口（§5 比较了“只许前拨”的做法，没有采用）。
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
| `kit/service/global/activity` `Config.Now`（`activity_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟 | 维护者点名：活动窗口与协调器两端同钟。截止、宽限、退避都是同一个钟上的差值，偏移固定时与系统时长相同 |
| `service/mail` `Config.Now`、`RedisConfig.Now`（`kit/service/mail/mail_mod.go:88` 装配） | 缺省 `time.Now` | Mod 注入业务时钟；**领取租约改用新增的 `Config.SystemNow`** | 邮件创建 / 过期 / 投递 / 已读是业务时间；`ClaimDeadlineUnix` 是 30s 的领取租约，属系统时钟 |
| `service/mail/redis_store.go:165` 信封键 TTL | 等于业务剩余时长 | 业务剩余时长 + `StorageGrace`（缺省 24h） | 存储 TTL 只兜底且要比业务过期长；业务过期一直由 `Envelope.Expired` 判断 |
| `kit/service/rank` `RedisConfig.Now`（`rank_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟 | 同分按“谁先达到”排序是排行规则 |
| `service/session` `Config.Now`（`kit/service/session/session_mod.go` 装配） | 缺省 `time.Now` | Mod 注入业务时钟 | 副本 run 的截止时间发给客户端，是玩法计时；run / claim 没有存储 TTL |
| `demo/internal/service/game/activity.go.tmpl` `tickWorld` / `openCurrentWindow` / 结算 / `now` 字段 | `time.Now()` | `runner.clock()`，缺省 `app.BusinessClock(registry).Now` | World 定时器的时间来源（A1 之后定时器由调用方钉时间，见 `timer/scheduler.go` `SetClock` 注释与 `timer_component.go.tmpl` `scheduler(now)`），窗口 id、关窗截止、结算时间 |
| `demo/internal/service/game/gm.go.tmpl:354`（GM 提前关窗）、`:402`（GM 入会） | `time.Now()` | 业务时钟 | 必须与 runner 算出同一个窗口 id；入会时间是玩法时间 |
| `demo/internal/service/game/spawner.go.tmpl:70/143` | ticker 时间、`time.Now()` | 业务时钟（ticker 频率仍是系统） | 怪物重生是冷却 |
| `demo/game/controllers/player/claim_mail.go.tmpl:74`、`finish_dungeon.go.tmpl:88`、`guild.go.tmpl:104/133` | `time.Now().Unix()` | 业务时钟 | 与邮件业务过期 / session 结算时间 / 公会玩法时间比较 |
| `demo/internal/service/game/purchase_drain.go.tmpl:104` | `time.Now().Unix()` | 业务时钟 | handler 的 `nowUnix` 参数统一是业务事务时间（该 handler 现在不读它） |

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
| kit 服务 | `kit/service/directory`（目录项 TTL 租约）、`kit/service/global` 路由（迁移审计时间）、`kit/service/platform`（订单、支付、投递退避）、`service/match`（排队票据超时、等待放宽）、`kit/service/chat`（消息时间戳与保留期清理）、`kit/service/account`（创建认领 TTL、审计；`wt-revn09f` 在改，本轮不碰） |
| game-demo 模板 | `playerowner.go.tmpl`（玩家归属租约）、`matchmaker.go.tmpl:78`（与 match 服务的票据时间同钟）、`gift_saga.go.tmpl:353` 与 `game/handler/start_gift.go.tmpl:48`（saga 截止）、`game/controllers/player/purchase.go.tmpl:63`（支付时间）、`internal/access/player/tcp/auth.go.tmpl`（会话 id）、`cmd/loadtest`；落在 `game` 目录下的几处标 `//glsvet:system-clock` |

### 3.3 拿不准、交维护者

| 项 | 本轮做法 | 要定什么 |
| --- | --- | --- |
| match 票据（5 分钟超时、等待越久窗口越宽） | 系统时钟 | 这是“真实等待时长”。若要把匹配超时也算业务规则（随偏移测），match 服务与 game 的 `matchmaking.Pools` 要一起换钟 |
| chat 消息时间戳 | 系统时钟 | 消息时间展示给玩家，但保留期清理是存储回收。若展示时间要跟偏移，需拆成“业务展示时间 + 系统保留时间” |
| account 创建时间、认领 TTL | 不改（其他 agent 在改 account） | 账号创建时间若用于业务（新手期、账号年龄奖励），应改业务钟；认领 TTL 是租约，留系统钟 |
| platform 订单 | 系统时钟 | 支付与对账按真实时间。若有“限时礼包”这类业务过期，应由业务侧判断，不放在 platform |
| security 会话令牌有效期 | 系统时钟 | 安全有效期按真实时间 |
| activity 进度账本 `ExpiresAtUnix` | 业务钟打戳，去重靠 Redis 相对 TTL | 字段只做记录，没人比较；去重窗口是“客户端重试视野”（系统概念），相对 TTL 不受偏移影响 |
| saga（`DeadlineAt`、迟到告警） | 系统时钟，不改 | `wt-sagadir` 推送后若要把某些业务截止换钟，另列 |

## 4. 迁移与兼容

**持久化时间戳属于哪个钟**（偏移为 0 时两者相同，存量数据不需要迁移）：

| 钟 | 字段 |
| --- | --- |
| 业务 | activity 协调器的 Activity / Window / Dispatch / ProgressReservation 时间；World 的 `Timers[*].EndUnixMilli`、`timer_next_due`、活动结算时间；mail 信封 `CreatedAtUnix` / `ExpiresAtUnix`、mailbox 条目与 settled claim 的投递 / 更新 / 结算时间；session run 的 `StartedAtUnix` / `DeadlineUnix` / `FinishedAtUnix`；rank 缺省 tiebreak；game-demo 公会建立 / 加入时间、邮件领取与副本领奖的 `nowUnix` |
| 系统 | mail `ClaimDeadlineUnix`；WAL、回执、outbox、saga、订单、目录、路由、账号、聊天、锁与租约的所有时间戳 |

- 偏移跨重启变化（只可能发生在非生产）：前拨后，业务时间戳整体“过去了”，到期的定时器、窗口、邮件在下一次检查时成批处理；后拨后，已打戳的业务截止会晚到一个偏移量。系统时钟的租约不受影响（这正是把 mail 领取租约拆成系统钟的原因）。
- **偏移为 0 时行为不变**：业务时钟 `Now()` = `time.Now()`（`offset == 0` 时直接返回，不做 `Add`）；所有服务在没注入时仍退回 `time.Now`。唯一的行为变化是 mail 信封的 Redis TTL 多了 `StorageGrace`（缺省 24h），只影响空间回收，业务过期判断不变；`TestTheKeyTTLComesFromTheInjectedClock` 改为断言“剩余时长 + grace”。
- **热路径**：nest / dataengine / sync / entity 一行不改，C01 不重跑。`fctx` 请求上下文仍是原来的 `clock.Now()`（一次原子读 + Add）。新增的接口调用只在 kit 服务的单次请求里出现，那里每次都有 Redis 往返（毫秒级），一次接口分派（纳秒级）可以忽略。
- 已生成工程不迁移；`roost project sync` 可取新模板。模板与 codegen 同步改。

## 5. 运行期调整：只许启动时（比较）

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

- §3.3 各项等维护者定：match 票据、chat 消息时间、account 创建时间（`wt-revn09f` 在改 account）、saga 截止（`wt-sagadir` 在改 saga）。
- “所有进程同一个偏移”目前只写在文档与 T-270，没有 doctor 检查；需要时可让 `roost project doctor` 比较各服务配置里的 `time.logic_offset`。
