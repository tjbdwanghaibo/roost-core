# M-04:删掉 RemotePolicyCapable、GetEntityGroupFunc 与 EntityGroup 枚举

**类型:重构(实施改动),不是缺陷修复。** 与 `docs/bug/` 的 RR 编号无关,不关闭任何 RR。
**仓库**:roost-core `entity/`、`nest/`;roost-codegen `internal/entity/`、`internal/roost/`。
**日期**:2026-09-10。**前置**:[M-01](M-01-entity-kind-registry.md)、
[M-02](M-02-category-leaves-the-id.md)、[M-03](M-03-category-lock-order.md)。
**兼容性:破坏性。** 删除了导出符号,消费方需要同版本升级 core 与 codegen,并重新生成实体接线。

## 删了什么,为什么它们没意义了

**`RemotePolicyCapable`。** 它的全部作用是把 kind 放进第一个锁档。锁档现在是 kind 的
category(M-03),所以这个值说不出 category 说不了的事。删掉之后 `RemotePolicy` 只剩三个值:
None、Managed、Mirror,而 `RemoteCapable()` 变成 `Managed || Mirror` —— 名字仍然准确,因为
managed 与 mirror 确实都是"可能在别处",这正是 ID 里 remote 位记录的东西。以前那个叫 Capable
的**策略**才是混淆源:它的字面意思和 `RemoteCapable()` 谓词重名,含义却是"capable 但不 managed"。

**`GetEntityGroupFunc`。** 应用层把 category 映射成锁档的钩子。category 的值现在就是锁档,
不需要映射。顺带解决一个隐患:它是个包级可写变量,消费方在管理器启停时反复赋值与置 nil。

**`EntityGroupRemote` / `Player` / `Alliance` / `Other` / `Cnt`。** 旧的四档枚举。档位现在是
category 的值,枚举里那套 0 到 3 的标度没有对应物;`EntityGroupCnt` 本来就没有任何使用者。

## 换来的形态

`GetEntityGroup` 只剩一条路径:

```go
func GetEntityGroup(guid int64) int {
	if rank, ok := lockRankOf(GetEntityKindFromID(guid)); ok {
		return rank
	}
	if GetEntityRemoteFromID(guid) {
		return int(EntityCategoryRemote)
	}
	return int(EntityCategoryUnknown)
}
```

从 ID 取 kind 是位移,查档是一次原子载入。M-03 里"声明了 category 才走新路径"的双路径消失:
**声明与否不再改变锁档的来源**,声明只是给 category 起名字,好让日志、报错和
`ValidateEntityRegistry` 能说 "world" 而不是 "2"。

新增两个保留值与推荐分类:

```go
const EntityCategoryUnknown EntityCategory = 255 // 本进程不认识的 kind,排最后

const (
	EntityCategoryWorld        = EntityCategoryRemote + 1
	EntityCategoryPlayerScoped = EntityCategoryRemote + 2
	EntityCategoryPlayer       = EntityCategoryRemote + 3
	EntityCategoryOther        = EntityCategoryRemote + 4
)
```

推荐分类是常量而非要求 —— category 现在是完整的 uint8,项目可以在中间插值或在 Other 之后继续。
真正的要求只有一条:远程托管的 kind 必须在 `EntityCategoryRemote`,由 `ValidateEntityRegistry` 校验。
`EntityCategoryUnknown` 是保留值,任何 kind 都不得注册进去,`RegisterEntityCategories` 也拒绝声明它。

**未知 kind 排最后是保守答案。** 档位只用于比较,而持有最后一档之后什么都锁不了,所以一个
本进程无法推理的 ID 永远不会成为"别人的获取被判为逆序"的原因。

## roost-codegen 同步改动

**`remote=capable` 与 `remote=true` 系列拼写改为报错**,而不是静默降级成 `none` ——
降级会改变一个已有实体的锁档。报错文本直接给出替代方案:锁序来自 category,请把 kind 注册进
某个 category 并使用 `remote=none|managed|mirror`。`remote=bogus` 这类拼写错误仍然得到原来的
"不是这几个之一"的信息,不会被误导到迁移提示上。

**`roost add entity` 脚手架归 `entity.EntityCategoryOther`**,并且不再自铸 per-entity 的
`EntityCategory<Name> = 1`。原来每个实体各铸一个 1,现在那是留给远程托管实体的档:它们会全部
被排在所有东西之前,而且彼此同档、互相不能叠锁。生成的文件里写明"category 就是这个 kind 的锁档,
Other 是安全默认",并提示等顺序明确后再挪到 World / PlayerScoped / Player。

## 证明

**core** `entity/lock_order_no_hook_promises_test.go`,
`TestLockOrderIsTheCategoryWithNoApplicationHook`:**故意不声明 category**,断言各 kind 的档位就是
它的 category、managed 即便声明在靠后的 category 也仍按 remote 档排、未知 kind 排
`EntityCategoryUnknown`、未知但带 remote 位的按 remote 档。修前红三条:

```text
managed declared late: lock rank = 0, want the category 1
unknown kind lock rank = 3, want last (255)
unknown remote-bit id lock rank = 0, want remote (1)
```

**codegen** `internal/entity/remote_capable_removed_promises_test.go`:五种 capable / 布尔真拼写
必须被拒且报错要提到 category,存活拼写仍被接受,`bogus` 仍得到普通拼写错误。做过回退验证 ——
临时恢复旧映射后 `remote=on was accepted; it must be refused` 变红,恢复后绿。

**codegen** `internal/roost/add_entity_category_promises_test.go`:建工程后连加两个实体,断言两个
文件都注册进 `entity.EntityCategoryOther`、都不含 `entity.EntityCategory = 1`、都不再声明
per-entity 的 category 常量。

core 全仓 build / vet / test 绿,`entity` 与 `nest` 含 `-race`;roost-kit 全仓构建与测试绿
(kit 未引用任何被删符号);roost-codegen 全仓测试绿。

## 被改动的既有测试,都是刻意的

- `nest/nest_test.go`:`nestRemoteCapableKind` 原来注册为 `RemotePolicyCapable`,改为
  `RemotePolicyMirror` —— 同样是"remote-capable 但非 managed",而且 Mirror 还说出了一件真实的事:
  本地只读副本。它和 `nestRemoteManagedKind` 的 category 都归 `EntityCategoryRemote`。
- `nest/cast_test.go`:`withCastGroupFunc` 原来安装档位映射钩子,现在只给 category 起名字;
  三个本地 category 从 1、2、3 挪到 2、3、4,让开 remote 独占的 1 档,相对顺序不变。
  用到 `nestRemoteManagedKind` 的几处 ID 构造改用 `EntityCategoryRemote`,与它的注册一致
  (否则 `MustRegisterEntityKindCategory` 报 category 冲突)。
- `entity/category_lock_order_promises_test.go`:M-03 那条"未声明就走旧钩子"的测试随钩子删除。
- `entity/kind_registry_promises_test.go`:`EntityGroupRemote` 改为 `EntityCategoryRemote`。

## 消费方升级须知

1. **同版本升级 core 与 codegen**,并重新生成实体接线:生成物里的 `entity.RemotePolicyCapable`
   引用会编译失败。
2. **标记里所有 `remote=capable` / `remote=true` 改掉**,并给该 kind 选一个 category。原来靠
   Capable 进 remote 档的 kind,若要保持"排在玩家之前",选一个小于玩家 category 的值,例如
   `EntityCategoryWorld`。
3. **删掉对 `GetEntityGroupFunc` 的赋值**,把它表达的顺序改写成 category 的取值。
4. **`EntityCategoryRemote` 占用值 1。** 旧工程里若有非远程托管的 kind 用了 category 1,
   要挪走,否则它会和远程托管实体同档、彼此不能叠锁,而 `ValidateEntityRegistry` 只会报出
   "managed 的 kind 不在 remote 档"这一侧的错误。
5. 在 bootstrap 末尾调 `ValidateEntityRegistry()`,它会一次列出所有不一致。

## 边界与未做

- **封表仍未做。** `ValidateEntityRegistry` 只校验不冻结。冻结需要一个"注册结束了"的时点,
  而注册目前散在生成的 `RegisterEntity()`、手写聚合与各种 `init()` 里,没有任何地方拥有这个时点。
  等生成器成为唯一写入方(下一步)才有。
- **`Mirror` 仍然只是声明。** 它现在是"remote-capable 但非 managed"的唯一表达,但没有任何
  订阅快照、拒绝本地写的实现。给它真实语义是独立的一件事。
- ID 布局仍是 `[remote(1)][unique(52)][kind(8)][legacy category(2)]`。把那两位并给 kind 的
  破坏性变更(kind 从 256 到 1024)仍未做,也不影响以上任何一步。
