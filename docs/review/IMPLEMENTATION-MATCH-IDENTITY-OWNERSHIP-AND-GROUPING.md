# 匹配：队列身份、对象所有权和配对执行

基线 Core 9f31436、Kit 4830150、Codegen 242b438；[09-17 证据](REVIEW-2026-09-17.md)。本文解释当前实现及新边界，不表示建议已实施。

## 队列状态和原子提交

Core/service/match 把 Waiting、Tickets、Matches、SubjectTickets、Requests 放在一个 queueState 中；versionstore.Update 通过单键 CAS 使 Commit 的退队、票据置 matched 和保存比赛一起发生。候选读取不锁住后续提交，Commit 必须再次校验票存在、仍等待且主体不重复。

这层原子性成立的前提是队列身份正确。当前 Queue.Key 同时参与存储和路由，直接拼接自由字符串会合并不同 Queue。给 CAS 加重试不会修复错误键；RR-20260917-01 应从身份编码和旧数据迁移修复，票据归属检查作防御。

## 策略与应用执行

Kit Mod 装配 Store，Core 的 Grouping 是应用工具。demo game goroutine 每 500ms 读取至多 64 候选，Group 选票后 Commit；争抢/缺票/已匹配由下一 tick 重新读取，其他错误向外报告。成功后通知 Nest 统计、推送玩家，再从当前候选窗口删除已消费票继续配对。

统计/推送失败不撤销已经提交的比赛；票据中 MatchID 是查询依据。12 个新 demo 场景通过，包含推送不可用时 2/4/5 票仍有正确 matched/余票状态。未证明断线后客户端自动保有 ticketID 或实际 PollMatch 网络路径，本轮仅源码阅读该端点。

ScoreWindow 以最老候选为锚，随等待时间放宽窗口，按距离/到达顺序选人；整型距离和增长必须在有界非负域计算。先发生溢出再 cap 已经太晚，abs(MinInt64) 也不能得到合法非负距离。RR-20260917-02 覆盖筛选和排序的同一算术源头，不能只修其中一处。

## 所有权和后端差异

queueState.clone 目前复制 map 与 Waiting，却不复制引用字段。MemoryStore 按值保存泛型对象，值内切片仍共享；match 对外返回 Ticket、Match 时会泄露内部 Payload/Members/TicketIDs。外部修改结果就成为未经 Update 的状态变更，RR-20260917-03 在内存后端七个入口复现。

所有权应在类型化服务边界声明并执行：输入何时归服务所有、输出是否独立快照、内部对象是否只读。深复制要包含 Subject.Payload；仅复制 []Subject 还不够。不要默认“Go struct 按值返回”意味着完整快照，也不要将内存泄露推断成 Redis JSON 落盘同样共享 Go 引用。

## 容量与性能

单键聚合便于原子性，却让每次 clone/编码与整个队列历史大小相关。MaxQueueLength 限等待队列，不限已完成票/比赛/请求账本；TicketTTL 不是历史保留期限。128 场且等待为零的样例仍留下 256 Tickets、128 Matches、256 Requests，一年后 Sweep 也不清理。

需要先定义重试窗口、历史查询和归档责任，再做带容量边界的回收；删除幂等账本可能将旧请求当新请求。该项目前是设计观察，无真实后端压测数据，不声称已测得性能退化阈值。业务分区、候选分页和历史保留是不同约束，不能互相替代。
