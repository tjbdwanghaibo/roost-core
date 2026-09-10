# M-01：entity kind 注册表改为按 kind 的无锁定长表

**类型:重构(实施改动),不是缺陷修复。** 与 `docs/bug/` 里的 RR 编号无关,不关闭任何 RR。
**仓库 / 位置**:roost-core `entity/entity_factory.go`。**日期**:2026-09-10。
**行为承诺**:公开 API 的答案与改动前逐项一致,只有"读取不再取锁"这一条是新的。

## 起因

审查 `EntityGuard.CheckContainAllLock` 与 `CheckContainAllIDs` 的重复实现时(见本目录
`M-00-entity-guard-lock-order.md` 之前的两次 refactor 提交),往下追锁序的推导链发现:

```
GetEntityGroup(guid)
  → IsRemoteCapableEntityID(guid)
    → IsEntityKindRemoteCapable(kind)
      → GetEntityKindRemotePolicy(kind)
        → factoryMu.RLock() + map 查找
```

`GetEntityGroup` 有三个调用点,全在热路径上,而且都在**已经持有实体互斥**的情况下调用:

| 调用点 | 频度 |
| --- | --- |
| `nest/nest_dispatch.go` `cmpGuidFunc` | 排序比较器,每次比较调两次,一次 cast 是 O(n log n) 次 |
| `entity/entity_guard.go` `maxLockedGroup` | 每个已持有的锁调一次,O(n) |
| `nest/dispatcher.go` `ForEachSpliceBatch` | 广播每个 id 调一次 |

两个后果:每次查询付一把读写锁的代价;更要紧的是形成一条"先持实体互斥、再取注册表锁"的
获取边,注册期的写锁会把正在做锁序判断的 goroutine 全部挡住。

ID 里本来就带 kind(第 2 到 9 位,纯位移取出),而 `EntityKind` 是 uint8,所以这张表根本
不需要 map,定长 256 槽足够,读取可以完全无锁。

## 改动

**三张 map 合成一条按 kind 的记录。** 原来是 `factoryByKind`、`kindCategoryByKind`、
`kindPolicyByKind` 三张 map 挤在一把 `sync.RWMutex` 下,现在是:

```go
type entityKindEntry struct {
    category EntityCategory
    policy   RemotePolicy
    builder  *EntityBuilderParam // nil until RegisterEntityBuilder runs
}

var (
    registryMu  sync.Mutex // writers only; readers are lock-free
    kindEntries [1 << EntityKindBits]atomic.Pointer[entityKindEntry]
)
```

记录**发布后不可变**:每次更新存一份新的拷贝。所以无锁读者永远看到一条自洽的记录,而不是
三张 map 在两次写之间的中间状态 —— 旧实现靠 RLock 才拿到这个一致性。

**读取全部改成一次原子载入**:`EntityCategoryOfKind`、`GetEntityKindRemotePolicy`、
`GetEntityBuilderParam`、`GetAllEntityBuilders`,以及经由它们的 `IsEntityKindRemoteCapable`、
`IsEntityKindRemoteManaged`、`ResolveEntityKindCategory`、`MustEntityCategoryOfKind`,
和锁序热路径上的 `GetEntityGroup`。

**写入语义逐条保留**,包括那段容忍部分声明的调和规则:category 冲突报错;policy 相等直接返回;
已有 none 可被真实 policy 升级;已有真实 policy 遇到 none 视为部分重复声明而忽略;其余报错。
`RegisterEntityBuilder` 仍然先声明 kind 再存 builder,重复 builder 仍然 panic。

**一处死代码随结构消失**:`GetEntityKindRemotePolicy` 原本在 policy map 查不到时回落去查
builder。因为 `RegisterEntityBuilder` 总是先声明 kind,那条回落从来不可能触发;现在 builder
与 policy 同在一条记录里,"没有记录"就等于"没有 builder",回落在结构上就不存在了。注释写明了。

**`factoryMu` 更名 `registryMu`** 并从 `RWMutex` 改为 `Mutex`:它现在只串行化写入,名字里
的 factory 已经不是它守护的东西。

## 证明

`entity/kind_registry_promises_test.go` 两条。

`TestKindRegistryReadsDoNotBlockOnRegistrationWrites`:像注册那样持住 `registryMu`,再从另一个
goroutine 读 category、policy、group、builder,两秒内必须全部返回。旧实现读者要同一把锁,
**修前红**:

```text
--- FAIL: TestKindRegistryReadsDoNotBlockOnRegistrationWrites (2.00s)
    registry reads blocked while a registration held the registry;
    the lock-ordering hot path can be stalled by a registration
```

`TestKindRegistryKeepsItsRegistrationRules`:把写入语义逐条钉住 —— 只注册 category 时 policy
为 none、none 被升级、反向被忽略、category 与 policy 冲突各自报错、kind none 与 category none
被拒、builder 注册同时声明 kind 且只列一次、`ResetEntityRegistryForTest` 清空每个槽。这条修前
修后都绿,它的作用是保证换存储没有改变答案。

`go test ./entity -count=1 -race` 绿;core 全仓 `go build ./...`、`go vet ./...`、`go test ./...`
绿;roost-kit `go build ./...` 与 `./service/... ./dataengine/...` 绿。

## 边界与未做

- **只换存储,没有改任何语义。** category 仍在 ID 的低两位、仍被 `GetEntityGroup` 读取,
  `RemotePolicyCapable` 仍然存在并仍然把实体归到 remote 锁档,`GetEntityGroupFunc` 这个包级
  可写变量仍然是应用层钩子。锁档**没有**在注册期预计算,因为那个钩子可以在注册之后才被赋值
  (cube 系在 mgr 启停时反复赋值与置 nil),预计算会读到过期值。这些都属于下面的第二步。
- 消费方范围:roost-kit 与 roost-codegen。`cube` 用的是 `cube-core` 另一条模块线,不在本工作区
  的 `go.work` 里,本次改动不影响它;同样的收敛若要进游戏侧,需要在 cube-core 单独实施。
- `EntityKind` 是 uint8,定长表 256 槽约 2KB,`GetAllEntityBuilders` 从遍历 map 变成遍历 256 槽。

## 这是分步方案的第一步,后续三步尚未实施

与用户讨论收敛出的目标形态(仅记录,**代码里都还没有**):

**第二步:category 重设计,删 Capable。** category 值本身即锁序,五档:

| category | 值即锁序 | 成员 |
| --- | --- | --- |
| `EntityCategoryRemote` | 1 | 远程托管实体 |
| `EntityCategoryWorld` | 2 | 世界地图对象 |
| `EntityCategoryPlayerScoped` | 3 | 预留,预期首批成员是玩家在世界地图上的化身 |
| `EntityCategoryPlayer` | 4 | 玩家本体 |
| `EntityCategoryOther` | 5 | 兜底,只给认不出 kind 的跨服 ID |

同时:`RemotePolicy` 整个删除,Managed 由 `Category == Remote` 表达、Mirror 自己一个 category;
`GetEntityGroupFunc` 与 `EntityGroup*` 枚举删除,锁档在注册期算进 `[256]uint8`;
category 停止从 ID 读取(先停读继续写,老 ID 位级不变、零数据迁移);
"归属方是谁"这个关系标签从 `EntityCategory` 拆成自己的类型,因为它和"kind 的锁档"是两件事,
今天共用一个类型且会随 Managed 实体改档而失配。

判据只有两条,不必逐条核对权限差异:**不得引入"从允许变拒绝"**(放宽不会让现有代码失效,
收紧会让原本跑通的路径在运行期开始报 `ErrCastDeadlockRisk`);以及 **Remote 必须是最小档**,
这是唯一有物理依据的排序约束 —— 持着本地互斥去等分布式锁会把一把锁持过一次网络往返。
封表时校验这两条。另外 `Other` 不能当已注册 kind 的静默默认值,漏标 category 要直接报错,
否则漏标的 kind 落到末档就是一次静默收紧。

**第三步:标记成为唯一书写位置。** `category=` 进实体标记,生成器产出身份层注册文件,
手写的 kind 定义表与手写的 `RegisterEntity()` 聚合列举都由生成器接管,
`SealEntityRegistry()` 在 bootstrap 末尾封表并按 kind 名报出缺失项。

**第四步(可选,破坏性):** ID 布局把 category 的两位并给 kind,kind 从 8 位到 10 位。
这一步会让老 ID 解错(kind 从 bit 2 挪到 bit 0),需要数据迁移,收益是 kind 从 256 到 1024。
不做也不影响前三步。
