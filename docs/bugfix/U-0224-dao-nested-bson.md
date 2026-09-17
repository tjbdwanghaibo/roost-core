# U-0224：dao 生成的嵌套 struct 没有 BSON 表示，落库只剩 `{"dirtyhook": {}}`

**仓库 / 位置**：roost-codegen `internal/dao/template_nested.go`（嵌套 struct 模板）；消费方 `template_dao.go` 的 `marshalCommitState` /
`CaptureRollbackState` / `RestoreRollbackState` / `MarshalSync` / `ApplySync` 把嵌套值直接放进 `bson.M` 交给反射编码。
**来源**：用户复审提出（"DirtyHook 有没有加 bson ignore"），无 RR 编号。**修复单元**：U-0224（C2）。**定位文档**：TROUBLESHOOTING T-118。

## 问题

生成的嵌套 struct（`//roost:dao` 字段引用的 struct，如 `Position`、`EquipInfo`）形如：

```go
type Position struct {
	dataengine.DirtyHook
	x int32
	y int32
}
```

字段全部未导出（变更只能走生成的 setter，dirty / undo 才不会被绕过），也没有任何 `MarshalBSON`。DAO 的文档把 `d.pos` 原样放进 `bson.M`，
mongo-driver v2 反射编码看不见未导出字段，只看见导出的内嵌 `DirtyHook`（自己的字段也未导出）。用真实驱动跑一遍（`bsonprobe`）：

```
{"pos": {"dirtyhook": {}}, "equips": {"1": {"dirtyhook": {}}}, "_id": 7}
```

嵌套数据一个字节都没有存进 Mongo；回滚快照（`CaptureRollbackState`）与同步（`MarshalSync`）走同一条路，同样丢。demo 的 Player / World 只有标量与 `map[int64]int32`，
所以实跑没暴露；golden 测试只比对生成文本，不编译、不编码。

## 根因

C2：模板承诺"嵌套 struct 带 dirty 传播、随父 DAO 持久化"，但只生成了访问器与 setter，没有给编码器留任何入口；而 `DirtyHook` 作为导出的内嵌字段
反而进了文档。

## 方案选择

- 把嵌套字段改成导出 + bson tag：绕过 setter 就能改状态，破坏 dirty / undo 的不变量。不采用。
- 只加 `bson:"-"`（用户问的那一半）：文档从 `{"dirtyhook": {}}` 变成 `{}`，数据仍然丢。不够。
- **采用**：`dataengine.DirtyHook \`bson:"-" json:"-"\`` + 为每个嵌套类型生成 `MarshalBSON()`（**值接收者**——父 DAO 放进 `bson.M` 的是值不是指针）
  与 `UnmarshalBSON()`（指针接收者），经一个私有 wire struct（`<name>BSONDoc`，字段 snake_case 键，与 DAO 字段键同一套 `bsonKey`）。
  map 字段（`fmap.SmallSafeMap`）经生成的 `<field>RawMap()` / `set<Field>RawMap()` 转原生 map，与 DAO 层的做法一致。
  嵌套里的嵌套、`[]*T`、`map[K]*T` 都靠反射编码器对字段 / 元素调用各自的 `MarshalBSON` 自然覆盖。

## 改动

- `internal/dao/template_nested.go`：tag、import bson、`<name>BSONDoc`、`MarshalBSON` / `UnmarshalBSON`、map raw 辅助。
- `internal/dao/gen.go`：嵌套模板的 FuncMap 抽成 `nestedFuncMap()`（加 `bsonKey`），测试用同一份渲染。
- `internal/dao/parse_test.go`：`TestGenerateNested` 的 `"x int32"` 字面断言改成容忍 gofmt 列对齐。
- golden 三份重生成（`gen_position_nested.go`、`gen_equip_info_nested.go`、`gen_gem_info_nested.go`）。
- 测试：`nested_bson_promises_test.go`。

## 证明

- 修前红：`generated nested struct lacks "dataengine.DirtyHook \`bson:"-" json:"-"\`"`、`lacks "func (s Position) MarshalBSON() ([]byte, error)"`、
  `lacks "\`bson:"x"\`"`……（五条）。
- 修后：`internal/dao` 全绿；把重生成的三份 golden 放进一个带 mongo-driver v2 与 roost-core v1.15.5 的临时模块编译、编码、解码：
  `stored: {"_id": 7, "pos": {"x": 3, "y": 4}, "equips": {"1": {"level": 5, "star": 2, "gems": null}}}`，`restored: pos=(3,4) equip[1]=(level 5, star 2)`。

## 未做 / 边界

- codegen 单测仍不编译生成物；生成物对真实驱动的编码行为只有这次手工 probe，建议进 framework-compat 的 demo scenario（给 demo 加一个嵌套字段即可覆盖）。
- 已有部署里嵌套字段的历史文档是 `{"dirtyhook": {}}`：解码到新代码得到零值，等于数据本来就没存过；没有迁移可做。
- 嵌套里的嵌套（`EquipInfo.gems` 的 `*GemInfo`）从存储解码回来后没有 `SetNotify` 接线，改它不会把 dirty 传给父级——模板一直如此，另记 Wanted W-2026-09-17-02，不在本单元顺手改。
