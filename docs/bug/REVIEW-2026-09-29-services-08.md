# Service 第八轮：Activity Cluster 与购买内容重放

**后续状态：RR-31/32已在[第七批](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md)2/2修复并通过原断言/正式回归；下方“未修”是原review时点。**购买故障钩子适配为真实Eval写后丢回复，独立catalog升级保留Count10；Activity合法Cluster多game部分恢复也已验证。新Mail RR-33另见[第九轮](REVIEW-2026-09-29-services-09.md)。

2026-09-29，基线 `db642494f7dbe594539244dbdba356490d466a51` 加[第六批修复](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md)。本轮先修 RR-28/29/30，再继续 review；下述 Activity/购买生产文件与基线 blob 未改变，**两项新问题只记录，尚未修复**。[复跑](../review/evidence/service-review-20260929-08/README.md) · [结果](../review/evidence/service-review-20260929-08/RESULTS.json)。

| 编号 | 等级 | 确认问题 |
| --- | --- | --- |
| [RR-20260929-31](#rr-20260929-31) | P2 | Activity 未校验 Cluster 共同 tag，聚合已完成但结果 dispatch 无法建立 |
| [RR-20260929-32](#rr-20260929-32) | P2 | 生成 demo 的 grantDeliverer 在未知写结果后按新 catalog 重试，覆盖首次持久 grant 的内容 |

<a id="rr-20260929-31"></a>
## RR-20260929-31：Activity 的同槽启动承诺没有执行

位置：[activity_mod.go:63](../../kit/service/global/activity/activity_mod.go#L63) 的 Init，[redis_store.go:81](../../kit/service/global/activity/redis_store.go#L81) 的 dispatch value/index 配置，以及 [service.go:1251](../../kit/service/global/activity/service.go#L1251) 的 ensureDispatches。OwedDispatchKey 注释声称 Mod.Init 拒绝无 tag Cluster，但实际 Init 只检查 KeyPrefix/TTL/预算。

触发：正式 Mod.Init/Provide 配置三 master Cluster 与无有效共同 tag 前缀，完成单 game 的合法活动。Open 的单 key Window/Activity CAS 正常；NotifyPhase 先将聚合提交为 complete，settleCompletion 再创建 dispatch 与 per-game owed index，双 key Lua 返回 CROSSSLOT。

本轮 `review8:activity`、`review8:{}:activity`、`review8:{activity`、`review8:{}:{valid}:activity` 四例均通过 Init/Provide；真实结果均为 `aggregation=complete, dispatch_found=false, lookup_err=nil, notify_err=CROSSSLOT`。有效 tag 控制通过 Open→Notify complete→Owed→Attempt→Ack→退出 Owed，全链 1 个场景通过。无 tag 不是所有 key 必然跨槽，而是无法保证所有活动同槽，当前四组确实失败。

影响：聚合结果已记录，game 暂时拿不到正式 dispatch；重复通知或 sweep 不会改变 key 的 slot，坏配置持续阻断恢复。完成活动尚可通过原窗口/已知 key 查到，不声称聚合结果已丢失，也没有证明所有重试被永久遗忘。跨槽拒绝发生在执行脚本前，本轮没有部分 value 写入证据。

实施建议：复用本批已有 `mods.ValidateClusterKeyPrefix(cfg,"activity",prefix)`，在 Cluster Init 准入，单机不变；不在窄 RedisClient 内猜拓扑，不自动改旧 prefix。补配置表、真实 tagged 生命周期和旧数据恢复：改前缀前迁移/核对已 complete、尚无 dispatch 的活动，利用已有 window sweep/ensureDispatches 重建，避免只改配置后旧聚合从新 keyspace 消失。全服务公共 tag 集中一个 slot 的代价仍在；按 group 分片需另列兼容设计。

与 [RR-29](REVIEW-2026-09-29-services-07.md#rr-20260929-29) 同类，但这是未接入检查的 Activity 及“先完成聚合、再落结果交付”链路；不把 Rank 原修复判成失效。本轮未改 Activity 代码。

<a id="rr-20260929-32"></a>
## RR-20260929-32：稳定 OrderID 不能使可变 grant 内容幂等

位置：[collaborators.go.tmpl:126](../../demo/internal/service/platform/collaborators.go.tmpl#L126) 每次 Lookup/Encode，[第 141 行 HSet](../../demo/internal/service/platform/collaborators.go.tmpl#L141) 无条件覆盖；[purchase.go.tmpl:61](../../demo/game/purchase/purchase.go.tmpl#L61) 的 catalog 与 Grant 注释承诺 resolved 内容不受付款/领取之间的 catalog 编辑影响。范围是框架提供的 demo/生成消费者模板，**不是任意自定义 Deliverer 或平台服务统一实现都必然有此 bug**。

确定触发：

1. 旧版本 potion_pack 的 Count=10，生产 grantDeliverer 向真实 Redis 写入 orderID 对应 grant。运输层在实际 HSet 成功之后返回 DeadlineExceeded；这不是 ErrDeliveryNotApplied，平台应保留不确定尝试并允许重试。
2. 玩家还未领取，部署新版本，把同商品 Count 改为 3、保留 ProductID/价格。
3. 新进程同订单重试 Deliver。当前代码重新查 catalog，HSet 同一个 field，将 Redis 中已经解析且持久的 Count=10 覆盖为 3。

证据使用已正式 CLI 生成的消费工程，与当前两个模板逐字核对（模块占位符/换行规范化）；两个独立 `go test` 二进制顺序执行旧版 seed、旧版重建 control，再用 overlay 编译新 catalog 的第三个二进制。真实 driver/Redis 写后丢回复 seam 不改变 HSet 行为：seed/control 均 `resolved_count=10` 通过，升级后 `resolved_count=3`，期望首次内容 10 的断言失败。不是测试中直接替换已存 grant，也未改正式 catalog。

期望：已持久 grant 的 ItemID/Count/PlayerID/支付事实为首次有效解析的承诺，重试应恢复同一内容；实际按新 catalog 改写。ClaimPurchase 的永久 OrderID ledger 能防已领取后再加一次物品，但在首次领取之前无 ledger，它不能修复被改掉的待领取内容。本轮没有执行真实资产/Nest/Mongo 领取，不把“后续会按 3 发货”的源码推导写成实际资金或背包验证。

实施方向优先已有工具：扩展现有 grantStore 所需的 ScriptRunner/读取能力，在**单个 grants Hash** 内用 Redis Eval 做插入缺失、已存在返回原字节，当前 IRedis 已有 Eval/HGet，无需新存储/新包或全接口 HSetNX。正确处理首次成功/错误未知/并发/内容不一致，恢复时读取已经持久的商品快照，不能用新 catalog 覆盖。仅 Go HExists→HSet 仍有竞态；HSetNX 后盲目视为当前新内容也不能兑现返回/审计契约。

不能据此自动压缩 ClaimPurchase ledger。game HDel 后迟到旧 deliverer 仍可能再写该 orderID；若要归档，需有稳定履约回执或持久 outbox 身份，生产者也必须拒绝已 fulfilled 的再创建，覆盖 producer 在途/未知结果和 consumer durable ack。简单“领取后删去 ledger+grant”会重开重复资产通路。已生成 collaborators 属写一次业务文件，模板修复不自动覆盖用户工程，应给消费方明确升级补丁与已落 grant 保留规则。

## 观察与边界

Mail 的 Mailbox/SendLedger 为单 key versionstore，不能仅因没有共同 prefix tag 登记同类 bug；Envelope 的批量 MGet 还依赖 driver 路由，本轮未测所有跨槽批量读取。购买 HGetAll 无分页、未领取 grants 无数量上限，永久 ClaimPurchase ledger 也增长；沿既有容量观察，未跑新长期压测，不重复 RR。

新 review 共 **8 次叶子执行：3 pass/5 预期 fail/0 skip/build-fail/race 报告**（四 Activity 配置失败 + 一个升级失败，归属两个 RR）。完整正式回归和上轮 15/15 修后结果不覆盖这些新断言。真实渠道/资产、replica/failover、强杀、归档迁移和生产容量仍未验证。
