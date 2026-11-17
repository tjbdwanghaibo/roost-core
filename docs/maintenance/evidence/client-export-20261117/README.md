# client_export 实施证据（2026-11-17）

需求与维护者决定：[FEATURE-2026-11-17-client-export.md](../../FEATURE-2026-11-17-client-export.md)。现行说明：[codegen 参考 §2.1](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#21-前后端公共协议目录client_export)、[客户端手册](../../../../client/README.md#前后端公共协议目录client_export)、[Unity 手册](../../../../client/unity/README.md#两条接入路线)。

这是功能实施，不是缺陷修复：没有“修前红”，每个新行为有回归用例，另做 8 个变异证明用例能抓到。

## 设计

- **配置**：`roost.yaml` 的 `client_export`（`ClientExportSpec`，`omitempty`）。`dir` 缺省 `shared/protocol`，必须在工程内、不在 `protocol/ db/ game/ internal/ service/ configs/ cmd/ deploy/` 下、不用生成时跳过的目录名；`languages` 只认 `csharp`（为空只导出 proto 与 manifest）；`csharp.namespace` 是点分 C# 标识符；`csharp.protoc` 相对路径按读 `roost.yaml` 时的工程根解析（生成跑在暂存树里，暂存树不保留可执行位）。
- **生成**：`generatorsFor` 只在声明了 `client_export` 时在最后（registry 之后）追加 `client-export` 一步，读本轮刚生成的 `protocol/proto/protocol.proto`、`db/dao_sync.proto`、`db/dao_sync_apply.cs`，解析 `protocol/def`（`protocol.ExportClientIDs`，与 `-csharp` 同一个生成函数 `generateCSharpIDsIn`），扫错误码（`errcode.Definitions` + 新增 `errcode.GenerateCSharp`）。导出目录里归生成器的只有固定的 8 个文件名，本轮不产出的删掉，别的文件不动。
- **protoc**：`sdkGoogleProtobufVersion = "3.31.1"` 是 `Roost.Client.Codecs.csproj` 的副本（`TestSDKGoogleProtobufVersionMatchesCodecs` 读 csproj 核对），protoc 版本由它推出（去掉 `3.` 得 31.1），不另写。查找顺序 `client_export.csharp.protoc` → `ROOST_PROTOC` → PATH；`runGenerators` 在跑任何生成器之前核对 `protoc --version`，不对就报错（写明需要的版本、实际版本、来源与下载地址）。protoc 在临时目录对导出的 proto 副本运行，生成文件头里的 `source:` 是 `protocol.proto` / `dao_sync.proto`，不带临时路径。
- **命名空间**：缺省协议 `Roost.Protocol`、DAO 同步 `Roost.Dao.Sync`（protoc 按 proto package 推出，导出的 proto 不加选项，与服务端那份逐字节相同），消息号与错误码 `Roost.Generated`（`MessageIds.cs` 与 `-csharp` 输出逐字节相同）；配了 namespace：协议、消息号、错误码在 `<ns>`，DAO 同步在 `<ns>.DaoSync`（proto 副本加 `option csharp_namespace`，`DaoSyncApply.cs` 换掉命名空间行）。
- **manifest.json**：`kind: roost-client-export`、`schema: 1`、`core`（工程 go.mod 里 require 的 roost-core）、`generator`（与 `roost version` 同一来源）、`languages`、`protoc` / `google_protobuf`、`csharp_namespaces`、`sources`、`files`、`messages`、`error_codes`。哈希一律 sha256、按 LF 算（CRLF 检出不算变化）。
  - **哈希范围**：`sources.protocol_def` = `protocol/def` 下全部非测试 `.go`，按相对路径排序，路径、长度、内容一起算；`sources.dao_sync` = 同步字段的 schema，即 `db/def` 生成的 `dao_sync.proto`（只由同步字段决定）；`files` = 除 `manifest.json` 外每个导出文件。
  - `messages`：名字、方向（request / response / push）、号、proto 消息名，按号再按方向排序；`error_codes`：码、名字、说明，按码排序。
- **`--check`**：`checkGenerated` 的快照改用 `snapshotGeneratedFor(root, manifest)`，按路径认领导出目录（protoc 的 C# 与 manifest.json 没有统一生成头）；暂存提交 `planStagedProjectCommit` 的两次遍历、`snapshotProjectInputs` 同样按 `ClientExport.owns` 认领，所以导出文件随批次提交、退役的被删、不算业务输入。
- **doctor**：`client-export:fresh`（`checkClientExport`，声明了才有，非 strict 也跑）不跑 protoc：读 manifest、核对每个文件哈希；重算 `protocol/def` 哈希；开了 dao 时用 dao 生成器把 `db/def` 生成到临时目录取 `dao_sync.proto` 算同步字段哈希（不读可能过期的 `db/dao_sync.proto`）；重扫错误码与清单比；`languages` 与 `roost.yaml` 比。
- **`roost client sync --from <服务端工程> --to <客户端目录> [--dry-run]`**：读服务端 `roost.yaml` 找导出目录（要求声明了 csharp），先核对导出文件哈希（手改 / 拷了一半拒绝），把 `csharp/` 平铺拷进 `--to` 并另存 `manifest.json`；对比 `--to` 里上次的 `manifest.json`，打印版本变化与新增 / 变化 / 删除的消息号（方向 + 名字为键）和错误码（名字为键）；上次登记过、这次没有的文件删掉；不是上次同步写的同名文件拒绝覆盖；上次写过又被手改的照样覆盖并提示。不生成 `.meta`。

## 文件

代码：`codegen/internal/roost/client_export.go`（新）、`client_sync.go`（新）、`manifest.go`、`generate.go`、`project.go`、`doctor.go`、`cli.go`、`help.go`；`codegen/internal/protocol/client_export.go`（新）、`csharp.go`；`codegen/internal/errcode/csharp.go`（新）。
用例：`codegen/internal/roost/client_export_test.go`、`codegen/internal/protocol/client_export_test.go`、`codegen/internal/errcode/csharp_test.go`。没有 golden 文件变化。
文档：`codegen/docs/CODEGEN_REFERENCE.zh-CN.md` §2 / §2.1、`codegen/docs/PROJECT_GENERATOR.zh-CN.md` §4 字段表与 §13 doctor 清单、`client/README.md`、`client/unity/README.md`、`CHANGELOG.md`。

## 验收（第 4 节逐条，另加）

| 项 | 结果 | 证据 |
| --- | --- | --- |
| 新建工程、装 wallet bag enter、手写 HelloPing，`roost generate` 后导出目录齐全 | 8 个文件齐全，`go build ./...` 通过 | [acceptance.txt](acceptance.txt) §1、§3 |
| `csharp/` 放进 netstandard2.1 工程，与 `Roost.Client`、`Roost.Client.Codecs`（Google.Protobuf 3.31.1）一起编译 | `dotnet build` 0 警告 0 错误（Nullable enable、TreatWarningsAsErrors、LangVersion 8）；另配 `namespace: Game.Protocol` 再生成、再编译也通过 | §8、§10 |
| 只改 `protocol/def`、不重新生成：`generate --check` 与 doctor 都报过期 | `--check` rc=1 列出导出目录的 3 个文件；doctor `FAIL client-export:fresh … protocol/def changed` | §7 |
| 两次生成逐字节相同 | `--force` 重写后 `diff -r` 无差异，整个工程 `git status` 干净，`--check` 通过 | §4 |
| manifest 的消息号、错误码与 `msgid_gen.go`、`internal/errors` 一致 | 7 个 Msg 常量、12 条消息、12 个错误码完全一致 | §5 |
| 没有声明 `client_export` 的工程生成结果逐字节不变 | 新旧生成器（HEAD 与 HEAD+本改动，都从 `git archive` 的干净树构建）各走 project new → module add wallet bag enter → add protocol → generate → project sync → generate --check，`diff -r -x .git` 无差异（195 个文件） | [same-tree.txt](same-tree.txt)、[same-tree.sh](same-tree.sh) |
| protoc 版本不对时报错，错误里写明需要的版本 | PATH 上的 36.2：在任何生成器之前失败，`needs protoc 31.1 … reports "libprotoc 36.2"; install protoc 31.1 from …/releases/tag/v31.1`，导出目录未写出 | §2、[2-wrong-protoc.log](2-wrong-protoc.log) |
| `roost client sync` | 首次 5 个文件；改协议 / 加错误码 / 改说明后再同步：报 HelloWave 请求与应答新增、`hello_too_many` 新增、`wallet_short` 说明变化 | §9 |

命令：`acceptance.sh <roost> <本目录>`（protoc 31.1 由 `ROOST_PROTOC` 指向，PATH 上是 36.2）；`same-tree.sh <roost> <目录名>` 两次后 `diff -r -x .git same-old same-new`。

单测：

- `GOWORK=off go test -count=1 ./codegen/internal/protocol/ ./codegen/internal/errcode/`：通过。
- `codegen/internal/roost` 新增 8 个用例（`TestSDKGoogleProtobufVersionMatchesCodecs`、`TestClientExportSpecValidation`、`TestUndeclaredClientExportChangesNothing`、`TestGenerateClientExport`、`TestClientExportNamespace`、`TestClientExportProtocVersion`、`TestClientExportTransactionalCommitAndRetirement`、`TestClientSync`），用假的 protoc（shell 脚本）；连同改动涉及的 help / generate / staged / snapshot / doctor / manifest / retire 用例共 166 个：[roost-targeted-tests.txt](roost-targeted-tests.txt)。
- `GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：通过。
- 没跑 codegen 全量、pretag、genre 脚本（按批次规则由主线最后统一跑）。

## 变异

[mutate.py](mutate.py) 在干净副本里逐个改一处关键代码、跑对应用例、再还原，结果在 [mutations.txt](mutations.txt)：

| 变异 | 抓到它的用例 |
| --- | --- |
| M1 `--check` 不认领导出目录 | `TestGenerateClientExport` |
| M2 不核对 protoc 版本 | `TestClientExportProtocVersion` |
| M3 暂存提交不认领导出目录 | `TestClientExportTransactionalCommitAndRetirement` |
| M4 doctor 不比 `protocol/def` 哈希 | `TestGenerateClientExport` |
| M5 client sync 不删上次写过、这次没有的文件 | `TestClientSync` |
| M6 没声明 `client_export` 也跑 client-export | `TestUndeclaredClientExportChangesNothing` |
| M7 清单丢掉应答 | `TestExportClientIDsMatchesCSharpOutput` |
| M8 错误码说明不做 XML 转义 | `TestGenerateCSharp` |

## 拿不准的点

1. **`MessageIds.cs` / `ErrorCodes.cs` 的缺省命名空间**：按“即 `-csharp` 的输出”取 `Roost.Generated`（现有客户端代码 `using Roost.Generated` 不用改），没有并进 `Roost.Protocol`。配了 namespace 时三者合一、DAO 同步另放 `<ns>.DaoSync`（避免 DAO 的 `Sync*` 类型与协议消息撞名）。
2. **manifest 带生成器版本**：换了生成器版本（包括本地每次构建的伪版本），`--check` 会因 `manifest.json` 变化报过期；`versions.core: latest` 的工程每次 core 发版后要重新生成一次导出目录。
3. **错误码只含工程自己的 `errcode.Define`**：框架段 1–999 不在 `ErrorCodes.cs` 里。
4. **生成工程里的 `docs/ROOST_YAML.zh-CN.md` 没写 `client_export`**：那份是所有工程都会生成的模板，改它会让没声明的工程生成结果变化，与“逐字节不变”冲突；字段说明在 codegen 文档与 `roost help client`。
5. **去掉 `client_export` 声明后**，导出目录不再归生成器，旧文件留在原处，要手工删。
6. **Unity 托管路线的打包**：业务工程不需要原生插件，但现有 `scripts/client/build-unity.py` 打包时仍一起构建原生插件（要 CMake / C++）。只要托管 DLL 的打包入口本轮没做。
7. Unity Editor / Player 没有实跑；C# 只在 .NET SDK 10 的 netstandard2.1 工程里编译验证。

## 给 skill 的建议文字（R9 收尾时由协调者改 `docs/agent-skills/**`）

**compose §5A 第 7 项“Unity 客户端接入”整段替换为：**

> - **两条路线，缺省托管**：`GameClient` + `RoostClientDriver`（常驻 GameObject 上挂 `RoostClientDriver`，`Attach(client)` 后它每帧在主线程 Pump），网络在 C# 里，直接编译导出的协议代码，业务工程不需要原生插件与 C++ 构建环境。只有要原生网络内核、或要与 C++ 客户端行为一致时才走 `RoostNativeDriver` + `NativeGameClient`（要按平台打原生包，见 roost-core 的 [client/unity/README](../../../../client/unity/README.md)）。同一连接只选一条。
> - **协议代码从导出目录来**：工程 `roost.yaml` 声明 `client_export`（`dir: shared/protocol`、`languages: [csharp]`），`roost generate` 把 `Protocol.cs`、`DaoSync.cs`、`DaoSyncApply.cs`、`MessageIds.cs`、`ErrorCodes.cs` 与 `manifest.json` 写进 `shared/protocol/` 并提交。生成要 protoc 31.1（与 SDK 锁定的 Google.Protobuf 3.31.1 同版本），用 `ROOST_PROTOC` 或 `client_export.csharp.protoc` 指过去；版本不对 generate 会报错。客户端仓库用 `roost client sync --from <服务端工程> --to <Unity 目录>` 同步，它会报告新增和变化的消息号、错误码。不要再手跑 protoc 或 `-csharp`。
> - **SDK**：引用 `Roost.Client`、`Roost.Client.Codecs` 与 Google.Protobuf 3.31.1（游戏工程自己锁版本），Unity 项目选 .NET Standard 2.1。
> - **最小接线（托管）**：先异步完成登录入口取票（第 3A 步）；`new GameClient(new ConnectionOptions{Host, Port})`，注册业务 PB 的回复 / 推送（`client.Request(MessageIds.RequestX, MessageIds.ResponseX, req, Reply.Parser, …)`、`client.Subscribe(MessageIds.PushY, Push.Parser, …)`）与自身视图 `client.Entities.RegisterDao("player", SyncPlayerDao.Parser, (patch, previous) => patch.Apply(previous))`；`driver.Attach(client)`，`await client.Connection.ConnectAsync(取票)`。地址是第 1A 步定下的入口：嵌入 TCP 时连 game，独立 Gate 时连 Gate。错误码用 `ErrorCodes.Name(code)` / `ErrorCodes.Message(code)` 查。
> - **边界**：重连不自动重发业务请求（超时可能已经执行，按请求键幂等重发）；回到前台要重新取票登录；完整说明见 roost-core 的 [client/README](../../../../client/README.md)。

**DEMO 第 6 节“协议与错误码”末尾加一句：**

> 这些协议与错误码的客户端代码在导出目录 `shared/protocol/`（`roost.yaml` 的 `client_export`）；写明本轮 `shared/protocol/manifest.json` 的 `sources.protocol_def`、`sources.dao_sync` 两个哈希与 `generator` 版本，客户端据此判断是否需要重新同步。

**DEMO 第 7 节“客户端对接”开头加一句：**

> 客户端代码从导出目录 `shared/protocol/csharp/` 同步（`roost client sync --from <服务端工程> --to <客户端目录>`），不要手写或复制 PB 类型；本轮导出的 manifest 哈希见第 6 节。

**§2A 第 6 步“更新记忆”后加一条：**

> 7. **协议或同步字段变了，通知客户端**：这一轮改了 `protocol/def`、DAO 的 `sync` 字段或错误码时，先 `roost generate`（导出目录随之更新，`roost project doctor` 的 `client-export:fresh` 为 OK），在 `docs/DEMO-HISTORY.zh-CN.md` 本轮一节写“客户端要重新同步”，列出新增和修改的消息号与错误码（可直接贴 `roost client sync --dry-run` 的输出），以及新的 manifest 哈希。

**§2 新工程**：建完工程后在 `roost.yaml` 末尾加 `client_export:` 段（客户端是 Unity / C# 时），并在第 5 步验收里加 `roost project doctor` 的 `client-export:fresh` 为 OK。
