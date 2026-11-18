# TS 客户端连玩家接入第 1 轮：证据（2026-11-18）

方案 [FEATURE-2026-11-18-ts-gate-client](../../FEATURE-2026-11-18-ts-gate-client.md)（D1～D10 维护者 2026-11-18 全部按推荐 A），本轮做第 9 节的第 1 轮：连接、握手（带连接代号）、心跳、超时、重连、请求 / 应答、推送、校时；TS 协议导出（`*_pb.ts`、消息号、错误码）；`roost client sync --lang`；浏览器冒烟。不含自身视图同步与配置表。

**基线**：main `70c9d2f406bddf6d20cac8678e8746330af42b63`（`70c9d2f4`）。本机 macOS（Darwin 25.5.0，ARM64），node v26.7.0 / npm 11.19.0，Go `GOWORK=off`，protoc 31.1（`~/tools/protoc-31.1/bin/protoc`；PATH 上是 36.2）。

| 文件 | 内容 |
|---|---|
| [pb-libraries.txt](pb-libraries.txt) | 开工第一步：protobuf-es 2.17.0、ts-proto 2.22.0、protobufjs 8.8.1 的实测（依赖、int64、presence、protoc 版本影响、包体） |
| [red-ts-no-implementation.txt](red-ts-no-implementation.txt) | 修前（先红）：gate 用例写好、实现还没有时 `npm test` 的原样输出（45 个 tsc 错误） |
| [red-go-integration-no-build.txt](red-go-integration-no-build.txt) | 修前（先红）：Go 集成用例在 SDK 还没构建出 `dist/gate` 时的原样输出 |
| [mutations-ts.txt](mutations-ts.txt) | node 用例的 12 个变异（`client/ts/test/mutate.mjs`），全部变红 |
| [mutations-integration.txt](mutations-integration.txt) | Go 集成用例的 4 个变异（`client/ts/test/mutate-integration.sh`），全部变红 |
| [mutations-codegen.txt](mutations-codegen.txt) | 生成器侧 6 个变异：5 个变红，1 个是等价变异（说明在文件末尾） |
| [green-test-client-ts-full.txt](green-test-client-ts-full.txt) | 修后（变绿）：`ROOST_TS_FULL=1 ./scripts/test-client-ts.sh` 的完整输出 |
| [browser-smoke.txt](browser-smoke.txt) | Claude 内置浏览器面板里的冒烟：`SMOKE PASS held 100s on one connection, 6 heartbeat round trips` |

## 1. 开工第一步：三个 PB 库实测（D3 的前提）

D3 的前提是“protobuf-es 运行库零依赖、int64 是 bigint、生成物不受 protoc 小版本影响、插件能答 `--version`”。实测全部成立，没有推翻（[pb-libraries.txt](pb-libraries.txt)）：

| | protobuf-es 2.17.0 | ts-proto 2.22.0 | protobufjs 8.8.1 |
|---|---|---|---|
| 运行库依赖 | 无 | 生成物依赖 `@bufbuild/protobuf` | `long` |
| int64 | `bigint`（9007199254740993 精确） | 缺省 `number`，大值解码直接抛错；`forceLong=bigint` 才行 | `Long` 对象 |
| 经 protoc | 是（`protoc-gen-es --version` 答 `v2.17.0`） | 是 | 否（pbjs 自己解析 .proto） |
| protoc 31.1 与 36.2 的生成物 | 逐字节相同（三份 proto） | — | — |
| 运行库 min / gzip | 67,623 / 18,057 | 10,820 / 3,584 | 44,170 / 12,682 |
| game-demo 全部消息 min / gzip | 147,895 / 37,638 | 625,901 / 60,594 | 1,853,434 / 123,339 |

- 与 Go 互通：解码 Go 写的金样逐字段相等；重新编码后字节顺序不同（Go 按声明顺序、protobuf-es 按字段号），proto3 允许任意顺序，不影响互通。
- 与方案的出入：`@bufbuild/protoc-gen-es` 2.17.0 的 `engines` 是 node ≥ 22（2.12.0 起 ≥ 20），方案写的“node 18+”不成立，已写进 KNOWN-LIMITS 与文档。
- SDK 自己的包体（esbuild 0.28.2 `--bundle --minify --format=esm --target=es2020`，gzip -9；不含 protobuf-es）：`@roost/client/gate` 22,775 / 7,463 字节，`@roost/client/relay` 27,326 / 9,037，根路径（gate + relay + web）58,310 / 17,901。

## 2. 先红后绿

**TS 用例**：`client/ts/test/gate-*.test.ts` 先写，实现前 `cd client/ts && npm test` 原样输出见 [red-ts-no-implementation.txt](red-ts-no-implementation.txt)（`tsc` 报 45 个错误，gate 子路径没有被测的导出）。实现后：

```sh
cd client/ts && npm test          # ℹ tests 102 / pass 102 / fail 0（relay 74 + gate 28，含 relay 真实对端）
```

**Go 集成**：`infra/network/gateway` 的 `TestTheTSClientWorksOverWebSocket`、`TestTheTSClientWorksOverWebSocketThroughAStandaloneGate`，node 进程（`client/ts/test/gate-driver.mjs`）由 Go 驱动，覆盖握手、请求应答、校时、推送、心跳撑过 `idle_timeout`、服务端关会话后重连且代号更大、请求超时与迟到应答丢弃、计时器冻结（模拟后台页面）被断开后重连、连接超时；独立 Gate 上跑握手、请求、心跳与冻结后的重连。SDK 没构建时的红见 [red-go-integration-no-build.txt](red-go-integration-no-build.txt)。

```sh
ROOST_TS_GATE_DRIVER=$PWD/client/ts/test/gate-driver.mjs GOWORK=off go test -count=1 -run '^TestTheTSClientWorksOverWebSocket' ./infra/network/gateway
# ok  github.com/tjbdwanghaibo/roost-core/infra/network/gateway  10.619s
```

C# 用例的对端抽成共用之后，`TestTheCSharpClientWorksOverWebSocket` 两项照常通过（`dotnet build` 后带 `ROOST_CLIENT_TEST_DLL` 跑）。

**生成器**：`codegen/internal/roost` 的 ts 用例与实现在同一提交里写成，没有单独保存“实现前”的运行（实现前它们只能是编译失败）；红用变异证明（§3 的 C1～C6）。

```sh
GOWORK=off go test -count=1 -run 'TestClientExport|TestGenerateClientExport|TestClientSync|TestSDK|TestUndeclaredClientExport|TestClientConfig|Help' ./codegen/internal/roost   # ok
GOWORK=off go test -count=1 ./codegen/internal/protocol ./codegen/internal/errcode   # ok
ROOST_TS_FULL=1 ROOST_PROTOC=<protoc 31.1> GOWORK=off go test -count=1 -run TestClientExportTSWithTheRealToolchain ./codegen/internal/roost   # ok：真 protoc + protoc-gen-es，tsc（NodeNext）编译导出与 SDK
```

**一条命令**：`ROOST_TS_FULL=1 ROOST_PROTOC=<protoc 31.1> ./scripts/test-client-ts.sh` 依次跑 build、`npm test`、两项 Go 集成、`test/gen/dao_sync_pb.ts` 重新生成后逐字节比较、真实工具链用例，全部通过（[green-test-client-ts-full.txt](green-test-client-ts-full.txt)）。

## 3. 变异

| 组 | 改坏什么 | 结果 |
|---|---|---|
| M1～M12（[mutations-ts.txt](mutations-ts.txt)） | 拆帧发送、认证不带代号信封、心跳占业务名额、晚应答当推送、应答推迟到微任务、回调里断开后仍处理同一消息的剩余帧、包头不查 bits3..7、重连不重新取票、退避不加抖动不翻倍、心跳超时不断开、连接超时不覆盖取票、文本消息不断开 | 全部变红 |
| G1～G4（[mutations-integration.txt](mutations-integration.txt)） | 代号不递增、不发心跳、断线不重连、请求不计超时 | 全部变红（对真实玩家接入） |
| C1～C6（[mutations-codegen.txt](mutations-codegen.txt)） | protoc-gen-es 版本不核对、`--es_opt` 少 `import_extension=js`（假插件 / 真工具链）、两种语言时 sync 不要求 `--lang`、语言表去掉 ts、消息号去掉 `as const` | C3 等价变异（导出的两个 proto 互不导入，生成物里没有相对导入，参数由 C2 守住），其余变红 |

## 4. 浏览器冒烟

`cd client/ts && npm run build && node demo/serve-gate-smoke.mjs 8791`，Claude 内置浏览器面板（Chromium 156，面板与标签页在前台）打开 `http://127.0.0.1:8791/`，对端是 `TestServeAWebSocketPlayerAccessForTheBrowserSmoke`（缺省配置，`idle_timeout` 90s）。页面握手 22ms、保持 100s、6 次心跳往返，最后一行 `SMOKE PASS held 100s on one connection, 6 heartbeat round trips`（[browser-smoke.txt](browser-smoke.txt)）。Safari、Firefox、手机浏览器、后台标签页没有跑。

## 5. 守卫与入口

- `scripts/test-client-ts.sh`：`npm test` 之后跑两项 Go 集成；`ROOST_TS_FULL=1` 另跑真实工具链。pretag 在 `run_ts` 时调它（`scripts/pretag.sh`，没改）。
- `scripts/pretag-scope.py`：TS 触发条件在原有 `client/ts/`、`client/relay/`、`client/spec/`、`client/wire/`、`service/relay/`、`service/lobby/`、`infra/network/wsconn/` 之上加 `infra/network/gateway/` 与 codegen 的 client_export / client sync / TS 生成器。以 `d9af17aa` 为基线试跑：只改 codegen 时输出 `TS yes`。
- `TestSDKProtobufEsVersionMatchesClientTS`：`client/ts/package.json` 的 peer / dev 版本与生成器核对的 protoc-gen-es 版本一致。

没有跑：全量 pretag、模块矩阵、`GOWORK=off go test ./...` 全量（按本轮要求只跑相关包与根包）。
