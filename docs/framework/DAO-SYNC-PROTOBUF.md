# DAO 同步使用 Protobuf

维护者决定：持久化保留 BSON，默认 DAO 同步改用 Protobuf。自 v1.35.0 起默认同步为 PB；v1.34.2 的默认同步仍是 BSON。服务端与客户端必须配套重新生成、更新，不提供旧 BSON 同步双读。

## 边界

- BSON 继续用于 Mongo、WAL 中持久状态、DAO rollback 和 hydration。本次不改变落库或提交确认语义。
- 同步仍按 dirty mask 选字段，由视图白名单进一步裁剪；一个 handler 提交后汇总变化，setter 不逐个发网络消息。
- 默认 DAO packer 输出真正的 PB 字段，不把 BSON 或 JSON 原样装入 PB bytes。Sync frame / SubjectUpdate 的对象、代际、版本、命名空间、视图与恢复协议保持既有职责。
- 初次入场、重连和恢复发送允许视图的全量；普通变化只包含被选中字段。map / slice 目前按整个字段替换，不在本次增加键级补丁。

## 字段存在性

PB 的默认值省略规则不能作为“字段未变化”的依据。生成的 DAO 同步消息对每个顶层字段使用消息包装：包装不存在表示本次不改，包装存在而内部值为默认值表示明确改成零、false、空集合或空引用。客户端不对增量直接使用普通 MergeFrom 合并集合，而按包装存在性替换整个字段。

例如 `SyncPlayerDao.coins` 是一个含 `int64 value` 的消息：`coins == null` 不更新；`coins != null && coins.Value == 0` 清零金币。容器中允许的空嵌套引用同样要明确表示，不能把 nil 元素丢弃导致数组错位。

`roost generate` 从 DAO 定义生成 `db/dao_sync.proto`、`db/gen_dao_sync.go`、`db/dao_sync_apply.cs`。Go 无需 protoc；C# 对 proto 运行 protoc 生成类型后，加入生成的 Apply 文件。存储键与 PB 字段编号分离；同步 schema 与客户端必须配套更新，不加入旧 BSON 双读路径。默认 packer 的编码号为 2；客户端按 namespace/profile/encoding/schema 精确注册。旧客户端不能继续按 BSON 解码。

## 验收

包含标量精度（64 位 ID 不经浮点）、零/false、空 map/slice、嵌套对象、空引用、仅修改一个字段、非同步字段不可见、截断错误不部分应用、正式 generated self_sync、Go/C# 同一包交叉验证，以及字段裁剪与空增量。重新运行现有 DAO runtime 与玩法生成脚本，确认 BSON 持久化、事务 undo 和生成漂移不受影响。

## 客户端接入

```bash
protoc -I db --csharp_out=Client/Generated db/dao_sync.proto
cp db/dao_sync_apply.cs Client/Generated/
```

生成的命名空间为 `Roost.Dao.Sync`，例如 `SyncPlayerDao`。字段编号依据当前 DAO 定义顺序；调整顺序、字段或类型时必须配套更新客户端，不能只更换服务端。当前不维护跨版本字段编号注册表。

```csharp
using Roost.Client.Codecs;
using Roost.Dao.Sync;

client.Entities.RegisterDao("player", SyncPlayerDao.Parser,
    (patch, previous) => patch.Apply(previous));
```

`Apply` 克隆旧状态，再替换本次出现的字段，保证旧快照不被改写；Full 时传入 null。应用层只读这些视图，不自行修改返回的 PB 对象。自定义 Profile 指定对应的 profile/schema；自定义非 DAO packer 使用 `EntityViews.Register` 注册自己的 codec，不能仅凭“编码号相同”混用消息类型。

Go 消费者可 `UnmarshalSyncPlayerDao` 得到有类型的 patch；`.Values()` 是按逻辑字段名读取的便利视图，供 demo 机器人等动态消费者使用。它只列出这次携带的字段，不是累积状态，也不把嵌套对象重新编码成 BSON。内部生成的嵌套 BSONDoc 结构被复用为内存快照进行类型转换，网络路径没有 BSON marshal。

## 已执行验证与边界

DAO runtime 的持久 BSON、事务回滚和 PB patch 回归已通过；同一份 Go 生成 DAO 产生的 PB 金样由 protoc 生成的 C# 类型解码，并验证精度、零值、空集合、空引用与字段替换。金样位于 `client/spec/dao-sync.json`，脚本 `codegen/scripts/dao-golden-runtime.sh` 与 `scripts/test-client-dotnet.sh` 分别验证两端。

没有以本轮验证宣称 PB 一定比 BSON 快：字段包装有存在性开销，嵌套结构目前有内存 DTO 转换，实际 CPU、分配和带宽需同负载对照。没有改成 map 单键增量，没有改变 frame/SubjectUpdate 版本，也没有增加客户端预测或持久格式迁移。

本轮本地回归（2026-10-31）：`go test ./...`、`go vet ./...`、pretag 中 16 个离线生成验证脚本（13 类玩法与 3 个生成器专项）、DAO runtime、.NET 客户端测试及 `git diff --check` 通过。.NET 包含真实 TCP 的心跳、请求超时、重连成功与失败上限回调；Unity 适配器只做源码编译与共享核心验证，未运行 Unity Editor/IL2CPP。本轮不是完整发版验收，也未重跑真实 Mongo 恢复与性能压测。
