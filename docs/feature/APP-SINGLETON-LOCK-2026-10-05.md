# App 单实例锁：同一服务类型 + sid 只跑一个进程（2026-10-05）

- 范围：core `app`（`app/app.go` 的 `run`、`app/runtime_failure.go`、`app/config_validation.go`），kit 的 Redis 后端（`kit/redis`）与 Nest Mod（`kit/nest/nest_mod.go`），codegen 的 bootstrap / 配置 / 停机预算 / 部署清单，game-demo 的所有权改写（[静态绑定方案](PLAYEROWNER-STATIC-BINDING-2026-10-05.md)）。
- 基线：main `c3aa0edd`。行号按这个提交。codebase-memory 索引代际为 2026-09-30，本文引用的 dataengine / nestwal / kit / redis 文件 coverage 为 `metadata_match`；`app/app.go` 为 `metadata_changed`，`docs/` 与 codegen 模板不在索引内，这些都按当前源码直接读取。
- 性质：**只出方案**，代码没改。
- 维护者 2026-10-05 的决定（本文的前提）：
  1. 只考虑**同一 sid 崩溃重启时短暂出现两个进程**这一个场景。
  2. 这个保证**由 App 本身提供**，DataEngine、activity、PlayerOwners 等模块不感知锁、不各自检查。
  3. 不做 DataEngine 层的 per-record fencing token（模块级处理）。
  4. （追加）activity 不再持有自己的全局租约：“谁还活着”由 App 单实例锁的只读查询 `Live` 提供，模块不各自维护活性（§3.6、§7.2）。
  5. D-A、D-B 按推荐（§10）。
- 取代：静态绑定方案 §2 的 `game/sidlock` 包与“`Service.Init` 第一步获取”、§2.5 的四个后台循环检查 `Held()`、§2.6 的可选项 F、§2.4 末尾“activity 以 standby 启动”与 §7 D5 的“sid 锁做成 kit Mod”。同日曾按“DataEngine 层 fencing + 多场景威胁模型”起草的方向已按维护者决定撤回，没有落成文档。
- 修订（2026-10-05 独立审查后）：§3.3 的时间关系补上调度方式与 `renew_interval ≤ guard`；§3.4 补“迟到的 Applied 不作数”；§3.5 补共享 Mod 停机不完整与预算预留；§4 逐个列出拿锁后、WAL `flock` 之前启动的 Mod；§5 规定 `OnFail` 的实现约束；§6.3 bootstrap 改为“项目里有 redis 就生成”，补上部署侧的启动等待；§7.2 改为 activity 用 `Live`；§8 补红的取得方式。

## 0. 结论先行

1. **锁放在 core `app` 里，在任何 Mod `Init` 之前获取。** 只有 App 能做到“在所有 Mod 之前”：Mod 的 `Init` / `Provide` / `Start` 是按阶段整体推进的（`app/app.go:183-245`），任何一个 Mod 都没法保证自己先于其他 Mod 的副作用；App 还持有 `RuntimeFailure` 和停机顺序，正好用来做“失锁 fail-stop”和“全部 Mod 停完才释放”。
2. **后端默认 Redis 单键 CAS**，复用 core 现成的 `redis.CompareAndSet` / `CompareAndDelete`（`redis/cas.go:105`、`:230`）。core `app` 只定义一个窄接口，Redis 实现放在 `kit/redis`，从同一份 `redis.*` 配置建一条独立的小连接。键是 `<singleton.key_prefix>:<server_type>:<sid>`。
3. **时间参数 15s / 3s / 5s，启动等待上限 2×TTL = 30s**（静态绑定方案 D1、D2 的推荐，维护者已同意）。
4. **失锁即 fail-stop，不做“窗口耗尽先停准入”**（静态绑定方案 D3 的简化，理由见 §3.4）：续期被明确告知不是自己的，或者续期失败且已经到了窗口末尾，就 `RuntimeFailure.Fail`，走 App 现成的停机路径。
5. **Nest 围栏由 App 统一触发**：`RuntimeFailure` 增加一个首次失败时的回调登记，kit 的 Nest Mod 把 `NestMgr.Fence` 登记进去。之后任何 fail-stop（失锁、DataEngine fatal、Remote fatal）都会立即拒绝新的和排队中的 Nest 派发，模块自己不用再写。
6. **DataEngine 不需要 fencing token**：崩溃重启的两个进程用的是同一个 WAL 目录，`nestwal.Open` 的 `flock` 保证同一时刻只有一个 DataEngine 写者，新进程在投影任何新写入之前先重放旧进程的 WAL（§4）。
7. game-demo 侧：`game/sidlock` 不再新增；`PlayerOwners` 不看任何锁状态；四个后台循环不检查 `Held()`；`game_route.key_prefix` 随按玩家 Redis 表一起删除。**activity 不再持有自己的全局租约**（维护者追加决定）：它的租约唯一用途是算“协调器该等哪些 game 服”，改为调用 App 锁暴露的只读查询 `Live`；`AcquireLease` / `RenewLease` / `ReleaseLease`、`incarnation`、`leaseStanding` 与 standby/retake 逻辑全部删除，崩溃重启时“activity 租约 30s 长于 App 锁 15s”的冲突随之消失（§3.6、§7.2）。
8. 实施约 **4～5 个 agent 日，5 笔提交**（§9）。D-A、D-B 维护者已按推荐决定（§10）。

---

## 1. 威胁模型

### 1.1 唯一处理的场景

同一服务类型、同一 sid，旧进程 P1 卡住或没有完全退出（SIGSTOP、长 GC、调试器暂停、进程管理器误判它已死），新进程 P2 已经被拉起。两者在同一台主机、同一个 WAL 目录上（生成的部署清单对带 WAL 的服务用 `volumeClaimTemplates` 让 WAL 随实例走，`codegen/internal/roost/render_deploy.go:964`；systemd / compose 的 WAL 在固定路径 `:133`、`:510`）。

| 时刻 | 没有 App 锁（现状） | 有 App 锁 |
| --- | --- | --- |
| P2 启动 | 所有 Mod 立即 Init / Start：连接 Redis / Mongo / NATS，RemoteEntity 恢复，订阅 bus；直到 DataEngine `Start` 打开 WAL 时才因 `flock` 失败（`nestwal/wal.go:222-228`，`ErrLocked`），然后退出 | 在任何 Mod Init 之前等锁。P1 的键还在，P2 什么都不做 |
| P1 卡住超过 TTL | — | P1 的键过期，P2 拿到锁，开始启动 Mod |
| P2 的 DataEngine `Start` | — | P1 仍然活着（卡住的进程仍持有 `flock`）：P2 在打开 WAL 时失败，正常停掉已启动的 Mod、释放锁、非零退出，由外部重启重试（同一网络命名空间里 P2 的 ops 端口 bind 会先失败，但 `OpsMod.Start` 在 goroutine 里 `ListenAndServe`、只记错误日志，`kit/ops/ops_mod.go:136-139`，不会让 P2 在 DataEngine 之前退出；player TCP 的 `net.Listen` 在 access Mod 的 `Start` 里，排在 DataEngine 之后）。P1 已经死了：`flock` 拿到，先重放 P1 留下的 WAL（`dataengine/engine/runtime.go:86`），再开始服务 |
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
	// Get 只读地返回每个键当前的值（不存在为 nil），顺序与 keys 相同；供 §3.6 的 Live 查询使用。
	// 实现不得依赖单条多键命令（Redis Cluster 下跨槽会 CROSSSLOT），见 §3.6。
	Get(ctx context.Context, keys []string) ([][]byte, error)
	Close() error
}

// SingletonOpener 在配置读完之后、任何 Mod Init 之前调用，自己建连接。
type SingletonOpener func(cfg *viper.Viper) (SingletonStore, error)

// Singleton 安装单实例锁的后端。是否启用由每个服务的 singleton.enabled 决定。
func (a *App) Singleton(open SingletonOpener) *App
```

- `singleton.enabled=true` 但 bootstrap 没有安装 opener：启动失败（fail-closed），错误写明要在 bootstrap 里调用 `Singleton`。
- 装了 opener 但 `enabled=false`：不建连接，行为与现在完全相同。
- `kit/redis` 提供 `kitredis.SingletonStore`（一个 `SingletonOpener`）：把 `RedisMod.Init` 的配置解析抽成包内函数复用，`PoolSize=2`、`MinIdleConns=0`，打开后先 Ping；两个 CAS 方法直接转调 `fredis.CompareAndSet` / `fredis.CompareAndDelete`（客户端 `ContextTimeoutEnabled`，单次调用的 ctx 截止时间生效，`redis/driver/client.go:53-75`）。CAS 只涉及单键，不需要 hash tag。opener 在任何 Mod 之前只依赖已读完的 viper 配置，不依赖 Redis Mod，自洽。缺 `redis.addr` 与 `redis.cluster_addrs` 时由 opener 报错（不要沿用 `RedisMod.Init` 的 `localhost:6379` 兜底），这样 core 的 `ValidateServiceConfig` 不需要知道 `redis.*` 键名。
- `run` 在 `NewRegistry` 之后把 Live 查询登记成 Registry 能力（`app.ModSingleton`，值为 `app.SingletonLiveness`，§3.6）。`enabled=false` 时不登记，用到它的模块在 Init 里报明确的错误（fail-closed）。

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

- **窗口**：每次 CAS 之前记下 `asked := now()`（单调时钟），Applied 之后 `validUntil = asked + ttl`。Redis 的 TTL 从它处理请求的时刻起算，不早于 `asked`，所以本地窗口不会晚于键过期（RR-20261004-14 的教训）。前提是本机单调时钟不停走：Go 在 Linux 上用 `CLOCK_MONOTONIC`，主机挂起期间不计时，那属于 §1.2 不处理的情形。
- **调度**：续期按**固定节拍**发起，节拍点是上一次 Applied 的 `asked + k × renew_interval`；单次超时等于 `renew_interval`，超时或报错就在下一个节拍点重试，不在失败后再额外睡一个间隔。这样相邻两次结论之间最多隔一个 `renew_interval`。
- **由 `ValidateServiceConfig` 钉住的关系**（启用时，四个值都为正）：
  1. `renew_interval ≤ guard`：进程没有卡住、只是续期一直 Unknown 时，进入 `[validUntil − guard, validUntil)` 之后一个节拍内一定有一次结论落在键过期之前，Lost 先于别人能拿到锁判定。原稿只有第 2 条，它说的是“容忍一次续期失败”（活性），不保证这一点；若按“失败后再睡一个间隔”调度，结论间隔可达 `2 × renew_interval = 6s > guard`。
  2. `2 × renew_interval ≤ ttl − guard`：一次续期超时之后，下一次仍在窗口内发起，一次抖动不会判 Lost。
  3. `startup_wait ≥ ttl + 2 × renew_interval`：卡住的旧持有者最后一次续期可能在 P2 启动前后才被 Redis 处理（在途请求），键最晚在 P2 启动后约 `ttl` 过期，P2 的重试间隔又是一个 `renew_interval`；原稿的 `ttl + renew_interval` 在等号处没有余量。
  默认 15 / 3 / 5 / 30s 三条都满足。违反就启动失败。

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
- **迟到的 Applied 不作数**：Applied 只有在回复到达时 `now < asked + ttl − guard` 才生效。整个进程被 SIGSTOP 时，在途的那次续期可能已被 Redis 处理（键延到 `asked + ttl`），SIGCONT 后 goroutine 读到的是停之前的 Applied，此时 `asked + ttl` 早已过去、键可能已是 P2 的。若照单全收，P1 会以 Held 再跑一个节拍才发现 NotHeld。规则：迟到的 Applied 当作 Unknown，并**立即**再续一次，以那次（`asked` 在恢复之后）的结论为准。实现上 ctx 超时通常会先把这次调用变成错误，但 SIGCONT 后计时器与网络数据谁先就绪没有保证，所以要显式判断。
- **启动获取**（D1：等待）：
  1. Acquire Applied → Held，启动续期 goroutine，继续 `run`。
  2. Applied=false 且 `current` 等于自己的值：上一次 Acquire 已经落地、只是回复丢了。这个值只有本进程设过、而且还没启动任何 Mod，直接认领，随后立即 Renew 一次，用那次的 `asked` 起算窗口。
  3. `current` 是别人的值：持有者的值变化时记一条 Info 日志（带 hostname / pid），`renew_interval` 后重试。
  4. Acquire 报错或超时（Unknown）：同样 `renew_interval` 后重试，计入 `startup_wait`；最后一次若是报错，错误写成 `singleton %s: store unavailable: %w`，与“被别人持有”区分开。
  5. 超过 `startup_wait`：返回 `singleton %s is held by a live process (%s)`。这时对方一直在续期，说明真的有两个健康进程配了同一个 sid，属于部署错误，不能抢锁。
  - 等待期间收到 SIGTERM 就直接退出：此时什么都没启动，也没持有锁。注意 `run` 现在到 `Service.Init` 之后才注册信号（`app/app.go:282`），等待期间生产上是默认处置（进程直接终止），符合本条；为了可测，等待循环要同时监听 `a.signalSource`（测试注入）或一个可取消的 ctx。拿锁之后、`:282` 之前收到 SIGTERM 同样是默认处置，不会 Release，下一次启动最多等一个 TTL，这是现状的延续，不在本次改。
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
| 正常停机或非 Lost 的 fail-stop（DataEngine fatal、Remote fatal），所有 Mod 都停完 | 是。下一次启动不用等 TTL |
| `sortMods` 失败、Mod Init / Provide / Start 失败、`unknown server type`、`PhaseModsStarted` 生命周期失败、`Service.Init` 失败，且已启动的 Mod 已按现有逻辑逆序停完 | 是 |
| `Service.Shutdown` 超时直接返回、不停 Mod（`app/app.go:346-351`），或服务专属 Mod 停机不完整而保留共享 Mod（`:357-364`） | **否**。只停续期，键在 TTL 内自然过期。此时还有组件可能在用依赖，不能让新进程提前拿锁 |
| 共享 Mod 停机不完整（`:367` 的 `stopModsReverseWithContext` 返回 `stopIncomplete`，原稿漏列） | **否**，理由同上 |
| Lost | 否。键已经不是自己的，CompareAndDelete 也删不掉别人的值；只停续期 |

- `run` 有十几个返回点（`app/app.go:174-379`）。实现用一个 `defer` 统一收尾：先停续期，再按一个只在“全部 Mod 都停完”的路径上置位的标志决定是否 Release，避免逐个返回点漏掉。
- Release 的 ctx：停机路径用 `min(shutdownCtx 剩余, 3s)`，剩余为零就跳过、等键过期；启动失败路径没有 `shutdownCtx`，固定 3s。
- 预算：`stopModsReverseBefore` 会把 `shutdownCtx` 的剩余时间全部分给 Mod（`app/app.go:420-450`），只在 codegen 里给 `total_timeout` 加 3s 并不能保证 Release 有时间。启用时 App 给 Mod 停机用的 ctx 截止时间提前 3s（`deadline − releaseBudget`），留给 Release。
- 释放之后不会有旧进程的 goroutine 继续写：Release 只在所有 Mod 停完之后发生，随后 `run` 返回、`main` 以非零或零退出；超时与 Lost 路径不 Release，进程退出后键自然过期。唯一的先后差是 DataEngine 在停机中途关闭 WAL（释放 `flock`）时，排在它后面的 Mod（game 服务里是 RemoteEntity、syncbus、NATS、Redis、Mongo，见 §4）还在停，此时 App 锁仍由本进程持有（非 Lost）或已被 P2 持有（Lost），后一种情况 P2 可能先于这几个 Mod 停完打开 WAL；它们在停机阶段只做排空，RemoteEntity 的写由 Mongo 权威校验，列为接受的边界。

### 3.6 只读的活性查询 `Live`（维护者追加决定）

```go
// app/singleton.go
// SingletonLiveness 回答“同一服务类型下，这些 sid 中哪些有进程持有单实例锁”。只读，不需要本进程持有锁。
type SingletonLiveness interface {
	Live(ctx context.Context, serverType string, sids []int32) ([]int32, error)
}
```

- 实现：App 用自己的 `key_prefix` 拼出 `<key_prefix>:<serverType>:<sid>`，调用 `SingletonStore.Get`，值非空即活，按入参顺序返回活着的 sid。一次调用最多 `MaxPageSize` 个 sid（与 `LiveGames` 的上限相同）。
- **Redis Cluster**：各 sid 的键散在不同槽，单条 `MGET` 会报 `CROSSSLOT`。`kitredis` 的 `Get` 按键逐个 `GET`（单机走一次 pipeline，Cluster 由客户端按槽拆分），不要求 `key_prefix` 带 hash tag。
- 能力名 `app.ModSingleton`，`run` 在 `NewRegistry` 之后、任何 Mod 之前登记，模块用 `app.Lookup[app.SingletonLiveness](registry, app.ModSingleton)` 取得。`enabled=false` 时不登记。
- “活”的含义是**进程持有锁**：从拿锁（任何 Mod Init 之前）到全部 Mod 停完、Release 为止；崩溃的进程最多再算 `ttl` 秒；卡住的进程键过期后不算。与 activity 原来的租约语义的差异见 §7.2。
- `serverType` 由调用方传入自己的 `server_type`（`run` 写进配置，`app/app.go:125`），所以同一部署里同一子命令的各个 sid 天然对得上；不同服务类型、同一 sid 的键因 `server_type` 段不同而不冲突。

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
- P2 拿锁之后、DataEngine 打开 WAL 之前启动的 Mod：只有在 P1 卡住超过 TTL 时才会出现，P2 随后在 `flock` 处退出。按生成的 game-demo bootstrap（共享 `lock, ops, statslog`；game 专属按 `sortMods` 的 DFS，`app/app.go:621-700`，依赖见下）逐个核对（审查时生成工程实测 bootstrap）：
  - 顺序：所有 Mod 先整体 Init、再整体 Provide、再整体 Start；Start 顺序为 `lock → ops → statslog →` `configdata → mongo → redis → nats`（NATS 可选依赖 Redis）`→ syncbus → remote_entity`（DataEngine 开了远端投影，可选依赖它；它硬依赖 redis / syncbus / mongo）`→ dataengine → etcd → manager → nest → saga → 九个 ClientMod → accessplayer → accessplayertcp`。DataEngine 之后的 Mod 不会 Start，但它们的 Provide 已经执行过（只构造对象、登记能力，未见监听或订阅）。
  - `lock`：只登记进程内锁管理器（`kit/lock/lock_mod.go:23-27`）。`ops`：HTTP 监听在 goroutine 里，失败只记日志（`kit/ops/ops_mod.go:118-142`）。`configdata` / `mongo` / `redis`：读配置、建连接、Ping。
  - `nats`：`bus.Start` 对 `Server(sid)`、`ServiceInstance(type, sid)`、`ServiceAll(type)`、`All()` 做**非队列**订阅（`bus/bus.go:287-306`）。这是唯一确定的按 sid 外部动作：P1 卡住期间发给本 sid 的消息 P2 也会收到；此时业务 handler 还没注册，按 `bus: no handler` 走死信（`bus/bus.go:802-807`）。core NATS 的非队列订阅是扇出，P1 恢复后仍会收到同一条，不会因此丢消息；开了 `nats.reliable` 时死信会多一条记录。接受。
  - `syncbus`：JetStream 模式只建对象、打日志；core NATS 模式同样只建对象（`kit/syncbus/mod.go:204-218`）。
  - `remote_entity`：启动 snapshot / interest 复制订阅，`RecoverOutbox` 扫的是**全部** sid 的 `state=applied` 远端事务（`remoteentity/mongo_committer.go:280-288`，不按 sid，幂等发布后标记 published），`StartFinalizer` 只处理本进程登记的事务。写由它自己的 Mongo 权威（`_owner_epoch` / `_grant_fence`，`remoteentity/mongo_committer.go:416-418`）校验。不构成按 sid 的副作用。
  - `dataengine` 自己在打开 WAL 之前做 `EnsureInfrastructure`（Mongo）与 `EnsureStream`（JetStream），都是幂等的基础设施检查（`dataengine/engine/assembly.go:113-119`）。
  - 结论：P2 在 `flock` 处退出之前，除了 NATS 订阅造成的死信，没有按 sid 的外部写。
- P2 因 `flock` 失败退出时按 §3.5 **释放**自己刚拿的锁。随后的行为是确定的：P1 恢复后续期 `CompareAndSet(key, v1, v1)`，键不存在或是 P3 的值，一律 Applied=false → NotHeld → Lost → fail-stop；不存在“P1 在 P2 释放后重新拿回”的路径，因为 Held 状态只能续期自己的值，重新 Acquire 只在启动阶段发生。P1 一直不恢复时，外部重启的 P3、P4… 每次都会立即拿到锁、重复上一条的启动动作、在 `flock` 处退出；重启节奏由进程管理器的退避决定（systemd `RestartSec=2s`，`codegen/internal/roost/render_deploy.go:157-158`）。
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
- 实现约束（审查补充）：
  - 回调**不放在 `sync.Once.Do` 里执行**（现在 `Fail` 用 `once.Do` 投递 `done`，`app/runtime_failure.go:30`）。`Once.Do` 里的函数若 panic，`Once` 视为已完成，`done` 永远不会投递，`run` 不会醒来；若回调里（间接）再调 `Fail`，`Once.Do` 重入会死锁。改为：互斥锁下判断并置位“已失败”、取出回调列表，解锁后逐个执行（每个回调 `recover`，panic 记日志并并入错误），最后在 `defer` 里投递 `done`。之后的 `Fail` 只并入错误、立即返回。
  - `OnFail` 与首次 `Fail` 并发：在同一把锁下判断“已失败”——已失败就在登记者的 goroutine 上立即调用，否则追加到列表；保证每个回调恰好调用一次。
  - 回调在调用 `Fail` 的 goroutine 上同步执行。现有调用方：DataEngine `onFatal`（WAL / Projector / outbox 的后台 goroutine，`kit/dataengine/mod.go:412-431`）、RemoteEntity 的 `deps.OnFatal`（`kit/remoteentity/remote_entity_mod.go:207`）、新增的续期 goroutine。都不在 Nest 快池 worker 上，也都不持有 `NestMgr.lifecycleMu`，而 `NestMgr.Fence` 只拿这把锁和 dispatcher 的 `mu` 各一次、不做 I/O（`nest/nest.go:227-240`、`nest/dispatcher.go:141-149`），所以不会阻塞也不会死锁。将来若有快池里的 handler 调 `Fail`，同样只多一次短暂加锁。
  - 启动阶段的失败：`run` 现在只在 `Serve` 开始后才 `select` `runtimeFailure.Done()`（`app/app.go:293-301`）。启动期间（例如 DataEngine 重放很长）若发生 Lost，后面的 Mod 仍会继续启动，直到进入 `select` 才停。建议 `run` 在每个阶段之间检查 `runtimeFailure.Err()`，非空就按启动失败路径停掉已启动的 Mod（Lost 时不 Release）。这对现有的 DataEngine / Remote fatal 同样适用，可随第 1 笔一起做。
- kit 的 Nest Mod 在 `Provide` 里构造 `NestMgr` 之后登记 `failure.OnFail(mgr.Fence)`。`NestMgr.Fence` 只拿一把锁、设置错误、通知 dispatcher（`nest/nest.go:227-240`），满足“快速、不阻塞”。
- 效果：失锁、DataEngine fatal、Remote fatal 走同一条路，都会立即拒绝新的和排队中的 Nest 派发，然后 `run` 执行 `Service.Shutdown`（game-demo 在这里关闭全部连接）与 Mod 停机。DataEngine `onFatal` 里显式的 `Fence` 保留不动（幂等），不在本次改。RemoteEntity fatal 从此也会围栏 Nest，这是一处行为变化，方向是更安全的 fail-stop，列入 CHANGELOG。
- 没有 App 级准入开关，也不向业务暴露 `Held()`。App 注册一个 `singleton` 健康检查（Held 为 OK，窗口内的 Unknown 为 Degraded，Lost 为 Fail，附带持有者的值），只供运维观察；它只进 `/readyz`（`/healthz` 是无条件的存活应答，`kit/ops/ops_mod.go:179-185`），不会触发存活探针重启。

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

- 后端连接沿用同一服务配置里的 `redis.*`（`addr` / `password` / `db` / `cluster_addrs`）。启用 singleton 的服务必须配置 `redis.addr` 或 `redis.cluster_addrs`；缺失由 `kitredis.SingletonStore` 在打开时报错（§3.1），core 不检查 `redis.*` 键名。
- 校验放在 `app/config_validation.go`，风格与现有的 `validatePositiveDurationIfSet` 一致：`key_prefix` 非空且不含空白，四个时长为正，外加 §3.3 的三条关系。

### 6.2 默认对哪些服务启用

**推荐：生成器对 resolved mods 包含 `dataengine` 的服务默认写 `singleton.enabled: true`**（game-demo 的 game 服务就在其中），其他服务写 `enabled: false` 并留注释。理由：带 DataEngine 的服务按 sid 划分 WAL 目录（`kit/dataengine/mod.go:110`），设计上就是“每个 sid 一个写者”（更正 2026-10-05：原稿还以“outbox owner 按 sid（`:182`）”佐证，不成立——`MongoOutboxStore.Claim` 的过滤条件不含 owner / sid（`dataengine/engine/outbox_store.go:61-66`），owner 只是标签，围栏靠逐记录的 `lease_token`），启用不改变部署形态；没有 DataEngine 的框架服务（account、mail、match 等）是否按 sid 多副本部署，生成器不知道，默认打开可能让现有的多副本部署起不来（D-B）。

### 6.3 codegen 改动

| 位置 | 改动 |
| --- | --- |
| `render.go` 的 `renderBootstrap`（`:308` 起，`app.New` 那一行在 `:380`） | 项目里**任何服务用到 `redis` Mod** 就生成 `a.Singleton(kitredis.SingletonStore)` 并加 `kitredis` import（原稿是“有服务启用 singleton 时”）。理由：是否启用写在归应用所有的配置里，生成器只能按 manifest 推断；bootstrap 是生成文件，用户不能手改。按“启用”生成的话，D-B 说的“其他服务可以手工打开”在没有 dataengine 服务的项目里做不到（打开后 fail-closed、又改不了 bootstrap）。`enabled=false` 时 opener 不会被调用，多生成一行没有代价 |
| 服务配置渲染（`renderServiceConfig`，`render.go:534`，配置段拼接在 `:543-548`） | 为带 `dataengine` 的服务追加 `singleton:` 段（`key_prefix: roost:<project>:singleton`），其他服务追加 `enabled: false` 的同一段并注释；该服务的 mods 里没有 `redis` 时只追加 `redis:` 配置段（`modCatalog["redis"].Config`），不强加 Redis Mod |
| `appendModConfigSections`（`render.go:598` 起，后加 Mod 时追加配置段） | 后来通过 `add mod dataengine` 给服务加上 DataEngine 时，同样追加 `singleton:` 段，否则这个服务会以 `enabled` 缺省（false）静默不启用。这是新工程的 `add` 流程，不属于“已生成工程迁移” |
| `shutdown_budget.go` 的 `serviceShutdownPlan`（`:113-133`） | 启用 singleton 的服务把 Release 的 3s 计入 `total_timeout`（配合 §3.5 App 侧的预留才有效） |
| `render_deploy.go:945` 的 k8s `startupProbe` | 现在是 `failureThreshold: 30 × periodSeconds: 2 = 60s`，探 `/healthz`。`/healthz` 由**共享**的 ops Mod 提供，它在拿锁之后第一批 Start（§4），而且无条件返回 200，所以 DataEngine 重放不影响它，需要覆盖的只是 `startup_wait` 加少量启动时间。改为按 `startup_wait + 30s` 计算（默认 60s，阈值不变）；`startup_wait` 调大时随之调大 |
| shell 部署的就绪等待（`render_deploy.go:126`、`:396` 的 `HEALTH_ATTEMPTS` 默认 30，探 `/readyz`，每次间隔 1s） | 正常发布先优雅停旧进程（键已释放，不等待）；旧进程停机超时或崩溃时新进程要先等最多 `ttl + 2×renew_interval` 再做 DataEngine 重放。默认值改为 `startup_wait + dataengine.startup_timeout` 对应的次数（约 60），或在文档里写明需要时调大 |
| compose 健康检查（`render_cicd.go:458`，`start_period: 30s`、`retries: 6 × interval 10s`） | `start_period` 改为 `startup_wait + dataengine.startup_timeout`（默认 60s） |
| systemd 单元（`render_deploy.go:151-159`） | `Type=simple`，`TimeoutStartSec` 不起作用，不需要改；默认 `KillMode=control-group` 下 systemd 重启前会杀掉单元里残留的进程，所以 systemd 部署很难出现本方案的“同主机并存”，锁在这里主要是兜底 |
| `demo.go:311-312` | 删除 `game_route` 段（它只给按玩家的 Redis 表用，`playerowner.go.tmpl:387`），`singleton.key_prefix` 由 `renderServiceConfig` 写入，demo 不再另写；`demo_prod_config_promises_test.go:92`、`:112` 改为检查 `singleton`；`demo.go:296-322` 的注释与 `activity.game_sids` 段注释里的 “LiveGames” 改为 “App 单实例锁的 Live 查询”；`demo.go:609` 的 `activity_lease_test.go` 从 demo 文件清单删除（§7.2） |

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
| §2.4 末尾 activity 以 `leaseStandby` 启动及其回归 | **作废**（维护者追加决定）：activity 不再持有全局租约，改用 App 的 `Live` 查询（§7.2） |

静态绑定方案的其余部分（§3 登录按 `server_id` 校验、赠礼按 `FromSID` 路由、matchmaker 只看驻留表，§4 驻留表与闲置卸载，`playerroute` 整包删除）不变。

### 7.2 activity：不再持有全局租约，改用 App 的 `Live` 查询（维护者追加决定）

> 原稿此节是“`bindAndLease` 改为 standby”，理由是 activity 全局租约 TTL 30s（`kit/service/global/service.go:48` `DefaultLeaseTTL`）长于 App 锁 15s，崩溃重启时新进程的 `AcquireLease` 会撞上旧进程还活着的租约（`bindAndLease` 现在会让 `Init` 失败）。维护者 2026-10-05 决定：“走 App 级别，不需要各个模块单独处理”。本节据此重写，standby 方案作废。

**现状**：activity 的租约只有一个用途——`expectedGameSIDs`（`demo/internal/service/game/activity.go.tmpl:297-313`）调用 `routing.LiveGames(GroupID, candidates, …)`，得出开窗时协调器该等哪些 game 服（`openCurrentWindow`，`:267`）。为此它维护 `incarnation`、`lease`（`leaseStanding` 三态，`:80-104`）、`bindAndLease` 里的 `AcquireLease`（`:205-211`）、`renewLease` 心跳与 standby/retake（`:491-552`，`leaseNotOurs` 在 `:558`，由 `runActivity` 每 5s 调用，`:242`）、停机时的 `ReleaseLease`（`:159-173`）。

**改为**：

- `expectedGameSIDs` 调 `app.SingletonLiveness.Live(ctx, serverType, runner.candidates)`，`serverType` 取 `registry.Config().GetString("server_type")`（`run` 写入，`app/app.go:125`）；结果为空时仍回退到“只有自己”（`:306-311` 的现有逻辑保留）。runner 在 `startActivity` 里用 `app.Lookup[app.SingletonLiveness](registry, app.ModSingleton)` 取得，取不到就让 `Init` 报错，写明“需要 singleton.enabled=true”。
- 删除：`incarnation`、`lease` / `leaseStanding` 及其常量、`bindAndLease` 里的 `AcquireLease`（`routing.Bind` 的组绑定与租约无关，**保留**，函数可改名 `bindGroup`）、停机函数里的 `ReleaseLease`、`renewLease` 及 `runActivity` 里对它的调用、`leaseNotOurs`、`activity_lease_test.go.tmpl`（以及 `demo.go:609` 的清单项）。其他测试里实现 `svcglobal.Routing` 的假对象相应去掉租约方法的依赖（接口本身不变）。
- 崩溃重启时“30s 租约 vs 15s 锁”的冲突随之消失：activity 不再有任何需要“拿到”的东西。

**对照核实**：

- **sid 与 server_type 能否对上**：`candidates` 来自 game 自己的 `activity.game_sids`（`demo.go:319-322` 生成，默认 `[1000]`）加上本进程 sid（`candidateSIDs`，`activity.go.tmpl:177-186`），都是 game 进程的 `--sid`；App 锁键是 `<key_prefix>:<server_type>:<sid>`，同一部署的 game 进程跑同一个子命令（`app.ServiceName("<game 服务名>")`），`server_type` 相同。所以只要用**本进程的** `server_type` 查询，就能直接对上；不要写死 `"game"`（demo 的 game 服务名可由生成参数决定）。`key_prefix` 由 App 自己拼，activity 不需要知道。
- **语义差异**（`LiveGames` 现状，`kit/service/global/service.go:450-488`）：
  1. **按组过滤**：`LiveGames` 只要 `lease.GlobalGroupID == groupID`，并且 `checkLeaseBinding`（`:494-503`）要求租约的 `GameSID / RouteEpoch / GlobalSID / GlobalGroupID` 与当前路由绑定一致，过期路由的租约当作不活。`Live` 不知道组和路由。demo 里所有 game 都在启动时 `Bind` 到同一个 `gameactivity.GroupID`，静态绑定方案下路由也不迁移，所以结果一致；候选集合本身就是“本部署的 game”，组过滤是冗余的。若将来一个部署里有多个活动组，需要让 `activity.game_sids` 只列本组的 sid。
  2. **错误语义**：`LiveGames` 对每个候选调 `Resolve`，任一候选**没有路由绑定**会让整次调用报错（`checkLeaseBinding` 只吞 `ErrLeaseNotHolder`），窗口不开；`Live` 只看键，未绑定、未启动的 sid 简单地不算活。比现状宽容，方向是对的。
  3. **“活”的时间段**：租约从 `Service.Init` 里 `AcquireLease` 起、到停机函数 `ReleaseLease` 止（停机最早阶段）；App 锁从任何 Mod Init 之前起、到全部 Mod 停完止。所以正在启动（含 DataEngine 重放）和正在优雅停机的进程也算活。启动中的进程会被等，它很快会上线并通知，可以接受；停机中的进程若恰在开窗时被算进 expected，这个窗口会等到宽限期结束（只影响那一个窗口，停机时长是秒级，概率低）。崩溃的进程最多算 15s（原来最多 30s），比现状好。
  4. **跨 Redis**：`LiveGames` 经 global 服务查询，与 game 用不用同一个 Redis 无关；`Live` 读的是 App 锁所在的 Redis，要求同一部署的 game 进程共用一个 `redis.*` 与 `singleton.key_prefix`——生成的部署就是这样。
- **kit `service/global` 的租约 API**（`AcquireLease` / `RenewLease` / `ReleaseLease` / `GameLease` / `LiveGames`）：demo 不再使用。**推荐保留、不标弃用**，只在 kit service README 与 USER_GUIDE 里写明“生成的 game-demo 改用 App 单实例锁的 Live 查询，这组 API 留给需要经 global 服务跨 Redis 查询、或需要负载快照与路由世代的部署”。理由：它是框架服务对外的 RPC 接口，有负载快照（`RenewLease` 的 `load`）和路由世代校验，`Live` 不能完全替代；本次不删除。

### 7.3 静态绑定方案 §4.4 回归的去向调整

- 原本“转写到 sid 锁”的 9 条，改为由 app 包的回归承担同一承诺（§8.1 的对应关系）：RR-20261004-14 的两条（窗口从 `asked` 起算、启动获取丢回复靠 `current` 等于自己认领）、`TestALostLeaseFencesThePlayer`（Lost → Nest 围栏 + RuntimeFailure，恰好一次）、`TestTheHandBackPassBudgetFitsInsideTheLease`（§3.3 的常量关系）、`TestASidMatchWithoutAConfirmedLeaseIsNotOwnership`（键里是同 sid 上一个实例的值 → 启动要等）。
- `TestAdmissionStopsOneGuardBandBeforeTheDeadline`、`TestAnUnknownRenewalRunsAdmissionOutInsteadOfPretending`：没有准入窗口了，改写成“续期 Unknown 时，Lost 在 `now ≥ validUntil − guard` 之后的第一个节拍判定，不提前，也不晚于 `validUntil`”。
- activity 的 `activity_lease_test.go.tmpl` 三条（RR-20260930-24：租约过期下一拍重取、别人持有时按拍重试、瞬时错误不放弃租约）：租约删除后这些承诺不再存在；对应的新承诺是“expected 集合等于 Live 返回的活 sid、Live 为空时只有自己、Live 报错时不开窗”，由 activity 用假 `SingletonLiveness` 的用例覆盖（§8.1）。
- `TestARetakenLeaseClosesTheSessionsThatLivedThroughTheGap`（RR-20260930-23）：承诺变成“fail-stop 停机时关闭全部连接”，由 game-demo 现有的 `service_shutdown_test.go.tmpl` 加一条用例覆盖。
- `TestOneBusyEntityDoesNotHoldUpTheRefreshLoop`（RR-20260920-12）：锁的续期在 App 的 goroutine 上，与闲置卸载天然分开，用例删除。
- 其余（闲置卸载、WriteGate、登录、gift、matchmaker）按静态绑定方案原样处理。

---

## 8. 测试与验证

### 8.1 app 包（先红后绿，假 store + 可控时钟）

时钟用可注入的单调时钟（含节拍 / 定时器），不用 sleep 制造时序（roost-coding 的验证纪律）；假 store 能按调用顺序返回 Applied / NotHeld / 报错 / 丢回复（落地但返回错误）/ 延迟返回。

**红怎么取得**：新 API 不存在时测试编译不过，那不是“红”。第 1 笔先落一个能编译、不做事的骨架——`SingletonStore` / `SingletonLiveness` 接口、`App.Singleton` 只保存 opener、`RuntimeFailure.OnFail` 只保存回调不调用、`run` 不拿锁——让下表每条都在**断言**上失败（例如 #1 记录到 Mod Init 被调用、#13 回调没被调用），保留这次输出作为修前证据，再实现。`kit/nest` 那一条（`Fail` 之后新派发得到 `ErrNestFenced`）以及“RemoteEntity fatal 也围栏 Nest”不依赖新 API，在现有代码上直接就是红。

**同进程跑两个 `App.run` 不可靠**：`run` 会改进程级全局量（`flog.Init` / `flog.Close`、`metrics.SetDefaultRegistry`、`fctx.SetRuntimeConfig`、`clock.SetOffset`，`app/app.go:132-151`、`app/registry.go:35`），两个实例并发会互相覆盖，`-race` 也会报。#1～#3 的“另一个持有者”用**预置了别人值的假 store** 模拟，只跑一个 `App.run`；两个真实进程的互斥留给 §8.2 演练。

| # | 回归 | 钉住的规则 |
| --- | --- | --- |
| 1 | 假 store 里键是别人的值：`run` 在任何 Mod `Init` 之前阻塞（用一个记录 Init 调用的 Mod 断言没被调用）；键被删除（模拟对方正常停机 Release）后拿到锁并完成启动 | §3.4 启动获取 |
| 2 | 别人的值一直在（模拟对方续期）：到 `startup_wait` 失败，错误含持有者的值，没有任何 Mod 被 Init，键没被改动 | D1 上限、不抢锁 |
| 3 | 别人的值在可控时钟走过 TTL 后过期：拿到锁；Acquire 一直报错到上限 → 错误是 `store unavailable` 而不是“被持有” | 等待而不是立即失败、§3.4 第 4 步 |
| 4 | Acquire 落地但回复丢失：重试时 `current` 等于自己 → 认领，并以随后那次 Renew 的 `asked` 起算窗口；`current` 是别人 → 继续等 | 丢回复认领（RR-20261004-14 同形） |
| 5 | Renew 答 NotHeld（键被删 / 是别人的值）：`RuntimeFailure` 收到恰好一次，`OnFail` 回调先于 `Done()` 执行；之后迟到的 Applied 不复活 | Lost 吸收态、W08-2 同形 |
| 6 | Renew 连续 Unknown：Lost 在 `now ≥ validUntil − guard` 之后的第一个节拍判定，且不晚于 `validUntil`；窗口内的 Unknown 不判 Lost；窗口过了但下一次（`asked` 在恢复之后的）Renew Applied → 继续 Held | §3.3 调度与关系 1、§3.4 只在续期失败时看窗口 |
| 7 | 窗口从 `asked` 起算：CAS 处理慢（假 store 延迟返回）时，`validUntil` 不晚于 `asked + ttl` | RR-20261004-14 |
| 7b | 迟到的 Applied：假 store 在时钟走过 `ttl` 之后才返回 Applied → 不进入 Held，立即再续一次；那次答 NotHeld → Lost | §3.4 迟到的 Applied 不作数 |
| 8 | 正常停机：所有 Mod 停完之后才 Release，Release 只删自己的值 | §3.5 |
| 9 | `Service.Shutdown` 超时、服务专属 Mod 停机不完整、共享 Mod 停机不完整三条路径：不 Release，只停续期；Lost 路径不 Release | §3.5 退出路径表 |
| 10 | Mod Start 失败、`Service.Init` 失败：已启动 Mod 停完后 Release | 同上 |
| 11 | `enabled=true` 无 opener → 启动失败；`enabled=false` → 不调用 opener | §3.1 fail-closed |
| 12 | 配置关系违反（`renew_interval > guard`、`2×renew_interval > ttl − guard`、`startup_wait < ttl + 2×renew_interval`、缺 `key_prefix`）→ `ValidateServiceConfig` 报错 | §3.3 |
| 13 | `RuntimeFailure.OnFail`：首次失败调用一次、按登记顺序；失败后登记立即调用；并发 `Fail` 与并发 `OnFail` 下每个回调仍只调用一次（`-race`）；回调 panic 时 `Done()` 仍投递；回调里再调 `Fail` 不死锁 | §5 实现约束 |
| 14 | `Live`：登记在 `app.ModSingleton`；按 `<key_prefix>:<serverType>:<sid>` 查询，值非空的 sid 按入参顺序返回；store 报错时返回错误；超过上限的 sid 数被拒绝；`enabled=false` 时不登记 | §3.6 |

kit：`kit/nest` 一条“`RuntimeFailure.Fail` 之后新派发得到 `ErrNestFenced`”；`kit/redis` 的 `SingletonStore` 在真实 Redis 上跑 Acquire / Renew / Release / 丢键 / 他人持有 / `Get` 多键六条（integration 标签，隔离环境），`Get` 再在 `redis-cluster-suites.sh` 的 Cluster 上跑一次，确认跨槽不报 `CROSSSLOT`。codegen：项目有 redis 就生成 `Singleton` 调用、带 dataengine 的服务有 `singleton` 段（含 `add mod dataengine` 追加）、`total_timeout` 计入 3s、shell / compose 就绪等待、demo 的 prod 配置承诺测试改为检查 `singleton`、demo 清单不再含 `activity_lease_test.go`。

game-demo（生成工程）：activity 用假 `SingletonLiveness` 的三条——expected 集合等于 Live 返回的活 sid（不含未返回的候选）；Live 为空时 expected 只有自己；Live 报错时本拍不开窗；以及“取不到 `app.ModSingleton` 时 `startActivity` 报错”。

### 8.2 真实进程演练（第 5 笔提交，隔离环境）

环境：`kit/scripts/integration/dataengine-env.sh`，按本机说明用 `ROOST_IT_HOME=$HOME/.roost-it ROOST_IT_PORT_OFFSET=1000`；env 文件只 `source`、不打印（含凭据）。生成的 game-demo 工程使用独立的 `nats.prefix`、Mongo 库名、`singleton.key_prefix`，不写共享的 game / saga / remote_entity 库与 `ROOST_*` 流。

1. **SIGSTOP 旧进程**：P1（sid 1000）跑机器人；`kill -STOP P1`；用**同一配置、同一 WAL 目录**拉起 P2。期望：P2 日志显示在等 `<prefix>:game:1000`（带 P1 的 hostname / pid），没有任何 Mod 的 init 日志；约 15s 后 P2 拿到锁，启动到 DataEngine 时因 `nestwal: directory is already locked` 退出，退出前释放锁。
2. **SIGCONT 旧进程**：`kill -CONT P1`。期望：P1 下一次续期答 NotHeld（键已过期或已被 P2 释放），日志 `singleton lock lost`，Nest 拒绝新派发，进程非零退出；全程没有 `fatal projection version conflict`。
3. **接手**：再拉起 P2，它立即拿到锁、重放 P1 的 WAL、机器人在 P2 上通过；对比 P1 最后确认的写入在 Mongo 里都在（用机器人的确认记录对账）。
4. **崩溃重启**：`kill -9` 正在服务的进程后立刻拉起新进程：新进程等待不超过 `ttl + renew_interval`（默认约 18s：键最晚在最后一次续期后 15s 过期，再加一个重试间隔）后启动；新进程的 `Service.Init` 不再因 activity 租约冲突失败（已没有租约），新进程持锁后 `Live` 即把本 sid 算活。
5. **优雅停机**：`SIGTERM` 后立即拉起新进程：新进程不等待（键已释放）。
6. 记录每一步的耗时（等待时长、P1 从 SIGCONT 到退出）和日志片段，写进演练记录（小型脱敏证据进 docs，原始日志放 `artifacts/`）。

### 8.3 性能

锁只在启动、每 3s 续期、停机时各一次 Redis 往返，不在请求路径上；Nest 的 `OnFail` 只在失败时执行。不需要性能对照，第 1 笔提交跑根包与 app / nest 定向基准确认无变化即可。

---

## 9. 实施拆分

与静态绑定方案的 4 笔合并后（它的第 1 笔 `game/sidlock` 取消）：

| # | 提交 | 内容 | 验证 | 估时（agent 日） |
| --- | --- | --- | --- | --- |
| 1 | `feat(app)：同一服务类型 + sid 的单实例锁` | 先落骨架取红（§8.1）；`app/singleton.go`（接口、状态机、`App.Singleton`、`Live` 与 `app.ModSingleton` 能力）、`run` 的挂点与退出路径（统一 `defer`、停机预算预留）、`RuntimeFailure.OnFail`（不在 `Once` 里执行回调）、配置校验、健康检查；`kit/redis` 的 `SingletonStore`（含逐键 `Get`）；`kit/nest` 登记 `OnFail(mgr.Fence)`；USER_GUIDE / CHANGELOG（含 RemoteEntity fatal 也围栏 Nest 的行为变化、kit service README 关于 global 租约 API 的说明，§7.2） | `GOWORK=off go build ./... && go vet ./...`；`go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/`；`kit/redis` integration（隔离 Redis）；根包 `GOWORK=off go test -count=1 .`；按 fix-contract-review 做组合复核（改变了关闭所有权与 fail-stop 路径） | 1.5 |
| 2 | `feat(codegen)：生成 App 单实例锁的装配与配置` | bootstrap `Singleton`（项目有 redis 即生成）、带 dataengine 的服务 `singleton` 段与 `redis` 配置段（含 `appendModConfigSections`）、停机预算 +3s、startupProbe / shell `HEALTH_ATTEMPTS` / compose `start_period`、demo 用 `singleton.key_prefix` 替换 `game_route`、demo 清单删 `activity_lease_test.go` | `GOWORK=off go test -count=1 ./codegen/internal/roost/`（含 `TestDemo*` 与 prod 配置承诺）；生成一个 demo 工程 `go build ./... && go vet ./...` | 0.5 |
| 3 | `refactor(demo)：PlayerOwners 改为静态绑定` | 静态绑定方案的第 2 笔去掉 sid 锁部分：改写 `playerowner.go`（驻留表、`Admit` / `Serve` / `AdmitBound` / `Resident`、闲置卸载）、`enter_game` 用 `Serve`、`auth.go` 写 Claims、删除 `playerroute`、activity 改用 `Live` 并删除全局租约相关代码与 `activity_lease_test`（§7.2）、按 §7.3 迁移回归、`service_shutdown_test` 加“fail-stop 关闭全部连接” | 生成工程 `go test -race -count=3 ./internal/service/<game>/ ./game/controllers/player/` 与 `go test ./...`；仓库 codegen 测试与根包 | 1～1.5 |
| 4 | `refactor(demo)：赠礼与 matchmaker 按 server_id 路由` | 静态绑定方案的第 3 笔，去掉旧 payload 兜底 | 同上，加 `gift_handoff_test` 迁移 | 0.5 |
| 5 | `docs(demo)：…` 与演练记录 | GAME_DEMO_TEMPLATE 新节（取代 §9.11.2 的所有权表）、README、CHANGELOG、bugfix 索引里相关 RR 标注“已被取代”；§8.2 演练 | §8.2 六步全部通过 | 0.5～1 |

合计约 **4～5 个 agent 日**。第 1、2 笔不依赖 demo，可以先合入；第 3 笔依赖第 1、2 笔（生成工程要有锁才能删掉按玩家的 Redis 表）。

---

## 10. 维护者决定（2026-10-05 已按推荐决定）

| # | 问题 | 选项 | 决定 |
| --- | --- | --- | --- |
| D-A | 续期一直失败、窗口耗尽时（静态绑定方案 D3 曾推荐“先停准入、宽限 1×TTL”） | 到窗口末尾即 fail-stop / 先停准入再宽限 | **即 fail-stop**（维护者 2026-10-05 按推荐决定，§3.4）：本场景里窗口耗尽的就是旧进程；“暂停”要在每个入口加检查才成立，与“模块不感知锁”冲突。代价是 Redis 连续不可用约 10s 以上时进程重启 |
| D-B | 默认对哪些服务启用 | 只对带 dataengine 的服务（含 game） / 对所有服务 | **只对带 dataengine 的服务**（维护者 2026-10-05 按推荐决定，§6.2）。其他服务可以手工打开（bootstrap 在项目有 redis 时总会安装 opener，§6.3） |

D1（等待，上限 2×TTL）、D2（15 / 3 / 5s）沿用维护者已同意的推荐，只是对象从 sid 锁换成 App 锁；D4 作废；D5 按本文实施。另：activity 不持有自己的租约、改用 App 的 `Live` 查询（维护者追加决定，§7.2）；kit `service/global` 的租约 API **改为删除**（维护者 2026-10-05：“所有这种都需要 app 接管”，见 §12，取代 §7.2 末条的“保留”）。

## 11. 没有核实的事实

- 没有实现，也没有跑演练。§1.1 的时间线、§4 的 `flock` 互斥与“先重放后服务”来自读码（`nestwal/wal.go:222-228`、`dataengine/engine/runtime.go:86-106`），由第 5 笔提交用真实进程验证。
- ~~DataEngine 之前启动的 Mod 是否还有按 sid 的副作用~~：审查时已按生成的 game-demo bootstrap 逐个核对，结论见 §4（只有 NATS 非队列订阅造成的死信）。其他项目的 Mod 组合不同，第 1 笔提交在 USER_GUIDE 里写明“拿锁后、WAL `flock` 前启动的 Mod 不应有按 sid 的外部写”这条约定。
- ~~systemd `TimeoutStartSec`、compose 健康检查时长~~：已核对，见 §6.3（systemd 是 `Type=simple`，不涉及；shell 部署 `HEALTH_ATTEMPTS` 与 compose `start_period` 需要调整）。
- `Live` 的“停机中仍算活”会让恰在那一刻开的活动窗口等到宽限期（§7.2 差异 3），没有量化；实测若成问题，可让 App 在 `PhaseServiceStopping` 时把键的值改成“停机中”标记、`Live` 不计入（不改释放时机）。
- 生产部署里崩溃重启是否总在同一个 WAL 卷上，取决于部署方式；生成的 k8s 清单用 `volumeClaimTemplates`（`render_deploy.go:964`），其他部署方式不在本文保证范围内（§1.2）。

## 12. 全仓接管清单（维护者 2026-10-05：“所有这种都需要 app 接管”）

全仓盘点了“按进程 / 按 sid 的单实例、存活登记、持有者租约、失锁自停”这一类机制（只读盘点，覆盖 core、kit、codegen、demo 模板；`skill/`、`robot/`、`scripts/`、`ai/` 只做关键词扫描）。结论：

| 机制 | 位置 | 处理 | 落在哪一笔 |
| --- | --- | --- | --- |
| demo 按玩家租约 `playerroute` / `PlayerOwners` 租约部分 | `demo/game/playerroute/`、`playerowner.go.tmpl` | 删除，改静态绑定（静态绑定方案） | 3 |
| activity 自己的 global 租约 | `activity.go.tmpl:189-211,297-315,521-551` | 删除，改用 `App.Live`（§7.2） | 3 |
| kit `service/global` 租约 API（`AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames`、`GameLease` / `LeaseState`、`Config.Leases` / `LeaseTTL` / `NewIncarnation`、错误码 570105～570108、codegen 的 `global.lease_ttl`） | `kit/service/global/service.go:253-503`、`types.go:151-210`、`routing_rpc_gen.go:44-48`、`codegen/internal/roost/framework_services.go:185` | **删除**：activity 改完后唯一调用方消失；存活由 App 统一提供。错误码按 `types.go` 惯例标 retired、不复用；RPC 重新生成；CHANGELOG 标破坏性变更。路由 `Bind` / `Resolve` / 迁移是分组归属（epoch CAS），不属于这一类，保留 | 3b（紧跟第 3 笔） |
| etcd 选举 capability `ModEtcdElection`、Redis 锁 capability `ModRedisLock` | `kit/etcd/etcd_mod.go:140`、`kit/redis/redis_mod.go:81` | **停止发布这两个 capability**：仓库内无使用者，“进程 / sid 级唯一”归 App。core 原语（`etcd/election.go`、`redis/lock.go`）保留给 cron 去重之类的键级用途，注释写明“进程 / sid 级单例请用 App 单实例锁”。CHANGELOG 标破坏性变更 | 2b（紧跟第 2 笔） |
| etcd Discovery 注册（`<prefix><type>/<sid>`，10s 租约） | `etcd/driver/discovery.go:76-182`、`kit/etcd/etcd_mod.go:59-152` | 保留为**地址 / 元数据发现**，不再承担存活语义：`App.Live` 是唯一的存活权威。Mod Start 在拿锁之后，所以同一 sid 同时只有一个进程注册。代码只改注释 + USER_GUIDE 写分工 | 5 |

不属于这一类、不动（粒度比进程细或语义不同）：WAL `flock`（按目录的最后一道防线，§4 依赖它）、DataEngine outbox worker（逐记录竞争消费，`lease_token` 围栏）、`LeaseFence` / saga 步骤 inbox / saga 引擎认领（逐命令 / 逐记录）、RemoteEntity 所有权与版本锁（按实体，身份用 sid——App 锁是它的前提）、RemoteEntity `RecoverOutbox`（全局幂等）、kit 各定时扫描（本来就无 leader 锁，靠 versionstore CAS 支持多副本）、session / directory / mail / platform 的 claim（按键 CAS）、JetStream 竞争消费 durable、进程内锁。

默认“一个 sid 一个进程”、由 App 锁使之成立而无需改动的代码：World 实体 ID = sid、demo `runtimeid` 以 sid 分片（两个同 sid 进程会撞 ID）、bus 按实例订阅 `svc.<type>.<sid>`、JetStream 按实例 RPC durable、syncbus 每 sid 一个 durable、部署清单单副本。

`Live` 只能看见开了 `singleton.enabled` 的服务类型；activity 的 candidates 都是 game（默认开启）。若项目给 game 关掉 singleton，`Live` 恒空、activity 退化为只等自己——USER_GUIDE 写明这条约束。
