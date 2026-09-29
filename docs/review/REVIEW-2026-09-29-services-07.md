# Service 第七轮：维护竞争与 Redis Cluster（2026-09-29）

继续第六轮列出的 service 专项，完成 Platform pending 清理/延后与并发回调、Rank Cluster 装配及 Reset/Remove 交错、Match 终态保留与 sweep 预算含义。**确认三个新缺陷，均未实施；有界主链整理仍完成，不能称 service 已无 bug。**

[问题与实施交接](../bug/REVIEW-2026-09-29-services-07.md) · [机制学习](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md) · [跨轮进度](PROGRESS.md) · [完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [复跑证据](evidence/service-review-20260929-07/README.md)。

## 同步与旧修复

| 仓库/远端 main | fetch/pull 前 → 后 | 状态 |
| --- | --- | --- |
| cube-core / roost-core | 1625fee0bf0884792acf8520fd5bb26183dd0b64 → 相同 | 干净；实际审查复用 detached `D:/whb_s/.tmp/review-service-20260928` 同 SHA |
| cube-kit / roost-kit | f0e5b67aa43473c2bcabd2ae180c69559fd6e20e → 相同 | 干净、pull --ff-only 无新增 |
| cube-codegen / roost-codegen | 1e028fa4b2927ffb5d7772b90440e7447dc665cf → 相同 | 干净、pull --ff-only 无新增 |

同步在本轮测试之前完成；三仓远端身份与根已核对。框架 service 实际源码在 core 单仓；另两仓只核对基线，不计本轮新审范围。源码相对上轮未变：第六轮之后只有文档提交。读 bug/bugfix 索引及相关 ledger，未发现新增修复说明；上轮 RR-25/26/27/旧 Opening 的 4/4 结论沿已执行的第六轮证据，不再假称重跑全部 830 个事件。当前四包完整回归重新包含 Grouping 和 Rank 正式修复测试；Activity/Chat 沿源码未变的原证据。

## 新增有界范围与场景分母

| 范围/不变量 | 本次实际验证 | 状态 |
| --- | --- | --- |
| Platform 缺失判断→退休→并发回调→下一次 due→重建 | 2 条真实 Redis 失败；ghost-only、既有 paid、终态后 defer 3 控制通过 | RR-28 P1，0/1 修复 |
| Platform hash tag 准入→service Mod→driver→订单/index CAS | 空 tag/未闭合 tag 2 失败，有效 tag 1 正常控制通过 | RR-30 P2，0/1 修复 |
| Rank hash tag 准入→service Mod→driver→Lua/分页/维护 | 无 tag/空 tag 2 失败；有效 tag 提交/去重/分页/Remove/Reset 1 控制通过 | RR-29 P2，0/1 修复 |
| Rank 在途 Add 与 Remove/Reset、重放证明 | 2 条真实 Redis 屏障交错通过，删除后重新计算为 5 | 已验证此交错；不推定所有维护交错完整 |
| Match terminal 状态、Requests、年龄清理与 JSON 大小 | Memory/Redis 两个 64 次取消、一年后 Sweep/终态重放控制通过 | 复核既有容量观察，不新增同类 RR |

合计 **15 个叶子：9 PASS、6 预期 FAIL、0 SKIP、0 build-fail，race 无报告**；6 FAIL 对应三个独立缺陷。复现用 `.go.txt` 加 Go overlay，不在正式包遗留测试，不改源码/go.mod/生成物。初稿 Match 传 Sweep limit=256（合法上限 200）造成两个额外夹具失败，已改用 MaxPageSize 后重跑；不把初稿 8 FAIL 当产品证据，最终材料记录 6 FAIL。

本轮另运行无 overlay 的：

```text
go test -race -count=1 -p=1 -timeout=180s -tags integration -json ./kit/service/platform ./kit/service/rank ./service/match ./versionstore
```

实际 **4 包 / 268 测试及子测试 PASS，0 fail/skip/build-fail**。原测试全绿与新边界失败同时存在，说明既有门禁未覆盖这些竞争/配置。没有生产修改，未重跑全仓编译/12 生成检查；RPC/生成 API 与模板未变化，复用上轮记录。本轮 service Mod.Init/Provide 与生产 Redis driver 都实际执行；没有执行完整 app 生命周期/RPC 网络、真实支付渠道或资产发放。

## 环境与图谱

Go 1.27.0 windows/amd64，Redis 8.8.0；独享单机 127.0.0.1:16409，Cluster 三 master 16406/16407/16408（无 replica）。测试前已观察 cluster_state=ok、16384 slots、cluster_size=3。配置无持久化、仅回环监听；结束验证后按 CONFIG GET dir 确认归属并关闭本轮四实例，不影响共享 MCP/索引。

使用 codebase-memory Tier 2 Verify，list_projects 与三仓 index_status 核对项目/root/git HEAD。旧独立 kit/codegen MCP 连接报 Transport closed，主 MCP 仍能读取三个独立项目，没有把聚合 D-whb_s 当正确基线。Core 代际 **08:12:07Z full/recording complete**；相关 search 全部处理至 has_more=false（driver 文件两页 30+19）。RetirePending 双向 trace 连接 retryPendingOnce/run 与 IndexRemove/IndexRemoveIn，关键 snippet 对上当前源码。

[coverage 汇总](evidence/service-review-20260929-07/COVERAGE.json) 合并 34 个证据/复用路径及有界 scopes，无记录 gap，路径均 metadata_changed；关键结论读取实际当前函数源码补证。34 是工具检查路径数，包含依赖/测试/复用证据，不是本轮逐行审查生产文件数。图谱同名 receiver 方法存在合并/错连限制（Rank Init trace 含 server 关系），以 Mod 源码、实际 Provide/调用为准，不据 inbound=0 作无调用结论。新证据文件是非生产文档，直接读写/哈希校验。

## 学习与后续

这次失败集中在正常 CAS 之外的维护及启动契约：订单索引在正常写入时原子，不意味着无条件清理也安全；Lua 在单机执行正确，不意味着 Cluster 的 key 放置可用。实施方案优先使用已有 versionstore、Redis Lua、kit/mods；不添加新的服务/队列/存储系统。

阶段主链仍为 10/10 域有界整理完成，100 路径分母沿上轮；本轮三新缺陷 **0/3 修复**，历史 4/4 仍有效。新增真实 Cluster 同槽证据，仅收窄原“Cluster 未验证”边界，**未收敛 HA、failover、跨进程强杀、真实 allocator/支付及长期容量**。K1/K2/K3 状态不因此改变。

下一入口：优先验收 RR-28 条件退休与 RR-29/30 hash tag 准入（用户请求 bugfix 时）；继续纯 review 则进入 Match 聚合容量/归档契约和购买 ledger/HGetAll 履约边界。不得把历史年龄观察重发成新 bug，也不把三个 master 当复制容灾测试。
