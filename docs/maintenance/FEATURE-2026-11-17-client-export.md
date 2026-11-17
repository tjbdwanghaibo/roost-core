# 前后端公共协议目录（client_export）：需求与维护者决定

状态：**已定，排进 2026-11-17 批次**（维护者 2026-11-17：四个待定项“都按推荐，排进本批”）。实施完把结论写进现行文档（`codegen/docs`、`docs/framework`、skill），本文留作来源。

## 1. 背景与现状（v1.52.0，2026-11-17 核对）

使用方的客户端是 Unity（C#），与服务端是两个仓库。客户端不引用服务端路径，而是把服务端导出的 C# 同步进自己的仓库，用 core 的客户端 SDK（`Roost.Client` / `Roost.Client.Codecs` / `client/unity`）处理。

- 协议唯一源头：`protocol/def/*.go`（带 `pb` 标签的结构体，消息号写在 `//roost:msg id=…`）。
- `roost generate` 产出：
  - `protocol/proto/protocol.proto`：`package roost.protocol`，只有 `go_package`，没有 `csharp_namespace`；
  - `protocol/pb/*.pb.go`、`protocol/msgid/msgid_gen.go`、`protocol/protocol_manifest.json`；
  - `db/dao_sync.proto`（`package roost.dao.sync`），以及 C# 的 `db/dao_sync_apply.cs`。
- C# 消息号表要单独执行 `codegen/cmd/protocol -def ./protocol/def -csharp <文件>`。
- 错误码：`internal/errors` 的 `errcode.Define` 没有客户端产物，`codegen/internal/errcode` 只输出 CSV。
- skill：只在 compose §5A 第 7 项写了手工步骤（`protoc --csharp_out`、`-csharp` 消息号、拷贝 `dao_sync_apply.cs`）。没有固定目录，没有一键命令，也没有版本或哈希可核对。
- 版本没对齐：SDK 锁定 `Google.Protobuf` 3.31.1，而维护者本机的 protoc 是 36.2（libprotoc）。照 skill 生成的 C# 比运行库新，Google 不保证这种组合能用。
- Unity 接入路线有两种说法：`client/README.md` 推荐新业务用 `GameClient`（托管），`client/unity/README.md` 与 compose §5A 第 7 项写的是 `RoostNativeDriver`（原生内核）。

## 2. 要做的

1. **`roost.yaml` 声明导出**：
   ```yaml
   client_export:
     dir: shared/protocol       # 相对工程根，产物提交进仓库
     languages: [csharp]
     csharp:
       namespace: Game.Protocol # 可选
   ```
   `roost generate` 每次整份写出这个目录；同样的输入，输出逐字节相同。
   ```text
   <dir>/
   ├── proto/protocol.proto
   ├── proto/dao_sync.proto
   ├── csharp/Protocol.cs        protoc 生成
   ├── csharp/DaoSync.cs         protoc 生成
   ├── csharp/DaoSyncApply.cs    即 db/dao_sync_apply.cs
   ├── csharp/MessageIds.cs      即 -csharp 的输出
   ├── csharp/ErrorCodes.cs      错误码：名字 → 数字，附说明
   └── manifest.json
   ```
2. **manifest.json** 包含：
   - core 版本、生成器版本、protoc 与 Google.Protobuf 版本；
   - 源头哈希（`protocol/def` 与同步字段），以及每个导出文件的哈希；
   - 消息号清单（名字、号、方向：请求 / 应答 / 推送）与错误码清单。
3. **检查**：`roost generate --check` 覆盖导出目录；`roost project doctor` 新增 `client-export:fresh`。
4. **客户端同步工具（简单版）** `roost client sync --from <服务端工程> --to <客户端目录>`：
   - 把 `csharp/` 拷到客户端目录；
   - 对比两边的 manifest，报告新增和变化的消息号与错误码；
   - 不生成 Unity 的 `.meta`。
5. **skill**（R9 收尾时由协调者改，避免与 R9 冲突）：
   - compose §5A 第 7 项改为引用导出目录；
   - DEMO 第 6、7 节写明导出目录和本轮 manifest 的哈希；
   - §2A：协议或同步字段变了时，HISTORY 写“客户端要重新同步”，并列出新增和修改的消息号、错误码；
   - 统一 Unity 接入路线（见第 3 节第 4 条）。

## 3. 维护者决定（2026-11-17，都按推荐）

1. **C# 生成方式：固定 protoc 版本，由生成器核对**。不在生成器里自己实现 protobuf 的 C# 输出，也不在生成时自动下载 protoc。
   - 声明了 `csharp` 时，要求 `protoc --version` 与 SDK 锁定的 Google.Protobuf 同版本（当前 31.1），版本不对就报错，并给出安装方法。
   - `MessageIds.cs`、`ErrorCodes.cs`、`DaoSyncApply.cs` 由 Go 生成，不依赖 protoc。
2. **导出目录**：缺省名 `shared/protocol`，**不默认开启**。没有声明 `client_export` 的工程，生成结果与现在逐字节相同。
3. **C# 命名空间**：缺省沿用 proto package（`Roost.Protocol`、`Roost.Dao.Sync`），可以配置。
4. **Unity 缺省接入：托管**（`GameClient` + `RoostClientDriver`），直接编译导出的 `csharp/`，不需要 C++ 构建环境。原生 `RoostNativeDriver` 是可选路线：需要原生网络内核，或要与 C++ 客户端行为一致时再用。skill 写清两条路线各适合什么情况。

## 4. 验收

- 新建工程、装模块、手写一个协议，执行 `roost generate` 后：
  - 导出目录齐全；
  - `csharp/` 放进 netstandard2.1 工程，能和 `Roost.Client`、`Roost.Client.Codecs`（Google.Protobuf 3.31.1）一起编译通过。
- 只改 `protocol/def`、不重新生成时，`generate --check` 和 doctor 都报过期。
- 两次生成的结果逐字节相同。
- manifest 里的消息号、错误码与 `msgid_gen.go`、`internal/errors` 一致。
- 没有声明 `client_export` 的工程，生成结果与现在逐字节相同。
- protoc 版本不对时报错，错误里写明需要的版本。
