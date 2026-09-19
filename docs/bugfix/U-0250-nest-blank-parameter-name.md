# U-0250：handler 参数名写成 `_`，生成的 sender 不是合法 Go

- 仓库：roost-codegen `internal/nest`
- 缺陷类：C2（生成物不成立）
- 来源：修 RR-20260919-06 时撞上，无 RR 编号
- 相关：T-144；测试 `internal/nest/blank_param_promises_test.go`

## 问题

`func handlerGrantPurchase(target player.IBagEntity, orderID string, ..., _ int64)` 是合法 Go——
那个参数不再被读，按惯例写成 `_`。生成的 sender 把参数名原样抄进自己的签名，然后**把它当实参传出去**：

```go
func (s *GrantPurchaseSender) Send_GrantPurchase(ctx context.Context, id int64, ..., _ int64) error {
	return client.Dispatch(ctx, handlerName..., id, nest.NewParams(orderID, itemID, count, paidAtUnix, _), ...)
}
```

`cannot use _ as value or type`，四处，全在生成文件里，和"我把某个参数改成了 `_`"之间没有任何提示。

## 根因

`internal/nest/parse.go` 直接用 AST 里的参数名。对 handler 自己的签名它是对的（那里 `_` 合法），
对生成物不对：生成物要**声明并转发**同名形参。名字在 wire 上不存在（参数是位置化的），所以名字只是代码生成的细节。

## 方案

- 采用：**解析阶段就换成可用名**。`usableParamName(name, index)`：`_` 或空串 → `arg<index>`。
  一处改，handler wrapper、sender、syncsender 三套模板全都对。
- 没采用：拒绝 `_` 参数并要求改名。合法的 Go 写法不该被生成器否决，而且报错要写清"哪个参数"才有用，
  成本比换名高。
- 没采用：在模板里判断。三套模板三处判断，下一套模板还会漏。

## 改动

`internal/nest/parse.go`：新增 `usableParamName`，非实体参数的 `Name` 经它产生。

## 证明

`internal/nest/blank_param_promises_test.go`：对带 `_ int64` 的 handler 生成全部产物，
扫描每个 `*_nest_gen.go` 确认没有任何一处声明或传递空白名。修前红（生成物里出现 `_ int64` 与 `, _)`），
修后通过；`go test ./...` 全绿，生成工程 `go build ./...` 通过。

## 未做 / 边界

- 只处理非实体参数。实体参数叫 `_` 会在更早的地方失败（它要被当作锁定目标使用），没有构造这条用例。
- 换出来的名字是 `arg<index>`，index 是**非实体参数序号**。同一个 handler 里同时有 `_` 和一个真叫
  `arg0` 的参数会撞名——理论上可能，没有防；真出现的话生成物编译不过，是可见的失败。
