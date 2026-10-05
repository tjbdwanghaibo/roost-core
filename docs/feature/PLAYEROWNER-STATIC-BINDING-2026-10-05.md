# game-demo 玩家所有权：按静态绑定简化（2026-10-05）

- 范围：`demo/internal/service/game/playerowner.go.tmpl`（`PlayerOwners`）、`demo/game/playerroute/playerroute.go.tmpl`（Redis 共享表 `Store`），以及调用方 `demo/game/controllers/player/enter_game.go.tmpl`、`gift_saga.go.tmpl`、`matchmaker.go.tmpl`、`service.go.tmpl`、`activity.go.tmpl`、`demo/internal/access/player/tcp/auth.go.tmpl`。
- 基线：main `3127d37c`。`playerowner.go.tmpl` 最后一次改动是 `890abdda`（RR-20261004-14），所以 [状态机文档](PLAYEROWNER-LEASE-STATE-MACHINE-2026-10-04.md) 里的 `po:N` 行号仍然有效。本文 `sv:N` 指 `service.go.tmpl`，`gs:N` 指 `gift_saga.go.tmpl`，`mm:N` 指 `matchmaker.go.tmpl`，`ac:N` 指 `activity.go.tmpl`，`eg:N` 指 `enter_game.go.tmpl`，`au:N` 指 `auth.go.tmpl`。
- 性质：方案。**状态（2026-10-05）**：按 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §9 的合并拆分，本文第 2 笔（去掉 sid 锁部分，即 App 锁方案第 3 笔）已实施，提交 `f051e24a`，实施记录与回归去向见该方案 §13；赠礼与 matchmaker 按 `server_id` 路由（本文第 3 笔）也已实施：matchmaker 改用 `Resident` 随 App 锁方案第 3 笔完成，赠礼按 `FromSID` 准入与转交是 App 锁方案第 4 笔，记录（含与 §3.3 的差异：转交接收方核对信封与载荷一致、`Encode` 拒绝无 sid 的赠礼）见该方案 §13；§3.3 的旧 payload 兜底按维护者决定未做。
- 取代：状态机文档 §7 推荐的“重写核心、外部接口不变”（选项 A）。那份文档的前提是“玩家可以在进程间动态迁移”，维护者 2026-10-05 确认这个前提不成立。它对现状的分析（§1～§4）仍然是本文删除清单的依据。

> **2026-10-05 维护者决定**（本文其余部分保留原文，与下列决定冲突处以决定为准）：
> 1. **已生成工程不考虑**：不写迁移步骤，不做兼容旧 payload 的兜底。§5 作废；§3.3 的旧 payload 兜底与 D4 作废。
> 2. **D1～D3 按推荐**：启动拿不到锁就等，上限 2×TTL；TTL / 续期 / 保护带 15 / 3 / 5s；D3 的对象换成 App 锁之后，[App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §3.4 建议简化为“窗口耗尽即 fail-stop”，列为该方案的 D-A 请维护者确认。
> 3. **D4 作废**（随第 1 条）。
> 4. **D5 改为“App 层单实例锁，只覆盖崩溃重启短暂并存，不做模块级 fencing”**：同一服务类型 + sid 的进程锁由 core `app` 在任何 Mod `Init` 之前获取、在全部 Mod 停完之后释放，失锁走 App 统一的 fail-stop；DataEngine、activity、PlayerOwners 等模块不感知锁。见 [APP-SINGLETON-LOCK-2026-10-05.md](APP-SINGLETON-LOCK-2026-10-05.md)。本文 §2 的 `game/sidlock` 包、“`Service.Init` 第一步获取”、§2.5 的 `Held()` 检查、§2.6 的可选项 F 都被它取代；§6 的实施拆分改用该方案 §9 的合并拆分。
> 5. **（追加）activity 不再持有自己的全局租约**：维护者要求“走 App 级别，不需要各个模块单独处理”。activity 的租约只用来算协调器该等哪些 game 服（`LiveGames`），改为调用 App 单实例锁暴露的只读查询 `Live`；`AcquireLease` / `RenewLease` / `ReleaseLease`、`incarnation`、standby/retake 与 `activity_lease_test` 删除。本文 §2.4 末尾“activity 以 `leaseStandby` 启动”及其回归作废，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §3.6、§7.2。
> 6. D-A（窗口耗尽即 fail-stop）、D-B（默认只对带 dataengine 的服务启用）按推荐决定。

## 0. 结论先行

1. 维护者给的两条事实：**玩家建角时由 account 绑定到一个 game server（`Role.ServerID`），之后不迁移；一个 sid 正常只有一个进程，只有崩溃重启时可能短暂并存两个。** 这样一来，“同一玩家同一时刻至多一个写者”就等价于“同一 sid 同一时刻至多一个写者”。现在的按玩家 Redis 租约、续租、跨间断撤离、归还、重新认领，解决的是“玩家在进程间漂移”这个不存在的问题。今天连修的 7 条 RR，以及状态机文档里新推出的 8 个违例场景，全都发生在这套机制内部。
2. 简化后只剩三件事：
   - **一把按 sid 的进程锁**：一个 Redis 键，值是本进程实例的随机 token，原子 CAS 设置和续期。只有一个 goroutine 负责它，不做任何别的工作。拿不到就等，丢了就自我围栏、进程退出。
   - **一张本地驻留表**：本进程为哪些玩家服务过、最近一次使用是什么时候。只用于准入判断和闲置卸载，不碰 Redis。
   - **按 `server_id` 的静态判定与路由**：登录看 account 校验会话时返回的角色 `ServerID`；赠礼步骤看命令里携带的发送方 sid；matchmaker 只看本地驻留表。
3. 不变量：状态机文档的 I1～I11 中，I1、I2、I3 改写到 sid 锁上；I4、I5 由构造自动成立（续期 goroutine 只续一个键；锁一丢进程就退出，内存副本不会跨过间断）；I7、I8（按玩家）删除；I6、I9、I10、I11 保留在闲置卸载上。状态机文档的假设 A3 不再是正确性假设，A1、A2、A4 仍然需要，另外新增一条 A5（§1.3）。
4. 规模：`playerowner.go` 约 1400 行降到 300 行左右；`playerroute` 整包删除，换成约 250 行的 `game/sidlock`；现有回归（45 个顶层用例 + 13 个子测试）里，按“前提消失”删除 15 个顶层用例，其余 30 个转写或保留，承诺不降低（§4.4）。不改 core 或 kit 的公开接口，`ownerroute` 原样复用。约 **2～3 个 agent 日，分 4 笔提交**（§6）。
5. 需要维护者拍板的有 5 点（§7）。最影响行为的两点：sid 锁 TTL 取 15s，因为它决定崩溃重启要停服多久；窗口耗尽但 Redis 没有答复时，先停止准入、等一个确定的答复，而不是立即退出。

---

## 1. 新前提与需要保证的性质

### 1.1 前提（维护者 2026-10-05）

- 角色的 `server_id` 在建角时由 account 写入（`kit/service/account/types.go:260` `Role.ServerID`，`create_role.go:139`），之后没有任何接口能改它（`UpdateProfile` 明确不能改 server，`service.go:325` 注释）。
- 一个 sid 只部署一个 game 进程。只有在崩溃重启时，旧进程可能还没完全死（卡住、SIGSTOP、网络分区、长 GC），新进程就已经拉起来了，两者短暂并存。
- 重启通常在同一台主机、同一个 WAL 目录上（见 §2.6）；也可能换主机（容器重调度）。

### 1.2 性质 P1～P5

| # | 性质 | 由谁保证 |
| --- | --- | --- |
| **P1** | 本进程只为 `server_id == 本进程 sid` 的玩家服务：只装载这些玩家，只为这些玩家准入写 | 登录时校验（§3.1）；后台步骤校验命令携带的 sid（§3.3）；`Admit` 要求本地驻留表里有这个玩家的记录，而记录只在通过校验之后才建立（§4.2） |
| **P2** | 同一 sid 任一时刻至多一个进程在写，崩溃重启的短暂并存也一样 | sid 进程锁（§2）：新进程只有在 Redis 里旧值消失之后才能拿到锁；旧进程的本地准入窗口在键过期之前至少一个 `AdmissionGuard` 就已经关闭（§2.4 的时间论证） |
| **P3** | 旧进程一旦不能证明自己还持有 sid 锁，就立即停止准入；一旦被确定地告知锁已不是自己的，就 fail-stop（Nest 围栏、关连接、进程退出）。新进程在旧锁确定失效之前不开始服务 | §2.4、§2.5 |
| **P4** | 跨服交互按 `server_id` 路由到目标 sid 的进程：赠礼的 debit / refund 交给发送方所在的 sid；matchmaker 只匹配本 sid 的玩家 | §3.3、§3.4 |
| **P5** | 闲置卸载只是本地内存管理：不碰 Redis，不交出任何东西；卸载之后在本进程重新装载，等待的是 DataEngine 已有的冷加载投影屏障 | §4.3 |

### 1.3 对照状态机文档的不变量 I1～I11

| # | 原不变量（摘要） | 新前提下 | 说明 |
| --- | --- | --- | --- |
| I1 | `Admit` 放行 ⇒ Redis 键是本进程这一次的租约，并且 `now < asked + Lease − guard` | **由 sid 锁保证（改写）** | 改成：`Admit` 放行 ⇒ sid 锁的键在一次确认过的 CAS（发出时刻记为 `asked`）里仍然是本实例的值，并且 `now < asked + TTL − AdmissionGuard`。对象从“每个玩家一把”变成“每个进程一把” |
| I2 | 同一玩家任何时刻至多一个进程的副本可服务 | **P1 + P2 推出** | 不同 sid 之间：静态绑定保证玩家只会出现在自己的 sid 上（P1）。同一 sid 之内：sid 锁保证（P2） |
| I3 | 告诉调用方 `mine=true` ⇒ 共享表认本进程，并且本地放行 | **改写，基本自动成立** | 没有共享表了。改成：`Serve` / `AdmitBound` 返回成功 ⇒ 绑定的 sid 等于本进程 sid，并且此刻 sid 锁放行 |
| I4 | 刷新回合有界；刷新循环不等自己 | **由构造自动成立** | 续期 goroutine 只对一个键做一次 CAS，不撤离、不归还、不重新认领，所以不需要回合预算。闲置卸载在另一个 goroutine 上（§4.3） |
| I5 | 跨过间断的副本在扔掉之前不被放行 | **由构造自动成立（fail-stop）** | 单个实例在持锁期间，键里一直是它自己的 token，这个 token 只在启动获取时设一次，所以续期答 Applied 本身就证明了连续持有。一旦被告知不是自己的（NotHeld），进程退出，内存副本随进程消亡。“按玩家的间断”不再存在，`interrupted` / `continuous` / `abandon` 都不需要 |
| I6 | 连接与副本同生命周期 | **保留在闲置卸载上；跨间断的部分删除** | 只卸载没有连接的玩家；登录如果撞上正在进行的卸载，就等它结束再装载。fail-stop 时连接与副本一起结束 |
| I7 | 归还只释放自己标记的那一次；归还进行中不确认认领 | **删除** | 没有按玩家的 Redis 释放。剩下的本地部分（卸载进行中不准入、登录等卸载结束）并入 I10 |
| I8 | 交出租约之前，本进程的写入已经落库 | **按玩家的部分删除；sid 级的部分变成假设 A5** | 卸载不交出任何东西；本进程重新装载走 `entity_repository.go:178` 的 `WaitEntityProjection`（roost-coding “冷卸载重载必须等待该 Entity 的在途投影”）。崩溃重启的情形见 A5 |
| I9 | 续租不算使用，认领和准入算使用 | **保留** | sid 续期不碰任何玩家的 `lastUsed`；`Serve`、`AdmitBound`、`Admit` 写 `lastUsed` |
| I10 | 撤离不可取消；每个玩家至多一个撤离；调用方等待有界 | **保留** | 卸载的单航班、`evictWait` 都原样保留 |
| I11 | `mu` 不跨等待；快池上不阻塞，`Admit` 只读本地状态 | **保留，并且更简单** | `Admit` 读 sid 锁的原子状态，再在 `mu` 内读一条记录。后台准入（`AdmitBound`）也不再有 Redis 往返 |

**假设**（对照状态机文档 §3 的 A1～A4）：

- **A1** Redis 不提前丢键：仍然需要，但范围从每个玩家一个键缩小到每个进程一个键。键提前丢失之后，下一次续期会被告知 NotHeld，进程 fail-stop，这是安全的方向。只有“丢键”和“崩溃重启的并存”同时发生时，才可能有两个写者，直到旧进程下一次续期（≤ `RenewInterval`）。要彻底关掉，见 §2.6 的可选项 F。
- **A2** 被准入的事务在 `AdmissionGuard`（5s）内结束：仍然需要。注意崩溃重启的场景恰好就是“旧进程卡住”，事务被暂停在准入和提交之间的情形比原来更现实，见 §2.6。
- **A3** 后台两次准入的间隔不超过闲置阈值：**不再是正确性假设**。违反它的后果只是一次多余的卸载和冷加载，冷加载会等投影。
- **A4** 本地单调时钟与 Redis 时钟的速率差可以忽略：仍然需要。
- **A5（新增）** 崩溃重启时，旧进程在锁内已经提交、但还没投影的 WAL 记录，要么由接手的进程先重放（同一个 WAL 目录；同主机时 WAL 的 `flock` 保证旧进程已经死了），要么在新进程装载对应玩家之前已经落库。跨主机并且 WAL 不随 sid 迁移时，这条没有保证。现状的按玩家租约在“租约过期”这个分支上有完全相同的缺口（状态机文档 I8 的“围栏不交出租约”），所以本方案没有降低保证。

---

## 2. sid 进程锁

> **已被取代（2026-10-05）**：本节的锁改由 core `app` 统一提供（[App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md)），在任何 Mod `Init` 之前获取，而不是放在 `Service.Init` 第一步；不新增 `game/sidlock` 包，业务不检查 `Held()`，失锁由 App 统一围栏 Nest 并 fail-stop。本节的后端比较（§2.1，结论仍是 Redis 单键）、键值与 CAS 语义（§2.2）、时间参数（§2.3）、单写者状态机与“窗口从 `asked` 起算”（§2.4）被该方案沿用；§2.4 的放置位置、§2.5 的四个后台循环检查、§2.6 的“锁获取的位置”与可选项 F 不再适用。§2.4 末尾“activity 以 standby 启动”也作废：activity 不再持有全局租约，改用 App 锁的 `Live` 查询（该方案 §7.2）。

### 2.1 放在哪里：三个后端的比较

| | Redis 单键（推荐） | 复用 activity 的全局租约（`svcglobal.Routing.AcquireLease` / `RenewLease`） | etcd lease + txn |
| --- | --- | --- | --- |
| 是否已经是依赖 | 是。game 进程今天就靠它（`game_route.key_prefix`、`mods.Redis`） | 是，但它是另一个服务，要跨 bus | 否。demo 没有部署 etcd |
| 原子性与结论 | `fredis.CompareAndSet`（`redis/cas.go:46`）一次往返。`Expected=nil` 表示“键必须不存在”；失败时返回 `Current` | `Leases.Update` 里 CAS；拒绝时只回错误码，不暴露当前持有者（`service.go` `RenewLease` 的注释） | 线性一致，lease 到期由服务端判定 |
| TTL 精度与时钟 | 毫秒（PSETEX），用 Redis 服务端时钟 | **秒**：`ExpiresAtUnix = now.Add(TTL).Unix()` 截断（`kit/service/global/service.go:298`），用 global 服务的时钟 | 秒级 TTL |
| 故障耦合 | 只依赖 Redis | 把每个 game 的写能力绑在 global 服务和 bus 上；路由换绑（`RouteEpoch`）会让续期答 NotHolder，进而触发 fail-stop | 新增一套基础设施 |
| 语义是否吻合 | 吻合：拿不到就等，丢了就退出 | **不吻合**：activity 的 `leaseStandby` 是“失去租约也继续跑、每拍重试”（`ac:521-552`，RR-20260930-24），这是活性语义，不是围栏语义 | 吻合 |
| 能否关掉 A1 | 不能（failover 时异步复制可能丢写） | 取决于 global 的存储 | 能 |

**选 Redis 单键。** 理由：
1. 已经是 game 进程的硬依赖，不增加故障面。
2. core 已经有一次往返的 CAS / CAD，并且失败时带回 `Current`，正好用来在启动等待时识别旧持有者。
3. 毫秒 TTL 和本地窗口的推导（`asked + TTL`）与 RR-20261004-14 修好的那一套完全同形。
4. activity 租约的语义是活性（谁还活着、预期集合），不是围栏。把两者合并，就要么改 activity 的 standby 语义，要么让围栏依赖一个秒级、跨服务的租约。

activity 的全局租约照旧保留，继续服务于协调器的预期集合，只改一处启动行为（§2.4 末尾）。etcd 作为 A1 真正成为问题时的升级路径，不在本次范围内。

### 2.2 键、值与操作

- 键：`<game_route.key_prefix>:sid:<sid>`。沿用已有配置 `game_route.key_prefix`，不新增配置项。与旧的按玩家键 `<prefix>:owner:<playerID>`（`playerroute.go.tmpl` `Store.key`）不冲突。
- 值：`<token>|<hostname>|<pid>|<started_unix_ms>`。`token` 是本实例启动时用 `crypto/rand` 生成的 16 位十六进制随机串，沿用 `playerroute.newToken` 的做法：同 sid 重启就是另一个持有者（原来的 `TestARestartOnTheSameSidIsADifferentOwner`）。后三段只是给运维看的诊断信息；CAS 比较整个值，所以它们不影响语义。
- 操作（每个都是一次原子往返）：

| 操作 | 实现 | 结论 |
| --- | --- | --- |
| `Acquire` | `CompareAndSet{Expected:nil, Next:v, TTL}` | `Applied` → 拿到；否则 `Current` 就是当前持有者的值 |
| `Renew` | `CompareAndSet{Expected:v, Next:v, TTL}` | `Applied` → **Held**；`!Applied` → **NotHeld(Current)**；报错 → **Unknown** |
| `Release` | `CompareAndDelete(v)` | 只在优雅停机时调用 |

`Applied=false` 一律是 NotHeld，从结构上就不存在“没生效却回答是我们的”（状态机文档 W08-3 的根因在这里不存在）。

### 2.3 时间参数

| 参数 | 推荐值 | 关系 / 理由 |
| --- | --- | --- |
| `TTL` | **15s**（D2） | 决定崩溃重启的停服时长：新进程最多等一个 TTL。也决定能容忍多长的 Redis 抖动（见下） |
| `RenewInterval` | 3s | `RenewInterval + renewTimeout ≤ TTL − AdmissionGuard`（3 + 3 ≤ 10）：一次续期超时之后，下一次续期仍然落在窗口内 |
| `renewTimeout` | 3s（= `RenewInterval`） | 单次 CAS 的 ctx 上限，保证续期 goroutine 不会被一次挂住的调用卡死 |
| `AdmissionGuard` | 5s（不变） | 留给在途事务（A2）。和现在的 `po:330` 是同一个常量 |
| 准入窗口 | `asked + TTL − AdmissionGuard` = 最后一次确认的续期**发出**之后 10s | 连续丢两次续期仍然不中断服务，第三次成功就能续上 |
| 启动等待上限 | `2 × TTL` = 30s | ≥ `TTL + RenewInterval`。如果旧持有者已经卡住，它的键一定在一个 TTL 内过期；超过上限还拿不到，说明对方仍在续期，属于部署错误（§2.4） |
| 失联宽限 | 窗口结束后再等 `TTL`（D3） | 在此期间停止准入、继续续期，等 Redis 给出确定的答复；宽限用完还是 Unknown，就 fail-stop |

由测试钉住的常量关系（转写自 `TestTheHandBackPassBudgetFitsInsideTheLease`）：`RenewInterval + renewTimeout ≤ TTL − AdmissionGuard`；启动等待上限 ≥ `TTL + RenewInterval`。

如果维护者希望和现在的数字一致（30 / 10 / 5），关系同样成立，代价是崩溃重启最多停服 30s（D2）。

### 2.4 状态机（只有一个写者 goroutine）

```text
          Acquire 成功                 Renew: Held（确认）
Starting ───────────────▶ Held ◀────────────────────────────┐
   │  等待：Current≠nil、未到上限                │            │
   │  上限到了仍被持有 → Init 失败              │ 窗口耗尽  │ Renew: Held
   ▼                                           ▼（只看时间）│
 (退出)                                    Unconfirmed ─────┘
                                               │ Renew: NotHeld，或宽限用完
                     Held ── Renew: NotHeld ──▶ Lost（吸收态）→ fail-stop
```

- **单写者**：获取、续期以及它们的结论，都在同一个 goroutine 里顺序执行：发出 CAS、等它返回、应用结论，然后才发下一次。所以不存在“旧回复作用在新状态上”这种交错，也就不需要世代号（状态机文档 W08-2 那一类问题，根因是多个 goroutine 按玩家 id 插入或更新，这里不存在）。`Lost` 是吸收态：之后到达的任何结论都不会把它改回去。
- **本地窗口从请求发出时刻起算**（RR-20261004-14 的教训）：每次 CAS 之前先记下 `asked := now()`；Applied 之后 `validUntil = max(validUntil, asked + TTL)`。Redis 的 TTL 从它处理请求的那一刻起算，不早于 `asked`，所以本地窗口不会晚于键过期。
- **`Admitted()`**：`state ∈ {Held} ∧ now < validUntil − AdmissionGuard`。`validUntil` 和 `state` 用原子量保存，`Admit` 不加锁、不阻塞（I11）。`Unconfirmed` 只表示“窗口已经过了、还没得到确定答复”，这时 `Admitted()` 已经按时间返回 false。
- **启动获取（P3 的后半句）**（**已被取代**：改由 App 在任何 Mod `Init` 之前获取，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §3.5；下面的循环步骤被沿用）：放在 `Service.Init` 的**第一步**，在 `EnsureWorld`（`sv:69`）之前，因为 World 也是按 sid 的实体（`WorldUniqueID = sid`，GAME_DEMO_TEMPLATE §9.11.4）。循环如下：
  1. `Acquire`。Applied 就进入 Held，启动续期 goroutine，继续 Init。
  2. 没生效，并且 `Current` 等于自己的值：说明上一次 `Acquire` 已经落地、只是回复丢了（RR-20261004-14 的“丢回复”在锁上的形态）。这个值只有本实例设过，而且还没有开始服务，可以直接认领。认领之后立即 `Renew` 一次，用这次的 `asked` 起算窗口。
  3. 没生效，并且 `Current` 是别人的值：记一条日志（只在持有者的值变化时记，带上值里的 hostname / pid），等 `min(RenewInterval, 键的剩余 TTL)` 后重试。
  4. 超过启动等待上限仍被别人持有：`Init` 返回错误 `sid %d is held by a live process (%s)`，进程以非零码退出，由外部重启。
- **为什么推荐“等”而不是“立即失败、由外部重启”（D1）**：崩溃重启时旧进程多半已经死了或卡住了，它的键最多一个 TTL 就会过期。等待的停服时间就是剩余的 TTL；如果立即失败，k8s 的 `CrashLoopBackOff` 会把重启间隔一路退避到几分钟，停服时间反而更长。只有对方一直在续期（真的有两个健康进程配了同一个 sid）才会等到上限，那时失败退出是对的：这是部署错误，不能抢锁。
- **P2 的时间论证**：新进程只有在 Redis 里旧值消失之后才能 `Acquire`。旧值消失的时刻不早于旧进程最后一次确认续期的 Redis 处理时刻加 TTL，也就不早于 `asked_old + TTL`。旧进程的准入在 `asked_old + TTL − AdmissionGuard` 就已经停止。所以在 A4 下，新进程开始服务时，旧进程已经至少有 `AdmissionGuard` 不再准入新事务，剩下的只有 A2 和 A5。
- **activity 的配合**（**已作废**（2026-10-05 维护者追加决定）：activity 删除自己的全局租约，改用 App 锁的 `Live` 查询，下面的 standby 改法不做，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §7.2）（RR-20260930-24 的形状）：新进程拿到 sid 锁之后，旧进程的 activity 全局租约可能还没过期。它每 5s 续一次（`ac:41`），TTL 是 30s，所以最多还活 30s。现在 `bindAndLease`（`ac:189-212`）遇到 `held` 冲突会让 `Init` 失败。要改成：冲突时以 `leaseStandby` 启动，`renewLease` 已经会在每次心跳时重试 `AcquireLease`（`ac:541-551`）。这是本方案里唯一一处 activity 改动，现有 `activity_lease_test` 的承诺不变，再加一条“启动遇到活租约就以 standby 开始”的回归。

### 2.5 失锁时的自我围栏

> **已被取代（2026-10-05）**：失锁动作由 App 统一执行（`RuntimeFailure.Fail` → 登记的回调围栏 Nest → `Service.Shutdown` 关闭连接），不在业务里实现；下表“窗口耗尽先停准入”与四个后台循环检查 `Held()` 都删除，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §3.4、§5、§7.1。

| 触发 | 动作 | 进程是否退出 |
| --- | --- | --- |
| 窗口耗尽（续期一直 Unknown） | `Admitted()` 按时间变为 false：WriteGate、`Serve`、`AdmitBound` 全部拒绝；按 sid 运行的后台循环（下表）在每轮开头检查 `Held()`，跳过这一轮。**连接保留**，客户端收到的是可重试的拒绝 | 否。继续续期：答 Held（键里仍是本实例的 token，连续性得到证明）就回到 Held，恢复服务；答 NotHeld 或宽限用完，就转入下一行 |
| `Renew` 答 NotHeld（键没了，或者是别人的值），或失联宽限用完 | ① 在同一个临界区里进入 `Lost`（吸收态）；② `NestMgr.Fence(errSIDLockLost)`：立即拒绝所有新的和排队中的 Nest 派发，这是进程级围栏，覆盖 World、玩家和所有后台事务（`nest/nest.go:227`，与 DataEngine 的 `onFatal` 同一条路，`kit/dataengine/mod.go:412-431`）；③ 对驻留表里每个玩家调用 `CloseSessions(playerID, errSIDLockLost)`；④ `app.RuntimeFailure.Fail(err)`，走 App 现成的 fail-stop 停机路径（`app/app.go:293-305`） | **是**，非零退出，由外部重启 |
| 优雅停机 | 在 `Service.Shutdown` 最后，所有消费者已停、所有连接已关之后，`Release` | 正常退出。下一次启动不用等 TTL |

需要在每轮开头检查 `Held()` 的后台循环（它们不经过 WriteGate）：`tickWorld`（`ac:216`）、`runActivity`（`ac:235`）、`startSpawner`、`formMatches`（`mm:131`）。gift 的步骤消费者走 `AdmitBound`，所以不用另加检查。

**为什么 NotHeld 要退出、不尝试重新获取**：键没了或者换了值，说明这期间可能有别的实例持有过、写过（例如新进程起来、服务了几秒、又优雅停机释放了键）。本进程内存里的每一个驻留副本都无法证明仍然和权威一致。逐个撤离、再逐个重载，正是现状 `retakeLost` / `fence` / `abandon` 那一整套交错的来源。整个进程退出、由外部重启，等价于一次完整的“扔掉全部副本”，而且不可能出错。代价是：Redis 一旦丢键（failover），所有 game 进程都会重启一次。

### 2.6 与 DataEngine 写入的关系

- **窗口外不再提交**：围栏之后 Nest 拒绝一切新派发；窗口耗尽时 `Admit` 拒绝。真正的漏洞只剩 A2：事务在准入之后、提交之前被暂停（这正是“旧进程卡住”的形态），恢复之后照样提交到 WAL。
- **同主机重启已经天然互斥**：WAL 目录按 sid 划分（`kit/dataengine/mod.go:110`，`data/wal/dataengine/<sid>`），`nestwal.Open` 用 `flock(LOCK_EX|LOCK_NB)` 锁 `writer.lock`（`nestwal/wal.go:222-228`）。旧进程只要还活着（哪怕被 SIGSTOP），新进程在 DataEngine Mod 的 `Start`（`dataengine/engine/assembly.go:119` 打开 WAL；Mod 的 `Init` 只解析配置）就会失败（`ErrLocked`）并退出。旧进程死后锁才释放，新进程启动时先**重放**旧 WAL，再做任何装载。所以同主机的并存不会产生两个 DataEngine 写者，A5 自动成立。sid 锁真正要防的，是**跨主机**并存（容器换节点重调度、节点分区），以及 DataEngine 以外的副作用：bus 寻址 `<prefix>.svc.<type>.<sid>` 上两个进程同时订阅、按 sid 的 World 定时器、activity、matchmaker 和 gift 消费者。
- **锁获取的位置**（**已被取代**：App 锁在所有 Mod 之前获取，本条描述的缺口不再存在，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §2、§4）：Mod 先于 `Service.Init` 启动，所以 DataEngine 重放本机 WAL、outbox worker（owner 默认是 `dataengine-<sid>`，`kit/dataengine/mod.go:182`）都在获取 sid 锁之前就开始了。同主机时由 `flock` 兜底；跨主机时，本机 WAL 是本机上次运行留下的，重放它与远端旧进程是否还活着无关。v1 把获取放在 `Service.Init` 第一步就够了。如果要把这一段也盖住，需要把 sid 锁做成一个 kit Mod，排在 DataEngine 之前（要改 codegen 的 mod catalog 和停机预算），列为后续（§7 D5 的备注）。
- **DataEngine 层 fencing token（可选项 F，不推荐本次做）**（**维护者 2026-10-05 决定不做**：只覆盖崩溃重启的短暂并存，由 App 锁 + 同目录 WAL `flock` 保证，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §4）：core 已经有现成的机制 `dataengine.LeaseFence`（`dataengine/lease_fence.go`）：WAL 记录携带一个 Mongo 协调文档的 `owner` / `token` 前提，投影时在同一个 Mongo 事务里校验并写确认，前提不成立就把整条记录当成幂等空操作。如果把 sid 锁的 `(sid, epoch)` 写进一个 Mongo 文档（获取时 epoch 加一），让每条玩家 / World 记录都带上它，就能关掉 A1、A2 和跨主机的 A5。代价是：每个投影事务多一次条件写；被围栏的记录变成“已确认但没生效”（丢的是已经确认的写入，而不是 fatal 冲突）。这需要先定义“已确认写入被围栏后怎么告知客户端”，属于跨进程移交那条 feature 线（状态机文档 §8 D3、CARRYOVER A6 / C24），在这里登记，不并入本方案。

---

## 3. 登录与路由

### 3.1 登录：`server_id` 从哪来、在哪里校验

- **来源**：TCP 握手的认证器已经调用 account 的 `ValidateSession`（`au:68-87`），返回的 `Role` 里就有 `ServerID`，这是 account 存储里的权威值，不是客户端声称的值。会话 token 本身（`security.SignSessionToken(playerID, …)`）不含 sid，**也不需要加**：sid 每次都随角色查出来，比塞进票据更可靠，因为票据里的 sid 只是签发那一刻的快照。`SelectRole` 返回给客户端的 `Session.ServerID` 告诉客户端该连哪个服。
- **改动**：
  1. `au:87` 改成 `newPrincipal(role.PlayerID, role.ServerID)`，把 sid 写进 `Principal.Claims["server_id"]`。`clonePrincipal` 会复制 Claims（`render_player_tcp.go:984`），所以 handler 能通过 `context.Session.Principal()` 读到它。认证器只负责记录，不负责拒绝，因为握手失败只能断开连接，客户端得不到原因。
  2. `HandleEnterGame`（`eg:41-75`）里，`owners.Claim` 换成 `owners.Serve(loginCtx, playerID, boundSID)`：
     - Claims 里没有 `server_id`（认证器被替换过）→ fail-closed，返回 `ErrPlayerElsewhere`，`owner_sid=0`，并记 Error 日志。
     - `boundSID != SID()` → 返回 `ErrPlayerElsewhere`（100015），`owner_sid=boundSID`。实体不装载，也不建驻留记录。
     - sid 锁不放行（启动中，或者窗口耗尽）→ 返回 `ErrLoginTimeout`（100021，客户端重试），日志里写明原因是 `sid lock not held`。
     - 否则：等这个玩家正在进行的卸载结束（受 `loginCtx` 约束，超时同样回 `login_timeout`），建立或刷新驻留记录，然后 `GetOrCreate`。
- **非登录消息**：WriteGate 的 `AdmitMessage` 只看 sid 锁和驻留记录（§4.2）。一个会话如果 EnterGame 被拒绝，它的玩家在本进程就没有驻留记录，后续消息全部被 WriteGate 拒绝。所以“实体是否本服”由“只有通过绑定校验才会建立记录”来保证，WriteGate 本身不需要知道 sid，生成的 `WriteGate` 接口（`render_access.go:379-381`）也不用改。

### 3.2 错误码与客户端处理

- **复用 `ErrPlayerElsewhere`（100015），不新增错误码。** 含义从“玩家驻留在另一个进程（等租约过期或改连持有者）”收窄为“玩家绑定在另一个服”。描述文本要改（`player_elsewhere.go.tmpl` 的注释与 message），并且明确告诉客户端：**不要在本服重试**，应当改连 `owner_sid` 对应的网关；如果 `owner_sid=0`，重新走 `SelectRole`，按返回的 `Session.ServerID` 连接。
- 新前提下，这个错误只会由客户端连错服、或者网关路由配置错误引起，不再是正常运行中的瞬时状态。

### 3.3 赠礼：debit / refund 交给发送方所在的 sid

- **数据来源，选最简单可靠的一种：命令里携带。** `gift.State` 增加 `FromSID int32`（json `from_sid`）。赠礼由发送方在线时、在发送方会话所在的进程上发起，而这个进程按 P1 必然就是发送方绑定的 sid。所以发起时把本进程 sid 写进去即可：`handlerStartGift` 增加参数 `fromSID int32`，controller 传入 `owners.SID()`。这个 handler 带 `//roost:nest`，加参数之后要重新生成 sender（`roost` 的 sender 生成，同属这一笔提交）。
- 为什么不选其他来源：
  - 查 account：没有“按 playerID 查角色”的公开 RPC（`Accounts` 接口只有 `ValidateSession` 需要 token，`account_rpc.go:42-68`），而且每次准入都要跨 bus。
  - 在 Player DAO 里存 sid：老文档没有这个字段，需要回填；每次准入还要读一次 Mongo。
- **准入**（`admitPhase`，`gs:378-412`）：`owners.Owns(ctx, From)` 换成 `owners.AdmitBound(From, state.FromSID)`：
  - `FromSID == SID()`，并且 sid 锁放行、玩家没有在卸载：建立或刷新驻留记录，放行。
  - `FromSID != SID()`：不属于本服，`handOver` 转交给 `FromSID`，然后拒绝，消息 nak。与现在一样，转交是优化，重新投递才是兜底。
  - 本服、但 sid 锁不放行：拒绝，消息 nak，不转交。
- **转交路由**：保留 core 的 `ownerroute.Router`，只把键从玩家 id 换成 sid：`Router[giftStepHandoff, int32, sidRoute]`，`KeyOf` 返回 `cmd.FromSID`，`Routes` 换成一个静态解析器：`GetRoute(ctx, sid) = (sidRoute{sid}, sid > 0, nil)`。`giftStepHandoff` 增加 `FromSID`。core 的 `ownerroute` 不用改。
- **接收方**（`runHandoff`，`gs:276-296`）：`OwnedHere` 换成 `AdmitBound(cmd.PlayerID, cmd.FromSID)`。不是本服的就静默丢弃（语义不变）。
- **旧 payload**（没有 `from_sid`，`FromSID==0`）：refund 必须执行，不能简单拒绝。推荐在**切换前排空进行中的赠礼 saga**（停止发起新赠礼，等待 `gift.Deadline` 以及补偿全部结束）。兜底规则：`FromSID==0` 时，如果发送方在本进程有驻留记录就当作本服处理，否则拒绝、不转交。这条兜底只给排空期之外的遗留消息用，日志里带 `legacy_gift_payload`（D4）。

### 3.4 matchmaker

- `ownedCandidates`（`mm:109-127`）里的 `owners.OwnedHere` 换成 `owners.Resident(ticket.Subject.ID)`：只读判断，条件是有驻留记录、没有在卸载、sid 锁放行。不需要任何新数据：票只能由 `JoinQueue` 写入，而 `JoinQueue` 要经过 WriteGate，所以玩家一定在其绑定的 sid 上有驻留记录。别的 sid 的票在这里看不见，也就不会被匹配。
- 行为与现在相同：别人的票留在队列里，由它的 sid 去匹配；跨服匹配仍然不支持（GAME_DEMO_TEMPLATE §9.11.4）。玩家下线并被卸载之后，票留到 TTL 过期。原来的租约到期之后也是这样。
- 后续可选：静态绑定之后，持有者不会再变，所以“按 sid 分区的队列 / 消费者”（GAME_DEMO_TEMPLATE §9.11.6、§9.12.5 写的“持有者会变，需要先定接管契约”）的前提消失了。可以把 `Queue.Partition` 设成 sid，消掉共享窗口里的拥挤；代价是 `match.sweep_queues` 要按 sid 逐条列出。不在本方案范围内。

---

## 4. 删除 / 保留清单

### 4.1 包与文件

| 现状 | 去留 |
| --- | --- |
| `demo/game/playerroute/`（`playerroute.go` 322 行、测试 439 行） | **整包删除**，新增 `demo/game/sidlock/`：`Store`（Acquire / Renew / Release，基于 `fredis.CompareAndSet` / `CompareAndDelete`，保留 `keyspace` 这类窄接口以便测试）和 `Lock`（§2.4 的状态机、续期循环、`Admitted()` / `Held()`、`OnLost` 回调）。`Route`、`GetRoute`、`Owner`、`Owns`、`Refresh`、`decodeRoute` 全部删除 |
| `playerowner.go`（1411 行） | 改写成约 300 行：驻留表、`Admit` / `AdmitMessage` / `Serve` / `AdmitBound` / `Resident`、闲置卸载、`playerEvictor`（原样保留） |
| `playerowner_test.go`（35 个顶层用例 + 13 个子测试） | 按 §4.4 迁移 |
| codegen `demo.go:602-605` 的模板清单、`render_dev_run.go:213` 的注释 | 随之更新（文件名、why 文案） |

### 4.2 `PlayerOwners` 字段与机制逐项去留

| 现有 | 去留 | 理由 |
| --- | --- | --- |
| `store leaseTable`（按玩家的 Redis 表） | **删** | P1 + P2 |
| `local map[int64]*leaseState` | **改**：`map[int64]*resident{lastUsed}` | 只是“本进程验证过、为其服务过的玩家”加上最近使用时间 |
| `leaseState.validUntil` | **删**（移到 sid 锁，只有一份） | I1 改写 |
| `leaseState.lost` | **删** | 状态机文档 §2.1 已经说明它没用 |
| `leaseState.interrupted`、`markUnansweredClaim`、`confirmRenewal` 遇上 interrupted 改为重新认领 | **删** | I5 由 fail-stop 保证；“丢回复”只在 sid 锁的启动获取里出现，由 §2.4 第 2 步处理 |
| `continuous` 判断、跨间断撤离、`abandon`、`errStaleCopyKept` | **删** | 同一实例持锁期间没有间断 |
| `handingBack` / `handBackDone`、`finishHandBack`、`stillOurs` | **删** | 没有交还；卸载用单航班的 `dropping` 表达 |
| `claiming` / `enterClaim` / `leaveClaim` | **删** | 认领和归还的互斥（RR-20260921-03）只是为了不在 Redis 里释放别人刚认领的键。本地剩下的部分，“登录等卸载结束”，由 `dropping` 的 `done` 承担 |
| `evictions`（单航班）、`dropResident`、`runEviction`、`evictWait` | **保留**，改名为 `dropping` | I10 |
| `refreshLoop` / `renew` / `refreshPass` / `retakeLost` / `retakeOne` / `handBackIdle` / `handBackOne` | **删**。换成 sid 锁的续期 goroutine（只续期）加上一个独立的卸载 goroutine | I4 由构造保证 |
| `handBackPassBudget` / `handBackBudget` / `projectionWait` / `projections` / `Projections()` | **删** | 没有交还，也就不需要等投影；本进程重载由 DataEngine 冷加载屏障保证 |
| `fence`、`Fence(sessionFencer)` | **改**：`Fence` 设置器保留（卸载要用 `ActiveSessions`，失锁要用 `CloseSessions`）；`fence()` 合并到 §2.5 的失锁动作 | — |
| 公开 `Release` | **删** | 没有调用方（状态机文档 §1.1）；它也是 W08-2b 的入口 |
| `ErrLeaseNotHeld` | **保留**，文案改为 “this process does not hold its sid lock, or does not serve the player” | WriteGate 的拒绝文本 |
| `AdmissionGuard` | **保留**（移到 `sidlock`） | §2.3 |
| `HandBackIdle` | **改名为 `IdleUnload`**，推荐 5 分钟 | 只关系到内存和重载开销，不再关系到“别的进程多久能接手” |

闲置卸载保留的最小形状：
- 一个 goroutine，每 30s 扫一次驻留表。选出 `ActiveSessions == 0`、`now − lastUsed ≥ IdleUnload`、没有在卸载的玩家，在锁内标记 `dropping`，在锁外调用 `dropResident`（等待受 `evictWait` 约束）。
- 撤离结束时（不论成功还是失败）在锁内摘掉 `dropping`。成功就删掉驻留记录；失败说明副本还在、并且仍然有效（没有间断），记录原样保留，下一轮再试。
- `sid 锁 Lost` 之后卸载 goroutine 停止，进程本来也要退出了。

### 4.3 新判定与对外接口

| 接口 | 现状 | 新 |
| --- | --- | --- |
| `Admit(playerID) error`（快池，不阻塞） | 按玩家的租约窗口 + 撤离进行中拒绝 | `sid.Admitted() ∧ resident[pid] 存在 ∧ !dropping[pid]`，放行时写 `lastUsed` |
| `AdmitMessage(pid, msgID)` | 登录豁免，其余走 `Admit` | 不变 |
| `Claim(ctx, pid) (bool, error)`（登录） | Redis 认领 + 跨间断撤离 | **改为** `Serve(ctx, pid, boundSID) error`（§3.1） |
| `Owns(ctx, pid)`（gift 准入，可能认领） | Redis 读 + 认领 | **改为** `AdmitBound(pid, boundSID) (local bool, err error)`（§3.3），不阻塞、没有 Redis |
| `OwnedHere(ctx, pid)`（matchmaker、handoff 接收） | Redis 读 | **改为** `Resident(pid) bool`（matchmaker）；handoff 接收用 `AdmitBound` |
| `OwnerSID(ctx, pid)` | Redis 读 | **删**。登录用会话 Claims 里的 sid，gift 用 `FromSID` |
| `Routes()` | `*playerroute.Store` 作为解析器 | **删**。gift 用静态 sid 解析器（§3.3） |
| `SID()` | `store.SID()` | 保留，取配置里的 `sid` |
| 新增 `Held() bool` | — | 给按 sid 运行的后台循环用（§2.5） |
| `Start` / `Stop` / `Evict` / `Fence` | — | 保留。`Start` 启动卸载 goroutine；sid 锁由 `Service.Init` 第一步启动，在 `Service.Shutdown` 最后释放 |
| `controller.go.tmpl:97-100` 的 `playerOwners` 接口 | `Claim`、`OwnerSID` | 改为 `Serve`、`SID` |
| `gift_saga.go.tmpl:214-225` 的 `playerOwnership` | `Owns`、`OwnedHere`、`OwnerSID` | 改为 `AdmitBound`、`SID` |

`service.go.tmpl` 装配顺序：`Init` 第一步 `sidlock.Acquire`（等待）并启动续期，`OnLost` 接到 §2.5 的动作（`NestMgr` 用 `mods.ModNest` 查找，`RuntimeFailure` 用 `app.ModRuntimeFailure` 查找）；`NewPlayerOwners` 不再需要 Redis 和 `Projections`（`sv:123-155` 简化）；WriteGate 注册不变（`sv:158`）；`Shutdown` 在 `owners.Stop()` 之后、最后一步 `Release`。

### 4.4 回归的去留（现有 58 条 + 相关 RR）

**playerroute（10 条）**：
- 转写到 `sidlock.Store`（8 条）：`TestClaimGivesOnePlayerOneOwner` → 一个 sid 只有一个持有者；`TestLeaseLapsesWithoutRefreshAndRefreshOnlyExtendsOurOwn`；`TestReleaseOnlyFreesOurOwnClaim`；`TestNewStoreRefusesWhatItCannotAddress`；`TestAStaleReleaseMustNotDeleteTheNewOwner`；`TestAStaleRefreshMustNotExtendTheNewOwner`；`TestARestartOnTheSameSidIsADifferentOwner`；`TestRefreshReportsPerPlayerOutcomesIncludingUnknown` → Renew 的 Held / NotHeld / Unknown 三种结论。
- 删除（2 条）：`TestClaimByTheOwnerExtendsTheLease`（并入 Renew）、`TestGetRouteReportsAnUnownedPlayerAsNotFound`（没有路由表了）。

**playerowner（35 条，含 13 个子测试）**：

| 处理 | 用例 | 理由 / 新形态 |
| --- | --- | --- |
| **转写到 sid 锁** | `TestALostLeaseFencesThePlayer` | NotHeld → `Lost`：Nest 围栏、关连接、`OnLost` 恰好调用一次 |
| | `TestAnUnknownRenewalRunsAdmissionOutInsteadOfPretending` | Unknown → 窗口按时间耗尽，不提前、不延后 |
| | `TestAdmissionStopsOneGuardBandBeforeTheDeadline` | 原样转写 |
| | `TestASidMatchWithoutAConfirmedLeaseIsNotOwnership` | 键里是同 sid 的上一个实例的值 → 不算持有，启动时要等 |
| | `TestAdmissionStopsAGuardBeforeTheKeyRedisSetCanExpire`（4 个子测试，RR-20261004-14） | 获取和续期的窗口都从 `asked` 起算 |
| | `TestAClaimRedisDidNotAnswerIsNotConfirmedByTheNextRenewal`（3 个子测试，RR-20261004-14） | 启动获取丢了回复：重试时 `Current` 等于自己才认领，并用随后那次续期的 `asked` 起算；`Current` 是别人就等 |
| | `TestARetakenLeaseClosesTheSessionsThatLivedThroughTheGap`（RR-20260930-23） | 承诺“间断之后不留脱离场景的连接”改为：失锁时关闭全部驻留玩家的连接，然后进程退出 |
| | `TestTheHandBackPassBudgetFitsInsideTheLease` | §2.3 的常量关系 |
| | `TestOneBusyEntityDoesNotHoldUpTheRefreshLoop`（RR-20260920-12） | 一个忙实体的卸载不推迟 sid 续期（两者在不同的 goroutine 上） |
| **原样保留** | `TestTheWriteGateExemptsLoginAndRefusesEverythingElse` | 没有驻留记录或 sid 锁不放行时拒绝；登录豁免 |
| | `TestRenewalsDoNotCountAsUse` | sid 续期不写 `lastUsed`（I9） |
| **转写到闲置卸载** | `TestABackgroundClaimIsHandedBackWhenNobodyIsPlaying`（RR-20260920-10） | 后台准入建立的记录在闲置后被卸载 |
| | `TestAPlayerWithASessionIsNeverHandedBack` | 有连接就不卸载 |
| | `TestWorkInProgressKeepsTheLease` | `Admit` 刷新 `lastUsed`，所以不会被卸载 |
| | `TestALeaseRetakenForNewWorkIsNotIdle`（RR-20260920-11） | `Serve` / `AdmitBound` 算使用 |
| | `TestAHandBackThatCannotDropTheCopyKeepsTheLease`、`TestAFailedHandBackPutsTheLeaseBackInService` | 撤离失败 → 记录保留，`dropping` 摘掉之后继续准入 |
| | `TestAHandBackInFlightAdmitsNothing`、`TestAHandBackWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds`（2 个子测试，RR-20261004-11） | 卸载进行中 `Admit` 拒绝；登录加入正在进行的撤离，结束之后再装载 |
| | `TestAClaimWaitsAtMostTheBudgetForABusyEntity`、`TestASecondDropJoinsTheOneAlreadyRunning`（RR-20260920-12） | `Serve` 等待有界；撤离单航班 |
| | `TestAClaimDuringAHandBackNeverEndsWithAnUnownedPlayer`（4 个子测试，RR-20260921-03） | 收成一条：卸载进行中登录，结局一定可服务（记录存在、放行）。“Redis 里没人持有”这个结局不再可能出现 |
| **删除（前提消失）** | `TestALapsedLeaseNobodyElseTookIsRetakenInsteadOfFenced` | **行为变化**：sid 锁窗口耗尽之后，键如果已经消失，就不再重新获取，而是 fail-stop（§2.5 的理由）。用新回归“NotHeld(missing) 不重新获取”代替 |
| | `TestARetakeWhoseCopyCannotBeDroppedStillClosesTheSessions` | 没有重新认领；关连接的承诺由上面转写的 RR-23 用例承担 |
| | `TestAClaimAtLoginDoesNotCloseTheConnectionThatIsLoggingIn` | `Serve` 不会关闭任何连接 |
| | `TestALeaseThatWasInterruptedDropsTheResidentCopy`、`TestFencingAlsoDropsTheCopy`（RR-20260920-09） | 没有按玩家的间断。sid 级的间断用进程退出处理（由 `TestALostLeaseFencesThePlayer` 的转写覆盖） |
| | `TestAClaimWhoseCopyCannotBeDroppedIsNotTakenIntoService`、`TestAClaimWhoseCopyCannotBeDroppedIsNotRevivedByTheNextRenewal`（RR-20261001-07） | 不存在“跨间断、需要扔掉的陈旧副本” |
| | `TestAnUninterruptedLeaseKeepsItsPlayer` | 没有按玩家的续租 |
| | `TestAHandBackWaitsForThePlayersWritesToReachTheDatabase`（RR-20260926-31） | 不再把玩家交给别的进程。本进程重载的投影等待由 DataEngine 冷加载屏障保证（`entity_repository.go:178`），已有回归 `TestEnterGameColdLoadStuckOnProjectionAnswersWithinTheLoginBudget`（`enter_game_test.go.tmpl:160`）覆盖 |
| | `TestAHandBackPassHoldsTheRefreshLoopLessThanTheLeaseAllows`、`TestAHandBackPassStopsWaitingWhenItsBudgetRunsOut`（RR-20260921-04） | 卸载不再跑在续期 goroutine 上，回合预算没有了；“续期不被卸载阻塞”由转写的 RR-12 用例承担 |
| | `TestARefreshPassRetakingLostLeasesHoldsTheLoopLessThanTheLeaseAllows`、`TestARefreshPassStopsWaitingForRetakesWhenItsBudgetRunsOut`（RR-20261004-10） | 没有重新认领 |

计数：playerowner 35 个顶层用例里，删除 13 个，转写或保留 22 个（其中 RR-20260921-03 的 4 个子测试收成 1 条）；playerroute 10 个里，删除 2 个，转写 8 个。合计删除 15 个顶层用例，被删的用例都不带子测试；13 个子测试中有 9 个随所在用例转写，RR-03 的 4 个合并成 1 条。

**按 RR 归类**：

| RR | 新前提下 | 承诺的去向 |
| --- | --- | --- |
| RR-20260930-23 | 重新认领的路径消失 | **转写**：失锁时关闭全部连接（随后退出），不会留下脱离场景的连接 |
| RR-20261001-07 | **删除**：不存在跨间断的陈旧副本 | — |
| RR-20260921-03 | Redis 释放的部分**删除**；本地互斥**转写** | 卸载进行中的登录，结局一定可服务 |
| RR-20260921-04 | **删除**：回合预算没有了 | 由构造保证：续期 goroutine 不做任何其他工作；转写的 RR-12 用例钉住 |
| RR-20261004-10 | **删除**：没有重新认领 | 同上 |
| RR-20261004-11 | **转写** | 卸载进行中 `Admit` 拒绝；登录加入撤离 |
| RR-20261004-14 | **转写到 sid 锁** | 窗口从 `asked` 起算；启动获取丢了回复，靠 `Current` 等于自己来认领 |

**新增回归**（sid 锁与静态路由）：`Lost` 是吸收态，迟到的 Held 不会复活（W08-2 的同类问题）；`Applied=false` 一定是 NotHeld（W08-3 的同类问题）；启动遇到活持有者，等到上限后失败，并且不抢锁；启动遇到卡住的持有者，等到键过期后获取；Unconfirmed 之后 Held → 恢复，NotHeld → fail-stop；登录 sid 不匹配 → `player_elsewhere`，并且不装载；Claims 缺 sid → fail-closed；gift `FromSID` 不是本服 → 转交给 `FromSID`、不在本服执行；旧 payload 的兜底；matchmaker 只保留驻留玩家；~~activity 启动遇到活租约 → 以 standby 开始~~（作废，改为 activity 用假 Live 源的 expected 集合用例，见 App 单实例锁方案 §8.1）。

状态机文档 §4 的 8 个探针场景（W08-1 / 2a / 2b / 3、N-OWNS、N-CONT、N-TOLD）所依赖的机制全部删除，所以不转正。其中 W08-2（迟到的结论复活状态）和 W08-3（没生效却回答“是我们的”）在 sid 锁上有同形的风险，由上面前两条新回归钉住。

---

## 5. 已生成工程的迁移

维护者 2026-10-05 决定不考虑已生成工程，本节作废。

---

## 6. 工作量与实施拆分

> **已被取代（2026-10-05）**：实施改用 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md) §9 的合并拆分（App 锁一笔、codegen 一笔、本文的第 2～4 笔去掉 sid 锁部分）。下表第 1 笔 `game/sidlock` 取消，保留原文作对照。

| # | 提交 | 内容 | 验证 | 估时（agent 日） |
| --- | --- | --- | --- | --- |
| 1 | `feat(demo)：game/sidlock 按 sid 的进程锁` | 新包（Store + Lock），不接线；playerroute 的 8 条转写 + RR-14 的两条转写 + 新增的锁回归 | 生成工程 `go build ./... && go vet ./...`，`go test -race -count=3 ./game/sidlock/` | 0.5 |
| 2 | `refactor(demo)：PlayerOwners 改为静态绑定 + sid 锁` | 改写 `playerowner.go`；接线 `service.go`（Init 第一步获取、`OnLost` 接 Nest 围栏和 RuntimeFailure、Shutdown 释放）；`enter_game` 改用 `Serve`；`auth.go` 写 Claims；activity 以 standby 启动并检查 `Held()`；spawner 和 matchmaker 检查 `Held()`；删除 `playerroute`；codegen `demo.go` 模板清单；按 §4.4 迁移回归 | 生成工程 build / vet / `go test -race -count=3 ./internal/service/<game>/ ./game/controllers/player/`、`go test ./...`；仓库 `GOWORK=off go test -count=1 ./codegen/internal/roost/`（含 `TestDemo*` 和 prod 配置承诺）与根包 `GOWORK=off go test -count=1 .`；按 fix-contract-review 做组合复核（这一笔改变了 TTL 和关闭所有权） | 1～1.5 |
| 3 | `refactor(demo)：赠礼与 matchmaker 按 server_id 路由` | `gift.State.FromSID`、`start_gift` 加参数并重新生成 sender、`admitPhase` / `runHandoff` / `Router` 改为按 sid、旧 payload 兜底；matchmaker 改用 `Resident` | 同上，加上 `gift_handoff_test` 迁移 | 0.5 |
| 4 | `docs(demo)：…` 与演练记录 | GAME_DEMO_TEMPLATE 新节、README、CHANGELOG、bugfix 索引中相关 RR 的“已被取代”标注 | **真实隔离环境演练**（brew 的 Redis / Mongo / NATS，按 roost 本地环境说明）：① 两个 sid 各自跑自己的机器人，`success` 全绿，两个进程都活着，日志里能看到双向转交；② **崩溃重启的短暂并存**：sid 1000 的 P1 跑机器人时 `kill -STOP`，用**不同的 WAL 目录**（模拟跨主机）起同 sid 的 P1′，确认 P1′ 在 `Init` 等到旧键过期再服务、机器人在 P1′ 上通过；然后 `kill -CONT` P1，确认 P1 立即拒绝准入、下一次续期答 NotHeld、Nest 被围栏、非零退出，整个过程没有 `fatal projection version conflict`；③ 同 WAL 目录：P1′ 在 DataEngine Init 时因 `ErrLocked` 退出（现状行为，记录在案）；④ 用 `redis-cli CLIENT PAUSE <ms>` 暂停 Redis，时长小于 TTL 时准入恢复、进程不重启；时长超过窗口 + 宽限时 fail-stop | 0.5～1 |

合计约 **2.5～3.5 个 agent 日**，比状态机文档推荐的重写（3～4 日）略少。主要差别在风险：删掉的代码远多于新写的，新写的核心（sid 锁）只有一个写者 goroutine，没有按玩家的交错。

---

## 7. 需要维护者决定的点

| # | 问题 | 选项 | 推荐 |
| --- | --- | --- | --- |
| D1 | 启动拿不到 sid 锁 | 在 `Init` 里等到旧锁过期，有上限（2×TTL），超过上限失败 / 立即失败，交给外部重启 | **等待**。崩溃重启时，停服时长就是剩余 TTL，不会被 CrashLoopBackOff 拉长；对方一直在续期才会失败，那时失败是对的（§2.4） |
| D2 | sid 锁的 TTL / 续期间隔 / 准入保护带 | 15s / 3s / 5s，或沿用 30s / 10s / 5s | **15 / 3 / 5**。崩溃重启最多停服 15s；能容忍连续两次续期失败；每个进程每 3s 一次 Redis 操作，开销可以忽略 |
| D3 | 窗口耗尽，但 Redis 没有给出确定答复（Unknown） | 先停准入、继续续期，答 Held 就恢复、宽限（1×TTL）用完就 fail-stop / 窗口一耗尽就 fail-stop | **先停准入再等**。Redis 短暂抖动不会让整个服重启；只有被确定告知 NotHeld 才退出。代价是按 sid 运行的 4 个后台循环要各自检查 `Held()` |
| D4（**作废**：维护者决定不考虑已生成工程） | 赠礼的旧 payload（没有 `from_sid`） | 切换前排空 + 兜底（发送方在本进程有驻留记录就本服处理） / 只排空 / 只兜底 | **排空 + 兜底** |
| D5（**维护者 2026-10-05 决定：App 层单实例锁，只覆盖崩溃重启短暂并存，不做模块级 fencing**，见 [App 单实例锁方案](APP-SINGLETON-LOCK-2026-10-05.md)） | DataEngine 层 fencing token（§2.6 可选项 F）：关掉 A1 / A2 / 跨主机的 A5 | 本次不做，登记到跨进程移交的 feature 线（与状态机文档 §8 D3 合并） / 一起做 | **不做**。另外，sid 锁做成排在 DataEngine 之前的 kit Mod（盖住 Mod 启动阶段，需要改 codegen）也一并列为后续 |

**仍然未知、本方案没有验证的**：
- 没有实现，也没有跑演练。§2.4 的时间论证、§2.6 关于同主机 `flock` 互斥的结论来自读码（`nestwal/wal.go:222-228`），实施的第 4 笔提交要用真实进程验证。
- 生产部署里，sid 重启是否总在同一个 WAL 卷上（决定 A5 是否自动成立）取决于部署方式，demo 的 `deploy/` 没有表达这一点。
- 在 Mod 启动阶段（获取 sid 锁之前），除 DataEngine 重放和 outbox 之外，是否还有别的 Mod 会写按 sid 的状态，没有逐个核对，留给第 2 笔提交。
