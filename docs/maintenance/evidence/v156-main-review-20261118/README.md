# 复验材料 · 2026-11-18

Linux amd64，Go 1.27.2，Node 24.22.1，.NET SDK 10.0.402，Redis 8.0.2，protoc 31.1。Go 使用 `GOWORK=off`，TS 依赖通过被测树的 `npm ci` 获取。每份检出提供自己的 SDK、CLI、模块模板和 core。日志中时间为执行器的 UTC。Redis 节点、房间、账号、地址均属于本轮测试。

## 原五项

| 材料 | 内容 |
|---|---|
| [tag.log](tag.log)、[main.log](main.log) | 每个提交的内存检查、CORS 检查、正常交接和原自动快照测试 12 轮。默认后端的 Redis 子项显示跳过，独立 Redis 运行见下一行。删除了依赖下载行和 201 笔换服的重复 begin INFO 行 |
| [redis.log](redis.log) | 本机 Redis Cluster 三主节点、16384 槽全覆盖，05 在两个提交各跑一次。为 06 另起单机 Redis，同一命令跑内存和 Redis 的顺序、乱序与真正重连。日志保留后端、提交、断言结果，省略重复 begin INFO |
| [merge-assert.patch](merge-assert.patch) | 05 相对被测树的唯一测试改动。原生 201 笔用例已经检查无关操作确实在读取后完成、目标换服仍在途、实际合服拒绝及源服别名不变。额外断言目标服的反向索引仍为空 |
| [transfer-guard.log](transfer-guard.log) | 07 修后 SDK 的 `TestATransferPreparedInAnEarlierHostTermIsDropped` 每个提交各 3 轮。另见两份默认后端日志中的同任期正常交接 |
| [tag-ts-golden.log](tag-ts-golden.log)、[main-golden.log](main-golden.log) | 两份检出的 TS relay 金样与 Go relay / wire 金样。Go SDK、relay、wire、lobby 及 httpserver 的相关实现和 go.mod / go.sum 在两个提交间无改动；account 包后续增量在 presence.go 及其测试 |

在两个被测检出分别运行。以下命令假设 `EVIDENCE` 指向本目录的绝对路径，生成文件仅放在专用测试检出中。本轮实际用 Go overlay 加载等价的测试文件。

```sh
git apply --unidiff-zero "$EVIDENCE/merge-assert.patch"
old=88d559511ff565956b8015c1541d7594d612c762
base=docs/maintenance/evidence/main-review-20261118
git show "$old:$base/review_new_seat_test.go.txt" > service/lobby/review_seat_test.go
git show "$old:$base/review_origin_test.go.txt" > infra/network/httpserver/review_origin_test.go
go test -v -count=1 -run '^TestAMergeCheckSeesEveryTransferWhileOthersSettle$' ./service/account
go test -v -count=1 -run '^(TestReviewPriorSeatReport|TestALateOnlineReportStillRecordsAReconnectAfterALeave)$' ./service/lobby
go test -v -count=1 -run '^(TestReviewPortalDefaultPort|TestCheckCORSOriginsRefusesWhatABrowserNeverSends|TestCORSAllowsOnlyListedOriginsThroughTheEngine)$' ./infra/network/httpserver
go test -v -count=1 -run '^TestTransferHandsTheHandoffToTheNewHost$' ./client/relay
go test -v -count=12 -run '^TestAutoSnapshotIsPushedWhileHost$' ./client/relay
```

05 的真实后端复跑时设 `ROOST_REVIEW_CLUSTER` 为隔离 Cluster 的三个逗号分隔节点。06 复跑时设 `ROOST_REDIS_TEST_ADDR` 为独立单机 Redis。两次检出的 Redis 验证均使用各自生成的测试前缀。

## 新发现的对照

- **10 · CORS**。复制 [origin_probe.go.txt](origin_probe.go.txt) 为临时目录下的 `origin_probe.go`。从被测检出构建它，以配置原文和 `node -p 'new URL(process.argv[1]).origin' "$url"` 的结果为两个参数运行。探针调用实际配置检查和预检处理器。两个提交均测无端口、443、0443、8443 和 08443。[结果](origin-ports.log)
- **11 · TS 主动取消**。从 main 的 `client/ts` 执行 `npm ci`、`npm run build`、`./node_modules/.bin/tsc -p tsconfig.test.json`，再运行 `node "$EVIDENCE/cancel.mjs" "$PWD"`。使用仓内假时钟及挂起的取票协作者，记录取票次数、取消信号与 Promise 状态，无外部服务。[结果](cancel.log)
- **12 · pretag 选择器**。在固定 main 运行 `python3 scripts/pretag-scope.py HEAD^ scripts/test-modules-generated.sh`，再将基线换成 `v1.56.0`。生成脚本参数对本项 SDK 开关结论无影响。[补丁范围](scope.log) · [全发布范围](scope-release.log)
- **13 · WebSocket 地址检查**。用 main 生成的 game 工程安装 portal。在 account 开发配置的两个副本中分别设置 `portal.websocket_servers` 的服号 `"1"` 指向 `ws://192.0.2.30:7080/ws` 和 `ws://192.0.2.300:7080/ws`，分别执行 `roost config check --service account --file <副本> --json`。以 Node WHATWG URL 对照两值，使用编译出的 `BrowserWebSocketTransport.connect` 检查无效地址。合法地址仅检查解析。[结果](portal-addr.log)

## 增量功能

生成工程 `example.com/review/ember` 使用 main 的 `roost project new`、`go mod edit -replace`、`roost module add login portal mailbox purchase` 与 `roost module verify --json`。[完整 JSON](modules.json)

TS 的 28 个 Gate 用例从 `dist-test/test/gate-*.test.js` 运行。Go 集成测试使用同一 main 的 TS driver 或编译出的 C# 测试 DLL，分别运行 gateway 包中的 `TestTheTSClientWorksOverWebSocket` 与 `TestTheCSharpClientWorksOverWebSocket` 前缀用例。[TS](ts-gate.log) · [TS 集成](gateway.log) · [C# 自测](cs-sdk.log) · [C# 集成](cs-gateway.log) · [WebGL 桥](jslib.log) · [托管打包](unity.log)

生成器运行 `TestGeneratedComponentFollowsTheComponentPackageRule` 和 `TestClientExportTSWithTheRealToolchain`，设置 `ROOST_TS_FULL=1` 和 `ROOST_PROTOC`，另有导出配置、接口及 sync 定向测试。最初两次组件夹具因 Go 无法获取云环境上级空 .git 的 VCS 状态失败；将 TMPDIR 设在有效的临时 Git 仓库后，用原始构建 / vet 和断言完成复跑。[定向测试](codegen.log) · [最终运行](full-export-final.log)

MOBA 和 SLG 分别从 main 执行 `scripts/test-moba-generated.sh` 与 `scripts/test-slg-generated.sh` 的默认功能档，使用各自的生成目录。夹具和模块的跳过及真实依赖范围以日志和主报告为准。[MOBA](moba.log) · [SLG](slg.log)
