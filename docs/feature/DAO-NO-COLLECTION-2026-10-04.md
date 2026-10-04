# DAO 无集合声明（`//roost:dao nocoll`）

日期：2026-10-04。基线 main `9bf20e28`（v1.19.2 之后）。来源：[WANTED W-2026-09-18-09](../bug/WANTED.md)，
维护者 2026-10-04 选 **A**：保留“全 `nopersist,sync` 的 DAO”作为无持久化实体（刷出来的怪）的位置权威，
同时让 `//roost:dao` 能声明“没有集合”，这类 DAO 不再编造 Mongo 集合名，生成器也不再产出一条永远不会写任何东西的持久化路径。

## 1. 现状与问题

- `demo/db/def/monster.go` 为了能生成，只好写 `//roost:dao coll=monsters db=game`；四个字段全是 `dao:"nopersist,sync"`。
- 生成物 `gen_monster_dao.go` 因此带着 `MonsterDaoDBName = "game"`、`MonsterDaoCollection = "monsters"`、`SchemaVersion`、
  `Migrate`、`PrepareMutation` / `AcceptMutation`、`Marshal*` / `Unmarshal`、`RestorePersisted` 与字段级 Patch——
  `monster` 实体是 `noPersist=true`，`nest.MarkPersist` 一次也不会被调用（没有持久字段），这些代码只是死路径，
  而 `monsters` 这个名字会被读代码的人当成“真有这张表”。
- 解析器对 `coll=` / `db=` 是硬要求：`//roost:dao on X requires coll= and db=`，没有别的写法。

## 2. 语法

```go
//roost:dao nocoll
type MonsterDao struct {
	PosX int64 `bson:"pos_x" dao:"nopersist,sync"`
	...
}
```

选独立的裸标志 `nocoll`，理由：

- **不会被当成一个集合名读**。`coll=-` 读起来仍是“集合叫 `-`”，而且在本改动之前 `coll=-` 是**合法输入**（会生成一张叫 `-` 的集合），
  复用它等于悄悄改变一个旧写法的含义；`-` 一个字符在 review 里也容易漏看。
- **好搜、好 review**：`rg 'roost:dao nocoll'` 一次列出全部内存 DAO。
- **不选 `memory`**：仓库里“内存”已经有 `durability=memory`（Nest handler）、`lifetime`、Redis DAO 等多个含义，`nocoll` 只说一件事——没有集合。
- **拼错是响的**：写成 `nocol` / `no-coll` 而又没有 `coll=` / `db=` 时，原有的 “requires coll= and db=” 错误照常触发
  （错误信息补充提示 `nocoll`）；写成 `nocoll=true` 等带值形式直接报错，要求写裸标志。

与旧写法共存：`coll=` / `db=` / `dbscope=` / `schema=` 的解析逐字不变；没有 `nocoll` 的定义，解析结果与生成物**字节不变**
（`hero` / `variety` golden 不变即为证）。其它未知裸词保持原来的“忽略”行为，不借这次收紧。

## 3. 语义与校验规则

| 编号 | 规则 | 报错位置 | 报错要点 |
| --- | --- | --- | --- |
| R1 | `nocoll` 不能与 `coll=`、`db=`、`dbscope=`、`schema=` 同时出现 | DAO 解析 | 点名冲突的键：这些键描述的是一份它永远不会写的存储 |
| R2 | `nocoll` 是裸标志，`nocoll=<值>` 拒绝 | DAO 解析 | 要求写 `//roost:dao nocoll` |
| R3 | 无集合 DAO 的**全部字段**（`dao:"-"` 排除的除外）必须 `nopersist` | DAO 生成 | 点名全部持久字段；提示没有 dao tag 的字段默认 `persist,sync`；修法二选一：标 `nopersist,…` 或给 DAO `coll=`/`db=` |
| R4 | 使用无集合 DAO 的实体必须 `noPersist=true`，且不能 `remote=managed` | Entity 生成 | 点名实体、字段、DAO 类型；持久实体会在加载 / 删除时去找一张不存在的集合 |
| R5 | 没有 `coll=`/`db=` 又没有 `nocoll` | DAO 解析（原有） | 原文保留，追加“或用 nocoll 声明无集合 DAO” |

R4 的判定方式沿用 RR-20260926-45 的先例（`validateRemoteDaoScopes`）：Entity 生成器读实体所在模块内 DAO 包的**生成源码**，
看到 `<Dao>RegistryKey` 常量即认定为无集合 DAO。`roost generate` 里 dao 生成器先于 entity 运行，读到的是本轮产物。
DAO 包在模块外、源码不可读时无法在生成期判定：Entity 生成器按旧形状引用 `<Dao>Collection`，无集合 DAO 没有这个常量，
`go build` 直接失败（`undefined: db.XxxDaoCollection`）——仍在运行前失败，只是信息不如 R4 直接。

## 4. 生成物差异

无集合 DAO 选**不生成**持久化方法（而不是生成运行期报错的桩）：没有持久字段就不会有 `nest.MarkPersist`，
框架没有任何一条路径会对它调用这些方法；把它们删掉，任何“需要持久化能力”的使用方（持久实体的接线、
`remote=managed` 的 `MarshalPersist` 调用、手写代码）在**编译期**就失败，比运行期桩更早、更不可能被吞。

| 生成物 | 有集合 DAO（不变） | 无集合 DAO |
| --- | --- | --- |
| 常量 | `<Dao>DBName`、`<Dao>Collection`、`<Dao>SchemaVersion` | 只有 `<Dao>RegistryKey = "<Dao>"`（DaoManager 登记键，值是类型名，不是集合名） |
| `entity.DaoInterface` | `DbName()`/`CollName()` 返回库名 / 集合名 | `DbName()` 返回 `""`；`CollName()` 返回 `<Dao>RegistryKey`——接口是框架的，不改；它在这里只是实体内 DAO 的登记键 |
| `DbScope`、`SchemaVersion()`、`Migrate` | 有 | 无 |
| `nest.MutationParticipant`（`PrepareMutation`/`AcceptMutation`）及 `var _` 断言 | 有 | 无 |
| `Marshal`/`MarshalPersist`/`marshalPersistData`/`marshalCommitState`/字段级 Patch | 有 | 无 |
| `Unmarshal`/`RestorePersisted`（`entity.PersistedDaoLoader`） | 有 | 无 |
| 字段 mask、`mark*Dirty`（只剩 `MarkSync`）、`Init`、Get/Set/Add/Del/Range/Len、undo、`CaptureRollbackState`/`RestoreRollbackState` | 有 | **有**（Nest 内存回滚仍要用） |
| `<Dao>SyncFields`/`SyncFields()`/`MarshalSync(mask)`/`ApplySync` | 有 | **有**（packer 形状不变） |
| import | 含 `migration`、`sort` | 不含 |
| 类型注释 | `is the DAO for collection "x"` | `has no collection: …never stored…` |

Entity 生成物：无集合 DAO 在 `param.Dao[...]`、`DaoManager.Set(...)`、`MapDAOSyncChanges(...)` 里用
`<Dao>RegistryKey` 取代 `<Dao>Collection`；有集合 DAO 的接线字节不变。`entity.BuildEntity` 以 `dao.CollName()` 作键建 `param.Dao`，
与 `<Dao>RegistryKey` 一致，所以新建路径无需改框架。

## 5. 兼容性与迁移

- 旧定义一律不受影响。把一个 DAO 从有集合改为无集合：改 marker 为 `nocoll`、确认字段全 `nopersist`、确认使用它的实体
  `noPersist=true`，然后 `roost generate`。DAO 生成文件名不变（`gen_<name>_dao.go`），按内容哈希重写，持久化方法随之消失；
  Entity 接线文件同轮重写为 `RegistryKey`。DAO 生成器没有按集合分出的其它产物，`removeOrphanGenerated` 的规则不变
  （只删带生成头、命名匹配、本轮未产出的文件）；`roost generate --check` 比较的是同一组文件的内容。
- 业务代码若引用过 `db.XxxDaoCollection` / `XxxDaoDBName` / `XxxDaoSchemaVersion` 或调用过 `Marshal`/`Unmarshal`，会在编译期指出——
  这些引用对一个从不存储的 DAO 本来就没有意义。`db/migrations` 里若为该集合注册过迁移步骤，应删除。
- Mongo 里如果曾经真的写出过 `monsters` 集合（本 demo 不会：没有持久字段），由部署方自行清理；生成器不碰数据。
- 反向（无集合改回有集合）就是普通的新 DAO：加 `coll=`/`db=`，再按需改字段意图。

## 6. 未采用方案

- **`coll=-`**：见 §2；会改变一个旧的合法写法的含义。
- **`memory` / `storage=none`**：`memory` 含义过载；`storage=` 是一个只有两个取值的枚举，提前做成扩展点没有第二个使用方。
- **生成运行期报错的桩**（保留 `PrepareMutation` 等，返回 `ErrNoCollection`）：桩让持久实体误用一路编译通过，要到第一次加载 / 删除才炸；
  而且 Entity 接线仍需要一个常量名。删掉方法让编译器做这件事。
- **保留 `<Dao>Collection` 常量但改值**（如 `"MonsterDao"`）：Entity 生成器无需改，但名字仍在说“集合”，违背本决定的出发点。
- **把位置放进组件普通字段（W 条目的另一种形状）**：维护者已否决——会出现两种位置权威。
- **改 `entity.DaoInterface` / DaoManager 的键语义**：超出范围（不改 Entity 持久化框架本身）。

## 7. 实施结果

已实施（worktree 提交，未推送）：方案 `9630f0f5`，实现 `12f855a7`，demo `f3f6e947`。本地 Go 1.27.0 darwin/arm64。

| 位置 | 改动 |
| --- | --- |
| `codegen/internal/dao/parse.go` | `DaoDef.NoCollection`；`parseFlags` 收集裸词；`noCollectionParam` 实现 R1/R2；R5 错误文本追加 nocoll 提示 |
| `codegen/internal/dao/gen.go` | `validateNoCollectionFields`（R3）；模板函数 `daoKeyConst` → `<Dao>RegistryKey` |
| `codegen/internal/dao/template_dao.go` | 按 `.Dao.NoCollection` 分支：import、类型注释、`RegistryKey` 常量、`DbName`/`CollName`，跳过全部持久化方法；有集合分支字节不变 |
| `codegen/internal/entity/nocoll_dao.go` | `resolveNoCollectionDaos`（R4 + 标记 `DaoField.NoCollection`），`declaresConst` 读 DAO 包生成源码 |
| `codegen/internal/entity/remote_dao_scope.go` | 抽出 `daoSourceDir`（与 RR-20260926-45 的作用域检查共用 DAO 包定位） |
| `codegen/internal/entity/gen.go` / `main.go` / `parse.go` | 非 Remote 接线三处（`param.Dao[...]`、`DaoManager.Set`、`MapDAOSyncChanges`）改用 `daoKey`；生成前整包解析 |
| `codegen/internal/dao/testdata/def/wraith.go` + golden + `runtime/nocoll_test.go` | 无集合 fixture、逐字 golden、真实 roost-core 上的 daoruntime 门 |
| `demo/db/def/monster.go.tmpl` 等 | `//roost:dao nocoll`；monster 测试补无存储能力门；README / 步骤说明 / `roost help dao` / CODEGEN_REFERENCE §4.1 / USER_GUIDE §3 |

### 先红后绿

修前（基线 `9bf20e28`，新语法）：

```text
$ go run ./codegen/cmd/dao -def <scratch>/def -out <scratch>/out -pkg db   # def: //roost:dao nocoll，两个 nopersist,sync 字段
parse definitions: ghost.go: line 3: //roost:dao on GhostDao requires coll= and db=
exit status 1
```

`codegen/internal/dao/nocoll_promises_test.go` 修前（只加 `NoCollection` 字段让测试编译）：5 个用例全红，例如
`a nocoll DAO whose fields are all nopersist must generate: parse definitions: ghost.go: line 5: //roost:dao on GhostDao requires coll= and db=`、
`//roost:dao nocoll coll=ghosts db=game parsed`（nocoll 被静默忽略，生成为有集合 DAO）。
`codegen/internal/entity/nocoll_dao_promises_test.go` 修前：`wire is missing "param.Dao[db.GhostDaoRegistryKey]"`、
`wire still references the collection constant a nocoll DAO does not have`、`a stored entity with a nocoll DAO was generated`（持久 / remote=managed 两例）。

修后：

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go test -count=1 ./codegen/internal/dao/`（含 golden；`hero`/`variety` 等既有 golden 未改一字节） | ok |
| `GOWORK=off go test -count=1 ./codegen/internal/entity/` | ok |
| `codegen/scripts/dao-golden-runtime.sh` 等价流程（临时模块 `replace` 到本 worktree，含 `nocoll_test.go`） | vet ok；`TestANoCollectionDao{ReplicatesAndNeverPersists,RollsBackInMemory,HasNoStorageCapability}` PASS，全包 ok |
| `GOWORK=off go test -count=1 ./codegen/...` | 全部 ok |
| `GOWORK=off go vet ./codegen/...`、`gofmt -l`（含 testdata） | 干净 |
| `GOWORK=off go test -count=1 .`（含 `TestCoreDependencyBoundary`） | ok |
| `GOWORK=off go generate ./...` | 工作树无新增改动 |

### 生成工程（game-demo）

`roost project new planet -skip-deps -module example.com/planet -template game-demo`（scratchpad）+ `go mod edit -replace` 到本 worktree + `go mod tidy`：

- `GOWORK=off go build ./... && go vet ./... && go test ./...`：全绿；`game/entities/monster` 的
  `TestAnEphemeralMonsterReplicatesButNeverPersists`、`TestMonsterFarViewOmitsHP`、`TestTheMonsterDaoHasNoCollection` PASS。
- `db/gen_monster_dao.go` 只有 `MonsterDaoRegistryKey = "MonsterDao"`；`monster_gen_wire.go` 用 `db.MonsterDaoRegistryKey`。
  全工程 `grep '"monsters"\|coll=monsters\|MonsterDaoCollection'` 无命中；`grep -rn monsters --include=*.go` 只剩 spawner 的 map 字段名与注释。
- `roost generate --check`：`generated files are up to date`。
- 迁移演练：把 `db/def/monster.go` 改回 `coll=monsters db=game` 再 `roost generate`（两文件出现 `MonsterDaoCollection`/`RestorePersisted`，仍可编译）；
  再改回 `nocoll`：`--check` 报 `db/gen_monster_dao.go, game/entities/monster/monster_gen_wire.go` 过期，`roost generate` 原地重写二者，
  旧常量与持久化方法清零，`db/` 无残留文件，build / monster 测试 / `--check` 绿。
- 误用演练：去掉 Monster 的 `noPersist=true lifetime=ephemeral` 后 `roost generate` 失败：
  `entity Monster: DAO field dao (*db.MonsterDao) is declared //roost:dao nocoll and is never stored, but the entity is persistent; mark the entity noPersist=true, or give the DAO coll= and db= and regenerate`。

### 未验证 / 边界

- DAO 位于实体所在模块之外时 R4 无法生成期判定，靠 `undefined: …Collection` 编译失败兜底（§3）；手写 builder 把无集合 DAO 注册进持久实体时，
  只会在加载（`does not implement PersistedDaoLoader` / 空库名查询）或删除（`does not implement nest.MutationParticipant`）时失败，没有启动期检查——
  加启动期检查需要改 Entity 注册框架，超出本次范围。
- 未跑 Mongo / NATS 实跑（`make dev-run` + loadtest）；本改动不触及运行期框架，怪物原本就不写库。
- 未刷新 codebase-memory 索引（索引根是主仓 checkout，不是本 worktree）。
