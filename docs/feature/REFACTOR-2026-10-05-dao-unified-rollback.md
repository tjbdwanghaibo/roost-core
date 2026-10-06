# 回滚统一走 DAO（维护者决定 A1）

2026-10-05，分支 `a1dao`，基线 `d346b45c`。来由：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) A1，维护者原话“回滚都使用 dao 的实现方式，这样回滚都可以统一了”。取代原推荐（组件各自登记 undo / Checkpoint / rebuild）。

**目标**：事务内会改、回滚时要恢复的状态一律放在 DAO（必要时用非持久字段），由 Nest 现有的 DAO 回滚（`RollbackUndo` 的生成 setter 逆操作、`RollbackState` 的 DAO 快照）统一兜住。组件不持有需要回滚的内存状态，只读写 DAO，不再自己登记 undo。

**为什么**：同一缺口已出过四次——NC-61（属性层只在组件内存）、NC-65（加载不重建 Gear/Final）、NC-140（World 定时器堆）、N09 O1（skill Runtime 状态不在事务里）。前三次都是组件自己调 `CurrentRollbackTx().RecordUndo(...)` 补，每个组件各写一套快照、各自决定在哪里调用，漏一处就是资产错。

代码定位：codebase-memory 项目 `Users-whb-roost-roost-core` 的共享 generation 停在 09-30、根路径是主检出，且 `codegen/internal/dao/testdata` 不在索引内；本方案的盘点与结论以 worktree 当前源码（`rg` + 直接读）为准。

## 1. 盘点

全仓（core、kit、codegen 模板、demo 模板）搜 `RecordUndo` / `RecordUndoToken` / `CurrentRollbackTx` / `DeferRollback` / `RollbackParticipant` / `captureRollback`，以及所有实现组件（`entity.ComponentBase` / `RegisterComponentFactory`）的类型。

| 位置 | 事务内会改的内存状态 | 现状（怎么回滚） | 目标 |
| --- | --- | --- | --- |
| `demo/.../player/attribute_component.go.tmpl` `AttributeComponent.container` | Base / Gear / Final 三层 profile | `captureRollback`：复制全部层，`tx.RecordUndo(component, …)`（undo 与 state 两种策略都登记）；加载时 `OnInitFinish` 自己重建（NC-65） | 组件无状态。Base → `attr_base`（已有 `persist,sync`）；Gear → 新增 `attr_gear`（`nopersist,nosync`）；Final → `attr_final`（已有 `nopersist,sync`）。`Container()` / `Final()` 每次从 DAO 构造；删除 `captureRollback` |
| `demo/.../world/timer_component.go.tmpl` `TimerComponent.scheduler` | `timer.Scheduler` 的堆、按 id 索引、种子 | `captureRollback`：复制 `Nodes()` / `Seed()`，undo 里 `rebuild`；加载时 `OnInitFinish` 重建 | 组件无状态。节点在 `timers`（已有 `persist`）、种子在 `timer_seed`（已有 `persist`）；新增 `timer_next_due`（`nopersist,nosync`）让空转 Tick O(1) 判断。武装 / 触发时从 DAO 建一个只活一次调用的调度器，经变更钩子写穿 DAO 后丢弃；删除 `captureRollback` 与常驻堆 |
| `skill/combatcomponent` `CombatDao` | vitals / attributes / buffs | 状态已全部在手写 `CombatDao` 里，`RollbackState` 快照（`CaptureRollbackState`）已覆盖；`RollbackUndo` 的逆操作由**组件** `undoVitals / undoAttributes / undoBuffs` 调 `nest.RecordUndo(dao, …)` 登记 | undo 登记移进 `CombatDao`（与生成 DAO setter 同形：DAO 自己“记逆操作 → 改 → 标脏”）。组件只调 DAO 方法，不再出现 `RecordUndo` |
| `skill` `Runtime` | 冷却、ammo、cast、proc 账本、state mutation 流、revision | 不在任何事务里（N09 O1）；仓内没有正式调用方 | 本次不迁，列为后续，理由见 §4.4 |
| demo 其余组件（profile、bag、equipment、map、guild roster、monster body、world stats） | 无（只有 `owner`），全部读写 DAO | — | 不变 |
| `codegen/internal/roost/add_entity.go` 组件骨架 | 无 | 注释只说“持久或同步状态经 DAO” | 注释改写为本规范 |
| kit | 没有实体组件 | — | 不变 |
| core `attribute.Container`、`timer.Scheduler` | 基础数据结构，不依赖 nest | 由持有者决定是否回滚 | 不改：它们可以作为一次调用内的临时计算，不再被组件常驻持有 |
| 生成 DAO（`template_dao.go`、`template_nested.go`）、nest 内部、`saga` / `dataengine` 的 `CurrentRollbackTx` | DAO 字段 / 事务持久化意图（不是组件内存） | 本身就是统一机制 | 不变；README 的 `AddGold` 手写 DAO 示例是 DAO 方法自己登记，符合本规范 |

## 2. DAO 能力缺口

现状核对（`codegen/internal/dao/gen.go`、`template_dao.go`）：

- `dao:"nopersist,sync"` 字段：生成 setter（`RollbackUndo` 下登记逆操作）；`CaptureRollbackState` / `RestoreRollbackState` 覆盖**所有**字段（`RollbackState` 下随快照恢复）；`marshalCommitState` / `marshalPersistData` / patch / `Unmarshal` 只取 `persistFields`，不进 WAL、不进 Mongo；`MarshalSync` 只取 `syncFields`。即“参与回滚、不持久”已经支持。
- **缺口**：`dao:"nopersist,nosync"`（不持久、不同步，只参与回滚）字段**不生成 mutator**——`filterDirty` 只收 `persist || sync` 的字段，于是标量 / 嵌套 / 切片没有 `Set`/`Add`，map 连 `Get`/`Range`/`Len` 都没有。README 把它写成“字段仍在内存中”，实际只能经快照恢复，业务改不了。

最小扩展：`dirtyFields` 收所有非 `dao:"-"` 字段（函数改名 `mutableFields`）。`nopersist,nosync` 字段因此得到与其他字段同形的 mutator：`RollbackUndo` 下登记逆操作，`RollbackState` 下已在快照里；`mark…Dirty` 对它为空（不 `MarkPersist`、不 `MarkSync`），所以不进提交记录、不进 WAL / Mongo、不进同步。不新增标注：`nopersist,nosync` 本身就是“只参与事务”的语义，再加 `transient` 是同义词。只新增方法，不改已有签名和持久格式。

两种表达派生 / 非持久状态的方式比较：

| | A. 非持久 DAO 字段（选用） | B. 回滚后由持久字段重算 |
| --- | --- | --- |
| 回滚 | 生成 DAO 已有的 undo / 快照，零新机制 | 要一个回滚后钩子（Nest 在所有 DAO 逆操作执行完后调用组件），属 nest 改动 |
| 正确性 | 回滚恢复的是事务开始时的值本身 | 重算依赖的输入不一定都在 DAO：Gear 依赖物品配置，回滚时读到的配置代可能已不是写入时的；重算若要写 DAO（`attr_final`），回滚中的事务已关闭，生成 setter 登记 undo 会 panic |
| 顺序 | 无 | 钩子必须排在全部 DAO 逆操作之后（嵌套事务、动态 Cast、CreateInScope 新实体各有自己的捕获时机） |
| 成本 | 每次改派生值多一份 undo（map 每键一次） | 回滚时重算一次，成功路径零成本 |

选 A。B 的唯一优点是成功路径少记几条 undo，§6 的数据表明这部分成本可以忽略；B 引入 nest 钩子、回滚时读配置、回滚中写 DAO 三个新问题。另考虑过“生成 DAO 加恢复代计数、组件缓存惰性重建”：每个读入口都要先比对代数，漏一处就是陈旧读；缓存先改、写 DAO 失败的窗口仍会丢；目前两个组件都不需要常驻缓存，不做。将来确有昂贵缓存时再按此加。

## 3. 派生状态的统一触发点

派生值本身是 DAO 非持久字段，由组件里**唯一一个**写派生值的函数产生（属性：`derive()`，从 `attr_base` 与身上装备算出 `attr_gear` / `attr_final`；定时器：写 `timer_next_due`）。触发点：

- **加载**（`OnInitFinish`，新建与从存储构建共用）：非持久字段不随文档回来，必须算一次。只写 `nopersist` 字段：生成 setter 不 `MarkPersist`、无事务时不登记 undo，加载期不需要事务、不产生持久写。
- **业务变更**：改了源字段的同一事务里立即调用（`LevelUp`、`RefreshGear`、武装、触发），派生值与源字段同一笔 undo / 快照。
- **热更新**：配置决定的派生值（Gear 依赖物品表）由业务事务重算（`RefreshGear`），与业务变更同一入口；配置热更不自动扫在线玩家（观察 C-O8 不变）。
- **回滚**：**不是**触发点。派生值和源字段一起由 DAO 恢复到事务开始时的值，不需要重算。

这就消除了“加载一套、回滚一套、热更一套”：只有一个 derive，回滚不需要任何组件代码。

## 4. 迁移

### 4.1 attribute（`demo/db/def/player.go.tmpl`、`demo/game/entities/player/attribute_component.go.tmpl`）

- DAO 增 `AttrGear map[int32]int64 bson:"attr_gear" dao:"nopersist,nosync,map=fast"`。schema 不变（不持久的字段不进文档）。
- 组件删 `container` 字段与 `captureRollback`。`Container()` 返回从 DAO 构造的容器（Base / Gear / Final 三层的副本，改它不影响任何东西），`Final()` 从 `attr_final` 构造。
- Base 的来源：`attr_base` 有值用它，没有（从未写过的新玩家）用等级曲线 `ForLevel(level)`。`LevelUp(levels)` 在调用方已把等级加上之后调用（`ProfileComponent.AddExp`），没写过 `attr_base` 时从 `ForLevel(level-levels)` 起升级，与旧实现“加载时按当时等级装 Base、升级时在其上加”等价。
- 写入：`LevelUp` 写 `attr_base`、`attr_final`；`RefreshGear` 写 `attr_gear`、`attr_base`、`attr_final`（旧实现两者都写 base 与 final，持久内容不变）。

### 4.2 World 定时器（`demo/db/def/world.go.tmpl`、`demo/game/entities/world/timer_component.go.tmpl`）

- DAO 增 `TimerNextDue int64 bson:"timer_next_due" dao:"nopersist,nosync"`（最早到期的 UnixMilli，0 表示没有）。
- 组件删 `scheduler` 字段、`rebuild`、`captureRollback`。`Tick(now)`：`timer_next_due` 为 0 或未到 → 直接返回（空转 Tick 不扫描、不建堆）；到期 → 从 DAO 节点建调度器、触发、写穿、按调度器剩余最早到期写回 `timer_next_due`。`ScheduleActivityPhase` 同理。`PendingActivityPhase` 直接扫 DAO。
- 只支持带类型的定时器（闭包定时器不可存，本来就不在 DAO 里）。

### 4.3 combatcomponent（`skill/combatcomponent/component.go`）

`CombatDao` 增 `beginChange(mask)`（要求事务；每个字段每笔事务登记一次逆操作，`owner` 是 DAO 自己，与旧实现的 undo key 相同）与 `markChanged(mask)`；组件的 mutator 改为调这两个方法。行为不变，`nest.RecordUndo` 从组件里消失。

### 4.4 skill Runtime：列为后续

Runtime 是自带锁、调度器、进程表、trace / presentation / state mutation 投递缓冲的独立执行引擎，状态散在十几个 map 里，彼此有引用。放进 DAO 只有两条路：① 每笔事务把 `Checkpoint()` 写进一个非持久 DAO 字段、回滚后用 `RestoreRuntime` 重建——每笔事务一次 JSON + sha256 全量序列化，且 Runtime 的投递缓冲按设计不进 checkpoint，回滚后要重新对齐 Host；② 把 Runtime 的权威状态拆成 DAO 字段，等于重写 Runtime。仓内没有正式调用方，代价远大于本次范围。后续入口：接入方在 handler 里推进 Runtime 时，先按 ① 设计接入形态（谁持有 checkpoint 字段、Host 对齐、成本门槛），与 B4（文档约束）一起由维护者定。本次在 `docs/skill/skill-casting-and-combat.md` 写明现状约束。

## 5. 规范与检查

- `docs/agent-skills/roost-coding/SKILL.md` 执行契约（Nest 与 Entity）加一条：事务内会改的状态一律放在 DAO（必要时用 `nopersist` 字段），组件不得自行维护需要回滚的内存状态，不在组件里登记 undo。
- 生成器文档：`codegen/README.md` 的 tag 表与 undo 小节；生成工程文档（`render_docs.go`）的 DAO 说明；`add entity` 组件骨架注释；demo 两个 DAO 定义的字段注释。
- `cmd/glsvet` 加提示（不计入失败、不改退出码，与 A3“只做提示”一致）：在组件方法（接收者类型嵌入 `ComponentBase` 或名字以 `Component` 结尾）里直接调用 `RecordUndo` / `RecordUndoToken` / `DeferRollback` 时打印 `hint:`。（2026-10-06 [RR-20261006-13](../bugfix/RR-20261006-13.md) 起也跟进一层同包包级 helper 函数：组件方法调用直接登记 undo 的包级函数时，在调用处提示。）
- 2026-10-06 第十三轮“A1 盲区”（维护者选 A，[记录](A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)）：组件把可变状态放在普通字段里、也不登记 undo 时同样漏回滚，glsvet 加字段写提示——组件方法（`OnInitFinish` / `OnDestroy` 除外，同样跟进一层同包 helper）给组件自身字段赋值或改字段里的 map / slice 元素，而字段不是 DAO 句柄（类型名以 `Dao` / `DAO` 结尾）、不是函数类型、也没有 `//roost:cache` 标注（缓存类字段，写在字段上一行或行尾）时打印 `hint:`，同样不计入失败。

## 6. 兼容、Nest 衔接与性能

- **持久格式**：新增的两个字段都是 `nopersist`，不进文档、不进 WAL；已有字段不变；schema 版本不变。
- **生成 DAO**：`nopersist,nosync` 字段新增 mutator 方法（只增不改）。golden 随之更新。
- **已生成工程**：不迁移（维护者此前决定）；需要的话 `roost project sync` 取新模板。旧模板的 `captureRollback` 在新 core 上仍然可用（`RecordUndo` API 未删）。
- **Nest**：零改动。回滚仍是 `RollbackTx.Rollback()` 逆序执行已登记的逆操作 / 快照恢复，快慢双池、准入、提交、拒绝、结果未知的分支都不经过本次改动的代码；组件不再登记逆操作只是少了几条 `rollbacks` 项。
- **性能**：见 §8 的同机前后对照。

## 7. 验证计划

- 先红后绿：组合用例（属性：handler 失败 / 提交被拒 × undo / state；定时器：武装 / 触发 × 两种失败；combat：undo 失败）。红：旧实现把 `captureRollback` / 组件 `RecordUndo` 置空后运行；绿：新实现（没有任何组件 undo）。
- codegen：`nopersist,nosync` mutator 的生成断言与 daoruntime 运行（undo、state 快照、提交记录不含该字段、同步不含该字段）。
- 生成 game-demo（replace 到 worktree）：NC-61 / NC-65 / NC-140 回归、真实 WAL 写入后重放、确认非持久字段不进 WAL；`go build ./... && go vet ./...`、相关包 `-race -count=3`。
- core：`go test ./codegen/...`、`go generate ./...` 后 porcelain、glsvet、根包、`go build ./... && go vet ./...`。

## 8. 实施状态与验证结果

已实施（分支 `a1dao`，基线 `d346b45c`）。证据在 [evidence/dao-unified-rollback-20261005](evidence/dao-unified-rollback-20261005/)。

### 改动

| 文件 | 内容 |
| --- | --- |
| `codegen/internal/dao/gen.go`、`template_dao.go` | `dirtyFields` → `mutableFields`：所有字段都生成 mutator；`nopersist,nosync` 字段的 mark 函数为空 |
| `codegen/internal/dao/testdata/def/variety.go`、`golden/*` | fixture 加 `Pending map[int32]int64 dao:"nopersist,nosync,map=fast"`；golden 只多出 `SetNeither` / `SetScratch` / `Pending` 的 mutator 与 getter |
| `codegen/internal/dao/transient_field_promises_test.go`、`testdata/runtime/transient_test.go` | 生成断言（有 mutator，快照覆盖，存储 / 提交 / 同步函数不碰它）；daoruntime：undo 回滚、state 快照恢复、提交记录（Put 与 patch）与同步载荷不含、存储读回不带 |
| `demo/db/def/player.go.tmpl`、`world.go.tmpl` | 新增 `attr_gear`、`timer_next_due`（均 `nopersist,nosync`） |
| `demo/game/entities/player/attribute_component.go.tmpl` | 组件无状态：`Container()` / `Final()` 从 DAO 构造；`derive` = `writeGear` + `recompose`；删 `container`、`captureRollback`；写 map 时删掉降为 0 的属性、不重写未变的值 |
| `demo/game/entities/world/timer_component.go.tmpl` | 组件无状态：武装 / 触发时从 DAO 建调度器、写穿后丢弃；空转 Tick 只读 `timer_next_due`；删 `scheduler`、`rebuild`、`captureRollback` |
| `demo/.../attribute_component_test.go.tmpl`、`timer_component_test.go.tmpl` | 新增组合用例（真实 Nest 派发 × undo/state × handler 失败/提交被拒）、真实 WAL 写入 + 重放检查、提交记录检查；原用例里“先取 Gear 再断言它被原地改”改成重新读取（`Container()` 现在是副本） |
| `skill/combatcomponent/component.go`、`dao_rollback_promises_test.go` | undo 登记移进 `CombatDao.beginChange` / `markChanged`；新增两种策略 × 两条失败路径的组合用例 |
| `cmd/glsvet/main.go`、`main_test.go` | 组件方法里的 `RecordUndo` / `RecordUndoToken` / `DeferRollback` 打印 `hint:`，不计入 findings |
| 文档 | roost-coding 执行契约、`codegen/README.md`、生成工程文档（`render_docs.go`）、`add entity` 组件骨架注释、根 README、`skill/README.md`、`docs/skill/skill-casting-and-combat.md`（含 Runtime 现状约束）、TROUBLESHOOTING T-227 判别、bugfix 经验、NC-61 / NC-65 / NC-140 修复记录追加“后续” |

Nest 零改动（`git diff d346b45c -- nest` 为空）。

### 先红后绿

- 生成 game-demo（新用例 + 旧组件模板，replace 到 worktree）：旧组件**带**手写 undo 时新用例全部通过（用例有效）；把两个 `captureRollback` 置为立即返回后，`TestAttributeLayersRollBackWithTheTransaction` 2 叶、`TestAttributeRollbackIsTheDaoRollback` 4 叶、`TestTimerRollbackIsTheDaoRollback` 8 叶失败（[red-demo](evidence/dao-unified-rollback-20261005/red-demo.txt)，如 `base after rollback = &{HP:110 Attack:12 …}, want HP 100 attack 10`、`rolled back, but node 1 is still pending`、`the rolled-back tick took the deadline away`）。新模板（组件里没有任何 undo）全部通过（[green-demo](evidence/dao-unified-rollback-20261005/green-demo.txt)）。
- combatcomponent：把组件里的 `nest.RecordUndo` 换成空操作后，`TestNestUndoRollbackRestoresCombatStateExactly` 与新用例 undo 两叶失败（state 两叶通过：状态本来就在 DAO 快照里）（[red-combat](evidence/dao-unified-rollback-20261005/red-combat.txt)）；undo 移进 DAO 后全部通过。
- codegen：旧生成器下 `TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync` 6 条断言失败（`the generated DAO has no "func (d *VarietyDao) SetNeither(v int64)"` 等，[red-codegen](evidence/dao-unified-rollback-20261005/red-codegen.txt)）；存储 / 同步隔离的断言在旧生成器上已通过，说明缺的只是 mutator。
- glsvet：旧代码上 5 条提示（两个模板组件、combatcomponent 三个 undo 函数，[hints](evidence/dao-unified-rollback-20261005/glsvet-hints-before.txt)），新代码全仓 0 条，退出码均为 0。

### 验证（`GOWORK=off`）

- core：`gofmt -l` 空；`go test -race -count=3 ./cmd/glsvet ./codegen/internal/dao ./skill/combatcomponent` 通过；`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后 porcelain 只有本次改动；daoruntime（golden + `testdata/runtime/*_test.go`，replace 到 worktree）通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 退出 0；`go build ./... && go vet ./...` 通过；根包 `go test -count=1 .` 通过。
- 生成 game-demo（新模板，replace 到 worktree）：`go build ./... && go vet ./...` 通过；`go test -race -count=3 ./game/entities/... ./game/handler/...` 通过；`go test -count=1 ./...` 通过。其中 NC-61（`TestAttributeLayersRollBackWithTheTransaction`）、NC-65（`TestFinalIsRecomputedFromWornGearWhenThePlayerLoads`）、NC-140（`TestTheTimerHeapRollsBackWithTheTransaction`）原用例不改断言照样通过。
- WAL：`TestNonPersistentAttributeLayersStayOutOfTheWAL` 用真实 `nestwal`（v2 记录）提交一次换装（Put）与一次升级（patch），关闭后重开 `Replay`：两条记录都有 `attr_base`、都没有 `attr_gear` / `attr_final`；用重放出的文档加载玩家，Final 与提交前相同。`TestTimerBookkeepingStaysOutOfTheCommitRecord` 检查定时器提交记录不含 `timer_next_due`。

### 性能（Apple M5，Go 1.27.0，同机同命令，`-count 8 -benchtime 300ms`，旧 = `d346b45c` 模板生成，新 = 本分支模板生成；[benchstat](evidence/dao-unified-rollback-20261005/bench-stat.txt)，基准源码 [属性](evidence/dao-unified-rollback-20261005/attribute_bench_test.go.txt) / [定时器](evidence/dao-unified-rollback-20261005/timer_bench_test.go.txt)）

| 场景（一笔 undo 事务，`RunIsolatedTransaction`） | 旧 | 新 | 变化 |
| --- | --- | --- | --- |
| 升级提交（Base + Final） | 5.17µs / 130 allocs | 5.16µs / 134 allocs | 时间无显著差异，+4 allocs |
| 升级后失败回滚 | 3.02µs | 2.44µs | −19% |
| 换装 / 卸装提交（Gear + Final + Base） | 6.66µs / 11.0KiB | 4.95µs / 8.3KiB | −26% 时间、−25% 内存 |
| 空转 Tick（1 个未到期节点） | 417ns / 8 allocs | 388ns / 5 allocs | 时间无显著差异 |
| 武装 + 触发（另有 16 个待触发节点，两笔事务） | 6.35µs / 154 allocs | 6.66µs / 173 allocs | +4.9% 时间、+12% allocs |

DAO 逐键 undo 的成本低于旧实现每次复制全部层（`CloneProfile` × 3）；只写变了的键也减少了 undo 与脏标记。代价在定时器武装 / 触发：每次从 DAO 建一个调度器（O(n log n)，n 为待触发节点数），换来不存在常驻堆；World 的节点数是活动窗口数（个位到十位），按此量级接受。节点数到数千时应改为按 `timer_next_due` 只取到期节点的写法，或再评估“DAO 恢复代 + 惰性缓存”（§2 已列）。combatcomponent 只是把同样的逆操作从组件搬进 DAO，执行的代码相同，未单独测。

### 未完成 / 后续

- skill Runtime 状态进 DAO：未做，理由与入口见 §4.4，与 B4 一起待维护者定。
- 已生成工程不迁移；需要时 `roost project sync`。已生成工程里的旧 `captureRollback` 在新 core 上仍可用。
- 未在真实三进程（WAL + Mongo 投影）链路上跑被拒提交；本次用真实 Nest 引擎 + 拒绝的 committer（与 WAL 准入失败同一 `rejectCommit` 路径）与真实文件 WAL 的写入 / 重放覆盖。
