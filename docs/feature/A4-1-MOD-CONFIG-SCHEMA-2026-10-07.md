# A4 ①：每个 Mod 声明自己的配置，校验、生成器、doctor 共用这一份声明（2026-10-07）

依据：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) A4 行（①）与第十三轮“原下个大版本项本版完成”
（维护者原话：“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”）。
前序：[A4 ② 严格读取](REFACTOR-2026-10-05-strict-config-reads.md)（`3e3350d5`）、RR-20260926-12、RR-20261005-NC-190 / NC-192、N02 O2。
维护者的配置原则（[记忆 config-ease-of-use]）：配置要容易用，需要手写的整理代码尽量少，结构简单易懂；规则只在运行时加载时强制一次。

## 1. 现在的问题

一个配置键的知识分散在四处，新加一个键要改四处，漏一处没有任何东西报错（A8、A4 两批都出过“清单漏键”“模板缺键”）：

| 处 | 现在的形状 |
| --- | --- |
| Mod 的读取 | `Init` 里 `read.Int("dataengine.outbox.workers")`，缺省值、范围散在 `positive(v, 2)`、`if v < 0 { return … }` 里 |
| app 的启动校验 | `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys` 三张手写清单（17 / 99 / 79 个键），外加 syncbus 三段、`.call_timeout` 后缀；守卫测试用正则扫源码，核对读取点都在清单里 |
| 生成器 | `codegen/internal/roost/catalog.go` 的 `Config` 字符串、`framework_services.go` 的 `ConfigFunc`、`player_tcp_config.go` 的缺省表，键名、示例值、注释全手写 |
| doctor | 只查 YAML 能解析、`sid` 存在、生产示例没有 `127.0.0.1`；不知道任何键的类型 |

## 2. 方案

### 2.1 声明的形状：配置结构体 + tag（推荐，已实施）

每个 Mod 把自己读的键写成一个结构体，字段类型就是键的类型，tag 写键名、缺省值、范围 / 枚举、是否必填、说明：

```go
type outboxConfig struct {
	Workers       int           `config:"workers" default:"2" min:"1" help:"outbox 发布 worker 数"`
	LeaseDuration time.Duration `config:"lease_duration" default:"30s" min:"1ns" example:"30s"`
	Owner         string        `config:"owner" help:"outbox 租约的持有者名，不写取 <server_type>-<sid>"`
}

type modConfig struct {
	Database string       `config:"dataengine.database" required:"true" example:"game"`
	Outbox   outboxConfig `config:"dataengine.outbox"`
	…
}
```

| tag | 含义 |
| --- | --- |
| `config` | 键名。嵌套结构体的键 = 外层前缀 + `.` + 内层键；以 `_` 结尾的前缀直接拼接（saga 的 `config:"result_"` 下 `ack_wait` 是 `saga.result_ack_wait`，一组同形的平铺键共用一个结构体）；匿名嵌入的结构体不加前缀（共享一组键，例如 `app.ServiceIdentity` 的 `sid` / `server_type`）；`config:"x,closed"` 的段下出现没有声明的键报错（saga 的 `step_defaults`；map 的元素总是 closed） |
| `default` | 键没写时的值（YAML 写法）。不写 = 类型零值 |
| `min` / `max` | 闭区间。数字、时长按类型比较；`min:"1"` / `min:"1ns"` 报错文本是 “must be positive” |
| `enum` | 用 `\|` 分隔的可选值，比较时忽略大小写与两端空白，读出的值规范成小写 |
| `required:"true"` | 必须写且非空 |
| `secret:"true"` | 生产环境（`env` / `app.env` / `environment` 为 prod / production）必须非空且不以 `dev-` 开头 |
| `example` | 生成器写进配置文件的值（生效的一行）。没有 `example` 的键在生成的配置里写成注释 `# key: <default>`，作为文档，不覆盖代码缺省 |
| `help` | 说明，生成器写成键上方的注释；USER_GUIDE 不再逐键抄写 |

字段类型：`bool`、`int` / `int32` / `int64` / `uint32`、`float64`、`time.Duration`、`string`、`[]string`（YAML 列表或逗号分隔串）、
嵌套结构体、`map[string]T`（键名里是 `*`，例如 saga 的 `saga.steps.*.*.timeout`；map 下出现没声明的字段报错）。

读出类型与原 `app.ConfigBool` / `ConfigDuration` / `ConfigInt` 的严格规则一致，错误文本不变（`config: x must be true or false, got "on"`、
`… needs a unit …`、`… must be a whole number …`）；新增的范围 / 枚举 / 必填错误一律是 `config: <键> …` 开头、点名键。

结构体自己的跨键规则（`warn_unacked_records ≤ max_unacked_records`、生产环境 ops 端点不能绑公网）写成可选方法
`ValidateConfig(production bool) error`，在声明检查全部通过之后、自内向外执行。匿名嵌入的结构体的 `ValidateConfig` 被提升为外层的方法，
由外层那一次调用执行；外层自己定义了 `ValidateConfig` 时要显式调用被嵌入的那个（Go 的方法遮蔽，`kit/dataengine` 的 `config` 就是这样）。
缺省值（`default`）在键没写时生效；写了 0 或负数按范围检查。需要“0 = 交给 core 决定”的键声明 `min:"0"` 不写 `default`，Mod 只在值大于 0 时覆盖 core 的缺省。

**另一种同样合理的形式（未采用，供维护者选）**：Mod 实现 `ConfigSchema()` 返回一张键表，读取用句柄：

```go
var workers = app.IntKey("dataengine.outbox.workers", 2).Min(1).Help("…")
func (m *Mod) Init(cfg *viper.Viper) error { c := app.Load(cfg, schema); m.workers = workers.Get(c) … }
```

好处是声明是普通值、可以运行时拼键名；代价是每个键要写一次声明、一次读取，“声明了没读”要靠扫描句柄变量，
读出来的值没有结构，Mod 里还是一个个变量。结构体形式里字段就是读取结果，声明与读取是同一行，业务作者手写最少，所以推荐结构体。

### 2.2 Mod 怎么接：两行

```go
func (*Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(modConfig{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	if err := app.LoadConfig(cfg, &m.cfg); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	…  // 只剩把 m.cfg 交给 core 的代码，以及声明表达不了的跨键规则
}
```

- `app.LoadConfig(cfg, &dst)`：按 dst 的声明一次读完——没写的取 `default`，写了的检查类型、范围、枚举、必填、生产密钥，
  再调 `ValidateConfig`；全部错误一次报出（`errors.Join`）。这是 Mod 读配置的**唯一**入口。
- `ConfigSchema()`（`app.ModConfigSchema` 接口）：App 在任何 Mod Init 之前，把本服务全部 Mod（共享 + 服务专属）的声明与 App 自己的
  声明合并，对配置做同一套检查，所有 Mod 的错误一次报全。同一个键被两个 Mod 声明时，声明必须完全相同（例如 `redis.*` 由 Redis Mod 与
  单实例锁共用一个结构体），否则启动报错——两处声明漂移在第一次启动就暴露。
- 服务配置没有运行期 reload：配置只在启动时加载一次（App 启动检查 + 各 Mod 的 `LoadConfig`），两条路走同一个检查，
  范围外的值两处都拒绝（`TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup`）。configdata 表的 reload 是另一套机制（B10，`configdata/rules`），不受影响。
- 进程不报“没有任何 Mod 声明的键”：生成的配置会带别的进程才注册的 Mod 的键（`remote_entity.mirror.shutdown_timeout` 是只读 Mirror 的停机预算），
  业务代码也会读框架段里的键（game-demo 的 game 读 `activity.key_prefix`、`platform.key_prefix`），进程分不出哪些是笔误；拼错的键名由 doctor 判断（2.6）。

`app.ConfigReader` 删除；`app.ConfigBool` / `ConfigDuration` / `ConfigInt` / `ConfigInt64` 保留为单键的严格解析（工具、测试用，
解析规则在 `internal/configschema`，与声明读取同一份），框架代码不再用它们读配置（守卫见 2.5）。`app.ValidateServiceConfig(cfg)` 保留，
只检查 App 自己的声明；`app.CheckConfig(cfg, mods...)` / `App.CheckServiceConfig(service, cfg)` 检查 App 与给定 Mod 的全部声明。

### 2.3 共享的声明包：`internal/configschema`

声明的解析（tag → 键表）、严格类型解析、检查、合并、生成 YAML 段都在 `internal/configschema`，只依赖标准库。
app 用它读 viper；生成器（codegen）用它渲染配置段和做 doctor 检查。codegen 不导入运行时（`TestCoreDependencyBoundary`），
这个包与 `configdata/rules`（B10）一样是允许 codegen 导入的叶子包，根包测试守住它只依赖标准库。app 以类型别名 `app.ConfigSchema`
对外暴露键表类型，业务代码不直接导入 internal 包。

### 2.4 生成器：配置段从声明生成

- `kit/internal/configschemagen` 是一个小程序：导入 app 与 kit 的各 Mod，取它们的 `ConfigSchema()`，按生成器认识的单元
  （`catalog.go` 的 Mod 名、`framework_services.go` 的服务名、`app`）分组，写出 `codegen/internal/roost/kitconfig_gen.go`
  （`var kitConfigSchemas = map[string]configschema.Schema{…}`，`go:generate` 在 `catalog.go`）。kit 能导入 Mod，codegen 不能，
  所以由 kit 侧把声明“快照”成 codegen 的数据文件；这是数据，不是 import。
- `catalog.go` 的 `Config`、`framework_services.go` 的 `ConfigFunc` 改成 `kitConfigSchemas[name].YAML(…)`：有 `example` 的键写成生效行，
  其余写成带缺省值的注释行，`help` 写成键上方的注释。项目相关的示例值用占位符（`{project}`）。生产示例沿用原来的文本变换
  （`127.0.0.1` → `CHANGE_ME`、流副本 1 → 3 …）。
- 生成进工程的 Mod：player TCP 接入层的声明在生成器里是真实的 Go 类型 `playerTCPDeclaration`（启动生成器时就按 tag 解析、写错即 panic），
  渲染进生成的 `server_gen.go` 成为 `tcpConfig`，`add transport tcp` 补的键、参考配置 `player_tcp.yaml`、doctor 都从它来，`playerTCPConfigDefaults`
  的手写表删除。它的缺省值与生成的 `defaultConfig()` 相同（生成的 `TestDeclaredDefaultsAreDefaultConfig`），各键的上下界留在生成的
  `validateConfig`——它也检查代码里直接构造的 `Config`（`NewServer`），界只写一处。RPC 客户端 Mod 的声明（`clientModConfig`）写在模板里，
  kit/service 的九份 `*_rpc_assembly_gen.go` 经 `go generate` 重生成。
- 漂移守卫：
  1. `go generate ./...` 之后 porcelain 干净（CI 与本地都跑）；
  2. codegen 测试 `TestKitConfigSchemasMatchKitDeclarations` 用 `go run ../../../kit/internal/configschemagen -check` 比对快照与 kit 当前声明，不一致即红；
     `TestGeneratorConstantsMatchTheDeclaredExamples` 核对生成器自己的常量（DataEngine / player TCP 停机预算、`stats_log.dir`、`config_data.dir`、
     `activity.groups_file`）与声明的示例值相同；`TestPlayerTCPDeclarationAgreesWithKit` 核对生成的接入层与 kit/nest 对 `nest.request_timeout` 的声明相同；
  3. codegen 测试 `TestGeneratedConfigsMatchDeclarations` 渲染全量工程（全部 Mod、全部框架服务、单实例锁、player TCP）的开发配置、生产示例、
     k8s Secret 示例，按每个服务实际注册的 Mod 合并声明逐份检查：框架段里没有未声明的键、每个值通过类型 / 范围 / 枚举 / 必填。

### 2.5 “声明与读取一致”的守卫（替代三张清单与正则扫描）

删除 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`、syncbus 三段与 `.call_timeout` 后缀登记，以及按正则核对清单的
`TestEveryFrameworkBoolSwitchIsCheckedStrictly` / `TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly` / `TestFrameworkCodeDoesNotReadConfigLeniently`。换成：

- **读了没声明**：`TestFrameworkModsReadConfigOnlyThroughDeclarations`（app）用 `go/ast` 扫 app、kit 的非测试源码：
  对 `*viper.Viper`（参数、字段、变量、`Registry.Config()`）调用任何读方法（`Get*`、`IsSet`、`AllKeys`、`UnmarshalKey`、`Sub` …）或
  `app.Config*` 即失败；两份生成模板（字符串里的 Go 源码）按文本检查同样的调用。唯一的例外是 `app` 的 viper 适配器（`config_schema.go`）
  与单键读取本身（`config_values.go`）。框架代码里读配置只剩 `app.LoadConfig`。
- **声明了没读**：同一个测试找出所有带 `config` tag 的结构体字段，要求它在本包非测试代码里被读（选择子 `.Field` 出现在声明之外）。
  只为生成器写出、运行时不读的键因此无法存在。
- **声明与 Init 对得上**：kit 测试 `TestEveryKitModLoadsWhatItDeclares` 对每个 kit Mod，把它声明的每个键依次写成错误类型的值调用 `Init`，
  要求报错点名该键（证明 `Init` 的 `LoadConfig` 用的就是 `ConfigSchema()` 返回的那份声明）；同时合并全部 kit Mod 与 app 的声明，冲突即红。

### 2.6 doctor

`roost project doctor` 新增 `config-schema:<服务>`：对开发配置、生产示例、k8s Secret 示例里的 `config.yaml`，按该服务注册的 Mod
（与生成 bootstrap 用同一个解析）合并快照里的声明检查。FAIL：类型、范围、枚举、必填不合声明，生产示例与 Secret 示例里的 `secret` 键为空或 dev-，
框架段（任一框架声明用到的顶层段）里出现没有任何声明的键（拼错的键名）。WARN：键有声明但这个服务没有 Mod 读它（别的服务的段，或业务代码读的框架键；
新生成的 game-demo 的 game 配置因此有一行 WARN：`activity.*`、`platform.*` 是 game 的业务代码读的）。业务 Mod 的声明只有编译后的进程知道，所以真实的生产配置用进程自己检查：
每个服务命令新增 `--check-config`（加载并检查配置后退出，不启动任何 Mod），`--print-config`（打印本服务全部 Mod 声明生成的配置段，
包括业务 Mod）。

### 2.7 业务 Mod 前后对比

之前（A4 ② 之后的写法）：

```go
type ShopMod struct {
	maxItems int
	refresh  time.Duration
	region   string
}

func (m *ShopMod) Init(cfg *viper.Viper) error {
	read := app.NewConfigReader(cfg)
	m.maxItems = read.Int("shop.max_items")
	m.refresh = read.Duration("shop.refresh_interval")
	m.region = strings.ToLower(strings.TrimSpace(cfg.GetString("shop.region")))
	if err := read.Err(); err != nil {
		return fmt.Errorf("shop mod: %w", err)
	}
	if m.maxItems <= 0 {
		m.maxItems = 100
	}
	if m.maxItems > 10000 {
		return fmt.Errorf("shop mod: shop.max_items must be at most 10000, got %d", m.maxItems)
	}
	if m.refresh <= 0 {
		m.refresh = time.Hour
	}
	switch m.region {
	case "cn", "us", "eu":
	default:
		return fmt.Errorf("shop mod: shop.region must be cn, us or eu, got %q", m.region)
	}
	return nil
}
// 另外：在 configs/service/config.game.yaml、生产示例、k8s Secret 示例里手写 shop: 段并抄一遍注释；
// 写错类型的值只有这个 Mod Init 时才报，和其他 Mod 的错误分两次看到。
```

之后：

```go
type shopConfig struct {
	MaxItems int           `config:"shop.max_items" default:"100" min:"1" max:"10000" help:"每个玩家商店最多上架的物品数"`
	Refresh  time.Duration `config:"shop.refresh_interval" default:"1h" min:"1m" help:"商店刷新间隔"`
	Region   string        `config:"shop.region" enum:"cn|us|eu" required:"true" example:"cn" help:"商店所在区服"`
}

type ShopMod struct{ cfg shopConfig }

func (*ShopMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(shopConfig{}) }

func (m *ShopMod) Init(cfg *viper.Viper) error { return app.LoadConfig(cfg, &m.cfg) }
// 配置段：`<bin> game --print-config` 打印；启动前 / `--check-config` 与框架 Mod 的错误一起一次报全。
```

## 3. 改动面

| 位置 | 改动 |
| --- | --- |
| `internal/configschema`（新） | 声明解析、严格类型解析（自 `app/config_values.go` 移入）、检查、合并、YAML 渲染、`map` 通配 |
| `app` | `ConfigSchema` 别名、`SchemaOf`、`LoadConfig`、`ModConfigSchema`；App 自己的键（`sid`、`server_type`、`env`、`log.*`、`time.logic_offset`、`shutdown.total_timeout`、`metrics.*`、`singleton.*`）写成 `appConfig`；启动时合并全部 Mod 声明检查；`--check-config` / `--print-config`；删三张清单与 `ConfigReader` |
| kit 全部 Mod | dataengine、nest（含 entity sync）、remoteentity（含 Mirror）、saga（含步骤预算）、nats、syncbus、redis（含单实例锁存储）、mongo、etcd、ops、statslog、configdata、mods（persistence、service 公共键）、service 下九个服务 Mod 与它们的 RPC 客户端 Mod |
| 生成模板 | RPC 客户端 Mod（`servicerpc/template.go`）与 player TCP 接入层改成结构体声明 + `app.LoadConfig` |
| codegen | `kitconfig_gen.go`（生成）；catalog / framework services / player TCP 的配置段从声明渲染；doctor `config-schema:*` |
| 守卫 | 2.5 的三条；codegen 2.4 的两条 |

## 4. 行为与兼容

线上未部署，不做旧格式兼容（维护者第十三轮）。变化写进 CHANGELOG：

- 以前“写 0 或负数静默取默认”的键，现在按声明的范围拒绝（例如 `dataengine.outbox.workers: 0`）。要缺省就不写这个键。
  文档里本来就写着“0 取框架默认”的键（`nest.*` 的 worker / 容量、`nest.unload_resync.*`、`remote_entity.snapshot_interest_per_consumer` 等）
  声明成 `min:"0"`，行为不变。
- 字符串枚举（`nats.rpc.transport`、`sync.transport`、`syncbus.transport`、`sync.entity.mode`、`persistence.engine` …）在声明里检查，所有服务、
  所有值都检查，不再只在某些 transport 下检查相关时长。
- syncbus 只读 `syncbus.*` 段，旧的 `room.*` / `sync.*` 同名回退删除（`sync.transport` 等键不再被 syncbus 读取）。
- 生产规则跟着键的主人走：密钥非空非 `dev-`（`secret` tag）、ops 端点不绑公网（ops 的 `ValidateConfig`）、Redis 地址必填（Redis 配置的
  `ValidateConfig`，Redis Mod 与单实例锁共用）。`admin_gateway.*` 的生产检查删除：仓内没有任何代码读取这个段（C1 方案 1 的同一原则）。
- 生成的配置文件形状改变：每个框架段带说明注释，未写进 starter 的键以注释形式列出缺省值。已生成工程的配置是应用自有文件，不迁移。
- 生成的 player TCP 接入层与 RPC 客户端 Mod 调用 `app.LoadConfig` / `app.SchemaOf`，需要包含本变更的 core（v1.23.0）；
  生成器 Core 下限随 v1.23.0 发版时升到 v1.23.0（发版步骤里与 `framework-compat.yml` 的 minimum 行同步）。

## 5. 验证

`GOWORK=off`：`gofmt -l` 空；改动包 `go vet`、`go test -race -count=3`；`kit/...` 全量；kit integration 只跑受影响用例；
`go test ./codegen/...`；`go generate ./...` 后 porcelain 干净；新生成 game-demo（replace 到 worktree）build / vet / test 与 `project doctor`；
`codegen/scripts/source-head-check.sh full`；根包；`go build ./... && go vet ./...`。先红后绿与变异见第 6 节。

## 6. 实施状态

已实施（`d1226825`，分支 `a4s`，基线 `66d72a33`，rebase 到 `b839b77f`），未发版。

### 迁移清单

| 单元 | 声明 | 备注 |
| --- | --- | --- |
| app | `appConfig`（`ServiceIdentity`、`environmentConfig`、`log.*`、`time.logic_offset`、`metrics.*`、`shutdown.total_timeout`、`singleton.*`） | 单实例锁的三条时间关系在 `singletonConfig.ValidateConfig`；`log.*` 的 `SetDefault` 删除，缺省值在声明里 |
| kit/mods | `PersistenceConfig`（nest 与 dataengine 嵌入）、`ServiceMetricsConfig`（九个服务嵌入） | `KeyPrefix` / `Secret` / `Duration` / `RequiredDuration` / `ResolvePersistenceEngine` / `RedisClusterAddrs` 删除 |
| kit/redis | `Config`（Redis Mod 与 `SingletonStore` 共用）、`ClusterConfig`（服务 Mod 与 remoteentity 嵌入） | 生产 Redis 地址规则在 `Config.ValidateConfig` |
| kit/ops、statslog、configdata、etcd、mongo、nats、syncbus | 各自的 `config` | ops 的 admin 令牌与生产公网绑定在 `ValidateConfig`；syncbus 删 `room` / `sync` 回退；etcd 删 gate 回退 |
| kit/nest、dataengine | `config`（dataengine 的 `EffectsConfig` 导出给 `EffectStreamRetention` / saga） | 写 0 取 core 缺省的键 `min:"0"` |
| kit/remoteentity | `entityConfig`、`mirrorConfig`，共用 `snapshotConfig`、`mongoConfig` | 0 有含义的键的 `default` 与 `DefaultConfig` 相同（有用例核对） |
| kit/saga | `config`（`stepBudgetConfig` 用于 `step_defaults` 与 `steps.*.*`，`consumerConfig` 以 `result_` 等前缀平铺） | 缺省值与 `coresaga.DefaultOptions` 相同（有用例核对） |
| kit/service 九个服务 Mod 与九个 RPC 客户端 Mod | 各自的 `config` / `clientModConfig` | `account.KeyPrefix` / `platform.KeyPrefix` 给 game-demo 业务代码读同一个键 |
| 生成模板 | player TCP `tcpConfig`（生成器 `playerTCPDeclaration` 渲染）、RPC 客户端 `clientModConfig` | 见 2.4 |

### 先红后绿

- **读了没声明**：把新守卫的两个 AST 用例放到基线 `66d72a33` 上运行（临时文件，未提交）：`TestFrameworkModsReadConfigOnlyThroughDeclarations` 红，
  201 处直接读 viper / 单键读取，例如 `../kit/configdata/configdata.go:38:19`、`../kit/dataengine/mod.go:104:10`；本分支绿。
- **范围在加载与启动时都拒绝**：基线上同形用例（临时文件）：

  ```text
  --- FAIL: TestA4RangeBaseline/outbox_workers_negative (0.00s)
      zz_a4_range_baseline_test.go:35: Init = <nil>; want a refusal naming dataengine.outbox.workers
      zz_a4_range_baseline_test.go:38: ValidateServiceConfig = <nil>; want a refusal naming dataengine.outbox.workers
  （effects_replicas_zero、redis_pool_negative、saga_workers_negative、syncbus_storage_typo 同样两处 <nil>）
  ```

  本分支 `TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup`（kit）九项在 `Mod.Init` 与 `app.CheckConfig` 两处都点名拒绝。
- **生成的配置与声明不一致**：`TestGeneratedConfigsMatchDeclarations` 首次运行即发现生成的 `shutdown.serve_wait_timeout` 没有声明、没有读取方
  （[RR-20261006-38](../bug/RR-20261006-38.md)），修复后绿。

### 变异（全部变红，跑完还原）

| # | 变异 | 变红的用例 |
| --- | --- | --- |
| M1 | `kit/ops` Init 加一行 `cfg.GetString("ops.extra")` | `TestFrameworkModsReadConfigOnlyThroughDeclarations`：`../kit/ops/ops_mod.go:126:6` |
| M2 | ops 配置加一个没人读的字段 `Forgotten int config:"ops.forgotten"` | `TestEveryDeclaredConfigFieldIsRead`：`Forgotten (config:"ops.forgotten")` |
| M3 | ops Init 不调 `LoadConfig` | `TestEveryKitModLoadsWhatItDeclares`：`ops: sid = 8k: Init = <nil>` 等 |
| M4 | 去掉 `dataengine.outbox.workers` 的 `min:"1"` | `TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup`：Init / CheckConfig 都返回 nil |
| M5 | 生成器把 `serve_wait_timeout` 加回 shutdown 段 | `TestGeneratedConfigsMatchDeclarations`：`shutdown.serve_wait_timeout is not a key any framework mod declares` |
| M6 | 改 `redis.pool_size` 的 example 不重新生成快照 | `TestKitConfigSchemasMatchKitDeclarations`：快照不是 kit 当前的声明 |
| M7 | 快照里 `redis.pool_size` 的 example 改成 -1 | `TestGeneratedConfigsMatchDeclarations`：`redis.pool_size must not be negative, got -1` |

### 验证（`GOWORK=off`）

见提交说明与下方补记；`gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./kit/... ./app/... ./codegen/... .`；
`go test -count=1 ./app ./internal/... ./kit/... .`；`go test -count=1 ./codegen/...`；改动包 `go test -race -count=3`；`go generate ./...` 后 porcelain 干净；
新生成 game-demo（`-skip-deps` 生成后 replace 到 worktree、`go mod tidy`）`go build ./... && go vet ./... && go test ./...` 通过，
`roost project doctor` 退出 0（`config-schema:game` 一行 WARN，见 2.6），`planet game --check-config` 对开发配置与生产示例输出 `config ok`，
对写错的配置一次报出三个 Mod 的错误；`planet mail --print-config` 打印全部声明。

### 另一种声明形式（供维护者选）

见 2.1 末尾：Mod 方法返回键表 + 读取句柄。未采用的理由：每个键写两次（声明、读取），“声明了没读”要靠扫描句柄变量，读出的值没有结构。

### 未完成 / 后续

- 生成器 Core 下限（`codegen/internal/roost/manifest.go` 的 `minimumVersions.Core` 与 `framework-compat.yml` 的 minimum 行）要在发 v1.23.0 时升到 v1.23.0：
  生成的 player TCP 接入层与 RPC 客户端 Mod 调用 `app.LoadConfig` / `app.SchemaOf`，v1.22.0 没有。本分支未改（发版步骤统一改）。
- 业务 Mod 的声明只在编译后的进程里，doctor 只检查框架声明；业务键用 `--check-config` / `--print-config`。**（已由 §7 改为 doctor 编译工程、读回进程的声明。）**
- “声明了没读”的守卫只扫 app 与 kit；生成工程里的业务 Mod 没有同样的守卫（`glsvet` 可以以后加提示）。**（已由 §7 补上：生成工程同样守住，doctor 读得到业务声明。）**

## 7. 收尾：game-demo 的业务键有声明、生成工程同样守住（v1.23.0 发版前，2026-10-07）

维护者要求交给 review 前不留能绕过检查的分支和 WARN。§6 留下两件事，登记为 [RR-20261006-40](../bug/RR-20261006-40.md)：

1. 新生成的 game-demo 跑 `roost project doctor` 有一行 WARN：game 配置里的 `activity.groups_file`、`activity.key_prefix`、`platform.key_prefix`、
   `platform.payment_secret` 是 game 的业务代码读的，没有任何 game 的声明（业务代码直接 `registry.Config().GetString(...)`，或经 kit 的
   `platform.KeyPrefix(cfg)` 读一个本进程没有任何 Mod 声明的键）。这些键写错类型、拼错、漏写，App 启动检查都看不到，要到业务代码第一次读才暴露。
2. “读了没声明 / 声明了没读”的守卫只扫 app 与 kit。修前新生成的 game-demo 有 16 处直接读 viper（game 读 activity.* / platform.* / sid /
   server_type / saga.* / dataengine.*，生成器写的 `game/lifecycle/world_singleton.go` 读 sid）。

### 7.1 doctor 怎么知道业务声明

业务声明只有编译后的进程知道，这一点不变。§6 写的 `--check-config` 检查真实生产配置的值，但**不覆盖** WARN 这件事：进程不报“没有任何声明的键”
（生成的配置会带别的进程才注册的 Mod 的键，例如只读 Mirror 的 `remote_entity.mirror.shutdown_timeout`，实测 game 的进程声明里没有它），生成工程的
CI 也没有跑 `--check-config`（`make ci` 是 fmt / vet / glsvet / test / check-generated / config-check-all / id-check）。所以选：

- **doctor 在工程里编译一次进程，对每个服务运行 `<bin> <service> --print-config-schema`**（新 flag，把这个服务的全部声明——App、全部 Mod、服务本身——打印成
  JSON 键表），进程声明里框架快照没有的键就是业务声明。doctor 用它：①检查三份配置里业务键的值（类型 / 范围 / 枚举 / 必填 / 生产密钥）；②框架段里由业务
  声明的键不再算“本服务没人读”；③只有业务声明用到的段里出现没有声明的键（拼错）为 FAIL。工程编译不过（`compile:go-list` 已 FAIL）时只做框架那一半；
  能编译但进程读不出声明（`go build` 链接失败、`bootstrap.New()` 报错、两份声明冲突）时 `config-schema:<service>` 为 FAIL。
- 没有改用 `go run . <svc> --check-config`：它只答“值对不对”，答不了“这个键有没有声明”；而且逐服务逐文件 `go run` 要编译多次。一次 `go build`
  加每个服务一次 `--print-config-schema` 最简单。实测新生成的 game-demo 全量 doctor（strict）约 19s。

### 7.2 业务代码的键由业务服务声明

生成的 bootstrap 给业务服务注册的是框架 Mod，**没有业务 Mod 的注册位**（`services.<svc>.mods` 只认 kit 的 Mod 名）。要让 game 的业务代码有一个“业务 Mod”
声明它读的键，要么给生成器加一个每个业务服务都有的 Mod 注册钩子（每个工程多一个文件、bootstrap 形状变化），要么让业务服务本身声明。选后者：
`app` 把 `RegisterServer` 注册的 Service 与 Mod 同等对待——实现 `app.ModConfigSchema`（`ConfigSchema() app.ConfigSchema`）的服务，它的声明进 App 启动检查、
`CheckServiceConfig`、`ServiceConfigSchema`（`--print-config`）与 `--print-config-schema`，错误前缀是 `service <名字>:`。写法与 kit Mod 一致：`app.SchemaOf` +
`app.LoadConfig`。

game-demo：

| 位置 | 改动 |
| --- | --- |
| `demo/game/settings/settings.go.tmpl`（新） | `settings.Config`：嵌入 `app.ServiceIdentity`（与 App 的声明相同，Merge 不冲突），`activity.key_prefix` / `activity.groups_file`、`platform.key_prefix` / `platform.payment_secret`（全部 `required`，payment_secret 另加 `secret`），前缀的空白检查在 `Activity` / `Platform` 的 `ValidateConfig`；`Schema()`，以及 `Load` / `Identity` / `LoadActivity` / `LoadPlatform`（只读需要的部分，测试注册表只带 sid 也能用） |
| `internal/service/game/service.go` | `func (*Service) ConfigSchema() app.ConfigSchema { return settings.Schema() }` |
| `activity.go`、`playerowner.go`、`gm.go`、`spawner.go`、`purchase_drain.go`、`game/controllers/player/controller.go` | sid / server_type / activity.* / platform.* 经 `settings` 读；`purchase_drain` 不再经 kit 的 `platform.KeyPrefix` 读一个本进程没人声明的键；controller 的“payment_secret 为空”检查由 `required` 在启动时做 |
| `gift_saga.go`、`level_up_mail.go`、`activity.go` | saga.* / dataengine.* 是本进程框架 Mod 的键，经那个 Mod 自己的声明读：新增 `kitsaga.StreamSettings(cfg)`、`kitdataengine.EffectSettings(cfg)`（按 Mod 的整份配置结构体 `LoadConfig`，与 Mod Init 读的同一份），删掉 demo 里手抄一份缺省值的 `sagaSettings` / `effectSettings` 读取 |
| `activity_test.go` | 组文件拒绝用例的注册表补 `activity.key_prefix`（现在必填，与组文件一起在启动时检查） |
| 生成器 `framework_services.go` | 生成的 `game/lifecycle/world_singleton.go` 经 `app.ServiceIdentity` + `app.LoadConfig` 读 sid（所有带 World 的工程） |

### 7.3 生成工程的守卫

“读了没声明 / 声明了没读”的实现从 app 的测试文件挪到叶子包 `internal/configschema/guard.go`（`GoPackages` / `UndeclaredReads` / `UnreadFields`，只依赖
标准库，codegen 可以导入），app 的两个守卫测试改为调用它，判定不变。生成工程用同一份：

- codegen `TestGeneratedProjectsReadConfigOnlyThroughDeclarations`（`codegen/internal/roost/config_reads_promises_test.go`）：game-demo、saga、configdata、
  bare 四个夹具，加上全量渲染（game 模板 + 全部带配置的 Mod + player TCP + saga）的 Go 文件，逐包无直接读取，整个工程无没人读的声明字段（声明集中在
  `game/settings`、由别的包读，所以“没读”按工程算）。
- `roost project doctor` 新增 `config-reads`（FAIL）：对用户工程跑同一个检查。生成之后用户自己写的业务代码直接读 viper，doctor 同样点名——否则
  这种读取既不在 App 启动检查里，也不在 `config-schema` 里。
- codegen `TestDoctorReadsBusinessDeclarationsFromTheProcess`：game-demo 副本（roost-core replace 到本仓库、`go mod tidy`）经 `processConfigSchemas` 读回
  进程声明，全部 `config-schema:*` 为 OK、game 有 4 个业务键；删掉服务的 `ConfigSchema` 之后同一行 WARN 回来。

### 7.4 先红后绿

修前（基线 `82dfe672`）：

```text
$ roost project doctor -root <新生成的 game-demo>
WARN  config-schema:game   configs/service/config.game.yaml: no mod of game declares activity.groups_file, activity.key_prefix, platform.key_prefix, platform.payment_secret (another service's keys, or read by business code); configs/service/config.game.prod.example.yaml: …; deploy/k8s/base/secret.game.example.yaml: …
```

同一工程用 `configschema.UndeclaredReads` 扫（临时用例）：16 处直接读取——`game/controllers/player/controller.go:207`、`:218`，
`game/lifecycle/world_singleton.go:20`，`internal/service/game/activity.go:103`、`:109`、`:115`、`:146`，`gift_saga.go:655`、`:658`、`:661`，`gm.go:150`，
`level_up_mail.go:103`、`:106`、`:109`，`playerowner.go:115`，`spawner.go:186`。

修后：新生成的 game-demo（replace 到本 worktree）`go build ./... && go vet ./... && go test ./...` 通过；`roost project doctor`（strict）退出 0，**零 WARN、零 FAIL**：

```text
OK    config-schema:game   3 file(s) match the declarations of every mod and the service (4 business key(s))
OK    config-reads         66 package(s) read config only through declarations
```

变异（跑后还原）：

| 变异 | 结果 |
| --- | --- |
| game-demo 副本里 gm.go 加 `registry.Config().GetString("gm.secret_knob")`、settings 加 `Forgotten int config:"forgotten"` | `TestGeneratedProjectConfigGuardCatchesDrift`：两个守卫各点名一处；doctor `config-reads` 为 FAIL 并点名 `internal/service/game/gm.go:` 与 `Forgotten (config:"forgotten")` |
| 删掉 game 服务的 `ConfigSchema` | `TestDoctorReadsBusinessDeclarationsFromTheProcess`：`config-schema:game` 回到 WARN，点名那四个键 |
| app 的 `serviceDeclarations` 不收服务的声明 | `TestServiceDeclarationsAreCheckedAndPrintedWithTheMods`：`CheckServiceConfig = mod bank: …`（缺 `service shop: config: shop.max_items must be at most 10000` 等两条），`--print-config-schema lacks shop.region` / `shop.max_items` |

### 7.5 兼容

线上未部署，不做兼容。

- 新生成的 game-demo 多一个 `game/settings` 包；game 的配置少了任何一个 activity / platform 键现在在 App 启动检查时就拒绝（以前 activity.key_prefix 为空
  要到 activity 启动、payment_secret 为空要到 controller 构造时才拒绝，两处的判空检查随之删除）。已生成工程是应用自有代码，不迁移。
- 生成的 `world_singleton.go` 改用 `app.LoadConfig` 读 sid：`sid` 超出 int32 现在报错（以前截断）。
- doctor：工程能编译时多一次 `go build` 与每个服务一次 `--print-config-schema`；新增 `config-reads` 检查，用户工程里直接读 viper 的代码会让 doctor FAIL。
- `app`：服务的声明进启动检查；新 flag `--print-config-schema`；新 API `kitsaga.StreamSettings`、`kitdataengine.EffectSettings`。Core 下限照旧随 v1.23.0 升到 v1.23.0
  （生成的 game-demo 用到这三个新 API）。

### 7.6 验证（`GOWORK=off`）

已实施（`6e0619bb`，分支 `gaps`，未发版）。`gofmt -l` 空；改动包（app、internal/configschema、skill/...、kit/saga、kit/dataengine、codegen/...）`go vet`；`go test -race -count=3 ./skill/... ./app/... ./internal/configschema/... ./kit/saga/... ./kit/dataengine/...` 与 codegen 改动用例 `-race -count=3` 通过；`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后 porcelain 干净；skill/examples 三个示例实跑、sync-e2e 通过；根包 `go test -count=1 .`（含 `TestExamplesRun`）通过；`go build ./... && go vet ./...` 通过；新生成 game-demo（replace 到 worktree）`go build / vet / test ./...` 通过，`roost project doctor`（strict）退出 0、零 WARN 零 FAIL。见 [RR-20261006-40 修复记录](../bugfix/RR-20261006-40.md)。
