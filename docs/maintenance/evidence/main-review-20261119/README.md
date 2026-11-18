# 2026-11-19 续审证据

被测 main 为 `ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3`。Linux amd64，Go 1.27.2，.NET SDK 10.0.402，Node 24.22.1，`GOWORK=off`。以下路径以被测检出为根。历史探针来自[上轮固定报告](https://github.com/tjbdwanghaibo/roost-core/tree/bd63e13196a1f33dd0ff16ace8ba964aa97ec89c/docs/maintenance/evidence/v156-main-review-20261118)。

| 证据 | 复跑与判读 |
|---|---|
| [Origin](origin.log) | 运行 httpserver 包两个 CORS 配置回归和 account 包的数字主机回归。按上轮说明构建 `origin_probe.go.txt`，用 Node WHATWG URL 生成 Origin；保留五个端口输入、配置结果和实际预检返回码。 |
| [TS](ts.log) | 在被测树 client/ts 运行 npm ci、npm run build、tsc -p tsconfig.test.json、编译后的 gate-timers.test.js 和上轮 cancel.mjs。immediate=true 组 calls=0，观察到的 GateClosedError 在虚拟时间 0 已产生。此组 aborted=false 是探针初始值，因为没有进入取票函数。 |
| [pretag 自测](scope.log) · [原提交范围](scope-real.log) | 执行 scripts/pretag_scope_test.py 和根包 TestPretagScopeSelfTest。另在旧提交 bf68b3b8c 的干净检出中使用**新提交的选择器脚本**，以 bf68b3b8c^ 为基线、scripts/test-modules-generated.sh 为生成脚本参数。这还原只有 TS 验收脚本改变的真实提交，run_ts=1。C# 单脚本改动由选择器自测覆盖。 |
| [portal 配置](portal.log) · [主机函数对照](host-history.log) | 从新提交编译 roost CLI，生成 game 模板工程，go mod edit -replace 指向同一检出，安装 portal 和 login。在 account 开发配置的四份副本追加 portal.websocket_servers，服号为字符串 1，地址见日志。各执行 roost config check --service account --file 相应副本 --json，记录退出码。Node new URL 对照 URL 可解析性。另在旧、新检出直接调用 account.CheckWebSocketAddr 比对。 |
| [Unity 包及桥接用例](unity.log) | 执行 dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json、node client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs 及 scripts/client/build-unity.py --target linux --managed-only。 |
| [C++ 弱网夹具](weak.log) | 环境没有 CMake。用 g++ -std=c++17 -O2 -pthread -Iclient/cpp/include 编译 CMake 目标对应的 src/client.cpp、lockstep.cpp、snapshot_digest.cpp、lockstep_c.cpp 和 tests/weak.cpp（均在 client/cpp 下），对同一产物执行原 tests/weak.py 三次。 |
| [性能证据复算](perf.log) | 对 main 提交内 m-series-s1-s2-20261119 的两份 JSONL 核对样本数和五类错误总数，分别运行 python3 -I agg.py 对应文件。结果来自原始记录。 |

## Unity relay 的取消对照

把 [项目文件](Review.csproj.txt) 与 [测试程序](Program.cs.txt) 复制到空目录，分别命名为 Review.csproj 和 Program.cs。将环境变量 RuntimePath 指向本次托管包的 com.roost.client/Runtime 绝对路径，执行 `dotnet run --project Review.csproj`。项目使用交付包内的两个 DLL 和 BrowserWebSocket.cs，只以计数替身实现 JS Socket 接口。两组调用相同的公开 RelayClient.ConnectAsync；每组预算 100 ms，在主线程持续轮询桥接和 Pump 至少 300 ms。观察 Task 状态及 JS 的 Open / Close 次数，没有真实网络请求。[运行结果](relay-cancel.log)
