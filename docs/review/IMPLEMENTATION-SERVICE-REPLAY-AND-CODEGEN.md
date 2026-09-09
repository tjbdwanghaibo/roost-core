# 实现学习：领取、匹配重放与生成消费者

基线为[进度表](PROGRESS.md)第四轮 Kit/Codegen/Core SHA；测试证据见[第四轮](REVIEW-2026-09-09-04.md)。本文将源码事实与待实现建议分开。

## Mail：稳定领取 token 与租约不是同一个概念

阅读范围为 Kit `service/mail/service.go` 的领取入口与 `mailbox.go` 状态结构。Reserve 检查玩家、收件范围、有效期、附件、邮箱条目与领取状态，然后通过状态更新取得 Claim。活动领取租约阻止再次占用，超时后重试复用稳定 token。

Commit 检查 token 与条目对应关系；已领取可幂等返回，删除状态拒绝提交；成功转为 Claimed，清理租约期限但保留 token。Cancel 对不存在/已领取等情况按实现返回，不把撤销等同于生成新业务领取身份。

因此，token 表示业务领取的去重身份，deadline 表示一次尝试的占用期限。奖励系统需要以 token 做自身去重或原子回执，否则邮箱状态本身不能证明资产发放恰好一次。这是跨服务接入要求，不是本轮发现的新 bug。mail race 包测试通过，但未验证真实奖励系统、广播生命周期与持久后端故障。

## Match：队列原子性与请求归属分别验证

Kit `service/match/queue_store.go` 将 Waiting、Tickets、Matches、SubjectTickets、Requests 聚合在一个 queueState。Enqueue 在队列状态 Update 内执行过期处理、请求重放、主体重复检查和入队；Commit 在同一状态更新中检查人数、票据唯一、仍在等待及主体不重复，再统一生成匹配并移除等待关系。

这种边界使同队列状态变更可以在一次 CAS 中提交，但 CAS 不能自动验证业务身份。当前 Enqueue 的 Requests[requestID] 分支早于主体入队检查，命中便返回旧票；Ticket/Cancel 则有 validateOwnership。于是正常 API 分支和重放分支具有不同归属保证，形成 RR-20260909-05。

本轮测试用真实 MemoryStore 保留同归属重试作为正对照，并用 Ticket 的拒绝作为归属对照；不需要并发就能复现。建议审查同类服务时逐一比较“首次调用”“命中账本”“超时重试”的身份检查，不能只测试相同请求重试成功。吞吐、真实 Redis 竞争和所有 Sweep 保留期组合留待后续。

## Entity Codegen：符号唯一还需要产物完整

Codegen `internal/entity/main.go` 收集包内实体，再调用 `gen.go` 的 generateInPackage。当前默认模式使用每实体文件名，每实体注册 once/helper 名称唯一，包级 RegisterEntity 放在排序后的首实体产物中。

默认模式的真实双 Entity 消费者测试通过，证明 RR-20260909-04 的原触发已修复。但 -output 为整个循环提供同一路径，后一次生成覆盖前一次；聚合入口若位于被覆盖文件，就会连同声明消失。生成器退出成功只说明每次写文件成功，不代表最终消费者可编译。

RR-20260909-06 的验证分两层：真实 CLI 退出码与最终消费者编译；默认模式作为对照。建议把“生成器包测试”“CLI 产物编译”“正式 tag 消费者集成”看作不同门禁。本轮完成前两类相关场景，使用临时 go.work 连接最新源码，不冒充发布 tag 验收。
