# Service 专项收口：Cluster 批量读取、历史状态与履约

2026-09-29；基线 bcebb805 加第七批修复，源内容摘要见[证据](evidence/service-review-20260929-09/SOURCE.json)。[运行记录](REVIEW-2026-09-29-services-09.md)、[新缺陷](../bug/REVIEW-2026-09-29-services-09.md)。以下区分当前实现、实际执行和待实施设计。

## Cluster 约束由命令决定

Activity 的 dispatch 值与 owed 索引、Platform 的 order/pending、Rank 的排序/owner proof 均有多 key 原子脚本，需要共同有效 hash tag；当前三个 Mod 已调用共享验证。单 key versionstore CAS 本身不要求所有业务 key 同槽。Mail 单写成立，但 List 的信封 MGET 是另外一条多 key 命令，已由 RR-33 证实普通 prefix 的两封页失败。

现有 IPipeline.Get/Exec/FutureBytes 可用于有界批量单键读取。本轮实际 Cluster 候选保留输入位置、二进制/空/缺失，并报告 WRONGTYPE；不把“有接口”当可运行证明。Mail EnvelopeStore 应报告坏信封而非隐藏，方案放在 GetMany 较容易保持这一业务语义；改通用 MGet 时需另核对原始 MGET 对非 string key 的语义。一次 Exec 不等于一次全 Cluster 网络往返；节点内批处理与无跨节点原子快照是实际限制。

## Match 的活跃上限不等于历史上限

queueState 包含 Waiting、Tickets、Matches、SubjectTickets、Requests。Cancel 清 Waiting/SubjectTickets，却保留终态 ticket/request；每次 mutation clone 所有映射，Redis Get 解码和 CAS 编码整个 JSON。Enqueue 还读 QueueLength。因此一个已空队列的成本也随历史增长。

本轮用实际 queueState 形状分别在 Memory、真实单机 Redis 注入 64/256/1024/4096 条已取消记录；先验证旧 request 恢复同一终态 ticket，再各执行16次真实 Enqueue/Cancel、核验 Waiting=0 和 Tickets/Requests=history+16。没有将 direct seed 的时间算作性能，没有制造匹配或声称测过 Matches 历史。

| 初始终态数 | 最终 JSON 字节 | Redis 16 次循环总耗时 | Redis 进程内累计分配 |
| --- | ---: | ---: | ---: |
| 64 | 23,223 | 43.222 ms | 8,634,840 B |
| 256 | 79,669 | 117.472 ms | 36,837,024 B |
| 1024 | 306,395 | 577.303 ms | 158,588,160 B |
| 4096 | 1,231,067 | 1,468.277 ms | 670,435,432 B |

[完整两后端结果](evidence/service-review-20260929-09/RESULTS.json)保存 elapsed_us/allocated_bytes。串行、无 race/profile、无索引重建或其他本轮 CPU 工作重叠；Go1.27.0 Windows amd64、Redis8.8.0 loopback、16循环、一次短取样。分配是 Go 进程 TotalAlloc 增量，不是 Redis 内存；总时间包含 Enqueue 内部额外读，不是单次 latency/p99 或生产 TPS。尚无此专项 SLO，不把小样本外推成长稳故障或优化收益。

待实施：先定义 ticket/request 的权威回放/归档契约，区分“等待 TTL”与“历史可遗忘时刻”。可沿已有 versionstore、幂等请求 ID 和显式恢复索引将不可变终态从热聚合转出；完成结果和幂等映射必须一起有可靠恢复证明，不能简单定时删除 Requests。改变永远可重放的当前契约、拆跨 key 提交和旧数据迁移需独立设计/回归。KeyAffinityPicker 仅减少多 replica 写竞争，不能降低单个历史对象解码/复制成本。

## 购买的三个完成事实

当前：Platform 的 delivered 表示 durable outbox 接受责任；grant 字段保存已解析商品。Game PurchaseDrain HGetAll 后调用 strict GrantPurchase，ClaimPurchase 与 AddItem 进入同一事务；成功后 HDel。事务成功/删字段回复丢失时，永久 ledger 阻止重复入账。第七批修复使“字段仍存在”的首内容不再被新 catalog 覆盖，故障回归证明了这一点。

不存在的能力：权威 fulfilled 回传、producer 的 fulfilled 拒写门禁、ledger 安全压缩、按批 drain。HGetAll 与 ClaimPurchase map 仍可增长。同步 template 不覆盖已生成的业务文件；升级自有 producer 是部署者需要完成的实际动作。

归档实施方案（**尚未实现**）：

1. strict Nest 成功提交资产与稳定 OrderID/实际 goods receipt，沿现有 durable effects/WAL 发出可重放 fulfilled 通知。通知携带 owner/incarnation 和不可复用资产证明，不接受玩家自行声明成功。
2. Platform 通过已有 versionstore/CAS 存入不可逆履约证明；outbox producer 的写脚本必须在同一个原子条件里检查证明并拒绝再次写入，不能先 Get fulfilled 后晚写 grant。对接既有 pending/Admin 对账，不另造调度器。
3. 消费者收到已验证持久确认后，才讨论热 ledger 压缩；必须仍有能够拒绝任意旧重放的权威 tombstone/归档查询。只删除单条 ledger 不是压缩协议。混合旧 writer、在途旧请求和回退策略纳入门禁，否则迟到的旧 producer 可以再次写字段。
4. drain 分页复用现有有界 sorted index/versionstore 能力，grant 与待办索引在同一脚本写入，删除/缺失清理参考 IndexRemoveIfAbsent；不要给 HGetAll 再套一个循环并宣称读取有界。改变 key/hash-tag 布局先设计兼容和迁移。

故障验收至少覆盖 asset commit/通知丢失、确认写后丢回复、producer 迟到与混合旧版本、重复 drain/HDel 失败、重建 owner、商品改版和资产回执冲突。真实渠道/资产系统未接入，所以这里只交接设计，不计为完成实现。

## 外部资源与完成口径

Session 的 Release→markReleased 窗口、并发 Finish 都可能重复回调；第三/五轮已有确定性交错。demo 只记录日志，不能证明正式资源 exactly-once。以 run/resource/incarnation 为身份在资源端持久并发幂等，防迟到释放新资源；注释的过强承诺作为已有观察保留。

本阶段已完成10域主链整理以及本轮 Activity/购买修复、Mail batch、Match history、生成装配检查；源码归类100路径不是逐行/分支正确率。HA、真实支付/资产/allocator、跨进程强杀、历史迁移、长稳容量是具名外部验收条件，仍未执行，不能被包测试绿或图谱 coverage 状态替代。
