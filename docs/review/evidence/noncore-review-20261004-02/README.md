# N01 生命周期与 Ops：审查证据

2026-10-04，Go 1.27.0、Windows/amd64。产品源码为 `3529a569920f08f3f42a45da9a2119e5935b45ae`，测试执行时原 HEAD=183b0bdd 加上上一批四项修复，提交后的产品源码一致。manager/admin/Kit Ops 三个问题文件未改；[逐项结论](../../../bug/REVIEW-2026-10-04-noncore-02.md)。这是未修代码的失败证据，不是修复验收。

- manager_review_test.go.txt：普通/context Stop panic、启动回滚 panic 三 fail；重复 Start 一 fail；普通返回 error 清理控制一 pass。
- admin_review_test.go.txt：空 map/数组内 map，输入/Get/List 六 fail；普通嵌套 map 复制控制一 pass。
- lifecycle_review_test.go.txt：快照中替换/下次生效、停止 Hook panic 后继续，两 pass。
- ops_review_test.go.txt：真实回环 HTTP Shutdown 中取消一 fail；正常关闭/重复停止/明确失败 readiness 一 pass；Err 无 Status 为仅观察项一 pass。

**17 个叶子/独立项 = 11 fail + 5 正常 pass + 1 观察。** [review.jsonl](review.jsonl) 原始 Go JSON 事件包含父测试，因此 test fail=13、test pass=6；不包含编译失败或跳过。 [exits.json](exits.json) 为 review=1、既有 regression=0、vet=0；[summary.json](summary.json) 和 [regression-events.jsonl](regression-events.jsonl) 保留六包 race 的 54 个 test pass 事件、0 fail/skip。完整 272KB 既有测试日志只保留本机 scratch，未提交缓存/环境文件。

```powershell
& docs/review/evidence/noncore-review-20261004-02/Run-Review.ps1 `
  -GoExecutable 'C:/path/to/go.exe' `
  -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/noncore-review2-rerun'
```

依赖需项目 Go 1.27 和有效 cache/proxy，执行前设置 GOWORK=off。本机用了已有 go-env.ps1。脚本创建虚拟 *_test.go overlay，不写产品测试；新反例集预期 exit=1，正常回归/vet 必须通过。原始 Ops server 监听随机回环端口，只关闭本次创建对象，无外部生产依赖。

[source-manifest.json](source-manifest.json) 记录 17 个已读生产文件 Git blob/工作树 SHA256；[graph-coverage.json](graph-coverage.json) 保存 Tier 2 coverage/generation，metadata_changed / not_tracked 已源码补证，干净信号不保证索引穷尽。没跑生产 HA/长稳、并发重复 Start、Ops deadline/hijack、阻塞 checker 或完整 App 故障进程；详见运行记录。
