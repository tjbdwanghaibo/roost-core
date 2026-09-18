# ARCH-06：生成的同步字段词汇表

- 单元：M-12（架构 / 能力补齐，不占 U 编号，不计缺陷）
- 仓库：roost-core `dataengine` + roost-codegen `internal/dao`
- 来源：W-2026-09-18-02 经 09-18 第三轮分流转 ARCH-06

## 要解决的事

生成的每字段掩码常量（`heroDaoFieldLevel` 一类）是 DAO 包私有的。默认 packer 把掩码原样交给
`MarshalSync(mask)`，**没有功能缺陷**——掩码不透明地进、不透明地出。但凡是要把状态打成自己的客户端协议的项目
（多数项目都要）就必须知道哪个 bit 是哪个字段，而现在拿不到。

审查的判定：不是 bug，是缺一份**稳定的字段元数据**；且**不要直接导出内部常量**，那等于承诺一个跨版本的 ABI。

## 做法

- roost-core `dataengine`：`SyncFieldMeta{Name, WireName, Bit}` 与两个只读助手 `SyncFieldByName` / `SyncFieldsOf`。
  类型放在框架侧，于是所有 DAO 的表是同一个类型，一个通用消费者可以跨 DAO 工作（与 attribute 的 `Meta` 同形）。
- roost-codegen `internal/dao`：每个有同步字段的 DAO 生成 `<Dao>SyncFields()` 与同名方法。
- **稳定性写在文档注释里而不是暗示**：bit 只在同一份生成产物内稳定；任何跨越构建的东西（落库的投影、客户端缓存的布局）
  按 `Name` 或 `WireName` 键，不按 bit。

## 为什么这样测

文本比对证明不了这份词汇表**正确**——表和 setter 都从同一个模板来，一起写错也一起绿。
`internal/dao/testdata/runtime/syncfields_test.go`（dao 运行时门）改一个字段、读**真正的**脏掩码、再和表对照：

- 每个 bit 非零且互不相同（重复条目比没有更糟：消费者会自信地指错字段）；
- 设置 `Name` 之后，`SyncFieldsOf(mask)` 恰好只报 `Name`；
- 两种拼写都能查到，且查到的 bit 等于实测掩码；
- 该掩码确实能让 `MarshalSync` 产出载荷——这是"WireName 能用来定位载荷"的依据。

## 未做 / 边界

- **demo 没有使用它**：demo 的 packer 仍然把掩码原样交给 `MarshalSync`，因为它的载荷就是 DAO 的同步文档。
  第一个真正需要的使用方是"自己的客户端协议"，那不在 demo 的范围内——所以这份能力目前**只有测试在用**。
- 没有生成"按 bit 反查"的索引（表是切片，线性查找）。字段数是个位数量级，先不做。
- 嵌套结构（`internal/dao/template_nested.go`）没有同样的表：它的字段掩码是另一套，且当前没有使用方提出需要。
