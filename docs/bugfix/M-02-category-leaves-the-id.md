# M-02:category 离开 EntityID,注册表成为唯一权威

**类型:重构(实施改动),不是缺陷修复。** 与 `docs/bug/` 的 RR 编号无关,不关闭任何 RR。
**仓库 / 位置**:roost-core `entity/idgen.go`、`entity/resolver.go`、`entity/entity_factory.go`、
`entity/entity_guard.go`。**日期**:2026-09-10。**前置**:[M-01](M-01-entity-kind-registry.md)。
**数据迁移**:无。老 ID 与新 ID 位级完全一致。

## 为什么必须先做这一步

目标形态是"category 的值就是锁序",需要五档:remote、world、player-scoped、player、other。
而 category 编码在 EntityID 的低两位,`EntityCategoryNone` 占 0,可用值只有 1 到 3。
三个值装不下五档,所以在动 taxonomy 之前,category 必须先不受 ID 字段宽度约束。

kind 同样在 ID 里(第 2 到 9 位,8 位共 256 个值),而 M-01 已经让"按 kind 查注册表"变成一次
无锁数组载入。所以"从 ID 得到 category"完全可以改成"从 ID 取 kind,再查注册表",nest 只持有
full id 的约束依然满足,代价只是一次数组访问。

## 改动

**注册表成为 category 的权威。** `ResolveEntityID` 先从 ID 取 kind,再查注册表拿 category;
只有这个进程不认识的 kind(跨服 ID)才回落去读 ID 那两位,因为那时没有更好的依据。
`GetEntityGroup` 同样改为查注册表、认不出才回落,所以锁序在 category 超出字段时也是对的。

**注册表不再拒绝超出字段宽度的 category。** `registerEntityKindDefinitionLocked` 去掉
`category > EntityCategoryMask` 的拒绝,`buildEntityIDWithCategory` 同样。关于 pair 本身的
拒绝全部保留:kind 不得为 none、category 不得为 none、同一 kind 的 category 冲突报错。

**ID 里那两位降为历史填充,但继续写。** `makeEntityID` 仍然写 `category & EntityCategoryMask`。
没有任何读者再消费它,所以这个字段的意义只剩一个:让改动前后铸出的 ID 位级一致,任何已落库的
ID 都不需要迁移。超过三个可用值的 category 会在这两位留下无意义的余数,而这恰恰无害,因为没人读。
M-04 若实施(把这两位并给 kind)才会停止写入,那一步是破坏性的、需要迁移。

**`NormalizeFullID` 去掉一处自证。** 它原先把 `meta.Category` 和注册表的 category 相比并报
"category mismatch"。`meta.Category` 现在就来自注册表,两者只可能相等;而 kind 未注册时
`ResolveEntityKindCategory` 已经先报错。所以那行读起来像校验、实际证明不了任何事,删除并留注释。

**`GetEntityCategoryFromID` 标记 Deprecated。** 它现在只是"读 ID 的历史字段"这一个原始访问器,
保留给持有未链接 kind 的 ID 的进程。

## 证明

`entity/category_taxonomy_promises_test.go`,`TestEntityCategoriesBeyondTheIDMaskAreAllowed`:
注册两个 category 超出字段宽度的 kind;断言注册成功、注册表答出真实 category、ID 仍能
round-trip(kind 与 unique id 不变、resolve 出的 category 是注册的那个)、`NormalizeFullID` 接受。

修前红:

```text
--- FAIL: TestEntityCategoriesBeyondTheIDMaskAreAllowed
    registering category 4 must be allowed once category left the ID:
    entity id: category exceeds 2-bit limit
```

`go test ./entity -count=1 -race` 绿;core 全仓 build、vet、test 绿;roost-kit 构建与
`./service/... ./dataengine/... ./remoteentity/...` 绿;roost-codegen 构建与
`./internal/entity ./internal/roost` 绿。

## 两处既有测试被改动,都是刻意的

`kind_registration_promises_test.go`(U-0099 的测试)原先断言"category 超出字段宽度被拒绝并返回
`ErrInvalidCategory`"。这条契约正是本次要去掉的,已改为断言接受,并在测试里写明原因。
超出字段的用例换到自己的 kind 上,原有的 pair 规则序列不受影响。

`kind_registry_promises_test.go`(M-01 的测试)原先调用 `ResetEntityRegistryForTest`。包的
`init()` 注册了多个共享测试 kind,而 reset 清空每个槽,文件名排序让我的重置先跑,后面的测试
就找不到 builder 了。改为不重置、使用无人占用的高位 kind;"reset 清空每个槽"这条断言删掉,
它由 `remote_view_test.go` 覆盖。同类坑值得记住:**这个包的注册表是跨测试共享的,新测试不要
reset,要挑没被占用的 kind**;200 段已被占用的是 200 到 205、232、253、255、281。

## 边界与未做

- **锁序还没有改成 category 的值。** `GetEntityGroupFunc` 这个包级可写钩子仍然是应用层映射,
  `EntityGroup*` 枚举、`RemotePolicyCapable` 都还在,Capable 仍然把实体归到 remote 档。
  这些是 M-03。
- 没有删除任何导出符号,本次对 v1 消费方完全兼容。后续要删的 `RemotePolicyCapable`、
  `GetEntityGroupFunc`、`EntityGroup*` 属于破坏性变更,需要单独决定,并且 roost-codegen 的
  标记解析会同时受影响(`internal/entity/parse.go` 把 `remote=capable` 映射成
  `entity.RemotePolicyCapable`,生成物需要重新生成)。
- `cube` 用 `cube-core` 另一条模块线,不在本工作区,本次改动不影响它。
