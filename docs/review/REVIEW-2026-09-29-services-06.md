# Service 第六轮：第五批修复后的邻接审查（2026-09-29）

源码 `4b0837d70b6d62d84b7b3ebf9ab5e5b2b6a819f0`。基线从 cec1dd30 fetch/pull 后无远端 main 新源码，先完成[第五批 bugfix](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)，再继续 service review。**上轮四项原触发与已列邻接场景收敛，本轮列明范围没有新增确认缺陷。**不能据此宣布所有 service 在任何部署/历史数据/外部系统场景下都已无 bug。

[当前完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [实际实现学习](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md#第五批修复后的实际实现) · [逐路径范围](evidence/service-review-20260929-06/inventory.csv) · [可复跑证据](evidence/service-review-20260929-06/README.md)。

## 范围如何收口

仍为 Core 单仓 service/ 与 kit/service/ 的 100 个 tracked 生产 Go 路径，不把另一个 agent 的模块算入结论。本轮 6 个生产路径变更：Match Grouping、Rank redis_store、Chat store/types、Activity service/types；另外 94 个路径逐一比较 Git blob 未变化，复用[第五轮 10 域证据](REVIEW-2026-09-29-services-05.md)，没有假称这次逐行重读所有文件。四个新正式回归文件和一个旧测试契约更新单列，不计入生产路径分母。

本轮逐条检查：公开分组参数进入切片前的校验；Rank 纯计算→Lua CAS→请求 ring 的拒绝顺序及重读；Chat 年龄清理→Requests→Seq/History/正反游标/Gap/计数；Activity 准入意图→Create→确认→Notify→grace complete→dispatch/Delivering→旧大窗口扫描及重建。构造/Mod 保持原组装，Activity 默认 OpeningGrace 的语义改变但默认值未改，正式配置仍需 sweep_groups。

| 邻接场景 | 实际结论 |
| --- | --- |
| Grouping 空候选/非法大小/空模式；合法 2/64 | 先返回 ErrQueueInvalid，正常成组/未足数控制通过；不把策略放进存储 CAS |
| Rank 同请求拒绝后再合法提交、两个并发 +1 接近 MaxInt64 | 分数/Tie/Brief/rank 未被拒绝改写，幂等键未占用；CAS 竞争后只一个成功，不回绕 |
| Chat fresh prefix、expired interior/tail、limit=1、正反/空页 | 所有过期目标可清理，页内/边界/末尾洞显式 Gap；未跨洞的分页不误报；删除正文仍拒绝保留证明内重发 |
| Activity 晚 Create 与名额、准入/Create/confirm 写后丢回复 | 名额不按超时撤销，sweep 完成同计划；服务对象重建保持恢复路径和原 expected 集合；迟到 opener 可收到 insert-only 的 ErrExists |
| Activity 旧 257 窗口与旧无 Intent Opening | 读取至多 256 个不同 key，持久游标跨重建访问尾部；legacy 无计划不自动丢弃，以同 key/原 expected 集合 Open 补计划 |
| 生成购买/匹配消费与组装 | 正式生成消费者编译/已有 handler/game 测试通过；稳定 OrderID/token/Match.ID 责任仍明确，未连接生产资金或 allocator |

新增两个 Create/确认“写后丢回复”场景是在完整套件后补充的邻接审查，维护在正式 Activity 包；重新执行两模式全部 TestBugfix5，而非只读包测试绿灯。没有新的生产修改或额外 RR。

## 测试与证据边界

- 修前原 overlay：9 leaf pass / 12 leaf fail，保留历史记录；原 Activity 反例假定超时名额会被回收，本批安全契约改为拒绝第 257 次准入。正式回归用同慢调用交错验证“不超容量且原聚合可完成”，不通过弱化断言掩盖旧问题。
- 16 个测试包/830 测试及子测试 pass，无测试级 skip/fail/build-fail；随后补充两叶子/一父测试事件，无生产代码改动。
- 最终定向：Memory、Redis 两模式各 27 leaf / 32 test events 全过，共 54 次叶子执行。Rank 两模式均为真实 Redis；Activity/Chat 按模式切换；算法验证后端无关。
- 全仓编译、四包 vet、Chat/Activity 的自身 RPC -check 通过；其余生成接口/18 生成文件的 blob 未变化，沿用前轮门禁，不写成此轮重跑 12 次。
- 既有正式 CLI 生成工程 replace 指向本工作树，Platform/purchase 编译、handler/game 已有测试通过；只是依赖本地工程，不宣称重新通过完整联网安装、真实 NATS/Mongo 或外部支付。

计数、源文件 hash、失败输出与日志 hash 在[修复证据](../bugfix/evidence/service-bugfix-20260929-05/RESULTS.json)，不是行/分支覆盖率。对象重建不是强杀进程，多次本机 Redis 操作不是 Redis HA。

## 图谱

使用 codebase-memory Verify，roost-core 项目/root 经 list_projects/index_status 核对；search/双向 trace/snippet 定位旧函数，初代 05:34:58Z 路径过期/新测试 missing，直接读当前源码。同步修复到主仓后请求 full index_repository；调用长时间未返回完整结果，结束本次等待，没有停止共享 MCP 进程。随后 coverage 确认发布 **2026-09-29T08:12:07Z 全量代际，recording complete**，新 pendingScanBatch 可被 search_graph 定位，新测试路径已纳入。

[最终 coverage](evidence/service-review-20260929-06/COVERAGE.json)包含 11 个改动/正式测试路径和两个 service scope，无记录 gap，**仍均 metadata_changed**。因此保留当前源码补证，不声称图谱已证明方法解析完整/所有节点内容新鲜，也不把取消等待等同索引失败或成功无异常。

## “完全收敛”的实际结论

- **本轮确认缺陷：4/4 原触发与声明场景已修复、验证，修后邻接审查无新增确认缺陷。**
- **Service 主链/存储/组装契约：10/10 域完成本阶段有界审查，100/100 当前路径已核算。**
- **上线级外部系统/历史数据/长稳性能：未完全收敛，不能报告全局 100%。**

剩余是具名专项：旧无计划 Opening 与已被遗忘的 orphan/证明对账；Match 终态历史保留及大 queueState 成本；购买长期 ledger 与 HGetAll 的归档/履约回执；真实 allocator 的持久幂等/fence、渠道对账、HA/断网/跨进程强杀与容量负载。此前接受的设计边界与 CARRYOVER 不擅自转成“已验证无风险”。

兼容重点：Activity/Chat owner 协调升级，旧 Activity owner 会丢新意图或重做不安全回收；旧 Chat 读取不能识别新洞。此轮没有执行生产升级或迁移。后续可以转入这些专项或其他核心域，避免再从 service 主链第一轮重复扫描。
