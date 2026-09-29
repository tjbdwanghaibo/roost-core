# Service 第四轮问题与实施交接（2026-09-29）

基线：单仓 Core `bdbb61bc4340c5c6261f746f882039e6cc4f3928`。新增 2 项：**RR-20260929-23（P1）、RR-20260929-24（P3）**；另补旧 RR-20260910-02 的未领取删除变体（P2），不重复编号。生产源码未修改。

[复跑材料](../review/evidence/service-review-20260929-04/README.md) · [运行记录](../review/REVIEW-2026-09-29-services-04.md) · [机制和实施建议](../review/IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md)。新反例在 race 模式执行；Redis 是真实订单/邮箱后端，外部发奖使用受控 collaborator，不是生产支付或资产系统。

## RR-20260929-23

**P1，确认、未修：Platform 将外部发货超时当成确定未发货，清除 pending 后允许退款或再次发货。**

位置：[service.go](../../kit/service/platform/service.go) 506–518、587–608，关键是 598 行无条件 `removeAttempt`；[admin.go](../../kit/service/platform/admin.go) 178 行仅依赖 PendingAttempts 是否为空；[types.go](../../kit/service/platform/types.go) 282 行 Due 只判断终态和 backoff，不判断未知结果。

触发 A：合法已支付订单 MaxAttempts=1；Deliverer 完成一次发货后返回 `context.DeadlineExceeded`（响应丢失）。`recordFailure` 清除 PendingAttempts，进入 exhausted；随后 `SettleOutOfBand("refund...")` 成功进入 settled，完全没有要求 `ResolvePendingAttempts`。期望未知外部结果仍阻止退款；实际是 `grant=1 pending=[] exhausted → settled, err=nil`。

触发 B：同样的第一次结果，MaxAttempts=2；时钟推进超过 backoff，第二次 AttemptDelivery 再调用非幂等 Deliverer 并成功。实际外部计数为 2，订单只有一次 delivered 终态。接口注释声称 Deliverer 的 OrderID 去重只是可选，因此这个实现符合当前公开接口的描述。两个触发均在 Memory 和真实 Redis 订单后端失败。

这与 [RR-20260929-01 修复](../bugfix/RR-20260929-01.md) 分开：旧修复能保护**尚未返回的**在途调用，阻止人工结算并隔离迟到回包；本项是调用已经返回**未知结果错误**后，证明被删除。旧结算围栏未失效，但其前提被另一入口清空。旧文档提过外部幂等/对账边界，本轮把未覆盖的具体反例和错误分类缺口落实到证据，不能把“需幂等”理解为现在可以安全退款。

影响：非幂等业务发奖可能重复；即使发奖按 OrderID 幂等，已发货订单仍可能被当成未发货而人工退款。不会声称复现已经操作真实资产或支付退款。生成的 `grantDeliverer` 使用 Redis HSet，写成功后丢回复同样属于未知结果；本轮阅读该模板，未渲染并运行正式购买消费链。

实施建议（未实施）：沿用现有 Orders、AttemptSequence、PendingAttempts 与 owner-only 对账接口。只有**明确证明未发货**的类型化错误才能移除 pending；超时、断连和其他不能证明结果的错误默认保留。未知态不能单靠 backoff 重新发货或开放退款；若允许幂等重试，需要明确 Deliverer 的持久 OrderID 去重契约和权威查询/收据接口。补“写前拒绝 / 写后丢回复 / 查询失败 / 崩溃重启 / 幂等发奖后仍拒绝错误退款”的回归，并更新当前 at-most-one 注释。不要靠判断错误文本或 `ctx.Err()==nil` 推断没有发货。

## RR-20260929-24

**P3，确认、未修：Platform 重复回调的错误回包共享 MemoryStore 的 PendingAttempts 切片。**

位置：[service.go](../../kit/service/platform/service.go) 377 行 `Receipt{Order: existing.Value}`、386 行错误返回；已有 [Order.clone](../../kit/service/platform/types.go) 270 行在这里未使用。[MemoryStore.Get](../../versionstore/memory_store.go) 27–33 行只复制 struct，切片底层仍共享。

触发：第一次 HandleCallback 的 Deliverer 用通道暂停；同一签名回调再次进入，AttemptDelivery 返回 ErrDeliveryHeld，HandleCallback 将既有 receipt 和错误一起返回。调用者修改 `receipt.Order.PendingAttempts[0]`，再从 store 读回，pending 从 1 变成 101，Version 仍是 2。释放第一次 Deliverer 后，正常成功回包被判为 stale delivery completion，无法正确归档发货结果。

独立测试只经公开回调取得返回值，不直接修改 store；通道固定先后顺序。Memory 的行为断言失败，Redis 的相同对照通过，race 未报告数据竞争。范围仅进程内使用 MemoryStore 的返回所有权；Redis JSON 解码已隔离，不能宣称生产 Redis 订单被该回包修改。

实施建议（未实施）：复用现有 `Order.clone()`，包括错误返回分支；检查所有 receipt 输出和 Deliverer 参数的切片所有权。保留当前 backend 契约，不为一个 service 输出问题改变通用 MemoryStore 的泛型深拷贝规则。回归要同时检查权威值、Version 和第一次发货完成，而不是只判断返回切片地址不同。

## 旧 RR-20260910-02：未预约直接删除的残余变体

**P2，确认仍可复现，关联[原问题](REVIEW-2026-09-10-02.md)，本轮不新增 RR。**

位置：[mailbox.go](../../service/mail/mailbox.go) 239–240 行只在 ClaimToken 非空时保留墓碑；182–207 行没有墓碑时视为首次投递；[Delete](../../service/mail/service.go) 635 行承诺删除未领取附件即放弃。

触发：Send 一封有效附件邮件，不 Reserve，直接 Delete；当前 Reserve 正确拒绝。再用公开 Send 投递 200 封邮件，deleted Entry 被容量淘汰；删除一条 filler 腾出位置，延迟 fanout 重投原 ID，Reserve 成功返回原附件和首次 token。Memory / 真实 Redis 均失败。

这里没有“第一次已发奖”，不能写成重复奖励已经发生；问题是被放弃的附件重新可领、删除终态被展示保留策略遗忘。已有 token 的领取墓碑保护仍保留，不能把这个变体写成旧领取去重全面回退。

实施建议（未实施）：分别记录删除终态、发奖结算和在途证明；无 token 的 deleted 也要在信封重投窗口内保留身份，重投不得恢复。复用现有邮箱 CAS 和期限字段即可，不必增加新存储层。墓碑容量不足时明确拒绝或要求业务对账，不能自动淘汰仍有效的删除证明。旧已淘汰删除记录无法无损重建，升级说明应明确这个边界。

## 观察与限制

Mail Reserve → Delete → Commit：Entry 尚在时 Commit 返回 ErrMailMissing；容量淘汰保存 token 到 SettledClaims 后，同一 Commit 却返回 claimed。两后端均有一致结果，说明“展示删除”和“外部发奖事实”仍混在状态枚举里。本轮单列观察，未把业务是否允许删除在途附件替用户定案，也未证明实际资产重复。

Session 的 `releasePending → Release → markReleased` 仍要求外部幂等；demo 模板只记录日志，不是可验证的资源分配器。Match 写后丢回包能通过 Ticket.MatchID → Match 找回结果，直接重试 Commit 返回 conflict；属于接入恢复契约，未登记 bug。真实 Broker、生产发奖、Redis HA/断网、多进程强杀、长期容量压测均未执行。
