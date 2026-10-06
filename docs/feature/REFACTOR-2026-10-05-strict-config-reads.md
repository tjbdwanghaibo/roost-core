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
- `frameworkBoolKeys` 扩为三份登记 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`（17 / 99 / 79 个键，按 v1.23.0 代码冻结提交 `e6828e4f` 核对；本方案实施当时是 16 / 95 / 77，之后各批登记了 `service_metrics.enabled`、`ops.admin_timeout`、`remote_entity.cached_max_staleness`、`remote_entity.mirror.shutdown_timeout`、`remote_entity.snapshot_l2_tombstone_wait_timeout`、`remote_entity.snapshot_interest_per_consumer`、`remote_entity.snapshot_l2_tombstone_wait_replicas` 七个键），另有 syncbus 三段（`syncbus` / `room` / `sync`）的同名字段与按后缀登记的 `<service>.call_timeout`。`checkFrameworkConfigTypes` 逐个严格读取；与语义检查重复的同一条错误只报一次。
- `app.go` 的日志 / 时钟 / 停机预算、`registry.go` 的 metrics 上限改用严格读取。

**kit**：`dataengine`、`remoteentity`、`saga`（Mod 与步骤预算的 `max_attempts`）、`nats`（JetStream RPC、reliable、worker 数，类型化的键在建 bus 之前读完）、`nest`（Mod 与 `sync.entity.*`）、`syncbus`、`mongo`、`ops`、`etcd`、`statslog`、`mods.ResolvePersistenceEngine`、`service/platform`、`service/global/activity` 全部改用 `app.ConfigReader` / `app.Config*`。错误前缀沿用各 Mod 原有的（`dataengine mod: …`）。

**范围例外**

- `kit/redis/redis_mod.go` 的 `redis.db` / `redis.pool_size` / `redis.min_idle_conns` 三处 `GetInt`：kit/redis 当时由 A2 的 agent 修改，本批不动；三个键已在 `frameworkIntKeys` 里，启动时由 `ValidateServiceConfig` 严格检查。守卫测试按“文件 + 键”逐个放行这三处，新增读取照样变红。**留给 A2 之后**改成 `app.ConfigReader`。（2026-10-06 已改，见文末“A4 留项：kit/redis”。）
- 生成进工程的代码（player TCP 接入层 `codegen/internal/roost/render_player_tcp.go`、RPC 客户端 Mod 模板 `codegen/internal/servicerpc/template.go` 及其在 `kit/service/*_rpc_assembly_gen.go` 的产物）本批仍用 viper 的 getter：生成工程按生成器的下限（`manifest.go` 的 `Core`，当时为 v1.20.1）解析 roost-core，`app.ConfigReader` 只在下一版里有。它们读的键都已登记（`player_access.tcp.*` 字面登记，`<service>.call_timeout` 按后缀），新版 roost-core 上由启动校验兜住。~~下限升到 v1.20.2 之后再改成严格读取。~~ 已完成（2026-10-06，见文末“A4 留项：生成进工程的代码”）。
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

- 维护者决定 A4 ①：每个 Mod 声明自己的配置键与类型（键名、类型、默认值、是否必填、是否生产必需），校验 / 生成器 / doctor 共用一份声明。本批的三份登记加守卫是过渡形态：登记集中在 app，靠源码扫描与读取点保持同步。（2026-10-07 已实施：[A4 ①](A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)，三份登记、正则守卫与 `app.ConfigReader` 删除。）
- ~~A2 之后：kit/redis 三处整数读取改为 `app.ConfigReader`，删掉守卫里的放行。~~ 已完成（2026-10-06，见文末）。
- ~~生成器下限升到包含 `app.ConfigReader` 的版本之后：生成的 player TCP 接入层与 RPC 客户端 Mod 改用严格读取。~~ 已完成（2026-10-06，随 v1.20.2 下限，见文末）。

## A4 留项：kit/redis（2026-10-06）

`kit/redis/redis_mod.go` 的 `redisConfig`（Redis Mod 与单实例锁的 `SingletonStore` 共用）改用 `app.ConfigReader` 读 `redis.db` / `redis.pool_size` / `redis.min_idle_conns`，
返回 `(*fredis.Config, error)`；`8k`、`1.5`、`10s` 这样的值在 `RedisMod.Init` / `SingletonStore` 点名报错（`redis mod: config: redis.db …`）。
未设置或 ≤ 0 的 pool_size / min_idle_conns 仍取驱动默认值，合法配置行为不变。守卫测试 `TestFrameworkCodeDoesNotReadConfigLeniently` 删掉“文件 + 键”的三项放行。

- 先红：`TestRedisIntegerKeysAreReadStrictly`（`kit/redis/config_types_promises_test.go`）修前
  `config_types_promises_test.go:45: redis.db: 8k: Init returned <nil>, want a refusal naming redis.db`。
- 绿：同一用例（三键 × 三种坏值，Mod 与 SingletonStore 都拒绝；合法的 2 / 16 / 3 读对）；`go test -race -count=3 ./kit/redis ./app`；根包。
- 兼容：经 App 启动的进程此前已由 `ValidateServiceConfig` 拒绝这些值，行为不变；只有绕过 App 直接装配 Mod 的调用方（测试、工具）从“读成 0”变成报错。

## A4 留项：生成进工程的代码（2026-10-06）

生成器 Core 下限随 v1.20.2 升到包含 `app.ConfigReader` 的版本，两份模板改为严格读取：

- player TCP 接入层（`render_player_tcp.go` 的 `configFromViper`）：`read := app.NewConfigReader(cfg)` 读 `player_access.tcp.*` 的布尔、整数、时长与 `nest.request_timeout`，全部读完后 `read.Err()` 一次报出所有写错的键（`player tcp: config: player_access.tcp.enabled must be true or false, got "on"` …）。`max_handshake_bytes` / `max_payload_bytes` 先按 int 读，负数或超过 16 MiB 点名拒绝，再转 uint32（以前 `GetUint32(-1)` 读成 0、取默认）。`addr` 仍是 `GetString`。合法配置行为不变：未设置取默认，dispatch / login 预算的回退规则不变。
- RPC 客户端 Mod 模板：`app.ConfigDuration(cfg, "<service>.call_timeout")`，错误包成 `<pkg> client mod: config: <service>.call_timeout …`；负数仍按原文案拒绝。kit/service 的九份 `*_rpc_assembly_gen.go` 经 `go generate ./...` 重生成，servicerpc golden 刷新。
- 守卫：`TestFrameworkCodeDoesNotReadConfigLeniently` 把两份模板纳入扫描，删掉“生成文件的 `*.call_timeout`”放行，例外只剩 `sid`；`playerTCPReadPattern` 改认 `read.X(key + "…")`。生成工程测试新增 `TestConfigValuesOfTheWrongTypeAreRefusedByName`（`server_gen_test.go`）。

验证（worktree `rel122`，基线 `cb3e2549`）：

- 先红（修前生成器 + 本仓 core）：game-demo 里同一用例，`enabled: on`、`max_payload_bytes: 8k`、`max_handshake_bytes: -1`、`idle_timeout: 90`、`handshake_timeout: soon`、`nest.request_timeout: 7` 六项 `Init returned <nil>`（解析结果分别是 `Enabled:false`、默认 1048576、默认 8192、`IdleTimeout:90ns`、默认 5s、`DispatchTimeout:7ns LoginTimeout:7ns`），`max_connections: 1.5` 只报不点名的 `addr, limits and timeouts are outside safe bounds`；kit/service/mail 的 `mail.call_timeout: 5` / `soon` 两项 `error = <nil>`。
- 绿：同一用例 7 项全部点名拒绝，合法的字符串写法（`enabled: "true"`、`max_payload_bytes: "65536"`）照常读取；mail 两项点名拒绝、`2s` 读对。生成的 dev / prod example / k8s Secret 三份 game 配置经新 `Mod.Init` 与 `ValidateServiceConfig` 通过。新生成 game-demo（replace 到 worktree）`go build ./... && go vet ./... && go test ./...` 通过；`go test -count=1 ./codegen/...`、`./app ./kit/service/...` 通过；`go generate ./...` 之后 porcelain 干净。
- 兼容：经 App 启动的进程在 v1.20.2 core 上此前已由 `ValidateServiceConfig` 在启动第一步点名拒绝这些值（修前生成的 game-demo 用同一份坏配置启动，同样报 `config: player_access.tcp.enabled must be true or false, got "on"` 与 `max_payload_bytes must be a whole number, got "8k"`），行为不变；变化在绕过 App 直接调用 Mod Init 的路径（测试、工具），以及生成代码不再依赖登记表兜底。新生成的代码需要 core ≥ v1.20.2；已生成工程不迁移。
