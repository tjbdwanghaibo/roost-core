# M-05:category 写在标记上,生成的聚合注册末尾校验注册表

**类型:重构(实施改动),不是缺陷修复。** 与 `docs/bug/` 的 RR 编号无关,不关闭任何 RR。
**仓库**:roost-codegen `internal/entity/`、`internal/roost/`、`internal/registry/`。
roost-core 只改本文档。**日期**:2026-09-10。
**前置**:[M-01](M-01-entity-kind-registry.md) 到 [M-04](M-04-drop-capable-and-the-group-hook.md)。
**兼容性**:新增为主。旧标记没有 `category=` 时生成物与从前一致;生成的聚合注册文件多了一个
import 与一次校验调用,重新生成即可。

## 拆掉的隐式前置

生成的接线原来写:

```go
Category: entity.MustEntityCategoryOfKind(EntityKindPlayer),
```

一次运行期查表,查不到就 panic。于是"业务文件里那句手写的 `MustRegisterEntityKindCategory`
必须先跑"成了隐式前置,而这个先后顺序只由手写的聚合文件第一行保证 —— 谁把那一行删了或挪了位置,
得到的是一个 panic,而不是一条说得清的错误。

category 是这个 kind 的静态事实,没有理由在运行期解析。现在写在标记上:

```go
//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryOther
```

生成物直接粘进去,前置条件随之消失。`category=` 的取值要求是"导出标识符,可带包限定",
所以 `category=1`、`category="player"`、`category=entity.` 这类写法在生成器就报错,
而不是留到消费方的编译器 —— 把 category 搬进标记的意义本来就在这里。

没写 `category=` 时仍然生成 `MustEntityCategoryOfKind`,所以还没迁移标记的工程不受影响。

## 脚手架

`roost add entity` 生成的实体文件不再有那句手写注册,category 写在标记上:

```go
const EntityKindGuild entity.EntityKind = 2

//roost:entity id=2 entityKind=EntityKindGuild category=entity.EntityCategoryOther
type Guild struct { ... }
```

**同时修掉一处会让工程编译不过的遗留。** M-04 删掉了脚手架自铸的 `EntityCategory<Name> = 1`,
而 `roost add lifecycle` 生成的代码在两处引用它:`access.Get(ctx, fullID, <pkg>.EntityCategory<Name>)`
与 `EntityCreateParam{Category: <pkg>.EntityCategory<Name>}`。两处都改成
`entity.MustEntityCategoryOfKind(<pkg>.EntityKind<Name>)` —— 这里是运行期查表且合适,
因为 lifecycle 跑在 `RegisterAll` 之后,注册表已经完整。

## 聚合注册末尾的校验

`registry.RegisterAll()` 是整个工程里唯一知道"注册结束了"的时点:bootstrap 在 `app.New` 之前
调它一次,所有 `//roost:register` 函数按阶段跑完。校验挂在这里:

```go
	// phase: entity
	player.RegisterEntity()

	if err := entity.ValidateEntityRegistry(); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	return nil
```

一次列出所有不一致:远程托管的 kind 不在 remote 档、kind 落在没声明过的 category、kind 占了
保留的 unknown 档、remote 不是最小档。挂在任何更早的位置,报出来的只会是"这个 kind 还没注册"。

模板的 `fmt` 与 `roost-core/entity` 两个 import 因此变成无条件,`NeedsFmt` 这个条件量删除。

## 证明

`internal/entity/category_marker_promises_test.go` 三条:`category=` 被解析并直接进生成物、
生成物里不再有 `MustEntityCategoryOfKind`;不写 `category=` 仍保留运行期查表;
四种畸形取值被拒、三种合法取值被接受。前两条修前不编译(`EntityDef` 没有 `Category` 字段)。

`internal/roost/add_entity_category_promises_test.go`:连加两个实体,断言两个文件都在标记上写了
`category=entity.EntityCategoryOther`、都不再手写 `MustRegisterEntityKindCategory`、都不含
`entity.EntityCategory = 1`;再加一个 lifecycle,断言生成物不再引用被删的 per-entity 常量。

`internal/registry/validate_promises_test.go` 两条:生成的聚合调用
`entity.ValidateEntityRegistry()`、import 了 entity 包、而且调用位置在最后一个注册**之后**;
没有任何注册的空工程也照样校验。修前红。

roost-codegen 全仓测试绿(`internal/roost` 含建工程与升级兼容那批,约 23 秒)。

## 被改动的既有测试

`internal/registry/registry_test.go` 的 `TestGeneratedAggregateParsesAndImportsWhatItUses`
原来按"有无返回 error 的注册"断言 `fmt` 是否被 import。聚合现在总是以一次会返回 error 的校验
结尾,所以 `fmt` 与 entity 包都是无条件的,用例表里的 `wantFmt` 去掉,改为断言这三个 import
永远存在。测试头部注释写明了原因。

## 边界与未做

- **封表仍未做,而且值得先想清楚要不要做。** 现在有了落点:`RegisterAll` 之后把注册表冻结,
  再注册直接 panic。它能捕获的是**校验之后**才发生的注册 —— 某个懒加载的包在服务已经开始跑时
  注册了一个 kind,而在那之前铸出的 ID 曾按不完整的注册表解析过,锁档可能是 `EntityCategoryUnknown`
  而不是它真实的档。代价是合法的晚注册会 panic,消费方的测试里这种写法很常见。
  校验已经拿到了大部分价值(同一时刻、同样的错误、一次全列),冻结只多覆盖"更晚"这一类。
  建议等有工程真的采用了 taxonomy 再决定。
- **`category=` 尚未成为必填。** 让它必填要等消费方把现有标记迁完;必填之后
  `MustEntityCategoryOfKind` 这条生成分支才能删。
- **身份层与实现层仍未拆开。** 一个只路由 ID 的服务(gate)现在仍然要链接实体实现才能拿到
  kind 到 category 的映射,因为映射是随 `RegisterEntityBuilder` 一起注册的。要拆就得让生成器
  另出一个只含身份声明的包。这一条在 M-01 的方案里提过,尚未实施。
