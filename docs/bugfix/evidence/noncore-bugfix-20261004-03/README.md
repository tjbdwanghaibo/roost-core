# 请求边界三项修复证据

2026-10-04，Windows/amd64、Go1.27.0，基线 `c4aa1e7dd69bbf795fee878b1fb68e0bba11525a` 加本轮四个生产文件修复；[源码 SHA256](source-hashes.json)、[最终摘要](summary.json)、[运行与兼容](../../../review/REVIEW-2026-10-04-noncore-05.md)。NC-05～07 已修，未发版。

- [正式修前](formal-red.jsonl)：29叶子/独立项，14失败、15控制通过；[同集合修后](formal-green.jsonl)29全绿。后来添加诊断日志 sink panic 控制，正式最终集合30项，受影响包 race 已执行。
- [原审查附件不改断言的复测](original-green.jsonl)：34项全绿，含一个参数形状覆盖观察；该观察不是新契约验收。
- [最终回归事件](regression-events.jsonl)：20个有测试的包、754 test pass事件、0 fail，vet=0。首次全包结果[退出](regression-exits.json)为1，[失败原文](initial-failures.jsonl)含 Codegen 基础路径错误文本变化及缺 sh；前者已保持旧文本并[定向重跑](webroute-final.jsonl)。后者仅 `codegen/internal/roost` 重跑排除具名 TestDeployScriptsCarryNoKnownShellcheckFindings，[支持范围结果](roost-supported-exit.json)0。最终事件合并各包最后一次有效执行，不把首次失败写成成功。
- [skip](skips.jsonl)：9个原测试事件为本机工具/外部环境受限；另15个包级 skip 为 no test files，不计验证。具名部署静态检查另外因缺 sh 排除，不把这些当作通过。完整初始回归与支持范围日志在本机 scratch。
- [生成消费者退出](consumer-exits.json)：正常/退役生成0、consumer0；三个坏模式生成1，放入旧生成物后的 consumer0。consumer-*.jsonl 共13次叶子执行全通过；五个另一mode顶层skip仅选择阶段。generation-*.log保留当前正常输出与三种拒绝。

实际执行为：正式红 → 四文件修复 → 正式绿 → `go test -race -count=1 -timeout=180s -json ./security ./gateway ./webroute ./httpserver ./httpclient ./codegen/...` 与同范围 vet → 具名 shell 环境排除的 roost 包 → Webroute 错误文本兼容更正后的包 race/vet → 原 review overlay → CLI消费者。生成形状未变，没有需要重生成的 fixture差异。

```powershell
& docs/bugfix/evidence/noncore-bugfix-20261004-03/Run-Verify.ps1 -GoExecutable 'C:/path/to/go.exe' -OutputRoot 'D:/scratch/request-verify'
& docs/bugfix/evidence/noncore-bugfix-20261004-03/Run-Consumer.ps1 -GoExecutable 'C:/path/to/go.exe' -OutputRoot 'D:/scratch/new-empty-consumers'
```

依赖可用 Go、项目 cache/proxy；本机 source既有go-env.ps1、GOWORK=off。消费者需新空目录；复跑脚本只用正式 CLI、local replace 和回环随机端口，引用仓库原 review fixture。完整原日志在 `D:/whb_s/.tmp/noncore-bugfix-review-20261004-03` 与 `D:/whb_s/.tmp/webroute-fixed-consumers-20261004-03`，大日志/二进制/缓存不提交。[图谱覆盖](graph-coverage.json)代际09-30，当前源文补证；没有停止/重建共享索引。
