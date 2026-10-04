# N03 etcd 第五批：可复跑证据

2026-10-04，Windows/amd64、Go1.27.0；etcd源文基线`e62729ac`未改，其他目录含本轮NC-08～10修复。[运行](../../REVIEW-2026-10-04-noncore-08.md) · [NC-11/12](../../../bug/REVIEW-2026-10-04-noncore-08.md) · [摘要](summary.json) · [源码范围/blob](source-manifest.json) · [Tier2 coverage](graph-coverage.json)。

[review.jsonl](review.jsonl)：8叶子/独立项=2fail/6pass。driver_review_test.go.txt含实际clientv3→concurrency.NewSession→本机gRPC LeaseGrant取消反例、watch readiness三控制、队列关闭和mirror clone/CAS/stale控制；etcd_review_test.go.txt含受支持第三方watcher关闭预算反例、正常callback错误/显式关闭控制。两个失败均确认目标阶段入场及释放后的退出，不是钩子缺失/编译/环境失败。

[regression-events.jsonl](regression-events.jsonl)：etcd/etcd-driver两测试包45 test pass事件，0测试fail/skip；KitEtcd包级skip为no test files，不能算场景通过。三包vet exit0，[exits](exits.json) review1/regression0/vet0。[integration-environment.jsonl](integration-environment.jsonl)为实际`go test -race -tags integration -run '^TestReal' ./etcd/driver`，唯一选中项TestRealEtcdGetOfAMissingKeyIsNotFound因PATH无etcd skip，不计已验。

```powershell
& docs/review/evidence/noncore-review-20261004-08/Run-Review.ps1 -GoExecutable 'C:/path/to/go.exe' -OutputRoot 'D:/scratch/etcd-review8'
```

本轮已执行此脚本；以overlay加临时test路径，不修改产品测试/依赖/生成物。review预期exit1，既有race/vet必须0。完整临时日志在`D:/whb_s/.tmp/noncore-review-20261004-08`。gRPC listener/client/server为测试创建并关闭；不是实际etcd server、多机HA或生产长稳。默认scope只查本轮声明链，未声明N03全部场景完成。
