# glsvet：组件字段写提示（A1 盲区，维护者第十三轮决定 A）

2026-10-06，分支 `a1f`，基线 `71c8f394`。来由：[DECISIONS-PENDING 第十三轮](../review/DECISIONS-PENDING-2026-10-05.md)“A1 盲区”一行。维护者选 A：glsvet 对组件方法（不含初始化）写非 DAO 字段给提示，缓存类字段用简短标注豁免（原话“tag命名需要简洁一些”，定为 `//roost:cache`）。

## 1. 问题

A1 的规则是回滚统一走 DAO（[方案](REFACTOR-2026-10-05-dao-unified-rollback.md)）：事务里会改的状态放在 DAO，由 DAO 的 undo / 快照统一回滚。glsvet 原有的 A1 提示（`componentUndoHints`，RR-20261006-13 起跟进一层同包 helper）只抓“组件自己登记 undo”。组件把可变状态放在普通字段里、也不登记 undo 时，handler 失败或提交被拒后这些字段不回滚，glsvet 什么都不报——这比自己登记 undo 更糟，因为连“有人想过回滚”的痕迹都没有。

## 2. 规则

`cmd/glsvet/componentfields.go` 的 `componentFieldHints`，与 `componentUndoHints` 并列在 `vetDirectory` 里调用。

- **组件**：与现有 A1 提示同一口径——本包里名字以 `Component` 结尾的类型，或匿名嵌入 `ComponentBase` 的结构体（`componentTypes`，两个提示共用）。
- **写**：组件方法里以 `接收者.字段` 为根的赋值左值（`=`、`op=`、`++` / `--`、`for ... = range`，含 `c.f[k]`、`c.f.x`、`*c.f`），以及内建 `delete` / `clear` 的第一个参数。也就是“给组件自身字段赋值，或修改字段中的 map / slice 元素”。
- **跟进一层同包 helper**（口径与 RR-20261006-13 一致）：同包包级函数的参数类型是本包组件（或其指针），函数体里写了该参数的未豁免字段，组件方法调用它时在调用处提示。只跟一层。
- **初始化 / 装配钩子除外**：框架实际的组件生命周期钩子是 `entity.ComponentInterfaceBase` 的 `OnInitFinish`（实体新建或加载后）与 `OnDestroy`（销毁），都不在业务事务里；组件工厂（`RegisterComponentFactory` 的回调、`NewXComponent`）是函数不是方法，本来就不检查。`Name` 不写字段，不需要列。
- **豁免的字段**（不提示）：
  - DAO 句柄：字段类型名（去掉指针、包名、类型参数）以 `Dao` 或 `DAO` 结尾；
  - `//roost:cache`：写在字段声明上一行或行尾（`//roost:cache 理由` 也可以）。用于缓存、可从 DAO 重建的派生索引等允许不随事务回滚的字段；
  - 函数类型字段（`func(...)` 字面量或同包 `type X func(...)`）：装的是行为（投影、回调），由装配方法装上，不是事务状态。见 §4 的命中判定。
- **级别**：与现有 A1 提示一致，打印 `hint:`，不计入 findings、不改退出码。两个 A1 提示现在也计入结尾的 “N hint(s) for review” 汇总（之前只打印、不计数）。

只按语法判断、没有类型信息，所以看不见：先取到局部变量再改（`m := c.items; m[k] = v`）、经方法调用改（`c.items.Add(x)`）、嵌入类型提升上来的字段、跨两层以上的 helper。这与 A3 停止提示、RR-13 的取舍相同：提示是给复审的线索，不是证明。

### 解析模式（顺带生效）

读 `//roost:cache` 需要字段注释，`vetDirectory` 从按 `0` 解析改为 `parser.ParseComments`。顺带生效的是 `isNestHandler` 读函数文档里的 `roost:nest`：之前文档注释恒为 nil，只有名字以 `handler` 开头的函数才被当作 Nest handler 检查。改后全仓（`./...` 与 `-tests ./...`）、两个示例模块、重新生成的 game-demo 的 finding 与提示和改前相同（排序后逐行相同；包内文件的遍历顺序本来就不固定），没有新的违例，新增 `TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix` 钉住。

### 标注名

按维护者要求先实现 `//roost:cache`。考虑过更短的写法：`//cache`（没有 `roost:` 前缀，与仓内 `//roost:nest` / `//roost:dao` / `//roost:component` 指令族不一致，也容易与普通注释撞）、`//roost:nr`（不回滚的缩写，读不懂）。没有找到更短且同样清楚的，保留 `//roost:cache`。

## 3. 先红后绿

夹具 `cmd/glsvet/testdata/a1fields/quest.go`：`QuestComponent` 把进行中的任务放在 `active map` / `history []int32` / `progress int32` 三个普通字段里、不登记 undo；另有 `//roost:cache` 的 `lookup`（上一行标注）与 `byTag`（行尾标注）、函数字段 `onDone`、DAO 句柄 `dao`、`OnInitFinish` 里的初始化写、只读方法。用例 `componentfields_promises_test.go` 以 glsvet 命令运行（`runGlsvet`，断言输出与退出码）。

- **红**（`componentfields.go` 已在、`vetDirectory` 未接线，即旧 glsvet 行为）：`TestComponentFieldWritesOutsideTheDaoAreHinted` 失败，glsvet 输出为空——

  ```
  missing hint "quest.go:31:44: hint: component QuestComponent.Accept writes field active outside the DAO"
  ...
  0 field-write hints, want 5 (no hint for the //roost:cache fields, the func field, the DAO handle, OnInitFinish or reads)
  ```

  （红时的期望列号是初稿算错的列，绿时改为实际列；失败原因是没有任何输出，与列号无关。）`TestNestDirectiveMarksAHandlerWithoutTheHandlerPrefix` 在旧 `main.go` 上 `findings = 0, want 1`。
- **绿**：五条提示——`Accept`（map 元素）、`Finish` 两条（`delete`、`append` 赋值）、`Advance`（`++`）、`Reset`（经 `resetQuests` 写 `quests.progress`）；`lookup` / `byTag` / `onDone` / `dao` / `OnInitFinish` / 只读方法零提示；退出码 0。

## 4. 误报测试与命中判定

全部 `GOWORK=off`，同一个新 glsvet 二进制：

| 范围 | 字段写提示 | 判定与处理 |
| --- | --- | --- |
| 全仓 `./...` | 0 | — |
| 全仓 `-tests ./...` | 0（另有 4 条 A3 停止提示，改前就有） | — |
| 全仓各 `testdata` 目录 `-tests` | 0 | — |
| `skill/examples`、`examples` 两个示例模块（`./...` 与 `-tests`） | 0 | — |
| 重新生成的 game-demo（replace 到本 worktree，`./...` 与 `-tests`） | 0（`-tests` 另有 39 条 D-L3 时钟提示，在生成工程的测试文件里，改前同为 39 条） | — |
| 规则初稿（不豁免函数类型字段）在全仓 | 1：`skill/combatcomponent/component.go:252` `CombatComponent.ProjectAttributes` 写 `projection` | **规则误判，收紧**：`projection` 是业务给的属性投影函数，文档要求在实体工厂里构造后装一次（`ProjectAttributes` 是装配方法），状态全在 `CombatDao`。它不是缓存，加 `//roost:cache` 语义不对；也不是 A1 违规。收紧为“函数类型字段不提示”，`TestSkillPackagesGetNoComponentFieldHint` 扫 skill 四个包钉住 |

真问题 0 条、需要加 `//roost:cache` 的 0 条：A1 实施（`5407f127`）后框架与 demo 模板里的组件只剩 `owner` / `dao` / 投影函数，没有可变字段。所以没有组件源码或生成模板要改，生成的工程开箱 glsvet 干净（已用重新生成的 game-demo 验证）。

## 5. 改动面与兼容

- 代码：`cmd/glsvet/componentfields.go`（新）、`cmd/glsvet/main.go`（接线、带注释解析、A1 提示计数、组件识别改用共用的 `componentTypes`）、测试 `componentfields_promises_test.go` 与夹具 `testdata/a1fields/quest.go`。
- 文档：本记录；A1 方案 §5；roost-coding 执行契约 A1 条；`codegen/README.md` A1 段与生成工程文档（`render_docs.go` 的 DAO 规则，改了生成文档的文字）；`docs/skill/skill-casting-and-combat.md`；CHANGELOG；v1.23.0 发版文档 DAO-1 的未验证项 / 检查点与 W-2026-10-06-01 的归属说明；DECISIONS-PENDING 第十三轮。
- 兼容：只多出提示，退出码不变，CI 与生成工程的 glsvet 门禁不受影响。已有业务工程里组件若有可变字段，升级后会看到提示：把状态移进 DAO（不落库用 `nopersist`），或确属缓存的字段加 `//roost:cache`。`roost:nest` 文档标注从此真正生效：名字不以 `handler` 开头、只靠文档标注的 handler 现在会被检查，本仓与 game-demo 里没有新的违例。

## 6. 验证

命令：`gofmt -l` 空；`go vet ./cmd/glsvet`；`go test -race -count=3 ./cmd/glsvet/...`；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；全仓 `./...` 与 `-tests ./...`；`go test -count=1 ./codegen/...`、`go generate ./...` 后 porcelain；重新生成 game-demo 跑 build / vet / test / glsvet；根包 `go test -count=1 .`；`go build ./... && go vet ./...`。实际结果写在 §7。

## 7. 实施状态与验证结果

已实施（`565f657b`），未发版。验证（`GOWORK=off`，基线 `71c8f394`，Go 1.27.0）：

- `gofmt -l` 空；`go vet ./cmd/glsvet ./codegen/internal/roost` 通过；`go test -race -count=3 ./cmd/glsvet/...` 通过。
- `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 退出 0、无输出；全仓 `./...` 退出 0、无输出；`-tests ./...` 退出 0，只有改前就有的 4 条 A3 停止提示（`kit/nats` 等测试文件）。
- `go build ./... && go vet ./...` 通过；`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后 porcelain 只有本次改动；根包 `go test -count=1 .` 通过。
- 重新生成 game-demo（新 `render_docs.go`，replace 到本 worktree）：`go build ./... && go vet ./...` 通过，`go test -count=1 ./...` 通过；glsvet `./...` 退出 0、无输出，`-tests ./...` 字段写 / undo 提示 0 条（39 条 D-L3 时钟提示改前同样存在，不属本项）。

没有 RR：误报测试里没有真问题。未做：没有类型信息的盲区（§2 末段）不在本次范围。
