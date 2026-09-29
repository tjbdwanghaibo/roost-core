# Service 第五轮问题与实施交接（2026-09-29）

**最新实施状态**：RR-25/26/27 和旧 RR-20260914-02 的本轮残余 **4/4 已修复并通过正式回归**；见[第五批 bugfix 的决策、升级与证据](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)。Activity 采用持久创建计划与保留名额，Chat 同步处理页内/尾部 Gap；下方“未修”保留原审查时点，不表示当前状态。旧数据与真实外部系统边界未自动关闭。

源码基线 `b336ce62f75ce0d763138bc8523c4695724214a2`。先完成[第四批 bugfix](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md)，再审新范围。本轮 **3 个新问题（2 P2、1 P3）及旧 RR-20260914-02 的 P2 残余**均未实施。结果见[可执行证据](../review/evidence/service-review-20260929-05/README.md)；[完成矩阵](../review/SERVICE-REVIEW-COMPLETION-2026-09-29.md)与[机制学习](../review/IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md)分开记录。

## RR-20260929-25

**P2 · Grouping 公开入口没有校验 Queue，非法组大小引发 panic 或伪成功。状态：未修。**

位置：[grouping.go](../../service/match/grouping.go)，FirstComeGrouping.Group 第 30 行、ScoreWindowGrouping.Group 第 55 行；队列约束见 [types.go](../../service/match/types.go) 第 160 行 Queue.Validate，合法 GroupSize 为 2..64。

调用两个策略的 Group，提供两张候选票，GroupSize 分别为 -1、0、1、65。负数进入切片边界并 panic；0/1 可报告空组/单人组已形成；65 返回普通“未成组”而非 ErrQueueInvalid。正常大小为 2 的控制通过。8 个非法输入子测试均失败，测试用 recover 捕获 panic 后明确失败，没有吞掉异常。

影响是独立公开策略的配置错误无法安全返回；Store 的 Candidates/Commit 验证发生在另一入口，不能保护单独调用 Group。正式 demo 正常流程先调用 Candidates，合法配置控制通过，**没有声称有效队列会自行产生 panic**。该反例与后端无关。

实施方向：两个 Group 一进入就复用 Queue.Validate，先拒绝非法模式/组大小再执行长度判断和取 anchor。保留策略在存储 CAS 外执行的结构，复用已有 ErrQueueInvalid。验收覆盖空候选、负数/0/1/65、空模式、正常 2 与边界 64，以及 FIFO/score 两套策略；不向 Store 塞入业务配对算法。

## RR-20260929-26

**P2 · Rank UpdateAdd 的 int64 加法溢出后成功落库。状态：未修。**

位置：[redis_store.go](../../kit/service/rank/redis_store.go) 第 383..414 行 nextScore，关键第 407 行 `current.Value + incoming.Value`。公开 Submit 接受 int64，UpdateAdd 没有溢出检查。

真实 Redis 设置初始值 MaxInt64，再加 1；Submit 返回 nil 错误，Rank 读回 MinInt64。MinInt64 加 -1 则读回 MaxInt64。会把极高分变最低分，或极低分变最高分，且错误结果已写入持久榜。MaxInt64-1 加 1、MinInt64+1 加 -1 的合法边界控制均通过。两种复跑模式中 Rank 都使用真实 Redis，不能写成 Memory/Redis 两种 rank 实现的验证。

实施方向：在 nextScore 的 UpdateAdd 分支、Lua CAS 前检查正/负溢出，复用 ErrScoreInvalid，保持原 score、Tie、Brief、revision 与 request ring。不能静默 clamp，除非另行改变业务契约。验收须追加“拒绝的 RequestID 未被占用”：同 RequestID 改用合法 delta 能成功；同一 owner 并发和 CAS 重试也不能绕过检查。活动 RR-20260929-15 修的是另一套计分链，不能由它推定 Rank 已处理。

## RR-20260929-27

**P3 · Chat 按时间过期，但 Prune 只清理序列前缀，时钟偏移时漏掉已经过期的后续消息。状态：未修。**

位置：[store.go](../../kit/service/chat/store.go) 第 619 行 Prune，关键第 642 行的 Ring[0] 循环。Ring 以 Seq 排序；StoredAtUnix 是年龄依据，不保证同样单调。

步骤：在 T 发布 seq=1；时钟回退 2 秒，在 T-2 发布 seq=2；再推进 RetentionAge+1 秒。cutoff=T-1：seq=1 尚未过期，seq=2 已过期。Prune(limit=10) 返回 0、nil，因第一条尚未过期而根本不检查第二条。Memory 与真实 Redis 均成立；有限去重窗口控制通过。

影响是消息超过配置年龄仍被保留，至少延迟到前缀过期；不是无限内存增长证明，数量仍有 MaxRetain 上界。Seq 顺序本身正确，不建议按时间重排历史。

实施需明确保留契约：若允许序列中间删除，利用现有有界 Ring/CAS 扫描所有适用位置，但同步处理 Page/Scrollback 的 Gap 与游标语义；若坚持只删前缀，写入端需采用明确的单调保留时间，并说明与真实写入墙钟年龄的差别。不能只改注释声称墙钟单调。验收包含正常时钟、前后各 2 秒偏移、跨副本顺序、删除上限、游标/Gap、删除后仍拒绝保留窗口内 RequestID 重放。

## 旧 RR-20260914-02：Opening 回收后迟到确认突破窗口，并漏掉过期活动

**P2 残余，沿用[原问题](REVIEW-2026-09-14-02.md#rr-20260914-02--p2--开活动与-sweep-交错丢失窗口索引)，不重新编号。**

位置：[service.go](../../kit/service/global/activity/service.go) 第 227 行 OpenActivity、第 273 行 confirmWindow、第 699 行 AdvanceExpired（第 740 行最多扫描 256 个 Keys）。现有 opening/grace 修复保护普通 Create 间隙，但确认过程不验证是否仍持有准入名额。

1. 活动 zz-late 已进入 Opening，在实际 Activities.Create 前暂停。
2. 超过 OpeningGrace 后，AdvanceExpired 回收该名额。
3. 正常 Open 256 个其他活动填满窗口，这些活动尚未 Notify，保持 Pending。
4. 旧 Create 迟到完成；confirmWindow 无条件补入 key，Open 返回成功，pending=257/256。
5. zz-late 收到部分 Notify，再超过 GraceWindow。连续两轮 sweep 只扫描排序前 256 条，zz-late 排在末尾，仍为 collecting。

Memory 与真实 Redis 同样复现；正常未回收的 Opening 按期 complete 对照通过。没有伪造 Create 的存储结果，用通道阻塞的是合法慢调用。旧缺索引问题转化为“索引超容量且扫描不可达”，仍属于跨对象 opening 生命周期残余，不能简单宣布旧修复失效于所有场景。

实施方向：复用 versionstore CAS 和已有 opening 意图，给准入建立可验证的代次/所有权；回收后迟到 Create 必须有持久、可达的恢复路径，或在可证明安全的围栏下重获名额。**仅在 confirmWindow 检查容量然后返回错误，会留下已创建但不在扫描入口的活动；仅增大扫描上限也不能约束反复迟到确认。**验收包含这条满容量交错、回收后重试/同 key 新代、Create 写后丢回复、确认写失败、服务重建，以及超过上限的旧窗口的有界分页恢复。

## 不新增编号的观察与边界

Match 100 次 Enqueue/Cancel 后 Waiting=0、Tickets=100、Requests=100，延续[09-17 已有容量观察](REVIEW-2026-09-17.md#观察项终态历史保留使队列空了但聚合状态仍增长)，没有压测到性能故障。Session 两个 Finish 可并发进入 Releaser；测试资源端幂等后为 2 次调用/1 次效果，未连接真实 allocator，不能登记“已发生 double-free”。Account 的 Banned 目前明文约束 Login/CreateRole，不将 SelectRole 未重查封禁自动认定为新 bug。既有 CARRYOVER 的活动贡献接受项保持原结论。
