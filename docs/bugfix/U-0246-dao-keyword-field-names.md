# U-0246：字段名小写之后是 Go 关键字，生成物编译不过

- 仓库：roost-codegen `internal/dao`
- 缺陷类：C2（生成物不成立）
- 来源：实现 game-demo 第十六批（World 计时器节点）时自查，无 RR 编号
- 相关：T-140；测试 `internal/dao/keyword_field_promises_test.go`

## 问题

DAO 定义里一个叫 `Type` 的字段（计时器节点的类型、技能的 `Range`、配置的 `Map`……都是很自然的名字），
生成出来的结构体里私有字段是 `type int32` —— 不是合法 Go。生成器在 `format.Source` 阶段失败，
报的是：

```
generate nested TimerNode: format generated nested: 19:2: expected '}', found 'type' (and 8 more errors)
```

错误指向一个临时的生成文件，和"你的定义里有个叫 Type 的字段"之间没有任何提示。定义本身完全合法，
`bson:"type"` 也完全正常，所以第一反应是生成器坏了。

## 根因

`internal/dao/gen.go` 的 `fieldVarName` 把字段名的首段小写，用作生成结构体的私有字段名和局部变量名
（模板里是 `fieldVar`）。它处理了连续大写（`ID` → `id`、`HTTPPort` → `httpPort`），但没有处理
**结果恰好是 Go 关键字**这一种。

只有关键字会坏。字段名叫 `String`、`Len`、`New` 生成出 `string` / `len` / `new` 是合法的——
它们是预声明标识符，在自己的作用域里遮蔽掉外层名字而已；`type` / `range` / `select` / `map` / `func`
这些则根本不解析。

## 方案

- 采用：**只重命名私有名**。新增 `safeFieldVarName`：结果是关键字就加 `Value` 后缀（`type` → `typeValue`）。
  访问器（`GetType` / `SetType`）、BSON 键、脏位常量都保持字段自己的拼写，所以对调用方没有任何变化。
  两处模板函数表（dao 与 nested）都换成它——nested 有自己的模板和自己的同一份问题。
- 没采用：拒绝这类字段名并要求定义方改名。名字是业务的，`Type` 在计时器节点上就是对的词；
  让生成器绕开关键字比让每个工程绕开生成器合理。
- 没采用：给所有私有名统一加前缀（如 `f_`）。会改掉现有生成物里每一个字段名，diff 巨大而收益只在这一种情况。

## 改动

`internal/dao/gen.go`：新增 `goKeywords` 表与 `safeFieldVarName`，dao 与 nested 两张模板函数表的
`fieldVar` 指向它。

## 证明

`internal/dao/keyword_field_promises_test.go` 两条，修前红：

```
--- FAIL: TestAFieldWhoseLowercaseNameIsAKeywordGenerates
    generate: generate dao ThingDao: format generated dao: 18:2: expected '}', found 'type'
--- FAIL: TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates
    generate: generate nested Node: format generated nested: 19:2: expected '}', found 'type'
```

修后两条通过，`go test ./internal/dao`、`go test ./...` 全绿；golden 生成物无变化（现有定义里没有关键字字段）。
game-demo 第十六批的 `WorldDao.Timers`（`map[int64]*TimerNode`，`TimerNode.Type`）是第一个真实使用方，
生成、编译、实跑均通过。

## 未做 / 边界

- 只处理 Go **关键字**。预声明标识符（`string`、`len`、`cap`、`new`、`error`……）仍原样小写，
  因为它们合法；如果将来生成的模板在同一作用域里需要调用 `len(...)` 而某个字段叫 `Len`，会再出问题——
  那时候要修的是模板，不是这里。
- 后缀选了 `Value` 而不是下划线，和生成物里其它命名保持一致。改后缀会改变生成文件的内容（不改变行为），
  升级时 `make generate` 的 diff 里会看到。
