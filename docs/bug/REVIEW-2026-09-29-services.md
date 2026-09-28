# Core service 新问题与实施交接（2026-09-29）

审查基线：`6b73289cefd4e300a75f2550d4d68646172294ad`。9 月 28 日的初始基线 `b8a401ab` 到此提交没有修改 `service/`、`kit/service/`。本轮只登记问题，不修生产代码，不宣布旧问题关闭。执行环境、全部反例源码与复跑方法见 [复现说明](REPRO-2026-09-29-services.md)，设计与性能判断见 [运行报告](../review/REVIEW-2026-09-29-services.md) 与 [机制文档](../review/IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md)。

| 编号 | 优先级 | 问题 | 证据 |
| --- | --- | --- | --- |
| RR-20260929-01 | P1 | platform 迟到发货完成覆盖人工结算终态 | 受控并发反例，race |
| RR-20260929-02 | P1 | activity 尚未确认的进度请求被 FIFO 淘汰后重复计入 | 单次故障注入，race |
| RR-20260929-03 | P2 | account 跨服务器建角失败回滚释放已有角色名 | 真实 Directory API + store 故障注入，race |
| RR-20260929-04 | P2 | mail 过期不可见邮件仍占满收件箱 | 可控时钟，race |
| RR-20260929-05 | P2 | rank 含逗号的 requestID 无法去重 | 真实 Redis，race |
| RR-20260929-06 | P2 | rank 不改分数的并发更新丢失请求去重记录 | 真实 Redis + Lua 前暂停，race |
| RR-20260929-07 | P2 | chat 自定义共享频道键可碰撞私聊键 | 合法配置 + 按频道授权策略，race |
| RR-20260929-08 | P2 | activity 最后一次尝试的 ACK 窗口被立即重试关闭 | 不推进时钟，race |
| RR-20260929-09 | P2 | session 默认清理循环缺少可配置的 owner 来源 | 源码确认的接线缺口；未执行定时器反例 |
| RR-20260929-10 | P3 | rank Around 接受的最大半径随后被 Page 拒绝 | 真实 Redis，race |

上述优先级是本轮实施排序。P1 的实际业务影响依赖支付/奖励接入，反例未操作真实资金；不将内存状态机测试称为多进程故障认证。

## RR-20260929-01

**P1：迟到发货成功覆盖人工结算终态。未修复。**

位置：[platform/service.go](../../kit/service/platform/service.go) 466–488、513、536–553；[admin.go](../../kit/service/platform/admin.go) 156–190。

`AttemptDelivery` 以 `NextAttemptAtUnix` 表示退避/占用时间，随后在存储更新之外调用 Deliverer。时间到期并不证明调用已经结束。下一次调用可把仍在途的最后一次尝试标成 `DeliveryExhausted`，`SettleOutOfBand` 随即允许人工结算；原调用成功返回时仅检查“是否已 Delivered”，会覆盖 `DeliverySettled`。

复现：预算 1，首次 Deliverer 阻塞；推进时钟 6 秒，第二次尝试将订单置为 exhausted；人工结算备注 `refunded`；释放首次调用并返回成功。结果 `state=delivered note=refunded`。反例没有实际执行退款，但证明“exhausted 即无在途发货”的前提不成立；业务若据此退款，可能同时发货。

建议：复用 `versionstore` 保存尝试代际、在途/结果不确定状态与发货回执。成功和失败回写均比较尝试身份与允许的状态，不能覆盖人工终态。人工退款/结算必须先核实仍未确认的发货，而不是把预算耗尽当作未发货证明。仅拦截迟到状态回写不能撤销已经发生的外部发货，接收端按订单幂等及回执查询仍必需。

验收：慢成功、慢失败、耗尽后 reopen、人工结算与原请求交错、发货成功但保存回执失败，以及进程重启后核对结果。测试 `TestReviewSettledOrderSurvivesLateDelivery`。

## RR-20260929-02

**P1：进度已计入但 ledger 尚未确认的请求，被有界 ring 淘汰后再次计入。未修复。**

位置：[activity/service.go](../../kit/service/global/activity/service.go) 1031–1065；[types.go](../../kit/service/global/activity/types.go) 的 `MaxProgressWindow` / `appendBounded`。

两次独立写：先写 Participant 的分数与 requestID，再写 `ReservationApplied`。第二步失败时，只能由 Participant 的 ring 证明“已应用”。该 ring 只有 32 项，没有区分“ledger 已确认，可移除”与“尚未确认，必须保留”。

复现：只让 `lost` 的首次 ledger mark 失败一次；随后 32 个不同请求各增加 1 分；不推进时钟，仍在默认 ledger TTL 内；重试 `lost`。分数从 33 增至 34。不是超过 TTL 后重放，也不是测试永久拒绝 mark 导致的假失败。

建议：对尚未完成确认的请求保留独立且有界的证明，容量满时明确背压，不淘汰未确认项。复用 `versionstore` 的版本与索引，先定恢复状态机；如用 Redis Lua 原子写两个对象，需保证同槽并维护版本/索引契约及其他后端等价实现。单纯增加 32 或延长 TTL 只能延后触发。

验收：apply 成功/mark 失败后跨 ring 容量、重启、重复并发、ledger 到期及不同 delta 重用 requestID。测试 `TestReviewUnconfirmedProgressSurvivesRingEviction`。

## RR-20260929-03

**P2：同账号跨服务器建角失败，释放另一角色已经持有的名字。未修复。**

位置：[account/service.go](../../kit/service/account/service.go) 248、261、309、325；[directory/store.go](../../kit/service/directory/store.go) 117–133。

slot 以 account+server 区分，名字 owner 却只有 account。Directory 同 owner 的 Reserve 是幂等操作，允许返回已 committed 的原 claim。第二个服务器复用同名后，如果最后的 slot 写失败，回滚无条件 `Release(name, account)`，把第一个角色的名字释放。

复现：账号在 server 1 成功创建 Hero；server 2 用同名，注入 slot Update 失败；原角色记录仍在，`Names.Lookup("Hero")` 已不存在。是否产品允许跨服同名，不改变“失败操作删除已有所有权”的结论。

建议：名字 owner 应表达角色或唯一 slot 的身份，回滚只撤销本次真正获得的资源。保留创建意图和版本/claim token，区分幂等复用与新建。不直接修改 Directory 的同 owner 幂等契约来掩盖调用方错误；补兼容旧 owner 的迁移计划。

验收：跨服同账号、同服并发、旧 committed name、任一阶段失败/结果不确定、回滚失败与重试。测试 `TestReviewCrossServerRoleRollbackKeepsExistingName`。

## RR-20260929-04

**P2：过期且列表不可见的邮件仍占用邮箱，阻塞后续投递。未修复。**

位置：[mail/service.go](../../service/mail/service.go) 347、363、486–497；[mailbox.go](../../service/mail/mailbox.go) 的 `full` / `evict`。

List 跳过过期/缺失 envelope；邮箱 Entry 的未读状态和容量没有同步回收。evict 只允许 deleted/claimed，满箱拒绝发生在回收之前。常规玩家列表拿不到过期邮件的 ID；运维通过原始 Mailbox/已知 ID 手动删除仍可解堵，不能称为任何情况下都永久无法恢复。

复现：投递 200 封 1 秒有效期的未读邮件，推进 2 秒；列表可见数量 0、Unread 200；投递新邮件返回 `ErrMailboxFull`。本次使用内存 envelope，证明无需真实 Redis TTL 也会发生。

建议：在投递 Entry 中记录可判定的有效期，复用 mailbox 的 CAS 实施有界回收，同时更正 Unread；对旧 Entry 缺少 expiry、缺失 envelope 与正在领取的 token 定义安全策略。不能为了腾空间删除仍有效的领取去重证明。满箱准入前先完成安全回收。

验收：未读/已读到期、缺失 envelope、满箱、领取在途、旧格式与重复投递。测试 `TestReviewExpiredMailboxMakesRoom`。

## RR-20260929-05

**P2：逗号 requestID 破坏 rank 去重编码。未修复。**

位置：[rank/redis_store.go](../../kit/service/rank/redis_store.go) 394–425；[store.go](../../kit/service/rank/store.go) `validateSubmit`。

API 不拒绝逗号，ring 以逗号拼接与拆分。相同 `order,one` 连续执行 `UpdateAdd(10)` 得到 10、20，而非 10、10。片段 requestID 还可能被错误识别成已经执行。

建议：使用可逆编码（如 JSON 字符串数组/长度前缀），明确字节上限与旧数据迁移；若暂时禁止分隔符，要在写入前拒绝并明确兼容限制。测试 `TestReviewCommaRequestIsIdempotent`；补空白、非 ASCII、编码迁移及片段碰撞。

## RR-20260929-06

**P2：member 未变时 CAS 无法保护去重 ring。未修复。**

位置：[rank/redis_store.go](../../kit/service/rank/redis_store.go) Lua 101–112、Submit 159–169。

CAS 比较的是 member 编码，写的是 member 与 applied ring。`UpdateMax` 不增分时 member 不变；两个调用基于同一旧 ring 都能通过 CAS，并相互覆盖新 requestID。另外 no-change 分支忽略 swap 的 applied 布尔值。

真实 Redis 受控交错：初始 100；A `Max(90,"a")` 读完后停在 Lua 前；B `Max(80,"b")` 成功；恢复 A 覆盖 ring；随后无 key `Set(50)`；重放 B 得到 80，应该保持 50。本次未填满 ring，故不是约定去重窗口自然淘汰。

建议：把去重数据纳入 CAS 身份（单独递增 revision 或比较完整状态），no-op 也必须原子确认 requestID；CAS 失败按已有 versionstore 退避规则重新读，不返回伪成功。保证 Remove/Reset 的代际与 revision 初始化契约。测试 `TestReviewUnchangedMemberCASProtectsRequestRing`，再补 no-op 与真实增分/删除交错。

## RR-20260929-07

**P2：自定义共享频道可与私聊键碰撞，越过频道授权边界。未修复。**

位置：[chat/chat.go](../../kit/service/chat/chat.go) 281、403–415；[store.go](../../kit/service/chat/store.go) 504–530。

共享 key=`kind:target`，私聊 key=`kind:low:high`，kind 允许冒号。合法共享配置 Kind `private:1`、Target 2，与私聊用户 1/2 的 key 同为 `private:1:2`。读取策略先授权 query 所声明的共享频道，再读取碰撞的存储。

复现策略拒绝 viewer 3 读取 private，但允许其读取上述共享频道；共享 History 实际返回用户 1 的私聊 `secret`。触发依赖自定义 Kind，默认规则本轮未证明有此问题；不是测试故意允许 viewer 3 读私聊。

建议：scope 与各字段采用无歧义编码，参考已有 match Queue.Key 的字段转义；或在注册配置时拒绝分隔符并说明规则。若改变 key，必须处理历史频道迁移和重复消息标识。测试 `TestReviewCustomSharedChannelCannotReadPrivatePair`；补 shared/pair、多自定义 kind 的编码唯一性。

## RR-20260929-08

**P2：最后一次 dispatch 的 ACK 机会被未到期重试提前关闭。未修复。**

位置：[activity/service.go](../../kit/service/global/activity/service.go) 1316–1335、`AckDispatch`。

AttemptDispatch 先判断 `Attempts >= MaxAttempts`，后判断 Due；最后一次有效尝试刚返回，立即重复调用就持久化 exhausted，原 token 的 ACK 也被拒绝。

复现预算 1，首次尝试成功，未推进时钟，再次调用及 ACK 分别返回 `ErrDispatchExhausted`。正常按 Due 枚举的 runner 不一定触发，但公开 API 的回复丢失重试/重复请求可触发。

建议：先保留最后一次尝试的完整退避/ACK 窗口，再在到期且未确认时耗尽；保持预算和人工 reopen 的既有契约，不无限重试。测试 `TestReviewLastDispatchRetryKeepsAckWindow`，补到期边界、最后 ACK 与耗尽并发、重开后的旧 token。

## RR-20260929-09

**P2 接线缺口：session 默认 sweep 无 owner 来源，无法通过现有公开配置启用。未实施。**

位置：[session/server_run.go](../../kit/service/session/server_run.go) 58–61、83；[session_mod.go](../../kit/service/session/session_mod.go)；[session_rpc_assembly_gen.go](../../kit/service/session/session_rpc_assembly_gen.go) Server 字段；[service/session/service.go](../../service/session/service.go) Config / Sweep。

30 秒循环每次调用 `sweepOwners()`，该私有方法固定返回 nil。注释要求部署提供 roster，但 Config/Mod/Server 没有可注入来源，外部包不能覆盖该方法。默认独立 session Server 的这个循环不会主动释放过期 run 资源。

这是源码确认的自动接线缺口，**没有运行 30 秒定时器反例，不将其包装成第十个动态复现**。业务可自行调用 `Service.Sweep`，也可由后续 Enter 懒清理；若明确接受这种接入模式，需修正文档/去掉虚假的接线承诺，而不是宣称资源永远无法释放。

建议：参照已有 platform PendingSource，注入有界 owner 来源及游标，或由业务 Mod 明确承担 Sweep 并提供健康指标。默认不能扫描整个 store；来源失败、分页推进、停机取消都应有契约。手写扩展不要改生成文件后被下次生成覆盖。

验收：公开装配配置可实际让过期 owner 出现在 Sweep，Releaser 按 token 幂等；长期不再登录、批次公平性、来源错误与重启恢复。

## RR-20260929-10

**P3：Around 最大半径边界矛盾。未修复。**

位置：[rank/redis_store.go](../../kit/service/rank/redis_store.go) 303–317；[store.go](../../kit/service/rank/store.go) `validateRange`。

Around 接受 radius=100（MaxPageSize/2），随后调用 Page(limit=201)，后者上限 200，返回 `ErrRangeInvalid`。真实 Redis 单成员榜即可复现。

建议：将允许半径改为 `(MaxPageSize-1)/2`（99）并更新公开说明，或定义保留中心的截断规则；不要简单把所有页上限放大。测试 `TestReviewAroundAcceptedMaximumRadius`；补 99/100、榜首/榜尾和成员不存在。
