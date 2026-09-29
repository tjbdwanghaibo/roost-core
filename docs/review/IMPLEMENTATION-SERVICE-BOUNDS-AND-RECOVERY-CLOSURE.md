# Service：容量、年龄与资源恢复的实现学习

## 第五批修复后的实际实现

源码 4b0837d7，[实现、兼容与测试](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)、[修后审查](REVIEW-2026-09-29-services-06.md)已完成。下方原建议保留为第五轮的教学推导，本节是当前事实。

- Activity 用原 Window.Opening.Intent 保存初始 Activity，先持久准入再 Create；错误与超时不撤销名额。grace 后帮助原计划创建，确认只能消耗已有 slot。安全性依靠 insert-only Key 的同计划恢复，不靠短期计时器撤销慢调用，也没有增加第二套事务/存储系统。
- 旧超容量 pending 采用 Window.ScanAfter 持久游标，单批读取至多 256 个不同 key，重建 Service 后延续。窗口 JSON 本体依旧整对象读取/复制；对旧大对象的内存/延迟不作常量成本承诺。
- Group 的两条公开策略入口复用 Queue.Validate；Rank int64 正负溢出在 Lua CAS 与请求 ring 更新前拒绝，不静默饱和，拒绝后相同 RequestID 可合法再试。
- Chat 年龄清理遍历有界 Ring、按 limit 删除；保留 Seq 顺序和 Requests。Page.Gap 现在覆盖页内/游标边界/保留尾部洞，空 forward 页不推进输入 NextCursor。年龄与消息顺序的合同分别兑现。
- 恢复责任会保留容量：旧没有 Intent 且缺活动记录的 Opening 不能猜 expected 集合，需业务同 key Open 补计划；新已准入计划也不能用超时当取消。Activity/Chat owner 协调升级，JSON 可读不等于混合版本安全。

本批 Memory/Redis 的正式定向各 27 叶子通过；真实进程强杀、allocator/渠道与 HA 仍需专项，不由本节自动关闭。

## 第五轮原学习记录

源码 `b336ce62`，事实与[第五轮执行](REVIEW-2026-09-29-services-05.md)关联；以下建议尚未实现。现有架构和范围见[完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)。

## 准入、索引和扫描是一条完整证明链

Activity 的 OpenActivity 先 CAS Windows.Opening，再 Create Activities，最后 confirmWindow 把 opening 移到 Keys。Sweep 在 OpeningGrace 内保护未完成创建，超过期限且记录缺失则回收。这样修好了普通创建间隙，却没有给“已经回收”的慢创建设置围栏：迟到确认无条件补入 Keys，抢回已让出的容量。

不能只审 admitToWindow 的 256 上限。必须追踪每个写 Keys 的入口与释放名额的入口，并核对 AdvanceExpired 的扫描上限、排序与候选状态。测试把 256 个不通知的 Pending 放在前面，迟到活动放最后，证明容量溢出会转化为可达性丢失，而不仅是数字超限。

修复方案应利用既有 opening 意图/代次与 versionstore CAS，约束谁仍持有名额；已经写入的活动必须有持久可恢复入口。窗口无名额时返回错误不是回滚 Create 的证明；把问题移到无索引孤儿仍不安全。批量恢复需有有界游标/轮转，不能扩大一个固定扫描常量后称完成。

## 输入约束要在真正执行的公开入口生效

Match Store 验证 Queue 不能替代 Grouping.Group 的验证，因为 Grouping 被刻意放在 CAS 外，由调用者执行。FIFO/score 在 slicing 和读取 anchor 前复用 Queue.Validate，才能保证负数组大小返回业务错误。设计上保持策略独立是正确的，缺的是入口的契约校验。

Rank 的 UpdateAdd 在 nextScore 做纯 Go int64 运算，之后才 Lua CAS。CAS 保证没被并发覆盖，**不会判断结果是否算错**。加法检查要在提交与幂等 ring 更新前，拒绝后不能占用 RequestID；正负边界都需要测试。已有活动计分的校验不覆盖另一条 Rank 调用链。

## 顺序与年龄不能共享未经证明的单调性

Chat Seq 对消息先后负责；StoredAtUnix 对保留年龄负责。CAS 排好了 Seq，不能让不同副本时钟或回退后的 StoredAt 单调。Prune 的 prefix 假设把两者混成了同一种排序。

现有 Ring 数量有界，逐条年龄扫描在成本上可限定，但中间清理产生 Seq 空洞，客户端 Gap/分页协议必须同步处理。另一条设计路径是单调的保留时间；需明确其与实际墙钟年龄的区别。修改前先选契约，不能按 timestamp 排历史而破坏 Seq。

Chat RequestID ring 有限，正文删除后仍可拒绝窗口内重复；请求证明最终淘汰后同 ID 可以再次 Publish。这是明确的有限去重承诺，不是新 bug。Match 历史则没有完整终态归档机制：Waiting=0 仍保存 Tickets/Requests。删除历史之前必须确定查询期限、重放期限及安全身份，而不是为了小 JSON 任意裁掉证明。

## Service 状态与外部业务效果分别恢复

本批 Platform 修复区分“明确未应用”和“结果未知”：未知 pending 保留并停止自动重试，人工查询权威结果后 Resolve/Settle/Reopen。Mail 删除保留 terminal proof 到允许重投的有效窗口。二者都不能用超时、容量淘汰或重启来代表“外部动作一定没有发生”。

Session 的 Finish 可以重叠调用 Releaser，本轮可控 fixture 得到 calls=2/effects=1。这依赖资源端持久幂等，fixture 的 mutex/布尔值只是证据工具，不能拿去当跨进程方案。资源引用应包含 allocation incarnation；否则同字符串 ID 被重用时，迟到释放可能碰到新资源。建议由既有 owner/fence 与类型化 versionstore 记录释放身份/收据，明确调用方如何对账，不虚构已有 exactly-once allocator。

生成消费链保持稳定身份：Mail claim.Token → 库存；Platform OrderID → Redis 凭证 → Nest 购买 ledger；Match.ID → 房间；Session Run.ID 与 resource allocation → Release。能力注册与 generated transport 把调用者和 owner 解耦，但真实资产/房间系统要独立兑现幂等、回执与重建。demo 的空函数、日志和内存 Battle 对象适合教学，不承担上线证明。

## 如何理解本阶段完成

10 域主链与组装/存储契约已经整理；100 路径清单说明范围没有漏掉，57 个无变更路径复用以前源码证据、25 个变更非生成路径复读、18 个生成文件做一致性检查。它不说明每一行和每种交错都正确。

进度应同时记录三件事：已整理的源码主链、实际运行的场景、尚待实施的缺陷。HA/强杀/性能/真实 allocator 留作具名专项，而不是每轮重新浏览所有模块以增加文件触达率。建议先实施本轮四项，保留原红测并新增边界/并发控制，再决定下一类专项。
