# A4：框架配置一律严格读取，启动校验覆盖全部类型化的键（2026-10-05）

依据：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) A4 行（维护者第二轮：按推荐先做 ②，① 配置 schema 作为后续重构）。
背景：[RR-20261005-NC-190](../bugfix/RR-20261005-NC-190.md)（新增 `app.ConfigBool` / `ConfigDuration`，只覆盖 singleton、启动校验已列出的键、saga 步骤预算与 `mods.Duration`）、[N14 观察 O1 与方向判断](../review/REVIEW-2026-10-05-n14.md#方向判断)、RR-20260926-12、N02 O2。C1（NC-192 方案 1）同批实施，见 [bugfix](../bugfix/RR-20261005-NC-192.md)。

## 目标

1. app 与 kit 里的布尔、时长、整数配置不再经 viper 的宽松 getter 读取：`on` / `yes` 不再读成 false，不带单位的时长不再读成纳秒，`8k` / `1.5` / `10s` 这样的整数不再读成 0（取默认）或被截断。
2. `ValidateServiceConfig` 在任何 Mod Init 之前按同一规则检查框架读取的全部类型化的键，错误一次报全、点名键。
3. 守卫测试让以后新增的宽松读取、或没有登记的新键在 CI 里变红。
4. 生成工程的三份配置（开发、生产示例、k8s Secret 示例）在更严格的校验下全部通过。

## 改动面

**app（`app/config_values.go`、`app/config_validation.go`）**

- 新增 `ConfigInt` / `ConfigInt64`：接受 YAML 整数、没有小数部分的浮点数（YAML 的 `1e3`）与十进制字符串（环境变量覆盖），拒绝小数、带后缀、时长、布尔值。
- 新增 `ConfigReader`（`NewConfigReader(cfg)`，`Bool` / `Duration` / `Int` / `Int64`，`Err()` 汇总）：Mod 的 Init 一次读几十个键，逐个返回错误会把读取淹没在分支里；读完统一检查，运维一次看到全部写错的键。读取失败的键返回零值，调用方照常“≤0 取默认”，只在语义检查和返回前看 `Err()`。
- `frameworkBoolKeys` 扩为三份登记 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`（16 / 95 / 77 个键），另有 syncbus 三段（`syncbus` / `room` / `sync`）的同名字段与按后缀登记的 `<service>.call_timeout`。`checkFrameworkConfigTypes` 逐个严格读取；与语义检查重复的同一条错误只报一次。
- `app.go` 的日志 / 时钟 / 停机预算、`registry.go` 的 metrics 上限改用严格读取。

**kit**：`dataengine`、`remoteentity`、`saga`（Mod 与步骤预算的 `max_attempts`）、`nats`（JetStream RPC、reliable、worker 数，类型化的键在建 bus 之前读完）、`nest`（Mod 与 `sync.entity.*`）、`syncbus`、`mongo`、`ops`、`etcd`、`statslog`、`mods.ResolvePersistenceEngine`、`service/platform`、`service/global/activity` 全部改用 `app.ConfigReader` / `app.Config*`。错误前缀沿用各 Mod 原有的（`dataengine mod: …`）。

**范围例外**

- `kit/redis/redis_mod.go` 的 `redis.db` / `redis.pool_size` / `redis.min_idle_conns` 三处 `GetInt`：kit/redis 当时由 A2 的 agent 修改，本批不动；三个键已在 `frameworkIntKeys` 里，启动时由 `ValidateServiceConfig` 严格检查。守卫测试按“文件 + 键”逐个放行这三处，新增读取照样变红。**留给 A2 之后**改成 `app.ConfigReader`。（2026-10-06 已改，见文末“A4 留项：kit/redis”。）
- 生成进工程的代码（player TCP 接入层 `codegen/internal/roost/render_player_tcp.go`、RPC 客户端 Mod 模板 `codegen/internal/servicerpc/template.go` 及其在 `kit/service/*_rpc_assembly_gen.go` 的产物）仍用 viper 的 getter：生成工程按生成器的下限（`manifest.go` 的 `Core`，现为 v1.20.1）解析 roost-core，`app.ConfigReader` 只在下一版里有，改成严格读取会让按默认下限生成的工程编译失败、兼容矩阵 minimum 变红。它们读的键都已登记（`player_access.tcp.*` 字面登记，`<service>.call_timeout` 按后缀），新版 roost-core 上由启动校验兜住；下限升到 v1.20.2 之后可以再改成严格读取。
- `sid` 的读取点遍布各 Mod，保留 `GetInt32("sid")`：启动校验先严格检查它是 int32 范围内的正整数。
- `GetString` / `GetStringSlice` 不在本批范围（字符串没有类型问题；`redis.cluster_addrs` 的列表写法已由 NC-190 处理）。

**守卫（`app/config_strict_reads_promises_test.go`、`app/config_types_promises_test.go`）**

- `TestEveryFrameworkBoolSwitchIsCheckedStrictly`（N14 加的，保留）与新的 `TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly`：扫描 app、kit 与上述两个生成模板，按读取形式（`GetX`、`ConfigX`、`read.X`、`mods.Duration`、syncbus 的 `cfgX(cfg, read, …)`、player TCP 的 `key + "…"`）找出键，没登记就点名失败。`ConfigReader` 的变量按约定叫 `read`。
- `TestFrameworkCodeDoesNotReadConfigLeniently`：app、kit 的非测试源码出现 `GetBool` / `GetDuration` / `GetInt*` / `GetUint*` / `GetFloat*` 即失败，例外只有上面三类（`sid`、生成文件的 `*.call_timeout`、kit/redis 三处）。

## 兼容

行为收紧：以前被静默读成 false / 纳秒 / 0（取默认）/ 截断的写法，现在 App 启动时（以及直接调用 Mod Init 时）报错并点名键。`true` / `false` / `1` / `0` / `"true"`、带单位的时长、`0`、整数与十进制字符串照常接受。生成的配置全部合规（见验证）。删掉了 `remote_entity.sync_retry_queue_cap` 的非负检查：没有任何代码读取这个键。

## 验证

见 [bugfix 记录](../bugfix/RR-20261005-NC-192.md#a4-严格读取的验证) 与提交说明。修前红：`ValidateServiceConfig` 对 14 种写错类型的值返回 nil；kit 的 6 个 Mod Init 返回 nil（[证据](../bugfix/evidence/a4-config-20261005/README.md)）。

## 后续

- 维护者决定 A4 ①：每个 Mod 声明自己的配置键与类型（键名、类型、默认值、是否必填、是否生产必需），校验 / 生成器 / doctor 共用一份声明。本批的三份登记加守卫是过渡形态：登记集中在 app，靠源码扫描与读取点保持同步。
- ~~A2 之后：kit/redis 三处整数读取改为 `app.ConfigReader`，删掉守卫里的放行。~~ 已完成（2026-10-06，见文末）。
- 生成器下限升到包含 `app.ConfigReader` 的版本之后：生成的 player TCP 接入层与 RPC 客户端 Mod 改用严格读取。

## A4 留项：kit/redis（2026-10-06）

`kit/redis/redis_mod.go` 的 `redisConfig`（Redis Mod 与单实例锁的 `SingletonStore` 共用）改用 `app.ConfigReader` 读 `redis.db` / `redis.pool_size` / `redis.min_idle_conns`，
返回 `(*fredis.Config, error)`；`8k`、`1.5`、`10s` 这样的值在 `RedisMod.Init` / `SingletonStore` 点名报错（`redis mod: config: redis.db …`）。
未设置或 ≤ 0 的 pool_size / min_idle_conns 仍取驱动默认值，合法配置行为不变。守卫测试 `TestFrameworkCodeDoesNotReadConfigLeniently` 删掉“文件 + 键”的三项放行。

- 先红：`TestRedisIntegerKeysAreReadStrictly`（`kit/redis/config_types_promises_test.go`）修前
  `config_types_promises_test.go:45: redis.db: 8k: Init returned <nil>, want a refusal naming redis.db`。
- 绿：同一用例（三键 × 三种坏值，Mod 与 SingletonStore 都拒绝；合法的 2 / 16 / 3 读对）；`go test -race -count=3 ./kit/redis ./app`；根包。
- 兼容：经 App 启动的进程此前已由 `ValidateServiceConfig` 拒绝这些值，行为不变；只有绕过 App 直接装配 Mod 的调用方（测试、工具）从“读成 0”变成报错。

