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

## 成本核对（2026-09-17，用户追问"会多 GC 吗"）

`go build -gcflags=-m` 对生成的 `EquipInfo`：`MarshalBSON` 里的 wire struct **逃逸到堆**（作为 `any` 传给 `bson.Marshal`），`UnmarshalBSON` 的
`doc` **moved to heap**（取地址传给 `bson.Unmarshal`），`gemsRawMap` 的 `make(map)` 逃逸；此外 `Marshaler` 契约本身每个嵌套值返回一份新 `[]byte`，
父编码器再拷进自己的缓冲。基准（一份 DAO 文档：1 个嵌套 struct + 2 个嵌套 map 值、各含 3 个内层嵌套；Apple M 系列，mongo-driver v2.9.1）：

| | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| 生成的 MarshalBSON（本单元） | 3532 | 3242 | 61 |
| 同形状、字段导出、纯反射内联编码（下界） | 1890 | 1081 | 21 |
| 单个 `Position` 值 | 172 vs 161 | 144 vs 136 | 4 vs 3 |
| 解码同一份文档 | 2839 vs 1655 | 1009 vs 464 | 48 vs 31 |

结论：**"理论上不多 GC"不成立**——每个嵌套值多约 4 次分配（`any` 装箱、返回的 `[]byte`、map 的 raw 转换、父级拷贝），整份文档约 3×。
这是 `bson.Marshaler` 这条接口的固有代价，不是实现细节能省掉的。要回到下界，父 DAO 的文档字段应直接放**导出字段的 wire struct**
（`d.pos.bsonDoc()` / `map[K]*equipInfoBSONDoc`），由反射内联编码，不经过 `Marshaler`；见后续单元。

## 复核后的补修（仍归 U-0224）：wire 形式直接进父 DAO 文档

上面的核对说明 `bson.Marshaler` 这条路本身贵。补修把编码入口挪到父 DAO：嵌套类型生成 **wire 形式** `<name>BSONDoc`（导出字段）和
`bsonDoc()` / `setBSONDoc()` / `<x>FromBSONDoc` / `<x>PtrBSONDoc` / `<x>PtrFromBSONDoc`；DAO 模板的六个文档构造点（rollbackDoc 两处、
`marshalCommitState`、`marshalPersistData`、补丁 full-field、`MarshalSync`）经 `wireType` / `toWire` / `fromWire` 直接放 wire 形式，反射内联编码；
`ApplySync` / `Unmarshal` / `RestoreRollbackState` 反向转换后由既有的 `set<X>RawMap` / `Init` 接 dirty 钩子。容器用每包一份
`gen_dao_bson_helpers.go` 的泛型 `daoMapDocs` / `daoSliceDocs` 映射。`MarshalBSON` / `UnmarshalBSON` 保留给单独编码的嵌套值。
**未动**：map 单键补丁（`markXKeyDirty` → `nest.MarkPersistSet(path, val)`）仍记指针、提交时经 Marshaler——它记的是对象而不是快照，改成快照会改变
"提交时取当时状态"的语义；每次变更一个值、一次分配，可接受。

同一份真实生成的 `HeroDao`（1 个嵌套 struct + 2 个 map 值 × 3 个内层嵌套），旧 golden 与新 golden 在同一临时模块里对比：

| | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| 修前（Marshaler 路）`marshalCommitState` | 4944 | 4370 | 82 |
| **补修后（wire 形式）** | 3351 | 3325 | 63 |
| 反射下界（同一文档，wire 形式预先建好） | 2648 | 1225 | 33 |
| 修前 `Unmarshal` | 5951 | 2760 | 112 |
| **补修后 `Unmarshal`** | 4112 | 2017 | 93 |

到下界还差的 30 次分配是转换层本身：raw map（DAO 层原有）、wire map、每个指针元素一个 `*<x>BSONDoc`。再往下要么 map 值用值类型 wire（丢 nil 语义），
要么池化，收益递减，不在本单元做。

测试：`nested_bson_promises_test.go` 新增 `TestTheDaoDocumentCarriesNestedWireFormsNotMarshalers`（断言 DAO 文档里是 `d.pos.bsonDoc()` /
`daoMapDocs(…, equipInfoPtrBSONDoc)` 而不再是 `d.pos`）；golden 全部重生成；真实驱动往返 `TestHeroDaoRoundTrip` 通过。

## 为什么之前没发现（以及补的门）

四层都没有把生成物"跑起来"：

1. **codegen 的测试只比对文本**。`golden_test.go` 逐字节比对生成源码，`parse_test.go` 用 `go/parser` 断言结构（甚至有一条
   `assertStructFieldsUnexported` 专门断言"字段必须未导出"——这是刻意的设计，但没有人接着问"那它怎么序列化"）。生成物在 codegen 里
   **从不编译、从不编码**：codegen 对运行时零依赖（`go.mod` 只有 yaml），`testdata/` 又对 go 工具不可见，所以连"引用了 bson 却没实现
   Marshaler"这种能被编译器或驱动暴露的事都无从发生。
2. **demo 没有嵌套 struct**。game-demo 的 Player 是标量 + `map[int64]int32`，World 是两个计数器；framework-compat 的 demo scenario 与
   实跑压测都真编译、真落库、真重启核对，但覆盖不到嵌套字段这条路。
3. **core / kit 没有消费者**。core 的 dataengine 测试用手写 DAO 替身；kit 没有用生成嵌套 struct 的地方；`DirtyHook` 全仓只有一处定义。
4. **审计矩阵的盲区**。`internal/dao` 那行：09-06 U-0041 是"生成器层守卫"（redis mode / key、dbscope 校验），09-09 是脚本扫；覆盖矩阵的八个
   缺陷类里没有"生成物对第三方运行时契约是否成立"这一类——问题不在 Go 语义里（代码合法、测试全绿），在 mongo-driver 的反射规则里
   （未导出字段不可见、导出的空内嵌 struct 编成空子文档）。

一句话：**这是"生成器生成了什么"与"编解码器读到了什么"两个契约之间的缝**，而所有已有测试都站在第一个契约这一侧。

补的门（roost-codegen `scripts/dao-golden-runtime.sh`，ci.yml Linux 一步）：把 dao golden 放进临时模块，钉当前 roost-core pin 与
mongo-driver v2，跑 `internal/dao/testdata/runtime/roundtrip_test.go`——提交文档与回滚快照往返、`dirtyhook` 不入文档、nil 指针元素保留
null。它站在第二个契约那一侧；模板改坏编码形状会在 CI 红，而不是在某个业务的 Mongo 里静默丢字段。仍未做：给 demo 的 Player 加一个嵌套
字段让压测也覆盖（W-2026-09-17-02 接线补上之后一起做更合适）。
