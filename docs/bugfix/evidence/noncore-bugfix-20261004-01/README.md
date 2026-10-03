# App/HTTP 四项修复的可携带证据

2026-10-04；修前 HEAD `183b0bddc5b46f6d3b50c5edda2d08da45150a69`，Windows/amd64、Go 1.27.0。四项 NC-01～04 已修复、具名场景验证，未发版。新 review 的 manager/admin/Ops 问题另行登记，不计入这里的关闭范围。

正式测试从 [原 review 证据](../../../review/evidence/noncore-review-20261003-01/README.md) 转为 app/mod_validation_promises_test.go、httpclient/client_boundaries_promises_test.go、httpserver/json_boundary_promises_test.go；没有删旧反例或改断言门槛。

- [formal-red.log](formal-red.log)：实现修改前，正式原 30 场景 = 12 fail / 18 pass，exit=1。失败落在真实依赖准入、Transport deadline、StatusError 分类、业务函数调用计数，非编译/环境或等待超时。
- [formal-green.log](formal-green.log)：修后相同 30 场景全部通过，exit=0；这是添加额外对照之前的原集合结果。
- 追加 15 个场景后正式新增集合 45 场景通过；包括父/自定义 client、子链、context deadline、16 并发 Clone、响应读取失败/部分 body/Close 与 decoder cause。test pass 事件含父测试，不能直接当叶子数。
- 最终复跑原未改动 overlay、相关 App 子包及 Kit adapter、race/vet 的结果见 [verify-exits.json](verify-exits.json)、[verify-summary.json](verify-summary.json)。完整大日志保留本机 `.tmp/noncore-bugfix-review-20261004-01`，提交小型诊断/事件摘要。

```powershell
& docs/bugfix/evidence/noncore-bugfix-20261004-01/Run-Verify.ps1 `
  -GoExecutable 'C:/path/to/go.exe' `
  -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/noncore-fix-rerun'
```

脚本需可用项目 Go 1.27 和依赖缓存/代理，不设置生产资源地址。本机复用已有 go-env.ps1（不提交环境文件），GOWORK=off。部署、真实代理/TLS、外部资源、HA/长稳、整个 App/HTTP 无缺陷均未宣称。Graph generation 09-30/freshness metadata_changed，实际读取/修改源码和实跑补证，未重启共享索引。
