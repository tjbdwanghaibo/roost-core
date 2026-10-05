# 第27轮：Service 恢复增量与 Saga 并发发布

起点/初次拉取后 `1272671557c57d6abc97b99471335e3fe9c3f299`，main 干净，fetch/pull --ff-only 后没有新主分支提交。无新 Wanted；初次核对 canonical coding/bugfix/optimize 完整包与本机 LF 内容一致。使用 roost-review/bugfix/coding 接续第26轮停点，不重做旧 Service 十域。

收尾 fetch 发现 `b4152f15` 仅更新 coding/bugfix 的反复缺陷方向判断规则；已整合该提交，备份并同步本机两份完整 skill 包（各3文件），逐文件 LF 核对一致。补写下文方向判断；整合后28材料路径源码摘要与测试时完全一致，未重复执行产品回归。文档相对链接79个目标、4个复跑脚本语法及Collector实际运行通过；最终 diff/格式检查通过。

## 本轮新增审查与修复

| 场景 | 原结果/修复 | 最终证据 |
| --- | --- | --- |
| Mongo outbox候选读后另一发布者Claim/Nack | 未来next_attempt_at仍被原扫描者领取，P2 [NC-41](../bug/RR-20261005-NC-41.md) | 原子领取同时检查due/lease；3叶子，含合法到期、旧Ack/Nack围栏及排空 |
| Open复用存量计划 | 外层参数合法，持久Intent异键/collecting/空expected仍写入，P3 [NC-42](../bug/RR-20261005-NC-42.md) | 3形状先拒绝且窗口/活动无副作用，人工修复后公开入口成功/重复正常 |
| sweep存量opening键 | entry和Intent共同非法/跨组时仍执行，跨组写后还阻塞正常组 | 追加[RR-20261001-09残余](../bugfix/RR-20261001-09.md#复核后的补修2026-10-05)，3形状跳过/保留，正常到期活动完成 |
| 合法恢复与诊断 | 持久intent不能被新参数替换，legacy可补合法计划；坏Key的GroupID不能决定缓存归属 | 2合法/legacy控制，1所属窗口清理控制；不自动清生产坏记录 |
| ReconcileProgress | 读pending后另一个Service增加新证明 | 1新控制：旧对账只清旧证明，第二次对账收敛；未改admin |
| account/chat/global/Mail | 读取相对db4b7009的具名恢复/Gap/租约API删除与Mail比较增量，指标随链核对 | account/chat/global既有普通race复跑；global生成RPC/实际App.Live调用链尚未全审，留下一入口；Mail另外两包回归分列 |

合计13新正式叶子。原始9叶子7行为红/2控制，旧产品overlay还原两个生产文件同样7红2控制；最终13绿。相关race矩阵420 pass/2环境skip/0fail，根包14、全仓build、相关vet通过。正式DAO/Entity CLI生成periodic/on_change消费2race叶子通过；没有改生成形状，不以该两叶子宣称Activity/Saga外部协议验收。[红绿/逐叶/复跑](../bugfix/evidence/noncore-bugfix-20261005-17/README.md)。

Mail增量补跑 `./service/mail ./kit/service/mail` race共137叶子/0fail/0skip；不是新137项或十域重新完成，完整真实EnvelopeStore/外部发货/未知结果组合仍按交接接续。

## 设计、性能和证据限制

[实现学习与漏检复盘](IMPLEMENTATION-OUTBOX-CLAIM-AND-OPENING-RECOVERY.md)。原包保留，复用Store/CAS/lease和指标；无新公开API/BSON/wire/迁移，无新I/O调用、goroutine或自动重试。NC-42仅收紧坏持久计划：ErrConflict后保持数据，修可信原计划再试；已污染活动不自动修复。RR-09旧writer排空的升级前提不变。

图谱Tier2 Verify，roost-core ready32285/209828，generation2026-09-30落后HEAD；[28材料路径coverage/摘要](evidence/noncore-review-20261005-27/README.md)均源码补证。receiver同名、旧行号截断与标准库误边不当依赖证明；无解析缺口不等于完整，没有刷新或停止共享索引。

mongotest与Memory不能证明真实Mongo/Redis/Cluster/未知提交/HA；两skip为Activity Cluster生命周期/部分恢复。没有性能对照、长期容量或Unix进程树验收，不查询/等待GitHub CI，不发版/tag/部署。

方向判断：Saga 的 NC-37/38→39/40→41 连续暴露持久身份/适配器执行边界，Activity 的 RR-09→本轮残余/NC-42 则重复遗漏恢复入口的一致校验。建议统一契约矩阵和持久计划验证，当前证据不支持替换整个架构；本轮复用 helper，未继续叠加重试或自动坏数据修复。编号、已有提交、候选方向及代价见[学习文档的方向判断](IMPLEMENTATION-OUTBOX-CLAIM-AND-OPENING-RECOVERY.md#反复缺陷的方向判断)。

## 当前停点和完整交接

N06仍**部分场景完成**，不计completed/15。下一优先global RPC/生成routing/server_run/Mod/Redis与App.Live替代真实消费，随后Saga跨协调器、晚receipt/TTL与发布未知结果组合，再转N07/N08。不以本轮包绿关闭全部Service/Saga。

按用户追加要求，已把全部非核心剩余域、三大核心工作线接口与外部环境专项写为[后续review交接清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md)。另一agent可从N06 S4直接接续，不依赖聊天历史；本轮候选CSV只作范围定位，不当覆盖分母或逐文件未审断言。
