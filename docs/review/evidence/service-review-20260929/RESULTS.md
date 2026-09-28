# 本轮证据摘要

当前源代码基线 `6b73289cefd4e300a75f2550d4d68646172294ad`。初始 `b8a401ab` 到该提交的两个 service 路径无变更。

- 生产 Go 清单 96 个路径，生成文件 18 个；清单列的是证据方式，不是每行已经审完的认证。
- 图谱 project=roost-core，generation=2026-09-20T01:02:19Z；全量清单 check_index_coverage：20 metadata_changed，76 not_tracked，均以当前源码补证。scopes service/kit/service 无 recorded gap 不代表完整。
- 最终基线 race/integration：15 包 pass，1 别名包 no tests，测试级 pass 事件 684，测试级 skip/fail 0。包括补跑四个 ROOST_REDIS_TEST_ADDR 门禁。
- 9 个 TestReview 行为反例修前失败；rank 的 3 个使用本机独立 Redis。其余使用现有内存 helper 与受控时序/单次故障。没有 Go race detector 警告。
- 12 次 servicerpc -check 全部 exit 0：account/chat/global/activity/platform/rank 各 all 一次；mail/match/session 各 transport 和 assembly 一次。
- RR-09 session 是源码接线证据，不计入第十个动态反例。

完整方法和边界见 [运行报告](../../REVIEW-2026-09-29-services.md) 及 [复跑说明](../../../bug/REPRO-2026-09-29-services.md)。测试源码以 .go.txt 保留；Run-Repro.ps1 已在隔离工作树实跑验证，返回 1，自动清理六个临时 Go 测试。修复后契约如有合法变化，须保留修前触发证据并更新契约断言。
