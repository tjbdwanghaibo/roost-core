# 01 app 与生命周期（说明）

> 配套实现文档：[impl/01-app-lifecycle.md](../impl/01-app-lifecycle.md)（代码结构、时序图、不变量与守卫测试、review 检查点）。
> 源码基准：tag `v1.23.0`（`28912cd6`）。文中 `path:line` 都按这个 tag。codebase-memory 图谱的代际是 2026-09-23，`app/singleton.go`、`internal/*` 等文件不在图谱里，本篇全部结论按 tag 源码直接读取。

## 速览

- **这一块是什么**：进程的骨架。`app.App` 把一个二进制里的多个服务类型（`game`、`account`……）做成 CLI 子命令；每次启动只跑一个服务类型，按固定顺序完成「读配置并检查 → 单实例锁 → 共享 Mod → 服务专属 Mod → Service → 等信号 → 逆序停机」。
- **最重要的保证**：配置在任何 Mod `Init` 之前按本服务全部声明一次检查完；同一服务类型 + sid 同时只有一个进程在跑 Mod（开了 `singleton.enabled` 时）；任何 fail-stop（失锁、DataEngine fatal、Remote fatal）先围栏 Nest 再走同一条停机路径，并让进程非零退出；停机按「三步停机」处理，超预算时如实报错、**保留依赖与锁**，不在还在跑的组件底下拆资源。
- **最容易踩的坑**：① `DependsOn` 写的是 **Mod 名**，不是 capability 名（写 `health`、`bus`、`nats.jetstream` 会在启动时报 `unknown mod dependency`）；② `Service.Shutdown` 必须容忍部分初始化，并且受 ctx 约束——它在 5s / 停机总预算内没结束，App 就不停 Mod、不释放锁；③ 拿到单实例锁之后、`Service.Init` 结束之前，进程**还没注册 SIGTERM 处理**，这段时间收到 SIGTERM 按默认处置直接退出（键在 TTL 内过期）；④ 手写 Mod 实现 `StopBudget` 不会被生成器算进 `shutdown.total_timeout`，要手工调大；⑤ 启动失败时 Mod 的 `Stop` 可能在「Provide 过、没 Start」的状态下被调用，`Init` 失败时同段已 Init 的 Mod 则收不到 Stop（§4.3）。

### 本篇覆盖的包

| 包 | 职责 |
| --- | --- |
| `app` | `App`（CLI、启动 / 停机编排）、`Mod` / `Service` / `IManager` 接口、`Registry`、`RuntimeFailure`、单实例锁、配置声明入口（`SchemaOf` / `LoadConfig` / `CheckConfig`）、停机预算规划 |
| `app/buildinfo` | 链接期注入的版本信息（`buildinfo.VersionString()`，生成的 bootstrap 传给 `app.New`） |
| `lifecycle` | 生命周期 hook 注册表（`app.init`、`mods.started`、`service.started`、`service.stopping`、`service.stopped`、`config.reload`）；无 ctx 停止入口的 `ManagerGroup`（仓内无生产调用方） |
| `manager` | 服务级单例 manager 的生命周期引擎 `Engine`：依赖序启动、逆序停止、启动中可被停机中止、三步停机 |
| `kit/manager` | `ManagerMod`：把 `manager.Engine` 包成一个 `app.Mod`，登记在 `mods.ModManager` 下 |
| `kit/mods` | kit 全部 Mod 名 / capability 名常量表、`RegisterAll`（先预检再整批登记）、持久化引擎选择、服务 Mod 共用的小工具 |
| `kit/ops` | ops 端点（`/healthz`、`/readyz`、`/metrics`、`/statsz`、`/admin`）；本篇只讲 readiness 与生命周期的接线，admin / 指标见 11 分区 |
| `health` | 健康检查注册表：并发调用 checker、每个 checker 1.5s 期限、Degraded 算可用 |
| `internal/operation` | 停机对象共用类型：`Lifetime`（准入 + 在途计数 + 等排空）、`Serial`（停止入口串行，等待受各自 ctx 约束） |
| `internal/stopcontract` | 只给测试用的「三步停机」契约骨架 `Check` |
| `internal/configschema` | 配置声明（tag → `Schema`）、解码与检查、合并、YAML 输出、源码守卫。本篇只讲 app 侧的装配与检查时机，规则本身见 07 分区 |

跨分区：配置规则（tag 语义、严格读、热更）在 [07 配置](07-config.md)；业务时钟、业务时间高水位在 [10 时间](10-time.md)；指标、日志、admin、仪表盘在 [11 可观测](11-observability.md)；`//roost:register` 生成器、bootstrap 模板、停机预算的生成公式在 [12 代码生成](12-codegen.md)。

---

## 1. 定位与边界

**负责**

- 进程入口与 CLI：每个 `RegisterServer` 注册一个子命令，带 `--check-config` / `--print-config` / `--print-config-schema`；根命令带 `-c/--config`、`--sid`（`app/app.go:83`、`app/app.go:116`）。
- 启动编排：读配置并按声明检查、初始化日志、建 Registry、拿单实例锁、业务时间守卫、按依赖序 Init / Provide / Start 全部 Mod、`Service.Init`、`Service.Serve`。
- 停机编排：等信号 / fail-stop / Serve 退出 → `service.stopping` hook → `Service.Shutdown` → 逆序停服务专属 Mod → 逆序停共享 Mod → 释放单实例锁 → `service.stopped` hook。总时长受 `shutdown.total_timeout` 约束，按 Mod 的声明预算分配。
- 进程级统一保证（维护者原则：单实例、活性、fail-stop 由 core app 一次提供，模块继承）：单实例锁、只读活性查询 `Live`、`RuntimeFailure` fail-stop、退出原因写进日志、非零退出码。
- 能力注册表 `Registry`：Mod 在 Provide 时登记能力，Service 与后面的 Mod 用 `app.Lookup` 取。
- manager 引擎：服务内存单例（场景表、路由表等）的依赖序启动 / 逆序停止。

**不负责**

- 具体基础设施（Redis、Mongo、NATS、DataEngine、Nest 等）——它们是 kit 里的 Mod，各自的分区讲。
- 配置键的语义与规则——每个键由读它的 Mod 声明；App 只负责「什么时候、按哪些声明检查」。
- 业务的停机细节——App 只给 `Service.Shutdown` 一个带期限的 ctx，并在它不完整时保留依赖。
- 多进程的分布式协调——单实例锁只处理「同一 sid 崩溃重启时短暂出现两个进程」这一个场景（[方案 §1](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)），不做选主、不做 DataEngine 层的 fencing token。

→ [实现文档](../impl/01-app-lifecycle.md)对应：§1 包与文件地图。

## 2. 核心概念与术语

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| App | 顶层容器。一个二进制一个 App，含多个服务类型 | `app/app.go:28` |
| Mod | 基础设施模块。生命周期 `Init(cfg) → Provide(registry) → Start() → Stop()`（停机逆序） | `app/mod.go:12` |
| 共享 Mod / 服务专属 Mod | `App.Mods(...)` 注册的对所有服务类型生效；`RegisterServer(type, svc, mods...)` 的只在该服务类型启动时生效。共享 Mod 先于服务专属 Mod 启动、后于它们停止 | `app/app.go:76`、`app/app.go:83` |
| Service | 业务主体。`Init(registry) → Serve(ctx)（阻塞）→ Shutdown(ctx)`。一个进程只跑一个 | `app/service.go:12` |
| Registry | 能力表：`ModName → any`。启动时预置 7 项内建能力（health、metrics、admin、admin.metadata、lifecycle、runtime.failure、clock.business） | `app/registry.go:29` |
| capability（能力名） | Registry 的键，类型 `app.ModName`。常和 Mod 名相同，但一个 Mod 可以登记多个能力（例如 NATS Mod 登记 `nats`、`nats.rpc`、`nats.jetstream`、`bus`） | `kit/mods/name.go:6` |
| DependsOn / OptionalDependsOn | Mod 的硬依赖 / 存在时才排序的可选依赖，**写 Mod 名** | `app/mod.go:36`、`app/mod.go:45` |
| StopWithContext / StopBudget | Mod 的有界停止入口；声明自己需要的固定停机时长 | `app/mod.go:20`、`app/mod.go:30` |
| IManager / Engine / ManagerMod | 服务内存单例 / 驱动它们的引擎 / 把引擎包成 Mod | `app/manager.go:9`、`manager/engine.go:44`、`kit/manager/manager_mod.go:38` |
| lifecycle hook | 在 `app.init`、`mods.started`、`service.started`、`service.stopping`、`service.stopped`、`config.reload` 阶段被调用的回调 | `lifecycle/lifecycle.go:13` |
| RuntimeFailure / fail-stop | 进程级「不能再安全写」信号。第一次 `Fail` 同步执行全部 `OnFail` 回调（Nest 围栏）再唤醒停机 | `app/runtime_failure.go:20` |
| 单实例锁（singleton） | `<key_prefix>:<server_type>:<sid>` 一个 Redis 键，任何 Mod Init 之前获取、持有期间续期、全部 Mod 停完才释放 | `app/singleton.go:22` |
| Live（活性） | 只读查询「这些 sid 里哪些有进程持有锁」；停机中的进程仍算活（维护者决定 C5） | `app/singleton.go:69` |
| SingletonIncarnation | 本进程持有的锁的身份（键、sid、本次启动 token），模块据此接管上一代进程留下的按 sid 协调状态 | `app/singleton.go:80` |
| 三步停机 | ①发起关闭（幂等）→ ②在调用方 ctx 内等真实排空（超时返回 ctx 错误、保留对象、可重试）→ ③排空后才释放依赖 | `docs/agent-skills/roost-coding/SKILL.md` 生命周期复审要点；`internal/operation/lifetime.go:1` |
| 停机不完整 | 某一步因 ctx 取消 / 超时没停完。App 的统一处理：不再停它之后（更底层）的 Mod、不释放单实例锁 | `app/app.go:697` |
| 配置声明（ConfigSchema） | Mod / Service 读的键写成带 tag 的结构体，`app.SchemaOf` 得到声明，`app.LoadConfig` 读 | `app/config_schema.go:43`、`app/config_schema.go:64`、`app/config_schema.go:68` |
| readiness 就绪位 | ops Mod 的一个布尔：`service.started` 置真、`service.stopping` 置假；`/readyz` = 就绪位 ∧ 没有 checker 为 Fail | `kit/ops/ops_mod.go:154`、`kit/ops/ops_mod.go:284` |
| 静态注册 | 不需要 Registry、不做 I/O 的注册（实体 builder、组件、配置表、nest handler），由生成的 `registry.RegisterAll()` 在 `app.New` **之前**执行一次 | `docs/STATIC_REGISTRATION.md`；`codegen/internal/roost/render.go:394` |

→ [实现文档](../impl/01-app-lifecycle.md)对应：§2 关键类型与数据结构。

## 3. 设计原因

| 取舍 | 结论 | 出处 |
| --- | --- | --- |
| 单例、活性、fail-stop 放在哪 | 放在 core `app`，一次实现，模块继承、不感知锁、不各自检查。只有 App 能做到「在所有 Mod 之前」拿锁，也只有 App 持有停机顺序 | [单实例锁方案 §0、§2](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)；维护者 2026-10-05 决定 1～4 |
| 失锁怎么处理 | 失锁即 fail-stop（D-A），不做「窗口耗尽先停准入」；Nest 围栏由 `RuntimeFailure.OnFail` 统一触发，不由各模块自己找 Nest | 方案 §3.4、§5 |
| 为什么不要 DataEngine fencing token | 崩溃重启的两个进程用同一个 WAL 目录，`nestwal` 的 `flock` 已保证同一时刻一个写者；新进程先重放旧 WAL | 方案 §4 |
| 停机超时之后怎么办 | 如实返回 ctx 错误、保留对象和它的依赖，不在过期 ctx 下继续停依赖；重试会再等。「字段已清空 / once 已执行 / 列表已取走」不算已停 | roost-coding「生命周期与装配的复审要点」；A3 决定（同一不变量被打破 11 次后抽出共用类型 `internal/operation` 与契约骨架 `internal/stopcontract`，[A3 记录](../../feature/REFACTOR-2026-10-05-shared-stop-contract.md)） |
| 停机预算怎么分 | 一个总预算 `shutdown.total_timeout`；声明了 `StopBudget` 的 Mod 优先拿声明值，未声明的每个先留 3s 保底；不够时声明值按比例缩放并告警；连保底都给不起时全部均分 | RR-20260926-42、RR-20260926-51（维护者批准），`app/app.go:737` 注释 |
| 停机中的进程算不算「活」 | 算，直到 Release 删键。活性只有一个事实来源（锁本身），不引入「停机中」中间值 | 维护者决定 C5（`app/singleton.go:63`） |
| Degraded 算不算就绪 | 算。`/readyz` 只在有 checker 为 Fail 或就绪位为假时 503；Degraded 项列在响应体 `degraded_dependencies` | 维护者决定 D1（[D1 方案](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)） |
| 每个 checker 一个期限 | 1.5s（k8s 探针 `timeoutSeconds: 2`），到期报 Fail，不拖住整个 `/readyz`；同一 checker 同时最多一次调用 | 维护者第十二轮决定（[R12 §3](../../feature/DECISIONS-R12-KIT-2026-10-06.md)） |
| 配置怎么检查 | 每个 Mod / Service 声明自己读的键（A4 ①）；App 在任何 Mod Init 之前把 App 自己、本服务全部 Mod、服务本身的声明合并检查一次，**错误一次报全**；同一个键被两处声明得不一样也报错。生成器与 doctor 用同一份声明 | [A4 ① 方案](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)；维护者配置原则「规则只在运行时加载时强制一次」 |
| 为什么拼错的键名不在启动时报 | 生成的配置会带别的进程才注册的 Mod 的键，进程看不出哪些是笔误；交给 `roost project doctor` 的 `config-schema` 检查 | `app/config_schema.go:97` 注释；方案 §7.1 |
| 静态注册为什么不是 Mod | 必须在**任何** Mod `Init` 之前完成；放在 `app.New` 之前，排序问题在构造上就不存在 | [STATIC_REGISTRATION.md](../../STATIC_REGISTRATION.md)「三层分离」 |
| manager 为什么按服务建 | 同一个单例可能出现在多个服务的 manager 集里，只有正在运行的服务启动它 | `manager/engine.go:14` |
| 独立 manager 的顺序 | 保持注册顺序（不是 map 序），否则顺序 bug 变成偶发 bug | `manager/order.go:13` |

→ [实现文档](../impl/01-app-lifecycle.md)对应：§3 主流程、§9 历史与重要修复。

## 4. 怎么用

### 4.1 进程入口：生成的 bootstrap

生成工程根目录的 `main.go`（`codegen/internal/roost/render.go:80`）只有三步（`renderMain`，`codegen/internal/roost/render.go:298`）：

```go
a, err := bootstrap.New()
if err == nil { err = a.Execute() }
if err != nil { slog.Error("server exit", "err", err); os.Exit(1) }
```

`internal/bootstrap/generated.go`（`renderBootstrap`，`codegen/internal/roost/render.go:319`）的形状：

```go
func New() (*app.App, error) {
	if err := registry.RegisterAll(); err != nil { return nil, err } // 静态注册，必须最先
	a := app.New("planet", buildinfo.VersionString())
	a.Singleton(kitredis.SingletonStore)                            // 有 redis 时安装单实例锁后端
	a.Mods(kitlock.NewLockMod(), kitops.NewOpsMod(), kitstatslog.NewStatsLogMod()) // 共享 Mod
	a.RegisterServer(app.ServiceName("game"), serviceGame.New(),
		kitconfigdata.NewConfigDataMod(), kitmongo.NewMongoMod(), kitnats.NewNatsMod(nil),
		kitdataengine.NewMod(kitdataengine.WithEntityAccess(EntityAccess), kitdataengine.WithRemoteProjection(true)),
		kitnest.NewMod(EntityAccess), /* …服务专属 Mod… */)
	// …其他服务类型…
	return a, nil
}
```

真实生成物可以在远端的 `demo-generated` 分支看（`internal/bootstrap/generated.go`；提交信息形如 `demo-generated: roost-core <sha>`，是从 game-demo 模板生成的产物、不是 tag 内容；由哪个流程推送未核实）。

仓库内能直接运行的最小装配是 `app/example_test.go`（`GOWORK=off go test -run Example ./app`）：两个共享 Mod、两个服务类型、一个服务专属 Mod。注意它的 `ManagerMod` 是演示用的玩具，在 `Provide` 里启动 manager、`Stop` 不带 ctx；真实工程用 `kit/manager.NewManagerMod`（§4.5）。

`Mods(...)` / `RegisterServer(...)` 里写的顺序**不是**启动顺序：App 按 `DependsOn` / `OptionalDependsOn` 拓扑排序，没有依赖关系的 Mod 保持书写顺序（`app/app.go:910`）。

### 4.2 CLI

```bash
./planet game -c configs/service/config.game.yaml --sid 2001   # 启动 game
./planet game -c prod.yaml --check-config                      # 只加载并检查配置后退出（不启动任何 Mod）
./planet game --print-config                                   # 打印本服务全部声明（App + 全部 Mod + 服务本身）生成的 YAML
./planet game --print-config-schema                            # 同一份声明的 JSON 键表（roost project doctor 读它）
```

- 不写 `-c` 时用 `configs/service/config.<service>.yaml`；**默认路径**不存在只打一条 Warn、按缺省值继续，**显式** `-c` 指向的文件不存在或解析失败直接报错（`app/app.go:140`）。
- `sid` 以配置文件为准；只有显式写了 `--sid` 才覆盖（`--sid` 缺省 1000 只作为配置没写时的缺省，`app/app.go:137`、`app/app.go:147`）。`server_type` 总是被设成子命令名（`app/app.go:146`）。
- `--check-config` 走的就是启动前的同一次检查（`loadServiceConfig`），通过时打印 `config ok: <路径>`。注意默认路径文件不存在时它也会检查缺省值并打印这个路径——发布前检查请总是显式写 `-c`。

### 4.3 写一个 Mod（含配置声明与依赖）

最小写法（形状取自 `app/config_schema.go:16` 的注释和 kit Mod 的现行写法，例如 `kit/ops/ops_mod.go:67`、`kit/saga/mod.go:50`）：

```go
type shopConfig struct {
	app.ServiceIdentity // 需要 sid / server_type 时匿名嵌入，与 App 共用同一份声明
	MaxItems int           `config:"shop.max_items" default:"100" min:"1" max:"10000" help:"每个玩家商店最多上架的物品数"`
	Refresh  time.Duration `config:"shop.refresh_interval" default:"1h" min:"1m"`
}

type ShopMod struct{ cfg shopConfig; client fredis.IRedis }

func (*ShopMod) Name() app.ModName                 { return "shop" }
func (*ShopMod) DependsOn() []app.ModName          { return []app.ModName{mods.ModRedis} } // Mod 名，不是 capability 名
func (*ShopMod) ConfigSchema() app.ConfigSchema    { return app.SchemaOf(shopConfig{}) }
func (m *ShopMod) Init(cfg *viper.Viper) error     { return app.LoadConfig(cfg, &m.cfg) }
func (m *ShopMod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)                     // 能力在 Provide 里取；缺了就在这里点名失败
	if err != nil { return err }
	m.client = client
	return r.Register("shop", m)
}
func (m *ShopMod) Start() error                    { return nil }
func (m *ShopMod) Stop()                           { _ = m.StopWithContext(context.Background()) }
func (m *ShopMod) StopWithContext(ctx context.Context) error { /* 三步停机，见 4.7 */ return nil }
```

要点：

- **`Init` 只读配置**，不连外部依赖也可以；`Provide` 只登记能力、取别的 Mod 的能力；`Start` 才开始干活（起 goroutine、监听端口）。App 先把**同一段**的全部 Mod 都 Init 完，再全部 Provide，再逐个 Start（`app/app.go:315`）。
- **启动失败时谁会被 Stop**（按源码 `app/app.go:315`～`app/app.go:390` 归纳）：
  - 某个 Mod `Init` 失败：这一段已经 Init 的 Mod **都不会**收到 Stop（只停更早一段已启动的）。所以 `Init` 里不要建需要关闭的资源。
  - 某个 Mod `Provide` 失败：这一段已 Provide 的 Mod 和**失败的这个**都会收到 Stop（逆序），虽然它们都没 Start 过。
  - 某个 Mod `Start` 失败、或 Start 之间检查到 fail-stop：这一段**全部已 Provide 的 Mod**（包括失败的这个和排在它后面、还没 Start 的）都会收到 Stop。
  - 所以 `Stop` / `StopWithContext` 必须容忍「Provide 过但没 Start」，对没建起来的字段判 nil。守卫：`TestAppStopsModWhoseProvideFails`（`app/app_test.go:398`）。
- 需要别的 Mod 的能力就 `DependsOn` 那个 **Mod 的名字**（`kit/mods/name.go` 的常量），在 `Provide` 里 `app.Lookup`。Registry 内建的 `health`、`metrics`、`lifecycle`、`runtime.failure` 不是 Mod，不要写进 `DependsOn`（T-31）。
- 可选集成用 `OptionalDependsOn`：对方在同一段里存在时排在它后面，不存在就忽略（`app/mod.go:45`）。服务专属 Mod 可以 `DependsOn` 共享 Mod 的名字（共享段已经全部启动）；共享 Mod 不能依赖服务专属 Mod。
- 一次登记多个能力用 `Registry.RegisterBatch` 或 `mods.RegisterAll`：先预检全部名字、再一起发布，消费者看不到半套（`app/registry.go:54`、`kit/mods/registry.go:18`）。
- 业务里的真实例子：game-demo 的业务服务 `demo/internal/service/game/service.go.tmpl`（`ConfigSchema` 在 52 行，`Init` / `Shutdown` 在 59 / 243 行）与它的配置结构体 `demo/game/settings/settings.go.tmpl`（`Schema()` 在 60 行）。

### 4.4 写一个 Service

```go
type Service struct{ /* … */ }
func (*Service) Name() app.ServiceName                 { return "game" }
func (*Service) ConfigSchema() app.ConfigSchema        { return settings.Schema() } // 可选：业务代码自己读的键
func (s *Service) Init(r *app.Registry) error          { /* 取能力、建业务对象、起后台循环 */ }
func (s *Service) Serve(ctx context.Context) error     { <-ctx.Done(); return nil }
func (s *Service) Shutdown(ctx context.Context) error  { /* 受 ctx 约束；对没初始化的字段判 nil */ }
```

- `Init` 在全部 Mod 都 Start 之后调用；`Init` 返回之后 App 立刻派发 `service.started`（ops 就绪位在这里置真），然后才起 `Serve`。也就是说 **`/readyz` 变成 200 的条件是 `Init` 已返回**，不是 `Serve` 已经开始跑（推断自 `app/app.go:413`～`app/app.go:453` 的顺序）。
- `Serve` 返回（不论是否带错误）即开始停机；返回 `context.Canceled` 不算错误。
- **`Shutdown` 在启动失败时也会被调用**——包括 `Init` 自己返回错误的情况——所以必须容忍部分初始化。启动失败路径给它 5s（`app/app.go:595`）；正常停机路径给它 `shutdown.total_timeout` 的整段 ctx。没在时限内结束（含 panic、按 ctx 超时返回），App 不停 Mod、不释放单实例锁（`app/service.go:8`）。
- 业务服务读的键由服务自己声明（`ConfigSchema`），错误前缀是 `service <名字>:`（A4 ① 收尾，RR-20261006-40）。

### 4.5 用 manager 管服务内存单例

```go
a.RegisterServer("game", serviceGame.New(),
	kitmanager.NewManagerMod(sceneMgr, routeMgr /* 无依赖的保持注册顺序 */),
	/* 其他 Mod */)
```

- manager 实现 `app.IManager`（`Name / Start(registry) / Stop`），需要依赖其他 manager 时实现 `DependsOn() []string`，需要有界停机时实现 `StopWithContext(ctx) error`（`app/manager.go:9`～`app/manager.go:25`）。
- `ManagerMod.Start` = `Engine.Start`：按依赖序逐个 `Start(registry)`；某个失败时只回滚**已经成功**的那些（失败的那个自己负责清理）；停机请求到达时中止剩余的（`manager/engine.go:127`）。
- 一个 Engine 只有一次启动机会：重复 `Start`、失败后重试、Stop 之后再 Start 都返回 `ErrStartState`；`Start` 之后（包括第一个 `Start` 进行中）`Register` 返回 `ErrRegisterAfterStart`（`manager/engine.go:32`～`manager/engine.go:40`，T-188）。
- 静态注册方案把「需要 Registry 的业务接线」（建 runtime 登记到能力、注册要查表的 admin 命令）放在这一层（STATIC_REGISTRATION 层 B）。

`lifecycle.ManagerGroup` 是另一个泛型实现，**停止不带 ctx、不满足三步停机**，仓内与 cube / ssr 都没有生产调用方（`lifecycle/manager_group.go:37`）；新代码不要用它。

### 4.6 生命周期 hook

```go
reg := app.MustLookup[*lifecycle.Registry](r, app.ModLifecycle)
_ = reg.Register(lifecycle.Hook{Name: "shop.drain", Phase: lifecycle.PhaseServiceStopping, Order: 10,
	Handler: func(ctx context.Context, e lifecycle.Event) error { return drain(ctx) }})
```

- 同一阶段按 `Order` 升序，同 Order 保持登记顺序；同名 hook 再登记会替换（`lifecycle/lifecycle.go:55`）。
- 启动阶段（`app.init`、`mods.started`、`service.started`）同步派发、**没有期限**，第一个返回错误的 hook 让启动失败。
- 停机阶段（`service.stopping`、`service.stopped`）在 `shutdown.total_timeout` 内等，一个失败不挡住后面的；hook 不配合 ctx 到期时，App 按停机不完整处理（不调 `Shutdown`、不停 Mod、不释放锁），错误点名卡住的 hook（`app/app.go:857`，RR-20261005-NC-231、RR-20261006-25）。
- 用 `App` 的 Registry 里的 `lifecycle.Registry`，不要用包级 `lifecycle.DefaultRegistry()`——App 不向它派发事件。

### 4.7 写停机对象：三步停机 + 共用类型

新的停止入口按 roost-coding 的要求用共用类型（A3）：

```go
type Worker struct {
	work   operation.Lifetime // 在途调用的准入 + 计数
	stopMu operation.Serial   // 停止入口串行，后到者等待受自己的 ctx 约束
}

func (w *Worker) Handle(...) error {
	if !w.work.Begin() { return ErrStopped }
	defer w.work.End()
	/* … */
}

func (w *Worker) StopWithContext(ctx context.Context) error {
	if err := w.stopMu.Lock(ctx); err != nil { return err }
	defer w.stopMu.Unlock()
	if err := w.work.Wait(ctx); err != nil { return err } // ①关准入 ②在 ctx 内等排空；超时保留计数，可重试
	return w.releaseDependencies()                        // ③排空之后才释放
}
```

- 回归测试用 `internal/stopcontract.Check`（只能在本模块内导入；生成工程的契约用例由 codegen 注入骨架源码，`internal/stopcontract/stopcontract.go:16`）。现有例子：`manager/stop_contract_test.go:12`、`kit/ops/stop_contract_test.go:45`、`kit/nest/stop_contract_test.go`。
- 例外（roost-coding）：关闭不等在途工作、临界区很短时可以用 `sync.Mutex`（kit `RedisMod`）；标准库已经提供①②的直接用（ops 的 `http.Server.Shutdown`）。

### 4.8 fail-stop

- 发现「不能再安全写」的基础设施（DataEngine 存储结果致命、Remote 释放失败、失锁）调 `RuntimeFailure.Fail(err)`；App 醒来走正常停机路径，最后非零退出。
- 需要在 fail-stop 那一刻**立即**做的事（目前只有 Nest 围栏）在 Provide 里 `OnFail(hook)` 登记：首次失败时在调用 `Fail` 的 goroutine 上按登记顺序同步执行，**全部执行完才唤醒停机**；回调必须快速、不阻塞、不做 I/O（`app/runtime_failure.go:57`）。现有登记：`kit/nest/nest_mod.go:220`。现有 `Fail` 调用：`kit/dataengine/mod.go:476`、`kit/remoteentity/remote_entity_mod.go:116`、单实例锁 `app/singleton.go:474`。

### 4.9 单实例锁与活性查询

- 装配：bootstrap 调 `a.Singleton(kitredis.SingletonStore)`（`kit/redis/singleton.go:44`，从同一份 `redis.*` 配置建独立连接）；每个服务用 `singleton.enabled` 决定是否启用。启用而没装 opener 时启动失败（`ErrSingletonOpenerMissing`）。
- 模块不感知锁。需要「这些 sid 里哪些进程活着」的模块用 `app.Lookup[app.SingletonLiveness](r, app.ModSingleton)`；每次最多 200 个 sid（`app/singleton.go:93`）。
- 需要接管上一代同 sid 进程留下的协调状态（例如 Remote 共享锁）的模块读 `app.ModSingletonIncarnation`；**没有这个能力（未启用锁）就不得接管**（`app/singleton.go:76`）。

### 4.10 静态注册

- 打标记：`//roost:register phase=<phase> [order=<n>]`，打在无参数、返回空或单个 `error` 的包级函数上。阶段顺序 `pre → kind → config → component → entity → protocol → nest → route → post`（`codegen/internal/registry/parse.go:32`），同阶段按 `order`、再按 import 路径 + 函数名，输出确定。
- `roost generate` 汇总进 `internal/registry/generated.go`，`RegisterAll()` 内部带 `sync.Once`（`codegen/internal/registry/gen.go:102`），生成的 `bootstrap.New()` 第一行调用它，**早于 `app.New`**。
- 判据：不需要 `app.Registry`、不做 I/O 的注册才属于这一层；需要 Registry 的走 manager（4.5），资源型的是 Mod。细节归 12 分区。

→ [实现文档](../impl/01-app-lifecycle.md)对应：§3 主流程、§5 并发。

## 5. 配置

App 自己读的键（声明在 `app/config_schema.go:203`、`app/singleton.go:123`）。全部框架键的完整列表用 `--print-config` 打印；规则（类型、单位、枚举、生产检查）见 07 分区。

| 键 | 缺省 | 范围 / 校验 | 说明 |
| --- | --- | --- | --- |
| `server_type` | 子命令名 | 必填（App 写入） | 服务类型 |
| `sid` | 配置值，否则 `--sid`（1000） | 正整数（int32） | 实例号，同一服务类型内唯一 |
| `env` / `app.env` / `environment` | 空 | 任一为 `prod` / `production` 即生产 | 打开生产规则：secret 键非空非 `dev-`、ops 不绑公网、Redis 地址必填、`time.logic_offset` 必须为 0 |
| `log.level` / `log.json` / `log.stdout` / `log.file` / `log.dir` / `log.caller` / `log.rotate_interval` / `log.rotate_time_format` | info / false / true / true / `log` / false / 24h / 空 | 声明类型 | `flog.Init` 的参数（`app/app.go:190`） |
| `time.logic_offset` | 0 | 生产必须 0 | 业务时钟偏移，启动时设一次；细节见 10 分区 |
| `metrics.max_series_per_metric` | 0（框架默认） | ≥ 0 | 每个指标的序列上限 |
| `shutdown.total_timeout` | 30s | > 0 | 停机总预算：`Service.Shutdown` + 全部 Mod 停完 + 锁 Release 的上限 |
| `singleton.enabled` | false | 布尔（`on` 报错） | 启用单实例锁 |
| `singleton.key_prefix` | 空 | 启用时必填、不含空白 | 同一套部署全部服务相同 |
| `singleton.ttl` / `renew_interval` / `guard` | 15s / 3s / 5s | > 0；`renew_interval ≤ guard`；`2 × renew_interval ≤ ttl − guard` | 锁的有效期、续期节拍、窗口末尾的判定余量 |
| `singleton.startup_wait` | 不写取 `2 × ttl` | `≥ ttl + 2 × renew_interval` | 启动时等锁的上限 |

ops 侧和 readiness 相关的键：`ops.enabled`、`ops.addr`（缺省 `127.0.0.1:9100`，生产绑非回环要写 `ops.allow_public_addr: true`），见 `kit/ops/ops_mod.go:67`。

**检查时机**

1. 启动：`run` 第一步读文件、按「App + 本服务全部 Mod + 服务本身」的合并声明检查，全部错误一次返回，**任何 Mod Init 之前**（`app/app.go:150`、`app/app.go:182`）。错误按主人点名：`mod <名字>: config: <键> …`、`service <名字>: …`。
2. Mod `Init`：用 `app.LoadConfig` 按同一份声明读值，缺省值在这里生效。
3. `--check-config`：只做第 1 步。
4. 生成工程：`make config-check-all`（`roost config check --all`）与 `roost project doctor` 的 `config-schema:<服务>` / `config-reads`（12 分区）。

**停机预算的生成值**：生成器按每个服务实际注册的 Mod 计算 `total_timeout = 声明预算之和 + 3s × 未声明 Mod 数 + 5s（Service.Shutdown 余量）+ 3s（启用单实例锁时的 Release）`（`codegen/internal/roost/shutdown_budget.go:147`），game-demo 的 game 服务是 111s，部署宽限期 = 它 + 5s。只计生成器认识的 Mod：手写 Mod 的 `StopBudget`（例如 `RemoteMirrorMod`）要手工加进去（RR-20260926-66）。

→ [实现文档](../impl/01-app-lifecycle.md)对应：§7 持久化 / 协议格式（键空间、CLI 输出）。

## 6. 运行与运维

### 6.1 正常启动的日志顺序

```
starting server  name=… version=… type=game sid=… config=…
singleton: acquiring key=…                      （启用锁时）
singleton: acquired key=… value=<token|host|pid|ms>
mod init mod=<共享 Mod>   … → mod start mod=…
mod init (service-specific) mod=… → mod start (service-specific) mod=…
service init service=game
ops: serving addr=…                             （ops Mod Start 时）
```

`/healthz` 在 ops Mod Start 之后就答 200（只表示进程活着）；`/readyz` 在 `service.started` 之后才可能 200。

### 6.2 停机日志与退出

```
received signal, shutting down signal=terminated      （或 runtime infrastructure failure, shutting down / service exited with error）
service shutdown service=game
mod stop (service-specific) mod=… budget=…  → mod stopped mod=… duration=…
mod stop mod=… budget=…                     → mod stopped …
server stopped type=game
singleton: released key=…
```

- 进程退出码：`run` 的返回值经 `Execute` 交给 main，非 nil 时 main 打 `server exit` 并 `os.Exit(1)`。
- 返回之前 `run` 自己把最终错误写进文件日志：`app run failed err=…`（RR-20261006-07，`app/app.go:211`）——main 的 `server exit` 在文件日志已关闭之后打印，只到 stderr。配置检查失败发生在日志初始化之前，只会出现在 stderr。
- 停机期间才发生的 fail-stop 也会让进程非零退出：`app: runtime failure after shutdown began: …`（RR-20261005-NC-232，`app/app.go:246`）。
- 停机里第二次 SIGTERM 不做任何事（信号通道只读一次）；超过部署宽限期由平台 SIGKILL。

### 6.3 readiness

| 端点 | 200 的条件 | 用途 |
| --- | --- | --- |
| `/healthz` | ops 端点在监听 | 存活探针 |
| `/readyz` | 就绪位为真（`service.started` 之后、`service.stopping` 之前）且没有任何 checker 为 Fail | 流量切换、部署脚本等待 |

- `/readyz` 响应体：`ok`、`degraded`、`degraded_dependencies`、`dependencies`（每个 checker 的 `name / status / message / error / checked_at_ms`）、`service`、`sid`、`message`（`starting` / `service started` / `service stopping`）、`server_time_ms`（业务时间）、`metrics`（`kit/ops/ops_mod.go:298`）。
- 每个 checker 1.5s 期限，到期记 `fail`、`message: check timed out`；同一 checker 卡住期间后来的快照等同一次调用，不再新开（T-280）。
- 单实例锁的 checker 名是 `singleton`：持有 ok、窗口内续期结果未知 degraded、失锁或未持有 fail。

### 6.4 常见故障

| 现象 | 去看 |
| --- | --- |
| 启动即退：`unknown mod dependency "…"` | T-30、T-31、T-183：`DependsOn` 写了 capability 名，或缺少被依赖的 Mod |
| 启动即退：`config: … must be true or false` / `needs a unit` / `must be a whole number` | T-256；用 `--check-config` 在发布前检查 |
| 新进程卡在 `singleton: waiting for the current holder …` 或以 `singleton is held by a live process` 退出 | T-212 |
| 运行中 `singleton lock lost; fail-stop` | T-213 |
| 启动失败后 `service <name> cleanup after startup failure incomplete` | T-257 |
| 停机报某个 Mod `context deadline exceeded`、锁未释放 | T-251；必要时调大该 Mod 的预算与 `shutdown.total_timeout`，再 `roost project sync` |
| 停机后非零退出：`runtime failure after shutdown began` 或 `lifecycle service.stopping hook "<名字>" did not return …` | T-264 |
| `/readyz` 503 且某项 `check timed out` | T-280 |
| `/readyz` 200 但 `degraded: true` | T-267 |
| manager `ErrStartState` | T-188 |
| 业务时间相关的拒绝启动 | T-270、T-284（10 分区） |

（T 行见 [TROUBLESHOOTING.md](../../TROUBLESHOOTING.md)。）

→ [实现文档](../impl/01-app-lifecycle.md)对应：§6 失败与不确定结果处理、§8 测试与门禁。

## 7. 保证与不保证

**保证**

1. 配置错误在任何 Mod Init 之前一次报全；同一个键被两处声明得不一样即启动失败。
2. 同一段（共享段 / 服务专属段）里，每个 Mod 的 Init、Provide、Start 都排在它的硬依赖与在场的可选依赖之后；全部 Init 完才开始 Provide，全部 Provide 完才开始 Start；停止严格逆序，服务专属段先停、共享段后停。依赖图非法（缺失硬依赖、环、重名、空名、nil）在该段任何 Mod Init 之前失败——包括只有一个 Mod 的段（RR-20261003-NC-01）。
3. 启用单实例锁时：同一 `server_type + sid` 的 Mod 只在持锁期间运行；失锁在 `validUntil` 之前判定并 fail-stop；全部 Mod 停完才 Release；停机不完整、失锁时不 Release，键在 `ttl` 内过期。
4. 首次 `RuntimeFailure.Fail` 先同步执行完全部 `OnFail`（Nest 已拒绝新派发）再唤醒停机；启动期间发生的 fail-stop 让启动停在下一个阶段边界。
5. 停机中任何一步因 ctx 超时没停完，都不会再去停它更底层的依赖，不会释放单实例锁；`run` 按预算返回，错误如实带出。
6. fail-stop（不论发生在停机前还是停机中）都让进程非零退出。
7. `/readyz` 在 `service.stopping` 第一时间变 503，在停机任何资源释放之前。

**不保证 / 已知限制**

1. 拿到锁之后、`Service.Init` 返回之前没有 SIGTERM 处理：这段时间（包括 DataEngine 长时间重放）收到 SIGTERM 进程直接终止，不停 Mod、不 Release，下一次启动最多等一个 `ttl`（方案 §3.4 记为现状延续）。
2. 启动阶段的 lifecycle hook（`app.init`、`mods.started`、`service.started`）没有期限，卡住会让启动一直挂着。
3. 服务专属 Mod 的依赖图在共享 Mod **已经启动之后**才校验（`app/app.go:352`），图非法时先停掉共享 Mod 再返回；「图非法 → 没有任何 Mod Init」只对共享段成立。
4. 不配合 ctx 的回调（Mod 停止、hook、`Service.Shutdown`、health checker）杀不掉：App 只保证按预算返回并保留依赖，那些 goroutine 留到进程退出。
5. 单实例锁不替代杀掉旧进程：卡住的旧进程恢复后会失锁退出，但在那之前它仍持有 WAL `flock` 与端口（T-213）。
6. 拼错的键名（没有任何声明）不在启动时报，只由 doctor 报。
7. 手写 Mod 的 `StopBudget` 不进生成的 `total_timeout`。
8. `Live` 把停机中的进程算作活的，需要排除它们的调用方自己判断（C5）。

**需要外部验证**：真实 Redis / Redis Cluster 上的锁语义（`kit/redis` 的 integration 用例）；真实进程 kill -9 / SIGSTOP 演练（方案 §13 第 5 笔，2026-10-05 记录）。

→ [实现文档](../impl/01-app-lifecycle.md)对应：§4 不变量清单、§10 review 检查点。

## 8. 相关文档

- 实现：[impl/01-app-lifecycle.md](../impl/01-app-lifecycle.md)
- 总览与术语：[00 总览](00-overview.md)
- 配置规则：[07 配置](07-config.md)；时间：[10 时间](10-time.md)；可观测与 ops：[11 可观测与运维](11-observability.md)；生成器：[12 代码生成与工程](12-codegen.md)
- 方案：[单实例锁](../../feature/APP-SINGLETON-LOCK-2026-10-05.md)、[A4 ① 配置声明](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)、[A3 共用停机类型](../../feature/REFACTOR-2026-10-05-shared-stop-contract.md)、[D1 readyz](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)、[第十二轮决定](../../feature/DECISIONS-R12-KIT-2026-10-06.md)、[静态注册](../../STATIC_REGISTRATION.md)、[业务时间只许前进](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)
- 执行契约：[roost-coding](../../agent-skills/roost-coding/SKILL.md)「生命周期与装配的复审要点」「配置只经声明读」
- 快速参考：[USER_GUIDE](../../USER_GUIDE.md)（§2 单实例锁、§10 配置写法与启动校验）、[INTERNALS](../../INTERNALS.md)、[TROUBLESHOOTING](../../TROUBLESHOOTING.md)
