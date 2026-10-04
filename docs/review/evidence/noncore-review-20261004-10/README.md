# N04 第一批：反例、控制与独占 Redis

N04源文main `3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956`，Windows/Go1.27.0，GOWORK=off。产品未改。[运行](../../REVIEW-2026-10-04-noncore-10.md) · [问题](../../../bug/REVIEW-2026-10-04-noncore-10.md)。

| 证据 | 结果 |
| --- | --- |
| [review.jsonl](review.jsonl)、[exits.json](exits.json) | 普通overlay26叶子：9fail/16控制/1仅容量观察，review exit1是预期反例 |
| [regression.jsonl](regression.jsonl)、[vet.log](vet.log) | 五测试包race113 test pass事件、0fail/7skip，八包vet exit0；三个包无测试 |
| [summary.json](summary.json)、[skip-reasons.json](skip-reasons.json) | 父测试不重复计叶子，逐项环境skip原因保留 |
| [inventory.csv](inventory.csv)、[source-hashes.csv](source-hashes.csv) | N04固定40候选的20完整读取/具名片段/本批未读；实际证据文件blob或SHA256 |
| [redis-integration.jsonl](redis-integration.jsonl) | 原7个skip在专属真实Redis全部通过，0fail/skip；后补结果不覆盖原skip |
| [cache-redis.jsonl](cache-redis.jsonl) | 正式cache/driver/Redis6叶子2fail/4pass，补证NC-15 |
| [redis-exits.json](redis-exits.json)、[redis-cleanup.json](redis-cleanup.json)、[redis-version.log](redis-version.log) | integration exit0，cache review exit1；owned PID已退出；Redis8.8.0 |
| [redis-first-cleanup.json](redis-first-cleanup.json) | 首次MSYS路径启动失败实例已退出，没有算入测试通过 |

## 可复跑入口

```powershell
& ./docs/review/evidence/noncore-review-20261004-10/Run-Review.ps1 -GoExecutable <go.exe> -OutputRoot <new-scratch>/n04
& ./docs/review/evidence/noncore-review-20261004-10/Run-Redis.ps1 -GoExecutable <go.exe> -RedisExecutable <native-redis-server.exe> -RedisCliExecutable <redis-cli.exe> -OutputRoot <new-scratch>/redis
```

[Run-Review.ps1](Run-Review.ps1)只overlay两个[cache](cache_review_test.go.txt)/[migration](migration_review_test.go.txt)测试到虚拟路径；相关已有包随后race/vet。需自行配置可用模块缓存，勿修改go.mod降级。普通反例9fail中Fatal2、Layered3、Stale4；Migration四控制不是业务持久事务验证。1ns TTL元数据日志L1=1/L2=1/expiry=1000只是观察项。

[Run-Redis.ps1](Run-Redis.ps1)为Windows专属server选择空闲loopback端口，禁止持久化，隐藏窗口、等待PING，运行原7项和[cache实际Redis探针](cache_redis_review_test.go.txt)，finally只按它创建的process句柄退出。传入真正server可执行文件，不要用不能追踪子进程的shim；MSYS版本使用相对配置路径。该脚本不连接或清理外部业务Redis，不安装软件，不修改源码/模块，不提交发布。

首次绝对Windows配置路径在MSYS中被拼到工作目录，server在readiness前退出；路径改正后新实例通过。两次输出目录独立，保留首次cleanup元数据；本机路径错误不当产品红测。port/PID只是本次日志，不保证下次相同。

Raw/Hash已实际拒绝v4但返回nil，JSON同条件返回ErrStaleWrite，三种v6通过。真假Redis日志同根因，不能按11个失败叶子声称11bug。单节点测试不能证明Cluster同槽、failover、网络未知结果或Lua所有失败分支；Mongo/etcd真实部署及长稳未执行。空vet.log表示真实执行无输出，exit存于exits，不是未运行。
