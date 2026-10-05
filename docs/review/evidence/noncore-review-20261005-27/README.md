# 第27轮证据范围

[coverage](coverage.json) 为28材料路径，roost-core ready32285 nodes/209828 edges，generation2026-09-30T11:58:14Z。均没有记录解析缺口，metadata_changed/not_tracked 用当前源码补证；[LF摘要](source-hashes.csv)用于后续增量判断，不表示28文件全文穷尽审完。

search_graph 定位 account resumeRoleCreation、chat pageOf、activity AdvanceExpired/applyProgress 与 Saga ClaimOutbox/publisherLoop；相关双向trace与关键片段后读当前差异/精确范围。初次 slash 风格 qn_pattern 返回0，改为点号命名；一次宽 History 查询有分页，后续仅依赖缩小后的具名入口查询，不作全量结论。图谱混合 receiver 同名方法：ClaimOutbox 的片段指向内存测试实现，正式 MongoStore 证据使用当前源码；旧行号截断和标准库同名误边不作为调用证明。误列 saga/command.go 不存在，最终28路径清单已剔除。

未强制刷新、未删除锁/停止共享实例。实际新增13场景及两个已知问题/旧RR残余见[红绿](../../../bugfix/evidence/noncore-bugfix-20261005-17/README.md)。外部资源未执行，清洁 coverage 不是完整性证明。
