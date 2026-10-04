# N04 第二批复跑与真实 Redis 证据

源文main`1502f973`，RefHMap/mongotest/Redis产品未改；同工作树已修NC-13～15。Windows/Go1.27.0/GOWORK=off。[运行](../../REVIEW-2026-10-04-noncore-12.md) · [问题](../../../bug/REVIEW-2026-10-04-noncore-12.md)。

| 文件 | 内容 |
| --- | --- |
| [cache-redis.jsonl](cache-redis.jsonl) | 真实Redis14叶子=RefHMap8（5fail/3控制）+旧写修复6pass，不把修复控制当新RR |
| [integration.jsonl](integration.jsonl) | 原七Redis集成7pass/0fail/skip |
| [exits.json](exits.json)、[cleanup.json](cleanup.json)、[redis-version.log](redis-version.log) | integration exit0，review exit1；Redis8.8.0、owned PID10916退出true |
| [mongotest-review.jsonl](mongotest-review.jsonl)、[mongotest-exits.json](mongotest-exits.json) | 分页8叶子5fail/3控制，review exit1 |
| [review-regression.jsonl](review-regression.jsonl)、[review-exits.json](review-exits.json)、[review-vet.log](review-vet.log) | 三测试包race34pass/0fail/skip、五包vet0；Mongo/KitMongo无测试 |
| [inventory.csv](inventory.csv)、[source-hashes.csv](source-hashes.csv) | 固定40候选累计39全读、来源/blob/working hash；mongotest1–595有界读取，不能当40/40 |
| [coverage.json](coverage.json) | 收尾再核对40路径与cache/redis/mongo/migration四scope；generation仍09-30，所有路径metadata_changed，以已读当前源码/blob补证，无记录缺口不等于完整 |
| [共同统计](../../../bugfix/evidence/noncore-bugfix-20261004-06/summary.json) | 叶子不重复计父项；不同执行不叠成业务覆盖率 |

```powershell
& ./docs/review/evidence/noncore-review-20261004-12/Run-Redis.ps1 -GoExecutable <go.exe> -RedisExecutable <native-redis-server.exe> -RedisCliExecutable <redis-cli.exe> -OutputRoot <new-scratch>/redis
& ./docs/review/evidence/noncore-review-20261004-12/Run-Mongo.ps1 -GoExecutable <go.exe> -OutputRoot <new-scratch>/mongo
```

[Run-Redis](Run-Redis.ps1)按独占临时loopback端口、无持久化、隐藏窗口启动server，PING入场后overlay[RefHMap](refhmap_review_test.go.txt)和[旧写控制](cache_redis_review_test.go.txt)，finally只停止本次process并确认退出。需真正server而非shim；MSYS相对redis.conf+专属working directory。坏布局用例清理只删自己已知的root/registered测试键，不执行业务payload指向的任意键。脚本保留review exit1并返回日志，原七项integration失败会throw；必须检查JSONL，不能只看PowerShell exit0。

[Run-Mongo](Run-Mongo.ps1)只overlay[公开mongotest分页](mongotest_review_test.go.txt)，之后跑当前正常回归/vet，无Mongo服务端。review exit1与现有回归exit0分开。重跑当前未修版预期新审查十失败/六控制；不能将未知类型panic恢复成pass。本轮受控panic由testing.Error明确记fail。

脚本均不修改产品、模块、仓库技能、生产数据，不安装软件、不提交发布。单节点Redis不代表Cluster/HA/WAITAOF，mongotest不代表真实Mongo；网络故障/长稳/性能未执行。日志是小型脱敏事件证据，不提交Redis服务器原始大日志或二进制。
