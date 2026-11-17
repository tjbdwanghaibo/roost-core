# 配置表导出给客户端：导出组证据（2026-11-17）

方案：[FEATURE-2026-11-17-client-config-export.md](../../FEATURE-2026-11-17-client-config-export.md)（决定 3、4 修订版、7，第 4 节导出约定，第 6 节验收，第 8 节第 2 组）。前置：tablegen 组 `bcc38217`（[证据](../client-config-export-tablegen-20261117/README.md)）。现行说明：[codegen 参考 §2.2](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#22-配置表导出给客户端client_exportconfig)、`roost help client`。

机器：本机 macOS，Go 1.27.3，`GOWORK=off`，.NET SDK 10.0.302（netstandard2.1 的引用包取自本机 NuGet 缓存，没有下载）。这是功能实施，不是缺陷修复：没有“修前红”，每个新行为有回归用例，另做 17 个变异证明用例能抓到。codebase-memory-mcp 本会话连不上，代码阅读按当前源码与 rg。

## 设计

- **配置**：`client_export.config`（`ClientConfigExportSpec`）：`dir` 缺省 `shared/config`，`tables` 可选，`csharp.namespace` 缺省 `Roost.Config`。读 `roost.yaml` 时核对：目录相对、在工程内、不在 `configs/ game/ protocol/ db/ internal/ service/ cmd/ deploy/` 下、不用生成时跳过的名字（与协议目录共用 `validateExportDir`），不与协议导出目录相同或互相包含（两边都写 `manifest.json`）；`tables` 不空、不重复；命名空间合法；要 `config` feature。`tables` 里的表是否存在、是否带客户端字段要读 schema，生成与 doctor 时核对（`selectConfigTables`），被导出表 `ref` 到的表也必须导出。
- **只开 config**（最简单的写法）：`client_export` 下只写 `config`（不写 `dir`、`languages`、`csharp`）就只导出配置。`ClientExportSpec.protocol()` 判断是否导出协议：没有 `config`、或写了这三项之一时导出（所以已有的 `client_export: {}` 行为不变）。协议的生成步骤、doctor 项、`owns`、`client sync --to` 都按它走；配置导出不碰 protoc。
- **生成**：`generatorsFor` 在声明了 `config` 时最后追加 `client-config-export`（Always），只读 `configs/schema` 与 `configs/table`：`tablegen.ClientExport` → 按 `tables` 选表 → `tablegen.DataVersion`（选表后重算）→ 渲染 C# → 写 `data/<表>.json`（直接用模型的 Data）、`csharp/*.cs`、`manifest.json`。`data/*.json`、`csharp/*.cs`（只认这一层）与 `manifest.json` 由 `ClientConfigExportSpec.owns` 认领，所以 `--check` 快照、暂存提交、退役删除、业务输入快照都覆盖它们；本轮不产出的删掉，别的文件不动。打印 `client config export: N tables, omitted N tables / M fields` 与 `client config data version: <12 位> (<完整值>)`。
- **manifest.json**：`kind: roost-config-export`、`schema: 1`、`core`、`generator`、`data_version` / `data_version_short`、`csharp_namespace`、`omitted`、`tables`（名字、类型、种类、主键 JSON 名、文件、行数、`schema_hash`、`data_hash`、字段名 / 类型 / parser）、`files`（每个导出文件按 LF 的 sha256，与协议导出同一个 `contentHash`）。
- **C#**：每张表一个文件 `ConfigTables.<类型>.cs`（行类型 + `<类型>Table`，或单例类型 + internal `<类型>Reader`），加上入口 `ConfigTables.cs`（`Load`、`DataVersion` / `ShortDataVersion`、`DataFiles()`、`ConfigLoadException`）与运行部分 `ConfigJson.cs`（`client_config_csharp_runtime.go`：JSON 解析器、强类型读取、规则检查、哈希核对，全部 internal、无静态可变状态）。按表拆文件满足“单个文件太大就拆”，且改一张表只动它自己的文件。生成代码里引用生成的类型一律 `global::<ns>.<类型>`，避免表名、字段名同名的成员遮住类型；类型名与固定类型、`ConfigTables` 成员冲突、字段名不是 C# 标识符或等于类型名时生成报错；C# 关键字字段写 `@名字`。类型对应见参考 §2.2；不认识的类型（具名类型、结构体、`time.Duration`、`[]byte`、非 string 键的 map）报错并提示 parser 或 `side:"s"`。
- **加载**：先对全部数据文件核对哈希（CRLF → LF 后 sha256，与 `tablegen.DataHash` 同一算法），再逐表解析：在原始 JSON 上按 `framework/configdata/rules` 的语义查 required / unique / min / enum，然后按字段强类型读取（缺省 / null 为零值，类型不对与整数越界报位置），全部表读完后查 ref（零值表示没有引用，required 时零值也查）。异常 `ConfigLoadException` 带 Table / Row / Key / Field / Rule / Detail，消息格式与服务端 `rules.Error` 相同。
- **doctor `config-export:fresh`**：不渲染、不写文件：核对导出文件哈希；用 `tablegen.ClientExport` 算出现在的投影（同一套选表），比表集合、每张表 schema / 数据哈希、数据版本、命名空间；打印数据版本。只改只给服务端的列不算过期。C# 模板与生成器版本是否最新归 `--check`。
- **`roost client sync`**：新增 `--config-code-to` / `--config-data-to`（必须一起给，可以与 `--to` 一起用；`--to` 现在可省）。协议与配置共用 `planSync` / `syncPlan.run`：全部检查（导出哈希、同名非本工具文件）在写任何文件之前做完，两部分都先计划再执行。配置部分：`csharp/` → 代码目录、`data/` → 数据目录，服务端清单另存为代码目录的 `config-manifest.json`（同步记录，含数据版本）；报告 `config data version: 旧 -> 新`（或 `(unchanged)`、首次 `none -> 新`）、表增删、字段增删与改类型（parser 算在类型里）、行数变化。协议部分输出与行为不变（`TestClientSync` 原样通过）。
- **tablegen 两处收尾（第 0 项）**：横排表生成的 `Convert<Type>CSV` 加 `reader.Comment = '#'`；`DataVersion` 改为表名算进去（每张表 `name + "\x00" + hash + "\n"`，再 sha256），并导出 `tablegen.DataVersion(tables)` 供选表后的导出复用（同一个算法，不另写一份）。

## 文件

- 代码（新）：`codegen/internal/roost/client_config_export.go`、`client_config_csharp.go`、`client_config_csharp_runtime.go`。
- 代码（改）：`codegen/internal/roost/client_export.go`（`Config` 字段、`protocol()`、`validateExportDir`、`owns`）、`client_sync.go`（共用计划、配置同步与报告）、`generate.go`、`doctor.go`、`manifest.go`（config feature 校验一行）、`help.go`；`codegen/internal/tablegen/main.go`（一行）、`export.go`（`DataVersion`）。
- 用例：`codegen/internal/roost/client_config_export_test.go`（新）；`codegen/internal/tablegen/export_test.go`（横排 `#` 注释、表名进版本号）。
- 文档：`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`（§2 顺序、新增 §2.2、§9.2 注释行与 DataVersion）、`codegen/docs/PROJECT_GENERATOR.zh-CN.md`（字段表、doctor 清单）。

## 没声明 config 的工程，生成结果不变（第 0 项那一行除外）

[same-tree.sh](same-tree.sh)：旧生成器（`git archive HEAD` = `dd78962f` 的干净树）与新生成器各 `project new` 三种工程后 `diff -r`。结果 [same-tree.txt](same-tree.txt)：

| 工程 | 文件数 | 有差异的文件 | 差异 |
| --- | --- | --- | --- |
| plain | 75 | 0 | — |
| game | 87 | 0 | — |
| game-demo | 1402 | 1（`configs/generated/gen_table_config.go`） | 只有 6 行新增的 `reader.Comment = '#'`（6 张横排表各一行），别的差异 0 行 |

原来“逐字节相同”的对比改为“除这一行外相同”（协调者决定，线上尚未部署）。

## 单测

```
GOWORK=off go vet ./codegen/internal/roost/ ./codegen/internal/tablegen/        # 无输出
GOWORK=off go test -count=1 ./codegen/internal/tablegen/ ./codegen/internal/marker/   # ok / ok
ROOST_TEST_DOTNET=1 GOWORK=off go test -count=1 -v -run 'TestClientConfig|TestGenerateClientConfigExport|TestUndeclaredClientConfig|TestConfigCSharpTypes' ./codegen/internal/roost/
```

    --- PASS: TestClientConfigCSharpRuntime (3.32s)
    --- PASS: TestClientConfigExportSpecValidation (0.00s)
    --- PASS: TestConfigCSharpTypes (0.00s)
    --- PASS: TestUndeclaredClientConfigExportChangesNothing (0.00s)
    --- PASS: TestClientConfigSync (1.64s)
    --- PASS: TestGenerateClientConfigExport (3.51s)
    --- PASS: TestClientConfigExportTables (3.52s)
    --- PASS: TestClientConfigExportTransactionalRetirement (3.74s)

改动波及的 roost 用例（`-run 'Generate|Staged|Snapshot|Doctor|Manifest|Retire|ClientExport|ClientSync|ClientConfig|ConfigCSharp|Help|Undeclared|Table|Orchestrator'`，含协议导出的 8 个用例）：166 个通过、5 个 SKIP（原有的环境门控用例 4 个，加上这一轮没开 `ROOST_TEST_DOTNET` 的 `TestClientConfigCSharpRuntime`），`ok … 691.920s`，见 [roost-targeted-tests.txt](roost-targeted-tests.txt)。另跑 `GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：ok。没跑 codegen 全量与 pretag（按本批规则）。

新增用例覆盖：

- `TestClientConfigExportSpecValidation`：目录在 `configs/`、`game/`、绝对路径、工程外、跳过的名字，与协议目录三种重叠，`tables` 重复 / 空名，命名空间；`protocol()` 五种写法；`owns` 11 条路径；缺 config feature。
- `TestUndeclaredClientConfigExportChangesNothing`：没声明 config 时 `roost.yaml` 不多键、生成器列表没有配置导出；只声明 config 时没有协议导出（不需要 protoc）。
- `TestGenerateClientConfigExport`（夹具：scene 带 unique / enum；monster 带只给服务端的 `drop_seed`、只给客户端的 `model`、`scene_id` 上的 index、`[]int64`、`map[string]int32`、parser 字段；balance 竖排键值，含只给服务端 / 只给客户端的 key；tip `side=c`，含带换行的值）：导出文件清单；只给服务端的字段名 / 标题 / note 不出现在导出目录任何文件里、它们的值不出现在数据里；manifest 与 tablegen 模型逐表一致、文件哈希正确；C# 内含数据版本与预期声明、不出现 UnityEngine / JsonUtility / 反射；打印 omitted 与版本号；`--force` 再生成逐字节相同；`--check` 与 doctor 干净；改 CSV 后 `--check` 点名 `data/monster.json` 与 `manifest.json`、doctor 点名表与数据版本；只改服务端列 doctor 仍 OK；改 schema 报 schema 变化；手改导出文件报出文件。
- `TestClientConfigExportTables`：选表、未知表、ref 目标没选、`side=s` 的表。
- `TestClientConfigExportTransactionalRetirement`：暂存生成随批次提交、自定义目录与命名空间、去掉一张表后它的数据与 C# 被删、目录里别的文件不动、`--check` 干净。
- `TestClientConfigSync`：首次（10 个文件、`none -> 版本`、同步记录、无 `.meta`）；dry-run 不写；增量（版本旧 → 新、删 tip 两个文件、表删除、字段增 / 删 / 改类型、行数 3 → 4、没变的表不报）；再同步 `(unchanged)`；手写的同名文件拒绝覆盖且一个文件都不写；服务端导出被手改拒绝同步；两个配置参数必须一起给；只导出配置的工程不能用 `--to`。
- `TestConfigCSharpTypes`：类型对应 21 种、parser、嵌套读法、5 种不支持的类型、C# 字符串转义、6 种命名冲突、关键字字段。
- `TestClientConfigCSharpRuntime`（`ROOST_TEST_DOTNET=1` 才跑）：netstandard2.1 + LangVersion 8 + Nullable + 警告即错误 + XML 文档编译；`Load` 读回全部表全部行全部字段（与导出 JSON 逐字段比）；Get / TryGet / 不存在的键 / 非唯一 index；两次 `Load` 互不相干；CRLF 照读；四个 JSON 分别改动都报 `table <表名>: hash:`；readText 返回 null；可空 / 嵌套数组 / 字典里的数组 / float32 / uint64 最大值 / bool / 关键字字段的读回；required、ref、unique、min、enum、kv 的 required、数组元素类型错、int32 越界各报出表、行、主键、字段。

## 验收（第 6 节）

[acceptance.sh](acceptance.sh) `<roost> <本目录> <工作目录>`，结果 [acceptance.txt](acceptance.txt)：game-demo 工程（`-mods configdata,mongo,nats,dataengine,nest -template game-demo`）加四张夹具表，`client_export.config.csharp.namespace: Demo.Config`，导出全部带客户端字段的表（夹具 4 张 + demo 原有 6 张）。

| 项 | 结果 |
| --- | --- |
| `roost generate`（PATH 上只有 go 与系统目录，没有 protoc） | rc=0，`client config export: 10 tables, omitted 0 tables / 2 fields`，打印数据版本；23 个导出文件；`go build ./... && go vet ./configs/...` 通过；`--check` 干净；doctor `OK config-export:fresh … data version 41f9ec5c098a` |
| 只给服务端的字段不出现在导出目录 | `drop_seed`、`server_tick`、标题、note：0 个文件；`777001` 等值在数据里 0 个文件；服务端 `configs/data` 里没有只给客户端的值，没有 `tip.json` |
| 两次生成逐字节相同 | `generate --force` 后 `diff -r` 无差异 |
| 改 CSV 不重新生成 | `--check` rc=1，列出 `shared/config/csharp/ConfigTables.cs, data/monster.json, manifest.json`；doctor `FAIL config-export:fresh … table monster: data (configs/table/monster.csv) changed …; data version 41f9ec5c098a in manifest.json, ed94374cead5 from configs/table`；只改只给服务端的列 doctor 仍 OK |
| C# netstandard2.1 编译 | 0 警告 0 错误（LangVersion 8、Nullable、TreatWarningsAsErrors、GenerateDocumentationFile） |
| `Load` 读回、两个实例、改 JSON | 10 张表读回（monster 3 行、BySceneID(1)=10,12、item 5 行、building 4 行、balance、tip）；`independent True`；改 monster.json 报 `table monster: hash: …`；`ConfigTables.DataVersion` 与 manifest 的 `data_version` 相同 |
| `client sync` 首次 | 22 个文件（12 个 .cs 进代码目录、10 个 .json 进数据目录）+ `config-manifest.json`；`config data version: none -> 41f9ec5c098a`；0 个 `.meta` |
| `client sync` 增量 | 3 updated、2 removed、17 unchanged；`41f9ec5c098a -> cef6554809bb`；tip 表删除、`monster.icon` 增、`monster.rewards` 删、`monster.level int32 -> int64`、`monster: 3 -> 4`；同步记录里的 `data_version` 是新值 |
| `client sync` 手写的同名文件 | rc=1 `refuse to overwrite …/data/monster.json: it was not written by a previous roost client sync`；手写文件不变，代码目录没有被创建 |

跑完执行了 `dotnet build-server shutdown`（单测里也在退出时执行；构建都带 `--disable-build-servers`）。

## 变异

[mutations.sh](mutations.sh)（用 [mutate.py](mutate.py)：改一处、跑对应用例、还原；C# 相关的带 `ROOST_TEST_DOTNET=1`），结果 [mutations.txt](mutations.txt)，17 个全部变红，红都落在承诺本身（M3b 第一次因 Go 编译错误不算，改写变异后重跑变红，见 mutations.txt）：

| 变异 | 抓到它的用例 |
| --- | --- |
| M1 服务端字段进入导出（tablegen 投影不排除另一端字段） | `TestGenerateClientConfigExport`（C# 与数据里出现 `drop_seed`、`777`） |
| M2 `Load` 不核对哈希 | `TestClientConfigCSharpRuntime`（四个 TAMPER 没报） |
| M3a `Load` 不查 required / unique / min / enum | `TestClientConfigCSharpRuntime` |
| M3b `Load` 不查 ref | `TestClientConfigCSharpRuntime` |
| M4 sync 覆盖不是上次写的同名文件 | `TestClientConfigSync`、`TestClientSync` |
| M5a `--check` 不认领配置导出目录 | `TestGenerateClientConfigExport` |
| M5b doctor 不比数据哈希 | `TestGenerateClientConfigExport` |
| M5c doctor 不比 schema 哈希 | `TestGenerateClientConfigExport` |
| M6 DataVersion 不含表名 | `TestClientExportDataVersionCoversTableNames` |
| M7 横排 `Convert<Type>CSV` 不跳过 `#` | `TestClientExportFixtureEndToEnd` |
| M8a 没声明 config 也跑配置导出 | `TestUndeclaredClientConfigExportChangesNothing` |
| M8b 只写 config 也导出协议 | `TestUndeclaredClientConfigExportChangesNothing`、`TestClientConfigExportSpecValidation` |
| M9 sync 不删上次写过、这次没有的文件 | `TestClientConfigSync` |
| M10 sync 报告不比字段类型 | `TestClientConfigSync` |
| M11 生成不删退役的数据与 C# | `TestClientConfigExportTransactionalRetirement` |
| M12 `tables` 不过滤 | `TestClientConfigExportTables` |
| M13 配置目录可以放在 `configs/`、`game/` 下 | `TestClientConfigExportSpecValidation` |

## 拿不准的点

1. **“只开 config”的写法是隐式的**：`client_export` 下只有 `config` 时不导出协议。已有工程若写的是 `client_export: {}`（只导 proto 副本）再加 `config`，协议导出会停下（旧文件留在原处、不再被 doctor 核对）；要两者都导出需把协议的 `dir` 或 `languages` 写出来。另一种写法是显式的 `protocol: false`，多一个键。
2. **手改过的已同步文件**：按“规则与协议同步相同”，上次同步写过、之后在客户端被手改的文件照样覆盖并提示；拒绝的是目录里“不是上次同步写的同名文件”和服务端导出目录被手改。若第 6 节“手改过的文件拒绝覆盖”指前者，需要改成拒绝（协议同步也要一起改）。
3. **C# 单测要显式打开**：`TestClientConfigCSharpRuntime` 要 `ROOST_TEST_DOTNET=1`（编译 + 运行约 3 秒），没有它时 Skip 并写明原因；不默认打开是因为 pretag 机器不一定有 .NET SDK 与 netstandard 引用包缓存。
4. **按表拆文件**：方案写的是 `ConfigTables.cs`“必要时再拆”，实施为每张表一个文件（加入口与运行部分），不按大小阈值拆，避免表增删时整片换文件。
5. **类型范围**：只支持内建标量、指向标量的指针、切片与 `map[string]T`；具名类型（如 `type Kind int32`）、结构体、`time.Duration` 在客户端导出里报错，要改成 parser（客户端拿原文字符串）或 `side:"s"`。
6. **浮点的规范形式**：unique / enum 对浮点值比较时，C# 用 `"R"` 格式、Go 用 `'g'`，极大 / 极小的指数写法不同（如 `1E+21` 与 `1e+21`）；只影响浮点列上的 unique / enum 报错文字与极端值比较。
7. **横排 `Convert<Type>CSV` 报错的 `row=`**：加了注释行支持后，`row=` 是去掉注释行后的记录序号，不是文件行号（kv 的转换报文件行号）；只加一行的约束下没有改它，文档已写明。
8. **生成器版本写在 manifest 里**：与协议导出一样，换了生成器版本 `--check` 会报 `shared/config/manifest.json` 过期。

## 建议写进 CHANGELOG 的文字（本组不改 CHANGELOG）

- **配置表导出给客户端（`client_export.config`，配置表导出给客户端第 2 组，维护者 2026-11-17 决定 3、4、7）**：`roost.yaml` 的 `client_export.config`（`dir` 缺省 `shared/config`、`tables` 可选、`csharp.namespace` 缺省 `Roost.Config`；要 config feature，不要 protoc；`client_export` 下只写 `config` 时只导出配置）。`roost generate` 写出 `data/<表>.json`（只含客户端字段）、`csharp/`（netstandard2.1 纯 C#：每张表一个文件，`ConfigTables.Load(readText)` 返回实例，加载时核对每个文件的数据哈希与 required / unique / min / enum / ref，报出表、行、字段；自带 JSON 解析器，不用 JsonUtility、UnityEngine 与反射）与 `manifest.json`（`kind: roost-config-export`、数据版本、每张表的 schema / 数据哈希与行数、文件哈希），打印 omitted 表数 / 字段数与数据版本。`generate --check` 覆盖导出目录；doctor 新增 `config-export:fresh`；`roost client sync` 新增 `--config-code-to` / `--config-data-to`（同步记录 `config-manifest.json`，报告数据版本旧 → 新、表 / 字段 / 行数变化），`--to` 改为可选。数据版本只记录、可查看，登录时不核对；带 parser 的字段在客户端是 CSV 原文字符串。说明见 `codegen/docs/CODEGEN_REFERENCE.zh-CN.md` §2.2；证据 `docs/maintenance/evidence/client-config-export-20261117/`。
- **横排表的 `Convert<Type>CSV` 忽略 `#` 注释行；数据版本把表名算进去**（第 0 项，协调者 2026-11-17 定）：已有工程重新生成后 `configs/generated/gen_table_config.go` 每张横排表多一行 `reader.Comment = '#'`（升级需重新生成）；`tablegen.ClientExport` 的 `DataVersion` 改为每张表 `name + "\x00" + hash + "\n"` 再取 sha256，表改名时版本号也变，新增 `tablegen.DataVersion(tables)`。

## 建议写进 KNOWN-LIMITS 的文字（本组不改 KNOWN-LIMITS）

- **客户端配置导出只支持内建标量、指向标量的指针、切片与 `map[string]T`**（框架限制）：具名类型、结构体、`time.Duration`、`[]byte` 的字段导出时报错，改成 parser（客户端拿 CSV 原文）或 `side:"s"`。
- **带 parser 的字段在客户端只有 CSV 原文字符串**（方案第 7 节，已定）。
- **客户端数据版本没有运行时核对**（决定 4，已定）：`ConfigTables.DataVersion` 只供显示或上报。
- **横排 `Convert<Type>CSV` 报错的 `row=` 是记录序号，不是文件行号**（遗留，待维护者定）：有 `#` 注释行时两者不同。
