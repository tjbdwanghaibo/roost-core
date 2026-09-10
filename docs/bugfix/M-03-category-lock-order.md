# M-03:声明 category 后,category 的值就是锁序

**类型:重构(实施改动),不是缺陷修复。** 与 `docs/bug/` 的 RR 编号无关,不关闭任何 RR。
**仓库 / 位置**:roost-core `entity/category_order.go`(新增)、`entity/entity_guard.go`、
`entity/entity_factory.go`。**日期**:2026-09-10。
**前置**:[M-01](M-01-entity-kind-registry.md)、[M-02](M-02-category-leaves-the-id.md)。
**兼容性**:纯新增,没有删除或改变任何导出符号的行为。未声明 category 的应用行为一字不变。

## 这一步做什么

M-02 让 category 不再受 ID 字段宽度约束,这一步把锁序真正搬到 category 上:

```go
const EntityCategoryRemote EntityCategory = 1

type EntityCategoryDef struct {
    Category EntityCategory
    Name     string // 只用于诊断,没有 order 字段
}

func RegisterEntityCategories(defs ...EntityCategoryDef) error
func MustRegisterEntityCategories(defs ...EntityCategoryDef)
func EntityCategoryName(category EntityCategory) string
func ValidateEntityRegistry() error
```

**没有 order 字段,值即锁序。** 应用按要加锁的先后给 category 编号,低的先锁。
`EntityCategoryRemote` 固定为最低位,应用的 category 从 2 起。

**remote 最先这一条不是业务约定,是物理约束。** 远程托管实体的所有权锁在 dispatch 顶层获取,
持着本地实体互斥去等一次网络往返,会把那把锁挡在整条路径上,后端抖一下就级联。其余档位之间
怎么排都只是业务约定,想调就调。

**锁序在注册期派生,运行期一次原子载入。** `lockRankByKind [256]atomic.Uint32`,
在 kind 注册和 category 声明时算好。`GetEntityGroup` 先读它,读到就返回。这条路径被
排序比较器(每次比较两次)、`maxLockedGroup`(每个已持有锁一次)、广播分桶(每个 id 一次)调用。

**远程托管的档位是强制的,不是信任的。** 派生时若发现某个 kind 是 managed 但 category 不是
remote,直接按 remote 档给它排,因为"managed 排在本地实体之后"是唯一绝不能发生的顺序。
同时 `ValidateEntityRegistry` 会把这个声明报成错误,所以错误不会被静默掩盖。

**未知 kind 的档位。** 跨服 ID 的 kind 本进程不认识时:带 remote 位的按 remote 档,
其余按已声明的最大值,也就是最后一档。最后一档是保守答案 —— 持有它之后什么都锁不了,
不会破坏别人依赖的顺序。

**未声明 category 的应用完全不受影响。** 没有声明时每个 kind 的派生档位都是 0(未派生),
`GetEntityGroup` 落到原来的路径:remote-capable 排最前、`GetEntityGroupFunc` 映射 category。
两套档位数值不同标度(旧枚举 0 到 3、新的从 1 起),但它们只互相比较,而一个进程只走一条路径。

## 证明

`entity/category_lock_order_promises_test.go` 两条,都是新 API 所以修前不编译。

`TestRegisteredCategoriesMakeTheCategoryValueTheLockOrder`:声明五档,注册各档的 kind,
断言 `GetEntityGroup` 返回 category 的值;其中一个 kind 故意声明成"managed 但在 player 档",
断言它仍然按 remote 档排、并且 `ValidateEntityRegistry` 报错且错误里点出这个 kind 的编号;
另外断言 `EntityCategoryName` 只影响诊断输出。

`TestUndeclaredCategoriesKeepTheLegacyGroupHook`:不声明 category,断言钩子为 nil 时仍是旧默认
`EntityGroupOther`、设了钩子就走钩子的映射。这条钉住"这次改动不会移动任何现有部署"。

`go test ./entity -count=1 -race` 绿;core 全仓 build、vet、test 绿;roost-kit 与
roost-codegen 全仓构建与测试绿。

## 边界与未做

- **`ValidateEntityRegistry` 只校验,不封表。** 计划里的 `SealEntityRegistry` 还包含"封表后再写
  直接 panic",那一半要等生成器成为唯一写入方的那一步;现在迟到的注册仍然合法,这个函数可以
  重复调用。名字用 Validate 而不是 Seal,是为了不撒谎。
- **没有删除任何东西。** `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*` 都还在,
  Capable 在旧路径下仍然把实体归到 remote 档。删除它们是破坏性变更,需要单独决定,并且
  roost-codegen 的 `internal/entity/parse.go` 把 `remote=capable` 映射成
  `entity.RemotePolicyCapable`,同一次发布要一起改、生成物要重新生成。
- **`EntityCategoryRemote = 1` 与现有脚手架的取值冲突。** codegen 的 `roost add entity` 给每个
  新实体生成 `EntityCategory<Name> entity.EntityCategory = 1`,`internal/entity/testdata` 里
  玩家实体的 category 也是 1。这些取值在未声明 taxonomy 时没有特殊含义,但一个项目要采用
  taxonomy 就必须把 1 让给 remote、业务 category 从 2 起。脚手架的默认取值需要在生成器那一步
  一起调整,否则新项目一采用 taxonomy 就会被 `ValidateEntityRegistry` 判为"非 managed 的 kind
  占了 remote 档"。这一条是采用前必须处理的,记在这里以免遗漏。
- 广播分桶的密集桶上限 `spliceDenseGroupLimit` 是 8,五档都落在密集桶内;若某个应用把 category
  编到 8 以上,分桶会走稀疏 map 路径,只是慢一点,不影响正确性。
