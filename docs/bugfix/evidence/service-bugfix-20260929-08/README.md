# RR-33 可复跑证据

基线68cf87fc加本批源码；[修复说明](../../RR-20260929-33.md)。RESULTS记录修前原overlay（1fail/1pass）与修后、正式count2、18包race和编译等实际结果；SOURCE记录执行时内容。原日志/二进制在被忽略的本地`.tmp`，不要求另一机器拥有日志；这里提供源码与重跑入口。

先创建自己隔离的standalone及三master Cluster，确认16384slots/state=ok。本轮用16439和16436–16438，前缀每次唯一。脚本不启动/停止依赖、不提交代码：

```powershell
./docs/bugfix/evidence/service-bugfix-20260929-08/Run-Verify.ps1 -RedisAddress '127.0.0.1:16439' -ClusterAddresses '127.0.0.1:16436,127.0.0.1:16437,127.0.0.1:16438' -OutputDirectory '<自己的临时目录>'
```

当前树预期original两叶子pass、正式两包count2全部pass且0skip。复现修前红测：在68cf87fc单独checkout使用第九轮`mail_test.go.txt` overlay，运行`go test -count=1 -json -overlay <overlay.json> -run '^TestReview9MailClusterPage$' ./kit/service/mail`；预期untagged CROSSSLOT、tagged pass，不用旧第九轮全runner重复Match性能。

完整命令和3个外部Toxiproxy skip见[本轮记录](../../../review/REVIEW-2026-09-29-services-10.md)。RPC-CHECKS为12个实际只读check；生成消费者此次仅编译。生产key/JSON/wire未变，自定义窄Redis客户端Pipeline源码兼容要求见修复说明。
