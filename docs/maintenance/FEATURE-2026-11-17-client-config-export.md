# 配置表导出给客户端（client_export.config）：需求与维护者决定

**状态**：**已定，实施中**（维护者 2026-11-17：登录时不核对数据版本，但数据版本号要记在服务端仓库的导出目录里、有地方能看到；其余都按推荐）。实施完后，结论写进现行文档（`codegen/docs`、`docs/framework`、skill），本文留作来源。

## 1. 背景

- 使用方是两个仓库：Go 服务端用 roost-core v1.53.0 生成，Unity 客户端另成一仓。
- 协议已经有了 v1.53.0 的 `client_export`，导出到 `shared/protocol/`，再用 `roost client sync` 同步进客户端。这条链路在 Unity 6.7 上编译和测试都通过了。
- 配置数据还没有这样的链路，同一份数值有两份：
  - 客户端有大约 40 张 CSV，由自己的代码读取；
  - 服务端把其中的数值手抄成 Go 常量；
  - 策划改了表，服务端要手动跟改，没有任何检查能发现两边不一致。
- 目标：服务端工程是配置的唯一源头。一次 `roost generate` 同时产出服务端的 JSON 和 Go 绑定（现有能力），以及客户端的 JSON 和 C# 读取代码。客户端用 `roost client sync` 同步过去，直接编译，直接读取。

## 2. 现状（2026-11-17 核对代码）

- **`//roost:table`**（`codegen/internal/tablegen`）：
  - 标在 `configs/schema/*.go` 的结构体上，数据源是 `configs/table/<表>.csv`。
  - **只有第 1 行列名是必须的**；标题、类型、规则三行有就跳过，没有也行（`csvDataStart`）。
  - 字段类型来自 Go 结构体，不从 CSV 推断。
  - 产出：`configs/data/<表>.json`、`_manifest.json`、`configs/generated/gen_table_config.go`。
- **`//roost:object`**：已有的单例对象。单行宽表，一列一个字段，对应 cfggen 的 globals。
- **没有的**：
  - 字段和表的按端分组：只有 cfggen 的 `group` 有；
  - `index` 二级查询：只有 cfggen 有；
  - `#` 注释行；
  - `key,value,note` 竖排写法；
  - 客户端产物。
- 用自定义解析器（`Parser`）的字段，JSON 里存的就是原始字符串。

## 3. 维护者的决定（2026-11-17）

1. **按端分组**：
   - 字段用 struct tag `side:"c"`（只给客户端）或 `side:"s"`（只给服务端），不写表示两端都要。
   - 整张表在标记里写 `side=c`，只给客户端，服务端不登记。
   - 规则沿用 cfggen 的 `group`：
     - 主键不能被排除；
     - `ref` 指向的表必须也在同一端；
     - 一张表的字段全被排除时报错；
     - 生成时打印 “omitted N tables / M fields”。
   - 服务端的 `configs/data/*.json` 只含服务端字段；客户端导出只含客户端字段，只给服务端的字段绝不出现在导出目录里。
   - **只给客户端的字段在服务端的 schema 结构体里仍然存在，值是零值**。不另生成一份服务端结构体，在文档里写明这一点。
2. **竖排键值表**：沿用 `//roost:object`，加选项 `layout=kv`。
   - 字段和类型由 Go 结构体定义。
   - CSV 竖排，三列 `key,value,note`，一行对应一个字段；`note` 列成为生成代码里的注释。
   - 生成的 JSON 与普通 object 相同，都是单个对象。
   - 规则：key 必须对上字段名；必填字段不能缺；不允许重复 key 或未知 key。
3. **C# 读取**：
   - 生成代码自带一个小的 JSON 解析器，按字段强类型读取。不依赖 `JsonUtility`，不调用 UnityEngine，也不用反射，所以 IL2CPP 裁剪是安全的，不需要 `link.xml`。
   - JSON 形状照需求定：普通表是 `{"rows":[...]}`，键值表和 object 是单个对象。想用 `JsonUtility` 的游戏也能直接读。
   - 计算哈希前先把 CRLF 统一成 LF，防止 Windows 上 git 自动转换换行后对不上。
4. **数据版本号：记录，但登录时不核对**（维护者 2026-11-17 两次答复）：
   - `data_version` = 导出给客户端的全部表的数据哈希（按 LF）按表名排序后合起来再取 sha256，显示时用前 12 位十六进制。用内容哈希，不用递增数字：生成过程没有状态，同样的输入得到同样的版本号，保持逐字节可重复。
   - **记在哪里**：服务端仓库的导出目录 `shared/config/manifest.json`（提交进仓库）；生成的 C# 里有常量 `ConfigTables.DataVersion`，客户端可以显示或上报。
   - **在哪里看**：`roost generate` 和 doctor 的 `config-export:fresh` 打印当前版本号；`roost client sync` 打印“旧版本 → 新版本”，并存进同步记录。
   - **不做**：进入游戏的回应里不带版本号，SDK 不做比较，热更也不提示“客户端数据已过期”。两端的数据各自处理。
5. **index**：`//roost:table` 的字段加 `index` 选项，写法参照 cfggen：`index:"true"` 或 `index:"<名字>"`。服务端和客户端都生成二级查询。
6. **CSV**：
   - 支持 `#` 开头的注释行，读取时忽略；
   - 表头格式保持现状：列名一行必须有，标题、类型、规则三行可选；
   - 迁移说明里写清楚。
7. **同步**：并进现有的 `roost client sync`，新增两个参数 `--config-code-to <目录>` 和 `--config-data-to <目录>`。导出目录里有配置部分时才处理，规则与协议同步相同：
   - 先核对哈希；
   - 上次写过、这次没有的文件删掉；
   - 不是上次同步写的同名文件，拒绝覆盖；
   - 不生成 `.meta`；
   - 对比上一次的 manifest，报告表的增删、字段的增删和改类型、行数变化。

## 4. 导出约定

`roost.yaml`：

```yaml
client_export:
  dir: shared/protocol
  languages: [csharp]
  config:                        # 不写就不导出配置；没声明的工程，生成结果逐字节不变
    dir: shared/config           # 缺省 shared/config；相对工程根，提交进仓库
    tables: []                   # 可选；空表示全部带客户端字段的表
    csharp:
      namespace: Game.Config     # 可选，缺省 Roost.Config
```

- 只开 `config`、不需要协议导出的工程也要能用。具体写法由实施时定，选最简单的，并写进文档。
- 导出目录不能放在 `configs/`、`game/` 等服务端源码目录下。
- 配置导出不需要 protoc。
- 同样的输入，导出逐字节相同。

| 文件 | 内容 |
| --- | --- |
| `data/<表>.json` | 只含客户端字段。普通表是 `{"rows":[...]}`，键值表和 object 是单个对象。字段名用 schema 里的 JSON 原名 |
| `csharp/ConfigTables.cs`（必要时再拆成几个文件） | 每张表：一个 `[Serializable]` 行类型（public 字段，名字与 JSON 键一致），一个表类型（`Get(key)`、`TryGet`、`All`，以及按 index 生成的二级查询）。每个 object 或键值表：一个单例类型。另有一个读取入口 |
| `manifest.json` | `kind: roost-config-export`、schema、core 和生成器的版本、`data_version`（决定 4）、每张表的 schema 哈希、数据哈希（按 LF）和行数、每个导出文件的哈希 |

C# 读取入口：
- `ConfigTables Load(Func<string, string> readText)`：读文件的方式交给调用方，可以用 Resources、Addressables 或 StreamingAssets。只用 netstandard2.1 的纯 C#。
- 加载时核对每张表的数据哈希与内置的 manifest 一致，不一致就抛出带表名的异常。
- 加载时做 `required`、`unique`、`min`、`enum`、`ref` 检查，规则与服务端一致，报错时写明表、行和字段。
- 不留可变的静态状态。加载结果是一个实例，由调用方持有。

检查：
- `roost generate --check` 也覆盖 `shared/config`；
- `roost project doctor` 新增 `config-export:fresh`。它不重新生成，只核对导出内容与 `configs/table`、schema 的哈希一致。

## 5. skill（roost-genre 的 compose 写法）

- 生成玩法时，数值（奖励、曲线、上限、倍率、掉落）默认写进 `configs/table`，不写成 Go 常量；只有协议约束才写成常量。
- 需求拆分时列出哪些表两端共用、哪些字段只给服务端，写进 DEMO 的边界表。
- 已有工程的迁移步骤：
  1. `roost add table` 建表；
  2. 把常量换成 `<表>From(snap)` 读取；
  3. 场景测试改成从测试数据读；
  4. 打开 `client_export.config`；
  5. 客户端同步之后，删掉客户端原来的 CSV。

## 6. 验收

- 一个 game-demo 工程，包含：
  - 两张普通表，其中一张带只给服务端的字段和一个 index；
  - 一张竖排键值表；
  - 一张只给客户端的表。

  `roost generate` 后，`shared/config` 要符合第 4 节的约定。改了 CSV 但不重新生成时，`generate --check` 和 doctor 都要报过期。
- C#：在 netstandard2.1 工程里编译导出的代码，`Load` 能读回全部数据；改动任意一份 JSON，`Load` 报出表名；违反 `required` 或 `ref` 时，报出具体的行和字段。
- `roost client sync` 的三个场景：首次同步、增量同步、手改的文件被拒绝覆盖。
- 没声明 `client_export.config` 的工程，生成结果逐字节不变。

## 7. 非目标

- 不做 Excel 数据源；需要的话走 Luban 加 `RegisterExternalTables`。
- 不做运行时下发配置，也不做客户端热更配置。
- 登录时不核对数据版本，也不做运行时比较（维护者 2026-11-17）；版本号只记录、可查看，见决定 4。
- 不生成 Unity 专用代码（ScriptableObject、Addressables 接线）。
- 自定义解析器的字段在客户端只能拿到原始字符串，由游戏自己解析。这一点写进限制。

## 8. 实施分组

1. **tablegen 组**：按端分组、`layout=kv`、`index`、`#` 注释，只改 `codegen/internal/tablegen`、`codegen/internal/marker`，以及相关的 configdata 规则。
   - 对外交出一份“导出用的表模型”：哪些表、哪些字段归客户端，index，键值表的元数据，以及按 LF 计算的数据哈希。
2. **导出组**：等 tablegen 组和 deploy 组都交差后再开。负责：
   - `client_export.config` 的配置与校验、C# 生成、manifest；
   - `roost client sync` 的配置部分、`generate --check`、doctor 的 `config-export:fresh`；
   - netstandard2.1 编译验收。
3. **skill 与文档**：由协调者在最后统一写。
