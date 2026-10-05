# NC-39/40 红绿与 N06 取消恢复证据

2026-10-05；基线 `cb11be902ac7da98c5818743607bf3b50772aa7f`，Windows/amd64 Go1.27.0；`GOWORK=off`。[本轮](../../../review/REVIEW-2026-10-05-noncore-26.md) · [机制](../../../review/IMPLEMENTATION-SAGA-START-IDENTITY-AND-CANCELLATION.md) · [coverage/源码摘要](../../../review/evidence/noncore-review-20261005-26/README.md)。

| 执行 | 通过叶子 | fail | skip | 解释 |
| --- | --- | --- | --- | --- |
| 初始11反例 | 0 | 11 | 0 | 8启动身份、3完成异路由，全部落在业务承诺 |
| 旧产品overlay | 0 | 11 | 0 | 还原4生产文件，屏蔽2引用新字段的控制文件，只跑原API反例 |
| 最终正式 | 24 | 0 | 0 | 11反例+8兼容+5事务/普通inbox |
| 原生取消/屏障 | 2 | 0 | 0 | 普通取消保留lease、late receipt恢复；明确fenced释放并换token |
| 相关race矩阵 | 586 | 0 | 0 | Saga/Kit、DataEngine边界、NATS、metrics、mongotest；不是全仓覆盖率 |
| 根包 | 14 | 0 | 0 | 核心依赖/门禁 |
| codegen/internal/roost普通测试 | 280 | 0 | 10 | 已有Git Bash/Go PATH与独占TEMP；不是整个codegen树或Unix压力验收 |
| 正式CLI生成双同步模式 | 2 | 0 | 0 | periodic/on_change，DAO→Entity→正式消费者race |

不同运行数量不相加。build/vet/glsvet退出0见[checks](checks.json)；[codegen执行](codegen-result.json)、[vet](codegen-vet.json)、[生成结果](generated-sync.txt)、[生成退出码](generated-sync-exit.txt)。最终注释版编译另见[final-build](final-build.json)/[注释静态](final-comments-vet.json)。

[完整叶子清单/具名skip](results.json) · [初始红文本](red.txt) · [overlay红文本](overlay-overlay-red.txt) · [overlay入口/退出](overlay-result.json)。编写控制时误用测试替身方法名的[编译失败](fixture-build-failure.txt)单列，修正为现有Attempts后重跑通过；不作为产品缺陷红。没有删用例、放宽门禁或把skip算pass。

十个codegen skip仍为Windows/POSIX/外部环境限制，详见results中的codegen.tests；Unix tagged进程树测试在本机不编入，不会以skip出现，须在Linux/macOS另跑。cb11be90只核对源文/普通包，本机未独立重做作者压力测试。

## 可复跑入口

交付正常整合 Nest U-0279 `47a9132c` 后，33 材料路径 LF 摘要全部一致。最终追加 Nest/Saga/Kit Saga race **534 pass/0 fail/0 skip**、根包14及全仓 build/相关 vet/glsvet；[退出码](delivery-checks.json) · [逐叶结果](delivery-results.json)。两批矩阵包范围不同，不相加。作者生成工程千轮/负载压力未独立重跑，本轮保留 Unreleased，不查询/等待 GitHub CI。

在干净/可控源码上，PowerShell运行：

```powershell
& ./docs/bugfix/evidence/noncore-bugfix-20261005-16/Run-Repro.ps1 -Scratch D:/whb_s/.tmp/nc39-40-red-new
& ./docs/bugfix/evidence/noncore-bugfix-20261005-16/Run-Verify.ps1 -Scratch D:/whb_s/.tmp/nc39-40-check-new
& ./docs/bugfix/evidence/noncore-bugfix-20261005-16/Run-GeneratedSync.ps1 -Scratch D:/whb_s/.tmp/nc39-40-gen-new
```

Run-Repro的exit1需核对11行为叶子与文本，不把编译失败当红；Run-Verify按退出码遇错停止，Run-GeneratedSync使用正式DAO/Entity CLI，仅生成到新的scratch。追加控制命令：`go test -race -count=1 -run 'TestStartIdentity|TestNestCompletionRejectsForeignSagaRouteBeforeMutation|TestSagaCompletionTransactionCancellationAndRetry|TestMongoCommandInboxCancelledBusinessWriteCanRetryAtomically|TestNativeStepCancellationAndFenceRecovery' ./saga`。

本次原始JSON日志/生成消费在 `D:/whb_s/.tmp/noncore-review-20261005-26`；只提交小型文本/汇总，不提交缓存、二进制或凭据。[Collect-Evidence](Collect-Evidence.ps1)读取现有日志生成本表依据与33源码LF哈希，不自动重跑/提交/推送；未来复跑使用自己的路径。

## 验收边界

MongoStore/普通inbox使用mongotest私有事务快照；native两控制注入权威receipt，JetStream为替身。本机证明状态/回执/outbox、BSON身份与消费恢复，不能证明真实Mongo未知提交、NATS ACK/Term/重连、跨进程竞争或HA/容量。旧writer/自定义Store丢新增字段与可信历史回填另审；不自动迁移生产记录。

N06仍部分完成，Service相对db4b7009的[增量路径](../../../review/evidence/noncore-review-20261005-26/service-delta-paths.txt)只是下一任务清单。没有查询/等待GitHub CI，也没有发版/tag/部署。
