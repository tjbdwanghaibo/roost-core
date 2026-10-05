# Outbox 领取与 Activity opening 恢复：候选、持久意图和执行权

2026-10-05，源码基线 `12726715` 加本轮修复。[本轮](REVIEW-2026-10-05-noncore-27.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-17/README.md)。

## 现有主链

Saga coordinator 经 Apply 将状态和 outbox 原子持久化；publisherLoop 的 ClaimOutbox 先扫描到期候选，再原子取得 lease。发布成功 Ack 删除，失败 Nack 记录下一期限、释放租约并增加 attempt；旧 lease token 不能删除或重新延后新持有者的消息。扫描和领取分开，扫描返回的值不能代表之后仍满足执行条件。NC-41 将 due 和 lease 一起放到原子条件，不改变业务幂等/未知发布结果的责任。

Activity Open 先校验本次请求，再通过窗口 CAS 预留容量和持久 Intent，然后 Activities.Create，最后 confirmWindow 将 Opening 移入 Keys。原 Intent 在调用崩溃/丢回复后仍负责恢复，后来的参数不能替换它。NC-42 校验的是实际将被执行的持久对象，而不仅是新输入；拒绝后窗口和活动无副作用，合法计划/legacy 缺计划仍按原路径恢复。

AdvanceExpired 有界读取窗口：正常 collecting 到期完成并写 dispatch；有持久计划的 opening 过 grace 帮助 Create，不以期限证明旧 Create 未提交；legacy 无计划在旧 writer 排空的升级前提下回收。畸形 opening 保留名额、记指标和状态变化日志，其他活动继续。RR-09 残余增加非法 key/所属 group 的写前边界；诊断以所属窗口为准，不能相信坏 key 的 group 来判断应清理哪一条。

owner-only ReconcileProgress 从 participant 的 pending 证明补 ledger mark，再仅释放本次确认的 ID；另一个请求在读后加入的新证明必须保留。本轮通过共享正式 stores 的两个 Service 控制交错验证旧/新两次对账收敛，未修改 admin。请求 ID 超过 TTL 后复用、旧操作与新 reservation 的窄交错仍需单独建立身份/期限矩阵，不能由这一控制称全部幂等完成。

## 继续 review 的结论与代价

account 新增 nameCommittedElsewhere 只将 foreign committed 判作已死计划；foreign reserved 会过期，不能当永久失败。DeleteIf 同时使用读取版本/计划身份/未发布条件；Admin 先记账号备注再释放，因此 note 存在不等于释放成功。相关既有公开回归本轮重跑，未知/跨进程的所有组合未全验。

chat 新增变化只修正无游标/保留窗口内倒页的容量淘汰 Gap；AfterSeq 跨缺失序号、页内洞、尾部洞仍上报。history.gap 是实际缺口计数，不是历史曾经淘汰过的频道计数。global 已移除 liveness lease，只剩 route epoch/CAS；运行 liveness 由 App singleton 提供，不能将删掉的租约 API 再按旧报告补回来。Mail 同意图比较改为逐字段、nil/empty 切片等价，当前 diff 已读，完整新消费未本轮独立重做。

改动保留原包、Store/lease/CAS/指标能力，未增加 I/O 次数或 goroutine。缓存从按 Key 改为按所属窗口分组，只保存当前诊断并在对应窗口条目恢复/消失时清理；不是全局无界生命周期已证明。没有性能对照，不能声称吞吐改善或全部容量风险关闭。

## 为什么此前漏检

原 outbox 单测的 MemoryStore 在同一锁里扫描和领取，不存在 Mongo 的 Find→FindOneAndUpdate 窗口；已测“旧 token 拒绝”不等于已测“新领取遵守刚更新的期限”。原 malformed 检查只测 Intent.key 不同，没有测 entry 与 Intent 一致但共同非法/跨组；public Open 复用计划是另一个入口，sweep 跳过不代表 Open 同样拒绝。

以后先列候选发现与原子执行两个时点；分别验证“原输入合法但持久对象坏”“持久对象与索引一起异键”“拒绝后无副作用、合法恢复”。源码整理、行为测试、外部验证分开登记，测试数量不当覆盖率。

## 反复缺陷的方向判断

Saga 修复链：NC-37/38（`47fca740`，消费者健康/持久恢复代际）→ NC-39/40（`12726715`，启动身份/完成路由）→ 本轮 NC-41（Mongo outbox 原子领取的 due 条件）。这几轮不是同一个条件反复改坏，但都落在进程内状态、持久身份和正式适配器交界；MemoryStore 锁内实现与 Mongo 两阶段实现的差异造成新的漏检。当前证据支持保留现有 CAS、receipt、outbox、lease 方向，尚不足以证明需要替换 Saga 架构。建议下一轮先整理启动、执行、完成、发布、恢复的身份/期限契约矩阵，让正式适配器跑同一组跨协调器交错；成本是补正式场景与环境验证，不是继续增加缓存、重试分支或新状态。跨进程未知提交仍未验证，不能提前判断全部只剩实现细节。

Activity 修复链：RR-20261001-09（`e84194bc`，legacy opening 与 malformed Intent sweep）→ 本轮该编号残余（entry/Intent 共同非法或跨组、诊断归属）及 NC-42（公开 Open 复用坏持久计划）。原修复未覆盖 Open 入口，且验证只检查双方相等，未检查它们是否共同违反窗口所有权。这是恢复入口契约分散、同一不变量第二次遗漏的信号，需修正验证组织方式：Open 和 sweep 共用持久计划校验，窗口所属组独立约束存储键，诊断也依所属窗口管理；本轮已用现有 helper 做最小落实。代价是坏旧数据转为明确拒绝并需人工修可信原计划，未增加自动清理、超时推断或对业务数据的自动迁移。后续 request ID/receipt TTL 另列身份矩阵，避免把新的时间假设继续补到现有恢复分支。

结论是优先统一恢复契约和正式消费场景，保留已有架构；不能以本轮绿灯关闭 Saga/Activity 全生命周期，也不能把坏持久记录归因为当前正常 writer。
