# cfggen 配置 schema（meta 文件）参考

`cfggen` 是"meta 文件先行"的配置管线（简化版 Luban）：**一个 YAML 文件是唯一手写的配置定义**，Go 侧的行 struct、注册函数、类型化访问器全部生成；运行时（原子热更、回滚、内容 hash、请求一致性）由 roost-core 的 `configdata` 承担。

```bash
go run github.com/tjbdwanghaibo/roost-codegen/cmd/cfggen \
  -meta ./configs/schema/cfg.yaml -out ./cfg [-pkg cfg] [-groups s]
```

产物固定为 `<out>/cfg_gen.go` 一个文件。可运行的端到端示例：roost-core 仓库 `examples/configgen`。

## 一分钟上手

```yaml
# configs/schema/cfg.yaml
package: cfg
beans:
  - name: DropItem
    fields:
      - { name: item_id, type: int32 }
      - { name: weight,  type: int32 }
tables:
  - name: monster
    key: id
    comment: 怪物表
    fields:
      - { name: id,       type: int32 }
      - { name: name,     type: string }
      - { name: scene_id, type: int32, index: true }
      - { name: drop_id,  type: int32, ref: drop, required: true }
      - { name: rewards,  type: "[]DropItem" }
  - name: drop
    key: id
    fields:
      - { name: id,    type: int32 }
      - { name: items, type: "[]DropItem" }
globals:
  - name: world
    fields:
      - { name: width,  type: int32 }
      - { name: height, type: int32 }
```

业务侧接线一行、读取全类型化：

```go
cfg.MustRegisterGeneratedConfigData(configdata.DefaultRegistry()) // 装配期一次

snap := configdata.ActiveSnapshot()          // handler 内：同请求恒读同一快照
monsters, _ := cfg.MonsterTableFrom(snap)    // 主键表
wolf, ok := monsters.Get(1)
pack := cfg.MonsterBySceneID(snap, 7)        // 二级索引：强类型访问器
world, _ := cfg.WorldFrom(snap)              // 全局单例
```

## 顶层结构

| 键 | 必填 | 说明 |
| --- | --- | --- |
| `package` | 否 | 生成包名；`-pkg` 参数优先，两者都缺省时取输出目录名 |
| `groups` | 否 | 导出分组声明：`names`（允许出现的组名）、`target`（本次生成导出的组，`-groups` 参数优先）。见下方"导出分组" |
| `beans` | 否 | 嵌套结构定义（表/全局的字段可以用 bean 或 `[]bean`） |
| `tables` | 否 | 主键表：数据文件是**行数组**，按 `key` 建映射 |
| `globals` | 否 | **无主键的全局单例配置**：数据文件是**单个 JSON 对象**，整个快照只有一份（世界尺寸、全局开关、公式常数）。`objects` 是兼容别名 |

### 表 / 全局条目

| 键 | 适用 | 说明 |
| --- | --- | --- |
| `name` | 都 | snake_case，必须是合法 Go 标识符字符集；决定表名、生成类型名与数据文件默认名 |
| `key` | 仅 tables | 主键字段名；字段类型必须是整数或字符串。globals 写 key 会报错 |
| `file` | 都 | 数据文件名，默认 `<name>.json`；拒绝绝对路径与 `..` |
| `comment` | 都 | 生成到类型注释；多行会被折叠为一行 |
| `fields` | 都 | 字段列表（有序，决定 struct 字段顺序） |
| `group` | 都 | 整个条目只属于这些组（一个名字或列表）；不在目标组里的表 / 全局不生成、不注册、无访问器。省略 = 所有组 |

### 字段

| 键 | 说明 |
| --- | --- |
| `name` | snake_case，合法 Go 标识符字符集；也是 JSON 数据里的键名 |
| `type` | 见下方类型系统 |
| `index` | `true` 建以字段名命名的二级索引；写字符串则用该名字（须是合法标识符）。**`false`/省略 = 不建索引**；其他类型报错 |
| `skipempty` | 配合 `index`：零值行不进索引（`scene_id=0` 表示"不限场景"这类语义时避免零值桶爆炸） |
| `ref` | 引用另一张表的主键：字段类型必须与目标表 key 类型**完全一致**；目标表必须在本 meta 中声明。运行时每次 load/reload 校验非零值存在于目标表（零值 = 无引用） |
| `required` | 配合 `ref`：零值也报错——专门抓"数据侧字段改名导致整列静默归零"的事故 |
| `comment` | 生成到字段行注释；多行折叠 |
| `group` | 字段只属于这些组（`group: c` 或 `group: [c, s]`）；不在目标组里的字段从 struct 中去掉（连带它的索引访问器）。省略 = 所有组 |

`index`/`ref`（及其修饰）**只允许在 tables 上**；globals 写了直接报错（对象注册路径不会执行这些校验，静默忽略比报错危险得多）。

## 导出分组（前后端分开的配置）

一份 meta 同时描述客户端与服务器要的字段，服务器绑定只生成自己那部分——与 Luban 的 `group` 属性 / target 的 `groups` 一致。

```yaml
groups:
  names: [c, s]       # 允许出现的组名（拼错直接报错）
  target: [s]         # 这份 Go 绑定默认导出的组；-groups c,s 可以覆盖
tables:
  - name: monster
    key: id
    fields:
      - { name: id,       type: int32 }                     # 没写 group = 每个组都有
      - { name: hp,       type: int32,  group: s }          # 只给服务器
      - { name: model,    type: string, group: c }          # 只给客户端：服务器绑定里没有这个字段
      - { name: skin_id,  type: int32,  group: c, index: true }  # 连索引访问器一起省掉
  - name: ui_layout
    key: id
    group: c                                                # 整张表不进服务器
    fields: [ { name: id, type: int32 }, { name: path, type: string } ]
```

规则：

- `groups.target` 为空且没传 `-groups` = 导出一切，所以没有分组的旧 meta 生成结果一个字节都不变。
- 组名必须先在 `groups.names` 里声明；条目和字段的 `group`、`target`、`-groups` 里出现未声明的名字都报错。
- 主键字段不能被排除；`ref` 指向的表必须也在目标组里（否则绑定引用了运行时永不注册的表，每次加载都会失败）；一个 bean / 全局的字段全被排除也报错。
- 生成结束会打印"omitted N entries and M fields"，让"少了一个字段"是看得见的，不是静默的。
- **数据文件**：服务器加载的 JSON 里仍然会有客户端字段。默认（非 strict）模式下未知键被忽略，没问题；如果开了 `store.SetStrictJSON(true)`，导出数据的一侧也要按组过滤（Luban 的数据导出天生按 target 过滤；自写导出脚本要照做）。

## 类型系统

- 标量：`int32` `int64` `uint32` `uint64` `float32` `float64` `string` `bool`
- 标量数组：`"[]int32"`、`"[]string"` …（YAML 里要加引号）
- bean：直接写 bean 名（如 `DropItem`），或 `"[]DropItem"`
- key 只能是整数或字符串；index 只能是字符串/整数/bool（float 拒绝）；ref 只能是整数或字符串标量

bean 内部只能用标量/数组/其他 bean，**不能带 `index`/`ref`**；bean 沿非切片字段成环会被拒绝（`children: "[]Node"` 的切片自引用合法）。

## 命名映射（meta → 生成代码）

| meta | 生成 |
| --- | --- |
| 表/全局 `monster` | 行类型 `MonsterCfg`、访问器 `MonsterTableFrom` / `WorldFrom` |
| 字段 `scene_id` | struct 字段 `SceneID`（`id` 词固定大写为 `ID`），json tag 保持原名 |
| 索引 `scene_id`（或 `index: camp`） | 访问器 `MonsterBySceneID(snap, sceneID int32) []MonsterCfg` / `MonsterByCamp(...)` |
| 字段名撞 Go 关键字/保留名（`type`/`range`/`table`/`snap`…） | 参数名自动加后缀：`typeArg`——字段名照常可用 |
| bean 名 | 原样作为 Go 类型名（因此必须是导出形（大写开头）、非关键字、不遮蔽预声明标识符） |

所有生成的顶层标识符（类型名、访问器名、固定函数名）在生成期做**全量冲突检查**：`my_table` 与 `myTable`、表 `item` 与全局 `item_table`（派生名同为 `ItemTableFrom`）、bean 撞行类型名等都会在生成期报错，而不是产出编译不过的代码。

## 生成产物

`cfg_gen.go` 包含三部分（`// Code generated ... DO NOT EDIT.`）：

1. **行/bean/全局 struct**：带 `json` tag 与 `cfg` tag（`key` / `index[=名][,skipempty]` / `ref=表[,required]`），运行时校验全部由 tag 驱动；
2. **`RegisterGeneratedConfigData(r *configdata.Registry) error`** 与 Must 变体：逐表 `RegisterAutoTable`、逐全局 `RegisterObject`；
3. **类型化访问器**：`XxxTableFrom(snap)`、`XxxFrom(snap)`、每个索引一个 `XxxByYyy(snap, 值)`（字符串化规则与运行时索引严格一致）。

## 两层校验：什么时候拦住什么

**生成期（cfggen，schema 错误当场拒绝）**：未知 YAML 键（拼写错误）、名字字符集/重名/PascalCase 碰撞、key 未声明或类型非法、ref 目标表不存在或类型与其 key 不一致、index/ref 打在非法类型上、globals 带 key/index/ref、bean 非切片递归、file 路径逃逸。

**加载期（configdata，每次 load/reload 对真实数据执行）**：ref 目标表存在性与类型兼容（表级前置，空表也拦）、每行非零 ref 值的成员校验、`required` 的零值拒绝、JSON 为 `null`/空文档拒绝、`rows`/`records`/`data` 多包装键并存拒绝。可选 `store.SetStrictJSON(true)` 拒绝数据里的未知字段（防字段改名静默归零）。校验失败 = 整次 reload 被拒，**旧快照保持生效**。

## 数据文件格式

- tables：行数组 `[{...},{...}]`，或包装形式 `{"rows":[...]}`（`records`/`data` 亦可，但只能出现一个）；
- globals：单个 JSON 对象 `{...}`；
- 文件放在 configdata Store 的数据目录（kit Mod 的 `config_data.dir`，默认 `configs/data`）。

## 改表流程与 CI 建议

- 只改数据：改 JSON → 热更（GM `config.reload` / `Store.Reload`），不需要重新生成；
- 改结构（加字段/加表/改索引）：改 meta → 跑 cfggen → 提交 `cfg_gen.go` → 改数据 → 热更。
- CI 门禁建议：流水线里重跑 cfggen 后 `git diff --exit-code <out>/cfg_gen.go`，防止 meta 与产物脱节。

## 与 Luban 的关系

cfggen 覆盖"JSON 数据 + 轻量 schema"的场景。`groups` / `group` 对应 Luban 的 `groups` 声明与字段 / 表的 `group` 属性，语义一致（不写 = 属于所有组，target 决定导出哪些），迁到 Luban 时可以逐字搬。需要 Excel 族数据源、bean 继承/多态、path/range 校验时直接用 Luban：Luban 管定义/校验/导出/代码生成，其生成的 `Tables` 聚合经 `configdata.RegisterExternalTables` 装进同一套运行时（热更/回滚/hash 全继承，内容经字节指纹进 hash）。真实接入示例见 roost-core `examples/lubanreal`。

## 易错点速查

- `index: false` 就是不建索引（写 `false` 不会被当成开启）。
- 引用可选时用 `ref`（零值 = 无引用）；引用必填时加 `required`。
- 零值有业务含义的索引字段（如 `scene_id=0` = 不限）配 `skipempty`，否则零值全挤进 `"0"` 桶。
- 表名/全局名各自派生类型与访问器，起名时避开 `xxx_table`/`xxx_from` 这类会与派生名撞车的形态（撞了会在生成期报错，不会静默）。
- handler 内取快照一次（`snap := configdata.ActiveSnapshot()`）后传参使用；fan-out 的子 goroutine 不继承请求快照，必须显式传 `snap`。
