# U-0245：新建的 DAO 没有接嵌套回调，第一次存盘前的改动会悄悄丢掉

- 单元：U-0245 · 缺陷类 C2（承诺无实现）· 无 RR（**加 demo 嵌套字段时自查发现**，未发版）
- 仓库：roost-codegen `internal/dao`
- 排障行：T-139 · 与 U-0236 / U-0238 同一族（嵌套所有权），但这条是**从未接上**而不是接错

## 问题

`Init()`（接好嵌套值与 map 成员的回调）只在**装载路径**上被调用——
`UnmarshalBSON`、`ApplySync`、`RestorePersisted`。`New<Dao>()` 不调。

于是一个**新建**的实体（第一次登录的玩家、刚刷出来的怪），其嵌套字段的通知是空的：

```go
dao.GetEquipment().SetSlots(worn)   // 改了嵌套值
// → Equipment.Mark() → notify == nil → 什么也没发生
```

改动不标脏、不进持久化补丁、**不报错**——直到这个实体被存过一次再读回来为止。
中间这段时间里所有对嵌套字段的写入都丢了。

## 为什么此前看不见

- 生成器的运行时门每个 harness 都自己调了一次 `Init()`（`newHero()` 就在末尾调）；
- 顶层 Kind 3 的 `Set<Field>` 在赋值后会自己绑，所以"先 Set 再改"的路径是好的——
  掉进坑里的是"**只穿过嵌套值改、从不调 DAO 自己的 setter**"，而那恰恰是组件的正常写法；
- demo 此前没有任何嵌套 DAO 字段，所以生成的工程里没有使用方。

三个条件凑齐，这条缺陷在两轮专门审查嵌套所有权的 RR（U-0236、U-0238）里都没被碰到。

## 改动

`internal/dao/template_dao.go`：`New<Dao>()` 末尾调 `d.Init()`。构造时 map 是空的、嵌套值是零值，
绑定它们正是想要的；装载路径上的 `Init()` 保持不变（它还要处理被替换掉的内容）。

## 证明

`internal/dao/testdata/runtime/ownership_test.go` 的
`TestAFreshlyConstructedDaoPropagatesNestedChanges`：构造、**不调 Init**、直接穿过嵌套值改一个字段，
断言产生一条提交记录。

第一版红测试是绿的——因为我先调了 `SetPos`，而 Kind 3 的 setter 自己会绑。按组件的真实路径（不碰 setter）重写后：

```
--- FAIL: TestAFreshlyConstructedDaoPropagatesNestedChanges
    changing a nested value of a freshly constructed DAO produced 0 commit records, want 1 — the change would be lost
```

修后运行时门全绿；demo 侧 `game/entities/player/equipment_component_test.go` 的四条（穿戴落库、
两级下的改动落库、换下的件不再落库、回滚后所有权归位）也随之全绿——那四条正是先红在这上面的。

## 未做 / 边界

- **`Init()` 现在在构造与装载两处被调用**，是幂等的（只是重设 notify），但它的职责因此更像"重新接线"而不是"初始化"。
  名字没改。
- 没有验证：一个实体在**跨进程镜像**路径上被构造时是否也走到了这里（`remoteentity` 在 demo 里零覆盖）。
