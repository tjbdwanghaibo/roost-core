# DAO 同步使用 Protobuf

客户端 DAO 同步使用生成的类型化 PB；持久化、WAL、回滚和 hydration 继续使用 BSON。setter 只记录变化，Nest 成功提交后汇总同步，网络发送在 Entity 锁外，不是每次 setter 都发包。

## 从 setter 到同步

默认生成的 `self_sync` packer 使用变化树，不再编码、解码两份完整快照后逐项比较。现有 DataEngine / Entity / codegen 包内实现，不新增包或另一条调度链路。

1. 生成 setter 记录字段号，以及必要的 map key / list index。嵌套对象的 `DirtyHook` 将相对路径递归传到所属 DAO。
2. 同一路径多次写入合并，冻结时读取最终值；整体替换覆盖此前子变化。删除后又插入同一个 key，发送最终存在的值。
3. 默认 packer 先按 profile 白名单过滤顶层 mask，再读取相应变化路径并编码。只处理变化的元素和子字段；初次入场、重连、恢复及 Full dirty 仍发送授权视图全量。
4. `PreparedSubjectSync` 保存捕获版本。内容成功结算才确认该版本；失败、Abort 不确认。之后发生的写入保留到后续批次，旧冻结字节不随新写入改变。
5. 变化树随 Tracker 快照参加事务回滚。确认水位不回退，避免回滚把已结算的旧变化重新带回来。

每 DAO 的变化树最多 4096 个节点（不是字节数）；超过预算收敛为整体替换标记，后续继续安全记录。未同步的 DAO 也有此上限，但它不是全服务器内存预算。树只存路径与版本，不存历史业务值；事务快照会复制变化树，成本随未结算的变化量增长；编码时持元数据锁只读遍历，编码结果字节才是冻结内容。

## PB 表示

字段包装缺失表示不修改；存在且为零、空或 false 表示明确赋值。不要用 PB `MergeFrom` 代替生成 Apply。

| 字段 | 编码和应用 |
| --- | --- |
| 标量、`[]byte` | `value` 整体替换 |
| 嵌套结构体 | `value` 整体替换，或 `patch` 携带有变化的子字段 |
| map，`delta=false` | `value` 整体替换，包括清空 |
| map，`delta=true` | `removed` 删除 key，`value` 新增/覆盖元素，`patches` 递归修改既有嵌套元素 |
| list，`delta=false` | `value` 整体替换，包括清空 |
| list，`delta=true` | `updates[index]` 替换/追加元素，`patches[index]` 递归修改嵌套元素，`length` 表示最终长度 |

例如修改 `Equips[2].Gems[3].Level`，编码为 `Equips.patches[2].Gems.patches[3].Level.value`，不携带装备与宝石的其他字段。仍然是从 DAO 生成的强类型消息，不是 JSON 路径或通用值字典。nil 指针由原来的 optional 包装表示；nil 到对象、对象到 nil 都发送完整元素替换。

列表追加必须补齐所有新增索引；客户端拒绝越界、缺洞、缺失嵌套基线等非法操作，并原子发布新视图。EntitySync 外层负责版本顺序和断档恢复，补丁不能跳过外层顺序检查直接应用。

**密集变化回退**：显式 `SetAll`、整体 setter、清空、变化节点达到当前集合元素数，或元数据超预算时使用完整替换。稀疏变化不再为了比较 PB 大小而扫描、编码整个集合，因此不保证每份补丁都比完整字段更小；这是降低 CPU 的明确取舍。没有固定网络压缩比保证。

## 业务写法

保持通过生成 setter 修改，不能绕开访问器写容器内部数据：

```go
// DAO 顶层 map：现有 API。
dao.SetItems(itemID, count)
dao.DelItems(itemID)

// list 按现有索引更新，越界返回 false；Add 追加，SetAll 表示整体替换。
dao.SetFriendsAt(3, friendID)
dao.AddFriends(friendID)

// 嵌套结构体继续直接调用子 setter。
equip, _ := dao.GetEquips(equipID)
gem, _ := equip.GetGems(gemID)
gem.SetLevel(10)

// 嵌套 map 的 SetX 原本表示整体赋值，所以按 key 操作使用明确的 Key 后缀。
equip.SetGemsKey(gemID, gem)
equip.DelGemsKey(gemID)
equip.SetRunesAt(0, gem)
```

同一嵌套对象只能属于一个位置，不能把已绑定的对象再放到其他 key/index；用新对象或明确转移所有权。移除/替换后旧对象不再向原父节点报告变化。插入中间、删除中间、排序或整体重排用 `SetAll`（嵌套列表用 `SetX`）发送替换，不生成相对 splice；索引不会因补丁重试产生二次移动。

直接 `MarshalSync(mask)` 继续发送字段替换。**实体 ID 只在全量里**（2026-11-10）：`MarshalSync` 带 `entity_id`，给全量（`PackSubjectSnapshot`）用；增量用 `MarshalSyncDelta(mask)`（字段替换）或 `MarshalSyncChanges(mask)`（变化树），二者都不带 `entity_id`。生成的 `ApplySync*`（Go / C#）在增量上把缺省的 0 当作沿用基线的 ID，带了与基线不同的 ID 照旧拒绝；`Values()` 在没有 ID 的增量上不给 `_id`。依据：EntitySync 信封 v3 的增量本来就从同一对象引用的全量继承 subject ID（[ENTITY_SYNC 线格式](ENTITY_SYNC.md#线格式帧协议-2--信封-v3)），补丁里再带一遍是重复的 2～10 字节。自定义 packer 的增量若仍用 `MarshalSync` 也照样能被应用，只是多带 ID。`MarshalSyncChanges(mask)` 读取当前变化记录但不消费，须在 Entity 锁保护下使用。默认 `self_sync` 已接入；自定义 packer 若要使用该能力，应在 `PackSubjectDelta` 调用它、在 Snapshot 调用完整编码，并实现 `entity.SubjectSyncChangePacker` 委托同一 DAO 的版本获取和确认。多 DAO 自定义 packer需要自行管理各 DAO 捕获版本，不能共用一个未经映射的序号。未接入的 packer 仍使用字段替换。

## 客户端接入

`roost generate` 生成 `db/dao_sync.proto`、`db/gen_dao_sync.go`、`db/dao_sync_apply.cs`。Go 无需 protoc；C# / C++ 使用同一 proto。

```bash
protoc -I db --csharp_out=Client/Generated db/dao_sync.proto
cp db/dao_sync_apply.cs Client/Generated/
```

```csharp
client.Entities.RegisterDao("player", SyncPlayerDao.Parser,
    (patch, previous) => patch.Apply(previous));
```

Go 用 `ApplySyncPlayerDao(patch, previous)` 返回新的累积视图，Full 时传 nil，也可对镜像 DAO 调用 `ApplySync(raw)`。C++ 使用 `roost::apply_dao_patch` 或 `entity_views.register_dao`；C ABI 共用实现。三端均先应用到暂存状态，全部成功后才发布。Go 返回视图可能共享未变化字段，须按只读使用。

`.Values()` 返回 `(map[string]any,error)`，只接受字段替换或应用后的快照；原始递归/集合补丁必须先 Apply。64 位整数、nil 元素和零值均由 PB 保留。

## 升级与验证

DAO `sync_format=3`（字段号 2047）；EntitySync 外层编码号仍是 2，外层帧/内容版本机制不变。Go/C#/C++ 拒绝旧格式。服务端、proto、生成 Apply 和客户端 SDK 需配套重生成，停止旧连接后重连拿全量。BSON/WAL 格式不变，不需要清库。字段号仍依据定义顺序，不提供旧 schema 兼容。

回归覆盖嵌套结构体、map 内 map、list 内对象、按索引修改、清空/密集回退、失败回滚、混合按索引与整体赋值、冻结/确认交错和预算回退。Go 金样在 `client/spec/dao-sync.json`、`dao-collections.json`，供 C# / C++ 实际解码应用。正式生成工程另以真实 TCP → Nest → DAO → Sync 验证失败 handler 与重连，存储用替身。

复跑：`codegen/scripts/dao-golden-runtime.sh`（设置 `ROOST_CORE_REPLACE`）、`scripts/test-client-dotnet.sh`、`scripts/test-client-cpp.sh`（启用 Protobuf）、`go test ./codegen/internal/roost -run TestSelfSyncGeneratedProjectOverRealTCP -count=1`。性能口径见[微基准](../performance/DAO-COLLECTION-SYNC.md)。本机验证不代表 Linux、Unity Editor、IL2CPP 或移动设备实跑。
