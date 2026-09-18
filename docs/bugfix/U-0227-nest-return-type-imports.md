# U-0227：handler 半的生成文件 import 了只被返回类型用到的包

- 仓库：roost-codegen `internal/nest`
- 缺陷类：C8（生成物可编译性）
- 来源：实施 RR-20260917-08 时撞上，无 RR 编号
- 相关：T-121；测试 `internal/nest/return_type_imports_promises_test.go`

## 问题

一个 handler 的返回类型来自"既不是实体参数、也不是普通参数"的第三个包时，生成的 handler 半
（`<name>_nest_gen.go`）会 import 那个包却从不引用它，整个 handler 包编译失败：

```
game/handler/claim_dungeon_nest_gen.go:6:2: "example.com/rr08/game/dungeon" imported and not used
```

## 根因

`internal/nest/gen.go` 的 `generatedTypeRefs` 用 `if !senderOnly || syncSenderOnly` 收集返回类型的包——
handler 半（`senderOnly=false`）因此也被算进去。但 handler 半从不写出返回类型的名字：
`ret, err = handlerX(...)`，`ret` 是 `any`；多返回值那条也只是 `ret = []any{r0, r1}`，类型是推断出来的。
真正写出返回类型名字的只有 sync sender（`Sync_* / MultiSync_*` 的签名）。

为什么一直没暴露：既有 handler 的返回类型要么是内置类型（`int32`、`string`），要么来自实体参数已经 import 的包
（`world.Stats` 的 `world` 同时是实体参数的包）。`format.Source` 不查未使用 import，本包的测试又只比对文本。

## 方案

- 采用：把收集条件改成 `if syncSenderOnly`。async sender 原本就不收（`!true || false` 为假），行为不变。
- 没采用：在 handler 半加 `var _ pkg.Type` 之类的占位使用。那是把无关的类型依赖钉进 handler 半，
  下游包会因为一个它不需要的包而变重。
- 没采用：生成后跑 goimports 删无用 import。掩盖判据错误，而且会把"该有却漏了"的 import 也一起吃掉。

## 改动

`internal/nest/gen.go` `generatedTypeRefs`：返回类型只对 sync sender 收集，注释写明为什么。

## 证明

新测试 `TestHandlerSideGenerationDoesNotImportReturnOnlyPackages` 用 `go/parser` 走一遍生成文件，
把"没有任何标识符引用"的 import 列出来（`format.Source` 不做这件事，本包的文本比对也从没做过）。

修前：

```
--- FAIL: TestHandlerSideGenerationDoesNotImportReturnOnlyPackages
    handler-side file imports packages it never names: [example.com/game/dungeon]
```

修后：`go test ./internal/nest` 全绿；同一条测试还断言 sync sender **保留**了那个 import 且自身没有多余 import。
端到端：生成 game-demo（新增的 `ClaimDungeon` handler 返回 `dungeon.ClaimResult`）`go build ./...` 通过。

## 未做 / 边界

- 这条未使用 import 检查只用在这一个测试里，没有对所有 golden 生成物普遍施加；
  `internal/nest` 其余测试仍是文本比对。
- 返回类型用到**多个**第三方包（如 `map[a.K]b.V`）的情形按同一条路径收集，测试只覆盖单包。
