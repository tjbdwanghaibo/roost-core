# Service 第八轮：先修复，再继续审查

2026-09-29；基线 Core `db642494f7dbe594539244dbdba356490d466a51`，Kit `f0e5b67aa43473c2bcabd2ae180c69559fd6e20e`，Codegen `1e028fa4b2927ffb5d7772b90440e7447dc665cf`；fetch/快进无变化，主树与复用 detached 树开始均干净。实际框架、kit、codegen 已合入 Core，本轮生产/模板审查都在 Core，历史两个独立仓不当当前实现。

先完成[第六批 bugfix](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md)：**RR-28/29/30 三项已修复、声明场景验证**；然后继续 Activity Cluster、Mail 存储边界和生成 purchase producer→drain→strict handler→ClaimPurchase 链路。[两个新 P2 与实施交接](../bug/REVIEW-2026-09-29-services-08.md)未修复；[进度](PROGRESS.md)与[机制学习](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md#第六批修复后的实现与第八轮学习)同步。

## 范围与证据

| 范围 | 当前事实 / 实际验证 | 未验证 |
| --- | --- | --- |
| versionstore→Platform Retire | EXISTS+ZREM 原子缺失检查；迟到创建和对象重建、空/坏字节、执行前/后失败重试；单机/带 tag Cluster | 历史已丢 member 自动对账、强杀/复制 |
| mods→Rank/Platform Cluster | 首 tag 校验；无 tag/空/未闭合/空首对后有效对拒绝；真实 tagged 业务生命周期通过 | 仓外直接构造器/自定义拓扑、跨 slot 分片 |
| Activity Mod→Notify→ensureDispatches | Open 能成功，4 坏配置聚合 complete 后 dispatch=false/CROSSSLOT；有效 tag→Attempt/Ack/retire 通过，RR-31 | 旧数据迁移、所有部分多 game 与 HA 故障组合 |
| Mail Mod→stores | single-key CAS 无统一 tag 必要；不能把 Activity 缺口泛化给全服务 | driver MGet 跨 slot 业务页未新增实跑 |
| purchase generated producer/consumer | 模板与正式消费文件核对；真实 Redis HSet 写后丢回复、独立二进制 catalog 升级覆盖 grant，RR-32；永久 ledger/HGetAll 源码边界 | 本轮未执行实际资产/资金、不以编译证明 durable drain 端到端 |

## 实际执行

- 上轮原 overlay 修前 9/15 pass、6 fail；本批修后不改旧反例 **15/15 pass**。
- 新正式定向 race **43 叶子/51 测试及子测试事件通过**；完整 **17 包/826 测试及子测试事件/764 叶子 pass**，test skip=0，servicemetrics 无测试 package skip=1，fail/build-fail=0。[修复摘要](../bugfix/evidence/service-bugfix-20260929-06/RESULTS.json)。
- 全仓仅编译和改动四包 vet 通过，正式消费 Platform/purchase 编译、handler/game 既有测试通过；RPC/DAO 签名与生成文件不变，本轮未冒称重新做所有 RPC codegen 检查。
- 第八轮独立 overlay：Activity 1 控制 pass/4 fail；purchase seed/control 2 pass，升级 1 fail；合计 **3 pass/5 预期 fail/0 skip/build-fail**。原始初次消费测试缺 go.sum 的 setup failure 未当作 RR；临时消费模块以 -mod=mod 补实际 import 后正式复跑。[最终结果](evidence/service-review-20260929-08/RESULTS.json) · [可复跑入口](evidence/service-review-20260929-08/README.md)。

Redis 8.8.0 Windows loopback 单机 16419、三 master 16416/16417/16418、16384 slots、cluster_state=ok；独占目录/端口，无真实外部资源/replica。catalog 改动只在临时 overlay 的第三个二进制，不提交消费或正式模板改动。

## 图谱与源码版本

Codebase-memory Verify；nearest Git project 为 roost-core/root D:/whb_s/cube-core，index_status HEAD 与同步基线一致，generation **2026-09-29T08:12:07Z**。Kit/Codegen 历史项目 root/status ready 已核对，但非当前实现。结构定位 search_graph、调用双向 trace、snippet；ensureDispatches 第二页已读取，相关 search 分页完成。模板图节点只是 file/module 摘要，snippet 截断不能代表整个源码，读取全部相关模板补证。

[coverage](evidence/service-review-20260929-08/COVERAGE.json)含 29 个源码/正式测试依赖及四 scope：主树三份新正式测试在检查时 missing，既有路径 metadata_changed；flags_test 模板 parse_partial 64/80 的源码已补读，未据其图谱证明 purchase 边界。隔离树实际源码、回归与[Git blob/字节摘要](evidence/service-review-20260929-08/SOURCE.json)是本批 material 结论证据，不声称新测试当时已经索引或 scopes 干净证明完整。

测试时 HEAD 是基线，修复尚在工作树；记录 source_base+working_changes+源文件摘要，不把 HEAD 写成已经包含修复的 commit。提交同步后以新 index_status/coverage 追加事实，不停止共享 MCP 或清除未知锁。

**收尾事实**：实现与本轮记录提交为 `5f81e5c80ef3c8b7f517ab827157e3fc5fbc31d6`，已正常推送并通过 ls-remote 验证 origin/main；干净主树快进同步。full index_repository 成功返回 indexed，后续 index_status ready/HEAD=5f81e5c8，generation **2026-09-29T11:18:53Z**、recording complete、31125 nodes/206239 edges；新方法与共享验证 search 命中无更多页。[刷新后 coverage](evidence/service-review-20260929-08/COVERAGE-AFTER.json)的三个新测试均已纳入，29路径仍 metadata_changed、flags_test仍partial，因此保留源码/测试证据，不宣称metadata freshness已完全消除。独占四个Redis实例在核对CONFIG GET dir后均已关闭；共享MCP未停止。此次提交不等于发布/部署。

## 当前进度与下轮入口

Service 10/10 域、100 路径的**既有主链有界整理**保留；本轮为具名专项，不重新计算为逐行覆盖或全局 100% 无 bug。上轮三项 3/3 原触发关闭，本轮新增两项 0/2 实施。Wanted 标题均已分流/判定，本轮没有新增活动项；历史内容不再重复改结论。

下一入口：RR-31 接现有 Cluster 校验及旧 complete 恢复；RR-32 固定首次 grant 内容、producer/consumer 升级与 fulfilled 归档设计；随后 Match 历史聚合成本、购买 HGetAll/ledger 容量。当前 review 不授权实施这些新问题。实际渠道/资产回执、HA/强杀/长期容量与 K1/K2/K3 原专项仍保留其状态。
