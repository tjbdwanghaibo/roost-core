# N04 第三批：源码与反例证据

源码起点main `08d18be9`，同树先修NC-16～20；[问题](../../../bug/REVIEW-2026-10-04-noncore-14.md) · [运行](../../REVIEW-2026-10-04-noncore-14.md)。

| 证据 | 实际结果 |
| --- | --- |
| [review.jsonl](review.jsonl)、[退出码](exits.json) | 十二叶子6fail/6控制；真实Redis二项1fail/1控制，公开mongotest十项5fail/5控制，共五根因 |
| [cleanup.json](cleanup.json)、[Redis版本](redis-version.log) | Redis8.8.0、owned PID18760、退出true |
| [inventory.csv](inventory.csv)、[source-hashes.csv](source-hashes.csv) | N04固定40候选全文均已读；RefHMap/mongotest当前读、其余38按前轮working hash复用；源文40/40不等于业务覆盖率或场景完成 |
| [coverage.json](coverage.json) | 图谱generation09-30仍陈旧；关键路径metadata_changed、新测试not_tracked，以当前源码补证，无记录缺口非完整保证 |
| [initial-harness.jsonl](initial-harness.jsonl) | 初版探针对BSON嵌套类型误当M而panic、使用lookupPath读取D造成假失败；已修探针并重跑，不计新RR |
| [正常回归与修复统计](../../../bugfix/evidence/noncore-bugfix-20261004-07/summary.json) | 十包race/vet与生成消费通过，和本页新未修问题分开 |

```powershell
& ./docs/review/evidence/noncore-review-20261004-14/Run-Review.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -OutputRoot <new-scratch>/review
```

[Run-Review](Run-Review.ps1)只overlay[RefHMap未知结果](refhmap_unknown_test.go.txt)和[mongotest契约](mongotest_contracts_test.go.txt)。脚本为证据采集，review exit1按预期保存；必须看JSONL，不能用PowerShell exit0代表测试通过。无产品写入、生产实例、模块修改或发布。

Redis反例真实执行原Lua，包装IRedis在执行后交给另一正式store写v3，再返回DeadlineExceeded模拟回复丢失；这验证未知结果分支，不是TCP断包/Cluster/HA实测。原Set允许建议性Stale，不声称整个Set本应具备CAS或线性化；缺陷是把未知成功重放为无条件DEL/HSET且返回nil。Mongo只测试公开替身，不冒称真实Mongo结果。事务反例先正常Update建立现有M嵌套再abort；输出隔离反例操作BSON解码的D，未依赖自造数据表示。
