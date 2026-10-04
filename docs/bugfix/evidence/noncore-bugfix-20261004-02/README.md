# Manager / Admin / Ops 四项修复证据

2026-10-04，修前 `7e0d6ee29a134e75db142fda94ef93332b8e9841`，Windows/amd64、Go 1.27.0。[原问题](../../../bug/REVIEW-2026-10-04-noncore-02.md)。NC-01～04 已修、声明场景验证，未发版。

- [formal-red.log](formal-red.log)：修前 14 个叶子/独立项，11 fail / 3 pass，exit=1；真实 panic/回调次数、外部 schema 变更、回环 HTTP 未排空引用失败，非构建/工具或 harness 超时。
- [formal-green.log](formal-green.log)：修后相同 14 项全部转绿，没有放宽旧断言。
- 正式测试后补 21 项，合计 **35 个新增叶子/独立场景**；39 个 test pass 事件包含父测试。11 包 race 全通过、233 test pass 事件、0 fail/skip，同包 vet 通过。细节见 [summary.json](summary.json)、[exits.json](exits.json)、[regression-events.jsonl](regression-events.jsonl)。
- 原 review overlay 也按原文复跑，[original-green.jsonl](original-green.jsonl) 与 original_exit 另存；其中 Health 仅观察项仍是观察，不由本批修改或关闭。
- [source-hashes.json](source-hashes.json) 保存四产品/adapter与三正式测试 SHA256；图谱代际仍 09-30，新正式测试 not_tracked，当前源码与执行补证。完整大日志在本机 .tmp/noncore-bugfix-review-20261004-02，环境文件/缓存/二进制不提交。

```powershell
& docs/bugfix/evidence/noncore-bugfix-20261004-02/Run-Verify.ps1 `
  -GoExecutable 'C:/path/to/go.exe' `
  -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot 'D:/whb_s/.tmp/noncore-fix2-rerun'
```

脚本需项目 Go 1.27 和依赖 cache/proxy，GOWORK=off；本机复用已有 go-env.ps1，脚本没有配置外部生产资源。停止使用自己的随机回环 listener，没有停共享资源或共享索引。永久阻塞/所有用户 callbacks、完整 App 故障进程、bind/hijack、线上 HA/容量尚未验证。
