# Roost Service 第十轮：修 Mail 后继续 Pipeline review

2026-09-29；依用户“bugfix，然后继续review”，先RR-33，再邻接新内容。Core/main与复用隔离树同步至`68cf87fc79bb3d63c970bb0ce1439dd05466f039`，fetch无新增；历史Kit=f0e5b67、Codegen=1e028fa，均已归并Core，不再当现行实现。另一agent其他模块不改。

## 本轮结果和进度

**RR-33 1/1根因修复/声明场景已验；新增RR-34 P2独立Pipeline错误汇总缺陷未实施。**[修复兼容](../bugfix/RR-20260929-33.md)、[新问题与实施交接](../bug/REVIEW-2026-09-29-services-10.md)、[机制学习](IMPLEMENTATION-SERVICE-MAIL-BATCH-AND-PIPELINE-ERRORS.md)。Wanted标题均已分流/判定，本基线没有新增活动条目。

当前10/10service域主链有界整理继续完成；[100生产路径清单](evidence/service-review-20260929-10/inventory.csv)中97blob与上轮相同复用、3Mail变化复读。共享driver/client/pipeline/recovery和cache4个pipeline入口按本批错误与资源生命周期补查；两个正式integration文件新增。没有把测试数量、graph覆盖或这100路径核算变成逐行正确率。

| 本批有界场景 | 实际证据 | 当前结论 |
| --- | --- | --- |
| 原Mail跨槽2封 | 同一overlay修前untagged fail/tagged pass；修后2叶子pass | RR-33原触发关闭 |
| 正式Mod/capability分页 | 真Cluster无tag跨3owner、有tag1owner，16Send→两页各8无重漏→Reserve/Cancel | 装配未被私有实例替换，当前读取真实可用 |
| Core批读边界 | present/absent/repeated、empty/badJSON/wrongtype、过期/cancel；100上限/先校验；batch/future分别故障 | 逐future保留错误，无部分成功map |
| Pipeline生命周期 | 真backend Discard、两次Exec、旧future不被覆写 | 列明控制通过；不保证并发共享安全 |
| Pipeline错误组合 | 首missing→HSet WRONGTYPE，standalone和Cluster同owner均Exec=nil | RR-34两个反例确认，仅交接 |
| 当前源码影响 | cache loadHashes查future；cache写批次没有前置缺失GET；EvalBatchDurable原生pinned pipeline/逐结果 | 未复现这些调用者受RR-34影响，不推定仓外安全 |

## 验证与限制

[执行摘要](../bugfix/evidence/service-bugfix-20260929-08/RESULTS.json)保存红/绿/正式/完整/编译的日志hash、[实际源码](../bugfix/evidence/service-bugfix-20260929-08/SOURCE.json)保存Git blob及rawSHA。formal Mail `integration/race/count2` 314pass事件/280叶子执行，0fail/skip；完整service+driver18包965pass事件/888pass叶子、0fail/build-fail，3个Toxiproxy用例skip、servicemetrics无测试package skip1。Core/既有正式生成consumer编译、三包vet、12RPC只读check通过。RPC复跑最初PowerShell逗号参数被转为数组，修正引用后12check完成；该脚本传参错误不是行为反例。

新review3叶子：2预期反例fail、1控制pass，0skip/build-fail。Windowsamd64 Go1.27.0、Redis8.8.0 standalone和三master16384slot，无replica。HA/真实网络丢回复/Toxiproxy/生产升级/真实渠道資源/長稳没有执行，不混入已验场景。

## 图谱证据

Verify；独立roost-core，session确认ready/HEAD68cf87fc、full generation13:13:03Z。search→both trace→snippet均处理到无分页；GetMany snippet因receiver同名实际返回fake，直接补读production，trace也有interface/receiver误归，未以图谱0caller作安全依据。初始路径检查metadata_changed、两个新正式测试main尚不存在/go.txt未tracked，当前worktree精确源/实际测试补证；[原coverage](evidence/service-review-20260929-10/COVERAGE-BEFORE.json)保存代际与范围。最终索引状态见交付追加，不将Git HEAD变化当generation更新。

下一次bugfix：先RR-34所列aggregate/command/future行为，复用现有driver；之后按具名外部验收/归档设计继续。旧主链不从头重扫。提交推送是流程收尾，未发版/部署/数据迁移。
