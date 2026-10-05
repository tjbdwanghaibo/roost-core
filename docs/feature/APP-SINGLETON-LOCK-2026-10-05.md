# App 单实例锁：同一服务类型 + sid 只跑一个进程（2026-10-05）

- 范围：core `app`（`app/app.go` 的 `run`、`app/runtime_failure.go`、`app/config_validation.go`），kit 的 Redis 后端（`kit/redis`）与 Nest Mod（`kit/nest/nest_mod.go`），codegen 的 bootstrap / 配置 / 停机预算 / 部署清单，game-demo 的所有权改写（[静态绑定方案](PLAYEROWNER-STATIC-BINDING-2026-10-05.md)）。
- 基线：main `c3aa0edd`。行号按这个提交。codebase-memory 索引代际为 2026-09-30，本文引用的 dataengine / nestwal / kit / redis 文件 coverage 为 `metadata_match`；`app/app.go` 为 `metadata_changed`，`docs/` 与 codegen 模板不在索引内，这些都按当前源码直接读取。
- 性质：方案。**状态（2026-10-05）：第 1～5 笔已实施**（第 1、2、2b、3、3b、4 笔提交 `d4ac9853`、`6863dbc3`、`71c6fb6b`、`f051e24a`、`40d89ac6`、`5bdac773`；第 5 笔文档与真实进程演练见 §13，演练中的修复 `364b763c`）。
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
// 实现必须遵守 ctx 截止时间（§3.3 的“Lost 不晚于 validUntil”依赖它）；内部重试只能在同一个 ctx 内，
// CAS(v,v)、认领自己的值、按值删除重复执行都安全；续期不能被并发的 Get 拖住（审查收尾，§13）。
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
- `kit/redis` 提供 `kitredis.SingletonStore`（一个 `SingletonOpener`）：把 `RedisMod.Init` 的配置解析抽成包内函数复用，建两个独立客户端——CAS 一个、`Get`（Live）一个，各 `PoolSize=2`、`MinIdleConns=0`，并发的 Live 占满连接时续期不用等连接（审查收尾，§13）——打开后先 Ping；两个 CAS 方法直接转调 `fredis.CompareAndSet` / `fredis.CompareAndDelete`（客户端 `ContextTimeoutEnabled`，单次调用的 ctx 截止时间生效，`redis/driver/client.go:53-75`）。CAS 只涉及单键，不需要 hash tag。opener 在任何 Mod 之前只依赖已读完的 viper 配置，不依赖 Redis Mod，自洽。缺 `redis.addr` 与 `redis.cluster_addrs` 时由 opener 报错（不要沿用 `RedisMod.Init` 的 `localhost:6379` 兜底），这样 core 的 `ValidateServiceConfig` 不需要知道 `redis.*` 键名。
- `run` 在 `NewRegistry` 之后把 Live 查询登记成 Registry 能力（`app.ModSingleton`，值为 `app.SingletonLiveness`，§3.6）。`enabled=false` 时不登记，用到它的模块在 Init 里报明确的错误（fail-closed）。

### 3.2 键、值与操作

- 键：`<singleton.key_prefix>:<server_type>:<sid>`。`server_type` 和 `sid` 是 `run` 已经写进配置的值（`app/app.go:98-124`）。`key_prefix` 在启用时必填，沿用 `<service>.key_prefix` 的约定（`kit/mods/service_servicemods.go:34`：没有默认值，不能含空白），生成值 `roost:<project>:singleton`。
- 值：`<token>|<hostname>|<pid>|<started_unix_ms>`。`token` 是本次进程启动时 `crypto/rand` 生成的 16 位十六进制串，所以同 sid 重启一定是另一个持有者。后三段只给运维看；CAS 比较整个值，不影响语义。
- 操作（每个都是一次原子操作；单机下是一次往返，Redis Cluster 下 go-redis 按 `MaxRedirects` 处理 MOVED / ASK 与网络错误重试，都在同一个 ctx 内，重复执行安全）：

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
| `singleton.renew_interval` | 3s | 续期节拍；单次续期的超时取这个值，持有期间不越过 `validUntil` |
| `singleton.guard` | 5s | 窗口提前于键过期结束的量 |
| `singleton.startup_wait` | 30s（= 2×ttl） | 启动时等锁的上限 |

- **窗口**：每次 CAS 之前记下 `asked := now()`（单调时钟），Applied 之后 `validUntil = asked + ttl`。Redis 的 TTL 从它处理请求的时刻起算，不早于 `asked`，所以本地窗口不会晚于键过期（RR-20261004-14 的教训）。前提是本机单调时钟不停走：Go 在 Linux 上用 `CLOCK_MONOTONIC`，主机挂起期间不计时，那属于 §1.2 不处理的情形。
- **调度**：续期按**固定节拍**发起，节拍点是上一次 Applied 的 `asked + k × renew_interval`；单次超时取 `renew_interval`、持有期间截到 `validUntil`（`min(renew_interval, validUntil − asked)`，`asked` 已过 `validUntil` 时不截），超时或报错就在下一个节拍点重试，不在失败后再额外睡一个间隔。这样相邻两次结论之间最多隔一个 `renew_interval`。
- **由 `ValidateServiceConfig` 钉住的关系**（启用时，四个值都为正）：
  1. `renew_interval ≤ guard`：进程没有卡住、只是续期一直 Unknown 时，键过期之前一定**发起**一次落在 `[validUntil − guard, validUntil)` 里的续期：最后一个早于 `validUntil − guard` 的结论之后，下一拍不晚于 `validUntil − guard + renew_interval ≤ validUntil` 发起。不等式只管发起时刻、不管结论时刻：这一拍若给满 `renew_interval` 的单次超时，超时结论最晚落在 `validUntil − guard + 2 × renew_interval`，可能晚于键过期（默认 5s / 3s 时是 `validUntil + 1s`）。保证“Lost 不晚于 `validUntil` 判定”的是单次超时截断到 `validUntil`（见上一条“调度”，`e3810ef1`）；不等式的作用是让这样一拍一定存在。（更正：本条原写“进入窗口之后一个节拍内一定有一次结论落在键过期之前”，把截断的作用算到了不等式上，按 `e3810ef1` 的审查推导改正。）原稿只有第 2 条，它说的是“容忍一次续期失败”（活性），不保证这一点；若按“失败后再睡一个间隔”调度，结论间隔可达 `2 × renew_interval = 6s > guard`。
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
  ▶ 单实例锁：停止续期 → Release（仅当所有 Mod 都停完且未 Lost）→ Close（全部 Mod 停完或没启动任何 Mod 时；停机不完整留到进程退出）
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
- 预算：`stopModsReverseBefore` 会把 `shutdownCtx` 的剩余时间全部分给 Mod（`app/app.go:420-450`），只在 codegen 里给 `total_timeout` 加 3s 并不能保证 Release 有时间。启用时 App 给 Mod 停机用的 ctx 截止时间提前 3s（`deadline − releaseBudget`），留给 Release；进入停机时已 Lost 不会 Release，不预留（审查收尾，§13）。
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
- **kit `service/global` 的租约 API**（`AcquireLease` / `RenewLease` / `ReleaseLease` / `GameLease` / `LiveGames`）（**已作废**：按 §12 删除，第 3b 笔已实施，见 §13）：demo 不再使用。**推荐保留、不标弃用**，只在 kit service README 与 USER_GUIDE 里写明“生成的 game-demo 改用 App 单实例锁的 Live 查询，这组 API 留给需要经 global 服务跨 Redis 查询、或需要负载快照与路由世代的部署”。理由：它是框架服务对外的 RPC 接口，有负载快照（`RenewLease` 的 `load`）和路由世代校验，`Live` 不能完全替代；本次不删除。

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

- ~~没有实现，也没有跑演练~~：已实现（第 1～4 笔），§1.1 的时间线、§4 的 `flock` 互斥与“先重放后服务”已由第 5 笔真实进程演练验证（§13）。
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

## 13. 实施记录

### 第 1 笔（2026-10-05，提交 `d4ac9853`）

- 范围：`app/singleton.go`（`SingletonStore` / `SingletonOpener` / `SingletonLiveness`、`App.Singleton`、`app.ModSingleton`、状态机、健康检查 `singleton`）、`app/app.go` 的 `run` 挂点与统一 `defer` 收尾、`app/runtime_failure.go` 的 `OnFail`、`app/config_validation.go` 的 `singleton.*` 校验；`kit/redis/singleton.go`（`kitredis.SingletonStore`，`redisConfig` 与 `RedisMod.Init` 共用）；`kit/nest/nest_mod.go` 登记 `OnFail(mgr.Fence)`；`kit/mods.ModSingleton` 别名；ci.yml Redis job 与 `redis-cluster-suites.sh` 加 `./kit/redis`；CHANGELOG / USER_GUIDE / TROUBLESHOOTING T-212、T-213。§7.2 末条“kit service README 写明 global 租约 API 保留”按 §12 作废，未写。
- 先红后绿：骨架（接口、`App.Singleton` 只存 opener、`OnFail` 只存回调、`run` 不拿锁）下 §8.1 的 #1～#14 与“启动期间 RuntimeFailure 停在阶段边界”全部在断言上失败（例：#1 `service started serving while the singleton key belonged to another process`，#13 `hooks ran [], want [first second] once`，#12 `ValidateServiceConfig = <nil>`）；`kit/nest` 的 `TestRuntimeFailureFencesNestDispatch` 在原代码上 `FenceError after RuntimeFailure = <nil>`。实现后 `go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/` 通过，singleton 用例 `-count=30 -race` 稳定。
- 与方案的差异（均为细化，语义不变）：
  1. 启动获取时遇到**迟到的 Applied** 与“丢回复认领”同样处理：立即用一次 Renew 确认、以那次的 `asked` 起算窗口（§3.4 只对续期写了迟到规则）。
  2. Mod 停机截止时间的 Release 预留取 `min(3s, shutdown.total_timeout / 2)`，避免很小的 total_timeout 被预留吃光；codegen 第 2 笔给 total_timeout 加 3s 后与方案一致。
  3. `runtimeFailure.Err()` 的检查点：每个 Mod `Start` 之前、最后一个 `Start` 之后（`PhaseModsStarted` 之前）、`Service.Init` 之后（与 `PhaseServiceStarted` 失败同一清理路径：`Service.Shutdown` + 逆序停 Mod；该路径错误文本由 `cleanup after lifecycle failure` 改为 `cleanup after startup failure`）。
  4. 启动失败路径只有在逆序停止**全部完成**（没有 Mod 停机超时）时才 Release；`stopModsReverse` 为此返回是否停完。
  5. 健康检查在打开 store 时就登记，未持有时报 fail（`not acquired`）；等待期间 ops 还没启动，不可见。
  6. `kitredis.SingletonStore` 关闭 go-redis 自动重试（`MaxRetries = -1`），每次 CAS 是一次往返，重试由状态机按节拍负责。
  7. 测试注入的 `signalSource` 在启动等待期间被监听，收到信号 `run` 返回 nil（生产上等待期间不注册信号，按默认处置终止进程，与方案相同）。
  8. `Live` 拒绝空 `serverType`；空 `sids` 直接返回、不访问 store。
- 验证：见提交说明；`kitredis` integration 在隔离 Redis 上通过，`Get` 跨槽在本机临时起的 3 主 Redis Cluster（用完即删）上通过。未验证：§8.2 真实进程演练（第 5 笔）、codegen 生成链路（第 2 笔）。

### 第 2 笔（2026-10-05，提交 `6863dbc3`）

- 范围：`codegen/internal/roost` 的 `render.go`（`serviceSingletonEnabled` / `projectInstallsSingleton` / `renderSingletonConfig`、bootstrap、`renderServiceConfig`、`appendModConfigSections` + `turnGeneratedSingletonOn`、开发 compose）、`shutdown_budget.go`（`serviceShutdown.release`、摘要行与解析、doctor 按配置的 `singleton.enabled` 计入）、`render_deploy.go`（`startupAllowance`、install.sh / rollback.sh 的 `DEFAULT_HEALTH_ATTEMPTS`、shell README、k8s `kubernetesStartupFailureThreshold`）、`render_cicd.go`（compose `start_period`）、`catalog.go`（`dataengine.startup_timeout` 取常量）；文档 USER_GUIDE / DEPLOYMENT / PROJECT_GENERATOR / CHANGELOG。
- 先红后绿：新增 `singleton_promises_test.go` 六条（bootstrap 安装条件、只给 dataengine 服务打开、add mod 翻转与手改保持 + WARN、停机摘要含 Release 且能读回、doctor 计入 Release、部署启动等待）。只加常量与 `release` 字段的骨架下全部在断言上失败，例：`redis without dataengine: bootstrap does not install a.Singleton(kitredis.SingletonStore)`、`configs/service/config.account.yaml: a service without dataengine has singleton.enabled = "" (present false), want an explicit false`、`deploy/shell/install.sh lacks "  game) DEFAULT_HEALTH_ATTEMPTS=60 ;;\n"`、`compose game: start_period is not 60s`、`singleton on, total 105s: ok total_timeout …`。实现后通过；既有停机预算用例的钉住值随公式 +3s 更新（game-demo game 服务 `-mods configdata,mongo,nats,dataengine,nest` 108s / 113s → 111s / 116s，缺省 Mod 集 114s / 119s → 117s / 122s）；`demo_prod_config_promises_test.go` 加查三份 game 配置的 `singleton`（Secret 比对段加 `singleton`）。
- 与方案的差异：
  1. bootstrap 生成条件是“项目有 `redis` Mod **或** 有带 `dataengine` 的服务”，比 §6.3 的“有 redis”多一个分支：dataengine 服务不一定带 redis Mod（例如 `-mods configdata,nest`），它默认打开的 singleton 没有 opener 会启动即 fail-closed。同理开发 compose 对这类项目也起 redis。
  2. 停机摘要行在有 Release 时多一段 `+ 3s for the singleton release`，正则可选匹配；没有 Release 的块文本与旧版逐字相同（sync 照常识别、刷新）。doctor 按**配置**里的 `singleton.enabled` 计入 3s（生成公式按 manifest），`Set it to` 建议值随之 +3s。
  3. k8s startupProbe 在默认值下不变（60s = 30s + 30s），只在打开 singleton 的服务里加注释、阈值按 `startup_wait + 30s` 计算；shell 的 `HEALTH_ATTEMPTS` 改为按 Service 的 `case`（含 `*)` 兜底 30），README 列出各服务默认值。
  4. `add mod` 翻转只改与生成文本逐字相同的 `enabled: false` 段；改过且未写 `enabled: true` 的段保持并 WARN。
- 推迟到第 3 笔（会破坏当前 demo 运行，或去掉仍在用代码的回归）：`demo.go` 删除 `game_route` 段（`playerowner.go.tmpl:387` 仍在 Init 里要求 `game_route.key_prefix`）、`demo.go` 注释与 `activity.game_sids` 注释里 “LiveGames” 的改写（activity 仍用 `LiveGames`）、demo 清单删 `activity_lease_test.go`（它守的租约代码仍在）。`demo_prod_config_promises_test` 因此是“加查 singleton”而非“改为检查 singleton”。
- 验证：`GOWORK=off go test -count=1 ./codegen/...` 全绿；生成 `roost project new sdemo -template game-demo`，`go mod edit -replace` 指向本地 roost-core 后 `go build ./... && go vet ./...` 通过、`go test ./internal/service/game/` 通过、`shellcheck deploy/shell/*.sh deploy/docker/*.sh deploy/k8s/*.sh` 无输出；生成配置 game `singleton.enabled: true`、`key_prefix: roost:sdemo:singleton`、`total_timeout: 117s`，其他服务 `enabled: false`，bootstrap 有 `a.Singleton(kitredis.SingletonStore)`。

### 第 2b 笔（2026-10-05，提交 `71c6fb6b`）

- 范围：`kit/redis/redis_mod.go` 去掉 `ModRedisLock` capability，`kit/etcd/etcd_mod.go` 去掉 `ModEtcdElection`，`kit/mods/name.go` 删除两个常量；`redis/lock.go` 的 `IDistLock`、`etcd/election.go` 的 `IElection` 类型注释写明进程 / sid 级单例请用 `app.Singleton`；CHANGELOG `[Unreleased]` 新增 Removed（破坏性变更与迁移）、USER_GUIDE 单实例锁节、kit README（组件总览、capability 表、分布式锁与选主节）、PROJECT_GENERATOR 的 redis Mod 描述。
- 使用者核对：`git grep` 两个常量与 `redis.lock` / `etcd.election`，仓库内（core、kit、codegen、demo 模板）除发布点与文档外无引用；`redis/driver`、`etcd/driver` 的 `Assembly.Locks` / `Election` 保留（driver 自身测试在用）。仓外调用无法核对，按破坏性变更登记。
- 验证：`GOWORK=off go vet` 与 `go test -race -count=1 ./kit/etcd/... ./kit/redis/... ./kit/mods/...` 通过。
- 两笔合并后在干净 worktree：`gofmt -l` 空、`go build ./...`、`go generate ./...` 后 porcelain 为空、根包 `go test -count=1 .`（含 `TestCoreDependencyBoundary`）与 `go test -count=1 ./codegen/...` 全绿；rebase 到 `b291edb9` 之后复跑 build、根包、`kit/redis` / `kit/mods` 与 codegen 的 singleton / 停机 / demo 用例通过。未验证：生成工程的真实进程启动（拿锁、Release 预留实测，属第 5 笔演练）、kubeconform / `docker compose config` 对新模板的渲染（本机未跑）。

### 第 1 笔审查收尾（2026-10-05，提交 `4959a0dd`、`86f687cf`、`cd8c1ad3`、`cbccacdb`、`ad35bbcc` 与本笔文档）

第 1 笔审查修复 `e3810ef1`（续期单次超时截到 `validUntil`）、`b291edb9`（接手用例只推进启动等待的定时器）之后的遗留项，逐条处理：

1. **Live 与续期共用 PoolSize=2 的连接池**（`ad35bbcc`）：Redis 变慢时两条并发 Live 就能占满连接，续期等不到连接、单次超时内报错（Unknown），持续到窗口末尾误判 Lost。`kitredis.SingletonStore` 改为两个独立客户端（CAS 一个、Live 一个，各 `PoolSize=2`）。没有选“续期独占一条连接”：go-redis 的连接池没有“预留一条给某类调用”的接口，独立客户端是最简单可靠的隔离；CAS 客户端留 2 条给 Cluster 拓扑刷新与超时后重拨。回归 `TestSingletonStoreRenewalDoesNotWaitBehindStalledLiveQueries`（integration，真实 Redis + 只扣住 GET 的 TCP 代理，等代理确认 Live 的连接全部卡住后再以 1s 超时续期）；修前：`renew while every Live connection is stalled = false "" context deadline exceeded, want applied`。
2. **接口契约与 Cluster 重试**（本笔文档）：`app.SingletonStore` 注释写明实现必须遵守 ctx 截止时间（Lost 的时间界依赖它）、内部重试只能在同一个 ctx 内、重复执行安全、续期不能被 Live 拖住；`kitredis` 注释把“每次 CAS 一次往返”改为准确表述：单机一次往返，Cluster 下 go-redis 按 `MaxRedirects`（缺省 3）处理 MOVED / ASK 与网络错误重试（`osscluster.go` 的 `process`），重试与退避受 ctx 限定，CAS(v,v)、获取重试读到自己的值后认领、按值删除重复执行都安全。纯注释，无红测试。
3. **已 Lost 仍给 Release 预留 3s**（`4959a0dd`）：进入停机时已 Lost 则 `releaseReserve = 0`，整段 `shutdown.total_timeout` 给 Mod 停机。回归 `TestSingletonLostLockLeavesTheReleaseBudgetToModStop`（held 仍预留 3s、lost 不预留，比较 `Service.Shutdown` 与 Mod `StopWithContext` 收到的截止时间）；修前 lost 例：`mod stop deadline is 3s before the shutdown deadline, want 0s`。
4. **测试覆盖缺口**（`cd8c1ad3`）：新增 `TestSingletonLostDuringStartupStopsTheModsWithoutReleasing`（Mod Start 期间续期 NotHeld → 停在下一阶段边界、不启动后面的 Mod、停掉已启动的 Mod、不 Release、关闭 store）；`TestRunStopsStartingModsAfterARuntimeFailure` 改为单实例锁关 / 开各一遍（开启时非失锁的 fail-stop 照常 Release）。现有代码已满足，首跑即绿，没有修前红。
5. **`kitredis.SingletonStore.Close` 不幂等**（`cbccacdb`）：改为 `sync.Once`，之后返回第一次的结果。回归 `TestSingletonStoreCloseIsIdempotent`（不需要 Redis）；修前：`Close #2 = redis: client is closed, want nil`。
6. **不 Release 路径上 finish 关闭 store，仍在跑的组件 Live 报 client closed**（`86f687cf`）：评估为简单且与 `run` 的既有取舍一致——停机不完整时 `run` 本来就不等仍在跑的组件、保留它们的依赖到进程退出，store 承载 Live 能力，同理保留。改为只在没拿到锁（还没启动任何 Mod）或全部 Mod 已停完时关闭；停机不完整（含 Lost 且停机不完整）时不关闭，生产上进程随即退出；同一进程里再次 `Run`（测试）时这份 store 与那些组件一起泄漏，接受。回归：`TestSingletonIsNotReleasedWhenShutdownIsIncomplete` 三个子例补“store 未关闭、Live 仍可用”（假 store 关闭后 `Get` 报错，与真实客户端一致）；修前三例：`store closed while a component that may still call Live is running`。
7. **文档**（本笔）：USER_GUIDE 的 `renew_interval` 注释改为“单次续期的超时取这个值，持有期间不越过 validUntil”，后端 / 释放两条补两个客户端、Lost 不预留、停机不完整时连接留到进程退出；本文 §3.3 关系 1 的理由按审查推导改正（保证“Lost 不晚于 validUntil”的是单次超时截断，不等式只保证这一拍一定发起），§3.1 / §3.2 / §3.5 同步；CHANGELOG `[Unreleased]` 第 1 笔条目补一句。

只记录、不改的审查观察：
- 观察 6：启动获取丢回复后下次多等一个 ttl——启动等待以报错结束（最后一次 Acquire 已落地、回复丢失，`ErrSingletonStoreUnavailable`）时状态仍是 Waiting，不 Release，键是本次进程的值，下一次启动要等它过期。启动等待以报错结束本就意味着后端不可用，Release 大概率也发不出去，维持现状。
- 观察 7：`PhaseServiceStopped` 生命周期钩子在 `run` 返回前、`finish` 的 Release 之前执行，用的是同一个 `shutdownCtx`，钩子慢会吃掉 Release 的预算（剩余为零即跳过 Release、键等 ttl 过期）。方向安全，维持现状。

验证（`GOWORK=off`）：`gofmt -l` 空；`go vet ./app/ ./kit/redis/ ./kit/nest/`（含 `-tags integration`）通过；`go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/` 通过；app 单实例锁用例 `-race -count=50` 稳定；`kit/redis` integration 在隔离 Redis（`~/.roost-it` 环境）上 `-race -count=3` 通过，测试键前缀随机、用例结束删除，事后 SCAN 无残留；根包 `go test -count=1 .` 通过；干净 worktree `go build ./... && go vet ./...` 通过。未验证：两客户端在真实 Redis Cluster 上的 integration（本机未起 Cluster，`ROOST_REVIEW_CLUSTER` 用例跳过）。

### 第 3 笔（2026-10-05，提交 `f051e24a`）

- 范围：`demo/internal/service/game/playerowner.go.tmpl` 重写为本地驻留表 + 闲置卸载（`Serve` / `AdmitBound` / `Resident` / `SID` / `Admit` / `AdmitMessage` / `CloseServedSessions`）；`enter_game.go.tmpl` 用 `Serve`，`controller.go.tmpl` 的 `playerOwners` 只剩 `Serve`；`auth.go.tmpl` 把 `role.ServerID` 写进 `Principal.Claims["server_id"]`；`player_elsewhere.go.tmpl` 含义收窄；`service.go.tmpl` 去掉 Redis / DataEngine 投影装配，`Shutdown` 第一步断开服务中的玩家；`activity.go.tmpl` 删除全局租约、改用 `app.SingletonLiveness.Live`（§7.2）；`gift_saga.go.tmpl` / `matchmaker.go.tmpl` 的过渡改动；删除 `demo/game/playerroute/` 与 `activity_lease_test.go.tmpl`；codegen `demo.go`（清单增删、不再写 `game_route` 段、`activity.game_sids` 注释）、`demo_prod_config_promises_test.go`（检查 `singleton`、断言没有 `game_route`）、`render_dev_run.go`（注释与 second-game.sh 说明机器人要在该 sid 上建角色）；CHANGELOG `[Unreleased]` Changed 三条；GAME_DEMO_TEMPLATE §9.11.2 / §9.11.4 / activity 段加“已被取代”指向（完整新节在第 5 笔）。
- 先红后绿：新 API 先落骨架（`Serve` / `Admit` 返回 nil、`AdmitBound` 返回 false、`Resident` false、`CloseServedSessions` 0、`unloadIdle` 空、`expectedGameSIDs` 恒为自己、`startActivity` 不查 Live、`boundServerID` 恒为 `(0, true)`），新用例全部在断言上失败，例：`Serve(bound=2000) = <nil>, want player_elsewhere`、`a served player is not resident`、`background work for a player bound elsewhere left a resident record here`、`a write was admitted for a player this process does not serve: <nil>`、`closed 0 connections, want 2`、`Shutdown left served players connected: closed [], want 42 and 43`、`expected [1300], want exactly the live sids [1300 1302]`、`a Live query that failed produced an expected set`、`startActivity error "activity: game: capability \"service.global.activity\" not found; ..." does not name the missing capability`、`a login with no server_id reached the ownership table with [0]`、`the ownership table was asked about [0], want the session's bound sid 2000`。实现后 `go test -race -count=3` 通过，新用例 `-race -count=30` 稳定。
- 回归去向（逐条理由见提交说明）：静态绑定方案 §4.4 的“转写到闲置卸载 / 原样保留”各条全部转写（RR-20260920-10 / 11 / 12、RR-20260921-03 四个子测试合成一条加“等待受调用方 ctx 约束”、RR-20261004-11 两个子测试）；§7.3 的 9 条“转写到 sid 锁”由 app 包 singleton 回归承担（`TestSingletonNotHeldFailsOnceAndFencesBeforeShutdown`、`TestSingletonUnknownRenewals…`、`TestSingletonWaitsForTheHolderBeforeAnyModInit`、`TestSingletonWindowStartsWhenTheRenewalWasAsked`、`TestSingletonClaimsItsOwnValueAfterALostAcquireReply` / `…KeepsWaitingWhenALostReplyHidesAnotherHolder`、`TestValidateServiceConfigPinsSingletonTimeRelations`），RR-20260920-12 的“忙实体不拖续期”删除（续期在 App 的 goroutine 上）；RR-20260930-23 转写为 `service_shutdown_test` 的 `TestShutdownClosesTheConnectionsOfEveryServedPlayer`；§4.4 “删除（前提消失）”的 13 条删除；playerroute_test 十条随包删除（单键 CAS 语义由 `kit/redis` singleton integration 与 app 回归承担）；`activity_lease_test` 三条（RR-20260930-24）换成 activity 的 Live 三条 + 缺能力启动失败；`gift_handoff_test` 的 `TestAdmitRefusesWhenOwnershipCannotBeRead` 删除（准入不再读 Redis），`TestAHandoffDoesNotClaimAnUnownedPlayer` 在过渡期改为“不在本进程服务的发送方的步骤拒绝、不接入”。新增：Serve 三种结果、AdmitBound 只服务本服、`CloseServedSessions`、enter_game 缺 `server_id`（无会话 / 无 claim / 不可读 / 0）与绑定在别的服。
- 与方案的差异：
  1. WriteGate 的拒绝错误由 `ErrLeaseNotHeld` 改名 `ErrNotServedHere`（静态绑定方案 §4.2 写“保留名字、改文案”）：租约已不存在，名字会误导。仓库内无其他引用。
  2. `Serve` 自己返回 `errcode.Wrap(ErrPlayerElsewhere, …, "owner_sid", boundSID)`（服务包本来就引用 `internal/errors`），控制器只在 `server_id` 缺失时自己拒绝，`playerOwners` 接口只有 `Serve`（`SID` 留给第 4 笔的 `send_gift`）。卸载超出等待时返回包着 `context.DeadlineExceeded` 的错误，沿用 enter_game 现有的 `loginCutShort` 回 `login_timeout`，不新增错误码。
  3. `Service.Shutdown` 断开的是有驻留记录的玩家（只有经过 `Serve` 的玩家能在本进程做事）；其余连接仍随传输层 Mod 停止关闭。graceful 与 fail-stop 走同一路径。§5 原写“game-demo 在 `Service.Shutdown` 关闭全部连接”在本笔之前并不成立，本笔才实现。
  4. 闲置卸载逐个标记（先在锁外问连接数，再在锁内复核闲置后标记），不一次标记整批，也没有每轮人数上限：每人等待受 `evictWait` 约束，`Stop` 在人与人之间生效。`Fence` 设置器名字保留（现在只用于连接数与停机断开）。
  5. `startActivity` 第一步查 `app.ModSingleton`（先于其他能力），并要求 `server_type` 非空。
  6. 赠礼过渡用本地 `Admit`（发送方在本进程驻留才放行），没有用 `AdmitBound(From, SID())`：后者在多 sid 部署里会为绑定在别的服的玩家建记录、在本进程装载，形成两个写者。生产装配暂不装发送半边路由（`handoff=nil`，只拒绝、靠共享 durable 重投），`Router` 的路由类型换成 `sidRoute`，接收半边照常注册。代价：发送方不在任何进程驻留（离线且已卸载、或进程重启后）时，debit / refund 步骤要等他重新登录或到 saga 截止。
  7. second-game.sh 头部注明“玩家只在角色绑定的 sid 上服务，机器人用 `cmd/loadtest -server-id` 在该 sid 上建角色”（行为变化的直接后果，原脚本未提）。
- 验证（`GOWORK=off`）：`gofmt -l` 空；`go vet ./codegen/...`；`go test -count=1 ./codegen/...` 全绿（`codegen/internal/roost` 89s，未遇 `go_command_tree` 超时）；根包 `go test -count=1 .` 通过；干净 worktree（`f051e24a`）`go build ./...`、`go generate ./...` 后 porcelain 为空、根包与 codegen 复跑全绿；从该 worktree 生成 game-demo（`project new sdemo -template game-demo` + `go mod edit -replace`）`go build ./... && go vet ./...`、`go test -race -count=3 ./internal/service/game/ ./game/controllers/player/`、`go test ./...` 全绿；生成物无 `game/playerroute`、无 `activity_lease_test.go`、配置无 `game_route`。`git grep 'playerroute\|game_route\|LiveGames\|AcquireLease' demo codegen` 只剩 codegen 历史 CHANGELOG 与 prod 配置承诺测试里“不能再有 game_route”的断言及注释。
- 留给第 4 笔：`gift.State.FromSID` + `start_gift` 参数（重新生成 sender）、`admitPhase` / `runHandoff` 改 `AdmitBound(From, FromSID)`、`Router` 改按 sid 的静态解析器并在生产装配里装上发送半边、`gift_handoff_test` 迁移（恢复离线发送方在其绑定 sid 上执行）、控制器 `playerOwners` 加 `SID`。matchmaker 改用 `Resident` 已在本笔完成。第 3b 笔：kit `service/global` 租约 API 删除。第 5 笔：GAME_DEMO_TEMPLATE 新节、`render_access.go` 里 WriteGate 注释的“ownership lease”措辞、USER_GUIDE 补 game-demo activity 对 `Live` 的使用。
- 未验证：§8.2 真实进程演练（第 5 笔），包括两个 sid 各自机器人、崩溃重启后 activity 不再因租约冲突启动失败；多 sid 部署下赠礼过渡期的实际表现（只在单测与生成工程单测层面验证）。

### 第 3b 笔（2026-10-05，`refactor(kit)：删除 global 服务的租约 API`）

- 使用者核对：`rg 'AcquireLease|RenewLease|ReleaseLease|LiveGames|GameLease'` 在 `demo/`、`codegen/` 模板与代码里已无调用（第 3 笔 `f051e24a` 删掉了 activity 的租约），剩下的都在 `kit/service/global` 自身、`kit/service/integration` 测试与文档里。
- 范围：`kit/service/global` 删除 `AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames`、`sameLeaseBinding` / `checkLeaseBinding` / `cloneLease`、`randomIncarnation`；`types.go` 删除 `GameLease` / `LeaseState` 及其常量、`validateLoad` / `cloneLoad`、`MaxPageSize` / `MaxLoadEntries`；`Config.Leases` / `LeaseTTL` / `NewIncarnation`、`DefaultLeaseTTL`、`RedisStores.Leases`（`<prefix>:lease:` 键空间）删除；Mod 不再读 `global.lease_ttl`；`global_rpc.go` 的 `Routing` 只剩路由五个方法，`routing_rpc_gen.go` 用 `go generate`（`go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .`）重新生成，`routing_rpc_assembly_gen.go` 无变化；包注释、`server_run.go` 的“为什么没有周期性工作”改写为只讲路由。codegen `framework_services.go` 的 `global:` 段去掉 `lease_ttl: 30s`，Collabs 注释改写。`kit/service/integration`：`everyNamespace` 删除 `:global:lease:`，`TestGlobalRunsOnRedis` 的租约部分换成真实 Redis 上的迁移完成 + 二次 `Bind` 被拒 + `Resolve`，两处装配用例去掉 `AcquireLease`。
- 错误码：570105～570108（lease invalid / missing / not holder / expired）退役，按 `types.go` 里 570111～570124 的惯例删掉常量与哨兵、在常量块写注释说明、不复用。**与 §12 的差异**：570109（`ErrRangeInvalid`）一并退役——它唯一的产生方是 `LiveGames`，留下一个没有任何路径能产生的码正是 570111 当年被删的理由。`errcode_test.go` 的 `expectedCodes` / `segmentAllocated`（11 → 6）与“退役号不得复用”检查同步（570105～570109、570111）。
- 测试去向：只测租约的用例删除（`global_test.go` 11 条、`promises_test.go` 的 `contendedLeases` 替身与两条、`guards_promises_test.go` 的 `LiveGames` 一条、`rr_20260929_round2_test.go` 整个文件——RR-20260929 第二轮的两条租约回归）；混合用例去掉租约部分：守卫用例改名 `TestMigrationRequestsRefuseEachInvalidShape`、未知游戏服用例改名 `TestMigrationOperationsRefuseUnknownGames`，`TestRefusalsAndAcceptancesAreReported` / `TestANilReporterChangesNothing` 改为覆盖迁移完成与二次 `Bind` 的 `conflict:bind` 上报，`TestNewRejectsAnIncompleteConfig` 只剩缺路由 store。纯删除，没有先红后绿。
- 文档：CHANGELOG `[Unreleased]` Removed（破坏性变更与迁移）；kit service README 的包表、`run(ctx)` 表、上总线表（9 / 9 → 5 / 5）；USER_GUIDE 活性查询条补一句；GAME_DEMO_TEMPLATE activity 段的“已被取代”注记补“第 3b 笔删除”；本文 §7.2 末条标注已作废。README 包表里的单测 / 变异计数是抽取那一轮的快照（global 当时写 26，删除前实际 34 条、删除后 18 条），没有改。
- 验证（`GOWORK=off`，干净 worktree）：`gofmt -l` 空；`go build ./... && go vet ./...`（含 `-tags integration ./kit/service/...`）通过；`go test -race -count=1 ./kit/service/global/...` 通过；`go test -tags integration -count=1 -p 1 ./kit/service/global/... ./kit/service/integration/...` 在 `~/.roost-it` 的隔离 Redis 上通过（`REDIS_ADDR` 取该环境的地址，测试键前缀 `itest:<name>:<纳秒>`、用例结束删除）；`go test -count=1 ./codegen/...` 全绿；根包 `go test -count=1 .` 通过；`go generate ./...` 后 porcelain 为空；生成 game-demo（`project new sdemo3b -template game-demo` + `go mod edit -replace`）`go build ./... && go vet ./...` 通过、`go test ./internal/service/game/` 通过，生成的 `config.global*.yaml` 的 `global:` 段只有 `key_prefix`。
- 未验证：仓外调用方（按破坏性变更登记）；旧部署 Redis 里遗留的 `<global.key_prefix>:lease:*` 键不自动清理。

### 第 4 笔（2026-10-05，提交 `5bdac773`）

- 范围（静态绑定方案 §3.3，matchmaker 的 §3.4 已在第 3 笔完成）：`demo/game/gift/gift.go.tmpl`（`State.FromSID`，json `from_sid`；`Encode` 拒绝 `FromSID <= 0`，`Decode` 不要求，以便准入点名拒绝）；`demo/game/handler/start_gift.go.tmpl`（`handlerStartGift` 加参数 `fromSID int32`，写进状态；sender 由 `project new` / `roost generate` 从 `//roost:nest` 生成，仓库里没有提交的生成物，生成工程里的 `Sync_StartGift` 随之多一个 `fromSID int32` 参数）；`send_gift.go.tmpl` 传 `owners.SID()`，`controller.go.tmpl` 的 `playerOwners` 加 `SID()`（取不到驻留表时按 enter_game 的方式回错误码并记 Error）；`gift_saga.go.tmpl`：`playerOwnership` 只剩 `AdmitBound`，`admitPhase` 与 `runHandoff` 改为 `AdmitBound(From, FromSID)`，`giftStepHandoff` 带 `FromSID`，`giftHandoffRouter`（`ownerroute.Router[giftStepHandoff, int32, sidRoute]`，`KeyOf` = `cmd.FromSID`，静态解析器 `sidRoutes`：`GetRoute(sid) = (sidRoute{sid}, sid > 0, nil)`）由生产装配与测试共用，生产装配装上发送半边（取代 `handoff=nil`）；core `ownerroute` 未改。codegen `demo.go` 清单加 `send_gift_test.go`、更新 `gift_handoff_test.go` 说明；`demo/README.md` 赠礼节加“在哪个进程执行”；GAME_DEMO_TEMPLATE §9.12 加“部分取代”说明；CHANGELOG `[Unreleased]` Changed 一条。
- 准入语义：`FromSID == SID()` → `AdmitBound` 建立或刷新驻留记录（算一次使用）并放行，离线、没有副本的发送方也在其绑定 sid 上 debit / refund（Nest 慢池冷加载，驻留记录让闲置卸载之后收走副本），恢复了第 3 笔过渡期失去的执行；`FromSID != SID()` → 转交给 `FromSID` 后拒绝（nak），不建记录；本服但副本正在卸载 → 拒绝、不转交；`FromSID == 0` → 拒绝并记 Error（`gift saga: refusing a step whose payload names no sender sid`），不兜底。转交接收方：`AdmitBound` 返回非本服或卸载中 → 静默丢弃。核对第 3 笔警告：`PlayerOwners.AdmitBound` 只在 `boundSID == owners.sid` 时进入临界区建记录，`boundSID` 为 0 或别的 sid 直接返回 `(false, nil)`，不会为绑定在别的服的玩家建驻留记录；`NewPlayerOwners` 要求 sid > 0，所以 0 永远不是本服。
- 先红后绿：骨架（`State.FromSID` / `giftStepHandoff.FromSID` / `giftHandoffRouter` 已落，`handlerStartGift` 有参数但 `send_gift` 传 0，`Encode` 不查 sid，准入与接收方仍是第 3 笔的本地 `Admit` / `Resident`、`handOver` 不带 `FromSID`）下，生成工程新用例在断言上失败：
  - `TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo/debit`（refund 同）：`the debit of an offline sender bound to this sid was refused: gift saga: player 42 is not served by this process; leaving the step: player owners: this process is not serving the player right now: player 42 has not been taken into service here`
  - `TestAdmitRefusesAndHandsOverAForeignStep`：`handed over to [], want the sender's sid 1000`；`TestAHandoffIsAddressedToTheSendersBoundSid`：`handed over to [], want [1000 1002] (each sender's own sid)`
  - `TestAStepThatNamesNoSenderSidIsRefused`：`gift.Encode wrote a gift with no sender sid`；去掉这条前置断言后：`a step whose payload names no sender sid was admitted`（发送方在本进程驻留时，旧准入放行了无 sid 的载荷）
  - `TestAHandoffForAnOfflineSenderBoundHereIsRun`：`a handoff for an offline sender bound here = <nil>, want it taken on (the reservation reached)`
  - `TestAHandoffForAPlayerWeDoNotOwnIsDropped`：`no sender sid: a handoff for a sender not served here should be dropped quietly, got gift saga: handoff reserve: saga: invalid record`
  - `TestAHandoffWhoseEnvelopeDisagreesWithItsPayloadIsRefused`：`another player: a handoff whose envelope disagrees with its payload = <nil>, want a refusal before the claim`
  - `TestSendGiftWritesThisProcessSidAsTheSendersSid`：`StartGift got sender 4242 on sid 0, want 4242 on this process's sid 1300`
  - `TestAHandoffWithAnUnknownPhaseIsRefusedBeforeTheClaim` 也红（`= <nil>, want a refusal before the claim`），但这是夹具变化（真实空驻留表下旧接收方先按 `Resident` 丢弃）造成的，不是新行为；`TestAStepDoesNotClaimASenderBoundElsewhere`、`TestAStepForASenderBeingUnloadedIsRefusedWithoutAHandoff` 在骨架上即绿（旧准入同样不接入、卸载中同样拒绝），作为守护保留。
  实现后全部通过，新用例 `-race -count=30` 稳定。
- 回归去向：`TestAStepForAPlayerNobodyServesIsRefusedWithoutTakingThem`（第 3 笔对 `TestAHandoffDoesNotClaimAnUnownedPlayer` 的过渡反转）删除，原意恢复为两条：`TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo`（离线发送方在其绑定 sid 执行、建驻留记录）与 `TestAStepDoesNotClaimASenderBoundElsewhere`（绑定在别的服的发送方不在本进程接入）；新增转交按 `FromSID` 路由、`FromSID=0` 拒绝、卸载中拒绝不转交、接收端离线发送方执行 / 非本服与无 sid 静默丢弃、信封与载荷不一致拒绝、`send_gift` 传本进程 sid。准入与接收方测试改用真实 `PlayerOwners`（`newPlayerOwners`）与生产路由器 `giftHandoffRouter`，只替身传输。
- 与方案的差异：
  1. 转交接收方先解码载荷，要求信封的 `PlayerID` / `FromSID` 与载荷的 `From` / `FromSID` 一致，否则在接入与认领之前拒绝（方案只写“`AdmitBound(cmd.PlayerID, cmd.FromSID)`”）：接收方原来检查信封里的玩家、执行载荷里的玩家，两者不一致时检查就落空；现在按载荷判定、拒绝不一致的信封。执行用已解码的状态建实体 id，不再在认领之后第二次解码。
  2. `gift.Encode` 拒绝 `FromSID <= 0`（发起时就失败，`start_gift` 回内部错误），`Decode` 不要求，让步骤准入能把它和“解不开的载荷”区分开、按名拒绝并记 Error（解不开的载荷仍按原规则放行给 handler 处理）。
  3. `send_gift` 取不到驻留表时回错误码（与 enter_game 同样的处理），不发起赠礼。
- 验证（`GOWORK=off`，独立 worktree）：`gofmt -l` 空；`go build ./...`；`go vet ./codegen/...`；`go test -count=1 ./codegen/...` 全绿（`codegen/internal/roost` 84s，未遇 `go_command_tree` 超时）；根包 `go test -count=1 .` 通过；`go generate ./...` 后 porcelain 只有本笔改动；生成 game-demo（`project new sdemo4 -template game-demo` + `go mod edit -replace` 指向本 worktree）`Sync_StartGift(ctx, id, fromPlayerID int64, fromSID int32, …)`，`go build ./... && go vet ./...`、`go test -race -count=3 ./internal/service/game/ ./game/controllers/player/`、`go test ./...` 全绿。生成工程没有需要真实 Mongo / NATS 的赠礼 integration 用例，未起隔离环境。
- 未验证：多 sid 部署下的真实转交（两个 game 进程、bus 寻址 `<prefix>.svc.<game>.<sid>`、离线发送方在其 sid 上冷加载执行 debit / refund），属第 5 笔 §8.2 演练；`second-game.sh` 两进程实跑未做。

### 第 5 笔（2026-10-05，文档 + 真实进程演练）

- 范围：§8.2 真实进程演练；文档——GAME_DEMO_TEMPLATE 新节 §9.15（取代 §9.11.2 所有权表，§9.8.2 / §9.12 的取代注记压成指向）、`render_access.go` 的 WriteGate 注释去掉 “ownership lease”、USER_GUIDE（启动等待的精确表述、game-demo activity 对 `Live` 的使用、etcd Discovery 与 `App.Live` 的分工）与 `etcd/driver/discovery.go` / `kit/etcd/etcd_mod.go` 注释、bug / bugfix 索引与交接文档里玩家租约相关 RR 的“已被取代”标注（另含 RR-20260920-03 / 04、RR-20260929-13 / 17：前两条是 `playerroute` / 租约失效写入，后两条是第 3b 笔删掉的 global 租约 API；RR-20260928-06 只有 `game_route` 一部分失效，未标）、交接文档 2026-10-05 状态、TROUBLESHOOTING T-212 / T-213 按实际日志改写。演练中发现一处代码缺陷，单独一笔修复：`fix(codegen)：生成的 etcd.service_prefix 带结尾斜杠`。
- 环境：`~/.roost-it/roost-dataengine-it`（端口偏移 1000）；自起一个 etcd（127.0.0.1:23791，数据在 scratchpad）。生成 `roost project new drill5 -template game-demo` + `go mod edit -replace` 指向本 worktree（基线 `64acd782`，未含 `c493a791`）。所有 Mongo 库名、`nats.prefix`、syncbus / effects / saga 的主题与流、durable、Redis `key_prefix`（含 `singleton.key_prefix`、`snapshot_l2_key_prefix`）、`etcd.service_prefix` 都换成 `drill5_202610051039`；九个框架服务各起一个（sid 1000），`accountctl upsert-server` 注册 1300 / 1302。game 1300 与 1302 各自的 WAL 目录、ops / 客户端端口。
- **隔离的教训**：DAO 的库名是 `db/def` 编译期常量（生成为 `"game"`，`db/gen_*_dao.go` 的 `*DaoDBName`），不受 `dataengine.database` 配置影响。第一次预演只改了配置，Player / Guild / World 的投影写进了共享的 `game` 库（oplog 核对：`game.player` / `guild` / `world` / `_guild_id_sequence` 四个集合由本次预演创建，`game.players` 是 10-04 已有的）。正式演练前把三个 `*DaoDBName` 改成带后缀的库名、换新前缀重来；正式演练开始后 oplog 里除 `config`（事务会话簿记）与 noop 外只有 `drill5_202610051039_*` 三个库。以后在共享环境跑 game-demo 演练，要连 `db/def` 一起改。
- 时间线（`ttl / renew_interval / guard / startup_wait = 15s / 3s / 5s / 30s`；P* 都是 sid 1300、同一配置与 WAL 目录）：

| 步骤 | 动作 | 观测 | 耗时 |
| --- | --- | --- | --- |
| 0 | 起 P1；机器人 A、A2 各 10 个 | 都 `success=10`；Mongo 20 个玩家 | P1 启动到 `/readyz` 1.6s |
| 1 | SIGSTOP P1（键剩 13.2s）→ 起 P2 | P2 只有 `singleton: acquiring` / `waiting … holder=<P1 token>|<host>|43917|…`，没有任何 `mod init`；`acquired` 后 Mod 启动，ops 报端口占用，DataEngine 报 `nestwal: directory is already locked`，逆序停完后 `singleton: released`，退出码 1；键随即不存在 | 等锁 15.004s，拿锁到退出 0.22s |
| 2 | SIGCONT P1 | 下一拍续期 NotHeld：`singleton lock lost; fail-stop`（holder 为空）→ `runtime infrastructure failure` → `service shutdown` → Mod 逆序停 → `singleton: lock was lost; not releasing` → 退出码 1；全程无 `fatal projection version conflict` | SIGCONT 到判定失锁 10ms，到退出 156ms |
| 3 | 再起 P2 | 立即 `acquired`，重放 WAL、就绪；Mongo 20 个玩家与 SIGSTOP 时快照逐字段相同；机器人 B 10 个 `success=10`，Mongo 30 个玩家状态完全一致；60 个 saga 全部终结（30 completed / 30 compensated） | 启动到就绪 0.26s |
| 4 | kill -9 P2（键剩 13.3s）→ 立刻起 P3 | 等待后 `acquired`，activity 正常启动、`service init` 正常（没有租约冲突） | 等锁 15.005s，启动到就绪 15.1s |
| 5 | SIGTERM P3 → 40ms 后起 P4 | P3 66ms 停完并 `released`；P4 在 P3 释放之前就开始获取，于是在下一拍拿到 | P4 等锁 3.0s |
| 5b | SIGTERM P4，**等它退出**再起 P5 | P5 立即 `acquired` | 等锁 0ms，P4 停机 0.35s |
| 1b/2b/3b | 机器人跑在 P5 上（`player_tcp_connections 8`、`wal_unacked=1`）时 SIGSTOP → P6 → SIGCONT → P7 | 与 1～3 相同；P7 就绪后 `wal_unacked=0`，Mongo 40 个玩家与 SIGSTOP 时快照相同；P5 上的机器人 8 个在战斗阶段超时失败（进程被停），之前的写入都在 | P6 等锁 15.004s；SIGCONT 到判定失锁 4ms、到退出 62ms |
| 1c/2c | 4 个只 `enter_game` 后空闲的机器人连在 P7 上，SIGSTOP → P8 → SIGCONT | P7 失锁后 `game: disconnected the players this process served sessions_closed=4` | P8 等锁 15.006s |
| 6 | 起 P9（1300）与 Q1（1302） | 两个键各自持有；Q1 activity 的 `Live` 给出 `[1302, 1300]` | 都 < 0.2s 就绪 |
| 6a | 两个 sid 各 10 个机器人 | 都 `success=10`；P9 46 次 `step left to the sender's sid` / 14 次 `ran a step handed over by another process`，Q1 45 / 16；120 个 saga 全部终结 | — |
| 6b | 1300 上 10 个机器人赠礼进行中 kill -9 P9 → 立刻起 P10 | kill 时 7 个 saga 在途（`status=2` Waiting，debit / refund 阶段）；P10 上线后（`player_tcp_connections 0`，发送方全部离线）Q1 按 `FromSID` 转交、P10 执行，7 个全部 compensated（`attempt=0`），没有 `manual_required` | kill 到 P10 就绪 15.87s；最后一个在途 saga 在 P10 就绪后 0.55s 终结 |

- 结论：§8.2 六步全部符合预期，等待时长与参数一致。kill -9 / SIGSTOP 之后“立刻”起新进程实测都是 15.0s：获取按 `启动 + k × renew_interval` 重试，键在最后一次续期后 `ttl` 过期，而最后一次续期在停之前 0～3s，过期时刻落在 `(启动+12s, 启动+15s]`，于是总在 `启动+15s` 那一拍拿到；§8.2 第 4 步写的“约 18s”是上界 `ttl + renew_interval`，新进程晚于停机启动时等待相应缩短。跨服赠礼按 `FromSID` 转交、离线发送方在其绑定 sid 上执行都在真实进程上成立。
- 与方案预期的偏差（都不是代码缺陷，已写进文档；更正：第 3 条是代码缺陷，已修，见该条）：
  1. §8.2 第 5 步“SIGTERM 后立即拉起新进程不等待”只在旧进程**已经停完**时成立；旧进程还在停机时新进程等到释放后的下一拍（≤ `renew_interval`，实测 3.0s）——重试不监听删除。USER_GUIDE 与 T-212 已改为精确表述。
  2. §8.2 第 2 步期望“日志里有 Nest 被围栏”：`NestMgr.Fence` 不打日志，可见的只有 `runtime infrastructure failure`（`OnFail` 回调在它之前同步执行完）；连接关闭可见（`disconnected the players this process served`，前提是失锁时还有服务中的连接——1b 那次机器人在暂停期间已超时断开，没有这条）。
  3. P1 暂停超过 `etcd.lease_ttl`（10s）后恢复：`etcd Discovery: lease lost`，停机时 `mod etcd stop: etcd Discovery: revoke: etcdserver: requested lease not found` 记为 Mod 停机失败并并入退出错误。不影响 Release（只有超时算停机不完整）；同样的情况发生在一次正常停机里会让退出码变成非零。未修，记为观察。**已修（`b67d5945`）**：这其实是代码缺陷——租约已不存在时键已随租约删除，注销已经达成。`etcd/driver/discovery.go` 的 Revoke 路径把 `rpctypes.ErrLeaseNotFound`（及未转换 / 被包裹的 gRPC NotFound 同描述）视为注销已达成、记 Info，其他错误照旧报告；`Deregister` 在注册循环退出后再取消一次 keepalive（停机期间才完成的重注册不再留下续期中的 keepalive）。kit `EtcdMod` 停机的错误只来自 `Assembly.Close`，无需另改。“不影响 Release”已用 `TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError` 钉住；真机回归 `TestRealEtcdCloseAfterLeaseVanishedIsClean`（`-tags integration`，修前红文本与演练日志相同）。
- 观察（未改）：
  - 两个 sid 同时跑时，demo 场景的成本 p95 落进 16.384s 的桶，超过 loadtest 缺省 `-max-p95 16`，两边都 `rc=1`（机器人全部成功）：赠礼步骤落到非发送方进程要弹一两次才转交到位。单 sid 时整轮 7.4s。
  - 开窗是先写者赢：P9 先以 `[1300]` 打开 `race-1791168300`，Q1 随后日志 `activity: window open … expected_game_sids [1302, 1300]` 是它自己算的，协调器记录仍是 `[1300]`。日志易误读，已写进 GAME_DEMO_TEMPLATE §9.15.3。
  - cobra 在 `server exit` 之后打印 `Usage:`，与运行期错误无关的噪音。
- 演练产物：日志、时间线、玩家快照、生成工程在 scratchpad `c5-drill/`（不进仓库）。
- 清理：演练进程、etcd 均已停止。Mongo 库（`drill5_202610051033_*` 预演、`drill5_202610051039_*` 正式各 3 个）、JetStream 流（各 3 个 `DRILL5_…`）、Redis 键（`drill5_20261005103*:*`）以及预演误写进共享 `game` 库的四个集合，删除操作被本机的自动权限分类器拦下，**未删除**，交由维护者处理（命令见第 5 笔报告）。**更正（2026-10-05）**：维护者授权后已全部删除——6 个 `drill5_*` Mongo 库 dropDatabase；共享 `game` 库的 `player` / `guild` / `world` / `_guild_id_sequence` 四个集合 drop（删除前用 oplog 核对：四者的全部写入都在 10-05 02:35 UTC 预演窗口内，oplog 覆盖自 10-04 起；`game.players` 保留，仍 2 条）；6 个 `DRILL5_*` JetStream 流删除；Redis `drill5_20261005103*` 共 1240 个键 UNLINK。复查：无 `drill5_*` 库、流、键残留。
- 验证（`GOWORK=off`）：见提交说明。
- 未验证：Redis Cluster 下的真实进程演练；跨主机 / 换卷（不在范围内）；`c493a791` 与 obs34 的补偿预算调整之后的代码没有重跑演练（6b 在 `64acd782` 上测）。

### 第 3 / 3b / 4 笔审查收尾（2026-10-05，obs34）

审查第 3、3b、4 笔时的观察项，逐条处理（提交按标题，rebase 后提交号以 `git log` 为准）：

1. **两条测试承诺没有被钉住**（`test(demo)：钉住闲置卸载的锁内复核与“删记录先于关闭 done”`）：审查的两处变异——删掉 `unloadOne` 锁内对 `lastUsed` 的复核；把 `runEviction` 删除驻留记录挪到 `close(done)` 之后的另一个临界区——变异后整包仍绿。新增 `TestAUseBetweenTheSessionCheckAndTheMarkCancelsTheUnload`（fencer 的 `ActiveSessions` 回调里 `Serve` / `AdmitBound` 一次，断言不卸载）与 `TestALoginDuringAnUnloadEndsServed/the-record-is-gone-before-the-waiters-wake`（未导出测试缝 `PlayerOwners.afterUnloadDone`，`close(done)` 之后调用，测试在本 goroutine 上驱动 `runEviction`、在钩子里等醒来的 `Serve` 回答）。两处变异分别使新用例红：`a player used between the session check and the mark was unloaded: dropped=[77]`、`the unload deleted the record the login created after it woke`（同一变异下原 `ends-served` `-count=20` 仍绿）。
2. **认证写入的 claim 键没有测试**（`fix(demo)：认证写入与登录读取共用 server_id claim 键，补两条登录用例`）：键抽成 `kit/service/account.ServerIDClaim`（传输包与控制器包都已依赖 account 包，不引入两者之间的依赖），控制器的读取函数导出为 `BoundServerID`；新增生成工程 `internal/access/player/tcp/auth_test.go`（真实认证器产出的 Principal 被 `BoundServerID` 读出 sid），以及 controller 层“`Serve` 等卸载超时 → `login_timeout`”用例。变异：认证器改写别的键 → `read (0, false) … want the role's server 1300`；去掉 serve 一步的 `loginCutShort` → `code=1 reason="server error", want login_timeout`。
3. **发送方的 sid 宕机期间退款可能转为 manual_required**（`fix(demo)：赠礼退款的重试预算覆盖发送方 sid 的一次崩溃重启`）：`saga.Step` 的预算正反方向共用、没有按方向的选项，按“不新增机制”把生成的 `saga/gift_item/definition.go` 里 debit 步骤（补偿即退款）`MaxAttempts` 5 → 15（codegen demo 清单的 run 步骤 `demoGiftRefundBudget` 改写 `add saga` 的输出），Timeout 5s、退避 100ms..5s 不变。重试窗口下界 = 15 × 5s + 14 次退避的抖动下界（`d` 从 100ms 翻倍封顶 5s，取 `d/2`：0.05 + 0.1 + 0.2 + 0.4 + 0.8 + 1.6 + 8 × 2.5 = 23.15s）≈ 98.15s，上界 75 + 46.3 = 121.3s；要求 `startup_wait` 30s + `ttl` 15s + 拉起与 Init 余量 45s = 90s（原预算下界 25.75s）。debit 正向随之也是 15 次：扣款失败什么都没扣，只是更晚判失败（仍受 `gift.Deadline` 2 分钟约束）。生成工程测试 `gift_saga_budget_test.go` 按生成的 singleton 配置钉住关系，改预算前红：`a refund gets at least 25.75s of retries … = 1m30s; past that the refund is marked manual_required`。CHANGELOG 加 Changed 一条，并补 `c493a791` 的 Fixed。
4. **过时注释**（`docs(kit)：global/activity 注释不再说 global 负责存活`）：`kit/service/global/activity/service.go` 的 `Service` 注释与 `activity/types.go` 的包说明改为现状：global 只做路由分组，存活由 App 单实例锁的 `Live` 提供。

观察，未改：
- **冷加载超过 `IdleUnload` 后实体留在内存、没有驻留记录**：登录 `Serve` 建了记录之后，冷加载（例如等投影屏障）若比 `IdleUnload`（5 分钟）还久，期间登录已回 `login_timeout`、连接可能已断，闲置卸载会选中这条记录；`playerEvictor` 发现内存里还没有实体，按“已经不在”成功返回，记录被删。加载随后完成，实体留在 EntityManager 里却没有驻留记录，闲置卸载只扫记录、不会再收走它。只占内存：没有记录时 WriteGate 照常拒绝，下一次 `Serve` / `AdmitBound` 重新建记录后照常服务、照常闲置卸载。
- **`Destroy` 永不返回时泄漏**（RR-20261004-11 遗留）：卸载在自己的 goroutine 上跑到结束，等待方只受 `evictBudget` 约束；若 `EntityManager.Destroy` 永远等不到实体锁（实体上的事务永不结束），这个 goroutine 与 `evictions` 里的条目一直留着，该玩家此后 `Admit` / `AdmitBound` 一直拒绝、`Serve` 每次等满预算后回 `login_timeout`，直到进程重启。根因在实体上永不结束的事务，不在驻留表，维持现状。
- **优雅停机时先断会话、listener 仍开着**：`Service.Shutdown` 第一步 `CloseServedSessions` 断开服务中的玩家，但传输层 Mod 要到后面逆序停止时才关 listener；这段时间里立即重连的客户端可能在本进程再登录一次（`Serve` 建记录、装载），随后在传输层停止时再被断开、到接替的进程重登。只是多一次重登，不形成两个写者（同一 sid 只有本进程持锁，接替进程要等本进程释放），维持现状。

验证（`GOWORK=off`，独立 worktree，rebase 到 `10e2e0ea` 之后）：`gofmt -l` 空；`go build ./... && go vet ./...` 通过；`go generate ./...` 后 porcelain 只有本节文档；`go test -count=1 ./codegen/...` 全绿（`codegen/internal/roost` 87s）；根包 `go test -count=1 .` 通过。生成 game-demo（`project new sobs -template game-demo` + `go mod edit -replace` 指向本 worktree）：`go build ./... && go vet ./...`、`go test -race -count=3 ./internal/service/game/ ./game/controllers/player/ ./internal/access/...`、`go test ./...` 全绿，新用例 `-race -count=50` 稳定。没有重跑真实进程演练。
