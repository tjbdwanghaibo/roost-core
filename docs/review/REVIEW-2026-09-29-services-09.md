# Service 第九轮：修复后完成本阶段具名专项

2026-09-29；Core `bcebb80568559dcf895db0683cb85eee75b9ef53`，历史 Kit `f0e5b67aa43473c2bcabd2ae180c69559fd6e20e`、Codegen `1e028fa4b2927ffb5d7772b90440e7447dc665cf`。三仓更新无变化；当前单仓 Core 包含 service/kit/codegen。复用干净 detached 树，保留其他 agent 范围，使用 roost-bugfix、roost-review 和 codebase-memory Verify。

先完成[第七批](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md) RR-31/32 **2/2 修复验证**，再补上此前具名的 Mail 跨槽页、Match 历史聚合成本、购买持久 outbox/ledger 的归档界线、生成装配一致性。[RR-33 P2](../bug/REVIEW-2026-09-29-services-09.md)实际可复现，未修改其生产代码。[机制/待实施方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)与[完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)明确不同完成层级。

## 这次完成了什么

| 范围 | 已完成事实 / 场景 | 结论 / 限制 |
| --- | --- | --- |
| Activity Cluster | Mod 校验、单机兼容、真实 tagged Open/Notify/Owed/Attempt/Ack；第二 game 创建失败后重建恢复、token 不变 | RR-31 原触发关闭，非旧 prefix 自动迁移/强杀证明 |
| 购买 producer | HGet→当前 catalog（仅缺字段）→单 hash 首写脚本→校验 durable winner；lost reply/restart、不同 goods、下架、并发/交错、坏内容 | RR-32 原触发关闭；现有项目要合并 write-once 业务补丁 |
| Mail 批量读取 | Mod+公共构造器、固定 id，两次真实 Send、单/双封 List；Pipeline 跨槽/二进制/空/缺失/错误控制 | 新 RR-33；tagged 控制和现有工具候选通过，Mail 尚未修复 |
| Match 历史成本 | queueState/clone/Enqueue/Cancel/expiry、versionstore/Redis、旧请求 replay；两后端4历史规模、各16循环，空 live 仍有历史 | 旧容量观察补证；没有终态归档，没有生产 SLO/长期压力结论 |
| 购买消费/归档 | HGetAll→strict handler→ClaimPurchase/AddItem→HDel 的源码和新生成消费回归；提出 fulfilled 原子拒写与有界索引方案 | 设计尚未实现，不能删除永久 ledger；真实资产/资金未接入 |
| Session 外部资源 | 核对 Release→markReleased 与生成日志 collaborator，关联既有重叠回调证据 | 沿原契约观察，不重复登记；正式 allocator 幂等仍需业务验收 |
| 生成/装配 | 实际新 CLI 工程、generate、embed/steps/parse、完整 consumer 编译；原参数12 RPC -check | 正式生成一致性已核验，源码/toolchain 未降级 |

## 范围进度核算

[当前清单](evidence/service-review-20260929-09/inventory.csv)：tracked `service/`、`kit/service/` 生产 Go 文件仍 **100 路径（82非生成、18生成）**。相比第六轮4b0837d7，95 blob 相同复用已有主链记录；5变化为 Activity Mod、Platform Mod/store、Rank Mod/store，按第八/九轮实现和邻近调用复读。没有新增第11个功能域；辅助 split/integration/doc/metrics 路径保持原角色。

**10/10 域主链的本阶段有界整理、当前100路径的归类与差异核对、上述四项本机专项均完成。**不是本轮逐行重读全部100文件，不是100%行/分支/故障覆盖；未修新 RR-33 与外部验收另算。修复 RR-31/32 不升级全仓 K1/K2/K3 或另一 agent 的审查状态。Wanted 标题已全部分流/判定，无新的活动候选。

## 实际验证

- 修前旧 overlay：3 pass/5 fail；修后原业务断言 **8/8 pass**，购买 seam 适配到真实 Eval 写后丢回复。
- 新正式 regression：**33 叶子/37 事件 pass**；最终 service/versionstore/mods integration+race：**17包/909 pass事件/841 pass叶子、0 test skip/fail/build-fail**，metrics 无测试 package skip1。
- 消费者：**107 pass事件/100叶子**，3测试包，purchase 无测试包1；全 consumer 与全 Core 编译通过，定向 vet 通过。新 consumer 公网依赖解析起初受限，随后 local replace+实际 generate 成功；不是发布版本验证。
- demo 检查 **5/5**，RPC check **12/12**。使用原生成命令，保留环境/命令注释差异解释，不把 setup/STale 注释误报为功能故障。
- 新 review **11次叶子执行：10 pass、1预期反例 fail、0 skip/build-fail**。Mail页2叶子、Pipeline候选1、Match8。取样非race，先结束其他本轮Go工作，未与索引重建重叠；实际 sample 与限制见机制文档。

[修复结果](../bugfix/evidence/service-bugfix-20260929-07/RESULTS.json)、[审查结果/RPC](evidence/service-review-20260929-09/README.md)、独占 loopback Redis8.8.0 单机/3master/no replica。没有生产资产、渠道、allocator、HA/failover/分区、跨进程强杀、历史迁移或长期运行；这些条件明确未执行。

## 图谱证据与限制

项目 roost-core/root D:/whb_s/cube-core；本轮开始 ready、HEAD=bcebb805、full generation **2026-09-29T11:18:53Z**。历史 kit/codegen 独立项目 root/ready 核对，未把旧聚合 D-whb_s 当当前源码。graph search 相关分页读取完（Mail33两页、Match/clone、Pipeline、generator Run）；双向 trace 显示 Mail List→GetMany→IRedis.MGet、Match 主链→clone、RedisStore→NewStore 等关系，相关源代码直接补读。

图谱有同 qualified name 多 receiver/文件合并限制：Mail GetMany 的 snippet 返回 fake，Send 的 snippet 返回生成 capability，而真实业务在 redis_store.go/service.go；不能把这些 snippet 当生产实现。具体生产链以 exact 当前源码为准；接口 dispatch 的零 caller 不表示未使用。100 inventory 路径及新增依赖做 coverage；主树的新测试当时 missing、既有源码 metadata_changed，no_recorded_issue 并非完整证明。[coverage](evidence/service-review-20260929-09/COVERAGE.json)保留初始候选与补查记录；误猜的 state.go/redis.go 不存在，已弃用候选，实际 queue_store.go/redis/client.go 有补查/源码。

[SOURCE.json](evidence/service-review-20260929-09/SOURCE.json)与 inventory 保存实际 Git blob/字节 hash，明确 baseline+未提交工作内容。提交同步后以当前图谱 HEAD/generation 追加交付事实，未停止共享 MCP。

## 下轮入口

本阶段不再留“其余 service 尚未安排”的泛化待办。下一次 service bugfix 先实施 RR-33；历史容量/fulfilled/allocator/HA 等按学习文档的具体验收条件推进。源码整理已完成不能替代问题已修复/外部验证；已确认 RR-33 未修，不称 service 完全无缺陷。
