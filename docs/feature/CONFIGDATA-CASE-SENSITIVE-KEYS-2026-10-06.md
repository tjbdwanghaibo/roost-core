# configdata 键大小写敏感（2026-10-06）

维护者原话：“configdata 需要大小写敏感。”（[下一轮规划](../review/NEXT-ROUND-PLAN-2026-10-06.md) 第 2 项）原则沿用 B10：使用容易、手写代码少、结构简单。

基线 `origin/main` `2a4c835d`（v1.21.0 发版准备之后）。分支 `cfgcase`。本改动不进 v1.21.0。

## 1. 现状

- 加载层用 encoding/json 解进行结构体，键匹配大小写不敏感：`{"Level":1}` 静默填进 json 名为 `level` 的字段；一行里 `{"level":1,"Level":2}` 按文档顺序最后一个生效。严格模式（`DisallowUnknownFields`）也放过这两种，因为 `Level` 对 encoding/json 不是未知字段。
- `5a3c4a60` 第 5 项（[记录](../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)）为了让规则查到的值与类型化行一致，在 `configdata/rules.Rows` 里对“同一字段几种大小写拼写”按原文顺序只留最后一个，`Lookup` 先精确、否则大小写不敏感。
- 规则声明 `FieldRule.Field` 绑定字段时同样大小写不敏感（`fieldForJSONKey`）。
- tablegen：CSV 表头按 csv 名逐字查，只差大小写的表头被当成未知列跳过，这一列的值静默丢成零值；`-check` 走 `rules.Lookup`，同样大小写不敏感。生成的 `Convert<Type>CSV` 与 CSV 转换同形。
- cfggen：没有数据期检查；字段名只差大小写的两个字段已被“映射到同一个 Go 字段”拒绝（`exportName` 相同）。

## 2. 规则

数据文件里的键必须与声明字段的 json 名**逐字一致**。只差大小写的键（含一行里同一字段的几种拼写，至多一个是逐字拼写）在 Load / Reload / DryRun 一律拒绝，整次加载失败、旧快照保持，错误是 `*configdata.RuleError`，`Rule` 为 `case`，点名表、行（1 起，对象为 0）、行主键、应有拼写的位置与原文的键：

```text
configdata: table monster row 1 (key 1) field level: case: key "Level" must be spelled "level" (keys are case-sensitive)
configdata: table monster row 1 (key 1) field rewards[1].item_id: case: key "Item_ID" must be spelled "item_id" (keys are case-sensitive)
```

- **未声明的键维持原行为**：与所有声明名都不只差大小写的键，宽松模式忽略，严格模式由解码报 `unknown field`。
- **逐层**：嵌套结构体、切片 / 数组元素、map 的值按各自类型核对；map 的键、`interface{}`、自带 `UnmarshalJSON` 的类型（`json.RawMessage`、`time.Time` 等）由作者决定，不核对。没有 json 名的嵌入结构体（含指针嵌入）的字段按 encoding/json 提升到外层。
- **规则声明**：`FieldRule.Field` 必须逐字是字段的 json 名，否则注册失败，错误给出应有拼写。
- **CSV 表头**：只差大小写的表头在 CSV 转换与生成的 `Convert<Type>CSV` 里报 `header "Level" must be spelled "level" (column names are case-sensitive)`；未声明的列照旧跳过。

## 3. 改动面

| 位置 | 改动 |
| --- | --- |
| `configdata/rules` | 新增 `MisspelledKey`（唯一的拼写规则）、`CaseError`、`CheckKeys` / `CheckObjectKeys`（行形式，生成器用）。**删除** `5a3c4a60` 的 `hasCaseVariants` / `keepLastCaseVariant` / `foldName`，`Rows` 不再预处理，`Lookup` 只逐字匹配 |
| `configdata` | `keyspelling.go`：解码成功后按行类型逐层核对原始键（`checkKeySpelling`），表与对象每次加载都跑（不只在声明了 Rules 时）；`fieldForJSONKey` 改为逐字匹配 |
| `codegen/internal/tablegen` | `-check` 先跑 `rules.CheckKeys` / `CheckObjectKeys`；CSV 转换用 `rules.MisspelledKey` 核对表头；生成的 `Convert<Type>CSV` 多一个 `tablegenCheckHeader`（只用标准库，同一条规则的生成代码副本——生成代码不引入新 core API，不抬生成器 Core 下限） |
| cfggen | 不改代码：生成的 `cfg` / `json` 标签由运行时 configdata 核对；字段名只差大小写已在生成期被拒 |
| kit/configdata | 不改：Mod 的 Start / Reload 走同一个 Store |

生成期检查只看顶层字段（tablegen 的 meta 不知道嵌套类型的字段），嵌套由加载层核对；两处调用同一个 `MisspelledKey`。

## 4. 兼容

- **行为收紧**：以前能加载的、键只差大小写的配置数据，现在启动 / reload 失败。手写的 `FieldRule{Field: "Level"}` 对着 `json:"level"` 现在注册失败。键名改成声明的拼写即可。
- 生成的 game-demo 数据（`configs/data/*.json`）全部是生成器写出的精确键，不受影响（见验证）。
- 已生成工程不需要重新生成：校验在 core 里。重新生成只多出 `Convert<Type>CSV` 的表头检查。
- 性能：每次加载多解析一次载荷（原始行 + 逐层 map），只在加载 / 热更时发生。

## 5. 验证

先红后绿（修前文本）：

```text
$ GOWORK=off go test -count=1 -run 'Misspelled|Undeclared|RuleFieldMust' ./configdata/
--- FAIL: TestMisspelledKeysRejectLoad/variant_only (0.00s)            # {"id":1,"Level":1}
        key_case_promises_test.go:84: err = <nil>, want a *RuleError
--- FAIL: TestMisspelledKeysRejectLoad/exact_then_variant (0.00s)      # {"id":1,"level":1,"Level":2}
        key_case_promises_test.go:84: err = <nil>, want a *RuleError
    （variant_then_exact / second_row / nested / object / wrapped_rows 同样，宽松与严格模式各一遍）
--- FAIL: TestMisspelledKeysRejectReloadAndDryRun/variant_only (0.00s)
        key_case_promises_test.go:108: err = <nil>, want a *RuleError
    （其余 6 个子用例同样）
--- FAIL: TestRuleFieldMustMatchTheJSONNameExactly (0.00s)
    key_case_promises_test.go:151: register err = <nil>, want a refusal naming level
$ GOWORK=off go test -count=1 -run 'MisspelledKeys|CaseSensitive' ./codegen/internal/tablegen/
--- FAIL: TestCheckJSONRejectsMisspelledKeys (0.00s)
    key_case_promises_test.go:21: [{"id":1,"name":"slime","Level":3,"code":"a"}]: err = <nil>, want "table monster row 1 (key 1) field level: case: key \"Level\" must be spelled \"level\""
--- FAIL: TestCSVHeaderIsCaseSensitive (0.00s)
    key_case_promises_test.go:33: err = <nil>, want the misspelled header named
$ ROOST_CORE_DIR=<worktree> sh codegen/scripts/tablegen-runtime.sh -count=1 -run MisspelledHeaders   # 旧生成器
--- FAIL: TestGeneratedCSVConverterRejectsMisspelledHeaders (0.00s)
    roundtrip_test.go:131: ConvertMonsterCSV err = <nil>, want the misspelled header named
```

`TestUndeclaredKeysKeepTheirBehaviour`（未声明的键宽松忽略、严格拒绝）修前修后都通过，守“未声明的键维持原行为”。修后以上全部通过。`configdata/rules` 的 `5a3c4a60` 用例（`TestRulesCheckTheValueEncodingJSONDecodes` 等）随被删的选择逻辑一起换成 `key_spelling_promises_test.go`（拼写规则、`Lookup` 逐字）。

修后矩阵（全部 `GOWORK=off`）：

- `gofmt -l` 为空；`go build ./... && go vet ./...` 通过。
- `go test -race -count=3 ./configdata/... ./kit/configdata/ ./codegen/internal/tablegen/ ./codegen/internal/cfggen/` 通过（含 B10 规则用例）。
- 根包 `go test -count=1 .` 通过（`configdata/rules` 仍只依赖标准库）；`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后无改动。
- `ROOST_CORE_DIR=<worktree> sh codegen/scripts/tablegen-runtime.sh -race -count=1` 通过（原三条 + 新的表头用例）。
- `examples/configgen`、`examples/lubanreal` 运行正常。
- 生成 game-demo（`roost project new … -template game-demo`，replace 到 worktree）：`roost generate` 与 `--check` 通过，`go build ./... && go vet ./... && go test -count=1 ./...` 通过；临时用例走启动路径（`registry.RegisterAll` + kit `configdata` Mod `Init` / `Start`，`configs/data`）加载成功、严格模式 reload 成功；把 `spawn.json` 的 `template`、`feature_flag.json` 的 `enabled`、`item.json` 的 `max_stack` 各改一处大小写，Start 分别报 `table spawn row 1 (key 1) field template: case: key "Template" must be spelled "template"` 等（临时用例不入库）。
- 没有改 nest / entity / dataengine / sync，没有跑 glsvet；没有跑真实依赖（不涉及）。

## 6. 未验证 / 限制

- `tablegen-runtime.sh` 按 pin（v1.21.0）跑时新用例只依赖生成代码，不依赖本改动的 core；本改动的运行时行为在 pin 升到下一版之前只能用 `ROOST_CORE_DIR` 验。
- 结构体里有同一 json 名在不同嵌入深度重复（encoding/json 的同名消解）时，按“浅层优先”取类型，与 encoding/json 的歧义丢弃不完全相同；只影响那种结构体的嵌套核对。

## 实施状态

已实施（见 [下一轮规划](../review/NEXT-ROUND-PLAN-2026-10-06.md) 第 2 项状态栏的提交号），未发版。
