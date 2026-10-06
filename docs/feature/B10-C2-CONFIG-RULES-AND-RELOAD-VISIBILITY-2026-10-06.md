# B10 / C2：配置规则统一由运行时加载层强制；热更失败与回滚可见（2026-10-06）

> **状态（2026-10-06 核对）**：已实施（`b12216ed`），已随 v1.21.0 发布。文末“cfggen globals 规则留作后续”已在第十二轮实施（`229a5aa0`，[记录](ROUND12-SKILL-CFGGEN-2026-10-06.md)，未发版，随 v1.23.0 发布）。

维护者决定（[DECISIONS-PENDING 第四轮](../review/DECISIONS-PENDING-2026-10-05.md)）：

- **B10**：按方案 A——configdata 新增能看到原始 JSON 字段是否出现的接口，必填在加载 / 热更时检查；所有规则统一由运行时加载层强制，生成期检查只作提前反馈，与运行时用同一份规则声明。附加原则：**config 使用要容易，手写整理代码尽量少，结构简单易懂**。
- **C2**：保持“新快照即刻可见”（AfterApply 之前已发布，回滚前准入的请求读被回滚的那一代），写进契约；热更失败（整次拒绝）与回滚要可见——日志 + 指标，标签低基数（N07 C-O5、C-O6），并修正 observability README 的版本号说法（C-O7）。

基线 `origin/main` `4f0bab75`（v1.20.2 之后）。分支 `b10cfg`。

## 1. 现状

| 规则 | tablegen（Go 标签 + CSV） | cfggen（YAML → `cfg` 标签 → `RegisterAutoTable`） |
| --- | --- | --- |
| required | CSV 转 JSON、`-check` 时查（`parseCell` / `checkJSONFiles`）；运行时不查——configdata 只拿到类型化的行，分不清缺列与零值 | 只能配合 ref，含义是“零值也报错”；运行时（auto 表）查 |
| unique / min | CSV 转 JSON、`-check` 时查（`validateRows`）；运行时不查 | 不支持 |
| ref | 生成期只查 schema（`resolveRefs`）；运行时由**生成的** `ValidateTable` 逐表手写循环查 | 运行时由 auto 表的 `validateRefs` 查 |
| 主键唯一 | 生成期 `validateRows`；运行时 `newTable` | 运行时 `newTable` |

三套检查实现（tablegen 的 `validateRows`/`parseCell`、生成 loader 里的 ref 循环、auto 表的 `validateRefs`），两种语义的 `required`。直接改 `configs/data/*.json` 再 `gm.config.reload` 只经过运行时那一层，于是 required / unique / min 全被绕过（N07 H2e：删掉 spawn 的 `template` 后 reload 被接受，刷出 template 0）。

热更可见性：`Store.ReloadWithReason` 在 build / 监听者 Validate 阶段失败时不经过任何监听者，kit 的指标只挂在 AfterApply / Rollback 上，所以失败的 reload 在日志与指标里都不留痕迹；AfterApply 失败被撤回时 metrics 监听者已经记了一次 `ok`，回滚又记一次 `rollback`（一次尝试两笔）；`reason` 标签是 GM payload 里运维自由填写的文本。

## 2. 目标设计

### 2.1 一份规则表示、一个检查器：`configdata/rules`

新增叶子包 `configdata/rules`（只依赖标准库），是“配置数据规则”的唯一实现，运行时加载层与生成器共用：

```go
type Rule struct {
	Field    string   // 数据文件里的 JSON 键
	Required bool     // 每行都必须出现且不为 null
	Unique   bool     // 任两行的值不相同（缺省 / null 的行不参与）
	Min      string   // 十进制下限，"" 表示无
	Ref      string   // 非零值必须是该表的主键（由加载层在全部表加载后查）
	Enum     []string // 允许的取值（字符串形式），空表示不限
}
type Error struct{ Table string; Row int; Key, Field, Rule, Detail string } // “表 / 行 / 字段 / 规则”
func Document(raw []byte) ([]byte, error)                                 // 文件 → 载荷（rows / records / data 单包装键）
func Rows(payload []byte, object bool) ([]map[string]json.RawMessage, error)
func Check(table string, rows []map[string]json.RawMessage, rules []Rule, key func(row int) string) error
func CheckObject(table string, row map[string]json.RawMessage, rules []Rule) error
func Lookup(row map[string]json.RawMessage, field string) (json.RawMessage, bool) // encoding/json 的键匹配
func Canonical(value json.RawMessage) string                                      // 1 / 1.0 / 1e0 同形
```

- 规则在**原始 JSON 行**上检查，所以缺列与零值分得清。`Ref` 需要目标表，由 configdata 在全部表加载后用类型化的表查（沿用 auto 表的 `refKeyLookup`），`Check` 跳过它。
- 错误一律是 `*rules.Error`，文本形如 `config table spawn row 1 (key 1) field template: required: missing or null`，可 `errors.As` 取结构化字段。
- 分层：codegen 层原本不得 import core（生成器独立于运行时）。这里给根包边界测试加**唯一例外** `configdata/rules`，并加一条门禁：该包只能 import 标准库。理由：规则的检查是“同一份规则声明”的一部分，例外只开给一个无依赖的叶子包。

### 2.2 运行时加载层（configdata）

- `TableDef.Rules` / `ObjectDef.Rules []FieldRule`（`FieldRule = rules.Rule`，`RuleError = rules.Error` 别名）。加载时同一份字节解一次类型化行、一次原始行，跑 `rules.Check`；全部表加载后跑 ref；任何违反 → 整次 build 失败，旧快照保持（沿用现有语义）。
- 注册时校验声明：规则字段必须对应行类型的一个字段（按 encoding/json 的键匹配），`Min` 只能用在数值字段、`Enum` 只能用在字符串 / 整数 / bool、`Ref` 只能用在整数 / 字符串（可为指针）；对象不支持 `Unique` / `Ref`。拼错的规则在启动时失败，不是“永远不查”。
- `required` 统一为“出现且不为 null”。`required` + `ref`：值必须出现且是目标表的主键（零值不再当“无引用”放过）。
- auto 表（`cfg` 标签）解析成同一组 `Rule`：`required` 不再要求配 `ref`，新增 `unique`、`min=<n>`、`enum=a|b`；auto 表自己的 `validateRefs` 删除，ref 走公共路径。

### 2.3 生成器

- **tablegen**：`metaRules(meta)` 把 schema 标签（`required` / `unique` / `min` / `ref` / 新增 `enum:"a|b"`；主键隐含 `unique`）转成 `[]rules.Rule`，这一份同时用于：
  1. CSV 转 JSON：先写出将要落盘的 JSON，再 `rules.Check`（空的 required 单元格写成 null，由同一检查器报出）；
  2. `-json -check`：`rules.Document` + `rules.Rows` + `rules.Check`（删掉 tablegen 自己的 `validateRows` 与 `jsonRows`）；
  3. 生成的 loader：`TableDef{..., Rules: []configdata.FieldRule{{Field: "template", Required: true}, ...}}`，由 configdata 在每次加载 / 热更执行。
  生成 loader 里手写的 ref 循环（每个带 ref 的表约 25 行）删除。改一个标签，`roost generate` 后生成期与运行时同时变。
- **cfggen**：YAML 字段新增 `unique`、`min`、`enum`，`required` 不再要求 `ref`；输出仍是 `cfg` 标签，由 auto 表解析成同一组 `Rule`。cfggen 不处理数据，没有生成期数据检查。
- 生成 API：tablegen 每张表多一个 `<Type>Table()`（取当前请求钉住的快照里的表，nil 安全），与已有的 `<Type>By<Key>(id)` 对称；其余不变。

### 2.4 tablegen 与 cfggen 能否合成一套

评估结论：**规则表示与检查已合成一套**（`rules.Rule` + `rules.Check` + configdata 的 ref），两种标签方言只是两个入口。把 tablegen 的 schema 也改成 `cfg` 标签、生成器只留一个，代价是所有已有工程的 schema 标签都要改写（CSV 流程、`csv` / `title` 标签 cfggen 没有对应物），收益只是少一个解析函数；**列为后续**，不在本轮做。cfggen 的 globals 也暂不支持规则（cfggen 现在对 globals 拒绝 index / ref，规则同理先拒绝），列为后续。

### 2.5 C2：契约与可见性

- 契约（configdata 包注释、`docs/USER_GUIDE.md`）：Reload 先 build + Validate + BeforeApply，然后**发布**，再跑 lifecycle emit 与 AfterApply；发布后到 AfterApply 结束之间准入的请求读新的一代；AfterApply 失败时撤回，期间准入的请求整个生命周期读被撤回的那一代（钉住准入时的快照，不撕裂）。业务若不能接受，就把检查放进 Validate / BeforeApply。
- Store 每次 Load / Reload / Rollback 结束后恰好报告一次结果（`ReloadOutcome`，含阶段 `build` / `validate` / `before_apply` / `apply`、是否已发布后撤回、候选版本、当前版本、错误），通过 `Store.OnReloadOutcome(func(ReloadOutcome)) (unsubscribe func())` 订阅；Store 自己写日志：成功 Info `config reload applied`，失败 Warn `config reload failed`（stage、reason、version、error），撤回 Warn `config reload reverted`，运维 Rollback Info `config rolled back`。
- kit `configdata` Mod 改用这个结果记指标（不再挂 ReloadHook），标签只取低基数值：
  - `configdata.reload.total{result=ok|failed}`：每次 Load / Reload 恰好一笔；
  - `configdata.rollback.total{trigger=apply_failed|operator}`：发布后被撤回 / 运维 `Store.Rollback` 成功；
  - `configdata.version`：当前在用的一代。
  `reason` 不再作标签（C-O6），进日志。
- observability README（C-O7）：版本号由单调计数器分配，失败与 DryRun 也占号，成功后版本变大但不一定 +1；rollback 回到上一代的版本号。

## 3. 改动面

| 位置 | 改动 |
| --- | --- |
| `configdata/rules/`（新） | `Rule` / `Error` / `Document` / `Rows` / `Check` |
| `configdata/configdata.go` | `TableDef.Rules`、`ObjectDef.Rules`、注册期声明校验、加载期规则与 ref；`ReloadOutcome` / `OnReloadOutcome`；日志；包注释写 C2 契约 |
| `configdata/auto.go` | `cfg` 标签 → `Rule`；删 auto 自己的 ref 检查 |
| `kit/configdata` | 指标改由 `OnReloadOutcome` 记录，去 `reason` 标签 |
| `codegen/internal/tablegen` | `metaRules`；CSV / `-check` / 生成 loader 共用规则；`enum` 标签；`<Type>Table()` |
| `codegen/internal/cfggen` | `required` 独立、`unique` / `min` / `enum` |
| `codegen/scripts/tablegen-runtime.sh` | `ROOST_CORE_DIR` 用本地 core 替换 pin |
| `dependency_boundary_test.go` | codegen → `configdata/rules` 例外 + 叶子门禁 |
| demo 模板 | `refresh.go` 删 `spawnRow`、用生成的 accessor；schema 注释；observability README |
| 文档 | USER_GUIDE、CODEGEN_REFERENCE、CFGGEN_META、help、kit README、TROUBLESHOOTING、CHANGELOG、NC-75 记录 |

## 4. 兼容

- **新 configdata API**：生成器输出（tablegen 的 `Rules`）需要包含本改动的 core。下次发版时上调 `minimumVersions.Core`（`codegen/internal/roost/manifest.go`）与 `.github/workflows/framework-compat.yml` 的 minimum 行——发版准备时统一改，本分支不改。在那之前 CI 里按 pin 跑的 `tablegen-runtime.sh` / framework-compat 最低版本行会因 pin 里还没有 `FieldRule` 而编译失败，这是预期的“下限该升了”的信号。
- **已生成的工程不迁移**：旧的 `gen_table_config.go` 不带 `Rules`，在新 core 上照常编译、行为不变（只是仍不查 required）；重新 `roost generate` 后获得运行时规则。
- 行为收紧：
  - 运行时开始执行 required / unique / min（tablegen）——以前直接改 JSON 能绕过的数据现在 reload / 启动被拒（旧快照保持）；
  - auto 表 `required`（配 ref）：缺列或 null 以前因“零值”被拒，现在以 required 被拒；值为 0 时按普通 ref 查目标表（目标表真有主键 0 时不再误拒）；
  - tablegen object 文件也按 `rows` / `records` / `data` 单包装键读取（与 configdata 一致，以前 `-check` 对 object 不拆包装）。
- 指标：`configdata.reload.total` 去掉 `reason` 标签、`result` 取值从 `ok|rollback` 改为 `ok|failed`，新增 `configdata.rollback.total{trigger}`；看板按 `sum(rate(configdata_reload_total))` 的查询不受影响。

## 5. 验证（先红后绿）

| 承诺 | 回归 | 修前（红） |
| --- | --- | --- |
| 删除 required 字段 / required 为 null / 低于 min 后 reload 被点名拒绝 | `codegen/scripts/tablegen-runtime.sh`：`TestDeclaredRulesAreEnforcedOnReload` | `reload accepted a required column is deleted` 等三条 |
| 一个标签同时驱动生成期检查与生成的 loader | `codegen/internal/tablegen/rules_single_source_promises_test.go` | `min=1: generated loader does not carry {Field: "level", Min: "1"}` |
| 失败 / 撤回的 reload 留日志与指标、标签低基数 | `kit/configdata/reload_visibility_promises_test.go` | `result=ok = 3, want 2`、`result=failed = 0`、`rollback.total = 0`、`carries label "reason"`、日志 0 行 |

另有 configdata / rules 单测覆盖各规则与注册期声明校验；端到端在生成的 game-demo 上用 `gm.config.reload` 验证（见第 6 节）。

## 6. 实施状态

**已实施，未发版**（分支 `b10cfg`，提交见 DECISIONS-PENDING 第四轮表）。与第 2 节的差异：无（cfggen globals 的规则如方案所列留作后续）。

### 改前 / 改后业务需要手写的代码

| 场景 | 改前 | 改后 |
| --- | --- | --- |
| 防“删列 / 改名读成零值”（N07 H2e） | 无入口：configdata 看不到原始 JSON；只能在每张表手写 `ValidateTable` 再读一遍文件（方案 B，约 25～30 行 / 表，有替换窗口），或在使用处写 `if row.Template == 0` 之类的防御 | 0 行：schema 标签 `required:"true"`（已有的标签直接生效） |
| min / unique / enum | 运行时无入口，同上 | 0 行：标签 |
| tablegen 的 ref | 生成器为每个带 ref 的表生成约 25 行的 `ValidateTable` 循环 | 生成物里每条规则 1 行 `{Field: "scene_id", Ref: "scene"}` |
| game-demo 读 spawn 表（`refresh.go`） | 15 行代码：`SpawnTableFrom(configdata.ActiveSnapshot())` + `ok` 判断两处，外加手写 `spawnRow` 包装 | 3 行：`generated.SpawnTable().Rows()`、`generated.SpawnByID(group)`（生成的 accessor，nil 安全） |
| 热更失败可见 | 业务 / 运维自己在 GM 命令处补日志；kit 的指标看不到 build 阶段失败 | 0 行：Store 自带日志，kit 自带指标 |

game-demo 的其余配置读取（`ItemByID`、flags 的 `FeatureFlagTableFrom(snapshot)`）本来就是一行 accessor，没有可去掉的胶水；flags 钩子是业务逻辑（表 → featureflag 的转换），保留。

### 红绿与验证（全部 `GOWORK=off`，基线 `4f0bab75`）

修前红文本见 [evidence/b10-c2-20261006/red.txt](evidence/b10-c2-20261006/red.txt)：

1. `ROOST_CORE_DIR=<worktree> sh codegen/scripts/tablegen-runtime.sh`：`TestDeclaredRulesAreEnforcedOnReload` 三子测试修前 `reload accepted …`，修后 `-race` 全绿（含原 `TestDanglingRef…` / `TestGeneratedLoader…`）。
2. `go test -run TestOneTagDrives ./codegen/internal/tablegen/`：修前 `generated loader does not carry {Field: "level", Min: "1"}`，修后通过（min=1 / min=5 两轮，生成期与 loader 同变）。
3. `go test -run TestFailedReloadAndRollback ./kit/configdata/`：修前 `result=ok = 3`、`result=failed = 0`、`rollback.total = 0`、`reason` 标签、日志 0 行；修后通过。
4. 端到端（[evidence/b10-c2-20261006/e2e-reload.txt](evidence/b10-c2-20261006/e2e-reload.txt)）：当前 CLI 全新生成 game-demo（replace 到本 worktree，DAO 库名改唯一），隔离环境 Mongo / NATS / Redis + 私有 etcd，起 global + game；`gm.config.reload`：count 3→4 成功（版本 2，存活 4）；删掉 spawn 的 template → `configdata: table spawn row 1 (key 1) field template: required: missing or null`；hp 0 → `min`；template null → `required`；三次失败后版本仍 2、存活仍 4，`configdata_reload_total{result="failed"} 3`，game 日志三条 Warn `config reload failed … stage=build`；恢复后 reload 版本 6（失败占号）。修前同一场景见 N07 第二批 H2e（reload 被接受、刷出 template 0，日志 0 行）。用完删除自建的 Mongo 库（`b10cfg_*`）、JetStream 流（`B10CFG_*`）、Redis 键（`roost:b10gd:*`）与私有 etcd 数据。

本地矩阵：`gofmt -l` 空；`go build ./... && go vet ./...` 通过；`go test -race -count=3 ./configdata/... ./kit/configdata/ ./codegen/internal/tablegen/ ./codegen/internal/cfggen/` 通过；根包 `go test -count=1 .` 通过（含新增 `TestSharedConfigRulesStayALeaf`）；`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后无改动；`tablegen-runtime.sh` / `cfggen-golden-runtime.sh`（`ROOST_CORE_DIR` 指向本 worktree，`-race`）通过；生成 game-demo tidy / build / vet / `go test ./...` / `roost generate --check` 通过，`game/scene/runtime`、`internal/service/game`、`game/handler` `-race -count=3` 通过；`examples/configgen`、`examples/lubanreal` 运行正常（悬空 ref 被点名拒绝）。未跑 glsvet（没改三大模块）。

### 未完成 / 后续

- 发版时上调生成器 Core 下限（`minimumVersions.Core`、framework-compat minimum 行）；在此之前 CI 按 pin 跑的两个 runtime 门与 framework-compat 最低版本格会失败（预期）。
- cfggen globals 的规则（required / min / enum 走 `ObjectDef.Rules`）；tablegen 生成期的 ref 数据检查（现在只在加载时查）；tablegen 与 cfggen 两种标签方言合一（见 2.4）。
- 生成的 game-demo 没有运维 Rollback 入口，也没有会失败的 AfterApply 监听者：`stage=apply` 撤回与运维 Rollback 的日志 / 指标由单测覆盖（`TestEveryReloadReportsOneOutcome`、`TestFailedReloadAndRollbackAreCountedAndLogged`），未在真实进程里触发。
