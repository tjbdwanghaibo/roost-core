# 非三大核心审查第一批证据

基线 `746567ff37cf69878b74331cb9bcc69b159dfb63`，Windows/amd64，Go 1.27.0，2026-10-03。

仅用 Go overlay 将三个 `.go.txt` 作为临时测试文件注入 App/HTTP 包，产品源码、正式测试、go.mod/go.sum 不变。App 探针复用现有 `orderedTestMod`、`errService`、`newTestApp` 测试辅助，它们在原包中存在；不是复制产品实现。HTTP deadline 观察标准库传入 Transport 的 context，没有 sleep 或读取 timeout 字段作结论。StatusError 走真实 httptest HTTP；服务端探针通过完整 Engine 路由和业务调用计数。

复跑（从仓库根；设置自己的缓存/代理环境）：

```powershell
& docs/review/evidence/noncore-review-20261003-01/Run-Review.ps1 `
  -GoExecutable 'C:/path/to/go.exe' `
  -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/noncore-review-rerun'
```

原实现 overlay **exit=1 是预期红**：30 个具名叶子/独立控制，12 fail / 18 pass，关联四个根因。App 12 场景 7 fail/5 pass；client 11 场景 3 fail/8 pass；server 7 场景 2 fail/5 pass。报告只计最末场景，失败父测试不重复算。日志 [repro.log](repro.log) 保留原输出。

既有七包 `go test -race -count=1 -json` exit=0：123 test pass 事件、7 package pass、0 fail/skip。123 含父测试/子测试/Example，不叫 123 个独立叶子覆盖。精简原始 pass 事件在 [regression-events.jsonl](regression-events.jsonl)，完整 JSON 输出保留本机 `D:/whb_s/.tmp/noncore-review-20261003-01/regression.jsonl`。同七包 vet exit=0，无诊断。复跑脚本会记录三个退出码，不将复现 exit=1 混为环境失败；修后反例应转绿，不能要求持续红。

[退出码](exits.json) · [数量摘要](results-summary.json) · [关键源码 SHA256](source-hashes.json) · [当前跟踪文件清单](inventory.csv) · [清单摘要](inventory-summary.json)。

`Measure-Inventory.ps1` 从 Git index 枚举 `.go`/`.tmpl`，排除 `_test.go`/`_test.go.tmpl`、testdata/tests、根 demo/examples/docs，**保留 cmd 和生成文件**；不是代码发现或完成覆盖证明。当前 783 个 Go 候选、0 个独立 `.tmpl`；生成模板可能嵌在 Go/string/其他扩展名中，不能从零推断没有模板。本清单用于规划和 blob 增量，Codegen 审查必须追踪实际模板资源。更新源码后重跑清单时，应在新证据目录保存，不能覆盖本次历史基线。

图谱项目 `roost-core` 根 `D:/whb_s/cube-core`，ready；generation `2026-09-30T11:58:14Z`。[最终 coverage 原始记录](graph-coverage.json) 中关键源码均 no_recorded_issue / metadata_changed，已回读当前源码；三个新探针 not_tracked，原文和实跑补证。误尝试的 client_test.go/server_test.go 路径不存在，纠正为 httpclient_test.go/httpserver_test.go 后核验。图谱同名 heuristic 误连只作线索，不用零 callers 或 architecture 外部依赖摘要证明没有动态调用。

本轮没有真实代理/TLS/重定向、生产 Linux、多机 HA、长期容量测试。通过事件可支持具名场景，不支持整包无 bug 的断言。
