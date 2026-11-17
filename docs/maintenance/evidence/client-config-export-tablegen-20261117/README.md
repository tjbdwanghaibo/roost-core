# 配置表导出给客户端：tablegen 组证据（2026-11-17）

方案：`docs/maintenance/FEATURE-2026-11-17-client-config-export.md`（决定 1、2、4 修订版、5、6，第 8 节第 1 组）。机器：本机 macOS，Go 1.27，`GOWORK=off`。

## 1. 未用新写法的工程，生成结果逐字节不变

HEAD 生成器（`55a9fd02`）与 HEAD + 本组改动的生成器，各自 `roost project new` 三种工程后 `diff -r`：

| 工程 | 命令 | 文件数 | diff -r |
| --- | --- | --- | --- |
| plain | `project new planet -module example.com/planet -out plain` | 75 | 0（无差异） |
| game | `... -out game -template game` | 87 | 0 |
| game-demo | `... -out demo -mods configdata,mongo,nats,dataengine,nest -template game-demo` | 1402 | 0 |

game-demo（本组生成器）再跑 `roost generate --check`：`generated files are up to date`。

## 2. 单测（tablegen、marker）

```
GOWORK=off go vet ./codegen/internal/tablegen/ ./codegen/internal/marker/      # 无输出
GOWORK=off go test -race -count=1 -v ./codegen/internal/tablegen/ ./codegen/internal/marker/
```

    --- PASS: TestCSVHeaderIsCaseSensitive
    --- PASS: TestCheckJSONEnforcesTheDeclaredRules
    --- PASS: TestCheckJSONRejectsMisspelledKeys
    --- PASS: TestClientExportDataVersionFollowsClientDataOnly
    --- PASS: TestClientExportFixtureEndToEnd
    --- PASS: TestClientExportRejectsBadSchema
    --- PASS: TestConvertCSVToJSONRefusesEditedOrLegacyUnownedRetirement
    --- PASS: TestConvertCSVToJSONRetiresOwnedDataAndPreservesManualData
    --- PASS: TestEveryMarkerKindHasASpecAndAGuardCase
    --- PASS: TestEveryMarkerRefusesAMisspeltKey
    --- PASS: TestFindRetiredPrefixReportsProductionSourcesOnly
    --- PASS: TestGeneratedLoaderAndObjectConverterMatchSchema
    --- PASS: TestHeaderCaseCheckCoversTheOtherSidesColumns
    --- PASS: TestIndexDeclarationsAreRejected
    --- PASS: TestIndexTagNamesTheIndex
    --- PASS: TestKVSheetBecomesTheSameObjectAsAWideSheet
    --- PASS: TestKVSheetRejectsBadKeys
    --- PASS: TestKVTemplateIsVertical
    --- PASS: TestObjectCSVRejectsMultipleDataRows
    --- PASS: TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader
    --- PASS: TestOnlyTheRoostPrefixIsRead
    --- PASS: TestParseMetaRootUsesTargetProjectModulePath
    --- PASS: TestProjectSideKeepsOnlyThatSide
    --- PASS: TestReadCSVRecordsEnforcesDeclaredRules
    --- PASS: TestReadCSVRecordsSkipsCommentLines
    --- PASS: TestReadCSVRecordsSkipsTitleTypeAndRuleRows
    --- PASS: TestRefDeclarationsAreResolvedAtGeneration
    --- PASS: TestRegexpIsAnchoredAndKindIsLiteral
    --- PASS: TestRunReturnsFlagErrors
    --- PASS: TestServerArtifactsCarryNoClientOnlyData
    --- PASS: TestSideDeclarationsAreParsedStrictly
    --- PASS: TestSideRulesAreRejected
    --- PASS: TestTableManifestOtherThanV2IsRejected
    --- PASS: TestUnknownTableKeyIsRejected
    --- PASS: TestUnlabelledFieldsRoundTripThroughGeneratedJSON
    --- PASS: TestWriteGeneratedRefusesToOverwriteWithoutForce
    ok   github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen 2.803s
    ok   github.com/tjbdwanghaibo/roost-core/codegen/internal/marker 3.483s

新增用例（全部在 `codegen/internal/tablegen`）：

- 按端分组：`TestSideRulesAreRejected`（主键两向排除、ref 跨端两向、object 字段全被排除两向）、`TestSideDeclarationsAreParsedStrictly`（side 取值、字段与整表的端相反、table 上写 layout、未知 layout）、`TestProjectSideKeepsOnlyThatSide`、`TestServerArtifactsCarryNoClientOnlyData`（JSON / manifest / Go 绑定 / omitted 打印 / -check）、`TestHeaderCaseCheckCoversTheOtherSidesColumns`。
- 竖排键值：`TestKVSheetBecomesTheSameObjectAsAWideSheet`（与横排 object JSON 逐字节相同、注释、note、按端）、`TestKVSheetRejectsBadKeys`（重复 / 未知 / 大小写 / 必填缺行 / 必填空值 / min / 解析失败 / 无表头 / 表头多列；只给客户端的必填缺行服务端也报）、`TestKVTemplateIsVertical`。
- index：`TestIndexTagNamesTheIndex`、`TestIndexDeclarationsAreRejected`（非标识符、object、主键、浮点、具名类型、parser、重名、与主键访问器同名）。
- `#` 注释：`TestReadCSVRecordsSkipsCommentLines`（含带引号的 `"#red"` 是数据、报错行号是文件行号）。
- 导出模型：`TestClientExportFixtureEndToEnd`、`TestClientExportDataVersionFollowsClientDataOnly`、`TestClientExportRejectsBadSchema`。

端到端夹具 `codegen/internal/tablegen/testdata/clientexport/`：scene（两端）、monster（带只给服务端的 `drop_seed`、只给客户端且必填的 `model`、`scene_id` 上的非唯一 index、parser 字段）、balance（`layout=kv`，各有一个只给服务端 / 只给客户端的 key）、tip（`side=c`）；CSV 都带 `#` 注释行。`TestClientExportFixtureEndToEnd` 跑 tablegen 的 data 与 go 两步后：

- `configs/data` 的每个文件都不含 `tip`、`model`、`shop_banner` 及其值，`balance.json` 逐字节等于只含服务端三键的对象，Go 绑定不含 Tip、不含只给客户端字段的 note；
- 生成的 Go 对本仓库 configdata 编译运行（`go run -mod=mod`）：`MonsterBySceneID(1)` 返回 10、12（文件顺序），`MonsterBySceneID(3)` 为空，`Model` / `ShopBanner` 在服务端为零值，`ConvertBalanceCSV` 读夹具 CSV（含注释）成功、重复 / 未知 / 缺必填 key 各报行号，`ConvertMonsterCSV` 容忍只给客户端的列；
- `ClientExport` 的四张表里没有 `DropSeed` / `ServerTick` 字段，Data 里没有 `drop_seed` / `server_tick` / `777` / `888`；两次调用 `reflect.DeepEqual`。

`TestClientExportDataVersionFollowsClientDataOnly`：只改 `drop_seed` 与 kv 的 `server_tick`，`DataVersion` 不变；scene.csv 换成 CRLF，不变；monster 只给客户端的 `model`、scene 的 `name`、kv 只给客户端的 `shop_banner`、tip 的 `text` 各改一处，`DataVersion` 每次都变。

另外跑过：`codegen/scripts/tablegen-runtime.sh`（`ROOST_CORE_DIR` 指本仓库）`ok`；`GOWORK=off go test -run 'TestOrchestratorDoesNotRepeatGeneratorDefaultPaths|TestStagedProjectCommitAndCheckSeeTableJSONRetirement' ./codegen/internal/roost/` `ok`；在本组生成器生成的 game 工程里放入夹具，`roost generate` 后 `generate --check` 干净，改 kv 的 note 后 `--check` 报 `configs/generated/gen_table_config.go` 过期；该工程 `go build ./configs/... && go vet ./configs/...` 对已发布的 roost-core v1.53.0 通过。

## 3. 变异

脚本逐处撤掉关键的一行，`-run` 跑对应用例，记录后恢复（恢复后全量单测见第 2 节）。全部变红，红都落在承诺本身：

```
M1 主键不能排除: RED -run 'TestSideRulesAreRejected'
    --- FAIL: TestSideRulesAreRejected (0.01s)
    side_test.go:49: validateMetas = <nil>, want an error containing "key field ID is side:\"s\""
    side_test.go:49: validateMetas = <nil>, want an error containing "key field ID is side:\"c\""
M2 ref 同端: RED -run 'TestSideRulesAreRejected|TestClientExportRejectsBadSchema'
    --- FAIL: TestClientExportRejectsBadSchema (0.01s)
    export_test.go:292: ClientExport = <nil>, want the cross-side ref named
    --- FAIL: TestSideRulesAreRejected (0.01s)
    side_test.go:49: validateMetas = <nil>, want an error containing "ref target \"scene\" is not on the server side"
M3 服务端 JSON 去掉客户端字段（横排）: RED -run 'TestServerArtifactsCarryNoClientOnlyData|TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.01s)
    export_test.go:81: server configs/data/monster.json carries client-only "model":
    --- FAIL: TestServerArtifactsCarryNoClientOnlyData (0.00s)
    side_test.go:153: server item.json = [
M3b 服务端 JSON 去掉客户端字段（kv）: RED -run 'TestKVSheetBecomesTheSameObjectAsAWideSheet|TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.01s)
    export_test.go:81: server configs/data/balance.json carries client-only "shop_banner":
    --- FAIL: TestKVSheetBecomesTheSameObjectAsAWideSheet (0.00s)
    kv_test.go:47: server kv values = map[string]interface {}{"banner":"summer", "max_level":60, "start_gold":100, "tick":50}, want map[string]interface {}{"max_level":60, "start_gold":100, "tick":50}
M4 导出模型去掉服务端字段: RED -run 'TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.69s)
    export_test.go:174: export model carries server-only field balance.ServerTick
M5 kv 重复 key（生成时）: RED -run 'TestKVSheetRejectsBadKeys'
    --- FAIL: TestKVSheetRejectsBadKeys (0.02s)
    kv_test.go:90: readKVObject = <nil>, want an error containing "balance.csv:4: duplicate key \"max_level\" (first at line 2)"
M5b kv 重复 key（运行时转换）: RED -run 'TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.69s)
    export_test.go:164: generated server binding: exit status 1
    duplicate kv key: <nil>
    exit status 1
M6 # 注释（生成时读取）: RED -run 'TestReadCSVRecordsSkipsCommentLines|TestKVSheetBecomesTheSameObjectAsAWideSheet|TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.00s)
    export_test.go:60: balance.csv:1: a layout=kv sheet starts with the header key,value,note, got "# 竖排键值：一行一个字段"
    --- FAIL: TestKVSheetBecomesTheSameObjectAsAWideSheet (0.00s)
    kv_test.go:43: balance.csv:1: a layout=kv sheet starts with the header key,value,note, got "# 数值"
M6b # 注释（运行时 kv 转换）: RED -run 'TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.69s)
    export_test.go:164: generated server binding: exit status 1
    ConvertBalanceCSV = {MaxLevel:0 StartGold:0 ServerTick:0 ShopBanner:}, line=1: a layout=kv sheet starts with the header key,value,note, got "# 竖排键值：一行一个字段"
    exit status 1
M7 index 查询（登记索引）: RED -run 'TestClientExportFixtureEndToEnd'
    --- FAIL: TestClientExportFixtureEndToEnd (0.68s)
    export_test.go:164: generated server binding: exit status 1
    MonsterBySceneID(1) = []
    exit status 1
M8 DataVersion 跟随客户端数据: RED -run 'TestClientExportDataVersionFollowsClientDataOnly'
    --- FAIL: TestClientExportDataVersionFollowsClientDataOnly (0.01s)
    export_test.go:280: editing monster.csv (slime.prefab -> slime2.prefab) left the data version at e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
M8b DataVersion 不含服务端字段: RED -run 'TestClientExportDataVersionFollowsClientDataOnly'
    --- FAIL: TestClientExportDataVersionFollowsClientDataOnly (0.01s)
    export_test.go:253: server-only edits changed the data version: 3065534afb5590c45843c7b081ff27288ff618ea5b877e8783aeb6bba0d6ccdd -> 8425cd1177e91e4b40716156c3416cb272b698e5415390fc4db075cbb12ea0b1
```

M1 用 `_ = key` 代替删除的 return，避免编译错误冒充变红。

## 4. 建议写进 CHANGELOG 的文字（本组不改 CHANGELOG）

- **配置表：按端分组、竖排键值表、index、`#` 注释行（配置表导出给客户端第 1 组，维护者 2026-11-17 决定 1、2、5、6）**：`//roost:table` / `//roost:object` 新增标记选项 `side=c|s`、`//roost:object` 新增 `layout=kv`；字段 tag 新增 `side:"c"|"s"` 与 `index:"true"|"<名字>"`。服务端的 `configs/data`、`_manifest.json`、Go 绑定只含服务端投影（`side=c` 的表不登记，只给客户端的字段不进 JSON；schema 结构体里仍有这些字段，值为零值），生成打印 `tablegen: server side omitted N tables / M fields (client only)`；主键不能带 side、ref 必须指向同一端的表、一端字段全被排除时报错，两端每次生成都核对。`layout=kv` 的 CSV 是 `key,value,note` 竖排，JSON 与普通 object 相同，note 写进 `<Type>Cfg` 注释（`roost generate` 的 config-go 步骤为此带 `-csv configs/table`，并把 `configs/table/` 列为输入）；未知 / 重复 key、必填缺行报文件、行与 key。index 生成非唯一的 `<Type>By<Index>(v) []<Type>Cfg`。CSV 里 `#` 开头的行在生成步骤与 kv 的 `Convert<Type>CSV` 里忽略，转换报错的 `row=` 改为文件行号。新增 `tablegen.ClientExport(metaDir, csvDir)` 导出模型（含 `DataVersion`），供第 2 组生成 `client_export.config`。不用这些写法的工程生成结果逐字节不变。说明见 `codegen/docs/CODEGEN_REFERENCE.zh-CN.md` §9.2；证据 `docs/maintenance/evidence/client-config-export-tablegen-20261117/`。

## 5. 建议写进 KNOWN-LIMITS 的文字（本组不改 KNOWN-LIMITS）

- **横排表生成的 `Convert<Type>CSV` 不识别 `#` 注释行**（遗留，待维护者定）：生成步骤与 kv 表的转换忽略 `#` 行，但横排表（`//roost:table`、横排 `//roost:object`）生成的运行时 `Convert<Type>CSV` 仍把 `#` 行当数据。补上只需在模板里加一行 `reader.Comment = '#'`，但会让每个已有工程的 `gen_table_config.go` 每张表多一行，与“未用新写法的工程生成结果逐字节不变”冲突。选项：A（推荐）无条件加上，接受这一行差异并在 CHANGELOG 写“重新生成”；B 维持现状，文档要求给 `Convert<Type>CSV` 的 CSV 不带注释。
- **index 只支持内建 string / 整数 / bool 字段**（框架限制）：生成器只看 Go 类型表达式，不知道具名类型（如 `type Kind int32`）的底层类型，具名类型、浮点、指针、带 parser 的字段上写 index 生成时拒绝。
- **带 parser 的字段在客户端只有原始字符串**（方案第 7 节，已定）：导出模型里 `Parser` 非空，JSON 里是 CSV 原文。
