# Service 第十一轮证据与范围

本轮基线328fc6ef加共享driver修复；Service生产源没有变化。100路径[清单](inventory.csv)逐行计算当前Git blob，与第十轮100/100一致，沿未变内容复用既有主链证据，不当逐行/分支覆盖率。相关新源摘要和红绿/整体结果在[第九批bugfix证据](../../../bugfix/evidence/service-bugfix-20260929-09/README.md)。

Tier2图谱项目roost-core，主树root D:/whb_s/cube-core；[开始coverage](COVERAGE-BEFORE.json)覆盖100路径+9邻接证据路径和3scope，generation14:09:57Z；旧PowerShell两脚本partial直接全读，新测试未入主树。metadata_changed/missing由当前源内容、blob对照和真实执行补证。run/维护搜索133条分页100+33、专门runner29条无剩余；双向depth1 trace有同名误连，源码为准，没有作动态调用全覆盖承诺。

[当前12次RPC检查](RPC-CHECKS.json)。本轮定向58叶子执行/70事件、整体951pass叶子/1035事件，3Toxiproxytest skip和1无测试package skip分开计；Core/consumer编译和vet通过。交付后索引刷新证据在此补记，不改变运行源码或重跑文档测试。

[阶段结论与剩余真实环境事项](../../REVIEW-2026-09-29-services-11.md) · [完成矩阵](../../SERVICE-REVIEW-COMPLETION-2026-09-29.md)。不将本机三master无replica、hook模拟错误或示例effects称为生产验收。
