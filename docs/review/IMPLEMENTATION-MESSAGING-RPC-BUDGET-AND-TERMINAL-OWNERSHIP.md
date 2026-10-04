# 通信 RPC：协议、预算和唯一收尾责任

2026-10-04，N03源码等于`c4aa1e7d`，整棵产品工作树对应`49796514`。[审查实证与范围](REVIEW-2026-10-04-noncore-06.md)，[三个待修问题](../bug/REVIEW-2026-10-04-noncore-06.md)。本文区分既有机制与建议，未实施新通信修复。

## 调用经过哪些门槛

ServiceRPC.BusClient可按显式SID或discovery+picker定位实例，轻量Call/CallTo与JetStream CallReliable/CallToReliable按配置选择；CallChecked再将response里的业务status转成Go错误。Discovery失败不代表目标业务拒绝；RPC收到回复也不代表response成功，业务status必须单独判断。Affinity对候选复制/按sid排序后hash，稳定集合下同key稳定；成员变化仍要共享Store/CAS保证正确性。

目前Call入口创建timeout，但CallDiscoveredChecked先发现再进入Call，发现阶段没有这个预算，NC-10由此成立。建议整个组合入口只建立一个覆盖发现/选择/传输/status的child deadline，保持更短parent，发现后使用剩余预算。不要发现出错就向随机实例发副作用，不在不确定结果后自动重试。单独PickServer契约另写清。

## 两种传输的回包责任

轻量Bus将MsgName/SessionID/MsgID与业务payload封成NatsMsg，实例和服务queue按subject订阅；收到后按声明method派入pool。版本1 response envelope将transport业务拒绝与success payload分开，decodeRPCResponse/Bytes共用契约，不能把裸error对象解成业务零值。

JetStream请求携带ReplySubject/DeadlineAt，caller pending channel按requestID关联。request handler执行后encodeRPCSuccess/Failure，publish成功才返回nil供driver ACK；publish失败可能重投。当前无handler分支遗漏envelope（NC-08），最小修复应复用encodeRPCFailure，不放宽客户端version门禁。没有订阅的broker行为和有订阅/无handler是两种场景。

JetStream settle依据nil/error/permanent/MaxDeliver选ACK/NAK/TERM，settle失败有指标日志；业务副作用仍可能在回复/ACK失败前已经发生。Async ReliableStore只给消息inbox/DLQ，不给RPC自动事务幂等；正式业务应利用稳定RequestID/持久Store，不能把timeout解释为一定未执行。默认同步RetryPolicy一次，显式重试要明确这个责任。

## Pending 与停止

NATS RPC用LoadAndDelete选唯一terminal winner，reply/timeout/publish error/Stop竞争；winner关闭timer/sub并向callback pool投递。rpcTask的arrival guard让正常handler+OnRelease只执行一次；pool拒绝会由OnRelease同步完成，因此“callback只在pool执行”的接口注释存在例外，需明确接入约束。队列有界不等于pending有界，每call的timer/sub/记录仍要容量预算。

Assembly先Stop RPC再DrainWithContext连接，Kit转发ctx；目前RPC.Stop用Background排空并可在callback处永久等待（NC-09）。建议复用Pool.StopWithContext、唯一stop状态与可重复等待，预算结束只代表caller不等了，资源/完成责任仍在。满队列下同步fallback也要覆盖，不能单替换最后pool.Stop就声称整段有界，更不能用丢弃callback或无界后台goroutine换绿灯。

Bus停止先关闭准入/取消base，再在lifeMu外unsubscribe/drain。不能持锁等待会回调注册的handler。JetStream消费ctx不直接继承创建订阅时的ctx，避免setup调用结束就让长期订阅handler全取消；处理预算来自Bus lifetime与wire deadline。仍需真实连接/重连、关闭与迟到响应的专项。

## 本批证明与未证明

17项包括4失败/13控制，三个根因待修；六包race/vet通过。证明了正式JS分支产物的协议不一致、真实pool的取消等待缺失、组合调用没给discovery预算；没有真实NATS/etcd/Redis、跨机器故障或吞吐验证。Affinity复制+插入排序O(n²)、每call订阅/计时器与DLQ批次是成本观察，未有benchmark，先围绕正式业务路径建立预算和容量场景再优化。
