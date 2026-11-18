# H5 用 TypeScript 客户端连玩家接入：方案

**状态**：**方案，待维护者决定**（维护者 2026-11-18 要求：非 Unity 的 H5——Cocos Creator、Laya、纯网页——要有一套 TS 客户端，经 WebSocket 连 roost 游戏服，能力对齐 C# 托管客户端；第一版只做浏览器，传输可替换，以后加微信、抖音小游戏时不改上层）。本文只出方案，没有写实现。基线 main `0a9976b8`。第 10 节的决定点定下后再动手；实施完把结论写进现行文档（`client/README.md`、`client/ts/README.md`、`docs/framework/GATEWAY.md` §6、`codegen/docs`、skill），本文留作来源。

## 1. 背景与现状（2026-11-18 核对代码）

- **C# 已覆盖 Unity（含 WebGL）**：`client/dotnet/Roost.Client` 经可替换传输 `IGateTransport`（`GateTransport.cs`）连嵌入 TCP 或独立 Gate 的 WebSocket 监听，WebGL 上靠 `ConnectionOptions.PumpDriven`（`PumpScheduler.cs`）单线程调度。证据见 [csharp-gate-ws-20261118](evidence/csharp-gate-ws-20261118/README.md)，用法见 [client/README“C# 经 WebSocket 连玩家接入”](../../client/README.md#c-经-websocket-连玩家接入含-unity-webgl)。
- **TS 只连 relay**：`client/ts` 是 `@roost/relay-client`（`package.json`：`private: true`，运行时零依赖，`ws` 只在 node 测试里用）。帧编解码在 `src/codec.ts`，只认 relay 规则（`validate` 允许 flags bits3..4 的通道位，Gate 帧要求 bits3..7 全 0，两者不同）。浏览器 WebSocket 抽象是 `src/client.ts` 的 `WebSocketLike` 与 `RelayClientOptions.webSocket` 工厂。GATEWAY §6 与 KNOWN-LIMITS 都写着“TS 只有开房间 SDK，没有连玩家接入的客户端”。
- **skill 的客户端表**把“网页（TS / JS）连 game”写成**不支持**（`docs/agent-skills/roost-genre/references/compose.md` 第 278 行）。TS SDK 的交付方式是“把同版本的 `$CORE/client/ts` 拷出来再 `npm ci && npm run build`”（同一行）。
- **协议导出只有 C#**：`codegen/internal/roost/client_export.go` 的 `validate` 只接受 `languages: [csharp]`，`buildClientExport` 只在 `spec.csharp()` 时跑 protoc 和写 `csharp/*.cs`。`roost client sync`（`client_sync.go` 的 `planProtocolSync`）只拷 `csharp/` 前缀的文件，没声明 csharp 时报错。配置导出（`client_config_export.go`、`client_config_csharp*.go`）同样只有 C#。`roost help client` 的说明在 `help.go` 第 376～399 行。
- **服务端契约**：
  - [GATEWAY §6](../framework/GATEWAY.md#6-websocket-接入)：每条二进制消息一个 RS v2 帧，服务端按字节流读，写出的每帧恰好一条消息。文本消息以 1003 关闭。不用 WebSocket ping，保活用 RS 心跳（MsgID=0）。**带 `Origin` 的请求必须匹配 `allowed_origins`，白名单为空时拒绝所有浏览器**。
  - RS v2 包头：`client/wire/packet.go`，金样 `client/spec/packets.json`（`pb`、`sync`、`auth`、`auth_generation` 等 12 条）。
  - 鉴权首包的连接代号信封：`client/wire/auth.go`，规则见 [client/README“鉴权首包的连接代号”](../../client/README.md#鉴权首包的连接代号)。
- **WebSocket 地址**：维护者 2026-11-18 定由 portal 进服请求带 `"transport":"ws"` 返回完整 URL（client/README 第 264 行）。**main 上 portal 还没有这一项**：`modules/portal`、`modules/login`、`modules/enter` 里搜不到 `transport` 的进服字段，KNOWN-LIMITS 写的是“portal 一侧另行实现”。TS 与 C# 一样只认调用方给的完整 URL，不受影响；端到端登录演练要等它。

## 2. 范围对照：C# `Roost.Client` 有什么，TS 第一版做什么

“轮”指第 9 节的分轮。理由一栏引用的都是 C# 源码（`client/dotnet/Roost.Client/` 下）。

| C# 能力 | 位置 | TS | 理由 |
|---|---|---|---|
| RS v2 包编解码、按字节流拼帧 | `Packet.cs`（`PacketCodec.Encode` / `ReadAsync`） | **第 1 轮** | 一切的基础，金样 `packets.json` 现成 |
| 鉴权首包与连接代号 | `AuthPayload.cs`（`Encode`、`NextConnectionGeneration` = max(上一个+1, Unix 纳秒)） | **第 1 轮** | 不带代号时跨 Gate 退回“后完成者胜出”（KNOWN-LIMITS 第 317 行）。浏览器只有毫秒，按 README 用 `Unix 毫秒 × 1000000`，代号用 `bigint` |
| 可替换传输 | `GateTransport.cs`（`IGateTransport`、`TcpGateTransport`、`WebSocketGateTransport`） | **第 1 轮**：接口 + 浏览器 `WebSocket` 实现；**以后**：微信、抖音 | 浏览器没有 TCP，只做 WebSocket；接口形状见 §7.1 |
| 会话：请求序列、在途上限、推送分流、晚应答丢弃、写失败关连接 | `TcpSession.cs`（`SendAsync`、`ReadLoopAsync`） | **第 1 轮** | 单线程，不需要发送锁与 `SemaphoreSlim`；序列在发送时分配，顺序就是线上顺序 |
| 连接：取票、连接超时、心跳（缺省 15s，心跳独占一个槽位）、心跳超时、自动重连（退避加抖动、每次重新取票）、`ReconnectResult`、`ConnectionVersion`、`HeartbeatRoundTrips` | `Connection.cs`、`ConnectionOptions.cs`、`ReconnectResult.cs` | **第 1 轮** | 不心跳就被 `idle_timeout`（缺省 90s）断开；重连语义与 C# 相同（不自动重发业务请求） |
| 业务入口：`Subscribe`、`Request`（回调）、`Error` / `StateChanged` / `Reconnected` / `UnhandledPush`、多播逐个通知与销毁边界 | `GameClient.cs` | **第 1 轮** | 回调时机改成事件循环，不要 `Pump`（§7.3） |
| `PumpDriven` 单线程调度 | `PumpScheduler.cs` | **不做** | 浏览器本来就单线程，`setTimeout` 与 WebSocket 事件都在主线程；C# 要它是因为 WebGL 上线程池与计时器不运行（证据 §1） |
| 校时：`ProbeClock`（MsgID=0、载荷 `RCN1`、应答 20 字节）、`NetworkClock` | `Connection.ProbeClockAsync`、`NetworkClock.cs` | **第 1 轮** | 90 行纯算法，服务端已有（`TestAuthenticatedClockProbe` 在 TCP 与 WebSocket 上各跑一遍，GATEWAY §6 回归）；本地时钟用 `performance.now()` 秒 |
| Protobuf 便利重载（按生成类型收发、`RegisterDao`） | `Roost.Client.Codecs/ProtobufExtensions.cs` | **第 1 轮**（请求、推送）；**第 2 轮**（`registerDao`） | 对应 TS 的 `@roost/client/protobuf` 子路径，见 §3、§7 |
| EntitySync：`SyncFrame` / `SubjectUpdate` 解码、`SyncReceiver` 基线、`EntityViews`（Full / Delta / Remove、代际、版本、命名空间与 profile 路由）、`EntityChanged` / `EntitiesChanged` / `EntitiesReset` | `SyncFrame.cs`、`SyncReceiver.cs`、`EntityViews.cs`、`GameClient.Pump` 的 Sync 分支 | **第 2 轮** | 自身视图同步要它；§5 |
| 生成的 DAO Apply | `codegen/internal/dao/sync_apply_csharp.go` 生成的 `dao_sync_apply.cs` | **第 2 轮**（Go 生成 TS 版） | §5 |
| `SnapshotBuffer`（表现插值）、`PredictionHistory`（输入预测）、`ClientTrace` | 同名文件 | **第 3 轮** | 纯算法共约 200 行，[网络体验接入](../framework/NETWORK-EXPERIENCE.md) 是按业务显式启用的，不挡连入 |
| 场景时间域 `SceneTimeDomain` | `TimeDomain.cs`（79 行） | **第 3 轮** | 纯公式；要等场景同步下来的锚点，所以排在 EntitySync 之后 |
| Lockstep：命令编解码、`LockstepClient`、追帧、快照恢复（C7 / C9 / CA） | `LockstepClient.cs`、`LockstepCodec.cs`（共 537 行） | **以后做** | 体量大、有确定性模拟的前提；H5 帧同步需求没出现。到时用 `packets.json` 的 lockstep 金样逐字节对齐 |
| 地形 `Roost.Client.Terrain` | `Terrain/*.cs` | **不做** | 只为 Unity mesh 设计（包解压、网格、岸线），H5 没有需求 |
| Unity 适配（`RoostClientDriver`、`RoostConnection`、`RoostSceneTime`） | `client/unity/Runtime` | **不适用** | 引擎胶水 |
| 原生 C++ 路线 | `client/cpp` | **不适用** | |

另外两条 C# 有、TS 也要照做的约束：

- **票据**：1～8182 字节 UTF-8，不能以 `0x00` 开头（`TcpSession.ConnectAsync`，服务端 `MaxHandshakeBytes` 8 KiB 减 10 字节信封）。
- **容量**：在途请求上限（缺省 64）、单包上限（缺省 1 MiB，与服务端 `max_payload_bytes` 一致）。C# 的推送队列上限（`MaxQueuedPushes`）在 TS 里没有对应物：推送收到就派发，不排队（§7.3）。

## 3. 包与目录

### 3.1 和现有 relay 包合一还是分开

可行做法：

- **A. 合成一个包 `@roost/client`**：子路径 `@roost/client/gate`、`@roost/client/relay`、`@roost/client/web`（`WebHost`）、`@roost/client/protobuf`，根路径导出全部。共用的帧层放 `src/wire/`。
  - `package.json` 的 `exports` 声明子路径，打包器按子路径与 tree-shaking 去掉不用的部分。
  - 代价：包改名，已经用 `@roost/relay-client` 的工程要改 import。线上尚未部署（维护者 2026-10-06 规则），不做兼容；CHANGELOG 写“改 import”。
- **B. 保留 `@roost/relay-client`，在里面加 gate**：不改名。名字从此名不副实。
- **C. npm workspaces 分三个包**（`wire`、`relay`、`gate`）：边界最清楚，但拷源码构建的使用方要处理三个包之间的本地依赖，交付变复杂。

**推荐 A**。理由：

- 共用帧层只有一份。
- 交付仍是一个目录、一次构建。
- 与 C# 的划分对应：零依赖的核心对应 `Roost.Client`，`protobuf` 子路径对应 `Roost.Client.Codecs`。

**共用帧层怎么放**：

- 新增 `src/wire/header.ts`，放 16 字节包头的读写、`HEADER_SIZE` / `VERSION` / 最大载荷常量，以及按字节流拼帧的 `FrameReader`。`FrameReader` 收一个校验函数，在等载荷之前调用（与现在 relay 的做法相同）。
- 两种规则各自一个校验函数：
  - Gate：bits3..7 为 0、类型 3 保留、MsgID=0 时 flags 为 0（同 `PacketCodec.CheckHeader` 与 `client/wire/packet.go`）；
  - relay：沿用 `codec.ts` 现在的 `validate`。
- `codec.ts` 改成建在它上面，对外 API 不变。relay 的 27 条金样（`test/golden.test.ts`）保证改动没有带坏它。
- Gate 的包类型与鉴权信封放 `src/gate/packet.ts`、`src/gate/auth.ts`。

目录草图（A）：

```text
client/ts/
├── package.json            name @roost/client；exports: ".", "./gate", "./relay", "./web", "./protobuf"
├── src/wire/header.ts      共用：包头、FrameReader
├── src/relay/…             现 codec.ts、client.ts、lobby.ts、compress.ts（搬目录，API 不变）
├── src/web/…               现 web.ts（WebHost）
├── src/gate/packet.ts      Gate 包与校验
├── src/gate/auth.ts        鉴权信封、连接代号
├── src/gate/transport.ts   GateTransport 接口 + BrowserWebSocketTransport
├── src/gate/session.ts     会话（对应 TcpSession）
├── src/gate/connection.ts  连接（对应 Connection）
├── src/gate/game-client.ts 业务入口（对应 GameClient）
├── src/gate/clock.ts       ClockSample、NetworkClock
├── src/gate/sync/…         第 2 轮：SyncFrame、SubjectUpdate、SyncReceiver、EntityViews
├── src/protobuf/index.ts   可选：依赖 @bufbuild/protobuf（peer），按 Schema 收发、registerDao
└── test/…
```

### 3.2 怎么给游戏工程用

`@roost/relay-client` 现在不在 npm 上，skill 的做法是拷源码构建（compose.md 第 278、665 行）。可行做法：

1. **拷源码构建，`file:` 依赖或 `npm pack` 包**：
   - 把同版本的 `$CORE/client/ts` 拷出来，`npm ci && npm run build`，得到 `dist/`（ESM + `.d.ts`）。
   - 再 `npm pack` 出 `roost-client-<版本>.tgz`，游戏工程 `npm i ./roost-client-<版本>.tgz`（或 `"@roost/client": "file:…"`）。
   - Cocos Creator 3.x、Vite / webpack 的网页都按普通 npm 依赖用。
   - 运行库 `@bufbuild/protobuf` 由游戏工程按锁定的精确版本自己装（§4.4），与 C# 里游戏工程自己管 Google.Protobuf 相同。
2. **单文件 ESM**：
   - 构建时多出一个 `roost-client.mjs`（esbuild 打包，`@bufbuild/protobuf` 作外部依赖不打进去）。
   - 适合不用打包器的纯网页：配一个 import map 指向 `@bufbuild/protobuf` 的 ESM 文件。
   - 代价：多一个 devDependency（esbuild）。`.d.ts` 合成一个文件还要再加工具（如 dts-bundle-generator），否则仍是一个目录。
3. **发 npm（公开或私有 registry）**：最省事，但要账号、发布流程与版本策略，属于产品选择，第一版不做。

**推荐 1**，作为 skill 里写的正式方式。2 等有不用打包器的使用方再加，不进第一版。

**protobuf-es 方案下的取舍**：

- 不打包时，导出的 `*_pb.ts` 与 SDK 引用的是游戏工程 `node_modules` 里的同一份 `@bufbuild/protobuf`，类型和运行时只有一份。
- 单文件把它打进去，`*_pb.ts` 就找不到它；所以要打也只能把它作外部依赖。

## 4. 协议导出（TS）

### 4.1 PB 生成选型

roost 的协议特征决定了要看什么：

- **用到了 oneof、enum、map、嵌套**：`protocol/def` 支持 `oneof` 标签与 enum（`codegen/internal/protocol/util.go` 的 `parseProtoTagOptions`、`reverse.go`）。
- **大量 64 位整数**：
  - Go 的 `int` 映射成 `int64`（`codegen/internal/dao/sync_proto.go` 的 `expression`）；
  - 实体 ID 金样是 9007199254740993，超过 2^53（`client/dotnet/Roost.Client.Tests/DaoProtobufTests.cs`）。
- **DAO 补丁有专门的存在性要求**：
  - 补丁靠“包装消息在不在”表示字段有没有改（`SyncXxxField{value}`，`client/spec/dao-sync.json` 的 `zero_empty` 就是值为零、包装在的情形）；
  - 列表补丁用 `map<uint64, …> updates`（`codegen/internal/dao/testdata/golden/dao_sync.proto`）。

| | protobuf-es v2（`@bufbuild/protobuf` + `@bufbuild/protoc-gen-es`） | ts-proto（`protoc-gen-ts_proto`） | protobufjs v7（`protobufjs` + `protobufjs-cli`） |
|---|---|---|---|
| 运行时依赖 | `@bufbuild/protobuf`，本身零依赖 | 2.x 起生成码引用 `@bufbuild/protobuf/wire`（读写器）；可选 `long` | `protobufjs/minimal`；int64 要 `long` |
| 生成方式 | protoc 插件（node），可走 `protoc --plugin` | protoc 插件（node） | `pbjs` / `pbts` 自己解析 .proto，**不经 protoc** |
| 生成物 | TS 源码：消息类型 + `XxxSchema`，用 `create` / `toBinary` / `fromBinary` / `clone` / `equals` 操作普通对象 | 每个消息一组 `encode` / `decode` / `fromJSON` 函数，生成码较长 | 静态 JS + `.d.ts`，或运行时反射（体积更大） |
| int64 | 缺省 `bigint`，精确；没有 BigInt 的环境退回字符串 | 缺省 `number`，超过 2^53 时生成码抛错；要设 `forceLong=bigint` | `Long` 对象；没装 `long` 时退成 `number`，丢精度 |
| 存在性 | 消息字段缺省为 `undefined`，在即编码（含空消息） | 同左 | `null` / `undefined`，同样保留 |
| 64 位 map 键 | 字符串键（JS 对象键只能是字符串） | 视选项为 `Map` 或对象 | 字符串键 |
| 标准一致性 | 自称完整通过 protobuf 官方 conformance 测试 | 未声明完整通过 | 有已知偏差，维护活跃度低 |
| 浏览器兼容 | ES2017 + BigInt；要 `TextEncoder` / `TextDecoder`，没有的环境可在运行库里换实现 | 同样要 BigInt（选 bigint 时） | 不要 BigInt；Cocos / 微信小游戏历史上用得最多 |
| 与 C#（Google.Protobuf 3.31.1）一致 | 都是标准 proto3 二进制，按金样核对即可 | 同左 | 同左 |
| 包体 | 运行库按反射编解码，生成码小；**本机未实测**，第一轮在 game-demo 的协议上用 esbuild 量 min+gzip，写进证据 | 运行库小、生成码大；同样待测 | minimal 运行库最小；同样待测 |

表中依赖、生成方式、int64、存在性这几列来自各库文档与已知用法，**没有在本机装包核实**：本轮只出方案，没有下载新包。第一轮开工的第一步是装三套，在 `codegen/internal/dao/testdata/golden/dao_sync.proto` 与 game-demo 的 `protocol.proto` 上各生成一次，做三件事：

- 跑 `dao-sync.json` / `dao-collections.json` 解码；
- 核对 int64 与存在性；
- 量包体。

结果与上表不符时回来改选型。

**推荐 protobuf-es v2**。理由：

1. 64 位整数缺省就精确，与 C# 的 `long` 语义相同。另两个都要额外选项，选错了会静默丢精度（protobufjs）或运行时抛错（ts-proto）。
2. 走 protoc 插件，能沿用 C# 那套“protoc 精确锁定”（§4.4）。protobufjs 自带解析器，绕开了这把锁。
3. 运行时零依赖、TS 原生类型、ESM，标准一致性最好。

代价：

- 运行环境要有 BigInt。Chrome 67、Safari 14 起都有；小游戏在 iOS 14 以下没有，到小游戏那一轮再核实。
- `bigint` 字段业务算术要写 `BigInt(…)`（§10 D5）。

**不推荐的第四条路：Go 生成器自己输出 TS 编解码**。

- 好处：服务端不需要 node。
- 但 roost 协议要覆盖 oneof、enum、map、嵌套，等于自己再维护一个 protobuf 实现。
- 维护者给 C# 定的原则是“不在生成器里自己实现 protobuf 的 C# 输出”（[client-export 决定 1](FEATURE-2026-11-17-client-export.md#3-维护者决定2026-11-17都按推荐)），TS 照同一原则。

### 4.2 消息号与错误码常量

两份都由 Go 直接生成，不依赖 protoc 或 node。数据来源与 C# 共用一份：`protocol.ExportClientIDs` 的消息清单、`codeerr.Definitions`。

**`ts/message_ids.ts`**：

- 与 `MessageIds.cs` 的名字一一对应：

  ```ts
  // Code generated by roost client_export. DO NOT EDIT.
  export const MessageIds = {
    RequestAddExp: 10001,
    ResponseAddExp: 10001,
    PushEnterStatus: 10042,
  } as const;
  ```
- 用法 `MessageIds.RequestAddExp` 与 C# 一致。
- 实现：`protocol` 包里加 `generateTSIDs`，和 `generateCSharpIDsIn` 读同一份 `Definitions`。

**`ts/error_codes.ts`**：

- 写法：

  ```ts
  export const ErrorCodes = { NotEnoughGold: 100001, … } as const;
  export function errorCodeName(code: number): string | undefined;
  export function errorCodeMessage(code: number): string | undefined;
  ```
- 说明写成 JSDoc。
- 和 C# 的差别：C# 是 `ErrorCodes.Name(code)`，TS 放成两个独立函数。TS 的类静态成员不能叫 `name` / `length` / `prototype`，冻结对象的键也不能是 `__proto__`。分开之后只需要处理 `__proto__` 一个名字（加后缀 `_`，同 `errcode/csharp.go` 的 `csharpOwnSymbols` 做法）。
- 实现：`codegen/internal/errcode` 加 `GenerateTS`，与 `GenerateCSharp` 共用 PascalCase 转换和重名检查。

**是否另生成按消息绑定 Schema 的路由表**（`Routes.AddExp = { request, response, requestSchema, responseSchema }`）：

- manifest 的 `messages[].type` 足够生成它，TS 也有 Schema 对象可以直接引用。
- 但 C# 没有，第一版不做，保持两边一样。列为以后的选项。

### 4.3 `client_export.languages` 加 `ts` 的配置形状

```yaml
client_export:
  dir: shared/protocol
  languages: [csharp, ts]      # 可以只写 ts
  protoc: tools/protoc-31.1/bin/protoc   # 新：两种语言共用（见 D4）；不写依次找 ROOST_PROTOC、PATH
  csharp:
    namespace: Game.Protocol
  ts:
    protoc_gen_es: client/node_modules/.bin/protoc-gen-es   # 可选；不写依次找 ROOST_PROTOC_GEN_ES、PATH
```

**导出目录多出 `ts/`**（`clientExportFiles` 加这几项，`owns` 跟着认）：

```text
<dir>/ts/protocol_pb.ts      protoc-gen-es（target=ts, import_extension=js）
<dir>/ts/dao_sync_pb.ts      protoc-gen-es
<dir>/ts/dao_sync_apply.ts   Go 生成（§5）
<dir>/ts/message_ids.ts      Go 生成
<dir>/ts/error_codes.ts      Go 生成
```

**`manifest.json`**：

- 加 `protoc_gen_es` 与 `protobuf_es` 两个版本字段；
- `languages` 写 `["csharp","ts"]`；
- `files` 照常登记每个文件的哈希。

**校验**（`ClientExportSpec.validate`）：

- `languages` 接受 `ts`；
- 写了 `ts:` 却没列 `ts` 时报错（与 `csharp` 的写法相同）。

**没声明 `ts` 的工程，生成结果逐字节不变**：

- 照 client-export 决定 2 的要求；
- `TestUndeclaredClientExportChangesNothing` 的思路扩到“只声明 csharp 时与现在相同”。

### 4.4 protoc 与插件的版本锁定

照 C# 的做法：生成器核对版本，不下载。C# 的锁定链是：

1. `sdkGoogleProtobufVersion = "3.31.1"`；
2. 推出 protoc 31.1（`requiredProtocVersion`）；
3. 用 `protoc --version` 核对；
4. `TestSDKGoogleProtobufVersionMatchesCodecs` 核对生成器常量与 csproj 一致。

TS 的锁定链：

1. **运行库与插件同一精确版本**。protobuf-es 要求生成码与运行库同版本。
   - `client/ts/package.json` 的 `peerDependencies` 写 `"@bufbuild/protobuf": "<精确版本>"`；
   - `devDependencies` 写同版本的 `@bufbuild/protobuf` 与 `@bufbuild/protoc-gen-es`；
   - `package-lock.json` 已提交，`npm ci` 装出的就是这一版。
2. **生成器常量 `sdkProtobufEsVersion`**（client_export.go）是它的副本。新守卫 `TestSDKProtobufEsVersionMatchesClientTS` 读 `client/ts/package.json`，核对三处一致。生成器不能读仓库文件，所以要副本加守卫，与 C# 同一理由。
3. **生成时核对插件版本**：
   - 按 `ts.protoc_gen_es` → 环境变量 `ROOST_PROTOC_GEN_ES` → PATH 找插件，跑 `protoc-gen-es --version`，要求等于常量；不符或找不到时报错，给出安装方法（例如在拷出的 `client/ts` 里 `npm ci`，插件在 `node_modules/.bin/protoc-gen-es`）。
   - protobuf-es 的插件框架会答 `--version`（据其插件框架 `@bufbuild/protoplugin` 的文档）；第一轮实测核实，不答时改为读插件所在包的 `package.json`。
4. **protoc 仍要求 31.1**：
   - 理由一：protoc-gen-es v2 把解析后的文件描述符序列化进 `*_pb.ts`，protoc 版本不同可能带来字节差异，而 `generate --check` 要求逐字节可复现。
   - 理由二：和 C# 共用一份 protoc，不另立版本。
   - 本机 PATH 上的 protoc 是 36.2（本轮核对 `protoc --version` 得 `libprotoc 36.2`），所以要用 `ROOST_PROTOC` 或配置指向 31.1。
5. **代价**：声明了 `ts` 的**服务端**开发机要有 node 18+ 来跑插件。C# 只要 protoc 一个二进制，TS 多了 node（D3 的取舍之一）。

### 4.5 `roost client sync` 怎么同步 TS 产物

**现状**：

- `planProtocolSync` 写死 `csharp/` 前缀；
- 上次同步写了哪些文件，从客户端目录里另存的 `manifest.json` 认（`owned`）。

**改法**：

- 加 `--lang csharp|ts`：
  - 只声明了一种语言时可以省略；
  - 两种都声明时必须写（一个客户端仓库只用一种）。
- `--lang ts` 时：
  - 拷 `ts/` 下的文件，平铺进 `--to`；
  - 另存 `manifest.json`；
  - `owned` 只认 `ts/` 前缀。
- 其余规则不变：
  - 先核对服务端导出哈希；
  - 上次写过、这次没有的删掉；
  - 不是上次同步写的同名文件拒绝覆盖。
- 报告消息号与错误码变化的代码（`reportClientExportDiff`）与语言无关，直接复用。
- 配置同步（`--config-code-to`）同样加 `--lang ts`，拷 `ts/` 而不是 `csharp/`（§6）。
- `help.go` 的 `client` 一节与 compose.md 的客户端表同步改。

## 5. 自身视图同步

### 5.1 EntitySync 帧格式（TS 要实现的解码）

**外层包头**：flags 类型位 = 1（Sync），推送 flags = 3，载荷是原始 Sync 帧，不经 PB（client/README“包头：RS v2”）。

**Sync 帧协议 2**（`SyncFrame.Decode` 注释，与 Go `framework/sync/frame` 一致）：

- 开头是 `"RF"` 协议 2，kind 1 = Full、2 = Delta；
- 之后是无符号 varint，依次为 StreamID、Epoch、Tick、[Delta: Tick−BaseTick]、SchemaVersion、对象数；
- 对象：操作 1 字节（1 create、2 update、3 remove），varint ID、Generation，create 时带 Archetype，非 remove 时带组件数；
- 组件：操作 1 字节（1 set、2 remove），varint TypeID，set 时带 SchemaVersion、长度、数据；
- varint 必须最短编码、不越过字段范围；
- 对象数受 `maxObjects × 2` 限制，组件数受 `maxComponents × 2` 限制。

**subject 信封 v3**（`SubjectUpdate.Decode`）：

- flags：bit0 全量、bit1 带 Mask、bit2 带 Reason；
- 全量：SubjectId、Kind、Version、BaseVersion、[Mask]、[Reason]、Encoding，Lod 1 字节，SchemaVersion，Namespace，Profile（空串归一成 `"default"`），载荷；
- 增量：BaseVersion、步长（≥1）、[Mask]、[Reason]，载荷；身份从同一对象、同一组件上的基线继承，BaseVersion 必须等于基线的 Version。

**基线与视图规则**：

- `SyncReceiver`：首帧必须 Full，重复或旧代返回 false，缺口抛错并要求新的 Full；
- `EntityViews.Apply`：整帧解码、版本校验都成功才替换视图；Full 清场时对旧代际发 `SnapshotReplacement` 的 Remove；
- `GameClient.Pump`：Sync 应用失败就断开连接、清视图，靠重连收新的 Full。

**TS 的数值类型**：

- `SubjectId`、`StreamId`、`Version`、`Mask` 是 64 位，用 `bigint`；
- Epoch、Tick、ObjectId、Generation 是 32 位以内，用 `number`。

**工作量**：

- C# 的 `SyncFrame.cs` + `SyncReceiver.cs` + `EntityViews.cs` 共 425 行，TS 约 450 行；
- 测试要覆盖：
  - `packets.json` 的 `sync` 金样；
  - 坏 varint、截断、缺口、Full 清场与代际；
  - 用 Go entitysync Manager 下发的真实帧夹具 `client/dotnet/Roost.Client.Terrain.Tests/Fixtures/terrain-confirm-frames.json` 跑跨语言 `EntityViews`（C# 的 `TestChunkSyncFromFrames` 用的同一份）。

### 5.2 C# 的 DAO Apply 是怎么生成的

**入口**：

- `codegen/internal/dao/main.go` 第 143 行，把 `buildSyncCSharp(defs)` 写成 `db/dao_sync_apply.cs`；
- 每个有同步字段的工程都有，与 `dao_sync.proto`、`gen_dao_sync.go` 一起写出。

**生成器**：`codegen/internal/dao/sync_apply_csharp.go`（115 行）。

- `buildTrackedSyncCSharp` 为每个 DAO 生成 `Apply(this SyncXxx patch, SyncXxx previous)`。它的规则：
  - 核对 `SyncFormat == 3`；
  - 增量补丁可以不带实体 ID（沿用基线的），带了就必须一致；
  - 从 `previous.Clone()` 起，逐个存在的包装字段调 `ApplyField`；
  - 全部验证成功才交回新对象，旧视图不变。
- 嵌套类型另生成 `ApplyPatch(SyncPatchXxx, SyncXxx)`。
- `emitTrackedCSharpField` 按字段形状分四种：
  - **map**：`Delta=false` 是整体替换，此时带 `Removed` / `Patches` 报错；`Delta=true` 时先删 `Removed`、再写 `Value`，嵌套 map 元素逐个 `Patches`；
  - **list**：`Delta=false` 整体替换；`Delta=true` 按 `Length` 重建，`Updates[index]` 覆盖、其余取基线，超出基线又没有更新的位置报“空洞”，嵌套元素逐个 `Patches`；
  - **嵌套单值**：带 `Patch` 时对基线递归应用；
  - **标量与 bytes**：整体替换。
- 属性名由 `syncCSharpProperty`（`sync_proto.go`）按 protoc C# 插件的命名规则推出，撞上 `Parser`、`Clone` 等名字时加 `_`。
- `client_export` 只是把 `db/dao_sync_apply.cs` 改个命名空间（`daoSyncApplyNamespace`）复制进 `csharp/DaoSyncApply.cs`。

**跨语言验证**：

- C# 测试工程直接编译生成器金样 `codegen/internal/dao/testdata/golden/dao_sync.proto` 与 `dao_sync_apply.cs`（`Roost.Client.Tests.csproj` 第 17～18 行）；
- 用 Go 写出的 `client/spec/dao-sync.json`（全量、零值存在性）和 `dao-collections.json`（10 组 full / patch / want）核对合并结果（`DaoProtobufTests.cs`）。

### 5.3 TS 版要生成什么

**新增生成函数** `buildTrackedSyncTS(defs)`（放 `sync_apply_ts.go`，与 C# 版并列），输出 `dao_sync_apply.ts`。

- 结构照搬 C# 版，每个 DAO 一个导出函数：

  ```ts
  export function applySyncHeroDao(patch: SyncHeroDao, previous: SyncHeroDao | undefined): SyncHeroDao
  ```
- 用 protobuf-es 的 `clone(SyncHeroDaoSchema, previous)` 起步，不修改入参。
- 字段名按 protoc-gen-es 的本地名规则（lowerCamelCase，保留字加 `$` 后缀）推出，另写一个 `syncTSProperty`，与 `syncCSharpProperty` 并列。命名对不上时 §8.4 的编译测试会红。
- **与 C# 的差别都来自 protobuf-es 的表示**：
  - `map<uint64, …>` 的键是十进制字符串：`Updates` 下标要 `BigInt(key)` 比较，`Length` 是 `bigint`；
  - 列表是数组，map 是普通对象：替换用新对象，不用 `MergeFrom`，与 C# 注释“绝不使用 MergeFrom 追加”同理。

**放在哪里写出**：只在 `client_export` 声明了 `ts` 时生成，不写进每个工程的 `db/`。

- 写进 `db/` 会改动所有有同步字段的工程的生成结果，违反“没声明的工程逐字节不变”。
- `client_export` 像 `currentDaoSyncHash` 那样把 dao 生成器跑在临时目录里取 TS 版。
- 为此 dao 包导出一个 `BuildSyncTS(defDir)`，或给 `dao.Run` 加 `-ts` 输出项。

**SDK 一侧**（`@roost/client/protobuf`）：

```ts
registerDao(entities, "player", SyncPlayerDaoSchema, applySyncPlayerDao, { profile: "self" })
```

- 对应 C# `ProtobufExtensions.RegisterDao`：编码号 2（DAO PB），profile 缺省 `"self"`；
- 载荷先用 `fromBinary` 解码，再交给生成的 apply。

**金样**：

- 生成器金样加 `codegen/internal/dao/testdata/golden/dao_sync_apply.ts`；
- `client/ts/test` 编译它与对应的 `dao_sync_pb.ts`；
- 用 `dao-sync.json` / `dao-collections.json` 跑出与 C# 相同的断言。

### 5.4 工作量估计（粗）

| 项 | 量 |
|---|---|
| TS Sync 解码 + 基线 + `EntityViews` + `GameClient` 的 Sync 分支与三个事件 | 约 500 行 + 测试约 500 行 |
| Go：`sync_apply_ts.go` + 命名规则 + 金样 + client_export 接线 | 约 250 行 + 测试约 150 行 |
| 跨语言：dao 金样、`terrain-confirm-frames.json`、生成工程端到端（§8.2 第 3 条） | 约 300 行 |

合计约一轮。

## 6. 配置表（TS 读取与哈希校验）

C# 的现状（`client_config_csharp.go`、`client_config_csharp_runtime.go`）：

- 导出 `data/<表>.json`、`csharp/ConfigTables.cs`（入口 `Load(readText)`、`DataVersion`）、`ConfigJson.cs`（自带 JSON 解析器与规则检查，877 行模板）、每张表一个 `ConfigTables.<类型>.cs`；
- 加载时把每个文件 CRLF 统一成 LF 后算 SHA-256，与生成时的 `DataHash`（`tablegen/export.go` 的 `DataHash`）比较，不一致抛 `ConfigLoadException`；
- 再检查 required / unique / min / enum / ref。

TS 要生成与写的：

1. **`ts/config_tables.ts`**：
   - 入口 `ConfigTables.load(readFile: (name: string) => Promise<Uint8Array | string>): Promise<ConfigTables>`，异步：浏览器读文件是 `fetch`；
   - 收到 `Uint8Array` 时直接按原字节算哈希；收到字符串时先 UTF-8 编码再算，Go 写出的 JSON 没有 BOM，结果相同；
   - `DataVersion` / `ShortDataVersion` 常量。
2. **`ts/config_json.ts`**：自带的 JSON 读取。不能直接用 `JSON.parse`：
   - Go 的 `int` / `int64` 写成 JSON 数字，可能超过 2^53，`JSON.parse` 会静默丢精度；
   - C# 的 `ConfigJson` 也是自己解析数字文本，按类型范围检查（`ReadInt64` 等）；
   - TS 版按字段类型读数字原文：int8～int32 与 uint8～uint32 读成 `number`，int64 / uint64 见 D5。
3. **每张表一个 `ts/config_<表>.ts`**：
   - 行类型 `interface`、表类 `get` / `tryGet` / `all` / `by<index>`；
   - object / kv 是单例；
   - kv 的 note 写成 JSDoc。
4. **规则检查**：照 C# 运行部分移植 required / unique / min / enum / ref，报表、行、字段。
5. **SHA-256 用自带的纯 TS 实现**（约 100 行，放 SDK 或生成进 `config_json.ts`），不用 `crypto.subtle`。理由有三：
   - `crypto.subtle` 只在安全上下文（https 或 localhost）可用，http 部署的 H5 页面拿不到；
   - 它是异步接口；
   - 小游戏环境没有它。

**工作量**：

- Go 模板与生成器约 1,000～1,300 行（C# 两个文件共约 1,640 行，TS 少了 `[Serializable]`、命名空间与关键字处理）；
- 测试约 500 行（照 `client_config_export_test.go` 的 C# 用例，另加 node 下加载 game-demo 导出、改一个字节被哈希拒绝）。

## 7. API 草图

命名尽量照 C#，时间一律用毫秒数（照 relay TS 的 `…Ms` 习惯）。

### 7.1 可替换传输

```ts
/** SDK 与玩家接入之间的一条连接（对应 C# IGateTransport）。一个实例只连一次；重连时 SDK 向工厂要新的。 */
export interface GateTransport {
  /** 连到 url（ws:// / wss://）。失败或被 close 打断时 reject。 */
  connect(url: string): Promise<void>;
  /** 发一整帧（一条二进制消息），同步返回；连接已断时抛错。SDK 保证不并发调用。 */
  send(frame: Uint8Array): void;
  /** 立即关闭，可重复调用；之后不再回调 onData。 */
  close(): void;
  /** 收到的字节，按到达顺序，不要求与帧对齐（SDK 自己拼帧）。 */
  onData: ((chunk: Uint8Array) => void) | null;
  /** 连接断开（对端关闭、出错、收到文本消息）；error 为空表示正常关闭。只回调一次。 */
  onClose: ((error?: Error) => void) | null;
}
export type GateTransportFactory = () => GateTransport;

/** 浏览器 WebSocket（globalThis.WebSocket，binaryType = "arraybuffer"）；文本消息当协议错误关闭。缺省实现。 */
export class BrowserWebSocketTransport implements GateTransport { /* … */ }
// 以后：WechatSocketTransport（wx.connectSocket 的 SocketTask：onMessage({data: ArrayBuffer})、send({data})、close）、
//       DouyinSocketTransport（tt.connectSocket，同形）。上层代码不变，只换 transportFactory。
```

和 C# `IGateTransport` 的两点差别都来自单线程：

- **`send` 是同步的**：浏览器与小游戏的 send 都不阻塞；C# 在 PumpDriven 下也要求同步完成。
- **收字节是回调，不是 `ReceiveAsync` 拉取**：浏览器 WebSocket 与 `wx.connectSocket` 都是事件。

`ws` 包与浏览器 `WebSocket` 都满足 relay 已有的 `WebSocketLike`。所以 node 测试与浏览器共用 `BrowserWebSocketTransport`，构造时可以传 `webSocket: (url) => new WebSocket(url)`，与 `RelayClientOptions.webSocket` 同样的注入方式。

### 7.2 主要类型

```ts
export interface ConnectionOptions {
  webSocketUrl: string;                 // 必填：portal 进服应答给的完整地址（D8），路径与服务端 websocket.path 一致（缺省 /ws）
  transportFactory?: GateTransportFactory; // 缺省 () => new BrowserWebSocketTransport()
  connectTimeoutMs?: number;            // 10000，覆盖取票与握手
  requestTimeoutMs?: number;            // 10000
  heartbeatIntervalMs?: number;         // 15000；0 关闭
  heartbeatTimeoutMs?: number;          // 5000
  autoReconnect?: boolean;              // false
  maxReconnectAttempts?: number;        // 8
  reconnectInitialDelayMs?: number;     // 1000
  reconnectMaxDelayMs?: number;         // 30000
  maxPayloadBytes?: number;             // 1 MiB
  maxPendingRequests?: number;          // 64
}

export enum ConnectionState { Disconnected, Connecting, Reconnecting, Ready, Disposed }
export interface ReconnectResult { attempt: number; connectionVersion: number; error?: Error; willRetry: boolean }
export type TicketProvider = (signal: AbortSignal) => Promise<string>;

export class Connection {                       // 对应 C# Connection
  constructor(options: ConnectionOptions);
  readonly state: ConnectionState;
  readonly lastError: Error | undefined;
  readonly connectionVersion: number;
  readonly heartbeatRoundTrips: number;
  connect(ticket: string | TicketProvider, signal?: AbortSignal): Promise<void>;
  requestAsync(messageId: number, responseId: number, payload: Uint8Array, signal?: AbortSignal): Promise<Packet>;
  notifyAsync(messageId: number, payload: Uint8Array): void;
  probeClockAsync(signal?: AbortSignal): Promise<ClockSample>;
  disconnect(): void;
  dispose(): void;
}

export class GameClient {                        // 对应 C# GameClient
  constructor(options: ConnectionOptions, maxEntities?: number);
  readonly connection: Connection;
  readonly entities: EntityViews;                // 第 2 轮
  on(event: "error", fn: (e: Error) => void): () => void;
  on(event: "stateChanged", fn: (s: ConnectionState) => void): () => void;
  on(event: "reconnected", fn: (r: ReconnectResult) => void): () => void;
  on(event: "unhandledPush", fn: (p: Packet) => void): () => void;
  on(event: "entityChanged", fn: (c: EntityChange) => void): () => void;   // 第 2 轮
  on(event: "entitiesChanged", fn: (f: SyncFrame) => void): () => void;    // 第 2 轮
  on(event: "entitiesReset", fn: () => void): () => void;                  // 第 2 轮
  subscribe<T>(messageId: number, parse: (b: Uint8Array) => T, receive: (v: T) => void): () => void;
  request<T>(messageId: number, responseId: number, payload: Uint8Array,
             parse: (b: Uint8Array) => T, received: (v: T) => void, failed: (e: Error) => void, signal?: AbortSignal): void;
  probeClock(received: (s: ClockSample) => void, failed: (e: Error) => void): void;
  dispose(): void;
}

export class NetworkClock { /* observe(sample)、now(localNow)、reset()、ready、roundTrip，同 C# */ }

// @roost/client/protobuf（peer 依赖 @bufbuild/protobuf，对应 Roost.Client.Codecs）
export function request<Req extends Message, Res extends Message>(client: GameClient, requestId: number, responseId: number,
  reqSchema: GenMessage<Req>, req: MessageInitShape<GenMessage<Req>>, resSchema: GenMessage<Res>,
  received: (r: Res) => void, failed: (e: Error) => void, signal?: AbortSignal): void;
export function subscribe<T extends Message>(client: GameClient, pushId: number, schema: GenMessage<T>, received: (v: T) => void): () => void;
export function registerDao<T extends Message>(entities: EntityViews, ns: string, schema: GenMessage<T>,
  apply: (patch: T, previous: T | undefined) => T, options?: { profile?: string; schema?: number }): void;
```

C# 的多播事件在 TS 里写成 `on(…)`：返回取消函数，逐个订阅者调用，某个订阅者抛错交给 `error`。销毁边界照 `GameClient.Notify` 的规则：每次调用前检查 `disposed`。

### 7.3 用法与回调时机

```ts
import { GameClient } from "@roost/client/gate";
import { request, subscribe, registerDao } from "@roost/client/protobuf";
import { MessageIds } from "./shared/protocol/message_ids.js";
import { AddExpReqSchema, AddExpRespSchema, EnterStatusSchema } from "./shared/protocol/protocol_pb.js";

const client = new GameClient({ webSocketUrl: enter.wsUrl, autoReconnect: true });
client.on("error", (e) => console.warn(e));
client.on("stateChanged", (s) => showState(s));
subscribe(client, MessageIds.PushEnterStatus, EnterStatusSchema, (push) => onEnter(push));
await client.connection.connect((signal) => fetchFreshTicket(signal));   // 每次握手（含重连）重新取票

request(client, MessageIds.RequestAddExp, MessageIds.ResponseAddExp, AddExpReqSchema, { amount: 10n }, AddExpRespSchema,
  (resp) => render(resp), (err) => showRetry(err));
```

**事件循环模型**：浏览器只有一个线程，SDK 不需要 `Pump`，也不提供 `PumpDriven`。回调时机如下。

- **网络回调**：推送、请求的 `received` / `failed`、`entityChanged`、`stateChanged` 都在 WebSocket `message` / `close` 事件的那个任务里**同步**调用。
  - 一条消息里有几帧，就按线上顺序逐帧处理；
  - 同一帧内先应用、再通知（与 C# 相同）。
  - 所以回调形式（`GameClient.request` / `subscribe`）的应答与推送，严格按线上顺序到达。
- **Promise 形式**：`Connection.requestAsync` 的续行在微任务里执行，即当前这条 WebSocket 消息的所有帧都处理完之后。应答与推送合在同一条消息时，推送回调会先于 `await` 之后的代码。顺序要紧的业务用回调形式，文档写明。这和 C# `GameClient` 不保证“完成”与“推送”的相对顺序（`Pump` 先派完成、再派推送）是同一类边界。
- **计时**：心跳间隔、各种超时、重连退避都用 `setTimeout`。页面在后台时浏览器会给计时器限流：
  - 至少约 1 秒一次；
  - Chrome 后台超过 5 分钟后，同一链上的计时器约 1 分钟一次。
  - 心跳 15s 被拉长到约 60s 时仍在 `idle_timeout` 90s 以内；超时只会晚到、不会早到。
  - 手机浏览器会冻结后台页面，冻结期间被服务端按 idle 断开；回到前台后按 `autoReconnect` 重连（新票据、新代号，`entitiesReset` 后重新收 Full）。
  - 与 C# “不 Pump 就不跑”的后果相同，只是触发条件换成浏览器冻结。
- **回调可以重入调用 SDK**，例如在回调里 `dispose` 或再发请求。
  - 正在处理的这条消息里剩下的帧，`dispose` 之后不再派发；
  - 不支持在回调里同步等待（单线程也做不到）。
- **Cocos / Laya**：它们的 `update` 与网络事件在同一个线程，直接用上面的回调，不需要额外的派发队列。若以后有业务要求“回调只在 `update` 里发生”，再加可选的排队模式与 `pump()`（D6）。

## 8. 测试与验证计划

### 8.1 node 下的自动化（`ws` 包，`npm test`）

- **金样**：
  - `packets.json` 的 `pb`、`sync`、`auth`、`auth_generation` 逐字节编出与解开；
  - Gate 包头的拒绝规则：bits3..7、类型 3、MsgID=0 带 flags、超上限；
  - relay 的 27 条金样不变。
- **假玩家接入**（node 里起 `ws` 服务端），照 C# `TestWebSocketMessageBoundaries`：
  - 客户端每条消息恰好一帧，鉴权首包带代号信封、代号非 0 且递增；
  - 握手应答拆成两条消息发、应答与推送合成一条发、一帧拆成 7 字节一条、接近 1 MiB 的大帧，都能拼回；
  - 文本消息后断开，`lastError` 是协议错误；
  - 请求超时后迟到的应答被丢弃、连接保留；在途上限、心跳独占槽位（照 `HeartbeatCapacityTests.cs`）；
  - 回调里 `dispose` 的边界（照 `DisposalTests.cs`）；
  - 用假时钟（注入 `setTimeout` / `now`，照 relay TS 的做法）测心跳、退避、超时。
- **单元**：`NetworkClock` 的拒绝条件（照 C# 的判定），票据长度与 `0x00` 开头。
- **先红后绿**：每条用例先在没有实现、或去掉被测那段逻辑的代码上跑红，修前的输出原样抄进证据；再做变异，例如一帧拆成两条消息发、去掉代号信封、心跳不独占槽位、晚应答当推送派发，各自要红在对应的断言上（照 C# 证据 §3 的变异表）。

### 8.2 连真实 Go 玩家接入的集成测试

照 `infra/network/gateway/tcp_transport_test.go` 的 `TestTheCSharpClientWorksOverWebSocket` / `…ThroughAStandaloneGate`，由 Go 驱动客户端进程。这样服务端一侧的断言（连接代号递增）也能做。

1. **`TestTheTSClientWorksOverWebSocket`**：
   - 复用同一个 `dispatch`，请求协议同 C#：`gen` / `push:` / `close` / `slow`；
   - `idle_timeout` 1s，TS 心跳 250ms；
   - TS 一侧是 `client/ts/test/gate-driver.mjs`（node + `ws`）；
   - 环境变量 `ROOST_TS_GATE_DRIVER` 指向它，没设时跳过，与 `ROOST_CLIENT_TEST_DLL` 同样处理；
   - `runCSharpClient` 抽成通用的 `runClientProcess`。
   - 核对：握手、请求应答、校时、推送、心跳撑过 `idle_timeout` 且往返 ≥ 5、服务端关会话后自动重连且代号更大、请求超时（`slow` 1.5s 对 1s）、连接超时（对端接 TCP 不升级）。
2. **`…ThroughAStandaloneGate`**：Gate → 进程内 NATS 替身 → Game Ingress；握手、请求、心跳保持、被断开后重连。
3. **第 2 轮：生成工程端到端**：照 `scripts/test-gate-client.sh` 与 `client/dotnet/Roost.Client.Demo`，用生成的 game-demo，经 TS 客户端跑：
   - AddExp PB 应答；
   - DAO PB 的 Full / Delta、Remove、手动重连后 Full。
   - 这一步同时验证导出的 `protocol_pb.ts`、`dao_sync_apply.ts` 与真实服务端一致。

### 8.3 浏览器实跑（Claude 内置浏览器面板）

只验连入、握手、心跳保活，与 Unity GateSmoke 的口径相同（维护者 2026-11-18 对 WebGL 的要求）。

- **对端**：复用 `TestServeAWebSocketPlayerAccessForTheBrowserSmoke`。它是缺省配置加 WebSocket 监听，`allowed_origins` 放行 127.0.0.1 / localhost 的页面，任何票据都认，请求原样回。
- **页面**：新增 `client/ts/demo/gate-smoke.html` 与 `serve-gate-smoke.mjs`。照 `client/dotnet/unity-webgl-smoke/serve-gate.mjs` 起对端、发静态页，自动补 `?ws=…&hold=100`（`idle_timeout` 90 + 10）。
- **判定**：页面日志以 `SMOKE` 开头，记录握手耗时、每 10s 的心跳往返数，最后 `SMOKE PASS held 100s`。
- **范围**：
  - 浏览器会带 `Origin`，这一步同时验证 GATEWAY §6 的白名单路径；node 的 `ws` 不带，验不到；
  - Safari、Firefox、手机浏览器、真实后台标签页不在本轮（内置面板始终可见），登记进 KNOWN-LIMITS。

### 8.4 生成器测试

- **`codegen/internal/protocol`**：`generateTSIDs` 的金样，与 `MessageIds.cs` 同一份定义。
- **`codegen/internal/errcode`**：`GenerateTS` 的金样、`__proto__` 后缀、重名报错。
- **`codegen/internal/dao`**：`dao_sync_apply.ts` 金样（`testdata/golden/`）。
- **`codegen/internal/roost`**（照 `client_export_test.go` 的 `fakeProtoc`，加一个假的 `protoc-gen-es`）：
  - 声明 `ts` 时导出 `ts/` 全部文件与 manifest 字段；
  - 只声明 csharp 时结果与现在逐字节相同；
  - 插件版本不对或找不到时的报错；
  - 查找顺序 `ts.protoc_gen_es` → `ROOST_PROTOC_GEN_ES` → PATH；
  - `client sync --lang ts` 的增删改与拒绝覆盖；
  - `generate --check` 与 doctor `client-export:fresh` 覆盖 `ts/`；
  - 守卫 `TestSDKProtobufEsVersionMatchesClientTS`。
- **真实工具链**（`scripts/test-client-ts.sh` 的完整模式，要 protoc 31.1 与 `npm ci`）：
  - 生成一个声明 `ts` 的工程，用真实 protoc + `node_modules/.bin/protoc-gen-es` 导出；
  - 用 `tsc` 把导出目录与 SDK 一起编译；
  - 两次生成逐字节相同。
- 这一组改了生成器，按维护手册要跑全量 `GOWORK=off go test ./...`，并重新生成 game-demo 跑 build / vet / test。

### 8.5 加进 pretag 与 `scripts/test-*`

- **`scripts/test-client-ts.sh`**：在现有 `npm run build && npm test` 之后，加 `ROOST_TS_GATE_DRIVER=… GOWORK=off go test -count=1 -run '^TestTheTSClientWorksOverWebSocket' ./infra/network/gateway`（约 15s）。真实工具链那一条只在完整模式跑（`ROOST_TS_FULL=1`），照 `ROOST_DOTNET_GOLDEN_ONLY` 的分法。
- **pretag**：`scripts/pretag.sh` 第 192 行已经在 `run_ts` 时跑 `test-client-ts.sh`，不用改。
- **`scripts/pretag-scope.py`** 第 151 行的 TS 触发条件要加：
  - `client/wire/`、`infra/network/gateway/`、`infra/network/wsconn/`：TS 集成用例依赖它们；
  - `codegen/internal/roost/client_export*.go`：导出的 TS；
  - `client/relay/`、`service/relay/`、`service/lobby/`：现有 relay TS 用例的对端就是它们。
- **CI**：照 `client-dotnet` job 的 `ROOST_REQUIRE_DOTNET=1`，用 `ROOST_REQUIRE_NODE=1` 防止静默跳过。脚本已支持这个变量。

## 9. 分轮

| 轮 | 做到哪 | 可交付 | 粗略工作量 |
|---|---|---|---|
| **1. 连上、握手、收发协议** | <ul><li>包改名 `@roost/client` 与共用 `src/wire`（relay 金样不变）</li><li>`GateTransport` + 浏览器实现</li><li>会话、连接（心跳、超时、重连、连接代号）、`GameClient`（请求、推送、事件）、校时</li><li>`@roost/client/protobuf` 的请求与推送</li><li>`client_export` 的 `ts`：`protocol_pb.ts`、`dao_sync_pb.ts`、`message_ids.ts`、`error_codes.ts`、manifest、版本锁与守卫</li><li>`client sync --lang ts`</li><li>开工先做 §4.1 的三库实测</li></ul> | <ul><li>H5 页面拿到 portal 给的地址与票据就能连 game、收发业务 PB</li><li>证据：node 用例、Go 集成两项、浏览器面板 `SMOKE PASS`、包体对照</li><li>skill 客户端表“网页连 game”改为可用（自身视图同步除外）</li></ul> | <ul><li>TS 约 1,000 行 + 测试约 1,000 行</li><li>Go 约 600 行（导出、sync、集成用例）+ 测试约 400 行</li><li>约 2 个工作会话</li></ul> |
| **2. 自身视图同步** | <ul><li>Sync 解码、`SyncReceiver`、`EntityViews`、三个事件</li><li>Go 生成 `dao_sync_apply.ts`</li><li>`registerDao`</li><li>跨语言金样</li><li>生成工程端到端（AddExp + DAO Full / Delta / Remove / 重连）</li></ul> | 进入游戏后的自身视图与 C# 对齐，§5.4 | 约 1 个工作会话 |
| **3. 配置表与网络体验** | <ul><li>配置导出的 TS 读取（自带 JSON 数字读取、纯 TS SHA-256、规则检查）</li><li>`client sync --config-* --lang ts`</li><li>`SnapshotBuffer` / `PredictionHistory` / `SceneTimeDomain` / `ClientTrace` 移植</li></ul> | H5 能读服务端导出的配置表并核对数据哈希 | <ul><li>Go 约 1,300 行 + TS 约 300 行 + 测试约 600 行</li><li>约 1～2 个工作会话</li></ul> |
| **以后** | <ul><li>微信、抖音小游戏传输与真机（BigInt、`TextEncoder`、`wx.connectSocket` 的 1 MiB 帧）</li><li>Cocos Creator / Laya 最小工程接入验证</li><li>Lockstep</li><li>Safari / Firefox / 手机浏览器</li><li>单文件 ESM、发 npm</li></ul> | 按需求逐项 | 视项而定 |

每轮结束：改现行文档（`client/README.md` 加 TS 一节、`client/ts/README.md`、GATEWAY §6“客户端”、`help.go`、compose.md 客户端表），写 KNOWN-LIMITS 与 CHANGELOG `[Unreleased]`，证据放 `docs/maintenance/evidence/ts-gate-client-<日期>/`。

## 10. 风险与需要维护者决定的点

每条给选项，**推荐项在前**。

- **D1 包怎么分**：
  - **A. 合成 `@roost/client`，子路径 gate / relay / web / protobuf，`@roost/relay-client` 改名不兼容**（推荐，§3.1）；
  - B. 保留 relay 包名，往里加 gate；
  - C. npm workspaces 拆三个包。
- **D2 交付方式**：
  - **A. 拷同版本 `client/ts` 构建，`npm pack` 或 `file:` 依赖装进游戏工程**（推荐，沿用 skill 现在的做法）；
  - B. 另出单文件 ESM（加 esbuild devDependency）；
  - C. 发 npm（要账号与发布流程）。
- **D3 PB 库与生成位置**：
  - **A. protobuf-es v2，`roost generate` 在服务端跑 protoc 31.1 + `protoc-gen-es`**（推荐）。代价：声明 `ts` 的服务端开发机要装 node；
  - B. ts-proto（`forceLong=bigint`）；
  - C. protobufjs（不经 protoc，绕开版本锁，int64 是 `Long`）；
  - D. Go 生成器自写 TS 编解码（服务端不要 node，但要自己维护一个覆盖 oneof / enum / map 的 protobuf 实现，与 C# 的决定 1 相反）。
  - 第一轮开工先做三库实测，结果与 §4.1 的表不符时回来改。
- **D4 protoc 路径的配置位置**：
  - **A. 新增顶层 `client_export.protoc` 两种语言共用，删掉 `csharp.protoc`**（推荐）。读到旧写法报错并提示搬到顶层；线上尚未部署，不做兼容；CHANGELOG 写“roost.yaml 要改”；
  - B. 保留 `csharp.protoc`，TS 另加 `ts.protoc`，两处各写一份。
- **D5 64 位整数**：
  - PB 字段：**A. 用 protobuf-es 缺省的 `bigint`**（推荐，精确，与 C# `long` 相同）；B. 在导出的 proto 上加 `jstype = JS_STRING`。
  - 配置表的 `int` / `int64`：
    - **A. 读成 `number`，加载时超出 ±(2^53−1) 抛错，报表、行、字段**（推荐。配置值很少超过 2^53，超了也会大声失败；业务代码不用到处写 `BigInt`）；
    - B. 一律 `bigint`，与 PB 一致但业务算术麻烦。
- **D6 回调时机**：
  - **A. 网络事件里立即同步派发，不做 `Pump`**（推荐，§7.3）；
  - B. 另提供排队模式与 `pump()`，给要求回调只在引擎 `update` 里发生的业务。等有这种需求再加。
- **D7 小游戏传输放哪里**（影响以后那一轮）：
  - **A. 放进 SDK**（`@roost/client/minigame`）。它们是纯 TS、没有引擎胶水，与 C# 浏览器桥接因为 jslib 和 Unity 而不进 SDK 的理由不同；
  - B. 照 C# 只给参考写法。
- **D8 WebSocket 地址来源**：
  - **A. SDK 只认调用方给的完整 URL，与 C# 相同**（推荐）。
  - 风险：portal 的 `"transport":"ws"` 在 main 上还没实现（§1），TS 的登录端到端要等它。第一轮的测试不经 portal。
- **D9 `roost client sync` 的写法**：
  - **A. 加 `--lang csharp|ts`，只声明一种语言时可省**（推荐）；
  - B. 新旗标 `--ts-to`。
- **D10 Lockstep**：
  - **A. 以后做，有 H5 帧同步需求时再做，用 `packets.json` 的 lockstep 金样对齐**（推荐）；
  - B. 第一版就做（多约 600 行加测试）。

**不需要决定、但要知道的风险**：

- **浏览器的 `Origin`**：`allowed_origins` 为空时所有浏览器都被拒（GATEWAY §6）。H5 部署必须写页面来源，生成工程文档与 skill 要强调。
- **后台冻结**：手机浏览器冻结后台页面后会被 `idle_timeout` 断开，回前台靠重连。同类边界已登记在 C# 一侧。
- **`TextEncoder` / BigInt 在小游戏里的可用性**：第一版只做浏览器，不受影响；小游戏那一轮先核实。SDK 自己的 UTF-8 编码（票据）写成不依赖 `TextEncoder` 的小函数，免得以后返工。
- **导出的 `*_pb.ts` 依赖使用方工程里的 `@bufbuild/protobuf` 精确版本**：版本不一致时 protobuf-es 会在运行时报错。`client sync --lang ts` 的报告里打印要求的版本。
- **观察（尚未确认是否有意）**：`scripts/pretag-scope.py` 第 150 行的 C# 触发条件不含 `infra/network/gateway/`、`infra/network/wsconn/`。可 `test-client-dotnet.sh` 跑的 C# WebSocket 与跨 Gate 用例就在 `infra/network/gateway`，而那个包自己的 Go 测试在没设 `ROOST_CLIENT_TEST_DLL` 时跳过这些用例。结果是只改 gateway 的发版，按范围跑的 pretag 不会跑 C# 客户端用例。§8.5 给 TS 补的条件同样适合 C#，要不要一起改请维护者定。
