# App 单实例锁：同一服务类型 + sid 只跑一个进程（2026-10-05）

- 范围：core `app`（`app/app.go` 的 `run`、`app/runtime_failure.go`、`app/config_validation.go`），kit 的 Redis 后端（`kit/redis`）与 Nest Mod（`kit/nest/nest_mod.go`），codegen 的 bootstrap / 配置 / 停机预算 / 部署清单，game-demo 的所有权改写（[静态绑定方案](PLAYEROWNER-STATIC-BINDING-2026-10-05.md)）。
- 基线：main `c3aa0edd`。行号按这个提交。codebase-memory 索引代际为 2026-09-30，本文引用的 dataengine / nestwal / kit / redis 文件 coverage 为 `metadata_match`；`app/app.go` 为 `metadata_changed`，`docs/` 与 codegen 模板不在索引内，这些都按当前源码直接读取。
- 性质：**只出方案**，代码没改。
- 维护者 2026-10-05 的决定（本文的前提）：
  1. 只考虑**同一 sid 崩溃重启时短暂出现两个进程**这一个场景。
  2. 这个保证**由 App 本身提供**，DataEngine、activity、PlayerOwners 等模块不感知锁、不各自检查。
  3. 不做 DataEngine 层的 per-record fencing token（模块级处理）。
- 取代：静态绑定方案 §2 的 `game/sidlock` 包与“`Service.Init` 第一步获取”、§2.5 的四个后台循环检查 `Held()`、§2.6 的可选项 F 与 §7 D5 的“sid 锁做成 kit Mod”。同日曾按“DataEngine 层 fencing + 多场景威胁模型”起草的方向已按维护者决定撤回，没有落成文档。

## 0. 结论先行

1. **锁放在 core `app` 里，在任何 Mod `Init` 之前获取。** 只有 App 能做到“在所有 Mod 之前”：Mod 的 `Init` / `Provide` / `Start` 是按阶段整体推进的（`app/app.go:183-245`），任何一个 Mod 都没法保证自己先于其他 Mod 的副作用；App 还持有 `RuntimeFailure` 和停机顺序，正好用来做“失锁 fail-stop”和“全部 Mod 停完才释放”。
2. **后端默认 Redis 单键 CAS**，复用 core 现成的 `redis.CompareAndSet` / `CompareAndDelete`（`redis/cas.go:105`、`:230`）。core `app` 只定义一个窄接口，Redis 实现放在 `kit/redis`，从同一份 `redis.*` 配置建一条独立的小连接。键是 `<singleton.key_prefix>:<server_type>:<sid>`。
3. **时间参数 15s / 3s / 5s，启动等待上限 2×TTL = 30s**（静态绑定方案 D1、D2 的推荐，维护者已同意）。
4. **失锁即 fail-stop，不做“窗口耗尽先停准入”**（静态绑定方案 D3 的简化，理由见 §3.4）：续期被明确告知不是自己的，或者续期失败且已经到了窗口末尾，就 `RuntimeFailure.Fail`，走 App 现成的停机路径。
5. **Nest 围栏由 App 统一触发**：`RuntimeFailure` 增加一个首次失败时的回调登记，kit 的 Nest Mod 把 `NestMgr.Fence` 登记进去。之后任何 fail-stop（失锁、DataEngine fatal、Remote fatal）都会立即拒绝新的和排队中的 Nest 派发，模块自己不用再写。
6. **DataEngine 不需要 fencing token**：崩溃重启的两个进程用的是同一个 WAL 目录，`nestwal.Open` 的 `flock` 保证同一时刻只有一个 DataEngine 写者，新进程在投影任何新写入之前先重放旧进程的 WAL（§4）。
7. game-demo 侧：`game/sidlock` 不再新增；`PlayerOwners` 不看任何锁状态；四个后台循环不检查 `Held()`；`game_route.key_prefix` 随按玩家 Redis 表一起删除。activity 的 `bindAndLease` 改 standby **仍然要改**，因为 activity 全局租约的 TTL（30s）长于 App 锁（15s），App 锁拿到时旧进程的 activity 租约可能还活着（§7.2）。
8. 实施约 **4～5 个 agent 日，5 笔提交**（§9）。需要维护者决定的只有 2 点（§10）。

---

## 1. 威胁模型

### 1.1 唯一处理的场景

同一服务类型、同一 sid，旧进程 P1 卡住或没有完全退出（SIGSTOP、长 GC、调试器暂停、进程管理器误判它已死），新进程 P2 已经被拉起。两者在同一台主机、同一个 WAL 目录上（生成的部署清单对带 WAL 的服务用 `volumeClaimTemplates` 让 WAL 随实例走，`codegen/internal/roost/render_deploy.go:964`；systemd / compose 的 WAL 在固定路径 `:133`、`:510`）。

| 时刻 | 没有 App 锁（现状） | 有 App 锁 |
| --- | --- | --- |
| P2 启动 | 所有 Mod 立即 Init / Start：连接 Redis / Mongo / NATS，RemoteEntity 恢复，订阅 bus；直到 DataEngine `Start` 打开 WAL 时才因 `flock` 失败（`nestwal/wal.go:222-228`，`ErrLocked`），然后退出 | 在任何 Mod Init 之前等锁。P1 的键还在，P2 什么都不做 |
| P1 卡住超过 TTL | — | P1 的键过期，P2 拿到锁，开始启动 Mod |
| P2 的 DataEngine `Start` | — | P1 仍然活着（卡住的进程仍持有 `flock`）：P2 在打开 WAL 时失败，正常停掉已启动的 Mod、释放锁、非零退出，由外部重启重试。P1 已经死了：`flock` 拿到，先重放 P1 留下的 WAL（`dataengine/engine/runtime.go:86`），再开始服务 |
| P1 恢复（SIGCONT） | P1 照常服务，继续写自己的 WAL，订阅、定时器、matchmaker 全部照旧 | P1 的续期 goroutine 下一次 CAS 得到“不是我的”（键已过期或是 P2 的值），立即 `RuntimeFailure.Fail`：Nest 围栏、优雅停机、非零退出，释放 `flock` |

App 锁在这个场景里提供的东西：
- P2 在 P1 的键还活着的时候**不启动任何 Mod**，也就不会在 DataEngine 之前产生 bus 订阅、Redis 写、RemoteEntity 恢复这类副作用（现状下 DataEngine 之前的 Mod 都已经启动过了）。
- P1 恢复之后**自己知道已经被替换**，fail-stop，而不是继续服务一个已经有接替者的 sid。现状下 P1 会一直跑下去，P2 则在 `flock` 上反复失败重启。
- 没有 DataEngine 的服务（只用 Redis / bus 的按 sid 单例）也得到同样的保证，因为锁不依赖 WAL。

### 1.2 不处理（维护者决定）

跨主机或换卷的并存（容器换节点、WAL 不随实例迁移）、网络分区、Redis failover 丢键、旧主机的 WAL 后来被重放：都不在范围内，本方案不为它们提供保证，也不展开分析。

---

## 2. 放在哪一层

| 位置 | 能否在所有 Mod 之前 | 能否统一 fail-stop | 结论 |
| --- | --- | --- | --- |
| core `app.run` | 能：配置读完、日志就绪、`Registry` 建好（`app/app.go:162`）之后，`sortMods` 和第一个 `Init`（`:183`）之前 | 能：`RuntimeFailure` 是 `Registry` 的内建能力（`app/registry.go:40`），`run` 已经在 `runtimeFailure.Done()` 上等待（`:293-301`） | **采用** |
| kit Mod（排在 DataEngine 之前） | 不能：所有 Mod 先整体 `Init`、再整体 `Provide`、再整体 `Start`；Redis 连接要到 `Provide` 才有，锁最早只能在 `Start` 拿到，那时排在它前面的 Mod 已经 `Start` 过了。共享 Mod 与服务专属 Mod 之间的可选依赖还会被忽略（`app/app.go:688`） | 能 | 不采用 |
| 业务 `Service.Init` 第一步（静态绑定方案 §2.4） | 不能：所有 Mod 都已经启动 | 要在业务里接 Nest / RuntimeFailure | 不采用 |

**core 与 kit 的分工**：core `app` 定义接口、状态机、时间推导、生命周期挂点和 fail-stop；它不 import Redis 驱动。Redis 的实现放在 `kit/redis`，因为 kit 已经负责 `redis.*` 的配置解析（`kit/redis/redis_mod.go:32-55`）和驱动装配（`redisdriver.Assemble`）。这与 Mod “kit 解析配置、core 提供机制”的分工一致。

---

## 3. 锁

### 3.1 core 接口与 App 选项

```go
// app/singleton.go
// SingletonStore 是单实例锁的后端：两个原子操作，语义与 redis.CompareAndSet / CompareAndDelete 相同。
// expected 为 nil 表示“键必须不存在”；Applied=false 时 current 是键里现在的值（不存在为 nil）。
type SingletonStore interface {
	CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (applied bool, current []byte, err error)
	CompareAndDelete(ctx context.Context, key string, expected []byte) (applied bool, err error)
	Close() error
}

// SingletonOpener 在配置读完之后、任何 Mod Init 之前调用，自己建连接。
type SingletonOpener func(cfg *viper.Viper) (SingletonStore, error)

// Singleton 安装单实例锁的后端。是否启用由每个服务的 singleton.enabled 决定。
func (a *App) Singleton(open SingletonOpener) *App
```

- `singleton.enabled=true` 但 bootstrap 没有安装 opener：启动失败（fail-closed），错误写明要在 bootstrap 里调用 `Singleton`。
- 装了 opener 但 `enabled=false`：不建连接，行为与现在完全相同。
- `kit/redis` 提供 `kitredis.SingletonStore`（一个 `SingletonOpener`）：把 `RedisMod.Init` 的配置解析抽成包内函数复用，`PoolSize=2`、`MinIdleConns=0`，打开后先 Ping；两个方法直接转调 `fredis.CompareAndSet` / `fredis.CompareAndDelete`。Redis Cluster 只涉及单键，不需要 hash tag。

### 3.2 键、值与操作

- 键：`<singleton.key_prefix>:<server_type>:<sid>`。`server_type` 和 `sid` 是 `run` 已经写进配置的值（`app/app.go:98-124`）。`key_prefix` 在启用时必填，沿用 `<service>.key_prefix` 的约定（`kit/mods/service_servicemods.go:34`：没有默认值，不能含空白），生成值 `roost:<project>:singleton`。
- 值：`<token>|<hostname>|<pid>|<started_unix_ms>`。`token` 是本次进程启动时 `crypto/rand` 生成的 16 位十六进制串，所以同 sid 重启一定是另一个持有者。后三段只给运维看；CAS 比较整个值，不影响语义。
- 操作（每个都是一次原子往返）：

| 操作 | 调用 | 结论 |
| --- | --- | --- |
| Acquire | `CompareAndSet(key, nil, v, TTL)` | Applied → 持有；否则 `current` 是当前持有者 |
| Renew | `CompareAndSet(key, v, v, TTL)` | Applied → Held；Applied=false → NotHeld；报错或超时 → Unknown |
| Release | `CompareAndDelete(key, v)` | 只删自己的值 |

Applied=false 一律是 NotHeld，结构上不存在“没生效却回答是我们的”。

### 3.3 时间参数

| 配置 | 默认 | 关系 |
| --- | --- | --- |
| `singleton.ttl` | 15s | 崩溃重启最长要等的时间 |
| `singleton.renew_interval` | 3s | 单次续期的超时也取这个值 |
| `singleton.guard` | 5s | 窗口提前于键过期结束的量 |
| `singleton.startup_wait` | 30s（= 2×ttl） | 启动时等锁的上限 |

- **窗口**：每次 CAS 之前记下 `asked := now()`（单调时钟），Applied 之后 `validUntil = asked + ttl`。Redis 的 TTL 从它处理请求的时刻起算，不早于 `asked`，所以本地窗口不会晚于键过期（RR-20261004-14 的教训）。
- **由 `ValidateServiceConfig` 钉住的关系**（启用时）：`2 × renew_interval ≤ ttl − guard`（一次续期超时之后，下一次仍落在窗口内）；`startup_wait ≥ ttl + renew_interval`（卡住的旧持有者的键一定在等待期内过期）；四个值都为正。违反就启动失败。

### 3.4 状态机（一个 goroutine）

```text
 Waiting ──Acquire Applied / current==自己的值──▶ Held ──Renew Applied──┐
   │ current 是别人的值、未到上限：每 renew_interval 重试          ▲      │
   │ 到上限仍被别人持有                                            └──────┘
   ▼                                                    Renew NotHeld，或
 启动失败（run 返回错误，非零退出）                     Renew Unknown 且 now ≥ validUntil − guard
                                                                  ▼
                                                        Lost（吸收态）→ RuntimeFailure.Fail
```

- **单写者**：获取、续期及其结论都在同一个 goroutine 里顺序执行，不存在旧回复作用到新状态上的交错，也就不需要世代号。`Lost` 之后不再续期，迟到的结论不会把它改回去。
- **启动获取**（D1：等待）：
  1. Acquire Applied → Held，启动续期 goroutine，继续 `run`。
  2. Applied=false 且 `current` 等于自己的值：上一次 Acquire 已经落地、只是回复丢了。这个值只有本进程设过、而且还没启动任何 Mod，直接认领，随后立即 Renew 一次，用那次的 `asked` 起算窗口。
  3. `current` 是别人的值：持有者的值变化时记一条 Info 日志（带 hostname / pid），`renew_interval` 后重试。
  4. 超过 `startup_wait`：返回 `singleton %s is held by a live process (%s)`。这时对方一直在续期，说明真的有两个健康进程配了同一个 sid，属于部署错误，不能抢锁。
  - 等待期间收到 SIGTERM 就直接退出：此时什么都没启动，也没持有锁。
- **为什么“窗口耗尽”不先停准入、而是到窗口末尾就 fail-stop**（D3 的简化）：
  1. 在本场景里，续期一直失败到窗口末尾，几乎只有一种原因：这个进程自己卡住了 `ttl − guard` 以上，它就是那个“旧进程”。让它留在一个“暂停服务”的状态等答复，只会拉长与新进程的并存。
  2. 要让“暂停”真的生效，就得在 Nest 之外的每个入口（bus handler、后台循环、定时器）都检查一个开关，这正是维护者不要的模块级检查；只在 Nest 加开关，又不是一个完整的保证。
  3. 代价是 Redis 连续不可用超过约 10s 时进程会退出重启。Redis 长时间不可用不在本方案范围内，而且重启是安全的方向；需要更宽容时调大 `ttl`。
  - 只看时间不够：进程卡住后恢复，窗口按时间已经过了，但键可能仍是自己的（卡住时间在 `ttl − guard` 与 `ttl` 之间）。所以“窗口末尾”只在**续期失败**时才判 Lost；恢复后的那次续期如果 Applied，就证明键一直是自己的、没有别人拿过，照常继续。
- **Lost 时的动作**：`RuntimeFailure.Fail(fmt.Errorf("singleton lock lost: %w", cause))`，日志里带当前持有者的值。之后由 App 统一处理（§5）。

### 3.5 在 `run` 里的位置与释放

```text
run:
  读配置 → ValidateServiceConfig → 日志 → NewRegistry → PhaseAppInit
  ▶ 单实例锁：Open → Acquire（等待）→ 启动续期 goroutine
  Mod Init → Provide → Start（共享，再服务专属）→ PhaseModsStarted
  Service.Init → Serve …（等待信号 / RuntimeFailure / Serve 结束）
  Service.Shutdown → 服务专属 Mod 逆序停 → 共享 Mod 逆序停
  ▶ 单实例锁：停止续期 → Release（仅当所有 Mod 都停完）→ Close
```

| 退出路径 | 是否 Release |
| --- | --- |
| 正常停机，所有 Mod 都停完 | 是。下一次启动不用等 TTL |
| Mod Init / Provide / Start 失败、`unknown server type`、`Service.Init` 失败，且已启动的 Mod 已按现有逻辑逆序停完 | 是 |
| `Service.Shutdown` 超时直接返回、不停 Mod（`app/app.go:347-351`），或服务专属 Mod 停机不完整而保留共享 Mod（`:357-364`） | **否**。只停续期，键在 TTL 内自然过期。此时还有组件可能在用依赖，不能让新进程提前拿锁 |
| Lost | 否。键已经不是自己的，CompareAndDelete 也删不掉别人的值；只停续期 |

Release 用 `min(shutdownCtx 剩余, 3s)` 的 ctx；剩余为零就跳过，等键过期。

---

## 4. 与 DataEngine 的关系：为什么不需要 fencing token

在 §1.1 的场景里，DataEngine 的单写者由两层共同保证：

1. **同一时刻只有一个 DataEngine 写者**：WAL 目录按 sid 划分（`kit/dataengine/mod.go:110`），`nestwal.Open` 对 `writer.lock` 加 `flock(LOCK_EX|LOCK_NB)`（`nestwal/wal.go:222-228`）。卡住的进程仍然持有这把锁，P2 打不开 WAL；P1 死后锁才释放。
2. **新进程先重放、后服务**：DataEngine `Start` 在打开 WAL（`dataengine/engine/assembly.go:119`）之后，先 `Projector.Flush` 把旧进程留下的全部记录投影完（`dataengine/engine/runtime.go:86`），然后才注册 loader、置 ready（`:106`）。所以 P2 看到的 Mongo 已经包含 P1 全部已确认的写入。

App 锁在这之上补的是：P2 不会在 P1 的键还活着时启动 DataEngine 之前的那些 Mod；P1 恢复后自己 fail-stop，而不是继续服务。

**残余边界**（都在同一个 WAL 上，不会产生两个 DataEngine 写者）：
- P1 有事务在准入之后、写 WAL 之前被暂停：恢复后照样追加到同一个 WAL，随后 P1 fail-stop；这条记录由下一个打开该目录的进程按 WAL 顺序重放。因为期间没有别的写者，它不会与谁冲突。
- P1 恢复到续期 goroutine 拿到“不是我的”答复之间（一次 Redis 往返，毫秒级），快池可能还会处理已经排队的请求。DataEngine 写入仍然只有 P1 一个写者，是合法的；非 DataEngine 副作用（一次 Redis 写、一次 bus 发布）可能多发生一次。这是本方案接受的边界。
- P1 卡住后一直不恢复（SIGSTOP 之后没人发 SIGCONT）：P2 每次都在 DataEngine 打开 WAL 时失败退出、由外部重启重试。App 锁不替代杀掉旧进程；运维看到的是 P2 日志里的 `nestwal: directory is already locked`。
- P2 拿锁之后、DataEngine 打开 WAL 之前启动的 Mod（Redis / Mongo / NATS 连接、RemoteEntity）：只有在 P1 卡住超过 TTL 时才会出现，P2 随后在 `flock` 处退出。RemoteEntity 的写由它自己的 Mongo 权威（`_owner_epoch` / `_grant_fence`，`remoteentity/mongo_committer.go:416-418`）校验。这些 Mod 的 `Start` 是否还有别的按 sid 副作用，没有逐个核对，留给第 1 笔提交（§11）。
- 不在范围内：WAL 不在同一个目录（换主机、换卷、改了 `dataengine.wal.dir`）。那时 `flock` 不再互斥，App 锁也不提供 DataEngine 层的保证（维护者决定）。

---

## 5. fail-stop 由 App 统一触发

现状：DataEngine 的 `onFatal` 自己查找 `NestMgr` 调 `Fence`，再调 `RuntimeFailure.Fail`（`kit/dataengine/mod.go:412-431`）；RemoteEntity 的 fatal 只调 `Fail`，不围栏 Nest（`kit/remoteentity/remote_entity_mod.go:206-208`）。每加一个 fail-stop 来源，就要再写一遍“找 Nest、围栏”。

改为：

```go
// app/runtime_failure.go
// OnFail 登记一个在首次失败时调用的回调（只调用一次，按登记顺序，同步执行）。
// 回调必须快速、不阻塞：它运行在调用 Fail 的 goroutine 上。失败已经发生时登记，立即调用。
func (r *RuntimeFailure) OnFail(hook func(error))
```

- `Fail` 在第一次失败时先依次执行回调，再向 `done` 投递，所以 `run` 醒来开始优雅停机时，Nest 已经被围栏。
- kit 的 Nest Mod 在 `Provide` 里构造 `NestMgr` 之后登记 `failure.OnFail(mgr.Fence)`。`NestMgr.Fence` 只拿一把锁、设置错误、通知 dispatcher（`nest/nest.go:227-240`），满足“快速、不阻塞”。
- 效果：失锁、DataEngine fatal、Remote fatal 走同一条路，都会立即拒绝新的和排队中的 Nest 派发，然后 `run` 执行 `Service.Shutdown`（game-demo 在这里关闭全部连接）与 Mod 停机。DataEngine `onFatal` 里显式的 `Fence` 保留不动（幂等），不在本次改。RemoteEntity fatal 从此也会围栏 Nest，这是一处行为变化，方向是更安全的 fail-stop，列入 CHANGELOG。
- 没有 App 级准入开关，也不向业务暴露 `Held()`。App 注册一个 `singleton` 健康检查（Held 为 OK，Lost 为 Fail，附带持有者的值），只供运维观察。

---

## 6. 配置与装配

### 6.1 配置

```yaml
singleton:
  enabled: true
  key_prefix: roost:<project>:singleton
  ttl: 15s
  renew_interval: 3s
  guard: 5s
  startup_wait: 30s
```

- 后端连接沿用同一服务配置里的 `redis.*`（`addr` / `password` / `db` / `cluster_addrs`）。启用 singleton 的服务必须有 `redis` 段；没有就在 `ValidateServiceConfig` 报错。
- 校验放在 `app/config_validation.go`，风格与现有的 `validatePositiveDurationIfSet` 一致，外加 §3.3 的两条关系。

### 6.2 默认对哪些服务启用

**推荐：生成器对 resolved mods 包含 `dataengine` 的服务默认写 `singleton.enabled: true`**（game-demo 的 game 服务就在其中），其他服务写 `enabled: false` 并留注释。理由：带 DataEngine 的服务按 sid 划分 WAL 目录（`kit/dataengine/mod.go:110`）、outbox owner 也按 sid（`:182`），设计上就是“每个 sid 一个写者”，启用不改变部署形态；没有 DataEngine 的框架服务（account、mail、match 等）是否按 sid 多副本部署，生成器不知道，默认打开可能让现有的多副本部署起不来（D-B）。

### 6.3 codegen 改动

| 位置 | 改动 |
| --- | --- |
| `render.go` 的 `renderBootstrap`（`:380` 起） | 有任何服务启用 singleton 时，生成 `a.Singleton(kitredis.SingletonStore)`，并加 `kitredis` import |
| 服务配置渲染（`render.go:543`、`:598` 的配置段拼接） | 为带 `dataengine` 的服务追加 `singleton:` 段；该服务的 mods 里没有 `redis` 时只追加 `redis:` 配置段（`modCatalog["redis"].Config`），不强加 Redis Mod |
| `shutdown_budget.go` 的 `serviceShutdownPlan`（`:113-133`） | 启用 singleton 的服务把 Release 的 3s 计入 `total_timeout` |
| `render_deploy.go:945` 的 k8s `startupProbe` | 现在是 `failureThreshold: 30 × periodSeconds: 2 = 60s`。`/healthz` 由 ops Mod 提供，要等锁拿到、Mod 启动之后才有。改为按 `startup_wait + dataengine.startup_timeout + 30s` 计算（默认 90s，`failureThreshold: 45`）；systemd 的 `TimeoutStartSec`（如有）同理，实施时核对 |
| `demo.go:311-312` | 删除 `game_route` 段（它只给按玩家的 Redis 表用，`playerowner.go.tmpl:387`），`singleton.key_prefix` 用同一套前缀推导；`demo_prod_config_promises_test.go:92`、`:112` 改为检查 `singleton` |

---

## 7. game-demo 侧（相对静态绑定方案）

### 7.1 删除或改为“由 App 保证”

| 静态绑定方案里的设计 | 现在 |
| --- | --- |
| §2 新增 `demo/game/sidlock/`（Store + Lock，约 250 行）及其回归 | **不新增**。机制在 core `app`，回归在 app 包（§8.1） |
| §2.4 `Service.Init` 第一步获取 sid 锁、`Shutdown` 最后释放 | **删除**。App 在 Mod 之前获取、在 Mod 之后释放 |
| §2.5 失锁动作：`NestMgr.Fence`、`CloseSessions`、`RuntimeFailure.Fail` | **由 App 统一**：`Fail` → `OnFail` 回调围栏 Nest → `Service.Shutdown` 关闭全部连接 |
| §2.5 `tickWorld`、`runActivity`、`startSpawner`、`formMatches` 每轮检查 `Held()` | **删除** |
| §4.3 `Admit` = `sid.Admitted() ∧ resident ∧ !dropping` | 改为 `resident ∧ !dropping`；`Serve` 去掉 “sid lock not held → `login_timeout`” 分支；`AdmitBound`、`Resident` 同样不看锁 |
| §4.3 新增 `Held()` | **删除** |
| §4.2 `AdmissionGuard` 移到 `sidlock` | 成为 App 的 `singleton.guard` |
| §3.3 旧 payload（无 `from_sid`）的兜底 | 维护者决定不考虑已生成工程，**删除**（D4 作废） |
| §5 迁移 | **作废** |
| 配置 `game_route.key_prefix` | **删除**，改为 `singleton.key_prefix`（§6.3） |

静态绑定方案的其余部分（§3 登录按 `server_id` 校验、赠礼按 `FromSID` 路由、matchmaker 只看驻留表，§4 驻留表与闲置卸载，`playerroute` 整包删除）不变。

### 7.2 activity：`bindAndLease` 仍要改为 standby

activity 的全局租约 TTL 默认 30s（`kit/service/global/service.go:48` `DefaultLeaseTTL`）、每 5s 续期一次（`activity.go.tmpl:41`）。App 锁的 TTL 是 15s，所以：
- 旧进程崩溃（死了）：App 锁最多 15s 后可拿，旧进程的 activity 租约最长还要再活约 15s。
- 旧进程卡住：同理。

新进程走到 `Service.Init` → `bindAndLease`（`activity.go.tmpl:148`、`:189-212`）时，`AcquireLease` 可能得到 `held`，现在的写法会让 `Init` 失败、进程退出、外部重启，直到旧租约过期。App 锁不能消除这个冲突，所以按静态绑定方案 §2.4 末尾的做法改：冲突时以 `leaseStandby` 启动，`renewLease` 已经会在每次心跳时重试 `AcquireLease`（`activity.go.tmpl:541-551`）。这是 activity 对它**自己的**全局租约的活性处理，不是检查 App 锁，不违背“模块不感知锁”。回归：“启动遇到活租约就以 standby 开始，下一次心跳拿到租约”。

### 7.3 静态绑定方案 §4.4 回归的去向调整

- 原本“转写到 sid 锁”的 9 条，改为由 app 包的回归承担同一承诺（§8.1 的对应关系）：RR-20261004-14 的两条（窗口从 `asked` 起算、启动获取丢回复靠 `current` 等于自己认领）、`TestALostLeaseFencesThePlayer`（Lost → Nest 围栏 + RuntimeFailure，恰好一次）、`TestTheHandBackPassBudgetFitsInsideTheLease`（§3.3 的常量关系）、`TestASidMatchWithoutAConfirmedLeaseIsNotOwnership`（键里是同 sid 上一个实例的值 → 启动要等）。
- `TestAdmissionStopsOneGuardBandBeforeTheDeadline`、`TestAnUnknownRenewalRunsAdmissionOutInsteadOfPretending`：没有准入窗口了，改写成“续期 Unknown 时，Lost 恰好在 `validUntil − guard` 判定，不提前、不延后”。
- `TestARetakenLeaseClosesTheSessionsThatLivedThroughTheGap`（RR-20260930-23）：承诺变成“fail-stop 停机时关闭全部连接”，由 game-demo 现有的 `service_shutdown_test.go.tmpl` 加一条用例覆盖。
- `TestOneBusyEntityDoesNotHoldUpTheRefreshLoop`（RR-20260920-12）：锁的续期在 App 的 goroutine 上，与闲置卸载天然分开，用例删除。
- 其余（闲置卸载、WriteGate、登录、gift、matchmaker）按静态绑定方案原样处理。

---

## 8. 测试与验证

### 8.1 app 包（先红后绿，假 store + 可控时钟）

时钟用可注入的单调时钟，不用 sleep 制造时序（roost-coding 的验证纪律）；假 store 能按调用顺序返回 Applied / NotHeld / 报错 / 丢回复（落地但返回错误）。

| # | 回归 | 钉住的规则 |
| --- | --- | --- |
| 1 | 两个 App 实例同 server_type + sid：第二个在任何 Mod `Init` 之前阻塞（用一个记录 Init 调用的 Mod 断言没被调用）；第一个正常停机后第二个拿到锁并完成启动 | §3.4 启动获取、§3.5 释放 |
| 2 | 第一个一直续期：第二个到 `startup_wait` 失败，错误含持有者的值，没有任何 Mod 被 Init，键没被改动 | D1 上限、不抢锁 |
| 3 | 持有者卡住（不续期）：第二个在键过期后拿到 | 等待而不是立即失败 |
| 4 | Acquire 落地但回复丢失：重试时 `current` 等于自己 → 认领，并以随后那次 Renew 的 `asked` 起算窗口；`current` 是别人 → 继续等 | 丢回复认领（RR-20261004-14 同形） |
| 5 | Renew 答 NotHeld（键被删 / 是别人的值）：`RuntimeFailure` 收到恰好一次，`OnFail` 回调先于 `Done()` 执行；之后迟到的 Applied 不复活 | Lost 吸收态、W08-2 同形 |
| 6 | Renew 连续 Unknown：Lost 恰好在 `validUntil − guard` 判定；窗口内的 Unknown 不判 Lost；窗口过了但下一次 Renew Applied（卡住后恢复、键仍是自己的）→ 继续 Held | §3.4 只在续期失败时看窗口 |
| 7 | 窗口从 `asked` 起算：CAS 处理慢（假 store 延迟返回）时，`validUntil` 不晚于 `asked + ttl` | RR-20261004-14 |
| 8 | 正常停机：所有 Mod 停完之后才 Release，Release 只删自己的值 | §3.5 |
| 9 | `Service.Shutdown` 超时与服务专属 Mod 停机不完整两条路径：不 Release，只停续期 | §3.5 退出路径表 |
| 10 | Mod Start 失败、`Service.Init` 失败：已启动 Mod 停完后 Release | 同上 |
| 11 | `enabled=true` 无 opener → 启动失败；`enabled=false` → 不调用 opener | §3.1 fail-closed |
| 12 | 配置关系违反（`2×renew_interval > ttl − guard`、`startup_wait < ttl + renew_interval`、缺 `key_prefix`）→ `ValidateServiceConfig` 报错 | §3.3 |
| 13 | `RuntimeFailure.OnFail`：首次失败调用一次、按登记顺序；失败后登记立即调用；并发 `Fail` 下仍只调用一次（`-race`） | §5 |

kit：`kit/nest` 一条“`RuntimeFailure.Fail` 之后新派发得到 `ErrNestFenced`”；`kit/redis` 的 `SingletonStore` 在真实 Redis 上跑 Acquire / Renew / Release / 丢键 / 他人持有五条（integration 标签，隔离环境）。codegen：bootstrap 生成 `Singleton` 调用、带 dataengine 的服务有 `singleton` 段、`total_timeout` 计入 3s、startupProbe 阈值、demo 的 prod 配置承诺测试改为检查 `singleton`。

### 8.2 真实进程演练（第 5 笔提交，隔离环境）

环境：`kit/scripts/integration/dataengine-env.sh`，按本机说明用 `ROOST_IT_HOME=$HOME/.roost-it ROOST_IT_PORT_OFFSET=1000`；env 文件只 `source`、不打印（含凭据）。生成的 game-demo 工程使用独立的 `nats.prefix`、Mongo 库名、`singleton.key_prefix`，不写共享的 game / saga / remote_entity 库与 `ROOST_*` 流。

1. **SIGSTOP 旧进程**：P1（sid 1000）跑机器人；`kill -STOP P1`；用**同一配置、同一 WAL 目录**拉起 P2。期望：P2 日志显示在等 `<prefix>:game:1000`（带 P1 的 hostname / pid），没有任何 Mod 的 init 日志；约 15s 后 P2 拿到锁，启动到 DataEngine 时因 `nestwal: directory is already locked` 退出，退出前释放锁。
2. **SIGCONT 旧进程**：`kill -CONT P1`。期望：P1 下一次续期答 NotHeld（键已过期或已被 P2 释放），日志 `singleton lock lost`，Nest 拒绝新派发，进程非零退出；全程没有 `fatal projection version conflict`。
3. **接手**：再拉起 P2，它立即拿到锁、重放 P1 的 WAL、机器人在 P2 上通过；对比 P1 最后确认的写入在 Mongo 里都在（用机器人的确认记录对账）。
4. **崩溃重启**：`kill -9` 正在服务的进程后立刻拉起新进程：新进程等待不超过 15s 后启动，activity 以 standby 开始、下一次心跳拿到租约。
5. **优雅停机**：`SIGTERM` 后立即拉起新进程：新进程不等待（键已释放）。
6. 记录每一步的耗时（等待时长、P1 从 SIGCONT 到退出）和日志片段，写进演练记录（小型脱敏证据进 docs，原始日志放 `artifacts/`）。

### 8.3 性能

锁只在启动、每 3s 续期、停机时各一次 Redis 往返，不在请求路径上；Nest 的 `OnFail` 只在失败时执行。不需要性能对照，第 1 笔提交跑根包与 app / nest 定向基准确认无变化即可。

---

## 9. 实施拆分

与静态绑定方案的 4 笔合并后（它的第 1 笔 `game/sidlock` 取消）：

| # | 提交 | 内容 | 验证 | 估时（agent 日） |
| --- | --- | --- | --- | --- |
| 1 | `feat(app)：同一服务类型 + sid 的单实例锁` | `app/singleton.go`（接口、状态机、`App.Singleton`）、`run` 的挂点与退出路径、`RuntimeFailure.OnFail`、配置校验、健康检查；`kit/redis` 的 `SingletonStore`；`kit/nest` 登记 `OnFail(mgr.Fence)`；核对 DataEngine 之前启动的 Mod 是否有按 sid 副作用；USER_GUIDE / CHANGELOG | `GOWORK=off go build ./... && go vet ./...`；`go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/`；`kit/redis` integration（隔离 Redis）；根包 `GOWORK=off go test -count=1 .`；按 fix-contract-review 做组合复核（改变了关闭所有权与 fail-stop 路径） | 1.5 |
| 2 | `feat(codegen)：生成 App 单实例锁的装配与配置` | bootstrap `Singleton`、带 dataengine 的服务 `singleton` 段与 `redis` 配置段、停机预算 +3s、startupProbe 阈值、demo 用 `singleton.key_prefix` 替换 `game_route` | `GOWORK=off go test -count=1 ./codegen/internal/roost/`（含 `TestDemo*` 与 prod 配置承诺）；生成一个 demo 工程 `go build ./... && go vet ./...` | 0.5 |
| 3 | `refactor(demo)：PlayerOwners 改为静态绑定` | 静态绑定方案的第 2 笔去掉 sid 锁部分：改写 `playerowner.go`（驻留表、`Admit` / `Serve` / `AdmitBound` / `Resident`、闲置卸载）、`enter_game` 用 `Serve`、`auth.go` 写 Claims、删除 `playerroute`、activity 以 standby 启动、按 §7.3 迁移回归、`service_shutdown_test` 加“fail-stop 关闭全部连接” | 生成工程 `go test -race -count=3 ./internal/service/<game>/ ./game/controllers/player/` 与 `go test ./...`；仓库 codegen 测试与根包 | 1～1.5 |
| 4 | `refactor(demo)：赠礼与 matchmaker 按 server_id 路由` | 静态绑定方案的第 3 笔，去掉旧 payload 兜底 | 同上，加 `gift_handoff_test` 迁移 | 0.5 |
| 5 | `docs(demo)：…` 与演练记录 | GAME_DEMO_TEMPLATE 新节（取代 §9.11.2 的所有权表）、README、CHANGELOG、bugfix 索引里相关 RR 标注“已被取代”；§8.2 演练 | §8.2 六步全部通过 | 0.5～1 |

合计约 **4～5 个 agent 日**。第 1、2 笔不依赖 demo，可以先合入；第 3 笔依赖第 1、2 笔（生成工程要有锁才能删掉按玩家的 Redis 表）。

---

## 10. 需要维护者决定的点

| # | 问题 | 选项 | 推荐 |
| --- | --- | --- | --- |
| D-A | 续期一直失败、窗口耗尽时（静态绑定方案 D3 曾推荐“先停准入、宽限 1×TTL”） | 到窗口末尾即 fail-stop / 先停准入再宽限 | **即 fail-stop**（§3.4）：本场景里窗口耗尽的就是旧进程；“暂停”要在每个入口加检查才成立，与“模块不感知锁”冲突。代价是 Redis 连续不可用约 10s 以上时进程重启 |
| D-B | 默认对哪些服务启用 | 只对带 dataengine 的服务（含 game） / 对所有服务 | **只对带 dataengine 的服务**（§6.2）。其他服务可以手工打开 |

D1（等待，上限 2×TTL）、D2（15 / 3 / 5s）沿用维护者已同意的推荐，只是对象从 sid 锁换成 App 锁；D4 作废；D5 按本文实施。

## 11. 没有核实的事实

- 没有实现，也没有跑演练。§1.1 的时间线、§4 的 `flock` 互斥与“先重放后服务”来自读码（`nestwal/wal.go:222-228`、`dataengine/engine/runtime.go:86-106`），由第 5 笔提交用真实进程验证。
- DataEngine 之前启动的 Mod（尤其 RemoteEntity 的 `Start`）是否还有按 sid 的副作用，没有逐个核对；第 1 笔提交核对并在这里补结论。
- systemd 单元是否设置了 `TimeoutStartSec`、compose 的健康检查时长，没有核对；第 2 笔提交随 startupProbe 一起处理。
- 生产部署里崩溃重启是否总在同一个 WAL 卷上，取决于部署方式；生成的 k8s 清单用 `volumeClaimTemplates`（`render_deploy.go:964`），其他部署方式不在本文保证范围内（§1.2）。
