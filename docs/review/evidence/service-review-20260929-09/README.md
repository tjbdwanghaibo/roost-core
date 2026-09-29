# 第九轮证据与复跑

source_base=bcebb805 加本轮修复，当前100生产路径在 inventory.csv；源码摘要见 SOURCE.json，图谱初始/补查见 COVERAGE.json，最终实际 review 的11叶子为10pass/1预期fail。大日志保留在本机隔离目录，不提交二进制/缓存/凭据。

先启动自己隔离的 Redis8.8.0 单机与3master完整16384slot Cluster，核验 state ok；不让成本取样与 Go 编译/race/索引重建并跑。原执行为Windows Go1.27.0，单机16429，Cluster16426/16427/16428。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
./docs/review/evidence/service-review-20260929-09/Run-Review.ps1 `
  -RedisAddress 127.0.0.1:16429 `
  -ClusterAddresses '127.0.0.1:16426,127.0.0.1:16427,127.0.0.1:16428' `
  -OutputDirectory D:/whb_s/.tmp/service-review9-replay
```

脚本只写输出目录、设置后还原测试环境，通过 Go overlay 读取 mail_test.go.txt/match_test.go.txt；不会改生产源码、启动/停止 Redis、提交、推送。Mail 期望 untagged fail/tagged pass，其他候选/观察全pass，无 skip/build-fail；Mail已修复后此旧 review 脚本预期要改，不能把0exit当成该历史反例仍成立。RESULTS.json保存每项实际结果/日志hash，RPC-CHECKS.json保存12个按原 go:generate cwd/参数的只读比较。

`-RPCOnly` 可以在不重复成本取样的情况下单独检查生成物。部分环境 Go 子进程需模块缓存写权限，隔离worktree Git safe.directory 可只在本次进程中传递，不改全局设置。初次根目录参数比较造成 regenerate header 注释差异，最终使用源码原声明排除；CRLF 已由生成器自身规范化。

Match直接seed终态状态，不计seed耗时；每规模实际16个Enqueue/Cancel，旧request恢复和最终map/Waiting计数有断言。elapsed是短取样总耗时、TotalAlloc是Go分配，不当SLO/p99/生产TPS。源内容与原始输出保留，未测 Matches 历史或长期公平。
