# v1.23.1 阶段清单核对

核对范围：REMAINING-FIXES-v1.23.1-2026-10-07的B1～B8，FRAMEWORK-DOCS-FINDINGS登记行、bug/bugfix索引、WANTED/CARRYOVER与CORE-OPTIMIZATION-HANDOFF。按最新记录覆盖旧日期状态，未做全仓新一轮逐函数审计。

| 范围 | 当前判断 |
| --- | --- |
| B1～B5/B7/B8 | 前轮已提交验收，记录见v1.23.1双文档；不因历史“未修”重复改造 |
| B6 RR-22～24/26～28 | 已验收；保持正式接入、ReplicaStore与会话生命周期边界 |
| B6 RR-25 | 保留MaxDeliver方案已实施；旧无限重投候选废弃；5包race、真实NATS 5场景通过 |
| 本轮确认缺陷 | 随RR-25修复，B1～B8清单内没有仍待实施的确认缺陷；最终全仓/矩阵与tag下载验收待下节回填 |
| 已决定保留 | Windows Y14只保留编译/CLI范围；C8公开外部集成API保留，不虚构生产调用 |
| 独立产品方向 | Remote outbox唯一发布者仍待决定，当前既有修复不依赖这项重构 |
| 历史验证边界 | CARRYOVER仍列部分真实etcd/HA、迁移复杂组合、外部业务/长容量边界；本轮不能把这些全部计为通过。WANTED表的历史候选已有分流去向，不等于所有外部验证完成 |
| 未做 | 24小时长稳、真实生产部署、Windows完整正确性、客户端完整引擎矩阵 |

因此可以评估阶段性发布，但不能宣称“全仓没有任何问题/所有场景已验”。此前已接受的Sync 50ms少量长尾不改变；本轮也没有新的TPS容量结论。

## 最终验收

RR-25行为红/绿、最终目标race与真实NATS日志保存 `artifacts/perf/rr25-bounded-20261008/`；旧方案日志保留在remaining-fixes目录，不混充新方案证据。最终pretag、21格矩阵、tag与tag生成工程结果在实际完成后追加。
