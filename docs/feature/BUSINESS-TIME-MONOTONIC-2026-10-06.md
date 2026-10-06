# 业务时间只许前进（2026-10-06）

维护者 2026-10-06 指示（[下一轮规划 §1](../review/NEXT-ROUND-PLAN-2026-10-06.md)）：业务时钟（真实时间 + `time.logic_offset`）在一套部署的整个生命期内不得往回走；为“偏移往回调”写的特殊逻辑随之清理。
本文是方案与实施记录。基线 `2a4c835d`，分支 `monotime`。源码核对以当前源码为准：codebase-memory 共享 generation 停在 09-30（早于 D-L3），本轮用 `rg` 与直接阅读逐行核对 `app`、`clock`、`kit/service/{global/activity,mail,chat,account,match}`、`service/mail`。

## 1. 目标

- **单调约束**：同一套部署的业务时间只许前进。偏移可以前拨（或保持），不能让业务时间回到这套部署已经走到过的时刻；要“回到过去”只能清库重建。
- **由 App 统一兜住**：启动时检查、运行中推进高水位，都在 `app` 里做，各模块不感知。
- **清理**：只为“偏移往回调”存在的系统钟拆分合并回业务钟；属于系统用途的保留，写明理由。

## 2. 高水位放在哪：App 的协调存储（共享 Redis）

| 候选 | 取舍 |
| --- | --- |
| **共享 Redis，经单实例锁已有的 `app.SingletonStore`（采用）** | App 在任何 Mod Init 之前就已经用它（`kitredis.SingletonStore`，自己建连接、不依赖 Mod），生成工程都装了它；只用已有的原子操作 `CompareAndSet`（`ttl = 0` 即不过期的 `SET`），core 不加驱动依赖、kit 不加新实现 |
| global 服务 | 启动时 global 不一定在线，要引入“先起 global”的顺序依赖；global 自己启动时又要另找地方存；RPC 失败的语义更复杂 |
| Mongo | App 层没有 Mongo 连接，要么新增一个，要么依赖 Mod，都比 Redis 多一层 |

键：`<singleton.key_prefix>:business_time`（生成工程的 `key_prefix` 是 `roost:<project>:singleton`，同一套部署所有服务一样——`Live` 跨服务查询本来就要求如此）。
值：`<业务时间 unix 毫秒>|<写入者的偏移>|<server_type>:<sid>`，只比较第一段；后两段给拒绝信息点名用。不设 TTL：高水位要比任何一次停机都活得久。

## 3. 规则

- **谁检查**：非生产环境里，开了单实例锁（`singleton.enabled`）的进程，以及配了非 0 偏移的进程。
  - 生产（`env` / `app.env` / `environment` 为 `prod` / `production`）不检查：偏移强制为 0，业务时间就是真实时间，靠 NTP；这样生产行为一字不变（不多一次 Redis 读写，也不会因为主机时钟偏差多出一种拒绝启动）。
  - 偏移非 0 而进程没有协调存储（bootstrap 没装 `App.Singleton`，或没写 `singleton.key_prefix`）：**拒绝启动**并说明怎么补——不能在没有守卫的情况下用偏移。`singleton.enabled` 可以仍是 false，App 只为高水位打开一个连接。
  - 偏移为 0、没开单实例锁的进程（如生成工程里不带 dataengine 的服务）不检查：它们的业务时间就是真实时间；同一套部署从 +24h 改回 0 时，开了锁的服务（带 dataengine 的 game 等）会拒绝启动，部署起不来，运维就能看到。
- **启动**：单实例锁拿到之后、第一个 Mod Init 之前。读高水位 H，本进程业务时间 `B = 真实时间 + 新偏移`：`B < H − 容差` 拒绝启动，错误 `app.ErrBusinessTimeMovedBack` 点名偏移、B、H、H 的写入者与键；否则把高水位推到 `max(H, B)`。读写失败也拒绝启动（fail-closed：守卫读不到就无法证明没回退）。
- **运行中**：每 10s 把高水位推到当前业务时间（读到的已经更大就不写）。失败只记 Warn、下一拍再试，不 fail-stop：高水位只守“下一次启动”，运行中的业务不依赖它；进程崩溃时高水位最多落后 10s，这一段由容差吸收。
- **原子推进**：只用 `CompareAndSet`。`CAS(nil, B)` 首次写入；不生效时拿到当前值 H，`H ≥ B` 就不写，否则 `CAS(H, B)`；被别的进程抢先就用对方的值重来，最多 8 次（值单调，竞争只会让 H 变大）。
- **容差 1 分钟**：吸收各主机之间的时钟偏差（NTP 同步的主机在毫秒级，没有同步的虚拟机、开发机通常在秒级），以及运行中推进的 10s 间隔。被放过的回退最多 1 分钟，与生产里多主机之间本来就有的时钟偏差同一量级——下面删掉的拆分在生产里本来就承受这种偏差。容差不做成配置：调大等于放宽约束，调小会让正常的主机偏差拒绝启动。
- **回到过去**：只能清库重建（含这个键）。只删这个键而保留业务数据，等于跳过守卫，已打戳的业务截止会晚到一个回退量——文档写明不要这样做。

## 4. 逐项核对：哪些拆分只为回调

判据：拆到系统钟的理由若是“偏移往回调”，在单调业务时间下合并回业务钟；若是系统用途（依赖 Redis 服务端 TTL、与另一个读系统钟的进程比较的租约、Ack、超时、空间回收、安全有效期、审计），保留。

| 位置 | 判定 | 处理 |
| --- | --- | --- |
| activity 派发 `NextAttemptAtUnix`（创建、退避、重开）、到期比较、owed 索引查询 | 只为回调（`5a3c4a60` 的修复理由就是“往回拨 D 多挂 D”）；值只由协调器自己与自己比较，没有服务端 TTL。前拨时退避提前结束，只是早一点重试 | 改回业务钟，删 `Config.SystemNow`；`ReopenDispatch` 写业务钟的当前时间（仍是“立即可取”）。维护者明确要求 activity 走业务时间 |
| activity 进度凭证 `ExpiresAtUnix` | 只是记录，没人比较；账本去重靠 Redis 相对 TTL，与绝对时间戳无关 | 改回业务钟（与 `CreatedAtUnix` 同钟） |
| mail 领取租约 `ClaimDeadlineUnix` | 只为回调（代码注释原文“偏移在两次运行之间往回调时……多挂一个偏移量”）；只在 mail 服务内部比较，所有实例同一个偏移，存在没有 TTL 的 versionstore 里。前拨时租约提前到期，重试拿到的是**同一个 token**（设计如此，见 `Entry.ClaimToken`），不会重复发放 | 改回业务钟，删 `Config.SystemNow` |
| mail 信封存储宽限 `StorageGrace` | 可配置这一点只为回调（文档教人“调大它覆盖更大的回拨”）；但“存储 TTL 比业务过期更长”是 D-L3 §1 的独立原则（过期后领取报 `ErrExpired` 而不是 `ErrMailMissing`，并吸收主机时钟偏差） | 删 `RedisConfig.StorageGrace` 字段，宽限固定为 `EnvelopeStorageGrace = 24h`（值不变，生产行为不变） |
| chat `StoredAtUnix` 与 `Prune` 保留期 | 系统用途：空间回收按真实年龄；前拨一周时按业务钟会删掉刚存的消息 | 保留 `SystemNow` |
| account 会话 token 签发 / 校验、`Session.ExpiresAtUnix` | 系统用途：安全有效期，token 由 `security.VerifySessionToken` 按真实时间校验 | 保留 |
| account `GameServer.UpdatedAtUnix`、`AdminActionAtUnix` | 系统用途：运维记录与审计 | 保留 |
| match | 第八轮整体走业务钟，没有拆分 | 不变 |

## 5. 改动面

- `app`：新增 `business_time.go`（高水位守卫），`App.run` 在单实例锁之后、Mod Init 之前调用；`SingletonStore` 文档写明它也承载高水位键；`clock` 包注释同步。
- `kit/service/global/activity`：删 `Config.SystemNow` 与全部读取点；Mod 不再注入；删 `system_clock_promises_test.go`（回调场景，见 §6），新增单调业务时间下的行为用例。
- `service/mail`、`kit/service/mail`：删 `Config.SystemNow`、`RedisConfig.StorageGrace`；`DefaultEnvelopeStorageGrace` 改名 `EnvelopeStorageGrace`。
- `kit/redis`：只加一个 integration 用例（真实 Redis 上 +24h 写下高水位、改回 0 被拒），实现不变。
- `docs/agent-skills/roost-coding/SKILL.md`：时钟一段把“mail 的 `Now` / `SystemNow`”的例子换成 chat，并写明不要再为回调拆系统钟。
- 文档：D-L3 方案（新增 §10“业务时间单调”，更正 §1、§3.1、§3.3、§4、§5、§9）、USER_GUIDE、TROUBLESHOOTING T-270、发版前审查收尾记录追加更正、CHANGELOG、交接 §7、规划第 1 项状态、生成工程的说明（`render_docs.go`）。

## 6. 兼容

- **生产（偏移 0）行为不变**：生产不跑守卫；两个钟相等，activity / mail 合并回业务钟没有可观察差异；信封宽限仍是 24h。
- **公开 API**：删除 `activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`，`mail.DefaultEnvelopeStorageGrace` 改名 `EnvelopeStorageGrace`。它们都是 v1.21.0 新加的，只有 kit Mod 与测试在用；手工装配设置了它们的调用方编译报错，删掉那一行即可。
- **持久格式不变**：activity / mail 记录上的字段不变；偏移为 0 时写下的系统钟时间戳与业务钟一致。偏移非 0 的测试环境里，旧代码按系统钟写的 `NextAttemptAtUnix` / `ClaimDeadlineUnix` 比业务钟早一个偏移，升级后只会更早到期（重试提前），不会多挂。
- **测试环境**：开了单实例锁的进程多一个不过期的 Redis 键，每 10s 一次 CAS；从这一版起，偏移改小超过“距上次运行的真实时间 + 1 分钟”会拒绝启动；要用偏移的进程必须能连上协调存储。
- `5a3c4a60` 的 `system_clock_promises_test.go`（偏移从 +24h 拨回 0 后派发多挂 24h）删除：它构造的场景现在在 App 启动时就被拒绝，组件层不再承诺它；由 `app` 的启动拒绝用例取代，组件层改测“单调业务时间下行为不变”。

## 7. 验证计划

- 红绿：`app` 新用例——高水位在 +24h 偏移下写到 H，偏移改回 0 启动：修前能启动（红），修后被拒并点名；前拨、同偏移重启、回退在容差内、首次启动都照常；生产跳过；偏移非 0 而没有协调存储被拒；运行中高水位推进；读失败拒绝启动。
- 每删一处：activity、mail 各有用例证明单调业务时间（含偏移恒定 +24h、只前拨）下退避 / 到期 / 租约 / 凭证的行为与原来相同；偏移为 0 时两钟相等。
- `gofmt`、改动包 `vet` 与 `-race -count=3`、根包、`go build ./... && go vet ./...`、`codegen` 测试（改了生成说明）。真实依赖：`remote-acceptance.lock` 不存在时只跑 mail integration 自己的用例。

## 8. 实施状态

已实施（分支 `monotime`，基线 `2a4c835d`，未发版）。按 §2～§6 落地，没有偏离方案的地方。

### 先红后绿

修前 = 当前代码去掉 `App.run` 里对 `startBusinessTimeGuard` 的调用（没有守卫；新用例引用的 `ErrBusinessTimeMovedBack` 等已声明以便编译）：

```text
$ GOWORK=off go test -count=1 -run 'TestBusinessTimeMovingBackRefusesToStart|TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime|TestAnUnreadableHighWaterMarkRefusesToStart' ./app/
--- FAIL: TestBusinessTimeMovingBackRefusesToStart (0.01s)
    business_time_promises_test.go:59: moving time.logic_offset from 24h back to 0 started the service; business time must only move forward
--- FAIL: TestAnUnreadableHighWaterMarkRefusesToStart (0.01s)
    business_time_promises_test.go:165: started without being able to read the business time high-water mark
--- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime (10.06s)
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/a_non-zero_offset_checks_and_keeps_the_mark (5.02s)
        business_time_promises_test.go:184: run did not return
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/forward_offset_serves_and_closes_the_store_at_stop (0.01s)
        business_time_promises_test.go:199: high-water mark = 0001-01-01 00:00:00 +0000 UTC, want real time + 1h
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/no_store_to_keep_the_mark (5.01s)
        business_time_promises_test.go:213: run did not return
```

（`TestTheHighWaterMarkAdvancesWhileRunning` 修前同样红：`high-water mark stayed at 0001-01-01 00:00:00 +0000 UTC while the service ran`。）

修后全部通过：

- `app`：`TestBusinessTimeMovingBackRefusesToStart`（+24h 写下的高水位、偏移改回 0：返回 `ErrBusinessTimeMovedBack`，错误点名 `time.logic_offset = 0s`、高水位时刻、键与写入者 `24h0m0s|game:1`、清库重建；服务没有 Init；高水位不变；单实例锁已释放）、`TestBusinessTimeMayMoveForwardOrStay`（首次启动、同偏移重启、前拨、容差内回退、改小不超过经过的真实时间：都照常启动，高水位只增）、`TestTheHighWaterMarkAdvancesWhileRunning`（运行中推进，停机后不再写）、`TestAnUnreadableHighWaterMarkRefusesToStart`、`TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime`（没开锁：非 0 偏移只为高水位打开连接、拒绝 / 正常停机都关闭它；没有 opener 报 `ErrBusinessTimeGuardMissing`；偏移为 0 不碰存储）、`TestProductionDoesNotRunTheBusinessTimeGuard`。既有单实例锁与 D-L3 用例全部照常（替身存储把高水位键单独记账，`ttl = 0` 按 Redis 语义不过期）。
- `kit/redis`（真实 Redis，隔离环境 `~/.roost-it/roost-dataengine-it`，随机前缀、用完删键）：`TestBusinessTimeHighWaterMarkOnRealRedis`——完整 App 经 `kitredis.SingletonStore` 以 +24h 启动、停机后高水位 ≈ 真实时间 + 24h；偏移改回 0 启动返回 `ErrBusinessTimeMovedBack`，键不变。
- 删拆分后“单调业务时间下行为不变”：`kit/service/global/activity` 的 `TestDispatchBackoffAndProofExpiryRunOnTheMonotonicBusinessClock`（+24h 跨重启：立即可取的照样立即可取；退避差 1 秒不可取、owed 清单不列，到点可取并列出；凭证 `ExpiresAtUnix − CreatedAtUnix = ReservationTTL`；前拨一天退避提前结束）与 `TestAReopenedDispatchIsOwedNow`（重开写业务钟当前时间、立即可取、owed 列出）；`service/mail` 的 `TestMailExpiryAndTheClaimLeaseRunOnTheMonotonicBusinessClock`（+24h：过期 = 业务时间 + 1h；租约恰好 30s，差 1 秒 `ErrClaimHeld`，到点重试拿到同一个 token；前拨一分钟租约提前结束、仍是同一个 token；前拨过过期点 `ErrExpired`）。这些用例只注入一个钟，在拆分的代码上同样通过（`SystemNow` 为 nil 时沿用 `Now`）——它们证明的正是“合并前后行为相同”。偏移为 0 时两个钟相等，生产没有差异。
- `5a3c4a60` 的 `kit/service/global/activity/system_clock_promises_test.go` 删除：它构造的“偏移从 +24h 拨回 0”在 App 启动时就被拒绝，组件不再承诺；由上面的 app 与 kit/redis 用例取代。`TestTheModWiresTheCoordinatorToTheBusinessClock` 去掉对 `SystemNow` 的断言。mail 的 `TestTheKeyTTLComesFromTheInjectedClock` 改用 `EnvelopeStorageGrace`；integration 用例改为在包内把宽限缩到 1ms。

### 验证

全部 `GOWORK=off`：

- `gofmt -l` 为空；`go build ./... && go vet ./...`；`go vet -tags integration ./service/mail/ ./kit/service/integration/ ./kit/redis/`。
- `go test -race -count=3 ./app/ ./clock/ ./service/mail/ ./kit/service/mail/ ./kit/service/global/activity/`：通过。
- `go test -count=1 ./kit/... ./service/...`：通过；根包 `go test -count=1 .` 通过；`go test -count=1 ./codegen/...` 通过（`render_docs.go` 的时间规则改了一句）。
- 真实依赖（`remote-acceptance.lock` 不存在时，只跑自己的用例）：`go test -tags integration -run TestBusinessTimeHighWaterMarkOnRealRedis ./kit/redis/`、`go test -tags integration -run TestIntegration ./service/mail/` 通过。
- Nest / DataEngine / Sync / Entity 没改，glsvet 与 C01 不重跑；没改生成形状（只改生成说明的一句文字），没有重新生成 game-demo。

### 未完成 / 风险

- 没开单实例锁、偏移为 0 的进程不检查（方案 §3）：同一套部署从 +24h 改回 0 时由开了锁的服务拒绝启动；只有这类进程、没有任何开锁服务的部署检查不到。
- 运行中推进失败只记 Warn，没有指标；进程崩溃时高水位最多落后 10s（容差吸收）。
- 只在单机 Redis 上验证；Redis Cluster 下高水位是单键，不涉及跨槽。
