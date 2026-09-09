# Roost Review 问题索引

本目录保存只审查、不修改源码的发现。框架范围为 core、kit、codegen。
历史修复账本仍见 [history/ledger.md](../history/ledger.md)，这里使用独立 RR 编号。

| 编号 | 优先级 | 仓库 | 问题 | 状态 |
| --- | --- | --- | --- | --- |
| RR-20260908-01 | P2 | kit | Session 忽略幂等账本 Create 的竞争失败 | 已修复（U-0154，kit v1.14.3）→ [bugfix](../bugfix/RR-20260908-01.md) |
| RR-20260908-02 | P2 | core | ReadThrough 取消等待不归还名额 | 已修复（U-0155，core v1.15.2）→ [bugfix](../bugfix/RR-20260908-02.md) |
| RR-20260908-03 | P2 | codegen | 单行 import 包拆分产生非法 Go 语法 | 已修复（U-0156，codegen v1.15.4）→ [bugfix](../bugfix/RR-20260908-03.md) |
| RR-20260909-01 | P2 | core/docs | 当前接入指南的版本与必需参数不一致 | 已修复（U-0157，core v1.15.2）→ [bugfix](../bugfix/RR-20260909-01.md) |
| RR-20260909-02 | P2 | kit | Session 冲突清理误删重新取得的 claim（ABA） | 已修复（U-0158，未发版）→ [bugfix](../bugfix/RR-20260909-02.md) · [第二轮复核](REVIEW-2026-09-09-02.md) |
| RR-20260909-03 | P2 | core | Assembly 停机未完成即遗失 Runtime，重试虚报成功 | 已修复（U-0159，未发版）→ [bugfix](../bugfix/RR-20260909-03.md) · [第三轮](REVIEW-2026-09-09-03.md) |
| RR-20260909-04 | P2 | codegen | 同包多个 Entity 生成重名注册符号，消费者无法编译 | 已修复（U-0160，未发版）→ [bugfix](../bugfix/RR-20260909-04.md) · [第三轮](REVIEW-2026-09-09-03.md) |

[问题详情](REVIEW-2026-09-08.md) · [复现附录](REPRO-2026-09-08.md) ·
[学习与验证记录](../review/REVIEW-2026-09-08.md)。

2026-09-09 已在最新提交复现前三项，并完成图谱查询、覆盖检查与过期路径源码补证。
见[最新复核及新增问题](REVIEW-2026-09-09.md)、[架构评估与验证](../review/REVIEW-2026-09-09.md)。

2026-09-09 下午四项全部修复，修改与修改方式见 [bugfix/](../bugfix/README.md)。

第二轮独立验收：前三个原代码问题的旧复现均已通过；Session 新清理竞态单列 RR-20260909-02。
RR-20260909-01 的主要修复已落实，当前版本快照及少量仓数措辞仍待对齐。
见[复核结论](REVIEW-2026-09-09-02.md)与[运行记录](../review/REVIEW-2026-09-09-02.md)。

2026-09-09 晚：第二、三轮的三项已修复（U-0158～U-0160），RR-20260909-01 的文档残留随 U-0161 收尾；见 [bugfix/](../bugfix/README.md)。
