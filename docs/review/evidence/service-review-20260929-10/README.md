# 第十轮 review 证据

68cf87fc加RR-33修复。[问题/实施交接](../../../bug/REVIEW-2026-09-29-services-10.md) · [运行](../../REVIEW-2026-09-29-services-10.md) · [修复红绿/实际SOURCE](../../../bugfix/evidence/service-bugfix-20260929-08/RESULTS.json)。

`pipeline_test.go.txt`是review overlay，未修改生产driver或正式测试。用独占真实standalone/三masterCluster执行：

```powershell
./docs/review/evidence/service-review-20260929-10/Run-Review.ps1 -RedisAddress '127.0.0.1:16439' -ClusterAddresses '127.0.0.1:16436,127.0.0.1:16437,127.0.0.1:16438' -OutputDirectory '<自己的临时目录>'
```

默认严格要求两个missing→failed HSet反例fail、一个reuse/Discard控制pass且无skip/build-fail；runner识别预期反例，不能把环境/编译失败当bug证明。RR-34修复后加`-ExpectFixed`要求三个叶子全pass。脚本不启动/停止Redis、不提交源码。

RESULTS保存失败值、错误和日志hash；inventory只核算100个service生产路径（97未变复用/3变化），不代表逐行/测试覆盖率。COVERAGE-BEFORE记录初始13:13:03Z代际metadata_changed/missing/nottracked和boundedscopes，当前源码补证；最终图谱/交付记录以本轮追加为准。raw日志在本地.tmp，不提交大日志或Redis数据。
