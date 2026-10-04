# N02 接入与正式 Webroute 消费者证据

2026-10-04，Go1.27.0、Windows/amd64，源码`483350ca438a690b1c80da56ce79a64d2154dcf6`；实际运行在提交前同一产品工作树。本批未修改security/gateway/webroute/codegen源码。新[NC-05～07](../../../bug/REVIEW-2026-10-04-noncore-04.md)仍未修。

- security_review_test.go.txt：超burst请求占key名额1fail，token/signature10、连续补充/keycap/并发tokens/同key超额4控制pass。
- gateway_review_test.go.txt：reporter panic 1fail，nil/正常reporter/健康endpoint3控制、deadline/auth4控制pass。
- webroute_review_test.go.txt：三个非法pattern fail、JSON/raw门槛7控制pass，参数形状等价覆盖仅观察pass。
- runtime共34叶子/独立项=5fail+28控制+1观察，[review.jsonl](review.jsonl)原始事件、[exits.json](exits.json)退出结果；六包race/vet通过111test pass事件、0fail/skip。[摘要](summary.json)、[回归事件](regression-events.jsonl)。
- handlers.go.txt / consumer_test.go.txt 经正式CLI生成，独立module以local replace使用当前Core；[consumer-exits.json](consumer-exits.json)记录正常/退役均0，三个非法path生成0而consumer1。五阶段原始consumer-*.jsonl有13叶子执行=3fail/10pass，以及5个另一mode顶层skip（阶段选择，不计覆盖）。[非法生成物](generated-invalid.go.txt)、生成/退役日志均归档。

```powershell
& docs/review/evidence/noncore-review-20261004-04/Run-Review.ps1 `
  -GoExecutable 'C:/path/to/go.exe' -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/noncore-request-rerun'
& docs/review/evidence/noncore-review-20261004-04/Run-Consumer.ps1 `
  -GoExecutable 'C:/path/to/go.exe' -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/webroute-consumer-new-rerun'
```

依赖项目Go与cache/proxy，GOWORK=off；本机用了已有go-env.ps1。Consumer需新的空OutputRoot，防止覆盖先前证据；只修改自己创建的临时业务module。新反例预期exit1，正常regression/vet和消费者必须0；编译失败不得算作反例通过。正常阶段listener随机回环，只关闭自己对象；没有操作生产资源、TLS代理、外部HA或tag消费者。

[source-manifest.json](source-manifest.json)记录8个N02源文与四Codegen入口blob/工作树SHA256；[graph-coverage.json](graph-coverage.json)记录Tier2代际与覆盖限制。旧generation09-30、metadata_changed/new附件not_tracked均源码补证，没有刷新共享索引。原大回归日志/CLI二进制/依赖缓存只保留本机.tmp，不提交。
