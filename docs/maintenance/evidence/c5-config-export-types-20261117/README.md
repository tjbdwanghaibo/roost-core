# 交接 C5：配置导出的类型范围，三个小项（2026-11-17 实施）

方案：[open-work-c5-config-export-types-20261117](../open-work-c5-config-export-types-20261117/README.md)。维护者 2026-11-17 决定“按推荐”：(a) 选项 1、(b) 选项 1、(c) 按方案的唯一修法。三项都是先红后绿，关键一行各做了变异。

## (a) 具名类型按底层类型处理

做了什么：

- `codegen/internal/tablegen/named.go`（新）：读 schema 时收集同一目录（同一个 Go 包）里全部非测试 `.go` 文件的类型声明；底层是内建标量的具名类型（`type Rank int32`、具名类型的具名类型 `type Tier Rank`、别名 `type Power = int64`）记下底层类型。`Field.Resolved` 是把这些类型换成底层类型后的表达式（`[]Rank` → `[]int32`）。
- CSV 转 JSON（`parseCell`）、index 的可用性与键（`validateIndexes`、`indexKeyExpr`）、导出模型（`ExportField.Type` / `ExportIndex.Type`）都看 `Resolved`；C# 因此生成底层类型（`int`），不生成 enum。manifest 里的字段类型也是底层类型（schema 哈希随之覆盖底层类型的变化）。
- 服务端生成的 Go：schema 包里的类型名写成带导入别名的 `meta0.Rank`（主键类型、`<Type>By<Index>` 参数、`Convert<Type>CSV` 的解析）。以前生成代码里写的是没有包名的 `Kind`（例如 `tablegenParseJSON[Kind](raw)`），生成的 Go 无法编译——这一点是读生成文本得出的（742ccafc 上生成后看到 `tablegenParseJSON[Kind](raw)` 位于 package generated 里），没有单独编译验证。解析辅助函数的类型约束加宽（`~int64`、`~uint64`、`~float64`），多一个 `tablegenParseBool`。
- `ref` 的类型核对改成按“导入路径 + 类型名”比较（同一个包里每张表各有一个导入别名，按别名比较会误报）。
- 别的包里的具名类型、底层不是内建标量的具名类型：index 与 C# 导出时仍报错，报错用 `tablegen.NamedTypeReason` 说明原因：
  - `time.Duration is defined in another package; the generator reads only the schema package's own declarations, so it cannot see the underlying type (...)`
  - `Reward is a named type whose underlying type is not a built-in scalar (for example a struct); only named types over string, bool, integers and floats declared in the schema package are handled as their underlying type`
  - 底层是浮点的具名类型上写 index：`got Rate (float64)`。

先红（修前，原文摘录）：

```
--- FAIL: TestNamedScalarTypesFollowTheirUnderlyingType
    export_test.go:374: table hero field Rank: index needs a string, integer or bool field, got Rank
--- FAIL: TestIndexDeclarationsAreRejected/named_float
    err = table m field Rate: index needs a string, integer or bool field, got Rate, want it to contain "got Rate (float64)"
--- FAIL: TestIndexDeclarationsAreRejected/named_type_from_another_package
    err = table m field D: index needs a string, integer or bool field, got time.Duration, want it to contain "time.Duration is defined in another package"
--- FAIL: TestIndexDeclarationsAreRejected/named_struct
    err = table m field R: index needs a string, integer or bool field, got Reward, want it to contain "Reward is a named type whose underlying type is not a built-in scalar"
--- FAIL: TestGenerateClientConfigExport / TestClientConfigCSharpRuntime（夹具加了 Rank 字段与 index）
    generate: generator config-template: table monster field Rank: index needs a string, integer or bool field, got Rank
```

方案要求的“schema 里加一个 `type Kind int32` 的字段，现在导出报错”：把夹具里 Rank 的 index 暂时去掉再跑 `TestGenerateClientConfigExport`，修前报：

```
generate: generator client-config-export: client_export.config: table monster field Rank: type Rank is not supported in the client export (built-in scalars, pointers to them, slices and map[string]T are); give the field parser:"..." so the client gets the raw string, or side:"s"
```

测试（绿）：

- `codegen/internal/tablegen/export_test.go` `TestNamedScalarTypesFollowTheirUnderlyingType`：类型声明在同包另一个文件；具名主键 `HeroID`、具名 int / 具名的具名 / string / float / bool / 别名 / `[]HeroID`；生成服务端 JSON（`"element": "42"` 是字符串）与 Go，`go run` 后 `HeroByID`、`HeroByRank(2)` → `1,3`、`HeroByElement("42")` → `2,3`、`ConvertHeroCSV` 全对、`ref` 到具名主键成立；导出模型字段类型是 `ID:int32,Rank:int32,Tier:int32,Element:string,Rate:float64,Ready:bool,Power:int64,Friends:[]int32`。
- `codegen/internal/tablegen/index_test.go`：具名浮点、别的包、具名结构体三条拒绝，报错带原因。
- `codegen/internal/roost/client_config_export_test.go`：夹具 monster 加 `Rank Rank` + `index:"true"`；`TestGenerateClientConfigExport` 核对 manifest 字段 `{rank int32}`、C# `public int rank;` / `ByRank(int value)`；`TestConfigCSharpTypes` 核对两种原因文字。
- **两端一致**（`TestClientConfigCSharpRuntime` + `serverRankLookups`）：同一份 schema 与 CSV，服务端用 tablegen 生成 JSON 与 Go 绑定后 `go run`，C# 用导出代码，两边都打印 `RANK rank=1 -> 11; rank=2 -> 10,12; rank=3 -> `，逐字相同。

变异：`resolveType` 把具名标量换成 `"string"`（“按字符串处理”）：

```
--- FAIL: TestNamedScalarTypesFollowTheirUnderlyingType
    export_test.go:374: hero.csv: table hero row 1 (key 1) field rank: min: value 2 is not a number
--- FAIL: TestClientConfigCSharpRuntime
    client_config_export_test.go:859: dotnet build: exit status 1   （C# 的 ByRank 参数变成 string，两端一致用例编译不过）
```

## (b) 数字的规范写法：明确规则 + 两端共用样例表

实施前核对（维护者要求）：规范字符串**没有**进入数据版本号或任何哈希。

- `tablegen.DataHash` / `DataVersion` / 服务端 `_manifest.json` 的哈希按数据文件字节（`encoding/json` 输出）算；`SchemaHash` 按导出字段声明与 `rules.Rule`（tag 原文的 Enum / Min）算；configdata 的快照哈希按类型化表的 `json.Marshal` 算（`framework/configdata/configdata.go`、`external.go`、`changes.go`）。`rules.Canonical` 只在 `rules.Check`（unique / min / enum 报错）与 tablegen 的 `rowKeyOf`（报错点名主键）用；C# 的 `Canonical` 只在规则检查、`KeyOf`（报错点名）用，`CanonicalFloat` 经 `canonicalCS` 只用于主键 / ref，而主键与 ref 不允许浮点。
- 影响：C# 运行部分 `ConfigJson.cs` 的内容变了，已导出工程的 `shared/config` 里这个文件哈希变化，`roost generate --check` 报过期，重新生成并提交即可；`data_version` 不变。

做了什么：

- `framework/configdata/rules/rules.go`：`Canonical` 的注释写成明确规则（行为不变，Go 是基准）：int64 / uint64 范围的整数原文写十进制；否则按 float64 解析，超出范围（1e400）不是标量、取原文；整数值且 |v| < 2^53 写整数（-0 为 0）；其余同 `strconv.FormatFloat(f, 'g', -1, 64)`——最短往返位数，十进制指数在 [-4, 6) 内写定点小数，否则 `d.ddde±XX`。
- 共用样例表 `framework/configdata/rules/testdata/canonical_numbers.json`：57 条，覆盖整数（含 int64 / uint64 边界、2^53 前后）、小数、定点与指数的切换点（0.0001 / 0.00001、999999.5 / 1000000.5）、极大（1e21、1.797…e308、超出 int64 / uint64 的整数）、极小（5e-324、1e-400 下溢为 0）、负零、超出 float64 范围（1e400）。
- Go：`rules_test.go` `TestCanonicalNumbersMatchTheSharedTable` 逐条比较。
- C#：`ConfigJson.TryCanonicalNumber` / `CanonicalFloat` 改成同一条规则（用 `"R"` 只取最短往返的数字与指数，按 Go 的规则重排；超出 double 范围不是标量）。`TestClientConfigCSharpRuntime` 把运行部分直接编进控制台程序（`Canon.Config`），对同一份样例表逐条比较。

先红（修前 C#，与 Go 不同的 24 条，摘录）：

```
CANON 1e21 -> 1E+21            （Go：1e+21，方案点名的这一条）
CANON 0.00001 -> 1E-05         （Go：1e-05）
CANON 1234567.5 -> 1234567.5   （Go：1.2345675e+06）
CANON 12345678.9 -> 12345678.9 （Go：1.23456789e+07）
CANON 1e16 -> 10000000000000000（Go：1e+16）
CANON 9007199254740992.0 -> 9007199254740992（Go：9.007199254740992e+15）
CANON 1e400 -> Infinity        （Go：1e400）
```

所以只改大小写（方案选项 2）不够：定点 / 指数的切换点、超出范围的处理两端也不同。

变异：

- C# 的 `CanonicalFloat` 在整数分支之后直接 `return value.ToString("R", ...)`（改回 "R"）：22 条变红（1e21、0.00001、1234567.5 …；1e400 两条因范围判断仍在而不红）。
- 去掉 `double.IsInfinity / IsNaN` 判断：`CANON 1e400 -> 1e400`、`CANON -1e400 -> -1e400` 两条变红。

## (c) 运行时横排转换报文件行号

做了什么：`codegen/internal/tablegen/main.go` 的生成模板。横排表的 `Convert<Type>CSV` 改用共用的 `tablegenReadCSV`：逐条 `Read`，用 `reader.FieldPos(0)` 记下每条记录开始的行；`convert<Type>Records(records, lines)` 报错 `row=` 用这个行号。多行带引号的单元格报记录开始的那一行。

先红（修前）：

```
ConvertSceneCSV(bad id after comments) = row=3 field=ID value="x": ..., want row=5      （前两行是注释，报的比文件行号小 2）
ConvertSceneCSV(multi-line record) = row=2 field=ID value="x": ..., want row=3
```

（第二条是暂时去掉第一条断言后单独跑出来的。）

测试：`TestClientExportFixtureEndToEnd` 的生成 Go 程序里加两条：前面两行注释、第 5 行坏值报 `row=5`；`name,id` 表头下一条记录占第 3、4 行（坏值在第 4 行）报 `row=3`。

变异：

- 改回 `rowIndex+1`：`row=3 ... want row=5` 变红。
- `FieldPos(0)` 改成 `FieldPos(len(record) - 1)`（取出错单元格那一侧的行）：`row=4 ... want row=3` 变红。

## 生成物

仓库里没有提交的生成物受影响：`rg` 查 `gen_table_config.go`、`shared/config`、`ConfigJson.cs` 都没有提交件；demo 与 modules 的 schema 是 `.go.tmpl` 模板，没有具名类型。生成模板的变化由 roost 包里生成工程的用例（`TestGenerateClientConfigExport` 的 `generate --check`、`TestModuleVerifyLoadsTheProjectConfigData` 等编译生成工程的用例）与 `codegen/scripts/tablegen-runtime.sh`（`ROOST_CORE_DIR` 指向本检出）覆盖。

已有工程重新生成后的变化：`configs/generated/gen_table_config.go` 的 CSV 转换部分（`tablegenReadCSV`、辅助函数约束、`tablegenParseBool`）；声明了 `client_export.config` 的工程 `shared/config/csharp/ConfigJson.cs` 与 `manifest.json` 的文件哈希。

## 验证命令与结果

```
gofmt -l codegen/internal/tablegen codegen/internal/roost framework/configdata   # 无输出
GOWORK=off go build ./...                                                          # 通过
GOWORK=off go vet ./codegen/internal/tablegen/ ./codegen/internal/roost/ ./framework/configdata/...   # 通过
GOWORK=off go test -race -count=1 ./codegen/internal/tablegen/ ./framework/configdata/...
  ok codegen/internal/tablegen 15.6s / ok framework/configdata 7.1s / ok framework/configdata/rules 6.5s
ROOST_REQUIRE_DOTNET=1 GOWORK=off go test -race -count=1 -timeout 30m \
  -run 'ClientConfig|ConfigCSharp|TableJSON|TableTemplate|ConfigData|ShopTable|Tablegen|TableConfig|ClientExport' ./codegen/internal/roost/
  （结果见下）
cd codegen && ROOST_CORE_DIR=<本检出> GOPROXY=off sh scripts/tablegen-runtime.sh -count=1   # ok
```

dotnet 10.0.302（本机），`ROOST_REQUIRE_DOTNET=1`，C# 用例没有 SKIP。

roost 包选中的 22 个用例全部 PASS（`ok codegen/internal/roost 188.2s`）：TestClientConfigCSharpRuntime、TestConfigCSharpTypes、TestGenerateClientConfigExport、TestClientConfigExportTables、TestClientConfigExportTransactionalRetirement、TestClientConfigSync、TestClientConfigExportKeepsTheProtocolExport、TestClientConfigExportSpecValidation、TestUndeclaredClientConfigExportChangesNothing、TestModuleClientRulesAndShopTable、TestModuleVerifyLoadsTheProjectConfigData、TestGenerateSkipsConfigDataWhenSchemaHasNoCSVYet、TestGenerateWritesTheTableTemplateItReports、TestStagedProjectCommitAndCheckSeeTableJSONRetirement、TestProductionImageCarriesConfigDataWhereConfigDataDirResolves、TestShellInstallPutsConfigDataWhereConfigDataDirResolves 与协议导出的 6 个 TestClientExport* / TestGenerateClientExport / TestUndeclaredClientExportChangesNothing。

生成模板变了，另跑了编译生成 game-demo 的用例：`GOWORK=off go test -count=1 -timeout 40m -run 'TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout|TestDemoTemplateGeneratesABuildableWritePath' ./codegen/internal/roost/` → `ok 50.9s`（macOS 上不跳过；生成的 game-demo 对本检出 build + vet 通过）。整包 `codegen/internal/roost` 与全量 `go test ./...` 没有跑（按批次规则由协调者最后统一跑）。

## 尚未确认

- **Unity / IL2CPP 上 `double.ToString("R")` 是否给出最短往返位数**：新 `CanonicalFloat` 依赖它（.NET Core 3.0 起是最短往返）。本机只用 dotnet 10 跑过样例表；Unity 的 Mono / IL2CPP 运行库没有验证。若某个运行库的 "R" 不是最短，极端浮点值的 unique / enum 报错文字可能与服务端不同。
- (a) 里“以前具名类型让生成的服务端 Go 编译不过”是读生成文本得出的，没有在 742ccafc 上单独编译。

## 发现（不在本项范围，没有改）

- **指针标量字段让生成的服务端 Go 编译不过**：schema 里 `Bonus *int32` 的横排表，生成的 `Convert<Type>CSV` 报 `cannot use v (variable of type int32) as *int32 value in assignment`（本轮用临时用例在当前代码上编译确认；与本轮改动无关，742ccafc 的 `parseExpr` 已是先去掉 `*` 再解析）。客户端导出支持指针（可空类型），服务端 Go 绑定不支持；要么生成取地址的赋值，要么在 tablegen 里拒绝指针字段。需要维护者定。

## 建议写进 CHANGELOG（[Unreleased]）的文字

- **配置表：schema 包里的具名标量类型按底层类型处理**（交接 C5 (a)，维护者选项 1）：`type Rank int32` 这类声明在 schema 包里、底层是内建标量的具名类型，CSV 转换、index、客户端导出都按底层类型（C# 生成 `int`，不生成 enum；manifest 字段类型写底层类型）；服务端生成的 Go 写成 `meta0.Rank`（以前生成代码里是没有包名的类型名，无法编译）。别的包里的具名类型与底层不是内建标量的具名类型仍报错，报错说明原因。
- **配置表：数字规范写法两端一致**（交接 C5 (b)）：C# 运行部分的 `ConfigJson.Canonical` 改成与 `rules.Canonical` 相同的规则（`1e+21`、`1e-05`、`1.2345675e+06`，超出 double 范围取原文），两端共用样例表 `framework/configdata/rules/testdata/canonical_numbers.json`。规范字符串不进入任何哈希，`data_version` 不变；`shared/config/csharp/ConfigJson.cs` 内容变化，`roost generate --check` 会报过期，重新生成并提交。
- **配置表：横排表运行时转换报文件行号**（交接 C5 (c)）：生成的 `Convert<Type>CSV` 报错的 `row=` 是文件里的行号（注释行计数，跨行的带引号单元格报记录开始的行）。**升级**：重新生成并提交 `configs/generated/gen_table_config.go`（CSV 转换部分变化：`tablegenReadCSV`、解析辅助函数约束加宽、`tablegenParseBool`）。

## 建议改 KNOWN-LIMITS 的文字

`docs/maintenance/KNOWN-LIMITS.md` “配置表导出给客户端（2026-11-17）”一节：

- 删除“横排表转换报错的行号是去掉注释后的序号”（已修）。
- 删除“浮点的规范形式 Go 与 C# 不同”（已修）；如需保留运行库差异，改为：“C# 的浮点规范写法依赖 `double.ToString("R")` 给出最短往返位数（.NET Core 3.0 起成立），Unity Mono / IL2CPP 上尚未验证。”
- “具名类型不支持”改为：“**别的包里的具名类型、底层不是内建标量的具名类型不支持**：schema 包里底层是内建标量的具名类型（`type Rank int32`）按底层类型导出与建索引（C# 是底层类型，不生成 enum）；别的包里的（`time.Duration`、`foo.Kind`，生成器只读 schema 包自己的声明）与底层是结构体等的具名类型，导出与 index 报错并说明原因，要改成 parser 或 `side:"s"`。结构体、`[]byte`、键不是 string 的 map 同样不支持。”
- 新增（遗留，待维护者定）：“指针标量字段（`*int32`）的横排表，生成的服务端 `Convert<Type>CSV` 编译不过（`cannot use v (variable of type int32) as *int32 value`）；客户端导出支持指针。”
