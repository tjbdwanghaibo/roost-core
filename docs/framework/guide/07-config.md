# 07 配置（说明）

> 配套实现文档：[impl/07-config.md](../impl/07-config.md)（文件地图、主流程与状态机、不变量与守卫测试、review 检查点）。
> 源码基准：tag `v1.23.0`（`28912cd6`）。文中 `path:line` 都按这个 tag。codebase-memory 图谱的代际是 2026-09-30，`internal/configschema/*`、`app/config_schema.go`、`configdata/rules`、`configdata/keyspelling.go`、`codegen/internal/roost/config_schema_doctor.go`、`kit/internal/configschemagen` 都不在图谱里（`not_tracked`），`configdata/configdata.go`、`hotcode/registry.go`、两个生成器为 `metadata_changed`。本篇只用图谱定位，结论全部按 tag 源码直接读取。

## 速览

- **这一块是什么**：两种完全不同的“配置”。**服务配置**是进程启动时读一次的 YAML（`configs/service/config.<服务>.yaml`，经 viper），每个 Mod 和业务服务用“配置结构体 + tag”声明自己读的键（维护者决定 A4 ①）；**业务数据配置**是策划 / 程序维护的数据表（`configs/data/*.json`），由 `configdata.Store` 加载成不可变快照，可以热更、回滚。另外两个小件：`featureflag`（进程内开关表）与 `hotcode`（函数级热补丁点）。
- **最重要的保证**：服务配置的每个键只有一份声明，启动检查、`LoadConfig` 读取、生成器写配置、doctor 检查全部用它；App 在任何 Mod `Init` 之前按本服务全部声明检查一次，**错误一次报全**。业务数据的列规则（required / unique / min / enum / ref）与键的大小写只在 configdata 加载层强制，每次 Load / Reload 都查，违反就整次拒绝、旧快照继续生效；一次请求钉住准入时的那一代快照。
- **最容易踩的坑**：① 框架与业务代码读服务配置**只能**经 `app.LoadConfig`，直接调 `cfg.GetString(...)` 会被守卫 / doctor 判 FAIL；声明了不读同样 FAIL。② 进程**不报**“没有任何声明的键”（拼错的键名），这件事只有 `roost project doctor` 的 `config-schema` 查；`make ci` 里的 `roost config check --all` **不按声明检查**（只查 YAML 能解析、有 `sid`）。③ 热更只读 `configs/data` 下的 JSON，改 CSV 必须先 `roost generate`。④ 热更“发布后撤回”期间准入的请求会读到被撤回的那一代；不能接受的检查要放进 `ValidateReload` / `BeforeApplyReload`。⑤ tablegen 的 schema 字段**必须写 `json` 标签**：不写时生成器按 snake_case 写 JSON 键，运行时按 Go 字段名读，没有规则的多词字段（`MaxStack`、`HP`）静默读成零值（§7.2 F1，本次核对发现）。

### 本篇覆盖的包

| 包 | 职责 |
| --- | --- |
| `internal/configschema` | 服务配置声明：tag → 键表（`Schema`）、严格类型解析、`Decode` / `Check`、`Merge`、YAML 渲染、源码守卫（读了没声明 / 声明了没读）。只依赖标准库，codegen 可以导入 |
| `app`（配置部分） | `ConfigSchema` 别名、`SchemaOf`、`LoadConfig`、`CheckConfig`、`ModConfigSchema`、`ServiceIdentity`、App 自己的 `appConfig`、`--check-config` / `--print-config` / `--print-config-schema`。装配时机见 01 |
| `kit/internal/configschemagen` | 把 app 与 kit 全部 Mod 的声明快照成 `codegen/internal/roost/kitconfig_gen.go` |
| `codegen/internal/roost`（配置部分） | 从快照渲染生成配置段；doctor 的 `config-schema:<服务>` / `config-reads`；`roost config check` |
| `configdata` | 业务数据快照：表 / 单例对象 / custom / 外部表（Luban）定义、`Store` 的 Load / Reload / DryRun / Rollback、监听者协议、结果报告、键拼写检查、auto 表（`cfg` 标签） |
| `configdata/rules` | 数据列规则与键拼写规则的唯一实现，加载层与生成器共用；只依赖标准库 |
| `kit/configdata` | `config_data` Mod：读 `config_data.dir`、启动时 Load、按结果记指标、登记 `*configdata.Store` |
| `codegen/internal/tablegen` | Go 结构体 schema + CSV → JSON + 生成 loader（`roost generate` 默认执行） |
| `codegen/internal/cfggen` | YAML meta → 生成 Go 结构体与注册代码（可选，单独执行） |
| `featureflag` | 进程内开关表（名字 → 开 / 关），版本号单调 |
| `hotcode` | 补丁点注册、替换、还原、Go plugin 补丁包、admin 命令 |

跨分区：App 什么时候读配置、启动检查在启动序列里的位置、`singleton.*` / `shutdown.*` 等 App 自己的键见 [01 app 说明](01-app-lifecycle.md)；Nest 消息怎样携带请求上下文（快照代际随消息传递）见 [02 nest 说明](02-nest-entity.md)；kit 各服务的键见 [09 kit 服务](09-kit-services.md)；指标暴露、admin 端点的 Bearer 鉴权、仪表盘见 [11 可观测](11-observability.md)；`roost generate` / `project doctor` 的整体、生成工程模板见 [12 代码生成](12-codegen.md)。

---

## 1. 定位与边界

| | 服务配置 | 业务数据配置 |
| --- | --- | --- |
| 内容 | 基础设施与进程参数：Redis 地址、worker 数、超时、键前缀、密钥 | 游戏数值与内容：物品、怪物、刷怪表、开关表 |
| 来源 | `configs/service/config.<服务>.yaml`（`-c` 指定），viper 读入 | `config_data.dir`（缺省 `configs/data`）下的 JSON |
| 谁声明 | 读它的 Mod / 业务服务（配置结构体 + tag） | 业务 schema：tablegen 的 Go 结构体，或 cfggen 的 YAML meta |
| 何时读 | 启动时一次，**没有运行期 reload** | 启动 Load 一次，之后按需 Reload / Rollback |
| 检查在哪 | App 启动检查 + `LoadConfig`（同一份声明） | configdata 加载层（同一份规则） |
| 生成期反馈 | 生成器从声明写配置段；doctor 检查工程配置 | tablegen CSV 转换与 `-check`；cfggen 只查 meta |

**负责**

- 服务配置的声明形状、严格类型解析、范围 / 枚举 / 必填 / 生产密钥检查、跨键规则的调用时机、多份声明的合并与冲突、生成配置段、doctor 检查、源码守卫。
- 业务数据的定义、加载、规则、键拼写、热更 / 回滚的协议与可见性、结果日志与指标、两条生成管线。
- 进程内开关表与函数热补丁。

**不负责**

- App 启动 / 停机顺序本身（01）。
- 每个 kit Mod 的键含义（看各分区与 `<bin> <服务> --print-config`）。
- 多进程之间同步业务数据：每个进程各自 Reload，没有集群级“一起热更”（game-demo 的 GM 命令描述写明 “reload config data in THIS process”，`demo/internal/service/game/gm.go.tmpl:426`）。
- 配置下发系统（配置中心、环境变量覆盖）：app 只读一个 YAML 文件，**没有** `AutomaticEnv` / `BindEnv`（全仓 grep 无调用）；`internal/configschema/parse.go:71`、`:135` 注释里“环境变量覆盖”的说法没有对应实现。

→ [实现文档](../impl/07-config.md)对应：§1 包与文件地图。

## 2. 核心概念与术语

| 术语 | 含义 | 定义位置 |
| --- | --- | --- |
| 配置声明（ConfigSchema） | 一组键的声明（`[]configschema.Key`）。由配置结构体经 `app.SchemaOf` 得到时还记得结构体类型，检查时会执行 `ValidateConfig` | `internal/configschema/schema.go:63`；`app/config_schema.go:32` |
| 键（Key） | 完整点分键名 + 类型（bool / int / float / duration / string / strings / map / section）+ default / example / min / max / enum / required / secret / closed / help | `internal/configschema/schema.go:40` |
| 配置结构体 | 字段类型就是键类型、tag 写键名与规则的 Go 结构体；`LoadConfig` 把值读进它 | `app/config_schema.go:16` 注释 |
| `ModConfigSchema` | `ConfigSchema() ConfigSchema` 接口；Mod 与业务服务（`RegisterServer` 的 Service）都可以实现 | `app/config_schema.go:43` |
| `LoadConfig` | 框架与业务代码读服务配置的**唯一**入口：按声明一次读完、检查、调 `ValidateConfig`，全部错误一次返回 | `app/config_schema.go:68` |
| 启动检查 | App 在任何 Mod Init 之前，把 App、本服务全部 Mod、服务本身的声明逐份检查并合并 | `app/config_schema.go:85`；`app/app.go:150` |
| `ValidateConfig(production)` | 配置结构体的可选方法：声明表达不了的跨键规则，声明检查全通过后自内向外调用 | `internal/configschema/schema.go:70`；`internal/configschema/check.go:273` |
| 生产环境 | `env` / `app.env` / `environment` 任一为 `prod` / `production`（不分大小写） | `app/config_schema.go:168`、`:186` |
| starter 键 | 声明了 `example` 的键，生成器把它写成配置文件里的生效行；其余键只出现在 `--print-config` | `internal/configschema/schema.go:46`；`yaml.go:10` |
| 声明快照 | `kitConfigSchemas`：kit 与 app 全部声明的 Go 字面量，生成器与 doctor 用（codegen 不能导入 kit） | `codegen/internal/roost/kitconfig_gen.go`；`kit/internal/configschemagen/main.go:50` |
| 业务声明 | 进程声明里快照没有的键；doctor 编译工程并运行 `--print-config-schema` 读回 | `codegen/internal/roost/config_schema_doctor.go:42`、`:99` |
| Snapshot | 业务数据的一代：全部表、对象、custom，带 `Version` / `LoadedAt` / `Hash`，发布后不再改 | `configdata/configdata.go:39` |
| TableDef / ObjectDef / CustomDef | 表（有主键、可有二级索引）/ 单例对象 / 由已加载数据派生的运行时结构 | `configdata/configdata.go:400`、`:494`、`:556` |
| 外部表 | 外部工具（Luban）生成的聚合体，作为一个 custom 成员挂进快照 | `configdata/external.go:63` |
| auto 表 | 由行结构体的 `cfg` 标签推出主键、索引、规则的表（cfggen 用） | `configdata/auto.go:110` |
| 列规则（FieldRule） | required / unique / min / enum / ref，加载层在原始 JSON 上检查 | `configdata/rules/rules.go:30`；`configdata/fieldrules.go:16` |
| 键拼写规则 | 数据文件的键必须与字段 json 名逐字一致，只差大小写即拒绝（Rule `case`） | `configdata/rules/rules.go:165`；`configdata/keyspelling.go:22` |
| ReloadListener | 热更的四段回调：`ValidateReload` / `BeforeApplyReload` / `AfterApplyReload` / `RollbackReload` | `configdata/configdata.go:711` |
| ReloadOutcome | 每次 Load / Reload / Rollback 恰好一份的结果（阶段、版本、错误） | `configdata/configdata.go:914` |
| 钉住（ActiveSnapshot） | 请求上下文里带着准入时的那一代快照，同一请求内的读不跨代 | `configdata/configdata.go:1452` |
| tablegen / cfggen | 两条业务数据生成管线；维护者 2026-10-06 选 A：保持两条 | [B10 §2.4](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) |
| 补丁点（patch point） | `hotcode.Register(name, fn)` 登记的一个可在运行期替换的函数 | `hotcode/registry.go:110`（包级入口）、`:167`（`Registry.Register`） |

→ [实现文档](../impl/07-config.md)对应：§2 关键类型与数据结构。

## 3. 设计原因

| 取舍 | 结论 | 出处 |
| --- | --- | --- |
| 一个键的知识放在哪 | 放在读它的 Mod 的配置结构体上：字段就是读取结果，声明与读取同一行。以前散在四处（Mod 读取、app 三张手写清单、生成器字符串、doctor），加键漏一处没人报错 | [A4 ① §1、§2.1](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md) |
| 结构体 + tag 还是键表 + 读取句柄 | 选结构体：业务作者手写最少，“声明了没读”能用 AST 查。另一形式（`app.IntKey(...).Min(1)` 句柄）每个键写两次，留作备选 | A4 ① §2.1 末 |
| 什么时候检查 | 启动时一次、任何 Mod Init 之前、错误一次报全；`LoadConfig` 走同一套检查，范围外的值两处都拒绝。维护者配置原则：“规则只在运行时加载时强制一次” | A4 ① §2.2；`kit/config_schema_promises_test.go:115` |
| 为什么不报“没人声明的键” | 生成的配置会带别的进程才注册的 Mod 的键（例如只读 Mirror 的 `remote_entity.mirror.shutdown_timeout`），进程分不出笔误；交给 doctor | `app/config_schema.go:97` 注释；A4 ① §7.1 |
| 同一个键两处声明 | 必须完全相同（同一个结构体被共用，例如 `redis.*` 由 Redis Mod 与单实例锁共用），否则启动报错，不允许“谁先 Init 谁说了算” | `internal/configschema/schema.go:294` |
| 业务代码的键谁声明 | 业务服务本身（生成的 bootstrap 没有业务 Mod 的注册位）；写法与 Mod 相同 | A4 ① §7.2；RR-20261006-40 |
| 宽松解析还是严格解析 | 严格：`on` / `yes` 不是布尔、不带单位的时长、`8k` 都报错并点名键（viper 的 `cast` 会静默读成零值 / 纳秒） | `internal/configschema/parse.go:13` 注释；[A4 ②](../../feature/REFACTOR-2026-10-05-strict-config-reads.md)、RR-20261005-NC-190 |
| 生成器怎么拿到 kit 的声明 | codegen 不导入运行时（`TestCoreDependencyBoundary`），由 kit 侧程序把声明快照成 codegen 包里的数据文件；漂移由测试与 `go generate` 门禁抓 | A4 ① §2.4 |
| 业务数据规则在哪强制 | 只在 configdata 加载层（每次 Load / Reload）；生成期检查只是提前反馈，两处跑同一段代码。直接改 JSON 再 reload 也绕不过 | 维护者决定 B10（[B10 §2](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)） |
| 数据键大小写 | 敏感。encoding/json 大小写不敏感会让 `Level` 静默填进 `level`、几种拼写取最后一个 | 维护者原话“configdata 需要大小写敏感”（[方案](../../feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md)） |
| 热更的新快照何时可见 | 在 `AfterApplyReload` 之前就发布；AfterApply 失败才撤回，期间准入的请求读被撤回的那一代。不能接受的检查放进发布之前的阶段 | 维护者决定 C2（B10 / C2 §2.5） |
| 热更失败怎么看到 | Store 每次尝试恰好报告一次结果并写日志；kit 据此计指标，标签低基数，运维填的 reason 只进日志 | C2；N07 C-O5 / C-O6 |
| tablegen 与 cfggen 合不合并 | 不合并（维护者选 A）：两条管线服务两种工作方式（策划 CSV / 程序 YAML），真正需要统一的是规则，这一层已统一 | B10 §2.4；第十三轮 |
| 开关的来源 | 配置表是唯一来源；GM 临时翻转只在本进程、下次 reload 被表覆盖（两处来源静默不一致比一处更糟） | `demo/internal/service/game/flags.go.tmpl:18` 注释 |
| 补丁点为什么显式登记 | 补丁点是运维承诺（可以运行期替换、还原恢复原函数），集中在一处便于审阅 | `demo/internal/service/game/flags.go.tmpl:112` 注释 |

→ [实现文档](../impl/07-config.md)对应：§3 主流程、§9 历史与重要修复。

## 4. 怎么用

### 4.1 服务配置：声明一个 Mod 的键

```go
type shopConfig struct {
	app.ServiceIdentity // 需要 sid / server_type 时匿名嵌入，与 App 共用同一份声明
	MaxItems int           `config:"shop.max_items" default:"100" min:"1" max:"10000" help:"每个玩家商店最多上架的物品数"`
	Refresh  time.Duration `config:"shop.refresh_interval" default:"1h" min:"1m"`
	Region   string        `config:"shop.region" enum:"cn|us|eu" required:"true" example:"cn"`
	Secret   string        `config:"shop.sign_secret" secret:"true" example:"CHANGE_ME"`
}

func (*ShopMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(shopConfig{}) }
func (m *ShopMod) Init(cfg *viper.Viper) error  { return app.LoadConfig(cfg, &m.cfg) }
```

形状取自 `app/config_schema.go:16` 的注释；真实例子：`kit/configdata/configdata.go:22`（一个键）、`kit/ops/ops_mod.go:67`（含跨键规则）、`kit/saga/config.go:31`（嵌套、平铺、closed、map 全用到）。

**tag 语义**（解析在 `internal/configschema/schema.go:154`，检查在 `check.go:209`、`parse.go:221`）

| tag | 含义 | 细节 |
| --- | --- | --- |
| `config:"a.b"` | 键名 | 必须非空、全小写、不含空格与 `*`（`schema.go:143`）；嵌套时拼上外层前缀 |
| `default:"…"` | 键没写（或写成空值 `key:`）时的值，YAML 写法 | 不写 = 类型零值；`Of` 时就解析并按 min / max / enum 检查，写错即 panic（`schema.go:238`） |
| `min` / `max` | 闭区间 | 按类型比较（时长用 `1ns`、`1m`）。报错文本：下限 0 → `must not be negative`；整数 / 时长下限 1 → `must be positive`；其他 → `must be at least …` / `must be at most …`（`parse.go:221`） |
| `enum:"a\|b"` | 字符串可选值 | 只能用在 string 字段；比较忽略大小写与两端空白，**读出的值规范成小写**（`parse.go:181`）；空串不算违反（要必填另写 `required`） |
| `required:"true"` | 必须写且非空 | “空”指 nil、空白字符串、空列表；整数 / 布尔的 0 / false 不算空（`parse.go:207`）。报错带 example（不含占位符时）：`config: x is required (for example cn)` |
| `secret:"true"` | 生产环境必须非空且不以 `dev-` 开头（不分大小写） | 只在生产检查；`config: production requires non-dev <key>`（`check.go:236`） |
| `example:"…"` | 生成器写进配置文件的生效值（starter 键） | 可以是空串（仍算 starter）；可以带 `{project}` 占位符；不带占位符时 `Of` 会检查它合法 |
| `help:"…"` | 说明 | 生成器写成键上方的注释；写在嵌套结构体字段上时是段注释 |
| `config:"x,closed"` | 段下出现没声明的键报错 | `config: saga.step_defaults.timout is not a known key; saga.step_defaults takes backoff_max, …` |

**字段类型**：`bool`；`int` / `int8`～`int64` / `uint`～`uint64`（都是 int 类，超出字段宽度报 `does not fit in an int32`）；`float32` / `float64`；`time.Duration`；`string`；`[]string`（YAML 列表或逗号分隔串）；嵌套结构体；`map[string]T`（`kindOf`，`schema.go:216`）。其他类型 `Of` 时报 `unsupported config field type`。

**严格解析**（`internal/configschema/parse.go`）

| 类型 | 接受 | 拒绝（报错并点名键） |
| --- | --- | --- |
| bool | YAML 布尔、`strconv.ParseBool` 认的串（`true` / `FALSE` / `t` / `1`…）、整数 0 / 1 | `on` / `yes` / `off` / `no`、拼写错误（`:22`） |
| duration | `15s` / `500ms` / `1m30s`、0、空串 | 不带单位的非零数字（`needs a unit … a bare number would be read as nanoseconds`）、解析不了的串（`:42`） |
| int | YAML 整数、无小数部分的浮点（`1e3`）、十进制串 | `8k`、`1.5`、`10s`、布尔（`:73`） |
| float | 数字、十进制串 | 解析不了的串（`:98`）。注意 `NaN` 会被接受且绕过 min / max（§7.2 F8） |
| string | 任何标量（`123` 还原成 `"123"`），去两端空白 | 映射、列表（`:120`） |
| strings | YAML 列表、逗号分隔串；每项去空白、丢空项 | 其他（`:136`） |

### 4.2 嵌套、匿名嵌入、`_` 前缀平铺、map 通配、closed 段

| 写法 | 键名怎么拼 | 例子 |
| --- | --- | --- |
| 带 tag 的嵌套结构体 | 外层前缀 + `.` + 内层键（`schema.go:115`） | `kit/dataengine/mod.go:138` 的 `Outbox struct{…} \`config:"dataengine.outbox"\`` 下的 `workers` → `dataengine.outbox.workers` |
| 匿名嵌入（不写 tag） | 不加前缀，共享一组键（`schema.go:129`） | `app.ServiceIdentity` → `sid`、`server_type`；`mods.PersistenceConfig` → `persistence.engine`、`dataengine.enabled` |
| 前缀以 `_` 结尾 | 直接拼接，不加 `.`：一组同形的平铺键共用一个结构体 | `kit/saga/config.go:64` `Result consumerConfig \`config:"result_"\`` 下的 `ack_wait` → `saga.result_ack_wait` |
| `map[string]T` | 段名下每一级用 `*` 通配；元素是结构体时**总是 closed** | `kit/saga/config.go:37` `Steps map[string]map[string]stepBudgetConfig \`config:"steps"\`` → `saga.steps.*.*.timeout` 等 |
| `,closed` | 段下没声明的键报错；不写 closed 的段对多余键不报（交给 doctor） | `kit/saga/config.go:35` `step_defaults,closed` |

- map 的值写成标量会报 `config: saga.steps must be a map, got 3`（`check.go:138`）。
- map 元素名来自配置里实际出现的下一级键（`check.go:142`），所以 `saga.steps.<saga 类型>.<步骤名>` 不必事先声明类型名；它们是否对应已注册的定义由 saga 自己检查（06 分区）。
- 匿名嵌入的结构体如果有 `ValidateConfig`，它被提升为外层的方法、由外层那一次调用执行；**外层自己也定义了 `ValidateConfig` 时会遮住它**，要显式调用（`kit/dataengine/mod.go:160` 就这样做）。

### 4.3 跨键规则：`ValidateConfig(production bool)`

声明表达不了的规则写成配置结构体的方法：

```go
func (c *config) ValidateConfig(production bool) error {
	if c.AdminEnabled && c.AdminToken == "" { return errors.New("config: ops.admin_enabled requires admin_token: …") }
	if production && c.Enabled && !isLoopbackListenAddr(c.Addr) && !c.AllowPublicAddr { return fmt.Errorf("config: production ops.addr %q is not loopback; …", c.Addr) }
	return nil
}
```

（节选自 `kit/ops/ops_mod.go:82`。）规则：

- **只在全部声明检查都通过之后**调用（`check.go:33`）：类型都对了才谈得上跨键。
- **自内向外**：先嵌套结构体、再外层（`check.go:273`）；外层规则可以假定内层已成立。
- 现有实现：`appConfig`（`server_type` 必填、`sid` 正数、生产业务时钟偏移为 0，`app/config_schema.go:234`）、`singletonConfig`（三条时间关系，`app/singleton.go:146`）、Redis 的生产地址必填（`kit/redis/redis_mod.go:63`）、ops（上例）、`PersistenceConfig`（只能是 dataengine，`kit/mods/persistence.go:22`）、dataengine（告警水位 ≤ 硬上限，`kit/dataengine/mod.go:160`）、remoteentity（`kit/remoteentity/config.go:47`、`:120`）。
- **只有进程执行它**：启动检查与 `--check-config` 会跑；doctor 的 `config-schema` 用的是没有类型信息的键表，**不跑**跨键规则（§4.7）。

### 4.4 业务服务的声明

业务代码自己读的键由业务服务声明（生成的 bootstrap 没有业务 Mod 的位置）。game-demo 的写法：

- `demo/game/settings/settings.go.tmpl:27` `settings.Config`：嵌入 `app.ServiceIdentity`（与 App 声明相同，合并不冲突）、`activity.key_prefix` / `activity.groups_file`、`platform.key_prefix` / `platform.payment_secret`（全部 `required`，payment_secret 另加 `secret`），前缀空白检查在 `Activity` / `Platform` 的 `ValidateConfig`（`:54`、`:57`）。
- `demo/internal/service/game/service.go.tmpl:52` `func (*Service) ConfigSchema() app.ConfigSchema { return settings.Schema() }`。
- 读取：`settings.Load` / `Identity` / `LoadActivity` / `LoadPlatform`（`settings.go.tmpl:64`～`:105`），都经 `app.LoadConfig(registry.Config(), dst)`。只读需要的子集也可以：子集结构体的键必须与完整声明一致。
- 读本进程**框架 Mod** 的键（`dataengine.*`、`saga.*`）不要自己再声明一份，用那个 Mod 的访问函数：`kitsaga.StreamSettings(cfg)`、`kitdataengine.EffectSettings(cfg)`（A4 ① §7.2）。

错误前缀：服务的错误是 `service <名字>: config: …`，Mod 的是 `mod <名字>: config: …`，App 自己的不带前缀（`app/config_schema.go:94`）。

### 4.5 守卫：读了没声明 / 声明了没读

两条规则，框架（app、kit）与生成工程用同一份实现（`internal/configschema/guard.go`）：

| 规则 | 判定 | 违反时 |
| --- | --- | --- |
| 读了没声明 | 对 `*viper.Viper`（参数、字段、`var` 变量、`viper.New()` / `.Config()` 的返回值）调任何读方法（`Get*`、`IsSet`、`AllKeys`、`Sub`、`Unmarshal*` …，`guard.go:26`），或调 `app.ConfigBool` / `ConfigDuration` / `ConfigInt` / `ConfigInt64`（`guard.go:36`；`:123` 还识别已不存在的旧名 `NewConfigReader`）。判定在 `guard.go:66`。包级 `viper.GetString(...)`（全局 viper）不在识别范围 | 框架：`TestFrameworkModsReadConfigOnlyThroughDeclarations` 红；用户工程：doctor `config-reads` FAIL |
| 声明了没读 | 带 `config` tag 的字段，在同一个包（用户工程按整个工程）的非测试代码里没有任何 `.字段名` 选择子（`guard.go:144`） | 框架：`TestEveryDeclaredConfigFieldIsRead` 红；用户工程：doctor `config-reads` FAIL |

这是按名字的语法检查（不做类型推导），误报时改名或改写法，不加豁免（`guard.go:23`）。唯一例外是 app 的 viper 适配器与单键读取本身（`app/config_schema.go`、`app/config_values.go`）。`app.ConfigBool` 等单键读取保留给工具和测试，规则与声明读取相同。

### 4.6 生成器：配置段从声明生成

```bash
go generate ./codegen/internal/roost      # = go run ../../../kit/internal/configschemagen -out kitconfig_gen.go（catalog.go:16）
```

- 快照按生成器认识的单元分组：`app`、catalog 的 Mod 名（`dataengine` 段同时含 nest 与持久化的键，`remote_entity` 段同时含 Mirror 的停机预算）、框架服务名、`<服务>.client`（`kit/internal/configschemagen/main.go:50`）。
- 生成的服务配置里，每个 Mod 的段是 `kitConfigSchemas[name].StarterYAML(...)`（`codegen/internal/roost/catalog.go:23`、`:29`）：有 `example` 的键写成生效行，`help` 写成上方注释；框架服务的段（`frameworkConfigSection`）把 `{project}` 换成工程名，Mod 的段不做替换。没有 example 的键不写进 starter 配置，用 `--print-config` 查。
- 生产示例沿用文本变换（`127.0.0.1` → `CHANGE_ME` 等），细节归 12 分区。
- 改了 kit 的声明不重新生成：`go generate ./...` 之后工作区不干净（CI `.github/workflows/ci.yml:62`），`TestKitConfigSchemasMatchKitDeclarations` 红。

### 4.7 命令行与 doctor

| 命令 | 做什么 | 用哪份声明 | 跑 `ValidateConfig` | 报“没声明的键” |
| --- | --- | --- | --- | --- |
| `<bin> <服务> --check-config [-c 文件]` | 读配置、按启动检查同一套检查，通过打印 `config ok: <路径>`，不启动任何 Mod（`app/app.go:96`） | 进程里的全部声明（App + Mod + 服务） | 是 | 否（只有 closed 段与 map 下） |
| `<bin> <服务> --print-config` | 打印全部声明的参考 YAML：starter 键写 example（含占位符的写 default），其余写 default，带 help（`app/app.go:158`、`yaml.go:24`） | 同上 | — | — |
| `<bin> <服务> --print-config-schema` | 把全部声明打印成 JSON 键表（`[]configschema.Key`），给 doctor 读（`app/app.go:170`） | 同上 | — | — |
| `roost project doctor` → `config-schema:<服务>` | 检查 `configs/service/config.<服务>.yaml`、`config.<服务>.prod.example.yaml`、`deploy/k8s/base/secret.<服务>.example.yaml`（后两份按生产检查，Secret 取 `stringData["config.yaml"]`）（`config_schema_doctor.go:163`） | 框架快照（按 bootstrap 给该服务注册的 Mod 合并）+ 从进程读回的业务声明 | **否** | 是：框架段里没有任何框架 / 业务声明的键 FAIL；只有业务用到的段里没声明的键 FAIL；有框架声明但本服务进程不声明的键 WARN |
| `roost project doctor` → `config-reads` | 对整个工程跑 §4.5 两条守卫（`config_schema_doctor.go:295`） | — | — | — |
| `roost config check --service/--all [--production]`（`make config-check` / `config-check-all`，在 `make ci` 里） | **只查** YAML 能解析、有 `sid`；`--production` 时查 `change_me` / `127.0.0.1` / `localhost` / `dev-` 字样（`codegen/internal/roost/doctor.go:810`） | **不用声明** | 否 | 否 |

要点：

- **发布前检查真实生产配置用 `--check-config`，并总是显式写 `-c`**：默认路径不存在时它按缺省值检查并照样打印 `config ok`（[01 说明 §4.2](01-app-lifecycle.md#42-cli)）。
- doctor 要编译工程（一次 `go build -mod=readonly`，`config_schema_doctor.go:61`）并对每个服务运行一次 `--print-config-schema`。工程编译不过时只做框架那一半并在 Detail 里说明；能编译但读不出声明（链接失败、`bootstrap.New()` 报错、两份声明冲突）时 `config-schema:<服务>` 为 FAIL（`:192`）。
- `make ci`（Makefile 模板的 `ci:` 目标，`codegen/internal/roost/render.go:993`：`ci: fmt-check vet glsvet test test-race check-generated config-check-all id-check`）**不含** doctor，也不跑 `--check-config`：生成工程的 CI 不按声明检查配置值（A4 ① §7.1 已写明）。按声明的检查只在 doctor、`--check-config` 与进程启动时发生。

### 4.8 业务数据：configdata 基本用法

```go
reg := configdata.NewRegistry()
configdata.MustRegisterTable(reg, configdata.TableDef[int64, Item]{
	Name: "item", File: "item.json", Key: func(v Item) int64 { return v.ID },
	Rules: []configdata.FieldRule{{Field: "id", Required: true, Unique: true}, {Field: "name", Required: true}},
	ValidateTable: func(ctx *configdata.BuildContext, t *configdata.Table[int64, Item]) error { /* 跨行约束 */ return nil },
})
store := configdata.NewStore(reg, "configs/data")
snap, err := store.Load(ctx)                           // 首次加载
item, ok := configdata.MustTableFrom[int64, Item](snap, "item").Get(1)
```

生产工程里这些都是生成的：

- tablegen 生成 `configs/generated/gen_table_config.go`：`RegisterGeneratedConfigData(r)`、带 `//roost:register phase=config` 的 `RegisterConfigData()`（注册到 `configdata.DefaultRegistry()`，由静态注册在 `app.New` 之前执行），每张表 `<Type>TableFrom(snap)`、`<Type>Table()`（当前请求钉住的快照）、`<Type>By<Key>(id)`，每个对象 `<Type>ConfigFrom(snap)`（`codegen/internal/tablegen/main.go:786`～`:857`）。
- kit 的 `config_data` Mod（`kit/configdata/configdata.go:44`）读 `config_data.dir`，用 `DefaultRegistry()` 建 Store，`Start` 时 `Load`（失败即启动失败，`:96`），并把 `*configdata.Store` 登记在能力名 `config_data` 下（`kit/mods/name.go:42`）。
- 业务读：`generated.ItemByID(id)`（game-demo `demo/game/entities/player/bag_component.go.tmpl:65`）、`generated.SpawnTable().Rows()`（`demo/game/scene/runtime/refresh.go.tmpl:63`）。

| 定义 | 用途 | 关键点 |
| --- | --- | --- |
| `TableDef[K, V]` | 行列表 + 主键（+ 二级索引 `IndexDef`，键为字符串） | 主键重复整次拒绝（`configdata.go:201`）；`Validate` 每行一次、`ValidateTable` 每表一次（空表也跑），都在规则与 ref 之后（`:466`） |
| `ObjectDef[V]` | 单例对象（一个 JSON 对象） | 规则只有 required / min / enum（unique / ref 对单个对象无意义，注册时拒绝，`fieldrules.go:73`） |
| `CustomDef[V]` | 从已加载的表 / 对象派生的运行时结构 | 在全部表 / 对象**校验之后**才 build（可以假定引用完整，`configdata.go:1373`）；值必须能被哈希（全是未导出字段的结构体要调 `Snapshot.SetFingerprint`，否则 build 失败） |
| `RegisterExternalTables` | 外部工具（Luban `code_go_json`）生成的聚合体 | 回调只能经注入的 `read` 读文件（禁止逃出数据目录、build 返回后再读报错、一个文件都没读即失败）；读到的文件折成指纹进 `Hash`（`external.go:63`）；示例 `examples/lubanreal` |
| `RegisterAutoTable[K, V]` | 由 `cfg` 标签推出映射（cfggen 用） | `key` / `index[=name]` / `skipempty` / `required` / `unique` / `min=` / `enum=a\|b` / `ref=` （`auto.go:22`）；标签写错在注册时失败 |

数据文件形状（`configdata/rules/rules.go:102`）：表是行的数组，对象是一个 JSON 对象；也可以是**只含一个** `rows` / `records` / `data` 键的对象包着它。空文件、`null`、包装键为 null、多个包装键并存都拒绝。

### 4.9 两条生成管线：tablegen 与 cfggen（维护者选 A）

| | tablegen | cfggen |
| --- | --- | --- |
| 手写什么 | `configs/schema/*.go` 里的 Go 结构体，`//roost:table name=… key=…` / `//roost:object`，字段带 `csv` / `json` / `title` 与规则标签 | 一个 YAML：`configs/schema/cfg.yaml`（tables / globals / beans，类 Luban），不写 Go 结构体 |
| 数据 | 策划编辑的 CSV（`configs/table`），转换成 `configs/data/*.json`；另生成 CSV 模板（`configs/table_template`） | 直接维护 JSON |
| 单例配置 | `//roost:object` | `globals`（`objects` 是兼容别名） |
| 嵌套结构 | 字段类型写 Go 类型，CSV 单元格里写 JSON（`parseCell`，`tablegen/main.go:667`） | `beans`（含 `[]bean`，检查 bean 环） |
| 二级索引 | 无 | `index: true` / `index: <名字>`，`skipempty` |
| 分组导出 | 无 | `groups`（Luban 式，客户端专用字段不进服务端绑定） |
| 怎么跑 | `roost generate` 默认执行三步：CSV 模板、CSV → JSON（`configs/table` 有 CSV 才跑）、Go 访问代码 → `configs/generated`（`codegen/internal/roost/generate.go:132`～`:157`） | 可选，单独执行：`go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/cfggen -meta configs/schema/cfg.yaml -out configs/cfg`；输出不能与 tablegen 共用 `configs/generated`（两边都生成 `RegisterConfigData`，T-233） |
| 生成的读取 API | `<T>Table()`（钉住的快照）、`<T>By<Key>(id)`、`<T>TableFrom(snap)`、`<T>ConfigFrom(snap)` | `<T>TableFrom(snap)`、`<T>By<Index>(snap, v)`、`<T>From(snap)`——**都要显式传快照**，传 `configdata.ActiveSnapshot()` 才是钉住的那一代 |
| 生成期数据检查 | CSV 转换与 `-check` 跑同一组规则（`rules.Check`）与表头 / 键拼写 | 无（不处理数据），只查 meta：未知字段（`KnownFields`）、类型、规则声明、ref 目标与类型 |
| 运行时规则 | 生成的 `TableDef.Rules` / `ObjectDef.Rules` 字面量 | 表：`cfg` 标签 → auto 表解析成同一组 `FieldRule`；globals：`ObjectDef.Rules` 字面量 |
| 适合 | 策划用表格填数、CSV 为交付物 | 程序维护的配置、需要 bean / 二级索引、不想手写 Go 结构体 |

注意两条管线**都能写单例配置**：tablegen 用 `//roost:object`（CSV 的第一行数据就是这个对象），cfggen 用 `globals`。区别在工作方式（策划填 CSV / 程序写 YAML + JSON），不在能力。

两条管线的规则最终都落到 `configdata/rules`，由 configdata 在每次加载与 reload 时用同一个检查器执行（B10）。

**tablegen 最小示例**（game-demo 的真实 schema，`demo/configs/schema/item.go.tmpl:16`）：

```go
//roost:table name=item key=ID
type Item struct {
	ID       int64  `csv:"id" json:"id" title:"ID" required:"true" unique:"true"`
	Name     string `csv:"name" json:"name" title:"Name" required:"true"`
	MaxStack int32  `csv:"max_stack" json:"max_stack" title:"MaxStack" required:"true"`
	Attack   int64  `csv:"attack" json:"attack" title:"Attack"`
	HP       int64  `csv:"hp" json:"hp" title:"HP"`
}
```

- 不写 `key=` 时主键是第一个字段（`codegen/internal/tablegen/main.go:292`）；主键隐含 `unique`（`:569`）。**`key=` 写错（不存在的字段）不报错，静默回落到第一个字段**（`:1104`～`:1113`，§7.2 F5）。
- 标记必须紧贴在 `type` 的上一行（`:231`）；中间隔一行注释，这个类型就不会生成任何代码，也不报错。
- 规则标签：`required:"true"`、`unique:"true"`、`min:"<n>"`、`enum:"a|b"`、`ref:"<表名>"`（目标必须是同一 schema 的表、字段类型等于目标主键类型，否则生成失败，`:1031`）、`parser:"…"`（单元格原样当字符串）。
- CSV 前几行：表头（csv 名）、可选的标题行、类型行、规则行（与 meta 逐格相同才被识别并跳过，`:653`）。空的 required 单元格写成 null，由同一检查器报 required（`:669`）。
- **每个字段都写 `json` 标签**，并与 `csv` 名一致。不写时生成器用 csv 名（缺省 snake_case）当 JSON 键（`:275`），运行时却按 Go 字段名匹配：没有规则的 `MaxStack` / `HP` 读不到 `max_stack` / `h_p`、**静默为 0**；`Level` 遇到 `level` 被当成大小写错误整次拒绝（`case: key "level" must be spelled "Level"`）；带规则的字段或主键在注册时就失败（`rule field max_stack matches no field of …`）（§7.2 F1）。
- 单例（`//roost:object`）的 CSV 只取第一行数据，多余的行静默丢弃（`:375`）。
- `-json … -check`：只检查、不写（`tablegen -meta ./configs/schema -json ./configs/data -check`），reload 前可以先跑。
- `configs/data/_manifest.json`（v2）记录生成器拥有的 JSON 及其哈希；未登记的 JSON 视为手写数据、永远保留；登记过的在 meta 删除后退役（内容被改过则报错不删）（`tablegen/main.go:344`；规则详见 [CODEGEN_REFERENCE](../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md) §9）。

**cfggen 最小示例**（仓库内可运行：`examples/configgen`，`cd examples && go run ./configgen`）：

```yaml
package: cfg
tables:
  - name: monster
    key: id
    fields:
      - { name: id,       type: int32, required: true }
      - { name: scene_id, type: int32, index: true }
      - { name: drop_id,  type: int32, ref: drop }
      - { name: level,    type: int32, min: 1 }
      - { name: kind,     type: string, enum: [normal, boss] }
globals:
  - name: world
    fields:
      - { name: width, type: int32, required: true, min: 1 }
```

**globals 的规则**（第十二轮，`codegen/internal/cfggen/main.go:447`～`:466`、`:796`）：

| 选项 | tables | globals | beans |
| --- | --- | --- | --- |
| `required` / `min` / `enum` | `cfg` 标签 → auto 表规则 | `ObjectDef.Rules` 字面量 | 拒绝（`:496`） |
| `unique` | 支持 | 拒绝（单个对象没有可比的行） | 拒绝 |
| `ref` / `index` / `skipempty` | 支持 | 拒绝（对象注册路径不读 `cfg` 标签，写了也不会执行） | 拒绝（`:493`） |
| `key` | 必填 | 拒绝 | — |

enum 值不能为空、不能含 `,` `|` `"` `` ` ``；min 只用于数值类型；enum 不用于浮点（`main.go:637`）。规则声明在生成时就用 `rules.Rule.Validate` 检查，与运行时同一段代码。

### 4.10 规则与大小写

| 规则 | 语义（在原始 JSON 行上检查） | 错误示例 |
| --- | --- | --- |
| required | 每行都必须出现且不为 null（缺列与零值分得清） | `configdata: table spawn row 1 (key 1) field template: required: missing or null` |
| unique | 任两行的值不同；缺省或 null 的行不参与。值比较用规范形式（`1`、`1.0`、`1e0` 相同；字符串 `"1"` 与数字 `1` 也相同） | `… field id: unique: value 3 repeats row 1` |
| min | 值是 JSON 数字且 ≥ 下限 | `… field hp: min: value 0 is below min=1` |
| enum | 值的字符串形式（字符串原样，数字规范十进制，布尔 true / false）在列表里，**大小写敏感** | `… field kind: enum: value Boss is not one of [normal boss]` |
| ref | 全部表加载后检查：非零值必须是目标表的主键；`required` + `ref` 时零值也要是。目标表不存在 / 键类型不兼容是 schema 错误（空表也查） | `… field drop_id: ref: references missing drop key 999` |
| case | 数据文件的键必须与字段 json 名逐字一致（嵌套结构体、切片元素、map 的值逐层查）；只差大小写的键或一行里同一字段几种拼写都拒绝 | `… field level: case: key "Level" must be spelled "level" (keys are case-sensitive)` |

- 规则字段 `FieldRule.Field` 也必须逐字是 json 名；指向不存在的字段、`min` 用在字符串上、`enum` 用在浮点上，**注册时**就失败（`configdata/fieldrules.go:33`）。
- **未声明的键**（与任何字段名都不只差大小写）：缺省模式忽略；严格模式（`Store.SetStrictJSON(true)`）报 `unknown field`。kit 的 Mod 不打开严格模式，也没有配置键能打开它（`kit/configdata/configdata.go` 不调用 `SetStrictJSON`）。
- 自带 `UnmarshalJSON` 的类型、`interface{}`、map 的键不做拼写检查（`keyspelling.go:63`）。
- 跨行 / 跨表的业务约束写 `ValidateTable` / `Validate`，它们在规则之后运行。业务代码不需要再为“缺列 / 零值 / 悬空引用”写防御代码。

### 4.11 热更与回滚

**触发**：框架没有内置的 reload 入口；`*configdata.Store` 的 `Reload` / `ReloadWithReason` / `DryRun` / `Rollback` 由业务调用。game-demo 提供 GM 命令 `gm.config.reload {reason?}`（`demo/internal/service/game/gm.go.tmpl:419`），只作用于**收到命令的那个进程**。game-demo 没有 Rollback 与 DryRun 的命令（B10 文末已列为后续）。

```bash
# 改 CSV 时先生成 JSON（reload 不读 CSV，T-231）
roost generate
# 可选：用同一个检查器先检查
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/tablegen -meta ./configs/schema -json ./configs/data -check
# 然后经 admin 端点对每个进程执行 gm.config.reload（admin 的鉴权与调用方式见 11 分区）
```

**一次 Reload 的阶段**（`configdata/configdata.go:860`、`:1039`）：

| 阶段 | 做什么 | 失败时 |
| --- | --- | --- |
| build | 读全部文件、拼写、规则、ref、`Validate*`、custom build、算哈希 | 什么都没发布；`stage=build` |
| validate | 监听者 `ValidateReload`（与 DryRun 互斥串行） | 什么都没发布；`stage=validate` |
| before_apply | 监听者 `BeforeApplyReload` | 已成功的那些监听者逆序收到 `RollbackReload`；`stage=before_apply` |
| **发布** | `Current`、`DefaultStore`、fctx 运行时配置一起切到新的一代 | — |
| apply | lifecycle `config.reload` 事件，然后监听者 `AfterApplyReload` | **撤回**：Current 回到旧的一代，全部 BeforeApply 成功过的监听者逆序收到 `RollbackReload`；`stage=apply` |

- 首次 Load 失败不调用任何 `RollbackReload`（没有可回去的一代，监听者不得把 nil `Old` 当成“回到缺省”）。
- `DryRun(ctx, reason)`：build + validate，不发布、不报告结果，不持 Store 锁（长 DryRun 不阻塞紧急 Rollback）。
- `Rollback(ctx, reason)`：把**上一代**重新发布，走同一套监听者协议。只有一级撤销：回滚成功后“上一代”被清空，再回滚报 `configdata: previous snapshot not found`（`configdata.go:1136`）。
- 版本号来自单调计数器，失败、撤回、DryRun 也占号：成功后版本变大但不一定 +1；Rollback 回到上一代的版本号（版本会回落）。
- 监听者回调里的 panic 变成错误、走失败路径（`safeReloadCall`）；回调里不要调 `runtime.Goexit` / `t.Fatal`。

**可见性契约**（维护者决定 C2，`configdata/doc.go:16`）：

- 发布之后准入的请求读新的一代；AfterApply 之前就已发布。
- 发布到撤回之间准入的请求，整个生命周期读的都是被撤回的那一代；同一请求内两次读不会跨代。
- 回滚之前准入的请求读的仍是被回滚的那一代。
- 不能接受“读到会被撤回的一代”的检查放进 `ValidateReload` / `BeforeApplyReload`。

**一次请求读哪一代**：`fctx.NewContext` 创建请求上下文时把当时的运行时配置（configdata 发布后就是当前快照）放进 `Context.Config`；`configdata.ActiveSnapshot()` 先取它，没有才取 `Current()`。Nest 消息带着发送方的上下文快照（含这一代：同步分支 `nest/client.go:205`，异步分支 `:216` → `nest/nest.go:686`），接收方用 `WithSnapshot(msg.Context)` 建上下文（`nest/nest_dispatch.go:245` → `fctx/context.go:244`），所以由一个请求派生的 Nest 消息也读同一代（[02 说明](02-nest-entity.md)）。没有请求上下文的后台 goroutine 读 `Current()`，两次读之间可能跨代。

**监听者**：

```go
unsubscribe := store.AddReloadListener(configdata.ReloadHook{
	HookName:   "game.featureflags",
	AfterApply: func(ctx context.Context, e configdata.ReloadEvent) error { return publish(store.Current()) },
	Rollback:   func(ctx context.Context, e configdata.ReloadEvent, cause error) { /* 把副作用退回 e.Old */ },
})
```

有副作用的 `AfterApply` **要写 `Rollback`**：撤回时只有 BeforeApply 成功过的监听者收到 `RollbackReload`，不写就留着新一代的副作用（game-demo 的开关监听者就没写，§7.2 F2）。

### 4.12 featureflag

```go
featureflag.DefaultStore().Replace([]featureflag.Flag{{Name: "purchase", Enabled: true}}) // 整体替换，版本 +1
if featureflag.Enabled("purchase") { /* … */ }                                            // 未定义的名字读作关
featureflag.Set(featureflag.Flag{Name: "purchase", Enabled: false})                       // 单个覆盖，版本 +1
```

- 进程内、读写锁保护的 map，没有持久化、没有跨进程同步（`featureflag/flags.go:15`）。
- 不随请求钉住：一次请求里两次 `Enabled` 之间可能翻转。所以只在**入口**读（事务打开之前），不在事务中途读（`demo/game/flags/flags.go.tmpl` 注释）。
- game-demo 的接法（`demo/internal/service/game/flags.go.tmpl:42`）：表 `featureflag`（`demo/configs/schema/feature_flag.go.tmpl:16`，数据 `configs/data/feature_flag.json`）是唯一来源；启动时发布一次，每次 reload 在 `AfterApply` 里 `Replace`；GM `gm.flag.set` 只改本进程、下次 reload 被表覆盖；`gm.flag.list` 列出当前值；代码读到但表里没有的开关打 Warn。

### 4.13 hotcode

```go
// 启动时登记（game-demo：demo/internal/service/game/flags.go.tmpl:123）
hotcode.Register("rewards.level_up", OriginalLevelUpReward)
// 调用处每次解析
reward := hotcode.Resolve("rewards.level_up", OriginalLevelUpReward)(level)
```

- `Replace(name, fn, meta)` 要求签名**完全相同**（`ErrTypeMismatch`）；`Resolve[T]` 在 T 与登记签名只差命名时转换，签名真不同时回落 fallback 并计入 `PointInfo.ResolveMismatches`（`hotcode/registry.go:134`）。
- `Revert(name)` 回到**原函数**，不是上一个补丁。
- Go plugin 补丁包：导出 `PatchBundle`（实现 `hotcode.Bundle`），`LoadPlugin(path)` 只接受 `.so`，只在 darwin / linux / freebsd 上可用（`hotcode/plugin.go:27`；其他平台返回不支持）。Apply 失败或 panic 时，被它改过的点恢复成加载前那一代（`ApplyBundle`，`registry.go:261`）。Go 运行时不能卸载 plugin。
- admin 命令（`hotcode.RegisterAdminCommands(reg)`，`hotcode/admin.go:28`）：`hotcode.list`、`hotcode.revert {name}`、`hotcode.load_plugin {path}`。必须传 `app.Lookup` 得到的 admin 实例（game-demo 在 `gm.go.tmpl:502`）。`load_plugin` 会执行该路径上的任意代码，安全边界就是 admin 令牌（11 分区）。
- 能登记成补丁点的函数必须每次调用独立：补丁在两次调用之间换掉它，需要新旧两版对“进行中状态”达成一致的函数不适合。

→ [实现文档](../impl/07-config.md)对应：§3 主流程、§5 并发。

## 5. 配置

**本分区自己读的服务配置键只有一个**：

| 键 | 缺省 | 范围 / 校验 | 说明 |
| --- | --- | --- | --- |
| `config_data.dir` | `configs/data` | 字符串；启动 Load 时目录必须存在且是目录 | 业务数据目录，相对进程工作目录（`kit/configdata/configdata.go:23`）。生成的镜像把数据拷到 `/app/configs/data`（`codegen/internal/roost/catalog.go:37` 注释） |

App 自己的键（`sid`、`server_type`、`env`、`log.*`、`time.logic_offset`、`metrics.*`、`shutdown.total_timeout`、`singleton.*`）见 [01 说明 §5](01-app-lifecycle.md#5-配置)。全部框架键的完整列表、缺省值与说明不在文档里逐键抄写：用 `<bin> <服务> --print-config` 打印（它就是声明本身）。

**服务配置的通用规则**（适用于每个声明的键）：

| 规则 | 效果 |
| --- | --- |
| 键没写 | 取 `default`；没有 default 取类型零值 |
| 写了空值（`key:` 或 `key: ~`） | 当作没写 |
| 写了 0 或负数 | 按 min / max 检查，不会静默换成缺省值（A4 ① 起的行为变化）。要缺省就不写这个键；文档写着“0 交给 core 决定”的键声明为 `min:"0"` |
| 写了声明之外的键 | 进程不报（closed 段与 map 下除外）；doctor 判 FAIL 或 WARN |
| 同一个键两份声明不同 | 启动报 `config: keys declared differently by two mods: <键>` |
| 生产环境 | 打开 `secret` 检查与各 `ValidateConfig` 里的生产规则（ops 不绑公网、Redis 地址必填、业务时钟偏移为 0） |

**业务数据的“配置”**：没有服务配置键控制热更或严格模式；严格 JSON 只能由代码调 `Store.SetStrictJSON(true)` 打开。

→ [实现文档](../impl/07-config.md)对应：§7 持久化 / 协议格式。

## 6. 运行与运维

### 6.1 服务配置的错误

启动检查失败发生在日志初始化**之前**，错误只出现在 stderr（[01 说明 §6.2](01-app-lifecycle.md#62-停机日志与退出)）。常见文本：

| 错误 | 原因 | 处理 |
| --- | --- | --- |
| `config: singleton.enabled must be true or false, got "on"` | YAML 1.2 下 `on` 是字符串 | 写 `true` / `false`（T-256） |
| `config: singleton.ttl = 15 needs a unit (for example 15s or 500ms)` | 时长没写单位 | 写 `15s` |
| `config: <键> must be a whole number, got "8k"` | 整数写了单位 / 小数 | 写十进制整数 |
| `mod dataengine: config: dataengine.outbox.workers must be positive, got 0` | 0 / 负数不再静默取缺省 | 删掉这一行用缺省，或写正数 |
| `config: <键> must be one of a, b, got "x"` | 枚举拼错 | 按列表写（大小写无所谓） |
| `config: <键> is required (for example …)` | 必填键没写 | 照示例补上 |
| `config: production requires non-dev <键>` | 生产环境密钥为空或 `dev-` 开头 | 换真实密钥 |
| `config: <段>.<键> is not a known key; <段> takes …` | closed 段里拼错 | 按列出的名字写 |
| `config: keys declared differently by two mods: …` | 两个 Mod（或服务）对同一键的声明不同 | 代码问题：共用同一个结构体 |
| `service game: config: activity.key_prefix is required` | 业务服务的声明 | 补业务键（RR-20261006-40 起启动时检查） |

同一个错误值可能被报多次：嵌入 `app.ServiceIdentity` 的每个 Mod 都会对 `sid: abc` 各报一行（`mod nats: config: sid must be a whole number …`），按第一条处理即可（§7.2 F9）。

### 6.2 业务数据的日志与指标

| 信号 | 何时 | 内容 |
| --- | --- | --- |
| Info `config reload applied` | Load / Reload 成功 | `op=reload reason version live elapsed` |
| Info `config rolled back` | Rollback 成功 | `op=rollback …` |
| Warn `config reload failed: the live generation is unchanged` | build / validate / before_apply 失败，或 Rollback 失败 | 加 `stage`、`error` |
| Warn `config reload reverted: the generation was published and taken back; requests admitted in between read it` | apply 阶段撤回 | 加 `stage=apply`、`error` |
| 计数 `configdata.reload.total{result=ok\|failed}` | 每次 Load / Reload 一笔（含启动 Load） | Prometheus 名 `configdata_reload_total` |
| 计数 `configdata.rollback.total{trigger=apply_failed\|operator}` | Reload 撤回一次记 `apply_failed`；`Store.Rollback` **成功**记 `operator`；失败的 Rollback（**包括发布后又被撤回的 Rollback**）不计，只有日志 | `configdata_rollback_total`；第一次递增前不存在，No data 是好消息 |
| 仪表 `configdata.version` | 每次结果后设成**正在服务**的一代 | `configdata_version`；撤回后回到旧版本，回滚后回落 |

（`kit/configdata/configdata.go:72`～`:92`；仪表盘与 README 见 `demo/deploy/dev/observability/README.md.tmpl`。）

### 6.3 常见故障

| 现象 | 原因 | TROUBLESHOOTING |
| --- | --- | --- |
| reload / 启动报 `… required: missing or null`（或 unique / min / enum / ref），`stage=build` | 数据违反 schema 规则；拒绝期间旧快照一直在用 | T-261 |
| reload / 启动报 `… case: key "Level" must be spelled "level"`，或注册报 `rule field Level matches no field of …` | 键大小写不一致 | T-275 |
| 改了 `configs/table/*.csv`，`gm.config.reload` 返回 ok、版本前进，数据没变 | reload 只读 `configs/data` 的 JSON | T-231 |
| `roost generate` 报 `ref target "Z" is not a table in this schema`；reload 报 `references missing …` | ref 声明或数据错 | T-241 |
| cfggen 生成后 `RegisterConfigData redeclared` | cfggen 输出与 tablegen 共用了 `configs/generated` | T-233 |
| 生产启动报 `config: production requires redis.addr or redis.cluster_addrs` | 生产 Redis 地址规则（`kit/redis/redis_mod.go:65`） | T-286 |
| 生产启动报 `config: production requires time.logic_offset = 0` | 业务时钟偏移只给测试环境 | T-270 |
| 表数据某列全为 0，没有任何报错 | tablegen schema 字段漏写 `json` 标签（多词字段名） | 未登记（§7.2 F1） |

见 [TROUBLESHOOTING](../../TROUBLESHOOTING.md)。

→ [实现文档](../impl/07-config.md)对应：§6 失败与不确定结果处理。

## 7. 保证与不保证

### 7.1 契约

**服务配置**

- 启动时，App 的、本服务全部 Mod 的、服务本身的声明在任何 Mod `Init` 之前检查完，类型 / 范围 / 枚举 / 必填 / 生产密钥 / closed 段 / 跨键规则的全部错误一次返回，按主人点名。
- Mod `Init` 里 `LoadConfig` 读到的值与启动检查用的是同一份声明，范围外的值两处都拒绝；读出的值已经去掉两端空白，枚举值已是小写。
- 两份对同一键的声明不同，进程起不来。
- 框架（app、kit）与 game-demo 生成工程里，读配置只经声明、每个声明的字段都被读（守卫测试）；用户工程靠 doctor `config-reads`。
- 生成器写出的配置与 kit 当前声明一致（快照漂移即红），且通过按 Mod 合并的完整检查（`TestGeneratedConfigsMatchDeclarations`、`TestA4GeneratedConfigsPassValidation`）。

**业务数据**

- 每次 Load / Reload / DryRun 对每张表、每个对象执行键拼写检查与声明的规则；违反就整次失败，旧快照继续服务。
- 发布与撤回作为一个整体在 Store 锁与全局发布锁下完成；撤回只恢复仍指向本次提交的全局槽位（另一个 Store 同时发布不会被冲掉）。
- 每次 Load / Reload / Rollback 恰好一份 `ReloadOutcome`、一条日志；kit 据此恰好一笔计数。
- 一次请求（及其派生的 Nest 消息）内读不跨代。
- `Snapshot.Hash` 覆盖每张表的行内容、对象、custom（外部表用读入文件的指纹）；内容变化一定反映在哈希上（序列化不出的值让 build 失败而不是哈希不变）。

### 7.2 已知限制与本次核对发现的问题

已知限制（设计如此或已有文档）：

- 进程不报拼错的服务配置键；`make ci` 不按声明检查配置；doctor 不跑 `ValidateConfig`。真实生产配置请用 `<bin> <服务> --check-config -c <文件>`。
- 服务配置没有运行期 reload，改了要重启。
- 只有一级 Rollback；game-demo 没有 Rollback / DryRun 的 GM 命令；热更只作用于一个进程。
- 快照“不可变”靠约定：`Table.Get` 返回行值、`Rows()` 只浅拷贝切片，行里的切片 / map / 指针与快照共享，修改会影响所有读者（推断自 `configdata/configdata.go:251`、`:271`，无深拷贝）。
- featureflag 不随请求钉住、不跨进程；GM 覆盖被下次 reload 冲掉。
- hotcode 的 `Revert` 只能回到原函数；admin 的 `load_plugin` 不保留 Bundle，`Bundle.Revert` 无法经 admin 调用（`hotcode/admin.go:64`～`:68`）；plugin 不能卸载。

本次核对发现、尚未登记的问题（条件、证据 `path:line` 与修复方向见[实现文档 §11](../impl/07-config.md#11-源码疑点与文档不一致)，编号一致）：

| # | 问题 | 什么时候出事 |
| --- | --- | --- |
| F1 | tablegen schema 字段漏写 `json` 标签：生成器按 snake_case 写 JSON 键，运行时按 Go 字段名读 | 没有规则的多词字段（`MaxStack`、`HP`）**静默为 0**；`Level` 这种只差大小写的整次加载失败；带规则的字段或主键注册失败 |
| F2 | game-demo 的开关监听者没有 `Rollback` | 业务再加一个 `AfterApply` 会失败的监听者时，configdata 撤回到旧一代，featureflag 仍是新一代的开关 |
| F3 | 自我续发的 Nest 异步消息链一直读最初那一代快照（由代码推出，未实测） | handler 每次处理都给自己（或在实体之间来回）发异步消息，形成循环：reload 对这条链永远不可见 |
| F4 | 运维 Rollback 发布后又被撤回时不计任何指标 | Rollback 时某个 `AfterApply` 失败：只有一条 Warn 日志 |
| F5 | tablegen `key=` 写错静默回落到第一个字段 | `//roost:table key=Idd` |
| F6 | tablegen 单例 CSV 多于一行数据时多余行静默丢弃 | `//roost:object` 的 CSV 写了两行 |
| F7 | `_` 结尾前缀 + `,closed` 时 closed 不生效；doctor 对 `_` 结尾的 YAML 段拼键与 viper 不同 | 运维把 `saga.result_ack_wait` 写成嵌套的 `saga: {result_: {ack_wait: …}}`：doctor 绿，进程静默忽略 |
| F8 | 浮点键的 `NaN` 绕过 min / max；YAML 里 ≥ 2^63 的整数读成负数 | 业务声明的浮点键 / 无 `min` 的 int64 键；框架键当前不受影响 |
| F9 | 同一错误被每个嵌入 `ServiceIdentity` 的 Mod 重复报出，`uniqueErrors` 去不掉 | `sid: abc` 报八九行 |
| F10 | 几处注释 / 文档过时（用例名、环境变量、`NewConfigReader`、manifest `generated_at`、fctx 异步注释） | 只影响阅读 |

### 7.3 需要外部验证的项

- 多进程部署下逐个进程执行 `gm.config.reload` 的顺序与部分失败处理（运维流程，不在框架内）。
- Go plugin 在真实构建环境下的可用性（需要与主程序同一工具链、同一依赖版本构建，`plugin.Open` 的限制；未在本次核对中实测）。

→ [实现文档](../impl/07-config.md)对应：§4 不变量清单、§10 review 检查点、§11 疑点。

## 8. 相关文档

- 方案与决定：[A4 ① 每个 Mod 声明配置](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)、[A4 ② 严格读取](../../feature/REFACTOR-2026-10-05-strict-config-reads.md)、[B10 / C2 规则与热更可见性](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)、[configdata 键大小写敏感](../../feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md)、[第十二轮（cfggen globals 规则）](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)、[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md)。
- 修复记录：[RR-20261006-38](../../bugfix/RR-20261006-38.md)（生成配置有键无声明）、[RR-20261006-40](../../bugfix/RR-20261006-40.md)（业务键由服务声明、生成工程守卫）。
- 使用参考：[USER_GUIDE](../../USER_GUIDE.md)（§10“配置数据：规则、热更与可见性”）、[codegen README](../../../codegen/README.md)（“配置管线：该用哪条”）、[CODEGEN_REFERENCE](../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md)、[CFGGEN_META](../../../codegen/docs/CFGGEN_META.zh-CN.md)、[TROUBLESHOOTING](../../TROUBLESHOOTING.md)、[静态注册](../../STATIC_REGISTRATION.md)。
- 其他分区：[01 app](01-app-lifecycle.md)、[02 nest](02-nest-entity.md)、[03 dataengine](03-dataengine.md)、[06 saga](06-saga.md)、[09 kit 服务](09-kit-services.md)、[11 可观测](11-observability.md)、[12 代码生成](12-codegen.md)、[00 总览](00-overview.md)。
- 实现：[impl/07-config.md](../impl/07-config.md)。
